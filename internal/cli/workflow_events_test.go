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
func hydratedIssue(t *testing.T, id string, labels []string, state model.State) model.Issue {
	t.Helper()
	issue, err := model.HydrateStatus(model.Issue{ID: id, IssueType: model.TypeTask, Labels: labels}, model.StatusView{Value: state})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}
	return issue
}

func TestShowTicketOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-1", []string{"need-design"}, model.StateOpen)
	got := showTicketOccasion(issue)
	want := workflows.Occasion{Event: workflows.EventShowTicket, IssueID: "lit-1", Labels: []string{"need-design"}}
	if got.Event != want.Event || got.IssueID != want.IssueID || !slices.Equal(got.Labels, want.Labels) {
		t.Fatalf("showTicketOccasion() = %+v, want %+v", got, want)
	}
	if got.Entered != nil || got.Exited != nil {
		t.Fatalf("showTicketOccasion() carries a transition: %+v", got)
	}
}

func TestBacklogOccasionCarriesNoSingleTicket(t *testing.T) {
	t.Parallel()
	got := backlogOccasion()
	if got.Event != workflows.EventShowBacklog {
		t.Fatalf("backlogOccasion().Event = %q, want %q", got.Event, workflows.EventShowBacklog)
	}
	if got.IssueID != "" || got.Labels != nil || got.Entered != nil || got.Exited != nil {
		t.Fatalf("backlogOccasion() = %+v, want only Event set", got)
	}
}

func TestNextPulledOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-2", []string{"epic-child"}, model.StateOpen)
	got := nextPulledOccasion(issue)
	if got.Event != workflows.EventNextPulled || got.IssueID != "lit-2" || !slices.Equal(got.Labels, []string{"epic-child"}) {
		t.Fatalf("nextPulledOccasion() = %+v", got)
	}
}

func TestTicketCreatedOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-3", []string{"needs-triage"}, model.StateOpen)
	got := ticketCreatedOccasion(issue)
	if got.Event != workflows.EventTicketCreated || got.IssueID != "lit-3" || !slices.Equal(got.Labels, []string{"needs-triage"}) {
		t.Fatalf("ticketCreatedOccasion() = %+v", got)
	}
}

func TestTicketUpdatedOccasionCarriesNoTransition(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-4", []string{"blocked"}, model.StateInProgress)
	got := ticketUpdatedOccasion(issue)
	if got.Event != workflows.EventTicketUpdated || got.IssueID != "lit-4" || !slices.Equal(got.Labels, []string{"blocked"}) {
		t.Fatalf("ticketUpdatedOccasion() = %+v", got)
	}
	if got.Entered != nil || got.Exited != nil {
		t.Fatalf("ticketUpdatedOccasion() carries a transition: %+v", got)
	}
}

func TestCommentAddedOccasion(t *testing.T) {
	t.Parallel()
	issue := hydratedIssue(t, "lit-5", []string{"discuss"}, model.StateOpen)
	got := commentAddedOccasion(issue)
	if got.Event != workflows.EventCommentAdded || got.IssueID != "lit-5" || !slices.Equal(got.Labels, []string{"discuss"}) {
		t.Fatalf("commentAddedOccasion() = %+v", got)
	}
}

// TestTransitionOccasionMovesBothLevels: every lifecycle transition fires its
// event with the states it moved the ticket out of and into. A status action
// exits and enters the phase inside in_progress along with in_progress
// itself; a phase move exits and enters phases alone; a call that leaves the
// ticket where it was exits and enters nothing.
func TestTransitionOccasionMovesBothLevels(t *testing.T) {
	t.Parallel()
	at := func(state model.State, phase model.Phase) ticketState {
		return ticketState{issue: hydratedIssue(t, "lit-9", []string{"tracked"}, state), phase: phase}
	}
	cases := []struct {
		name        string
		action      model.Action
		before      ticketState
		after       ticketState
		wantEvent   workflows.Event
		wantExited  []string
		wantEntered []string
	}{
		{"start", model.Start{Assignee: "claude"}, at(model.StateOpen, ""), at(model.StateInProgress, model.PhasePlan),
			workflows.EventWorkStarted, []string{"open"}, []string{"in_progress", "plan"}},
		{"reclaim", model.Start{Assignee: "claude"}, at(model.StateInProgress, model.PhaseReview), at(model.StateInProgress, model.PhaseReview),
			workflows.EventWorkStarted, nil, nil},
		{"done", model.Done{}, at(model.StateInProgress, model.PhaseMerge), at(model.StateClosed, ""),
			workflows.EventWorkFinished, []string{"in_progress", "merge"}, []string{"closed"}},
		{"close", model.Close{Outcome: model.Wontfix{}}, at(model.StateOpen, ""), at(model.StateClosed, ""),
			workflows.EventTicketClosed, []string{"open"}, []string{"closed"}},
		{"reopen", model.Reopen{}, at(model.StateClosed, ""), at(model.StateOpen, ""),
			workflows.EventTicketReopened, []string{"closed"}, []string{"open"}},
		{"phase", model.SetPhase{Phase: model.PhaseReview}, at(model.StateInProgress, model.PhaseImpl), at(model.StateInProgress, model.PhaseReview),
			workflows.EventPhaseSet, []string{"impl"}, []string{"review"}},
		{"phase unchanged", model.SetPhase{Phase: model.PhaseReview}, at(model.StateInProgress, model.PhaseReview), at(model.StateInProgress, model.PhaseReview),
			workflows.EventPhaseSet, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fires := transitionOccasion(tc.action, tc.before, tc.after)
			if !fires {
				t.Fatalf("transitionOccasion(%T) fires = false, want an occasion", tc.action)
			}
			if got.Event != tc.wantEvent {
				t.Fatalf("Event = %q, want %q", got.Event, tc.wantEvent)
			}
			if got.IssueID != "lit-9" {
				t.Fatalf("IssueID = %q, want lit-9", got.IssueID)
			}
			if !slices.Equal(got.Labels, []string{"tracked"}) {
				t.Fatalf("Labels = %v, want [tracked]", got.Labels)
			}
			if !slices.Equal(got.Exited, tc.wantExited) || !slices.Equal(got.Entered, tc.wantEntered) {
				t.Fatalf("Exited/Entered = %v/%v, want %v/%v", got.Exited, got.Entered, tc.wantExited, tc.wantEntered)
			}
		})
	}
}

// Archive, unarchive, delete and restore are no lifecycle transition: they
// fire no occasion, whatever state the ticket is in.
func TestRetentionActionsFireNothing(t *testing.T) {
	t.Parallel()
	inReview := ticketState{issue: hydratedIssue(t, "lit-9", nil, model.StateInProgress), phase: model.PhaseReview}
	for _, action := range []model.Action{model.Archive{}, model.Unarchive{}, model.Delete{}, model.Restore{}} {
		if got, fires := transitionOccasion(action, inReview, inReview); fires {
			t.Errorf("transitionOccasion(%T) = %+v, want no occasion", action, got)
		}
	}
}
