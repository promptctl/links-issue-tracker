package store

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// AddRelation reaches the same edge by another door. It must refuse on the
// same terms, or the rule is only as strong as which store method the caller
// invoked.
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

// A parent that is not an epic: the wait graph carries no edge there, so a
// rule derived from waits cannot see it, while hydration walks the parent
// chain regardless. Containment refuses a non-epic parent outright
// (storage.RejectParentThatIsNotAContainer), so this is refused before the
// cycle is even judged; what is pinned is that the refusal, whichever rule
// speaks it, leaves the tree untouched.
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
// written before the rule can still hold one, and every read that derives an
// epic's state refuses it. Doctor is where that state is named, so the operator
// learns it from a report that still runs.
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
	// The repair it names has to work on the state it names: a loop is
	// usually leaves, and `lit parent clear` refuses a leaf, so the one named
	// is the reparent, which replaces the edge without climbing.
	for _, line := range report.Errors {
		if strings.Contains(line, "parent cycle") && !strings.Contains(line, "lit parent set --child") {
			t.Errorf("Doctor().Errors names %q as the repair; want 'lit parent set --child'", line)
		}
	}
	// The checks this report could not run say so. Otherwise their zero values
	// are indistinguishable from a clean result, and the status line renders a
	// check that never happened as a check that found nothing.
	for _, want := range []string{storage.CheckRankInversions, storage.CheckDuplicateRanks, storage.CheckDependencyCycle} {
		if !slices.Contains(report.Unchecked, want) {
			t.Errorf("Doctor().Unchecked = %v, want it to name %s as unrun", report.Unchecked, want)
		}
	}
	// And a clean report claims nothing of the sort.
	if len(clean.Unchecked) != 0 {
		t.Errorf("Doctor().Unchecked = %v on a tree, want empty: every check ran", clean.Unchecked)
	}
}

