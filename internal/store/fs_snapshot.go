package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FsSnapshot is one filesystem-level backup of a Dolt repo directory.
// Snapshots live in <storageDir>/snapshots/<name>/, outside Dolt's awareness,
// so a Dolt-level corruption — or a bug in our auto-heal code that runs
// through Dolt's SQL layer — cannot touch them.
//
// [LAW:single-enforcer] These snapshots are the disaster-recovery floor.
// The Dolt safety branch (pre-migrate-<ns>) lives INSIDE Dolt's storage and
// protects against migration-level mistakes; the filesystem snapshot lives
// OUTSIDE and protects against repo-level corruption. Different blast
// radius, different protection.
type FsSnapshot struct {
	Name       string    `json:"name"`        // snap-<unixnano>-<hash8>
	Path       string    `json:"path"`        // absolute path to the snapshot directory
	CreatedAt  time.Time `json:"created_at"`  // parsed from Name
	SizeBytes  int64     `json:"size_bytes"`  // sum of contained file sizes; -1 if not measured
}

// fsSnapshotKeepN is the retention budget for filesystem snapshots. Older
// snapshots are pruned on each new takeFsSnapshot call. 5 chosen as a
// balance between recovery flexibility (more is better) and disk usage
// (more is worse, on filesystems without copy-on-write).
const fsSnapshotKeepN = 5

// fsSnapshotPrefix is the per-snapshot directory prefix. The full directory
// name is fsSnapshotPrefix + unix-nano + "-" + 8-hex-hash so that lexical
// sort matches chronological order.
const fsSnapshotPrefix = "snap-"

// snapshotsDirFor returns the absolute path to the snapshots directory for a
// given Dolt root. Snapshots live as a sibling of the dolt repo, so
// dropping the dolt repo doesn't take them with it.
func snapshotsDirFor(doltRootDir string) string {
	parent := filepath.Dir(filepath.Clean(doltRootDir))
	return filepath.Join(parent, "snapshots")
}

// takeFsSnapshot copies the entire Dolt repo directory at srcDoltDir into a
// new snapshot under snapshotsDirFor(srcDoltDir). Returns the snapshot name
// on success. Best-effort prunes older snapshots beyond fsSnapshotKeepN.
//
// Failure modes are surfaced to the caller; the caller decides whether to
// proceed without the snapshot (logged, non-fatal) or abort. Snapshot
// failure is NOT a fatal Open error by policy — losing a backup is
// worse than not having one, but worse still is refusing to run at all.
func takeFsSnapshot(srcDoltDir string) (string, error) {
	srcDoltDir = filepath.Clean(srcDoltDir)
	info, err := os.Stat(srcDoltDir)
	if err != nil {
		return "", fmt.Errorf("stat src dolt dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("src dolt dir is not a directory: %s", srcDoltDir)
	}

	snapsDir := snapshotsDirFor(srcDoltDir)
	if err := os.MkdirAll(snapsDir, 0o755); err != nil {
		return "", fmt.Errorf("create snapshots dir: %w", err)
	}

	name := generateSnapshotName(srcDoltDir)
	dst := filepath.Join(snapsDir, name)

	// Snapshot goes to a temp name first, then renames into place. Half-
	// written snapshots that crash mid-copy leave a `<name>.in-progress`
	// directory we can detect and clean up rather than a partial snapshot
	// that looks valid.
	tmp := dst + ".in-progress"
	if err := os.RemoveAll(tmp); err != nil {
		return "", fmt.Errorf("clean stale tmp: %w", err)
	}
	if err := copyDirCowFirst(srcDoltDir, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("rename snapshot into place: %w", err)
	}

	// Prune oldest beyond retention. Errors here are non-fatal — the new
	// snapshot is already on disk.
	_ = pruneFsSnapshots(snapsDir, fsSnapshotKeepN)

	return name, nil
}

// listFsSnapshots returns every snapshot in the snapshots directory, sorted
// newest-first. Half-written `.in-progress` directories are skipped.
func listFsSnapshots(snapshotsDir string) ([]FsSnapshot, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}
	var out []FsSnapshot
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, fsSnapshotPrefix) {
			continue
		}
		if strings.HasSuffix(name, ".in-progress") {
			continue
		}
		ts, ok := parseSnapshotTimestamp(name)
		if !ok {
			continue
		}
		out = append(out, FsSnapshot{
			Name:      name,
			Path:      filepath.Join(snapshotsDir, name),
			CreatedAt: ts,
			SizeBytes: -1,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// restoreFsSnapshot replaces dstDoltDir with the contents of snapshot `name`
// in snapshotsDir. Atomic at the directory-rename level: the current dolt
// dir is renamed aside (kept as `dolt.pre-restore-<ts>` for safety) before
// the snapshot is moved into place.
//
// The caller is responsible for ensuring no SQL connection is open against
// dstDoltDir at the time of restore — Dolt holds file locks and the rename
// will fail (or worse, corrupt the destination) if a connection is live.
// `lit doctor --restore-snapshot` enforces this by not opening the DB.
func restoreFsSnapshot(snapshotsDir, dstDoltDir, name string) error {
	src := filepath.Join(snapshotsDir, name)
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat snapshot %s: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("snapshot %s is not a directory", name)
	}

	dstDoltDir = filepath.Clean(dstDoltDir)
	// Stage the restored content under a temp name in the same parent
	// directory so the final move is a same-filesystem rename (atomic).
	parent := filepath.Dir(dstDoltDir)
	tmp := filepath.Join(parent, fmt.Sprintf("dolt.restoring-%d", time.Now().UnixNano()))
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clean stale restore staging: %w", err)
	}
	if err := copyDirCowFirst(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("copy snapshot %s into staging: %w", name, err)
	}

	// Rotate the existing dolt dir aside as a safety net. If anything goes
	// wrong after this point, the operator can rename it back.
	preRestore := filepath.Join(parent, fmt.Sprintf("dolt.pre-restore-%d", time.Now().UnixNano()))
	if _, err := os.Stat(dstDoltDir); err == nil {
		if err := os.Rename(dstDoltDir, preRestore); err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("rotate current dolt aside: %w", err)
		}
	}

	// Atomic move-into-place.
	if err := os.Rename(tmp, dstDoltDir); err != nil {
		// Best-effort rollback: restore the previous dolt dir from
		// preRestore. If that also fails, the operator has both
		// directories on disk and can hand-restore.
		if _, statErr := os.Stat(preRestore); statErr == nil {
			_ = os.Rename(preRestore, dstDoltDir)
		}
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("move restored dolt into place: %w", err)
	}

	return nil
}

