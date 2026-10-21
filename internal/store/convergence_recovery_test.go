package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Each test here fails one recovery step through Open, then opens again with
// every injection cleared to show what the step left: what the step's act
// says about the state is what a second open finds. Serial: the injections
// are package-level.

// failingMigration makes every goose step fail as a migration one past the
// registry, which earns a quarantine, and returns its version.
func failingMigration(t *testing.T, sentinel error) int64 {
	t.Helper()
	version := headVersion(t) + 1
	migrationUpByOneForTest = func(context.Context, *sql.DB, *goose.Provider) (*goose.MigrationResult, error) {
		return &goose.MigrationResult{
			Source: &goose.Source{Version: version, Path: fmt.Sprintf("%05d_test.sql", version)},
		}, sentinel
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })
	return version
}

// assertRecovery checks what the event of the failure err records for its
// reset outcome: the checkpoint it reset to or tried to, and why the recovery
// step failed, want, or null when want is "".
func assertRecovery(t *testing.T, err error, want string) {
	t.Helper()
	var held *CheckpointResetError
	var failed *CheckpointResetFailedError
	var checkpoint string
	switch {
	case errors.As(err, &held):
		checkpoint = held.Checkpoint.Name
	case errors.As(err, &failed):
		checkpoint = failed.Checkpoint.Name
	default:
		t.Fatalf("error = %v; want a reset outcome", err)
	}
	facts, _ := model.ErrorRecord(err)
	if checkpoint == "" || facts["migration.checkpoint"] != checkpoint {
		t.Errorf("migration.checkpoint = %v; want the outcome's checkpoint %q", facts["migration.checkpoint"], checkpoint)
	}
	got, recordedKey := facts["migration.recovery_error"]
	text, _ := got.(*string)
	switch {
	case !recordedKey:
		t.Error("migration.recovery_error is not recorded")
	case want == "" && text != nil:
		t.Errorf("migration.recovery_error = %q; want null, the recovery held", *text)
	case want != "" && (text == nil || !strings.Contains(*text, want)):
		t.Errorf("migration.recovery_error = %v; want it to say %q", got, want)
	}
}

// eventFact is what the event of the failure err records under key, through
// the walk the CLI's invocation event takes over the whole error tree.
func eventFact(err error, key string) any {
	facts, _ := model.ErrorRecord(err)
	return facts[key]
}

// openErr is the error an Open returns, closing the store when it opened.
func openErr(t *testing.T, doltRoot string) error {
	t.Helper()
	st, err := Open(context.Background(), testEnv(), doltRoot, "test-workspace-id")
	if err == nil {
		if closeErr := st.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}
	return err
}

