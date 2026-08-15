package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Push decisions, shared by the durable sync trace and the push-outcome
// marker so the two records can never spell one outcome two ways.
// [LAW:one-source-of-truth] The skip decisions ("no_sync_remote",
// "remote_empty") ride through pushOutcomeOf from syncPushOutcome.reason,
// where they are already the trace's vocabulary.
const (
	pushDecisionPushed = "pushed"
	pushDecisionError  = "error"
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
}

// failed reports whether the record describes a push attempt that did not
// land — the one condition the staleness banner warns on. Skip decisions
// (no remote, empty remote) are healthy states, not failures, and an unknown
// decision written by a newer binary is deliberately not treated as one.
func (r pushOutcomeRecord) failed() bool {
	return r.Decision == pushDecisionError
}

// pushOutcomeMarkerPath is the single marker for the last push attempt's
// outcome; its modification time is when that attempt completed.
// [LAW:one-source-of-truth]
func pushOutcomeMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "push-outcome.last")
}

// pushOutcomeOf derives the marker record from one performSyncPush completion:
// a could-not-attempt error (reconcile, remote resolution, refs check), a push
// that ran and failed, a deliberate skip, or a push that landed. Pure, so the
// derivation is table-testable apart from the file write.
// [LAW:dataflow-not-control-flow]
func pushOutcomeOf(outcome syncPushOutcome, err error) pushOutcomeRecord {
	switch {
	case err != nil:
		return pushOutcomeRecord{Decision: pushDecisionError, Reason: err.Error()}
	case outcome.pushErr != nil:
		return pushOutcomeRecord{
			Decision: pushDecisionError,
			Reason:   outcome.pushErr.Error(),
			Remote:   outcome.remote,
			Branch:   outcome.branch,
		}
	case outcome.status == "skipped":
		return pushOutcomeRecord{Decision: outcome.reason, Remote: outcome.remote}
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
	rec := pushOutcomeOf(outcome, attemptErr)
	recordPushOutcome(ws, rec)
	observePushOutcomeForOwner(ctx, ws, rec, attemptErr, outcome.pushErr)
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
