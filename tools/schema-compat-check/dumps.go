package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// A row is one table row keyed by column name, so rows from two dumps compare
// regardless of the order each dump read the columns in.
type row map[string]any

func rowsOf(t store.RawTable) []row {
	rows := make([]row, len(t.Rows))
	for i, cells := range t.Rows {
		r := row{}
		for j, col := range t.Columns {
			r[col] = cells[j]
		}
		rows[i] = r
	}
	return rows
}

// key is a row's identity for comparison: its canonical JSON (encoding/json
// writes map keys sorted).
func (r row) key() string {
	b, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("a row decoded from JSON did not re-encode: %v", err))
	}
	return string(b)
}

func tablesByName(d store.RawDump) map[string]store.RawTable {
	tables := map[string]store.RawTable{}
	for _, t := range d.Tables {
		tables[t.Name] = t
	}
	return tables
}

// carriesData reports whether a cell holds a value a fixture wrote rather than
// a column default: anything but NULL, the empty string, zero or false.
func carriesData(cell any) bool {
	switch v := cell.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case float64:
		return v != 0
	case bool:
		return v
	default:
		return true
	}
}

// unexercised names every table and column tree holds that known lacks and
// that holds no data in tree: schema a release does not know, which the
// fixture never wrote, so the scenario could not have shown the release keeps
// it. known is a store at the release's schema; tree is the store at this
// tree's schema after the fixture ran. [LAW:no-silent-failure] coverage is
// read from the stores, so no list of migrations exists to fall behind.
func unexercised(known, tree store.RawDump) []string {
	knownTables := tablesByName(known)
	var missing []string
	for _, t := range tree.Tables {
		rows := rowsOf(t)
		kt, tableKnown := knownTables[t.Name]
		if !tableKnown {
			if len(rows) == 0 {
				missing = append(missing, fmt.Sprintf("table %s has no rows", t.Name))
			}
			continue
		}
		for _, col := range t.Columns {
			if slices.Contains(kt.Columns, col) {
				continue
			}
			if !slices.ContainsFunc(rows, func(r row) bool { return carriesData(r[col]) }) {
				missing = append(missing, fmt.Sprintf("column %s.%s holds no data", t.Name, col))
			}
		}
	}
	slices.Sort(missing)
	return missing
}

// titleEdit is the one change the scenario has the release make to a row the
// tree wrote: the issue's title set to a new value.
type titleEdit struct {
	issueID, title string
}

// editedColumns are the columns a title edit may change on the edited issue.
var editedColumns = []string{"title", "updated_at"}

// lost names every row of before that after does not hold unchanged, apart
// from edit: the edited issue may differ in editedColumns alone, and its title
// must be the new one. before is the tree's store before the release's round
// trip, after the same store once the round trip is pulled back.
func lost(before, after store.RawDump, edit titleEdit) []string {
	afterTables := tablesByName(after)
	var losses []string
	for _, bt := range before.Tables {
		at, ok := afterTables[bt.Name]
		if !ok {
			losses = append(losses, fmt.Sprintf("table %s is gone", bt.Name))
			continue
		}
		afterRows := rowsOf(at)
		remaining := map[string]int{}
		for _, r := range afterRows {
			remaining[r.key()]++
		}
		for _, r := range rowsOf(bt) {
			if bt.Name == "issues" && r["id"] == edit.issueID {
				losses = append(losses, editLosses(r, afterRows, edit)...)
				continue
			}
			if remaining[r.key()] == 0 {
				losses = append(losses, fmt.Sprintf("table %s lost or changed row %s", bt.Name, r.key()))
				continue
			}
			remaining[r.key()]--
		}
	}
	return losses
}

// editLosses compares the edited issue's row before and after the edit.
func editLosses(before row, afterRows []row, edit titleEdit) []string {
	i := slices.IndexFunc(afterRows, func(r row) bool { return r["id"] == edit.issueID })
	if i < 0 {
		return []string{fmt.Sprintf("issue %s is gone", edit.issueID)}
	}
	after := afterRows[i]
	var losses []string
	if after["title"] != edit.title {
		losses = append(losses, fmt.Sprintf("issue %s title is %v, want the release's edit %q", edit.issueID, after["title"], edit.title))
	}
	for _, col := range slices.Sorted(maps.Keys(before)) {
		if slices.Contains(editedColumns, col) {
			continue
		}
		if (row{col: before[col]}).key() != (row{col: after[col]}).key() {
			losses = append(losses, fmt.Sprintf("issue %s column %s was %v, now %v", edit.issueID, col, before[col], after[col]))
		}
	}
	return losses
}
