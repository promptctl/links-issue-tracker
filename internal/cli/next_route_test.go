package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// These tests exercise routeNext directly against hand-built claims.Standings
// rather than through claims.Derive: the predicate that turns evidence into a
// Standing is already proven by internal/claims's own tests. What routeNext
// adds is the selection precedence GIVEN a Standing per lane, so these tests
// hold the standings fixed and vary only the routing question.
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

// expired is the standing of a lane whose claim has aged out: the same
// Unclaimed every never-started lane derives, because an expired claim is not
// a claim and the type has nowhere to record that one existed.
var expired claims.Standing = claims.Unclaimed{}

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

func (h readyTestHarness) gather() ([]annotation.AnnotatedIssue, map[string]storage.IssueRelations, map[string]storage.IssueRelations) {
	h.t.Helper()
	gathered, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	return gathered.rows, gathered.details, gathered.epics
}

// A checkout's own held lane wins over a higher-ranked, entirely unclaimed
// epic — routing step 1 outranks plain backlog order. Without claims, `next`
// would return B.1 (top composite rank); with the checkout's own claim on
// epic A, it must not.
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

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
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
// links-claims-1b0p acceptance 4: an expired hold is no hold at all, and a live
// one must be left exactly where it was.
func TestRouteNextRoutesAroundLaneHeldByAnother(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution)}

	// This checkout holds no claims of its own, so routing starts straight at
	// the global pool, exactly as design-docs/work-claims.md specifies for the
	// zero state.
	outcome := routeNext(rows, details, epics, standings, model.Attribution{}, focusScope{})
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
// "No work left" is literal: A.1 is DONE, so its lane holds nothing to serve
// or resume. A lane with work still in flight is handed that work back
// (TestRouteNextResumesOwnInFlightTicket), so epic continuation has to be
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

	rows, details, epics := h.gather()
	// A.1 is closed, so it is not among the gathered rows — its lane is
	// addressed straight from the issue, which is the shape of the real case:
	// a claim outlives the ticket that established it.
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
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
// nothing, and the epic has no other lane to offer — does not trap the
// checkout there. `next` serves the top ready ticket outside the epic, carrying
// the exhaustion so the pick is announced beside why the epic stopped, never
// as a silent hop (links-next-5sxz).
//
// This outcome is reachable only while the claim is live.
func TestRouteNextServesPastAnExhaustedEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedPastExhaustion)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedPastExhaustion serving epic B's B.1", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q", served.Row.ID, b1.ID)
	}
	if served.Lane.Epic() != epicB.ID {
		t.Fatalf("served.Lane.Epic() = %q, want %q — starting the pick claims epic B's lane", served.Lane.Epic(), epicB.ID)
	}
	if len(served.Exhaustion.Epics) != 1 || served.Exhaustion.Epics[0] != epicA.ID {
		t.Fatalf("exhaustion.Epics = %v, want [%q]", served.Exhaustion.Epics, epicA.ID)
	}
	if len(served.Exhaustion.Blocked) != 0 {
		t.Fatalf("exhaustion.Blocked = %v, want none (epic A has nothing queued)", served.Exhaustion.Blocked)
	}

	// Nothing blocks epic A, so there is nothing a new ticket could clear:
	// moving on is the only route, and it is not phrased as an alternative.
	var out strings.Builder
	if _, err := renderNextOutcome(&out, served, details, claimContext{}, ""); err != nil {
		t.Fatalf("renderNextOutcome error = %v", err)
	}
	text := out.String()
	if strings.Contains(text, "to stay") {
		t.Fatalf("rendered = %q, want no stay route — nothing blocks epic A", text)
	}
	if !strings.Contains(text, "\nmove on to the top ready ticket outside it: run `lit start "+b1.ID) {
		t.Fatalf("rendered = %q, want the move-on route alone on its line, naming %q", text, b1.ID)
	}
}

// With nothing ready outside the exhausted epic either, exhaustion is the
// answer. Nothing blocks epic A here — its lane is merely done — so no route
// stays: a new ticket would have nothing to clear.
func TestRouteNextExhaustionIsTerminalWhenNothingElseIsReady(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — B.1's lane is held elsewhere, so nothing outside epic A is ready", outcome, outcome)
	}
	msg := exhausted.Error()
	if want := "no ready work in epic(s) " + epicA.ID + " — nothing else is queued behind what's already in progress; `lit next` has nothing ready outside it either"; msg != want {
		t.Fatalf("exhausted.Error() = %q, want exactly %q", msg, want)
	}
}

