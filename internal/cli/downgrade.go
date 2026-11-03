package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// `lit downgrade` has two forms, and the flags choose one before anything
// opens:
//
//   - `--to <tag>` composes the schema-side Downgrade boundary (internal/store)
//     and the binary-side release pipeline (internal/release) into the rollback
//     of a bad upgrade. Sequence (each stage runs unconditionally; failure
//     stops the pipeline): resolve the tag through the release manifest; call
//     Store.Downgrade(target schema) — pre-snapshot refusals propagate
//     verbatim, post-snapshot failures arrive as *DowngradeRollbackError whose
//     Error() carries the operator restore instruction; resolve the running
//     binary's real path and atomically install the prior binary there; print.
//     The user's next shell prompt invokes the prior binary; no re-exec.
//   - `--schema <v> --remote <name>` lowers a shared remote and holds it there:
//     Store.DowngradeAndHold(v) steps this backlog to v with the schema hold in
//     the last reversed commit, and the one live-store push sends that head
//     (design-docs/schema-reversibility.md, "Lowering a remote, and making it
//     stay lowered"). It installs nothing: the binary that reads a held store
//     is the release at the hold, which `--to` installs.
//
// The two forms acquire differently, and the difference is the whole reason
// they are two: the release form opens the store as every mutating command
// does, migrating; the remote form opens the sync session, which never
// migrates, so it opens a store already held below this lit's registry — the
// state a re-run after a failed push finds — where the migrating open would
// refuse it (SchemaCeilingError).
//
// [LAW:single-enforcer] release.Resolver owns artifact resolution,
// release.Installer owns binary install, the engine's storage.SchemaMigrator
// owns the schema reverse and the hold, pushLiveStore owns the push. This
// file sequences them and contains no novel logic itself.
// [LAW:no-mode-explosion] Two forms, each a documented command of the design,
// and no --dry-run, --force, or --skip-snapshot; each would have to earn its
// way via a concrete user need.

// downgradeForm is which of the two forms the flags asked for.
// [LAW:types-are-the-program] a closed enum the parse produces once; the
// acquisition and the work read it, never the flags.
type downgradeForm int

const (
	downgradeToRelease downgradeForm = iota
	downgradeRemote
)

// downgradeFormOf reads the parsed flag set as one form: `--to` alone is the
// release form, `--schema` with `--remote` the remote form, and every other
// combination — either remote flag alone, `--to` beside either, none — is
// refused here, the one place the combination rule lives.
// [LAW:single-enforcer] [LAW:parse-dont-validate]
func downgradeFormOf(fs *cobraFlagSet) (downgradeForm, error) {
	to, schema, remote := fs.Changed("to"), fs.Changed("schema"), fs.Changed("remote")
	switch {
	case to && !schema && !remote:
		return downgradeToRelease, nil
	case !to && schema && remote:
		return downgradeRemote, nil
	}
	return 0, model.ValidationError{Message: "downgrade: give --to <version> to install a release and step this backlog down to its schema, or --schema <v> --remote <name> to lower a remote and hold it there; the two forms do not combine, and --schema and --remote go together"}
}

// downgradeLeaf is the production command: the real release seam, the real
// binary path, and the acquisition each form needs.
func downgradeLeaf() wsLeaf {
	return withDowngradeAcquisition(func() downgradeDeclaration {
		return downgradeLeafWith(&release.HTTPResolver{}, &release.HTTPInstaller{}, currentBinaryPath)
	})()
}

