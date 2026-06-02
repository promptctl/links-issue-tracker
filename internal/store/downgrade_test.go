package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/doltcli"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

// openWorkspaceForDowngrade opens a fresh workspace at registry-max — every
// downgrade test starts from this state because Open is the only legitimate
// way to land a goose-managed workspace.
func openWorkspaceForDowngrade(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, doltRoot
}

// snapshotCount returns how many entries exist in the workspace's snapshots
// directory. Used to assert "no snapshot taken" for the refusal paths.
func snapshotCount(t *testing.T, doltRoot string) int {
	t.Helper()
	dir := migrationSnapshotsDir(doltRoot)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("readdir snapshots: %v", err)
	}
	return len(entries)
}

// TestDowngradeTargetEqualIsNoOp pins the no-op branch: target == current
// returns nil without taking a snapshot.
func TestDowngradeTargetEqualIsNoOp(t *testing.T) {
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	current, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	before := snapshotCount(t, doltRoot)
	if err := st.Downgrade(ctx, current); err != nil {
		t.Fatalf("Downgrade(current) = %v, want nil", err)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("no-op downgrade took %d snapshot(s); want 0", delta)
	}
}

// TestDowngradeTargetAheadRefused pins the forward-direction refusal: a target
// above current yields a typed error and takes no snapshot.
func TestDowngradeTargetAheadRefused(t *testing.T) {
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	current, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	before := snapshotCount(t, doltRoot)
	err = st.Downgrade(ctx, current+5)
	var ahead *DowngradeTargetAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("err = %v, want *DowngradeTargetAheadError", err)
	}
	if ahead.Current != current || ahead.Target != current+5 {
		t.Fatalf("err fields = {Current:%d Target:%d}, want {%d %d}",
			ahead.Current, ahead.Target, current, current+5)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("ahead-refusal took %d snapshot(s); want 0", delta)
	}
}

// TestDowngradeBelowBaselineRefused pins the destructive-Down guard: target
// below baselineVersion (the only Down whose execution drops every table) is
// refused before invoking goose, so the snapshot is NOT taken.
func TestDowngradeBelowBaselineRefused(t *testing.T) {
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	before := snapshotCount(t, doltRoot)
	err := st.Downgrade(ctx, baselineVersion-1)
	var below *DowngradeBelowBaselineError
	if !errors.As(err, &below) {
		t.Fatalf("err = %v, want *DowngradeBelowBaselineError", err)
	}
	if below.Target != baselineVersion-1 {
		t.Fatalf("err.Target = %d, want %d", below.Target, baselineVersion-1)
	}
	if !strings.Contains(err.Error(), "would destroy the workspace") {
		t.Fatalf("err message missing destructive warning: %v", err)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("below-baseline-refusal took %d snapshot(s); want 0", delta)
	}
}

// TestDowngradeRollbackOnFailure pins the rollback path: when a Down step
// fails after the snapshot is taken, the returned error is a typed
// DowngradeRollbackError naming the snapshot, and the snapshot exists on disk
// for `lit snapshots restore` to consume.
//
// To exercise a failing Down without a multi-migration registry we (a) seed a
// fake "applied" row in goose_db_version one above registry-max so the
// classify step admits us into the loop, and (b) inject a Down hook that
// returns a real-looking error. The hook is invoked AFTER the snapshot guard
// fires, so DowngradeRollbackError must wrap the failure.
func TestDowngradeRollbackOnFailure(t *testing.T) {
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)

	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	// Stamp one fake version above the registry so applyDownMigrations enters
	// the loop with current > target and the hook runs at least once.
	stampGooseVersion(t, ctx, st, registryMax+1)

	want := errors.New("synthetic down failure")
	migrationDownForTest = func(ctx context.Context, _ *goose.Provider) (*goose.MigrationResult, error) {
		return nil, want
	}
	t.Cleanup(func() { migrationDownForTest = nil })

	err = st.Downgrade(ctx, registryMax)
	var rb *DowngradeRollbackError
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v, want *DowngradeRollbackError", err)
	}
	if !errors.Is(rb, want) {
		t.Fatalf("rollback error does not wrap synthetic cause: %v", rb)
	}
	if rb.Snapshot.Name == "" || rb.Snapshot.Path == "" {
		t.Fatalf("rollback snapshot incomplete: %+v", rb.Snapshot)
	}
	if _, statErr := os.Stat(rb.Snapshot.Path); statErr != nil {
		t.Fatalf("snapshot path not present on disk: %v", statErr)
	}
	if !strings.Contains(rb.Error(), "lit snapshots restore "+rb.Snapshot.Name) {
		t.Fatalf("rollback message missing literal restore command: %v", rb)
	}
}

