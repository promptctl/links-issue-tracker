package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// backlogPreamble explains what the backlog view is so an agent reading top to
// bottom understands the ordering story before scanning rows. It stresses what
// makes this the full workable view: nothing is hidden, blocked items keep their
// ranked position, and the surrounding context (epic, depends-on, blocking
// reasons) is visible so the order is auditable.
//
// It also has to state how the view says a group-scoped fact, because the view
// only says it once. An agent that reads "each row carries its parent epic" and
// then finds nine of ten siblings without an epic line will conclude those nine
// have no epic. [FRAMING:representation] The preamble is a map of the view and
// has to be redrawn whenever the view moves.
const backlogPreamble = `This is the full backlog in priority/rank order — every workable item, blocked or not.
Items at the top are ranked higher than items below them. Blocked items stay where they were ranked
so you can see WHY the queue is shaped this way, not just what is ready next.
Read every row: each carries its dependencies, blocking reasons, and what closing it would unblock.
That context is the ordering rationale — the dependency graph IS the priority story.
An epic line and a claim line describe a whole run of rows and are printed once, on the row that
opens the run, so a row without one continues the run above it. 'blocked: earlier sibling X' names
the one row directly ahead of it in its lane, not every row ahead of it.
Rows claimed by another checkout show who holds them and how fresh, but claim visibility here is
just that — visibility; only 'lit next' routes by claim, serving this checkout's own lanes first.
Use 'lit next' to pick the top workable item to start.`

// printBacklogOutput renders the backlog as a numbered list with inline
// per-row context (parent epic, dependencies, blocking reasons, in-progress
// suffix, unblocks). Empty data flows through the same path — the "(backlog
// empty)" message is one path-end, not a branch around the rendering loop.
func printBacklogOutput(w io.Writer, columns []string, issues []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, cc claimContext) error {
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
	now := time.Now()
	var above backlogRun
	for i, entry := range issues {
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, resolved, "  ", nil))
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		lane := model.LaneOf(entry.Issue, details[entry.ID].Parent)
		group := above.advance(entry.ParentEpic, cc, lane, now)
		if err := printBacklogContext(w, entry, unblocksMap, group); err != nil {
			return err
		}
		above = group.run
	}
	return printRankInversions(w, issues)
}

// backlogRun is the group-scoped context the rows above already put on screen:
// the epic that was named and the lane whose claim was described. Neither fact
// belongs to a row — every child of an epic shares one epic line, every member
// of a lane shares one claim line — so a list that derives them per row prints
// the same sentence verbatim under each of ten siblings and buries the lines
// that ARE per-row.
// [LAW:one-source-of-truth] The row that opens a run is where the fact is
// stated; the rows below it read it from there. The zero value has stated
// nothing, so row 1 always opens.
//
// Adjacency, not a set of every subject seen: a run that resumes further down
// the list states its facts again, because a reader who has scrolled past the
// first mention no longer has it on screen.
type backlogRun struct {
	epicID string
	lane   model.LaneID
}

// backlogRowContext is the group-scoped half of one row's context block,
// resolved against the run above it, together with the run it leaves behind.
// Returning both from one call is what keeps them honest: the run recorded is
// by construction the run just rendered, so "what is on screen" cannot drift
// from what was printed. [LAW:one-source-of-truth]
//
// The zero value of epic and claim carries two facts at once — "the row above
// said it" and "there is nothing to say" — because both are the same
// instruction to the printer, which is therefore unconditional over its data.
// [LAW:dataflow-not-control-flow]
type backlogRowContext struct {
	epic  string
	claim string
	run   backlogRun
}

// advance resolves what a row under epic, in lane, states for itself given the
// run above it. Both facts are looked up on every row; only the values differ.
func (above backlogRun) advance(epic *annotation.ParentEpicRef, cc claimContext, lane model.LaneID, now time.Time) backlogRowContext {
	// formatClaimLine answers "" for an unclaimed lane, which is already the
	// printer's "no line" value — the discarded bool restates the empty string
	// rather than carrying a signal this drops.
	claim, _ := formatClaimLine(cc, lane, now)
	here := backlogRun{epicID: epicID(epic), lane: lane}
	return backlogRowContext{
		epic:  openingRun(backlogEpicLine(epic), here.epicID, above.epicID),
		claim: openingRun(claim, here.lane.String(), above.lane.String()),
		run:   here,
	}
}

