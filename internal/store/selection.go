package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/selector"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// frag is SQL text with the arguments it binds, in the order it binds them, so
// a statement is assembled from pieces whose text and arguments are never
// kept apart. [LAW:one-source-of-truth]
type frag struct {
	sql  string
	args []any
}

func sqlf(text string, args ...any) frag { return frag{sql: text, args: args} }

func joinFrags(sep string, parts []frag) frag {
	var out frag
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, part.sql)
		out.args = append(out.args, part.args...)
	}
	out.sql = strings.Join(texts, sep)
	return out
}

func (f frag) within(before, after string) frag {
	return frag{sql: before + f.sql + after, args: f.args}
}

// compiler turns a selection into SQL over the issue the statement lists,
// bound as i. An atom is TRUE where it holds, and FALSE or NULL where it does
// not, which a WHERE reads alike, so an atom on an indexed column is one the
// planner reads through the index. A negation (!=, !path) reads NULL as FALSE
// (negate): a NULL column is no value, as it is in the in-memory engine.
// [LAW:one-source-of-truth]
type compiler struct {
	selection storage.Selection
	facts     derived
	aliases   int
}

// derived is what a selection reads that no column holds, read before its
// statement by the model's own rules: an epic's state from the leaves the walk
// tallies, and an in-progress leaf's phase from its history. The SQL compares
// the values those rules give, so it states neither rule a second time.
// [LAW:one-source-of-truth]
type derived struct {
	states fact
	phases fact
	// progress is every admitted epic's when the selector reads a state: the
	// states are read from it, and a listing hydrates the epics it lists with
	// it rather than walking again.
	progress map[model.ID]model.Progress
}

// fact is one derived field's values: the issues that hold each value, and
// what every other issue holds. read is whether the statement read it, since
// one is read only for a selector that names its field.
type fact struct {
	read      bool
	byValue   map[string][]model.ID
	otherwise string
}

// compileSelection is the predicate over i that selection selects: the issues
// it admits, among them those its selector selects.
func compileSelection(selection storage.Selection, facts derived) frag {
	c := &compiler{selection: selection, facts: facts}
	return joinFrags(" AND ", []frag{c.admits("i"), c.conjunction(selection.Selector.Atoms(), "i")})
}

func (c *compiler) fresh(prefix string) string {
	c.aliases++
	return fmt.Sprintf("%s%d", prefix, c.aliases)
}

// admits is the read's universe at alias a: an archived or deleted issue
// exists for the read only when the selection includes it, at every issue a
// path reaches as at the one listed.
func (c *compiler) admits(a string) frag {
	parts := []string{}
	if !c.selection.IncludeArchived {
		parts = append(parts, a+".archived_at IS NULL")
	}
	if !c.selection.IncludeDeleted {
		parts = append(parts, a+".deleted_at IS NULL")
	}
	if len(parts) == 0 {
		return sqlf("TRUE")
	}
	return sqlf(strings.Join(parts, " AND "))
}

// conjunction is every atom holding, those reading the row's own values
// first: Dolt evaluates AND left to right and stops at the first false, and
// runs a subquery when a row first asks it, so a statement whose own atoms
// keep no row runs none.
func (c *compiler) conjunction(atoms []selector.Atom, a string) frag {
	if len(atoms) == 0 {
		return sqlf("TRUE")
	}
	ordered := slices.Clone(atoms)
	slices.SortStableFunc(ordered, func(x, y selector.Atom) int { return cmp.Compare(atomCost(x), atomCost(y)) })
	parts := make([]frag, 0, len(ordered))
	for _, atom := range ordered {
		parts = append(parts, c.atom(atom, a))
	}
	return joinFrags(" AND ", parts)
}

// atomCost ranks what an atom asks of a row: a value of its own, or a
// subquery.
func atomCost(atom selector.Atom) int {
	if ownValue(atom.Path()) {
		return 0
	}
	return 1
}

// ownValue reports whether path reads a column of the row it starts from,
// joining nothing.
func ownValue(path selector.Path) bool {
	last := path.Last()
	return !path.Anchored() && len(path.Steps()) == 1 && last.Of() == nil && !listField(last)
}

