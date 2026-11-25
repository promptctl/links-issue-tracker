package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
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

// v12Seeded is a store at v12 holding the v12 seed, every table filled.
func v12Seeded(t *testing.T) *Store {
	t.Helper()
	st, _ := storeAtVersion(t, migrations.FS, timestampsMigrationVersion)
	applySeed(t, st.db, seedsByVersion(t, registryMigrations(t))[timestampsMigrationVersion])
	return st
}

// assertColumnSetRefused fails unless setting the column's timestamps to value
// is refused by the column's own CHECK, named in the error.
func assertColumnSetRefused(t *testing.T, st *Store, c timestampColumn, value string) {
	t.Helper()
	stmt := "UPDATE " + c.table + " SET " + c.column + " = ? WHERE " + c.rows()
	if _, err := st.db.ExecContext(context.Background(), stmt, value); err == nil {
		t.Errorf("%s.%s accepted %q", c.table, c.column, value)
	} else if !strings.Contains(err.Error(), c.checkName()) {
		t.Errorf("%s.%s refused %q, but not by %s: %v", c.table, c.column, value, c.checkName(), err)
	}
}

// The CHECKs are the guard against any writer that spells an instant any way
// but the stored one, an older release included: every timestamp column refuses
// the trimmed spelling, a trailing character, and a date that does not exist.
func TestEveryTimestampColumnRefusesWhatIsNotTheStoredSpelling(t *testing.T) {
	st := v12Seeded(t)
	for _, c := range timestampColumns() {
		for _, value := range []string{
			"2026-01-01T00:00:00.5Z",
			"2026-01-01T00:00:00Z",
			"2026-01-01T00:00:00.000000000+01:00",
			"2026-01-01T00:00:00.000000000Z\n",
			"2026-13-45T25:61:61.000000000Z",
			"2026-02-30T10:00:00.000000000Z",
		} {
			assertColumnSetRefused(t, st, c, value)
		}
	}
}

