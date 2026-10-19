package cli

import (
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// This file is the command surface's half of the event layer described in
// promptctl-orchestration-ffqz.2: one pure occasion-builder function per
// semantic event, so the payload a command hands to workflows.Dispatch is
// unit-testable with no store, no app, no mocks. [LAW:effects-at-boundaries]
// The commands themselves (cli.go, workable.go) call these and dispatch the
// result, through dispatchOccasion, at the point their mutation or read
// already succeeded — a rename of the command touches only its registration,
// never the event it fires.
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
// transition — Entered/Exited stay empty.
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

// transitionEvents maps every lifecycle action to the event it fires: the
// four status actions and the phase move. Retention actions fire nothing and
// have no entry (see transitionOccasion).
var transitionEvents = map[model.ActionName]workflows.Event{
	model.ActionStart:  workflows.EventWorkStarted,
	model.ActionPhase:  workflows.EventPhaseSet,
	model.ActionDone:   workflows.EventWorkFinished,
	model.ActionClose:  workflows.EventTicketClosed,
	model.ActionReopen: workflows.EventTicketReopened,
}

// ticketState is where a ticket stands at one moment: its row, which carries
// its status, the history read beside it, and the phase that history puts it
// in — empty when it has none, which is every ticket not in progress.
type ticketState struct {
	issue  model.Issue
	events []model.IssueEvent
	phase  model.Phase
}

// states is every state the ticket is in, outermost first: its status, then
// the phase inside it.
func (t ticketState) states() []string {
	states := []string{string(t.issue.State())}
	if t.phase != "" {
		states = append(states, string(t.phase))
	}
	return states
}

// transitionOccasion is the single builder every lifecycle transition routes
// through — transitionLeaf in cli.go is already the one enforcer of the
// transition itself; this is the one place that turns a completed transition
// into the event it fires. [LAW:single-enforcer] before and after are where
// the ticket stood on either side of the write.
//
// The states are read as a statechart reads nested states, a phase being a
// state inside in_progress: a transition exits the states the ticket was in
// and no longer is, and enters the ones it is in now and was not. So a start
// from open enters in_progress and plan, a done from review exits in_progress
// and review, and a phase move exits and enters one phase each, never leaving
// in_progress. A call that leaves the ticket where it was — a reclaim, a move
// to the phase it is in — exits and enters nothing, while its event still
// fires: history records mutations that happened, not calls that were made.
//
// A retention action (archive/unarchive/delete/restore) is no lifecycle
// transition and fires nothing, so fires is false. Any other action without
// an entry in transitionEvents is a variant added to the lifecycle package
// without a decision here — a maintenance bug, not a state transitionLeaf can
// construct today — so it panics rather than dispatching an eventless
// occasion or skipping silently. [LAW:no-silent-failure]
func transitionOccasion(action model.Action, before, after ticketState) (occasion workflows.Occasion, fires bool) {
	if _, retention := action.(model.RetentionAction); retention {
		return workflows.Occasion{}, false
	}
	event, ok := transitionEvents[action.Name()]
	if !ok {
		// string(), not Verb(): this names the EVENT MAPPING's key, which is the
		// persisted encoding, and it is a developer diagnostic rather than
		// guidance an agent will run. The conversion is written out so the gate
		// in internal/model/lifecycle can hold one rule with no exceptions.
		panic(fmt.Sprintf("workflow_events: no event mapped for lifecycle action %q", string(action.Name())))
	}
	return workflows.Occasion{
		Event:   event,
		IssueID: after.issue.ID,
		Labels:  after.issue.Labels,
		Exited:  difference(before.states(), after.states()),
		Entered: difference(after.states(), before.states()),
	}, true
}

// difference is the set difference from \ to, kept in from's order.
func difference(from, to []string) []string {
	return slices.DeleteFunc(slices.Clone(from), func(state string) bool { return slices.Contains(to, state) })
}
