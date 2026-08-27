package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
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

	parent, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
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

func TestRunNewAppendsByDefaultAndPromotesOnTopFlag(t *testing.T) {
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
	second := runCreate("Second")              // default: appends after First
	promoted := runCreate("Promoted", "--top") // explicit: jumps the queue

	issues, err := ap.Store.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	var got []string
	for _, is := range issues {
		got = append(got, is.ID)
	}
	want := []string{promoted, first, second}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rank order = %#v, want %#v (creation order by default, --top promoted to front)", got, want)
	}
}

// TestRunNewAppendsWithinItsFrame is the frame half of the placement contract:
// a child lands after its existing siblings, and does so without disturbing
// the rank order of the epics around it. The interesting case is the SECOND
// epic — its child is created last, so under a global-top default it would
// have displaced everything; the assertion is that both epics' children stack
// in creation order and each stays inside its own frame.
func TestRunNewAppendsWithinItsFrame(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	runCreate := func(title string, args ...string) string {
		t.Helper()
		var stdout bytes.Buffer
		base := []string{"--title", title, "--topic", "place"}
		if err := runNew(ctx, &stdout, ap, append(base, args...)); err != nil {
			t.Fatalf("runNew(%q) error = %v", title, err)
		}
		return firstIssueID(t, stdout.String())
	}

	epicA := runCreate("Epic A", "--type", "epic")
	epicB := runCreate("Epic B", "--type", "epic")
	a1 := runCreate("A one", "--type", "task", "--parent", epicA)
	a2 := runCreate("A two", "--type", "task", "--parent", epicA)
	b1 := runCreate("B one", "--type", "task", "--parent", epicB)

	rankOf := func(id string) string {
		t.Helper()
		issue, err := ap.Store.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", id, err)
		}
		return issue.Rank
	}

	if rankOf(a1) >= rankOf(a2) {
		t.Fatalf("A one rank %q >= A two rank %q; want the second child appended after the first", rankOf(a1), rankOf(a2))
	}
	// Epic B's child was created after both of A's, so a global-top default
	// would have sorted it above them; bottom placement keeps it last.
	if rankOf(a2) >= rankOf(b1) {
		t.Fatalf("A two rank %q >= B one rank %q; want the later-created child appended after the earlier ones", rankOf(a2), rankOf(b1))
	}
	// Children never reorder their epics: composite rank keys on the epic's
	// own rank, which no child create touches.
	if rankOf(epicA) >= rankOf(epicB) {
		t.Fatalf("Epic A rank %q >= Epic B rank %q; want the epics still in creation order after their children were filed", rankOf(epicA), rankOf(epicB))
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
