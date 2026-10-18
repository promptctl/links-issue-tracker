package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// reRootedCheckouts is two checkouts over one git remote after alpha re-rooted
// the shared history at cut, the head bravo adopted, and force-pushed it.
// bravo has edited the ticket's title without pushing; when concurrent, alpha
// edited its description and pushed before the re-root.
type reRootedCheckouts struct {
	alpha, bravo string
	epic, ticket string
	cut          string
	root         string
	sink         string
}

func newReRootedCheckouts(t *testing.T, concurrent bool) reRootedCheckouts {
	t.Helper()
	r, base := newSharedCheckouts(t)
	r.cut, _ = syncHeadAndBranch(t, r.bravo)
	runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
	if concurrent {
		runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
		runCLIInDir(t, r.alpha, "sync", "push")
	}
	r.root = reRootAlphaAndForcePush(t, r, r.cut)
	finishSharedCheckouts(t, r, base)
	return r
}

// newSharedCheckouts is the start every re-rooted pair shares: alpha seeded
// an epic holding the ticket and a filler over one git remote and pushed it,
// and bravo adopted it. It returns the directory both live under.
func newSharedCheckouts(t *testing.T) (reRootedCheckouts, string) {
	t.Helper()
	t.Setenv(DisableAutoSyncEnvVar, "1")
	maskGlobalConfig(t)
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")
	r := reRootedCheckouts{alpha: filepath.Join(base, "alpha"), bravo: filepath.Join(base, "bravo"), sink: filepath.Join(base, "notifications")}

	runGit(t, base, "clone", remote, "alpha")
	runGit(t, r.alpha, "config", "user.email", "a@a.co")
	runGit(t, r.alpha, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(r.alpha, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, r.alpha, "add", "-A")
	runGit(t, r.alpha, "commit", "-m", "seed")
	runGit(t, r.alpha, "push", "origin", "HEAD")
	runCLIInDir(t, r.alpha, "init", "--skip-hooks", "--skip-agents")
	r.epic = newEpicInDir(t, r.alpha, "shared-epic")
	r.ticket = extractTicketID(t, runCLIInDir(t, r.alpha, "new", "--title", "shared-ticket", "--topic", "demo", "--type", "task", "--parent", r.epic))
	runCLIInDir(t, r.alpha, "new", "--title", "filler", "--topic", "demo", "--type", "task", "--parent", r.epic)
	runCLIInDir(t, r.alpha, "sync", "push", "--set-upstream")

	runGit(t, base, "clone", remote, "bravo")
	runGit(t, r.bravo, "config", "user.email", "b@b.co")
	runGit(t, r.bravo, "config", "user.name", "bravo")
	runCLIInDir(t, r.bravo, "init", "--skip-hooks", "--skip-agents")
	return r, base
}

// reRootAlphaAndForcePush cuts alpha's history at cut, force-pushes it, and
// returns the re-root's parentless root.
func reRootAlphaAndForcePush(t *testing.T, r reRootedCheckouts, cut string) string {
	t.Helper()
	alphaWS, err := workspace.Resolve(r.alpha)
	if err != nil {
		t.Fatalf("resolve alpha: %v", err)
	}
	_, branch := syncHeadAndBranch(t, r.alpha)
	reRooted, err := store.ReRoot(context.Background(), alphaWS.DatabasePath, branch, cut)
	if err != nil {
		t.Fatalf("ReRoot(alpha) error = %v", err)
	}
	runCLIInDir(t, r.alpha, "sync", "push", "--force")
	return reRooted.Root
}

// finishSharedCheckouts sends each checkout's owner notifications to its own
// sink, turns the automatic sync back on, and isolates the events the test
// reads.
func finishSharedCheckouts(t *testing.T, r reRootedCheckouts, base string) {
	t.Helper()
	writeOwnerNotifyProjectConfig(t, r.bravo, r.sink)
	// alpha gets the same config so its own commands spawn no background
	// receive; its notifications go elsewhere.
	writeOwnerNotifyProjectConfig(t, r.alpha, filepath.Join(base, "alpha-notifications"))
	t.Setenv(DisableAutoSyncEnvVar, "0")
	isolateEvents(t)
}

// syncHeadAndBranch reads the workspace's head commit and active branch.
func syncHeadAndBranch(t *testing.T, dir string) (head, branch string) {
	t.Helper()
	ws, err := workspace.Resolve(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	session, closeStore, err := openSyncSession(underInvocation(context.Background()), ws)
	if err != nil {
		t.Fatalf("open sync session at %s: %v", dir, err)
	}
	defer func() { _ = closeStore() }()
	status, err := session.syncer.SyncStatus(context.Background())
	if err != nil {
		t.Fatalf("SyncStatus(%s): %v", dir, err)
	}
	return status.HeadCommit, status.Branch
}

// assertFollowedCleanly checks what an entry point left behind once it
// followed: its event carries every follow it made, in order, bravo holds
// both edits, nothing surfaced the remote as unrelated or notified the owner,
// and lit doctor exits clean.
func (r reRootedCheckouts) assertFollowedCleanly(t *testing.T, entries []string, wantDescription bool) {
	t.Helper()
	ev := lastEvent(t)
	follows, _ := ev.Facts["reroot.follows"].([]any)
	if len(follows) != len(entries) {
		t.Fatalf("event %q reroot.follows = %v; want one follow for each of %v", ev.Event, ev.Facts["reroot.follows"], entries)
	}
	for i, entry := range entries {
		follow, _ := follows[i].(map[string]any)
		if kept, _ := follow["reroot.kept"].(float64); follow["reroot.follow"] != entry || follow["reroot.cut"] != r.cut || follow["reroot.root"] != r.root || kept < 2 {
			t.Fatalf("event %q follow %d = %v; want follow=%s at %s to %s keeping the cut and bravo's edit", ev.Event, i, follow, entry, r.cut, r.root)
		}
	}
	show := runCLIInDir(t, r.bravo, "show", r.ticket)
	if !strings.Contains(show, "bravo-title") || strings.Contains(show, "alpha-description") != wantDescription {
		t.Fatalf("bravo's ticket after the follow (alpha's description expected: %v):\n%s", wantDescription, show)
	}
	ws, err := workspace.Resolve(r.bravo)
	if err != nil {
		t.Fatalf("resolve bravo: %v", err)
	}
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("a receive block is pending after the follow (stat err=%v)", err)
	}
	if payload, err := os.ReadFile(r.sink); !os.IsNotExist(err) {
		t.Fatalf("the owner was notified after the follow: %q (err=%v)", payload, err)
	}
	if doctor := runCLIInDir(t, r.bravo, "doctor"); strings.Contains(doctor, "unrelated") || strings.Contains(doctor, "diverged") {
		t.Fatalf("lit doctor after the follow:\n%s", doctor)
	}
}

// TestEverySyncEntryPointFollowsAReRootedRemote drives each sync entry point
// in bravo against the remote alpha re-rooted. Each follows the re-root and
// leaves bravo holding both checkouts' edits — or, for a push with no write
// on the remote since bravo forked, lands bravo's — with the follow on the
// command's event and nothing reporting unrelated histories.
func TestEverySyncEntryPointFollowsAReRootedRemote(t *testing.T) {
	t.Run("pull", func(t *testing.T) {
		r := newReRootedCheckouts(t, true)
		runCLIInDir(t, r.bravo, "sync", "pull")
		r.assertFollowedCleanly(t, []string{"pull"}, true)
	})
	t.Run("receive", func(t *testing.T) {
		r := newReRootedCheckouts(t, true)
		runCLIInDir(t, r.bravo, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
		r.assertFollowedCleanly(t, []string{"receive"}, true)
	})
	t.Run("reconcile", func(t *testing.T) {
		r := newReRootedCheckouts(t, true)
		runCLIInDir(t, r.bravo, "sync", "reconcile")
		r.assertFollowedCleanly(t, []string{"reconcile"}, true)
	})
	t.Run("fetch", func(t *testing.T) {
		// A fetch settles nothing. With no write on the remote since bravo
		// forked, the follow leaves bravo only ahead; what it must not leave
		// is a freshness that reports the re-root as a divergence, which lit
		// doctor refuses on.
		r := newReRootedCheckouts(t, false)
		runCLIInDir(t, r.bravo, "sync", "fetch")
		r.assertFollowedCleanly(t, []string{"fetch"}, false)
	})
	t.Run("push", func(t *testing.T) {
		r := newReRootedCheckouts(t, false)
		runCLIInDir(t, r.bravo, "sync", "push")
		r.assertFollowedCleanly(t, []string{"push"}, false)
		runCLIInDir(t, r.alpha, "sync", "pull")
		if show := runCLIInDir(t, r.alpha, "show", r.ticket); !strings.Contains(show, "bravo-title") {
			t.Fatalf("alpha missing bravo's pushed edit:\n%s", show)
		}
	})
	t.Run("on-change mirror", func(t *testing.T) {
		r := newReRootedCheckouts(t, false)
		runCLIInDir(t, r.bravo, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
		// The clone follows and pushes, and the live store receives that
		// push by following the same re-root: the event carries both.
		r.assertFollowedCleanly(t, []string{"push", "receive"}, false)
		if bravoHead, _ := syncHeadAndBranch(t, r.bravo); bravoHead == r.cut {
			t.Fatalf("bravo's head is still the cut %s", r.cut)
		}
		runCLIInDir(t, r.alpha, "sync", "pull")
		alphaHead, _ := syncHeadAndBranch(t, r.alpha)
		if bravoHead, _ := syncHeadAndBranch(t, r.bravo); alphaHead != bravoHead {
			t.Fatalf("after the mirror's push alpha is at %s and bravo at %s", alphaHead, bravoHead)
		}
	})
}

// `lit sync fetch` follows only for the freshness reads, which never ask the
// network for the sync branch. With no default branch known on this machine
// and none the remote can name, the fetch lands, reports fetched, and follows
// nothing, as no read has a freshness to misreport.
func TestSyncFetchWithNoKnownSyncBranchFetchesAndFollowsNothing(t *testing.T) {
	t.Setenv(DisableAutoSyncEnvVar, "1")
	maskGlobalConfig(t)
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")
	runGit(t, base, "clone", remote, "alpha")
	alpha := filepath.Join(base, "alpha")
	runGit(t, alpha, "config", "user.email", "a@a.co")
	runGit(t, alpha, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(alpha, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, alpha, "add", "-A")
	runGit(t, alpha, "commit", "-m", "seed")
	runGit(t, alpha, "push", "origin", "HEAD")
	runCLIInDir(t, alpha, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/no-such-branch")
	runGit(t, alpha, "remote", "set-head", "origin", "-d")
	ws, err := workspace.Resolve(alpha)
	if err != nil {
		t.Fatalf("resolve alpha: %v", err)
	}
	if err := os.Remove(advertisedSyncBranchPath(ws, "origin")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove the sync branch record: %v", err)
	}

	isolateEvents(t)
	if out := runCLIInDir(t, alpha, "sync", "fetch"); strings.TrimSpace(out) != "fetched" {
		t.Fatalf("lit sync fetch printed %q, want fetched", out)
	}
	if follows, ok := lastEvent(t).Facts["reroot.follows"].([]any); !ok || len(follows) != 0 {
		t.Fatalf("reroot.follows = %v; want an empty list", lastEvent(t).Facts["reroot.follows"])
	}
}

// TestSyncPushPutsTheHistoryBoundOnItsEvent pushes from alpha under the
// default bound. The push's event carries the one bound it kept: the remote
// head it left and that head's height, short of the height that re-roots,
// with nothing published.
func TestSyncPushPutsTheHistoryBoundOnItsEvent(t *testing.T) {
	r, _ := newSharedCheckouts(t)
	runCLIInDir(t, r.alpha, "new", "--title", "pushed", "--topic", "demo", "--type", "task", "--parent", r.epic)
	isolateEvents(t)
	runCLIInDir(t, r.alpha, "sync", "push")
	head, _ := syncHeadAndBranch(t, r.alpha)
	ev := readEvents(t)[0]
	records, _ := ev.Facts["history_bound.records"].([]any)
	if len(records) != 1 {
		t.Fatalf("event %q history_bound.records = %v; want the push's one bound", ev.Event, ev.Facts["history_bound.records"])
	}
	record, _ := records[0].(map[string]any)
	height, _ := record["history_bound.height"].(float64)
	if record["history_bound.head"] != head || height < 2 || record["history_bound.fired"] != false || record["history_bound.reroot_at"] != float64(2000) || record["history_bound.keep"] != float64(1000) {
		t.Fatalf("event %q bound = %v; want alpha's head %s at its height, short of 2000, keeping 1000", ev.Event, record, head)
	}
	if record["history_bound.lease"] != "" || record["history_bound.error"] != nil || record["reroot.head"] != "" {
		t.Fatalf("event %q bound = %v; want nothing published or failed", ev.Event, record)
	}
	if follows, ok := ev.Facts["reroot.follows"].([]any); !ok || len(follows) != 0 {
		t.Fatalf("event %q reroot.follows = %v; want an empty list: nothing followed, so nothing collected", ev.Event, ev.Facts["reroot.follows"])
	}
}
