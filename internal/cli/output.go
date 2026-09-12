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

// formatEpicLine renders the "epic:" context text for a ref, and "" — the
// printer's "no line" value — for the absent ref. Formatting is split from
// printing because the backlog must choose between this text and a different
// one for the same row, and both spellings of an epic line have to come from
// here. [LAW:one-source-of-truth]
func formatEpicLine(epic *annotation.ParentEpicRef) string {
	if epic == nil {
		return ""
	}
	return fmt.Sprintf("epic: %s  %s", epic.ID, epic.Title)
}

// printEpicLine renders the indented "epic:" context line shown identically
// under ready and backlog rows. A nil ref (issue has no epic parent) emits
// nothing — absence is data, not a caller-side branch.
// [LAW:dataflow-not-control-flow]
func printEpicLine(w io.Writer, indent string, epic *annotation.ParentEpicRef) error {
	return printContextLine(w, indent, formatEpicLine(epic))
}

// epicID names the epic a ref points at, and "" for the absent ref. It sits
// beside printEpicLine so ParentEpicRef's nil case is answered in one file
// rather than at each caller that needs the id to compare.
// [LAW:single-enforcer]
func epicID(epic *annotation.ParentEpicRef) string {
	if epic == nil {
		return ""
	}
	return epic.ID
}

