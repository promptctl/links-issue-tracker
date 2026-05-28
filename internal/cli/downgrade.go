package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/release"
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
//  4. syscall.Exec into the prior binary on Unix; on Windows print the one-
//     command re-run instruction.
//
// [LAW:single-enforcer] release.Resolver owns artifact resolution,
// release.Installer owns binary install, store.Downgrade owns schema reverse.
// This composer sequences them and contains no novel logic itself.
// [LAW:dataflow-not-control-flow] The pipeline runs the same stages every
// invocation; --to is data, not a mode toggle.
// [LAW:no-mode-explosion] One flag (--to). No --dry-run, --force, or
// --skip-snapshot; each would have to earn its way via a concrete user need.
func runDowngrade(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runDowngradeWith(ctx, stdout, ap, args, &release.HTTPResolver{}, &release.HTTPInstaller{})
}

// runDowngradeWith is the body parameterised over Resolver and Installer so
// tests can substitute fakes. The exported runDowngrade picks the production
// defaults.
func runDowngradeWith(
	ctx context.Context,
	stdout io.Writer,
	ap *app.App,
	args []string,
	resolver release.Resolver,
	installer release.Installer,
) error {
	fs := newCobraFlagSet("downgrade")
	to := fs.String("to", "", "Target binary version (v-prefixed git tag, e.g. v0.4.1)")
	jsonOut := fs.Bool("json", false, "Output JSON")
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

	if err := ap.Store.Downgrade(ctx, target.Manifest.Schema.Max); err != nil {
		return err
	}

	binPath, err := currentBinaryPath()
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

	payload := map[string]string{
		"status":      "downgraded",
		"target":      tag,
		"schema":      fmt.Sprintf("%d", target.Manifest.Schema.Max),
		"binary_path": binPath,
	}
	if err := printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		_, err := fmt.Fprintf(w,
			"downgraded to %s (schema v%s) installed at %s\n",
			p["target"], p["schema"], p["binary_path"],
		)
		return err
	}); err != nil {
		return err
	}

	// Release the workspace before exec — the next process opens it fresh.
	_ = ap.Close()

	return execIntoBinary(binPath, append([]string{binPath}, "version"), os.Environ(), stdout)
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
