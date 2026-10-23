package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/designstore"
)

// The store's size ceiling, and how the store holds it.
//
// A store is four parts on disk, each a directory with one owner: the chunk
// store (current state, the history the window keeps, garbage not yet
// collected), the temp table files Dolt stages pushes and fetches in, the
// recovery snapshots, and the remote cache. The ceiling governs the first
// three. The remote cache is measured and reported but left out: its floor is
// the remote's reachable data, which no local action lowers, so counting it
// would fire one checkout's ceiling on another checkout's writes, and its one
// local defense, eviction, is undone by the next fetch. The remote's history
// bound and the ref sweep already hold it.
//
// Of the governed parts, only the automatic recovery snapshots have a size
// that is a policy choice: compaction already bounds the chunk store's
// garbage on its own signals (compaction.go), the window bounds its history,
// a write open collects dead temp files (temp_table_files.go), and the user's
// own snapshots are theirs. So the ceiling decides how many automatic
// snapshots are kept, and a store whose other parts alone exceed it has
// nothing left lit may reclaim — the case doctor names.

// The ceiling is derived, not chosen. It is room for a design-size store —
// its rows, the history the default window keeps, and a journal at the size
// that makes a shallow pass due — plus one recovery snapshot of that store
// per automatic kind, which is the newest of each that retention never
// retires, plus the user snapshots the default snapshot.retention_budget
// keeps, which retention never touches. The design-size store's rows and
// commits are costed at designstore's allowances, its measured figures plus
// stated headroom, so a store whose re-measured figures stay within them is a
// store this ceiling holds; the nightly lane's `perfbench --sizes design`
// fails when one does not. [LAW:one-source-of-truth]
const (
	// designHistoryCommits is the default window's re_root_at
	// (sync.history_bound.re_root_at): the most commits a bounded history
	// holds before a push re-roots it.
	designHistoryCommits = 2_000
	// designUserSnapshots is snapshot.retention_budget's default: the user
	// snapshots a store on default settings keeps, each a copy of the store.
	designUserSnapshots = 5

	storeSizeCeiling = (designstore.CompactedBytesAllowance + designHistoryCommits*designstore.CommitBytesAllowance + journalDueBytes) * (1 + int64(len(automaticSnapshotLabels)) + designUserSnapshots)

	// automaticSnapshotsPerKind is the most snapshots of one automatic kind
	// kept however much room the ceiling leaves: past ten recovery points
	// for one kind of operation, an eleventh restores nothing a person would
	// reach for.
	automaticSnapshotsPerKind = 10
)

// automaticSnapshotLabels are the kinds of recovery snapshot lit takes on its
// own, by the label each kind's producer stamps. Every other snapshot in the
// directory is the user's. An array, so the ceiling above can count it.
var automaticSnapshotLabels = [...]string{migrationSnapshotLabel, reconcileSnapshotLabel, downgradeSnapshotLabel}

// automaticSnapshotKind names the automatic kind a snapshot name was stamped
// with, or reports that it was not stamped by any.
func automaticSnapshotKind(name string) (string, bool) {
	for _, label := range automaticSnapshotLabels {
		if isStampedSnapshotName(name, label) {
			return label, true
		}
	}
	return "", false
}

// IsAutomaticSnapshotName reports whether name is a recovery snapshot lit took
// on its own, so a reader outside this package tells lit's snapshots from the
// user's by the one list of kinds above. [LAW:one-source-of-truth]
func IsAutomaticSnapshotName(name string) bool {
	_, ok := automaticSnapshotKind(name)
	return ok
}

// footprint is the store at a doltRootDir on disk, part by part. It is the
// engine's own reading, with the automatic snapshots itemized because
// retention decides on each one; contract renders it for a reader of
// storage.Footprint.
type footprint struct {
	chunkStore     int64
	tempTableFiles int64
	remoteCache    int64
	// automatic is every automatic recovery snapshot, newest first.
	automatic []sizedSnapshot
	// otherSnapshots is everything else in the snapshots directory: the
	// user's own snapshots, and any take still in flight.
	otherSnapshots int64
}

