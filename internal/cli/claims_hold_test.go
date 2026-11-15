package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// foreignStream is the stream token of a checkout other than the harness's
// own: a worktree of it or another clone, which the store tells apart from
// this checkout the same way, by token. Neither is enumerated on this machine,
// so a line naming it names the token.
const foreignStream = "clonestream00001"

// holdHarness is a ready harness whose leaves name who is acting with --by, so
// one test can act as two sessions: the session env is cleared, and each run
// says who it is.
func holdHarness(t *testing.T) readyTestHarness {
	t.Helper()
	h := newReadyTestHarness(t)
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "")
	return h
}

// runTransitionAs runs `lit <verb>` in process in this checkout as by, with
// args after the id, and returns what it printed and its error.
func (h readyTestHarness) runTransitionAs(spec transitionSpec, id model.ID, by string, args ...string) (string, error) {
	h.t.Helper()
	var out bytes.Buffer
	err := runLeaf(h.t, transitionLeaf(spec), h.ctx, &out, h.ap, append([]string{id.Raw(), "--by", by}, args...))
	return out.String(), err
}

// heldTicket files one task under an epic and starts it as by in the checkout
// whose stream is checkout, then points the store back at this harness's own
// checkout, the one every later leaf runs in.
func (h readyTestHarness) heldTicket(checkout, by string) (epic, ticket model.Issue) {
	h.t.Helper()
	epic = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Hold epic"), Lane: storage.SharedLane, Topic: "hold", IssueType: "epic"})
	ticket = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("held ticket"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID})
	h.startIn(checkout, ticket.ID, by)
	return epic, ticket
}

// startIn records a start of id by by in the checkout whose stream is
// checkout, then points the store back at this harness's own checkout.
func (h readyTestHarness) startIn(checkout string, id model.ID, by string) {
	h.t.Helper()
	h.asCheckout(checkout)
	if _, err := h.ap.Store.Apply(h.ctx, id, storage.Change{Action: model.Start{Assignee: by}, Actor: by}); err != nil {
		h.t.Fatalf("start %s as %s in %s: %v", id, by, checkout, err)
	}
	h.asCheckout(h.ap.Stream.Value())
}

// heldByForeign is the line the hold gate names a ticket held by
// foreignStream with: the checkout, and nobody acting in it.
func (h readyTestHarness) heldByForeign(id model.ID) string {
	return id.Raw() + " is held by " + nameCheckout(model.NewAttribution(foreignStream, h.ap.Workspace.WorkspaceID)) + " (active under a minute ago)"
}

// assertRefused is the refusal a checkout that does not hold a ticket meets:
// the takeover_unconfirmed error whose message is exactly want.
func assertRefused(t *testing.T, err error, want string) {
	t.Helper()
	var refusal takeoverUnconfirmedError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v (%T), want a takeoverUnconfirmedError", err, err)
	}
	if refusal.Message != want {
		t.Fatalf("refusal = %q\nwant      %q", refusal.Message, want)
	}
	if got := commandErrorReason(err); got != reasonTakeoverUnconfirmed {
		t.Fatalf("error reason = %v, want %v", got, reasonTakeoverUnconfirmed)
	}
}

// runNext runs `lit next` in process in this checkout and returns its output.
func (h readyTestHarness) runNext() string {
	h.t.Helper()
	var out bytes.Buffer
	if err := runLeaf(h.t, nextLeaf(), h.ctx, &out, h.ap, nil); err != nil {
		h.t.Fatalf("next: %v", err)
	}
	return out.String()
}

// Two sessions in the checkout that holds a ticket both act on it as its
// holder: the second session's `lit next` hands it back, and its start, phase
// and done all land with no --take and nothing said about a hold.
func TestTwoSessionsInTheHoldingCheckoutBothActWithoutTake(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, ticket := h.heldTicket(h.ap.Stream.Value(), "session-a")

	if got, want := openingLine(h.runNext()), ticket.ID.Raw()+" is already in progress in a lane you hold — continue where you left off"; got != want {
		t.Fatalf("next opens with %q, want %q — the checkout's ticket is any of its sessions' to continue", got, want)
	}
	for _, step := range []struct {
		by   string
		spec transitionSpec
		args []string
	}{
		{"session-b", startSpec, nil},
		{"session-a", phaseSpec, []string{"--to", string(model.PhaseImpl)}},
		{"session-b", phaseSpec, []string{"--to", string(model.PhaseReview)}},
		{"session-a", doneSpec, nil},
	} {
		out, err := h.runTransitionAs(step.spec, ticket.ID, step.by, step.args...)
		if err != nil {
			t.Fatalf("%s %v as %s = %v, want it to land without --take", step.spec.name, step.args, step.by, err)
		}
		if strings.Contains(out, "--take") || strings.Contains(out, "held by") || strings.Contains(out, "claim transferred") {
			t.Fatalf("%s %v as %s printed %q, want nothing about the hold", step.spec.name, step.args, step.by, out)
		}
	}
	if got := h.issueDetail(ticket.ID).State(); got != model.StateClosed {
		t.Fatalf("state after done = %v, want closed", got)
	}
}

