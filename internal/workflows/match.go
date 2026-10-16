package workflows

import (
	"fmt"
	"slices"
)

// Occasion is one moment at which workflow definitions may fire: the semantic
// event that occurred, and whatever ticket context the moment carries. Zero
// values mean the context is absent — no single acted-on ticket, no state
// transition — and a definition constrained on an absent context does not
// fire.
type Occasion struct {
	// Event is the semantic event fired, or zero when the moment carries none.
	Event Event
	// IssueID identifies the acted-on ticket, empty when the moment has no
	// single acted-on ticket (e.g. a backlog-wide view). Matching never reads
	// it — Matches keys only on Event/Labels/Entered/Exited — it exists so
	// display and tracing consumers can name which ticket fired an event
	// without re-deriving it from context. [LAW:one-source-of-truth] one
	// Occasion carries everything a dispatch site knows about the moment,
	// rather than a second payload type duplicating Labels/Entered/Exited
	// alongside it.
	IssueID string
	// Labels are the acted-on ticket's labels in the store's canonical form
	// (model.NormalizeLabel; definitions are stamped into the same form at
	// parse time, so comparison here is exact). Nil when the moment has no
	// single acted-on ticket (e.g. a backlog-wide view).
	Labels []string
	// Entered and Exited name the states the ticket's transition moved it into
	// and out of, when one happened: a transition is exit-of-some plus
	// enter-of-others. A transition can enter or exit several states at once,
	// because a state can sit inside another — a phase is a state inside
	// in_progress, so a start enters both. Both empty when no transition
	// occurred.
	Entered []string
	Exited  []string
}

// Matches reports whether the definition fires on the occasion. Composition
// is OR within a dimension (any listed value satisfies it) and AND across
// dimensions (every declared dimension must be satisfied); an undeclared
// dimension constrains nothing. An inert definition never fires.
// [LAW:dataflow-not-control-flow] The same three checks run for every
// definition on every occasion; which dimensions bite is decided entirely by
// the data.
func (d Definition) Matches(o Occasion) bool {
	if d.Inert() {
		return false
	}
	return matchEvents(d.Events, o.Event) &&
		matchLabels(d.Labels, o.Labels) &&
		matchStates(d.States, o)
}

// Matching returns the definitions that fire on the occasion, in the Set's
// stable ID order.
func (s Set) Matching(o Occasion) []Definition {
	var out []Definition
	for _, def := range s.Definitions {
		if def.Matches(o) {
			out = append(out, def)
		}
	}
	return out
}

func matchEvents(bound []Event, fired Event) bool {
	if len(bound) == 0 {
		return true
	}
	return fired != "" && slices.Contains(bound, fired)
}

func matchLabels(bound []string, carried []string) bool {
	if len(bound) == 0 {
		return true
	}
	return slices.ContainsFunc(bound, func(label string) bool {
		return slices.Contains(carried, label)
	})
}

func matchStates(bound []StateActivation, o Occasion) bool {
	if len(bound) == 0 {
		return true
	}
	return slices.ContainsFunc(bound, func(activation StateActivation) bool {
		return activation.matches(o)
	})
}

// matches reports whether the occasion's transition satisfies one state
// activation: the activation's state is among those the transition entered,
// or exited, as the activation's side says. "No transition happened" is an
// empty side, so it never satisfies a state binding.
func (a StateActivation) matches(o Occasion) bool {
	observed := map[When][]string{WhenEnter: o.Entered, WhenExit: o.Exited}[a.When]
	return slices.Contains(observed, a.State)
}

// MatchReasons returns, for a definition that fires on the occasion, which
// specific value in each declared dimension satisfied it — "event:work_started",
// "label:needs-design", "state:open(enter)" — the shared "why" computation
// behind both the firing trace (a real occasion) and `lit workflows dry-run`
// (a hypothetical one). Nil when the definition does not match.
// [LAW:single-enforcer] built directly on Matches/matches rather than
// re-deriving the boolean logic, so the two can never disagree about whether
// a definition fired.
func (d Definition) MatchReasons(o Occasion) []string {
	if !d.Matches(o) {
		return nil
	}
	var reasons []string
	if len(d.Events) > 0 {
		reasons = append(reasons, "event:"+string(o.Event))
	}
	for _, label := range d.Labels {
		if slices.Contains(o.Labels, label) {
			reasons = append(reasons, "label:"+label)
		}
	}
	for _, activation := range d.States {
		if activation.matches(o) {
			reasons = append(reasons, fmt.Sprintf("state:%s(%s)", activation.State, activation.When))
		}
	}
	return reasons
}
