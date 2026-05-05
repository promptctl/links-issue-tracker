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
	if input.Prefix == "" {
		input.Prefix = h.ap.Workspace.IssuePrefix
	}
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

// addDependency creates a "blocks" relation: dependent depends on dependency.
// In the relation table: SrcID=dependent, DstID=dependency.
func (h readyTestHarness) addDependency(dependentID, dependencyID string) {
	h.t.Helper()
	if _, err := h.ap.Store.AddRelation(h.ctx, store.AddRelationInput{
		SrcID:     dependentID,
		DstID:     dependencyID,
		Type:      "blocks",
		CreatedBy: "agent",
	}); err != nil {
		h.t.Fatalf("AddRelation(blocks) error = %v", err)
	}
}

func (h readyTestHarness) runReadyJSON(args ...string) []annotation.AnnotatedIssue {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	allArgs := append(append([]string{}, args...), "--json")
	if err := runReady(h.ctx, &stdout, &stderr, h.ap, allArgs); err != nil {
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
	stdout, _ := h.runReadyTextStreams(args...)
	return stdout
}

func (h readyTestHarness) runReadyTextStreams(args ...string) (string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runReady(h.ctx, &stdout, &stderr, h.ap, args); err != nil {
		h.t.Fatalf("runReady(%v) error = %v", args, err)
	}
	return stdout.String(), stderr.String()
}

func (h readyTestHarness) runReadyErr(args ...string) error {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	return runReady(h.ctx, &stdout, &stderr, h.ap, args)
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

	openA := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Open issue A",
		Topic:     "alpha",
		IssueType: "task",
		Priority:  2,
		Assignee:  "alice",
	})
	openB := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Open issue B",
		Topic:     "bravo",
		IssueType: "bug",
		Priority:  1,
		Assignee:  "bob",
	})
	h.addDependency(openB.ID, openA.ID)

	closed := h.createIssue(store.CreateIssueInput{Prefix: "test", 
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
	blocker, ok := findAnnotation(got[1].Annotations, annotation.OpenDependency)
	if !ok {
		t.Fatalf("got[1] missing open_dependency annotation: %#v", got[1].Annotations)
	}
	if blocker.Message != openA.ID {
		t.Fatalf("open_dependency message = %q, want %q", blocker.Message, openA.ID)
	}
}

func TestRunReadyMarksNeedsDesignLabelAsBlocked(t *testing.T) {
	h := newReadyTestHarness(t)

	plain := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Ready leaf",
		Topic:     "alpha",
		IssueType: "task",
		Priority:  2,
	})
	flagged := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Needs design first",
		Topic:     "alpha",
		IssueType: "task",
		Priority:  2,
		Labels:    []string{NeedsDesignLabel},
	})

	got := h.runReadyJSON()
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got=%#v", len(got), got)
	}

	byID := map[string]annotation.AnnotatedIssue{got[0].ID: got[0], got[1].ID: got[1]}
	if isReadyBlocked(byID[plain.ID].Annotations) {
		t.Fatalf("plain issue should not be blocked, annotations=%#v", byID[plain.ID].Annotations)
	}
	if !isReadyBlocked(byID[flagged.ID].Annotations) {
		t.Fatalf("needs-design issue should be blocked, annotations=%#v", byID[flagged.ID].Annotations)
	}
	if _, ok := findAnnotation(byID[flagged.ID].Annotations, annotation.NeedsDesign); !ok {
		t.Fatalf("missing NeedsDesign annotation: %#v", byID[flagged.ID].Annotations)
	}
}

