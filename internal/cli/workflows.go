package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
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

const workflowsUsage = "usage: lit workflows [show <id> | edit <id> | edit [--when <moment>]... [--label <name>]... | dry-run [--when <moment>]... [--label <name>]... [--issue <id>]]"

// workflowsFamily is the named surface under `workflows`: one definition
// resolved (`show <id>`), a definition opened or scaffolded (`edit <id>`,
// `edit --when <moment> --label <name>`), and a hypothetical occasion
// explained (`dry-run`). The
// legal-name set comes from this table like every other family's, and each
// shape's flag surface is its own leaf's declaration. [LAW:one-source-of-truth]
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
func workflowsDispatch(ctx context.Context, args []string) (wsLeaf, []string, error) {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return wsLeaf{}, nil, HelpRequestedError{Usage: workflowsUsage, Detail: helpText("workflows")}
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return workflowsOverviewLeaf(), args, nil
	}
	return resolveWsLeaf(ctx, workflowsFamily, args)
}

// workflowsOverviewLeaf renders the lifecycle spine annotated with what is
// active at each point. It takes no flags and no positionals, so its whole
// declaration is the empty surface that makes `lit workflows --bogus` a usage
// error instead of a silently ignored token.
func workflowsOverviewLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows")
	return wsLeaf{fs: fs, positionals: 0, usage: workflowsUsage, work: func(_ context.Context, inv Invocation, ws workspace.Info, _ []string) error {
		return renderWorkflowsOverview(inv.Stdout, workflows.Load(inv.Env, ws.RootDir))
	}}
}

func workflowsShowLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows show")
	return wsLeaf{fs: fs, positionals: 1, usage: workflowsUsage, work: func(_ context.Context, inv Invocation, ws workspace.Info, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: workflowsUsage}
		}
		return renderWorkflowDefinition(inv.Stdout, workflows.Load(inv.Env, ws.RootDir), positional[0])
	}}
}

// workflowsEditLeaf takes a definition's id, to open or override it, or the
// keys of a new one, to scaffold it — one or the other, never both: the
// positional only ever names a definition, so no text is read two ways.
// [LAW:parse-dont-validate]
func workflowsEditLeaf() wsLeaf {
	fs := newCobraFlagSet("workflows edit")
	when := fs.StringArray("when", "Scaffold a new definition firing at this moment, spelled as a when: entry: enter <state>, exit <state>, or an event (repeatable)")
	labels := fs.StringArray("label", "Scaffold a new definition firing on tickets carrying this label (repeatable)")
	return wsLeaf{fs: fs, positionals: 1, usage: workflowsUsage, work: func(_ context.Context, inv Invocation, ws workspace.Info, positional []string) error {
		keyed := len(*when)+len(*labels) > 0
		switch {
		case len(positional) == 1 && !keyed:
			return runWorkflowsEdit(inv, ws, positional[0])
		case len(positional) == 0 && keyed:
			return editFreshDefinition(inv, ws, *when, *labels)
		}
		return UsageError{Message: workflowsUsage}
	}}
}

// readKeyFlags reads --when and --label as a definition's own keys read their
// entries (workflows.Entries): each moment and label once, in the form an
// occasion carries, or the first value naming no moment lit fires or no
// label a ticket can carry, refused. `dry-run` and `edit` both read their
// flags here. [LAW:single-enforcer]
func readKeyFlags(whenTexts, labelTexts []string) ([]workflows.Moment, []string, error) {
	when, refused := workflows.Entries(whenTexts, workflows.ParseMoment)
	if len(refused) > 0 {
		return nil, nil, model.ValidationError{Message: fmt.Sprintf("--when %q is not a moment lit fires: %v", refused[0].Text, refused[0].Err)}
	}
	labels, refused := workflows.Entries(labelTexts, model.NormalizeLabel)
	if len(refused) > 0 {
		return nil, nil, model.ValidationError{Message: fmt.Sprintf("--label %q is not a label a ticket can carry: %v", refused[0].Text, refused[0].Err)}
	}
	return when, labels, nil
}

