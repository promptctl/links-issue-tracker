package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// seedTaskUnder creates a task with the given title inside the epic parentID,
// returning the new task's id.
func seedTaskUnder(t *testing.T, ctx context.Context, ap *app.App, title, parentID string) string {
	t.Helper()
	return seedTaskIn(t, ctx, ap, title, parentID)
}

// TestCloseRendersLiveAdjacency pins the capture-at-close payload: finishing a
// ticket that sits in a live neighborhood surfaces its parent, the siblings
// still in play, related neighbors, and the dependents the close just unblocked
// — and omits a sibling that is already closed.
func TestCloseRendersLiveAdjacency(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent := seedEpicRaw(t, ctx, ap, "Parent epic", "redirect")
	focal := seedTaskUnder(t, ctx, ap, "Focal ticket", parent)
	openSibling := seedTaskUnder(t, ctx, ap, "Open sibling", parent)
	closedSibling := seedTaskUnder(t, ctx, ap, "Closed sibling", parent)
	related := seedOpenIssueRaw(t, ctx, ap, "Related neighbor")
	dependent := seedOpenIssueRaw(t, ctx, ap, "Dependent ticket")

	// related-to edge focal <-> related.
	if _, err := ap.Store.AddRelation(ctx, storage.AddRelationInput{SrcID: focal, DstID: related, Type: "related-to", CreatedBy: "test"}); err != nil {
		t.Fatalf("AddRelation(related) error = %v", err)
	}
	// blocks convention: SrcID=dependent, DstID=dependency. dependent depends on
	// focal, so closing focal unblocks dependent.
	if _, err := ap.Store.AddRelation(ctx, storage.AddRelationInput{SrcID: dependent, DstID: focal, Type: "blocked-by", CreatedBy: "test"}); err != nil {
		t.Fatalf("AddRelation(blocks) error = %v", err)
	}
	approvePlan(t, ctx, ap, closedSibling)
	approvePlan(t, ctx, ap, focal)
	// Close the sibling that must not appear as live adjacency.
	var sink bytes.Buffer
	if err := runTransition(ctx, &sink, ap, []string{closedSibling}, doneSpec); err != nil {
		t.Fatalf("runTransition(done closedSibling) error = %v", err)
	}

	var out bytes.Buffer
	if err := runTransition(ctx, &out, ap, []string{focal}, doneSpec); err != nil {
		t.Fatalf("runTransition(done focal) error = %v", err)
	}
	text := out.String()

	for _, want := range []string{
		"parent:\n- " + parent,
		"siblings:\n- " + openSibling,
		"related:\n- " + related,
		"unblocks: " + dependent,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("close adjacency missing %q; got:\n%s", want, text)
		}
	}
	if strings.Contains(text, closedSibling) {
		t.Fatalf("closed sibling %s leaked into live adjacency:\n%s", closedSibling, text)
	}
}

// TestCloseWithNoAdjacencyPrintsNothing pins the silent-when-empty contract:
// closing a ticket alone in its epic emits no sibling, related or unblocks
// section at all. The parent group is the one it cannot be without — every
// ticket lives inside an epic — so that is the only group a lonely close
// renders.
func TestCloseWithNoAdjacencyPrintsNothing(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic := seedEpicRaw(t, ctx, ap, "Lonely epic", "redirect")
	id := seedTaskUnder(t, ctx, ap, "Lonely ticket", epic)

	var out bytes.Buffer
	if err := runTransition(ctx, &out, ap, []string{id, "--resolution", "obsolete"}, closeSpec); err != nil {
		t.Fatalf("runTransition(close obsolete) error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "parent:\n- "+epic) {
		t.Fatalf("lonely close omitted the epic it lives inside; got:\n%s", text)
	}
	for _, group := range []string{"siblings:", "related:", "unblocks:"} {
		if strings.Contains(text, group) {
			t.Fatalf("isolated close emitted %q section; got:\n%s", group, text)
		}
	}
}

// TestCloseAdjacencyOnlyOnFinish pins the gate: a non-closing transition
// (reopen) renders no adjacency block even when the ticket has live neighbors.
func TestCloseAdjacencyOnlyOnFinish(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent := seedEpicRaw(t, ctx, ap, "Parent epic", "redirect")
	focal := seedTaskUnder(t, ctx, ap, "Focal ticket", parent)
	_ = seedTaskUnder(t, ctx, ap, "Open sibling", parent)

	// Drive focal to closed, then reopen it; only the reopen output is inspected.
	approvePlan(t, ctx, ap, focal)
	var sink bytes.Buffer
	if err := runTransition(ctx, &sink, ap, []string{focal}, doneSpec); err != nil {
		t.Fatalf("runTransition(done focal) error = %v", err)
	}
	var out bytes.Buffer
	if err := runTransition(ctx, &out, ap, []string{focal}, openSpec); err != nil {
		t.Fatalf("runTransition(reopen focal) error = %v", err)
	}
	if strings.Contains(out.String(), "siblings:") {
		t.Fatalf("reopen emitted an adjacency block; got:\n%s", out.String())
	}
}
