package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// backlogPreamble explains what the backlog view is so an agent reading top to
// bottom understands the ordering story before scanning rows. The text leans
// on the same vocabulary `lit ready` uses ("backlog") but stresses what makes
// this view different: nothing is hidden, blocked items keep their ranked
// position, and the surrounding context (epic, depends-on, blocking reasons)
// is visible so the order is auditable.
const backlogPreamble = `This is the full backlog in priority/rank order — every workable item, blocked or not.
Items at the top are ranked higher than items below them. Blocked items stay where they were ranked
so you can see WHY the queue is shaped this way, not just what is ready next.
Read every row: each carries its parent epic, dependencies, blocking reasons, and what closing it would unblock.
That context is the ordering rationale — the dependency graph IS the priority story.
Use 'lit next' to pick the top workable item; 'lit ready' to skip blocked items entirely.`

// runBacklog renders every workable leaf in canonical priority/rank order with
// blocking annotations inline. It reuses gatherWorkableAnnotated so the
// "workable set + order" definition cannot drift from `lit ready` / `lit next`;
// the only thing backlog does differently is skip the ready-specific blocking
// sort so blocked items remain interleaved at their ranked position.
//
// [LAW:single-enforcer] Shared workable pipeline; backlog adds presentation only.
// [LAW:dataflow-not-control-flow] Every row flows through the same render path;
// variability (in_progress suffix, blocking reasons, parent epic) lives in
// values on the annotated row, not in branches that skip operations.
func runBacklog(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("backlog")
	assignee := fs.String("assignee", "", "Filter by assignee")
	issueType := fs.String("type", "", "Filter by issue type")
	status := fs.String("status", "", "Filter by status: open|in_progress (closed excludes everything)")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	limit := fs.Int("limit", 0, "Limit results")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	fs.JSONFlag()
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit backlog [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--limit N] [--columns ...] [--json]"}
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
	annotated = applyLimit(annotated, *limit)
	columns := parseColumns(*columnsExpr)
	return printValue(stdout, annotated, func(w io.Writer, v any) error {
		return printBacklogOutput(w, columns, v.([]annotation.AnnotatedIssue))
	})
}

// printBacklogOutput renders the backlog as a numbered list with inline
// per-row context (parent epic, dependencies, blocking reasons, in-progress
// suffix, unblocks). Empty data flows through the same path — the "(backlog
// empty)" message is one path-end, not a branch around the rendering loop.
func printBacklogOutput(w io.Writer, columns []string, issues []annotation.AnnotatedIssue) error {
	resolved := resolveColumns(columns)
	if _, err := fmt.Fprintln(w, backlogPreamble); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, strings.Repeat("─", 80)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(issues) == 0 {
		if _, err := fmt.Fprintln(w, "(backlog empty)"); err != nil {
			return err
		}
		return nil
	}

	unblocksMap := buildUnblocksMap(issues)
	for i, entry := range issues {
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, resolved, "  "))
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if err := printBacklogContext(w, entry, unblocksMap); err != nil {
			return err
		}
	}
	return printRankInversions(w, issues)
}

// printBacklogContext prints the indented context block under a single
// backlog row. Every annotation kind has its own line shape so a reader
// can scan vertically and see exactly why an item sits where it does:
// "blocked: ..." surfaces non-dependency blockers, "depends on: ..." names
// open dependencies, "in_progress: ..." surfaces age/orphan status, and
// "unblocks: ..." shows leverage.
func printBacklogContext(w io.Writer, entry annotation.AnnotatedIssue, unblocksMap map[string][]string) error {
	const indent = "    "
	readiness := ClassifyReadiness(entry.Annotations)
	if entry.ParentEpic != nil {
		if _, err := fmt.Fprintf(w, "%sepic: %s  %s\n", indent, entry.ParentEpic.ID, entry.ParentEpic.Title); err != nil {
			return err
		}
	}
	if reasons := nonDependencyBlockingReasons(readiness); len(reasons) > 0 {
		if _, err := fmt.Fprintf(w, "%sblocked: %s\n", indent, strings.Join(reasons, "; ")); err != nil {
			return err
		}
	}
	if deps := readiness.DependencyIDs(); len(deps) > 0 {
		if _, err := fmt.Fprintf(w, "%sdepends on: %s\n", indent, strings.Join(deps, ", ")); err != nil {
			return err
		}
	}
	if entry.State() == model.StateInProgress {
		if _, err := fmt.Fprintf(w, "%sin_progress: %s\n", indent, inProgressSuffix(entry)); err != nil {
			return err
		}
	}
	if unblocks := unblocksMap[entry.ID]; len(unblocks) > 0 {
		if _, err := fmt.Fprintf(w, "%sunblocks: %s\n", indent, strings.Join(unblocks, ", ")); err != nil {
			return err
		}
	}
	return nil
}

// nonDependencyBlockingReasons formats the classified blocking reasons that
// aren't already represented by the "depends on:" line — missing-field and
// needs-design. Open dependencies are surfaced separately so a reader sees
// them as the concrete blocker IDs rather than a category label.
func nonDependencyBlockingReasons(readiness IssueReadiness) []string {
	var reasons []string
	for _, reason := range readiness.BlockingReasons() {
		switch reason.Kind {
		case annotation.MissingField:
			reasons = append(reasons, "missing "+reason.Detail)
		case annotation.NeedsDesign:
			reasons = append(reasons, "needs-design")
		}
	}
	return reasons
}
