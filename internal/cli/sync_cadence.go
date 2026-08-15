package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// DisableAutoSyncEnvVar is the process-level kill switch for all automatic sync
// (the on-change push mirror and the receive). When set to a truthy value, no
// command schedules a mirror or runs a receive. It exists for environments that
// must never trigger sync as a side effect of a lit command — CI, sandboxes, and
// lit's own test suite — and is distinct from `sync.receive = false` (which
// disables only receive, via config). Exported so out-of-package callers (the
// cmd/lit signal acceptance test) target the one canonical env-var name rather
// than a drift-prone literal. [LAW:one-source-of-truth]
const DisableAutoSyncEnvVar = "LIT_DISABLE_AUTO_SYNC"

// receiveDebounceInterval bounds how often an automatic receive runs: a command
// burst (an agent running many commands in seconds) triggers at most one fetch
// per interval. The receive is inline, so this also bounds how often a command
// pays the fetch latency.
const receiveDebounceInterval = 10 * time.Second

// mirrorSpawnDebounceInterval bounds how often a command spawns a NEW on-change
// mirror subprocess: a mutation burst triggers at most one spawn per interval,
// not one fork/exec per mutation (links-sync-pgct.11). The single-flight sync-push
// lock (TryAcquireSyncPushLock) already stops two mirrors from both opening an
// engine, but it does nothing about the fork/exec + wait-for-parent-exit startup
// cost of every LOSING mirror in between — each one is real CPU/process-table
// pressure that a loaded machine feels, and that pressure is what turned into
// engine-write-lock contention exhausting the transient-retry budget on CI (a
// burst of `lit new` calls with no pacing spawned up to 15 subprocesses, most of
// which existed only to lose the single-flight race). Shorter than
// receiveDebounceInterval: push latency is the whole point of on-change cadence
// (no explicit `sync push` step), so an ordinary, non-bursty mutation should still
// get a near-immediate mirror — only a genuine burst coalesces. Coalescing is
// still safe at any interval: the eventual mirror pushes the current HEAD, which
// already includes every commit that landed since the last one that ran (same
// "commits that landed before this push go out with it" property spawnBackgroundMirror
// already documents for the single-flight lock).
const mirrorSpawnDebounceInterval = 1 * time.Second

// shouldSyncAfterMutation is the pure push-cadence decision: only a mutating
// (write) command under the on-change policy triggers the push mirror. Read-mode
// commands and the opt-in on-push policy never do.
// [LAW:dataflow-not-control-flow] The access mode is the mutation marker that
// already flows through the one app boundary; no command re-decides this.
func shouldSyncAfterMutation(accessMode app.AccessMode, cadence config.SyncCadence) bool {
	return accessMode == app.AccessWrite && cadence == config.SyncCadenceOnChange
}

// maybeAutoSyncAfterCommand is the single owner of automatic sync. It runs after
// a successful command AND after that command's embedded engine has been closed,
// reads config once, and performs the two orthogonal halves the policy selects:
// the push mirror (a mutating command under on-change cadence) and the receive
// (any command, when enabled). [LAW:single-enforcer] Command handlers stay
// unaware of either policy.
//
// The push mirror is a detached worker that opens its own engine only after this
// process exits; the receive is inline and runs now, on its own engine, with no
// other engine open in this process — embedded Dolt permits exactly one
// read-write engine per path, so the receive must never overlap the command's.
// [LAW:no-ambient-temporal-coupling]
func maybeAutoSyncAfterCommand(ctx context.Context, accessMode app.AccessMode, ws workspace.Info) {
	if isTruthyEnv(os.Getenv(DisableAutoSyncEnvVar)) {
		return
	}
	cfg, err := config.Load(pathspec.New(ws.RootDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic sync skipped, config unreadable: %v\n", err)
		return
	}
	if shouldSyncAfterMutation(accessMode, cfg.Sync.Cadence) && shouldSpawnMirrorNow(ws, time.Now(), mirrorSpawnDebounceInterval) {
		// The marker records "an attempt happened" and is written first,
		// unconditionally for every outcome below — remote-check error, no
		// remote, spawn failure, spawn success. The debounce rate-limits the
		// whole attempt (including its stderr noise when something is wrong),
		// not just the happy path: a burst against a broken remote check must
		// not re-run the check and re-print the warning on every mutation.
		// [LAW:dataflow-not-control-flow]
		if err := markMirrorSpawnAttempt(ws); err != nil {
			fmt.Fprintf(os.Stderr, "lit: on-change mirror-spawn debounce marker not written: %v\n", err)
		}
		// Cheap precondition, mirroring receiveInline's own check below: a
		// remote-less workspace has nothing to push to, so skip the subprocess
		// spawn entirely rather than pay fork/exec cost only to have the mirror
		// discover "no remote" for itself. This matters more now that on-change
		// is the shipped default (links-sync-pgct.3) rather than an opt-in a
		// user chose knowing the cost. [LAW:carrying-cost]
		hasRemote, err := workspaceHasGitRemote(ctx, ws)
		if err != nil {
			// Unexpected — surface it rather than silently skip or silently spawn
			// against unknown remote state. [LAW:no-silent-failure]
			fmt.Fprintf(os.Stderr, "lit: on-change background push not started, could not check git remotes: %v\n", err)
		} else if hasRemote {
			if err := spawnBackgroundMirror(ws, os.Getpid()); err != nil {
				fmt.Fprintf(os.Stderr, "lit: on-change background push not started: %v\n", err)
			}
		}
	}
	if cfg.Sync.Receive {
		receiveInline(ctx, ws)
	}
}

// shouldRunNow reports whether the debounce interval has elapsed since
// markerPath was last touched. A missing or unreadable marker means "never run
// (or cannot tell)" → allow. now and interval are parameters so the decision is
// testable without sleeping. [LAW:one-type-per-behavior] One debounce primitive;
// automatic receive and the on-change mirror spawn are two instances
// distinguished only by which marker path and interval they pass in — neither
// owns its own copy of the stat-and-compare logic.
func shouldRunNow(markerPath string, now time.Time, interval time.Duration) bool {
	info, err := os.Stat(markerPath)
	if err != nil {
		return true
	}
	return now.Sub(info.ModTime()) >= interval
}

// markRunAttempt records "this ran now" by setting markerPath's modification
// time to now, creating it if absent.
func markRunAttempt(ws workspace.Info, markerPath string) error {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for debounce marker: %w", err)
	}
	if err := os.WriteFile(markerPath, nil, 0o644); err != nil {
		return fmt.Errorf("write debounce marker %s: %w", filepath.Base(markerPath), err)
	}
	return nil
}

