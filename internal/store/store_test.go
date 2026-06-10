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

	"github.com/promptctl/links-issue-tracker/internal/doltcli"
	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

func openIssueStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Renderer cleanup", Topic: "renderer", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue epic error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Move pass validation", Topic: "renderer", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue child error = %v", err)
	}
	related, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Guard shared buffers", Topic: "renderer", IssueType: "feature", Priority: 0})
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Container", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	openLeaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Open leaf", Topic: "life", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(open leaf) error = %v", err)
	}
	closedLeaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Closed leaf", Topic: "life", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(closed leaf) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedLeaf.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
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

// The one real-invariant rejection in the target-state model: transitioning a
// container whose children are not all done. The assertion is on the typed
// category (ContainerActionError + live unfinished count), not the prose.
// [LAW:behavior-not-structure]
func TestTransitionEpicRejectsWithUnfinishedChildCount(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Container", Topic: "reject", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	children := make([]model.Issue, 2)
	for i := range children {
		child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child", Topic: "reject", IssueType: "task", Priority: 0, ParentID: epic.ID})
		if err != nil {
			t.Fatalf("CreateIssue(child %d) error = %v", i, err)
		}
		children[i] = child
	}

	for _, action := range []string{"close", "done", "start", "reopen"} {
		_, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: epic.ID, Action: action, CreatedBy: "tester"})
		var containerErr model.ContainerActionError
		if !errors.As(err, &containerErr) {
			t.Fatalf("TransitionIssue(epic, %s) error = %v, want model.ContainerActionError", action, err)
		}
		if containerErr.Unfinished() != len(children) {
			t.Fatalf("TransitionIssue(epic, %s) unfinished = %d, want %d", action, containerErr.Unfinished(), len(children))
		}
		if containerErr.ID != epic.ID {
			t.Fatalf("TransitionIssue(epic, %s) rejection names %q, want %q", action, containerErr.ID, epic.ID)
		}
	}

	for _, child := range children {
		if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "done", CreatedBy: "tester"}); err != nil {
			t.Fatalf("TransitionIssue(child done) error = %v", err)
		}
	}

	// All children done: the unfinished-children rejection must stop firing.
	_, err = st.TransitionIssue(ctx, TransitionIssueInput{IssueID: epic.ID, Action: "close", CreatedBy: "tester"})
	var containerErr model.ContainerActionError
	if !errors.As(err, &containerErr) {
		t.Fatalf("TransitionIssue(epic, close) after children done error = %v, want model.ContainerActionError", err)
	}
	if containerErr.Unfinished() != 0 {
		t.Fatalf("TransitionIssue(epic, close) after children done unfinished = %d, want 0 (rejection must reflect live child state)", containerErr.Unfinished())
	}
}

