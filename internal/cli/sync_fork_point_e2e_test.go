package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newForkedBelowTheCut is the shared pair after alpha wrote past the head
// bravo adopted, cut its history at its own head — a commit bravo never saw,
// so bravo cannot follow — force-pushed it, and pushed an edit to the
// ticket's description on top. bravo has edited the ticket's title without
// pushing. adopted is the head bravo adopted: the remote head it last knew.
func newForkedBelowTheCut(t *testing.T) (r reRootedCheckouts, adopted string) {
	t.Helper()
	return newForkedBelowTheCutAfter(t, func(r reRootedCheckouts) {
		runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
	})
}

// newForkedBelowTheCutAfter is newForkedBelowTheCut with bravoWrites in place
// of bravo's title edit: what bravo does between adopting the remote and
// alpha's re-root.
func newForkedBelowTheCutAfter(t *testing.T, bravoWrites func(reRootedCheckouts)) (r reRootedCheckouts, adopted string) {
	t.Helper()
	r, base := newSharedCheckouts(t)
	adopted, _ = syncHeadAndBranch(t, r.bravo)
	bravoWrites(r)
	for _, title := range []string{"past-1", "past-2"} {
		runCLIInDir(t, r.alpha, "new", "--title", title, "--topic", "demo", "--type", "task", "--parent", r.epic)
	}
	runCLIInDir(t, r.alpha, "sync", "push")
	r.cut, _ = syncHeadAndBranch(t, r.alpha)
	r.root = reRootAlphaAndForcePush(t, r, r.cut)
	runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
	runCLIInDir(t, r.alpha, "sync", "push")
	finishSharedCheckouts(t, r, base)
	return r, adopted
}

// TestEverySettlingEntryPointMergesAForkBelowTheCut drives each entry point
// that settles a divergence in bravo against the remote alpha re-rooted
// above every commit bravo holds. The fetch records the fork point before it
// moves the tracking ref, the reconcile merges against it, bravo holds both
// edits with nothing reporting unrelated histories, and the events name the
// fork point and the base.
func TestEverySettlingEntryPointMergesAForkBelowTheCut(t *testing.T) {
	for _, entry := range []struct {
		name string
		// fetch and settle are the commands whose events carry the fork
		// point's record and the reconcile's base: one command for an entry
		// point that does both.
		fetch, settle []string
	}{
		{name: "pull", settle: []string{"sync", "pull"}},
		{name: "receive", settle: []string{"sync", backgroundReceiveSubcommand, "--parent-pid", "0"}},
		{name: "reconcile", fetch: []string{"sync", "fetch"}, settle: []string{"sync", "reconcile"}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			r, adopted := newForkedBelowTheCut(t)
			if entry.fetch != nil {
				runCLIInDir(t, r.bravo, entry.fetch...)
				assertForkPointRecorded(t, adopted)
			}
			runCLIInDir(t, r.bravo, entry.settle...)
			if entry.fetch == nil {
				assertForkPointRecorded(t, adopted)
			}
			ev := lastEvent(t)
			bases := ev.Occurrences["reconcile.bases"]
			if len(bases) != 1 {
				t.Fatalf("event %q reconcile.bases = %v; want the one reconcile's base", ev.Event, ev.Occurrences["reconcile.bases"])
			}
			base, _ := bases[0].(map[string]any)
			if base["reconcile.base"] != "fork_point" || base["reconcile.base_commit"] != adopted || base["reconcile.fork_point"] != adopted {
				t.Fatalf("event %q base = %v; want fork_point at the adopted head %s", ev.Event, base, adopted)
			}
			r.assertFollowedCleanly(t, nil, true)
		})
	}
}

