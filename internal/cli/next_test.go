package cli

import (
	"bytes"
	"io"
	"slices"
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
	gathered, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
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
	return routeNext(gathered.rows, gathered.details, gathered.epics, cc.standings, evidenceIn(h.t, h.ctx, h.ap), cc.self, gathered.scope, storage.IssueCriteria{})
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
// Inside runNextRow nothing could reach it but a full routing run; out here,
// the totality the comment above asserts is directly testable by
// TestServedRowIsTotalOverTheSealedSum.
// [LAW:one-source-of-truth]
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
	case ServedPastExhaustion:
		return served.Row, true
	case ServedPastNarrowing:
		return served.Row, true
	}
	return annotation.AnnotatedIssue{}, false
}

// Go type switches are not exhaustive, so nothing in the compiler holds
// servedRow level with the sealed sum — the renderer's default panics, but this
// helper just reports "routing declined to serve", which is the WRONG answer
// rather than a loud one. Asserted over every variant the sum has, so a new
// outcome that carries a row fails here by name instead of turning a served row
// into a confusing failure far from its cause.
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
		{"the global pool", ServedFromNewLane{poolPick{Row: row}}, true},
		{"the on-path dependency", ServedFromDependency{Row: row, Gates: "test-gated-1", Blocker: row.ID}, true},
		{"past an exhausted scope", ServedPastExhaustion{poolPick: poolPick{Row: row}, Exhaustion: Exhausted{Epics: []string{"test-epic-1"}}}, true},
		{"past a scope the filter narrowed", ServedPastNarrowing{poolPick: poolPick{Row: row}, Narrowing: Narrowed{Hidden: annotation.AnnotatedIssue{Issue: model.Issue{ID: "test-hidden-1"}}}}, true},
		{"exhausted carries none", Exhausted{}, false},
		// Hidden is the ticket the filter kept from being served, so it must
		// not read as the row this outcome hands over.
		{"narrowed carries none", Narrowed{Hidden: row}, false},
		{"no work carries none", NoWork{}, false},
	}
	// The sum is sealed at ten cases (isNextOutcome, next_route.go). Counting
	// them here is what makes an added variant fail loudly at this table instead
	// of passing unnoticed because nobody thought to cover it.
	if len(cases) != 10 {
		t.Fatalf("table covers %d outcomes, want all 10 NextOutcome variants — add the new one", len(cases))
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
// leaf in the same rank order `lit ready` produces.
func TestRunNextReturnsTopReadyLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First leaf", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: "first"})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second leaf", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "second"})

	got := h.runNextRow()
	if got.ID != first.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order)", got.ID, first.ID)
	}
}

// An in-progress leaf in a lane ANOTHER checkout holds is not a workable start;
// `lit next` routes around it and returns the next open one. The claim is what
// makes it untouchable — its holder is working it right now — so this is
// stated against a foreign holder rather than against the state alone.
func TestRunNextRoutesAroundAnInProgressLeafHeldElsewhere(t *testing.T) {
	h := newReadyTestHarness(t)
	// Distinct lanes: the foreign hold, not lane order, is what keeps the open
	// leaf from being served first.
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: "theirs"})
	openLeaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "open"})
	h.asCheckout("otherstream01")
	h.applyAction(inProgress.ID, model.Start{Assignee: "tester"}, "")

	got := h.runNextRow()
	if got.ID != openLeaf.ID {
		t.Fatalf("next.ID = %q, want %q (a lane held elsewhere is routed around)", got.ID, openLeaf.ID)
	}
}

// The other half is the opposite answer on the same shape: work in flight in a
// lane THIS checkout holds is handed back to be resumed, not skipped in favour
// of a lower-ranked open leaf.
func TestRunNextResumesOwnWorkInFlight(t *testing.T) {
	// The session that started it is the session asking, spelled out rather
	// than inherited: this test's subject is the sentence that says the work is
	// YOURS, and an ambient CLAUDE_CODE_SESSION_ID would decide that off-stage
	// — green on a CI runner that sets none, and a different sentence on any
	// developer machine inside an agent session.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-mine")
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: "mine"})
	lowerRanked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "other"})
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

