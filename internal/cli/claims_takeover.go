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
	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// laneRelation is what a lane's standing means TO THIS CHECKOUT — the single
// reading of claims.Standing that every consumer in this package shares.
//
// Read against an identity in several places, a lane's standing can disagree
// with itself. Resolving the reading once here is what lets the takeover gate
// and the routing verdict consume a value instead of re-deriving "is this
// mine, is it fresh" inline.
// [LAW:one-source-of-truth] [LAW:types-are-the-program]
//
// There are three relations and not four. A lane whose claim has expired is
// laneUnclaimed, indistinguishable here from one nobody ever started, because
// claims.Standing has no variant for it: an expired claim is over, not a
// grade of claim.
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
// Its result is what the start owes after Apply (transitionSpec.authorize):
// the transfer notice when the lane was held, ours or another's, and the
// claimant changes hands, the empty string otherwise; and the lane's count for
// the statusline file. A lane nobody holds has no claim to transfer, so the
// notice is not even asked for there — the gate is the one read that knows the
// lane's standing, and asking transferNotice to re-derive it would gather every
// event a second time. The count is that same evidence with the written ticket
// in place of the one read, so it costs no second gather either, and a start
// that reopens a closed ticket counts it as no longer done.
// [LAW:one-source-of-truth]
//
// Enforcement lives here and only here per [LAW:single-enforcer]: `lit
// start` is the one command that transfers a claim, so it is the one place
// that gates the transfer.
func authorizeStart(ctx context.Context, stdout io.Writer, ap *app.App, issueID string, prior model.Issue, start model.Start, take bool) (afterApply, error) {
	// A takeover is worth confirming only for a start that can land. Issue.Apply
	// refuses a frozen issue and a container whoever holds the lane, so its
	// refusal answers first; gating first would send the caller to --take and
	// then to this same refusal. Store.Apply runs it again on the row it writes.
	// [LAW:single-enforcer]
	if _, err := prior.Apply(start); err != nil {
		return nil, err
	}
	relations, err := ap.Store.GetRelationsByIDs(ctx, []string{issueID})
	if err != nil {
		return nil, err
	}
	lane := model.LaneOf(prior, relations[issueID].Parent)
	cc, err := gatherClaimContext(ctx, stdout, ap)
	if err != nil {
		return nil, err
	}
	notice, err := gateTakeover(ctx, stdout, ap, issueID, start, take, cc, lane)
	if err != nil {
		return nil, err
	}
	return func(written model.Issue) (string, statusline.Facts) {
		return notice, statusline.Facts{laneCounted(written.ID, cc.evidence.LaneProgressWith(lane, written))}
	}, nil
}

// gateTakeover confirms a takeover when another checkout holds the lane, and
// returns the transfer notice the start owes.
func gateTakeover(ctx context.Context, stdout io.Writer, ap *app.App, issueID string, start model.Start, take bool, cc claimContext, lane model.LaneID) (string, error) {
	switch relationOf(cc.standings.Of(lane), cc.self) {
	case laneHeldForeign:
		if err := confirmFreshTakeover(stdout, cc, lane, take); err != nil {
			return "", err
		}
	case laneUnclaimed:
		return "", nil
	}
	return transferNotice(ctx, ap, issueID, start)
}

// confirmFreshTakeover is the deliberate act the design demands before a
// foreign hold may be overridden — never a lock, always an explicit crossing.
// --take is that crossing wherever it is passed. Without it, an interactive
// terminal is prompted and a non-interactive caller (an agent, a script, a test
// capturing output into a buffer) is refused, matching the ticket's acceptance
// line: "an agent without a TTY can take over a fresh-claimed lane only by
// passing the explicit flag." --take is read before the terminal is, so the flag
// the refusal names works at a terminal too.
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
	if take {
		_, err := fmt.Fprintf(stdout, "%s — taking over (--take)\n", line)
		return err
	}
	if !isTerminal(stdout) {
		return takeoverUnconfirmedError{Message: fmt.Sprintf("%s — this lane is claimed and active; pass --take to confirm the takeover", line)}
	}
	confirmed, err := askYesNo(stdout, line+"\ntake over this lane?")
	if err != nil {
		return err
	}
	if !confirmed {
		return takeoverUnconfirmedError{Message: "takeover declined"}
	}
	return nil
}

// askYesNo asks question at the terminal and reports whether the answer
// starts with y. Anything else — including no answer at all — is a no, so the
// destructive side of every gate that asks is the one a person has to type.
// [LAW:single-enforcer] the takeover and the purge gates ask through here.
func askYesNo(stdout io.Writer, question string) (bool, error) {
	if _, err := fmt.Fprintf(stdout, "%s [y/N] ", question); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y"), nil
}

// takeoverUnconfirmedError is the gate's answer when a live foreign hold was
// not crossed: no --take off a terminal, or a "no" at the prompt. Both are the
// gate working, not a fault, so the type carries that to the sinks, which name
// --take instead of the default advice to retry and run `lit doctor`. One type
// for both arms: what each says differs, the act that clears them — --take —
// does not. [LAW:one-type-per-behavior]
type takeoverUnconfirmedError struct {
	Message string
}

func (e takeoverUnconfirmedError) Error() string { return e.Message }
