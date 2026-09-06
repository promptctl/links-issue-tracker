package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// columnSpec is one printable column: the name `--columns` accepts and the cell
// that name renders, carried on a single value.
//
// Those two facts used to live apart — an accept-list in resolveColumns, a
// render switch in formatIssueColumns, a relationship-membership map beside
// them, and the default projection spelled twice. Four maps of one territory,
// and they had already drifted: `rank` was in none of them though it is the key
// the listing is ordered by, and `state` is spelled `status` everywhere else in
// the CLI, so the name a caller reaches for first was silently dropped. Keeping
// name and renderer on one value is what makes a column that is acceptable but
// unrenderable — or renderable but unnamed — unrepresentable rather than
// merely absent today. [LAW:one-source-of-truth] the column vocabulary is this
// table and nothing else.
type columnSpec struct {
	name string
	// needsRelations marks a column served from the relationship graph rather
	// than from the issue row. Selecting one is what makes the list path pay
	// for the relation query. [LAW:dataflow-not-control-flow] the load is
	// chosen by a value carried on the selected columns, never by a branch on
	// column identity.
	//
	// A nil rels map renders these as "-" for every row, which is the honest
	// zero only for a projection that names no relation column — the case of
	// the fixed, source-constant projections that pass nil. A surface whose
	// caller CHOOSES the columns has to supply the map, or it accepts a name
	// it cannot render and prints a dash indistinguishable from a real "no
	// parent". That is why the workable runner derives the map for its views
	// instead of leaving each renderer to remember.
	needsRelations bool
	// render receives the issue's own relationColumns rather than the whole
	// map, so a renderer cannot read another issue's relations.
	render func(issue model.Issue, rels relationColumns) string
}

// columnRegistry is the vocabulary. Adding a column here is the whole change:
// the accept-set, the `--columns` help text, the rejection message's list of
// valid names, and the relation-load decision are all derived from this slice,
// so none of them can be updated late or forgotten.
var columnRegistry = []columnSpec{
	{name: "id", render: func(i model.Issue, _ relationColumns) string { return i.ID }},
	{name: "state", render: func(i model.Issue, _ relationColumns) string { return formatIssueState(i) }},
	{name: "type", render: func(i model.Issue, _ relationColumns) string { return string(i.IssueType) }},
	{name: "topic", render: func(i model.Issue, _ relationColumns) string { return i.Topic }},
	{name: "priority", render: func(i model.Issue, _ relationColumns) string { return i.Priority.String() }},
	{name: "title", render: func(i model.Issue, _ relationColumns) string { return i.Title }},
	// rank is printable because it is the key `lit ls` orders by; a listing
	// that cannot show its own sort key forces the reader to infer the order.
	{name: "rank", render: func(i model.Issue, _ relationColumns) string { return emptyDash(i.Rank) }},
	{name: "assignee", render: func(i model.Issue, _ relationColumns) string { return emptyDash(i.AssigneeValue()) }},
	{name: "labels", render: func(i model.Issue, _ relationColumns) string {
		return emptyDash(strings.Join(i.Labels, ","))
	}},
	{name: "updated_at", render: func(i model.Issue, _ relationColumns) string {
		return i.UpdatedAt.Format(time.RFC3339)
	}},
	{name: "created_at", render: func(i model.Issue, _ relationColumns) string {
		return i.CreatedAt.Format(time.RFC3339)
	}},
	{name: "parent", needsRelations: true, render: func(_ model.Issue, rels relationColumns) string {
		return emptyDash(rels.parentID)
	}},
	{name: "blocked", needsRelations: true, render: func(_ model.Issue, rels relationColumns) string {
		return blockedLabel(rels.blocked)
	}},
}

// columnsByName indexes the registry for exact-name lookup. Derived, never
// hand-written: a registry entry with no index row is not expressible.
var columnsByName = func() map[string]columnSpec {
	byName := make(map[string]columnSpec, len(columnRegistry))
	for _, spec := range columnRegistry {
		byName[spec.name] = spec
	}
	return byName
}()

// mustColumns resolves a projection this package writes itself — the default,
// and the fixed projections of commands that take no `--columns` flag. Those
// names are source constants, so a miss is a programmer error and not a user's:
// it panics rather than rendering a short row, which is the same refusal to
// paper over a missing column that parseColumnSelection gives the CLI caller.
// [LAW:no-silent-failure]
func mustColumns(names ...string) []columnSpec {
	out := make([]columnSpec, 0, len(names))
	for _, name := range names {
		spec, ok := columnsByName[name]
		if !ok {
			panic(fmt.Sprintf("cli: internal projection names unknown column %q; valid columns: %s",
				name, strings.Join(sortedColumnNames(), ", ")))
		}
		out = append(out, spec)
	}
	return out
}

// defaultColumns is the projection a caller who names no columns gets. It is
// spelled once and resolved through the same registry every named column goes
// through, so the default cannot name a column that does not exist.
func defaultColumns() []columnSpec {
	return mustColumns("id", "state", "topic", "title")
}

// sortedColumnNames is the advertised vocabulary, in one order, for the help
// text and the rejection message alike — the two places a caller learns which
// names exist, kept identical by construction.
func sortedColumnNames() []string {
	names := make([]string, 0, len(columnRegistry))
	for _, spec := range columnRegistry {
		names = append(names, spec.name)
	}
	slices.Sort(names)
	return names
}

// columnsFlagUsage is the `--columns` help string. It enumerates the vocabulary
// so the names are discoverable without first provoking a rejection.
func columnsFlagUsage() string {
	return "Comma-separated output columns: " + strings.Join(sortedColumnNames(), ", ")
}

// parseColumnSelection is the one checkpoint for `--columns`. It turns the raw
// flag expression into the columns to render, or fails loudly with a usage
// error naming both the offending word and the full vocabulary.
//
// The output type is the proof: a []columnSpec can only be assembled here, out
// of registry entries, so no stage downstream can be holding a column name that
// nothing knows how to render — and none of them re-checks, because inland
// there is nothing left to check. What this replaces was a validator that threw
// the proof away and, worse, mapped failure onto the success-shaped default
// projection: `--columns bogus` returned a well-formed table under exit 0, a
// value with the exact shape of a real answer meaning "I could not do my job".
// [LAW:parse-dont-validate] [LAW:no-silent-failure]
func parseColumnSelection(expr string) ([]columnSpec, error) {
	names := splitCSV(strings.ToLower(expr))
	if len(names) == 0 {
		// An empty expression is the caller asking for nothing in particular,
		// which the default projection answers. splitCSV also drops empty
		// elements, so a stray comma lands here rather than in the reject set.
		// Only a NAMED column can be wrong.
		return defaultColumns(), nil
	}
	out := make([]columnSpec, 0, len(names))
	for _, name := range names {
		spec, ok := columnsByName[name]
		if !ok {
			return nil, UsageError{Message: fmt.Sprintf(
				"unknown --columns name %q; valid columns: %s",
				name, strings.Join(sortedColumnNames(), ", "))}
		}
		out = append(out, spec)
	}
	return out, nil
}

// columnNames renders the selected columns' headers.
func columnNames(columns []columnSpec) []string {
	names := make([]string, 0, len(columns))
	for _, spec := range columns {
		names = append(names, spec.name)
	}
	return names
}

// projectsRelationColumn reports whether any selected column is served from the
// relationship graph — the data-shaped signal the list path uses to decide
// whether to pay for the relation-graph query.
func projectsRelationColumn(columns []columnSpec) bool {
	return slices.ContainsFunc(columns, func(spec columnSpec) bool { return spec.needsRelations })
}
