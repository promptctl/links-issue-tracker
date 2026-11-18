package cli

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
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
	return h.runNextOutcomeAsked(workableFilter{})
}

// runNextOutcomeAsked is runNextOutcome for a run under filter flags: the whole
// queue is gathered and the filter is handed to routing, as `lit next` does.
func (h readyTestHarness) runNextOutcomeAsked(filter workableFilter) NextOutcome {
	h.t.Helper()
	asked, err := filter.criteria()
	if err != nil {
		h.t.Fatalf("filter.criteria error = %v", err)
	}
	gathered, err := gatherWorkableQueue(h.ctx, runEnv(h.t), h.ap)
	if err != nil {
		h.t.Fatalf("gatherWorkableQueue error = %v", err)
	}
	cc, err := gatherClaimContext(h.ctx, runEnv(h.t), io.Discard, h.ap)
	if err != nil {
		h.t.Fatalf("gatherClaimContext error = %v", err)
	}
	// The gathered scope, not focusScope{}: this helper stands in for `lit next`
	// itself, and handing routing an empty scope here would quietly answer every
	// focus test from the unfocused path — green, and about a command nobody runs.
	return routeNext(gathered.rows, gathered.details, gathered.epics, cc.standings, evidenceIn(h.t, h.ctx, h.ap), cc.self, gathered.scope, asked)
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
	if row, ok := routedRow(outcome); ok {
		return row
	}
	h.t.Fatalf("routeNext = %#v (%T), want an outcome carrying a served row", outcome, outcome)
	return annotation.AnnotatedIssue{}
}

// sealedSumRow is the row each serving variant in sealedNextOutcomes carries:
// an open task, hydrated so it prints as a row of the backlog does.
var sealedSumRow = func() annotation.AnnotatedIssue {
	issue, err := model.HydrateStatus(model.Issue{ID: "test-served-1", IssueType: model.TypeTask, Labels: model.Texts([]string{"tracked"})}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		panic(err)
	}
	return annotation.AnnotatedIssue{Issue: issue}
}()

// sealedNextOutcomes is one value of every NextOutcome variant, and whether it
// hands a row over. Every test that ranges over the whole sum reads it here,
// so the count TestRoutedRowIsTotalOverTheSealedSum holds it to covers them
// all. [LAW:one-source-of-truth]
var sealedNextOutcomes = []struct {
	name    string
	outcome NextOutcome
	served  bool
}{
	{"the claimed lane", ServedFromClaim{Row: sealedSumRow}, true},
	{"own work resumed", ResumedOwnWork{Row: sealedSumRow}, true},
	{"the epic's next lane", ServedFromEpicLane{Row: sealedSumRow}, true},
	{"the global pool", ServedFromNewLane{poolPick{Row: sealedSumRow}}, true},
	{"the on-path dependency", ServedFromDependency{Row: sealedSumRow, Gates: "test-gated-1", Blocker: sealedSumRow.ID}, true},
	{"past an exhausted scope", ServedPastExhaustion{poolPick: poolPick{Row: sealedSumRow}, Exhaustion: Exhausted{Epics: []model.ID{"test-epic-1"}}}, true},
	{"past a scope the filter narrowed", ServedPastNarrowing{poolPick: poolPick{Row: sealedSumRow}, Narrowing: Narrowed{Hidden: annotation.AnnotatedIssue{Issue: model.Issue{ID: "test-hidden-1"}}}}, true},
	{"exhausted carries none", Exhausted{}, false},
	// Hidden is the ticket the filter kept from being served, so it must
	// not read as the row this outcome hands over.
	{"narrowed carries none", Narrowed{Hidden: sealedSumRow}, false},
	{"no work carries none", NoWork{}, false},
}

