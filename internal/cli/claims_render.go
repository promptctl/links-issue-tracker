package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// formatClaimLine renders "who has this and how is it going" for one lane,
// per design-docs/work-claims.md's "Finding a claimant". It answers false
// for Unclaimed — the zero state carries no holder to describe, so listings
// render no line at all rather than an empty or placeholder one.
//
// Two tiers, exactly as the design spells them: the dossier (holder,
// freshness, lane progress) is built entirely from cc.evidence and
// cc.standings — the shared, synced data — so it renders identically on any
// clone. The address (path, branch) renders only when cc.addresses resolves
// the holder to a live worktree THIS machine enumerated; a remote claimant
// gets the dossier and nothing more. [privacy invariant]
func formatClaimLine(cc claimContext, lane model.LaneID, now time.Time) (string, bool) {
	var tenure claims.Tenure
	var line string
	switch standing := cc.standings.Of(lane).(type) {
	case claims.Held:
		tenure = standing.Tenure
		line = claimPrefix(tenure.By, holdFresh, cc)
		if len(standing.Contested) > 0 {
			line += fmt.Sprintf(" · contested by %s", strings.Join(nameCheckouts(standing.Contested), ", "))
		}
	case claims.Stale:
		tenure = standing.Tenure
		line = claimPrefix(tenure.By, holdKindOf(standing.Holder), cc)
	default:
		return "", false
	}
	parts := []string{line, humanizeCoarseDuration(now.Sub(tenure.LastActivity)) + " ago"}
	if progress := formatLaneProgress(cc.evidence.LaneProgress(lane)); progress != "" {
		parts = append(parts, progress)
	}
	return strings.Join(parts, " · "), true
}

// holdKind is what a claim line says this hold IS — the whole vocabulary for
// it, in one sealed set, so the badge and the parenthetical cannot come to
// disagree about a lane they are describing together. [LAW:one-source-of-truth]
//
// It replaced a bool. Two values could say fresh-or-not and nothing else, so a
// holder whose worktree sits locked on disk had to borrow the word for a holder
// who left, and "(stale)" was the only thing `lit next` could call a session
// that was still running (links-claims-2wk2).
type holdKind int

const (
	// holdFresh: the claim is inside the freshness window.
	holdFresh holdKind = iota
	// holdStale: past the window, with nothing to say the holder is still here.
	holdStale
	// holdLocked: past the window, but the holder locked its worktree — an
	// explicit do-not-disturb that outranks the clock.
	holdLocked
)

// holdKindOf reads an expired claim's badge off what this machine can still see
// of its holder. Present reads the same as Unprovable deliberately: a worktree
// that merely exists can outlive the session that made it, so its presence
// downgrades nothing on this line — it earns its keep at the ORPHANED/abandoned
// wording instead, where the claim is being called dead rather than old.
func holdKindOf(holder claims.Presence) holdKind {
	if holder == claims.Locked {
		return holdLocked
	}
	return holdStale
}

// expiredHolder is what this machine can still see of the checkout behind a
// lane's EXPIRED claim, and the one reading of a Standing the wording surfaces
// share — `lit next`'s takeover sentence and `lit backlog`'s in-progress
// suffix, which between them produced both halves of links-claims-2wk2's
// report and must not start disagreeing about one lane.
// [LAW:single-enforcer]
//
// Every standing but Stale answers Unprovable, and the word is exact rather
// than a stand-in: a held lane has no expired claim to describe, and an
// unclaimed one has no holder at all — in both cases there is nothing here this
// machine has proven about a lapsed holder, which is what Unprovable means.
func expiredHolder(standing claims.Standing) claims.Presence {
	if stale, expired := standing.(claims.Stale); expired {
		return stale.Holder
	}
	return claims.Unprovable
}

// claimPrefix is the badge half of the claim line: where a listing can walk
// over to the claimant, or, failing that, the opaque discriminator the
// shared database actually carries. kind controls only the label — an expired
// claim from a still-live local worktree still resolves to that worktree's
// address, since "go look at what it was doing" is exactly as true of a
// stale claim as a fresh one.
func claimPrefix(by model.Attribution, kind holdKind, cc claimContext) string {
	if checkout, ok := cc.addresses[by]; ok {
		branch := checkout.Branch
		if branch == "" {
			branch = "detached HEAD"
		}
		return fmt.Sprintf("claimed here%s: %s (%s)", claimTag(kind), checkout.Path, branch)
	}
	return fmt.Sprintf("claimed: %s (%s)", nameCheckout(by), holdState(by, kind))
}

