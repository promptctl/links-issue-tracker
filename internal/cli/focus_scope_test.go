package cli

import (
	"bytes"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// rowPosition reads the backlog's own printed numbering rather than counting
// matches itself: the number in the output is what a reader acts on, and a
// helper that recomputed it could agree with a renderer that had stopped
// agreeing with the list. [FRAMING:representation]
func rowPosition(t *testing.T, text, issueID string) int {
	t.Helper()
	numbered := regexp.MustCompile(`(?m)^\s*(\d+)\.\s+` + regexp.QuoteMeta(issueID) + `\b`)
	match := numbered.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("no numbered backlog row for %s in:\n%s", issueID, text)
	}
	pos, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("unparseable row number %q for %s", match[1], issueID)
	}
	return pos
}

// runNextRowArgs drives `lit next` with flags and narrows to the row it served,
// so a flag's effect is observed through the command an agent actually runs.
func (h readyTestHarness) runNextRowArgs(args ...string) annotation.AnnotatedIssue {
	h.t.Helper()
	text := h.runNextText(args...)
	rows, _, _, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	for _, row := range rows {
		if strings.Contains(text, row.ID) {
			return row
		}
	}
	h.t.Fatalf("`lit next %v` named no gathered row:\n%s", args, text)
	return annotation.AnnotatedIssue{}
}

// The focus scope is a membership rule, and the way a membership rule fails is
// by being written to recognize the shape it expects instead of to reject every
// shape it does not. So the accept/reject set is pinned here as DATA — one row
// per (scope state, row state) pair — rather than as an assertion derived from
// the predicate, which would restate the implementation and pass for any
// implementation, including a wrong one.
func TestFocusScopeHoldsExactly(t *testing.T) {
	onPath := annotation.AnnotatedIssue{Annotations: []annotation.Annotation{{Kind: annotation.FocusPath, Message: "goal-1"}}}
	offPath := annotation.AnnotatedIssue{Annotations: []annotation.Annotation{{Kind: annotation.OpenDependency, Message: "dep"}}}
	bare := annotation.AnnotatedIssue{}

	cases := []struct {
		name  string
		scope focusScope
		row   annotation.AnnotatedIssue
		want  bool
	}{
		// The unfocused workspace is the zero state, and its scope is the whole
		// queue. A scope that held nothing here would empty every view the day
		// nobody focused anything.
		{"unfocused holds an annotated row", focusScope{}, onPath, true},
		{"unfocused holds an off-path row", focusScope{}, offPath, true},
		{"unfocused holds a bare row", focusScope{}, bare, true},

		// Focused: membership is the FocusPath annotation and nothing else.
		{"focused holds the path row", focusScope{goals: []string{"goal-1"}}, onPath, true},
		{"focused rejects an annotated off-path row", focusScope{goals: []string{"goal-1"}}, offPath, false},
		{"focused rejects a bare row", focusScope{goals: []string{"goal-1"}}, bare, false},

		// Several goals is one scope, not one scope per goal: a row on ANY
		// goal's path is in. The annotation names the goal that reached it, and
		// membership must not start depending on WHICH.
		{"two goals hold a row reached by either", focusScope{goals: []string{"goal-1", "goal-2"}}, onPath, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.holds(tc.row); got != tc.want {
				t.Fatalf("holds() = %v, want %v", got, tc.want)
			}
		})
	}
}

