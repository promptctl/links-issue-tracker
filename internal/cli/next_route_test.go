package cli

import (
	"fmt"
	"strings"
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

	// publicAttribution is what a checkout that has minted no stream token
	// computes as its own identity. `next` opens in app.AccessRead, which
	// resolves the stream through workspace.ReadStream and never mints, so
	// this is the ordinary self of any checkout before its first write --
	// including a brand-new clone, the design's own headline scenario.
	publicAttribution = model.Attribution{}
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
	rows, details, _, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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
	outcome := routeNext(rows, details, standings, model.Attribution{}, focusScope{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane", outcome, outcome)
	}
	if served.Row.ID != a2.ID {
		t.Fatalf("served = %q, want %q (epic A's other lane before epic B)", served.Row.ID, a2.ID)
	}
	if served.Lane.Epic() != epicA.ID {
		t.Fatalf("served.Lane.Epic() = %q, want %q", served.Lane.Epic(), epicA.ID)
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

			outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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
			if msg := exhausted.Error(); !strings.Contains(msg, epicA.ID) {
				t.Fatalf("exhausted.Error() = %q, want the diagnostic to name the exhausted scope %q", msg, epicA.ID)
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (on-path dependency)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the on-path external dependency)", served.Row.ID, dep.ID)
	}
	if want := laneOf(t, details, served.Row); served.Lane != want {
		t.Fatalf("served.Lane = %v, want %v; the pick would claim a lane this checkout does not hold, and it is the dependency's own lane that gets claimed", served.Lane, want)
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

	rows, details, _, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{IssueType: model.TypeBug})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the on-path dependency's lane is held fresh elsewhere, so `next` must not offer what `start` would refuse", outcome, outcome)
	}
	named := false
	for _, blocked := range exhausted.Blocked {
		if blocked.ID != dep.ID {
			continue
		}
		named = true
		if blocked.Kind != reachHeldFresh {
			t.Fatalf("blocked dependency %q classified %v, want reachHeldFresh — another checkout holds its lane fresh", dep.ID, blocked.Kind)
		}
	}
	if !named {
		t.Fatalf("exhausted.Blocked = %v, want it to name %q — routing around the dependency must not hide it", exhausted.Blocked, dep.ID)
	}
	// The diagnostic must not undo the routing decision one line later: `lit
	// start` on this dependency hits the fresh-takeover gate, so telling the
	// agent to start it is N2 in the message instead of the pick.
	msg := exhausted.Error()
	if !strings.Contains(msg, dep.ID) {
		t.Fatalf("exhausted.Error() = %q, want it to name the blocker %q", msg, dep.ID)
	}
	if !strings.Contains(msg, "claimed by another checkout") {
		t.Fatalf("exhausted.Error() = %q, want it to name %q as held elsewhere rather than as work to pick up", msg, dep.ID)
	}
	if strings.Contains(msg, "`lit start` it") {
		t.Fatalf("exhausted.Error() = %q, want no instruction to start %q — `lit start` would hit the fresh-takeover gate", msg, dep.ID)
	}
}

// A blocker this run cannot see. gatherWorkableAnnotated narrows rows by
// --type/--labels/--assignee and to leaves, while the OpenDependency
// annotation comes from the store, so `lit next --type bug` can be gated by a
// task it never gathered. The diagnostic then knows the id and nothing else,
// and saying anything about its standing would be invention.
func TestExhaustionNamesABlockerOutsideThisViewAsSuch(t *testing.T) {
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
	// The narrowed gather: the dependency is unclaimed and ready, and simply
	// not in this run's rows.
	var inView []annotation.AnnotatedIssue
	for _, row := range rows {
		if row.ID == dep.ID {
			continue
		}
		inView = append(inView, row)
	}

	outcome := routeNext(inView, details, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the gating dependency is not in view, so nothing here is startable", outcome, outcome)
	}
	named := false
	for _, blocked := range exhausted.Blocked {
		if blocked.ID != dep.ID {
			continue
		}
		named = true
		if blocked.Kind != reachOutOfView {
			t.Fatalf("blocked dependency %q classified %v, want reachOutOfView — it is absent from the gathered rows", dep.ID, blocked.Kind)
		}
	}
	if !named {
		t.Fatalf("exhausted.Blocked = %v, want it to name %q", exhausted.Blocked, dep.ID)
	}
	msg := exhausted.Error()
	if !strings.Contains(msg, "outside this view") {
		t.Fatalf("exhausted.Error() = %q, want %q named as outside this view", msg, dep.ID)
	}
	if strings.Contains(msg, "claimed by another checkout") {
		t.Fatalf("exhausted.Error() = %q, want no claim about who holds %q — this run never gathered it", msg, dep.ID)
	}
	if strings.Contains(msg, "`lit start` it") {
		t.Fatalf("exhausted.Error() = %q, want no instruction to start %q", msg, dep.ID)
	}
}

