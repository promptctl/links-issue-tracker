package cli

import (
	"strings"
	"testing"
)

// A ticket whose parent has other children conveys those siblings, and its
// own position among them, exactly once — via the epic "Children:" block —
// never through a separate "siblings:" group.
func TestRunShowListsSiblingsExcludingSelfOnce(t *testing.T) {
	f := newEpicFixture(t, "Plan epic", "the why")
	first := f.addChild("First sibling")
	focus := f.addChild("Focused child")
	third := f.addChild("Third sibling")

	out := showOutput(t, f.ap, focus)

	if strings.Contains(out, "\nsiblings:\n") {
		t.Errorf("show output must not contain a separate siblings group, got:\n%s", out)
	}
	// first and third are pure siblings: each id must appear exactly once.
	// focus is the shown ticket itself, so its id legitimately appears twice
	// (the show header, then its own row in the epic block) — that is
	// self-identification, not sibling duplication.
	for _, id := range []string{first, third} {
		if n := strings.Count(out, id); n != 1 {
			t.Errorf("expected sibling %s to appear exactly once in show output, got %d in:\n%s", id, n, out)
		}
	}
	if !strings.Contains(out, first+" ") || !strings.Contains(out, "First sibling") {
		t.Errorf("epic Children block missing first sibling in:\n%s", out)
	}
	if !strings.Contains(out, third+" ") || !strings.Contains(out, "Third sibling") {
		t.Errorf("epic Children block missing third sibling in:\n%s", out)
	}
	if !strings.Contains(out, focus+" ") || !strings.Contains(out, "(you are here)") {
		t.Errorf("epic Children block must mark the focal ticket's position, got:\n%s", out)
	}
	// Rank order: First (created first) precedes Third (created last).
	if strings.Index(out, first) > strings.Index(out, third) {
		t.Errorf("siblings should be rank-ordered, got:\n%s", out)
	}
}

// An only child has a parent but no peers: the epic Children block still
// identifies the parent epic and marks the ticket's own position, with no
// separate siblings group.
func TestRunShowOnlyChildRendersNoSiblingsGroup(t *testing.T) {
	f := newEpicFixture(t, "Solo epic", "the why")
	only := f.addChild("Only child")

	out := showOutput(t, f.ap, only)

	if strings.Contains(out, "\nsiblings:\n") {
		t.Errorf("an only child must render no siblings group, got:\n%s", out)
	}
	if !strings.Contains(out, "Epic: "+f.epicID) {
		t.Errorf("an only child must still identify its parent epic, got:\n%s", out)
	}
	if !strings.Contains(out, only+" ") || !strings.Contains(out, "(you are here)") {
		t.Errorf("an only child must still show its own position, got:\n%s", out)
	}
}

// A parentless ticket (the epic itself) has no siblings group at all.
func TestRunShowParentlessTicketRendersNoSiblingsGroup(t *testing.T) {
	f := newEpicFixture(t, "Epic", "the why")
	// The epic itself is parentless.
	if out := showOutput(t, f.ap, f.epicID); strings.Contains(out, "\nsiblings:\n") {
		t.Errorf("a parentless ticket must render no siblings group, got:\n%s", out)
	}
}
