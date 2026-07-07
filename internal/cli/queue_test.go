package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

type queueTestHarness struct {
	t   *testing.T
	ctx context.Context
	ap  *app.App
}

func newQueueTestHarness(t *testing.T) queueTestHarness {
	t.Helper()
	return queueTestHarness{
		t:   t,
		ctx: context.Background(),
		ap:  newTestCLIApp(t),
	}
}

func (h queueTestHarness) createIssue(input store.CreateIssueInput) (id string) {
	h.t.Helper()
	if input.Prefix == "" {
		input.Prefix = h.ap.Workspace.IssuePrefix.Value()
	}
	// Fixtures author top-to-bottom in listing order, so append at the bottom
	// to make creation order equal rank order (production default is top).
	input.Placement = store.RankBottom
	issue, err := h.ap.Store.CreateIssue(h.ctx, input)
	if err != nil {
		h.t.Fatalf("CreateIssue(%q) error = %v", input.Title, err)
	}
	return issue.ID
}

func (h queueTestHarness) addDependency(dependentID, dependencyID string) {
	h.t.Helper()
	if _, err := h.ap.Store.AddRelation(h.ctx, store.AddRelationInput{
		SrcID: dependentID, DstID: dependencyID, Type: "blocks", CreatedBy: "agent",
	}); err != nil {
		h.t.Fatalf("AddRelation(blocks) error = %v", err)
	}
}

// runQueueIDs renders the queue and extracts the issue ID leading each row, in
// render order — the structured probe over the command's pull-order logic
// (which items survive the not-blocked filter, in what order) read from text.
func (h queueTestHarness) runQueueIDs(args ...string) []string {
	h.t.Helper()
	return issueIDsFromText(h.runQueueText(args...))
}

func (h queueTestHarness) runQueueText(args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	if err := runQueue(h.ctx, &stdout, h.ap, args); err != nil {
		h.t.Fatalf("runQueue(%v) error = %v", args, err)
	}
	return stdout.String()
}

// The defining contract of `lit queue`: blocked items are dropped entirely, not
// kept inline (as `lit backlog` does) — the pull order is exactly the items an
// agent can pull, in rank order.
func TestQueueDropsBlockedItems(t *testing.T) {
	h := newQueueTestHarness(t)
	a := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "A urgent", Topic: "blk", IssueType: "task", Priority: 1})
	b := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "B blocked", Topic: "blk", IssueType: "task", Priority: 1})
	c := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "C normal", Topic: "blk", IssueType: "task", Priority: 0})
	flagged := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "D needs design", Topic: "blk", IssueType: "task", Priority: 0, Labels: []string{NeedsDesignLabel}})
	h.addDependency(b, a) // b is dependency-gated, must be dropped

	got := h.runQueueIDs()
	wantOrder := []string{a, c} // b dropped (open dep), flagged dropped (needs-design)
	if len(got) != len(wantOrder) {
		t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if got[i] != want {
			t.Fatalf("queue[%d].ID = %q, want %q; full order=%v", i, got[i], want, got)
		}
	}
	for _, id := range got {
		if id == b || id == flagged {
			t.Fatalf("blocked item %q must not appear in queue; got=%v", id, got)
		}
	}
}

// in_progress items are unblocked and hold a rank position, so they are part of
// the pull sequence the operator is shaping — kept, not dropped.
func TestQueueKeepsInProgressInRankOrder(t *testing.T) {
	h := newQueueTestHarness(t)
	a := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "A", Topic: "inp", IssueType: "task", Priority: 1})
	b := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "B", Topic: "inp", IssueType: "task", Priority: 1})
	c := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "C", Topic: "inp", IssueType: "task", Priority: 1})
	if _, err := h.ap.Store.Apply(h.ctx, b, store.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("start(%s) error = %v", b, err)
	}

	got := h.runQueueIDs()
	wantOrder := []string{a, b, c}
	if len(got) != len(wantOrder) {
		t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if got[i] != want {
			t.Fatalf("queue[%d].ID = %q, want %q; full order=%v", i, got[i], want, got)
		}
	}
}

// Terseness is the point: no onboarding prose preamble (unlike `lit ready`) and
// no per-row epic/dependency context block (unlike `lit backlog`).
func TestQueueTextHasNoPreambleOrPerRowContext(t *testing.T) {
	h := newQueueTestHarness(t)
	blocker := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "ctx", IssueType: "task", Priority: 1})
	dependent := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "ctx", IssueType: "task", Priority: 1})
	h.addDependency(dependent, blocker)

	text := h.runQueueText()
	if strings.Contains(text, "This is the backlog") || strings.Contains(text, "full backlog in priority/rank order") {
		t.Fatalf("queue must not print an onboarding/backlog preamble; got:\n%s", text)
	}
	if strings.Contains(text, "depends on:") || strings.Contains(text, "unblocks:") || strings.Contains(text, "epic:") {
		t.Fatalf("queue must not print per-row context lines; got:\n%s", text)
	}
	if !strings.Contains(text, blocker) {
		t.Fatalf("expected unblocked item %s in queue; got:\n%s", blocker, text)
	}
	if strings.Contains(text, dependent) {
		t.Fatalf("blocked item %s must be absent from queue; got:\n%s", dependent, text)
	}
}

func TestQueueEmptyDataShowsMarker(t *testing.T) {
	h := newQueueTestHarness(t)
	text := h.runQueueText()
	if !strings.Contains(text, "(queue empty)") {
		t.Fatalf("expected '(queue empty)'; got:\n%s", text)
	}
}

func TestQueueRespectsLimit(t *testing.T) {
	h := newQueueTestHarness(t)
	for i := 0; i < 5; i++ {
		h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "T", Topic: "lim", IssueType: "task", Priority: 1})
	}

	got := h.runQueueIDs("--limit", "2")
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (limit)", len(got))
	}
}
