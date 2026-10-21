package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// forkBelowTheCut is two checkouts sharing a remote after one of them
// re-rooted it above every commit the other holds. delta adopted the remote,
// edited the ticket's title without pushing, and went quiet. alpha then wrote
// past the head delta adopted, cut its history at its own newest commit — one
// delta never saw, so delta holds no copy of the cut and cannot follow —
// force-pushed the re-rooted history, and pushed an edit to the same ticket's
// description on top of it.
type forkBelowTheCut struct {
	alpha, delta string
	remoteURL    string
	epic, ticket string
	// adopted is the head delta adopted: the remote head delta last knew.
	adopted string
	// remoteHead is the remote's head once alpha's edit landed.
	remoteHead  string
	alphaReRoot storage.ReRooted
}

const (
	deltaTitle            = "delta-title"
	alphaPastCutDescribed = "alpha-description-past-the-cut"
)

func newForkBelowTheCut(t *testing.T, ctx context.Context) forkBelowTheCut {
	t.Helper()
	return newForkBelowTheCutAfter(t, ctx, func(f forkBelowTheCut) {
		updateLocal(t, ctx, f.delta, f.ticket, storage.UpdateIssueInput{Title: strptr(deltaTitle)})
	})
}

// newForkBelowTheCutAfter is newForkBelowTheCut with deltaWrites in place of
// delta's title edit: what delta does between adopting the remote and alpha's
// re-root.
func newForkBelowTheCutAfter(t *testing.T, ctx context.Context, deltaWrites func(forkBelowTheCut)) forkBelowTheCut {
	t.Helper()
	f := forkBelowTheCut{alpha: migratedDoltDir(t), delta: migratedDoltDir(t), remoteURL: "file://" + filepath.Join(t.TempDir(), "remote")}
	f.epic = seedReconcileRemote(t, ctx, f.alpha, f.remoteURL)
	f.ticket = createAndPush(t, ctx, f.alpha, f.epic, "x")
	adoptRemote(t, ctx, f.delta, f.remoteURL)
	f.adopted = headAt(t, ctx, f.delta)
	deltaWrites(f)

	for _, title := range []string{"past-1", "past-2", "past-3"} {
		createAndPush(t, ctx, f.alpha, f.epic, title)
	}
	var err error
	f.alphaReRoot, err = ReRoot(ctx, f.alpha, "master", headAt(t, ctx, f.alpha))
	if err != nil {
		t.Fatalf("ReRoot(alpha) error = %v", err)
	}
	sync := openSyncOrFatal(t, ctx, f.alpha)
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, true); err != nil {
		t.Fatalf("force push of alpha's re-rooted history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(alpha force push) error = %v", err)
	}
	updateAndPush(t, ctx, f.alpha, f.ticket, storage.UpdateIssueInput{Description: strptr(alphaPastCutDescribed)})
	f.remoteHead = headAt(t, ctx, f.alpha)
	return f
}

// assertMerged checks the ticket on st carries both checkouts' edits.
func (f forkBelowTheCut) assertMerged(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	issue := getIssueOrFatal(t, ctx, st, f.ticket)
	if issue.Title != deltaTitle || issue.Description != alphaPastCutDescribed {
		t.Fatalf("ticket title, description = %q, %q; want delta's %q and alpha's %q", issue.Title, issue.Description, deltaTitle, alphaPastCutDescribed)
	}
}

// assertForkPointBase checks st's one reconcile merged against the fork
// point, which is the head delta adopted.
func (f forkBelowTheCut) assertForkPointBase(t *testing.T, st *Store) {
	t.Helper()
	want := storage.ReconcileBase{Source: storage.ReconcileBaseForkPoint, Commit: f.adopted, ForkPoint: f.adopted}
	if bases := st.ReconcileBases(); len(bases) != 1 || bases[0] != want {
		t.Fatalf("ReconcileBases() = %+v, want exactly %+v", bases, want)
	}
}

