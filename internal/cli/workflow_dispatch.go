package cli

import (
	"context"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// dispatchOccasion is the one way a command fires a workflow occasion:
// workflows.Dispatch injects whatever fires into stdout, and the command's
// event records the occasion and which definitions fired on it, an empty list
// when none did. [LAW:nothing-unseen] [LAW:single-enforcer] every dispatching
// command routes here, so none can fire guidance its event does not name.
func dispatchOccasion(ctx context.Context, stdout io.Writer, ws workspace.Info, o workflows.Occasion) error {
	fired, err := workflows.Dispatch(inv.Env, stdout, os.Stderr, ws, o)
	ids := make([]string, len(fired))
	for i, def := range fired {
		ids[i] = def.ID
	}
	telemetry.Set(ctx, "workflows.event", string(o.Event))
	telemetry.Set(ctx, "workflows.entered", append([]string{}, o.Entered...))
	telemetry.Set(ctx, "workflows.exited", append([]string{}, o.Exited...))
	telemetry.Set(ctx, "workflows.fired", ids)
	return err
}
