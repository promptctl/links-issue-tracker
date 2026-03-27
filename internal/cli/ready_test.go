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
	"time"

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

func (h readyTestHarness) backdateUpdatedAt(issueID string, age time.Duration) {
	h.t.Helper()
	backdated := time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
	if err := h.ap.Store.ExecRawForTest(h.ctx, "UPDATE issues SET updated_at = ? WHERE id = ?", backdated, issueID); err != nil {
		h.t.Fatalf("backdateUpdatedAt(%q) error = %v", issueID, err)
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

func TestRunReadyShowsInProgressSection(t *testing.T) {
	h := newReadyTestHarness(t)

	issue := h.createIssue(store.CreateIssueInput{
		Title:     "Claimed work",
		Topic:     "claimed",
		IssueType: "task",
		Priority:  1,
	})
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "start",
		Reason:    "claim",
		CreatedBy: "agent",
	}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	got := h.runReadyJSON()
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].ID != issue.ID {
		t.Fatalf("got[0].ID = %q, want %q", got[0].ID, issue.ID)
	}
	if got[0].Status != "in_progress" {
		t.Fatalf("got[0].Status = %q, want in_progress", got[0].Status)
	}
}

func TestRunReadyAnnotatesOrphanedInProgressIssues(t *testing.T) {
	h := newReadyTestHarness(t)

	issue := h.createIssue(store.CreateIssueInput{
		Title:     "Stale work",
		Topic:     "stale",
		IssueType: "task",
		Priority:  1,
	})
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "start",
		Reason:    "claim",
		CreatedBy: "agent",
	}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	h.backdateUpdatedAt(issue.ID, 25*time.Hour)

	got := h.runReadyJSON()
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	_, ok := findAnnotation(got[0].Annotations, annotation.Orphaned)
	if !ok {
		t.Fatalf("expected orphaned annotation, got: %#v", got[0].Annotations)
	}
}

func TestRunReadyNoOrphanedAnnotationWhenRecent(t *testing.T) {
	h := newReadyTestHarness(t)

	issue := h.createIssue(store.CreateIssueInput{
		Title:     "Fresh work",
		Topic:     "fresh",
		IssueType: "task",
		Priority:  1,
	})
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "start",
		Reason:    "claim",
		CreatedBy: "agent",
	}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}

	got := h.runReadyJSON()
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if _, ok := findAnnotation(got[0].Annotations, annotation.Orphaned); ok {
		t.Fatalf("recently started issue should not be orphaned: %#v", got[0].Annotations)
	}
}

func TestRunReadyAnnotatesRankInversion(t *testing.T) {
	h := newReadyTestHarness(t)

	// first is created first (better rank), second is created second (worse rank).
	// second depends on first — first is ranked above second, no inversion.
	// But if we make first depend on second (second blocks first), second has
	// worse rank than first — that's a rank inversion.
	first := h.createIssue(store.CreateIssueInput{
		Title:     "First issue (better rank)",
		Topic:     "first",
		IssueType: "task",
		Priority:  1,
	})
	second := h.createIssue(store.CreateIssueInput{
		Title:     "Second issue (worse rank)",
		Topic:     "second",
		IssueType: "task",
		Priority:  4,
	})
	// first depends on second — second (dependency) has worse rank → inversion.
	h.addBlocks(first.ID, second.ID)

	got := h.runReadyJSON()

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	var firstEntry annotation.AnnotatedIssue
	for _, entry := range got {
		if entry.ID == first.ID {
			firstEntry = entry
			break
		}
	}
	if firstEntry.ID == "" {
		t.Fatal("first issue not found in output")
	}
	inv, ok := findAnnotation(firstEntry.Annotations, annotation.RankInversion)
	if !ok {
		t.Fatalf("first issue missing rank_inversion annotation: %#v", firstEntry.Annotations)
	}
	if !strings.Contains(inv.Message, second.ID) {
		t.Fatalf("rank_inversion message = %q, want to contain %q", inv.Message, second.ID)
	}
}

