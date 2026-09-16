package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// requireInheritedDependency fails unless the gathered row for id carries
// exactly one dependency annotation naming dep, of the inherited kind.
func requireInheritedDependency(t *testing.T, rows []annotation.AnnotatedIssue, id, dep string) {
	t.Helper()
	row := findRow(rows, id)
	var got []annotation.Annotation
	for _, ann := range row.Annotations {
		if ann.Kind == annotation.OpenDependency || ann.Kind == annotation.InheritedDependency {
			got = append(got, ann)
		}
	}
	if len(got) != 1 || got[0].Kind != annotation.InheritedDependency || got[0].Message != dep {
		t.Fatalf("%s dependency annotations = %v, want exactly one inherited_dependency on %s", id, got, dep)
	}
}

// A blocks edge onto an epic holds back every child of that epic, in every
// lane. The children sit in two lanes on purpose: the same-lane sibling gate
// already serializes one lane behind its first child, so blocking only that
// first child (the workaround this replaces) held a one-lane epic and let a
// second lane straight through (links-epic-block-xpkz). The gate is ranked
// below both children, so rank cannot be what holds them.
func TestBlockedEpicGatesEveryLaneOfItsChildren(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "Epic", Topic: "epic-block", IssueType: "epic"})
	laneA := h.createIssue(storage.CreateIssueInput{Title: "lane a", Topic: "epic-block", IssueType: "task", ParentID: epic.ID, Lane: "a"})
	laneB := h.createIssue(storage.CreateIssueInput{Title: "lane b", Topic: "epic-block", IssueType: "task", ParentID: epic.ID, Lane: "b"})
	gate := h.createIssue(storage.CreateIssueInput{Title: "gate", Topic: "gate", IssueType: "task"})
	h.addDependency(epic.ID, gate.ID)

	pullable := h.runPullableAnnotated(workableFilter{})
	if containsID(pullable, laneA.ID) || containsID(pullable, laneB.ID) {
		t.Fatalf("children of a blocked epic are pullable: got=%v", ids(pullable))
	}
	workable := h.runWorkableAnnotated(workableFilter{}, 0)
	requireInheritedDependency(t, workable, laneA.ID, gate.ID)
	requireInheritedDependency(t, workable, laneB.ID, gate.ID)
	if pick := h.runNextRow(); pick.ID != gate.ID {
		t.Fatalf("next = %q, want the gate %q — the only unblocked row", pick.ID, gate.ID)
	}

	text := h.runWorkableText()
	if want := "depends on: " + gate.ID + " (via epic)"; strings.Count(text, want) != 2 {
		t.Fatalf("backlog should name the inherited gate once under each child as %q; got:\n%s", want, text)
	}
	if !unblocksLineNames(text, laneA.ID) || !unblocksLineNames(text, laneB.ID) {
		t.Fatalf("the gate's row should say closing it unblocks both children; got:\n%s", text)
	}
	if strings.Contains(text, "blocked: depends on") {
		t.Fatalf("an inherited dependency belongs on the \"depends on:\" line alone; got:\n%s", text)
	}

	h.closeIssue(gate.ID, "done")
	pullable = h.runPullableAnnotated(workableFilter{})
	if !containsID(pullable, laneA.ID) || !containsID(pullable, laneB.ID) {
		t.Fatalf("closing the gate should free both lanes: got=%v", ids(pullable))
	}
}

