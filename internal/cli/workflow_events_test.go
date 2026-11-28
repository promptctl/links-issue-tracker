package cli

import (
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// hydratedIssue builds a model.Issue with a real lifecycle (State() works)
// without a store — model.HydrateStatus is the pure boundary the model
// package itself exposes for exactly this. [LAW:effects-at-boundaries]
func hydratedIssue(t *testing.T, id model.ID, labels []string, state model.State) model.Issue {
	t.Helper()
	issue, err := model.HydrateStatus(model.Issue{ID: id, IssueType: model.TypeTask, Labels: model.Texts(labels)}, model.StatusView{Value: state})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}
	return issue
}

// assertReadOccasion checks what a read moment's builder is expected to hand dispatch:
// the one event, and the ticket it read, if any.
func assertReadOccasion(t *testing.T, got workflows.Occasion, event workflows.Event, id model.ID, labels []string) {
	t.Helper()
	if !slices.Equal(workflows.Spellings(got.Moments), []string{string(event)}) || got.IssueID != id || !slices.Equal(got.Labels, labels) {
		t.Fatalf("occasion = %+v, want only %s on %q labeled %v", got, event, id, labels)
	}
}

// Every command acting on one ticket fires its event with the ticket's id
// and labels, and nothing else.
func TestTicketOccasion(t *testing.T) {
	t.Parallel()
	for _, event := range []workflows.Event{workflows.EventShowTicket, workflows.EventNextPulled, workflows.EventTicketUpdated, workflows.EventCommentAdded} {
		issue := hydratedIssue(t, "lit-1", []string{"need-design"}, model.StateInProgress)
		assertReadOccasion(t, ticketOccasion(event, issue), event, "lit-1", []string{"need-design"})
	}
}

func TestBacklogOccasionCarriesNoSingleTicket(t *testing.T) {
	t.Parallel()
	assertReadOccasion(t, backlogOccasion(), workflows.EventShowBacklog, "", nil)
}

// A created ticket fires ticket_created and enters every state it starts in,
// outermost first.
func TestCreatedOccasionEntersTheInitialStates(t *testing.T) {
	t.Parallel()
	got := createdOccasion(hydratedIssue(t, "lit-3", []string{"needs-triage"}, model.StateOpen))
	if spelled := workflows.Spellings(got.Moments); !slices.Equal(spelled, []string{"ticket_created", "enter open", "enter live"}) {
		t.Fatalf("Moments = %q, want ticket_created, then enter open and enter live", spelled)
	}
	if got.IssueID != "lit-3" || !slices.Equal(got.Labels, []string{"needs-triage"}) {
		t.Fatalf("occasion = %+v, want lit-3 labeled needs-triage", got)
	}
}

// TestTransitionOccasionCrossesAsAStatechart: every lifecycle transition fires
// the crossings it made and nothing naming the command, in the order a
// statechart makes them: the states it moved the ticket out of, innermost
// first, then the ones it moved it into, outermost first. A status action
// exits and enters the phase inside in_progress along with in_progress itself;
// a phase move exits and enters phases alone; a retention move exits and
// enters retentions alone; a call that leaves the ticket where it was — a
// reclaim, a move to the phase it is in — crosses nothing, and still names the
// ticket and its labels. done and close make the same crossing into closed.
func TestTransitionOccasionCrossesAsAStatechart(t *testing.T) {
	t.Parallel()
	at := func(state model.State, phase model.Phase, retention model.Retention) ticketState {
		issue := hydratedIssue(t, "lit-9", []string{"tracked"}, state)
		issue.SetRetention(retention)
		var events []model.IssueEvent
		if phase != "" {
			events = []model.IssueEvent{{ID: "e1", Action: string(model.ActionPhase), Changes: []model.FieldChange{{Field: model.PhaseField, To: string(phase)}}}}
		}
		return ticketState{issue: issue, events: events}
	}
	live, archived, deleted := model.Retention(model.Live{}), model.Retention(model.Archived{}), model.Retention(model.Deleted{})
	cases := []struct {
		name   string
		before ticketState
		after  ticketState
		want   []string
	}{
		{"start", at(model.StateOpen, "", live), at(model.StateInProgress, model.PhasePlan, live),
			[]string{"exit open", "enter in_progress", "enter plan"}},
		{"reclaim", at(model.StateInProgress, model.PhaseReview, live), at(model.StateInProgress, model.PhaseReview, live),
			[]string{}},
		{"done", at(model.StateInProgress, model.PhaseMerge, live), at(model.StateClosed, "", live),
			[]string{"exit merge", "exit in_progress", "enter closed"}},
		{"close", at(model.StateOpen, "", live), at(model.StateClosed, "", live),
			[]string{"exit open", "enter closed"}},
		{"close from a phase", at(model.StateInProgress, model.PhaseImpl, live), at(model.StateClosed, "", live),
			[]string{"exit impl", "exit in_progress", "enter closed"}},
		{"reopen", at(model.StateClosed, "", live), at(model.StateOpen, "", live),
			[]string{"exit closed", "enter open"}},
		{"phase", at(model.StateInProgress, model.PhaseImpl, live), at(model.StateInProgress, model.PhaseReview, live),
			[]string{"exit impl", "enter review"}},
		{"phase unchanged", at(model.StateInProgress, model.PhaseReview, live), at(model.StateInProgress, model.PhaseReview, live),
			[]string{}},
		{"archive", at(model.StateClosed, "", live), at(model.StateClosed, "", archived),
			[]string{"exit live", "enter archived"}},
		{"delete an archived ticket", at(model.StateClosed, "", archived), at(model.StateClosed, "", deleted),
			[]string{"exit archived", "enter deleted"}},
		{"restore", at(model.StateOpen, "", deleted), at(model.StateOpen, "", live),
			[]string{"exit deleted", "enter live"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transitionOccasion(tc.before, tc.after)
			if spelled := workflows.Spellings(got.Moments); !slices.Equal(spelled, tc.want) {
				t.Fatalf("Moments = %q, want %q", spelled, tc.want)
			}
			if got.IssueID != "lit-9" {
				t.Fatalf("IssueID = %q, want lit-9", got.IssueID)
			}
			if !slices.Equal(got.Labels, []string{"tracked"}) {
				t.Fatalf("Labels = %v, want [tracked]", got.Labels)
			}
		})
	}
}
