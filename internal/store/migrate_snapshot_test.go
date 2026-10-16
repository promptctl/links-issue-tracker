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
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestMigrateSnapshotFreshDBOpenTakesExactlyOneSnapshot pins the "fresh-DB
// Open takes exactly one snapshot before reconcile" acceptance criterion.
// The snapshot must exist in the workspace snapshots directory after Open
// returns and must be the only entry there.
func TestMigrateSnapshotFreshDBOpenTakesExactlyOneSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	snaps, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	snapsBefore, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
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
	snapsAfter, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
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
	// serial: no t.Parallel — rewrites the package-level
	// migrationPostSnapshotHookForTest hook.
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
	// serial: no t.Parallel — rewrites the package-level
	// migrationPostSnapshotHookForTest hook.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := dbsnapshot.DirFor(doltRoot)

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

	// Force re-migration by dropping goose's history so the next Open
	// re-adopts the workspace; without work to do, the hook never fires.
	// The drop runs before the hook is installed so the helper's own
	// internal Open cannot trip it.
	withGooseHistoryDropped(t, ctx, doltRoot)
	// Inject a synthetic failure so a *new* Open captures a snapshot and
	// then errors out; the snapshot it captures should restore cleanly.
	// t.Cleanup guards against early-fail leaks; the explicit reset below
	// scopes the hook to exactly the failing Open so subsequent Opens in
	// this test cannot accidentally trigger it.
	migrationPostSnapshotHookForTest = func() error { return errors.New("synthetic failure") }
	t.Cleanup(func() { migrationPostSnapshotHookForTest = nil })
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
	if _, err := restored.ListIssues(ctx, storage.ListIssuesFilter{}); err != nil {
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := dbsnapshot.DirFor(doltRoot)

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
		if _, err := dbsnapshot.Take(context.Background(), doltRoot, snapshotsDir, label); err != nil {
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := dbsnapshot.DirFor(doltRoot)

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
		if _, err := dbsnapshot.Take(context.Background(), doltRoot, snapshotsDir, "user"); err != nil {
			t.Fatalf("Take user snapshot %d: %v", i, err)
		}
	}
	for i := 0; i < migrationSnapshotRetention+5; i++ {
		label := formatMigrationSnapshotLabel(time.Now().Add(time.Duration(i) * time.Nanosecond))
		if _, err := dbsnapshot.Take(context.Background(), doltRoot, snapshotsDir, label); err != nil {
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
	t.Parallel()
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
	// serial: no t.Parallel — rewrites the package-level
	// migrationPostSnapshotHookForTest hook.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapshotsDir := dbsnapshot.DirFor(doltRoot)

	// Step 1: Seed the workspace with rich data covering all required fields.
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(seed) error = %v", err)
	}

	// The epic every leaf below lives in: a leaf is never created outside one.
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix:    "test",
		Title:     "Seed epic",
		Topic:     "seed",
		IssueType: "epic",
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}

	// Issue A: feature, urgent priority, labels, transitions to in_progress with assignee.
	issueA, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix:    "test",
		Title:     "Alpha feature",
		Topic:     "feature",
		IssueType: "feature",
		Priority:  model.PriorityUrgent,
		Labels:    []string{"backend", "critical"},
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	if _, err := st.Apply(ctx, issueA.ID, storage.Change{Action: model.Start{Assignee: "alice"}, Actor: "alice"}); err != nil {
		t.Fatalf("StartIssue(A) error = %v", err)
	}

	// Issue B: task, receives a comment.
	issueB, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix:    "test",
		Title:     "Beta task",
		Topic:     "backend",
		IssueType: "task",
		Priority:  model.PriorityNormal,
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{
		IssueID:   issueB.ID,
		Body:      "Needs review before merge.",
		CreatedBy: "bob",
	}); err != nil {
		t.Fatalf("AddComment(B) error = %v", err)
	}

	// Issue C: chore with label.
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix:    "test",
		Title:     "Gamma chore",
		Topic:     "infra",
		IssueType: "chore",
		Priority:  model.PriorityNormal,
		Labels:    []string{"infra"},
		ParentID:  epic.ID,
	}); err != nil {
		t.Fatalf("CreateIssue(C) error = %v", err)
	}

	// Dependency edge: A depends on B (B blocks A).
	// blocks convention: src_id=dependent, dst_id=dependency.
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID:     issueA.ID,
		DstID:     issueB.ID,
		Type:      "blocked-by",
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
	if _, err := restored.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix:    "test",
		Title:     "Post-restore write check",
		Topic:     "verify",
		IssueType: "task",
		ParentID:  epic.ID,
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
// Callers that install migrationPostSnapshotHookForTest (all serial) do so
// only after this helper returns; the helper itself must not touch that
// package-level hook, because it also runs inside parallel tests, where any
// write to it races every concurrently migrating Open.
func withGooseHistoryDropped(t *testing.T, ctx context.Context, doltRoot string) {
	t.Helper()
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

// rollbackHistory is the two-step history: a workspace migrated forward from
// v3, then again from v5. It returns its migration snapshots newest first —
// the one holding v5, the one holding v3, and the empty workspace the fresh
// open captured — so a test can break one before searching.
func rollbackHistory(t *testing.T, ctx context.Context, doltRoot string) []dbsnapshot.Snapshot {
	t.Helper()
	withStore(t, ctx, doltRoot, func(*Store) {}) // fresh: migrates from an empty workspace
	for _, from := range []int64{3, 5} {
		withStore(t, ctx, doltRoot, func(st *Store) {
			if _, err := st.Downgrade(ctx, from); err != nil {
				t.Fatalf("Downgrade(%d) error = %v", from, err)
			}
		})
		withStore(t, ctx, doltRoot, func(*Store) {}) // migrates forward from `from`
	}
	listed, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	var migrationSnaps []dbsnapshot.Snapshot
	for _, snap := range listed {
		if IsMigrationSnapshotName(snap.Name) {
			migrationSnaps = append(migrationSnaps, snap)
		}
	}
	if len(migrationSnaps) != 3 {
		t.Fatalf("migration snapshots = %+v, want 3 (from v5, from v3, fresh) in listing %+v", migrationSnaps, listed)
	}
	return migrationSnaps
}

// breakSnapshotBaseline drops a baseline column inside a snapshot, as a later
// migration that rewrote the baseline would leave it.
func breakSnapshotBaseline(t *testing.T, ctx context.Context, snapshotPath string) {
	t.Helper()
	snap, err := openStoreConnection(ctx, snapshotPath, "test-workspace-id", engineWrite)
	if err != nil {
		t.Fatalf("open snapshot %s: %v", snapshotPath, err)
	}
	mustExec(t, ctx, snap, `ALTER TABLE issues DROP COLUMN title`)
	mustCommit(t, ctx, snap, "test: break the snapshot's baseline")
	if err := snap.Close(); err != nil {
		t.Fatalf("close snapshot %s: %v", snapshotPath, err)
	}
}

// TestRollbackSnapshotIsOneTheOlderLitOpens runs the search over the two-step
// history by the rule a lit's open applies: a snapshot ahead of the lit is
// offered while its baseline is intact, since the lit operates it and it is
// the closest rollback; one whose baseline is broken is passed over; the empty
// workspace the fresh open captured is never offered, since restoring it would
// discard every issue.
func TestRollbackSnapshotIsOneTheOlderLitOpens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snaps := rollbackHistory(t, ctx, doltRoot)
	fromV5, fromV3 := snaps[0], snaps[1]

	type want struct {
		snapshot string
		examined int
	}
	search := func(stage string, cases map[int64]want) {
		t.Helper()
		withStore(t, ctx, doltRoot, func(st *Store) {
			for maxSupported, w := range cases {
				got := st.searchRollbackSnapshot(ctx, maxSupported)
				if got.err != nil || got.unreadable != 0 {
					t.Errorf("%s: search(max %d) err = %v, unreadable = %d; want every snapshot read", stage, maxSupported, got.err, got.unreadable)
				}
				if got.snapshot != w.snapshot || got.examined != w.examined {
					t.Errorf("%s: search(max %d) = %q after %d examined, want %q after %d", stage, maxSupported, got.snapshot, got.examined, w.snapshot, w.examined)
				}
			}
		})
	}

	search("baselines intact", map[int64]want{
		2: {fromV5.Name, 1},
		3: {fromV5.Name, 1},
		5: {fromV5.Name, 1},
	})
	breakSnapshotBaseline(t, ctx, fromV5.Path)
	search("v5 baseline broken", map[int64]want{
		2: {fromV3.Name, 2},
		3: {fromV3.Name, 2},
		4: {fromV3.Name, 2},
		5: {fromV5.Name, 1},
	})
	breakSnapshotBaseline(t, ctx, fromV3.Path)
	search("v3 and v5 baselines broken", map[int64]want{
		2: {"", 3},
		3: {fromV3.Name, 2},
	})
}