func TestRunReadyNoRankInversionWhenDependencyRankedAbove(t *testing.T) {
	h := newReadyTestHarness(t)

	// first is created first (better rank), second is created second (worse rank).
	// second depends on first — first (dependency) has better rank → no inversion.
	first := h.createIssue(store.CreateIssueInput{
		Title:     "First issue (better rank)",
		Topic:     "first",
		IssueType: "task",
		Priority:  1,
	})
	second := h.createIssue(store.CreateIssueInput{
		Title:     "Second issue (worse rank)",
		Topic:     "second",
		IssueType: "task",
		Priority:  4,
	})
	// second depends on first — first (dependency) ranked above second → no inversion.
	h.addBlocks(second.ID, first.ID)

	got := h.runReadyJSON()

	for _, entry := range got {
		if entry.ID == second.ID {
			if _, ok := findAnnotation(entry.Annotations, annotation.RankInversion); ok {
				t.Fatalf("should NOT have rank_inversion when dependency is ranked above: %#v", entry.Annotations)
			}
			return
		}
	}
	t.Fatal("second issue not found in output")
}

func TestRunReadyTextOutputShowsRankInversions(t *testing.T) {
	h := newReadyTestHarness(t)

	first := h.createIssue(store.CreateIssueInput{
		Title:     "First issue",
		Topic:     "first",
		IssueType: "task",
		Priority:  1,
	})
	second := h.createIssue(store.CreateIssueInput{
		Title:     "Second issue",
		Topic:     "second",
		IssueType: "task",
		Priority:  4,
	})
	// first depends on second — second (dependency) has worse rank → inversion.
	h.addBlocks(first.ID, second.ID)

	text := h.runReadyText()
	if !strings.Contains(text, "rank inversion") {
		t.Fatalf("text output missing rank inversion warning: %q", text)
	}
	if !strings.Contains(text, "lit doctor --fix") {
		t.Fatalf("text output missing fix instructions: %q", text)
	}
}

func TestRunReadyTextOutputIncludesPreamble(t *testing.T) {
	h := newReadyTestHarness(t)

	h.createIssue(store.CreateIssueInput{
		Title:     "Some task",
		Topic:     "task",
		IssueType: "task",
		Priority:  1,
	})

	text := h.runReadyText()
	if !strings.Contains(text, "This is the backlog") {
		t.Fatal("text output missing preamble")
	}
	if !strings.Contains(text, "─") {
		t.Fatal("text output missing separator line")
	}
}

func TestRunReadyTextOutputShowsNumberedItems(t *testing.T) {
	h := newReadyTestHarness(t)

	a := h.createIssue(store.CreateIssueInput{
		Title: "First", Topic: "aaa", IssueType: "task", Priority: 1,
	})
	b := h.createIssue(store.CreateIssueInput{
		Title: "Second", Topic: "bbb", IssueType: "task", Priority: 2,
	})

	text := h.runReadyText()
	aIdx := strings.Index(text, a.ID)
	bIdx := strings.Index(text, b.ID)
	if aIdx < 0 || bIdx < 0 {
		t.Fatalf("expected both issue IDs in output, got: %s", text)
	}
	if aIdx > bIdx {
		t.Fatal("higher priority issue should appear before lower priority")
	}
	if !strings.Contains(text, " 1. ") || !strings.Contains(text, " 2. ") {
		t.Fatal("expected numbered items in output")
	}
}

func TestRunReadyTextOutputShowsInlineDeps(t *testing.T) {
	h := newReadyTestHarness(t)

	blocker := h.createIssue(store.CreateIssueInput{
		Title: "Blocker", Topic: "blk", IssueType: "task", Priority: 1,
	})
	dependent := h.createIssue(store.CreateIssueInput{
		Title: "Dependent", Topic: "dep", IssueType: "task", Priority: 2,
	})
	h.addBlocks(dependent.ID, blocker.ID)

	text := h.runReadyText()
	if !strings.Contains(text, "unblocks: "+dependent.ID) {
		t.Fatalf("expected unblocks line for blocker, got: %s", text)
	}
}

func TestRunReadyTextOutputCapsAt10(t *testing.T) {
	h := newReadyTestHarness(t)

	for i := 0; i < 12; i++ {
		h.createIssue(store.CreateIssueInput{
			Title:     fmt.Sprintf("Task %d", i),
			Topic:     fmt.Sprintf("topic-%02d", i),
			IssueType: "task",
			Priority:  i % 5,
		})
	}

	text := h.runReadyText()
	if !strings.Contains(text, "10. ") {
		t.Fatal("expected 10th numbered item")
	}
	if strings.Contains(text, "11. ") {
		t.Fatal("should not show 11th numbered item")
	}
	if !strings.Contains(text, "2 more ready tickets not shown") {
		t.Fatalf("expected overflow message, got: %s", text)
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
