package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// firstError returns the first non-nil error. pushOutcomeOf's canceled arm
// matches on either of two mutually exclusive carriers (the could-not-attempt
// error and the ran-and-failed pushErr) and needs whichever one fired.
func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Push decisions, shared by the durable sync trace and the push-outcome
// marker so the two records can never spell one outcome two ways.
// [LAW:one-source-of-truth] The skip decisions ("no_sync_remote",
// "remote_empty") ride through pushOutcomeOf from syncPushOutcome.skip,
// whose typed values are already the trace's vocabulary.
//
// "canceled" and "workspace_busy" exist because the FAILING banner and the
// owner page key on the decision: an operator tearing an attempt down
// (ctrl-C, process shutdown) and a mirror timing out behind another lit
// process legitimately holding the workspace's one write engine (a long
// reconcile or import — this repo's own prescribed serialized workflow) are
// endings of THIS attempt, not evidence the push channel is degraded, and
// recording them as "error" pages the owner and banners every later command
// over a healthy workspace whose next attempt pushes fine. The class is
// decided once here, from the error values, for both consumers.
// [LAW:parse-dont-validate]
const (
	pushDecisionPushed        = "pushed"
	pushDecisionError         = "error"
	pushDecisionCanceled      = "canceled"
	pushDecisionWorkspaceBusy = "workspace_busy"
)

// pushOutcomeRecord is how the last completed push attempt ended — the cheap,
// engine-free answer to "are pushes working?" that the sync traces (a
// one-file-per-decision audit log) are too heavy to give on every command.
// It plays the role fetch-success.last plays for fetch freshness: the traces
// answer "what happened, in order"; the marker answers "where do things stand
// now". Remote and Branch are empty when the attempt failed before resolving
// them. [LAW:types-are-the-program] the banner's predicate is a field read,
// not a trace-log scan.
type pushOutcomeRecord struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Branch   string `json:"branch,omitempty"`
	// ObservedBy is the lit version that ran the attempt. Reason is frozen at
	// write time and replayed verbatim, so this stamp is what tells a reader
	// whether the binary has changed since the verdict was reached. Empty for a
	// dev build, an unreadable build stamp, or a record written before the field
	// existed: "no observer to compare", not an error.
	ObservedBy string `json:"observed_by,omitempty"`
}

// failed reports whether the record describes a push attempt that did not
// land — the one condition the staleness banner warns on. Skip decisions
// (no remote, empty remote), a deliberate cancellation, and a legitimately
// busy workspace are healthy or self-resolving states, not failures, and an
// unknown decision written by a newer binary is deliberately not treated as
// one.
func (r pushOutcomeRecord) failed() bool {
	return r.Decision == pushDecisionError
}

// pushOutcomeMarkerPath is the single marker for the last push attempt's
// outcome; its modification time is when that attempt completed.
// [LAW:one-source-of-truth]
func pushOutcomeMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "push-outcome.last")
}

// pushOutcomeOf derives the marker record from one push-attempt completion: a
// deliberate cancellation, a workspace legitimately busy, a could-not-attempt
// error (reconcile, remote resolution, refs check, or a mirror dying before
// its attempt), a push that ran and failed, a deliberate skip, or a push that
// landed. Pure, so the derivation is table-testable apart from the file write.
// [LAW:dataflow-not-control-flow] The canceled/busy classes are matched before
// the generic error arms so the two failure-keyed consumers (banner, owner
// page) can never see them as channel degradation, whichever stage of the
// attempt they interrupted.
func pushOutcomeOf(outcome syncPushOutcome, err error, observedBy string) pushOutcomeRecord {
	rec := classifyPushOutcome(outcome, err)
	// [LAW:dataflow-not-control-flow] Every outcome is stamped, so an empty
	// ObservedBy means only "no observer", never "not a failure".
	rec.ObservedBy = observedBy
	return rec
}