// Go type switches are not exhaustive, so nothing in the compiler holds
// routedRow level with the sealed sum, and a variant it misses reads as "no
// ticket" — the WRONG answer rather than a loud one. Asserted over every
// variant the sum has, so a new outcome that carries a row fails here by name
// instead of printing that nothing is ready above a ticket routing served.
func TestRoutedRowIsTotalOverTheSealedSum(t *testing.T) {
	t.Parallel()
	row, cases := sealedSumRow, sealedNextOutcomes
	// The sum is sealed at ten cases (isNextOutcome, next_route.go). Counting
	// them here is what makes an added variant fail loudly at this table instead
	// of passing unnoticed because nobody thought to cover it.
	if len(cases) != 10 {
		t.Fatalf("table covers %d outcomes, want all 10 NextOutcome variants — add the new one", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := routedRow(tc.outcome)
			if ok != tc.served {
				t.Fatalf("routedRow(%T) served = %v, want %v", tc.outcome, ok, tc.served)
			}
			if tc.served && got.ID != row.ID {
				t.Fatalf("routedRow(%T) = %q, want the row it carries (%q)", tc.outcome, got.ID, row.ID)
			}
			if !tc.served && got.ID != "" {
				t.Fatalf("routedRow(%T) = %q, want the zero row — a terminal outcome serves nothing", tc.outcome, got.ID)
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
	return runLeaf(h.t, nextLeaf(), h.ctx, &stdout, h.ap, args)
}

// nextFixture replaces the shipped `next` definition with a body that prints
// <ticket> and nothing else, so a test reads what `lit next` hands its workflow
// rather than the shipped wording around it.
const nextFixture = "---\nid: next\ndescription: fixture\nevents: [next]\n---\n<ticket>"

// useNextFixture installs nextFixture in the project layer of the workspace at
// root, where it overrides the embedded `next` by id.
func useNextFixture(t *testing.T, root string) {
	t.Helper()
	writeCLIWorkflow(t, root, "next.md", nextFixture)
}

// runNextText runs `lit next` under nextFixture and returns what it printed:
// the <ticket> value the run handed its workflow.
func (h readyTestHarness) runNextText(args ...string) string {
	h.t.Helper()
	useNextFixture(h.t, h.ap.Workspace.RootDir)
	var stdout bytes.Buffer
	if err := runLeaf(h.t, nextLeaf(), h.ctx, &stdout, h.ap, args); err != nil {
		h.t.Fatalf("runNext(%v) error = %v", args, err)
	}
	return stdout.String()
}

// `lit next` returns the top of the ready partition: the first open, unblocked
// leaf in the same rank order `lit ready` produces.
func TestRunNextReturnsTopReadyLeaf(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First leaf"), Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: storage.MustParseLane("first")})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second leaf"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("second")})

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
	t.Parallel()
	h := newReadyTestHarness(t)
	// Distinct lanes: the foreign hold, not lane order, is what keeps the open
	// leaf from being served first.
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Already started"), Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: storage.MustParseLane("theirs")})
	openLeaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Workable"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("open")})
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
	t.Parallel()
	// The session that started it is the session asking, spelled out rather
	// than inherited: this test's subject is whether the work is YOURS, and an
	// ambient CLAUDE_CODE_SESSION_ID would decide that off-stage — green on a
	// CI runner that sets none, and a different answer on any developer machine
	// inside an agent session.
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "sess-mine")
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Already started"), Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: storage.MustParseLane("mine")})
	lowerRanked := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Workable"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("other")})
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
	if !strings.Contains(text, inProgress.ID.Raw()) {
		t.Fatalf("next output = %q, want %s handed back as our work in flight", text, inProgress.ID)
	}
	if strings.Contains(text, lowerRanked.ID.Raw()) {
		t.Fatalf("next output = %q, want %q not served while our own work is in flight", text, lowerRanked.ID)
	}
}

// Two agent sessions in ONE checkout. Session sess-peer starts a ticket; session
// sess-mine runs `lit next` and is handed it back as this checkout's own work in
// flight, because the checkout holds the lane, whichever session acts in it.
func TestRunNextResumesTheCheckoutsWorkForAnySessionInIt(t *testing.T) {
	t.Parallel()
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "sess-peer")
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Peer's work, in flight"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.applyAction(theirs.ID, model.Start{Assignee: "claude_sess-peer"}, "")

	// Same checkout, same stream token, different session.
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "sess-mine")
	resumed, ok := h.runNextOutcome().(ResumedOwnWork)
	if !ok || resumed.Row.ID != theirs.ID {
		t.Fatalf("next as sess-mine = %#v, want %s handed back as this checkout's work in flight", resumed, theirs.ID)
	}
}

