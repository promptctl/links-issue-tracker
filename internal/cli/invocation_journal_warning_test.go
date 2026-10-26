package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// journalWarning is one dolt.journal_warnings occurrence, as the events file
// holds it.
type journalWarning struct {
	Store string `json:"dolt.journal_warning.store"`
	Error string `json:"dolt.journal_warning.error"`
}

// A read whose store passed over a journal index it could not read, and read
// the whole journal instead, says so on its event, naming the store: the
// warning Dolt reported is a dolt.journal_warnings occurrence, and it does not
// reach stderr through Dolt's logger. [LAW:nothing-unseen]
func TestAReadThatPassedOverTheJournalIndexSaysSoOnItsEvent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	newEpicInDir(t, repo, "Indexed epic")
	noms := nomsDir(t, repo)
	corruptMidFile(t, filepath.Join(noms, "journal.idx"))
	logged := captureDoltLogger(t)

	out := runCLIInDir(t, repo, "ls")

	requireJournalWarning(t, "lit ls", noms, "passed over its index", logged, out)
}

// A write whose store recovered a journal tail an interrupted write left, one
// no record parser accepts, says so on its event the same way.
func TestAWriteThatRecoveredAJournalTailSaysSoOnItsEvent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Torn epic")
	noms := nomsDir(t, repo)
	appendUnparseableTail(t, filepath.Join(noms, doltChunkJournalName))
	logged := captureDoltLogger(t)

	out := runCLIInDir(t, repo, "new", "--title", "after the tear", "--topic", "demo", "--type", "task", "--parent", epic)

	requireJournalWarning(t, "lit new", noms, "invalid journal record length", logged, out)
}

// doltChunkJournalName is the chunk journal's file name in a store's noms
// directory: Dolt's journal address, thirty-two v's.
var doltChunkJournalName = strings.Repeat("v", 32)

// nomsDir is the noms directory of the workspace store at repo, where Dolt
// keeps its chunk journal and journal.idx.
func nomsDir(t *testing.T, repo string) string {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(store.DoltJournalLockPath(ws.DatabasePath))
}

// captureDoltLogger sends Dolt's logger, logrus's standard one, to a buffer
// for the rest of the test.
func captureDoltLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	prev := logrus.StandardLogger().Out
	logrus.SetOutput(&logged)
	t.Cleanup(func() { logrus.SetOutput(prev) })
	return &logged
}

// requireJournalWarning fails unless the last event is command's, and carries
// a dolt.journal_warnings occurrence from the store at noms whose error holds
// want, which neither Dolt's logger nor the command's output, stdout and stderr
// together, carried.
func requireJournalWarning(t *testing.T, command, noms, want string, logged *bytes.Buffer, out string) {
	t.Helper()
	ev := lastEvent(t)
	if ev.Event != command {
		t.Fatalf("last event = %q, want %s", ev.Event, command)
	}
	raw, err := json.Marshal(ev.Occurrences["dolt.journal_warnings"])
	if err != nil {
		t.Fatal(err)
	}
	var warnings []journalWarning
	if err := json.Unmarshal(raw, &warnings); err != nil {
		t.Fatalf("dolt.journal_warnings = %s, want entries naming a store and an error: %v", raw, err)
	}
	found := false
	for _, w := range warnings {
		found = found || (w.Store == noms && strings.Contains(w.Error, want))
	}
	if !found {
		t.Fatalf("dolt.journal_warnings = %s, want one from %s holding %q", raw, noms, want)
	}
	if strings.Contains(logged.String(), want) {
		t.Fatalf("Dolt's logger wrote %q; the warning belongs on the event alone", logged.String())
	}
	if strings.Contains(out, want) {
		t.Fatalf("%s wrote %q; the warning belongs on the event alone", command, out)
	}
}

// corruptMidFile overwrites 64 bytes in the middle of path, the way a torn
// write leaves a file no reader can parse.
func corruptMidFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 128 {
		t.Fatalf("%s holds %d bytes; the test needs an index with records to corrupt", path, info.Size())
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xA5}, 64), info.Size()/2); err != nil {
		t.Fatal(err)
	}
}

// appendUnparseableTail appends eight 0xFF bytes to path: a record length past
// the journal's maximum, the tail an interrupted write leaves.
func appendUnparseableTail(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(bytes.Repeat([]byte{0xFF}, 8)); err != nil {
		t.Fatal(err)
	}
}

// An invocation that met nothing Dolt observes says so for each observer:
// git.cancels and dolt.journal_warnings are [], not absent.
func TestAnInvocationThatMetNoneRecordsEmptyObservations(t *testing.T) {
	recorder := telemetry.Begin(telemetry.Event{})
	invocationContext(context.Background(), recorder)
	line, err := telemetry.Line(recorder.Finish(telemetry.End{At: time.Now()}))
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Occurrences map[string]json.RawMessage `json:"occurrences"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"git.cancels", "dolt.journal_warnings"} {
		if got := string(ev.Occurrences[key]); got != "[]" {
			t.Errorf("%s = %q, want []", key, got)
		}
	}
}
