package cli

import (
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// These tests exercise routeNext directly against hand-built claims.Standings
// rather than through claims.Derive: the predicate that turns evidence into a
// Standing is links-claims-1ihf.3/.4's contract, already proven by
// internal/claims's own tests. What links-claims-1ihf.5 adds is the
// selection precedence GIVEN a Standing per lane, so these tests hold the
// standings fixed and vary only the routing question.
// [LAW:decomposition] one seam, one test surface.
//
// The orphan fact is held the same way, for the same reason: `orphan` below
// attaches the annotation the gather's annotator would attach, rather than
// backdating a store row by six hours to earn it. What is under test is what
// routeNext does GIVEN the fact — newOrphanedAnnotator's threshold is its own
// contract, tested where it lives. [LAW:behavior-not-structure]

var (
	selfAttribution  = model.NewAttribution("self-stream", "ws")
	otherAttribution = model.NewAttribution("other-stream", "ws")
)

func heldBy(who model.Attribution) claims.Standing {
	return claims.Held{Tenure: claims.Tenure{By: who}}
}

func staleBy(who model.Attribution) claims.Standing {
	return claims.Stale{Tenure: claims.Tenure{By: who}}
}

func laneOf(t *testing.T, details map[string]storage.IssueRelations, row annotation.AnnotatedIssue) model.LaneID {
	t.Helper()
	return model.LaneOf(row.Issue, details[row.ID].Parent)
}

func rowByID(t *testing.T, rows []annotation.AnnotatedIssue, id string) annotation.AnnotatedIssue {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no row with id %q among %d rows", id, len(rows))
	return annotation.AnnotatedIssue{}
}

// orphan marks a gathered row as orphaned in place.
func orphan(t *testing.T, rows []annotation.AnnotatedIssue, id string) {
	t.Helper()
	for i := range rows {
		if rows[i].ID == id {
			rows[i].Annotations = append(rows[i].Annotations, annotation.Annotation{
				Kind:    annotation.Orphaned,
				Message: "in_progress for 100h0m0s with no update",
			})
			return
		}
	}
	t.Fatalf("no row with id %q to mark orphaned among %d rows", id, len(rows))
}

func (h readyTestHarness) transition(id string, action model.Action) {
	h.t.Helper()
	if _, err := h.ap.Store.Apply(h.ctx, id, storage.Change{Action: action, Actor: "tester"}); err != nil {
		h.t.Fatalf("Apply(%s, %T) error = %v", id, action, err)
	}
}

func (h readyTestHarness) gather() ([]annotation.AnnotatedIssue, map[string]storage.IssueRelations) {
	h.t.Helper()
	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	return rows, details
}

// A checkout's own held lane wins over a higher-ranked, entirely unclaimed
// epic — routing step 1 outranks plain backlog order. This is the ticket's
// namesake bug: without claims, `next` would return B.1 (top composite
// rank); with the checkout's own claim on epic A, it must not.
func TestRouteNextServesOwnClaimOverHigherRankedUnclaimedLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	// A.2 sits in its own lane so the lane gate does not block it behind the
	// in_progress default-lane sibling A.1 — this test's contract is claim
	// precedence over rank, not lane-gate membership.
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromClaim)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromClaim", outcome, outcome)
	}
	if served.Row.ID != a2.ID {
		t.Fatalf("served = %q, want %q (own claim beats higher-ranked unclaimed epic)", served.Row.ID, a2.ID)
	}
}

// A lane held (fresh) by another checkout is routed around silently in the
// global pool, even though it outranks the unclaimed lane behind it — the
// second half of the acceptance scenario: "work another checkout is
// actively driving is routed around, visible but not pullable." This is
// links-claims-1b0p acceptance 4: the resume/takeover work admits STALE holds
// and must leave a live one exactly where it was.
func TestRouteNextRoutesAroundLaneHeldByAnother(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution)}

	// This checkout holds no claims of its own, so routing starts straight at
	// the global pool, exactly as design-docs/work-claims.md specifies for the
	// zero state.
	outcome := routeNext(rows, details, standings, model.Attribution{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane", outcome, outcome)
	}
	if served.Row.ID != c1.ID {
		t.Fatalf("served = %q, want %q (B.1's lane is held elsewhere and must be skipped)", served.Row.ID, c1.ID)
	}
}

