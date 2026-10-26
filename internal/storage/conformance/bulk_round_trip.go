package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// aBulkFileReadOutOfTheStoreGoesBackInAsANoOp is the contract behind editing
// tickets in place through `lit ls --format yaml` and `lit import`: the update
// documents stating a selection's editable state, applied unedited, write
// nothing — no event, no updated_at, no edge — and are reported as held, not
// updated; the same documents with one title edited move exactly that title on
// exactly that issue. The selection holds an epic waiting on a leaf of another
// epic that waits on a leaf of the first: a loop only through the hierarchy,
// which the store keeps and restating it writes nothing, so it is no refusal.
func aBulkFileReadOutOfTheStoreGoesBackInAsANoOp(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	// blocked_by never joins two issues of one epic, so the blocker has its own.
	elsewhere := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "elsewhere", Topic: "core", IssueType: model.TypeEpic})
	blocker := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "blocker", Topic: "core", ParentID: elsewhere.ID})
	full := mustCreate(t, ctx, st, storage.CreateIssueInput{
		Title: "full", Topic: "core", ParentID: epic.ID, Priority: model.PriorityUrgent,
		Description: "first line\n\nsecond paragraph", Prompt: "a prompt", Plan: "a plan",
		Assignee: "ada", Lane: "a-lane", Labels: []string{"alpha", "beta"},
	})
	for _, edge := range [][2]string{{full.ID, blocker.ID}, {elsewhere.ID, full.ID}} {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: edge[0], DstID: edge[1], Type: model.RelBlockedBy, CreatedBy: "ada"}); err != nil {
			t.Fatalf("AddRelation(%s blocked by %s) error = %v", edge[0], edge[1], err)
		}
	}
	ids := []string{epic.ID, elsewhere.ID, blocker.ID, full.ID}
	// A write the round trip makes would stamp this instant, so an updated_at
	// it bumped reads as different from the one the setup left.
	clk.Pin(laterInstant)
	before := stateOf(t, ctx, st, ids)
	docs := documentsOf(t, ctx, st, ids)

	result, err := st.BulkApply(ctx, prefix, "ada", docs)
	if err != nil {
		t.Fatalf("BulkApply(the selection's own documents) error = %v", err)
	}
	assertStrings(t, "updated", result.Updated, nil)
	assertStrings(t, "fields held", result.FieldsHeld, ids)
	// The two epics sit at the top level, so their documents state no parent.
	assertStrings(t, "parent held", result.ParentHeld, []string{blocker.ID, full.ID})
	assertStrings(t, "blocked_by held", result.BlockedByHeld, ids)
	if result.EdgesAdded+result.EdgesRemoved != 0 {
		t.Errorf("edges added %d, removed %d, want none", result.EdgesAdded, result.EdgesRemoved)
	}
	for id, was := range before {
		if now := stateOf(t, ctx, st, []string{id})[id]; now.updatedAt != was.updatedAt || now.events != was.events {
			t.Errorf("%s after the unedited round trip: updated_at %s, %d events; before it: %s, %d events", id, now.updatedAt, now.events, was.updatedAt, was.events)
		}
	}

	edited := "full, retitled"
	docs[len(docs)-1].Title = &edited
	result, err = st.BulkApply(ctx, prefix, "ada", docs)
	if err != nil {
		t.Fatalf("BulkApply(one title edited) error = %v", err)
	}
	assertStrings(t, "updated", result.Updated, []string{full.ID})
	assertStrings(t, "fields held", result.FieldsHeld, ids[:len(ids)-1])
	events, err := st.ListEvents(ctx, full.ID)
	if err != nil {
		t.Fatalf("ListEvents(%s) error = %v", full.ID, err)
	}
	if len(events) != before[full.ID].events+1 {
		t.Fatalf("%s has %d events after one edited title, want %d", full.ID, len(events), before[full.ID].events+1)
	}
	if got, want := events[len(events)-1].Changes, []model.FieldChange{{Field: "title", From: "full", To: edited}}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("the edit's event records %+v, want %+v", got, want)
	}
	for _, id := range ids[:len(ids)-1] {
		if now := stateOf(t, ctx, st, []string{id})[id]; now != before[id] {
			t.Errorf("%s moved when only %s's title was edited: %+v, before %+v", id, full.ID, now, before[id])
		}
	}
}

// issueState is what a write to an issue would move: its updated_at and the
// length of its history.
type issueState struct {
	updatedAt string
	events    int
}

