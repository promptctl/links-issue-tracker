package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

var depFamily = commandFamily[appSubcommand]{
	usage: "usage: lit dep <add|rm|ls> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWriteRoutine, declare: depAddLeaf}},
		{name: "rm", payload: appSubcommand{access: app.AccessWriteRoutine, declare: depRmLeaf}},
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
// Exactly-one is decided here for both commands; a command names one edge. A
// flag repeated is refused while parsing (as every String flag is), and a flag given counts
// as naming its edge even when its value is empty, so `--blocked-by ""` beside
// `--related-to X` is refused rather than read as the related-to edge alone.
func registerEdgeFlags(fs *cobraFlagSet) func(usage string) (model.RelationType, string, error) {
	values := make([]*string, len(edgeFlags))
	for i, row := range edgeFlags {
		values[i] = fs.String(row.name, "", row.usage)
	}
	return func(usage string) (model.RelationType, string, error) {
		var named []int
		for i, row := range edgeFlags {
			if fs.Changed(row.name) {
				named = append(named, i)
			}
		}
		if len(named) != 1 || strings.TrimSpace(*values[named[0]]) == "" {
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
	return appLeaf{fs: fs, usage: usageLine0, positionals: 1, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
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
		rel, err := ap.Store.AddRelation(ctx, storage.AddRelationInput{SrcID: issueID, DstID: otherID, Type: rt, CreatedBy: resolveActor(inv.Env)})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(inv.Stdout, depRelationLine(rel)); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

func depRmLeaf() appLeaf {
	fs := newCobraFlagSet("dep rm")
	edge := registerEdgeFlags(fs)
	const usageLine1 = "usage: lit dep rm <id> " + edgeFlagsUsage
	return appLeaf{fs: fs, usage: usageLine1, positionals: 1, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
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
		if _, err := fmt.Fprintln(inv.Stdout, "ok"); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

func depLsLeaf() appLeaf {
	fs := newCobraFlagSet("dep ls")
	relType := fs.String("type", "", "Filter relation type: blocked-by|parent-child|related-to")
	const usageLine2 = "usage: lit dep ls <issue-id> [--type blocked-by|parent-child|related-to]"
	return appLeaf{fs: fs, usage: usageLine2, positionals: 1, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
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
			if _, err := fmt.Fprintln(inv.Stdout, depRelationLine(rel)); err != nil {
				return err
			}
		}
		return nil
	}}
}

// depRelationLine renders an edge as the sentence it records, endpoints in
// stored order: "A blocked by B", "C child of P", "A related to B".
func depRelationLine(rel model.Relation) string {
	return fmt.Sprintf("%s %s %s", rel.SrcID, relationReading(rel.Type), rel.DstID)
}

// relationReading is each relation's reading between its src and dst.
// [LAW:no-silent-failure] The default panics rather than printing two ids with
// nothing between them, which would hide which end waits.
func relationReading(rt model.RelationType) string {
	switch rt {
	case model.RelBlockedBy:
		return "blocked by"
	case model.RelParentChild:
		return "child of"
	case model.RelRelatedTo:
		return "related to"
	default:
		panic("relationReading: relation type with no reading: " + string(rt))
	}
}
