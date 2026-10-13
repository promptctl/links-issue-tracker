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
		{name: "add", payload: appSubcommand{access: app.AccessWrite, declare: depAddLeaf}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, declare: depRmLeaf}},
		{name: "ls", payload: appSubcommand{access: app.AccessRead, declare: depLsLeaf}},
	},
}

// edgeFlags are the relations `lit dep add` and `lit dep rm` write, one flag
// per relation, each named for the sentence its edge records: `lit dep add A
// --blocked-by B` is "A is blocked by B". The issue named first is the edge's
// src and the flag's value its dst, the order the store keeps, so nothing flips
// on the way through. parent-child is absent: `lit parent set` and `lit parent
// clear` own that edge. [LAW:one-source-of-truth]
var edgeFlags = []struct {
	name  string
	usage string
	rel   model.RelationType
}{
	{name: "blocked-by", usage: "Issue that must close before this one can start", rel: model.RelBlockedBy},
	{name: "related-to", usage: "Issue this one is related to (no ordering)", rel: model.RelRelatedTo},
}

const edgeFlagsUsage = "--blocked-by <blocker-id> | --related-to <peer-id>"

// registerEdgeFlags declares every edgeFlags row on fs and returns the reader
// that turns the parsed flags into the one edge they name. [LAW:single-enforcer]
// Exactly-one is decided here for both commands; a command names one edge.
func registerEdgeFlags(fs *cobraFlagSet) func(usage string) (model.RelationType, string, error) {
	values := make([]*string, len(edgeFlags))
	for i, row := range edgeFlags {
		values[i] = fs.String(row.name, "", row.usage)
	}
	return func(usage string) (model.RelationType, string, error) {
		var named []int
		for i, value := range values {
			if *value != "" {
				named = append(named, i)
			}
		}
		if len(named) != 1 {
			return "", "", UsageError{Message: usage}
		}
		return edgeFlags[named[0]].rel, *values[named[0]], nil
	}
}

func depAddLeaf() appLeaf {
	fs := newCobraFlagSet("dep add")
	edge := registerEdgeFlags(fs)
	resolveActor := registerActor(fs)
	const usageLine0 = "usage: lit dep add <id> " + edgeFlagsUsage
	return appLeaf{fs: fs, usage: usageLine0, positionals: 1, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usageLine0}
		}
		rt, otherID, err := edge(usageLine0)
		if err != nil {
			return err
		}
		issueID := positional[0]
		// Self-loop check: a relation from an issue to itself is meaningless and
		// would otherwise corrupt downstream blocker traversals. Cheap to catch
		// here; transitive cycle detection is the store's.
		if issueID == otherID {
			return model.ValidationError{Message: fmt.Sprintf("dep add: self-loop rejected (%s %s %s)", issueID, rt, otherID)}
		}
		// [LAW:single-enforcer] Sibling blocked-by edges are rejected at the CLI
		// policy boundary, and only here: import's blocked_by may still order
		// siblings. Within one epic, rank is the canonical ordering; a blocked-by
		// edge would duplicate that signal.
		if rt == model.RelBlockedBy {
			if err := rejectSameEpicBlocks(ctx, ap, issueID, otherID); err != nil {
				return err
			}
		}
		rel, err := ap.Store.AddRelation(ctx, storage.AddRelationInput{SrcID: issueID, DstID: otherID, Type: rt, CreatedBy: resolveActor()})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, depRelationLine(rel)); err != nil {
			return err
		}
		return emitBreadcrumb(stdout, "update")
	}}
}

func depRmLeaf() appLeaf {
	fs := newCobraFlagSet("dep rm")
	edge := registerEdgeFlags(fs)
	const usageLine1 = "usage: lit dep rm <id> " + edgeFlagsUsage
	return appLeaf{fs: fs, usage: usageLine1, positionals: 1, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usageLine1}
		}
		rt, otherID, err := edge(usageLine1)
		if err != nil {
			return err
		}
		if err := ap.Store.RemoveRelation(ctx, positional[0], otherID, rt); err != nil {
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
	relType := fs.String("type", "", "Filter relation type: blocked-by|parent-child|related-to")
	const usageLine2 = "usage: lit dep ls <issue-id> [--type blocked-by|parent-child|related-to]"
	return appLeaf{fs: fs, usage: usageLine2, positionals: 1, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usageLine2}
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
		for _, rel := range relations {
			if _, err := fmt.Fprintln(stdout, depRelationLine(rel)); err != nil {
				return err
			}
		}
		return nil
	}}
}

// rejectSameEpicBlocks errors when both endpoints are leaves of one epic —
// siblings, which rank already orders. The other half of the rule, an edge
// between an issue and an epic it sits under, is the store's: it refuses that
// shape on every blocks write, imports included, with the same message.
func rejectSameEpicBlocks(ctx context.Context, ap *app.App, fromID, toID string) error {
	fromEpic, err := leafEpicID(ctx, ap, fromID)
	if err != nil {
		return err
	}
	toEpic, err := leafEpicID(ctx, ap, toID)
	if err != nil {
		return err
	}
	if fromEpic != "" && fromEpic == toEpic {
		return model.ValidationError{Message: storage.SameEpicBlocksRejectionMessage}
	}
	return nil
}

// leafEpicID returns the epic a leaf issue belongs to: its parent's ID when the
// parent is a container, otherwise "" (floating, or itself a container — an
// epic's ordering against its own children is the store's ancestry rule).
func leafEpicID(ctx context.Context, ap *app.App, issueID string) (string, error) {
	detail, err := ap.Store.GetIssueDetail(ctx, issueID)
	if err != nil {
		return "", err
	}
	if !detail.Issue.IsContainer() && detail.Parent != nil && detail.Parent.IsContainer() {
		return detail.Parent.ID, nil
	}
	return "", nil
}

// depRelationLine renders an edge as the sentence it records, endpoints in
// stored order: "A blocked by B", "C child of P", "A related to B".
func depRelationLine(rel model.Relation) string {
	return fmt.Sprintf("%s %s %s", rel.SrcID, relationVerb[rel.Type], rel.DstID)
}

// relationVerb is each relation's reading between its src and dst.
var relationVerb = map[model.RelationType]string{
	model.RelBlockedBy:   "blocked by",
	model.RelParentChild: "child of",
	model.RelRelatedTo:   "related to",
}