// withDowngradeAcquisition adapts the declaration to the workspace pipeline:
// after the parse, the form picks the open, and each form's work runs over
// exactly the dependencies it has.
// [LAW:no-ambient-temporal-coupling] The declaration passes straight through —
// only the WORK is wrapped, so nothing opens before the parse that a help
// request never gets past.
func withDowngradeAcquisition(declare func() downgradeDeclaration) wsLeafFn {
	return func() wsLeaf {
		d := declare()
		return adaptLeaf[downgradeScope, workspace.Info](d.leaf, func(ctx context.Context, inv Invocation, ws workspace.Info, positional []string) error {
			form, err := downgradeFormOf(d.leaf.fs)
			if err != nil {
				return err
			}
			current, err := version.Get()
			if err != nil {
				return fmt.Errorf("downgrade: read this binary's version info: %w", err)
			}
			if form == downgradeRemote {
				request, err := d.remote()
				if err != nil {
					return err
				}
				command := []string{"downgrade", "--schema", strconv.FormatInt(request.schema, 10)}
				return runWithSyncSession(ctx, inv, ws, func(ctx context.Context, session syncSession) error {
					migrator, err := storage.SchemaMigration.Of(session.engine)
					if err != nil {
						return err
					}
					// The remote is resolved before the store is lowered and
					// held, so a remote the push would skip leaves the store as
					// it was.
					if err := requireLivePushTarget(ctx, inv, ws, session, request.remote, command); err != nil {
						return err
					}
					return remoteDowngradeWork(ctx, inv, request, remoteDowngradeScope{schema: migrator, current: current, push: func(ctx context.Context, inv Invocation) error {
						return pushLiveStore(ctx, inv, ws, session, request.remote, livePushOptions{command: command})
					}})
				})
			}
			// [LAW:parse-dont-validate] Schema migration is a capability: an
			// engine whose data has no shape versioned apart from the data itself
			// has nothing to downgrade. Asking the opened store yields the
			// migrator, which satisfies the narrower schemaDowngrader, so the
			// work never sees an engine that cannot revert.
			return runAppIn(ctx, inv, ws, app.AccessWrite, func(ctx context.Context, ap *app.App) error {
				migrator, err := storage.SchemaMigration.Of(ap.Store)
				if err != nil {
					return err
				}
				return d.leaf.work(ctx, inv, downgradeScope{schema: migrator, current: current}, positional)
			})
		})
	}
}

// schemaDowngrader is the schema-side dependency the release form calls. The
// production implementation is the engine's storage.SchemaMigrator; tests
// substitute a fake.
//
// [LAW:types-are-the-program] The CLI doesn't need the full storage.Store API
// for downgrade; it needs exactly the reverse, which reports what it did.
// Narrowing the dependency to it makes the pipeline testable without a real
// Dolt workspace and prevents callers from coupling to incidental Store
// methods.
type schemaDowngrader interface {
	Downgrade(ctx context.Context, targetSchemaVersion int64) (*storage.Downgraded, error)
}

// schemaHolder is the schema-side dependency the remote form calls: the
// reverse that leaves the store held at its target.
type schemaHolder interface {
	DowngradeAndHold(ctx context.Context, targetSchemaVersion int64) (*storage.Downgraded, error)
}

// downgradeScope is what the release form's work runs against: the store's
// schema verbs, and this binary's identity, whose schema support decides
// whether the reverse can run. It mirrors upgradeScope.
type downgradeScope struct {
	schema  schemaDowngrader
	current version.Info
}

// remoteDowngradeScope is what the remote form's work runs against: the
// holding reverse, this binary's identity, and the push of the held head,
// which closes over the sync session the form opened.
type remoteDowngradeScope struct {
	schema  schemaHolder
	current version.Info
	push    func(ctx context.Context, inv Invocation) error
}

// remoteDowngradeRequest is the remote form's parsed flags: a schema version
// at or above 1 and a named remote. [LAW:parse-dont-validate]
type remoteDowngradeRequest struct {
	schema int64
	remote string
}

// parseRemoteDowngrade is the remote form's checkpoint on its two values.
func parseRemoteDowngrade(schema int, remote string) (remoteDowngradeRequest, error) {
	if schema < 1 {
		return remoteDowngradeRequest{}, model.ValidationError{Message: fmt.Sprintf("downgrade: --schema %d is not a schema version, an integer at or above 1", schema)}
	}
	name, err := requireRemoteFlag("downgrade", remote)
	if err != nil {
		return remoteDowngradeRequest{}, err
	}
	return remoteDowngradeRequest{schema: int64(schema), remote: name}, nil
}

// requireRemoteFlag is the one check on a `--remote` that names the remote a
// command lowers or lifts: non-empty once trimmed. Which remote the name
// denotes is the push's resolution (resolveSyncTarget), as for `lit sync
// push`. [LAW:single-enforcer]
func requireRemoteFlag(verb string, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", model.ValidationError{Message: verb + ": --remote requires a non-empty remote name"}
	}
	return name, nil
}

// downgradeLeafShape is the release form's leaf over the verbs its work
// needs, rather than over a whole opened app. [LAW:types-are-the-program] the
// narrow resource is what lets a test drive the real declaration→parse→work
// path against a fake, with no store and no app anywhere in the picture.
type downgradeLeafShape = leaf[downgradeScope]

// downgradeDeclaration is `lit downgrade` declared: its one flag set, carried
// by the release form's leaf, and the remote form's values read from that
// same parse.
type downgradeDeclaration struct {
	leaf   downgradeLeafShape
	remote func() (remoteDowngradeRequest, error)
}