// The operator with no session of their own names themselves with --by: `lit
// start --by bob` records bob and assigns bob, and `lit next` in the same
// checkout reports that ticket as this checkout's own work in flight.
func TestStartNamesTheSessionlessOperatorWithBy(t *testing.T) {
	t.Parallel()
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "")
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

	useNextFixture(t, repo)
	text := runCLIInDir(t, repo, "next")
	if !strings.Contains(text, mine+"  in_progress") || !strings.Contains(text, "claimed here:") {
		t.Fatalf("next after start --by bob = %q, want the checkout's own work handed back, its lane claimed here", text)
	}
}

// `lit start --assignee` was the session-less identity flag before --by
// replaced it, so a caller still passing it is pointed at --by rather than
// told only that the flag is unknown. A command that takes no identity has
// nothing to point at, so there it stays an ordinary unknown flag.
func TestStartAssigneePointsToBy(t *testing.T) {
	t.Parallel()
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "")
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
// so: on start, where it becomes the assignee, and on a mutating command, where
// it is the actor.
func TestByFlagIsDocumentedInHelp(t *testing.T) {
	t.Parallel()
	repo, _ := initBootstrapTestRepo(t)
	for _, command := range [][]string{{"start"}, {"label", "add"}} {
		page := render(t, repo, append(command, "--help"))
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	nextUp := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})
	h.applyAction(finished.ID, model.Done{}, "finished")

	outcome := h.runNextOutcome()
	served, ok := outcome.(ServedFromClaim)
	if !ok {
		t.Fatalf("routeNext = %#v (%T), want ServedFromClaim", outcome, outcome)
	}
	if served.Row.ID != nextUp.ID {
		t.Fatalf("served.ID = %q, want %q (the next ticket of the lane we hold)", served.Row.ID, nextUp.ID)
	}

	// The harness workspace is no git checkout, so lit cannot address the
	// holder and the claim line names its stream rather than "claimed here".
	text := h.runNextText()
	if !strings.Contains(text, nextUp.ID.Raw()) || !strings.Contains(text, "claimed: ") {
		t.Fatalf("next output = %q, want %q served with its lane's claim line", text, nextUp.ID)
	}
}

// A ready ticket two hops down is on the path: our epic's work waits on a
// blocker outside it, the blocker waits on one more, and that one is ready. It
// is served as work toward our epic, naming the blocker between so `lit show`
// can confirm the chain, rather than reported as a stuck epic beside an
// invitation to file a ticket that already exists.
func TestRunNextServesAReadyTicketBehindABlockerThatIsNotStartable(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Other epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready elsewhere"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside dependency"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("blocker")})
	blockersBlocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Deeper still"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("deeper")})
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
}

// A blocker held behind the earlier ticket of its own lane is cleared by that
// ticket, just as by a dependency, so a ready earlier lane-mate is on the path
// and is served — the lane a ticket waits in is as much a prerequisite as an
// edge it declares.
func TestRunNextServesTheEarlierTicketOfABlockersOwnLane(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Other epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready elsewhere"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	earlier := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Earlier in the blocker's lane"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("shared")})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside dependency"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("shared")})
	h.addDependency(gated.ID, blocker.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	served, ok := h.runNextOutcome().(ServedFromDependency)
	if !ok || served.Row.ID != earlier.ID || served.Gates != gated.ID || !slices.Equal(served.Via, []hop{{Prerequisite: blocker.ID, Through: blocker.ID}}) {
		t.Fatalf("served %+v, want %s gating %s through %s, the ticket its lane holds behind it", served, earlier.ID, gated.ID, blocker.ID)
	}
}

