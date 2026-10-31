package cli

import (
	"fmt"
	"testing"
)

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

// What tree order cost a read is on its event, added across the levels it
// climbed, and a command that climbed nothing says 0, not nothing.
func TestAReadSaysWhatTreeOrderCostIt(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	first := newEpicInDir(t, repo, "First")
	second := newEpicInDir(t, repo, "Second")
	third := newEpicInDir(t, repo, "Third")
	runCLIInDir(t, repo, "backlog")
	ev := lastEvent(t)
	levels, counted := ev.Counts["rank.ancestry_levels"]
	links, linked := ev.Counts["rank.ancestry_links"]
	if !counted || levels != 0 || !linked || links != 0 {
		t.Fatalf("lit backlog counts = %v, want rank.ancestry_levels 0 and rank.ancestry_links 0", ev.Counts)
	}

	leaf := func(parent, title string) string {
		t.Helper()
		return extractTicketID(t, runCLIInDir(t, repo, "new", "--title", title, "--topic", "demo", "--type", "task", "--parent", parent))
	}
	subject := leaf(third, "subject")
	for _, parent := range []string{first, second} {
		runCLIInDir(t, repo, "dep", "add", subject, "--blocked-by", leaf(parent, "blocker"))
	}
	runCLIInDir(t, repo, "show", subject)
	ev = lastEvent(t)
	if ev.Counts["rank.ancestry_levels"] < 2 || ev.Counts["rank.ancestry_links"] < 2 {
		t.Errorf("lit show counts = %v, want the levels and the edges its two blockers climbed, added up", ev.Counts)
	}
}