// Two agent sessions in ONE checkout. Session sess-peer starts a ticket;
// session sess-mine runs `lit next` and must not be told "already in progress
// in a lane you hold — continue where you left off", which would be false about
// the only thing an agent acts on — whose work it is.
//
// What is NOT asserted is as deliberate as what is. The row still comes back:
// the lane really does belong to this checkout, lanes are keyed on the checkout
// on purpose (design-docs/work-claims.md rejects session-bound claims by name),
// and routing past it would strand the fresh session that inherits a dead
// predecessor's work.
func TestRunNextNamesThePeerSessionWorkingThisCheckoutsLane(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-peer")
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Peer's work, in flight", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.applyAction(theirs.ID, model.Start{Assignee: "claude_sess-peer"}, "")

	// Same checkout, same stream token, different session — asCheckout is
	// deliberately not used, because switching streams would make this the
	// foreign-lane case.
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

// The operator with no session of their own, who is nonetheless somebody, names
// themselves once and is the same person to the write and to the read: `lit
// start --by bob` assigns bob, and `lit next --by bob` reports that ticket as
// bob's own rather than warning him off somebody else's. One flag on both
// commands, driven through the CLI, so a start that read its assignee from
// anywhere but --by fails here.
func TestStartAndNextNameTheSessionlessOperatorWithOneFlag(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	repo, _ := initBootstrapTestRepo(t)
	epic := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "Home epic", "--topic", "next", "--type", "epic"))
	mine := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "Bob's ticket", "--topic", "next", "--type", "task", "--parent", epic))

	runCLIInDir(t, repo, "start", mine, "--by", "bob")
	// An unassigned ticket also reads as the reader's own, so the start event is
	// asserted first: without it a start that dropped the name would pass. One
	// event names bob twice, as the actor and as the assignee it wrote.
	history := runCLIInDir(t, repo, "history", mine)
	if !strings.Contains(history, "[bob @") || !strings.Contains(history, "assignee: - → bob") {
		t.Fatalf("history after start --by bob = %q, want the start recorded by bob, assigning bob", history)
	}

	text := runCLIInDir(t, repo, "next", "--by", "bob")
	if !strings.Contains(text, mine+" is already in progress in a lane you hold — continue where you left off") {
		t.Fatalf("next --by bob after start --by bob = %q, want bob's own work reported as his", text)
	}
	if strings.Contains(text, "not to you") {
		t.Fatalf("next --by bob after start --by bob = %q, want no warning about the reader's own ticket", text)
	}
}

// `lit start --assignee` was the session-less identity flag before --by
// replaced it, so a caller still passing it is pointed at --by rather than
// told only that the flag is unknown. A command that takes no identity has
// nothing to point at, so there it stays an ordinary unknown flag.
func TestStartAssigneePointsToBy(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	repo, _ := initBootstrapTestRepo(t)
	epic := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "Home epic", "--topic", "next", "--type", "epic"))
	ticket := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "Bob's ticket", "--topic", "next", "--type", "task", "--parent", epic))

	_, err := runCLIInDirErr(t, repo, "start", ticket, "--assignee", "bob")
	if err == nil || ExitCode(err) != ExitValidation || !strings.Contains(err.Error(), "--by <name>") {
		t.Fatalf("start --assignee bob = %v (exit %d), want exit %d naming --by", err, ExitCode(err), ExitValidation)
	}
	_, err = runCLIInDirErr(t, repo, "show", ticket, "--assignee", "bob")
	if err == nil || ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "unknown flag: --assignee") {
		t.Fatalf("show --assignee bob = %v (exit %d), want exit %d unknown flag", err, ExitCode(err), ExitUsage)
	}
}

// --by is how a session-less operator names themselves, so --help has to say
// so: on start, where it becomes the assignee, on next, where it is the reader,
// and on a mutating command, where it is the actor.
func TestByFlagIsDocumentedInHelp(t *testing.T) {
	initBootstrapTestRepo(t)
	for _, command := range [][]string{{"start"}, {"next"}, {"label", "add"}} {
		page := render(t, append(command, "--help"))
		if !strings.Contains(page, "--by string") || !strings.Contains(page, byUsage) {
			t.Errorf("`lit %s --help` = %q, want --by listed with its usage %q", strings.Join(command, " "), page, byUsage)
		}
	}
}

// A lane we hold whose next ticket is startable serves it with NO announcement:
// no claim is established, because we already hold the lane. This is the
// routing case with the quietest output and therefore the one most easily
// broken without anyone noticing.
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

