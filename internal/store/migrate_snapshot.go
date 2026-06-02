package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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
// MigrationRollbackError path is exercised.
//
// Modeled on commit_lock.go's commitLockPIDRunning swap-in pattern: a
// package-level var defaulting to no-op, settable from store_test.go.
var migrationPostSnapshotHookForTest func() error

// migrationSnapshotLabel is the label prefix every migration-recovery snapshot
// carries. The pre-restore rotation in dbsnapshot.Restore uses the directory
// itself, not the label, so the label is purely human-facing.
const migrationSnapshotLabel = "pre-migrate"

// IsMigrationSnapshotName reports whether name was stamped by the migration
// system (vs. produced by `lit snapshots new` or some future user flow).
//
// Snapshot names follow dbsnapshot's <unix-ns>[-<label>] scheme. The
// migration system always stamps the label as
//
//	"<migrationSnapshotLabel>-<unix-ns>"
//
// (see formatMigrationSnapshotLabel). A plain substring match on the label
// would misclassify a user-created snapshot whose --label happened to
// contain "pre-migrate"; instead, recognize the exact stamped shape:
// the label component equals migrationSnapshotLabel followed by '-' and an
// all-digit timestamp suffix. A user reproducing that shape verbatim is
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
	idx := strings.IndexByte(name, '-')
	if idx < 0 {
		return false
	}
	head, label := name[:idx], name[idx+1:]
	if !isAllDigits(head) {
		return false
	}
	const prefix = migrationSnapshotLabel + "-"
	if !strings.HasPrefix(label, prefix) {
		return false
	}
	suffix := label[len(prefix):]
	return isAllDigits(suffix)
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
