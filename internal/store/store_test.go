package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/doltcli"
	"github.com/bmf/links-issue-tracker/internal/issueid"
	"github.com/bmf/links-issue-tracker/internal/model"
)

func openIssueStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := st.EnsureIssuePrefix(ctx, "test"); err != nil {
		t.Fatalf("EnsureIssuePrefix() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return st
}

func issueIDs(issues []model.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func containsIssueID(issues []model.Issue, id string) bool {
	for _, issue := range issues {
		if issue.ID == id {
			return true
		}
	}
	return false
}

func TestStoreCreateEpicAndRelations(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Renderer cleanup", Topic: "renderer", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue epic error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Move pass validation", Topic: "renderer", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue child error = %v", err)
	}
	related, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Guard shared buffers", Topic: "renderer", IssueType: "feature", Priority: 2})
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

func TestEpicLifecycleCapabilitiesAndProgress(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Container", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	openLeaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Open leaf", Topic: "life", IssueType: "task", Priority: 2, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(open leaf) error = %v", err)
	}
	closedLeaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Closed leaf", Topic: "life", IssueType: "task", Priority: 2, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(closed leaf) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedLeaf.ID, Action: "start", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedLeaf.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(done) error = %v", err)
	}
	leaf, err := st.GetIssue(ctx, openLeaf.ID)
	if err != nil {
		t.Fatalf("GetIssue(open leaf) error = %v", err)
	}
	if leaf.Capabilities().Status == nil {
		t.Fatalf("leaf Capabilities().Status = nil, want owned status")
	}
	loadedEpic, err := st.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue(epic) error = %v", err)
	}
	if loadedEpic.Capabilities().Status != nil {
		t.Fatalf("epic Capabilities().Status = %#v, want nil", loadedEpic.Capabilities().Status)
	}
	progress := loadedEpic.Progress()
	if progress.Open != 1 || progress.Closed != 1 || progress.Total != 2 {
		t.Fatalf("epic Progress() = %#v, want open=1 closed=1 total=2", progress)
	}
	issues, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if !containsIssueID(issues, epic.ID) {
		t.Fatalf("ListIssues() ids=%v, want epic %s included", issueIDs(issues), epic.ID)
	}
}

// Epic state is derived from children; ListIssues filters by that derived state
// rather than the dead i.status DB column. This regression test pins the three
// epic shapes against the canonical default and explicit status filters.
func TestListIssuesStatusFilterUsesDerivedEpicState(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	openEpic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "All open", Topic: "derived", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(openEpic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Open child", Topic: "derived", IssueType: "task", Priority: 2, ParentID: openEpic.ID}); err != nil {
		t.Fatalf("CreateIssue(openEpic child) error = %v", err)
	}

	mixedEpic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Mixed children", Topic: "derived", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(mixedEpic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Mixed open", Topic: "derived", IssueType: "task", Priority: 2, ParentID: mixedEpic.ID}); err != nil {
		t.Fatalf("CreateIssue(mixedEpic open child) error = %v", err)
	}
	mixedClosedChild, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Mixed closed", Topic: "derived", IssueType: "task", Priority: 2, ParentID: mixedEpic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(mixedEpic closed child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: mixedClosedChild.ID, Action: "start", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(mixed closed start) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: mixedClosedChild.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(mixed closed done) error = %v", err)
	}

	closedEpic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "All closed", Topic: "derived", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(closedEpic) error = %v", err)
	}
	closedChild, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Closed child", Topic: "derived", IssueType: "task", Priority: 2, ParentID: closedEpic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(closedEpic child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedChild.ID, Action: "start", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(closed child start) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedChild.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(closed child done) error = %v", err)
	}

	cases := []struct {
		name     string
		statuses []string
		want     map[string]bool
	}{
		{name: "default open+in_progress", statuses: []string{"open", "in_progress"}, want: map[string]bool{openEpic.ID: true, mixedEpic.ID: true, closedEpic.ID: false}},
		{name: "open only", statuses: []string{"open"}, want: map[string]bool{openEpic.ID: true, mixedEpic.ID: false, closedEpic.ID: false}},
		{name: "in_progress only", statuses: []string{"in_progress"}, want: map[string]bool{openEpic.ID: false, mixedEpic.ID: true, closedEpic.ID: false}},
		{name: "closed only", statuses: []string{"closed"}, want: map[string]bool{openEpic.ID: false, mixedEpic.ID: false, closedEpic.ID: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, err := st.ListIssues(ctx, ListIssuesFilter{Statuses: tc.statuses, IssueTypes: []string{"epic"}})
			if err != nil {
				t.Fatalf("ListIssues(%v) error = %v", tc.statuses, err)
			}
			got := map[string]bool{}
			for _, issue := range issues {
				got[issue.ID] = true
			}
			for id, expect := range tc.want {
				if got[id] != expect {
					t.Fatalf("ListIssues(%v) epic %s present=%v, want %v (got ids=%v)", tc.statuses, id, got[id], expect, issueIDs(issues))
				}
			}
		})
	}
}