// sizedSnapshot is one automatic recovery snapshot with its kind and size.
type sizedSnapshot struct {
	dbsnapshot.Snapshot
	kind  string
	bytes int64
}

// measureFootprint walks every part of the store rooted at doltRootDir. It
// reads the filesystem only, with no engine and no lock of its own. It walks
// every file, so it is never on the push path: it runs in the backstop, in
// doctor, and after a snapshot take, which has just copied the whole store.
func measureFootprint(doltRootDir string) (footprint, error) {
	var f footprint
	var err error
	if f.chunkStore, err = chunkStoreBytes(doltRootDir); err != nil {
		return footprint{}, err
	}
	if f.tempTableFiles, err = bytesUnder(TempTableFilesDir(doltRootDir)); err != nil {
		return footprint{}, fmt.Errorf("measure temp table files: %w", err)
	}
	if f.remoteCache, err = bytesUnder(remoteCacheBaseFor(doltRootDir)); err != nil {
		return footprint{}, fmt.Errorf("measure the remote cache: %w", err)
	}
	if f.automatic, f.otherSnapshots, err = measureSnapshots(doltRootDir); err != nil {
		return footprint{}, err
	}
	return f, nil
}

// measureSnapshots sizes every entry in the snapshots directory once: the
// automatic snapshots each, newest first, and the rest as one sum.
func measureSnapshots(doltRootDir string) ([]sizedSnapshot, int64, error) {
	listed, err := dbsnapshot.List(doltRootDir)
	if err != nil {
		return nil, 0, err
	}
	dir := dbsnapshot.DirFor(doltRootDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read snapshots dir: %w", err)
	}
	sizes := make(map[string]int64, len(entries))
	var total int64
	for _, entry := range entries {
		n, err := bytesUnder(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, 0, fmt.Errorf("measure snapshot %s: %w", entry.Name(), err)
		}
		sizes[entry.Name()] = n
		total += n
	}
	var automatic []sizedSnapshot
	for _, snap := range listed {
		if kind, ok := automaticSnapshotKind(snap.Name); ok {
			automatic = append(automatic, sizedSnapshot{Snapshot: snap, kind: kind, bytes: sizes[snap.Name]})
			total -= sizes[snap.Name]
		}
	}
	return automatic, total, nil
}

// contract renders the footprint for readers of the storage contract, under
// ceiling.
func (f footprint) contract(ceiling int64) storage.Footprint {
	var automatic int64
	for _, snap := range f.automatic {
		automatic += snap.bytes
	}
	return storage.Footprint{
		Ceiling: ceiling,
		Components: []storage.FootprintComponent{
			{Name: "chunk store", Bytes: f.chunkStore, Governed: true},
			{Name: "temp table files", Bytes: f.tempTableFiles, Governed: true},
			{Name: "automatic snapshots", Bytes: automatic, Governed: true},
			{Name: "user snapshots", Bytes: f.otherSnapshots, Governed: true},
			{Name: "remote cache", Bytes: f.remoteCache, Governed: false},
		},
	}
}

