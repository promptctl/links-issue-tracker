package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The compaction backstop is what collects a store that nothing else collects.
//
// Compaction previously rode entirely on the push path, so a workspace with no
// remote — or one that simply goes a long time between explicit pushes — had
// nothing reclaiming its storage at all. Measured on a fresh remote-less
// workspace, 150 mutations with no push grew the chunk journal to 6.5 MB with
// the per-mutation cost still climbing, and a single shallow pass reclaimed 69%
// of it. That is the gap this closes.
//
// It runs where the inline receive runs and for the same reason: the depth of a
// compaction pass is irrelevant to its safety, but its TIMING is not. DOLT_GC
// transitions the store read-only mid-run and collides with a live engine, so
// the pass must not overlap one. maybeAutoSyncAfterCommand calls this only
// after the command's own engine has closed, which is the same window the
// inline receive already opens its engine in. [LAW:no-ambient-temporal-coupling]
//
// Note that the on-change mirror is NOT a candidate host for this work, despite
// already holding the commit lock: ensureMirrorCoverage short-circuits on a
// confirmed remote-less workspace before it claims, so no mirror ever spawns
// there — precisely the workspace this backstop exists for.

const (
	// compactProbeInterval bounds how often this asks the engine whether a pass
	// is owed. Asking costs an engine open, so it is debounced exactly the way
	// the inline receive debounces its fetch, and for the same reason.
	//
	// This is when to LOOK, never whether to collect — the engine's own
	// footprint decides that, so a quiet workspace pays one cheap question and
	// no pass. Dolt's own auto-GC splits the two the same way, polling on a
	// timer and collecting on a threshold. [LAW:no-ambient-temporal-coupling]
	//
	// It doubles as the retry floor for a pass that failed: a successful pass
	// needs none (it drops the footprint below the threshold, so the store
	// itself stops asking), while a failing one would otherwise re-attempt on
	// every subsequent mutation and turn one broken store into a stall on every
	// command. [LAW:carrying-cost]
	compactProbeInterval = 15 * time.Minute

	// compactTimeout bounds a single pass so a wedged collection cannot hang
	// the command that triggered it.
	//
	// A deep pass over this repository's 72 MB store measured 1.36s, so this
	// covers a store some thirty times larger before it binds at all — it is a
	// bound on pathology, not on size. It is deliberately not larger than that:
	// this pass runs after the on-change mirror has been spawned, so it is a
	// term in parentPostSpawnTail and every second here is a second the mirror
	// must be willing to wait for a healthy parent. [LAW:carrying-cost]
	compactTimeout = 45 * time.Second
)

// compactMarkerPath is the single marker for "a compaction was last attempted
// here": its modification time is when that attempt ran.
// [LAW:one-source-of-truth]
func compactMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "compact.last")
}