// listField reports whether f's values are rows of a table of their own.
func listField(f *selector.Field) bool {
	switch f.Kind() {
	case selector.Texts, selector.Records, selector.Rows:
		return true
	default:
		return false
	}
}

// join is one table a path joins and the condition joining it.
type join struct {
	table string
	on    frag
}

// reach is what a path reads: the tables it joins, and the expressions its
// values are over the last of them.
type reach struct {
	joins  []join
	values []frag
}

// body is FROM through WHERE over the path's joins, every join's condition in
// the WHERE.
func (r reach) body(cond frag) frag {
	tables := make([]string, 0, len(r.joins))
	conds := make([]frag, 0, len(r.joins)+1)
	for _, j := range r.joins {
		tables = append(tables, j.table)
		conds = append(conds, j.on)
	}
	conds = append(conds, cond.within("(", ")"))
	return joinFrags(" ", []frag{sqlf("FROM " + strings.Join(tables, ", ") + " WHERE"), joinFrags(" AND ", conds)})
}

func (c *compiler) atom(atom selector.Atom, a string) frag {
	switch test := atom.Test().(type) {
	case selector.Count:
		return c.counted(atom.Path(), a, test)
	case selector.Presence:
		held := c.holding(atom.Path(), a, present)
		if test.Op == selector.Absent {
			return negate(held)
		}
		return held
	case selector.Match:
		op := test.Op
		if op == selector.Ne {
			op = selector.Eq
		}
		held := c.holding(atom.Path(), a, func(v frag) frag { return valueMatch(v, op, test.Values) })
		if test.Op == selector.Ne {
			return negate(held)
		}
		return held
	default:
		panic(fmt.Sprintf("store: a selector test of type %T has no SQL", test))
	}
}

// negate holds where atom does not, reading NULL as FALSE.
func negate(atom frag) frag { return atom.within("NOT COALESCE(", ", FALSE)") }

// holding is whether some value path reads from the issue bound as a
// satisfies cond: a value of the row's own, compared where it stands; some
// issue in the workspace (inWorkspace); or a is one of the rows the path holds
// from (heldFrom).
func (c *compiler) holding(path selector.Path, a string, cond func(frag) frag) frag {
	switch {
	case ownValue(path):
		_, values := c.fieldValues(path.Last(), a)
		return anyValue(values, cond).within("(", ")")
	case path.Anchored():
		r := c.reach(path, "")
		return inWorkspace(r.body(anyValue(r.values, cond)))
	default:
		return c.heldFrom(path, a, func(r reach, _ string) frag { return r.body(anyValue(r.values, cond)) })
	}
}

// inWorkspace is whether the statement from FROM onward finds a row, a
// subquery that reads no issue the statement lists. It is asked as a
// membership (memberOf) because Dolt caches no EXISTS: inside an expression
// an EXISTS runs again for every row.
func inWorkspace(rest frag) frag {
	return memberOf(sqlf("1"), joinFrags(" ", []frag{sqlf("SELECT 1"), rest}))
}

// memberOf is whether v is one of the values subquery selects. The test stays
// inside an expression, where Dolt runs the subquery once and hashes what it
// selects: a bare IN over a subquery in a WHERE is a semi-join, which Dolt
// plans by the tables' sizes, on a small store as a nested loop that runs the
// subquery again for every row.
func memberOf(v, subquery frag) frag {
	return joinFrags(" ", []frag{v.within("COALESCE(", " IN ("), subquery, sqlf("), FALSE)")})
}

// heldFrom is whether the issue bound as a is one of the rows the path holds
// from: the path read from a fresh issue, every row it holds from selected by
// one subquery that reads nothing of a (memberOf). A subquery that read a
// would run again for every row, and Dolt misbinds a reference to a inside an
// ON or an OR.
// rest is FROM onward, given the path's joins and the fresh issue's alias.
func (c *compiler) heldFrom(path selector.Path, a string, rest func(r reach, start string) frag) frag {
	start := c.fresh("i")
	r := c.reach(path, start)
	r.joins = slices.Insert(r.joins, 0, join{table: "issues " + start, on: sqlf("TRUE")})
	return memberOf(sqlf(a+".id"), joinFrags(" ", []frag{sqlf("SELECT " + start + ".id"), rest(r, start)}))
}

