package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Stash is one thing a released Down stashes before it destroys it, and the
// next Up's replay writes back.
//
// [LAW:one-source-of-truth] StashSQL and RestoreSQL are the only definitions
// of the SQL that writes and applies the carrier; every registry statement
// that touches schema_stash is one of their outputs.
type Stash interface {
	// StashSQL is the statements a Down runs before the drop, the carrier's
	// DDL first.
	StashSQL() []string
	// RestoreSQL is the statements the replay runs once the structure exists
	// again, and which of them writes the rows it counts.
	RestoreSQL() (stmts []string, writes int)
	// Name is the stash's table and column, as the replay reports it.
	Name() string
}

// Column is one string column of an existing table: its table, its name, and
// the table's primary key in key order.
type Column struct {
	Table  string
	Column string
	Key    []string
}

// Rows is a whole table a Down drops: every column, the primary key, and, for
// a table whose rows die with a parent row, the parent table and the column
// referencing it. Its entries carry column_name '*' and the row as an array.
type Rows struct {
	Table     string
	Columns   []string
	Key       []string
	Parent    string
	ParentRef string
}

// rowKey is an entry's row_key over the table aliased as alias. A one-column
// key is that column's text, with no JSON formatting a Dolt upgrade could
// change between the stash and the restore; a composite key is the parts
// joined by a unit separator, which no key column holds.
func rowKey(key []string, alias string) string {
	parts := make([]string, len(key))
	for i, k := range key {
		parts[i] = fmt.Sprintf("CAST(%s.%s AS CHAR)", alias, k)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return fmt.Sprintf("CONCAT_WS(CHAR(31), %s)", strings.Join(parts, ", "))
}

// value is the JSON_ARRAY of cols over alias. SUBSTRING(c, 1) is
// load-bearing: Dolt's JSON_ARRAY over a TEXT column encodes the column's
// storage address instead of its text.
func value(cols []string, alias string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("SUBSTRING(%s.%s, 1)", alias, c)
	}
	return "JSON_ARRAY(" + strings.Join(parts, ", ") + ")"
}

// element is entry s's i-th array element as the column's text, NULL kept
// apart from a missing entry by the array itself.
func element(i int) string {
	at := fmt.Sprintf("JSON_EXTRACT(s.value, '$[%d]')", i)
	return fmt.Sprintf("IF(JSON_TYPE(%s) = 'NULL', NULL, JSON_UNQUOTE(%s))", at, at)
}

func carrier() string { return strings.TrimSpace(StashCarrierDDL) }

func (c Column) Name() string { return c.Table + "." + c.Column }

func (c Column) StashSQL() []string {
	return []string{
		carrier(),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', %s FROM %s t;",
			c.Table, rowKey(c.Key, "t"), c.Column, value([]string{c.Column}, "t"), c.Table),
	}
}

// RestoreSQL writes c's entries onto the rows that still exist, then deletes
// exactly the entries it wrote. An entry that matched no row stays: from SQL,
// a row deleted below is indistinguishable from a key spelled differently,
// and only the second is a loss.
func (c Column) RestoreSQL() ([]string, int) {
	match := fmt.Sprintf("s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s", c.Table, c.Column, rowKey(c.Key, "t"))
	return []string{
		carrier(),
		// Single-table: Dolt refuses the joined form, UPDATE issues JOIN
		// schema_stash, with issues_status_check on a valid row.
		fmt.Sprintf("UPDATE %s t SET t.%s = (SELECT %s FROM schema_stash s WHERE %s) WHERE EXISTS (SELECT 1 FROM schema_stash s WHERE %s);",
			c.Table, c.Column, element(0), match, match),
		fmt.Sprintf("DELETE s FROM schema_stash s JOIN %s t ON %s WHERE s.table_name = '%s' AND s.column_name = '%s';",
			c.Table, match, c.Table, c.Column),
	}, 1
}

func (r Rows) Name() string { return r.Table + ".*" }

