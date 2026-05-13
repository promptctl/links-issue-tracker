package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	for _, postBaselineTable := range []string{"migration_quarantine", "migration_log"} {
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
// goose_db_version.
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
// TestPerMigrationCommitsInVersionOrder verifies that every applied
// migration in a single Open produces a distinct Dolt commit attributed
// to migrationCommitAuthor, that the commits land in ascending version
// order, and that dolt_log records the same count as the runner's
// event stream emitted.
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

	committed := parseMigrateCommitEvents(t, buf.String())
	// Expect every registered baseline + post-baseline SQL migration + the
	// injected v99998. Asserts ascending order and that the injected version
	// is last. The exact count tracks how many migration files exist so the
	// test stays correct as new migrations land.
	if len(committed) < 2 {
		t.Fatalf("expected at least 2 migrate.commit events (baseline + injected), got %d: %v\nraw:\n%s", len(committed), committed, buf.String())
	}
	for i := 1; i < len(committed); i++ {
		if committed[i] <= committed[i-1] {
			t.Fatalf("committed versions not in ascending order: %v", committed)
		}
	}
	if committed[len(committed)-1] != 99998 {
		t.Fatalf("expected last committed version = 99998 (injected), got %d in %v", committed[len(committed)-1], committed)
	}

	// Verify dolt_log shows one commit per migration carrying the
	// "migrate: v" subject prefix. dolt_log.committer stores the name
	// portion only (not "name <email>"), so filtering by message prefix is
	// the reliable cross-version approach.
	var commitCount int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log WHERE message LIKE 'migrate: v%'").Scan(&commitCount); err != nil {
		t.Fatalf("dolt_log count query error = %v", err)
	}
	if commitCount != len(committed) {
		t.Fatalf("expected %d migration commits in dolt_log, got %d", len(committed), commitCount)
	}
}

// TestMiddleMigrationFailureNoCommitForFailed verifies that when a migration in
// the middle of the sequence fails: (a) all successfully-applied migrations
// before it emitted a migrate.commit event, (b) the failing migration did not
// emit a commit event, and (c) subsequent versions are never attempted.
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
	committed := parseMigrateCommitEvents(t, output)

	// The failing version must not appear in commit events.
	for _, v := range committed {
		if v == failVersion {
			t.Fatalf("migrate.commit emitted for failing version %d; expected none", failVersion)
		}
	}
	// v99999 must not appear (not attempted).
	for _, v := range committed {
		if v == 99999 {
			t.Fatalf("migrate.commit emitted for v99999 which should not have been attempted")
		}
	}
	// At least v1 and v2 (real SQL migrations) committed before the failure.
	if len(committed) == 0 {
		t.Fatal("expected at least one migrate.commit event before the failing migration")
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

// parseMigrateCommitEvents scans migrationEventWriter output for JSON
// lines with event="migrate.commit" and returns the version numbers in the
// order they appear. Used by per-migration-commit tests to assert order
// and completeness without depending on dolt_log state after a rollback.
func parseMigrateCommitEvents(t *testing.T, output string) []int64 {
	t.Helper()
	var versions []int64
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt["event"] != "migrate.commit" {
			continue
		}
		if v, ok := evt["version"].(float64); ok {
			versions = append(versions, int64(v))
		}
	}
	return versions
}
func TestEventLinesAreValidJSON(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	var buf bytes.Buffer
	origWriter := migrationEventWriter
	migrationEventWriter = &buf
	t.Cleanup(func() { migrationEventWriter = origWriter })

	st, err := Open(ctx, doltRoot, "event-json-ws-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("no event lines captured")
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Errorf("line %d is not valid JSON: %v\nline: %s", i+1, err, line)
			continue
		}
		if _, ok := evt["ts"]; !ok {
			t.Errorf("line %d missing 'ts' field: %s", i+1, line)
		}
		if _, ok := evt["event"]; !ok {
			t.Errorf("line %d missing 'event' field: %s", i+1, line)
		}
	}
}

