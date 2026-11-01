package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestIsTopLevelIDMigrationBackfill runs 00012's Up over rows that predate the
// column. Every upgraded store's id size rests on this backfill: a row it missed
// would undercount, and a store migrated fresh has no rows to show that.
func TestIsTopLevelIDMigrationBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}
	if _, err := provider.DownTo(ctx, 11); err != nil {
		t.Fatalf("DownTo(11) error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range []string{"t-a", "t-a.1", "t-a.1.1", "t-b", "t-c.2"} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at)
			VALUES (?, ?, '', 'open', 0, 'task', 'misc', '', ?, '', ?, ?)`, id, "issue "+id, id, now, now); err != nil {
			t.Fatalf("seed issue %s: %v", id, err)
		}
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up() error = %v", err)
	}

	var got, want int
	if err := st.db.QueryRowContext(ctx, topLevelIDCount).Scan(&got); err != nil {
		t.Fatalf("count through the index: %v", err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id NOT LIKE '%.%'`).Scan(&want); err != nil {
		t.Fatalf("count by the id's shape: %v", err)
	}
	if want != 2 {
		t.Fatalf("seeded top-level ids = %d, want 2", want)
	}
	if got != want {
		t.Errorf("top-level ids through the index = %d, want %d", got, want)
	}
}
