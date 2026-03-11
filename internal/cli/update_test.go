package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func TestRunUpdateSupportsStatusTransitionWithoutExplicitReason(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Update status",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--status", "in_progress", "--json"}); err != nil {
		t.Fatalf("runUpdate(--status in_progress --json) error = %v", err)
	}

	var updated model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update output) error = %v", err)
	}
	if updated.Status != "in_progress" {
		t.Fatalf("updated.Status = %q, want in_progress", updated.Status)
	}

	detail, err := ap.Store.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.History) < 2 {
		t.Fatalf("len(detail.History) = %d, want >= 2", len(detail.History))
	}
	last := detail.History[len(detail.History)-1]
	if last.Action != "start" {
		t.Fatalf("last.Action = %q, want start", last.Action)
	}
	if !strings.Contains(last.Reason, "status update via lit update") {
		t.Fatalf("last.Reason = %q, want default update reason", last.Reason)
	}
}

func TestRunUpdateSupportsFieldMutations(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Update fields",
		IssueType: "task",
		Priority:  3,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--priority", "1", "--assignee", "alice", "--labels", "api,urgent", "--json"}); err != nil {
		t.Fatalf("runUpdate(field flags --json) error = %v", err)
	}

	var updated model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update output) error = %v", err)
	}
	if updated.Priority != 1 {
		t.Fatalf("updated.Priority = %d, want 1", updated.Priority)
	}
	if updated.Assignee != "alice" {
		t.Fatalf("updated.Assignee = %q, want alice", updated.Assignee)
	}
	if len(updated.Labels) != 2 || updated.Labels[0] != "api" || updated.Labels[1] != "urgent" {
		t.Fatalf("updated.Labels = %#v, want [api urgent]", updated.Labels)
	}
}

func TestRunUpdateRejectsReasonWithoutStatus(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Validation",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	err = runUpdate(ctx, &stdout, ap, []string{created.ID, "--reason", "not allowed", "--json"})
	if err == nil {
		t.Fatal("runUpdate(--reason without --status) error = nil, want validation error")
	}
	if err.Error() != "--reason requires --status" {
		t.Fatalf("runUpdate error = %q, want %q", err.Error(), "--reason requires --status")
	}
}

func TestRunUpdateRejectsEmptyStatusValue(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Empty status",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	err = runUpdate(ctx, &stdout, ap, []string{created.ID, "--status=", "--json"})
	if err == nil {
		t.Fatal("runUpdate(--status= --json) error = nil, want validation error")
	}
	if err.Error() != "--status requires a non-empty value" {
		t.Fatalf("runUpdate error = %q, want %q", err.Error(), "--status requires a non-empty value")
	}
}
