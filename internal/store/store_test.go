package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func TestStoreCreateEpicAndRelations(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Renderer cleanup", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue epic error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Move pass validation", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue child error = %v", err)
	}
	related, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Guard shared buffers", IssueType: "feature", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue related error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: child.ID, DstID: epic.ID, Type: "parent-child", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation parent-child error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: child.ID, DstID: related.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation blocks error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: child.ID, DstID: related.ID, Type: "related-to", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation related-to error = %v", err)
	}
	if _, err := st.AddComment(ctx, AddCommentInput{IssueID: child.ID, Body: "Need compile boundary first.", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	detail, err := st.GetIssueDetail(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != epic.ID {
		t.Fatalf("parent = %#v, want %s", detail.Parent, epic.ID)
	}
	if len(detail.DependsOn) != 1 || detail.DependsOn[0].ID != related.ID {
		t.Fatalf("depends_on = %#v, want %s", detail.DependsOn, related.ID)
	}
	if len(detail.Related) != 1 || detail.Related[0].ID != related.ID {
		t.Fatalf("related = %#v, want %s", detail.Related, related.ID)
	}
	if len(detail.Comments) != 1 {
		t.Fatalf("comments len = %d, want 1", len(detail.Comments))
	}
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export error = %v", err)
	}
	if export.WorkspaceID != "test-workspace-id" {
		t.Fatalf("workspace_id = %q", export.WorkspaceID)
	}
	if len(export.Issues) != 3 {
		t.Fatalf("issues len = %d, want 3", len(export.Issues))
	}
}

func TestStoreRejectsInvalidIssueType(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Bad", IssueType: "weird", Priority: 2}); err == nil {
		t.Fatal("expected invalid issue type error")
	}
}