// A blocked epic with nothing ready outside it names the one route left:
// stay, file the ticket that clears the blocker under the epic — spelled with
// the epic's own id — and make the blocker wait on it, since filing alone
// leaves the blocked work waiting on what it waited on.
func TestRouteNextTerminalExhaustionNamesTheStayRouteAgainstABlock(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside, theirs", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, blocker.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, blocker.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row outside epic A is another checkout's", outcome, outcome)
	}
	want := "no ready work in epic(s) " + epicA.ID + " — blocked on " + blocker.ID +
		" (on your path but claimed by another checkout right now); `lit next` has nothing ready outside it either" +
		" — to stay, file the ticket that clears a blocker under the epic with `lit new --parent " + epicA.ID + " --top`, then make that blocker wait on it with `lit dep add --from <new> --to <blocker>`"
	if msg := exhausted.Error(); msg != want {
		t.Fatalf("exhausted.Error() = %q, want exactly %q", msg, want)
	}

	// Follow the route as printed. The edge goes through the CLI's own policy
	// boundary, which refuses a blocks edge inside one epic, and the next
	// route serves the new ticket from the lane this checkout holds.
	unblocker := followStayRoute(t, h, []string{"--parent", epicA.ID, "--top"}, blocker.ID)
	rows, details, epics = h.gather()
	outcome = routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	if served, ok := outcome.(ServedFromClaim); !ok || served.Row.ID != unblocker {
		t.Fatalf("after following the stay route, routeNext = %#v (%T), want ServedFromClaim serving %s", outcome, outcome, unblocker)
	}
}

// followStayRoute runs the two commands the stay route prints — `lit new` with
// the flags it names, then `lit dep add` making the blocker wait on the new
// ticket — through the CLI's own handlers, so a route naming a placement or an
// edge lit refuses or mis-ranks fails here. It returns the new ticket's id.
func followStayRoute(t *testing.T, h readyTestHarness, newFlags []string, blockerID string) string {
	t.Helper()
	var out strings.Builder
	args := append([]string{"--title", "Clears the blocker", "--topic", "next", "--type", "task"}, newFlags...)
	if err := runNew(h.ctx, &out, h.ap, args); err != nil {
		t.Fatalf("lit new %v, as the stay route prints it: %v", newFlags, err)
	}
	newID := strings.Fields(out.String())[0]
	if err := runDepAdd(h.ctx, io.Discard, h.ap, []string{"--from", newID, "--to", blockerID}); err != nil {
		t.Fatalf("lit dep add --from %s --to %s, as the stay route prints it: %v", newID, blockerID, err)
	}
	return newID
}

// When the block is declared on the epic itself, every child inherits it — a
// ticket filed under the epic starts out held back too. The stay route still
// files it there, because the edge it prints next is what frees it: an epic's
// blocker never holds back a child it waits on. Both halves are observed, the
// new child held back before the edge and served after it.
func TestRouteNextStayRouteWorksWhenTheEpicItselfIsBlocked(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocks the whole epic, theirs", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(epicA.ID, blocker.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, blocker.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row outside epic A is another checkout's", outcome, outcome)
	}
	if want := "under the epic with `lit new --parent " + epicA.ID + " --top`, then make that blocker wait on it"; !strings.Contains(exhausted.Error(), want) {
		t.Fatalf("exhausted.Error() = %q, want it to contain %q", exhausted.Error(), want)
	}

	var out strings.Builder
	if err := runNew(h.ctx, &out, h.ap, []string{"--title", "Clears the blocker", "--topic", "next", "--type", "task", "--parent", epicA.ID, "--top"}); err != nil {
		t.Fatalf("lit new --parent %s --top: %v", epicA.ID, err)
	}
	unblocker := strings.Fields(out.String())[0]
	rows, _, _ = h.gather()
	if ClassifyReadiness(rowByID(t, rows, unblocker).Annotations).IsReady() {
		t.Fatalf("%s under blocked epic %s is ready before the edge, want it held back by %s — else the edge below proves nothing", unblocker, epicA.ID, blocker.ID)
	}

	if err := runDepAdd(h.ctx, io.Discard, h.ap, []string{"--from", unblocker, "--to", blocker.ID}); err != nil {
		t.Fatalf("lit dep add --from %s --to %s: %v", unblocker, blocker.ID, err)
	}
	rows, details, epics = h.gather()
	outcome = routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	if served, ok := outcome.(ServedFromClaim); !ok || served.Row.ID != unblocker {
		t.Fatalf("after following the stay route, routeNext = %#v (%T), want ServedFromClaim serving %s", outcome, outcome, unblocker)
	}
}

// A dependency inside the epic is never the blocker exhaustion names. lit
// refuses a blocks edge between two tickets of one epic, yet one can exist — an
// import writes it, or it predates the rule — and a stay route named against it
// prints a `lit dep add` lit then refuses. Here the sibling is another
// checkout's work, so the epic waits on work underway and no route stays.
func TestRouteNextExhaustionNeverNamesASiblingAsTheBlocker(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2, theirs", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.3", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a3"})
	h.addDependency(a3.ID, a2.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — A.2 is another checkout's and A.3 waits on it", outcome, outcome)
	}
	if want := "no ready work in epic(s) " + epicA.ID + " — nothing else is queued behind what's already in progress; `lit next` has nothing ready outside it either"; exhausted.Error() != want {
		t.Fatalf("exhausted.Error() = %q, want exactly %q — sibling %s is not a blocker to clear from outside", exhausted.Error(), want, a2.ID)
	}
}