// A start on a ticket another checkout holds is refused, naming that checkout
// and nobody in it, and leaves the ticket as it was.
func TestStartRefusedOnATicketAnotherCheckoutHoldsNamesTheCheckout(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, ticket := h.heldTicket(foreignStream, "session-a")

	_, err := h.runTransitionAs(startSpec, ticket.ID, "session-b")
	assertRefused(t, err, h.heldByForeign(ticket.ID)+" — pass --take to take it over")
	if got := h.issueDetail(ticket.ID).AssigneeValue(); got != "session-a" {
		t.Fatalf("assignee after the refused start = %q, want the hold untouched", got)
	}
}

// freeTicket files a ready ticket alone in an epic of its own, a lane nobody
// holds, ranked below everything filed before it.
func (h readyTestHarness) freeTicket() (epic, ticket model.Issue) {
	h.t.Helper()
	epic = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Free epic"), Lane: storage.SharedLane, Topic: "free", IssueType: "epic"})
	return epic, h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("nobody's"), Lane: storage.SharedLane, Topic: "free", IssueType: "task", ParentID: epic.ID})
}

// `lit next` never serves a ticket another checkout holds: it routes past it
// to the next lane nobody holds.
func TestRouteNextRoutesAroundTicketHeldByAnotherCheckout(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	h.heldTicket(foreignStream, "session-a")
	freeEpic, free := h.freeTicket()

	if got, want := openingLine(h.runNext()), "from a worktree of its own off the trunk (not the primary clone, where every session starts), run `lit start "+free.ID.Raw()+"` to claim the default lane of epic "+freeEpic.ID.Raw(); got != want {
		t.Fatalf("next opens with %q, want %q — the ticket another checkout holds must be routed around", got, want)
	}
}

// The hold is the lane's, so a sibling in a lane this checkout holds is its next
// ticket whoever its assignee names.
func TestRouteNextServesTheHolderItsLaneSiblingWhateverItsAssignee(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	epic, held := h.heldTicket(h.ap.Stream.Value(), "session-a")
	if _, err := h.runTransitionAs(doneSpec, held.ID, "session-a"); err != nil {
		t.Fatalf("done %s as its holder: %v", held.ID, err)
	}
	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("next in the lane"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID, Assignee: "somebody-else"})
	h.freeTicket()

	// Served from the lane this checkout holds, so nothing is announced above
	// the row: the summary line is the first thing printed.
	if got, want := openingLine(h.runNext()), sibling.ID.Raw()+"  open  hold  next in the lane"; got != want {
		t.Fatalf("next opens with %q, want %q — the lane's sibling is the holder's, whoever it is assigned to", got, want)
	}
}

// openingLine is the first line a command printed about the work, past the
// warning a harness outside any git repository draws on every claim read.
func openingLine(text string) string {
	line, _, _ := strings.Cut(withoutLivenessWarning(text), "\n")
	return line
}

// withoutLivenessWarning drops gatherCheckouts' warning that this machine's
// checkouts could not be enumerated: the harness is not a git repository, so
// every claim read here draws it, and it says nothing about the hold.
func withoutLivenessWarning(text string) string {
	if strings.HasPrefix(text, "warning: could not enumerate local checkouts") {
		_, rest, _ := strings.Cut(text, "\n")
		return rest
	}
	return text
}

// `lit phase` and `lit done` on a ticket another checkout holds are refused,
// and leave it as it was; the same command with --take is applied and says
// what it did to the hold: done establishes, so it takes the lane over, and a
// phase move does not, so it says it acted without taking it.
func TestPhaseAndDoneRefusedForAnotherCheckoutUnlessTake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		verb string
		spec transitionSpec
		args []string
		// how is what --take does to the hold for this verb.
		how crossing
		// notice is what the verb itself prints after the takeover line.
		notice string
		// landed reports whether the transition took effect.
		landed func(h readyTestHarness, id model.ID) bool
	}{
		{"phase", phaseSpec, []string{"--to", string(model.PhaseReview)}, actsOnHeld, "phase: plan → review\n", func(h readyTestHarness, id model.ID) bool {
			state, err := readTicketState(h.ctx, h.ap.Store, h.issueDetail(id))
			if err != nil {
				h.t.Fatalf("read %s: %v", id, err)
			}
			return state.phase == model.PhaseReview
		}},
		{"done", doneSpec, nil, takesOver, "", func(h readyTestHarness, id model.ID) bool {
			return h.issueDetail(id).State() == model.StateClosed
		}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()
			h := holdHarness(t)
			_, ticket := h.heldTicket(foreignStream, "session-a")
			refusal := h.heldByForeign(ticket.ID)

			_, err := h.runTransitionAs(tc.spec, ticket.ID, "session-b", tc.args...)
			assertRefused(t, err, refusal+tc.how.refusal)
			if tc.landed(h, ticket.ID) {
				t.Fatalf("%s from another checkout without --take changed the ticket", tc.verb)
			}

			out, err := h.runTransitionAs(tc.spec, ticket.ID, "session-b", append(tc.args, "--take")...)
			if err != nil {
				t.Fatalf("%s --take from another checkout = %v, want it applied", tc.verb, err)
			}
			if want := refusal + tc.how.announce + tc.notice; !strings.HasPrefix(withoutLivenessWarning(out), want) {
				t.Fatalf("%s --take printed %q, want it to open with %q", tc.verb, out, want)
			}
			if !tc.landed(h, ticket.ID) {
				t.Fatalf("%s --take from another checkout did not change the ticket", tc.verb)
			}
		})
	}
}