func TestStoreListIssuesSupportsAdvancedFilters(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	issueA, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:       "Renderer contract cleanup",
		Description: "Fix the renderer contract for draw prep.",
		IssueType:   "task",
		Priority:    1,
		Assignee:    "bmf",
		Labels:      []string{"renderer", "contract"},
	})
	if err != nil {
		t.Fatalf("CreateIssue issueA error = %v", err)
	}
	issueB, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:       "Fluid defaults",
		Description: "Tune the fluid presets.",
		IssueType:   "feature",
		Priority:    3,
		Assignee:    "e-prawn",
	})
	if err != nil {
		t.Fatalf("CreateIssue issueB error = %v", err)
	}
	if _, err := st.AddComment(ctx, AddCommentInput{IssueID: issueA.ID, Body: "Need compiler contract first.", CreatedBy: "bmf"}); err != nil {
		t.Fatalf("AddComment() error = %v", err)
	}

	now := time.Now().UTC()
	before := now.Add(-time.Hour)
	after := now.Add(time.Hour)
	hasComments := true
	issues, err := st.ListIssues(ctx, ListIssuesFilter{
		Status:        "open",
		IssueType:     "task",
		Assignee:      "bmf",
		PriorityMax:   intPtr(2),
		SearchTerms:   []string{"renderer", "draw prep"},
		IDs:           []string{issueA.ID, issueB.ID},
		LabelsAll:     []string{"renderer"},
		HasComments:   &hasComments,
		UpdatedAfter:  &before,
		UpdatedBefore: &after,
	})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].ID != issueA.ID {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestStoreLabelsAreWritableFirstClassData(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:     "Renderer cleanup",
		IssueType: "task",
		Priority:  1,
		Labels:    []string{"Renderer", "gpu"},
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if len(issue.Labels) != 2 || issue.Labels[0] != "gpu" || issue.Labels[1] != "renderer" {
		t.Fatalf("issue.Labels = %#v", issue.Labels)
	}

	labels, err := st.AddLabel(ctx, AddLabelInput{IssueID: issue.ID, Name: "contracts", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("AddLabel() error = %v", err)
	}
	if len(labels) != 3 {
		t.Fatalf("labels after add = %#v", labels)
	}

	updated, err := st.UpdateIssue(ctx, issue.ID, UpdateIssueInput{Labels: &[]string{"critical", "renderer"}})
	if err != nil {
		t.Fatalf("UpdateIssue() error = %v", err)
	}
	if len(updated.Labels) != 2 || updated.Labels[0] != "critical" || updated.Labels[1] != "renderer" {
		t.Fatalf("updated.Labels = %#v", updated.Labels)
	}

	labels, err = st.RemoveLabel(ctx, issue.ID, "critical")
	if err != nil {
		t.Fatalf("RemoveLabel() error = %v", err)
	}
	if len(labels) != 1 || labels[0] != "renderer" {
		t.Fatalf("labels after remove = %#v", labels)
	}

	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.Issue.Labels) != 1 || detail.Issue.Labels[0] != "renderer" {
		t.Fatalf("detail.Issue.Labels = %#v", detail.Issue.Labels)
	}

	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if len(export.Labels) != 1 || export.Labels[0].Name != "renderer" {
		t.Fatalf("export.Labels = %#v", export.Labels)
	}
}

func intPtr(value int) *int { return &value }

func TestReplaceFromExportAndSyncState(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Renderer cleanup", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	export := model.Export{
		Version:     1,
		WorkspaceID: "foreign-workspace",
		ExportedAt:  time.Now().UTC(),
		Issues: []model.Issue{{
			ID:          "issue-replaced",
			Title:       "Imported issue",
			Description: "from file sync",
			Status:      "open",
			Priority:    2,
			IssueType:   "task",
			Labels:      []string{"imported"},
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}},
		Labels: []model.Label{{
			IssueID:   "issue-replaced",
			Name:      "imported",
			CreatedAt: time.Now().UTC(),
			CreatedBy: "sync",
		}},
		History: []model.IssueHistory{{
			ID:         "hist-1",
			IssueID:    "issue-replaced",
			Action:     "created",
			Reason:     "imported from sync",
			FromStatus: "",
			ToStatus:   "open",
			CreatedAt:  time.Now().UTC(),
			CreatedBy:  "sync",
		}},
	}
	if err := st.ReplaceFromExport(ctx, export); err != nil {
		t.Fatalf("ReplaceFromExport() error = %v", err)
	}

	issues, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "issue-replaced" {
		t.Fatalf("issues = %#v", issues)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "imported" {
		t.Fatalf("labels = %#v", issues[0].Labels)
	}

	state := SyncState{Path: "/tmp/export.json", ContentHash: "abc123"}
	if err := st.RecordSyncState(ctx, state); err != nil {
		t.Fatalf("RecordSyncState() error = %v", err)
	}
	loadedState, err := st.GetSyncState(ctx)
	if err != nil {
		t.Fatalf("GetSyncState() error = %v", err)
	}
	encoded, _ := json.Marshal(loadedState)
	if string(encoded) == "" || loadedState.Path != state.Path || loadedState.ContentHash != state.ContentHash {
		t.Fatalf("loadedState = %#v", loadedState)
	}

	if _, err := st.GetIssue(ctx, issue.ID); err == nil {
		t.Fatalf("expected original issue %s to be replaced", issue.ID)
	}
}

func TestIssueLifecycleTracksReasonHistory(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Renderer cleanup", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	closed, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "close", Reason: "done", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}
	if closed.Status != "closed" || closed.ClosedAt == nil {
		t.Fatalf("closed = %#v", closed)
	}
	reopened, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "reopen", Reason: "follow-up work", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(reopen) error = %v", err)
	}
	if reopened.Status != "open" || reopened.ClosedAt != nil {
		t.Fatalf("reopened = %#v", reopened)
	}
	archived, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "archive", Reason: "inactive", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(archive) error = %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Fatalf("archived = %#v", archived)
	}

	activeIssues, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if len(activeIssues) != 0 {
		t.Fatalf("activeIssues = %#v", activeIssues)
	}

	allIssues, err := st.ListIssues(ctx, ListIssuesFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListIssues(include archived) error = %v", err)
	}
	if len(allIssues) != 1 || allIssues[0].ID != issue.ID {
		t.Fatalf("allIssues = %#v", allIssues)
	}

	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.History) != 4 {
		t.Fatalf("history = %#v", detail.History)
	}
	if detail.History[1].Action != "close" || detail.History[1].Reason != "done" {
		t.Fatalf("history[1] = %#v", detail.History[1])
	}
	if detail.History[2].Action != "reopen" || detail.History[2].Reason != "follow-up work" {
		t.Fatalf("history[2] = %#v", detail.History[2])
	}
	if detail.History[3].Action != "archive" || detail.History[3].Reason != "inactive" {
		t.Fatalf("history[3] = %#v", detail.History[3])
	}
}
