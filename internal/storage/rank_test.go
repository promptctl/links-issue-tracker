package storage

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The conformance suite pins tree order on both engines through listings, but
// neither engine can be driven into the two hierarchies tree order has no
// answer for: the write boundary refuses a second parent and a parent cycle.
// Restored data can still carry either, so the refusals are pinned here, on
// the edges alone.

// A child two parents claim has no place, and neither does anything beneath
// it; the refusal names the child whose edges are the fault. An issue outside
// that subtree still has its place, so a view that holds none of them answers.
func TestTwoParentsLeaveOnlyThatSubtreeUnplaced(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "c", ParentID: "p1", ParentRank: "a"},
		{ChildID: "c", ParentID: "p2", ParentRank: "b"},
		{ChildID: "c.g", ParentID: "c", ParentRank: "m"},
		{ChildID: "p1.x", ParentID: "p1", ParentRank: "a"},
	})
	for _, id := range []string{"c", "c.g"} {
		err := ancestry.Place([]model.Issue{{ID: "p1.x"}, {ID: id}})
		var twoParents TwoParentsError
		if !errors.As(err, &twoParents) || !strings.Contains(err.Error(), "c has two parents, p1 and p2") {
			t.Errorf("Place(%s) error = %v, want a TwoParentsError naming c and both parents", id, err)
		}
	}
	if err := ancestry.Place([]model.Issue{{ID: "p1"}, {ID: "p1.x"}, {ID: "p2"}}); err != nil {
		t.Errorf("Place(outside the subtree) error = %v, want nil", err)
	}
	if err := ancestry.Refusal(); err == nil || !strings.Contains(err.Error(), "c has two parents") {
		t.Errorf("Refusal() = %v, want the workspace's one refusal", err)
	}
}

func TestAParentCycleLeavesItsMembersUnplaced(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "a", ParentID: "b", ParentRank: "k"},
		{ChildID: "b", ParentID: "a", ParentRank: "m"},
	})
	if err := ancestry.Place([]model.Issue{{ID: "a"}}); err == nil || !strings.Contains(err.Error(), "loops back") {
		t.Fatalf("Place(a) error = %v, want a refusal naming the loop", err)
	}
	if err := ancestry.Place([]model.Issue{{ID: "outsider"}}); err != nil {
		t.Errorf("Place(outsider) error = %v, want nil", err)
	}
}

// Two frame-mates can hold one key — restored data, or the empty key of every
// unranked issue — and tree order still keeps each subtree whole. An outsider
// that ties with a container, on either side of it by id, lists wholly before
// or wholly after the container's subtree, never between the container and its
// children; and two tied containers' children do not interleave, whichever
// order their own keys would put them in.
func TestTreeOrderKeepsASubtreeWholeAcrossATiedKey(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "e1.a", ParentID: "e1", ParentRank: "k"},
		{ChildID: "e1.b", ParentID: "e1", ParentRank: "k"},
		{ChildID: "e2.a", ParentID: "e2", ParentRank: "k"},
	})
	issues := []model.Issue{
		{ID: "e2.a", Rank: "a"},
		{ID: "e1.b", Rank: "z"},
		{ID: "e0", Rank: "k"},
		{ID: "e2", Rank: "k"},
		{ID: "e1.a", Rank: "b"},
		{ID: "e3", Rank: "k"},
		{ID: "e1", Rank: "k"},
	}
	if err := ancestry.Sort(issues); err != nil {
		t.Fatalf("Sort error = %v", err)
	}
	got := make([]string, 0, len(issues))
	for _, issue := range issues {
		got = append(got, issue.ID)
	}
	want := []string{"e0", "e1", "e1.a", "e1.b", "e2", "e2.a", "e3"}
	if !slices.Equal(got, want) {
		t.Fatalf("tree order = %v, want %v", got, want)
	}
}
