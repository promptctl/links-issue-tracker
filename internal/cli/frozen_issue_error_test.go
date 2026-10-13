package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Every status command on an archived or deleted ticket, driven through the
// real command: the refusal has to travel from Issue.Apply through the store to
// the sink still typed, so a layer that wraps it untyped drops it back to exit 1
// and the retry advice, and fails here.
func TestStatusCommandOnFrozenTicketRefusesNamingTheThaw(t *testing.T) {
	for _, freeze := range []struct {
		action model.RetentionAction
		state  string
		thaw   string
	}{
		{model.Archive{}, "archived", "unarchive"},
		{model.Delete{}, "deleted", "restore"},
	} {
		for _, spec := range []transitionSpec{startSpec, doneSpec, closeSpec, openSpec} {
			t.Run(freeze.state+"/"+spec.name, func(t *testing.T) {
				h := newReadyTestHarness(t)
				epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home", Topic: "frozen", IssueType: "epic"})
				ticket := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Frozen", Topic: "frozen", IssueType: "task", ParentID: epic.ID})
				h.applyAction(ticket.ID, freeze.action, "")

				err := h.runTransitionErr(ticket.ID, spec, actionFlags[spec.name]...)
				var frozen model.FrozenIssueError
				if !errors.As(err, &frozen) {
					t.Fatalf("%s on a %s ticket error = %v, want FrozenIssueError", spec.name, freeze.state, err)
				}
				want := fmt.Sprintf("issue %s is %s, so its status cannot change: run `lit %s %s` first", ticket.ID, freeze.state, freeze.thaw, ticket.ID)
				if err.Error() != want {
					t.Errorf("message = %q, want %q", err.Error(), want)
				}
				rendered := renderCommandError(t, err)
				header := fmt.Sprintf("error (code=%d, reason=%s): ", ExitValidation, reasonRefused)
				if !strings.HasPrefix(rendered, header) {
					t.Errorf("rendered error does not open with %q:\n%s", header, rendered)
				}
				for _, fault := range []string{retryAdvice, doctorAdvice} {
					if strings.Contains(rendered, fault) {
						t.Errorf("rendered error advises %q, which cannot clear a frozen ticket:\n%s", fault, rendered)
					}
				}
			})
		}
	}
}

// `start` is the one status command with a gate ahead of Apply: the takeover
// confirmation for a lane another checkout holds. What Issue.Apply refuses
// whoever holds the lane answers before that gate, so the caller is never sent
// to --take for a start that could not land. The live sibling is the control:
// it meets the takeover gate, so the lane is held, and the frozen row is
// refused ahead of a gate that is really there.
func TestStartRefusesAFrozenTicketBeforeTheTakeoverGate(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Held", Topic: "frozen", IssueType: "epic"})
	theirs := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Theirs", Topic: "frozen", IssueType: "task", ParentID: epic.ID})
	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Live sibling", Topic: "frozen", IssueType: "task", ParentID: epic.ID})
	archived := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Archived", Topic: "frozen", IssueType: "task", ParentID: epic.ID})
	h.applyAction(archived.ID, model.Archive{}, "")
	h.asCheckout("otherstream01")
	h.applyAction(theirs.ID, model.Start{Assignee: "tester"}, "")

	var takeover takeoverUnconfirmedError
	if err := h.runTransitionErr(sibling.ID, startSpec); !errors.As(err, &takeover) {
		t.Fatalf("start on a live ticket in the held lane error = %v, want takeoverUnconfirmedError", err)
	}
	var frozen model.FrozenIssueError
	if err := h.runTransitionErr(archived.ID, startSpec); !errors.As(err, &frozen) {
		t.Fatalf("start on an archived ticket in the held lane error = %v, want FrozenIssueError", err)
	}
}