// downgradeLeafWith is the declaration parameterised over the typed
// dependencies so tests can substitute fakes. downgradeLeaf picks the
// production implementations: HTTPResolver/HTTPInstaller for the release side
// and currentBinaryPath for binary-path resolution.
func downgradeLeafWith(
	resolver release.Resolver,
	installer release.Installer,
	binPathFn func() (string, error),
) downgradeDeclaration {
	fs := newCobraFlagSet("downgrade")
	to := fs.String("to", "", "Target binary version (release tag vX.Y.Z, e.g. v0.4.1): install it and step this backlog down to its schema")
	schema := fs.Int("schema", 0, "With --remote: the schema version to lower the remote to and hold it at")
	remote := fs.String("remote", "", "With --schema: the remote to lower; this backlog steps down, holds itself there, and pushes")
	// --to is required by its form, and the derived sentence cannot say so:
	// "values are passed as flags: --to" reads exactly like a flag you may omit.
	// Requiredness is the leaf's own knowledge. [LAW:one-source-of-truth]
	work := func(ctx context.Context, inv Invocation, scope downgradeScope, _ []string) error {
		tag, err := normalizeReleaseTag(*to, "downgrade")
		if err != nil {
			return err
		}
		// [LAW:nothing-unseen] the release the command installs and the schema
		// versions the reverse is decided from — the same ones `lit upgrade`
		// records for its mirror decision — each set as soon as it is known, so
		// an exit before the decision still carries them.
		telemetry.Set(ctx, "downgrade.tag", tag.String())

		platform := release.CurrentPlatform()
		target, err := resolver.Resolve(ctx, tag, platform)
		if err != nil {
			return err
		}

		telemetry.Set(ctx, "downgrade.target_schema_max", target.Manifest.Schema.Max)
		telemetry.Set(ctx, "downgrade.running_schema_min", scope.current.Schema.Min)
		telemetry.Set(ctx, "downgrade.running_schema_max", scope.current.Schema.Max)

		// What the reverse did is recorded before its error is: the workspace's
		// version as the store read it under the workspace lock and decided
		// from, and how far a reverse that stopped part-way got. done is nil
		// only when the store failed before that read.
		done, err := scope.schema.Downgrade(ctx, target.Manifest.Schema.Max)
		if done != nil {
			facts, counts := done.EventRecord()
			telemetry.Record(ctx, facts, counts)
		}
		if err != nil {
			return err
		}
		// [LAW:no-silent-failure] the reverse has committed, so a prune failure
		// does not stop the install. Nothing is written before the install, so
		// no write can stand between the reverse and the binary that reads it;
		// [LAW:dataflow-not-control-flow] the warning is a value, "" when the
		// prune succeeded, ending whichever output ends the command.
		pruneWarning := downgradePruneWarning(done.PruneErr)

		binPath, err := installOverCurrentBinary(ctx, installer, binPathFn, target)
		if err != nil {
			// [LAW:no-silent-failure] schema is already downgraded at this point;
			// surface the install failure with the exact recovery the operator
			// needs (run the prior binary themselves, or restore the snapshot).
			return fmt.Errorf(
				"downgrade: schema reversed to v%d but installing prior binary failed: %w\n\nrecover by either:\n  - installing %s manually (download from %s), then re-running lit; or\n  - restoring the pre-downgrade snapshot via `lit snapshots list` + `lit snapshots restore <name>`%s",
				target.Manifest.Schema.Max, err, tag, target.Artifact.URL, pruneWarning,
			)
		}
		telemetry.Set(ctx, "downgrade.installed_path", binPath)

		// [LAW:dataflow-not-control-flow] The post-install step is a single
		// print. Re-exec'ing into the prior binary on Unix or printing a human
		// re-run line on Windows would add a platform mode for no measurable
		// benefit — the rename has already happened, the user's next shell
		// prompt runs the prior binary.
		_, err = fmt.Fprintf(inv.Stdout,
			"downgraded to %s (schema v%d) installed at %s\nre-run `lit version` to confirm.%s\n",
			tag, target.Manifest.Schema.Max, binPath, pruneWarning,
		)
		return err
	}
	return downgradeDeclaration{
		leaf:   downgradeLeafShape{fs: fs, positionals: 0, usage: "usage: lit downgrade --to <version> | --schema <v> --remote <name>", work: work},
		remote: func() (remoteDowngradeRequest, error) { return parseRemoteDowngrade(*schema, *remote) },
	}
}

