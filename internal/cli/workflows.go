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

// runWorkflows routes the command's shapes: bare (the overview), `show <id>`
// (one definition, resolved), `edit <id-or-point>` (scaffold/open an
// override), and `dry-run` (explain a hypothetical occasion). Extends the
// hand-rolled positional switch .4 established rather than migrating to
// commandFamily — see promptctl-orchestration-ffqz.4's comment on why bare
// `lit workflows` (overview-by-default) doesn't fit that helper.
// [LAW:dataflow-not-control-flow] every shape but dry-run runs the same
// load-then-render pipeline; only which render function receives the Set
// varies. dry-run alone needs its own flagset, since it is the only shape
// with flags of its own.
func runWorkflows(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	_ = ctx
	positional, flagArgs := splitArgs(args, 2)
	switch {
	case len(positional) == 0:
		if err := parseNoWorkflowsFlags(flagArgs, stdout); err != nil {
			return err
		}
		return renderWorkflowsOverview(stdout, workflows.Load(ws.RootDir))
	case len(positional) == 2 && positional[0] == "show":
		if err := parseNoWorkflowsFlags(flagArgs, stdout); err != nil {
			return err
		}
		return renderWorkflowDefinition(stdout, workflows.Load(ws.RootDir), positional[1])
	case len(positional) == 2 && positional[0] == "edit":
		if err := parseNoWorkflowsFlags(flagArgs, stdout); err != nil {
			return err
		}
		return runWorkflowsEdit(stdout, ws, positional[1])
	case len(positional) == 1 && positional[0] == "dry-run":
		return runWorkflowsDryRun(stdout, ws, flagArgs)
	default:
		return UsageError{Message: workflowsUsage}
	}
}

// parseNoWorkflowsFlags is the shared "this shape takes no flags" guard the
// overview/show/edit shapes all run: any flag-shaped or oversupplied
// positional argument fails loudly rather than being silently ignored.
func parseNoWorkflowsFlags(flagArgs []string, stdout io.Writer) error {
	fs := newCobraFlagSet("workflows")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: workflowsUsage}
	}
	return nil
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