// The GRANULARITY RULING (ticket comment, 2026-08-24): once a checkout's own
// held lane has no work left, the rest of that SAME epic's lanes come before
// any other epic, however it ranks — "ALL LANES IN AN EPIC SHOULD BE SURFACED
// BEFORE ANY LANE FROM THE NEXT EPIC."
//
// "No work left" is now literal: A.1 is DONE, so its lane holds nothing to
// serve or resume. It used to be merely in_progress, which reached step 2 only
// because an in_progress row was servable to nobody — the gate this ticket
// removes. A lane with work still in flight is handed that work back
// (TestRouteNextResumesOwnInFlightTicket), so epic continuation now has to be
// asked with the lane genuinely finished.
func TestRouteNextContinuesEpicBeforeHigherRankedOtherEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	rows, details := h.gather()
	// A.1 is closed, so it is not among the gathered rows — its lane is
	// addressed straight from the issue, which is the shape of the real case:
	// a claim outlives the ticket that established it.
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane", outcome, outcome)
	}
	if served.Row.ID != a2.ID {
		t.Fatalf("served = %q, want %q (epic A's other lane before epic B)", served.Row.ID, a2.ID)
	}
	if served.Epic != epicA.ID {
		t.Fatalf("served.Epic = %q, want %q", served.Epic, epicA.ID)
	}
}

// A checkout's own epic having no reachable work — its held lane holds
// nothing, and the epic has no other lane to offer — is a loud diagnostic,
// never a silent hop to a leaf outside the epic. The GRANULARITY RULING is
// explicit that this is the emergency the ticket exists to close: "root cause
// ... sessions closed a child of epic A then hopped to epic B, repeatedly."
//
// links-claims-1b0p acceptance 2 adds the stale case, and it is the reason the
// table has two rows: heldBySelf matched claims.Held only, so a checkout whose
// own claim aged out disowned its own lane, ownLanes came back empty, and this
// entire diagnostic became unreachable at exactly the moment staleness made it
// matter (G1). The two standings must reach the same verdict.
func TestRouteNextExhaustionNeverFallsToAnotherEpic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(model.Attribution) claims.Standing
	}{
		{"fresh own claim", heldBy},
		{"stale own claim", staleBy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
			a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
			h.transition(a1.ID, model.Start{Assignee: "tester"})
			h.transition(a1.ID, model.Done{})

			epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
			h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

			rows, details := h.gather()
			standings := claims.Standings{model.LaneOf(a1, &epicA): tc.claim(selfAttribution)}

			outcome := routeNext(rows, details, standings, selfAttribution)
			exhausted, ok := outcome.(Exhausted)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want Exhausted (never epic B's B.1)", outcome, outcome)
			}
			if len(exhausted.Epics) != 1 || exhausted.Epics[0] != epicA.ID {
				t.Fatalf("exhausted.Epics = %v, want [%q]", exhausted.Epics, epicA.ID)
			}
			if len(exhausted.Blocked) != 0 {
				t.Fatalf("exhausted.Blocked = %v, want none (epic A has nothing queued)", exhausted.Blocked)
			}
			if err := exhaustedError(exhausted); err == nil {
				t.Fatal("exhaustedError(exhausted) = nil, want a diagnostic error")
			}
		})
	}
}

// An out-of-lane dependency that gates the claimed lane's blocked ticket is
// offered as on-path — design-docs/work-claims.md, Routing step 1 — and is
// announced as the claim it establishes. It comes back as ServedFromNewLane
// and not ServedFromClaim: the dependency is by definition OUTSIDE the claimed
// lane, so starting it claims a second lane, and ServedFromClaim's contract is
// that nothing is claimed and nothing is said (links-claims-1b0p, N3).
func TestRouteNextOffersOnPathDependencyAsANewLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)

	rows, details := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (on-path dependency)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the on-path external dependency)", served.Row.ID, dep.ID)
	}
	if served.Lane == "" {
		t.Fatal("served.Lane is empty; the pick claims a lane this checkout does not hold and must name it")
	}
}

