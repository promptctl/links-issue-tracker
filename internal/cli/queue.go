package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
)

// runQueue renders the pull order: every workable item that is not blocked, in
// canonical rank order, one terse row each with no preamble and no per-row
// context. It is the third projection over gatherWorkableAnnotated, and its
// question is the discriminator that separates it from the other two:
// `backlog` answers "why is the queue shaped this way" (blocked items inline,
// full per-row epic/dependency context), `ready` answers "what should the next
// agent work on" (coaching prose, capped, sectioned), and `queue` answers "what
// is the rank-ordered pull sequence I am shaping with lit rank" (blocked
// dropped, terse, uncapped).
//
// [LAW:one-source-of-truth] Ranking is canonical in the store; queue is a
// projection over the shared pipeline, never a second pull-order computation.
// [LAW:single-enforcer] "What is workable, in what order" comes entirely from
// gatherWorkableAnnotated; queue adds only the not-blocked filter and a terse
// render. The canonical rank order is used unmodified — no ready-specific
// re-sort.
func runQueue(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("queue")
	assignee := fs.String("assignee", "", "Filter by assignee")
	issueType := fs.String("type", "", "Filter by issue type")
	status := fs.String("status", "", "Filter by status: open|in_progress (closed excludes everything)")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	limit := fs.Int("limit", 0, "Limit results")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit queue [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--limit N] [--columns ...] [--json]")
	}
	rf := workableFilter{
		Assignee:  strings.TrimSpace(*assignee),
		IssueType: strings.TrimSpace(*issueType),
		Status:    strings.TrimSpace(*status),
		Labels:    splitCSV(*labels),
	}
	annotated, _, err := gatherWorkableAnnotated(ctx, ap, rf)
	if err != nil {
		return err
	}
	pullable := filterPullable(annotated)
	pullable = applyLimit(pullable, *limit)
	columns := parseColumns(*columnsExpr)
	return printValue(stdout, pullable, *jsonOut, func(w io.Writer, v any) error {
		return printQueueOutput(w, columns, v.([]annotation.AnnotatedIssue))
	})
}

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
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, resolved, "  "))
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
