package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestWriteRehomedNamesEachMoveAndIsSilentWithoutOne(t *testing.T) {
	var none bytes.Buffer
	if err := writeRehomed(&none, "the backup", nil); err != nil {
		t.Fatalf("writeRehomed(nil) error = %v", err)
	}
	if none.Len() != 0 {
		t.Errorf("writeRehomed(nil) printed %q, want nothing", none.String())
	}

	var out bytes.Buffer
	if err := writeRehomed(&out, "the merged backlog", []storage.Rehomed{
		{IssueID: "acme-core-bb22", Epic: "acme-unfiled-0bf7"},
		{IssueID: "acme-core-cc33", Epic: ""},
	}); err != nil {
		t.Fatalf("writeRehomed error = %v", err)
	}
	for _, want := range []string{
		"moved 2 ticket(s) the merged backlog held outside an epic:",
		"acme-core-bb22", "into", "acme-unfiled-0bf7",
		"acme-core-cc33", "to the top level",
		"lit parent set --child <id> --parent <epic-id>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}

	metadata := map[string]string{}
	rehomedMetadata(metadata, nil)
	if metadata["rehomed"] != "0" {
		t.Errorf("metadata rehomed = %q, want %q: zero moves is recorded, not omitted", metadata["rehomed"], "0")
	}
}

// TestBackupRestoreNamesTheTicketsItMovesIntoAnEpic drives `lit backup restore`
// over a backup holding a task with no epic — the shape every backup taken
// before schema v7 holds. The restore lands it inside the unfiled epic, prints
// the move, and records it on the restore's sync trace.
func TestBackupRestoreNamesTheTicketsItMovesIntoAnEpic(t *testing.T) {
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	task := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", "Loose", "--topic", "demo", "--parent", epic))

	created := strings.Fields(runCLIInDir(t, repo, "backup", "create"))
	if len(created) != 2 {
		t.Fatalf("backup create printed %v, want a name and a path", created)
	}
	payload, err := os.ReadFile(created[1])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var backup map[string]any
	if err := json.Unmarshal(payload, &backup); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	backup["relations"] = []any{}
	edited, err := json.Marshal(backup)
	if err != nil {
		t.Fatalf("encode backup: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pre-containment.json")
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	out := runCLIInDir(t, repo, "backup", "restore", "--path", path)
	if !strings.Contains(out, "moved 1 ticket(s) the backup held outside an epic:") || !strings.Contains(out, task) {
		t.Errorf("restore output %q does not name the move of %s", out, task)
	}
	shown := runCLIInDir(t, repo, "show", task)
	if !strings.Contains(shown, "Unfiled tickets") {
		t.Errorf("show %s = %q, want it inside the unfiled epic", task, shown)
	}

	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	var restore *syncTraceRecord
	for _, record := range readSyncTraceRecords(t, ws) {
		if record.Command == "lit backup restore" {
			restore = &record
		}
	}
	if restore == nil {
		t.Fatalf("no sync trace record for lit backup restore")
	}
	if restore.Metadata["rehomed"] != "1" || !strings.Contains(restore.Metadata["rehomed_moves"], task+" into ") {
		t.Errorf("restore trace metadata = %v, want the one move of %s", restore.Metadata, task)
	}
}
