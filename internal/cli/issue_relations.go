package cli

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

var labelFamily = commandFamily[appSubcommand]{
	usage: "usage: lit label <add|rm> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWrite, declare: labelAddLeaf}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, declare: labelRmLeaf}},
	},
}

var parentFamily = commandFamily[appSubcommand]{
	usage: "usage: lit parent <set|clear> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "set", payload: appSubcommand{access: app.AccessWrite, declare: parentSetLeaf}},
		{name: "clear", payload: appSubcommand{access: app.AccessWrite, declare: parentClearLeaf}},
	},
}

func labelAddLeaf() appLeaf {
	fs := newCobraFlagSet("label add")
	resolveActor := registerActor(fs)
	const usageLine0 = "usage: lit label add <issue-id> <label>"
	return appLeaf{fs: fs, usage: usageLine0, positionals: 2, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 2 {
			return UsageError{Message: usageLine0}
		}
		issue, err := applyLabelEdit(ctx, ap, positional[0], storage.AddLabel{Name: positional[1], Actor: resolveActor(inv.Env)})
		if err != nil {
			return err
		}
		if err := printLabels(inv.Stdout, issue.Labels); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

func labelRmLeaf() appLeaf {
	fs := newCobraFlagSet("label rm")
	resolveActor := registerActor(fs)
	const usageLine1 = "usage: lit label rm <issue-id> <label>"
	return appLeaf{fs: fs, usage: usageLine1, positionals: 2, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 2 {
			return UsageError{Message: usageLine1}
		}
		issue, err := applyLabelEdit(ctx, ap, positional[0], storage.RemoveLabel{Name: positional[1], Actor: resolveActor(inv.Env)})
		if err != nil {
			return err
		}
		if err := printLabels(inv.Stdout, issue.Labels); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

// applyLabelEdit is every label verb's write, single and bulk alike: it
// applies the verb's plan and records on the command's event whether the
// labels field moved. A label added to an issue that already holds it lands
// and records no history row, and label.changed is what tells that run from
// an edit. [LAW:single-enforcer] [LAW:nothing-unseen]
func applyLabelEdit(ctx context.Context, ap *app.App, issueID string, plan storage.ChangePlan) (model.Issue, error) {
	applied, err := ap.Store.Apply(ctx, issueID, plan)
	if err != nil {
		return model.Issue{}, err
	}
	telemetry.Set(ctx, "label.changed", len(applied.Fields) > 0)
	return applied.Issue, nil
}

func parentSetLeaf() appLeaf {
	fs := newCobraFlagSet("parent set")
	child := fs.String("child", "", "Child issue ID (required)")
	parent := fs.String("parent", "", "Parent issue ID (required)")
	resolveActor := registerActor(fs)
	const usageLine2 = "usage: lit parent set --child <id> --parent <id>"
	return appLeaf{fs: fs, usage: usageLine2, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if *child == "" || *parent == "" {
			return UsageError{Message: usageLine2}
		}
		rel, err := ap.Store.SetParent(ctx, storage.SetParentInput{
			ChildID:   *child,
			ParentID:  *parent,
			CreatedBy: resolveActor(inv.Env),
		})
		if err != nil {
			return err
		}
		// [LAW:one-source-of-truth] One edge reads one way whichever command
		// prints it: `parent set` renders through the same sentence as `dep ls`.
		if _, err := fmt.Fprintln(inv.Stdout, depRelationLine(rel)); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

func parentClearLeaf() appLeaf {
	fs := newCobraFlagSet("parent clear")
	const usageLine3 = "usage: lit parent clear <child-id>"
	return appLeaf{fs: fs, usage: usageLine3, positionals: 1, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usageLine3}
		}
		if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(inv.Stdout, "ok"); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}
