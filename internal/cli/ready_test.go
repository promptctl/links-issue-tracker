package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func newTestCLIApp(t *testing.T) *app.App {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return &app.App{Store: st}
}

func TestRunReadyReturnsOnlyOpenIssues(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	openA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Open issue A",
		IssueType: "task",
		Priority:  2,
		Assignee:  "alice",
	})
	if err != nil {
		t.Fatalf("CreateIssue(openA) error = %v", err)
	}
	openB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Open issue B",
		IssueType: "bug",
		Priority:  1,
		Assignee:  "bob",
	})
	if err != nil {
		t.Fatalf("CreateIssue(openB) error = %v", err)
	}

	closed, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Already done",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(closed) error = %v", err)
	}
	if _, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID:   closed.ID,
		Action:    "close",
		Reason:    "not ready work",
		CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("TransitionIssue(close) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, []string{"--json"}); err != nil {
		t.Fatalf("runReady(--json) error = %v", err)
	}

	var got []model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got=%#v", len(got), got)
	}
	want := map[string]struct{}{
		openA.ID: {},
		openB.ID: {},
	}
	for _, issue := range got {
		if _, ok := want[issue.ID]; !ok {
			t.Fatalf("unexpected issue in ready output: %q (all=%#v)", issue.ID, got)
		}
		if issue.Status != "open" {
			t.Fatalf("ready output included non-open issue: %#v", issue)
		}
	}
}

func TestRunReadySupportsAssigneeAndLimit(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Alice old",
		IssueType: "task",
		Priority:  1,
		Assignee:  "alice",
	}); err != nil {
		t.Fatalf("CreateIssue(alice old) error = %v", err)
	}
	if _, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:     "Bob task",
		IssueType: "task",
		Priority:  0,
		Assignee:  "bob",
	}); err != nil {
		t.Fatalf("CreateIssue(bob) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runReady(ctx, &stdout, ap, []string{"--assignee", "alice", "--limit", "1", "--json"}); err != nil {
		t.Fatalf("runReady(--assignee --limit --json) error = %v", err)
	}

	var got []model.Issue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(ready output) error = %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1; got=%#v", len(got), got)
	}
	if got[0].Assignee != "alice" {
		t.Fatalf("got[0].Assignee = %q, want alice", got[0].Assignee)
	}
}
