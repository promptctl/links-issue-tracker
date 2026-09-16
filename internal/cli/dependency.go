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

// pendingEdge is an edge `lit dep add` or `lit parent set` is about to write:
// how a refusal names it, how it changes the relations of each issue it
// touches, and the issue every link it adds reaches, directly or through the
// blockers that issue inherits.
type pendingEdge struct {
	name  string
	patch func(storage.IssueRelations) storage.IssueRelations
	pivot string
}

// proposeEdge describes the edge rt, from, to as `lit dep add` takes it, and
// reports false when the edge adds no link readiness waits on. A blocks edge
// makes to depend on from, so every link it adds, to to or to an issue under
// it, reaches from. A parent-child edge puts from under to: the epic waits on
// it, it waits on the epic's blockers and its earlier lane-mates, its later
// lane-mates wait on it, and when it is an epic its own children inherit the
// new blockers, so every added link reaches from or one of those blockers. A
// parent that is not an epic adds none of these.
func proposeEdge(ctx context.Context, fetch relationsFetch, rt model.RelationType, from, to string) (pendingEdge, bool, error) {
	if rt != model.RelBlocks && rt != model.RelParentChild {
		return pendingEdge{}, false, nil
	}
	rels, err := fetch(ctx, []string{from, to})
	if err != nil {
		return pendingEdge{}, false, err
	}
	for _, id := range []string{from, to} {
		if _, ok := rels[id]; !ok {
			return pendingEdge{}, false, storage.NotFoundError{Entity: "issue", ID: id}
		}
	}
	fromIssue, toIssue := rels[from].Issue, rels[to].Issue
	if rt == model.RelBlocks {
		return pendingEdge{
			name:  fmt.Sprintf("%s blocks %s", from, to),
			pivot: from,
			patch: func(rel storage.IssueRelations) storage.IssueRelations {
				if rel.Issue.ID == to {
					rel.DependsOn = slices.Concat(rel.DependsOn, []model.Issue{fromIssue})
				}
				return rel
			},
		}, true, nil
	}
	if !toIssue.IsContainer() {
		return pendingEdge{}, false, nil
	}
	withoutChild := func(children []model.Issue) []model.Issue {
		return slices.DeleteFunc(slices.Clone(children), func(c model.Issue) bool { return c.ID == from })
	}
	return pendingEdge{
		name:  fmt.Sprintf("%s under epic %s", from, to),
		pivot: from,
		patch: func(rel storage.IssueRelations) storage.IssueRelations {
			switch rel.Issue.ID {
			case from:
				rel.Parent = &toIssue
			case to:
				rel.Children = append(withoutChild(rel.Children), fromIssue)
			default:
				rel.Children = withoutChild(rel.Children)
			}
			return rel
		},
	}, true, nil
}

// rejectWaitCycle refuses an edge that would leave issues waiting on each other
// forever. It reads the relations as they will be once the edge is written and
// walks the links readiness gates on (fetchWaitLinks), so the loops it refuses
// are the ones readiness would deadlock in, whether they run through an epic's
// blockers, an epic waiting on its children, or lane order, and closed issues
// are never part of one. Before this check, `lit dep add --from gate --to epic`
// followed by `lit dep add --from child --to gate` was accepted, and the child
// and the gate then waited on each other with nothing to break the loop.
//
// A loop that is already there without the edge is not this edge's to refuse.
// A loop of blocks edges alone, closed by a blocks edge, is the store's: it
// refuses every such cycle, which rank order needs. [LAW:single-enforcer]
func rejectWaitCycle(ctx context.Context, st storage.Store, rt model.RelationType, from, to string) error {
	cache := map[string]storage.IssueRelations{}
	before := func(ctx context.Context, ids []string) (map[string]storage.IssueRelations, error) {
		return relationsByID(ctx, st.GetRelationsByIDs, cache, ids)
	}
	edge, adds, err := proposeEdge(ctx, before, rt, from, to)
	if err != nil || !adds {
		return err
	}
	after := func(ctx context.Context, ids []string) (map[string]storage.IssueRelations, error) {
		rels, err := before(ctx, ids)
		if err != nil {
			return nil, err
		}
		patched := make(map[string]storage.IssueRelations, len(rels))
		for id, rel := range rels {
			patched[id] = edge.patch(rel)
		}
		return patched, nil
	}
	pivot, err := after(ctx, []string{edge.pivot})
	if err != nil {
		return err
	}
	ancestry, err := fetchContainerAncestry(ctx, after, pivot)
	if err != nil {
		return err
	}
	starts := []string{edge.pivot}
	for _, gate := range ancestry.inheritedDependencies(pivot[edge.pivot]) {
		starts = append(starts, gate.ID)
	}
	for _, start := range starts {
		loop, err := findWaitLoop(ctx, after, start)
		if err != nil {
			return err
		}
		blocksOnly := rt == model.RelBlocks && !slices.ContainsFunc(loop, func(l waitLink) bool { return l.kind != waitsOnDependency })
		if loop == nil || blocksOnly {
			continue
		}
		existing, err := findWaitLoop(ctx, before, start)
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		steps := make([]string, len(loop))
		for i, l := range loop {
			steps[i] = l.String()
		}
		return ValidationError{Message: fmt.Sprintf("refusing %s, which would leave issues waiting on each other forever: %s", edge.name, strings.Join(steps, ", "))}
	}
	return nil
}

// findWaitLoop returns a shortest loop of links that hold their waiter, from
// start back to start in waiting order, or nil when there is none. It walks
// breadth-first, one fetchWaitLinks expansion per step.
func findWaitLoop(ctx context.Context, fetch relationsFetch, start string) ([]waitLink, error) {
	reachedBy := map[string]waitLink{}
	for frontier := []string{start}; len(frontier) > 0; {
		links, err := fetchWaitLinks(ctx, fetch, frontier)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, link := range links {
			if _, seen := reachedBy[link.prereq]; seen || !link.holds() {
				continue
			}
			reachedBy[link.prereq] = link
			if link.prereq == start {
				loop := []waitLink{link}
				for id := link.waiter.ID; id != start; id = reachedBy[id].waiter.ID {
					loop = append(loop, reachedBy[id])
				}
				slices.Reverse(loop)
				return loop, nil
			}
			next = append(next, link.prereq)
		}
		frontier = next
	}
	return nil, nil
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