func TestFixRankInversionsConvergesWhenDependencyBlocksMultipleIssues(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	dependentA, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependent A", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(dependentA) error = %v", err)
	}
	dependentB, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependent B", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(dependentB) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Shared blocker", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(blocker) error = %v", err)
	}

	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: dependentA.ID, DstID: blocker.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(A blocks blocker) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: dependentB.ID, DstID: blocker.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(B blocks blocker) error = %v", err)
	}

	before, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(before) error = %v", err)
	}
	if before.RankInversions != 2 {
		t.Fatalf("Doctor(before).RankInversions = %d, want 2", before.RankInversions)
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 1 {
		t.Fatalf("FixRankInversions() fixed = %d, want 1 (one dependency issue reranked)", fixed)
	}

	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(after) error = %v", err)
	}
	if after.RankInversions != 0 {
		t.Fatalf("Doctor(after).RankInversions = %d, want 0", after.RankInversions)
	}
}

func TestFixRankInversionsConvergesWhenPassCreatesNewInversion(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependent", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	upstream, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Upstream blocker", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(upstream) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Middle blocker", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(blocker) error = %v", err)
	}

	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: dependent.ID, DstID: blocker.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(dependent blocks blocker) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: blocker.ID, DstID: upstream.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(blocker blocks upstream) error = %v", err)
	}

	before, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(before) error = %v", err)
	}
	if before.RankInversions != 1 {
		t.Fatalf("Doctor(before).RankInversions = %d, want 1", before.RankInversions)
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed < 1 {
		t.Fatalf("FixRankInversions() fixed = %d, want >= 1", fixed)
	}

	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(after) error = %v", err)
	}
	if after.RankInversions != 0 {
		t.Fatalf("Doctor(after).RankInversions = %d, want 0", after.RankInversions)
	}
}

// Regression: dst.status is NULL for epic dependencies (state lives in the
// AllOf lifecycle, not the column). The previous `dst.status != 'closed'`
// filter evaluated NULL as not-true and silently excluded every blocks-edge
// pointing at an open epic — Doctor reported 0 inversions and --fix was a
// no-op even when ready.go's annotator flagged the same edge.
func TestFixRankInversionsDetectsEpicDependency(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Release", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Blocking epic", Topic: "rank", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Epic child", Topic: "rank", IssueType: "task", Priority: 2, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue(epic child) error = %v", err)
	}
	if err := st.RankToBottom(ctx, epic.ID); err != nil {
		t.Fatalf("RankToBottom(epic) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: dependent.ID, DstID: epic.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(dependent blocks epic) error = %v", err)
	}

	before, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(before) error = %v", err)
	}
	if before.RankInversions != 1 {
		t.Fatalf("Doctor(before).RankInversions = %d, want 1 (epic dependency ranked below dependent)", before.RankInversions)
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 1 {
		t.Fatalf("FixRankInversions() fixed = %d, want 1", fixed)
	}

	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(after) error = %v", err)
	}
	if after.RankInversions != 0 {
		t.Fatalf("Doctor(after).RankInversions = %d, want 0", after.RankInversions)
	}
}

func TestFixRankInversionsIgnoresClosedEpic(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependent", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Closed epic", Topic: "rank", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Closed child", Topic: "rank", IssueType: "task", Priority: 2, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(epic child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "start", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(child start) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(child done) error = %v", err)
	}
	if err := st.RankToBottom(ctx, epic.ID); err != nil {
		t.Fatalf("RankToBottom(epic) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: dependent.ID, DstID: epic.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(dependent blocks epic) error = %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.RankInversions != 0 {
		t.Fatalf("Doctor().RankInversions = %d, want 0 (closed epic dependency is not a live inversion)", report.RankInversions)
	}
}

func TestFixRankInversionsIgnoresDeletedIssues(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependent", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Blocker", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(blocker) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: dependent.ID, DstID: blocker.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(dependent blocks blocker) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID:   blocker.ID,
		Action:    "delete",
		Reason:    "removed",
		CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("TransitionIssue(delete blocker) error = %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.RankInversions != 0 {
		t.Fatalf("Doctor().RankInversions = %d, want 0 for deleted issues", report.RankInversions)
	}
	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 0 {
		t.Fatalf("FixRankInversions() fixed = %d, want 0 for deleted issues", fixed)
	}
}

func TestStoreRejectsInvalidIssueType(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Bad", Topic: "bad", IssueType: "weird", Priority: 2}); err == nil {
		t.Fatal("expected invalid issue type error")
	}
}

func TestStoreCreateIssueRequiresTopic(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Bad", IssueType: "task", Priority: 2}); err == nil {
		t.Fatal("expected missing topic error")
	} else if !strings.Contains(err.Error(), "topic is required") {
		t.Fatalf("CreateIssue() error = %v, want missing topic validation", err)
	}
}

func TestStoreCreateIssueUsesBeadsCompatibleIDFormat(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:       "Renderer cleanup",
		Description: "Normalize issue IDs with beads.",
		Topic:       "renderer",
		IssueType:   "task",
		Priority:    1,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	re := regexp.MustCompile(`^test-renderer-[0-9a-z]{3,8}$`)
	if !re.MatchString(issue.ID) {
		t.Fatalf("issue.ID = %q, want test-renderer-<3-8 base36 chars>", issue.ID)
	}
	if issue.Topic != "renderer" {
		t.Fatalf("issue.Topic = %q, want renderer", issue.Topic)
	}
}

