package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// UpgradeTargetBehindError reports that the requested upgrade target's schema
// support ends BELOW the workspace's current applied schema — installing it
// would leave a binary that cannot even open this workspace. Upgrade refuses
// rather than stranding the user; the remedy depends on whether THIS binary can
// open the workspace, carried by WorkspaceOpenable:
//
//   - openable: the workspace opened cleanly, so this is a plain backward move —
//     `lit downgrade` CAN run (it reverses the schema, then installs the older
//     binary). Point at it.
//   - not openable: the applied version was recovered from a schema-ahead refusal
//     (the workspace is ahead of this binary), so `lit downgrade` cannot run here
//     either — it would hit the same refusal on open. The remedy is a NEWER
//     target, never a reverse.
//
// [LAW:types-are-the-program] The message is a discriminated rendering of the
// domain fact (openable), not a guess — the same misleading-remediation trap this
// PR set out to kill would return if it unconditionally named downgrade. It is
// the mirror of store.DowngradeTargetAheadError: each direction refuses the
// other's job, so version traversal has exactly two entry points and neither
// impersonates the other [LAW:one-type-per-behavior].
type UpgradeTargetBehindError struct {
	Current int64
	Target  int64
	Tag     string
	// WorkspaceOpenable is true when this binary opened the workspace to read
	// Current, false when Current was recovered from the schema-ahead refusal.
	WorkspaceOpenable bool
}

