package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRunHistoryPrintsMultiEditTransitionTrail exercises `lit history` on a
// ticket edited several times through the real CLI update path. The trail must
// carry every field-level `from → to` transition those edits produced — the
// full history `lit show` deliberately does not render. This is a behavioral
// test [LAW:behavior-not-structure]: it drives genuine edits and asserts on the
// transitions a reader sees, not on how the events are stored.
func TestRunHistoryPrintsMultiEditTransitionTrail(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Original title", Topic: "history", IssueType: "task", Priority: 0,
		ParentID: seedEpicRaw(t, ctx, ap, "History", "history"),
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	// Three real edits: two title renames plus a priority bump, so the trail has
	// several distinct field transitions to render.
	edits := [][]string{
		{issue.ID, "--title", "Renamed once", "--by", "editor"},
		{issue.ID, "--title", "Renamed twice", "--by", "editor"},
		{issue.ID, "--priority", "1", "--by", "editor"},
	}
	for _, args := range edits {
		if err := runUpdate(ctx, &bytes.Buffer{}, ap, args); err != nil {
			t.Fatalf("runUpdate(%v) error = %v", args, err)
		}
	}

	var stdout bytes.Buffer
	if err := runHistory(ctx, &stdout, ap, []string{issue.ID}); err != nil {
		t.Fatalf("runHistory() error = %v", err)
	}
	out := stdout.String()

	// Header identifies the ticket by id + final title, then the labelled trail.
	for _, want := range []string{issue.ID, "Renamed twice", "\nhistory:\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lit history output missing %q in:\n%s", want, out)
		}
	}

	// Every edit's field transition appears as a `field: from → to` line.
	wantTransitions := []string{
		"title: Original title → Renamed once",
		"title: Renamed once → Renamed twice",
		"priority: 0 → 1",
	}
	for _, want := range wantTransitions {
		if !strings.Contains(out, want) {
			t.Fatalf("lit history output missing transition %q in:\n%s", want, out)
		}
	}
	// No prose field changed, so the trail is the whole output: it ends at the
	// last change it records.
	if !strings.HasSuffix(out, "    priority: 0 → 1\n") {
		t.Fatalf("lit history output does not end at its last transition:\n%s", out)
	}
}

// seedProseHistory creates a ticket and edits its prose fields through the
// real update path: the description three times (so it holds versions 1–3,
// one of them multi-line), the prompt once, and the title once beside them.
func seedProseHistory(t *testing.T, ctx context.Context, ap *app.App) string {
	t.Helper()
	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Original title", Description: "first draft", Prompt: "old prompt",
		Topic: "history", IssueType: "task", Priority: 0,
		ParentID: seedEpicRaw(t, ctx, ap, "History", "history"),
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	edits := [][]string{
		{issue.ID, "--description", "second draft\nline two", "--by", "editor"},
		{issue.ID, "--description", "third draft", "--title", "Renamed", "--by", "editor"},
		{issue.ID, "--prompt", "new prompt", "--by", "editor"},
	}
	for _, args := range edits {
		if err := runUpdate(ctx, &bytes.Buffer{}, ap, args); err != nil {
			t.Fatalf("runUpdate(%v) error = %v", args, err)
		}
	}
	return issue.ID
}

// TestRunHistoryShowsProseChangesAsVersions is the trail an agent reads: a
// change to a prose field is a step between numbered versions, and no
// superseded text — nor the current one — is printed in it. A scalar field
// beside it keeps its from → to line. [LAW:behavior-not-structure]
func TestRunHistoryShowsProseChangesAsVersions(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)

	var stdout bytes.Buffer
	if err := runHistory(ctx, &stdout, ap, []string{id}); err != nil {
		t.Fatalf("runHistory() error = %v", err)
	}
	out := stdout.String()

	for _, want := range []string{
		"    description: v1 → v2\n",
		"    description: v2 → v3\n",
		"    prompt: v1 → v2\n",
		"    title: Original title → Renamed\n",
		"\nversions:\n    description: 3 versions — lit history " + id + " --version description@<n> prints one\n" +
			"    prompt: 2 versions — lit history " + id + " --version prompt@<n> prints one\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("lit history output missing %q in:\n%s", want, out)
		}
	}
	for _, text := range []string{"first draft", "second draft", "line two", "third draft", "old prompt", "new prompt"} {
		if strings.Contains(out, text) {
			t.Fatalf("lit history printed prose text %q in its trail:\n%s", text, out)
		}
	}
}

// TestRunHistoryVersionPrintsTheTextThatVersionHeld: every version the trail
// numbers is retrievable by that number, bare, so it round-trips into
// `lit update --description`. Version n+1 is what the n-th change wrote.
func TestRunHistoryVersionPrintsTheTextThatVersionHeld(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)

	for ref, want := range map[string]string{
		"description@1": "first draft\n",
		"description@2": "second draft\nline two\n",
		"description@3": "third draft\n",
		"prompt@1":      "old prompt\n",
		"prompt@2":      "new prompt\n",
	} {
		var stdout bytes.Buffer
		if err := runHistory(ctx, &stdout, ap, []string{id, "--version", ref}); err != nil {
			t.Fatalf("runHistory(--version %s) error = %v", ref, err)
		}
		if got := stdout.String(); got != want {
			t.Fatalf("runHistory(--version %s) = %q, want %q", ref, got, want)
		}
	}
}

// TestRunHistoryVersionPastTheTrailIsNotFound: a version the trail does not
// hold is a lookup miss (exit 4) that says how many it does hold — including
// none, for a field never changed.
func TestRunHistoryVersionPastTheTrailIsNotFound(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)
	untouched, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Never edited", Description: "only draft", Topic: "history", IssueType: "task",
		ParentID: seedEpicRaw(t, ctx, ap, "Untouched", "history"),
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	for _, tc := range []struct{ id, ref, want string }{
		{id, "description@4", "records 3 versions of description"},
		{untouched.ID, "description@1", "records 0 versions of description"},
	} {
		err := runHistory(ctx, &bytes.Buffer{}, ap, []string{tc.id, "--version", tc.ref})
		if ExitCode(err) != ExitNotFound || commandErrorReason(err) != reasonVersionNotFound || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("runHistory(%s --version %s) = exit %d %v, want exit %d naming %q", tc.id, tc.ref, ExitCode(err), err, ExitNotFound, tc.want)
		}
	}
}

// TestRunHistoryVersionRejectsAMalformedReference: --version names a prose
// field and a version counted from 1, and anything else is a usage error —
// decided before the id is looked up, so it holds for an id that names nothing.
func TestRunHistoryVersionRejectsAMalformedReference(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedProseHistory(t, ctx, ap)

	for _, tc := range []struct{ id, ref string }{
		{id, "title@1"},
		{id, "description"},
		{id, "description@0"},
		{id, "description@x"},
		{id, "@1"},
		{id, ""},
		{"test-missing", "status@1"},
	} {
		err := runHistory(ctx, &bytes.Buffer{}, ap, []string{tc.id, "--version", tc.ref})
		if ExitCode(err) != ExitUsage {
			t.Fatalf("runHistory(%s --version %q) = exit %d %v, want exit %d", tc.id, tc.ref, ExitCode(err), err, ExitUsage)
		}
	}
}
