package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSnapshotCreatedOnEveryOpenAgainstExistingWorkspace verifies the basic
// contract: opening a workspace that already has content produces a
// filesystem snapshot in the snapshots/ sibling directory. Fresh-init opens
// don't snapshot (there's nothing meaningful to back up).
func TestSnapshotCreatedOnEveryOpenAgainstExistingWorkspace(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapsDir := snapshotsDirFor(doltRoot)

	// First Open: fresh init, no prior content, NO snapshot expected.
	first, err := Open(ctx, doltRoot, "fs-snap-first-id")
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	snaps, err := listFsSnapshots(snapsDir)
	if err != nil {
		t.Fatalf("listFsSnapshots after first Open: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected 0 snapshots after fresh init, got %d: %v", len(snaps), snaps)
	}

	// Second Open: workspace exists, snapshot MUST be taken.
	second, err := Open(ctx, doltRoot, "fs-snap-first-id")
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer second.Close()

	snaps, err = listFsSnapshots(snapsDir)
	if err != nil {
		t.Fatalf("listFsSnapshots after second Open: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot after second Open, got %d", len(snaps))
	}
	if !strings.HasPrefix(snaps[0].Name, fsSnapshotPrefix) {
		t.Errorf("snapshot name %q lacks expected prefix %q", snaps[0].Name, fsSnapshotPrefix)
	}
	// Snapshot must contain a real Dolt repo — verify by looking for the
	// `links/.dolt` path (Dolt's per-database internal storage; the
	// "links" segment is the fixed database name, see buildDoltDSN).
	if _, err := os.Stat(filepath.Join(snaps[0].Path, "links", ".dolt")); err != nil {
		t.Errorf("snapshot %s missing links/.dolt subdir: %v", snaps[0].Name, err)
	}
}