// A chain that passes through an epic passes through a ticket under it, and the
// pick records both: `lit show` on our row lists the epic, `lit show` on the
// pick lists the ticket, and either alone leaves a link the reader cannot find.
func TestRunNextNamesTheTicketUnderAnEpicTheChainPassesThrough(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Other epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready elsewhere"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	blockingEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Blocking epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	under := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Its only ticket"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: blockingEpic.ID})
	upstreamEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready upstream"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: upstreamEpic.ID})
	h.addDependency(gated.ID, blockingEpic.ID)
	h.addDependency(under.ID, upstream.ID)
	h.applyAction(finished.ID, model.Done{}, "finished")

	served, ok := h.runNextOutcome().(ServedFromDependency)
	if !ok || served.Row.ID != upstream.ID || !slices.Equal(served.Via, []hop{{Prerequisite: blockingEpic.ID, Through: under.ID}}) {
		t.Fatalf("served %+v, want %s through %s under %s", served, upstream.ID, under.ID, blockingEpic.ID)
	}
}

// The chain can run through blocks on epics alone: our epic waits on another
// epic, and that epic waits on a ready ticket. Neither block is on a ticket,
// so each link is reached by readiness passing an epic's blockers down to the
// tickets under it — our row inherits the first, and the blocking epic's
// ticket the second. The ready ticket at the end is served as on-path work,
// not passed over for another epic's.
func TestRunNextServesTheReadyBlockerOfAnEpicOurEpicWaitsOn(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Other epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready elsewhere"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	blockingEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Blocking epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	under := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Its only ticket"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: blockingEpic.ID})
	upstreamEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready upstream"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: upstreamEpic.ID})
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
}

// The walk follows the backlog's order, not distance: the ready ticket two hops
// down the chain of our higher-ranked row is served ahead of one gating our
// lower-ranked row directly, though that one is nearer and itself ranked first.
func TestRunNextServesTheChainOfOurHighestRankedRowFirst(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ranked first"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ranked second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	near := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Near"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("near")})
	stuck := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Stuck"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("stuck")})
	deep := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Deep"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("deep")})
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
// reachable, and another epic has a ready ticket. `next` serves that ticket,
// and the outcome records why our epic stopped. Asserted through runNext too:
// a run that handed nothing back would be repeated verbatim by every later
// `next`, and the agent would sit in the epic until its claim expired.
//
// The gating dependency is itself blocked, and its own blocker waits on an
// event outside this repository, so nothing along the chain is ours to take:
// that is what forecloses routing steps 1b and 2b and leaves the epic
// exhausted. The outcome records how far the epic had come — First is done,
// Second is not — and every link of the chain the walk reached.
func TestRunNextServesPastAnEpicWhoseWorkIsAllBlocked(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	otherEpic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Other epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	elsewhere := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Ready elsewhere"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: otherEpic.ID})
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Our epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("First"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Second"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	outside := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 0})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Outside dependency"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("blocker")})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream release"), Topic: "next", IssueType: "task", Priority: 0, ParentID: outside.ID, Lane: storage.MustParseLane("upstream"), Labels: []string{ExternalLabel}})
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

	if got := served.Exhaustion.Progress; got.Done != 1 || got.Total != 2 {
		t.Fatalf("exhaustion.Progress = %+v, want 1 of 2 done", got)
	}
	if text := h.runNextText(); !strings.Contains(text, elsewhere.ID.Raw()) {
		t.Fatalf("next output = %q, want %s handed back", text, elsewhere.ID)
	}
}

// Blocked leaves (open dependency) are skipped just as `lit ready` partitions
// them out of the ready section. With deterministic creation-order ranking the
// blocker (rank 1 in its epic, its own lane, no own dependencies) is
// unambiguously top, so the assertion pins both "dependent skipped" and the
// exact expected pick.
func TestRunNextSkipsBlockedLeaf(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Blocker"), Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: storage.MustParseLane("blocker")})
	dependent := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Dependent"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("dependent")})
	h.addDependencyWithinEpic(dependent.ID, blocker.ID)
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Unblocked third"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("third")})

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
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	upstream := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Upstream fix lands"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("upstream"), Labels: []string{ExternalLabel}})
	waiting := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Adopt the upstream fix"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("waiting")})
	h.addDependencyWithinEpic(waiting.ID, upstream.ID)
	workable := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Workable here"), Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("workable")})

	if got := h.runNextRow(); got.ID != workable.ID {
		t.Fatalf("next.ID = %q, want %q — the outside event %s and its dependent %s are not workable here", got.ID, workable.ID, upstream.ID, waiting.ID)
	}
}