// A ready ticket two hops down is on the path: our epic's work waits on a
// blocker outside it, the blocker waits on one more, and that one is ready. It
// is served as work toward our epic, naming the blocker between so `lit show`
// can confirm the chain, rather than reported as a stuck epic beside an
// invitation to file a ticket that already exists.
func TestRunNextServesAReadyTicketBehindABlockerThatIsNotStartable(t *testing.T) {
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Other epic", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready elsewhere", Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside epic", Topic: "next", IssueType: "epic", Priority: 0})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside dependency", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "blocker"})
	blockersBlocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Deeper still", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "deeper"})
	h.addDependency(gated.ID, blocker.ID)
	h.addDependencyWithinEpic(blocker.ID, blockersBlocker.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	outcome := h.runNextOutcome()
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency", outcome, outcome)
	}
	if served.Row.ID != blockersBlocker.ID || served.Blocker != blockersBlocker.ID || served.Gates != gated.ID || !slices.Equal(served.Via, []hop{{Prerequisite: blocker.ID, Through: blocker.ID}}) {
		t.Fatalf("served %s blocking %s, gating %s via %v; want %s blocking itself, gating %s via [%s]", served.Row.ID, served.Blocker, served.Gates, served.Via, blockersBlocker.ID, gated.ID, blocker.ID)
	}
	if text, want := h.runNextText(), "(gates "+gated.ID+", which is on your path, through "+blocker.ID+")"; !strings.Contains(text, want) {
		t.Fatalf("next output = %q, want it to contain %q", text, want)
	}
}

// A blocker held behind the earlier ticket of its own lane is cleared by that
// ticket, just as by a dependency, so a ready earlier lane-mate is on the path
// and is served — the lane a ticket waits in is as much a prerequisite as an
// edge it declares.
func TestRunNextServesTheEarlierTicketOfABlockersOwnLane(t *testing.T) {
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Other epic", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready elsewhere", Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside epic", Topic: "next", IssueType: "epic", Priority: 0})
	earlier := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Earlier in the blocker's lane", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "shared"})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside dependency", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "shared"})
	h.addDependency(gated.ID, blocker.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	served, ok := h.runNextOutcome().(ServedFromDependency)
	if !ok || served.Row.ID != earlier.ID || served.Gates != gated.ID || !slices.Equal(served.Via, []hop{{Prerequisite: blocker.ID, Through: blocker.ID}}) {
		t.Fatalf("served %+v, want %s gating %s through %s, the ticket its lane holds behind it", served, earlier.ID, gated.ID, blocker.ID)
	}
}

// A chain that passes through an epic passes through a ticket under it, and the
// announcement names both: `lit show` on our row lists the epic, `lit show` on
// the pick lists the ticket, and naming either alone leaves a link the reader
// cannot find.
func TestRunNextNamesTheTicketUnderAnEpicTheChainPassesThrough(t *testing.T) {
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Other epic", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready elsewhere", Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	blockingEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocking epic", Topic: "next", IssueType: "epic", Priority: 0})
	under := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Its only ticket", Topic: "next", IssueType: "task", Priority: 0, ParentID: blockingEpic.ID})
	upstreamEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Upstream epic", Topic: "next", IssueType: "epic", Priority: 0})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready upstream", Topic: "next", IssueType: "task", Priority: 0, ParentID: upstreamEpic.ID})
	h.addDependency(gated.ID, blockingEpic.ID)
	h.addDependency(under.ID, upstream.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	served, ok := h.runNextOutcome().(ServedFromDependency)
	if !ok || served.Row.ID != upstream.ID || !slices.Equal(served.Via, []hop{{Prerequisite: blockingEpic.ID, Through: under.ID}}) {
		t.Fatalf("served %+v, want %s through %s under %s", served, upstream.ID, under.ID, blockingEpic.ID)
	}
	if text, want := h.runNextText(), "(gates "+gated.ID+", which is on your path, through "+under.ID+" under "+blockingEpic.ID+")"; !strings.Contains(text, want) {
		t.Fatalf("next output = %q, want it to contain %q", text, want)
	}
}

