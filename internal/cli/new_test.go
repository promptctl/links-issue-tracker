package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
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
	if err := runNew(ctx, newOutputModeWriter(&stdout, outputModeText), ap, []string{
		"--title", "Tighten repro",
		"--topic", "renderer",
		"--parent", parent.ID,
		"--type", "task",
		"--priority", "1",
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

func TestRunNewRanksToTopByDefaultAndBottomOnFlag(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	runCreate := func(title string, args ...string) model.Issue {
		t.Helper()
		var stdout bytes.Buffer
		base := []string{"--title", title, "--topic", "place", "--type", "task", "--json"}
		if err := runNew(ctx, newOutputModeWriter(&stdout, outputModeText), ap, append(base, args...)); err != nil {
			t.Fatalf("runNew(%q) error = %v", title, err)
		}
		var created model.Issue
		if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
			t.Fatalf("json.Unmarshal(runNew %q) error = %v", title, err)
		}
		return created
	}

	first := runCreate("First")
	second := runCreate("Second")                 // default: jumps above First
	appended := runCreate("Appended", "--bottom") // explicit: lands at the bottom

	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	var got []string
	for _, is := range issues {
		got = append(got, is.ID)
	}
	want := []string{second.ID, first.ID, appended.ID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rank order = %#v, want %#v (newest-first default, --bottom appended last)", got, want)
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

func TestRunNewWithoutAssigneeCreatesUnassigned(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-creator")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	var stdout bytes.Buffer
	if err := runNew(ctx, newOutputModeWriter(&stdout, outputModeText), ap, []string{
		"--title", "Born unclaimed", "--topic", "lifecycle", "--json",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}
	var created model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("json.Unmarshal(runNew output) error = %v", err)
	}
	// open means unclaimed: creation is not a claim, so the session identity
	// must not be stamped onto a ticket nobody started. [LAW:one-source-of-truth]
	if got := created.AssigneeValue(); got != "" {
		t.Fatalf("created.AssigneeValue() = %q, want empty: lit new must not self-assign from session env", got)
	}
}

func TestRunNewExplicitAssigneeHonoredVerbatim(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-creator")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	var stdout bytes.Buffer
	if err := runNew(ctx, newOutputModeWriter(&stdout, outputModeText), ap, []string{
		"--title", "Pre-assigned", "--topic", "lifecycle", "--assignee", "alice", "--json",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}
	var created model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("json.Unmarshal(runNew output) error = %v", err)
	}
	if got, want := created.AssigneeValue(), "alice"; got != want {
		t.Fatalf("created.AssigneeValue() = %q, want %q: explicit assignee must not be rewritten to the caller", got, want)
	}
}
