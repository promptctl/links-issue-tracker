package backup

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
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
