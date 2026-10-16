package store

import (
	"context"
	"errors"
	"fmt"
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
// MigrationRollbackError path is exercised. A package-level var defaulting
// to no-op, settable from store_test.go.
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
	return isStampedSnapshotName(name, migrationSnapshotLabel)
}

// isStampedSnapshotName reports whether name has the exact shape a system-stamped
// snapshot carries for the given label: dbsnapshot's <unix-ns> head, then the
// label, then an all-digit timestamp suffix — i.e. "<unix-ns>-<label>-<unix-ns>".
// The migration, downgrade, and reconcile systems each stamp a distinct label,
// so their disjoint predicates (IsMigrationSnapshotName / IsDowngradeSnapshotName
// / IsReconcileSnapshotName) share this one shape check and cannot collect each
// other's snapshots. [LAW:one-source-of-truth] the stamped-shape rule lives once;
// [LAW:one-type-per-behavior] three labels, one predicate.
func isStampedSnapshotName(name, label string) bool {
	idx := strings.IndexByte(name, '-')
	if idx < 0 {
		return false
	}
	head, rest := name[:idx], name[idx+1:]
	if !isAllDigits(head) {
		return false
	}
	prefix := label + "-"
	if !strings.HasPrefix(rest, prefix) {
		return false
	}
	return isAllDigits(rest[len(prefix):])
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
	databaseDir string
	label       string
	taken       *dbsnapshot.Snapshot
}

func newSnapshotGuard(databaseDir, label string) *snapshotGuard {
	return &snapshotGuard{
		databaseDir: databaseDir,
		label:       label,
	}
}

// ensure takes the snapshot on first call and returns the cached snapshot on
// subsequent calls. Idempotent within one migrate() invocation.
func (g *snapshotGuard) ensure(ctx context.Context) (dbsnapshot.Snapshot, error) {
	if g.taken != nil {
		return *g.taken, nil
	}
	snap, err := dbsnapshot.Take(ctx, g.databaseDir, g.label)
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

// formatMigrationSnapshotLabel returns the label used for migration-recovery
// snapshots. The trailing timestamp is purely cosmetic — dbsnapshot.Take
// already encodes the take-time in the directory name — but it makes the
// label readable in operator-facing listings.
func formatMigrationSnapshotLabel(t time.Time) string {
	return fmt.Sprintf("%s-%d", migrationSnapshotLabel, t.UTC().UnixNano())
}

// rollbackSearch is what the refusal's search for a lossy rollback found.
type rollbackSearch struct {
	// snapshot is the newest migration snapshot this lit operates, or "".
	snapshot string
	// examined counts the migration snapshots the search read to an answer or
	// failed to read; unreadable counts the failures.
	examined, unreadable int
	// err is why snapshots could not be listed or read, or why the search
	// stopped early; nil when every snapshot it reached was read.
	err error
}

// searchRollbackSnapshot finds the newest migration snapshot holding a
// workspace a lit whose registry tops out at maxSupported operates
// (snapshotOperable). A snapshot that cannot be read is passed over and counted,
// and why is kept: whether it was a closer rollback than the one offered is
// unknown. [LAW:no-silent-failure] A canceled ctx ends the search; it is not a
// fact about the snapshot being read.
//
// [LAW:one-source-of-truth] what a snapshot holds is read from the snapshot
// itself, so every migration snapshot answers alike whichever lit minted it,
// and its name is left in the shape every released lit classifies.
func (s *Store) searchRollbackSnapshot(ctx context.Context, maxSupported int64) rollbackSearch {
	snaps, err := dbsnapshot.List(s.doltRootDir)
	if err != nil {
		return rollbackSearch{err: fmt.Errorf("list snapshots: %w", err)}
	}
	var search rollbackSearch
	var errs []error
	for _, snap := range snaps {
		if !IsMigrationSnapshotName(snap.Name) {
			continue
		}
		operable, err := snapshotOperable(ctx, snap.Path, s.workspaceID, maxSupported)
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("stopped looking for a rollback snapshot: %w", ctxErr))
			break
		}
		search.examined++
		if err != nil {
			search.unreadable++
			errs = append(errs, fmt.Errorf("read the schema snapshot %s holds: %w", snap.Name, err))
			continue
		}
		if operable {
			search.snapshot = snap.Name
			break
		}
	}
	search.err = errors.Join(errs...)
	return search
}

// snapshotOperable reports whether a lit whose registry tops out at
// maxSupported operates the workspace a snapshot holds, by the rule its open
// applies: a schema at or below maxSupported, or one ahead of it whose live
// schema still carries the baseline (baselineCarried, which the refusal uses).
// The empty workspace the fresh-open snapshot captured holds no data, so it is
// never a rollback.
//
// The snapshot is read in place by a read engine. Dolt rewrites its journal
// bookkeeping when an engine opens a store, here as on the first open after
// the snapshot is restored; the data the snapshot holds is unchanged. A clone
// would avoid that rewrite at the price of a full copy on a filesystem with no
// copy-on-write, and of a store-sized tree stranded by a hard exit.
func snapshotOperable(ctx context.Context, snapshotPath, workspaceID string, maxSupported int64) (_ bool, err error) {
	snap, err := openStoreConnection(ctx, snapshotPath, workspaceID, engineRead)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, snap.Close()) }()
	state, err := snap.classifyMigrationState(ctx)
	if err != nil {
		return false, err
	}
	switch {
	case state.phase == phaseFresh:
		return false, nil
	case state.appliedVersion <= maxSupported:
		return true, nil
	default:
		_, carried, err := snap.baselineCarried(ctx)
		return carried, err
	}
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
