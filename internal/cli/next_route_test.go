package cli

import (
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

var (
	selfAttribution  = model.NewAttribution("self-stream", "ws")
	otherAttribution = model.NewAttribution("other-stream", "ws")
)

func heldBy(who model.Attribution) claims.Standing {
	return claims.Held{Tenure: claims.Tenure{By: who}}
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
	if _, err := h.ap.Store.Apply(h.ctx, a1.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("Start(A.1) error = %v", err)
	}
	// A.2 sits in its own lane so the lane gate does not block it behind the
	// in_progress default-lane sibling A.1 — this test's contract is claim
	// precedence over rank, not lane-gate membership.
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
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
// actively driving is routed around, visible but not pullable."
func TestRouteNextRoutesAroundLaneHeldByAnother(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution)}

	// This checkout holds no claims of its own — self.Present() is false —
	// so routing starts straight at the global pool, exactly as design-docs/
	// work-claims.md specifies for the zero state.
	outcome := routeNext(rows, details, standings, model.Attribution{})
	served, ok := outcome.(ServedFromGlobal)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromGlobal", outcome, outcome)
	}
	if served.Row.ID != c1.ID {
		t.Fatalf("served = %q, want %q (B.1's lane is held elsewhere and must be skipped)", served.Row.ID, c1.ID)
	}
}

// The GRANULARITY RULING (ticket comment, 2026-08-24): once a checkout's own
// held lane has no ready row, the rest of that SAME epic's unclaimed lanes
// come before any other epic, however it ranks — "ALL LANES IN AN EPIC
// SHOULD BE SURFACED BEFORE ANY LANE FROM THE NEXT EPIC."
func TestRouteNextContinuesEpicBeforeHigherRankedOtherEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	if _, err := h.ap.Store.Apply(h.ctx, a1.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("Start(A.1) error = %v", err)
	}
	// A.2 sits in its own lane so the lane gate does not block it behind the
	// in_progress default-lane sibling A.1 — this test's contract is
	// epic-continuation, not lane-gate membership.
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(selfAttribution)}

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
// nothing ready, and the epic has no other lane to offer — is a loud
// diagnostic, never a silent hop to a leaf outside the epic. The
// GRANULARITY RULING is explicit that this is the emergency the ticket
// exists to close: "root cause ... sessions closed a child of epic A then
// hopped to epic B, repeatedly."
func TestRouteNextExhaustionNeverFallsToAnotherEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	if _, err := h.ap.Store.Apply(h.ctx, a1.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("Start(A.1) error = %v", err)
	}

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, standings, selfAttribution)
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted (never epic B's B.1)", outcome, outcome)
	}
	if len(exhausted.Epics) != 1 || exhausted.Epics[0] != epicA.ID {
		t.Fatalf("exhausted.Epics = %v, want [%q]", exhausted.Epics, epicA.ID)
	}
	if len(exhausted.Blocked) != 0 {
		t.Fatalf("exhausted.Blocked = %v, want none (A.1 is merely in_progress, nothing queued behind it)", exhausted.Blocked)
	}

	// The rendered error must be loud (non-nil) and must never leak epic B's
	// ticket into what looks like a legitimate pick.
	err = exhaustedError(exhausted)
	if err == nil {
		t.Fatal("exhaustedError(exhausted) = nil, want a diagnostic error")
	}
}

// An out-of-lane dependency that gates the claimed lane's blocked ticket is
// offered as on-path — design-docs/work-claims.md, Routing step 1 — rather
// than the checkout falling through to Exhausted or, worse, the dependency
// being served (correctly) but mislabeled as an ordinary global pick.
func TestRouteNextOffersOnPathDependency(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	if _, err := h.ap.Store.Apply(h.ctx, a1.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("Start(A.1) error = %v", err)
	}
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
	}

	outcome := routeNext(rows, details, standings, selfAttribution)
	served, ok := outcome.(ServedFromClaim)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromClaim (on-path dependency)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the on-path external dependency)", served.Row.ID, dep.ID)
	}
}

// A Stale claim is never reached by bare `next` — design-docs/work-claims.md
// Routing step 5 is explicit that takeover is `lit start`'s deliberate act
// (links-claims-1ihf.7), not a default this command may exercise. A stale
// lane must be skipped exactly as a fresh foreign hold is.
func TestRouteNextSkipsStaleLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, b1.ID)): claims.Stale{Tenure: claims.Tenure{By: otherAttribution}},
	}

	outcome := routeNext(rows, details, standings, model.Attribution{})
	served, ok := outcome.(ServedFromGlobal)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromGlobal", outcome, outcome)
	}
	if served.Row.ID != c1.ID {
		t.Fatalf("served = %q, want %q (B.1's stale lane must not be a default pull)", served.Row.ID, c1.ID)
	}
}
