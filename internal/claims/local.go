package claims

import "github.com/promptctl/links-issue-tracker/internal/model"

// Presence is what this machine can prove about the checkout behind a holder.
// It is the fourth leg's answer in full, and it is four-valued because the leg
// asks two questions at once — can I check, and what did I find — and every
// pairing of those is a real state a reader acts on differently.
//
// It exists because the leg used to answer with a bool. Presence and absence
// are not one bit: "I enumerated and it is gone", "I enumerated and it is
// there", and "I cannot see that checkout at all" are three findings, and a set
// membership test can only carry two. The missing value was the expensive one —
// a present worktree read as an absent one — because the derivation had nowhere
// to put "still there, just quiet" and so called it abandoned.
// [LAW:types-are-the-program]
type Presence int

const (
	// Unprovable is a holder this machine cannot check: another clone's
	// workspace, or the public checkout, which names no worktree to go looking
	// for. It is the zero value so that the zero LocalCheckouts — a machine that
	// has enumerated nothing — answers it for everyone, and freshness alone
	// governs, which is the honest default for evidence we cannot audit.
	Unprovable Presence = iota
	// Gone is a checkout of THIS workspace that the enumeration did not list:
	// git says its working tree no longer exists. This is the one finding
	// strong enough to disprove evidence outright rather than merely age it.
	Gone
	// Present is a live working tree. It sustains nothing on its own — a tree
	// can outlive the session that made it — but it is proof that the holder
	// has not been cleaned up, which is the difference between "stale" and
	// "abandoned" and is the whole reason this value is distinguishable.
	Present
	// Locked is a live working tree its holder has run `git worktree lock` on:
	// an explicit, deliberate do-not-disturb marker. It is the one local signal
	// that speaks for the holder rather than merely about it, so it is the one
	// that carries a claim past the freshness window.
	Locked
)

// String names the finding. Presence is the subject of assertions that
// otherwise report it as the bare int it is, and "holder presence = 0, want 2"
// makes a reader decode the enum before they can read their own failure.
func (p Presence) String() string {
	switch p {
	case Gone:
		return "gone"
	case Present:
		return "present"
	case Locked:
		return "locked"
	}
	return "unprovable"
}

// LiveCheckout is one enumerated working tree reduced to what the predicate
// reads: which stream it holds, and whether it is locked.
//
// Locked is a bool and not a Presence because a checkout the enumeration
// LISTED cannot be Gone and cannot be Unprovable — those two are findings about
// the list, not entries in it. Admitting them at this seam would make an
// illegal state representable in exchange for a tidier-looking type.
// [LAW:types-are-the-program]
type LiveCheckout struct {
	Stream string
	Locked bool
}

// LocalCheckouts is what this machine can prove about its own checkouts: the
// workspace it belongs to, and what it found for each checkout of that
// workspace that still exists on disk. It is the fourth leg of the claim
// predicate — "the holder is live, as far as this machine can tell" — reduced to
// the only form that leg can honestly take.
//
// The asymmetry is deliberate. Deleting a worktree is a local fact, observable
// instantly by the machine that owns it and by nobody else, so a claim from a
// deleted checkout dies here at once and everywhere else waits out the freshness
// window. A different clone on the same machine carries a different workspace id
// and is never pruned by this one.
//
// The zero value is a machine that has enumerated nothing and therefore proves
// nothing: every holder reads Unprovable, which is precisely the "where
// uncheckable, assume live and let freshness govern" default. That the
// degenerate case is the zero value — rather than a nil callback each caller
// must remember to fill in, or a flag saying whether the callback is meaningful
// — is why no caller of Derive has an unsafe way to spell "I cannot check."
// [LAW:types-are-the-program]
//
// Enumerating the live worktrees is not this package's job (it is the liveness
// ticket's); this type is the seam that work plugs into, as data.
type LocalCheckouts struct {
	workspace string
	live      map[string]Presence
}

// NewLocalCheckouts records the live checkouts of one workspace. Callers that
// cannot enumerate pass the zero LocalCheckouts instead of calling this with a
// guess.
func NewLocalCheckouts(workspaceID string, live []LiveCheckout) LocalCheckouts {
	found := make(map[string]Presence, len(live))
	for _, checkout := range live {
		// [LAW:dataflow-not-control-flow] The lock is a value the enumeration
		// carried in, not a branch this decides; the table below is the whole
		// mapping from "what git listed" to "what the predicate reads".
		presence := Present
		if checkout.Locked {
			presence = Locked
		}
		found[checkout.Stream] = presence
	}
	return LocalCheckouts{workspace: workspaceID, live: found}
}

// PresenceOf is the one reading of an attribution against this machine's
// enumeration, and the only place the three findings are told apart. Every
// consumer — the derivation's void filter, the renderer's label, the takeover
// gate — reads its verdict rather than re-deriving one from the raw set, which
// is what kept "the clock expired" and "the holder is gone" from ever again
// being spelled the same way. [LAW:single-enforcer] [LAW:parse-dont-validate]
//
// The workspace gate runs first and answers Unprovable for everything it
// rejects, which is what keeps the zero LocalCheckouts inert: otherwise its
// empty workspace would match the public checkout's empty workspace and report
// every unattributed holder Gone on the strength of having enumerated nothing.
func (l LocalCheckouts) PresenceOf(at model.Attribution) Presence {
	if !at.Present() || at.Workspace() != l.workspace {
		return Unprovable
	}
	if presence, found := l.live[at.Stream()]; found {
		return presence
	}
	return Gone
}

// Void reports that an event's producer is a checkout this machine has proven
// absent, which makes the event evidence about a stream that no longer exists.
//
// Voiding is stronger than ignoring, and the difference is why derivation drops
// these events outright rather than merely refusing to let them hold a claim. An
// *unattributed* establishing event belongs to the public checkout, an
// unaddressable holder that still holds; reading past it to an older, attributed
// ancestor would invent a holder that the public one already superseded. A void
// event is *disproven*: we know exactly which checkout produced it and we know
// it is gone, so the lane genuinely reverts to whoever else has standing in it.
// Unaddressable stops the search; disproven falls through.
//
// Gone is the only finding that voids. Present and Locked are checkouts we can
// see, and Unprovable is the public checkout and every other clone — precisely
// the holders no machine can prove absent, whose evidence outlives every
// enumeration and is retired by freshness alone.
func (l LocalCheckouts) Void(at model.Attribution) bool {
	return l.PresenceOf(at) == Gone
}
