package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// `lit next` used to be one more workableView preset over the shared
// backlog/next pipeline (see workable.go): order, keep the first ready row,
// render. Claim routing broke that shape — the pick is no longer "the first
// ready row of an ordered list," it is a multi-step precedence over data
// (claim standings, this checkout's identity) the backlog view never reads,
// producing a discriminated NextOutcome the old single-row keep()/render()
// signature has no way to carry. Forking next out of workableView is the
// honest move once the shapes diverge (backlog stays exactly what it was);
// stretching the shared preset to fit would have re-tangled the two.
// [LAW:decomposition] [LAW:carrying-cost]
const nextUsage = "usage: lit next [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--all]"

func nextLeaf() appLeaf {
	fs := newCobraFlagSet("next")
	assignee := fs.String("assignee", "", "Filter by assignee")
	issueType := fs.String("type", "", "Filter by issue type")
	status := fs.String("status", "", "Filter by status: open|in_progress")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	// Same flag, same meaning, same deletion condition as `lit backlog --all`:
	// route over the whole queue rather than the focused goal's path.
	// [LAW:one-source-of-truth] one name for one idea across both surfaces.
	all := fs.Bool("all", false, "Ignore the focus scope and route over the whole queue")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if fs.NArg() != 0 {
			return UsageError{Message: nextUsage}
		}
		statusState, err := parseWorkableStatus(*status)
		if err != nil {
			return err
		}
		issueTypeValue, err := parseWorkableType(*issueType)
		if err != nil {
			return err
		}
		// [LAW:single-enforcer] Same staleness warning, same position, as every
		// other ordinary read command (links-sync-pgct.2).
		if err := printSyncStalenessWarning(ctx, stdout, ap.Workspace, ap.Store, time.Now()); err != nil {
			return err
		}
		rows, details, focus, err := gatherWorkableAnnotated(ctx, ap, workableFilter{
			Assignee:  strings.TrimSpace(*assignee),
			IssueType: issueTypeValue,
			Status:    statusState,
			Labels:    splitCSV(*labels),
		})
		if err != nil {
			return err
		}
		cc, err := gatherClaimContext(ctx, stdout, ap)
		if err != nil {
			return err
		}
		occasion, err := renderNextOutcome(stdout, routeNext(rows, details, cc.standings, cc.self, focus.scopeFor(*all)), details, cc)
		if err != nil {
			return err
		}
		return workflows.Dispatch(stdout, os.Stderr, ap.Workspace, occasion)
	}}
}

// renderNextOutcome prints the row routeNext selected — or, for Exhausted
// and NoWork, returns the loud diagnostic instead of printing a ticket that
// was never picked. A claim this pick WOULD establish (EpicLane, NewLane) is
// named above the row, so the commitment is visible before it is made
// (design-docs/work-claims.md, Routing step 4); a lane already held names
// nothing to commit — nothing at all for ServedFromClaim, which prints exactly
// as `next` always has, and the state it is already in for ResumedOwnWork,
// since being handed back a ticket already in flight is the one pick that looks
// like a fresh start but is not one.
//
// Every line here is in the conditional or reports a state that already holds.
// `lit next` claims nothing and starts nothing — `lit start` does — so a line
// in the perfect tense would be reporting a side effect this command does not
// have, which is exactly how an agent came to believe it held a claim it did
// not (links-next-output-5aee).
func renderNextOutcome(w io.Writer, outcome NextOutcome, details map[string]storage.IssueRelations, cc claimContext) (workflows.Occasion, error) {
	var row annotation.AnnotatedIssue
	var announce string
	switch o := outcome.(type) {
	case ServedFromClaim:
		row = o.Row
	case ResumedOwnWork:
		row = o.Row
		announce = fmt.Sprintf("%s is already in progress in a lane you hold — continue where you left off\n", o.Row.ID)
	case ServedFromEpicLane:
		row = o.Row
		announce = startAdvice(o.Row, o.Lane, expiredHolder(cc.standings.Of(o.Lane))) + " (a second lane of an epic you already hold a lane in)\n"
	case ServedFromNewLane:
		row = o.Row
		announce = startAdvice(o.Row, o.Lane, expiredHolder(cc.standings.Of(o.Lane))) + "\n"
	// The two terminal outcomes travel outward AS THEMSELVES. Rendering them
	// into an untyped error here discarded the very discriminator routing had
	// just established, so both sinks — ExitCode and commandErrorReason — fell
	// through to "command_failed", whose remediation told the agent to retry a
	// deterministic answer and then run `lit doctor` on a healthy workspace
	// (links-cli-cpou). That is the second instance of the class register.go's
	// resolve fixed for command paths; see the citation there.
	// [LAW:types-are-the-program] classification is carried by the type, never
	// re-derived from the message.
	case Exhausted:
		return workflows.Occasion{}, o
	case NoWork:
		return workflows.Occasion{}, o
	default:
		panic(fmt.Sprintf("renderNextOutcome: unhandled NextOutcome %T", outcome))
	}
	if announce != "" {
		if _, err := io.WriteString(w, announce); err != nil {
			return workflows.Occasion{}, err
		}
	}
	lane := model.LaneOf(row.Issue, details[row.ID].Parent)
	if err := printNextSummary(w, row, cc, lane); err != nil {
		return workflows.Occasion{}, err
	}
	return nextPulledOccasion(row.Issue), nil
}