func TestStorePromptRoundTripCreateUpdateAndSearch(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	created, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:       "Render the cube",
		Description: "Standard scene fixture.",
		Prompt:      "Run the renderer at 1024x768 and assert no NaNs in the depth buffer.",
		Topic:       "renderer",
		IssueType:   "task",
		Priority:    2,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if created.Prompt == "" {
		t.Fatalf("created.Prompt is empty; want preserved through CreateIssue")
	}

	got, err := st.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Prompt != created.Prompt {
		t.Fatalf("GetIssue prompt = %q, want %q", got.Prompt, created.Prompt)
	}

	newPrompt := "Re-run with --headless and capture screenshot to /tmp/out.png"
	updated, err := st.UpdateIssue(ctx, created.ID, UpdateIssueInput{Prompt: &newPrompt})
	if err != nil {
		t.Fatalf("UpdateIssue() error = %v", err)
	}
	if updated.Prompt != newPrompt {
		t.Fatalf("UpdateIssue prompt = %q, want %q", updated.Prompt, newPrompt)
	}

	matches, err := st.ListIssues(ctx, ListIssuesFilter{SearchTerms: []string{"headless"}})
	if err != nil {
		t.Fatalf("ListIssues(search) error = %v", err)
	}
	found := false
	for _, m := range matches {
		if m.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("search for prompt-only term did not return %q", created.ID)
	}

	cleared := ""
	cleared2, err := st.UpdateIssue(ctx, created.ID, UpdateIssueInput{Prompt: &cleared})
	if err != nil {
		t.Fatalf("UpdateIssue(clear prompt) error = %v", err)
	}
	if cleared2.Prompt != "" {
		t.Fatalf("UpdateIssue(clear) prompt = %q, want empty", cleared2.Prompt)
	}
}

func TestGenerateHashIssueIDIsDeterministicForSameInputs(t *testing.T) {
	createdAt := time.Date(2026, 3, 19, 12, 0, 0, 0, time.UTC)

	first := issueid.GenerateHashID("test", "parser", "Fix parser", "Adopt beads ID shape", "links", createdAt, 6, 0)
	second := issueid.GenerateHashID("test", "parser", "Fix parser", "Adopt beads ID shape", "links", createdAt, 6, 0)

	if first != second {
		t.Fatalf("issueid.GenerateHashID() = %q then %q, want deterministic output", first, second)
	}
}

func TestEnsureIssuePrefixNormalizesAndClampsConfiguredValue(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	if err := st.EnsureIssuePrefix(ctx, "Renderer Platform Team"); err != nil {
		t.Fatalf("EnsureIssuePrefix() error = %v", err)
	}
	got, err := st.getMeta(ctx, nil, "issue_prefix")
	if err != nil {
		t.Fatalf("getMeta(issue_prefix) error = %v", err)
	}
	if got != "renderer-pla" {
		t.Fatalf("issue_prefix = %q, want renderer-pla", got)
	}
}