// classifyPushOutcome decides which decision an attempt's ending was, and the
// remote, branch and reason that name it; pushOutcomeOf dates what it classifies.
func classifyPushOutcome(outcome syncPushOutcome, err error) pushOutcomeRecord {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(outcome.pushErr, context.Canceled):
		return pushOutcomeRecord{
			Decision: pushDecisionCanceled,
			Reason:   firstError(err, outcome.pushErr).Error(),
			Remote:   outcome.remote,
			Branch:   outcome.branch,
		}
	case errors.Is(err, store.ErrWorkspaceBusy):
		return pushOutcomeRecord{Decision: pushDecisionWorkspaceBusy, Reason: err.Error()}
	case err != nil:
		return pushOutcomeRecord{Decision: pushDecisionError, Reason: err.Error()}
	case outcome.pushErr != nil:
		return pushOutcomeRecord{
			Decision: pushDecisionError,
			Reason:   outcome.pushErr.Error(),
			Remote:   outcome.remote,
			Branch:   outcome.branch,
		}
	case outcome.skip != syncTargetReady:
		return pushOutcomeRecord{Decision: string(outcome.skip), Remote: outcome.remote}
	default:
		return pushOutcomeRecord{
			Decision: pushDecisionPushed,
			Remote:   outcome.remote,
			Branch:   outcome.branch,
		}
	}
}

// completePushAttempt is the one completion seam for how a push attempt ended
// — the attempt that ran (performSyncPush's deferred call, any outcome) and
// the attempt that could not start (a mirror dying before its engine opened, a
// spawner that could not launch a mirror) both flow through here, so the
// push-outcome marker and the owner's out-of-band channel are always fed the
// same record and can never disagree about the same attempt.
// [LAW:single-enforcer] Callers construct nothing themselves: the record is
// derived once, from the same two values, for every producer.
func completePushAttempt(ctx context.Context, ws workspace.Info, outcome syncPushOutcome, attemptErr error) {
	rec := pushOutcomeOf(outcome, attemptErr, runningBinaryVersion())
	recordPushOutcome(ws, rec)
	observePushOutcomeForOwner(ctx, ws, rec)
}

// recordPushOutcome writes the marker atomically (writeMarkerAtomic: an explicit
// `lit sync push` and a detached mirror can complete near-simultaneously, and a
// reader must see one whole record or the other). A write failure is surfaced to
// stderr and not returned — the push's own success or failure is already decided
// and must not be re-colored by bookkeeping. [LAW:no-silent-failure]
func recordPushOutcome(ws workspace.Info, rec pushOutcomeRecord) {
	err := func() error {
		payload, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return writeMarkerAtomic(ws, pushOutcomeMarkerPath(ws), append(payload, '\n'))
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: push-outcome marker not written: %v\n", err)
	}
}

// runningBinaryVersion reads this binary's version at the boundary, so the
// renderers below stay pure over a string. [LAW:effects-at-boundaries] An
// unreadable version is reported on stderr and returned as "", the value a dev
// build has: bookkeeping must not fail a push whose outcome is already decided.
// [LAW:no-silent-failure]
func runningBinaryVersion() string {
	info, err := version.Get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: push-outcome provenance unavailable, build version unreadable: %v\n", err)
		return ""
	}
	return info.Version
}

// pushOutcomeProvenance renders the clause that stops a recorded failure reading
// as a live one: when the binary has changed since the attempt, it says so. It
// claims only a change, never which version is newer, because after a rollback
// the recorded verdict may still hold. Empty when there is nothing to compare or
// nothing changed; callers interpolate it unconditionally.
// [LAW:dataflow-not-control-flow] [LAW:single-enforcer] the banner and doctor
// both date a recorded failure through this one function.
func pushOutcomeProvenance(rec pushOutcomeRecord, running string) string {
	if rec.ObservedBy == "" || running == "" || rec.ObservedBy == running {
		return ""
	}
	return fmt.Sprintf(" (recorded by lit %s; you are now running %s, so the binary has changed since this verdict)", rec.ObservedBy, running)
}

// lastPushOutcome reads the marker. ok is false when no push has ever been
// attempted on this workspace — absence is a real, distinct state, mirroring
// lastFetchSuccessAge. Any other failure (unreadable file, corrupt JSON) is a
// real operational fault, surfaced to stderr rather than folded into the same
// quiet ok=false. [LAW:no-silent-failure] at reads what
// [LAW:one-source-of-truth] the file's own mtime records: when the attempt
// completed.
func lastPushOutcome(ws workspace.Info, now time.Time) (rec pushOutcomeRecord, age time.Duration, ok bool) {
	path := pushOutcomeMarkerPath(ws)
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "lit: push-outcome marker unreadable: %v\n", err)
		}
		return pushOutcomeRecord{}, 0, false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: push-outcome marker unreadable: %v\n", err)
		return pushOutcomeRecord{}, 0, false
	}
	if err := json.Unmarshal(payload, &rec); err != nil {
		fmt.Fprintf(os.Stderr, "lit: push-outcome marker corrupt: %v\n", err)
		return pushOutcomeRecord{}, 0, false
	}
	return rec, now.Sub(info.ModTime()), true
}
