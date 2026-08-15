// Package dbsnapshot takes filesystem-level snapshots of a Dolt storage
// directory using copy-on-write where available (APFS clonefile on Darwin,
// FICLONE on Linux) with a recursive-copy fallback. Snapshots are recovery
// primitives — they exist so a schema mutation can be rolled back to a
// known-good on-disk state without manual SQL or Dolt-internal branches.
//
// Trust boundary: callers MUST NOT hold an open Dolt connection on the
// destination directory while calling Restore. Take on a live workspace path
// requires the caller to hold, for the whole call, the workspace SHARED lock
// (transitively via an open Store, as the migration system does, or directly
// via store.LockWorkspaceShared, as the CLI does) plus the commit lock — the
// shared hold keeps a directory rotator (snapshots restore, adopt,
// promotion/heal) from rewriting the tree mid-copy, and the commit lock keeps
// writers from committing under the walk. Open Dolt connections are otherwise
// fine to keep during Take. For clean recovery the migration system should
// snapshot before the commit it's protecting. (This package cannot import
// store, so the requirement is a documented precondition, not an acquired
// one; PR #379's review caught the previous "Take is safe with open
// connections" wording inviting the next caller to skip the locks.)
//
// Distinct from those store-owned preconditions, the package acquires its own
// producer beacon (an flock inside snapshotsDir, see producerBeaconName): Take
// holds it shared, residue collection probes it exclusive, and it is always
// the innermost hold — after workspace, engine, and commit locks — taken by
// code that already holds nothing else it could wait on, so it cannot extend
// any lock cycle.
package dbsnapshot

import (
	"context"
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

	"github.com/promptctl/links-issue-tracker/internal/filelock"
)

// Snapshot is a frozen copy of a Dolt storage directory.
type Snapshot struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// ErrSnapshotMissing is returned by Restore when the named snapshot doesn't exist.
var ErrSnapshotMissing = errors.New("dbsnapshot: snapshot not found")

// producerBeaconName is the flock-backed liveness beacon inside snapshotsDir.
// Every Take holds it SHARED for its whole reserve→copy→rename window;
// CollectOrphanedResidue probes it EXCLUSIVE, so holding exclusively is
// kernel proof (surviving SIGKILL, panics, and os.Exit alike) that every
// producer artifact in the directory belongs to a dead producer. The name
// contains a dot, which sanitizeLabel maps out of every legal snapshot name,
// so no snapshot can ever collide with it.
const producerBeaconName = ".links-snapshot-producer.lock"

// producerBeaconRetryAttempts/producerBeaconRetryDelay bound how long a Take
// waits out a collector's exclusive hold. That hold covers only a ReadDir and
// a handful of renames (deletes happen after it is released), so ~1s is three
// orders of magnitude of headroom; exhausting it means a collector is wedged,
// which should surface loudly rather than queue silently.
const (
	producerBeaconRetryAttempts = 20
	producerBeaconRetryDelay    = 50 * time.Millisecond
)

func producerBeaconPath(snapshotsDir string) string {
	return filepath.Join(snapshotsDir, producerBeaconName)
}

