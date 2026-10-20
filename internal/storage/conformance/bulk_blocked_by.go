package conformance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// tasksInEpicsOfTheirOwn creates n tasks, each under an epic of its own: a
// blocked_by between two leaves of one epic is refused, since rank orders
// them, so a chain across tasks needs one epic apiece.
func tasksInEpicsOfTheirOwn(t *testing.T, ctx context.Context, st storage.Store, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range n {
		epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: fmt.Sprintf("epic %d", i), Topic: "core", IssueType: model.TypeEpic})
		ids[i] = mustCreate(t, ctx, st, storage.CreateIssueInput{Title: fmt.Sprintf("task %d", i), Topic: "core", ParentID: epic.ID}).ID
	}
	return ids
}

// blockersOf lists the issues id is blocked by, sorted.
func blockersOf(t *testing.T, ctx context.Context, st storage.Store, id string) []string {
	t.Helper()
	rels, err := st.ListRelationsForIssue(ctx, id, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue(%s) error = %v", id, err)
	}
	var blockers []string
	for _, rel := range rels {
		if rel.SrcID == id {
			blockers = append(blockers, rel.DstID)
		}
	}
	slices.Sort(blockers)
	return blockers
}

// blockedByGraph is every blocked-by edge out of ids, one "a<-b" per edge, so
// a test can say a file left exactly the edges it states and no others.
func blockedByGraph(t *testing.T, ctx context.Context, st storage.Store, ids []string) []string {
	t.Helper()
	var edges []string
	for _, id := range ids {
		for _, blocker := range blockersOf(t, ctx, st, id) {
			edges = append(edges, id+"<-"+blocker)
		}
	}
	return edges
}

func blockedBy(ids ...string) *[]string { return &ids }

// aBulkFileWiresADependencyChainAcrossExistingIssues is the batch case the
// update document's blocked_by exists for: a chain across five existing
// tickets is one file, and it leaves those edges and no others. A document
// stating the set its issue already holds is reported as held.
func aBulkFileWiresADependencyChainAcrossExistingIssues(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	ids := tasksInEpicsOfTheirOwn(t, ctx, st, 5)
	specs := []storage.BulkIssueSpec{{ID: ids[0], BlockedBy: blockedBy()}}
	var want []string
	for i := 1; i < 5; i++ {
		specs = append(specs, storage.BulkIssueSpec{ID: ids[i], BlockedBy: blockedBy(ids[i-1])})
		want = append(want, ids[i]+"<-"+ids[i-1])
	}

	result, err := st.BulkApply(ctx, prefix, "ada", specs)
	if err != nil {
		t.Fatalf("BulkApply error = %v", err)
	}
	assertStrings(t, "blocked-by edges", blockedByGraph(t, ctx, st, ids), want)
	assertStrings(t, "blocked_by set", result.BlockedBySet, ids[1:])
	assertStrings(t, "blocked_by held", result.BlockedByHeld, ids[:1])
	if result.EdgesAdded != 4 || result.EdgesRemoved != 0 {
		t.Errorf("edges added, removed = %d, %d, want 4, 0", result.EdgesAdded, result.EdgesRemoved)
	}
}

// aBulkFileClosingADependencyCycleChangesNothing: a cycle through two
// separate documents is refused, and all five tickets keep exactly the edges
// they held, whether the file's own documents close the cycle — which the gate
// refuses before any write — or an edge already stored closes it, which only
// the write can see. The documents ahead of the one refused, an edge removed
// and an edge added, are not left written. [LAW:no-silent-failure]
func aBulkFileClosingADependencyCycleChangesNothing(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	for _, c := range []struct {
		name  string
		specs func(ids []string) []storage.BulkIssueSpec
	}{
		{"by the file's own documents", func(ids []string) []storage.BulkIssueSpec {
			return []storage.BulkIssueSpec{
				{ID: ids[4], BlockedBy: blockedBy()},
				{ID: ids[0], BlockedBy: blockedBy(ids[3])},
				{ID: ids[3], BlockedBy: blockedBy(ids[0])},
			}
		}},
		{"through an edge already stored", func(ids []string) []storage.BulkIssueSpec {
			// ids[2] <- ids[1] <- ids[0] is stored, so ids[0] <- ids[2]
			// closes the cycle through ids[1]'s document, which restates the
			// edge it holds.
			return []storage.BulkIssueSpec{
				{ID: ids[4], BlockedBy: blockedBy()},
				{ID: ids[3], BlockedBy: blockedBy(ids[4])},
				{ID: ids[1], BlockedBy: blockedBy(ids[0])},
				{ID: ids[0], BlockedBy: blockedBy(ids[2])},
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ids := tasksInEpicsOfTheirOwn(t, ctx, st, 5)
			mustBlock(t, ctx, st, ids[1], ids[0])
			mustBlock(t, ctx, st, ids[2], ids[1])
			mustBlock(t, ctx, st, ids[4], ids[3])
			before := blockedByGraph(t, ctx, st, ids)

			_, err := st.BulkApply(ctx, prefix, "ada", c.specs(ids))
			if err == nil || !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("BulkApply error = %v, want the cycle refused", err)
			}
			assertStrings(t, "blocked-by edges after the refusal", blockedByGraph(t, ctx, st, ids), before)
		})
	}
}