// snapshotsToPrune is the one retention rule for automatic recovery
// snapshots: which of f's to retire to hold the store under ceiling.
//
// The newest snapshot of each kind always stays, so the latest recovery point
// for every kind of operation survives however full the store is. The room
// the rest may take is the ceiling less everything retention cannot touch —
// the chunk store, the temp files, the user's snapshots — and less those
// newest ones. Newest first, each further snapshot stays while its kind holds
// fewer than automaticSnapshotsPerKind and it fits the room left; once one
// does not fit, every older one goes too, so a store never keeps an old
// recovery point it dropped a newer one for. How many go is a function of how
// far over the store stands: one byte over retires the oldest snapshot, not
// all but one of each kind. [LAW:dataflow-not-control-flow]
//
// Pure, so the rule is exercised by handing it a footprint.
// [LAW:effects-at-boundaries]
func snapshotsToPrune(f footprint, ceiling int64) []sizedSnapshot {
	room := ceiling - f.chunkStore - f.tempTableFiles - f.otherSnapshots
	held := map[string]int{}
	newest := map[string]bool{}
	for _, snap := range f.automatic {
		if held[snap.kind] == 0 {
			held[snap.kind] = 1
			newest[snap.Name] = true
			room -= snap.bytes
		}
	}
	var prune []sizedSnapshot
	full := false
	for _, snap := range f.automatic {
		if newest[snap.Name] {
			continue
		}
		full = full || snap.bytes > room
		if full || held[snap.kind] >= automaticSnapshotsPerKind {
			prune = append(prune, snap)
			continue
		}
		held[snap.kind]++
		room -= snap.bytes
	}
	return prune
}

// ceilingHold is what holding the ceiling did: the snapshots it retired, and
// the store's footprint once they were gone.
type ceilingHold struct {
	pruned    []string
	footprint storage.Footprint
}

// pruneAutomaticSnapshots holds the store under its ceiling by retiring the
// automatic recovery snapshots snapshotsToPrune names. It is the one
// enforcer of automatic-snapshot retention: every producer of an automatic
// snapshot calls it after its take, and the backstop calls it on its probe,
// so there is one rule and one number for how many are kept.
// [LAW:single-enforcer]
//
// It takes no lock of its own, as the per-kind prunes it replaced took none.
// A producer calls it inside the commit lock it took its snapshot under; the
// backstop calls it holding Dolt's LOCK through its write engine, which every
// producer's engine also needs, so no take runs beside it. The newest of each
// kind is never retired, which keeps the snapshot a producer just took out of
// reach whoever is pruning.
//
// It stops between removals once ctx is done, so the caller's deadline bounds
// the removals too — each is a whole copy of the store — and whatever it left
// is retired by the next prune.
//
// The footprint it returns is measured once, before the removals, less what
// was removed — the store as the prune left it, without a second walk.
func (s *Store) pruneAutomaticSnapshots(ctx context.Context) (ceilingHold, error) {
	f, err := measureFootprint(s.doltRootDir)
	if err != nil {
		return ceilingHold{}, fmt.Errorf("measure store footprint: %w", err)
	}
	var hold ceilingHold
	gone := map[string]bool{}
	for _, snap := range snapshotsToPrune(f, s.sizeCeiling) {
		if err = ctx.Err(); err != nil {
			break
		}
		if err = dbsnapshot.Remove(snap.Snapshot); err != nil {
			break
		}
		hold.pruned = append(hold.pruned, snap.Name)
		gone[snap.Name] = true
	}
	kept := f.automatic[:0:0]
	for _, snap := range f.automatic {
		if !gone[snap.Name] {
			kept = append(kept, snap)
		}
	}
	f.automatic = kept
	hold.footprint = f.contract(s.sizeCeiling)
	return hold, err
}

// footprint measures the store this handle is open on, under its ceiling.
func (s *Store) footprint() (storage.Footprint, error) {
	f, err := measureFootprint(s.doltRootDir)
	if err != nil {
		return storage.Footprint{}, fmt.Errorf("measure store footprint: %w", err)
	}
	return f.contract(s.sizeCeiling), nil
}

// overCeilingWarning is how a store over its ceiling reads in a health
// report: what it holds against what it may, and the part to look at.
func overCeilingWarning(fp storage.Footprint) (string, bool) {
	over, ok := fp.Over()
	if !ok {
		return "", false
	}
	return fmt.Sprintf("store over its size ceiling: %s against %s; the largest part is the %s at %s",
		storage.FormatBytes(fp.GovernedBytes()), storage.FormatBytes(fp.Ceiling), over.Largest.Name, storage.FormatBytes(over.Largest.Bytes)), true
}
