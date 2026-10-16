package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
)

// migrationSnapshotRetention bounds how many migration-recovery snapshots are
// kept on disk. The retention budget is per-workspace; older snapshots roll
// off via Prune at the tail of every mutating Open.
const migrationSnapshotRetention = 10

// migrationPostSnapshotHookForTest is a test-only injection point that fires
// inside runMigration *after* the snapshot guard has had a chance to take
// the recovery snapshot. Production callers leave it nil; tests reassign it
// to simulate a mutation failure with the snapshot already in hand, so the
// MigrationRollbackError path is exercised. A package-level var defaulting
// to no-op, settable from store_test.go.
var migrationPostSnapshotHookForTest func() error

// migrationSnapshotLabel is the label prefix every migration-recovery snapshot
// carries; the schema version the snapshot holds follows it (see
// formatMigrationSnapshotLabel), so the name is where that version is recorded.
// legacyMigrationSnapshotLabel is the prefix lits minted before the version was
// recorded, a Unix-ns timestamp following it instead. Snapshots of that shape
// stay on disk until retention rolls them off, so they still classify as
// migration snapshots; what schema they hold is unknown.
const (
	migrationSnapshotLabel       = "pre-migrate-schema"
	legacyMigrationSnapshotLabel = "pre-migrate"
)

// IsMigrationSnapshotName reports whether name was stamped by the migration
// system (vs. produced by `lit snapshots new` or some future user flow).
//
// Snapshot names follow dbsnapshot's <unix-ns>[-<label>] scheme. The
// migration system stamps the label as
//
//	"<migrationSnapshotLabel>-<schema version>"
//
// (see formatMigrationSnapshotLabel); lits before it stamped
// "<legacyMigrationSnapshotLabel>-<unix-ns>". A plain substring match on the
// label would misclassify a user-created snapshot whose --label happened to
// contain "pre-migrate"; instead, recognize the exact stamped shapes: the
// label component equals one of the two prefixes followed by '-' and an
// all-digit suffix. A user reproducing that shape verbatim is
// indistinguishable from a real migration snapshot — that is the strongest
// signal a name-based classifier can carry without restructuring the
// snapshots-directory layout.
//
// [LAW:one-source-of-truth] Every classifier — tests, CLI listings,
// recovery tooling — routes through this predicate so renaming the label
// moves them together.
// [LAW:types-are-the-program] The predicate is the exact shape the
// migration system stamps; "matches by accident" is impossible without a
// user deliberately mimicking the format.
func IsMigrationSnapshotName(name string) bool {
	_, recorded := stampedSnapshotSuffix(name, migrationSnapshotLabel)
	_, legacy := stampedSnapshotSuffix(name, legacyMigrationSnapshotLabel)
	return recorded || legacy
}

// migrationSnapshotSchemaVersion returns the schema version a migration
// snapshot's name records — the version the workspace was at when the snapshot
// was taken. ok is false for any other name, including a legacy migration
// snapshot, which recorded none.
// [LAW:one-source-of-truth] the name is the one record of that version: it is
// written once, by formatMigrationSnapshotLabel where migrate takes the
// snapshot, and read only here.
func migrationSnapshotSchemaVersion(name string) (int64, bool) {
	suffix, ok := stampedSnapshotSuffix(name, migrationSnapshotLabel)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(suffix, 10, 64)
	return v, err == nil
}

// isStampedSnapshotName reports whether name has the exact shape a system-stamped
// snapshot carries for the given label: dbsnapshot's <unix-ns> head, then the
// label, then an all-digit suffix — i.e. "<unix-ns>-<label>-<digits>".
// The migration, downgrade, and reconcile systems each stamp a distinct label,
// so their disjoint predicates (IsMigrationSnapshotName / IsDowngradeSnapshotName
// / IsReconcileSnapshotName) share this one shape check and cannot collect each
// other's snapshots. [LAW:one-source-of-truth] the stamped-shape rule lives once;
// [LAW:one-type-per-behavior] three labels, one predicate.
func isStampedSnapshotName(name, label string) bool {
	_, ok := stampedSnapshotSuffix(name, label)
	return ok
}

// stampedSnapshotSuffix returns the all-digit suffix following label in a
// system-stamped snapshot name, and whether name has that stamped shape at all.
func stampedSnapshotSuffix(name, label string) (string, bool) {
	idx := strings.IndexByte(name, '-')
	if idx < 0 {
		return "", false
	}
	head, rest := name[:idx], name[idx+1:]
	if !isAllDigits(head) {
		return "", false
	}
	prefix := label + "-"
	if !strings.HasPrefix(rest, prefix) {
		return "", false
	}
	suffix := rest[len(prefix):]
	return suffix, isAllDigits(suffix)
}

