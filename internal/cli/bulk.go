package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func validateBulkCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit bulk <label|close|archive|import> ...", "label", "close", "archive", "import")
}

func runBulk(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
	switch args[0] {
	case "label":
		if len(args) < 2 {
			return errors.New("usage: lit bulk label <add|rm> ...")
		}
		action := args[1]
		fs := newCobraFlagSet("bulk label")
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		label := fs.String("label", "", "Label name")
		by := fs.String("by", os.Getenv("USER"), "Label actor")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[2:], stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*label) == "" {
			return errors.New("--label is required")
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			switch action {
			case "add":
				_, err := ap.Store.AddLabel(ctx, store.AddLabelInput{
					IssueID:   issueID,
					Name:      *label,
					CreatedBy: *by,
				})
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			case "rm":
				_, err := ap.Store.RemoveLabel(ctx, issueID, *label)
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			default:
				return errors.New("usage: lit bulk label <add|rm> ...")
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "close", "archive":
		fs := newCobraFlagSet("bulk transition")
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		reason := fs.String("reason", "", "Lifecycle reason")
		by := fs.String("by", os.Getenv("USER"), "Lifecycle actor")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			_, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
				IssueID:   issueID,
				Action:    args[0],
				Reason:    *reason,
				CreatedBy: *by,
			})
			if err != nil {
				results[issueID] = err.Error()
				continue
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "import":
		fs := newCobraFlagSet("bulk import")
		path := fs.String("path", "", "Path to JSON export")
		force := fs.Bool("force", false, "Force import over unsynced local state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if strings.TrimSpace(*path) == "" {
			return errors.New("--path is required")
		}
		if err := restoreFromExportPath(ctx, ap, *path, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "imported", "path": filepath.Clean(*path)}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
}
