package cli

import (
	"context"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runAppFamily drives an app-mode family the way familyCmd does, minus the
// app open: the table picks the row, the row's handler runs on the remaining
// arguments. Tests dispatch through the production table so they exercise the
// absorbed routing behavior, not a private path. [LAW:behavior-not-structure]
func runAppFamily(f commandFamily[appSubcommand], ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	sub, err := f.resolve(args)
	if err != nil {
		return err
	}
	return sub.run(ctx, stdout, ap, args[1:])
}

// runWsFamily is runAppFamily for workspace-mode families.
func runWsFamily(f commandFamily[wsRunFn], ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	run, err := f.resolve(args)
	if err != nil {
		return err
	}
	return run(ctx, stdout, ws, args[1:])
}
