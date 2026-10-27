package store

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/google/uuid"

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
	// epics is the epic every ticket commit() files into, by store root; a
	// copy of a root shares its epic, a store of unrelated history seeds its
	// own. Seeding it is its own commit, so each commit() is exactly one.
	epics map[string]string
}

func newLandFixture(t *testing.T) *landFixture {
	t.Helper()
	f := &landFixture{t: t, ctx: context.Background(), base: t.TempDir(), live: migratedDoltDir(t), epics: map[string]string{}}
	f.remoteURL = "file://" + filepath.Join(f.base, "remote")
	f.seedEpic(f.live)
	f.commit(f.live, "c1")
	st := f.openSync(f.live)
	if err := st.SyncAddRemote(f.ctx, remoteName(t, "origin"), f.remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := st.SyncPush(f.ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("seed push error = %v", err)
	}
	f.close(st)
	return f
}

func (f *landFixture) openSync(root string) *Store {
	f.t.Helper()
	st, err := OpenSync(f.ctx, testEnv(), testStderr(), root, "ws")
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

// seedEpic lands the epic commit() files tickets into on the store at root.
func (f *landFixture) seedEpic(root string) {
	f.t.Helper()
	st, err := Open(f.ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		f.t.Fatalf("Open(epic) error = %v", err)
	}
	defer f.close(st)
	epic, err := st.CreateIssue(f.ctx, storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		f.t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	f.epics[root] = epic.ID
}

// commit lands one issue on the store at root and returns the new head.
func (f *landFixture) commit(root, title string) string {
	f.t.Helper()
	epic, ok := f.epics[root]
	if !ok {
		f.t.Fatalf("commit(%q): no epic seeded on %s", title, root)
	}
	st, err := Open(f.ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		f.t.Fatalf("Open(%q) error = %v", title, err)
	}
	defer f.close(st)
	if _, err := st.CreateIssue(f.ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic}); err != nil {
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
	f.epics[dst] = f.epics[root]
	return dst
}

// push pushes the store at root to the remote; force rewrites it.
func (f *landFixture) push(root string, force bool) {
	f.t.Helper()
	st := f.openSync(root)
	defer f.close(st)
	if _, err := st.SyncPush(f.ctx, remoteName(f.t, "origin"), "master", false, force); err != nil {
		f.t.Fatalf("SyncPush(force=%t) error = %v", force, err)
	}
}

// takenClone reads the clone at root as the receive's take does, before
// anything runs on it.
func takenClone(t *testing.T, ctx context.Context, root string) TakenClone {
	t.Helper()
	taken, err := ReadTakenClone(ctx, root)
	if err != nil {
		t.Fatalf("ReadTakenClone(%s) error = %v", root, err)
	}
	return taken
}

// fetchedClone is the receive's shape: a clone of the live store, taken, that
// fetched.
func (f *landFixture) fetchedClone() TakenClone {
	f.t.Helper()
	clone, _ := f.prunedClone()
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
	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", nil)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if landed.Record != FetchedHeadMoved {
		t.Fatalf("LandFetchedHead() = %s, want moved", landed.Record)
	}
	if landed.Held <= 0 {
		t.Fatalf("LandFetchedHead() held = %s, want the landing's hold measured", landed.Held)
	}
	if got := f.trackingRef(); got != peerHead {
		t.Fatalf("tracking ref after landing = %s, want the peer's head %s", got, peerHead)
	}
	if err := os.RemoveAll(filepath.Dir(clone.root)); err != nil {
		t.Fatalf("remove clone: %v", err)
	}

	st := f.openSync(f.live)
	defer f.close(st)
	result, err := st.SyncSettleReceived(f.ctx, remoteName(t, "origin"), "master")
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

// TestLandFetchedHeadRecordsWhatTheFetchSaw pins that a ref the live store
// left as the take saw it follows the fetch whatever the relation: backwards
// onto a remote that was reset, onto a rewritten history that shares an
// ancestor, and onto an unrelated one. A fetch records where the remote was
// when it looked. The superseded arm is the other side of the take: a clone
// taken before the live store fetched a later push itself lands nothing, and
// the live fetch's ref stands. The unchanged arm is the pair: a fetch that
// found what the ref already names writes nothing.
func TestLandFetchedHeadRecordsWhatTheFetchSaw(t *testing.T) {
	t.Parallel()
	liveFetch := func(f *landFixture) {
		t.Helper()
		st := f.openSync(f.live)
		defer f.close(st)
		if err := st.SyncFetch(f.ctx, remoteName(f.t, "origin")); err != nil {
			t.Fatalf("SyncFetch(live) error = %v", err)
		}
	}
	land := func(f *landFixture, clone TakenClone) LandedFetch {
		t.Helper()
		landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", nil)
		if err != nil {
			t.Fatalf("LandFetchedHead() error = %v", err)
		}
		return landed
	}
	t.Run("backwards onto a reset remote", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		seed := f.trackingRef()
		resetter := f.copyOf(f.live)
		peer := f.copyOf(f.live)
		f.commit(peer, "peer-ticket")
		f.push(peer, false)
		liveFetch(f)
		f.push(resetter, true)
		if landed := land(f, f.fetchedClone()); landed.Record != FetchedHeadMoved || f.trackingRef() != seed {
			t.Fatalf("LandFetchedHead() = %s, ref %s; want moved back onto the reset remote's head %s", landed.Record, f.trackingRef(), seed)
		}
	})
	t.Run("superseded by a live fetch since the take", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		stale := f.fetchedClone()
		peer := f.copyOf(f.live)
		peerHead := f.commit(peer, "peer-ticket")
		f.push(peer, false)
		liveFetch(f)
		if got := f.trackingRef(); got != peerHead {
			t.Fatalf("tracking ref after the live fetch = %s, want %s", got, peerHead)
		}
		if landed := land(f, stale); landed.Record != FetchedHeadSuperseded || f.trackingRef() != peerHead {
			t.Fatalf("LandFetchedHead() = %s, ref %s; want superseded, the live fetch's %s standing", landed.Record, f.trackingRef(), peerHead)
		}
	})
	t.Run("rewritten with a shared ancestor", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		rewriter := f.copyOf(f.live)
		peer := f.copyOf(f.live)
		f.commit(peer, "peer-ticket")
		f.push(peer, false)
		liveFetch(f)
		rewrittenHead := f.commit(rewriter, "rewritten-ticket")
		f.push(rewriter, true)
		if landed := land(f, f.fetchedClone()); landed.Record != FetchedHeadMoved || f.trackingRef() != rewrittenHead {
			t.Fatalf("LandFetchedHead() = %s, ref %s; want moved onto the rewritten head %s", landed.Record, f.trackingRef(), rewrittenHead)
		}
	})
	t.Run("unrelated history", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		unrelated := unrelatedDoltDir(t)
		f.seedEpic(unrelated)
		unrelatedHead := f.commit(unrelated, "unrelated-ticket")
		st := f.openSync(unrelated)
		if err := st.SyncAddRemote(f.ctx, remoteName(t, "origin"), f.remoteURL); err != nil {
			t.Fatalf("SyncAddRemote(unrelated) error = %v", err)
		}
		f.close(st)
		f.push(unrelated, true)
		if landed := land(f, f.fetchedClone()); landed.Record != FetchedHeadMoved || f.trackingRef() != unrelatedHead {
			t.Fatalf("LandFetchedHead() = %s, ref %s; want moved onto the unrelated head %s", landed.Record, f.trackingRef(), unrelatedHead)
		}
	})
	t.Run("unchanged", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		before := f.trackingRef()
		if landed := land(f, f.fetchedClone()); landed.Record != FetchedHeadUnchanged || f.trackingRef() != before {
			t.Fatalf("LandFetchedHead() = %s, ref %s; want unchanged at %s", landed.Record, f.trackingRef(), before)
		}
	})
}

