// Package dbsnapshot takes filesystem-level snapshots of a Dolt storage
// directory using copy-on-write where available (APFS clonefile on Darwin,
// FICLONE on Linux) with a recursive-copy fallback. Snapshots are recovery
// primitives — they exist so a schema mutation can be rolled back to a
// known-good on-disk state without manual SQL or Dolt-internal branches.
//
// Trust boundary: callers MUST NOT hold an open Dolt connection on the
// destination directory while calling Restore. Take on a live workspace path
// requires the caller to hold, for the whole call: the workspace SHARED lock
// (transitively via an open Store, as the migration system does, or directly
// via store.LockWorkspaceShared, as the CLI does), Dolt's own journal lock
// (transitively via the open Store's engine, which holds it for its
// lifetime, or directly via store.LockDoltJournalExclusive), and the commit
// lock. That hold sequence follows lit's lock discipline — canonical in
// package store's doc (internal/store/doc.go); this paragraph owns only
// which holds Take's callers must bring, never the order's rationale.
// The shared hold keeps a directory rotator (snapshots restore, adopt,
// promotion/heal) from rewriting the tree mid-copy; the journal hold keeps a
// concurrent open's engine-lifecycle I/O — journal crash-recovery after an
// unclean kill, close-time flush — from rewriting the journal under the walk
// (links-sync-pgct.15); the commit lock keeps writers from committing under
// it. Open Dolt connections are otherwise fine to keep during Take. For
// clean recovery the migration system should snapshot before the commit it's
// protecting. (This package cannot import store, so the requirement is a
// documented precondition, not an acquired one; PR #379's review caught the
// previous "Take is safe with open connections" wording inviting the next
// caller to skip the locks.)
//
// Distinct from those store-owned preconditions, the package acquires its own
// producer beacon (an flock inside snapshotsDir, see producerBeaconName): Take
// holds it shared, residue collection probes it exclusive, and it is always
// the innermost hold — after the workspace, journal, and commit locks — taken by
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

	"github.com/promptctl/primitives/filelock"
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

// ErrSnapshotsBusy is the sentinel Take's producer-beacon contention wraps,
// so callers detect "retry shortly" programmatically the same way every other
// lock contention in this codebase is detected — via errors.Is, with the
// message text as context guidance rather than the contract.
var ErrSnapshotsBusy = errors.New("snapshot producer beacon busy")

