package cli

import (
	"bytes"
	"errors"
	"reflect"
	"regexp"
	"slices"
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
	gathered, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	for _, row := range gathered.rows {
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
// per (scope state, row state, lane) triple — rather than as an assertion
// derived from the predicate, which would restate the implementation and pass
// for any implementation, including a wrong one.
func TestFocusScopeHoldsExactly(t *testing.T) {
	onPath := annotation.AnnotatedIssue{Annotations: []annotation.Annotation{{Kind: annotation.FocusPath, Message: "goal-1"}}}
	offPath := annotation.AnnotatedIssue{Annotations: []annotation.Annotation{{Kind: annotation.OpenDependency, Message: "dep"}}}
	bare := annotation.AnnotatedIssue{}

	laneIn := func(epic, key string) model.LaneID {
		parent := model.Issue{ID: epic, IssueType: model.TypeEpic}
		return model.LaneOf(model.Issue{ID: "any-member", Lane: key}, &parent)
	}
	soloLane := func(id string) model.LaneID { return model.LaneOf(model.Issue{ID: id}, nil) }

	focused := focusScope{goals: []string{"goal-1"}}
	// This checkout holds lane "held" of epic E, and the solo lane S.
	owned := focused.owning(ownWork{
		lanes: map[model.LaneID]bool{laneIn("E", "held"): true, soloLane("S"): true},
		epics: map[string]bool{"E": true},
	})
	unrelated := laneIn("F", "x")

	cases := []struct {
		name  string
		scope focusScope
		row   annotation.AnnotatedIssue
		lane  model.LaneID
		want  bool
	}{
		// The unfocused workspace is the zero state, and its scope is the whole
		// queue. A scope that held nothing here would empty every view the day
		// nobody focused anything.
		{"unfocused holds an annotated row", focusScope{}, onPath, unrelated, true},
		{"unfocused holds an off-path row", focusScope{}, offPath, unrelated, true},
		{"unfocused holds a bare row", focusScope{}, bare, unrelated, true},

		// Focused, owning nothing: membership is the FocusPath annotation and
		// nothing else.
		{"focused holds the path row", focused, onPath, unrelated, true},
		{"focused rejects an annotated off-path row", focused, offPath, unrelated, false},
		{"focused rejects a bare row", focused, bare, unrelated, false},
		{"focused rejects a row in a lane only another scope would own", focused, offPath, laneIn("E", "held"), false},

		// Several goals is one scope, not one scope per goal: a row on ANY
		// goal's path is in. The annotation names the goal that reached it, and
		// membership must not start depending on WHICH.
		{"two goals hold a row reached by either", focusScope{goals: []string{"goal-1", "goal-2"}}, onPath, unrelated, true},

		// Owning this checkout's work: its lanes and every lane of their epics
		// are in wherever the path runs, and nothing else is added.
		{"owned holds an off-path row in a held lane", owned, offPath, laneIn("E", "held"), true},
		{"owned holds a bare row in a held lane", owned, bare, laneIn("E", "held"), true},
		{"owned holds an off-path row in another lane of a held lane's epic", owned, offPath, laneIn("E", "other"), true},
		{"owned holds an off-path row in a held solo lane", owned, offPath, soloLane("S"), true},
		{"owned still holds the path row", owned, onPath, unrelated, true},
		{"owned rejects an off-path row in another epic", owned, offPath, unrelated, false},
		// A solo lane has no epic, so holding one must not make every other
		// solo lane ours through the empty epic id.
		{"owned rejects an off-path row in a solo lane it does not hold", owned, offPath, soloLane("T"), false},

		// --all is the unfocused scope, owned or not.
		{"--all holds an off-path row in an unrelated lane", focused.scopeFor(true).owning(owned.own), offPath, unrelated, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.holds(tc.row, tc.lane); got != tc.want {
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

	inScope, excluded := focusScope{goals: []string{"g"}}.partition(rows, nil)
	if got := rowIDsJoined(inScope); got != "b,d" {
		t.Fatalf("in-scope ids = %q, want %q", got, "b,d")
	}
	if got := rowIDsJoined(excluded); got != "a,c" {
		t.Fatalf("excluded ids = %q, want %q", got, "a,c")
	}

	// --all resolves to the unfocused scope, so everything is in and the
	// excluded half is empty — not "everything is excluded".
	inScope, excluded = focusScope{goals: []string{"g"}}.scopeFor(true).partition(rows, nil)
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

// `lit rank --top` is honored by the store and `lit ls` shows it first. The view
// answers over the scope, so the rank a caller sets is the rank the view shows —
// and the off-path row is named as withheld.
func TestTopRankReachesTheTopOfTheFocusedView(t *testing.T) {
	h := newReadyTestHarness(t)

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	early := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Path step one", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "early",
	})
	late := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Path step two", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "late",
	})
	h.addDependencyWithinEpic(goal.ID, early.ID)
	h.addDependencyWithinEpic(goal.ID, late.ID)
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

// --limit narrows the printed rows AFTER the scope partition, so the notice has
// to report it or the completeness sentence describes a list that was cut after
// it was written.
//
// The wants are literal sentences for the same reason the scope test uses them:
// a want rebuilt from focusNotice would agree with whatever focusNotice said.
func TestBacklogNoticeReportsTheLimitTrimNotJustTheScope(t *testing.T) {
	h := newReadyTestHarness(t)

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	prereqA := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq A", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "a",
	})
	prereqB := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq B", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "b",
	})
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	_ = h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated", Topic: "noise", IssueType: "task", ParentID: noise.ID,
	})
	h.addDependencyWithinEpic(goal.ID, prereqA.ID)
	h.addDependencyWithinEpic(goal.ID, prereqB.ID)

	// Unfocused and untrimmed: the strong completeness sentence is true, and the
	// trim clause is absent rather than reading "0 more row(s)".
	text := h.runWorkableText()
	if want := "Nothing is hidden: every workable item is listed."; !strings.Contains(text, want) {
		t.Fatalf("untrimmed backlog missing %q\n%s", want, text)
	}
	if strings.Contains(text, "--limit trimmed this run") {
		t.Fatalf("untrimmed backlog must not mention a trim\n%s", text)
	}

	// Unfocused and trimmed: four workable rows, two shown. The completeness
	// claim must be gone — it is the one sentence --limit falsifies outright.
	text = h.runWorkableText("--limit", "2")
	if strings.Contains(text, "Nothing is hidden") {
		t.Fatalf("`--limit 2` over 4 rows must not claim nothing is hidden\n%s", text)
	}
	if want := "Every workable item is in scope. --limit trimmed this run: 2 more workable row(s) are not shown."; !strings.Contains(text, want) {
		t.Fatalf("trimmed backlog missing %q\n%s", want, text)
	}

	// Focused AND trimmed: two numbers, two causes, two escapes. One combined
	// count would tell the reader rows are off the focus path when --limit is
	// what removed them.
	h.setLabels(goal.ID, FocusLabel)
	text = h.runWorkableText("--limit", "1")
	wantScope := "Focused on " + goal.ID + " — listing only its unfinished prerequisite path and this checkout's lanes with the rest of their epics, in rank order; 1 workable row(s) outside them are not shown (`lit backlog --all` for the whole queue)."
	if !strings.Contains(text, wantScope) {
		t.Fatalf("focused+trimmed backlog missing scope clause %q\n%s", wantScope, text)
	}
	if want := "--limit trimmed this run: 2 more workable row(s) are not shown."; !strings.Contains(text, want) {
		t.Fatalf("focused+trimmed backlog missing trim clause %q\n%s", want, text)
	}
}

