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

// interruptedAtV11 leaves at dir the store a process left when it died between
// v11's Up and its stash replay: stepped down to v10 and committed there, then
// settled and raised by goose with nothing replayed or committed. v11 is the
// top of the registry, so nothing pending runs it, and its Up creates
// plan_reviews, which a hard reset alone keeps. It returns the open store, an
// issue the step stashed, and that issue's plan.
func interruptedAtV11(t *testing.T, dir string) (*Store, string, string) {
	t.Helper()
	ctx := context.Background()
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider, err := openAtVersion(ctx, dir, migrations.FS, 11)
	if st != nil {
		t.Cleanup(func() { _ = st.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	applySeed(t, st.db, seeds[11])
	stashed := queryOne(t, st.db, `SELECT id FROM issues WHERE work_plan <> '' ORDER BY id LIMIT 1`)
	plan := queryOne(t, st.db, `SELECT work_plan FROM issues WHERE id = '`+stashed+`'`)
	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("Down to v10: %v", err)
	}
	if err := st.commitWorkingSet(ctx, "stepped down to v10"); err != nil {
		t.Fatalf("commit v10: %v", err)
	}
	if _, err := migrations.SettleStash(ctx, st.db, 11); err != nil {
		t.Fatalf("settle v11: %v", err)
	}
	if _, err := provider.UpByOne(ctx); err != nil {
		t.Fatalf("Up to v11: %v", err)
	}
	return st, stashed, plan
}

// requireReplayed fails t unless st holds plan back on stashed and an empty
// carrier.
func requireReplayed(t *testing.T, st *Store, stashed, plan string) {
	t.Helper()
	if got := queryOne(t, st.db, `SELECT COALESCE(work_plan, '') FROM issues WHERE id = '`+stashed+`'`); got != plan {
		t.Fatalf("work_plan of %s = %q, want the stashed %q", stashed, got, plan)
	}
	if got := queryOne(t, st.db, `SELECT CAST(COUNT(*) AS CHAR) FROM schema_stash`); got != "0" {
		t.Fatalf("the carrier holds %s entries, want none", got)
	}
}

// TestAStepThatDiedBeforeItsReplayRunsAgain: the next open writes the stash
// back, the carrier is empty, and the open's event names the replay, not a
// hold.
func TestAStepThatDiedBeforeItsReplayRunsAgain(t *testing.T) {
	t.Parallel()
	st, stashed, plan := interruptedAtV11(t, migratedDoltDir(t))

	done := migrateLocked(t, context.Background(), st)
	requireReplayed(t, st, stashed, plan)
	replayed, _ := done.Probed[stashReplayedFact].([]map[string]any)
	if !slices.ContainsFunc(replayed, func(r map[string]any) bool {
		return r["version"] == int64(11) && r["stash"] == "issues.work_plan"
	}) {
		t.Fatalf("%s = %v, want v11's issues.work_plan", stashReplayedFact, done.Probed[stashReplayedFact])
	}
	if held, _ := done.Probed[stashHeldFact].([]map[string]any); len(held) != 0 {
		t.Fatalf("%s = %v, want none", stashHeldFact, held)
	}
}

// TestAStepWhoseCommitFailedAfterStagingRunsAgain is the same step left
// staged, as a DOLT_COMMIT -A that failed after its add leaves it: the step
// still runs again whole.
func TestAStepWhoseCommitFailedAfterStagingRunsAgain(t *testing.T) {
	t.Parallel()
	st, stashed, plan := interruptedAtV11(t, migratedDoltDir(t))
	if _, err := st.db.Exec(`CALL DOLT_ADD('-A')`); err != nil {
		t.Fatalf("stage the step: %v", err)
	}

	migrateLocked(t, context.Background(), st)
	requireReplayed(t, st, stashed, plan)
}

// TestANewerLitsInterruptedStepIsUndoneByAWriterItAllows: a step above this
// binary's registry, left uncommitted on a store this binary may write, is
// reset away, so this binary's commits never version it.
func TestANewerLitsInterruptedStepIsUndoneByAWriterItAllows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := storeAtVersion(t, migrations.FS, 11)
	if err := st.commitWorkingSet(ctx, "at v11"); err != nil {
		t.Fatalf("commit v11: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (12, 1)`); err != nil {
		t.Fatalf("record v12: %v", err)
	}

	migrateLocked(t, ctx, st)
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != 11 {
		t.Fatalf("working schema = v%d (%v), want v11", got, err)
	}
}

// TestAReadOpenHandsAnInterruptedStepToTheWriteOpen: a read serves no half
// step as applied; it hands off, and the write open runs the step again.
func TestAReadOpenHandsAnInterruptedStepToTheWriteOpen(t *testing.T) {
	t.Parallel()
	dir := migratedDoltDir(t)
	st, stashed, plan := interruptedAtV11(t, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reader, err := OpenForRead(context.Background(), testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	requireReplayed(t, reader, stashed, plan)
}

// TestTheSyncOpenVersionsNoInterruptedStep: the sync open undoes the step
// before any write of its own, so its stranded-write commit cannot make the
// half step HEAD's, and the next migrating open runs it whole.
func TestTheSyncOpenVersionsNoInterruptedStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := migratedDoltDir(t)
	st, stashed, plan := interruptedAtV11(t, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	syncer, err := OpenSync(ctx, testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync: %v", err)
	}
	if err := syncer.withCommitLock(ctx, func(ctx context.Context) error {
		return syncer.commitStrandedWrites(ctx, strandedWritesCommitMessage)
	}); err != nil {
		t.Fatalf("commit stranded writes: %v", err)
	}
	if got, err := syncer.schemaVersionAtCommit(ctx, "HEAD"); err != nil || got != 10 {
		t.Fatalf("HEAD's schema = v%d (%v), want v10", got, err)
	}
	if err := syncer.Close(); err != nil {
		t.Fatalf("close the sync store: %v", err)
	}

	writer, err := Open(ctx, testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	requireReplayed(t, writer, stashed, plan)
}

// TestAStepThatDiedAtItsSettleRunsAgain is a process that died after v4's
// settle dropped a unit a write below changed, before goose recorded v4's Up:
// the next open settles again from HEAD's carrier and reports the drop the
// dead settle made, which a retry over the half-settled carrier reports as 0.
// A write to another table since the settle stands.
func TestAStepThatDiedAtItsSettleRunsAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider := storeAtVersion(t, migrations.FS, 4)
	applySeed(t, st.db, seeds[4])
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("Down to v3: %v", err)
	}
	execSQL(t, st.db, `UPDATE issues SET status = 'closed', closed_at = '2026-02-01T00:00:00Z', resolution = 'duplicate' WHERE id = 'seed-t4'`)
	if err := st.commitWorkingSet(ctx, "written below v4"); err != nil {
		t.Fatalf("commit v3: %v", err)
	}
	if _, err := migrations.SettleStash(ctx, st.db, 4); err != nil {
		t.Fatalf("settle v4: %v", err)
	}
	execSQL(t, st.db, `UPDATE issues SET title = 'written after the settle' WHERE id = 'seed-t4'`)

	done := migrateLocked(t, ctx, st)
	var title string
	if err := st.db.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = 'seed-t4'`).Scan(&title); err != nil {
		t.Fatalf("read seed-t4's title: %v", err)
	}
	if title != "written after the settle" {
		t.Fatalf("seed-t4's title = %q, want the write the undo of the settle leaves standing", title)
	}
	settled, _ := done.Probed[stashSettledFact].([]map[string]any)
	if !slices.ContainsFunc(settled, func(r map[string]any) bool {
		return r["version"] == int64(4) && r["stash"] == "issues.redirect_target" && r["rows"] == int64(1)
	}) {
		t.Fatalf("%s = %v, want v4's issues.redirect_target with one unit", stashSettledFact, done.Probed[stashSettledFact])
	}
}
