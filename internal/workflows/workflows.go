// Package workflows is the definition model for `lit workflows`:
// user-authored guidance that lit injects into agent-facing output at declared
// work-lifecycle moments.
//
// A workflow definition is a markdown file with YAML frontmatter, discovered
// recursively under a layer root (`.lit/workflows/` in the project,
// `<config>/workflows/` globally, embedded defaults last). The folder
// hierarchy is arbitrary and user-defined — the path carries no activation
// meaning; frontmatter alone decides where a definition activates, along
// three declarative dimensions:
//
//   - labels: the acted-on ticket carries any of the listed labels
//   - states: the ticket enters or exits any of the listed states
//   - events: any of the listed semantic events fired (see events.go)
//
// Within a dimension the listed values are alternatives (OR); a definition
// with several dimensions requires all of them (AND). A dimension left out
// constrains nothing. A definition with no dimensions at all is inert: it
// loads, it is visible, it never fires.
//
// Workflow bodies are injected text, never executed, and no lifecycle is
// enforced or gated — this package only answers "which guidance is in place,
// and does it apply to this moment".
package workflows

import (
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// When says from which side a state activation observes a transition: the
// ticket entering the state, or exiting it. A transition between two states
// is expressible as exit-of-one plus enter-of-the-other; there is no separate
// transition concept.
type When string

const (
	WhenEnter When = "enter"
	WhenExit  When = "exit"
)

// StateActivation binds a definition to one state from one side. State names
// are open strings, deliberately not the lifecycle enum: built-in states
// (open, in_progress, closed) and future custom stages are the same shape,
// so new stage names need zero new machinery here.
// [LAW:one-type-per-behavior]
type StateActivation struct {
	State string
	When  When
}

// Definition is one loaded workflow definition: its identity, the activation
// dimensions parsed from frontmatter, the guidance body, and where it came
// from.
type Definition struct {
	// ID is the definition's primary key across layers: a nearer layer's
	// definition overrides a farther one with the same ID. Defaults to the
	// layer-relative path with the ".md" suffix removed and spaces replaced
	// by underscores.
	ID string
	// Name is an optional pretty name for display surfaces.
	Name string

	Labels []string
	States []StateActivation
	Events []Event

	// Body is the guidance markdown injected when the definition fires.
	Body string

	// Source is the layer the definition was resolved from; Path is its
	// layer-relative file path. Both exist for display and debuggability.
	Source templates.Source
	Path   string
}

// Inert reports whether the definition declares no activation dimension at
// all. An inert definition never fires.
func (d Definition) Inert() bool {
	return len(d.Labels) == 0 && len(d.States) == 0 && len(d.Events) == 0
}

// Warning is a non-fatal problem found while loading definitions. Loading
// never fails a lit invocation; every problem becomes a warning for the
// observability surfaces to print. [LAW:no-silent-failure]
type Warning struct {
	Source  templates.Source
	Path    string
	Message string
}

// Set is the merged, precedence-resolved collection of workflow definitions
// for a workspace, plus every warning produced while loading it.
type Set struct {
	// Definitions holds one definition per ID, nearest layer winning,
	// sorted by ID for stable iteration and display order.
	Definitions []Definition
	Warnings    []Warning
}
