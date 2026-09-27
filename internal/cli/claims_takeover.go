package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// laneRelation is what a lane's standing means TO THIS CHECKOUT — the single
// reading of claims.Standing that every consumer in this package shares.
//
// It exists because a lane's standing was once read against an identity in
// four places, and they disagreed: `lit start` read an aged-out claim as
// yours, `lit next` read it as nobody's, and the renderer sided with start
// (links-claims-1b0p, owner ruling 3). Resolving the reading once here is
// what lets the takeover gate and the routing verdict consume a value instead
// of re-deriving "is this mine, is it fresh" inline.
// [LAW:one-source-of-truth] [LAW:types-are-the-program]
//
// There are three relations and not four. A lane whose claim has expired is
// laneUnclaimed, indistinguishable here from one nobody ever started, because
// claims.Standing has no variant for it: an expired claim is over, not a
// grade of claim (links-claims-y6yz). The relation that used to sit between
// these — a lapsed lane, offered as a takeover with the lapsed holder's
// provenance — is gone with the variant that carried it.
type laneRelation int

const (
	// laneUnclaimed: nobody holds it.
	laneUnclaimed laneRelation = iota
	// laneOurs: this checkout holds it right now.
	laneOurs
	// laneHeldForeign: another checkout holds it right now. Routed around.
	laneHeldForeign
)

// relationOf is the one place a Standing is read against an identity.
//
// Only a minted token can match. The public checkout is every unattributed
// writer at once, and a checkout with no token has recorded nothing, so a zero
// self equal to a zero holder proves nothing about whose lane it is.
// [LAW:single-enforcer]
func relationOf(standing claims.Standing, self model.Attribution) laneRelation {
	held, ok := standing.(claims.Held)
	if !ok {
		return laneUnclaimed
	}
	if self.Present() && held.By == self {
		return laneOurs
	}
	return laneHeldForeign
}

// authorizeStart is the boundary `lit start` calls before it writes anything.
// It derives the target lane's standing and, only when another checkout holds
// the lane right now, enforces the deliberate act the design demands before a
// live claim may be overridden. Every other lane — unclaimed, this checkout's
// own, or one whose claim has expired, which is unclaimed — passes with no
// ceremony: the happy path pays one extra evidence gather and nothing else,
// exactly as "no confirmation, no warning, no ceremony on the happy path"
// demands.
//
// It reports whether anybody held the lane, ours or another's, so the
// transfer notice can say "claim transferred" only where a claim existed to
// transfer (transitionSpec.authorize).
//
// Enforcement lives here and only here per [LAW:single-enforcer]: `lit
// start` is the one command that transfers a claim, so it is the one place
// that gates the transfer.
func authorizeStart(ctx context.Context, stdout io.Writer, ap *app.App, issueID string, prior model.Issue, take bool) (laneHeld bool, err error) {
	relations, err := ap.Store.GetRelationsByIDs(ctx, []string{issueID})
	if err != nil {
		return false, err
	}
	lane := model.LaneOf(prior, relations[issueID].Parent)
	cc, err := gatherClaimContext(ctx, stdout, ap)
	if err != nil {
		return false, err
	}
	relation := relationOf(cc.standings.Of(lane), cc.self)
	if relation == laneHeldForeign {
		if err := confirmFreshTakeover(stdout, cc, lane, take); err != nil {
			return false, err
		}
	}
	return relation != laneUnclaimed, nil
}

// confirmFreshTakeover is the deliberate act the design demands before a
// foreign hold may be overridden — never a lock, always an explicit crossing.
// An interactive terminal is prompted directly; a non-interactive caller (an
// agent, a script, a test capturing output into a buffer) must already have
// passed --take, matching the ticket's acceptance line: "an agent without a
// TTY can take over a fresh-claimed lane only by passing the explicit flag."
// isTerminal(stdout) is the same interactivity signal openOrPrintWorkflowFile
// already uses, so a captured-stdout test never blocks on a stdin read it did
// not ask for.
//
// The claim line is the dossier formatClaimLine already builds for every
// other claim-aware surface (`next`, `backlog`), reused rather than re-derived.
// It fails loudly rather than proceeding without naming the holder: the caller
// reaches here only for a Held standing, which formatClaimLine always renders,
// so ok=false means the two have gone out of sync. [LAW:no-silent-failure]
func confirmFreshTakeover(stdout io.Writer, cc claimContext, lane model.LaneID, take bool) error {
	line, ok := formatClaimLine(cc, lane, time.Now())
	if !ok {
		return fmt.Errorf("claims: %v is held by another checkout but has no claim line to show", lane)
	}
	if !isTerminal(stdout) {
		if !take {
			return fmt.Errorf("%s — this lane is claimed and active; pass --take to confirm the takeover", line)
		}
		_, err := fmt.Fprintf(stdout, "%s — taking over (--take)\n", line)
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s\ntake over this lane? [y/N] ", line); err != nil {
		return err
	}
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read takeover confirmation: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
		return fmt.Errorf("takeover declined")
	}
	return nil
}
