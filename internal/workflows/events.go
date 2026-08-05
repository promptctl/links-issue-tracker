package workflows

import "slices"

// Event is a named semantic moment in the work lifecycle. Commands dispatch
// events; workflow definitions bind to events, never to command names — so
// commands can be renamed freely while user definitions keep firing, as long
// as the event stays the same. The constants below are that stable contract.
// [LAW:one-source-of-truth] This catalog is the single home of event names;
// dispatch wires commands to these constants, never to fresh strings.
type Event string

const (
	// EventShowBacklog fires when the agent views the workable backlog
	// (today: `lit backlog`).
	EventShowBacklog Event = "show_backlog"
	// EventShowTicket fires when the agent views one ticket's details
	// (today: `lit show`).
	EventShowTicket Event = "show_ticket"
	// EventNextPulled fires when the agent asks for the next workable ticket
	// (today: `lit next`).
	EventNextPulled Event = "next_pulled"
	// EventWorkStarted fires when the agent claims a ticket and begins work
	// (today: `lit start`).
	EventWorkStarted Event = "work_started"
	// EventWorkFinished fires when claimed work finishes on the success path
	// (today: `lit done`).
	EventWorkFinished Event = "work_finished"
	// EventTicketClosed fires when a ticket is closed without finishing —
	// wontfix, obsolete, duplicate (today: `lit close`).
	EventTicketClosed Event = "ticket_closed"
	// EventTicketReopened fires when a closed ticket is reopened
	// (today: `lit open`).
	EventTicketReopened Event = "ticket_reopened"
	// EventTicketCreated fires when a new ticket is created
	// (today: `lit new`, `lit followup`).
	EventTicketCreated Event = "ticket_created"
	// EventTicketUpdated fires when an existing ticket's fields change
	// (today: `lit update`).
	EventTicketUpdated Event = "ticket_updated"
	// EventCommentAdded fires when a comment lands on a ticket
	// (today: `lit comment`).
	EventCommentAdded Event = "comment_added"
)

// Catalog returns every event lit can dispatch, in lifecycle display order:
// discovery, viewing, claiming, finishing, and mutation.
func Catalog() []Event {
	return []Event{
		EventShowBacklog,
		EventNextPulled,
		EventShowTicket,
		EventWorkStarted,
		EventWorkFinished,
		EventTicketClosed,
		EventTicketReopened,
		EventTicketCreated,
		EventTicketUpdated,
		EventCommentAdded,
	}
}

// Known reports whether the event is in this binary's catalog. A definition
// bound to an unknown event still loads — it may target a newer lit — but it
// can never fire here, which the loader surfaces as a warning.
func (e Event) Known() bool {
	return slices.Contains(Catalog(), e)
}