// A blocker nobody holds, blocked by a further dependency of its own. It is
// unreachable for a readiness reason, not a claims reason, so the diagnostic
// must not name a holder — the agent's next move is down the chain, not to
// stand down.
func TestExhaustionNamesAnUnreadyBlockerWithoutNamingAHolder(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	deeper := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocker's blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)
	h.addDependency(dep.ID, deeper.ID)

	rows, details := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the on-path dependency is itself blocked", outcome, outcome)
	}
	named := false
	for _, blocked := range exhausted.Blocked {
		if blocked.ID != dep.ID {
			continue
		}
		named = true
		if blocked.Kind != reachNotReady {
			t.Fatalf("blocked dependency %q classified %v, want reachNotReady — nobody holds its lane; it is blocked by %q", dep.ID, blocked.Kind, deeper.ID)
		}
	}
	if !named {
		t.Fatalf("exhausted.Blocked = %v, want it to name %q", exhausted.Blocked, dep.ID)
	}
	msg := exhausted.Error()
	if !strings.Contains(msg, "not startable right now") {
		t.Fatalf("exhausted.Error() = %q, want %q named as not startable", msg, dep.ID)
	}
	if strings.Contains(msg, "claimed by another checkout") {
		t.Fatalf("exhausted.Error() = %q, want no holder named for %q — its lane is unclaimed", msg, dep.ID)
	}
	if strings.Contains(msg, "`lit start` it") {
		t.Fatalf("exhausted.Error() = %q, want no instruction to start %q — it is blocked by %q", msg, dep.ID, deeper.ID)
	}
}

// Step 2's other admission. Every stale-foreign test for this step uses an
// open row, which reaches takeoverWork through the lane's staleness and
// announces as a plain start; this is the orphaned-in-flight disjunct, which
// is the case ServedFromEpicLane's doc comment actually asserts and the only
// one that announces as a takeover.
func TestRouteNextTakesOverAnAbandonedSiblingLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	h.transition(a2.ID, model.Start{Assignee: "other"})

	rows, details := h.gather()
	orphan(t, rows, a2.ID)
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): staleBy(otherAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane — an abandoned sibling lane of our own epic is takeable", outcome, outcome)
	}
	if served.Row.ID != a2.ID || served.Lane.Epic() != epicA.ID {
		t.Fatalf("served = %q in epic %q, want %q in %q", served.Row.ID, served.Lane.Epic(), a2.ID, epicA.ID)
	}
	advice := startAdvice(served.Row, served.Lane)
	if !strings.Contains(advice, "take over") || !strings.Contains(advice, "in progress and abandoned") {
		t.Fatalf("startAdvice = %q, want a takeover — this pick inherits %q's unfinished work rather than beginning it", advice, a2.ID)
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane (a stale sibling lane of our own epic is admitted)", outcome, outcome)
	}
	if served.Row.ID != a2.ID || served.Lane.Epic() != epicA.ID {
		t.Fatalf("served = %q in epic %q, want %q in %q", served.Row.ID, served.Lane.Epic(), a2.ID, epicA.ID)
	}
	if served.Row.State() != model.StateOpen {
		t.Fatalf("served row state = %v, want open — the pick must rest on staleness alone, never on an orphan", served.Row.State())
	}
}

