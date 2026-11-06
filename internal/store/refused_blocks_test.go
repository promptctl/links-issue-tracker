package store

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A reparent checks no blocked-by edge, so moving a task under the epic of the
// task it waits on leaves an edge `lit dep add` would now refuse. Doctor names
// each such edge as a warning, with the `lit dep rm` that removes it, and the
// store stays readable: an older lit's store holds these legitimately.
func TestDoctorNamesABlockedByEdgeAReparentLeftInsideOneEpic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	create := func(title, issueType string, parentID model.ID) model.Issue {
		t.Helper()
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle(title), Lane: storage.SharedLane, Topic: "blocks", IssueType: model.IssueType(issueType), ParentID: parentID, Placement: storage.RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		return issue
	}
	second := create("E2", "epic", "")
	first := create("E1", "epic", "")
	a, b := create("A", "task", first.ID), create("B", "task", second.ID)
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: a.ID, DstID: b.ID, Type: model.RelBlockedBy, CreatedBy: "test"}); err != nil {
		t.Fatalf("AddRelation(A blocked-by B across epics) error = %v, want it legal", err)
	}

	clean, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(clean.RefusedBlocks) != 0 || len(clean.Warnings) != 0 {
		t.Fatalf("Doctor() refused blocks %v, warnings %v on an edge between two epics; want none", clean.RefusedBlocks, clean.Warnings)
	}

	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: a.ID, ParentID: second.ID, CreatedBy: "test"}); err != nil {
		t.Fatalf("SetParent(A under E2) error = %v, want the reparent to go through over the edge", err)
	}
	// Tree order puts A under E2 wherever its old key falls; ranking it below B
	// settles the inversion the move left, so the edge is the only finding.
	if _, err := st.RankBelow(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("RankBelow(A, B) error = %v", err)
	}
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(report.Errors) != 0 {
		t.Errorf("Doctor().Errors = %v, want none: every read answers over the edge", report.Errors)
	}
	if want := []storage.DependencyEdge{{Dependent: a.ID, Dependency: b.ID}}; !equalEdges(report.RefusedBlocks, want) {
		t.Fatalf("Doctor().RefusedBlocks = %v, want %v", report.RefusedBlocks, want)
	}
	wantRepair := "'lit dep rm " + a.ID.Raw() + " --blocked-by " + b.ID.Raw() + "'"
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], wantRepair) {
		t.Fatalf("Doctor().Warnings = %v, want one warning naming %s", report.Warnings, wantRepair)
	}

	if err := st.RemoveRelation(ctx, a.ID, b.ID, model.RelBlockedBy); err != nil {
		t.Fatalf("RemoveRelation() error = %v", err)
	}
	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() after the removal error = %v", err)
	}
	if len(after.RefusedBlocks) != 0 || len(after.Warnings) != 0 {
		t.Errorf("Doctor() after the removal: refused blocks %v, warnings %v, want none", after.RefusedBlocks, after.Warnings)
	}
}

func equalEdges(got, want []storage.DependencyEdge) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
