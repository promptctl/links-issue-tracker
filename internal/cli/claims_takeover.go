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
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// laneRelation is what a lane's standing means TO THIS HOLDER — the single
// reading of claims.Standing that every consumer in this package shares.
//
// Read against an identity in several places, a lane's standing can disagree
// with itself. Resolving the reading once here is what lets the hold gate and
// the routing verdict consume a value instead of re-deriving "is this mine, is
// it fresh" inline.
// [LAW:one-source-of-truth] [LAW:types-are-the-program]
//
// An expired claim is not a relation of its own. A lane whose claim has expired is
// laneUnclaimed, indistinguishable here from one nobody ever started, because
// claims.Standing has no variant for it: an expired claim is over, not a
// grade of claim.
type laneRelation int

const (
	// laneUnclaimed: nobody holds it.
	laneUnclaimed laneRelation = iota
	// laneOurs: this session, in this checkout, holds it right now.
	laneOurs
	// laneHeldForeign: another session or another checkout holds it right
	// now. Routed around, and acted on only through --take.
	laneHeldForeign
)

// relationOf is the one place a Standing is read against the reader.
//
// A lane is ours only when the hold names this checkout AND this session. A
// second session in the same tree, or in a worktree of it, is a different
// holder, which is what keeps one in-flight ticket in one session's hands.
//
// The match is plain equality of the pair. The zero session is a value like
// any other: a shell that names no identity, in the checkout it started work
// from, holds its own work, and a session-bearing caller meets that hold as
// foreign, as the shell meets a session's. The checkout half alone must be
// identified: the public checkout is every writer with no token at once, so a
// zero checkout equal to a zero holder proves nothing about whose lane it is.
// Nothing else — no assignee, no liveness probe — enters the decision.
// [LAW:single-enforcer]
func relationOf(standing claims.Standing, self claims.Holder) laneRelation {
	held, ok := standing.(claims.Held)
	if !ok {
		return laneUnclaimed
	}
	if self.Checkout.Present() && held.By == self {
		return laneOurs
	}
	return laneHeldForeign
}

// holdReading is what the hold gate read about the target's lane, handed to
// the transition it gated: the lane, and its standing at the gate. A start
// reads the standing again for its transfer notice and the lane for its rollup
// count, so neither is derived twice. [LAW:one-source-of-truth]
type holdReading struct {
	lane      claims.Lane
	standing  claims.Standing
	addresses map[model.Attribution]workspace.Checkout
}

// heldAuthorizeFunc is the authorize hook of a transition the hold gate
// guards, run after the gate with what it read.
type heldAuthorizeFunc = func(ctx context.Context, inv Invocation, ap *app.App, issueID model.ID, prior ticketState, action model.Action, hold holdReading) (afterApply, error)

// withHoldGate is the one wiring of the hold gate onto a transition: it
// registers the single --take flag, runs authorizeHold ahead of the
// transition's own hook, and prints the takeover line ahead of whatever that
// hook owes. `start`, `phase` and `done` — the verbs that act on work in
// flight — are built through it, so the three cannot disagree about who may
// act or what crossing a hold takes. [LAW:single-enforcer]
func withHoldGate(inner heldAuthorizeFunc) func(*cobraFlagSet) authorizeFunc {
	return func(fs *cobraFlagSet) authorizeFunc {
		take := fs.Bool("take", false, "Confirm acting on a ticket whose lane another session holds right now (required for non-interactive callers; without it an interactive terminal is prompted instead)")
		// One enumeration of this machine's checkouts per invocation, made the
		// first time a gate needs it: `lit bulk` gates every id against the
		// same machine, and the enumeration is a subprocess.
		var gathered *checkoutFacts
		return func(ctx context.Context, inv Invocation, ap *app.App, issueID model.ID, prior ticketState, action model.Action, actor string) (model.Action, afterApply, error) {
			checkouts := func() (checkoutFacts, error) {
				if gathered == nil {
					facts, err := gatherCheckouts(inv.Env, inv.Stdout, ap)
					if err != nil {
						return checkoutFacts{}, err
					}
					gathered = &facts
				}
				return *gathered, nil
			}
			action, hold, takeover, err := authorizeHold(ctx, inv, ap, issueID, prior, action, actor, *take, checkouts)
			if err != nil {
				return nil, nil, err
			}
			owed, err := inner(ctx, inv, ap, issueID, prior, action, hold)
			if err != nil {
				return nil, nil, err
			}
			return action, func(written ticketState, handover *claims.Handover) (string, statusline.Facts) {
				notice, facts := owed(written, handover)
				return takeover + notice, facts
			}, nil
		}
	}
}

