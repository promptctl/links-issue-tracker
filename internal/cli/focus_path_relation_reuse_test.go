package cli

import (
	"context"
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// countingGraphSource wraps a focusGraphSource and records every subject id
// handed to GetRelationsByIDs across a whole walk (with multiplicity). It lets a
// test assert the focus-path walk's query behavior without inspecting code
// shape: which subjects reach the store, and how often. ListIssues passes
// straight through — only relation loads are the contract under test.
type countingGraphSource struct {
	inner   focusGraphSource
	fetched []string
}

func (c *countingGraphSource) ListIssues(ctx context.Context, filter store.ListIssuesFilter) ([]model.Issue, error) {
	return c.inner.ListIssues(ctx, filter)
}

func (c *countingGraphSource) GetRelationsByIDs(ctx context.Context, ids []string) (map[string]store.IssueRelations, error) {
	c.fetched = append(c.fetched, ids...)
	return c.inner.GetRelationsByIDs(ctx, ids)
}

func (c *countingGraphSource) fetchCounts() map[string]int {
	counts := make(map[string]int, len(c.fetched))
	for _, id := range c.fetched {
		counts[id]++
	}
	return counts
}

// The focus-path walk must reuse relations the listing pipeline already fetched
// and must never re-query a subject within a single walk. This is the
// query-count-shaped acceptance for links-query-efficiency-988d.2, observed
// behaviorally by counting the subject ids handed to GetRelationsByIDs:
//   - no subject is fetched more than once per walk (the per-walk memo dedups
//     subjects that recur across BFS levels — here the focused epic is both a
//     frontier subject and, a level later, a parent-epic subject), and
//   - subjects donated through the seed are never fetched at all (the listing
//     pipeline's relations are reused),
//
// while the derived path map is identical seeded vs unseeded — the seed is a
// transparent performance hint, not a behavior change.
func TestFocusPathWalkReusesFetchedRelations(t *testing.T) {
	h := newReadyTestHarness(t)

	// A focused epic with two children, the later depending on the earlier, gives
	// a multi-level walk: frontier [epic] -> children [c1, c2], whose parent-epic
	// fetch re-references the epic one level after it was first loaded.
	epic := h.createIssue(store.CreateIssueInput{Prefix: "test",
		Title: "Focused epic", Topic: "goal", IssueType: "epic",
	})
	c1 := h.createIssue(store.CreateIssueInput{Prefix: "test",
		Title: "Step 1", Topic: "goal", IssueType: "task", ParentID: epic.ID,
	})
	c2 := h.createIssue(store.CreateIssueInput{Prefix: "test",
		Title: "Step 2", Topic: "goal", IssueType: "task", ParentID: epic.ID,
	})
	h.addDependency(c2.ID, c1.ID)
	h.setLabels(epic.ID, FocusLabel)

	// Unseeded walk: the canonical result, and proof the memo fetches each
	// recurring subject at most once within one walk. Pre-memo, the epic was
	// fetched twice (frontier subject, then parent-epic subject a level later).
	bare := &countingGraphSource{inner: h.ap.Store}
	wantPath, err := fetchFocusPathGoals(h.ctx, bare)
	if err != nil {
		t.Fatalf("fetchFocusPathGoals(unseeded) error = %v", err)
	}
	for id, n := range bare.fetchCounts() {
		if n > 1 {
			t.Fatalf("subject %s fetched %d times in one walk; the per-walk memo must fetch each subject at most once", id, n)
		}
	}

	// Seeded walk: donate the relations the listing pipeline already holds for the
	// epic and its children. Every walk subject is covered, so none must reach the
	// store.
	seed, err := h.ap.Store.GetRelationsByIDs(h.ctx, []string{epic.ID, c1.ID, c2.ID})
	if err != nil {
		t.Fatalf("GetRelationsByIDs(seed) error = %v", err)
	}
	seeded := &countingGraphSource{inner: h.ap.Store}
	gotPath, err := fetchFocusPathGoals(h.ctx, seeded, seed)
	if err != nil {
		t.Fatalf("fetchFocusPathGoals(seeded) error = %v", err)
	}
	counts := seeded.fetchCounts()
	for id := range seed {
		if counts[id] != 0 {
			t.Fatalf("seeded subject %s re-fetched %d times; donated relations must be reused, not re-queried", id, counts[id])
		}
	}
	if len(seeded.fetched) != 0 {
		t.Fatalf("seeded walk fetched %v; with every subject donated the store must not be hit", seeded.fetched)
	}

	// Transparency: the seed changes which subjects are fetched, never the derived
	// path map.
	if !reflect.DeepEqual(wantPath, gotPath) {
		t.Fatalf("seeded path = %v, unseeded path = %v; the seed must not change the result", gotPath, wantPath)
	}
}