// TestSnapshotRetentionPrunesOldest verifies the prune budget: after more
// than fsSnapshotKeepN opens, only the newest N snapshots survive.
func TestSnapshotRetentionPrunesOldest(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapsDir := snapshotsDirFor(doltRoot)

	// Seed the workspace.
	first, err := Open(ctx, doltRoot, "fs-snap-retention-id")
	if err != nil {
		t.Fatalf("seed Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("seed Close() error = %v", err)
	}

	// Open enough times to exceed retention. Each Open takes one snapshot.
	const opens = fsSnapshotKeepN + 3
	for i := 0; i < opens; i++ {
		st, err := Open(ctx, doltRoot, "fs-snap-retention-id")
		if err != nil {
			t.Fatalf("Open %d error = %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close %d error = %v", i, err)
		}
	}

	snaps, err := listFsSnapshots(snapsDir)
	if err != nil {
		t.Fatalf("listFsSnapshots error = %v", err)
	}
	if len(snaps) != fsSnapshotKeepN {
		t.Fatalf("expected retention of %d snapshots, got %d: %v", fsSnapshotKeepN, len(snaps), snapNames(snaps))
	}
	// And they must be the NEWEST ones — list is sorted newest-first, so
	// timestamps strictly decrease.
	for i := 1; i < len(snaps); i++ {
		if !snaps[i-1].CreatedAt.After(snaps[i].CreatedAt) && !snaps[i-1].CreatedAt.Equal(snaps[i].CreatedAt) {
			t.Errorf("snapshot %d (%s) is newer than %d (%s); list not sorted newest-first",
				i, snaps[i].CreatedAt, i-1, snaps[i-1].CreatedAt)
		}
	}
}

// TestSnapshotRoundTripRestoresDataAfterCorruption is the disaster-recovery
// proof: open a workspace, write data, take a snapshot via Open, deliberately
// corrupt the dolt directory, restore from snapshot, verify the data is
// recovered. This is the strongest assertion the file-level protection
// actually does what it claims.
func TestSnapshotRoundTripRestoresDataAfterCorruption(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapsDir := snapshotsDirFor(doltRoot)

	// Seed: create the workspace and write a unique marker into meta.
	const markerKey = "fs_snapshot_test_marker"
	const markerValue = "data-must-survive-disaster"
	seed, err := Open(ctx, doltRoot, "fs-snap-roundtrip-id")
	if err != nil {
		t.Fatalf("seed Open() error = %v", err)
	}
	if _, err := seed.db.ExecContext(ctx,
		"INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)", markerKey, markerValue); err != nil {
		t.Fatalf("write marker error = %v", err)
	}
	if err := seed.commitWorkingSet(ctx, "seed marker"); err != nil {
		t.Fatalf("commit marker error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close() error = %v", err)
	}

	// Second Open takes a snapshot of the workspace containing the marker.
	provoke, err := Open(ctx, doltRoot, "fs-snap-roundtrip-id")
	if err != nil {
		t.Fatalf("provoke Open() error = %v", err)
	}
	if err := provoke.Close(); err != nil {
		t.Fatalf("provoke Close() error = %v", err)
	}

	snaps, err := listFsSnapshots(snapsDir)
	if err != nil {
		t.Fatalf("listFsSnapshots error = %v", err)
	}
	if len(snaps) == 0 {
		t.Fatal("no snapshot taken; cannot test restore")
	}

	// Corrupt the dolt repo: blow away the entire directory.
	if err := os.RemoveAll(doltRoot); err != nil {
		t.Fatalf("simulate corruption error = %v", err)
	}

	// Restore from the snapshot.
	if err := restoreFsSnapshot(snapsDir, doltRoot, snaps[0].Name); err != nil {
		t.Fatalf("restoreFsSnapshot error = %v", err)
	}

	// Re-open after restore. Marker must be present.
	t.Setenv("LIT_NO_FS_SNAPSHOT", "1") // skip snapshot of restored state for test cleanliness
	recovered, err := Open(ctx, doltRoot, "fs-snap-roundtrip-id")
	if err != nil {
		t.Fatalf("recovered Open() error = %v", err)
	}
	defer recovered.Close()

	var got string
	if err := recovered.db.QueryRowContext(ctx,
		"SELECT meta_value FROM meta WHERE meta_key = ?", markerKey).Scan(&got); err != nil {
		t.Fatalf("read marker after restore error = %v (data was lost)", err)
	}
	if got != markerValue {
		t.Fatalf("marker after restore = %q, want %q (data survived but mutated)", got, markerValue)
	}
}

// TestSnapshotsLiveOutsideDoltDir verifies the load-bearing layout
// invariant: snapshots are stored as a SIBLING of the dolt dir, not inside
// it. Otherwise blowing away the dolt dir would take the snapshots with it.
func TestSnapshotsLiveOutsideDoltDir(t *testing.T) {
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapsDir := snapshotsDirFor(doltRoot)

	doltAbs, err := filepath.Abs(doltRoot)
	if err != nil {
		t.Fatalf("abs dolt: %v", err)
	}
	snapsAbs, err := filepath.Abs(snapsDir)
	if err != nil {
		t.Fatalf("abs snaps: %v", err)
	}
	if strings.HasPrefix(snapsAbs, doltAbs+string(filepath.Separator)) {
		t.Fatalf("snapshots dir %q is inside dolt dir %q; protection is moot when the parent is corrupted", snapsAbs, doltAbs)
	}
}

// TestSnapshotSkipsOnDisableEnv verifies the LIT_NO_FS_SNAPSHOT escape hatch
// works for tests and CI hot paths that don't want snapshots accumulating.
func TestSnapshotSkipsOnDisableEnv(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snapsDir := snapshotsDirFor(doltRoot)

	// Seed.
	first, err := Open(ctx, doltRoot, "fs-snap-disable-id")
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	first.Close()

	t.Setenv("LIT_NO_FS_SNAPSHOT", "1")
	second, err := Open(ctx, doltRoot, "fs-snap-disable-id")
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	snaps, err := listFsSnapshots(snapsDir)
	if err != nil {
		t.Fatalf("listFsSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected 0 snapshots with LIT_NO_FS_SNAPSHOT=1, got %d: %v", len(snaps), snapNames(snaps))
	}
}

func snapNames(snaps []FsSnapshot) []string {
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.Name
	}
	return out
}
