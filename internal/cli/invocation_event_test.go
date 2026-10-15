package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// isolateEvents gives the test its own XDG_STATE_HOME, so the events file it
// reads holds only the invocations it ran.
func isolateEvents(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func eventsPath(t *testing.T) string {
	t.Helper()
	dir, err := config.StateDir()
	if err != nil {
		t.Fatalf("StateDir error = %v", err)
	}
	return filepath.Join(dir, eventsFileName)
}

// readEvents is every event in the events file, oldest first.
func readEvents(t *testing.T) []telemetry.Event {
	t.Helper()
	raw, err := os.ReadFile(eventsPath(t))
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	var events []telemetry.Event
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var ev telemetry.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("events file line %q is not an event: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func lastEvent(t *testing.T) telemetry.Event {
	t.Helper()
	events := readEvents(t)
	return events[len(events)-1]
}

var traceID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Every invocation appends exactly one event, under its own trace id, saying
// which command ran, where, and how it ended — outside any workspace too.
func TestEveryInvocationLeavesOneEvent(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	runCLIInDir(t, dir, "version")
	runCLIInDir(t, dir, "version")

	events := readEvents(t)
	if len(events) != 2 {
		t.Fatalf("events file holds %d events, want 2 (one per invocation)", len(events))
	}
	wantCwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Event != "lit version" || ev.Service != "lit" {
			t.Errorf("event, service = %q, %q; want lit version, lit", ev.Event, ev.Service)
		}
		if ev.Outcome != telemetry.OutcomeOK || ev.ExitCode != ExitOK || ev.Reason != "" || ev.Error != "" {
			t.Errorf("end = %q %d %q %q, want ok 0 with no reason or error", ev.Outcome, ev.ExitCode, ev.Reason, ev.Error)
		}
		if !traceID.MatchString(ev.TraceID) {
			t.Errorf("trace_id = %q, want 32 lowercase hex digits", ev.TraceID)
		}
		if ev.Sink != eventsSink || ev.PID != os.Getpid() || ev.Cwd != wantCwd {
			t.Errorf("sink, pid, cwd = %q, %d, %q; want %q, %d, %q", ev.Sink, ev.PID, ev.Cwd, eventsSink, os.Getpid(), wantCwd)
		}
		if ev.StartedAt.IsZero() || ev.DurationMS < 0 {
			t.Errorf("started_at, duration_ms = %v, %d; want a start and a non-negative duration", ev.StartedAt, ev.DurationMS)
		}
	}
	if events[0].TraceID == events[1].TraceID {
		t.Errorf("both invocations carry trace id %q", events[0].TraceID)
	}
}

// A failed invocation's event carries the exit code and reason its error line
// printed, read from the same error.
func TestAFailedInvocationRecordsItsExitCodeAndReason(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		args   []string
		event  string
		reason commandReason
	}{
		{[]string{"nosuchcmd"}, "lit", reasonUnknownCommand},
		{[]string{"next"}, "lit next", reasonOutsideGitWorkspace},
	} {
		_, err := runCLIInDirAllowError(t, dir, tc.args...)
		if err == nil {
			t.Fatalf("Run(%v) succeeded, want a failure", tc.args)
		}
		ev := lastEvent(t)
		if ev.Event != tc.event || ev.Outcome != telemetry.OutcomeError || ev.ExitCode != ExitCode(err) || ev.Reason != string(tc.reason) || ev.Error != err.Error() {
			t.Errorf("Run(%v) event = %q %q %d %q %q; want %q error %d %q %q", tc.args, ev.Event, ev.Outcome, ev.ExitCode, ev.Reason, ev.Error, tc.event, ExitCode(err), tc.reason, err.Error())
		}
	}
}

// The event names the flags a command was given and never their values: a
// value can be a title, a body, or a pasted secret.
func TestTheEventNamesFlagsButNotTheirValues(t *testing.T) {
	isolateEvents(t)
	const value = "a-title-that-must-stay-on-the-command-line"
	if _, err := runCLIInDirAllowError(t, t.TempDir(), "new", "--type", "task", "--title", value); err == nil {
		t.Fatal("lit new outside a workspace succeeded")
	}
	ev := lastEvent(t)
	if ev.Event != "lit new" || strings.Join(ev.Flags, ",") != "title,type" {
		t.Errorf("event, flags = %q, %q; want lit new, [title type]", ev.Event, ev.Flags)
	}
	raw, err := os.ReadFile(eventsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), value) {
		t.Errorf("the events file holds the --title value:\n%s", raw)
	}
}

// Every command names its own event, by the path the caller typed, without
// the command doing anything for it: dispatch names each level it reaches —
// the command, then the leaf the one shared parse runs — so a family answered
// at its own level is named too.
func TestEveryCommandNamesItsEvent(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	// `bulk label add` and `bulk label rm` name values of the `bulk label`
	// leaf's positional, which its work resolves only after its parse; a help
	// request is answered by that parse, so `bulk label` is what answered.
	answeredBy := map[string]string{
		"bulk label add": "bulk label",
		"bulk label rm":  "bulk label",
	}
	for _, cp := range leafPaths(t) {
		runCLIInDir(t, dir, append(append([]string{}, cp.path...), "--help")...)
		typed := strings.Join(cp.path, " ")
		named := typed
		if parent, ok := answeredBy[typed]; ok {
			named = parent
		}
		if got, want := lastEvent(t).Event, "lit "+named; got != want {
			t.Errorf("`%s --help` recorded event %q, want %q", want, got, want)
		}
	}
}

// Past its cap the events file moves aside, one generation kept, and the
// next event starts a fresh file.
func TestTheEventsFileRotatesPastItsCap(t *testing.T) {
	isolateEvents(t)
	path := eventsPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	full := bytes.Repeat([]byte("x"), eventsMaxBytes+1)
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIInDir(t, t.TempDir(), "version")

	kept, err := os.ReadFile(path + ".1")
	if err != nil || !bytes.Equal(kept, full) {
		t.Fatalf("previous generation = %d bytes, %v; want the full file moved aside", len(kept), err)
	}
	if events := readEvents(t); len(events) != 1 || events[0].Event != "lit version" {
		t.Errorf("fresh events file = %+v, want the one new event", events)
	}
}

// An invocation that panics is recorded as a panic, with the exit status the
// Go runtime gives a process that dies of one.
func TestAPanickingInvocationEndsAsAPanic(t *testing.T) {
	at := time.Now()
	end := invocationEnd(at, nil, "index out of range")
	if end.Outcome != telemetry.OutcomePanic || end.ExitCode != goPanicExitCode || end.Error != "index out of range" || end.Reason != "" || !end.At.Equal(at) {
		t.Errorf("invocationEnd(panic) = %+v, want a panic at exit %d carrying its value", end, goPanicExitCode)
	}
}
