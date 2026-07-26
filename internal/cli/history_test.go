package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// TestRunHistoryPrintsMultiEditTransitionTrail exercises `lit history` on a
// ticket edited several times through the real CLI update path. The trail must
// carry every field-level `from → to` transition those edits produced — the
// full history `lit show` deliberately no longer renders. This is a behavioral test
// [LAW:behavior-not-structure]: it drives genuine edits and asserts on the
// transitions a reader sees, not on how the events are stored.
func TestRunHistoryPrintsMultiEditTransitionTrail(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test",
		Title: "Original title", Topic: "history", IssueType: "task", Priority: 0,
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
}
