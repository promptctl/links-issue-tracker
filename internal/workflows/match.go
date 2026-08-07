package workflows

import "slices"

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
	// Entered and Exited name the states of the ticket's transition, when one
	// happened: a transition between two states is exit-of-one plus
	// enter-of-the-other. Both zero when no transition occurred.
	Entered string
	Exited  string
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
// activation. The occasion side must be non-empty: "no transition happened"
// never satisfies a state binding, whatever the activation says.
func (a StateActivation) matches(o Occasion) bool {
	observed := map[When]string{WhenEnter: o.Entered, WhenExit: o.Exited}[a.When]
	return observed != "" && observed == a.State
}
