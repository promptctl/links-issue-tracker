package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLocalHasTicketsDoesNotCreateStore proves the adopt gate can answer "is
// there a local backlog to lose?" for an absent or empty store WITHOUT creating
// it — the property that lets a fresh init clone straight into the target path.
func TestLocalHasTicketsDoesNotCreateStore(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "dolt")

	has, err := LocalHasTickets(ctx, root, "ws")
	if err != nil {
		t.Fatalf("LocalHasTickets(absent): %v", err)
	}
	if has {
		t.Fatalf("LocalHasTickets(absent) = true, want false")
	}
	if dirExists(filepath.Join(root, doltDatabaseName)) {
		t.Fatalf("LocalHasTickets created the store; it must only observe, never create")
	}

	if _, err := EnsureDatabase(ctx, root, "ws"); err != nil {
		t.Fatalf("EnsureDatabase: %v", err)
	}
	has, err = LocalHasTickets(ctx, root, "ws")
	if err != nil {
		t.Fatalf("LocalHasTickets(empty): %v", err)
	}
	if has {
		t.Fatalf("LocalHasTickets(empty) = true, want false")
	}
}

// TestAdoptRemoteByCloneBootstrapsAndReAdopts proves adopt-by-clone (1) bootstraps
// an absent store directly from the remote's backlog, and (2) re-adopting over
// an already-opened store still yields a readable backlog in the SAME process —
// the regression guard for dolt's in-process singleton chunk-store cache, which
// would otherwise serve the pre-adopt (stale) store after the directory is
// replaced. Uses a plain dolt file remote; the git-backed transport is covered
// end-to-end by the CLI init integration test.
func TestAdoptRemoteByCloneBootstrapsAndReAdopts(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	producer := filepath.Join(base, "producer")
	consumer := filepath.Join(base, "consumer")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, producer, remoteURL)

	if err := AdoptRemoteByClone(ctx, consumer, "ws", "origin", remoteURL, "master"); err != nil {
		t.Fatalf("AdoptRemoteByClone(first): %v", err)
	}
	assertHasIssueAfterAdopt(t, ctx, consumer, id)
	if has, err := LocalHasTickets(ctx, consumer, "ws"); err != nil || !has {
		t.Fatalf("LocalHasTickets(after adopt) = %v, %v; want true, nil", has, err)
	}

	// The consumer store has now been opened (and cached) in this process. A
	// second adopt must evict that cache so the re-cloned data is read fresh.
	if err := AdoptRemoteByClone(ctx, consumer, "ws", "origin", remoteURL, "master"); err != nil {
		t.Fatalf("AdoptRemoteByClone(re-adopt): %v", err)
	}
	assertHasIssueAfterAdopt(t, ctx, consumer, id)
}

func assertHasIssueAfterAdopt(t *testing.T, ctx context.Context, root, id string) {
	t.Helper()
	st, err := OpenForRead(ctx, root, "ws")
	if err != nil {
		t.Fatalf("OpenForRead(%s) after adopt: %v", root, err)
	}
	defer st.Close()
	if _, err := st.GetIssue(ctx, id); err != nil {
		t.Fatalf("GetIssue(%s) after adopt: %v (the adopted backlog is not readable)", id, err)
	}
}

// TestAdoptRemoteByCloneFailedCloneLeavesNoResidue pins AdoptRemoteByClone's
// two-state postcondition on the RETURNED-failure arm (links-sync-pgct.9): a
// clone that fails and returns leaves neither a partial database directory
// nor an adopt-pending marker, so the retry the error text asks for starts
// from a provably clean slate — and the retry itself succeeds.
func TestAdoptRemoteByCloneFailedCloneLeavesNoResidue(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	producer := filepath.Join(base, "producer")
	consumer := filepath.Join(base, "consumer")
	remoteURL := "file://" + filepath.Join(base, "remote")
	id := seedReconcileRemote(t, ctx, producer, remoteURL)

	if err := AdoptRemoteByClone(ctx, consumer, "ws", "origin", remoteURL, "branch-the-remote-does-not-have"); err == nil {
		t.Fatal("AdoptRemoteByClone(bad branch) = nil, want error")
	}
	if dirExists(filepath.Join(consumer, doltDatabaseName)) {
		t.Fatal("failed adopt left a partial database directory behind")
	}
	if _, statErr := os.Stat(AdoptPendingMarkerPath(consumer)); !os.IsNotExist(statErr) {
		t.Fatalf("failed adopt left the adopt-pending marker behind (stat = %v); a handled failure must clear it", statErr)
	}

	// The clean slate is not cosmetic: the retry adopts successfully.
	if err := AdoptRemoteByClone(ctx, consumer, "ws", "origin", remoteURL, "master"); err != nil {
		t.Fatalf("AdoptRemoteByClone(retry): %v", err)
	}
	if _, statErr := os.Stat(AdoptPendingMarkerPath(consumer)); !os.IsNotExist(statErr) {
		t.Fatalf("successful adopt left the adopt-pending marker behind (stat = %v)", statErr)
	}
	assertHasIssueAfterAdopt(t, ctx, consumer, id)
}

