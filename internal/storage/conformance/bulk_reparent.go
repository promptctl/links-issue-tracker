package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// aBulkFileReparentsItsUpdateDocuments is the contract behind moving a batch
// of tickets under a new epic with one `lit import`: every update document
// stating a parent lands under it, and one whose parent already is the one it
// states is reported as held rather than refused or counted as moved.
func aBulkFileReparentsItsUpdateDocuments(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	from := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("from"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	to := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("to"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	var specs []storage.BulkIssueSpec
	var moved []model.ID
	for i := range 3 {
		task := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle(fmt.Sprintf("task %d", i)), Lane: storage.SharedLane, Topic: "core", ParentID: from.ID})
		specs = append(specs, storage.BulkIssueSpec{ID: task.ID.Raw(), Parent: ptr(to.ID.Raw())})
		moved = append(moved, task.ID)
	}
	already := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("already"), Lane: storage.SharedLane, Topic: "core", ParentID: to.ID})
	specs = append(specs, storage.BulkIssueSpec{ID: already.ID.Raw(), Parent: ptr(to.ID.Raw())})

	result, err := st.BulkApply(ctx, prefix, "ada", specs)
	if err != nil {
		t.Fatalf("BulkApply error = %v", err)
	}
	assertStrings(t, "reparented", result.Reparented, moved)
	assertStrings(t, "parent already held", result.ParentHeld, []model.ID{already.ID})
	for _, id := range append(moved, already.ID) {
		assertStrings(t, id.Raw()+"'s parents", parentsOf(t, ctx, st, id), []model.ID{to.ID})
	}
}

// aBulkFileStatingAParentCycleWritesNothing: a cycle the file states with its
// own documents — here a create under an update document that the file puts
// under that create — needs no read to refuse, so it is refused before the
// create lands, and no tombstone is left behind. [LAW:single-enforcer]
func aBulkFileStatingAParentCycleWritesNothing(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	x := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("x"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	before := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true})
	title, topic, epicType := "c", "core", "epic"
	_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "c", Title: &title, Topic: &topic, IssueType: &epicType, Parent: ptr(x.ID.Raw())},
		{ID: x.ID.Raw(), Parent: ptr("c")},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("BulkApply error = %v, want the cycle refused", err)
	}
	if after := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true}); len(after) != len(before) {
		t.Errorf("issues, deleted included, = %d after the refusal, want %d: a cycle the file states is refused before any write", len(after), len(before))
	}
	assertStrings(t, "x's parents", parentsOf(t, ctx, st, x.ID), nil)
}

// aRefusedReparentAppliesNoDocument: a cycle through the hierarchy already
// stored is judged by the reparent write, which lands whole or not at all,
// and it comes before the file's ordering and updates, so a file of update
// documents refused there changes nothing — not the reparent beside it, not a
// title, not the order.
func aRefusedReparentAppliesNoDocument(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	a := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("a"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	b := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("b"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic, ParentID: a.ID})
	r := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("r"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	task := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("task"), Lane: storage.SharedLane, Topic: "core", ParentID: a.ID})
	order := []model.ID{a.ID, b.ID, task.ID, r.ID}
	assertOrder(t, ctx, st, "before", order...)
	top, renamed := "top", "renamed"
	_, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{ID: task.ID.Raw(), Parent: ptr(r.ID.Raw()), Title: &renamed},
		{ID: a.ID.Raw(), Parent: ptr(b.ID.Raw())},
		{ID: r.ID.Raw(), Rank: &top},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("BulkApply error = %v, want the stored cycle refused", err)
	}
	assertStrings(t, "the task's parents", parentsOf(t, ctx, st, task.ID), []model.ID{a.ID})
	assertStrings(t, "a's parents", parentsOf(t, ctx, st, a.ID), nil)
	if got := mustGet(t, ctx, st, task.ID).Title; got != "task" {
		t.Errorf("title = %q after a refused reparent, want it unchanged", got)
	}
	assertOrder(t, ctx, st, "after a refused reparent", order...)
}

