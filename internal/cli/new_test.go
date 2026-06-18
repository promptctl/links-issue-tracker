package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// firstIssueID returns the issue ID leading the first row of a mutation
// command's text summary (printIssueSummary prints "<id> [..] <title>"). With
// --json removed, text is the sole surface, so a test that needs the
// created/updated issue extracts its ID here and re-reads the row from the
// store to assert fields the summary line doesn't carry. Child IDs carry a
// ".<n>" suffix, so this reads the first field verbatim rather than validating
// against the flat-ID token shape.
func firstIssueID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			return fields[0]
		}
	}
	t.Fatalf("output has no issue ID row: %q", out)
	return ""
}

func TestRunNewSupportsTopicAndParent(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title:     "Renderer cleanup",
		Topic:     "renderer",
		IssueType: "epic",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runNew(ctx, &stdout, ap, []string{
		"--title", "Tighten repro",
		"--topic", "renderer",
		"--parent", parent.ID,
		"--type", "task",
		"--priority", "1",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}

	createdID := firstIssueID(t, stdout.String())
	if createdID != parent.ID+".1" {
		t.Fatalf("created.ID = %q, want %q", createdID, parent.ID+".1")
	}
	created, err := ap.Store.GetIssue(ctx, createdID)
	if err != nil {
		t.Fatalf("GetIssue(%s) error = %v", createdID, err)
	}
	if created.Topic != "renderer" {
		t.Fatalf("created.Topic = %q, want renderer", created.Topic)
	}
}

func TestRunNewRanksToTopByDefaultAndBottomOnFlag(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	runCreate := func(title string, args ...string) string {
		t.Helper()
		var stdout bytes.Buffer
		base := []string{"--title", title, "--topic", "place", "--type", "task"}
		if err := runNew(ctx, &stdout, ap, append(base, args...)); err != nil {
			t.Fatalf("runNew(%q) error = %v", title, err)
		}
		return firstIssueID(t, stdout.String())
	}

	first := runCreate("First")
	second := runCreate("Second")                 // default: jumps above First
	appended := runCreate("Appended", "--bottom") // explicit: lands at the bottom

	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	var got []string
	for _, is := range issues {
		got = append(got, is.ID)
	}
	want := []string{second, first, appended}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rank order = %#v, want %#v (newest-first default, --bottom appended last)", got, want)
	}
}

func TestRunQuickstartLoadsTemplateGuidance(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	templatePath := filepath.Join(ap.Workspace.RootDir, ".lit", "templates", "quickstart.md")
	if err := os.MkdirAll(filepath.Dir(templatePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(template dir) error = %v", err)
	}
	template := strings.Join([]string{
		"## Custom quickstart",
		"",
		"Use `lit ready`.",
		"",
	}, "\n")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runQuickstart(ctx, &stdout, ap.Workspace, nil); err != nil {
		t.Fatalf("runQuickstart() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "## Custom quickstart") {
		t.Fatalf("quickstart output missing template body: %q", output)
	}
}

func TestRunNewWithoutAssigneeCreatesUnassigned(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-creator")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	var stdout bytes.Buffer
	if err := runNew(ctx, &stdout, ap, []string{
		"--title", "Born unclaimed", "--topic", "lifecycle",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}
	created, err := ap.Store.GetIssue(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	// open means unclaimed: creation is not a claim, so the session identity
	// must not be stamped onto a ticket nobody started. [LAW:one-source-of-truth]
	if got := created.AssigneeValue(); got != "" {
		t.Fatalf("created.AssigneeValue() = %q, want empty: lit new must not self-assign from session env", got)
	}
}

func TestRunNewExplicitAssigneeHonoredVerbatim(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-creator")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	var stdout bytes.Buffer
	if err := runNew(ctx, &stdout, ap, []string{
		"--title", "Pre-assigned", "--topic", "lifecycle", "--assignee", "alice",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}
	created, err := ap.Store.GetIssue(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got, want := created.AssigneeValue(), "alice"; got != want {
		t.Fatalf("created.AssigneeValue() = %q, want %q: explicit assignee must not be rewritten to the caller", got, want)
	}
}
