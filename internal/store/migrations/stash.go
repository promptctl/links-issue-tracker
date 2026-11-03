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
// [LAW:one-source-of-truth] StashSQL, ImageSQL, SettleSQL and RestoreSQL are
// the only definitions of the SQL that writes and applies the carrier; every
// registry statement that touches schema_stash is one of their outputs.
type Stash interface {
	// StashSQL is the statements a Down runs before the drop, the carrier's
	// DDL first. It replaces whatever the stash held: entries a raise that
	// never replayed left behind describe a store this Down is stepping away
	// from, and an image of them would vouch for rows nobody stashed.
	StashSQL() []string
	// ImageSQL is the statements a Down runs last, once the store is at the
	// version below: an image of each unit the stash's entries write onto,
	// as the Down left it.
	ImageSQL() []string
	// SettleSQL is the statements the forward step runs before its Up: they
	// delete every entry whose unit no longer matches its image, with the
	// image, and the last one's count is the units settled. The store below
	// wrote that unit, and the Up derives it from that write.
	SettleSQL() []string
	// RestoreSQL is the statements the replay runs once the structure exists
	// again, each writing rows it counts. It never creates the carrier: a
	// store no Down stashed in has none, and holds nothing to replay.
	RestoreSQL() []string
	// Name is the stash's table and column, as the replay reports it.
	Name() string
	// Entry is the table_name and column_name of every entry it writes.
	Entry() (table, column string)
}

// Unit is what one entry's restore writes onto: its key over the entry
// (aliased s), and its image at the version below over a key expression.
// The image holds only what a write below can change that the restore would
// overwrite, so a write below that leaves it alone keeps the stash.
type Unit struct {
	key   string
	image func(key string) string
}

// row is cols of the row of table under the entry's own key, NULL when a
// write below deleted it: the columns the restore overwrites, or that what it
// writes is legal on only as they were.
func row(table string, key, cols []string) Unit {
	return Unit{key: "s.row_key", image: func(k string) string {
		return fmt.Sprintf("(SELECT %s FROM %s u WHERE %s = %s)", value(cols, "u"), table, rowKey(key, "u"), k)
	}}
}

// ticket is the issues row under the entry's key, as cols of it.
func ticket(cols ...string) Unit { return row("issues", []string{"id"}, cols) }

// parents is a ticket's parent edges, whole, the ticket being key: filing,
// reparenting, or re-creating an edge below changes them.
func parents(key string) Unit {
	return Unit{key: key, image: func(k string) string {
		return fmt.Sprintf("(SELECT GROUP_CONCAT(%s ORDER BY u.dst_id SEPARATOR ',') FROM relations u WHERE u.src_id = %s AND u.type = 'parent-child')", rowKey(relationColumns, "u"), k)
	}}
}

// undisputed is a row of a table the version below does not have, so no
// write below reaches it.
func undisputed() Unit { return Unit{key: "s.row_key", image: func(string) string { return "NULL" }} }

func image(entry string) string { return entry + "#image" }

// imageSQL records, for each entry of table's entry, its unit's image.
func imageSQL(table, entry string, u Unit) []string {
	return []string{fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', JSON_ARRAY(%s) FROM schema_stash s WHERE s.table_name = '%s' AND s.column_name = '%s';",
		table, u.key, image(entry), u.image(u.key), table, entry)}
}

// settleSQL deletes each entry whose unit's image is not what the Down
// recorded, then those images; the second statement counts the units.
func settleSQL(table, entry string, u Unit) []string {
	stale := func(alias string) string {
		return fmt.Sprintf("CAST(%[1]s.value AS CHAR) <> CAST(JSON_ARRAY(%[2]s) AS CHAR)", alias, u.image(alias+".row_key"))
	}
	return []string{
		fmt.Sprintf("DELETE FROM schema_stash WHERE table_name = '%[1]s' AND column_name = '%[2]s' AND row_key IN (SELECT s.row_key FROM schema_stash s JOIN schema_stash i ON i.table_name = '%[1]s' AND i.column_name = '%[3]s' AND i.row_key = %[4]s WHERE s.table_name = '%[1]s' AND s.column_name = '%[2]s' AND %[5]s);",
			table, entry, image(entry), u.key, stale("i")),
		fmt.Sprintf("DELETE FROM schema_stash WHERE table_name = '%s' AND column_name = '%s' AND %s;", table, image(entry), stale("schema_stash")),
	}
}

