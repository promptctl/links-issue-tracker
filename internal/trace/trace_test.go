package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteWritesUnderKindDirAndStampsIDIntoBuild(t *testing.T) {
	storageDir := t.TempDir()

	type record struct {
		ID   string `json:"id"`
		Note string `json:"note"`
	}

	id, path, err := Write(storageDir, "widgets", "my-trigger", func(id string, recordedAt time.Time) ([]byte, error) {
		if recordedAt.IsZero() {
			t.Fatalf("build called with zero recordedAt")
		}
		return json.Marshal(record{ID: id, Note: "hello"})
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if filepath.Dir(path) != Dir(storageDir, "widgets") {
		t.Fatalf("path dir = %q, want %q", filepath.Dir(path), Dir(storageDir, "widgets"))
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var got record
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.ID != id {
		t.Fatalf("record.ID = %q, want %q", got.ID, id)
	}
	if got.Note != "hello" {
		t.Fatalf("record.Note = %q, want hello", got.Note)
	}
}

func TestWriteRetriesOnFilenameCollision(t *testing.T) {
	storageDir := t.TempDir()
	calls := 0
	_, _, err := Write(storageDir, "widgets", "trigger", func(id string, recordedAt time.Time) ([]byte, error) {
		calls++
		if calls == 1 {
			// Pre-create the file this first attempt will try to claim, forcing a retry.
			path := filepath.Join(Dir(storageDir, "widgets"), id+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}
		return []byte("{}"), nil
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if calls < 2 {
		t.Fatalf("build called %d times, want at least 2 (a retry after the collision)", calls)
	}
}

func TestSlugCanonicalizesAndFallsBackOnEmpty(t *testing.T) {
	cases := map[string]string{
		"work_finished":      "work-finished",
		"  Git Pre-Push!!  ": "git-pre-push",
		"":                   "trace",
		"   ":                "trace",
	}
	for input, want := range cases {
		if got := Slug(input); got != want {
			t.Fatalf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
}
