package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/release"
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
// release.Installer owns binary install, store.Downgrade owns schema reverse.
// This composer sequences them and contains no novel logic itself.
// [LAW:dataflow-not-control-flow] The pipeline runs the same stages every
// invocation; --to is data, not a mode toggle.
// [LAW:no-mode-explosion] One flag (--to). No --dry-run, --force, or
// --skip-snapshot; each would have to earn its way via a concrete user need.
func runDowngrade(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runDowngradeWith(ctx, stdout, ap.Store, args, &release.HTTPResolver{}, &release.HTTPInstaller{}, currentBinaryPath)
}

// schemaDowngrader is the schema-side dependency runDowngradeWith calls. The
// production implementation is *store.Store; tests substitute a fake.
//
// [LAW:types-are-the-program] The CLI doesn't need the full *store.Store API
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
	fs.JSONFlag()
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit downgrade --to <version> [--json]")
	}
	tag, err := normalizeDowngradeTag(*to)
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
		// [LAW:no-silent-fallbacks] schema is already downgraded at this point;
		// surface the install failure with the exact recovery the operator
		// needs (run the prior binary themselves, or restore the snapshot).
		return fmt.Errorf(
			"downgrade: schema reversed to v%d but installing prior binary failed: %w\n\nrecover by either:\n  - installing %s manually (download from %s), then re-running lit; or\n  - restoring the pre-downgrade snapshot via `lit snapshots list` + `lit snapshots restore <name>`",
			target.Manifest.Schema.Max, err, tag, target.Artifact.URL,
		)
	}

	payload := downgradeResult{
		Status:     "downgraded",
		Target:     tag,
		Schema:     target.Manifest.Schema.Max,
		BinaryPath: binPath,
	}
	// [LAW:dataflow-not-control-flow] The post-install step is a single print.
	// An earlier draft re-exec'd into the prior binary on Unix and printed a
	// human re-run line on Windows, but both branches violated the --json
	// contract (extra stdout after the JSON document) and added a platform
	// mode for no measurable benefit — the rename has already happened, the
	// user's next shell prompt runs the prior binary.
	return printValue(stdout, payload, func(w io.Writer, v any) error {
		p := v.(downgradeResult)
		_, err := fmt.Fprintf(w,
			"downgraded to %s (schema v%d) installed at %s\nre-run `lit version` to confirm.\n",
			p.Target, p.Schema, p.BinaryPath,
		)
		return err
	})
}

// downgradeResult is the typed JSON payload `lit downgrade` emits with --json.
// Schema is an int64 to match version.Info.SchemaSupport's numeric encoding;
// machine consumers don't have to parse a string to recover the number.
//
// [LAW:types-are-the-program] One struct projects to both text and JSON;
// the text renderer reads typed fields, never re-deriving them from strings.
type downgradeResult struct {
	Status     string `json:"status"`
	Target     string `json:"target"`
	Schema     int64  `json:"schema"`
	BinaryPath string `json:"binary_path"`
}

// normalizeDowngradeTag accepts either "v0.4.1" or "0.4.1" and returns the
// v-prefixed form the resolver requires. Mirrors mkmanifest's tag/version
// distinction: the v-prefixed tag is the URL path segment.
func normalizeDowngradeTag(in string) (string, error) {
	t := strings.TrimSpace(in)
	if t == "" {
		return "", errors.New("downgrade: --to <version> is required")
	}
	if !strings.HasPrefix(t, "v") {
		t = "v" + t
	}
	// Reject obvious URL-path foot-guns; resolver re-validates the v-prefix.
	if strings.ContainsAny(t, "/\\") || strings.Contains(t, "..") || strings.ContainsAny(t, " \t\r\n") {
		return "", fmt.Errorf("downgrade: --to %q is not a valid release tag", in)
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
