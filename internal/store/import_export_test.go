package store

import (
	"context"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Nothing reads this label back, so this test is the only thing holding it to
// the code it names.
func TestFixIntegrityStampsItsCommitLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Self", Topic: "integrity", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	// Seeded through the db directly because AddRelation refuses a self-targeting
	// related-to (relations.go:297); only a path that bypasses that guard can
	// leave the row this repair deletes.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'related-to', ?, 'seed')`,
		issue.ID, issue.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed self related-to row error = %v", err)
	}
	// Committed before the repair runs, or the seed and its deletion cancel out
	// in one working set and the repair has no diff to commit.
	if err := st.commitWorkingSetOnce(ctx, commitStamp{Message: "seed self related-to"}); err != nil {
		t.Fatalf("commit seeded row error = %v", err)
	}

	before, err := headCommitHash(ctx, st)
	if err != nil {
		t.Fatalf("read head before repair: %v", err)
	}

	if _, err := st.FixIntegrity(ctx); err != nil {
		t.Fatalf("FixIntegrity() error = %v", err)
	}

	var selfRows int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id = dst_id`).Scan(&selfRows); err != nil {
		t.Fatalf("count self related-to rows: %v", err)
	}
	if selfRows != 0 {
		t.Fatalf("self related-to rows after repair = %d, want 0", selfRows)
	}

	// withMutation does not set AllowEmpty, so a repair that changed nothing
	// lands no commit and HEAD still carries whatever message preceded it. The
	// message assertion below only means something once HEAD has moved.
	after, err := headCommitHash(ctx, st)
	if err != nil {
		t.Fatalf("read head after repair: %v", err)
	}
	if after == before {
		t.Fatalf("head did not move across the repair (%s), so the message below proves nothing", before)
	}

	var message string
	if err := st.db.QueryRowContext(ctx, `SELECT message FROM dolt_log('HEAD') LIMIT 1`).Scan(&message); err != nil {
		t.Fatalf("read repair commit message: %v", err)
	}
	if message != "fix integrity" {
		t.Fatalf("repair commit message = %q, want %q", message, "fix integrity")
	}
}

func headCommitHash(ctx context.Context, st *Store) (string, error) {
	var hash string
	err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log('HEAD') LIMIT 1`).Scan(&hash)
	return hash, err
}
