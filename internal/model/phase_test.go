package model

import (
	"errors"
	"testing"
	"time"
)

var phaseClock = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// at is the n-th instant of a history, so a case states its order by number.
func at(n int) time.Time { return phaseClock.Add(time.Duration(n) * time.Minute) }

func startEvent(id string, n int) IssueEvent {
	return IssueEvent{ID: id, Action: string(ActionStart), CreatedAt: at(n), Changes: []FieldChange{
		{Field: "assignee", From: "", To: "ada"},
		{Field: "status", From: "open", To: "in_progress"},
	}}
}

// reclaimEvent is a same-state start: a new holder, no status move.
func reclaimEvent(id string, n int) IssueEvent {
	return IssueEvent{ID: id, Action: string(ActionStart), CreatedAt: at(n), Changes: []FieldChange{
		{Field: "assignee", From: "ada", To: "grace"},
	}}
}

func phaseEvent(id string, n int, from, to Phase) IssueEvent {
	return IssueEvent{ID: id, Action: string(ActionPhase), CreatedAt: at(n), Changes: []FieldChange{
		{Field: PhaseField, From: string(from), To: string(to)},
	}}
}

// TestPhaseOf derives every case from PhaseOf's rule: only an in-progress leaf
// has a phase, and it is the one set by the newest event that sets one — a
// status move into in_progress sets plan, a phase move sets its phase — with
// plan when nothing has set one.
func TestPhaseOf(t *testing.T) {
	inProgress := func(t *testing.T) Issue {
		return hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateInProgress)
	}
	cases := []struct {
		name    string
		issue   func(t *testing.T) Issue
		events  []IssueEvent
		want    Phase
		wantHas bool
	}{
		{"an open leaf has none, whatever its history", func(t *testing.T) Issue {
			return hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateOpen)
		}, []IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseReview)}, "", false},
		{"a closed leaf has none, whatever its history", func(t *testing.T) Issue {
			return hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateClosed)
		}, []IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseReview)}, "", false},
		{"an in-progress epic has none", func(t *testing.T) Issue {
			child := hydratedIssue(t, Issue{ID: "c", IssueType: "task"}, StateInProgress)
			epic, err := HydrateAllOf(Issue{ID: "E", IssueType: "epic"}, []Issue{child})
			if err != nil {
				t.Fatalf("HydrateAllOf() error = %v", err)
			}
			return epic
		}, []IssueEvent{phaseEvent("e1", 1, PhasePlan, PhaseReview)}, "", false},
		{"in progress with no history reads as plan", inProgress, nil, PhasePlan, true},
		{"a start enters plan", inProgress, []IssueEvent{startEvent("e1", 1)}, PhasePlan, true},
		{"a phase move after the start sets its phase", inProgress,
			[]IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseImpl)}, PhaseImpl, true},
		{"a later start resets to plan", inProgress,
			[]IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseMerge), startEvent("e3", 3)}, PhasePlan, true},
		{"a reclaim moves no status and keeps the phase", inProgress,
			[]IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseReview), reclaimEvent("e3", 3)}, PhaseReview, true},
		{"the newest wins whatever order the slice holds", inProgress,
			[]IssueEvent{phaseEvent("e3", 3, PhaseImpl, PhaseReview), startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseImpl)}, PhaseReview, true},
		// Both orders, so neither "the first found wins" nor "the last found
		// wins" passes for the id rule.
		{"a tie on time goes to the greater id", inProgress,
			[]IssueEvent{phaseEvent("e9", 2, PhasePlan, PhaseMerge), phaseEvent("e1", 2, PhasePlan, PhaseImpl)}, PhaseMerge, true},
		{"a tie on time goes to the greater id, listed the other way", inProgress,
			[]IssueEvent{phaseEvent("e1", 2, PhasePlan, PhaseImpl), phaseEvent("e9", 2, PhasePlan, PhaseMerge)}, PhaseMerge, true},
		{"a phase change row on an event that is not a phase move sets nothing", inProgress,
			[]IssueEvent{startEvent("e1", 1), {ID: "e2", CreatedAt: at(2), Changes: []FieldChange{{Field: PhaseField, From: "plan", To: "merge"}}}}, PhasePlan, true},
		{"a phase a newer lit named reads as recorded", inProgress,
			[]IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, "design")}, Phase("design"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, has := PhaseOf(tc.issue(t), tc.events)
			if got != tc.want || has != tc.wantHas {
				t.Errorf("PhaseOf() = (%q, %v), want (%q, %v)", got, has, tc.want, tc.wantHas)
			}
		})
	}
}