// TestMigrationTimelineReconstructable verifies that event lines emitted during
// a normal Open contain enough information to reconstruct the migration timeline:
// migrate.start before migrate.commit, safety_branch.created first, and
// numeric version values in both events.
func TestMigrationTimelineReconstructable(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	var buf bytes.Buffer
	origWriter := migrationEventWriter
	migrationEventWriter = &buf
	t.Cleanup(func() { migrationEventWriter = origWriter })

	st, err := Open(ctx, doltRoot, "timeline-ws-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	type migEvt struct {
		Event   string  `json:"event"`
		Version float64 `json:"version"`
		Name    string  `json:"name"`
	}
	var events []migEvt
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var e migEvt
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		events = append(events, e)
	}

	// First event must be safety_branch.created.
	if len(events) == 0 || events[0].Event != "safety_branch.created" {
		first := ""
		if len(events) > 0 {
			first = events[0].Event
		}
		t.Fatalf("first event = %q, want safety_branch.created", first)
	}

	// For each version that has a migrate.commit, there must be a migrate.start
	// earlier in the sequence with the same version.
	startSeen := make(map[int64]int) // version → index of migrate.start
	for i, e := range events {
		if e.Event == "migrate.start" {
			startSeen[int64(e.Version)] = i
		}
		if e.Event == "migrate.commit" {
			startIdx, ok := startSeen[int64(e.Version)]
			if !ok {
				t.Errorf("migrate.commit for v%d has no preceding migrate.start", int64(e.Version))
			}
			if startIdx >= i {
				t.Errorf("migrate.start (idx=%d) not before migrate.commit (idx=%d) for v%d", startIdx, i, int64(e.Version))
			}
			// migrate.start must carry the "name" field.
			if events[startIdx].Name == "" {
				t.Errorf("migrate.start for v%d has empty name field", int64(e.Version))
			}
		}
	}
	// At least one migrate.commit must be present (we applied v1 baseline).
	found := false
	for _, e := range events {
		if e.Event == "migrate.commit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no migrate.commit event found in timeline")
	}
}
// TestMigrationLogSuccessRow verifies that after a successful Open the
// migration_log table contains at least one row with status='success',
// NULL error_text, and a non-NULL finished_at timestamp.
func TestMigrationLogSuccessRow(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "miglog-success-ws-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	type row struct {
		version      int64
		status       string
		errorText    *string
		rowsAffected int64
		finishedAt   *string
	}
	var got row
	if err := st.db.QueryRowContext(ctx,
		`SELECT version, status, error_text, rows_affected, finished_at
		 FROM migration_log WHERE status = 'success' LIMIT 1`).Scan(
		&got.version, &got.status, &got.errorText, &got.rowsAffected, &got.finishedAt,
	); err != nil {
		t.Fatalf("query migration_log success row error = %v", err)
	}
	if got.status != "success" {
		t.Errorf("status = %q, want 'success'", got.status)
	}
	if got.errorText != nil {
		t.Errorf("error_text = %q, want NULL", *got.errorText)
	}
	// rows_affected is defined (not NULL); value is 0 for DDL migrations.
	if got.finishedAt == nil {
		t.Error("finished_at is NULL, want a timestamp")
	}
}

// TestMigrationLogFailureRow verifies that after a failed migration the
// migration_log table contains a row with status='failure', error_text
// populated, and finished_at set. Because DOLT_RESET rolls back any writes
// made before the reset, the failure row is written after the reset (alongside
// the quarantine commit) so it survives.
func TestMigrationLogFailureRow(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// First open: apply real migrations so migration_log exists.
	first, err := Open(ctx, doltRoot, "miglog-fail-ws-id")
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Second open with an injected failing migration.
	const failVersion int64 = 99777
	t.Cleanup(installBadMigration(failVersion, errors.New("synthetic log test failure")))

	_, err = Open(ctx, doltRoot, "miglog-fail-ws-id")
	if err == nil {
		t.Fatal("Open() succeeded, want MigrationError")
	}
	var me *MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("got %T, want *MigrationError", err)
	}

	// Re-open without the injected migration to get a valid store handle.
	extraMigrationProviderOptions = nil
	st, err := Open(ctx, doltRoot, "miglog-fail-ws-id")
	if err != nil {
		t.Fatalf("recovery Open() = %v", err)
	}
	defer st.Close()

	var status, errorText string
	var finishedAt *string
	if err := st.db.QueryRowContext(ctx,
		`SELECT status, error_text, finished_at
		 FROM migration_log WHERE version = ? AND status = 'failure' LIMIT 1`,
		failVersion).Scan(&status, &errorText, &finishedAt); err != nil {
		t.Fatalf("query migration_log failure row error = %v (no failure row persisted after reset?)", err)
	}
	if status != "failure" {
		t.Errorf("status = %q, want 'failure'", status)
	}
	if errorText == "" {
		t.Error("error_text is empty, want a non-empty error message")
	}
	if finishedAt == nil {
		t.Error("finished_at is NULL, want a timestamp")
	}
}

// TestMigrationLogNotReadByProductionCode verifies that no production code
// reads migration_log to drive behavior — it is write-only observability.
// This test is a compile-time / grep assertion: if it finds a SELECT FROM
// migration_log in non-test source files, it fails.
//
// smoke.go is the documented exception: its smoke probe issues a SELECT
// against migration_log purely to confirm the expected columns exist (the
// rows are closed immediately, never scanned), so it does not "drive
// behavior" off migration_log content — only off whether the query
// succeeded. [LAW:behavior-not-structure] the structural grep is a proxy
// for the behavioral contract; smoke probes don't violate the behavioral
// contract, so they don't need to satisfy the proxy.
func TestMigrationLogNotReadByProductionCode(t *testing.T) {
	// Walk the Go source tree for SELECT ... FROM migration_log in
	// non-test files. runGrepInProductionCode enumerates files via
	// os.ReadDir on the package directory (tests run with cwd = the
	// package dir), then scans each non-_test.go file for the pattern.
	out, err := runGrepInProductionCode(t, "migration_log")
	if err != nil {
		t.Fatalf("grep error: %v", err)
	}
	for _, line := range out {
		if strings.HasPrefix(line, "smoke.go:") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "select") &&
			strings.Contains(strings.ToLower(line), "migration_log") {
			t.Errorf("production code reads migration_log (SELECT found):\n  %s", line)
		}
	}
}

// runGrepInProductionCode searches non-test Go source files in the store
// package for the given pattern. Returns matching lines.
func runGrepInProductionCode(t *testing.T, pattern string) ([]string, error) {
	t.Helper()
	// Enumerate production (non-_test.go) .go files in the current package
	// directory (internal/store).
	pkgDir := "." // tests run with working dir = package dir
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(pkgDir + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, pattern) {
				matches = append(matches, e.Name()+": "+line)
			}
		}
	}
	return matches, nil
}
