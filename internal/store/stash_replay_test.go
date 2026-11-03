package store

import (
	"context"
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