func TestPlanPhase(t *testing.T) {
	history := []IssueEvent{startEvent("e1", 1), phaseEvent("e2", 2, PhasePlan, PhaseReview)}

	leaf := hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateInProgress)
	move, err := leaf.PlanPhase(history, PhaseImpl)
	if err != nil {
		t.Fatalf("PlanPhase() error = %v", err)
	}
	if move != (PhaseChange{From: PhaseReview, To: PhaseImpl}) || !move.Changed() {
		t.Errorf("PlanPhase(review back to impl) = %+v, Changed %v; want review -> impl, changed", move, move.Changed())
	}
	same, err := leaf.PlanPhase(history, PhaseReview)
	if err != nil {
		t.Fatalf("PlanPhase(same) error = %v", err)
	}
	if same.Changed() {
		t.Errorf("PlanPhase to the current phase is Changed; it must record nothing")
	}

	open := hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateOpen)
	child := hydratedIssue(t, Issue{ID: "c", IssueType: "task"}, StateInProgress)
	epic, err := HydrateAllOf(Issue{ID: "E", IssueType: "epic"}, []Issue{child})
	if err != nil {
		t.Fatalf("HydrateAllOf() error = %v", err)
	}
	archived := hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateInProgress)
	archived.SetRetention(Archived{At: at(0)})
	archivedOpen := hydratedIssue(t, Issue{ID: "T", IssueType: "task"}, StateOpen)
	archivedOpen.SetRetention(Archived{At: at(0)})
	for _, tc := range []struct {
		name  string
		issue Issue
		want  string
	}{
		{"not in progress", open, "cannot set the phase of T: it is open, and only an in-progress ticket has a phase — `lit start T` enters plan"},
		{"an epic", epic, "cannot set the phase of epic E: an epic's state derives from its children, and only an in-progress ticket has a phase"},
		{"frozen", archived, "issue T is archived, so its phase cannot change: run `lit unarchive T` first"},
		// Not in progress is the refusal that names the next working step:
		// `lit start` itself names the unarchive it also needs.
		{"frozen and not in progress", archivedOpen, "cannot set the phase of T: it is open, and only an in-progress ticket has a phase — `lit start T` enters plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.issue.PlanPhase(history, PhaseImpl)
			var refused ValidationError
			if !errors.As(err, &refused) {
				t.Fatalf("PlanPhase() error = %v, want a ValidationError", err)
			}
			if err.Error() != tc.want {
				t.Errorf("PlanPhase() error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestNextEventInstant states the stamp a write takes against the history it
// saw: the clock's reading when it is past all of it, and otherwise the
// instant just past the newest event, whichever order the history arrives in.
func TestNextEventInstant(t *testing.T) {
	history := []IssueEvent{startEvent("e3", 3), startEvent("e1", 1), startEvent("e2", 2)}
	for _, c := range []struct {
		name string
		now  time.Time
		seen []IssueEvent
		want time.Time
	}{
		{"no history takes the clock", at(0), nil, at(0)},
		{"a clock past the history takes the clock", at(4), history, at(4)},
		{"a clock behind the history steps past its newest event", at(2), history, at(3).Add(time.Nanosecond)},
		{"a clock at the newest event steps past it", at(3), history, at(3).Add(time.Nanosecond)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := NextEventInstant(c.now, NewestInstant(c.seen))
			if !got.Equal(c.want) {
				t.Fatalf("NextEventInstant = %s, want %s", got, c.want)
			}
			for _, event := range c.seen {
				written := IssueEvent{ID: "e0", CreatedAt: got}
				if CompareEventRecency(written, event) <= 0 {
					t.Errorf("the written event sorts at or before %s, which it was written against", event.ID)
				}
			}
		})
	}
}
