package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workerHandoff is what a command hands the detached worker it spawns: the
// decisions it made from its own checkout, as values. The worker outlives the
// command, and the checkout may be removed before the worker reads anything,
// so the worker asks the checkout nothing; what it needs from the repository
// it asks the repository. [LAW:no-ambient-temporal-coupling]
//
// It travels on the worker's stdin, which reaches the worker alone: the git,
// hook and other children the worker starts get /dev/null, and the project
// config it carries never shows in a process listing.
type workerHandoff struct {
	// CheckoutRoot is the launcher's checkout root, the base a relative path
	// URL is joined to, lexically.
	CheckoutRoot string `json:"checkout_root"`
	// SyncRemote is the remote the launcher's sync-remote precedence chose, ""
	// for none.
	SyncRemote string `json:"sync_remote"`
	// Project is the launcher's project config layer's content.
	Project []byte `json:"project"`
}

// handoffOf is the handoff a command spawning a worker from ws gives it. A
// worker spawned by a worker re-hands what it was handed: every field is read
// off ws's values, none from disk. The sync remote is chosen here, by the one
// precedence (chooseSyncRemote), which asks the upstream only when its answer
// can decide. [LAW:single-enforcer]
func handoffOf(ctx context.Context, ws workspace.Info) (workerHandoff, error) {
	remotes, err := ws.Remotes.List(ctx)
	if err != nil {
		return workerHandoff{}, fmt.Errorf("list git remotes for the worker: %w", err)
	}
	chosen, found, err := chooseSyncRemote("", ws.Remotes.Upstream, remotes)
	if err != nil {
		return workerHandoff{}, fmt.Errorf("choose the worker's sync remote: %w", err)
	}
	project, err := ws.Project.Content()
	if err != nil {
		return workerHandoff{}, fmt.Errorf("read the project config for the worker: %w", err)
	}
	handoff := workerHandoff{CheckoutRoot: ws.Remotes.Root().String(), Project: project}
	if found {
		handoff.SyncRemote = chosen.Name()
	}
	return handoff, nil
}

// workerWorkspace is what a worker's build of its workspace came to: the
// handed workspace, or, when the handoff or the store's config could not be
// read, the store alone and the reason. [LAW:no-silent-failure] each worker
// routes a failed build through the seam its other pre-attempt deaths use, at
// the point they use it; nothing that can fail runs before the worker's own
// ordering does.
type workerWorkspace struct {
	ws     workspace.Info
	failed error
}

// buildWorkerWorkspace reads the handoff from stdin and builds the workspace
// for the store at loc. A failed build's workspace is the store with no
// project layer of its own, so a failure's completion reads the global
// config alone.
func buildWorkerWorkspace(ctx context.Context, env environ.Env, loc workspace.Location, stdin io.Reader) workerWorkspace {
	ws, err := handedWorkspace(env, loc, stdin)
	if err != nil {
		failedWS := infoForLocation(loc)
		failedWS.Remotes = workspace.HandedRemotes(env, loc.Repository(), workspace.CheckoutRoot{}, "")
		failedWS.Project = config.HandedProject(nil)
		return workerWorkspace{ws: failedWS, failed: fmt.Errorf("build the worker's workspace: %w", err)}
	}
	reportConfigLayers(ctx, env, ws.Project)
	return workerWorkspace{ws: ws}
}

func handedWorkspace(env environ.Env, loc workspace.Location, stdin io.Reader) (workspace.Info, error) {
	var handoff workerHandoff
	if err := json.NewDecoder(stdin).Decode(&handoff); err != nil {
		return workspace.Info{}, fmt.Errorf("read the launcher's handoff: %w", err)
	}
	root, err := workspace.HandedCheckoutRoot(handoff.CheckoutRoot)
	if err != nil {
		return workspace.Info{}, err
	}
	return workspace.Handed(env, loc, root, handoff.SyncRemote, config.HandedProject(handoff.Project))
}
