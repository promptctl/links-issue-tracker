package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// --status is validated once, at the shared flag seam, so one table over the
// four views is the whole contract: every workable command rejects the same
// inputs with the same usage error. [LAW:single-enforcer]
var workableViews = []workableView{readyView, backlogView, queueView, nextView}

func (h readyTestHarness) runViewErr(view workableView, args ...string) error {
	h.t.Helper()
	var stdout bytes.Buffer
	return runWorkable(h.ctx, &stdout, h.ap, args, view)
}

// Unrecognized statuses used to be silently coerced to open, answering a
// different question than asked; closed is rejected too because a workable
// row is never closed — the result would be empty by construction.
func TestWorkableStatusRejectsInvalidValues(t *testing.T) {
	h := newReadyTestHarness(t)
	h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Open leaf", Topic: "status", IssueType: "task", Priority: 1})

	for _, view := range workableViews {
		for _, value := range []string{"weird", "closed", "CLOSED", "done"} {
			err := h.runViewErr(view, "--status", value)
			var usageErr UsageError
			if !errors.As(err, &usageErr) {
				t.Fatalf("lit %s --status %s error = %v, want UsageError", view.name, value, err)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("lit %s --status %s exit code = %d, want %d", view.name, value, got, ExitUsage)
			}
			if !strings.Contains(err.Error(), "open, in_progress") {
				t.Fatalf("lit %s --status %s error = %q, want the legal values named", view.name, value, err)
			}
			if !strings.Contains(err.Error(), value) {
				t.Fatalf("lit %s --status %s error = %q, want the rejected value echoed", view.name, value, err)
			}
		}
	}
}

func TestWorkableStatusAcceptsLegalValues(t *testing.T) {
	h := newReadyTestHarness(t)
	issue := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Open leaf", Topic: "status", IssueType: "task", Priority: 1})

	text := h.runReadyText("--status", "open")
	if !strings.Contains(text, issue.ID) {
		t.Fatalf("ready --status open output = %q, want %q listed", text, issue.ID)
	}

	// in_progress matches nothing here; the point is it parses (no usage
	// error) and narrows honestly instead of coercing.
	err := h.runViewErr(readyView, "--status", "in_progress")
	if err != nil {
		t.Fatalf("ready --status in_progress error = %v, want nil", err)
	}
	text = h.runReadyText("--status", "in_progress")
	if strings.Contains(text, issue.ID) {
		t.Fatalf("ready --status in_progress output = %q, want %q filtered out", text, issue.ID)
	}
}
