package dbsnapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/filelock"
)

// fabricateDeadResidue plants the exact on-disk shape an uncleanly killed
// Take leaves behind: a partially-written .tmp tree and its .reserve claim,
// with no live producer holding the beacon.
func fabricateDeadResidue(t *testing.T, snapshotsDir, stamp string) (tmpPath, reservePath string) {
	t.Helper()
	tmpPath = filepath.Join(snapshotsDir, stamp+".tmp")
	reservePath = filepath.Join(snapshotsDir, stamp+".reserve")
	if err := os.MkdirAll(filepath.Join(tmpPath, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpPath, "nested", "partial"), []byte("half-copied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reservePath, 0o755); err != nil {
		t.Fatal(err)
	}
	return tmpPath, reservePath
}

// TestCollectOrphanedResidue_RemovesDeadResidue pins the ticket's acceptance
// shape (links-snapshots-3dtv): residue stranded by a killed producer — the
// .tmp copy, the .reserve claim, and any .condemned corpse from an earlier
// interrupted collection — is reclaimed, while real snapshots, legacy
// directories, and the beacon itself are untouched.
func TestCollectOrphanedResidue_RemovesDeadResidue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	snap, err := Take(context.Background(), src, snapshotsDir, "keep-me")
	if err != nil {
		t.Fatal(err)
	}
	fabricateDeadResidue(t, snapshotsDir, "1700000000000000001")
	condemnedLeftover := filepath.Join(snapshotsDir, "1700000000000000002.tmp.condemned")
	if err := os.MkdirAll(condemnedLeftover, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(snapshotsDir, "snap-old-junk")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("CollectOrphanedResidue: %v", err)
	}

	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if isProducerArtifactName(e.Name()) {
			t.Errorf("residue survived collection: %s", e.Name())
		}
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Errorf("real snapshot was collected: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy directory was collected: %v", err)
	}
}

// TestCollectOrphanedResidue_SkipsWhileProducerLive pins the liveness proof:
// while any producer holds the beacon shared (the shape of a live mid-copy
// Take), the exclusive probe reports contention and collection defers —
// residue is untouched, because indistinguishable residue could be that
// producer's own in-flight paths. After the holder releases, the same call
// collects. No ages, no PIDs: the discriminator is the kernel's.
func TestCollectOrphanedResidue_SkipsWhileProducerLive(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	tmpPath, reservePath := fabricateDeadResidue(t, snapshotsDir, "1700000000000000003")

	release, acquired, err := filelock.Acquire(context.Background(), producerBeaconPath(snapshotsDir), false, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("simulated producer hold: acquired=%v err=%v", acquired, err)
	}

	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("collection while producer live must skip cleanly, got %v", err)
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf(".tmp of a live producer was touched: %v", err)
	}
	if _, err := os.Stat(reservePath); err != nil {
		t.Fatalf(".reserve of a live producer was touched: %v", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release simulated producer hold: %v", err)
	}

	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("collection after producer death: %v", err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".tmp must be collected once the producer is gone, stat err=%v", err)
	}
	if _, err := os.Stat(reservePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".reserve must be collected once the producer is gone, stat err=%v", err)
	}
}

// TestCollectOrphanedResidue_MissingDirIsNoop pins that a workspace which has
// never snapshotted stays untouched — no directory, no beacon file, no error.
func TestCollectOrphanedResidue_MissingDirIsNoop(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "never-created")
	if err := CollectOrphanedResidue(dir); err != nil {
		t.Fatalf("missing dir must be a noop, got %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collection must not create the snapshots dir, stat err=%v", err)
	}
}

