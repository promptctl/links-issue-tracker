package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/historywindow"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// historyCanBeReRootedVersion is the schema version that marks a store's
// history as one a push can re-root: 00010_history_can_be_re_rooted.sql.
const historyCanBeReRootedVersion = 10

// windowOf is the history window that re-roots at reRootAt and keeps keep.
func windowOf(t *testing.T, reRootAt, keep uint64) historywindow.Window {
	t.Helper()
	window, err := historywindow.New(reRootAt, keep)
	if err != nil {
		t.Fatal(err)
	}
	return window
}

// unfiredBound is a window no test store grows near: a push under it reads
// the remote head's height and does nothing else.
func unfiredBound(t *testing.T) storage.BoundWindow {
	t.Helper()
	return storage.BoundWindowRead(windowOf(t, 2000, 1000), nil)
}

// compactPush files a ticket under epic at root and pushes it the way `lit
// sync push` and the pre-push hook do, keeping bound as every landed push
// does, and returns what the bound did.
func compactPush(t *testing.T, ctx context.Context, root, epic, title string, bound historywindow.Window) storage.HistoryBound {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(%s) error = %v", root, err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", ParentID: epic}); err != nil {
		t.Fatalf("CreateIssue(%s) error = %v", title, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(%s) error = %v", root, err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	defer func() { _ = sync.Close() }()
	kept, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", storage.BoundWindowRead(bound, nil), false, false)
	if err != nil {
		t.Fatalf("SyncCompactAndPush(%s) error = %v", title, err)
	}
	bounds := sync.HistoryBounds()
	if len(bounds) != 1 {
		t.Fatalf("HistoryBounds() after one push = %+v, want one", bounds)
	}
	// The kept push carries the bound, and says it published its re-root
	// exactly when the remote's tracking ref, which the bound's fetch leaves
	// on whatever the remote carries, no longer names the pushed head.
	remote, err := commitHashOfRef(ctx, sync.db, "remotes/origin/master")
	if err != nil {
		t.Fatal(err)
	}
	if moved := remote != kept.Head; moved != (kept.Bound.Publication == storage.ReRootPublished) || kept.Bound.Publication != bounds[0].Publication {
		t.Fatalf("the push = %+v, the remote at %s; want the bound published exactly when the remote moved off the pushed head", kept, remote)
	}
	return bounds[0]
}

// heightOf is the height of rev in the store at root, as dolt_log reports it.
func heightOf(t *testing.T, ctx context.Context, st *Store, rev string) uint64 {
	t.Helper()
	var height uint64
	if err := st.db.QueryRowContext(ctx, `SELECT commit_order FROM dolt_log(?) LIMIT 1`, rev).Scan(&height); err != nil {
		t.Fatalf("read the height of %s: %v", rev, err)
	}
	return height
}

// assertCollected fails unless each of heads is gone from st's store, not
// merely unlisted: dolt_log finds no commit for it. Any other error is not
// proof it was collected. The error crosses the SQL wire as text, so it is
// matched by Dolt's own sentinel's text.
func assertCollected(t *testing.T, ctx context.Context, st *Store, heads ...string) {
	t.Helper()
	for _, head := range heads {
		var n int
		err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log(?)`, head).Scan(&n)
		if err == nil || !strings.Contains(err.Error(), datas.ErrCommitNotFound.Error()) {
			t.Fatalf("dolt_log(%s) = %d commits, error %v; want it collected (%v)", head, n, err, datas.ErrCommitNotFound)
		}
	}
}

// branchesUnder is every branch of st whose name begins with prefix.
func branchesUnder(t *testing.T, ctx context.Context, st *Store, prefix string) map[string]string {
	t.Helper()
	heads, err := refHeadsUnder(ctx, st.db, `SELECT name, hash FROM dolt_branches`, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return heads
}

// TestAPushPastTheBoundReRootsWithNoCommandRun drives ordinary writes, each
// pushed the way the pre-push hook pushes, past a small bound. The push that
// reaches it re-roots the remote and this checkout to the kept window, drops
// the refs that pinned the old history, and the store shrinks; the backlog is
// unchanged, the remote then refuses every lit whose schema support stops
// below the version that marks re-rootable history, and the next write pushes
// as an ordinary fast-forward.
func TestAPushPastTheBoundReRootsWithNoCommandRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)

	st := openSyncOrFatal(t, ctx, alpha)
	seeded := heightOf(t, ctx, st, "HEAD")
	checkpoints := len(branchesUnder(t, ctx, st, migrationCheckpointPrefix+"-"))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if checkpoints == 0 {
		t.Fatalf("the migrated store holds no %s checkpoint, so the test cannot show the bound dropping one", migrationCheckpointPrefix)
	}
	bound := windowOf(t, seeded+2, 3)

	under := compactPush(t, ctx, alpha, epic, "under", bound)
	if under.Fired() || under.Height != seeded+1 || under.Err != nil || under.ReRooted.Head != "" || under.Lease != "" || under.Publication != storage.ReRootNotPushed || under.Collection.Ran {
		t.Fatalf("a push under the bound = %+v; want height %d, not fired, nothing published", under, seeded+1)
	}
	// A fetch records the fork point every checkout that syncs holds, here on
	// the head the push under the bound left.
	st = openSyncOrFatal(t, ctx, alpha)
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatal(err)
	}
	if forkPoint := branchesUnder(t, ctx, st, forkPointPrefix)["origin/master"]; forkPoint != under.Head {
		t.Fatalf("the fork point is %q; want it on the head %s the push under the bound left", forkPoint, under.Head)
	}
	// A re-root scratch a killed bound left behind, pinning the old history:
	// the next bound to fire deletes it with its own.
	if err := execProcedureDiscard(ctx, st.db, "DOLT_BRANCH", reRootScratchPrefix+"killed", under.Head); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before := exportOf(t, ctx, alpha)

	fired := compactPush(t, ctx, alpha, epic, "past", bound)
	if fired.Err != nil {
		t.Fatalf("the push past the bound stopped: %v", fired.Err)
	}
	if !fired.Fired() || fired.Height != bound.ReRootAt() || fired.Lease != storage.LeaseHeld || !fired.Collection.Ran {
		t.Fatalf("the push past the bound = %+v; want fired at height %d, lease held, store collected", fired, bound.ReRootAt())
	}
	if fired.ReRooted.Kept() != int(bound.Keep()) || fired.ReRooted.Dropped != int(fired.Height-bound.Keep()) {
		t.Fatalf("the re-root kept %d and dropped %d commits; want %d kept and %d dropped", fired.ReRooted.Kept(), fired.ReRooted.Dropped, bound.Keep(), fired.Height-bound.Keep())
	}
	if c := fired.Collection; c.BytesAfter >= c.BytesBefore {
		t.Fatalf("the store went from %d to %d bytes across the GC; want it smaller", c.BytesBefore, c.BytesAfter)
	}

	st = openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	head := headCommit(t, ctx, st)
	if forkPoints := branchesUnder(t, ctx, st, forkPointPrefix); forkPoints["origin/master"] != head {
		t.Fatalf("fork points = %v; want origin/master's on the re-rooted head %s", forkPoints, head)
	}
	// The old history is gone from the store, not merely unlisted: neither the
	// head the push replaced nor the one the old fork point named resolves.
	assertCollected(t, ctx, st, fired.Head, under.Head)
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatal(err)
	}
	if head != fired.ReRooted.Head || heightOf(t, ctx, st, "HEAD") != bound.Keep() {
		t.Fatalf("alpha's head is %s at height %d; want the published head %s at the kept height %d", head, heightOf(t, ctx, st, "HEAD"), fired.ReRooted.Head, bound.Keep())
	}
	if remote, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || remote != head {
		t.Fatalf("the remote's head is %s (%v); want alpha's re-rooted head %s", remote, err, head)
	}
	for _, prefix := range []string{migrationCheckpointPrefix + "-", migrationDriftRepairCheckpointPrefix + "-", reconcileScratchPrefix, reRootScratchPrefix} {
		if left := branchesUnder(t, ctx, st, prefix); len(left) != 0 {
			t.Fatalf("branches under %q still pin the old history: %v", prefix, left)
		}
	}
	schema, err := st.schemaVersionAtCommit(ctx, head)
	if err != nil || schema < historyCanBeReRootedVersion {
		t.Fatalf("the re-rooted remote head records schema %d (%v); want at least %d, so every lit that cannot follow a re-root refuses to write to it", schema, err, historyCanBeReRootedVersion)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := exportOf(t, ctx, alpha); !backlogGrewBy(before, got, "past") {
		t.Fatalf("the backlog after the re-root holds %d issues; want the %d before it and the ticket the push carried", len(got.Issues), len(before.Issues))
	}

	after := compactPush(t, ctx, alpha, epic, "after", bound)
	if after.Fired() || after.Height != bound.Keep()+1 || after.Err != nil {
		t.Fatalf("the push after the re-root = %+v; want an ordinary push at height %d", after, bound.Keep()+1)
	}

	event := fired.EventValue()
	want := map[string]any{
		"history_bound.height": fired.Height,
		"history_bound.fired":  true,
		"history_bound.keep":   bound.Keep(),
		"history_bound.lease":  string(storage.LeaseHeld),
		"reroot.cut":           fired.ReRooted.Cut,
		"reroot.kept":          int(bound.Keep()),
		"reroot.dropped":       fired.ReRooted.Dropped,
	}
	for key, value := range want {
		if event[key] != value {
			t.Fatalf("event %s = %v, want %v (event %v)", key, event[key], value, event)
		}
	}
	if elapsed := event["history_bound.elapsed_ms"]; elapsed != fired.Elapsed.Milliseconds() || fired.Elapsed < fired.ReRooted.Elapsed {
		t.Fatalf("event history_bound.elapsed_ms = %v, bound elapsed %s; want the bound's whole work, which holds the re-root's %s", elapsed, fired.Elapsed, fired.ReRooted.Elapsed)
	}
}

// backlogGrewBy reports whether after holds every issue of before, with the
// same title, and one more titled title.
func backlogGrewBy(before, after model.Export, title string) bool {
	titles := map[string]string{}
	for _, issue := range after.Issues {
		titles[issue.ID] = issue.Title
	}
	for _, issue := range before.Issues {
		if titles[issue.ID] != issue.Title {
			return false
		}
		delete(titles, issue.ID)
	}
	for _, added := range titles {
		if len(titles) != 1 || added != title {
			return false
		}
	}
	return len(titles) == 1
}

// boundAt fires the bound on alpha at head, keeping keep commits, as a push
// that left the remote at head and reached the bound would; the re-root reads
// only what the window keeps.
func boundAt(t *testing.T, ctx context.Context, alpha, head string, window historywindow.Window) storage.HistoryBound {
	t.Helper()
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	b := storage.HistoryBound{Remote: "origin", Branch: "master", Head: head, Window: window}
	b.Err = st.publishBound(ctx, remoteName(t, "origin"), &b)
	return b
}

// TestAStaleLeaseLeavesTheBranchOnItsHistory has a peer push an ordinary
// commit after alpha's push landed and before alpha's re-root reached the
// remote. The lease is stale, the remote keeps the peer's push, and alpha's
// branch, checkpoints and history stay exactly as they were.
func TestAStaleLeaseLeavesTheBranchOnItsHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")
	adoptRemote(t, ctx, bravo, remoteURL)
	createAndPush(t, ctx, bravo, epic, "b1")
	bravoHead := headAt(t, ctx, bravo)
	alphaHead := headAt(t, ctx, alpha)
	st := openSyncOrFatal(t, ctx, alpha)
	checkpoints := branchesUnder(t, ctx, st, migrationCheckpointPrefix+"-")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	b := boundAt(t, ctx, alpha, alphaHead, windowOf(t, 4, 3))
	if b.Err != nil || b.Lease != storage.LeaseStale || b.Publication != storage.ReRootNotCarried || b.Collection.Ran || b.ReRooted.Head == "" {
		t.Fatalf("the bound over a remote a peer moved = %+v; want a re-root refused as stale, the remote read without it, and nothing collected", b)
	}
	if got := b.EventValue()["history_bound.publication"]; got != "not carried" {
		t.Fatalf("the bound's history_bound.publication = %v; want not carried", got)
	}
	if report := historyBoundReport(b); !strings.Contains(report, " commits and dropping ") || !strings.Contains(report, ": not carried") {
		t.Fatalf("the bound's maintenance account = %q; want the re-root reported not carried", report)
	}
	st = openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if head := headCommit(t, ctx, st); head != alphaHead {
		t.Fatalf("alpha's head moved to %s; want it left at %s", head, alphaHead)
	}
	if remote, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || remote != bravoHead {
		t.Fatalf("the remote's head is %s (%v); want bravo's push %s", remote, err, bravoHead)
	}
	if left := branchesUnder(t, ctx, st, migrationCheckpointPrefix+"-"); len(left) != len(checkpoints) {
		t.Fatalf("checkpoints = %v; want them all kept, %v", left, checkpoints)
	}
	if left := branchesUnder(t, ctx, st, reRootScratchPrefix); len(left) != 0 {
		t.Fatalf("the re-root's scratch branch was left behind: %v", left)
	}
	if followed := st.FollowedReRoots(); len(followed) != 0 {
		t.Fatalf("alpha followed %+v; want nothing to follow", followed)
	}
}

// TestAStaleLeaseOverTheLandedReRootStillFollowsIt is the transport retry the
// lease's verdict cannot see past: alpha's re-root landed, bravo followed it
// and pushed on top, and then alpha's push of the same re-root comes back
// stale. The fetch shows the remote carries the re-root, so alpha follows it,
// collects, and is left behind bravo's push rather than on the old history.
func TestAStaleLeaseOverTheLandedReRootStillFollowsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")

	// The attempt that landed: alpha's re-root on the remote, alpha not yet
	// following it.
	st := openSyncOrFatal(t, ctx, alpha)
	seen, err := commitHashOfRef(ctx, st.db, "remotes/origin/master")
	if err != nil {
		t.Fatal(err)
	}
	landed, err := st.reRootScratch(ctx, "master", seen, windowOf(t, 4, 3))
	if err != nil {
		t.Fatalf("reRootScratch error = %v", err)
	}
	if lease, err := st.publishReRoot(ctx, remoteName(t, "origin"), "master", seen); err != nil || lease != storage.LeaseHeld {
		t.Fatalf("the landed attempt = %q, %v; want held", lease, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	adoptRemote(t, ctx, bravo, remoteURL)
	createAndPush(t, ctx, bravo, epic, "b1")

	b := boundAt(t, ctx, alpha, seen, windowOf(t, 4, 3))
	if b.Lease != storage.LeaseStale {
		t.Fatalf("the retried push = %+v; want its lease stale over bravo's push", b)
	}
	if b.Err != nil || b.Publication != storage.ReRootPublished || !b.Collection.Ran {
		t.Fatalf("the bound after a stale lease over its own landed re-root = %+v; want it read as published, and alpha to follow and collect", b)
	}
	st = openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	var root string
	if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() ORDER BY commit_order LIMIT 1`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if root != landed.Root {
		t.Fatalf("alpha's history is rooted at %s; want the landed re-root's root %s", root, landed.Root)
	}
	fresh, err := st.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State() != storage.SyncBehind {
		t.Fatalf("alpha against the remote = %q; want behind bravo's push", fresh.State())
	}
}

// TestAHeldLeaseTheBranchCannotFollowIsTheBoundsError publishes a re-root
// that lands, over a local branch the follow cannot recognize: past the
// pushed head, alpha holds a second commit that rebuilds to the re-root's
// root, so no one cut is evidenced. The remote carries the re-root and the
// branch stays on the old history, unrelated to it, and the bound says so
// rather than ending as though nothing were wrong.
func TestAHeldLeaseTheBranchCannotFollowIsTheBoundsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")
	head := headAt(t, ctx, alpha)
	const keep = 3
	withChunkStore(t, alpha, func(ddb *doltdb.DoltDB) {
		branch := ref.NewBranchRef("master")
		headCommit, err := readHistoryCommit(ctx, ddb, hash.Parse(head))
		if err != nil {
			t.Fatal(err)
		}
		cut, err := cutIn(ctx, ddb, headCommit, windowOf(t, keep+1, keep))
		if err != nil {
			t.Fatal(err)
		}
		value, meta, err := commitContent(ctx, readCommit(t, ctx, ddb, cut))
		if err != nil {
			t.Fatal(err)
		}
		twin, err := ddb.CommitDanglingWithParentCommits(ctx, value, []*doltdb.Commit{headCommit.commit}, meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := ddb.SetHeadToCommit(ctx, branch, twin); err != nil {
			t.Fatal(err)
		}
	})

	b := boundAt(t, ctx, alpha, head, windowOf(t, keep+1, keep))
	if b.Lease != storage.LeaseHeld || b.Publication != storage.ReRootPublished || b.Collection.Ran {
		t.Fatalf("the bound = %+v; want its re-root to land and nothing collected", b)
	}
	if b.Err == nil || !strings.Contains(b.Err.Error(), "carries a re-root the branch did not follow") {
		t.Fatalf("the bound's error = %v; want the branch left unrelated to the remote reported", b.Err)
	}
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if remote, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || remote != b.ReRooted.Head {
		t.Fatalf("the remote's head is %s (%v); want the re-root %s", remote, err, b.ReRooted.Head)
	}
}

// mergeBubbleHistory puts alpha's master on a history whose long line came
// in as a second parent, the shape a checkout with a short line of its own
// leaves after merging a peer's, and returns its commits by name:
//
//	r - C - a1 ------------ X
//	     \                 /
//	      s1 - s2 - s3 - s4
//
// X's height is 7, its first-parent line (X a1 C r) holds 4 commits, and
// every line meets at X, C and r, and nowhere between X and C.
func mergeBubbleHistory(t *testing.T, ctx context.Context, alpha string) map[string]hash.Hash {
	t.Helper()
	c := map[string]hash.Hash{}
	withChunkStore(t, alpha, func(ddb *doltdb.DoltDB) {
		root, err := commitRoot(ctx, ddb, readCommit(t, ctx, ddb, branchHead(t, ctx, ddb, "master")))
		if err != nil {
			t.Fatal(err)
		}
		if c["r"], err = root.HashOf(); err != nil {
			t.Fatal(err)
		}
		value, _, err := commitContent(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		for i, spec := range []struct{ name, parents string }{
			{"C", "r"}, {"a1", "C"}, {"s1", "C"}, {"s2", "s1"}, {"s3", "s2"}, {"s4", "s3"}, {"X", "a1 s4"},
		} {
			var parents []*doltdb.Commit
			for _, p := range strings.Fields(spec.parents) {
				parents = append(parents, readCommit(t, ctx, ddb, c[p]))
			}
			meta := &datas.CommitMeta{Name: "t", Email: "t@links.local", Description: spec.name, Timestamp: uint64(1_800_000_000_000 + i), UserTimestamp: int64(1_800_000_000_000 + i)}
			nc, err := ddb.CommitDanglingWithParentCommits(ctx, value, parents, meta)
			if err != nil {
				t.Fatalf("commit %s: %v", spec.name, err)
			}
			if c[spec.name], err = nc.HashOf(); err != nil {
				t.Fatal(err)
			}
		}
		if err := ddb.SetHead(ctx, ref.NewBranchRef("master"), c["X"]); err != nil {
			t.Fatal(err)
		}
	})
	return c
}

// bubbleHeadOf is mergeBubbleHistory on alpha with the commit seeding the
// remote on top of X: its commits by name, the seeded head, and its height,
// 8.
func bubbleHeadOf(t *testing.T, ctx context.Context, alpha string) (map[string]hash.Hash, string) {
	t.Helper()
	remoteURL, _ := gitRemote(t)
	c := mergeBubbleHistory(t, ctx, alpha)
	seedReconcileRemote(t, ctx, alpha, remoteURL)
	head := headAt(t, ctx, alpha)
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if height := heightOf(t, ctx, st, head); height != 8 {
		t.Fatalf("the seeded head's height = %d; want 8, one above X", height)
	}
	return c, head
}

// TestABoundThatFiresCutsWhereEveryLineMeets keeps the bound over
// bubbleHeadOf's head, at height 8, where every line meets at X (height 7)
// and C (height 2) and nowhere between. A window that keeps 7 cuts at C, at
// the height it keeps; one that keeps 6 under 8 would cut inside the bubble,
// so it cuts at C too, keeping 7; one that keeps 2 cuts at X. Each re-root
// lands, leaves the head where the window no longer fires, alpha follows it,
// and the backlog is unchanged.
func TestABoundThatFiresCutsWhereEveryLineMeets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reRootAt, keep, height uint64
		cut                    string
	}{
		{reRootAt: 8, keep: 7, height: 7, cut: "C"},
		{reRootAt: 8, keep: 6, height: 7, cut: "C"},
		{reRootAt: 3, keep: 2, height: 2, cut: "X"},
	} {
		t.Run(fmt.Sprintf("window %d/%d", tc.reRootAt, tc.keep), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			alpha := migratedDoltDir(t)
			c, head := bubbleHeadOf(t, ctx, alpha)
			before := exportOf(t, ctx, alpha)
			window := windowOf(t, tc.reRootAt, tc.keep)

			b := boundAt(t, ctx, alpha, head, window)
			if b.Err != nil || b.ReRooted.Cut != c[tc.cut].String() || b.ReRooted.Height != tc.height || b.Lease != storage.LeaseHeld || b.Publication != storage.ReRootPublished || !b.Collection.Ran {
				t.Fatalf("the bound over a merge-heavy history = %+v; want it cut at %s %s leaving height %d, published, followed and collected", b, tc.cut, c[tc.cut], tc.height)
			}
			if (storage.HistoryBound{Height: b.ReRooted.Height, Window: window}).Fired() {
				t.Fatalf("the re-root left the head at height %d, where %s fires again", b.ReRooted.Height, window)
			}
			st := openSyncOrFatal(t, ctx, alpha)
			got := heightOf(t, ctx, st, "HEAD")
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if got != tc.height {
				t.Fatalf("alpha's head height after following = %d; want the re-root's, %d", got, tc.height)
			}
			if after := exportOf(t, ctx, alpha); !reflect.DeepEqual(after, before) {
				t.Fatalf("the backlog after the re-root:\n%+v\nwant\n%+v", after, before)
			}
		})
	}
}

