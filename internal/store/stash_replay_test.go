package store

import (
	"context"
	"database/sql"
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

// TestAnOpenReplaysTheStashARaiseWithoutReplayLeft is the store a release
// older than the stash raised: this registry steps it down, which stashes,
// and goose alone steps it up again, as that release's identical Ups do,
// replaying nothing. The next open writes the stash back and says so.
func TestAnOpenReplaysTheStashARaiseWithoutReplayLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	top := migrations.Baseline
	for v := range migrations.Restores {
		top = max(top, v)
	}
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider := storeAtVersion(t, migrations.FS, top)
	applySeed(t, st.db, seeds[top])
	before := exportTables(t, st.db)
	if _, err := provider.DownTo(ctx, top-1); err != nil {
		t.Fatalf("Down to v%d: %v", top-1, err)
	}
	if _, err := provider.UpTo(ctx, top); err != nil {
		t.Fatalf("Up to v%d without replay: %v", top, err)
	}
	if exportTables(t, st.db) == before {
		t.Fatalf("v%d's seed loses nothing to a raise without replay; seed what its Down stashes", top)
	}

	done := migrateLocked(t, ctx, st)
	if after := exportTables(t, st.db); after != before {
		t.Fatalf("the open did not write the stash back:\n%s", unifiedSchemaDiff("seeded", "after the open", before, after))
	}
	replayed, _ := done.Probed[stashReplayedFact].([]map[string]any)
	var rows int64
	for _, r := range replayed {
		if r["version"] != top {
			t.Errorf("replayed %v, want only v%d's stash", r, top)
		}
		rows += r["rows"].(int64)
	}
	if rows == 0 {
		t.Fatalf("%s = %v, want v%d's stash with the rows it wrote", stashReplayedFact, done.Probed[stashReplayedFact], top)
	}

	again := migrateLocked(t, ctx, st)
	if got, ok := again.Probed[stashReplayedFact]; ok {
		t.Fatalf("an open with nothing to write reports %s = %v, want it absent", stashReplayedFact, got)
	}
}

// TestALateReplayKeepsWhatWasWrittenSince is the same raise with a plan
// written after it and a stashed ticket deleted: the open's replay leaves the
// newer plan, and consumes every entry, the deleted ticket's too, so the next
// open has nothing pending.
func TestALateReplayKeepsWhatWasWrittenSince(t *testing.T) {
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
	var planned []string
	rows, err := st.db.QueryContext(ctx, `SELECT row_key FROM schema_stash
		WHERE table_name = 'issues' AND column_name = 'work_plan'
		ORDER BY JSON_UNQUOTE(JSON_EXTRACT(value, '$[0]')) = '', row_key`)
	if err != nil {
		t.Fatalf("read stashed plans: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read stashed plans: %v", err)
		}
		planned = append(planned, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("read stashed plans: %v", err)
	}
	if len(planned) < 2 {
		t.Fatalf("v11's seed stashes %d tickets' plans, want two: one rewritten, one deleted", len(planned))
	}
	kept, gone := planned[0], planned[1]
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET work_plan = 'written since' WHERE id = ?`, kept); err != nil {
		t.Fatalf("write a plan: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM issues WHERE id = ?`, gone); err != nil {
		t.Fatalf("delete a planned ticket: %v", err)
	}

	migrateLocked(t, ctx, st)
	var plan string
	if err := st.db.QueryRowContext(ctx, `SELECT work_plan FROM issues WHERE id = ?`, kept).Scan(&plan); err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if plan != "written since" {
		t.Fatalf("work_plan of %s = %q, want the plan written since the raise", kept, plan)
	}
	var held int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_stash`).Scan(&held); err != nil {
		t.Fatalf("count stash entries: %v", err)
	}
	if held != 0 {
		t.Fatalf("the replay left %d stash entries, want none", held)
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
