package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workflowsDryRunLeaf answers "what would inject when a ticket labeled
// <label> enters closed?" without a real occasion ever happening: it builds an
// Occasion from flags alone, matches it against the loaded Set, and prints
// every match with why it matched (Definition.MatchReasons) and the body
// that would be injected. This is the same "why" computation Dispatch's real
// firing trace uses (internal/workflows/trace.go), so a hypothetical here
// and a real firing explain themselves identically. [LAW:single-enforcer]
//
// Each --when is read by workflows.ParseMoment, the reader of a `when:`
// entry, so a dry-run is asked in the spelling a definition is written in. An
// event this lit never fires is refused rather than explained: no real
// occasion carries it, so a match on it would describe nothing that can
// happen. [LAW:parse-dont-validate]
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
		moments, err := dryRunMoments(*when)
		if err != nil {
			return err
		}
		occasion := workflows.Occasion{
			Moments: moments,
			IssueID: model.ID(*issue),
			Labels:  *labels,
		}
		set := workflows.Load(inv.Env, ws.RootDir)
		matched := set.Matching(occasion)

		if err := printDryRunOccasion(inv.Stdout, occasion); err != nil {
			return err
		}
		return printDryRunMatches(inv.Stdout, matched, occasion)
	}}
}

// dryRunMoments reads every --when value into the moment it names, refusing
// one this lit can never produce.
func dryRunMoments(values []string) ([]workflows.Moment, error) {
	var moments []workflows.Moment
	for _, value := range values {
		moment, ok := workflows.ParseMoment(value)
		if !ok {
			continue
		}
		if !moment.Known() {
			return nil, model.ValidationError{Message: fmt.Sprintf("--when %q is not a moment lit fires: write enter <state> or exit <state> for a change of a ticket's state, or one of the events %s", value, strings.Join(workflows.Spellings(catalogMoments()), ", "))}
		}
		moments = append(moments, moment)
	}
	return moments, nil
}

// catalogMoments is the event catalog as moments.
func catalogMoments() []workflows.Moment {
	var moments []workflows.Moment
	for _, event := range workflows.Catalog() {
		moments = append(moments, event)
	}
	return moments
}

func printDryRunOccasion(w io.Writer, o workflows.Occasion) error {
	_, err := fmt.Fprintf(w, "occasion: when=%s labels=%s issue=%s\n\n",
		quotedList(workflows.Spellings(o.Moments)), orDash(strings.Join(o.Labels, ",")), orDash(fmt.Sprint(o.IssueID)))
	return err
}

// quotedList renders moments as a list of quoted spellings, [] when there are
// none. Labels can join on a bare comma because a label cannot contain one; a
// state name is an open string that can, so each moment is quoted to keep one
// crossing of a state named "foo,bar" apart from two crossings.
func quotedList(spellings []string) string {
	quoted := make([]string, len(spellings))
	for i, spelling := range spellings {
		quoted[i] = strconv.Quote(spelling)
	}
	return "[" + strings.Join(quoted, ",") + "]"
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
