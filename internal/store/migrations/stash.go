package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// StashColumn names one string column a step stashes before it destroys it:
// its table, its name, and the table's primary key in key order.
//
// [LAW:one-source-of-truth] StashSQL and RestoreSQL are the only definitions
// of the SQL that writes and applies the carrier; every registry statement
// that touches schema_stash is one of their outputs.
type StashColumn struct {
	Table  string
	Column string
	Key    []string
}

// rowKey is the entry's row_key over the table aliased as alias. A one-column
// key is that column's text, with no JSON formatting a Dolt upgrade could
// change between the stash and the restore; a composite key is the parts
// joined by a unit separator, which no key column holds.
func (c StashColumn) rowKey(alias string) string {
	if len(c.Key) == 1 {
		return fmt.Sprintf("CAST(%s.%s AS CHAR)", alias, c.Key[0])
	}
	parts := make([]string, len(c.Key))
	for i, k := range c.Key {
		parts[i] = fmt.Sprintf("CAST(%s.%s AS CHAR)", alias, k)
	}
	return fmt.Sprintf("CONCAT_WS(CHAR(31), %s)", strings.Join(parts, ", "))
}

// StashSQL is the statements that copy c into the carrier, the
// carrier's DDL first. SUBSTRING(c, 1) is load-bearing: Dolt's JSON_ARRAY over
// a TEXT column encodes the column's storage address instead of its text.
func StashSQL(c StashColumn) []string {
	return []string{
		strings.TrimSpace(StashCarrierDDL),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', JSON_ARRAY(SUBSTRING(t.%s, 1)) FROM %s t;",
			c.Table, c.rowKey("t"), c.Column, c.Column, c.Table),
	}
}

// RestoreSQL is the statements that write c's entries back onto the
// rows that still exist, then delete exactly the entries they wrote. An entry
// that matched no row stays: from SQL, a row deleted below is indistinguishable
// from a key spelled differently, and only the second is a loss.
func RestoreSQL(c StashColumn) []string {
	match := fmt.Sprintf("s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s", c.Table, c.Column, c.rowKey("t"))
	return []string{
		strings.TrimSpace(StashCarrierDDL),
		// Single-table: Dolt refuses the joined form, UPDATE issues JOIN
		// schema_stash, with issues_status_check on a valid row.
		fmt.Sprintf("UPDATE %s t SET t.%s = (SELECT IF(JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) = 'NULL', NULL, JSON_UNQUOTE(JSON_EXTRACT(s.value, '$[0]'))) FROM schema_stash s WHERE %s) WHERE EXISTS (SELECT 1 FROM schema_stash s WHERE %s);",
			c.Table, c.Column, match, match),
		fmt.Sprintf("DELETE s FROM schema_stash s JOIN %s t ON %s WHERE s.table_name = '%s' AND s.column_name = '%s';",
			c.Table, match, c.Table, c.Column),
	}
}

// Restores is each version's stash restore: what that version's Up re-adds
// and its released Down stashed. It lives here, outside every pinned Up byte,
// so a released Down is made lossless without touching its Up.
var Restores = map[int64][]StashColumn{
	2: {{Table: "issues", Column: "lane", Key: []string{"id"}}},
}

// Execer is the one method ReplayStash needs from a database handle.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ReplayStash applies version's restore to db and returns the rows each
// column's restore wrote, in Restores order. It is the one restorer: the
// runner calls it after every forward step and the round-trip gate steps
// through it the same way. [LAW:single-enforcer]
func ReplayStash(ctx context.Context, db Execer, version int64) ([]int64, error) {
	var written []int64
	for _, c := range Restores[version] {
		stmts := RestoreSQL(c)
		var n int64
		for i, stmt := range stmts {
			res, err := db.ExecContext(ctx, stmt)
			if err != nil {
				return written, fmt.Errorf("restore v%d %s.%s: %w", version, c.Table, c.Column, err)
			}
			if i == 1 {
				if n, err = res.RowsAffected(); err != nil {
					return written, fmt.Errorf("restore v%d %s.%s: %w", version, c.Table, c.Column, err)
				}
			}
		}
		written = append(written, n)
	}
	return written, nil
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