// TestAGitRemoteWithANewURLKeepsItsForkPoint changes bravo's git remote URL
// after the fetch that brought the re-rooted history. The next sync re-points
// the Dolt remote to match, and since it is the same remote the fork point
// stays and the pull merges against it.
func TestAGitRemoteWithANewURLKeepsItsForkPoint(t *testing.T) {
	r, adopted := newForkedBelowTheCut(t)
	runCLIInDir(t, r.bravo, "sync", "fetch")
	assertForkPointRecorded(t, adopted)
	url, err := exec.Command("git", "-C", r.bravo, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("read bravo's git remote: %v", err)
	}
	link := filepath.Join(t.TempDir(), "remote-link.git")
	if err := os.Symlink(strings.TrimSpace(string(url)), link); err != nil {
		t.Fatalf("link the remote: %v", err)
	}
	runGit(t, r.bravo, "remote", "set-url", "origin", link)
	runCLIInDir(t, r.bravo, "sync", "pull")
	bases := lastEvent(t).Occurrences["reconcile.bases"]
	if len(bases) != 1 {
		t.Fatalf("reconcile.bases = %v; want the one reconcile's base", bases)
	}
	if base, _ := bases[0].(map[string]any); base["reconcile.base"] != "fork_point" || base["reconcile.fork_point"] != adopted {
		t.Fatalf("base = %v; want fork_point at the adopted head %s", base, adopted)
	}
	r.assertFollowedCleanly(t, nil, true)
}

// assertForkPointRecorded checks the last command's event recorded the fork
// point at the head bravo adopted, before its fetch moved the tracking ref.
func assertForkPointRecorded(t *testing.T, adopted string) {
	t.Helper()
	ev := lastEvent(t)
	records := ev.Occurrences["fork_point.records"]
	if len(records) == 0 {
		t.Fatalf("event %q fork_point.records = %v; want the fetch's record", ev.Event, ev.Occurrences["fork_point.records"])
	}
	record, _ := records[0].(map[string]any)
	if record["fork_point.outcome"] != "recorded" || record["fork_point.head"] != adopted || record["fork_point.branch"] != "master" {
		t.Fatalf("event %q fork point record = %v; want recorded at the adopted head %s", ev.Event, record, adopted)
	}
}

// TestAQuietForkReceivesOntoTheRemoteHead is bravo with nothing of its own
// past the head it adopted, receiving the history alpha re-rooted above it.
// The receive lands bravo on alpha's head, reconciles nothing, and its event
// says it landed across the fork point.
func TestAQuietForkReceivesOntoTheRemoteHead(t *testing.T) {
	r, _ := newForkedBelowTheCutAfter(t, func(reRootedCheckouts) {})
	runCLIInDir(t, r.bravo, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
	ev := lastEvent(t)
	outcomes := ev.Occurrences["receive.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("event %q receive.outcomes = %v; want the one receive's outcome", ev.Event, ev.Occurrences["receive.outcomes"])
	}
	if outcome, _ := outcomes[0].(map[string]any); outcome["receive.decision"] != "fast_forwarded" || outcome["receive.base"] != "fork_point" ||
		outcome["receive.ahead"] != float64(0) || !(outcome["receive.behind"].(float64) > 0) {
		t.Fatalf("receive outcome = %v; want fast_forwarded across the fork_point base, nothing ahead and alpha's history behind", outcome)
	}
	if stranded, present := ev.Occurrences["stranded_writes.commits"]; !present || len(stranded) != 0 {
		t.Fatalf("stranded_writes.commits = %#v; want an empty list: bravo stranded no write", ev.Occurrences["stranded_writes.commits"])
	}
	if bases := ev.Occurrences["reconcile.bases"]; len(bases) != 0 {
		t.Fatalf("reconcile.bases = %v; want none: bravo has nothing to replay", bases)
	}
	alphaHead, _ := syncHeadAndBranch(t, r.alpha)
	if bravoHead, _ := syncHeadAndBranch(t, r.bravo); bravoHead != alphaHead {
		t.Fatalf("bravo's head after the receive = %s, want alpha's %s", bravoHead, alphaHead)
	}
	if show := runCLIInDir(t, r.bravo, "show", r.ticket); !strings.Contains(show, "alpha-description") {
		t.Fatalf("bravo's ticket after the receive:\n%s\nwant alpha's description", show)
	}
}