// writeMarkerAtomic writes a storage-dir marker via temp-file-and-rename: two
// lit processes (a foreground command and a detached mirror) can complete
// near-simultaneously, and a reader must see one whole payload or the other,
// never a torn write. [LAW:no-ambient-temporal-coupling]
// [LAW:one-type-per-behavior] the one atomic-marker primitive for the cli's
// storage-dir markers; the push-outcome and owner-notify markers are instances
// distinguished only by path and payload. The store's adopt-pending marker
// (store.writeAdoptPendingMarker) is the same temp-and-rename shape plus the
// fsync its crash-survival contract needs — it cannot share this function
// without an import cycle (store cannot import cli), so the two are kept
// deliberately congruent rather than unified.
func writeMarkerAtomic(ws workspace.Info, markerPath string, payload []byte) error {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(ws.StorageDir, filepath.Base(markerPath)+"-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), markerPath)
}

// receiveMarkerPath is the single debounce marker for automatic receive: its
// modification time is the last time a receive was attempted. [LAW:one-source-of-truth]
func receiveMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "receive.last")
}

// shouldReceiveNow reports whether the debounce interval has elapsed since the
// last receive attempt.
func shouldReceiveNow(ws workspace.Info, now time.Time, interval time.Duration) bool {
	return shouldRunNow(receiveMarkerPath(ws), now, interval)
}

// markReceiveAttempt records "a receive was attempted now".
func markReceiveAttempt(ws workspace.Info) error {
	return markRunAttempt(ws, receiveMarkerPath(ws))
}

// mirrorSpawnMarkerPath is the single debounce marker for the on-change mirror
// spawn: its modification time is the last time a mirror subprocess was
// spawned. [LAW:one-source-of-truth]
func mirrorSpawnMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "mirror-spawn.last")
}

// shouldSpawnMirrorNow reports whether the debounce interval has elapsed since
// the last mirror spawn.
func shouldSpawnMirrorNow(ws workspace.Info, now time.Time, interval time.Duration) bool {
	return shouldRunNow(mirrorSpawnMarkerPath(ws), now, interval)
}

// markMirrorSpawnAttempt records "a mirror was spawned now".
func markMirrorSpawnAttempt(ws workspace.Info) error {
	return markRunAttempt(ws, mirrorSpawnMarkerPath(ws))
}

// workspaceHasGitRemote reports whether the workspace has at least one git remote
// configured — the cheap precondition for any receive, so a single-machine repo
// never opens a sync store only to resolve "no remote". It returns the read error
// rather than collapsing it into false: "no remote configured" (false, nil) is a
// legitimate silent skip, but "could not read remotes" is an unexpected failure
// the caller must surface, not silently treat as absence. [LAW:no-silent-failure]
// [LAW:no-defensive-null-guards] The two conditions are distinct values, not one
// folded bool.
func workspaceHasGitRemote(ctx context.Context, ws workspace.Info) (bool, error) {
	remotes, err := workspace.GitRemotes(ctx, ws.RootDir)
	if err != nil {
		return false, err
	}
	return len(remotes) > 0, nil
}

// isTruthyEnv reports whether an environment value enables a flag. It accepts the
// standard boolean spellings (1/0, t/f, true/false, case-insensitive) and treats
// anything unrecognized — including empty/unset — as false, so a flag is only
// enabled by an explicit truthy value.
func isTruthyEnv(value string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return parsed
}