// The list of columns the tests and the probe walk is read off the schema, not
// trusted: every *_at column of the store's data is one of them, and each
// carries the CHECK its name promises. A migration that adds a timestamp column
// without its CHECK fails here, naming the column. migration_quarantine is
// lit's bookkeeping, created outside goose, not the store's data.
func TestEveryTimestampColumnOfTheStoreIsGuardedByItsCheck(t *testing.T) {
	ctx := context.Background()
	st := v12Seeded(t)
	listed := map[string]bool{}
	for _, c := range timestampColumns() {
		// A change row's value is a timestamp only for some fields; it is no
		// *_at column, and its CHECK is looked up below.
		if c.scope == "" {
			listed[c.table+"."+c.column] = true
		}
	}
	rows, err := st.db.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = DATABASE() AND column_name LIKE '%\_at' AND table_name <> 'migration_quarantine'`)
	if err != nil {
		t.Fatalf("read the schema's timestamp columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		if !listed[table+"."+column] {
			t.Errorf("%s.%s is a timestamp column no CHECK guards: add it to timestampColumns and give it a %s_%s_stamp CHECK", table, column, table, column)
		}
		delete(listed, table+"."+column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for column := range listed {
		t.Errorf("timestampColumns lists %s, which the schema does not have", column)
	}
	for _, c := range timestampColumns() {
		var name string
		if err := st.db.QueryRowContext(ctx, `SELECT constraint_name FROM information_schema.table_constraints
			WHERE table_schema = DATABASE() AND table_name = ? AND constraint_name = ? AND constraint_type = 'CHECK'`,
			c.table, c.checkName()).Scan(&name); err != nil {
			t.Errorf("%s.%s has no CHECK named %s: %v", c.table, c.column, c.checkName(), err)
		}
	}
}

// Whatever StampText writes, at the edges of its range, every CHECK admits:
// the Go layout and the SQL shape are two spellings of one fact, and this is
// what holds them together.
func TestEveryTimestampColumnAdmitsWhatStampTextWrites(t *testing.T) {
	ctx := context.Background()
	st := v12Seeded(t)
	for _, instant := range []time.Time{
		time.Date(2026, time.November, 24, 10, 0, 7, 0, time.UTC),
		time.Date(2026, time.November, 24, 10, 0, 7, 1, time.UTC),
		time.Date(2026, time.November, 24, 10, 0, 7, 999999999, time.UTC),
		time.Date(2024, time.February, 29, 23, 59, 59, 999999999, time.UTC),
		time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC),
	} {
		for _, c := range timestampColumns() {
			stmt := "UPDATE " + c.table + " SET " + c.column + " = ? WHERE " + c.rows()
			if _, err := st.db.ExecContext(ctx, stmt, model.StampText(instant)); err != nil {
				t.Errorf("%s.%s refused StampText(%s) = %q: %v", c.table, c.column, instant, model.StampText(instant), err)
			}
		}
	}
}

// A value the migration cannot respell faithfully is left as it was and refused
// by the CHECK that follows, so the migration fails loudly: an offset RFC3339
// does not allow, a trailing newline, a calendar day that does not exist, and
// an instant that leaves the year range in UTC. None becomes NULL or a
// different instant, whichever column holds it.
func TestTimestampsMigrationRefusesWhatItCannotRespellFaithfully(t *testing.T) {
	for _, c := range []struct{ name, stmt, constraint string }{
		{"an offset of 25 hours", `UPDATE comments SET created_at = '2026-01-01T10:00:00+25:00' WHERE id = 'seed-c1'`, "comments_created_at_stamp"},
		{"an offset of 99 minutes", `UPDATE comments SET created_at = '2026-01-01T10:00:00+05:99' WHERE id = 'seed-c1'`, "comments_created_at_stamp"},
		{"a trailing newline", "UPDATE comments SET created_at = CONCAT('2026-01-01T10:00:00Z', CHAR(10)) WHERE id = 'seed-c1'", "comments_created_at_stamp"},
		{"a day that does not exist, in a column that may be NULL", `UPDATE issues SET closed_at = '2026-02-30T10:00:00+01:00' WHERE id = 'seed-t2'`, "issues_closed_at_stamp"},
		{"a day that does not exist, spelled the stored way", `UPDATE issues SET closed_at = '2026-02-30T10:00:00.000000000Z' WHERE id = 'seed-t2'`, "issues_closed_at_stamp"},
		// The engine itself refuses the conversion, so no constraint is named.
		{"an instant past year 9999 in UTC, in a column that may be NULL", `UPDATE issues SET closed_at = '9999-12-31T23:30:00-01:00' WHERE id = 'seed-t2'`, "outside of datetime"},
		{"a value in a change row of a timestamp field", `UPDATE issue_event_changes SET to_value = '2026-01-04T05:00:00+25:00' WHERE event_id = 'seed-e1' AND field = 'closed_at'`, "issue_event_changes_to_value_stamp"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, up := v11Seeded(t)
			if _, err := st.db.ExecContext(context.Background(), c.stmt); err != nil {
				t.Fatal(err)
			}
			err := up()
			if err == nil {
				t.Fatalf("Up to v12 accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.constraint) {
				t.Errorf("error does not say %q: %v", c.constraint, err)
			}
		})
	}
}

// A change row's absent value is NULL and only NULL: an empty one, which no
// writer of this store produced but an older import could have, becomes NULL,
// and a text field's empty value is left alone.
func TestTimestampsMigrationSpellsAbsenceAsNull(t *testing.T) {
	ctx := context.Background()
	st, up := v11Seeded(t)
	for _, stmt := range []string{
		`UPDATE issue_event_changes SET from_value = '' WHERE event_id = 'seed-e2' AND field = 'archived_at'`,
		`UPDATE issue_event_changes SET from_value = '' WHERE event_id = 'seed-e1' AND field = 'status'`,
	} {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := up(); err != nil {
		t.Fatalf("Up to v%d: %v", timestampsMigrationVersion, err)
	}
	var timestampFrom sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT from_value FROM issue_event_changes WHERE event_id = 'seed-e2' AND field = 'archived_at'`).Scan(&timestampFrom); err != nil {
		t.Fatal(err)
	}
	if timestampFrom.Valid {
		t.Errorf("a timestamp change's empty from_value became %q, want NULL", timestampFrom.String)
	}
	var textFrom sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT from_value FROM issue_event_changes WHERE event_id = 'seed-e1' AND field = 'status'`).Scan(&textFrom); err != nil {
		t.Fatal(err)
	}
	if !textFrom.Valid || textFrom.String != "" {
		t.Errorf("a text change's empty from_value became %v, want it untouched", textFrom)
	}
}

// The open that runs v12 says what it did: how many timestamps it respells and
// how many of those lose an offset to UTC. Each is read before the migration
// runs, from the spellings the store holds then.
func TestTimestampsMigrationProbeCountsWhatItRespellsAndWhatItLoses(t *testing.T) {
	ctx := context.Background()
	st, _ := v11Seeded(t)
	before, err := probeTimestampSpelling(ctx, st.db)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	for _, stmt := range []string{
		// already stored-spelled: one fewer to respell
		`UPDATE comments SET created_at = '2026-01-03T00:00:00.100000000Z' WHERE id = 'seed-c1'`,
		// an offset spelling is still a respelling, and the one that loses information
		`UPDATE labels SET created_at = '2026-01-03T02:00:00.5+02:00' WHERE label = 'seeded'`,
	} {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	after, err := probeTimestampSpelling(ctx, st.db)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got, want := after["migration.v12_stamps_respelled"], before["migration.v12_stamps_respelled"].(int)-1; got != want {
		t.Errorf("stamps respelled = %v, want %v", got, want)
	}
	if got, want := after["migration.v12_offsets_to_utc"], before["migration.v12_offsets_to_utc"].(int)+1; got != want {
		t.Errorf("offsets to UTC = %v, want %v", got, want)
	}
}
