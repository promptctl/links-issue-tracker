package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
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

// relationOf is the one place a Standing is read against this checkout.
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
// ceremony: the happy path pays one read of the lane's family and nothing else,
// exactly as "no confirmation, no warning, no ceremony on the happy path"
// demands. The read costs the family's size and not the store's, because a
// start asks about one lane; `next` and `backlog` route across all of them.
//
// Its result is what the start owes after Apply (transitionSpec.authorize):
// the takeover line when --take crossed a foreign hold, then the transfer
// notice when Apply reports the claim changed hands and the lane's
// standing names the predecessor as its holder, the empty string otherwise;
// and, for the statusline file, the start with its lane's count for its rollup
// progress, and the phase the start left the ticket in. A handover from anyone
// the lane does not name — nobody holds it, or somebody else does — goes
// unannounced, and the gate is the one read that knows the lane's standing. The count is that same
// reading with the written ticket in place of the one read, so it costs no
// second read, and a start that reopens a closed ticket counts it as no
// longer done.
// [LAW:one-source-of-truth]
//
// Enforcement lives here and only here per [LAW:single-enforcer]: `lit
// start` is the one command that transfers a claim, so it is the one place
// that gates the transfer.
func authorizeStart(ctx context.Context, inv Invocation, ap *app.App, issueID string, prior model.Issue, start model.Start, take bool) (afterApply, error) {
	// A takeover is worth confirming only for a start that can land. Issue.Apply
	// refuses a frozen issue and a container whoever holds the lane, so its
	// refusal answers first; gating first would send the caller to --take and
	// then to this same refusal. Store.Apply runs it again on the row it writes.
	// [LAW:single-enforcer]
	if _, err := prior.Apply(start); err != nil {
		return nil, err
	}
	family, err := ap.Store.LaneFamily(ctx, issueID)
	if err != nil {
		return nil, err
	}
	telemetry.Set(ctx, "start.lane_family_issues", len(family.Issues))
	lane, err := claims.NewLane(issueID, family)
	if err != nil {
		return nil, err
	}
	checkouts, err := gatherCheckouts(inv.Env, inv.Stdout, ap)
	if err != nil {
		return nil, err
	}
	standing := lane.Standing(checkouts.fresh, checkouts.local)
	relation := relationOf(standing, ownAttribution(ap))
	// Recorded before the gate, so a refused takeover's event still says whose
	// lane it met.
	telemetry.Set(ctx, "start.lane_held", relation != laneUnclaimed)
	var takeover string
	if relation == laneHeldForeign {
		line, ok := claimLineOf(standing, lane.Progress(), checkouts.addresses, time.Now())
		if !ok {
			return nil, fmt.Errorf("claims: %v is held by another checkout but has no claim line to show", lane.ID())
		}
		if takeover, err = confirmFreshTakeover(inv.Stdin, inv.Stdout, line, take); err != nil {
			return nil, err
		}
	}
	return func(written ticketState, handover *claims.Handover) (string, statusline.Facts) {
		transfer := transferNotice(standing, handover)
		telemetry.Set(ctx, "start.handover", handover != nil)
		telemetry.Set(ctx, "start.transfer_announced", transfer != "")
		return takeover + transfer, statusline.Facts{started(lane.ProgressWith(written.issue), written.issue), phaseRead(written)}
	}, nil
}

// confirmFreshTakeover is the deliberate act the design demands before a
// foreign hold may be overridden — never a lock, always an explicit crossing.
// --take is that crossing wherever it is passed, and the line saying so is
// returned for the start to print once Apply has landed it, so a start that
// fails after the gate announces no takeover. Without it, an interactive
// terminal is prompted and a non-interactive caller (an agent, a script, a test
// capturing output into a buffer) is refused, matching the ticket's acceptance
// line: "an agent without a TTY can take over a fresh-claimed lane only by
// passing the explicit flag." --take is read before the terminal is, so the flag
// the refusal names works at a terminal too.
// isTerminal(stdout) is the same interactivity signal openOrPrintWorkflowFile
// already uses, so a captured-stdout test never blocks on a stdin read it did
// not ask for.
//
// line is the dossier claimLineOf builds for every claim-aware surface
// (`next`, `backlog`), reused rather than re-derived. The caller fails loudly
// rather than proceeding without naming the holder: it comes here only for a
// Held standing, which claimLineOf always renders, so no line means the two
// have gone out of sync. [LAW:no-silent-failure]
func confirmFreshTakeover(stdin io.Reader, stdout io.Writer, line string, take bool) (announcement string, err error) {
	if take {
		return line + " — taking over (--take)\n", nil
	}
	if !isTerminal(stdout) {
		return "", takeoverUnconfirmedError{Message: fmt.Sprintf("%s — this lane is claimed and active; pass --take to confirm the takeover", line)}
	}
	confirmed, err := askYesNo(stdin, stdout, line+"\ntake over this lane?")
	if err != nil {
		return "", err
	}
	if !confirmed {
		return "", takeoverUnconfirmedError{Message: "takeover declined"}
	}
	return "", nil
}

// askYesNo asks question at the terminal and reports whether the answer
// starts with y. Anything else — including no answer at all — is a no, so the
// destructive side of every gate that asks is the one a person has to type.
// [LAW:single-enforcer] the takeover and the purge gates ask through here.
//
// The answer is read a byte at a time, never through a buffer, so a run that
// asks more than once — a bulk start meeting two held lanes — leaves each
// later answer on stdin for the question it answers.
func askYesNo(stdin io.Reader, stdout io.Writer, question string) (bool, error) {
	if _, err := fmt.Fprintf(stdout, "%s [y/N] ", question); err != nil {
		return false, err
	}
	var answer []byte
	for b := make([]byte, 1); ; {
		n, err := stdin.Read(b)
		if n == 1 && b[0] == '\n' {
			break
		}
		answer = append(answer, b[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(answer))), "y"), nil
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