// parentCycle is the pure half of the detector, so its shapes are pinned
// directly: a forest reports nothing, and a loop reports only its own members —
// never the tail that merely leads into it.
func TestParentCycleFindsOnlyTheLoop(t *testing.T) {
	cases := []struct {
		name     string
		parentOf map[string][]string
		want     []string
	}{
		{name: "a forest has no cycle", parentOf: map[string][]string{"c": {"b"}, "b": {"a"}, "x": {"a"}}, want: nil},
		{name: "empty", parentOf: map[string][]string{}, want: nil},
		{name: "a two-node loop", parentOf: map[string][]string{"a": {"b"}, "b": {"a"}}, want: []string{"a", "b"}},
		{name: "a self parent", parentOf: map[string][]string{"a": {"a"}}, want: []string{"a"}},
		// "tail" hangs off the loop b->c->d->b. A walk from tail enters the
		// loop, so a detector that reported its whole path would name tail as a
		// member of a loop it is not in.
		{name: "a tail leading into a loop", parentOf: map[string][]string{"tail": {"b"}, "b": {"c"}, "c": {"d"}, "d": {"b"}}, want: []string{"b", "c", "d"}},
		// Restored data can hold two parent edges for one child, because the
		// reconcile replay writes relation rows verbatim. A detector keeping one
		// parent per child follows whichever edge it kept and reports a clean
		// hierarchy while hydration, which joins every matching row, still
		// recurses through the other one.
		{name: "a loop reachable only through a second parent edge", parentOf: map[string][]string{"a": {"root", "b"}, "b": {"a"}}, want: []string{"a", "b"}},
		// The same shape with the loop on the first edge, so the case cannot
		// pass by always taking the last edge either.
		{name: "a loop on the first of two parent edges", parentOf: map[string][]string{"a": {"b", "root"}, "b": {"a"}}, want: []string{"a", "b"}},
		// A node with two parents that both reach roots is still a forest: more
		// than one parent is not by itself a loop.
		{name: "two parents, both acyclic", parentOf: map[string][]string{"a": {"p", "q"}, "p": {"root"}, "q": {"root"}}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storage.ParentCycle(tc.parentOf)
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

// seedParentEdge closes a parent edge directly, simulating data that entered
// through a path the write boundary does not gate — the reconcile replay, or a
// workspace written before the guard existed.
func seedParentEdge(t *testing.T, ctx context.Context, st *Store, childID, parentID string) {
	t.Helper()
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, 'import')`,
		childID, parentID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed parent edge %s -> %s: %v", childID, parentID, err)
	}
}

// Doctor names 'lit parent clear' as the repair for a loop member that is an
// epic belonging at the top level. That instruction has to work on the state
// doctor just described: a repair that crashes on the fault it repairs leaves
// the workspace unreadable, which is the condition this whole rule exists to
// prevent.
//
// The anti-vacuity precondition is the doctor check below: on this workspace a
// ClearParent that began with GetIssue would be refused by the read it
// started with (storage.ParentCycleError) rather than breaking the loop.
func TestClearParentBreaksAStoredCycleWithoutHydrating(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "S")
	seedParentEdge(t, ctx, st, chain[0].ID, chain[1].ID)

	// The workspace is genuinely unreadable first — otherwise this case would
	// pass against a store where hydration never had a problem.
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(report.ParentCycle) == 0 {
		t.Fatal("Doctor().ParentCycle is empty; the fixture did not create a loop")
	}

	if err := st.ClearParent(ctx, chain[0].ID); err != nil {
		t.Fatalf("ClearParent(%s) on a looped hierarchy error = %v, want the loop broken", chain[0].ID, err)
	}

	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() after repair error = %v", err)
	}
	if len(after.ParentCycle) != 0 {
		t.Errorf("Doctor().ParentCycle = %v after 'lit parent clear', want the loop gone", after.ParentCycle)
	}
	// The repaired workspace reads again, which is the point of the repair.
	if _, err := st.GetIssue(ctx, chain[0].ID); err != nil {
		t.Errorf("GetIssue(%s) after repair error = %v, want the workspace readable", chain[0].ID, err)
	}
}

// A loop is usually leaves, which 'lit parent clear' refuses to detach, so the
// repair doctor names first is the reparent onto an epic outside the loop. It
// has to work on the stored loop: its writes read columns and edges, never a
// hydrated issue, and the climb it runs starts above the epic it names.
func TestSetParentBreaksAStoredLoopOfLeaves(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := parentChain(t, ctx, st, "E")[0]
	var leaves []model.Issue
	for _, title := range []string{"A", "B"} {
		leaf, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "hier", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		leaves = append(leaves, leaf)
	}
	a, b := leaves[0], leaves[1]
	if _, err := st.db.ExecContext(ctx, `DELETE FROM relations WHERE type = 'parent-child' AND src_id IN (?, ?)`, a.ID, b.ID); err != nil {
		t.Fatalf("detach the leaves: %v", err)
	}
	seedParentEdge(t, ctx, st, a.ID, b.ID)
	seedParentEdge(t, ctx, st, b.ID, a.ID)
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(report.ParentCycle) == 0 {
		t.Fatal("Doctor().ParentCycle is empty; the fixture did not create a loop")
	}

	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: a.ID, ParentID: epic.ID, CreatedBy: "test"}); err != nil {
		t.Fatalf("SetParent(%s under %s) on a stored loop of leaves error = %v, want the loop broken", a.ID, epic.ID, err)
	}
	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() after the reparent error = %v", err)
	}
	if len(after.ParentCycle) != 0 {
		t.Errorf("Doctor().ParentCycle = %v after 'lit parent set', want the loop gone", after.ParentCycle)
	}
	if _, err := st.GetIssue(ctx, a.ID); err != nil {
		t.Errorf("GetIssue(%s) after the reparent error = %v, want the workspace readable", a.ID, err)
	}
}

// A recovery rebuild writes its source as it was, so a source from before
// every ticket lived inside an epic brings back the tickets the write boundary
// refuses. Doctor names each of them and the repair, as a warning the recovery
// gate does not refuse, and the repair clears them.
func TestDoctorNamesTicketsOutsideAnEpic(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := parentChain(t, ctx, st, "E")[0]
	var leaves []model.Issue
	for _, title := range []string{"loose", "host", "under"} {
		leaf, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "hier", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		leaves = append(leaves, leaf)
	}
	loose, host, under := leaves[0], leaves[1], leaves[2]

	clean, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if len(clean.Errors) != 0 || len(clean.Warnings) != 0 {
		t.Fatalf("Doctor() errors %v, warnings %v on a contained hierarchy; want none", clean.Errors, clean.Warnings)
	}

	if _, err := st.db.ExecContext(ctx, `DELETE FROM relations WHERE type = 'parent-child' AND src_id IN (?, ?)`, loose.ID, under.ID); err != nil {
		t.Fatalf("detach the leaves: %v", err)
	}
	seedParentEdge(t, ctx, st, under.ID, host.ID)
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	wantLines := []string{
		"tickets outside an epic: " + loose.ID + " (task)",
		"tickets under a parent that is not an epic: " + under.ID + " under " + host.ID + " (task)",
	}
	if len(report.Errors) != 0 {
		t.Errorf("Doctor().Errors = %v, want none: every read still answers over a ticket outside an epic", report.Errors)
	}
	if len(report.Warnings) != len(wantLines) {
		t.Fatalf("Doctor().Warnings = %v, want %d warnings, one per broken half of containment", report.Warnings, len(wantLines))
	}
	for i, want := range wantLines {
		if !strings.HasPrefix(report.Warnings[i], want) || !strings.Contains(report.Warnings[i], "lit parent set --child <id> --parent <epic-id>") {
			t.Errorf("Doctor().Warnings[%d] = %q, want it to begin %q and name the repair", i, report.Warnings[i], want)
		}
	}

	for _, leaf := range []model.Issue{loose, under} {
		if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: leaf.ID, ParentID: epic.ID, CreatedBy: "test"}); err != nil {
			t.Fatalf("SetParent(%s under %s) error = %v", leaf.ID, epic.ID, err)
		}
	}
	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() after the repair error = %v", err)
	}
	if len(after.Errors) != 0 || len(after.Warnings) != 0 {
		t.Errorf("Doctor() errors %v, warnings %v after 'lit parent set'; want none", after.Errors, after.Warnings)
	}
}

// The rule is about the edges, not the lifecycle of their endpoints. A soft
// delete is reversible, so a loop tolerated because one member is deleted is a
// loop waiting to come back: restoring that member yields exactly the hierarchy
// the guard promised could not exist. Consumers also disagree about which dead
// edges they climb — lifecycleChildrenByEpicIDs keeps a dead container's
// membership — so a detector scoped to any one of them misses loops another
// still walks.
func TestTheCycleRuleSeesAnEdgeThroughADeletedIssue(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "S")
	parent, child := chain[0], chain[1]

	if _, err := st.db.ExecContext(ctx, `UPDATE issues SET deleted_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), child.ID); err != nil {
		t.Fatalf("soft-delete %s: %v", child.ID, err)
	}

	// The edge child -> parent is still stored and still climbed. Making the
	// parent a child of it would therefore close a loop hydration can walk.
	_, err := st.SetParent(ctx, storage.SetParentInput{ChildID: parent.ID, ParentID: child.ID, CreatedBy: "test"})
	if err == nil {
		t.Fatalf("SetParent(child=%s, parent=%s) succeeded, want a refusal: the deleted issue's parent edge is still climbed by hydration", parent.ID, child.ID)
	}

	// And the refusal left the hierarchy alone.
	rels, relErr := st.ListRelationsForIssue(ctx, parent.ID, model.RelParentChild)
	if relErr != nil {
		t.Fatalf("ListRelationsForIssue error = %v", relErr)
	}
	for _, rel := range rels {
		if rel.SrcID == parent.ID {
			t.Errorf("the refused write still stored %s -> %s", rel.SrcID, rel.DstID)
		}
	}
}

