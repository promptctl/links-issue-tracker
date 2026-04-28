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

func TestRunTransitionRefusesEpicAndStartsLeaf(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Epic container",
		Topic:     "lifecycle",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	leaf, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Leaf work",
		Topic:     "lifecycle",
		IssueType: "task",
		Priority:  2,
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	var stdout bytes.Buffer
	err = runTransition(ctx, &stdout, ap, []string{epic.ID, "--assignee", "tester"}, "start")
	if err == nil {
		t.Fatal("runTransition(start epic) returned nil; want refusal")
	}
	if !strings.Contains(err.Error(), "no start action available") {
		t.Fatalf("runTransition(start epic) error = %q, want no start action available", err.Error())
	}
	stdout.Reset()
	if err := runTransition(ctx, &stdout, ap, []string{leaf.ID, "--assignee", "tester", "--json"}, "start"); err != nil {
		t.Fatalf("runTransition(start leaf) error = %v", err)
	}
	var started model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &started); err != nil {
		t.Fatalf("json.Unmarshal(start output) error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Fatalf("started.State() = %q, want in_progress", started.State())
	}
}

func TestRunShowEpicJSONOmitsProgressAndStatus(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Epic container",
		Topic:     "show",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Open child",
		Topic:     "show",
		IssueType: "task",
		Priority:  2,
		ParentID:  epic.ID,
	}); err != nil {
		t.Fatalf("CreateIssue(open child) error = %v", err)
	}
	closedChild, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Closed child",
		Topic:     "show",
		IssueType: "task",
		Priority:  2,
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(closed child) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: closedChild.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{IssueID: closedChild.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(done) error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runShow(ctx, &stdout, ap, []string{epic.ID, "--json"}); err != nil {
		t.Fatalf("runShow(epic --json) error = %v", err)
	}
	var payload struct {
		Issue map[string]json.RawMessage `json:"issue"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal(show output) error = %v", err)
	}
	if _, ok := payload.Issue["status"]; ok {
		t.Fatalf("epic JSON issue has status field: %s", stdout.String())
	}
	if _, ok := payload.Issue["progress"]; ok {
		t.Fatalf("epic JSON issue has progress field: %s", stdout.String())
	}
}

func TestRunUpdateSupportsStatusTransitionWithoutExplicitReason(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Update status",
		Topic:     "status",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runUpdate(ctx, &stdout, ap, []string{created.ID, "--status", "in_progress", "--assignee", "tester", "--json"}); err != nil {
		t.Fatalf("runUpdate(--status in_progress --json) error = %v", err)
	}

	var updated model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("json.Unmarshal(update output) error = %v", err)
	}
	if updated.State() != model.StateInProgress {
		t.Fatalf("updated.State() = %q, want in_progress", updated.State())
	}

	detail, err := ap.Store.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.Events) < 2 {
		t.Fatalf("len(detail.Events) = %d, want >= 2", len(detail.Events))
	}
	last := detail.Events[len(detail.Events)-1]
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
		Topic:     "fields",
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
	if updated.AssigneeValue() != "alice" {
		t.Fatalf("updated.AssigneeValue() = %q, want alice", updated.AssigneeValue())
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
		Topic:     "validation",
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
		Topic:     "status",
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