// spineLabels returns every label any loaded definition binds to, sorted, so
// the overview's label section only shows labels actually in play rather than
// every label ever used on a ticket.
func spineLabels(set workflows.Set) []string {
	var labels []string
	for _, def := range set.Definitions {
		if keys, fires := def.Activation.Keys(); fires {
			labels = append(labels, keys.Labels.Entries()...)
		}
	}
	slices.Sort(labels)
	return slices.Compact(labels)
}

// boundAt is the definitions in defs whose key lists entry, in defs' order.
// A definition that never fires is bound nowhere.
func boundAt[T comparable](defs []workflows.Definition, key func(workflows.Keys) workflows.Key[T], entry T) []workflows.Definition {
	var at []workflows.Definition
	for _, def := range defs {
		if keys, fires := def.Activation.Keys(); fires && slices.Contains(key(keys).Entries(), entry) {
			at = append(at, def)
		}
	}
	return at
}

func whenKey(keys workflows.Keys) workflows.Key[workflows.Moment] { return keys.When }
func labelsKey(keys workflows.Keys) workflows.Key[string]         { return keys.Labels }

// renderWorkflowsOverview prints the lifecycle spine annotated with the
// definitions active at each point, then any loaded-but-never-fires files so
// "why isn't my file firing" is answerable from this one view.
func renderWorkflowsOverview(w io.Writer, set workflows.Set) error {
	// Every point lists its definitions least specific first, so a project
	// file reads last at the point it shares with the defaults.
	defs := set.DefinitionsByLayer()
	if _, err := fmt.Fprintln(w, "lit workflows — work lifecycle guidance (project > global > embedded)"); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "\nWhen"); err != nil {
		return err
	}
	for _, moment := range workflows.Moments() {
		if err := printSpinePoint(w, "  "+moment.String(), boundAt(defs, whenKey, moment)); err != nil {
			return err
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
		if err := printSpinePoint(w, "  "+label, boundAt(defs, labelsKey, label)); err != nil {
			return err
		}
	}

	return printWorkflowWarnings(w, set.WarningsByLayer())
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
// the overview's points and dry-run's matches share.
// [LAW:one-source-of-truth]
func formatDefinitionRef(def workflows.Definition) string {
	if def.Name != "" {
		return fmt.Sprintf("%s %q (%s)", def.ID, def.Name, def.Source)
	}
	return fmt.Sprintf("%s (%s)", def.ID, def.Source)
}

// printWorkflowWarnings surfaces every load/parse warning — files with no
// keys, entries naming nothing lit fires, keys this lit does not read,
// malformed frontmatter, duplicate ids — so a file that loads
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
		if _, err := fmt.Fprintf(w, "  %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

// renderWorkflowDefinition prints one definition fully resolved: its
// identity and source, when it fires — its keys, or `fires: never` — and its
// body. `lit workflows show <id>`'s whole job.
func renderWorkflowDefinition(w io.Writer, set workflows.Set, id string) error {
	def, ok := set.Lookup(id)
	if !ok {
		return model.ValidationError{Message: fmt.Sprintf("no workflow definition with id %q (run `lit workflows` to see loaded ids)", id)}
	}
	type field struct{ key, value string }
	fields := []field{
		{"id", def.ID},
		{"name", orDash(def.Name)},
		{"source", string(def.Source)},
		{"path", def.Path},
	}
	activation := []field{{"fires", "never"}}
	if keys, fires := def.Activation.Keys(); fires {
		activation = []field{
			{"when", orDash(strings.Join(workflows.Spellings(keys.When.Entries()), ", "))},
			{"labels", orDash(strings.Join(keys.Labels.Entries(), ", "))},
		}
	}
	fields = append(fields, activation...)
	for _, f := range fields {
		if _, err := fmt.Fprintf(w, "%s: %s\n", f.key, f.value); err != nil {
			return err
		}
	}
	// The load's warnings for this file, so a definition that fires less than
	// it says, or never, shows why beside when it fires.
	if err := printWorkflowWarnings(w, set.WarningsFor(def)); err != nil {
		return err
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
