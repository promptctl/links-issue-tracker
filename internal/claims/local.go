package claims

import "github.com/promptctl/links-issue-tracker/internal/model"

// LocalCheckouts is what this machine can prove about its own checkouts: the
// workspace it belongs to, and the stream tokens of the checkouts of that
// workspace that still exist on disk. It is the fourth leg of the claim
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
// nothing: it voids no evidence, which is precisely the "where uncheckable,
// assume live and let freshness govern" default. That the degenerate case is the
// zero value — rather than a nil callback each caller must remember to fill in,
// or a flag saying whether the callback is meaningful — is why no caller of
// Derive has an unsafe way to spell "I cannot check."
// [LAW:types-are-the-program]
//
// Enumerating the live worktrees is not this package's job (it is the liveness
// ticket's); this type is the seam that work plugs into, as data.
type LocalCheckouts struct {
	workspace string
	live      map[string]struct{}
}

// NewLocalCheckouts records the live checkouts of one workspace by their stream
// tokens. Callers that cannot enumerate pass the zero LocalCheckouts instead of
// calling this with a guess.
func NewLocalCheckouts(workspaceID string, liveStreams []string) LocalCheckouts {
	live := make(map[string]struct{}, len(liveStreams))
	for _, stream := range liveStreams {
		live[stream] = struct{}{}
	}
	return LocalCheckouts{workspace: workspaceID, live: live}
}

// Void reports that an event's producer is a checkout this machine has proven
// absent, which makes the event evidence about a stream that no longer exists.
//
// Voiding is stronger than ignoring, and the difference is why derivation drops
// these events outright rather than merely refusing to let them hold a claim. An
// *unattributed* establishing event is unknown — somebody took this lane and the
// record does not say who — and reading past it to an older, attributed ancestor
// would invent a holder that the unknown event already superseded. A void event
// is *disproven*: we know exactly which checkout produced it and we know it is
// gone, so the lane genuinely reverts to whoever else has standing in it. Unknown
// stops the search; disproven falls through.
//
// An event with no attribution at all names no checkout and so can never be
// proven absent — which is also what keeps the zero LocalCheckouts inert, since
// otherwise its empty workspace would match an absent pair's empty workspace.
func (l LocalCheckouts) Void(at model.Attribution) bool {
	if !at.Present() || at.Workspace() != l.workspace {
		return false
	}
	_, alive := l.live[at.Stream()]
	return !alive
}