// A sibling that is itself blocked is not named either: it is an open row of
// the epic, so the walk reaches past it to what holds it back, and the stay
// route names that — an edge lit accepts, followed here as printed.
func TestRouteNextExhaustionNamesTheOutsideBlockerBehindASibling(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.3", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a3"})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside, theirs", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a3.ID, a2.ID)
	h.addDependency(a2.ID, blocker.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, blocker.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row outside epic A is another checkout's", outcome, outcome)
	}
	want := "no ready work in epic(s) " + epicA.ID + " — blocked on " + blocker.ID +
		" (on your path but claimed by another checkout right now); `lit next` has nothing ready outside it either" +
		" — to stay, file the ticket that clears a blocker under the epic with `lit new --parent " + epicA.ID + " --top`, then make that blocker wait on it with `lit dep add --from <new> --to <blocker>`"
	if msg := exhausted.Error(); msg != want {
		t.Fatalf("exhausted.Error() = %q, want exactly %q — naming %s alone, not sibling %s", msg, want, blocker.ID, a2.ID)
	}
	followStayRoute(t, h, []string{"--parent", epicA.ID, "--top"}, blocker.ID)
}

// Under a focus label the pool is the focus path, so an empty pool is not
// "nothing ready outside the epic": ready rows off the path were never asked
// about. They are named, with the way to ask — and our own epic's rows are
// not among them, since steps 1-2b walk those whether focus is on or not.
func TestRouteNextExhaustionNamesReadyRowsTheFocusWithheld(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	offPath := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready, off the focus path", Topic: "next", IssueType: "task", Priority: 0})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(otherAttribution),
	}
	// No gathered row carries the FocusPath annotation, so an active scope
	// withholds every one of them from the pool.
	focused := focusScope{goals: []string{"test-goal"}}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focused)
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the focus path holds nothing ready", outcome, outcome)
	}
	if len(exhausted.OffPath) != 1 || exhausted.OffPath[0].ID != offPath.ID {
		t.Fatalf("exhausted.OffPath = %+v, want exactly [%s] — %s is epic A's own, walked by steps 1-2b", exhausted.OffPath, offPath.ID, a2.ID)
	}
	msg := exhausted.Error()
	if strings.Contains(msg, "nothing ready outside it either") {
		t.Fatalf("exhausted.Error() = %q, want no claim that nothing outside is ready — %s is, off the focus path", msg, offPath.ID)
	}
	if want := "nothing on the focus path outside it is ready either: " + offPath.ID + " (off the focus path this run answered over — `lit next --all` to route over the whole queue)"; !strings.Contains(msg, want) {
		t.Fatalf("exhausted.Error() = %q, want it to contain %q", msg, want)
	}
}

// This checkout finished one ticket of epic A and walked away, its claim on the
// lane expired, and the backlog ranks another epic's leaf first. An expired
// claim is not a claim, so the checkout holds nothing, and `next` routes by
// rank from the global pool — the unclaimed leaf at the top, not the lane this
// checkout once held. The derivation answers Unclaimed for the expired lane
// (claims_test.go pins that), so what this holds is routing's half: given that
// standing, the lane's history buys it nothing. If the expired lane still
// counted as held, it would outrank the entire backlog for as long as the epic
// stayed open, and with one checkout in the repository nothing could ever
// release it.
func TestRouteNextExpiredOwnLaneDoesNotOutrankTheBacklog(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 0})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})

	rows, details, epics := h.gather()
	if rows[0].ID != b1.ID {
		t.Fatalf("fixture rank order = %q first, want %q — this test's premise is that the expired lane ranks below the pool's top", rows[0].ID, b1.ID)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a2.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane — an expired claim of our own holds nothing", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q (the backlog's top, never %q from the lane we let expire)", served.Row.ID, b1.ID, a2.ID)
	}
}

// An out-of-lane dependency that gates the claimed lane's blocked ticket is
// offered as on-path — design-docs/work-claims.md, Routing step 1 — and is
// announced as the claim it establishes. It comes back as ServedFromDependency
// and not ServedFromClaim: the dependency is by definition OUTSIDE the claimed
// lane, so starting it claims a second lane, and ServedFromClaim's contract is
// that nothing is claimed and nothing is said. It is not ServedFromNewLane
// either: that type is the global pool's, and sharing it would leave this pick
// rendering the pool's line verbatim.
func TestRouteNextOffersOnPathDependencyAsANewLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency (on-path dependency)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the on-path external dependency)", served.Row.ID, dep.ID)
	}
	// The pick is FOR the blocked row, and naming it is the whole point of the
	// outcome: a step-1b pick that cannot say what it unblocks is the bug.
	if served.Gates != a2.ID {
		t.Fatalf("served.Gates = %q, want %q — the pick must name the blocked row it unblocks, not merely be correct about which dependency to serve", served.Gates, a2.ID)
	}
	if want := laneOf(t, details, served.Row); served.Lane != want {
		t.Fatalf("served.Lane = %v, want %v; the pick would claim a lane this checkout does not hold, and it is the dependency's own lane that gets claimed", served.Lane, want)
	}
}

