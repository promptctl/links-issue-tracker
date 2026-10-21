package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
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
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
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
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, true); err != nil {
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
	pulled, err := bravo.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(bravo) error = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized || pulled.Unrelated != nil {
		t.Fatalf("pull state = %q (unrelated %+v), want %q", pulled.State, pulled.Unrelated, storage.SyncPullLinearized)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryPull)
	r.assertMerged(t, ctx, bravo)
	if _, err := bravo.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("bravo's push after the follow error = %v", err)
	}

	alpha := openSyncOrFatal(t, ctx, r.alpha)
	defer func() { _ = alpha.Close() }()
	back, err := alpha.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(alpha) error = %v", err)
	}
	if back.State != storage.SyncPullFastForwarded || back.Base != storage.ReconcileBaseMergeBase || len(alpha.FollowedReRoots()) != 0 {
		t.Fatalf("alpha's pull = %q across base %q with follows %+v, want a fast-forward along shared history (%q) and no follow", back.State, back.Base, alpha.FollowedReRoots(), storage.ReconcileBaseMergeBase)
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

	if _, err := bravo.SyncPull(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncPull(bravo) error = %v", err)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryPull)
	r.assertMerged(t, ctx, bravo)
	assertCollected(t, ctx, bravo, oldHead, r.cut)
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
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	fsperm.Deny(t, unreadable, fsperm.Read)

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	if err := bravo.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(bravo) error = %v", err)
	}
	if err := bravo.SyncFollowReRoot(ctx, remoteName(t, "origin"), "master", storage.SyncEntryFetch); err != nil {
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
	received, err := bravo.SyncReceive(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReceive(bravo) error = %v", err)
	}
	if received.State != storage.SyncReceiveDiverged {
		t.Fatalf("receive state = %q, want %q", received.State, storage.SyncReceiveDiverged)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryReceive)
	reconciled, err := bravo.SyncReconcile(ctx, remoteName(t, "origin"), "master")
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
	if err := bravo.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(bravo) error = %v", err)
	}
	reconciled, err := bravo.SyncReconcile(ctx, remoteName(t, "origin"), "master")
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

	pushed, err := bravo.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), false, false)
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
		pushed, err := bravo.SyncPush(ctx, remoteName(t, "origin"), "master", false, false)
		if err != nil {
			t.Fatalf("SyncPush(bravo) error = %v", err)
		}
		r.assertFollowed(t, bravo, storage.SyncEntryPush)
		if head := headCommit(t, ctx, bravo); pushed.Head != head {
			t.Fatalf("pushed head %s, want bravo's re-rooted head %s", pushed.Head, head)
		}
		alpha := openSyncOrFatal(t, ctx, r.alpha)
		defer func() { _ = alpha.Close() }()
		back, err := alpha.SyncPull(ctx, remoteName(t, "origin"), "master")
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
		_, err := bravo.SyncPush(ctx, remoteName(t, "origin"), "master", false, false)
		if err == nil {
			t.Fatalf("SyncPush(bravo) landed over alpha's concurrent edit")
		}
		if !errors.Is(err, actions.ErrCantFF) {
			t.Fatalf("the push was not refused as non-fast-forward after following: %v", err)
		}
		r.assertFollowed(t, bravo, storage.SyncEntryPush)
		pulled, err := bravo.SyncPull(ctx, remoteName(t, "origin"), "master")
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
// push, from a clone of bravo: it follows on the clone and lands. The clone
// is thrown away after the push, so the follow collects nothing on it — its
// checkpoints still stand and its record says no GC ran — while its fork
// point is across the cut, since the receive that carries the push lands
// that fork point on the live store.
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
	pushed, err := st.SyncPushFromClone(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), false, false)
	if err != nil {
		t.Fatalf("SyncPushFromClone error = %v", err)
	}
	if pushed.Superseded != nil || pushed.Head != headCommit(t, ctx, st) {
		t.Fatalf("push = %+v, want bravo's re-rooted head %s landed", pushed, headCommit(t, ctx, st))
	}
	r.assertFollowed(t, st, storage.SyncEntryPush)
	if c := st.FollowedReRoots()[0].Collection; c != (storage.Collection{}) {
		t.Fatalf("the clone's follow collected %+v; want no collection on a clone", c)
	}
	if event := st.FollowedReRoots()[0].EventValue(); event["collection.ran"] != false {
		t.Fatalf("the clone's follow event = %v; want collection.ran false", event)
	}
	if len(branchesUnder(t, ctx, st, migrationCheckpointPrefix+"-")) == 0 {
		t.Fatalf("the clone's %s checkpoints are gone; want them left for the clone's removal, not collected", migrationCheckpointPrefix)
	}
	// The fork point moves after the retry, onto the head the remote then
	// carries: the clone's own push, above the cut.
	if forkPoint := branchesUnder(t, ctx, st, forkPointPrefix)["origin/master"]; forkPoint != pushed.Head {
		t.Fatalf("the clone's fork point is %q; want it moved across the cut onto the pushed head %s", forkPoint, pushed.Head)
	}
}