// `lit next` exposes the standard narrowing knobs so "the next workable bug"
// is expressible; the filter runs in the shared pipeline, so next answers the
// same narrowed question ready/backlog/queue would.
func TestRunNextTypeFilterPicksMatchingLeaf(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	task := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Top-ranked task"), Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: storage.MustParseLane("task")})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Lower-ranked bug"), Topic: "next", IssueType: "bug", Priority: 0, ParentID: epic.ID, Lane: storage.MustParseLane("bug")})

	text := h.runNextText("--type", "bug")
	if !strings.Contains(text, bug.ID.Raw()) {
		t.Fatalf("next --type bug output = %q, want %q picked", text, bug.ID)
	}
	if strings.Contains(text, task.ID.Raw()) {
		t.Fatalf("next --type bug output = %q, want %q filtered out despite outranking the bug", text, task.ID)
	}
}

// A held lane whose ready ticket is a task, asked for bugs: the bug elsewhere
// is served, the outcome records the task the filter hid, and nothing calls
// the epic out of work. Driven through runNext too, since the defect was in
// what the command handed routing.
func TestRunNextUnderAFilterServesPastTheTicketItHid(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic A"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("A.2"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epicA.ID})
	epicB := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic B"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("B.1"), Lane: storage.SharedLane, Topic: "next", IssueType: "bug", Priority: 1, ParentID: epicB.ID})
	h.applyAction(first.ID, model.Start{Assignee: "tester"}, "")
	h.applyAction(first.ID, model.Done{}, "")

	past, ok := h.runNextOutcomeAsked(workableFilter{IssueTypes: []model.IssueType{"bug"}}).(ServedPastNarrowing)
	if !ok || past.Row.ID != bug.ID || past.Narrowing.Hidden.ID != second.ID {
		t.Fatalf("next --type bug = %#v, want %s served past %s, which the filter hid — %s is ready in the epic, so it is not exhausted", past, bug.ID, second.ID, second.ID)
	}
	text := h.runNextText("--type", "bug")
	if !strings.Contains(text, bug.ID.Raw()) || strings.Contains(text, second.ID.Raw()) {
		t.Fatalf("next --type bug = %q, want %s handed back and %s left out", text, bug.ID, second.ID)
	}
}

// --limit and --columns stay off next: a single-row summary has no row count
// or column set to vary, so accepting them would be accepting input the
// command cannot honor.
func TestRunNextRejectsLimitAndColumns(t *testing.T) {
	t.Parallel()
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
// on?" — so these two tests pin both answers it can get.
//
// Driven through runNext rather than routeNext: only the real command proves
// --status reaches the gather that makes the in_progress row available to route
// at all.
func TestRunNextStatusInProgressResumesOurOwnWorkInFlight(t *testing.T) {
	t.Parallel()
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "sess-mine")
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	mine := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Already started"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.applyAction(mine.ID, model.Start{Assignee: "claude_sess-mine"}, "")

	text := h.runNextText("--status", "in_progress")
	if !strings.Contains(text, mine.ID.Raw()) {
		t.Fatalf("next --status in_progress = %q, want it to hand back %q — narrowing to in_progress is how an agent asks what it already holds", text, mine.ID)
	}
	resumed, ok := h.runNextOutcomeAsked(workableFilter{Status: model.StateInProgress}).(ResumedOwnWork)
	if !ok || resumed.Row.ID != mine.ID {
		t.Fatalf("next --status in_progress = %#v, want a resumption of %q and not a fresh start", resumed, mine.ID)
	}
}

