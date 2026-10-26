package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
)

func TestCreateListLatestAndPrune(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{
		Version:     1,
		WorkspaceID: "ws-test",
		ExportedAt:  time.Now().UTC(),
	}

	first, err := Create(storageDir, export)
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	if filepath.Dir(first.Path) == "" || first.Name == "" {
		t.Fatalf("Create(first) returned invalid snapshot metadata: %+v", first)
	}

	export.WorkspaceID = "ws-test-2"
	second, err := Create(storageDir, export)
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}

	export.WorkspaceID = "ws-test-3"
	third, err := Create(storageDir, export)
	if err != nil {
		t.Fatalf("Create(third): %v", err)
	}

	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}

	latest, err := Latest(storageDir)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == nil {
		t.Fatalf("Latest returned nil")
	}
	if latest.Path != list[0].Path {
		t.Fatalf("Latest path = %q, want %q", latest.Path, list[0].Path)
	}

	if err := Prune(storageDir, 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	listAfterPrune, err := List(storageDir)
	if err != nil {
		t.Fatalf("List after prune: %v", err)
	}
	if len(listAfterPrune) != 2 {
		t.Fatalf("List after prune len = %d, want 2", len(listAfterPrune))
	}

	paths := map[string]bool{
		first.Path:  false,
		second.Path: false,
		third.Path:  false,
	}
	for _, snapshot := range listAfterPrune {
		paths[snapshot.Path] = true
	}
	if paths[first.Path] {
		t.Fatalf("oldest snapshot was not pruned")
	}
	if !paths[second.Path] || !paths[third.Path] {
		t.Fatalf("expected newest snapshots to remain after prune")
	}
}

func TestPruneRejectsNonPositiveKeep(t *testing.T) {
	t.Parallel()

	if err := Prune(t.TempDir(), 0); err == nil {
		t.Fatalf("Prune(keep=0) expected error")
	}
}

// A take of content the set already holds lands on the snapshot that holds it,
// so repeated takes of an unchanged store leave one file, the newest, holding
// the newer take.
func TestCreateHoldsEachContentOnce(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{Version: 1, WorkspaceID: "ws-test", ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, err := Create(storageDir, export)
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	if first.Refreshed {
		t.Errorf("Create(first).Refreshed = true, want false: the set held nothing")
	}
	other, err := Create(storageDir, model.Export{Version: 1, WorkspaceID: "ws-other", ExportedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Create(other): %v", err)
	}
	if other.Refreshed || other.Path == first.Path {
		t.Fatalf("Create(other) = %+v, want a new snapshot beside %s", other, first.Path)
	}

	export.ExportedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	again, err := Create(storageDir, export)
	if err != nil {
		t.Fatalf("Create(again): %v", err)
	}
	if !again.Refreshed || again.Path != first.Path {
		t.Fatalf("Create(again) = %+v, want %s refreshed", again, first.Path)
	}
	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Path != first.Path {
		t.Fatalf("List = %+v, want 2 snapshots with %s newest", list, first.Path)
	}
	held, _, err := syncfile.Read(first.Path)
	if err != nil {
		t.Fatalf("read %s: %v", first.Path, err)
	}
	if !held.ExportedAt.Equal(export.ExportedAt) {
		t.Errorf("snapshot exported_at = %v, want the newer take's %v", held.ExportedAt, export.ExportedAt)
	}
}

// A snapshot an older lit named by its time is listed and pruned by age with
// the rest, and never stands for content a new take holds.
func TestPruneAgesOutAnOlderLitsSnapshot(t *testing.T) {
	t.Parallel()

	storageDir := t.TempDir()
	export := model.Export{Version: 1, WorkspaceID: "ws-test", ExportedAt: time.Now().UTC()}
	if _, err := Create(storageDir, export); err != nil {
		t.Fatalf("Create: %v", err)
	}
	legacy := filepath.Join(storageDir, "backups", "20260101-000000.000000000.json")
	if _, err := syncfile.WriteAtomic(legacy, export); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(legacy, old, old); err != nil {
		t.Fatalf("age legacy snapshot: %v", err)
	}
	taken, err := Create(storageDir, export)
	if err != nil {
		t.Fatalf("Create(again): %v", err)
	}
	if taken.Path == legacy {
		t.Fatalf("Create landed on the older lit's snapshot %s", legacy)
	}
	list, err := List(storageDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[1].Path != legacy {
		t.Fatalf("List = %+v, want the older lit's snapshot listed last", list)
	}
	if err := Prune(storageDir, 1); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("older lit's snapshot survived a prune past it: stat error = %v", err)
	}
}
