// Package deadline is the deadlines one run's background sync work runs under
// (Deadlines) and the measured figures lit ships them at (Shipped). It sits
// below every package that reads one — the store's locks, the cli's workers,
// and the environ.Env that carries a run's deadlines to both — so a run's
// deadlines are one value handed down, never a figure each layer keeps.
// [LAW:one-way-deps] [LAW:one-source-of-truth]
package deadline

import (
	"encoding/json"
	"fmt"
	"time"
)

// The co-resident-holder sizing chain. Measured facts at the root — what each
// routine holder of the live store costs to wait out — and every wait on the
// store derived from them by arithmetic (Deadlines.HolderWait).
// [LAW:one-source-of-truth] Hand-set waits would each have to be remembered
// into agreement, and one set below the cost of the work it bounds is nothing
// the code could notice. Change a measurement; the waits follow.
//
// The mirror holds the live store only to clone it — workspace shared,
// Dolt's LOCK, commit lock, one copy-on-write clone of the dolt directory,
// release — and pushes from the clone with no lock on the live store held.
// The push's own deadline is the separate chain below (mirrorPushDeadline).
const (
	// mirrorCloneObservedTail is the slowest a HEALTHY hold — take the three
	// locks, clone the dolt directory, clear the pending marker, release —
	// has been measured to run. Measured 2026-09-27 against this repository's
	// own store (283 MB on disk: 94 MB noms, 183 MB git-remote-cache), load
	// average 7–13, as the clone step alone, which is the whole hold but for
	// three flock calls and one unlink:
	//
	//   - APFS clonefile of the whole directory (dbsnapshot.CloneTree's
	//     Darwin path), 6 samples: 0.109s–0.240s.
	//   - byte-for-byte copy of the same directory (the walk-and-copy path a
	//     filesystem without copy-on-write falls back to), 2 samples:
	//     0.390s, 0.461s.
	//
	// The hold has two healthy regimes — a copy-on-write clone whose cost is
	// metadata, and a byte copy whose cost is proportional to the store — and
	// a stall detector sits above the SLOWER of them, so the figure is the
	// byte copy's tail rounded up to the next 100ms, not the clone's.
	// [FRAMING:representation] a map of an operation whose territory grows
	// with the store; mirror.log's `hold=` values are the place to re-measure
	// from, and a byte-copy regime on a slow disk is the reading that would
	// move it.
	mirrorCloneObservedTail = 500 * time.Millisecond

	// mirrorHoldStallFactor is what separates "slow" from "stalled". A hold
	// still running at twice the slowest healthy hold ever recorded is not a
	// hold running a bit long; it is a copy that has stopped making progress,
	// and the budget fires there. A budget set AT the cost of the work is not
	// a safety valve; it is a scheduled failure.
	mirrorHoldStallFactor = 2

	// mirrorHoldBudget is the shipped deadline each mirror hold on the live
	// store runs under — the clone step, and the pushed-head record after the
	// push (Deadlines.MirrorHold).
	mirrorHoldBudget = mirrorCloneObservedTail * mirrorHoldStallFactor

	// mirrorHoldCancelLag is how much longer a cut hold can keep the store
	// after its deadline has fired. The clone step's one uncancelable unit is
	// a single clonefile syscall (the walk-and-copy path checks the context
	// between 32 MiB chunks), and a syscall that ran long is bounded by the
	// clone tail above; the record step's is one journal append. So a cut
	// lands within one observed tail of its deadline — a bound derived from
	// the measurement, not a second measurement, which is why it is not
	// exported as an "observed" figure.
	mirrorHoldCancelLag = mirrorCloneObservedTail

	// LockPollInterval is how often a contender re-tries a held store lock,
	// and so the granularity of every wait on the store: a holder that
	// releases is noticed within this much. coResidentWaitHeadroom is a
	// multiple of it for that reason — the headroom has to cover sleeping
	// through the release, not a step of the hold. [LAW:one-source-of-truth]
	// declared once, read by the store's holdWait and by the sizing chain
	// here.
	LockPollInterval = 100 * time.Millisecond

	// coResidentWaitHeadroom is scheduling slop above the ceiling, and the one
	// number here that is a judgment rather than a measurement — so it is
	// pinned to something real: eight polls, which is how much of the wait
	// can be spent asleep between polls on a machine under load. It is
	// deliberately not folded into the steps above; slop hidden inside a
	// measured step is how a measured step stops being measurable.
	coResidentWaitHeadroom = 8 * LockPollInterval
)

