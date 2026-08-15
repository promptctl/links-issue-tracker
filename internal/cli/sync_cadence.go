package cli

import (
	"context"
	"errors"
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

// remoteAbsentRecheckInterval bounds how often a confirmed remote-less
// workspace re-runs the mirror path's git-remote check. The mirror-pending
// claim carries the coverage guarantee only where a remote exists; without
// one, every mutation would otherwise pay a git subprocess plus marker
// create/remove churn re-confirming the same absence — the rate bound the
// deleted spawn debounce used to provide for exactly this state.
// [LAW:carrying-cost] The only cost is mirror onset: a remote added to a
// hot workspace waits at most this long before mutations resume claiming.
const remoteAbsentRecheckInterval = 10 * time.Second

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
	if shouldSyncAfterMutation(accessMode, cfg.Sync.Cadence) {
		ensureMirrorCoverage(ctx, ws)
	}
	if cfg.Sync.Receive {
		receiveInline(ctx, ws)
	}
}

// ensureMirrorCoverage upholds the on-change tail guarantee for the mutation
// that just committed (links-sync-pgct.12): claim the mirror-pending marker,
// and either a fresh marker proves an already-spawned, not-yet-cleared mirror
// will read HEAD after this command's closed engine session (see
// sync_mirror_pending.go for the ordering proof), or this command owns the
// spawn. Spawn rate falls out of the claim itself — one spawn per
// clear-to-claim cycle, however dense the burst — replacing the fixed 1s
// debounce whose window was exactly the stranded-tail bug.
// [LAW:no-ambient-temporal-coupling]
//
// Every failure that breaks the claim-to-mirror chain completes through the
// same pushOutcomeRecord seam as a push attempt that ran (completePushAttempt),
// so the staleness banner and the owner's out-of-band channel hear about a
// mirror that could not even start exactly the way they hear about a push
// that failed — one representation of push health, never a second.
// [LAW:one-source-of-truth] The claim this command owns is then released so
// the next mutation retries the spawn instead of trusting a mirror that never
// launched — and only an owned claim is released, so a degraded claim read can
// never remove another mutation's live claim.
func ensureMirrorCoverage(ctx context.Context, ws workspace.Info) {
	// A recently confirmed remote-less workspace short-circuits before the
	// claim: no remote means no coverage to owe, so no marker churn and no git
	// subprocess per mutation — the debounce applies only to re-confirming
	// absence, never to any path that issues a coverage verdict.
	if !shouldRunNow(remoteAbsentMarkerPath(ws), time.Now(), remoteAbsentRecheckInterval) {
		return
	}
	ownsClaim := false
	claim, claimErr := claimMirrorPending(ws, time.Now())
	switch {
	case claimErr != nil:
		// Coverage state unreadable: spawning is the safe side (a redundant
		// mirror is a no-op push behind the single-flight lock; a skipped one
		// is a stranded tail), but the broken marker is loud. [LAW:no-silent-failure]
		fmt.Fprintf(os.Stderr, "lit: mirror-pending marker unavailable (%v); spawning a mirror regardless\n", claimErr)
	case claim == pendingCovered:
		return
	default:
		ownsClaim = true
	}
	// An owned claim that turns out unspawnable is released BEFORE the
	// completion effects below: the completion can run the owner-notify hook
	// to its cap, and mutations landing during it must not read the doomed
	// claim as live coverage. [LAW:no-ambient-temporal-coupling]
	releaseClaim := func() {
		if ownsClaim {
			clearMirrorPending(ws)
		}
	}
	// Cheap precondition, mirroring receiveInline's own check: a remote-less
	// workspace has nothing to push to, so skip the subprocess spawn entirely
	// rather than pay fork/exec cost only to have the mirror discover "no
	// remote" for itself. This matters more now that on-change is the shipped
	// default (links-sync-pgct.3) rather than an opt-in a user chose knowing
	// the cost. [LAW:carrying-cost]
	hasRemote, err := workspaceHasGitRemote(ctx, ws)
	if err != nil {
		releaseClaim()
		fmt.Fprintf(os.Stderr, "lit: on-change background push not started, could not check git remotes: %v\n", err)
		completePushAttempt(ctx, ws, syncPushOutcome{}, fmt.Errorf("check git remotes before on-change mirror spawn: %w", err))
		return
	}
	if !hasRemote {
		// A healthy skip, not a failure: with no remote there is no coverage to
		// owe, and un-claiming keeps the marker truthful for the day a remote
		// is added. The push-outcome marker is untouched — the mirror path owns
		// writing "no_sync_remote" when an attempt actually resolves remotes —
		// and the absence marker rate-bounds re-confirming this state.
		releaseClaim()
		if err := markRunAttempt(ws, remoteAbsentMarkerPath(ws)); err != nil {
			fmt.Fprintf(os.Stderr, "lit: remote-absent marker not written: %v\n", err)
		}
		return
	}
	// A connected workspace retires any leftover absence marker so the next
	// remote-less confirmation starts a fresh interval rather than inheriting
	// a stale one. Absent is the common case and costs one syscall.
	if err := os.Remove(remoteAbsentMarkerPath(ws)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "lit: remote-absent marker not cleared: %v\n", err)
	}
	if err := spawnBackgroundMirror(ws, os.Getpid()); err != nil {
		releaseClaim()
		fmt.Fprintf(os.Stderr, "lit: on-change background push not started: %v\n", err)
		completePushAttempt(ctx, ws, syncPushOutcome{}, fmt.Errorf("spawn on-change mirror: %w", err))
	}
}

// shouldRunNow reports whether the debounce interval has elapsed since
// markerPath was last touched. A missing or unreadable marker means "never run
// (or cannot tell)" → allow. now and interval are parameters so the decision is
// testable without sleeping. [LAW:one-type-per-behavior] The one debounce
// primitive, parametrized by marker path and interval; automatic receive and
// the remote-absent recheck are its instances (the on-change mirror spawn
// stopped being one when links-sync-pgct.12 replaced its time window with the
// mirror-pending claim — a rate bound cannot carry a coverage guarantee).
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

// remoteAbsentMarkerPath is the single marker for "this workspace was last
// confirmed to have no git remote": its modification time is when that
// confirmation ran. [LAW:one-source-of-truth]
func remoteAbsentMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "remote-absent.last")
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
