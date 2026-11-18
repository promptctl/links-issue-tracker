package cli

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
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

func laneOf(t *testing.T, details map[model.ID]storage.IssueRelations, row annotation.AnnotatedIssue) model.LaneID {
	t.Helper()
	return model.LaneOf(row.Issue, details[row.ID].Parent)
}

func rowByID(t *testing.T, rows []annotation.AnnotatedIssue, id model.ID) annotation.AnnotatedIssue {
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
func orphan(t *testing.T, rows []annotation.AnnotatedIssue, id model.ID) {
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

func (h readyTestHarness) transition(id model.ID, action model.Action) {
	h.t.Helper()
	if _, err := h.ap.Store.Apply(h.ctx, id, storage.Change{Action: action, Actor: "tester"}); err != nil {
		h.t.Fatalf("Apply(%s, %T) error = %v", id, action, err)
	}
}

// evidenceIn is the claim evidence ap's store holds now, read as `next` reads
// it. Routing counts a stopped scope's progress from it; the standings a test
// routes under stay hand-built.
func evidenceIn(t testing.TB, ctx context.Context, ap *app.App) claims.Evidence {
	t.Helper()
	evidence, err := readEvidence(ctx, ap)
	if err != nil {
		t.Fatalf("readEvidence error = %v", err)
	}
	return evidence
}

func (h readyTestHarness) gather() ([]annotation.AnnotatedIssue, map[model.ID]storage.IssueRelations, map[model.ID]storage.IssueRelations) {
	h.t.Helper()
	gathered, err := gatherWorkableAnnotated(h.ctx, runEnv(h.t), h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	return gathered.rows, gathered.details, gathered.epics
}

// A checkout's own held lane wins over a higher-ranked, entirely unclaimed
// epic — routing step 1 outranks plain backlog order. Without claims, `next`
// would return B.1 (top of rank order); with the checkout's own claim on
// epic A, it must not.
func TestRouteNextServesOwnClaimOverHigherRankedUnclaimedLane(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	// A.2 sits in its own lane so the lane gate does not block it behind the
	// in_progress default-lane sibling A.1 — this test's contract is claim
	// precedence over rank, not lane-gate membership.
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic C"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("C.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution)}

	// This checkout holds no claims of its own, so routing starts straight at
	// the global pool, exactly as design-docs/work-claims.md specifies for the
	// zero state.
	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), model.Attribution{}, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})

	rows, details, epics := h.gather()
	// A.1 is closed, so it is not among the gathered rows — its lane is
	// addressed straight from the issue, which is the shape of the real case:
	// a claim outlives the ticket that established it.
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane", outcome, outcome)
	}
	if served.Row.ID != a2.ID {
		t.Fatalf("served = %q, want %q (epic A's other lane before epic B)", served.Row.ID, a2.ID)
	}
}

// A checkout's own epic having no reachable work — its held lane holds
// nothing, and the epic has no other lane to offer — does not trap the
// checkout there. `next` serves the top ready ticket outside the epic, carrying
// the exhaustion so the invocation event records why the epic stopped beside
// the pick that left it.
//
// This outcome is reachable only while the claim is live.
func TestRouteNextServesPastAnExhaustedEpic(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedPastExhaustion)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedPastExhaustion serving epic B's B.1", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q", served.Row.ID, b1.ID)
	}
	if len(served.Exhaustion.Epics) != 1 || served.Exhaustion.Epics[0] != epicA.ID {
		t.Fatalf("exhaustion.Epics = %v, want [%q]", served.Exhaustion.Epics, epicA.ID)
	}
	// Nothing blocks epic A, so there is nothing a new ticket could clear:
	// moving on is the only route.
	if len(served.Exhaustion.Blocked) != 0 {
		t.Fatalf("exhaustion.Blocked = %v, want none (epic A has nothing queued)", served.Exhaustion.Blocked)
	}
}

// With nothing ready outside the exhausted epic either, exhaustion is the
// answer. Nothing blocks epic A here — its lane is merely done — so no route
// stays: a new ticket would have nothing to clear.
func TestRouteNextExhaustionIsTerminalWhenNothingElseIsReady(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — B.1's lane is held elsewhere, so nothing outside epic A is ready", outcome, outcome)
	}
	if !slices.Equal(exhausted.Epics, []model.ID{epicA.ID}) {
		t.Fatalf("exhausted.Epics = %v, want [%q]", exhausted.Epics, epicA.ID)
	}
	if want := (claims.Progress{Done: 1, Total: 1}); exhausted.Progress != want {
		t.Fatalf("exhausted.Progress = %+v, want %+v — epic A's one ticket is done", exhausted.Progress, want)
	}
	if len(exhausted.Blocked) != 0 || len(exhausted.Held) != 0 {
		t.Fatalf("exhausted.Blocked = %+v, Held = %+v, want neither — nothing in epic A is left to do", exhausted.Blocked, exhausted.Held)
	}
	nothingOutside(t, exhausted)
}

// nothingOutside fails unless the exhausted scope reports nothing outside it:
// no row a filter hid, and none a focus label withheld.
func nothingOutside(t *testing.T, exhausted Exhausted) {
	t.Helper()
	if exhausted.Narrowing != nil || len(exhausted.OffPath) != 0 {
		t.Fatalf("exhausted.Narrowing = %+v, OffPath = %v, want neither — nothing outside the scope is ready, hidden or withheld", exhausted.Narrowing, exhausted.OffPath)
	}
}

// A blocked epic with nothing ready outside it reports its blocker as one a
// ticket filed here could clear. The one route left is to stay: file that
// ticket under the epic and make the blocker wait on it, since filing alone
// leaves the blocked work waiting on what it waited on.
func TestRouteNextTerminalExhaustionNamesTheStayRouteAgainstABlock(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside, theirs"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependency(a2.ID, blocker.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, blocker.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row outside epic A is another checkout's", outcome, outcome)
	}
	if !slices.Equal(exhausted.Epics, []model.ID{epicA.ID}) {
		t.Fatalf("exhausted.Epics = %v, want [%q] — the epic the clearing ticket is filed under", exhausted.Epics, epicA.ID)
	}
	if want := (claims.Progress{Done: 1, Total: 2}); exhausted.Progress != want {
		t.Fatalf("exhausted.Progress = %+v, want %+v", exhausted.Progress, want)
	}
	if len(exhausted.Blocked) != 1 || exhausted.Blocked[0].ID != blocker.ID || readingOf(t, exhausted.Blocked[0]).Kind != reachHeldFresh || !exhausted.Blocked[0].Clearable {
		t.Fatalf("exhausted.Blocked = %+v, want %s alone, held by another checkout and clearable", exhausted.Blocked, blocker.ID)
	}
	nothingOutside(t, exhausted)

	// Follow the route to stay. The edge goes through the CLI's own policy
	// boundary, which refuses a blocked-by edge inside one epic, and the next
	// route serves the new ticket from the lane this checkout holds.
	unblocker := followStayRoute(t, h, []string{"--parent", epicA.ID.Raw(), "--top"}, blocker.ID)
	rows, details, epics = h.gather()
	outcome = routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	if served, ok := outcome.(ServedFromClaim); !ok || served.Row.ID != unblocker {
		t.Fatalf("after following the stay route, routeNext = %#v (%T), want ServedFromClaim serving %s", outcome, outcome, unblocker)
	}
}

