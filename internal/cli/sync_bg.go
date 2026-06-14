package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// backgroundMirrorSubcommand is the hidden `lit sync` subcommand the on-change
// cadence owner spawns. It is absent from the family usage string, so it never
// appears in help; it exists only as the detached worker's entrypoint.
const backgroundMirrorSubcommand = "__mirror-bg"

// mirrorLogName is the detached worker's durable output sink. A detached
// process owns no terminal, so its stdout/stderr must land somewhere inspectable
// rather than /dev/null — otherwise a trace-write failure or a panic vanishes.
const mirrorLogName = "mirror.log"

const (
	// mirrorParentWaitTimeout bounds the wait for the spawning command to
	// release its engine. The wait ends the instant the parent exits; the cap
	// only guards a parent that never exits (e.g. a long-lived REPL), in which
	// case the mirror gives up rather than hang forever.
	mirrorParentWaitTimeout = 30 * time.Second
	mirrorParentPollDelay   = 20 * time.Millisecond
)

// spawnBackgroundMirror starts the detached mirror and returns immediately,
// without waiting for it. [LAW:effects-at-boundaries] The mutating command's
// change is already durable in the local Dolt store; getting it to the remote
// is an effect pushed entirely off the command's own latency path into a
// separate process. The automation-trace env is set here so a push that runs
// and fails records a trace through the one shared writer the pre-push hook
// already uses. [LAW:one-source-of-truth]
func spawnBackgroundMirror(ws workspace.Info, parentPID int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lit binary: %w", err)
	}
	cmd := exec.Command(self, "sync", backgroundMirrorSubcommand, "--parent-pid", strconv.Itoa(parentPID))
	cmd.Dir = ws.RootDir
	cmd.Stdin = nil
	// Route the detached worker's output to a durable log. [LAW:no-silent-failure]
	// If the log cannot be opened, surface that on the command's terminal-attached
	// stderr and still spawn with discarded streams — the mirror matters more than
	// its log, and the inability to log is itself loud here rather than swallowed.
	logFile, logErr := os.OpenFile(filepath.Join(ws.StorageDir, mirrorLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change mirror log unavailable (%v); worker output will be discarded\n", logErr)
	} else {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}
	cmd.SysProcAttr = detachSysProcAttr()
	cmd.Env = mirrorEnv()
	startErr := cmd.Start()
	if logFile != nil {
		// The child inherited its own dup of the fd at exec; the parent's copy is
		// no longer needed. Closing it cannot affect the child's logging.
		_ = logFile.Close()
	}
	return startErr
}

// mirrorEnv builds the detached mirror's environment: the parent's environment
// with every automation-trace variable stripped, then the mirror's own trigger
// and reason set. [LAW:one-source-of-truth] The parent's
// LNKS_AUTOMATION_TRACE_REF_FILE points at a file the parent's caller reads to
// learn which trace the command recorded; the detached mirror must not inherit
// it and overwrite that file with its own trace path after the command has
// returned. The mirror has no reader for a trace-ref file, so it carries none —
// it records traces by trigger alone.
func mirrorEnv() []string {
	stripped := []string{
		automationTriggerEnvVar + "=",
		automationReasonEnvVar + "=",
		automationTraceRefFileEnvVar + "=",
	}
	parent := os.Environ()
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		keep := true
		for _, prefix := range stripped {
			if strings.HasPrefix(kv, prefix) {
				keep = false
				break
			}
		}
		if keep {
			env = append(env, kv)
		}
	}
	return append(env,
		automationTriggerEnvVar+"=on-change",
		automationReasonEnvVar+"=on-change cadence mirrored after a mutating command",
	)
}

