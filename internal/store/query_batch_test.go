package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// makeIssues creates n issues at the bottom of the workspace and returns their
// ids in creation order, which is also their rank order.
func makeIssues(t *testing.T, ctx context.Context, st *Store, n int, titlef string) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf(titlef, i), Topic: "batch",
			IssueType: "task", Placement: storage.RankBottom,
		})
		if err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		ids = append(ids, issue.ID)
	}
	return ids
}

// An id filter longer than one batch must come back whole.
//
// This is the failure batching can hide and the query cannot report: a read
// that asks only the first batch, or drops the trailing partial one, answers
// with a union of whatever it did ask for, and at that point an issue whose
// batch was never queried is indistinguishable from one the other filters
// excluded. Nothing errors and the caller prints a shorter truth.
//
// The count assertion covers every batch at once; the three named probes sit in
// the first batch, a middle one, and the trailing partial one, so a failure
// says WHICH end was lost rather than only that a number was wrong.
func TestListIssuesByIDsSpansEveryBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const count = 2*idBatchSize + 2
	ids := makeIssues(t, ctx, st, count, "subject %d")

	got, err := st.ListIssues(ctx, storage.ListIssuesFilter{IDs: ids})
	if err != nil {
		t.Fatalf("ListIssues(%d ids) error = %v", len(ids), err)
	}
	if len(got) != count {
		t.Fatalf("ListIssues(%d ids) returned %d issues, want %d — a batch went unqueried", len(ids), len(got), count)
	}
	for _, i := range []int{0, idBatchSize + 1, count - 1} {
		if !containsIssueID(got, ids[i]) {
			t.Fatalf("id %d of %d (%s) is missing: its batch was never queried", i, count, ids[i])
		}
	}
}

// The parent filter batches over the PARENTS, so its boundary is the number of
// parents named and not the number of children they hold. One child each is
// what separates the two: a filter that queried only the first batch of parents
// would still return rows, just fewer of them.
func TestListIssuesByParentIDsSpansEveryBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const count = 2*idBatchSize + 2
	parents := makeIssues(t, ctx, st, count, "parent %d")
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
		t.Fatalf("ListIssues(%d parents) returned %d issues, want exactly the %d children — a parent batch went unqueried", len(parents), len(got), len(children))
	}
}

// The two id-shaped filters intersect, and the intersection is taken before the
// scan rather than by the scan. Both boundaries are exercised at once: the ids
// kept straddle the id filter's batches, and the parents naming them straddle
// the parent filter's.
func TestListIssuesByIDsIntersectsParentIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const count = 2*idBatchSize + 2
	parents := makeIssues(t, ctx, st, count, "parent %d")
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
	// One child from the first batch, one from a middle batch, one from the
	// trailing partial batch, and — the case that separates an intersection
	// from a union — one id that is nobody's child.
	wanted := []string{children[0], children[idBatchSize+1], children[count-1]}
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

	all := makeIssues(t, ctx, st, 3, "row %d")
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
// The frame's top is read to find what the stack is placed above, and the rows
// the stack itself holds are not walls — they are about to be vacated. When the
// stack is already at the top, EVERY row the top-of-frame read yields before
// the answer is one of them, so a read that stops at the first row anchors the
// stack against its own current position and re-places it a little higher on
// every repeat. That drift is invisible in the ORDER, which is why this asserts
// on the keys: from the second application onward the assigned ranks are a
// fixed point, because the row they are measured against is the same one.
//
// The stack is deliberately larger than one row so that a read bounded by a
// constant — rather than by the size of the moving set — fails here too.
func TestRankSetRepeatedIsAFixedPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeIssues(t, ctx, st, 6, "row %d")
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

	ranksAfter(1)
	settled := ranksAfter(2)
	for round := 3; round <= 5; round++ {
		if got := ranksAfter(round); !maps.Equal(got, settled) {
			t.Fatalf("round %d ranks = %v, want the round-2 ranks %v — the stack is being anchored against itself and drifting", round, got, settled)
		}
	}
}

// nearestRankOutside must read past every row it may have to skip.
//
// This is tested directly rather than through RankSet because RankSet cannot
// reach the case: it anchors on the topmost row of the frame that is NOT
// moving, so in a single-frame workspace everything above the anchor is moving
// and a read bounded at one row happens to give the same answer. The contract
// the helper states is the stronger one — the first qualifying row in the
// order, whatever it takes to get there — and a read bounded by a constant
// answers "" instead, which every caller reads as the open end of the
// keyspace.
func TestNearestRankOutsideReadsPastEveryExcludedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := makeIssues(t, ctx, st, 5, "row %d")
	listed, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	byID := map[string]string{}
	for _, issue := range listed {
		byID[issue.ID] = issue.Rank
	}

	const query = `SELECT id, item_rank FROM issues
		WHERE deleted_at IS NULL AND item_rank != ''
		ORDER BY item_rank ASC`

	// Everything ahead of ids[3] in the order is excluded, so the answer is
	// four rows deep and no shallower read can find it.
	got, err := nearestRankOutside(ctx, st.db, query, ids[:3])
	if err != nil {
		t.Fatalf("nearestRankOutside(3 excluded) error = %v", err)
	}
	if want := byID[ids[3]]; got != want {
		t.Fatalf("nearestRankOutside(3 excluded) = %q, want %q — the read stopped before it had passed every excluded row", got, want)
	}

	// The two-sided pair: excluding nothing must not read past the first row,
	// so a helper that skipped rows for some other reason fails here.
	got, err = nearestRankOutside(ctx, st.db, query, nil)
	if err != nil {
		t.Fatalf("nearestRankOutside(none excluded) error = %v", err)
	}
	if want := byID[ids[0]]; got != want {
		t.Fatalf("nearestRankOutside(none excluded) = %q, want the first row %q", got, want)
	}

	// Excluding every row there is means there is no key outside, which every
	// caller turns into the open end of the keyspace rather than an error.
	got, err = nearestRankOutside(ctx, st.db, query, ids)
	if err != nil {
		t.Fatalf("nearestRankOutside(all excluded) error = %v", err)
	}
	if got != "" {
		t.Fatalf("nearestRankOutside(all excluded) = %q, want \"\" — the open end of the keyspace", got)
	}
}
