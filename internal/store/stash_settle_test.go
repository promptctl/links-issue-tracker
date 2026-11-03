package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// TestARaiseKeepsWhatWasWrittenBelowTheDown steps a seeded store down one
// version, writes there, and opens it, which raises it again: the write made
// below survives the raise, and a store written nothing below comes back as
// a store never stepped down does.
func TestARaiseKeepsWhatWasWrittenBelowTheDown(t *testing.T) {
	t.Parallel()
	seeds := seedsByVersion(t, registryMigrations(t))
	cases := []struct {
		name string
		at   int64
		// below writes at the version below at, and returns what to check
		// once the store is raised.
		below func(t *testing.T, db *sql.DB) func(t *testing.T, db *sql.DB)
	}{
		{"a ticket closed as a duplicate below v4 keeps the redirect its Up derives", 4, func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			execSQL(t, db, `UPDATE issues SET status = 'closed', closed_at = '2026-02-01T00:00:00Z', updated_at = '2026-02-01T00:00:00Z', resolution = 'duplicate' WHERE id = 'seed-t4'`)
			execSQL(t, db, `INSERT INTO relations (src_id, dst_id, type, created_at, created_by) VALUES ('seed-t4', 'seed-t1', 'related-to', '2026-02-01T00:00:00Z', 'test')`)
			return func(t *testing.T, db *sql.DB) {
				if got := queryOne(t, db, `SELECT redirect_target FROM issues WHERE id = 'seed-t4'`); got != "seed-t1" {
					t.Errorf("redirect_target of seed-t4 = %q, want seed-t1", got)
				}
			}
		}},
		{"a related-to edge deleted below v4 stays deleted", 4, func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			execSQL(t, db, `DELETE FROM relations WHERE type = 'related-to' AND src_id = 'seed-t1' AND dst_id = 'seed-t5'`)
			return func(t *testing.T, db *sql.DB) {
				if got := queryOne(t, db, `SELECT CAST(COUNT(*) AS CHAR) FROM relations WHERE type = 'related-to' AND ((src_id = 'seed-t1' AND dst_id = 'seed-t5') OR (src_id = 'seed-t5' AND dst_id = 'seed-t1'))`); got != "0" {
					t.Errorf("%s related-to edges between seed-t1 and seed-t5, want none", got)
				}
			}
		}},
		{"a follow-up reparented below v6 keeps its new parent", 6, func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			followUp := queryOne(t, db, `SELECT c.id FROM issues c WHERE EXISTS (SELECT 1 FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'follows_up' AND s.row_key = c.id AND JSON_EXTRACT(s.value, '$[0]') IS NOT NULL AND JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) <> 'NULL') ORDER BY c.id LIMIT 1`)
			fileUnderNewEpic(t, db, followUp)
			return func(t *testing.T, db *sql.DB) {
				if got := parentsOf(t, db, followUp); !slices.Equal(got, []string{"below-epic"}) {
					t.Errorf("parents of %s = %v, want [below-epic]", followUp, got)
				}
			}
		}},
		{"an unfiled child filed under a real epic below v7 has that one parent", 7, func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			fileUnderNewEpic(t, db, "seed-t4")
			lane := queryOne(t, db, `SELECT lane FROM issues WHERE id = 'seed-t4'`)
			return func(t *testing.T, db *sql.DB) {
				if got := parentsOf(t, db, "seed-t4"); !slices.Equal(got, []string{"below-epic"}) {
					t.Errorf("parents of seed-t4 = %v, want [below-epic]", got)
				}
				if got := queryOne(t, db, `SELECT lane FROM issues WHERE id = 'seed-t4'`); got != lane {
					t.Errorf("lane of seed-t4 = %q, want %q, the lane it had below", got, lane)
				}
			}
		}},
		{"a redirect's ticket resolved otherwise below v4 keeps no redirect", 4, func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			redirected := queryOne(t, db, `SELECT s.row_key FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'redirect_target' AND JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) <> 'NULL' ORDER BY s.row_key LIMIT 1`)
			execSQL(t, db, `UPDATE issues SET resolution = 'wontfix' WHERE id = '`+redirected+`'`)
			return func(t *testing.T, db *sql.DB) {
				if got := queryOne(t, db, `SELECT redirect_target FROM issues WHERE id = '`+redirected+`'`); got != "" {
					t.Errorf("redirect_target of %s = %q, want none on a ticket resolved wontfix", redirected, got)
				}
			}
		}},
		{"a related-to edge re-created below v4 does not get its stashed author back", 4, func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			const edge = `type = 'related-to' AND src_id = 'seed-t1' AND dst_id = 'seed-t5'`
			execSQL(t, db, `UPDATE relations SET created_by = 'below' WHERE `+edge)
			return func(t *testing.T, db *sql.DB) {
				if got := queryOne(t, db, `SELECT CAST(COUNT(*) AS CHAR) FROM relations WHERE `+edge+` AND created_by = 'alice'`); got != "0" {
					t.Errorf("the seed-t1 related-to edge came back with its stashed author, alice")
				}
			}
		}},
		{"a store written nothing below v4 comes back whole", 4,func(t *testing.T, db *sql.DB) func(*testing.T, *sql.DB) {
			return func(t *testing.T, db *sql.DB) {
				ref, _ := storeAtVersion(t, migrations.FS, 4)
				applySeed(t, ref.db, seeds[4])
				migrateLocked(t, context.Background(), ref)
				if want, got := exportTables(t, ref.db), exportTables(t, db); got != want {
					t.Errorf("the raised store differs from one never stepped down:\n%s", unifiedSchemaDiff("never stepped down", "stepped down and raised", want, got))
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st, provider := storeAtVersion(t, migrations.FS, tc.at)
			applySeed(t, st.db, seeds[tc.at])
			if _, err := provider.DownTo(ctx, tc.at-1); err != nil {
				t.Fatalf("Down to v%d: %v", tc.at-1, err)
			}
			check := tc.below(t, st.db)
			migrateLocked(t, ctx, st)
			check(t, st.db)
		})
	}
}

