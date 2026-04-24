package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func validateLabelCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit label <add|rm> ...", "add", "rm")
}

func validateParentCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit parent <set|clear> ...", "set", "clear")
}

func runLabel(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit label <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("label add")
		by := fs.String("by", os.Getenv("USER"), "Label author")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		labels, err := ap.Store.AddLabel(ctx, store.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: *by})
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("label rm")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1])
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	default:
		return errors.New("usage: lit label <add|rm> ...")
	}
}

func runParent(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit parent <set|clear> ...")
	}
	switch args[0] {
	case "set":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("parent set")
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit parent set <child-id> <parent-id> [--by <user>] [--json]")
		}
		rel, err := ap.Store.SetParent(ctx, store.SetParentInput{
			ChildID:   positional[0],
			ParentID:  positional[1],
			CreatedBy: *by,
		})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			relation := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --parent-child--> %s\n", relation.SrcID, relation.DstID)
			return err
		})
	case "clear":
		positional, flagArgs := splitArgs(args[1:], 1)
		fs := newCobraFlagSet("parent clear")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("usage: lit parent clear <child-id> [--json]")
		}
		if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	default:
		return errors.New("usage: lit parent <set|clear> ...")
	}
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("children")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit children <parent-id> [--json]")
	}
	children, err := ap.Store.ListChildren(ctx, positional[0])
	if err != nil {
		return err
	}
	return printValue(stdout, children, *jsonOut, func(w io.Writer, v any) error {
		return printIssueLines(w, v.([]model.Issue), []string{"id", "state", "title"})
	})
}