// TestAForkBelowTheCutMergesAgainstItsForkPoint is the ticket's sequence: a
// checkout whose unpushed edit forks below the remote's cut pulls, keeps both
// edits without meeting unrelated histories, pushes, and the checkout that
// re-rooted pulls the result.
func TestAForkBelowTheCutMergesAgainstItsForkPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCut(t, ctx)

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	unpushed := commitsBetween(t, ctx, delta, f.adopted, headCommit(t, ctx, delta))
	pulled, err := delta.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized || pulled.Unrelated != nil {
		t.Fatalf("pull state = %q (unrelated %+v), want %q", pulled.State, pulled.Unrelated, storage.SyncPullLinearized)
	}
	if followed := delta.FollowedReRoots(); len(followed) != 0 {
		t.Fatalf("FollowedReRoots() = %+v; delta holds no copy of the cut, so there is nothing to follow", followed)
	}
	records := delta.ForkPointRecords()
	if want := (storage.ForkPointRecord{Remote: "origin", Branch: "master", Head: f.adopted, Outcome: storage.ForkPointRecorded}); len(records) == 0 || records[0] != want {
		t.Fatalf("ForkPointRecords() = %+v, want the pull's fetch to record %+v first", records, want)
	}
	f.assertForkPointBase(t, delta)
	f.assertMerged(t, ctx, delta)
	assertLinearSpineToRemoteHead(t, ctx, delta, f.remoteHead)
	// The replay lands delta's own commits since the fork point and the marker,
	// and nothing of the history below the fork point.
	if replayed := commitsBetween(t, ctx, delta, f.remoteHead, headCommit(t, ctx, delta)); replayed != unpushed+1 {
		t.Fatalf("the merge put %d commits on the remote head; want %d: delta's %d since the fork point and the marker", replayed, unpushed+1, unpushed)
	}
	if _, err := delta.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("delta's push after the merge error = %v", err)
	}

	alpha := openSyncOrFatal(t, ctx, f.alpha)
	defer func() { _ = alpha.Close() }()
	back, err := alpha.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(alpha) error = %v", err)
	}
	if back.State != storage.SyncPullFastForwarded {
		t.Fatalf("alpha's pull = %q, want %q", back.State, storage.SyncPullFastForwarded)
	}
	f.assertMerged(t, ctx, alpha)
	if a, d := headCommit(t, ctx, alpha), headCommit(t, ctx, delta); a != d {
		t.Fatalf("heads after syncing both ways: alpha %s, delta %s", a, d)
	}
}

// TestAForkPointOutlivesTheFetchThatJumpedPastIt crashes delta between the
// fetch that brought the re-rooted remote and the reconcile: the next fetch
// finds the tracking ref unrelated and keeps the fork point, so the pull
// still merges against the head delta last knew.
func TestAForkPointOutlivesTheFetchThatJumpedPastIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCut(t, ctx)

	crashed := openSyncOrFatal(t, ctx, f.delta)
	if err := crashed.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatalf("Close(delta) error = %v", err)
	}

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	// Ahead is delta's one edit since the fork point, not the history below it
	// that the remote's cut dropped.
	fresh, err := delta.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness(delta) error = %v", err)
	}
	if fresh.Ahead != 1 || fresh.State() != storage.SyncDiverged || fresh.Base.Source != storage.ReconcileBaseForkPoint {
		t.Fatalf("freshness = ahead %d, %q, base %q; want ahead 1, %q, base %q", fresh.Ahead, fresh.State(), fresh.Base.Source, storage.SyncDiverged, storage.ReconcileBaseForkPoint)
	}
	pulled, err := delta.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized {
		t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullLinearized)
	}
	want := storage.ForkPointRecord{Remote: "origin", Branch: "master", Head: f.remoteHead, Outcome: storage.ForkPointKept}
	if records := delta.ForkPointRecords(); len(records) != 1 || records[0] != want {
		t.Fatalf("ForkPointRecords() = %+v, want the pull's fetch to keep the fork point: %+v", records, want)
	}
	f.assertForkPointBase(t, delta)
	f.assertMerged(t, ctx, delta)
}

