package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// relationsAndCommentsAreTotallyOrdered pins the order every reader carries
// relations and comments in: oldest first by the instant, ties by the row's
// identity. An export's order is part of its content, so an engine that left
// it to insertion order, to a stamp's spelling, or to whatever order tied rows
// came back in would serialize one store two ways.
//
// The LATER group is written first, and the pair of instants is the one whose
// spellings sort the other way, so a reader returning rows as recorded and one
// comparing the stamps' text both fail. Inside each group every row shares one
// stamp, so only the tie-break orders it.
func relationsAndCommentsAreTotallyOrdered(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	// The creates are inside the later group, not before it: an unpinned create
	// would stamp the real instant, later than either pinned one.
	clk.Pin(laterInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	subject := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "subject", Topic: "core", ParentID: epic.ID})
	peers := make([]model.Issue, 0, 6)
	for _, title := range []string{"one", "two", "three", "four", "five", "six"} {
		peers = append(peers, mustCreate(t, ctx, st, storage.CreateIssueInput{Title: title, Topic: "core", ParentID: epic.ID}))
	}
	for _, group := range []struct {
		at    time.Time
		peers []model.Issue
	}{
		{laterInstant, peers[:3]},
		{earlierInstant, peers[3:]},
	} {
		clk.Pin(group.at)
		for _, peer := range group.peers {
			if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: subject.ID, DstID: peer.ID, Type: model.RelRelatedTo, CreatedBy: "ada"}); err != nil {
				t.Fatalf("AddRelation related-to %s: %v", peer.ID, err)
			}
			if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: subject.ID, Body: storagetest.CommentBody(t, "on "+peer.Title), CreatedBy: "ada"}); err != nil {
				t.Fatalf("AddComment: %v", err)
			}
		}
	}

	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	detail, err := st.GetIssueDetail(ctx, subject.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail: %v", err)
	}
	incident, err := st.ListRelationsForIssue(ctx, subject.ID)
	if err != nil {
		t.Fatalf("ListRelationsForIssue: %v", err)
	}
	// Seven: the parent edge and six related-to edges.
	for _, read := range []struct {
		name string
		rels []model.Relation
	}{{"Export", export.Relations}, {"GetIssueDetail", detail.Relations}, {"ListRelationsForIssue", incident}} {
		if len(read.rels) < 7 {
			t.Fatalf("%s returned %d relations, want at least 7", read.name, len(read.rels))
		}
		assertRelationsOrdered(t, read.name, read.rels)
	}
	for _, read := range []struct {
		name     string
		comments []model.Comment
	}{{"Export", export.Comments}, {"GetIssueDetail", detail.Comments}} {
		if len(read.comments) != 6 {
			t.Fatalf("%s returned %d comments, want 6", read.name, len(read.comments))
		}
		assertCommentsOrdered(t, read.name, read.comments)
	}
}

// assertRelationsOrdered and assertCommentsOrdered check each key on its own,
// deliberately not through storage.RelationOrdering or CommentOrdering: those
// are what both engines sort by, so a check against them could not fail on a
// change to the rule itself. [LAW:behavior-not-structure]
func assertRelationsOrdered(t *testing.T, read string, rels []model.Relation) {
	t.Helper()
	for i := 1; i < len(rels); i++ {
		prev, cur := rels[i-1], rels[i]
		if cur.CreatedAt.Before(prev.CreatedAt) {
			t.Errorf("%s: relations step backwards in time at index %d: %s after %s", read, i, cur.CreatedAt, prev.CreatedAt)
			continue
		}
		prevKey := [3]string{prev.SrcID, prev.DstID, string(prev.Type)}
		curKey := [3]string{cur.SrcID, cur.DstID, string(cur.Type)}
		if cur.CreatedAt.Equal(prev.CreatedAt) && !keyAscends(prevKey[:], curKey[:]) {
			t.Errorf("%s: relations sharing an instant are not ascending by (src, dst, type) at index %d: %v after %v", read, i, curKey, prevKey)
		}
	}
}

func assertCommentsOrdered(t *testing.T, read string, comments []model.Comment) {
	t.Helper()
	for i := 1; i < len(comments); i++ {
		prev, cur := comments[i-1], comments[i]
		if cur.CreatedAt.Before(prev.CreatedAt) {
			t.Errorf("%s: comments step backwards in time at index %d: %s after %s", read, i, cur.CreatedAt, prev.CreatedAt)
			continue
		}
		if cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID <= prev.ID {
			t.Errorf("%s: comments sharing an instant are not id-ascending at index %d: %s after %s", read, i, cur.ID, prev.ID)
		}
	}
}

// keyAscends reports whether cur sorts strictly after prev, compared column by
// column.
func keyAscends(prev, cur []string) bool {
	for i := range prev {
		if prev[i] != cur[i] {
			return prev[i] < cur[i]
		}
	}
	return false
}
