package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/model"
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
	if err := st.EnsureIssuePrefix(ctx, "test"); err != nil {
		t.Fatalf("EnsureIssuePrefix() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return &app.App{
		Workspace: workspace.Info{
			RootDir:      workspaceRoot,
			DatabasePath: filepath.Join(workspaceRoot, "dolt"),
			WorkspaceID:  "test-workspace-id",
			IssuePrefix:  "test",
		},
		Store: st,
	}
}

type readyTestHarness struct {
	t   *testing.T
	ctx context.Context
	ap  *app.App
}

func newReadyTestHarness(t *testing.T) readyTestHarness {
	t.Helper()
	return readyTestHarness{
		t:   t,
		ctx: context.Background(),
		ap:  newTestCLIApp(t),
	}
}

func (h readyTestHarness) writeProjectConfig(content string) {
	h.t.Helper()
	configDir := filepath.Join(h.ap.Workspace.RootDir, ".lit")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		h.t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0o644); err != nil {
		h.t.Fatalf("WriteFile(config.toml) error = %v", err)
	}
}

func (h readyTestHarness) writeReadyConfig(requiredFields ...string) {
	h.t.Helper()
	encodedFields, err := json.Marshal(requiredFields)
	if err != nil {
		h.t.Fatalf("json.Marshal(requiredFields) error = %v", err)
	}
	h.writeProjectConfig(fmt.Sprintf("[ready]\nrequired_fields = %s\n", encodedFields))
}

func (h readyTestHarness) createIssue(input store.CreateIssueInput) model.Issue {
	h.t.Helper()
	issue, err := h.ap.Store.CreateIssue(h.ctx, input)
	if err != nil {
		h.t.Fatalf("CreateIssue(%q) error = %v", input.Title, err)
	}
	return issue
}

func (h readyTestHarness) closeIssue(issueID, reason string) {
	h.t.Helper()
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{
		IssueID:   issueID,
		Action:    "close",
		Reason:    reason,
		CreatedBy: "tester",
	}); err != nil {
		h.t.Fatalf("TransitionIssue(close) error = %v", err)
	}
}

func (h readyTestHarness) addBlocks(srcID, dstID string) {
	h.t.Helper()
	if _, err := h.ap.Store.AddRelation(h.ctx, store.AddRelationInput{
		SrcID:     srcID,
		DstID:     dstID,
		Type:      "blocks",
		CreatedBy: "agent",
	}); err != nil {
		h.t.Fatalf("AddRelation(blocks) error = %v", err)
	}
}

func (h readyTestHarness) runReadyJSON(args ...string) []annotation.AnnotatedIssue {
	h.t.Helper()
	var stdout bytes.Buffer
	allArgs := append(append([]string{}, args...), "--json")
	if err := runReady(h.ctx, &stdout, h.ap, allArgs); err != nil {
		h.t.Fatalf("runReady(%v) error = %v", allArgs, err)
	}
	var got []annotation.AnnotatedIssue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		h.t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}
	return got
}

func (h readyTestHarness) runReadyText(args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	if err := runReady(h.ctx, &stdout, h.ap, args); err != nil {
		h.t.Fatalf("runReady(%v) error = %v", args, err)
	}
	return stdout.String()
}

func (h readyTestHarness) runReadyErr(args ...string) error {
	h.t.Helper()
	var stdout bytes.Buffer
	return runReady(h.ctx, &stdout, h.ap, args)
}

func findAnnotation(annotations []annotation.Annotation, kind annotation.Kind) (annotation.Annotation, bool) {
	for _, item := range annotations {
		if item.Kind == kind {
			return item, true
		}
	}
	return annotation.Annotation{}, false
}

