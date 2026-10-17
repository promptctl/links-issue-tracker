package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// reRootedRemote is two checkouts sharing a remote after one of them re-rooted
// it. bravo adopted the remote, then edited the ticket's title without
// pushing; alpha, when concurrent, edited the same ticket's description and
// pushed it, then cut its history at cut — a commit bravo holds — and
// force-pushed the re-rooted history.
type reRootedRemote struct {
	alpha, bravo string
	remoteURL    string
	ticket       string
	cut          string
	// alphaReRoot is alpha's own re-root, so its Root is the remote's root.
	alphaReRoot storage.ReRooted
	// bravoKept is how many commits bravo's branch reaches from its head
	// down to cut, read from dolt_log before anything followed.
	bravoKept int
}

const (
	bravoTitle       = "bravo-title"
	alphaDescription = "alpha-description"
)

func newReRootedRemote(t *testing.T, ctx context.Context, concurrent bool) reRootedRemote {
	t.Helper()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	r := reRootedRemote{alpha: migratedDoltDir(t), bravo: migratedDoltDir(t), remoteURL: remoteURL}
	epic := seedReconcileRemote(t, ctx, r.alpha, remoteURL)
	st, err := Open(ctx, r.alpha, "ws")
	if err != nil {
		t.Fatalf("Open(alpha) error = %v", err)
	}
	for i := range 4 {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("t%d", i), Topic: "topic", IssueType: "task", ParentID: epic})
		if err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		r.ticket = issue.ID
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(alpha) error = %v", err)
	}
	sync := openSyncOrFatal(t, ctx, r.alpha)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(alpha) error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(alpha sync) error = %v", err)
	}
	adoptRemote(t, ctx, r.bravo, remoteURL)

	// The cut is two commits below the head bravo adopted, so both sides
	// keep commits above it and drop commits below it.
	withChunkStore(t, r.bravo, func(ddb *doltdb.DoltDB) {
		r.cut = firstParentBelowHead(t, ctx, ddb, 2).String()
	})
	updateLocal(t, ctx, r.bravo, r.ticket, storage.UpdateIssueInput{Title: strptr(bravoTitle)})
	r.bravoKept = logLength(t, ctx, r.bravo, "HEAD") - logLength(t, ctx, r.bravo, r.cut) + 1
	if concurrent {
		updateAndPush(t, ctx, r.alpha, r.ticket, storage.UpdateIssueInput{Description: strptr(alphaDescription)})
	}

	r.alphaReRoot, err = ReRoot(ctx, r.alpha, "master", r.cut)
	if err != nil {
		t.Fatalf("ReRoot(alpha) error = %v", err)
	}
	sync = openSyncOrFatal(t, ctx, r.alpha)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, true); err != nil {
		t.Fatalf("force push of alpha's re-rooted history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(alpha force push) error = %v", err)
	}
	return r
}

// assertFollowed checks the one follow st applied: under entry, at the cut,
// arriving at the remote's own root, keeping what dolt_log said it would.
func (r reRootedRemote) assertFollowed(t *testing.T, st *Store, entry storage.SyncEntry) {
	t.Helper()
	followed := st.FollowedReRoots()
	if len(followed) != 1 {
		t.Fatalf("FollowedReRoots() = %+v, want exactly one follow", followed)
	}
	f := followed[0]
	if f.Entry != entry || f.Cut != r.cut || f.Root != r.alphaReRoot.Root {
		t.Fatalf("followed under %q at %s to root %s; want %q at %s to the remote's root %s", f.Entry, f.Cut, f.Root, entry, r.cut, r.alphaReRoot.Root)
	}
	if f.Kept() != r.bravoKept {
		t.Fatalf("the follow kept %d commits; dolt_log counted %d from bravo's head down to the cut", f.Kept(), r.bravoKept)
	}
}

