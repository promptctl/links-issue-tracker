package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

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
	if row, ok := servedRow(outcome); ok {
		return row
	}
	h.t.Fatalf("routeNext = %#v (%T), want an outcome carrying a served row", outcome, outcome)
	return annotation.AnnotatedIssue{}
}

// servedRow is the single place the harness decides which outcomes carry a row.
// It was inlined in runNextRow, where nothing could reach it but a full routing
// run — so the totality the comment above asserts was never exercised, and this
// switch silently fell a variant behind the renderer's when step 1b got its own
// outcome (links-next-output-4hor). Lifted out, the claim is directly testable
// by TestServedRowIsTotalOverTheSealedSum. [LAW:one-source-of-truth]
func servedRow(outcome NextOutcome) (annotation.AnnotatedIssue, bool) {
	switch served := outcome.(type) {
	case ServedFromClaim:
		return served.Row, true
	case ResumedOwnWork:
		return served.Row, true
	case ServedFromEpicLane:
		return served.Row, true
	case ServedFromNewLane:
		return served.Row, true
	case ServedFromDependency:
		return served.Row, true
	}
	return annotation.AnnotatedIssue{}, false
}

// Go type switches are not exhaustive, so nothing in the compiler holds
// servedRow level with the sealed sum — the renderer's default panics, but this
// helper just reports "routing declined to serve", which is the WRONG answer
// rather than a loud one. Asserted over every variant the sum has, so a new
// outcome that carries a row fails here by name instead of turning a served row
// into a confusing failure far from its cause (links-next-output-4hor).
func TestServedRowIsTotalOverTheSealedSum(t *testing.T) {
	row := annotation.AnnotatedIssue{Issue: model.Issue{ID: "test-served-1"}}
	cases := []struct {
		name    string
		outcome NextOutcome
		served  bool
	}{
		{"the claimed lane", ServedFromClaim{Row: row}, true},
		{"own work resumed", ResumedOwnWork{Row: row}, true},
		{"the epic's next lane", ServedFromEpicLane{Row: row}, true},
		{"the global pool", ServedFromNewLane{Row: row}, true},
		{"the on-path dependency", ServedFromDependency{Row: row, Gates: "test-gated-1"}, true},
		{"exhausted carries none", Exhausted{}, false},
		{"no work carries none", NoWork{}, false},
	}
	// The sum is sealed at seven cases (isNextOutcome, next_route.go). Counting
	// them here is what makes an added variant fail loudly at this table instead
	// of passing unnoticed because nobody thought to cover it.
	if len(cases) != 7 {
		t.Fatalf("table covers %d outcomes, want all 7 NextOutcome variants — add the new one", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := servedRow(tc.outcome)
			if ok != tc.served {
				t.Fatalf("servedRow(%T) served = %v, want %v", tc.outcome, ok, tc.served)
			}
			if tc.served && got.ID != row.ID {
				t.Fatalf("servedRow(%T) = %q, want the row it carries (%q)", tc.outcome, got.ID, row.ID)
			}
			if !tc.served && got.ID != "" {
				t.Fatalf("servedRow(%T) = %q, want the zero row — a terminal outcome serves nothing", tc.outcome, got.ID)
			}
		})
	}
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
	// The session that started it is the session asking, spelled out rather
	// than inherited: this test's subject is the sentence that says the work is
	// YOURS, and an ambient CLAUDE_CODE_SESSION_ID would decide that off-stage
	// — green on a CI runner that sets none, and a different sentence on any
	// developer machine inside an agent session.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-mine")
	h := newReadyTestHarness(t)
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	lowerRanked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable", Topic: "next", IssueType: "task", Priority: 0})
	h.applyAction(inProgress.ID, model.Start{Assignee: "claude_sess-mine"}, "")

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

