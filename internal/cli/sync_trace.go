package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/trace"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// syncTraceKind is this trace kind's directory name under
// <storageDir>/traces/, alongside the "automation" and "workflows" kinds.
// [LAW:one-source-of-truth]
const syncTraceKind = "sync"

// syncTraceRecord is one durable record of a sync- or init-relevant decision:
// what command reached it, what it decided, and the result. Unlike
// automationTraceRecord (recorded only when LNKS_AUTOMATION_TRIGGER is set),
// a syncTraceRecord is written for every occasion this package calls
// recordSyncTrace on, whether the occasion was run directly or fired under
// automation — Trigger just says which, read fresh from the same environment
// automationContext reads, so the two trace kinds can never disagree about
// what triggered a given occasion. [LAW:one-source-of-truth]
type syncTraceRecord struct {
	ID          string            `json:"id"`
	RecordedAt  string            `json:"recorded_at"`
	WorkspaceID string            `json:"workspace_id"`
	Command     string            `json:"command"`
	Decision    string            `json:"decision"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	Trigger     string            `json:"trigger,omitempty"`
	BuildNote   string            `json:"build_note,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// syncTraceDir mirrors automationTraceDir/workflows' firing-trace directory
// helper — the on-disk home a caller (a test, `lit doctor`) can list without
// re-deriving trace.Dir's join. [LAW:one-source-of-truth]
func syncTraceDir(ws workspace.Info) string {
	return trace.Dir(ws.StorageDir, syncTraceKind)
}

// recordSyncTrace writes one syncTraceRecord UNCONDITIONALLY — the durable
// counterpart to maybeRecordAutomatedCommandTrace's LNKS_AUTOMATION_TRIGGER-gated
// write. It is called at every sync/init decision this package makes, whether
// the occasion was triggered by automation or run directly, so an interactive
// `lit init` or `lit sync fetch/pull/push/reconcile` leaves the same durable
// trail an automated one does. A write failure is returned, never swallowed
// here — each call site decides how to surface it (my callers print a
// best-effort stderr line, matching how a maybeRecordAutomatedCommandTrace
// write failure is already handled at every existing call site).
// [LAW:no-silent-failure] [LAW:one-source-of-truth] every sync/init decision
// trace uses one shared record shape and the one collision-retry writer
// (trace.Write) every trace kind in this codebase shares.
func recordSyncTrace(ws workspace.Info, record syncTraceRecord) (string, error) {
	record.Command = strings.TrimSpace(record.Command)
	record.Decision = strings.TrimSpace(record.Decision)
	record.Status = strings.TrimSpace(record.Status)
	record.Reason = strings.TrimSpace(record.Reason)
	record.Trigger = readAutomationContextFromEnv().Trigger
	record.BuildNote = strings.TrimSpace(record.BuildNote)
	record.Metadata = compactTraceMetadata(record.Metadata)
	_, path, err := trace.Write(ws.StorageDir, syncTraceKind, trace.Slug(record.Command), func(id string, recordedAt time.Time) ([]byte, error) {
		record.ID = id
		record.RecordedAt = recordedAt.Format(time.RFC3339Nano)
		record.WorkspaceID = ws.WorkspaceID
		payload, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(payload, '\n'), nil
	})
	return path, err
}

// recordSyncTraceLogged writes a fully-formed syncTraceRecord and prints a
// best-effort stderr line on a write failure, never returning the error — the
// shared tail every call site that has already computed its own
// decision/status/reason (rather than deriving them from a single err, which
// recordSyncCommandTrace covers) still needs. [LAW:no-silent-failure]
// [LAW:one-source-of-truth] one place decides how a trace-write failure is
// surfaced, for every caller that builds its own record.
func recordSyncTraceLogged(ws workspace.Info, record syncTraceRecord) {
	if _, traceErr := recordSyncTrace(ws, record); traceErr != nil {
		fmt.Fprintf(os.Stderr, "lit: %s trace not recorded: %v\n", record.Command, traceErr)
	}
}

// recordSyncCommandTrace is the shape most explicit sync commands need: one
// named decision on success, or the uniform "error" decision (with err's text
// as the reason) on failure — so a caller never has to invent its own error
// label. A trace-write failure is reported to stderr and never returned: the
// command's own success or failure is already decided by err, independent of
// whether recording it durably also succeeded. [LAW:no-silent-failure]
func recordSyncCommandTrace(ws workspace.Info, command, decision string, err error, metadata map[string]string) {
	status := "ok"
	reason := ""
	if err != nil {
		status = "error"
		decision = "error"
		reason = err.Error()
	}
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   command,
		Decision:  decision,
		Status:    status,
		Reason:    reason,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  metadata,
	})
}

