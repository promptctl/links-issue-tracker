package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A forced push replaces only the head it judged. The consumer fetched the
// remote at this tree's schema; the producer then lowered and held it; the
// consumer forces without fetching. The verdicts it passed were judged against
// the stale tracking head, so the lease on that head is what keeps the force
// from re-raising the held remote: the push is refused naming the head it
// leased on, and the remote keeps its held head.
func TestAForcedPushOverARemoteThatMovedSinceTheFetchIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")
	max := registryMax(t)

	seed := openSyncOrFatal(t, ctx, producer)
	if err := seed.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed) = %v", err)
	}
	if _, err := seed.SyncPush(ctx, origin, "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed) = %v", err)
	}
	fetchedHead := headCommit(t, ctx, seed)

	st := openSyncOrFatal(t, ctx, consumer)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) = %v", err)
	}
	if err := st.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch(consumer) = %v", err)
	}

	// The remote moves after the consumer's fetch: lowered one schema and held
	// there, the lowering the hold exists to make stick.
	if _, err := seed.DowngradeAndHold(ctx, max-1); err != nil {
		t.Fatalf("DowngradeAndHold(%d) = %v", max-1, err)
	}
	if _, err := seed.SyncPush(ctx, origin, "master", false, false); err != nil {
		t.Fatalf("SyncPush(held lowering) = %v", err)
	}
	heldHead := headCommit(t, ctx, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("Close(seed) = %v", err)
	}

	_, err := st.SyncPush(ctx, origin, "master", false, true)
	var lost storage.ForcedPushLeaseError
	if !errors.As(err, &lost) {
		t.Fatalf("forced SyncPush without fetching = %v, want a storage.ForcedPushLeaseError", err)
	}
	if want := (storage.ForcedPushLeaseError{Remote: origin, Branch: "master", Expected: fetchedHead}); lost != want {
		t.Errorf("refusal = %+v, want %+v", lost, want)
	}
	if errors.As(err, new(pushRefusedError)) || errors.Is(err, storage.ErrPushDiverged) {
		t.Errorf("the lost lease is judged as a refusal on history: %v", err)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused force = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != heldHead {
		t.Fatalf("remote head after the refused force = %s, want %s untouched", got, heldHead)
	}
}

// A forced push to a branch this checkout holds no head of has nothing a
// force may replace: it is refused before it sends anything, with no head
// leased on, and the remote keeps its head.
func TestAForcedPushWithNoTrackingRefSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")

	seed := openSyncOrFatal(t, ctx, producer)
	if err := seed.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed) = %v", err)
	}
	if _, err := seed.SyncPush(ctx, origin, "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed) = %v", err)
	}
	remoteHead := headCommit(t, ctx, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("Close(seed) = %v", err)
	}

	st := openSyncOrFatal(t, ctx, consumer)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) = %v", err)
	}
	_, err := st.SyncPush(ctx, origin, "master", false, true)
	var lost storage.ForcedPushLeaseError
	if !errors.As(err, &lost) {
		t.Fatalf("forced SyncPush with no tracking ref = %v, want a storage.ForcedPushLeaseError", err)
	}
	if want := (storage.ForcedPushLeaseError{Remote: origin, Branch: "master"}); lost != want {
		t.Errorf("refusal = %+v, want %+v", lost, want)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused force = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != remoteHead {
		t.Fatalf("remote head after the refused force = %s, want %s untouched", got, remoteHead)
	}
}
