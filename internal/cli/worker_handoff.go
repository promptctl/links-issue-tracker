package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workerHandoff is what a command hands the detached worker it spawns: the
// decisions it made from its own checkout, and the facts its run was composed
// with, as values. The worker outlives the command, and the checkout may be
// removed before the worker reads anything, so the worker asks the checkout
// nothing; what it needs from the repository it asks the repository.
// [LAW:no-ambient-temporal-coupling]
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
	// Deadlines, ReleasedSchema and RaisePolicy are the launcher's run's, off
	// its Env, which the worker runs under in place of its own process's: a
	// run's facts are the run's whole tree of processes. In production they
	// equal the worker's own, since the launcher and the worker are one
	// binary; a test's run hands its own. Deadlines reads back only as
	// deadline.Of builds them. [LAW:one-source-of-truth]
	Deadlines      deadline.Deadlines    `json:"deadlines"`
	ReleasedSchema version.SchemaCeiling `json:"released_schema"`
	RaisePolicy    version.RaisePolicy   `json:"raise_policy"`
}

// handoffOf is the handoff a command spawning a worker from ws, in env, gives
// it. A worker spawned by a worker re-hands what it was handed: every field is
// read off ws's values and env, which carries what the worker was handed, none
// from disk. The sync remote is chosen here, by the one precedence
// (chooseSyncRemote), which asks the upstream only when its answer can decide.
// [LAW:single-enforcer]
func handoffOf(ctx context.Context, ws workspace.Info, env environ.Env) (workerHandoff, error) {
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
	handoff := workerHandoff{
		CheckoutRoot:   ws.Remotes.Root().String(),
		Project:        project,
		Deadlines:      env.Deadlines(),
		ReleasedSchema: env.ReleasedSchema(),
		RaisePolicy:    env.RaisePolicy(),
	}
	if found {
		handoff.SyncRemote = chosen.Name()
	}
	return handoff, nil
}

// workerWorkspace is what a worker's build of its workspace came to: built
// (builtWorker), or failed (failedWorker) when the handoff or the store's
// config could not be read. [LAW:types-are-the-program] each arm holds only
// what it has: a failed build was handed no run facts, so it has none to read.
// [LAW:no-silent-failure] each worker routes a failed build through the seam
// its other pre-attempt deaths use, at the point they use it; nothing that can
// fail runs before the worker's own ordering does.
type workerWorkspace interface {
	// info is the workspace the worker runs against: the handed one, or, for a
	// failed build, the store alone with no project layer (config.NoProject),
	// so a failure's completion reads the global config alone.
	info() workspace.Info
	// runOf is inv as the worker runs it: under the run facts its spawner
	// handed it when built, and as its own process's when the build failed,
	// since nothing was handed.
	runOf(inv Invocation) Invocation
}

// builtWorker is a workspace built from the handoff, and the worker process's
// environment under the run facts the handoff carried.
type builtWorker struct {
	ws  workspace.Info
	env environ.Env
}

func (b builtWorker) info() workspace.Info { return b.ws }

func (b builtWorker) runOf(inv Invocation) Invocation { return inv.withEnv(b.env) }

// failedWorker is a build that failed, and why.
type failedWorker struct {
	ws  workspace.Info
	err error
}

func (f failedWorker) info() workspace.Info { return f.ws }

func (f failedWorker) runOf(inv Invocation) Invocation { return inv }

// buildWorkerWorkspace reads the handoff from stdin and builds the workspace
// for the store at loc, in the worker process's env.
func buildWorkerWorkspace(ctx context.Context, stderr io.Writer, env environ.Env, loc workspace.Location, stdin io.Reader) workerWorkspace {
	built := handedWorkerWorkspace(env, loc, stdin)
	// Every worker's event says which project layer it ran with, a failed
	// build's included. [LAW:nothing-unseen]
	reportConfigLayers(ctx, stderr, env, built.info().Project)
	return built
}

func handedWorkerWorkspace(env environ.Env, loc workspace.Location, stdin io.Reader) workerWorkspace {
	ws, handedEnv, err := handedWorkspace(env, loc, stdin)
	if err != nil {
		failedWS := infoForLocation(loc)
		failedWS.Project = config.NoProject()
		return failedWorker{ws: failedWS, err: fmt.Errorf("build the worker's workspace: %w", err)}
	}
	return builtWorker{ws: ws, env: handedEnv}
}

// handedWorkspace is the workspace the handoff on stdin builds, and env under
// the run facts it carries. A handoff whose deadlines are missing or not
// positive does not decode (deadline.Deadlines.UnmarshalJSON), and one that
// carries no released-schema ceiling is refused: every ceiling names its bound,
// so a missing one is no ceiling at all, not a ceiling of 0.
// [LAW:parse-dont-validate]
func handedWorkspace(env environ.Env, loc workspace.Location, stdin io.Reader) (workspace.Info, environ.Env, error) {
	var handoff workerHandoff
	if err := json.NewDecoder(stdin).Decode(&handoff); err != nil {
		return workspace.Info{}, environ.Env{}, fmt.Errorf("read the launcher's handoff: %w", err)
	}
	if handoff.ReleasedSchema.Bound == "" {
		return workspace.Info{}, environ.Env{}, errors.New("read the launcher's handoff: it carries no released-schema ceiling")
	}
	handedEnv := env.WithDeadlines(handoff.Deadlines).WithReleasedSchema(handoff.ReleasedSchema).WithRaisePolicy(handoff.RaisePolicy)
	root, err := workspace.HandedCheckoutRoot(handoff.CheckoutRoot)
	if err != nil {
		return workspace.Info{}, environ.Env{}, err
	}
	ws, err := workspace.Handed(handedEnv, loc, root, handoff.SyncRemote, config.HandedProject(handoff.Project))
	return ws, handedEnv, err
}

// workerSpecOf is the detached worker a spawn starts, everything but the holds
// it inherits and the log it writes: its directory, its command line, its
// environment and its handoff's bytes. It reads nothing and starts nothing, so
// what a spawn starts is decided here and tested without a repository or a
// lock; the spawner takes the holds, opens the log and starts it.
// [LAW:effects-at-boundaries]
func workerSpecOf(handoff workerHandoff, storageDir, subcommand string, env environ.Env, from spawner) (detachedWorker, error) {
	payload, err := json.Marshal(handoff)
	if err != nil {
		return detachedWorker{}, fmt.Errorf("encode the worker's handoff: %w", err)
	}
	return detachedWorker{
		dir:     storageDir,
		args:    []string{subcommand, "--store", storageDir, "--parent-pid", from.flag()},
		env:     env,
		handoff: payload,
	}, nil
}
