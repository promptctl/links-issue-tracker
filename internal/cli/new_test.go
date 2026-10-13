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
// command's text summary (printIssueSummary prints "<id> [..] <title>"). Text
// is the sole surface, so a test that needs the created/updated issue extracts
// its ID here and re-reads the row from the store to assert fields the summary
// line doesn't carry. Child IDs carry a dotted hash suffix, so this reads the
// first field verbatim rather than validating against the flat-ID token shape.
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
	// The id hangs under the parent; the segment beneath it is a content hash,
	// not a position, so nothing asserts its value. [LAW:behavior-not-structure]
	if !strings.HasPrefix(createdID, parent.ID+".") {
		t.Fatalf("created.ID = %q, want it to hang under %q", createdID, parent.ID)
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

	// Epics: the top level is the frame whose whole order the listing shows,
	// and epics are what lives there.
	runCreate := func(title string, args ...string) string {
		t.Helper()
		var stdout bytes.Buffer
		base := []string{"--title", title, "--topic", "place", "--type", "epic"}
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
	// Children never reorder their epics: tree order places an epic by its
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
		"--title", "Born unclaimed", "--topic", "lifecycle", "--parent", seedEpicRaw(t, ctx, ap, "Lifecycle", "lifecycle"),
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
		"--title", "Pre-assigned", "--topic", "lifecycle", "--assignee", "alice", "--parent", seedEpicRaw(t, ctx, ap, "Lifecycle", "lifecycle"),
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

// TestRunNewLeafWithoutParentIsRefused pins the containment rule at the
// command surface: every ticket except an epic lives inside one, so `lit new`
// with the default type and no --parent is a validation refusal (exit 3, the
// same shape as any other refused value) whose message names the way in.
func TestRunNewLeafWithoutParentIsRefused(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var stdout bytes.Buffer
	err := runNew(ctx, &stdout, ap, []string{"--title", "Homeless", "--topic", "lifecycle"})
	if err == nil {
		t.Fatalf("runNew(no --parent) = nil, want a refusal; output:\n%s", stdout.String())
	}
	if code := ExitCode(err); code != ExitValidation {
		t.Fatalf("runNew(no --parent) exit code = %d, want %d (error: %v)", code, ExitValidation, err)
	}
	if reason := commandErrorReason(err); reason != "validation_refused" {
		t.Fatalf("runNew(no --parent) reason = %q, want validation_refused (error: %v)", reason, err)
	}
	want := "cannot create a task outside an epic: every ticket except an epic lives inside one, so give it the epic it belongs to as its parent, or make it an epic"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("runNew(no --parent) error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestRunNewEpicWithoutParentIsTopLevel is the other half of the rule: an epic
// is the one type that may live at the top level, so `--type epic` with no
// --parent creates it there.
func TestRunNewEpicWithoutParentIsTopLevel(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var stdout bytes.Buffer
	if err := runNew(ctx, &stdout, ap, []string{"--title", "A home", "--topic", "lifecycle", "--type", "epic"}); err != nil {
		t.Fatalf("runNew(--type epic, no --parent) error = %v", err)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.Issue.IssueType != "epic" {
		t.Fatalf("created.IssueType = %q, want epic", detail.Issue.IssueType)
	}
	if detail.Parent != nil {
		t.Fatalf("created parent = %s, want none: an epic may be top-level", detail.Parent.ID)
	}
}
