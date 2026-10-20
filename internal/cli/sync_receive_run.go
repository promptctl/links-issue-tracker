package cli

import (
	"context"
	"errors"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// receiveStep is where a receive stopped: the closed, ordered set of the
// steps it runs, in the order it runs them, and receiveStepNone when it ran to
// the end. [LAW:types-are-the-program] one value answers "how far did this
// receive get", and every reading of the outcome is a comparison against it,
// so no two readings can disagree about whether the fetch happened.
type receiveStep int

const (
	receiveStepNone receiveStep = iota
	receiveStepFetch
	receiveStepLand
	receiveStepOpen
	receiveStepLiveRemotes
	receiveStepSettle
	receiveStepReconcile
)

// String is the step's name on the event and the trace; empty for a receive
// that ran to the end.
func (s receiveStep) String() string {
	switch s {
	case receiveStepNone:
		return ""
	case receiveStepFetch:
		return "fetch"
	case receiveStepLand:
		return "land"
	case receiveStepOpen:
		return "open"
	case receiveStepLiveRemotes:
		return "live_remotes"
	case receiveStepSettle:
		return "settle"
	case receiveStepReconcile:
		return "reconcile"
	default:
		return "unknown"
	}
}

// describe is the step in a sentence a reader of the sync-failure block acts on.
func (s receiveStep) describe() string {
	switch s {
	case receiveStepFetch:
		return "fetching from the remote"
	case receiveStepLand:
		return "landing the fetch on the live store"
	case receiveStepOpen:
		return "opening the live store"
	case receiveStepLiveRemotes:
		return "reconciling the live store's remotes from git"
	case receiveStepSettle:
		return "settling onto the fetched head (following a re-root of the remote, when there is one)"
	case receiveStepReconcile:
		return "reconciling a divergence"
	default:
		return "an unknown step (" + s.String() + ")"
	}
}

// unsettles reports whether a stop at this step leaves the receive fetched but
// unsettled: after the fetch, before the settle finished. A stop at the fetch
// is the staleness banner's, and a stop at the reconcile is a divergence's.
func (s receiveStep) unsettles() bool {
	return s >= receiveStepLand && s <= receiveStepSettle
}

// unsettledLastingAfter is how long a run of post-fetch stops lasts, with no
// settle anywhere, before it is surfaced. Two debounce intervals: the stop
// that starts the run is zero old, so surfacing takes a later stop at least
// this much after it, and one cut of the receive's shared deadline — as often
// contention behind a slow fetch as a fault — never qualifies on its own.
// Surfacing also takes a stop between the first and this one (unsettled.last),
// so two cuts with an idle gap between them are not a run that retries failed
// to clear; under use the receives run every debounce interval, so that stop
// is there by the time the run is this old.
const unsettledLastingAfter = 2 * receiveDebounceInterval

// countsTowardUnsettled reports whether this outcome is a post-fetch stop that
// belongs to a run. A cancellation is the attempt being torn down, not
// evidence about the checkout, so it neither starts nor extends one.
func (o syncReceiveOutcome) countsTowardUnsettled() bool {
	return o.stopped.unsettles() && !errors.Is(o.err, context.Canceled)
}

// unsettledLasting reports whether this receive is surfaced as
// syncFailureReceiveUnsettled: it stopped after its fetch, a run of such stops
// is current, another stop came between the run's first and this one, and the
// run has lasted at least unsettledLastingAfter.
// [LAW:dataflow-not-control-flow] read off the outcome's values and the clock
// passed in; nothing here reads a record.
func (o syncReceiveOutcome) unsettledLasting(now time.Time) bool {
	return o.countsTowardUnsettled() && !o.unsettledSince.IsZero() &&
		o.unsettledPreviousStop.After(o.unsettledSince) && now.Sub(o.unsettledSince) >= unsettledLastingAfter
}

// unsettledObservation is what a receive learned about the run of post-fetch
// stops: when the current run began (zero when none is current), the latest
// stop before this receive's within that run (zero when none), when a settle
// last succeeded (zero when none ever has), and any failure reading or
// writing the records.
type unsettledObservation struct {
	since        time.Time
	previousStop time.Time
	settledLast  time.Time
	err          error
}

// observeUnsettledRun is how a receive takes part in the run of post-fetch
// stops. Every receive reads the run (readUnsettledRun), so its trace and
// event carry the times as they stand. Only the automatic receive, under
// the receive lock, writes it (ownUnsettledRun); the mirror's receive of its
// re-rooted push does not hold that lock, so it only reads
// (leaveUnsettledRun), and its stop is counted by the next automatic receive,
// which fetches past the remote head the push moved.
// [LAW:one-type-per-behavior] one call on every receive, the owner is data.
type observeUnsettledRun func(o syncReceiveOutcome, now time.Time) unsettledObservation

// readUnsettledRun is the run as the three records describe it now.
func readUnsettledRun(databasePath string) unsettledObservation {
	first, firstOK, firstErr := store.ReadUnsettledFirst(databasePath)
	last, lastOK, lastErr := store.ReadUnsettledLast(databasePath)
	settled, settledOK, settledErr := store.ReadSettledLast(databasePath)
	obs := unsettledObservation{
		since:       currentRunStart(first, firstOK, settled, settledOK),
		settledLast: settled,
		err:         errors.Join(firstErr, lastErr, settledErr),
	}
	if lastOK && !obs.since.IsZero() && !last.Before(obs.since) {
		obs.previousStop = last
	}
	return obs
}

// leaveUnsettledRun is the receive that does not own the run: it reads it and
// writes nothing.
func leaveUnsettledRun(databasePath string) observeUnsettledRun {
	return func(syncReceiveOutcome, time.Time) unsettledObservation {
		return readUnsettledRun(databasePath)
	}
}

// ownUnsettledRun is the automatic receive's part in the run, under the
// receive lock it holds: a stop that counts records itself in unsettled.last,
// and starts a run when none is current by writing unsettled.first. It never
// reads a record to write it back, and it never touches settled.last, which
// only the settle writes (store/receive_episode.go). [LAW:single-enforcer]
// one writer per record. After a run is started settled.last is read again,
// so a settle that finished between the first read and the write ends the
// run the write started.
func ownUnsettledRun(databasePath string) observeUnsettledRun {
	return func(o syncReceiveOutcome, now time.Time) unsettledObservation {
		obs := readUnsettledRun(databasePath)
		if !o.countsTowardUnsettled() {
			return obs
		}
		lastErr := store.WriteUnsettledLast(databasePath, now)
		if !obs.since.IsZero() {
			obs.err = errors.Join(obs.err, lastErr)
			return obs
		}
		firstErr := store.WriteUnsettledFirst(databasePath, now)
		settled, settledOK, rereadErr := store.ReadSettledLast(databasePath)
		return unsettledObservation{
			since:       currentRunStart(now, true, settled, settledOK),
			settledLast: settled,
			err:         errors.Join(obs.err, lastErr, firstErr, rereadErr),
		}
	}
}

// currentRunStart is the start of the run of post-fetch stops that is still
// current, or zero: a run is current until a settle succeeds after it began.
func currentRunStart(first time.Time, firstOK bool, settled time.Time, settledOK bool) time.Time {
	if !firstOK || (settledOK && !first.After(settled)) {
		return time.Time{}
	}
	return first
}

// unixOrZero is a time on the event: Unix seconds, or 0 for no time.
// [LAW:nothing-unseen] zero and absent are different facts, and the event
// always carries the key.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
