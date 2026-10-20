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
	fs.Retire("continue", "--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag")
	filterFlags := registerWorkableFilter(fs)
	// Same flag, same meaning, same deletion condition as `lit backlog --all`:
	// route over the whole queue rather than the focused goal's path.
	// [LAW:one-source-of-truth] one name for one idea across both surfaces.
	all := fs.Bool("all", false, "Ignore the focus scope and route over the whole queue")
	// `next` is read-only and still needs to know who is asking, because it is
	// the one read command whose output names an identity — resumeAdvice says
	// whether the ticket in flight is yours. So it takes the same --by every
	// mutating command takes, and reads it the same way: the session env wins,
	// --by answers when there is none. `lit start --by bob` wrote the assignee
	// through that rule, so `lit next --by bob` compares like with like; read
	// from the env alone, lit would tell bob the ticket he assigned himself
	// belongs to somebody else. Not the command's own --assignee: that narrows
	// the view, and a filter must not be able to rename the reader.
	//
	// It travels to the renderer as an argument rather than on claimContext.
	// That context is gathered by four commands and read for its standings; a
	// reader identity sitting there would be produced in one place for every
	// caller and consumed in one, so the next read command to render it would
	// resolve the reader by whatever the gatherer happened to do — env only,
	// silently different from this rule. One producer, one consumer, named at
	// the call. [LAW:one-source-of-truth]
	actor := registerActor(fs)
	return appLeaf{fs: fs, positionals: 0, usage: nextUsage, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
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
		if err := printStalenessWarning(ctx, stdout, ap.Workspace, ap.Store, time.Now()); err != nil {
			return err
		}
		// The whole queue, not the filtered one: routing applies the filter to
		// what it may serve, and reads every row to decide it (see routeNext).
		gathered, err := gatherWorkableQueue(ctx, ap)
		if err != nil {
			return err
		}
		cc, err := gatherClaimContext(ctx, stdout, ap)
		if err != nil {
			return err
		}
		outcome := routeNext(gathered.rows, gathered.details, gathered.epics, cc.standings, cc.evidence, cc.self, gathered.scope.scopeFor(*all), asked)
		recordNextEvent(ctx, outcome, len(gathered.keepRows(asked).rows))
		ap.Statusline.Record(nextStatuslineFacts(outcome, !filter.narrows() && !*all, cc.evidence)...)
		occasion, err := renderNextOutcome(stdout, outcome, gathered.details, cc, actor())
		if err != nil {
			return err
		}
		return dispatchOccasion(ctx, stdout, ap.Workspace, occasion)
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
func renderNextOutcome(w io.Writer, outcome NextOutcome, details map[string]storage.IssueRelations, cc claimContext, actingAs string) (workflows.Occasion, error) {
	var row annotation.AnnotatedIssue
	var announce string
	switch o := outcome.(type) {
	case ServedFromClaim:
		row = o.Row
	case ResumedOwnWork:
		row = o.Row
		announce = resumeAdvice(o.Row, actingAs) + "\n"
	case ServedFromEpicLane:
		row = o.Row
		announce = startAdvice(o.Row, o.Lane) + " (a second lane of an epic you already hold a lane in)\n"
	case ServedFromNewLane:
		row = o.Row
		announce = o.advice() + "\n"
	// Steps 1b and 2b say what they are for: "this unblocks work on your path"
	// is a fact about the WALK the row cannot show on its own, whereas step 2's
	// shared epic is visible in the id. A pool pick's one such fact, the focus
	// scope that narrowed it, is said by its advice.
	case ServedFromDependency:
		row = o.Row
		announce = startAdvice(o.Row, o.Lane) + dependencyReason(o) + "\n"
	// Leaving the scope is the agent's choice, so every route is named and
	// none is taken: the row is the last route, printed like any pick.
	case ServedPastExhaustion:
		row = o.Row
		routes := append(o.Exhaustion.stay(), "move on to the top ready ticket outside it: "+o.advice())
		announce = o.Exhaustion.why() + "\n" + strings.Join(routes, "\nor ") + "\n"
	// The hidden ticket's route comes first, as the stay route does above: it
	// is the answer the unfiltered question gets, and the row is the one this
	// question gets.
	case ServedPastNarrowing:
		row = o.Row
		announce = o.Narrowing.Error() + "\nor take the top ready ticket the filter selects, outside the work you hold: " + o.advice() + "\n"
	// The three terminal outcomes travel outward AS THEMSELVES. Rendering them
	// into an untyped error here would discard the very discriminator routing
	// has just established, so both sinks — ExitCode and commandErrorReason —
	// would fall through to "command_failed", whose remediation tells the agent
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
// a lane this checkout holds. Two sentences, and the one it picks turns on a
// single question: does the ticket name somebody who is not the one asking?
//
// "continue where you left off" is a claim about WHO, and the lane cannot
// support it. A lane is keyed on the checkout, deliberately — many sessions in
// one checkout are one claimant, which is what lets a fresh session inherit its
// predecessor's work with no re-briefing (design-docs/work-claims.md). But two
// sessions running in one checkout at once are also one claimant, and there
// the sentence would tell the second one it has been working a ticket the first
// is mid-PR on.
//
// The assignee cannot separate a live peer from a predecessor who stopped, and
// NOTHING lit holds can: every session mints a new identity, so both read as
// "not you", and claims carry staleness heuristics with no liveness probe by
// design. The orphan clock fails open as a discriminator: `updated_at` moves on
// field writes and transitions, not on the work, and a comment never touches
// the row (store.go, AddComment), so a session committing and commenting all
// day goes "orphaned" at six hours while still holding the branch.
//
// So it does not adjudicate. The asymmetry decides the default: a warning that
// was not needed costs one check, and silence that was needed costs a
// collision. It names who the ticket says has it, asks for the check, and leads
// with continuing rather than dropping the work — the predecessor hand-down is
// the common case and must stay cheap, which is what keeps this a line to read
// rather than a gate to clear.
//
// The one thing that silences it is an assignee that names nobody: empty is the
// ordinary state of a ticket started by a checkout driving no agent session, and
// there is no name to contradict. An unidentified READER is not such a case —
// nobody's name is the empty string, so a ticket assigned to anyone at all is
// assigned to somebody other than a shell that resolved no identity.
//
// What the sentence may say is bounded by what the mismatch proves, which is
// only that the name on the ticket is not this command's. It is not proof of a
// session — an assignee is free text, and `lit new --assignee bob` writes a
// person there — nor of a checkout: a lane taken with `lit start --take` can
// still hold a sibling ticket assigned in the checkout it came from. So the line
// names the assignee, says it is not you, and stops.
func resumeAdvice(row annotation.AnnotatedIssue, actingAs string) string {
	if assignee := strings.TrimSpace(row.AssigneeValue()); assignee != "" && assignee != actingAs {
		return fmt.Sprintf("%s is in progress and assigned to %s, not to you — check that they have stopped before you continue it, or take other work from `lit backlog`", row.ID, assignee)
	}
	return fmt.Sprintf("%s is already in progress in a lane you hold — continue where you left off", row.ID)
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
func startAdvice(row annotation.AnnotatedIssue, lane model.LaneID) string {
	object, named := lane.Describe()
	if !named {
		object = "it"
	}
	advice := fmt.Sprintf("run `lit start %s` to claim %s", row.ID, object)
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
func (p poolPick) advice() string {
	return startAdvice(p.Row, p.Lane) + p.Scope.pickClause()
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