// TestLandingOfMovesOnlyARefStandingAsTaken pins the landing's decision over
// every way the take, the live store and the clone's fetch can see the ref.
func TestLandingOfMovesOnlyARefStandingAsTaken(t *testing.T) {
	t.Parallel()
	at := func(s string) refHead { return refHead{addr: hash.Of([]byte(s)), held: true} }
	absent := refHead{}
	for _, c := range []struct {
		name                 string
		taken, live, fetched refHead
		want                 FetchedHeadRecord
	}{
		{"the remote moved", at("a"), at("a"), at("b"), FetchedHeadMoved},
		{"a first fetch", absent, absent, at("b"), FetchedHeadMoved},
		{"the remote did not move", at("a"), at("a"), at("a"), FetchedHeadUnchanged},
		{"the live store reached the fetched head since the take", at("a"), at("b"), at("b"), FetchedHeadUnchanged},
		{"the live store moved the ref since the take", at("a"), at("c"), at("b"), FetchedHeadSuperseded},
		{"the live store moved the ref back to the fetched head's parent", at("a"), at("c"), at("a"), FetchedHeadSuperseded},
		{"the live store created the ref since the take", absent, at("c"), at("b"), FetchedHeadSuperseded},
		{"the live store deleted the ref since the take", at("a"), absent, at("b"), FetchedHeadSuperseded},
		{"the remote has no such branch", at("a"), at("a"), absent, FetchedHeadAbsent},
		{"the remote has no such branch and the live store moved", at("a"), at("c"), absent, FetchedHeadSuperseded},
		{"the remote has no such branch the live store never held", absent, absent, absent, FetchedHeadAbsent},
		{"the live store pruned the branch the fetch found gone", at("a"), absent, absent, FetchedHeadAbsent},
	} {
		if got := landingOf(c.taken, c.live, c.fetched); got != c.want {
			t.Errorf("%s: landingOf() = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestALandingNeverWindsTheLiveRefBackAcrossACut is the receive racing a
// re-rooting push: the receive's clone is taken and fetches the remote's
// pre-cut head, then a push on the live store reaches the bound, re-roots the
// remote, moves the live tracking ref onto the re-rooted head and collects the
// history below the cut, and only then does the landing run. The ref stays on
// the re-rooted head, the pre-cut head stays collected, and the settle finds
// the branch up to date with the remote rather than unrelated to it.
func TestALandingNeverWindsTheLiveRefBackAcrossACut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	live := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, live, remoteURL)
	st := openSyncOrFatal(t, ctx, live)
	seeded := heightOf(t, ctx, st, "HEAD")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	bound := windowOf(t, seeded+2, 3)
	under, _ := compactPush(t, ctx, live, epic, "under", bound)
	if under.Fired() || under.Err != nil {
		t.Fatalf("a push under the bound = %+v; want it not fired", under)
	}

	cloneRoot := filepath.Join(t.TempDir(), "dolt")
	if err := dbsnapshot.CloneTree(ctx, live, cloneRoot); err != nil {
		t.Fatalf("CloneTree() error = %v", err)
	}
	clone := takenClone(t, ctx, cloneRoot)
	fetcher := openSyncOrFatal(t, ctx, cloneRoot)
	if err := fetcher.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	fetched, err := commitHashOfRef(ctx, fetcher.db, "remotes/origin/master")
	if err != nil || fetched != under.Head {
		t.Fatalf("the clone fetched %s (%v); want the pre-cut head %s", fetched, err, under.Head)
	}
	if err := fetcher.Close(); err != nil {
		t.Fatalf("Close(clone) error = %v", err)
	}

	fired, collected := compactPush(t, ctx, live, epic, "past", bound)
	if !fired.Fired() || fired.Err != nil || !collected.Ran {
		t.Fatalf("the push past the bound = %+v, collection %+v; want it fired and the store collected", fired, collected)
	}

	landed, err := LandFetchedHead(ctx, testEnv(), testStderr(), live, clone, remoteName(t, "origin"), "master", nil)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if landed.Record != FetchedHeadSuperseded {
		t.Fatalf("LandFetchedHead() = %s, want superseded by the re-rooting push", landed.Record)
	}
	st = openSyncOrFatal(t, ctx, live)
	defer func() { _ = st.Close() }()
	if ref, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || ref != fired.ReRooted.Head {
		t.Fatalf("the live tracking ref is %s (%v); want the re-rooted head %s", ref, err, fired.ReRooted.Head)
	}
	assertCollected(t, ctx, st, under.Head)
	result, err := st.SyncSettleReceived(ctx, remoteName(t, "origin"), "master")
	if err != nil || result.State != storage.SyncReceiveUpToDate {
		t.Fatalf("SyncSettleReceived() = %+v (%v); want up to date with the re-rooted remote", result, err)
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
	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "no-such-branch", nil)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if landed.Record != FetchedHeadAbsent {
		t.Fatalf("LandFetchedHead() = %s, want absent", landed.Record)
	}
	if got := f.trackingRef(); got != before {
		t.Fatalf("landing an absent branch moved remotes/origin/master from %s to %s", before, got)
	}
}

// exec runs procedure with args on the store at root.
func (f *landFixture) exec(root, procedure string, args ...string) {
	f.t.Helper()
	st := f.openSync(root)
	defer f.close(st)
	if err := execProcedureDiscard(f.ctx, st.db, procedure, args...); err != nil {
		f.t.Fatalf("%s%v on %s: %v", procedure, args, root, err)
	}
}

// liveFetch fetches origin into the live store itself, as a foreground sync
// does.
func (f *landFixture) liveFetch() {
	f.t.Helper()
	st := f.openSync(f.live)
	defer f.close(st)
	if err := st.SyncFetch(f.ctx, remoteName(f.t, "origin")); err != nil {
		f.t.Fatalf("SyncFetch(live) error = %v", err)
	}
}

// prunedClone is fetchedClone with what the clone's fetch pruned, as the
// receive reads it off the clone's session.
func (f *landFixture) prunedClone() (TakenClone, []storage.PrunedRef) {
	f.t.Helper()
	clone := f.copyOf(f.live)
	taken := takenClone(f.t, f.ctx, clone)
	st := f.openSync(clone)
	defer f.close(st)
	if err := st.SyncFetch(f.ctx, remoteName(f.t, "origin")); err != nil {
		f.t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	return taken, st.PrunedRefs()
}

// liveRefs is the live store's tracking refs and fork points of origin, by
// branch.
func (f *landFixture) liveRefs() (tracked, forkPoints map[string]string) {
	f.t.Helper()
	st := f.openSync(f.live)
	defer f.close(st)
	owner := ownerOf(remoteName(f.t, "origin"))
	tracked, err := refHeadsUnder(f.ctx, st.db, `SELECT name, hash FROM dolt_remote_branches`, trackingRefPrefix(owner))
	if err != nil {
		f.t.Fatalf("list tracking refs: %v", err)
	}
	forkPoints, err = refHeadsUnder(f.ctx, st.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(owner, ""))
	if err != nil {
		f.t.Fatalf("list fork points: %v", err)
	}
	return tracked, forkPoints
}

// TestLandFetchedHeadCarriesTheClonesPrunes deletes a branch on the remote
// whose live fork point trails its tracking ref — the live store fetched it
// across a move, so the fork point holds the older head — and lands a clone
// whose fetch pruned it. The live tracking ref goes because it still holds
// the head the clone pruned, and the fork point goes because it is left
// without its ref, whatever head it trails at; the sync branch's ref and fork
// point stay.
func TestLandFetchedHeadCarriesTheClonesPrunes(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	peer := f.copyOf(f.live)
	f.exec(peer, "DOLT_BRANCH", "feature")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	f.liveFetch()
	_, before := f.liveRefs()
	older := f.commit(peer, "peer-ticket")
	f.exec(peer, "DOLT_BRANCH", "-f", "feature", "master")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	f.liveFetch()
	tracked, forkPoints := f.liveRefs()
	if tracked["feature"] != older || forkPoints["feature"] == "" || forkPoints["feature"] == older {
		t.Fatalf("live feature ref %s, fork point %q; want the ref at %s and a fork point trailing it", tracked["feature"], forkPoints["feature"], older)
	}
	trailing := forkPoints["feature"]
	if before["master"] == "" {
		t.Fatalf("live fork points before the prune = %v, want master's recorded", before)
	}
	f.exec(peer, "DOLT_PUSH", "origin", ":feature")

	clone, pruned := f.prunedClone()
	wantPruned := []storage.PrunedRef{{Remote: "origin", Branch: "feature", Head: older}}
	if !slices.Equal(pruned, wantPruned) {
		t.Fatalf("the clone's PrunedRefs() = %+v, want %+v", pruned, wantPruned)
	}
	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", pruned)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if !slices.Equal(landed.Pruned, wantPruned) {
		t.Errorf("LandedFetch.Pruned = %+v, want %+v", landed.Pruned, wantPruned)
	}
	wantDropped := []storage.ForkPointRecord{{Remote: "origin", Branch: "feature", Head: trailing, Outcome: storage.ForkPointDropped}}
	if !slices.Equal(landed.ForkPointsDropped, wantDropped) {
		t.Errorf("LandedFetch.ForkPointsDropped = %+v, want %+v", landed.ForkPointsDropped, wantDropped)
	}
	tracked, forkPoints = f.liveRefs()
	if _, ok := tracked["feature"]; ok || tracked["master"] == "" {
		t.Errorf("live tracking refs after the landing = %v, want master's alone", tracked)
	}
	if _, ok := forkPoints["feature"]; ok || forkPoints["master"] == "" {
		t.Errorf("live fork points after the landing = %v, want master's alone", forkPoints)
	}
}

// TestLandFetchedHeadLeavesRefsTheLiveStoreWroteSinceTheTake lands a clone
// whose fetch pruned feature after the live store, meanwhile, fetched the
// remote with feature recreated at another head and a branch newer the clone
// never saw. Neither is the ref the clone pruned, so both stand, with their
// fork points.
func TestLandFetchedHeadLeavesRefsTheLiveStoreWroteSinceTheTake(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	peer := f.copyOf(f.live)
	f.exec(peer, "DOLT_BRANCH", "feature")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	f.liveFetch()
	f.exec(peer, "DOLT_PUSH", "origin", ":feature")

	clone, pruned := f.prunedClone()
	if len(pruned) != 1 || pruned[0].Branch != "feature" {
		t.Fatalf("the clone's PrunedRefs() = %+v, want feature's", pruned)
	}
	recreated := f.commit(peer, "peer-ticket")
	f.exec(peer, "DOLT_BRANCH", "-f", "feature", "master")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	f.exec(peer, "DOLT_BRANCH", "newer")
	f.exec(peer, "DOLT_PUSH", "origin", "newer")
	f.liveFetch()
	trackedBefore, forkPointsBefore := f.liveRefs()
	if trackedBefore["feature"] != recreated || trackedBefore["newer"] == "" {
		t.Fatalf("live tracking refs before the landing = %v, want feature at %s and newer", trackedBefore, recreated)
	}

	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", pruned)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if len(landed.Pruned) != 0 || len(landed.ForkPointsDropped) != 0 {
		t.Errorf("LandFetchedHead() pruned %+v and dropped %+v; want nothing deleted", landed.Pruned, landed.ForkPointsDropped)
	}
	tracked, forkPoints := f.liveRefs()
	if !maps.Equal(tracked, trackedBefore) || !maps.Equal(forkPoints, forkPointsBefore) {
		t.Errorf("live refs after the landing = %v / %v, want them as they stood, %v / %v", tracked, forkPoints, trackedBefore, forkPointsBefore)
	}
}

// TestLandFetchedHeadLeavesARefTheClonesSessionFetchedBack lands a clone
// whose session fetched twice: the first fetch pruned feature, and the
// second brought it back at the very head it pruned, as a peer re-pushing it
// does. The clone ends holding feature, so the prune is undone and the live
// ref, at the same head, stands with its fork point.
func TestLandFetchedHeadLeavesARefTheClonesSessionFetchedBack(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	peer := f.copyOf(f.live)
	f.exec(peer, "DOLT_BRANCH", "feature")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	f.liveFetch()
	trackedBefore, forkPointsBefore := f.liveRefs()
	f.exec(peer, "DOLT_PUSH", "origin", ":feature")

	cloneRoot := f.copyOf(f.live)
	clone := takenClone(t, f.ctx, cloneRoot)
	st := f.openSync(cloneRoot)
	if err := st.SyncFetch(f.ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	if err := st.SyncFetch(f.ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("second SyncFetch(clone) error = %v", err)
	}
	pruned := st.PrunedRefs()
	f.close(st)
	wantPruned := []storage.PrunedRef{{Remote: "origin", Branch: "feature", Head: trackedBefore["feature"]}}
	if !slices.Equal(pruned, wantPruned) {
		t.Fatalf("the clone's PrunedRefs() = %+v, want %+v", pruned, wantPruned)
	}

	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", pruned)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if len(landed.Pruned) != 0 || len(landed.ForkPointsDropped) != 0 {
		t.Errorf("LandFetchedHead() pruned %+v and dropped %+v; want nothing deleted", landed.Pruned, landed.ForkPointsDropped)
	}
	tracked, forkPoints := f.liveRefs()
	if !maps.Equal(tracked, trackedBefore) || !maps.Equal(forkPoints, forkPointsBefore) {
		t.Errorf("live refs after the landing = %v / %v, want them as they stood, %v / %v", tracked, forkPoints, trackedBefore, forkPointsBefore)
	}
}

// TestLandFetchedHeadPrunesARefTheClonesSessionMovedFirst lands a clone
// whose session fetched twice: the first fetch moved feature past the head
// the take saw, and the second pruned it there. The live ref still stands as
// taken, so the prune is the newest word on it and it goes, though the clone
// pruned it at another head.
func TestLandFetchedHeadPrunesARefTheClonesSessionMovedFirst(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	peer := f.copyOf(f.live)
	f.exec(peer, "DOLT_BRANCH", "feature")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	f.liveFetch()
	trackedBefore, _ := f.liveRefs()

	cloneRoot := f.copyOf(f.live)
	clone := takenClone(t, f.ctx, cloneRoot)
	moved := f.commit(peer, "peer-ticket")
	f.exec(peer, "DOLT_BRANCH", "-f", "feature", "master")
	f.exec(peer, "DOLT_PUSH", "origin", "feature")
	st := f.openSync(cloneRoot)
	if err := st.SyncFetch(f.ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	f.exec(peer, "DOLT_PUSH", "origin", ":feature")
	if err := st.SyncFetch(f.ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("second SyncFetch(clone) error = %v", err)
	}
	pruned := st.PrunedRefs()
	f.close(st)
	wantPruned := []storage.PrunedRef{{Remote: "origin", Branch: "feature", Head: moved}}
	if !slices.Equal(pruned, wantPruned) || moved == trackedBefore["feature"] {
		t.Fatalf("the clone's PrunedRefs() = %+v, want %+v, pruned past the taken %s", pruned, wantPruned, trackedBefore["feature"])
	}

	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", pruned)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if !slices.Equal(landed.Pruned, wantPruned) {
		t.Errorf("LandedFetch.Pruned = %+v, want %+v", landed.Pruned, wantPruned)
	}
	if tracked, _ := f.liveRefs(); tracked["feature"] != "" {
		t.Errorf("live tracking refs after the landing = %v, want feature's gone", tracked)
	}
}

// TestLandFetchedHeadKeepsTheSyncBranchsForkPointAcrossARePoint lands a
// clone's fetch on a live store whose remote was re-pointed, which removes
// the tracking refs and keeps the fork points until a fetch brings the refs
// back. The landing sets the sync branch's ref before it drops orphans, so
// the sync branch's fork point stays.
func TestLandFetchedHeadKeepsTheSyncBranchsForkPointAcrossARePoint(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.liveFetch()
	_, forkPointsBefore := f.liveRefs()
	if forkPointsBefore["master"] == "" {
		t.Fatalf("live fork points before the re-point = %v, want master's", forkPointsBefore)
	}
	link := filepath.Join(f.base, "remote-link")
	if err := os.Symlink(strings.TrimPrefix(f.remoteURL, "file://"), link); err != nil {
		t.Fatalf("link the remote: %v", err)
	}
	st := f.openSync(f.live)
	if err := st.SyncSetRemoteURL(f.ctx, remoteName(t, "origin"), "file://"+link); err != nil {
		t.Fatalf("SyncSetRemoteURL() error = %v", err)
	}
	f.close(st)

	clone, pruned := f.prunedClone()
	landed, err := LandFetchedHead(f.ctx, testEnv(), testStderr(), f.live, clone, remoteName(t, "origin"), "master", pruned)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	if len(landed.ForkPointsDropped) != 0 {
		t.Errorf("LandedFetch.ForkPointsDropped = %+v, want none", landed.ForkPointsDropped)
	}
	tracked, forkPoints := f.liveRefs()
	if tracked["master"] == "" || forkPoints["master"] != forkPointsBefore["master"] {
		t.Errorf("live refs after the landing = %v / %v, want master's ref back and its fork point at %s", tracked, forkPoints, forkPointsBefore["master"])
	}
}

// TestLandFetchedHeadKeepsAFollowTheLiveStoreRanDuringTheClonesFetch runs the
// follow inside the receive: bravo's clone is taken, bravo pulls alpha's
// re-root and collects the history below the cut, and only then does the
// clone fetch, recording its fork point against its own pre-cut snapshot.
// The landing derives the fork point from bravo's own branch instead: it
// stays at the remote's re-rooted head where the follow put it, and the
// clone's pre-cut fork point is not copied back into bravo's store.
func TestLandFetchedHeadKeepsAFollowTheLiveStoreRanDuringTheClonesFetch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReRootedRemote(t, ctx, true)
	cloneRoot := filepath.Join(t.TempDir(), "dolt")
	if err := dbsnapshot.CloneTree(ctx, r.bravo, cloneRoot); err != nil {
		t.Fatalf("CloneTree() error = %v", err)
	}
	clone := takenClone(t, ctx, cloneRoot)

	bravo := openSyncOrFatal(t, ctx, r.bravo)
	if _, err := bravo.SyncPull(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncPull(bravo) error = %v", err)
	}
	r.assertFollowed(t, bravo, storage.SyncEntryPull)
	if err := bravo.Close(); err != nil {
		t.Fatalf("Close(bravo) error = %v", err)
	}

	cloneStore := openSyncOrFatal(t, ctx, cloneRoot)
	if err := cloneStore.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	cloneForkPoint := branchesUnder(t, ctx, cloneStore, forkPointPrefix)["origin/master"]
	if err := cloneStore.Close(); err != nil {
		t.Fatalf("Close(clone) error = %v", err)
	}
	if cloneForkPoint == "" || cloneForkPoint == r.alphaReRoot.Head {
		t.Fatalf("the clone's fork point is %q; want the pre-cut head its snapshot shared with the remote", cloneForkPoint)
	}

	landed, err := LandFetchedHead(ctx, testEnv(), testStderr(), r.bravo, clone, remoteName(t, "origin"), "master", nil)
	if err != nil {
		t.Fatalf("LandFetchedHead() error = %v", err)
	}
	want := []storage.ForkPointRecord{{Remote: "origin", Branch: "master", Head: r.alphaReRoot.Head, Outcome: storage.ForkPointUnchanged}}
	if !slices.Equal(landed.ForkPoints, want) {
		t.Fatalf("LandedFetch.ForkPoints = %+v, want %+v", landed.ForkPoints, want)
	}
	bravo = openSyncOrFatal(t, ctx, r.bravo)
	defer func() { _ = bravo.Close() }()
	if forkPoint := branchesUnder(t, ctx, bravo, forkPointPrefix)["origin/master"]; forkPoint != r.alphaReRoot.Head {
		t.Fatalf("bravo's fork point after the landing is %s; want the re-rooted head %s the follow recorded", forkPoint, r.alphaReRoot.Head)
	}
	assertCollected(t, ctx, bravo, cloneForkPoint)
}

