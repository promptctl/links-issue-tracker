package storage_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

var born = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// The ids migration 00007 mints for a store whose top-level ids mostly carry
// "acme", and for one with no readable prefix — pinned as literals, the same
// two migration_containment_test.go pins, so the mint here and the mint there
// answer to one contract.
const (
	acmeUnfiled     = "acme-unfiled-0bf7"
	fallbackUnfiled = "lit-unfiled-e2be"
)

func leaf(t *testing.T, id, rank string) model.Issue {
	t.Helper()
	issue, err := model.HydrateStatus(model.Issue{ID: id, Title: id, IssueType: model.TypeTask, Topic: "core", Rank: rank, CreatedAt: born, UpdatedAt: born}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus(%s): %v", id, err)
	}
	return issue
}

func epic(id string) model.Issue {
	return model.Issue{ID: id, Title: id, IssueType: model.TypeEpic, Topic: "core", CreatedAt: born, UpdatedAt: born}
}

func under(child, parent string) model.Relation {
	return model.Relation{SrcID: child, DstID: parent, Type: model.RelParentChild, CreatedAt: born, CreatedBy: "seed"}
}

// parents is every parent-child edge as child -> sorted parents.
func parents(export model.Export) map[string][]string {
	out := map[string][]string{}
	for _, r := range export.Relations {
		if r.Type == model.RelParentChild {
			out[r.SrcID] = append(out[r.SrcID], r.DstID)
		}
	}
	for _, ps := range out {
		slices.Sort(ps)
	}
	return out
}

func issueOf(t *testing.T, export model.Export, id string) model.Issue {
	t.Helper()
	for _, issue := range export.Issues {
		if issue.ID == id {
			return issue
		}
	}
	t.Fatalf("export holds no issue %s", id)
	return model.Issue{}
}

// assertContained states the rule over a whole export: every leaf has a parent
// edge, and every parent edge lands on an epic.
func assertContained(t *testing.T, export model.Export) {
	t.Helper()
	typeOf := map[string]model.IssueType{}
	for _, issue := range export.Issues {
		typeOf[issue.ID] = issue.IssueType
	}
	ps := parents(export)
	for _, issue := range export.Issues {
		if !issue.IsContainer() && len(ps[issue.ID]) == 0 {
			t.Errorf("%s (%s) is outside every epic", issue.ID, issue.IssueType)
		}
		for _, p := range ps[issue.ID] {
			if !typeOf[p].IsContainer() {
				t.Errorf("%s sits under %s, which is a %q, not an epic", issue.ID, p, typeOf[p])
			}
		}
	}
}

func TestHomeStrandedLeavesAContainedExportAlone(t *testing.T) {
	in := model.Export{
		Issues:    []model.Issue{epic("acme-core-aa11"), leaf(t, "acme-core-aa11.1", "M")},
		Relations: []model.Relation{under("acme-core-aa11.1", "acme-core-aa11")},
	}
	out, rehomed := storage.HomeStranded(in)
	if rehomed != nil {
		t.Errorf("rehomed = %+v, want nothing named", rehomed)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("export changed:\n got %+v\nwant %+v", out, in)
	}
}

// A leaf with no parent goes under the store's unfiled epic, minted as 00007
// mints it: its id from the prefix most top-level ids carry, its row fixed, and
// its place one key after the highest-ranked live top-level leaf. The leaf
// becomes its own lane and its edge carries its own birth, by "links".
func TestHomeStrandedFilesALooseLeafUnderAMintedUnfiledEpic(t *testing.T) {
	gone := leaf(t, "acme-core-dd44", "A")
	gone.SetRetention(model.Deleted{At: born})
	in := model.Export{
		Issues: []model.Issue{
			epic("acme-core-aa11"),
			leaf(t, "acme-core-bb22", "M"),
			leaf(t, "acme-core-cc33", "K"),
			gone,
			epic("demo-core-ee55"),
		},
	}
	out, rehomed := storage.HomeStranded(in)

	want := []storage.Rehomed{
		{IssueID: "acme-core-bb22", Epic: acmeUnfiled},
		{IssueID: "acme-core-cc33", Epic: acmeUnfiled},
		{IssueID: "acme-core-dd44", Epic: acmeUnfiled},
	}
	if !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v, want %+v", rehomed, want)
	}
	assertContained(t, out)
	minted := issueOf(t, out, acmeUnfiled)
	if minted.Title != "Unfiled tickets" || minted.Topic != "unfiled" || minted.IssueType != model.TypeEpic {
		t.Errorf("minted epic = %q/%q/%q, want the unfiled epic", minted.Title, minted.Topic, minted.IssueType)
	}
	if fixed := time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC); !minted.CreatedAt.Equal(fixed) || !minted.UpdatedAt.Equal(fixed) {
		t.Errorf("minted epic born %s, updated %s; want both %s, the instant every checkout mints", minted.CreatedAt, minted.UpdatedAt, fixed)
	}
	// "K" is the highest-ranked live top-level leaf; the deleted "A" does not
	// place the epic.
	if minted.Rank != "K0" {
		t.Errorf("minted epic rank = %q, want %q", minted.Rank, "K0")
	}
	for _, id := range []string{"acme-core-bb22", "acme-core-cc33", "acme-core-dd44"} {
		if got := issueOf(t, out, id).Lane; got != id {
			t.Errorf("%s lane = %q, want its own id", id, got)
		}
	}
	for _, r := range out.Relations {
		if r.SrcID == "acme-core-bb22" && (!r.CreatedAt.Equal(born) || r.CreatedBy != "links") {
			t.Errorf("new edge stamped %s by %q, want the child's birth %s by %q", r.CreatedAt, r.CreatedBy, born, "links")
		}
	}
	// The input export is untouched.
	if len(in.Relations) != 0 || issueOf(t, in, "acme-core-bb22").Lane != "" {
		t.Errorf("HomeStranded mutated its input")
	}
}

