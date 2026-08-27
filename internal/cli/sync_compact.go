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
		recordCompactError(ws, fmt.Errorf("open store for compaction: %w", err))
		return
	}
	defer closeStore()

	// Whether a pass is owed is the engine's judgment, not this caller's: it is
	// a fact about how the engine stores data. [LAW:decomposition]
	outcome, err := session.syncer.CompactIfDue(timeoutCtx)
	if err != nil {
		recordCompactError(ws, err)
		return
	}
	if !outcome.Ran {
		// Nothing was owed. The common case, and not worth a trace.
		return
	}
	recordSyncCommandTrace(ws, compactTraceCommand, "ok", nil, compactionTraceMetadata(outcome))
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
// An engine that could not measure leaves Detail empty; that key is then
// dropped by compactTraceMetadata, which already owns what an empty value
// means, so this builder never re-decides it. [LAW:single-enforcer]
func compactionTraceMetadata(outcome storage.CompactionOutcome) map[string]string {
	return map[string]string{
		"depth":  outcome.Depth.String(),
		"detail": outcome.Detail,
	}
}

// compactTraceCommand names the backstop in the automation trace. It is not a
// real command line — no user typed it — and it is deliberately distinct from
// `lit sync compact` so an operator reading traces can tell the automatic pass
// from the one they ran themselves. [LAW:one-source-of-truth]
const compactTraceCommand = "compaction backstop"

// recordCompactError routes a backstop failure through the same trace seam as
// every other automatic sync failure, so maintenance health is discoverable
// exactly where sync health already is rather than in a channel of its own.
// [LAW:single-enforcer]
func recordCompactError(ws workspace.Info, cause error) {
	recordSyncCommandTrace(ws, compactTraceCommand, "error", cause, nil)
}