// TestARemoteThatDropsTheForkPointsTicketsStaysUnrelated replaces the remote
// with a history that never held delta's tickets. The fork point is recorded,
// but the remote accounts for none of the tickets at it, so the pull reports
// unrelated histories exactly as it does with no fork point, and delta's
// backlog is untouched.
func TestARemoteThatDropsTheForkPointsTicketsStaysUnrelated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha, delta := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	ticket := createAndPush(t, ctx, alpha, epic, "x")
	adoptRemote(t, ctx, delta, remoteURL)
	adopted := headAt(t, ctx, delta)
	updateLocal(t, ctx, delta, ticket, storage.UpdateIssueInput{Title: strptr(deltaTitle)})

	other := unrelatedDoltDir(t)
	st, err := Open(ctx, other, "ws")
	if err != nil {
		t.Fatalf("Open(other) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "other", Topic: "topic", IssueType: "epic"}); err != nil {
		t.Fatalf("CreateIssue(other) error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(other) error = %v", err)
	}
	sync := openSyncOrFatal(t, ctx, other)
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(other) error = %v", err)
	}
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, true); err != nil {
		t.Fatalf("force push of the other history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(other sync) error = %v", err)
	}

	d := openSyncOrFatal(t, ctx, delta)
	defer func() { _ = d.Close() }()
	pulled, err := d.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullUnrelated {
		t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullUnrelated)
	}
	want := storage.ReconcileBase{Source: storage.ReconcileBaseNone, ForkPoint: adopted, UnaccountedTickets: 2, UnaccountedEvents: eventsAt(t, ctx, d, adopted)}
	if bases := d.ReconcileBases(); len(bases) != 1 || bases[0] != want {
		t.Fatalf("ReconcileBases() = %+v, want %+v: the epic, the ticket and every event at the fork point, none on the remote", bases, want)
	}
	if issue := getIssueOrFatal(t, ctx, d, ticket); issue.Title != deltaTitle {
		t.Fatalf("delta's ticket title = %q after the refused pull, want its own %q", issue.Title, deltaTitle)
	}
}

// TestARemoteMissingAnEditAtTheForkPointStaysUnrelated force-pushes a
// re-rooted history built by a checkout that never fetched the head delta
// adopted. Every ticket is still on the remote, but the title edit at the
// fork point is not, and a merge against the fork point would read that as
// the remote reverting it. So the pull reports unrelated histories and
// delta keeps the edit.
func TestARemoteMissingAnEditAtTheForkPointStaysUnrelated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha, bravo, delta := migratedDoltDir(t), migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	ticket := createAndPush(t, ctx, alpha, epic, "x")
	adoptRemote(t, ctx, bravo, remoteURL)
	const bravoTitle = "bravo-title"
	updateAndPush(t, ctx, bravo, ticket, storage.UpdateIssueInput{Title: strptr(bravoTitle)})
	adoptRemote(t, ctx, delta, remoteURL)
	adopted := headAt(t, ctx, delta)
	updateLocal(t, ctx, delta, ticket, storage.UpdateIssueInput{Description: strptr("delta-description")})

	updateLocal(t, ctx, alpha, ticket, storage.UpdateIssueInput{Description: strptr("alpha-description")})
	if _, err := ReRoot(ctx, alpha, "master", headAt(t, ctx, alpha)); err != nil {
		t.Fatalf("ReRoot(alpha) error = %v", err)
	}
	sync := openSyncOrFatal(t, ctx, alpha)
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, true); err != nil {
		t.Fatalf("force push of alpha's re-rooted history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(alpha force push) error = %v", err)
	}

	d := openSyncOrFatal(t, ctx, delta)
	defer func() { _ = d.Close() }()
	pulled, err := d.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullUnrelated {
		t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullUnrelated)
	}
	want := storage.ReconcileBase{Source: storage.ReconcileBaseNone, ForkPoint: adopted, UnaccountedEvents: 1}
	if bases := d.ReconcileBases(); len(bases) != 1 || bases[0] != want {
		t.Fatalf("ReconcileBases() = %+v, want %+v: bravo's edit is the one row at the fork point the remote lacks", bases, want)
	}
	if issue := getIssueOrFatal(t, ctx, d, ticket); issue.Title != bravoTitle {
		t.Fatalf("delta's ticket title = %q after the refused pull, want bravo's %q", issue.Title, bravoTitle)
	}
}

