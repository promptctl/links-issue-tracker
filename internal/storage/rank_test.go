package storage

import (
	"strings"
	"testing"
)

// The conformance suite pins tree order on both engines through listings, but
// neither engine can be driven into the two hierarchies tree order has no
// answer for: the write boundary refuses a second parent and a parent cycle.
// Restored data can still carry either, so the refusals are pinned here, on
// the edges alone.

func TestNewRankAncestryRefusesAChildWithTwoParents(t *testing.T) {
	t.Parallel()
	_, err := NewRankAncestry([]ParentLink{
		{ChildID: "c", ParentID: "p1", ParentRank: "a"},
		{ChildID: "c", ParentID: "p2", ParentRank: "b"},
	})
	if err == nil || !strings.Contains(err.Error(), "c has two parents, p1 and p2") {
		t.Fatalf("NewRankAncestry error = %v, want one naming c and both parents", err)
	}
}

func TestNewRankAncestryRefusesAParentCycle(t *testing.T) {
	t.Parallel()
	_, err := NewRankAncestry([]ParentLink{
		{ChildID: "a", ParentID: "b", ParentRank: "k"},
		{ChildID: "b", ParentID: "a", ParentRank: "m"},
	})
	if err == nil || !strings.Contains(err.Error(), "loops back") {
		t.Fatalf("NewRankAncestry error = %v, want a refusal naming the loop", err)
	}
}
