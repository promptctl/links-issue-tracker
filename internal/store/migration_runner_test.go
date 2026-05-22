package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/doltcli"
)

// TestFreshOpenStampsBaselineVersion pins the fresh-workspace acceptance: Open
// applies 00001_baseline.sql, goose records v1, and the apply lands as one
// Dolt commit whose message names the migration.
func TestFreshOpenStampsBaselineVersion(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	version, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if version != baselineVersion {
		t.Fatalf("recorded version = %d, want %d", version, baselineVersion)
	}

	log, err := doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log error = %v", err)
	}
	if !strings.Contains(log, "migrate: v1 00001_baseline.sql") {
		t.Fatalf("dolt log missing per-migration commit message:\n%s", log)
	}
}

// TestPreGooseAdoptionStampsWithoutRerunningBaseline pins the adoption path: a
// workspace already at the canonical shape but lacking goose history is
// re-stamped at the baseline version (not re-created), and the baseline tables
// survive untouched.
func TestPreGooseAdoptionStampsWithoutRerunningBaseline(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Seed a row so we can prove adoption preserves data (does not re-run baseline).
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at)
		 VALUES ('keep-me','Keep','', 'open', 0, 'task', 'misc', '', 'M', '2026-01-01', '2026-01-01')`,
	); err != nil {
		t.Fatalf("seed row error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "seed row"); err != nil {
		t.Fatalf("commit seed error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) adoption error = %v", err)
	}
	defer second.Close()

	version, err := second.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if version != baselineVersion {
		t.Fatalf("post-adoption version = %d, want %d", version, baselineVersion)
	}
	var seeded string
	if err := second.db.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = 'keep-me'`).Scan(&seeded); err != nil {
		t.Fatalf("seeded row missing after adoption (baseline was wrongly re-run?): %v", err)
	}
	if seeded != "Keep" {
		t.Fatalf("seeded row title = %q, want %q", seeded, "Keep")
	}
}

// TestAdoptionDeletesLegacySchemaVersionKey pins the one-source-of-truth
// cleanup: after adoption, the legacy meta.schema_version key is removed so
// goose_db_version is the sole applied-state authority.
func TestAdoptionDeletesLegacySchemaVersionKey(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.ExecRawForTest(ctx, `INSERT INTO meta(meta_key, meta_value) VALUES ('schema_version', '1')`); err != nil {
		t.Fatalf("seed legacy schema_version error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "seed legacy schema_version"); err != nil {
		t.Fatalf("commit legacy key error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) adoption error = %v", err)
	}
	defer second.Close()

	var present int
	err = second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'schema_version'`).Scan(&present)
	if err != nil {
		t.Fatalf("query legacy key error = %v", err)
	}
	if present != 0 {
		t.Fatal("adoption did not delete legacy meta.schema_version key")
	}
}

// stampGooseVersionAhead records an applied goose version one past the registry
// max, simulating a workspace written by a newer binary. It commits the working
// set so a subsequent Open observes the stamp from the committed working set.
func stampGooseVersionAhead(t *testing.T, ctx context.Context, doltRoot string) int64 {
	t.Helper()
	registryMax, err := registryMaxVersion()
	if err != nil {
		t.Fatalf("registryMaxVersion() error = %v", err)
	}
	ahead := registryMax + 1

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(stamp) error = %v", err)
	}
	if err := st.ExecRawForTest(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, ahead,
	); err != nil {
		_ = st.Close()
		t.Fatalf("stamp ahead version error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "stamp ahead version for test"); err != nil {
		_ = st.Close()
		t.Fatalf("commit ahead stamp error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(stamp) error = %v", err)
	}
	return ahead
}

// TestOpenRefusesWorkspaceAheadOfBinary pins the forward-compat refusal: a
// workspace stamped one version past the registry max is refused with a typed
// error naming both versions, instead of opening silently (the version-ahead
// path is a willMutate no-op, so without the guard it would slip through).
func TestOpenRefusesWorkspaceAheadOfBinary(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	if _, err := Open(ctx, doltRoot, "test-workspace-id"); err != nil {
		t.Fatalf("Open(fresh) error = %v", err)
	}
	ahead := stampGooseVersionAhead(t, ctx, doltRoot)
	registryMax := ahead - 1

	_, err := Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() of workspace-ahead returned nil error; want refusal")
	}
	var unsupported *UnsupportedSchemaVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Open() error = %v (%T); want *UnsupportedSchemaVersionError", err, err)
	}
	if unsupported.WorkspaceVersion != ahead {
		t.Errorf("WorkspaceVersion = %d, want %d", unsupported.WorkspaceVersion, ahead)
	}
	if unsupported.MaxSupported != registryMax {
		t.Errorf("MaxSupported = %d, want %d", unsupported.MaxSupported, registryMax)
	}
}

// TestUnsupportedSchemaVersionMessageShape pins the operator-facing remediation
// string so it cannot silently regress: it must name both versions and use the
// forward-only "please upgrade lit" phrasing, never "delete" or "manual SQL".
func TestUnsupportedSchemaVersionMessageShape(t *testing.T) {
	msg := (&UnsupportedSchemaVersionError{WorkspaceVersion: 7, MaxSupported: 3}).Error()

	want := "please upgrade lit (your workspace is at schema version 7; this binary supports up to 3)"
	if msg != want {
		t.Fatalf("Error() = %q, want %q", msg, want)
	}
	for _, forbidden := range []string{"delete", "DELETE", "manual SQL", "drop", "DROP"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("remediation message contains forbidden phrase %q: %q", forbidden, msg)
		}
	}
}

// TestOpenAllowsWorkspaceExactlyAtMax pins the boundary on the allow side: a
// workspace stamped exactly at the registry max opens cleanly as a no-op (the
// guard refuses strictly-greater only).
func TestOpenAllowsWorkspaceExactlyAtMax(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(fresh) error = %v", err)
	}
	registryMax, err := registryMaxVersion()
	if err != nil {
		t.Fatalf("registryMaxVersion() error = %v", err)
	}
	atMax, err := first.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if atMax != registryMax {
		t.Fatalf("fresh workspace stamped at %d, want registry max %d", atMax, registryMax)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() of workspace at exactly the max version = %v; want clean open", err)
	}
	defer second.Close()
}
