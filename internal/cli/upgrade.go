package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// UpgradeTargetBehindError reports that the requested upgrade target's schema
// support ends BELOW the workspace's current applied schema — installing it
// would leave a binary that cannot even open this workspace. That is a backward
// move, which is `lit downgrade`'s job (it reverses the schema first, then
// installs the older binary), so upgrade refuses rather than stranding the user.
//
// [LAW:types-are-the-program] The exact mirror of store.DowngradeTargetAheadError:
// each direction refuses the other's job and names the sibling command, so
// version traversal has exactly two entry points and neither impersonates the
// other [LAW:one-type-per-behavior].
type UpgradeTargetBehindError struct {
	Current int64
	Target  int64
	Tag     string
}

func (e *UpgradeTargetBehindError) Error() string {
	return fmt.Sprintf(
		"cannot upgrade to %s: its schema support ends at v%d but this workspace is already at v%d — that is a backward move; use `lit downgrade --to %s` instead (it reverses the schema before installing the older binary)",
		e.Tag, e.Target, e.Current, e.Tag,
	)
}

// runUpgrade composes the release pipeline (internal/release) into the
// forward-direction counterpart of `lit downgrade`. Where downgrade must reverse
// the schema itself before installing the older binary — because only the
// current, newer binary holds those down-migrations — upgrade does NOT touch the
// schema: the migrations to move FORWARD live in the TARGET binary's registry,
// not this one's, so the forward migration happens on the installed binary's
// next Open() through the existing migrate() boundary (snapshot-first, quarantine,
// atomic). Upgrade's job is therefore purely resolve + install.
//
// [LAW:one-type-per-behavior] downgrade and upgrade are one behavior — version
// traversal parameterized by direction — sharing the single release seam
// (Resolver + Installer + currentBinaryPath). The schema step is owned by
// whichever binary has the migrations; that is the ONLY thing the direction
// changes, and it is why upgrade has no store schema call while downgrade does.
// [LAW:no-mode-explosion] One flag (--to). No --dry-run/--force; each must earn
// its way via a concrete user need.
//
// It is a WORKSPACE-mode command, not an app-mode one: upgrade must run in the
// exact state it is FOR — a workspace whose schema is ahead of this binary, on
// which a normal store open refuses with UnsupportedSchemaVersionError. So it
// resolves only the workspace paths here and opens the store best-effort inside
// workspaceSchemaReader, rather than letting the app pre-open (and dead-end) the
// store before this code runs. It also never writes the store — the only write
// is to the binary on disk — so read-only best-effort access is exactly right.
func runUpgrade(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runUpgradeWith(ctx, stdout, workspaceSchemaReader{ws: ws}, args, &release.HTTPResolver{}, &release.HTTPInstaller{}, currentBinaryPath)
}

// schemaReader is the schema-side dependency runUpgradeWith reads to make the
// symmetric backward-move refusal. The production implementation is
// workspaceSchemaReader; tests substitute a fake.
//
// [LAW:types-are-the-program] Upgrade needs exactly one fact — the workspace's
// applied schema version — so it depends on that one verb, not on a whole opened
// store. This keeps the pipeline testable without a real Dolt workspace.
type schemaReader interface {
	AppliedSchemaVersion(ctx context.Context) (int64, error)
}

// workspaceSchemaReader reads the workspace's applied schema version by opening
// the store best-effort. It tolerates the ONE open failure upgrade exists to fix
// — a workspace whose schema is ahead of this binary — because that refusal
// (UnsupportedSchemaVersionError) is the very message that sent the user here and
// it carries the workspace version as data. Reading the version off the refusal
// and proceeding is what stops `lit upgrade` from dead-ending on the store it
// cannot open. Any OTHER open failure propagates. [LAW:no-silent-failure]
// [LAW:dataflow-not-control-flow] the applied version is handshake DATA taken
// from a clean open's value OR the typed refusal — never inferred from a query
// happening to succeed.
type workspaceSchemaReader struct {
	ws workspace.Info
}

