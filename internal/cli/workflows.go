package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// lit workflows is the see-it surface promptctl-orchestration-ffqz.4 promised:
// a static read of what workflows.Load already resolved, projected onto the
// lifecycle a user already knows (pull -> work -> close) instead of onto the
// filesystem layout that produced it. Nothing here loads or matches
// definitions itself — that stays match.go/load.go's job — so this file is
// pure presentation over an already-resolved Set. [LAW:single-enforcer]

const workflowsUsage = "usage: lit workflows [show <id> | edit <id-or-point> | dry-run [--event <name>] [--label <name>]... [--enter <state>] [--exit <state>] [--issue <id>]]"

// workflowsFamily is the named surface under `workflows`: one definition
// resolved (`show <id>`), an override scaffolded or opened (`edit
// <id-or-point>`), and a hypothetical occasion explained (`dry-run`). The
// hand-rolled positional switch .4 established is gone: the legal-name set now
// comes from this table like every other family's, and each shape's flag
// surface is its own leaf's declaration. [LAW:one-source-of-truth]
var workflowsFamily = commandFamily[wsSubcommand]{
	usage: workflowsUsage,
	subcommands: []subcommandRow[wsSubcommand]{
		{name: "show", payload: wsSubcommand{declare: workflowsShowLeaf}},
		{name: "edit", payload: wsSubcommand{declare: workflowsEditLeaf}},
		{name: "dry-run", payload: wsSubcommand{declare: workflowsDryRunLeaf}},
	},
}

// workflowsDispatch turns argv into the leaf that serves it, with nothing open:
// bare `lit workflows` is the overview, a subcommand name walks the family, and
// -h/--help is the family's usage — the same answer resolve gives at every
// other level. [LAW:parse-dont-validate] the token is recognized once here and
// comes back as a wsLeaf, so no shape downstream re-reads argv to learn which
// one it is.
//
// The bare default is selected by the ABSENCE of a subcommand name, never by a
// flag, exactly as a nested family's bare path is (`lit sync reconcile`). A
// top-level command has no owning subcommandRow to carry `bare`, so that
// pairing is stated here instead. [LAW:dataflow-not-control-flow]
func workflowsDispatch(args []string) (wsLeaf, []string, error) {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return wsLeaf{}, nil, HelpRequestedError{Usage: workflowsUsage}
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return workflowsOverviewLeaf(), args, nil
	}
	return resolveWsLeaf(workflowsFamily, args)
}

// workflowsOverviewLeaf renders the lifecycle spine annotated with what is
// active at each point. It takes no flags and no positionals, so its whole
// declaration is the empty surface that makes `lit workflows --bogus` a usage
// error instead of a silently ignored token.
func workflowsOverviewLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows")
	return wsLeaf{fs: fs, positionals: 0, work: func(_ context.Context, stdout io.Writer, ws workspace.Info, _ []string) error {
		if fs.NArg() != 0 {
			return UsageError{Message: workflowsUsage}
		}
		return renderWorkflowsOverview(stdout, workflows.Load(ws.RootDir))
	}}
}

func workflowsShowLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows show")
	return wsLeaf{fs: fs, positionals: 1, work: func(_ context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
		if len(positional) != 1 || fs.NArg() != 0 {
			return UsageError{Message: workflowsUsage}
		}
		return renderWorkflowDefinition(stdout, workflows.Load(ws.RootDir), positional[0])
	}}
}

func workflowsEditLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows edit")
	return wsLeaf{fs: fs, positionals: 1, work: func(_ context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
		if len(positional) != 1 || fs.NArg() != 0 {
			return UsageError{Message: workflowsUsage}
		}
		return runWorkflowsEdit(stdout, ws, positional[0])
	}}
}

// builtinStates is the lifecycle's own state order — the spine every
// workspace has, with or without a single workflow file authored against it.
var builtinStates = []string{string(model.StateOpen), string(model.StateInProgress), string(model.StateClosed)}

// spineStates returns the states the overview walks: the built-in three, in
// lifecycle order, followed by any custom stage a loaded definition binds to
// that isn't one of them, alphabetically. Custom stages are first-class by
// design (states are open strings, never a closed enum — see workflows.go),
// so a definition authored against one is what puts it on the spine at all.
func spineStates(set workflows.Set) []string {
	seen := map[string]bool{}
	states := append([]string{}, builtinStates...)
	for _, s := range states {
		seen[s] = true
	}
	var custom []string
	for _, def := range set.Definitions {
		for _, activation := range def.States {
			if !seen[activation.State] {
				seen[activation.State] = true
				custom = append(custom, activation.State)
			}
		}
	}
	sort.Strings(custom)
	return append(states, custom...)
}

// spineLabels returns every label any loaded definition binds to, sorted, so
// the overview's label section only shows labels actually in play rather than
// every label ever used on a ticket.
func spineLabels(set workflows.Set) []string {
	seen := map[string]bool{}
	var labels []string
	for _, def := range set.Definitions {
		for _, label := range def.Labels {
			if !seen[label] {
				seen[label] = true
				labels = append(labels, label)
			}
		}
	}
	sort.Strings(labels)
	return labels
}

