package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// contextIndent is the single indent used for every context line printed under
// a ready or backlog row. [LAW:one-source-of-truth] one width, referenced by
// both renderers, so a change moves both views together.
const contextIndent = "    "

// The CLI owns history timestamp presentation; store/model keep time.Time as
// canonical data. [LAW:one-source-of-truth]
const historyTimestampLayout = "Jan 2, 2006 3:04 PM MST"

// printEpicLine renders the indented "epic:" context line shown identically
// under ready and backlog rows. A nil ref (issue has no epic parent) emits
// nothing — absence is data, not a caller-side branch.
// [LAW:dataflow-not-control-flow]
func printEpicLine(w io.Writer, indent string, epic *annotation.ParentEpicRef) error {
	if epic == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, "%sepic: %s  %s\n", indent, epic.ID, epic.Title)
	return err
}

// printIDListLine renders one indented "<label>: id, id, ..." context line —
// the shared shape behind both "depends on:" and "unblocks:". An empty list
// emits nothing, so callers pass the list rather than branching on its length.
// [LAW:one-type-per-behavior] both lines are one behavior differing only in
// label and data. [LAW:dataflow-not-control-flow]
func printIDListLine(w io.Writer, indent, label string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "%s%s: %s\n", indent, label, strings.Join(ids, ", "))
	return err
}

func printIssueSummary(w io.Writer, issue model.Issue) error {
	_, err := fmt.Fprintf(w, "%s [%s/%s/%s/%s] %s%s\n", issue.ID, formatIssueState(issue), issue.IssueType, issue.Topic, issue.Priority, issue.Title, formatLabels(issue.Labels))
	return err
}

func printIssueTable(w io.Writer, issues []model.Issue, columns []string, rels map[string]relationColumns) error {
	resolved := resolveColumns(columns)
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(resolved, "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, resolved, "\t", rels)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []string, rels map[string]relationColumns) error {
	resolved := resolveColumns(columns)
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, resolved, " | ", rels)); err != nil {
			return err
		}
	}
	return nil
}

