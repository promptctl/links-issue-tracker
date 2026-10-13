package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestSyncResolveUnrelatedTakeLocalKeepsTheRemotesPurge pins that a take
// discards the other side's work but never its purges. The remote purged a
// ticket the local side also holds (the two histories share it the way a
// lifeboat restore shares it); taking local must not hand that ticket back to
// the remote, and the remote's record must ride the push.
func TestSyncResolveUnrelatedTakeLocalKeepsTheRemotesPurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")

	sharedID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	localEpicID, localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)
	shareIssue(t, ctx, rootA, rootB, sharedID)
	purgeAndPush(t, ctx, rootA, sharedID)

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	token := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", storage.TakeLocal)
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", storage.TakeLocal, token)
	if err != nil {
		t.Fatalf("SyncResolveUnrelated(TakeLocal): %v", err)
	}
	// The inventory the owner approved names the shared ticket as purged, not
	// as local work the take keeps.
	assertIDSet(t, "purged", res.Unrelated.Purged, []string{sharedID})
	assertIDSet(t, "only-local (kept)", res.Unrelated.OnlyLocal, []string{localEpicID, localIssueID})
	assertLocalIssueIDs(t, ctx, syncB, []string{localEpicID, localIssueID})
	assertPurged(t, ctx, syncB, sharedID)

	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(B) after take-local: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	if _, err := syncA.SyncReceive(ctx, "origin", "master"); err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	assertLocalIssueIDs(t, ctx, syncA, []string{localEpicID, localIssueID})
	assertPurged(t, ctx, syncA, sharedID)
}

// TestSyncResolveUnrelatedTakeRemoteCarriesTheLocalPurge is the mirror: local
// purged a ticket the remote still holds. Taking remote adopts the remote's
// backlog less that ticket, keeps the local record, and lands it as one commit
// the next push carries, so the remote drops the ticket too.
func TestSyncResolveUnrelatedTakeRemoteCarriesTheLocalPurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")

	sharedID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	forkUnrelatedClone(t, ctx, rootB, remoteURL)
	shareIssue(t, ctx, rootA, rootB, sharedID)
	purgeIssue(t, ctx, rootB, sharedID)

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	token := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", storage.TakeRemote)
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", storage.TakeRemote, token)
	if err != nil {
		t.Fatalf("SyncResolveUnrelated(TakeRemote): %v", err)
	}
	assertIDSet(t, "purged", res.Unrelated.Purged, []string{sharedID})
	assertIDSet(t, "only-remote (kept)", res.Unrelated.OnlyRemote, nil)
	if res.CarriedPurges != 1 {
		t.Errorf("CarriedPurges = %d, want 1", res.CarriedPurges)
	}
	assertLocalIssueIDs(t, ctx, syncB, nil)
	assertPurged(t, ctx, syncB, sharedID)
	assertWorkingSetClean(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != storage.SyncAhead || fresh.Ahead != 1 {
		t.Fatalf("post-take-remote freshness = %q ahead=%d, want ahead by the one commit carrying the purge", fresh.State(), fresh.Ahead)
	}

	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(B) after take-remote: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	if _, err := syncA.SyncReceive(ctx, "origin", "master"); err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	assertLocalIssueIDs(t, ctx, syncA, nil)
	assertPurged(t, ctx, syncA, sharedID)
}

// shareIssue copies one issue row, birth and all, from one store into another
// whose history is unrelated, the way a restore from the first store's export
// would: afterwards both histories hold the same ticket.
func shareIssue(t *testing.T, ctx context.Context, fromRoot, toRoot, id string) {
	t.Helper()
	from := openOrFatal(t, ctx, fromRoot)
	source, err := from.Export(ctx)
	if err != nil {
		t.Fatalf("Export(from): %v", err)
	}
	if err := from.Close(); err != nil {
		t.Fatalf("Close(from): %v", err)
	}
	to := openOrFatal(t, ctx, toRoot)
	defer func() { _ = to.Close() }()
	target, err := to.Export(ctx)
	if err != nil {
		t.Fatalf("Export(to): %v", err)
	}
	target.Issues = append(target.Issues, issueByID(t, source, id))
	if _, err := to.ReplaceFromExport(ctx, target); err != nil {
		t.Fatalf("ReplaceFromExport(to): %v", err)
	}
}

