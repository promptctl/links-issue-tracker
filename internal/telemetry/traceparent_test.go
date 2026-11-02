package telemetry

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

var spanIDShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Every event names its own span, and no two events share one.
func TestBeginMintsASpanIDPerEvent(t *testing.T) {
	first := Begin(Event{}).Finish(End{})
	second := Begin(Event{}).Finish(End{})
	for _, ev := range []Event{first, second} {
		if !spanIDShape.MatchString(ev.SpanID) {
			t.Errorf("SpanID = %q, want 16 lowercase hex digits", ev.SpanID)
		}
		if ev.ParentSpanID != "" {
			t.Errorf("ParentSpanID = %q on an invocation nothing spawned", ev.ParentSpanID)
		}
	}
	if first.SpanID == second.SpanID {
		t.Errorf("two events share span id %q", first.SpanID)
	}
}

// The traceparent an invocation hands a process it spawns, read by that
// process, begins an event in the spawner's trace under the spawner's span.
func TestAnEventBegunUnderATraceparentJoinsTheSpawnersTrace(t *testing.T) {
	spawner := Begin(Event{})
	parent, err := ParseTraceparent(Traceparent(With(context.Background(), spawner)))
	if err != nil {
		t.Fatalf("the spawner's own traceparent is unreadable: %v", err)
	}
	spawned := BeginUnder(Event{}, parent).Finish(End{})
	spawnerEvent := spawner.Finish(End{})
	if spawned.TraceID != spawnerEvent.TraceID {
		t.Errorf("spawned trace id = %q, want the spawner's %q", spawned.TraceID, spawnerEvent.TraceID)
	}
	if spawned.ParentSpanID != spawnerEvent.SpanID {
		t.Errorf("spawned parent span = %q, want the spawner's span %q", spawned.ParentSpanID, spawnerEvent.SpanID)
	}
	if spawned.SpanID == spawnerEvent.SpanID {
		t.Errorf("the spawned event reuses the spawner's span id %q", spawned.SpanID)
	}
}

// Of every shape a TRACEPARENT value can take, one is read and the rest are
// refused with their reason, never half-read.
func TestParseTraceparentReadsOnlyAVersion00Value(t *testing.T) {
	const trace, span = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	tests := []struct {
		name  string
		value string
		want  Parent
		err   string
	}{
		{name: "sampled", value: "00-" + trace + "-" + span + "-01", want: Parent{TraceID: trace, SpanID: span}},
		{name: "unsampled", value: "00-" + trace + "-" + span + "-00", want: Parent{TraceID: trace, SpanID: span}},
		{name: "empty", value: "", err: "is not 00-"},
		{name: "a field missing", value: "00-" + trace + "-" + span, err: "is not 00-"},
		{name: "a field too long", value: "00-" + trace + "0-" + span + "-01", err: "is not 00-"},
		{name: "a fifth field", value: "00-" + trace + "-" + span + "-01-extra", err: "is not 00-"},
		{name: "uppercase hex", value: "00-" + strings.ToUpper(trace) + "-" + span + "-01", err: "outside 0-9a-f"},
		{name: "non-hex", value: "00-" + strings.Repeat("g", 32) + "-" + span + "-01", err: "outside 0-9a-f"},
		{name: "a version lit does not write", value: "01-" + trace + "-" + span + "-01", err: "only version 00"},
		{name: "the forbidden version", value: "ff-" + trace + "-" + span + "-01", err: "only version 00"},
		{name: "all-zero trace id", value: "00-" + strings.Repeat("0", 32) + "-" + span + "-01", err: "all-zero"},
		{name: "all-zero parent id", value: "00-" + trace + "-" + strings.Repeat("0", 16) + "-01", err: "all-zero"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTraceparent(tc.value)
			if tc.err == "" {
				if err != nil || got != tc.want {
					t.Fatalf("ParseTraceparent(%q) = %+v, %v; want %+v", tc.value, got, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("ParseTraceparent(%q) error = %v; want it to say %q", tc.value, err, tc.err)
			}
			if !got.IsZero() {
				t.Fatalf("ParseTraceparent(%q) returned %+v beside its error", tc.value, got)
			}
		})
	}
}
