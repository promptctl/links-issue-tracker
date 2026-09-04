package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// runNextRow reproduces exactly what `lit next` picks — the shared workable
// pipeline and claim routing — and returns the chosen annotated row. This
// harness never mints a stream token or attributes a write (newTestCLIApp
// opens the store directly, bypassing app.Open's AttributeTo), so every lane
// derives Unclaimed and routing always lands on ServedFromNewLane — the
// claims-aware routing degenerating to exactly the pre-claims "first ready
// row" pick these tests pin. [LAW:single-enforcer]
func (h readyTestHarness) runNextRow() annotation.AnnotatedIssue {
	h.t.Helper()
	annotated, details, err := gatherWorkableAnnotated(h.ctx, h.ap, workableFilter{})
	if err != nil {
		h.t.Fatalf("gatherWorkableAnnotated error = %v", err)
	}
	cc, err := gatherClaimContext(h.ctx, io.Discard, h.ap)
	if err != nil {
		h.t.Fatalf("gatherClaimContext error = %v", err)
	}
	outcome := routeNext(annotated, details, cc.standings, cc.self)
	served, ok := outcome.(ServedFromNewLane)
	if !ok {
		h.t.Fatalf("routeNext = %#v (%T), want ServedFromNewLane (harness carries no claims)", outcome, outcome)
	}
	return served.Row
}

func (h readyTestHarness) runNextErr(args ...string) error {
	h.t.Helper()
	var stdout bytes.Buffer
	return runNext(h.ctx, &stdout, h.ap, args)
}

func (h readyTestHarness) runNextText(args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	if err := runNext(h.ctx, &stdout, h.ap, args); err != nil {
		h.t.Fatalf("runNext(%v) error = %v", args, err)
	}
	return stdout.String()
}

// `lit next` returns the top of the ready partition: the first open, unblocked
// leaf in the same composite-rank order `lit ready` produces.
func TestRunNextReturnsTopReadyLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	first := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "First leaf", Topic: "next", IssueType: "task", Priority: 1})
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Second leaf", Topic: "next", IssueType: "task", Priority: 0})

	got := h.runNextRow()
	if got.ID != first.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order)", got.ID, first.ID)
	}
}

// In-progress leaves are not workable starts; `lit next` skips them and returns
// the next open one. The agent should `lit done` an in-progress leaf, not
// `lit start` it again.
func TestRunNextSkipsInProgressLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	inProgress := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Already started", Topic: "next", IssueType: "task", Priority: 1})
	if _, err := h.ap.Store.Apply(h.ctx, inProgress.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("StartIssue error = %v", err)
	}
	openLeaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Workable", Topic: "next", IssueType: "task", Priority: 0})

	got := h.runNextRow()
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
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Blocker", Topic: "next", IssueType: "task", Priority: 1})
	dependent := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Dependent", Topic: "next", IssueType: "task", Priority: 0})
	h.addDependency(dependent.ID, blocker.ID)
	h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Unblocked third", Topic: "next", IssueType: "task", Priority: 0})

	got := h.runNextRow()
	if got.ID == dependent.ID {
		t.Fatalf("next.ID = %q (dependent), want a non-blocked leaf", got.ID)
	}
	if got.ID != blocker.ID {
		t.Fatalf("next.ID = %q, want %q (top of ready order after skipping blocked dependent)", got.ID, blocker.ID)
	}
}

// `lit next` exposes the standard narrowing knobs so "the next workable bug"
// is expressible; the filter runs in the shared pipeline, so next answers the
// same narrowed question ready/backlog/queue would.
func TestRunNextTypeFilterPicksMatchingLeaf(t *testing.T) {
	h := newReadyTestHarness(t)
	task := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Top-ranked task", Topic: "next", IssueType: "task", Priority: 1})
	bug := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Lower-ranked bug", Topic: "next", IssueType: "bug", Priority: 0})

	text := h.runNextText("--type", "bug")
	if !strings.Contains(text, bug.ID) {
		t.Fatalf("next --type bug output = %q, want %q picked", text, bug.ID)
	}
	if strings.Contains(text, task.ID) {
		t.Fatalf("next --type bug output = %q, want %q filtered out despite outranking the bug", text, task.ID)
	}
}