func (r Rows) StashSQL() []string {
	return []string{
		carrier(),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '*', %s FROM %s t;",
			r.Table, rowKey(r.Key, "t"), value(r.Columns, "t"), r.Table),
	}
}

// RestoreSQL re-inserts r's rows into the table its Up re-created empty, then
// deletes the entries whose row is back. A row whose parent was deleted below
// died with it, as the table's cascade would have done, and its entry stays.
func (r Rows) RestoreSQL() ([]string, int) {
	elems := make([]string, len(r.Columns))
	for i := range r.Columns {
		elems[i] = element(i)
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM schema_stash s WHERE s.table_name = '%s' AND s.column_name = '*'",
		r.Table, strings.Join(r.Columns, ", "), strings.Join(elems, ", "), r.Table)
	if r.Parent != "" {
		insert += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM %s p WHERE p.id = %s)", r.Parent, element(indexOf(r.Columns, r.ParentRef)))
	}
	return []string{
		carrier(),
		insert + ";",
		fmt.Sprintf("DELETE s FROM schema_stash s JOIN %s t ON s.table_name = '%s' AND s.column_name = '*' AND s.row_key = %s WHERE s.table_name = '%s' AND s.column_name = '*';",
			r.Table, r.Table, rowKey(r.Key, "t"), r.Table),
	}, 1
}

func indexOf(cols []string, c string) int {
	for i, x := range cols {
		if x == c {
			return i
		}
	}
	panic(fmt.Sprintf("migrations: %q is not among %v", c, cols))
}

// Restores is each version's stash: what that version's Up re-adds and its
// released Down stashed. It lives here, outside every pinned Up byte, so a
// released Down is made lossless without touching its Up.
var Restores = map[int64][]Stash{
	2: {Column{Table: "issues", Column: "lane", Key: []string{"id"}}},
	3: {Column{Table: "issues", Column: "resolution", Key: []string{"id"}}},
	5: {
		Column{Table: "issue_events", Column: "stream_id", Key: []string{"id"}},
		Column{Table: "issue_events", Column: "workspace_id", Key: []string{"id"}},
	},
	9: {Rows{Table: "purges", Columns: []string{"issue_id", "issue_created_at", "purged_at", "purged_by"}, Key: []string{"issue_id", "issue_created_at"}}},
	11: {
		Column{Table: "issues", Column: "work_plan", Key: []string{"id"}},
		Rows{Table: "plan_reviews", Columns: []string{"id", "issue_id", "plan_digest", "verdict", "laws", "created_at", "created_by"}, Key: []string{"id"}, Parent: "issues", ParentRef: "issue_id"},
	},
}

// Execer is the one method ReplayStash needs from a database handle.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Replayed is one stash's replay: what it restored and how many rows.
type Replayed struct {
	Version int64
	Name    string
	Rows    int64
}

// ReplayStash applies version's stash to db. It is the one restorer: the
// runner calls it after every forward step and the round-trip gate steps
// through it the same way. [LAW:single-enforcer]
func ReplayStash(ctx context.Context, db Execer, version int64) ([]Replayed, error) {
	var replayed []Replayed
	for _, st := range Restores[version] {
		stmts, writes := st.RestoreSQL()
		r := Replayed{Version: version, Name: st.Name()}
		for i, stmt := range stmts {
			res, err := db.ExecContext(ctx, stmt)
			if err != nil {
				return replayed, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
			}
			if i == writes {
				if r.Rows, err = res.RowsAffected(); err != nil {
					return replayed, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
				}
			}
		}
		replayed = append(replayed, r)
	}
	return replayed, nil
}

// GooseStatements renders stmts as goose statement blocks, the form a Down
// section carries them in.
func GooseStatements(stmts []string) string {
	var b strings.Builder
	for _, s := range stmts {
		b.WriteString("-- +goose StatementBegin\n" + s + "\n-- +goose StatementEnd\n")
	}
	return b.String()
}
