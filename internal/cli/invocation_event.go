package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
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

// beginInvocation opens the event for an invocation starting at now in dir,
// named "lit" until the dispatch reaches a command and names it.
func beginInvocation(now time.Time, dir WorkingDir) *telemetry.Recorder {
	// A cwd that cannot be read is recorded as empty: every command that needs
	// the cwd fails on it loudly, and that failure is on this same event.
	cwd, _ := dir.Path()
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

// invocationContext is the context an invocation's dispatch runs under: its
// open event, and the observers that put Dolt's own facts on that event,
// which reach lit only as text through the SQL driver, while the context
// reaches the work Dolt does under it. One puts on the event every git run
// its context ended, Dolt's git included. The other puts there every
// recoverable journal problem a store opened under it reported, such as an
// index a read open passed over to read the whole journal, in place of the
// logrus line on stderr Dolt writes by default. Each key starts as [] so an
// invocation that met none says so, not nothing. [LAW:nothing-unseen]
// [LAW:single-enforcer]
func invocationContext(ctx context.Context, recorder *telemetry.Recorder) context.Context {
	event := telemetry.With(ctx, recorder)
	telemetry.Append(event, "git.cancels")
	telemetry.Append(event, "dolt.journal_warnings")
	ctx = nbs.WithJournalWarningObserver(event, func(warning nbs.JournalWarning) {
		telemetry.Append(event, "dolt.journal_warnings", journalWarningValue(warning))
	})
	return gitcmd.WithObserver(ctx, func(cancelled *gitcmd.Cancelled) {
		telemetry.Append(event, "git.cancels", gitCancelValue(cancelled))
	})
}

// journalWarningValue is one dolt.journal_warnings occurrence: the store whose
// journal reported it, since one invocation can open several, and what it
// reported.
func journalWarningValue(w nbs.JournalWarning) map[string]any {
	return map[string]any{
		"dolt.journal_warning.store": w.Store,
		"dolt.journal_warning.error": telemetry.ErrorText(w.Err),
	}
}

// gitCancelValue is one git.cancels occurrence. survivors is null when no
// count was taken.
func gitCancelValue(c *gitcmd.Cancelled) map[string]any {
	return map[string]any{
		"git.cancel.cause":      telemetry.ErrorText(c.Cause),
		"git.cancel.processes":  c.Kill.Processes,
		"git.cancel.survivors":  c.Kill.Survivors,
		"git.cancel.kill_error": telemetry.ErrorText(c.Kill.Err),
		"git.cancel.output_cut": c.OutputCut,
	}
}

// invocationEnd is how an invocation ended: Run's returned error, or the
// panic it died of. A failed command's exit code and reason are the ones
// WriteCommandError prints for the same error, read off one classification.
// [LAW:one-source-of-truth]
func invocationEnd(at time.Time, err error, panicked any) telemetry.End {
	end := telemetry.End{At: at, Outcome: telemetry.OutcomeOK, ExitCode: ExitOK}
	switch {
	case panicked != nil:
		end.Outcome = telemetry.OutcomePanic
		end.ExitCode = goPanicExitCode
		end.Error = fmt.Sprint(panicked)
	case err != nil:
		reason := commandErrorReason(err)
		end.Outcome = telemetry.OutcomeError
		end.ExitCode = reason.exit
		end.Reason = reason.String()
		end.Error = err.Error()
		end.Facts, end.Counts = model.ErrorRecord(err)
	}
	return end
}

// interruptedEnd is an invocation the interrupt guard hard-exited with code:
// it was still running when the grace after signal ran out.
func interruptedEnd(at time.Time, signal string, code int) telemetry.End {
	return telemetry.End{
		At:       at,
		Outcome:  telemetry.OutcomeInterrupted,
		ExitCode: code,
		Error:    fmt.Sprintf("still running when the grace after %s ran out; hard-exited with %d", signal, code),
	}
}

// invocationRecord writes one invocation's event, once. An invocation can end
// two ways at once — its stack unwinding just as the interrupt guard's hard
// exit fires — and its event is one record: the first end to arrive is
// written, and a later one returns only once that write has landed, so the
// hard exit cannot cut it short. [LAW:one-source-of-truth]
//
// gitSpawnsBefore is the env's git count when the invocation began, so the
// event's git.lit_spawns is this invocation's even when one Env serves
// several in-process runs one after another.
type invocationRecord struct {
	once            sync.Once
	recorder        *telemetry.Recorder
	stderr          io.Writer
	env             environ.Env
	gitSpawnsBefore int64
}

func (r *invocationRecord) close(end telemetry.End) {
	r.once.Do(func() {
		// Every git lit itself started for the invocation, zero included, on
		// every way it can end. Dolt's own git inside a fetch or push is not
		// started through the Env and is not in it. [LAW:nothing-unseen]
		counts := map[string]int{"git.lit_spawns": int(r.env.GitSpawns() - r.gitSpawnsBefore)}
		maps.Copy(counts, end.Counts)
		end.Counts = counts
		recordInvocation(r.stderr, r.env, r.recorder.Finish(end))
	})
}

// recordInvocation is the export edge: the one place an invocation's event
// leaves the process. A failure to write it is reported on stderr and never
// becomes the command's failure — the command's own work is already decided.
// [LAW:single-enforcer] [LAW:no-silent-failure]
func recordInvocation(stderr io.Writer, env environ.Env, ev telemetry.Event) {
	if err := writeEvent(stderr, env, ev); err != nil {
		fmt.Fprintf(stderr, "lit: invocation event not recorded: %v\n", err)
	}
}

func writeEvent(stderr io.Writer, env environ.Env, ev telemetry.Event) error {
	dir, err := config.StateDir(env)
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
