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
	// [LAW:single-enforcer][LAW:one-source-of-truth] Validate and canonicalize the
	// path before deriving the lock path, backup names, and rename target from it,
	// so the swap never creates artifacts in an unintended (cwd-relative) directory
	// and a trailing separator can't make backup naming and backup scanning target
	// different directories — every derivation flows from this one cleaned form.
	canonicalDoltDir, err = validateDoltRootDir(canonicalDoltDir)
	if err != nil {
		return PromotionResult{}, err
	}
	// Close the candidate store and surrender its Dolt directory before any
	// rename: an open handle blocks a directory rename on Windows, and the
	// promoted store is reopened fresh from the canonical path regardless.
	src, err := cand.detachForPromotion()
	if err != nil {
		return PromotionResult{}, fmt.Errorf("surrender candidate workspace for promotion: %w", err)
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

	// [LAW:single-enforcer] The lost-update gate runs under the SAME exclusive lock
	// that serializes the swap, between heal (canonical now guaranteed present) and
	// the first rename (nothing on disk has moved yet). Re-reading the live head
	// here, inside the critical section, is what makes the check atomic with the
	// install: a check outside the lock would reopen the very dump→promote window
	// this closes. A mismatch aborts before any rename, so the abort path changes
	// nothing on disk — the concurrent commit stays live, undisturbed.
	if headErr := verifyHeadUnchanged(ctx, canonicalDoltDir, cand.workspaceID, cand.expectedHead); headErr != nil {
		return PromotionResult{}, headErr
	}

	// [LAW:no-silent-failure] Roll BACK, never forward: any failure between the
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

	backup, err := uniqueBackupPath(canonicalDoltDir, time.Now().UTC().UnixNano())
	if err != nil {
		return PromotionResult{}, err
	}
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

// ErrWorkspaceAdvanced is the sentinel a lost-update abort wraps: the live
// workspace moved past the commit the candidate was rebuilt to replace. Callers
// detect it with errors.Is regardless of the operator-facing detail attached.
var ErrWorkspaceAdvanced = errors.New("workspace advanced since dump")

// ErrMissingDumpProvenance is the sentinel for a candidate whose dump carries no
// head — the lost-update gate cannot run because there is no commit to compare
// against. [LAW:types-are-the-program] "Provenance absent" is a distinct theorem
// from "workspace advanced": a dump produced by DumpRaw always records its head,
// so an empty one means the dump crossed a trust boundary that dropped it (an
// artifact decoded from JSON predating head tracking, or assembled by hand). It
// gets its own error rather than masquerading as a spurious advance from "".
var ErrMissingDumpProvenance = errors.New("dump has no recorded head commit")

// verifyHeadUnchanged is the lost-update gate. It re-reads the live workspace's
// Dolt head below the migration gate and refuses the promotion unless it still
// matches the head the candidate was rebuilt from.
//
// [LAW:single-enforcer] It reuses the one below-the-gate reader (openStoreConnection
// + readDoltHead) the dump itself used, never a second head-resolution path. The
// read takes no workspace lock — PromoteCandidate already holds the exclusive hold,
// so the head cannot move between this read and the swap.
//
// [LAW:no-silent-failure] A read failure aborts the promotion: if the live head
// cannot be confirmed unchanged, installing the candidate could silently regress a
// concurrent commit, so the safe action is to refuse and surface why. Read-only
// below the gate, the worst case is a read error with the live workspace untouched.
func verifyHeadUnchanged(ctx context.Context, canonicalDoltDir, workspaceID, expectedHead string) (err error) {
	// [LAW:no-silent-failure] A candidate with no recorded head cannot be checked
	// for staleness; promoting it would gamble the live workspace on an unverifiable
	// snapshot. Refuse with the provenance-specific error rather than comparing
	// against "" and reporting a bogus advance.
	if expectedHead == "" {
		return fmt.Errorf("%w: cannot verify the live workspace has not advanced; re-run `lit lifeboat dump` against the current workspace and recover from that artifact", ErrMissingDumpProvenance)
	}
	s, err := openStoreConnection(canonicalDoltDir, workspaceID)
	if err != nil {
		return fmt.Errorf("re-read live workspace head: %w", err)
	}
	defer func() {
		if closeErr := s.db.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			err = errors.Join(err, closeErr)
		}
	}()
	live, err := readDoltHead(ctx, s.db)
	if err != nil {
		return err
	}
	if live != expectedHead {
		return fmt.Errorf("%w: the candidate was rebuilt from %s but the live workspace is now at %s; "+
			"a concurrent commit landed during recovery — nothing was changed, re-run recovery against the current state",
			ErrWorkspaceAdvanced, expectedHead, live)
	}
	return nil
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
	// [LAW:single-enforcer][LAW:one-source-of-truth] Validate and canonicalize: an
	// empty path would put .links-workspace.lock in cwd and scan cwd for backups,
	// and a trailing separator would make healCanonical/newestBackup look in the
	// wrong directory. The cleaned form is what the lock and the backup scan agree
	// on.
	canonicalDoltDir, err = validateDoltRootDir(canonicalDoltDir)
	if err != nil {
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
// legitimate no-op: the install proceeds with no backup. [LAW:no-silent-failure]
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

// uniqueBackupPath returns a backup path for the canonical dir that does not yet
// exist, formatted with the fixed-width stamp newestBackup recognizes.
//
// [LAW:types-are-the-program] The path is unused BY CONSTRUCTION rather than
// assumed-unique-because-nanoseconds: a coarse clock could repeat UnixNano for two
// promotions, and since a backup is the most precious artifact in the flow, the
// move-aside must never clobber an existing one. PromoteCandidate holds the
// exclusive lock across this probe and the subsequent rename, so a path found free
// here stays free until it is used. Stepping the stamp forward keeps it the same
// fixed width and still chronological (the later promotion's backup is the newer
// one). [LAW:one-source-of-truth] The same promotionStampWidth drives the format
// here and the recognizer in isPromotionBackup.
func uniqueBackupPath(canonicalDoltDir string, nanos int64) (string, error) {
	for {
		path := fmt.Sprintf("%s.backup-%0*d", canonicalDoltDir, promotionStampWidth, nanos)
		switch _, err := os.Stat(path); {
		case errors.Is(err, os.ErrNotExist):
			return path, nil
		case err != nil:
			return "", fmt.Errorf("probe backup path: %w", err)
		}
		nanos++
	}
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
// so lexical order is chronological order. [LAW:no-silent-failure] Listing is
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