// followStayRoute clears a blocker reported clearable, in the two commands
// that takes — `lit new` with the given flags, then `lit dep add` making the
// blocker wait on the new ticket — through the CLI's own handlers, so a
// blocker reported clearable against a placement or an edge lit refuses or
// mis-ranks fails here. It returns the new ticket's id.
func followStayRoute(t *testing.T, h readyTestHarness, newFlags []string, blockerID model.ID) model.ID {
	t.Helper()
	var out strings.Builder
	args := append([]string{"--title", "Clears the blocker", "--topic", "next", "--type", "task"}, newFlags...)
	if err := runNew(t, h.ctx, &out, h.ap, args); err != nil {
		t.Fatalf("lit new %v, filing the ticket that clears %s: %v", newFlags, blockerID, err)
	}
	newID := strings.Fields(out.String())[0]
	if err := runDepAdd(t, h.ctx, io.Discard, h.ap, []string{blockerID.Raw(), "--blocked-by", newID}); err != nil {
		t.Fatalf("lit dep add %s --blocked-by %s, clearing a blocker reported clearable: %v", blockerID, newID, err)
	}
	return model.ID(newID)
}

// When the block is declared on the epic itself, every child inherits it — a
// ticket filed under the epic starts out held back too. The blocker is still
// reported clearable from under the epic, because the edge added next is what
// frees the new child: an epic's blocker never holds back a child it waits on.
// Both halves are observed, the new child held back before the edge and served
// after it.
func TestRouteNextStayRouteWorksWhenTheEpicItselfIsBlocked(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Blocks the whole epic, theirs"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependency(epicA.ID, blocker.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, blocker.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row outside epic A is another checkout's", outcome, outcome)
	}
	if !slices.Equal(exhausted.Epics, []model.ID{epicA.ID}) {
		t.Fatalf("exhausted.Epics = %v, want [%q] — the epic the clearing ticket is filed under", exhausted.Epics, epicA.ID)
	}
	if !slices.ContainsFunc(exhausted.Blocked, func(b blockerReach) bool { return b.ID == blocker.ID && b.Clearable }) {
		t.Fatalf("exhausted.Blocked = %+v, want %s in it, clearable — a ticket under the blocked epic can still free it", exhausted.Blocked, blocker.ID)
	}

	var out strings.Builder
	if err := runNew(t, h.ctx, &out, h.ap, []string{"--title", "Clears the blocker", "--topic", "next", "--type", "task", "--parent", epicA.ID.Raw(), "--top"}); err != nil {
		t.Fatalf("lit new --parent %s --top: %v", epicA.ID, err)
	}
	unblocker := model.ID(strings.Fields(out.String())[0])
	rows, _, _ = h.gather()
	if ClassifyReadiness(rowByID(t, rows, unblocker).Annotations).IsReady() {
		t.Fatalf("%s under blocked epic %s is ready before the edge, want it held back by %s — else the edge below proves nothing", unblocker, epicA.ID, blocker.ID)
	}

	if err := runDepAdd(t, h.ctx, io.Discard, h.ap, []string{blocker.ID.Raw(), "--blocked-by", unblocker.Raw()}); err != nil {
		t.Fatalf("lit dep add %s --blocked-by %s: %v", blocker.ID, unblocker, err)
	}
	rows, details, epics = h.gather()
	outcome = routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	if served, ok := outcome.(ServedFromClaim); !ok || served.Row.ID != unblocker {
		t.Fatalf("after following the stay route, routeNext = %#v (%T), want ServedFromClaim serving %s", outcome, outcome, unblocker)
	}
}

// A dependency inside the epic is never the blocker exhaustion names. lit
// refuses a blocked-by edge between two tickets of one epic, yet one can exist — an
// import writes it, or it predates the rule — and reported as a blocker to
// clear it would ask for a `lit dep add` lit then refuses. Here the sibling is
// another checkout's work, so the epic waits on work underway and reports no
// blocker at all.
func TestRouteNextExhaustionNeverNamesASiblingAsTheBlocker(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2, theirs"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.3"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a3")})
	h.addDependencyWithinEpic(a3.ID, a2.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — A.2 is another checkout's and A.3 waits on it", outcome, outcome)
	}
	if want := (claims.Progress{Done: 1, Total: 3}); exhausted.Progress != want {
		t.Fatalf("exhausted.Progress = %+v, want %+v", exhausted.Progress, want)
	}
	if len(exhausted.Blocked) != 0 || len(exhausted.Held) != 0 {
		t.Fatalf("exhausted.Blocked = %+v, Held = %+v, want neither — sibling %s is not a blocker to clear from outside, and the epic waits only on work underway", exhausted.Blocked, exhausted.Held, a2.ID)
	}
	nothingOutside(t, exhausted)
}

// A sibling that is itself blocked is not named either: it is an open row of
// the epic, so the walk reaches past it to what holds it back, and reports
// that as the blocker to clear — an edge lit accepts, added here.
func TestRouteNextExhaustionNamesTheOutsideBlockerBehindASibling(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.3"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a3")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside, theirs"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependencyWithinEpic(a3.ID, a2.ID)
	h.addDependency(a2.ID, blocker.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, blocker.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row outside epic A is another checkout's", outcome, outcome)
	}
	if !slices.Equal(exhausted.Epics, []model.ID{epicA.ID}) {
		t.Fatalf("exhausted.Epics = %v, want [%q] — the epic the clearing ticket is filed under", exhausted.Epics, epicA.ID)
	}
	if want := (claims.Progress{Done: 1, Total: 3}); exhausted.Progress != want {
		t.Fatalf("exhausted.Progress = %+v, want %+v", exhausted.Progress, want)
	}
	if len(exhausted.Blocked) != 1 || exhausted.Blocked[0].ID != blocker.ID || readingOf(t, exhausted.Blocked[0]).Kind != reachHeldFresh || !exhausted.Blocked[0].Clearable {
		t.Fatalf("exhausted.Blocked = %+v, want %s alone, held by another checkout and clearable — not sibling %s", exhausted.Blocked, blocker.ID, a2.ID)
	}
	nothingOutside(t, exhausted)
	followStayRoute(t, h, []string{"--parent", epicA.ID.Raw(), "--top"}, blocker.ID)
}

// This checkout finished its own lane of epic A while another checkout still
// holds the epic's open lane, so the epic stops for it with work left — but
// none of that work is this checkout's. Routing counts the two apart, so a
// reader can tell the pick past the scope is new work.
func TestRouteNextCountsTheCheckoutsOwnLanesApartFromItsEpic(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2, theirs"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(otherAttribution),
	}
	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedPastExhaustion)
	if !ok || served.Row.ID != b1.ID {
		t.Fatalf("routeNext = %#v (%T), want ServedPastExhaustion serving %s", outcome, outcome, b1.ID)
	}
	if got, want := served.Exhaustion.Progress, (claims.Progress{Done: 1, Total: 2}); got != want {
		t.Fatalf("Progress = %+v, want %+v — the epic's every lane", got, want)
	}
	if got, want := served.Exhaustion.OwnProgress, (claims.Progress{Done: 1, Total: 1}); got != want {
		t.Fatalf("OwnProgress = %+v, want %+v — only the lane this checkout holds", got, want)
	}
}

