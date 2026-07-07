package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
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
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, resolved, "  ", nil))
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
	readiness := ClassifyReadiness(entry.Annotations)
	if err := printEpicLine(w, contextIndent, entry.ParentEpic); err != nil {
		return err
	}
	// "blocked:" joins reasons with "; " (not IDs with ", "), so it is its own
	// line shape rather than the shared printIDListLine. [LAW:carrying-cost]
	if reasons := nonDependencyBlockingReasons(readiness); len(reasons) > 0 {
		if _, err := fmt.Fprintf(w, "%sblocked: %s\n", contextIndent, strings.Join(reasons, "; ")); err != nil {
			return err
		}
	}
	if err := printIDListLine(w, contextIndent, "depends on", readiness.DependencyIDs()); err != nil {
		return err
	}
	if entry.State() == model.StateInProgress {
		if _, err := fmt.Fprintf(w, "%sin_progress: %s\n", contextIndent, inProgressSuffix(entry)); err != nil {
			return err
		}
	}
	return printIDListLine(w, contextIndent, "unblocks", unblocksMap[entry.ID])
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
