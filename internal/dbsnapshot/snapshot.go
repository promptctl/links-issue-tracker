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
	created := time.Now().UTC()
	name := formatName(created, label)
	finalPath := filepath.Join(snapshotsDir, name)
	tmpPath := finalPath + ".tmp"
	if err := cloneTree(databaseDir, tmpPath); err != nil {
		_ = os.RemoveAll(tmpPath)
		return Snapshot{}, fmt.Errorf("clone tree: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.RemoveAll(tmpPath)
		return Snapshot{}, fmt.Errorf("rename tmp to final: %w", err)
	}
	return Snapshot{Path: finalPath, Name: name, Created: created}, nil
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
		if strings.HasSuffix(name, ".tmp") {
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
	snapshotPath := filepath.Join(snapshotsDir, name)
	info, err := os.Stat(snapshotPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrSnapshotMissing
		}
		return "", fmt.Errorf("stat snapshot: %w", err)
	}
	if !info.IsDir() {
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
			return os.MkdirAll(dstPath, info.Mode().Perm())
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
	return nil
}
