package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
	"github.com/promptctl/primitives/filelock"
)

// Budget is how many snapshots a workspace keeps: the default of `lit backup
// create --keep`, and what every restore prunes to. [LAW:one-source-of-truth]
const Budget = 20

// ErrBusy marks a take that gave up waiting for another process's hold on the
// backups set. A retry once the holder exits succeeds.
var ErrBusy = errors.New("backups busy")

// A hold spans one store export, one durable write of it and a prune, seconds
// on the largest stores, and its holder acquires nothing else, so a holder
// still in front after thirty seconds is stalled rather than busy.
const (
	lockAttempts   = 3000
	lockRetryDelay = 10 * time.Millisecond
)

// lockPath mints the backups set's lock in the storage dir beside the store's
// locks. It is the innermost lock: its holder waits on no other.
func lockPath(storageDir string) string {
	return filepath.Join(storageDir, ".links-backups.lock")
}

// dir is the set's directory in the storage dir.
func dir(storageDir string) string {
	return filepath.Join(storageDir, "backups")
}

// snapshotExt ends every snapshot's name: create names one <digest>.json.
const snapshotExt = ".json"

// isSnapshotName reports whether a file of the set's directory named name is
// a snapshot.
func isSnapshotName(name string) bool {
	return strings.HasSuffix(name, snapshotExt)
}

type Snapshot struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Size    int64     `json:"size"`
}

// Taken is what a take that held the set's lock did: how long it waited for
// the lock, how many killed takes' temp files it removed and how many it could
// not, and the snapshot it wrote, nil when its read or its create failed. [LAW:nothing-unseen] Each fact
// is here exactly when the step that produces it ran, so a removal that never
// ran cannot read as one that found nothing.
type Taken struct {
	Waited         time.Duration
	ResidueRemoved int
	ResidueStuck   int
	Written        *Written
}

// Written is the snapshot a take wrote, whether a snapshot of the same content
// already stood at its name, and how many older snapshots its prune removed.
type Written struct {
	Snapshot
	Refreshed bool
	Pruned    int
}

// Take removes what killed takes left, reads an export and writes it into the
// set, then prunes the set to keep, all under the set's lock.
// [LAW:single-enforcer] It is the one writer of the set: a take of content the
// set holds rewrites a name a concurrent prune may have listed as past keep,
// and a read made before the lock could land after a newer one and stand as
// the newest. A nil Taken means the take never held the lock; a take that held
// it returns its Taken beside any error.
func Take(ctx context.Context, storageDir string, keep int, read func() (model.Export, error)) (*Taken, error) {
	if keep <= 0 {
		return nil, fmt.Errorf("keep must be > 0")
	}
	path := lockPath(storageDir)
	start := time.Now()
	release, acquired, err := filelock.Acquire(ctx, path, true, lockAttempts, lockRetryDelay)
	waited := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("lock backups: %w", err)
	}
	if !acquired {
		return nil, fmt.Errorf("%w: another lit process held %s for over %s; retry once it exits",
			ErrBusy, path, time.Duration(lockAttempts)*lockRetryDelay)
	}
	taken, err := takeHeld(storageDir, keep, read)
	taken.Waited = waited
	if relErr := release(); relErr != nil {
		return &taken, errors.Join(err, fmt.Errorf("release backups lock: %w", relErr))
	}
	return &taken, err
}

// takeHeld removes the residue first, before the write that needs the room:
// on a full disk a take whose write fails reaches no later step, so a removal
// after it would never run in the one case it matters most. A removal that
// fails is reported beside the write, never in place of it
// [LAW:dataflow-not-control-flow]: one temp file that will not go must not
// cost the set its next snapshot.
func takeHeld(storageDir string, keep int, read func() (model.Export, error)) (Taken, error) {
	removed, stuck, residueErr := removeResidue(dir(storageDir))
	written, err := write(storageDir, keep, read)
	return Taken{ResidueRemoved: removed, ResidueStuck: stuck, Written: written}, errors.Join(residueErr, err)
}

// write reads an export, writes it into the set and prunes the set to keep. A
// nil Written means its read or its create failed; a prune that fails after
// the create returns the Written with an error naming it.
func write(storageDir string, keep int, read func() (model.Export, error)) (*Written, error) {
	export, err := read()
	if err != nil {
		return nil, err
	}
	written, err := create(storageDir, export)
	if err != nil {
		return nil, err
	}
	written.Pruned, err = prune(storageDir, keep)
	if err != nil {
		return &written, fmt.Errorf("prune backups after writing %s: %w", written.Path, err)
	}
	return &written, nil
}

