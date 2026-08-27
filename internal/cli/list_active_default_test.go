package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// runLs drives the shared list logic against the app's store and returns its
// rendered lines. It calls runListWithStore directly (the store-choosing runList
// entrypoint routes cwd-vs-`--at`); these tests exercise the query itself.
func runLs(t *testing.T, ap *app.App, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := runListWithStore(context.Background(), &buf, ap.Store, args); err != nil {
		t.Fatalf("runListWithStore(%v) error = %v", args, err)
	}
	return buf.String()
}

// TestListClosedOnlyFilterIsNotSilentlyEmptied is the kkew.3 acceptance: an
// explicit filter that only matches closed issues (resolution:wontfix) must
// return those issues, not an empty list, while a bare `lit ls` still shows
// only active work. [LAW:no-silent-failure]
func TestListClosedOnlyFilterIsNotSilentlyEmptied(t *testing.T) {
	h := newReadyTestHarness(t)
	openBug := h.createIssue(storage.CreateIssueInput{Title: "still open", IssueType: model.TypeBug, Topic: "filtering"})
	wontfixBug := h.createIssue(storage.CreateIssueInput{Title: "declined bug", IssueType: model.TypeBug, Topic: "filtering"})
	if _, err := h.ap.Store.Apply(h.ctx, wontfixBug.ID, storage.Change{
		Action: model.Close{Outcome: model.Wontfix{}},
		Actor:  "tester",
		Reason: "not doing it",
	}); err != nil {
		t.Fatalf("Apply(close wontfix) error = %v", err)
	}
	ap := h.ap

	// The closed-only filter must surface the wontfix-closed issue.
	got := runLs(t, ap, "--query", "resolution:wontfix")
	if !strings.Contains(got, wontfixBug.ID) {
		t.Fatalf("ls --query resolution:wontfix omitted the wontfix issue %q; output:\n%s", wontfixBug.ID, got)
	}
	if strings.Contains(got, openBug.ID) {
		t.Fatalf("ls --query resolution:wontfix leaked the open issue %q; output:\n%s", openBug.ID, got)
	}

	// A bare `lit ls` still shows only active work — the wontfix issue is hidden.
	bare := runLs(t, ap)
	if !strings.Contains(bare, openBug.ID) {
		t.Fatalf("bare ls omitted the open issue %q; output:\n%s", openBug.ID, bare)
	}
	if strings.Contains(bare, wontfixBug.ID) {
		t.Fatalf("bare ls leaked the closed wontfix issue %q; the active-work default must still hide it; output:\n%s", wontfixBug.ID, bare)
	}
}
