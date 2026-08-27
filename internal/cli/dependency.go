package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

var depFamily = commandFamily[appSubcommand]{
	usage: "usage: lit dep <add|rm|ls> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWrite, run: runDepAdd}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, run: runDepRm}},
		{name: "ls", payload: appSubcommand{access: app.AccessRead, run: runDepLs}},
	},
}

func runDepAdd(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("dep add")
	relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
	from := fs.String("from", "", "Source issue ID (required)")
	to := fs.String("to", "", "Target issue ID (required)")
	resolveActor := registerActor(fs)
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if *from == "" || *to == "" || fs.NArg() != 0 {
		return UsageError{Message: "usage: lit dep add --from <id> --to <id> [--type blocks|parent-child|related-to]"}
	}
	// [LAW:single-enforcer] The CLI flag is the trust boundary; everything
	// downstream receives the sealed RelationType.
	rt, err := model.ParseRelationType(*relType)
	if err != nil {
		return err
	}
	fromID, toID := *from, *to
	// Self-loop check: a relation from an issue to itself is meaningless and
	// would otherwise corrupt downstream blocker traversals. Cheap to catch
	// here; transitive cycle detection is a follow-up.
	if fromID == toID {
		return fmt.Errorf("dep add: self-loop rejected (%s -> %s)", fromID, toID)
	}
	// [LAW:single-enforcer] Same-epic blocks are rejected at the CLI policy
	// boundary so the store stays a thin substrate. Within one epic, rank is
	// the canonical ordering; a 'blocks' edge would duplicate that signal.
	if rt == model.RelBlocks {
		if err := rejectSameEpicBlocks(ctx, ap, fromID, toID); err != nil {
			return err
		}
	}
	srcID, dstID := rt.StoreEndpoints(fromID, toID)
	rel, err := ap.Store.AddRelation(ctx, storage.AddRelationInput{SrcID: srcID, DstID: dstID, Type: rt, CreatedBy: resolveActor()})
	if err != nil {
		return err
	}
	cliRel := depRelationForCLI(rel)
	if _, err := fmt.Fprintln(stdout, depRelationLine(cliRel)); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

func runDepRm(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("dep rm")
	relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
	from := fs.String("from", "", "Source issue ID (required)")
	to := fs.String("to", "", "Target issue ID (required)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if *from == "" || *to == "" || fs.NArg() != 0 {
		return UsageError{Message: "usage: lit dep rm --from <id> --to <id> [--type blocks|parent-child|related-to]"}
	}
	rt, err := model.ParseRelationType(*relType)
	if err != nil {
		return err
	}
	srcID, dstID := rt.StoreEndpoints(*from, *to)
	if err := ap.Store.RemoveRelation(ctx, srcID, dstID, rt); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "ok"); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

func runDepLs(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("dep ls")
	relType := fs.String("type", "", "Filter relation type")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to]"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to]"}
	}
	// [LAW:dataflow-not-control-flow] An absent --type is the empty filter
	// set; a present one is parsed at this trust boundary, so a bad value
	// errors loudly instead of silently matching nothing.
	var typeFilter []model.RelationType
	if strings.TrimSpace(*relType) != "" {
		rt, err := model.ParseRelationType(*relType)
		if err != nil {
			return err
		}
		typeFilter = append(typeFilter, rt)
	}
	relations, err := ap.Store.ListRelationsForIssue(ctx, positional[0], typeFilter...)
	if err != nil {
		return err
	}
	cliRelations := make([]model.Relation, 0, len(relations))
	for _, rel := range relations {
		cliRelations = append(cliRelations, depRelationForCLI(rel))
	}
	for _, rel := range cliRelations {
		if _, err := fmt.Fprintln(stdout, depRelationLine(rel)); err != nil {
			return err
		}
	}
	return nil
}

// depRelationForCLI flips a store-oriented relation back into the CLI's human
// order. StoreEndpoints is an involution, so the same mapping serves both
// directions. [LAW:dataflow-not-control-flow] Applied unconditionally;
// the per-type variability lives in the RelationType value.
func depRelationForCLI(rel model.Relation) model.Relation {
	rel.SrcID, rel.DstID = rel.Type.StoreEndpoints(rel.SrcID, rel.DstID)
	return rel
}

// [LAW:one-source-of-truth] The rejection text is part of the user-facing CLI
// contract and is asserted verbatim in tests; both sites read it from here so
// they cannot drift.
const sameEpicBlocksRejectionMessage = "Do not set 'blocks' relationships between two issues in the same epic.  Use rank to specify that one issue must be completed before another issue"

// rejectSameEpicBlocks errors when both endpoints resolve to the same epic
// membership.
func rejectSameEpicBlocks(ctx context.Context, ap *app.App, fromID, toID string) error {
	fromEpic, err := issueEpicID(ctx, ap, fromID)
	if err != nil {
		return err
	}
	toEpic, err := issueEpicID(ctx, ap, toID)
	if err != nil {
		return err
	}
	if fromEpic != "" && fromEpic == toEpic {
		return ValidationError{Message: sameEpicBlocksRejectionMessage}
	}
	return nil
}

// issueEpicID returns the issue's epic membership for the same-epic check:
// its own ID if it is a container (epic), its parent ID if the parent is a
// container, otherwise "" (floating — not a member of any epic).
func issueEpicID(ctx context.Context, ap *app.App, issueID string) (string, error) {
	detail, err := ap.Store.GetIssueDetail(ctx, issueID)
	if err != nil {
		return "", err
	}
	if detail.Issue.IsContainer() {
		return detail.Issue.ID, nil
	}
	if detail.Parent != nil && detail.Parent.IsContainer() {
		return detail.Parent.ID, nil
	}
	return "", nil
}

func depRelationLine(rel model.Relation) string {
	switch rel.Type {
	case model.RelBlocks:
		return fmt.Sprintf("%s --blocks--> %s", rel.SrcID, rel.DstID)
	case model.RelParentChild:
		return fmt.Sprintf("%s --child-of--> %s", rel.SrcID, rel.DstID)
	case model.RelRelatedTo:
		return fmt.Sprintf("%s --related-to--> %s", rel.SrcID, rel.DstID)
	default:
		return fmt.Sprintf("%s --depends-on--> %s", rel.SrcID, rel.DstID)
	}
}