func TestRunReadySupportsAssigneeAndLimit(t *testing.T) {
	h := newReadyTestHarness(t)

	h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Alice old",
		Topic:     "alice",
		IssueType: "task",
		Priority:  1,
		Assignee:  "alice",
	})
	h.createIssue(store.CreateIssueInput{Prefix: "test", 
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
	if got[0].AssigneeValue() != "alice" {
		t.Fatalf("got[0].AssigneeValue() = %q, want alice", got[0].AssigneeValue())
	}
}

func TestRunReadyAcceptsOmitemptyRequiredFieldAndAnnotatesMissing(t *testing.T) {
	h := newReadyTestHarness(t)
	h.writeReadyConfig("assignee")

	issue := h.createIssue(store.CreateIssueInput{Prefix: "test", 
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

	issue := h.createIssue(store.CreateIssueInput{Prefix: "test", 
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
	if got[0].State() != model.StateInProgress {
		t.Fatalf("got[0].State() = %q, want in_progress", got[0].State())
	}
}

func TestRunReadyAnnotatesOrphanedInProgressIssues(t *testing.T) {
	h := newReadyTestHarness(t)

	issue := h.createIssue(store.CreateIssueInput{Prefix: "test", 
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

	issue := h.createIssue(store.CreateIssueInput{Prefix: "test", 
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
	first := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "First issue (better rank)",
		Topic:     "first",
		IssueType: "task",
		Priority:  1,
	})
	second := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Second issue (worse rank)",
		Topic:     "second",
		IssueType: "task",
		Priority:  4,
	})
	// first depends on second — second (dependency) has worse rank → inversion.
	h.addDependency(first.ID, second.ID)

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
	first := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "First issue (better rank)",
		Topic:     "first",
		IssueType: "task",
		Priority:  1,
	})
	second := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Second issue (worse rank)",
		Topic:     "second",
		IssueType: "task",
		Priority:  4,
	})
	// second depends on first — first (dependency) ranked above second → no inversion.
	h.addDependency(second.ID, first.ID)

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

	first := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "First issue",
		Topic:     "first",
		IssueType: "task",
		Priority:  1,
	})
	second := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Second issue",
		Topic:     "second",
		IssueType: "task",
		Priority:  4,
	})
	// first depends on second — second (dependency) has worse rank → inversion.
	h.addDependency(first.ID, second.ID)

	text := h.runReadyText()
	if !strings.Contains(text, "rank inversion") {
		t.Fatalf("text output missing rank inversion warning: %q", text)
	}
	if !strings.Contains(text, "lit doctor --fix") {
		t.Fatalf("text output missing fix instructions: %q", text)
	}
}

func TestRunReadyPreambleGoesToStderr(t *testing.T) {
	h := newReadyTestHarness(t)

	h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Some task",
		Topic:     "task",
		IssueType: "task",
		Priority:  1,
	})

	stdout, stderr := h.runReadyTextStreams()
	if !strings.Contains(stderr, "This is the backlog") {
		t.Fatal("stderr missing preamble")
	}
	if !strings.Contains(stderr, "─") {
		t.Fatal("stderr missing separator line")
	}
	if strings.Contains(stdout, "This is the backlog") {
		t.Fatal("preamble leaked to stdout; it must be parseable-data only")
	}
}

func TestRunReadyTextOutputShowsNumberedItems(t *testing.T) {
	h := newReadyTestHarness(t)

	a := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title: "First", Topic: "aaa", IssueType: "task", Priority: 1,
	})
	b := h.createIssue(store.CreateIssueInput{Prefix: "test", 
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

	blocker := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title: "Blocker", Topic: "blk", IssueType: "task", Priority: 1,
	})
	dependent := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title: "Dependent", Topic: "dep", IssueType: "task", Priority: 2,
	})
	h.addDependency(dependent.ID, blocker.ID)

	text := h.runReadyText()
	if !strings.Contains(text, "unblocks: "+dependent.ID) {
		t.Fatalf("expected unblocks line for blocker, got: %s", text)
	}
}

func TestRunReadyTextOutputCapsAt10(t *testing.T) {
	h := newReadyTestHarness(t)

	for i := 0; i < 12; i++ {
		h.createIssue(store.CreateIssueInput{Prefix: "test", 
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

// [LAW:dataflow-not-control-flow] (links-agent-epic-model-uew.1)
// Epics are never workable entries in `ready`: the data boundary excludes
// them, so downstream annotation / sort / render code never sees them.
func TestRunReadyExcludesEpics(t *testing.T) {
	h := newReadyTestHarness(t)

	epic := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Epic container",
		Topic:     "epic-topic",
		IssueType: "epic",
		Priority:  1,
	})
	leafTask := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Leaf task under epic",
		Topic:     "epic-topic",
		IssueType: "task",
		Priority:  1,
		ParentID:  epic.ID,
	})
	standaloneBug := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Standalone bug",
		Topic:     "bug-topic",
		IssueType: "bug",
		Priority:  1,
	})

	got := h.runReadyJSON()

	gotIDs := make(map[string]bool, len(got))
	for _, entry := range got {
		gotIDs[entry.ID] = true
		if entry.IssueType == "epic" {
			t.Errorf("ready returned epic %q; epics should be filtered out", entry.ID)
		}
	}
	if !gotIDs[leafTask.ID] {
		t.Errorf("ready missing leaf task %q; got=%v", leafTask.ID, gotIDs)
	}
	if !gotIDs[standaloneBug.ID] {
		t.Errorf("ready missing standalone bug %q; got=%v", standaloneBug.ID, gotIDs)
	}
	if gotIDs[epic.ID] {
		t.Errorf("ready included epic %q; want excluded", epic.ID)
	}
}