// renderWorkflowsOverview prints the lifecycle spine annotated with the
// definitions active at each point, then any loaded-but-never-fires files so
// "why isn't my file firing" is answerable from this one view.
func renderWorkflowsOverview(w io.Writer, set workflows.Set) error {
	if _, err := fmt.Fprintln(w, "lit workflows — work lifecycle guidance (project > global > embedded)"); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "\nEvents"); err != nil {
		return err
	}
	for _, event := range workflows.Catalog() {
		var at []workflows.Definition
		for _, def := range set.Definitions {
			for _, e := range def.Events {
				if e == event {
					at = append(at, def)
					break
				}
			}
		}
		if err := printSpinePoint(w, "  "+string(event), at); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(w, "\nStates"); err != nil {
		return err
	}
	for _, state := range spineStates(set) {
		if _, err := fmt.Fprintf(w, "  %s\n", state); err != nil {
			return err
		}
		for _, when := range []workflows.When{workflows.WhenEnter, workflows.WhenExit} {
			var at []workflows.Definition
			for _, def := range set.Definitions {
				for _, activation := range def.States {
					if activation.State == state && activation.When == when {
						at = append(at, def)
						break
					}
				}
			}
			if err := printSpinePoint(w, "    "+string(when), at); err != nil {
				return err
			}
		}
	}

	labels := spineLabels(set)
	if _, err := fmt.Fprintln(w, "\nLabels"); err != nil {
		return err
	}
	if len(labels) == 0 {
		if _, err := fmt.Fprintln(w, "  (none bound)"); err != nil {
			return err
		}
	}
	for _, label := range labels {
		var at []workflows.Definition
		for _, def := range set.Definitions {
			for _, l := range def.Labels {
				if l == label {
					at = append(at, def)
					break
				}
			}
		}
		if err := printSpinePoint(w, "  "+label, at); err != nil {
			return err
		}
	}

	return printWorkflowWarnings(w, set.Warnings)
}

// printSpinePoint renders one lifecycle point's label followed by every
// definition bound there, or nothing extra when none are.
// [LAW:dataflow-not-control-flow] every point runs the same print, whether or
// not any definition matched it — the definition list is the only thing that
// varies.
func printSpinePoint(w io.Writer, label string, at []workflows.Definition) error {
	if len(at) == 0 {
		_, err := fmt.Fprintln(w, label)
		return err
	}
	refs := make([]string, len(at))
	for i, def := range at {
		refs[i] = formatDefinitionRef(def)
	}
	_, err := fmt.Fprintf(w, "%s  [%s]\n", label, strings.Join(refs, ", "))
	return err
}

// formatDefinitionRef is the one rendering of "which definition, from where"
// every spine point and the warnings list shares.
// [LAW:one-source-of-truth]
func formatDefinitionRef(def workflows.Definition) string {
	if def.Name != "" {
		return fmt.Sprintf("%s %q (%s)", def.ID, def.Name, def.Source)
	}
	return fmt.Sprintf("%s (%s)", def.ID, def.Source)
}

// printWorkflowWarnings surfaces every load/parse warning — inert files,
// unknown events, malformed frontmatter, duplicate ids — so a file that loads
// but never fires is diagnosable from the same view that shows what does
// fire, instead of failing silently.
func printWorkflowWarnings(w io.Writer, warnings []workflows.Warning) error {
	if len(warnings) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nWarnings (loaded but not fully active)"); err != nil {
		return err
	}
	for _, warning := range warnings {
		// One line per warning even when the underlying cause (a yaml parse
		// error, say) embeds newlines of its own.
		message := strings.Join(strings.Fields(warning.Message), " ")
		if _, err := fmt.Fprintf(w, "  %s %s: %s\n", warning.Source, warning.Path, message); err != nil {
			return err
		}
	}
	return nil
}

// renderWorkflowDefinition prints one definition fully resolved: its
// activation frontmatter, its source, and its body — `lit workflows show
// <id>`'s whole job.
func renderWorkflowDefinition(w io.Writer, set workflows.Set, id string) error {
	def, ok := set.Lookup(id)
	if !ok {
		return ValidationError{Message: fmt.Sprintf("no workflow definition with id %q (run `lit workflows` to see loaded ids)", id)}
	}
	fields := []struct{ key, value string }{
		{"id", def.ID},
		{"name", orDash(def.Name)},
		{"source", string(def.Source)},
		{"path", def.Path},
		{"labels", orDash(strings.Join(def.Labels, ", "))},
		{"states", orDash(formatStateActivations(def.States))},
		{"events", orDash(formatEvents(def.Events))},
	}
	for _, f := range fields {
		if _, err := fmt.Fprintf(w, "%s: %s\n", f.key, f.value); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "---"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, def.Body)
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func formatStateActivations(activations []workflows.StateActivation) string {
	parts := make([]string, len(activations))
	for i, a := range activations {
		parts[i] = fmt.Sprintf("%s(%s)", a.State, a.When)
	}
	return strings.Join(parts, ", ")
}

func formatEvents(events []workflows.Event) string {
	parts := make([]string, len(events))
	for i, e := range events {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
}