// TestASettleReportsWhatItDropped is the duplicate case's open: its event
// names the stash whose units a write below settled.
func TestASettleReportsWhatItDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider := storeAtVersion(t, migrations.FS, 4)
	applySeed(t, st.db, seeds[4])
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("Down to v3: %v", err)
	}
	execSQL(t, st.db, `UPDATE issues SET status = 'closed', closed_at = '2026-02-01T00:00:00Z', resolution = 'duplicate' WHERE id = 'seed-t4'`)
	done := migrateLocked(t, ctx, st)
	settled, _ := done.Probed[stashSettledFact].([]map[string]any)
	if !slices.ContainsFunc(settled, func(r map[string]any) bool {
		return r["version"] == int64(4) && r["stash"] == "issues.redirect_target" && r["rows"] == int64(1)
	}) {
		t.Fatalf("%s = %v, want v4's issues.redirect_target with one unit", stashSettledFact, done.Probed[stashSettledFact])
	}
}

// TestADownReplacesWhatARaiseWithoutReplayLeft: an entry a raise that never
// replayed left in the carrier is not the store this Down steps away from,
// so the raise after it does not write it.
func TestADownReplacesWhatARaiseWithoutReplayLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seeds := seedsByVersion(t, registryMigrations(t))
	st, provider := storeAtVersion(t, migrations.FS, 4)
	applySeed(t, st.db, seeds[4])
	execSQL(t, st.db, strings.TrimSpace(migrations.StashCarrierDDL))
	execSQL(t, st.db, `INSERT INTO schema_stash (table_name, row_key, column_name, value) VALUES ('relations', CONCAT_WS(CHAR(31), 'seed-t1', 'seed-t2', 'related-to'), '*related-to', JSON_ARRAY('seed-t1', 'seed-t2', 'related-to', '2026-01-01 00:00:00', 'held'))`)
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("Down to v3: %v", err)
	}
	migrateLocked(t, ctx, st)
	if got := queryOne(t, st.db, `SELECT CAST(COUNT(*) AS CHAR) FROM relations WHERE created_by = 'held'`); got != "0" {
		t.Errorf("%s held related-to edges written by the raise, want none", got)
	}
}

func execSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func queryOne(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return got.String
}

// fileUnderNewEpic makes id the one child of a new epic, below-epic.
func fileUnderNewEpic(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	execSQL(t, db, `INSERT INTO issues (id, title, description, status, priority, issue_type, topic, assignee, created_at, updated_at) VALUES ('below-epic', 'Filed below', '', NULL, 0, 'epic', 'core', '', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z')`)
	execSQL(t, db, `DELETE FROM relations WHERE type = 'parent-child' AND src_id = '`+id+`'`)
	execSQL(t, db, `INSERT INTO relations (src_id, dst_id, type, created_at, created_by) VALUES ('`+id+`', 'below-epic', 'parent-child', '2026-02-01T00:00:00Z', 'test')`)
}

func parentsOf(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT dst_id FROM relations WHERE type = 'parent-child' AND src_id = ? ORDER BY dst_id`, id)
	if err != nil {
		t.Fatalf("read parents of %s: %v", id, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("read parents of %s: %v", id, err)
		}
		got = append(got, p)
	}
	return got
}
