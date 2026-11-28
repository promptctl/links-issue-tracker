package cli

import (
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// This file is the command surface's half of the occasion layer: the pure
// builders of what each command fires — one for a ticket a command read or
// changed, one for the backlog, one for a created ticket, one for every
// transition — so the payload a command fires is unit-testable with no store,
// no app, no mocks. [LAW:effects-at-boundaries] A read or a single write fires
// through dispatchOccasion; a transition or a comment, which a bulk run makes
// once per ticket, fires through its command's loadedWorkflows.fire, which
// dispatchOccasion also calls. [LAW:one-source-of-truth]

// ticketOccasion is what a command fires for the one ticket it acted on: the
// event it fires, and the ticket's id and labels. `lit show` fires
// show_ticket, `lit next` next_pulled, `lit update` ticket_updated (status
// never moves through update) and `lit comment add` comment_added.
func ticketOccasion(event workflows.Event, issue model.Issue) workflows.Occasion {
	return workflows.Occasion{
		Moments: []workflows.Moment{event},
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

// createdOccasion is what `lit new` and `lit followup` fire once the ticket
// exists: ticket_created, then the entries into every state it starts in, as
// a statechart's initial transition enters its initial configuration — so a
// ticket created open enters open and live.
func createdOccasion(issue model.Issue) workflows.Occasion {
	occasion := ticketOccasion(workflows.EventTicketCreated, issue)
	occasion.Moments = append(occasion.Moments, crossings(nil, model.ConfigurationOf(issue, nil))...)
	return occasion
}

// ticketState is where a ticket stands at one moment: its row, which carries
// its status and retention, and the history read beside it, which carries its
// phase. [LAW:one-source-of-truth] everything else about where it stands is
// derived from those two, never stored beside them.
type ticketState struct {
	issue  model.Issue
	events []model.IssueEvent
}

// phase is the phase the ticket's history puts it in (model.PhaseOf), empty
// when it has none, which is every ticket not in progress.
func (t ticketState) phase() model.Phase {
	phase, _ := model.PhaseOf(t.issue, t.events)
	return phase
}

// configuration is every state node the ticket is in (model.ConfigurationOf).
func (t ticketState) configuration() []model.StateNode {
	return model.ConfigurationOf(t.issue, t.events)
}

// transitionOccasion is the single builder every transition routes through —
// applyTransition in cli.go is the one step every transition verb runs,
// single or bulk, and it calls this — so it is the one place that turns a
// completed transition into what it fires. [LAW:single-enforcer] before and
// after are where the ticket stood on either side of the write.
//
// What a transition fires is the crossings it made, never a name for the
// command: `lit done` and `lit close` both enter closed, and a definition
// bound to `enter closed` cannot tell them apart. So a start from open exits
// open and enters in_progress and plan, a done from review exits review and
// in_progress and enters closed, a phase move exits and enters one phase each,
// never leaving in_progress, and an archive exits live and enters archived. A
// transition that leaves the ticket where it was — a reclaim, a move to the
// phase it is in — crosses nothing, so only a definition bound by labels alone
// fires on it. [LAW:dataflow-not-control-flow] every transition builds its
// occasion the same way; what it crossed is the value that varies.
func transitionOccasion(before, after ticketState) workflows.Occasion {
	return workflows.Occasion{
		Moments: crossings(before.configuration(), after.configuration()),
		IssueID: after.issue.ID,
		Labels:  model.RawTexts(after.issue.Labels),
	}
}

// crossings are the moves from one configuration to another in the order a
// statechart makes them: the exits of every node from has and to lacks,
// innermost first, then the entries of every node to has and from lacks,
// outermost first. Each region's nodes run outermost first
// (model.ConfigurationOf), so a done from review exits review, then
// in_progress, then enters closed.
func crossings(from, to []model.StateNode) []workflows.Moment {
	var out []workflows.Moment
	for _, node := range slices.Backward(from) {
		if !slices.Contains(to, node) {
			out = append(out, workflows.Crossing{Side: workflows.Exit, State: node})
		}
	}
	for _, node := range to {
		if !slices.Contains(from, node) {
			out = append(out, workflows.Crossing{Side: workflows.Enter, State: node})
		}
	}
	return out
}
