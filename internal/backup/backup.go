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

type Snapshot struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Size    int64     `json:"size"`
}

// Taken is the snapshot a take wrote, and whether a snapshot of the same
// content already stood at its name.
type Taken struct {
	Snapshot
	Refreshed bool
}

// Take reads an export and writes it into the set, then prunes the set to keep,
// all under the set's lock. [LAW:single-enforcer] It is the one writer of the
// set: a take of content the set holds rewrites a name a concurrent prune may
// have listed as past keep, and a read made before the lock could land after a
// newer one and stand as the newest. A nil Taken means nothing was written; a
// prune that fails after the write returns the Taken with an error naming it.
func Take(ctx context.Context, storageDir string, keep int, read func() (model.Export, error)) (*Taken, error) {
	if keep <= 0 {
		return nil, fmt.Errorf("keep must be > 0")
	}
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	path := lockPath(storageDir)
	release, acquired, err := filelock.Acquire(ctx, path, true, lockAttempts, lockRetryDelay)
	if err != nil {
		return nil, fmt.Errorf("lock backups: %w", err)
	}
	if !acquired {
		return nil, fmt.Errorf("%w: another lit process held %s for over %s; retry once it exits",
			ErrBusy, path, time.Duration(lockAttempts)*lockRetryDelay)
	}
	taken, err := takeHeld(storageDir, keep, read)
	if relErr := release(); relErr != nil {
		return taken, errors.Join(err, fmt.Errorf("release backups lock: %w", relErr))
	}
	return taken, err
}

func takeHeld(storageDir string, keep int, read func() (model.Export, error)) (*Taken, error) {
	export, err := read()
	if err != nil {
		return nil, err
	}
	taken, err := create(storageDir, export)
	if err != nil {
		return nil, err
	}
	if err := prune(storageDir, keep); err != nil {
		return &taken, fmt.Errorf("prune backups after writing %s: %w", taken.Path, err)
	}
	return &taken, nil
}

// create writes export as the snapshot named by its content digest, the one
// restore.digest records: exported_at is left out, so two takes of an unchanged
// store share a name. Content the set already holds lands on that name and
// replaces the older take, which makes it the newest. The set holds each
// content once, and a path handed out earlier still holds its content. export
// must come from a store (syncfile.ContentHash). [LAW:one-source-of-truth]
// [LAW:dataflow-not-control-flow]
func create(storageDir string, export model.Export) (Taken, error) {
	backupsDir := filepath.Join(storageDir, "backups")
	if err := os.MkdirAll(backupsDir, 0o755); err != nil {
		return Taken{}, fmt.Errorf("create backups dir: %w", err)
	}
	digest, err := syncfile.ContentHash(export)
	if err != nil {
		return Taken{}, err
	}
	path := filepath.Join(backupsDir, digest+".json")
	// Whether the name was already taken is recorded, never acted on: the
	// write below is the same either way.
	_, err = os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Taken{}, fmt.Errorf("stat backup snapshot: %w", err)
	}
	refreshed := err == nil
	if _, err := syncfile.WriteAtomic(path, export); err != nil {
		return Taken{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Taken{}, fmt.Errorf("stat backup snapshot: %w", err)
	}
	return Taken{
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
	backupsDir := filepath.Join(storageDir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Snapshot{}, nil
		}
		return nil, fmt.Errorf("read backups dir: %w", err)
	}
	snapshots := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
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

func prune(storageDir string, keep int) error {
	snapshots, err := List(storageDir)
	if err != nil {
		return err
	}
	if len(snapshots) <= keep {
		return nil
	}
	for _, snapshot := range snapshots[keep:] {
		if err := os.Remove(snapshot.Path); err != nil {
			return fmt.Errorf("remove backup %s: %w", snapshot.Path, err)
		}
	}
	return nil
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