// Column is one string column of an existing table: its table, its name, the
// table's primary key in key order, the slice's tag, the rows it stashes
// (Where over the table aliased t, empty for every row), and the unit each
// entry writes onto. An entry's column_name is the column and the tag, so two
// versions' Downs that stash one column never overwrite each other's entries.
type Column struct {
	Table  string
	Column string
	Key    []string
	Slice  string
	Where  string
	Unit   Unit
}

func (c Column) entry() string {
	if c.Slice == "" {
		return c.Column
	}
	return c.Column + "@" + c.Slice
}

// Rows is the rows of a table a Down drops or rewrites, each entry carrying
// the row as an array: every column, the primary key, the stashed slice (Where
// over the table aliased t, empty for every row), the slice's tag, the
// columns that reference issues(id), whose rows die with their issue, and the
// unit each entry writes onto. An entry's column_name is '*' and the tag, so
// two slices of one table, stashed by two versions' Downs, never replay each
// other's rows.
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
	Unit    Unit
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
		Unit:    parents(element(0)),
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
		carrier(), dropEntries(c.Table, c.entry()), dropEntries(c.Table, image(c.entry())),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', %s FROM %s t%s;",
			c.Table, rowKey(c.Key, "t"), c.entry(), value([]string{c.Column}, "t"), c.Table, where),
	}
}

func (c Column) ImageSQL() []string { return imageSQL(c.Table, c.entry(), c.Unit) }

func (c Column) SettleSQL() []string { return settleSQL(c.Table, c.entry(), c.Unit) }

// RestoreSQL writes c's entries onto the rows that still exist. One whose row
// was deleted below has nothing left to restore.
func (c Column) RestoreSQL() []string {
	match := fmt.Sprintf("s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s", c.Table, c.entry(), rowKey(c.Key, "t"))
	return []string{
		// Single-table: Dolt refuses the joined form, UPDATE issues JOIN
		// schema_stash, with issues_status_check on a valid row.
		fmt.Sprintf("UPDATE %s t SET t.%s = (SELECT %s FROM schema_stash s WHERE %s) WHERE EXISTS (SELECT 1 FROM schema_stash s WHERE %s);",
			c.Table, c.Column, element(0), match, match),
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
		carrier(), dropEntries(r.Table, r.entry()), dropEntries(r.Table, image(r.entry())),
		fmt.Sprintf("REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '%s', %s, '%s', %s FROM %s t%s;",
			r.Table, rowKey(r.Key, "t"), r.entry(), value(r.Columns, "t"), r.Table, where),
	}
}

func (r Rows) ImageSQL() []string { return imageSQL(r.Table, r.entry(), r.Unit) }

func (r Rows) SettleSQL() []string { return settleSQL(r.Table, r.entry(), r.Unit) }

