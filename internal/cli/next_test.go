package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// runNextOutcome reproduces exactly what `lit next` decides — the shared
// workable pipeline and claim routing — and hands back the routing verdict
// itself, so a test can assert WHICH case picked a row rather than only which
// row came out. [LAW:single-enforcer] one reproduction of the pipeline; the
// narrowing helpers below all read through it.
//
// This harness mints no stream token (newTestCLIApp builds the App directly,
// with no app.Open to mint one), which is not the absence of an identity: an
// unattributed checkout IS the public checkout, and so is every write it makes,
// so the harness holds the lanes it works exactly as a token-carrying checkout
// holds its own. That is what makes ServedFromClaim, ResumedOwnWork and
// Exhausted reachable from here at all. Use asCheckout to write as somebody
// else.
func (h readyTestHarness) runNextOutcome() NextOutcome {
	h.t.Helper()
	annotated, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	cc, err := gatherClaimContext(h.ctx, io.Discard, h.ap)
	if err != nil {
		h.t.Fatalf("gatherClaimContext error = %v", err)
	}
	return routeNext(annotated, details, cc.standings, cc.self)
}

// runNextRow narrows an outcome to the row it served, for the ordering and
// filter tests that care which ticket came back and not how routing reached it.
//
// [LAW:parse-dont-validate] The switch is the boundary between "routing served
// something" and "routing declined to", and it is total over the sealed set:
// every served variant carries a Row, and the two that carry none fail here, by
// name, instead of yielding the zero AnnotatedIssue — a ticket with an empty id
// that would read as an ordinary answer and fail some later assertion far from
// the cause.
func (h readyTestHarness) runNextRow() annotation.AnnotatedIssue {
	h.t.Helper()
	outcome := h.runNextOutcome()
	switch served := outcome.(type) {
	case ServedFromClaim:
		return served.Row
	case ResumedOwnWork:
		return served.Row
	case ServedFromEpicLane:
		return served.Row
	case ServedFromNewLane:
		return served.Row
	}
	h.t.Fatalf("routeNext = %#v (%T), want an outcome carrying a served row", outcome, outcome)
	return annotation.AnnotatedIssue{}
}

// asCheckout re-attributes everything the harness writes from here on to
// another checkout's stream token — the same store-level seam app.Open uses to
// stamp a real checkout's identity onto its work, which is why a test driving
// it produces evidence indistinguishable from a second checkout's.
//
// It is how a test spells "somebody else did this". Without it every write
// belongs to the public checkout, which is this checkout's own identity, so an
// unswitched write can only ever produce work of our own.
func (h readyTestHarness) asCheckout(streamToken string) {
	h.t.Helper()
	h.ap.Store.AttributeTo(streamToken)
}

func (h readyTestHarness) runNextErr(args ...string) error {
	h.t.Helper()
	var stdout bytes.Buffer
	return runNext(h.ctx, &stdout, h.ap, args)
}

func (h readyTestHarness) runNextText(args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	if err := runNext(h.ctx, &stdout, h.ap, args); err != nil {
		h.t.Fatalf("runNext(%v) error = %v", args, err)
	}
	return stdout.String()
}

// `lit next` returns the top of the ready partition: the first open, unblocked
// leaf in the same composite-rank order `lit ready` produces.
func TestRunNextReturnsTopReadyLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First leaf", Topic: "next", IssueType: "task", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second leaf", Topic: "next", IssueType: "task", Priority: 0})

	got := h.runNextRow()
	if got.ID != first.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order)", got.ID, first.ID)
	}
}

// An in-progress leaf in a lane ANOTHER checkout holds is not a workable start;
// `lit next` routes around it and returns the next open one. The claim is what
// makes it untouchable — its holder is working it right now — so this is the
// half of the old "in-progress leaves are skipped" rule that survives, and it
// is stated against a foreign holder rather than against the state alone.
func TestRunNextRoutesAroundAnInProgressLeafHeldElsewhere(t *testing.T) {
	h := newReadyTestHarness(t)
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	openLeaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable", Topic: "next", IssueType: "task", Priority: 0})
	h.asCheckout("otherstream01")
	h.applyAction(inProgress.ID, model.Start{Assignee: "tester"}, "")

	got := h.runNextRow()
	if got.ID != openLeaf.ID {
		t.Fatalf("next.ID = %q, want %q (a lane held elsewhere is routed around)", got.ID, openLeaf.ID)
	}
}

