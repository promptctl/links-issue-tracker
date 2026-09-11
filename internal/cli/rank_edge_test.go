package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRankToEdgeReportsItsFrame covers what `lit rank --top/--bottom` actually
// prints. The storage tests assert rank strings, which is the right assertion
// there and the reason none of them can see this: an edge move inside an epic
// and an edge move across the backlog write the same shape of key, and only the
// emitted text tells a reader which one happened.
//
// [LAW:behavior-not-structure] the contract here is the text a user reads, so
// the assertions are on that text and not on which store method ran.
func TestRankToEdgeReportsItsFrame(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "edge", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	first, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "edge", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(first) error = %v", err)
	}
	second, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Second", Topic: "edge", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(second) error = %v", err)
	}
	standalone, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "edge", IssueType: "task", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}

	// A child promoted inside its epic leads its siblings and nothing else. An
	// unqualified "moved to the top" would read as the top of the backlog, so
	// the frame it was scoped to has to be named.
	var stdout bytes.Buffer
	if err := runRank(ctx, &stdout, ap, []string{second.ID, "--top"}); err != nil {
		t.Fatalf("rank second --top error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, second.ID+" is inside "+epic.ID) {
		t.Errorf("rank --top output = %q, want it to name the epic %s the move was scoped to", out, epic.ID)
	}
	if !strings.Contains(out, "leaving the rest of the queue unchanged") {
		t.Errorf("rank --top output = %q, want it to say the rest of the queue is untouched", out)
	}
	// The move was real: the promoted child now precedes the sibling it passed.
	promoted, err := ap.Store.GetIssue(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetIssue(second) error = %v", err)
	}
	led, err := ap.Store.GetIssue(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetIssue(first) error = %v", err)
	}
	if promoted.Rank >= led.Rank {
		t.Errorf("after --top the promoted child ranks %q, want it below its sibling's %q", promoted.Rank, led.Rank)
	}

	// Repeating it writes nothing. The command exits 0 and prints the same
	// ticket either way, so an unchanged order reported as a promotion is the
	// one outcome a reader cannot tell from a real one. [LAW:no-silent-failure]
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{second.ID, "--top"}); err != nil {
		t.Fatalf("repeat rank second --top error = %v", err)
	}
	out = stdout.String()
	if !strings.Contains(out, "is already at the top of "+epic.ID) {
		t.Errorf("no-op rank --top output = %q, want it to report the edge was already held", out)
	}
	if !strings.Contains(out, "nothing to rank") {
		t.Errorf("no-op rank --top output = %q, want it to say nothing was written", out)
	}
	// The no-op path deliberately prints no issue summary: that summary line is
	// what a successful move looks like, and printing it here is exactly the
	// confusion the message exists to remove.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, second.ID+" [") {
			t.Errorf("no-op rank --top printed a success summary line %q", line)
		}
	}

	// A top-level issue's frame is the backlog, which the epic-scoped sentence
	// must not claim to be an epic.
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{standalone.ID, "--top"}); err != nil {
		t.Fatalf("rank standalone --top error = %v", err)
	}
	out = stdout.String()
	if strings.Contains(out, "is inside") {
		t.Errorf("top-level rank --top output = %q, want no frame-scoping note", out)
	}
	if strings.Contains(out, "nothing to rank") {
		t.Errorf("top-level rank --top output = %q, want a real move, not a no-op", out)
	}

	// And a top-level no-op names the backlog, which is frameLabel's whole job:
	// TopLevel is the empty Frame, so an unrendered one would print "the top of
	// ; nothing to rank".
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{standalone.ID, "--top"}); err != nil {
		t.Fatalf("repeat rank standalone --top error = %v", err)
	}
	if !strings.Contains(stdout.String(), "is already at the top of the backlog") {
		t.Errorf("top-level no-op output = %q, want it to name the backlog", stdout.String())
	}
}

// TestRankToBottomReportsItsFrame is the --bottom half of the case above, and it
// is a separate function rather than more assertions inside that one because the
// two arms of runRank's switch pair their own edge word with their own store
// call. Nothing shared decides that pairing, so `--top` assertions cannot reach a
// defect that exists only in the `--bottom` arm: an edge word left reading "top"
// there, or RankToTop called where RankToBottom belongs, would ship green.
//
// Each of those two mistakes fails a different assertion here on purpose — the
// printed word is checked against "bottom", and the resulting order is checked
// independently, so neither can stand in for the other. [LAW:behavior-not-structure]
func TestRankToBottomReportsItsFrame(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "edge", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	first, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "First", Topic: "edge", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(first) error = %v", err)
	}
	last, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Last", Topic: "edge", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(last) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runRank(ctx, &stdout, ap, []string{first.ID, "--bottom"}); err != nil {
		t.Fatalf("rank first --bottom error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, first.ID+" is inside "+epic.ID) {
		t.Errorf("rank --bottom output = %q, want it to name the epic %s the move was scoped to", out, epic.ID)
	}
	// The word the user asked for, echoed back. An arm that kept "top" here
	// would otherwise report the opposite of what it did.
	if !strings.Contains(out, "ranked it to the bottom of "+epic.ID+"'s children") {
		t.Errorf("rank --bottom output = %q, want it to say the move went to the bottom of the epic's children", out)
	}

	// The order, checked on its own: a --bottom arm wired to RankToTop prints
	// nothing wrong and still moves the issue the wrong way.
	demoted, err := ap.Store.GetIssue(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetIssue(first) error = %v", err)
	}
	passed, err := ap.Store.GetIssue(ctx, last.ID)
	if err != nil {
		t.Fatalf("GetIssue(last) error = %v", err)
	}
	if demoted.Rank <= passed.Rank {
		t.Errorf("after --bottom the demoted child ranks %q, want it after its sibling's %q", demoted.Rank, passed.Rank)
	}

	// Repeating it writes nothing, and says so in the same words as the top edge.
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{first.ID, "--bottom"}); err != nil {
		t.Fatalf("repeat rank first --bottom error = %v", err)
	}
	out = stdout.String()
	if !strings.Contains(out, "is already at the bottom of "+epic.ID) {
		t.Errorf("no-op rank --bottom output = %q, want it to report the edge was already held", out)
	}
	if !strings.Contains(out, "nothing to rank") {
		t.Errorf("no-op rank --bottom output = %q, want it to say nothing was written", out)
	}
}