// TestARefusedPushIsRetriedWithNothingCollected drives a push the re-rooted
// remote refuses through retryPushAfterFollow alone. The retry lands while
// the history below the cut still stands, so nothing a caller finishes the
// follow with — a live store's collection below the cut — delays the retry.
func TestARefusedPushIsRetriedWithNothingCollected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	bravo := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	origin := remoteName(t, "origin")

	pushed, pushErr := bravo.pushMutation(ctx, origin, "master", false, false)
	pushed, pushErr, followed, followErr := bravo.retryPushAfterFollow(ctx, origin, "master", false, pushed, pushErr)
	if pushErr != nil || followErr != nil {
		t.Fatalf("retryPushAfterFollow error = %v, follow error = %v", pushErr, followErr)
	}
	if followed.Head == "" || pushed.Head != headCommit(t, ctx, bravo) {
		t.Fatalf("followed %+v and pushed %+v; want the follow and its retry landed", followed, pushed)
	}
	if len(branchesUnder(t, ctx, bravo, migrationCheckpointPrefix+"-")) == 0 {
		t.Fatalf("bravo's %s checkpoints are gone once the retry landed; want nothing collected before the caller finishes the follow", migrationCheckpointPrefix)
	}
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
	if err := bravo.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(bravo) error = %v", err)
	}
	_, err := bravo.SyncResolveUnrelated(ctx, remoteName(t, "origin"), "master", storage.TakeRemote, "")
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
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
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
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, true); err != nil {
		t.Fatalf("force push of alpha's re-rooted history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatal(err)
	}

	st = openSyncOrFatal(t, ctx, bravo)
	defer func() { _ = st.Close() }()
	pulled, err := st.SyncPull(ctx, remoteName(t, "origin"), "master")
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
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch error = %v", err)
	}
	reconciled, err := st.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile error = %v", err)
	}
	if reconciled.State != storage.SyncReconcileUnrelated || len(st.FollowedReRoots()) != 0 {
		t.Fatalf("reconcile = %q with follows %+v; want unrelated histories and no follow", reconciled.State, st.FollowedReRoots())
	}
}

// probeThenGap has a store on bravo probe the re-rooted remote as unrelated
// and give its engine up, as its follow does before the hold waits for LOCK;
// gap then runs, as another process would in that wait, and the store's hold
// opens after it. It returns the store, open, and what the hold returned.
func probeThenGap(t *testing.T, ctx context.Context, r reRootedRemote, gap func()) (*Store, storage.FollowedReRoot, bool) {
	t.Helper()
	origin := remoteName(t, "origin")
	first := openSyncOrFatal(t, ctx, r.bravo)
	t.Cleanup(func() { _ = first.Close() })
	if err := first.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch error = %v", err)
	}
	unrelated, err := first.unrelatedToRemote(ctx, origin, "master")
	if err != nil || !unrelated {
		t.Fatalf("the probe = unrelated %t, err %v; want the re-rooted remote unrelated", unrelated, err)
	}
	if err := first.closeEngine(); err != nil {
		t.Fatalf("closeEngine error = %v", err)
	}
	gap()
	if err := first.reopenEngine(ctx); err != nil {
		t.Fatalf("reopenEngine error = %v", err)
	}
	followed, crossed, err := first.followUnderHold(ctx, origin, "master", storage.SyncEntryFetch)
	if err != nil {
		t.Fatalf("the hold error = %v", err)
	}
	return first, followed, crossed
}

