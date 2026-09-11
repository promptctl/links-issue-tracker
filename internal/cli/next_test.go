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
// The harness mints a stream token the way app.Open does, so its own writes
// hold lanes as this checkout. Use asCheckout to write as somebody else.
func (h readyTestHarness) runNextOutcome() NextOutcome {
	h.t.Helper()
	annotated, details, focus, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	cc, err := gatherClaimContext(h.ctx, io.Discard, h.ap)
	if err != nil {
		h.t.Fatalf("gatherClaimContext error = %v", err)
	}
	// The gathered scope, not focusScope{}: this helper stands in for `lit next`
	// itself, and handing routing an empty scope here would quietly answer every
	// focus test from the unfocused path — green, and about a command nobody runs.
	return routeNext(annotated, details, cc.standings, cc.self, focus)
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
// It is how a test spells "somebody else did this"; an empty token writes as
// the public checkout. Without it every write is this checkout's own.
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
	if !strings.Contains(text, inProgress.ID+" is already in progress in a lane you hold") {
		t.Fatalf("next output = %q, want it to say %s is already in flight and ours", text, inProgress.ID)
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
	// The claim line beneath the row still prints "claimed here: …" — that is a
	// true report of a hold we already have. What must be absent is every phrase
	// that offers a commitment, because there is none left to make.
	for _, advice := range []string{"run `lit start", "to claim", "to take over", "is already in progress"} {
		if strings.Contains(text, advice) {
			t.Fatalf("next output = %q, want no %q line — the lane was already ours, so there is nothing to commit and nothing to say", text, advice)
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

// `lit next --status in_progress` is a documented flag combination that had no
// test at all, which is how it stayed unable to return anything for as long as
// routing gated servability on model.StateOpen (links-cli-q7hg). It is the
// question an agent asks after a crash or a context reset — "what was I already
// on?" — so these two tests pin both answers it can get, and the wording that
// separates them.
//
// Driven through runNext rather than routeNext: what was untested is the FLAG,
// and only the real command proves --status reaches the gather that makes the
// in_progress row available to route at all.
func TestRunNextStatusInProgressResumesOurOwnWorkInFlight(t *testing.T) {
	h := newReadyTestHarness(t)
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	h.applyAction(mine.ID, model.Start{Assignee: "tester"}, "")

	text := h.runNextText("--status", "in_progress")
	if !strings.Contains(text, mine.ID) {
		t.Fatalf("next --status in_progress = %q, want it to hand back %q — narrowing to in_progress is how an agent asks what it already holds", text, mine.ID)
	}
	if !strings.Contains(text, mine.ID+" is already in progress in a lane you hold") {
		t.Fatalf("next --status in_progress = %q, want it reported as a resumption of %q and not a fresh start", text, mine.ID)
	}
}

// The half that survived links-claims-1b0p: when the only in_progress rows are
// held fresh by other checkouts, every one verdicts routeAround and the walk
// ends at NoWork. That used to print "no ready work" — byte-identical to an
// empty backlog, telling an agent asking what it was on that its work is gone.
func TestRunNextStatusInProgressNamesForeignHeldWorkRatherThanReadingEmpty(t *testing.T) {
	h := newReadyTestHarness(t)
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs, in flight", Topic: "next", IssueType: "task", Priority: 1})
	h.asCheckout("otherstream01")
	h.applyAction(theirs.ID, model.Start{Assignee: "tester"}, "")

	err := h.runNextErr("--status", "in_progress")
	if err == nil {
		t.Fatal("next --status in_progress error = nil, want the loud diagnostic: the row exists and is not ours to take")
	}
	if err.Error() == "no ready work" {
		t.Fatal("next --status in_progress = \"no ready work\" — the exact empty-backlog wording, for a backlog holding one live in_progress ticket")
	}
	if !strings.Contains(err.Error(), theirs.ID) {
		t.Fatalf("next --status in_progress = %q, want it to name %q rather than report the queue empty", err.Error(), theirs.ID)
	}
	if !strings.Contains(err.Error(), "another checkout holds right now") {
		t.Fatalf("next --status in_progress = %q, want it to say why %q is not servable", err.Error(), theirs.ID)
	}
	// Still an answer and not a fault: naming the row must not have moved this
	// off the exit code a looping caller reads (links-cli-cpou).
	var stderr bytes.Buffer
	if code := WriteCommandError(&stderr, err); code != ExitNoWork {
		t.Fatalf("exit code = %d, want %d (ExitNoWork) — work held elsewhere is the backlog's state, not a fault", code, ExitNoWork)
	}
}

// No ready work → non-nil error so the calling shell exits non-zero.
// Agents script `lit next` in loops; silent empty success would be a hang.
//
// The exit code separates the two nonzero meanings a looping caller has to tell
// apart — "stop, there is nothing for you" versus "lit is broken" — so that
// telling them apart never requires parsing the English (links-cli-cpou).
func TestRunNextErrorsWhenNoReadyWork(t *testing.T) {
	h := newReadyTestHarness(t)
	err := h.runNextErr()
	if err == nil {
		t.Fatal("runNext() error = nil, want non-nil for empty ready set")
	}
	// Exactly, not merely contains: NoWork now appends a clause naming the rows
	// the pool walk went past, and an empty backlog has none, so the sentence
	// stays the one `next` has always printed (links-cli-q7hg, criterion 2). A
	// contains-check would pass on a message that had grown a clause here.
	if err.Error() != "no ready work" {
		t.Fatalf("runNext() error = %q, want exactly %q — an empty backlog gained no clause", err.Error(), "no ready work")
	}
	var stderr bytes.Buffer
	if code := WriteCommandError(&stderr, err); code != ExitNoWork {
		t.Fatalf("exit code = %d, want %d (ExitNoWork) — an empty backlog is not a generic fault", code, ExitNoWork)
	}
	if out := stderr.String(); strings.Contains(out, "Retry the command") || strings.Contains(out, "lit doctor") {
		t.Fatalf("an empty backlog must not be described as a retryable fault: %q", out)
	}
}

// TestRenderNextOutcomeTerminalOutcomesKeepTheirType pins links-cli-cpou at the
// seam that caused it. renderNextOutcome used to render the router's two
// terminal outcomes into UNTYPED errors, throwing away the discriminator
// routeNext had just established for the express purpose of keeping the
// exhaustion case distinguishable. Both sinks dispatch by type, so both fell
// through to "command_failed", whose remediation tells the agent to retry an
// answer that is deterministic and then to run `lit doctor` against a perfectly
// healthy workspace — two dead ends, attached to a message saying the situation
// calls for a deliberate act.
//
// The test drives the real seam rather than the error types in isolation:
// asserting commandErrorReason(Exhausted{}) alone would still pass if this
// function went back to wrapping the outcome in errors.New.
func TestRenderNextOutcomeTerminalOutcomesKeepTheirType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		outcome    NextOutcome
		wantReason string
		// wantAct is the deliberate act the message calls for but cannot name,
		// which is the whole job the remediation line has left to do.
		wantAct string
	}{
		{
			name:       "exhausted",
			outcome:    Exhausted{Epics: []string{"links-epic-abcd"}},
			wantReason: "scope_exhausted",
			wantAct:    "lit start <id>",
		},
		{
			name:       "no work",
			outcome:    NoWork{},
			wantReason: "no_ready_work",
			wantAct:    "lit new",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			occasion, err := renderNextOutcome(io.Discard, tc.outcome, nil, claimContext{})
			if err == nil {
				t.Fatalf("renderNextOutcome(%T) error = nil, want the terminal answer", tc.outcome)
			}
			// No ticket was handed back, so no next_pulled event fires — the
			// workflow dispatch must not be told a pull happened.
			if occasion.Event != "" || occasion.IssueID != "" {
				t.Fatalf("renderNextOutcome(%T) occasion = %#v, want none — no ticket was handed back", tc.outcome, occasion)
			}
			if got := commandErrorReason(err); got != tc.wantReason {
				t.Fatalf("commandErrorReason = %q, want %q — the outcome lost its type crossing the seam", got, tc.wantReason)
			}
			var stderr bytes.Buffer
			if code := WriteCommandError(&stderr, err); code != ExitNoWork {
				t.Fatalf("exit code = %d, want %d (ExitNoWork)", code, ExitNoWork)
			}
			out := stderr.String()
			if strings.Contains(out, "Retry the command") || strings.Contains(out, "lit doctor") {
				t.Fatalf("a deterministic terminal answer must carry neither a retry nor a doctor referral: %q", out)
			}
			// The remediation must agree with the message body, so the body has
			// to still be there to agree with.
			if !strings.Contains(out, tc.outcome.(error).Error()) {
				t.Fatalf("stderr dropped the outcome's own message: %q", out)
			}
			if !strings.Contains(out, tc.wantAct) {
				t.Fatalf("remediation does not name the deliberate act %q: %q", tc.wantAct, out)
			}
		})
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

// Every line renderNextOutcome can print, asserted as bytes. The outcome is
// constructed rather than routed to, so each line is asserted alone; the routing
// that produces each one is pinned in next_route_test.go. Standings are left
// empty deliberately: formatClaimLine stays on its ("", false) arm, so nothing
// but this line is under assertion.
func TestRenderNextOutcomeSpeaksOnlyInTheConditional(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	fresh := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	h.transition(inFlight.ID, model.Start{Assignee: "other"})

	rows, details := h.gather()
	cc := claimContext{self: selfAttribution}
	freshRow := rowByID(t, rows, fresh.ID)
	inFlightRow := rowByID(t, rows, inFlight.ID)
	freshLane := laneOf(t, details, freshRow)
	inFlightLane := laneOf(t, details, inFlightRow)

	for _, tc := range []struct {
		name    string
		outcome NextOutcome
		want    string
	}{
		{"a lane already held says nothing", ServedFromClaim{Row: freshRow}, ""},
		{"own work in flight is reported as the state it is in", ResumedOwnWork{Row: inFlightRow},
			inFlight.ID + " is already in progress in a lane you hold — continue where you left off"},
		{"the epic's next lane names what a start would lock", ServedFromEpicLane{Row: freshRow, Lane: freshLane},
			"run `lit start " + fresh.ID + "` to claim lane a1 of epic " + epicA.ID + " (a second lane of an epic you already hold a lane in)"},
		{"abandoned work is taken over, not claimed fresh", ServedFromNewLane{Row: inFlightRow, Lane: inFlightLane},
			inFlight.ID + " is in progress and abandoned — run `lit start " + inFlight.ID + "` to take over lane a2 of epic " + epicA.ID},
		// Step 2 admits takeoverWork, so the epic's next lane can carry an
		// abandoned row: the one place the verb-dependent sentence and the
		// fixed suffix are concatenated. Pinned whole, because a product left
		// partly covered is where this ticket's tautology survived.
		{"the epic's next lane takes over abandoned work, qualifier and all", ServedFromEpicLane{Row: inFlightRow, Lane: inFlightLane},
			inFlight.ID + " is in progress and abandoned — run `lit start " + inFlight.ID + "` to take over lane a2 of epic " + epicA.ID + " (a second lane of an epic you already hold a lane in)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := renderNextOutcome(&buf, tc.outcome, details, cc); err != nil {
				t.Fatalf("renderNextOutcome(%T) error = %v", tc.outcome, err)
			}
			out := buf.String()
			if tc.want == "" {
				if !strings.HasPrefix(out, fresh.ID) {
					t.Fatalf("ServedFromClaim printed %q; its contract is that no claim is established and nothing is said above the row", out)
				}
				return
			}
			if !strings.HasPrefix(out, tc.want+"\n") {
				t.Fatalf("renderNextOutcome printed %q, want it to open with %q", out, tc.want)
			}
		})
	}
}

// `lit next` reports a pick; `lit start` takes it. This is that contract as the
// only thing that finally settles it — not what the output says, but what the
// store holds after the command has run.
//
// Asserted through runNext rather than renderNextOutcome so the whole command
// path is under it, and over the pick that had the most to lie about: an
// unclaimed solo ticket, the case whose line once read "starting X claims X".
// The wording assertions elsewhere in this file all become vacuous if the
// command ever does start claiming, and this is what would still fail.
func TestRunNextClaimsNothingAndStartsNothing(t *testing.T) {
	h := newReadyTestHarness(t)
	target := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Unclaimed", Topic: "next", IssueType: "task", Priority: 0})

	before := h.issueDetail(target.ID)
	text := h.runNextText()
	after := h.issueDetail(target.ID)

	if !strings.Contains(text, target.ID) {
		t.Fatalf("next output = %q, want it to serve %q — this test asserts nothing if the row was never picked", text, target.ID)
	}
	if after.State() != before.State() {
		t.Fatalf("status moved %v → %v across `lit next`; only `lit start` may move it", before.State(), after.State())
	}
	if after.Assignee != before.Assignee {
		t.Fatalf("assignee moved %q → %q across `lit next`; only `lit start` may set it", before.Assignee, after.Assignee)
	}
	if after.State() != model.StateOpen || after.Assignee != "" {
		t.Fatalf("ticket is %v assigned to %q after `lit next`, want it still open and unassigned", after.State(), after.Assignee)
	}

	// The output must not claim otherwise either: a reader who believes the
	// perfect tense skips `lit start` and works unclaimed, which is the harm the
	// state assertions above prove has not happened but the line could still
	// report (links-next-output-5aee).
	for _, lie := range []string{"starting " + target.ID, "claims " + target.ID, "taking over " + target.ID, "resuming " + target.ID} {
		if strings.Contains(text, lie) {
			t.Fatalf("next output = %q contains %q — it reports a side effect this command does not have", text, lie)
		}
	}
	if !strings.Contains(text, "run `lit start "+target.ID+"`") {
		t.Fatalf("next output = %q, want it to name the command that would actually claim %q", text, target.ID)
	}
}
