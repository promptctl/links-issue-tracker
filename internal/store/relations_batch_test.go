package store

import (
	"context"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// GetRelationsByIDs must return, for every subject, the same structural edges
// GetIssueDetail resolves — it is the batch form of the same query, so the two
// cannot disagree.
func TestGetRelationsByIDsMatchesIssueDetail(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	epic, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "rel", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	childA, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child A", Topic: "rel", IssueType: "task", Priority: 0, ParentID: epic.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(childA) error = %v", err)
	}
	childB, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Child B", Topic: "rel", IssueType: "task", Priority: 0, ParentID: epic.ID, Placement: RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(childB) error = %v", err)
	}
	upstream, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Upstream", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(upstream) error = %v", err)
	}
	downstream, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Downstream", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(downstream) error = %v", err)
	}
	peer, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Peer", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(peer) error = %v", err)
	}
	// childA depends on upstream; downstream depends on childA; childA related-to peer.
	mustRelate(t, ctx, st, childA.ID, upstream.ID, "blocks")
	mustRelate(t, ctx, st, downstream.ID, childA.ID, "blocks")
	mustRelate(t, ctx, st, childA.ID, peer.ID, "related-to")

	subjects := []string{epic.ID, childA.ID, childB.ID}
	rels, err := st.GetRelationsByIDs(ctx, append(subjects, "test-nonexistent"))
	if err != nil {
		t.Fatalf("GetRelationsByIDs() error = %v", err)
	}

	if _, ok := rels["test-nonexistent"]; ok {
		t.Errorf("a subject that does not exist must be absent from the result, got a bundle")
	}

	for _, id := range subjects {
		rel, ok := rels[id]
		if !ok {
			t.Fatalf("subject %s missing from batch result", id)
		}
		detail, err := st.GetIssueDetail(ctx, id)
		if err != nil {
			t.Fatalf("GetIssueDetail(%s) error = %v", id, err)
		}
		if rel.Issue.ID != detail.Issue.ID {
			t.Errorf("%s: Issue.ID = %q, want %q", id, rel.Issue.ID, detail.Issue.ID)
		}
		assertSameIDs(t, id+" Children", rel.Children, detail.Children)
		assertSameIDs(t, id+" DependsOn", rel.DependsOn, detail.DependsOn)
		assertSameIDs(t, id+" Blocks", rel.Blocks, detail.Blocks)
		if (rel.Parent == nil) != (detail.Parent == nil) {
			t.Errorf("%s: Parent nil-ness differs (batch %v, detail %v)", id, rel.Parent, detail.Parent)
		}
		if rel.Parent != nil && detail.Parent != nil && rel.Parent.ID != detail.Parent.ID {
			t.Errorf("%s: Parent.ID = %q, want %q", id, rel.Parent.ID, detail.Parent.ID)
		}
	}

	// Spot-check the wiring the consumers actually read.
	if got := rels[epic.ID].Children; len(got) != 2 || got[0].ID != childA.ID || got[1].ID != childB.ID {
		t.Errorf("epic children = %v, want [%s %s] in rank order", ids(got), childA.ID, childB.ID)
	}
	if got := rels[childA.ID].DependsOn; len(got) != 1 || got[0].ID != upstream.ID {
		t.Errorf("childA DependsOn = %v, want [%s]", ids(got), upstream.ID)
	}
	if got := rels[childA.ID].Blocks; len(got) != 1 || got[0].ID != downstream.ID {
		t.Errorf("childA Blocks = %v, want [%s]", ids(got), downstream.ID)
	}
}

func mustRelate(t *testing.T, ctx context.Context, st *Store, src, dst string, relType model.RelationType) {
	t.Helper()
	if _, err := st.AddRelation(ctx, AddRelationInput{SrcID: src, DstID: dst, Type: relType, CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(%s %s->%s) error = %v", relType, src, dst, err)
	}
}

func assertSameIDs(t *testing.T, label string, got, want []model.Issue) {
	t.Helper()
	gotIDs, wantIDs := ids(got), ids(want)
	if len(gotIDs) != len(wantIDs) {
		t.Errorf("%s: ids = %v, want %v", label, gotIDs, wantIDs)
		return
	}
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("%s: ids = %v, want %v", label, gotIDs, wantIDs)
			return
		}
	}
}

func ids(issues []model.Issue) []string {
	out := make([]string, len(issues))
	for i, issue := range issues {
		out[i] = issue.ID
	}
	return out
}

// mergeRelations reunifies the two per-endpoint-column queries into what the
// old single query returned: each relation once, created_at ascending. The
// dedupe leg is load-bearing on every real backlog — an edge between two
// subjects arrives from both queries — and a duplicate here becomes a
// duplicated child in every bucketed view downstream.
func TestMergeRelationsDedupesAndOrders(t *testing.T) {
	at := func(sec int) time.Time { return time.Date(2026, 8, 25, 0, 0, sec, 0, time.UTC) }
	edge := func(src, dst string, relType model.RelationType, created time.Time) model.Relation {
		return model.Relation{SrcID: src, DstID: dst, Type: relType, CreatedAt: created}
	}
	shared := edge("epic-1", "epic-1.2", model.RelParentChild, at(1))
	srcOnly := edge("epic-1", "outside-a", model.RelParentChild, at(3))
	dstOnly := edge("outside-b", "epic-1.2", model.RelBlocks, at(0))
	// Same timestamp as shared: order must still be deterministic, by key.
	tied := edge("epic-1", "epic-1.2", model.RelBlocks, at(1))

	got := mergeRelations(
		[]model.Relation{shared, srcOnly, tied},
		[]model.Relation{dstOnly, shared, tied},
	)

	want := []model.Relation{dstOnly, tied, shared, srcOnly}
	if len(got) != len(want) {
		t.Fatalf("merged %d relations, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("merged[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