// assertCutNotFound fails unless st's one hold found no cut, for reason, and
// neither moved the branch nor declined a cut, and the hold's reading is the
// event value of reroot.cuts_not_found.
func assertCutNotFound(t *testing.T, st *Store, followed storage.FollowedReRoot, reason storage.CutNotFoundReason) {
	t.Helper()
	want := []storage.CutNotFound{{Entry: storage.SyncEntryFetch, Reason: reason}}
	if followed.Head != "" || len(st.DeclinedReRoots()) != 0 || !slices.Equal(st.CutsNotFound(), want) {
		t.Fatalf("the hold = %+v, declines %+v, cuts not found %+v; want no follow, no declined cut, and %+v", followed, st.DeclinedReRoots(), st.CutsNotFound(), want)
	}
	wantEvent := map[string]any{"reroot.cut_not_found": "fetch", "reroot.cut_not_found_reason": string(reason)}
	if event := st.CutsNotFound()[0].EventValue(); !maps.Equal(event, wantEvent) {
		t.Fatalf("the cut not found's event value = %v, want %v", event, wantEvent)
	}
}

// TestAFollowReadsTheRelationUnderItsHold interleaves two follows of one
// re-root on bravo, as two processes would: the second follows in the gap
// between the first's probe and its hold, so the first's hold opens on a
// branch already across the cut. The hold reads the histories as sharing
// commits and leaves the branch where the second follow put it.
func TestAFollowReadsTheRelationUnderItsHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	first, followed, crossed, acrossTheCut := followInTheGap(t, ctx, r)
	assertCutNotFound(t, first, followed, storage.CutNotFoundSharedHistory)
	if head := headCommit(t, ctx, first); head != acrossTheCut || !crossed {
		t.Fatalf("bravo's head after the first follow's hold = %s, crossed %t; want %s where the second follow left it, crossed", head, crossed, acrossTheCut)
	}
}

// followInTheGap is probeThenGap with a second store following the re-root
// on bravo in the gap. It also returns the head the second follow left.
func followInTheGap(t *testing.T, ctx context.Context, r reRootedRemote) (*Store, storage.FollowedReRoot, bool, string) {
	t.Helper()
	var acrossTheCut string
	first, followed, crossed := probeThenGap(t, ctx, r, func() {
		second := openSyncOrFatal(t, ctx, r.bravo)
		defer func() { _ = second.Close() }()
		if err := second.SyncFollowReRoot(ctx, remoteName(t, "origin"), "master", storage.SyncEntryFetch); err != nil {
			t.Fatalf("the second follow error = %v", err)
		}
		r.assertFollowed(t, second, storage.SyncEntryFetch)
		acrossTheCut = headCommit(t, ctx, second)
	})
	return first, followed, crossed, acrossTheCut
}

// TestARefusedPushIsRetriedWhenAnotherFollowCrossedInTheGap has bravo's push
// refused over the re-root, and another store follow it between the
// refused push's probe and its hold. The hold moves nothing, but the branch
// is across the cut, so the refusal was the re-root's and the retry lands.
func TestARefusedPushIsRetriedWhenAnotherFollowCrossedInTheGap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	origin := remoteName(t, "origin")
	refused := openSyncOrFatal(t, ctx, r.bravo)
	_, pushErr := refused.pushMutation(ctx, origin, "master", false, false)
	if err := refused.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}
	var refusal pushRefusedError
	if !errors.As(pushErr, &refusal) {
		t.Fatalf("bravo's push over the re-root error = %v, want a refusal", pushErr)
	}
	first, _, crossed, _ := followInTheGap(t, ctx, r)
	pushed, err := first.retryOverFollow(ctx, origin, "master", false, crossed, pushErr)
	if err != nil || pushed.Head != headCommit(t, ctx, first) {
		t.Fatalf("the retry = %+v, error %v; want bravo's head %s pushed", pushed, err, headCommit(t, ctx, first))
	}
}

// TestAFollowReadsARemoteWithTwoRootsAsNoCut has another writer's fetch move
// the tracking ref, between a follow's probe and its hold, onto a merge of
// the re-rooted history with a second parentless commit. Histories that still
// share no commit have no one cut to follow at a remote with two roots; a
// branch another store followed first shares the remote's history, whatever
// its roots.
func TestAFollowReadsARemoteWithTwoRootsAsNoCut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		followFirst bool
		want        storage.CutNotFoundReason
	}{
		{"unrelated", false, storage.CutNotFoundNotOneRoot},
		{"followed in the gap", true, storage.CutNotFoundSharedHistory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := newReRootedRemote(t, ctx, false)
			first, followed, crossed := probeThenGap(t, ctx, r, func() {
				if tc.followFirst {
					func() {
						second := openSyncOrFatal(t, ctx, r.bravo)
						defer func() { _ = second.Close() }()
						if err := second.SyncFollowReRoot(ctx, remoteName(t, "origin"), "master", storage.SyncEntryFetch); err != nil {
							t.Fatalf("the second follow error = %v", err)
						}
					}()
				}
				withChunkStore(t, r.bravo, func(ddb *doltdb.DoltDB) { mergeASecondRoot(t, ctx, ddb, ref.NewRemoteRef("origin", "master")) })
			})
			assertCutNotFound(t, first, followed, tc.want)
			if crossed != tc.followFirst {
				t.Fatalf("the hold read crossed %t, want %t", crossed, tc.followFirst)
			}
		})
	}
}