// runBackgroundMirror is the detached worker. It runs as its own process after
// the spawning command has returned, so it must establish two invariants before
// touching the store: the command's engine is released (wait-for-parent), and
// no other mirror is running (single-flight). Only then does it open its own
// engine and push. [LAW:no-ambient-temporal-coupling]
func runBackgroundMirror(ctx context.Context, _ io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("sync " + backgroundMirrorSubcommand)
	parentPID := fs.Int("parent-pid", 0, "PID of the spawning command; the mirror waits for it to exit")
	if err := parseFlagSet(fs, args, io.Discard); err != nil {
		return err
	}

	// 1. Wait for the spawning command's embedded engine to be released. Opening
	// a second engine on the same path while the first is live collides on
	// Dolt's online garbage collection. If the parent outlives the timeout, the
	// precondition is unmet — abort rather than race a live engine.
	// [LAW:no-ambient-temporal-coupling]
	if !waitForProcessExit(*parentPID, mirrorParentWaitTimeout, mirrorParentPollDelay) {
		return recordMirrorError(ws, fmt.Errorf(
			"spawning command (pid %d) still running after %s; skipping mirror to avoid racing its engine",
			*parentPID, mirrorParentWaitTimeout))
	}

	// 2. Single-flight. A lost race is the coalescing path, not an error: the
	// holding mirror pushes the current HEAD (which already includes this
	// commit) and re-reads freshness before it releases.
	release, acquired, err := store.TryAcquireSyncPushLock(ws.DatabasePath)
	if err != nil {
		return recordMirrorError(ws, fmt.Errorf("acquire sync-push lock: %w", err))
	}
	if !acquired {
		return nil
	}
	// The kernel drops the flock on process exit, so an unlock error here cannot
	// strand the lock; surfacing it would only add noise to a detached worker.
	defer func() { _ = release() }()

	// 3. Mirror on this worker's own engine — the only one open on the path now.
	syncStore, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return recordMirrorError(ws, fmt.Errorf("open sync store: %w", err))
	}
	defer syncStore.Close()
	return mirrorOnce(ctx, syncStore, ws)
}

// mirrorOnce runs the one shared push path, without compaction. It is a single
// path with no freshness branch: [LAW:dataflow-not-control-flow] the skip
// decisions (no remote, empty remote) already live in performSyncPush, and an
// up-to-date push is a cheap no-op, so the mirror does not pre-decide whether to
// push. It does not loop, either — the engine reports the tracking ref "as of
// last fetch", so an in-session re-read is stale. Coalescing of a burst comes
// from dolt push sending the current HEAD (commits that landed before this push
// go out with it) funnelled through the single-flight lock; a commit that lands
// after this push is mirrored by the next mutation's mirror or the pre-push
// hook. The unsynced window shrinks toward zero without ever blocking a mutation.
func mirrorOnce(ctx context.Context, syncStore *store.Store, ws workspace.Info) error {
	// The mirror pushes without compaction — plain SyncPush, never the
	// compact-and-push variant the explicit command uses.
	outcome, err := performSyncPush(ctx, syncStore, ws, "", false, false, syncStore.SyncPush)
	if err != nil {
		// Could-not-attempt (reconcile/remote resolution): record and stop.
		return recordMirrorError(ws, err)
	}
	// performSyncPush records its own trace (push-ok, push-failure, or skip). If
	// that trace write itself failed, surface it rather than drop it. [LAW:no-silent-failure]
	if outcome.traceErr != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change mirror trace not recorded: %v\n", outcome.traceErr)
	}
	// outcome.pushErr (e.g. offline) is already captured in that trace; the
	// mutation is durable locally and the next push retries, so the mirror stops
	// cleanly either way.
	return nil
}

// waitForProcessExit blocks until pid is gone, returning true, or the timeout
// elapses with the process still alive, returning false. The ordering owner is
// the liveness check, not the sleep: each iteration tests the real signal
// (process gone) and the poll delay is only the interval between checks. The
// boolean lets the caller distinguish "parent released the engine" from "parent
// outlived the wait" rather than proceeding blindly on a timeout.
// [LAW:no-ambient-temporal-coupling]
func waitForProcessExit(pid int, timeout, poll time.Duration) bool {
	if pid <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
	return true
}

// recordMirrorError writes the failure to the shared automation trace so a
// detached mirror that fails is loud out-of-band rather than silent. It always
// returns nil: the mutation is already durable, so the mirror is best-effort
// and never reports a non-zero exit. [LAW:no-silent-failure] If the trace write
// itself fails, the error is not swallowed — it goes to stderr, the worker's
// only remaining channel (discarded when detached, visible when the hidden
// subcommand is run in the foreground for debugging).
func recordMirrorError(ws workspace.Info, cause error) error {
	if _, traceErr := maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync push",
		"mirror Dolt data to the configured git remote",
		"error",
		cause.Error(),
		map[string]string{"error": cause.Error()},
	); traceErr != nil {
		fmt.Fprintf(os.Stderr,
			"lit: on-change mirror could not record failure trace (%v); original error: %v\n",
			traceErr, cause)
	}
	return nil
}
