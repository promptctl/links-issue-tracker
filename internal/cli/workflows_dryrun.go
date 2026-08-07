package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runWorkflowsDryRun answers "what would inject on <event> for a ticket
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
func runWorkflowsDryRun(stdout io.Writer, ws workspace.Info, flagArgs []string) error {
	fs := newCobraFlagSet("workflows dry-run")
	event := fs.String("event", "", "Semantic event the hypothetical occasion fires (see the event catalog in 'lit workflows')")
	labels := fs.StringArray("label", "Label the hypothetical ticket carries (repeatable)")
	enter := fs.String("enter", "", "State the hypothetical ticket enters")
	exit := fs.String("exit", "", "State the hypothetical ticket exits")
	issue := fs.String("issue", "", "Issue id to interpolate into <id> in previewed bodies")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: workflowsUsage}
	}

	occasion := workflows.Occasion{
		Event:   workflows.Event(*event),
		IssueID: *issue,
		Labels:  *labels,
		Entered: *enter,
		Exited:  *exit,
	}
	set := workflows.Load(ws.RootDir)
	matched := set.Matching(occasion)

	if err := printDryRunOccasion(stdout, occasion); err != nil {
		return err
	}
	return printDryRunMatches(stdout, matched, occasion)
}

func printDryRunOccasion(w io.Writer, o workflows.Occasion) error {
	_, err := fmt.Fprintf(w, "occasion: event=%s labels=%s entered=%s exited=%s issue=%s\n\n",
		orDash(string(o.Event)), orDash(strings.Join(o.Labels, ",")), orDash(o.Entered), orDash(o.Exited), orDash(o.IssueID))
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
