package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// runDowngrade composes the schema-side Downgrade boundary (internal/store)
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
func runDowngrade(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	// [LAW:parse-dont-validate] Schema migration is a capability: an engine
	// whose data has no shape versioned apart from the data itself has nothing
	// to downgrade. Asking here yields the migrator, which already satisfies
	// the narrower schemaDowngrader below — so the pipeline never sees an
	// engine that cannot revert.
	migrator, err := storage.SchemaMigration.Of(ap.Store)
	if err != nil {
		return err
	}
	return runDowngradeWith(ctx, stdout, migrator, args, &release.HTTPResolver{}, &release.HTTPInstaller{}, currentBinaryPath)
}

// schemaDowngrader is the schema-side dependency runDowngradeWith calls. The
// production implementation is the engine's storage.SchemaMigrator; tests
// substitute a fake.
//
// [LAW:types-are-the-program] The CLI doesn't need the full storage.Store API
// for downgrade; it needs exactly this verb. Narrowing the dependency to the
// one method that's used makes the pipeline testable without a real Dolt
// workspace and prevents callers from coupling to incidental Store methods.
type schemaDowngrader interface {
	Downgrade(ctx context.Context, targetSchemaVersion int64) error
}

// runDowngradeWith is the body parameterised over the typed dependencies so
// tests can substitute fakes. The exported runDowngrade picks the production
// implementations: ap.Store for the schema side, HTTPResolver/HTTPInstaller
// for the release side, and currentBinaryPath for binary-path resolution.
func runDowngradeWith(
	ctx context.Context,
	stdout io.Writer,
	store schemaDowngrader,
	args []string,
	resolver release.Resolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	fs := newCobraFlagSet("downgrade")
	to := fs.String("to", "", "Target binary version (v-prefixed git tag, e.g. v0.4.1)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit downgrade --to <version>"}
	}
	tag, err := normalizeReleaseTag(*to, "downgrade")
	if err != nil {
		return err
	}

	platform := release.CurrentPlatform()
	target, err := resolver.Resolve(ctx, tag, platform)
	if err != nil {
		return err
	}

	if err := store.Downgrade(ctx, target.Manifest.Schema.Max); err != nil {
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

	// [LAW:dataflow-not-control-flow] The post-install step is a single print.
	// An earlier draft re-exec'd into the prior binary on Unix and printed a
	// human re-run line on Windows, but both branches added a platform mode for
	// no measurable benefit — the rename has already happened, the user's next
	// shell prompt runs the prior binary.
	_, err = fmt.Fprintf(stdout,
		"downgraded to %s (schema v%d) installed at %s\nre-run `lit version` to confirm.\n",
		tag, target.Manifest.Schema.Max, binPath,
	)
	return err
}

// normalizeReleaseTag is the shared --to normalizer for both version-traversal
// commands (downgrade and upgrade): it accepts either "v0.4.1" or "0.4.1" and
// returns the v-prefixed form the resolver requires. verb ("downgrade" /
// "upgrade") only colours the error text — the accept shape is identical because
// the target is one thing (a release tag), whichever direction the traversal
// runs. Mirrors mkmanifest's tag/version distinction: the v-prefixed tag is the
// URL path segment.
// [LAW:one-source-of-truth] one tag grammar, not a per-command copy that could
// drift on what a legal tag is; both callsites invoke it directly, so there is
// no per-direction wrapper to echo this rule.
func normalizeReleaseTag(in string, verb string) (string, error) {
	t := strings.TrimSpace(in)
	if t == "" {
		// One message covers both an omitted flag (default "") and a
		// whitespace-only value — both TrimSpace to "" — without a branch
		// [LAW:dataflow-not-control-flow]. "requires a non-empty version" is true
		// for either, where "is required" wrongly implied the flag was absent.
		return "", ValidationError{Message: verb + ": --to requires a non-empty version"}
	}
	if !strings.HasPrefix(t, "v") {
		t = "v" + t
	}
	// Reject obvious URL-path foot-guns; resolver re-validates the v-prefix.
	// [LAW:one-type-per-behavior] An invalid tag is the same class of failure as a
	// missing one — bad --to input — so both return ValidationError (exit 3), not
	// a plain error that would dispatch to the generic exit 1.
	if strings.ContainsAny(t, "/\\") || strings.Contains(t, "..") || strings.ContainsAny(t, " \t\r\n") {
		return "", ValidationError{Message: fmt.Sprintf("%s: --to %q is not a valid release tag", verb, in)}
	}
	return t, nil
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