// startAdvice varies on two axes, and this pins every cell of the product.
//
// The verb is the takeover verdicts above made visible. An in-progress row
// reaches a lane this checkout does not hold only once the orphan annotation has
// refuted its holder's claim, so calling that a plain claim promises greenfield
// on a ticket that may carry another checkout's unmerged working tree.
//
// The object is the lane, and each of LaneID's three shapes once rendered
// through String() into a sentence that misinformed the reader
// (links-next-output-5aee): a solo lane spelled the ticket's own id, so the line
// read "starting X claims X" and no reader could take a tautology as advice
// about a command they had yet to run; an epic's default lane, whose key is
// empty, trailed a bare "#" that reads as an unfilled template slot. Only the
// named lane ever carried information, and it is the rarest of the three.
//
// Both unnamed cells read "it", and they read it in different places: "claim
// it", but "take it over", because a particle verb splits around a pronoun.
// That is why startAdvice spells its four sentences out instead of substituting
// one object into two.
//
// Whatever else changes here, no cell may contain "#" or say the ticket's id
// where a lane belongs — that is the whole of the ticket's second defect, and a
// table is the only way to see all three shapes fail at once.
func TestStartAdviceNamesTheCommandAndTheLaneShape(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	named := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	unnamed := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	solo := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Parentless", Topic: "next", IssueType: "task", Priority: 0})
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.3", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a3"})
	unnamedInFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.4", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	soloInFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Parentless, abandoned", Topic: "next", IssueType: "task", Priority: 0})
	for _, abandoned := range []string{inFlight.ID, unnamedInFlight.ID, soloInFlight.ID} {
		h.transition(abandoned, model.Start{Assignee: "other"})
	}

	rows, details := h.gather()

	for _, tc := range []struct{ name, id, want string }{
		{"a named lane is the one shape worth naming", named.ID,
			"run `lit start " + named.ID + "` to claim lane a1 of epic " + epicA.ID},
		{"an epic's default lane is words, not a trailing #", unnamed.ID,
			"run `lit start " + unnamed.ID + "` to claim the default lane of epic " + epicA.ID},
		{"a solo lane is the ticket, so the lane goes unnamed", solo.ID,
			"run `lit start " + solo.ID + "` to claim it"},
		{"an in-flight row is taken over, not claimed fresh", inFlight.ID,
			inFlight.ID + " is in progress and abandoned — run `lit start " + inFlight.ID + "` to take over lane a3 of epic " + epicA.ID},
		{"a default lane is still words on the takeover verb", unnamedInFlight.ID,
			unnamedInFlight.ID + " is in progress and abandoned — run `lit start " + unnamedInFlight.ID + "` to take over the default lane of epic " + epicA.ID},
		{"a solo lane still goes unnamed on the takeover verb", soloInFlight.ID,
			soloInFlight.ID + " is in progress and abandoned — run `lit start " + soloInFlight.ID + "` to take it over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := rowByID(t, rows, tc.id)
			got := startAdvice(row, laneOf(t, details, row))
			if got != tc.want {
				t.Fatalf("startAdvice = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "#") {
				t.Fatalf("startAdvice = %q, want no %q — String()'s lane grammar is for logs, not for a sentence", got, "#")
			}
			if !strings.Contains(got, "run `lit start "+tc.id+"`") {
				t.Fatalf("startAdvice = %q, want it to name the command the reader would run", got)
			}
		})
	}
}

// The tautology, stated as its own premise rather than left implicit in the
// table above. A parentless ticket's lane IS that ticket — LaneOf keys a solo
// lane by the issue id — so any advice that names both says one id twice. The
// table's expected string would survive a rename of the phrase "it"; this does
// not survive naming the lane at all.
func TestStartAdviceNeverSpellsASoloTicketTwice(t *testing.T) {
	h := newReadyTestHarness(t)
	solo := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Parentless", Topic: "next", IssueType: "task", Priority: 0})

	rows, details := h.gather()
	row := rowByID(t, rows, solo.ID)
	got := startAdvice(row, laneOf(t, details, row))

	if n := strings.Count(got, solo.ID); n != 1 {
		t.Fatalf("startAdvice = %q names %q %d times, want exactly 1 — a solo lane is its ticket, so naming the lane repeats the id and reads as a log line rather than as advice", got, solo.ID, n)
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

			outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
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

// Step 1b applies the verdict itself rather than going through pick/accept, so
// its admission of a takeover is a path of its own: a dependency gating our
// lane, abandoned in flight by the checkout that holds its lane, is offered
// here even though the global-pool step never sees it.
func TestRouteNextTakesOverAnAbandonedOnPathDependency(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)
	h.transition(dep.ID, model.Start{Assignee: "other"})

	rows, details := h.gather()
	orphan(t, rows, dep.ID)
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):  heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): staleBy(otherAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (the abandoned on-path dependency is takeable)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the dependency gating our own blocked lane)", served.Row.ID, dep.ID)
	}
	if served.Row.State() != model.StateInProgress {
		t.Fatalf("served row state = %v, want in_progress — the point of this path is that step 1b admits a takeover", served.Row.State())
	}
}

