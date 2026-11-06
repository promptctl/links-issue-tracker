package storage

import (
	"errors"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The conformance suite pins tree order on both engines through listings, but
// neither engine can be driven into the two hierarchies the write boundary
// refuses: a second parent and a parent cycle. Restored data can still carry
// either, so what tree order does with them is pinned here, on the edges alone.

func treeOrder(t *testing.T, ancestry RankAncestry, issues []model.Issue) []model.ID {
	t.Helper()
	ancestry.Sort(issues)
	ids := make([]model.ID, 0, len(issues))
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
	if want := []model.ID{"p2", "p1", "c", "c.g", "p1.x"}; !slices.Equal(got, want) {
		t.Errorf("tree order = %v, want %v", got, want)
	}
	want := []ParentConflict{{ChildID: "c", Parents: []model.ID{"p1", "p2"}}}
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
	want := []model.ID{"e0", "e1", "e1.a", "e1.b", "e2", "e2.a", "e3"}
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
	issue := map[model.ID]model.Issue{}
	for _, i := range []model.Issue{
		{ID: "t", Rank: "a"}, {ID: "e1", Rank: "b"}, {ID: "e2", Rank: "c"}, {ID: "t2", Rank: "d"},
		{ID: "e1.y", Rank: "a"}, {ID: "e1.n", Rank: "m"}, {ID: "e1.x", Rank: "z"},
		{ID: "e1.n.q", Rank: "a"}, {ID: "e2.p", Rank: "a"},
	} {
		issue[i.ID] = i
	}
	cases := []struct {
		dependent, dependency model.ID
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
		order := NewDependencyOrder(ancestry, []DependencyEdge{{Dependent: tc.dependent, Dependency: tc.dependency}})
		if got := order.Inverted(dependent, dependency); got != tc.want {
			t.Errorf("Inverted(%s blocked by %s) = %v, want %v", tc.dependent, tc.dependency, got, tc.want)
		}
	}
	// An edge the order was not built from — a closed issue's — is none of its
	// inversions, however tree order lists its two ends.
	if NewDependencyOrder(ancestry, nil).Inverted(issue["e1.y"], issue["e1.x"]) {
		t.Errorf("Inverted(e1.y blocked by e1.x) over an order without that edge = true, want false")
	}
}

func TestFrameRepresentatives(t *testing.T) {
	t.Parallel()
	// wantFrame is asserted alongside the representatives because the frame is
	// what every neighbor lookup seeded by them gets scoped to: representatives
	// that are right in a frame that is wrong still write a key into the wrong
	// keyspace.
	cases := []struct {
		name      string
		chains    [][]model.ID
		want      []model.ID
		wantFrame Frame
		wantErr   bool
	}{
		{name: "all top-level", chains: [][]model.ID{{"x"}, {"y"}, {"z"}}, want: []model.ID{"x", "y", "z"}, wantFrame: TopLevel},
		{name: "a lone top-level issue", chains: [][]model.ID{{"x"}}, want: []model.ID{"x"}, wantFrame: TopLevel},
		{name: "a lone child is its own representative in its epic", chains: [][]model.ID{{"g", "s", "e"}}, want: []model.ID{"g"}, wantFrame: "s"},
		{name: "siblings same epic", chains: [][]model.ID{{"c1", "e"}, {"c2", "e"}, {"c3", "e"}}, want: []model.ID{"c1", "c2", "c3"}, wantFrame: "e"},
		{name: "children plus outsider resolve to roots", chains: [][]model.ID{{"c1", "e"}, {"x"}, {"c2", "e"}}, want: []model.ID{"e", "x", "e"}, wantFrame: TopLevel},
		{name: "children of two epics under shared parent with outsider", chains: [][]model.ID{{"c1", "e1", "p"}, {"c2", "e2", "p"}, {"x"}}, want: []model.ID{"p", "p", "x"}, wantFrame: TopLevel},
		{name: "children of two epics under shared parent", chains: [][]model.ID{{"c1", "e1", "p"}, {"c2", "e2", "p"}}, want: []model.ID{"e1", "e2"}, wantFrame: "p"},
		{name: "grandchild vs child of shared epic", chains: [][]model.ID{{"g", "s", "e"}, {"c", "e"}}, want: []model.ID{"s", "c"}, wantFrame: "e"},
		{name: "issue with its own container", chains: [][]model.ID{{"c", "e"}, {"e"}}, wantErr: true},
		{name: "container with deep descendant", chains: [][]model.ID{{"e"}, {"g", "s", "e"}}, wantErr: true},
		{name: "container alongside outsider and descendant", chains: [][]model.ID{{"e"}, {"x"}, {"c", "e"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reps, frame, err := FrameRepresentatives(tc.chains)
			if tc.wantErr {
				var containment FrameContainmentError
				if !errors.As(err, &containment) {
					t.Fatalf("FrameRepresentatives() error = %v, want FrameContainmentError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FrameRepresentatives() error = %v", err)
			}
			if len(reps) != len(tc.want) {
				t.Fatalf("FrameRepresentatives() = %v, want %v", reps, tc.want)
			}
			for i := range reps {
				if reps[i] != tc.want[i] {
					t.Fatalf("FrameRepresentatives() = %v, want %v", reps, tc.want)
				}
			}
			if frame != tc.wantFrame {
				t.Fatalf("FrameRepresentatives() frame = %q, want %q", frame, tc.wantFrame)
			}
			// Every representative is a member of the frame it came back with,
			// which is the property that makes scoping a query by that frame
			// find exactly the rows these keys are compared against.
			for i, chain := range tc.chains {
				container := TopLevel
				if idx := slices.Index(chain, reps[i]); idx >= 0 && idx+1 < len(chain) {
					container = Frame(chain[idx+1])
				}
				if container != frame {
					t.Fatalf("representative %s sits in frame %q, not the reported %q", reps[i], container, frame)
				}
			}
		})
	}
}

// Edges lift onto the frame-mates tree order decides them by. Where lifted
// edges run both ways the frame-mates form a group, at whatever depth they
// meet, and the edges inside a group neither count as inversions nor constrain
// the repair; every other lifted edge is a constraint, once, and an edge
// between a container and its own descendant is neither. A group is
// interleaved unless a loop of the issues' own edges runs through it, even one
// whose closing edge lifts into a frame below the group.
func TestDependencyOrderGroupsInterleavedFrameMates(t *testing.T) {
	t.Parallel()
	ancestry := NewRankAncestry([]ParentLink{
		{ChildID: "a.1", ParentID: "a", ParentRank: "a"},
		{ChildID: "a.2", ParentID: "a", ParentRank: "a"},
		{ChildID: "b.1", ParentID: "b", ParentRank: "b"},
		{ChildID: "b.2", ParentID: "b", ParentRank: "b"},
		{ChildID: "c.1", ParentID: "c", ParentRank: "c"},
		{ChildID: "c.1.x", ParentID: "c.1", ParentRank: "a"},
		{ChildID: "c.1.y", ParentID: "c.1", ParentRank: "a"},
		{ChildID: "c.2", ParentID: "c", ParentRank: "c"},
		{ChildID: "c.2.x", ParentID: "c.2", ParentRank: "b"},
		{ChildID: "c.3", ParentID: "c", ParentRank: "c"},
		{ChildID: "f.1", ParentID: "f", ParentRank: "f"},
		{ChildID: "f.2", ParentID: "f", ParentRank: "f"},
		{ChildID: "g.1", ParentID: "g", ParentRank: "g"},
		{ChildID: "h.1", ParentID: "h", ParentRank: "h"},
		{ChildID: "h.2", ParentID: "h", ParentRank: "h"},
		{ChildID: "h.3", ParentID: "h", ParentRank: "h"},
		{ChildID: "k.1", ParentID: "k", ParentRank: "k"},
		{ChildID: "k.2", ParentID: "k", ParentRank: "k"},
		{ChildID: "k.3", ParentID: "k", ParentRank: "k"},
	})
	order := NewDependencyOrder(ancestry, []DependencyEdge{
		{Dependent: "a.1", Dependency: "b.1"},     // a waits on b ...
		{Dependent: "b.1", Dependency: "a.2"},     // ... and b on a: {a, b}
		{Dependent: "a.1", Dependency: "b.2"},     // a second edge lifting onto a -> b
		{Dependent: "c.1.x", Dependency: "c.2.x"}, // inside c, c.1 waits on c.2 ...
		{Dependent: "c.2.x", Dependency: "c.3"},   // ... c.2 on c.3 ...
		{Dependent: "c.3", Dependency: "c.1.y"},   // ... and c.3 on c.1: {c.1, c.2, c.3}
		{Dependent: "c.1.x", Dependency: "c.1.y"}, // frame-mates inside c.1, no cycle
		{Dependent: "c", Dependency: "a"},         // top level: c waits on a
		{Dependent: "c.2", Dependency: "c.2.x"},   // a container on its own child
		{Dependent: "f.1", Dependency: "g.1"},     // f waits on g ...
		{Dependent: "g.1", Dependency: "f.2"},     // ... and g on f: {f, g} ...
		{Dependent: "f.2", Dependency: "f.1"},     // ... closed into a loop inside f
		{Dependent: "h.1", Dependency: "k.1"},     // h waits on k ...
		{Dependent: "k.2", Dependency: "h.2"},     // ... and k on h: {h, k}, no loop ...
		{Dependent: "h.1", Dependency: "h.3"},     // ... though h.1 sits on a loop in h
		{Dependent: "h.3", Dependency: "h.1"},
		{Dependent: "k.1", Dependency: "k.3"}, // ... and k.1 on another in k
		{Dependent: "k.3", Dependency: "k.1"},
	})
	want := [][]model.ID{{"a", "b"}, {"c.1", "c.2", "c.3"}, {"h", "k"}}
	if got := order.Interleaved(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("Interleaved() = %v, want %v", got, want)
	}
	wantConstraints := []DependencyEdge{
		{Dependent: "c.1.x", Dependency: "c.1.y"},
		{Dependent: "c", Dependency: "a"},
		{Dependent: "f.2", Dependency: "f.1"},
	}
	if got := order.Constraints(); !slices.Equal(got, wantConstraints) {
		t.Errorf("Constraints() = %v, want %v", got, wantConstraints)
	}
	issue := func(id model.ID, rank string) model.Issue { return model.Issue{ID: id, Rank: rank} }
	// b lists after a, so a.1 waiting on b.1 would read inverted outside the group.
	if order.Inverted(issue("a.1", "a"), issue("b.1", "a")) {
		t.Errorf("Inverted(a.1 blocked by b.1) = true, want false: a and b are interleaved")
	}
	// c.1.y lists after c.1.x, and the two are frame-mates in no group.
	if !order.Inverted(issue("c.1.x", "a"), issue("c.1.y", "b")) {
		t.Errorf("Inverted(c.1.x blocked by c.1.y) = false, want true")
	}
}

// Shape names what building the ancestry placed past, the second parent and the
// loop, and the deepest chain it held; a clean tree names neither.
func TestAnAncestryShapeNamesWhatItPlacedPast(t *testing.T) {
	t.Parallel()
	clean := NewRankAncestry([]ParentLink{
		{ChildID: "e.1", ParentID: "e", ParentRank: "a"},
		{ChildID: "e.1.x", ParentID: "e.1", ParentRank: "b"},
	}).Shape()
	if clean.Deepest != 2 || len(clean.Conflicts) != 0 || len(clean.Loops) != 0 {
		t.Errorf("clean tree shape = %+v, want deepest 2 and nothing placed past", clean)
	}

	shape := NewRankAncestry([]ParentLink{
		{ChildID: "c", ParentID: "p2", ParentRank: "a"},
		{ChildID: "c", ParentID: "p1", ParentRank: "b"},
		{ChildID: "l1", ParentID: "l2", ParentRank: "a"},
		{ChildID: "l2", ParentID: "l1", ParentRank: "b"},
		{ChildID: "x", ParentID: "l1", ParentRank: "c"},
		{ChildID: "y", ParentID: "x", ParentRank: "d"},
	}).Shape()
	if len(shape.Conflicts) != 1 || shape.Conflicts[0].ChildID != "c" || !slices.Equal(shape.Conflicts[0].Parents, []model.ID{"p1", "p2"}) {
		t.Errorf("conflicts = %+v, want c under p1 and p2", shape.Conflicts)
	}
	if !slices.Equal(shape.Loops, []model.ID{"l1", "l2"}) {
		t.Errorf("loops = %v, want [l1 l2]: x and y sit below the loop, not on it", shape.Loops)
	}
}
