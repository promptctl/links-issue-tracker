package workflows

import "slices"

// Event names a read moment: lit showing or recording something that changes
// no ticket's state. A change of state is never an event; it is the Crossing
// it makes, whichever command makes it, so `lit done` and `lit close` reach
// one `enter closed` and no definition can tell the commands apart.
// Commands fire events; workflow definitions name events, never command
// names — so commands can be renamed freely while user definitions keep
// firing, as long as the event stays the same. The constants below are that
// stable contract.
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

// Catalog returns every event lit can fire, in display order: discovery,
// viewing, then mutation.
func Catalog() []Event {
	return []Event{
		EventShowBacklog,
		EventNextPulled,
		EventShowTicket,
		EventTicketCreated,
		EventTicketUpdated,
		EventCommentAdded,
	}
}

func (e Event) String() string { return string(e) }

// Known reports whether the event is in this binary's catalog. A definition
// naming an unknown event still loads — it may target a newer lit — but that
// entry can never fire here, which the loader surfaces as a warning.
func (e Event) Known() bool {
	return slices.Contains(Catalog(), e)
}

func (Event) moment() {}