// backlogEpicLine is what a row that OPENS an epic run states. A run under no
// epic says so out loud, because suppressing a repeat costs the reader the
// thing absence used to mean: before the runs existed every row carried its
// own epic line, so a row without one had no epic, full stop. Leave the
// no-epic run silent and that one blank now means both "continues the epic
// above" and "has none" — an absence shaped exactly like an answer — and a
// standalone ticket that happens to sort under an epic's last child reads as
// part of it. sortByCompositeRank interleaves them by rank, so that adjacency
// is routine, and in a real backlog most rows have no epic at all.
// [FRAMING:representation]
//
// The zero-value run's empty epicID IS the no-epic subject, so a list that
// opens with standalone rows opens already inside that run and says nothing.
// That is right rather than merely convenient: the line exists to stop a row
// being read as part of the epic above it, and the first row has none.
func backlogEpicLine(epic *annotation.ParentEpicRef) string {
	if line := formatEpicLine(epic); line != "" {
		return line
	}
	return "epic: none"
}

// openingRun returns value when subject differs from the subject the row above
// stated, and the zero value when the run continues. Suppressing a repeated
// epic line and a repeated claim line is one behavior over two data types, so
// it is one function. [LAW:one-type-per-behavior]
func openingRun[T any](value T, subject, above string) T {
	if subject == above {
		var restated T
		return restated
	}
	return value
}

// printBacklogContext prints the indented context block under a single
// backlog row. Every annotation kind has its own line shape so a reader
// can scan vertically and see exactly why an item sits where it does:
// "blocked: ..." surfaces non-dependency blockers, "depends on: ..." names
// open dependencies, "in_progress: ..." surfaces age/orphan status, and
// "unblocks: ..." shows leverage.
func printBacklogContext(w io.Writer, entry annotation.AnnotatedIssue, unblocksMap map[string][]string, group backlogRowContext) error {
	readiness := ClassifyReadiness(entry.Annotations)
	if err := printContextLine(w, contextIndent, group.epic); err != nil {
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
	if err := printContextLine(w, contextIndent, group.claim); err != nil {
		return err
	}
	return printIDListLine(w, contextIndent, "unblocks", unblocksMap[entry.ID])
}

// nonDependencyBlockingReasons formats the classified blocking reasons that
// aren't already represented by the "depends on:" line. Open dependencies are
// surfaced separately so a reader sees them as the concrete blocker IDs rather
// than a category label; the sibling gate names its blocker too, because the
// remedy differs from a declared edge's — close, re-rank, or re-lane the
// sibling, never `lit dep`.
//
// Total over RoleBlocking, and the default is why. Of the registry's four
// blocking kinds this switch phrased two and left OpenDependency to the line
// below, but EarlierSiblingPending — registered after the switch was written —
// fell through into silence: the backlog said "top of the queue, nothing
// blocking" while routing skipped the row, which is the divergence
// links-claims-gxxw was filed for. A code gap that hides a blocker must be
// louder than the blocker it hides — the call ClassifyReadiness makes one seam
// over. [LAW:no-silent-failure] [LAW:one-source-of-truth] the registry is the
// single authority on what blocks; rendering may not carry a shorter list.
func nonDependencyBlockingReasons(readiness IssueReadiness) []string {
	var reasons []string
	for _, reason := range readiness.BlockingReasons() {
		switch reason.Kind {
		case annotation.OpenDependency:
			// carried by the "depends on:" line, as concrete blocker IDs
		case annotation.MissingField:
			reasons = append(reasons, "missing "+reason.Detail)
		case annotation.NeedsDesign:
			reasons = append(reasons, "needs-design")
		case annotation.EarlierSiblingPending:
			reasons = append(reasons, "earlier sibling "+reason.Detail+" still open")
		default:
			panic("nonDependencyBlockingReasons: blocking kind with no phrasing: " + reason.Kind.String())
		}
	}
	return reasons
}