// claimTag is the parenthetical an addressable holder's badge carries, and
// empty for the ordinary fresh hold — the happy path stays unadorned.
func claimTag(kind holdKind) string {
	switch kind {
	case holdLocked:
		return " (locked)"
	case holdStale:
		return " (stale)"
	}
	return ""
}

// holdState is the parenthetical half of a claim line a listing cannot walk
// over to: how much the reader can conclude about a holder they cannot address.
//
// "elsewhere" asserts the hold is not this checkout's, and only an addressable
// holder supports that. The public checkout is a bucket every unattributed
// write shares, so comparing it against this checkout's own identity
// establishes that both are unaddressable and nothing more -- it was once read
// here as proof the lane was ours, which rendered "claimed here: this checkout"
// on `lit sync`'s contested-lane report, where the identity being compared is
// the zero Attribution of an app.App built with no Stream at all rather than
// any real checkout's. Freshness is the whole of what an unaddressable holder
// can honestly report. [LAW:parse-dont-validate]
// holdLocked is answered here for totality rather than because it happens: a
// locked holder is one this machine enumerated, and enumerating it is what
// produced its address, so this arm is reached only if the two ever come apart.
// Answering it is cheaper than reasoning about why it cannot be reached.
func holdState(by model.Attribution, kind holdKind) string {
	switch {
	case kind == holdLocked:
		return "locked"
	case kind == holdStale:
		return "stale"
	case by.Present():
		return "elsewhere"
	}
	return "unaddressed"
}

// formatLaneProgress renders the "how is it going" fraction. A lane with no
// members the evidence saw (the zero LaneProgress) renders nothing — that
// shape cannot happen for a lane with a Held or Stale standing, since a
// holder implies at least one member, but the empty string keeps the
// function total rather than assuming its only caller's invariant.
func formatLaneProgress(progress claims.LaneProgress) string {
	if progress.Total == 0 {
		return ""
	}
	if progress.Active != nil {
		return fmt.Sprintf("%s in progress, %d/%d done", progress.Active.ID, progress.Done, progress.Total)
	}
	return fmt.Sprintf("%d/%d done", progress.Done, progress.Total)
}

// describeClaimant names a holder in the transfer notice, rendering the
// identity halves it actually carries: the assignee when there is one, the
// stream label when the checkout is recorded, both when both are.
//
// Both halves are shown together rather than the "better" one alone because
// either half can be the only one that moved. Two agent sessions transfer under
// distinct assignees; two worktrees of ONE session transfer under identical
// ones — and a notice that printed only the assignee would render that as
// "X -> X", saying a transfer happened while showing nothing that changed.
//
// The public checkout is the single exception, dropped when an assignee already
// names somebody. It is a bucket rather than an address, so beside a real name
// it discriminates nothing, and "alice (the public checkout) -> bob (the public
// checkout)" spends both halves on the half that did not move. Where there is
// no assignee it is the whole answer, and a better one than the "(unassigned)"
// this printed before the ruling: the record here holds an establishing event,
// so somebody demonstrably took this ticket, and "(unassigned)" describes the
// empty assignee field while saying nothing about the holder being announced.
func describeClaimant(c claims.Claimant) string {
	switch {
	case c.Assignee == "":
		return nameCheckout(c.Checkout)
	case !c.Checkout.Present():
		return c.Assignee
	}
	return fmt.Sprintf("%s (%s)", c.Assignee, nameCheckout(c.Checkout))
}

// nameCheckout names a holder for a human, and is the one place either kind of
// holder gets a name — claim lines, contest lists, and transfer notices all
// read through here rather than each deciding what an unidentified holder
// looks like. [LAW:single-enforcer]
//
// An identified checkout is named by its stream token, trimmed to a readable
// label. Truncation is a display nicety, not a privacy measure — the full token
// is already opaque and carries nothing identifying — so a shorter label is
// never wrong to show, only occasionally ambiguous against another token
// sharing the same prefix, which contested-lane rendering already disambiguates
// by listing every contestant.
//
// The public checkout (model.Attribution.Present) is named rather than
// rendered from its token, because it has none: reading its empty stream
// through the label path produced "stream " with nothing after it, an
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
	return "stream " + stream
}

func nameCheckouts(attributions []model.Attribution) []string {
	labels := make([]string, len(attributions))
	for i, a := range attributions {
		labels[i] = nameCheckout(a)
	}
	return labels
}
