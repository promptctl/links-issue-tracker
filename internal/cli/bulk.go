package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

var bulkFamily = commandFamily[appSubcommand]{
	usage: "usage: lit bulk <label|close|archive|import> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "label", payload: appSubcommand{access: app.AccessWrite, run: runBulkLabel}},
		{name: "close", payload: appSubcommand{access: app.AccessWrite, run: runBulkTransition(model.ActionClose)}},
		{name: "archive", payload: appSubcommand{access: app.AccessWrite, run: runBulkTransition(model.ActionArchive)}},
		{name: "import", payload: appSubcommand{access: app.AccessWrite, run: runBulkImport}},
	},
}

// bulkLabelOp is the per-issue mutation a bulk label action applies.
type bulkLabelOp func(ctx context.Context, ap *app.App, issueID, label, by string) error

var bulkLabelFamily = commandFamily[bulkLabelOp]{
	usage: "usage: lit bulk label <add|rm> ...",
	subcommands: []subcommandRow[bulkLabelOp]{
		{name: "add", payload: func(ctx context.Context, ap *app.App, issueID, label, by string) error {
			_, err := ap.Store.AddLabel(ctx, store.AddLabelInput{
				IssueID:   issueID,
				Name:      label,
				CreatedBy: by,
			})
			return err
		}},
		{name: "rm", payload: func(ctx context.Context, ap *app.App, issueID, label, _ string) error {
			_, err := ap.Store.RemoveLabel(ctx, issueID, label)
			return err
		}},
	},
}

func runBulkLabel(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New(bulkLabelFamily.usage)
	}
	fs := newCobraFlagSet("bulk label")
	ids := fs.String("ids", "", "Comma-separated issue IDs")
	label := fs.String("label", "", "Label name")
	by := fs.String("by", os.Getenv("USER"), "")
	fs.Hide("by")
	fs.JSONFlag()
	if err := parseFlagSet(fs, args[1:], stdout); err != nil {
		return err
	}
	issueIDs := splitCSV(*ids)
	if len(issueIDs) == 0 {
		return ValidationError{Message: "--ids is required"}
	}
	if strings.TrimSpace(*label) == "" {
		return ValidationError{Message: "--label is required"}
	}
	// Resolved after the flag checks to preserve the established error
	// precedence: missing --ids/--label surface before an unknown action does.
	op, err := bulkLabelFamily.resolve(args)
	if err != nil {
		return err
	}
	results := map[string]string{}
	for _, issueID := range issueIDs {
		if err := op(ctx, ap, issueID, *label, *by); err != nil {
			results[issueID] = err.Error()
			continue
		}
		results[issueID] = "ok"
	}
	return printValue(stdout, results, func(w io.Writer, v any) error {
		entries := v.(map[string]string)
		for issueID, status := range entries {
			if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
				return err
			}
		}
		return nil
	})
}

// runBulkTransition builds the handler for a bulk lifecycle action. The
// action is fixed by the family row, so the body never re-reads argv to
// learn which subcommand it is serving. [LAW:dataflow-not-control-flow]
func runBulkTransition(action model.ActionName) appRunFn {
	return func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		fs := newCobraFlagSet("bulk transition")
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		reason := fs.String("reason", "", "Lifecycle reason")
		by := fs.String("by", os.Getenv("USER"), "")
		fs.Hide("by")
		fs.JSONFlag()
		if err := parseFlagSet(fs, args, stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return ValidationError{Message: "--ids is required"}
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			_, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
				IssueID:   issueID,
				Action:    action,
				Reason:    *reason,
				CreatedBy: *by,
			})
			if err != nil {
				results[issueID] = err.Error()
				continue
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

func runBulkImport(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("bulk import")
	path := fs.String("path", "", "Path to JSON export")
	force := fs.Bool("force", false, "Force import over unsynced local state")
	fs.JSONFlag()
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if strings.TrimSpace(*path) == "" {
		return ValidationError{Message: "--path is required"}
	}
	if err := restoreFromExportPath(ctx, ap, *path, *force); err != nil {
		return err
	}
	payload := map[string]string{"status": "imported", "path": filepath.Clean(*path)}
	return printValue(stdout, payload, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
		return err
	})
}