// The shape links-claims-gxxw was reported against, pinned end to end: this
// checkout's own lane holds the top-ranked open row, the claim on it has gone
// stale, and another epic's unclaimed leaf sits below it. `next` serves the
// top row and announces nothing — a stale claim of our own is evidence we
// stepped away, never permission to start a lane somewhere else.
//
// The arm is capacityFor's laneOurs branch reached with an OPEN row, which no
// other test drives under staleness: the fresh-hold case
// (TestRouteNextServesOwnClaimOverHigherRankedUnclaimedLane) never goes stale,
// and the stale case (TestRouteNextResumesOwnOrphanInStaleLane) reaches
// resumeWork through an orphan. Between them sat the exact combination the
// field report showed, untested.
func TestRouteNextServesOpenWorkInOurOwnStaleLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

	rows, details := h.gather()
	if rows[0].ID != a1.ID {
		t.Fatalf("fixture rank order = %q first, want %q — this test's premise is that our own lane ranks top", rows[0].ID, a1.ID)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): staleBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromClaim)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromClaim — never a fresh lane in epic B (%s)", outcome, outcome, b1.ID)
	}
	if served.Row.ID != a1.ID {
		t.Fatalf("served = %q, want %q (open work in our own lane, stale or not)", served.Row.ID, a1.ID)
	}
}

// TestRouteNextDoesNotAdoptStalePublicHistory is the routing half of the ruling
// that a bucket identity is a holder but never a proof of identity, and it
// pins the regression the public-checkout change introduced before it was
// caught in review.
//
// The setup is the normal state of a freshly upgraded repository read by a
// brand-new checkout: pre-attribution history derives as a STALE hold by the
// public checkout, and the checkout asking is itself unminted, so both sides
// of the comparison are the zero Attribution. On bare equality that reads as
// "our own lane we stepped away from", and `next` would hand back an epic this
// checkout never touched -- announcing it as work already in flight in a lane
// it holds. Every such lane in the backlog matches, so the failure is not one
// stray pick but the whole backlog being adopted at once.
//
// What must NOT come back is ResumedOwnWork; that is the load-bearing half.
// The lane is a stale foreign one, so the orphan in it is offered as the
// takeover it actually is, with the provenance a takeover carries.
func TestRouteNextDoesNotAdoptStalePublicHistory(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "whoever-came-before"})

	rows, details := h.gather()
	orphan(t, rows, a1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): staleBy(publicAttribution)}

	outcome := routeNext(rows, details, standings, publicAttribution, focusScope{})
	if resumed, adopted := outcome.(ResumedOwnWork); adopted {
		t.Fatalf("routeNext resumed %q as this checkout's own work; an unminted self shares the public bucket with the history, which proves both unaddressable, not both us", resumed.Row.ID)
	}
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane: a stale public lane is a takeover, carrying its provenance", outcome, outcome)
	}
	if served.Row.ID != a1.ID {
		t.Fatalf("served = %q, want %q", served.Row.ID, a1.ID)
	}
}

// TestRouteNextRoutesAroundAFreshPublicHold is the fresh half. A checkout with
// no token has recorded nothing, so a fresh unattributed hold is somebody else's
// work in flight — a binary older than attribution, or history from the hours
// before an upgrade — and routing walks around it rather than resuming it.
func TestRouteNextRoutesAroundAFreshPublicHold(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(a1.ID, model.Start{Assignee: "whoever-is-working-it"})

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(publicAttribution)}

	outcome := routeNext(rows, details, standings, publicAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane: a fresh public hold is foreign work in flight", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q (the held lane routed around)", served.Row.ID, b1.ID)
	}
}

