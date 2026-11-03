package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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
	// again: every one but the last writes rows it counts, and the last
	// deletes every entry the stash holds, so a replay leaves none behind. It
	// never creates the carrier: a store no Down stashed in has none, and
	// holds nothing to replay. version is the stash's, at the version the store
	// is at. A late replay runs after an Up that did
	// not replay, onto data written since.
	RestoreSQL(version, at int64, late bool) []string
	// Name is the stash's table and column, as the replay reports it.
	Name() string
	// Entry is the table_name and column_name of every entry it writes.
	Entry() (table, column string)
}

// Column is one string column of an existing table: its table, its name, the
// table's primary key in key order, the slice's tag, the rows it stashes
// (Where over the table aliased t, empty for every row), Left, true of a row
// (aliased t) whose column still holds what the Down or the Up wrote there,
// Kept, a column the Down leaves in place, and When, true of a row (t) and
// its entry (s) the stashed value is legal on. An entry's column_name is the
// column and the tag, so two versions' Downs that stash one column never
// overwrite each other's entries.
//
// The replay that runs right after the Up writes a dropped column back
// whole: the stash, taken at the Down, is the record of it. A late replay,
// and every replay of a Kept column, which the store below writes, writes
// onto a row only where Left holds: a value written since is newer than the
// stash and stays.
type Column struct {
	Table  string
	Column string
	Key    []string
	Slice  string
	Where  string
	Left   string
	Kept   bool
	When   string
}

func (c Column) entry() string {
	if c.Slice == "" {
		return c.Column
	}
	return c.Column + "@" + c.Slice
}

// Rows is the rows of a table a Down drops or rewrites, each entry carrying
// the row as an array: every column, the primary key, the stashed slice (Where
// over the table aliased t, empty for every row), the slice's tag, and the
// columns that reference issues(id), whose rows die with their issue. An
// entry's column_name is '*' and the tag, so two slices of one table, stashed
// by two versions' Downs, never replay each other's rows.
//
// Prune, when set, picks the rows (over the table by its name) that the Up
// re-derived from what the Down left: the replay deletes each one the stash
// does not hold byte for byte before it puts the stashed rows back. A live
// row under a stashed key is the Up's re-creation of it, and takes the
// stashed values.
type Rows struct {
	Table   string
	Columns []string
	Key     []string
	Where   string
	Slice   string
	Refs    []string
	Prune   string
}

func (r Rows) entry() string { return "*" + r.Slice }

var relationColumns = []string{"src_id", "dst_id", "type", "created_at", "created_by"}

var relationKey = []string{"src_id", "dst_id", "type"}

// parentEdges is the parent-child edges of the tickets where picks (over
// issues aliased i), which a Down rewrites. The replay makes each stashed
// ticket's parent edges exactly the stashed ones: the Up re-derives them from
// what the Down left, and only the stash knows what they were.
func parentEdges(where string) Rows {
	r := Rows{
		Table:   "relations",
		Columns: relationColumns,
		Key:     relationKey,
		Where:   fmt.Sprintf("t.type = 'parent-child' AND t.src_id IN (SELECT i.id FROM issues i WHERE %s)", where),
		Slice:   "parent-child",
		Refs:    []string{"src_id", "dst_id"},
	}
	r.Prune = fmt.Sprintf("relations.type = 'parent-child' AND relations.src_id IN (SELECT %s FROM schema_stash s WHERE s.table_name = 'relations' AND s.column_name = '%s')", element(0), r.entry())
	return r
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

func (c Column) Name() string { return c.Table + "." + c.entry() }

func (c Column) Entry() (string, string) { return c.Table, c.entry() }

func (c Column) StashSQL() []string {
	where := ""
	if c.Where != "" {
		where = " WHERE " + c.Where
	}
	return []string{
		carrier(),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', %s FROM %s t%s;",
			c.Table, rowKey(c.Key, "t"), c.entry(), value([]string{c.Column}, "t"), c.Table, where),
	}
}

