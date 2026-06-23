package cli

import (
	"strings"
	"testing"
)

// siblingsBlock returns the lines of the `siblings:` relationship group from
// `lit show` output — the `- <id> [state] <title>` rows under the header, and
// nothing else. Empty string means the group was omitted entirely.
func siblingsBlock(out string) string {
	const header = "\nsiblings:\n"
	start := strings.Index(out, header)
	if start < 0 {
		return ""
	}
	rest := out[start+len(header):]
	var rows []string
	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(line, "- ") {
			break
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// A ticket whose parent has other children lists those siblings — its peers,
// in rank order — and never itself.
func TestRunShowListsSiblingsExcludingSelf(t *testing.T) {
	f := newEpicFixture(t, "Plan epic", "the why")
	first := f.addChild("First sibling")
	focus := f.addChild("Focused child")
	third := f.addChild("Third sibling")

	block := siblingsBlock(showOutput(t, f.ap, focus))
	if block == "" {
		t.Fatalf("expected a siblings group for a child with peers, got none in:\n%s", showOutput(t, f.ap, focus))
	}
	for _, want := range []string{first + " ", "First sibling", third + " ", "Third sibling"} {
		if !strings.Contains(block, want) {
			t.Errorf("siblings group missing %q in:\n%s", want, block)
		}
	}
	if strings.Contains(block, focus) {
		t.Errorf("siblings group must exclude the focal ticket %s, got:\n%s", focus, block)
	}
	// Rank order: First (created first) precedes Third (created last).
	if strings.Index(block, first) > strings.Index(block, third) {
		t.Errorf("siblings should be rank-ordered, got:\n%s", block)
	}
}

// An only child has a parent but no peers: the siblings group is omitted.
func TestRunShowOnlyChildRendersNoSiblings(t *testing.T) {
	f := newEpicFixture(t, "Solo epic", "the why")
	only := f.addChild("Only child")

	if block := siblingsBlock(showOutput(t, f.ap, only)); block != "" {
		t.Errorf("an only child must render no siblings group, got:\n%s", block)
	}
}

// A parentless ticket has no siblings group at all.
func TestRunShowParentlessTicketRendersNoSiblings(t *testing.T) {
	f := newEpicFixture(t, "Epic", "the why")
	// The epic itself is parentless.
	if block := siblingsBlock(showOutput(t, f.ap, f.epicID)); block != "" {
		t.Errorf("a parentless ticket must render no siblings group, got:\n%s", block)
	}
}