// --limit and --columns stay off next: a single-row summary has no row count
// or column set to vary, so accepting them would be accepting input the
// command cannot honor.
func TestRunNextRejectsLimitAndColumns(t *testing.T) {
	h := newReadyTestHarness(t)
	for _, args := range [][]string{{"--limit", "2"}, {"--columns", "id"}} {
		err := h.runNextErr(args...)
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("runNext(%v) error = %v, want unknown-flag usage error", args, err)
		}
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

// `--continue` is retired: it predates claim routing, which now subsumes the
// epic-affinity bias unconditionally (routeNext's ServedFromEpicLane step).
// Passing the flag surfaces a pointer to that replacement instead of an
// unhelpful "unknown flag" error.
func TestRunNextContinueFlagIsRetired(t *testing.T) {
	h := newReadyTestHarness(t)
	err := h.runNextErr("--continue")
	if err == nil {
		t.Fatal("runNext(--continue) error = nil, want retirement error")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("runNext(--continue) error = %q, want it to say the flag is retired", err.Error())
	}
}

// A leaf picked by `lit next` carries its parent epic inline so the agent knows
// which epic it would be joining before it claims the leaf.
func TestRunNextCarriesParentEpic(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Container", Topic: "next", IssueType: "epic", Priority: 1})
	leaf := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Leaf", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})

	got := h.runNextRow()
	if got.ID != leaf.ID {
		t.Fatalf("next.ID = %q, want %q", got.ID, leaf.ID)
	}
	if got.ParentEpic == nil {
		t.Fatal("next.ParentEpic = nil, want populated for leaf under epic")
	}
	if got.ParentEpic.ID != epic.ID {
		t.Fatalf("next.ParentEpic.ID = %q, want %q", got.ParentEpic.ID, epic.ID)
	}
}

// Every announcement renderNextOutcome can print, asserted as bytes. The
// outcome is constructed rather than routed to, which is what lets all four
// served variants be reached from a harness that mints no stream token — the
// routing that produces each one is pinned separately in next_route_test.go.
// Standings are left empty deliberately: formatClaimLine stays on its
// ("", false) arm, so nothing but the announcement is under assertion.
func TestRenderNextOutcomeAnnouncesEachClaimEstablishingPick(t *testing.T) {
	h := newReadyTestHarness(t)
	epicA := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "next", IssueType: "epic", Priority: 1})
	fresh := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.1", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a1"})
	inFlight := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "A.2", Topic: "next", IssueType: "task", Priority: 0, ParentID: epicA.ID, Lane: "a2"})
	h.transition(inFlight.ID, model.Start{Assignee: "other"})

	rows, details := h.gather()
	cc := claimContext{self: selfAttribution}
	freshRow := rowByID(t, rows, fresh.ID)
	inFlightRow := rowByID(t, rows, inFlight.ID)

	for _, tc := range []struct {
		name    string
		outcome NextOutcome
		want    string
	}{
		{"a lane already held says nothing", ServedFromClaim{Row: freshRow}, ""},
		{"own work in flight is resumed, not started", ResumedOwnWork{Row: inFlightRow}, "resuming " + inFlight.ID + " — already in progress in a lane you hold"},
		{"the epic's next lane names the claim it establishes", ServedFromEpicLane{Row: freshRow, Epic: epicA.ID, Lane: "A#1"}, "continuing epic " + epicA.ID + ": starting " + fresh.ID + " claims A#1"},
		{"abandoned work is taken over, not started", ServedFromNewLane{Row: inFlightRow, Lane: "A#2"}, "taking over " + inFlight.ID + " (in progress, abandoned) — claims A#2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := renderNextOutcome(&buf, tc.outcome, details, cc); err != nil {
				t.Fatalf("renderNextOutcome(%T) error = %v", tc.outcome, err)
			}
			out := buf.String()
			if tc.want == "" {
				for _, verb := range []string{"resuming", "starting", "continuing", "taking over"} {
					if strings.HasPrefix(out, verb) {
						t.Fatalf("ServedFromClaim announced %q; its contract is that no claim is established and nothing is said", out)
					}
				}
				return
			}
			if !strings.HasPrefix(out, tc.want+"\n") {
				t.Fatalf("renderNextOutcome printed %q, want it to open with %q", out, tc.want)
			}
		})
	}
}