func TestHomeStrandedReusesTheUnfiledEpicTheExportHolds(t *testing.T) {
	held := epic("other-unfiled-zz99")
	held.Title, held.Topic, held.Rank = "Unfiled tickets", "unfiled", "Q"
	in := model.Export{Issues: []model.Issue{epic("acme-core-aa11"), held, leaf(t, "acme-core-bb22", "M")}}
	out, rehomed := storage.HomeStranded(in)
	if want := []storage.Rehomed{{IssueID: "acme-core-bb22", Epic: "other-unfiled-zz99"}}; !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v, want %+v", rehomed, want)
	}
	if len(out.Issues) != len(in.Issues) {
		t.Errorf("issues = %d, want %d: no second unfiled epic", len(out.Issues), len(in.Issues))
	}
	if got := issueOf(t, out, "other-unfiled-zz99").Rank; got != "Q" {
		t.Errorf("held epic rank = %q, want %q: a held epic is never rewritten", got, "Q")
	}
}

func TestHomeStrandedMintsUnderTheFallbackPrefix(t *testing.T) {
	out, rehomed := storage.HomeStranded(model.Export{Issues: []model.Issue{leaf(t, "t-loose", "M")}})
	if want := []storage.Rehomed{{IssueID: "t-loose", Epic: fallbackUnfiled}}; !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v, want %+v", rehomed, want)
	}
	assertContained(t, out)
}

// A child whose parent is not an epic climbs to the nearest epic above it, the
// lowest id among epics at that depth; with no epic above, a leaf goes unfiled
// and an epic goes to the top level. Two epics one step up — twin's — resolve
// to the lower id. A loop of leaves ends. A ticket already
// inside an epic keeps it and only loses a second edge onto a leaf.
func TestHomeStrandedClimbsToTheNearestEpic(t *testing.T) {
	in := model.Export{
		Issues: []model.Issue{
			epic("acme-core-e01"), epic("acme-core-e02"), epic("acme-core-e03"),
			leaf(t, "mid", "M"), leaf(t, "low", "M"), leaf(t, "fork", "M"),
			leaf(t, "orphan-host", "M"), leaf(t, "orphan", "M"),
			epic("acme-core-stray"),
			leaf(t, "loop-a", "M"), leaf(t, "loop-b", "M"),
			leaf(t, "home", "M"),
			leaf(t, "twin", "M"),
		},
		Relations: []model.Relation{
			under("mid", "acme-core-e01"), under("low", "mid"),
			under("fork", "low"), under("fork", "mid"),
			under("orphan", "orphan-host"),
			under("acme-core-stray", "orphan-host"),
			under("loop-a", "loop-b"), under("loop-b", "loop-a"),
			under("home", "acme-core-e03"), under("home", "mid"),
			under("orphan-host", "acme-core-e02"),
			under("twin", "orphan-host"), under("twin", "mid"),
		},
	}
	out, rehomed := storage.HomeStranded(in)
	want := []storage.Rehomed{
		{IssueID: "acme-core-stray", Epic: "acme-core-e02"},
		{IssueID: "fork", Epic: "acme-core-e01"},
		{IssueID: "home", Epic: "acme-core-e03"},
		{IssueID: "loop-a", Epic: acmeUnfiled},
		{IssueID: "loop-b", Epic: acmeUnfiled},
		{IssueID: "low", Epic: "acme-core-e01"},
		{IssueID: "orphan", Epic: "acme-core-e02"},
		{IssueID: "twin", Epic: "acme-core-e01"},
	}
	if !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v\nwant %+v", rehomed, want)
	}
	assertContained(t, out)
	got := parents(out)
	for child, want := range map[string][]string{"low": {"acme-core-e01"}, "fork": {"acme-core-e01"}, "home": {"acme-core-e03"}, "mid": {"acme-core-e01"}} {
		if !reflect.DeepEqual(got[child], want) {
			t.Errorf("%s parents = %v, want %v", child, got[child], want)
		}
	}
	if lane := issueOf(t, out, "home").Lane; lane != "" {
		t.Errorf("home lane = %q, want it left alone: it never left its epic", lane)
	}
	if lane := issueOf(t, out, "low").Lane; lane != "low" {
		t.Errorf("low lane = %q, want its own id", lane)
	}
}