// partition returns BOTH halves because the excluded half is what lets a view
// tell "there is no work" from "there is no work on your path". A partition
// that dropped it would type-check and read fine.
func TestFocusScopePartitionKeepsBothHalvesInOrder(t *testing.T) {
	row := func(id string, path bool) annotation.AnnotatedIssue {
		out := annotation.AnnotatedIssue{}
		out.ID = id
		if path {
			out.Annotations = []annotation.Annotation{{Kind: annotation.FocusPath, Message: "g"}}
		}
		return out
	}
	rows := []annotation.AnnotatedIssue{row("a", false), row("b", true), row("c", false), row("d", true)}

	inScope, excluded := focusScope{goals: []string{"g"}}.partition(rows)
	if got := rowIDsJoined(inScope); got != "b,d" {
		t.Fatalf("in-scope ids = %q, want %q", got, "b,d")
	}
	if got := rowIDsJoined(excluded); got != "a,c" {
		t.Fatalf("excluded ids = %q, want %q", got, "a,c")
	}

	// --all resolves to the unfocused scope, so everything is in and the
	// excluded half is empty — not "everything is excluded".
	inScope, excluded = focusScope{goals: []string{"g"}}.scopeFor(true).partition(rows)
	if got := rowIDsJoined(inScope); got != "a,b,c,d" {
		t.Fatalf("--all in-scope ids = %q, want %q", got, "a,b,c,d")
	}
	if len(excluded) != 0 {
		t.Fatalf("--all excluded = %v, want none", rowIDsJoined(excluded))
	}
}

func rowIDsJoined(rows []annotation.AnnotatedIssue) string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return strings.Join(out, ",")
}

// The ticket's headline, as a test: `lit rank --top` is honored by the store and
// `lit ls` shows it first, while `lit backlog` put it at position 39 because
// every focus-path row was hoisted above it. The view now answers over the
// scope, so the rank a caller sets is the rank the view shows — and the off-path
// row that used to sink silently is named as withheld instead.
func TestTopRankReachesTheTopOfTheFocusedView(t *testing.T) {
	h := newReadyTestHarness(t)

	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task",
	})
	early := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Path step one", Topic: "goal", IssueType: "task",
	})
	late := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Path step two", Topic: "goal", IssueType: "task",
	})
	h.addDependency(goal.ID, early.ID)
	h.addDependency(goal.ID, late.ID)
	h.setLabels(goal.ID, FocusLabel)

	// Ranked to the top of the view it is aimed at, and it lands there.
	var stdout bytes.Buffer
	if err := runRank(h.ctx, &stdout, h.ap, []string{late.ID, "--top"}); err != nil {
		t.Fatalf("runRank(--top) error = %v", err)
	}
	text := h.runWorkableText()
	if pos := rowPosition(t, text, late.ID); pos != 1 {
		t.Fatalf("`lit rank --top` then `lit backlog`: %s at position %d, want 1\n%s", late.ID, pos, text)
	}
}

// A focused backlog lists the path and says so, naming the goal, the count it
// withheld, and the flag that lifts the scope. The wants are the literal
// sentences: a test that rebuilt them from focusNotice would pass whatever
// focusNotice said, including nothing.
func TestFocusedBacklogNamesItsScopeAndItsEscape(t *testing.T) {
	h := newReadyTestHarness(t)

	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task",
	})
	prereq := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq", Topic: "goal", IssueType: "task",
	})
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated", Topic: "noise", IssueType: "task",
	})
	h.addDependency(goal.ID, prereq.ID)

	// Unfocused: the view states its own completeness, and every row is there.
	text := h.runWorkableText()
	if want := "Nothing is hidden: every workable item is listed."; !strings.Contains(text, want) {
		t.Fatalf("unfocused backlog missing %q\n%s", want, text)
	}
	if !strings.Contains(text, off.ID) {
		t.Fatalf("unfocused backlog missing off-path row %s\n%s", off.ID, text)
	}

	h.setLabels(goal.ID, FocusLabel)

	// Focused: scoped, and it says exactly what it withheld and how to get it.
	text = h.runWorkableText()
	want := "Focused on " + goal.ID + " — listing only its unfinished prerequisite path, in rank order; 1 workable row(s) off that path are not shown (`lit backlog --all` for the whole queue)."
	if !strings.Contains(text, want) {
		t.Fatalf("focused backlog missing notice %q\n%s", want, text)
	}
	if strings.Contains(text, off.ID) {
		t.Fatalf("focused backlog must not list off-path row %s\n%s", off.ID, text)
	}
	if !strings.Contains(text, prereq.ID) {
		t.Fatalf("focused backlog missing path row %s\n%s", prereq.ID, text)
	}

	// --all lifts the scope and says that it did, so a reader cannot mistake
	// this run's whole queue for an unfocused workspace.
	text = h.runWorkableText("--all")
	if want := "Focus is on " + goal.ID + "; this run bypassed it and lists the whole queue."; !strings.Contains(text, want) {
		t.Fatalf("`--all` backlog missing notice %q\n%s", want, text)
	}
	if !strings.Contains(text, off.ID) {
		t.Fatalf("`--all` backlog missing off-path row %s\n%s", off.ID, text)
	}
}