// The trim count names the --limit narrowing and no other. It is measured
// across applyLimit alone, not across the whole keep→limit stretch, because the
// sentence it feeds names --limit as the cause: a count spanning both would
// report a view's own keep() drops as a trim the reader never asked for and
// cannot undo by dropping the flag.
//
// backlogView keeps every row, so the two spans are equal there and no existing
// want can tell them apart; this drives a view that filters, which is what keep
// being a function value is for. Under a count measured from the scope the
// dropped row surfaces as a phantom --limit trim on a run that passed no
// --limit at all.
func TestBacklogTrimCountNamesOnlyTheLimitNarrowing(t *testing.T) {
	h := newReadyTestHarness(t)

	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	for _, title := range []string{"Alpha", "Beta", "Gamma", "Delta"} {
		_ = h.createIssue(storage.CreateIssueInput{Prefix: "test",
			Title: title, Topic: "noise", IssueType: "task", ParentID: noise.ID,
		})
	}

	dropLast := backlogView
	dropLast.keep = func(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue {
		if len(rows) == 0 {
			return rows
		}
		return rows[:len(rows)-1]
	}

	var stdout bytes.Buffer
	if err := runWorkable(h.ctx, &stdout, h.ap, nil, dropLast); err != nil {
		t.Fatalf("runWorkable(dropLast) error = %v", err)
	}
	text := stdout.String()
	if strings.Contains(text, "--limit trimmed this run") {
		t.Fatalf("no --limit was passed, so the view's own keep() drop must not be reported as a trim\n%s", text)
	}
}

// A focused backlog lists the path and says so, naming the goal, the count it
// withheld, and the flag that lifts the scope. The wants are the literal
// sentences: a test that rebuilt them from focusNotice would pass whatever
// focusNotice said, including nothing.
func TestFocusedBacklogNamesItsScopeAndItsEscape(t *testing.T) {
	h := newReadyTestHarness(t)

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	prereq := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "prereq",
	})
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated", Topic: "noise", IssueType: "task", ParentID: noise.ID,
	})
	h.addDependencyWithinEpic(goal.ID, prereq.ID)

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
	want := "Focused on " + goal.ID + " — listing only its unfinished prerequisite path and this checkout's lanes with the rest of their epics, in rank order; 1 workable row(s) outside them are not shown (`lit backlog --all` for the whole queue)."
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

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "bug", ParentID: goalEpic.ID,
	})
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated", Topic: "noise", IssueType: "task", ParentID: noise.ID,
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

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	blocked := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "prereq",
	})
	gate := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Design gate", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "gate",
	})
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Unrelated ready work", Topic: "noise", IssueType: "task", ParentID: noise.ID,
	})
	h.addDependencyWithinEpic(goal.ID, blocked.ID)
	h.addDependencyWithinEpic(blocked.ID, gate.ID)
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
// startable: withheldByScope names every excluded row
// without consulting capacityFor, because the kind records which question the
// run asked rather than a verdict about the row. A lead promising startable
// work off the path is therefore an assertion nothing checked.
//
// The fixture makes that claim false and not merely unverified: every off-path
// row is itself blocked, so an agent sent to `lit next --all` finds nothing.
// The sibling test above covers the case where the withheld row IS ready.
func TestFocusPathDeadEndDoesNotClaimOffPathWorkIsStartable(t *testing.T) {
	h := newReadyTestHarness(t)

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	gate := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Design gate", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "gate",
	})
	h.addDependencyWithinEpic(goal.ID, gate.ID)
	h.setLabels(gate.ID, NeedsDesignLabel)
	h.setLabels(goal.ID, FocusLabel)

	// Off the path, and not startable either: blocked by its own open
	// prerequisite, which is likewise off the path and likewise gated.
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Off-path, blocked", Topic: "noise", IssueType: "task", ParentID: noise.ID, Lane: "off",
	})
	offGate := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Off-path blocker", Topic: "noise", IssueType: "task", ParentID: noise.ID, Lane: "off-gate",
	})
	h.addDependencyWithinEpic(off.ID, offGate.ID)
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
	for _, note := range []string{poolNotes[reachNotReady], offFocusPathNote} {
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

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	onPath := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Path work", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "path",
	})
	elsewhere := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Elsewhere epic", Topic: "elsewhere", IssueType: "epic",
	})
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "My in-flight work", Topic: "elsewhere", IssueType: "task", ParentID: elsewhere.ID,
	})
	h.addDependencyWithinEpic(goal.ID, onPath.ID)

	// This checkout starts work off the path, then a goal is focused.
	h.transition(mine.ID, model.Start{Assignee: "tester"})
	h.setLabels(goal.ID, FocusLabel)

	if row := h.runNextRow(); row.ID != mine.ID {
		t.Fatalf("next = %q, want this checkout's own in-flight row %q — the focus scope must not reach step 1", row.ID, mine.ID)
	}
}