func TestHomeStrandedSendsAnEpicWithNoEpicAboveToTheTopLevel(t *testing.T) {
	in := model.Export{
		Issues:    []model.Issue{epic("acme-core-e01"), leaf(t, "host", "M"), epic("acme-core-sub")},
		Relations: []model.Relation{under("acme-core-sub", "host")},
	}
	out, rehomed := storage.HomeStranded(in)
	want := []storage.Rehomed{{IssueID: "acme-core-sub", Epic: ""}, {IssueID: "host", Epic: acmeUnfiled}}
	if !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v, want %+v", rehomed, want)
	}
	if ps := parents(out)["acme-core-sub"]; len(ps) != 0 {
		t.Errorf("acme-core-sub parents = %v, want none: an epic may stand at the top level", ps)
	}
	assertContained(t, out)
}

// Route 1 of the ticket: the merge breaks a parent cycle by deleting one edge,
// and the child it cuts loose is left with no parent. Every parent is an epic,
// so every member of a cycle is one, and the one cut loose stands at the top
// level, where an epic may: nothing is stranded and nothing moves.
func TestAMergeThatBreaksAParentCycleStrandsNothing(t *testing.T) {
	base := model.Export{Issues: []model.Issue{epic("acme-core-x0a"), epic("acme-core-y0a")}}
	local, remote := base, base
	local.Relations = []model.Relation{under("acme-core-x0a", "acme-core-y0a")}
	remote.Relations = []model.Relation{under("acme-core-y0a", "acme-core-x0a")}
	merged, ok := merge.ThreeWay(base, local, remote).Settled()
	if !ok {
		t.Fatalf("merge did not settle")
	}
	if len(parents(merged)) != 1 {
		t.Fatalf("merged parent edges = %v, want the cycle broken to one edge", parents(merged))
	}
	out, rehomed := storage.HomeStranded(merged)
	if len(rehomed) != 0 {
		t.Errorf("rehomed = %+v, want nothing: the epic cut loose may stand at the top level", rehomed)
	}
	assertContained(t, out)
}

// Route 2 of the ticket: one side holds an epic retyped to a task — a shape the
// write boundary refuses (fields.go), which only restored data can carry —
// while the other files a child under it. The merge keeps the retype, so the
// child sits under a task, and the task itself sits at the top level; both go
// under the unfiled epic.
func TestAMergeThatRetypesAParentHomesItsChild(t *testing.T) {
	baseEpic := epic("acme-core-e01")
	retyped := leaf(t, "acme-core-e01", "")
	base := model.Export{Issues: []model.Issue{baseEpic}}
	local := model.Export{Issues: []model.Issue{retyped}}
	remote := model.Export{
		Issues:    []model.Issue{baseEpic, leaf(t, "acme-core-e01.1", "M")},
		Relations: []model.Relation{under("acme-core-e01.1", "acme-core-e01")},
	}
	merged, ok := merge.ThreeWay(base, local, remote).Settled()
	if !ok {
		t.Fatalf("merge did not settle")
	}
	if got := issueOf(t, merged, "acme-core-e01").IssueType; got != model.TypeTask {
		t.Fatalf("merged type = %q, want the retype kept, so the child sits under a task", got)
	}
	out, rehomed := storage.HomeStranded(merged)
	want := []storage.Rehomed{{IssueID: "acme-core-e01", Epic: acmeUnfiled}, {IssueID: "acme-core-e01.1", Epic: acmeUnfiled}}
	if !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v, want %+v", rehomed, want)
	}
	assertContained(t, out)
}

// Route 3 at the merge: a purge on one side drops the epic a child on the other
// side was filed under, and the merge applies the purge to every side, cutting
// the child's edge. The end-to-end sync is
// TestSyncReconcileHomesAChildOfAnEpicPurgedOnTheOtherSide.
func TestAMergeThatPurgesAParentHomesItsChild(t *testing.T) {
	purge := model.Purge{IssueID: "acme-core-e01", IssueCreatedAt: born, PurgedAt: born, PurgedBy: "grace"}
	base := model.Export{Issues: []model.Issue{epic("acme-core-e01"), epic("acme-core-e02")}}
	local := model.Export{Issues: []model.Issue{epic("acme-core-e02")}, Purges: []model.Purge{purge}}
	remote := model.Export{
		Issues:    []model.Issue{epic("acme-core-e01"), epic("acme-core-e02"), leaf(t, "acme-core-e01.1", "M")},
		Relations: []model.Relation{under("acme-core-e01.1", "acme-core-e01")},
	}
	merged, ok := merge.ThreeWay(base, local, remote).Settled()
	if !ok {
		t.Fatalf("merge did not settle")
	}
	out, rehomed := storage.HomeStranded(merged)
	if want := []storage.Rehomed{{IssueID: "acme-core-e01.1", Epic: acmeUnfiled}}; !reflect.DeepEqual(rehomed, want) {
		t.Errorf("rehomed = %+v, want %+v", rehomed, want)
	}
	assertContained(t, out)
}