// Take clones databaseDir into <snapshotsDir>/<name>/ and returns the snapshot.
// label is optional ("" = no suffix); non-safe characters are normalized.
// Atomicity: the clone lands in <name>.tmp first, then renames to <name> on
// success — an interrupt leaves no half-snapshot in the listing, and whatever
// .tmp/.reserve residue an uncleanable death (SIGKILL, the interrupt guard's
// post-grace hard exit) strands is reclaimed at the next Take's entry, under
// the producer beacon's liveness proof.
//
// ctx cancels the copy between files and between chunks of a large file, so
// an interrupt aborts the walk and runs the cleanup below instead of dying
// with the tree half-written.
//
// [LAW:single-enforcer] All snapshot creation flows through this one function;
// no other code constructs snapshot directories. Residue collection rides the
// same boundary — Take's entry — because that is the one point reached by
// every producer on every attempt BEFORE new disk is consumed: an ENOSPC'd
// copy whose retry must first reclaim a dead predecessor's store-sized corpse
// is the motivating regime, and any wiring downstream of a successful take
// (the retention tail, say) is unreachable in exactly that regime.
func Take(ctx context.Context, databaseDir, snapshotsDir, label string) (Snapshot, error) {
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
	// Collection runs before this Take holds the beacon shared — holding it
	// would turn the collector's exclusive probe into a guaranteed self-skip.
	// A collection failure is loud but must not fail the take: the snapshot
	// this call was asked for is still mintable, and the residue stays for
	// the next attempt (same shape, same rationale, and the same stderr
	// channel as the reconcile prune's post-durable-success demotion in
	// sync_reconcile.go). [LAW:no-silent-failure] loud, but never a false
	// failure of work that can still succeed.
	if collectErr := CollectOrphanedResidue(snapshotsDir); collectErr != nil {
		fmt.Fprintf(os.Stderr, "lit: could not collect orphaned snapshot residue (the take proceeds; collection retries next take): %v\n", collectErr)
	}
	releaseBeacon, acquired, err := filelock.Acquire(ctx, producerBeaconPath(snapshotsDir), false, producerBeaconRetryAttempts, producerBeaconRetryDelay)
	if err != nil {
		return Snapshot{}, fmt.Errorf("acquire snapshot producer beacon: %w", err)
	}
	if !acquired {
		return Snapshot{}, fmt.Errorf("dbsnapshot: residue collection is holding the snapshots directory at %s and did not finish within its budget; retry: %w", snapshotsDir, ErrSnapshotsBusy)
	}
	// LIFO with the .reserve removal below: the beacon outlives every producer
	// artifact, so a collector can never classify a live Take's paths as dead.
	// A failed release only defers residue collection until this process
	// exits (the kernel drops the hold then regardless), so it is demoted to
	// the same stderr channel as a collection failure rather than converting
	// a possibly-already-durable snapshot into a reported failure.
	defer func() {
		if relErr := releaseBeacon(); relErr != nil {
			fmt.Fprintf(os.Stderr, "lit: could not release snapshot producer beacon (residue collection defers until this process exits): %v\n", relErr)
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
		reservePath := finalPath + reserveSuffix
		tmpPath := finalPath + tmpSuffix
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
	// Residue collection deliberately does NOT ride this boundary: every
	// prune call site sits downstream of a successful Take, which makes a
	// prune-side sweep unreachable in the disk-full regime where reclaiming
	// a dead predecessor's corpse is the whole point, and a collection
	// failure here would disable retention for every producer at once.
	// Collection lives at Take's entry instead.
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

// The name suffixes the producer machinery owns: an in-flight copy (.tmp), a
// reservation claim (.reserve), and a corpse the collector has condemned
// (.condemned). No legal snapshot name can carry one — sanitizeLabel maps '.'
// out of every label — so the suffix namespace is disjoint from snapshot
// names by construction, not by filtering discipline.
//
// [LAW:one-source-of-truth] reserveSnapshotPaths mints .tmp/.reserve names
// and condemnResidue mints .condemned names from these same constants the
// predicates below read.
const (
	tmpSuffix       = ".tmp"
	reserveSuffix   = ".reserve"
	condemnedSuffix = ".condemned"
)

var producerArtifactSuffixes = []string{tmpSuffix, reserveSuffix, condemnedSuffix}

// IsProducerArtifactName reports whether name sits in the producer-artifact
// suffix namespace. This is the broad REJECTION predicate: any such name is
// refused as a snapshot (List, Restore validation) regardless of its head.
// Destruction uses the deliberately narrower isCollectibleResidue — reject
// broadly, delete narrowly. Exported so tests outside the package can assert
// "no producer artifact was stranded" against the same definition.
func IsProducerArtifactName(name string) bool {
	for _, suffix := range producerArtifactSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// isCollectibleResidue reports whether the collector may destroy the entry:
// a .tmp/.reserve whose remaining head has exactly the <unix-ns>[-<label>]
// shape reserveSnapshotPaths mints, or a .condemned corpse whose full name
// has exactly the shape condemnResidue mints from those. A foreign
// "backup.tmp" — or a foreign "backup.condemned" — an operator parked in
// the directory fails the shape check and is untouchable: every
// non-collector consumer of this directory treats unrecognized names as
// inert, and the one destructive actor must not be the exception. The
// suffix alone is never provenance; lit does not own the directory's
// namespace, only the shapes it provably minted. [LAW:parse-dont-validate]
func isCollectibleResidue(name string) bool {
	if isCollectorCondemnedName(name) {
		return true
	}
	return isCollectibleArtifactName(name)
}

// isCollectibleArtifactName reports whether name is a producer artifact the
// collector may condemn: a .tmp/.reserve suffix over a head with exactly the
// <unix-ns>[-<label>] shape reserveSnapshotPaths mints.
func isCollectibleArtifactName(name string) bool {
	for _, suffix := range []string{tmpSuffix, reserveSuffix} {
		if head, ok := strings.CutSuffix(name, suffix); ok {
			_, parses := parseName(head)
			return parses
		}
	}
	return false
}

// isCollectorCondemnedName reports whether name has exactly the shape
// condemnResidue mints: a collectible .tmp/.reserve artifact name, then a
// "." + positive all-digit nanosecond stamp, then ".condemned".
func isCollectorCondemnedName(name string) bool {
	head, ok := strings.CutSuffix(name, condemnedSuffix)
	if !ok {
		return false
	}
	dot := strings.LastIndexByte(head, '.')
	if dot < 0 {
		return false
	}
	if _, ok := parsePositiveDigits(head[dot+1:]); !ok {
		return false
	}
	return isCollectibleArtifactName(head[:dot])
}

// parseName is the one predicate for "is this a snapshot name". Rejecting
// producer artifacts here (not just in List's loop) means every consumer —
// List, validateSnapshotName, and through it Restore — refuses them from one
// source; previously Restore would accept a labeled ".tmp" leftover ("<ns>-
// <label>.tmp" parses as <ns>) and install a torn partial copy as the
// database. [LAW:one-source-of-truth]
func parseName(name string) (time.Time, bool) {
	if IsProducerArtifactName(name) {
		return time.Time{}, false
	}
	head, label, dashed := name, "", false
	if idx := strings.IndexByte(name, '-'); idx >= 0 {
		head, label, dashed = name[:idx], name[idx+1:], true
	}
	ns, ok := parsePositiveDigits(head)
	if !ok {
		return time.Time{}, false
	}
	if dashed && !isMintableLabel(label) {
		return time.Time{}, false
	}
	return time.Unix(0, ns).UTC(), true
}

// parsePositiveDigits parses s as a positive int64 minted by
// strconv.FormatInt — the round-trip check rejects what ParseInt would
// tolerate but no lit producer ever writes: a sign prefix, leading zeros.
// The sign hole was live: "+123.tmp" classified as lit-minted residue and
// was destroyed.
func parsePositiveDigits(s string) (int64, bool) {
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ns <= 0 || strconv.FormatInt(ns, 10) != s {
		return 0, false
	}
	return ns, true
}

// isMintableLabel reports whether label is a shape sanitizeLabel can emit:
// non-empty, drawn from the [A-Za-z0-9_-] alphabet, not dash-terminated at
// either end. Deliberately no length bound — labels minted by pre-cap
// binaries exist on disk and must stay listable and restorable, so the
// parser accepts the union of shapes lit ever minted while maxLabelBytes
// bounds only what it mints today.
func isMintableLabel(label string) bool {
	if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// maxLabelBytes bounds the sanitized label so every name minted FROM a
// snapshot name also fits a 255-byte NAME_MAX: the worst case is the
// condemnation rename, <ns>-<label>.reserve.<ns>.condemned — 19+1 (head) +
// 8 (".reserve") + 20 (".<ns>") + 10 (".condemned") = 58 bytes of frame, so
// 128 label bytes leaves a wide margin. Without the cap, a killed Take with
// a near-NAME_MAX label left residue whose condemnation rename could never
// succeed (ENAMETOOLONG), stranding the corpse for every later collection.
const maxLabelBytes = 128

// sanitizeLabel is a lossy normalizer, not a validator: it maps illegal
// runes to '-', trims, and truncates to maxLabelBytes, so any input yields
// a legal (possibly empty) label rather than an error.
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
	clean := b.String()
	if len(clean) > maxLabelBytes {
		clean = clean[:maxLabelBytes]
	}
	return strings.Trim(clean, "-")
}

// isDoltJournalLockRel reports whether rel — a path relative to the database
// root being copied — is a Dolt journal LOCK file: <database>/.dolt/noms/LOCK.
// The suffix shape is the whole definition; any path of that shape under a
// Dolt storage directory is that database's journal lock, whatever the
// database is named.
func isDoltJournalLockRel(rel string) bool {
	return strings.HasSuffix(filepath.ToSlash(rel), "/.dolt/noms/LOCK")
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
			// Dolt's journal LOCK file is the one entry the copy omits: it is
			// contentless (the lock lives on holders' open file descriptions,
			// never in bytes) and Dolt recreates it at every engine open, so
			// a restored snapshot neither needs nor misses it — while on
			// Windows the snapshot's own journal hold is a MANDATORY
			// LockFileEx, and reading the file through a second handle here
			// would either fail the copy outright or release the hold
			// mid-walk, reopening the tear window the hold exists to close.
			// (Darwin's single-syscall Clonefile fast path may still carry
			// the file; under POSIX advisory flock that is inert.)
			if isDoltJournalLockRel(rel) {
				return nil
			}
			return copyFile(ctx, srcPath, dstPath)
		default:
			return fmt.Errorf("dbsnapshot: unsupported file type at %s: %v", srcPath, d.Type())
		}
	})
}

// plainFileCopy is the universal fallback: open src, create dst with the
// source's perm bits, chunked copy. The destination's Close error is part of
// the copy's outcome, not discarded via defer: on write-back-at-close
// filesystems (NFS commit-on-close, delayed-allocation ENOSPC) a failed final
// flush is the only signal the file is truncated, and swallowing it would let
// Take rename a torn copy into the listing as a restorable snapshot.
// [LAW:no-silent-failure]
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
	if err := copyWithContext(ctx, dstF, srcF); err != nil {
		_ = dstF.Close()
		return err
	}
	// OpenFile's mode is filtered by umask; Chmod forces exact source perms.
	if err := dstF.Chmod(info.Mode().Perm()); err != nil {
		_ = dstF.Close()
		return err
	}
	return dstF.Close()
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
