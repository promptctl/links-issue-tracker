package backup

import (
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
)

// Budget is how many snapshots a workspace keeps: the default of `lit backup
// create --keep`, and what every restore prunes to. [LAW:one-source-of-truth]
const Budget = 20

type Snapshot struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Size    int64     `json:"size"`
}

// Taken is the snapshot Create wrote, and whether a snapshot of the same
// content already stood at its name.
type Taken struct {
	Snapshot
	Refreshed bool
}

// Create writes export as the snapshot named by its content digest, the one
// restore.digest records: exported_at is left out, so two takes of an unchanged
// store share a name. Content the set already holds lands on that name and
// replaces the older take, which makes it the newest. The set holds each
// content once, whoever writes and in whatever order, and a path handed out
// earlier still holds its content. export must come from a store
// (syncfile.ContentHash). [LAW:one-source-of-truth]
// [LAW:dataflow-not-control-flow]
func Create(storageDir string, export model.Export) (Taken, error) {
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

func Prune(storageDir string, keep int) error {
	if keep <= 0 {
		return fmt.Errorf("keep must be > 0")
	}
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
