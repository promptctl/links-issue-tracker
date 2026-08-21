package dbsnapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	condemnedLeftover := filepath.Join(snapshotsDir, "1700000000000000002.tmp.1700000000000000005.condemned")
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
		if IsProducerArtifactName(e.Name()) {
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

// TestTake_CollectsResidueAtEntry pins the wiring: collection runs at Take's
// entry — the one point every producer reaches on every attempt BEFORE new
// disk is consumed — so an ENOSPC'd retry reclaims a dead predecessor's
// corpse first, and a dead take's leftovers are gone by the time the next
// take's own copy begins. (Wiring it downstream of a successful take, e.g.
// in the retention prune, is unreachable in exactly the disk-full regime
// that motivates collection.)
func TestTake_CollectsResidueAtEntry(t *testing.T) {
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
	tmpPath, reservePath := fabricateDeadResidue(t, snapshotsDir, "1700000000000000004")

	snap, err := Take(context.Background(), src, snapshotsDir, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Take entry must collect .tmp residue, stat err=%v", err)
	}
	if _, err := os.Stat(reservePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Take entry must collect .reserve residue, stat err=%v", err)
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("the take itself must still succeed: %v", err)
	}
}

// TestCollectOrphanedResidue_SparesForeignNames pins delete-narrowly: the
// collector destroys only names whose full shape lit provably minted (a
// parseable <unix-ns>[-label] head under .tmp/.reserve, or the collector's
// own <artifact>.<ns>.condemned). A foreign "backup.tmp" — or a foreign
// "backup.condemned", since a suffix is never provenance over a directory
// lit doesn't own — is inert to every consumer, including the one
// destructive one.
func TestCollectOrphanedResidue_SparesForeignNames(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	spared := []string{
		"backup.tmp",       // foreign head under a producer suffix
		"backup.condemned", // condemned suffix without the collector's shape
		"backup.tmp.1700000000000000001.condemned", // collector frame around a foreign head
		"1700000000000000006.tmp.condemned",        // stampless: no collector ever minted it
	}
	for _, name := range spared {
		if err := os.MkdirAll(filepath.Join(snapshotsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	foreignFile := filepath.Join(snapshotsDir, "notes.reserve")
	if err := os.WriteFile(foreignFile, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	litShaped, _ := fabricateDeadResidue(t, snapshotsDir, "1700000000000000006")
	litCondemned := filepath.Join(snapshotsDir, "1700000000000000007.reserve.1700000000000000002.condemned")
	if err := os.MkdirAll(litCondemned, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("CollectOrphanedResidue: %v", err)
	}

	for _, name := range spared {
		if _, err := os.Stat(filepath.Join(snapshotsDir, name)); err != nil {
			t.Fatalf("foreign %s must be spared: %v", name, err)
		}
	}
	if _, err := os.Stat(foreignFile); err != nil {
		t.Fatalf("foreign notes.reserve must be spared: %v", err)
	}
	if _, err := os.Stat(litShaped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lit-shaped residue must be collected, stat err=%v", err)
	}
	if _, err := os.Stat(litCondemned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collector-shaped condemned corpse must be collected, stat err=%v", err)
	}
}

// TestCollectOrphanedResidue_LongLabelResidueStillCondemnable pins the label
// bound end-to-end: the longest label sanitizeLabel can emit still leaves the
// condemnation rename (<name>.<ns>.condemned) inside a 255-byte NAME_MAX, so
// a killed Take with a maximal label leaves residue the collector can rename
// and reap rather than a corpse stranded behind ENAMETOOLONG forever.
func TestCollectOrphanedResidue_LongLabelResidueStillCondemnable(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	longName := formatName(time.Unix(0, 1700000000000000008), strings.Repeat("x", 300))
	residue := filepath.Join(snapshotsDir, longName+".tmp")
	if err := os.MkdirAll(residue, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("CollectOrphanedResidue: %v", err)
	}
	if _, err := os.Stat(residue); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("long-label residue must be collected, stat err=%v", err)
	}
}

// TestCollectOrphanedResidue_ConvergesOnStampReuse pins self-healing when a
// producer reuses the exact stamp of residue an interrupted collection left
// behind: the fresh corpse and the old .condemned corpse coexist, and one
// collection removes both — condemned names carry a fresh nanosecond stamp,
// so the rename can never collide and wedge collection permanently.
func TestCollectOrphanedResidue_ConvergesOnStampReuse(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	tmpPath, _ := fabricateDeadResidue(t, snapshotsDir, "1700000000000000007")
	// The corpse an interrupted collection actually leaves carries the
	// collector's nanosecond stamp — condemnResidue always mints
	// <artifact>.<ns>.condemned, never a bare .condemned.
	oldCondemned := tmpPath + ".1700000000000000001.condemned"
	if err := os.MkdirAll(oldCondemned, 0o755); err != nil {
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
		if IsProducerArtifactName(e.Name()) {
			t.Fatalf("residue survived stamp-reuse collection: %s", e.Name())
		}
	}
}

// TestTake_CollectionFailureDoesNotFailTake pins the failure-domain split: an
// undeletable corpse degrades collection (loud on stderr, retried next take)
// but must not fail the take — the snapshot this call was asked for is still
// mintable, and failing it would also starve retention at every producer.
func TestTake_CollectionFailureDoesNotFailTake(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
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
	tmpPath, _ := fabricateDeadResidue(t, snapshotsDir, "1700000000000000008")
	nested := filepath.Join(tmpPath, "nested")
	if err := os.Chmod(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restoreCondemnedPerms(t, snapshotsDir) })

	snap, err := Take(context.Background(), src, snapshotsDir, "")
	if err != nil {
		t.Fatalf("Take must succeed despite a collection failure: %v", err)
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("snapshot must exist: %v", err)
	}
}

// restoreCondemnedPerms re-opens write permission on every condemned corpse's
// nested dir so t.TempDir cleanup can remove the tree.
func restoreCondemnedPerms(t *testing.T, snapshotsDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(snapshotsDir, "*.condemned"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		_ = os.Chmod(filepath.Join(m, "nested"), 0o755)
	}
	matches, err = filepath.Glob(filepath.Join(snapshotsDir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		_ = os.Chmod(filepath.Join(m, "nested"), 0o755)
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
	// Residue present while a producer is live: the entry collection's
	// exclusive probe must skip (any of it could be the live producer's own
	// in-flight paths), and the take itself must still proceed.
	tmpPath, _ := fabricateDeadResidue(t, snapshotsDir, "1700000000000000009")
	if _, err := Take(context.Background(), src, snapshotsDir, ""); err != nil {
		t.Fatalf("Take must coexist with another live producer's shared hold: %v", err)
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("residue must be spared while a producer is live: %v", err)
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
	// Classification renames the residue (to a uniquely-stamped .condemned
	// name) before deleting, so the undeletable dir lives under a *.condemned
	// path by the time RemoveAll hits it — located by glob below.
	if err := os.Chmod(filepath.Join(tmpPath, "nested"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restoreCondemnedPerms(t, snapshotsDir) })

	err := CollectOrphanedResidue(snapshotsDir)
	if err == nil {
		t.Fatal("collection must fail loudly when residue cannot be removed")
	}
	if !strings.Contains(err.Error(), "condemned") {
		t.Fatalf("error %q must name the condemned residue", err.Error())
	}
	// Convergence: fixing the permission lets the next collection finish.
	restoreCondemnedPerms(t, snapshotsDir)
	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		t.Fatalf("collection after fixing perms: %v", err)
	}
}