// printContextLine renders one already-formatted indented context line — the
// shape behind context whose text a caller composed, such as the claim line.
// The empty string emits nothing, so callers pass the text rather than
// branching on whether they have any. [LAW:dataflow-not-control-flow]
func printContextLine(w io.Writer, indent, text string) error {
	if text == "" {
		return nil
	}
	_, err := fmt.Fprintf(w, "%s%s\n", indent, text)
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

func printIssueTable(w io.Writer, issues []model.Issue, columns []columnSpec, cells map[string]derivedColumns) error {
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(columnNames(columns), "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, columns, "\t", cells)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []columnSpec, cells map[string]derivedColumns) error {
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, columns, " | ", cells)); err != nil {
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
	if err := printIssueGroup(w, "parent", optionalGroup(detail.Parent)); err != nil {
		return err
	}
	if detail.Parent != nil && detail.Parent.Description != "" {
		if _, err := fmt.Fprintf(w, "%s\n", indentLines(detail.Parent.Description, "  ")); err != nil {
			return err
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
	if err := printIssueGroup(w, "redirect", optionalGroup(detail.RedirectTarget)); err != nil {
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
// --field` accepts and how each renders. It overlaps the --columns vocabulary
// (columns.go) without matching it: it also exposes the multi-line fields
// (description, prompt) the field-limited view exists to serve, which a
// single-line table row cannot carry, and it does not name the relationship
// columns, which are not the issue's own fields. That the two vocabularies
// differ is why `--columns` must name the ones it rejects — `status` and
// `description` are field names a caller reasonably tries as columns.
// [LAW:one-source-of-truth]
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

// optionalGroup adapts a single optional issue — a redirect target, a parent —
// to the slice printIssueGroup renders, so every such group reuses the one
// definition of the "- id [standing] title" line format and the omit-when-empty
// rule. A nil issue yields the empty slice, which printIssueGroup omits.
// [LAW:one-type-per-behavior] The redirect and the parent differ only in the
// label printIssueGroup is given; both are one optional issue rendered as a
// group, so one adapter serves them. The parent had its own hand-rolled line
// instead, which is how it came to print no standing at all — a closed epic
// parent read exactly like an open one (promptctl-output-p60y).
func optionalGroup(issue *model.Issue) []model.Issue {
	if issue == nil {
		return nil
	}
	return []model.Issue{*issue}
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

// issueStanding is the one standing word for where an issue stands: the
// retention axis when it dominates, otherwise the status axis carrying the
// close's recorded reason.
//
// State() alone is shape-agnostic — leaves return their owned status, containers
// return state derived from children — but it is only half the truth: a deleted
// ticket's status is still "open", so rendering State() printed dead blockers as
// "[open]" and sent readers hunting for an id that appears in no listing. A
// frozen issue's status describes work nobody may do, which is why the retention
// name replaces it rather than joining it.
//
// The close reason joins the status because "closed" alone is a directional
// lie: dropping it can only make a body of work look MORE finished than it is,
// never less, so a wontfix declination read as completed work and an agent
// acted on the wrong picture (promptctl-output-p60y). A `lit done` close
// records no reason and renders the bare word — the absence is the data, not a
// fifth member of the sealed set.
// [LAW:one-source-of-truth] Every surface that names a referenced issue's state
// reads this, so the epic plan's markers and the relationship groups cannot
// disagree about one ticket; Frozen and RetentionName stay the sole owners of
// "out of the flow" and of the words for it.
func issueStanding(issue model.Issue) string {
	if model.Frozen(issue.Retention()) {
		return model.RetentionName(issue.Retention())
	}
	return string(issue.State()) + resolutionSuffix(issue.ResolutionValue())
}

// resolutionSuffix renders a recorded close reason as the tail of a standing
// word — ":wontfix" — and "" for a close that recorded none, which is the
// identity for the concatenation above rather than a case its caller steers
// around. [LAW:dataflow-not-control-flow] absence is a value here, exactly as
// in formatEpicLine and laneTag.
//
// ":" and not "+": the "+" in formatIssueState means both lifecycle axes are
// true of one ticket ("open+deleted"), while a resolution is the closed state's
// own payload — it exists on no other state — so it refines the word rather
// than standing beside it. [LAW:comments-carry-meaning] the distinction is the
// notation's whole meaning and is invisible in the code.
func resolutionSuffix(resolution *model.Resolution) string {
	if resolution == nil {
		return ""
	}
	return ":" + string(*resolution)
}

// derivedColumns carries the per-issue facts that cannot be read off the issue
// row — one cell per column above sourceIssue. Each field has exactly one
// producer, and the two producers sit at different rungs of the ladder:
// parentID comes from the canonical graph (storage.IssueRelations) so the list
// view never reinterprets edge semantics, and blocked comes from
// ClassifyReadiness so it cannot carry a shorter list than the annotation
// registry. Nothing else may write either field.
// [LAW:one-source-of-truth] It was named relationColumns while both cells were
// derived from relations, and that name is what made a dependency-edge `blocked`
// look like it belonged here.
//
// The zero value is the honest answer for an issue whose derived data was not
// loaded (no parent, not blocked), which is exactly what a nil map yields on
// lookup — and it is reachable only for a projection that named no derived
// column, because columnSourceFor makes the loader satisfy the maximum rung any
// selected column asks for.
type derivedColumns struct {
	parentID string
	blocked  bool
}

// The columns a projection can name — and the data each is computed from — are
// the column registry's to state; see columns.go.

func formatIssueColumns(issue model.Issue, columns []columnSpec, delimiter string, cells map[string]derivedColumns) string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		// Reading a nil map yields the zero derivedColumns — "-" for an issue
		// whose derived data wasn't loaded — so no renderer needs a guard.
		values = append(values, column.render(issue, cells[issue.ID]))
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
	if err := printIssueGroup(w, "parent", optionalGroup(detail.Parent)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "siblings", liveIssues(detail.Siblings)); err != nil {
		return err
	}
	// Surface the redirect at the close moment too: closing as duplicate/
	// superseded, the freshest fact is where the work went. Same store-shaped
	// IssueDetail, same omit-when-empty group as lit show.
	if err := printIssueGroup(w, "redirect", optionalGroup(detail.RedirectTarget)); err != nil {
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
	// [LAW:one-source-of-truth] Frozen owns the predicate and RetentionName the
	// word; this surface picks only the composition — it appends where
	// issueStanding replaces, because a ticket's own line carries both axes
	// ("open+deleted") while a line naming another ticket carries one standing.
	if model.Frozen(issue.Retention()) {
		parts = append(parts, model.RetentionName(issue.Retention()))
	}
	return strings.Join(parts, "+")
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