// compactInline runs a compaction pass when the store's on-disk footprint says
// one is due, INLINE in the command process after that command's engine has
// closed.
//
// It is best-effort and never affects the command's result: the command's work
// is already committed and its output already produced, so a maintenance
// failure must not retroactively fail it. It is still loud — every failure is
// recorded through the same sync-trace seam every other automatic sync failure
// uses, so a store that has stopped collecting is discoverable rather than
// silently growing. [LAW:no-silent-failure]
func compactInline(ctx context.Context, ws workspace.Info) {
	if !shouldRunNow(compactMarkerPath(ws), time.Now(), compactProbeInterval) {
		return
	}
	// Marked before the attempt, so a store that fails every pass is asked at
	// the interval rather than on every command. [LAW:single-enforcer] one
	// writer for this marker: this owner.
	if err := markRunAttempt(ws, compactMarkerPath(ws)); err != nil {
		// An unwritable marker means the interval is not in force. Say so and
		// proceed: skipping maintenance because a debounce file cannot be
		// written would trade a bounded annoyance for an unbounded store.
		// [LAW:no-silent-failure]
		fmt.Fprintf(os.Stderr, "lit: compaction probe interval not recorded: %v\n", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, compactTimeout)
	defer cancel()
	session, closeStore, err := openSyncSession(timeoutCtx, ws)
	if err != nil {
		recordBackstopFailure(ws, fmt.Errorf("open store for compaction: %w", err))
		return
	}
	defer closeStore()

	compactThroughSession(timeoutCtx, ws, session)
}

// compactThroughSession asks the engine whether a pass is owed and records what
// came back. It is split from opening the store because those are two jobs: the
// caller above owns a session's lifetime, while everything here is the decision
// about what the durable trail should say. [LAW:decomposition]
//
// The split is also what makes those decisions reachable. compactInline opens
// its own session, so a test could only drive this through a real store whose
// footprint had crossed a threshold — and reaching one from the command layer
// would mean spelling the engine's on-disk layout here, which is exactly what
// the storage seam exists to prevent. Taking the session as a parameter lets a
// fake engine produce each outcome directly, the way runSyncCompact is already
// tested.
func compactThroughSession(ctx context.Context, ws workspace.Info, session syncSession) {
	// Whether a pass is owed is the engine's judgment, not this caller's: it is
	// a fact about how the engine stores data. [LAW:decomposition]
	outcome, err := session.syncer.CompactIfDue(ctx)
	if err != nil {
		// The outcome goes with the error rather than being dropped for it: a
		// pass whose reconnect failed still rewrote the store, and the depth is
		// known here even when the pass never got that far. Neither survives
		// anywhere else once this returns. [LAW:no-silent-failure]
		recordCompactFailure(ws, compactTraceCommand, outcome, err)
		return
	}
	if !outcome.Ran {
		// Nothing was owed. The common case, and not worth a trace.
		return
	}
	recordCompactionSuccess(ws, compactTraceCommand, outcome)
}

// recordCompactionSuccess is the one way a completed pass reaches the durable
// trail. Both entry points record through it — the backstop above and the
// explicit `lit sync compact` — so the decision and the metadata each have a
// single home, and the only thing a caller supplies is the one thing that
// genuinely differs between them: which command ran. [LAW:composability] the
// variability crosses one boundary as a value.
//
// It exists because sharing the metadata renderer alone was not enough. The two
// paths still passed their own decision string, so the same event was recorded
// as "ok" by the backstop and "compacted" by the command, and an operator
// filtering the trail for successful compactions saw only half of them. Command
// already says which path ran — compactTraceCommand is deliberately distinct
// from the command line for exactly that purpose — so a second axis saying it
// again could only disagree. [LAW:one-source-of-truth]
//
// The decision is spelled here and nowhere else, which is what stops a third
// entry point from inventing a fourth vocabulary: there is nothing left for it
// to spell.
func recordCompactionSuccess(ws workspace.Info, command string, outcome storage.CompactionOutcome) {
	recordSyncCommandTrace(ws, command, "compacted", nil, compactionTraceMetadata(outcome))
}

// compactionTraceMetadata renders a finished pass into the trace's vocabulary.
// Both compaction paths record through this one function — the backstop above
// and the explicit `lit sync compact` — so the durable trail carries one shape
// whichever entry point ran.
//
// It exists because the two paths spelled the keys themselves and drifted: one
// wrote "mode" while the other wrote "depth", and only one carried the detail,
// so a reader asking "what did the last compaction reclaim" had to know which
// entry point ran before it could know which key to read. A single renderer
// cannot disagree with itself. [LAW:one-source-of-truth]
//
// The depth is spelled "depth" because that is the contract's own name for it
// (CompactionOutcome.Depth); "mode" was this file's older word for the same
// fact. Detail rides along on both paths because a scheduled pass may have
// nobody reading its stdout, which leaves the trace as the only surviving
// account of what it reclaimed — the same reason syncPushTraceMetadata carries
// the push's maintenance line. [LAW:no-silent-failure]
//
// An empty Detail is dropped by compactTraceMetadata, which already owns what an
// empty metadata value means, so this builder never re-decides it.
// [LAW:single-enforcer] What makes Detail empty is the contract's to say, and it
// says so on CompactionOutcome.Detail; restating it here is what put an earlier
// version of this comment at odds with it. [LAW:one-source-of-truth]
func compactionTraceMetadata(outcome storage.CompactionOutcome) map[string]string {
	metadata := compactionDepthMetadata(outcome.Depth)
	metadata["detail"] = outcome.Detail
	return metadata
}

// compactionDepthMetadata names the depth alone, for the outcome that has
// nothing else to report: a pass that failed before reclaiming anything still
// knows which depth was asked for, and an operator scheduling the deep form
// needs the trail to say which one kept failing.
//
// It is a separate renderer rather than a literal at the failure site because
// the key would then be spelled in three places, and this vocabulary has
// already drifted twice when it was spelled in two. [LAW:one-source-of-truth]
func compactionDepthMetadata(depth storage.GCMode) map[string]string {
	return map[string]string{"depth": depth.String()}
}

// recordCompactFailure is the one way a compaction that errored reaches the
// durable trail, mirroring recordCompactionSuccess above: both entry points
// record through it, both hand it an outcome, and neither spells a decision or
// a metadata key of its own. [LAW:one-source-of-truth]
//
// It takes the outcome rather than the error alone because a failure still knows
// things. The engine reports the depth whenever one was chosen, and reports Ran
// when the pass itself completed and only the work after it failed — so a
// reconnect that failed behind a finished deep collection is recorded as the
// rewrite it was, rather than as an error that reads like nothing happened.
// [LAW:no-silent-failure]
func recordCompactFailure(ws workspace.Info, command string, outcome storage.CompactionOutcome, cause error) {
	recordSyncCommandTrace(ws, command, "error", cause, compactionFailureMetadata(outcome))
}

// compactionFailureMetadata renders what is known about a pass that errored.
//
// A depth is recorded whenever one was chosen, and Valid is that test: the
// contract numbers its depths from one precisely so an outcome that chose none
// carries the zero rather than defaulting to the shallow depth. That case is
// real — a due-check whose own measurement fails never reaches a depth — and
// recording the default there would be worse than recording nothing, because a
// fabricated fact in a diagnostic trail outlives the incident it misdescribes.
// [LAW:no-silent-failure]
//
// The detail rides along only when the pass ran, since only then is there a
// reclaim to describe; the contract reserves an empty Detail for the outcome
// that did nothing, and this asks for it on exactly that outcome.
func compactionFailureMetadata(outcome storage.CompactionOutcome) map[string]string {
	if !outcome.Depth.Valid() {
		return nil
	}
	metadata := compactionDepthMetadata(outcome.Depth)
	if outcome.Ran {
		metadata["detail"] = outcome.Detail
	}
	return metadata
}

// compactTraceCommand names the backstop in the automation trace. It is not a
// real command line — no user typed it — and it is deliberately distinct from
// `lit sync compact` so an operator reading traces can tell the automatic pass
// from the one they ran themselves. [LAW:one-source-of-truth]
const compactTraceCommand = "compaction backstop"

// syncCompactTraceCommand names the explicit command in the automation trace.
// Its two records — the success and the failure — must agree on what they call
// it, so the name is written once. [LAW:one-source-of-truth]
const syncCompactTraceCommand = "lit sync compact"

// recordBackstopFailure routes a backstop failure through the same trace seam as
// every other automatic sync failure, so maintenance health is discoverable
// exactly where sync health already is rather than in a channel of its own.
// [LAW:single-enforcer]
//
// The zero outcome is the honest argument for a failure with no pass behind it —
// a store that could not even be opened attempted nothing, and the renderer
// records no depth for it rather than inventing one.
func recordBackstopFailure(ws workspace.Info, cause error) {
	recordCompactFailure(ws, compactTraceCommand, storage.CompactionOutcome{}, cause)
}
