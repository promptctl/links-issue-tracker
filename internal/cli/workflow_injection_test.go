package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// writeProjectWorkflow authors a workflow definition file under the app's
// workspace at .lit/workflows/<rel>, creating parent directories as needed —
// the arbitrary-nested-path project layer runTransition (via
// workflows.Dispatch) reads from.
func writeProjectWorkflow(t *testing.T, ap *app.App, rel, content string) {
	t.Helper()
	path := filepath.Join(ap.Workspace.RootDir, ".lit", "workflows", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// TestRunTransitionInjectsProjectWorkflowFromArbitraryNestedPath is the
// ticket's end-to-end done-claim: a project workflow file, authored under a
// nested path with no meaning of its own, fires through the real command path
// (not just workflows.Dispatch called directly) and its body reaches stdout.
func TestRunTransitionInjectsProjectWorkflowFromArbitraryNestedPath(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	writeProjectWorkflow(t, ap, "reviews/deep/nested/close.md", "---\nevents: [ticket_closed]\n---\nTicket <id> closed without finishing — check for a duplicate.")

	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Injection test", Topic: "workflows", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID, "--resolution", "wontfix"}, closeSpec); err != nil {
		t.Fatalf("runTransition(close) error = %v", err)
	}
	want := "Ticket " + issue.ID + " closed without finishing — check for a duplicate."
	if got := stdout.String(); !bytes.Contains([]byte(got), []byte(want)) {
		t.Fatalf("runTransition(close) stdout = %q, want it to contain %q", got, want)
	}
}

// TestRunTransitionProjectLayerOverridesEmbeddedDoneByID is the ticket's other
// explicit done-claim: a project-layer definition with id "done" replaces the
// embedded default of the same id, end to end through `lit done`.
func TestRunTransitionProjectLayerOverridesEmbeddedDoneByID(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	writeProjectWorkflow(t, ap, "done.md", "---\nid: done\nevents: [work_finished]\n---\nCUSTOM: <id> wrapped up.")

	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Override test", Topic: "workflows", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := ap.Store.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("StartIssue() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID}, doneSpec); err != nil {
		t.Fatalf("runTransition(done) error = %v", err)
	}
	got := stdout.String()
	want := "CUSTOM: " + issue.ID + " wrapped up."
	if !bytes.Contains([]byte(got), []byte(want)) {
		t.Fatalf("runTransition(done) stdout = %q, want the project override %q", got, want)
	}
	if bytes.Contains([]byte(got), []byte("has been closed")) {
		t.Fatalf("runTransition(done) stdout = %q, want the embedded default's body suppressed by the override", got)
	}
}
