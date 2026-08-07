package cli

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// This file is the command surface's half of the event layer described in
// promptctl-orchestration-ffqz.2: one pure occasion-builder function per
// semantic event, so the payload a command hands to workflows.Dispatch is
// unit-testable with no store, no app, no mocks. [LAW:effects-at-boundaries]
// The commands themselves (cli.go, workable.go) call these and dispatch the
// result at the point their mutation or read already succeeded — a rename of
// the command touches only its registration, never the event it fires.
// [LAW:one-source-of-truth]

// showTicketOccasion is what `lit show` fires: it always names one ticket, so
// labels travel with it, and viewing carries no state transition of its own.
func showTicketOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Event:   workflows.EventShowTicket,
		IssueID: issue.ID,
		Labels:  issue.Labels,
	}
}

// backlogOccasion is what `lit backlog` fires: a canonical example of a
// moment with no single acted-on ticket (Occasion's own doc comment names
// this exact case), so it carries no IssueID or Labels.
func backlogOccasion() workflows.Occasion {
	return workflows.Occasion{Event: workflows.EventShowBacklog}
}

// nextPulledOccasion is what `lit next` fires when it has a ticket to hand
// back — the only case it is ever called, since the empty-selection path
// returns an error before any occasion is built.
func nextPulledOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Event:   workflows.EventNextPulled,
		IssueID: issue.ID,
		Labels:  issue.Labels,
	}
}

// ticketCreatedOccasion is what `lit new` and `lit followup` fire once the
// issue exists, carrying whatever labels it was created with.
func ticketCreatedOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Event:   workflows.EventTicketCreated,
		IssueID: issue.ID,
		Labels:  issue.Labels,
	}
}

// ticketUpdatedOccasion is what `lit update` fires. Status never moves
// through update (runUpdate rejects --status), so this never carries a
// transition — Entered/Exited stay zero.
func ticketUpdatedOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Event:   workflows.EventTicketUpdated,
		IssueID: issue.ID,
		Labels:  issue.Labels,
	}
}

// commentAddedOccasion is what `lit comment add` fires, naming the ticket the
// comment landed on.
func commentAddedOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Event:   workflows.EventCommentAdded,
		IssueID: issue.ID,
		Labels:  issue.Labels,
	}
}

// statusTransitionEvents maps the four status-changing actions to the events
// they fire. Archive/Unarchive/Delete/Restore are RetentionActions, not
// StatusActions, so they cannot reach this map's caller in the first place
// (see transitionOccasion) — there is no "no event" entry to maintain here.
// [LAW:one-source-of-truth] mirrors events.go's catalog exactly.
var statusTransitionEvents = map[model.ActionName]workflows.Event{
	model.ActionStart:  workflows.EventWorkStarted,
	model.ActionDone:   workflows.EventWorkFinished,
	model.ActionClose:  workflows.EventTicketClosed,
	model.ActionReopen: workflows.EventTicketReopened,
}

// transitionOccasion is the single builder every status transition
// (start/done/close/open) routes through — runTransition in cli.go is
// already the one enforcer of the transition itself; this is the one place
// that turns a completed transition into the event it fires.
// [LAW:single-enforcer] prior and issue are the pre- and post-transition
// reads runTransition already has on hand, so no extra store round-trip.
//
// action.Name() is guaranteed to be a key in statusTransitionEvents: both are
// sealed to the lifecycle package, and its four StatusAction implementations
// are exactly this map's four keys. A miss means a StatusAction variant was
// added there without a matching entry here — a maintenance bug, not a state
// runTransition can construct today — so it panics rather than dispatching
// an eventless occasion. [LAW:no-silent-failure]
func transitionOccasion(action model.StatusAction, prior, issue model.Issue) workflows.Occasion {
	event, ok := statusTransitionEvents[action.Name()]
	if !ok {
		panic(fmt.Sprintf("workflow_events: no event mapped for status action %q", action.Name()))
	}
	return workflows.Occasion{
		Event:   event,
		IssueID: issue.ID,
		Labels:  issue.Labels,
		Entered: string(issue.State()),
		Exited:  string(prior.State()),
	}
}