// `lit backlog` answers over the scope `lit next` routes by, so this checkout's
// lanes and the rest of their epics are never missing from the list. The case
// it exists for is an agent back from a crash or a reset, running `lit backlog`
// to learn what it was doing: its in-flight ticket off the focused path must be
// on screen, together with the rest of the epic next would keep it in, and
// named as off the path for those lanes rather than counted among the rows the
// focus withheld. Driven through the CLI so the event the run leaves is the one
// asserted.
func TestFocusedBacklogListsThisCheckoutsOwnWork(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	goalEpic := newEpicInDir(t, repo, "Goal epic")
	goal := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", goalEpic, "--title", "Goal", "--topic", "goal"))
	ours := newEpicInDir(t, repo, "Our epic")
	mine := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", ours, "--lane", "mine", "--title", "In flight", "--topic", "ours"))
	sibling := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", ours, "--lane", "other", "--title", "Rest of our epic", "--topic", "ours"))
	elsewhere := newEpicInDir(t, repo, "Elsewhere")
	unrelated := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", elsewhere, "--title", "Not ours", "--topic", "elsewhere"))

	runCLIInDir(t, repo, "start", mine)
	runCLIInDir(t, repo, "label", "add", goal, FocusLabel)

	text := runCLIInDir(t, repo, "backlog")
	for _, id := range []string{goal, mine, sibling} {
		rowPosition(t, text, id)
	}
	if strings.Contains(text, unrelated) {
		t.Fatalf("focused backlog lists %s, which is neither on the path nor this checkout's work:\n%s", unrelated, text)
	}
	if want := "Focused on " + goal + " — listing only its unfinished prerequisite path and this checkout's lanes with the rest of their epics, in rank order; 1 workable row(s) outside them are not shown (`lit backlog --all` for the whole queue)."; !strings.Contains(text, want) {
		t.Fatalf("focused backlog missing %q — own work counted as withheld?\n%s", want, text)
	}
	ownClause := " Off that path but in this checkout's lanes or their epics: "
	if !strings.Contains(text, ownClause) {
		t.Fatalf("focused backlog does not name the own work it listed off the path:\n%s", text)
	}
	ev := lastEvent(t)
	if got, want := ev.Facts["backlog.focus_goals"], []any{goal}; !reflect.DeepEqual(got, want) {
		t.Errorf("backlog.focus_goals = %#v, want %#v", got, want)
	}
	if got, want := ev.Facts["backlog.off_path"], []any{map[string]any{"id": unrelated, "ticket": unrelated, "reach": "off_focus_path"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("backlog.off_path = %#v, want %#v", got, want)
	}
	if got := ev.Facts["backlog.own_off_path"]; !sameIDs(got, mine, sibling) {
		t.Errorf("backlog.own_off_path = %#v, want exactly %s and %s", got, mine, sibling)
	}
	if got := ev.Facts["backlog.trimmed"]; got != float64(0) {
		t.Errorf("backlog.trimmed = %#v, want 0", got)
	}

	// --limit cuts rows the scope kept; it does not change what the scope
	// decided. The own rows stay on the event whether or not they were printed,
	// and the trim count accounts for the ones that were not.
	runCLIInDir(t, repo, "backlog", "--limit", "1")
	ev = lastEvent(t)
	if got := ev.Facts["backlog.own_off_path"]; !sameIDs(got, mine, sibling) {
		t.Errorf("--limit 1: backlog.own_off_path = %#v, want exactly %s and %s", got, mine, sibling)
	}
	if got := ev.Facts["backlog.trimmed"]; got != float64(2) {
		t.Errorf("--limit 1: backlog.trimmed = %#v, want 2", got)
	}

	// --all lists the whole queue, so nothing is off a path and no fact says
	// a scope narrowed it.
	whole := runCLIInDir(t, repo, "backlog", "--all")
	if strings.Contains(whole, ownClause) {
		t.Fatalf("`lit backlog --all` names own work off a path it did not apply:\n%s", whole)
	}
	ev = lastEvent(t)
	for _, key := range []string{"backlog.focus_goals", "backlog.off_path", "backlog.own_off_path"} {
		if got := ev.Facts[key]; !reflect.DeepEqual(got, []any{}) {
			t.Errorf("--all: %s = %#v, want []", key, got)
		}
	}
}

// sameIDs reports whether a fact holding a list of ids holds exactly want, in
// any order: the order is the backlog's rank order, which this test does not
// set.
func sameIDs(fact any, want ...string) bool {
	got, ok := fact.([]any)
	if !ok || len(got) != len(want) {
		return false
	}
	for _, id := range want {
		if !slices.Contains(got, any(id)) {
			return false
		}
	}
	return true
}

// A focus label narrows the pool `lit next` picks from, so a ticket ranked
// above the pick but off the path is passed over without being asked about. The
// pick says which question produced it — the goal and the way out — or an agent
// checking it against `lit backlog --all` reads a lower-ranked ticket served
// first as a ranking bug. A run over the whole queue serves the higher-ranked
// ticket and claims no scope it did not apply.
func TestFocusedPickSaysWhichQuestionProducedIt(t *testing.T) {
	h := newReadyTestHarness(t)

	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	prereq := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "prereq",
	})
	h.addDependencyWithinEpic(goal.ID, prereq.ID)
	h.setLabels(goal.ID, FocusLabel)
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	off := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Ready, off the focus path", Topic: "noise", IssueType: "task", ParentID: noise.ID, Lane: "off",
	})
	// Ranked above every path row: the `--all` run serving it is what shows the
	// focused run passed over a higher-ranked ticket.
	if _, err := h.ap.Store.RankToTop(h.ctx, noise.ID); err != nil {
		t.Fatalf("RankToTop: %v", err)
	}

	focused := h.runNextText()
	if want := "run `lit start " + prereq.ID + "` to claim lane prereq of epic " + goalEpic.ID + " (focused on " + goal.ID + ": picked from the focus path alone — `lit next --all` to route over the whole queue)\n"; !strings.Contains(focused, want) {
		t.Fatalf("focused `lit next` printed:\n%s\nwant the pick line %q", focused, want)
	}

	whole := h.runNextText("--all")
	if want := "run `lit start " + off.ID + "` to claim lane off of epic " + noise.ID + "\n"; !strings.Contains(whole, want) {
		t.Fatalf("`lit next --all` printed:\n%s\nwant the pick line %q", whole, want)
	}
}

