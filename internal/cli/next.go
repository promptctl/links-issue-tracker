package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
	fs.Retire("continue", "--continue is retired; claim routing already serves the lanes you hold first in `lit next` — run `lit next` with no flag")
	filterFlags := registerWorkableFilter(fs)
	// Same flag, same meaning, same deletion condition as `lit backlog --all`:
	// route over the whole queue rather than the focused goal's path.
	// [LAW:one-source-of-truth] one name for one idea across both surfaces.
	all := fs.Bool("all", false, "Ignore the focus scope and route over the whole queue")
	// `next` is read-only and still needs to know who is asking, because a lane
	// is held by a session in a checkout, and only the holder session is
	// served the lane's work. So it takes the same --by every mutating command
	// takes, and reads it the same way: the session env wins, --by answers when
	// there is none. `lit start --by bob` recorded its hold through that rule,
	// so `lit next --by bob` compares like with like. Not the command's own
	// --assignee: that narrows the view, and a filter must not be able to
	// rename the reader. [LAW:one-source-of-truth]
	actor := registerActor(fs)
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
		cc, err := gatherClaimContext(ctx, inv.Env, inv.Stdout, ap, actor(inv.Env))
		if err != nil {
			return err
		}
		outcome := routeNext(gathered.rows, gathered.details, gathered.epics, cc.standings, cc.evidence, cc.self, gathered.scope.scopeFor(*all), asked)
		primaryClone := ap.Workspace.PrimaryClone()
		recordNextEvent(ctx, outcome, len(gathered.keepRows(asked).rows), primaryClone)
		ap.Statusline.Record(nextStatuslineFacts(outcome, !filter.narrows() && !*all, cc.evidence)...)
		occasion, err := renderNextOutcome(inv.Stdout, outcome, gathered.details, cc, primaryClone)
		if err != nil {
			return err
		}
		return dispatchOccasion(ctx, inv.Stderr, inv.Env, inv.Stdout, ap.Workspace, occasion)
	}}
}

