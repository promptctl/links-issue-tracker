package cli

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reachKindNames is how the invocation event spells each reachKind. Indexed by
// the kind, like reachNotes, and held total by TestEveryReachKindHasAnEventName.
var reachKindNames = [reachKindCount]string{
	reachTakeable:        "takeable",
	reachHeldFresh:       "held_fresh",
	reachNotReady:        "not_ready",
	reachAwaitingOutside: "awaiting_outside",
	reachOutOfView:       "out_of_view",
	reachOffFocusPath:    "off_focus_path",
}

// reachFact is one row a walk went past, as the event records it: the id the
// diagnostic named, the gathered ticket its kind was read from (the id itself,
// a ticket under it, or empty when the run could see none), and the kind.
type reachFact struct {
	ID     string `json:"id"`
	Ticket string `json:"ticket"`
	Reach  string `json:"reach"`
}

func reachFacts(rows []rowReach) []reachFact {
	facts := make([]reachFact, len(rows))
	for i, row := range rows {
		facts[i] = reachFact{ID: row.ID, Ticket: row.Row.ID, Reach: reachKindNames[row.Kind]}
	}
	return facts
}

// nextDecision is what one routeNext answer puts on the invocation's event.
// Every field is recorded on every run, so an empty list says the walk named
// nothing of that kind, and a nil Picked says it picked nothing.
//
// DependencyPath is a dependency pick's whole chain: the row of ours it gates,
// every dependency between, then the dependency it is work toward. Its length
// is how deep the walk went to find the pick.
type nextDecision struct {
	Outcome        string
	Picked         *string
	DependencyPath []string
	Exhaustion     Exhausted
	Unreachable    []rowReach
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
		return nextDecision{Outcome: "served_from_new_lane", Picked: picked(o.Row.ID)}
	case ServedFromDependency:
		path := append(append([]string{o.Gates}, o.Via...), o.Blocker)
		return nextDecision{Outcome: "served_from_dependency", Picked: picked(o.Row.ID), DependencyPath: path}
	case ServedPastExhaustion:
		return nextDecision{Outcome: "served_past_exhaustion", Picked: picked(o.Row.ID), Exhaustion: o.Exhaustion}
	case Exhausted:
		return nextDecision{Outcome: "exhausted", Exhaustion: o}
	case NoWork:
		return nextDecision{Outcome: "no_work", Unreachable: o.Unreachable}
	}
	panic(fmt.Sprintf("decideNextEvent: unhandled NextOutcome %T", outcome))
}

// recordNextEvent puts what routeNext decided on the invocation's event: the
// outcome, the ticket it picked and the chain a dependency pick was reached
// through, every row its diagnostics named with what this checkout may do about
// each, whether it offered the route that stays in the scope, and how many rows
// it routed over. Its stdout says the same in prose,
// for the session that ran it; this is what is left of it afterwards.
// [LAW:nothing-unseen]
func recordNextEvent(ctx context.Context, outcome NextOutcome, gathered int) {
	decision := decideNextEvent(outcome)
	telemetry.Set(ctx, "next.outcome", decision.Outcome)
	telemetry.Set(ctx, "next.picked", decision.Picked)
	telemetry.Set(ctx, "next.dependency_path", append([]string{}, decision.DependencyPath...))
	telemetry.Set(ctx, "next.exhausted_epics", append([]string{}, decision.Exhaustion.Epics...))
	telemetry.Set(ctx, "next.blocked", reachFacts(decision.Exhaustion.Blocked))
	telemetry.Set(ctx, "next.held", reachFacts(decision.Exhaustion.Held))
	telemetry.Set(ctx, "next.off_path", reachFacts(decision.Exhaustion.OffPath))
	telemetry.Set(ctx, "next.unreachable", reachFacts(decision.Unreachable))
	telemetry.Set(ctx, "next.stay_offered", len(decision.Exhaustion.stay()) > 0)
	telemetry.Count(ctx, "next.rows_gathered", gathered)
}