// aBulkFileIsJudgedOnTheHierarchyItEndsWith: whether a file is accepted is a
// fact about the hierarchy it describes, never about the order its documents
// come in. Swapping an epic with the epic above it, and a move that frees the
// way for another through stored edges alone, apply in either order.
// [LAW:no-ambient-temporal-coupling]
func aBulkFileIsJudgedOnTheHierarchyItEndsWith(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := func(title string, parent model.ID) model.ID {
		return mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle(title), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic, ParentID: parent}).ID
	}
	for _, reversed := range []bool{false, true} {
		apply := func(what string, docs ...storage.BulkIssueSpec) {
			if reversed {
				docs[0], docs[1] = docs[1], docs[0]
			}
			if _, err := st.BulkApply(ctx, prefix, "ada", docs); err != nil {
				t.Fatalf("%s (reversed=%v): BulkApply error = %v", what, reversed, err)
			}
		}
		// B under A, swapped: A under B, B under R.
		a := epic("a", "")
		b := epic("b", a)
		r := epic("r", "")
		apply("swapping an epic with the epic above it", storage.BulkIssueSpec{ID: a.Raw(), Parent: ptr(b.Raw())}, storage.BulkIssueSpec{ID: b.Raw(), Parent: ptr(r.Raw())})
		assertStrings(t, "a's parents", parentsOf(t, ctx, st, a), []model.ID{b})
		assertStrings(t, "b's parents", parentsOf(t, ctx, st, b), []model.ID{r})

		// P under Y under X: X under P, Y under R. No edge in the file orders
		// the two documents; only the stored Y-above-P does.
		x := epic("x", "")
		y := epic("y", x)
		p := epic("p", y)
		r = epic("r", "")
		apply("a move freeing the way through stored edges", storage.BulkIssueSpec{ID: x.Raw(), Parent: ptr(p.Raw())}, storage.BulkIssueSpec{ID: y.Raw(), Parent: ptr(r.Raw())})
		assertStrings(t, "x's parents", parentsOf(t, ctx, st, x), []model.ID{p})
		assertStrings(t, "y's parents", parentsOf(t, ctx, st, y), []model.ID{r})
	}
}

// aBulkFileClearsAParentWithAnEmptyOne: `parent: ""` is `lit parent clear`,
// with its rule — a nested epic is lifted to the top level, a leaf is refused,
// since a leaf lives inside an epic — and a file clearing a leaf beside an
// epic it could lift lifts neither. The rule needs only the leaf's stored
// type, so the refusal comes before any write: a create beside it leaves no
// tombstone.
func aBulkFileClearsAParentWithAnEmptyOne(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	outer := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("outer"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	nested := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("nested"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic, ParentID: outer.ID})
	leaf := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("leaf"), Lane: storage.SharedLane, Topic: "core", ParentID: outer.ID})
	topLevel := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("top level"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})

	before := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true})
	title, topic, epicType := "fresh", "core", "epic"
	if _, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{Title: &title, Topic: &topic, IssueType: &epicType},
		{ID: nested.ID.Raw(), Parent: ptr("")},
		{ID: leaf.ID.Raw(), Parent: ptr("")},
	}); err == nil {
		t.Fatal("BulkApply clearing a leaf's parent succeeded; want it refused")
	}
	if after := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true}); len(after) != len(before) {
		t.Errorf("issues, deleted included, = %d after the refusal, want %d: clearing a leaf is refused before any write", len(after), len(before))
	}
	assertStrings(t, "nested's parents after a refused file", parentsOf(t, ctx, st, nested.ID), []model.ID{outer.ID})

	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{ID: nested.ID.Raw(), Parent: ptr("")},
		{ID: topLevel.ID.Raw(), Parent: ptr("")},
	})
	if err != nil {
		t.Fatalf("BulkApply error = %v", err)
	}
	assertStrings(t, "nested's parents", parentsOf(t, ctx, st, nested.ID), nil)
	assertStrings(t, "reparented", result.Reparented, []model.ID{nested.ID})
	assertStrings(t, "parent already held", result.ParentHeld, []model.ID{topLevel.ID})
}