// The chain can run through blocks on epics alone: our epic waits on another
// epic, and that epic waits on a ready ticket. Neither block is on a ticket,
// so each link is reached by readiness passing an epic's blockers down to the
// tickets under it — our row inherits the first, and the blocking epic's
// ticket the second. The ready ticket at the end is served as on-path work,
// not passed over for another epic's.
func TestRunNextServesTheReadyBlockerOfAnEpicOurEpicWaitsOn(t *testing.T) {
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Other epic", Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready elsewhere", Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	blockingEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocking epic", Topic: "next", IssueType: "epic", Priority: 0})
	under := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Its only ticket", Topic: "next", IssueType: "task", Priority: 0, ParentID: blockingEpic.ID})
	upstreamEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Upstream epic", Topic: "next", IssueType: "epic", Priority: 0})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready upstream", Topic: "next", IssueType: "task", Priority: 0, ParentID: upstreamEpic.ID})
	h.addDependency(epic.ID, blockingEpic.ID)
	h.addDependency(blockingEpic.ID, upstream.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	outcome := h.runNextOutcome()
	served, ok := outcome.(ServedFromDependency)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromDependency", outcome, outcome)
	}
	if served.Row.ID != upstream.ID || served.Blocker != upstream.ID || served.Gates != gated.ID || !slices.Equal(served.Via, []hop{{Prerequisite: blockingEpic.ID, Through: under.ID}}) {
		t.Fatalf("served %s blocking %s, gating %s via %v; want %s blocking itself, gating %s through %s under %s", served.Row.ID, served.Blocker, served.Gates, served.Via, upstream.ID, gated.ID, under.ID, blockingEpic.ID)
	}
	if text, want := h.runNextText(), "(gates "+gated.ID+", which is on your path, through "+under.ID+" under "+blockingEpic.ID+")"; !strings.Contains(text, want) {
		t.Fatalf("next output = %q, want it to contain %q", text, want)
	}
}

// The walk follows the backlog's order, not distance: the ready ticket two hops
// down the chain of our higher-ranked row is served ahead of one gating our
// lower-ranked row directly, though that one is nearer and itself ranked first.
func TestRunNextServesTheChainOfOurHighestRankedRowFirst(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ranked first", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ranked second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside epic", Topic: "next", IssueType: "epic", Priority: 0})
	near := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Near", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "near"})
	stuck := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Stuck", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "stuck"})
	deep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Deep", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "deep"})
	h.addDependency(first.ID, stuck.ID)
	h.addDependencyWithinEpic(stuck.ID, deep.ID)
	h.addDependency(second.ID, near.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	served, ok := h.runNextOutcome().(ServedFromDependency)
	if !ok || served.Row.ID != deep.ID || served.Gates != first.ID {
		t.Fatalf("served %+v, want %s gating %s — not %s, which gates only the lower-ranked %s", served, deep.ID, first.ID, near.ID, second.ID)
	}
}

// The trap links-next-5sxz names: our epic still has open work, none of it is
// reachable, and another epic has a ready ticket. `next` serves that ticket and
// says why our epic stopped and how to stay in it instead — the agent chooses.
// Asserted through runNext, where the exit path is decided: answered as an
// error, it would be repeated verbatim by every later `next`, and the agent
// would sit in the epic until its claim expired.
//
// The gating dependency is itself blocked, and its own blocker waits on an
// event outside this repository, so nothing along the chain is ours to take:
// that is what forecloses routing steps 1b and 2b and leaves the epic
// exhausted. The diagnostic says how far the epic had come — First is done,
// Second is not — and the walk reaches the end of the chain, so it names
// every link by what it is to this checkout — and offers no route to stay,
// since the chain's only end awaits that event and the link before it clears
// only when it does.
func TestRunNextServesPastAnEpicWhoseWorkIsAllBlocked(t *testing.T) {
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Other epic", Topic: "next", IssueType: "epic", Priority: 1})
	elsewhere := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Ready elsewhere", Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Our epic", Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside epic", Topic: "next", IssueType: "epic", Priority: 0})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Outside dependency", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "blocker"})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Upstream release", Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: "upstream", Labels: []string{ExternalLabel}})
	h.addDependency(gated.ID, blocker.ID)
	h.addDependencyWithinEpic(blocker.ID, upstream.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	outcome := h.runNextOutcome()
	served, ok := outcome.(ServedPastExhaustion)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedPastExhaustion", outcome, outcome)
	}
	if served.Row.ID != elsewhere.ID {
		t.Fatalf("served = %q, want %q — the top ready ticket outside our epic", served.Row.ID, elsewhere.ID)
	}
	if len(served.Exhaustion.Epics) != 1 || served.Exhaustion.Epics[0] != epic.ID {
		t.Fatalf("exhaustion.Epics = %v, want [%s]", served.Exhaustion.Epics, epic.ID)
	}
	if got := served.Exhaustion.Blocked; len(got) != 2 || got[0].ID != blocker.ID || got[1].ID != upstream.ID {
		t.Fatalf("exhaustion.Blocked = %+v, want the gating dependency %s, then its own %s", got, blocker.ID, upstream.ID)
	}

	text := h.runNextText()
	for _, want := range []string{
		"no ready work in epic(s) " + epic.ID + " (1/2 done) — ",
		blocker.ID + " (on your path but not startable right now",
		upstream.ID + " (on your path but waiting on an event outside this repository",
		"\nmove on to the top ready ticket outside it: run `lit start " + elsewhere.ID + "`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("next output = %q, want it to contain %q", text, want)
		}
	}
	if strings.Contains(text, "to stay, file") {
		t.Fatalf("next output = %q, want no route to stay — no ticket filed here clears %s, and %s waits only on it", text, upstream.ID, blocker.ID)
	}
}