// TestABubbleSpanningTheWindowLeavesNoCut keeps a window that re-roots at 7
// and keeps 6 over bubbleHeadOf's head, at height 8. Only a cut at height 2
// or 3 leaves the head inside it; the lines meet at C, at 2, which leaves 7,
// and not at 3, inside the bubble. Nothing is written or pushed, and the
// bound's error names X, the lowest the lines meet above the window, where it
// cuts once the head is at height 12.
func TestABubbleSpanningTheWindowLeavesNoCut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	alpha := migratedDoltDir(t)
	c, head := bubbleHeadOf(t, ctx, alpha)

	b := boundAt(t, ctx, alpha, head, windowOf(t, 7, 6))
	if b.Err == nil || !strings.Contains(b.Err.Error(), c["X"].String()+", at height 7, where the bound cuts once the head is at height 12") || b.Lease != "" || b.ReRooted.Head != "" {
		t.Fatalf("the bound over a bubble spanning the window = %+v; want no cut, nothing pushed, naming X at height 7 and height 12", b)
	}
	if got := headAt(t, ctx, alpha); got != head {
		t.Fatalf("alpha's head = %s; want it unchanged at %s", got, head)
	}
}

// TestAReRootPushWithNoVerdictLeavesNoScratch has the bound's lease push
// reach no verdict: the remote's path is a file, not a git repository. The
// push ran, so the fetch after it is what could settle whether an attempt
// landed; it fails too, and the re-root stays possibly published. The scratch
// the re-root was written on is gone anyway, so nothing keeps the history
// below the cut reachable.
func TestAReRootPushWithNoVerdictLeavesNoScratch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, remotePath := gitRemote(t)
	alpha := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")
	head := headAt(t, ctx, alpha)
	if err := os.Rename(remotePath, remotePath+".gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remotePath, []byte("not a store"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := boundAt(t, ctx, alpha, head, windowOf(t, 4, 3))
	if b.Err == nil || b.Lease != storage.LeaseUnsettled || b.Publication != storage.ReRootPossiblyPublished || b.ReRooted.Head == "" {
		t.Fatalf("the bound over a remote that is a file = %+v; want a re-root written, its push unsettled, possibly published, and an error", b)
	}
	if !strings.Contains(b.Err.Error(), "fetch to learn whether") {
		t.Fatalf("the bound's error = %v; want the fetch that would settle the unsettled push to have run", b.Err)
	}
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if left := branchesUnder(t, ctx, st, reRootScratchPrefix); len(left) != 0 {
		t.Fatalf("the re-root's scratch branch was left behind: %v", left)
	}
}

