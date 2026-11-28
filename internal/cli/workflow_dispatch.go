package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// loadedWorkflows is the workflow Set one command fires its occasions
// against, loaded once for the whole command — a bulk run fires every item's
// occasion against the one load — with where its firings trace and complain.
type loadedWorkflows struct {
	set    workflows.Set
	ws     workspace.Info
	stderr io.Writer
}

// loadWorkflows loads the Set a command fires against and reports what the
// load warned: each warning on stderr, one line apiece, and all of them on
// the command's event as workflows.warnings, [] when there were none — so a
// definition that stopped firing is announced by the command it no longer
// fires on, not only by `lit workflows`, and "nothing is bound here" reads
// apart from "what was bound here could not be read".
// [LAW:no-silent-failure] stderr, not stdout: stdout is the agent-facing
// stream, where a config diagnostic would read as guidance.
// [LAW:one-source-of-truth] Warning.String is the one rendering, and each line
// is rendered once for both sinks.
func loadWorkflows(ctx context.Context, inv Invocation, ws workspace.Info) loadedWorkflows {
	set := workflows.Load(inv.Env, ws.RootDir)
	warnings := set.WarningsByLayer()
	lines := make([]string, len(warnings))
	for i, warning := range warnings {
		lines[i] = warning.String()
		fmt.Fprintf(inv.Stderr, "lit: workflow %s\n", lines[i])
	}
	telemetry.Set(ctx, "workflows.warnings", lines)
	return loadedWorkflows{set: set, ws: ws, stderr: inv.Stderr}
}

// fire is the guidance the occasion fires, with ctx's event recording the
// occasion's moments, which definitions fired on it (an empty list when none
// did), and why the firing trace was not recorded (null when it was). A trace
// that could not be written is also a line on stderr, beside the command's
// other diagnostics. [LAW:nothing-unseen] [LAW:single-enforcer] every firing
// goes through here, so no command can fire guidance its event does not name,
// or drop a trace its event does not report.
func (l loadedWorkflows) fire(ctx context.Context, o workflows.Occasion) string {
	firing := workflows.Fire(l.set, l.ws, o)
	ids := make([]string, len(firing.Fired))
	for i, def := range firing.Fired {
		ids[i] = def.ID
	}
	if firing.Untraced != nil {
		fmt.Fprintf(l.stderr, "lit: workflow firing trace could not be recorded (%v)\n", firing.Untraced)
	}
	telemetry.Set(ctx, "workflows.when", workflows.Spellings(o.Moments))
	telemetry.Set(ctx, "workflows.fired", ids)
	telemetry.Set(ctx, "workflows.trace_error", telemetry.ErrorText(firing.Untraced))
	return firing.Guidance
}

// dispatchOccasion is a command that fires one occasion: it loads the
// workflows, fires the occasion, and prints the guidance on stdout.
func dispatchOccasion(ctx context.Context, inv Invocation, ws workspace.Info, o workflows.Occasion) error {
	_, err := io.WriteString(inv.Stdout, loadWorkflows(ctx, inv, ws).fire(ctx, o))
	return err
}
