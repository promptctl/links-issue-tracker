package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func TestRunNewSupportsTopicAndParent(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Renderer cleanup",
		Topic:     "renderer",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runNew(ctx, &stdout, ap, []string{
		"--title", "Tighten repro",
		"--topic", "renderer",
		"--parent", parent.ID,
		"--type", "task",
		"--priority", "2",
		"--json",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}

	var created model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("json.Unmarshal(runNew output) error = %v", err)
	}
	if created.Topic != "renderer" {
		t.Fatalf("created.Topic = %q, want renderer", created.Topic)
	}
	if created.ID != parent.ID+".1" {
		t.Fatalf("created.ID = %q, want %q", created.ID, parent.ID+".1")
	}
}

func TestRunQuickstartIncludesPrefixAndTopics(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Renderer cleanup",
		Topic:     "renderer",
		IssueType: "task",
		Priority:  1,
	}); err != nil {
		t.Fatalf("CreateIssue(renderer) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Docs cleanup",
		Topic:     "docs",
		IssueType: "task",
		Priority:  2,
	}); err != nil {
		t.Fatalf("CreateIssue(docs) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runQuickstart(ctx, &stdout, ap.Workspace, []string{"--json"}); err != nil {
		t.Fatalf("runQuickstart(--json) error = %v", err)
	}

	var payload struct {
		IssuePrefix string   `json:"issue_prefix"`
		Topics      []string `json:"topics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal(quickstart output) error = %v", err)
	}
	if payload.IssuePrefix != "test" {
		t.Fatalf("payload.IssuePrefix = %q, want test", payload.IssuePrefix)
	}
	if len(payload.Topics) != 2 || payload.Topics[0] != "docs" || payload.Topics[1] != "renderer" {
		t.Fatalf("payload.Topics = %#v, want [docs renderer]", payload.Topics)
	}
}
