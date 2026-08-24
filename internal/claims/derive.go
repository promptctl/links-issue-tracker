package claims

import (
	"slices"
	"strings"
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
// default 24h), validated positive where the config is loaded.
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
	for lane, members := range evidence.members {
		standings[lane] = standingOf(members, evidence.events[lane], fresh, local)
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

	// Leg 2 — the holder produced the latest establishing event. Two ways for
	// that to name nobody, and they are one answer: no lifecycle transition ever
	// happened here, or the one that did carries no attribution.
	//
	// The unattributed case is the common one on any repository with history,
	// because attribution was added to events that already existed and is never
	// backfilled — it is historical fact, so the events that predate it will
	// carry none forever. The derivation stops at it rather than scanning back to
	// the newest ancestor that does carry attribution, and that restraint is the
	// point: an unattributed `start` says somebody took this lane and the record
	// does not say who, so an older attributed event is not the best available
	// answer — it is an answer we have positive evidence was superseded. Handing
	// the lane to a checkout that demonstrably walked away from it, more
	// confidently the older the repository, is worse than admitting we cannot
	// tell. Unclaimed is the honest reading, and it is also the pre-claims
	// behavior, so the cost of admitting it is nil.
	establisher, found := latestEstablisher(admissible)
	if !found || !establisher.Attribution.Present() {
		return Unclaimed{}
	}
	holder := establisher.Attribution
	activity, establishers := trails(admissible)
	tenure := Tenure{By: holder, Since: establisher.CreatedAt, LastActivity: activity[holder]}

	// Leg 3 — the claim is fresh. Freshness is measured from the holder's last
	// mutation of any kind in the lane, not from the establishing event, so
	// ordinary working commentary carries a claim through a long stretch on a
	// single ticket. Past the window the lane is available again, but it keeps
	// its provenance: somebody was here, and whoever takes it over should know.
	if !fresh.Covers(tenure.LastActivity) {
		return Stale{Tenure: tenure}
	}
	return Held{Tenure: tenure, Contested: contestants(holder, activity, establishers, fresh)}
}

// latestEstablisher returns the last event that takes or transfers the lane.
func latestEstablisher(events []model.IssueEvent) (model.IssueEvent, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if establishes(events[i]) {
			return events[i], true
		}
	}
	return model.IssueEvent{}, false
}

// trails folds the lane's events into the two facts the predicate needs about
// each checkout that appears in them: when it last did anything here, and
// whether any of it was an establishing act. The events are ordered oldest
// first, so the last write to activity wins and is the checkout's latest.
func trails(events []model.IssueEvent) (activity map[model.Attribution]time.Time, establishers map[model.Attribution]struct{}) {
	activity = make(map[model.Attribution]time.Time)
	establishers = make(map[model.Attribution]struct{})
	for _, event := range events {
		activity[event.Attribution] = event.CreatedAt
		if establishes(event) {
			establishers[event.Attribution] = struct{}{}
		}
	}
	return activity, establishers
}

// contestants are the checkouts other than the holder that also have live
// evidence in this lane: an offline race, or a takeover the previous holder has
// not yet seen.
//
// Contest requires an establishing act, which is what keeps it meaningful. A
// second checkout that only commented here is not disputing possession — under
// the looser reading every lane anyone had ever glanced at would report as
// contested, and an annotation that fires constantly is one nobody reads.
//
// Most recently active first, then by stream token so that two checkouts
// last seen in the same instant still render in a stable order.
func contestants(holder model.Attribution, activity map[model.Attribution]time.Time, establishers map[model.Attribution]struct{}, fresh Freshness) []model.Attribution {
	contested := []model.Attribution{}
	for candidate := range establishers {
		if candidate == holder || !candidate.Present() || !fresh.Covers(activity[candidate]) {
			continue
		}
		contested = append(contested, candidate)
	}
	slices.SortFunc(contested, func(a, b model.Attribution) int {
		if order := activity[b].Compare(activity[a]); order != 0 {
			return order
		}
		return strings.Compare(a.Stream(), b.Stream())
	})
	return contested
}