// engineOpenContentionError marks a failure as a store acquisition starved by
// a co-resident holder. The store's ErrWorkspaceBusy sentinel alone cannot
// carry that fact — the mutation family's mid-command commit-lock contention
// wraps the same sentinel and is traced by its own handlers — so the
// acquisition boundaries stamp their failures with this type and the dispatch
// boundary reads the stamp. The stamp also carries WHERE the contention
// happened: only the boundary knows which store it failed against, and an
// ambient cwd re-derivation downstream files `ls --at <foreign>` contention
// under the wrong workspace or nowhere. [LAW:parse-dont-validate] the check's
// proof — and its filing location — travel in the type, so neither can be
// re-derived (wrongly) downstream.
type engineOpenContentionError struct {
	err error
	ws  workspace.Info
}

func (e engineOpenContentionError) Error() string { return e.err.Error() }
func (e engineOpenContentionError) Unwrap() error { return e.err }

// markEngineOpenContention stamps an acquisition-boundary failure when it is
// the holder-contention class, binding it to the workspace whose store was
// contended; every other error passes through untouched.
func markEngineOpenContention(err error, ws workspace.Info) error {
	if err == nil || !errors.Is(err, store.ErrWorkspaceBusy) {
		return err
	}
	return engineOpenContentionError{err: err, ws: ws}
}

// commandPath keeps the tokens that name a command — up to the first
// flag-shaped token, capped at two, every family being at most two words
// deep — and drops everything else: flag values and positionals are the
// invocation's payload, not its name.
func commandPath(args []string) []string {
	path := args
	for i, arg := range path {
		if i == 2 || strings.HasPrefix(arg, "-") {
			path = path[:i]
			break
		}
	}
	return path
}

// infoForLocation builds the trace-filing identity of a store addressed by
// location alone — no checkout, no cwd: its path geometry plus the
// workspace id its config records. A location whose config cannot be read
// still files under the right storage dir, with the id left empty — the
// filing location is the fact that matters.
func infoForLocation(loc workspace.Location) workspace.Info {
	info := workspace.Info{Location: loc}
	if cfg, err := workspace.ReadConfig(loc.ConfigPath); err == nil {
		info.WorkspaceID = cfg.WorkspaceID
	}
	return info
}

// recordEngineOpenContentionTrace leaves a durable sync-trace record when a
// command's store acquisition failed against a co-resident holder — the
// record that did not exist when links-sync-pgct.11.1 was hit in the field,
// leaving the starved command uncorrelatable against the sync traces the
// mirror DOES write. It fires only on the acquisition-boundary stamp above,
// never on the bare busy sentinel — the mutation family's mid-command
// commit-lock contention already leaves its handler's own trace, and a second
// record here would give one event two stories. [LAW:single-enforcer] whether
// an error earns this record is decided here, once, at the dispatch boundary
// — the one seam where the verbatim invocation is known (never ambient
// os.Args, which lies for the in-process Run callers the test suite uses).
// The trace files under the workspace the stamp carries — the contended
// store's own, which is where the holder's traces live. It records the
// command PATH, never the full argv: attribution needs which command was
// starved, and the verbatim invocation carries free-text payloads (titles,
// bodies, pasted secrets) that never reached the store and must not be
// durably persisted here as a side effect of the store being busy.
func recordEngineOpenContentionTrace(args []string, err error) {
	var open engineOpenContentionError
	if !errors.As(err, &open) {
		return
	}
	recordSyncTraceLogged(open.ws, syncTraceRecord{
		Command:   formatCommand(commandPath(args)),
		Decision:  commandErrorReason(err),
		Status:    "error",
		Reason:    err.Error(),
		BuildNote: resolveBuildStatusNote(time.Now()),
	})
}

// recordSyncHeldTrace records a held, agent-actionable outcome (a prose
// conflict or an unrelated-histories divergence) — not a failure of the
// operation itself, which ran to completion and definitively determined this
// state, so status is "ok" like every other completed decision; a status of
// "error" is reserved for the operation not running to completion at all.
// The reason is the failure's own domain-level whatLine(), never the full
// multi-paragraph agent-instructions block Error() renders — a trace record
// is a durable one-line-per-decision log, not a copy of the block already
// printed to the operator. [LAW:one-source-of-truth] BuildNote is read off
// the failure the caller already resolved, so it is not recomputed here.
func recordSyncHeldTrace(ws workspace.Info, command string, failure SyncFailureError, metadata map[string]string) {
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   command,
		Decision:  string(failure.Failure.Class),
		Status:    "ok",
		Reason:    failure.Failure.whatLine(),
		BuildNote: failure.Failure.BuildNote,
		Metadata:  metadata,
	})
}
