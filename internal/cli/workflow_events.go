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

// statusTransitionEvents maps the four status-changing actions to the events
// they fire. [LAW:one-source-of-truth] mirrors events.go's catalog exactly.
var statusTransitionEvents = map[model.ActionName]workflows.Event{
	model.ActionStart:  workflows.EventWorkStarted,
	model.ActionDone:   workflows.EventWorkFinished,
	model.ActionClose:  workflows.EventTicketClosed,
	model.ActionReopen: workflows.EventTicketReopened,
}

// ticketState is where a ticket stands at one moment: its row, which carries
// its status, and the phase its history puts it in — empty when it has none,
// which is every ticket not in progress.
type ticketState struct {
	issue model.Issue
	phase model.Phase
}

// states is every state the ticket is in, outermost first: its status, then
// the phase inside it.
func (t ticketState) states() []string {
	return append([]string{string(t.issue.State())}, t.phases()...)
}

// phases is the ticket's phase as the states of the phase level: one, or none.
func (t ticketState) phases() []string {
	if t.phase == "" {
		return nil
	}
	return []string{string(t.phase)}
}

// transitionOccasion is the single builder every lifecycle transition routes
// through — transitionLeaf in cli.go is already the one enforcer of the
// transition itself; this is the one place that turns a completed transition
// into the event it fires. [LAW:single-enforcer] before and after are where
// the ticket stood on either side of the write.
//
// The states a transition exits and enters are read as a statechart reads
// nested states, a phase being a state inside in_progress. A status action
// moves the whole ticket, so it exits every state the ticket was in and
// enters every state it is in now: a start from open enters in_progress and
// plan, a done from review exits in_progress and review. A phase move stays
// inside in_progress, so it exits and enters phases alone. A move to where the
// ticket already is re-enters it, as a reclaim re-enters in_progress.
//
// A retention action (archive/unarchive/delete/restore) is no lifecycle
// transition and fires nothing, so fires is false. Action is a sealed sum, so
// a variant this switch does not name is one added to the lifecycle package
// without a decision here — a maintenance bug, not a state transitionLeaf can
// construct today — and it panics rather than dispatching or skipping
// silently, as an unmapped status action does. [LAW:no-silent-failure]
func transitionOccasion(action model.Action, before, after ticketState) (occasion workflows.Occasion, fires bool) {
	switch action := action.(type) {
	case model.StatusAction:
		event, ok := statusTransitionEvents[action.Name()]
		if !ok {
			// string(), not Verb(): this names the EVENT MAPPING's key, which is the
			// persisted encoding, and it is a developer diagnostic rather than
			// guidance an agent will run. The conversion is written out so the gate
			// in internal/model/lifecycle can hold one rule with no exceptions.
			panic(fmt.Sprintf("workflow_events: no event mapped for status action %q", string(action.Name())))
		}
		return movedOccasion(event, after.issue, before.states(), after.states()), true
	case model.SetPhase:
		return movedOccasion(workflows.EventPhaseSet, after.issue, before.phases(), after.phases()), true
	case model.RetentionAction:
		return workflows.Occasion{}, false
	}
	panic(fmt.Sprintf("workflow_events: no occasion decided for action %T", action))
}

// movedOccasion is the occasion of a transition that moved issue out of the
// exited states and into the entered ones.
func movedOccasion(event workflows.Event, issue model.Issue, exited, entered []string) workflows.Occasion {
	return workflows.Occasion{
		Event:   event,
		IssueID: issue.ID,
		Labels:  issue.Labels,
		Entered: entered,
		Exited:  exited,
	}
}
