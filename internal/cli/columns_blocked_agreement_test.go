package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// `blocked` is one column name printed by two commands, and for a while it named
// two different questions: `lit backlog` asked the annotation registry, `lit ls`
// asked the dependency edges. A ticket held up by nothing but an earlier sibling
// printed `blocked` on one and `-` on the other, and neither said so
// (links-columns-4hdq). The registry is the single authority on what blocks and
// rendering may not carry a shorter list, so the column now has one producer.
//
// This file is the pin for that. It is deliberately an AGREEMENT test rather
// than a pair of expectations: it reads the same cell off both surfaces and
// requires them to match, so a future predicate that drifts from the registry
// fails here whatever the two happen to say — including in the direction where
// backlog is the one that went wrong.

// blockedCellFromLS runs `lit ls --columns id,blocked` and returns the cell for
// id. It routes through runListWithStore with the harness's own ready policy,
// built exactly as runList builds it for a workspace store, so the
// required-field reason is genuinely in play rather than configured away.
func blockedCellFromLS(h readyTestHarness, id string) string {
	h.t.Helper()
	var out bytes.Buffer
	if err := runListWithStore(h.ctx, &out, h.ap.Store, workspaceReadyPolicy(h.ap), []string{"--columns", "id,blocked"}); err != nil {
		h.t.Fatalf("ls --columns id,blocked: %v", err)
	}
	cells := fieldsOf(lineForID(h.t, out.String(), id))
	if len(cells) != 2 {
		h.t.Fatalf("ls row for %s: want 2 cells, got %d: %v\nfull output:\n%s", id, len(cells), cells, out.String())
	}
	return cells[1]
}

// blockedCellFromBacklog runs `lit backlog --columns id,blocked` and returns the
// cell for id, alongside the whole output so a caller can prove the reason it
// seeded actually fired.
func blockedCellFromBacklog(h readyTestHarness, id string) (cell, out string) {
	h.t.Helper()
	rendered, err := runBacklogColumns(h, "id,blocked")
	if err != nil {
		h.t.Fatalf("backlog --columns id,blocked: %v", err)
	}
	cells := backlogCells(h.t, rendered, id)
	if len(cells) != 2 {
		h.t.Fatalf("backlog row for %s: want 2 cells, got %d: %v\nfull output:\n%s", id, len(cells), cells, rendered)
	}
	return cells[1], rendered
}

