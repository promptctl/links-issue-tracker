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

// placeFor decides where a pick is started. It is total over the sealed
// NextOutcome sum and panics on a variant it does not name, as decideNextEvent
// does. [LAW:single-enforcer] this is the one place the decision is made; the
// /next skill and quickstart work tell a session to follow the start advice it
// shapes.
//
// A new lane served in a linked worktree is started there: that worktree holds
// nothing — its epic is done or its claim lapsed — so it carries the new work.
func placeFor(outcome NextOutcome, primaryClone bool) startPlace {
	newLane := startHere
	if primaryClone {
		newLane = startOffPrimaryClone
	}
	switch outcome.(type) {
	case ServedFromClaim, ResumedOwnWork, ServedFromEpicLane, ServedFromDependency:
		return startHere
	case ServedFromNewLane:
		return newLane
	case ServedPastExhaustion, ServedPastNarrowing:
		return startBesideHeldWork
	case Exhausted, Narrowed, NoWork:
		return startNoPick
	}
	panic(fmt.Sprintf("placeFor: unhandled NextOutcome %T", outcome))
}

// where is the clause that opens a pick's start advice, so the command and the
// checkout it runs in are one instruction: empty for a pick started here, and
// for one that leaves, the worktree to run `lit start` from — with why, said
// after "not" — since the claim lands on the checkout `lit start` runs in.
// [LAW:no-silent-failure] every place is named, as in placeFor, so a place
// added later cannot print the plain advice by omission.
func (p startPlace) where() string {
	leave := func(reason string) string {
		return fmt.Sprintf("from a worktree of its own off the trunk (not %s), ", reason)
	}
	switch p {
	case startNoPick, startHere:
		return ""
	case startOffPrimaryClone:
		return leave("the primary clone, where every session starts")
	case startBesideHeldWork:
		return leave("this checkout, whose own work is unfinished")
	}
	panic(fmt.Sprintf("startPlace.where: unhandled place %d", p))
}

// eventName is how the invocation event spells the place; the no-pick place
// records as null.
func (p startPlace) eventName() *string {
	spell := func(name string) *string { return &name }
	switch p {
	case startNoPick:
		return nil
	case startHere:
		return spell("here")
	case startOffPrimaryClone:
		return spell("worktree_off_primary_clone")
	case startBesideHeldWork:
		return spell("worktree_beside_held_work")
	}
	panic(fmt.Sprintf("startPlace.eventName: unhandled place %d", p))
}

// recordStartPlace puts where the pick goes on the invocation's event, beside
// the checkout kind that decided it. [LAW:nothing-unseen]
func recordStartPlace(ctx context.Context, place startPlace, primaryClone bool) {
	telemetry.Set(ctx, "next.start_place", place.eventName())
	telemetry.Set(ctx, "next.primary_clone", primaryClone)
}