// VerifyCandidate's health gate exists for untrusted data, and a parent cycle is
// exactly the untrusted shape that stops the gates below it: they all read
// through Export, which hydrates and so refuses the loop. Without the early
// return, the gate fails with that refusal instead of reporting the finding.
func TestVerifyCandidateReportsAParentCycleRatherThanHanging(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "S")
	seedParentEdge(t, ctx, st, chain[0].ID, chain[1].ID)

	report, err := VerifyCandidate(ctx, RawDump{}, ShapeMapping{}, st)
	if err != nil {
		t.Fatalf("VerifyCandidate() error = %v, want a report naming the cycle", err)
	}
	found := false
	for _, f := range report.Findings {
		if f.Law == LawHealth && strings.Contains(f.Detail, "parent cycle") {
			found = true
		}
	}
	if !found {
		t.Errorf("VerifyCandidate().Findings = %+v, want a health finding naming the parent cycle", report.Findings)
	}
}

// A read that derives an epic's state from its children cannot derive one for
// an epic that is its own descendant. Over a stored loop of epics it refuses,
// naming the loop and the repair, instead of descending into the loop until
// the context runs out — the deadline here is what turns that hang into a
// failure of this test rather than of the suite.
func TestAReadRefusesAStoredLoopOfEpicsByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "F")
	// The tree reads before the loop closes, so a refusal below is the loop's.
	if _, err := st.ListIssues(ctx, storage.ListIssuesFilter{}); err != nil {
		t.Fatalf("ListIssues() on a tree error = %v", err)
	}
	seedParentEdge(t, ctx, st, chain[0].ID, chain[1].ID)

	_, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	var refusal storage.ParentCycleError
	if !errors.As(err, &refusal) {
		t.Fatalf("ListIssues() over a loop error = %v, want a ParentCycleError", err)
	}
	if !slices.Contains(refusal.Cycle, chain[0].ID) || !slices.Contains(refusal.Cycle, chain[1].ID) || len(refusal.Cycle) != 2 {
		t.Errorf("ParentCycleError.Cycle = %v, want exactly %s and %s", refusal.Cycle, chain[0].ID, chain[1].ID)
	}
	if !strings.Contains(err.Error(), "lit parent set --child") {
		t.Errorf("refusal %q names no repair; want 'lit parent set --child'", err)
	}
	// One issue's read meets the same loop.
	if _, err := st.GetIssue(ctx, chain[1].ID); !errors.As(err, &refusal) {
		t.Errorf("GetIssue(%s) over a loop error = %v, want a ParentCycleError", chain[1].ID, err)
	}
}