// TestAQuietForkBelowTheCutLandsOnTheRemoteHead is a checkout with nothing
// of its own past its fork point receiving a re-root it cannot follow. The
// remote holds every row it holds, so it is behind, and the receive lands it
// on the remote head itself: no reconcile, and no commit of its own on top.
func TestAQuietForkBelowTheCutLandsOnTheRemoteHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCutAfter(t, ctx, func(forkBelowTheCut) {})

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	if err := delta.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	fresh, err := delta.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness(delta) error = %v", err)
	}
	if fresh.Ahead != 0 || fresh.Behind == 0 || fresh.State() != storage.SyncBehind || fresh.Base.Source != storage.ReconcileBaseForkPoint || fresh.OldestDivergedUnix != 0 {
		t.Fatalf("freshness = ahead %d, behind %d, %q, base %q, oldest diverged %d; want ahead 0, behind > 0, %q, base %q, undated", fresh.Ahead, fresh.Behind, fresh.State(), fresh.Base.Source, fresh.OldestDivergedUnix, storage.SyncBehind, storage.ReconcileBaseForkPoint)
	}
	received, err := delta.SyncReceive(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReceive(delta) error = %v", err)
	}
	if received.State != storage.SyncReceiveFastForwarded || received.Base != storage.ReconcileBaseForkPoint {
		t.Fatalf("receive = %q across base %q, want %q across %q", received.State, received.Base, storage.SyncReceiveFastForwarded, storage.ReconcileBaseForkPoint)
	}
	if head := headCommit(t, ctx, delta); head != f.remoteHead {
		t.Fatalf("delta's head after the receive = %s, want the remote head %s", head, f.remoteHead)
	}
	if bases, followed := delta.ReconcileBases(), delta.FollowedReRoots(); len(bases) != 0 || len(followed) != 0 {
		t.Fatalf("ReconcileBases() = %+v, FollowedReRoots() = %+v; want neither: delta holds no copy of the cut and nothing to replay", bases, followed)
	}
	if issue := getIssueOrFatal(t, ctx, delta, f.ticket); issue.Description != alphaPastCutDescribed {
		t.Fatalf("ticket description = %q, want the remote's %q", issue.Description, alphaPastCutDescribed)
	}
}

// TestAQuietForkBelowTheCutPullsAcrossItsForkPoint is the same checkout
// pulling instead: the pull lands on the remote head with no merge commit,
// and its result says the base was the fork point, which a fast-forward along
// shared history never reports.
func TestAQuietForkBelowTheCutPullsAcrossItsForkPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCutAfter(t, ctx, func(forkBelowTheCut) {})

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	pulled, err := delta.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullFastForwarded || pulled.Base != storage.ReconcileBaseForkPoint {
		t.Fatalf("pull = %q across base %q, want %q across %q", pulled.State, pulled.Base, storage.SyncPullFastForwarded, storage.ReconcileBaseForkPoint)
	}
	if head := headCommit(t, ctx, delta); head != f.remoteHead {
		t.Fatalf("delta's head after the pull = %s, want the remote head %s", head, f.remoteHead)
	}
}

// TestAForkPointIsNoneWhenNothingWasShared fetches twice into a store whose
// history shares no commit with the remote's: the second fetch has a tracking
// ref to record and no history in common with it, and no fork point was ever
// recorded, so the record says there is none rather than one kept.
func TestAForkPointIsNoneWhenNothingWasShared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	seedReconcileRemote(t, ctx, migratedDoltDir(t), remoteURL)
	other := openSyncOrFatal(t, ctx, unrelatedDoltDir(t))
	defer func() { _ = other.Close() }()
	if err := other.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(other) error = %v", err)
	}
	for range 2 {
		if err := other.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
			t.Fatalf("SyncFetch(other) error = %v", err)
		}
	}
	tracked, err := commitHashOfRef(ctx, other.db, "remotes/origin/master")
	if err != nil {
		t.Fatalf("read the tracking ref: %v", err)
	}
	want := storage.ForkPointRecord{Remote: "origin", Branch: "master", Head: tracked, Outcome: storage.ForkPointNone}
	if records := other.ForkPointRecords(); len(records) != 1 || records[0] != want {
		t.Fatalf("ForkPointRecords() = %+v, want only the second fetch's %+v", records, want)
	}
}

// TestRemovingARemoteDropsItsForkPoints records a fork point, removes the
// remote, and finds the fork point deleted with the tracking ref it guarded,
// so nothing holds its commit's chunks through GC.
func TestRemovingARemoteDropsItsForkPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	seedReconcileRemote(t, ctx, migratedDoltDir(t), remoteURL)
	delta := migratedDoltDir(t)
	adoptRemote(t, ctx, delta, remoteURL)
	adopted := headAt(t, ctx, delta)

	st := openSyncOrFatal(t, ctx, delta)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	if err := st.SyncRemoveRemote(ctx, "origin"); err != nil {
		t.Fatalf("SyncRemoveRemote(delta) error = %v", err)
	}
	want := []storage.ForkPointRecord{
		{Remote: "origin", Branch: "master", Head: adopted, Outcome: storage.ForkPointRecorded},
		{Remote: "origin", Branch: "master", Head: adopted, Outcome: storage.ForkPointDropped},
	}
	if records := st.ForkPointRecords(); !slices.Equal(records, want) {
		t.Fatalf("ForkPointRecords() = %+v, want %+v", records, want)
	}
	left, err := refHeadsUnder(ctx, st.db, `SELECT name, hash FROM dolt_branches`, forkPointPrefix)
	if err != nil {
		t.Fatalf("list fork points: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("fork points after the remote's removal = %v, want none", left)
	}
}

