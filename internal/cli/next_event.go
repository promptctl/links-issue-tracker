package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reachKindNames is how the invocation event spells each reachKind a walk can
// report. Indexed by the kind and bounded by reachTakeable, like reachNotes, and
// held total by TestEveryReachKindHasAnEventName.
var reachKindNames = [reachTakeable]string{
	reachHeldFresh:       "held_fresh",
	reachHeldByItself:    "held_by_itself",
	reachNotReady:        "not_ready",
	reachAwaitingOutside: "awaiting_outside",
}

// The event's reach for the entries of the two lists that are not kinds, as
// outOfViewNote and offFocusPathNote are their words.
const (
	outOfViewReach    = "out_of_view"
	offFocusPathReach = "off_focus_path"
)

// reachFact is one row a walk went past, as the event records it: the id the
// diagnostic named, the gathered ticket its kind was read from (the id itself,
// a ticket under it, or empty when the run could see none), and the kind.
type reachFact struct {
	ID     string `json:"id"`
	Ticket string `json:"ticket"`
	Reach  string `json:"reach"`
}

// progressFact is a stopped scope's progress as the event records it: the
// fields of claims.Progress under their wire names, converted rather than
// copied, so a field added to one and not the other stops the build.
// [LAW:one-source-of-truth]
type progressFact struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// fact is the row as the event records it — the one mapping every list of rows
// goes through. [LAW:one-source-of-truth]
func (r rowReach) fact() reachFact {
	return reachFact{ID: r.ID, Ticket: r.Row.ID, Reach: reachKindNames[r.Kind]}
}

func reachFacts(rows []rowReach) []reachFact {
	facts := make([]reachFact, len(rows))
	for i, row := range rows {
		facts[i] = row.fact()
	}
	return facts
}

// offPathFacts records the rows a focus label withheld. Each is its own
// gathered ticket.
func offPathFacts(ids []string) []reachFact {
	facts := make([]reachFact, len(ids))
	for i, id := range ids {
		facts[i] = reachFact{ID: id, Ticket: id, Reach: offFocusPathReach}
	}
	return facts
}

// blockerFact is one blocker as the event records it: the row it is, and
// whether a ticket filed here could free it.
type blockerFact struct {
	reachFact
	Clearable bool `json:"clearable"`
}

// blockerFacts records every blocker in walk order. One out of view has no
// ticket, since the run could see none.
func blockerFacts(blockers []blockerReach) []blockerFact {
	facts := make([]blockerFact, len(blockers))
	for i, blocker := range blockers {
		fact := reachFact{ID: blocker.ID, Reach: outOfViewReach}
		if row, ok := blocker.read(); ok {
			fact = row.fact()
		}
		facts[i] = blockerFact{reachFact: fact, Clearable: blocker.Clearable}
	}
	return facts
}

// nextDecision is what one routeNext answer puts on the invocation's event.
// Every field is recorded on every run, so an empty list says the walk named
// nothing of that kind, and a nil Picked says it picked nothing.
//
// DependencyPath is a dependency pick's whole chain: the row of ours it gates,
// every prerequisite between — an epic followed by the ticket under it the
// walk went through — then the dependency it is work toward. Its length is how
// deep the walk went to find the pick.
//
// HiddenByFilter is the ticket the run's filter left out when that is why the
// run served past its scope or served nothing — the one routing over the whole
// queue would have handed over.
//
// PickedFocusGoals is the focus goals whose prerequisite path the pool was
// narrowed to when the pool served the pick: empty for a pick over the whole
// queue, and for a pick from steps 1-2b, which no scope narrows.
// HiddenFocusGoals is the same fact about HiddenByFilter.
type nextDecision struct {
	Outcome          string
	Picked           *string
	PickedFocusGoals []string
	DependencyPath   []string
	Exhaustion       Exhausted
	HiddenByFilter   *string
	HiddenFocusGoals []string
	NoWork           NoWork
}

