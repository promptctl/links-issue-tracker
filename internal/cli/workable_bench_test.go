package cli

import (
	"context"
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The workable gather is what every `lit backlog` and `lit next` pays before it
// prints anything, so it is the one stage whose cost is worth pinning.
//
// The sizes are the repo's performance envelope: 5x the largest backlog on this
// device. `lit stores --counts` is how that number is found cheaply — it was 118
// workable rows, so 590 is the design target and 118 is today.
//
// What these measure, and why both filters are here: the gather reads the whole
// workable queue because the facts a narrowed view still has to state truthfully
// — what closing a row releases, how many rank inversions the repo holds — are
// properties of the queue. So the filtered and unfiltered costs are expected to
// be EQUAL, and a filtered run that is much cheaper means a fact is being
// derived from a narrowed set again (links-listing-85sd).
func benchGather(b *testing.B, rows int, rf workableFilter) {
	h := backlogTestHarness{t: &testing.T{}, ctx: context.Background(), ap: newTestCLIApp(&testing.T{})}
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "env", IssueType: "epic"})
	gate := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Gate", Topic: "env", IssueType: "task"})
	h.addDependency(epic, gate)
	for i := 0; i < rows; i++ {
		kind := model.IssueType("task")
		if i%3 == 0 {
			kind = model.IssueType("bug")
		}
		h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("row %d", i), Topic: "env", IssueType: kind, ParentID: epic})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := gatherWorkableAnnotated(h.ctx, h.ap, rf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGather590Unfiltered(b *testing.B) { benchGather(b, 590, workableFilter{}) }
func BenchmarkGather590TypeBug(b *testing.B) {
	benchGather(b, 590, workableFilter{IssueType: model.IssueType("bug")})
}

func BenchmarkGather118Unfiltered(b *testing.B) { benchGather(b, 118, workableFilter{}) }
func BenchmarkGather118TypeBug(b *testing.B) {
	benchGather(b, 118, workableFilter{IssueType: model.IssueType("bug")})
}
