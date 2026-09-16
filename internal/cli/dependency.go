package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

var depFamily = commandFamily[appSubcommand]{
	usage: "usage: lit dep <add|rm|ls> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWrite, declare: depAddLeaf}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, declare: depRmLeaf}},
		{name: "ls", payload: appSubcommand{access: app.AccessRead, declare: depLsLeaf}},
	},
}

func depAddLeaf() appLeaf {
	fs := newCobraFlagSet("dep add")
	relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
	from := fs.String("from", "", "Source issue ID (required)")
	to := fs.String("to", "", "Target issue ID (required)")
	resolveActor := registerActor(fs)
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
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
		// would otherwise corrupt downstream blocker traversals. Longer cycles are
		// refused by rejectWaitCycle below and by the store.
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
		if err := rejectWaitCycle(ctx, ap.Store, rt, fromID, toID); err != nil {
			return err
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
	}}
}

func depRmLeaf() appLeaf {
	fs := newCobraFlagSet("dep rm")
	relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
	from := fs.String("from", "", "Source issue ID (required)")
	to := fs.String("to", "", "Target issue ID (required)")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
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
	}}
}

func depLsLeaf() appLeaf {
	fs := newCobraFlagSet("dep ls")
	relType := fs.String("type", "", "Filter relation type")
	return appLeaf{fs: fs, positionals: 1, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
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
	}}
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

// waitLink is one step along which readiness makes an issue wait: a blocks
// edge, or an epic holding back a child. A blocks edge onto an epic gates every
// issue under that epic (links-epic-block-xpkz), so an epic's hold carries the
// epic's blockers down to its children.
type waitLink struct {
	before, after string
	hold          bool
}

func (l waitLink) String() string {
	if l.hold {
		return fmt.Sprintf("epic %s holds back %s", l.before, l.after)
	}
	return fmt.Sprintf("%s blocks %s", l.before, l.after)
}

// rejectWaitCycle refuses an edge that would leave issues waiting on each other
// forever. The store already refuses a cycle of blocks edges alone, which rank
// order needs, and it keeps that refusal. This check adds only the cycles that
// pass through an epic's hold, which the store's graph does not contain. Before
// this check, `lit dep add --from gate --to epic` followed by `lit dep add --from
// child --to gate` was accepted, and the child and the gate then waited on each
// other with nothing to break the loop.
//
// rt, from and to name the edge as `lit dep add` takes it. A blocks edge makes
// to wait on from. A parent-child edge onto an epic makes the epic hold back
// from. A parent that is not an epic holds nothing back. The edge closes a cycle
// when the issue it puts second already comes before the one it puts first.
func rejectWaitCycle(ctx context.Context, st storage.Store, rt model.RelationType, from, to string) error {
	var link waitLink
	switch rt {
	case model.RelBlocks:
		link = waitLink{before: from, after: to}
	case model.RelParentChild:
		parent, err := st.GetIssue(ctx, to)
		if err != nil {
			return err
		}
		if !parent.IsContainer() {
			return nil
		}
		link = waitLink{before: to, after: from, hold: true}
	default:
		return nil
	}
	path, err := findWaitPath(ctx, st, link.after, link.before)
	if err != nil {
		return err
	}
	// A path of blocks edges alone, closed by a blocks edge, is the store's
	// refusal to make. [LAW:single-enforcer]
	throughHold := link.hold || slices.ContainsFunc(path, func(l waitLink) bool { return l.hold })
	if path == nil || !throughHold {
		return nil
	}
	steps := make([]string, len(path))
	for i, l := range path {
		steps[i] = l.String()
	}
	return ValidationError{Message: fmt.Sprintf("refusing %s: %s already waits on %s (%s), so this edge would close a loop. A blocks edge onto an epic holds back every issue under that epic", link, link.before, link.after, strings.Join(steps, ", "))}
}

// findWaitPath returns the steps by which start already comes before goal, in
// order, or nil when it does not. It walks breadth-first, one batched relations
// query per step, so the path it returns is a shortest one.
func findWaitPath(ctx context.Context, st storage.Store, start, goal string) ([]waitLink, error) {
	reached := map[string]waitLink{start: {}}
	for frontier := []string{start}; len(frontier) > 0; {
		relations, err := st.GetRelationsByIDs(ctx, frontier)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, id := range frontier {
			rel := relations[id]
			links := make([]waitLink, 0, len(rel.Blocks)+len(rel.Children))
			for _, dependent := range rel.Blocks {
				links = append(links, waitLink{before: id, after: dependent.ID})
			}
			if rel.Issue.IsContainer() {
				for _, child := range rel.Children {
					links = append(links, waitLink{before: id, after: child.ID, hold: true})
				}
			}
			for _, l := range links {
				if _, seen := reached[l.after]; seen {
					continue
				}
				reached[l.after] = l
				if l.after == goal {
					return tracePath(reached, start, goal), nil
				}
				next = append(next, l.after)
			}
		}
		frontier = next
	}
	return nil, nil
}

// tracePath follows the links findWaitPath recorded back from goal to start and
// returns them in walking order.
func tracePath(reached map[string]waitLink, start, goal string) []waitLink {
	var path []waitLink
	for id := goal; id != start; id = reached[id].before {
		path = append(path, reached[id])
	}
	slices.Reverse(path)
	return path
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