// aBulkFileRanksIssuesInTheFrameItMovesThemTo: a document may move its issue
// under an epic the same file creates, and its `rank: top` orders it in that
// new frame, because the file's ordering is written after its reparents.
func aBulkFileRanksIssuesInTheFrameItMovesThemTo(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	home := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("home"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	first := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("first"), Lane: storage.SharedLane, Topic: "core", ParentID: home.ID})
	second := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("second"), Lane: storage.SharedLane, Topic: "core", ParentID: home.ID})
	title, topic, epicType, top := "new home", "core", "epic", "top"
	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{LocalID: "new", Title: &title, Topic: &topic, IssueType: &epicType},
		{ID: second.ID.Raw(), Parent: ptr("new"), Rank: &top},
		{ID: first.ID.Raw(), Parent: ptr("new"), Rank: &top},
	})
	if err != nil {
		t.Fatalf("BulkApply error = %v", err)
	}
	created := mustCreatedID(t, result.Created, "new")
	if result.Ranked.Frame != storage.Frame(created) {
		t.Errorf("the ordering landed in frame %q, want the new epic %s", result.Ranked.Frame, created)
	}
	assertIssueIDs(t, "the new epic's children", mustChildren(t, ctx, st, created), []model.ID{second.ID, first.ID})
}

// aBulkFileJudgesItsBlockedByOnTheHierarchyItEndsWith: a create's blocked_by
// naming an issue the same file reparents is judged where the file puts that
// issue, not where it stood. Moved out of the create's epic, the edge is
// legal and lands; moved into it, the two are leaves of one epic, which the
// gate refuses before any write. [LAW:no-ambient-temporal-coupling]
func aBulkFileJudgesItsBlockedByOnTheHierarchyItEndsWith(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	e := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("e"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	f := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("f"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	x := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("x"), Lane: storage.SharedLane, Topic: "core", ParentID: e.ID})
	title, topic, taskType := "t", "core", "task"
	create := func(parent model.ID) storage.BulkIssueSpec {
		return storage.BulkIssueSpec{LocalID: "t", Title: &title, Topic: &topic, IssueType: &taskType, Parent: ptr(parent.Raw()), BlockedBy: ptr(model.RawIDs([]model.ID{x.ID}))}
	}

	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{create(e.ID), {ID: x.ID.Raw(), Parent: ptr(f.ID.Raw())}})
	if err != nil {
		t.Fatalf("moving x out of t's epic: BulkApply error = %v", err)
	}
	assertStrings(t, "x's parents", parentsOf(t, ctx, st, x.ID), []model.ID{f.ID})
	tID := mustCreatedID(t, result.Created, "t")
	rels, err := st.ListRelationsForIssue(ctx, tID, model.RelBlockedBy)
	if err != nil || len(rels) != 1 || rels[0].DstID != x.ID {
		t.Fatalf("t's blocked_by = %v (error %v), want one edge to %s", rels, err, x.ID)
	}

	before := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true})
	if _, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{create(e.ID), {ID: x.ID.Raw(), Parent: ptr(e.ID.Raw())}}); err == nil || !strings.Contains(err.Error(), storage.SameEpicBlocksRejectionMessage) {
		t.Fatalf("moving x into t's epic: BulkApply error = %v, want the same-epic refusal", err)
	}
	if after := mustList(t, ctx, st, storage.ListIssuesFilter{IncludeDeleted: true}); len(after) != len(before) {
		t.Errorf("issues, deleted included, = %d after the refusal, want %d: the file states both places, so it is refused before any write", len(after), len(before))
	}
	assertStrings(t, "x's parents after the refusal", parentsOf(t, ctx, st, x.ID), []model.ID{f.ID})
}

// parentsOf lists the parents childID holds, by the edges it is the child of.
func parentsOf(t *testing.T, ctx context.Context, st storage.Store, childID model.ID) []model.ID {
	t.Helper()
	rels, err := st.ListRelationsForIssue(ctx, childID, model.RelParentChild)
	if err != nil {
		t.Fatalf("ListRelationsForIssue(%s) error = %v", childID, err)
	}
	var parents []model.ID
	for _, rel := range rels {
		if rel.SrcID == childID {
			parents = append(parents, rel.DstID)
		}
	}
	return parents
}
