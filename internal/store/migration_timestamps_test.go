package store

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

const timestampsMigrationVersion = 12

// v11Seeded is a store at v11 holding the v11 seed: every table filled, every
// timestamp spelled the trimmed way v11 wrote it.
func v11Seeded(t *testing.T) (*Store, func() error) {
	t.Helper()
	ctx := context.Background()
	st, provider := storeAtVersion(t, migrations.FS, timestampsMigrationVersion-1)
	applySeed(t, st.db, seedsByVersion(t, registryMigrations(t))[timestampsMigrationVersion-1])
	return st, func() error {
		_, err := provider.UpTo(ctx, timestampsMigrationVersion)
		return err
	}
}

// An offset spelling keeps its instant: v12 stores it in UTC, nine digits.
func TestTimestampsMigrationNormalizesAnOffsetToTheSameInstantInUTC(t *testing.T) {
	ctx := context.Background()
	st, up := v11Seeded(t)
	for _, stmt := range []string{
		`UPDATE issues SET created_at = '2026-01-01T02:00:00.5+02:00' WHERE id = 'seed-t1'`,
		`UPDATE issues SET updated_at = '2026-01-03T00:00:00-05:30' WHERE id = 'seed-t1'`,
		`UPDATE comments SET created_at = '2026-01-03T09:00:00.123456789+09:00' WHERE id = 'seed-c1'`,
		`UPDATE issue_event_changes SET to_value = '2026-01-04T05:00:00.25+05:00' WHERE event_id = 'seed-e1' AND field = 'closed_at'`,
	} {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := up(); err != nil {
		t.Fatalf("Up to v%d: %v", timestampsMigrationVersion, err)
	}
	for _, c := range []struct{ query, want string }{
		{`SELECT created_at FROM issues WHERE id = 'seed-t1'`, "2026-01-01T00:00:00.500000000Z"},
		{`SELECT updated_at FROM issues WHERE id = 'seed-t1'`, "2026-01-03T05:30:00.000000000Z"},
		{`SELECT created_at FROM comments WHERE id = 'seed-c1'`, "2026-01-03T00:00:00.123456789Z"},
		{`SELECT to_value FROM issue_event_changes WHERE event_id = 'seed-e1' AND field = 'closed_at'`, "2026-01-04T00:00:00.250000000Z"},
	} {
		var got string
		if err := st.db.QueryRowContext(ctx, c.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.query, got, c.want)
		}
	}
}

// A value that is no timestamp is not rewritten, and the CHECK that follows
// refuses it by name, so the migration fails instead of storing it.
func TestTimestampsMigrationRefusesAValueThatIsNoTimestamp(t *testing.T) {
	ctx := context.Background()
	st, up := v11Seeded(t)
	if _, err := st.db.ExecContext(ctx, `UPDATE comments SET created_at = 'yesterday' WHERE id = 'seed-c1'`); err != nil {
		t.Fatal(err)
	}
	err := up()
	if err == nil {
		t.Fatal("Up to v12 accepted a comment whose created_at is 'yesterday'")
	}
	if !strings.Contains(err.Error(), "comments_created_at_stamp") {
		t.Errorf("error does not name the refusing constraint: %v", err)
	}
}

// The CHECKs are the guard against any writer that spells an instant the old
// way, an older release included: every timestamp column refuses it.
func TestEveryTimestampColumnRefusesTheTrimmedSpelling(t *testing.T) {
	ctx := context.Background()
	st, _ := storeAtVersion(t, migrations.FS, timestampsMigrationVersion)
	applySeed(t, st.db, seedsByVersion(t, registryMigrations(t))[timestampsMigrationVersion])
	for _, c := range []struct{ table, column, where string }{
		{"issues", "created_at", "id = 'seed-t1'"},
		{"issues", "updated_at", "id = 'seed-t1'"},
		{"issues", "closed_at", "id = 'seed-t2'"},
		{"issues", "archived_at", "id = 'seed-t3'"},
		{"issues", "deleted_at", "id = 'seed-t3'"},
		{"comments", "created_at", "id = 'seed-c1'"},
		{"issue_events", "created_at", "id = 'seed-e1'"},
		{"labels", "created_at", "label = 'seeded'"},
		{"relations", "created_at", "type = 'blocked-by'"},
		{"plan_reviews", "created_at", "id = 'seed-r1'"},
		{"purges", "issue_created_at", "issue_id = 'seed-gone'"},
		{"purges", "purged_at", "issue_id = 'seed-gone'"},
		{"issue_event_changes", "from_value", "event_id = 'seed-e2' AND field = 'deleted_at'"},
		{"issue_event_changes", "to_value", "event_id = 'seed-e1' AND field = 'closed_at'"},
	} {
		stmt := "UPDATE " + c.table + " SET " + c.column + " = '2026-01-01T00:00:00.5Z' WHERE " + c.where
		if _, err := st.db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s.%s accepted the trimmed spelling: %s", c.table, c.column, stmt)
		}
	}
}
