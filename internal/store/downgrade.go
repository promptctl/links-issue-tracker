package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/pressly/goose/v3"
)

// migrationDownForTest, if non-nil, replaces provider.Down(ctx) inside
// applyDownMigrations. Tests use this to drive the loop without needing a
// multi-migration registry or a real failing Down. Parallels
// migrationUpByOneForTest on the forward path.
var migrationDownForTest func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error)

// downgradeSnapshotLabel is the label prefix every downgrade-recovery snapshot
// carries. Distinct from migrationSnapshotLabel so retention budgets for the
// forward (Open) and reverse (user-invoked) paths can evolve independently.
//
// [LAW:single-enforcer] migrate() owns forward convergence at Open and stamps
// snapshots with migrationSnapshotLabel; Downgrade() owns user-invoked reverse
// and stamps with downgradeSnapshotLabel. The IsMigrationSnapshotName and
// IsDowngradeSnapshotName predicates are disjoint, so each owner's prune
// budget governs exactly its own snapshots.
const downgradeSnapshotLabel = "lit-downgrade"

// downgradeSnapshotRetention bounds how many downgrade-recovery snapshots are
// kept on disk. Independent of migrationSnapshotRetention so the two reverse
// boundaries (forward at Open, reverse at user-invoked downgrade) can evolve
// their retention policies separately.
const downgradeSnapshotRetention = 10

// IsDowngradeSnapshotName reports whether name was stamped by Downgrade
// (vs. migrate(), `lit snapshots new`, or any other producer).
//
// Mirrors IsMigrationSnapshotName: dbsnapshot's <unix-ns>-<label> scheme with
// the label component equal to "<downgradeSnapshotLabel>-<unix-ns>".
//
// [LAW:one-source-of-truth] Every classifier — tests, CLI listings, downgrade
// pruning — routes through this predicate so renaming the label moves them
// together. The user-snapshot classifier (cli/snapshots.go) excludes both
// migration and downgrade kinds, so each producer's snapshots only roll off
// under their own retention budget.
// [LAW:types-are-the-program] The predicate is the exact shape Downgrade
// stamps; "matches by accident" is impossible without a user deliberately
// mimicking the format.
func IsDowngradeSnapshotName(name string) bool {
	idx := strings.IndexByte(name, '-')
	if idx < 0 {
		return false
	}
	head, label := name[:idx], name[idx+1:]
	if !isAllDigits(head) {
		return false
	}
	const prefix = downgradeSnapshotLabel + "-"
	if !strings.HasPrefix(label, prefix) {
		return false
	}
	return isAllDigits(label[len(prefix):])
}

// downgradeMigrationFailedError carries the underlying goose error from a Down
// step. It exists so DowngradeRollbackError.Unwrap can reach the original cause
// while preserving the formatted message that names the failing version.
type downgradeMigrationFailedError struct {
	Version int64
	Cause   error
}

func (e *downgradeMigrationFailedError) Error() string {
	return fmt.Sprintf("down-migrate v%d: %v", e.Version, e.Cause)
}

func (e *downgradeMigrationFailedError) Unwrap() error { return e.Cause }

// DowngradeTargetAheadError reports that the requested target sits strictly
// above the workspace's current applied version. The forward upgrade path
// lives on Open; downgrade refuses to impersonate it. (target == current is
// a no-op handled in Downgrade itself, not this error type.)
//
// [LAW:types-are-the-program] Distinct refusal causes are distinct types, not
// a kind field on a generic DowngradeError.
type DowngradeTargetAheadError struct {
	Current int64
	Target  int64
}

func (e *DowngradeTargetAheadError) Error() string {
	return fmt.Sprintf(
		"cannot downgrade to v%d: workspace is already at v%d (use the normal forward upgrade path)",
		e.Target, e.Current,
	)
}

// DowngradeBelowBaselineError reports that the requested target sits below the
// embedded baseline. Running Down past baseline drops every table; Downgrade
// refuses before invoking goose so the destructive baseline Down is unreachable
// from this entry point. Recovery is the same `lit snapshots restore` command
// the runtime rollback path advertises.
type DowngradeBelowBaselineError struct {
	Target int64
}