// aBulkFileReplacesABlockedBySetWhole: an update document's blocked_by is the
// issue's whole set, as labels is, so an edge it leaves out is dropped and
// `blocked_by: []` empties the set. The file is judged on the graph it ends
// with, so trading a blocker for the issue it blocked applies in either
// document order, though the trade's other half alone would close a cycle. A
// file that has landed writes nothing when it is imported again.
func aBulkFileReplacesABlockedBySetWhole(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed=%t", reversed), func(t *testing.T) {
			ids := tasksInEpicsOfTheirOwn(t, ctx, st, 4)
			a, b, c, d := ids[0], ids[1], ids[2], ids[3]
			mustBlock(t, ctx, st, a, c)
			mustBlock(t, ctx, st, d, b)
			specs := []storage.BulkIssueSpec{
				{ID: a, BlockedBy: blockedBy(b)},
				{ID: c, BlockedBy: blockedBy(a)},
				{ID: d, BlockedBy: blockedBy()},
			}
			if reversed {
				slices.Reverse(specs)
			}

			result, err := st.BulkApply(ctx, prefix, "ada", specs)
			if err != nil {
				t.Fatalf("BulkApply error = %v", err)
			}
			assertStrings(t, "blocked-by edges", blockedByGraph(t, ctx, st, ids), []string{a + "<-" + b, c + "<-" + a})
			if result.EdgesAdded != 2 || result.EdgesRemoved != 2 {
				t.Errorf("edges added, removed = %d, %d, want 2, 2", result.EdgesAdded, result.EdgesRemoved)
			}

			again, err := st.BulkApply(ctx, prefix, "ada", specs)
			if err != nil {
				t.Fatalf("BulkApply again error = %v", err)
			}
			if len(again.BlockedBySet) != 0 || len(again.BlockedByHeld) != 3 || again.EdgesAdded+again.EdgesRemoved != 0 {
				t.Errorf("the landed file imported again set %v, held %v, added %d, removed %d edges, want every set held and no edge written", again.BlockedBySet, again.BlockedByHeld, again.EdgesAdded, again.EdgesRemoved)
			}
		})
	}
}

// aBulkFileStatingBlockedByOnAMissingIssueIsRefused: a set stated on an id
// that names no issue is refused as not found, never read as an empty set the
// issue already holds.
func aBulkFileStatingBlockedByOnAMissingIssueIsRefused(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{{ID: "conf-core-none", BlockedBy: blockedBy()}})
	if notFound := (storage.NotFoundError{}); !errors.As(err, &notFound) {
		t.Fatalf("BulkApply error = %v, want the missing issue refused as not found", err)
	}
	if len(result.BlockedByHeld) != 0 {
		t.Errorf("blocked_by held = %v after the refusal, want none", result.BlockedByHeld)
	}
}

// aBulkFileRefusedAfterItsEdgesPutsBackTheBlockedBy: a refusal after the edge
// step — here an update naming no issue — gives every issue back the
// blocked-by edges it held, and the result says the set was undone.
func aBulkFileRefusedAfterItsEdgesPutsBackTheBlockedBy(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	ids := tasksInEpicsOfTheirOwn(t, ctx, st, 3)
	mustBlock(t, ctx, st, ids[0], ids[1])
	before := blockedByGraph(t, ctx, st, ids)
	title := "renamed"

	result, err := st.BulkApply(ctx, prefix, "ada", []storage.BulkIssueSpec{
		{ID: ids[0], BlockedBy: blockedBy(ids[2])},
		{ID: "conf-core-none", Title: &title},
	})
	if err == nil {
		t.Fatal("BulkApply error = nil, want the update of a missing issue refused")
	}
	assertStrings(t, "blocked-by edges after the refusal", blockedByGraph(t, ctx, st, ids), before)
	assertStrings(t, "blocked_by undone", result.BlockedByUndone, ids[:1])
	assertStrings(t, "blocked_by set", result.BlockedBySet, nil)
}

// aBulkFileJudgesAnUpdatesBlockedByOnTheHierarchyItEndsWith: two leaves of one
// epic may not block each other, so an update document's blocked_by between
// them applies when another document moves one of them out, and is refused,
// with nothing written, when another moves one of them in — in either
// document order, because the hierarchy is written before any blocked-by
// edge.
func aBulkFileJudgesAnUpdatesBlockedByOnTheHierarchyItEndsWith(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed=%t", reversed), func(t *testing.T) {
			home := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "home", Topic: "core", IssueType: model.TypeEpic})
			away := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "away", Topic: "core", IssueType: model.TypeEpic})
			x := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "x", Topic: "core", ParentID: home.ID})
			y := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "y", Topic: "core", ParentID: home.ID})
			specs := []storage.BulkIssueSpec{
				{ID: x.ID, BlockedBy: blockedBy(y.ID)},
				{ID: y.ID, Parent: ptr(away.ID)},
			}
			if reversed {
				slices.Reverse(specs)
			}

			if _, err := st.BulkApply(ctx, prefix, "ada", specs); err != nil {
				t.Fatalf("BulkApply error = %v", err)
			}
			assertStrings(t, "x's blockers", blockersOf(t, ctx, st, x.ID), []string{y.ID})
			assertStrings(t, "y's parents", parentsOf(t, ctx, st, y.ID), []string{away.ID})

			z := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "z", Topic: "core", ParentID: away.ID})
			into := []storage.BulkIssueSpec{
				{ID: x.ID, BlockedBy: blockedBy(y.ID, z.ID)},
				{ID: z.ID, Parent: ptr(home.ID)},
			}
			if reversed {
				slices.Reverse(into)
			}
			_, err := st.BulkApply(ctx, prefix, "ada", into)
			if err == nil || !strings.Contains(err.Error(), storage.SameEpicBlocksRejectionMessage) {
				t.Fatalf("BulkApply moving z in beside x = %v, want the blocked_by inside one epic refused", err)
			}
			assertStrings(t, "x's blockers after the refusal", blockersOf(t, ctx, st, x.ID), []string{y.ID})
			assertStrings(t, "z's parents after the refusal", parentsOf(t, ctx, st, z.ID), []string{away.ID})
		})
	}
}
