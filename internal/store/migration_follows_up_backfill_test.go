package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestFollowsUpMigrationBackfill exercises 00006's Up backfill and Down
// re-nesting against real pre-column data. The backfill contract: every child
// of a closed non-container records that parent as the ticket it follows up and
// moves beside it — under the nearest ancestor that is a container or still
// open, or to the top level when the climb finds none — while a child of an
// open leaf or of an epic is left exactly as found. Down re-nests each
// follow-up under the ticket it follows up so the pre-upgrade reader renders
// the graph it wrote.
func TestFollowsUpMigrationBackfill(t *testing.T) {
	t.Parallel()
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
	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("DownTo(5) error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	seedIssue := func(id, issueType string, status any) {
		t.Helper()
		var closedAt any
		if status == "closed" {
			closedAt = now
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at)
			VALUES (?, ?, '', ?, 0, ?, 'followup', '', ?, '', ?, ?, ?)`,
			id, "issue "+id, status, issueType, id, now, now, closedAt); err != nil {
			t.Fatalf("seed issue %s: %v", id, err)
		}
	}
	seedParent := func(child, parent string) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, 'seed')`, child, parent, now); err != nil {
			t.Fatalf("seed parent %s -> %s: %v", child, parent, err)
		}
	}

	// t-epic holds t-closed, a finished task that `lit followup` nested t-f1
	// under: the shape the column exists to retire. t-f1 belongs beside
	// t-closed, inside the epic.
	seedIssue("t-epic", "epic", nil)
	seedIssue("t-closed", "task", "closed")
	seedParent("t-closed", "t-epic")
	seedIssue("t-f1", "task", "open")
	seedParent("t-f1", "t-closed")
	// t-top is a closed top-level bug with a follow-up under it: the climb finds
	// no epic, so t-f2 becomes top-level, beside t-top. The follow-up is itself
	// closed, which changes nothing — the shape, not the child's state, is what
	// the backfill reads.
	seedIssue("t-top", "bug", "closed")
	seedIssue("t-f2", "task", "closed")
	seedParent("t-f2", "t-top")
	// A follow-up filed on a follow-up: t-chain (closed, top-level) holds t-mid
	// (closed), which holds t-f3. One hop would re-nest t-f3 under t-chain,
	// another closed leaf; the climb carries it to the top level. t-mid records
	// t-chain and t-f3 records t-mid — each names the ticket it was filed on.
	seedIssue("t-chain", "task", "closed")
	seedIssue("t-mid", "task", "closed")
	seedParent("t-mid", "t-chain")
	seedIssue("t-f3", "task", "open")
	seedParent("t-f3", "t-mid")
	// Controls. t-open is a leaf that is still open: its child is not a
	// follow-up of finished work and stays put. t-closed itself is a child of
	// an epic and stays put — a closed child of a container is ordinary.
	seedIssue("t-open", "task", "open")
	seedIssue("t-c", "task", "open")
	seedParent("t-c", "t-open")

	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("UpTo(6) error = %v", err)
	}

	wantFollowsUp := map[string]string{
		"t-f1":     "t-closed",
		"t-f2":     "t-top",
		"t-mid":    "t-chain",
		"t-f3":     "t-mid",
		"t-c":      "",
		"t-closed": "",
	}
	for id, want := range wantFollowsUp {
		if got := readFollowsUp(t, ctx, st, id); got != want {
			t.Errorf("after Up: %s follows_up = %q, want %q", id, got, want)
		}
	}
	wantParent := map[string]string{
		"t-f1":     "t-epic",
		"t-f2":     "",
		"t-mid":    "",
		"t-f3":     "",
		"t-c":      "t-open",
		"t-closed": "t-epic",
	}
	for id, want := range wantParent {
		if got := readParent(t, ctx, st, id); got != want {
			t.Errorf("after Up: %s parent = %q, want %q", id, got, want)
		}
	}

	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("DownTo(5) after backfill error = %v", err)
	}
	// Every follow-up is back under the ticket it follows up: the moved edge
	// repointed, the deleted ones re-materialized. The controls are untouched.
	wantRestored := map[string]string{
		"t-f1":     "t-closed",
		"t-f2":     "t-top",
		"t-mid":    "t-chain",
		"t-f3":     "t-mid",
		"t-c":      "t-open",
		"t-closed": "t-epic",
	}
	for id, want := range wantRestored {
		if got := readParent(t, ctx, st, id); got != want {
			t.Errorf("after Down: %s parent = %q, want %q", id, got, want)
		}
	}
	var columns int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'issues' AND column_name = 'follows_up'`).Scan(&columns); err != nil {
		t.Fatalf("probe follows_up column: %v", err)
	}
	if columns != 0 {
		t.Errorf("after Down: follows_up column still present")
	}
}

func readFollowsUp(t *testing.T, ctx context.Context, st *Store, id string) string {
	t.Helper()
	var target sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT follows_up FROM issues WHERE id = ?`, id).Scan(&target); err != nil {
		t.Fatalf("read follows_up of %s: %v", id, err)
	}
	return target.String
}

// readParent returns the single parent an issue's parent-child edge names, or
// "" when it has none. Two edges would be a second fact the test did not seed
// and is reported as such.
func readParent(t *testing.T, ctx context.Context, st *Store, id string) string {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, `SELECT dst_id FROM relations WHERE src_id = ? AND type = 'parent-child'`, id)
	if err != nil {
		t.Fatalf("read parent of %s: %v", id, err)
	}
	parents, err := scanIDColumn(rows)
	if err != nil {
		t.Fatalf("scan parents of %s: %v", id, err)
	}
	switch len(parents) {
	case 0:
		return ""
	case 1:
		return parents[0]
	default:
		t.Fatalf("%s has %d parents %v, want at most one", id, len(parents), parents)
		return ""
	}
}
