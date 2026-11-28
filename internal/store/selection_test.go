package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// selecting is the listing of what text selects, read against the real clock.
func selecting(t testing.TB, text string) storage.Listing {
	t.Helper()
	sel, err := storage.ParseSelector(text, time.Now())
	if err != nil {
		t.Fatalf("ParseSelector(%q) error = %v", text, err)
	}
	return storage.Listing{Selection: storage.Selection{Selector: sel}}
}

// A selection that reads an epic's state reads it from the walk hydration
// reads, and an epic on a stored loop has no state to read. So the selection
// refuses, naming the loop, wherever in it the state is read — and whether or
// not the looped epics are among what it selects, since every epic's state
// decides what it selects. A selection that reads no state has nothing the
// loop decides, and answers. The in-memory engine refuses a loop at the write,
// so this is the one engine that can hold one.
func TestASelectionReadingAStateRefusesAStoredLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := openIssueStore(t, ctx)
	loop := parentChain(t, ctx, st, "E", "F")
	parentChain(t, ctx, st, "Outside")
	seedParentEdge(t, ctx, st, loop[0].ID, loop[1].ID)

	for _, text := range []string{"status=open", "status!=closed", "title=Outside status=open", "children(status=open)", "/issues(status=closed)/count=0"} {
		_, err := st.SelectIssueIDs(ctx, selecting(t, text).Selection)
		var refusal storage.ParentCycleError
		if !errors.As(err, &refusal) {
			t.Errorf("SelectIssueIDs(%q) over a loop error = %v, want a ParentCycleError", text, err)
			continue
		}
		if !slices.Contains(refusal.Cycle, loop[0].ID) || !slices.Contains(refusal.Cycle, loop[1].ID) || len(refusal.Cycle) != 2 {
			t.Errorf("SelectIssueIDs(%q) refusal names %v, want exactly %s and %s", text, refusal.Cycle, loop[0].ID, loop[1].ID)
		}
	}
	ids, err := st.SelectIssueIDs(ctx, selecting(t, "title=Outside").Selection)
	if err != nil || len(ids) != 1 {
		t.Errorf("SelectIssueIDs(title=Outside) = %v, %v; want the one issue, since no state is read", ids, err)
	}
}