// mergeASecondRoot moves tracking onto a merge of its head with a parentless
// commit of the same content, so its history has two roots.
func mergeASecondRoot(t *testing.T, ctx context.Context, ddb *doltdb.DoltDB, tracking ref.DoltRef) {
	t.Helper()
	head, err := ddb.ResolveCommitRef(ctx, tracking)
	if err != nil {
		t.Fatal(err)
	}
	value, meta, err := commitContent(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	secondMeta := *meta
	secondMeta.Description = "a second root"
	scratch := ref.NewBranchRef("second-root")
	secondRoot, err := ddb.CommitWithParentCommits(ctx, value, scratch, nil, &secondMeta)
	if err != nil {
		t.Fatal(err)
	}
	if err := ddb.DeleteBranch(ctx, scratch, nil); err != nil {
		t.Fatal(err)
	}
	merged, err := ddb.CommitDanglingWithParentCommits(ctx, value, []*doltdb.Commit{head, secondRoot}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := ddb.SetHeadToCommit(ctx, tracking, merged); err != nil {
		t.Fatal(err)
	}
}

// TestAFollowWhoseTrackingRefIsPrunedUnderItsHoldFollowsNothing has another
// writer prune the tracking ref, as a receive lands a fetch's prune
// (landPrunes), between a follow's probe and its hold. The hold reads the ref
// gone, which is nothing to follow, as the probe reads a ref never fetched.
func TestAFollowWhoseTrackingRefIsPrunedUnderItsHoldFollowsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	var before string
	first, followed, crossed := probeThenGap(t, ctx, r, func() {
		withChunkStore(t, r.bravo, func(ddb *doltdb.DoltDB) {
			head, err := ddb.ResolveCommitRef(ctx, ref.NewBranchRef("master"))
			if err != nil {
				t.Fatal(err)
			}
			addr, err := head.HashOf()
			if err != nil {
				t.Fatal(err)
			}
			before = addr.String()
			if err := ddb.DeleteBranch(ctx, ref.NewRemoteRef("origin", "master"), nil); err != nil {
				t.Fatal(err)
			}
		})
	})
	assertCutNotFound(t, first, followed, storage.CutNotFoundNoTrackingRef)
	if head := headCommit(t, ctx, first); head != before || crossed {
		t.Fatalf("bravo's head after the hold = %s, crossed %t; want %s, unmoved and not crossed", head, crossed, before)
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
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch error = %v", err)
	}
	reconciled, err := st.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile error = %v", err)
	}
	notOneRebuild := []storage.CutNotFound{{Entry: storage.SyncEntryReconcile, Reason: storage.CutNotFoundNotOneRebuild}}
	if len(st.FollowedReRoots()) != 0 || len(st.DeclinedReRoots()) != 0 || !slices.Equal(st.CutsNotFound(), notOneRebuild) {
		t.Fatalf("follows = %+v, declines = %+v, cuts not found = %+v; want neither and %+v, since no one cut is evidenced", st.FollowedReRoots(), st.DeclinedReRoots(), st.CutsNotFound(), notOneRebuild)
	}
	if bases := st.ReconcileBases(); reconciled.State != storage.SyncReconcileLinearized || len(bases) != 1 || bases[0].Source != storage.ReconcileBaseForkPoint {
		t.Fatalf("reconcile = %q against %+v; want a merge against the fork point", reconciled.State, bases)
	}
}