// TestAReRootRefusedBeforeTheRemoteIsNotPushed has the bound's lease push
// refused before it is sent: the head it would replace is at a schema above
// this binary's. The re-root is written and never leaves the checkout, so the
// bound says not pushed and reads nothing: alpha's tracking ref still lags
// where its last fetch left it.
func TestAReRootRefusedBeforeTheRemoteIsNotPushed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	seedReconcileRemote(t, ctx, alpha, remoteURL)
	adoptRemote(t, ctx, bravo, remoteURL)

	// bravo, as a newer binary would, advances the remote's schema.
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Open(ctx, bravo, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newer.db.ExecContext(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, registryMax+1); err != nil {
		t.Fatal(err)
	}
	if err := newer.commitWorkingSet(ctx, "a newer binary's schema"); err != nil {
		t.Fatal(err)
	}
	if err := newer.Close(); err != nil {
		t.Fatal(err)
	}
	pusher := openSyncOrFatal(t, ctx, bravo)
	if _, err := pusher.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(bravo) error = %v", err)
	}
	if err := pusher.Close(); err != nil {
		t.Fatal(err)
	}
	ahead := headAt(t, ctx, bravo)

	// alpha reads the remote's head through a second name for it, so the
	// head is in alpha's store while remotes/origin/master still lags.
	st := openSyncOrFatal(t, ctx, alpha)
	if err := st.SyncAddRemote(ctx, remoteName(t, "mirror"), remoteURL); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, "mirror")); err != nil {
		t.Fatal(err)
	}
	lagging, err := commitHashOfRef(ctx, st.db, "remotes/origin/master")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	b := boundAt(t, ctx, alpha, ahead, windowOf(t, 4, 3))
	if !errors.As(b.Err, new(*RemoteSchemaAheadError)) || b.ReRooted.Head == "" || b.Lease != "" || b.Publication != storage.ReRootNotPushed || b.RemoteGC.Outcome != "" {
		t.Fatalf("the bound over a schema-ahead head = %+v; want a re-root written, refused before the remote, not pushed, and no remote GC", b)
	}
	st = openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if got, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || got != lagging {
		t.Fatalf("remotes/origin/master after the refusal = %s (%v); want it left at %s, no fetch run", got, err, lagging)
	}
}