// TestDowngradeHappyPathSteppedAndCommitted pins the multi-step happy path:
// applied above target, every Down runs, recorded version lands at target,
// one Dolt commit per reversed migration.
//
// We simulate the multi-step world the registry will reach later: stamp two
// fake versions above target, inject a Down hook that "reverses" the highest
// applied row, and assert (a) the recorded version walks down to target, and
// (b) the dolt log carries the expected per-step commit messages.
func TestDowngradeHappyPathSteppedAndCommitted(t *testing.T) {
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	vA, vB := registryMax+1, registryMax+2
	stampGooseVersion(t, ctx, st, vA)
	stampGooseVersion(t, ctx, st, vB)
	if err := st.commitWorkingSet(ctx, "test: seed fake applied versions"); err != nil {
		t.Fatalf("commit seed = %v", err)
	}

	migrationDownForTest = func(ctx context.Context, _ *goose.Provider) (*goose.MigrationResult, error) {
		current, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			return nil, err
		}
		gs, err := database.NewStore(goose.DialectMySQL, gooseVersionTable)
		if err != nil {
			return nil, err
		}
		if err := gs.Delete(ctx, st.db, current); err != nil {
			return nil, err
		}
		return &goose.MigrationResult{
			Source: &goose.Source{
				Version: current,
				Path:    fmt.Sprintf("/test/%05d_fake.sql", current),
			},
		}, nil
	}
	t.Cleanup(func() { migrationDownForTest = nil })

	if err := st.Downgrade(ctx, registryMax); err != nil {
		t.Fatalf("Downgrade(%d) = %v, want nil", registryMax, err)
	}
	got, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	if got != registryMax {
		t.Fatalf("recorded version after downgrade = %d, want %d", got, registryMax)
	}
	// Happy path must take exactly one downgrade snapshot on top of Open's
	// migration snapshot.
	if delta := snapshotCount(t, doltRoot) - 1; delta != 1 {
		t.Fatalf("happy-path took %d snapshot(s) above the migration snapshot; want exactly 1",
			delta)
	}
	log, err := doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log = %v", err)
	}
	for _, v := range []int64{vA, vB} {
		want := fmt.Sprintf("downgrade: revert v%d %05d_fake.sql", v, v)
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing per-step commit %q:\n%s", want, log)
		}
	}
}

// TestDowngradeIncompleteWhenGooseExhausted pins the no-silent-fallback
// guarantee for the registry-vs-target mismatch case: if goose returns
// ErrNoNextVersion while the recorded version is still above target, the
// loop must NOT report success — it raises DowngradeIncompleteError so the
// operator sees the gap.
func TestDowngradeIncompleteWhenGooseExhausted(t *testing.T) {
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	stampGooseVersion(t, ctx, st, registryMax+1)
	if err := st.commitWorkingSet(ctx, "test: seed fake version"); err != nil {
		t.Fatalf("commit seed = %v", err)
	}
	migrationDownForTest = func(ctx context.Context, _ *goose.Provider) (*goose.MigrationResult, error) {
		return nil, goose.ErrNoNextVersion
	}
	t.Cleanup(func() { migrationDownForTest = nil })

	err = st.Downgrade(ctx, registryMax)
	var rb *DowngradeRollbackError
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v, want *DowngradeRollbackError wrapping DowngradeIncompleteError", err)
	}
	var incomplete *DowngradeIncompleteError
	if !errors.As(rb.Cause, &incomplete) {
		t.Fatalf("rb.Cause = %v, want *DowngradeIncompleteError", rb.Cause)
	}
	if incomplete.Current != registryMax+1 || incomplete.Target != registryMax {
		t.Fatalf("incomplete = {Current:%d Target:%d}, want {%d %d}",
			incomplete.Current, incomplete.Target, registryMax+1, registryMax)
	}
}

