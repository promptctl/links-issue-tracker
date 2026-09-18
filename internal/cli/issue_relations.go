package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
	return appLeaf{fs: fs, usage: usageLine0, positionals: 2, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 2 {
			return UsageError{Message: usageLine0}
		}
		labels, err := ap.Store.AddLabel(ctx, storage.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: resolveActor()})
		if err != nil {
			return err
		}
		if err := printLabels(stdout, labels); err != nil {
			return err
		}
		return emitBreadcrumb(stdout, "update")
	}}
}

func labelRmLeaf() appLeaf {
	fs := newCobraFlagSet("label rm")
	const usageLine1 = "usage: lit label rm <issue-id> <label>"
	return appLeaf{fs: fs, usage: usageLine1, positionals: 2, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 2 {
			return UsageError{Message: usageLine1}
		}
		labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1])
		if err != nil {
			return err
		}
		if err := printLabels(stdout, labels); err != nil {
			return err
		}
		return emitBreadcrumb(stdout, "update")
	}}
}

func parentSetLeaf() appLeaf {
	fs := newCobraFlagSet("parent set")
	child := fs.String("child", "", "Child issue ID (required)")
	parent := fs.String("parent", "", "Parent issue ID (required)")
	resolveActor := registerActor(fs)
	const usageLine2 = "usage: lit parent set --child <id> --parent <id>"
	return appLeaf{fs: fs, usage: usageLine2, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if *child == "" || *parent == "" {
			return UsageError{Message: usageLine2}
		}
		rel, err := ap.Store.SetParent(ctx, storage.SetParentInput{
			ChildID:   *child,
			ParentID:  *parent,
			CreatedBy: resolveActor(),
		})
		if err != nil {
			return err
		}
		// [LAW:one-source-of-truth] `parent set` and `dep add --type parent-child`
		// write the SAME parent-child edge through the SAME store owner
		// (setSingleValuedEdgeTx); `parent` is the ergonomic face of that one path,
		// not a divergent second writer. The only thing that used to diverge was the
		// *presented* line: this command printed "--parent-child-->" while `dep`
		// prints the identical edge via depRelationForCLI/depRelationLine as
		// "--child-of-->". Rendering through the same canonical projection here means
		// one edge reads one way whichever command created it.
		if _, err := fmt.Fprintln(stdout, depRelationLine(depRelationForCLI(rel))); err != nil {
			return err
		}
		return emitBreadcrumb(stdout, "update")
	}}
}

func parentClearLeaf() appLeaf {
	fs := newCobraFlagSet("parent clear")
	const usageLine3 = "usage: lit parent clear <child-id>"
	return appLeaf{fs: fs, usage: usageLine3, positionals: 1, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usageLine3}
		}
		if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, "ok"); err != nil {
			return err
		}
		return emitBreadcrumb(stdout, "update")
	}}
}
