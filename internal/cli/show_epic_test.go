package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
	// Both children sit in the default lane, which is one sequential chain, so
	// the focused child is genuinely held back by the sibling ranked ahead of
	// it. Asserted through runShow rather than the builder: this is the whole
	// path — show → epic block → the readiness gate `lit next` routes on —
	// answering with one verdict. (links-epic-context-oezb)
	want := "  ▶ [blocked: earlier sibling " + sibling + " still open] " + focus + "  Focused child   (you are here)"
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
	// One lane, so B waits on A — the epic-level view answers exactly as the
	// leaf-level one above does.
	for _, want := range []string{
		"    [ready]       " + a + "  Child A",
		"    [blocked: earlier sibling " + a + " still open] " + b + "  Child B",
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
	free, err := ap.Store.CreateIssue(context.Background(), storage.CreateIssueInput{
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

// writeUnrelatedlyBrokenConfig writes a repo config whose only defect is in a
// setting the show path never reads. snapshot.retention_budget is validated by
// config.Load like every other field, so it stands in for "this repo's config
// cannot be loaded, for reasons that have nothing to do with the ready policy".
func writeUnrelatedlyBrokenConfig(t *testing.T, ap *app.App) {
	t.Helper()
	configDir := filepath.Join(ap.Workspace.RootDir, ".lit")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[snapshot]\nretention_budget = -1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}
}

// An issue in no epic needs no ready policy, so it must not be coupled to
// whether this repo's config loads at all. The plan slice is what wants the
// policy, and a parentless ticket has none.
func TestRunShowParentlessTicketIgnoresUnreadableConfig(t *testing.T) {
	ap := newTestCLIApp(t)
	free, err := ap.Store.CreateIssue(context.Background(), storage.CreateIssueInput{
		Prefix: "test", Title: "Free floating", Topic: "misc", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(free) error = %v", err)
	}
	writeUnrelatedlyBrokenConfig(t, ap)

	var buf bytes.Buffer
	if err := runShow(context.Background(), &buf, ap, []string{free.ID}); err != nil {
		t.Fatalf("show of a ticket in no epic must not read repo config, got error = %v", err)
	}
	if !strings.Contains(buf.String(), "Free floating") {
		t.Errorf("show output missing the issue body:\n%s", buf.String())
	}
}

// An epic member genuinely needs the policy, so an unreadable config is a real
// failure — but it must arrive before the body is written. A body printed ahead
// of the error is shaped exactly like the legitimate "no epic block" output, so
// a caller holding only stdout could not tell the two apart.
func TestRunShowEpicMemberFailsBeforeWritingBodyOnUnreadableConfig(t *testing.T) {
	f := newEpicFixture(t, "Plan epic", "the why")
	child := f.addChild("A child")
	writeUnrelatedlyBrokenConfig(t, f.ap)

	var buf bytes.Buffer
	err := runShow(context.Background(), &buf, f.ap, []string{child})
	if err == nil {
		t.Fatalf("show of an epic member under an unreadable config must fail, got nil; output:\n%s", buf.String())
	}
	if buf.Len() != 0 {
		t.Errorf("failed plan resolution must leave stdout empty, got:\n%s", buf.String())
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
