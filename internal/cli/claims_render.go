package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// formatClaimLine renders "who has this and how is it going" for one lane,
// per design-docs/work-claims.md's "Finding a claimant". It answers false
// for Unclaimed — the zero state carries no holder to describe, so listings
// render no line at all rather than an empty or placeholder one. A lane whose
// claim has expired IS Unclaimed, so it renders nothing either: there is no
// "(stale)" badge, because there is no claim left to badge.
//
// Two tiers, exactly as the design spells them: the dossier (holder,
// freshness, lane progress) is built entirely from cc.evidence and
// cc.standings — the shared, synced data — so it renders identically on any
// clone. The address (path, branch) renders only when cc.addresses resolves
// the holder to a live worktree THIS machine enumerated; a remote claimant
// gets the dossier and nothing more. [privacy invariant]
func formatClaimLine(cc claimContext, lane model.LaneID) (string, bool) {
	held, ok := cc.standings.Of(lane).(claims.Held)
	if !ok {
		return "", false
	}
	return claimLineOf(held, cc.evidence.Lane(lane), cc.addresses, cc.judgedAt), true
}

// claimLineOf is formatClaimLine for a lane already known to be held, on the
// values it reads, for a reader holding one lane and no claimContext. It takes
// the hold because a listing asks once a row, so an unclaimed lane has to cost
// nothing and counting a lane's progress has to wait for one.
func claimLineOf(held claims.Held, lane claims.Lane, addresses map[model.Attribution]workspace.Checkout, judgedAt time.Time) string {
	parts := []string{claimPrefix(held.By, addresses), activityAge(held.LastActivity, judgedAt)}
	if progress := formatLaneProgress(lane.Progress()); progress != "" {
		parts = append(parts, progress)
	}
	return strings.Join(parts, " · ")
}

// activityAge is how long before judgedAt a holder last acted, the one
// rendering of a holder's age the claim line and the hold gate share.
// [LAW:one-source-of-truth] Activity recorded after judgedAt says so: folded
// into "under a minute ago" it would read as ordinary fresh activity and hide
// the skew that put it there. [LAW:no-silent-failure]
func activityAge(lastActivity, judgedAt time.Time) string {
	if age := judgedAt.Sub(lastActivity); age >= 0 {
		return humanizeCoarseDuration(age) + " ago"
	}
	return humanizeCoarseDuration(lastActivity.Sub(judgedAt)) + " in the future"
}

// claimPrefix is the badge half of the claim line: where a listing can walk
// over to the claimant, or, failing that, the opaque discriminator the
// shared database actually carries. The address is checkoutAddress's, so a
// claim line and a hold-gate refusal name one checkout the same way.
// [LAW:one-source-of-truth]
func claimPrefix(by model.Attribution, addresses map[model.Attribution]workspace.Checkout) string {
	if _, ok := addresses[by]; ok {
		return "claimed here: " + checkoutAddress(by, addresses)
	}
	return fmt.Sprintf("claimed: %s (%s)", checkoutAddress(by, addresses), holdState(by))
}

// checkoutAddress names a holder as a reader can find it: its path and branch
// when this machine enumerated it, the opaque name otherwise. The claim line,
// the hold gate's refusal, `next`'s in-progress notice and the transfer notice
// all name a holder through here, so one checkout reads one way wherever it
// appears. [LAW:single-enforcer]
func checkoutAddress(by model.Attribution, addresses map[model.Attribution]workspace.Checkout) string {
	if checkout, ok := addresses[by]; ok {
		return fmt.Sprintf("%s (%s)", checkout.Path, branchOf(checkout))
	}
	return nameCheckout(by)
}

// branchOf names a live local checkout's branch, the one spelling the claim
// line and the hold gate share for a detached one. [LAW:one-source-of-truth]
func branchOf(checkout workspace.Checkout) model.Text {
	if checkout.Branch == "" {
		return "detached HEAD"
	}
	return checkout.Branch
}

// holdState is the parenthetical half of a claim line a listing cannot walk
// over to: how much the reader can conclude about a holder they cannot address.
//
// "elsewhere" asserts the hold is not this checkout's, and only an addressable
// holder supports that. The public checkout is a bucket every unattributed
// write shares, so comparing it against this checkout's own identity
// establishes that both are unaddressable and nothing more. Reading it as proof
// the lane is ours would render "claimed here: this checkout" where the
// identity being compared is the zero Attribution rather than any real
// checkout's. [LAW:parse-dont-validate]
func holdState(by model.Attribution) string {
	if by.Present() {
		return "elsewhere"
	}
	return "unaddressed"
}

// formatLaneProgress renders the "how is it going" fraction. A lane with no
// members the evidence saw (the zero LaneProgress) renders nothing — that
// shape cannot happen for a lane with a Held standing, since a holder implies
// at least one member, but the empty string keeps the function total rather
// than assuming its only caller's invariant.
func formatLaneProgress(progress claims.LaneProgress) string {
	if progress.Total == 0 {
		return ""
	}
	if progress.Active != nil {
		return fmt.Sprintf("%s in progress, %s", progress.Active.ID, formatProgress(progress.Progress))
	}
	return formatProgress(progress.Progress)
}

// formatProgress renders the fraction itself, the one spelling the claim line
// and the exhaustion diagnostic share. [LAW:one-source-of-truth]
func formatProgress(progress claims.Progress) string {
	return fmt.Sprintf("%d/%d done", progress.Done, progress.Total)
}

// nameCheckout names a holder for a human, and is the one place either kind of
// holder gets a name — claim lines, the hold gate's refusal, and transfer
// notices all read through here rather than each deciding what an unidentified
// holder looks like. [LAW:single-enforcer]
//
// An identified checkout is named by its stream token, trimmed to a readable
// label. Truncation is a display nicety, not a privacy measure — the full token
// is already opaque and carries nothing identifying — so a shorter label is
// never wrong to show, only occasionally ambiguous against another token
// sharing the same prefix.
//
// The public checkout (model.Attribution.Present) is named rather than
// rendered from its token, because it has none: reading its empty stream
// through the label path would produce "stream " with nothing after it, an
// answer-shaped void that says a checkout was named while naming none. Being
// unaddressable is a fact worth printing, not a gap to paper over — "the public
// checkout" tells a reader both that somebody worked here and that there is
// nobody to go ask. [LAW:parse-dont-validate]
func nameCheckout(a model.Attribution) string {
	if !a.Present() {
		return "the public checkout"
	}
	const labelLen = 8
	stream := a.Stream()
	if len(stream) > labelLen {
		stream = stream[:labelLen]
	}
	return fmt.Sprintf("stream %s", stream)
}
