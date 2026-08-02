package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// runPullableAnnotated reproduces the pullable set an agent can start now — the
// shared workable gather narrowed to rows with no readiness blocker — so the
// lane gate's membership can be observed directly. It classifies through the
// public ClassifyReadiness rather than any retired command's private filter.
func (h readyTestHarness) runPullableAnnotated(rf workableFilter) []annotation.AnnotatedIssue {
	h.t.Helper()
	annotated, _, err := gatherWorkableAnnotated(h.ctx, h.ap, rf)
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated(%+v) error = %v", rf, err)
	}
	var pullable []annotation.AnnotatedIssue
	for _, row := range annotated {
		if ClassifyReadiness(row.Annotations).IsReady() {
			pullable = append(pullable, row)
		}
	}
	return pullable
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

	// The pullable set drops the blocked urgent sibling; only the earlier one is pullable.
	pullable := h.runPullableAnnotated(workableFilter{})
	if !containsID(pullable, first.ID) {
		t.Fatalf("pullable missing earlier sibling %q; got=%v", first.ID, ids(pullable))
	}
	if containsID(pullable, urgentLater.ID) {
		t.Fatalf("pullable contains blocked urgent later sibling %q; got=%v", urgentLater.ID, ids(pullable))
	}

	// next hands back the earlier sibling, never the urgent-but-blocked one.
	if pick := h.runNextRow(false); pick.ID != first.ID {
		t.Fatalf("next = %q, want earlier sibling %q", pick.ID, first.ID)
	}

	// The blocked sibling stays present in the full workable view, annotated with
	// the blocking fact pointing at the earlier sibling — the gate is a readiness
	// classification, not a removal from the backlog.
	workable := h.runWorkableAnnotated(workableFilter{}, 0)
	row := findRow(workable, urgentLater.ID)
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
	pullableAfter := h.runPullableAnnotated(workableFilter{})
	if !containsID(pullableAfter, urgentLater.ID) {
		t.Fatalf("after closing earlier sibling, pullable should contain %q; got=%v", urgentLater.ID, ids(pullableAfter))
	}
	if pick := h.runNextRow(false); pick.ID != urgentLater.ID {
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

	pullable := h.runPullableAnnotated(workableFilter{})
	if !containsID(pullable, first.ID) || !containsID(pullable, otherLane.ID) {
		t.Fatalf("both lanes should be pullable in parallel; got=%v", ids(pullable))
	}
	// Among ready items urgent still orders first — ordering is unchanged.
	if pick := h.runNextRow(false); pick.ID != otherLane.ID {
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
	pullable := h.runPullableAnnotated(workableFilter{Assignee: "alice"})
	if containsID(pullable, mineLater.ID) {
		t.Fatalf("alice's later sibling should be gated by bob's open earlier sibling; got=%v", ids(pullable))
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
