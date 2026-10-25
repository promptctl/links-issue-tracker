package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func TestDroppedByIsTheWorkspaceTicketsTheFileDoesNotHold(t *testing.T) {
	export := func(ids ...string) model.Export {
		issues := make([]model.Issue, len(ids))
		for i, id := range ids {
			issues[i] = model.Issue{ID: id}
		}
		return model.Export{Issues: issues}
	}
	dropped := droppedBy(export("a", "b", "c"), export("b", "d"))
	if len(dropped) != 2 || dropped[0].ID != "a" || dropped[1].ID != "c" {
		t.Errorf("droppedBy = %v, want a and c: held now, absent from the file", dropped)
	}
	if dropped := droppedBy(export("a"), export("a", "d")); len(dropped) != 0 {
		t.Errorf("droppedBy = %v, want none: the file holds every ticket the workspace does", dropped)
	}

	var none bytes.Buffer
	if err := writeDropped(&none, nil, "/backups/x.json"); err != nil || none.Len() != 0 {
		t.Errorf("writeDropped(nil) printed %q (%v), want nothing", none.String(), err)
	}
	metadata := map[string]string{}
	absentMetadata(metadata, nil)
	if metadata["absent_from_file"] != "0" {
		t.Errorf("metadata absent_from_file = %q, want %q: zero is recorded, not omitted", metadata["absent_from_file"], "0")
	}
}

// TestBackupRestoreOfAnEditedExportNamesTheTicketsItDestroys drives the misuse
// the report exists for: an agent exports, hand-edits a title, and restores
// the file after another writer made a ticket. The restore names that ticket,
// the backup that still holds it, and the edit path the agent wanted.
func TestBackupRestoreOfAnEditedExportNamesTheTicketsItDestroys(t *testing.T) {
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	edited := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", "Before", "--topic", "demo", "--parent", epic))
	var export map[string]any
	if err := json.Unmarshal([]byte(runCLIInDir(t, repo, "export")), &export); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	for _, issue := range export["issues"].([]any) {
		if fields := issue.(map[string]any); fields["id"] == edited {
			fields["title"] = "After"
		}
	}
	payload, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("encode export: %v", err)
	}
	path := filepath.Join(t.TempDir(), "edited.json")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write export: %v", err)
	}
	later := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", "Made meanwhile", "--topic", "demo", "--parent", epic))

	out := runCLIInDir(t, repo, "backup", "restore", "--path", path)
	for _, want := range []string{"removed 1 ticket(s) the workspace held and the file does not", "the pre-restore backup ", later, "Made meanwhile", "lit ls --format yaml", "lit import --path"} {
		if !strings.Contains(out, want) {
			t.Errorf("restore output %q lacks %q", out, want)
		}
	}
	if restore := restoreTraceRecord(t, repo); restore.Metadata["absent_from_file"] != "1" || restore.Metadata["absent_from_file_ids"] != later {
		t.Errorf("restore trace metadata = %v, want absent_from_file=1 (%s)", restore.Metadata, later)
	}

	_, rest, found := strings.Cut(out, "the pre-restore backup ")
	snapshot, _, _ := strings.Cut(rest, " still holds them")
	held, err := os.ReadFile(snapshot)
	if !found || err != nil || !strings.Contains(string(held), later) {
		t.Errorf("the pre-restore backup %q named in %q does not hold %s (read error %v)", snapshot, out, later, err)
	}
}

// A restore that drops nothing traces a counted zero, and one refused over
// unsynced state traces the tickets it would have dropped: the count is taken
// before the refusal, so the record of a refused restore says what was at
// stake.
func TestBackupRestoreTracesWhatTheFileLacksWhetherItLandsOrIsRefused(t *testing.T) {
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	created := strings.Fields(runCLIInDir(t, repo, "backup", "create"))
	if len(created) != 2 {
		t.Fatalf("backup create printed %v, want a name and a path", created)
	}
	if out := runCLIInDir(t, repo, "backup", "restore", "--path", created[1]); strings.Contains(out, "removed ") {
		t.Errorf("restore of a backup of the workspace as it is printed %q, want no ticket removed", out)
	}
	if restore := restoreTraceRecord(t, repo); restore.Metadata["absent_from_file"] != "0" || restore.Metadata["absent_from_file_ids"] != "" {
		t.Errorf("restore trace metadata = %v, want absent_from_file=0", restore.Metadata)
	}

	later := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", "Unsynced", "--topic", "demo", "--parent", epic))
	_, err := runCLIInDirAllowError(t, repo, "backup", "restore", "--path", created[1])
	var conflict MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("restore over a ticket made since the last restore error = %v (%T), want the unsynced-state refusal", err, err)
	}
	if restore := restoreTraceRecord(t, repo); restore.Metadata["absent_from_file"] != "1" || restore.Metadata["absent_from_file_ids"] != later {
		t.Errorf("refused restore trace metadata = %v, want absent_from_file=1 (%s)", restore.Metadata, later)
	}
}

// A restore that fails before it has read both the file and the workspace
// never counted what it would drop, so its trace has no dropped key: absent,
// not a zero that was never counted.
func TestBackupRestoreThatCannotReadItsFileTracesNoDroppedCount(t *testing.T) {
	repo := newInitTestRepo(t)
	if _, err := runCLIInDirAllowError(t, repo, "backup", "restore", "--path", filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatalf("restore of a missing file succeeded")
	}
	if restore := restoreTraceRecord(t, repo); restore.Metadata["path"] == "" {
		t.Fatalf("restore trace metadata = %v, want the failed restore's record", restore.Metadata)
	} else if absent, ok := restore.Metadata["absent_from_file"]; ok {
		t.Errorf("restore trace metadata absent_from_file = %q, want no key: nothing was counted", absent)
	}
}