// Under a focus label the pool is the focus path, so an empty pool is not
// "nothing ready outside the epic": ready rows off the path were never asked
// about. They are named in OffPath — and our own epic's rows are not among
// them, since steps 1-2b walk those whether focus is on or not.
func TestRouteNextExhaustionNamesReadyRowsTheFocusWithheld(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	offPath := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready, off the focus path"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(otherAttribution),
	}
	// No gathered row carries the FocusPath annotation, so an active scope
	// withholds every one of them from the pool.
	focused := focusScope{goals: []model.ID{"test-goal"}}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focused, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the focus path holds nothing ready", outcome, outcome)
	}
	if len(exhausted.OffPath) != 1 || exhausted.OffPath[0] != offPath.ID {
		t.Fatalf("exhausted.OffPath = %+v, want exactly [%s] — %s is epic A's own, walked by steps 1-2b", exhausted.OffPath, offPath.ID, a2.ID)
	}
	if exhausted.Narrowing != nil {
		t.Fatalf("exhausted.Narrowing = %+v, want none — the focus withheld %s, no filter hid it", exhausted.Narrowing, offPath.ID)
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})

	rows, details, epics := h.gather()
	if rows[0].ID != b1.ID {
		t.Fatalf("fixture rank order = %q first, want %q — this test's premise is that the expired lane ranks below the pool's top", rows[0].ID, b1.ID)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a2.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane — an expired claim of our own holds nothing", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q (the backlog's top, never %q from the lane we let expire)", served.Row.ID, b1.ID, a2.ID)
	}
}

// An out-of-lane dependency that gates the claimed lane's blocked ticket is
// offered as on-path — design-docs/work-claims.md, Routing step 1. It comes
// back as ServedFromDependency and not ServedFromClaim: the dependency is by
// definition OUTSIDE the claimed lane, so starting it claims a second lane, and
// ServedFromClaim's contract is that nothing is claimed. It is not
// ServedFromNewLane either: that type is the global pool's, and sharing it
// would leave this pick indistinguishable from the pool's.
func TestRouteNextOffersOnPathDependencyAsANewLane(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("External blocker"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependency(a2.ID, dep.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
}

// One dependency can gate several of our own blocked rows, and only one id fits
// in Gates. Which one is therefore part of the contract, not an
// accident of the walk: the queue-first gated row. The expectation here is read
// OUT OF the gathered queue rather than written in as an id, so the test pins
// the rule and cannot be satisfied by a fixture that happens to order the two
// rows the way the assertion guessed.
func TestOnPathDependencyNamesTheQueueFirstRowItGates(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.3"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a3")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("External blocker"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependency(a2.ID, dep.ID)
	h.addDependency(a3.ID, dep.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a3.ID)): heldBy(selfAttribution),
	}

	// The rule's own subject, rendered from the queue: the first of the two
	// gated rows in gather order.
	var queueFirst model.ID
	for _, row := range rows {
		if row.ID == a2.ID || row.ID == a3.ID {
			queueFirst = row.ID
			break
		}
	}
	if queueFirst == "" {
		t.Fatalf("neither %s nor %s is in the gathered queue; the fixture does not exercise the rule", a2.ID, a3.ID)
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency (one dependency gating two of our rows)", outcome, outcome)
	}
	if served.Row.ID != dep.ID {
		t.Fatalf("served = %q, want %q (the shared external dependency)", served.Row.ID, dep.ID)
	}
	if served.Gates != queueFirst {
		t.Fatalf("served.Gates = %q, want %q — a dependency gating several of our rows names the queue-first one, so the outcome is deterministic across runs", served.Gates, queueFirst)
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "tester"})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	orphan(t, rows, a1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	h.transition(b1.ID, model.Start{Assignee: "other"})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic C"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("C.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	orphan(t, rows, b1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	h.transition(b1.ID, model.Start{Assignee: "other"})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic C"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("C.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): heldBy(otherAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane", outcome, outcome)
	}
	if served.Row.ID != c1.ID {
		t.Fatalf("served = %q, want %q (B.1 is in flight in a lane held fresh — leave it)", served.Row.ID, c1.ID)
	}
}

// onlyBugs is the filter `lit next --type bug` hands routing.
func onlyBugs(t *testing.T) storage.IssueCriteria {
	t.Helper()
	asked, err := workableFilter{IssueTypes: []model.IssueType{model.TypeBug}}.criteria()
	if err != nil {
		t.Fatalf("criteria error = %v", err)
	}
	return asked
}

// The checkout holds a fresh claim on a lane whose only ticket is a task, and
// asks for bugs. Its epic is not exhausted — the filter left its next ticket
// out — so the bug elsewhere is served beneath the ticket it hid, and nothing
// calls the epic out of work.
func TestRouteNextNamesTheTicketAFilterHidFromTheHeldScope(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	served, ok := outcome.(ServedPastNarrowing)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedPastNarrowing (epic B's bug, beneath the task the filter hid)", outcome, outcome)
	}
	if served.Row.ID != b1.ID || served.Narrowing.Hidden.ID != a1.ID {
		t.Fatalf("served %q past hidden %q, want %q past %q", served.Row.ID, served.Narrowing.Hidden.ID, b1.ID, a1.ID)
	}
}

// With nothing the filter selects ready anywhere, the answer is the hidden
// ticket alone — still not an exhausted epic.
func TestRouteNextNarrowedWhenNothingTheFilterSelectsIsReady(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	narrowed, ok := outcome.(Narrowed)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Narrowed naming %q", outcome, outcome, a1.ID)
	}
	if narrowed.Hidden.ID != a1.ID {
		t.Fatalf("hidden = %q, want %q", narrowed.Hidden.ID, a1.ID)
	}
}

// A scope whose work is genuinely stuck is exhausted under a filter too, and
// its blocker is classified from the whole queue: the task blocking it is held
// by another checkout, which the filter does not make "outside this view".
func TestRouteNextStillReportsAnExhaustedScopeUnderAFilter(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	epicD := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic D"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Their task"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicD.ID})
	h.addDependency(a1.ID, dep.ID)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	served, ok := outcome.(ServedPastExhaustion)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedPastExhaustion — nothing in epic A is reachable, filter or not", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q", served.Row.ID, b1.ID)
	}
	if len(served.Exhaustion.Blocked) != 1 || served.Exhaustion.Blocked[0].ID != dep.ID || readingOf(t, served.Exhaustion.Blocked[0]).Kind != reachHeldFresh {
		t.Fatalf("blocked = %#v, want %q held fresh", served.Exhaustion.Blocked, dep.ID)
	}
}

