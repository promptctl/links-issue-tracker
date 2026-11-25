package cli

import (
	"context"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// dispatchOccasion is the one way a command fires a workflow occasion:
// workflows.Dispatch injects whatever fires into stdout and reports the load's
// warnings on stderr, and the command's event records the occasion's moments,
// which definitions fired on it and what the load warned, each an empty list
// when there were none — so "nothing is bound here" and "what was bound here
// could not be read" are told apart from the event alone.
// [LAW:nothing-unseen] [LAW:single-enforcer] every dispatching command routes
// here, so none can fire guidance its event does not name.
func dispatchOccasion(ctx context.Context, stderr io.Writer, env environ.Env, stdout io.Writer, ws workspace.Info, o workflows.Occasion) error {
	fired, warnings, err := workflows.Dispatch(env, stdout, stderr, ws, o)
	ids := make([]string, len(fired))
	for i, def := range fired {
		ids[i] = def.ID
	}
	lines := make([]string, len(warnings))
	for i, warning := range warnings {
		lines[i] = warning.String()
	}
	telemetry.Set(ctx, "workflows.when", workflows.Spellings(o.Moments))
	telemetry.Set(ctx, "workflows.fired", ids)
	telemetry.Set(ctx, "workflows.warnings", lines)
	return err
}
