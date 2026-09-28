package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// landFixture is a live store, a file remote it has pushed to, and helpers to
// move the remote from elsewhere and to fetch into a clone of the live store.
type landFixture struct {
	t         *testing.T
	ctx       context.Context
	base      string
	live      string
	remoteURL string
	clones    int
}

func newLandFixture(t *testing.T) *landFixture {
	t.Helper()
	f := &landFixture{t: t, ctx: context.Background(), base: t.TempDir(), live: migratedDoltDir(t)}
	f.remoteURL = "file://" + filepath.Join(f.base, "remote")
	f.commit(f.live, "c1")
	st := f.openSync(f.live)
	if err := st.SyncAddRemote(f.ctx, "origin", f.remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := st.SyncPush(f.ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("seed push error = %v", err)
	}
	f.close(st)
	return f
}

func (f *landFixture) openSync(root string) *Store {
	f.t.Helper()
	st, err := OpenSync(f.ctx, root, "ws")
	if err != nil {
		f.t.Fatalf("OpenSync(%s) error = %v", root, err)
	}
	return st
}

func (f *landFixture) close(st *Store) {
	f.t.Helper()
	if err := st.Close(); err != nil {
		f.t.Fatalf("Close() error = %v", err)
	}
}

// commit lands one issue on the store at root and returns the new head.
func (f *landFixture) commit(root, title string) string {
	f.t.Helper()
	st, err := Open(f.ctx, root, "ws")
	if err != nil {
		f.t.Fatalf("Open(%q) error = %v", title, err)
	}
	defer f.close(st)
	if _, err := st.CreateIssue(f.ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0}); err != nil {
		f.t.Fatalf("CreateIssue(%q) error = %v", title, err)
	}
	var head string
	if err := st.db.QueryRowContext(f.ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1`).Scan(&head); err != nil {
		f.t.Fatalf("read head after %q: %v", title, err)
	}
	return head
}

// copyOf clones the Dolt root at root to a fresh path.
func (f *landFixture) copyOf(root string) string {
	f.t.Helper()
	f.clones++
	dst := filepath.Join(f.base, "copies", strings.Repeat("c", f.clones), "dolt")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		f.t.Fatalf("mkdir copy parent: %v", err)
	}
	if err := dbsnapshot.CloneTree(f.ctx, root, dst); err != nil {
		f.t.Fatalf("CloneTree() error = %v", err)
	}
	return dst
}

// push pushes the store at root to the remote; force rewrites it.
func (f *landFixture) push(root string, force bool) {
	f.t.Helper()
	st := f.openSync(root)
	defer f.close(st)
	if _, err := st.SyncPush(f.ctx, "origin", "master", false, force); err != nil {
		f.t.Fatalf("SyncPush(force=%t) error = %v", force, err)
	}
}

// fetchedClone is the receive's shape: a clone of the live store that fetched.
func (f *landFixture) fetchedClone() string {
	f.t.Helper()
	clone := f.copyOf(f.live)
	st := f.openSync(clone)
	defer f.close(st)
	if err := st.SyncFetch(f.ctx, "origin", false); err != nil {
		f.t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	return clone
}

func (f *landFixture) trackingRef() string {
	f.t.Helper()
	st := f.openSync(f.live)
	defer f.close(st)
	var h string
	if err := st.db.QueryRowContext(f.ctx, `SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/master'`).Scan(&h); err != nil {
		f.t.Fatalf("read tracking ref: %v", err)
	}
	return h
}