// A scope that really is stuck, under a filter that also hides the only ready
// ticket outside it: the epic is exhausted, and what lies outside is the task
// the filter left out — not "nothing ready outside it". The control drops that
// task, and the same run then truly has nothing ready outside the epic.
func TestRouteNextExhaustedNamesThePoolTicketAFilterHid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		readyTask  bool
		wantHidden bool
	}{
		{name: "the filter hid the pool's ready task", readyTask: true, wantHidden: true},
		{name: "nothing ready outside the epic at all", readyTask: false, wantHidden: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
			epicD := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic D"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Their task"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicD.ID})
			h.addDependency(a1.ID, dep.ID)
			var b1 model.Issue
			if tc.readyTask {
				epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
				b1 = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})
			}

			rows, details, epics := h.gather()
			standings := claims.Standings{
				model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
				laneOf(t, details, rowByID(t, rows, dep.ID)): heldBy(otherAttribution),
			}

			outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
			exhausted, ok := outcome.(Exhausted)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want Exhausted — nothing the filter selects is ready anywhere", outcome, outcome)
			}
			if !tc.wantHidden {
				nothingOutside(t, exhausted)
				return
			}
			if exhausted.Narrowing == nil || exhausted.Narrowing.Hidden.ID != b1.ID {
				t.Fatalf("narrowing = %#v, want %q hidden by the filter", exhausted.Narrowing, b1.ID)
			}
		})
	}
}

// The diagnostics answer the run's question, so under a filter they name only
// rows it selects. Off the focus path lie a ready bug and a ready task; asked
// for bugs, the exhausted epic names the bug and not the task.
func TestRouteNextExhaustionUnderAFilterNamesOnlyWithheldRowsItSelects(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	offBug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Bug off the path"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("bug")})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Task off the path"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("task")})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): heldBy(otherAttribution),
	}
	focused := focusScope{goals: []model.ID{"test-goal"}}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focused, onlyBugs(t))
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the focus path holds nothing ready", outcome, outcome)
	}
	if len(exhausted.OffPath) != 1 || exhausted.OffPath[0] != offBug.ID {
		t.Fatalf("exhausted.OffPath = %+v, want exactly [%s] — the task off the path is not what this run asked for", exhausted.OffPath, offBug.ID)
	}
}

// With nothing ready anywhere, NoWork under a filter names the rows it walked
// past that the filter selects: the bug another checkout holds, not the task.
func TestRouteNextNoWorkUnderAFilterNamesOnlyRowsItSelects(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Their bug"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("bug")})
	task := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Their task"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("task")})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, bug.ID)):  heldBy(otherAttribution),
		laneOf(t, details, rowByID(t, rows, task.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	noWork, ok := outcome.(NoWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want NoWork — nothing is ready, filter or not", outcome, outcome)
	}
	if len(noWork.Unreachable) != 1 || noWork.Unreachable[0].ID != bug.ID {
		t.Fatalf("unreachable = %+v, want exactly [%s] — the task is not what this run asked for", noWork.Unreachable, bug.ID)
	}
}

// A checkout holding no lane, asking for bugs in a backlog whose one ready
// ticket is a task: the filter emptied the pool, not the backlog, so the answer
// is Narrowed naming the task. The control adds a ready bug, which is served.
func TestRouteNextNarrowedForACheckoutHoldingNoLane(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	task := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Task"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("task")})

	rows, details, epics := h.gather()
	outcome := routeNext(rows, details, epics, claims.Standings{}, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	narrowed, ok := outcome.(Narrowed)
	if !ok || narrowed.Hidden.ID != task.ID {
		t.Fatalf("routeNext = %#v (%T), want Narrowed naming %q", outcome, outcome, task.ID)
	}

	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Bug"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("bug")})
	rows, details, epics = h.gather()
	outcome = routeNext(rows, details, epics, claims.Standings{}, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	if served, ok := outcome.(ServedFromNewLane); !ok || served.Row.ID != bug.ID {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane serving %q", outcome, outcome, bug.ID)
	}
}

// The filter narrows what is served, never what the walks read: a bug of ours
// waits on a task, which waits on a ready bug. Under --type bug the task is
// not servable, but the walk still goes through it to the bug it waits on.
func TestRouteNextFindsADependencyThroughATicketTheFilterHides(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicA.ID})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	between := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("The task between"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	epicY := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic Y"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("The bug upstream"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicY.ID})
	h.addDependency(a1.ID, between.ID)
	h.addDependency(between.ID, upstream.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency (%q, through %q)", outcome, outcome, upstream.ID, between.ID)
	}
	if served.Row.ID != upstream.ID || served.Gates != a1.ID || len(served.Via) != 1 || served.Via[0].Prerequisite != between.ID {
		t.Fatalf("served %q gating %q via %v, want %q gating %q via %q", served.Row.ID, served.Gates, served.Via, upstream.ID, a1.ID, between.ID)
	}
}

// An epic blocker is cleared by any ticket under it, so when the filter leaves
// out the first of them, the next one it selects is still work toward the
// blocker and is served as such.
func TestRouteNextServesTheTicketUnderAnEpicBlockerThatTheFilterSelects(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicA.ID})
	epicE := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic E"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("E's task"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicE.ID, Lane: storage.MustParseLane("task")})
	eBug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("E's bug"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicE.ID, Lane: storage.MustParseLane("bug")})
	h.addDependency(a1.ID, epicE.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, onlyBugs(t))
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency serving %q", outcome, outcome, eBug.ID)
	}
	if served.Row.ID != eBug.ID || served.Blocker != epicE.ID {
		t.Fatalf("served %q under blocker %q, want %q under %q", served.Row.ID, served.Blocker, eBug.ID, epicE.ID)
	}
}

// A gating dependency sitting in a lane another checkout holds fresh is one
// `lit start` refuses, so offering it would have `next` recommend what `start`
// blocks. It is routed around like any other fresh foreign hold, and
// exhaustion still names it rather than going quiet about why there is nothing
// to do.
// [LAW:no-silent-failure]
func TestRouteNextRoutesAroundOnPathDependencyHeldFresh(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("External blocker"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependency(a2.ID, dep.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):  heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
		if readingOf(t, blocked).Kind != reachHeldFresh {
			t.Fatalf("blocked dependency %q classified %v, want reachHeldFresh — another checkout holds its lane fresh", dep.ID, readingOf(t, blocked).Kind)
		}
	}
	if !named {
		t.Fatalf("exhausted.Blocked = %v, want it to name %q — routing around the dependency must not hide it", exhausted.Blocked, dep.ID)
	}
}

