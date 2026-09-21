package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The sizes are this repo's performance envelope: 5x the largest backlog on
// this device (118 rows today, so 590 is the design target), the same pair the
// gather benchmark in internal/cli uses.
//
// These pin the set-membership filters that take a caller-supplied id list.
// Each element of an `IN (...)` list becomes an index range the planner merges
// pairwise before a row is read, so the cost of one clause grows with the
// SQUARE of the list — which is why the interesting axis here is the number of
// ids and not the number of rows they select.

func benchIssueIDs(b *testing.B, rows int) (*Store, context.Context, []string) {
	b.Helper()
	ctx := context.Background()
	st := openIssueStore(b, ctx)
	ids := make([]string, 0, rows)
	for i := range rows {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("row %d", i), Topic: "env",
			IssueType: "task", Placement: storage.RankBottom,
		})
		if err != nil {
			b.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		ids = append(ids, issue.ID)
	}
	return st, ctx, ids
}

func benchListByIDs(b *testing.B, rows int) {
	st, ctx, ids := benchIssueIDs(b, rows)
	b.ResetTimer()
	for range b.N {
		got, err := st.ListIssues(ctx, storage.ListIssuesFilter{IDs: ids})
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != len(ids) {
			b.Fatalf("ListIssues(%d ids) returned %d rows, want %d", len(ids), len(got), len(ids))
		}
	}
}

func BenchmarkListByIDs590(b *testing.B) { benchListByIDs(b, 590) }
func BenchmarkListByIDs118(b *testing.B) { benchListByIDs(b, 118) }

// The parent filter's axis is the number of PARENTS named, so each parent here
// carries exactly one child: the id list the filter builds is as long as the
// row set is small, which is the case the EXISTS subquery planned worst.
func benchListByParents(b *testing.B, parents int) {
	ctx := context.Background()
	st := openIssueStore(b, ctx)
	parentIDs := make([]string, 0, parents)
	for i := range parents {
		parent, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("parent %d", i), Topic: "env",
			IssueType: "task", Placement: storage.RankBottom,
		})
		if err != nil {
			b.Fatalf("CreateIssue(parent %d) error = %v", i, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("child %d", i), Topic: "env",
			IssueType: "task", Placement: storage.RankBottom, ParentID: parent.ID,
		}); err != nil {
			b.Fatalf("CreateIssue(child %d) error = %v", i, err)
		}
		parentIDs = append(parentIDs, parent.ID)
	}
	b.ResetTimer()
	for range b.N {
		got, err := st.ListIssues(ctx, storage.ListIssuesFilter{ParentIDs: parentIDs})
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != parents {
			b.Fatalf("ListIssues(%d parents) returned %d rows, want %d", parents, len(got), parents)
		}
	}
}

func BenchmarkListByParents590(b *testing.B) { benchListByParents(b, 590) }
func BenchmarkListByParents118(b *testing.B) { benchListByParents(b, 118) }

// RankSet is the only caller that hands the two frame-edge reads an unbounded
// exclusion set, and it hands the same set to both. It is a write, so a commit
// sits inside the measurement; the exclusion's cost has to clear that to be
// worth anything, which is exactly the question.
func benchRankSet(b *testing.B, rows int) {
	st, ctx, ids := benchIssueIDs(b, rows)
	b.ResetTimer()
	for range b.N {
		if _, err := st.RankSet(ctx, ids); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRankSet590(b *testing.B) { benchRankSet(b, 590) }
func BenchmarkRankSet118(b *testing.B) { benchRankSet(b, 118) }
