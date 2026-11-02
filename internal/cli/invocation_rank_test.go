package cli

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// A command that builds tree order more than once over the same damage puts
// each damaged ticket on its event once, and a build that met nothing new adds
// nothing.
func TestEachDamagedTicketGoesOnTheEventOnce(t *testing.T) {
	t.Parallel()
	recorder := telemetry.Begin(telemetry.Event{})
	met := newAncestryMet(telemetry.With(context.Background(), recorder))
	shape := storage.AncestryShape{
		Deepest:   3,
		Conflicts: []storage.ParentConflict{{ChildID: "c", Parents: []string{"p1", "p2"}}},
		Loops:     []string{"l1", "l2"},
	}
	met.record(shape)
	met.record(shape)
	met.record(storage.AncestryShape{Loops: []string{"l3"}})
	ev := recorder.Finish(telemetry.End{})
	if got := len(ev.Occurrences["rank.ancestry_conflicts"]); got != 1 {
		t.Errorf("rank.ancestry_conflicts = %v, want c once", ev.Occurrences["rank.ancestry_conflicts"])
	}
	var loops []string
	for _, entry := range ev.Occurrences["rank.ancestry_loops"] {
		loops = append(loops, entry.(map[string]any)["rank.ancestry_loop.child"].(string))
	}
	if !slices.Equal(loops, []string{"l1", "l2", "l3"}) {
		t.Errorf("rank.ancestry_loops = %v, want each loop ticket once", loops)
	}
	if ev.Counts["rank.ancestry_deepest"] != 3 {
		t.Errorf("rank.ancestry_deepest = %d, want 3", ev.Counts["rank.ancestry_deepest"])
	}
}

// What a write did to rank keys is on its event, and a command that wrote none
// says 0 and [], not nothing.
func TestAWriteSaysWhatItDidToRankKeys(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	newEpicInDir(t, repo, "First")
	if ev := lastEvent(t); ev.Counts["rank.longest_key"] == 0 {
		t.Errorf("lit new counts = %v, want the key it wrote", ev.Counts)
	}
	runCLIInDir(t, repo, "backlog")
	ev := lastEvent(t)
	longest, counted := ev.Counts["rank.longest_key"]
	respaces, listed := ev.Occurrences["rank.respaces"]
	if !counted || longest != 0 || !listed || len(respaces) != 0 {
		t.Fatalf("lit backlog counts = %v, occurrences = %v; want rank.longest_key 0 and rank.respaces []", ev.Counts, ev.Occurrences)
	}

	// One spot, filed over and over, until its gaps are spent.
	anchor := newEpicInDir(t, repo, "Anchor")
	for i := range 80 {
		filed := newEpicInDir(t, repo, fmt.Sprintf("Crowding %d", i))
		runCLIInDir(t, repo, "rank", filed, "--above", anchor)
		anchor = filed
		ev := lastEvent(t)
		respaces := ev.Occurrences["rank.respaces"]
		if len(respaces) == 0 {
			continue
		}
		respace, ok := respaces[0].(map[string]any)
		if !ok {
			t.Fatalf("rank.respaces[0] = %#v", respaces[0])
		}
		for _, key := range []string{"rank.respace.rows", "rank.respace.rewritten", "rank.respace.longest_key", "rank.respace.ceiling"} {
			if value, ok := respace[key].(float64); !ok || value <= 0 {
				t.Errorf("%s = %#v, want a positive number", key, respace[key])
			}
		}
		if ev.Counts["rank.longest_key"] == 0 {
			t.Errorf("a respace's event counts = %v, want the longest key written", ev.Counts)
		}
		return
	}
	t.Fatal("80 placements at one spot never respaced")
}

// What tree order cost a read is on its event, added across its queries, a
// command that read no parents says 0, not nothing, and a read of every parent
// edge is on the event like a climb, because it is the read that grows with the
// store.
func TestAReadSaysWhatTreeOrderCostIt(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	first := newEpicInDir(t, repo, "First")
	second := newEpicInDir(t, repo, "Second")
	third := newEpicInDir(t, repo, "Third")
	runCLIInDir(t, repo, "quickstart")
	ev := lastEvent(t)
	levels, counted := ev.Counts["rank.ancestry_levels"]
	links, linked := ev.Counts["rank.ancestry_links"]
	if !counted || levels != 0 || !linked || links != 0 {
		t.Fatalf("lit quickstart counts = %v, want rank.ancestry_levels 0 and rank.ancestry_links 0", ev.Counts)
	}

	leaf := func(parent, title string) string {
		t.Helper()
		return extractTicketID(t, runCLIInDir(t, repo, "new", "--title", title, "--topic", "demo", "--type", "task", "--parent", parent))
	}
	subject := leaf(third, "subject")
	for _, parent := range []string{first, second} {
		runCLIInDir(t, repo, "dep", "add", subject, "--blocked-by", leaf(parent, "blocker"))
	}
	runCLIInDir(t, repo, "close", subject, "--resolution", "wontfix", "--reason", "measured")
	ev = lastEvent(t)
	if ev.Counts["rank.ancestry_levels"] != 2 || ev.Counts["rank.ancestry_links"] != 2 {
		t.Errorf("lit close counts = %v, want the two levels and two edges its blockers climbed and no whole read", ev.Counts)
	}
	runCLIInDir(t, repo, "backlog")
	ev = lastEvent(t)
	if ev.Counts["rank.ancestry_levels"] < 1 || ev.Counts["rank.ancestry_links"] < 3 {
		t.Errorf("lit backlog counts = %v, want the whole read and at least the three parent edges", ev.Counts)
	}
}