// TestAReRootsPublicationIsTheReadingsElseTheLeases walks every lease
// outcome that reached the remote against every reading of it: a reading
// decides, and without one only a held lease says the remote carries it.
func TestAReRootsPublicationIsTheReadingsElseTheLeases(t *testing.T) {
	for _, tc := range []struct {
		lease   storage.LeaseOutcome
		reading remoteReading
		want    storage.ReRootPublication
	}{
		{storage.LeaseHeld, remoteCarries, storage.ReRootPublished},
		{storage.LeaseStale, remoteCarries, storage.ReRootPublished},
		{storage.LeaseUnsettled, remoteCarries, storage.ReRootPublished},
		{storage.LeaseHeld, remoteLacks, storage.ReRootNotCarried},
		{storage.LeaseStale, remoteLacks, storage.ReRootNotCarried},
		{storage.LeaseUnsettled, remoteLacks, storage.ReRootNotCarried},
		{storage.LeaseHeld, remoteUnread, storage.ReRootPublished},
		{storage.LeaseStale, remoteUnread, storage.ReRootPossiblyPublished},
		{storage.LeaseUnsettled, remoteUnread, storage.ReRootPossiblyPublished},
	} {
		if got := publicationOf(tc.lease, tc.reading); got != tc.want {
			t.Errorf("publicationOf(%s, %d) = %s; want %s", tc.lease, tc.reading, got, tc.want)
		}
	}
}