func stateOf(t *testing.T, ctx context.Context, st storage.Store, ids []string) map[string]issueState {
	t.Helper()
	out := make(map[string]issueState, len(ids))
	for _, id := range ids {
		issue, err := st.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", id, err)
		}
		events, err := st.ListEvents(ctx, id)
		if err != nil {
			t.Fatalf("ListEvents(%s) error = %v", id, err)
		}
		out[id] = issueState{updatedAt: issue.UpdatedAt.String(), events: len(events)}
	}
	return out
}

// documentsOf reads the update documents stating each issue's editable state,
// in the order ids names them, as `lit ls --format yaml` does.
func documentsOf(t *testing.T, ctx context.Context, st storage.Store, ids []string) []storage.BulkIssueSpec {
	t.Helper()
	relations, err := st.GetRelationsByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("GetRelationsByIDs error = %v", err)
	}
	docs := make([]storage.BulkIssueSpec, len(ids))
	for i, id := range ids {
		docs[i] = storage.BulkUpdateDocument(relations[id])
	}
	return docs
}

// aReadOutFileKeepsWhatOtherWritersChanged is the contract behind editing a
// read-out file while other agents work: between the read and the import,
// another writer raises the issue's priority, moves its lane and adds a
// blocker, and the file edits only the title. The title lands; the priority,
// the lane and the new edge stand, and the result names each as kept. The same
// file moving the lane elsewhere too is a conflict that writes nothing, since
// landing it would erase the other writer's lane.
func aReadOutFileKeepsWhatOtherWritersChanged(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	elsewhere := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "elsewhere", Topic: "core", IssueType: model.TypeEpic})
	blocker := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "blocker", Topic: "core", ParentID: elsewhere.ID})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "issue", Topic: "core", ParentID: epic.ID})
	docs := documentsOf(t, ctx, st, []string{issue.ID})

	clk.Pin(laterInstant)
	urgent, theirs := model.PriorityUrgent, "their-lane"
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Fields: storage.UpdateIssueInput{Priority: &urgent, Lane: &theirs}, Actor: "bea"}); err != nil {
		t.Fatalf("Apply(another writer's priority and lane) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: issue.ID, DstID: blocker.ID, Type: model.RelBlockedBy, CreatedBy: "bea"}); err != nil {
		t.Fatalf("AddRelation(another writer's blocker) error = %v", err)
	}

	conflicting := docs[0]
	ours := "our-lane"
	conflicting.Lane = &ours
	before := mustExport(t, ctx, st)
	_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{conflicting})
	var conflict storage.ConflictError
	if !errors.As(err, &conflict) || len(conflict.Conflicts) != 1 || conflict.Conflicts[0].Slot != (storage.Slot{Kind: storage.FieldSlot, Issue: issue.ID, Name: "lane"}) {
		t.Fatalf("BulkApply(a lane edit over another writer's) error = %v, want one conflict on %s lane", err, issue.ID)
	}
	assertWroteNothing(t, ctx, st, before, "BulkApply(a conflicting read-out file)")

	retitled := "issue, retitled"
	docs[0].Title = &retitled
	result, err := st.BulkApply(ctx, prefix, "ada", docs)
	if err != nil {
		t.Fatalf("BulkApply(a title edit beside another writer's changes) error = %v", err)
	}
	assertStrings(t, "updated", result.Updated, []string{issue.ID})
	kept := make([]string, len(result.Kept))
	for i, slot := range result.Kept {
		kept[i] = slot.String()
	}
	assertStrings(t, "kept", kept, []string{issue.ID + " priority", issue.ID + " lane", issue.ID + " blocked-by " + blocker.ID})
	relations, err := st.GetRelationsByIDs(ctx, []string{issue.ID})
	if err != nil {
		t.Fatalf("GetRelationsByIDs error = %v", err)
	}
	now := relations[issue.ID]
	if now.Issue.Title != retitled || now.Issue.Priority != urgent || now.Issue.Lane != theirs {
		t.Errorf("after the import: title %q, priority %v, lane %q; want %q and the other writer's %v and %q", now.Issue.Title, now.Issue.Priority, now.Issue.Lane, retitled, urgent, theirs)
	}
	if len(now.BlockedBy) != 1 || now.BlockedBy[0].ID != blocker.ID {
		t.Errorf("after the import: blocked by %v, want the other writer's %s", now.BlockedBy, blocker.ID)
	}
	if now.Parent == nil || now.Parent.ID != epic.ID {
		t.Errorf("after the import: parent %v, want %s", now.Parent, epic.ID)
	}
}
