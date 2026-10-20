package telemetry

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

var traceIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Each event gets its own W3C trace id.
func TestBeginMintsAFreshTraceIDPerEvent(t *testing.T) {
	first := Begin(Event{}).Finish(End{})
	second := Begin(Event{}).Finish(End{})
	for _, ev := range []Event{first, second} {
		if !traceIDShape.MatchString(ev.TraceID) {
			t.Errorf("TraceID = %q, want 32 lowercase hex digits", ev.TraceID)
		}
	}
	if first.TraceID == second.TraceID {
		t.Errorf("two events share trace id %q", first.TraceID)
	}
}

// What the command adds through the context lands on the event Finish returns,
// beside the identity Begin was given and the end Finish was given.
func TestFactsAddedThroughTheContextLandOnTheFinishedEvent(t *testing.T) {
	started := time.Date(2026, 10, 15, 6, 0, 0, 0, time.FixedZone("MDT", -6*60*60))
	recorder := Begin(Event{Event: "lit", Service: "lit", StartedAt: started})
	ctx := With(context.Background(), recorder)

	Command(ctx, "lit next")
	if got := EventName(ctx); got != "lit next" {
		t.Errorf("EventName = %q, want lit next", got)
	}
	Flags(ctx, []string{"all"})
	Set(ctx, "next.outcome", "no_work")
	Count(ctx, "next.rows_gathered", 0)

	ev := recorder.Finish(End{At: started.Add(1500 * time.Millisecond), Outcome: OutcomeError, ExitCode: 6, Reason: "no_ready_work", Error: "no ready work"})
	if ev.Event != "lit next" || ev.Service != "lit" {
		t.Errorf("Event, Service = %q, %q; want lit next, lit", ev.Event, ev.Service)
	}
	if len(ev.Flags) != 1 || ev.Flags[0] != "all" {
		t.Errorf("Flags = %q, want [all]", ev.Flags)
	}
	if ev.Facts["next.outcome"] != "no_work" {
		t.Errorf("Facts[next.outcome] = %v, want no_work", ev.Facts["next.outcome"])
	}
	if n, ok := ev.Counts["next.rows_gathered"]; !ok || n != 0 {
		t.Errorf("Counts[next.rows_gathered] = %d, %v; want a recorded zero", n, ok)
	}
	if ev.DurationMS != 1500 {
		t.Errorf("DurationMS = %d, want 1500", ev.DurationMS)
	}
	if ev.StartedAt.Location() != time.UTC || !ev.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v in UTC", ev.StartedAt, started)
	}
	if ev.Outcome != OutcomeError || ev.ExitCode != 6 || ev.Reason != "no_ready_work" || ev.Error != "no ready work" {
		t.Errorf("end fields = %q %d %q %q, want error 6 no_ready_work, no ready work", ev.Outcome, ev.ExitCode, ev.Reason, ev.Error)
	}
}

// What can occur more than once keeps every occurrence, in order, and a key
// nothing was appended to reads as an empty list rather than absent, on the
// event and on its line.
func TestAppendKeepsEveryValueInOrder(t *testing.T) {
	recorder := Begin(Event{})
	ctx := With(context.Background(), recorder)
	Append(ctx, "reroot.follows")
	empty := recorder.Finish(End{})
	Append(ctx, "reroot.follows", "push")
	Append(ctx, "reroot.follows", "receive", "pull")
	ev := recorder.Finish(End{})
	if got, ok := empty.Occurrences["reroot.follows"]; !ok || got == nil || len(got) != 0 {
		t.Errorf("after an empty Append, Occurrences[reroot.follows] = %#v; want an empty list", got)
	}
	if line, err := Line(empty); err != nil || !strings.Contains(string(line), `"occurrences":{"reroot.follows":[]}`) {
		t.Errorf("the empty Append's line = %s, %v; want occurrences carrying reroot.follows as []", line, err)
	}
	got := ev.Occurrences["reroot.follows"]
	if len(got) != 3 || got[0] != "push" || got[1] != "receive" || got[2] != "pull" {
		t.Errorf("Occurrences[reroot.follows] = %#v; want [push receive pull]", got)
	}
}

// A key Set replaces and a key Append accumulates are separate, so the same
// name written both ways keeps both values, whichever came first.
func TestASetAndAnAppendOfOneKeyBothSurvive(t *testing.T) {
	for _, setFirst := range []bool{true, false} {
		recorder := Begin(Event{})
		ctx := With(context.Background(), recorder)
		if setFirst {
			Set(ctx, "push.outcomes", "decided")
		}
		Append(ctx, "push.outcomes", "landed")
		if !setFirst {
			Record(ctx, map[string]any{"push.outcomes": "decided"}, nil)
		}
		ev := recorder.Finish(End{})
		if ev.Facts["push.outcomes"] != "decided" || len(ev.Occurrences["push.outcomes"]) != 1 || ev.Occurrences["push.outcomes"][0] != "landed" {
			t.Errorf("set first %t: Facts = %#v, Occurrences = %#v; want the set value and the appended one both kept", setFirst, ev.Facts, ev.Occurrences)
		}
	}
}

// A finished event is a value: a fact added after Finish does not reach it.
func TestAFinishedEventDoesNotChangeAfterwards(t *testing.T) {
	recorder := Begin(Event{})
	ctx := With(context.Background(), recorder)
	ev := recorder.Finish(End{})
	Set(ctx, "late", true)
	Count(ctx, "late", 1)
	if len(ev.Facts) != 0 || len(ev.Counts) != 0 {
		t.Errorf("finished event changed: facts %v, counts %v", ev.Facts, ev.Counts)
	}
}

// Adding a fact where no invocation is open is a defect in the caller, and it
// says so rather than dropping the fact.
func TestAFactWithNoOpenEventPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Set on a context without an event did not panic")
		}
	}()
	Set(context.Background(), "next.outcome", "no_work")
}

// The written form is one JSON line carrying the default field names, with
// empty flags, counts and facts written as empty rather than left out.
func TestLineIsOneJSONLineInTheDefaultFieldNames(t *testing.T) {
	line, err := Line(Begin(Event{Event: "lit version"}).Finish(End{Outcome: OutcomeOK}))
	if err != nil {
		t.Fatalf("Line error = %v", err)
	}
	if !strings.HasSuffix(string(line), "\n") || strings.Count(string(line), "\n") != 1 {
		t.Fatalf("Line = %q, want exactly one newline-terminated line", line)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatalf("Line is not JSON: %v", err)
	}
	for _, name := range []string{"event", "trace_id", "service", "started_at", "duration_ms", "outcome", "exit_code", "sink", "sink_rotation", "flags", "counts", "facts"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("Line has no %q field: %s", name, line)
		}
	}
	for name, want := range map[string]string{"flags": "[]", "counts": "{}", "facts": "{}"} {
		if got := string(fields[name]); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}