func purgeIssue(t *testing.T, ctx context.Context, root, id string) {
	t.Helper()
	st := openOrFatal(t, ctx, root)
	defer func() { _ = st.Close() }()
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: id, Actor: "tester"}); err != nil {
		t.Fatalf("Purge(%s): %v", id, err)
	}
}

func purgeAndPush(t *testing.T, ctx context.Context, root, id string) {
	t.Helper()
	purgeIssue(t, ctx, root, id)
	sync := openSyncOrFatal(t, ctx, root)
	defer func() { _ = sync.Close() }()
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(after purge): %v", err)
	}
}

// assertPurged fails unless the store holds exactly one purge record and it
// names id.
func assertPurged(t *testing.T, ctx context.Context, st *Store, id string) {
	t.Helper()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(export.Purges) != 1 || export.Purges[0].IssueID != id {
		t.Fatalf("purge records = %+v, want exactly the record of %s", export.Purges, id)
	}
}

func openOrFatal(t *testing.T, ctx context.Context, root string) *Store {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(%s): %v", root, err)
	}
	return st
}

// TestSyncResolveUnrelatedTakeRemoteAdoptsAPrePurgeRemote pins take-remote
// against a remote last written below the migration that adds the purges
// table, the shape every remote a released lit pushed has. With nothing to
// carry the take is the plain reset and adopts that head as before; with a
// local purge to carry it lifts the head on scratch and lands the record, and
// the remote drops the ticket once the push lands.
func TestSyncResolveUnrelatedTakeRemoteAdoptsAPrePurgeRemote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		purge bool
	}{{"nothing to carry", false}, {"a purge to carry", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rootA := migratedDoltDir(t)
			rootB := unrelatedDoltDir(t)
			remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")

			sharedID := seedPrePurgeRemote(t, ctx, rootA, remoteURL)
			forkUnrelatedClone(t, ctx, rootB, remoteURL)
			shareIssue(t, ctx, rootA, rootB, sharedID)
			want := []string{sharedID}
			if tc.purge {
				purgeIssue(t, ctx, rootB, sharedID)
				want = nil
			}

			syncB := openSyncOrFatal(t, ctx, rootB)
			if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
				t.Fatalf("SyncFetch(B): %v", err)
			}
			token := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", storage.TakeRemote)
			res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", storage.TakeRemote, token)
			if err != nil {
				t.Fatalf("SyncResolveUnrelated(TakeRemote) onto a pre-purge remote: %v", err)
			}
			if res.State != storage.SyncReconcileTookRemote {
				t.Fatalf("state = %q, want %q", res.State, storage.SyncReconcileTookRemote)
			}
			if want := map[bool]int{false: 0, true: 1}[tc.purge]; res.CarriedPurges != want {
				t.Errorf("CarriedPurges = %d, want %d", res.CarriedPurges, want)
			}
			assertScratchBranchCleanedUp(t, ctx, syncB)
			fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
			if err != nil {
				t.Fatalf("SyncFreshness(B): %v", err)
			}
			wantState := map[bool]storage.SyncFreshnessState{false: storage.SyncUpToDate, true: storage.SyncAhead}[tc.purge]
			if fresh.State() != wantState {
				t.Fatalf("freshness = %q ahead=%d, want %q: the take writes only when it carries a purge", fresh.State(), fresh.Ahead, wantState)
			}
			if tc.purge {
				if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
					t.Fatalf("SyncPush(B): %v", err)
				}
			}
			// A plain take-remote leaves this session on the adopted head's own
			// schema; the next open lifts it, so the content is read from one.
			if err := syncB.Close(); err != nil {
				t.Fatalf("Close(B): %v", err)
			}
			reopened := openOrFatal(t, ctx, rootB)
			defer func() { _ = reopened.Close() }()
			assertLocalIssueIDs(t, ctx, reopened, want)
			if tc.purge {
				assertPurged(t, ctx, reopened, sharedID)
			}
		})
	}
}

// seedPrePurgeRemote is seedReconcileRemote with the seed store stepped back
// below the purges migration before its first push, so the remote head is the
// shape a released lit writes.
func seedPrePurgeRemote(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	st := openOrFatal(t, ctx, root)
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "seed", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
	}
	if err := st.Downgrade(ctx, 8); err != nil {
		t.Fatalf("Downgrade(seed) to v8: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	defer func() { _ = sync.Close() }()
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed): %v", err)
	}
	if _, err := sync.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed): %v", err)
	}
	return epic.ID
}
