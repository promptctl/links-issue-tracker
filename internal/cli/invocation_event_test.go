package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/primitives/filelock"
)

// isolateEvents gives the test its own XDG_STATE_HOME, so the events file it
// reads holds only the invocations it ran.
func isolateEvents(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func eventsPath(t *testing.T) string {
	t.Helper()
	dir, err := config.StateDir(testEnv())
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
// Where is the directory the invocation was handed, which is not the test
// process's own.
func TestEveryInvocationLeavesOneEvent(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	if wd, err := os.Getwd(); err != nil || wd == dir {
		t.Fatalf("process cwd = %q, %v; the test needs it to differ from %q", wd, err, dir)
	}
	runCLIInDir(t, dir, "version")
	runCLIInDir(t, dir, "version")

	events := readEvents(t)
	if len(events) != 2 {
		t.Fatalf("events file holds %d events, want 2 (one per invocation)", len(events))
	}
	wantCwd := dir
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
		if ev.SinkRotation != telemetry.RotationNone || ev.SinkRotationError != "" {
			t.Errorf("sink_rotation = %q %q, want none: the file is far under its cap", ev.SinkRotation, ev.SinkRotationError)
		}
		if ev.StartedAt.IsZero() || ev.DurationMS < 0 {
			t.Errorf("started_at, duration_ms = %v, %d; want a start and a non-negative duration", ev.StartedAt, ev.DurationMS)
		}
	}
	if events[0].TraceID == events[1].TraceID {
		t.Errorf("both invocations carry trace id %q", events[0].TraceID)
	}
	// An event's error can quote what was typed, so only its owner reads it.
	for path, want := range map[string]os.FileMode{eventsPath(t): fsperm.FileModeAsStored(t, 0o600), filepath.Dir(eventsPath(t)): fsperm.DirModeAsStored(t, 0o700)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

// The root parses its own flags, so its event names them as a leaf's does:
// `lit --help` and a bare `lit` are told apart.
func TestTheRootRecordsItsFlags(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		args  []string
		flags string
	}{
		{[]string{"--help"}, "help"},
		{[]string{}, ""},
	} {
		runCLIInDir(t, dir, tc.args...)
		if ev := lastEvent(t); ev.Event != "lit" || strings.Join(ev.Flags, ",") != tc.flags {
			t.Errorf("Run(%v) event, flags = %q, %q; want lit, [%s]", tc.args, ev.Event, ev.Flags, tc.flags)
		}
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
		if ev.Event != tc.event || ev.Outcome != telemetry.OutcomeError || ev.ExitCode != ExitCode(err) || ev.Reason != tc.reason.String() || ev.Error != err.Error() {
			t.Errorf("Run(%v) event = %q %q %d %q %q; want %q error %d %q %q", tc.args, ev.Event, ev.Outcome, ev.ExitCode, ev.Reason, ev.Error, tc.event, ExitCode(err), tc.reason, err.Error())
		}
	}
}

// An add that found its edge already in place ends its event as the answer it
// is, not a fault: the exit code and reason say the state already held, and
// the error names the edge, so the repeats are countable and attributable.
func TestAnExistingEdgeEndsTheEventAsAlreadyHolding(t *testing.T) {
	exists := storage.RelationExistsError{SrcID: "test-a", DstID: "test-b", Type: model.RelBlockedBy}
	end := invocationEnd(time.Now(), fmt.Errorf("add relation: %w", exists), nil)
	if end.Outcome != telemetry.OutcomeError || end.ExitCode != ExitNoWork || end.Reason != reasonStateAlreadyHolds.String() || !strings.Contains(end.Error, exists.Error()) {
		t.Errorf("invocationEnd(existing edge) = %+v, want exit %d %s naming the edge", end, ExitNoWork, reasonStateAlreadyHolds)
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
	if events := readEvents(t); len(events) != 1 || events[0].Event != "lit version" || events[0].SinkRotation != telemetry.RotationRotated {
		t.Errorf("fresh events file = %+v, want the one new event, recording the rotation", events)
	}
}

// The size check and the rename are one step under the log's lock: while
// another process holds it, the log is not moved, and once it is released the
// rotation goes ahead.
func TestRotationWaitsForTheLogsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("over the cap"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, acquired, err := filelock.Acquire(context.Background(), path+".lock", true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("hold the log's lock: acquired %v, %v", acquired, err)
	}
	type result struct {
		rotated bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		rotated, err := rotateLog(path, 1)
		done <- result{rotated, err}
	}()

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(path + ".1"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the log was rotated while another process held its lock (stat .1: %v)", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if got := <-done; !got.rotated || got.err != nil {
		t.Fatalf("rotateLog after the release = %v, %v; want rotated", got.rotated, got.err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no previous generation after the rotation: %v", err)
	}
}

// A log under its cap is appended to without its lock, so a filesystem whose
// flock fails or stalls costs nothing until the log needs rotating.
func TestALogUnderItsCapDoesNotTakeItsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("under the cap"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, acquired, err := filelock.Acquire(context.Background(), path+".lock", true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("hold the log's lock: acquired %v, %v", acquired, err)
	}
	defer func() { _ = release() }()

	if rotated, err := rotateLog(path, 1<<20); rotated || err != nil {
		t.Errorf("rotateLog under the cap = %v, %v; want no rotation and no error while another process holds the lock", rotated, err)
	}
}

// An invocation's event is written once, by whichever end reaches it first,
// and a second end returns only after that write has landed: the interrupt
// guard's hard exit, arriving while the deferred end is still writing, cannot
// exit the process before the line is in the file.
func TestASecondEndWaitsForTheFirstEndsWrite(t *testing.T) {
	isolateEvents(t)
	path := eventsPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Over its cap, so a write takes the rotation lock, which the test holds
	// to keep the first end mid-write.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), eventsMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	release, acquired, err := filelock.Acquire(context.Background(), path+".lock", true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("hold the rotation lock: acquired %v, %v", acquired, err)
	}

	record := &invocationRecord{recorder: telemetry.Begin(telemetry.Event{Event: "lit sync push"}), stderr: io.Discard, env: testEnv()}
	written := make(chan bool, 2)
	for _, end := range []telemetry.End{{Outcome: telemetry.OutcomeOK}, interruptedEnd(time.Now(), "SIGTERM", 143)} {
		go func() {
			record.close(end)
			// Written: the oversized file rotated away and the event begun a
			// new one.
			info, err := os.Stat(path)
			written <- err == nil && info.Size() > 0 && info.Size() < eventsMaxBytes
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if err := release(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !<-written {
			t.Fatal("an end returned before the event was in the file")
		}
	}
	if events := readEvents(t); len(events) != 1 || events[0].Event != "lit sync push" {
		t.Errorf("events = %+v, want the one event, written once", events)
	}
}

// A rotation that cannot run is said on the event that grew the file past its
// cap, and on stderr, and the event is still written.
func TestAFailedRotationIsRecordedOnTheEvent(t *testing.T) {
	isolateEvents(t)
	path := eventsPath(t)
	// A non-empty directory where the previous generation goes: the rename
	// onto it fails.
	if err := os.MkdirAll(filepath.Join(path+".1", "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), eventsMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runCLIInDir(t, t.TempDir(), "version")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ev telemetry.Event
	if err := json.Unmarshal(raw[eventsMaxBytes+1:], &ev); err != nil {
		t.Fatalf("no event appended past the oversized file: %v", err)
	}
	if ev.Event != "lit version" || ev.SinkRotation != telemetry.RotationFailed || !strings.Contains(ev.SinkRotationError, path+".1") {
		t.Errorf("event = %q, sink_rotation %q %q; want lit version, failed, naming %s.1", ev.Event, ev.SinkRotation, ev.SinkRotationError, path)
	}
	if !strings.Contains(out, "rotation failed") {
		t.Errorf("stderr does not report the failed rotation:\n%s", out)
	}
}

// An invocation the interrupt guard hard-exits is recorded as interrupted,
// naming the signal and the exit code the guard exits with.
func TestAHardExitedInvocationEndsAsInterrupted(t *testing.T) {
	at := time.Now()
	end := interruptedEnd(at, "SIGTERM", 143)
	if end.Outcome != telemetry.OutcomeInterrupted || end.ExitCode != 143 || !strings.Contains(end.Error, "SIGTERM") || !strings.Contains(end.Error, "143") || end.Reason != "" || !end.At.Equal(at) {
		t.Errorf("interruptedEnd = %+v, want interrupted by SIGTERM at exit 143", end)
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

// A schema ahead of this binary ends the invocation's event with one exit code
// and one reason whichever boundary refused: the workspace on open, or the
// remote on a sync write, which reaches the sink as the sync-failure block. The
// facts say which: the workspace refusal records its rollback offer, the remote
// one the remote it refused, and neither records the other's; both record the
// schema versions they compared.
func TestASchemaAheadRefusalEndsTheEventAsSchemaAheadAtEitherBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refusal   error
		wantFacts map[string]any
		otherKey  string
	}{
		{
			name:      "workspace",
			refusal:   fmt.Errorf("open store: %w", &store.UnsupportedSchemaVersionError{WorkspaceVersion: 7, MaxSupported: 3}),
			wantFacts: map[string]any{"schema_ahead.rollback_snapshot": ""},
			otherKey:  "schema_ahead.remote",
		},
		{
			name:    "remote",
			refusal: asSyncFailure(&store.RemoteSchemaAheadError{Remote: "origin", Branch: "master", RemoteVersion: 7, BinarySupportedMax: 3}),
			wantFacts: map[string]any{
				"schema_ahead.remote":             "origin",
				"schema_ahead.branch":             "master",
				"schema_ahead.remote_schema":      float64(7),
				"schema_ahead.running_schema_max": float64(3),
			},
			otherKey: "schema_ahead.rollback_snapshot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := telemetry.Line(telemetry.Begin(telemetry.Event{}).Finish(invocationEnd(time.Now(), tc.refusal, nil)))
			if err != nil {
				t.Fatal(err)
			}
			var written struct {
				ExitCode int            `json:"exit_code"`
				Reason   string         `json:"reason"`
				Facts    map[string]any `json:"facts"`
			}
			if err := json.Unmarshal(line, &written); err != nil {
				t.Fatal(err)
			}
			if written.ExitCode != ExitValidation || written.Reason != "workspace_schema_ahead" {
				t.Errorf("event ended exit_code=%d reason=%q, want %d workspace_schema_ahead", written.ExitCode, written.Reason, ExitValidation)
			}
			for key, want := range tc.wantFacts {
				if got, ok := written.Facts[key]; !ok || got != want {
					t.Errorf("%s = %#v (present %v), want %#v", key, got, ok, want)
				}
			}
			if got, ok := written.Facts[tc.otherKey]; ok {
				t.Errorf("%s = %#v recorded by the other boundary's refusal", tc.otherKey, got)
			}
		})
	}
}

// A sync-failure block's event records which class it was: every class ends on
// the one sync_divergence reason, so the reason alone cannot say.
func TestASyncFailureEndsTheEventWithItsClass(t *testing.T) {
	failure := SyncFailureError{Failure: SyncFailure{Class: syncFailurePushDiverged, Remote: "origin", Branch: "master", Cause: storage.ErrPushDiverged}}
	line, err := telemetry.Line(telemetry.Begin(telemetry.Event{}).Finish(invocationEnd(time.Now(), failure, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		ExitCode int            `json:"exit_code"`
		Reason   string         `json:"reason"`
		Facts    map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(line, &written); err != nil {
		t.Fatal(err)
	}
	if written.ExitCode != ExitConflict || written.Reason != "sync_divergence" {
		t.Errorf("event ended exit_code=%d reason=%q, want %d sync_divergence", written.ExitCode, written.Reason, ExitConflict)
	}
	if got := written.Facts["sync.failure_class"]; got != "push_diverged" {
		t.Errorf("sync.failure_class = %#v, want push_diverged", got)
	}
}

// A schema-ahead refusal's event records which snapshot it offered as the
// lossy rollback — none is an empty name — why snapshots could not be read, and
// how many it read, and a wrapped refusal records the same.
func TestASchemaAheadRefusalRecordsItsRollbackOffer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		snapshot      string
		lookupErr     error
		wantLookupErr any
		examined      int
		unreadable    int
	}{
		{name: "offered", snapshot: "1700000000-pre-migrate-1700000000", wantLookupErr: nil, examined: 2},
		{name: "none", snapshot: "", wantLookupErr: nil, examined: 3},
		{name: "offered past an unreadable snapshot", snapshot: "1700000000-pre-migrate-1700000000", lookupErr: errors.New("read the schema snapshot x holds: bad manifest"), wantLookupErr: "read the schema snapshot x holds: bad manifest", examined: 2, unreadable: 1},
		{name: "lookup failed", lookupErr: errors.New("list snapshots: permission denied"), wantLookupErr: "list snapshots: permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := &store.UnsupportedSchemaVersionError{WorkspaceVersion: 7, MaxSupported: 3, SnapshotName: tc.snapshot, SnapshotLookupErr: tc.lookupErr, SnapshotsExamined: tc.examined, SnapshotsUnreadable: tc.unreadable}
			end := invocationEnd(time.Now(), fmt.Errorf("open store: %w", refusal), nil)
			line, err := telemetry.Line(telemetry.Begin(telemetry.Event{}).Finish(end))
			if err != nil {
				t.Fatal(err)
			}
			var written struct {
				Facts  map[string]any `json:"facts"`
				Counts map[string]int `json:"counts"`
			}
			if err := json.Unmarshal(line, &written); err != nil {
				t.Fatal(err)
			}
			if got := written.Facts["schema_ahead.rollback_snapshot"]; got != tc.snapshot {
				t.Errorf("schema_ahead.rollback_snapshot = %#v, want %q", got, tc.snapshot)
			}
			if got, ok := written.Facts["schema_ahead.rollback_lookup_error"]; !ok || got != tc.wantLookupErr {
				t.Errorf("schema_ahead.rollback_lookup_error = %#v (present %v), want %#v", got, ok, tc.wantLookupErr)
			}
			for key, want := range map[string]int{"schema_ahead.snapshots_examined": tc.examined, "schema_ahead.snapshots_unreadable": tc.unreadable} {
				if got, ok := written.Counts[key]; !ok || got != want {
					t.Errorf("counts.%s = %d (present %v), want a recorded %d", key, got, ok, want)
				}
			}
		})
	}
}

// Every event counts the git its invocation started, zero included, and the
// count is the invocation's own when one environment serves several runs:
// `lit workspace` asks git where the checkout is, once, each time it runs.
func TestEveryEventCountsTheGitItsInvocationStarted(t *testing.T) {
	isolateEvents(t)
	outside := t.TempDir()
	runCLIInDir(t, outside, "version")
	if got, ok := lastEvent(t).Counts["git.lit_spawns"]; !ok || got != 0 {
		t.Fatalf("lit version event git.lit_spawns = %d (present %v), want 0", got, ok)
	}

	repo := t.TempDir()
	runGit(t, repo, "init")
	inv := dirInvocation(repo, io.Discard, io.Discard, []string{"workspace"})
	for run := range 2 {
		if err := Run(context.Background(), inv); err != nil {
			t.Fatalf("lit workspace run %d: %v", run, err)
		}
		if got := lastEvent(t).Counts["git.lit_spawns"]; got != 1 {
			t.Fatalf("lit workspace run %d event git.lit_spawns = %d, want 1", run, got)
		}
	}
}
