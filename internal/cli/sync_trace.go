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

// recordEngineOpenContentionTrace leaves a durable sync-trace record when a
// command's store open failed against a co-resident holder — the record that
// did not exist when links-sync-pgct.11.1 was hit in the field, leaving the
// starved command uncorrelatable against the sync traces the mirror DOES
// write. It self-gates on the one contention sentinel every store lock stamps,
// so call sites hand it every open failure and exactly the contention class is
// recorded. [LAW:single-enforcer] whether an open failure earns a durable
// record is decided here, once. The command is read from the process's own
// argv: this trace exists to say which command was starved, and argv is that
// fact's only source at this seam.
func recordEngineOpenContentionTrace(ws workspace.Info, err error) {
	if !errors.Is(err, store.ErrWorkspaceBusy) {
		return
	}
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   formatCommand(os.Args[1:]),
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
