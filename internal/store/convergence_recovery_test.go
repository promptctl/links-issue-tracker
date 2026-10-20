package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
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
	migrationUpByOneForTest = func(context.Context, *goose.Provider) (*goose.MigrationResult, error) {
		return &goose.MigrationResult{
			Source: &goose.Source{Version: version, Path: fmt.Sprintf("%05d_test.sql", version)},
		}, sentinel
	}
	t.Cleanup(func() { migrationUpByOneForTest = nil })
	return version
}

// openErr is the error an Open returns, closing the store when it opened.
func openErr(t *testing.T, doltRoot string) error {
	t.Helper()
	st, err := Open(context.Background(), doltRoot, "test-workspace-id")
	if err == nil {
		if closeErr := st.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}
	return err
}

// A failed reset after a failed goose step leaves a working set that may hold
// the step's partial changes: the failure says to restore the snapshot it
// names, and it names one.
func TestAFailedResetAfterAFailedMigrationCallsForTheSnapshot(t *testing.T) {
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	sentinel := errors.New("simulated migration failure")
	failingMigration(t, sentinel)
	injectFailpoint(t, errors.New("simulated reset failure"), stepMigrationReset)

	fail := asConvergenceError(t, openErr(t, doltRoot))
	if fail.Step != stepMigrationReset || fail.Step.Act() != ActRestoreSnapshot {
		t.Fatalf("failure step = %s (act %d); want %s, which restores the snapshot", fail.Step, fail.Step.Act(), stepMigrationReset)
	}
	if fail.Snapshot == nil {
		t.Fatal("a failure that calls for the snapshot carries none")
	}
	if !errors.Is(fail, sentinel) {
		t.Errorf("failure = %v; want it to carry the migration failure it was resetting after", fail)
	}
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
	clear := injectFailpoint(t, errors.New("simulated insert failure"), stepRecordQuarantine)

	err := openErr(t, doltRoot)
	clear()
	migrationUpByOneForTest = nil
	fail := asConvergenceError(t, err)
	var reset *CheckpointResetError
	if fail.Step != stepRecordQuarantine || !errors.As(fail, &reset) || reset.Quarantined {
		t.Fatalf("failure = step %s, reset %+v; want %s with the reset held and no quarantine in force", fail.Step, reset, stepRecordQuarantine)
	}
	var quarantined *QuarantineBlockError
	if errors.As(fail, &quarantined) {
		t.Errorf("failure carries a quarantine (v%d); none was recorded", quarantined.Version)
	}
	if err := openErr(t, doltRoot); err != nil {
		t.Errorf("second Open() error = %v; want it to run the migrations, unblocked", err)
	}
}

// A quarantine whose commit fails is in force all the same: the row stays in
// the working set, so the failure carries it and the next open refuses on it.
func TestAFailedQuarantineCommitLeavesTheQuarantineInForce(t *testing.T) {
	doltRoot := migratedWorkspaceWithGooseHistoryDropped(t)
	version := failingMigration(t, errors.New("simulated migration failure"))
	clear := injectFailpoint(t, errors.New("simulated commit failure"), stepCommitQuarantine)

	err := openErr(t, doltRoot)
	clear()
	migrationUpByOneForTest = nil
	fail := asConvergenceError(t, err)
	var quarantined *QuarantineBlockError
	if fail.Step != stepCommitQuarantine || !errors.As(fail, &quarantined) || quarantined.Version != version {
		t.Fatalf("failure = step %s, %v; want %s carrying the quarantine of v%d", fail.Step, fail, stepCommitQuarantine, version)
	}
	if !errors.As(openErr(t, doltRoot), &quarantined) || quarantined.Version != version {
		t.Errorf("second Open() did not refuse on the quarantine of v%d", version)
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
		resetFails bool
		want       ConvergenceStep
	}{
		"the reset held":   {false, stepDriftRepair},
		"the reset failed": {true, stepDriftRepairReset},
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

			st, err := Open(context.Background(), doltRoot, "test-workspace-id")
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
