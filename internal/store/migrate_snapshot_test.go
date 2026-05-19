package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/dbsnapshot"
)

// TestMigrateSnapshotFreshDBOpenTakesExactlyOneSnapshot pins the "fresh-DB
// Open takes exactly one snapshot before reconcile" acceptance criterion.
// The snapshot must exist in the workspace snapshots directory after Open
// returns and must be the only entry there.
func TestMigrateSnapshotFreshDBOpenTakesExactlyOneSnapshot(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	snaps, err := dbsnapshot.List(migrationSnapshotsDir(doltRoot))
	if err != nil {
		t.Fatalf("List snapshots error = %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("fresh-DB Open snapshot count = %d, want 1; got %+v", len(snaps), snaps)
	}
	if !strings.Contains(snaps[0].Name, migrationSnapshotLabel) {
		t.Fatalf("snapshot label missing %q in %q", migrationSnapshotLabel, snaps[0].Name)
	}
}

// TestMigrateSnapshotNoOpOpenTakesNoSnapshot pins the "no-op Open (workspace
// already at canonical shape, no pending versioned migrations) takes no
// snapshot" acceptance criterion. A second Open against a workspace that is
// already at canonical shape must not increase the snapshot count.
func TestMigrateSnapshotNoOpOpenTakesNoSnapshot(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	snapsBefore, err := dbsnapshot.List(migrationSnapshotsDir(doltRoot))
	if err != nil {
		t.Fatalf("List snapshots after first open error = %v", err)
	}

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}
	snapsAfter, err := dbsnapshot.List(migrationSnapshotsDir(doltRoot))
	if err != nil {
		t.Fatalf("List snapshots after second open error = %v", err)
	}
	if len(snapsAfter) != len(snapsBefore) {
		t.Fatalf("no-op Open created snapshot: before=%d after=%d; entries=%+v", len(snapsBefore), len(snapsAfter), snapsAfter)
	}
}

// TestMigrateSnapshotFailureSurfacesRestoreCommand pins the "simulated
// reconcile failure produces an error whose message contains the snapshot
// directory name and the literal `lit snapshots restore <name>` command"
// acceptance criterion. The failure injection fires post-snapshot, ensuring
// the rollback path is exercised.
func TestMigrateSnapshotFailureSurfacesRestoreCommand(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	sentinel := errors.New("synthetic post-snapshot failure")
	migrationPostSnapshotHookForTest = func() error { return sentinel }
	t.Cleanup(func() { migrationPostSnapshotHookForTest = nil })

	_, err := Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() returned nil error; expected MigrationRollbackError")
	}
	rollback, ok := asMigrationRollbackError(err)
	if !ok {
		t.Fatalf("error = %v (%T); expected *MigrationRollbackError", err, err)
	}
	if !errors.Is(rollback, sentinel) {
		t.Fatalf("rollback cause = %v; expected to unwrap to sentinel", rollback.Cause)
	}
	msg := rollback.Error()
	if !strings.Contains(msg, rollback.Snapshot.Path) {
		t.Fatalf("error message missing snapshot path %q: %s", rollback.Snapshot.Path, msg)
	}
	want := fmt.Sprintf("lit snapshots restore %s", rollback.Snapshot.Name)
	if !strings.Contains(msg, want) {
		t.Fatalf("error message missing literal %q: %s", want, msg)
	}
}

