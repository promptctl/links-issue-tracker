package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func newTestCLIApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", "")
	t.Setenv("LIT_CONFIG_PROJECT_PATH", "")
	ctx := context.Background()
	workspaceRoot := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(workspaceRoot, "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return &app.App{
		Workspace: workspace.Info{
			RootDir: workspaceRoot,
		},
		Store: st,
	}
}

func TestRunReadyReturnsReadyAndNotReadyIssues(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	openA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Open issue A",
		IssueType: "task",
		Priority:  2,
		Assignee:  "alice",
	})
	if err != nil {
		t.Fatalf("CreateIssue(openA) error = %v", err)
	}
	openB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Open issue B",
		IssueType: "bug",
		Priority:  1,
		Assignee:  "bob",
	})
	if err != nil {
		t.Fatalf("CreateIssue(openB) error = %v", err)
	}
	if _, err := ap.Store.AddRelation(ctx, store.AddRelationInput{
		SrcID:     openB.ID,
		DstID:     openA.ID,
		Type:      "blocks",
		CreatedBy: "agent",
	}); err != nil {
		t.Fatalf("AddRelation(blocks) error = %v", err)
	}

	closed, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Already done",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(closed) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID:   closed.ID,
		Action:    "close",
		Reason:    "not ready work",
		CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, []string{"--json"}); err != nil {
		t.Fatalf("runReady(--json) error = %v", err)
	}

	var got readyCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}

	if len(got.Ready) != 1 {
		t.Fatalf("len(got.Ready) = %d, want 1; got=%#v", len(got.Ready), got.Ready)
	}
	if got.Ready[0].ID != openA.ID {
		t.Fatalf("got.Ready[0].ID = %q, want %q", got.Ready[0].ID, openA.ID)
	}
	if len(got.NotReady) != 1 {
		t.Fatalf("len(got.NotReady) = %d, want 1; got=%#v", len(got.NotReady), got.NotReady)
	}
	if got.NotReady[0].Issue.ID != openB.ID {
		t.Fatalf("got.NotReady[0].Issue.ID = %q, want %q", got.NotReady[0].Issue.ID, openB.ID)
	}
	if got.NotReady[0].Reason != "Blocked by ticket "+openA.ID {
		t.Fatalf("got.NotReady[0].Reason = %q", got.NotReady[0].Reason)
	}
}

func TestRunReadySupportsAssigneeAndLimit(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Alice old",
		IssueType: "task",
		Priority:  1,
		Assignee:  "alice",
	}); err != nil {
		t.Fatalf("CreateIssue(alice old) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Bob task",
		IssueType: "task",
		Priority:  0,
		Assignee:  "bob",
	}); err != nil {
		t.Fatalf("CreateIssue(bob) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, []string{"--assignee", "alice", "--limit", "1", "--json"}); err != nil {
		t.Fatalf("runReady(--assignee --limit --json) error = %v", err)
	}

	var got readyCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}

	if len(got.Ready) != 1 {
		t.Fatalf("len(got.Ready) = %d, want 1; got=%#v", len(got.Ready), got.Ready)
	}
	if got.Ready[0].Assignee != "alice" {
		t.Fatalf("got.Ready[0].Assignee = %q, want alice", got.Ready[0].Assignee)
	}
	if len(got.NotReady) != 0 {
		t.Fatalf("len(got.NotReady) = %d, want 0", len(got.NotReady))
	}
}

func TestRunReadyMarksMissingRequiredFieldAsNotReady(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	configDir := filepath.Join(ap.Workspace.RootDir, ".lit")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	configContent := "[ready]\nrequired_fields = [\"description\"]\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Needs description",
		IssueType: "task",
		Priority:  1,
		Assignee:  "alice",
	})
	if err != nil {
		t.Fatalf("CreateIssue(issue) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, []string{"--json"}); err != nil {
		t.Fatalf("runReady(--json) error = %v", err)
	}

	var got readyCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}

	if len(got.Ready) != 0 {
		t.Fatalf("len(got.Ready) = %d, want 0", len(got.Ready))
	}
	if len(got.NotReady) != 1 {
		t.Fatalf("len(got.NotReady) = %d, want 1", len(got.NotReady))
	}
	if got.NotReady[0].Issue.ID != issue.ID {
		t.Fatalf("got.NotReady[0].Issue.ID = %q, want %q", got.NotReady[0].Issue.ID, issue.ID)
	}
	if got.NotReady[0].Reason != "Field description not set" {
		t.Fatalf("got.NotReady[0].Reason = %q, want %q", got.NotReady[0].Reason, "Field description not set")
	}
}

func TestRunReadyMarksUnknownRequiredFieldAsNotFound(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	configDir := filepath.Join(ap.Workspace.RootDir, ".lit")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	configContent := "[ready]\nrequired_fields = [\"made_up_field\"]\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Unknown field",
		IssueType: "task",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(issue) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, []string{"--json"}); err != nil {
		t.Fatalf("runReady(--json) error = %v", err)
	}

	var got readyCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}
	if len(got.NotReady) != 1 {
		t.Fatalf("len(got.NotReady) = %d, want 1", len(got.NotReady))
	}
	if got.NotReady[0].Issue.ID != issue.ID {
		t.Fatalf("got.NotReady[0].Issue.ID = %q, want %q", got.NotReady[0].Issue.ID, issue.ID)
	}
	if got.NotReady[0].Reason != "Field made_up_field not found" {
		t.Fatalf("got.NotReady[0].Reason = %q", got.NotReady[0].Reason)
	}
}

func TestRunReadyTextOutputIncludesNotReadySectionAndReason(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	configDir := filepath.Join(ap.Workspace.RootDir, ".lit")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	configContent := "[ready]\nrequired_fields = [\"description\"]\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}

	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:       "Ready ticket",
		IssueType:   "task",
		Priority:    1,
		Description: "ship it",
	}); err != nil {
		t.Fatalf("CreateIssue(ready) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Missing description",
		IssueType: "task",
		Priority:  2,
	}); err != nil {
		t.Fatalf("CreateIssue(not ready) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, nil); err != nil {
		t.Fatalf("runReady() error = %v", err)
	}
	text := stdout.String()
	if !strings.Contains(text, "Ready\n") {
		t.Fatalf("ready output missing Ready section header: %q", text)
	}
	if !strings.Contains(text, "\nNot Ready\n") {
		t.Fatalf("ready output missing Not Ready section header: %q", text)
	}
	if !strings.Contains(text, "Field description not set") {
		t.Fatalf("ready output missing not-ready reason: %q", text)
	}
}

func TestRunReadyReturnsConfigErrorForInvalidProjectConfig(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	configDir := filepath.Join(ap.Workspace.RootDir, ".lit")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[ready\nrequired_fields = [\"description\"]"), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}

	var stdout bytes.Buffer
	err := runReady(ctx, &stdout, ap, []string{"--json"})
	if err == nil {
		t.Fatal("runReady expected config parse error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("runReady error = %q, want parse config context", err.Error())
	}
}
