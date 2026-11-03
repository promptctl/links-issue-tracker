package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// migrateLocked runs st's migration as Open does, under the commit lock.
func migrateLocked(t *testing.T, ctx context.Context, st *Store) storage.Migrated {
	t.Helper()
	var done storage.Migrated
	if err := st.withCommitLock(ctx, func(ctx context.Context) (err error) {
		done, err = st.migrate(ctx)
		return err
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return done
}

// TestARaiseWithoutReplayHoldsItsStash is the store a release older than the
// stash raised: this registry steps it down, which stashes, and goose alone
// steps it up again, as that release's identical Ups do, replaying nothing.
// A plan is written since. No settle ran before those Ups, so the next open
// writes none of the stash: the plan written since stays, every entry stays
// in the carrier, and the open's event names each stash it holds, as does
// every open after it.
func TestARaiseWithoutReplayHoldsItsStash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider := storeAtVersion(t, migrations.FS, 11)
	applySeed(t, st.db, seeds[11])
	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("Down to v10: %v", err)
	}
	if _, err := provider.UpTo(ctx, 11); err != nil {
		t.Fatalf("Up to v11 without replay: %v", err)
	}
	kept := queryOne(t, st.db, `SELECT row_key FROM schema_stash WHERE table_name = 'issues' AND column_name = 'work_plan' ORDER BY row_key LIMIT 1`)
	execSQL(t, st.db, `UPDATE issues SET work_plan = 'written since' WHERE id = '`+kept+`'`)
	entries := queryOne(t, st.db, `SELECT CAST(COUNT(*) AS CHAR) FROM schema_stash`)

	for _, open := range []string{"the first open", "the next open"} {
		done := migrateLocked(t, ctx, st)
		if got := queryOne(t, st.db, `SELECT work_plan FROM issues WHERE id = '`+kept+`'`); got != "written since" {
			t.Fatalf("after %s, work_plan of %s = %q, want the plan written since the raise", open, kept, got)
		}
		if got := queryOne(t, st.db, `SELECT CAST(COUNT(*) AS CHAR) FROM schema_stash`); got != entries {
			t.Fatalf("after %s, the carrier holds %s entries, want the %s it held", open, got, entries)
		}
		held, _ := done.Probed[stashHeldFact].([]map[string]any)
		if !slices.ContainsFunc(held, func(r map[string]any) bool {
			return r["version"] == int64(11) && r["stash"] == "issues.work_plan" && r["rows"].(int64) > 0
		}) {
			t.Fatalf("after %s, %s = %v, want v11's issues.work_plan", open, stashHeldFact, done.Probed[stashHeldFact])
		}
	}
}

// TestARedirectReopenedBelowStaysUnwritten is a duplicate with a redirect,
// stepped down below v4 and reopened there: the open that raises it again
// writes no redirect onto an open ticket, which issues_redirect_target_check
// refuses, so the raise completes.
func TestARedirectReopenedBelowStaysUnwritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider := storeAtVersion(t, migrations.FS, 4)
	applySeed(t, st.db, seeds[4])
	var id string
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM issues WHERE redirect_target IS NOT NULL ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("v4's seed holds no redirect: %v", err)
	}
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("Down to v3: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET status = 'open', resolution = NULL, closed_at = NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("reopen %s: %v", id, err)
	}

	migrateLocked(t, ctx, st)
	var redirect sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT redirect_target FROM issues WHERE id = ?`, id).Scan(&redirect); err != nil {
		t.Fatalf("read the redirect: %v", err)
	}
	if redirect.Valid {
		t.Fatalf("redirect_target of reopened %s = %q, want none", id, redirect.String)
	}
}

// TestAWritingOpenReportsAnEmptyReplay is a raise with an empty stash: the
// open writes, replays nothing, and reports the empty list, so "nothing to
// replay" reads apart from "the replay never ran".
func TestAWritingOpenReportsAnEmptyReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := storeAtVersion(t, migrations.FS, migrations.Baseline)
	done := migrateLocked(t, ctx, st)
	got, ok := done.Probed[stashReplayedFact].([]map[string]any)
	if !ok || len(got) != 0 {
		t.Fatalf("%s = %#v, want an empty list", stashReplayedFact, done.Probed[stashReplayedFact])
	}
}