// counted is whether the count path reads from the issue bound as a satisfies
// test. From the workspace it is one aggregate, which has a row even over no
// values. From a row it is the rows the count grouped by the path's start
// holds of, or, for a test zero satisfies, every row but those it fails, since
// a row the path reads nothing from is in no group.
func (c *compiler) counted(path selector.Path, a string, test selector.Count) frag {
	if path.Anchored() {
		r := c.reach(path, "")
		v := r.values[0]
		return inWorkspace(joinFrags(" ", []frag{r.body(present(v)), sqlf("HAVING"), countMatch(v.within("COUNT(DISTINCT ", ")"), test)}))
	}
	zero := test.Holds(0)
	held := c.heldFrom(path, a, func(r reach, start string) frag {
		v := r.values[0]
		having := countMatch(v.within("COUNT(DISTINCT ", ")"), test)
		if zero {
			having = having.within("NOT (", ")")
		}
		return joinFrags(" ", []frag{r.body(present(v)), sqlf("GROUP BY " + start + ".id HAVING"), having})
	})
	if zero {
		return negate(held)
	}
	return held
}

func anyValue(values []frag, cond func(frag) frag) frag {
	parts := make([]frag, 0, len(values))
	for _, v := range values {
		parts = append(parts, cond(v))
	}
	return joinFrags(" OR ", parts)
}

// present is whether expression v holds a value: the empty text and NULL are
// none.
func present(v frag) frag { return v.within("COALESCE(", ", '') <> ''") }

// sqlOps spells each comparison the grammar has in SQL.
var sqlOps = map[selector.Op]string{selector.Lt: "<", selector.Le: "<=", selector.Gt: ">", selector.Ge: ">="}

func valueMatch(v frag, op selector.Op, values []string) frag {
	switch op {
	case selector.Eq:
		return inList(v, values)
	case selector.Contains:
		parts := make([]frag, 0, len(values))
		for _, value := range values {
			parts = append(parts, joinFrags("", []frag{v.within("LOWER(", ")"), sqlf(` LIKE ? ESCAPE '\\'`, "%"+likeEscaper.Replace(value)+"%")}))
		}
		return joinFrags(" OR ", parts)
	default:
		return joinFrags(" ", []frag{v, sqlf(sqlOps[op]+" ?", values[0])})
	}
}

func countMatch(count frag, test selector.Count) frag {
	values := make([]any, len(test.Values))
	for i, n := range test.Values {
		values[i] = n
	}
	switch test.Op {
	case selector.Eq:
		return joinFrags(" ", []frag{count, sqlf("IN ("+placeholders(len(values))+")", values...)})
	case selector.Ne:
		return joinFrags(" ", []frag{count, sqlf("IN ("+placeholders(len(values))+")", values...)}).within("NOT (", ")")
	default:
		return joinFrags(" ", []frag{count, sqlf(sqlOps[test.Op]+" ?", values[0])})
	}
}

// inList is whether v is one of values. One value is one index range. A list
// binds as one JSON array read through JSON_TABLE, because the planner turns
// each value of a literal list into an index range and merges the ranges
// pairwise (idset.go).
func inList(v frag, values []string) frag {
	if len(values) == 1 {
		return joinFrags(" ", []frag{v, sqlf("= ?", values[0])})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		panic("store: marshal a value list: " + err.Error())
	}
	return memberOf(v, sqlf("SELECT j.v FROM JSON_TABLE(?, '$[*]' COLUMNS (v VARCHAR(16383) PATH '$')) j", string(encoded)))
}