// authorizeHold is the boundary a transition on held work calls before it
// writes anything. It derives the target lane's standing and, only when a
// holder other than this one holds the lane right now, demands the deliberate
// act before the transition may proceed. Every other lane — unclaimed, this
// holder's own, or one whose claim has expired, which is unclaimed — passes
// with no ceremony: when this holder was the lane's last taker, the gate pays
// one read of the lane's family and nothing else, with no config and no
// enumeration of the machine's checkouts. The read stays off the store's size,
// because a transition asks about one lane; `next` and `backlog` route across
// all of them.
//
// The self it reads against is this checkout and the session the transition
// is recorded under — actor, the resolved identity Store.Apply stamps on the
// event — so "ours" here is exactly the holder the write will name.
// [LAW:one-source-of-truth]
//
// takeover is the line owed once Apply lands when --take crossed a foreign
// hold, so a transition that fails after the gate announces no takeover.
// The action returned is the one Apply writes: a start on a ticket whose own record
// already names this holder, in a lane somebody took since, comes back as a
// reclaim, so taking the lane back records the event that does it.
func authorizeHold(ctx context.Context, inv Invocation, ap *app.App, issueID model.ID, prior ticketState, action model.Action, actor string, take bool, gather func() (checkoutFacts, error)) (model.Action, holdReading, string, error) {
	// A takeover is worth confirming only for a transition that can land. The
	// issue's own rules refuse a frozen issue, a container, and a phase move
	// off in-progress work whoever holds the lane, so their refusal answers
	// first; gating first would send the caller to --take and then to this
	// same refusal. Store.Apply runs them again on the row it writes.
	// [LAW:single-enforcer]
	//
	// A transition that will record nothing crosses no hold, so it is not
	// gated: demanding --take for it, or announcing one, would describe an act
	// that never happens. A start is the exception the ticket's own record
	// cannot see: its lane may have been taken through a sibling since, and
	// only the lane read below can tell.
	self := ownHolder(ap, actor)
	records, err := landable(prior, action, self)
	if err != nil {
		return nil, holdReading{}, "", err
	}
	start, starts := action.(model.Start)
	if !records && !starts {
		return action, holdReading{}, "", nil
	}
	family, err := ap.Store.LaneFamily(ctx, issueID)
	if err != nil {
		return nil, holdReading{}, "", err
	}
	verb := action.Name().Verb()
	telemetry.Set(ctx, verb+".lane_family_issues", len(family.Issues))
	lane, err := claims.NewLane(issueID, family)
	if err != nil {
		return nil, holdReading{}, "", err
	}
	// This holder took the lane last, so nobody can hold it but this holder:
	// the transition is ours, and needs neither the clock nor the machine's
	// checkouts to say so. The public checkout is every unattributed writer at
	// once, so it is never ours here, as relationOf never answers it ours.
	taker, taken := lane.LastTaker()
	if taken && self.Checkout.Present() && taker == self {
		telemetry.Set(ctx, verb+".lane_held", true)
		return action, holdReading{lane: lane}, "", nil
	}
	// A start the ticket's record calls a repeat is owed a write only when the
	// lane went to somebody else since; the public checkout re-running its own
	// start took nothing from anyone.
	if starts && !records && !(taken && taker == self) {
		start.Reclaim = true
		action = start
	}
	checkouts, err := gather()
	if err != nil {
		return nil, holdReading{}, "", err
	}
	standing := lane.Standing(checkouts.fresh, checkouts.local)
	relation := relationOf(standing, self)
	// Recorded before the gate, so a refused takeover's event still says whose
	// lane it met.
	telemetry.Set(ctx, verb+".lane_held", relation != laneUnclaimed)
	takeover := ""
	if relation == laneHeldForeign {
		// relationOf answers laneHeldForeign for a Held standing and nothing else.
		line := heldByLine(issueID, standing.(claims.Held), checkouts.addresses, time.Now())
		if takeover, err = confirmFreshTakeover(inv.Stdin, inv.Stdout, line, crossingOf(action.Name()), take); err != nil {
			return nil, holdReading{}, "", err
		}
	}
	return action, holdReading{lane: lane, standing: standing, addresses: checkouts.addresses}, takeover, nil
}

