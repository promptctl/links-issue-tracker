package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func (h readyTestHarness) runNextJSON(args ...string) annotation.AnnotatedIssue {
	h.t.Helper()
	var stdout bytes.Buffer
	allArgs := append(append([]string{}, args...), "--json")
	if err := runNext(h.ctx, &stdout, h.ap, allArgs); err != nil {
		h.t.Fatalf("runNext(%v) error = %v", allArgs, err)
	}
	var got annotation.AnnotatedIssue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		h.t.Fatalf("json.Unmarshal(next output) error = %v", err)
	}
	return got
}

func (h readyTestHarness) runNextErr(args ...string) error {
	h.t.Helper()
	var stdout bytes.Buffer
	return runNext(h.ctx, &stdout, h.ap, args)
}

// `lit next` returns the top of the ready partition: the first open, unblocked
// leaf in the same composite-rank order `lit ready` produces.
func TestRunNextReturnsTopReadyLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	first := h.createIssue(store.CreateIssueInput{Title: "First leaf", Topic: "next", IssueType: "task", Priority: 1})
	h.createIssue(store.CreateIssueInput{Title: "Second leaf", Topic: "next", IssueType: "task", Priority: 2})

	got := h.runNextJSON()
	if got.ID != first.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order)", got.ID, first.ID)
	}
}

// In-progress leaves are not workable starts; `lit next` skips them and returns
// the next open one. The agent should `lit done` an in-progress leaf, not
// `lit start` it again.
func TestRunNextSkipsInProgressLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	inProgress := h.createIssue(store.CreateIssueInput{Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{IssueID: inProgress.ID, Action: "start", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start) error = %v", err)
	}
	openLeaf := h.createIssue(store.CreateIssueInput{Title: "Workable", Topic: "next", IssueType: "task", Priority: 2})

	got := h.runNextJSON()
	if got.ID != openLeaf.ID {
		t.Fatalf("next.ID = %q, want %q (in-progress leaves are not startable)", got.ID, openLeaf.ID)
	}
}

// Blocked leaves (open dependency) are skipped just as `lit ready` partitions
// them out of the ready section. With deterministic creation-order ranking the
// blocker (rank 1, no parent epic, no own dependencies) is unambiguously top,
// so the assertion pins both "dependent skipped" and the exact expected pick.
func TestRunNextSkipsBlockedLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	blocker := h.createIssue(store.CreateIssueInput{Title: "Blocker", Topic: "next", IssueType: "task", Priority: 1})
	dependent := h.createIssue(store.CreateIssueInput{Title: "Dependent", Topic: "next", IssueType: "task", Priority: 2})
	h.addDependency(dependent.ID, blocker.ID)
	h.createIssue(store.CreateIssueInput{Title: "Unblocked third", Topic: "next", IssueType: "task", Priority: 3})

	got := h.runNextJSON()
	if got.ID == dependent.ID {
		t.Fatalf("next.ID = %q (dependent), want a non-blocked leaf", got.ID)
	}
	if got.ID != blocker.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order after skipping blocked dependent)", got.ID, blocker.ID)
	}
}

// No ready work → non-nil error so the calling shell exits non-zero.
// Agents script `lit next` in loops; silent empty success would be a hang.
func TestRunNextErrorsWhenNoReadyWork(t *testing.T) {
	h := newReadyTestHarness(t)
	err := h.runNextErr()
	if err == nil {
		t.Fatal("runNext() error = nil, want non-nil for empty ready set")
	}
	if !strings.Contains(err.Error(), "no ready work") {
		t.Fatalf("runNext() error = %q, want contains \"no ready work\"", err.Error())
	}
}

// `--continue` biases toward leaves whose parent epic derives to in_progress
// (any child started). The composite-rank order would otherwise pick a
// higher-ranked leaf in another epic; --continue keeps the agent in the same
// epic context until it's done.
func TestRunNextContinueBiasesTowardInProgressEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	// epicA gets created first → composite rank places its leaves above epicB's.
	epicA := h.createIssue(store.CreateIssueInput{Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(store.CreateIssueInput{Title: "A.1", Topic: "next", IssueType: "task", Priority: 2, ParentID: epicA.ID})
	a2 := h.createIssue(store.CreateIssueInput{Title: "A.2", Topic: "next", IssueType: "task", Priority: 2, ParentID: epicA.ID})

	epicB := h.createIssue(store.CreateIssueInput{Title: "Epic B", Topic: "next", IssueType: "epic", Priority: 1})
	b1 := h.createIssue(store.CreateIssueInput{Title: "B.1", Topic: "next", IssueType: "task", Priority: 2, ParentID: epicB.ID})

	// Start B.1 so epicB derives to in_progress; epicA stays open.
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{IssueID: b1.ID, Action: "start", CreatedBy: "tester"}); err != nil {
		t.Fatalf("TransitionIssue(start B.1) error = %v", err)
	}

	// Default order would pick A.1 (top composite rank).
	defaultPick := h.runNextJSON()
	if defaultPick.ID != a1.ID {
		t.Fatalf("default next = %q, want A.1 %q (composite-rank top)", defaultPick.ID, a1.ID)
	}

	// --continue should skip past A.1/A.2 to find a leaf under in_progress epic B.
	// B.1 is in_progress (skipped); there are no other open leaves under B.
	// So --continue falls back to top-of-queue: A.1.
	// Reframe the test: add B.2 so there is a workable leaf under B.
	b2 := h.createIssue(store.CreateIssueInput{Title: "B.2", Topic: "next", IssueType: "task", Priority: 2, ParentID: epicB.ID})
	_ = a2

	continuePick := h.runNextJSON("--continue")
	if continuePick.ID != b2.ID {
		t.Fatalf("--continue next = %q, want B.2 %q (under in_progress epic)", continuePick.ID, b2.ID)
	}
}

// `--continue` with no in-progress epic falls back to the default top-of-queue
// pick — the bias is "prefer if available," not "filter to only".
func TestRunNextContinueFallsBackWhenNoInProgressEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(store.CreateIssueInput{Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	a1 := h.createIssue(store.CreateIssueInput{Title: "A.1", Topic: "next", IssueType: "task", Priority: 2, ParentID: epicA.ID})

	got := h.runNextJSON("--continue")
	if got.ID != a1.ID {
		t.Fatalf("--continue with no in-progress epic returned %q, want %q (fallback to top)", got.ID, a1.ID)
	}
}

// The JSON shape carries enough context for the agent to claim the leaf:
// id, annotations, and parent_epic when present. Verify the contract since
// scripts depend on it.
func TestRunNextJSONShapeCarriesParentEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(store.CreateIssueInput{Title: "Container", Topic: "next", IssueType: "epic", Priority: 1})
	leaf := h.createIssue(store.CreateIssueInput{Title: "Leaf", Topic: "next", IssueType: "task", Priority: 2, ParentID: epic.ID})

	got := h.runNextJSON()
	if got.ID != leaf.ID {
		t.Fatalf("next.ID = %q, want %q", got.ID, leaf.ID)
	}
	if got.ParentEpic == nil {
		t.Fatal("next.ParentEpic = nil, want populated for leaf under epic")
	}
	if got.ParentEpic.ID != epic.ID {
		t.Fatalf("next.ParentEpic.ID = %q, want %q", got.ParentEpic.ID, epic.ID)
	}
	if got.Annotations == nil {
		t.Fatal("next.Annotations = nil, want non-nil slice (even if empty) so JSON is stable")
	}
}