// pruneFsSnapshots keeps the newest keepN snapshots and removes the rest.
// Half-written `.in-progress` directories older than 1h are also removed.
func pruneFsSnapshots(snapshotsDir string, keepN int) error {
	snaps, err := listFsSnapshots(snapshotsDir)
	if err != nil {
		return err
	}
	if len(snaps) > keepN {
		for _, s := range snaps[keepN:] {
			if err := os.RemoveAll(s.Path); err != nil {
				return fmt.Errorf("prune snapshot %s: %w", s.Name, err)
			}
		}
	}
	// Clean stale in-progress dirs (>1h old).
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".in-progress") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(snapshotsDir, entry.Name()))
		}
	}
	return nil
}

// copyDirCowFirst copies src → dst. Tries copy-on-write strategies first
// (APFS clonefile on macOS, reflink on Linux Btrfs/XFS), falling back to
// a deep recursive copy. The COW paths are essentially free; the fallback
// is a full byte-for-byte copy.
//
// On a fresh-init dolt repo this is ~43MB today; APFS clonefile completes
// in microseconds. On ext4 without reflinks the deep copy is ~100-200ms.
// [LAW:dataflow-not-control-flow] The strategy ordering is data-driven via
// `cpStrategies()` rather than scattered branches; the same exec loop
// runs every time.
func copyDirCowFirst(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create dst parent: %w", err)
	}
	var lastErr error
	for _, strat := range cpStrategies() {
		cmd := exec.Command(strat.bin, append(strat.args, src, dst)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%s %s: %w (output: %s)", strat.bin, strings.Join(strat.args, " "), err, strings.TrimSpace(string(out)))
		// On failure, remove partial dst before the next attempt so we
		// don't conflict with "destination exists" errors.
		_ = os.RemoveAll(dst)
	}
	return lastErr
}

type cpStrategy struct {
	bin  string
	args []string
}

// cpStrategies returns the ordered list of copy strategies to try, most
// efficient first. The list is platform-aware: macOS prefers `cp -cR`
// (APFS clonefile), Linux prefers `cp -aR --reflink=auto` (Btrfs/XFS),
// everywhere falls back to plain `cp -aR`.
func cpStrategies() []cpStrategy {
	switch runtime.GOOS {
	case "darwin":
		return []cpStrategy{
			{bin: "cp", args: []string{"-cR"}},        // APFS clonefile
			{bin: "cp", args: []string{"-pR"}},        // Plain recursive, preserve attrs
		}
	case "linux":
		return []cpStrategy{
			{bin: "cp", args: []string{"-aR", "--reflink=auto"}},
			{bin: "cp", args: []string{"-aR"}},
		}
	default:
		return []cpStrategy{
			{bin: "cp", args: []string{"-pR"}},
		}
	}
}

