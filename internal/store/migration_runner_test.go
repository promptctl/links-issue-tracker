package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestPerMigrationCommitsInVersionOrder verifies that 3 pending migrations
// produce 3 distinct Dolt commits attributed to migrationCommitAuthor, emitted
// in ascending version order.
func TestPerMigrationCommitsInVersionOrder(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Inject a third migration (v99998) that inserts a meta row so DOLT_COMMIT
	// sees a real change and produces an actual commit (not "nothing to commit").
	// RunTx is required: goose holds the sole connection (MaxOpenConns=1) via
	// sql.Conn for the lifetime of ApplyVersion; RunTx reuses that conn's
	// transaction rather than acquiring a second connection and deadlocking.
	m3 := goose.NewGoMigration(99998, &goose.GoFunc{
		RunTx: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)",
				"test_migration_v99998", "1")
			return err
		},
	}, nil)
	extraMigrationProviderOptions = func() []goose.ProviderOption {
		return []goose.ProviderOption{goose.WithGoMigrations(m3)}
	}
	t.Cleanup(func() { extraMigrationProviderOptions = nil })

	var buf bytes.Buffer
	origWriter := migrationEventWriter
	migrationEventWriter = &buf
	t.Cleanup(func() { migrationEventWriter = origWriter })

	st, err := Open(ctx, doltRoot, "per-migration-order-ws-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	committed := parseMigrationCommittedEvents(t, buf.String())
	if len(committed) != 3 {
		t.Fatalf("expected 3 migrate.committed events, got %d: %v\nraw:\n%s", len(committed), committed, buf.String())
	}
	if committed[0] != 1 || committed[1] != 2 || committed[2] != 99998 {
		t.Fatalf("committed versions not in ascending order: %v", committed)
	}

	// Verify dolt_log shows 3 commits carrying the "migrate: v" subject prefix.
	// dolt_log.committer stores the name portion only (not "name <email>"), so
	// filtering by message prefix is the reliable cross-version approach.
	var commitCount int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log WHERE message LIKE 'migrate: v%'").Scan(&commitCount); err != nil {
		t.Fatalf("dolt_log count query error = %v", err)
	}
	if commitCount != 3 {
		t.Fatalf("expected 3 migration commits in dolt_log, got %d", commitCount)
	}
}

// TestMiddleMigrationFailureNoCommitForFailed verifies that when a migration in
// the middle of the sequence fails: (a) all successfully-applied migrations
// before it emitted a migrate.committed event, (b) the failing migration did not
// emit a committed event, and (c) subsequent versions are never attempted.
func TestMiddleMigrationFailureNoCommitForFailed(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// v99997: succeeds and mutates state so its commit is real.
	// v99998: fails — middle of the injected sequence.
	// v99999: would succeed but must not be attempted after v99998 fails.
	// RunTx is required for DML migrations: goose holds the sole connection via
	// sql.Conn; RunTx reuses that conn's transaction rather than deadlocking.
	const failVersion int64 = 99998
	mOk := goose.NewGoMigration(99997, &goose.GoFunc{
		RunTx: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)",
				"test_migration_v99997", "1")
			return err
		},
	}, nil)
	mFail := goose.NewGoMigration(failVersion, &goose.GoFunc{
		RunTx: func(_ context.Context, _ *sql.Tx) error {
			return errors.New("intentional middle-migration failure")
		},
	}, nil)
	mAfter := goose.NewGoMigration(99999, &goose.GoFunc{
		RunTx: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)",
				"test_migration_v99999", "1")
			return err
		},
	}, nil)
	extraMigrationProviderOptions = func() []goose.ProviderOption {
		return []goose.ProviderOption{goose.WithGoMigrations(mOk, mFail, mAfter)}
	}
	t.Cleanup(func() { extraMigrationProviderOptions = nil })

	var buf bytes.Buffer
	origWriter := migrationEventWriter
	migrationEventWriter = &buf
	t.Cleanup(func() { migrationEventWriter = origWriter })

	_, err := Open(ctx, doltRoot, "middle-fail-ws-id")
	if err == nil {
		t.Fatal("Open() succeeded, want MigrationError")
	}
	var me *MigrationError
	if !errors.As(err, &me) || me.Version != failVersion {
		t.Fatalf("Open() error = %v, want MigrationError for v%d", err, failVersion)
	}

	output := buf.String()
	committed := parseMigrationCommittedEvents(t, output)

	// The failing version must not appear in committed events.
	for _, v := range committed {
		if v == failVersion {
			t.Fatalf("migrate.committed emitted for failing version %d; expected none", failVersion)
		}
	}
	// v99999 must not appear (not attempted).
	for _, v := range committed {
		if v == 99999 {
			t.Fatalf("migrate.committed emitted for v99999 which should not have been attempted")
		}
	}
	// At least v1 and v2 (real SQL migrations) committed before the failure.
	if len(committed) == 0 {
		t.Fatal("expected at least one migrate.committed event before the failing migration")
	}

	// Second Open: remove the injected migrations entirely. After the rollback,
	// migration_quarantine does not exist yet (it was created by v2 which was
	// rolled back) so the quarantine record for v99998 could not be written.
	// Clearing the injected set lets the workspace recover cleanly by applying
	// only the real SQL migrations.
	buf.Reset()
	extraMigrationProviderOptions = nil
	st, err := Open(ctx, doltRoot, "middle-fail-ws-id")
	if err != nil {
		t.Fatalf("second Open() = %v, want success (injected migrations removed, workspace recoverable)", err)
	}
	defer st.Close()
	requireGooseVersionPresent(t, ctx, st, baselineVersion)
}

