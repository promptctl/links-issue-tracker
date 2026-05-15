// Package dbsnapshot takes filesystem-level snapshots of a Dolt storage
// directory using copy-on-write where available (APFS clonefile on Darwin,
// FICLONE on Linux) with a recursive-copy fallback. Snapshots are recovery
// primitives — they exist so a schema mutation can be rolled back to a
// known-good on-disk state without manual SQL or Dolt-internal branches.
//
// Trust boundary: callers MUST NOT hold an open Dolt connection on the
// destination directory while calling Restore. Take is safe with open
// connections (the snapshot is independent), but for clean recovery the
// migration system should snapshot before the commit it's protecting.
package dbsnapshot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot is a frozen copy of a Dolt storage directory.
type Snapshot struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// ErrSnapshotMissing is returned by Restore when the named snapshot doesn't exist.
var ErrSnapshotMissing = errors.New("dbsnapshot: snapshot not found")

// Take clones databaseDir into <snapshotsDir>/<name>/ and returns the snapshot.
// label is optional ("" = no suffix); non-safe characters are normalized.
// Atomicity: the clone lands in <name>.tmp first, then renames to <name> on
// success — an interrupt leaves no half-snapshot in the listing.
//
// [LAW:single-enforcer] All snapshot creation flows through this one function;
// no other code constructs snapshot directories.
func Take(databaseDir, snapshotsDir, label string) (Snapshot, error) {
	info, err := os.Stat(databaseDir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat database dir: %w", err)
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("database dir is not a directory: %s", databaseDir)
	}
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create snapshots dir: %w", err)
	}
	reserved, err := reserveSnapshotPaths(snapshotsDir, label)
	if err != nil {
		return Snapshot{}, err
	}
	defer os.Remove(reserved.reservePath)
	if err := cloneTree(databaseDir, reserved.tmpPath); err != nil {
		_ = os.RemoveAll(reserved.tmpPath)
		return Snapshot{}, fmt.Errorf("clone tree: %w", err)
	}
	if err := os.Rename(reserved.tmpPath, reserved.finalPath); err != nil {
		_ = os.RemoveAll(reserved.tmpPath)
		return Snapshot{}, fmt.Errorf("rename tmp to final: %w", err)
	}
	return Snapshot{Path: reserved.finalPath, Name: reserved.name, Created: reserved.created}, nil
}

// reservedPaths bundles the four paths a successful reservation produces. The
// .reserve sentinel is the atomic claim on a slot; tmpPath is where cloneTree
// writes; finalPath is where Rename installs the snapshot on success.
type reservedPaths struct {
	created     time.Time
	name        string
	finalPath   string
	tmpPath     string
	reservePath string
}

// reserveSnapshotPaths atomically claims a free (<name>, <name>.tmp,
// <name>.reserve) triple under snapshotsDir by os.Mkdir-ing the .reserve
// sentinel. The Mkdir call is the kernel-atomic primitive that fails with
// EEXIST under any contention (in-process, cross-process, cross-host on a
// shared FS), eliminating the check-then-use race. On EEXIST we increment
// the candidate by 1ns and retry. Bounded by maxReserveAttempts.
//
// finalPath/tmpPath are also stat-checked under the held reservation as a
// paranoia gate against stale leftovers (e.g. a crash that left .tmp behind
// without holding .reserve). The .reserve sentinel sits at a sibling path so
// the Darwin Clonefile fast path (which requires dst not to exist) is
// unaffected.
const maxReserveAttempts = 1024

