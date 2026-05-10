package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestFreshWorkspaceStampsBaselineViaGoose verifies that opening an empty
// workspace creates the goose_db_version table and records version 1 (the
// baseline) as applied. Fresh workspaces never go through adoption — goose
// runs 00001_baseline.sql directly.
func TestFreshWorkspaceStampsBaselineViaGoose(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "fresh-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	requireGooseVersionPresent(t, ctx, st, baselineVersion)
	requireMetaSchemaVersionAbsent(t, ctx, st)
}

// TestPreGooseWorkspaceIsAdoptedAndStamped verifies adoption: a workspace with
// application tables and a legacy meta.schema_version row gets the goose
// versioning table created, baseline stamped as applied, and the legacy
// schema_version row removed. Simulates a workspace that existed before the
// goose layer landed by stripping goose_db_version after a fresh Open.
func TestPreGooseWorkspaceIsAdoptedAndStamped(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "pregoose-workspace-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if _, err := first.db.ExecContext(ctx, "DROP TABLE "+gooseVersionTable); err != nil {
		t.Fatalf("drop goose table error = %v", err)
	}
	// Drop every post-baseline migration's table so the workspace mirrors a
	// true pre-goose state (schema only at baseline shape). Without this,
	// adoption stamps version 1 and goose then tries to apply 2+ against a
	// schema that already has those tables. Update this list whenever a new
	// post-baseline migration ships.
	for _, postBaselineTable := range []string{"migration_quarantine"} {
		if _, err := first.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+postBaselineTable); err != nil {
			t.Fatalf("drop post-baseline table %s error = %v", postBaselineTable, err)
		}
	}
	if _, err := first.db.ExecContext(ctx,
		`INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)`,
		"schema_version", "1"); err != nil {
		t.Fatalf("seed legacy meta.schema_version error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() (adoption) error = %v", err)
	}
	defer second.Close()

	requireGooseVersionPresent(t, ctx, second, baselineVersion)
	requireMetaSchemaVersionAbsent(t, ctx, second)
}

// TestSecondOpenIsIdempotent verifies that re-opening a workspace that's
// already on goose makes no additional state changes — no extra rows in
// goose_db_version, no spurious commits.
func TestSecondOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "idempotent-workspace-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	rowsBefore := countGooseVersionRows(t, ctx, first)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer second.Close()
	rowsAfter := countGooseVersionRows(t, ctx, second)
	if rowsAfter != rowsBefore {
		t.Fatalf("goose_db_version row count changed across opens: before=%d after=%d", rowsBefore, rowsAfter)
	}
}

func requireGooseVersionPresent(t *testing.T, ctx context.Context, st *Store, version int) {
	t.Helper()
	var applied int
	err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+gooseVersionTable+" WHERE version_id = ? AND is_applied = TRUE",
		version).Scan(&applied)
	if err != nil {
		t.Fatalf("query goose version %d error = %v", version, err)
	}
	if applied == 0 {
		t.Fatalf("expected goose version %d to be marked applied; not found", version)
	}
}

func requireMetaSchemaVersionAbsent(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var present int
	err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM meta WHERE meta_key = ?`, "schema_version").Scan(&present)
	if err != nil {
		t.Fatalf("query meta.schema_version error = %v", err)
	}
	if present != 0 {
		t.Fatalf("expected legacy meta.schema_version to be absent; found %d row(s)", present)
	}
}

func countGooseVersionRows(t *testing.T, ctx context.Context, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+gooseVersionTable).Scan(&n); err != nil {
		t.Fatalf("count %s error = %v", gooseVersionTable, err)
	}
	return n
}

// TestDryRunSucceedsWithPendingMigrations verifies that LIT_MIGRATE_DRY_RUN=1
// runs all pending migrations, returns ErrDryRun, and leaves the workspace
// untouched (goose_db_version absent means no migration was committed).
func TestDryRunSucceedsWithPendingMigrations(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	t.Setenv("LIT_MIGRATE_DRY_RUN", "1")

	_, err := Open(ctx, doltRoot, "dry-run-pending-ws-id")
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("Open() = %v, want ErrDryRun", err)
	}

	// Workspace is untouched: open without dry-run succeeds, proving the
	// workspace was not left in a partially-migrated state.
	if err := os.Unsetenv("LIT_MIGRATE_DRY_RUN"); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, doltRoot, "dry-run-pending-ws-id")
	if err != nil {
		t.Fatalf("second Open() = %v, want success (workspace untouched after dry-run)", err)
	}
	defer st.Close()
	requireGooseVersionPresent(t, ctx, st, baselineVersion)
}

// TestDryRunSucceedsWithNoPendingMigrations verifies that LIT_MIGRATE_DRY_RUN=1
// on a workspace that is already fully migrated (0 pending) returns ErrDryRun
// and leaves state unchanged.
func TestDryRunSucceedsWithNoPendingMigrations(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Apply all migrations normally.
	first, err := Open(ctx, doltRoot, "dry-run-none-ws-id")
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	rowsBefore := countGooseVersionRows(t, ctx, first)
	first.Close()

	// Dry-run with 0 pending.
	t.Setenv("LIT_MIGRATE_DRY_RUN", "1")
	_, err = Open(ctx, doltRoot, "dry-run-none-ws-id")
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("dry-run Open() = %v, want ErrDryRun", err)
	}

	// Verify state unchanged: re-open normally.
	if err := os.Unsetenv("LIT_MIGRATE_DRY_RUN"); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, doltRoot, "dry-run-none-ws-id")
	if err != nil {
		t.Fatalf("third Open() = %v, want success", err)
	}
	defer second.Close()
	rowsAfter := countGooseVersionRows(t, ctx, second)
	if rowsAfter != rowsBefore {
		t.Fatalf("goose_db_version rows changed after dry-run: before=%d after=%d", rowsBefore, rowsAfter)
	}
}

// TestDryRunFailingMigrationLeavesWorkspaceUntouched verifies that a migration
// that errors during dry-run returns a non-ErrDryRun error and leaves the
// workspace in a state that can be opened fresh afterward.
func TestDryRunFailingMigrationLeavesWorkspaceUntouched(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Inject a failing Go migration via the test seam.
	failing := goose.NewGoMigration(99999, &goose.GoFunc{
		RunDB: func(_ context.Context, _ *sql.DB) error {
			return errors.New("intentional dry-run test failure")
		},
	}, nil)
	extraMigrationProviderOptions = func() []goose.ProviderOption {
		return []goose.ProviderOption{goose.WithGoMigrations(failing)}
	}
	t.Cleanup(func() { extraMigrationProviderOptions = nil })

	t.Setenv("LIT_MIGRATE_DRY_RUN", "1")
	_, err := Open(ctx, doltRoot, "dry-run-fail-ws-id")
	if err == nil || errors.Is(err, ErrDryRun) {
		t.Fatalf("Open() = %v, want a migration failure error", err)
	}

	// Workspace is untouched: remove the failing migration and clear dry-run,
	// then verify a clean open succeeds.
	extraMigrationProviderOptions = nil
	if err := os.Unsetenv("LIT_MIGRATE_DRY_RUN"); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, doltRoot, "dry-run-fail-ws-id")
	if err != nil {
		t.Fatalf("second Open() = %v, want success (workspace untouched)", err)
	}
	defer st.Close()
	requireGooseVersionPresent(t, ctx, st, baselineVersion)
}