// A failed reset after a failed goose step leaves the step's partial changes in
// the working set, and every later open runs the step again on top of them:
// the failure calls for the snapshot it names, and restoring that snapshot is
// what clears them.
func TestAFailedResetAfterAFailedMigrationLeavesWhatOnlyTheSnapshotClears(t *testing.T) {
	ctx := context.Background()
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	sentinel := errors.New("simulated migration failure")
	resetErr := errors.New("simulated reset failure")
	version := headVersion(t) + 1
	// The step creates a table, then fails, as a real step whose second
	// statement fails leaves its first applied. found records, for each run,
	// whether that table was already there when the step started.
	var found []bool
	migrationUpByOneForTest = func(ctx context.Context, db *sql.DB, _ *goose.Provider) (*goose.MigrationResult, error) {
		var tables int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'partial_step'`).Scan(&tables); err != nil {
			return nil, err
		}
		found = append(found, tables > 0)
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS partial_step (id INT PRIMARY KEY)`); err != nil {
			return nil, err
		}
		return &goose.MigrationResult{
			Source: &goose.Source{Version: version, Path: fmt.Sprintf("%05d_test.sql", version)},
		}, sentinel
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })
	clear := injectFailpoint(t, resetErr, stepMigrationReset)

	fail := asConvergenceError(t, openErr(t, doltRoot))
	if fail.Step != stepMigrationReset || fail.Step.Act() != ActRestoreSnapshot {
		t.Fatalf("failure step = %s (act %d); want %s, which restores the snapshot", fail.Step, fail.Step.Act(), stepMigrationReset)
	}
	if fail.Snapshot == nil {
		t.Fatal("a failure that calls for the snapshot carries none")
	}
	var resetFailed *CheckpointResetFailedError
	if !errors.As(fail, &resetFailed) || !errors.Is(resetFailed.ResetErr, resetErr) {
		t.Fatalf("failure = %v; want a *CheckpointResetFailedError whose ResetErr is the reset's failure", fail)
	}
	var step *MigrationStepError
	if !errors.As(resetFailed.Cause, &step) || step.Version != version || !errors.Is(step, sentinel) {
		t.Errorf("CheckpointResetFailedError.Cause = %v; want v%d's *MigrationStepError carrying the migration failure", resetFailed.Cause, version)
	}
	assertRecovery(t, fail, "simulated reset failure")
	if got := eventFact(fail, "migration.failed_version"); got != version {
		t.Errorf("migration.failed_version = %v, want %d", got, version)
	}

	// The retry runs the step on top of what the failed run left, and its reset
	// fails too, so the partial change is still there for the restore to clear.
	retry := asConvergenceError(t, openErr(t, doltRoot))
	clear()
	if retry.Step != stepMigrationReset {
		t.Fatalf("retry failure step = %s (%v); want %s again", retry.Step, retry, stepMigrationReset)
	}
	if len(found) != 2 || found[0] || !found[1] {
		t.Fatalf("partial_step present at each run's start = %v; want [false true]: absent before the first run, left for the retry", found)
	}

	// Restoring the snapshot the failure named gives back the workspace as it
	// was before the run, and the next open migrates cleanly.
	migrationUpByOneForTest = nil
	if _, err := dbsnapshot.Restore(doltRoot, fail.Snapshot.Name); err != nil {
		t.Fatalf("Restore(%s) error = %v", fail.Snapshot.Name, err)
	}
	withStore(t, ctx, doltRoot, func(st *Store) {
		cols, err := st.tableColumns(ctx, "partial_step")
		if err != nil {
			t.Fatalf("tableColumns(partial_step) error = %v", err)
		}
		if len(cols) != 0 {
			t.Errorf("after the restore partial_step still exists with columns %v; want it gone", cols)
		}
	})
}

// A failed reset after a failed migration commit leaves a complete step goose
// already recorded, which the next open reads as applied and commits.
func TestAFailedResetAfterAFailedCommitLeavesAStepTheNextOpenCompletes(t *testing.T) {
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	clear := injectFailpoint(t, errors.New("simulated failure"), stepCommitMigration, stepCommitMigrationReset)

	fail := asConvergenceError(t, openErr(t, doltRoot))
	clear()
	if fail.Step != stepCommitMigrationReset || fail.Step.Act() != ActClearCause {
		t.Fatalf("failure step = %s (act %d); want %s, which a retry clears", fail.Step, fail.Step.Act(), stepCommitMigrationReset)
	}
	var resetFailed *CheckpointResetFailedError
	if !errors.As(fail, &resetFailed) || resetFailed.ResetErr == nil {
		t.Fatalf("failure = %v; want a *CheckpointResetFailedError carrying why the reset failed", fail)
	}
	assertRecovery(t, fail, "simulated failure")

	withStore(t, context.Background(), doltRoot, func(st *Store) {
		applied, err := st.recordedMigrationVersion(context.Background())
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		if applied != headVersion(t) {
			t.Errorf("second open is at v%d; want the registry's v%d", applied, headVersion(t))
		}
	})
}

// Recording a quarantine that fails leaves none in force: the reset held, and
// the next open runs the migration again.
func TestAFailedQuarantineInsertLeavesNoQuarantine(t *testing.T) {
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	failingMigration(t, errors.New("simulated migration failure"))
	insertErr := errors.New("simulated insert failure")
	clear := injectFailpoint(t, insertErr, stepRecordQuarantine)

	err := openErr(t, doltRoot)
	clear()
	migrationUpByOneForTest = nil
	fail := asConvergenceError(t, err)
	var reset *CheckpointResetError
	if fail.Step != stepRecordQuarantine || !errors.As(fail, &reset) || reset.Quarantine != nil || !errors.Is(reset.QuarantineErr, insertErr) {
		t.Fatalf("failure = step %s, reset %+v; want %s with the reset held, the insert's failure, and no quarantine in force", fail.Step, reset, stepRecordQuarantine)
	}
	var quarantined *QuarantineBlockError
	if errors.As(fail, &quarantined) {
		t.Errorf("failure carries a quarantine (v%d); none was recorded", quarantined.Version)
	}
	if !errors.Is(fail, insertErr) {
		t.Errorf("failure = %v; want the insert's failure in its tree, for the classifier", fail)
	}
	assertRecovery(t, fail, "simulated insert failure")
	if err := openErr(t, doltRoot); err != nil {
		t.Errorf("second Open() error = %v; want it to run the migrations, unblocked", err)
	}
}

