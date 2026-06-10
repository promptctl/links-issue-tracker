package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
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
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("dep add")
	relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
	blocker := fs.String("blocker", "", "Issue that blocks (only with --type blocks)")
	blocked := fs.String("blocked", "", "Issue that is blocked (only with --type blocks)")
	by := fs.String("by", os.Getenv("USER"), "")
	fs.Hide("by")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	// [LAW:single-enforcer] The CLI flag is the trust boundary; everything
	// downstream receives the sealed RelationType.
	rt, err := model.ParseRelationType(*relType)
	if err != nil {
		return err
	}
	fromID, toID, err := resolveDepAddEndpoints(positional, rt, *blocker, *blocked, fs.NArg())
	if err != nil {
		return err
	}
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
	rel, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: srcID, DstID: dstID, Type: rt, CreatedBy: *by})
	if err != nil {
		return err
	}
	cliRel := depRelationForCLI(rel)
	return printValue(stdout, cliRel, *jsonOut, withQuickstartBreadcrumb("update", func(w io.Writer, v any) error {
		r := v.(model.Relation)
		_, err := fmt.Fprintln(w, depRelationLine(r))
		return err
	}))
}

func runDepRm(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("dep rm")
	relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to (blocks uses <blocker-id> <blocked-id>)")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: lit dep rm <from-id> <to-id> [--type ...]")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit dep rm <from-id> <to-id> [--type ...]")
	}
	rt, err := model.ParseRelationType(*relType)
	if err != nil {
		return err
	}
	srcID, dstID := rt.StoreEndpoints(positional[0], positional[1])
	if err := ap.Store.RemoveRelation(ctx, srcID, dstID, rt); err != nil {
		return err
	}
	return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, withQuickstartBreadcrumb("update", func(w io.Writer, _ any) error {
		_, err := fmt.Fprintln(w, "ok")
		return err
	}))
}

func runDepLs(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("dep ls")
	relType := fs.String("type", "", "Filter relation type")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
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
	return printValue(stdout, cliRelations, *jsonOut, func(w io.Writer, v any) error {
		list := v.([]model.Relation)
		for _, rel := range list {
			if _, err := fmt.Fprintln(w, depRelationLine(rel)); err != nil {
				return err
			}
		}
		return nil
	})
}

// resolveDepAddEndpoints chooses between positional and named-flag input for
// 'lit dep add'. Named flags (--blocker/--blocked) only apply to --type blocks.
// Mixing positional and named flags is an error: the user would have to know
// the orientation rule to mix them safely, which defeats the purpose.
// [LAW:single-enforcer] One place decides which input form was used.
func resolveDepAddEndpoints(positional []string, relType model.RelationType, blocker, blocked string, extraArgs int) (string, string, error) {
	usage := "usage: lit dep add <from-id> <to-id> [--type blocks|parent-child|related-to]\n  or:  lit dep add --blocker <id> --blocked <id> (only with --type blocks)"
	hasNamed := blocker != "" || blocked != ""
	if hasNamed {
		if relType != model.RelBlocks {
			return "", "", fmt.Errorf("--blocker/--blocked only apply with --type blocks; got --type %s", relType)
		}
		if blocker == "" || blocked == "" {
			return "", "", errors.New("--blocker and --blocked must both be provided")
		}
		if len(positional) > 0 || extraArgs > 0 {
			return "", "", errors.New("provide either positional <from> <to> or --blocker/--blocked, not both")
		}
		// "from" in CLI convention = blocker; "to" = blocked.
		return blocker, blocked, nil
	}
	if len(positional) != 2 || extraArgs != 0 {
		return "", "", errors.New(usage)
	}
	return positional[0], positional[1], nil
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
		return errors.New(sameEpicBlocksRejectionMessage)
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