// One dependency can gate several of our own blocked rows, and only one id fits
// in the announcement. Which one is therefore part of the contract, not an
// accident of the walk: the queue-first gated row. The expectation here is read
// OUT OF the gathered queue rather than written in as an id, so the test pins
// the rule and cannot be satisfied by a fixture that happens to order the two
// rows the way the assertion guessed.
func TestOnPathDependencyNamesTheQueueFirstRowItGates(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.3", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a3"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)
	h.addDependency(a3.ID, dep.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a3.ID)): heldBy(selfAttribution),
	}

	// The rule's own subject, rendered from the queue: the first of the two
	// gated rows in gather order.
	var queueFirst string
	for _, row := range rows {
		if row.ID == a2.ID || row.ID == a3.ID {
			queueFirst = row.ID
			break
		}
	}
	if queueFirst == "" {
		t.Fatalf("neither %s nor %s is in the gathered queue; the fixture does not exercise the rule", a2.ID, a3.ID)
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency (one dependency gating two of our rows)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the shared external dependency)", served.Row.ID, dep.ID)
	}
	if served.Gates != queueFirst {
		t.Fatalf("served.Gates = %q, want %q — a dependency gating several of our rows names the queue-first one, so the announcement is deterministic across runs", served.Gates, queueFirst)
	}
}

// links-claims-1b0p acceptance 5 (finding N8), which involves no staleness at
// all: a checkout holding a FRESH claim, whose lane's only member is the
// in_progress ticket it is working, is handed that ticket back. It must not
// get the Exhausted diagnostic, and it must not hop.
//
// This is the promise `lit quickstart work` makes in writing — "a fresh
// session here routes back to it automatically".
func TestRouteNextResumesOwnInFlightTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	resumed, ok := outcome.(ResumedOwnWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ResumedOwnWork (the ticket this checkout is on)", outcome, outcome)
	}
	if resumed.Row.ID != a1.ID {
		t.Fatalf("resumed = %q, want %q", resumed.Row.ID, a1.ID)
	}
}

// An orphan of our own in a lane whose claim has expired is not handed back as
// ours to resume. The claim is gone, so the lane is nobody's, and the orphan is
// what it would be in any other lane: abandoned work in flight, offered from
// the global pool by rank. The ticket in flight is still served — it ranks
// first here — but from the pool, never as a resumption of a lane this checkout
// no longer holds.
func TestRouteNextServesOwnOrphanFromAnExpiredLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	orphan(t, rows, a1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	if _, resumed := outcome.(ResumedOwnWork); resumed {
		t.Fatalf("routeNext = %#v, want a pick from the pool — an expired claim holds nothing to resume", outcome)
	}
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (%q served by rank; %q ranks below it)", outcome, outcome, a1.ID, b1.ID)
	}
	if served.Row.ID != a1.ID {
		t.Fatalf("served = %q, want %q (the orphan ranks first, so rank serves it)", served.Row.ID, a1.ID)
	}
}

// links-claims-1b0p acceptance 3: the orphan is the only work in a lane
// another checkout let expire, and it is offered to a bare `lit next` here.
// Two facts make this reachable together — an expired foreign claim is no
// hold, and an in_progress row is servable at all — and either alone leaves
// the pick unreachable.
func TestRouteNextServesOrphanInAnExpiredForeignLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	h.transition(b1.ID, model.Start{Assignee: "other"})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	orphan(t, rows, b1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (the abandoned row in the expired lane)", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q (the orphan outranks unclaimed %q)", served.Row.ID, b1.ID, c1.ID)
	}
}

// The other half of the same rule: an in_progress row in a lane another
// checkout holds is somebody's work in flight, and it is left alone whether or
// not the orphan clock has reached it. A lane nobody holds is proof on its own
// that an in-flight row is takeable
// (TestRouteNextServesUnorphanedInFlightRowInAnUnheldLane), so the live hold is
// the case where "leave it" holds.
func TestRouteNextLeavesUnabandonedInFlightWorkAlone(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	h.transition(b1.ID, model.Start{Assignee: "other"})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution)}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane", outcome, outcome)
	}
	if served.Row.ID != c1.ID {
		t.Fatalf("served = %q, want %q (B.1 is in flight in a lane held fresh — leave it)", served.Row.ID, c1.ID)
	}
}

// Ownership is a fact about the workspace, so a display filter must not be able
// to change it. The checkout holds a FRESH claim on a lane whose only ticket is
// a task, and asks for bugs. Its own lane's rows vanish from the gathered set —
// and the pick it gets must still be announced as leaving its exhausted epic,
// rather than as a plain global-pool pick, which is what deriving ownership
// from the filtered rows would produce.
func TestRouteNextKeepsOwnershipUnderADisplayFilter(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicB.ID})

	gathered, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{IssueType: model.TypeBug})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	// The gather already applied --type; these are the rows `lit next` routes over.
	rows, details, epics := gathered.rows, gathered.details, gathered.epics
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedPastExhaustion)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedPastExhaustion (epic B's bug, announced as leaving epic A)", outcome, outcome)
	}
	if len(served.Exhaustion.Epics) != 1 || served.Exhaustion.Epics[0] != epicA.ID {
		t.Fatalf("exhaustion.Epics = %v, want [%q]", served.Exhaustion.Epics, epicA.ID)
	}
}

// A gating dependency sitting in a lane another checkout holds fresh is one
// `lit start` refuses, so offering it would have `next` recommend what `start`
// blocks. It is routed around like any other fresh foreign hold, and
// exhaustion still names it rather than going quiet about why there is nothing
// to do.
// [LAW:no-silent-failure]
func TestRouteNextRoutesAroundOnPathDependencyHeldFresh(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):  heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
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

	rows, details, epics := h.gather()
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

	outcome := routeNext(inView, details, epics, standings, selfAttribution, focusScope{})
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
// stand down. The further dependency's lane is held elsewhere, so nothing
// outside the epic is ready and the diagnostic is the whole answer.
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

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                        heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, deeper.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
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