// RestoreSQL gives each live row under a stashed key the stashed values and
// inserts each stashed row the table does not hold. A row whose issue was
// deleted below died with it, as the table's cascade would have done.
func (r Rows) RestoreSQL() []string {
	elems := make([]string, len(r.Columns))
	for i := range r.Columns {
		elems[i] = element(i)
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
	if r.Prune != "" {
		stmts = append(stmts, fmt.Sprintf("DELETE FROM %s WHERE %s AND NOT EXISTS (SELECT 1 FROM schema_stash s WHERE s.table_name = '%s' AND s.column_name = '%s' AND s.row_key = %s AND s.value = %s);",
			r.Table, r.Prune, r.Table, r.entry(), rowKey(r.Key, r.Table), value(r.Columns, r.Table)))
	}
	if len(sets) > 0 {
		stmts = append(stmts, fmt.Sprintf("UPDATE %s t SET %s WHERE EXISTS (SELECT 1 FROM schema_stash s WHERE %s);",
			r.Table, strings.Join(sets, ", "), match))
	}
	return append(stmts, insert+";")
}

// Restores is each version's stash: what that version's Up re-adds and its
// released Down stashed. It lives here, outside every pinned Up byte, so a
// released Down is made lossless without touching its Up.
var Restores = map[int64][]Stash{
	2: {Column{Table: "issues", Column: "lane", Key: []string{"id"}, Unit: ticket("id")}},
	// A resolution is legal on a ticket's closure only as it was, and a
	// redirect on its resolution too: issues_redirect_target_check.
	3: {Column{Table: "issues", Column: "resolution", Key: []string{"id"}, Unit: ticket("status", "closed_at")}},
	4: {
		Column{Table: "issues", Column: "redirect_target", Key: []string{"id"}, Unit: ticket("status", "closed_at", "resolution")},
		Rows{Table: "relations", Columns: relationColumns, Key: relationKey, Where: "t.type = 'related-to'", Slice: "related-to", Refs: []string{"src_id", "dst_id"}, Unit: row("relations", relationKey, relationColumns)},
	},
	5: {
		Column{Table: "issue_events", Column: "stream_id", Key: []string{"id"}, Unit: row("issue_events", []string{"id"}, []string{"id"})},
		Column{Table: "issue_events", Column: "workspace_id", Key: []string{"id"}, Unit: row("issue_events", []string{"id"}, []string{"id"})},
	},
	6: {
		Column{Table: "issues", Column: "follows_up", Key: []string{"id"}, Unit: parents("s.row_key")},
		parentEdges("i.follows_up IS NOT NULL"),
	},
	7: unfiledEpic(),
	9: {Rows{Table: "purges", Columns: []string{"issue_id", "issue_created_at", "purged_at", "purged_by"}, Key: []string{"issue_id", "issue_created_at"}, Unit: undisputed()}},
	11: {
		Column{Table: "issues", Column: "work_plan", Key: []string{"id"}, Unit: ticket("id")},
		Rows{Table: "plan_reviews", Columns: []string{"id", "issue_id", "plan_digest", "verdict", "laws", "created_at", "created_by"}, Key: []string{"id"}, Refs: []string{"issue_id"}, Unit: undisputed()},
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
			Unit: row("issues", []string{"id"}, []string{"id", "title", "description", "agent_prompt", "status", "priority", "issue_type", "topic", "assignee", "item_rank", "lane", "created_at", "updated_at", "closed_at", "archived_at", "deleted_at", "resolution", "redirect_target", "follows_up"}),
		},
		Rows{Table: "comments", Columns: []string{"id", "issue_id", "body", "created_at", "created_by"}, Key: []string{"id"}, Where: of("issue_id"), Slice: "unfiled", Refs: []string{"issue_id"}, Unit: row("comments", []string{"id"}, []string{"id", "issue_id", "body", "created_at", "created_by"})},
		Rows{Table: "labels", Columns: []string{"issue_id", "label", "created_at", "created_by"}, Key: []string{"issue_id", "label"}, Where: of("issue_id"), Slice: "unfiled", Refs: []string{"issue_id"}, Unit: row("labels", []string{"issue_id", "label"}, []string{"issue_id", "label", "created_at", "created_by"})},
		Rows{Table: "issue_events", Columns: []string{"id", "issue_id", "action", "reason", "actor", "created_at", "stream_id", "workspace_id"}, Key: []string{"id"}, Where: of("issue_id"), Slice: "unfiled", Refs: []string{"issue_id"}, Unit: row("issue_events", []string{"id"}, []string{"id", "issue_id", "action", "reason", "actor", "created_at", "stream_id", "workspace_id"})},
		Rows{Table: "issue_event_changes", Columns: []string{"event_id", "field", "from_value", "to_value"}, Key: []string{"event_id", "field"}, Where: fmt.Sprintf("t.event_id IN (SELECT v.id FROM issue_events v WHERE %s)", strings.ReplaceAll(of("issue_id"), "t.issue_id", "v.issue_id")), Slice: "unfiled", Unit: row("issue_event_changes", []string{"event_id", "field"}, []string{"event_id", "field", "from_value", "to_value"})},
		Rows{
			Table: "relations", Columns: relationColumns, Key: relationKey,
			Where: of("dst_id"), Slice: "unfiled", Refs: []string{"src_id", "dst_id"},
			Prune: fmt.Sprintf("relations.type = 'parent-child' AND relations.src_id IN (SELECT %s FROM schema_stash s WHERE s.table_name = 'relations' AND s.column_name = '*unfiled') AND relations.dst_id IN (SELECT e.id FROM issues e WHERE e.%s)", element(0), strings.ReplaceAll(epic, " AND ", " AND e.")),
			Unit:  parents(element(0)),
		},
		Rows{Table: "relations", Columns: relationColumns, Key: relationKey, Where: of("src_id"), Slice: "unfiled-src", Refs: []string{"src_id", "dst_id"}, Unit: row("relations", relationKey, relationColumns)},
		// The Down resets a lane spelled as its ticket's id, and the Up spells
		// the lane of every child it leaves at the top level so: whatever lane
		// a write below sets, the Up spells it again, so it is not the unit.
		Column{Table: "issues", Column: "lane", Key: []string{"id"}, Slice: "unfiled",
			Where: fmt.Sprintf("(t.lane = t.id OR t.id IN (SELECT c.src_id FROM relations c WHERE c.type = 'parent-child' AND %s))", strings.ReplaceAll(of("dst_id"), "t.dst_id", "c.dst_id")),
			Unit:  parents("s.row_key")},
	}
}

