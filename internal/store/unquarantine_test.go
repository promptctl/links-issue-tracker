package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// quarantinedWorkspace builds a workspace whose top `pending` migrations are
// reversed and each quarantined, the state failed opens leave behind, and
// returns its Dolt root and the lowest quarantined version.
func quarantinedWorkspace(t *testing.T, ctx context.Context, pending int64) (string, int64) {
	t.Helper()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	first := headVersion(t) - pending + 1
	withStore(t, ctx, doltRoot, func(st *Store) {
		if _, err := st.Downgrade(ctx, first-1); err != nil {
			t.Fatalf("Downgrade(v%d) error = %v", first-1, err)
		}
		for v := first; v <= headVersion(t); v++ {
			if err := st.recordQuarantine(ctx, &QuarantineBlockError{Version: v, Name: "failed.sql", ErrorText: "simulated failure"}); err != nil {
				t.Fatalf("recordQuarantine(v%d) error = %v", v, err)
			}
		}
		if err := st.commitWorkingSet(ctx, "quarantine for test"); err != nil {
			t.Fatalf("commitWorkingSet error = %v", err)
		}
	})
	return doltRoot, first
}

// A quarantined workspace is unquarantined by clearing the quarantine its
// opens are refused on; the next open then runs that migration and succeeds.
func TestUnquarantineClearsTheBlockingQuarantineAndTheNextOpenRunsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, version := quarantinedWorkspace(t, ctx, 1)

	done, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Unquarantine() error = %v", err)
	}
	if done.Cleared.Version != version || done.Cleared.Next != nil {
		t.Fatalf("Unquarantine() = cleared v%d, next %v; want cleared v%d, next nil", done.Cleared.Version, done.Cleared.Next, version)
	}
	// Dolt history names the clear as its own commit, not folded into
	// whatever commits next.
	st, err := openInitialized(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id", engineRead)
	if err != nil {
		t.Fatalf("openInitialized() error = %v", err)
	}
	var head string
	if err := st.db.QueryRowContext(ctx, `SELECT message FROM dolt_log() LIMIT 1`).Scan(&head); err != nil {
		t.Fatalf("read dolt_log error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if want := fmt.Sprintf("unquarantine v%d failed.sql", version); head != want {
		t.Errorf("Dolt HEAD message = %q, want %q", head, want)
	}
	withStore(t, ctx, doltRoot, func(st *Store) {
		applied, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		if applied != version {
			t.Errorf("open after Unquarantine left the workspace at v%d; want the cleared v%d run", applied, version)
		}
	})
}

// With two quarantines in force, one Unquarantine clears the lower and names
// the higher, which the next open is still refused on.
func TestUnquarantineNamesTheQuarantineTheNextOpenStillMeets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, version := quarantinedWorkspace(t, ctx, 2)

	done, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Unquarantine() error = %v", err)
	}
	if done.Cleared.Version != version || done.Cleared.Next == nil || done.Cleared.Next.Version != version+1 {
		t.Fatalf("Unquarantine() = cleared v%d, next %v; want cleared v%d, next v%d", done.Cleared.Version, done.Cleared.Next, version, version+1)
	}
	_, openErr := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var blocked *QuarantineBlockError
	if !errors.As(openErr, &blocked) || blocked.Version != version+1 {
		t.Fatalf("Open() after Unquarantine = %v; want the quarantine of v%d", openErr, version+1)
	}
}

// A workspace no quarantine blocks answers that the state already holds, and
// nothing is committed.
func TestUnquarantineWithNothingQuarantinedWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	var before string
	withStore(t, ctx, doltRoot, func(st *Store) {
		var err error
		if before, err = readDoltHead(ctx, st.db); err != nil {
			t.Fatalf("readDoltHead() error = %v", err)
		}
	})

	_, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var none NoQuarantineError
	if !errors.As(err, &none) || !none.Satisfied() {
		t.Fatalf("Unquarantine() error = %v (%T); want NoQuarantineError", err, err)
	}
	withStore(t, ctx, doltRoot, func(st *Store) {
		after, err := readDoltHead(ctx, st.db)
		if err != nil {
			t.Fatalf("readDoltHead() error = %v", err)
		}
		if after != before {
			t.Errorf("Dolt HEAD moved from %s to %s; Unquarantine with nothing quarantined must commit nothing", before, after)
		}
	})
}

