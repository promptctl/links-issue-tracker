package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
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
	// Force re-migration by dropping goose's history so the next Open
	// re-adopts the workspace; without work to do, the hook never fires.
	withGooseHistoryDropped(t, ctx, doltRoot)
	failedOpen, openErr := Open(ctx, doltRoot, "test-workspace-id")
	migrationPostSnapshotHookForTest = nil
	if openErr == nil {
		_ = failedOpen.Close()
		t.Fatal("Open() after goose-history drop returned nil error; expected rollback")
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
	withGooseHistoryDropped(t, ctx, doltRoot)

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
	withGooseHistoryDropped(t, ctx, doltRoot)
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

// TestDataSurvivesFailedMigrationSnapshotRestore is the data-survival contract
// test: a workspace seeded with rich state (N>=3 issues with title, status,
// priority, topic, labels, and assignee; a dependency edge; a comment; and
// field-history events) is byte-identical after a failed migration is recovered
// via dbsnapshot.Restore. [LAW:behavior-not-structure] [LAW:verifiable-goals]
func TestDataSurvivesFailedMigrationSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := migrationSnapshotsDir(doltRoot)

	// Step 1: Seed the workspace with rich data covering all required fields.
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(seed) error = %v", err)
	}

	// Issue A: feature, urgent priority, labels, transitions to in_progress with assignee.
	issueA, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "Alpha feature",
		Topic:     "feature",
		IssueType: "feature",
		Priority:  model.PriorityUrgent,
		Labels:    []string{"backend", "critical"},
	})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID:   issueA.ID,
		Action:    "start",
		CreatedBy: "alice",
		Assignee:  "alice",
	}); err != nil {
		t.Fatalf("TransitionIssue(A start) error = %v", err)
	}

	// Issue B: task, receives a comment.
	issueB, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "Beta task",
		Topic:     "backend",
		IssueType: "task",
		Priority:  model.PriorityNormal,
	})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}
	if _, err := st.AddComment(ctx, AddCommentInput{
		IssueID:   issueB.ID,
		Body:      "Needs review before merge.",
		CreatedBy: "bob",
	}); err != nil {
		t.Fatalf("AddComment(B) error = %v", err)
	}

	// Issue C: chore with label.
	if _, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "Gamma chore",
		Topic:     "infra",
		IssueType: "chore",
		Priority:  model.PriorityNormal,
		Labels:    []string{"infra"},
	}); err != nil {
		t.Fatalf("CreateIssue(C) error = %v", err)
	}

	// Dependency edge: A depends on B (B blocks A).
	// blocks convention: src_id=dependent, dst_id=dependency.
	if _, err := st.AddRelation(ctx, AddRelationInput{
		SrcID:     issueA.ID,
		DstID:     issueB.ID,
		Type:      "blocks",
		CreatedBy: "alice",
	}); err != nil {
		t.Fatalf("AddRelation(A depends on B) error = %v", err)
	}

	// Step 2: Capture the full pre-mutation state.
	before, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export(before) error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed) error = %v", err)
	}

	// Verify the seeded state has the expected shape before relying on it.
	if n := len(before.Issues); n < 3 {
		t.Fatalf("seed invariant: issue count = %d, want >= 3", n)
	}
	if len(before.Relations) == 0 {
		t.Fatal("seed invariant: no relations in export")
	}
	if len(before.Comments) == 0 {
		t.Fatal("seed invariant: no comments in export")
	}
	if len(before.Events) == 0 {
		t.Fatal("seed invariant: no events in export")
	}

	// Step 3: Drop goose history so the next Open re-enters phaseAdopt.
	withGooseHistoryDropped(t, ctx, doltRoot)

	// Step 4: Inject a post-snapshot failure so a mutating Open captures a
	// snapshot then errors, returning a MigrationRollbackError carrying it.
	sentinel := errors.New("synthetic post-snapshot failure")
	migrationPostSnapshotHookForTest = func() error { return sentinel }
	t.Cleanup(func() { migrationPostSnapshotHookForTest = nil })
	_, openErr := Open(ctx, doltRoot, "test-workspace-id")
	migrationPostSnapshotHookForTest = nil
	if openErr == nil {
		t.Fatal("Open() after goose-history drop returned nil error; expected MigrationRollbackError")
	}
	rollback, ok := asMigrationRollbackError(openErr)
	if !ok {
		t.Fatalf("error = %v (%T); expected *MigrationRollbackError", openErr, openErr)
	}
	if !errors.Is(rollback, sentinel) {
		t.Fatalf("rollback cause = %v; want to unwrap to sentinel", rollback.Cause)
	}

	// Step 5: Restore the snapshot the failure carried.
	rotated, err := dbsnapshot.Restore(doltRoot, snapshotsDir, rollback.Snapshot.Name)
	if err != nil {
		t.Fatalf("Restore error = %v", err)
	}
	if rotated == "" {
		t.Fatal("Restore returned empty rotated path; expected the pre-restore rotation to exist")
	}
	t.Cleanup(func() { _ = os.RemoveAll(rotated) })

	// Step 6: Reopen, export, and assert byte-identical state.
	restored, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(restored) error = %v", err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Errorf("Close(restored) error = %v", err)
		}
	}()

	after, err := restored.Export(ctx)
	if err != nil {
		t.Fatalf("Export(after) error = %v", err)
	}
	assertExportStateIdentical(t, before, after)

	// Step 7: Assert the restored workspace is writable.
	if _, err := restored.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "Post-restore write check",
		Topic:     "verify",
		IssueType: "task",
	}); err != nil {
		t.Fatalf("CreateIssue(post-restore) error = %v", err)
	}
}

// assertExportStateIdentical compares two Export values by marshaling both to
// JSON with ExportedAt zeroed. ExportedAt is the only field that legitimately
// differs between two exports of the same workspace state.
// [LAW:behavior-not-structure]
func assertExportStateIdentical(t *testing.T, want, got model.Export) {
	t.Helper()
	want.ExportedAt = time.Time{}
	got.ExportedAt = time.Time{}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("exported state after restore differs from pre-mutation state:\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
}

// withGooseHistoryDropped removes the goose_db_version table from an existing
// workspace, leaving the full canonical schema in place. The next Open then
// classifies the workspace as phaseAdopt (pre-goose schema, no history) and
// re-stamps the baseline version — a mutating migrate() pass, which is what
// the snapshot/failure/prune tests need to drive.
func withGooseHistoryDropped(t *testing.T, ctx context.Context, doltRoot string) {
	t.Helper()
	prev := migrationPostSnapshotHookForTest
	migrationPostSnapshotHookForTest = nil
	defer func() { migrationPostSnapshotHookForTest = prev }()

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("withGooseHistoryDropped Open error = %v", err)
	}
	// A real pre-goose workspace predates every migration, so revert post-baseline
	// migrations to the baseline shape before stripping goose history.
	revertToBaseline(t, st)
	if err := st.ExecRawForTest(ctx, `DROP TABLE IF EXISTS goose_db_version`); err != nil {
		_ = st.Close()
		t.Fatalf("ExecRawForTest drop goose_db_version error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "drop goose history for test"); err != nil {
		_ = st.Close()
		t.Fatalf("commitWorkingSet error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("withGooseHistoryDropped Close error = %v", err)
	}
}
