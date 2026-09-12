package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// statusSetFixture seeds one issue in each of the three states and returns
// their ids, so a listing's membership names which buckets it actually spanned.
func statusSetFixture(t *testing.T) (h readyTestHarness, openID, inProgressID, closedID string) {
	t.Helper()
	h = newReadyTestHarness(t)
	open := h.createIssue(storage.CreateIssueInput{Title: "still open", IssueType: model.TypeBug, Topic: "filtering"})
	started := h.createIssue(storage.CreateIssueInput{Title: "being worked", IssueType: model.TypeBug, Topic: "filtering"})
	closed := h.createIssue(storage.CreateIssueInput{Title: "finished", IssueType: model.TypeBug, Topic: "filtering"})
	if _, err := h.ap.Store.Apply(h.ctx, started.ID, storage.Change{
		Action: model.Start{}, Actor: "tester", Reason: "picking it up",
	}); err != nil {
		t.Fatalf("Apply(start) error = %v", err)
	}
	if _, err := h.ap.Store.Apply(h.ctx, closed.ID, storage.Change{
		Action: model.Done{}, Actor: "tester", Reason: "shipped",
	}); err != nil {
		t.Fatalf("Apply(done) error = %v", err)
	}
	return h, open.ID, started.ID, closed.ID
}

// TestListStatusSetSpansBucketsInOneListing is the links-listing-09m5
// acceptance. storage.ListIssuesFilter.Statuses is a set both engines already
// OR together; this pins that every spelling the CLI offers can actually reach
// that set, and that they all reach the SAME one.
//
// The repeated-flag row is the one that fails loudest on a regression: while
// `--status` was a plain string flag, a second occurrence overwrote the first,
// so `--status closed --status in_progress` answered with closed work only and
// said nothing about the half it dropped. [LAW:no-silent-failure]
func TestListStatusSetSpansBucketsInOneListing(t *testing.T) {
	h, openID, inProgressID, closedID := statusSetFixture(t)

	spellings := []struct {
		name string
		args []string
	}{
		{"flag, comma-joined", []string{"--status", "closed,in_progress"}},
		{"flag, repeated", []string{"--status", "closed", "--status", "in_progress"}},
		{"query, comma-joined", []string{"--query", "status:closed,in_progress"}},
		{"query, repeated terms", []string{"--query", "status:closed status:in_progress"}},
		{"flag and query together", []string{"--status", "closed", "--query", "status:in_progress"}},
	}
	for _, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			got := runLs(t, h.ap, sp.args...)
			if !strings.Contains(got, closedID) {
				t.Fatalf("lit ls %v omitted the closed issue %q — the set did not widen; output:\n%s", sp.args, closedID, got)
			}
			if !strings.Contains(got, inProgressID) {
				t.Fatalf("lit ls %v omitted the in_progress issue %q — the set did not widen; output:\n%s", sp.args, inProgressID, got)
			}
			// The union is still a narrowing: naming two states must not quietly
			// become "no filter" and hand back everything.
			if strings.Contains(got, openID) {
				t.Fatalf("lit ls %v leaked the open issue %q — the filter stopped narrowing; output:\n%s", sp.args, openID, got)
			}
		})
	}
}

// TestListStatusSetRejectsABadMemberLoudly pins that widening did not buy
// leniency. A typo anywhere in the set, and an empty value in either grammar,
// must fail before a single row prints — a partial or defaulted listing here
// looks exactly like a real answer to the question the caller asked.
// [LAW:no-silent-failure]
func TestListStatusSetRejectsABadMemberLoudly(t *testing.T) {
	h, _, _, _ := statusSetFixture(t)

	for _, args := range [][]string{
		{"--status", "closed,todo"},
		{"--status", "todo,closed"},
		{"--status", "closed", "--status", "todo"},
		{"--status", ""},
		{"--status", "closed,"},
		{"--query", "status:closed,todo"},
		{"--query", "status:"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out strings.Builder
			err := runListWithStore(h.ctx, &out, h.ap.Store, noReadyPolicy, args)
			if err == nil {
				t.Fatalf("lit ls %v returned no error; output:\n%s", args, out.String())
			}
			if out.Len() != 0 {
				t.Fatalf("lit ls %v printed %q before rejecting — a rejection that already emitted rows is the partial answer this boundary exists to prevent", args, out.String())
			}
		})
	}
}
