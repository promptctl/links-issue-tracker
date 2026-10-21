package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The compaction backstop is what collects a store that nothing else collects,
// and what holds every store under its size ceiling. Compaction otherwise rides
// on the push path, so a workspace with no remote — or one that simply goes a
// long time between explicit pushes — would have nothing reclaiming its storage
// at all; and nothing else ever asks whether the store as a whole has grown
// past its bound.
//
// The depth of a compaction pass is irrelevant to its safety, but its TIMING
// is not. DOLT_GC transitions the store read-only mid-run and collides with a
// live engine, so the pass must not overlap one. maybeAutoSyncAfterCommand
// calls this only after the command's own engine has closed, and the detached
// mirror and receive it spawns open theirs only once this process has exited.
// [LAW:no-ambient-temporal-coupling]
//
// Note that the on-change mirror is NOT a candidate host for this work, despite
// already holding the commit lock: ensureMirrorCoverage short-circuits on a
// confirmed remote-less workspace before it claims, so no mirror ever spawns
// there — precisely the workspace this backstop exists for.

const (
	// compactProbeInterval bounds how often this asks the engine whether a pass
	// is owed. Asking costs an engine open, so it is debounced exactly the way
	// the automatic receive is, and for the same reason.
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

// maintainInline runs the engine's maintenance probe — a compaction pass when
// one is due, and the size ceiling's hold — INLINE in the command process after
// that command's engine has closed.
//
// It is best-effort and never affects the command's result: the command's work
// is already committed and its output already produced, so a maintenance
// failure must not retroactively fail it. It is still loud — every failure is
// recorded through the same sync-trace seam every other automatic sync failure
// uses, so a store that has stopped collecting is discoverable rather than
// silently growing. [LAW:no-silent-failure]
func maintainInline(ctx context.Context, env environ.Env, ws workspace.Info) {
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
	m, closeStore, err := openMaintainer(timeoutCtx, env, ws)
	if err != nil {
		recordBackstopFailure(env, ws, fmt.Errorf("open store for compaction: %w", err))
		return
	}
	defer closeStore()

	maintainThrough(timeoutCtx, env, ws, m)
}

// maintainer is all the backstop asks of the engine. The backstop runs after a
// command that is not a sync command, so it holds no sync session: nothing it
// holds can follow, fetch or settle, and the command's event carries none of
// a sync session's lists, whose [] says a sync looked and found none.
// [LAW:types-are-the-program] [LAW:nothing-unseen]
type maintainer interface {
	MaintainIfDue(ctx context.Context) (storage.MaintenanceOutcome, error)
}

// openMaintainer opens the workspace's sync engine for maintenance alone. The
// returned close is the caller's to defer; it releases the workspace lock.
func openMaintainer(ctx context.Context, env environ.Env, ws workspace.Info) (maintainer, func() error, error) {
	st, syncer, err := openSyncEngineAt(ctx, env, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	return syncer, st.Close, nil
}

// maintainThrough asks the engine for its maintenance probe and records what
// came back. It is split from opening the store because those are two jobs:
// the caller above owns the engine's lifetime, while everything here is the
// decision about what the durable trail should say. [LAW:decomposition]
//
// The split is also what makes those decisions reachable. maintainInline opens
// its own engine, so a test could only drive this through a real store whose
// footprint had crossed a threshold — and reaching one from the command layer
// would mean spelling the engine's on-disk layout here, which is exactly what
// the storage seam exists to prevent. Taking the maintainer as a parameter
// lets a fake engine produce each outcome directly, the way runSyncCompact is
// already tested.
func maintainThrough(ctx context.Context, env environ.Env, ws workspace.Info, m maintainer) {
	// What is owed is the engine's judgment, not this caller's: it is a fact
	// about how the engine stores data. [LAW:decomposition]
	outcome, err := m.MaintainIfDue(ctx)
	if err != nil {
		// The outcome goes with the error rather than being dropped for it: a
		// pass whose reconnect failed still rewrote the store, a prune that
		// failed partway still retired what it named, and the depth is known
		// here even when the pass never got that far. None of it survives
		// anywhere else once this returns. [LAW:no-silent-failure]
		recordSyncCommandTrace(env, ws, compactTraceCommand, "error", err, maintenanceTraceMetadata(outcome))
		return
	}
	decision, spoken := maintenanceDecision(outcome)
	if !spoken {
		// Nothing was owed and the store is within its ceiling. The common
		// case, and not worth a trace.
		return
	}
	recordSyncCommandTrace(env, ws, compactTraceCommand, decision, nil, maintenanceTraceMetadata(outcome))
}

// maintenanceDecision names what a probe did, or reports it did nothing worth
// a trace. A pass that ran is compactedDecision, the decision the explicit
// command records, so a trail filtered for compactions finds both paths. A store over
// its ceiling is worth a trace whatever else happened: it is how a store that
// has stopped shrinking stays discoverable. [LAW:no-silent-failure]
func maintenanceDecision(outcome storage.MaintenanceOutcome) (string, bool) {
	_, over := outcome.Footprint.Over()
	switch {
	case outcome.Compaction.Ran:
		return compactedDecision, true
	case len(outcome.SnapshotsPruned) > 0:
		return "pruned snapshots", true
	case over:
		return "over ceiling", true
	}
	return "", false
}

// maintenanceTraceMetadata renders a probe into the trace's vocabulary: the
// compaction's account, then the ceiling's. Empty values are dropped by
// compactTraceMetadata, which owns what an empty value means.
// [LAW:single-enforcer]
func maintenanceTraceMetadata(outcome storage.MaintenanceOutcome) map[string]string {
	metadata := map[string]string{"snapshots_pruned": strings.Join(outcome.SnapshotsPruned, ",")}
	maps.Copy(metadata, compactionAttemptMetadata(outcome.Compaction))
	if !outcome.Footprint.Measured() {
		return metadata
	}
	metadata["governed_bytes"] = strconv.FormatInt(outcome.Footprint.GovernedBytes(), 10)
	metadata["ceiling_bytes"] = strconv.FormatInt(outcome.Footprint.Ceiling, 10)
	if over, ok := outcome.Footprint.Over(); ok {
		metadata["over_ceiling"] = fmt.Sprintf("%s %d", over.Largest.Name, over.Largest.Bytes)
	}
	return metadata
}

// compactedDecision is the decision a completed pass is recorded under, by
// both entry points — the backstop and the explicit `lit sync compact` — so an
// operator filtering the trail for successful compactions sees both. Command
// already says which path ran (compactTraceCommand is deliberately distinct
// from the command line for exactly that purpose), so a second axis saying it
// again could only disagree. [LAW:one-source-of-truth]
const compactedDecision = "compacted"

// recordCompactionSuccess is the one way the explicit command's completed pass
// reaches the durable trail. It renders through compactionAttemptMetadata, as
// the backstop's maintenanceTraceMetadata does, so the trail carries one shape
// for a pass whichever entry point ran it.
func recordCompactionSuccess(env environ.Env, ws workspace.Info, command string, outcome storage.CompactionOutcome) {
	recordSyncCommandTrace(env, ws, command, compactedDecision, nil, compactionAttemptMetadata(outcome))
}

// compactionDepthMetadata names the depth alone, for the outcome that has
// nothing else to report: a pass that failed before reclaiming anything still
// knows which depth was asked for, and an operator scheduling the deep form
// needs the trail to say which one kept failing.
//
// It is a separate renderer rather than a literal at the failure site because
// the key would then be spelled in three places. [LAW:one-source-of-truth]
func compactionDepthMetadata(depth storage.GCMode) map[string]string {
	return map[string]string{"depth": depth.String()}
}

// recordCompactFailure is the one way the explicit command's compaction that
// errored reaches the durable trail, mirroring recordCompactionSuccess above.
// [LAW:one-source-of-truth]
//
// It takes the outcome rather than the error alone because a failure still knows
// things. The engine reports the depth whenever one was chosen, and reports Ran
// when the pass itself completed and only the work after it failed — so a
// reconnect that failed behind a finished deep collection is recorded as the
// rewrite it was, rather than as an error that reads like nothing happened.
// [LAW:no-silent-failure]
func recordCompactFailure(env environ.Env, ws workspace.Info, command string, outcome storage.CompactionOutcome, cause error) {
	recordSyncCommandTrace(env, ws, command, "error", cause, compactionAttemptMetadata(outcome))
}

// compactionAttemptMetadata renders what is known about a pass that was
// attempted, whether or not the call around it then failed. Every compaction
// record renders through it — the backstop's and the explicit command's, on
// success and failure — so a reader asking "what did the last compaction
// reclaim" reads the same keys whichever entry point ran. The depth is spelled
// "depth" because that is the contract's own name for it
// (CompactionOutcome.Depth). Detail rides along because a scheduled pass may
// have nobody reading its stdout, which leaves the trace as the only surviving
// account of what it reclaimed. [LAW:one-source-of-truth] [LAW:no-silent-failure]
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
func compactionAttemptMetadata(outcome storage.CompactionOutcome) map[string]string {
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
func recordBackstopFailure(env environ.Env, ws workspace.Info, cause error) {
	recordCompactFailure(env, ws, compactTraceCommand, storage.CompactionOutcome{}, cause)
}
