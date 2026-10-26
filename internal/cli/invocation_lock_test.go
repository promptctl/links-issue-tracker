package cli

import (
	"encoding/json"
	"testing"
)

// lockHoldEntry is one lock.holds occurrence, as the event file carries it.
type lockHoldEntry struct {
	Lock              string `json:"lock.hold.lock"`
	Routine           bool   `json:"lock.hold.routine"`
	HeldMS            int64  `json:"lock.hold.held_ms"`
	ExecutingMS       int64  `json:"lock.hold.executing_ms"`
	LongestIdleMS     int64  `json:"lock.hold.longest_idle_ms"`
	Heartbeats        int    `json:"lock.hold.heartbeats"`
	HeartbeatFailures int    `json:"lock.hold.heartbeat_failures"`
}

// lastLockOccurrences is the last event's lock.holds and lock.waits, failing
// unless the event is command's and both keys are present.
func lastLockOccurrences(t *testing.T, command string) ([]lockHoldEntry, []map[string]any) {
	t.Helper()
	ev := lastEvent(t)
	if ev.Event != command {
		t.Fatalf("last event = %q, want %s", ev.Event, command)
	}
	var holds []lockHoldEntry
	var waits []map[string]any
	for key, into := range map[string]any{"lock.holds": &holds, "lock.waits": &waits} {
		value, present := ev.Occurrences[key]
		if !present {
			t.Fatalf("%s event has no %s; an invocation that held or waited on nothing says [], not nothing", command, key)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s = %s: %v", key, raw, err)
		}
	}
	return holds, waits
}

// Which writes hold Dolt's LOCK as routine is pinned by what each command's
// event says about its hold, the observable fact, rather than by the shape of
// the registration table: a command whose work does not grow with its input
// heartbeats to the writers behind it, and every other write keeps the long
// hold its waiters fail on, naming it. A new opt-in to routine is a reviewed
// change to this list.
func TestEachWriteSaysWhetherItsLockHoldWasRoutine(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Holds")
	first := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "first", "--topic", "demo", "--type", "task", "--parent", epic))
	second := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "second", "--topic", "demo", "--type", "task", "--parent", epic))

	for _, c := range []struct {
		event   string
		args    []string
		routine bool
	}{
		{"lit new", []string{"new", "--title", "third", "--topic", "demo", "--type", "task", "--parent", epic}, true},
		{"lit update", []string{"update", first, "--title", "renamed"}, true},
		{"lit comment add", []string{"comment", "add", first, "--body", "noted"}, true},
		{"lit label add", []string{"label", "add", first, "held"}, true},
		{"lit dep add", []string{"dep", "add", second, "--related-to", first}, true},
		{"lit start", []string{"start", first}, true},
		{"lit rank set", []string{"rank", "set", second, first}, false},
	} {
		runCLIInDir(t, repo, c.args...)
		holds, waits := lastLockOccurrences(t, c.event)
		if len(holds) != 1 || holds[0].Lock != "LOCK" {
			t.Fatalf("%s: lock.holds = %+v, want the one LOCK hold of its write engine", c.event, holds)
		}
		hold := holds[0]
		if hold.Routine != c.routine {
			t.Errorf("%s: lock.hold.routine = %t, want %t", c.event, hold.Routine, c.routine)
		}
		if hold.HeartbeatFailures != 0 || hold.Heartbeats < 1 {
			t.Errorf("%s: hold %+v, want its arrival written and no heartbeat failing", c.event, hold)
		}
		if len(waits) != 0 {
			t.Errorf("%s: lock.waits = %+v, want [] with nothing contending", c.event, waits)
		}
	}
}

// A read holds no LOCK and waits on none, and says both as [].
func TestAReadSaysItHeldNoLock(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	newEpicInDir(t, repo, "Read me")
	runCLIInDir(t, repo, "backlog")
	holds, waits := lastLockOccurrences(t, "lit backlog")
	if len(holds) != 0 || len(waits) != 0 {
		t.Fatalf("lit backlog: lock.holds = %+v, lock.waits = %+v, want both []", holds, waits)
	}
}