// The defect itself, end to end: two agent sessions in ONE checkout. Session
// sess-peer starts a ticket; session sess-mine runs `lit next` and is told
// "already in progress in a lane you hold — continue where you left off",
// which is false about the only thing an agent acts on — whose work it is.
// Three sessions believed it, and each time disproving it took a hand check of
// git worktrees and push times, because nothing lit printed disagreed
// (links-routing-t6fa).
//
// What is NOT asserted is as deliberate as what is. The row still comes back:
// the lane really does belong to this checkout, lanes are keyed on the checkout
// on purpose (design-docs/work-claims.md rejects session-bound claims by name),
// and routing past it would strand the fresh session that inherits a dead
// predecessor's work — the case this same sentence serves correctly. Only the
// wording was ever wrong, so only the wording changes.
func TestRunNextNamesThePeerSessionWorkingThisCheckoutsLane(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-peer")
	h := newReadyTestHarness(t)
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Peer's work, in flight", Topic: "next", IssueType: "task", Priority: 1})
	h.applyAction(theirs.ID, model.Start{Assignee: "claude_sess-peer"}, "")

	// Same checkout, same stream token, different session — asCheckout is
	// deliberately not used, because switching streams would make this the
	// already-solved foreign-lane case instead of this ticket's.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-mine")
	text := h.runNextText()

	if !strings.Contains(text, theirs.ID) {
		t.Fatalf("next = %q, want the lane's work in flight still handed back (%s)", text, theirs.ID)
	}
	if !strings.Contains(text, "claude_sess-peer") {
		t.Fatalf("next = %q, want it to name claude_sess-peer as the session holding %s", text, theirs.ID)
	}
	if strings.Contains(text, "continue where you left off") {
		t.Fatalf("next = %q, want it not to tell this session it was the one working %s", text, theirs.ID)
	}
}