// inFlightState is the clause naming what happened to the work a takeover pick
// would inherit — the one word of that sentence that turns on evidence rather
// than on grammar, which is why it is a value the sentences interpolate instead
// of a fifth and sixth sentence beside them. [LAW:dataflow-not-control-flow]
//
// "abandoned" is a claim about the holder, and the routing that reaches here
// used to make it on the strength of a clock alone. Against a worktree this
// machine can still see, it is not merely alarming but false, and it was
// telling agents to take lanes whose owning session was running — the pick it
// was reported on had its holder locked on an open PR (links-claims-2wk2). The
// command is still offered in every case: what this changes is what the reader
// is told they are taking, never whether they may.
func inFlightState(holder claims.Presence) string {
	switch holder {
	case claims.Locked:
		return "claimed by a locked worktree whose claim has gone stale"
	case claims.Present:
		return "stale, though its holder's worktree is still on disk"
	}
	return "abandoned"
}

// startAdvice is the line every pick that would establish a claim prints above
// its row: what running `lit start` would lock, never what this command did.
// It was `claimAnnouncement`, and the rename is the fix — an announcement
// reports, and reporting is the one thing a read-only command must not do.
//
// The verb turns on the row's own lifecycle state: routing admits an
// in-progress row into a lane this checkout does not hold only once the orphan
// annotation has proven its holder's claim self-refuting (capacityFor), so
// plain "claim" would promise greenfield on a ticket that may carry another
// checkout's unmerged working tree.
//
// The object turns on the lane's shape, which is why Describe answers in two
// parts. A solo lane IS the ticket, so naming it spelled the same id twice
// ("starting X claims X") — a tautology no reader could take as advice about a
// command they had yet to run. The pronoun is the caller's answer to that,
// available here and nowhere else because the ticket is named one clause
// earlier.
//
// The two verbs spell their sentences out rather than sharing one with the
// object substituted, because English puts the pronoun in different places:
// "claim it", but "take it over" — a particle verb splits around a pronoun and
// reads wrong with one trailing it. next_route_test.go pins all six cells of
// verb and lane shape across these four sentences: a named lane and an epic's
// default lane differ only in the words Describe hands back.
func startAdvice(row annotation.AnnotatedIssue, lane model.LaneID, holder claims.Presence) string {
	described, named := lane.Describe()
	if row.State() == model.StateInProgress {
		state := inFlightState(holder)
		if !named {
			return fmt.Sprintf("%s is in progress and %s — run `lit start %s` to take it over", row.ID, state, row.ID)
		}
		return fmt.Sprintf("%s is in progress and %s — run `lit start %s` to take over %s", row.ID, state, row.ID, described)
	}
	if !named {
		return fmt.Sprintf("run `lit start %s` to claim it", row.ID)
	}
	return fmt.Sprintf("run `lit start %s` to claim %s", row.ID, described)
}
