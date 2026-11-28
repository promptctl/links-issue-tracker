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

// A storage dir that is not absolute would put the trace wherever the
// process happens to be standing, so Write refuses it before building or
// writing anything.
func TestWriteRefusesAStorageDirThatIsNotAbsolute(t *testing.T) {
	for _, storageDir := range []string{"", "relative-storage"} {
		_, _, err := Write(storageDir, "widgets", "my-trigger", func(string, time.Time) ([]byte, error) {
			t.Fatalf("Write(%q) built a record, want a refusal first", storageDir)
			return nil, nil
		})
		if err == nil {
			t.Fatalf("Write(%q) error = nil, want a refusal", storageDir)
		}
		if _, statErr := os.Stat(filepath.Join(storageDir, "traces")); !os.IsNotExist(statErr) {
			t.Fatalf("Stat(traces under %q) error = %v, want nothing written relative to the working directory", storageDir, statErr)
		}
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
		"show_ticket":              "show-ticket",
		"exit review enter closed": "exit-review-enter-closed",
		"  Git Pre-Push!!  ":       "git-pre-push",
		"":                         "trace",
		"   ":                      "trace",
	}
	for input, want := range cases {
		if got := Slug(input); got != want {
			t.Fatalf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
}