// TestRemoteCacheLandingCopiesTheFetchedRemotesLastKnownRefs drives the git
// half with plain git, for a clone that fetched origin: its mirror gained a
// last-known ref of origin, moved origin's other one onto an unrelated commit —
// a non-fast-forward, as a remote that was reset produces — holds another
// remote's last-known ref at the value it copied from the live mirror, which a
// live push has moved since, shares a fourth unchanged, and holds an instance
// ref the live mirror has since swept. The live mirror gains origin's new and
// moved refs with their objects, keeps the other remote's newer value and the
// unchanged ref, and the swept orphan stays swept; a mirror only the clone has
// is moved over whole.
func TestRemoteCacheLandingCopiesTheFetchedRemotesLastKnownRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	liveBase := filepath.Join(base, "live", dbsnapshot.RemoteCacheDirName)
	cloneBase := filepath.Join(base, "clone", dbsnapshot.RemoteCacheDirName)
	key := strings.Repeat("a", 64)
	onlyCloneKey := strings.Repeat("b", 64)
	git := func(gitDir string, args ...string) string {
		t.Helper()
		out, err := mirrorGit(context.Background(), testEnv(), gitDir, nil, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commitOn := func(gitDir, refName, message string) string {
		t.Helper()
		tree := git(gitDir, "mktree")
		out, err := mirrorGit(context.Background(), testEnv().With("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"), gitDir, nil,
			"commit-tree", tree, "-m", message).CombinedOutput()
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
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := mirrorGit(context.Background(), testEnv(), dir, nil, "init", "--bare", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", dir, err, out)
		}
	}
	const lastKnown = lastKnownRefNamespace + "origin/dolt/data"
	const added = lastKnownRefNamespace + "origin/dolt/other"
	const unchanged = lastKnownRefNamespace + "other/dolt/data"
	const unfetched = lastKnownRefNamespace + "upstream/dolt/data"
	orphan := trackingRefNamespace + "origin/dolt/data/" + uuid.NewString()
	commitOn(liveRepo, lastKnown, "where live last saw the remote")
	cloneLastKnown := commitOn(cloneRepo, lastKnown, "where the clone's fetch saw it")
	liveUnchanged := commitOn(liveRepo, unchanged, "both hold this")
	if out, err := mirrorFetch(context.Background(), testEnv(), cloneRepo, liveRepo, unchanged+":"+unchanged).CombinedOutput(); err != nil {
		t.Fatalf("fetch %s from the live mirror: %v\n%s", unchanged, err, out)
	}
	addedOID := commitOn(cloneRepo, added, "what the clone fetched")
	commitOn(cloneRepo, unfetched, "the value the clone copied from live")
	livePushed := commitOn(liveRepo, unfetched, "where a live push left upstream since")
	commitOn(cloneRepo, orphan, "an orphan the clone copied before the live sweep")

	plan := planRemoteCacheLanding(ctx, testEnv(), liveBase, cloneBase, remoteName(t, "origin"))
	if err := plan.apply(ctx, testEnv()); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if got := git(liveRepo, "rev-parse", added); got != addedOID {
		t.Fatalf("live mirror's %s = %s, want the clone's %s", added, got, addedOID)
	}
	if got := git(liveRepo, "cat-file", "-t", addedOID); got != "commit" {
		t.Fatalf("live mirror holds %s as %q, want its commit", addedOID, got)
	}
	if got := git(liveRepo, "rev-parse", lastKnown); got != cloneLastKnown {
		t.Fatalf("live mirror's %s = %s, want the clone's %s", lastKnown, got, cloneLastKnown)
	}
	if got := git(liveRepo, "rev-parse", unfetched); got != livePushed {
		t.Fatalf("live mirror's %s = %s, want the live push's %s kept: the clone never fetched that remote", unfetched, got, livePushed)
	}
	if got := git(liveRepo, "rev-parse", unchanged); got != liveUnchanged {
		t.Fatalf("live mirror's %s = %s, want it left at %s", unchanged, got, liveUnchanged)
	}
	if out, err := mirrorGit(context.Background(), testEnv(), liveRepo, nil, "rev-parse", "--verify", "--quiet", orphan).CombinedOutput(); err == nil {
		t.Fatalf("landing put the swept orphan %s back in the live mirror at %s", orphan, strings.TrimSpace(string(out)))
	}
	if len(plan.copies) != 1 || len(plan.copies[0].refspecs) != 2 {
		t.Fatalf("plan copies = %+v, want one mirror carrying exactly origin's two last-known refs that differ", plan.copies)
	}
	if _, err := os.Stat(filepath.Join(liveBase, onlyCloneKey, "repo.git")); err != nil {
		t.Fatalf("the mirror only the clone had was not moved into the live store: %v", err)
	}
}

// TestARemoteCachePlanReportsItsFailuresOnlyWhereItLands pins the plan's
// failures to the landing that carries it out: a landing that lands the fetch
// reports a mirror it could not plan, and a superseded one, which copies none
// of the plan, reports nothing of it.
func TestARemoteCachePlanReportsItsFailuresOnlyWhereItLands(t *testing.T) {
	unlisted := errors.New("list the clone's mirrors")
	plan := remoteCacheLanding{unplanned: unlisted}
	for _, tc := range []struct {
		record FetchedHeadRecord
		want   error
	}{
		{FetchedHeadMoved, unlisted},
		{FetchedHeadUnchanged, unlisted},
		{FetchedHeadAbsent, unlisted},
		{FetchedHeadSuperseded, nil},
	} {
		if err := plan.landedBy(tc.record).apply(context.Background(), testEnv()); !errors.Is(err, tc.want) {
			t.Errorf("a %s landing's apply() = %v, want %v", tc.record, err, tc.want)
		}
	}
}