// A scope stopped by its own ticket, held by a label on that ticket, names the
// ticket and the reason. Nothing in it is in progress, so the in-progress
// sentence would be false; and a ticket inside the epic cannot take a blocks
// edge, so no stay route is offered against it.
func TestExhaustionNamesTheScopesOwnTicketHeldByALabel(t *testing.T) {
	for _, tc := range []struct {
		label string
		note  string
	}{
		{label: ExternalLabel, note: "on your path but waiting on an event outside this repository"},
		{label: NeedsDesignLabel, note: "on your path but not startable right now"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
			lead := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a", Labels: []string{tc.label}})
			h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a"})

			rows, details, epics := h.gather()
			standings := claims.Standings{laneOf(t, details, rowByID(t, rows, lead.ID)): heldBy(selfAttribution)}
			outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
			exhausted, ok := outcome.(Exhausted)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want Exhausted — A.1 is held by %q and A.2 waits behind it", outcome, outcome, tc.label)
			}
			want := "no ready work in epic(s) " + epicA.ID + " — held here: " + lead.ID + " (" + tc.note
			if msg := exhausted.Error(); !strings.HasPrefix(msg, want) {
				t.Fatalf("exhausted.Error() = %q, want it to start %q", msg, want)
			}
			if msg := exhausted.Error(); strings.Contains(msg, "already in progress") || strings.Contains(msg, "to stay") {
				t.Fatalf("exhausted.Error() = %q, want neither the in-progress sentence nor a stay route — nothing is in progress and %q is inside the epic", msg, lead.ID)
			}
		})
	}
}

// The external label holds a ticket in flight too: it says no work here moves
// the ticket, which starting it did not change. So lit next neither resumes it
// for the checkout that started it nor offers it to another as abandoned work,
// and it serves the plain ticket ranked behind it instead.
func TestAnExternalTicketInFlightIsNeverServed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ownsLane bool
	}{
		{name: "in this checkout's own lane", ownsLane: true},
		{name: "in a lane nobody holds", ownsLane: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			waiting := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Upstream fix lands", Topic: "next", IssueType: "task", Priority: 0, Labels: []string{ExternalLabel}})
			h.transition(waiting.ID, model.Start{Assignee: "tester"})
			plain := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Plain work", Topic: "next", IssueType: "task", Priority: 0})

			rows, details, epics := h.gather()
			standings := claims.Standings{}
			if tc.ownsLane {
				standings[laneOf(t, details, rowByID(t, rows, waiting.ID))] = heldBy(selfAttribution)
			}
			outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
			var served annotation.AnnotatedIssue
			switch o := outcome.(type) {
			case ServedFromNewLane:
				served = o.Row
			case ServedPastExhaustion:
				served = o.Row
				if want := waiting.ID + " (on your path but waiting on an event outside this repository"; !strings.Contains(o.Exhaustion.why(), want) {
					t.Fatalf("exhaustion = %q, want it to contain %q", o.Exhaustion.why(), want)
				}
			default:
				t.Fatalf("routeNext = %#v (%T), want %q served past the external ticket", outcome, outcome, plain.ID)
			}
			if served.ID != plain.ID {
				t.Fatalf("routeNext served %q, want %q — %q is in flight but waits on an outside event", served.ID, plain.ID, waiting.ID)
			}
		})
	}
}

// A blocker the external label holds waits on an event outside the repository,
// so exhaustion names it as such and never tells the agent to file a ticket
// that would clear it: no ticket filed here can. Beside a blocker a ticket
// could clear, the route comes back, naming the blockers it can act on.
func TestExhaustionNeverOffersToClearABlockerAwaitingAnOutsideEvent(t *testing.T) {
	stayRoute := "to stay, file the ticket that clears a blocker"
	for _, tc := range []struct {
		name        string
		alsoPlain   bool
		heldForeign bool
		wantStaying bool
	}{
		{name: "external blocker alone", alsoPlain: false, wantStaying: false},
		{name: "external blocker in a lane another checkout holds", heldForeign: true, wantStaying: false},
		{name: "external blocker beside a clearable one", alsoPlain: true, wantStaying: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
			a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
			h.transition(a1.ID, model.Start{Assignee: "tester"})
			h.transition(a1.ID, model.Done{})
			a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
			upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Upstream fix lands", Topic: "next", IssueType: "task", Priority: 0, Labels: []string{ExternalLabel}})
			h.addDependency(a2.ID, upstream.ID)
			standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}
			if tc.heldForeign {
				rows, details, _ := h.gather()
				standings[laneOf(t, details, rowByID(t, rows, upstream.ID))] = heldBy(otherAttribution)
			}
			if tc.alsoPlain {
				plain := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs", Topic: "next", IssueType: "task", Priority: 0})
				h.addDependency(a2.ID, plain.ID)
				rows, details, _ := h.gather()
				standings[laneOf(t, details, rowByID(t, rows, plain.ID))] = heldBy(otherAttribution)
			}

			rows, details, epics := h.gather()
			outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
			exhausted, ok := outcome.(Exhausted)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want Exhausted — epic A's one open row waits on %q", outcome, outcome, upstream.ID)
			}
			idx := slices.IndexFunc(exhausted.Blocked, func(r rowReach) bool { return r.ID == upstream.ID })
			if idx < 0 || exhausted.Blocked[idx].Kind != reachAwaitingOutside {
				t.Fatalf("exhausted.Blocked = %v, want %q classified reachAwaitingOutside", exhausted.Blocked, upstream.ID)
			}
			msg := exhausted.Error()
			if want := upstream.ID + " (on your path but waiting on an event outside this repository"; !strings.Contains(msg, want) {
				t.Fatalf("exhausted.Error() = %q, want it to contain %q", msg, want)
			}
			if staying := strings.Contains(msg, stayRoute); staying != tc.wantStaying {
				t.Fatalf("exhausted.Error() = %q, stay route present = %v, want %v", msg, staying, tc.wantStaying)
			}
		})
	}
}

