package cli

import (
	"fmt"
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
// first child would hold a one-lane epic and let a second lane straight
// through. The gate is ranked below both children, so rank cannot be what holds
// them.
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

// dependThenReparent reaches a blocks edge between an issue and its ancestor
// the one way it still can be reached: the store refuses that edge outright,
// so childID is detached from parentID while the edge is written and put back
// after. Readiness has to stay correct over the shape a reparent leaves.
func (h readyTestHarness) dependThenReparent(dependentID, dependencyID, childID, parentID string) {
	h.t.Helper()
	if err := h.ap.Store.ClearParent(h.ctx, childID); err != nil {
		h.t.Fatalf("ClearParent(%s) error = %v", childID, err)
	}
	h.addDependency(dependentID, dependencyID)
	if _, err := h.ap.Store.SetParent(h.ctx, storage.SetParentInput{ChildID: childID, ParentID: parentID, CreatedBy: "agent"}); err != nil {
		h.t.Fatalf("SetParent(%s under %s) error = %v", childID, parentID, err)
	}
}

// An epic's blocker holds back every issue under the epic except one the
// blocker waits on itself, directly or through other issues, because holding
// that one back would leave the two waiting on each other forever. Every
// workspace below was reachable through `lit dep add`, `lit parent set`,
// `lit parent clear`, `lit rank` or a lane change. The exception is read from
// the workspace as it is, so each case is stated as its final shape. held maps
// a row to the one blocker it must inherit; every other row inherits none, and
// pullable lists rows that must be startable.
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
			outer, inner, first, second, other := nested(h)
			h.dependThenReparent(outer.ID, second.ID, inner.ID, outer.ID)
			return expectation{held: map[string]string{other.ID: second.ID}, pullable: []string{first.ID}}
		}},
		{"that leaf also depends on another child of the epic", func(h readyTestHarness) expectation {
			outer, inner, first, second, other := nested(h)
			h.dependThenReparent(outer.ID, second.ID, inner.ID, outer.ID)
			h.addDependency(second.ID, other.ID)
			return expectation{pullable: []string{first.ID, other.ID}}
		}},
		{"a nested epic blocks its outer epic", func(h readyTestHarness) expectation {
			outer, inner, first, _, other := nested(h)
			h.dependThenReparent(outer.ID, inner.ID, inner.ID, outer.ID)
			return expectation{held: map[string]string{other.ID: inner.ID}, pullable: []string{first.ID}}
		}},
		{"the outer epic blocks a nested epic", func(h readyTestHarness) expectation {
			outer, inner, first, _, other := nested(h)
			h.dependThenReparent(inner.ID, outer.ID, inner.ID, outer.ID)
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
			annotated, err := annotateIssues(h.ctx, h.ap.Store, nil, all)
			if err != nil {
				t.Fatalf("annotateIssues error = %v", err)
			}
			for _, row := range annotated.rows {
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
			// A blocker readiness drops closes no loop, so lit doctor, reading
			// the same links, finds none either.
			loops, err := findWaitLoops(h.ctx, h.ap.Store)
			if err != nil {
				t.Fatalf("findWaitLoops error = %v", err)
			}
			if len(loops) != 0 {
				t.Fatalf("findWaitLoops = %v, want none", loops)
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
// holding a lane of the blocked epic is offered the gate as on-path work through
// step 1b, and a checkout that holds only a finished lane of the epic is offered
// it through step 2b. It is never handed the unrelated ready ticket ranked above
// the gate, and never the gated child it would have been handed before the gate
// held.
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
	rows, details, epics := h.gather()
	doneLane := model.LaneOf(done, &epic)
	gatedLane := laneOf(t, details, rowByID(t, rows, gated.ID))

	t.Run("holding the gated lane", func(t *testing.T) {
		standings := claims.Standings{doneLane: heldBy(selfAttribution), gatedLane: heldBy(selfAttribution)}
		outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
		// An epic's gate reaches us through step 1b, so it arrives as the
		// dependency outcome and says what it unblocks.
		served, ok := outcome.(ServedFromDependency)
		if !ok || served.Row.ID != gate.ID {
			t.Fatalf("routeNext = %#v (%T), want ServedFromDependency serving the gate %s", outcome, outcome, gate.ID)
		}
		if served.Gates != gated.ID {
			t.Fatalf("served.Gates = %q, want %q — an epic's gate reaches us through the gated child, and a non-empty id is not the assertion: the dependency naming itself would satisfy that", served.Gates, gated.ID)
		}
	})
	t.Run("holding only the epic", func(t *testing.T) {
		standings := claims.Standings{doneLane: heldBy(selfAttribution)}
		outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
		// The gated child sits in a lane we do not hold, so the gate reaches us
		// through step 2b rather than 1b — and still ahead of the unrelated
		// ticket the pool would serve once the epic is exhausted.
		served, ok := outcome.(ServedFromDependency)
		if !ok || served.Row.ID != gate.ID {
			t.Fatalf("routeNext = %#v (%T), want ServedFromDependency serving the gate %s", outcome, outcome, gate.ID)
		}
		if served.Gates != gated.ID {
			t.Fatalf("served.Gates = %q, want %q", served.Gates, gated.ID)
		}
	})
}

// When the dependency on a checkout's path is an epic, the work that clears it
// sits under it: an epic cannot be started, and the gather never returns one as
// a row. Routing therefore descends to the epic's ready ticket, at any depth,
// and serves it as on-path work, naming the epic and the row it frees. When
// nothing under the epic is this checkout's to take, it still reports
// exhaustion naming the epic. Either way it never hops to the unrelated ready
// ticket.
func TestRouteNextDescendsAnEpicBlockerToItsWorkableChild(t *testing.T) {
	for depth := 1; depth <= 2; depth++ {
		t.Run(fmt.Sprintf("the ready ticket %d epic(s) down", depth), func(t *testing.T) {
			h := newReadyTestHarness(t)
			_ = h.createIssue(storage.CreateIssueInput{Title: "unrelated", Topic: "other", IssueType: "task"})
			epicB := h.createIssue(storage.CreateIssueInput{Title: "B", Topic: "epic-block", IssueType: "epic"})
			b1 := h.createIssue(storage.CreateIssueInput{Title: "b1", Topic: "epic-block", IssueType: "task", ParentID: epicB.ID, Lane: "b1"})
			h.transition(b1.ID, model.Start{Assignee: "tester"})
			h.transition(b1.ID, model.Done{})
			b2 := h.createIssue(storage.CreateIssueInput{Title: "b2", Topic: "epic-block", IssueType: "task", ParentID: epicB.ID, Lane: "b2"})
			epicA := h.createIssue(storage.CreateIssueInput{Title: "A", Topic: "epic-block", IssueType: "epic"})
			parent := epicA.ID
			for level := 1; level < depth; level++ {
				parent = h.createIssue(storage.CreateIssueInput{Title: "nested", Topic: "epic-block", IssueType: "epic", ParentID: parent}).ID
			}
			a1 := h.createIssue(storage.CreateIssueInput{Title: "a1", Topic: "epic-block", IssueType: "task", ParentID: parent})
			h.addDependency(epicB.ID, epicA.ID)

			rows, details, epics := h.gather()
			requireInheritedDependency(t, rows, b2.ID, epicA.ID)
			bLane := laneOf(t, details, rowByID(t, rows, b2.ID))
			aLane := laneOf(t, details, rowByID(t, rows, a1.ID))

			outcome := routeNext(rows, details, epics, claims.Standings{bLane: heldBy(selfAttribution)}, selfAttribution, focusScope{})
			served, ok := outcome.(ServedFromDependency)
			if !ok || served.Row.ID != a1.ID {
				t.Fatalf("routeNext = %#v (%T), want ServedFromDependency serving %s, the ready ticket under the blocking epic %s", outcome, outcome, a1.ID, epicA.ID)
			}
			if served.Gates != b2.ID || served.Blocker != epicA.ID {
				t.Fatalf("served gates %q through %q, want %q through %q — a1 has no edge to b2, so the pick must name the epic between them", served.Gates, served.Blocker, b2.ID, epicA.ID)
			}
			if served.Lane != aLane {
				t.Fatalf("served.Lane = %v, want %v, the lane a start of %s would claim", served.Lane, aLane, a1.ID)
			}

			// With a1 held elsewhere epic B is exhausted, and the unrelated ready
			// ticket is served past it — beside the exhaustion, which is what
			// this half reads.
			held := claims.Standings{bLane: heldBy(selfAttribution), aLane: heldBy(otherAttribution)}
			past, ok := routeNext(rows, details, epics, held, selfAttribution, focusScope{}).(ServedPastExhaustion)
			if !ok {
				t.Fatalf("with %s's lane held elsewhere, routeNext did not serve past the exhausted epic", a1.ID)
			}
			exhausted := past.Exhaustion
			blockers := make([]string, len(exhausted.Blocked))
			for i, b := range exhausted.Blocked {
				blockers[i] = b.ID
			}
			if !slices.Equal(exhausted.Epics, []string{epicB.ID}) || !slices.Equal(blockers, []string{epicA.ID}) {
				t.Fatalf("Exhausted = epics %v blocked on %v, want epic %s blocked on %s", exhausted.Epics, blockers, epicB.ID, epicA.ID)
			}
			// The epic is read through the work under it: its one ticket is held
			// elsewhere, so it is held, not outside the view — the epic's own
			// row is never gathered, and reading that would repeat the bug.
			if kind := exhausted.Blocked[0].Kind; kind != reachHeldFresh {
				t.Fatalf("blocker %s classified as %v, want reachHeldFresh (%s is held by another checkout)", epicA.ID, kind, a1.ID)
			}

			// Holding only b1's closed lane, b2 is ours by epic but not by lane,
			// so step 1b does not look at it and step 2b does. a1 is free, so it
			// is served — a1, the ticket `lit start` can act on, not A, which
			// cannot be started, with A named as what gates b2.
			b1Lane := model.LaneOf(b1, &epicB)
			served, ok = routeNext(rows, details, epics, claims.Standings{b1Lane: heldBy(selfAttribution)}, selfAttribution, focusScope{}).(ServedFromDependency)
			if !ok || served.Row.ID != a1.ID || served.Gates != b2.ID || served.Blocker != epicA.ID {
				t.Fatalf("holding only %s's lane, routeNext = %#v, want step 2b serving %s through %s for %s", b1.ID, served, a1.ID, epicA.ID, b2.ID)
			}
		})
	}
}
