package syncfile

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func TestWriteAndReadAtomicExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links", "export.json")
	export := model.Export{
		Version:     1,
		WorkspaceID: "workspace-test",
		ExportedAt:  time.Now().UTC(),
		Issues: []model.Issue{{
			ID:        "issue-1",
			Title:     "Renderer cleanup",
			Status:    "open",
			Priority:  1,
			IssueType: "task",
			Labels:    []string{"renderer"},
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}},
	}
	hash, err := WriteAtomic(path, export)
	if err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	readExport, readHash, err := Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if hash != readHash {
		t.Fatalf("hash mismatch %q != %q", hash, readHash)
	}
	if len(readExport.Issues) != 1 || readExport.Issues[0].ID != "issue-1" {
		t.Fatalf("readExport = %#v", readExport)
	}
}
