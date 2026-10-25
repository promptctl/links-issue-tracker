package conformance

import (
	"context"
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