func printIssueDetail(w io.Writer, detail model.IssueDetail) error {
	issue := detail.Issue
	archivedAt, deletedAt := model.RetentionTimestamps(issue.Retention())
	if _, err := fmt.Fprintf(w, "%s\n%s\n\ntype: %s\ntopic: %s\npriority: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, issue.Title, issue.IssueType, issue.Topic, issue.Priority, emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(archivedAt), formatOptionalTime(deletedAt)); err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] Capability presence is the type-encoded
	// answer to leaf-vs-container; the printer dispatches once on that single
	// shape signal rather than asking IsContainer or comparing issue types.
	if caps := issue.Capabilities(); caps.Status != nil {
		if _, err := fmt.Fprintf(w, "status: %s\nassignee: %s\n", caps.Status.Value, emptyDash(issue.AssigneeValue())); err != nil {
			return err
		}
		// Resolution is closed-only optional data; the line appears exactly when a
		// close recorded one (absent for open/in_progress and for a `done`/legacy
		// close). [LAW:dataflow-not-control-flow] presence of the value, not a mode.
		if caps.Status.Resolution != nil {
			if _, err := fmt.Fprintf(w, "resolution: %s\n", *caps.Status.Resolution); err != nil {
				return err
			}
		}
	} else {
		progress := issue.Progress()
		if _, err := fmt.Fprintf(w, "children: %d closed, %d in_progress, %d open (%d total)\n", progress.Closed, progress.InProgress, progress.Open, progress.Total); err != nil {
			return err
		}
	}
	// "unblocks:" surfaces the same leverage signal `lit backlog` shows inline:
	// IDs of open issues that depend on this one, i.e. would lose this as an
	// open dependency when it closes. Empty list = no leverage; line omitted.
	if ids := openUnblockIDs(detail.Blocks); len(ids) > 0 {
		if _, err := fmt.Fprintf(w, "unblocks: %s\n", strings.Join(ids, ", ")); err != nil {
			return err
		}
	}
	// [LAW:dataflow-not-control-flow] Parent block precedes the leaf description
	// so an agent reading top-to-bottom encounters containing context before
	// the specific leaf details. When the parent has a description, it inlines
	// indented under the parent line. (links-agent-epic-model-uew.3)
	if detail.Parent != nil {
		if _, err := fmt.Fprintf(w, "\nparent:\n- %s %s\n", detail.Parent.ID, detail.Parent.Title); err != nil {
			return err
		}
		if detail.Parent.Description != "" {
			if _, err := fmt.Fprintf(w, "%s\n", indentLines(detail.Parent.Description, "  ")); err != nil {
				return err
			}
		}
	}
	if issue.Description != "" {
		if _, err := fmt.Fprintf(w, "\ndescription:\n%s\n", issue.Description); err != nil {
			return err
		}
	}
	if issue.Prompt != "" {
		if _, err := fmt.Fprintf(w, "\nprompt:\n%s\n", issue.Prompt); err != nil {
			return err
		}
	}
	if err := printIssueGroup(w, "children", detail.Children); err != nil {
		return err
	}
	// No "siblings" group here: when this issue has an epic parent,
	// writeEpicContext's "Epic: ... Children:" block already lists every
	// sibling (plus this ticket itself, marked "(you are here)") in rank
	// order — a strict superset of a bare siblings list. Printing both would
	// repeat the same ids twice for zero added information.
	// [LAW:one-source-of-truth]
	if err := printIssueGroup(w, "depends_on", detail.DependsOn); err != nil {
		return err
	}
	if err := printIssueGroup(w, "blocks", detail.Blocks); err != nil {
		return err
	}
	// redirect precedes related: it is the canonical "where did this work go"
	// edge, distinct from incidental peer links. The store already excluded it
	// from Related, so the two groups never overlap. [LAW:dataflow-not-control-flow]
	if err := printIssueGroup(w, "redirect", redirectGroup(detail.RedirectTarget)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	if len(detail.Comments) > 0 {
		if _, err := fmt.Fprintln(w, "\ncomments:"); err != nil {
			return err
		}
		for _, c := range detail.Comments {
			if _, err := fmt.Fprintf(w, "- [%s] %s\n", c.CreatedBy, strings.ReplaceAll(c.Body, "\n", "\\n")); err != nil {
				return err
			}
		}
	}
	// [LAW:one-source-of-truth] `lit show` renders only current state; the
	// field-level transition trail lives behind `lit history` (printIssueHistory)
	// so a cold reader never mistakes a superseded before→after line for a
	// current fact. The shared printHistoryEvents renderer keeps its home there.
	return nil
}

// issueFieldNames is the single definition of which field names `lit show
// --field` accepts and how each renders. It is a superset of the table
// --columns set (resolveColumns): it also exposes the multi-line fields
// (description, prompt) the field-limited view exists to serve, which a
// single-line table row cannot carry. [LAW:one-source-of-truth]
var issueFieldNames = map[string]func(model.Issue) string{
	"id":          func(i model.Issue) string { return i.ID },
	"title":       func(i model.Issue) string { return i.Title },
	"description": func(i model.Issue) string { return i.Description },
	"prompt":      func(i model.Issue) string { return i.Prompt },
	"type":        func(i model.Issue) string { return string(i.IssueType) },
	"topic":       func(i model.Issue) string { return i.Topic },
	"priority":    func(i model.Issue) string { return i.Priority.String() },
	"status":      func(i model.Issue) string { return string(i.State()) },
	"assignee":    func(i model.Issue) string { return i.AssigneeValue() },
	"labels":      func(i model.Issue) string { return strings.Join(i.Labels, ",") },
	"rank":        func(i model.Issue) string { return i.Rank },
	"lane":        func(i model.Issue) string { return i.Lane },
	"created_at":  func(i model.Issue) string { return i.CreatedAt.Format(time.RFC3339) },
	"updated_at":  func(i model.Issue) string { return i.UpdatedAt.Format(time.RFC3339) },
}

// sortedIssueFieldNames lists the valid --field names for the unknown-field
// error message, so a typo is answered with the exact accepted vocabulary
// instead of a bare rejection.
func sortedIssueFieldNames() []string {
	names := make([]string, 0, len(issueFieldNames))
	for name := range issueFieldNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// printIssueFields renders exactly the requested fields and nothing else — no
// header, no parent/epic context, no siblings, no children summary — so an
// agent can read a ticket's description (or any other single field) without
// the full `lit show` dump. Fields are validated before anything prints, so
// an unknown name in a multi-field request fails clean rather than emitting
// a partial result. A single field prints its bare value with no label, so
// the output round-trips directly into `lit update --description` and
// similar; multiple fields print as "name: value" lines so a multi-line
// value stays attributable to its field.
func printIssueFields(w io.Writer, issue model.Issue, fields []string) error {
	type resolvedField struct {
		name  string
		value string
	}
	resolved := make([]resolvedField, 0, len(fields))
	for _, field := range fields {
		name := strings.ToLower(strings.TrimSpace(field))
		getValue, ok := issueFieldNames[name]
		if !ok {
			return UsageError{Message: fmt.Sprintf("unknown --field %q; valid fields: %s", field, strings.Join(sortedIssueFieldNames(), ", "))}
		}
		resolved = append(resolved, resolvedField{name: name, value: getValue(issue)})
	}
	if len(resolved) == 1 {
		_, err := fmt.Fprintln(w, resolved[0].value)
		return err
	}
	for _, entry := range resolved {
		if _, err := fmt.Fprintf(w, "%s: %s\n", entry.name, entry.value); err != nil {
			return err
		}
	}
	return nil
}

// printHistoryEvents renders the field-level transition trail: one
// "- [actor @ time] action reason" line per event, followed by that event's
// indented "field: from → to" change lines. This is the single definition of
// how the transition trail is formatted; the dedicated `lit history` command
// (printIssueHistory) is its only caller. [LAW:one-source-of-truth] one renderer,
// so the trail can never render two ways; [LAW:decomposition] carved at the
// event/detail joint, ready for any future surface that needs the same trail.
func printHistoryEvents(w io.Writer, events []model.IssueEvent) error {
	for _, event := range events {
		// Plain field updates carry no Action; "update" is their display label.
		// [LAW:dataflow-not-control-flow] absence of intent is data, not a mode.
		action := event.Action
		if action == "" {
			action = "update"
		}
		if _, err := fmt.Fprintf(w, "- [%s @ %s] %s %s\n", event.Actor, formatHistoryTimestamp(event.CreatedAt), action, strings.ReplaceAll(event.Reason, "\n", "\\n")); err != nil {
			return err
		}
		for _, change := range event.Changes {
			if _, err := fmt.Fprintf(w, "    %s: %s → %s\n", change.Field, emptyDash(change.From), emptyDash(change.To)); err != nil {
				return err
			}
		}
	}
	return nil
}

// printIssueHistory renders the standalone `lit history` view: an identifying
// header (id + title) so the output stands alone, then the field-level
// transition trail via the shared printHistoryEvents. Every issue carries at
// least its "created" event, so the trail is never empty in practice; an empty
// Events slice honestly prints just the header rather than fabricating a line.
func printIssueHistory(w io.Writer, detail model.IssueDetail) error {
	if _, err := fmt.Fprintf(w, "%s\n%s\n\nhistory:\n", detail.Issue.ID, detail.Issue.Title); err != nil {
		return err
	}
	return printHistoryEvents(w, detail.Events)
}

// redirectGroup adapts the single optional redirect target to the slice
// printIssueGroup renders, so the redirect reuses the one definition of the
// "- id [state] title" line format and the omit-when-empty rule. A nil target
// yields the empty slice, which printIssueGroup omits.
func redirectGroup(target *model.Issue) []model.Issue {
	if target == nil {
		return nil
	}
	return []model.Issue{*target}
}

func printIssueGroup(w io.Writer, label string, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", label); err != nil {
		return err
	}
	for _, issue := range issues {
		// The standing marker is why the group is worth reading at all: the
		// agent learns where each referenced issue stands inline, without a
		// second 'lit show' per id.
		if _, err := fmt.Fprintf(w, "- %s [%s] %s\n", issue.ID, issueStanding(issue), issue.Title); err != nil {
			return err
		}
	}
	return nil
}

// issueStanding is the one word for where an issue stands, composed from the
// two orthogonal lifecycle axes with retention dominating status.
//
// State() alone is shape-agnostic — leaves return their owned status, containers
// return state derived from children — but it is only half the truth: a deleted
// ticket's status is still "open", so rendering State() printed dead blockers as
// "[open]" and sent readers hunting for an id that appears in no listing. A
// frozen issue's status describes work nobody may do, which is why the retention
// name replaces it rather than joining it.
// [LAW:one-source-of-truth] Every surface that names a referenced issue's state
// reads this, so the epic plan's markers and the relationship groups cannot
// disagree about one ticket; Frozen and RetentionName stay the sole owners of
// "out of the flow" and of the words for it.
func issueStanding(issue model.Issue) string {
	if model.Frozen(issue.Retention()) {
		return model.RetentionName(issue.Retention())
	}
	return string(issue.State())
}

// relationColumns carries the per-issue relationship facts the relationship
// columns project, derived once from the canonical graph (store.IssueRelations)
// so the list view never reinterprets edge semantics. The zero value is the
// honest answer for an issue with no relations loaded (no parent, not blocked),
// which is exactly what a nil rels map yields on lookup.
type relationColumns struct {
	parentID string
	blocked  bool
}

// relationColumnNames is the single definition of which projection columns are
// served from the relationship graph rather than the issue row. Selecting one
// is what triggers the batch relation load in the list path. These are populated
// only on the `lit ls` path (listRelationColumns); other --columns surfaces pass
// a nil rels map, so until they thread one these columns render "-" there.
// [LAW:one-source-of-truth] relationship-column membership decided once, here.
var relationColumnNames = map[string]struct{}{"parent": {}, "blocked": {}}

// projectsRelationColumn reports whether any resolved column is served from the
// relationship graph — the data-shaped signal the list path uses to decide
// whether to pay the relation-graph query.
func projectsRelationColumn(columns []string) bool {
	for _, column := range columns {
		if _, ok := relationColumnNames[column]; ok {
			return true
		}
	}
	return false
}

func formatIssueColumns(issue model.Issue, columns []string, delimiter string, rels map[string]relationColumns) string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		switch column {
		case "id":
			values = append(values, issue.ID)
		case "state":
			values = append(values, formatIssueState(issue))
		case "type":
			values = append(values, string(issue.IssueType))
		case "topic":
			values = append(values, issue.Topic)
		case "priority":
			values = append(values, issue.Priority.String())
		case "title":
			values = append(values, issue.Title)
		case "assignee":
			values = append(values, emptyDash(issue.AssigneeValue()))
		case "labels":
			values = append(values, emptyDash(strings.Join(issue.Labels, ",")))
		case "updated_at":
			values = append(values, issue.UpdatedAt.Format(time.RFC3339))
		case "created_at":
			values = append(values, issue.CreatedAt.Format(time.RFC3339))
		case "parent":
			// Reading a nil map yields the zero relationColumns — "-" for an issue
			// whose relations weren't loaded — so the column needs no guard.
			values = append(values, emptyDash(rels[issue.ID].parentID))
		case "blocked":
			values = append(values, blockedLabel(rels[issue.ID].blocked))
		}
	}
	return strings.Join(values, delimiter)
}