// HolderWait is the ONE answer to "how long does a caller wait for
// a co-resident holder of a store to let go" — a live write Store in this
// or another process, a non-lit dolt process, or the snapshot copy's
// LockDoltJournalExclusive hold. It is sized to the longest ROUTINE hold, the
// mirror's clone or record step, because a wait shorter than a legal routine
// hold does not protect anyone, it manufactures "another process is holding
// this workspace's Dolt store open" out of a workspace behaving exactly as
// designed — and a wait longer than that answers a stalled holder with
// silence, which is what sends a user or agent into a retry that adds a
// second waiter. Every other holder is not routine and is named: a snapshot
// copy, a migration, an import, the explicit push (which still holds the
// store across the network), all of them fail a contender after this wait
// with the holder's pid, command and age in the refusal. The automatic
// receive fetches on a clone, so its holds on this store — the copy, the
// landing, the settle — are routine ones, except a landing of a fetch that
// brought much more than the usual few commits, which is named like the rest.
//
// The wait counts from the last time the holders in front of the contender
// showed progress, not from the contender's arrival (holdWait): sixteen `lit
// new` at once serialize on this store at tens of milliseconds each and every
// one lands, because each one's arrival is progress; a holder that stands
// still for this long is the one that fails the contender. A routine write
// hold (engineWriteRoutine; engineAccess says which holds are) also shows
// progress while its engine works, by heartbeat (lockHold), so it is waited out
// however long a loaded machine stretches it; this figure is then the longest
// it may go without engine work. Every hold that sends no heartbeat (the long holds above, a foreign
// dolt, a lit older than heartbeats) is judged by this figure exactly as
// before.
//
// It is the run's hold ceiling plus headroom: derived from the hold the run's
// holders run under, so a run that lifts its hold lifts its waits with it, and
// a contention test shrinks the wait by handing a short hold.
// [LAW:one-source-of-truth]
func (d Deadlines) HolderWait() time.Duration {
	return d.HoldCeiling() + coResidentWaitHeadroom
}

// HoldCeiling is the longest a mirror can hold the live store's LOCK and
// commit lock: its deadline plus the lag its cut takes to land. This — not the
// hold's deadline — is the number every co-resident waiter must outlast.
func (d Deadlines) HoldCeiling() time.Duration {
	return d.MirrorHold() + mirrorHoldCancelLag
}

// The push deadline chain. The mirror's push runs from a clone with no lock on
// the live store held, so nothing waits on it — but the mirror process itself
// holds the single-flight sync-push lock for its whole run, and every mirror
// spawned meanwhile loses that race and exits. A push hung on a stalled
// transport would therefore stop pushes for as long as the remote cared to
// stall, so the push runs under a deadline.
const (
	// mirrorPushObservedTail is the slowest a HEALTHY push — open the clone's
	// engine, push, close — has been measured to run on a real workspace.
	// Measured 2026-09-12 against this repo's own store, from two samples,
	// because the one the mirror keeps for itself is censored by the very
	// deadline sized from it:
	//
	//   - .git/links/mirror.log, 279 cycles: min 10.6s; p50 13.2s over all of
	//     them, 12.7s over the 235 that were not cut; slowest uncut cycle
	//     20.0s. 44 of the 279 (15.8%) were cut, so every cycle that would
	//     have run longer is recorded as a cut and the log cannot show the
	//     tail. A floor, never a ceiling.
	//   - 20 foreground `lit sync push` runs of the same store: min 9.8s,
	//     p50 12.2s, max 17.0s. Uncensored — the foreground push shares
	//     performSyncPush with the mirror and is deliberately unbounded — so
	//     this is the sample the deadline can honestly be sized against.
	//
	// Compared like with like — completed work against completed work, the
	// mirror's uncut p50 of 12.7s against the foreground's 12.2s — the two
	// agree to about half a second: the background path does not cost twice
	// the foreground one. Both paths ARE the push. The engine open and close
	// bracketing it measure ~0.3s together, so the cost is the network round
	// trip and nothing else. [FRAMING:representation] a map of an operation
	// whose territory — a push against a remote holding a repository that
	// grows — moves; mirror.log's `push=` values are the place to re-measure
	// from.
	mirrorPushObservedTail = 20 * time.Second

	// mirrorPushStallFactor: a transport that has stopped answering is not a
	// push running a bit long, it is a push that will never end, so the
	// deadline fires at twice the slowest healthy push ever recorded, where
	// the only thing on the far side is a stall.
	mirrorPushStallFactor = 2

	// mirrorPushDeadline is the shipped deadline the mirror's push runs under
	// (Deadlines.MirrorPush).
	mirrorPushDeadline = mirrorPushObservedTail * mirrorPushStallFactor

	// MirrorPushCancelLagObserved is how much longer a cut push keeps its
	// engine (the clone's) after its deadline has already fired:
	// cancellation reaches the transport, but the push does not unwind
	// instantly. Measured 2026-09-12 over the 44 cut cycles in mirror.log as
	// elapsed-minus-budget: p50 1.3s, but 21.4s at the tail.
	//
	// Exported because the cli's push-deadline regression tests assert where
	// a cut push ENDS, and restating that bound there would be a second,
	// unattributed copy of this figure that a re-measurement here leaves
	// behind. [LAW:one-source-of-truth] No store wait is sized against it: the
	// push holds nothing on the live store, so its lag is the mirror process's
	// own lifetime and nobody else's.
	MirrorPushCancelLagObserved = 22 * time.Second
)