// Whether a hidden ticket is inside the scope does not depend on which chain
// reaches it first. Here our higher-ranked row's chain reaches it through an
// outside blocker's dependency edge, which says nothing of its lane, before our
// other row's lane says it is ours: it is still never named as a blocker, and
// the outside blocker, which clears when our own ticket does, is not one a
// ticket filed here could clear.
func TestExhaustionPlacesAHiddenLaneMateWhicheverChainReachesItFirst(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ranked first"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("x")})
	hidden := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Earlier in lane l"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("l")})
	later := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Later in lane l"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("l")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside blocker"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicX.ID})
	h.addDependency(first.ID, outside.ID)
	h.addDependency(outside.ID, hidden.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, first.ID)): heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, later.ID)): heldBy(selfAttribution),
	}
	var inView []annotation.AnnotatedIssue
	for _, row := range rows {
		if row.ID != hidden.ID {
			inView = append(inView, row)
		}
	}

	exhausted, ok := routeNext(inView, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{}).(Exhausted)
	if !ok {
		t.Fatal("routeNext did not answer Exhausted — every row in view waits on other work")
	}
	if len(exhausted.Blocked) != 1 || exhausted.Blocked[0].ID != outside.ID || readingOf(t, exhausted.Blocked[0]).Kind != reachNotReady {
		t.Fatalf("exhausted.Blocked = %+v, want only %s, not ready — %s sits in our lane l", exhausted.Blocked, outside.ID, hidden.ID)
	}
	if exhausted.Blocked[0].Clearable {
		t.Fatalf("exhausted.Blocked[0] = %+v, want it not clearable — %s clears when our own %s does", exhausted.Blocked[0], outside.ID, hidden.ID)
	}
}

// An epic blocking us is reported by the ticket under it that something here
// can move. One of its tickets is held by a label on itself, another waits on
// an outside event — through an external ticket it depends on, or by carrying
// the external label itself: the epic reads as held by itself, through the
// first. It closes only when both do, and the second waits on that event
// whatever is filed here, so the epic is not a blocker a ticket could clear.
func TestExhaustionReportsAnEpicBlockerByItsTicketHeldByItself(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// waitOutside makes E.1 wait on an outside event.
		waitOutside func(h readyTestHarness, waits model.Issue)
	}{
		{name: "through an external ticket", waitOutside: func(h readyTestHarness, waits model.Issue) {
			epicU := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic U"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream release"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicU.ID, Labels: []string{ExternalLabel}})
			h.addDependency(waits.ID, upstream.ID)
		}},
		{name: "by its own external label", waitOutside: func(h readyTestHarness, waits model.Issue) {
			h.setLabels(waits.ID, ExternalLabel)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			ours := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a")})
			epicE := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic E"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			waits := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("E.1 waits"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicE.ID, Lane: storage.MustParseLane("e1")})
			held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("E.2 needs design"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicE.ID, Lane: storage.MustParseLane("e2"), Labels: []string{NeedsDesignLabel}})
			h.addDependency(ours.ID, epicE.ID)
			tc.waitOutside(h, waits)

			rows, details, epics := h.gather()
			standings := claims.Standings{laneOf(t, details, rowByID(t, rows, ours.ID)): heldBy(selfAttribution)}

			exhausted, ok := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{}).(Exhausted)
			if !ok {
				t.Fatal("routeNext did not answer Exhausted — nothing on the path is takeable")
			}
			if got := readingOf(t, exhausted.Blocked[0]); got.ID != epicE.ID || got.Row.ID != held.ID || got.Kind != reachHeldByItself {
				t.Fatalf("exhausted.Blocked[0] = {%s via %s, %v}, want {%s via %s, held by itself}", got.ID, got.Row.ID, got.Kind, epicE.ID, held.ID)
			}
			if slices.ContainsFunc(exhausted.Blocked, func(b blockerReach) bool { return b.Clearable }) {
				t.Fatalf("exhausted.Blocked = %+v, want none clearable — %s closes only when %s does, which waits on an outside event", exhausted.Blocked, epicE.ID, waits.ID)
			}
		})
	}
}

// Blockers are named in the order the walk reached them, which is the rank of
// the rows they gate, whether or not the run could see them. Here the
// higher-ranked row waits on a blocker out of view and the lower-ranked one on
// a blocker another checkout holds: both are clearable, and the out-of-view
// one comes first.
func TestExhaustionNamesBlockersInWalkOrderWhetherOrNotInView(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	a3 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.3"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a3")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	gone := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Out of view"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("gone")})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Theirs"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("theirs")})

	// The out-of-view blocker gates whichever of ours the queue ranks first.
	rows, _, _ := h.gather()
	first, second := a2, a3
	if slices.IndexFunc(rows, func(r annotation.AnnotatedIssue) bool { return r.ID == a3.ID }) < slices.IndexFunc(rows, func(r annotation.AnnotatedIssue) bool { return r.ID == a2.ID }) {
		first, second = a3, a2
	}
	h.addDependency(first.ID, gone.ID)
	h.addDependency(second.ID, theirs.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, a2.ID)):     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a3.ID)):     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, theirs.ID)): heldBy(otherAttribution),
	}
	var inView []annotation.AnnotatedIssue
	for _, row := range rows {
		if row.ID != gone.ID {
			inView = append(inView, row)
		}
	}

	outcome := routeNext(inView, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — one blocker is out of view and the other is held elsewhere", outcome, outcome)
	}
	var named []model.ID
	for _, blocked := range exhausted.Blocked {
		named = append(named, blocked.ID)
	}
	if want := []model.ID{gone.ID, theirs.ID}; !slices.Equal(named, want) {
		t.Fatalf("exhausted.Blocked = %v, want %v — the walk reached %s first, through %s", named, want, gone.ID, first.ID)
	}
	if !exhausted.Blocked[0].Clearable || !exhausted.Blocked[1].Clearable {
		t.Fatalf("exhausted.Blocked = %+v, want both clearable — nothing seen holds %s, and %s waits on nothing", exhausted.Blocked, gone.ID, theirs.ID)
	}
}

// A row of ours waiting behind an earlier ticket of its own lane waits on our
// own work, even when a filter keeps that ticket out of view: its lane is the
// row's own, so it is inside the scope and never a blocker to clear from
// outside. Collected, it would ask for a ticket filed in our epic and a
// blocked-by edge lit refuses between two of its tickets.
func TestExhaustionNeverNamesAnEarlierLaneMateOutOfViewAsABlocker(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	earlier := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Earlier in our lane"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("ours")})
	later := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Later in our lane"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("ours")})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		laneOf(t, details, rowByID(t, rows, later.ID)): heldBy(selfAttribution),
	}
	// The narrowed gather `lit next --type bug` makes: the earlier ticket is
	// open and ready, and simply not in this run's rows.
	var inView []annotation.AnnotatedIssue
	for _, row := range rows {
		if row.ID != earlier.ID {
			inView = append(inView, row)
		}
	}

	outcome := routeNext(inView, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the only row in view waits on our own lane", outcome, outcome)
	}
	for _, blocked := range exhausted.Blocked {
		if blocked.ID == earlier.ID {
			t.Fatalf("exhausted.Blocked = %+v, want no %s — it is the earlier ticket of our own lane", exhausted.Blocked, earlier.ID)
		}
	}
	if slices.ContainsFunc(exhausted.Blocked, func(b blockerReach) bool { return b.Clearable }) {
		t.Fatalf("exhausted.Blocked = %+v, want none clearable — nothing outside the scope blocks it", exhausted.Blocked)
	}
}