// A quarantine whose commit fails is in force all the same: the row stays in
// the working set, so the failure carries it and the next open refuses on it.
func TestAFailedQuarantineCommitLeavesTheQuarantineInForce(t *testing.T) {
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	version := failingMigration(t, errors.New("simulated migration failure"))
	commitErr := errors.New("simulated commit failure")
	clear := injectFailpoint(t, commitErr, stepCommitQuarantine)

	err := openErr(t, doltRoot)
	clear()
	migrationUpByOneForTest = nil
	fail := asConvergenceError(t, err)
	var quarantined *QuarantineBlockError
	if fail.Step != stepCommitQuarantine || !errors.As(fail, &quarantined) || quarantined.Version != version {
		t.Fatalf("failure = step %s, %v; want %s carrying the quarantine of v%d", fail.Step, fail, stepCommitQuarantine, version)
	}
	var reset *CheckpointResetError
	if !errors.As(fail, &reset) || reset.Quarantine == nil || !errors.Is(reset.QuarantineErr, commitErr) {
		t.Fatalf("failure = %v; want a *CheckpointResetError with the quarantine in force and the commit's failure", fail)
	}
	assertRecovery(t, fail, "simulated commit failure")
	if got := eventFact(fail, "migration.quarantine_version"); got != version {
		t.Errorf("migration.quarantine_version = %v, want %d", got, version)
	}
	refusal := openErr(t, doltRoot)
	if !errors.As(refusal, &quarantined) || quarantined.Version != version {
		t.Errorf("second Open() did not refuse on the quarantine of v%d", version)
	}
	if got := eventFact(refusal, "migration.quarantine_version"); got != version {
		t.Errorf("the refusal's migration.quarantine_version = %v, want %d", got, version)
	}
}

// driftedWorkspace is a migrated workspace whose live schema lacks content
// versions the goose log claims: the drift every write open repairs.
func driftedWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `ALTER TABLE issues DROP CONSTRAINT issues_redirect_target_check`)
		mustExec(t, ctx, st, `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check`)
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN lane`)
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN resolution`)
		mustCommit(t, ctx, st, "test: drift the live schema from the goose log")
	})
	return doltRoot
}

// A failed drift repair, whether or not its reset held, leaves what the next
// open's probe repairs.
func TestAFailedDriftRepairIsRepairedByTheNextOpen(t *testing.T) {
	for name, tc := range map[string]struct {
		resetFails    bool
		want          ConvergenceStep
		recoveryError string
	}{
		"the reset held":   {false, stepDriftRepair, ""},
		"the reset failed": {true, stepDriftRepairReset, "simulated reset failure"},
	} {
		t.Run(name, func(t *testing.T) {
			doltRoot := driftedWorkspace(t)
			versionContentDriftRepairForTest = func(context.Context) ([]string, error) {
				return nil, errors.New("simulated repair failure")
			}
			t.Cleanup(func() { versionContentDriftRepairForTest = nil })
			if tc.resetFails {
				injectFailpoint(t, errors.New("simulated reset failure"), stepDriftRepairReset)
			}

			fail := asConvergenceError(t, openErr(t, doltRoot))
			failpointForTest, versionContentDriftRepairForTest = nil, nil
			if fail.Step != tc.want || fail.Step.Act() != ActClearCause {
				t.Fatalf("failure step = %s (act %d); want %s, which a retry clears", fail.Step, fail.Step.Act(), tc.want)
			}
			var held *CheckpointResetError
			var resetFailed *CheckpointResetFailedError
			if tc.resetFails != errors.As(fail, &resetFailed) || !tc.resetFails != errors.As(fail, &held) {
				t.Fatalf("failure = %v (reset held %t, reset failed %t); want the reset outcome for resetFails=%t", fail, held != nil, resetFailed != nil, tc.resetFails)
			}
			assertRecovery(t, fail, tc.recoveryError)
			withStore(t, context.Background(), doltRoot, func(st *Store) {
				cols, err := st.tableColumns(context.Background(), "issues")
				if err != nil {
					t.Fatalf("tableColumns(issues) error = %v", err)
				}
				if !cols["lane"] || !cols["resolution"] {
					t.Errorf("second open left the drift: issues columns %v", cols)
				}
			})
		})
	}
}