// links-claims-1b0p acceptance 5 (finding N8), which involves no staleness at
// all: a checkout holding a FRESH claim, whose lane's only member is the
// in_progress ticket it is working, is handed that ticket back. It must not
// get the Exhausted diagnostic, and it must not hop.
//
// This is the promise `lit quickstart work` makes in writing — "a fresh
// session here routes back to it automatically" — and it failed for every
// parentless ticket and for the last ticket of any epic, because servability
// required model.StateOpen.
func TestRouteNextResumesOwnInFlightTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	resumed, ok := outcome.(ResumedOwnWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ResumedOwnWork (the ticket this checkout is on)", outcome, outcome)
	}
	if resumed.Row.ID != a1.ID {
		t.Fatalf("resumed = %q, want %q", resumed.Row.ID, a1.ID)
	}
}

// links-claims-1b0p acceptance 1, the headline: one checkout claims a lane,
// goes stale past the freshness window, and the lane's only remaining work is
// the in_progress orphan. Bare `lit next` offers THAT ticket back to resume —
// it does not return another epic's leaf, and it does not call the work a
// takeover, because a stale claim of your own is evidence you stepped away,
// not evidence the work stopped being yours.
func TestRouteNextResumesOwnOrphanInStaleLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details := h.gather()
	orphan(t, rows, a1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): staleBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	resumed, ok := outcome.(ResumedOwnWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ResumedOwnWork, never epic B's %q", outcome, outcome, b1.ID)
	}
	if resumed.Row.ID != a1.ID {
		t.Fatalf("resumed = %q, want %q (this checkout's own abandoned lane)", resumed.Row.ID, a1.ID)
	}
}

// links-claims-1b0p acceptance 3: the orphan is the only work in a lane
// another checkout let go stale, and it is offered to a bare `lit next` here.
// Two facts had to change together for this to be reachable — a stale foreign
// lane is admitted (G3), and an in_progress row is servable at all (G2) — and
// fixing either alone leaves the pick unreachable.
func TestRouteNextTakesOverOrphanInForeignStaleLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	h.transition(b1.ID, model.Start{Assignee: "other"})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details := h.gather()
	orphan(t, rows, b1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): staleBy(otherAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (takeover of the stale lane)", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q (the orphan outranks unclaimed %q)", served.Row.ID, b1.ID, c1.ID)
	}
}

// The other half of the same rule, and the reason admitting stale lanes is not
// a general loosening: an in_progress row in a stale foreign lane that nobody
// has abandoned is still somebody's work in flight. Only the orphan annotation
// — the proof that the claim asserting somebody is working it is self-refuting
// — makes it takeable.
func TestRouteNextLeavesUnabandonedInFlightWorkAlone(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	h.transition(b1.ID, model.Start{Assignee: "other"})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): staleBy(otherAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane", outcome, outcome)
	}
	if served.Row.ID != c1.ID {
		t.Fatalf("served = %q, want %q (B.1 is in flight and not orphaned — leave it)", served.Row.ID, c1.ID)
	}
}

// links-claims-1b0p, N1: ownership is a fact about the workspace, so a display
// filter must not be able to change it. The checkout holds a FRESH claim on a
// lane whose only ticket is a task, and asks for bugs. Its own lane's rows
// vanish from the gathered set — and it must still get its epic's Exhausted
// diagnostic rather than another epic's leaf, which is what deriving ownership
// from the filtered rows produced.
func TestRouteNextKeepsOwnershipUnderADisplayFilter(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicB.ID})

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{IssueType: model.TypeBug})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted (epic B's bug is a hop)", outcome, outcome)
	}
	if len(exhausted.Epics) != 1 || exhausted.Epics[0] != epicA.ID {
		t.Fatalf("exhausted.Epics = %v, want [%q]", exhausted.Epics, epicA.ID)
	}
}

// The N2 regression: before this ticket, onPathDependency saw no standings at
// all and offered a gating dependency sitting in a lane another checkout holds
// fresh — which `lit start` then refused, so `next` recommended what `start`
// blocked. The dependency is now routed around like any other fresh foreign
// hold, and exhaustion still names it rather than going quiet about why there
// is nothing to do. [LAW:no-silent-failure]
func TestRouteNextRoutesAroundOnPathDependencyHeldFresh(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)

	rows, details := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):  heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution)
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the on-path dependency's lane is held fresh elsewhere, so `next` must not offer what `start` would refuse", outcome, outcome)
	}
	if !slices.Contains(exhausted.Blocked, dep.ID) {
		t.Fatalf("exhausted.Blocked = %v, want it to name %q — routing around the dependency must not hide it", exhausted.Blocked, dep.ID)
	}
}

