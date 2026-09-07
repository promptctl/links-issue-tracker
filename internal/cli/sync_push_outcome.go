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
	// ObservedBy is the lit version that ran the attempt this record describes.
	// Reason is a sentence that was true when it was written and is rendered
	// verbatim forever after, so without a stamp naming its observer a reader
	// cannot tell a live constraint from an expired one: the field incident was
	// `lit doctor` reporting "this binary supports only up to 4" while running a
	// binary that supports 5, because the text was replayed rather than
	// recomputed. The observer is the one condition behind such a verdict that is
	// cheap to compare and that the remediation itself asks the operator to
	// change, so it travels WITH the sentence it dates.
	// [FRAMING:representation] the record is a map of a past attempt; stamping it
	// is what stops it being read as a map of the present.
	//
	// Empty means the attempt could not name its own version — a dev build
	// (version.Info.Version is "" there) or an unreadable build stamp — and also
	// what every record written before this field existed reads as. Empty is a
	// real domain value, "no observer to compare", never a failure to hide.
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
	// [LAW:dataflow-not-control-flow] Every outcome is stamped, not just the
	// failing ones: which decisions a future reader will want dated is not a fact
	// this derivation knows, and a stamp applied on one arm only is a field whose
	// emptiness would mean two different things.
	rec.ObservedBy = observedBy
	return rec
}

// classifyPushOutcome is the classification half: which decision this ending
// was, and the remote/branch/reason that name it. It is split out so
// pushOutcomeOf stays a TOTAL derivation — inputs to whole record, stamp
// included — rather than handing a half-filled record back to its caller to
// finish. A record completed at the call site is a record some future call site
// forgets to complete. [LAW:single-enforcer] [LAW:decomposition] one sentence
// each: this one classifies, its caller dates what was classified.
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

// runningBinaryVersion reads this binary's version for the provenance stamp,
// mirroring resolveBuildStatusNote's shape: the effect (reading build info) is
// resolved at the boundary so everything below it stays pure over a string.
// [LAW:effects-at-boundaries]
//
// A version that cannot be read is reported and recorded as "" — the same value
// a dev build produces — rather than failing the completion around it: the push
// this record describes has already succeeded or failed, and bookkeeping must
// not re-color that verdict, exactly as recordPushOutcome's own write failure
// does not. [LAW:no-silent-failure] the failure is loud on the channel that
// cannot lie about the push.
func runningBinaryVersion() string {
	info, err := version.Get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: push-outcome provenance unavailable, build version unreadable: %v\n", err)
		return ""
	}
	return info.Version
}

// pushOutcomeProvenance renders the clause that keeps a recorded failure from
// reading as a live one. A record's Reason is a sentence frozen at write time —
// "this binary supports only up to 4" stays in the present tense however many
// binaries later it is printed — so when the binary has CHANGED since the
// attempt, the reader is told so and pointed at the one thing that settles it: a
// fresh attempt. Nothing here re-tests anything; re-testing is what `lit sync
// push` is, and every push already re-evaluates its preconditions from scratch.
//
// An empty clause is the answer whenever the comparison cannot be made — no
// observer stamped, no readable running version, or the same version on both
// sides — and the callers interpolate it unconditionally, so the sentence they
// build reads exactly as it did before when there is nothing to say.
// [LAW:dataflow-not-control-flow] the versions decide the text, never whether a
// renderer runs. Pure over two strings. [LAW:single-enforcer] both surfaces that
// replay a recorded failure — the banner and doctor — date it through this one
// function, so they cannot drift into dating it two ways.
func pushOutcomeProvenance(rec pushOutcomeRecord, running string) string {
	if rec.ObservedBy == "" || running == "" || rec.ObservedBy == running {
		return ""
	}
	return fmt.Sprintf(" (recorded by lit %s; you are now running %s, so this verdict predates your binary)", rec.ObservedBy, running)
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
