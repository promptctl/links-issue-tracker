package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workflowsDryRunLeaf answers "what would inject when a ticket labeled
// <label> enters closed?" without a real occasion ever happening: it builds an
// Occasion from flags alone, matches it against the loaded Set, and prints
// every match with why it matched (Definition.MatchReasons) and the body
// that would be injected. This is the same "why" computation a real firing's
// trace uses (internal/workflows/trace.go), so a hypothetical here and a real
// firing explain themselves identically. [LAW:single-enforcer]
//
// Each flag is read as a definition's own key reads its entries
// (readKeyFlags), so a dry-run is asked in the spelling a definition is
// written in and compared in the form a real occasion carries. A value naming
// no moment lit fires, or no label a ticket can carry, is refused rather than
// explained: no real occasion carries it, so a match on it would describe
// nothing that can happen. [LAW:parse-dont-validate]
//
// It loads the definitions as a firing command does (loadWorkflows), so the
// load's warnings reach stderr and the event here too: the surface that
// explains why something fires names what can never fire.
// [LAW:nothing-unseen] It never writes a firing trace — nothing actually
// fired, there is nothing to record — and needs no store access, so it stays
// a wsCmd like the rest of `lit workflows`.
func workflowsDryRunLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows dry-run")
	when := fs.StringArray("when", "A moment the hypothetical occasion carries, spelled as a when: entry: enter <state>, exit <state>, or an event (repeatable: a start from open is exit open, enter in_progress, enter plan)")
	labels := fs.StringArray("label", "Label the hypothetical ticket carries (repeatable)")
	issue := fs.String("issue", "", "Issue id to interpolate into <id> in previewed bodies")
	return wsLeaf{fs: fs, positionals: 0, usage: workflowsUsage, work: func(ctx context.Context, inv Invocation, ws workspace.Info, _ []string) error {
		moments, carried, err := readKeyFlags(*when, *labels)
		if err != nil {
			return err
		}
		occasion := workflows.Occasion{Moments: moments, IssueID: model.ID(*issue), Labels: carried}
		matched := loadWorkflows(ctx, inv, ws).set.Matching(occasion)

		if err := printDryRunOccasion(inv.Stdout, occasion); err != nil {
			return err
		}
		return printDryRunMatches(inv.Stdout, matched, occasion)
	}}
}

func printDryRunOccasion(w io.Writer, o workflows.Occasion) error {
	_, err := fmt.Fprintf(w, "occasion: when=%s labels=%s issue=%s\n\n",
		orDash(strings.Join(workflows.Spellings(o.Moments), ",")), orDash(strings.Join(o.Labels, ",")), orDash(fmt.Sprint(o.IssueID)))
	return err
}

func printDryRunMatches(w io.Writer, matched []workflows.Match, o workflows.Occasion) error {
	if _, err := fmt.Fprintf(w, "Fired (%d)\n", len(matched)); err != nil {
		return err
	}
	if len(matched) == 0 {
		_, err := fmt.Fprintln(w, "  (none)")
		return err
	}
	for _, match := range matched {
		if _, err := fmt.Fprintf(w, "  %s  [%s]\n", formatDefinitionRef(match.Definition), strings.Join(match.Reasons, ", ")); err != nil {
			return err
		}
		body := workflows.Interpolate(match.Definition.Body, o.IssueID)
		for _, line := range strings.Split(body, "\n") {
			if _, err := fmt.Fprintf(w, "    %s\n", line); err != nil {
				return err
			}
		}
	}
	return nil
}