func TestNewIssueIDCollisionsAdvanceNonce(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	createdAt := time.Date(2026, 3, 19, 12, 0, 0, 0, time.UTC)
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer tx.Rollback()

	firstID, err := newIssueID(ctx, tx, "test", "parser", "Duplicate title", "Duplicate description", "links", createdAt, "")
	if err != nil {
		t.Fatalf("newIssueID(first) error = %v", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO issues(
		id, title, description, status, priority, issue_type, topic, assignee, created_at, updated_at, closed_at, archived_at, deleted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL)`,
		firstID, "Duplicate title", "Duplicate description", "open", 1, "task", "parser", "", createdAt.Format(time.RFC3339Nano), createdAt.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert first issue error = %v", err)
	}

	secondID, err := newIssueID(ctx, tx, "test", "parser", "Duplicate title", "Duplicate description", "links", createdAt, "")
	if err != nil {
		t.Fatalf("newIssueID(second) error = %v", err)
	}
	if secondID == firstID {
		t.Fatalf("secondID = %q, want collision fallback to choose a different ID than %q", secondID, firstID)
	}
}

func TestCreateIssueChildIDsIncrementFromParent(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	parent, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:     "Renderer cleanup",
		Topic:     "renderer",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	childOne, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:     "Fix first race",
		Topic:     "renderer",
		ParentID:  parent.ID,
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue(childOne) error = %v", err)
	}
	childTwo, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:     "Fix second race",
		Topic:     "renderer",
		ParentID:  parent.ID,
		IssueType: "task",
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("CreateIssue(childTwo) error = %v", err)
	}

	if childOne.ID != parent.ID+".1" {
		t.Fatalf("childOne.ID = %q, want %q", childOne.ID, parent.ID+".1")
	}
	if childTwo.ID != parent.ID+".2" {
		t.Fatalf("childTwo.ID = %q, want %q", childTwo.ID, parent.ID+".2")
	}
	detail, err := st.GetIssueDetail(ctx, childTwo.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(childTwo) error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != parent.ID {
		t.Fatalf("detail.Parent = %#v, want %q", detail.Parent, parent.ID)
	}
}

func TestStoreListIssuesSupportsAdvancedFilters(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issueA, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:       "Renderer contract cleanup",
		Description: "Fix the renderer contract for draw prep.",
		Topic:       "renderer",
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
		Topic:       "fluid",
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
		Statuses:      []string{"open"},
		IssueTypes:    []string{"task"},
		Assignees:     []string{"bmf"},
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

func TestStoreListChildrenDefaultsToRankOrder(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	parent, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Parent", Topic: "tree", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	childA, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Child A", Topic: "tree", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(childA) error = %v", err)
	}
	childB, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Child B", Topic: "tree", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(childB) error = %v", err)
	}
	if _, err := st.SetParent(ctx, SetParentInput{ChildID: childA.ID, ParentID: parent.ID, CreatedBy: "tester"}); err != nil {
		t.Fatalf("SetParent(childA) error = %v", err)
	}
	if _, err := st.SetParent(ctx, SetParentInput{ChildID: childB.ID, ParentID: parent.ID, CreatedBy: "tester"}); err != nil {
		t.Fatalf("SetParent(childB) error = %v", err)
	}

	children, err := st.ListChildren(ctx, parent.ID)
	if err != nil {
		t.Fatalf("ListChildren() error = %v", err)
	}
	if got, want := issueIDs(children), []string{childA.ID, childB.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ListChildren() ids = %#v, want %#v", got, want)
	}
}

func TestStoreGetIssueDetailDefaultsRelatedIssueGroupsToRankOrder(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	main, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Main", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(main) error = %v", err)
	}
	depA, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependency A", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(depA) error = %v", err)
	}
	depB, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependency B", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(depB) error = %v", err)
	}
	blockedA, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Blocked A", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(blockedA) error = %v", err)
	}
	blockedB, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Blocked B", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(blockedB) error = %v", err)
	}
	childA, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Child A", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(childA) error = %v", err)
	}
	childB, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Child B", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(childB) error = %v", err)
	}
	relatedA, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Related A", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(relatedA) error = %v", err)
	}
	relatedB, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Related B", Topic: "order", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(relatedB) error = %v", err)
	}

	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: main.ID, DstID: depB.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(main->depB) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: main.ID, DstID: depA.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(main->depA) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: blockedB.ID, DstID: main.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(blockedB->main) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: blockedA.ID, DstID: main.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(blockedA->main) error = %v", err)
	}
	if _, err := st.SetParent(ctx, SetParentInput{ChildID: childB.ID, ParentID: main.ID, CreatedBy: "tester"}); err != nil {
		t.Fatalf("SetParent(childB) error = %v", err)
	}
	if _, err := st.SetParent(ctx, SetParentInput{ChildID: childA.ID, ParentID: main.ID, CreatedBy: "tester"}); err != nil {
		t.Fatalf("SetParent(childA) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: main.ID, DstID: relatedB.ID, Type: "related-to", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(main<->relatedB) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: main.ID, DstID: relatedA.ID, Type: "related-to", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(main<->relatedA) error = %v", err)
	}

	detail, err := st.GetIssueDetail(ctx, main.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if got, want := issueIDs(detail.DependsOn), []string{depA.ID, depB.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DependsOn ids = %#v, want %#v", got, want)
	}
	if got, want := issueIDs(detail.Blocks), []string{blockedA.ID, blockedB.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Blocks ids = %#v, want %#v", got, want)
	}
	if got, want := issueIDs(detail.Children), []string{childA.ID, childB.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Children ids = %#v, want %#v", got, want)
	}
	if got, want := issueIDs(detail.Related), []string{relatedA.ID, relatedB.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Related ids = %#v, want %#v", got, want)
	}
}

func TestStoreLabelsAreWritableFirstClassData(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:     "Renderer cleanup",
		Topic:     "renderer",
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

func issueWithStatus(t *testing.T, issue model.Issue, status model.State) model.Issue {
	t.Helper()
	hydrated, err := model.HydrateOwnedStatus(issue, model.StatusView{Value: status})
	if err != nil {
		t.Fatalf("HydrateOwnedStatus() error = %v", err)
	}
	return hydrated
}

func TestReplaceFromExportAndSyncState(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Renderer cleanup", Topic: "renderer", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	export := model.Export{
		Version:     1,
		WorkspaceID: "foreign-workspace",
		ExportedAt:  time.Now().UTC(),
		Issues: []model.Issue{issueWithStatus(t, model.Issue{
			ID:          "issue-replaced",
			Title:       "Imported issue",
			Description: "from file sync",
			Priority:    2,
			IssueType:   "task",
			Labels:      []string{"imported"},
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}, model.StateOpen)},
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
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Renderer cleanup", Topic: "renderer", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	closed, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "close", Reason: "done", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}
	if closed.State() != model.StateClosed || closed.ClosedAtValue() == nil {
		t.Fatalf("closed = %#v", closed)
	}
	reopened, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "reopen", Reason: "follow-up work", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(reopen) error = %v", err)
	}
	if reopened.State() != model.StateOpen || reopened.ClosedAtValue() != nil {
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

func TestTransitionIssueAllowsEmptyReason(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "No reason needed", Topic: "triage", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	closed, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "close", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(close, empty reason) error = %v", err)
	}
	if closed.State() != model.StateClosed {
		t.Fatalf("closed.State() = %q, want closed", closed.State())
	}
	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.History) != 2 {
		t.Fatalf("history = %#v", detail.History)
	}
	if detail.History[1].Action != "close" || detail.History[1].Reason != "" {
		t.Fatalf("history[1] = %#v (want action=close reason=\"\")", detail.History[1])
	}
}

func TestIssueStatusClaimAndDoneAreDeterministic(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Claim me", Topic: "claims", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if issue.State() != model.StateOpen {
		t.Fatalf("issue.State() = %q, want open", issue.State())
	}

	started, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "start",
		Reason:    "claim",
		CreatedBy: "agent-a",
	})
	if err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Fatalf("started.State() = %q, want in_progress", started.State())
	}

	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "start",
		Reason:    "competing claim",
		CreatedBy: "agent-b",
	}); err == nil {
		t.Fatal("expected claim conflict when claiming an already in_progress issue")
	}

	done, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "done",
		Reason:    "implemented",
		CreatedBy: "agent-a",
	})
	if err != nil {
		t.Fatalf("TransitionIssue(done) error = %v", err)
	}
	if done.State() != model.StateClosed || done.ClosedAtValue() == nil {
		t.Fatalf("done = %#v, want closed with ClosedAt", done)
	}

	openIssues, err := st.ListIssues(ctx, ListIssuesFilter{Statuses: []string{"open"}})
	if err != nil {
		t.Fatalf("ListIssues(open) error = %v", err)
	}
	if len(openIssues) != 0 {
		t.Fatalf("openIssues = %#v, want empty", openIssues)
	}

	closedIssues, err := st.ListIssues(ctx, ListIssuesFilter{Statuses: []string{"closed"}})
	if err != nil {
		t.Fatalf("ListIssues(closed) error = %v", err)
	}
	if len(closedIssues) != 1 || closedIssues[0].ID != issue.ID {
		t.Fatalf("closedIssues = %#v", closedIssues)
	}
}

func TestOpenDoesNotCreateStartupCommitWhenSchemaIsCurrent(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	beforeLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log before reopen error = %v", err)
	}

	st, err = Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() reopen error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() reopen error = %v", err)
	}

	afterLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log after reopen error = %v", err)
	}

	if countNonEmptyLines(afterLog) != countNonEmptyLines(beforeLog) {
		t.Fatalf("startup reopen created extra commit:\nbefore:\n%s\nafter:\n%s", beforeLog, afterLog)
	}
}

func TestOpenPreservesExistingSchemaVersionMeta(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.setMeta(ctx, nil, "schema_version", "2"); err != nil {
		t.Fatalf("setMeta(schema_version) error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "set schema version"); err != nil {
		t.Fatalf("commitWorkingSet() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	st, err = Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() reopen error = %v", err)
	}
	defer st.Close()

	got, err := st.getMeta(ctx, nil, "schema_version")
	if err != nil {
		t.Fatalf("getMeta(schema_version) error = %v", err)
	}
	if got != "2" {
		t.Fatalf("schema_version = %q, want 2", got)
	}
}

func TestOpenForReadDoesNotCreateStartupCommitWhenSchemaIsCurrent(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	beforeLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log before read-open error = %v", err)
	}

	readStore, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	if _, err := readStore.ListIssues(ctx, ListIssuesFilter{}); err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if err := readStore.Close(); err != nil {
		t.Fatalf("Close() read error = %v", err)
	}

	afterLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log after read-open error = %v", err)
	}

	if countNonEmptyLines(afterLog) != countNonEmptyLines(beforeLog) {
		t.Fatalf("read-open created extra commit:\nbefore:\n%s\nafter:\n%s", beforeLog, afterLog)
	}
}

func TestOpenForReadDoesNotCreateDatabaseWhenMissing(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	readStore, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		_ = readStore.Close()
		t.Fatal("OpenForRead() error = nil, want missing database failure")
	}
	if _, err := os.Stat(filepath.Join(doltRoot, "links")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repo path stat error = %v, want not exist", err)
	}
}

func TestOpenForReadAutoMigratesExistingSchema(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Create the database without running migration.
	if _, err := EnsureDatabase(ctx, doltRoot, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}
	seed, err := openStoreConnection(doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("openStoreConnection() error = %v", err)
	}
	// Create a bare issues table missing the topic column to simulate a stale schema.
	_, err = seed.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS issues (
		id VARCHAR(191) PRIMARY KEY,
		title TEXT NOT NULL,
		description TEXT NOT NULL,
		status VARCHAR(32) NOT NULL,
		priority INT NOT NULL,
		issue_type VARCHAR(32) NOT NULL,
		assignee TEXT NOT NULL,
		created_at VARCHAR(64) NOT NULL,
		updated_at VARCHAR(64) NOT NULL,
		closed_at VARCHAR(64) NULL,
		archived_at VARCHAR(64) NULL,
		deleted_at VARCHAR(64) NULL
	)`)
	if err != nil {
		t.Fatalf("create stale schema error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close() error = %v", err)
	}

	// OpenForRead should auto-migrate and add the missing topic column.
	readStore, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer readStore.Close()

	// Verify migration ran by checking the topic column exists.
	var topicExists int
	err = readStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'topic'`).Scan(&topicExists)
	if err != nil {
		t.Fatalf("check topic column error = %v", err)
	}
	if topicExists == 0 {
		t.Fatal("OpenForRead did not auto-migrate: topic column missing")
	}
}

func TestListChildrenReturnsEpicChildrenWithDerivedLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	root, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Root epic", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(root) error = %v", err)
	}
	sub, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Sub epic", Topic: "life", IssueType: "epic", Priority: 1, ParentID: root.ID})
	if err != nil {
		t.Fatalf("CreateIssue(sub) error = %v", err)
	}
	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Leaf", Topic: "life", IssueType: "task", Priority: 2, ParentID: sub.ID})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: leaf.ID, Action: "close", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}

	children, err := st.ListChildren(ctx, root.ID)
	if err != nil {
		t.Fatalf("ListChildren() error = %v", err)
	}
	if len(children) != 1 || children[0].ID != sub.ID {
		t.Fatalf("children = %#v, want sub epic %s", children, sub.ID)
	}
	progress := children[0].Progress()
	if !children[0].IsContainer() || progress.Closed != 1 || progress.Total != 1 {
		t.Fatalf("sub epic container/progress = %v/%#v, want closed 1 total 1", children[0].IsContainer(), progress)
	}
}

func TestGetIssueDetailRelationSidesAreHydrated(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	parent, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Parent epic", Topic: "detail", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	subject, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Subject", Topic: "detail", IssueType: "task", Priority: 2, ParentID: parent.ID})
	if err != nil {
		t.Fatalf("CreateIssue(subject) error = %v", err)
	}
	dependency, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependency epic", Topic: "detail", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(dependency) error = %v", err)
	}
	dependencyLeaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Dependency leaf", Topic: "detail", IssueType: "task", Priority: 2, ParentID: dependency.ID})
	if err != nil {
		t.Fatalf("CreateIssue(dependency leaf) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: dependencyLeaf.ID, Action: "close", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(close dependency leaf) error = %v", err)
	}
	related, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Related epic", Topic: "detail", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(related) error = %v", err)
	}
	blocked, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Blocked by subject", Topic: "detail", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(blocked) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: subject.ID, DstID: dependency.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(depends on epic) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: subject.ID, DstID: related.ID, Type: "related-to", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(related epic) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: blocked.ID, DstID: subject.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(blocked) error = %v", err)
	}

	detail, err := st.GetIssueDetail(ctx, subject.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.Parent == nil || !detail.Parent.IsContainer() {
		t.Fatalf("parent = %#v, want hydrated container", detail.Parent)
	}
	if len(detail.DependsOn) != 1 || !detail.DependsOn[0].IsContainer() || detail.DependsOn[0].State() != model.StateClosed {
		t.Fatalf("DependsOn = %#v, want closed hydrated epic", detail.DependsOn)
	}
	if len(detail.Related) != 1 || !detail.Related[0].IsContainer() {
		t.Fatalf("Related = %#v, want hydrated epic", detail.Related)
	}
	if len(detail.Blocks) != 1 || detail.Blocks[0].Capabilities().Status == nil {
		t.Fatalf("Blocks = %#v, want hydrated leaf", detail.Blocks)
	}
	if len(detail.DependsOn) > 0 && detail.DependsOn[0].Labels == nil {
		t.Fatalf("DependsOn[0].Labels = nil, want hydrated label slice")
	}
	if detail.Parent != nil && detail.Parent.Labels == nil {
		t.Fatalf("Parent.Labels = nil, want hydrated label slice")
	}
}

func TestEpicAsDependencyDerivedState(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Leaf", Topic: "dep", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Epic dep", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Epic child", Topic: "dep", IssueType: "task", Priority: 2, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "close", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: leaf.ID, DstID: epic.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(blocks) error = %v", err)
	}
	detail, err := st.GetIssueDetail(ctx, leaf.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.DependsOn) != 1 || detail.DependsOn[0].State() != model.StateClosed {
		t.Fatalf("DependsOn = %#v, want epic dependency with closed derived state", detail.DependsOn)
	}
}

// (links-agent-epic-model-uew.7) After the schema cleanup, container rows
// persist NULL in the status column instead of the invented "open". The
// dead-data write is gone; any future code that reads i.status on an epic
// will get NULL and fail loudly instead of silently lying.
func TestCreateEpicPersistsNullStatusColumn(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Container", Topic: "schema", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Leaf", Topic: "schema", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	var epicStatus, leafStatus sql.NullString
	if err := st.db.QueryRowContext(ctx, "SELECT status FROM issues WHERE id = ?", epic.ID).Scan(&epicStatus); err != nil {
		t.Fatalf("query epic status error = %v", err)
	}
	if err := st.db.QueryRowContext(ctx, "SELECT status FROM issues WHERE id = ?", leaf.ID).Scan(&leafStatus); err != nil {
		t.Fatalf("query leaf status error = %v", err)
	}
	if epicStatus.Valid {
		t.Fatalf("epic.status = %q (Valid=true), want NULL", epicStatus.String)
	}
	if !leafStatus.Valid || leafStatus.String != string(model.StateOpen) {
		t.Fatalf("leaf.status = %#v, want Valid open", leafStatus)
	}
}

// (links-agent-epic-model-uew.7) Container ↔ non-container IssueType changes
// would orphan the lifecycle expression: an epic carries an AllOf lifecycle
// that derives state from children, and a leaf carries an OwnedStatus carrying
// status/assignee/closed_at. Crossing that boundary via UpdateIssue would
// either drop the leaf's status or leave AllOf attached to a row whose schema
// requires owned status. Refused at the trust boundary instead of patched up
// downstream with an invented default.
func TestUpdateIssueRefusesContainerLeafTypeChange(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Container", Topic: "schema", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Leaf", Topic: "schema", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	taskType := "task"
	if _, err := st.UpdateIssue(ctx, epic.ID, UpdateIssueInput{IssueType: &taskType}); err == nil {
		t.Fatal("UpdateIssue(epic -> task) succeeded; container ↔ leaf type changes must be refused")
	}
	epicType := "epic"
	if _, err := st.UpdateIssue(ctx, leaf.ID, UpdateIssueInput{IssueType: &epicType}); err == nil {
		t.Fatal("UpdateIssue(task -> epic) succeeded; container ↔ leaf type changes must be refused")
	}
	bugType := "bug"
	if _, err := st.UpdateIssue(ctx, leaf.ID, UpdateIssueInput{IssueType: &bugType}); err != nil {
		t.Fatalf("UpdateIssue(task -> bug) error = %v; same-kind type changes must remain legal", err)
	}
}

// (links-agent-epic-model-uew.7) ensureStatusConstraint compares Dolt's
// reported CHECK clause against canonicalStatusCheckClause. If Dolt's
// normalization ever drifts from ours, the comparison would silently fail and
// every Open() would drop+re-add the constraint, producing a fresh schema
// commit each time. This test pins migration idempotence at the observable
// boundary — the Dolt commit log — so any future drift is loud.
func TestMigrationIsIdempotentOnSecondOpen(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	commitsBefore, err := doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log before reopen error = %v", err)
	}
	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}
	commitsAfter, err := doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log after reopen error = %v", err)
	}
	if countNonEmptyLines(commitsAfter) != countNonEmptyLines(commitsBefore) {
		t.Fatalf("migration produced extra commit on second Open():\nbefore:\n%s\nafter:\n%s", commitsBefore, commitsAfter)
	}
}

// (links-agent-epic-model-uew.7) The CHECK constraint encodes the invariant
// at the schema level: any attempt to write a non-NULL status on an epic row
// is rejected at INSERT time, mechanically — no future code path can quietly
// re-introduce the dead-data lie.
func TestSchemaRejectsEpicWithNonNullStatus(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := st.ExecRawForTest(ctx,
		`INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-illegal-epic", "Illegal", "", "open", 1, "epic", "schema", "", "ZZZ", now, now,
	)
	if err == nil {
		t.Fatal("INSERT epic with status='open' succeeded; CHECK constraint should reject it")
	}
}