func TestRunReadyAnnotatesBlockedIssues(t *testing.T) {
	h := newReadyTestHarness(t)

	openA := h.createIssue(store.CreateIssueInput{
		Title:     "Open issue A",
		Topic:     "alpha",
		IssueType: "task",
		Priority:  2,
		Assignee:  "alice",
	})
	openB := h.createIssue(store.CreateIssueInput{
		Title:     "Open issue B",
		Topic:     "bravo",
		IssueType: "bug",
		Priority:  1,
		Assignee:  "bob",
	})
	h.addBlocks(openB.ID, openA.ID)

	closed := h.createIssue(store.CreateIssueInput{
		Title:     "Already done",
		Topic:     "closed",
		IssueType: "task",
		Priority:  0,
	})
	h.closeIssue(closed.ID, "not ready work")

	got := h.runReadyJSON()

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got=%#v", len(got), got)
	}
	if isReadyBlocked(got[0].Annotations) {
		t.Fatalf("got[0] should not be blocked, annotations=%#v", got[0].Annotations)
	}
	if got[0].ID != openA.ID {
		t.Fatalf("got[0].ID = %q, want %q", got[0].ID, openA.ID)
	}
	if !isReadyBlocked(got[1].Annotations) {
		t.Fatalf("got[1] should be blocked, annotations=%#v", got[1].Annotations)
	}
	if got[1].ID != openB.ID {
		t.Fatalf("got[1].ID = %q, want %q", got[1].ID, openB.ID)
	}
	blocker, ok := findAnnotation(got[1].Annotations, annotation.BlockedBy)
	if !ok {
		t.Fatalf("got[1] missing blocked_by annotation: %#v", got[1].Annotations)
	}
	if blocker.Message != openA.ID {
		t.Fatalf("blocked_by message = %q, want %q", blocker.Message, openA.ID)
	}
}

func TestRunReadySupportsAssigneeAndLimit(t *testing.T) {
	h := newReadyTestHarness(t)

	h.createIssue(store.CreateIssueInput{
		Title:     "Alice old",
		Topic:     "alice",
		IssueType: "task",
		Priority:  1,
		Assignee:  "alice",
	})
	h.createIssue(store.CreateIssueInput{
		Title:     "Bob task",
		Topic:     "bob",
		IssueType: "task",
		Priority:  0,
		Assignee:  "bob",
	})

	got := h.runReadyJSON("--assignee", "alice", "--limit", "1")

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1; got=%#v", len(got), got)
	}
	if got[0].Assignee != "alice" {
		t.Fatalf("got[0].Assignee = %q, want alice", got[0].Assignee)
	}
}

func TestRunReadyAcceptsOmitemptyRequiredFieldAndAnnotatesMissing(t *testing.T) {
	h := newReadyTestHarness(t)
	h.writeReadyConfig("assignee")

	issue := h.createIssue(store.CreateIssueInput{
		Title:       "Needs assignee",
		Topic:       "assignee",
		IssueType:   "task",
		Priority:    1,
		Description: "still missing assignee",
	})

	got := h.runReadyJSON()

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].ID != issue.ID {
		t.Fatalf("got[0].ID = %q, want %q", got[0].ID, issue.ID)
	}
	if !isReadyBlocked(got[0].Annotations) {
		t.Fatal("issue with missing required field should be blocked")
	}
	missingField, ok := findAnnotation(got[0].Annotations, annotation.MissingField)
	if !ok {
		t.Fatalf("got[0] missing missing_field annotation: %#v", got[0].Annotations)
	}
	if missingField.Message != "assignee" {
		t.Fatalf("missing_field message = %q, want assignee", missingField.Message)
	}
}

func TestRunReadyErrorsOnInvalidRequiredField(t *testing.T) {
	h := newReadyTestHarness(t)
	h.writeReadyConfig("made_up_field")

	err := h.runReadyErr("--json")
	if err == nil {
		t.Fatal("runReady expected error for invalid required field")
	}
	if !strings.Contains(err.Error(), "made_up_field") {
		t.Fatalf("error = %q, want mention of made_up_field", err.Error())
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error = %q, want 'does not exist' context", err.Error())
	}
}

func TestRunReadyTextOutputIncludesNotReadySectionAndReason(t *testing.T) {
	h := newReadyTestHarness(t)
	h.writeReadyConfig("description")

	h.createIssue(store.CreateIssueInput{
		Title:       "Ready ticket",
		Topic:       "ready",
		IssueType:   "task",
		Priority:    1,
		Description: "ship it",
	})
	h.createIssue(store.CreateIssueInput{
		Title:     "Missing description",
		Topic:     "missing",
		IssueType: "task",
		Priority:  2,
	})

	text := h.runReadyText()
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
	h := newReadyTestHarness(t)
	h.writeProjectConfig("[ready\nrequired_fields = [\"description\"]")

	err := h.runReadyErr("--json")
	if err == nil {
		t.Fatal("runReady expected config parse error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("runReady error = %q, want parse config context", err.Error())
	}
}