// Take clones databaseDir into <snapshotsDir>/<name>/ and returns the snapshot.
// label is optional ("" = no suffix); non-safe characters are normalized.
// Atomicity: the clone lands in <name>.tmp first, then renames to <name> on
// success — an interrupt leaves no half-snapshot in the listing, and whatever
// .tmp/.reserve residue an uncleanable death (SIGKILL, the interrupt guard's
// post-grace hard exit) strands is reclaimed by CollectOrphanedResidue on a
// later prune, under the producer beacon's liveness proof.
//
// ctx cancels the copy between files and between chunks of a large file, so
// an interrupt aborts the walk and runs the cleanup below instead of dying
// with the tree half-written.
//
// [LAW:single-enforcer] All snapshot creation flows through this one function;
// no other code constructs snapshot directories.
func Take(ctx context.Context, databaseDir, snapshotsDir, label string) (snap Snapshot, err error) {
	// One explicit gate so a pre-canceled ctx refuses identically on every
	// platform — Darwin's single-syscall Clonefile fast path would otherwise
	// mint a snapshot the walk-based platforms refuse.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Snapshot{}, ctxErr
	}
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
	releaseBeacon, acquired, err := filelock.Acquire(ctx, producerBeaconPath(snapshotsDir), false, producerBeaconRetryAttempts, producerBeaconRetryDelay)
	if err != nil {
		return Snapshot{}, fmt.Errorf("acquire snapshot producer beacon: %w", err)
	}
	if !acquired {
		return Snapshot{}, fmt.Errorf("dbsnapshot: residue collection is holding the snapshots directory at %s and did not finish within its budget; retry", snapshotsDir)
	}
	// LIFO with the .reserve removal below: the beacon outlives every producer
	// artifact, so a collector can never classify a live Take's paths as dead.
	// [LAW:no-silent-failure] A failed release leaves the beacon held for the
	// process lifetime, deferring residue collection; surface it.
	defer func() {
		if relErr := releaseBeacon(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	reserved, err := reserveSnapshotPaths(snapshotsDir, label)
	if err != nil {
		return Snapshot{}, err
	}
	defer os.Remove(reserved.reservePath)
	if err := cloneTree(ctx, databaseDir, reserved.tmpPath); err != nil {
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
//
// Operates on the entire snapshots directory. Use PruneMatching when the
// directory holds multiple producers (e.g. user snapshots + migration
// snapshots) so each producer's retention budget is bounded independently.
func Prune(snapshotsDir string, keep int) error {
	return PruneMatching(snapshotsDir, keep, nil)
}

// PruneMatching removes oldest snapshots whose name satisfies match, until at
// most keep matching snapshots remain. Snapshots that do not satisfy match
// are untouched, regardless of age. match == nil treats every snapshot as
// matching (equivalent to Prune).
//
// [LAW:single-enforcer] The two-producer snapshots directory (user snapshots
// from `lit snapshots new`; migration snapshots from migrate) is partitioned
// at the *prune* boundary, not at the directory boundary, so each producer's
// retention budget is bounded independently and one producer cannot evict
// the other's snapshots.
// [LAW:types-are-the-program] The kind discriminator (which producer
// owns this snapshot) is carried in the name and read by match; the
// directory traversal is fixed.
func PruneMatching(snapshotsDir string, keep int, match func(name string) bool) error {
	if keep <= 0 {
		return fmt.Errorf("dbsnapshot: keep must be > 0")
	}
	// Residue collection rides every producer's existing retention tail, so
	// "an interrupted take's leftovers are reclaimed by the next take/prune"
	// holds without any caller remembering a second cleanup call.
	// [LAW:single-enforcer] this is the one boundary all pruning flows
	// through, so it is also the one boundary residue collection needs.
	if err := CollectOrphanedResidue(snapshotsDir); err != nil {
		return err
	}
	snapshots, err := List(snapshotsDir)
	if err != nil {
		return err
	}
	matched := snapshots
	if match != nil {
		matched = make([]Snapshot, 0, len(snapshots))
		for _, s := range snapshots {
			if match(s.Name) {
				matched = append(matched, s)
			}
		}
	}
	if len(matched) <= keep {
		return nil
	}
	for _, snapshot := range matched[keep:] {
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

// producerArtifactSuffixes are the name suffixes the producer machinery owns:
// an in-flight copy (.tmp), a reservation claim (.reserve), and a corpse the
// collector has condemned (.condemned). No legal snapshot name can carry one —
// sanitizeLabel maps '.' out of every label — so the suffix namespace is
// disjoint from snapshot names by construction, not by filtering discipline.
var producerArtifactSuffixes = []string{".tmp", ".reserve", ".condemned"}

func isProducerArtifactName(name string) bool {
	for _, suffix := range producerArtifactSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// parseName is the one predicate for "is this a snapshot name". Rejecting
// producer artifacts here (not just in List's loop) means every consumer —
// List, validateSnapshotName, and through it Restore — refuses them from one
// source; previously Restore would accept a labeled ".tmp" leftover ("<ns>-
// <label>.tmp" parses as <ns>) and install a torn partial copy as the
// database. [LAW:one-source-of-truth]
func parseName(name string) (time.Time, bool) {
	if isProducerArtifactName(name) {
		return time.Time{}, false
	}
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
// Symlinks are recreated; other special entries error out. The per-entry ctx
// gate is what lets an interrupt abandon a long fallback copy mid-tree and
// hand control back to Take's cleanup within the interrupt guard's grace.
func walkAndCopy(ctx context.Context, src, dst string, copyFile func(ctx context.Context, src, dst string) error) error {
	return filepath.WalkDir(src, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
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
			return copyFile(ctx, srcPath, dstPath)
		default:
			return fmt.Errorf("dbsnapshot: unsupported file type at %s: %v", srcPath, d.Type())
		}
	})
}

// plainFileCopy is the universal fallback: open src, create dst with the
// source's perm bits, chunked copy.
func plainFileCopy(ctx context.Context, src, dst string) error {
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
	if err := copyWithContext(ctx, dstF, srcF); err != nil {
		return err
	}
	// OpenFile's mode is filtered by umask; Chmod forces exact source perms.
	return dstF.Chmod(info.Mode().Perm())
}

// copyContextChunk bounds how many bytes copy between ctx checks. Dolt table
// files can be multi-gigabyte monoliths, so between-files cancellation alone
// would leave an interrupt blocked for the length of one file — the exact
// slow-copy case the ctx support exists for. 32MiB keeps cancellation latency
// sub-second on any medium worth copying to.
const copyContextChunk = 32 << 20

// copyWithContext copies src into dst in copyContextChunk pieces, consulting
// ctx between pieces. io.CopyN is used (rather than wrapping src in a
// ctx-checking io.Reader) because *os.File's ReadFrom unwraps the LimitedReader
// CopyN builds, so each chunk still rides the kernel fast paths
// (copy_file_range/sendfile) instead of degrading to a userspace buffer loop.
func copyWithContext(ctx context.Context, dst, src *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, copyContextChunk); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
