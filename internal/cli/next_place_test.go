package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// placeFor answers for every variant of the sealed sum, in both kinds of
// checkout. The expected places are written per variant from the rule — a
// checkout's own work starts where it is, work outside what it holds goes to a
// worktree of its own, and new work leaves the primary clone — not read back off
// placeFor.
func TestPlaceForIsTotalOverTheSealedSum(t *testing.T) {
	want := map[string]struct{ primary, linked startPlace }{
		"the claimed lane":                 {startHere, startHere},
		"own work resumed":                 {startHere, startHere},
		"the epic's next lane":             {startHere, startHere},
		"the global pool":                  {startOffPrimaryClone, startHere},
		"the on-path dependency":           {startHere, startHere},
		"past an exhausted scope":          {startBesideHeldWork, startBesideHeldWork},
		"past a scope the filter narrowed": {startBesideHeldWork, startBesideHeldWork},
		"exhausted carries none":           {startNoPick, startNoPick},
		"narrowed carries none":            {startNoPick, startNoPick},
		"no work carries none":             {startNoPick, startNoPick},
	}
	if len(want) != len(sealedNextOutcomes) {
		t.Fatalf("expectations cover %d outcomes, the sealed sum has %d — add the new one", len(want), len(sealedNextOutcomes))
	}
	for _, tc := range sealedNextOutcomes {
		expected, ok := want[tc.name]
		if !ok {
			t.Fatalf("no expected place for %q", tc.name)
		}
		for _, checkout := range []struct {
			primary bool
			want    startPlace
		}{{true, expected.primary}, {false, expected.linked}} {
			if got := placeFor(tc.outcome, checkout.primary); got != checkout.want {
				t.Errorf("placeFor(%s, primary=%v) = %v, want %v", tc.name, checkout.primary, got, checkout.want)
			}
		}
	}
}

// The start advice opens with the worktree to run `lit start` from only for a
// pick that leaves, so the command and its checkout are one instruction.
func TestStartAdviceNamesTheWorktreeOnlyForAPickThatLeaves(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "next", IssueType: "epic", Priority: 1})
	ticket := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Pick", Topic: "next", IssueType: "task", Priority: 0, ParentID: epic.ID})
	rows, _, _ := h.gather()
	row := rowByID(t, rows, ticket.ID)
	for _, tc := range []struct {
		place startPlace
		want  string
	}{
		{startNoPick, "run `lit start " + ticket.ID + "` to claim it"},
		{startHere, "run `lit start " + ticket.ID + "` to claim it"},
		{startOffPrimaryClone, "from a worktree of its own off the trunk (not the primary clone, where every session starts), run `lit start " + ticket.ID + "` to claim it"},
		{startBesideHeldWork, "from a worktree of its own off the trunk (not this checkout, whose own work is unfinished), run `lit start " + ticket.ID + "` to claim it"},
	} {
		if got := startAdvice(row, model.LaneOf(row.Issue, nil), tc.place); got != tc.want {
			t.Errorf("startAdvice(place %v) = %q, want %q", tc.place, got, tc.want)
		}
	}
}

// Where the pick goes lands on the event beside the checkout kind, and a run
// that served nothing records the place as null rather than leaving it out.
func TestNextRecordsWhereItsPickStarts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		place   startPlace
		primary bool
		want    any
	}{
		{"new work in the primary clone", startOffPrimaryClone, true, "worktree_off_primary_clone"},
		{"past held work in a linked worktree", startBesideHeldWork, false, "worktree_beside_held_work"},
		{"own work", startHere, false, "here"},
		{"no pick", startNoPick, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := telemetry.Begin(telemetry.Event{})
			recordStartPlace(telemetry.With(context.Background(), recorder), tc.place, tc.primary)
			facts := writtenFacts(t, recorder.Finish(telemetry.End{}))
			place, recorded := facts["next.start_place"]
			if !recorded || place != tc.want {
				t.Errorf("next.start_place = %v (recorded %v), want %v", place, recorded, tc.want)
			}
			if facts["next.primary_clone"] != tc.primary {
				t.Errorf("next.primary_clone = %v, want %v", facts["next.primary_clone"], tc.primary)
			}
		})
	}
}

// End to end: the same new work is sent out of the primary clone and started in
// place from a linked worktree that holds nothing, and each run's event says
// which kind of checkout decided it.
func TestNextSendsNewWorkOutOfThePrimaryCloneOnly(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	runGit(t, repo, "config", "user.email", "next@example.com")
	runGit(t, repo, "config", "user.name", "next")
	runGit(t, repo, "commit", "--allow-empty", "-m", "seed")
	epic := newEpicInDir(t, repo, "Home")
	ticket := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "Ready here", "--topic", "demo"))
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-b", "linked", linked)

	for _, tc := range []struct {
		name        string
		dir         string
		wantLine    bool
		wantPlace   string
		wantPrimary bool
	}{
		{"the primary clone", repo, true, "worktree_off_primary_clone", true},
		{"a linked worktree", linked, false, "here", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runCLIInDir(t, tc.dir, "next")
			line := "from a worktree of its own off the trunk (not the primary clone, where every session starts), run `lit start " + ticket + "` to claim"
			if strings.Contains(out, line) != tc.wantLine {
				t.Errorf("output contains %q = %v, want %v\noutput:\n%s", line, !tc.wantLine, tc.wantLine, out)
			}
			ev := lastEvent(t)
			if ev.Facts["next.start_place"] != tc.wantPlace || ev.Facts["next.primary_clone"] != tc.wantPrimary {
				t.Errorf("next.start_place, next.primary_clone = %v, %v; want %s, %v", ev.Facts["next.start_place"], ev.Facts["next.primary_clone"], tc.wantPlace, tc.wantPrimary)
			}
		})
	}
}