// TestIsDowngradeSnapshotNameSymmetry pins the disjointness of the two
// system-kind predicates: every migration snapshot name fails the downgrade
// predicate and vice versa. The CLI's user-snapshot classifier depends on
// these two predicates being non-overlapping — overlap would mean a
// downgrade snapshot is also classified as a migration snapshot, breaking
// the "each producer owns its own retention" invariant.
func TestIsDowngradeSnapshotNameSymmetry(t *testing.T) {
	migName := "1700000000000000000-pre-migrate-1700000000000000001"
	dgName := "1700000000000000000-lit-downgrade-1700000000000000001"
	if !IsMigrationSnapshotName(migName) {
		t.Fatalf("IsMigrationSnapshotName(%q) = false, want true", migName)
	}
	if IsDowngradeSnapshotName(migName) {
		t.Fatalf("IsDowngradeSnapshotName(%q) = true, want false (overlap with migration kind)", migName)
	}
	if !IsDowngradeSnapshotName(dgName) {
		t.Fatalf("IsDowngradeSnapshotName(%q) = false, want true", dgName)
	}
	if IsMigrationSnapshotName(dgName) {
		t.Fatalf("IsMigrationSnapshotName(%q) = true, want false (overlap with downgrade kind)", dgName)
	}
}

// TestDowngradeUntouchedOpen pins the isolation acceptance: a workspace that
// rode Downgrade back to baseline re-opens cleanly — Open's migrate() sees no
// version-ahead condition and performs no work beyond the no-op classify path.
// This is the simulation of "downgraded workspace re-opens on the prior
// binary" called out in the ticket.
func TestDowngradeUntouchedOpen(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) = %v", err)
	}
	current, err := first.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion(first) = %v", err)
	}
	// No-op downgrade (target == current) — proves the contract end-to-end
	// against the live registry without needing a Down section we don't have
	// in the embedded registry yet.
	if err := first.Downgrade(ctx, current); err != nil {
		t.Fatalf("Downgrade(no-op) = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) = %v", err)
	}

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) after downgrade = %v", err)
	}
	defer second.Close()
	again, err := second.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion(second) = %v", err)
	}
	if again != current {
		t.Fatalf("recorded version after re-Open = %d, want %d (untouched)", again, current)
	}
}

// TestDowngradeRequiresGooseManaged pins the phase guard: a workspace whose
// classify step does not yield phaseManaged refuses without taking a
// snapshot. Open always converges to phaseManaged on success, so we exercise
// the guard by opening normally and then dropping goose_db_version — that
// reverts the workspace to a phaseAdopt-shaped classification (canonical
// tables present, no goose history), which Downgrade has nothing to act on.
func TestDowngradeRequiresGooseManaged(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Drop goose_db_version so classify sees phaseAdopt (canonical shape
	// present, no goose table). Downgrade has nothing to reverse here.
	if _, err := st.db.ExecContext(ctx, `DROP TABLE `+gooseVersionTable); err != nil {
		t.Fatalf("drop goose table = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "test: drop goose table"); err != nil {
		t.Fatalf("commit drop = %v", err)
	}

	before := snapshotCount(t, doltRoot)
	err = st.Downgrade(ctx, baselineVersion)
	if err == nil {
		t.Fatal("Downgrade on non-managed workspace returned nil")
	}
	if !strings.Contains(err.Error(), "not goose-managed") {
		t.Fatalf("err = %v, want phase-guard message", err)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("phase-refusal took %d snapshot(s); want 0", delta)
	}
	// Sanity: ensure no DowngradeRollbackError is produced for refusals.
	var rb *DowngradeRollbackError
	if errors.As(err, &rb) {
		t.Fatalf("phase refusal must not produce DowngradeRollbackError: %v", rb)
	}
}

// stampGooseVersion inserts a fake applied row into goose_db_version at
// `version`. Used by tests that need to simulate "applied above registry-max"
// without standing up a real second migration.
func stampGooseVersion(t *testing.T, ctx context.Context, st *Store, version int64) {
	t.Helper()
	gs, err := database.NewStore(goose.DialectMySQL, gooseVersionTable)
	if err != nil {
		t.Fatalf("goose store = %v", err)
	}
	if err := gs.Insert(ctx, st.db, database.InsertRequest{Version: version}); err != nil {
		t.Fatalf("stamp goose version %d: %v", version, err)
	}
}

// Compile-time guards: the typed errors satisfy `error` and unwrap cleanly.
var (
	_ error          = (*DowngradeTargetAheadError)(nil)
	_ error          = (*DowngradeBelowBaselineError)(nil)
	_ error          = (*DowngradeRollbackError)(nil)
	_ dbsnapshot.Snapshot
)
