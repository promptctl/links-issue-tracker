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
