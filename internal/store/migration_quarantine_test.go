package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestQuarantineTableCreatedByFreshOpen pins that a fresh-workspace Open
// creates the migration_quarantine table before any goose migration runs,
// so it is always available for recording failures.
func TestQuarantineTableCreatedByFreshOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Simulate a pre-goose workspace by dropping goose history.
	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
// This verifies the bootstrap-before-checkpoint ordering, which keeps the
// quarantine table from being erased by the very reset it is meant to survive.
func TestQuarantineTableSurvivesCheckpointReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	if err := st.recordQuarantine(ctx, &QuarantineBlockError{Version: 3, Name: "00003_test.sql", ErrorText: "simulated v3 failure"}); err != nil {
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
// a quarantine row, and returns the migration step's SchemaConvergenceError,
// whose cause is the CheckpointResetError. A subsequent Open succeeds (phaseManaged,
// appliedVersion=registryMax, willMutate=false), and the test verifies the
// checkpoint branch and quarantine commit persisted across the reset.
func TestMigrationFailureCheckpointPath(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// First Open: bootstrap the workspace.
	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Force re-migration so applyPendingMigrations runs on next Open.
	withGooseHistoryDropped(t, ctx, doltRoot)

	// Inject a migration failure via the test hook. The synthetic migration sits
	// one past HEAD so it cannot collide with (or quarantine) a real registry
	// migration that the recovery reopen must still be free to apply.
	failVersion := headVersion(t) + 1
	failName := fmt.Sprintf("%05d_test.sql", failVersion)
	sentinel := errors.New("simulated migration body error")
	migrationUpByOneForTest = func(ctx context.Context, _ *sql.DB, provider *goose.Provider) (*goose.MigrationResult, error) {
		return &goose.MigrationResult{
			Source: &goose.Source{Version: failVersion, Path: failName},
		}, sentinel
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	if openErr == nil {
		t.Fatal("Open() returned nil error; expected the migration step's failure")
	}

	// The failure names the migration step and the snapshot the open took.
	rollback := asConvergenceError(t, openErr)
	if rollback.Step != stepMigration || rollback.Snapshot == nil {
		t.Fatalf("failure = step %s, snapshot %v; want step %s with the snapshot", rollback.Step, rollback.Snapshot, stepMigration)
	}

	// Cause must be a CheckpointResetError (from the Dolt-checkpoint layer).
	var cpErr *CheckpointResetError
	if !errors.As(rollback.Cause, &cpErr) {
		t.Fatalf("rollback.Cause = %v (%T); expected *CheckpointResetError", rollback.Cause, rollback.Cause)
	}
	var step *MigrationStepError
	if !errors.As(cpErr.Cause, &step) || step.Version != failVersion {
		t.Errorf("CheckpointResetError.Cause = %v; want the *MigrationStepError of v%d", cpErr.Cause, failVersion)
	}
	if cpErr.Checkpoint.Name == "" {
		t.Error("CheckpointResetError.Checkpoint.Name is empty")
	}
	if !errors.Is(cpErr.Cause, sentinel) {
		t.Errorf("CheckpointResetError.Cause does not wrap sentinel: %v", cpErr.Cause)
	}
	// The run that quarantines already carries the quarantine, so it is
	// classified as the refusal every later open returns.
	var firstQ *QuarantineBlockError
	if !errors.As(openErr, &firstQ) || firstQ.Version != failVersion {
		t.Errorf("failed Open error = %v, want it to carry the quarantine of v%d", openErr, failVersion)
	}

	// Error message must name the checkpoint and the snapshot. It gives no
	// restore instruction: the reset already restored the pre-migration state.
	msg := rollback.Error()
	if !strings.Contains(msg, cpErr.Checkpoint.Name) {
		t.Errorf("error message missing checkpoint name %q: %s", cpErr.Checkpoint.Name, msg)
	}
	if !strings.Contains(msg, rollback.Snapshot.Name) {
		t.Errorf("error message missing snapshot name %q: %s", rollback.Snapshot.Name, msg)
	}
	if strings.Contains(msg, "lit snapshots restore") {
		t.Errorf("error message advises a restore the reset already did: %s", msg)
	}

	// The quarantine recorded during the failed migration persisted across the
	// checkpoint reset, so reopening is refused: it guards the workspace against
	// auto-retrying the known-bad migration until an operator clears the row.
	_, reopenErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if reopenErr == nil {
		t.Fatal("reopen after quarantined failure returned nil error; expected QuarantineBlockError")
	}
	var qErr *QuarantineBlockError
	if !errors.As(reopenErr, &qErr) {
		t.Fatalf("reopen error = %v (%T); expected *QuarantineBlockError", reopenErr, reopenErr)
	}
	if qErr.Version != failVersion {
		t.Errorf("QuarantineBlockError.Version = %d, want %d", qErr.Version, failVersion)
	}
	if !strings.Contains(qErr.ErrorText, sentinel.Error()) {
		t.Errorf("QuarantineBlockError.ErrorText = %q, does not carry the failure cause %q", qErr.ErrorText, sentinel.Error())
	}

	// The checkpoint branch created during the failed Open must still exist:
	// PruneCheckpoints only fires on a successful migration pass, which the
	// failure aborted. Read below the migration gate — a lit Open is (correctly)
	// blocked — through the reader restore uses.
	var cps []storage.Checkpoint
	observeDolt(t, ctx, doltRoot, "test-workspace-id", func(s *Store) {
		var err error
		if cps, err = s.ListCheckpoints(ctx, migrationCheckpointPrefix); err != nil {
			t.Fatal(err)
		}
	})
	if !slices.ContainsFunc(cps, func(cp storage.Checkpoint) bool { return cp.Name == cpErr.Checkpoint.Name }) {
		t.Errorf("checkpoint %q not among the checkpoints %+v", cpErr.Checkpoint.Name, cps)
	}
}

// TestRealMigrationFailureIsQuarantined pins that a migration goose itself
// fails is quarantined: no test hook stands in for the step. Goose returns a
// failed step's result only inside a *goose.PartialError, so this is the one
// test that sees goose's own failure shape.
func TestRealMigrationFailureIsQuarantined(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	// 00009_purges.sql creates the purges table, so running it again on a
	// workspace that has the table fails.
	const failVersion, failName = int64(9), "00009_purges.sql"
	withStore(t, ctx, doltRoot, func(st *Store) {
		if err := st.ExecRawForTest(ctx, fmt.Sprintf(`DELETE FROM goose_db_version WHERE version_id >= %d`, failVersion)); err != nil {
			t.Fatalf("delete goose rows from v%d error = %v", failVersion, err)
		}
		if err := st.commitWorkingSet(ctx, "forget goose history from the failing migration for test"); err != nil {
			t.Fatalf("commitWorkingSet error = %v", err)
		}
	})

	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var cpErr *CheckpointResetError
	if !errors.As(openErr, &cpErr) {
		t.Fatalf("Open() error = %v; want a *CheckpointResetError", openErr)
	}
	var step *MigrationStepError
	if !errors.As(cpErr.Cause, &step) || step.Version != failVersion || step.Name != failName {
		t.Errorf("CheckpointResetError.Cause = %v; want the *MigrationStepError of v%d %q", cpErr.Cause, failVersion, failName)
	}
	if cpErr.Quarantine == nil || cpErr.Quarantine.Version != failVersion {
		t.Errorf("CheckpointResetError.Quarantine = %v; want the quarantine of v%d in force", cpErr.Quarantine, failVersion)
	}

	_, reopenErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var qErr *QuarantineBlockError
	if !errors.As(reopenErr, &qErr) || qErr.Version != failVersion {
		t.Fatalf("reopen error = %v; want the *QuarantineBlockError of v%d", reopenErr, failVersion)
	}
}

// TestTransientMigrationFailureIsNotQuarantined pins that a step goose fails
// with online-GC contention is reset without a quarantine, so the next open
// runs it again: the contention says nothing against the migration. The hook
// fails the way goose does, a nil result and a *goose.PartialError, so the
// failed version is identified from goose's own failure shape.
func TestTransientMigrationFailureIsNotQuarantined(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook.
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	failVersion := headVersion(t) + 1
	migrationUpByOneForTest = func(context.Context, *sql.DB, *goose.Provider) (*goose.MigrationResult, error) {
		return nil, &goose.PartialError{
			Failed: &goose.MigrationResult{Source: &goose.Source{Version: failVersion, Path: fmt.Sprintf("%05d_test.sql", failVersion)}},
			Err:    errors.New("cannot update manifest: database is read only"),
		}
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(context.Background(), testEnv(), testStderr(), doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	var cpErr *CheckpointResetError
	if !errors.As(openErr, &cpErr) {
		t.Fatalf("Open() error = %v; want a *CheckpointResetError", openErr)
	}
	var step *MigrationStepError
	if !errors.As(cpErr.Cause, &step) || step.Version != failVersion || cpErr.Quarantine != nil {
		t.Errorf("CheckpointResetError = {Cause: %v, Quarantine: %v}, want v%d's *MigrationStepError and no quarantine", cpErr.Cause, cpErr.Quarantine, failVersion)
	}
	reopened, err := Open(context.Background(), testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("reopen after a transient migration failure error = %v; want the migration run again", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened) error = %v", err)
	}
}

// TestMigrationCheckpointCreatedByFreshOpen pins that a fresh-workspace Open
// creates at least one pre-migrate checkpoint branch as part of applying the
// baseline migration.
func TestMigrationCheckpointCreatedByFreshOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Drive more Opens than the retention count by dropping goose history.
	for i := 0; i < migrationCheckpointRetention+3; i++ {
		st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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

// TestMigrationFailureResetSurvivesCallerCancel pins that a migration step the
// caller cancels mid-way is reset to the end and is not quarantined: the step
// cancels the context Open was called with and fails with that cancellation,
// as a real step's statements do, and Open returns the CheckpointResetError of
// a completed reset that carries no quarantine, so the next open runs the
// migration again. lit init's adopt timeout cancels exactly that context
// (init_sync.go).
func TestMigrationFailureResetSurvivesCallerCancel(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook.
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	failVersion := headVersion(t) + 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	migrationUpByOneForTest = func(ctx context.Context, _ *sql.DB, _ *goose.Provider) (*goose.MigrationResult, error) {
		cancel()
		return &goose.MigrationResult{
			Source: &goose.Source{Version: failVersion, Path: fmt.Sprintf("%05d_test.sql", failVersion)},
		}, fmt.Errorf("simulated migration statement: %w", ctx.Err())
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	assertCancelledRunResetWithoutQuarantine(t, openErr, failVersion, doltRoot)
}

// TestMigrationCommitCancelResetsWithoutQuarantine pins that a cancellation
// landing after a step succeeds but before its Dolt commit resets the working
// set too, rather than leaving the step applied and unversioned, and earns no
// quarantine: the migration did nothing wrong.
func TestMigrationCommitCancelResetsWithoutQuarantine(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook.
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	failVersion := headVersion(t) + 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	migrationUpByOneForTest = func(context.Context, *sql.DB, *goose.Provider) (*goose.MigrationResult, error) {
		cancel()
		return &goose.MigrationResult{
			Source: &goose.Source{Version: failVersion, Path: fmt.Sprintf("%05d_test.sql", failVersion)},
		}, nil
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	assertCancelledRunResetWithoutQuarantine(t, openErr, failVersion, doltRoot)
}

// TestVersionContentDriftCommitCancelResets pins that a drift repair whose
// commit fails after its ALTERs applied — here because the caller cancelled
// between them — is reset to its checkpoint, not left applied for the next
// unrelated commit to version under its own message.
func TestVersionContentDriftCommitCancelResets(t *testing.T) {
	// serial: no t.Parallel — installs the package-level
	// versionContentDriftRepairForTest hook.
	bg := context.Background()
	withStore(t, bg, filepath.Join(t.TempDir(), "dolt"), func(st *Store) {
		applied, err := st.recordedMigrationVersion(bg)
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		versionContentDriftRepairForTest = func(ctx context.Context) ([]string, error) {
			if _, err := st.db.ExecContext(ctx, `ALTER TABLE issues ADD COLUMN drift_repair_probe INT`); err != nil {
				return nil, err
			}
			cancel()
			return []string{"issues.drift_repair_probe"}, nil
		}
		t.Cleanup(func() { versionContentDriftRepairForTest = nil })

		mismatch := &VersionContentMismatchError{Version: 2, Name: "00002_add_lane.sql", Missing: []string{"issues.lane"}}
		_, fail := st.repairVersionContentDriftWithRollback(ctx, applied, mismatch)
		versionContentDriftRepairForTest = nil
		var held *CheckpointResetError
		if fail == nil || fail.Step != stepDriftRepair || !errors.Is(fail, context.Canceled) || !errors.As(fail, &held) {
			t.Fatalf("repairVersionContentDriftWithRollback() failure = %v; want step drift_repair reporting the failed commit with a completed reset", fail)
		}
		cols, err := st.tableColumns(bg, "issues")
		if err != nil {
			t.Fatalf("tableColumns(issues) error = %v", err)
		}
		if cols["drift_repair_probe"] {
			t.Error("issues still has the column the uncommitted repair added; the reset did not run")
		}
	})
}

// TestMigrationSuccessPruneSurvivesCallerCancel pins that a migration run whose
// last step has landed finishes its bookkeeping when the caller cancels then:
// the checkpoint prune does not stop on the cancellation, so a migrated
// workspace is not reported as a failed migration.
func TestMigrationSuccessPruneSurvivesCallerCancel(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook.
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	migrationUpByOneForTest = func(context.Context, *sql.DB, *goose.Provider) (*goose.MigrationResult, error) {
		cancel()
		return nil, goose.ErrNoNextVersion
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	if err != nil {
		t.Fatalf("Open() error = %v; the migrated workspace was reported as a failed open", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// migratedWorkspaceWithGooseHistoryDropped bootstraps a workspace, then drops
// its goose history so the next Open runs applyPendingMigrations.
func migratedWorkspaceWithGooseHistoryDropped(t *testing.T) string {
	t.Helper()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	first, err := Open(context.Background(), testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	withGooseHistoryDropped(t, context.Background(), doltRoot)
	return doltRoot
}

// assertCancelledRunResetWithoutQuarantine checks the outcome of an Open whose
// migration run the caller cancelled: a completed reset for failVersion, caused
// by the cancellation, with no recovery step stopped by it, no quarantine on
// the error the CLI classifies, and a reopen that is not refused.
func assertCancelledRunResetWithoutQuarantine(t *testing.T, openErr error, failVersion int64, doltRoot string) {
	t.Helper()
	var cpErr *CheckpointResetError
	if !errors.As(openErr, &cpErr) {
		t.Fatalf("Open() error = %v; want a *CheckpointResetError from a reset that completed", openErr)
	}
	var step *MigrationStepError
	if !errors.As(cpErr.Cause, &step) || step.Version != failVersion || !errors.Is(cpErr.Cause, context.Canceled) {
		t.Errorf("CheckpointResetError.Cause = %v, want v%d's *MigrationStepError caused by context canceled", cpErr.Cause, failVersion)
	}
	if strings.Contains(openErr.Error(), "prune migration checkpoints") {
		t.Errorf("Open() error = %v; the checkpoint prune stopped on the caller's cancellation", openErr)
	}
	var quarantined *QuarantineBlockError
	if errors.As(openErr, &quarantined) {
		t.Errorf("Open() error carries a quarantine of v%d; a cancelled run says nothing against the migration", quarantined.Version)
	}
	reopened, err := Open(context.Background(), testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("reopen after a cancelled migration run error = %v; want the migration run again", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened) error = %v", err)
	}
}

// TestCheckpointResetErrorUnknownVersion pins that CheckpointResetError.Error
// emits a "version unknown" message (without a quarantine-clear SQL instruction)
// when goose fails before any step starts, naming no migration, so Version
// stays 0, and says the next open runs the migration again, since nothing
// quarantined it. Otherwise
// the operator would see misleading output like v0 "" and a DELETE targeting a
// quarantine row that was never inserted.
func TestCheckpointResetErrorUnknownVersion(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withGooseHistoryDropped(t, ctx, doltRoot)

	sentinel := errors.New("simulated nil-result error")
	migrationUpByOneForTest = func(ctx context.Context, _ *sql.DB, provider *goose.Provider) (*goose.MigrationResult, error) {
		return nil, sentinel
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	migrationUpByOneForTest = nil
	if openErr == nil {
		t.Fatal("Open() returned nil error; expected CheckpointResetError")
	}

	rollback := asConvergenceError(t, openErr)
	if rollback.Step != stepMigration {
		t.Errorf("failure step = %s; want %s", rollback.Step, stepMigration)
	}
	var cpErr *CheckpointResetError
	if !errors.As(rollback.Cause, &cpErr) {
		t.Fatalf("rollback.Cause = %v (%T); expected *CheckpointResetError", rollback.Cause, rollback.Cause)
	}
	var step *MigrationStepError
	if !errors.As(cpErr.Cause, &step) || step.Version != 0 {
		t.Errorf("CheckpointResetError.Cause = %v; want a *MigrationStepError of version 0 (nil result)", cpErr.Cause)
	}
	// No version, no quarantine row: nothing here may read as quarantined.
	var q *QuarantineBlockError
	if errors.As(openErr, &q) {
		t.Errorf("failed Open with unknown version carries a quarantine (v%d); none was recorded", q.Version)
	}

	msg := cpErr.Error()
	if !strings.Contains(msg, "version unknown") {
		t.Errorf("error message missing 'version unknown': %s", msg)
	}
	if strings.Contains(msg, "lit unquarantine") {
		t.Errorf("error message must not advise clearing a quarantine when version is unknown: %s", msg)
	}
	if !strings.Contains(msg, "the next open runs it again") {
		t.Errorf("error message does not say the next open runs the migration again: %s", msg)
	}
}

// TestReopenBlockedByQuarantineOnAdoptionPath pins that Open fails at the
// check_quarantine step, carrying no recovery snapshot, when the adoption
// path encounters a quarantine row for a version above the baseline. The
// quarantine fast-fail fires before guard.ensure(), so no recovery snapshot is
// created. This tests criterion 2 end-to-end through Open() itself.
func TestReopenBlockedByQuarantineOnAdoptionPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// First Open: bootstrap the workspace (creates quarantine table).
	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}

	// Seed a quarantine row for a hypothetical v2 (above the baseline).
	// After adoption stamps v1, appliedVersion becomes baselineVersion (1), so
	// checkPendingQuarantine(1) finds this v2 row and returns QuarantineBlockError.
	// A quarantine row for v1 itself must NOT block after adoption because
	// adoption confirmed the baseline schema is present and stamped v1.
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (2, '00002_hypothetical.sql', 'simulated failure', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed quarantine row error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "seed quarantine row for test"); err != nil {
		t.Fatalf("commit quarantine row error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Drop goose history so the next Open classifies as phaseAdopt.
	// adoption stamps v1 → state.appliedVersion = baselineVersion → checkPendingQuarantine(1)
	// finds the v2 row → QuarantineBlockError.
	withGooseHistoryDropped(t, ctx, doltRoot)

	openErr := func() error {
		st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
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
	if qErr.Version != 2 {
		t.Errorf("QuarantineBlockError.Version = %d, want 2", qErr.Version)
	}
	// The quarantine fast-fail fires before guard.ensure(), so no recovery
	// snapshot is created: the failure carries none.
	fail := asConvergenceError(t, openErr)
	if fail.Step != stepCheckQuarantine || fail.Snapshot != nil {
		t.Errorf("failure = step %s, snapshot %v; want step %s with no snapshot: quarantine fast-fail must fire before guard.ensure()", fail.Step, fail.Snapshot, stepCheckQuarantine)
	}
}

// withStaleShapeQuarantineTable replaces a workspace's migration_quarantine
// table with the older version_id/reason/quarantined_at shape a pre-restructure
// lit build produced, optionally seeding one row of "history" under that shape.
// The workspace must already be open with no other pending mutation, since this
// commits directly rather than going through migrate().
func withStaleShapeQuarantineTable(t *testing.T, ctx context.Context, doltRoot string, seedRow bool) {
	t.Helper()
	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("withStaleShapeQuarantineTable Open error = %v", err)
	}
	if err := st.ExecRawForTest(ctx, `DROP TABLE migration_quarantine`); err != nil {
		_ = st.Close()
		t.Fatalf("drop canonical migration_quarantine error = %v", err)
	}
	if err := st.ExecRawForTest(ctx, `CREATE TABLE migration_quarantine (
		version_id     BIGINT NOT NULL,
		reason         TEXT NOT NULL,
		quarantined_at VARCHAR(64) NOT NULL,
		PRIMARY KEY (version_id)
	)`); err != nil {
		_ = st.Close()
		t.Fatalf("create stale-shape migration_quarantine error = %v", err)
	}
	if seedRow {
		if err := st.ExecRawForTest(ctx,
			`INSERT INTO migration_quarantine (version_id, reason, quarantined_at) VALUES (2, 'old-build failure', '2025-01-01T00:00:00Z')`,
		); err != nil {
			_ = st.Close()
			t.Fatalf("seed stale-shape quarantine row error = %v", err)
		}
	}
	if err := st.commitWorkingSet(ctx, "simulate stale-shape migration_quarantine table"); err != nil {
		_ = st.Close()
		t.Fatalf("commitWorkingSet error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("withStaleShapeQuarantineTable Close error = %v", err)
	}
}

// TestQuarantineTableSelfHealsStaleShapeWhenEmpty pins that a
// migration_quarantine table left in an older lit build's shape
// (version_id/reason/quarantined_at) with zero rows is corrected in place by
// an ordinary lit Open, with no manual SQL.
func TestQuarantineTableSelfHealsStaleShapeWhenEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withStaleShapeQuarantineTable(t, ctx, doltRoot, false)

	// Drop goose history so this Open is a mutating adoption pass — the phase
	// that reaches ensureQuarantineTable. A non-mutating managed Open never
	// touches the table at all, so it would prove nothing about self-heal.
	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() over a stale-shape empty migration_quarantine failed: %v — self-heal must succeed via ordinary Open", err)
	}
	defer second.Close()

	cols, err := second.tableColumns(ctx, "migration_quarantine")
	if err != nil {
		t.Fatalf("tableColumns error = %v", err)
	}
	if !quarantineShapeMatches(cols) {
		t.Fatalf("migration_quarantine columns = %v, want exactly %v (self-heal did not correct the shape)", cols, canonicalQuarantineColumns)
	}
}

// TestQuarantineTableRefusesToDiscardStaleShapeHistory pins the boundary of
// self-heal: a stale-shape migration_quarantine table that still holds rows
// is NOT silently dropped and recreated — that would discard quarantine
// history under column names this binary can't translate. Open must refuse
// loudly instead, naming the shape and row count.
func TestQuarantineTableRefusesToDiscardStaleShapeHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withStaleShapeQuarantineTable(t, ctx, doltRoot, true)
	withGooseHistoryDropped(t, ctx, doltRoot)

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if st != nil {
		_ = st.Close()
	}
	if err == nil {
		t.Fatal("Open() over a stale-shape migration_quarantine WITH rows returned nil error; expected a refusal naming the shape/row count")
	}
	msg := err.Error()
	if !strings.Contains(msg, "non-canonical shape") || !strings.Contains(msg, "1 row") {
		t.Errorf("error message = %q; want it to name the non-canonical shape and the row count", msg)
	}
}

// TestFailedMigrationReportsCheckpointPruneFailure pins that a checkpoint
// prune failing on the migration-failure path reaches the caller beside the
// migration failure, while the reset before it succeeded: on the failure's
// PruneErr and in its message, never in its cause, so it never decides how
// the failure is classified.
func TestFailedMigrationReportsCheckpointPruneFailure(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationUpByOneForTest
	// hook and failpointForTest.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withGooseHistoryDropped(t, ctx, doltRoot)

	migrationFailure := errors.New("simulated migration failure")
	pruneFailure := errors.New("simulated prune failure")
	migrationUpByOneForTest = func(context.Context, *sql.DB, *goose.Provider) (*goose.MigrationResult, error) {
		return nil, migrationFailure
	}
	injectFailpoint(t, pruneFailure, pruneMigrationCheckpoints)
	t.Cleanup(func() { migrationUpByOneForTest = nil })

	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if openErr == nil {
		t.Fatal("Open() returned nil error; expected the migration failure")
	}
	var resetErr *CheckpointResetError
	if !errors.As(openErr, &resetErr) {
		t.Errorf("error = %v; expected a CheckpointResetError (the reset succeeded)", openErr)
	}
	if !errors.Is(openErr, migrationFailure) {
		t.Errorf("error = %v; expected it to carry the migration failure", openErr)
	}
	fail := asConvergenceError(t, openErr)
	if !errors.Is(fail.PruneErr, pruneFailure) || !strings.Contains(fail.Error(), pruneFailure.Error()) {
		t.Errorf("failure = %v (PruneErr %v); expected the checkpoint prune failure on PruneErr and in the message", fail, fail.PruneErr)
	}
	if errors.Is(fail.Cause, pruneFailure) {
		t.Errorf("cause = %v; the prune failure must not be in the cause the failure is classified by", fail.Cause)
	}
}