// TestAPrunedTrackingRefTakesItsForkPointWithIt deletes a branch on the
// remote after delta recorded its fork point: the next fetch, which always
// prunes, drops the tracking ref, the fork point goes with it, and PrunedRefs
// says so.
func TestAPrunedTrackingRefTakesItsForkPointWithIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha := migratedDoltDir(t)
	seedReconcileRemote(t, ctx, alpha, remoteURL)
	pusher := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = pusher.Close() }()
	if err := execProcedureDiscard(ctx, pusher.db, "DOLT_BRANCH", "feature"); err != nil {
		t.Fatalf("create the feature branch: %v", err)
	}
	if err := execProcedureDiscard(ctx, pusher.db, "DOLT_PUSH", "origin", "feature"); err != nil {
		t.Fatalf("push the feature branch: %v", err)
	}
	delta := migratedDoltDir(t)
	adoptRemote(t, ctx, delta, remoteURL)
	st := openSyncOrFatal(t, ctx, delta)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	feature, err := commitHashOfRef(ctx, st.db, "remotes/origin/feature")
	if err != nil {
		t.Fatalf("read the feature tracking ref: %v", err)
	}
	if err := execProcedureDiscard(ctx, pusher.db, "DOLT_PUSH", "origin", ":feature"); err != nil {
		t.Fatalf("delete the remote's feature branch: %v", err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	records := st.ForkPointRecords()
	want := storage.ForkPointRecord{Remote: "origin", Branch: "feature", Head: feature, Outcome: storage.ForkPointDropped}
	if len(records) == 0 || records[len(records)-1] != want {
		t.Fatalf("ForkPointRecords() = %+v, want the pruning fetch to end with %+v", records, want)
	}
	wantPruned := []storage.PrunedRef{{Remote: "origin", Branch: "feature", Head: feature}}
	if pruned := st.PrunedRefs(); !slices.Equal(pruned, wantPruned) {
		t.Fatalf("PrunedRefs() = %+v, want %+v", pruned, wantPruned)
	}
	if _, err := commitHashOfRef(ctx, st.db, "remotes/origin/feature"); err == nil {
		t.Fatalf("remotes/origin/feature survived the fetch after the remote deleted feature")
	}
	left, err := refHeadsUnder(ctx, st.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(ownerOf(remoteName(t, "origin")), ""))
	if err != nil {
		t.Fatalf("list fork points: %v", err)
	}
	if _, ok := left["feature"]; ok || len(left) != 1 {
		t.Fatalf("fork points after the prune = %v, want master's alone", left)
	}
}

// TestARemotesNewURLKeepsItsForkPoint re-points delta's remote after the
// fetch that brought the re-rooted history, as a git remote whose URL changed
// does on every sync. The remote is the same remote, so its fork point stays
// and the pull still merges against it.
func TestARemotesNewURLKeepsItsForkPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCut(t, ctx)
	link := filepath.Join(t.TempDir(), "remote-link")
	if err := os.Symlink(strings.TrimPrefix(f.remoteURL, "file://"), link); err != nil {
		t.Fatalf("link the remote: %v", err)
	}

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	if err := delta.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	if err := delta.SyncSetRemoteURL(ctx, remoteName(t, "origin"), "file://"+link); err != nil {
		t.Fatalf("SyncSetRemoteURL(delta) error = %v", err)
	}
	pulled, err := delta.SyncPull(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized {
		t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullLinearized)
	}
	f.assertForkPointBase(t, delta)
	f.assertMerged(t, ctx, delta)
}

