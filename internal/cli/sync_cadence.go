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

// disableAutoSyncEnvVar is the process-level kill switch for all automatic sync
// (the on-change push mirror and the receive). When set to a truthy value, no
// command schedules a mirror or runs a receive. It exists for environments that
// must never trigger sync as a side effect of a lit command — CI, sandboxes, and
// lit's own test suite — and is distinct from `sync.receive = false` (which
// disables only receive, via config).
const disableAutoSyncEnvVar = "LIT_DISABLE_AUTO_SYNC"

// receiveDebounceInterval bounds how often an automatic receive runs: a command
// burst (an agent running many commands in seconds) triggers at most one fetch
// per interval. The receive is inline, so this also bounds how often a command
// pays the fetch latency.
const receiveDebounceInterval = 10 * time.Second

// shouldSyncAfterMutation is the pure push-cadence decision: only a mutating
// (write) command under the on-change policy triggers the push mirror. Read-mode
// commands and the default on-push policy never do.
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
	if isTruthyEnv(os.Getenv(disableAutoSyncEnvVar)) {
		return
	}
	cfg, err := config.Load(pathspec.New(ws.RootDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic sync skipped, config unreadable: %v\n", err)
		return
	}
	if shouldSyncAfterMutation(accessMode, cfg.Sync.Cadence) {
		if err := spawnBackgroundMirror(ws, os.Getpid()); err != nil {
			fmt.Fprintf(os.Stderr, "lit: on-change background push not started: %v\n", err)
		}
	}
	if cfg.Sync.Receive {
		receiveInline(ctx, ws)
	}
}

// receiveMarkerPath is the single debounce marker for automatic receive: its
// modification time is the last time a receive was attempted. [LAW:one-source-of-truth]
func receiveMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "receive.last")
}

// shouldReceiveNow reports whether the debounce interval has elapsed since the
// last receive attempt. A missing or unreadable marker means "never received (or
// cannot tell)" → allow. now and interval are parameters so the decision is
// testable without sleeping.
func shouldReceiveNow(ws workspace.Info, now time.Time, interval time.Duration) bool {
	info, err := os.Stat(receiveMarkerPath(ws))
	if err != nil {
		return true
	}
	return now.Sub(info.ModTime()) >= interval
}

// markReceiveAttempt records "a receive was attempted now" by setting the marker
// file's modification time to now (creating it if absent).
func markReceiveAttempt(ws workspace.Info) error {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for receive marker: %w", err)
	}
	if err := os.WriteFile(receiveMarkerPath(ws), nil, 0o644); err != nil {
		return fmt.Errorf("write receive marker: %w", err)
	}
	return nil
}

// workspaceHasGitRemote reports whether the workspace has at least one git remote
// configured — the cheap precondition for any receive, so a single-machine repo
// never opens a sync store only to resolve "no remote". An error reading remotes
// is treated as "cannot tell, skip": receive is best-effort and the next interval
// retries. [LAW:no-silent-failure] The skip is a real domain choice (no confirmed
// remote), not a swallowed failure.
func workspaceHasGitRemote(ws workspace.Info) bool {
	remotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return false
	}
	return len(remotes) > 0
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
