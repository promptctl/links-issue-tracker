package memory

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestCreateIssueLeavesNoRecordWhenPlacementFails pins that a creation which
// cannot be placed leaves nothing behind.
//
// Placement became fallible when it started dispatching on RankPlacement, and
// the record used to be committed to e.issues before place ran. A failure in
// between stranded the record in e.issues while absent from e.order: findable
// by GetIssue, hydrated through a missing pos key, and so reported at a
// fabricated rank of "000000000" that collides with whichever issue genuinely
// holds the first slot. A fabricated rank is worse than a refused creation,
// because nothing about it looks wrong. [LAW:no-silent-failure]
//
// This test is white-box on purpose, and the reason is the defect's shape. The
// two fields are one fact kept twice — e.order is THE rank order and e.issues
// is what may appear in it — so the orphan IS the divergence, and divergence is
// what there is to assert. [LAW:one-source-of-truth] Every contract read
// projects through e.order, so the orphan is invisible from outside the
// package: the earlier black-box form of this test, which asserted over
// ListIssues and GetIssue, was confirmed to pass with the defect reintroduced.
// Assurance that survives its own defect is worse than no test, so the
// assertion moved to the altitude the divergence actually lives at.
//
// The unrecognized placement is the one input that reaches place's error arm,
// and it is the same input the engine's own dispatch rejects.
// TestCreateIssueRefusesAnUnknownPlacementInAnEmptyWorkspace pins that the
// placement is judged before the workspace is consulted.
//
// place used to answer the empty order first and append unconditionally, which
// meant the dispatch that rejects an unrecognized placement never ran for the
// very first issue: the same call that is refused once a second issue exists
// was accepted as the first, and the workspace a caller happened to be pointed
// at decided whether its input was valid. The population is a question about
// where an issue lands, never about whether the request makes sense.
//
// It is deliberately the sibling of the test below rather than another case
// inside it: that one always creates an anchor first, so e.order is never empty
// by the time it makes its bad-placement call, and it cannot reach this arm.
func TestCreateIssueRefusesAnUnknownPlacementInAnEmptyWorkspace(t *testing.T) {
	ctx := context.Background()
	engine, err := New("memory-create-placement-empty", storage.SystemClock)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("Close error = %v", err)
		}
	})

	if _, err := engine.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "mem", Title: "first", Topic: "place", IssueType: "task",
		Placement: storage.RankPlacement(99),
	}); err == nil {
		t.Fatal("the first issue in a workspace was created with an unrecognized placement; want the same refusal the second would get")
	}
	if len(engine.issues) != 0 || len(engine.order) != 0 {
		t.Errorf("a refused creation left %d records and %d positions behind, want none", len(engine.issues), len(engine.order))
	}
}

func TestCreateIssueLeavesNoRecordWhenPlacementFails(t *testing.T) {
	ctx := context.Background()
	engine, err := New("memory-create-placement", storage.SystemClock)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("Close error = %v", err)
		}
	})

	if _, err := engine.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "mem", Title: "anchor", Topic: "place", IssueType: "task",
	}); err != nil {
		t.Fatalf("CreateIssue(anchor) error = %v", err)
	}
	issuesBefore, orderBefore := len(engine.issues), len(engine.order)

	// An unrecognized placement fails the dispatch inside place.
	if _, err := engine.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "mem", Title: "doomed", Topic: "place", IssueType: "task",
		Placement: storage.RankPlacement(99),
	}); err == nil {
		t.Fatal("CreateIssue with an unrecognized placement succeeded; want an error")
	}

	if got := len(engine.issues); got != issuesBefore {
		t.Errorf("after a refused creation the engine holds %d records, want %d — the failed creation was committed", got, issuesBefore)
	}
	if got := len(engine.order); got != orderBefore {
		t.Errorf("after a refused creation the order holds %d positions, want %d", got, orderBefore)
	}
	// The invariant itself, stated once: nothing is in e.issues that e.order
	// cannot place, whatever the counts above happen to be.
	if len(engine.issues) != len(engine.order) {
		t.Errorf("%d records against %d positions: a record with no position hydrates at a rank it does not hold", len(engine.issues), len(engine.order))
	}
}