// removeResidue removes the temp files of takes killed mid-write (a SIGKILL
// or a power loss between atomicfile's temp file and its rename) and returns
// how many it removed and how many it could not, beside an error naming each
// of those. Each is the
// size of an export, under a name no later take reuses, so nothing else would
// ever reclaim it.
//
// Only a take holding the set's lock calls it. The take is the set's one
// writer, so under the lock no write into the directory is running, and the
// kernel drops a killed take's hold with its process: the lock is the proof
// that every temp file here is dead, with no age to guess at. A take by lit
// v0.23 or older holds no such lock. On POSIX a temp of one still running is
// removed too, and its rename then fails loudly; on Windows, where the writer's
// open handle forbids the removal, this take reports the temp it could not
// remove.
func removeResidue(backupsDir string) (removed, stuck int, err error) {
	entries, err := os.ReadDir(backupsDir)
	if errors.Is(err, fs.ErrNotExist) {
		// A set never written to holds nothing.
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read backups dir: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		// atomicfile's temp files are files; a directory under such a name is
		// someone else's.
		target, ok := atomicfile.TempTarget(entry.Name())
		if entry.IsDir() || !ok || !isSnapshotName(target) {
			continue
		}
		path := filepath.Join(backupsDir, entry.Name())
		if err := os.Remove(path); err != nil {
			errs = append(errs, fmt.Errorf("remove a killed backup's temp file %s: %w", path, err))
			continue
		}
		removed++
	}
	return removed, len(errs), errors.Join(errs...)
}

// create writes export as the snapshot named by its content digest, the one
// restore.digest records: exported_at is left out, so two takes of an unchanged
// store share a name. Content the set already holds lands on that name and
// replaces the older take, which makes it the newest. The set holds each
// content once, and a path handed out earlier still holds its content. export
// must come from a store (syncfile.ContentHash). [LAW:one-source-of-truth]
// [LAW:dataflow-not-control-flow]
func create(storageDir string, export model.Export) (Written, error) {
	backupsDir := dir(storageDir)
	if err := os.MkdirAll(backupsDir, 0o755); err != nil {
		return Written{}, fmt.Errorf("create backups dir: %w", err)
	}
	digest, err := syncfile.ContentHash(export)
	if err != nil {
		return Written{}, err
	}
	path := filepath.Join(backupsDir, digest+snapshotExt)
	// Whether the name was already taken is recorded, never acted on: the
	// write below is the same either way.
	_, err = os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Written{}, fmt.Errorf("stat backup snapshot: %w", err)
	}
	refreshed := err == nil
	if _, err := syncfile.WriteAtomic(path, export); err != nil {
		return Written{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Written{}, fmt.Errorf("stat backup snapshot: %w", err)
	}
	return Written{
		Snapshot: Snapshot{
			Path:    path,
			Name:    info.Name(),
			Created: info.ModTime().UTC(),
			Size:    info.Size(),
		},
		Refreshed: refreshed,
	}, nil
}

func List(storageDir string) ([]Snapshot, error) {
	backupsDir := dir(storageDir)
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Snapshot{}, nil
		}
		return nil, fmt.Errorf("read backups dir: %w", err)
	}
	snapshots := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isSnapshotName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, Snapshot{
			Path:    filepath.Join(backupsDir, entry.Name()),
			Name:    entry.Name(),
			Created: info.ModTime().UTC(),
			Size:    info.Size(),
		})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Created.After(snapshots[j].Created) })
	return snapshots, nil
}

// prune removes every snapshot past the newest keep and returns how many it
// removed, stopping at the first removal that fails.
func prune(storageDir string, keep int) (int, error) {
	snapshots, err := List(storageDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, snapshot := range snapshots[min(keep, len(snapshots)):] {
		if err := os.Remove(snapshot.Path); err != nil {
			return removed, fmt.Errorf("remove backup %s: %w", snapshot.Path, err)
		}
		removed++
	}
	return removed, nil
}

func Latest(storageDir string) (*Snapshot, error) {
	snapshots, err := List(storageDir)
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, nil
	}
	latest := snapshots[0]
	return &latest, nil
}