// The reader with no session of their own, who is nonetheless somebody. `lit
// start abc --assignee bob` with no CLAUDE_CODE_SESSION_ID writes `bob` on the
// row, and `--assignee`'s own help calls itself the fallback for exactly that
// case. Reading the reader from the env alone would then resolve the two halves
// of one comparison by two different rules, and tell bob that the ticket he
// assigned himself belongs to somebody else — with nothing on this command able
// to say otherwise, since `next --assignee` narrows the view and must not double
// as an identity. The hidden `--by` every mutating command carries answers it
// here too.
func TestRunNextTreatsTheByFallbackAsTheReadersIdentity(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	h := newReadyTestHarness(t)
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Assigned to me by flag", Topic: "next", IssueType: "task", Priority: 1})
	h.applyAction(mine.ID, model.Start{Assignee: "bob"}, "")

	text := h.runNextText("--by", "bob")
	if !strings.Contains(text, mine.ID+" is already in progress in a lane you hold — continue where you left off") {
		t.Fatalf("next --by bob = %q, want bob's own work reported as his", text)
	}
	if strings.Contains(text, "not to you") {
		t.Fatalf("next --by bob = %q, want no warning about the reader's own assignee", text)
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
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-mine")
	h := newReadyTestHarness(t)
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	h.applyAction(mine.ID, model.Start{Assignee: "claude_sess-mine"}, "")

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
		// wantAbsent are claims the remediation may not make about this
		// outcome's rows. Checked as a list so every case runs the same loop
		// over whatever it forbids. [LAW:dataflow-not-control-flow]
		wantAbsent []string
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
		{
			// The focus scope withheld every startable row, so the message
			// says the backlog is NOT empty — the situation in which offering
			// `lit new` is the remediation-contradicts-message defect. Both
			// populations are present because the real outcome carries both:
			// an on-path row step 4 walked and rejected, and an off-path row
			// it never examined.
			name: "no work withheld by focus scope",
			outcome: NoWork{Unreachable: []rowReach{
				{ID: "links-gate-onpath", Kind: reachNotReady},
				{ID: "links-other-offpath", Kind: reachOffFocusPath},
			}},
			wantReason: "no_ready_work",
			wantAct:    "lit next --all",
			// withheldByScope stamps the off-path row without ever running
			// capacityFor on it, so the remediation holds no reading of its
			// capacity and may pass no verdict on it — in any wording, which
			// is why the pin is the bare word and not one sentence's phrasing.
			// NoWork.Error() already declines the same verdict; a remediation
			// that makes it contradicts the message it prints under
			// (links-cli-cpou).
			wantAbsent: []string{"startable"},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			occasion, err := renderNextOutcome(io.Discard, tc.outcome, nil, claimContext{}, "")
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
			// Asserted against the remediation alone rather than all of stderr:
			// the withheld case's per-row note already names `lit next --all`
			// in the message body, so a stderr-wide check would stay green with
			// the remediation silent — the exact gap this case exists to close.
			rem := commandErrorRemediation(commandErrorReason(err))
			if !strings.Contains(rem, tc.wantAct) {
				t.Fatalf("remediation does not name the deliberate act %q: %q", tc.wantAct, rem)
			}
			for _, claim := range tc.wantAbsent {
				if strings.Contains(rem, claim) {
					t.Fatalf("remediation claims %q over rows whose capacity was never read: %q", claim, rem)
				}
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

// The same outcome, the other sentence: what `next` prints when the ticket in
// flight in this checkout's lane carries a DIFFERENT session's name. Asserted
// as bytes beside the table above, and separately from it, because it is the
// one line whose wording depends on a value the table holds empty — the
// claimContext's acting identity.
//
// The two assertions are not one. That it names the holder is the fix; that it
// has stopped saying "a lane you hold" is the defect, and a sentence could
// easily acquire the name while keeping the claim (links-routing-t6fa).
func TestRenderNextOutcomeNamesTheOtherSessionWorkingOurLane(t *testing.T) {
	h := newReadyTestHarness(t)
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs, in flight", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(inFlight.ID, model.Start{Assignee: "claude_sess-peer"})

	rows, _ := h.gather()
	inFlightRow := rowByID(t, rows, inFlight.ID)
	cc := claimContext{self: selfAttribution}
	const reader = "claude_sess-mine"

	var out bytes.Buffer
	if _, err := renderNextOutcome(&out, ResumedOwnWork{Row: inFlightRow}, map[string]storage.IssueRelations{}, cc, reader); err != nil {
		t.Fatalf("renderNextOutcome() error = %v", err)
	}
	text := out.String()
	want := inFlight.ID + " is in progress and assigned to claude_sess-peer, not to you — check that they have stopped before you continue it, or take other work from `lit backlog`"
	if !strings.Contains(text, want) {
		t.Fatalf("render = %q, want it to contain %q", text, want)
	}
	if strings.Contains(text, "a lane you hold") {
		t.Fatalf("render = %q, want it NOT to tell a session the work is its own", text)
	}
}

// Quiet is not proof the holder stopped, which is why no clock guards the
// warning. An earlier fix suppressed it on the orphan annotation — in flight
// with no update inside the threshold — on the premise that a session actively
// working a ticket keeps it moving. lit does not enforce that premise: a comment
// never touches the issue row, and neither does a commit or a push, so a session
// that holds a branch all day and says so in comments crosses the threshold
// while still working. Suppressing there restores the original defect on a
// timer, so the sentence survives the clock.
func TestRenderNextOutcomeStillNamesTheHolderOfAQuietTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	quiet := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Held, and silent about it", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(quiet.ID, model.Start{Assignee: "claude_sess-peer"})
	h.backdateUpdatedAt(quiet.ID, 7*time.Hour)

	rows, _ := h.gather()
	cc := claimContext{self: selfAttribution}
	const reader = "claude_sess-mine"

	var out bytes.Buffer
	if _, err := renderNextOutcome(&out, ResumedOwnWork{Row: rowByID(t, rows, quiet.ID)}, map[string]storage.IssueRelations{}, cc, reader); err != nil {
		t.Fatalf("renderNextOutcome() error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "claude_sess-peer") {
		t.Fatalf("render = %q, want the holder named even on a row that has gone quiet", text)
	}
	if strings.Contains(text, "continue where you left off") {
		t.Fatalf("render = %q, want no claim that this session was the one working it", text)
	}
}

// A reader that resolved no identity is not the ticket's holder either. Nobody's
// name is the empty string, so a ticket assigned to anyone at all is assigned to
// somebody other than a plain shell — the case where a person runs `lit next` in
// a checkout an agent session has work in flight in.
func TestRenderNextOutcomeNamesTheHolderToAnUnidentifiedReader(t *testing.T) {
	h := newReadyTestHarness(t)
	agents := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "An agent started this", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(agents.ID, model.Start{Assignee: "claude_sess-agent"})

	rows, _ := h.gather()
	cc := claimContext{self: selfAttribution}
	// The whole point: this reader resolved no identity at all.
	const reader = ""

	var out bytes.Buffer
	if _, err := renderNextOutcome(&out, ResumedOwnWork{Row: rowByID(t, rows, agents.ID)}, map[string]storage.IssueRelations{}, cc, reader); err != nil {
		t.Fatalf("renderNextOutcome() error = %v", err)
	}
	if text := out.String(); !strings.Contains(text, "claude_sess-agent") {
		t.Fatalf("render = %q, want the holder named to a reader who resolved no identity of their own", text)
	}
}

// The other half of the minted-both-halves rule, and the half a mutation proved
// nothing else covers: a ticket in flight with NO assignee, read by a session
// that has one. An unassigned in-progress ticket is ordinary — a checkout
// driving no agent session resolves no identity to write there — so an empty
// assignee names nobody, and there is nothing for it to contradict. Compare
// relationOf, which refuses to read a zero attribution as a match for the same
// reason: absence is not an identity.
func TestRenderNextOutcomeSaysNothingAboutAnUnassignedTicketInFlight(t *testing.T) {
	h := newReadyTestHarness(t)
	unassigned := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Started by nobody in particular", Topic: "next", IssueType: "task", Priority: 1})
	h.transition(unassigned.ID, model.Start{Assignee: ""})

	rows, _ := h.gather()
	cc := claimContext{self: selfAttribution}
	const reader = "claude_sess-mine"

	var out bytes.Buffer
	if _, err := renderNextOutcome(&out, ResumedOwnWork{Row: rowByID(t, rows, unassigned.ID)}, map[string]storage.IssueRelations{}, cc, reader); err != nil {
		t.Fatalf("renderNextOutcome() error = %v", err)
	}
	if text := out.String(); !strings.Contains(text, unassigned.ID+" is already in progress in a lane you hold — continue where you left off") {
		t.Fatalf("render = %q, want the lane's own sentence — an empty assignee names no other session", text)
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
	// The reader is the row's assignee, so the ResumedOwnWork case below is
	// genuinely their own work. Any other reader makes it a mismatch and prints
	// the other sentence — pinned in its own test rather than here.
	const reader = "other"
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
		// Step 1b, both verbs. Its qualifier concatenates onto the
		// verb-dependent sentence exactly as step 2's does, so the product needs
		// both cells: a fresh claim and a takeover. This is the pick an agent is
		// least likely to predict, and it printed the global pool's line verbatim
		// until links-next-output-4hor — so what these two cells pin is not only
		// the new clause but that the two picks stopped rendering alike.
		{"the on-path dependency names the row it unblocks", ServedFromDependency{Row: freshRow, Lane: freshLane, Gates: inFlight.ID},
			"run `lit start " + fresh.ID + "` to claim lane a1 of epic " + epicA.ID + " (gates " + inFlight.ID + ", which is in a lane you hold)"},
		{"an abandoned dependency is taken over and still names what it unblocks", ServedFromDependency{Row: inFlightRow, Lane: inFlightLane, Gates: fresh.ID},
			inFlight.ID + " is in progress and abandoned — run `lit start " + inFlight.ID + "` to take over lane a2 of epic " + epicA.ID + " (gates " + fresh.ID + ", which is in a lane you hold)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := renderNextOutcome(&buf, tc.outcome, details, cc, reader); err != nil {
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

// The two picks that both establish a claim in a lane we do not hold must remain
// tellable apart BY THE OUTPUT ALONE — that is the acceptance criterion
// links-next-output-4hor was filed on, and it is not implied by either line
// being correct in isolation. Asserted on ONE row deliberately: holding the row,
// the lane and the standings fixed leaves the routing step as the only variable,
// so a future edit that made the qualifier unconditional (or dropped it) could
// not pass this by changing the fixture.
func TestDependencyPickIsDistinguishableFromThePool(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	fresh := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	blocked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	rows, details := h.gather()
	cc := claimContext{self: selfAttribution}
	// This table is about startAdvice, whose sentences carry no identity;
	// the reader is named only so the one ResumedOwnWork arm has an answer.
	const reader = ""
	row := rowByID(t, rows, fresh.ID)
	lane := laneOf(t, details, row)

	render := func(o NextOutcome) string {
		var buf bytes.Buffer
		if _, err := renderNextOutcome(&buf, o, details, cc, reader); err != nil {
			t.Fatalf("renderNextOutcome(%T) error = %v", o, err)
		}
		return buf.String()
	}
	pool := render(ServedFromNewLane{Row: row, Lane: lane})
	dep := render(ServedFromDependency{Row: row, Lane: lane, Gates: blocked.ID})

	if pool == dep {
		t.Fatalf("the global pool and the on-path dependency render identically as %q — the pick an agent cannot predict is the one that must explain itself", pool)
	}
	if !strings.Contains(dep, blocked.ID) {
		t.Fatalf("dependency pick rendered %q, want it to name the blocked row %q it unblocks", dep, blocked.ID)
	}
	if strings.Contains(pool, "gates") {
		t.Fatalf("global-pool pick rendered %q, want no gating clause — it gates nothing, and a line that claims otherwise is worse than the silence it replaced", pool)
	}
}
