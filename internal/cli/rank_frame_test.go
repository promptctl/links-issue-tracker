package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRankCrossFrameReportsResolution verifies the rank command tells the
// user when a cross-frame request was resolved to the containing epic —
// moving an issue other than the one named must never be silent.
func TestRankCrossFrameReportsResolution(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Child", Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	standalone, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "task", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(standalone) error = %v", err)
	}

	// Moved-side resolution: ranking the child against the standalone moves the epic.
	var stdout bytes.Buffer
	if err := runRank(ctx, &stdout, ap, []string{child.ID, "--above", standalone.ID}); err != nil {
		t.Fatalf("rank child --above standalone error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, child.ID+" is inside "+epic.ID) {
		t.Errorf("rank output = %q, want moved-side resolution note naming %s and %s", out, child.ID, epic.ID)
	}
	// [LAW:behavior-not-structure] The contract is that the issue-summary line
	// describes the epic that moved, not that it sits at any fixed position
	// (the success output now ends with a quickstart breadcrumb).
	summaryFound := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, epic.ID+" [") {
			summaryFound = true
		}
	}
	if !summaryFound {
		t.Errorf("rank output = %q, want a summary line describing the epic that moved (%s)", out, epic.ID)
	}

	// Anchor-side resolution: ranking the standalone against the child anchors to the epic.
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{standalone.ID, "--below", child.ID}); err != nil {
		t.Fatalf("rank standalone --below child error = %v", err)
	}
	if !strings.Contains(stdout.String(), child.ID+" is inside "+epic.ID) {
		t.Errorf("rank output = %q, want anchor-side resolution note", stdout.String())
	}
}

// TestRankSetNamesTheFrameItStackedIn covers the summary line of lit rank set,
// which had no CLI-level test at all.
//
// rank set anchors at the top of the representatives' own frame, so ordering
// three children of an epic leads that epic's children and moves nothing in the
// queue at large. The summary said "ranked 3 issues at top", which reads as the
// head of the backlog — the same ambiguity the edge verbs were given frameLabel
// to remove, left standing on the one verb whose whole subject is the frame.
// Agents read this output as ground truth, so a summary that overstates the
// scope of the move is a wrong answer, not a cosmetic one.
// [LAW:no-silent-failure]
func TestRankSetNamesTheFrameItStackedIn(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "set", IssueType: "epic", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	children := make([]string, 0, 2)
	for _, title := range []string{"C1", "C2"} {
		child, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "set", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", title, err)
		}
		children = append(children, child.ID)
	}
	outsider, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Outsider", Topic: "set", IssueType: "task", Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(outsider) error = %v", err)
	}

	// Siblings: the stack lands inside the epic, and the summary must say so.
	var stdout bytes.Buffer
	if err := runRankSet(ctx, &stdout, ap, []string{children[1], children[0]}); err != nil {
		t.Fatalf("rank set siblings error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "at the top of "+epic.ID) {
		t.Errorf("rank set output = %q, want it to name the epic %s the stack landed in", out, epic.ID)
	}

	// Top level: the summary must name the backlog rather than an epic, or the
	// frame-naming would be worse than the bare wording it replaced.
	stdout.Reset()
	if err := runRankSet(ctx, &stdout, ap, []string{outsider.ID, epic.ID}); err != nil {
		t.Fatalf("rank set top-level error = %v", err)
	}
	out = stdout.String()
	if !strings.Contains(out, "at the top of the backlog") {
		t.Errorf("rank set output = %q, want it to name the backlog for a top-level stack", out)
	}
	if strings.Contains(out, "at the top of "+epic.ID) {
		t.Errorf("rank set output = %q, named the epic %s for a stack that landed at the top level", out, epic.ID)
	}
}