// likeEscaper makes a fragment's % and _ mean themselves, as a contains test
// in the in-memory engine reads them.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// reach is what path reads from the issue bound as from, or from the
// workspace for none.
func (c *compiler) reach(path selector.Path, from string) reach {
	var r reach
	for _, step := range path.Steps() {
		f := step.Field()
		if f.Of() == nil {
			joins, values := c.fieldValues(f, from)
			r.joins = append(r.joins, joins...)
			r.values = values
			continue
		}
		to := c.fresh("i")
		joins := c.rowJoins(f, from, to)
		last := &joins[len(joins)-1]
		last.on = joinFrags(" AND ", []frag{last.on, c.admits(to), c.conjunction(step.Where().Atoms(), to)})
		r.joins = append(r.joins, joins...)
		r.values = []frag{sqlf(to + ".id")}
		from = to
	}
	return r
}

// rowJoins reaches the issues field f holds from the issue bound as from
// (none, for the workspace), binding them as to. The last join is the issue
// table, so the caller can hold it to the read's universe.
func (c *compiler) rowJoins(f *selector.Field, from, to string) []join {
	r := c.fresh("r")
	issue := "issues " + to
	switch f {
	case storage.WorkspaceIssues:
		return []join{{table: issue, on: sqlf("TRUE")}}
	case storage.IssueParent.Field:
		return []join{
			{table: "relations " + r, on: sqlf(r + ".src_id = " + from + ".id AND " + r + ".type = 'parent-child'")},
			{table: issue, on: sqlf(to + ".id = " + r + ".dst_id")},
		}
	case storage.IssueChildren:
		return []join{
			{table: "relations " + r, on: sqlf(r + ".dst_id = " + from + ".id AND " + r + ".type = 'parent-child'")},
			{table: issue, on: sqlf(to + ".id = " + r + ".src_id")},
		}
	case storage.IssueSiblings:
		up, parent := c.fresh("r"), c.fresh("i")
		return []join{
			{table: "relations " + up, on: sqlf(up + ".src_id = " + from + ".id AND " + up + ".type = 'parent-child'")},
			{table: "issues " + parent, on: joinFrags(" AND ", []frag{sqlf(parent + ".id = " + up + ".dst_id"), c.admits(parent)})},
			{table: "relations " + r, on: sqlf(r + ".dst_id = " + up + ".dst_id AND " + r + ".type = 'parent-child' AND " + r + ".src_id <> " + from + ".id")},
			{table: issue, on: sqlf(to + ".id = " + r + ".src_id")},
		}
	case storage.IssueBlockedBy:
		return []join{
			{table: "relations " + r, on: sqlf(r + ".src_id = " + from + ".id AND " + r + ".type = 'blocked-by'")},
			{table: issue, on: sqlf(to + ".id = " + r + ".dst_id")},
		}
	case storage.IssueBlocks:
		return []join{
			{table: "relations " + r, on: sqlf(r + ".dst_id = " + from + ".id AND " + r + ".type = 'blocked-by'")},
			{table: issue, on: sqlf(to + ".id = " + r + ".src_id")},
		}
	case storage.IssueRelated:
		return []join{
			{table: relatedEnds + " " + r, on: sqlf(r + ".near = " + from + ".id")},
			{table: issue, on: sqlf(to + ".id = " + r + ".far")},
		}
	case storage.IssueFollowsUp:
		return []join{{table: issue, on: sqlf(to + ".id = " + from + ".follows_up")}}
	case storage.IssueFollowUps:
		return []join{{table: issue, on: sqlf(to + ".follows_up = " + from + ".id")}}
	case storage.IssueRedirectTarget:
		return []join{{table: issue, on: sqlf(to + ".id = " + from + ".redirect_target")}}
	default:
		panic(fmt.Sprintf("store: field %s has no SQL", f.Name()))
	}
}

// relatedEnds is every related edge from each of its ends, near to far. A
// relation is stored once in one direction and reads the same from both, and
// a choice of the far end in the join (a CASE, or an OR of the two directions)
// is one Dolt misreads.
const relatedEnds = `(SELECT src_id AS near, dst_id AS far FROM relations WHERE type = 'related-to'
	UNION ALL SELECT dst_id, src_id FROM relations WHERE type = 'related-to')`