// receiveDeadline is the shipped deadline the automatic receive runs under
// (Figures.Receive says what it bounds).
const receiveDeadline = 15 * time.Second

// pushProofDeadline is the shipped deadline a push's proof runs under: one
// `git ls-remote` (1.2-1.7s measured over ssh, 2026-09-27) and one local `git
// cat-file`. The push has already landed, so a proof cut short costs only the
// next receive one fetch.
const pushProofDeadline = 10 * time.Second

// compactionDeadline is the shipped deadline one compaction pass runs under,
// so a wedged collection cannot hang the command that triggered it.
//
// A deep pass over this repository's 72 MB store measured 1.36s, so this
// covers a store some thirty times larger before it binds at all — it is a
// bound on pathology, not on size. It is deliberately not larger than that:
// the pass runs after the on-change mirror has been spawned, so it is a term in
// the spawner's post-spawn tail, and every second here is a second the mirror
// must be willing to wait for a healthy parent. [LAW:carrying-cost]
//
// It also bounds how long a writer waits behind a pass. This deadline is what
// lets the backstop's hold be routine (store.OpenMaintenance): a pass ends on
// its own, so the writers behind it wait rather than fail. It cuts DOLT_GC's
// marking; a collection past marking finishes its local disk work first.
const compactionDeadline = 45 * time.Second

// Figures are a run's deadlines as numbers, the one list of them: what Of
// builds a Deadlines from, and what a Deadlines is written as when a command
// hands its run's deadlines to the worker it spawns. A Figures may hold any
// number; only Of reads one, and it admits none that is not positive.
// [LAW:parse-dont-validate]
type Figures struct {
	// MirrorHold is the deadline each background-mirror hold on the live store
	// runs under: the clone step before the push, and the pushed-head record
	// after it. It is a deadline, not a bound on the hold: a cut lands up to
	// mirrorHoldCancelLag later, and HoldCeiling is the number that follows
	// from that. Every co-resident waiter's patience (HolderWait) derives from
	// it.
	MirrorHold time.Duration `json:"mirror_hold"`
	// MirrorPush is the deadline the background mirror's push runs under: the
	// clone engine's open and the push from it. The close after them takes no
	// ctx and is not bounded by it. The push traverses the network with no
	// inherent bound, so the bound is imposed by the actor that owns it. It
	// holds nothing on the live store; it exists so a stalled transport cannot
	// wedge the single-flight mirror forever. HistoryBound derives from it.
	MirrorPush time.Duration `json:"mirror_push"`
	// Receive bounds the automatic receive — the remote check and the fetch
	// its detached worker runs when the receive debounce lapses — so an
	// offline or slow remote cannot keep the worker running indefinitely. A
	// cut abandons only the fetch (the next interval retries). The fetch runs
	// on a clone of the store, so this deadline bounds the worker, not a hold:
	// the live store is held for the clone's copy (under MirrorHold) and for
	// the landing of the fetch, whose cost is proportional to how far the
	// remote moved and which only this deadline bounds.
	Receive time.Duration `json:"receive"`
	// PushProof bounds a landed push's proof against the remote: one `git
	// ls-remote`, which crosses the network, and one local `git cat-file`.
	PushProof time.Duration `json:"push_proof"`
	// Compaction bounds one compaction pass, the backstop a writing command
	// runs after it has spawned its mirror: a term in the spawner's tail, which
	// the worker's wait for its spawner derives from.
	Compaction time.Duration `json:"compaction"`
}

