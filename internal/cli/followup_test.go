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

func TestRunFollowupParentsToClosedTicket(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Renderer cache invalidation",
		Topic:     "renderer",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID: parent.ID, Action: "start", CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID: parent.ID, Action: "done", CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("TransitionIssue(done) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runFollowup(ctx, &stdout, ap, []string{
		"--on", parent.ID,
		"--title", "Surface stale cache rows in doctor",
		"--json",
	}); err != nil {
		t.Fatalf("runFollowup() error = %v", err)
	}

	var created model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("json.Unmarshal(runFollowup output) error = %v", err)
	}
	if created.ID != parent.ID+".1" {
		t.Fatalf("created.ID = %q, want %q", created.ID, parent.ID+".1")
	}
	if created.Topic != parent.Topic {
		t.Fatalf("created.Topic = %q, want inherited %q", created.Topic, parent.Topic)
	}
	if !strings.Contains(created.Description, parent.ID) {
		t.Fatalf("created.Description = %q, want reference to parent %q", created.Description, parent.ID)
	}
	if created.IssueType != "task" {
		t.Fatalf("created.IssueType = %q, want task", created.IssueType)
	}
}

func TestRunFollowupRespectsExplicitOverrides(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
		Title:     "Sync flow tightening",
		Topic:     "sync",
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runFollowup(ctx, &stdout, ap, []string{
		"--on", parent.ID,
		"--title", "Add sync metrics surface",
		"--description", "Custom description for the follow-up.",
		"--topic", "observability",
		"--type", "feature",
		"--priority", "1",
		"--labels", "metrics,observability",
		"--json",
	}); err != nil {
		t.Fatalf("runFollowup() error = %v", err)
	}

	var created model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("json.Unmarshal(runFollowup output) error = %v", err)
	}
	if created.Topic != "observability" {
		t.Fatalf("created.Topic = %q, want observability", created.Topic)
	}
	if created.Description != "Custom description for the follow-up." {
		t.Fatalf("created.Description = %q, want explicit override", created.Description)
	}
	if created.IssueType != "feature" {
		t.Fatalf("created.IssueType = %q, want feature", created.IssueType)
	}
	if created.Priority != 1 {
		t.Fatalf("created.Priority = %d, want 1", created.Priority)
	}
}

func TestRunFollowupRequiresOnAndTitle(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	cases := []struct {
		name string
		args []string
	}{
		{"missing --on", []string{"--title", "x"}},
		{"missing --title", []string{"--on", "test-aaaaaaaa-bbbbbbbb"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runFollowup(ctx, &stdout, ap, c.args); err == nil {
				t.Fatalf("runFollowup() expected usage error, got nil")
			}
		})
	}
}

func TestRunFollowupUnknownParentReturnsError(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var stdout bytes.Buffer
	err := runFollowup(ctx, &stdout, ap, []string{
		"--on", "test-deadbeef-cafef00d",
		"--title", "Phantom follow-up",
	})
	if err == nil {
		t.Fatalf("runFollowup() expected error for unknown parent, got nil")
	}
}