// Blocked leaves (open dependency) are skipped just as `lit ready` partitions
// them out of the ready section. With deterministic creation-order ranking the
// blocker (rank 1 in its epic, its own lane, no own dependencies) is
// unambiguously top, so the assertion pins both "dependent skipped" and the
// exact expected pick.
func TestRunNextSkipsBlockedLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: "blocker"})
	dependent := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "dependent"})
	h.addDependencyWithinEpic(dependent.ID, blocker.ID)
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Unblocked third", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "third"})

	got := h.runNextRow()
	if got.ID == dependent.ID {
		t.Fatalf("next.ID = %q (dependent), want a non-blocked leaf", got.ID)
	}
	if got.ID != blocker.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order after skipping blocked dependent)", got.ID, blocker.ID)
	}
}

// A ticket whose resolution is an outside event is not served, and neither is
// the ticket waiting on it; next goes past both to work this repo can do.
func TestRunNextSkipsATicketWaitingOnAnOutsideEvent(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Upstream fix lands", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "upstream", Labels: []string{ExternalLabel}})
	waiting := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Adopt the upstream fix", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "waiting"})
	h.addDependencyWithinEpic(waiting.ID, upstream.ID)
	workable := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable here", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "workable"})

	if got := h.runNextRow(); got.ID != workable.ID {
		t.Fatalf("next.ID = %q, want %q — the outside event %s and its dependent %s are not workable here", got.ID, workable.ID, upstream.ID, waiting.ID)
	}
}

// `lit next` exposes the standard narrowing knobs so "the next workable bug"
// is expressible; the filter runs in the shared pipeline, so next answers the
// same narrowed question ready/backlog/queue would.
func TestRunNextTypeFilterPicksMatchingLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	task := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Top-ranked task", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: "task"})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Lower-ranked bug", Topic: "next", IssueType: "bug", Priority: 0, ParentID: epic.ID, Lane: "bug"})

	text := h.runNextText("--type", "bug")
	if !strings.Contains(text, bug.ID) {
		t.Fatalf("next --type bug output = %q, want %q picked", text, bug.ID)
	}
	if strings.Contains(text, task.ID) {
		t.Fatalf("next --type bug output = %q, want %q filtered out despite outranking the bug", text, task.ID)
	}
}

