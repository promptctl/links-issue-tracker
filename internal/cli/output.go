package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func printIssueSummary(w io.Writer, v any) error {
	issue := v.(model.Issue)
	_, err := fmt.Fprintf(w, "%s [%s/%s/%s/P%d] %s%s\n", issue.ID, formatIssueState(issue), issue.IssueType, issue.Topic, issue.Priority, issue.Title, formatLabels(issue.Labels))
	return err
}

func printIssueTable(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(resolved, "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, resolved, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, resolved, " | ")); err != nil {
			return err
		}
	}
	return nil
}

func printIssueDetail(w io.Writer, detail model.IssueDetail) error {
	issue := detail.Issue
	if _, err := fmt.Fprintf(w, "%s\n%s\n\nstatus: %s\ntype: %s\ntopic: %s\npriority: %d\nassignee: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, issue.Title, issue.Status, issue.IssueType, issue.Topic, issue.Priority, emptyDash(issue.Assignee), emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(issue.ArchivedAt), formatOptionalTime(issue.DeletedAt)); err != nil {
		return err
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
	if err := printIssueGroup(w, "children", detail.Children); err != nil {
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
	if len(detail.History) > 0 {
		if _, err := fmt.Fprintln(w, "\nhistory:"); err != nil {
			return err
		}
		for _, event := range detail.History {
			if _, err := fmt.Fprintf(w, "- [%s] %s %s (%s -> %s)\n", event.CreatedBy, event.Action, strings.ReplaceAll(event.Reason, "\n", "\\n"), emptyDash(event.FromStatus), emptyDash(event.ToStatus)); err != nil {
				return err
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
		if _, err := fmt.Fprintf(w, "- %s %s\n", issue.ID, issue.Title); err != nil {
			return err
		}
	}
	return nil
}

func formatIssueColumns(issue model.Issue, columns []string, delimiter string) string {
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
			values = append(values, strconv.Itoa(issue.Priority))
		case "title":
			values = append(values, issue.Title)
		case "assignee":
			values = append(values, emptyDash(issue.Assignee))
		case "labels":
			values = append(values, emptyDash(strings.Join(issue.Labels, ",")))
		case "updated_at":
			values = append(values, issue.UpdatedAt.Format(time.RFC3339))
		case "created_at":
			values = append(values, issue.CreatedAt.Format(time.RFC3339))
		}
	}
	return strings.Join(values, delimiter)
}

func resolveColumns(columns []string) []string {
	if len(columns) == 0 {
		// [LAW:dataflow-not-control-flow] Default listing still flows through the same projection path.
		return []string{"id", "state", "topic", "title"}
	}
	valid := map[string]struct{}{
		"id": {}, "state": {}, "type": {}, "topic": {}, "priority": {}, "title": {}, "assignee": {}, "labels": {}, "updated_at": {}, "created_at": {},
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

func printLabels(w io.Writer, v any) error {
	labels := v.([]string)
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

func formatIssueState(issue model.Issue) string {
	parts := []string{issue.Status}
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
