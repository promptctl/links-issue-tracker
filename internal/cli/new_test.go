package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func TestRunNewSupportsTopicAndParent(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", 
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

func TestRunQuickstartLoadsTemplateGuidance(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	templatePath := filepath.Join(ap.Workspace.RootDir, ".lit", "templates", "quickstart.md")
	if err := os.MkdirAll(filepath.Dir(templatePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(template dir) error = %v", err)
	}
	template := strings.Join([]string{
		"## Custom quickstart",
		"",
		"Use `lit ready`.",
		"",
	}, "\n")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runQuickstart(ctx, &stdout, ap.Workspace, nil); err != nil {
		t.Fatalf("runQuickstart() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "## Custom quickstart") {
		t.Fatalf("quickstart output missing template body: %q", output)
	}
}
