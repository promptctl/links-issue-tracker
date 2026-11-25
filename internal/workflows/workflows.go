// Package workflows is the definition model for `lit workflows`:
// user-authored guidance that lit injects into agent-facing output at declared
// work-lifecycle moments.
//
// A workflow definition is a markdown file with YAML frontmatter, discovered
// recursively under a layer root (`.lit/workflows/` in the project,
// `<config>/workflows/` globally, embedded defaults last). The folder
// hierarchy is arbitrary and user-defined — the path carries no activation
// meaning; frontmatter alone decides where a definition activates, with two
// keys:
//
//   - when: any of the listed moments happened — a crossing of the ticket's
//     state (`enter closed`, `exit plan`) or a read moment's event
//     (`show_ticket`, see events.go)
//   - labels: the acted-on ticket carries any of the listed labels
//
// Within a key the listed values are alternatives (OR); a definition with
// both requires both (AND). A key left out constrains nothing. A file lit
// cannot fire on its keys — it declares neither, or a key lit does not read,
// or a key none of whose entries lit can read, or its frontmatter is
// malformed — loads as a definition that never fires: it is visible, it holds
// its id, and the load warns why.
//
// Workflow bodies are injected text, never executed, and no lifecycle is
// enforced or gated — this package only answers "which guidance is in place,
// and does it apply to this moment".
package workflows

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// Moment is one thing that can happen at an occasion, and one entry of a
// definition's `when:` list: a read moment, named by its Event, or a Crossing
// of the ticket's state. An occasion carries the moments that happened and a
// definition lists the moments it fires on, so matching is equality between
// the two and nothing else. [LAW:types-are-the-program] the union is sealed:
// Event and Crossing are its only members, and ParseMoment, the one reader of
// a moment's spelling, admits only moments this lit fires.
type Moment interface {
	// String is the moment as a `when:` entry spells it, the spelling
	// ParseMoment reads back.
	String() string
	moment()
}

// Side is which way a crossing goes through its state. A transition between
// two states is exit-of-one plus enter-of-the-other; there is no separate
// transition concept.
type Side string

const (
	Enter Side = "enter"
	Exit  Side = "exit"
)

// Crossing is a ticket's state changing into or out of one of States.
// [LAW:one-type-per-behavior] a status and a phase are crossed the same way.
type Crossing struct {
	Side  Side
	State string
}

func (c Crossing) String() string { return string(c.Side) + " " + c.State }
func (Crossing) moment()          {}

// States is every state a ticket can be in, in the order the work passes
// through them: the statuses, with the phases inside in_progress after it.
// [LAW:one-source-of-truth] both come from the lifecycle, so a state it adds
// is crossable here with no edit.
func States() []string {
	var states []string
	for _, status := range model.States() {
		states = append(states, string(status))
		if status == model.StateInProgress {
			for _, phase := range model.Phases() {
				states = append(states, string(phase))
			}
		}
	}
	return states
}

// Moments is every moment this lit fires, in display order: each event in the
// catalog, then entering and exiting each of States.
func Moments() []Moment {
	var moments []Moment
	for _, event := range Catalog() {
		moments = append(moments, event)
	}
	for _, state := range States() {
		moments = append(moments, Crossing{Side: Enter, State: state}, Crossing{Side: Exit, State: state})
	}
	return moments
}

// Spellings is each moment as a `when:` entry spells it, in order — the one
// rendering every display, trace and event fact shares.
// [LAW:one-source-of-truth]
func Spellings(moments []Moment) []string {
	out := make([]string, len(moments))
	for i, m := range moments {
		out[i] = m.String()
	}
	return out
}

// Definition is one loaded workflow definition: its identity, when it fires,
// the guidance body, and where it came from.
type Definition struct {
	// ID is the definition's primary key across layers: a nearer layer's
	// definition overrides a farther one with the same ID. Defaults to the
	// layer-relative path with the ".md" suffix removed and spaces replaced
	// by underscores.
	ID string
	// Name is an optional pretty name for display surfaces.
	Name string

	Activation Activation

	// Body is the guidance markdown injected when the definition fires.
	Body string

	// Source is the layer the definition was resolved from; Path is its
	// layer-relative file path. Both exist for display and debuggability.
	Source templates.Source
	Path   string
}