// A blocker this run cannot see. gatherWorkableAnnotated narrows rows by
// --type/--labels/--assignee and to leaves, while the OpenDependency
// annotation comes from the store, so `lit next --type bug` can be gated by a
// task it never gathered. The diagnostic then knows the id and nothing else,
// and saying anything about its standing would be invention.
func TestExhaustionNamesABlockerOutsideThisViewAsSuch(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("External blocker"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
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

	outcome := routeNext(inView, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
		if blocked.Best != nil {
			t.Fatalf("blocked dependency %q read through %q as %v, want it out of view — it is absent from the gathered rows", dep.ID, blocked.Best.Row.ID, blocked.Best.Kind)
		}
	}
	if !named {
		t.Fatalf("exhausted.Blocked = %v, want it to name %q", exhausted.Blocked, dep.ID)
	}
}

// A blocker nobody holds, blocked by a further dependency of its own whose lane
// another checkout holds: a chain with nothing takeable anywhere along it. The
// walk descends to the further dependency and offers nothing, so the diagnostic
// is the whole answer, and it names each link by its own reach — the blocker as
// not startable, never as held, since its lane is unclaimed, and the further
// dependency as held. That end is held, not awaiting an outside event, so it
// is still a blocker a ticket filed here could clear.
func TestExhaustionNamesEachLinkOfAStuckChainByItsOwnReach(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("External blocker"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("dep")})
	deeper := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Blocker's blocker"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("deeper")})
	h.addDependency(a2.ID, dep.ID)
	h.addDependencyWithinEpic(dep.ID, deeper.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                        heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, deeper.ID)): heldBy(otherAttribution),
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted — the on-path dependency is itself blocked", outcome, outcome)
	}
	blocked := func(id model.ID) blockerReach {
		t.Helper()
		idx := slices.IndexFunc(exhausted.Blocked, func(b blockerReach) bool { return b.ID == id })
		if idx < 0 {
			t.Fatalf("exhausted.Blocked = %v, want it to name %q", exhausted.Blocked, id)
		}
		return exhausted.Blocked[idx]
	}
	if kind := readingOf(t, blocked(dep.ID)).Kind; kind != reachNotReady {
		t.Fatalf("blocked dependency %q classified %v, want reachNotReady — nobody holds its lane; it is blocked by %q", dep.ID, kind, deeper.ID)
	}
	end := blocked(deeper.ID)
	if kind := readingOf(t, end).Kind; kind != reachHeldFresh {
		t.Fatalf("blocked dependency %q classified %v, want reachHeldFresh — the walk reaches the blocker's own blocker and reports who holds it", deeper.ID, kind)
	}
	if !end.Clearable {
		t.Fatalf("blocked dependency %q is not clearable, want it clearable — the chain ends there, and an outside event does not hold it", deeper.ID)
	}
}

// A scope stopped by its own ticket, held by a label on that ticket, names the
// ticket in Held with the reason. Nothing in it is in progress, so Held is what
// keeps the scope from reading as waiting on work underway; and a ticket inside
// the epic cannot take a blocks edge, so it is not reported as a blocker.
func TestExhaustionNamesTheScopesOwnTicketHeldByALabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		label string
		kind  reachKind
	}{
		{label: ExternalLabel, kind: reachAwaitingOutside},
		{label: NeedsDesignLabel, kind: reachHeldByItself},
	} {
		t.Run(tc.label, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			lead := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a"), Labels: []string{tc.label}})
			h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a")})

			rows, details, epics := h.gather()
			standings := claims.Standings{laneOf(t, details, rowByID(t, rows, lead.ID)): heldBy(selfAttribution)}
			outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
			exhausted, ok := outcome.(Exhausted)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want Exhausted — A.1 is held by %q and A.2 waits behind it", outcome, outcome, tc.label)
			}
			if !slices.Equal(exhausted.Epics, []model.ID{epicA.ID}) {
				t.Fatalf("exhausted.Epics = %v, want [%q]", exhausted.Epics, epicA.ID)
			}
			if want := (claims.Progress{Done: 0, Total: 2}); exhausted.Progress != want {
				t.Fatalf("exhausted.Progress = %+v, want %+v", exhausted.Progress, want)
			}
			if len(exhausted.Held) != 1 || exhausted.Held[0].ID != lead.ID || exhausted.Held[0].Kind != tc.kind {
				t.Fatalf("exhausted.Held = %+v, want %s alone, reach kind %v", exhausted.Held, lead.ID, tc.kind)
			}
			if len(exhausted.Blocked) != 0 {
				t.Fatalf("exhausted.Blocked = %+v, want none — %q is inside the epic, not a blocker outside it", exhausted.Blocked, lead.ID)
			}
		})
	}
}

