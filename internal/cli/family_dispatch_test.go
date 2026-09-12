package cli

import (
	"context"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runAppFamily drives an app-mode family the way familyCmd does, minus the
// app open: the table picks the row, the row's leaf declares and parses, and
// its work runs on the resource the test supplies. Tests dispatch through the
// production table so they exercise the absorbed routing behavior, not a
// private path. [LAW:behavior-not-structure]
func runAppFamily(f commandFamily[appSubcommand], ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	sub, err := f.resolve(args)
	if err != nil {
		return err
	}
	if sub.retired != nil {
		return *sub.retired
	}
	return runLeaf(sub.declare(), ctx, stdout, ap, args[1:])
}

// runWsFamily is runAppFamily for workspace-mode families, walking nested
// families to the same depth wsFamilyCmd does.
func runWsFamily(f commandFamily[wsSubcommand], ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	l, rest, err := resolveWsLeaf(f, args)
	if err != nil {
		return err
	}
	return runLeaf(l, ctx, stdout, ws, rest)
}
