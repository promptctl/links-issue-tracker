package memory_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestConcurrentUseIsSerialized proves the engine is safe to share, which is a
// claim its package doc makes and which the conformance suite — sequential by
// construction — cannot check.
//
// The discipline behind the claim is structural: every exported method takes
// the lock and delegates to an unexported one, and no unexported method ever
// takes it. That is what lets a batch drive creates and applies under a single
// hold without deadlocking on itself, and it is also what a future method
// could quietly break by doing its own work behind an exported name. Under
// `-race` this test is where that shows up. [LAW:verifiable-goals]
func TestConcurrentUseIsSerialized(t *testing.T) {
	t.Parallel()
	engine := newEngine(t)
	ctx := context.Background()

	const writers = 8
	const perWriter = 6
	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				issue, err := engine.CreateIssue(ctx, storage.CreateIssueInput{
					Title:  fmt.Sprintf("writer %d item %d", writer, i),
					Topic:  "core",
					Prefix: "conf",
				})
				if err != nil {
					t.Errorf("CreateIssue error = %v", err)
					return
				}
				if _, err := engine.Apply(ctx, issue.ID, storage.Change{
					Action: model.Start{Assignee: "ada"}, Actor: "ada",
				}); err != nil {
					t.Errorf("Apply error = %v", err)
					return
				}
				if _, err := engine.ListIssues(ctx, storage.ListIssuesFilter{}); err != nil {
					t.Errorf("ListIssues error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Every write landed exactly once: the rank order and the issue table are
	// two views of one set, and a lost or doubled update would show as a
	// disagreement between them.
	count, err := engine.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount error = %v", err)
	}
	if want := int64(writers * perWriter); count != want {
		t.Errorf("LocalIssueCount = %d, want %d", count, want)
	}
	listed, err := engine.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues error = %v", err)
	}
	if int64(len(listed)) != count {
		t.Errorf("listing holds %d issues, the store holds %d", len(listed), count)
	}
	ranks := map[string]struct{}{}
	for _, issue := range listed {
		if _, dup := ranks[issue.Rank]; dup {
			t.Fatalf("two issues share rank %q; the order is not a total one", issue.Rank)
		}
		ranks[issue.Rank] = struct{}{}
	}
}
