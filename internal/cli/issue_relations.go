package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

var labelFamily = commandFamily[appSubcommand]{
	usage: "usage: lit label <add|rm> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWrite, run: runLabelAdd}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, run: runLabelRm}},
	},
}

var parentFamily = commandFamily[appSubcommand]{
	usage: "usage: lit parent <set|clear> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "set", payload: appSubcommand{access: app.AccessWrite, run: runParentSet}},
		{name: "clear", payload: appSubcommand{access: app.AccessWrite, run: runParentClear}},
	},
}

func runLabelAdd(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("label add")
	by := fs.String("by", os.Getenv("USER"), "")
	fs.Hide("by")
	fs.JSONFlag()
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return UsageError{Message: "usage: lit label add <issue-id> <label> [--json]"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit label add <issue-id> <label> [--json]"}
	}
	labels, err := ap.Store.AddLabel(ctx, store.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: *by})
	if err != nil {
		return err
	}
	return printValue(stdout, labels, withQuickstartBreadcrumb("update", printLabels))
}

func runLabelRm(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("label rm")
	fs.JSONFlag()
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return UsageError{Message: "usage: lit label rm <issue-id> <label> [--json]"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit label rm <issue-id> <label> [--json]"}
	}
	labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1])
	if err != nil {
		return err
	}
	return printValue(stdout, labels, withQuickstartBreadcrumb("update", printLabels))
}

func runParentSet(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("parent set")
	by := fs.String("by", os.Getenv("USER"), "")
	fs.Hide("by")
	fs.JSONFlag()
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return UsageError{Message: "usage: lit parent set <child-id> <parent-id> [--json]"}
	}
	rel, err := ap.Store.SetParent(ctx, store.SetParentInput{
		ChildID:   positional[0],
		ParentID:  positional[1],
		CreatedBy: *by,
	})
	if err != nil {
		return err
	}
	return printValue(stdout, rel, withQuickstartBreadcrumb("update", func(w io.Writer, v any) error {
		relation := v.(model.Relation)
		_, err := fmt.Fprintf(w, "%s --parent-child--> %s\n", relation.SrcID, relation.DstID)
		return err
	}))
}

func runParentClear(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("parent clear")
	fs.JSONFlag()
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit parent clear <child-id> [--json]"}
	}
	if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
		return err
	}
	return printValue(stdout, map[string]string{"status": "ok"}, withQuickstartBreadcrumb("update", func(w io.Writer, _ any) error {
		_, err := fmt.Fprintln(w, "ok")
		return err
	}))
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("children")
	fs.JSONFlag()
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit children <parent-id> [--json]"}
	}
	children, err := ap.Store.ListChildren(ctx, positional[0])
	if err != nil {
		return err
	}
	return printValue(stdout, children, func(w io.Writer, v any) error {
		return printIssueLines(w, v.([]model.Issue), []string{"id", "state", "title"})
	})
}