// Design step 6 in its plainest form: a stale claim no longer vetoes an
// otherwise-ready ticket. Nothing here is started and nothing is orphaned, so
// the pick rests on staleness alone — the `!started && readiness.IsReady()`
// half of capacityFor's takeability that every other stale-foreign test in
// this file reaches only through an orphaned in-progress row.
func TestRouteNextServesOpenTicketInForeignStaleLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): staleBy(otherAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (the stale lane is admitted, not skipped)", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q — a stale claim must not push a ready ticket behind the lower-ranked unclaimed %q", served.Row.ID, b1.ID, c1.ID)
	}
	if served.Row.State() != model.StateOpen {
		t.Fatalf("served row state = %v, want open — this pick must rest on staleness alone, never on an orphan", served.Row.State())
	}
}

// The same admission at the epic-continuation step, which is itself new here:
// step 2 previously required an unclaimed lane, so a stale sibling lane of the
// checkout's own epic was skipped in favour of exhaustion.
func TestRouteNextContinuesEpicIntoForeignStaleLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): staleBy(otherAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane (a stale sibling lane of our own epic is admitted)", outcome, outcome)
	}
	if served.Row.ID != a2.ID || served.Epic != epicA.ID {
		t.Fatalf("served = %q in epic %q, want %q in %q", served.Row.ID, served.Epic, a2.ID, epicA.ID)
	}
	if served.Row.State() != model.StateOpen {
		t.Fatalf("served row state = %v, want open — the pick must rest on staleness alone, never on an orphan", served.Row.State())
	}
}

// The announcement is the visible half of the takeover verdicts above. An
// in-progress row reaches a lane this checkout does not hold only once the
// orphan annotation has refuted its holder's claim, so calling that "starting"
// promises greenfield on a ticket that may carry another checkout's unmerged
// working tree.
func TestClaimAnnouncementDistinguishesTakeoverFromFreshStart(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	fresh := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	h.transition(inFlight.ID, model.Start{Assignee: "other"})

	rows, _ := h.gather()

	for _, tc := range []struct{ name, id, want string }{
		{"an open row begins the work", fresh.ID, "starting " + fresh.ID + " claims A#1"},
		{"an in-flight row inherits it", inFlight.ID, "taking over " + inFlight.ID + " (in progress, abandoned) — claims A#1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimAnnouncement(rowByID(t, rows, tc.id), "A#1"); got != tc.want {
				t.Fatalf("claimAnnouncement = %q, want %q", got, tc.want)
			}
		})
	}
}

// Step 1 competes two capacities in one pick, and the comment above `pick`
// warns that ranking them against each other "would quietly reintroduce this
// ticket's headline symptom" — a warning earned, because an earlier draft
// looped over `accept` in preference order and did exactly that. Run in both
// rank orders: a preference for either capacity fails one arm.
func TestRouteNextStep1RanksAcrossCapacitiesRatherThanBetweenThem(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resumeFirst bool
	}{
		{"the resumable lane ranks first", true},
		{"the servable lane ranks first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
			mk := func(title, lane string) model.Issue {
				return h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: lane})
			}
			var inFlight, ready model.Issue
			if tc.resumeFirst {
				inFlight, ready = mk("A.resume", "r"), mk("A.serve", "s")
			} else {
				ready, inFlight = mk("A.serve", "s"), mk("A.resume", "r")
			}
			h.transition(inFlight.ID, model.Start{Assignee: "tester"})

			rows, details := h.gather()
			standings := claims.Standings{
				laneOf(t, details, rowByID(t, rows, inFlight.ID)): heldBy(selfAttribution),
				laneOf(t, details, rowByID(t, rows, ready.ID)):    heldBy(selfAttribution),
			}

			outcome := routeNext(rows, details, standings, selfAttribution)
			if tc.resumeFirst {
				resumed, ok := outcome.(ResumedOwnWork)
				if !ok || resumed.Row.ID != inFlight.ID {
					t.Fatalf("routeNext = %#v (%T), want ResumedOwnWork on %q — composite rank decides, never a preference for servable work", outcome, outcome, inFlight.ID)
				}
				return
			}
			served, ok := outcome.(ServedFromClaim)
			if !ok || served.Row.ID != ready.ID {
				t.Fatalf("routeNext = %#v (%T), want ServedFromClaim on %q — composite rank decides, never a preference for resumable work", outcome, outcome, ready.ID)
			}
		})
	}
}