// Step 2's other admission. Every expired-foreign test for this step uses an
// open row, which announces as a plain start; this is the in-flight disjunct,
// which is the case ServedFromEpicLane's doc comment actually asserts and the
// only one whose announcement says the row is somebody's abandoned work.
func TestRouteNextServesAnAbandonedSiblingLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	h.transition(a2.ID, model.Start{Assignee: "other"})

	rows, details, epics := h.gather()
	orphan(t, rows, a2.ID)
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): expired,
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane — an abandoned sibling lane of our own epic is servable", outcome, outcome)
	}
	if served.Row.ID != a2.ID || served.Lane.Epic() != epicA.ID {
		t.Fatalf("served = %q in epic %q, want %q in %q", served.Row.ID, served.Lane.Epic(), a2.ID, epicA.ID)
	}
	advice := startAdvice(served.Row, served.Lane)
	if !strings.Contains(advice, "in progress and nobody holds it") {
		t.Fatalf("startAdvice = %q, want it to say the row is in flight and unheld — this pick inherits %q's unfinished work rather than beginning it", advice, a2.ID)
	}
}

// Design step 6 in its plainest form: an expired claim does not veto an
// otherwise-ready ticket. Nothing here is started and nothing is orphaned, so
// the pick rests on the lane being unheld alone — the `!started && IsReady()`
// half of capacityFor that every other expired-foreign test in this file
// reaches only through an in-progress row.
func TestRouteNextServesOpenTicketInAnExpiredForeignLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic C", Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "C.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (the expired lane is nobody's, so its ticket is served)", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q — an expired claim must not push a ready ticket behind the lower-ranked %q", served.Row.ID, b1.ID, c1.ID)
	}
	if served.Row.State() != model.StateOpen {
		t.Fatalf("served row state = %v, want open — this pick must rest on the lane being unheld alone, never on an orphan", served.Row.State())
	}
}

// The same admission at the epic-continuation step: a sibling lane of the
// checkout's own epic whose claim has expired is nobody's, so step 2 continues
// into it rather than declaring the epic exhausted.
func TestRouteNextContinuesEpicIntoAnExpiredForeignLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): expired,
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane (an expired sibling lane of our own epic is nobody's)", outcome, outcome)
	}
	if served.Row.ID != a2.ID || served.Lane.Epic() != epicA.ID {
		t.Fatalf("served = %q in epic %q, want %q in %q", served.Row.ID, served.Lane.Epic(), a2.ID, epicA.ID)
	}
	if served.Row.State() != model.StateOpen {
		t.Fatalf("served row state = %v, want open — the pick must rest on the lane being unheld alone, never on an orphan", served.Row.State())
	}
}

// startAdvice varies on two axes, and this pins every cell of the product.
//
// The lead clause is routing's admission made visible. An in-progress row
// reaches a lane this checkout does not hold only when nobody holds that lane,
// so a bare "claim it" would promise greenfield on a ticket that may carry
// another checkout's unmerged working tree; the clause says the row is in
// flight and unheld, and nothing about who left it — an expired claim is not a
// claim, and the wording carries no provenance.
//
// The object is the lane, and rendering LaneID's three shapes through String()
// would misinform the reader: a solo lane spells the ticket's own id, so the
// line reads "starting X claims X" and no reader can take a tautology as advice
// about a command they have yet to run; an epic's default lane, whose key is
// empty, trails a bare "#" that reads as an unfilled template slot. Only the
// named lane carries information, and it is the rarest of the three.
//
// Whatever else changes here, no cell may contain "#" or say the ticket's id
// where a lane belongs, and a table is the only way to see all three shapes
// fail at once.
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

	rows, details, _ := h.gather()

	for _, tc := range []struct{ name, id, want string }{
		{"a named lane is the one shape worth naming", named.ID,
			"run `lit start " + named.ID + "` to claim lane a1 of epic " + epicA.ID},
		{"an epic's default lane is words, not a trailing #", unnamed.ID,
			"run `lit start " + unnamed.ID + "` to claim the default lane of epic " + epicA.ID},
		{"a solo lane is the ticket, so the lane goes unnamed", solo.ID,
			"run `lit start " + solo.ID + "` to claim it"},
		{"an in-flight row says so before the same advice", inFlight.ID,
			inFlight.ID + " is in progress and nobody holds it — run `lit start " + inFlight.ID + "` to claim lane a3 of epic " + epicA.ID},
		{"a default lane is still words on an in-flight row", unnamedInFlight.ID,
			unnamedInFlight.ID + " is in progress and nobody holds it — run `lit start " + unnamedInFlight.ID + "` to claim the default lane of epic " + epicA.ID},
		{"a solo lane still goes unnamed on an in-flight row", soloInFlight.ID,
			soloInFlight.ID + " is in progress and nobody holds it — run `lit start " + soloInFlight.ID + "` to claim it"},
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

	rows, details, _ := h.gather()
	row := rowByID(t, rows, solo.ID)
	got := startAdvice(row, laneOf(t, details, row))

	if n := strings.Count(got, solo.ID); n != 1 {
		t.Fatalf("startAdvice = %q names %q %d times, want exactly 1 — a solo lane is its ticket, so naming the lane repeats the id and reads as a log line rather than as advice", got, solo.ID, n)
	}
}