func (e *DowngradeBelowBaselineError) Error() string {
	return fmt.Sprintf(
		"cannot downgrade to v%d: baseline is v%d — going below it would destroy the workspace; "+
			"restore a pre-upgrade snapshot via `lit snapshots restore <name>` instead",
		e.Target, baselineVersion,
	)
}

// DowngradeRollbackError wraps a downgrade failure that occurred after the
// recovery snapshot was taken. Parallel in shape and intent to
// MigrationRollbackError — the operator-facing recovery instruction is the
// same: `lit snapshots restore <name>`.
//
// [LAW:single-enforcer] The recovery-instruction format lives here so every
// downgrade caller sees the same words, mirroring the migrate side.
type DowngradeRollbackError struct {
	Snapshot dbsnapshot.Snapshot
	Cause    error
}

func (e *DowngradeRollbackError) Error() string {
	return fmt.Sprintf(
		"downgrade: %v\n\nthe workspace state before this downgrade is preserved at:\n  %s\n\nto restore, run:\n  lit snapshots restore %s",
		e.Cause, e.Snapshot.Path, e.Snapshot.Name,
	)
}

func (e *DowngradeRollbackError) Unwrap() error { return e.Cause }

// DowngradeIncompleteError reports that goose ran out of reversible
// migrations before reaching target — recorded version remains above target
// even though Down returned ErrNoNextVersion. A silent success in this case
// would lie to the operator that the downgrade reached the target.
//
// [LAW:no-silent-fallbacks] ErrNoNextVersion with current > target is a
// registry-vs-target mismatch, not "we're done"; it must surface so the
// operator can investigate which versions are missing inversions.
type DowngradeIncompleteError struct {
	Current int64
	Target  int64
}

func (e *DowngradeIncompleteError) Error() string {
	return fmt.Sprintf(
		"downgrade incomplete: goose has no more reversible migrations but recorded version v%d still above target v%d",
		e.Current, e.Target,
	)
}

// Downgrade reverses migrations to bring the workspace to targetSchemaVersion,
// taking a recovery snapshot first and committing one Dolt commit per reversed
// migration. It is invoked only by the future `lit downgrade` command (ticket
// .4); no Open-path code reaches it.
//
// The entire pipeline runs under the store's writer-exclusion commit lock —
// classify, snapshot, and the Down loop are serialized against every other
// writer just like migrate()'s mutations are. Acquisition is reentrant: the
// per-step commitWorkingSet calls inside applyDownMigrations short-circuit
// because the lock is already held.
//
// Refusals (no snapshot taken):
//   - target == current applied: no-op, returns nil.
//   - target > current applied: DowngradeTargetAheadError.
//   - target < baselineVersion: DowngradeBelowBaselineError.
//   - workspace not in phaseManaged: a plain error (no goose log to reverse).
//
// On a down-migration failure after the snapshot is taken, the returned error
// is a DowngradeRollbackError carrying the snapshot name and the literal
// recovery command.
//
// [LAW:single-enforcer] This is the sole reverse-migration boundary; migrate()
// remains untouched and owns only forward convergence. They share primitives
// (newGooseProvider, the snapshotGuard type, withCommitLock) but never share
// control flow.
// [LAW:dataflow-not-control-flow] The same sequence — classify → refuse-or-
// snapshot → loop-Down-and-commit → prune — runs every invocation. Variability
// lives in targetSchemaVersion and the recorded version, not in which stages
// execute.
// [LAW:one-source-of-truth] goose_db_version remains the applied-state
// authority for both directions; this loop reads it via recordedMigrationVersion
// and lets goose mutate it the same way Up does.
func (s *Store) Downgrade(ctx context.Context, targetSchemaVersion int64) error {
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return s.downgradeLocked(ctx, targetSchemaVersion)
	})
}

