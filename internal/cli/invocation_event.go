package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// eventsFileName is the per-user file every invocation's event is appended
// to, one JSON line each, under config.StateDir. One file for every checkout
// and every repository: an invocation outside any workspace — `lit version`,
// an unknown command — leaves its event too, and the cwd on each event says
// where it ran.
const eventsFileName = "events.jsonl"

// eventsMaxBytes caps the events file. Rotation keeps one previous
// generation, so the two together hold at most about twice this.
const eventsMaxBytes = 8 * 1024 * 1024

// eventsSink is where recordInvocation writes: the local file.
const eventsSink = "file"

// goPanicExitCode is the exit status the Go runtime gives a process that dies
// of a panic, which is how an invocation that panics ends.
const goPanicExitCode = 2

// beginInvocation opens the event for an invocation starting at now, named
// "lit" until the dispatch reaches a command and names it.
func beginInvocation(now time.Time) *telemetry.Recorder {
	// A cwd that cannot be read is recorded as empty: every command that needs
	// the cwd fails on it loudly, and that failure is on this same event.
	cwd, _ := os.Getwd()
	return telemetry.Begin(telemetry.Event{
		Event:     "lit",
		Service:   "lit",
		Version:   version.Version,
		Commit:    version.Commit,
		PID:       os.Getpid(),
		Cwd:       cwd,
		StartedAt: now,
	})
}

// invocationEnd is how an invocation ended: Run's returned error, or the
// panic it died of. The exit code and reason are the ones WriteCommandError
// prints for the same error, read from the same two functions.
// [LAW:one-source-of-truth]
func invocationEnd(at time.Time, err error, panicked any) telemetry.End {
	end := telemetry.End{At: at, Outcome: telemetry.OutcomeOK, ExitCode: ExitCode(err)}
	switch {
	case panicked != nil:
		end.Outcome = telemetry.OutcomePanic
		end.ExitCode = goPanicExitCode
		end.Error = fmt.Sprint(panicked)
	case err != nil:
		end.Outcome = telemetry.OutcomeError
		end.Reason = string(commandErrorReason(err))
		end.Error = err.Error()
	}
	return end
}

// interruptedEnd is an invocation the interrupt guard hard-exited with code:
// it was still running when the grace after an interrupt ran out.
func interruptedEnd(at time.Time, code int) telemetry.End {
	return telemetry.End{
		At:       at,
		Outcome:  telemetry.OutcomeInterrupted,
		ExitCode: code,
		Error:    fmt.Sprintf("still running when the grace after an interrupt ran out; hard-exited with %d", code),
	}
}

// recordInvocation is the export edge: the one place an invocation's event
// leaves the process. A failure to write it is reported on stderr and never
// becomes the command's failure — the command's own work is already decided.
// [LAW:single-enforcer] [LAW:no-silent-failure]
func recordInvocation(stderr io.Writer, ev telemetry.Event) {
	if err := writeEvent(stderr, ev); err != nil {
		fmt.Fprintf(stderr, "lit: invocation event not recorded: %v\n", err)
	}
}

func writeEvent(stderr io.Writer, ev telemetry.Event) error {
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	// Owner-only: an event's error can quote what was typed on the command
	// line, and its cwd says where its owner works.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(dir, eventsFileName)
	rotated, rotateErr := rotateLog(path, eventsMaxBytes)
	ev.Sink = eventsSink
	ev.SinkRotation, ev.SinkRotationError = rotationFacts(rotated, rotateErr)
	if rotateErr != nil {
		fmt.Fprintf(stderr, "lit: %s rotation failed (%v); the file keeps growing past its cap\n", path, rotateErr)
	}
	line, err := telemetry.Line(ev)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	// One write per event: an O_APPEND write lands whole at the end of the
	// file, so invocations finishing at once in other checkouts never
	// interleave their lines.
	_, writeErr := file.Write(line)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write events file: %w", err)
	}
	return nil
}

// rotationFacts is what rotateLog's result puts on the event.
func rotationFacts(rotated bool, err error) (telemetry.Rotation, string) {
	switch {
	case err != nil:
		return telemetry.RotationFailed, err.Error()
	case rotated:
		return telemetry.RotationRotated, ""
	}
	return telemetry.RotationNone, ""
}