// TestRollbackSnapshotPassesOverAnUnreadableSnapshot pins that a migration
// snapshot the search cannot read does not hide an older one it can offer, and
// that the unreadable one is counted and named beside the offer.
func TestRollbackSnapshotPassesOverAnUnreadableSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	withStore(t, ctx, doltRoot, func(st *Store) {
		if _, err := st.Downgrade(ctx, 3); err != nil {
			t.Fatalf("Downgrade(3) error = %v", err)
		}
	})
	withStore(t, ctx, doltRoot, func(*Store) {}) // migrates forward from v3, snapshotting it
	listed, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	if len(listed) == 0 || !IsMigrationSnapshotName(listed[0].Name) {
		t.Fatalf("newest snapshot is not the migration snapshot from v3: %+v", listed)
	}
	fromV3 := listed[0].Name

	// Newer than every snapshot taken: a copy of the v3 snapshot under a
	// migration-shaped name, its Dolt manifest overwritten so no engine opens it.
	now := time.Now().UnixNano()
	unreadable := fmt.Sprintf("%d-%s", now, formatMigrationSnapshotLabel(time.Unix(0, now)))
	unreadableDir := filepath.Join(dbsnapshot.DirFor(doltRoot), unreadable)
	if err := dbsnapshot.CloneTree(ctx, listed[0].Path, unreadableDir); err != nil {
		t.Fatalf("clone %s: %v", fromV3, err)
	}
	if err := os.WriteFile(filepath.Join(unreadableDir, doltDatabaseName, ".dolt", "noms", "manifest"), []byte("not a manifest"), 0o644); err != nil {
		t.Fatalf("corrupt the manifest: %v", err)
	}

	withStore(t, ctx, doltRoot, func(st *Store) {
		got := st.searchRollbackSnapshot(ctx, 3)
		if got.snapshot != fromV3 || got.examined != 2 || got.unreadable != 1 {
			t.Errorf("search(max 3) = %q after %d examined, %d unreadable; want %q past the unreadable %q, 2 examined, 1 unreadable", got.snapshot, got.examined, got.unreadable, fromV3, unreadable)
		}
		if got.err == nil || !strings.Contains(got.err.Error(), unreadable) {
			t.Errorf("search err = %v; want the unreadable snapshot %q named", got.err, unreadable)
		}
	})
}

// TestRollbackSnapshotSearchStopsWhenCanceled pins that a canceled search ends
// as a cancellation, never as snapshots that could not be read.
func TestRollbackSnapshotSearchStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	rollbackHistory(t, ctx, doltRoot)
	withStore(t, ctx, doltRoot, func(st *Store) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		got := st.searchRollbackSnapshot(canceled, 3)
		if !errors.Is(got.err, context.Canceled) || got.unreadable != 0 || got.examined != 0 || got.snapshot != "" {
			t.Errorf("canceled search = %+v; want only the cancellation, nothing examined or unreadable", got)
		}
	})
}
