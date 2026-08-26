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
		line = claimPrefix(tenure.By, false, cc)
		if len(standing.Contested) > 0 {
			line += fmt.Sprintf(" · contested by %s", strings.Join(shortStreams(standing.Contested), ", "))
		}
	case claims.Stale:
		tenure = standing.Tenure
		line = claimPrefix(tenure.By, true, cc)
	default:
		return "", false
	}
	parts := []string{line, humanizeCoarseDuration(now.Sub(tenure.LastActivity)) + " ago"}
	if progress := formatLaneProgress(cc.evidence.LaneProgress(lane)); progress != "" {
		parts = append(parts, progress)
	}
	return strings.Join(parts, " · "), true
}

// claimPrefix is the badge half of the claim line: where a listing can walk
// over to the claimant, or, failing that, the opaque discriminator the
// shared database actually carries. stale controls only the label — a stale
// claim from a still-live local worktree still resolves to that worktree's
// address, since "go look at what it was doing" is exactly as true of a
// stale claim as a fresh one.
func claimPrefix(by model.Attribution, stale bool, cc claimContext) string {
	tag := ""
	if stale {
		tag = " (stale)"
	}
	if checkout, ok := cc.addresses[by]; ok {
		branch := checkout.Branch
		if branch == "" {
			branch = "detached HEAD"
		}
		return fmt.Sprintf("claimed here%s: %s (%s)", tag, checkout.Path, branch)
	}
	state := "elsewhere"
	if stale {
		state = "stale"
	}
	return fmt.Sprintf("claimed: stream %s (%s)", shortStream(by), state)
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

// shortStream trims an opaque stream token to a readable label. Truncation
// is a display nicety, not a privacy measure — the full token is already
// opaque and carries nothing identifying — so a shorter label is never wrong
// to show, only occasionally ambiguous against another token sharing the
// same prefix, which contested-lane rendering already disambiguates by
// listing every contestant.
func shortStream(a model.Attribution) string {
	const labelLen = 8
	stream := a.Stream()
	if len(stream) > labelLen {
		return stream[:labelLen]
	}
	return stream
}

func shortStreams(attributions []model.Attribution) []string {
	labels := make([]string, len(attributions))
	for i, a := range attributions {
		labels[i] = shortStream(a)
	}
	return labels
}