// generateSnapshotName produces a sortable, collision-resistant name.
// Format: snap-<unix-nano>-<8-hex-of-src-path-hash>. The hash provides
// uniqueness when two takes happen in the same nanosecond (rare but
// theoretically possible) and keeps the name fixed-width.
func generateSnapshotName(srcPath string) string {
	now := time.Now().UTC().UnixNano()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", srcPath, now)))
	return fmt.Sprintf("%s%d-%s", fsSnapshotPrefix, now, hex.EncodeToString(sum[:4]))
}

// parseSnapshotTimestamp extracts the UnixNano timestamp from a snapshot
// directory name. Returns false on malformed input.
func parseSnapshotTimestamp(name string) (time.Time, bool) {
	rest := strings.TrimPrefix(name, fsSnapshotPrefix)
	dash := strings.Index(rest, "-")
	if dash < 0 {
		return time.Time{}, false
	}
	nanos, err := strconv.ParseInt(rest[:dash], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, nanos).UTC(), true
}

// RestoreResult describes the outcome of a snapshot restore so callers
// (the CLI today, automation tomorrow) can render the operator-facing
// summary consistently.
type RestoreResult struct {
	Name           string `json:"name"`
	DoltRepoPath   string `json:"dolt_repo_path"`
	PreRestorePath string `json:"pre_restore_path"`
}

// SnapshotsDirForDoltRepo is the public projection of snapshotsDirFor. The
// CLI uses this to display paths and to scope list/restore operations.
func SnapshotsDirForDoltRepo(doltRepoPath string) string {
	return snapshotsDirFor(doltRepoPath)
}

// ListWorkspaceSnapshots is the public projection of listFsSnapshots,
// scoped to a workspace's dolt repo path. Returns newest-first.
func ListWorkspaceSnapshots(doltRepoPath string) ([]FsSnapshot, error) {
	return listFsSnapshots(snapshotsDirFor(doltRepoPath))
}

// LookupWorkspaceSnapshot returns the snapshot with the given name, or an
// error if it does not exist. Used by restore to validate the user's
// argument before any destructive action.
func LookupWorkspaceSnapshot(snapshotsDir, name string) (FsSnapshot, error) {
	snaps, err := listFsSnapshots(snapshotsDir)
	if err != nil {
		return FsSnapshot{}, err
	}
	for _, s := range snaps {
		if s.Name == name {
			return s, nil
		}
	}
	return FsSnapshot{}, fmt.Errorf("snapshot %q not found (run `lit snapshots list` to see available snapshots)", name)
}

// RestoreWorkspaceSnapshot replaces the Dolt repo at doltRepoPath with the
// snapshot identified by `name`. The current Dolt repo is rotated aside to
// a `dolt.pre-restore-<ts>` directory; the operator can delete that once
// confident the restore is correct, or rename it back to undo the restore.
//
// Caller-side precondition: no SQL connection is open against doltRepoPath
// at the time of the call. `lit snapshots restore` does not open Dolt, so
// the CLI satisfies this naturally.
func RestoreWorkspaceSnapshot(snapshotsDir, doltRepoPath, name string) (RestoreResult, error) {
	if _, err := LookupWorkspaceSnapshot(snapshotsDir, name); err != nil {
		return RestoreResult{}, err
	}
	parent := filepath.Dir(filepath.Clean(doltRepoPath))
	preRestore := filepath.Join(parent, fmt.Sprintf("dolt.pre-restore-%d", time.Now().UnixNano()))
	if err := restoreFsSnapshot(snapshotsDir, doltRepoPath, name); err != nil {
		return RestoreResult{}, err
	}
	// restoreFsSnapshot computes its own preRestore timestamp; we recover
	// it by scanning for the newest dolt.pre-restore-* sibling.
	preRestore = findNewestPreRestore(parent)
	return RestoreResult{
		Name:           name,
		DoltRepoPath:   doltRepoPath,
		PreRestorePath: preRestore,
	}, nil
}

// findNewestPreRestore returns the path of the most recent
// `dolt.pre-restore-<ts>` sibling of doltRepoPath. Empty string if none
// exists (the restore replaced a non-existent dolt dir).
func findNewestPreRestore(parent string) string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	var newest string
	var newestTs int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "dolt.pre-restore-") {
			continue
		}
		tsStr := strings.TrimPrefix(name, "dolt.pre-restore-")
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			continue
		}
		if ts > newestTs {
			newestTs = ts
			newest = filepath.Join(parent, name)
		}
	}
	return newest
}
