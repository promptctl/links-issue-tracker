package claims

import (
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Claimant is who the record names as holding one ticket: the assignee written
// on the issue row, and the checkout whose establishing event put it there.
//
// The pair is one value because neither half identifies a holder alone, and
// keeping them apart would let the write side and this package mean different
// things by "owner". The assignee is the human or agent identity — routinely
// EMPTY, because a checkout with no agent session resolves no identity at all,
// and identical across every checkout one session drives. The checkout is the
// per-worktree stream token, and it is what lane ownership is actually keyed
// on: standingOf reads the latest establishing event's Attribution and never
// looks at an assignee. So two checkouts of one identity taking a lane from
// each other are a real transfer that the assignee cannot see, and a write side
// comparing only assignees would read that transfer as a repeated self-start
// and discard it.
// [LAW:one-source-of-truth]
type Claimant struct {
	// Established reports that somebody took this ticket — that the history
	// carries an establishing event at all. It is its own fact because BOTH
	// identity halves go empty on real holders: a checkout driving no agent
	// session resolves no assignee, and an event recorded before attribution
	// existed carries no checkout. Reading a holder off either half alone
	// therefore reads "assignee set by `lit new --assignee`, never started" as
	// a hold, or a genuine pre-attribution hold as nobody.
	Established bool
	Assignee    string
	Checkout    model.Attribution
}

// ClaimantOf reads the claimant a ticket's own record names: the assignee on the
// issue, and the checkout on the newest establishing event among its events.
//
// events is that issue's history — the write side hands over what it read for
// the one row it is about to move, not a lane's run — because the question here
// is "who does the log say last took THIS ticket", which is exactly the event
// a new start supersedes. An issue whose establishing event predates
// attribution carries the public checkout, the same holder Derive reads it as;
// one whose history establishes nothing carries that same zero value with
// Established false, which is why Held reads the flag and not the pair.
func ClaimantOf(issue model.Issue, events []model.IssueEvent) Claimant {
	establisher, found := LatestEstablisher(events)
	return Claimant{Established: found, Assignee: issue.AssigneeValue(), Checkout: establisher.Attribution}
}

// Held reports that the record names a holder, so a caller asking "was this
// ticket already taken" reads it here rather than spelling the question again.
// [LAW:one-source-of-truth]
//
// Note what it does NOT read: the identity halves. An empty assignee is the
// ordinary state of a human checkout, and the public checkout is the ordinary
// state of a record older than attribution — either one read as "nobody" makes
// a real holder vanish. The flag is the only half that separates "somebody took
// this" from "nobody has", because both identity halves go empty on holders
// that are real: announcing a hand-off needs a predecessor to have EXISTED, not
// to have been addressable.
func (c Claimant) Held() bool { return c.Established }

// After returns the claimant a status action installs, taken by the checkout
// performing it. Start is the act of taking a ticket, so it is the only action
// that moves the claimant; every other transition preserves ownership, which is
// orthogonal to the status it drives. [LAW:types-are-the-program] The new owner
// comes from the one variant that carries one, not from a loose parameter every
// other action would have to ignore.
//
// A storage engine compares After's result against ClaimantOf's to decide
// whether a same-state transition owes a write. That comparison is the whole
// reason the pair exists, and it is why a `done` re-run against an already-done
// ticket still records nothing from a new checkout: done does not take, so the
// claimant it installs is the one already there.
func (c Claimant) After(action model.StatusAction, taker model.Attribution) Claimant {
	start, ok := action.(model.Start)
	if !ok {
		return c
	}
	return Claimant{Established: true, Assignee: strings.TrimSpace(start.Assignee), Checkout: taker}
}

// Handover is a ticket's claim passing from one holder to another: the
// claimant the record named before a start, and the one that start installed.
type Handover struct {
	From, To Claimant
}

// HandoverTo is the handover a write moving the claimant from c to next
// records, and nil when it records none: nobody had taken the ticket, so there
// was no claim to pass on, or the holder did not change.
//
// A storage engine asks it of the same pair it compares to decide whether the
// write is owed, and reports the answer, so the handover a caller announces is
// the one the write was decided on rather than a second reading of the rule.
// [LAW:one-source-of-truth]
func (c Claimant) HandoverTo(next Claimant) *Handover {
	if !c.Held() || next == c {
		return nil
	}
	return &Handover{From: c, To: next}
}

// Live reports that the claim a handover passed on was still a claim: the
// lane's standing names the handover's predecessor as its holder. A ticket's
// record goes on naming whoever last started it after that claim is over —
// it expired, the ticket finished, or a later start elsewhere in the lane
// superseded it — and a predecessor who holds nothing hands nothing over.
//
// The public checkout matches like any holder: it is the predecessor's real
// identity, not a claim of ownership by whoever asks, so the minted-token rule
// that guards "is this lane mine" does not apply.
func (h Handover) Live(lane Standing) bool {
	held, ok := lane.(Held)
	return ok && held.By == h.From.Checkout
}
