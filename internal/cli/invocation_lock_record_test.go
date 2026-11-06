package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lockRecordEntry is one lock.records occurrence, as the event file carries it.
type lockRecordEntry struct {
	Lock         string  `json:"lock.record.lock"`
	SweepRetired int     `json:"lock.record.sweep_retired"`
	SweepError   *string `json:"lock.record.sweep_error"`
	PublishError *string `json:"lock.record.publish_error"`
}

// lastLockRecords is the last event's lock.records entries for lock, failing
// unless the event is command's and the key is present.
func lastLockRecords(t *testing.T, command, lock string) []lockRecordEntry {
	t.Helper()
	ev := lastEvent(t)
	if ev.Event != command {
		t.Fatalf("last event = %q, want %s", ev.Event, command)
	}
	value, present := ev.Occurrences["lock.records"]
	if !present {
		t.Fatalf("%s event has no lock.records", command)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var all []lockRecordEntry
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("lock.records = %s: %v", raw, err)
	}
	var of []lockRecordEntry
	for _, r := range all {
		if r.Lock == lock {
			of = append(of, r)
		}
	}
	return of
}

// Every lock a command takes says, on the command's event, what its upkeep
// of the lock's holder records did: the dead holders' records its sweep
// retired, and the sweep's and the publish's failures. A failure is also
// said on stderr, by the CLI, and costs the command nothing.
func TestALockAcquisitionPutsItsHolderRecordUpkeepOnTheEvent(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	repo := newInitTestRepo(t)
	newEpicInDir(t, repo, "Records")
	const lock = ".links-workspace.lock"
	holders := filepath.Join(repo, ".git", "links", ".links-lock-holders", lock)
	// The lock's path as the store names it: through the temp dir's symlinks.
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(resolved, ".git", "links", lock)

	newEpic := func(title string) (string, []lockRecordEntry) {
		t.Helper()
		_, stderr, err := runCLIInDirSplit(t, repo, "new", "--title", title, "--topic", "demo", "--type", "epic")
		if err != nil {
			t.Fatalf("lit new: %v\n%s", err, stderr)
		}
		return stderr, lastLockRecords(t, "lit new", lock)
	}

	t.Run("a clean acquisition", func(t *testing.T) {
		stderr, records := newEpic("clean")
		if len(records) != 1 || records[0].SweepRetired != 0 || records[0].SweepError != nil || records[0].PublishError != nil {
			t.Fatalf("records = %+v, want one with nothing retired and nothing failed", records)
		}
		if strings.Contains(stderr, "holder") {
			t.Errorf("stderr = %q, want no holder notice", stderr)
		}
	})

	t.Run("a dead holder's record retired", func(t *testing.T) {
		// A record no process holds: what a SIGKILLed holder leaves.
		if err := os.MkdirAll(holders, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(holders, "holder-424242-1"), []byte(`{"pid":424242}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, records := newEpic("swept")
		if len(records) != 1 || records[0].SweepRetired != 1 || records[0].SweepError != nil {
			t.Fatalf("records = %+v, want the one dead record retired", records)
		}
	})

	t.Run("a sweep that fails", func(t *testing.T) {
		// A directory under a record's name: the probe cannot open it.
		unprobeable := filepath.Join(holders, "holder-unprobeable")
		if err := os.MkdirAll(unprobeable, 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(unprobeable)
		stderr, records := newEpic("unswept")
		if len(records) != 1 || records[0].SweepError == nil || !strings.Contains(*records[0].SweepError, "unprobeable") || records[0].PublishError != nil {
			t.Fatalf("records = %+v, want the sweep's failure, naming the record, and the publish standing", records)
		}
		if !strings.Contains(stderr, "could not retire the records of holders gone from "+lockPath) {
			t.Errorf("stderr = %q, want the notice of the failed sweep, naming the lock", stderr)
		}
	})

	t.Run("a publish that fails", func(t *testing.T) {
		// A regular file where the holder directory belongs: no record can be
		// written under it.
		if err := os.RemoveAll(holders); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(holders, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		stderr, records := newEpic("unpublished")
		if len(records) != 1 || records[0].PublishError == nil {
			t.Fatalf("records = %+v, want the publish's failure", records)
		}
		if !strings.Contains(stderr, "could not record this process as the holder of "+lockPath) {
			t.Errorf("stderr = %q, want the notice of the failed publish, naming the lock", stderr)
		}
	})
}
