package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// backgroundMirrorSubcommand is the hidden `lit sync` subcommand the on-change
// cadence owner spawns. It is absent from the family usage string, so it never
// appears in help; it exists only as the detached worker's entrypoint.
const backgroundMirrorSubcommand = "__mirror-bg"

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
	// A detached worker owns no terminal; discarding its streams keeps it from
	// writing over the command's output. Failures surface via the trace files.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachSysProcAttr()
	cmd.Env = append(os.Environ(),
		automationTriggerEnvVar+"=on-change",
		automationReasonEnvVar+"=on-change cadence mirrored after a mutating command",
	)
	return cmd.Start()
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
	// Dolt's online garbage collection.
	waitForProcessExit(*parentPID, mirrorParentWaitTimeout, mirrorParentPollDelay)

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

// mirrorOnce pushes the local branch to the remote once, without compaction.
// [LAW:dataflow-not-control-flow] It does not loop re-checking freshness: the
// embedded engine reports the remote-tracking ref "as of last fetch", so an
// in-session re-read after a push is stale and cannot tell the loop when to
// stop. Coalescing of a burst instead comes from two facts that need no loop —
// dolt push sends the current HEAD (so commits that landed before this push go
// out with it), and the single-flight lock funnels concurrent mutations through
// one mirror. A commit that lands after this push, while the lock is still held,
// is mirrored by the next mutation's mirror or the pre-push hook; the unsynced
// window shrinks toward zero without ever blocking a mutation.
func mirrorOnce(ctx context.Context, syncStore *store.Store, ws workspace.Info) error {
	ahead, synced, err := mirrorAhead(ctx, syncStore, ws)
	if err != nil {
		return recordMirrorError(ws, err)
	}
	// Never-synced means there is no remote-tracking ref yet: seeding an empty
	// remote is the explicit first `lit sync push --set-upstream`, not an
	// on-change side effect — the same skip `lit sync push` already makes. A zero
	// lead (as of last fetch) means there is nothing new to mirror.
	if !synced || ahead == 0 {
		return nil
	}
	outcome, err := performSyncPush(ctx, syncStore, ws, "", false, false, false)
	if err != nil {
		// Could-not-attempt (reconcile/remote resolution): record and stop.
		return recordMirrorError(ws, err)
	}
	if outcome.pushErr != nil {
		// The push ran and failed (e.g. offline). performSyncPush already recorded
		// the error trace; the mutation is durable locally and the next push
		// retries, so the mirror stops cleanly. [LAW:no-silent-failure]
		return nil
	}
	return nil
}

// mirrorAhead resolves the same remote and branch `lit sync` uses and returns
// how far the local branch leads the remote-tracking ref (as of the last
// fetch), plus whether a tracking ref exists at all. It reuses the shared
// resolution helpers so the mirror's target can never drift from the manual
// push's. [LAW:one-source-of-truth]
func mirrorAhead(ctx context.Context, syncStore *store.Store, ws workspace.Info) (int64, bool, error) {
	state, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return 0, false, err
	}
	remote, err := resolveSyncRemote("", workspace.UpstreamRemote(ws.RootDir), state.gitRemotes)
	if err != nil {
		return 0, false, err
	}
	if remote == "" {
		return 0, false, nil
	}
	branch, err := resolveSyncBranch(ws.RootDir, remote)
	if err != nil {
		return 0, false, err
	}
	freshness, err := syncStore.SyncFreshness(ctx, remote, branch)
	if err != nil {
		return 0, false, err
	}
	return freshness.Ahead, freshness.Synced, nil
}

// waitForProcessExit blocks until pid is gone or the timeout elapses. The
// ordering owner is the liveness check, not the sleep: each iteration tests the
// real signal (process gone) and the poll delay is only the interval between
// checks. [LAW:no-ambient-temporal-coupling]
func waitForProcessExit(pid int, timeout, poll time.Duration) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(poll)
	}
}

// recordMirrorError writes the failure to the shared automation trace so a
// detached mirror that fails is loud out-of-band rather than silent. It always
// returns nil: the mutation is already durable, so the mirror is best-effort
// and never reports a non-zero exit. [LAW:no-silent-failure]
func recordMirrorError(ws workspace.Info, cause error) error {
	_, _ = maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync push",
		"mirror Dolt data to the configured git remote",
		"error",
		cause.Error(),
		map[string]string{"error": cause.Error()},
	)
	return nil
}