// A held lane whose ready ticket is a task, asked for bugs: the bug elsewhere
// is served beneath a line naming the task the filter hid, and nothing calls
// the epic out of work. Driven through runNext, since the defect was in what
// the command handed routing.
func TestRunNextUnderAFilterServesPastTheTicketItHid(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "B.1", Topic: "next", IssueType: "bug", Priority: 1, ParentID: epicB.ID})
	h.applyAction(first.ID, model.Start{Assignee: "tester"}, "")
	h.applyAction(first.ID, model.Done{}, "")

	text := h.runNextText("--type", "bug")
	if strings.Contains(text, "no ready work in") {
		t.Fatalf("next --type bug = %q, want the epic not called exhausted — %s is ready in it", text, second.ID)
	}
	want := "`lit next` without --type, --status, --labels or --assignee hands you " + second.ID + ", which this run's filter leaves out\nor take the top ready ticket the filter selects, outside the work you hold: run `lit start " + bug.ID + "`"
	if !strings.Contains(text, want) {
		t.Fatalf("next --type bug = %q, want it to contain %q", text, want)
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

// `lit next --status in_progress` is a documented flag combination. It is the
// question an agent asks after a crash or a context reset — "what was I already
// on?" — so these two tests pin both answers it can get, and the wording that
// separates them.
//
// Driven through runNext rather than routeNext: only the real command proves
// --status reaches the gather that makes the in_progress row available to route
// at all.
func TestRunNextStatusInProgressResumesOurOwnWorkInFlight(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-mine")
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.applyAction(mine.ID, model.Start{Assignee: "claude_sess-mine"}, "")

	text := h.runNextText("--status", "in_progress")
	if !strings.Contains(text, mine.ID) {
		t.Fatalf("next --status in_progress = %q, want it to hand back %q — narrowing to in_progress is how an agent asks what it already holds", text, mine.ID)
	}
	if !strings.Contains(text, mine.ID+" is already in progress in a lane you hold") {
		t.Fatalf("next --status in_progress = %q, want it reported as a resumption of %q and not a fresh start", text, mine.ID)
	}
}

// When the only in_progress rows are held fresh by other checkouts, every one
// verdicts routeAround and the walk ends at NoWork. That must not print a bare
// "no ready work" — byte-identical to an empty backlog, telling an agent asking
// what it was on that its work is gone.
func TestRunNextStatusInProgressNamesForeignHeldWorkRatherThanReadingEmpty(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs, in flight", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
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
	// off the exit code a looping caller reads.
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
// telling them apart never requires parsing the English.
func TestRunNextErrorsWhenNoReadyWork(t *testing.T) {
	h := newReadyTestHarness(t)
	err := h.runNextErr()
	if err == nil {
		t.Fatal("runNext() error = nil, want non-nil for empty ready set")
	}
	// Exactly, not merely contains: NoWork appends a clause naming the rows
	// the pool walk went past, and an empty backlog has none. A contains-check
	// would pass on a message that had grown a clause here.
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

// TestRenderNextOutcomeTerminalOutcomesKeepTheirType pins that
// renderNextOutcome renders the router's terminal outcomes as TYPED errors,
// keeping the discriminator routeNext established to keep the exhaustion case
// distinguishable. Both sinks dispatch by type, so an untyped error falls
// through to "command_failed", whose remediation tells the agent to retry an
// answer that is deterministic and then to run `lit doctor` against a perfectly
// healthy workspace — two dead ends, attached to a message saying the situation
// calls for a deliberate act.
//
// The test drives the real seam rather than the error types in isolation:
// asserting commandErrorReason(Exhausted{}) alone would still pass if
// renderNextOutcome wrapped the outcome in errors.New.
func TestRenderNextOutcomeTerminalOutcomesKeepTheirType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		outcome    NextOutcome
		wantReason commandReason
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
			wantAct:    "finish or hand off what you already hold",
		},
		{
			name:       "narrowed",
			outcome:    Narrowed{Hidden: annotation.AnnotatedIssue{Issue: model.Issue{ID: "links-hidden-abcd"}}},
			wantReason: "scope_narrowed",
			wantAct:    "without the filter",
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
			outcome: NoWork{
				Unreachable: []rowReach{{ID: "links-gate-onpath", reading: reading{Kind: reachNotReady}}},
				OffPath:     []string{"links-other-offpath"},
			},
			wantReason: "no_ready_work",
			wantAct:    "lit next --all",
			// withheldByScope names the off-path row without ever running
			// capacityFor on it, so the remediation holds no reading of its
			// capacity and may pass no verdict on it — in any wording, which
			// is why the pin is the bare word and not one sentence's phrasing.
			// NoWork.Error() already declines the same verdict; a remediation
			// that makes it contradicts the message it prints under.
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

// `--continue` is retired: claim routing subsumes the epic-affinity bias
// unconditionally (routeNext's ServedFromEpicLane step).
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
// The two assertions are not one: it must name the holder, and it must not say
// "a lane you hold" — a sentence could easily acquire the name while keeping
// the claim.
func TestRenderNextOutcomeNamesTheOtherSessionWorkingOurLane(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs, in flight", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.transition(inFlight.ID, model.Start{Assignee: "claude_sess-peer"})

	rows, _, _ := h.gather()
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
// warning. A comment never touches the issue row, and neither does a commit or
// a push, so a session that holds a branch all day and says so in comments
// crosses the orphan threshold while still working.
func TestRenderNextOutcomeStillNamesTheHolderOfAQuietTicket(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	quiet := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Held, and silent about it", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.transition(quiet.ID, model.Start{Assignee: "claude_sess-peer"})
	h.backdateUpdatedAt(quiet.ID, 7*time.Hour)

	rows, _, _ := h.gather()
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
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	agents := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "An agent started this", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.transition(agents.ID, model.Start{Assignee: "claude_sess-agent"})

	rows, _, _ := h.gather()
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
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	unassigned := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Started by nobody in particular", Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.transition(unassigned.ID, model.Start{Assignee: ""})

	rows, _, _ := h.gather()
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

	rows, details, _ := h.gather()
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
		{"abandoned work says it is in flight and unheld before the same claim advice", ServedFromNewLane{poolPick{Row: inFlightRow, Lane: inFlightLane}},
			inFlight.ID + " is in progress and nobody holds it — run `lit start " + inFlight.ID + "` to claim lane a2 of epic " + epicA.ID},
		// Step 2 serves abandoned in-flight rows too, so the epic's next lane
		// can carry one: the one place the state-dependent sentence and the
		// fixed suffix are concatenated. Pinned whole, because a product left
		// partly covered is where a tautology survives.
		{"the epic's next lane serves abandoned work, qualifier and all", ServedFromEpicLane{Row: inFlightRow, Lane: inFlightLane},
			inFlight.ID + " is in progress and nobody holds it — run `lit start " + inFlight.ID + "` to claim lane a2 of epic " + epicA.ID + " (a second lane of an epic you already hold a lane in)"},
		// Step 1b, both states. Its qualifier concatenates onto the
		// state-dependent sentence exactly as step 2's does, so the product
		// needs both cells: a ready row and an abandoned one. This is the pick
		// an agent is least likely to predict, and these two cells pin that it
		// never renders the global pool's line verbatim.
		{"the on-path dependency names the row it unblocks", ServedFromDependency{Row: freshRow, Lane: freshLane, Gates: inFlight.ID, Blocker: fresh.ID},
			"run `lit start " + fresh.ID + "` to claim lane a1 of epic " + epicA.ID + " (gates " + inFlight.ID + ", which is on your path)"},
		{"an abandoned dependency is served and still names what it unblocks", ServedFromDependency{Row: inFlightRow, Lane: inFlightLane, Gates: fresh.ID, Blocker: inFlight.ID},
			inFlight.ID + " is in progress and nobody holds it — run `lit start " + inFlight.ID + "` to claim lane a2 of epic " + epicA.ID + " (gates " + fresh.ID + ", which is on your path)"},
		// Work under an epic that blocks has no edge of its own to the row it
		// frees, so the line names the epic between them.
		{"work under a blocking epic names the epic that gates", ServedFromDependency{Row: freshRow, Lane: freshLane, Gates: "test-gated", Blocker: epicA.ID},
			"run `lit start " + fresh.ID + "` to claim lane a1 of epic " + epicA.ID + " (it is in epic " + epicA.ID + ", which gates test-gated on your path)"},
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
// path is under it, and over the pick that has the most to lie about: an
// unclaimed ticket in a lane nobody holds.
// The wording assertions elsewhere in this file all become vacuous if the
// command ever does start claiming, and this is what would still fail.
func TestRunNextClaimsNothingAndStartsNothing(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "next", IssueType: "epic", Priority: 1})
	target := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Unclaimed", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})

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
	// report.
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
// tellable apart BY THE OUTPUT ALONE — it is not implied by either line being
// correct in isolation. Asserted on ONE row deliberately: holding the row,
// the lane and the standings fixed leaves the routing step as the only variable,
// so a future edit that made the qualifier unconditional (or dropped it) could
// not pass this by changing the fixture.
func TestDependencyPickIsDistinguishableFromThePool(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	fresh := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	blocked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})

	rows, details, _ := h.gather()
	cc := claimContext{self: selfAttribution}
	// Every outcome here is announced by startAdvice, whose sentences carry no
	// identity, so the reader is passed only because renderNextOutcome's
	// signature requires one and is read by no branch under test.
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
	pool := render(ServedFromNewLane{poolPick{Row: row, Lane: lane}})
	dep := render(ServedFromDependency{Row: row, Lane: lane, Gates: blocked.ID, Blocker: row.ID})

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