// The gate reaches every depth: an epic nested under the blocked epic is under
// it too. A gate is named once however many epics above an issue pass it down,
// and a child that already depends on the gate directly is named once, by its
// own edge, because the remedy for that edge is on the child.
func TestBlockedEpicGatesNestedEpicsAndNamesADirectEdgeOnce(t *testing.T) {
	h := newReadyTestHarness(t)
	outer := h.createIssue(storage.CreateIssueInput{Title: "Outer", Topic: "epic-block", IssueType: "epic"})
	inner := h.createIssue(storage.CreateIssueInput{Title: "Inner", Topic: "epic-block", IssueType: "epic", ParentID: outer.ID})
	deep := h.createIssue(storage.CreateIssueInput{Title: "deep", Topic: "epic-block", IssueType: "task", ParentID: inner.ID, Lane: "a"})
	direct := h.createIssue(storage.CreateIssueInput{Title: "direct", Topic: "epic-block", IssueType: "task", ParentID: inner.ID, Lane: "b"})
	gate := h.createIssue(storage.CreateIssueInput{Title: "gate", Topic: "gate", IssueType: "task"})
	h.addDependency(outer.ID, gate.ID)
	h.addDependency(inner.ID, gate.ID)
	h.addDependency(direct.ID, gate.ID)

	workable := h.runWorkableAnnotated(workableFilter{}, 0)
	requireInheritedDependency(t, workable, deep.ID, gate.ID)
	row := findRow(workable, direct.ID)
	deps := ClassifyReadiness(row.Annotations).BlockingReasons()
	if len(deps) != 1 || deps[0].Kind != annotation.OpenDependency || deps[0].Detail != gate.ID {
		t.Fatalf("%s blocking reasons = %v, want only its own edge on %s", direct.ID, deps, gate.ID)
	}
}

// A blocker the hierarchy already relates to the epic it blocks gates nothing
// through that epic. A workspace can hold every shape below: `lit dep add`
// accepted each one until rejectWaitCycle, and still accepts the last
// (links-hierarchy-kh57). Each one used to stall an issue that nothing else held
// back:
//   - a leaf two levels down blocks the outer epic behind an unfinished
//     lane-mate. The lane-mate inherited the leaf while the leaf waited on the
//     lane-mate.
//   - that leaf also depends on another child of the outer epic. The child
//     inherited the leaf while the leaf waited on the child.
//   - the nested epic blocks the outer epic. The leaf waited for its own epic.
//   - the outer epic blocks the nested epic. The leaf waited for an epic that
//     cannot finish before the leaf does.
func TestBlockedEpicIgnoresABlockerItsHierarchyRelates(t *testing.T) {
	type edge struct{ dependent, blocker string }
	cases := []struct {
		name  string
		edges func(outer, inner, first, second, other model.Issue) []edge
	}{
		{"leaf behind a lane-mate", func(outer, inner, first, second, other model.Issue) []edge {
			return []edge{{outer.ID, second.ID}}
		}},
		{"leaf that depends on a child of the epic", func(outer, inner, first, second, other model.Issue) []edge {
			return []edge{{outer.ID, second.ID}, {second.ID, other.ID}}
		}},
		{"nested epic", func(outer, inner, first, second, other model.Issue) []edge {
			return []edge{{outer.ID, inner.ID}}
		}},
		{"ancestor epic", func(outer, inner, first, second, other model.Issue) []edge {
			return []edge{{inner.ID, outer.ID}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			outer := h.createIssue(storage.CreateIssueInput{Title: "Outer", Topic: "epic-block", IssueType: "epic"})
			inner := h.createIssue(storage.CreateIssueInput{Title: "Inner", Topic: "epic-block", IssueType: "epic", ParentID: outer.ID})
			first := h.createIssue(storage.CreateIssueInput{Title: "first", Topic: "epic-block", IssueType: "task", ParentID: inner.ID, Lane: "a"})
			second := h.createIssue(storage.CreateIssueInput{Title: "second", Topic: "epic-block", IssueType: "task", ParentID: inner.ID, Lane: "a"})
			other := h.createIssue(storage.CreateIssueInput{Title: "other", Topic: "epic-block", IssueType: "task", ParentID: outer.ID, Lane: "other"})
			for _, e := range tc.edges(outer, inner, first, second, other) {
				h.addDependency(e.dependent, e.blocker)
			}

			for _, row := range h.runWorkableAnnotated(workableFilter{}, 0) {
				if annotation.HasAny(row.Annotations, annotation.InheritedDependency) {
					t.Fatalf("%s inherits a blocker from inside its own hierarchy: %v", row.ID, row.Annotations)
				}
			}
			pullable := h.runPullableAnnotated(workableFilter{})
			if !containsID(pullable, first.ID) || !containsID(pullable, other.ID) {
				t.Fatalf("pullable = %v, want the lane head %s and %s", ids(pullable), first.ID, other.ID)
			}
		})
	}
}

// Focus on a leaf of a blocked epic puts the epic's gate on the path, so the
// focused queue offers the gate instead of a goal nobody can start.
func TestFocusOnAChildOfABlockedEpicReachesTheGate(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "Epic", Topic: "epic-block", IssueType: "epic"})
	goal := h.createIssue(storage.CreateIssueInput{Title: "goal", Topic: "epic-block", IssueType: "task", ParentID: epic.ID})
	_ = h.createIssue(storage.CreateIssueInput{Title: "elsewhere", Topic: "other", IssueType: "task"})
	gate := h.createIssue(storage.CreateIssueInput{Title: "gate", Topic: "gate", IssueType: "task"})
	h.addDependency(epic.ID, gate.ID)
	h.setLabels(goal.ID, FocusLabel)

	path, err := fetchFocusPathGoals(h.ctx, h.ap.Store)
	if err != nil {
		t.Fatalf("fetchFocusPathGoals error = %v", err)
	}
	if path[gate.ID] != goal.ID {
		t.Fatalf("focus path = %v, want the epic's gate %s attributed to goal %s", path, gate.ID, goal.ID)
	}
	if pick := h.runNextRow(); pick.ID != gate.ID {
		t.Fatalf("focused next = %q, want the gate %q", pick.ID, gate.ID)
	}
}

