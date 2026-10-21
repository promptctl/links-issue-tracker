package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("output %q is %d line(s), want a header and one line per move", out.String(), len(lines))
	}
	for i, want := range [][]string{
		{"moved 2 ticket(s) the merged backlog held outside an epic;", "lit parent set --child <id> --parent <epic-id>"},
		{"acme-core-bb22", " into ", "acme-unfiled-0bf7"},
		{"acme-core-cc33", " to the top level"},
	} {
		for _, part := range want {
			if !strings.Contains(lines[i], part) {
				t.Errorf("line %d %q lacks %q", i, lines[i], part)
			}
		}
	}

	metadata := map[string]string{}
	landingMetadata(metadata, storage.Landing{})
	for _, key := range []string{"unlooped", "rehomed", "rekeyed"} {
		if metadata[key] != "0" {
			t.Errorf("metadata %s = %q, want %q: zero changes is recorded, not omitted", key, metadata[key], "0")
		}
	}
}

func TestWriteLandingNamesEachRekeyAndIsSilentWithoutOne(t *testing.T) {
	var none bytes.Buffer
	if err := writeLanding(&none, "the backup", storage.Landing{}); err != nil {
		t.Fatalf("writeLanding(empty) error = %v", err)
	}
	if none.Len() != 0 {
		t.Errorf("writeLanding(empty) printed %q, want nothing", none.String())
	}

	landing := storage.Landing{Rekeyed: []storage.Rekeyed{
		{IssueID: "acme-core-bb22", From: "V", Rank: "U"},
		{IssueID: "acme-core-cc33", From: "a-1", Rank: "W"},
	}}
	var out bytes.Buffer
	if err := writeLanding(&out, "the merged backlog", landing); err != nil {
		t.Fatalf("writeLanding error = %v", err)
	}
	line := strings.TrimSuffix(out.String(), "\n")
	for _, part := range []string{"gave 2 ticket(s) in the merged backlog a new rank key so that no two share one;", "acme-core-bb22", "acme-core-cc33"} {
		if !strings.Contains(line, part) {
			t.Errorf("output %q lacks %q", line, part)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("output %q is more than one line", out.String())
	}

	metadata := map[string]string{}
	landingMetadata(metadata, landing)
	if want := `acme-core-bb22 "V" -> "U", acme-core-cc33 "a-1" -> "W"`; metadata["rekeyed"] != "2" || metadata["rekeyed_moves"] != want {
		t.Errorf("metadata = %v, want rekeyed=2 and rekeyed_moves %s", metadata, want)
	}
}

// TestBackupRestoreNamesTheTicketsItMovesIntoAnEpic drives `lit backup restore`
// over a backup holding a task with no epic — the shape every backup taken
// before schema v7 holds. The restore lands it inside the unfiled epic, prints
// the move, and records it on the restore's sync trace.
func TestBackupRestoreNamesTheTicketsItMovesIntoAnEpic(t *testing.T) {
	repo, task, path := newPreContainmentBackup(t)

	out := runCLIInDir(t, repo, "backup", "restore", "--path", path)
	if !strings.Contains(out, "moved 1 ticket(s) the backup held outside an epic;") || !strings.Contains(out, task) {
		t.Errorf("restore output %q does not name the move of %s", out, task)
	}
	shown := runCLIInDir(t, repo, "show", task)
	if !strings.Contains(shown, "Unfiled tickets") {
		t.Errorf("show %s = %q, want it inside the unfiled epic", task, shown)
	}
	if restore := restoreTraceRecord(t, repo); restore.Metadata["rehomed"] != "1" || !strings.Contains(restore.Metadata["rehomed_moves"], task+" into ") {
		t.Errorf("restore trace metadata = %v, want the one move of %s", restore.Metadata, task)
	}
}

// TestBackupRestoreTracesItsMovesWhenALaterStepFails fails the restore after
// the store has committed it — the sync base cannot be written — and finds the
// moves on the trace all the same: the trace records what the store holds.
func TestBackupRestoreTracesItsMovesWhenALaterStepFails(t *testing.T) {
	repo, task, path := newPreContainmentBackup(t)
	ws, err := workspace.Resolve(testEnv(), repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	base := filepath.Join(ws.StorageDir, "last-sync-base.json")
	if err := os.RemoveAll(base); err != nil {
		t.Fatalf("clear sync base: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "blocked"), 0o755); err != nil {
		t.Fatalf("block sync base: %v", err)
	}

	if _, err := runCLIInDirAllowError(t, repo, "backup", "restore", "--path", path, "--force"); err == nil {
		t.Fatalf("restore succeeded with its sync base blocked; the test needs a failure after the commit")
	}
	if restore := restoreTraceRecord(t, repo); restore.Metadata["rehomed"] != "1" || !strings.Contains(restore.Metadata["rehomed_moves"], task+" into ") {
		t.Errorf("restore trace metadata = %v, want the one move the store committed, %s", restore.Metadata, task)
	}
}

// newPreContainmentBackup returns a repo holding a task inside an epic, the
// task's id, and the path of a backup of that repo with every parent edge
// removed: the shape every backup taken before schema v7 holds.
func newPreContainmentBackup(t *testing.T) (repo, task, path string) {
	t.Helper()
	repo = newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	task = extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", "Loose", "--topic", "demo", "--parent", epic))

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
	path = filepath.Join(t.TempDir(), "pre-containment.json")
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	return repo, task, path
}

// restoreTraceRecord is the last sync trace record `lit backup restore` left.
func restoreTraceRecord(t *testing.T, repo string) syncTraceRecord {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), repo)
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
	return *restore
}

// TestBackupRestoreNamesTheTicketsItGivesAKeyOfTheirOwn drives `lit backup
// restore` over a backup in which two epics hold one rank key. The restore
// gives the one listed first a new key, prints which, and records the move on
// the restore's trace.
func TestBackupRestoreNamesTheTicketsItGivesAKeyOfTheirOwn(t *testing.T) {
	repo := newInitTestRepo(t)
	first := newEpicInDir(t, repo, "First")
	second := newEpicInDir(t, repo, "Second")
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
	issues, _ := backup["issues"].([]any)
	if len(issues) != 2 {
		t.Fatalf("backup holds %d issue(s), want the two epics", len(issues))
	}
	shared, _ := issues[0].(map[string]any)["rank"].(string)
	issues[1].(map[string]any)["rank"] = shared
	moved := min(first, second)
	edited, err := json.Marshal(backup)
	if err != nil {
		t.Fatalf("encode backup: %v", err)
	}
	path := filepath.Join(t.TempDir(), "shared-key.json")
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	out := runCLIInDir(t, repo, "backup", "restore", "--path", path, "--force")
	if want := "gave 1 ticket(s) in the backup a new rank key so that no two share one; every listing keeps its order: " + quoteRemote(moved).inline() + "\n"; !strings.Contains(out, want) {
		t.Errorf("restore output %q lacks %q", out, want)
	}
	if doctor := runCLIInDir(t, repo, "doctor"); !strings.Contains(doctor, " duplicate_ranks=0 ") {
		t.Errorf("doctor after the restore:\n%s\nwant duplicate_ranks=0", doctor)
	}
	restore := restoreTraceRecord(t, repo)
	if want := fmt.Sprintf("%s %q -> ", moved, shared); restore.Metadata["rekeyed"] != "1" || !strings.HasPrefix(restore.Metadata["rekeyed_moves"], want) {
		t.Errorf("restore trace metadata = %v, want rekeyed=1 and a move starting %s", restore.Metadata, want)
	}
}
