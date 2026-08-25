package app

import (
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// LocalCheckouts is what this machine can prove about its own checkouts, in the
// form claim derivation consumes: this workspace's id, paired with the tokens of
// the checkouts of this workspace that still exist on disk.
//
// It lives at this boundary and not inside internal/claims, and the placement is
// load-bearing rather than tidy. That package imports only internal/model and
// touches no clock, no store, and no filesystem, which is what makes "deriving a
// claim writes nothing" a fact about its shape instead of a promise about its
// behavior — internal/store/claims_readonly_test.go holds the database to it.
// Enumerating worktrees is a filesystem effect; performing it here and handing
// down a value keeps the effect at the edge where every other one lives.
// [LAW:effects-at-boundaries]
//
// The workspace id is the scope, and it is load-bearing for privacy as much as
// for correctness: a different clone of the same repository on this same machine
// carries a different workspace id, so this enumeration never speaks to its
// claims — they age out here exactly as a remote machine's do.
//
// A caller that cannot enumerate must pass the zero claims.LocalCheckouts, which
// voids nothing and leaves freshness to govern — never a guess assembled from a
// partial answer. That is why the error arm returns the zero value and the
// error: this reports what it proved, or it reports that it proved nothing.
// [LAW:no-silent-failure]
func (a *App) LocalCheckouts() (claims.LocalCheckouts, error) {
	checkouts, err := workspace.LiveCheckouts(a.Workspace.RootDir)
	if err != nil {
		return claims.LocalCheckouts{}, err
	}
	return claims.NewLocalCheckouts(a.Workspace.WorkspaceID, streamTokens(checkouts)), nil
}

// streamTokens projects enumerated checkouts onto the tokens claim derivation
// compares evidence against.
//
// Checkouts that have never mutated carry no token and contribute none. They are
// live, and they hold no claim either — a checkout produces its first token and
// its first work event in the same command — so their absence from this set
// voids nothing.
//
// The filter is not a backstop against a bug: model.Attribution collapses a
// half pair to the absent one, so an empty token could never reach the live set
// through a lookup anyway. It is here because the set means "the tokens of the
// live checkouts", and "" is not one — a member that stands for no checkout
// makes the set's own description false for whatever reads it next.
// [LAW:types-are-the-program]
func streamTokens(checkouts []workspace.Checkout) []string {
	tokens := make([]string, 0, len(checkouts))
	for _, checkout := range checkouts {
		if !checkout.Stream.Present() {
			continue
		}
		tokens = append(tokens, checkout.Stream.Value())
	}
	return tokens
}
