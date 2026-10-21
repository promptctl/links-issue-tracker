package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// placeFor answers for every variant of the sealed sum, in both kinds of
// checkout. The expected places are written per variant from the rule — a
// checkout's own work starts where it is, work outside what it holds goes to a
// worktree of its own, and new work leaves the main clone — not read back off
// placeFor.
func TestPlaceForIsTotalOverTheSealedSum(t *testing.T) {
	want := map[string]struct{ main, linked startPlace }{
		"the claimed lane":                 {startHere, startHere},
		"own work resumed":                 {startHere, startHere},
		"the epic's next lane":             {startHere, startHere},
		"the global pool":                  {startOffMainClone, startHere},
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
			main bool
			want startPlace
		}{{true, expected.main}, {false, expected.linked}} {
			got := placeFor(tc.outcome, checkout.main)
			if got.Place != checkout.want {
				t.Errorf("placeFor(%s, main=%v) = %v, want %v", tc.name, checkout.main, got.Place, checkout.want)
			}
			wantTicket := ""
			if tc.served {
				wantTicket = sealedSumRow.ID
			}
			if got.Ticket != wantTicket {
				t.Errorf("placeFor(%s, main=%v).Ticket = %q, want %q", tc.name, checkout.main, got.Ticket, wantTicket)
			}
		}
	}
}

// The line under a pick says where to start it only when that is somewhere
// other than here, and names the ticket in the command it gives.
func TestStartPlaceNoteDirectsOnlyAPickThatLeaves(t *testing.T) {
	for _, tc := range []struct {
		place startPlace
		want  string
	}{
		{startNoPick, ""},
		{startHere, ""},
		{startOffMainClone, "start test-1 from a worktree of its own, not the main clone, where every session starts: create one off the trunk and run `lit start test-1` from inside it — the claim lands on the checkout `lit start` runs in\n"},
		{startBesideHeldWork, "start test-1 from a worktree of its own, not this checkout, whose own work is unfinished: create one off the trunk and run `lit start test-1` from inside it — the claim lands on the checkout `lit start` runs in\n"},
	} {
		if got := (startPlacement{Place: tc.place, Ticket: "test-1"}).note(); got != tc.want {
			t.Errorf("note(%v) = %q, want %q", tc.place, got, tc.want)
		}
	}
}

// Where the pick goes lands on the event beside the checkout kind, and a run
// that served nothing records the place as null rather than leaving it out.
func TestNextRecordsWhereItsPickStarts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		placement startPlacement
		main      bool
		want      any
	}{
		{"new work in the main clone", startPlacement{Place: startOffMainClone, Ticket: "test-1"}, true, "worktree_off_main_clone"},
		{"past held work in a linked worktree", startPlacement{Place: startBesideHeldWork, Ticket: "test-1"}, false, "worktree_beside_held_work"},
		{"own work", startPlacement{Place: startHere, Ticket: "test-1"}, false, "here"},
		{"no pick", startPlacement{Place: startNoPick}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := telemetry.Begin(telemetry.Event{})
			recordStartPlace(telemetry.With(context.Background(), recorder), tc.placement, tc.main)
			facts := writtenFacts(t, recorder.Finish(telemetry.End{}))
			place, recorded := facts["next.start_place"]
			if !recorded || place != tc.want {
				t.Errorf("next.start_place = %v (recorded %v), want %v", place, recorded, tc.want)
			}
			if facts["next.main_checkout"] != tc.main {
				t.Errorf("next.main_checkout = %v, want %v", facts["next.main_checkout"], tc.main)
			}
		})
	}
}

// End to end: the same new work is sent out of the main clone and started in
// place from a linked worktree that holds nothing, and each run's event says
// which kind of checkout decided it.
func TestNextSendsNewWorkOutOfTheMainCloneOnly(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	runGit(t, repo, "commit", "--allow-empty", "-m", "seed")
	epic := newEpicInDir(t, repo, "Home")
	ticket := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "Ready here", "--topic", "demo"))
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-b", "linked", linked)

	for _, tc := range []struct {
		name      string
		dir       string
		wantLine  bool
		wantPlace string
		wantMain  bool
	}{
		{"the main clone", repo, true, "worktree_off_main_clone", true},
		{"a linked worktree", linked, false, "here", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runCLIInDir(t, tc.dir, "next")
			line := "start " + ticket + " from a worktree of its own, not the main clone"
			if strings.Contains(out, line) != tc.wantLine {
				t.Errorf("output contains %q = %v, want %v\noutput:\n%s", line, !tc.wantLine, tc.wantLine, out)
			}
			ev := lastEvent(t)
			if ev.Facts["next.start_place"] != tc.wantPlace || ev.Facts["next.main_checkout"] != tc.wantMain {
				t.Errorf("next.start_place, next.main_checkout = %v, %v; want %s, %v", ev.Facts["next.start_place"], ev.Facts["next.main_checkout"], tc.wantPlace, tc.wantMain)
			}
		})
	}
}
