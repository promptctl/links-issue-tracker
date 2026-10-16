package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// downgradeLeaf composes the schema-side Downgrade boundary (internal/store)
// and the binary-side release pipeline (internal/release) into a single
// user-facing command.
//
// Sequence (each stage runs unconditionally; failure stops the pipeline):
//  1. Resolve --to <tag> through the release manifest → typed Target.
//  2. Call Store.Downgrade(target schema). Pre-snapshot refusals propagate
//     verbatim; post-snapshot failures arrive as *DowngradeRollbackError
//     whose Error() carries the operator restore instruction.
//  3. Resolve the running binary's real path (os.Executable + EvalSymlinks)
//     and atomically install the prior binary there.
//  4. Print the result (human or JSON) and return. The user's next shell
//     prompt invokes the prior binary; no re-exec is needed.
//
// [LAW:single-enforcer] release.Resolver owns artifact resolution,
// release.Installer owns binary install, the engine's storage.SchemaMigrator
// owns schema reverse.
// This composer sequences them and contains no novel logic itself.
// [LAW:dataflow-not-control-flow] The pipeline runs the same stages every
// invocation; --to is data, not a mode toggle.
// [LAW:no-mode-explosion] One flag (--to). No --dry-run, --force, or
// --skip-snapshot; each would have to earn its way via a concrete user need.
func downgradeLeaf() appLeaf {
	return withSchemaMigrator(func() downgradeLeafShape {
		return downgradeLeafWith(&release.HTTPResolver{}, &release.HTTPInstaller{}, currentBinaryPath)
	})()
}

// withSchemaMigrator adapts a downgrade leaf to the app pipeline, asking the
// opened store for the schema verbs the work needs.
// [LAW:parse-dont-validate] Schema migration is a capability: an engine whose
// data has no shape versioned apart from the data itself has nothing to
// downgrade. Asking here yields the migrator, which already satisfies the
// narrower schemaDowngrader below — so the work never sees an engine that
// cannot revert.
// [LAW:no-ambient-temporal-coupling] The declaration passes straight through —
// only the WORK is wrapped, so the capability is requested after the parse that
// a help request never gets past.
func withSchemaMigrator(declare func() downgradeLeafShape) appLeafFn {
	return func() appLeaf {
		l := declare()
		return adaptLeaf[downgradeScope, *app.App](l, func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
			migrator, err := storage.SchemaMigration.Of(ap.Store)
			if err != nil {
				return err
			}
			current, err := version.Get()
			if err != nil {
				return fmt.Errorf("downgrade: read this binary's version info: %w", err)
			}
			return l.work(ctx, stdout, downgradeScope{schema: migrator, current: current}, positional)
		})
	}
}

// schemaDowngrader is the schema-side dependency downgrade's work calls. The
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

// downgradeScope is what downgrade's work runs against: the store's schema
// verbs, and this binary's identity, whose schema support decides whether the
// reverse can run. It mirrors upgradeScope.
type downgradeScope struct {
	schema  schemaDowngrader
	current version.Info
}

// downgradeLeafShape is downgrade's leaf over the verbs its work needs, rather
// than over a whole opened app. [LAW:types-are-the-program] the narrow
// resource is what lets a test drive the real declaration→parse→work path
// against a fake, with no store and no app anywhere in the picture.
type downgradeLeafShape = leaf[downgradeScope]

// downgradeLeafWith is the leaf parameterised over the typed dependencies so
// tests can substitute fakes. downgradeLeaf picks the production
// implementations: the store's own migrator for the schema side,
// HTTPResolver/HTTPInstaller for the release side, and currentBinaryPath for
// binary-path resolution.
func downgradeLeafWith(
	resolver release.Resolver,
	installer release.Installer,
	binPathFn func() (string, error),
) downgradeLeafShape {
	fs := newCobraFlagSet("downgrade")
	to := fs.String("to", "", "Target binary version (release tag vX.Y.Z, e.g. v0.4.1)")
	// --to is required, not optional, and the derived sentence cannot say so:
	// "values are passed as flags: --to" reads exactly like a flag you may omit.
	// Requiredness is the leaf's own knowledge. [LAW:one-source-of-truth]
	return downgradeLeafShape{fs: fs, positionals: 0, usage: "usage: lit downgrade --to <version>", work: func(ctx context.Context, stdout io.Writer, scope downgradeScope, _ []string) error {
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

		binPath, err := binPathFn()
		if err != nil {
			return fmt.Errorf("downgrade: resolve current binary: %w", err)
		}

		if err := installer.Install(ctx, target, binPath); err != nil {
			// [LAW:no-silent-failure] schema is already downgraded at this point;
			// surface the install failure with the exact recovery the operator
			// needs (run the prior binary themselves, or restore the snapshot).
			return fmt.Errorf(
				"downgrade: schema reversed to v%d but installing prior binary failed: %w\n\nrecover by either:\n  - installing %s manually (download from %s), then re-running lit; or\n  - restoring the pre-downgrade snapshot via `lit snapshots list` + `lit snapshots restore <name>`",
				target.Manifest.Schema.Max, err, tag, target.Artifact.URL,
			)
		}
		telemetry.Set(ctx, "downgrade.installed_path", binPath)

		// [LAW:dataflow-not-control-flow] The post-install step is a single
		// print. Re-exec'ing into the prior binary on Unix or printing a human
		// re-run line on Windows would add a platform mode for no measurable
		// benefit — the rename has already happened, the user's next shell
		// prompt runs the prior binary.
		if _, err := fmt.Fprintf(stdout,
			"downgraded to %s (schema v%d) installed at %s\nre-run `lit version` to confirm.\n",
			tag, target.Manifest.Schema.Max, binPath,
		); err != nil {
			return err
		}
		// [LAW:no-silent-failure] the downgrade is complete, so a prune failure
		// does not fail the command; it is said here and on the event.
		if done.PruneErr != nil {
			_, err = fmt.Fprintf(stdout, "warning: old downgrade snapshots were not removed: %v\n", done.PruneErr)
		}
		return err
	}}
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
