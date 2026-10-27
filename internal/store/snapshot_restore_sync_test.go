package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestASnapshotLeavesOutTheRemoteCacheAndStillSyncsOnceRestored: a recovery
// snapshot of a store that has pushed through a git remote holds neither the
// remote's mirror nor a temp table file a dead process left. Restored, the
// store receives a peer's push and pushes its own, so the mirror the snapshot
// left out is cloned again on demand rather than missed.
func TestASnapshotLeavesOutTheRemoteCacheAndStillSyncsOnceRestored(t *testing.T) {
	ctx := context.Background()
	origin := remoteName(t, "origin")
	live := pushedSyncStore(t, ctx)
	root := live.doltRootDir
	if err := live.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	cache := filepath.Join(root, doltDatabaseName, ".dolt", dbsnapshot.RemoteCacheDirName)
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the pushed store holds no remote cache to leave out: %v", err)
	}
	temp := TempTableFilesDir(root)
	if err := os.MkdirAll(temp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temp, "buffered_file_byte_sink_1"), []byte("left by a dead process"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The peer is a copy of the store as it stands, mirror and all, the way a
	// second checkout of the same remote holds one.
	peerRoot := cloneStoreTree(t, ctx, root, t.TempDir())

	snap, err := dbsnapshot.Take(ctx, testStderr(), root, "test")
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	for _, left := range []string{dbsnapshot.RemoteCacheDirName, dbsnapshot.TempTableFilesDirName} {
		path := filepath.Join(snap.Path, doltDatabaseName, ".dolt", left)
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the snapshot holds %s (stat err = %v); restoring it never needs it", left, err)
		}
	}

	writeAndPush := func(root, title string) {
		t.Helper()
		st, err := Open(ctx, testEnv(), testStderr(), root, "ws")
		if err != nil {
			t.Fatalf("Open(%s) error = %v", root, err)
		}
		commitPayload(t, ctx, st, title)
		if err := st.Close(); err != nil {
			t.Fatalf("Close() after %q error = %v", title, err)
		}
		sync := openSyncOrFatal(t, ctx, root)
		defer sync.Close()
		if _, err := sync.SyncCompactAndPush(ctx, ctx, origin, "master", unfiredBound(t), false, false); err != nil {
			t.Fatalf("SyncCompactAndPush(%q) error = %v", title, err)
		}
	}
	writeAndPush(peerRoot, "peer")

	if _, err := dbsnapshot.Restore(root, snap.Name); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	restored := openSyncOrFatal(t, ctx, root)
	received, err := restored.SyncReceive(ctx, origin, "master")
	if err != nil {
		t.Fatalf("SyncReceive() on the restored store error = %v", err)
	}
	if received.State != storage.SyncReceiveFastForwarded {
		t.Fatalf("receive on the restored store = %q, want %q: it must take the peer's push", received.State, storage.SyncReceiveFastForwarded)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the restored store's receive did not clone the remote's mirror again: %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	writeAndPush(root, "restored")

	peer := openSyncOrFatal(t, ctx, peerRoot)
	defer peer.Close()
	got, err := peer.SyncReceive(ctx, origin, "master")
	if err != nil {
		t.Fatalf("SyncReceive() on the peer error = %v", err)
	}
	if got.State != storage.SyncReceiveFastForwarded {
		t.Fatalf("receive on the peer = %q, want %q: the restored store's push must land", got.State, storage.SyncReceiveFastForwarded)
	}
}
