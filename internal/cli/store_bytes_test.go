package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

var numberedBacklogRow = regexp.MustCompile(`(?m)^\s*\d+\. `)

// A command's event says how long it ran by the wall clock, measured from
// outside it, and what it ran over: the store's size on disk and the rows the
// view gathered. The clock is held to the right order of magnitude, so a clock
// started late (after the store open, say) or stopped early fails here, where
// a non-negative check would pass it.
func TestAnInvocationsDurationIsItsWallClock(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Measured epic")
	for _, title := range []string{"first", "second", "third"} {
		runCLIInDir(t, repo, "new", "--title", title, "--topic", "demo", "--type", "task", "--parent", epic)
	}

	began := time.Now()
	out := runCLIInDir(t, repo, "backlog")
	measured := time.Since(began).Milliseconds()

	ev := lastEvent(t)
	if ev.Event != "lit backlog" {
		t.Fatalf("last event = %q, want lit backlog", ev.Event)
	}
	if ev.DurationMS <= 0 || ev.DurationMS > measured || ev.DurationMS*2 < measured {
		t.Fatalf("duration_ms = %d against a measured %dms; want it within (measured/2, measured]", ev.DurationMS, measured)
	}
	printed := len(numberedBacklogRow.FindAllString(out, -1))
	if printed == 0 {
		t.Fatalf("backlog printed no rows:\n%s", out)
	}
	if got := writtenEvent(t, ev).Counts["backlog.rows_gathered"]; got != printed {
		t.Errorf("backlog.rows_gathered = %d, want the %d rows a backlog narrowed by nothing printed", got, printed)
	}
	assertStoreMeasured(t, ev)
}

// Every seam that runs a command against a store records that store's size:
// the sync family's, and `ls --at`'s, whose store is the one --at names.
func TestEveryStoreSeamRecordsTheStoresSize(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		isolateEvents(t)
		repo := newInitTestRepo(t)
		runCLIInDir(t, repo, "sync", "status")
		ev := lastEvent(t)
		if ev.Event != "lit sync status" {
			t.Fatalf("last event = %q, want lit sync status", ev.Event)
		}
		assertStoreMeasured(t, ev)
	})
	t.Run("ls --at", func(t *testing.T) {
		isolateEvents(t)
		storeDir, _ := foreignStore(t, "ws-foreign", "proj")
		runCLIInDir(t, t.TempDir(), "ls", "--at", storeDir)
		ev := lastEvent(t)
		if ev.Event != "lit ls" {
			t.Fatalf("last event = %q, want lit ls", ev.Event)
		}
		assertStoreMeasured(t, ev)
		want, err := engine.ChunkStoreBytes(workspace.LocationFromStorageDir(storeDir).DatabasePath)
		if err != nil {
			t.Fatal(err)
		}
		if got := writtenFacts(t, ev)["store.chunk_store_bytes"]; got != float64(want) {
			t.Errorf("store.chunk_store_bytes = %v, want %v: the --at store's own size", got, want)
		}
	})
}

// A reading that fails is recorded as null bytes beside its reason, never as a
// store of 0 bytes: a store with no chunk store, and one whose chunk store
// cannot be read.
func TestAFailedStoreReadingIsNullBytesAndItsReason(t *testing.T) {
	unreadable := t.TempDir()
	noms := filepath.Join(unreadable, "links", ".dolt", "noms")
	if err := os.MkdirAll(filepath.Join(noms, "oldgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(noms, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(noms, 0o755) })

	for name, root := range map[string]string{
		"absent":     filepath.Join(t.TempDir(), "dolt"),
		"unreadable": unreadable,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := telemetry.Begin(telemetry.Event{})
			recordStoreBytes(telemetry.With(context.Background(), recorder), root)
			facts := writtenFacts(t, recorder.Finish(telemetry.End{At: time.Now()}))
			if got, ok := facts["store.chunk_store_bytes"]; !ok || got != nil {
				t.Errorf("store.chunk_store_bytes = %#v (present %v), want null", got, ok)
			}
			if got, _ := facts["store.chunk_store_bytes_error"].(string); got == "" {
				t.Errorf("store.chunk_store_bytes_error = %#v, want the reading's reason", facts["store.chunk_store_bytes_error"])
			}
		})
	}
}

// assertStoreMeasured holds an event to a successful store reading: a size
// above zero and no error.
func assertStoreMeasured(t *testing.T, ev telemetry.Event) {
	t.Helper()
	facts := writtenFacts(t, ev)
	if got, _ := facts["store.chunk_store_bytes"].(float64); got <= 0 {
		t.Errorf("store.chunk_store_bytes = %#v, want a size above zero", facts["store.chunk_store_bytes"])
	}
	if got, ok := facts["store.chunk_store_bytes_error"]; !ok || got != nil {
		t.Errorf("store.chunk_store_bytes_error = %#v (present %v), want null", got, ok)
	}
}
