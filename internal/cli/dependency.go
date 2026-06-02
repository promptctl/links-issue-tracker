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

func validateDepCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit dep <add|rm|ls> ...", "add", "rm", "ls")
}

func runDep(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit dep <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
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
		fromID, toID, err := resolveDepAddEndpoints(positional, *relType, *blocker, *blocked, fs.NArg())
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
		if strings.TrimSpace(*relType) == "blocks" {
			if err := rejectSameEpicBlocks(ctx, ap, fromID, toID); err != nil {
				return err
			}
		}
		srcID, dstID := depStoreEndpoints(*relType, fromID, toID)
		rel, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: srcID, DstID: dstID, Type: *relType, CreatedBy: *by})
		if err != nil {
			return err
		}
		cliRel := depRelationForCLI(rel)
		return printValue(stdout, cliRel, *jsonOut, func(w io.Writer, v any) error {
			r := v.(model.Relation)
			_, err := fmt.Fprintln(w, depRelationLine(r))
			return err
		})
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
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
		srcID, dstID := depStoreEndpoints(*relType, positional[0], positional[1])
		if err := ap.Store.RemoveRelation(ctx, srcID, dstID, *relType); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	case "ls":
		positional, flagArgs := splitArgs(args[1:], 1)
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
		relations, err := ap.Store.ListRelationsForIssue(ctx, positional[0], *relType)
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
	default:
		return errors.New("usage: lit dep <add|rm|ls> ...")
	}
}

// resolveDepAddEndpoints chooses between positional and named-flag input for
// 'lit dep add'. Named flags (--blocker/--blocked) only apply to --type blocks.
// Mixing positional and named flags is an error: the user would have to know
// the orientation rule to mix them safely, which defeats the purpose.
// [LAW:single-enforcer] One place decides which input form was used.
func resolveDepAddEndpoints(positional []string, relType, blocker, blocked string, extraArgs int) (string, string, error) {
	usage := "usage: lit dep add <from-id> <to-id> [--type blocks|parent-child|related-to]\n  or:  lit dep add --blocker <id> --blocked <id> (only with --type blocks)"
	hasNamed := blocker != "" || blocked != ""
	if hasNamed {
		if relType != "blocks" {
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

func depStoreEndpoints(relType, fromID, toID string) (string, string) {
	// [LAW:single-enforcer] CLI-to-store orientation normalization for dep commands is centralized in one function.
	// [LAW:one-source-of-truth] Store keeps one canonical blocks encoding (dependent -> dependency); CLI maps from human order.
	if strings.TrimSpace(relType) == "blocks" {
		return toID, fromID
	}
	return fromID, toID
}

func depRelationForCLI(rel model.Relation) model.Relation {
	if strings.TrimSpace(rel.Type) != "blocks" {
		return rel
	}
	rel.SrcID, rel.DstID = rel.DstID, rel.SrcID
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
	switch strings.TrimSpace(rel.Type) {
	case "blocks":
		return fmt.Sprintf("%s --blocks--> %s", rel.SrcID, rel.DstID)
	case "parent-child":
		return fmt.Sprintf("%s --child-of--> %s", rel.SrcID, rel.DstID)
	case "related-to":
		return fmt.Sprintf("%s --related-to--> %s", rel.SrcID, rel.DstID)
	default:
		return fmt.Sprintf("%s --depends-on--> %s", rel.SrcID, rel.DstID)
	}
}
