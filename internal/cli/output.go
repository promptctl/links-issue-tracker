package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func printIssueSummary(w io.Writer, issue model.Issue) error {
	_, err := fmt.Fprintf(w, "%s [%s/%s/%s/%s] %s%s\n", issue.ID, formatIssueState(issue), issue.IssueType, issue.Topic, model.PriorityName(issue.Priority), issue.Title, formatLabels(issue.Labels))
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
	if _, err := fmt.Fprintf(w, "%s\n%s\n\ntype: %s\ntopic: %s\npriority: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, issue.Title, issue.IssueType, issue.Topic, model.PriorityName(issue.Priority), emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(issue.ArchivedAt), formatOptionalTime(issue.DeletedAt)); err != nil {
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
	// "unblocks:" surfaces the same leverage signal `lit ready` shows inline:
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
	// Siblings sit beside children so the parent-child neighborhood reads as one
	// block: this ticket's children, then its peers under the shared parent.
	if err := printIssueGroup(w, "siblings", detail.Siblings); err != nil {
		return err
	}
	if err := printIssueGroup(w, "depends_on", detail.DependsOn); err != nil {
		return err
	}
	if err := printIssueGroup(w, "blocks", detail.Blocks); err != nil {
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
	if len(detail.Events) > 0 {
		if _, err := fmt.Fprintln(w, "\nhistory:"); err != nil {
			return err
		}
		for _, event := range detail.Events {
			action := event.Action
			if action == "" {
				action = "update"
			}
			if _, err := fmt.Fprintf(w, "- [%s] %s %s\n", event.Actor, action, strings.ReplaceAll(event.Reason, "\n", "\\n")); err != nil {
				return err
			}
			for _, change := range event.Changes {
				if _, err := fmt.Fprintf(w, "    %s: %s → %s\n", change.Field, emptyDash(change.From), emptyDash(change.To)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func printIssueGroup(w io.Writer, label string, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", label); err != nil {
		return err
	}
	for _, issue := range issues {
		// State() is shape-agnostic: leaves return their owned status,
		// containers return state derived from children. The agent reads the
		// referenced issue's state inline without a second 'lit show'.
		if _, err := fmt.Fprintf(w, "- %s [%s] %s\n", issue.ID, issue.State(), issue.Title); err != nil {
			return err
		}
	}
	return nil
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
			values = append(values, issue.IssueType)
		case "topic":
			values = append(values, issue.Topic)
		case "priority":
			values = append(values, model.PriorityName(issue.Priority))
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

// isLiveIssue reports whether an issue is still in play — open or in_progress,
// and neither archived nor deleted. This is the single definition of "live
// adjacency" shared by the now-unblocked-dependents line and the open-siblings
// filter, so the two surfaces cannot drift on what counts as actionable.
// [LAW:single-enforcer] Liveness decided once, here.
func isLiveIssue(issue model.Issue) bool {
	return issue.State() != model.StateClosed && issue.ArchivedAt == nil && issue.DeletedAt == nil
}

// openUnblockIDs returns the IDs of issues from blocks that are still live —
// the set this issue's closure would actually unblock from a "ready" perspective.
func openUnblockIDs(blocks []model.Issue) []string {
	ids := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if !isLiveIssue(b) {
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
		if isLiveIssue(issue) {
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
	if issue.ArchivedAt != nil {
		parts = append(parts, "archived")
	}
	if issue.DeletedAt != nil {
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
