package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// bulkEditGuidance closes the two usage refusals an agent earns by guessing at
// editing many issues — `lit bulk update` and `lit update <id> <id> ...` — with
// the command that does it. [LAW:one-source-of-truth]
const bulkEditGuidance = "(to edit many issues' fields: `lit import --path <file.yaml>`)"

var bulkFamily = commandFamily[appSubcommand]{
	usage: "usage: lit bulk <label|comment|start|done|close|open|archive|unarchive|delete|restore> ... " + bulkEditGuidance,
	subcommands: []subcommandRow[appSubcommand]{
		{name: "label", nestedUsage: bulkLabelFamily.usage, payload: appSubcommand{access: app.AccessWrite, declare: bulkLabelLeaf}},
		{name: "comment", payload: appSubcommand{access: app.AccessWrite, declare: bulkCommentLeaf}},
		{name: "start", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(startSpec)}},
		{name: "done", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(doneSpec)}},
		{name: "close", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(closeSpec)}},
		{name: "open", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(openSpec)}},
		{name: "archive", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(archiveSpec)}},
		{name: "unarchive", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(unarchiveSpec)}},
		{name: "delete", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(deleteSpec)}},
		{name: "restore", payload: appSubcommand{access: app.AccessWrite, declare: bulkTransitionLeaf(restoreSpec)}},
		// `bulk import` is retired: it would be a second name for the
		// export-restore `backup restore` owns, and it is the odd verb out in a
		// family of per-`--ids` fan-out ops. Kept dispatchable so an old
		// invocation returns the documented pointer instead of the bare family
		// usage error. [LAW:no-silent-failure]
		retiredSubcommand[appSubcommand]("bulk", "import", bulkImportRetirementGuidance),
	},
}

// bulkLabelOp is the per-issue mutation a bulk label action applies. The actor
// is the resolved acting identity for the invocation, not a raw flag value.
type bulkLabelOp func(ctx context.Context, ap *app.App, issueID, label, actor string) error

// unlandedItem pairs a bulk-operation target with the error its change did not
// land on: a refusal, a fault, or a state that already holds.
type unlandedItem struct {
	IssueID string
	Err     error
}

// BulkError is a bulk operation in which at least one item did not land: the
// items that did not, each with its error, out of the Ran it was handed. Its
// presence is the machine-checkable signal, so a script chaining off a bulk
// command does not proceed as if every item moved. The unlanded items are
// carried as data, never written to the stdout result channel.
// [LAW:no-silent-failure] [LAW:types-are-the-program]
//
// No Unwrap, deliberately: errors.As over a tree of items would classify the
// run by whichever item it met first. The classifier reads the items itself
// and derives the run's reason from all of them (bulkReason).
type BulkError struct {
	Ran      int
	Unlanded []unlandedItem
}

// Error names each unlanded item with the reason its error classifies as, the
// reason its single command would have printed, so a run whose items differ
// still says what each one calls for.
func (e BulkError) Error() string {
	parts := make([]string, len(e.Unlanded))
	for i, item := range e.Unlanded {
		parts[i] = fmt.Sprintf("%s (%s): %v", item.IssueID, commandErrorReason(item.Err), item.Err)
	}
	return fmt.Sprintf("bulk operation: %d of %d item(s) did not land: %s", len(e.Unlanded), e.Ran, strings.Join(parts, "; "))
}

// runBulkOver applies op to each issue ID in order, writing to stdout, per
// landed item, the notice op returned for it and then one "id ok" line — the
// data channel carries results only — and collecting every item that did not
// land. If any did not, it returns a BulkError, whose reason the classifier
// derives from those items; they never reach stdout. This is the single place
// a bulk item's landing is decided, so the exit-code contract cannot drift
// between bulk verbs. [LAW:single-enforcer] [LAW:no-silent-failure]
//
// Each item runs in its own telemetry occurrence and leaves one bulk.items
// entry: the id, its error and that error's reason, and the facts and counts
// its work recorded, with those its error declared about itself, as a failed
// single command's join its event. Run on the event itself, the second item's
// facts would replace the first's. [LAW:nothing-unseen]
func runBulkOver(ctx context.Context, stdout io.Writer, issueIDs []string, op func(ctx context.Context, issueID string) (notice string, err error)) error {
	var unlanded []unlandedItem
	for _, issueID := range issueIDs {
		itemCtx, closeScope := telemetry.Occurrence(ctx)
		notice, err := op(itemCtx, issueID)
		errFacts, errCounts := model.ErrorRecord(err)
		telemetry.Record(itemCtx, errFacts, errCounts)
		facts, counts := closeScope()
		telemetry.Append(ctx, "bulk.items", map[string]any{
			"bulk.item.id":     issueID,
			"bulk.item.error":  telemetry.ErrorText(err),
			"bulk.item.reason": itemReason(err),
			"bulk.item.facts":  facts,
			"bulk.item.counts": counts,
		})
		if err != nil {
			unlanded = append(unlanded, unlandedItem{IssueID: issueID, Err: err})
			continue
		}
		if _, err := io.WriteString(stdout, notice+issueID+" ok\n"); err != nil {
			return err
		}
	}
	if len(unlanded) > 0 {
		return BulkError{Ran: len(issueIDs), Unlanded: unlanded}
	}
	return nil
}

// itemReason is the reason token an item's error classifies as, or nil for an
// item that landed, as telemetry.ErrorText is nil for its error.
func itemReason(err error) *string {
	if err == nil {
		return nil
	}
	reason := commandErrorReason(err).String()
	return &reason
}

