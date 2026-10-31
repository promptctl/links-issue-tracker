package conformance

import (
	"context"
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// adjacencyHoldsWhatTheDetailHolds pins the two single-issue reads to one
// answer: every group GetIssueAdjacency carries is GetIssueDetail's, for a leaf
// with every kind of neighbour and for the epic above it. A missing id is the
// detail's NotFound and not an empty neighbourhood.
func adjacencyHoldsWhatTheDetailHolds(t *testing.T, ctx context.Context, st storage.Store, _ *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	elsewhere := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "elsewhere", Topic: "core", IssueType: model.TypeEpic})
	origin := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "origin", Topic: "core", ParentID: epic.ID})
	subject := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "subject", Topic: "core", ParentID: epic.ID, FollowsUp: origin.ID})
	mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "a follow-up of the subject", Topic: "core", ParentID: epic.ID, FollowsUp: subject.ID})
	canonical := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "canonical", Topic: "core", ParentID: elsewhere.ID})
	blocker := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "blocker", Topic: "core", ParentID: elsewhere.ID})
	dependent := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "dependent", Topic: "core", ParentID: elsewhere.ID})
	peer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "peer", Topic: "core", ParentID: elsewhere.ID})
	for _, edge := range []storage.AddRelationInput{
		{SrcID: subject.ID, DstID: blocker.ID, Type: model.RelBlockedBy, CreatedBy: "ada"},
		{SrcID: dependent.ID, DstID: subject.ID, Type: model.RelBlockedBy, CreatedBy: "ada"},
		{SrcID: subject.ID, DstID: peer.ID, Type: model.RelRelatedTo, CreatedBy: "ada"},
	} {
		if _, err := st.AddRelation(ctx, edge); err != nil {
			t.Fatalf("AddRelation(%+v): %v", edge, err)
		}
	}
	if _, err := st.Apply(ctx, subject.ID, storage.Change{
		Action: model.Close{Outcome: model.Duplicate{Of: canonical.ID}}, Actor: "ada",
	}); err != nil {
		t.Fatalf("Apply close-as-duplicate error = %v", err)
	}

	for _, id := range []string{subject.ID, epic.ID} {
		detail, err := st.GetIssueDetail(ctx, id)
		if err != nil {
			t.Fatalf("GetIssueDetail(%s): %v", id, err)
		}
		adjacency, err := st.GetIssueAdjacency(ctx, id)
		if err != nil {
			t.Fatalf("GetIssueAdjacency(%s): %v", id, err)
		}
		for _, group := range []struct {
			name string
			got  any
			want any
		}{
			{"Issue", adjacency.Issue, detail.Issue},
			{"Relations", adjacency.Relations, detail.Relations},
			{"Parent", adjacency.Parent, detail.Parent},
			{"Children", adjacency.Children, detail.Children},
			{"Siblings", adjacency.Siblings, detail.Siblings},
			{"BlockedBy", adjacency.BlockedBy, detail.BlockedBy},
			{"Blocks", adjacency.Blocks, detail.Blocks},
			{"Related", adjacency.Related, detail.Related},
			{"RedirectTarget", adjacency.RedirectTarget, detail.RedirectTarget},
			{"FollowsUp", adjacency.FollowsUp, detail.FollowsUp},
		} {
			if !reflect.DeepEqual(group.got, group.want) {
				t.Errorf("GetIssueAdjacency(%s).%s = %+v, want the detail's %+v", id, group.name, group.got, group.want)
			}
		}
	}
	// The fixture reaches every group, or the comparison above proves nothing.
	detail, err := st.GetIssueDetail(ctx, subject.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(%s): %v", subject.ID, err)
	}
	if detail.Parent == nil || len(detail.Siblings) == 0 || len(detail.BlockedBy) == 0 || len(detail.Blocks) == 0 || len(detail.Related) == 0 || detail.RedirectTarget == nil || detail.FollowsUp == nil || len(detail.FollowUps) == 0 {
		t.Errorf("the subject's detail = %+v, want every group populated", detail)
	}

	_, err = st.GetIssueAdjacency(ctx, "no-such-issue")
	assertNotFound(t, err, "issue", "GetIssueAdjacency")
}

// treeOrderReadsContainersARowDoesNotShow pins what a read knowing its rows must
// still reach: the leaves here tie in their own frames' keys and in their
// parents', so only the epics two levels up, which no listing below returns,
// order them. The one ranked to the top lists its subtree first, whichever was
// created first, whichever way a read names the rows, and in every group a
// single ticket's neighbours make.
func treeOrderReadsContainersARowDoesNotShow(t *testing.T, ctx context.Context, st storage.Store, _ *clock) {
	subtree := func(title string) (outer, middle, leaf model.Issue) {
		outer = mustCreate(t, ctx, st, storage.CreateIssueInput{Title: title, Topic: "core", IssueType: model.TypeEpic})
		middle = mustCreate(t, ctx, st, storage.CreateIssueInput{Title: title + " middle", Topic: "core", IssueType: model.TypeEpic, ParentID: outer.ID})
		leaf = mustCreate(t, ctx, st, storage.CreateIssueInput{Title: title + " leaf", Topic: "core", ParentID: middle.ID})
		return outer, middle, leaf
	}
	firstOuter, firstMiddle, firstLeaf := subtree("first")
	secondOuter, secondMiddle, secondLeaf := subtree("second")
	for _, outer := range []model.Issue{firstOuter, secondOuter} {
		if _, err := st.RankToTop(ctx, outer.ID); err != nil {
			t.Fatalf("RankToTop(%s): %v", outer.ID, err)
		}
		want := []string{firstLeaf.ID, secondLeaf.ID}
		if outer.ID == secondOuter.ID {
			want = []string{secondLeaf.ID, firstLeaf.ID}
		}
		assertIssueIDs(t, "a listing bound to the parents", mustList(t, ctx, st, storage.ListIssuesFilter{ParentIDs: []string{firstMiddle.ID, secondMiddle.ID}}), want)
		assertIssueIDs(t, "a listing bound to the ids", mustList(t, ctx, st, storage.ListIssuesFilter{IDs: []string{firstLeaf.ID, secondLeaf.ID}}), want)
	}

	third := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "third", Topic: "core", IssueType: model.TypeEpic})
	subject := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "subject", Topic: "core", ParentID: third.ID})
	for _, leaf := range []model.Issue{firstLeaf, secondLeaf} {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: subject.ID, DstID: leaf.ID, Type: model.RelBlockedBy, CreatedBy: "ada"}); err != nil {
			t.Fatalf("AddRelation: %v", err)
		}
	}
	relations, err := st.GetRelationsByIDs(ctx, []string{subject.ID})
	if err != nil {
		t.Fatalf("GetRelationsByIDs: %v", err)
	}
	assertIssueIDs(t, "a ticket's blockers", relations[subject.ID].BlockedBy, []string{secondLeaf.ID, firstLeaf.ID})
}
