package cli

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// annotateIssues annotates the subjects as its own relations fetch returned
// them, not as the caller handed them in. Every annotator reads its relations
// out of that fetch, so annotating the caller's copies would classify one
// snapshot of an issue against another snapshot's edges — and hand back rows
// whose Issue is older than the details returned beside them.
//
// Driven by a caller copy that is deliberately stale rather than by a race:
// the contract is "the row describes what the store says now", and a stale
// input is that contract's observable case without timing to arrange.
func TestAnnotateIssuesRowsComeFromTheRelationsFetch(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	created, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Current title", Topic: "snapshot", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	// The caller's copy, as an earlier read would have left it.
	stale := created
	stale.Title = "Title from an older read"

	annotated, details, err := annotateIssues(ctx, ap.Store, nil, []model.Issue{stale})
	if err != nil {
		t.Fatalf("annotateIssues() error = %v", err)
	}
	if len(annotated) != 1 {
		t.Fatalf("annotateIssues() returned %d rows, want 1", len(annotated))
	}
	if got := annotated[0].Issue.Title; got != "Current title" {
		t.Errorf("row must carry the fetched issue, got title %q, want %q", got, "Current title")
	}
	// The row and the details returned beside it must be the same snapshot —
	// that is what lets a caller render from either without them disagreeing.
	if got, want := annotated[0].Issue.Title, details[created.ID].Issue.Title; got != want {
		t.Errorf("row and details disagree: row title %q, details title %q", got, want)
	}
}
