package storage_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func blockedBy(dependent, dependency string) model.Relation {
	return model.Relation{SrcID: dependent, DstID: dependency, Type: model.RelBlockedBy, CreatedAt: born, CreatedBy: "seed"}
}

// Every expected cut below is worked by hand from the rule — in each loop the
// member with the greatest id loses its edge onto the next member — so a cut
// of the wrong edge, or a loop left standing, fails here.
func TestBreakParentCyclesCutsTheGreatestMembersEdgeInEachLoop(t *testing.T) {
	cases := []struct {
		name      string
		relations []model.Relation
		want      map[string][]string
		cuts      []storage.Unlooped
	}{
		{
			name:      "two epics under each other",
			relations: []model.Relation{under("a", "b"), under("b", "a")},
			want:      map[string][]string{"a": {"b"}},
			cuts:      []storage.Unlooped{{IssueID: "b", Parent: "a", Cycle: []string{"a", "b"}}},
		},
		{
			name:      "a three-member loop",
			relations: []model.Relation{under("a", "b"), under("b", "c"), under("c", "a")},
			want:      map[string][]string{"a": {"b"}, "b": {"c"}},
			cuts:      []storage.Unlooped{{IssueID: "c", Parent: "a", Cycle: []string{"a", "b", "c"}}},
		},
		{
			// a hangs below a b<->c loop, so the loop does not begin where the
			// walk does; a's own edge is not part of it and stays.
			name:      "a loop entered from a tail",
			relations: []model.Relation{under("a", "b"), under("b", "c"), under("c", "b")},
			want:      map[string][]string{"a": {"b"}, "b": {"c"}},
			cuts:      []storage.Unlooped{{IssueID: "c", Parent: "b", Cycle: []string{"b", "c"}}},
		},
		{
			name:      "an issue under itself",
			relations: []model.Relation{under("e", "e")},
			want:      map[string][]string{},
			cuts:      []storage.Unlooped{{IssueID: "e", Parent: "e", Cycle: []string{"e"}}},
		},
		{
			// x holds two parents, root and y; only the edge back from y closes a
			// loop, so x keeps both of its own.
			name:      "a child with two parents, one of them in a loop",
			relations: []model.Relation{under("x", "root"), under("x", "y"), under("y", "x")},
			want:      map[string][]string{"x": {"root", "y"}},
			cuts:      []storage.Unlooped{{IssueID: "y", Parent: "x", Cycle: []string{"x", "y"}}},
		},
		{
			name:      "two separate loops",
			relations: []model.Relation{under("p", "q"), under("q", "p"), under("r", "s"), under("s", "r")},
			want:      map[string][]string{"p": {"q"}, "r": {"s"}},
			cuts: []storage.Unlooped{
				{IssueID: "q", Parent: "p", Cycle: []string{"p", "q"}},
				{IssueID: "s", Parent: "r", Cycle: []string{"r", "s"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A blocked-by edge that runs the same way as a cut one is not a parent
			// edge, so it survives every cut.
			in := model.Export{Relations: append(tc.relations, blockedBy(tc.cuts[0].IssueID, tc.cuts[0].Parent))}
			out, cuts := storage.BreakParentCycles(in)
			if got := parents(out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parent edges = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(cuts, tc.cuts) {
				t.Errorf("cuts = %+v, want %+v", cuts, tc.cuts)
			}
			if !slices.Contains(out.Relations, blockedBy(tc.cuts[0].IssueID, tc.cuts[0].Parent)) {
				t.Errorf("relations = %+v, want the blocked-by edge kept", out.Relations)
			}
		})
	}
}

func TestBreakParentCyclesLeavesAForestAlone(t *testing.T) {
	in := model.Export{
		Issues:    []model.Issue{epic("e"), epic("f")},
		Relations: []model.Relation{under("f", "e"), under("t", "f"), blockedBy("t", "e")},
	}
	out, cuts := storage.BreakParentCycles(in)
	if cuts != nil {
		t.Errorf("cuts = %+v on a forest, want none", cuts)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("export changed on a forest:\n got  %+v\n want %+v", out, in)
	}
}

// Two clones that each moved a different ticket under the other merge into a
// loop: each edge was legal where it was written, so no write could refuse it.
// The landing every merged export goes through is what cuts it.
func TestALoopTwoClonesMergeIntoIsCutOnLanding(t *testing.T) {
	issues := []model.Issue{leaf(t, "a", "M"), leaf(t, "b", "N")}
	base := model.Export{WorkspaceID: "wsA", Issues: issues}
	local := model.Export{WorkspaceID: "wsA", Issues: issues, Relations: []model.Relation{under("a", "b")}}
	remote := model.Export{WorkspaceID: "wsB", Issues: issues, Relations: []model.Relation{under("b", "a")}}
	merged, ok := merge.ThreeWay(base, local, remote).Provisional()
	if !ok {
		t.Fatal("ThreeWay() held no provisional export")
	}
	out, cuts := storage.BreakParentCycles(merged)
	if got, want := parents(out), map[string][]string{"a": {"b"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("parent edges = %v, want %v", got, want)
	}
	if want := []storage.Unlooped{{IssueID: "b", Parent: "a", Cycle: []string{"a", "b"}}}; !reflect.DeepEqual(cuts, want) {
		t.Errorf("cuts = %+v, want %+v", cuts, want)
	}
}
