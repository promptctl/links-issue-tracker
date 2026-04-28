package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func runOrphanedJSON(t *testing.T, h readyTestHarness, args ...string) []annotation.AnnotatedIssue {
	t.Helper()
	var stdout bytes.Buffer
	allArgs := append(append([]string{}, args...), "--json")
	if err := runOrphaned(h.ctx, &stdout, h.ap, allArgs); err != nil {
		t.Fatalf("runOrphaned(%v) error = %v", allArgs, err)
	}
	var got []annotation.AnnotatedIssue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return got
}

func startIssueForTest(t *testing.T, h readyTestHarness, id string) {
	t.Helper()
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{
		IssueID:   id,
		Action:    "start",
		Reason:    "claim",
		CreatedBy: "agent",
	}); err != nil {
		t.Fatalf("TransitionIssue(start, %s): %v", id, err)
	}
}

func TestRunOrphanedListsStaleInProgress(t *testing.T) {
	h := newReadyTestHarness(t)

	stale := h.createIssue(store.CreateIssueInput{
		Title:     "Stale work",
		Topic:     "stale",
		IssueType: "task",
	})
	fresh := h.createIssue(store.CreateIssueInput{
		Title:     "Fresh work",
		Topic:     "fresh",
		IssueType: "task",
	})
	startIssueForTest(t, h, stale.ID)
	startIssueForTest(t, h, fresh.ID)
	h.backdateUpdatedAt(stale.ID, 7*time.Hour)

	got := runOrphanedJSON(t, h)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (only stale should be orphaned)", len(got))
	}
	if got[0].ID != stale.ID {
		t.Fatalf("got[0].ID = %q, want %q", got[0].ID, stale.ID)
	}
	if _, ok := findAnnotation(got[0].Annotations, annotation.Orphaned); !ok {
		t.Fatalf("expected Orphaned annotation, got: %#v", got[0].Annotations)
	}
}

func TestRunOrphanedExcludesOpenAndClosed(t *testing.T) {
	h := newReadyTestHarness(t)

	open := h.createIssue(store.CreateIssueInput{Title: "Open", Topic: "topic", IssueType: "task"})
	closed := h.createIssue(store.CreateIssueInput{Title: "Closed", Topic: "topic", IssueType: "task"})
	h.backdateUpdatedAt(open.ID, 7*time.Hour)
	h.closeIssue(closed.ID, "done")
	h.backdateUpdatedAt(closed.ID, 7*time.Hour)

	got := runOrphanedJSON(t, h)
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0 (orphan applies only to in_progress)", len(got))
	}
}

func TestRunOrphanedSortsOldestFirst(t *testing.T) {
	h := newReadyTestHarness(t)

	older := h.createIssue(store.CreateIssueInput{Title: "Older", Topic: "topic", IssueType: "task"})
	newer := h.createIssue(store.CreateIssueInput{Title: "Newer", Topic: "topic", IssueType: "task"})
	startIssueForTest(t, h, older.ID)
	startIssueForTest(t, h, newer.ID)
	h.backdateUpdatedAt(older.ID, 48*time.Hour)
	h.backdateUpdatedAt(newer.ID, 7*time.Hour)

	got := runOrphanedJSON(t, h)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].ID != older.ID {
		t.Fatalf("got[0].ID = %q, want oldest %q first", got[0].ID, older.ID)
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

	mine := h.createIssue(store.CreateIssueInput{Title: "Mine", Topic: "topic", IssueType: "task", Assignee: "alice"})
	theirs := h.createIssue(store.CreateIssueInput{Title: "Theirs", Topic: "topic", IssueType: "task", Assignee: "bob"})
	startIssueForTest(t, h, mine.ID)
	startIssueForTest(t, h, theirs.ID)
	h.backdateUpdatedAt(mine.ID, 7*time.Hour)
	h.backdateUpdatedAt(theirs.ID, 7*time.Hour)

	got := runOrphanedJSON(t, h, "--assignee", "alice")
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("--assignee=alice: got %d rows, want 1 (alice's only): %+v", len(got), got)
	}
}

func TestRunOrphanedExcludesContainerEpics(t *testing.T) {
	h := newReadyTestHarness(t)

	epic := h.createIssue(store.CreateIssueInput{Title: "Epic", Topic: "topic", IssueType: "epic"})
	leaf := h.createIssue(store.CreateIssueInput{Title: "Leaf", Topic: "topic", IssueType: "task", ParentID: epic.ID})
	startIssueForTest(t, h, leaf.ID)
	// Epic's State() derives from the leaf and is in_progress, but its
	// own UpdatedAt is irrelevant — orphan is a leaf-only concept.
	h.backdateUpdatedAt(epic.ID, 48*time.Hour)
	h.backdateUpdatedAt(leaf.ID, 7*time.Hour)

	got := runOrphanedJSON(t, h)
	for _, entry := range got {
		if entry.ID == epic.ID {
			t.Fatalf("epic %q should not appear in lit orphaned (containers excluded)", epic.ID)
		}
	}
	if len(got) != 1 || got[0].ID != leaf.ID {
		t.Fatalf("expected the leaf %q only, got: %+v", leaf.ID, got)
	}
}
