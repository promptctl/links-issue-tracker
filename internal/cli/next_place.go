package cli

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// startPlace is where `lit next` tells a session to start the ticket it
// served. A lane's tickets are worked in the one checkout that holds it, and a
// checkout carries one epic at a time (design-docs/work-claims.md, GRANULARITY
// RULING), so the checkout is part of the answer, not only the ticket: a pick
// started from the wrong one makes a second claimant for its lane.
type startPlace int

const (
	// startNoPick is the place of an outcome that served no ticket.
	startNoPick startPlace = iota
	// startHere is this checkout's own work: the lane it holds, the epic it
	// carries, or a dependency on that epic's path.
	startHere
	// startOffPrimaryClone is new work served in the primary clone, where every
	// session starts, so work begun there is shared by all of them.
	startOffPrimaryClone
	// startBesideHeldWork is a pick from outside the work this checkout holds,
	// which is unfinished and stays here.
	startBesideHeldWork
)

// startPlaceNames is how the invocation event spells each place; the no-pick
// place records as null.
var startPlaceNames = map[startPlace]string{
	startHere:            "here",
	startOffPrimaryClone: "worktree_off_primary_clone",
	startBesideHeldWork:  "worktree_beside_held_work",
}

// startPlacement is the place for one served ticket.
type startPlacement struct {
	Place  startPlace
	Ticket string
}

// placeFor decides where a pick is started. It is total over the sealed
// NextOutcome sum and panics on a variant it does not name, as decideNextEvent
// does. [LAW:single-enforcer] this is the one place the decision is made; the
// /next skill and quickstart work tell a session to follow the line it prints.
//
// A new lane served in a linked worktree is started there: that worktree holds
// nothing — its epic is done or its claim lapsed — so it carries the new work.
func placeFor(outcome NextOutcome, primaryClone bool) startPlacement {
	newLane := startHere
	if primaryClone {
		newLane = startOffPrimaryClone
	}
	switch o := outcome.(type) {
	case ServedFromClaim:
		return startPlacement{Place: startHere, Ticket: o.Row.ID}
	case ResumedOwnWork:
		return startPlacement{Place: startHere, Ticket: o.Row.ID}
	case ServedFromEpicLane:
		return startPlacement{Place: startHere, Ticket: o.Row.ID}
	case ServedFromDependency:
		return startPlacement{Place: startHere, Ticket: o.Row.ID}
	case ServedFromNewLane:
		return startPlacement{Place: newLane, Ticket: o.Row.ID}
	case ServedPastExhaustion:
		return startPlacement{Place: startBesideHeldWork, Ticket: o.Row.ID}
	case ServedPastNarrowing:
		return startPlacement{Place: startBesideHeldWork, Ticket: o.Row.ID}
	case Exhausted, Narrowed, NoWork:
		return startPlacement{Place: startNoPick}
	}
	panic(fmt.Sprintf("placeFor: unhandled NextOutcome %T", outcome))
}

// startPlaceReasons is why a pick goes to a worktree of its own, said after
// "not". A place absent here prints no line: work started where `lit next` ran
// needs no direction.
var startPlaceReasons = map[startPlace]string{
	startOffPrimaryClone: "the primary clone, where every session starts",
	startBesideHeldWork:  "this checkout, whose own work is unfinished",
}

// note is the line `lit next` prints under a pick that belongs in a worktree
// of its own, and the empty string for every other place.
func (p startPlacement) note() string {
	reason, own := startPlaceReasons[p.Place]
	if !own {
		return ""
	}
	return fmt.Sprintf("start %s from a worktree of its own, not %s: create one off the trunk and run `lit start %s` from inside it — the claim lands on the checkout `lit start` runs in\n", p.Ticket, reason, p.Ticket)
}

// recordStartPlace puts where the pick goes on the invocation's event, beside
// the checkout kind that decided it. [LAW:nothing-unseen]
func recordStartPlace(ctx context.Context, placement startPlacement, primaryClone bool) {
	var place *string
	if name, picked := startPlaceNames[placement.Place]; picked {
		place = &name
	}
	telemetry.Set(ctx, "next.start_place", place)
	telemetry.Set(ctx, "next.primary_clone", primaryClone)
}
