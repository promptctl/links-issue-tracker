package cli

import (
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
)

// filterPullable keeps the rows an agent can pull now: workable and gated by no
// readiness blocker. "Blocked" is decided once in ClassifyReadiness and read
// here through the typed result, so the pull-order set cannot drift from what
// `lit ready` treats as blocked. in_progress items that are not blocked are
// kept — they hold rank positions and are part of the shape being verified.
func filterPullable(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue {
	out := make([]annotation.AnnotatedIssue, 0, len(rows))
	for _, row := range rows {
		if !ClassifyReadiness(row.Annotations).IsReady() {
			continue
		}
		out = append(out, row)
	}
	return out
}

// printQueueOutput renders the pull order as a numbered rank-position list, one
// row per pullable item. The number is the position in the pull sequence — the
// thing `lit rank` shapes — so the order is verifiable by reading it. Empty
// data flows to the "(queue empty)" path-end, not a branch around the loop.
func printQueueOutput(w io.Writer, columns []string, issues []annotation.AnnotatedIssue) error {
	resolved := resolveColumns(columns)
	if len(issues) == 0 {
		_, err := fmt.Fprintln(w, "(queue empty)")
		return err
	}
	for i, entry := range issues {
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, resolved, "  ", nil))
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
