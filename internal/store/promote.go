package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// promote.go installs a verified candidate at the workspace's canonical path.
// The store is shared and lives at a fixed path that consumers cannot be
// repointed at, so promotion is an in-place swap at that path — never a wipe,
// never a repoint. Every step is an atomic rename(2), so the only state an
// interruption can leave at rest is "canonical directory absent", which one heal
// function reverses by restoring the moved-aside original.

// PromotionResult records where the rebuilt workspace landed and where the prior
// contents were preserved. Backup is persisted, not pruned: the pre-recovery copy
// is the most precious artifact in the flow.
type PromotionResult struct {
	Canonical string
	Backup    string
}

// PromoteCandidate atomically installs cand's rebuilt Dolt directory at
// canonicalDoltDir, moving the prior contents aside as a persisted backup.
//
// [LAW:single-enforcer] Serialization reuses the one workspace-exclusivity lock —
// the identical hold lit snapshots restore takes to rotate the Dolt directory —
// so the brief window in which the canonical path is mid-swap is invisible to
// every other consumer (they take the shared hold and wait). The lock file is a
// sibling of the Dolt directory, never part of it, so it is held continuously
// while the guarded directory is briefly absent.
//
// [LAW:types-are-the-program] Because every step is an atomic rename, "torn
// half-swapped directory" is unrepresentable; the only interrupted-at-rest state
// is "canonical absent, backup present", a detectable and recoverable state. The
// original is moved aside and never wiped, so "pre-recovery data destroyed" is
// likewise unrepresentable — the original is always one rename from restored.
func PromoteCandidate(ctx context.Context, canonicalDoltDir string, cand *Candidate) (_ PromotionResult, err error) {
	// [LAW:single-enforcer] Reject an empty path before deriving the lock path and
	// backup names from it, so the swap never creates artifacts in an unintended
	// (cwd-relative) directory — the same boundary Open/DumpRaw enforce.
	if err := validateDoltRootDir(canonicalDoltDir); err != nil {
		return PromotionResult{}, err
	}
	// Close the candidate store and surrender its Dolt directory before any
	// rename: an open handle blocks a directory rename on Windows, and the
	// promoted store is reopened fresh from the canonical path regardless.
	src, err := cand.detachForPromotion()
	if err != nil {
		return PromotionResult{}, fmt.Errorf("close candidate store before promotion: %w", err)
	}

	release, err := LockWorkspaceExclusive(ctx, canonicalDoltDir)
	if err != nil {
		return PromotionResult{}, err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()

	// Heal a prior crash before swapping: if the canonical directory is absent at
	// swap start, a previous promotion was interrupted between its two renames;
	// restore its backup first so this swap starts from the known invariant
	// (canonical present). [LAW:single-enforcer] This is the same heal the deferred
	// failure handler runs — one code path, two callers.
	if healErr := healCanonical(canonicalDoltDir); healErr != nil {
		return PromotionResult{}, healErr
	}

	// [LAW:no-silent-fallbacks] Roll BACK, never forward: any failure between the
	// two renames restores the known-good original, never the unverified
	// candidate. The handler fires only on error, so a successful swap is never
	// undone.
	defer func() {
		if err != nil {
			if healErr := healCanonical(canonicalDoltDir); healErr != nil {
				err = errors.Join(err, healErr)
			}
		}
	}()

	// [LAW:one-source-of-truth] Fixed-width nanosecond stamp so the lexical order
	// newestBackup relies on is the chronological order — an unpadded width would
	// let a shorter (older) stamp sort after a longer (newer) one across digit-count
	// boundaries. The width is shared with the stamp validator below.
	backup := fmt.Sprintf("%s.backup-%0*d", canonicalDoltDir, promotionStampWidth, time.Now().UTC().UnixNano())
	var preserved string
	preserved, err = moveAside(canonicalDoltDir, backup)
	if err != nil {
		return PromotionResult{}, err
	}
	if err = os.Rename(src, canonicalDoltDir); err != nil {
		return PromotionResult{}, fmt.Errorf("install rebuilt workspace at canonical path: %w", err)
	}
	// [LAW:types-are-the-program] Backup names a path that exists exactly when one
	// was made; when nothing pre-existed it is empty, never a phantom path.
	return PromotionResult{Canonical: canonicalDoltDir, Backup: preserved}, nil
}

// HealWorkspace repairs a workspace whose canonical directory is absent because a
// prior promotion was interrupted between its two renames: under the exclusive
// lock, it restores the newest backup. It is a no-op when the canonical directory
// is present, so it is safe to run unconditionally before any recovery.
//
// [LAW:single-enforcer] It brackets the same healCanonical the swap's deferred
// handler uses with the same exclusive lock, so "restore an interrupted swap" has
// exactly one implementation — reachable both mid-swap and as the standalone
// repair a fresh recovery runs first. Without this pre-step the promote-start heal
// is unreachable after a crash, because the read path gates on the canonical
// directory existing.
func HealWorkspace(ctx context.Context, canonicalDoltDir string) (err error) {
	// [LAW:single-enforcer] An empty path would put .links-workspace.lock in cwd
	// and scan cwd for backups; reject it at this exported boundary like the rest.
	if err := validateDoltRootDir(canonicalDoltDir); err != nil {
		return err
	}
	release, err := LockWorkspaceExclusive(ctx, canonicalDoltDir)
	if err != nil {
		return err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	return healCanonical(canonicalDoltDir)
}

// moveAside renames the canonical directory to its backup path, returning the
// backup path it actually created — empty when nothing pre-existed to preserve, so
// the caller never reports a path that does not exist. An absent canonical is a
// legitimate no-op: the install proceeds with no backup. [LAW:no-silent-fallbacks]
// Any stat error other than not-exist is a distinct failure mode the operator must
// see, not a missing dir.
func moveAside(canonicalDoltDir, backup string) (string, error) {
	switch _, statErr := os.Stat(canonicalDoltDir); {
	case statErr == nil:
		if err := os.Rename(canonicalDoltDir, backup); err != nil {
			return "", fmt.Errorf("move existing workspace aside: %w", err)
		}
		return backup, nil
	case errors.Is(statErr, os.ErrNotExist):
		return "", nil
	default:
		return "", fmt.Errorf("stat canonical workspace: %w", statErr)
	}
}

// healCanonical restores the canonical workspace from its newest backup when the
// canonical directory is absent. That absence is the sole interrupted-at-rest
// state a swap can leave (every step is an atomic rename), so this total function
// over {present, absent} is the whole self-recovery surface.
//
// [LAW:dataflow-not-control-flow] Run unconditionally at swap start and from the
// swap's deferred failure handler; the canonical directory's presence is the
// datum that decides whether it acts, not a caller-supplied "are we recovering?"
// flag.
func healCanonical(canonicalDoltDir string) error {
	switch _, statErr := os.Stat(canonicalDoltDir); {
	case statErr == nil:
		return nil
	case errors.Is(statErr, os.ErrNotExist):
		// fall through to restore
	default:
		return fmt.Errorf("stat canonical workspace: %w", statErr)
	}
	backup, err := newestBackup(canonicalDoltDir)
	if err != nil {
		return err
	}
	if backup == "" {
		// Canonical absent and no backup to restore: only reachable when nothing
		// pre-existed (a fresh install) or by external deletion. Either way this
		// process holds no copy to put back; the caller's own error (if any)
		// carries the real failure.
		return nil
	}
	if err := os.Rename(backup, canonicalDoltDir); err != nil {
		return fmt.Errorf("restore canonical workspace from backup %q: %w", backup, err)
	}
	return nil
}

// promotionStampWidth is the fixed digit width of a promotion backup's
// nanosecond stamp. [LAW:one-source-of-truth] One constant drives both the name
// PromoteCandidate writes and the pattern newestBackup accepts, so the producer
// and the recognizer cannot disagree. 19 digits holds every int64 UnixNano value
// (the type overflows in 2262, still 19 digits), so the stamps are equal-width
// and lexical order equals chronological order.
const promotionStampWidth = 19

// isPromotionBackup reports whether name is a backup PromoteCandidate produced:
// the backup prefix followed by exactly the fixed-width nanosecond stamp.
func isPromotionBackup(name, prefix string) bool {
	suffix, ok := strings.CutPrefix(name, prefix)
	if !ok || len(suffix) != promotionStampWidth {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// newestBackup returns the most recent promotion backup for the canonical path,
// or "" when none exist. Backups are named with a fixed-width nanosecond stamp,
// so lexical order is chronological order. [LAW:no-silent-fallbacks] Listing is
// by directory scan with a prefix filter rather than a glob, so a path containing
// glob metacharacters cannot silently skip a real backup.
func newestBackup(canonicalDoltDir string) (string, error) {
	dir := filepath.Dir(canonicalDoltDir)
	prefix := filepath.Base(canonicalDoltDir) + ".backup-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("scan workspace backups: %w", err)
	}
	var names []string
	for _, e := range entries {
		// [LAW:types-are-the-program] A restorable backup is exactly what
		// PromoteCandidate produces: a DIRECTORY named prefix + the fixed-width
		// nanosecond stamp. A stray regular file, or a hand-named directory like
		// "<prefix>manual", is not a workspace and must not be selected — it would
		// break the lexical-is-chronological ordering and could roll the workspace
		// back to the wrong contents.
		if e.IsDir() && isPromotionBackup(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1]), nil
}
