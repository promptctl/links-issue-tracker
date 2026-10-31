package store

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// storedOrderKeys reads every issue's id and key off the table, because
// hydrating a parent loop refuses and this is what tree order compares.
func storedOrderKeys(t *testing.T, ctx context.Context, st *Store) []model.Issue {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, `SELECT id, item_rank FROM issues`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var issues []model.Issue
	for rows.Next() {
		var issue model.Issue
		if err := rows.Scan(&issue.ID, &issue.Rank); err != nil {
			t.Fatal(err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return issues
}

func idsOf(issues []model.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

// The climb and the whole read are one answer about the rows a climb was asked
// about, over every fault restored data can hold: a parent loop, a child two
// parents claim, and a deleted parent, which frames nothing. A seed holding a
// child and its own parent reads no edge twice, which the ancestry would report
// as a child with two parents.
func TestTheClimbOrdersAsTheWholeReadDoes(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "outer", "middle", "inner", "leaf")
	other := parentChain(t, ctx, st, "other", "beside")
	claimed := parentChain(t, ctx, st, "claimer", "claimed")
	seedParentEdge(t, ctx, st, claimed[1].ID, other[0].ID)
	loop := parentChain(t, ctx, st, "loop one", "loop two")
	seedParentEdge(t, ctx, st, loop[0].ID, loop[1].ID)
	gone := parentChain(t, ctx, st, "gone", "orphan")
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET deleted_at = '2026-10-31T00:00:00Z' WHERE id = ?`, gone[0].ID); err != nil {
		t.Fatal(err)
	}

	whole, err := loadRankAncestry(ctx, st.db)
	if err != nil {
		t.Fatal(err)
	}
	stored := storedOrderKeys(t, ctx, st)
	for _, seed := range [][]model.Issue{
		stored,
		{chain[3], chain[1]},
		{claimed[1], other[1]},
		{loop[0], chain[3]},
		{gone[1], chain[3]},
	} {
		climbed, err := loadRankAncestryAbove(ctx, st.db, idsOf(seed))
		if err != nil {
			t.Fatal(err)
		}
		byClimb, byWhole := slices.Clone(seed), slices.Clone(seed)
		climbed.Sort(byClimb)
		whole.Sort(byWhole)
		if !slices.Equal(idsOf(byClimb), idsOf(byWhole)) {
			t.Errorf("climbing from %v orders %v, want the whole read's %v", idsOf(seed), idsOf(byClimb), idsOf(byWhole))
		}
	}
	all, err := loadRankAncestryAbove(ctx, st.db, idsOf(stored))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all.Conflicts(), whole.Conflicts()) {
		t.Errorf("a climb from every issue reports conflicts %+v, want the whole read's %+v", all.Conflicts(), whole.Conflicts())
	}
}

// A group of one has nothing to order, so a read of tickets with at most one
// neighbour on each side reads no ancestry, whatever else it reads. Siblings
// share a frame, so however many a ticket has they read none either.
func TestAGroupOfOneClimbsNothing(t *testing.T) {
	var levels, links int
	ctx := WithRankObserver(context.Background(), RankObserver{
		Wrote:    func(int) {},
		Respaced: func(Respace) {},
		Climbed: func(n int) {
			levels++
			links += n
		},
	})
	st := openIssueStore(t, ctx)
	first := parentChain(t, ctx, st, "first", "first child")
	second := parentChain(t, ctx, st, "second", "second child")
	third := parentChain(t, ctx, st, "third", "third child")
	wire := func(blocker model.Issue) {
		t.Helper()
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: third[1].ID, DstID: blocker.ID, Type: model.RelBlockedBy, CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		levels, links = 0, 0
		if _, err := st.GetRelationsByIDs(ctx, []string{third[0].ID, third[1].ID, first[0].ID}); err != nil {
			t.Fatal(err)
		}
	}
	wire(first[1])
	if levels != 0 || links != 0 {
		t.Errorf("one blocker climbed %d levels and %d links, want none", levels, links)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "sibling", Topic: "core", Prefix: "test", IssueType: model.TypeTask, ParentID: first[0].ID}); err != nil {
		t.Fatal(err)
	}
	levels, links = 0, 0
	adjacency, err := st.GetIssueAdjacency(ctx, first[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjacency.Siblings) != 1 {
		t.Fatalf("Siblings = %v, want the one created", idsOf(adjacency.Siblings))
	}
	if levels != 0 || links != 0 {
		t.Errorf("a ticket with a sibling and one neighbour climbed %d levels and %d links, want none", levels, links)
	}
	wire(second[1])
	if levels != 2 || links != 2 {
		t.Errorf("two blockers climbed %d levels and %d links, want the two parent edges and the empty level above them", levels, links)
	}
}

// Siblings compare on their own keys, so a child restored data gives a second
// parent lists among that parent's children by its key, wherever tree order
// files it, in Children and Siblings alike. Here tree order would file it under
// the lower parent, which ranks after the higher one, so it would follow every
// child of the higher parent; its key puts it first.
func TestChildrenCompareOnTheirOwnKeys(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	one := parentChain(t, ctx, st, "one", "held")
	two := parentChain(t, ctx, st, "two", "kept")
	lower, upper := one, two
	if upper[0].ID < lower[0].ID {
		lower, upper = upper, lower
	}
	if _, err := st.RankToTop(ctx, upper[0].ID); err != nil {
		t.Fatal(err)
	}
	child, kept := lower[1], upper[1]
	later, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "later", Topic: "core", Prefix: "test", IssueType: model.TypeTask, ParentID: upper[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	seedParentEdge(t, ctx, st, child.ID, upper[0].ID)
	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, "!", child.ID); err != nil {
		t.Fatal(err)
	}
	relations, err := st.GetRelationsByIDs(ctx, []string{upper[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := idsOf(relations[upper[0].ID].Children), []string{child.ID, kept.ID, later.ID}; !slices.Equal(got, want) {
		t.Errorf("Children of %s = %v, want %v", upper[0].ID, got, want)
	}
	adjacency, err := st.GetIssueAdjacency(ctx, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := idsOf(adjacency.Siblings), []string{child.ID, later.ID}; !slices.Equal(got, want) {
		t.Errorf("Siblings of %s = %v, want %v", kept.ID, got, want)
	}
}
