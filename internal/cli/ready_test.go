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

func TestRunReadyAnnotatesPriorityInversion(t *testing.T) {
	h := newReadyTestHarness(t)

	// highPri (P1) depends on lowPri (P4) — that's an inversion.
	lowPri := h.createIssue(store.CreateIssueInput{
		Title:     "Low priority blocker",
		Topic:     "low",
		IssueType: "task",
		Priority:  4,
	})
	highPri := h.createIssue(store.CreateIssueInput{
		Title:     "High priority dependent",
		Topic:     "high",
		IssueType: "task",
		Priority:  1,
	})
	h.addBlocks(highPri.ID, lowPri.ID)

	got := h.runReadyJSON()

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	var highEntry annotation.AnnotatedIssue
	for _, entry := range got {
		if entry.ID == highPri.ID {
			highEntry = entry
			break
		}
	}
	if highEntry.ID == "" {
		t.Fatal("high priority issue not found in output")
	}
	inv, ok := findAnnotation(highEntry.Annotations, annotation.PriorityInversion)
	if !ok {
		t.Fatalf("high priority issue missing priority_inversion annotation: %#v", highEntry.Annotations)
	}
	if !strings.Contains(inv.Message, lowPri.ID) {
		t.Fatalf("priority_inversion message = %q, want to contain %q", inv.Message, lowPri.ID)
	}
}

func TestRunReadyNoPriorityInversionWhenBlockerIsHigherPriority(t *testing.T) {
	h := newReadyTestHarness(t)

	// lowPri (P4) depends on highPri (P1) — no inversion.
	highPri := h.createIssue(store.CreateIssueInput{
		Title:     "High priority blocker",
		Topic:     "high",
		IssueType: "task",
		Priority:  1,
	})
	lowPri := h.createIssue(store.CreateIssueInput{
		Title:     "Low priority dependent",
		Topic:     "low",
		IssueType: "task",
		Priority:  4,
	})
	h.addBlocks(lowPri.ID, highPri.ID)

	got := h.runReadyJSON()

	for _, entry := range got {
		if entry.ID == lowPri.ID {
			if _, ok := findAnnotation(entry.Annotations, annotation.PriorityInversion); ok {
				t.Fatalf("low priority dependent should NOT have priority_inversion: %#v", entry.Annotations)
			}
			return
		}
	}
	t.Fatal("low priority issue not found in output")
}

