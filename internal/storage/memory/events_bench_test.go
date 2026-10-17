package memory_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The sizes are this repo's performance envelope: 5x the largest backlog on
// this device (118 rows today, so 590 is the design target), the same pair the
// store and gather benchmarks use.
//
// The axis is how much history the REST of the store holds. One issue's
// history is read on every status transition, so its cost should track that
// issue's events alone, as the Dolt engine's indexed query does: the two
// benchmarks below report the same time per read.

func benchListEvents(b *testing.B, rows int) {
	engine := newEngine(b, storage.SystemClock)
	ctx := context.Background()
	epic, err := engine.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "bench", Title: "env epic", Topic: "env", IssueType: model.TypeEpic})
	if err != nil {
		b.Fatalf("CreateIssue(epic) error = %v", err)
	}
	ids := make([]string, 0, rows)
	for i := range rows {
		issue, err := engine.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "bench", Title: fmt.Sprintf("row %d", i), Topic: "env", ParentID: epic.ID})
		if err != nil {
			b.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		if _, err := engine.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
			b.Fatalf("Apply(start %d) error = %v", i, err)
		}
		ids = append(ids, issue.ID)
	}
	want, err := engine.ListEvents(ctx, ids[0])
	if err != nil {
		b.Fatalf("ListEvents error = %v", err)
	}
	b.ResetTimer()
	for range b.N {
		got, err := engine.ListEvents(ctx, ids[0])
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != len(want) {
			b.Fatalf("ListEvents returned %d events, want %d", len(got), len(want))
		}
	}
}

func BenchmarkListEvents590(b *testing.B) { benchListEvents(b, 590) }
func BenchmarkListEvents118(b *testing.B) { benchListEvents(b, 118) }
