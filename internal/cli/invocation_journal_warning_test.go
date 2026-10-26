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

// A read whose store passed over a journal index it could not read, and read
// the whole journal instead, says so on its event: the warning Dolt reported
// is a dolt.journal_warnings occurrence, and nothing reaches stderr through
// Dolt's logger. [LAW:nothing-unseen]
func TestAReadThatPassedOverTheJournalIndexSaysSoOnItsEvent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Indexed epic")
	for i := range 40 {
		runCLIInDir(t, repo, "new", "--title", "ticket "+strings.Repeat("x", i), "--topic", "demo", "--type", "task", "--parent", epic)
	}
	ws, err := workspace.Resolve(testEnv(), repo)
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(filepath.Dir(store.DoltJournalLockPath(ws.DatabasePath)), "journal.idx")
	corruptMidFile(t, index)

	var logged bytes.Buffer
	prev := logrus.StandardLogger().Out
	logrus.SetOutput(&logged)
	t.Cleanup(func() { logrus.SetOutput(prev) })

	runCLIInDir(t, repo, "ls")

	ev := lastEvent(t)
	if ev.Event != "lit ls" {
		t.Fatalf("last event = %q, want lit ls", ev.Event)
	}
	warnings := ev.Occurrences["dolt.journal_warnings"]
	passedOver := false
	for _, w := range warnings {
		text, ok := w.(string)
		if !ok {
			t.Fatalf("dolt.journal_warnings entry %#v is not the warning's text", w)
		}
		passedOver = passedOver || strings.Contains(text, "passed over its index")
	}
	if !passedOver {
		t.Fatalf("dolt.journal_warnings = %v, want the read's pass over its corrupt index", warnings)
	}
	if logged.Len() != 0 {
		t.Fatalf("Dolt's logger wrote %q; the warning belongs on the event alone", logged.String())
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