// fieldValues is the expressions field f's values are, read from the issue
// bound as a, and the table a list of them joins.
func (c *compiler) fieldValues(f *selector.Field, a string) ([]join, []frag) {
	col := func(name string) ([]join, []frag) { return nil, []frag{sqlf(a + "." + name)} }
	switch f {
	case storage.IssueID.Field:
		return col("id")
	case storage.IssueTitle:
		return col("title")
	case storage.IssueTopic:
		return col("topic")
	case storage.IssueType.Field:
		return col("issue_type")
	case storage.IssuePriority:
		return nil, []frag{sqlf(priorityName(a))}
	case storage.IssueAssignee:
		return col("assignee")
	case storage.IssueLane:
		return col("lane")
	case storage.IssueStatus.Field:
		// A leaf's state is its column; an epic's, whose column is NULL, is
		// the one its leaves derive.
		return nil, []frag{c.derivedValue(f, c.facts.states, a).within("COALESCE("+a+".status, ", ")")}
	case storage.IssueResolution:
		return col("resolution")
	case storage.IssueCreatedAt:
		return col("created_at")
	case storage.IssueUpdatedAt:
		return col("updated_at")
	case storage.IssueClosedAt:
		return col("closed_at")
	case storage.IssueDescription:
		return col("description")
	case storage.IssuePrompt:
		return col("agent_prompt")
	case storage.IssuePlan:
		return col("work_plan")
	case storage.IssueText:
		return nil, []frag{sqlf(a + ".title"), sqlf(a + ".description"), sqlf("COALESCE(" + a + ".agent_prompt, '')"), sqlf(a + ".work_plan"), sqlf(a + ".topic")}
	case storage.IssuePhase:
		return nil, []frag{c.derivedValue(f, c.facts.phases, a)}
	case storage.IssueLabels.Field:
		l := c.fresh("l")
		return []join{{table: "labels " + l, on: sqlf(l + ".issue_id = " + a + ".id")}}, []frag{sqlf(l + ".label")}
	case storage.IssueComments:
		m := c.fresh("c")
		return []join{{table: "comments " + m, on: sqlf(m + ".issue_id = " + a + ".id")}}, []frag{sqlf(m + ".id")}
	case storage.IssuePlanReviews:
		v := c.fresh("v")
		return []join{{table: "plan_reviews " + v, on: sqlf(v + ".issue_id = " + a + ".id")}}, []frag{sqlf(v + ".id")}
	default:
		panic(fmt.Sprintf("store: field %s has no SQL", f.Name()))
	}
}

// derivedValue is the value derived fact holds for the issue bound as a. A
// statement that compiles a derived field reads its fact first, and one that
// did not has nothing true to say of it, so compiling it is the program's
// mistake. [LAW:no-silent-failure]
func (c *compiler) derivedValue(f *selector.Field, held fact, a string) frag {
	if !held.read {
		panic("store: a selection compiled " + f.Name() + " without reading it")
	}
	otherwise := sqlf("NULL")
	if held.otherwise != "" {
		otherwise = sqlf("?", held.otherwise)
	}
	if len(held.byValue) == 0 {
		return otherwise
	}
	parts := []frag{sqlf("CASE")}
	for _, value := range slices.Sorted(maps.Keys(held.byValue)) {
		ids := held.byValue[value]
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = string(id)
		}
		parts = append(parts, sqlf("WHEN"), inList(sqlf(a+".id"), names), sqlf("THEN ?", value))
	}
	parts = append(parts, sqlf("ELSE"), otherwise, sqlf("END"))
	return joinFrags(" ", parts)
}

// priorityName is the priority of the issue bound as a by the name a selector
// compares, from the one vocabulary model.Priorities states.
func priorityName(a string) string {
	var b strings.Builder
	b.WriteString("CASE " + a + ".priority")
	for _, p := range model.Priorities() {
		fmt.Fprintf(&b, " WHEN %d THEN '%s'", int(p), p.String())
	}
	b.WriteString(" END")
	return b.String()
}

// derivedFacts reads the facts no column holds that selection's selector
// reads, and only those. An epic's state is read where the selector reads a
// state, and refuses a parent loop as hydration does, so a read that derives
// no state answers over a loop it never reads.
func (s *Store) derivedFacts(ctx context.Context, selection storage.Selection) (derived, error) {
	var facts derived
	var err error
	if selection.Selector.Reads(storage.IssueStatus.Field) {
		if facts.progress, err = s.admittedProgress(ctx, selection); err != nil {
			return derived{}, err
		}
		facts.states = statesOf(facts.progress)
	}
	if selection.Selector.Reads(storage.IssuePhase) {
		if facts.phases, err = s.leafPhases(ctx); err != nil {
			return derived{}, err
		}
	}
	return facts, nil
}