func (e *UpgradeTargetBehindError) Error() string {
	// [LAW:dataflow-not-control-flow] one field selects the remediation text; the
	// refusal itself (target below workspace) is the same fact either way.
	if e.WorkspaceOpenable {
		return fmt.Sprintf(
			"cannot upgrade to %s: its schema support ends at v%d but this workspace is already at v%d — that is a backward move; use `lit downgrade --to %s` instead (it reverses the schema before installing the older binary)",
			e.Tag, e.Target, e.Current, e.Tag,
		)
	}
	return fmt.Sprintf(
		"cannot upgrade to %s: it supports only through schema v%d but this workspace is at v%d, which this binary cannot open — pick an upgrade target whose schema support reaches v%d or newer (this binary is too old to reverse the schema here, so an older target is not an option)",
		e.Tag, e.Target, e.Current, e.Current,
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
// [LAW:no-mode-explosion] One flag (--to), whose default value is the latest
// published release (resolved through the release feed) — a default, not a
// second mode; every stage below runs identically either way. No
// --dry-run/--force; each must earn its way via a concrete user need.
// Downgrade keeps requiring --to: a backward move stays deliberate, so
// "latest" can never be a downgrade target.
//
// It is a WORKSPACE-mode command, not an app-mode one: upgrade must run in the
// exact state it is FOR — a workspace whose schema is ahead of this binary, on
// which a normal store open refuses with UnsupportedSchemaVersionError. So it
// resolves only the workspace paths here and opens the store best-effort inside
// workspaceSchemaReader, rather than letting the app pre-open (and dead-end) the
// store before this code runs. It also never writes the store — the only write
// is to the binary on disk — so read-only best-effort access is exactly right.
func runUpgrade(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	current, err := version.Get()
	if err != nil {
		return fmt.Errorf("upgrade: read this binary's version info: %w", err)
	}
	return runUpgradeWith(ctx, stdout, workspaceSchemaReader{ws: ws}, args, current, &release.HTTPResolver{}, &release.HTTPInstaller{}, currentBinaryPath)
}

// upgradeResolver is upgrade's release-side dependency: tag→Target resolution
// plus the latest-tag lookup that serves as --to's default value. Downgrade
// depends on release.Resolver alone — it has no latest, by design.
type upgradeResolver interface {
	release.Resolver
	release.LatestResolver
}

// workspaceSchema is what upgrade learns about the target workspace before it
// decides: the applied schema version, and whether THIS binary could open the
// workspace to read it. Openable is the discriminator the backward-move
// remediation turns on — false means the version was recovered from a
// schema-ahead refusal, so a reverse (`lit downgrade`) cannot run here.
// [LAW:types-are-the-program] the two facts travel together so no consumer can
// hold a version without also knowing whether it came from a clean open.
type workspaceSchema struct {
	AppliedVersion int64
	Openable       bool
}

// schemaReader is the schema-side dependency runUpgradeWith reads to make the
// symmetric backward-move refusal. The production implementation is
// workspaceSchemaReader; tests substitute a fake.
//
// [LAW:types-are-the-program] Upgrade needs exactly the applied version and
// whether the workspace opened — so it depends on that one verb, not on a whole
// opened store. This keeps the pipeline testable without a real Dolt workspace.
type schemaReader interface {
	ReadWorkspaceSchema(ctx context.Context) (workspaceSchema, error)
}

// workspaceSchemaReader reads the workspace schema by opening the store
// best-effort. It tolerates the ONE open failure upgrade exists to fix — a
// workspace whose schema is ahead of this binary — because that refusal
// (UnsupportedSchemaVersionError) is the very message that sent the user here and
// it carries the workspace version as data. Reading the version off the refusal
// (and marking the workspace not-Openable) is what stops `lit upgrade` from
// dead-ending on the store it cannot open. Any OTHER open failure propagates.
// [LAW:no-silent-failure]
// [LAW:dataflow-not-control-flow] the applied version is handshake DATA taken
// from a clean open's value OR the typed refusal — never inferred from a query
// happening to succeed.
type workspaceSchemaReader struct {
	ws workspace.Info
}

func (r workspaceSchemaReader) ReadWorkspaceSchema(ctx context.Context) (schema workspaceSchema, err error) {
	st, err := engine.Open(ctx, engine.ReadOnly, r.ws.DatabasePath, r.ws.WorkspaceID)
	if err != nil {
		if v, ok := appliedVersionFromOpenErr(err); ok {
			return workspaceSchema{AppliedVersion: v, Openable: false}, nil
		}
		return workspaceSchema{}, err
	}
	// [LAW:no-silent-failure] Store.Close releases the workspace shared lock; a
	// discarded close error could leave the workspace pinned against a later
	// snapshot restore. Surface it into the return, but never let it mask a real
	// read error — the primary failure wins, close only fills a clean return.
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	migrator, err := storage.SchemaMigration.Of(st)
	if err != nil {
		return workspaceSchema{}, err
	}
	version, err := migrator.AppliedSchemaVersion(ctx)
	if err != nil {
		return workspaceSchema{}, err
	}
	return workspaceSchema{AppliedVersion: version, Openable: true}, nil
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
// version.Get for this binary's identity, HTTPResolver/HTTPInstaller for the
// release side, and currentBinaryPath for binary-path resolution.
//
// Sequence (each stage runs unconditionally; failure stops the pipeline):
//  1. Resolve the target tag: the normalized --to value, or — when --to is
//     omitted — the latest published release, from the release feed.
//  2. Resolve the tag through the release manifest → typed Target.
//  3. Read the workspace schema; refuse a target whose schema support ends below
//     it (a backward move — UpgradeTargetBehindError names the right remedy from
//     whether this binary can open the workspace: a reverse, or a newer target).
//  4. An unpinned invocation that is already on the resolved release keeps it:
//     a friendly no-op naming the version. A pinned --to always installs — the
//     explicit tag is a command (and the reinstall path for a damaged binary).
//  5. Resolve the running binary's real path and atomically install the target.
//  6. Print from → to. The next lit invocation runs the installed binary, whose
//     Open() forward-migrates the workspace if it trails the new registry.
//
// [LAW:dataflow-not-control-flow] The pipeline runs the same stages every
// invocation; the tag, its origin (pinned or defaulted), and the
// applied/target versions are data, not mode toggles.
func runUpgradeWith(
	ctx context.Context,
	stdout io.Writer,
	schema schemaReader,
	args []string,
	current version.Info,
	resolver upgradeResolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	fs := newCobraFlagSet("upgrade")
	to := fs.String("to", "", "Target binary version (v-prefixed git tag, e.g. v0.9.0); omit to upgrade to the latest release")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit upgrade [--to <version>]"}
	}
	// [LAW:parse-dont-validate] --to's default value is the latest published
	// release; an empty flag is not an error to guard against but the input
	// that selects the feed as the tag's source. pinned records the origin —
	// it decides only whether already-current is a no-op or a reinstall.
	pinned := strings.TrimSpace(*to) != ""
	var tag string
	var err error
	if pinned {
		tag, err = normalizeReleaseTag(*to, "upgrade")
	} else {
		tag, err = resolver.LatestTag(ctx)
	}
	if err != nil {
		return err
	}

	platform := release.CurrentPlatform()
	target, err := resolver.Resolve(ctx, tag, platform)
	if err != nil {
		return err
	}

	// [LAW:no-silent-failure] Read the workspace schema BEFORE installing so a
	// backward-move request is refused without having overwritten the binary.
	// A target whose schema support ends below the workspace could not open it,
	// so installing it would strand the user; the refusal names the right remedy
	// (downgrade vs. a newer target) from whether this binary can open it.
	ws, err := schema.ReadWorkspaceSchema(ctx)
	if err != nil {
		return fmt.Errorf("upgrade: read workspace schema version: %w", err)
	}
	if target.Manifest.Schema.Max < ws.AppliedVersion {
		return &UpgradeTargetBehindError{
			Current:           ws.AppliedVersion,
			Target:            target.Manifest.Schema.Max,
			Tag:               tag,
			WorkspaceOpenable: ws.Openable,
		}
	}

	// An unpinned invocation asked for "current"; being current already
	// satisfies it. A pinned --to falls through and installs — the explicit
	// tag is a command, and the reinstall path for a damaged binary. A dev
	// build has no stamped Version, compares equal to no release, and so
	// always proceeds. This check runs AFTER the backward-move refusal: a
	// workspace ahead of even the latest release must be refused loudly
	// (naming both schema ranges), never soothed with "already current".
	if !pinned && target.Manifest.Version == current.Version {
		_, err = fmt.Fprintf(stdout,
			"already current: %s is the latest release; kept the installed binary.\n",
			tag,
		)
		return err
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
		"upgraded %s → %s (schema support through v%d) installed at %s\nthe next lit run migrates this workspace forward if it trails; re-run `lit version` to confirm.\n",
		fromLabel(current), tag, target.Manifest.Schema.Max, binPath,
	)
	return err
}

// fromLabel renders the running binary's identity for the from → to line: the
// v-prefixed release version, or a dev-build marker when nothing was stamped
// at link time (Info.IsDev is the typed form of that fact — no re-derivation
// from an empty string here).
func fromLabel(current version.Info) string {
	if current.IsDev {
		return "dev build"
	}
	return "v" + current.Version
}
