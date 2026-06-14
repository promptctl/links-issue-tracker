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
// selects on-change for a mutating command, mirrors the store to the remote.
// [LAW:single-enforcer] command handlers never see cadence; it is read and
// acted on here, once.
//
// The mirror runs in-process on the command's own open store, reusing the
// embedded Dolt engine that just performed the mutation — never a second
// engine on the same path, which would collide on Dolt's online GC.
// [LAW:no-ambient-temporal-coupling]
//
// Best-effort by contract: the mutation is already durable in the local Dolt
// store, so a config or push failure is surfaced loudly out-of-band (stderr
// plus the automation trace performSyncPush records) but never fails the
// command. [LAW:no-silent-failure]
func maybeSyncAfterMutation(ctx context.Context, accessMode app.AccessMode, ap *app.App) error {
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
	// The on-change cadence is the same mirror `lit sync push` performs with no
	// flags. [LAW:one-source-of-truth] The paired ticket (links-sync-eztx)
	// replaces this synchronous call with a non-blocking/debounced mirror; the
	// cadence decision above does not change.
	outcome, err := performSyncPush(ctx, ap.Store, ap.Workspace, "", false, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change sync failed: %v\n", err)
		return nil
	}
	if outcome.pushErr != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change sync failed: %v\n", outcome.pushErr)
	}
	return nil
}
