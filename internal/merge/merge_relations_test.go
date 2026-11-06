package merge

import (
	"sort"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func edge(src, dst string, typ model.RelationType) model.Relation {
	return model.Relation{SrcID: src, DstID: dst, Type: typ, CreatedAt: t1}
}

// mergedEdges runs the three relation sets through ThreeWay over a fixed set of
// tickets and returns what survived as "src>dst:type", sorted.
func mergedEdges(t *testing.T, base, local, remote []model.Relation) []string {
	t.Helper()
	issues := []model.Issue{open(t, "a"), open(t, "b"), open(t, "c"), open(t, "c1"), open(t, "p1"), open(t, "p2")}
	export := func(ws string, relations []model.Relation) model.Export {
		return model.Export{WorkspaceID: ws, Issues: issues, Relations: relations}
	}
	got := ThreeWay(export("wsA", base), export("wsA", local), export("wsB", remote))
	out := []string{}
	for _, relation := range provisional(t, got).Relations {
		out = append(out, relation.SrcID+">"+relation.DstID+":"+string(relation.Type))
	}
	sort.Strings(out)
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A relation one side removed and the other left alone is removed. A removal is
// a change off base exactly as an addition is, so it is not put back by the side
// that merely kept the row. [LAW:one-type-per-behavior] relations and labels
// decide membership the same way.
func TestThreeWayRelationRemovalIsNotResurrected(t *testing.T) {
	blocked := edge("a", "b", model.RelBlockedBy)
	related := edge("a", "c", model.RelRelatedTo)
	sibling := edge("a", "b", model.RelRelatedTo)
	tests := []struct {
		name                string
		base, local, remote []model.Relation
		want                []string
	}{
		{"local removes, remote unchanged", []model.Relation{blocked}, nil, []model.Relation{blocked}, []string{}},
		{"remote removes, local unchanged", []model.Relation{blocked}, []model.Relation{blocked}, nil, []string{}},
		{"both remove", []model.Relation{blocked}, nil, nil, []string{}},
		{"neither touches", []model.Relation{blocked}, []model.Relation{blocked}, []model.Relation{blocked}, []string{"a>b:blocked-by"}},
		{"local adds, remote unchanged", nil, []model.Relation{blocked}, nil, []string{"a>b:blocked-by"}},
		{"remote adds, local unchanged", nil, nil, []model.Relation{blocked}, []string{"a>b:blocked-by"}},
		{"both add the same", nil, []model.Relation{blocked}, []model.Relation{blocked}, []string{"a>b:blocked-by"}},
		{"both add different", nil, []model.Relation{blocked}, []model.Relation{related}, []string{"a>b:blocked-by", "a>c:related-to"}},
		{"local removes one, remote adds another", []model.Relation{blocked}, nil, []model.Relation{blocked, related}, []string{"a>c:related-to"}},
		{"removal is per type", []model.Relation{blocked, sibling}, []model.Relation{sibling}, []model.Relation{blocked, sibling}, []string{"a>b:related-to"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergedEdges(t, tt.base, tt.local, tt.remote); !equalStrings(got, tt.want) {
				t.Fatalf("relations = %v, want %v", got, tt.want)
			}
		})
	}
}

// A reparent drops child>old and adds child>new on one side; the other side's
// untouched child>old must not outvote it. Both parent orderings run, because
// the single-parent tiebreak prefers the larger parent id: a fix that only
// worked when the tiebreak happened to agree would pass one ordering.
func TestThreeWayReparentOnOneSideIsNotRevertedByTheOther(t *testing.T) {
	for _, pair := range []struct{ old, new string }{{"p1", "p2"}, {"p2", "p1"}} {
		oldEdge, newEdge := edge("c1", pair.old, model.RelParentChild), edge("c1", pair.new, model.RelParentChild)
		want := []string{"c1>" + pair.new + ":parent-child"}
		t.Run("local reparents "+pair.old+" to "+pair.new, func(t *testing.T) {
			if got := mergedEdges(t, []model.Relation{oldEdge}, []model.Relation{newEdge}, []model.Relation{oldEdge}); !equalStrings(got, want) {
				t.Fatalf("relations = %v, want %v", got, want)
			}
		})
		t.Run("remote reparents "+pair.old+" to "+pair.new, func(t *testing.T) {
			if got := mergedEdges(t, []model.Relation{oldEdge}, []model.Relation{oldEdge}, []model.Relation{newEdge}); !equalStrings(got, want) {
				t.Fatalf("relations = %v, want %v", got, want)
			}
		})
	}
}

// Two sides that reparent one child to different parents still leave it exactly
// one parent, and never the one both of them left.
func TestThreeWayConcurrentReparentLeavesOneNewParent(t *testing.T) {
	old := edge("c1", "p1", model.RelParentChild)
	got := mergedEdges(t, []model.Relation{old},
		[]model.Relation{edge("c1", "p2", model.RelParentChild)},
		[]model.Relation{edge("c1", "c", model.RelParentChild)})
	if len(got) != 1 || got[0] == "c1>p1:parent-child" {
		t.Fatalf("relations = %v, want exactly one parent, not the abandoned p1", got)
	}
}

// collidedFixture is one id, epic.16, minted independently on both sides (two
// birth instants, two titles), with the base holding the edges and label that
// our side's ticket also holds, as a re-minted id leaves them. Only ours is the
// surviving row, so remote's child rows for epic.16 are out of scope.
type collidedFixture struct {
	base, local, remote model.Export
}

func collided(t *testing.T) collidedFixture {
	t.Helper()
	born := func(hour int) time.Time { return time.Date(2026, 8, 27, hour, 0, 0, 0, time.UTC) }
	sixteen := func(hour int, title string) model.Issue {
		return leaf(t, "epic.16", model.StatusView{Value: model.StateOpen}, func(i *model.Issue) {
			i.Title = model.Text(title)
			i.CreatedAt = born(hour)
		})
	}
	endpoints := []model.Issue{open(t, "epic.a"), open(t, "epic.b")}
	ownedEdge := model.Relation{SrcID: "epic.16", DstID: "epic.a", Type: model.RelBlockedBy, CreatedAt: born(9)}
	inbound := model.Relation{SrcID: "epic.b", DstID: "epic.16", Type: model.RelBlockedBy, CreatedAt: born(9)}
	ownedLabel := model.Label{IssueID: "epic.16", Name: "ours", CreatedAt: born(9)}
	return collidedFixture{
		base: model.Export{WorkspaceID: "wsA", Issues: endpoints, Relations: []model.Relation{ownedEdge, inbound}, Labels: []model.Label{ownedLabel}},
		local: model.Export{
			WorkspaceID: "wsA", Issues: append([]model.Issue{sixteen(9, "Wire the release watchdog")}, endpoints...),
			Relations: []model.Relation{ownedEdge, inbound}, Labels: []model.Label{ownedLabel},
		},
		remote: model.Export{
			WorkspaceID: "wsB", Issues: append([]model.Issue{sixteen(16, "Adaptive id length")}, endpoints...),
			Relations: []model.Relation{{SrcID: "epic.16", DstID: "epic.b", Type: model.RelBlockedBy, CreatedAt: born(16)}},
			Labels:    []model.Label{{IssueID: "epic.16", Name: "theirs", CreatedAt: born(16)}},
		},
	}
}

// On a colliding id the surviving row is ours alone, so remote's rows for it are
// out of scope. Out of scope is "no opinion", not "removed": a remote that was
// filtered out must not read as having dropped a relation or label ours still
// holds, and that includes an edge to the colliding id from an ordinary ticket,
// which only ours describes.
func TestThreeWayCollisionDoesNotReadTheFilteredSideAsRemovingChildRows(t *testing.T) {
	f := collided(t)
	got := ThreeWay(f.base, f.local, f.remote)
	if len(got.Collisions) != 1 {
		t.Fatalf("collisions = %d, want 1; this fixture's premise is a collided id", len(got.Collisions))
	}
	export, _ := got.Provisional()
	edges := []string{}
	for _, relation := range export.Relations {
		edges = append(edges, relation.SrcID+">"+relation.DstID)
	}
	if want := []string{"epic.16>epic.a", "epic.b>epic.16"}; !equalStrings(edges, want) {
		t.Errorf("relations = %v, want %v", edges, want)
	}
	if len(export.Labels) != 1 || export.Labels[0].Name != "ours" {
		t.Errorf("labels = %+v, want only our label", export.Labels)
	}
}

// One side removed an issue with no purge record and the other edited it. The
// edit outranks the removal, so the ticket survives, and it survives with the
// edited side's relations and labels: the side that dropped the row has no say
// over rows that describe a ticket it no longer holds.
func TestThreeWayEditedIssueKeepsItsChildRowsWhenTheOtherSideDroppedIt(t *testing.T) {
	x := open(t, "x")
	y := open(t, "y")
	edited := x
	edited.Title = "x, edited"
	dep := model.Relation{SrcID: "x", DstID: "y", Type: model.RelBlockedBy, CreatedAt: t1}
	parent := model.Relation{SrcID: "x", DstID: "y", Type: model.RelParentChild, CreatedAt: t1}
	label := model.Label{IssueID: "x", Name: "keep", CreatedAt: t1}
	base := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{x, y}, Relations: []model.Relation{dep, parent}, Labels: []model.Label{label}}
	dropped := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{y}}
	kept := model.Export{WorkspaceID: "wsB", Issues: []model.Issue{edited, y}, Relations: []model.Relation{dep, parent}, Labels: []model.Label{label}}

	for name, sides := range map[string][2]model.Export{"local dropped": {dropped, kept}, "remote dropped": {kept, dropped}} {
		t.Run(name, func(t *testing.T) {
			export := provisional(t, ThreeWay(base, sides[0], sides[1]))
			if len(export.Issues) != 2 || len(export.Relations) != 2 || len(export.Labels) != 1 {
				t.Fatalf("issues=%d relations=%d labels=%d, want the edited ticket with its 2 relations and 1 label", len(export.Issues), len(export.Relations), len(export.Labels))
			}
		})
	}
}

// Each endpoint of a relation is held by a different side: one side dropped x and
// edited y, the other dropped y and edited x. Both tickets survive, and neither
// side's export can hold an edge between them, so no side describes it and the
// merge keeps none.
func TestThreeWayRelationBetweenTicketsHeldByDifferentSidesHasNoDescriber(t *testing.T) {
	x := open(t, "x")
	y := open(t, "y")
	editedX, editedY := x, y
	editedX.Title = "x, edited"
	editedY.Title = "y, edited"
	dep := model.Relation{SrcID: "x", DstID: "y", Type: model.RelBlockedBy, CreatedAt: t1}
	base := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{x, y}, Relations: []model.Relation{dep}}
	local := model.Export{WorkspaceID: "wsA", Issues: []model.Issue{editedX}}
	remote := model.Export{WorkspaceID: "wsB", Issues: []model.Issue{editedY}}

	export := provisional(t, ThreeWay(base, local, remote))
	if len(export.Issues) != 2 || len(export.Relations) != 0 {
		t.Fatalf("issues=%d relations=%+v, want both tickets and no edge", len(export.Issues), export.Relations)
	}
}
