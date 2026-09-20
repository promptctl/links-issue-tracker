package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Hydration's two id-keyed reads must cover every id, not only the first batch.
//
// hydrateIssues loads labels for every row it hydrates, and children for every
// container among them. Both were unbounded `IN (...)` lists until they were
// batched, and batching them introduces the same failure the relation gather
// has: a row whose labels were never queried is indistinguishable at the call
// site from one that genuinely has none, and a container whose children were
// never queried hydrates as though it were empty. Neither errors, and the
// listing simply shows less than is there.
//
// The assertion is over every epic rather than over chosen positions. The ids
// reach hydrateIssues from a map, so which batch any one of them lands in is
// not something this test can arrange, and "all of them" is the property that
// does not depend on the arrangement.
//
// Each child is closed, and that is load-bearing rather than incidental. A
// container composes its children's lifecycles, so an epic holding one open
// child and an epic holding none both report open, and an assertion written
// against open children passes in exactly the world it is meant to catch. One
// closed child makes the container closed, which nothing but a loaded child
// can produce.
//
// The childless epic is the other half of that control: it pins that the state
// being compared against is the one an unloaded container would have fallen
// back to, so "every epic differs from empty" cannot be satisfied vacuously.
func TestHydrationReadsCoverEveryBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const epicCount = 2*idBatchSize + 2
	const label = "spans"

	epics := make([]string, 0, epicCount)
	for i := range epicCount {
		epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("Epic %d", i), Topic: "hydrate", IssueType: "epic",
		})
		if err != nil {
			t.Fatalf("CreateIssue(epic %d) error = %v", i, err)
		}
		child, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("Child %d", i), Topic: "hydrate", IssueType: "task",
			ParentID: epic.ID, Placement: storage.RankBottom,
		})
		if err != nil {
			t.Fatalf("CreateIssue(child %d) error = %v", i, err)
		}
		if _, err := st.Apply(ctx, child.ID, storage.Change{Action: model.Close{Outcome: model.Wontfix{}}, Actor: "test", Reason: "hydration fixture"}); err != nil {
			t.Fatalf("Apply(close child %d) error = %v", i, err)
		}
		if _, err := st.AddLabel(ctx, storage.AddLabelInput{IssueID: epic.ID, Name: label, CreatedBy: "test"}); err != nil {
			t.Fatalf("AddLabel(epic %d) error = %v", i, err)
		}
		epics = append(epics, epic.ID)
	}

	childless, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Childless", Topic: "hydrate", IssueType: "epic",
	})
	if err != nil {
		t.Fatalf("CreateIssue(childless) error = %v", err)
	}

	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	byID := make(map[string]int, len(issues))
	for i, issue := range issues {
		byID[issue.ID] = i
	}

	at, ok := byID[childless.ID]
	if !ok {
		t.Fatalf("the childless epic is missing from the listing")
	}
	empty := issues[at].State()

	seen := 0
	for _, id := range epics {
		i, ok := byID[id]
		if !ok {
			t.Fatalf("epic %s is missing from the listing", id)
		}
		got := issues[i]
		if len(got.Labels) != 1 || got.Labels[0] != label {
			t.Fatalf("epic %s (%d of %d) Labels = %v, want [%s] — a label batch went unqueried",
				id, seen, epicCount, got.Labels, label)
		}
		if got.State() == empty {
			t.Fatalf("epic %s (%d of %d) hydrated to %q, the state a childless epic has — a children batch went unqueried",
				id, seen, epicCount, got.State())
		}
		if got.State() != model.StateClosed {
			t.Fatalf("epic %s (%d of %d) hydrated to %q, want closed — its one child is closed, so any other state means the children it composed were not the ones wired",
				id, seen, epicCount, got.State())
		}
		seen++
	}
	if seen != epicCount {
		t.Fatalf("checked %d epics, want %d", seen, epicCount)
	}
}