// The focus clause belongs to the pool's row, so on the two outcomes that print
// the pool's pick as one route among others it lands on that route's line: not
// on the stopped scope's reason, and not on the ticket the filter hid, which
// were not drawn from the pool's scope.
func TestFocusClauseLandsOnThePoolsRoute(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "next", IssueType: "epic"})
	ticket := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Pool pick", Topic: "next", IssueType: "task", ParentID: epic.ID, Lane: "pick"})
	rows, details, _ := h.gather()
	row := rowByID(t, rows, ticket.ID)
	pick := poolPick{Row: row, Lane: laneOf(t, details, row), Scope: focusScope{goals: []string{"test-goal"}}}
	const clause = " (focused on test-goal: picked from the focus path alone — `lit next --all` to route over the whole queue)"

	for _, tc := range []struct {
		name    string
		outcome NextOutcome
		route   string
	}{
		{
			// A clearable blocker puts the stay route first, so the pool's route is
			// the second of two joined on one block and not the only line.
			name: "past an exhausted scope",
			outcome: ServedPastExhaustion{poolPick: pick, Exhaustion: Exhausted{
				Epics:   []string{"test-held"},
				Blocked: []blockerReach{{depReach: depReach{ID: "test-dep"}, Clearable: true}},
			}},
			route: "or move on to the top ready ticket outside it: run `lit start " + ticket.ID + "` to claim lane pick of epic " + epic.ID + clause,
		},
		{
			name:    "past a scope the filter narrowed",
			outcome: ServedPastNarrowing{poolPick: pick, Narrowing: Narrowed{Hidden: rowFor("test-hidden")}},
			route:   "or take the top ready ticket the filter selects, outside the work you hold: run `lit start " + ticket.ID + "` to claim lane pick of epic " + epic.ID + clause,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := renderNextOutcome(&buf, tc.outcome, details, claimContext{self: selfAttribution}, "", false); err != nil {
				t.Fatalf("renderNextOutcome error = %v", err)
			}
			var carrying []string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.Contains(line, "focused on") {
					carrying = append(carrying, line)
				}
			}
			if len(carrying) != 1 || carrying[0] != tc.route {
				t.Fatalf("lines naming the focus = %q, want exactly the pool's route %q in:\n%s", carrying, tc.route, buf.String())
			}
		})
	}
}

