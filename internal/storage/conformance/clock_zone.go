package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// aClockReadInAnotherZoneRecordsUTC runs every write on a clock that reads in
// a zone that is not UTC. A comment and a relation record the clock's reading
// and a close records the write's stamp, and each reads back as that instant in
// UTC, the contract's one form: the clock gives a reading its form, so no
// writer has to.
func aClockReadInAnotherZoneRecordsUTC(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	offset := time.FixedZone("UTC-5", -5*60*60)
	clk.Pin(earlierInstant.In(offset))
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("work"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})
	peer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("peer"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})

	clk.Pin(laterInstant.In(offset))
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: issue.ID, Body: storagetest.CommentBody(t, "note"), CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: issue.ID, DstID: peer.ID, Type: model.RelRelatedTo, CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddRelation error = %v", err)
	}
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply done error = %v", err)
	}

	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	if len(detail.Comments) != 1 {
		t.Fatalf("detail holds %d comments, want the one added: %+v", len(detail.Comments), detail.Comments)
	}
	var related []model.Relation
	for _, rel := range detail.Relations {
		if rel.Type == model.RelRelatedTo {
			related = append(related, rel)
		}
	}
	if len(related) != 1 {
		t.Fatalf("detail holds %d related-to edges, want the one added: %+v", len(detail.Relations), detail.Relations)
	}
	closedAt := detail.Issue.ClosedAtValue()
	if closedAt == nil {
		t.Fatalf("ClosedAt after done = nil, want the write's stamp")
	}
	for _, read := range []struct {
		name string
		at   time.Time
	}{
		{"the comment's created_at", detail.Comments[0].CreatedAt},
		{"the relation's created_at", related[0].CreatedAt},
		{"closed_at", *closedAt},
	} {
		if !read.at.Equal(laterInstant) {
			t.Errorf("%s = %v, want the clock's %v", read.name, read.at, laterInstant)
		}
		if read.at.Location() != time.UTC {
			t.Errorf("%s = %v in %v, want UTC", read.name, read.at, read.at.Location())
		}
	}
}

// aRankMoveLeavesUpdatedAtAlone runs every rank verb on a clock past the
// issues' creation, each moving something, and pins that no issue's updated_at
// moves. A rank move reorders the backlog; it is not work on the ticket, and
// updated_at says how long the work has sat untouched.
func aRankMoveLeavesUpdatedAtAlone(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("a"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("b"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})
	c := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("c"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})
	frame := map[model.ID]struct{}{a.ID: {}, b.ID: {}, c.ID: {}}
	before := map[model.ID]time.Time{}
	for _, issue := range mustList(t, ctx, st, storage.ListIssuesFilter{}) {
		before[issue.ID] = issue.UpdatedAt
	}

	clk.Pin(laterInstant)
	for _, move := range []struct {
		name string
		run  func() error
		want []model.ID
	}{
		{"RankSet(a, b, c)", func() error { _, err := st.RankSet(ctx, []model.ID{a.ID, b.ID, c.ID}); return err }, []model.ID{a.ID, b.ID, c.ID}},
		{"RankToTop(c)", func() error { _, err := st.RankToTop(ctx, c.ID); return err }, []model.ID{c.ID, a.ID, b.ID}},
		{"RankToBottom(c)", func() error { _, err := st.RankToBottom(ctx, c.ID); return err }, []model.ID{a.ID, b.ID, c.ID}},
		{"RankAbove(c, a)", func() error { _, err := st.RankAbove(ctx, c.ID, a.ID); return err }, []model.ID{c.ID, a.ID, b.ID}},
		{"RankBelow(c, b)", func() error { _, err := st.RankBelow(ctx, c.ID, b.ID); return err }, []model.ID{a.ID, b.ID, c.ID}},
		{"RankSet(b, a)", func() error { _, err := st.RankSet(ctx, []model.ID{b.ID, a.ID}); return err }, []model.ID{b.ID, a.ID, c.ID}},
	} {
		if err := move.run(); err != nil {
			t.Fatalf("%s error = %v", move.name, err)
		}
		assertIssueIDs(t, "the epic's children after "+move.name, childrenOf(mustList(t, ctx, st, storage.ListIssuesFilter{}), frame), move.want)
	}

	for _, issue := range mustList(t, ctx, st, storage.ListIssuesFilter{}) {
		if !issue.UpdatedAt.Equal(before[issue.ID]) {
			t.Errorf("%s updated_at = %v after the rank moves, want the %v it held: a rank move is not work on the ticket", issue.Title, issue.UpdatedAt, before[issue.ID])
		}
	}
}