// A transition that records nothing crosses no hold, so another checkout is
// neither refused nor told it took anything over: a phase move to the phase
// the ticket is already in passes silently and leaves the hold where it was.
func TestNoOpTransitionFromAnotherCheckoutIsNotGated(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, ticket := h.heldTicket(foreignStream, "session-a")

	out, err := h.runTransitionAs(phaseSpec, ticket.ID, "session-b", "--to", string(model.PhasePlan))
	if err != nil {
		t.Fatalf("no-op phase move from another checkout = %v, want it to pass ungated", err)
	}
	if strings.Contains(out, "--take") || strings.Contains(out, "held by") {
		t.Fatalf("no-op phase move printed %q, want nothing about the hold", out)
	}
	if _, err := h.runTransitionAs(phaseSpec, ticket.ID, "session-b", "--to", string(model.PhaseReview)); err == nil {
		t.Fatalf("a real phase move from another checkout passed, want the hold to still be the other checkout's")
	}
}

// The hold is the lane's, so `lit next` can hand this checkout a ticket in
// flight that another checkout started, once this checkout took the lane
// through a sibling. It names the checkout that started it rather than saying
// to continue where this one left off.
func TestNextNamesTheCheckoutThatStartedASiblingInFlight(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	epic, theirs := h.heldTicket(foreignStream, "session-a")
	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("sibling"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID})

	if _, err := h.runTransitionAs(startSpec, sibling.ID, "session-b", "--take"); err != nil {
		t.Fatalf("start %s --take: %v", sibling.ID, err)
	}
	want := theirs.ID.Raw() + " is already in progress in a lane you hold, started by " + nameCheckout(model.NewAttribution(foreignStream, h.ap.Workspace.WorkspaceID)) + " — read its history before you continue it"
	if got := openingLine(h.runNext()); got != want {
		t.Fatalf("next opens with %q, want %q", got, want)
	}
}

// The hold moves through siblings. This checkout holds its ticket; another
// checkout then takes the lane by starting a sibling. `lit start --take` on
// this checkout's own ticket, whose record still names this checkout, takes
// the lane back and records it, so the sibling is this checkout's to move
// afterwards with no --take.
func TestTakeBackThroughAnUnchangedTicketMovesTheLaneHold(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	epic, mine := h.heldTicket(h.ap.Stream.Value(), "session-a")
	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("sibling"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID})
	h.startIn(foreignStream, sibling.ID, "session-c")

	_, err := h.runTransitionAs(phaseSpec, sibling.ID, "session-a", "--to", string(model.PhaseReview))
	assertRefused(t, err, h.heldByForeign(sibling.ID)+actsOnHeld.refusal)

	out, err := h.runTransitionAs(startSpec, mine.ID, "session-a", "--take")
	if err != nil {
		t.Fatalf("start %s --take: %v", mine.ID, err)
	}
	if !strings.Contains(out, "--take") {
		t.Fatalf("start %s --take printed %q, want the takeover announced", mine.ID, out)
	}
	if _, err := h.runTransitionAs(phaseSpec, sibling.ID, "session-a", "--to", string(model.PhaseReview)); err != nil {
		t.Fatalf("phase %s after taking the lane back = %v, want the hold to be this checkout's", sibling.ID, err)
	}
}

// The public checkout is every unattributed writer at once, so its holds never
// read as ours and a repeat start needs --take, but a start it re-runs on a
// ticket it last took took nothing from anyone, and records nothing.
func TestRepeatStartInThePublicCheckoutRecordsNothing(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, ticket := h.heldTicket("", "")
	h.asCheckout("")
	before, err := h.ap.Store.ListEvents(h.ctx, ticket.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if _, err := h.runTransitionAs(startSpec, ticket.ID, "", "--take"); err != nil {
		t.Fatalf("repeat start %s --take in the public checkout: %v", ticket.ID, err)
	}
	after, err := h.ap.Store.ListEvents(h.ctx, ticket.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("repeat start recorded %d events, want none", len(after)-len(before))
	}
}
