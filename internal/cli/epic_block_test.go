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

// An epic's blocker holds back every issue under the epic except one the
// blocker waits on itself, directly or through other issues, because holding
// that one back would leave the two waiting on each other forever. Every
// workspace below was reachable through `lit dep add`, `lit parent set`,
// `lit parent clear`, `lit rank` or a lane change, and each one used to stall
// the issues in the loop. The exception is read from the workspace as it is,
// so each case is stated as its final shape. held maps a row to the one
// blocker it must inherit; every other row inherits none, and pullable lists
// rows that must be startable.
func TestAnEpicsBlockerHoldsBackNothingItWaitsOn(t *testing.T) {
	// held is checked over every row, and again over the rows labeled view
	// when it is set, so a blocker no row's epic names is settled too.
	type expectation struct {
		held     map[string]string
		pullable []string
		view     string
	}
	task := func(h readyTestHarness, title, parent, lane string) model.Issue {
		return h.createIssue(storage.CreateIssueInput{Title: title, Topic: "epic-block", IssueType: "task", ParentID: parent, Lane: lane})
	}
	epic := func(h readyTestHarness, title, parent string) model.Issue {
		return h.createIssue(storage.CreateIssueInput{Title: title, Topic: "epic-block", IssueType: "epic", ParentID: parent})
	}
	// outer holds inner, whose first and second share lane a, and other in its
	// own lane.
	nested := func(h readyTestHarness) (outer, inner, first, second, other model.Issue) {
		outer = epic(h, "Outer", "")
		inner = epic(h, "Inner", outer.ID)
		first = task(h, "first", inner.ID, "a")
		second = task(h, "second", inner.ID, "a")
		other = task(h, "other", outer.ID, "other")
		return
	}
	cases := []struct {
		name  string
		build func(h readyTestHarness) expectation
	}{
		{"a leaf blocks its outer epic behind a lane-mate", func(h readyTestHarness) expectation {
			outer, _, first, second, other := nested(h)
			h.addDependency(outer.ID, second.ID)
			return expectation{held: map[string]string{other.ID: second.ID}, pullable: []string{first.ID}}
		}},
		{"that leaf also depends on another child of the epic", func(h readyTestHarness) expectation {
			outer, _, first, second, other := nested(h)
			h.addDependency(outer.ID, second.ID)
			h.addDependency(second.ID, other.ID)
			return expectation{pullable: []string{first.ID, other.ID}}
		}},
		{"a nested epic blocks its outer epic", func(h readyTestHarness) expectation {
			outer, inner, first, _, other := nested(h)
			h.addDependency(outer.ID, inner.ID)
			return expectation{held: map[string]string{other.ID: inner.ID}, pullable: []string{first.ID}}
		}},
		{"the outer epic blocks a nested epic", func(h readyTestHarness) expectation {
			outer, inner, first, _, other := nested(h)
			h.addDependency(inner.ID, outer.ID)
			return expectation{pullable: []string{first.ID, other.ID}}
		}},
		{"a gate that depends on a child of the epic it blocks", func(h readyTestHarness) expectation {
			e := epic(h, "E", "")
			c1 := task(h, "c1", e.ID, "a")
			c2 := task(h, "c2", e.ID, "b")
			g := task(h, "gate", "", "")
			h.addDependency(e.ID, g.ID)
			h.addDependency(g.ID, c1.ID)
			return expectation{held: map[string]string{c2.ID: g.ID}, pullable: []string{c1.ID}}
		}},
		{"a leaf blocks the nested epic ahead of it in its lane", func(h readyTestHarness) expectation {
			e0 := epic(h, "E0", "")
			e1 := epic(h, "E1", e0.ID)
			c := task(h, "c", e1.ID, "")
			b := task(h, "b", e0.ID, "")
			h.addDependency(e1.ID, b.ID)
			return expectation{pullable: []string{c.ID}}
		}},
		{"two epics each blocked by a child of the other", func(h readyTestHarness) expectation {
			e := epic(h, "E", "")
			c := task(h, "c", e.ID, "")
			e2 := epic(h, "E2", "")
			c2 := task(h, "c2", e2.ID, "")
			h.addDependency(e.ID, c2.ID)
			h.addDependency(e2.ID, c.ID)
			return expectation{pullable: []string{c.ID, c2.ID}}
		}},
		// g waits on x, and x's own gate h is dropped because h waits on x, so
		// the dropped link never carries g on to s: g holds s back, also in a
		// view that shows s alone.
		{"a dropped blocker carries no wait further", func(h readyTestHarness) expectation {
			e := epic(h, "E", "")
			s := task(h, "s", e.ID, "")
			f := epic(h, "F", "")
			x := task(h, "x", f.ID, "")
			g := task(h, "g", "", "")
			gh := task(h, "h", "", "")
			h.addDependency(e.ID, g.ID)
			h.addDependency(f.ID, gh.ID)
			h.addDependency(g.ID, x.ID)
			h.addDependency(gh.ID, x.ID)
			h.addDependency(gh.ID, s.ID)
			h.setLabels(s.ID, "shown")
			return expectation{held: map[string]string{s.ID: g.ID}, pullable: []string{x.ID}, view: "shown"}
		}},
		// z waits on epic k, and k stands behind l in their lane, but k's child
		// never waits on l, so neither does z: l depending on c is no loop.
		{"an epic's lane-mate is not what the epic waits on", func(h readyTestHarness) expectation {
			e0 := epic(h, "E0", "")
			l := task(h, "l", e0.ID, "")
			k := epic(h, "K", e0.ID)
			task(h, "k child", k.ID, "")
			e1 := epic(h, "E1", "")
			c := task(h, "c", e1.ID, "")
			z := task(h, "z", "", "")
			h.addDependency(z.ID, k.ID)
			h.addDependency(l.ID, c.ID)
			h.addDependency(e1.ID, z.ID)
			return expectation{held: map[string]string{c.ID: z.ID}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			want := tc.build(h)
			rows := h.runWorkableAnnotated(workableFilter{}, 0)
			if want.view != "" {
				rows = append(rows, h.runWorkableAnnotated(workableFilter{Labels: []string{want.view}}, 0)...)
			}
			for _, row := range rows {
				var got []string
				for _, ann := range row.Annotations {
					if ann.Kind == annotation.InheritedDependency {
						got = append(got, ann.Message)
					}
				}
				var wantGates []string
				if gate, held := want.held[row.ID]; held {
					wantGates = []string{gate}
				}
				if !slices.Equal(got, wantGates) {
					t.Fatalf("%s inherits %v, want %v", row.ID, got, wantGates)
				}
			}
			// No issue, epics included, is ever its own blocker.
			all, err := h.ap.Store.ListIssues(h.ctx, storage.ListIssuesFilter{})
			if err != nil {
				t.Fatalf("ListIssues error = %v", err)
			}
			annotated, _, _, err := annotateIssues(h.ctx, h.ap.Store, nil, all)
			if err != nil {
				t.Fatalf("annotateIssues error = %v", err)
			}
			for _, row := range annotated {
				for _, ann := range row.Annotations {
					if ann.Kind == annotation.InheritedDependency && ann.Message == row.ID {
						t.Fatalf("%s inherits itself", row.ID)
					}
				}
			}
			pullable := h.runPullableAnnotated(workableFilter{})
			for _, id := range want.pullable {
				if !containsID(pullable, id) {
					t.Fatalf("pullable = %v, want %s among them", ids(pullable), id)
				}
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