// TestPruneMatching_CollectsResidue pins the wiring: every producer's
// existing retention tail (PruneMatching) is the boundary residue collection
// rides, so an interrupted take's leftovers are reclaimed by the very next
// take/prune cycle with no caller changes.
func TestPruneMatching_CollectsResidue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	for i := 0; i < 3; i++ {
		if _, err := Take(context.Background(), src, snapshotsDir, ""); err != nil {
			t.Fatal(err)
		}
	}
	tmpPath, reservePath := fabricateDeadResidue(t, snapshotsDir, "1700000000000000004")

	if err := PruneMatching(snapshotsDir, 2, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prune must collect .tmp residue, stat err=%v", err)
	}
	if _, err := os.Stat(reservePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prune must collect .reserve residue, stat err=%v", err)
	}
	list, err := List(snapshotsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("retention budget must still apply: len=%d want 2", len(list))
	}
}

// TestTake_CanceledContextRefusesCleanly pins the uniform entry gate: a
// pre-canceled ctx refuses on every platform — including Darwin, whose
// Clonefile fast path would otherwise mint the snapshot in one syscall — and
// leaves no artifact of any kind on disk.
func TestTake_CanceledContextRefusesCleanly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Take(ctx, src, snapshotsDir, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, statErr := os.Stat(snapshotsDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused Take must create nothing, stat err=%v", statErr)
	}
}

// TestWalkAndCopy_CancelMidWalkStopsAndSurfacesCancellation pins the per-entry
// gate that makes a long fallback copy interruptible: cancellation raised
// during one file's copy stops the walk before the next entry and surfaces as
// the context's error, which is what lets Take's cleanup run inside the
// interrupt guard's grace instead of the process dying tree-half-written.
func TestWalkAndCopy_CancelMidWalkStopsAndSurfacesCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "z"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(root, "dst")
	ctx, cancel := context.WithCancel(context.Background())
	copied := 0
	err := walkAndCopy(ctx, src, dst, func(ctx context.Context, src, dst string) error {
		copied++
		cancel()
		return plainFileCopy(ctx, src, dst)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if copied != 1 {
		t.Fatalf("walk visited %d files after cancellation, want 1 (WalkDir order is lexical)", copied)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "z")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("file after the cancellation point must not be copied, stat err=%v", statErr)
	}
}

// TestTake_HoldsBeaconSharedForConcurrentProducers pins that the beacon never
// serializes producers against each other: two Takes may overlap (their
// reserved path triples are disjoint by construction), so the beacon is a
// liveness marker, not a mutex — a live shared holder must not make another
// Take refuse.
func TestTake_HoldsBeaconSharedForConcurrentProducers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, acquired, err := filelock.Acquire(context.Background(), producerBeaconPath(snapshotsDir), false, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("simulated concurrent producer: acquired=%v err=%v", acquired, err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}()
	if _, err := Take(context.Background(), src, snapshotsDir, ""); err != nil {
		t.Fatalf("Take must coexist with another live producer's shared hold: %v", err)
	}
}

// TestCollectOrphanedResidue_ErrorNamesThePath pins loud failure: when a
// condemned corpse cannot be removed, the error names it so the operator can
// reclaim the disk by hand. [LAW:no-silent-failure]
func TestCollectOrphanedResidue_ErrorNamesThePath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	t.Parallel()
	snapshotsDir := t.TempDir()
	tmpPath, _ := fabricateDeadResidue(t, snapshotsDir, "1700000000000000005")
	// Strip write permission on the nested dir so unlinking its child fails.
	// Classification renames the residue before deleting, so the undeletable
	// dir lives at the .condemned path by the time RemoveAll hits it.
	nested := filepath.Join(tmpPath, "nested")
	condemnedNested := filepath.Join(tmpPath+".condemned", "nested")
	if err := os.Chmod(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(nested, 0o755)
		_ = os.Chmod(condemnedNested, 0o755)
	})

	err := CollectOrphanedResidue(snapshotsDir)
	if err == nil {
		t.Fatal("collection must fail loudly when residue cannot be removed")
	}
	if !strings.Contains(err.Error(), "condemned") {
		t.Fatalf("error %q must name the condemned residue", err.Error())
	}
	// Convergence: fixing the permission lets the next collection finish.
	if err := os.Chmod(condemnedNested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("collection after fixing perms: %v", err)
	}
}