// Activation is when a definition fires: on the occasions its Keys select,
// or Never. [LAW:types-are-the-program] the union is sealed, so "holds its id
// but cannot fire" is a value a definition carries, not a shape some fields
// happen to take.
type Activation interface {
	// reasons is the definition's entries the occasion carries, spelled
	// when:<moment> and label:<name>, when the definition fires on it; nil
	// when it does not.
	reasons(o Occasion) []string
}

// Keys is a definition that fires on its keys. Parsing builds it only with at
// least one key listing an entry, and a key that lists none constrains
// nothing.
type Keys struct {
	When   []Moment
	Labels []string
}

// Never is a definition lit never fires. Why is in the load's warnings for
// its file (Set.WarningsFor).
type Never struct{}

func (Never) reasons(Occasion) []string { return nil }

// Keys is the entries the definition fires at, for the surfaces that list
// definitions by moment and label: its Keys, and none for one that never
// fires.
func (d Definition) Keys() Keys {
	keys, _ := d.Activation.(Keys)
	return keys
}

// Warning is a non-fatal problem found while loading definitions. Loading
// never fails a lit invocation; every problem becomes a warning for the
// observability surfaces to print. [LAW:no-silent-failure]
type Warning struct {
	Source  templates.Source
	Path    string
	Message string
}

// String is the warning on one line — its layer, its file, and its message,
// with the line breaks and invisibles model.TextLines finds (a yaml parse
// error's, a file name's) and the runs of space between them collapsed — the
// one rendering `lit workflows`, a firing command's stderr and its event all
// share. [LAW:one-source-of-truth]
func (w Warning) String() string {
	lines := model.TextLines(fmt.Sprintf("%s %s: %s", w.Source, w.Path, w.Message))
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// Set is the merged, precedence-resolved collection of workflow definitions
// for a workspace, plus every warning produced while loading it.
type Set struct {
	// Definitions holds one definition per ID, nearest layer winning,
	// sorted by ID. Matching keeps this order; display reads
	// DefinitionsByLayer.
	Definitions []Definition
	Warnings    []Warning
}

// layerRank is the one statement of layer specificity, least specific first:
// the defaults the binary ships, then the user's global definitions, then the
// project's own. Load's claim order (nearest first) is precedence; this rank is
// the reading order. [LAW:one-source-of-truth]
func layerRank(source templates.Source) int {
	switch source {
	case templates.SourceEmbedded:
		return 0
	case templates.SourceGlobal:
		return 1
	case templates.SourceProject:
		return 2
	}
	// Load only ever stamps the three layers above; any other source is a
	// broken invariant, reported where it is found. [LAW:no-silent-failure]
	panic(fmt.Sprintf("workflows: no specificity rank for layer %q", source))
}

// DefinitionsByLayer returns the definitions least specific first: embedded
// defaults, then global, then project, each layer in ID order. It is a display
// view; Matching keeps the Set's ID order, so what fires and in what order is
// unchanged.
func (s Set) DefinitionsByLayer() []Definition {
	out := slices.Clone(s.Definitions)
	slices.SortStableFunc(out, func(a, b Definition) int {
		return cmp.Compare(layerRank(a.Source), layerRank(b.Source))
	})
	return out
}

// WarningsByLayer returns the warnings in the same least-specific-first order
// as DefinitionsByLayer, keeping load order within each layer.
func (s Set) WarningsByLayer() []Warning {
	out := slices.Clone(s.Warnings)
	slices.SortStableFunc(out, func(a, b Warning) int {
		return cmp.Compare(layerRank(a.Source), layerRank(b.Source))
	})
	return out
}

// WarningsFor is the load's warnings about the file the definition was read
// from, in load order.
func (s Set) WarningsFor(def Definition) []Warning {
	var out []Warning
	for _, warning := range s.Warnings {
		if warning.Source == def.Source && warning.Path == def.Path {
			out = append(out, warning)
		}
	}
	return out
}

// Lookup returns the definition with the given ID, if one is loaded. IDs are
// unique within a resolved Set by construction (Load claims each ID once
// across layers), so at most one definition can match.
func (s Set) Lookup(id string) (Definition, bool) {
	for _, def := range s.Definitions {
		if def.ID == id {
			return def, true
		}
	}
	return Definition{}, false
}
