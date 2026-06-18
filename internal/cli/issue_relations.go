package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/app"
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
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return UsageError{Message: "usage: lit label add <issue-id> <label>"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit label add <issue-id> <label>"}
	}
	labels, err := ap.Store.AddLabel(ctx, store.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: *by})
	if err != nil {
		return err
	}
	if err := printLabels(stdout, labels); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

func runLabelRm(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("label rm")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return UsageError{Message: "usage: lit label rm <issue-id> <label>"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit label rm <issue-id> <label>"}
	}
	labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1])
	if err != nil {
		return err
	}
	if err := printLabels(stdout, labels); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

func runParentSet(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("parent set")
	by := fs.String("by", os.Getenv("USER"), "")
	fs.Hide("by")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return UsageError{Message: "usage: lit parent set <child-id> <parent-id>"}
	}
	rel, err := ap.Store.SetParent(ctx, store.SetParentInput{
		ChildID:   positional[0],
		ParentID:  positional[1],
		CreatedBy: *by,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s --parent-child--> %s\n", rel.SrcID, rel.DstID); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

func runParentClear(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("parent clear")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit parent clear <child-id>"}
	}
	if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "ok"); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("children")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit children <parent-id>"}
	}
	children, err := ap.Store.ListChildren(ctx, positional[0])
	if err != nil {
		return err
	}
	return printIssueLines(stdout, children, []string{"id", "state", "title"})
}
