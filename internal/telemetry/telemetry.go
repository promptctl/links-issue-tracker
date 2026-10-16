// Package telemetry builds the one wide event each lit invocation leaves: a
// single structured record of what ran, how it ended, and the facts the
// command decided along the way.
//
// The dispatcher opens the event when an invocation begins and closes it when
// the invocation ends; code in between only adds facts to it, through the
// context the dispatcher hands down. Nothing here emits: the package performs
// no I/O, and the dispatcher's export edge writes the finished event.
// [LAW:effects-at-boundaries] It imports only the standard library —
// .golangci.yml holds it to that — so any package can add a fact without a
// dependency cycle and without reaching the store.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"
)

// Outcome is how an invocation ended.
type Outcome string

const (
	OutcomeOK    Outcome = "ok"
	OutcomeError Outcome = "error"
	// OutcomePanic is an invocation that panicked; the event is written before
	// the panic carries on.
	OutcomePanic Outcome = "panic"
	// OutcomeInterrupted is an invocation still running when the interrupt
	// guard's grace ran out, ended by a hard exit that never unwound its stack.
	OutcomeInterrupted Outcome = "interrupted"
)

// Rotation is what the export edge did about the events file's size cap
// before appending the event.
type Rotation string

const (
	// RotationNone is a file under its cap, appended to as it was.
	RotationNone Rotation = "none"
	// RotationRotated is a file moved aside at its cap; the event is the first
	// line of the new one.
	RotationRotated Rotation = "rotated"
	// RotationFailed is a rotation that could not run, so the file grows past
	// its cap; sink_rotation_error says why.
	RotationFailed Rotation = "failed"
)

// Event is one invocation's record, in the field names every unit of work
// starts from (event, trace_id, service, started_at, duration_ms, outcome,
// error, sink, counts) plus what a CLI invocation adds: its exit code and
// reason, the flags it was given, and where it ran.
type Event struct {
	// Event is the command that ran, as its full path: "lit next",
	// "lit sync push". An invocation that never reached a command — bare
	// `lit`, an unknown command — is "lit".
	Event   string `json:"event"`
	TraceID string `json:"trace_id"`
	Service string `json:"service"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	PID     int    `json:"pid"`
	Cwd     string `json:"cwd"`
	// Flags are the names of the flags the command was given, never their
	// values: a value can be a title, a body, or a pasted secret.
	Flags      []string  `json:"flags"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	Outcome    Outcome   `json:"outcome"`
	ExitCode   int       `json:"exit_code"`
	// Reason is the machine-readable reason a failed invocation printed beside
	// its exit code; empty on success.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// Sink, SinkRotation, and SinkRotationError are the export edge's own
	// facts, set as it writes the event: where the event went, and what became
	// of the file's size cap on the way. [LAW:nothing-unseen] a telemetry
	// failure is itself telemetry.
	Sink              string   `json:"sink"`
	SinkRotation      Rotation `json:"sink_rotation"`
	SinkRotationError string   `json:"sink_rotation_error,omitempty"`
	// Counts holds the command's counts, zeros included: a zero says the
	// command ran and found none, which an absent key cannot.
	Counts map[string]int `json:"counts"`
	// Facts holds what the command decided, under dot-namespaced keys
	// ("next.outcome").
	Facts map[string]any `json:"facts"`
}

// Recorder is one invocation's event while it is still open.
type Recorder struct {
	mu sync.Mutex
	ev Event
}

// Begin opens an invocation's event from its identity — everything known
// before the command runs — and mints its trace id.
func Begin(identity Event) *Recorder {
	ev := identity
	ev.TraceID = newTraceID()
	ev.Counts = map[string]int{}
	ev.Facts = map[string]any{}
	return &Recorder{ev: ev}
}

// End is how an invocation finished, as the dispatcher saw it.
type End struct {
	At       time.Time
	Outcome  Outcome
	ExitCode int
	Reason   string
	Error    string
	// Facts and Counts are what the error the invocation ended with decided
	// and counted, recorded beside those the command set while it ran.
	Facts  map[string]any
	Counts map[string]int
}

// Finish closes the event and returns it, complete.
func (r *Recorder) Finish(end End) Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := r.ev
	// The duration is read before StartedAt is moved to UTC, which drops the
	// monotonic clock reading a wall-clock step would otherwise corrupt.
	ev.DurationMS = end.At.Sub(ev.StartedAt).Milliseconds()
	ev.StartedAt = ev.StartedAt.UTC()
	ev.Outcome = end.Outcome
	ev.ExitCode = end.ExitCode
	ev.Reason = end.Reason
	ev.Error = end.Error
	ev.Flags = append([]string{}, ev.Flags...)
	ev.Counts = maps.Clone(ev.Counts)
	ev.Facts = maps.Clone(ev.Facts)
	maps.Copy(ev.Facts, end.Facts)
	maps.Copy(ev.Counts, end.Counts)
	return ev
}

// Line is the event as one line of JSONL.
func Line(ev Event) ([]byte, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}
	return append(payload, '\n'), nil
}

type recorderKey struct{}

// With carries r on ctx for everything the invocation runs.
func With(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// from is the invocation's open event. Every unit of work runs under the
// dispatcher, which puts it there, so a context without one is a caller that
// bypassed the dispatcher — a defect, reported as one.
// [LAW:no-silent-failure]
func from(ctx context.Context) *Recorder {
	r, ok := ctx.Value(recorderKey{}).(*Recorder)
	if !ok {
		panic("telemetry: context carries no event; every invocation runs under cli.Run, which opens one")
	}
	return r
}

// Command names the command the invocation reached, by its full path.
func Command(ctx context.Context, path string) {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Event = path
}

// EventName is the command the invocation has reached so far, by its full
// path: the name its event carries.
func EventName(ctx context.Context) string {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ev.Event
}

// Subcommand extends the command path by the subcommand dispatch resolved
// next, for a family that knows the name it matched but not the path above it.
func Subcommand(ctx context.Context, name string) {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Event += " " + name
}

// Flags records the names of the flags the command was given.
func Flags(ctx context.Context, names []string) {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Flags = append([]string{}, names...)
}

// Set records one fact the command decided. A later Set of the same key
// replaces it.
func Set(ctx context.Context, key string, value any) {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Facts[key] = value
}

// ErrorText is err as a fact value: its message, or nil for no error, so the
// event says null rather than an empty string when nothing failed. Every fact
// that records an error records it through this one encoding.
// [LAW:one-source-of-truth]
func ErrorText(err error) *string {
	if err == nil {
		return nil
	}
	text := err.Error()
	return &text
}

// Count records one of the command's counts.
func Count(ctx context.Context, key string, n int) {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Counts[key] = n
}

// Record records facts and counts a value declared about itself, such as a
// result's EventRecord, as if each were Set or Counted.
func Record(ctx context.Context, facts map[string]any, counts map[string]int) {
	r := from(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	maps.Copy(r.ev.Facts, facts)
	maps.Copy(r.ev.Counts, counts)
}

// newTraceID is a W3C Trace Context trace id: 16 random bytes in lowercase hex.
func newTraceID() string {
	var id [16]byte
	// crypto/rand.Read never returns an error; it crashes the program on the
	// one platform failure it cannot recover from.
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}
