package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// holdHarness is a ready harness whose leaves name their session with --by, so
// one test can act as two sessions: the session env is cleared, and each run
// says who it is.
func holdHarness(t *testing.T) readyTestHarness {
	t.Helper()
	h := newReadyTestHarness(t)
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "")
	return h
}

// runTransitionAs runs `lit <verb>` in process as session by, with args after
// the id, and returns what it printed and its error.
func (h readyTestHarness) runTransitionAs(spec transitionSpec, id model.ID, by string, args ...string) (string, error) {
	h.t.Helper()
	var out bytes.Buffer
	err := runLeaf(h.t, transitionLeaf(spec), h.ctx, &out, h.ap, append([]string{id.Raw(), "--by", by}, args...))
	return out.String(), err
}

// heldTicket files one task under an epic and starts it as session holder in
// the checkout whose stream is checkout, then points the store back at this
// harness's own checkout, the one every later leaf runs in.
func (h readyTestHarness) heldTicket(checkout, holder string) (epic, ticket model.Issue) {
	h.t.Helper()
	epic = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Hold epic"), Lane: storage.SharedLane, Topic: "hold", IssueType: "epic"})
	ticket = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("held ticket"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID})
	h.asCheckout(checkout)
	if _, err := h.runTransitionAs(startSpec, ticket.ID, holder); err != nil {
		h.t.Fatalf("start %s as %s in %s: %v", ticket.ID, holder, checkout, err)
	}
	h.asCheckout(h.ap.Stream.Value())
	return epic, ticket
}

// assertRefusedNamingHolder is the refusal every non-holder meets: the
// takeover_unconfirmed error whose message names the holder session and the
// checkout it holds from, exactly.
func assertRefusedNamingHolder(t *testing.T, err error, want string) {
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

// A second session's `lit start` on a ticket another session holds is
// refused, naming the holder, wherever the holder's checkout is: this same
// checkout, a worktree of it, or another checkout. In process the latter two
// differ from this checkout the way the store sees them — by stream token —
// and neither is enumerated on this machine, so both are named by the token.
func TestStartRefusedOnASessionHeldTicketNamesTheHolder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		checkout func(h readyTestHarness) string
	}{
		{"same checkout", func(h readyTestHarness) string { return h.ap.Stream.Value() }},
		{"worktree of this checkout", func(readyTestHarness) string { return "worktreestream01" }},
		{"another checkout", func(readyTestHarness) string { return "clonestream00001" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := holdHarness(t)
			checkout := tc.checkout(h)
			_, ticket := h.heldTicket(checkout, "session-a")

			_, err := h.runTransitionAs(startSpec, ticket.ID, "session-b")
			address := nameCheckout(model.NewAttribution(checkout, h.ap.Workspace.WorkspaceID))
			assertRefusedNamingHolder(t, err, ticket.ID.Raw()+" is held by session-a in "+address+" (active under a minute ago) — pass --take to take it over")
			if got := h.issueDetail(ticket.ID).AssigneeValue(); got != "session-a" {
				t.Fatalf("assignee after the refused start = %q, want session-a's hold untouched", got)
			}

			// The holder itself passes, with no ceremony, from its own checkout.
			if checkout == h.ap.Stream.Value() {
				out, err := h.runTransitionAs(startSpec, ticket.ID, "session-a")
				if err != nil {
					t.Fatalf("holder's own start = %v, want it to pass", err)
				}
				if strings.Contains(out, "--take") {
					t.Fatalf("holder's own start = %q, want no takeover ceremony", out)
				}
			}
		})
	}
}

// freeTicket files a ready ticket alone in an epic of its own, a lane nobody
// holds, ranked below everything filed before it.
func (h readyTestHarness) freeTicket() (epic, ticket model.Issue) {
	h.t.Helper()
	epic = h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Free epic"), Lane: storage.SharedLane, Topic: "free", IssueType: "epic"})
	return epic, h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("nobody's"), Lane: storage.SharedLane, Topic: "free", IssueType: "task", ParentID: epic.ID})
}

// runNextAs runs `lit next` in process as session by and returns its output.
func (h readyTestHarness) runNextAs(by string) string {
	h.t.Helper()
	var out bytes.Buffer
	if err := runLeaf(h.t, nextLeaf(), h.ctx, &out, h.ap, []string{"--by", by}); err != nil {
		h.t.Fatalf("next --by %s: %v", by, err)
	}
	return out.String()
}