func reserveSnapshotPaths(snapshotsDir, label string) (reservedPaths, error) {
	candidate := time.Now().UTC()
	for attempt := 0; attempt < maxReserveAttempts; attempt++ {
		name := formatName(candidate, label)
		finalPath := filepath.Join(snapshotsDir, name)
		reservePath := finalPath + ".reserve"
		tmpPath := finalPath + ".tmp"
		switch err := os.Mkdir(reservePath, 0o755); {
		case err == nil:
			finalFree, statErr := pathFree(finalPath)
			if statErr != nil {
				_ = os.Remove(reservePath)
				return reservedPaths{}, statErr
			}
			tmpFree, statErr := pathFree(tmpPath)
			if statErr != nil {
				_ = os.Remove(reservePath)
				return reservedPaths{}, statErr
			}
			if !finalFree || !tmpFree {
				_ = os.Remove(reservePath)
				candidate = candidate.Add(time.Nanosecond)
				continue
			}
			return reservedPaths{
				created:     candidate,
				name:        name,
				finalPath:   finalPath,
				tmpPath:     tmpPath,
				reservePath: reservePath,
			}, nil
		case errors.Is(err, fs.ErrExist):
			candidate = candidate.Add(time.Nanosecond)
		default:
			return reservedPaths{}, fmt.Errorf("reserve %s: %w", reservePath, err)
		}
	}
	return reservedPaths{}, fmt.Errorf("dbsnapshot: no free snapshot name after %d attempts", maxReserveAttempts)
}

func pathFree(p string) (bool, error) {
	switch _, err := os.Stat(p); {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	default:
		return false, fmt.Errorf("stat %s: %w", p, err)
	}
}

// List returns snapshots in snapshotsDir, newest-first. Entries that don't
// match the <unix-ns>[-<label>] naming scheme are silently skipped so leftover
// directories from prior (incompatible) snapshot implementations don't pollute
// the listing.
//
// [LAW:one-source-of-truth] formatName and parseName are inverses; no other
// code parses or constructs snapshot directory names.
func List(snapshotsDir string) ([]Snapshot, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Snapshot{}, nil
		}
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}
	snapshots := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".reserve") {
			continue
		}
		created, ok := parseName(name)
		if !ok {
			continue
		}
		snapshots = append(snapshots, Snapshot{
			Path:    filepath.Join(snapshotsDir, name),
			Name:    name,
			Created: created,
		})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Created.After(snapshots[j].Created)
	})
	return snapshots, nil
}

// Restore moves <snapshotsDir>/<name>/ into databaseDir, after rotating any
// existing databaseDir to <databaseDir>.pre-restore-<unix-ns> for operator
// undo. Returns the rotated path (or "" if databaseDir didn't exist).
//
// [LAW:no-defensive-null-guards] Caller-must-not-hold-open-Dolt-connection is
// a documented invariant; we don't detect or close on the caller's behalf. The
// CLI wires this via r.wsCmd, which structurally cannot open a connection.
func Restore(databaseDir, snapshotsDir, name string) (string, error) {
	if err := validateSnapshotName(name); err != nil {
		return "", err
	}
	snapshotPath := filepath.Join(snapshotsDir, name)
	// Lstat (not Stat) so a symlink at snapshotPath fails the IsDir check rather
	// than being followed to whatever the attacker pointed it at. os.Rename
	// would otherwise install the symlink itself as the database directory.
	info, err := os.Lstat(snapshotPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrSnapshotMissing
		}
		return "", fmt.Errorf("stat snapshot: %w", err)
	}
	if !info.Mode().IsDir() {
		return "", fmt.Errorf("snapshot is not a directory: %s", snapshotPath)
	}
	rotatedPath := ""
	switch _, statErr := os.Stat(databaseDir); {
	case statErr == nil:
		rotatedPath = fmt.Sprintf("%s.pre-restore-%d", databaseDir, time.Now().UTC().UnixNano())
		if err := os.Rename(databaseDir, rotatedPath); err != nil {
			return "", fmt.Errorf("rotate existing database dir: %w", err)
		}
	case errors.Is(statErr, fs.ErrNotExist):
		// Nothing to rotate; restore proceeds without an undo path.
	default:
		return "", fmt.Errorf("stat database dir: %w", statErr)
	}
	if err := os.Rename(snapshotPath, databaseDir); err != nil {
		return rotatedPath, fmt.Errorf("install snapshot at database dir: %w", err)
	}
	return rotatedPath, nil
}

