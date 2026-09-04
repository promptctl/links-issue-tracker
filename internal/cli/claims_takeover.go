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

// takeoverRequirement classifies what `lit start` demands of the caller
// before it may proceed, derived purely from the target lane's standing
// against this checkout's own identity. The three cases are exactly the
// gradations design-docs/work-claims.md's "Release and abandonment" section
// draws: a lane nobody else holds (or that this checkout itself holds, fresh
// or stale) needs no ceremony; a lane whose holder has gone stale proceeds
// but must be informed; a lane someone else holds right now demands a
// deliberate act before it may be overridden. [LAW:types-are-the-program]
// the sealed set of three lives here once, so the boundary below dispatches
// on a value instead of re-deriving "is this mine, is it fresh" inline.
type takeoverRequirement int

const (
	takeoverNone takeoverRequirement = iota
	takeoverStaleInformed
	takeoverFreshConfirm
)

// laneRelation is what a lane's standing means TO THIS CHECKOUT — the single
// reading of claims.Standing that every consumer in this package shares.
//
// It exists because Stale was modeled as a sibling of Held and Unclaimed
// without carrying its own answer, so each consumer decided for itself which
// of the two it behaved like, and they decided differently: `lit start` read a
// stale lane as yours, `lit next` read it as nobody's, and the renderer sided
// with start. Four readings of one type is the under-specification the gates
// were compensating for (links-claims-1b0p, owner ruling 3). Resolving it once
// here is what lets both the takeover gate and the routing verdict below stop
// re-deriving it. [LAW:one-source-of-truth] [LAW:types-are-the-program]
type laneRelation int

const (
	// laneUnclaimed: nobody holds it and nobody is recorded as having held it.
	laneUnclaimed laneRelation = iota
	// laneOurs: this checkout holds it — fresh or stale. Staleness of your OWN
	// lane is not a loss of ownership; it is evidence you stepped away from
	// work that is still yours to pick back up.
	laneOurs
	// laneStaleForeign: another checkout held it and its evidence has aged out.
	// Available, but never silently — whoever takes it is told whose it was.
	laneStaleForeign
	// laneHeldForeign: another checkout holds it right now. Routed around.
	laneHeldForeign
)

// relationOf is the one place a Standing is read against an identity.
func relationOf(standing claims.Standing, self model.Attribution) laneRelation {
	switch s := standing.(type) {
	case claims.Held:
		if s.By == self {
			return laneOurs
		}
		return laneHeldForeign
	case claims.Stale:
		if s.By == self {
			return laneOurs
		}
		return laneStaleForeign
	default:
		return laneUnclaimed
	}
}

// classifyTakeover is the pure predicate behind the takeover gate: no I/O, no
// flags, no TTY reads — those all live in authorizeStart, the one caller.
// [LAW:effects-at-boundaries]
//
// A lane that is ours or nobody's needs no ceremony, which is why both collapse
// to takeoverNone here while staying distinct in laneRelation — routing needs
// the difference, this gate does not. [LAW:decomposition]
func classifyTakeover(standing claims.Standing, self model.Attribution) takeoverRequirement {
	switch relationOf(standing, self) {
	case laneHeldForeign:
		return takeoverFreshConfirm
	case laneStaleForeign:
		return takeoverStaleInformed
	default:
		return takeoverNone
	}
}

// authorizeStart is the boundary `lit start` calls before it writes anything.
// It derives the target lane's standing, classifies it, and — only for the
// two foreign-hold cases — enforces or prints the friction the ticket
// requires. Own lanes and unclaimed lanes take the takeoverNone branch and
// this function is a no-op past the read: the happy path pays one extra
// evidence gather and nothing else, exactly as "no confirmation, no warning,
// no ceremony on the happy path" demands.
//
// Enforcement lives here and only here per [LAW:single-enforcer]: `lit
// start` is the one command that transfers a claim, so it is the one place
// that gates the transfer.
func authorizeStart(ctx context.Context, stdout io.Writer, ap *app.App, issueID string, prior model.Issue, take bool) error {
	relations, err := ap.Store.GetRelationsByIDs(ctx, []string{issueID})
	if err != nil {
		return err
	}
	lane := model.LaneOf(prior, relations[issueID].Parent)
	cc, err := gatherClaimContext(ctx, stdout, ap)
	if err != nil {
		return err
	}
	switch classifyTakeover(cc.standings.Of(lane), cc.self) {
	case takeoverNone:
		return nil
	case takeoverStaleInformed:
		return printStaleProvenance(stdout, cc, lane)
	case takeoverFreshConfirm:
		return confirmFreshTakeover(stdout, cc, lane, take)
	default:
		// Unreachable: classifyTakeover returns only the three values above.
		// [LAW:no-silent-failure]
		return fmt.Errorf("claims: %s has no recognized takeover requirement", issueID)
	}
}

// claimLineOrPanic renders the dossier formatClaimLine already builds for
// every other claim-aware surface (`next`, `backlog`) — reused here rather
// than re-derived, per the design comment on this ticket pointing at
// claims_render.go's formatClaimLine/claimPrefix. It fails loudly rather
// than silently proceeding without provenance: classifyTakeover only reaches
// either caller when cc.standings.Of(lane) is Held or Stale, both of which
// formatClaimLine always renders a line for, so an ok=false here means the
// two functions have gone out of sync. [LAW:no-silent-failure]
func claimLineOrPanic(cc claimContext, lane model.LaneID, caller string) (string, error) {
	line, ok := formatClaimLine(cc, lane, time.Now())
	if !ok {
		return "", fmt.Errorf("claims: %s has a takeover requirement on %v but no claim line to show", caller, lane)
	}
	return line, nil
}

// printStaleProvenance is the "informed" half of the ticket: a stale foreign
// hold proceeds unprompted, but prints the dossier plus the advisory
// design-docs/work-claims.md's "Claimed with unmerged work in flight"
// paragraph requires. Checking for unmerged branches or PRs is left to the
// taking agent's judgment — lit stays ignorant of git and the forge.
func printStaleProvenance(stdout io.Writer, cc claimContext, lane model.LaneID) error {
	line, err := claimLineOrPanic(cc, lane, "stale takeover")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s — check for unmerged branches or PRs on this lane before building on it\n", line)
	return err
}

// confirmFreshTakeover is the deliberate act the design demands before a
// fresh foreign hold may be overridden — never a lock, always an explicit
// crossing. An interactive terminal is prompted directly; a non-interactive
// caller (an agent, a script, a test capturing output into a buffer) must
// already have passed --take, matching the ticket's acceptance line: "an
// agent without a TTY can take over a fresh-claimed lane only by passing the
// explicit flag." isTerminal(stdout) is the same interactivity signal
// openOrPrintWorkflowFile already uses, so a captured-stdout test never
// blocks on a stdin read it did not ask for.
func confirmFreshTakeover(stdout io.Writer, cc claimContext, lane model.LaneID, take bool) error {
	line, err := claimLineOrPanic(cc, lane, "fresh takeover")
	if err != nil {
		return err
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
