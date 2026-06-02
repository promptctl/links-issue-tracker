package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
)

type Snapshot struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Size    int64     `json:"size"`
}

func Create(storageDir string, export model.Export) (Snapshot, error) {
	backupsDir := filepath.Join(storageDir, "backups")
	if err := os.MkdirAll(backupsDir, 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create backups dir: %w", err)
	}
	filename := time.Now().UTC().Format("20060102-150405.000000000") + ".json"
	path := filepath.Join(backupsDir, filename)
	if _, err := syncfile.WriteAtomic(path, export); err != nil {
		return Snapshot{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat backup snapshot: %w", err)
	}
	return Snapshot{
		Path:    path,
		Name:    info.Name(),
		Created: info.ModTime().UTC(),
		Size:    info.Size(),
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