// renderNextOutcome prints the row routeNext selected — or, for Exhausted,
// Narrowed and NoWork, returns the loud diagnostic instead of printing a ticket
// that was never picked. A claim this pick WOULD establish (EpicLane, NewLane,
// Dependency) is named above the row, so the commitment is visible before it
// is made (design-docs/work-claims.md, Routing step 4); a lane already held
// names nothing to commit — nothing at all for ServedFromClaim, and the state
// it is already in for ResumedOwnWork, since being handed back a ticket already
// in flight is the one pick that looks like a fresh start but is not one.
//
// Every line here is in the conditional or reports a state that already holds.
// `lit next` claims nothing and starts nothing — `lit start` does — so a line
// in the perfect tense would be reporting a side effect this command does not
// have.
func renderNextOutcome(w io.Writer, outcome NextOutcome, details map[model.ID]storage.IssueRelations, cc claimContext, primaryClone bool) (workflows.Occasion, error) {
	// [LAW:single-enforcer] placeFor is the decision the event records too; it
	// is pure, and the leaf hands both the one outcome and checkout kind.
	place := placeFor(outcome, primaryClone)
	var row annotation.AnnotatedIssue
	var announce string
	switch o := outcome.(type) {
	case ServedFromClaim:
		row = o.Row
	case ResumedOwnWork:
		row = o.Row
		announce = resumeAdvice(o.Row) + "\n"
	case TakeOverHere:
		row = o.Row
		announce = takeOverAdvice(o, cc.addresses) + "\n"
	case ServedFromEpicLane:
		row = o.Row
		announce = startAdvice(o.Row, o.Lane, place) + " (a second lane of an epic you already hold a lane in)\n"
	case ServedFromNewLane:
		row = o.Row
		announce = o.advice(place) + "\n"
	// Steps 1b and 2b say what they are for: "this unblocks work on your path"
	// is a fact about the WALK the row cannot show on its own, whereas step 2's
	// shared epic is visible in the id. A pool pick's one such fact, the focus
	// scope that narrowed it, is said by its advice.
	case ServedFromDependency:
		row = o.Row
		announce = startAdvice(o.Row, o.Lane, place) + dependencyReason(o) + "\n"
	// Leaving the scope is the agent's choice, so every route is named and
	// none is taken: the row is the last route, printed like any pick.
	case ServedPastExhaustion:
		row = o.Row
		routes := append(o.Exhaustion.stay(), "move on to the top ready ticket outside it: "+o.advice(place))
		announce = o.Exhaustion.why() + "\n" + strings.Join(routes, "\nor ") + "\n"
	// The hidden ticket's route comes first, as the stay route does above: it
	// is the answer the unfiltered question gets, and the row is the one this
	// question gets.
	case ServedPastNarrowing:
		row = o.Row
		announce = o.Narrowing.Error() + "\nor take the top ready ticket the filter selects, outside the work you hold: " + o.advice(place) + "\n"
	// The three terminal outcomes travel outward AS THEMSELVES. Rendering them
	// into an untyped error here would discard the very discriminator routing
	// has just established, so the classifier, commandErrorReason, would fall
	// through to "command_failed", whose remediation tells the agent
	// to retry a deterministic answer and then run `lit doctor` on a healthy
	// workspace. [LAW:types-are-the-program] classification is carried by the
	// type, never re-derived from the message.
	case Exhausted:
		return workflows.Occasion{}, o
	case Narrowed:
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

// resumeAdvice is what `next` says when it hands back work already in flight in
// a lane this session holds. Routing serves such a row only to the holder — the
// session and checkout the hold names (relationOf) — so "continue where you
// left off" is said to the one reader it can be true of, and a second live
// session never reaches this line for it.
func resumeAdvice(row annotation.AnnotatedIssue) string {
	return fmt.Sprintf("%s is already in progress in a lane you hold — continue where you left off", row.ID)
}

// takeOverAdvice names the session that holds a lane from this checkout and
// the one command that moves the hold here, so a successor resumes its
// predecessor's work instead of opening a lane beside it.
func takeOverAdvice(o TakeOverHere, addresses map[model.Attribution]workspace.Checkout) string {
	return fmt.Sprintf("%s is held by %s — if you are continuing that session's work, take it over with `lit start %s --take`", o.Row.ID, nameHolder(o.Held.By, addresses), o.Row.ID)
}

// dependencyReason says why step 1b or 2b handed this row over. A pick under an
// epic that blocks names the epic, because the row itself has no edge to the
// ticket it frees. "On your path" rather than "in a lane you hold", because
// step 2b's gated row may sit in a sibling lane of the epic. A pick that gates
// it only through further prerequisites names them, nearest the gated row
// first, since the pick's own edge reaches the last of them and not the row.
// [LAW:dataflow-not-control-flow] the last inch of rendering, where the two
// arms are different sentences.
func dependencyReason(o ServedFromDependency) string {
	if o.Blocker == o.Row.ID {
		return fmt.Sprintf(" (gates %s, which is on your path%s)", o.Gates, viaClause(o.Via))
	}
	return fmt.Sprintf(" (it is in epic %s, which gates %s on your path%s)", o.Blocker, o.Gates, viaClause(o.Via))
}

// viaClause renders the chain between a dependency and the row it gates, or
// nothing when the dependency is the row's own.
func viaClause(via []hop) string {
	if len(via) == 0 {
		return ""
	}
	names := make([]string, len(via))
	for i, h := range via {
		names[i] = h.name()
	}
	return ", through " + strings.Join(names, ", ")
}

// startAdvice is the line every pick that would establish a claim prints above
// its row: what running `lit start` would lock, never what this command did.
//
// The lead clause turns on the row's own lifecycle state: routing serves an
// in-progress row from a lane this checkout does not hold only when nobody
// holds that lane (capacityFor), so the row is somebody's abandoned work and
// may carry their unmerged working tree. The clause says exactly that and no
// more — "nobody holds it" is the whole of what the standing proves. It does
// not say whose it was or how long ago they left: an expired claim is not a
// claim, and the row's history is `lit show`'s to tell.
//
// The object turns on the lane's shape, which is why Describe answers in two
// parts. A solo lane IS the ticket, so naming it would spell the same id twice
// ("starting X claims X") — a tautology no reader could take as advice about a
// command they have yet to run. The pronoun is the caller's answer to that,
// available here and nowhere else because the ticket is named one clause
// earlier. next_route_test.go pins all four cells of state and lane shape.
func startAdvice(row annotation.AnnotatedIssue, lane model.LaneID, place startPlace) string {
	object, named := lane.Describe()
	if !named {
		object = "it"
	}
	advice := fmt.Sprintf("%srun `lit start %s` to claim %s", place.where(), row.ID, object)
	if row.State() == model.StateInProgress {
		return fmt.Sprintf("%s is in progress and nobody holds it — %s", row.ID, advice)
	}
	return advice
}

// advice is a pool pick's startAdvice with the question that produced it: a
// focus label narrows the pool to its goals' prerequisite path, so a ticket
// ranked above the pick but off that path was never asked about, and a pick
// that said nothing would read as a ranking bug to anyone checking it against
// `lit backlog --all`. The whole queue is the zero scope and says nothing,
// because a pick over the whole queue is the backlog's top, as it reads.
// [LAW:dataflow-not-control-flow] every pool pick renders through here; the
// scope's value decides the clause.
func (p poolPick) advice(place startPlace) string {
	return startAdvice(p.Row, p.Lane, place) + p.Scope.pickClause()
}

// pickClause is how a row the pool offered names the focus scope it was drawn
// under, in the words offFocusPathNote uses for the same scope, and the empty
// string for the whole queue. "The focus path" is one path however many goals
// carry the label.
func (s focusScope) pickClause() string {
	if !s.active() {
		return ""
	}
	return fmt.Sprintf(" (focused on %s: picked from the focus path alone — `lit next --all` to route over the whole queue)", s.describe())
}