// RestoreSQL writes c's entries onto the rows that still exist, guarded as
// the type says, then deletes every entry: one whose row was deleted below,
// or holds a newer value, has nothing left to restore.
func (c Column) RestoreSQL(_, _ int64, late bool) []string {
	match := fmt.Sprintf("s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s", c.Table, c.entry(), rowKey(c.Key, "t"))
	guard := ""
	if late || c.Kept {
		guard = "(" + c.Left + ") AND "
	}
	legal := match
	if c.When != "" {
		legal += " AND (" + c.When + ")"
	}
	return []string{
		// Single-table: Dolt refuses the joined form, UPDATE issues JOIN
		// schema_stash, with issues_status_check on a valid row.
		fmt.Sprintf("UPDATE %s t SET t.%s = (SELECT %s FROM schema_stash s WHERE %s) WHERE %sEXISTS (SELECT 1 FROM schema_stash s WHERE %s);",
			c.Table, c.Column, element(0), match, guard, legal),
		dropEntries(c.Table, c.entry()),
	}
}

func dropEntries(table, column string) string {
	return fmt.Sprintf("DELETE FROM schema_stash WHERE table_name = '%s' AND column_name = '%s';", table, column)
}

func (r Rows) Name() string { return r.Table + "." + r.entry() }

func (r Rows) Entry() (string, string) { return r.Table, r.entry() }

func (r Rows) StashSQL() []string {
	where := ""
	if r.Where != "" {
		where = " WHERE " + r.Where
	}
	return []string{
		carrier(),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', %s FROM %s t%s;",
			r.Table, rowKey(r.Key, "t"), r.entry(), value(r.Columns, "t"), r.Table, where),
	}
}

// RestoreSQL gives each live row under a stashed key the stashed values,
// inserts each stashed row the table does not hold, then deletes every entry.
// A row whose issue was deleted below died with it, as the table's cascade
// would have done. A late replay only inserts: a live row, and one the Up
// derived, may carry a write made since. It inserts each row as the Ups up
// to at rewrote it.
func (r Rows) RestoreSQL(version, at int64, late bool) []string {
	elems := make([]string, len(r.Columns))
	for i, col := range r.Columns {
		elems[i] = rewritten(r.Table, col, element(i), version, at)
	}
	held := make([]string, len(r.Key))
	for i, k := range r.Key {
		held[i] = fmt.Sprintf("t.%s = %s", k, elems[slices.Index(r.Columns, k)])
	}
	match := fmt.Sprintf("s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s", r.Table, r.entry(), rowKey(r.Key, "t"))
	var sets []string
	for i, col := range r.Columns {
		if !slices.Contains(r.Key, col) {
			sets = append(sets, fmt.Sprintf("t.%s = (SELECT %s FROM schema_stash s WHERE %s)", col, elems[i], match))
		}
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM schema_stash s WHERE s.table_name = '%s' AND s.column_name = '%s' AND NOT EXISTS (SELECT 1 FROM %s t WHERE %s)",
		r.Table, strings.Join(r.Columns, ", "), strings.Join(elems, ", "), r.Table, r.entry(), r.Table, strings.Join(held, " AND "))
	for _, ref := range r.Refs {
		insert += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM issues p WHERE p.id = %s)", element(slices.Index(r.Columns, ref)))
	}
	var stmts []string
	if late {
		sets = nil
	} else if r.Prune != "" {
		stmts = append(stmts, fmt.Sprintf("DELETE FROM %s WHERE %s AND NOT EXISTS (SELECT 1 FROM schema_stash s WHERE s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s AND s.value = %s);",
			r.Table, r.Prune, r.Table, r.entry(), rowKey(r.Key, r.Table), value(r.Columns, r.Table)))
	}
	if len(sets) > 0 {
		stmts = append(stmts, fmt.Sprintf("UPDATE %s t SET %s WHERE EXISTS (SELECT 1 FROM schema_stash s WHERE %s);",
			r.Table, strings.Join(sets, ", "), match))
	}
	return append(stmts, insert+";", dropEntries(r.Table, r.entry()))
}

// laterRewrites is each value a released Up rewrites in place, by the
// version that rewrites it. A late replay carries a row it stashed below that
// version through the rewrite, as the Up would have.
var laterRewrites = []struct {
	version       int64
	table, column string
	from, to      string
}{
	{8, "relations", "type", "blocks", "blocked-by"},
}