// The other half is the one the harness could not reach before, and it is the
// opposite answer on the same shape: work in flight in a lane THIS checkout
// holds is handed back to be resumed, not skipped in favour of a lower-ranked
// open leaf. Skipping it is what once hid the very ticket a checkout was
// working from that checkout (links-claims-1b0p, N8) — the agent asked what to
// do next and was told to start something else.
//
// The harness mints no stream token, so its writes and its identity are alike
// the public checkout, and starting a ticket is enough to hold the lane.
func TestRunNextResumesOwnWorkInFlight(t *testing.T) {
	h := newReadyTestHarness(t)
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	lowerRanked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable", Topic: "next", IssueType: "task", Priority: 0})
	h.applyAction(inProgress.ID, model.Start{Assignee: "tester"}, "")

	outcome := h.runNextOutcome()
	resumed, ok := outcome.(ResumedOwnWork)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ResumedOwnWork", outcome, outcome)
	}
	if resumed.Row.ID != inProgress.ID {
		t.Fatalf("resumed.ID = %q, want %q (our own work in flight)", resumed.Row.ID, inProgress.ID)
	}

	text := h.runNextText()
	if !strings.Contains(text, "resuming "+inProgress.ID) {
		t.Fatalf("next output = %q, want it to announce resuming %s", text, inProgress.ID)
	}
	if strings.Contains(text, lowerRanked.ID) {
		t.Fatalf("next output = %q, want %q not served while our own work is in flight", text, lowerRanked.ID)
	}
}

// A lane we hold whose next ticket is startable serves it with NO announcement:
// no claim is established, because we already hold the lane, so `next` prints
// exactly what it always printed. This is the routing case with the quietest
// output and therefore the one most easily broken without anyone noticing.
//
// The hold rests on a `done`, not a `start` — completing a ticket mid-lane
// keeps the lane you are halfway through — so this also pins that the lane
// survives the ticket that established it being closed.
func TestRunNextServesTheNextTicketOfALaneWeHold(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	nextUp := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})
	h.applyAction(finished.ID, model.Done{}, "finished")

	outcome := h.runNextOutcome()
	served, ok := outcome.(ServedFromClaim)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromClaim", outcome, outcome)
	}
	if served.Row.ID != nextUp.ID {
		t.Fatalf("served.ID = %q, want %q (the next ticket of the lane we hold)", served.Row.ID, nextUp.ID)
	}

	text := h.runNextText()
	if !strings.Contains(text, nextUp.ID) {
		t.Fatalf("next output = %q, want %q served", text, nextUp.ID)
	}
	for _, announcement := range []string{"starting", "claims", "taking over", "resuming", "continuing epic"} {
		if strings.Contains(text, announcement) {
			t.Fatalf("next output = %q, want no %q announcement — the lane was already ours", text, announcement)
		}
	}
}

// Exhaustion is the loud refusal: our epic still has open work, none of it is
// reachable, and `next` says so instead of hopping to a leaf outside the epic.
// It is an error and not a row, so a caller that ignored the distinction would
// hand an agent the zero ticket — which is why the outcome type seals the two
// apart and why this asserts through runNext, where the exit path is decided.
//
// The gating dependency is itself blocked, so it is on our path and NOT ours to
// take: that is what forecloses routing step 1b and leaves exhaustion as the
// only honest answer.
func TestRunNextExhaustedNamesTheBlockerGatingOurEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside dependency", Topic: "next", IssueType: "task", Priority: 0})
	blockersBlocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Deeper still", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(gated.ID, blocker.ID)
	h.addDependency(blocker.ID, blockersBlocker.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	outcome := h.runNextOutcome()
	exhausted, ok := outcome.(Exhausted)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want Exhausted", outcome, outcome)
	}
	if len(exhausted.Epics) != 1 || exhausted.Epics[0] != epic.ID {
		t.Fatalf("exhausted.Epics = %v, want [%s]", exhausted.Epics, epic.ID)
	}
	if len(exhausted.Blocked) != 1 || exhausted.Blocked[0].ID != blocker.ID {
		t.Fatalf("exhausted.Blocked = %+v, want the one gating dependency %s", exhausted.Blocked, blocker.ID)
	}

	err := h.runNextErr()
	if err == nil {
		t.Fatalf("runNext on an exhausted epic = nil error, want the loud diagnostic")
	}
	for _, want := range []string{epic.ID, blocker.ID, "not startable right now"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runNext error = %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), blockersBlocker.ID) {
		t.Fatalf("runNext error = %q, want it to stop at our own path and not walk %q", err, blockersBlocker.ID)
	}
}

// Blocked leaves (open dependency) are skipped just as `lit ready` partitions
// them out of the ready section. With deterministic creation-order ranking the
// blocker (rank 1, no parent epic, no own dependencies) is unambiguously top,
// so the assertion pins both "dependent skipped" and the exact expected pick.
func TestRunNextSkipsBlockedLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "next", IssueType: "task", Priority: 1})
	dependent := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(dependent.ID, blocker.ID)
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Unblocked third", Topic: "next", IssueType: "task", Priority: 0})

	got := h.runNextRow()
	if got.ID == dependent.ID {
		t.Fatalf("next.ID = %q (dependent), want a non-blocked leaf", got.ID)
	}
	if got.ID != blocker.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order after skipping blocked dependent)", got.ID, blocker.ID)
	}
}