// isAllDigits returns true iff s is a non-empty string of ASCII digits. The
// head of a dbsnapshot name is a positive unix-ns timestamp; the migration
// label suffix is the same. Sharing this check keeps the validity rule in
// one place.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// snapshotGuard is migrate()'s single owner of dbsnapshot.Take. Helpers call
// ensure() *before* any DDL/DML they are about to run; the first such call
// takes the snapshot, subsequent calls return the cached value. A migrate()
// invocation whose helpers never observe work-to-do thus takes no snapshot.
//
// [LAW:single-enforcer] All migration-driven snapshot creation flows through
// guard.ensure(); helpers must NOT call dbsnapshot.Take directly.
// [LAW:types-are-the-program] The taken pointer is the discriminator that
// distinguishes "we have a recovery point" from "we don't"; the rollback
// error and the tail-end Prune both branch on this single field.
type snapshotGuard struct {
	databaseDir  string
	snapshotsDir string
	label        string
	taken        *dbsnapshot.Snapshot
}

func newSnapshotGuard(databaseDir, snapshotsDir, label string) *snapshotGuard {
	return &snapshotGuard{
		databaseDir:  databaseDir,
		snapshotsDir: snapshotsDir,
		label:        label,
	}
}

// ensure takes the snapshot on first call and returns the cached snapshot on
// subsequent calls. Idempotent within one migrate() invocation.
func (g *snapshotGuard) ensure(ctx context.Context) (dbsnapshot.Snapshot, error) {
	if g.taken != nil {
		return *g.taken, nil
	}
	snap, err := dbsnapshot.Take(ctx, g.databaseDir, g.snapshotsDir, g.label)
	if err != nil {
		return dbsnapshot.Snapshot{}, fmt.Errorf("snapshot before migration: %w", err)
	}
	g.taken = &snap
	return snap, nil
}

// took reports the snapshot if one was taken during this migrate() invocation.
// The boolean discriminates "we have a recovery point" from "no mutation
// happened, no recovery needed".
func (g *snapshotGuard) took() (dbsnapshot.Snapshot, bool) {
	if g.taken == nil {
		return dbsnapshot.Snapshot{}, false
	}
	return *g.taken, true
}

// MigrationRollbackError wraps a migration failure that occurred after the
// recovery snapshot was taken. The error message includes the literal
// `lit snapshots restore <name>` command so the operator can recover without
// reading docs or guessing the snapshot path.
//
// [LAW:single-enforcer] The recovery-instruction format lives here so every
// caller sees the same words and the operator playbook stays consistent.
type MigrationRollbackError struct {
	Snapshot dbsnapshot.Snapshot
	Cause    error
}

func (e *MigrationRollbackError) Error() string {
	return fmt.Sprintf(
		"migrate: %v\n\nthe workspace state before this migration is preserved at:\n  %s\n\nto restore, run:\n  lit snapshots restore %s",
		e.Cause, e.Snapshot.Path, e.Snapshot.Name,
	)
}

func (e *MigrationRollbackError) Unwrap() error { return e.Cause }

// migrationSnapshotsDir returns the workspace snapshots directory derived
// from the Dolt root path. Mirrors the convention used by cli/snapshots.go's
// snapshotsDirFor (sibling of <storageDir>/dolt) and by commitLockPathForDolt
// (sibling of <storageDir>/dolt for the lock file).
//
// [LAW:one-source-of-truth] The snapshots-directory location lives in two
// callers (cli and store); both derive it from the same sibling-of-database
// convention so they cannot drift.
func migrationSnapshotsDir(databaseDir string) string {
	cleaned := filepath.Clean(databaseDir)
	return filepath.Join(filepath.Dir(cleaned), "snapshots")
}

// formatMigrationSnapshotLabel returns the label for a migration-recovery
// snapshot of a workspace at schemaVersion. Recording the version in the name
// is what lets a later, older lit tell whether restoring the snapshot gives it
// a workspace it can open (see rollbackSnapshotName).
func formatMigrationSnapshotLabel(schemaVersion int64) string {
	return fmt.Sprintf("%s-%d", migrationSnapshotLabel, schemaVersion)
}

// rollbackSnapshotName returns the newest migration snapshot whose recorded
// schema version is at or below maxSupported — the newest one a lit whose
// registry tops out there can open — or "" when none is. A snapshot recording
// no version is never offered: whether it is still ahead is unknown.
func rollbackSnapshotName(snapshotsDir string, maxSupported int64) (string, error) {
	snaps, err := dbsnapshot.List(snapshotsDir)
	if err != nil {
		return "", fmt.Errorf("list snapshots: %w", err)
	}
	for _, snap := range snaps {
		if v, ok := migrationSnapshotSchemaVersion(snap.Name); ok && v <= maxSupported {
			return snap.Name, nil
		}
	}
	return "", nil
}

// asMigrationRollbackError unwraps err to find a *MigrationRollbackError if
// any. Centralized so test code (and any future caller that needs to surface
// recovery instructions) doesn't reimplement errors.As at every callsite.
func asMigrationRollbackError(err error) (*MigrationRollbackError, bool) {
	var target *MigrationRollbackError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
