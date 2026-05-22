package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestQuarantineTableCreatedByFreshOpen pins that a fresh-workspace Open
// creates the migration_quarantine table before any goose migration runs,
// so it is always available for recording failures.
func TestQuarantineTableCreatedByFreshOpen(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	exists, err := st.tableExists(ctx, "migration_quarantine")
	if err != nil {
		t.Fatalf("tableExists error = %v", err)
	}
	if !exists {
		t.Fatal("migration_quarantine table not created by fresh Open")
	}
}

// TestQuarantineTableCreatedByAdoption pins that the quarantine table exists
// after Open runs the adoption path (pre-goose workspace without goose
// history).
func TestQuarantineTableCreatedByAdoption(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Simulate a pre-goose workspace by dropping goose history.
	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second/adoption) error = %v", err)
	}
	defer second.Close()

	exists, err := second.tableExists(ctx, "migration_quarantine")
	if err != nil {
		t.Fatalf("tableExists error = %v", err)
	}
	if !exists {
		t.Fatal("migration_quarantine table missing after adoption path")
	}
}

// TestCheckPendingQuarantineBlocksPendingVersion pins that checkPendingQuarantine
// returns a QuarantineBlockError when a quarantine row exists for a version
// greater than the current applied version.
func TestCheckPendingQuarantineBlocksPendingVersion(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Manually seed a quarantine row for a hypothetical future migration v2.
	if err := st.ExecRawForTest(ctx,
		`INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (2, '00002_test.sql', 'simulated failure', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed quarantine row error = %v", err)
	}

	// Applied = 1 → v2 is pending and quarantined.
	err = st.checkPendingQuarantine(ctx, 1)
	var qErr *QuarantineBlockError
	if !errors.As(err, &qErr) {
		t.Fatalf("checkPendingQuarantine(applied=1) = %v (%T), want *QuarantineBlockError", err, err)
	}
	if qErr.Version != 2 {
		t.Errorf("QuarantineBlockError.Version = %d, want 2", qErr.Version)
	}
	if qErr.Name != "00002_test.sql" {
		t.Errorf("QuarantineBlockError.Name = %q, want %q", qErr.Name, "00002_test.sql")
	}
	if qErr.ErrorText != "simulated failure" {
		t.Errorf("QuarantineBlockError.ErrorText = %q, want %q", qErr.ErrorText, "simulated failure")
	}
}

// TestCheckPendingQuarantineAllowsAlreadyApplied pins that a quarantine row for
// a version ≤ appliedVersion does NOT block Open (the version is already past).
func TestCheckPendingQuarantineAllowsAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Quarantine row for v1 — which is the currently applied version.
	if err := st.ExecRawForTest(ctx,
		`INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (1, '00001_baseline.sql', 'old failure', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed quarantine row error = %v", err)
	}

	// Applied = 1 → v1 is not pending (already applied). Should be nil.
	if err := st.checkPendingQuarantine(ctx, 1); err != nil {
		t.Fatalf("checkPendingQuarantine(applied=1, quarantine_version=1) = %v, want nil", err)
	}
}

// TestQuarantineTableSurvivesCheckpointReset is the critical invariant test:
// after a Dolt checkpoint reset, the migration_quarantine table still exists.
// This verifies the bootstrap-before-checkpoint ordering that prevents the
// PR #119 bug (quarantine table erased by the very reset it was meant to survive).
func TestQuarantineTableSurvivesCheckpointReset(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Verify quarantine table is present before taking the checkpoint.
	if ok, err := st.tableExists(ctx, "migration_quarantine"); err != nil || !ok {
		t.Fatalf("quarantine table absent before checkpoint (err=%v, ok=%v)", err, ok)
	}

	// Take a Dolt checkpoint (quarantine table is in the committed state the
	// checkpoint points to).
	cp, err := st.CreateCheckpoint(ctx, "test-reset")
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}

	// Simulate some post-checkpoint work (like a migration would do).
	if err := st.ExecRawForTest(ctx, `INSERT INTO meta(meta_key, meta_value) VALUES ('post_cp', '1')`); err != nil {
		t.Fatalf("post-checkpoint insert error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "post-checkpoint work"); err != nil {
		t.Fatalf("commit post-checkpoint work error = %v", err)
	}

	// Hard reset to checkpoint — this must not erase the quarantine table.
	if err := st.ResetToCheckpoint(ctx, cp.Name); err != nil {
		t.Fatalf("ResetToCheckpoint() error = %v", err)
	}

	// Quarantine table must still exist.
	if ok, err := st.tableExists(ctx, "migration_quarantine"); err != nil || !ok {
		t.Fatalf("quarantine table ERASED by checkpoint reset (err=%v, ok=%v) — bootstrap-before-checkpoint ordering violated", err, ok)
	}

	// Post-checkpoint work must be gone (proves the reset actually worked).
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'post_cp'`).Scan(&count); err != nil {
		t.Fatalf("count post-checkpoint row error = %v", err)
	}
	if count != 0 {
		t.Error("post-checkpoint meta row survived reset (reset did not revert as expected)")
	}
}

// TestQuarantineRowInsertedAfterReset pins the ordering: after a simulated
// migration failure, the quarantine row is inserted AFTER the checkpoint reset
// (so it persists in the post-reset database, not in the pre-reset state that
// was just discarded).
func TestQuarantineRowInsertedAfterReset(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Create the checkpoint (quarantine table already committed by Open).
	cp, err := st.CreateCheckpoint(ctx, "pre-migrate")
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}

	// Simulate migration work + commit (like a successful v2 goose step would do).
	if err := st.ExecRawForTest(ctx, `INSERT INTO meta(meta_key, meta_value) VALUES ('migration_work', '1')`); err != nil {
		t.Fatalf("simulate migration work error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "simulate migration v2"); err != nil {
		t.Fatalf("commit simulate migration error = %v", err)
	}

	// Simulate a v3 failure: reset to checkpoint, then insert quarantine row.
	if err := st.ResetToCheckpoint(ctx, cp.Name); err != nil {
		t.Fatalf("ResetToCheckpoint() error = %v", err)
	}

	// Migration work from v2 must be gone (proves reset worked).
	var migCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'migration_work'`).Scan(&migCount); err != nil {
		t.Fatalf("count migration work row error = %v", err)
	}
	if migCount != 0 {
		t.Error("migration work survived checkpoint reset")
	}

	// Insert quarantine row AFTER reset — this is the key ordering.
	if err := st.recordQuarantine(ctx, 3, "00003_test.sql", "simulated v3 failure"); err != nil {
		t.Fatalf("recordQuarantine error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "migrate: quarantine v3"); err != nil {
		t.Fatalf("commit quarantine row error = %v", err)
	}

	// Quarantine row must exist in the post-reset database.
	var qCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_quarantine WHERE version = 3`).Scan(&qCount); err != nil {
		t.Fatalf("count quarantine row error = %v", err)
	}
	if qCount != 1 {
		t.Error("quarantine row missing after post-reset insert")
	}
}

// TestMigrationFailureCheckpointPath pins the full failure path: a simulated
// migration failure creates a Dolt checkpoint, resets the working set, records
// a quarantine row, and returns a CheckpointResetError. The next Open then
// returns a QuarantineBlockError because the pending version is quarantined.
func TestMigrationFailureCheckpointPath(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// First Open: bootstrap the workspace.
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Force re-migration so applyPendingMigrations runs on next Open.
	withGooseHistoryDropped(t, ctx, doltRoot)

	// Inject a migration failure via the test hook.
	sentinel := errors.New("simulated migration body error")
	migrationUpByOneForTest = func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error) {
		return &goose.MigrationResult{
			Source: &goose.Source{Version: 2, Path: "00002_test.sql"},
		}, sentinel
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(ctx, doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	if openErr == nil {
		t.Fatal("Open() returned nil error; expected CheckpointResetError wrapped in MigrationRollbackError")
	}

	// Error chain must include MigrationRollbackError (outer, from snapshot layer).
	rollback, ok := asMigrationRollbackError(openErr)
	if !ok {
		t.Fatalf("error = %v (%T); expected wrapped *MigrationRollbackError", openErr, openErr)
	}

	// Cause must be a CheckpointResetError (inner, from Dolt-checkpoint layer).
	var cpErr *CheckpointResetError
	if !errors.As(rollback.Cause, &cpErr) {
		t.Fatalf("rollback.Cause = %v (%T); expected *CheckpointResetError", rollback.Cause, rollback.Cause)
	}
	if cpErr.Version != 2 {
		t.Errorf("CheckpointResetError.Version = %d, want 2", cpErr.Version)
	}
	if cpErr.Checkpoint.Name == "" {
		t.Error("CheckpointResetError.Checkpoint.Name is empty")
	}
	if !errors.Is(cpErr.Cause, sentinel) {
		t.Errorf("CheckpointResetError.Cause does not wrap sentinel: %v", cpErr.Cause)
	}

	// Error message must mention the checkpoint name and the restore command.
	msg := rollback.Error()
	if !contains(msg, cpErr.Checkpoint.Name) {
		t.Errorf("error message missing checkpoint name %q: %s", cpErr.Checkpoint.Name, msg)
	}
	if !contains(msg, "lit snapshots restore") {
		t.Errorf("error message missing 'lit snapshots restore': %s", msg)
	}

	// After reset the workspace is phaseManaged (goose_db_version restored by
	// the checkpoint) with appliedVersion=1=registryMax, so willMutate=false
	// and this Open succeeds without re-running migrations.
	secondForList, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open after failed migration error = %v", err)
	}
	defer secondForList.Close()

	// The checkpoint branch created during the failed Open must still exist:
	// PruneCheckpoints only fires on a successful migration pass, which did
	// not run on this willMutate=false Open.
	listed, err := secondForList.ListCheckpoints(ctx, migrationCheckpointPrefix)
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	found := false
	for _, cp := range listed {
		if cp.Name == cpErr.Checkpoint.Name {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("checkpoint branch %q not found in dolt_branches (listed: %v)", cpErr.Checkpoint.Name, listed)
	}

	// The top Dolt commit must be the quarantine record written after the reset.
	var topMessage string
	if err := secondForList.db.QueryRowContext(ctx, `SELECT message FROM dolt_log() LIMIT 1`).Scan(&topMessage); err != nil {
		t.Fatalf("query dolt_log error = %v", err)
	}
	if !contains(topMessage, "quarantine") {
		t.Errorf("top dolt commit message %q does not mention quarantine", topMessage)
	}
}

// TestMigrationCheckpointCreatedByFreshOpen pins that a fresh-workspace Open
// creates at least one pre-migrate checkpoint branch as part of applying the
// baseline migration.
func TestMigrationCheckpointCreatedByFreshOpen(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	cps, err := st.ListCheckpoints(ctx, migrationCheckpointPrefix)
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	if len(cps) == 0 {
		t.Fatal("no migration checkpoints after fresh Open; expected at least one")
	}
}

// TestMigrationCheckpointRetentionBounded pins that repeated mutating Opens
// do not accumulate checkpoints beyond the retention limit.
func TestMigrationCheckpointRetentionBounded(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Drive more Opens than the retention count by dropping goose history.
	for i := 0; i < migrationCheckpointRetention+3; i++ {
		st, err := Open(ctx, doltRoot, "test-workspace-id")
		if err != nil {
			t.Fatalf("Open(%d) error = %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%d) error = %v", i, err)
		}
		if i < migrationCheckpointRetention+2 {
			withGooseHistoryDropped(t, ctx, doltRoot)
		}
	}

	// One final Open to read the checkpoint list.
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("final Open() error = %v", err)
	}
	defer st.Close()

	cps, err := st.ListCheckpoints(ctx, migrationCheckpointPrefix)
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	if len(cps) > migrationCheckpointRetention {
		t.Errorf("checkpoint count = %d, want <= retention=%d", len(cps), migrationCheckpointRetention)
	}
}

// TestReopenBlockedByQuarantineOnAdoptionPath pins that Open returns a
// QuarantineBlockError (inside MigrationRollbackError) when the adoption path
// (phaseAdopt, appliedVersion=0) encounters a quarantine row for a pending
// version. This tests criterion 2 end-to-end through Open() itself.
func TestReopenBlockedByQuarantineOnAdoptionPath(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// First Open: bootstrap the workspace (creates quarantine table).
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}

	// Seed a quarantine row for v1 so checkPendingQuarantine(appliedVersion=0)
	// finds it on the next adoption-path Open.
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (1, '00001_baseline.sql', 'simulated failure', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed quarantine row error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "seed quarantine row for test"); err != nil {
		t.Fatalf("commit quarantine row error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Drop goose history so the next Open classifies as phaseAdopt
	// (appliedVersion=0). checkPendingQuarantine(0) finds the v1 row and
	// returns QuarantineBlockError before any migration or checkpoint runs.
	withGooseHistoryDropped(t, ctx, doltRoot)

	openErr := func() error {
		st, err := Open(ctx, doltRoot, "test-workspace-id")
		if st != nil {
			_ = st.Close()
		}
		return err
	}()
	if openErr == nil {
		t.Fatal("Open() returned nil error; expected QuarantineBlockError")
	}

	var qErr *QuarantineBlockError
	if !errors.As(openErr, &qErr) {
		t.Fatalf("Open() error = %v (%T); expected *QuarantineBlockError in chain", openErr, openErr)
	}
	if qErr.Version != 1 {
		t.Errorf("QuarantineBlockError.Version = %d, want 1", qErr.Version)
	}
}

// contains is a strings.Contains helper used in test assertions.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || containsAt(s, substr))
}

func containsAt(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
