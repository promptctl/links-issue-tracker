package store

import (
	"context"
	"path/filepath"
	"testing"
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