// A pending workspace whose quarantine table is missing is ensured through the
// gate before it is read, so it answers that nothing is quarantined rather
// than failing on the missing table.
func TestUnquarantineEnsuresTheQuarantineTableBeforeReadingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	withStore(t, ctx, doltRoot, func(st *Store) {
		if _, err := st.Downgrade(ctx, headVersion(t)-1); err != nil {
			t.Fatalf("Downgrade error = %v", err)
		}
		if err := st.ExecRawForTest(ctx, `DROP TABLE migration_quarantine`); err != nil {
			t.Fatalf("drop migration_quarantine error = %v", err)
		}
		if err := st.commitWorkingSet(ctx, "drop quarantine table for test"); err != nil {
			t.Fatalf("commitWorkingSet error = %v", err)
		}
	})

	_, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var none NoQuarantineError
	if !errors.As(err, &none) {
		t.Fatalf("Unquarantine() error = %v (%T); want NoQuarantineError", err, err)
	}
}

// A quarantine row above every migration this binary has blocks no open of an
// up-to-date workspace, so it is not the quarantine Unquarantine clears.
func TestUnquarantineLeavesARowThatBlocksNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	withStore(t, ctx, doltRoot, func(st *Store) {
		if err := st.recordQuarantine(ctx, &QuarantineBlockError{Version: headVersion(t) + 1, Name: "newer.sql", ErrorText: "simulated failure"}); err != nil {
			t.Fatalf("recordQuarantine error = %v", err)
		}
		if err := st.commitWorkingSet(ctx, "quarantine for test"); err != nil {
			t.Fatalf("commitWorkingSet error = %v", err)
		}
	})

	_, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var none NoQuarantineError
	if !errors.As(err, &none) {
		t.Fatalf("Unquarantine() error = %v (%T); want NoQuarantineError", err, err)
	}
}

// recordQuarantineBeyondRegistry leaves the row a newer lit's failed migration
// leaves, a quarantine one past this binary's registry, and returns its version.
// It writes below the migration gate, since an open would migrate the store.
func recordQuarantineBeyondRegistry(t *testing.T, ctx context.Context, doltRoot string) int64 {
	t.Helper()
	s, err := openInitialized(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id", engineWrite)
	if err != nil {
		t.Fatalf("openInitialized error = %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()
	beyond := headVersion(t) + 1
	if err := s.recordQuarantine(ctx, &QuarantineBlockError{Version: beyond, Name: "newer.sql", ErrorText: "simulated failure"}); err != nil {
		t.Fatalf("recordQuarantine error = %v", err)
	}
	if err := s.commitWorkingSet(ctx, "quarantine for test"); err != nil {
		t.Fatalf("commitWorkingSet error = %v", err)
	}
	return beyond
}

// assertRefusesLitWithMigration checks that the quarantine of beyond is still in
// force for a lit whose registry reaches it.
func assertRefusesLitWithMigration(t *testing.T, ctx context.Context, doltRoot string, applied, beyond int64) {
	t.Helper()
	observeDolt(t, ctx, doltRoot, "test-workspace-id", func(s *Store) {
		var blocked *QuarantineBlockError
		if err := s.checkPendingQuarantine(ctx, applied, beyond); !errors.As(err, &blocked) || blocked.Version != beyond {
			t.Errorf("a lit with v%d pending got %v; want the quarantine of v%d in force", beyond, err, beyond)
		}
	})
}

// workspaceOneBehindHead is a migrated workspace with its last migration
// reversed: this binary has a migration to run.
func workspaceOneBehindHead(t *testing.T, ctx context.Context) string {
	t.Helper()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	withStore(t, ctx, doltRoot, func(st *Store) {
		if _, err := st.Downgrade(ctx, headVersion(t)-1); err != nil {
			t.Fatalf("Downgrade error = %v", err)
		}
	})
	return doltRoot
}

// A lit older than a quarantined migration runs the migrations it has: the
// quarantine names one it would never run, and stays to refuse the lit that
// has it.
func TestAnOlderLitMigratesPastAQuarantineItDoesNotHave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := workspaceOneBehindHead(t, ctx)
	beyond := recordQuarantineBeyondRegistry(t, ctx, doltRoot)

	head := headVersion(t)
	withStore(t, ctx, doltRoot, func(st *Store) {
		applied, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			t.Fatalf("recordedMigrationVersion error = %v", err)
		}
		if applied != head {
			t.Errorf("applied = v%d after the open, want v%d", applied, head)
		}
	})
	assertRefusesLitWithMigration(t, ctx, doltRoot, head, beyond)
}

// Clearing is the gate's answer, so a lit that is not refused cannot delete the
// quarantine guarding a newer lit from rerunning its failed migration.
func TestUnquarantineLeavesAQuarantineThisLitDoesNotHaveInForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := workspaceOneBehindHead(t, ctx)
	beyond := recordQuarantineBeyondRegistry(t, ctx, doltRoot)

	_, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var none NoQuarantineError
	if !errors.As(err, &none) {
		t.Fatalf("Unquarantine() error = %v (%T); want NoQuarantineError", err, err)
	}
	assertRefusesLitWithMigration(t, ctx, doltRoot, headVersion(t)-1, beyond)
}