// landable runs the issue's own refusal of action on the state the gate read —
// Issue.PlanPhase for a phase move, claims.PlanStatus for a status action, the
// same plans both engines run inside Apply — and reports whether the
// transition records anything when taken by self.
func landable(prior ticketState, action model.Action, self claims.Holder) (records bool, err error) {
	switch action := action.(type) {
	case model.SetPhase:
		move, err := prior.issue.PlanPhase(prior.events, action.Phase)
		return move.Changed(), err
	case model.StatusAction:
		plan, err := claims.PlanStatus(prior.issue, prior.events, action, self)
		if err != nil {
			return false, err
		}
		return plan.Records(), nil
	}
	// withHoldGate wraps only start, phase and done, so anything else is a
	// wiring error, not a caller's mistake. [LAW:no-silent-failure]
	return false, fmt.Errorf("hold gate: %T is neither a status action nor a phase move", action)
}

// heldByLine names who holds a ticket the caller is about to act on: the
// session, the checkout — its path and branch when this machine can address
// it — and how recently that holder last acted in the lane.
func heldByLine(issueID model.ID, held claims.Held, addresses map[model.Attribution]workspace.Checkout, now time.Time) string {
	return fmt.Sprintf("%s is held by %s (active %s ago)", issueID, nameHolder(held.By, addresses), humanizeCoarseDuration(now.Sub(held.LastActivity)))
}

// authorizeStart is what `lit start` owes once the hold gate has passed: the
// transfer notice when Apply reports the claim changed hands and the lane's
// standing names the predecessor as its holder, the empty string otherwise;
// and, for the statusline file, the start with its lane's count for its rollup
// progress, and the phase the start left the ticket in. A handover from anyone
// the lane does not name — nobody holds it, or somebody else does — goes
// unannounced, and the gate is the one read that knows the lane's standing.
// The count is that same reading with the written ticket in place of the one
// read, so it costs no second read, and a start that reopens a closed ticket
// counts it as no longer done. [LAW:one-source-of-truth]
func authorizeStart(ctx context.Context, _ Invocation, _ *app.App, _ model.ID, _ ticketState, action model.Action, hold holdReading) (afterApply, error) {
	return func(written ticketState, handover *claims.Handover) (string, statusline.Facts) {
		transfer := transferNotice(hold.standing, handover, hold.addresses)
		telemetry.Set(ctx, "start.handover", handover != nil)
		telemetry.Set(ctx, "start.transfer_announced", transfer != "")
		return transfer, statusline.Facts{started(hold.lane.ProgressWith(written.issue), written.issue), phaseRead(written)}
	}, nil
}

// confirmFreshTakeover is the deliberate act the design demands before a
// foreign hold may be crossed — never a lock, always an explicit crossing.
// --take is that crossing wherever it is passed, and the line saying so is
// returned for the transition to print once Apply has landed it, so a
// transition that fails after the gate announces no takeover. Without it, an
// interactive terminal is prompted and a non-interactive caller (an agent, a
// script, a test capturing output into a buffer) is refused: "an agent without
// a TTY can take over a fresh-claimed lane only by passing the explicit flag."
// --take is read before the terminal is, so the flag the refusal names works at
// a terminal too. interactive is the same signal openOrPrintWorkflowFile uses,
// so a captured-stdout test never blocks on a stdin read it did not ask for.
//
// line is heldByLine's naming of the holder; how is what crossing it does.
func confirmFreshTakeover(stdin io.Reader, stdout io.Writer, line string, how crossing, take bool) (announcement string, err error) {
	if take {
		return line + how.announce, nil
	}
	if !interactive(stdin, stdout) {
		return "", takeoverUnconfirmedError{Message: line + how.refusal}
	}
	confirmed, err := askYesNo(stdin, stdout, line+how.ask)
	if err != nil {
		return "", err
	}
	if !confirmed {
		return "", takeoverUnconfirmedError{Message: "takeover declined"}
	}
	return "", nil
}

// crossing is what --take does to a hold, in the words the gate says it with.
// An establishing verb (claims.Establishes) makes the caller the holder, so it
// takes the lane over; a phase move records work on the ticket and leaves the
// hold where it was, so it acts on held work and says so rather than claiming
// a transfer that never happens.
type crossing struct {
	announce, refusal, ask string
}

var (
	takesOver = crossing{
		announce: " — taking over (--take)\n",
		refusal:  " — pass --take to take it over",
		ask:      "\ntake it over?",
	}
	actsOnHeld = crossing{
		announce: " — acting on it without taking it over (--take)\n",
		refusal:  " — pass --take to act on it without taking it over",
		ask:      "\nact on it without taking it over?",
	}
)

func crossingOf(verb model.ActionName) crossing {
	if claims.Establishes(verb) {
		return takesOver
	}
	return actsOnHeld
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