// `lit next` never serves a ticket another live session holds, even in this
// same checkout: a second session is routed past the held ticket to the next
// lane nobody holds. The holder session, asking the same question in the same
// checkout, is handed its own work back.
func TestRouteNextRoutesAroundTicketHeldByAnotherSessionInThisCheckout(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, held := h.heldTicket(h.ap.Stream.Value(), "session-a")
	freeEpic, free := h.freeTicket()

	if got, want := openingLine(h.runNextAs("session-b")), "from a worktree of its own off the trunk (not the primary clone, where every session starts), run `lit start "+free.ID.Raw()+"` to claim the default lane of epic "+freeEpic.ID.Raw(); got != want {
		t.Fatalf("next as session-b opens with %q, want %q — the ticket session-a holds must be routed around", got, want)
	}
	if got, want := openingLine(h.runNextAs("session-a")), held.ID.Raw()+" is already in progress in a lane you hold — continue where you left off"; got != want {
		t.Fatalf("next as session-a opens with %q, want %q — the holder gets its own work back", got, want)
	}
}

// The hold is the lane's, so a sibling in the lane the session holds is that
// session's next ticket whoever its assignee names, and is no other session's.
func TestRouteNextServesTheHolderItsLaneSiblingWhateverItsAssignee(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	epic, held := h.heldTicket(h.ap.Stream.Value(), "session-a")
	if _, err := h.runTransitionAs(doneSpec, held.ID, "session-a"); err != nil {
		t.Fatalf("done %s as its holder: %v", held.ID, err)
	}

	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("next in the lane"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID, Assignee: "somebody-else"})
	freeEpic, free := h.freeTicket()

	// Served from the lane session-a holds, so nothing is announced above the
	// row: the summary line is the first thing printed.
	if got, want := openingLine(h.runNextAs("session-a")), sibling.ID.Raw()+"  open  hold  next in the lane"; got != want {
		t.Fatalf("next as session-a opens with %q, want %q — the lane's sibling is the holder's, whoever it is assigned to", got, want)
	}
	if got, want := openingLine(h.runNextAs("session-b")), "from a worktree of its own off the trunk (not the primary clone, where every session starts), run `lit start "+free.ID.Raw()+"` to claim the default lane of epic "+freeEpic.ID.Raw(); got != want {
		t.Fatalf("next as session-b opens with %q, want %q — session-a's lane is not session-b's", got, want)
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

// `lit phase` and `lit done` from a session that does not hold the ticket are
// refused, and leave it as it was; the same command with --take is applied and
// says what it did to the hold: done establishes, so it takes the lane over,
// and a phase move does not, so it says it acted without taking it.
func TestPhaseAndDoneRefusedForNonHolderUnlessTake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		verb string
		spec transitionSpec
		args []string
		// how is what --take does to the hold for this verb.
		how crossing
		// notice is what the verb itself prints after the takeover line.
		notice func(id model.ID) string
		// landed reports whether the transition took effect.
		landed func(h readyTestHarness, id model.ID) bool
	}{
		{"phase", phaseSpec, []string{"--to", string(model.PhaseReview)}, actsOnHeld, func(model.ID) string { return "phase: plan → review\n" }, func(h readyTestHarness, id model.ID) bool {
			state, err := readTicketState(h.ctx, h.ap.Store, h.issueDetail(id))
			if err != nil {
				h.t.Fatalf("read %s: %v", id, err)
			}
			return state.phase == model.PhaseReview
		}},
		{"done", doneSpec, nil, takesOver, func(id model.ID) string {
			return "warning: finished without an approved plan: " + id.Raw() + " has no plan\n"
		}, func(h readyTestHarness, id model.ID) bool {
			return h.issueDetail(id).State() == model.StateClosed
		}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()
			h := holdHarness(t)
			_, ticket := h.heldTicket(h.ap.Stream.Value(), "session-a")
			refusal := ticket.ID.Raw() + " is held by session-a in " + nameCheckout(ownAttribution(h.ap)) + " (active under a minute ago)"

			_, err := h.runTransitionAs(tc.spec, ticket.ID, "session-b", tc.args...)
			assertRefusedNamingHolder(t, err, refusal+tc.how.refusal)
			if tc.landed(h, ticket.ID) {
				t.Fatalf("%s by a non-holder without --take changed the ticket", tc.verb)
			}

			out, err := h.runTransitionAs(tc.spec, ticket.ID, "session-b", append(tc.args, "--take")...)
			if err != nil {
				t.Fatalf("%s --take by a non-holder = %v, want it applied", tc.verb, err)
			}
			if want := refusal + tc.how.announce + tc.notice(ticket.ID); !strings.HasPrefix(withoutLivenessWarning(out), want) {
				t.Fatalf("%s --take printed %q, want it to open with %q", tc.verb, out, want)
			}
			if !tc.landed(h, ticket.ID) {
				t.Fatalf("%s --take by a non-holder did not change the ticket", tc.verb)
			}
		})
	}
}

