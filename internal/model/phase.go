package model

import (
	"cmp"
	"fmt"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model/lifecycle"
)

type Phase = lifecycle.Phase
type SetPhase = lifecycle.SetPhase

const (
	PhasePlan   = lifecycle.PhasePlan
	PhaseImpl   = lifecycle.PhaseImpl
	PhaseReview = lifecycle.PhaseReview
	PhaseMerge  = lifecycle.PhaseMerge

	ActionPhase = lifecycle.ActionPhase
)

var (
	Phases     = lifecycle.Phases
	ParsePhase = lifecycle.ParsePhase
	JoinPhases = lifecycle.JoinPhases
)

// PhaseField is the change-row field a phase move records, on an event whose
// action is ActionPhase.
const PhaseField = "phase"

// CompareEventRecency is the total order over events lit means by "newer":
// when it happened, then by id to break a tie — arbitrary, but total and
// stable, which is what a predicate reading "the latest" needs. Claims and
// phases both read the latest of an issue's events, and one order keeps them
// agreeing about which event that is. [LAW:single-enforcer]
func CompareEventRecency(a, b IssueEvent) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
}

// NextEventInstant is the instant an event written at now takes when the
// history it was written against is seen: now, unless some event in seen is at
// or after it, and then the instant just past the newest of them. It is a
// hybrid logical clock: the wall clock, held back from ever running behind the
// history a write observed. CompareEventRecency orders by the stamp, so a clone
// whose clock runs behind would otherwise write an event that sorts before
// history it already synced and planned against, and the phase and the lane
// holder would both be read off the older event. The nanosecond step also
// parts a write from an event stamped at the same instant, which the id
// tie-break orders arbitrarily. [LAW:single-enforcer] the one statement of the
// rule both engines stamp by.
func NextEventInstant(now time.Time, seen []IssueEvent) time.Time {
	at := now
	for _, event := range seen {
		if !event.CreatedAt.Before(at) {
			at = event.CreatedAt.Add(time.Nanosecond)
		}
	}
	return at
}

// PhaseOf is the phase issue's own history puts it in. Only an in-progress
// leaf has one. The phase is never stored: it is the newest of the events
// that set it, which are the status moves into in_progress (each enters plan)
// and the phase moves after them. A same-state reclaim moves no status, so a
// takeover keeps the phase the work had reached; a ticket started by a lit
// that recorded no phases reads as plan. A phase is read back as recorded, so
// one a newer lit wrote under a name this lit does not know reads as that
// name rather than as an older phase it has left. [LAW:one-source-of-truth]
func PhaseOf(issue Issue, events []IssueEvent) (Phase, bool) {
	caps := issue.Capabilities()
	if caps.Status == nil || caps.Status.Value != StateInProgress {
		return "", false
	}
	phase := PhasePlan
	var latest IssueEvent
	found := false
	for _, event := range events {
		set, ok := phaseSetBy(event)
		if ok && (!found || CompareEventRecency(latest, event) < 0) {
			latest, phase, found = event, set, true
		}
	}
	return phase, true
}

// phaseSetBy is the phase an event puts its issue in, if it sets one: a start
// into in_progress enters plan, and a phase move enters the phase it names.
func phaseSetBy(event IssueEvent) (Phase, bool) {
	for _, change := range event.Changes {
		if change.Field == "status" && change.To == string(StateInProgress) {
			return PhasePlan, true
		}
		if event.Action == string(ActionPhase) && change.Field == PhaseField {
			return Phase(change.To), true
		}
	}
	return "", false
}

// PhaseChange is a planned phase move: the phase the ticket is in and the one
// it moves to. From equal to To is the documented no-op: history records
// mutations that happened, not calls that were made.
type PhaseChange struct {
	From Phase
	To   Phase
}

// Changed reports whether the move records anything.
func (c PhaseChange) Changed() bool { return c.From != c.To }

// FieldChanges are the change rows the move's event records.
func (c PhaseChange) FieldChanges() []FieldChange {
	return []FieldChange{{Field: PhaseField, From: string(c.From), To: string(c.To)}}
}

// PlanPhase plans moving i to phase to, given i's own history. It refuses a
// ticket that has no phase — an epic, or a leaf that is not in progress — and
// one that has left the flow. Any phase may follow any other: review back to
// impl after findings is routine work, not an error. Having no phase is
// refused first: its remedy is `lit start`, whose own gate names the thaw a
// frozen ticket also needs, so each refusal names the next step that works.
// [LAW:single-enforcer] Both storage engines plan a phase move here, so the
// rule and its refusals have one home.
func (i Issue) PlanPhase(events []IssueEvent, to Phase) (PhaseChange, error) {
	from, ok := PhaseOf(i, events)
	if !ok {
		return PhaseChange{}, ValidationError{Message: phaseRefusal(i)}
	}
	if thaw, frozen := lifecycle.Thaw(i.Retention()); frozen {
		return PhaseChange{}, ValidationError{Message: fmt.Sprintf("issue %s is %s, so its phase cannot change: run `lit %s %s` first", i.ID, lifecycle.RetentionName(i.Retention()), thaw.Name().Verb(), i.ID)}
	}
	return PhaseChange{From: from, To: to}, nil
}

func phaseRefusal(i Issue) string {
	caps := i.Capabilities()
	if caps.Status == nil {
		return fmt.Sprintf("cannot set the phase of epic %s: an epic's state derives from its children, and only an in-progress ticket has a phase", i.ID)
	}
	return fmt.Sprintf("cannot set the phase of %s: it is %s, and only an in-progress ticket has a phase — `lit start %s` enters plan", i.ID, caps.Status.Value.Display(), i.ID)
}
