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

func TestShowTicketOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-1", []string{"need-design"}, model.StateOpen)
	assertReadOccasion(t, showTicketOccasion(issue), workflows.EventShowTicket, "lit-1", []string{"need-design"})
}

func TestBacklogOccasionCarriesNoSingleTicket(t *testing.T) {
	t.Parallel()
	assertReadOccasion(t, backlogOccasion(), workflows.EventShowBacklog, "", nil)
}

func TestNextPulledOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-2", []string{"epic-child"}, model.StateOpen)
	assertReadOccasion(t, nextPulledOccasion(issue), workflows.EventNextPulled, "lit-2", []string{"epic-child"})
}

func TestTicketCreatedOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-3", []string{"needs-triage"}, model.StateOpen)
	assertReadOccasion(t, ticketCreatedOccasion(issue), workflows.EventTicketCreated, "lit-3", []string{"needs-triage"})
}

func TestTicketUpdatedOccasionCarriesNoCrossing(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-4", []string{"blocked"}, model.StateInProgress)
	assertReadOccasion(t, ticketUpdatedOccasion(issue), workflows.EventTicketUpdated, "lit-4", []string{"blocked"})
}

func TestCommentAddedOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-5", []string{"discuss"}, model.StateOpen)
	assertReadOccasion(t, commentAddedOccasion(issue), workflows.EventCommentAdded, "lit-5", []string{"discuss"})
}

// TestTransitionOccasionCrossesBothLevels: every lifecycle transition fires
// the crossings it made and nothing naming the command: the states it moved
// the ticket out of, then the ones it moved it into, each outermost first. A
// status action exits and enters the phase inside in_progress along with
// in_progress itself; a phase move exits and enters phases alone; a call that
// leaves the ticket where it was — a reclaim, an archive or delete or their
// undoing — crosses nothing, and still names the ticket and its labels. done
// and close make the same crossing into closed.
func TestTransitionOccasionCrossesBothLevels(t *testing.T) {
	t.Parallel()
	at := func(state model.State, phase model.Phase) ticketState {
		return ticketState{issue: hydratedIssue(t, "lit-9", []string{"tracked"}, state), phase: phase}
	}
	cases := []struct {
		name   string
		before ticketState
		after  ticketState
		want   []string
	}{
		{"start", at(model.StateOpen, ""), at(model.StateInProgress, model.PhasePlan),
			[]string{"exit open", "enter in_progress", "enter plan"}},
		{"reclaim", at(model.StateInProgress, model.PhaseReview), at(model.StateInProgress, model.PhaseReview),
			[]string{}},
		{"done", at(model.StateInProgress, model.PhaseMerge), at(model.StateClosed, ""),
			[]string{"exit in_progress", "exit merge", "enter closed"}},
		{"close", at(model.StateOpen, ""), at(model.StateClosed, ""),
			[]string{"exit open", "enter closed"}},
		{"close from a phase", at(model.StateInProgress, model.PhaseImpl), at(model.StateClosed, ""),
			[]string{"exit in_progress", "exit impl", "enter closed"}},
		{"reopen", at(model.StateClosed, ""), at(model.StateOpen, ""),
			[]string{"exit closed", "enter open"}},
		{"phase", at(model.StateInProgress, model.PhaseImpl), at(model.StateInProgress, model.PhaseReview),
			[]string{"exit impl", "enter review"}},
		{"phase unchanged", at(model.StateInProgress, model.PhaseReview), at(model.StateInProgress, model.PhaseReview),
			[]string{}},
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
