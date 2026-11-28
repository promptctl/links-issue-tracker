package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// configFamily is `lit config`: putting the checkout's committed project
// config in force on the store, and how the file stands against the config in
// force. Every clone runs on the config applied to the store, so an edit to
// the file has no effect until it is committed and applied.
var configFamily = commandFamily[appSubcommand]{
	usage: "usage: lit config <apply|status>",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "apply", payload: appSubcommand{access: app.AccessWriteRoutine, declare: configApplyLeaf}},
		{name: "status", payload: appSubcommand{access: app.AccessRead, declare: configStatusLeaf}},
	},
}

// configApplyLeaf is `lit config apply`: the committed project config file
// becomes the config in force, for every clone once it syncs.
func configApplyLeaf() appLeaf {
	fs := newCobraFlagSet("config apply")
	resolveActor := registerActor(fs)
	return appLeaf{fs: fs, positionals: 0, usage: "usage: lit config apply", work: func(ctx context.Context, inv Invocation, ap *app.App, _ []string) error {
		file, err := readProjectFile(ctx, inv.Env, ap.Workspace)
		if err != nil {
			return err
		}
		content, err := file.Applicable()
		if err != nil {
			return err
		}
		apply, wrote, err := ap.Store.ApplyConfig(ctx, content, resolveActor(inv.Env))
		if err != nil {
			return err
		}
		telemetry.Set(ctx, "config.applied", wrote)
		telemetry.Set(ctx, "config.digest", string(apply.Digest()))
		if !wrote {
			_, err = fmt.Fprintf(inv.Stdout, "%s is already the config in force (%s); nothing applied\n", file.Path, apply.Digest().Short())
			return err
		}
		_, err = fmt.Fprintf(inv.Stdout, "applied %s (%s): it is the config in force, and every clone runs on it once it syncs\n", file.Path, apply.Digest().Short())
		return err
	}}
}

// configStatusLeaf is `lit config status`: the config in force, and how the
// checkout's file stands against it.
func configStatusLeaf() appLeaf {
	fs := newCobraFlagSet("config status")
	return appLeaf{fs: fs, positionals: 0, usage: "usage: lit config status", work: func(ctx context.Context, inv Invocation, ap *app.App, _ []string) error {
		file, err := readProjectFile(ctx, inv.Env, ap.Workspace)
		if err != nil {
			return err
		}
		applies, err := ap.Store.ConfigApplies(ctx)
		if err != nil {
			return err
		}
		standing := config.StandingOf(file, applies)
		telemetry.Set(ctx, "config.standing", string(standing))
		_, err = fmt.Fprintf(inv.Stdout, "in force: %s\n%s: %s\n", inForceLine(applies), file.Path, standingSentence(standing))
		return err
	}}
}

// configStandingNotice is the line a command that serves work prints when the
// checkout's file is not the config in force, so an edit that has no effect is
// not mistaken for one that does. It is empty when the file is in force.
//
// A file whose standing cannot be read is a warning naming why, not a failure:
// the command serves work whatever the file says, so a checkout git cannot
// answer for still lists its queue. [LAW:no-silent-failure]
func configStandingNotice(ctx context.Context, env environ.Env, ap *app.App) (string, error) {
	file, err := readProjectFile(ctx, env, ap.Workspace)
	if err != nil {
		return fmt.Sprintf("warning: could not compare the project config file with the config in force (%v)\n", err), nil
	}
	applies, err := ap.Store.ConfigApplies(ctx)
	if err != nil {
		return "", err
	}
	switch standing := config.StandingOf(file, applies); standing {
	case config.StandingNotApplied, config.StandingUncommitted:
		return fmt.Sprintf("lit: %s is not the config in force: %s (lit config status)\n", file.Path, standingSentence(standing)), nil
	default:
		return "", nil
	}
}

func readProjectFile(ctx context.Context, env environ.Env, ws workspace.Info) (config.ProjectFile, error) {
	return workspace.ReadProjectFile(ctx, env, ws.RootDir, config.CheckoutProjectPath(env, pathspec.New(ws.RootDir)))
}

// inForceLine names the config in force: the newest apply, or the defaults.
func inForceLine(applies []model.ConfigApply) string {
	apply, applied := model.ConfigInForce(applies)
	if !applied {
		return "lit's defaults (no project config has been applied)"
	}
	return fmt.Sprintf("%s, applied by %s at %s", apply.Digest().Short(), apply.CreatedBy, apply.CreatedAt.UTC().Format(time.RFC3339))
}

// standingSentence says what a standing means for the person holding the file.
func standingSentence(standing config.Standing) string {
	switch standing {
	case config.StandingInForce:
		return "matches the config in force"
	case config.StandingNotApplied:
		return "committed but not in force; it has no effect until `lit config apply` puts it in force"
	case config.StandingUncommitted:
		return "has uncommitted changes, which have no effect; commit them, then run `lit config apply`"
	case config.StandingNoFile:
		return "no file; the applied config stays in force, and applying an empty file returns to the defaults"
	}
	panic(fmt.Sprintf("unknown config standing %q", standing))
}

// appliedProject is the config applied to ws's store, for a command that reads
// config and holds no store of its own: it opens the store read-only, the way
// every read command does, and closes it before returning. A workspace whose
// store `lit init` has not created has had nothing applied, so it runs on the
// defaults.
func appliedProject(ctx context.Context, env environ.Env, stderr io.Writer, ws workspace.Info) (config.ProjectLayer, error) {
	project, err := readAppliedProject(ctx, env, stderr, ws)
	if err != nil {
		return nil, err
	}
	reportConfigLayers(ctx, stderr, env, project)
	return project, nil
}

func readAppliedProject(ctx context.Context, env environ.Env, stderr io.Writer, ws workspace.Info) (config.ProjectLayer, error) {
	ap, err := app.Open(ctx, env, stderr, ws, app.AccessRead)
	if errors.Is(err, store.ErrWorkspaceNotInitialized) {
		return config.NoProject(), nil
	}
	if err != nil {
		// An open boundary like any other: holder contention is stamped for
		// Run's dispatch trace, as runWithApp stamps app.Open.
		return nil, markEngineOpenContention(err, ws)
	}
	return ap.Workspace.Project, ap.Close()
}