// assertMerged checks the ticket on st carries both checkouts' edits and that
// st's history is rooted at the remote's root, which it reached by following
// rather than by any merge with no common ancestor.
func (r reRootedRemote) assertMerged(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	issue := getIssueOrFatal(t, ctx, st, r.ticket)
	if issue.Title != bravoTitle || issue.Description != alphaDescription {
		t.Fatalf("ticket title, description = %q, %q; want bravo's %q and alpha's %q", issue.Title, issue.Description, bravoTitle, alphaDescription)
	}
	var root string
	if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() ORDER BY commit_order LIMIT 1`).Scan(&root); err != nil {
		t.Fatalf("read the history's root: %v", err)
	}
	if root != r.alphaReRoot.Root {
		t.Fatalf("history is rooted at %s, want the remote's root %s", root, r.alphaReRoot.Root)
	}
}

// TestSyncPullFollowsAReRootedRemote drives `lit sync pull` against a remote
// another checkout re-rooted, with a concurrent edit to one ticket on each
// side. The pull follows the re-root and merges both edits, and the two
// checkouts then sync both ways onto one head.
func TestSyncPullFollowsAReRootedRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	pulled, err := bravo.SyncPull(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncPull(bravo) error = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized || pulled.Unrelated != nil {
		t.Fatalf("pull state = %q (unrelated %+v), want %q", pulled.State, pulled.Unrelated, storage.SyncPullLinearized)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryPull)
	r.assertMerged(t, ctx, bravo)
	if _, err := bravo.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("bravo's push after the follow error = %v", err)
	}

	alpha := openSyncOrFatal(t, ctx, r.alpha)
	defer func() { _ = alpha.Close() }()
	back, err := alpha.SyncPull(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncPull(alpha) error = %v", err)
	}
	if back.State != storage.SyncPullFastForwarded || len(alpha.FollowedReRoots()) != 0 {
		t.Fatalf("alpha's pull = %q with follows %+v, want a fast-forward and no follow", back.State, alpha.FollowedReRoots())
	}
	r.assertMerged(t, ctx, alpha)
	if a, b := headCommit(t, ctx, alpha), headCommit(t, ctx, bravo); a != b {
		t.Fatalf("heads after syncing both ways: alpha %s, bravo %s", a, b)
	}
}

// TestSyncReceiveFollowsAReRootedRemote drives the receive: it follows the
// re-root and reports the concurrent edits as an ordinary divergence, which
// the reconcile the receive hands it to merges.
func TestSyncReceiveFollowsAReRootedRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	received, err := bravo.SyncReceive(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReceive(bravo) error = %v", err)
	}
	if received.State != storage.SyncReceiveDiverged {
		t.Fatalf("receive state = %q, want %q", received.State, storage.SyncReceiveDiverged)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryReceive)
	reconciled, err := bravo.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(bravo) error = %v", err)
	}
	if reconciled.State != storage.SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q, want %q", reconciled.State, storage.SyncReconcileLinearized)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryReceive)
	r.assertMerged(t, ctx, bravo)
}

// TestSyncReconcileFollowsAReRootedRemote drives `lit sync reconcile` over a
// tracking ref a plain fetch moved onto the re-rooted history.
func TestSyncReconcileFollowsAReRootedRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	if err := bravo.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(bravo) error = %v", err)
	}
	reconciled, err := bravo.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(bravo) error = %v", err)
	}
	if reconciled.State != storage.SyncReconcileLinearized || reconciled.Unrelated != nil {
		t.Fatalf("reconcile state = %q (unrelated %+v), want %q", reconciled.State, reconciled.Unrelated, storage.SyncReconcileLinearized)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryReconcile)
	r.assertMerged(t, ctx, bravo)
}

// TestSyncPushFollowsAReRootedRemote drives a push the re-rooted remote
// refuses. With no write on the remote since bravo's fork, the push follows
// and lands, and alpha fast-forwards to bravo's edit. With alpha's concurrent
// edit there, the push follows and is then refused as an ordinary divergence
// — never as histories with no common ancestor — and the pull after it merges
// both edits.
func TestSyncPushFollowsAReRootedRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("lands", func(t *testing.T) {
		t.Parallel()
		r := newReRootedRemote(t, ctx, false)
		bravo := openSyncOrFatal(t, ctx, r.bravo)
		defer func() { _ = bravo.Close() }()
		pushed, err := bravo.SyncPush(ctx, "origin", "master", false, false)
		if err != nil {
			t.Fatalf("SyncPush(bravo) error = %v", err)
		}
		r.assertFollowed(t, bravo, storage.SyncEntryPush)
		if head := headCommit(t, ctx, bravo); pushed.Head != head {
			t.Fatalf("pushed head %s, want bravo's re-rooted head %s", pushed.Head, head)
		}
		alpha := openSyncOrFatal(t, ctx, r.alpha)
		defer func() { _ = alpha.Close() }()
		back, err := alpha.SyncPull(ctx, "origin", "master")
		if err != nil {
			t.Fatalf("SyncPull(alpha) error = %v", err)
		}
		if back.State != storage.SyncPullFastForwarded || headCommit(t, ctx, alpha) != pushed.Head {
			t.Fatalf("alpha's pull = %q at %s, want a fast-forward to %s", back.State, headCommit(t, ctx, alpha), pushed.Head)
		}
		if got := getIssueOrFatal(t, ctx, alpha, r.ticket).Title; got != bravoTitle {
			t.Fatalf("alpha's ticket title = %q, want bravo's %q", got, bravoTitle)
		}
	})

	t.Run("refused as divergence", func(t *testing.T) {
		t.Parallel()
		r := newReRootedRemote(t, ctx, true)
		bravo := openSyncOrFatal(t, ctx, r.bravo)
		defer func() { _ = bravo.Close() }()
		_, err := bravo.SyncPush(ctx, "origin", "master", false, false)
		if err == nil {
			t.Fatalf("SyncPush(bravo) landed over alpha's concurrent edit")
		}
		if strings.Contains(err.Error(), noCommonAncestorMsg) {
			t.Fatalf("the push was refused for histories with no common ancestor after following: %v", err)
		}
		r.assertFollowed(t, bravo, storage.SyncEntryPush)
		pulled, err := bravo.SyncPull(ctx, "origin", "master")
		if err != nil {
			t.Fatalf("SyncPull(bravo) error = %v", err)
		}
		if pulled.State != storage.SyncPullLinearized {
			t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullLinearized)
		}
		r.assertMerged(t, ctx, bravo)
	})
}

// TestSyncPushFromCloneFollowsAReRootedRemote drives the on-change mirror's
// push, from a clone of bravo: it follows on the clone and lands.
func TestSyncPushFromCloneFollowsAReRootedRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	clone := filepath.Join(t.TempDir(), "dolt")
	if err := dbsnapshot.CloneTree(ctx, r.bravo, clone); err != nil {
		t.Fatalf("CloneTree(bravo) error = %v", err)
	}
	r.bravo = clone
	st := openSyncOrFatal(t, ctx, clone)
	defer func() { _ = st.Close() }()
	pushed, err := st.SyncPushFromClone(ctx, "origin", "master", false, false)
	if err != nil {
		t.Fatalf("SyncPushFromClone error = %v", err)
	}
	if pushed.Superseded != "" || pushed.Head != headCommit(t, ctx, st) {
		t.Fatalf("push = %+v, want bravo's re-rooted head %s landed", pushed, headCommit(t, ctx, st))
	}
	r.assertFollowed(t, st, storage.SyncEntryPush)
}

// TestSyncFollowsARemoteReRootedAboveItsOwnCut covers two checkouts that
// re-rooted at different cuts. bravo has already re-rooted its own history at
// a lower cut; alpha re-roots higher. A re-root keeps each commit's root value
// and metadata, so bravo's rewrite of alpha's cut still rebuilds to alpha's
// root, and bravo follows.
func TestSyncFollowsARemoteReRootedAboveItsOwnCut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	st, err := Open(ctx, alpha, "ws")
	if err != nil {
		t.Fatalf("Open(alpha) error = %v", err)
	}
	var ticket string
	for i := range 5 {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("t%d", i), Topic: "topic", IssueType: "task", ParentID: epic})
		if err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		ticket = issue.ID
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	sync := openSyncOrFatal(t, ctx, alpha)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(alpha) error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatal(err)
	}
	adoptRemote(t, ctx, bravo, remoteURL)

	var lowCut, highCut hash.Hash
	withChunkStore(t, bravo, func(ddb *doltdb.DoltDB) {
		lowCut = firstParentBelowHead(t, ctx, ddb, 4)
		highCut = firstParentBelowHead(t, ctx, ddb, 1)
	})
	if _, err := ReRoot(ctx, bravo, "master", lowCut.String()); err != nil {
		t.Fatalf("ReRoot(bravo, low cut) error = %v", err)
	}
	updateLocal(t, ctx, bravo, ticket, storage.UpdateIssueInput{Title: strptr(bravoTitle)})
	updateAndPush(t, ctx, alpha, ticket, storage.UpdateIssueInput{Description: strptr(alphaDescription)})
	alphaReRoot, err := ReRoot(ctx, alpha, "master", highCut.String())
	if err != nil {
		t.Fatalf("ReRoot(alpha, high cut) error = %v", err)
	}
	sync = openSyncOrFatal(t, ctx, alpha)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, true); err != nil {
		t.Fatalf("force push of alpha's re-rooted history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatal(err)
	}

	st = openSyncOrFatal(t, ctx, bravo)
	defer func() { _ = st.Close() }()
	pulled, err := st.SyncPull(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncPull(bravo) error = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized {
		t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullLinearized)
	}
	followed := st.FollowedReRoots()
	if len(followed) != 1 || followed[0].Root != alphaReRoot.Root || followed[0].Cut == highCut.String() {
		t.Fatalf("FollowedReRoots() = %+v; want one follow to alpha's root %s at bravo's rewrite of %s, not the commit itself", followed, alphaReRoot.Root, highCut)
	}
	r := reRootedRemote{ticket: ticket, alphaReRoot: alphaReRoot}
	r.assertMerged(t, ctx, st)
}

// TestSyncDoesNotFollowARemoteRootMatchedByMetadataAlone gives an unrelated
// checkout a commit carrying exactly the metadata of the remote's root over
// different tables. The commit resembles the root and does not rebuild to it,
// so nothing is followed and the remote stays unrelated histories.
func TestSyncDoesNotFollowARemoteRootMatchedByMetadataAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	stranger := unrelatedDoltDir(t)
	forkUnrelatedClone(t, ctx, stranger, r.remoteURL)

	var rootMeta *datas.CommitMeta
	withChunkStore(t, r.alpha, func(ddb *doltdb.DoltDB) {
		var err error
		rootMeta, err = readCommit(t, ctx, ddb, hash.Parse(r.alphaReRoot.Root)).GetCommitMeta(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})
	withChunkStore(t, stranger, func(ddb *doltdb.DoltDB) {
		branch := ref.NewBranchRef("master")
		head, err := ddb.ResolveCommitRef(ctx, branch)
		if err != nil {
			t.Fatal(err)
		}
		value, _, err := commitContent(ctx, head)
		if err != nil {
			t.Fatal(err)
		}
		forged, err := ddb.CommitDanglingWithParentCommits(ctx, value, []*doltdb.Commit{head}, rootMeta)
		if err != nil {
			t.Fatal(err)
		}
		if err := ddb.SetHeadToCommit(ctx, branch, forged); err != nil {
			t.Fatal(err)
		}
	})

	st := openSyncOrFatal(t, ctx, stranger)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch error = %v", err)
	}
	reconciled, err := st.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile error = %v", err)
	}
	if reconciled.State != storage.SyncReconcileUnrelated || len(st.FollowedReRoots()) != 0 {
		t.Fatalf("reconcile = %q with follows %+v; want unrelated histories and no follow", reconciled.State, st.FollowedReRoots())
	}
}

// TestSyncDoesNotFollowWhenTwoLocalCommitsRebuildToTheRoot gives bravo a
// second commit carrying the cut's tables and metadata over different
// parents. Both rebuild to the remote's root, and each would re-root bravo
// differently, so no one cut is evidenced and nothing is followed.
func TestSyncDoesNotFollowWhenTwoLocalCommitsRebuildToTheRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	withChunkStore(t, r.bravo, func(ddb *doltdb.DoltDB) {
		branch := ref.NewBranchRef("master")
		head, err := ddb.ResolveCommitRef(ctx, branch)
		if err != nil {
			t.Fatal(err)
		}
		value, meta, err := commitContent(ctx, readCommit(t, ctx, ddb, hash.Parse(r.cut)))
		if err != nil {
			t.Fatal(err)
		}
		twin, err := ddb.CommitDanglingWithParentCommits(ctx, value, []*doltdb.Commit{head}, meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := ddb.SetHeadToCommit(ctx, branch, twin); err != nil {
			t.Fatal(err)
		}
	})

	st := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch error = %v", err)
	}
	reconciled, err := st.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile error = %v", err)
	}
	if reconciled.State != storage.SyncReconcileUnrelated || len(st.FollowedReRoots()) != 0 {
		t.Fatalf("reconcile = %q with follows %+v; want unrelated histories and no follow", reconciled.State, st.FollowedReRoots())
	}
}

// TestFollowRefusesToRunUnderTheCommitLock pins the lock order: the follow
// takes Dolt's LOCK, which comes before the commit lock, so a caller holding
// the commit lock is refused before anything is read.
func TestFollowRefusesToRunUnderTheCommitLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	err := st.withCommitLock(ctx, func(ctx context.Context) error {
		_, err := st.followRemoteReRoot(ctx, "origin", "master", storage.SyncEntryPull)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "commit lock is held") {
		t.Fatalf("follow under the commit lock error = %v, want the lock-order refusal", err)
	}
}
