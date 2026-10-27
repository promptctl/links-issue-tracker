package syncfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func issueWithStatus(t *testing.T, issue model.Issue, status model.State) model.Issue {
	t.Helper()
	hydrated, err := model.HydrateStatus(issue, model.StatusView{Value: status})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}
	return hydrated
}

func TestWriteAndReadAtomicExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links", "export.json")
	export := model.Export{
		Version:     1,
		WorkspaceID: "workspace-test",
		ExportedAt:  time.Now().UTC(),
		Issues: []model.Issue{issueWithStatus(t, model.Issue{
			ID:        "issue-1",
			Title:     "Renderer cleanup",
			Priority:  1,
			IssueType: "task",
			Labels:    []string{"renderer"},
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}, model.StateOpen)},
	}
	hash, err := WriteAtomic(path, export)
	if err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	readExport, readHash, err := Decode(payload)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if hash != readHash {
		t.Fatalf("hash mismatch %q != %q", hash, readHash)
	}
	if len(readExport.Issues) != 1 || readExport.Issues[0].ID != "issue-1" {
		t.Fatalf("readExport = %#v", readExport)
	}
}

// Two exports of an unchanged store differ only in when they were taken, and
// their content hashes agree; a changed ticket changes the hash.
func TestContentHashCoversWhatTheExportHoldsNotWhenItWasTaken(t *testing.T) {
	taken := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	export := func(title string, at time.Time) model.Export {
		return model.Export{
			Version:     model.ExportVersion,
			WorkspaceID: "workspace-test",
			ExportedAt:  at,
			Issues: []model.Issue{issueWithStatus(t, model.Issue{
				ID:        "issue-1",
				Title:     title,
				IssueType: "task",
				CreatedAt: taken,
				UpdatedAt: taken,
			}, model.StateOpen)},
		}
	}
	hash := func(e model.Export) string {
		t.Helper()
		h, err := ContentHash(e)
		if err != nil {
			t.Fatalf("ContentHash() error = %v", err)
		}
		return h
	}
	first := hash(export("Before", taken))
	if later := hash(export("Before", taken.Add(time.Hour))); later != first {
		t.Errorf("ContentHash of the same content taken an hour later = %s, want %s", later, first)
	}
	if edited := hash(export("After", taken)); edited == first {
		t.Errorf("ContentHash of an export with a retitled ticket = %s, the same as before the edit", edited)
	}
}

func TestContentHashRefusesUnhydratedIssue(t *testing.T) {
	t.Parallel()
	// ContentHash relies on Issue.MarshalJSON to reject unhydrated values; the
	// hydrator's post-condition keeps this path unreachable from store output,
	// but the JSON boundary still enforces if any other producer slips one in.
	export := model.Export{
		Version:     1,
		WorkspaceID: "ws",
		ExportedAt:  time.Now().UTC(),
		Issues:      []model.Issue{{ID: "unhydrated-x", IssueType: "task"}},
	}
	_, err := ContentHash(export)
	if err == nil || !strings.Contains(err.Error(), "no hydrated lifecycle") {
		t.Fatalf("ContentHash error = %v, want no hydrated lifecycle error", err)
	}
}
