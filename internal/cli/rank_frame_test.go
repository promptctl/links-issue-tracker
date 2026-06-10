package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// TestRankCrossFrameReportsResolution verifies the rank command tells the
// user when a cross-frame request was resolved to the containing epic —
// moving an issue other than the one named must never be silent.
func TestRankCrossFrameReportsResolution(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "frame", IssueType: "epic", Placement: store.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	child, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child", Topic: "frame", IssueType: "task", ParentID: epic.ID, Placement: store.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	standalone, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Standalone", Topic: "frame", IssueType: "task", Placement: store.RankBottom})
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
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if summary := lines[len(lines)-1]; !strings.Contains(summary, epic.ID) {
		t.Errorf("rank summary line = %q, want it to describe the epic that moved (%s)", summary, epic.ID)
	}

	// Anchor-side resolution: ranking the standalone against the child anchors to the epic.
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{standalone.ID, "--below", child.ID}); err != nil {
		t.Fatalf("rank standalone --below child error = %v", err)
	}
	if !strings.Contains(stdout.String(), child.ID+" is inside "+epic.ID) {
		t.Errorf("rank output = %q, want anchor-side resolution note", stdout.String())
	}

	// JSON mode stays a single JSON document describing what moved.
	stdout.Reset()
	if err := runRank(ctx, &stdout, ap, []string{child.ID, "--above", standalone.ID, "--json"}); err != nil {
		t.Fatalf("rank --json error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("rank --json output is not one JSON doc: %v\n%s", err, stdout.String())
	}
	if doc["id"] != epic.ID {
		t.Errorf("rank --json doc id = %v, want the moved epic %s", doc["id"], epic.ID)
	}
}