// Prune removes oldest snapshots until at most keep remain. keep must be > 0.
func Prune(snapshotsDir string, keep int) error {
	if keep <= 0 {
		return fmt.Errorf("dbsnapshot: keep must be > 0")
	}
	snapshots, err := List(snapshotsDir)
	if err != nil {
		return err
	}
	if len(snapshots) <= keep {
		return nil
	}
	for _, snapshot := range snapshots[keep:] {
		if err := os.RemoveAll(snapshot.Path); err != nil {
			return fmt.Errorf("remove snapshot %s: %w", snapshot.Path, err)
		}
	}
	return nil
}

func formatName(t time.Time, label string) string {
	base := strconv.FormatInt(t.UnixNano(), 10)
	clean := sanitizeLabel(label)
	if clean == "" {
		return base
	}
	return base + "-" + clean
}

// validateSnapshotName rejects user-supplied names that would let Restore
// target anything outside the canonical <snapshotsDir>/<name> path. The two
// checks compose:
//   - name == filepath.Base(name): rejects path separators, "..", and absolute
//     paths (filepath.Base collapses them to a final component).
//   - parseName(name) ok: rejects ".tmp" leftovers, legacy snap-<ts>-<hash>/
//     directories, and any other non-canonical entry that List would skip.
//
// [LAW:single-enforcer] All Restore-time name validation flows through this
// one gate; callers don't reimplement path-safety checks.
func validateSnapshotName(name string) error {
	if name == "" {
		return errors.New("dbsnapshot: snapshot name is empty")
	}
	if name != filepath.Base(name) {
		return fmt.Errorf("dbsnapshot: snapshot name must be a single path component: %q", name)
	}
	if _, ok := parseName(name); !ok {
		return fmt.Errorf("dbsnapshot: snapshot name does not match the <unix-ns>[-<label>] scheme: %q", name)
	}
	return nil
}

func parseName(name string) (time.Time, bool) {
	head := name
	if idx := strings.IndexByte(name, '-'); idx >= 0 {
		head = name[:idx]
	}
	ns, err := strconv.ParseInt(head, 10, 64)
	if err != nil || ns <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns).UTC(), true
}

func sanitizeLabel(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// walkAndCopy creates dst as a tree-copy of src using copyFile for each
// regular file. Directories are recreated with their source perm bits.
// Symlinks are recreated; other special entries error out.
func walkAndCopy(src, dst string, copyFile func(src, dst string) error) error {
	return filepath.WalkDir(src, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, srcPath)
		if relErr != nil {
			return relErr
		}
		dstPath := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			if err := os.MkdirAll(dstPath, info.Mode().Perm()); err != nil {
				return err
			}
			// MkdirAll honors the process umask; Chmod forces exact source perms
			// so the snapshot is mode-identical regardless of who took it.
			return os.Chmod(dstPath, info.Mode().Perm())
		case d.Type()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(srcPath)
			if readErr != nil {
				return readErr
			}
			return os.Symlink(target, dstPath)
		case d.Type().IsRegular():
			return copyFile(srcPath, dstPath)
		default:
			return fmt.Errorf("dbsnapshot: unsupported file type at %s: %v", srcPath, d.Type())
		}
	})
}

// plainFileCopy is the universal fallback: open src, create dst with the
// source's perm bits, io.Copy.
func plainFileCopy(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()
	info, err := srcF.Stat()
	if err != nil {
		return err
	}
	dstF, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer dstF.Close()
	if _, err := io.Copy(dstF, srcF); err != nil {
		return err
	}
	// OpenFile's mode is filtered by umask; Chmod forces exact source perms.
	return dstF.Chmod(info.Mode().Perm())
}
