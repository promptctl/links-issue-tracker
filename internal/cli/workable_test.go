package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// --status is validated once, at the shared flag seam
// (parseWorkableStatus), so one table over every workable command is the
// whole contract: each rejects the same inputs with the same usage error,
// whether it runs through the workableView preset (backlog) or its own
// runner (next, forked out in next.go once claim routing gave it a
// genuinely different shape). [LAW:single-enforcer]
var workableCmds = []struct {
	name string
	run  func(h readyTestHarness, args ...string) error
}{
	{name: backlogView.name, run: func(h readyTestHarness, args ...string) error { return h.runViewErr(backlogView, args...) }},
	{name: "next", run: func(h readyTestHarness, args ...string) error {
		var stdout bytes.Buffer
		return runNext(h.ctx, &stdout, h.ap, args)
	}},
}

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
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Open leaf", Topic: "status", IssueType: "task", Priority: 1})

	for _, cmd := range workableCmds {
		for _, value := range []string{"weird", "closed", "CLOSED", "done"} {
			err := cmd.run(h, "--status", value)
			var usageErr UsageError
			if !errors.As(err, &usageErr) {
				t.Fatalf("lit %s --status %s error = %v, want UsageError", cmd.name, value, err)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("lit %s --status %s exit code = %d, want %d", cmd.name, value, got, ExitUsage)
			}
			if !strings.Contains(err.Error(), "open, in_progress") {
				t.Fatalf("lit %s --status %s error = %q, want the legal values named", cmd.name, value, err)
			}
			if !strings.Contains(err.Error(), value) {
				t.Fatalf("lit %s --status %s error = %q, want the rejected value echoed", cmd.name, value, err)
			}
		}
	}
}

func TestWorkableStatusAcceptsLegalValues(t *testing.T) {
	h := newReadyTestHarness(t)
	issue := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Open leaf", Topic: "status", IssueType: "task", Priority: 1})

	text := h.runWorkableText("--status", "open")
	if !strings.Contains(text, issue.ID) {
		t.Fatalf("backlog --status open output = %q, want %q listed", text, issue.ID)
	}

	// in_progress matches nothing here; the point is it parses (no usage
	// error) and narrows honestly instead of coercing.
	err := h.runViewErr(backlogView, "--status", "in_progress")
	if err != nil {
		t.Fatalf("backlog --status in_progress error = %v, want nil", err)
	}
	text = h.runWorkableText("--status", "in_progress")
	if strings.Contains(text, issue.ID) {
		t.Fatalf("backlog --status in_progress output = %q, want %q filtered out", text, issue.ID)
	}
}
