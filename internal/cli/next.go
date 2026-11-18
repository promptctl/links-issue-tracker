package cli

import (
	"context"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
)

// `lit next` is not a workableView preset over the shared backlog pipeline (see
// workable.go). The pick is not "the first ready row of an ordered list", it is
// a multi-step precedence over data (claim standings, this checkout's
// identity) the backlog view never reads, producing a discriminated
// NextOutcome a single-row keep()/render() signature has no way to carry.
// Stretching the shared preset to fit would tangle the two.
// [LAW:decomposition] [LAW:carrying-cost]
const nextUsage = "usage: lit next [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--all]"

func nextLeaf() appLeaf {
	fs := newCobraFlagSet("next")
	fs.Retire("continue", "--continue is retired; claim routing already serves your checkout's lanes first in `lit next` — run `lit next` with no flag")
	fs.Retire("by", "--by is retired here: this command reads no identity, because a lane belongs to the checkout — run it from the checkout that holds your work")
	filterFlags := registerWorkableFilter(fs)
	// Same flag, same meaning, same deletion condition as `lit backlog --all`:
	// route over the whole queue rather than the focused goal's path.
	// [LAW:one-source-of-truth] one name for one idea across both surfaces.
	all := fs.Bool("all", false, "Ignore the focus scope and route over the whole queue")
	return appLeaf{fs: fs, positionals: 0, usage: nextUsage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		filter, err := filterFlags.parse()
		if err != nil {
			return err
		}
		asked, err := filter.criteria()
		if err != nil {
			return err
		}
		// [LAW:single-enforcer] Same staleness warning, same position, as every
		// other ordinary read command.
		if err := printStalenessWarning(ctx, inv.Stderr, inv.Env, inv.Stdout, ap.Workspace, ap.Store, time.Now()); err != nil {
			return err
		}
		// The whole queue, not the filtered one: routing applies the filter to
		// what it may serve, and reads every row to decide it (see routeNext).
		gathered, err := gatherWorkableQueue(ctx, inv.Env, ap)
		if err != nil {
			return err
		}
		cc, err := gatherClaimContext(ctx, inv.Env, inv.Stdout, ap)
		if err != nil {
			return err
		}
		outcome := routeNext(gathered.rows, gathered.details, gathered.epics, cc.standings, cc.evidence, cc.self, gathered.scope.scopeFor(*all), asked)
		recordNextEvent(ctx, outcome, len(gathered.keepRows(asked).rows))
		ap.Statusline.Record(nextStatuslineFacts(outcome, !filter.narrows() && !*all, cc.evidence)...)
		// The text bound to `next` is the whole of this command's answer, on
		// every run: a run that hands no ticket back is an answer, not a failure.
		return dispatchOccasion(ctx, inv.Stderr, inv.Env, inv.Stdout, ap.Workspace, nextOccasion(outcome, gathered.details, cc))
	}}
}
