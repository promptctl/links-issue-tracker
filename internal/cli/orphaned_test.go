package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// runOrphanedIDs renders `lit orphaned` and extracts the issue ID leading each
// row, in render order. Membership in this list IS the orphaned classification
// — only orphaned in_progress leaves are emitted — so asserting an ID appears
// is asserting it was classified orphaned. [LAW:single-enforcer]
func runOrphanedIDs(t *testing.T, h readyTestHarness, args ...string) []string {
	t.Helper()
	var stdout bytes.Buffer
	if err := runOrphaned(h.ctx, &stdout, h.ap, args); err != nil {
		t.Fatalf("runOrphaned(%v) error = %v", args, err)
	}
	return issueIDsFromText(stdout.String())
}

func startIssueForTest(t *testing.T, h readyTestHarness, id string) {
	t.Helper()
	if _, err := h.ap.Store.Apply(h.ctx, id, store.Change{Action: model.Start{Assignee: "agent"}, Actor: "agent", Reason: "claim"}); err != nil {
		t.Fatalf("StartIssue(%s): %v", id, err)
	}
}

func TestRunOrphanedListsStaleInProgress(t *testing.T) {
	h := newReadyTestHarness(t)

	stale := h.createIssue(store.CreateIssueInput{Prefix: "test",
		Title:     "Stale work",
		Topic:     "stale",
		IssueType: "task",
	})
	fresh := h.createIssue(store.CreateIssueInput{Prefix: "test",
		Title:     "Fresh work",
		Topic:     "fresh",
		IssueType: "task",
	})
	startIssueForTest(t, h, stale.ID)
	startIssueForTest(t, h, fresh.ID)
	h.backdateUpdatedAt(stale.ID, 7*time.Hour)

	got := runOrphanedIDs(t, h)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (only stale should be orphaned)", len(got))
	}
	if got[0] != stale.ID {
		t.Fatalf("got[0] = %q, want %q", got[0], stale.ID)
	}
}

func TestRunOrphanedExcludesOpenAndClosed(t *testing.T) {
	h := newReadyTestHarness(t)

	open := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Open", Topic: "topic", IssueType: "task"})
	closed := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Closed", Topic: "topic", IssueType: "task"})
	h.backdateUpdatedAt(open.ID, 7*time.Hour)
	h.closeIssue(closed.ID, "done")
	h.backdateUpdatedAt(closed.ID, 7*time.Hour)

	got := runOrphanedIDs(t, h)
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0 (orphan applies only to in_progress)", len(got))
	}
}

func TestRunOrphanedSortsOldestFirst(t *testing.T) {
	h := newReadyTestHarness(t)

	older := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Older", Topic: "topic", IssueType: "task"})
	newer := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Newer", Topic: "topic", IssueType: "task"})
	startIssueForTest(t, h, older.ID)
	startIssueForTest(t, h, newer.ID)
	h.backdateUpdatedAt(older.ID, 48*time.Hour)
	h.backdateUpdatedAt(newer.ID, 7*time.Hour)

	got := runOrphanedIDs(t, h)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0] != older.ID {
		t.Fatalf("got[0] = %q, want oldest %q first", got[0], older.ID)
	}
}

func TestRunOrphanedTextEmpty(t *testing.T) {
	h := newReadyTestHarness(t)
	var stdout bytes.Buffer
	if err := runOrphaned(h.ctx, &stdout, h.ap, nil); err != nil {
		t.Fatalf("runOrphaned: %v", err)
	}
	if !strings.Contains(stdout.String(), "No orphaned issues") {
		t.Fatalf("expected 'No orphaned issues' message, got: %q", stdout.String())
	}
}

func TestRunOrphanedAssigneeFilter(t *testing.T) {
	h := newReadyTestHarness(t)

	mine := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Mine", Topic: "topic", IssueType: "task"})
	theirs := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Theirs", Topic: "topic", IssueType: "task"})
	if _, err := h.ap.Store.Apply(h.ctx, mine.ID, store.Change{Action: model.Start{Assignee: "alice"}, Actor: "alice"}); err != nil {
		t.Fatalf("StartIssue(mine) error = %v", err)
	}
	if _, err := h.ap.Store.Apply(h.ctx, theirs.ID, store.Change{Action: model.Start{Assignee: "bob"}, Actor: "bob"}); err != nil {
		t.Fatalf("StartIssue(theirs) error = %v", err)
	}
	h.backdateUpdatedAt(mine.ID, 7*time.Hour)
	h.backdateUpdatedAt(theirs.ID, 7*time.Hour)

	got := runOrphanedIDs(t, h, "--assignee", "alice")
	if len(got) != 1 || got[0] != mine.ID {
		t.Fatalf("--assignee=alice: got %d rows, want 1 (alice's only): %+v", len(got), got)
	}
}

func TestRunOrphanedExcludesContainerEpics(t *testing.T) {
	h := newReadyTestHarness(t)

	epic := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "topic", IssueType: "epic"})
	leaf := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "topic", IssueType: "task", ParentID: epic.ID})
	startIssueForTest(t, h, leaf.ID)
	// Epic's State() derives from the leaf and is in_progress, but its
	// own UpdatedAt is irrelevant — orphan is a leaf-only concept.
	h.backdateUpdatedAt(epic.ID, 48*time.Hour)
	h.backdateUpdatedAt(leaf.ID, 7*time.Hour)

	got := runOrphanedIDs(t, h)
	for _, id := range got {
		if id == epic.ID {
			t.Fatalf("epic %q should not appear in lit orphaned (containers excluded)", epic.ID)
		}
	}
	if len(got) != 1 || got[0] != leaf.ID {
		t.Fatalf("expected the leaf %q only, got: %+v", leaf.ID, got)
	}
}
