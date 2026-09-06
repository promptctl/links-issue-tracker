package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The backlog used to restate group-scoped facts once per row, so a ten-child
// epic in one sequential lane rendered the same three sentences ten times and
// buried the lines that were actually per-row. links-listing-x943. These tests
// pin what the reader is owed — each fact said once — not how the renderer
// arranges to say it. [LAW:behavior-not-structure]

// A sequential lane is a chain, and a chain's blocking fact is one edge per
// link. Naming every pending predecessor made the text grow quadratically down
// the epic: the tenth child restating the nine facts its nine predecessors had
// each already stated.
func TestBacklogNamesOnlyTheNearestPendingLaneMate(t *testing.T) {
	h := newBacklogTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "seq", IssueType: "epic", Priority: 1})
	var children []string
	for _, title := range []string{"First", "Second", "Third", "Fourth"} {
		children = append(children, h.createIssue(storage.CreateIssueInput{
			Prefix: "test", Title: title, Topic: "seq", IssueType: "task", Priority: 1, ParentID: epic,
		}))
	}

	blocked := blockedLinesByRow(t, h.runBacklogText(), children)

	// The lane head is pullable and says nothing about siblings.
	if line, ok := blocked[children[0]]; ok {
		t.Errorf("lane head %s is blocked by %q; it is the front of the lane", children[0], line)
	}
	// Every other link names the one directly ahead of it, and only that one.
	for i := 1; i < len(children); i++ {
		line, ok := blocked[children[i]]
		if !ok {
			t.Errorf("%s has no blocked line; it sits behind %s in a sequential lane", children[i], children[i-1])
			continue
		}
		if want := "earlier sibling " + children[i-1] + " still open"; !strings.Contains(line, want) {
			t.Errorf("%s blocked line = %q, want it to name %q", children[i], line, want)
		}
		for _, other := range children[:i-1] {
			if strings.Contains(line, other) {
				t.Errorf("%s blocked line = %q; it names %s, a predecessor the rank order above it already showed", children[i], line, other)
			}
		}
	}
}

// The epic line describes an epic, not a row. Ten siblings share one epic and
// are owed one epic line.
func TestBacklogNamesAnEpicOncePerRun(t *testing.T) {
	h := newBacklogTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Shared Epic", Topic: "run", IssueType: "epic", Priority: 1})
	for _, title := range []string{"First", "Second", "Third"} {
		h.createIssue(storage.CreateIssueInput{
			Prefix: "test", Title: title, Topic: "run", IssueType: "task", Priority: 1, ParentID: epic,
		})
	}

	text := h.runBacklogText()
	if got := strings.Count(text, "epic: "+epic+" "); got != 1 {
		t.Fatalf("epic line for %s appears %d times, want 1 — its three children share one epic:\n%s", epic, got, text)
	}
}

// The claim line answers "who holds this lane and how is it going". That is one
// fact about the lane, and every member of the lane used to carry a verbatim
// copy of it.
func TestBacklogDescribesALaneClaimOncePerRun(t *testing.T) {
	h := newBacklogTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Claimed Epic", Topic: "claim", IssueType: "epic", Priority: 1})
	for _, title := range []string{"First", "Second", "Third"} {
		h.createIssue(storage.CreateIssueInput{
			Prefix: "test", Title: title, Topic: "claim", IssueType: "task", Priority: 1, ParentID: epic,
		})
	}

	rows, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	lane := laneOf(t, details, rows[0])
	cc := claimContext{standings: claims.Standings{lane: heldBy(otherAttribution)}, self: selfAttribution}

	var out bytes.Buffer
	if err := printBacklogOutput(&out, nil, rows, details, cc); err != nil {
		t.Fatalf("printBacklogOutput error = %v", err)
	}
	text := out.String()

	// Count context lines only: the preamble's prose also says "claimed".
	claimed := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, contextIndent+"claimed") {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claim line appears %d times, want 1 — the three rows share lane %s:\n%s", claimed, lane, text)
	}
}

// blockedLinesByRow maps each id to the "blocked:" line rendered under its row,
// omitting ids whose row carries none. It reads the rendered text rather than
// the annotations so the assertions above are about what the reader sees.
func blockedLinesByRow(t *testing.T, text string, ids []string) map[string]string {
	t.Helper()
	lines := strings.Split(text, "\n")
	blocked := make(map[string]string)
	row := ""
	for _, line := range lines {
		if id, ok := rowHeadingID(line, ids); ok {
			row = id
			continue
		}
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "blocked:") && row != "" {
			blocked[row] = strings.TrimPrefix(trimmed, "blocked:")
		}
	}
	return blocked
}

// backlogRowHeading matches the numbered row heading the backlog prints per
// item ("%2d. <id> ..."), which is what separates a row from the indented
// context lines beneath it.
var backlogRowHeading = regexp.MustCompile(`^\s*\d+\.\s+(\S+)`)

// rowHeadingID reports which of ids heads this line, if any.
func rowHeadingID(line string, ids []string) (string, bool) {
	match := backlogRowHeading.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}
	for _, id := range ids {
		if match[1] == id {
			return id, true
		}
	}
	return "", false
}