// reachNotes is indexed by the kind, so no diagnostic can drop a kind's ids the
// way a list of pairs could — but Go fills an unlisted index with "", and the
// renderer would then print those ids under an empty parenthetical. The array
// makes the failure loud; only this makes it impossible.
//
// It asserts over reachKindCount rather than over four names on purpose: a fifth
// kind added to the enum fails here until both diagnostics have words for it,
// which is the whole reason the bound exists.
func TestEveryReachKindHasWordsInBothDiagnostics(t *testing.T) {
	t.Parallel()
	for _, diagnostic := range []struct {
		name  string
		notes reachNotes
	}{
		{"exhausted", exhaustedNotes},
		{"pool", poolNotes},
	} {
		for kind := reachKind(0); kind < reachKindCount; kind++ {
			if diagnostic.notes[kind] == "" {
				t.Fatalf("%s notes have nothing to say about reachKind %d — its ids would render under an empty parenthetical", diagnostic.name, kind)
			}
		}
	}
}

// NoWork was an empty struct, so "no ready work" answered two opposite
// questions in identical words: an empty backlog, and a backlog full of work
// this checkout may not have. The walk knew every row and every verdict at the
// moment it threw them away (links-cli-q7hg).
//
// Both surviving kinds are put in one pool on purpose. A single-kind fixture
// passes against a renderer that prints one note for everything it went past,
// which is the bool this type replaced: the message has to say that one row is
// somebody's live work and the other is merely gated, because those call for
// different acts.
func TestNoWorkNamesEachRowThePoolWalkWentPast(t *testing.T) {
	h := newReadyTestHarness(t)
	held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs, in flight", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(held.ID, model.Start{Assignee: "tester"})
	// Gated by the row above, so the whole pool is unstartable without either
	// row being takeable — and for two different reasons.
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ours to want, not to start", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(gated.ID, held.ID)

	rows, details := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, held.ID)): heldBy(otherAttribution)}

	// This checkout holds nothing, so routing starts straight at the global pool.
	outcome := routeNext(rows, details, standings, selfAttribution, focusScope{})
	noWork, ok := outcome.(NoWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want NoWork — nothing in the pool is takeable", outcome, outcome)
	}
	want := map[string]reachKind{held.ID: reachHeldFresh, gated.ID: reachNotReady}
	got := map[string]reachKind{}
	for _, row := range noWork.Unreachable {
		got[row.ID] = row.Kind
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("NoWork.Unreachable[%q] = %v, want %v — the walk's own verdict, kept rather than discarded (got %v)", id, got[id], kind, got)
		}
	}

	msg := noWork.Error()
	if msg == "no ready work" {
		t.Fatalf("NoWork.Error() = %q — the bare sentence is the empty-backlog answer, and this backlog has %d rows in it", msg, len(rows))
	}
	if !strings.Contains(msg, "the backlog is not empty") {
		t.Fatalf("NoWork.Error() = %q, want it to refute the empty reading outright", msg)
	}
	for _, id := range []string{held.ID, gated.ID} {
		if !strings.Contains(msg, id) {
			t.Fatalf("NoWork.Error() = %q, want it to name %q — a row walked past and not named is a row the agent is told does not exist", msg, id)
		}
	}
	if !strings.Contains(msg, "another checkout holds right now") {
		t.Fatalf("NoWork.Error() = %q, want %q reported as another checkout's live work", msg, held.ID)
	}
	if !strings.Contains(msg, "blocked by a dependency") {
		t.Fatalf("NoWork.Error() = %q, want %q reported as gated rather than as somebody's live work", msg, gated.ID)
	}
}

