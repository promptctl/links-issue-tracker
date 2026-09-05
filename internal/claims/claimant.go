package claims

import (
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Claimant is who the record names as holding one ticket: the assignee written
// on the issue row, and the checkout whose establishing event put it there.
//
// The pair is one value because neither half identifies a holder alone, and
// keeping them apart is what let the write side and this package mean different
// things by "owner". The assignee is the human or agent identity — routinely
// EMPTY, because a checkout with no agent session resolves no identity at all,
// and identical across every checkout one session drives. The checkout is the
// per-worktree stream token, and it is what lane ownership is actually keyed on:
// standingOf reads the latest establishing event's Attribution and never looks
// at an assignee. So two checkouts of one identity taking a lane from each other
// are a real transfer that the assignee cannot see, and a write side comparing
// only assignees read that transfer as a repeated self-start and discarded it.
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
// a new start supersedes. An issue whose history establishes nothing, or whose
// establishing event predates attribution, carries the absent checkout: the
// same "the record does not say who" that Derive reports as Unclaimed.
func ClaimantOf(issue model.Issue, events []model.IssueEvent) Claimant {
	establisher, found := LatestEstablisher(events)
	return Claimant{Established: found, Assignee: issue.AssigneeValue(), Checkout: establisher.Attribution}
}

// Held reports that the record names a holder, so a caller asking "was this
// ticket already taken" reads it here rather than spelling the question again.
// [LAW:one-source-of-truth]
//
// Note what it does NOT read: the identity halves. An empty assignee is the
// ordinary state of a human checkout, and an absent checkout is the ordinary
// state of a record older than attribution — either one read as "nobody" makes
// a real holder vanish. Derive answers a different question and is right to
// insist on the checkout: it routes lanes, and a hold it cannot address is one
// it must call Unclaimed. Announcing a hand-off only needs a predecessor to
// have existed.
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
