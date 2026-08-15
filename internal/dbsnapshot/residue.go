package dbsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/promptctl/links-issue-tracker/internal/filelock"
)

// CollectOrphanedResidue reclaims producer artifacts (.tmp copies, .reserve
// claims, .condemned corpses) stranded in snapshotsDir by producers that died
// without cleanup — a SIGKILL, a crash, or the interrupt guard's post-grace
// hard exit mid-copy. Such residue is invisible to List and untouchable by
// retention pruning (both refuse producer-artifact names), so without this
// sweep every interrupted take permanently eats up to a store-sized chunk of
// disk. PruneMatching runs it first, which makes "the next take or prune
// collects whatever an interrupted take left" a property of every existing
// producer's retention tail rather than a convention each caller must recall.
//
// Safety is a kernel liveness proof, not a timing bet: every live Take holds
// the producer beacon SHARED for its whole reserve→copy→rename window, and a
// dead one's hold evaporated with its process, so acquiring the beacon
// EXCLUSIVELY proves every producer artifact currently in the directory is
// orphaned. When the probe reports contention a producer is alive somewhere;
// collection simply isn't this call's turn — that producer's own retention
// tail prunes (and therefore collects) after it finishes, so the skip defers
// reclamation, never forfeits it. [LAW:no-ambient-temporal-coupling] no age
// thresholds, no PID files — the discriminator is owned by the kernel.
//
// The exclusive window covers only classification: each dead artifact is
// renamed to a .condemned name (an atomic, sub-millisecond severing from the
// producer namespace), and the beacon is released before the RemoveAll pass —
// a multi-gigabyte corpse deletion must not hold new producers out (the same
// scoping PruneMatching's callers apply to the prune itself). A .condemned
// name has no owner by construction, so deleting it needs no lock, and a
// death between rename and delete just leaves it for the next collection.
func CollectOrphanedResidue(snapshotsDir string) error {
	// A workspace that has never snapshotted has nothing to collect; probing
	// the beacon here would create the directory as a side effect.
	if _, err := os.Stat(snapshotsDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat snapshots dir: %w", err)
	}
	// context.Background: a single-attempt probe never sleeps, so there is no
	// wait for a context to bound.
	release, acquired, err := filelock.Acquire(context.Background(), producerBeaconPath(snapshotsDir), true, 1, 0)
	if err != nil {
		return fmt.Errorf("probe snapshot producer beacon: %w", err)
	}
	if !acquired {
		return nil
	}
	condemned, condemnErr := condemnResidue(snapshotsDir)
	if relErr := release(); relErr != nil || condemnErr != nil {
		// [LAW:no-silent-failure] Either failure leaves residue standing (and a
		// failed release blocks producers until this process exits); both are
		// convergent — the next collection retries — but neither is silent.
		return errors.Join(condemnErr, relErr)
	}
	var removeErrs []error
	for _, path := range condemned {
		if err := os.RemoveAll(path); err != nil {
			removeErrs = append(removeErrs, fmt.Errorf("remove condemned residue %s: %w", path, err))
		}
	}
	return errors.Join(removeErrs...)
}

// condemnResidue classifies every producer artifact in snapshotsDir as dead —
// valid while the caller holds the beacon exclusively — and severs each from
// the producer namespace by renaming it to a .condemned name. Returns the
// full set of condemned paths (newly severed and any left by an earlier
// interrupted collection) for the caller to delete after the beacon drops.
func condemnResidue(snapshotsDir string) ([]string, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}
	var condemned []string
	for _, entry := range entries {
		name := entry.Name()
		if name == producerBeaconName || !isProducerArtifactName(name) {
			continue
		}
		path := filepath.Join(snapshotsDir, name)
		if filepath.Ext(name) != ".condemned" {
			condemnedPath := path + ".condemned"
			if err := os.Rename(path, condemnedPath); err != nil {
				return nil, fmt.Errorf("condemn residue %s: %w", path, err)
			}
			path = condemnedPath
		}
		condemned = append(condemned, path)
	}
	return condemned, nil
}
