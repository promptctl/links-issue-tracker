package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// makeEpics creates n epics at the bottom of the workspace and returns their
// ids in creation order, which is also their rank order. Epics, because the
// callers name them as parents and as the whole top level of the workspace.
func makeEpics(t *testing.T, ctx context.Context, st *Store, n int, titlef string) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf(titlef, i), Topic: "batch",
			IssueType: "epic", Placement: storage.RankBottom,
		})
		if err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		ids = append(ids, issue.ID)
	}
	return ids
}

// manyIDs is how many ids the every-id tests read. The number itself is
// arbitrary: an id-keyed read is one join however many ids it carries, so
// there is no boundary to straddle, only more than one id to lose.
const manyIDs = 34

// A long id filter must come back whole.
//
// The query cannot report this failure: a read that answers for only part of
// its ids is still well-formed, and an issue it never read is
// indistinguishable from one the other filters excluded. Nothing errors and
// the caller prints a shorter truth.
//
// The count assertion covers every id at once; the three named probes sit at
// the start, the middle, and the end, so a failure says WHICH end was lost
// rather than only that a number was wrong.
func TestListIssuesByIDsAnswersForEveryID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const count = manyIDs
	ids := makeEpics(t, ctx, st, count, "subject %d")

	got, err := st.ListIssues(ctx, storage.ListIssuesFilter{IDs: ids})
	if err != nil {
		t.Fatalf("ListIssues(%d ids) error = %v", len(ids), err)
	}
	if len(got) != count {
		t.Fatalf("ListIssues(%d ids) returned %d issues, want %d — some ids were never read", len(ids), len(got), count)
	}
	for _, i := range []int{0, count / 2, count - 1} {
		if !containsIssueID(got, ids[i]) {
			t.Fatalf("id %d of %d (%s) is missing: it was never read", i, count, ids[i])
		}
	}
}

// The parent filter reads over the PARENTS, so what it must cover is the
// parents named and not the children they hold. One child each is what
// separates the two: a filter that read only some of the parents would still
// return rows, just fewer of them.
func TestListIssuesByParentIDsAnswersForEveryParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const count = manyIDs
	parents := makeEpics(t, ctx, st, count, "parent %d")
	children := make([]string, 0, count)
	for i, parent := range parents {
		child, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("child %d", i), Topic: "batch",
			IssueType: "task", Placement: storage.RankBottom, ParentID: parent,
		})
		if err != nil {
			t.Fatalf("CreateIssue(child %d) error = %v", i, err)
		}
		children = append(children, child.ID)
	}

	got, err := st.ListIssues(ctx, storage.ListIssuesFilter{ParentIDs: parents})
	if err != nil {
		t.Fatalf("ListIssues(%d parents) error = %v", len(parents), err)
	}
	if !slices.Equal(slices.Sorted(slices.Values(issueIDs(got))), slices.Sorted(slices.Values(children))) {
		t.Fatalf("ListIssues(%d parents) returned %d issues, want exactly the %d children — some parents were never read", len(parents), len(got), len(children))
	}
}

// The two id-shaped filters intersect, and the intersection is taken before the
// scan rather than by the scan.
func TestListIssuesByIDsIntersectsParentIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const count = manyIDs
	parents := makeEpics(t, ctx, st, count, "parent %d")
	children := make([]string, 0, count)
	for i, parent := range parents {
		child, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("child %d", i), Topic: "batch",
			IssueType: "task", Placement: storage.RankBottom, ParentID: parent,
		})
		if err != nil {
			t.Fatalf("CreateIssue(child %d) error = %v", i, err)
		}
		children = append(children, child.ID)
	}
	// Children from the start, the middle and the end, and — the case that
	// separates an intersection from a union — one id that is nobody's child.
	wanted := []string{children[0], children[count/2], children[count-1]}
	asked := append(slices.Clone(wanted), parents[0])

	got, err := st.ListIssues(ctx, storage.ListIssuesFilter{IDs: asked, ParentIDs: parents})
	if err != nil {
		t.Fatalf("ListIssues(ids+parents) error = %v", err)
	}
	if !slices.Equal(slices.Sorted(slices.Values(issueIDs(got))), slices.Sorted(slices.Values(wanted))) {
		t.Fatalf("ListIssues(ids+parents) = %v, want %v — the two filters must intersect, not union", issueIDs(got), wanted)
	}
}

// An id filter that narrows to nothing means no rows, and a filter of nothing
// but blanks means no narrowing. The two are one line apart in the code and
// opposite in effect, which is why they are pinned together: reading "empty set
// of ids" as "no filter" turns `lit ls --parent <childless-epic>` into a listing
// of the whole backlog.
func TestListIssuesEmptySelectionVersusBlankFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	all := makeEpics(t, ctx, st, 3, "row %d")
	childless := all[0]

	narrowed, err := st.ListIssues(ctx, storage.ListIssuesFilter{ParentIDs: []string{childless}})
	if err != nil {
		t.Fatalf("ListIssues(childless parent) error = %v", err)
	}
	if len(narrowed) != 0 {
		t.Fatalf("ListIssues(childless parent) = %v, want no rows — an empty selection is not an absent filter", issueIDs(narrowed))
	}

	blank, err := st.ListIssues(ctx, storage.ListIssuesFilter{IDs: []string{"", "   "}})
	if err != nil {
		t.Fatalf("ListIssues(blank ids) error = %v", err)
	}
	if len(blank) != len(all) {
		t.Fatalf("ListIssues(blank ids) returned %d issues, want all %d — a whitespace-only filter constrains nothing", len(blank), len(all))
	}
}

// Repeating a rank-set that has already arrived must change nothing.
//
// A rank set that re-placed its stack on every call would drift a little on
// each repeat, and that drift is invisible in the ORDER, which is why this
// asserts on the keys: the first application is already the fixed point.
func TestRankSetRepeatedIsAFixedPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeEpics(t, ctx, st, 6, "row %d")
	stack := []string{ids[5], ids[4], ids[3]}

	ranksAfter := func(round int) map[string]string {
		if _, err := st.RankSet(ctx, stack); err != nil {
			t.Fatalf("RankSet(round %d) error = %v", round, err)
		}
		listed, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
		if err != nil {
			t.Fatalf("ListIssues(round %d) error = %v", round, err)
		}
		if got := issueIDs(listed)[:len(stack)]; !slices.Equal(got, stack) {
			t.Fatalf("round %d top of workspace = %v, want %v", round, got, stack)
		}
		out := map[string]string{}
		for _, issue := range listed {
			out[issue.ID] = issue.Rank
		}
		return out
	}

	settled := ranksAfter(1)
	for round := 2; round <= 5; round++ {
		if got := ranksAfter(round); !maps.Equal(got, settled) {
			t.Fatalf("round %d ranks = %v, want the round-1 ranks %v — a repeat of an order that has arrived rewrote keys", round, got, settled)
		}
	}
}
