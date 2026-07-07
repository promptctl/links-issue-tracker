package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		{name: "close", payload: appSubcommand{access: app.AccessWrite, run: runBulkClose}},
		{name: "archive", payload: appSubcommand{access: app.AccessWrite, run: runBulkTransition(model.Archive{})}},
		{name: "import", payload: appSubcommand{access: app.AccessWrite, run: runBulkImport}},
	},
}

// bulkLabelOp is the per-issue mutation a bulk label action applies. The actor
// is the resolved acting identity for the invocation, not a raw flag value.
type bulkLabelOp func(ctx context.Context, ap *app.App, issueID, label, actor string) error

// itemFailure pairs a bulk-operation target with the error that befell it.
type itemFailure struct {
	IssueID string
	Err     error
}

// BulkFailureError aggregates the per-item failures of a bulk operation. Its
// presence is the machine-checkable signal that at least one item failed, so the
// exit-code boundary maps it to a non-zero code and a script chaining off a bulk
// command does not proceed on partial or total failure. The per-item failures
// are carried as data, never written to the stdout result channel.
// [LAW:no-silent-failure] [LAW:types-are-the-program]
type BulkFailureError struct {
	Failures []itemFailure
}

func (e BulkFailureError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = fmt.Sprintf("%s: %v", f.IssueID, f.Err)
	}
	return fmt.Sprintf("bulk operation: %d item(s) failed: %s", len(e.Failures), strings.Join(parts, "; "))
}

// runBulkOver applies op to each issue ID in order, writing one "id ok" line per
// success to stdout — the data channel carries results only — and collecting
// every failure. If any item failed it returns a BulkFailureError so the exit
// code reflects it; per-item failures never reach stdout. This is the single
// place bulk success/failure is decided, so the exit-code contract cannot drift
// between bulk verbs. [LAW:single-enforcer] [LAW:no-silent-failure]
func runBulkOver(stdout io.Writer, issueIDs []string, op func(issueID string) error) error {
	var failures []itemFailure
	for _, issueID := range issueIDs {
		if err := op(issueID); err != nil {
			failures = append(failures, itemFailure{IssueID: issueID, Err: err})
			continue
		}
		if _, err := fmt.Fprintf(stdout, "%s ok\n", issueID); err != nil {
			return err
		}
	}
	if len(failures) > 0 {
		return BulkFailureError{Failures: failures}
	}
	return nil
}

var bulkLabelFamily = commandFamily[bulkLabelOp]{
	usage: "usage: lit bulk label <add|rm> ...",
	subcommands: []subcommandRow[bulkLabelOp]{
		{name: "add", payload: func(ctx context.Context, ap *app.App, issueID, label, actor string) error {
			_, err := ap.Store.AddLabel(ctx, store.AddLabelInput{
				IssueID:   issueID,
				Name:      label,
				CreatedBy: actor,
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
	resolveActor := registerActor(fs)
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
	actor := resolveActor()
	return runBulkOver(stdout, issueIDs, func(issueID string) error {
		return op(ctx, ap, issueID, *label, actor)
	})
}

// runBulkClose closes every listed issue with one shared outcome. The outcome
// flags parse through the same gate `lit close` uses, so a bulk close can no
// longer record the resolution-less close that the close command itself
// forbids — the two boundaries agree about what a close requires by sharing
// the enforcer. [LAW:single-enforcer]
func runBulkClose(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("bulk close")
	ids := fs.String("ids", "", "Comma-separated issue IDs")
	reason := fs.String("reason", "", "Lifecycle reason")
	resolution, target := registerCloseOutcomeFlags(fs)
	resolveActor := registerActor(fs)
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	issueIDs := splitCSV(*ids)
	if len(issueIDs) == 0 {
		return ValidationError{Message: "--ids is required"}
	}
	outcome, err := closeOutcomeFromFlags(*resolution, *target, "usage: lit bulk close --ids <id,id,...> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]")
	if err != nil {
		return err
	}
	actor := resolveActor()
	return runBulkOver(stdout, issueIDs, func(issueID string) error {
		_, err := ap.Store.Apply(ctx, issueID, store.Change{
			Action: model.Close{Outcome: outcome},
			Actor:  actor,
			Reason: *reason,
		})
		return err
	})
}

// runBulkTransition builds the handler for a bulk retention action. The
// action is fixed by the family row, so the body never re-reads argv to
// learn which subcommand it is serving. [LAW:dataflow-not-control-flow]
func runBulkTransition(action model.Action) appRunFn {
	return func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		fs := newCobraFlagSet("bulk " + string(action.Name()))
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		reason := fs.String("reason", "", "Lifecycle reason")
		resolveActor := registerActor(fs)
		if err := parseFlagSet(fs, args, stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return ValidationError{Message: "--ids is required"}
		}
		actor := resolveActor()
		return runBulkOver(stdout, issueIDs, func(issueID string) error {
			_, err := ap.Store.Apply(ctx, issueID, store.Change{Action: action, Actor: actor, Reason: *reason})
			return err
		})
	}
}

func runBulkImport(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("bulk import")
	path := fs.String("path", "", "Path to JSON export")
	force := fs.Bool("force", false, "Force import over unsynced local state")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if strings.TrimSpace(*path) == "" {
		return ValidationError{Message: "--path is required"}
	}
	if err := restoreFromExportPath(ctx, ap, *path, *force); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "imported %s\n", filepath.Clean(*path))
	return err
}
