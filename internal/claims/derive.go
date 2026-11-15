package claims

import (
	"slices"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Freshness is the read-time judgement of what still counts: the clock reading
// this derivation runs against, and how far back evidence remains good for.
// Both travel as data because the derivation reads no clock of its own — the
// same evidence and the same Freshness always yield the same claims, which is
// what makes the predicate testable without waiting for time to pass.
// [LAW:effects-at-boundaries]
//
// Window is the configured freshness window T (claims.freshness_window,
// default 6h), validated positive where the config is loaded.
type Freshness struct {
	Now    time.Time
	Window time.Duration
}

// Covers reports whether evidence recorded at t is still inside the window.
// Evidence exactly on the boundary is covered: the window is how long a claim
// survives, so the instant it turns is the last one it holds.
// [LAW:single-enforcer] The one place the window is compared against a
// timestamp, so "stale" cannot come to mean two things.
func (f Freshness) Covers(t time.Time) bool {
	return !t.Before(f.Now.Add(-f.Window))
}

// Derive reads every lane's standing out of the evidence. It is the whole public
// act of this package, and it writes nothing: Evidence is a value, Freshness is a
// value, LocalCheckouts is a value, and Standings is a value.
func Derive(evidence Evidence, fresh Freshness, local LocalCheckouts) Standings {
	standings := make(Standings, len(evidence.members))
	for lane := range evidence.members {
		standings[lane] = evidence.Lane(lane).Standing(fresh, local)
	}
	return standings
}

// standingOf applies the four-legged predicate to one lane. Each failure returns
// the variant that failure means rather than a flag some later step interprets:
// the shape of the answer is the answer.
//
// The legs run 1, 4, 2, 3 — dependency order, not the order the design numbers
// them. Leg 4 is a filter over the evidence rather than a test of a holder, so
// it has to run before leg 2 asks which establishing event is the *latest* one:
// run afterwards, leg 2 would be answering that question over events leg 4 was
// about to disprove, and the lane would read unclaimed where it should have
// reverted to the checkout still working it.
//
// events arrive in the total order NewEvidence imposed, oldest first; "latest"
// throughout this file means "last in that order."
func standingOf(members []model.Issue, events []model.IssueEvent, fresh Freshness, local LocalCheckouts) Standing {
	// Leg 1 — the lane is unfinished. A lane with nothing left in play is over,
	// and a claim on finished work would route other streams around a lane that
	// has no work in it to take.
	if !slices.ContainsFunc(members, model.Issue.InPlay) {
		return Unclaimed{}
	}

	// Leg 4 — the holder is live as far as this machine can tell. It applies
	// here, as a filter, because a checkout this machine has proven deleted did
	// not merely stop holding the lane: its evidence is disproven, so the lane
	// reverts to whoever else has standing rather than to nobody. See
	// LocalCheckouts.Void for why this is the one kind of missing producer the
	// derivation reads past.
	//
	// Cloned because DeleteFunc compacts in place: without it, deriving once
	// would strip the void events out of the Evidence itself, and a second
	// derivation over the same reading — a different freshness window, a
	// re-enumerated machine — would answer from a backlog quietly missing the
	// events the first call decided to ignore. Evidence is a reading, and a
	// reading that changes when you read it is not one. [LAW:one-source-of-truth]
	admissible := slices.DeleteFunc(slices.Clone(events), func(event model.IssueEvent) bool {
		return local.Void(event.Attribution)
	})

	// Leg 2 — the holder produced the latest admissible establishing event. With
	// none left, nobody holds the lane.
	//
	// An establishing event with no attribution belongs to the public checkout
	// (model.Attribution.Present) and holds the lane like any other. Derivation
	// stops at it rather than scanning back to an older attributed event,
	// because that older event is one we have positive evidence was superseded.
	establisher, found := LatestEstablisher(admissible)
	if !found {
		return Unclaimed{}
	}
	holder := HolderOf(establisher)
	activity := lastActivityByHolder(admissible)
	tenure := Tenure{By: holder, Since: establisher.CreatedAt, LastActivity: activity[holder]}

	// Leg 3 — the claim is fresh. Freshness is measured from the holder's last
	// mutation of any kind in the lane, not from the establishing event, so
	// ordinary working commentary carries a claim through a long stretch on a
	// single ticket. Past the window the claim is over and the lane is
	// Unclaimed — the same value a lane nobody ever started derives, because
	// an expired claim is not a claim and nothing downstream is owed the fact
	// that one existed (links-claims-y6yz).
	//
	// The one thing that carries a claim past the window is a lock on the
	// holder's worktree. Every other liveness signal this machine has says a
	// tree EXISTS, which a dead session leaves behind just as readily, but
	// `git worktree lock` is the holder speaking — a deliberate do-not-disturb
	// nobody sets by walking away — so leg 3 defers to leg 4's finding in that
	// one case and in no other (links-claims-2wk2). Mere presence deliberately
	// does not sustain a claim: a worktree outliving its session is ordinary,
	// and the age-out exists precisely for that case.
	// [LAW:single-enforcer] This is the one place a claim's liveness is
	// decided; consumers read Held or Unclaimed and never a clock or a
	// presence of their own.
	if !fresh.Covers(tenure.LastActivity) && local.PresenceOf(holder.Checkout) != Locked {
		return Unclaimed{}
	}
	return Held{Tenure: tenure}
}

// lastActivityByHolder folds the lane's events into when each holder last did
// anything here. The events are ordered oldest first, so the last write wins and
// is the holder's latest.
//
// Keyed on the Holder, not the checkout, so a session's commentary keeps only its
// own hold fresh: another session in the same checkout does not refresh it.
func lastActivityByHolder(events []model.IssueEvent) map[Holder]time.Time {
	activity := make(map[Holder]time.Time)
	for _, event := range events {
		activity[HolderOf(event)] = event.CreatedAt
	}
	return activity
}