// Deadlines is the set of deadlines one run's background sync work runs
// under, every one positive: only Of builds one, so no partly-set or zero
// figure reaches a hold, a push or a wait. A run is handed it whole at its
// composition root (cmd/lit hands Shipped); it rides the run's environ.Env to
// every store open and every worker the run spawns, and a worker runs under
// its spawner's. A test shrinks or lifts a deadline by handing its own value,
// never by reaching into a process-wide one. [LAW:types-are-the-program]
// [LAW:no-shared-mutable-globals]
//
// The zero Deadlines is no run's: reading a figure from it panics, naming Of,
// so code handed none fails where it reads one rather than cutting every hold
// at once. IsZero lets a composition boundary refuse it first.
type Deadlines struct{ f Figures }

// Of is the deadlines f gives, or why f gives none: a figure that is not
// positive, named. [LAW:parse-dont-validate]
func Of(f Figures) (Deadlines, error) {
	for _, figure := range []struct {
		name  string
		value time.Duration
	}{
		{"mirror hold", f.MirrorHold},
		{"mirror push", f.MirrorPush},
		{"receive", f.Receive},
		{"push proof", f.PushProof},
		{"compaction", f.Compaction},
	} {
		if figure.value <= 0 {
			return Deadlines{}, fmt.Errorf("the %s deadline must be positive, got %s", figure.name, figure.value)
		}
	}
	return Deadlines{f: f}, nil
}

// Shipped is the deadlines lit ships with, each derived above from what it
// bounds. [LAW:one-source-of-truth]
func Shipped() Deadlines {
	return Deadlines{f: Figures{MirrorHold: mirrorHoldBudget, MirrorPush: mirrorPushDeadline, Receive: receiveDeadline, PushProof: pushProofDeadline, Compaction: compactionDeadline}}
}

// IsZero reports whether d is the zero Deadlines, which Of never builds.
func (d Deadlines) IsZero() bool { return d == Deadlines{} }

// Figures is d as numbers.
func (d Deadlines) Figures() Figures { return d.figures() }

// figures is d's figures, read through the one check that d is a run's.
// [LAW:single-enforcer]
func (d Deadlines) figures() Figures {
	if d.IsZero() {
		panic("deadline: read a figure of the zero Deadlines; a run's deadlines are built by deadline.Of")
	}
	return d.f
}

// MirrorHold is Figures.MirrorHold.
func (d Deadlines) MirrorHold() time.Duration { return d.figures().MirrorHold }

// MirrorPush is Figures.MirrorPush.
func (d Deadlines) MirrorPush() time.Duration { return d.figures().MirrorPush }

// Receive is Figures.Receive.
func (d Deadlines) Receive() time.Duration { return d.figures().Receive }

// PushProof is Figures.PushProof.
func (d Deadlines) PushProof() time.Duration { return d.figures().PushProof }

// Compaction is Figures.Compaction.
func (d Deadlines) Compaction() time.Duration { return d.figures().Compaction }

// HistoryBound is the deadline a landed push's history bound runs under. The
// bound crosses the network three times — the re-root's lease push, the
// remote's GC, the fetch that learns what the remote carries — and each leg
// is a transport of a push's kind, so each gets the push deadline. The local
// legs, the re-roots, ride in the stall factor's margin; their cost is
// reroot.elapsed_ms. The bound's follow collects nothing below the cut: the
// live store's push verb runs that full GC after the bound, on its own
// lifetime (collectOwedCut). The remote GC's upload has not been measured
// against a hosted remote; history_bound.elapsed_ms on the push's event is the
// figure to re-measure from. A cut bound fails nothing: the push landed. A cut
// before the re-root reaches the remote leaves the next push past the bound to
// run it again; a cut after it leaves the remote's history below the cut to
// the next re-root's GC, which collects it with its own, since the remote's GC
// replaces nothing once a push lands on top of the re-root. Read where the
// bound runs, never passed beside the push figure. [LAW:one-source-of-truth]
func (d Deadlines) HistoryBound() time.Duration { return 3 * d.MirrorPush() }

// MarshalJSON writes d as its Figures.
func (d Deadlines) MarshalJSON() ([]byte, error) { return json.Marshal(d.figures()) }

// UnmarshalJSON reads d from its Figures through Of, so a handoff carrying a
// figure that is missing or not positive is refused as it is read.
// [LAW:parse-dont-validate]
func (d *Deadlines) UnmarshalJSON(data []byte) error {
	var f Figures
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	built, err := Of(f)
	if err != nil {
		return err
	}
	*d = built
	return nil
}
