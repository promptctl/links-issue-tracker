package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// showOutput runs `lit show <id>` text rendering and returns the captured
// stdout, the integration surface the epic block is wired into.
func showOutput(t *testing.T, ap *app.App, id string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := runShow(context.Background(), &buf, ap, []string{id}); err != nil {
		t.Fatalf("runShow(%s) error = %v", id, err)
	}
	return buf.String()
}

// A leaf under an epic renders its own body plus the epic plan block, with the
// focused-child marker on its own row.
func TestRunShowChildRendersEpicBlockWithFocus(t *testing.T) {
	f := newEpicFixture(t, "Plan epic", "# Why this exists\nthe shared why")
	sibling := f.addChild("Sibling first")
	focus := f.addChild("Focused child")

	out := showOutput(t, f.ap, focus)

	if !strings.Contains(out, "Focused child") {
		t.Fatalf("show output missing the issue's own body:\n%s", out)
	}
	if !strings.Contains(out, "Epic: "+f.epicID+" — Plan epic") {
		t.Errorf("child show should append the epic block:\n%s", out)
	}
	if !strings.Contains(out, "Why: Why this exists") {
		t.Errorf("epic block should carry the why:\n%s", out)
	}
	want := "  ▶ [ready]       " + focus + "  Focused child   (you are here)"
	if !strings.Contains(out, want) {
		t.Errorf("focused child should be marked you-are-here, want %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "    [ready]       "+sibling+"  Sibling first") {
		t.Errorf("sibling should appear as an unfocused row:\n%s", out)
	}
}

// An epic renders its own body plus the children list, with no focus marker.
func TestRunShowEpicRendersChildrenNoFocus(t *testing.T) {
	f := newEpicFixture(t, "Top epic", "# Goal\nthe plan")
	a := f.addChild("Child A")
	b := f.addChild("Child B")

	out := showOutput(t, f.ap, f.epicID)

	if !strings.Contains(out, "Epic: "+f.epicID+" — Top epic") {
		t.Errorf("epic show should append the epic block:\n%s", out)
	}
	for _, want := range []string{
		"    [ready]       " + a + "  Child A",
		"    [ready]       " + b + "  Child B",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("epic show should list children, missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "▶") {
		t.Errorf("epic show passes no focus, must not render a you-are-here marker:\n%s", out)
	}
}

// A parentless non-epic issue is unchanged: no epic block at all.
func TestRunShowParentlessTicketHasNoEpicBlock(t *testing.T) {
	ap := newTestCLIApp(t)
	free, err := ap.Store.CreateIssue(context.Background(), store.CreateIssueInput{
		Prefix: "test", Title: "Free floating", Topic: "misc", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(free) error = %v", err)
	}

	out := showOutput(t, ap, free.ID)

	if !strings.Contains(out, "Free floating") {
		t.Fatalf("show output missing the issue body:\n%s", out)
	}
	if strings.Contains(out, "Epic:") {
		t.Errorf("an issue in no epic must render no epic block:\n%s", out)
	}
}

// An in_progress child inside an epic renders the in_progress marker in the
// block — the show path classifies status from live state, same as the renderer.
func TestRunShowInProgressChildRendersInProgressMarker(t *testing.T) {
	f := newEpicFixture(t, "Active epic", "work underway")
	working := f.addChild("In flight")
	f.transition(working, model.Start{Assignee: "test"})

	out := showOutput(t, f.ap, working)

	want := "  ▶ [in_progress] " + working + "  In flight   (you are here)"
	if !strings.Contains(out, want) {
		t.Errorf("in_progress child should render in_progress marker, want %q in:\n%s", want, out)
	}
}