// An empty focused view over a backlog that still holds off-path work is NOT an
// empty backlog. This is the cell a plain filter gets wrong: both facts render
// as "(backlog empty)", and an agent told its backlog is empty stops.
func TestFocusedBacklogSaysWhichEmptinessThisIs(t *testing.T) {
	h := newReadyTestHarness(t)

	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "bug",
	})
	h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated", Topic: "noise", IssueType: "task",
	})
	h.setLabels(goal.ID, FocusLabel)

	// --type task narrows every path row away while off-path work survives.
	text := h.runWorkableText("--type", "task")
	if want := "(nothing workable on the focus path — 1 row(s) off it, `lit backlog --all` to see them)"; !strings.Contains(text, want) {
		t.Fatalf("focused empty backlog missing %q\n%s", want, text)
	}
	if strings.Contains(text, "(backlog empty)") {
		t.Fatalf("focused view with off-path work must not claim the backlog is empty\n%s", text)
	}
}

// `lit next` must not quietly hand back an off-path ticket when the focus path
// has nothing startable. Serving one would be the silent substitution of a
// similar-looking query for the one asked — and the agent would never learn its
// focused goal was stuck.
func TestNextRefusesToSubstituteOffPathWorkForAStuckFocusPath(t *testing.T) {
	h := newReadyTestHarness(t)

	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task",
	})
	blocked := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq", Topic: "goal", IssueType: "task",
	})
	gate := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Design gate", Topic: "goal", IssueType: "task",
	})
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated ready work", Topic: "noise", IssueType: "task",
	})
	h.addDependency(goal.ID, blocked.ID)
	h.addDependency(blocked.ID, gate.ID)
	h.setLabels(gate.ID, NeedsDesignLabel)
	h.setLabels(goal.ID, FocusLabel)

	err := h.runNextErr()
	if err == nil {
		t.Fatal("next served a row, want the focus-path diagnostic")
	}
	var outcome NoWork
	if !errors.As(err, &outcome) {
		t.Fatalf("next error = %#v (%T), want NoWork", err, err)
	}
	got := outcome.Error()
	if want := "no ready work on the focus path"; !strings.HasPrefix(got, want) {
		t.Fatalf("NoWork.Error() = %q, want prefix %q", got, want)
	}
	if want := "`lit next --all`"; !strings.Contains(got, want) {
		t.Fatalf("NoWork.Error() = %q, must name the escape %q", got, want)
	}
	if !strings.Contains(got, off.ID) {
		t.Fatalf("NoWork.Error() = %q, must name the withheld row %s", got, off.ID)
	}

	// The exit code is the one `next` already uses for "ran correctly, nothing
	// to hand back". A focused dead end is that answer, not a new one.
	if code := ExitCode(err); code != ExitNoWork {
		t.Fatalf("exit code = %d, want ExitNoWork (%d)", code, ExitNoWork)
	}

	// And --all reaches the work the scope withheld.
	if row := h.runNextRowArgs("--all"); row.ID != off.ID {
		t.Fatalf("`lit next --all` = %q, want the off-path row %q", row.ID, off.ID)
	}
}