// The other half of the same type, and criterion 2 of the ticket: an empty
// backlog answers exactly as it always has. The new clause is driven entirely by
// the rows the walk went past, so no rows means no clause — pinned byte-for-byte,
// because "no ready work" is still the whole truth when there is nothing to say
// why about.
func TestNoWorkOnAGenuinelyEmptyBacklogIsUnchanged(t *testing.T) {
	h := newReadyTestHarness(t)

	rows, details := h.gather()
	outcome := routeNext(rows, details, claims.Standings{}, selfAttribution, focusScope{})
	noWork, ok := outcome.(NoWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want NoWork for an empty backlog", outcome, outcome)
	}
	if len(noWork.Unreachable) != 0 {
		t.Fatalf("NoWork.Unreachable = %v, want empty — nothing was gathered, so nothing was gone past", noWork.Unreachable)
	}
	if got := noWork.Error(); got != "no ready work" {
		t.Fatalf("NoWork.Error() = %q, want exactly %q unchanged", got, "no ready work")
	}
}

// The pool walk hands NoWork every row it went past, which in a busy workspace
// is the whole filtered backlog rather than the epic-scoped handful Exhausted
// reports. The clause has to stay one readable line at that size without lying
// about how much of it went unnamed.
//
// Asserted per kind rather than per message on purpose: describeReach groups the
// rows by kind and renders a clause each, so a cap applied to the joined message
// would pass a single-kind fixture and still emit an unbounded line the moment a
// second kind appeared.
func TestReachClauseNamesABoundedPrefixAndCountsTheRest(t *testing.T) {
	t.Parallel()

	rowsOfKind := func(kind reachKind, prefix string, n int) []rowReach {
		rows := make([]rowReach, 0, n)
		for i := range n {
			rows = append(rows, rowReach{ID: fmt.Sprintf("%s-%03d", prefix, i), Kind: kind})
		}
		return rows
	}

	t.Run("a pool over the cap names the cap's worth and counts the remainder", func(t *testing.T) {
		over := 7
		msg := NoWork{Unreachable: rowsOfKind(reachHeldFresh, "held", maxNamedPerKind+over)}.Error()

		if want := fmt.Sprintf("and %d more", over); !strings.Contains(msg, want) {
			t.Fatalf("NoWork.Error() = %q, want %q — a truncation that omits the count tells the agent the backlog is smaller than it is", msg, want)
		}
		if named := fmt.Sprintf("held-%03d", maxNamedPerKind-1); !strings.Contains(msg, named) {
			t.Fatalf("NoWork.Error() = %q, want it to name %q — the last id inside the cap is named, not counted", msg, named)
		}
		if dropped := fmt.Sprintf("held-%03d", maxNamedPerKind); strings.Contains(msg, dropped) {
			t.Fatalf("NoWork.Error() = %q, want %q left to the count — naming it means the cap never bit", msg, dropped)
		}
	})

	t.Run("a pool at the cap is named whole", func(t *testing.T) {
		msg := NoWork{Unreachable: rowsOfKind(reachHeldFresh, "held", maxNamedPerKind)}.Error()

		if strings.Contains(msg, "more") {
			t.Fatalf("NoWork.Error() = %q, want no remainder clause — every row is named, so there is nothing left to count", msg)
		}
		for i := range maxNamedPerKind {
			if id := fmt.Sprintf("held-%03d", i); !strings.Contains(msg, id) {
				t.Fatalf("NoWork.Error() = %q, want it to name %q — a set at the cap loses nothing", msg, id)
			}
		}
	})

	t.Run("the cap is per kind, so one kind cannot spend another's budget", func(t *testing.T) {
		over := 4
		pool := append(
			rowsOfKind(reachHeldFresh, "held", maxNamedPerKind+over),
			rowsOfKind(reachNotReady, "gated", maxNamedPerKind+over)...,
		)
		msg := NoWork{Unreachable: pool}.Error()

		if got := strings.Count(msg, fmt.Sprintf("and %d more", over)); got != 2 {
			t.Fatalf("NoWork.Error() = %q, counted %d remainder clauses, want 2 — each kind reports its own tail, or the second kind's rows vanish into the first kind's count", msg, got)
		}
		for _, id := range []string{"held-000", "gated-000"} {
			if !strings.Contains(msg, id) {
				t.Fatalf("NoWork.Error() = %q, want it to name %q — both kinds get a clause, and each names its own prefix", msg, id)
			}
		}
	})
}
