package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !IsMigrationSnapshotName(snaps[0].Name) {
		t.Fatalf("snapshot %q does not match the migration-snapshot stamp shape", snaps[0].Name)
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
	// t.Cleanup guards against early-fail leaks; the explicit reset below
	// scopes the hook to exactly the failing Open so subsequent Opens in
	// this test cannot accidentally trigger it.
	migrationPostSnapshotHookForTest = func() error { return errors.New("synthetic failure") }
	t.Cleanup(func() { migrationPostSnapshotHookForTest = nil })
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

	// Manufacture excess *migration-shaped* snapshots beyond the retention
	// budget. Kind-aware retention spares non-migration snapshots, so the
	// labels here must satisfy IsMigrationSnapshotName for the prune to
	// touch them at all.
	for i := 0; i < migrationSnapshotRetention+5; i++ {
		label := formatMigrationSnapshotLabel(time.Now().Add(time.Duration(i) * time.Nanosecond))
		if _, err := dbsnapshot.Take(doltRoot, snapshotsDir, label); err != nil {
			t.Fatalf("Take migration-shaped snapshot %d error = %v", i, err)
		}
	}
	beforeMigration := countMigrationSnapshots(t, snapshotsDir)
	if beforeMigration <= migrationSnapshotRetention {
		t.Fatalf("setup invariant: migration snapshot count = %d, expected > retention=%d", beforeMigration, migrationSnapshotRetention)
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

	if got := countMigrationSnapshots(t, snapshotsDir); got > migrationSnapshotRetention {
		t.Fatalf("post-migrate migration-snapshot count = %d, want <= retention=%d", got, migrationSnapshotRetention)
	}
}

// countMigrationSnapshots is a test helper that returns the number of
// snapshots in dir whose name satisfies IsMigrationSnapshotName.
func countMigrationSnapshots(t *testing.T, dir string) int {
	t.Helper()
	list, err := dbsnapshot.List(dir)
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	n := 0
	for _, s := range list {
		if IsMigrationSnapshotName(s.Name) {
			n++
		}
	}
	return n
}

// TestMigrationPruneSparesUserSnapshots pins the kind-aware retention
// boundary: migrate() must not evict user snapshots (i.e. snapshots that
// do not satisfy IsMigrationSnapshotName) under its own retention budget.
// Without this guarantee, a mutating Open could silently delete recovery
// artifacts the user explicitly created via `lit snapshots new`.
func TestMigrationPruneSparesUserSnapshots(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := migrationSnapshotsDir(doltRoot)

	// Bootstrap the workspace so the snapshots dir exists.
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Manufacture many user snapshots (label without the migration prefix)
	// plus several extra migration snapshots beyond the migration retention.
	const userSnaps = 25
	for i := 0; i < userSnaps; i++ {
		if _, err := dbsnapshot.Take(doltRoot, snapshotsDir, "user"); err != nil {
			t.Fatalf("Take user snapshot %d: %v", i, err)
		}
	}
	for i := 0; i < migrationSnapshotRetention+5; i++ {
		label := formatMigrationSnapshotLabel(time.Now().Add(time.Duration(i) * time.Nanosecond))
		if _, err := dbsnapshot.Take(doltRoot, snapshotsDir, label); err != nil {
			t.Fatalf("Take migration snapshot %d: %v", i, err)
		}
	}

	// Drive another mutating Open so migrate() runs and prunes.
	withSchemaVersionDropped(t, ctx, doltRoot)
	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}

	listed, err := dbsnapshot.List(snapshotsDir)
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	var userKept, migrationKept int
	for _, s := range listed {
		if IsMigrationSnapshotName(s.Name) {
			migrationKept++
		} else {
			userKept++
		}
	}
	if userKept != userSnaps {
		t.Errorf("user snapshots after migration prune = %d, want %d (migration retention should not touch user snapshots)", userKept, userSnaps)
	}
	if migrationKept > migrationSnapshotRetention {
		t.Errorf("migration snapshots after prune = %d, want <= %d", migrationKept, migrationSnapshotRetention)
	}
}

