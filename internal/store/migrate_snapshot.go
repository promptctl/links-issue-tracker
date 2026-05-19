package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/dbsnapshot"
)

// migrationSnapshotRetention bounds how many migration-recovery snapshots are
// kept on disk. The retention budget is per-workspace; older snapshots roll
// off via Prune at the tail of every mutating Open.
const migrationSnapshotRetention = 10

// migrationPostSnapshotHookForTest is a test-only injection point that fires
// inside runMigration *after* the snapshot guard has had a chance to take
// the recovery snapshot. Production callers leave it nil; tests reassign it
// to simulate a mutation failure with the snapshot already in hand, so the
// MigrationRollbackError path is exercised.
//
// Modeled on commit_lock.go's commitLockPIDRunning swap-in pattern: a
// package-level var defaulting to no-op, settable from store_test.go.
var migrationPostSnapshotHookForTest func() error

// migrationSnapshotLabel is the label prefix every migration-recovery snapshot
// carries. The pre-restore rotation in dbsnapshot.Restore uses the directory
// itself, not the label, so the label is purely human-facing.
const migrationSnapshotLabel = "pre-migrate"

// IsMigrationSnapshotName reports whether name is a migration-recovery
// snapshot (vs. one produced by `lit snapshots new` or some future user
// flow). Exposed so tests and CLI surfaces classify entries against the
// same source of truth that produced them, instead of hardcoding the
// "pre-migrate" string.
//
// [LAW:one-source-of-truth] The migration-snapshot label lives in this
// package; every classifier — tests, CLI listings, recovery tooling —
// routes through this predicate so renaming the label moves them together.
func IsMigrationSnapshotName(name string) bool {
	return strings.Contains(name, migrationSnapshotLabel)
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
func (g *snapshotGuard) ensure() (dbsnapshot.Snapshot, error) {
	if g.taken != nil {
		return *g.taken, nil
	}
	snap, err := dbsnapshot.Take(g.databaseDir, g.snapshotsDir, g.label)
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

// formatMigrationSnapshotLabel returns the label used for migration-recovery
// snapshots. The trailing timestamp is purely cosmetic — dbsnapshot.Take
// already encodes the take-time in the directory name — but it makes the
// label readable in operator-facing listings.
func formatMigrationSnapshotLabel(t time.Time) string {
	return fmt.Sprintf("%s-%d", migrationSnapshotLabel, t.UTC().UnixNano())
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