// Epic state is derived from children; ListIssues filters by that derived state
// rather than the dead i.status DB column. This regression test pins the three
// epic shapes against the canonical default and explicit status filters.
func TestListIssuesStatusFilterUsesDerivedEpicState(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	openEpic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "All open", Topic: "derived", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(openEpic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Open child", Topic: "derived", IssueType: "task", Priority: 0, ParentID: openEpic.ID}); err != nil {
		t.Fatalf("CreateIssue(openEpic child) error = %v", err)
	}

	mixedEpic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Mixed children", Topic: "derived", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(mixedEpic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Mixed open", Topic: "derived", IssueType: "task", Priority: 0, ParentID: mixedEpic.ID}); err != nil {
		t.Fatalf("CreateIssue(mixedEpic open child) error = %v", err)
	}
	mixedClosedChild, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Mixed closed", Topic: "derived", IssueType: "task", Priority: 0, ParentID: mixedEpic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(mixedEpic closed child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: mixedClosedChild.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(mixed closed start) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: mixedClosedChild.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(mixed closed done) error = %v", err)
	}

	closedEpic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "All closed", Topic: "derived", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(closedEpic) error = %v", err)
	}
	closedChild, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Closed child", Topic: "derived", IssueType: "task", Priority: 0, ParentID: closedEpic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(closedEpic child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedChild.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(closed child start) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: closedChild.ID, Action: "done", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(closed child done) error = %v", err)
	}

	cases := []struct {
		name     string
		statuses []model.State
		want     map[string]bool
	}{
		{name: "default open+in_progress", statuses: []model.State{model.StateOpen, model.StateInProgress}, want: map[string]bool{openEpic.ID: true, mixedEpic.ID: true, closedEpic.ID: false}},
		{name: "open only", statuses: []model.State{model.StateOpen}, want: map[string]bool{openEpic.ID: true, mixedEpic.ID: false, closedEpic.ID: false}},
		{name: "in_progress only", statuses: []model.State{model.StateInProgress}, want: map[string]bool{openEpic.ID: false, mixedEpic.ID: true, closedEpic.ID: false}},
		{name: "closed only", statuses: []model.State{model.StateClosed}, want: map[string]bool{openEpic.ID: false, mixedEpic.ID: false, closedEpic.ID: true}},
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

	dependentA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependent A", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(dependentA) error = %v", err)
	}
	dependentB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependent B", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(dependentB) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Shared blocker", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
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

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	upstream, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Upstream blocker", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(upstream) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Middle blocker", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
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

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Release", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocking epic", Topic: "rank", IssueType: "epic", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic child", Topic: "rank", IssueType: "task", Priority: 0, ParentID: epic.ID, Placement: RankBottom}); err != nil {
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

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Closed epic", Topic: "rank", IssueType: "epic", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Closed child", Topic: "rank", IssueType: "task", Priority: 0, ParentID: epic.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic child) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: child.ID, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
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

	dependent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(dependent) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
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

// A blocks cycle has no valid rank order, so the only durable fix is to keep
// it from existing. AddRelation rejects the edge that would close the loop.
func TestAddRelationRejectsBlocksCycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	a, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "A", Topic: "cycle", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	b, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "B", Topic: "cycle", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: a.ID, DstID: b.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(A blocks B) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: b.ID, DstID: a.ID, Type: "blocks", CreatedBy: "tester"}); err == nil {
		t.Fatal("AddRelation(B blocks A) = nil, want cycle rejection")
	} else if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("AddRelation(B blocks A) error = %v, want cycle rejection", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(report.DependencyCycle) != 0 {
		t.Fatalf("Doctor().DependencyCycle = %v, want empty (cycle never persisted)", report.DependencyCycle)
	}
}

// The cycle guard must follow transitive precedence, not just the direct edge:
// A->B->C means adding C->A closes a 3-cycle.
func TestAddRelationRejectsTransitiveBlocksCycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	mk := func(title string) string {
		issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: title, Topic: "cycle", IssueType: "task", Priority: 0})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		return issue.ID
	}
	a, b, c := mk("A"), mk("B"), mk("C")
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: a, DstID: b, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(A blocks B) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: b, DstID: c, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(B blocks C) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: c, DstID: a, Type: "blocks", CreatedBy: "tester"}); err == nil {
		t.Fatal("AddRelation(C blocks A) = nil, want transitive cycle rejection")
	}
}

func TestAddRelationRejectsSelfBlock(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	a, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "A", Topic: "cycle", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: a.ID, DstID: a.ID, Type: "blocks", CreatedBy: "tester"}); err == nil {
		t.Fatal("AddRelation(A blocks A) = nil, want self-block rejection")
	}
}