// A restore lands an export as the store's rules stand now, and a loop-free
// hierarchy is one of them: an export holding a loop of epics lands with one
// edge of the loop cut, the landing names the cut, and the store reads after.
// Which edge goes is worked by hand from the rule — the member with the
// greater id loses its edge onto the other.
func TestRestoreCutsALoopTheExportHolds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := openIssueStore(t, ctx)
	chain := parentChain(t, ctx, st, "E", "F")
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	export.Relations = append(export.Relations, model.Relation{SrcID: chain[0].ID, DstID: chain[1].ID, Type: model.RelParentChild, CreatedAt: time.Now().UTC(), CreatedBy: "import"})
	lo, hi := min(chain[0].ID, chain[1].ID), max(chain[0].ID, chain[1].ID)

	landing, err := st.ReplaceFromExport(ctx, export)
	if err != nil {
		t.Fatalf("ReplaceFromExport() of an export holding a loop error = %v, want it landed", err)
	}
	want := []storage.Unlooped{{IssueID: hi, Parent: lo, Cycle: []string{lo, hi}}}
	if !slices.EqualFunc(landing.Unlooped, want, func(a, b storage.Unlooped) bool {
		return a.IssueID == b.IssueID && a.Parent == b.Parent && slices.Equal(a.Cycle, b.Cycle)
	}) {
		t.Errorf("Landing.Unlooped = %+v, want %+v", landing.Unlooped, want)
	}
	if got := parentOf(t, ctx, st, chain); got[hi] != "" || got[lo] != hi {
		t.Errorf("parents after restore = %v, want %s under %s and %s at the top level", got, lo, hi, hi)
	}
	if _, err := st.ListIssues(ctx, storage.ListIssuesFilter{}); err != nil {
		t.Errorf("ListIssues() after the restore error = %v, want the store readable", err)
	}
}

