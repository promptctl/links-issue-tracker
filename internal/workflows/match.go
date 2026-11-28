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

// Matches reports whether the definition fires on the occasion: whether its
// activation gives a reason to. [LAW:single-enforcer] firing and its reasons
// are one computation, so the trace and dry-run can never explain a firing
// that did not happen or miss one that did.
func (d Definition) Matches(o Occasion) bool {
	return len(d.MatchReasons(o)) > 0
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

// MatchReasons returns, for a definition that fires on the occasion, which
// specific value in each declared key satisfied it — "when:enter closed",
// "label:needs-design" — the shared "why" computation behind both the firing
// trace (a real occasion) and `lit workflows dry-run` (a hypothetical one).
// Nil when the definition does not fire, which a definition that never fires
// never does.
func (d Definition) MatchReasons(o Occasion) []string {
	keys, fires := d.Activation.Keys()
	if !fires {
		return nil
	}
	return keys.reasons(o)
}

// reasons composes the two keys: OR within a key (any listed value the
// occasion carries satisfies it) and AND across them (each written key must
// be satisfied). [LAW:dataflow-not-control-flow] the same steps run for every
// definition on every occasion; which keys bite is decided by what they list.
// [LAW:one-type-per-behavior] moments and labels are both compared by
// equality — a crossing is its side and state, an event its name — so one
// test serves both keys.
func (k Keys) reasons(o Occasion) []string {
	when, whenHolds := k.When.carried(o.Moments)
	labels, labelsHolds := k.Labels.carried(o.Labels)
	if !whenHolds || !labelsHolds {
		return nil
	}
	var reasons []string
	for _, moment := range when {
		reasons = append(reasons, "when:"+moment.String())
	}
	for _, label := range labels {
		reasons = append(reasons, "label:"+label)
	}
	return reasons
}

// carried is the key's entries among the occasion's values, in listed order,
// and whether the key holds: a key left out holds on anything, and a written
// one when the occasion carries one of its entries.
func (k Key[T]) carried(values []T) ([]T, bool) {
	var out []T
	for _, entry := range k.entries {
		if slices.Contains(values, entry) {
			out = append(out, entry)
		}
	}
	return out, len(k.entries) == 0 || len(out) > 0
}