// rewritten is expr, the stashed value of table.column, as the Ups above
// version and up to at leave it.
func rewritten(table, column, expr string, version, at int64) string {
	for _, rw := range laterRewrites {
		if rw.table == table && rw.column == column && rw.version > version && rw.version <= at {
			expr = fmt.Sprintf("IF(%s = '%s', '%s', %s)", expr, rw.from, rw.to, expr)
		}
	}
	return expr
}

// Restores is each version's stash: what that version's Up re-adds and its
// released Down stashed. It lives here, outside every pinned Up byte, so a
// released Down is made lossless without touching its Up.
var Restores = map[int64][]Stash{
	2: {Column{Table: "issues", Column: "lane", Key: []string{"id"}, Left: "t.lane = ''"}},
	// A ticket reopened below holds no resolution.
	3: {Column{Table: "issues", Column: "resolution", Key: []string{"id"}, Left: "t.resolution IS NULL",
		When: "t.closed_at IS NOT NULL OR " + element(0) + " IS NULL"}},
	4: {
		// issues_redirect_target_check: a ticket reopened below holds no
		// redirect.
		Column{Table: "issues", Column: "redirect_target", Key: []string{"id"}, Left: "t.redirect_target IS NULL",
			When: "t.resolution IN ('duplicate','superseded') OR " + element(0) + " IS NULL"},
		Rows{Table: "relations", Columns: relationColumns, Key: relationKey, Where: "t.type = 'related-to'", Slice: "related-to", Refs: []string{"src_id", "dst_id"}},
	},
	5: {
		Column{Table: "issue_events", Column: "stream_id", Key: []string{"id"}, Left: "t.stream_id IS NULL"},
		Column{Table: "issue_events", Column: "workspace_id", Key: []string{"id"}, Left: "t.workspace_id IS NULL"},
	},
	6: {
		Column{Table: "issues", Column: "follows_up", Key: []string{"id"}, Left: "t.follows_up IS NULL"},
		parentEdges("i.follows_up IS NOT NULL"),
	},
	7: unfiledEpic(),
	9: {Rows{Table: "purges", Columns: []string{"issue_id", "issue_created_at", "purged_at", "purged_by"}, Key: []string{"issue_id", "issue_created_at"}}},
	11: {
		Column{Table: "issues", Column: "work_plan", Key: []string{"id"}, Left: "t.work_plan = ''"},
		Rows{Table: "plan_reviews", Columns: []string{"id", "issue_id", "plan_digest", "verdict", "laws", "created_at", "created_by"}, Key: []string{"id"}, Refs: []string{"issue_id"}},
	},
}

