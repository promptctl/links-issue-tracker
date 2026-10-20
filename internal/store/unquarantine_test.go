package store

import (
	"context"
	"errors"
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

	done, err := Unquarantine(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Unquarantine() error = %v", err)
	}
	if done.Cleared.Version != version || done.Next != nil {
		t.Fatalf("Unquarantine() = cleared v%d, next %v; want cleared v%d, next nil", done.Cleared.Version, done.Next, version)
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

	done, err := Unquarantine(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Unquarantine() error = %v", err)
	}
	if done.Cleared.Version != version || done.Next == nil || done.Next.Version != version+1 {
		t.Fatalf("Unquarantine() = cleared v%d, next %v; want cleared v%d, next v%d", done.Cleared.Version, done.Next, version, version+1)
	}
	_, openErr := Open(ctx, doltRoot, "test-workspace-id")
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

	_, err := Unquarantine(ctx, doltRoot, "test-workspace-id")
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

	_, err := Unquarantine(ctx, doltRoot, "test-workspace-id")
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

	_, err := Unquarantine(ctx, doltRoot, "test-workspace-id")
	var none NoQuarantineError
	if !errors.As(err, &none) {
		t.Fatalf("Unquarantine() error = %v (%T); want NoQuarantineError", err, err)
	}
}