func (r workspaceSchemaReader) AppliedSchemaVersion(ctx context.Context) (int64, error) {
	st, err := store.OpenForRead(ctx, r.ws.DatabasePath, r.ws.WorkspaceID)
	if err != nil {
		if version, ok := appliedVersionFromOpenErr(err); ok {
			return version, nil
		}
		return 0, err
	}
	defer st.Close()
	return st.AppliedSchemaVersion(ctx)
}

// appliedVersionFromOpenErr recovers the workspace's applied schema version from
// a store-open failure when — and only when — that failure is the schema-ahead
// refusal. UnsupportedSchemaVersionError.WorkspaceVersion IS the applied version,
// stated by the binary that could not operate it, so upgrade reads it and moves
// on. handled=false means the error is something else entirely (a genuine open
// failure the caller must surface). [LAW:types-are-the-program] the typed refusal
// carries the datum; there is no string parsing of the message.
func appliedVersionFromOpenErr(err error) (version int64, handled bool) {
	var ahead *store.UnsupportedSchemaVersionError
	if errors.As(err, &ahead) {
		return ahead.WorkspaceVersion, true
	}
	return 0, false
}

// runUpgradeWith is the body parameterised over the typed dependencies so tests
// can substitute fakes. The exported runUpgrade picks the production
// implementations: workspaceSchemaReader for the best-effort schema read,
// HTTPResolver/HTTPInstaller for the release side, and currentBinaryPath for
// binary-path resolution.
//
// Sequence (each stage runs unconditionally; failure stops the pipeline):
//  1. Resolve --to <tag> through the release manifest → typed Target.
//  2. Read the workspace's applied schema; refuse a target whose schema support
//     ends below it (a backward move — UpgradeTargetBehindError names downgrade).
//  3. Resolve the running binary's real path and atomically install the target.
//  4. Print the result. The next lit invocation runs the installed binary, whose
//     Open() forward-migrates the workspace if it trails the new registry.
//
// [LAW:dataflow-not-control-flow] The pipeline runs the same stages every
// invocation; --to and the applied/target versions are data, not mode toggles.
func runUpgradeWith(
	ctx context.Context,
	stdout io.Writer,
	store schemaReader,
	args []string,
	resolver release.Resolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	fs := newCobraFlagSet("upgrade")
	to := fs.String("to", "", "Target binary version (v-prefixed git tag, e.g. v0.9.0)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit upgrade --to <version>"}
	}
	tag, err := normalizeReleaseTag(*to, "upgrade")
	if err != nil {
		return err
	}

	platform := release.CurrentPlatform()
	target, err := resolver.Resolve(ctx, tag, platform)
	if err != nil {
		return err
	}

	// [LAW:no-silent-failure] Read the applied schema BEFORE installing so a
	// backward-move request is refused without having overwritten the binary.
	// A target whose schema support ends below the workspace could not open it,
	// so installing it would strand the user; that is downgrade's job, which
	// reverses the schema first.
	current, err := store.AppliedSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("upgrade: read workspace schema version: %w", err)
	}
	if target.Manifest.Schema.Max < current {
		return &UpgradeTargetBehindError{Current: current, Target: target.Manifest.Schema.Max, Tag: tag}
	}

	binPath, err := binPathFn()
	if err != nil {
		return fmt.Errorf("upgrade: resolve current binary: %w", err)
	}

	if err := installer.Install(ctx, target, binPath); err != nil {
		// [LAW:no-silent-failure] Nothing about the workspace schema has changed
		// yet (upgrade never touches it), so recovery is simply "install the
		// target yourself, or stay on this binary."
		return fmt.Errorf(
			"upgrade: installing %s failed: %w\n\nrecover by installing %s manually (download from %s), then re-running lit",
			tag, err, tag, target.Artifact.URL,
		)
	}

	// [LAW:dataflow-not-control-flow] One print, every invocation. The forward
	// migration (if the workspace trails the new registry) is the installed
	// binary's job on its next Open — this command does not and cannot run it.
	_, err = fmt.Fprintf(stdout,
		"upgraded to %s (schema support through v%d) installed at %s\nthe next lit run migrates this workspace forward if it trails; re-run `lit version` to confirm.\n",
		tag, target.Manifest.Schema.Max, binPath,
	)
	return err
}