// A focus that kept every row the filter selects out of the pool is said beside
// the ticket the filter hid, as it is on a pool pick: "drop the filter" alone
// would hand over the path ticket and never the bug the run asked for, which
// sits off the path.
func TestFilterHidingAFocusedPoolTicketNamesTheFocus(t *testing.T) {
	h := newReadyTestHarness(t)
	goalEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal epic", Topic: "goal", IssueType: "epic",
	})
	goal := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Goal", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "goal",
	})
	prereq := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Prereq", Topic: "goal", IssueType: "task", ParentID: goalEpic.ID, Lane: "prereq",
	})
	h.addDependencyWithinEpic(goal.ID, prereq.ID)
	h.setLabels(goal.ID, FocusLabel)
	noise := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Noise epic", Topic: "noise", IssueType: "epic",
	})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test",
		Title: "Ready bug, off the focus path", Topic: "noise", IssueType: "bug", ParentID: noise.ID, Lane: "off",
	})

	err := h.runNextErr("--type", "bug")
	want := "`lit next` without --type, --status, --labels or --assignee hands you " + prereq.ID + " (focused on " + goal.ID + ": picked from the focus path alone — `lit next --all` to route over the whole queue), which this run's filter leaves out"
	if err == nil || err.Error() != want {
		t.Fatalf("focused `lit next --type bug` = %v, want %q", err, want)
	}
	if row := h.runNextRowArgs("--all", "--type", "bug"); row.ID != bug.ID {
		t.Fatalf("`lit next --all --type bug` = %q, want the off-path bug %q the focus withheld", row.ID, bug.ID)
	}
}