// A quarantine on a migration this lit has refuses it as ever, and a further
// row past its registry is not the next thing it meets: the clear promises the
// run, and the run lands.
func TestAQuarantineBeyondTheRegistryIsNotTheNextOneMet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, version := quarantinedWorkspace(t, ctx, 1)
	beyond := recordQuarantineBeyondRegistry(t, ctx, doltRoot)

	var blocked *QuarantineBlockError
	if err := openErr(t, doltRoot); !errors.As(err, &blocked) || blocked.Version != version || blocked.Next != nil {
		t.Fatalf("Open() error = %v; want the quarantine of v%d and no Next", err, version)
	}
	done, err := Unquarantine(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Unquarantine() error = %v", err)
	}
	if done.Cleared.Version != version || done.Cleared.Next != nil {
		t.Errorf("Unquarantine() cleared %+v; want v%d and no Next", done.Cleared, version)
	}
	if err := openErr(t, doltRoot); err != nil {
		t.Fatalf("Open() after the clear error = %v", err)
	}
	assertRefusesLitWithMigration(t, ctx, doltRoot, version, beyond)
}

// A clear whose commit fails is still in force: the error names it with the
// facts a committed clear records, and the next open runs the migration.
func TestUnquarantineWhoseCommitFailsLeavesTheClearInForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, version := quarantinedWorkspace(t, ctx, 1)

	st, err := openInitialized(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id", engineWrite)
	if err != nil {
		t.Fatalf("openInitialized() error = %v", err)
	}
	injected := errors.New("injected: DOLT_COMMIT failed")
	// Fails only the commit that would version the clear: the gate's own
	// commit, which runs while the row is still there, lands.
	st.commitWorkingSetHookForTest = func() error {
		var rows int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_quarantine WHERE version = ?`, version).Scan(&rows); err != nil {
			return err
		}
		if rows == 0 {
			return injected
		}
		return nil
	}
	_, err = st.unquarantine(ctx)
	if closeErr := st.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	var uncommitted *UnquarantineCommitError
	if !errors.As(err, &uncommitted) || !errors.Is(err, injected) || uncommitted.Cleared.Version != version {
		t.Fatalf("unquarantine() error = %v (%T); want UnquarantineCommitError clearing v%d over the injected failure", err, err, version)
	}
	if facts, _ := uncommitted.EventRecord(); facts["unquarantine.cleared_version"] != version {
		t.Errorf("EventRecord() = %v; want unquarantine.cleared_version %d", facts, version)
	}
	withStore(t, ctx, doltRoot, func(st *Store) {
		applied, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		if applied != version {
			t.Errorf("open after the failed commit left the workspace at v%d; want the cleared v%d run", applied, version)
		}
	})
}
