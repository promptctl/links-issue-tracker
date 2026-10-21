package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workflowsDryRunLeaf answers "what would inject on <event> for a ticket
// labeled <label>?" without a real occasion ever happening: it builds an
// Occasion from flags alone, matches it against the loaded Set, and prints
// every match with why it matched (Definition.MatchReasons) and the body
// that would be injected. This is the same "why" computation Dispatch's real
// firing trace uses (internal/workflows/trace.go), so a hypothetical here
// and a real firing explain themselves identically. [LAW:single-enforcer]
//
// Never writes a firing trace itself — nothing actually fired, there is
// nothing to record — and needs no store access, only the loaded definition
// Set, so it stays a wsCmd like the rest of `lit workflows`.
func workflowsDryRunLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows dry-run")
	event := fs.String("event", "", "Semantic event the hypothetical occasion fires (see the event catalog in 'lit workflows')")
	labels := fs.StringArray("label", "Label the hypothetical ticket carries (repeatable)")
	enter := fs.StringArray("enter", "State the hypothetical ticket enters (repeatable: a start enters in_progress and plan)")
	exit := fs.StringArray("exit", "State the hypothetical ticket exits (repeatable)")
	issue := fs.String("issue", "", "Issue id to interpolate into <id> in previewed bodies")
	return wsLeaf{fs: fs, positionals: 0, usage: workflowsUsage, work: func(_ context.Context, inv Invocation, ws workspace.Info, _ []string) error {

		occasion := workflows.Occasion{
			Event:   workflows.Event(*event),
			IssueID: *issue,
			Labels:  *labels,
			Entered: *enter,
			Exited:  *exit,
		}
		set := workflows.Load(ws.RootDir)
		matched := set.Matching(occasion)

		if err := printDryRunOccasion(inv.Stdout, occasion); err != nil {
			return err
		}
		return printDryRunMatches(inv.Stdout, matched, occasion)
	}}
}

func printDryRunOccasion(w io.Writer, o workflows.Occasion) error {
	_, err := fmt.Fprintf(w, "occasion: event=%s labels=%s entered=%s exited=%s issue=%s\n\n",
		orDash(string(o.Event)), orDash(strings.Join(o.Labels, ",")), stateList(o.Entered), stateList(o.Exited), orDash(o.IssueID))
	return err
}

// stateList renders states as a list of quoted names, [] when there are none.
// Labels can join on a bare comma because a label cannot contain one; a state
// name is an open string that can, so each name is quoted to keep one state
// named "foo,bar" apart from the two states foo and bar.
func stateList(states []string) string {
	quoted := make([]string, len(states))
	for i, state := range states {
		quoted[i] = strconv.Quote(state)
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