// TestMigrateSnapshotRestoreRoundTripsPreMutationState pins the "restore
// round-trips the workspace to its pre-mutation state" acceptance criterion.
// After a simulated failure that retains the snapshot, calling
// dbsnapshot.Restore against the snapshot's name must produce a workspace
// directory that Open can succeed on.
func TestMigrateSnapshotRestoreRoundTripsPreMutationState(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := migrationSnapshotsDir(doltRoot)

	// First Open creates the workspace + retains its pre-migrate snapshot.
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	freshSnaps, err := dbsnapshot.List(snapshotsDir)
	if err != nil {
		t.Fatalf("List after first Open error = %v", err)
	}
	if len(freshSnaps) != 1 {
		t.Fatalf("first Open snapshot count = %d, want 1", len(freshSnaps))
	}
	freshSnapshot := freshSnaps[0]

	// Inject a synthetic failure so a *new* Open captures a snapshot and
	// then errors out; the snapshot it captures should restore cleanly.
	migrationPostSnapshotHookForTest = func() error { return errors.New("synthetic failure") }
	// Force re-migration by dropping the schema_version stamp so the
	// reconciler has work to do; without work the hook never fires.
	withSchemaVersionDropped(t, ctx, doltRoot)
	failedOpen, openErr := Open(ctx, doltRoot, "test-workspace-id")
	migrationPostSnapshotHookForTest = nil
	if openErr == nil {
		_ = failedOpen.Close()
		t.Fatal("Open() after schema_version drop returned nil error; expected rollback")
	}
	rollback, ok := asMigrationRollbackError(openErr)
	if !ok {
		t.Fatalf("error = %v (%T); expected MigrationRollbackError", openErr, openErr)
	}

	// Round-trip: restore the snapshot the failure carried.
	rotated, err := dbsnapshot.Restore(doltRoot, snapshotsDir, rollback.Snapshot.Name)
	if err != nil {
		t.Fatalf("Restore error = %v", err)
	}
	if rotated == "" {
		t.Fatal("Restore returned empty rotated path; expected the pre-restore rotation to exist")
	}
	// Cleanup the rotation residue so the temp dir tears down cleanly.
	t.Cleanup(func() { _ = os.RemoveAll(rotated) })

	// After restore, Open must succeed and return a usable Store.
	restored, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after restore error = %v", err)
	}
	if _, err := restored.ListIssues(ctx, ListIssuesFilter{}); err != nil {
		t.Fatalf("ListIssues() after restore error = %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatalf("Close() after restore error = %v", err)
	}

	// Restore moves the named snapshot directory out of snapshotsDir into
	// doltRoot — so the snapshot the rollback error pointed to must be gone.
	if _, err := os.Stat(rollback.Snapshot.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored snapshot still in snapshots dir: %v", err)
	}
	// The original fresh-Open snapshot is untouched by restore.
	if _, err := os.Stat(freshSnapshot.Path); err != nil {
		t.Fatalf("unrelated first-Open snapshot disappeared: %v", err)
	}
}

// TestMigrateSnapshotPruneEnforcesRetention pins the "Prune runs at the tail
// end of every successful mutating Open with a documented retention count"
// acceptance criterion. Manufacturing more than the retention count of
// snapshots and then triggering a mutating Open must reduce the listing to
// the retention count.
func TestMigrateSnapshotPruneEnforcesRetention(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := migrationSnapshotsDir(doltRoot)

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Manufacture excess snapshots beyond the retention budget.
	for i := 0; i < migrationSnapshotRetention+5; i++ {
		if _, err := dbsnapshot.Take(doltRoot, snapshotsDir, fmt.Sprintf("synthetic-%d", i)); err != nil {
			t.Fatalf("Take synthetic %d error = %v", i, err)
		}
	}
	before, err := dbsnapshot.List(snapshotsDir)
	if err != nil {
		t.Fatalf("List before re-open error = %v", err)
	}
	if len(before) <= migrationSnapshotRetention {
		t.Fatalf("setup invariant: snapshot count = %d, expected > retention=%d", len(before), migrationSnapshotRetention)
	}

	// Force the migration to do something so Prune runs at the tail.
	withSchemaVersionDropped(t, ctx, doltRoot)

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}

	after, err := dbsnapshot.List(snapshotsDir)
	if err != nil {
		t.Fatalf("List after re-open error = %v", err)
	}
	if len(after) > migrationSnapshotRetention {
		t.Fatalf("post-migrate snapshot count = %d, want <= retention=%d", len(after), migrationSnapshotRetention)
	}
}

// withSchemaVersionDropped clears the meta.schema_version stamp by opening
// the store, deleting the row, committing, and closing — driving the next
// migrate() into the "work to do" branch.
func withSchemaVersionDropped(t *testing.T, ctx context.Context, doltRoot string) {
	t.Helper()
	prev := migrationPostSnapshotHookForTest
	migrationPostSnapshotHookForTest = nil
	defer func() { migrationPostSnapshotHookForTest = prev }()

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("withSchemaVersionDropped Open error = %v", err)
	}
	if err := st.ExecRawForTest(ctx, `DELETE FROM meta WHERE meta_key = 'schema_version'`); err != nil {
		_ = st.Close()
		t.Fatalf("ExecRawForTest delete schema_version error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "drop schema_version for test"); err != nil {
		_ = st.Close()
		t.Fatalf("commitWorkingSet error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("withSchemaVersionDropped Close error = %v", err)
	}
}