// TestADeclinedFollowRecordsItsCutAndReason gives bravo a side line that
// forks below the cut and merges back above it. The cut is recognized — one
// local commit rebuilds to the remote's root — but no re-root of bravo's
// history at it reproduces the remote's, so the branch stays where it was and
// the decline is recorded under the entry that met it, with the cut and the
// side line that went around it.
func TestADeclinedFollowRecordsItsCutAndReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, false)
	var sideLine, head hash.Hash
	withChunkStore(t, r.bravo, func(ddb *doltdb.DoltDB) {
		branch := ref.NewBranchRef("master")
		tip, err := ddb.ResolveCommitRef(ctx, branch)
		if err != nil {
			t.Fatal(err)
		}
		value, _, err := commitContent(ctx, tip)
		if err != nil {
			t.Fatal(err)
		}
		belowCut, err := readCommit(t, ctx, ddb, hash.Parse(r.cut)).ParentHashes(ctx)
		if err != nil || len(belowCut) == 0 {
			t.Fatalf("the cut has no parent to fork a side line from: %v", err)
		}
		meta := func(description string, at uint64) *datas.CommitMeta {
			return &datas.CommitMeta{Name: "t", Email: "t@links.local", Description: description, Timestamp: at, UserTimestamp: int64(at)}
		}
		side, err := ddb.CommitDanglingWithParentCommits(ctx, value, []*doltdb.Commit{readCommit(t, ctx, ddb, belowCut[0])}, meta("side line", 1_900_000_000_000))
		if err != nil {
			t.Fatal(err)
		}
		merged, err := ddb.CommitDanglingWithParentCommits(ctx, value, []*doltdb.Commit{tip, side}, meta("merge the side line", 1_900_000_000_001))
		if err != nil {
			t.Fatal(err)
		}
		if sideLine, err = side.HashOf(); err != nil {
			t.Fatal(err)
		}
		if head, err = merged.HashOf(); err != nil {
			t.Fatal(err)
		}
		if err := ddb.SetHeadToCommit(ctx, branch, merged); err != nil {
			t.Fatal(err)
		}
	})

	st := openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch error = %v", err)
	}
	if err := st.SyncFollowReRoot(ctx, remoteName(t, "origin"), "master", storage.SyncEntryFetch); err != nil {
		t.Fatalf("SyncFollowReRoot error = %v; a declined follow is not a failure", err)
	}
	declined := st.DeclinedReRoots()
	if len(declined) != 1 || declined[0].Entry != storage.SyncEntryFetch || declined[0].Cut != r.cut ||
		!strings.Contains(declined[0].Reason, ErrSideLineAcrossCut.Error()) || !strings.Contains(declined[0].Reason, sideLine.String()) {
		t.Fatalf("DeclinedReRoots() = %+v; want one decline under %q at the cut %s naming the side line %s", declined, storage.SyncEntryFetch, r.cut, sideLine)
	}
	if followed := st.FollowedReRoots(); len(followed) != 0 {
		t.Fatalf("FollowedReRoots() = %+v; want none", followed)
	}
	if got := headCommit(t, ctx, st); got != head.String() {
		t.Fatalf("head after the decline = %s; want it unmoved at %s", got, head)
	}
}

// TestFollowAndBoundRefuseUnderTheCommitLockOnEveryRun pins where the lock
// order is asked for the two operations that take Dolt's LOCK only on the
// runs that rewrite history: on entry. Neither run here would rewrite
// anything (no remote was ever fetched), so only an entry refusal reports the
// misplaced caller.
func TestFollowAndBoundRefuseUnderTheCommitLockOnEveryRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	err := st.withCommitLock(ctx, func(ctx context.Context) error {
		_, err := st.followRemoteReRoot(ctx, remoteName(t, "origin"), "master", storage.SyncEntryPull)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "commit lock is held") {
		t.Fatalf("follow under the commit lock error = %v, want the lock-order refusal", err)
	}
	var bound storage.HistoryBound
	err = st.withCommitLock(ctx, func(ctx context.Context) error {
		bound = st.boundHistory(ctx, remoteName(t, "origin"), "master", windowOf(t, 2000, 1000))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Err == nil || !strings.Contains(bound.Err.Error(), "commit lock is held") {
		t.Fatalf("bound under the commit lock error = %v, want the lock-order refusal", bound.Err)
	}
}

// TestWithEngineClosedRefusesUnderTheCommitLock pins the lock order for the
// rewrites a follow and the bound's re-root run: they take Dolt's LOCK, which
// comes before the commit lock, so a caller holding the commit lock is
// refused before the engine is closed.
func TestWithEngineClosedRefusesUnderTheCommitLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	engine := st.db
	wrote := false
	err := st.withCommitLock(ctx, func(ctx context.Context) error {
		return st.withEngineClosed(ctx, func(context.Context, *doltdb.DoltDB) error {
			wrote = true
			return nil
		})
	})
	if err == nil || !strings.Contains(err.Error(), "commit lock is held") {
		t.Fatalf("withEngineClosed() under the commit lock error = %v, want the lock-order refusal", err)
	}
	if wrote || st.db != engine {
		t.Fatalf("withEngineClosed() under the commit lock wrote = %t, engine replaced = %t; want neither", wrote, st.db != engine)
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