// The external label holds a ticket in flight too: it says no work here moves
// the ticket, which starting it did not change. So lit next neither resumes it
// for the checkout that started it nor offers it to another as abandoned work,
// and it serves the plain ticket ranked behind it instead.
func TestAnExternalTicketInFlightIsNeverServed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		ownsLane bool
	}{
		{name: "in this checkout's own lane", ownsLane: true},
		{name: "in a lane nobody holds", ownsLane: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			// The two sit in different epics: with the waiting ticket's lane
			// held, its epic is the checkout's scope, and the plain ticket
			// has to be outside it to be served past the exhaustion rather
			// than as a sibling lane of the same epic.
			epicW := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic W"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			waiting := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream fix lands"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicW.ID, Labels: []string{ExternalLabel}})
			h.transition(waiting.ID, model.Start{Assignee: "tester"})
			epicP := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic P"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			plain := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Plain work"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicP.ID})

			rows, details, epics := h.gather()
			standings := claims.Standings{}
			if tc.ownsLane {
				standings[laneOf(t, details, rowByID(t, rows, waiting.ID))] = heldBy(selfAttribution)
			}
			outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
			var served annotation.AnnotatedIssue
			switch o := outcome.(type) {
			case ServedFromNewLane:
				served = o.Row
			case ServedPastExhaustion:
				served = o.Row
				if !slices.ContainsFunc(o.Exhaustion.Held, func(r rowReach) bool { return r.ID == waiting.ID && r.Kind == reachAwaitingOutside }) {
					t.Fatalf("exhaustion.Held = %+v, want %q in it, awaiting an outside event", o.Exhaustion.Held, waiting.ID)
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
// so exhaustion names it as such and never reports it clearable: no ticket
// filed here can clear it. The same holds for a blocker that waits on it,
// whether nobody holds its lane, another checkout does, or a label on itself
// holds it too: a ticket filed against it leaves it waiting on the outside
// event. Beside a blocker a ticket could clear, that blocker alone is
// clearable.
func TestExhaustionNeverOffersToClearABlockerAwaitingAnOutsideEvent(t *testing.T) {
	t.Parallel()
	// path makes A.2 wait on upstream, the external ticket, by way of a ticket
	// in epic Y when the row has one, and returns it.
	type path func(h readyTestHarness, a2, upstream, epicY model.Issue) model.Issue
	direct := func(h readyTestHarness, a2, upstream, _ model.Issue) model.Issue {
		h.addDependency(a2.ID, upstream.ID)
		return upstream
	}
	through := func(labels ...string) path {
		return func(h readyTestHarness, a2, upstream, epicY model.Issue) model.Issue {
			between := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Waits on upstream"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicY.ID, Lane: storage.MustParseLane("between"), Labels: labels})
			h.addDependency(a2.ID, between.ID)
			h.addDependency(between.ID, upstream.ID)
			return between
		}
	}
	for _, tc := range []struct {
		name string
		path path
		// heldForeign: another checkout holds the lane of the ticket path returns.
		heldForeign bool
		// alsoPlain: A.2 also waits on a ticket another checkout holds, which
		// waits on nothing — the one blocker a ticket filed here could clear.
		alsoPlain bool
	}{
		{name: "external blocker alone", path: direct},
		{name: "external blocker in a lane another checkout holds", path: direct, heldForeign: true},
		{name: "external blocker behind a blocker nobody holds", path: through()},
		{name: "external blocker behind a blocker another checkout holds", path: through(), heldForeign: true},
		{name: "external blocker behind a blocker a label on itself holds", path: through(NeedsDesignLabel)},
		{name: "external blocker beside a clearable one", path: direct, alsoPlain: true},
		{name: "external blocker behind a blocker, beside a clearable one", path: through(), alsoPlain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
			h.transition(a1.ID, model.Start{Assignee: "tester"})
			h.transition(a1.ID, model.Done{})
			a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
			epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream fix lands"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID, Lane: storage.MustParseLane("upstream"), Labels: []string{ExternalLabel}})
			epicY := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic Y"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			reached := tc.path(h, a2, upstream, epicY)
			var foreign []model.ID
			if tc.heldForeign {
				foreign = append(foreign, reached.ID)
			}
			var wantClearable []model.ID
			if tc.alsoPlain {
				plain := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Theirs"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicY.ID, Lane: storage.MustParseLane("plain")})
				h.addDependency(a2.ID, plain.ID)
				foreign = append(foreign, plain.ID)
				wantClearable = append(wantClearable, plain.ID)
			}
			rows, details, epics := h.gather()
			standings := claims.Standings{model.LaneOf(a1, &epicA): heldBy(selfAttribution)}
			for _, id := range foreign {
				standings[laneOf(t, details, rowByID(t, rows, id))] = heldBy(otherAttribution)
			}

			outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
			exhausted, ok := outcome.(Exhausted)
			if !ok {
				t.Fatalf("routeNext = %#v (%T), want Exhausted — epic A's one open row waits on %q", outcome, outcome, upstream.ID)
			}
			idx := slices.IndexFunc(exhausted.Blocked, func(r blockerReach) bool { return r.ID == upstream.ID })
			if idx < 0 || readingOf(t, exhausted.Blocked[idx]).Kind != reachAwaitingOutside {
				t.Fatalf("exhausted.Blocked = %v, want %q classified reachAwaitingOutside", exhausted.Blocked, upstream.ID)
			}
			if !slices.Equal(exhausted.Epics, []model.ID{epicA.ID}) {
				t.Fatalf("exhausted.Epics = %v, want [%q] — the epic a clearing ticket is filed under", exhausted.Epics, epicA.ID)
			}
			var clearable []model.ID
			for _, blocker := range exhausted.Blocked {
				if blocker.Clearable {
					clearable = append(clearable, blocker.ID)
				}
			}
			if !slices.Equal(clearable, wantClearable) {
				t.Fatalf("clearable blockers = %v of %+v, want %v — only a blocker a ticket filed here could free; %q waits only on %q", clearable, exhausted.Blocked, wantClearable, reached.ID, upstream.ID)
			}
		})
	}
}

// A blocker another checkout has started is past its prerequisites, as routing
// treats work in flight: the walk does not descend it, so an open dependency
// it still carries is named nowhere, and does not stop the blocker from being
// reported clearable.
func TestExhaustionOffersToClearAStartedBlockerWhateverItStillDependsOn(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicY := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic Y"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	started := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Theirs, underway"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicY.ID, Lane: storage.MustParseLane("started")})
	h.transition(started.ID, model.Start{Assignee: "them"})
	epicZ := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic Z"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	still := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Still open"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicZ.ID, Lane: storage.MustParseLane("still")})
	h.addDependency(a2.ID, started.ID)
	h.addDependency(started.ID, still.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                         heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, started.ID)): heldBy(otherAttribution),
		laneOf(t, details, rowByID(t, rows, still.ID)):   heldBy(otherAttribution),
	}
	exhausted, ok := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{}).(Exhausted)
	if !ok {
		t.Fatal("routeNext did not answer Exhausted — nothing on the path is takeable")
	}
	if len(exhausted.Blocked) != 1 || exhausted.Blocked[0].ID != started.ID || !exhausted.Blocked[0].Clearable {
		t.Fatalf("exhausted.Blocked = %v, want %q alone, clearable — %q is past its prerequisites", exhausted.Blocked, started.ID, started.ID)
	}
}

// Step 2's other admission. Every expired-foreign test for this step uses an
// open row, a plain start; this is the in-flight disjunct, which is the case
// ServedFromEpicLane's doc comment actually asserts and the only one where the
// row served is somebody's abandoned work.
func TestRouteNextServesAnAbandonedSiblingLane(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	h.transition(a2.ID, model.Start{Assignee: "other"})

	rows, details, epics := h.gather()
	orphan(t, rows, a2.ID)
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): expired,
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane — an abandoned sibling lane of our own epic is servable", outcome, outcome)
	}
	if served.Row.ID != a2.ID {
		t.Fatalf("served = %q, want %q", served.Row.ID, a2.ID)
	}
	if served.Row.State() != model.StateInProgress {
		t.Fatalf("served row state = %v, want in progress — this pick inherits %q's unfinished work rather than beginning it", served.Row.State(), a2.ID)
	}
}

// Design step 6 in its plainest form: an expired claim does not veto an
// otherwise-ready ticket. Nothing here is started and nothing is orphaned, so
// the pick rests on the lane being unheld alone — the `!started && IsReady()`
// half of capacityFor that every other expired-foreign test in this file
// reaches only through an in-progress row.
func TestRouteNextServesOpenTicketInAnExpiredForeignLane(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	epicC := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic C"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	c1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("C.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicC.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, b1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                    heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)): expired,
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromEpicLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromEpicLane (an expired sibling lane of our own epic is nobody's)", outcome, outcome)
	}
	if served.Row.ID != a2.ID {
		t.Fatalf("served = %q, want %q", served.Row.ID, a2.ID)
	}
	if served.Row.State() != model.StateOpen {
		t.Fatalf("served row state = %v, want open — the pick must rest on the lane being unheld alone, never on an orphan", served.Row.State())
	}
}