// [LAW:dataflow-not-control-flow] (links-agent-epic-model-uew.2)
// Each ready row carries its parent epic inline when the parent is type=epic,
// so an agent scanning ready knows which epic they'd be joining before they
// claim a leaf. Rows without an epic parent get no ParentEpic field.
func TestRunReadyCarriesParentEpic(t *testing.T) {
	h := newReadyTestHarness(t)

	epic := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Integrate foo subsystem end-to-end",
		Topic:     "epic-topic",
		IssueType: "epic",
		Priority:  1,
	})
	leaf := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Wire up the frobnicator",
		Topic:     "epic-topic",
		IssueType: "task",
		Priority:  1,
		ParentID:  epic.ID,
	})
	standalone := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Standalone bug",
		Topic:     "bug-topic",
		IssueType: "bug",
		Priority:  1,
	})
	nonEpicParent := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Parent feature",
		Topic:     "feat-topic",
		IssueType: "feature",
		Priority:  1,
	})
	childOfFeature := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Feature subtask",
		Topic:     "feat-topic",
		IssueType: "task",
		Priority:  1,
		ParentID:  nonEpicParent.ID,
	})

	got := h.runReadyJSON()

	byID := make(map[string]annotation.AnnotatedIssue, len(got))
	for _, entry := range got {
		byID[entry.ID] = entry
	}

	leafRow, ok := byID[leaf.ID]
	if !ok {
		t.Fatalf("leaf %q missing from ready output", leaf.ID)
	}
	if leafRow.ParentEpic == nil {
		t.Fatalf("leaf row missing ParentEpic; want {ID=%q Title=%q}", epic.ID, epic.Title)
	}
	if leafRow.ParentEpic.ID != epic.ID {
		t.Errorf("ParentEpic.ID = %q, want %q", leafRow.ParentEpic.ID, epic.ID)
	}
	if leafRow.ParentEpic.Title != epic.Title {
		t.Errorf("ParentEpic.Title = %q, want %q", leafRow.ParentEpic.Title, epic.Title)
	}

	if standaloneRow, ok := byID[standalone.ID]; !ok {
		t.Errorf("standalone %q missing from ready output", standalone.ID)
	} else if standaloneRow.ParentEpic != nil {
		t.Errorf("standalone row has ParentEpic=%+v; want nil", standaloneRow.ParentEpic)
	}

	if featureChildRow, ok := byID[childOfFeature.ID]; !ok {
		t.Errorf("feature child %q missing from ready output", childOfFeature.ID)
	} else if featureChildRow.ParentEpic != nil {
		t.Errorf("feature-child row has ParentEpic=%+v; want nil (parent is non-epic)", featureChildRow.ParentEpic)
	}

	text := h.runReadyText()
	if !strings.Contains(text, "epic: "+epic.ID+"  "+epic.Title) {
		t.Errorf("text output missing 'epic: %s  %s' line; output:\n%s", epic.ID, epic.Title, text)
	}
}

// [LAW:dataflow-not-control-flow] (links-agent-epic-model-uew.4)
// Leaves sort by (effective_epic_rank, own_rank), so all leaves under epic A
// appear before any leaves under epic B when A ranks higher than B — even
// when the leaves were created in interleaved order and their own ranks
// alternate between epics.
func TestRunReadyOrdersLeavesByCompositeRank(t *testing.T) {
	h := newReadyTestHarness(t)

	epicA := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Epic A",
		Topic:     "epic-a",
		IssueType: "epic",
		Priority:  1,
	})
	epicB := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "Epic B",
		Topic:     "epic-b",
		IssueType: "epic",
		Priority:  1,
	})
	leafA1 := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "A.1",
		Topic:     "epic-a",
		IssueType: "task",
		ParentID:  epicA.ID,
		Priority:  1,
	})
	leafB1 := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "B.1",
		Topic:     "epic-b",
		IssueType: "task",
		ParentID:  epicB.ID,
		Priority:  1,
	})
	leafA2 := h.createIssue(store.CreateIssueInput{Prefix: "test", 
		Title:     "A.2",
		Topic:     "epic-a",
		IssueType: "task",
		ParentID:  epicA.ID,
		Priority:  1,
	})

	got := h.runReadyJSON()

	gotIDs := make([]string, len(got))
	for i, entry := range got {
		gotIDs[i] = entry.ID
	}
	want := []string{leafA1.ID, leafA2.ID, leafB1.ID}
	if len(gotIDs) != len(want) {
		t.Fatalf("ready returned %d rows, want %d; ids=%v", len(gotIDs), len(want), gotIDs)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("ready row %d = %q, want %q; full order=%v", i, gotIDs[i], want[i], gotIDs)
		}
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
