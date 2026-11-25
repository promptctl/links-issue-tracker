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
// Each flag is read through the gate a definition's own entry is: --when by
// workflows.ParseMoment, --label by model.NormalizeLabel, so a dry-run is
// asked in the spelling a definition is written in and compared in the form
// a real occasion carries. A value naming no moment lit fires, or no label a
// ticket can carry, is refused rather than explained: no real occasion
// carries it, so a match on it would describe nothing that can happen.
// [LAW:parse-dont-validate]
//
// Never writes a firing trace itself — nothing actually fired, there is
// nothing to record — and needs no store access, only the loaded definition
// Set, so it stays a wsCmd like the rest of `lit workflows`.
func workflowsDryRunLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows dry-run")
	when := fs.StringArray("when", "A moment the hypothetical occasion carries, spelled as a when: entry: enter <state>, exit <state>, or an event (repeatable: a start from open is exit open, enter in_progress, enter plan)")
	labels := fs.StringArray("label", "Label the hypothetical ticket carries (repeatable)")
	issue := fs.String("issue", "", "Issue id to interpolate into <id> in previewed bodies")
	return wsLeaf{fs: fs, positionals: 0, usage: workflowsUsage, work: func(_ context.Context, inv Invocation, ws workspace.Info, _ []string) error {
		occasion, err := dryRunOccasion(*when, *labels, model.ID(*issue))
		if err != nil {
			return err
		}
		set := workflows.Load(inv.Env, ws.RootDir)
		matched := set.Matching(occasion)

		if err := printDryRunOccasion(inv.Stdout, occasion); err != nil {
			return err
		}
		return printDryRunMatches(inv.Stdout, matched, occasion)
	}}
}

// dryRunOccasion is the hypothetical occasion the flags describe, or the
// first flag value that names nothing a real occasion can carry.
func dryRunOccasion(when, labels []string, issueID model.ID) (workflows.Occasion, error) {
	occasion := workflows.Occasion{IssueID: issueID}
	for _, value := range when {
		moment, err := workflows.ParseMoment(value)
		if err != nil {
			return workflows.Occasion{}, model.ValidationError{Message: fmt.Sprintf("--when %q is not a moment lit fires: %v", value, err)}
		}
		occasion.Moments = append(occasion.Moments, moment)
	}
	for _, value := range labels {
		label, err := model.NormalizeLabel(value)
		if err != nil {
			return workflows.Occasion{}, model.ValidationError{Message: fmt.Sprintf("--label %q is not a label a ticket can carry: %v", value, err)}
		}
		occasion.Labels = append(occasion.Labels, label)
	}
	return occasion, nil
}

func printDryRunOccasion(w io.Writer, o workflows.Occasion) error {
	_, err := fmt.Fprintf(w, "occasion: when=%s labels=%s issue=%s\n\n",
		orDash(strings.Join(workflows.Spellings(o.Moments), ",")), orDash(strings.Join(o.Labels, ",")), orDash(fmt.Sprint(o.IssueID)))
	return err
}

func printDryRunMatches(w io.Writer, matched []workflows.Definition, o workflows.Occasion) error {
	if _, err := fmt.Fprintf(w, "Fired (%d)\n", len(matched)); err != nil {
		return err
	}
	if len(matched) == 0 {
		_, err := fmt.Fprintln(w, "  (none)")
		return err
	}
	for _, def := range matched {
		reasons := def.MatchReasons(o)
		if _, err := fmt.Fprintf(w, "  %s  [%s]\n", formatDefinitionRef(def), strings.Join(reasons, ", ")); err != nil {
			return err
		}
		body := workflows.Interpolate(def.Body, o.IssueID)
		for _, line := range strings.Split(body, "\n") {
			if _, err := fmt.Fprintf(w, "    %s\n", line); err != nil {
				return err
			}
		}
	}
	return nil
}
