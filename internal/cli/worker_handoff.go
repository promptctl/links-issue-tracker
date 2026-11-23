package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/store"
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
	// Deadlines are the launcher's run's (Invocation.Deadlines), which the
	// worker runs under in place of its own process's.
	Deadlines handedDeadlines `json:"deadlines"`
}

// handedDeadlines is store.Deadlines as the handoff carries it.
type handedDeadlines struct {
	MirrorHold time.Duration `json:"mirror_hold"`
	MirrorPush time.Duration `json:"mirror_push"`
	Receive    time.Duration `json:"receive"`
}

func handedDeadlinesOf(d store.Deadlines) handedDeadlines {
	return handedDeadlines{MirrorHold: d.MirrorHold, MirrorPush: d.MirrorPush, Receive: d.Receive}
}

// deadlines is the handed figures, or why there are none: a handoff that
// carries none is refused rather than read as deadlines of zero, which would
// cut every hold the worker takes. [LAW:parse-dont-validate]
func (h handedDeadlines) deadlines() (store.Deadlines, error) {
	if h == (handedDeadlines{}) {
		return store.Deadlines{}, errors.New("the launcher's handoff carries no deadlines")
	}
	return store.Deadlines{MirrorHold: h.MirrorHold, MirrorPush: h.MirrorPush, Receive: h.Receive}, nil
}

// handoffOf is the handoff a command spawning a worker from ws, under
// deadlines, gives it. A worker spawned by a worker re-hands what it was
// handed: every field is read off ws's values and the deadlines it was handed,
// none from disk. The sync remote is chosen here, by the one precedence
// (chooseSyncRemote), which asks the upstream only when its answer can decide.
// [LAW:single-enforcer]
func handoffOf(ctx context.Context, ws workspace.Info, deadlines store.Deadlines) (workerHandoff, error) {
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
	handoff := workerHandoff{CheckoutRoot: ws.Remotes.Root().String(), Project: project, Deadlines: handedDeadlinesOf(deadlines)}
	if found {
		handoff.SyncRemote = chosen.Name()
	}
	return handoff, nil
}

// workerWorkspace is what a worker's build of its workspace came to: the
// handed workspace and deadlines, or, when the handoff or the store's config
// could not be read, the store alone, the worker's own deadlines, and the
// reason. [LAW:no-silent-failure] each worker
// routes a failed build through the seam its other pre-attempt deaths use, at
// the point they use it; nothing that can fail runs before the worker's own
// ordering does.
type workerWorkspace struct {
	ws        workspace.Info
	deadlines store.Deadlines
	failed    error
}

// buildWorkerWorkspace reads the handoff from stdin and builds the workspace
// for the store at loc. A failed build's workspace is the store with no
// project layer (config.NoProject), so a failure's completion reads the
// global config alone, and its deadlines are own, the worker process's.
func buildWorkerWorkspace(ctx context.Context, stderr io.Writer, env environ.Env, loc workspace.Location, stdin io.Reader, own store.Deadlines) workerWorkspace {
	built := handedWorkerWorkspace(env, loc, stdin, own)
	// Every worker's event says which project layer it ran with, a failed
	// build's included. [LAW:nothing-unseen]
	reportConfigLayers(ctx, stderr, env, built.ws.Project)
	return built
}

func handedWorkerWorkspace(env environ.Env, loc workspace.Location, stdin io.Reader, own store.Deadlines) workerWorkspace {
	ws, deadlines, err := handedWorkspace(env, loc, stdin)
	if err != nil {
		failedWS := infoForLocation(loc)
		failedWS.Project = config.NoProject()
		return workerWorkspace{ws: failedWS, deadlines: own, failed: fmt.Errorf("build the worker's workspace: %w", err)}
	}
	return workerWorkspace{ws: ws, deadlines: deadlines}
}

func handedWorkspace(env environ.Env, loc workspace.Location, stdin io.Reader) (workspace.Info, store.Deadlines, error) {
	var handoff workerHandoff
	if err := json.NewDecoder(stdin).Decode(&handoff); err != nil {
		return workspace.Info{}, store.Deadlines{}, fmt.Errorf("read the launcher's handoff: %w", err)
	}
	deadlines, err := handoff.Deadlines.deadlines()
	if err != nil {
		return workspace.Info{}, store.Deadlines{}, err
	}
	root, err := workspace.HandedCheckoutRoot(handoff.CheckoutRoot)
	if err != nil {
		return workspace.Info{}, store.Deadlines{}, err
	}
	ws, err := workspace.Handed(env, loc, root, handoff.SyncRemote, config.HandedProject(handoff.Project))
	return ws, deadlines, err
}
