package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// TestListQuerySupersetMatchesDiscreteFlags is the kkew.2 acceptance at the CLI
// surface: the discrete flag form and the --query token form of the four new
// list options (sort/limit/archived/deleted) must render byte-identical output.
// Struct-level parity lives in the query package; this test proves the two
// grammars reach ListIssues with the same intent end to end, and that the flags
// actually do something (the archived/deleted issues are hidden by default and
// surfaced by both forms). [LAW:behavior-not-structure]
func TestListQuerySupersetMatchesDiscreteFlags(t *testing.T) {
	h := newReadyTestHarness(t)
	// Three plainly-visible issues plus one archived and one deleted, so every
	// dimension has an observable effect: sort orders them, limit truncates,
	// and the visibility flags decide whether the last two appear at all.
	a := h.createIssue(store.CreateIssueInput{Title: "alpha", IssueType: model.TypeTask, Topic: "sup"})
	b := h.createIssue(store.CreateIssueInput{Title: "bravo", IssueType: model.TypeTask, Topic: "sup"})
	c := h.createIssue(store.CreateIssueInput{Title: "charlie", IssueType: model.TypeTask, Topic: "sup"})
	arch := h.createIssue(store.CreateIssueInput{Title: "archived one", IssueType: model.TypeTask, Topic: "sup"})
	del := h.createIssue(store.CreateIssueInput{Title: "deleted one", IssueType: model.TypeTask, Topic: "sup"})
	if _, err := h.ap.Store.Apply(h.ctx, arch.ID, store.Change{Action: model.Archive{}, Actor: "tester", Reason: "shelve"}); err != nil {
		t.Fatalf("Apply(archive) error = %v", err)
	}
	if _, err := h.ap.Store.Apply(h.ctx, del.ID, store.Change{Action: model.Delete{}, Actor: "tester", Reason: "drop"}); err != nil {
		t.Fatalf("Apply(delete) error = %v", err)
	}

	flagForm := runLs(t, h.ap, "--sort", "rank:asc", "--limit", "10", "--include-archived", "--include-deleted")
	queryForm := runLs(t, h.ap, "--query", "sort:rank:asc limit:10 archived deleted")
	if flagForm != queryForm {
		t.Fatalf("flag form and query form diverged:\n--- flags ---\n%s\n--- query ---\n%s", flagForm, queryForm)
	}

	// Both forms must actually surface the archived and deleted issues; otherwise
	// identical-but-empty output would pass vacuously.
	for _, id := range []string{arch.ID, del.ID} {
		if !strings.Contains(queryForm, id) {
			t.Fatalf("query form omitted included issue %q; output:\n%s", id, queryForm)
		}
	}

	// Baseline: without the visibility tokens the archived and deleted issues are
	// hidden, proving the tokens are what surfaced them.
	bare := runLs(t, h.ap)
	for _, id := range []string{a.ID, b.ID, c.ID} {
		if !strings.Contains(bare, id) {
			t.Fatalf("bare ls omitted active issue %q; output:\n%s", id, bare)
		}
	}
	for _, id := range []string{arch.ID, del.ID} {
		if strings.Contains(bare, id) {
			t.Fatalf("bare ls leaked hidden issue %q; output:\n%s", id, bare)
		}
	}
}
