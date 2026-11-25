package cli

import (
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// This file is the command surface's half of the occasion layer: one pure
// occasion-builder function per read moment, and one for every transition,
// so the payload a command hands to workflows.Fire is unit-testable with
// no store, no app, no mocks. [LAW:effects-at-boundaries] The commands
// themselves (cli.go, workable.go, next.go) call these and dispatch the
// result, through dispatchOccasion, at the point their mutation or read
// already succeeded — a rename of the command touches only its registration,
// never the moment it fires. [LAW:one-source-of-truth]

// showTicketOccasion is what `lit show` fires: it always names one ticket, so
// labels travel with it, and viewing carries no state transition of its own.
func showTicketOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Moments: []workflows.Moment{workflows.EventShowTicket},
		IssueID: issue.ID,
		Labels:  model.RawTexts(issue.Labels),
	}
}

// backlogOccasion is what `lit backlog` fires: a canonical example of a
// moment with no single acted-on ticket (Occasion's own doc comment names
// this exact case), so it carries no IssueID or Labels.
func backlogOccasion() workflows.Occasion {
	return workflows.Occasion{Moments: []workflows.Moment{workflows.EventShowBacklog}}
}

// nextPulledOccasion is what `lit next` fires when it has a ticket to hand
// back — the only case it is ever called, since the empty-selection path
// returns an error before any occasion is built.
func nextPulledOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Moments: []workflows.Moment{workflows.EventNextPulled},
		IssueID: issue.ID,
		Labels:  model.RawTexts(issue.Labels),
	}
}

// ticketCreatedOccasion is what `lit new` and `lit followup` fire once the
// issue exists, carrying whatever labels it was created with.
func ticketCreatedOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Moments: []workflows.Moment{workflows.EventTicketCreated},
		IssueID: issue.ID,
		Labels:  model.RawTexts(issue.Labels),
	}
}

// ticketUpdatedOccasion is what `lit update` fires. Status never moves
// through update (runUpdate rejects --status), so this never carries a
// crossing.
func ticketUpdatedOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Moments: []workflows.Moment{workflows.EventTicketUpdated},
		IssueID: issue.ID,
		Labels:  model.RawTexts(issue.Labels),
	}
}

// commentAddedOccasion is what `lit comment add` fires, naming the ticket the
// comment landed on.
func commentAddedOccasion(issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Moments: []workflows.Moment{workflows.EventCommentAdded},
		IssueID: issue.ID,
		Labels:  model.RawTexts(issue.Labels),
	}
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

// transitionOccasion is the single builder every transition routes through —
// applyTransition in cli.go is the one step every transition verb runs,
// single or bulk, and it calls this — so it is the one place that turns a
// completed transition into what it fires. [LAW:single-enforcer] before and
// after are where the ticket stood on either side of the write.
//
// What a transition fires is the crossings it made, never a name for the
// command: `lit done` and `lit close` both enter closed, and a definition
// bound to `enter closed` cannot tell them apart. The states are read as a
// statechart reads nested states, a phase being a state inside in_progress:
// a transition exits the states the ticket was in and no longer is, and
// enters the ones it is in now and was not — exits first, then entries, each
// outermost first. So a start from open exits open and enters in_progress and
// plan, a done from review exits in_progress and review and enters closed,
// and a phase move exits and enters one phase each, never leaving
// in_progress. A transition that leaves the ticket's state where it was — a
// reclaim, a move to the phase it is in, an archive or delete or their
// undoing — crosses nothing, so only a definition bound by labels alone fires
// on it. [LAW:dataflow-not-control-flow] every transition builds its occasion
// the same way; what it crossed is the value that varies.
func transitionOccasion(before, after ticketState) workflows.Occasion {
	return workflows.Occasion{
		Moments: append(crossings(workflows.Exit, before.states(), after.states()), crossings(workflows.Enter, after.states(), before.states())...),
		IssueID: after.issue.ID,
		Labels:  model.RawTexts(after.issue.Labels),
	}
}

// crossings are the side's crossings of every state in from that is not in
// to, kept in from's order.
func crossings(side workflows.Side, from, to []string) []workflows.Moment {
	var out []workflows.Moment
	for _, state := range from {
		if !slices.Contains(to, state) {
			out = append(out, workflows.Crossing{Side: side, State: state})
		}
	}
	return out
}