// A child two parents claim is a fault in the data, and every reader still
// answers over it — the listing, the export behind backup and sync, `lit show`
// — because the readers are not where it is enforced. Tree order and the frame
// lookup both put the child under the lower parent id, so a rank move and the
// listing agree on where it sits; doctor is where the fault is said out loud,
// with its other checks still run; and the repair it names clears it.
func TestAChildWithTwoParentsIsReportedAndRepairable(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	first := parentChain(t, ctx, st, "E1", "C")
	second := parentChain(t, ctx, st, "E2")
	child := first[1]
	seedParentEdge(t, ctx, st, child.ID, second[0].ID)
	lower := min(first[0].ID, second[0].ID)

	listed, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v, want the workspace listed", err)
	}
	at := slices.IndexFunc(listed, func(issue model.Issue) bool { return issue.ID == child.ID })
	if at < 1 || listed[at-1].ID != lower {
		t.Errorf("ListIssues() = %v, want %s listed right after %s, the lower of its parents", issueIDs(listed), child.ID, lower)
	}
	end, err := st.RankToTop(ctx, child.ID)
	if err != nil {
		t.Fatalf("RankToTop(%s) error = %v", child.ID, err)
	}
	if end.Frame != storage.Frame(lower) {
		t.Errorf("RankToTop(%s).Frame = %q, want %q: the frame lookup and tree order must agree on its parent", child.ID, end.Frame, lower)
	}
	if _, err := st.Export(ctx); err != nil {
		t.Errorf("Export() error = %v, want the export behind backup and sync to answer", err)
	}
	if _, err := st.GetIssueDetail(ctx, child.ID); err != nil {
		t.Errorf("GetIssueDetail(%s) error = %v, want the child itself showable", child.ID, err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v, want a report naming the child", err)
	}
	if len(report.Errors) != 1 || !strings.Contains(report.Errors[0], child.ID+" has 2 parents") || !strings.Contains(report.Errors[0], "lit parent set --child "+child.ID) {
		t.Errorf("Doctor().Errors = %v, want one error naming %s's parents and the repair", report.Errors, child.ID)
	}
	if len(report.Unchecked) != 0 {
		t.Errorf("Doctor().Unchecked = %v, want every check run: the fault stops no read", report.Unchecked)
	}

	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: child.ID, ParentID: first[0].ID, CreatedBy: "test"}); err != nil {
		t.Fatalf("SetParent(%s under %s) error = %v, want every parent it held replaced with that one", child.ID, first[0].ID, err)
	}
	after, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() after repair error = %v", err)
	}
	if len(after.Errors) != 0 {
		t.Errorf("Doctor().Errors = %v after 'lit parent set', want none", after.Errors)
	}
}

// The detach rule keeps a leaf inside an epic, so it is judged on the parent
// edges the removal leaves. A leaf two parents claim stays inside an epic when
// one edge goes, and removing that edge is a repair, not a detach; removing the
// last one is the detach the rule refuses, and the refusal leaves the edge.
func TestRemovingOneOfTwoParentEdgesKeepsALeafInside(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	first := parentChain(t, ctx, st, "E1")[0]
	second := parentChain(t, ctx, st, "E2")[0]
	child, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "C", Topic: "hier", IssueType: "task", ParentID: first.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(C) error = %v", err)
	}
	seedParentEdge(t, ctx, st, child.ID, second.ID)

	if err := st.RemoveRelation(ctx, child.ID, second.ID, model.RelParentChild); err != nil {
		t.Fatalf("RemoveRelation(%s -> %s) error = %v, want the second parent edge removed: %s keeps %s", child.ID, second.ID, err, child.ID, first.ID)
	}
	err = st.RemoveRelation(ctx, child.ID, first.ID, model.RelParentChild)
	var refused model.ValidationError
	if !errors.As(err, &refused) {
		t.Fatalf("RemoveRelation(%s -> %s) error = %v, want a refusal: it is the leaf's last parent", child.ID, first.ID, err)
	}
	if got := parentOf(t, ctx, st, []model.Issue{child})[child.ID]; got != first.ID {
		t.Errorf("parent of %s after the refusal = %q, want %q kept", child.ID, got, first.ID)
	}
}

// "No such issue" and "that issue has no parent" send the operator to
// different places, and a typo'd id reported as a missing edge is the opposite
// answer: it says the hierarchy is fine when the id is not.
func TestClearParentReportsAnUnknownIssueAsMissing(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	err := st.ClearParent(ctx, "test-hier-nosuchissue")
	if err == nil {
		t.Fatal("ClearParent(unknown id) succeeded, want a not-found error")
	}
	var notFound storage.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("ClearParent(unknown id) error = %v, want a storage.NotFoundError", err)
	}
	if notFound.Entity != "issue" {
		t.Errorf("ClearParent(unknown id) reported a missing %q; the id names no issue at all, so reporting a missing parent edge is the opposite diagnosis", notFound.Entity)
	}

	// Anti-vacuity: an issue that exists and simply has no parent — an epic,
	// the one shape that can be parentless — still reports the edge as the
	// missing thing, so the case above is not passing because every absence is
	// called an issue.
	solo, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Solo", Topic: "hier", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue error = %v", err)
	}
	err = st.ClearParent(ctx, solo.ID)
	if err == nil {
		t.Fatal("ClearParent on a parentless issue succeeded, want an absence report")
	}
	if !errors.As(err, &notFound) || notFound.Entity != "parent relation" {
		t.Errorf("ClearParent(parentless issue) error = %v, want a missing \"parent relation\"", err)
	}
}