// TestBlockedColumnAgreesAcrossSurfaces is the acceptance pin: one fixture
// carrying all four blocking kinds, and for each of them `lit ls` and
// `lit backlog` print the same `blocked` cell.
//
// One fixture rather than four, because the kinds have to coexist to prove
// anything. Three of the four (the sibling gate, the missing field, needs-design)
// leave no dependency edge at all, which is exactly why the old `lit ls` could
// not see them — a per-kind fixture would let a regression that restored the
// dependency-only predicate keep passing three tests out of four while the whole
// column went back to meaning less than its name.
//
// Each row is blocked by exactly ONE kind. A row blocked by two would still
// print `blocked` after a regression removed one of them, so the assertion
// would survive the bug it exists to catch.
func TestBlockedColumnAgreesAcrossSurfaces(t *testing.T) {
	h := newReadyTestHarness(t)
	h.writeReadyConfig("description")

	// The dependency reason, standing alone: no epic, so no sibling gate.
	depBlocker := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Dependency", Topic: "agree", IssueType: "task", Description: "d",
	})
	byDependency := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Blocked by a dependency", Topic: "agree", IssueType: "task", Description: "d",
	})
	h.addDependency(byDependency.ID, depBlocker.ID)

	// The sibling gate: two children of one epic, the later one gated by the
	// earlier. Both carry a description so the field gate stays out of it.
	epic := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Epic", Topic: "agree", IssueType: "epic", Description: "d",
	})
	firstSibling := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "First sibling", Topic: "agree", IssueType: "task", ParentID: epic.ID, Description: "d",
	})
	bySibling := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Second sibling", Topic: "agree", IssueType: "task", ParentID: epic.ID, Description: "d",
	})

	// The required-field gate: the one row with no description.
	byMissingField := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Missing the required field", Topic: "agree", IssueType: "task",
	})

	// needs-design, carried by a label.
	byNeedsDesign := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Needs design", Topic: "agree", IssueType: "task",
		Description: "d", Labels: []string{NeedsDesignLabel},
	})

	// The control: nothing holds it up. Without it, a map populated with
	// `blocked` for every row would satisfy every assertion above it.
	unblocked := h.createIssue(storage.CreateIssueInput{
		Prefix: "agree", Title: "Workable", Topic: "agree", IssueType: "task", Description: "d",
	})

	for _, tc := range []struct {
		kind string
		id   string
		want string
		// fired is text the backlog output must contain for the case to mean
		// anything: it proves the reason this row was seeded for is the reason
		// actually holding it up. Without it a fixture that stopped exercising
		// a kind — a renamed label, a required field that no longer applies —
		// would go on agreeing on "-" and passing.
		fired string
	}{
		{kind: "open dependency", id: byDependency.ID, want: "blocked", fired: "depends on: " + depBlocker.ID},
		{kind: "earlier sibling pending", id: bySibling.ID, want: "blocked", fired: "earlier sibling " + firstSibling.ID + " still open"},
		{kind: "missing required field", id: byMissingField.ID, want: "blocked", fired: "missing description"},
		{kind: "needs design", id: byNeedsDesign.ID, want: "blocked", fired: NeedsDesignLabel},
		{kind: "nothing", id: unblocked.ID, want: "-"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			fromBacklog, rendered := blockedCellFromBacklog(h, tc.id)
			if tc.fired != "" && !strings.Contains(rendered, tc.fired) {
				t.Fatalf("the %s reason never fired for %s, so agreeing about it proves nothing "+
					"— wanted %q in:\n%s", tc.kind, tc.id, tc.fired, rendered)
			}
			fromLS := blockedCellFromLS(h, tc.id)
			if fromLS != fromBacklog {
				t.Errorf("blocked cell for %s (%s): ls says %q, backlog says %q — one column name, "+
					"two answers\nbacklog output:\n%s", tc.id, tc.kind, fromLS, fromBacklog, rendered)
			}
			if fromLS != tc.want {
				t.Errorf("blocked cell for %s (%s) = %q on both surfaces, want %q — they agree on "+
					"the wrong answer", tc.id, tc.kind, fromLS, tc.want)
			}
		})
	}
}

// TestBlockedColumnOnListIsTheRegistrysVerdictNotTheEdges is the same fix stated
// as the narrowest regression it prevents, on the surface that had it. The row
// here carries NO dependency edge, so the pre-fix predicate —
// `len(liveIssues(rel.DependsOn)) > 0` — could only ever answer "-" for it.
//
// It exists beside the agreement test because agreement is satisfiable from
// either side: if a future change made `lit backlog` answer the dependency-only
// question too, both surfaces would agree on "-" and only this assertion would
// notice the column had quietly gone back to meaning less.
func TestBlockedColumnOnListIsTheRegistrysVerdictNotTheEdges(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "edge", Title: "Epic", Topic: "edge", IssueType: "epic"})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "edge", Title: "First", Topic: "edge", IssueType: "task", ParentID: epic.ID})
	gated := h.createIssue(storage.CreateIssueInput{Prefix: "edge", Title: "Second", Topic: "edge", IssueType: "task", ParentID: epic.ID})

	detail, err := h.ap.Store.GetIssueDetail(h.ctx, gated.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(%s): %v", gated.ID, err)
	}
	if len(detail.DependsOn) != 0 {
		t.Fatalf("%s has %d dependency edges; this test is only meaningful with none, "+
			"since the old predicate read exactly those", gated.ID, len(detail.DependsOn))
	}

	if got := blockedCellFromLS(h, gated.ID); got != "blocked" {
		t.Errorf("ls blocked cell for %s = %q, want %q: it is gated by earlier sibling %s, which "+
			"the annotation registry reports and dependency edges cannot",
			gated.ID, got, "blocked", first.ID)
	}
	if got := blockedCellFromLS(h, first.ID); got != "-" {
		t.Errorf("ls blocked cell for %s = %q, want %q — the ungated sibling proves the cell is "+
			"computed rather than set for every row in an epic", first.ID, got, "-")
	}
}