// Cycles that slip past AddRelation (e.g. bulk import, which bypasses the
// interactive guard) must be diagnosable. Doctor names the cycle and
// FixRankInversions refuses with an actionable message instead of looping into
// the opaque "unable to converge" failure.
func TestDoctorAndFixDetectImportedBlocksCycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	a, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "A", Topic: "cycle", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	b, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "B", Topic: "cycle", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}
	// Insert the mutually-blocking pair directly, simulating data that entered
	// through a path that does not run the AddRelation cycle guard.
	for _, e := range [][2]string{{a.ID, b.ID}, {b.ID, a.ID}} {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'blocks', ?, 'import')`,
			e[0], e[1], time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed cyclic edge %v error = %v", e, err)
		}
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(report.DependencyCycle) == 0 {
		t.Fatal("Doctor().DependencyCycle is empty, want the cyclic members")
	}

	_, err = st.FixRankInversions(ctx)
	if err == nil {
		t.Fatal("FixRankInversions() = nil, want actionable cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("FixRankInversions() error = %v, want it to name the cycle", err)
	}
	if strings.Contains(err.Error(), "unable to converge") {
		t.Fatalf("FixRankInversions() error = %v, want actionable message, not opaque non-convergence", err)
	}
}

// A blank target status is the "no --status flag" signal. transitionFor must
// resolve it to "no transition" — the container-update bug was a normalization
// that turned blank into a real "open" target, then attempted a transition on
// every epic. This is the forcing function: should that normalization return,
// the blank cases below start reporting a transition and this test fails.
func TestTransitionForTreatsBlankTargetAsNoTransition(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t"} {
		if action, transitions := transitionFor(blank); transitions {
			t.Fatalf("transitionFor(%q) = (%q, true), want no transition", blank, action)
		}
	}
	for _, tc := range []struct {
		target string
		want   model.ActionName
	}{
		{"open", model.ActionReopen},
		{"in_progress", model.ActionStart},
		{"closed", model.ActionClose},
	} {
		action, transitions := transitionFor(tc.target)
		if !transitions || action != tc.want {
			t.Fatalf("transitionFor(%q) = (%q, %v), want (%q, true)", tc.target, action, transitions, tc.want)
		}
	}
}

// applyTransition is the validation a no-op dry-run reports through. A container
// derives its state from children and exposes no action, so any transition
// resolved against it must error — this is what turns a regression's spurious
// transition into a reported doctor failure. It must never mutate.
func TestApplyTransitionRejectsContainerAndArchived(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "dryrun", IssueType: "epic", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	hydratedEpic, err := st.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue(epic) error = %v", err)
	}
	if _, err := applyTransition(hydratedEpic, model.ActionReopen, "", ""); err == nil {
		t.Fatal("applyTransition(container, reopen) = nil, want rejection (containers expose no action)")
	}
	// validateNoopUpdate on a healthy container is the post-fix contract: a true
	// no-op resolves to no transition, so it must pass.
	if err := validateNoopUpdate(hydratedEpic); err != nil {
		t.Fatalf("validateNoopUpdate(container) = %v, want nil for a no-op", err)
	}

	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "dryrun", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	hydratedLeaf, err := st.GetIssue(ctx, leaf.ID)
	if err != nil {
		t.Fatalf("GetIssue(leaf) error = %v", err)
	}
	if _, err := applyTransition(hydratedLeaf, model.ActionStart, "agent", "claim"); err != nil {
		t.Fatalf("applyTransition(leaf, start) = %v, want acceptance", err)
	}

	// The archived/deleted guard refuses a transition regardless of the action's
	// own legality — an archived issue is frozen.
	archived, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: leaf.ID, Action: "archive", Reason: "inactive", CreatedBy: "tester"})
	if err != nil {
		t.Fatalf("TransitionIssue(archive) error = %v", err)
	}
	if _, err := applyTransition(archived, model.ActionClose, "", ""); err == nil {
		t.Fatal("applyTransition(archived, close) = nil, want refusal (archived issue is frozen)")
	}
}

// On a healthy repo every issue — container and leaf — accepts a no-op update,
// so the dry-run must report zero failures and must not mutate anything.
func TestDoctorNoopUpdateDryRunPassesHealthyRepoWithoutMutating(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "dryrun", IssueType: "epic", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child", Topic: "dryrun", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: child.ID, DstID: epic.ID, Type: "parent-child", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation error = %v", err)
	}

	var eventsBefore int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events`).Scan(&eventsBefore); err != nil {
		t.Fatalf("count events before error = %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.UpdateDryRunFailures != 0 {
		t.Fatalf("Doctor().UpdateDryRunFailures = %d, want 0 (errors: %v)", report.UpdateDryRunFailures, report.Errors)
	}
	for _, e := range report.Errors {
		if strings.Contains(e, "no-op update would fail") {
			t.Fatalf("Doctor() reported a dry-run failure on a healthy repo: %q", e)
		}
	}

	var eventsAfter int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events`).Scan(&eventsAfter); err != nil {
		t.Fatalf("count events after error = %v", err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("Doctor() dry-run mutated history: issue_events %d -> %d", eventsBefore, eventsAfter)
	}
}

func TestStoreRejectsInvalidIssueType(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Bad", Topic: "bad", IssueType: "weird", Priority: 0}); err == nil {
		t.Fatal("expected invalid issue type error")
	}
}

func TestStoreCreateIssueRequiresTopic(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Bad", IssueType: "task", Priority: 0}); err == nil {
		t.Fatal("expected missing topic error")
	} else if !strings.Contains(err.Error(), "topic is required") {
		t.Fatalf("CreateIssue() error = %v, want missing topic validation", err)
	}
}

func TestStoreCreateIssueUsesBeadsCompatibleIDFormat(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
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

	created, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
		Title:       "Render the cube",
		Description: "Standard scene fixture.",
		Prompt:      "Run the renderer at 1024x768 and assert no NaNs in the depth buffer.",
		Topic:       "renderer",
		IssueType:   "task",
		Priority:    0,
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

func TestCreateIssueNormalizesAndClampsConfiguredPrefix(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Title:     "renderer cleanup",
		Topic:     "renderer",
		IssueType: "task",
		Priority:  0,
		Prefix:    "Renderer Platform Team",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if !strings.HasPrefix(issue.ID, "renderer-pla-") {
		t.Fatalf("issue.ID = %q, want it to start with %q", issue.ID, "renderer-pla-")
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

	parent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
		Title:     "Renderer cleanup",
		Topic:     "renderer",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	childOne, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
		Title:     "Fix first race",
		Topic:     "renderer",
		ParentID:  parent.ID,
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(childOne) error = %v", err)
	}
	childTwo, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
		Title:     "Fix second race",
		Topic:     "renderer",
		ParentID:  parent.ID,
		IssueType: "task",
		Priority:  0,
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

	issueA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
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
	issueB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
		Title:       "Fluid defaults",
		Description: "Tune the fluid presets.",
		Topic:       "fluid",
		IssueType:   "feature",
		Priority:    0,
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
		Statuses:      []model.State{model.StateOpen},
		IssueTypes:    []string{"task"},
		Assignees:     []string{"bmf"},
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

func TestCreateIssuePlacement(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	// Default placement (zero value) surfaces fresh work at the top.
	first, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "First", Topic: "place", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(first) error = %v", err)
	}
	second, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Second", Topic: "place", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(second) error = %v", err)
	}
	// Explicit bottom placement appends after everything.
	appended, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Appended", Topic: "place", IssueType: "task", Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(appended) error = %v", err)
	}

	issues, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	got := issueIDs(issues)
	want := []string{second.ID, first.ID, appended.ID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rank order = %#v, want %#v (newest-first default, --bottom appended last)", got, want)
	}
}

func TestStoreListChildrenDefaultsToRankOrder(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	parent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Parent", Topic: "tree", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	childA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child A", Topic: "tree", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(childA) error = %v", err)
	}
	childB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child B", Topic: "tree", IssueType: "task", Priority: 0, Placement: RankBottom})
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

	main, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Main", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(main) error = %v", err)
	}
	depA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependency A", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(depA) error = %v", err)
	}
	depB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependency B", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(depB) error = %v", err)
	}
	blockedA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocked A", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(blockedA) error = %v", err)
	}
	blockedB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocked B", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(blockedB) error = %v", err)
	}
	childA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child A", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(childA) error = %v", err)
	}
	childB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child B", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(childB) error = %v", err)
	}
	relatedA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Related A", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(relatedA) error = %v", err)
	}
	relatedB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Related B", Topic: "order", IssueType: "task", Priority: 1, Placement: RankBottom})
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

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test",
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

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Renderer cleanup", Topic: "renderer", IssueType: "task", Priority: 1})
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
			Priority:    0,
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
		Events: []model.IssueEvent{{
			ID:        "evt-1",
			IssueID:   "issue-replaced",
			Action:    "created",
			Reason:    "imported from sync",
			Actor:     "sync",
			CreatedAt: time.Now().UTC(),
			Changes: []model.FieldChange{
				{Field: "status", From: "", To: "open"},
			},
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

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Renderer cleanup", Topic: "renderer", IssueType: "task", Priority: 1})
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
	if len(detail.Events) != 4 {
		t.Fatalf("events = %#v", detail.Events)
	}
	if detail.Events[1].Action != "close" || detail.Events[1].Reason != "done" {
		t.Fatalf("events[1] = %#v", detail.Events[1])
	}
	if detail.Events[2].Action != "reopen" || detail.Events[2].Reason != "follow-up work" {
		t.Fatalf("events[2] = %#v", detail.Events[2])
	}
	if detail.Events[3].Action != "archive" || detail.Events[3].Reason != "inactive" {
		t.Fatalf("events[3] = %#v", detail.Events[3])
	}
	// archive event records archived_at flip but NOT a fake status row.
	archiveChanges := detail.Events[3].Changes
	if len(archiveChanges) != 1 || archiveChanges[0].Field != "archived_at" {
		t.Fatalf("archive event changes = %#v; want one archived_at row, no status row", archiveChanges)
	}
}

func TestTransitionIssueAllowsEmptyReason(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "No reason needed", Topic: "triage", IssueType: "task", Priority: 1})
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
	if len(detail.Events) != 2 {
		t.Fatalf("events = %#v", detail.Events)
	}
	if detail.Events[1].Action != "close" || detail.Events[1].Reason != "" {
		t.Fatalf("events[1] = %#v (want action=close reason=\"\")", detail.Events[1])
	}
}

// TestApplyUpdateEveryTargetStateRecordsOneEvent exercises the 3x3-minus-diagonal
// of the target-state lifecycle: each of the six non-identity (from -> to) pairs
// must be reachable by a single ApplyUpdate call that records exactly one event
// with the canonical action for the target state. [LAW:behavior-not-structure]
// asserts the contract (one transition per --status change, canonical action),
// not the implementation (no compound action chains, no dispatch table).
func TestApplyUpdateEveryTargetStateRecordsOneEvent(t *testing.T) {
	cases := []struct {
		from       model.State
		to         model.State
		wantAction string
	}{
		{from: model.StateOpen, to: model.StateInProgress, wantAction: "start"},
		{from: model.StateOpen, to: model.StateClosed, wantAction: "close"},
		{from: model.StateInProgress, to: model.StateOpen, wantAction: "reopen"},
		{from: model.StateInProgress, to: model.StateClosed, wantAction: "close"},
		{from: model.StateClosed, to: model.StateOpen, wantAction: "reopen"},
		{from: model.StateClosed, to: model.StateInProgress, wantAction: "start"},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"_to_"+string(tc.to), func(t *testing.T) {
			ctx := context.Background()
			st := openIssueStore(t, ctx)
			issue, err := st.CreateIssue(ctx, CreateIssueInput{
				Prefix: "test", Title: "transition", Topic: "lifecycle", IssueType: "task", Priority: 0,
			})
			if err != nil {
				t.Fatalf("CreateIssue() error = %v", err)
			}
			// Drive the issue into the from-state via direct TransitionIssue
			// calls so the setup path is independent of the ApplyUpdate path
			// under test.
			if tc.from != model.StateOpen {
				if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
					IssueID: issue.ID, Action: "start", CreatedBy: "setup", Assignee: "setup",
				}); err != nil {
					t.Fatalf("setup TransitionIssue(start) error = %v", err)
				}
			}
			if tc.from == model.StateClosed {
				if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
					IssueID: issue.ID, Action: "done", CreatedBy: "setup",
				}); err != nil {
					t.Fatalf("setup TransitionIssue(done) error = %v", err)
				}
			}
			before, err := st.GetIssueDetail(ctx, issue.ID)
			if err != nil {
				t.Fatalf("GetIssueDetail(before) error = %v", err)
			}
			eventsBefore := len(before.Events)

			updated, err := st.ApplyUpdate(ctx, issue.ID, ApplyUpdateInput{
				TargetStatus:       string(tc.to),
				TransitionBy:       "tester",
				TransitionAssignee: "tester",
			})
			if err != nil {
				t.Fatalf("ApplyUpdate(%s -> %s) error = %v", tc.from, tc.to, err)
			}
			if updated.State() != tc.to {
				t.Fatalf("updated.State() = %q, want %q", updated.State(), tc.to)
			}

			after, err := st.GetIssueDetail(ctx, issue.ID)
			if err != nil {
				t.Fatalf("GetIssueDetail(after) error = %v", err)
			}
			added := after.Events[eventsBefore:]
			if len(added) != 1 {
				t.Fatalf("ApplyUpdate(%s -> %s) recorded %d events, want exactly 1: %#v",
					tc.from, tc.to, len(added), added)
			}
			if added[0].Action != tc.wantAction {
				t.Fatalf("ApplyUpdate(%s -> %s) action = %q, want %q",
					tc.from, tc.to, added[0].Action, tc.wantAction)
			}
		})
	}
}

// TestApplyUpdateSameTargetStateStillRecordsEvent asserts that ApplyUpdate
// records an event even when TargetStatus matches the current status. The
// Actor on issue_events is the audit substrate for "list every agent that
// interacted with this ticket" history queries; suppressing the event for
// same-state would erase the agent's claim from history. In particular,
// `lit update --status in_progress` on an already-in_progress issue is the
// canonical agent-reclaim path — it must record a start event with the new
// assignee, not silently no-op.
func TestApplyUpdateSameTargetStateStillRecordsEvent(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix: "test", Title: "reclaim via update", Topic: "lifecycle", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID: issue.ID, Action: "start", CreatedBy: "agent-a", Assignee: "agent-a",
	}); err != nil {
		t.Fatalf("setup TransitionIssue(start) error = %v", err)
	}
	before, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(before) error = %v", err)
	}
	eventsBefore := len(before.Events)

	if _, err := st.ApplyUpdate(ctx, issue.ID, ApplyUpdateInput{
		TargetStatus:       "in_progress",
		TransitionBy:       "agent-b",
		TransitionAssignee: "agent-b",
	}); err != nil {
		t.Fatalf("ApplyUpdate(in_progress -> in_progress) error = %v", err)
	}

	after, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(after) error = %v", err)
	}
	added := after.Events[eventsBefore:]
	if len(added) != 1 {
		t.Fatalf("same-state ApplyUpdate recorded %d events, want exactly 1: %#v", len(added), added)
	}
	if added[0].Action != "start" {
		t.Fatalf("same-state ApplyUpdate action = %q, want %q (claim must be recorded as start)",
			added[0].Action, "start")
	}
	if added[0].Actor != "agent-b" {
		t.Fatalf("same-state ApplyUpdate actor = %q, want %q (audit substrate must capture caller)",
			added[0].Actor, "agent-b")
	}
	reclaimed, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue(reclaimed) error = %v", err)
	}
	if got, want := reclaimed.AssigneeValue(), "agent-b"; got != want {
		t.Fatalf("reclaimed.AssigneeValue() = %q, want %q (start rewrites assignee even same-state)", got, want)
	}
}

func TestIssueStatusClaimAndDoneAreDeterministic(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Claim me", Topic: "claims", IssueType: "task", Priority: 0})
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
		Assignee:  "agent-a",
	})
	if err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	if started.State() != model.StateInProgress {
		t.Fatalf("started.State() = %q, want in_progress", started.State())
	}

	// Under the target-state lifecycle, `start --assignee` is sugar for
	// "set to InProgress + wire --assignee to the assignee column." A second
	// start on an already-in-progress issue is therefore a same-state claim
	// transfer (assignee column rewritten) rather than a verb-strict
	// rejection. Persistence is the contract — reload from the store to
	// assert the assignee column, since writeStatusTransition returns the
	// pre-Apply lifecycle snapshot.
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{
		IssueID:   issue.ID,
		Action:    "start",
		Reason:    "competing claim",
		CreatedBy: "agent-b",
		Assignee:  "agent-b",
	}); err != nil {
		t.Fatalf("TransitionIssue(start by agent-b) error = %v, want same-state success", err)
	}
	reclaimed, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if reclaimed.State() != model.StateInProgress {
		t.Fatalf("reclaimed.State() = %q, want in_progress", reclaimed.State())
	}
	if reclaimed.AssigneeValue() != "agent-b" {
		t.Fatalf("reclaimed.AssigneeValue() = %q, want agent-b", reclaimed.AssigneeValue())
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

	openIssues, err := st.ListIssues(ctx, ListIssuesFilter{Statuses: []model.State{model.StateOpen}})
	if err != nil {
		t.Fatalf("ListIssues(open) error = %v", err)
	}
	if len(openIssues) != 0 {
		t.Fatalf("openIssues = %#v, want empty", openIssues)
	}

	closedIssues, err := st.ListIssues(ctx, ListIssuesFilter{Statuses: []model.State{model.StateClosed}})
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

// TestOpenForReadRefusesUnreconcilableShape pins the contract that a
// workspace whose shape is structurally beyond any prior canonical state
// (here, an issues table with ONLY an id column — no title, no status, no
// description; far worse than any real historical shape ever had) cannot be
// forward-migrated and produces a SPECIFIC error naming the actual missing
// prerequisite.
//
// Compare to the historical canonical shapes the reconcile handles
// (TestOpenForwardMigratesPreConvergedColumnShape etc.) which forward-migrate
// to v1. The discriminator is: an issues table whose required columns are
// missing (status, priority, updated_at, issue_type, closed_at) trips the
// verifyIssuesReconcilable preflight in runMigration — the structural refusal
// fires BEFORE any reconcile DDL runs, naming the specific missing column(s).
//
// [LAW:no-silent-fallbacks] An unrecoverable shape produces a specific
// error naming the missing prerequisite column, not a vague refusal —
// and refuses before any mutation lands on the workspace.
func TestOpenForReadRefusesUnreconcilableShape(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	if _, err := EnsureDatabase(ctx, doltRoot, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}
	seed, err := openStoreConnection(doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("openStoreConnection() error = %v", err)
	}
	// A structurally impossible issues table: no real workspace shape ever
	// had issues without status/priority/updated_at/issue_type/closed_at.
	// verifyIssuesReconcilable in runMigration probes for these required
	// columns and refuses before any reconcile DDL runs.
	if _, err := seed.db.ExecContext(ctx, `CREATE TABLE issues (id VARCHAR(191) PRIMARY KEY)`); err != nil {
		_ = seed.Close()
		t.Fatalf("create unreconcilable schema error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close() error = %v", err)
	}

	_, err = OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("OpenForRead() on an unreconcilable schema returned nil error; expected a specific structural error")
	}
	// The error must name the missing prerequisite column — not a vague
	// "partial" or "restore from snapshot" message. verifyIssuesReconcilable
	// emits "missing reconcile prerequisites (...)" naming each absent
	// required column (status is one of them for this fixture).
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("error %q does not name the structural anomaly (missing status column)", err)
	}
	if strings.Contains(err.Error(), "restore it from a snapshot or recreate") {
		t.Fatalf("error %q still contains the data-destroying refusal guidance that was removed", err)
	}
}

func TestListChildrenReturnsEpicChildrenWithDerivedLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	root, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Root epic", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(root) error = %v", err)
	}
	sub, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Sub epic", Topic: "life", IssueType: "epic", Priority: 1, ParentID: root.ID})
	if err != nil {
		t.Fatalf("CreateIssue(sub) error = %v", err)
	}
	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "life", IssueType: "task", Priority: 0, ParentID: sub.ID})
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

	parent, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Parent epic", Topic: "detail", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	subject, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Subject", Topic: "detail", IssueType: "task", Priority: 0, ParentID: parent.ID})
	if err != nil {
		t.Fatalf("CreateIssue(subject) error = %v", err)
	}
	dependency, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependency epic", Topic: "detail", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(dependency) error = %v", err)
	}
	dependencyLeaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Dependency leaf", Topic: "detail", IssueType: "task", Priority: 0, ParentID: dependency.ID})
	if err != nil {
		t.Fatalf("CreateIssue(dependency leaf) error = %v", err)
	}
	if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: dependencyLeaf.ID, Action: "close", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(close dependency leaf) error = %v", err)
	}
	related, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Related epic", Topic: "detail", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(related) error = %v", err)
	}
	blocked, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocked by subject", Topic: "detail", IssueType: "task", Priority: 0})
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

	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(leaf) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic dep", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic child", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epic.ID})
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Container", Topic: "schema", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "schema", IssueType: "task", Priority: 0})
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Container", Topic: "schema", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	leaf, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "schema", IssueType: "task", Priority: 0})
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
	epic, err := source.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Sync epic", Topic: "sync", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := source.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Sync leaf", Topic: "sync", IssueType: "task", Priority: 0, ParentID: epic.ID})
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
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Close me", Topic: "life", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := st.writeStatusTransition(ctx, issue, "tester", "", "close", ""); err != nil {
		t.Fatalf("writeStatusTransition(first) error = %v", err)
	}
	_, err = st.writeStatusTransition(ctx, issue, "tester", "", "close", "")
	if err == nil || err.Error() != `close conflict: issue status is "closed"` {
		t.Fatalf("writeStatusTransition(second) error = %v, want close conflict", err)
	}
}

func TestArchiveReturnsHydratedIssue(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Archive epic", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child", Topic: "life", IssueType: "task", Priority: 0, ParentID: epic.ID}); err != nil {
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Archived snapshot", Topic: "life", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Archived child", Topic: "life", IssueType: "task", Priority: 0, ParentID: epic.ID})
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
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Reopen me", Topic: "life", IssueType: "task", Priority: 0})
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
	hydrated, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Hydrated", Topic: "life", IssueType: "task", Priority: 0})
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
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Archive me", Topic: "life", IssueType: "epic", Priority: 1})
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
	a, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "A", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	b, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "B", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}
	c, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "C", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
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
	a, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "A", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
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
	a, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "A", Topic: "rank", IssueType: "task", Priority: 0, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	if err := st.RankSet(ctx, []string{a.ID}); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("RankSet(single id) error = %v, want too-few error", err)
	}
}

// TestRemovePerChildBlockAfterRankReorder reproduces the bug where per-child
// block edges added when an epic-level block already exists cannot be removed
// after a rank reorder. The store-level orientation for blocks is:
// src=dependent (blocked), dst=dependency (blocker).
func TestRemovePerChildBlockAfterRankReorder(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	// Create blocker epic A and blocked epic B.
	epicA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocker epic A", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicA) error = %v", err)
	}
	epicB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Blocked epic B", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicB) error = %v", err)
	}

	// Create children of B.
	childB1, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child B.1", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childB1) error = %v", err)
	}
	childB2, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child B.2", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childB2) error = %v", err)
	}
	childB3, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child B.3", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childB3) error = %v", err)
	}

	// Add epic-to-epic block: A blocks B.
	// Store convention: src=blocked (B), dst=blocker (A).
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: epicB.ID, DstID: epicA.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(epic-level block) error = %v", err)
	}

	// Add per-child blocks: A blocks B.1, B.2, B.3.
	for _, childID := range []string{childB1.ID, childB2.ID, childB3.ID} {
		if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: childID, DstID: epicA.ID, Type: "blocks", CreatedBy: "tester"}); err != nil {
			t.Fatalf("AddRelation(per-child block %s) error = %v", childID, err)
		}
	}

	// Rank A above B.
	if _, err := st.RankAbove(ctx, epicA.ID, epicB.ID); err != nil {
		t.Fatalf("RankAbove(A, B) error = %v", err)
	}

	// Remove per-child blocks — this is where the bug manifests.
	for _, childID := range []string{childB1.ID, childB2.ID, childB3.ID} {
		if err := st.RemoveRelation(ctx, childID, epicA.ID, "blocks"); err != nil {
			t.Errorf("RemoveRelation(per-child block %s) error = %v", childID, err)
		}
	}

	// Remove epic-level block.
	if err := st.RemoveRelation(ctx, epicB.ID, epicA.ID, "blocks"); err != nil {
		t.Fatalf("RemoveRelation(epic-level block) error = %v", err)
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