// statesOf is each epic's state by model.Progress.State, the container rule
// the in-memory engine derives with, so the SQL compares that state rather
// than a second statement of the rule. [LAW:one-source-of-truth]
func statesOf(progress map[model.ID]model.Progress) fact {
	byState := map[string][]model.ID{}
	for id, p := range progress {
		state := string(p.State())
		byState[state] = append(byState[state], id)
	}
	return fact{read: true, byValue: byState, otherwise: string(model.StateOpen)}
}

// leafPhases is the phase of every in-progress leaf, by model.PhaseFrom over
// its history; no other issue has one. A leaf's stored status is its state,
// so the leaves are the rows whose column says in_progress.
func (s *Store) leafPhases(ctx context.Context) (fact, error) {
	inProgress := string(model.StateInProgress)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM issues WHERE status = ?`, inProgress)
	if err != nil {
		return fact{}, fmt.Errorf("read in-progress leaves: %w", err)
	}
	var leaves []model.ID
	for rows.Next() {
		var id model.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fact{}, err
		}
		leaves = append(leaves, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fact{}, err
	}
	events, err := s.queryEvents(ctx, `SELECT `+eventColumns+`, c.field, c.from_value, c.to_value
		FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id
		WHERE e.issue_id IN (SELECT id FROM issues WHERE status = ?)`, inProgress)
	if err != nil {
		return fact{}, fmt.Errorf("read in-progress histories: %w", err)
	}
	histories := map[model.ID][]model.IssueEvent{}
	for _, event := range events {
		histories[event.IssueID] = append(histories[event.IssueID], event)
	}
	byPhase := map[string][]model.ID{}
	for _, id := range leaves {
		phase := string(model.PhaseFrom(histories[id]))
		byPhase[phase] = append(byPhase[phase], id)
	}
	return fact{read: true, byValue: byPhase}, nil
}

// where is the predicate over i that selection selects, with the facts it
// read to state it.
func (s *Store) where(ctx context.Context, selection storage.Selection) (frag, derived, error) {
	facts, err := s.derivedFacts(ctx, selection)
	if err != nil {
		return frag{}, derived{}, err
	}
	return compileSelection(selection, facts), facts, nil
}

// SelectIssueIDs implements storage.IssueReader: the ids selection selects, id
// ascending.
func (s *Store) SelectIssueIDs(ctx context.Context, selection storage.Selection) ([]model.ID, error) {
	where, _, err := s.where(ctx, selection)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT i.id FROM issues i WHERE "+where.sql+" ORDER BY i.id", where.args...)
	if err != nil {
		return nil, fmt.Errorf("select issues: %w", err)
	}
	defer rows.Close()
	ids := []model.ID{}
	for rows.Next() {
		var id model.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListIssues implements storage.IssueReader: the selection's rows, read by its
// own statement, hydrated, ordered and cut. The order runs in Go because a
// container's state and tree order are readings no column holds.
func (s *Store) ListIssues(ctx context.Context, listing storage.Listing) ([]model.Issue, error) {
	where, facts, err := s.where(ctx, listing.Selection)
	if err != nil {
		return nil, err
	}
	rows, err := s.scanIssueRows(ctx, `SELECT `+issueColumnsQualified+` FROM issues i WHERE `+where.sql, where.args)
	if err != nil {
		return nil, err
	}
	listed := make([]model.ID, len(rows))
	for i, row := range rows {
		listed[i] = row.Issue.ID
	}
	ordering, err := s.listingOrdering(ctx, listing.Sort, listed)
	if err != nil {
		return nil, err
	}
	hydrated, err := s.hydrateKnowing(ctx, rows, facts.progress)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(hydrated, ordering)
	return capLimit(hydrated, listing.Limit), nil
}