// blockedLabel renders the blocked indicator as a self-describing token rather
// than a bare boolean, so the default headerless `lines` format stays legible
// (`id | blocked`) without relying on a column header.
func blockedLabel(blocked bool) string {
	if blocked {
		return "blocked"
	}
	return "-"
}

func resolveColumns(columns []string) []string {
	if len(columns) == 0 {
		// [LAW:dataflow-not-control-flow] Default listing still flows through the same projection path.
		return []string{"id", "state", "topic", "title"}
	}
	valid := map[string]struct{}{
		"id": {}, "state": {}, "type": {}, "topic": {}, "priority": {}, "title": {}, "assignee": {}, "labels": {}, "updated_at": {}, "created_at": {}, "parent": {}, "blocked": {},
	}
	out := make([]string, 0, len(columns))
	for _, column := range columns {
		normalized := strings.ToLower(strings.TrimSpace(column))
		if normalized == "" {
			continue
		}
		if _, ok := valid[normalized]; ok {
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return []string{"id", "state", "topic", "title"}
	}
	return out
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func printLabels(w io.Writer, labels []string) error {
	_, err := fmt.Fprintln(w, strings.Join(labels, ","))
	return err
}

func formatLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return " [" + strings.Join(labels, ",") + "]"
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func formatHistoryTimestamp(value time.Time) string {
	return value.Local().Format(historyTimestampLayout)
}

// humanizeCoarseDuration renders a non-negative duration as a coarse,
// human-legible phrase — days/hours/minutes bucketed at 48h/2h/2m, so "how
// long ago" reads consistently across every surface that reports an age
// (sync-divergence age in SyncFailure.agePhrase, build age in runVersion).
// [LAW:single-enforcer] the one place this bucketing convention is defined;
// callers with their own "unknown"/zero handling wrap this rather than
// reimplementing the thresholds.
func humanizeCoarseDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	default:
		return "under a minute"
	}
}

