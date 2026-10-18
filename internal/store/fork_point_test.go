package store

import (
	"context"
	"path/filepath"
	"testing"

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
	f := forkBelowTheCut{alpha: migratedDoltDir(t), delta: migratedDoltDir(t), remoteURL: "file://" + filepath.Join(t.TempDir(), "remote")}
	f.epic = seedReconcileRemote(t, ctx, f.alpha, f.remoteURL)
	f.ticket = createAndPush(t, ctx, f.alpha, f.epic, "x")
	adoptRemote(t, ctx, f.delta, f.remoteURL)
	f.adopted = headAt(t, ctx, f.delta)
	updateLocal(t, ctx, f.delta, f.ticket, storage.UpdateIssueInput{Title: strptr(deltaTitle)})

	for _, title := range []string{"past-1", "past-2", "past-3"} {
		createAndPush(t, ctx, f.alpha, f.epic, title)
	}
	var err error
	f.alphaReRoot, err = ReRoot(ctx, f.alpha, "master", headAt(t, ctx, f.alpha))
	if err != nil {
		t.Fatalf("ReRoot(alpha) error = %v", err)
	}
	sync := openSyncOrFatal(t, ctx, f.alpha)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, true); err != nil {
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
	pulled, err := delta.SyncPull(ctx, "origin", "master")
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
	if _, err := delta.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("delta's push after the merge error = %v", err)
	}

	alpha := openSyncOrFatal(t, ctx, f.alpha)
	defer func() { _ = alpha.Close() }()
	back, err := alpha.SyncPull(ctx, "origin", "master")
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
	if err := crashed.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(delta) error = %v", err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatalf("Close(delta) error = %v", err)
	}

	delta := openSyncOrFatal(t, ctx, f.delta)
	defer func() { _ = delta.Close() }()
	pulled, err := delta.SyncPull(ctx, "origin", "master")
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
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(other) error = %v", err)
	}
	if _, err := sync.SyncPush(ctx, "origin", "master", true, true); err != nil {
		t.Fatalf("force push of the other history error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(other sync) error = %v", err)
	}

	d := openSyncOrFatal(t, ctx, delta)
	defer func() { _ = d.Close() }()
	pulled, err := d.SyncPull(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncPull(delta) error = %v", err)
	}
	if pulled.State != storage.SyncPullUnrelated {
		t.Fatalf("pull state = %q, want %q", pulled.State, storage.SyncPullUnrelated)
	}
	want := storage.ReconcileBase{Source: storage.ReconcileBaseNone, ForkPoint: adopted, UnaccountedTickets: 2}
	if bases := d.ReconcileBases(); len(bases) != 1 || bases[0] != want {
		t.Fatalf("ReconcileBases() = %+v, want %+v: the epic and the ticket at the fork point, neither on the remote", bases, want)
	}
	if issue := getIssueOrFatal(t, ctx, d, ticket); issue.Title != deltaTitle {
		t.Fatalf("delta's ticket title = %q after the refused pull, want its own %q", issue.Title, deltaTitle)
	}
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
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(%s) error = %v", title, err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(%s sync) error = %v", root, err)
	}
	return issue.ID
}

// headAt is the head commit of the store at root.
func headAt(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	st := openSyncOrFatal(t, ctx, root)
	defer func() { _ = st.Close() }()
	return headCommit(t, ctx, st)
}