// unfiledEpic is v7's stash: the unfiled epic its Up minted, whole, with
// everything that hangs on it, and every lane spelled as its ticket's id,
// which its Down resets. The replay puts the stashed epic back under its own
// id with what hung on it, over the epic the Up minted again under that id.
// An epic the Up minted at another id goes only when every child it holds is
// a stashed ticket, which goes back under its stashed epic: a ticket filed
// under it after the Down keeps it. A stashed ticket's own edge to an
// unfiled epic is pruned, so it does not end with two parents.
func unfiledEpic() []Stash {
	const epic = "issue_type = 'epic' AND topic = 'unfiled' AND title = 'Unfiled tickets'"
	of := func(col string) string {
		return fmt.Sprintf("t.%s IN (SELECT e.id FROM issues e WHERE e.%s)", col, strings.ReplaceAll(epic, " AND ", " AND e."))
	}
	return []Stash{
		Rows{
			Table:   "issues",
			Columns: []string{"id", "title", "description", "agent_prompt", "status", "priority", "issue_type", "topic", "assignee", "item_rank", "lane", "created_at", "updated_at", "closed_at", "archived_at", "deleted_at", "resolution", "redirect_target", "follows_up"},
			Key:     []string{"id"},
			Where:   "t." + strings.ReplaceAll(epic, " AND ", " AND t."),
			Slice:   "unfiled",
			Prune: fmt.Sprintf("issues.%s AND issues.id NOT IN (SELECT s.row_key FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = '*unfiled') AND NOT EXISTS (SELECT 1 FROM relations c WHERE c.dst_id = issues.id AND c.type = 'parent-child' AND c.src_id NOT IN (SELECT %s FROM schema_stash s WHERE s.table_name = 'relations' AND s.column_name = '*unfiled'))",
				strings.ReplaceAll(epic, " AND ", " AND issues."), element(0)),
		},
		Rows{Table: "comments", Columns: []string{"id", "issue_id", "body", "created_at", "created_by"}, Key: []string{"id"}, Where: of("issue_id"), Slice: "unfiled", Refs: []string{"issue_id"}},
		Rows{Table: "labels", Columns: []string{"issue_id", "label", "created_at", "created_by"}, Key: []string{"issue_id", "label"}, Where: of("issue_id"), Slice: "unfiled", Refs: []string{"issue_id"}},
		Rows{Table: "issue_events", Columns: []string{"id", "issue_id", "action", "reason", "actor", "created_at", "stream_id", "workspace_id"}, Key: []string{"id"}, Where: of("issue_id"), Slice: "unfiled", Refs: []string{"issue_id"}},
		Rows{Table: "issue_event_changes", Columns: []string{"event_id", "field", "from_value", "to_value"}, Key: []string{"event_id", "field"}, Where: fmt.Sprintf("t.event_id IN (SELECT v.id FROM issue_events v WHERE %s)", strings.ReplaceAll(of("issue_id"), "t.issue_id", "v.issue_id")), Slice: "unfiled"},
		Rows{
			Table: "relations", Columns: relationColumns, Key: relationKey,
			Where: of("dst_id"), Slice: "unfiled", Refs: []string{"src_id", "dst_id"},
			Prune: fmt.Sprintf("relations.type = 'parent-child' AND relations.src_id IN (SELECT %s FROM schema_stash s WHERE s.table_name = 'relations' AND s.column_name = '*unfiled') AND relations.dst_id IN (SELECT e.id FROM issues e WHERE e.%s)", element(0), strings.ReplaceAll(epic, " AND ", " AND e.")),
		},
		Rows{Table: "relations", Columns: relationColumns, Key: relationKey, Where: of("src_id"), Slice: "unfiled-src", Refs: []string{"src_id", "dst_id"}},
		// The Down resets a lane spelled as its ticket's id, and the Up spells
		// the lane of every child it leaves at the top level so.
		Column{Table: "issues", Column: "lane", Key: []string{"id"}, Slice: "unfiled", Kept: true,
			Where: fmt.Sprintf("(t.lane = t.id OR t.id IN (SELECT c.src_id FROM relations c WHERE c.type = 'parent-child' AND %s))", strings.ReplaceAll(of("dst_id"), "t.dst_id", "c.dst_id")),
			Left:  "t.lane IN ('', t.id)"},
	}
}

// Querier is what ReplayStash needs from a database handle.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CarrierPresent says whether db holds schema_stash, which only a Down that
// stashed creates.
func CarrierPresent(ctx context.Context, db Querier) (bool, error) {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'schema_stash'`).Scan(&present); err != nil {
		return false, fmt.Errorf("look for the stash carrier: %w", err)
	}
	return present > 0, nil
}

// Replayed is one stash's replay: what it restored and how many rows.
type Replayed struct {
	Version int64
	Name    string
	Rows    int64
}

// ReplayStash applies version's stash to db. It is the one restorer: the
// runner calls it after every forward step, and late for a stash an Up that
// did not replay left, and the round-trip gate steps through it the same
// way. [LAW:single-enforcer]
func ReplayStash(ctx context.Context, db Querier, version, at int64, late bool) ([]Replayed, error) {
	if len(Restores[version]) == 0 {
		return nil, nil
	}
	present, err := CarrierPresent(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("replay v%d stash: %w", version, err)
	}
	if !present {
		return nil, nil
	}
	var replayed []Replayed
	for _, st := range Restores[version] {
		stmts := st.RestoreSQL(version, at, late)
		r := Replayed{Version: version, Name: st.Name()}
		for i, stmt := range stmts {
			res, err := db.ExecContext(ctx, stmt)
			if err != nil {
				return replayed, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
			}
			if i < len(stmts)-1 {
				n, err := res.RowsAffected()
				if err != nil {
					return replayed, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
				}
				r.Rows += n
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