// TestLandFetchedHeadCarriesAClonesFetchToTheLiveStore drives the receive's
// shape at the store level: a peer moves the remote, a clone of the live
// store fetches it, and the live store — which never contacted the network —
// ends up exactly as its own fetch would have left it: the tracking ref at the
// peer's head, the chunks behind it readable, and a settle that fast-forwards
// to it.
func TestLandFetchedHeadCarriesAClonesFetchToTheLiveStore(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	peer := f.copyOf(f.live)
	peerHead := f.commit(peer, "peer-ticket")
	f.push(peer, false)

	clone := f.fetchedClone()
	record, err := LandFetchedHead(f.ctx, f.live, clone, "origin", "master")
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if record != FetchedHeadMoved {
		t.Fatalf("LandFetchedHead() = %s, want moved", record)
	}
	if got := f.trackingRef(); got != peerHead {
		t.Fatalf("tracking ref after landing = %s, want the peer's head %s", got, peerHead)
	}
	if err := os.RemoveAll(filepath.Dir(clone)); err != nil {
		t.Fatalf("remove clone: %v", err)
	}

	st := f.openSync(f.live)
	defer f.close(st)
	result, err := st.SyncSettleReceived(f.ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncSettleReceived() error = %v", err)
	}
	if result.State != storage.SyncReceiveFastForwarded || result.Behind != 1 {
		t.Fatalf("SyncSettleReceived() = %+v, want fast-forwarded from behind by 1", result)
	}
	var titles int
	if err := st.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM issues WHERE title = 'peer-ticket'`).Scan(&titles); err != nil {
		t.Fatalf("read the peer's issue after the settle: %v", err)
	}
	if titles != 1 {
		t.Fatalf("the live store holds %d peer-ticket row(s) after the settle, want 1", titles)
	}
}

// TestLandFetchedHeadNeverMovesTheRefBackwards pins the carried arm: the live
// ref already names a descendant of what the clone fetched (here the live
// store fetched a later push itself), so landing leaves it alone. Its pair is
// the rewritten remote below, which does move the ref off a commit it does
// not descend from.
func TestLandFetchedHeadNeverMovesTheRefBackwards(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	stale := f.fetchedClone()
	peer := f.copyOf(f.live)
	peerHead := f.commit(peer, "peer-ticket")
	f.push(peer, false)
	st := f.openSync(f.live)
	if err := st.SyncFetch(f.ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(live) error = %v", err)
	}
	f.close(st)

	record, err := LandFetchedHead(f.ctx, f.live, stale, "origin", "master")
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if record != FetchedHeadCarried {
		t.Fatalf("LandFetchedHead() = %s, want carried: the live ref is past what the clone fetched", record)
	}
	if got := f.trackingRef(); got != peerHead {
		t.Fatalf("landing a stale fetch moved the tracking ref to %s; want it left at %s", got, peerHead)
	}
}

// TestLandFetchedHeadFollowsARewrittenRemote is the carried arm's pair: a
// remote force-pushed to a history that does not contain the live ref is
// recorded as the fetch found it, as Dolt's own fetch records it.
func TestLandFetchedHeadFollowsARewrittenRemote(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	rewriter := f.copyOf(f.live)
	peer := f.copyOf(f.live)
	f.commit(peer, "peer-ticket")
	f.push(peer, false)
	st := f.openSync(f.live)
	if err := st.SyncFetch(f.ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(live) error = %v", err)
	}
	f.close(st)
	rewrittenHead := f.commit(rewriter, "rewritten-ticket")
	f.push(rewriter, true)

	clone := f.fetchedClone()
	record, err := LandFetchedHead(f.ctx, f.live, clone, "origin", "master")
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if record != FetchedHeadMoved {
		t.Fatalf("LandFetchedHead() = %s, want moved onto the rewritten history", record)
	}
	if got := f.trackingRef(); got != rewrittenHead {
		t.Fatalf("tracking ref after landing a rewritten remote = %s, want %s", got, rewrittenHead)
	}
}

// TestLandFetchedHeadLeavesTheLiveStoreAloneForAnAbsentBranch pins the absent
// arm: the clone fetched and the remote has no such branch, so there is
// nothing to land and nothing is written.
func TestLandFetchedHeadLeavesTheLiveStoreAloneForAnAbsentBranch(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	before := f.trackingRef()
	clone := f.fetchedClone()
	record, err := LandFetchedHead(f.ctx, f.live, clone, "origin", "no-such-branch")
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if record != FetchedHeadAbsent {
		t.Fatalf("LandFetchedHead() = %s, want absent", record)
	}
	if got := f.trackingRef(); got != before {
		t.Fatalf("landing an absent branch moved remotes/origin/master from %s to %s", before, got)
	}
}

// TestLandFetchedHeadCutsAtTheHoldBudget pins that the landing's hold on the
// live store runs under MirrorHoldBudget and a cut is stamped with
// ErrMirrorHoldCut.
func TestLandFetchedHeadCutsAtTheHoldBudget(t *testing.T) {
	f := newLandFixture(t)
	peer := f.copyOf(f.live)
	f.commit(peer, "peer-ticket")
	f.push(peer, false)
	clone := f.fetchedClone()
	before := f.trackingRef()

	saved := MirrorHoldBudget
	MirrorHoldBudget = time.Nanosecond
	t.Cleanup(func() { MirrorHoldBudget = saved })
	_, err := LandFetchedHead(f.ctx, f.live, clone, "origin", "master")
	if !errors.Is(err, ErrMirrorHoldCut) {
		t.Fatalf("LandFetchedHead() under a zero budget error = %v, want ErrMirrorHoldCut", err)
	}
	MirrorHoldBudget = saved
	if got := f.trackingRef(); got != before {
		t.Fatalf("a cut landing moved the tracking ref from %s to %s", before, got)
	}
}

// TestLandRemoteCacheCopiesOnlyWhatTheCloneAdded drives the git half with
// plain git: the clone's mirror gained a ref and the commit behind it, and a
// ref both mirrors hold differs. The live mirror gains the new ref and its
// objects and keeps its own value of the shared ref; a mirror only the clone
// has is moved over whole.
func TestLandRemoteCacheCopiesOnlyWhatTheCloneAdded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	liveBase := filepath.Join(base, "live", remoteCacheDirName)
	cloneBase := filepath.Join(base, "clone", remoteCacheDirName)
	key := strings.Repeat("a", 64)
	onlyCloneKey := strings.Repeat("b", 64)
	git := func(gitDir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"--git-dir", gitDir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commitOn := func(gitDir, refName, message string) string {
		t.Helper()
		tree := git(gitDir, "mktree")
		cmd := exec.Command("git", "--git-dir", gitDir, "commit-tree", tree, "-m", message)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("commit-tree: %v\n%s", err, out)
		}
		oid := strings.TrimSpace(string(out))
		git(gitDir, "update-ref", refName, oid)
		return oid
	}
	liveRepo := filepath.Join(liveBase, key, "repo.git")
	cloneRepo := filepath.Join(cloneBase, key, "repo.git")
	for _, dir := range []string{liveRepo, cloneRepo, filepath.Join(cloneBase, onlyCloneKey, "repo.git")} {
		if out, err := exec.Command("git", "init", "--bare", "--quiet", dir).CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", dir, err, out)
		}
	}
	const shared = "refs/dolt/remotes/origin/dolt/data/shared"
	const added = "refs/dolt/remotes/origin/dolt/data/added"
	liveShared := commitOn(liveRepo, shared, "live's value")
	commitOn(cloneRepo, shared, "clone's value")
	addedOID := commitOn(cloneRepo, added, "what the clone fetched")

	if err := landRemoteCache(ctx, liveBase, cloneBase); err != nil {
		t.Fatalf("landRemoteCache() error = %v", err)
	}
	if got := git(liveRepo, "rev-parse", added); got != addedOID {
		t.Fatalf("live mirror's %s = %s, want the clone's %s", added, got, addedOID)
	}
	if got := git(liveRepo, "cat-file", "-t", addedOID); got != "commit" {
		t.Fatalf("live mirror holds %s as %q, want its commit", addedOID, got)
	}
	if got := git(liveRepo, "rev-parse", shared); got != liveShared {
		t.Fatalf("landing overwrote the live mirror's %s with %s; want its own %s kept", shared, got, liveShared)
	}
	if _, err := os.Stat(filepath.Join(liveBase, onlyCloneKey, "repo.git")); err != nil {
		t.Fatalf("the mirror only the clone had was not moved into the live store: %v", err)
	}
}