// TestAdoptRemoteByCloneHealsAbandonedAdoptResidue pins the NON-returning
// failure arm's recovery (links-sync-pgct.9): an adopt abandoned mid-clone
// (crash, SIGKILL, init's deadline abandoning the clone goroutine) leaves the
// durable marker plus whatever undefined partial state the clone had reached
// — fabricated here directly, since the whole point of the marker is that no
// in-process cleanup runs in that shape. The residue is deliberately
// UNOPENABLE junk: LocalHasTickets answering (false, nil) therefore proves it
// consumed the marker without opening the leftover, and the retried adopt
// discards it and re-clones to a working store with the marker gone.
func TestAdoptRemoteByCloneHealsAbandonedAdoptResidue(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	producer := filepath.Join(base, "producer")
	consumer := filepath.Join(base, "consumer")
	remoteURL := "file://" + filepath.Join(base, "remote")
	id := seedReconcileRemote(t, ctx, producer, remoteURL)

	junkDB := filepath.Join(consumer, doltDatabaseName)
	if err := os.MkdirAll(junkDB, 0o755); err != nil {
		t.Fatalf("MkdirAll(junk db): %v", err)
	}
	if err := os.WriteFile(filepath.Join(junkDB, "not-a-database"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("WriteFile(junk): %v", err)
	}
	// Garbage marker content: PRESENCE is the semantic, so an abandoned write
	// (or torn content) must condemn exactly like a well-formed marker.
	if err := os.WriteFile(AdoptPendingMarkerPath(consumer), []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile(marker): %v", err)
	}

	has, err := LocalHasTickets(ctx, consumer, "ws")
	if err != nil {
		t.Fatalf("LocalHasTickets(residue) = %v, want nil — the gate must consume the marker, not open the junk", err)
	}
	if has {
		t.Fatal("LocalHasTickets(residue) = true, want false: adopt residue is nothing-to-lose by construction")
	}

	if err := AdoptRemoteByClone(ctx, consumer, "ws", "origin", remoteURL, "master"); err != nil {
		t.Fatalf("AdoptRemoteByClone(over residue): %v", err)
	}
	if _, statErr := os.Stat(AdoptPendingMarkerPath(consumer)); !os.IsNotExist(statErr) {
		t.Fatalf("healing adopt left the adopt-pending marker behind (stat = %v)", statErr)
	}
	assertHasIssueAfterAdopt(t, ctx, consumer, id)
}

// TestPendingAdoptMarkerCondemnsEveryNormalOpen pins the refusal contract:
// with the marker present, every normal entry point — Open, OpenForRead,
// EnsureDatabase (which would otherwise CREATE-IF-NOT-EXISTS right over the
// residue), OpenSync, and DumpRaw — refuses loudly, naming the interrupted
// adopt and the `lit init` remedy, EVEN when the leftover happens to be a
// perfectly openable database. Removing the marker restores every open,
// proving the marker was the sole condemner.
func TestPendingAdoptMarkerCondemnsEveryNormalOpen(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "dolt")
	if _, err := EnsureDatabase(ctx, root, "ws"); err != nil {
		t.Fatalf("EnsureDatabase(setup): %v", err)
	}
	if err := writeAdoptPendingMarker(root, "origin", "master", time.Now()); err != nil {
		t.Fatalf("writeAdoptPendingMarker: %v", err)
	}

	assertCondemned := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s under adopt-pending marker = nil, want the interrupted-adopt refusal", name)
		}
		for _, want := range []string{"interrupted", "origin/master", "lit init"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s refusal = %q, want it to contain %q", name, err.Error(), want)
			}
		}
	}

	_, err := Open(ctx, root, "ws")
	assertCondemned("Open", err)
	_, err = OpenForRead(ctx, root, "ws")
	assertCondemned("OpenForRead", err)
	_, err = EnsureDatabase(ctx, root, "ws")
	assertCondemned("EnsureDatabase", err)
	_, err = OpenSync(ctx, root, "ws")
	assertCondemned("OpenSync", err)
	_, err = DumpRaw(ctx, root, "ws")
	assertCondemned("DumpRaw", err)

	if err := os.Remove(AdoptPendingMarkerPath(root)); err != nil {
		t.Fatalf("Remove(marker): %v", err)
	}
	st, err := OpenForRead(ctx, root, "ws")
	if err != nil {
		t.Fatalf("OpenForRead after clearing the marker: %v — the marker must be the sole condemner", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
