package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// parentChain builds issues linked child -> parent in the order given, so
// chain[0] is the root and each later element is a child of the one before it.
// It returns the issues root-first.
func parentChain(t *testing.T, ctx context.Context, st *Store, titles ...string) []model.Issue {
	t.Helper()
	out := make([]model.Issue, 0, len(titles))
	for _, title := range titles {
		in := storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "hier", IssueType: "epic", Placement: storage.RankBottom}
		if len(out) > 0 {
			in.ParentID = out[len(out)-1].ID
		}
		issue, err := st.CreateIssue(ctx, in)
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		out = append(out, issue)
	}
	return out
}

// parentOf reads the stored parent id of each issue, so a refused write can be
// checked against the tree it claimed to leave alone. An issue with no parent
// maps to "".
func parentOf(t *testing.T, ctx context.Context, st *Store, issues []model.Issue) map[string]string {
	t.Helper()
	out := make(map[string]string, len(issues))
	for _, issue := range issues {
		rels, err := st.ListRelationsForIssue(ctx, issue.ID, model.RelParentChild)
		if err != nil {
			t.Fatalf("ListRelationsForIssue(%s) error = %v", issue.ID, err)
		}
		out[issue.ID] = ""
		for _, rel := range rels {
			if rel.SrcID == issue.ID {
				out[issue.ID] = rel.DstID
			}
		}
	}
	return out
}

func sameParents(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, parent := range a {
		if b[id] != parent {
			return false
		}
	}
	return true
}

// A hierarchy is a tree: every issue reaches a root by walking up, and that is
// what every consumer walking the parent chain relies on. A cycle has no root,
// so it is incoherent state rather than an unusual shape — these cases pin the
// refusal at each write path, and that the refused write changes nothing.
//
// The assertion is on the STORED parent edges, not on the error alone: a write
// that reported an error and still landed would satisfy an error-only check.
func TestSetParentRefusesACycle(t *testing.T) {
	cases := []struct {
		name string
		// depth of the chain to build, root first
		chain []string
		// which chain index becomes the child, and which the parent
		child, parent int
	}{
		{name: "direct: parent under its own child", chain: []string{"E", "S"}, child: 0, parent: 1},
		{name: "transitive: root under its grandchild", chain: []string{"E", "S", "L"}, child: 0, parent: 2},
		{name: "middle: sub-epic under its own child", chain: []string{"E", "S", "L"}, child: 1, parent: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openIssueStore(t, ctx)
			chain := parentChain(t, ctx, st, tc.chain...)
			before := parentOf(t, ctx, st, chain)

			_, err := st.SetParent(ctx, storage.SetParentInput{ChildID: chain[tc.child].ID, ParentID: chain[tc.parent].ID})
			if err == nil {
				t.Fatalf("SetParent(child=%s, parent=%s) succeeded; it closes a parent cycle", chain[tc.child].ID, chain[tc.parent].ID)
			}
			if after := parentOf(t, ctx, st, chain); !sameParents(before, after) {
				t.Errorf("refused SetParent still changed the tree:\n before=%v\n after =%v", before, after)
			}
		})
	}
}

// The dep-add form reaches the same edge by another door. It must refuse on the
// same terms, or the rule is only as strong as which command the caller typed.
func TestAddRelationRefusesAParentCycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "S")
	before := parentOf(t, ctx, st, chain)

	_, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: chain[0].ID, DstID: chain[1].ID, Type: model.RelParentChild})
	if err == nil {
		t.Fatalf("AddRelation(parent-child %s->%s) succeeded; it closes a parent cycle", chain[0].ID, chain[1].ID)
	}
	if after := parentOf(t, ctx, st, chain); !sameParents(before, after) {
		t.Errorf("refused AddRelation still changed the tree:\n before=%v\n after =%v", before, after)
	}
}