// `lit next` exposes the standard narrowing knobs so "the next workable bug"
// is expressible; the filter runs in the shared pipeline, so next answers the
// same narrowed question ready/backlog/queue would.
func TestRunNextTypeFilterPicksMatchingLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	task := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Top-ranked task", Topic: "next", IssueType: "task", Priority: 1})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Lower-ranked bug", Topic: "next", IssueType: "bug", Priority: 0})

	text := h.runNextText("--type", "bug")
	if !strings.Contains(text, bug.ID) {
		t.Fatalf("next --type bug output = %q, want %q picked", text, bug.ID)
	}
	if strings.Contains(text, task.ID) {
		t.Fatalf("next --type bug output = %q, want %q filtered out despite outranking the bug", text, task.ID)
	}
}

// --limit and --columns stay off next: a single-row summary has no row count
// or column set to vary, so accepting them would be accepting input the
// command cannot honor.
func TestRunNextRejectsLimitAndColumns(t *testing.T) {
	h := newReadyTestHarness(t)
	for _, args := range [][]string{{"--limit", "2"}, {"--columns", "id"}} {
		err := h.runNextErr(args...)
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("runNext(%v) error = %v, want unknown-flag usage error", args, err)
		}
	}
}

// No ready work → non-nil error so the calling shell exits non-zero.
// Agents script `lit next` in loops; silent empty success would be a hang.
func TestRunNextErrorsWhenNoReadyWork(t *testing.T) {
	h := newReadyTestHarness(t)
	err := h.runNextErr()
	if err == nil {
		t.Fatal("runNext() error = nil, want non-nil for empty ready set")
	}
	if !strings.Contains(err.Error(), "no ready work") {
		t.Fatalf("runNext() error = %q, want contains \"no ready work\"", err.Error())
	}
}

// `--continue` is retired: it predates claim routing, which now subsumes the
// epic-affinity bias unconditionally (routeNext's ServedFromEpicLane step).
// Passing the flag surfaces a pointer to that replacement instead of an
// unhelpful "unknown flag" error.
func TestRunNextContinueFlagIsRetired(t *testing.T) {
	h := newReadyTestHarness(t)
	err := h.runNextErr("--continue")
	if err == nil {
		t.Fatal("runNext(--continue) error = nil, want retirement error")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("runNext(--continue) error = %q, want it to say the flag is retired", err.Error())
	}
}

// A leaf picked by `lit next` carries its parent epic inline so the agent knows
// which epic it would be joining before it claims the leaf.
func TestRunNextCarriesParentEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Container", Topic: "next", IssueType: "epic", Priority: 1})
	leaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})

	got := h.runNextRow()
	if got.ID != leaf.ID {
		t.Fatalf("next.ID = %q, want %q", got.ID, leaf.ID)
	}
	if got.ParentEpic == nil {
		t.Fatal("next.ParentEpic = nil, want populated for leaf under epic")
	}
	if got.ParentEpic.ID != epic.ID {
		t.Fatalf("next.ParentEpic.ID = %q, want %q", got.ParentEpic.ID, epic.ID)
	}
}

// Every announcement renderNextOutcome can print, asserted as bytes. The
// outcome is constructed rather than routed to, which is what lets all four
// served variants be reached from a harness that mints no stream token — the
// routing that produces each one is pinned separately in next_route_test.go.
// Standings are left empty deliberately: formatClaimLine stays on its
// ("", false) arm, so nothing but the announcement is under assertion.
func TestRenderNextOutcomeAnnouncesEachClaimEstablishingPick(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	fresh := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	h.transition(inFlight.ID, model.Start{Assignee: "other"})

	rows, details := h.gather()
	cc := claimContext{self: selfAttribution}
	freshRow := rowByID(t, rows, fresh.ID)
	inFlightRow := rowByID(t, rows, inFlight.ID)

	for _, tc := range []struct {
		name    string
		outcome NextOutcome
		want    string
	}{
		{"a lane already held says nothing", ServedFromClaim{Row: freshRow}, ""},
		{"own work in flight is resumed, not started", ResumedOwnWork{Row: inFlightRow}, "resuming " + inFlight.ID + " — already in progress in a lane you hold"},
		{"the epic's next lane names the claim it establishes", ServedFromEpicLane{Row: freshRow, Epic: epicA.ID, Lane: "A#1"}, "continuing epic " + epicA.ID + ": starting " + fresh.ID + " claims A#1"},
		{"abandoned work is taken over, not started", ServedFromNewLane{Row: inFlightRow, Lane: "A#2"}, "taking over " + inFlight.ID + " (in progress, abandoned) — claims A#2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := renderNextOutcome(&buf, tc.outcome, details, cc); err != nil {
				t.Fatalf("renderNextOutcome(%T) error = %v", tc.outcome, err)
			}
			out := buf.String()
			if tc.want == "" {
				for _, verb := range []string{"resuming", "starting", "continuing", "taking over"} {
					if strings.HasPrefix(out, verb) {
						t.Fatalf("ServedFromClaim announced %q; its contract is that no claim is established and nothing is said", out)
					}
				}
				return
			}
			if !strings.HasPrefix(out, tc.want+"\n") {
				t.Fatalf("renderNextOutcome printed %q, want it to open with %q", out, tc.want)
			}
		})
	}
}