var bulkLabelFamily = commandFamily[bulkLabelOp]{
	usage: "usage: lit bulk label <add|rm> ...",
	subcommands: []subcommandRow[bulkLabelOp]{
		{name: "add", payload: func(ctx context.Context, ap *app.App, issueID, label, actor string) error {
			_, err := ap.Store.AddLabel(ctx, storage.AddLabelInput{
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

// registerBulkIDs declares the --ids flag every bulk command acts on and
// returns its parser. A StringArray, because a bulk command acts on the whole
// set it was handed: `bulk close --ids A --ids B` closes both, where a
// single-value flag would close B and skip A without a word.
// [LAW:no-silent-failure] [LAW:single-enforcer] one declaration, so the bulk
// commands cannot disagree about which ids they were given.
func registerBulkIDs(fs *cobraFlagSet) func() ([]string, error) {
	ids := fs.StringArray("ids", "Issue IDs, comma-separated or repeated")
	return func() ([]string, error) {
		issueIDs, err := parseIDSet(*ids)
		if err != nil {
			return nil, err
		}
		if len(issueIDs) == 0 {
			return nil, model.ValidationError{Message: "--ids is required"}
		}
		return issueIDs, nil
	}
}

// bulkLabelLeaf serves the whole `bulk label <add|rm>` surface: the action name
// is this leaf's one positional, so the nested family resolve reads it from the
// parse rather than re-slicing raw argv.
func bulkLabelLeaf() appLeaf {
	fs := newCobraFlagSet("bulk label")
	issueIDsFlag := registerBulkIDs(fs)
	label := fs.String("label", "", "Label name")
	resolveActor := registerActor(fs)
	// No usage: here, deliberately. Wiring the family's sentence loses --ids and
	// --label -- the act that actually works -- and measurement said so: the
	// derived sentence names both, and "takes 1 positional argument" is exactly
	// true of a command whose one positional is the action. [LAW:one-source-of-truth]
	return appLeaf{fs: fs, positionals: 1, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) == 0 {
			return UsageError{Message: bulkLabelFamily.usage}
		}
		issueIDs, err := issueIDsFlag()
		if err != nil {
			return err
		}
		if strings.TrimSpace(*label) == "" {
			return model.ValidationError{Message: "--label is required"}
		}
		// Resolved after the flag checks to preserve the established error
		// precedence: missing --ids/--label surface before an unknown action does.
		// A help-shaped action never reaches here — the outer bulkFamily resolve
		// answers `bulk label --help` before the app is even opened (links-cli-zc3r).
		op, err := bulkLabelFamily.resolve(ctx, positional)
		if err != nil {
			return err
		}
		actor := resolveActor(inv.Env)
		return runBulkOver(ctx, inv.Stdout, issueIDs, func(ctx context.Context, issueID string) (string, error) {
			return "", op(ctx, ap, issueID, *label, actor)
		})
	}}
}

// bulkCommentLeaf is `lit comment add` in bulk form: one body, read and parsed
// once before any issue is touched, added to every listed issue through the
// step the single verb runs, beside the comments each issue already has. A
// landed item prints the single verb's `<id> <comment-id>` line.
// [LAW:single-enforcer] [LAW:one-type-per-behavior] [LAW:parse-dont-validate]
func bulkCommentLeaf() appLeaf {
	fs := newCobraFlagSet("bulk comment")
	issueIDsFlag := registerBulkIDs(fs)
	body := registerLongText(fs, "body", "Comment body")
	resolveActor := registerActor(fs)
	const usage = "usage: lit bulk comment --ids <id,id,...> (--body <text> | --body-file <path|->)"
	return appLeaf{fs: fs, positionals: 0, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		issueIDs, err := issueIDsFlag()
		if err != nil {
			return err
		}
		commentBody, err := model.ParseCommentBody(body.value())
		if err != nil {
			return err
		}
		actor := resolveActor(inv.Env)
		return runBulkOver(ctx, inv.Stdout, issueIDs, func(ctx context.Context, issueID string) (string, error) {
			comment, _, err := addComment(ctx, ap, issueID, commentBody, actor)
			return commentLine(comment), err
		})
	}}
}

// bulkTransitionLeaf is a transition verb in bulk form. It is built from the
// verb's own spec, so it takes that verb's flags (start's --take, close's
// --resolution and --of) and runs that verb's authorize hook, and every listed
// issue goes through applyTransition, the step the single verb runs. A
// transition the single verb refuses is that item's error here, in the same
// words. [LAW:single-enforcer] [LAW:one-type-per-behavior]
func bulkTransitionLeaf(spec transitionSpec) appLeafFn {
	return func() appLeaf {
		fs := newCobraFlagSet("bulk " + spec.name)
		issueIDsFlag := registerBulkIDs(fs)
		reason := fs.String("reason", "", "Lifecycle reason")
		resolveActor := registerActor(fs)
		usage := fmt.Sprintf("usage: lit bulk %s --ids <id,id,...>%s [--reason <text>]", spec.name, spec.synopsis)
		buildAction := spec.registerFlags(fs, usage)
		authorize := spec.authorize(fs)
		return appLeaf{fs: fs, positionals: 0, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
			issueIDs, err := issueIDsFlag()
			if err != nil {
				return err
			}
			actor := resolveActor(inv.Env)
			action, err := buildAction(actor)
			if err != nil {
				return err
			}
			return runBulkOver(ctx, inv.Stdout, issueIDs, func(ctx context.Context, issueID string) (string, error) {
				did, err := applyTransition(ctx, inv, ap, issueID, action, actor, *reason, authorize)
				return did.notice, err
			})
		}}
	}
}
