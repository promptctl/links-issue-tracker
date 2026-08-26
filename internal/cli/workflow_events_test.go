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
	if got.Entered != "" || got.Exited != "" {
		t.Fatalf("showTicketOccasion() carries a transition: %+v", got)
	}
}

func TestBacklogOccasionCarriesNoSingleTicket(t *testing.T) {
	t.Parallel()
	got := backlogOccasion()
	if got.Event != workflows.EventShowBacklog {
		t.Fatalf("backlogOccasion().Event = %q, want %q", got.Event, workflows.EventShowBacklog)
	}
	if got.IssueID != "" || got.Labels != nil || got.Entered != "" || got.Exited != "" {
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
	if got.Entered != "" || got.Exited != "" {
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

// TestTransitionOccasionAllFourStatusActions is the done-claim's explicit
// requirement: every one of the four status transitions dispatches the right
// event with the right from/to state pair.
func TestTransitionOccasionAllFourStatusActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		action      model.StatusAction
		priorState  model.State
		issueState  model.State
		wantEvent   workflows.Event
		wantEntered string
		wantExited  string
	}{
		{
			name:        "start",
			action:      model.Start{Assignee: "claude"},
			priorState:  model.StateOpen,
			issueState:  model.StateInProgress,
			wantEvent:   workflows.EventWorkStarted,
			wantEntered: "in_progress",
			wantExited:  "open",
		},
		{
			name:        "done",
			action:      model.Done{},
			priorState:  model.StateInProgress,
			issueState:  model.StateClosed,
			wantEvent:   workflows.EventWorkFinished,
			wantEntered: "closed",
			wantExited:  "in_progress",
		},
		{
			name:        "close",
			action:      model.Close{Outcome: model.Wontfix{}},
			priorState:  model.StateOpen,
			issueState:  model.StateClosed,
			wantEvent:   workflows.EventTicketClosed,
			wantEntered: "closed",
			wantExited:  "open",
		},
		{
			name:        "reopen",
			action:      model.Reopen{},
			priorState:  model.StateClosed,
			issueState:  model.StateOpen,
			wantEvent:   workflows.EventTicketReopened,
			wantEntered: "open",
			wantExited:  "closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prior := hydratedIssue(t, "lit-9", []string{"tracked"}, tc.priorState)
			issue := hydratedIssue(t, "lit-9", []string{"tracked"}, tc.issueState)
			got := transitionOccasion(tc.action, prior, issue)
			if got.Event != tc.wantEvent {
				t.Fatalf("Event = %q, want %q", got.Event, tc.wantEvent)
			}
			if got.IssueID != "lit-9" {
				t.Fatalf("IssueID = %q, want lit-9", got.IssueID)
			}
			if !slices.Equal(got.Labels, []string{"tracked"}) {
				t.Fatalf("Labels = %v, want [tracked]", got.Labels)
			}
			if got.Entered != tc.wantEntered || got.Exited != tc.wantExited {
				t.Fatalf("Entered/Exited = %q/%q, want %q/%q", got.Entered, got.Exited, tc.wantEntered, tc.wantExited)
			}
		})
	}
}

// TestRetentionActionsAreNotStatusActions documents, at compile time, why
// archive/unarchive/delete/restore never reach transitionOccasion: they do
// not implement model.StatusAction (no Target() State method), so the
// runTransition type assertion that guards the call excludes them
// structurally — there is no runtime case to test.
func TestRetentionActionsAreNotStatusActions(t *testing.T) {
	t.Parallel()
	var actions = []model.Action{model.Archive{}, model.Unarchive{}, model.Delete{}, model.Restore{}}
	for _, action := range actions {
		if _, ok := action.(model.StatusAction); ok {
			t.Fatalf("%T unexpectedly implements model.StatusAction", action)
		}
	}
}