// remoteDowngradeWork lowers this backlog to the requested schema, held
// there, and pushes the held head. The store's report is recorded before its
// error is, as the release form records its reverse; the push's own outcome
// and failure are pushLiveStore's, the same as `lit sync push`'s.
func remoteDowngradeWork(ctx context.Context, inv Invocation, request remoteDowngradeRequest, scope remoteDowngradeScope) error {
	// [LAW:nothing-unseen] the remote and the schema the command lowers to,
	// under the keys the release form uses for its target, so one query
	// reads both forms' targets.
	telemetry.Set(ctx, "downgrade.remote", request.remote)
	telemetry.Set(ctx, "downgrade.target_schema_max", request.schema)
	telemetry.Set(ctx, "downgrade.running_schema_min", scope.current.Schema.Min)
	telemetry.Set(ctx, "downgrade.running_schema_max", scope.current.Schema.Max)

	done, err := scope.schema.DowngradeAndHold(ctx, request.schema)
	if done != nil {
		facts, counts := done.EventRecord()
		telemetry.Record(ctx, facts, counts)
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(inv.Stdout, "%s; pushing the held head to %s%s\n", loweredLine(request.schema, done.Reversed), request.remote, downgradePruneWarning(done.PruneErr)); err != nil {
		return err
	}
	return scope.push(ctx, inv)
}

// loweredLine is the remote downgrade's result as a sentence: the lowering
// it made, or that the backlog was already there, as a re-run after a failed
// push finds it. [LAW:one-source-of-truth] worded from the store's report.
func loweredLine(schema int64, reversed int) string {
	if reversed == 0 {
		return fmt.Sprintf("this backlog is at schema v%d and held there", schema)
	}
	return fmt.Sprintf("lowered this backlog to schema v%d and held it there", schema)
}

// installOverCurrentBinary installs target over the running binary and
// returns the path it installed to. Resolving that path is part of the
// install, so a downgrade that cannot resolve it reports the one failure a
// reversed workspace without its binary gets, recovery included.
func installOverCurrentBinary(ctx context.Context, installer release.Installer, binPathFn func() (string, error), target *release.Target) (string, error) {
	binPath, err := binPathFn()
	if err != nil {
		return "", fmt.Errorf("resolve current binary: %w", err)
	}
	return binPath, installer.Install(ctx, target, binPath)
}

// downgradePruneWarning is the line a downgrade adds to its result when
// rolling old downgrade snapshots off failed after the reverse committed, with
// its leading newline, or "" when nothing failed.
func downgradePruneWarning(pruneErr error) string {
	if pruneErr == nil {
		return ""
	}
	return fmt.Sprintf("\nwarning: old downgrade snapshots were not removed: %v", pruneErr)
}

// normalizeReleaseTag is the shared --to normalizer for both version-traversal
// commands (downgrade and upgrade): it accepts either "v0.4.1" or "0.4.1" and
// parses the v-prefixed form into the release.Tag the resolver requires. verb
// ("downgrade" / "upgrade") only colours the error text — the accept shape is
// identical because the target is one thing (a release tag), whichever
// direction the traversal runs.
// [LAW:one-source-of-truth] release.ParseTag is the one tag grammar; this adds
// only the leniency a typed flag deserves (surrounding space, a missing v).
func normalizeReleaseTag(in string, verb string) (release.Tag, error) {
	t := strings.TrimSpace(in)
	if t == "" {
		// One message covers both an omitted flag (default "") and a
		// whitespace-only value — both TrimSpace to "" — without a branch
		// [LAW:dataflow-not-control-flow]. "requires a non-empty version" is
		// true for either, where "is required" would wrongly imply the flag was
		// absent.
		return release.Tag{}, model.ValidationError{Message: verb + ": --to requires a non-empty version"}
	}
	tag, err := release.ParseTag("v" + strings.TrimPrefix(t, "v"))
	if err != nil {
		// [LAW:one-type-per-behavior] An invalid tag is the same class of failure
		// as a missing one — bad --to input — so both return model.ValidationError
		// (exit 3), refused before the resolver makes any request.
		return release.Tag{}, model.ValidationError{Message: fmt.Sprintf("%s: --to %q: %v", verb, in, err)}
	}
	return tag, nil
}

// currentBinaryPath returns the absolute path of the running binary after
// resolving any symlinks. EvalSymlinks ensures the atomic rename overwrites
// the real file rather than the shim that points at it — matching
// scripts/install.sh's stale-binary detector's resolution rule.
func currentBinaryPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return real, nil
}
