package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// reportContestedLanes surfaces every lane whose merged evidence now names more
// than one live establisher — the moment design-docs/work-claims.md's
// "Distribution, races, and failure modes" says a partition-caused race first
// becomes knowable: two partitioned checkouts started the same lane, and their
// histories just met. It is the surface-only half of that section — routing is
// already decided by claims.Derive, evidence is untouched, and this call reads
// what the reconcile just committed and prints it, exactly once, per lane.
// [LAW:effects-at-boundaries] the only write already happened (the reconcile
// itself); everything here is a read.
//
// It reuses gatherClaimContext and formatClaimLine — the same evidence read and
// the same "who has this, contested by whom" rendering `next`/`backlog` already
// give the caller — so a contested lane reads identically here and there.
// [LAW:one-source-of-truth]
func reportContestedLanes(ctx context.Context, env environ.Env, stdout io.Writer, ws workspace.Info, syncStore storage.Store) error {
	cc, err := gatherClaimContext(ctx, env, stdout, &app.App{Workspace: ws, Store: syncStore})
	if err != nil {
		return err
	}
	return printContestedLanes(stdout, cc, time.Now())
}

// printContestedLanes is the report's rendering half: one line per contested
// lane in cc, each held to its row whatever its key holds, and nothing when no
// lane is contested.
func printContestedLanes(stdout io.Writer, cc claimContext, now time.Time) error {
	lanes := contestedLanes(cc.standings)
	if len(lanes) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(stdout, "contested: evidence from more than one checkout just met for these lanes —"); err != nil {
		return err
	}
	for _, lane := range lanes {
		line, ok := formatClaimLine(cc, lane, now)
		if !ok {
			// standingOf and contestedLanes agree on what "Held" means, so this
			// cannot happen; a silent skip here would hide that disagreement
			// rather than report it. [LAW:no-silent-failure]
			return fmt.Errorf("contested lane %s reported no claim line — standings and rendering disagree", lane)
		}
		if _, err := fmt.Fprintln(stdout, singleLine(fmt.Sprintf("  %s: %s", lane, line))); err != nil {
			return err
		}
	}
	return nil
}

// contestedLanes is the pure filter behind reportContestedLanes: every lane
// whose standing is Held with at least one contestant, in a stable order so the
// report reads the same across runs of the same evidence. [LAW:effects-at-boundaries]
func contestedLanes(standings claims.Standings) []model.LaneID {
	lanes := make([]model.LaneID, 0)
	for lane, standing := range standings {
		if held, ok := standing.(claims.Held); ok && len(held.Contested) > 0 {
			lanes = append(lanes, lane)
		}
	}
	slices.SortFunc(lanes, func(a, b model.LaneID) int {
		return strings.Compare(a.String(), b.String())
	})
	return lanes
}
