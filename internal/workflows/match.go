package workflows

import (
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Occasion is one moment at which workflow definitions may fire: what
// happened, and whatever ticket context the moment carries. Zero values mean
// the context is absent — no single acted-on ticket, nothing crossed — and a
// definition constrained on an absent context does not fire.
type Occasion struct {
	// Moments are what happened: the one event a read fires, or every
	// crossing a transition made. A transition can cross several states at
	// once, because a state can sit inside another — a phase is a state
	// inside in_progress, so a start from open exits open and enters both
	// in_progress and plan. Empty when a transition left the ticket where it
	// was.
	Moments []Moment
	// IssueID identifies the acted-on ticket, empty when the moment has no
	// single acted-on ticket (e.g. a backlog-wide view). Matching never reads
	// it — Matches keys only on Moments and Labels — it exists so display and
	// tracing consumers can name which ticket fired without re-deriving it
	// from context. [LAW:one-source-of-truth] one Occasion carries everything
	// a dispatch site knows about the moment, rather than a second payload
	// type duplicating Labels and Moments alongside it.
	IssueID model.ID
	// Labels are the acted-on ticket's labels in the store's canonical form
	// (model.NormalizeLabel; definitions are stamped into the same form at
	// parse time, so comparison here is exact). Nil when the moment has no
	// single acted-on ticket (e.g. a backlog-wide view).
	Labels []string
}

// Matches reports whether the definition fires on the occasion. Composition
// is OR within a key (any listed value satisfies it) and AND across keys
// (every declared key must be satisfied); an undeclared key constrains
// nothing. An inert definition never fires.
// [LAW:dataflow-not-control-flow] The same checks run for every definition on
// every occasion; which keys bite is decided entirely by the data.
func (d Definition) Matches(o Occasion) bool {
	if d.Inert() {
		return false
	}
	return matchAny(d.When, o.Moments) && matchAny(d.Labels, o.Labels)
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

// matchAny is one key's test: unconstrained when the definition lists
// nothing, otherwise satisfied by any listed value the occasion carries.
// Moments and labels are both compared by equality — a crossing is its side
// and state, an event its name — so one test serves both keys.
// [LAW:one-type-per-behavior]
func matchAny[T comparable](bound, carried []T) bool {
	if len(bound) == 0 {
		return true
	}
	return slices.ContainsFunc(bound, func(value T) bool {
		return slices.Contains(carried, value)
	})
}

// MatchReasons returns, for a definition that fires on the occasion, which
// specific value in each declared key satisfied it — "when:enter closed",
// "label:needs-design" — the shared "why" computation behind both the firing
// trace (a real occasion) and `lit workflows dry-run` (a hypothetical one).
// Nil when the definition does not match.
// [LAW:single-enforcer] built on Matches rather than re-deriving the boolean
// logic, so the two can never disagree about whether a definition fired.
func (d Definition) MatchReasons(o Occasion) []string {
	if !d.Matches(o) {
		return nil
	}
	var reasons []string
	for _, moment := range d.When {
		if slices.Contains(o.Moments, moment) {
			reasons = append(reasons, "when:"+moment.String())
		}
	}
	for _, label := range d.Labels {
		if slices.Contains(o.Labels, label) {
			reasons = append(reasons, "label:"+label)
		}
	}
	return reasons
}