// A parent that is not an epic is the case the earlier fix missed: the wait
// graph carries no edge there, so a rule derived from waits cannot see it,
// while hydration walks the parent chain regardless.
func TestSetParentRefusesACycleThroughANonEpicParent(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "E", Topic: "hier", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	task, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "T", Topic: "hier", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(task) error = %v", err)
	}
	chain := []model.Issue{epic, task}
	before := parentOf(t, ctx, st, chain)

	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: epic.ID, ParentID: task.ID}); err == nil {
		t.Fatalf("SetParent(child=%s, parent=%s) succeeded; it closes a parent cycle through a non-epic", epic.ID, task.ID)
	}
	if after := parentOf(t, ctx, st, chain); !sameParents(before, after) {
		t.Errorf("refused SetParent still changed the tree:\n before=%v\n after =%v", before, after)
	}
}

// The write boundary cannot refuse a cycle that was already stored — data
// written before the rule, or restored from an export, can still hold one, and
// every walk up the parent chain runs forever on it. Doctor is where that state
// is named, so the operator learns it from a report rather than from a crash.
func TestDoctorNamesAStoredParentCycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "S")

	// A clean hierarchy reports no cycle — without this the case cannot tell a
	// working detector from one that reports a cycle unconditionally.
	clean, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(clean.ParentCycle) != 0 {
		t.Fatalf("Doctor().ParentCycle = %v on a tree, want empty", clean.ParentCycle)
	}

	// Close the loop directly, simulating data that entered through a path that
	// does not run the write boundary's guard.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, 'import')`,
		chain[0].ID, chain[1].ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed cyclic parent edge error = %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(report.ParentCycle) == 0 {
		t.Fatal("Doctor().ParentCycle is empty, want the members of the loop")
	}
	for _, want := range []string{chain[0].ID, chain[1].ID} {
		if !slices.Contains(report.ParentCycle, want) {
			t.Errorf("Doctor().ParentCycle = %v, want it to name %s", report.ParentCycle, want)
		}
	}
	// The loop is an error, not a warning: nothing that walks the hierarchy can
	// run until it is broken.
	if len(report.Errors) == 0 {
		t.Error("Doctor().Errors is empty, want the parent cycle reported as an error")
	}
}

// parentCycle is the pure half of the detector, so its shapes are pinned
// directly: a forest reports nothing, and a loop reports only its own members —
// never the tail that merely leads into it.
func TestParentCycleFindsOnlyTheLoop(t *testing.T) {
	cases := []struct {
		name     string
		parentOf map[string]string
		want     []string
	}{
		{name: "a forest has no cycle", parentOf: map[string]string{"c": "b", "b": "a", "x": "a"}, want: nil},
		{name: "empty", parentOf: map[string]string{}, want: nil},
		{name: "a two-node loop", parentOf: map[string]string{"a": "b", "b": "a"}, want: []string{"a", "b"}},
		{name: "a self parent", parentOf: map[string]string{"a": "a"}, want: []string{"a"}},
		// "tail" hangs off the loop b->c->d->b. A walk from tail enters the
		// loop, so a detector that reported its whole path would name tail as a
		// member of a loop it is not in.
		{name: "a tail leading into a loop", parentOf: map[string]string{"tail": "b", "b": "c", "c": "d", "d": "b"}, want: []string{"b", "c", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parentCycle(tc.parentOf)
			if len(got) != len(tc.want) {
				t.Fatalf("parentCycle() = %v, want %v", got, tc.want)
			}
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("parentCycle() = %v, want it to contain %s", got, want)
				}
			}
		})
	}
}

// Reparenting that does not close a cycle must still work, or the guard has
// simply broken the feature. A sibling move and a move to the top level are the
// two ordinary shapes.
func TestSetParentStillAcceptsANonCyclicMove(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "S", "L")
	other, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Other", Topic: "hier", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(other) error = %v", err)
	}
	// L moves from under S to under the unrelated epic: no cycle.
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: chain[2].ID, ParentID: other.ID}); err != nil {
		t.Fatalf("SetParent(child=%s, parent=%s) error = %v; this move closes no cycle", chain[2].ID, other.ID, err)
	}
	got := parentOf(t, ctx, st, []model.Issue{chain[2]})
	if got[chain[2].ID] != other.ID {
		t.Errorf("parent of %s = %q, want %q", chain[2].ID, got[chain[2].ID], other.ID)
	}
}
