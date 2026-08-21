package dbsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/primitives/filelock"
)

// CollectOrphanedResidue reclaims producer artifacts (.tmp copies, .reserve
// claims, .condemned corpses) stranded in snapshotsDir by producers that died
// without cleanup — a SIGKILL, a crash, or the interrupt guard's post-grace
// hard exit mid-copy. Such residue is invisible to List and untouchable by
// retention pruning (both refuse producer-artifact names), so without this
// sweep every interrupted take permanently eats up to a store-sized chunk of
// disk. Take runs it at entry — reached by every producer on every attempt,
// before new disk is consumed — so "the next take collects whatever an
// interrupted take left" holds without any caller remembering a cleanup call,
// and holds in the disk-full regime where only reclaiming first lets the
// retry succeed.
//
// Safety is a kernel liveness proof, not a timing bet: every live Take holds
// the producer beacon SHARED for its whole reserve→copy→rename window, and a
// dead one's hold evaporated with its process, so acquiring the beacon
// EXCLUSIVELY proves every producer artifact currently in the directory is
// orphaned. When the probe reports contention a producer is alive somewhere;
// collection simply isn't this call's turn — that producer's own next take
// (or any later one) collects after it finishes, so the skip defers
// reclamation, never forfeits it. [LAW:no-ambient-temporal-coupling] no age
// thresholds, no PID files — the discriminator is owned by the kernel. The
// one caveat is a mixed-version window: a still-running pre-beacon binary's
// live Take holds no beacon, so it reads as dead here; overlapping such a
// Take needs only an old binary running beside a new one (its O_EXCL-era
// commit lock and today's flock don't exclude each other), and the exposure
// retires with the old binary.
//
// The exclusive window covers only classification: each dead artifact is
// renamed to a .condemned name (an atomic, sub-millisecond severing from the
// producer namespace), and the beacon is released before the RemoveAll pass —
// a multi-gigabyte corpse deletion must not extend the window producers
// retry against. A .condemned name has no owner by construction, so deleting
// it needs no lock; a death between rename and delete just leaves it for the
// next collection, and two collectors reaping concurrently converge because
// RemoveAll tolerates entries vanishing under it.
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

// condemnResidue classifies every collectible artifact in snapshotsDir as
// dead — valid while the caller holds the beacon exclusively — and severs
// each from the producer namespace by renaming it to a fresh .condemned name.
// Returns the full set of condemned paths (newly severed and any left by an
// earlier interrupted collection) for the caller to delete after the beacon
// drops.
//
// isCollectibleResidue, not the broad rejection predicate, gates destruction:
// a foreign *.tmp an operator parked here is spared. The condemned name
// carries a fresh nanosecond stamp so a rename can never collide with a
// leftover from an earlier interrupted collection — collection stays
// self-healing even if a producer reuses the exact original stamp.
func condemnResidue(snapshotsDir string) ([]string, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}
	var condemned []string
	for _, entry := range entries {
		name := entry.Name()
		if !isCollectibleResidue(name) {
			continue
		}
		path := filepath.Join(snapshotsDir, name)
		if !strings.HasSuffix(name, condemnedSuffix) {
			condemnedPath := fmt.Sprintf("%s.%d%s", path, time.Now().UnixNano(), condemnedSuffix)
			if err := os.Rename(path, condemnedPath); err != nil {
				return nil, fmt.Errorf("condemn residue %s: %w", path, err)
			}
			path = condemnedPath
		}
		condemned = append(condemned, path)
	}
	return condemned, nil
}