// When the only in_progress rows are held fresh by other checkouts, every one
// verdicts routeAround and the walk ends at NoWork. The run hands no ticket
// back, and the outcome records the row another checkout holds, so the event
// tells an empty backlog from one whose work is held elsewhere.
func TestRunNextStatusInProgressRecordsForeignHeldWork(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Theirs, in flight"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 1, ParentID: epic.ID})
	h.asCheckout("otherstream01")
	h.applyAction(theirs.ID, model.Start{Assignee: "tester"}, "")

	none, ok := h.runNextOutcomeAsked(workableFilter{Status: model.StateInProgress}).(NoWork)
	if !ok || len(none.Unreachable) != 1 || none.Unreachable[0].ID != theirs.ID || none.Unreachable[0].Kind != reachHeldFresh {
		t.Fatalf("next --status in_progress = %#v, want NoWork recording %s as held by another checkout", none, theirs.ID)
	}
	text := h.runNextText("--status", "in_progress")
	if !strings.HasSuffix(text, noTicket+"\n") {
		t.Fatalf("next --status in_progress = %q, want it to say no ticket and not hand back %s", text, theirs.ID)
	}
}

// An empty backlog is an answer, not a failure: the run succeeds and says there
// is no ticket.
func TestRunNextOnAnEmptyBacklogSaysThereIsNoTicket(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	if outcome, ok := h.runNextOutcome().(NoWork); !ok {
		t.Fatalf("routeNext on an empty backlog = %#v, want NoWork", outcome)
	}
	if text := h.runNextText(); !strings.HasSuffix(text, noTicket+"\n") {
		t.Fatalf("next on an empty backlog = %q, want <ticket> to read %q", text, noTicket)
	}
}

// Every outcome fires the one `next` event. One that hands a ticket back
// carries it as the occasion's ticket, with its labels, and in <ticket>; one
// that hands none back carries no ticket and the statement that there is none.
func TestNextOccasionCarriesTheTicketOrSaysThereIsNone(t *testing.T) {
	t.Parallel()
	for _, tc := range sealedNextOutcomes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			occasion := nextOccasion(tc.outcome, nil, claimContext{self: selfAttribution})
			if occasion.Event != workflows.EventNext {
				t.Fatalf("nextOccasion(%T).Event = %q, want %q", tc.outcome, occasion.Event, workflows.EventNext)
			}
			ticket := occasion.Data["ticket"]
			if tc.served && (occasion.IssueID != sealedSumRow.ID || !slices.Equal(occasion.Labels, []string{"tracked"}) || !strings.Contains(ticket, sealedSumRow.ID.Raw())) {
				t.Fatalf("nextOccasion(%T) = %#v, want %s as its ticket, labelled, and in <ticket>", tc.outcome, occasion, sealedSumRow.ID)
			}
			if !tc.served && (occasion.IssueID != "" || occasion.Labels != nil || ticket != noTicket) {
				t.Fatalf("nextOccasion(%T) = %#v, want no ticket and <ticket> = %q", tc.outcome, occasion, noTicket)
			}
		})
	}
}

// `--continue` is retired: claim routing subsumes the epic-affinity bias
// unconditionally (routeNext's ServedFromEpicLane step).
// Passing the flag surfaces a pointer to that replacement instead of an
// unhelpful "unknown flag" error.
func TestRunNextContinueFlagIsRetired(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Container"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	leaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Leaf"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})

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

// `lit next` reports a pick; `lit start` takes it. This is that contract as the
// only thing that finally settles it — not what the output says, but what the
// store holds after the command has run.
//
// Asserted through runNext so the whole command path is under it, and over the
// pick that has the most to lie about: an unclaimed ticket in a lane nobody
// holds.
func TestRunNextClaimsNothingAndStartsNothing(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "next", IssueType: "epic", Priority: 1})
	target := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Unclaimed"), Lane: storage.SharedLane, Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})

	before := h.issueDetail(target.ID)
	text := h.runNextText()
	after := h.issueDetail(target.ID)

	if !strings.Contains(text, target.ID.Raw()) {
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
}