// Routing treats the epic's gate as a dependency of the epic's lanes. A checkout
// holding a lane of the blocked epic is offered the gate as on-path work, and a
// checkout that holds only the epic is told its epic is blocked by the gate. It
// is never handed the unrelated ready ticket ranked above the gate, and never
// the gated child it would have been handed before the gate held.
func TestRouteNextTreatsAnEpicsGateAsOnPath(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "Epic", Topic: "epic-block", IssueType: "epic"})
	done := h.createIssue(storage.CreateIssueInput{Title: "done", Topic: "epic-block", IssueType: "task", ParentID: epic.ID, Lane: "done"})
	h.transition(done.ID, model.Start{Assignee: "tester"})
	h.transition(done.ID, model.Done{})
	gated := h.createIssue(storage.CreateIssueInput{Title: "gated", Topic: "epic-block", IssueType: "task", ParentID: epic.ID, Lane: "gated"})
	_ = h.createIssue(storage.CreateIssueInput{Title: "unrelated", Topic: "other", IssueType: "task"})
	gate := h.createIssue(storage.CreateIssueInput{Title: "gate", Topic: "gate", IssueType: "task"})
	h.addDependency(epic.ID, gate.ID)
	rows, details := h.gather()
	doneLane := model.LaneOf(done, &epic)
	gatedLane := laneOf(t, details, rowByID(t, rows, gated.ID))

	t.Run("holding the gated lane", func(t *testing.T) {
		standings := claims.Standings{doneLane: heldBy(selfAttribution), gatedLane: heldBy(selfAttribution)}
		outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
		served, ok := outcome.(ServedFromNewLane)
		if !ok || served.Row.ID != gate.ID {
			t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane serving the gate %s", outcome, outcome, gate.ID)
		}
	})
	t.Run("holding only the epic", func(t *testing.T) {
		standings := claims.Standings{doneLane: heldBy(selfAttribution)}
		outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
		exhausted, ok := outcome.(Exhausted)
		if !ok {
			t.Fatalf("routeNext = %#v (%T), want Exhausted naming the gate", outcome, outcome)
		}
		blockers := make([]string, len(exhausted.Blocked))
		for i, b := range exhausted.Blocked {
			blockers[i] = b.ID
		}
		if !slices.Equal(blockers, []string{gate.ID}) {
			t.Fatalf("Exhausted.Blocked = %v, want exactly the gate %s", blockers, gate.ID)
		}
	})
}
