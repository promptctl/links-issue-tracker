package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// runQueueJSON runs `lit queue --json` against the ready harness so the lane
// gate can be observed across all three membership surfaces (ready/queue/next)
// from one fixture.
func (h readyTestHarness) runQueueJSON(args ...string) []annotation.AnnotatedIssue {
	h.t.Helper()
	var stdout bytes.Buffer
	all := append(append([]string{}, args...), "--json")
	if err := runQueue(h.ctx, newOutputModeWriter(&stdout, outputModeText), h.ap, all); err != nil {
		h.t.Fatalf("runQueue(%v) error = %v", all, err)
	}
	var got []annotation.AnnotatedIssue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		h.t.Fatalf("json.Unmarshal(queue) error = %v", err)
	}
	return got
}

func containsID(rows []annotation.AnnotatedIssue, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// The core defect the epic targets: an urgent later sibling that depends (by
// intra-epic rank) on an unfinished earlier sibling must NOT surface ahead of
// it. The fix makes the later sibling a non-member of the ready set while the
// earlier one is open — priority ordering is untouched.
func TestLaneGateUrgentLaterSiblingBlockedByOpenEarlierSibling(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "lane", IssueType: "epic", Priority: 1})
	first := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "first", Topic: "lane", IssueType: "task", Priority: 0, ParentID: epic.ID})
	urgentLater := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "urgent later", Topic: "lane", IssueType: "task", Priority: 1, ParentID: epic.ID})

	// queue drops the blocked urgent sibling; only the earlier one is pullable.
	queue := h.runQueueJSON()
	if !containsID(queue, first.ID) {
		t.Fatalf("queue missing earlier sibling %q; got=%v", first.ID, ids(queue))
	}
	if containsID(queue, urgentLater.ID) {
		t.Fatalf("queue contains blocked urgent later sibling %q; got=%v", urgentLater.ID, ids(queue))
	}

	// next hands back the earlier sibling, never the urgent-but-blocked one.
	if pick := h.runNextJSON(); pick.ID != first.ID {
		t.Fatalf("next = %q, want earlier sibling %q", pick.ID, first.ID)
	}

	// ready sinks the blocked sibling below the ready one and annotates it with
	// the blocking fact pointing at the earlier sibling.
	ready := h.runReadyJSON()
	if ready[0].ID != first.ID {
		t.Fatalf("ready[0] = %q, want earlier sibling %q; order=%v", ready[0].ID, first.ID, ids(ready))
	}
	row := findRow(ready, urgentLater.ID)
	ann, ok := findAnnotation(row.Annotations, annotation.EarlierSiblingPending)
	if !ok {
		t.Fatalf("urgent later sibling missing EarlierSiblingPending annotation; annotations=%v", row.Annotations)
	}
	if ann.Message != first.ID {
		t.Fatalf("EarlierSiblingPending message = %q, want earlier sibling id %q", ann.Message, first.ID)
	}
	if ClassifyReadiness(row.Annotations).IsReady() {
		t.Fatal("urgent later sibling should be ready-blocked while earlier sibling is open")
	}

	// The moment the earlier sibling closes, the later one becomes pullable —
	// membership flips on the close, no rank or priority change needed.
	h.closeIssue(first.ID, "done")
	queueAfter := h.runQueueJSON()
	if !containsID(queueAfter, urgentLater.ID) {
		t.Fatalf("after closing earlier sibling, queue should contain %q; got=%v", urgentLater.ID, ids(queueAfter))
	}
	if pick := h.runNextJSON(); pick.ID != urgentLater.ID {
		t.Fatalf("after closing earlier sibling, next = %q, want %q", pick.ID, urgentLater.ID)
	}
}

// A sibling in a different lane runs in parallel: it is pullable regardless of
// an open earlier-ranked sibling in another lane. Distinct lane per child is
// the fully-parallel degenerate case; the old binary "parallel opt-out" is just
// "give it a lane nobody else shares".
func TestLaneGateDistinctLaneRunsInParallel(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "lane", IssueType: "epic", Priority: 1})
	first := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "first", Topic: "lane", IssueType: "task", Priority: 0, ParentID: epic.ID, Lane: "a"})
	otherLane := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "other lane", Topic: "lane", IssueType: "task", Priority: 1, ParentID: epic.ID, Lane: "b"})

	queue := h.runQueueJSON()
	if !containsID(queue, first.ID) || !containsID(queue, otherLane.ID) {
		t.Fatalf("both lanes should be pullable in parallel; got=%v", ids(queue))
	}
	// Among ready items urgent still orders first — ordering is unchanged.
	if pick := h.runNextJSON(); pick.ID != otherLane.ID {
		t.Fatalf("next = %q, want urgent distinct-lane sibling %q (ordering among ready)", pick.ID, otherLane.ID)
	}
}

// The flagged caveat: the sibling index must see EVERY sibling, including ones
// hidden from the current view by an assignee filter. An earlier sibling owned
// by someone else still gates my later same-lane sibling — otherwise the gate
// would leak whenever work is split across assignees.
func TestLaneGateSeesSiblingsHiddenByAssigneeFilter(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "lane", IssueType: "epic", Priority: 1})
	_ = h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "bob's first", Topic: "lane", IssueType: "task", Priority: 0, ParentID: epic.ID, Assignee: "bob"})
	mineLater := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "my later", Topic: "lane", IssueType: "task", Priority: 1, ParentID: epic.ID, Assignee: "alice"})

	// Viewing only alice's work, her later sibling is still gated by bob's open
	// earlier sibling even though bob's ticket is filtered out of the view.
	queue := h.runQueueJSON("--assignee", "alice")
	if containsID(queue, mineLater.ID) {
		t.Fatalf("alice's later sibling should be gated by bob's open earlier sibling; got=%v", ids(queue))
	}
}

func ids(rows []annotation.AnnotatedIssue) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func findRow(rows []annotation.AnnotatedIssue, id string) annotation.AnnotatedIssue {
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	return annotation.AnnotatedIssue{}
}