// Step 1 competes two capacities in one pick, and the comment above `pick`
// says why ranking them against each other is wrong. Run in both rank orders:
// a preference for either capacity fails one arm.
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

			rows, details, epics := h.gather()
			standings := claims.Standings{
				laneOf(t, details, rowByID(t, rows, inFlight.ID)): heldBy(selfAttribution),
				laneOf(t, details, rowByID(t, rows, ready.ID)):    heldBy(selfAttribution),
			}

			outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
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
// its admission of abandoned work is a path of its own: a dependency gating our
// lane, in flight in a lane nobody holds, is offered here even though the
// global-pool step never sees it.
func TestRouteNextServesAnAbandonedOnPathDependency(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "External blocker", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(a2.ID, dep.ID)
	h.transition(dep.ID, model.Start{Assignee: "other"})

	rows, details, epics := h.gather()
	orphan(t, rows, dep.ID)
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):  heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): expired,
	}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency (the abandoned on-path dependency is servable)", outcome, outcome)
	}
	if served.Gates != a2.ID {
		t.Fatalf("served.Gates = %q, want %q — the qualifier must survive the in-flight path naming the row it unblocks, and a merely non-empty id would let the dependency name itself", served.Gates, a2.ID)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the dependency gating our own blocked lane)", served.Row.ID, dep.ID)
	}
	if served.Row.State() != model.StateInProgress {
		t.Fatalf("served row state = %v, want in_progress — the point of this path is that step 1b admits abandoned in-flight work", served.Row.State())
	}
}

// The two clocks. Orphaning reads the row's last write by anyone; the lane's
// freshness reads its holder's last event. A peer's field write on an in-flight
// row keeps it un-orphaned while the holder's claim expires underneath it. Were
// routing to wait on the orphan clock, the lane would be nobody's to resume and
// the row not yet orphaned, and the ticket would vanish from `next` — served to
// nobody, named by no diagnostic. A lane nobody holds is itself the proof the
// row is abandoned, so the row is served on that fact alone; the orphan clock
// does not enter routing at all (capacityFor).
func TestRouteNextServesUnorphanedInFlightRowInAnUnheldLane(t *testing.T) {
	for _, tc := range []struct {
		name string
	}{
		{"an in-flight row in a lane nobody holds, whoever started it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
			a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})
			h.transition(a1.ID, model.Start{Assignee: "tester"})

			epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
			b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

			rows, details, epics := h.gather()
			row := rowByID(t, rows, a1.ID)
			if ClassifyReadiness(row.Annotations).IsOrphaned() {
				t.Fatalf("fixture %q is orphaned; this test's premise is an in-flight row the orphan clock has NOT reached", a1.ID)
			}
			standings := claims.Standings{laneOf(t, details, row): expired}

			outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
			served, ok := outcome.(ServedFromNewLane)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane serving %q — never %q with %q served to nobody", outcome, outcome, a1.ID, b1.ID, a1.ID)
			}
			if served.Row.ID != a1.ID {
				t.Fatalf("served = %q, want %q (in flight in a lane nobody holds, and ranked first)", served.Row.ID, a1.ID)
			}
		})
	}
}

// This checkout's own lane holds the top-ranked open row, and the claim on it
// has expired. The row is still the pick — it ranks first — but it is served
// from the global pool by rank, as it would be for any checkout, not from step
// 1 as work this checkout holds. The premise check on rank order is what
// separates this from TestRouteNextExpiredOwnLaneDoesNotOutrankTheBacklog: same
// expired own lane, opposite rank, and the pick follows the rank both times.
func TestRouteNextServesTopRankedRowInExpiredOwnLaneByRank(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	if rows[0].ID != a1.ID {
		t.Fatalf("fixture rank order = %q first, want %q — this test's premise is that the expired lane ranks top", rows[0].ID, a1.ID)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane — an expired claim is served by rank, never as a lane we hold (%s ranks below)", outcome, outcome, b1.ID)
	}
	if served.Row.ID != a1.ID {
		t.Fatalf("served = %q, want %q (it ranks first)", served.Row.ID, a1.ID)
	}
}

