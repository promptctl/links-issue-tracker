package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// backlogTestHarness mirrors readyTestHarness so the two surfaces can be
// exercised by the same builder helpers. Keeping a separate type clarifies
// which command is under test in each call site.
type backlogTestHarness struct {
	t   *testing.T
	ctx context.Context
	ap  *app.App
}

func newBacklogTestHarness(t *testing.T) backlogTestHarness {
	t.Helper()
	return backlogTestHarness{
		t:   t,
		ctx: context.Background(),
		ap:  newTestCLIApp(t),
	}
}

func (h backlogTestHarness) createIssue(input store.CreateIssueInput) (id string) {
	h.t.Helper()
	if input.Prefix == "" {
		input.Prefix = h.ap.Workspace.IssuePrefix
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

func (h backlogTestHarness) addDependency(dependentID, dependencyID string) {
	h.t.Helper()
	if _, err := h.ap.Store.AddRelation(h.ctx, store.AddRelationInput{
		SrcID: dependentID, DstID: dependencyID, Type: "blocks", CreatedBy: "agent",
	}); err != nil {
		h.t.Fatalf("AddRelation(blocks) error = %v", err)
	}
}

func (h backlogTestHarness) runBacklogJSON(args ...string) []annotation.AnnotatedIssue {
	h.t.Helper()
	var stdout bytes.Buffer
	all := append(append([]string{}, args...), "--json")
	if err := runBacklog(h.ctx, &stdout, h.ap, all); err != nil {
		h.t.Fatalf("runBacklog(%v) error = %v", all, err)
	}
	var got []annotation.AnnotatedIssue
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		h.t.Fatalf("json.Unmarshal(backlog) error = %v", err)
	}
	return got
}

func (h backlogTestHarness) runBacklogText(args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	if err := runBacklog(h.ctx, &stdout, h.ap, args); err != nil {
		h.t.Fatalf("runBacklog(%v) error = %v", args, err)
	}
	return stdout.String()
}

// Backlog must keep blocked items at their ranked position rather than push
// them to the bottom — that's the whole reason it exists alongside `lit ready`.
func TestBacklogKeepsBlockedItemsInRankOrder(t *testing.T) {
	h := newBacklogTestHarness(t)
	a := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "A urgent", Topic: "blk", IssueType: "task", Priority: 1})
	b := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "B urgent", Topic: "blk", IssueType: "task", Priority: 1})
	c := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "C normal", Topic: "blk", IssueType: "task", Priority: 0})
	h.addDependency(b, a) // b is blocked

	got := h.runBacklogJSON()
	wantOrder := []string{a, b, c}
	if len(got) != len(wantOrder) {
		t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(wantOrder), gotIDs(got))
	}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Fatalf("backlog[%d].ID = %q, want %q; full order=%v", i, got[i].ID, want, gotIDs(got))
		}
	}
}

func TestBacklogTextShowsBlockedReasonsInline(t *testing.T) {
	h := newBacklogTestHarness(t)
	blocker := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "blk", IssueType: "task", Priority: 1})
	blocked := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Blocked", Topic: "blk", IssueType: "task", Priority: 1})
	flagged := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Needs design", Topic: "blk", IssueType: "task", Priority: 0, Labels: []string{NeedsDesignLabel}})
	h.addDependency(blocked, blocker)

	text := h.runBacklogText()
	if !strings.Contains(text, "depends on: "+blocker) {
		t.Fatalf("expected 'depends on: %s' for blocked item; got:\n%s", blocker, text)
	}
	if !strings.Contains(text, "unblocks: "+blocked) {
		t.Fatalf("expected 'unblocks: %s' on blocker; got:\n%s", blocked, text)
	}
	if !strings.Contains(text, "blocked: needs-design") {
		t.Fatalf("expected 'blocked: needs-design' line for %s; got:\n%s", flagged, text)
	}
}

func TestBacklogTextShowsPreamble(t *testing.T) {
	h := newBacklogTestHarness(t)
	h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "Anything", Topic: "any", IssueType: "task", Priority: 1})

	text := h.runBacklogText()
	if !strings.Contains(text, "full backlog in priority/rank order") {
		t.Fatalf("missing preamble; got:\n%s", text)
	}
	if !strings.Contains(text, "─") {
		t.Fatalf("missing separator; got:\n%s", text)
	}
}

func TestBacklogEmptyDataShowsMarker(t *testing.T) {
	h := newBacklogTestHarness(t)
	text := h.runBacklogText()
	if !strings.Contains(text, "(backlog empty)") {
		t.Fatalf("expected '(backlog empty)'; got:\n%s", text)
	}
}

func TestBacklogRespectsLimit(t *testing.T) {
	h := newBacklogTestHarness(t)
	for i := 0; i < 5; i++ {
		h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "T", Topic: "lim", IssueType: "task", Priority: 1})
	}

	got := h.runBacklogJSON("--limit", "2")
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (limit)", len(got))
	}
}

func TestBacklogIncludesInProgressInline(t *testing.T) {
	h := newBacklogTestHarness(t)
	a := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "A", Topic: "inp", IssueType: "task", Priority: 1})
	b := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "B", Topic: "inp", IssueType: "task", Priority: 1})
	c := h.createIssue(store.CreateIssueInput{Prefix: "test", Title: "C", Topic: "inp", IssueType: "task", Priority: 1})
	if _, err := h.ap.Store.TransitionIssue(h.ctx, store.TransitionIssueInput{IssueID: b, Action: "start", CreatedBy: "tester", Assignee: "tester"}); err != nil {
		t.Fatalf("start(%s) error = %v", b, err)
	}

	got := h.runBacklogJSON()
	wantOrder := []string{a, b, c}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3; got=%v", len(got), gotIDs(got))
	}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Fatalf("backlog[%d].ID = %q, want %q; full order=%v", i, got[i].ID, want, gotIDs(got))
		}
	}

	text := h.runBacklogText()
	if !strings.Contains(text, "in_progress:") {
		t.Fatalf("expected 'in_progress:' suffix for started item; got:\n%s", text)
	}
}

func gotIDs(rows []annotation.AnnotatedIssue) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}