func TestRunReadyTextOutputShowsPriorityInversions(t *testing.T) {
	h := newReadyTestHarness(t)

	lowPri := h.createIssue(store.CreateIssueInput{
		Title:     "Low priority blocker",
		Topic:     "low",
		IssueType: "task",
		Priority:  4,
	})
	highPri := h.createIssue(store.CreateIssueInput{
		Title:     "High priority dependent",
		Topic:     "high",
		IssueType: "task",
		Priority:  1,
	})
	h.addBlocks(highPri.ID, lowPri.ID)

	text := h.runReadyText()
	if !strings.Contains(text, "Priority inversions") {
		t.Fatalf("text output missing priority inversions section: %q", text)
	}
	if !strings.Contains(text, highPri.ID) {
		t.Fatalf("text output missing high priority issue ID: %q", text)
	}
	if !strings.Contains(text, "blocker is lower priority") {
		t.Fatalf("text output missing inversion explanation: %q", text)
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

func TestFixPriorityPullForwardPromotesBlockers(t *testing.T) {
	h := newReadyTestHarness(t)

	lowPri := h.createIssue(store.CreateIssueInput{
		Title:     "Low priority blocker",
		Topic:     "low",
		IssueType: "task",
		Priority:  4,
	})
	highPri := h.createIssue(store.CreateIssueInput{
		Title:     "High priority dependent",
		Topic:     "high",
		IssueType: "task",
		Priority:  1,
	})
	h.addBlocks(highPri.ID, lowPri.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{highPri.ID, "--pull-forward", "--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].ID != lowPri.ID {
		t.Fatalf("changes[0].ID = %q, want %q", changes[0].ID, lowPri.ID)
	}
	if changes[0].OldPriority != 4 {
		t.Fatalf("changes[0].OldPriority = %d, want 4", changes[0].OldPriority)
	}
	if changes[0].NewPriority != 1 {
		t.Fatalf("changes[0].NewPriority = %d, want 1", changes[0].NewPriority)
	}
	updated, err := h.ap.Store.GetIssue(h.ctx, lowPri.ID)
	if err != nil {
		t.Fatalf("GetIssue error = %v", err)
	}
	if updated.Priority != 1 {
		t.Fatalf("blocker priority after fix = %d, want 1", updated.Priority)
	}
}

func TestFixPriorityPushBackDemotesDependent(t *testing.T) {
	h := newReadyTestHarness(t)

	lowPri := h.createIssue(store.CreateIssueInput{
		Title:     "Low priority blocker",
		Topic:     "low",
		IssueType: "task",
		Priority:  4,
	})
	highPri := h.createIssue(store.CreateIssueInput{
		Title:     "High priority dependent",
		Topic:     "high",
		IssueType: "task",
		Priority:  1,
	})
	h.addBlocks(highPri.ID, lowPri.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{highPri.ID, "--push-back", "--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].ID != highPri.ID {
		t.Fatalf("changes[0].ID = %q, want %q", changes[0].ID, highPri.ID)
	}
	if changes[0].OldPriority != 1 {
		t.Fatalf("changes[0].OldPriority = %d, want 1", changes[0].OldPriority)
	}
	if changes[0].NewPriority != 4 {
		t.Fatalf("changes[0].NewPriority = %d, want 4", changes[0].NewPriority)
	}
}

func TestFixPriorityPullForwardWalksTransitiveChain(t *testing.T) {
	h := newReadyTestHarness(t)

	a := h.createIssue(store.CreateIssueInput{
		Title: "A", Topic: "aaa", IssueType: "task", Priority: 4,
	})
	b := h.createIssue(store.CreateIssueInput{
		Title: "B", Topic: "bbb", IssueType: "task", Priority: 3,
	})
	c := h.createIssue(store.CreateIssueInput{
		Title: "C", Topic: "ccc", IssueType: "task", Priority: 1,
	})
	h.addBlocks(c.ID, b.ID)
	h.addBlocks(b.ID, a.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{c.ID, "--pull-forward", "--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2; changes=%#v", len(changes), changes)
	}
	for _, c := range changes {
		if c.NewPriority != 1 {
			t.Fatalf("change %s: NewPriority = %d, want 1", c.ID, c.NewPriority)
		}
	}
}

func TestFixPriorityNoInversion(t *testing.T) {
	h := newReadyTestHarness(t)

	blocker := h.createIssue(store.CreateIssueInput{
		Title: "Blocker", Topic: "blk", IssueType: "task", Priority: 1,
	})
	dependent := h.createIssue(store.CreateIssueInput{
		Title: "Dependent", Topic: "dep", IssueType: "task", Priority: 3,
	})
	h.addBlocks(dependent.ID, blocker.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{dependent.ID, "--pull-forward", "--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("len(changes) = %d, want 0 (no inversion)", len(changes))
	}
}

func TestFixPriorityBatchPullForwardFixesAllInversions(t *testing.T) {
	h := newReadyTestHarness(t)

	// Two independent dependency chains with inversions.
	blockerA := h.createIssue(store.CreateIssueInput{
		Title: "Blocker A", Topic: "blocker-a", IssueType: "task", Priority: 4,
	})
	depA := h.createIssue(store.CreateIssueInput{
		Title: "Dependent A", Topic: "dep-a", IssueType: "task", Priority: 1,
	})
	h.addBlocks(depA.ID, blockerA.ID)

	blockerB := h.createIssue(store.CreateIssueInput{
		Title: "Blocker B", Topic: "blocker-b", IssueType: "task", Priority: 3,
	})
	depB := h.createIssue(store.CreateIssueInput{
		Title: "Dependent B", Topic: "dep-b", IssueType: "task", Priority: 2,
	})
	h.addBlocks(depB.ID, blockerB.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{"--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2; changes=%#v", len(changes), changes)
	}

	updatedA, _ := h.ap.Store.GetIssue(h.ctx, blockerA.ID)
	if updatedA.Priority != 1 {
		t.Fatalf("blockerA priority = %d, want 1", updatedA.Priority)
	}
	updatedB, _ := h.ap.Store.GetIssue(h.ctx, blockerB.ID)
	if updatedB.Priority != 2 {
		t.Fatalf("blockerB priority = %d, want 2", updatedB.Priority)
	}
}

func TestFixPriorityBatchPushBackFixesAllInversions(t *testing.T) {
	h := newReadyTestHarness(t)

	blocker := h.createIssue(store.CreateIssueInput{
		Title: "Low pri blocker", Topic: "lpb", IssueType: "task", Priority: 4,
	})
	dep := h.createIssue(store.CreateIssueInput{
		Title: "High pri dep", Topic: "hpd", IssueType: "task", Priority: 1,
	})
	h.addBlocks(dep.ID, blocker.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{"--push-back", "--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].ID != dep.ID {
		t.Fatalf("changes[0].ID = %q, want %q", changes[0].ID, dep.ID)
	}

	updated, _ := h.ap.Store.GetIssue(h.ctx, dep.ID)
	if updated.Priority != 4 {
		t.Fatalf("dep priority = %d, want 4", updated.Priority)
	}
}

func TestFixPriorityBatchNoInversions(t *testing.T) {
	h := newReadyTestHarness(t)

	blocker := h.createIssue(store.CreateIssueInput{
		Title: "Good blocker", Topic: "good-blocker", IssueType: "task", Priority: 1,
	})
	dep := h.createIssue(store.CreateIssueInput{
		Title: "Good dep", Topic: "good-dep", IssueType: "task", Priority: 3,
	})
	h.addBlocks(dep.ID, blocker.ID)

	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{"--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("len(changes) = %d, want 0", len(changes))
	}
}

func TestFixPriorityBothFlagsError(t *testing.T) {
	h := newReadyTestHarness(t)

	err := runFixPriority(h.ctx, &bytes.Buffer{}, h.ap, []string{"--pull-forward", "--push-back"})
	if err == nil {
		t.Fatal("expected error for both flags")
	}
	if !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("error = %q, want 'at most one' message", err.Error())
	}
}

func TestFixPriorityDefaultsPullForward(t *testing.T) {
	h := newReadyTestHarness(t)

	blocker := h.createIssue(store.CreateIssueInput{
		Title: "Blocker", Topic: "blk", IssueType: "task", Priority: 4,
	})
	dep := h.createIssue(store.CreateIssueInput{
		Title: "Dep", Topic: "dep", IssueType: "task", Priority: 1,
	})
	h.addBlocks(dep.ID, blocker.ID)

	// No flags at all — should default to pull-forward.
	var stdout bytes.Buffer
	err := runFixPriority(h.ctx, &stdout, h.ap, []string{"--json"})
	if err != nil {
		t.Fatalf("runFixPriority error = %v", err)
	}
	var changes []priorityChange
	if err := json.Unmarshal(stdout.Bytes(), &changes); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	// Pull-forward promotes the blocker, not the dependent.
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if changes[0].ID != blocker.ID {
		t.Fatalf("changes[0].ID = %q, want %q (blocker promoted)", changes[0].ID, blocker.ID)
	}
	updated, _ := h.ap.Store.GetIssue(h.ctx, blocker.ID)
	if updated.Priority != 1 {
		t.Fatalf("blocker priority = %d, want 1", updated.Priority)
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
