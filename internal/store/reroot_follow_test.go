package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
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

// TestAFollowCollectsTheHistoryBelowTheCut has bravo follow alpha's re-root
// through `lit sync pull`, holding every kind of local ref that names a commit
// below the cut: its migration checkpoints, the fork point its adopt recorded,
// and a re-root scratch a killed bound left. The follow drops them and runs
// the full GC, so neither bravo's old head nor the cut resolves in its store
// any more, the store shrank, and the follow's event carries both sizes.
func TestAFollowCollectsTheHistoryBelowTheCut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	oldHead := headCommit(t, ctx, bravo)
	if err := execProcedureDiscard(ctx, bravo.db, "DOLT_BRANCH", reRootScratchPrefix+"killed", r.cut); err != nil {
		t.Fatal(err)
	}
	pins := []string{migrationCheckpointPrefix + "-", migrationDriftRepairCheckpointPrefix + "-", reconcileScratchPrefix, reRootScratchPrefix}
	if len(branchesUnder(t, ctx, bravo, migrationCheckpointPrefix+"-")) == 0 {
		t.Fatalf("bravo holds no %s checkpoint, so the test cannot show the follow dropping one", migrationCheckpointPrefix)
	}

	if _, err := bravo.SyncPull(ctx, "origin", "master"); err != nil {
		t.Fatalf("SyncPull(bravo) error = %v", err)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryPull)
	r.assertMerged(t, ctx, bravo)
	for _, old := range []string{oldHead, r.cut} {
		var n int
		if err := bravo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log(?)`, old).Scan(&n); err == nil {
			t.Fatalf("%s still resolves in bravo's store, with %d commits below it; want it collected", old, n)
		}
	}
	for _, prefix := range pins {
		if left := branchesUnder(t, ctx, bravo, prefix); len(left) != 0 {
			t.Fatalf("branches under %q still pin the history below the cut: %v", prefix, left)
		}
	}
	if forkPoint, remote := branchesUnder(t, ctx, bravo, forkPointPrefix)["origin/master"], r.alphaReRoot.Head; forkPoint != remote {
		t.Fatalf("bravo's fork point is %q; want it moved onto the remote's re-rooted head %s", forkPoint, remote)
	}

	c := bravo.FollowedReRoots()[0].Collection
	if !c.Ran || c.BytesAfter >= c.BytesBefore {
		t.Fatalf("the follow's collection = %+v; want the GC run and the store smaller after it", c)
	}
	event := bravo.FollowedReRoots()[0].EventValue()
	if event["collection.ran"] != true || event["collection.bytes_before"] != c.BytesBefore || event["collection.bytes_after"] != c.BytesAfter {
		t.Fatalf("the follow's event = %v; want collection.ran true and %d -> %d bytes", event, c.BytesBefore, c.BytesAfter)
	}
}

// TestAFollowCarriesAFailedCollection has bravo's collection fail after its
// fetch followed alpha's re-root: a directory under the chunk store that the
// size reading cannot walk. The collection is maintenance, so the follow
// still succeeds, the GC still runs, and the failure rides the follow's record
// and its event's collection.error instead of failing the fetch. The fetch is
// the entry point because it settles nothing: a pull's reconcile snapshots the
// whole store, which the same directory would fail first.
func TestAFollowCarriesAFailedCollection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)
	unreadable := filepath.Join(nomsDir(r.bravo), "unreadable")
	if err := os.Mkdir(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	if err := bravo.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(bravo) error = %v", err)
	}
	if err := bravo.SyncFollowReRoot(ctx, "origin", "master", storage.SyncEntryFetch); err != nil {
		t.Fatalf("SyncFollowReRoot(bravo) error = %v; want the follow to stand whatever its collection did", err)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryFetch)

	c := bravo.FollowedReRoots()[0].Collection
	if !c.Ran || c.Err == nil || !strings.Contains(c.Err.Error(), "measure the store's size on disk") {
		t.Fatalf("the follow's collection = %+v; want the GC run and the failed size reading carried", c)
	}
	if got, ok := bravo.FollowedReRoots()[0].EventValue()["collection.error"].(*string); !ok || got == nil || *got != c.Err.Error() {
		t.Fatalf("the follow's event collection.error = %v; want %q", got, c.Err.Error())
	}
}

// TestACollectionIsReportedWhole puts every fact of a collection on its event
// and in its maintenance line, and a collection that did nothing says nothing.
func TestACollectionIsReportedWhole(t *testing.T) {
	t.Parallel()
	c := storage.Collection{Ran: true, BytesBefore: 3 << 20, BytesAfter: 1 << 20, Sweep: "remote-cache ref sweep: 2 orphaned refs", Err: errors.New("prune pre-migrate checkpoints: refused")}
	want := map[string]any{
		"collection.ran":          true,
		"collection.bytes_before": int64(3 << 20),
		"collection.bytes_after":  int64(1 << 20),
		"collection.sweep":        "remote-cache ref sweep: 2 orphaned refs",
	}
	got := c.EventRecord()
	errText, ok := got["collection.error"].(*string)
	if !ok || errText == nil || *errText != "prune pre-migrate checkpoints: refused" {
		t.Fatalf("EventRecord() collection.error = %v, want the collection's error text", got["collection.error"])
	}
	delete(got, "collection.error")
	if !maps.Equal(got, want) {
		t.Fatalf("EventRecord() = %v, want %v", got, want)
	}
	if got, want := collectionReport(c), "store 3.0 MiB -> 1.0 MiB; remote-cache ref sweep: 2 orphaned refs; history below the cut not collected: prune pre-migrate checkpoints: refused"; got != want {
		t.Fatalf("collectionReport() = %q, want %q", got, want)
	}
	if got := collectionReport(storage.Collection{}); got != "" {
		t.Fatalf("collectionReport(zero) = %q, want nothing said", got)
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

// TestACompactingPushReportsItsFollowsCollection has `lit sync push` refused
// over alpha's re-root: bravo follows, collects below the cut and pushes
// again, and the push's maintenance account carries that collection.
func TestACompactingPushReportsItsFollowsCollection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()

	pushed, err := bravo.SyncCompactAndPush(ctx, "origin", "master", false, false)
	if err != nil {
		t.Fatalf("SyncCompactAndPush(bravo) error = %v", err)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryPush)
	c := bravo.FollowedReRoots()[0].Collection
	if want := fmt.Sprintf("store %s -> %s", humanBytes(c.BytesBefore), humanBytes(c.BytesAfter)); !c.Ran || !strings.Contains(pushed.Maintenance, want) {
		t.Fatalf("the push's maintenance = %q; want the follow's collection %+v reported as %q", pushed.Maintenance, c, want)
	}
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

// TestTakeFollowsAReRootedRemoteBeforeItCanDiscardASide asks to take the
// remote's side of what the tracking ref shows as unrelated histories. The
// take follows the re-root first, finds the histories shared, and refuses as
// it refuses any mergeable divergence: no approval is asked for and bravo's
// edit stays.
func TestTakeFollowsAReRootedRemoteBeforeItCanDiscardASide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)
	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	if err := bravo.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(bravo) error = %v", err)
	}
	_, err := bravo.SyncResolveUnrelated(ctx, "origin", "master", storage.TakeRemote, "")
	var approval OwnerApprovalRequiredError
	if err == nil || errors.As(err, &approval) || !strings.Contains(err.Error(), "has a base to merge") {
		t.Fatalf("take over a re-rooted remote error = %v, want the shared-history refusal", err)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryReconcile)
	if got := getIssueOrFatal(t, ctx, bravo, r.ticket).Title; got != bravoTitle {
		t.Fatalf("ticket title = %q after the refused take, want bravo's %q", got, bravoTitle)
	}
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
// differently, so no one cut is evidenced and nothing is followed. The
// reconcile then merges against bravo's fork point, as it does for any
// checkout that cannot follow.
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
	if len(st.FollowedReRoots()) != 0 {
		t.Fatalf("follows = %+v; want none, since no one cut is evidenced", st.FollowedReRoots())
	}
	if bases := st.ReconcileBases(); reconciled.State != storage.SyncReconcileLinearized || len(bases) != 1 || bases[0].Source != storage.ReconcileBaseForkPoint {
		t.Fatalf("reconcile = %q against %+v; want a merge against the fork point", reconciled.State, bases)
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

// TestAHoldCutByItsDeadlineLeavesTheStoreUsable ends the hold's lifetime
// while the engine is closed, as a bound's deadline can mid-re-root: the
// engine reopens anyway, so the session's next query answers.
func TestAHoldCutByItsDeadlineLeavesTheStoreUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	holdCtx, cut := context.WithCancel(ctx)
	err := st.withEngineClosed(holdCtx, func(holdCtx context.Context, _ *doltdb.DoltDB) error {
		cut()
		return holdCtx.Err()
	})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "reopen the engine") {
		t.Fatalf("withEngineClosed under a cut lifetime = %v; want the cut alone, the engine reopened", err)
	}
	if _, err := activeBranch(ctx, st.db); err != nil {
		t.Fatalf("the store after a cut hold: %v; want it answering", err)
	}
}
