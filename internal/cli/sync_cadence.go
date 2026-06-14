package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

// shouldSyncAfterMutation is the pure cadence decision: only a mutating
// (write) command under the on-change policy triggers a mirror. Read-mode
// commands and the default on-push policy never do.
// [LAW:dataflow-not-control-flow] The access mode is the mutation marker that
// already flows through the one app boundary; no command re-decides this.
func shouldSyncAfterMutation(accessMode app.AccessMode, cadence config.SyncCadence) bool {
	return accessMode == app.AccessWrite && cadence == config.SyncCadenceOnChange
}

// maybeSyncAfterMutation is the single owner of sync scheduling. After a
// successful command it reads the configured cadence and, when the policy
// selects on-change for a mutating command, schedules a mirror to the remote.
// [LAW:single-enforcer] command handlers never see cadence; it is read and
// acted on here, once.
//
// The mirror is non-blocking: a detached worker process pushes after this
// command exits, so the mutation never waits on a network round-trip. The
// worker waits for this command's embedded Dolt engine to be released before
// opening its own — never two engines on one path, which would collide on
// Dolt's online GC. [LAW:no-ambient-temporal-coupling] [LAW:effects-at-boundaries]
//
// Best-effort by contract: the mutation is already durable in the local Dolt
// store, so a failure to even start the worker is surfaced on stderr but never
// fails the command; a push that starts and fails surfaces out-of-band through
// the worker's automation trace. [LAW:no-silent-failure]
func maybeSyncAfterMutation(_ context.Context, accessMode app.AccessMode, ap *app.App) error {
	if accessMode != app.AccessWrite {
		return nil
	}
	cfg, err := config.Load(pathspec.New(ap.Workspace.RootDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change sync skipped, config unreadable: %v\n", err)
		return nil
	}
	if !shouldSyncAfterMutation(accessMode, cfg.Sync.Cadence) {
		return nil
	}
	if err := spawnBackgroundMirror(ap.Workspace, os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change background sync not started: %v\n", err)
	}
	return nil
}