// TestASetRemoteURLRetriedAfterItsRemoveLandsTheAdd puts the store where an
// attempt whose remove landed and whose add failed leaves it, with no remote,
// and runs the re-point again, as the mutation's retry does.
func TestASetRemoteURLRetriedAfterItsRemoveLandsTheAdd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha := migratedDoltDir(t)
	seedReconcileRemote(t, ctx, alpha, remoteURL)
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if err := execProcedureDiscard(ctx, st.db, "DOLT_REMOTE", "remove", "origin"); err != nil {
		t.Fatalf("remove the remote: %v", err)
	}
	// Forward slashes, as Dolt lists a file URL back: on Windows a backslashed
	// path reads back file://D:/... and would not match its own setting.
	moved := "file://" + filepath.ToSlash(filepath.Join(t.TempDir(), "moved"))
	if err := st.SyncSetRemoteURL(ctx, remoteName(t, "origin"), moved); err != nil {
		t.Fatalf("SyncSetRemoteURL() error = %v", err)
	}
	remotes, err := st.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Name != "origin" || remotes[0].URL != moved {
		t.Fatalf("remotes = %+v, want origin at %s", remotes, moved)
	}
}

// TestAQuietForksRefusedPushIsNotSuperseded pushes from a checkout with
// nothing past its fork point. The remote does not carry its head, so the
// refusal is the error, even though there is nothing for it to replay, and it
// is the divergence a receive heals, not a failure.
func TestAQuietForksRefusedPushIsNotSuperseded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCutAfter(t, ctx, func(forkBelowTheCut) {})
	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	if err := delta.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	pushed, err := delta.SyncPushFromClone(ctx, ctx, remoteName(t, "origin"), "master", shippedBound(t), false, false)
	if !errors.Is(err, storage.ErrPushDiverged) {
		t.Fatalf("SyncPushFromClone() = %+v, %v; want the refusal as %v, since the remote does not carry delta's head", pushed, err, storage.ErrPushDiverged)
	}
}

// strandWrite edits the ticket's title on root with DOLT_COMMIT failing, so
// the write stays staged in the working set with no commit over it.
func strandWrite(t *testing.T, ctx context.Context, root, ticket string) {
	t.Helper()
	strandTitle(t, ctx, root, ticket, strandedTitle)
}

// strandTitle is strandWrite with the title to strand.
func strandTitle(t *testing.T, ctx context.Context, root, ticket, title string) {
	t.Helper()
	st := openSyncOrFatal(t, ctx, root)
	st.commitWorkingSetHookForTest = func() error { return errors.New("injected: DOLT_COMMIT failed") }
	if _, err := st.Apply(ctx, ticket, storage.Change{Fields: storage.UpdateIssueInput{Title: strptr(title)}}); err == nil {
		t.Fatalf("Apply() with DOLT_COMMIT failing returned nil; want the failure")
	}
	st.commitWorkingSetHookForTest = nil
	if err := st.Close(); err != nil {
		t.Fatalf("Close(stranded write) error = %v", err)
	}
}

const strandedTitle = "stranded-title"

// TestAStrandedWriteSurvivesAReceive leaves a write staged by a failed commit
// on a checkout whose remote moved, along shared history and below a cut. The
// receive versions the write before it decides, so it reads a divergence, not
// a landing that would discard the write, and the pull merges the write with
// the remote's edit.
func TestAStrandedWriteSurvivesAReceive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// setup returns the checkout holding the stranded write, the ticket,
		// and the description the remote gave it.
		setup func(t *testing.T, ctx context.Context) (root, ticket, remoteDescription string)
	}{
		{"shared base", func(t *testing.T, ctx context.Context) (string, string, string) {
			alpha, beta := migratedDoltDir(t), migratedDoltDir(t)
			remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
			ticket := createAndPush(t, ctx, alpha, seedReconcileRemote(t, ctx, alpha, remoteURL), "x")
			adoptRemote(t, ctx, beta, remoteURL)
			updateAndPush(t, ctx, alpha, ticket, storage.UpdateIssueInput{Description: strptr("alpha-description")})
			strandWrite(t, ctx, beta, ticket)
			return beta, ticket, "alpha-description"
		}},
		{"fork-point base", func(t *testing.T, ctx context.Context) (string, string, string) {
			f := newForkBelowTheCutAfter(t, ctx, func(f forkBelowTheCut) { strandWrite(t, ctx, f.delta, f.ticket) })
			return f.delta, f.ticket, alphaPastCutDescribed
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root, ticket, remoteDescription := tc.setup(t, ctx)
			st := openSyncOrFatal(t, ctx, root)
			defer func() { _ = st.Close() }()
			received, err := st.SyncReceive(ctx, remoteName(t, "origin"), "master")
			if err != nil {
				t.Fatalf("SyncReceive() error = %v", err)
			}
			if received.State != storage.SyncReceiveDiverged {
				t.Fatalf("receive = %q; want %q over the versioned write", received.State, storage.SyncReceiveDiverged)
			}
			assertStrandedWritesVersioned(t, ctx, st)
			if _, err := st.SyncPull(ctx, remoteName(t, "origin"), "master"); err != nil {
				t.Fatalf("SyncPull() error = %v", err)
			}
			if issue := getIssueOrFatal(t, ctx, st, ticket); issue.Title != strandedTitle || issue.Description != remoteDescription {
				t.Fatalf("ticket title, description = %q, %q; want the stranded %q and the remote's %q", issue.Title, issue.Description, strandedTitle, remoteDescription)
			}
			assertWorkingSetClean(t, ctx, st)
		})
	}
}

