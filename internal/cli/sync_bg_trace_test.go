package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// A detached worker is a process of its own, so its event is its own — and,
// started with its spawner's TRACEPARENT, in the spawner's trace, naming the
// spawner's span as its parent. This starts a real worker (the suite's test
// binary, TestMain) and reads its event off the events file, so a mirror push
// that fails is tied back to the write that caused it. [LAW:nothing-unseen]
func TestADetachedWorkersEventJoinsTheTraceOfTheCommandThatSpawnedIt(t *testing.T) {
	isolateEvents(t)
	dir, _ := litRepoForCadence(t)
	spawner := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), spawner)
	stderr := testStderr(t)
	ws, err := resolveWorkspace(ctx, stderr, testEnv(), WorkingDirOf(dir, nil), workspace.PrefixRequest{})
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	// Parent pid 0: the worker has no spawner to wait out, and the spawner
	// here is this test process, which stays up.
	if err := spawnBackgroundMirror(ctx, stderr, testEnv(), ws, 0); err != nil {
		t.Fatalf("spawnBackgroundMirror: %v", err)
	}
	spawnerEvent := spawner.Finish(telemetry.End{})

	deadline := time.Now().Add(suiteStallDeadline)
	for {
		for _, ev := range eventsInFile(t) {
			if ev.Event != "lit "+backgroundMirrorSubcommand {
				continue
			}
			if ev.TraceID != spawnerEvent.TraceID {
				t.Fatalf("worker trace id = %q, want the spawner's %q", ev.TraceID, spawnerEvent.TraceID)
			}
			if ev.ParentSpanID != spawnerEvent.SpanID {
				t.Fatalf("worker parent span = %q, want the spawner's span %q", ev.ParentSpanID, spawnerEvent.SpanID)
			}
			if ev.SpanID == "" || ev.SpanID == spawnerEvent.SpanID {
				t.Fatalf("worker span id = %q, want one of its own beside the spawner's %q", ev.SpanID, spawnerEvent.SpanID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker left no event within %s; events written: %v", suiteStallDeadline, eventsInFile(t))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// eventsInFile is every event written so far; none while the file does not
// exist, which a worker still starting up has not yet written.
func eventsInFile(t *testing.T) []telemetry.Event {
	t.Helper()
	if _, err := os.Stat(eventsPath(t)); err != nil {
		return nil
	}
	return readEvents(t)
}

// An invocation handed a TRACEPARENT joins that trace under its span; one
// handed a value it cannot read starts a trace of its own and says why on its
// event; one handed none has neither.
func TestAnInvocationAdoptsTheTraceparentItWasHanded(t *testing.T) {
	const trace, span = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	isolateEvents(t)
	dir := t.TempDir()

	t.Setenv(telemetry.TraceparentEnvVar, "00-"+trace+"-"+span+"-01")
	runCLIInDir(t, dir, "version")
	ev := lastEvent(t)
	if ev.TraceID != trace || ev.ParentSpanID != span {
		t.Errorf("handed a traceparent, event trace/parent = %q/%q; want %q/%q", ev.TraceID, ev.ParentSpanID, trace, span)
	}
	if _, present := ev.Facts["trace.parent_error"]; present {
		t.Errorf("an adopted traceparent recorded trace.parent_error = %v", ev.Facts["trace.parent_error"])
	}

	t.Setenv(telemetry.TraceparentEnvVar, "not-a-traceparent")
	runCLIInDir(t, dir, "version")
	ev = lastEvent(t)
	if !traceID.MatchString(ev.TraceID) || ev.TraceID == trace || ev.ParentSpanID != "" {
		t.Errorf("handed garbage, event trace/parent = %q/%q; want a fresh trace and no parent", ev.TraceID, ev.ParentSpanID)
	}
	reason, _ := ev.Facts["trace.parent_error"].(string)
	if !strings.Contains(reason, "not-a-traceparent") {
		t.Errorf("trace.parent_error = %v; want it to name the value it refused", ev.Facts["trace.parent_error"])
	}

	if err := os.Unsetenv(telemetry.TraceparentEnvVar); err != nil {
		t.Fatal(err)
	}
	runCLIInDir(t, dir, "version")
	ev = lastEvent(t)
	if ev.ParentSpanID != "" {
		t.Errorf("handed no traceparent, event parent span = %q", ev.ParentSpanID)
	}
	if _, present := ev.Facts["trace.parent_error"]; present {
		t.Errorf("no traceparent recorded trace.parent_error = %v", ev.Facts["trace.parent_error"])
	}
}