// TestMigrationCommitMessageFormat verifies that migration commit messages
// follow the machine-parseable structured format:
//
//	migrate: v<N> <name>
//
//	duration_ms=<n>
//	source=<path>
func TestMigrationCommitMessageFormat(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "commit-format-ws-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Grab the earliest migration commit message (v1 baseline). Filter by message
	// prefix because dolt_log.committer stores the name portion only, not the
	// full "name <email>" string used in DOLT_COMMIT --author.
	var message string
	if err := st.db.QueryRowContext(ctx,
		"SELECT message FROM dolt_log WHERE message LIKE 'migrate: v%' ORDER BY date ASC LIMIT 1").Scan(&message); err != nil {
		t.Fatalf("dolt_log message query error = %v", err)
	}

	lines := strings.SplitN(message, "\n", -1)
	if len(lines) < 4 {
		t.Fatalf("commit message has too few lines (%d):\n%s", len(lines), message)
	}
	// Subject: "migrate: v1 baseline"
	if !strings.HasPrefix(lines[0], "migrate: v1 ") {
		t.Errorf("subject line %q does not start with 'migrate: v1 '", lines[0])
	}
	// Blank separator line.
	if lines[1] != "" {
		t.Errorf("expected blank line after subject, got %q", lines[1])
	}
	// Body key=value fields.
	body := strings.Join(lines[2:], "\n")
	for _, field := range []string{"duration_ms=", "source="} {
		if !strings.Contains(body, field) {
			t.Errorf("commit message body missing %q field\nbody:\n%s", field, body)
		}
	}
	// duration_ms must be a non-negative integer.
	if idx := strings.Index(body, "duration_ms="); idx >= 0 {
		raw := strings.Fields(body[idx:])[0]
		val := strings.TrimPrefix(raw, "duration_ms=")
		if n, err := strconv.ParseInt(val, 10, 64); err != nil || n < 0 {
			t.Errorf("duration_ms=%q is not a non-negative integer", val)
		}
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

// parseMigrationCommittedEvents scans migrationEventWriter output for
// "migrate.committed" lines and returns the version numbers in the order they
// appear.  Used by per-migration-commit tests to assert order and completeness
// without depending on dolt_log state after a rollback.
func parseMigrationCommittedEvents(t *testing.T, output string) []int64 {
	t.Helper()
	var versions []int64
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "migrate.committed") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "version=") {
				v, err := strconv.ParseInt(strings.TrimPrefix(field, "version="), 10, 64)
				if err == nil {
					versions = append(versions, v)
				}
			}
		}
	}
	return versions
}