// TestAStrandedWriteSurvivesAReconcile leaves a write staged by a failed
// commit on a checkout that diverged from its remote, and reconciles without
// a receive first. The reconcile plan versions the write before it reads the
// divergence, so the replay carries it rather than the landing discarding it.
func TestAStrandedWriteSurvivesAReconcile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	alpha, beta := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	ticket := createAndPush(t, ctx, alpha, seedReconcileRemote(t, ctx, alpha, remoteURL), "x")
	adoptRemote(t, ctx, beta, remoteURL)
	updateAndPush(t, ctx, alpha, ticket, storage.UpdateIssueInput{Description: strptr("alpha-description")})
	updateLocal(t, ctx, beta, ticket, storage.UpdateIssueInput{Assignee: strptr("beta-assignee")})
	// One stranded write staged and a second unstaged over it, so each table
	// they touch holds a change on both sides of the index.
	strandTitle(t, ctx, beta, ticket, "staged-title")
	stageWorkingSet(t, ctx, beta)
	strandWrite(t, ctx, beta, ticket)
	st := openSyncOrFatal(t, ctx, beta)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch() error = %v", err)
	}
	if _, err := st.SyncReconcile(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncReconcile() error = %v", err)
	}
	if issue := getIssueOrFatal(t, ctx, st, ticket); issue.Title != strandedTitle || issue.Description != "alpha-description" || issue.Assignee != "beta-assignee" {
		t.Fatalf("ticket title, description, assignee = %q, %q, %q; want the stranded %q, alpha's description and beta's assignee", issue.Title, issue.Description, issue.Assignee, strandedTitle)
	}
	assertStrandedWritesVersioned(t, ctx, st)
	assertWorkingSetClean(t, ctx, st)
}

// stageWorkingSet stages every change in root's working set.
func stageWorkingSet(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	st := openSyncOrFatal(t, ctx, root)
	if err := execProcedureDiscard(ctx, st.db, "DOLT_ADD", "-A"); err != nil {
		t.Fatalf("DOLT_ADD -A: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(staged) error = %v", err)
	}
}

