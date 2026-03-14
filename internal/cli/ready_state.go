package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

type notReadyIssue struct {
	Issue  model.Issue `json:"issue"`
	Reason string      `json:"reason"`
}

type readyCommandOutput struct {
	Ready    []model.Issue   `json:"ready"`
	NotReady []notReadyIssue `json:"not_ready"`
}

func deriveReadySections(ctx context.Context, st *store.Store, issues []model.Issue, requiredFields []string) ([]model.Issue, []notReadyIssue, error) {
	ready := make([]model.Issue, 0, len(issues))
	notReady := make([]notReadyIssue, 0, len(issues))
	for _, issue := range issues {
		reason, err := deriveNotReadyReason(ctx, st, issue, requiredFields)
		if err != nil {
			return nil, nil, err
		}
		if reason == "" {
			ready = append(ready, issue)
			continue
		}
		notReady = append(notReady, notReadyIssue{Issue: issue, Reason: reason})
	}
	return ready, notReady, nil
}

func deriveNotReadyReason(ctx context.Context, st *store.Store, issue model.Issue, requiredFields []string) (string, error) {
	reasons := make([]string, 0, len(requiredFields)+1)
	fields, err := issueFieldValues(issue)
	if err != nil {
		return "", err
	}
	for _, field := range requiredFields {
		value, ok := fields[field]
		if !ok {
			reasons = append(reasons, fmt.Sprintf("Field %s not found", field))
			continue
		}
		if !isRequiredFieldSet(value) {
			reasons = append(reasons, fmt.Sprintf("Field %s not set", field))
		}
	}
	// [LAW:dataflow-not-control-flow] Dependency lookup runs for every issue and yields data-driven readiness.
	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		return "", err
	}
	if blockers := openDependencyIDs(detail.DependsOn); len(blockers) > 0 {
		reasons = append(reasons, fmt.Sprintf("Blocked by ticket %s", blockers[0]))
	}
	return strings.Join(reasons, "; "), nil
}

func issueFieldValues(issue model.Issue) (map[string]any, error) {
	payload, err := json.Marshal(issue)
	if err != nil {
		return nil, fmt.Errorf("marshal issue fields: %w", err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(payload, &values); err != nil {
		return nil, fmt.Errorf("unmarshal issue fields: %w", err)
	}
	return values, nil
}

func isRequiredFieldSet(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func openDependencyIDs(dependsOn []model.Issue) []string {
	blockers := make([]string, 0, len(dependsOn))
	for _, dependency := range dependsOn {
		if dependency.Status != "closed" {
			blockers = append(blockers, dependency.ID)
		}
	}
	sort.Strings(blockers)
	return blockers
}

func applyReadyLimit(ready []model.Issue, notReady []notReadyIssue, limit int) ([]model.Issue, []notReadyIssue) {
	if limit <= 0 {
		return ready, notReady
	}
	if len(ready) >= limit {
		return ready[:limit], []notReadyIssue{}
	}
	remaining := limit - len(ready)
	if len(notReady) > remaining {
		return ready, notReady[:remaining]
	}
	return ready, notReady
}

func printReadySections(w io.Writer, format string, columns []string, ready []model.Issue, notReady []notReadyIssue) error {
	if _, err := fmt.Fprintln(w, "Ready"); err != nil {
		return err
	}
	if err := printReadySectionRows(w, format, columns, ready); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Not Ready"); err != nil {
		return err
	}
	return printNotReadySectionRows(w, format, columns, notReady)
}

func printReadySectionRows(w io.Writer, format string, columns []string, ready []model.Issue) error {
	if len(ready) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	switch format {
	case "", "lines":
		return printIssueLines(w, ready, columns)
	case "table":
		return printIssueTable(w, ready, columns)
	default:
		return fmt.Errorf("unsupported --format %q", format)
	}
}

func printNotReadySectionRows(w io.Writer, format string, columns []string, notReady []notReadyIssue) error {
	if len(notReady) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	switch format {
	case "", "lines":
		return printNotReadyLines(w, notReady, columns)
	case "table":
		return printNotReadyTable(w, notReady, columns)
	default:
		return fmt.Errorf("unsupported --format %q", format)
	}
}

func printNotReadyLines(w io.Writer, issues []notReadyIssue, columns []string) error {
	resolved := resolveColumns(columns)
	for _, entry := range issues {
		base := formatIssueColumns(entry.Issue, resolved, " | ")
		if _, err := fmt.Fprintf(w, "%s | %s\n", base, entry.Reason); err != nil {
			return err
		}
	}
	return nil
}

func printNotReadyTable(w io.Writer, issues []notReadyIssue, columns []string) error {
	resolved := resolveColumns(columns)
	headers := append(append([]string{}, resolved...), "reason")
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(headers, "\t"))); err != nil {
		return err
	}
	for _, entry := range issues {
		base := formatIssueColumns(entry.Issue, resolved, "\t")
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", base, entry.Reason); err != nil {
			return err
		}
	}
	return tw.Flush()
}