// The focus-scoped lead may say rows sit off the path. It may not say they are
// startable: withheldByScope stamps reachOffFocusPath on every excluded row
// without consulting capacityFor, because the kind records which question the
// run asked rather than a verdict about the row. A lead promising startable
// work off the path is therefore an assertion nothing checked — the same
// answer-shaped void links-cli-q7hg closed, one scope further out.
//
// The fixture makes that claim false and not merely unverified: every off-path
// row is itself blocked, so an agent sent to `lit next --all` finds nothing.
// The sibling test above covers the case where the withheld row IS ready, which
// is why the wrong lead survived it.
func TestFocusPathDeadEndDoesNotClaimOffPathWorkIsStartable(t *testing.T) {
	h := newReadyTestHarness(t)

	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task",
	})
	gate := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Design gate", Topic: "goal", IssueType: "task",
	})
	h.addDependency(goal.ID, gate.ID)
	h.setLabels(gate.ID, NeedsDesignLabel)
	h.setLabels(goal.ID, FocusLabel)

	// Off the path, and not startable either: blocked by its own open
	// prerequisite, which is likewise off the path and likewise gated.
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Off-path, blocked", Topic: "noise", IssueType: "task",
	})
	offGate := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Off-path blocker", Topic: "noise", IssueType: "task",
	})
	h.addDependency(off.ID, offGate.ID)
	h.setLabels(offGate.ID, NeedsDesignLabel)

	err := h.runNextErr()
	if err == nil {
		t.Fatal("next served a row, want the focus-path diagnostic")
	}
	var outcome NoWork
	if !errors.As(err, &outcome) {
		t.Fatalf("next error = %#v (%T), want NoWork", err, err)
	}
	got := outcome.Error()

	// Only the lead is under test. The per-row clauses legitimately say "not
	// startable" about rows whose capacity the walk DID read, and matching the
	// whole message would pass on that.
	lead, _, found := strings.Cut(got, ": ")
	if !found {
		t.Fatalf("NoWork.Error() = %q, want a lead ending in %q before the row clauses", got, ": ")
	}
	if strings.Contains(lead, "startable") {
		t.Fatalf("NoWork.Error() lead = %q — nothing off the focus path is startable in this fixture, and the walk never read those rows' capacity, so the lead may not claim it", lead)
	}

	// The same lead introduces two populations: goal and gate are ON the path and
	// were walked and rejected by step 4, while off and offGate were never
	// examined. A header claiming location for the whole list is false for one
	// half of it, and the on-path half is the news the agent has to act on.
	if strings.Contains(lead, "off that path") || strings.Contains(lead, "off the focus path") {
		t.Fatalf("NoWork.Error() lead = %q describes every row it introduces as off the focus path, but %s and %s are on it", lead, goal.ID, gate.ID)
	}
	for _, note := range []string{poolNotes[reachNotReady], poolNotes[reachOffFocusPath]} {
		if !strings.Contains(got, note) {
			t.Fatalf("NoWork.Error() = %q lists two populations but is missing the words one of them owns: %q", got, note)
		}
	}
	if !strings.Contains(got, gate.ID) {
		t.Fatalf("NoWork.Error() = %q must name the on-path gate %s — that is the row the agent has to act on", got, gate.ID)
	}

	// It still has to say the rows are there and how to reach them; refusing the
	// unchecked claim must not cost the reader the honest half of the answer.
	if !strings.Contains(got, off.ID) {
		t.Fatalf("NoWork.Error() = %q, must name the withheld row %s", got, off.ID)
	}
	if want := "`lit next --all`"; !strings.Contains(got, want) {
		t.Fatalf("NoWork.Error() = %q, must name the escape %q", got, want)
	}
}

// The regression this design is most exposed to: scoping the rows BEFORE
// routing would hide a lane this checkout already holds, and an agent with work
// in flight off the focused path would be told to start something else. Focus
// decides where a fresh session goes; it never decides whether your own work is
// still yours.
func TestFocusScopeNeverHidesThisCheckoutsOwnWork(t *testing.T) {
	h := newReadyTestHarness(t)

	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task",
	})
	onPath := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Path work", Topic: "goal", IssueType: "task",
	})
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "My in-flight work", Topic: "elsewhere", IssueType: "task",
	})
	h.addDependency(goal.ID, onPath.ID)

	// This checkout starts work off the path, then a goal is focused.
	h.transition(mine.ID, model.Start{Assignee: "tester"})
	h.setLabels(goal.ID, FocusLabel)

	if row := h.runNextRow(); row.ID != mine.ID {
		t.Fatalf("next = %q, want this checkout's own in-flight row %q — the focus scope must not reach step 1", row.ID, mine.ID)
	}
}