// What a rank set decided is on its event: the frame, the issues named and the
// keys it wrote, and a repeat of an order that has arrived says it wrote none.
func TestARankSetSaysWhatItWrote(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	first := newEpicInDir(t, repo, "First")
	second := newEpicInDir(t, repo, "Second")
	third := newEpicInDir(t, repo, "Third")

	runCLIInDir(t, repo, "rank", "set", third, first, second)
	facts := lastEvent(t).Facts
	for _, key := range []string{"rank_set.rewritten", "rank_set.touched"} {
		if n, _ := facts[key].(float64); n == 0 {
			t.Errorf("a reordering set: %s = %#v, want a positive count (facts %v)", key, facts[key], facts)
		}
	}
	if facts["rank_set.frame"] != "" || facts["rank_set.named"] != float64(3) {
		t.Errorf("a reordering set's facts = %v, want frame \"\" (top level) and 3 named", facts)
	}

	runCLIInDir(t, repo, "rank", "set", third, first, second)
	facts = lastEvent(t).Facts
	for key, want := range map[string]any{
		"rank_set.frame": "", "rank_set.named": float64(3), "rank_set.rewritten": float64(0),
		"rank_set.touched": float64(0), "rank_set.minted": float64(0),
	} {
		if facts[key] != want {
			t.Errorf("a repeated set: %s = %#v, want %#v", key, facts[key], want)
		}
	}
}

// A rank set refused over a tie names the pair on its event.
func TestARankSetRefusedOverATieNamesThePair(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	ids := []string{newEpicInDir(t, repo, "One"), newEpicInDir(t, repo, "Two")}
	slices.Sort(ids)
	low, high := ids[0], ids[1]
	ctx := context.Background()
	ap, err := app.Open(ctx, testEnv(), testStderr(t), resolvedWorkspace(t, repo), app.AccessWrite)
	if err != nil {
		t.Fatalf("app.Open() error = %v", err)
	}
	raw, err := storage.TestSupport.Of(ap.Store)
	if err != nil {
		t.Fatalf("TestSupport: %v", err)
	}
	if err := raw.ExecRawForTest(ctx, `UPDATE issues SET item_rank = 'M' WHERE id IN (?, ?)`, low, high); err != nil {
		t.Fatalf("plant tie: %v", err)
	}
	if err := ap.Close(); err != nil {
		t.Fatalf("close app: %v", err)
	}

	out, err := runCLIInDirAllowError(t, repo, "rank", "set", high, low)
	if err == nil {
		t.Fatalf("rank set over a reversed tie succeeded:\n%s", out)
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Errorf("exit code = %d, want %d; error = %v", got, ExitValidation, err)
	}
	pair, _ := lastEvent(t).Facts["rank_set.refused_tie"].([]any)
	if len(pair) != 2 || pair[0] != high || pair[1] != low {
		t.Errorf("rank_set.refused_tie = %#v, want [%s %s]", pair, high, low)
	}
}

// A listing's event carries the deepest chain its ancestry held, and a command
// that built none says 0, and [] for second parents and loops.
func TestAListingSaysHowDeepItsAncestryWent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	runCLIInDir(t, repo, "quickstart")
	ev := lastEvent(t)
	if deepest, counted := ev.Counts["rank.ancestry_deepest"]; !counted || deepest != 0 {
		t.Errorf("lit quickstart counts = %v, want rank.ancestry_deepest 0", ev.Counts)
	}
	for _, key := range []string{"rank.ancestry_conflicts", "rank.ancestry_loops"} {
		if entries, listed := ev.Occurrences[key]; !listed || len(entries) != 0 {
			t.Errorf("lit quickstart %s = %v (listed %v), want []", key, entries, listed)
		}
	}

	_, inner := newNestedEpicsInDir(t, repo)
	runCLIInDir(t, repo, "new", "--title", "leaf", "--topic", "demo", "--type", "task", "--parent", inner)
	runCLIInDir(t, repo, "backlog")
	if got := lastEvent(t).Counts["rank.ancestry_deepest"]; got != 2 {
		t.Errorf("lit backlog over a leaf two levels down: rank.ancestry_deepest = %d, want 2", got)
	}
}