// The other half of the CHECK invariant: leaf rows must carry a non-NULL
// status. `status IN (...)` against NULL evaluates to NULL, and MySQL/Dolt
// CHECK treats NULL as not-violated — so without an explicit `status IS NOT
// NULL` term in the canonical clause, a leaf row with NULL status would slip
// through. This test pins the schema-level rejection so any future drift in
// Dolt's CHECK-NULL semantics or in the clause itself fails loudly.
func TestSchemaRejectsLeafWithNullStatus(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := st.ExecRawForTest(ctx,
		`INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-illegal-leaf", "Illegal", "", nil, 1, "task", "schema", "", "ZZZ", now, now,
	)
	if err == nil {
		t.Fatal("INSERT task with status=NULL succeeded; CHECK constraint should reject it")
	}
}

func TestSyncRoundTripIncludingEpic(t *testing.T) {
	ctx := context.Background()
	source := openIssueStore(t, ctx)
	epic, err := source.CreateIssue(ctx, CreateIssueInput{Title: "Sync epic", Topic: "sync", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := source.CreateIssue(ctx, CreateIssueInput{Title: "Sync leaf", Topic: "sync", IssueType: "task", Priority: 2, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	if _, err := source.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "close", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}
	before, err := source.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue(before) error = %v", err)
	}
	export, err := source.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	data, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("Marshal(export) error = %v", err)
	}
	var decoded model.Export
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal(export) error = %v", err)
	}
	target := openIssueStore(t, ctx)
	if err := target.ReplaceFromExport(ctx, decoded); err != nil {
		t.Fatalf("ReplaceFromExport() error = %v", err)
	}
	after, err := target.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue(after) error = %v", err)
	}
	if after.Progress() != before.Progress() {
		t.Fatalf("round-trip progress = %#v, want %#v", after.Progress(), before.Progress())
	}
}

func TestCloseLeafUsesOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Close me", Topic: "life", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := st.writeStatusTransition(ctx, issue, "tester", "", "close"); err != nil {
		t.Fatalf("writeStatusTransition(first) error = %v", err)
	}
	_, err = st.writeStatusTransition(ctx, issue, "tester", "", "close")
	if err == nil || err.Error() != `close conflict: issue status is "closed"` {
		t.Fatalf("writeStatusTransition(second) error = %v, want close conflict", err)
	}
}

func TestArchiveReturnsHydratedIssue(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Archive epic", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Child", Topic: "life", IssueType: "task", Priority: 2, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	archived, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: epic.ID, Action: "archive", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(archive) error = %v", err)
	}
	progress := archived.Progress()
	if archived.ArchivedAt == nil || !archived.IsContainer() || progress.Total != 1 {
		t.Fatalf("archived issue = archived_at:%v container:%v progress:%#v, want hydrated archived epic", archived.ArchivedAt, archived.IsContainer(), progress)
	}
}

func TestArchivedEpicProgressIncludesArchivedChildren(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Archived snapshot", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Archived child", Topic: "life", IssueType: "task", Priority: 2, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "archive", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(archive child) error = %v", err)
	}
	activeEpic, err := st.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue(active epic) error = %v", err)
	}
	if progress := activeEpic.Progress(); progress.Total != 0 {
		t.Fatalf("active epic Progress() = %#v, want archived child excluded", progress)
	}
	archivedEpic, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: epic.ID, Action: "archive", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(archive epic) error = %v", err)
	}
	if progress := archivedEpic.Progress(); progress.Total != 1 || progress.Open != 1 {
		t.Fatalf("archived epic Progress() = %#v, want archived child snapshot included", progress)
	}
}

func TestReopenClearsClosedAt(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Reopen me", Topic: "life", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	closed, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "close", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}
	if closed.ClosedAtValue() == nil {
		t.Fatalf("ClosedAtValue() = nil after close")
	}
	reopened, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: issue.ID, Action: "reopen", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(reopen) error = %v", err)
	}
	if reopened.StatusValue() != string(model.StateOpen) || reopened.ClosedAtValue() != nil {
		t.Fatalf("reopened status/closed_at = %q/%#v, want open/nil", reopened.StatusValue(), reopened.ClosedAtValue())
	}
	loaded, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if loaded.ClosedAtValue() != nil {
		t.Fatalf("loaded ClosedAtValue() = %#v, want nil", loaded.ClosedAtValue())
	}
}

func TestExportRefusesUnhydratedIssue(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	hydrated, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Hydrated", Topic: "life", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if len(export.Issues) == 0 || export.Issues[0].ID != hydrated.ID {
		t.Fatalf("Export() returned %#v, want hydrated issue", export.Issues)
	}
	export.Issues = append(export.Issues, model.Issue{ID: "unhydrated-x", IssueType: "task"})
	if _, err := json.MarshalIndent(export, "", "  "); err == nil {
		t.Fatalf("MarshalIndent of export with unhydrated issue did not error")
	}
}

func TestArchiveSecondCallErrorsAlreadyArchived(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Archive me", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	archived, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: epic.ID, Action: "archive", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(archive) error = %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Fatalf("archived issue has nil ArchivedAt")
	}
	_, err = st.TransitionIssue(ctx, TransitionIssueInput{IssueID: epic.ID, Action: "archive", CreatedBy: "tester"})
	if err == nil || err.Error() != "issue is already archived" {
		t.Fatalf("re-archive error = %v, want already archived", err)
	}
}

func TestRankSetEstablishesAbsoluteTopOrder(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	a, err := st.CreateIssue(ctx, CreateIssueInput{Title: "A", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	b, err := st.CreateIssue(ctx, CreateIssueInput{Title: "B", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}
	c, err := st.CreateIssue(ctx, CreateIssueInput{Title: "C", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(C) error = %v", err)
	}

	// Apply absolute order [C, A, B] at the top, regardless of creation order.
	if err := st.RankSet(ctx, []string{c.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("RankSet() error = %v", err)
	}
	all, err := st.ListIssues(ctx, ListIssuesFilter{Limit: 0})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	got := issueIDs(all)
	want := []string{c.ID, a.ID, b.ID}
	if len(got) < 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ListIssues() top 3 ids = %#v, want prefix %#v", got, want)
	}
}

func TestRankSetRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	a, err := st.CreateIssue(ctx, CreateIssueInput{Title: "A", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	if err := st.RankSet(ctx, []string{a.ID, a.ID}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("RankSet(duplicates) error = %v, want duplicate-id error", err)
	}
}

func TestRankSetRejectsTooFewIDs(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	a, err := st.CreateIssue(ctx, CreateIssueInput{Title: "A", Topic: "rank", IssueType: "task", Priority: 2})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	if err := st.RankSet(ctx, []string{a.ID}); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("RankSet(single id) error = %v, want too-few error", err)
	}
}

func countNonEmptyLines(input string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(input), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