// assertStrandedWritesVersioned asserts the store landed exactly one commit
// over stranded writes, counting the tables that commit's own diff touched.
func assertStrandedWritesVersioned(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var tables int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT table_name) FROM dolt_diff WHERE commit_hash = (SELECT commit_hash FROM dolt_log WHERE message = ? LIMIT 1)`,
		strandedWritesCommitMessage,
	).Scan(&tables); err != nil {
		t.Fatalf("count the stranded commit's tables: %v", err)
	}
	want := []storage.StrandedWritesCommit{{Tables: tables}}
	if got := st.StrandedWritesCommits(); tables == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("StrandedWritesCommits() = %+v; want %+v, the tables the stranded commit's diff touched", got, want)
	}
}

// TestAnEventOfNoBaseTicketIsNotCounted counts over rows already read: an
// event at the base whose ticket the base does not hold records no edit a
// merge against the base could revert, so the remote need not hold it.
func TestAnEventOfNoBaseTicketIsNotCounted(t *testing.T) {
	ticket := model.Issue{ID: "t", CreatedAt: time.Unix(1, 0)}
	base := map[string]model.Issue{"t": ticket}
	baseEvents := map[string]string{"e-t": "t", "e-orphan": "gone"}
	tickets, events := countUnaccounted(base, base, baseEvents, map[string]string{}, nil)
	if tickets != 0 || events != 1 {
		t.Fatalf("countUnaccounted() = %d tickets, %d events; want 0 and 1: the ticket's event alone", tickets, events)
	}
}

// TestTheReceiveRecordsTheForkPointOnTheLiveStore drives the automatic
// receive's shape: a clone of delta fetches the re-rooted remote, and the
// landing records delta's fork point on delta ahead of the tracking ref, by
// the fetch's rule. The first landing records it at the head delta adopted,
// which delta's branch shares history with; the second finds the tracking
// ref on the re-rooted history, which delta's branch shares nothing with,
// and keeps it. Delta's own settle and reconcile then merge against it.
func TestTheReceiveRecordsTheForkPointOnTheLiveStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForkBelowTheCut(t, ctx)

	cloneRoot := filepath.Join(t.TempDir(), "clone", "dolt")
	if err := dbsnapshot.CloneTree(ctx, f.delta, cloneRoot); err != nil {
		t.Fatalf("CloneTree(delta) error = %v", err)
	}
	clone := takenClone(t, ctx, cloneRoot)
	fetcher := openSyncOrFatal(t, ctx, cloneRoot)
	if err := fetcher.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(clone) error = %v", err)
	}
	if err := fetcher.Close(); err != nil {
		t.Fatalf("Close(clone) error = %v", err)
	}
	for _, want := range []struct {
		ref       FetchedHeadRecord
		forkPoint storage.ForkPointRecord
	}{
		{FetchedHeadMoved, storage.ForkPointRecord{Remote: "origin", Branch: "master", Head: f.adopted, Outcome: storage.ForkPointRecorded}},
		{FetchedHeadUnchanged, storage.ForkPointRecord{Remote: "origin", Branch: "master", Head: f.remoteHead, Outcome: storage.ForkPointKept}},
	} {
		landed, err := LandFetchedHead(ctx, f.delta, clone, remoteName(t, "origin"), "master", nil)
		if err != nil {
			t.Fatalf("LandFetchedHead() error = %v", err)
		}
		if landed.Record != want.ref || !slices.Equal(landed.ForkPoints, []storage.ForkPointRecord{want.forkPoint}) {
			t.Fatalf("LandFetchedHead() landed the ref %s and the fork point %+v; want %s and %+v", landed.Record, landed.ForkPoints, want.ref, want.forkPoint)
		}
	}

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	received, err := delta.SyncSettleReceived(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncSettleReceived(delta) error = %v", err)
	}
	if received.State != storage.SyncReceiveDiverged {
		t.Fatalf("settle = %q, want %q", received.State, storage.SyncReceiveDiverged)
	}
	reconciled, err := delta.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(delta) error = %v", err)
	}
	if reconciled.State != storage.SyncReconcileLinearized || reconciled.BaseCommit != f.adopted {
		t.Fatalf("reconcile = %q against %q, want %q against the fork point %s", reconciled.State, reconciled.BaseCommit, storage.SyncReconcileLinearized, f.adopted)
	}
	f.assertForkPointBase(t, delta)
	f.assertMerged(t, ctx, delta)
}

// createAndPush files a ticket under epic at root and pushes it, returning
// its id.
func createAndPush(t *testing.T, ctx context.Context, root, epic, title string) string {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(%s) error = %v", root, err)
	}
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", ParentID: epic})
	if err != nil {
		t.Fatalf("CreateIssue(%s) error = %v", title, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(%s) error = %v", root, err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(%s) error = %v", title, err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(%s sync) error = %v", root, err)
	}
	return issue.ID
}

// commitsBetween counts the commits head reaches that base does not.
func commitsBetween(t *testing.T, ctx context.Context, st *Store, base, head string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log(?, ?)`, head, "^"+base).Scan(&n); err != nil {
		t.Fatalf("count commits from %s to %s: %v", base, head, err)
	}
	return n
}

// eventsAt counts the ticket events at commit, read straight off the table.
func eventsAt(t *testing.T, ctx context.Context, st *Store, commit string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events AS OF '`+commit+`'`).Scan(&n); err != nil {
		t.Fatalf("count the events at %s: %v", commit, err)
	}
	return n
}

// headAt is the head commit of the store at root.
func headAt(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	st := openSyncOrFatal(t, ctx, root)
	defer func() { _ = st.Close() }()
	return headCommit(t, ctx, st)
}