// downgradeLocked is the body of Downgrade executed under the commit lock.
// Split out so the lock acquisition stays a thin wrapper and the pipeline
// itself reads as the residue of its constraints.
func (s *Store) downgradeLocked(ctx context.Context, targetSchemaVersion int64) error {
	state, err := s.classifyMigrationState(ctx)
	if err != nil {
		return err
	}
	if state.phase != phaseManaged {
		return fmt.Errorf(
			"downgrade: workspace is not goose-managed (no goose_db_version table); run Open first to adopt or initialize",
		)
	}
	if targetSchemaVersion == state.appliedVersion {
		return nil
	}
	if targetSchemaVersion > state.appliedVersion {
		return &DowngradeTargetAheadError{Current: state.appliedVersion, Target: targetSchemaVersion}
	}
	if targetSchemaVersion < baselineVersion {
		return &DowngradeBelowBaselineError{Target: targetSchemaVersion}
	}

	snapshotsDir := migrationSnapshotsDir(s.doltRootDir)
	guard := newSnapshotGuard(
		s.doltRootDir,
		snapshotsDir,
		formatDowngradeSnapshotLabel(time.Now()),
	)
	snap, err := guard.ensure()
	if err != nil {
		return fmt.Errorf("downgrade: %w", err)
	}

	if err := s.applyDownMigrations(ctx, targetSchemaVersion); err != nil {
		return &DowngradeRollbackError{Snapshot: snap, Cause: err}
	}
	// [LAW:single-enforcer] Downgrade owns the prune budget for its own
	// snapshots. IsDowngradeSnapshotName is disjoint from
	// IsMigrationSnapshotName, so this sweep cannot collect migration
	// snapshots and migrate()'s sweep cannot collect downgrade ones.
	if err := dbsnapshot.PruneMatching(snapshotsDir, downgradeSnapshotRetention, IsDowngradeSnapshotName); err != nil {
		return fmt.Errorf("prune downgrade snapshots: %w", err)
	}
	return nil
}

// applyDownMigrations steps the workspace from its recorded version down to
// target, one migration at a time, with one Dolt commit per reversed step.
// Symmetric with applyPendingMigrations: a single goose provider drives the
// loop and commitWorkingSet records each step. The commit lock is already
// held by Downgrade, so commitWorkingSet's withCommitLock short-circuits.
//
// [LAW:no-silent-fallbacks] ErrNoNextVersion with current > target is a
// registry-vs-target mismatch, not a successful exit; it raises
// DowngradeIncompleteError so the operator sees that the target was never
// reached.
func (s *Store) applyDownMigrations(ctx context.Context, target int64) error {
	provider, err := newGooseProvider(s.db)
	if err != nil {
		return fmt.Errorf("construct downgrade provider: %w", err)
	}
	for {
		current, err := s.recordedMigrationVersion(ctx)
		if err != nil {
			return err
		}
		if current <= target {
			return nil
		}
		downOne := provider.Down
		if hook := migrationDownForTest; hook != nil {
			downOne = func(ctx context.Context) (*goose.MigrationResult, error) {
				return hook(ctx, provider)
			}
		}
		result, err := downOne(ctx)
		if err != nil {
			if errors.Is(err, goose.ErrNoNextVersion) {
				return &DowngradeIncompleteError{Current: current, Target: target}
			}
			return &downgradeMigrationFailedError{Version: current, Cause: err}
		}
		if result == nil {
			return fmt.Errorf("down-migrate v%d: goose returned nil result", current)
		}
		if result.Source == nil {
			return fmt.Errorf("down-migrate v%d: goose result has nil Source", current)
		}
		if err := s.commitWorkingSet(ctx, downgradeCommitMessage(result)); err != nil {
			return fmt.Errorf("commit downgrade revert of v%d: %w", result.Source.Version, err)
		}
	}
}

// downgradeCommitMessage is the one-line Dolt commit message for a reversed
// migration: `downgrade: revert v<N> <file>`, symmetric with
// migrationCommitMessage's `migrate: v<N> <file>` shape.
func downgradeCommitMessage(result *goose.MigrationResult) string {
	return fmt.Sprintf("downgrade: revert v%d %s", result.Source.Version, filepath.Base(result.Source.Path))
}

// formatDowngradeSnapshotLabel returns the label used for downgrade-recovery
// snapshots, mirroring formatMigrationSnapshotLabel's shape. The trailing
// timestamp is cosmetic — dbsnapshot.Take encodes take-time in the directory
// name — but makes the label legible in operator listings.
func formatDowngradeSnapshotLabel(t time.Time) string {
	return fmt.Sprintf("%s-%d", downgradeSnapshotLabel, t.UTC().UnixNano())
}
