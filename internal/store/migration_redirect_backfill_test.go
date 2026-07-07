package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestRedirectTargetMigrationBackfill exercises 00004's Up backfill and Down
// re-materialization against real pre-column data. The backfill contract:
// an issue with a redirecting resolution and EXACTLY ONE incident related-to
// edge gets that edge's counterpart as its redirect_target and the edge is
// deleted (it was machine-written bookkeeping); every other shape — ambiguous
// edge count, no edges, non-redirecting resolution — is left exactly as found,
// which is rendering parity with the pre-column reader. Down re-materializes
// one edge per recorded redirect so the pre-upgrade reader renders the same
// graph it wrote.
func TestRedirectTargetMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("DownTo(3) error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	closed := now
	seedIssue := func(id, status string, closedAt, resolution any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, resolution)
			VALUES (?, ?, '', ?, 0, 'task', 'redirect', '', ?, '', ?, ?, ?, ?)`,
			id, "issue "+id, status, id, now, now, closedAt, resolution); err != nil {
			t.Fatalf("seed issue %s: %v", id, err)
		}
	}
	seedEdge := func(src, dst string) {
		t.Helper()
		if src > dst {
			src, dst = dst, src
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'related-to', ?, 'seed')`, src, dst, now); err != nil {
			t.Fatalf("seed edge %s-%s: %v", src, dst, err)
		}
	}

	// t-canon: the canonical ticket; t-peer: an unrelated manual peer.
	seedIssue("t-canon", "open", nil, nil)
	seedIssue("t-peer", "open", nil, nil)
	// t-dup: redirecting close with exactly one edge — the unambiguous backfill row.
	seedIssue("t-dup", "closed", closed, "duplicate")
	seedEdge("t-dup", "t-canon")
	// t-ambig: redirecting close with TWO edges — the pre-column reader could not
	// tell the redirect from the peer; the backfill must not guess either.
	seedIssue("t-ambig", "closed", closed, "superseded")
	seedEdge("t-ambig", "t-canon")
	seedEdge("t-ambig", "t-peer")
	// t-noedge: redirecting close whose edge was lost — stays NULL, representable.
	seedIssue("t-noedge", "closed", closed, "duplicate")
	// t-wontfix: non-redirecting close with one edge — a manual peer link that
	// must survive untouched.
	seedIssue("t-wontfix", "closed", closed, "wontfix")
	seedEdge("t-wontfix", "t-peer")

	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("UpTo(4) error = %v", err)
	}

	wantTargets := map[string]string{
		"t-dup":     "t-canon",
		"t-ambig":   "",
		"t-noedge":  "",
		"t-wontfix": "",
	}
	for id, want := range wantTargets {
		if got := readRedirectTarget(t, ctx, st, id); got != want {
			t.Fatalf("after Up: %s redirect_target = %q, want %q", id, got, want)
		}
	}
	wantEdges := map[string]int{
		"t-dup":     0, // machine edge consumed by the backfill
		"t-ambig":   2, // ambiguous rows keep every edge
		"t-wontfix": 1, // manual peer link untouched
	}
	for id, want := range wantEdges {
		if got := countIncidentRelated(t, ctx, st, id); got != want {
			t.Fatalf("after Up: %s incident related-to edges = %d, want %d", id, got, want)
		}
	}

	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("DownTo(3) after backfill error = %v", err)
	}
	// The recorded redirect re-materializes as one canonical (sorted) edge; the
	// untouched rows keep exactly what they had.
	if got := countIncidentRelated(t, ctx, st, "t-dup"); got != 1 {
		t.Fatalf("after Down: t-dup incident related-to edges = %d, want 1 (re-materialized)", got)
	}
	var src, dst string
	if err := st.db.QueryRowContext(ctx, `SELECT src_id, dst_id FROM relations WHERE type = 'related-to' AND (src_id = 't-dup' OR dst_id = 't-dup')`).Scan(&src, &dst); err != nil {
		t.Fatalf("after Down: read re-materialized edge: %v", err)
	}
	if src != "t-canon" || dst != "t-dup" {
		t.Fatalf("after Down: re-materialized edge = (%s, %s), want canonical sorted endpoints (t-canon, t-dup)", src, dst)
	}
	if got := countIncidentRelated(t, ctx, st, "t-ambig"); got != 2 {
		t.Fatalf("after Down: t-ambig incident related-to edges = %d, want 2", got)
	}
	if got := countIncidentRelated(t, ctx, st, "t-wontfix"); got != 1 {
		t.Fatalf("after Down: t-wontfix incident related-to edges = %d, want 1", got)
	}
}

// TestRedirectTargetMigrationDownToleratesExistingManualEdge pins the Down's
// INSERT IGNORE contract: when a manual edge already links the redirect pair
// (legal after the recut — the round trip the column exists to allow), Down
// must not fail on the primary-key collision; the edge the redirect needs
// already exists.
func TestRedirectTargetMigrationDownToleratesExistingManualEdge(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at)
		VALUES ('t-canon', 'canonical', '', 'open', 0, 'task', 'redirect', '', 'a', '', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed canonical: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, resolution, redirect_target)
		VALUES ('t-dup', 'duplicate', '', 'closed', 0, 'task', 'redirect', '', 'b', '', ?, ?, ?, 'duplicate', 't-canon')`, now, now, now); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES ('t-canon', 't-dup', 'related-to', ?, 'seed')`, now); err != nil {
		t.Fatalf("seed manual edge: %v", err)
	}

	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("DownTo(3) with pre-existing manual edge error = %v", err)
	}
	if got := countIncidentRelated(t, ctx, st, "t-dup"); got != 1 {
		t.Fatalf("after Down: t-dup incident related-to edges = %d, want 1 (the existing edge, no duplicate)", got)
	}
}

func readRedirectTarget(t *testing.T, ctx context.Context, st *Store, id string) string {
	t.Helper()
	var target sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT redirect_target FROM issues WHERE id = ?`, id).Scan(&target); err != nil {
		t.Fatalf("read redirect_target of %s: %v", id, err)
	}
	return target.String
}

func countIncidentRelated(t *testing.T, ctx context.Context, st *Store, id string) int {
	t.Helper()
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations WHERE type = 'related-to' AND (src_id = ? OR dst_id = ?)`, id, id).Scan(&count); err != nil {
		t.Fatalf("count related edges of %s: %v", id, err)
	}
	return count
}