// A prune that fails after the work it trails committed does not fail the
// open: it is reported on the open's outcome.
func TestAPruneAfterCommittedWorkIsReportedNotFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		workspace func(*testing.T) string
		at        pruneStep
		label     string
	}{
		"migration checkpoints": {migratedWorkspaceWithGooseHistoryDropped, pruneMigrationCheckpoints, "prune migration checkpoints: "},
		"drift checkpoints":     {driftedWorkspace, pruneDriftCheckpoints, "prune version-content drift repair checkpoints: "},
	} {
		t.Run(name, func(t *testing.T) {
			doltRoot := tc.workspace(t)
			stuck := errors.New("simulated prune failure")
			injectFailpoint(t, stuck, tc.at)

			st, err := Open(context.Background(), testEnv(), doltRoot, "test-workspace-id")
			if err != nil {
				t.Fatalf("Open() error = %v; want the open to succeed past its prune", err)
			}
			defer func() {
				if err := st.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			pruneErr := st.Migrated().PruneErr
			if !errors.Is(pruneErr, stuck) || !strings.Contains(pruneErr.Error(), tc.label) {
				t.Errorf("Migrated().PruneErr = %v; want the %q failure", pruneErr, tc.label)
			}
		})
	}
}

// A failed drop of the legacy schema_version key leaves no goose state, so a
// retry adopts the workspace again and drops the key: the step's act, a
// retry, is what clears it.
func TestAFailedLegacyKeyDropIsClearedByARetry(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	withStore(t, ctx, doltRoot, func(st *Store) {
		if err := st.ExecRawForTest(ctx, `INSERT INTO meta (meta_key, meta_value) VALUES ('schema_version', '1')`); err != nil {
			t.Fatalf("seed legacy key: %v", err)
		}
		if err := st.commitWorkingSet(ctx, "seed legacy key"); err != nil {
			t.Fatalf("commit legacy key: %v", err)
		}
	})
	withGooseHistoryDropped(t, ctx, doltRoot)
	clear := injectFailpoint(t, errors.New("simulated drop failure"), stepAdoptDropLegacyKey)

	fail := asConvergenceError(t, openErr(t, doltRoot))
	if fail.Step != stepAdoptDropLegacyKey || fail.Step.Act() != ActClearCause {
		t.Fatalf("failure = step %s (act %d); want %s, cleared by a retry", fail.Step, fail.Step.Act(), stepAdoptDropLegacyKey)
	}
	clear()
	withStore(t, ctx, doltRoot, func(st *Store) {
		var keys int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'schema_version'`).Scan(&keys); err != nil {
			t.Fatalf("count legacy keys: %v", err)
		}
		if keys != 0 {
			t.Errorf("legacy schema_version keys after the retry = %d, want 0", keys)
		}
	})
}

// A failed adoption stamp leaves an empty goose table the next open would
// misread: the failure calls for the snapshot, and carries it.
func TestAFailedAdoptionStampCallsForTheSnapshot(t *testing.T) {
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	injectFailpoint(t, errors.New("simulated stamp failure"), stepAdoptStamp)

	fail := asConvergenceError(t, openErr(t, doltRoot))
	if fail.Step != stepAdoptStamp || fail.Step.Act() != ActRestoreSnapshot || fail.Snapshot == nil {
		t.Fatalf("failure = step %s (act %d), snapshot %v; want %s with the snapshot", fail.Step, fail.Step.Act(), fail.Snapshot, stepAdoptStamp)
	}
}
