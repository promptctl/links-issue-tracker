package store

import (
	"context"
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