// TestIsMigrationSnapshotNameRejectsUserCollisions pins the classifier's
// precision: a user `lit snapshots new --label <foo>` that happens to
// embed "pre-migrate" must NOT be misclassified as a migration snapshot.
// Only names matching the exact "<unix-ns>-pre-migrate-<unix-ns>" stamp
// produced by formatMigrationSnapshotLabel are migration snapshots.
func TestIsMigrationSnapshotNameRejectsUserCollisions(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Real migration snapshot — produced by formatMigrationSnapshotLabel.
		{"1779217187547513000-pre-migrate-1779217187516039000", true},
		// User snapshot with no --label (bare timestamp).
		{"1779217187547513000", false},
		// User --label that contains the substring but isn't the stamped shape.
		{"1779217187547513000-pre-migrate", false},
		{"1779217187547513000-pre-migrate-foo", false},
		{"1779217187547513000-pre-migrateofy", false},
		{"1779217187547513000-foo-pre-migrate-1234", false},
		// Non-snapshot directory shapes — head must be unix-ns digits.
		{"foo-pre-migrate-123", false},
		{"snap-1779217187547513000-pre-migrate-1234", false},
		{"-pre-migrate-1234", false},
		// User --label that LOOKS like the migration shape — by design,
		// indistinguishable from a real migration snapshot. Pin this so a
		// future "stricter" change has to address it deliberately.
		{"1779217187547513000-pre-migrate-9999999999999999999", true},
	}
	for _, c := range cases {
		if got := IsMigrationSnapshotName(c.name); got != c.want {
			t.Errorf("IsMigrationSnapshotName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestHasCanonicalStatusConstraintRejectsDriftedClauses pins the fingerprint
// matcher against the drifted-shape problem: a constraint that contains all
// the canonical *vocabulary* but has the epic-arm and leaf-arm conditions
// swapped (epic allowed non-NULL status) must NOT be classified as
// canonical — otherwise migrate cannot detect the drift and repair it.
//
// Inputs are pre-normalized (matches normalizeConstraintClause output) so
// the test asserts the matcher's logic, not Dolt-version-specific rendering.
func TestHasCanonicalStatusConstraintRejectsDriftedClauses(t *testing.T) {
	cases := []struct {
		name    string
		clause  string // already-normalized clause text
		want    bool
	}{
		{
			name:   "canonical text form",
			clause: "(issue_typein('epic')andstatusisnull)or(issue_typenotin('epic')andstatusisnotnullandstatusin('open','in_progress','closed'))",
			want:   true,
		},
		{
			name:   "dolt-rewritten form (parenthesized, NOT-rewritten)",
			clause: "(((issue_typein('epic'))andstatusisnull)or(((not((issue_typein('epic'))))and(not(statusisnull)))and(statusin('open','in_progress','closed'))))",
			want:   true,
		},
		{
			name:   "drifted: epic arm allows non-null status",
			clause: "(issue_typein('epic')andstatusisnotnull)or(issue_typenotin('epic')andstatusisnotnullandstatusin('open','in_progress','closed'))",
			want:   false,
		},
		{
			name:   "drifted: epic arm missing entirely",
			clause: "issue_typenotin('epic')andstatusisnotnullandstatusin('open','in_progress','closed')",
			want:   false,
		},
		{
			name:   "drifted: leaf arm omits NOT NULL gate",
			clause: "(issue_typein('epic')andstatusisnull)or(issue_typenotin('epic')andstatusin('open','in_progress','closed'))",
			want:   false,
		},
		{
			name:   "drifted: wrong status set",
			clause: "(issue_typein('epic')andstatusisnull)or(issue_typenotin('epic')andstatusisnotnullandstatusin('open','blocked'))",
			want:   false,
		},
		{
			// The drift the reviewer flagged: leaf-arm carries no
			// issue_type filter, so an epic with status='open' satisfies
			// the OR via the unguarded leaf branch.
			name:   "drifted: leaf arm missing NOT-IN epic guard",
			clause: "(issue_typein('epic')andstatusisnull)or(statusisnotnullandstatusin('open','in_progress','closed'))",
			want:   false,
		},
		{
			// Dolt rewrite of the canonical with a single-paren negation
			// "not(issue_typein('epic'))" — still canonical.
			name:   "dolt-rewritten leaf guard: single-paren negation",
			clause: "(issue_typein('epic')andstatusisnull)or(not(issue_typein('epic'))andstatusisnotnullandstatusin('open','in_progress','closed'))",
			want:   true,
		},
		{
			// Dolt's actually-observed double-paren rewrite; pin so a
			// future Dolt formatting change is caught by tests.
			name:   "dolt-rewritten leaf guard: double-paren negation",
			clause: "(((issue_typein('epic'))andstatusisnull)or(((not((issue_typein('epic'))))and(not(statusisnull)))and(statusin('open','in_progress','closed'))))",
			want:   true,
		},
	}
	for _, c := range cases {
		got := hasCanonicalStatusConstraint([]issueCheckConstraint{{name: "test", clause: c.clause}})
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
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
