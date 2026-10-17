package storage

import (
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The conformance suite pins tree order on both engines through listings, but
// neither engine can be driven into the two hierarchies the write boundary
// refuses: a second parent and a parent cycle. Restored data can still carry
// either, so what tree order does with them is pinned here, on the edges alone.

func treeOrder(t *testing.T, ancestry RankAncestry, issues []model.Issue) []string {
	t.Helper()
	ancestry.Sort(issues)
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

// A child two parents claim lists under the lower parent id, with its own
// subtree, however the edges arrive — and the conflict is kept for doctor.
// The parent ranked first is the higher id, so listing the child under
// whichever edge came first would put it in the other place.
func TestAChildTwoParentsClaimListsUnderTheLowerID(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "c", ParentID: "p2", ParentRank: "a"},
		{ChildID: "c", ParentID: "p1", ParentRank: "b"},
		{ChildID: "c.g", ParentID: "c", ParentRank: "m"},
		{ChildID: "p1.x", ParentID: "p1", ParentRank: "b"},
	})
	got := treeOrder(t, ancestry, []model.Issue{
		{ID: "p1.x", Rank: "z"},
		{ID: "c.g", Rank: "a"},
		{ID: "p1", Rank: "b"},
		{ID: "c", Rank: "m"},
		{ID: "p2", Rank: "a"},
	})
	if want := []string{"p2", "p1", "c", "c.g", "p1.x"}; !slices.Equal(got, want) {
		t.Errorf("tree order = %v, want %v", got, want)
	}
	want := []ParentConflict{{ChildID: "c", Parents: []string{"p1", "p2"}}}
	if got := ancestry.Conflicts(); !slices.EqualFunc(got, want, func(a, b ParentConflict) bool {
		return a.ChildID == b.ChildID && slices.Equal(a.Parents, b.Parents)
	}) {
		t.Errorf("Conflicts() = %v, want %v", got, want)
	}
}

// A chain that loops still yields a place for every issue on it: the walk
// ends where it would repeat, so a listing over the loop returns.
func TestAParentLoopStillSorts(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "a", ParentID: "b", ParentRank: "k"},
		{ChildID: "b", ParentID: "a", ParentRank: "m"},
		{ChildID: "d", ParentID: "a", ParentRank: "m"},
	})
	got := treeOrder(t, ancestry, []model.Issue{{ID: "d", Rank: "x"}, {ID: "a", Rank: "m"}, {ID: "b", Rank: "k"}, {ID: "t", Rank: "a"}})
	if len(got) != 4 {
		t.Errorf("tree order = %v, want all four issues", got)
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
	got := treeOrder(t, ancestry, issues)
	want := []string{"e0", "e1", "e1.a", "e1.b", "e2", "e2.a", "e3"}
	if !slices.Equal(got, want) {
		t.Fatalf("tree order = %v, want %v", got, want)
	}
}

// A rank inversion is read in tree order: the two issues' representatives in
// the frame that holds both are compared, never their own keys across frames.
// Each case names what its own keys alone would say, so a reading that
// compared raw keys fails the cases where the two disagree. An issue and its
// own container or descendant are never inverted, either way round.
func TestInvertedReadsTreeOrder(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "e1.x", ParentID: "e1", ParentRank: "b"},
		{ChildID: "e1.y", ParentID: "e1", ParentRank: "b"},
		{ChildID: "e1.n", ParentID: "e1", ParentRank: "b"},
		{ChildID: "e1.n.q", ParentID: "e1.n", ParentRank: "m"},
		{ChildID: "e2.p", ParentID: "e2", ParentRank: "c"},
	})
	issue := map[string]model.Issue{}
	for _, i := range []model.Issue{
		{ID: "t", Rank: "a"}, {ID: "e1", Rank: "b"}, {ID: "e2", Rank: "c"}, {ID: "t2", Rank: "d"},
		{ID: "e1.y", Rank: "a"}, {ID: "e1.n", Rank: "m"}, {ID: "e1.x", Rank: "z"},
		{ID: "e1.n.q", Rank: "a"}, {ID: "e2.p", Rank: "a"},
	} {
		issue[i.ID] = i
	}
	cases := []struct {
		dependent, dependency string
		rawSays, want         bool
	}{
		{"e1.y", "e1.x", true, true},     // frame-mates: their own keys decide
		{"e1.x", "e1.y", false, false},   // frame-mates, the other way round
		{"e1.x", "e2.p", false, true},    // e2 lists after e1, whatever e2.p's key
		{"e2.p", "e1.x", true, false},    // e1 lists before e2, whatever e1.x's key
		{"t", "e1.y", false, true},       // a top-level issue against a child: e1 is after t
		{"t2", "e1.y", false, false},     // e1 is before t2
		{"e1.y", "e1.n.q", false, true},  // different depths: e1.n stands for e1.n.q, after e1.y
		{"e1.x", "e1.n.q", false, false}, // e1.n is before e1.x
		{"e1", "e1.x", true, false},      // a container waiting on its own child
		{"e1.x", "e1", false, false},     // a child waiting on its own container
		{"e1", "e1.n.q", false, false},   // containment more than one level down
		{"e1.x", "e1.x", false, false},   // an issue and itself
	}
	for _, tc := range cases {
		dependent, dependency := issue[tc.dependent], issue[tc.dependency]
		if raw := dependency.Rank > dependent.Rank; raw != tc.rawSays {
			t.Fatalf("%s blocked by %s: raw keys say %v, case claims %v — the case no longer tests what it says", tc.dependent, tc.dependency, raw, tc.rawSays)
		}
		if got := ancestry.Inverted(dependent, dependency); got != tc.want {
			t.Errorf("Inverted(%s blocked by %s) = %v, want %v", tc.dependent, tc.dependency, got, tc.want)
		}
	}
}