// ImageSQL is the statements a Down at version runs last: every stash's
// image, in Restores order.
func ImageSQL(version int64) []string {
	var stmts []string
	for _, st := range Restores[version] {
		stmts = append(stmts, st.ImageSQL()...)
	}
	return stmts
}

// Querier is what the stash's steps need from a database handle.
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

// StashRows is one stash's count in one step: the units a settle dropped,
// the rows a replay wrote, or the entries it held.
type StashRows struct {
	Version int64
	Name    string
	Rows    int64
}

// Replayed is what one version's replay did: the rows each stash wrote, and
// the entries of each stash it held because no image judges them.
type Replayed struct {
	Restored []StashRows
	Held     []StashRows
}

// SettleStash runs version's settle on db, before its Up. It is the one
// judge of whether an entry still holds: an entry is decided here, at the
// version the Down left, or never. [LAW:single-enforcer]
func SettleStash(ctx context.Context, db Querier, version int64) ([]StashRows, error) {
	present, err := carrierFor(ctx, db, version)
	if err != nil || !present {
		return nil, err
	}
	var settled []StashRows
	for _, st := range Restores[version] {
		r := StashRows{Version: version, Name: st.Name()}
		stmts := st.SettleSQL()
		for i, stmt := range stmts {
			res, err := db.ExecContext(ctx, stmt)
			if err != nil {
				return settled, fmt.Errorf("settle v%d stash %s: %w", version, st.Name(), err)
			}
			if i == len(stmts)-1 {
				if r.Rows, err = res.RowsAffected(); err != nil {
					return settled, fmt.Errorf("settle v%d stash %s: %w", version, st.Name(), err)
				}
			}
		}
		settled = append(settled, r)
	}
	return settled, nil
}

// ReplayStash applies version's stash to db after its Up. It is the one
// restorer: the runner calls it after every forward step, and the round-trip
// gate steps through it the same way. [LAW:single-enforcer] A stash with
// entries and no image was written by a Down no settle can judge: it is held,
// never written, and stays in the carrier.
func ReplayStash(ctx context.Context, db Querier, version int64) (Replayed, error) {
	var out Replayed
	present, err := carrierFor(ctx, db, version)
	if err != nil || !present {
		return out, err
	}
	for _, st := range Restores[version] {
		table, column := st.Entry()
		var entries, images int64
		if err := db.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM schema_stash WHERE table_name = ? AND column_name = ?),
			(SELECT COUNT(*) FROM schema_stash WHERE table_name = ? AND column_name = ?)`,
			table, column, table, image(column)).Scan(&entries, &images); err != nil {
			return out, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
		}
		if entries > 0 && images == 0 {
			out.Held = append(out.Held, StashRows{Version: version, Name: st.Name(), Rows: entries})
			continue
		}
		r := StashRows{Version: version, Name: st.Name()}
		for _, stmt := range st.RestoreSQL() {
			res, err := db.ExecContext(ctx, stmt)
			if err != nil {
				return out, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return out, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
			}
			r.Rows += n
		}
		for _, stmt := range []string{dropEntries(table, column), dropEntries(table, image(column))} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return out, fmt.Errorf("replay v%d stash %s: %w", version, st.Name(), err)
			}
		}
		out.Restored = append(out.Restored, r)
	}
	return out, nil
}

// carrierFor says whether version has a stash and db a carrier to hold it.
func carrierFor(ctx context.Context, db Querier, version int64) (bool, error) {
	if len(Restores[version]) == 0 {
		return false, nil
	}
	present, err := CarrierPresent(ctx, db)
	if err != nil {
		return false, fmt.Errorf("v%d stash: %w", version, err)
	}
	return present, nil
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