// TestRouteNextDoesNotAdoptExpiredPublicHistory is the routing half of the ruling
// that a bucket identity is a holder but never a proof of identity.
//
// The setup is the normal state of a freshly upgraded repository read by a
// brand-new checkout: pre-attribution history is far older than the window, so
// its lanes derive Unclaimed, and the checkout asking is itself unminted. An
// unminted self and an unclaimed lane meet in the pool, by rank.
//
// What must NOT come back is ResumedOwnWork; that is the load-bearing half.
func TestRouteNextDoesNotAdoptExpiredPublicHistory(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "whoever-came-before"})

	rows, details, epics := h.gather()
	orphan(t, rows, a1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, publicAttribution, focusScope{})
	if resumed, adopted := outcome.(ResumedOwnWork); adopted {
		t.Fatalf("routeNext resumed %q as this checkout's own work; an unminted self shares the public bucket with the history, which proves both unaddressable, not both us", resumed.Row.ID)
	}
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane: the expired public lane is nobody's, and its orphan is served by rank", outcome, outcome)
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

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(publicAttribution)}

	outcome := routeNext(rows, details, epics, standings, publicAttribution, focusScope{})
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
// Words and speakability are asserted as one biconditional because the two ways
// they can disagree are both defects and only one of them is obvious. A
// speakable kind with no words renders ids under an empty parenthetical. Words
// for a kind the walk cannot stamp are a promise about a different walk:
// exhaustedNotes carrying reachOffFocusPath would tell a reader that an epic's
// own gating blocker wants `lit next --all`, when steps 1-3 never scope and so
// never need it.
//
// The union is asserted separately: a sixth kind cannot enter the enum until
// some walk claims it.
func TestEveryReachKindHasWordsInBothDiagnostics(t *testing.T) {
	t.Parallel()
	// reachOf's switch is total over its kinds. The exhaustion walk reaches
	// laneOurs rows and, via gatingDependencies, deps the gather never
	// returned — but never a takeable one: step 2b serves the first takeable
	// dependency over the same scope before exhaustion is reached.
	//
	// The pool walk stamps no takeable row either: NoWork is answered only with
	// no lane held, so nothing there is laneOurs and the pick has already
	// declined every takeable row, leaving routeAround as the only verdict
	// passedOver sees; and it asks reachOf about gathered rows only. So
	// reachOutOfView is unreachable there, and words for it would describe an
	// answer that walk can never give.
	exhaustedSpeaks := []reachKind{reachHeldFresh, reachNotReady, reachAwaitingOutside, reachOutOfView}
	poolSpeaks := []reachKind{reachHeldFresh, reachNotReady, reachAwaitingOutside, reachOffFocusPath}

	// reachTakeable is the one kind routing acts on instead of reporting: steps
	// 1b and 2b serve it. It means something without a walk that says it.
	spoken := map[reachKind]bool{reachTakeable: true}
	for _, diagnostic := range []struct {
		name   string
		notes  reachNotes
		speaks []reachKind
	}{
		{"exhausted", exhaustedNotes, exhaustedSpeaks},
		{"pool", poolNotes, poolSpeaks},
	} {
		speakable := map[reachKind]bool{}
		for _, kind := range diagnostic.speaks {
			speakable[kind] = true
			spoken[kind] = true
		}
		for kind := reachKind(0); kind < reachKindCount; kind++ {
			if speakable[kind] != (diagnostic.notes[kind] != "") {
				t.Errorf("%s notes: reachKind %d is speakable=%v but its words are %q — a speakable kind with no words renders its ids under an empty parenthetical, and words for an unspeakable kind describe an answer this walk can never give",
					diagnostic.name, kind, speakable[kind], diagnostic.notes[kind])
			}
		}
	}
	for kind := reachKind(0); kind < reachKindCount; kind++ {
		if !spoken[kind] {
			t.Errorf("reachKind %d is stamped by no diagnostic — a new kind needs a walk that says it before it can mean anything", kind)
		}
	}
}

// "No ready work" alone would answer two opposite questions in identical
// words: an empty backlog, and a backlog full of work this checkout may not
// have.
//
// Two kinds are put in one pool on purpose. A single-kind fixture passes
// against a renderer that prints one note for everything it went past: the
// message has to say that one row is somebody's live work and the other is
// merely gated, because those call for different acts.
func TestNoWorkNamesEachRowThePoolWalkWentPast(t *testing.T) {
	h := newReadyTestHarness(t)
	held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs, in flight", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(held.ID, model.Start{Assignee: "tester"})
	// Gated by the row above, so the whole pool is unstartable without either
	// row being takeable — and for two different reasons.
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ours to want, not to start", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(gated.ID, held.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, held.ID)): heldBy(otherAttribution)}

	// This checkout holds nothing, so routing starts straight at the global pool.
	outcome := routeNext(rows, details, epics, standings, selfAttribution, focusScope{})
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
	if !strings.Contains(msg, "not startable right now") {
		t.Fatalf("NoWork.Error() = %q, want %q reported as gated rather than as somebody's live work", msg, gated.ID)
	}
}

// The other half of the same type: an empty backlog. The clause is driven
// entirely by the rows the walk went past, so no rows means no clause — pinned
// byte-for-byte, because "no ready work" is still the whole truth when there is
// nothing to say why about.
func TestNoWorkOnAGenuinelyEmptyBacklogIsUnchanged(t *testing.T) {
	h := newReadyTestHarness(t)

	rows, details, epics := h.gather()
	outcome := routeNext(rows, details, epics, claims.Standings{}, selfAttribution, focusScope{})
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