// openUnblockIDs returns the IDs of issues from blocks that are still live —
// the set this issue's closure would actually unblock from a "ready" perspective.
func openUnblockIDs(blocks []model.Issue) []string {
	ids := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if !b.InPlay() {
			continue
		}
		ids = append(ids, b.ID)
	}
	return ids
}

// liveIssues returns the live members of issues, preserving order. The full set
// stays intact upstream (lit show needs every sibling); callers that want only
// the actionable neighborhood — the close/done adjacency view — filter here.
func liveIssues(issues []model.Issue) []model.Issue {
	out := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.InPlay() {
			out = append(out, issue)
		}
	}
	return out
}

// printCloseAdjacency renders a just-closed ticket's live neighborhood at the
// capture moment: its parent, the siblings still in play, related neighbors,
// and the dependents this close unblocked. Each group is omitted when empty, so
// closing an isolated ticket prints nothing. These are relationship FACTS, not a
// cue to act — the post-close guidance already carries the "why".
// [LAW:one-source-of-truth] Reuses lit show's group renderer and its
// now-unblocked-dependents derivation rather than minting a second
// representation of the same graph.
func printCloseAdjacency(w io.Writer, detail model.IssueDetail) error {
	parent := []model.Issue{}
	if detail.Parent != nil {
		parent = append(parent, *detail.Parent)
	}
	if err := printIssueGroup(w, "parent", parent); err != nil {
		return err
	}
	if err := printIssueGroup(w, "siblings", liveIssues(detail.Siblings)); err != nil {
		return err
	}
	// Surface the redirect at the close moment too: closing as duplicate/
	// superseded, the freshest fact is where the work went. Same store-shaped
	// IssueDetail, same omit-when-empty group as lit show.
	if err := printIssueGroup(w, "redirect", redirectGroup(detail.RedirectTarget)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	if ids := openUnblockIDs(detail.Blocks); len(ids) > 0 {
		if _, err := fmt.Fprintf(w, "\nunblocks: %s\n", strings.Join(ids, ", ")); err != nil {
			return err
		}
	}
	return nil
}

func formatIssueState(issue model.Issue) string {
	// State() is shape-agnostic: leaves return their owned status, containers
	// return the state derived from children. StatusValue() with an empty-string
	// fallback was a pellet — duplicate dispatch across the same discriminator.
	parts := []string{string(issue.State())}
	// [LAW:types-are-the-program] Retention is a sum, so at most one tag applies;
	// the old field pair could stack "+archived+deleted", a state the domain
	// never had.
	switch issue.Retention().(type) {
	case model.Archived:
		parts = append(parts, "archived")
	case model.Deleted:
		parts = append(parts, "deleted")
	}
	return strings.Join(parts, "+")
}

func parseColumns(input string) []string {
	return splitCSV(strings.ToLower(input))
}

// indentLines prefixes every line of s with prefix, preserving internal line
// breaks. Trailing newlines are stripped so callers that append their own "\n"
// (e.g., via Fprintf) do not produce a stray prefix-only line at the end.
func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