// A transition that records nothing crosses no hold, so a non-holder is neither
// refused nor told it took anything over: a phase move to the phase the ticket
// is already in passes silently and leaves the hold where it was.
func TestNoOpTransitionByANonHolderIsNotGated(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, ticket := h.heldTicket(h.ap.Stream.Value(), "session-a")

	out, err := h.runTransitionAs(phaseSpec, ticket.ID, "session-b", "--to", string(model.PhasePlan))
	if err != nil {
		t.Fatalf("no-op phase move by a non-holder = %v, want it to pass ungated", err)
	}
	if strings.Contains(out, "--take") || strings.Contains(out, "held by") {
		t.Fatalf("no-op phase move printed %q, want nothing about the hold", out)
	}
	if _, err := h.runTransitionAs(phaseSpec, ticket.ID, "session-b", "--to", string(model.PhaseReview)); err == nil {
		t.Fatalf("a real phase move by the non-holder passed, want the hold to still be session-a's")
	}
}

// A successor session in the holder's own checkout, under the assignee the
// ticket already names, takes the ticket with --take and then holds it: its
// next move needs no --take. The claimant is the holder (checkout and
// session), so the start is a real transfer even though the assignee is the
// same on both sides.
func TestTakeBySuccessorUnderTheSameAssigneeMovesTheHold(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	_, ticket := h.heldTicket(h.ap.Stream.Value(), "session-a")
	successor := "session-b"
	if _, err := h.ap.Store.Apply(h.ctx, ticket.ID, storage.Change{Fields: storage.UpdateIssueInput{Assignee: &successor}, Actor: "session-a"}); err != nil {
		t.Fatalf("reassign %s: %v", ticket.ID, err)
	}

	if _, err := h.runTransitionAs(startSpec, ticket.ID, "session-b", "--take"); err != nil {
		t.Fatalf("start --take by the successor = %v", err)
	}
	if _, err := h.runTransitionAs(phaseSpec, ticket.ID, "session-b", "--to", string(model.PhaseReview)); err != nil {
		t.Fatalf("successor's phase move after --take = %v, want the hold to be its own", err)
	}
}

// The hold is the lane's, so it moves through siblings. Session-b takes the
// lane by starting a sibling with --take, and `lit next` hands it session-a's
// ticket in flight, naming who started it rather than telling it to continue
// where it left off. Session-a then takes the lane back with `lit start
// --take` on its own ticket, whose record still names session-a. That start
// records, so session-b is the one refused afterwards.
func TestTakeBackThroughAnUnchangedTicketMovesTheLaneHold(t *testing.T) {
	t.Parallel()
	h := holdHarness(t)
	epic, mine := h.heldTicket(h.ap.Stream.Value(), "session-a")
	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("sibling"), Lane: storage.SharedLane, Topic: "hold", IssueType: "task", ParentID: epic.ID})

	if _, err := h.runTransitionAs(startSpec, sibling.ID, "session-b", "--take"); err != nil {
		t.Fatalf("start %s --take as session-b: %v", sibling.ID, err)
	}
	if got, want := openingLine(h.runNextAs("session-b")), mine.ID.Raw()+" is already in progress in a lane you hold, started by session-a in "; !strings.HasPrefix(got, want) {
		t.Fatalf("next as session-b opens with %q, want it to start %q — the sibling session-a started is not session-b's to continue blind", got, want)
	}

	out, err := h.runTransitionAs(startSpec, mine.ID, "session-a", "--take")
	if err != nil {
		t.Fatalf("start %s --take as session-a: %v", mine.ID, err)
	}
	if !strings.Contains(out, "--take") {
		t.Fatalf("start %s --take as session-a printed %q, want the takeover announced", mine.ID, out)
	}
	_, err = h.runTransitionAs(startSpec, sibling.ID, "session-b")
	var refusal takeoverUnconfirmedError
	if !errors.As(err, &refusal) || !strings.HasPrefix(refusal.Message, sibling.ID.Raw()+" is held by session-a in ") {
		t.Fatalf("start %s as session-b after session-a took the lane back = %v, want refused naming session-a", sibling.ID, err)
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