// Step 1 competes two capacities in one pick, and the comment above `pick`
// says why ranking them against each other is wrong. Run in both rank orders:
// a preference for either capacity fails one arm.
func TestRouteNextStep1RanksAcrossCapacitiesRatherThanBetweenThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		resumeFirst bool
	}{
		{"the resumable lane ranks first", true},
		{"the servable lane ranks first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			mk := func(title, lane string) model.Issue {
				return h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle(title), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane(lane)})
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

			outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
			if tc.resumeFirst {
				resumed, ok := outcome.(ResumedOwnWork)
				if !ok || resumed.Row.ID != inFlight.ID {
					t.Fatalf("routeNext = %#v (%T), want ResumedOwnWork on %q — rank order decides, never a preference for servable work", outcome, outcome, inFlight.ID)
				}
				return
			}
			served, ok := outcome.(ServedFromClaim)
			if !ok || served.Row.ID != ready.ID {
				t.Fatalf("routeNext = %#v (%T), want ServedFromClaim on %q — rank order decides, never a preference for resumable work", outcome, outcome, ready.ID)
			}
		})
	}
}

// Step 1b applies the verdict itself rather than going through pick/accept, so
// its admission of abandoned work is a path of its own: a dependency gating our
// lane, in flight in a lane nobody holds, is offered here even though the
// global-pool step never sees it.
func TestRouteNextServesAnAbandonedOnPathDependency(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a1")})
	h.transition(a1.ID, model.Start{Assignee: "tester"})
	h.transition(a1.ID, model.Done{})
	a2 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("a2")})
	epicX := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic X"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	dep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("External blocker"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicX.ID})
	h.addDependency(a2.ID, dep.ID)
	h.transition(dep.ID, model.Start{Assignee: "other"})

	rows, details, epics := h.gather()
	orphan(t, rows, dep.ID)
	standings := claims.Standings{
		model.LaneOf(a1, &epicA):                     heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, a2.ID)):  heldBy(selfAttribution),
		laneOf(t, details, rowByID(t, rows, dep.ID)): expired,
	}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	for _, tc := range []struct {
		name string
	}{
		{"an in-flight row in a lane nobody holds, whoever started it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})
			h.transition(a1.ID, model.Start{Assignee: "tester"})

			epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
			b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

			rows, details, epics := h.gather()
			row := rowByID(t, rows, a1.ID)
			if ClassifyReadiness(row.Annotations).IsOrphaned() {
				t.Fatalf("fixture %q is orphaned; this test's premise is an in-flight row the orphan clock has NOT reached", a1.ID)
			}
			standings := claims.Standings{laneOf(t, details, row): expired}

			outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})

	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})

	rows, details, epics := h.gather()
	if rows[0].ID != a1.ID {
		t.Fatalf("fixture rank order = %q first, want %q — this test's premise is that the expired lane ranks top", rows[0].ID, a1.ID)
	}
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	h.transition(a1.ID, model.Start{Assignee: "whoever-came-before"})

	rows, details, epics := h.gather()
	orphan(t, rows, a1.ID)
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): expired}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), publicAttribution, focusScope{}, storage.IssueCriteria{})
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicB.ID})
	h.transition(a1.ID, model.Start{Assignee: "whoever-is-working-it"})

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, a1.ID)): heldBy(publicAttribution)}

	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), publicAttribution, focusScope{}, storage.IssueCriteria{})
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane: a fresh public hold is foreign work in flight", outcome, outcome)
	}
	if served.Row.ID != b1.ID {
		t.Fatalf("served = %q, want %q (the held lane routed around)", served.Row.ID, b1.ID)
	}
}

// readingOf is the reading of a blocker the test expects in view, failing the
// test when the run could not read it.
func readingOf(t *testing.T, blocker blockerReach) rowReach {
	t.Helper()
	row, ok := blocker.read()
	if !ok {
		t.Fatalf("blocker %q is out of view, want it read through work in view", blocker.ID)
	}
	return row
}

// "No ready work" alone would answer two opposite questions in identical
// words: an empty backlog, and a backlog full of work this checkout may not
// have.
//
// Two kinds are put in one pool on purpose. A single-kind fixture passes
// against a walk that reports one kind for everything it went past: the
// outcome has to say that one row is somebody's live work and the other is
// merely gated, because those call for different acts.
func TestNoWorkNamesEachRowThePoolWalkWentPast(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Theirs, in flight"), Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID, Lane: storage.MustParseLane("held")})
	h.transition(held.ID, model.Start{Assignee: "tester"})
	// Gated by the row above — in its own lane, so the dependency is the one
	// thing holding it — and the whole pool is unstartable without either
	// row being takeable, for two different reasons.
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ours to want, not to start"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: storage.MustParseLane("gated")})
	h.addDependencyWithinEpic(gated.ID, held.ID)

	rows, details, epics := h.gather()
	standings := claims.Standings{laneOf(t, details, rowByID(t, rows, held.ID)): heldBy(otherAttribution)}

	// This checkout holds nothing, so routing starts straight at the global pool.
	outcome := routeNext(rows, details, epics, standings, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	noWork, ok := outcome.(NoWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want NoWork — nothing in the pool is takeable", outcome, outcome)
	}
	want := map[model.ID]reachKind{held.ID: reachHeldFresh, gated.ID: reachNotReady}
	got := map[model.ID]reachKind{}
	for _, row := range noWork.Unreachable {
		got[row.ID] = row.Kind
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("NoWork.Unreachable[%q] = %v, want %v — the walk's own verdict, kept rather than discarded (got %v)", id, got[id], kind, got)
		}
	}
}

// The pool walk can report every kind of row it goes past. Held fresh and not
// ready are reached above, and awaiting outside by
// TestNextLeavesTheRowsItWentPastOnTheEvent; this reaches the fourth, a row
// held by a label on itself.
func TestNoWorkReportsARowHeldByItself(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	held := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Needs design"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("held"), Labels: []string{NeedsDesignLabel}})

	rows, details, epics := h.gather()
	outcome := routeNext(rows, details, epics, claims.Standings{}, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	noWork, ok := outcome.(NoWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want NoWork — the one row is held by a label on itself", outcome, outcome)
	}
	if len(noWork.Unreachable) != 1 || noWork.Unreachable[0].ID != held.ID || noWork.Unreachable[0].Kind != reachHeldByItself {
		t.Fatalf("NoWork.Unreachable = %+v, want exactly %s, held by itself", noWork.Unreachable, held.ID)
	}
}

// The other half of the same type: an empty backlog. The outcome names only
// rows the walk went past or a focus label withheld, so with no rows it names
// none — both lists empty is what tells an empty backlog from one full of work
// this checkout may not have.
func TestNoWorkOnAGenuinelyEmptyBacklogIsUnchanged(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)

	rows, details, epics := h.gather()
	outcome := routeNext(rows, details, epics, claims.Standings{}, evidenceIn(h.t, h.ctx, h.ap), selfAttribution, focusScope{}, storage.IssueCriteria{})
	noWork, ok := outcome.(NoWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want NoWork for an empty backlog", outcome, outcome)
	}
	if len(noWork.Unreachable) != 0 {
		t.Fatalf("NoWork.Unreachable = %v, want empty — nothing was gathered, so nothing was gone past", noWork.Unreachable)
	}
	if len(noWork.OffPath) != 0 {
		t.Fatalf("NoWork.OffPath = %v, want empty — no focus label withheld anything", noWork.OffPath)
	}
}