func decideNextEvent(outcome NextOutcome) nextDecision {
	picked := func(id string) *string { return &id }
	switch o := outcome.(type) {
	case ServedFromClaim:
		return nextDecision{Outcome: "served_from_claim", Picked: picked(o.Row.ID)}
	case ResumedOwnWork:
		return nextDecision{Outcome: "resumed_own_work", Picked: picked(o.Row.ID)}
	case ServedFromEpicLane:
		return nextDecision{Outcome: "served_from_epic_lane", Picked: picked(o.Row.ID)}
	case ServedFromNewLane:
		return nextDecision{Outcome: "served_from_new_lane", Picked: picked(o.Row.ID), PickedFocusGoals: o.Scope.goals}
	case ServedFromDependency:
		path := []string{o.Gates}
		for _, h := range o.Via {
			path = append(path, h.ids()...)
		}
		path = append(path, o.Blocker)
		return nextDecision{Outcome: "served_from_dependency", Picked: picked(o.Row.ID), DependencyPath: path}
	case ServedPastExhaustion:
		return nextDecision{Outcome: "served_past_exhaustion", Picked: picked(o.Row.ID), PickedFocusGoals: o.Scope.goals, Exhaustion: o.Exhaustion}
	case ServedPastNarrowing:
		return nextDecision{Outcome: "served_past_narrowing", Picked: picked(o.Row.ID), PickedFocusGoals: o.Scope.goals, HiddenByFilter: picked(o.Narrowing.Hidden.ID), HiddenFocusGoals: o.Narrowing.Scope.goals}
	case Exhausted:
		decision := nextDecision{Outcome: "exhausted", Exhaustion: o}
		if o.Narrowing != nil {
			decision.HiddenByFilter = picked(o.Narrowing.Hidden.ID)
			decision.HiddenFocusGoals = o.Narrowing.Scope.goals
		}
		return decision
	case Narrowed:
		return nextDecision{Outcome: "narrowed", HiddenByFilter: picked(o.Hidden.ID), HiddenFocusGoals: o.Scope.goals}
	case NoWork:
		return nextDecision{Outcome: "no_work", NoWork: o}
	}
	panic(fmt.Sprintf("decideNextEvent: unhandled NextOutcome %T", outcome))
}

// recordNextEvent puts what routeNext decided on the invocation's event: the
// outcome, the ticket it picked and the focus goals that narrowed the pool it
// was picked from, the chain a dependency pick was reached
// through, how far a stopped scope had come, the ticket a filter hid and the
// focus goals it was drawn under, how far the lanes this checkout holds had
// come within that scope, every
// row its diagnostics named with what this checkout may do about each and, for
// a blocker, whether a ticket filed here could free it, whether it offered the
// route that stays in the scope, where its pick is started beside the checkout
// kind that decided it, and how many rows it chose among — the ones the filter
// selects, though routing reads the whole queue. Its stdout says the same in
// prose, for the session that ran it; this is what is left of it afterwards.
// [LAW:nothing-unseen]
func recordNextEvent(ctx context.Context, outcome NextOutcome, gathered int, primaryClone bool) {
	decision := decideNextEvent(outcome)
	telemetry.Set(ctx, "next.outcome", decision.Outcome)
	telemetry.Set(ctx, "next.picked", decision.Picked)
	telemetry.Set(ctx, "next.picked_focus_goals", append([]string{}, decision.PickedFocusGoals...))
	telemetry.Set(ctx, "next.dependency_path", append([]string{}, decision.DependencyPath...))
	telemetry.Set(ctx, "next.exhausted_epics", append([]string{}, decision.Exhaustion.Epics...))
	telemetry.Set(ctx, "next.exhausted_progress", progressFact(decision.Exhaustion.Progress))
	telemetry.Set(ctx, "next.exhausted_own_progress", progressFact(decision.Exhaustion.OwnProgress))
	telemetry.Set(ctx, "next.blocked", blockerFacts(decision.Exhaustion.Blocked))
	telemetry.Set(ctx, "next.held", reachFacts(decision.Exhaustion.Held))
	// One outcome at most carries rows the focus withheld, and they are the same
	// fact on either, so they land on one key. [LAW:one-source-of-truth]
	telemetry.Set(ctx, "next.off_path", offPathFacts(slices.Concat(decision.Exhaustion.OffPath, decision.NoWork.OffPath)))
	telemetry.Set(ctx, "next.hidden_by_filter", decision.HiddenByFilter)
	telemetry.Set(ctx, "next.hidden_focus_goals", append([]string{}, decision.HiddenFocusGoals...))
	telemetry.Set(ctx, "next.unreachable", reachFacts(decision.NoWork.Unreachable))
	telemetry.Set(ctx, "next.stay_offered", len(decision.Exhaustion.stay()) > 0)
	telemetry.Set(ctx, "next.start_place", placeFor(outcome, primaryClone).eventName())
	telemetry.Set(ctx, "next.primary_clone", primaryClone)
	telemetry.Count(ctx, "next.rows_gathered", gathered)
}
