package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// NextOutcome is what routeNext concluded, sealed to the cases the routing
// precedence in design-docs/work-claims.md — as amended by the epic-major
// GRANULARITY RULING on links-claims-1ihf.5 — can produce. A bare
// annotation.AnnotatedIssue cannot say WHICH case picked it, and the
// exhaustion case in particular must never be confused with an ordinary
// pick: it is an answer-shaped void unless the type forbids the confusion.
// [LAW:types-are-the-program] [LAW:parse-dont-validate]
//
// The served variants split on the one question the renderer must answer
// before handing a row over: does taking it establish a claim this checkout
// did not already hold? Every announcement follows from that.
type NextOutcome interface{ isNextOutcome() }

// ServedFromClaim is a ready ticket in a lane this checkout already holds —
// routing step 1. No new claim is established, so nothing is announced.
type ServedFromClaim struct{ Row annotation.AnnotatedIssue }

// ResumedOwnWork is a ticket already in flight in a lane this checkout holds,
// handed back to its holder — routing step 1 for work that is started rather
// than startable. Nothing is claimed and nothing is begun, so it reports a
// state rather than an act. Routing does not gate servability on
// model.StateOpen: an in_progress row would then be servable to nobody, which
// would hide every orphan and the very ticket the checkout is working on.
type ResumedOwnWork struct{ Row annotation.AnnotatedIssue }

// ServedFromEpicLane is a pick from a different lane of the same epic this
// checkout already holds a lane in — the GRANULARITY RULING's new step 2,
// epic-major before global. Starting it would establish a fresh claim on Lane,
// which is why it carries the lane to name, and it admits abandoned in-flight
// work exactly as ServedFromNewLane does.
//
// The epic is Lane.Epic() and is not carried beside it: two fields for one fact
// are two clocks. [LAW:one-source-of-truth]
type ServedFromEpicLane struct {
	Row  annotation.AnnotatedIssue
	Lane model.LaneID
}

// ServedFromNewLane is a ready ticket in a lane this checkout does NOT hold, so
// starting it would establish a claim — which is what Lane is carried to name.
// The global pool (step 4) is its only producer. Step 1b has its own outcome,
// ServedFromDependency: it establishes a claim, so it cannot be ServedFromClaim,
// whose contract is that nothing is announced, and sharing this type with the
// pool would leave the renderer unable to tell the two apart.
//
// Abandoned in-flight work arrives here too, and says so — startAdvice reads
// the row's state. A ready ticket reads as a fresh start whatever its lane's
// history: a lane whose claim expired is unclaimed, and nothing about the
// expired claim is printed (links-claims-y6yz).
//
// Lane is the LaneID and not its String(): the rendering belongs to whoever
// knows the reader, and stringifying here would throw away the discriminator
// the renderer needs to tell a lane worth naming from one that would only
// repeat the ticket. [LAW:types-are-the-program]
type ServedFromNewLane struct {
	Row  annotation.AnnotatedIssue
	Lane model.LaneID
}

// ServedFromDependency is routing step 1b or 2b: a ready ticket OUTSIDE our
// lanes that is work toward a dependency gating one of our blocked rows — in a
// lane we hold (1b), or anywhere in our epic (2b). Blocker is that dependency —
// Row's own id when it is a leaf, or an epic Row sits under, since an epic
// cannot be started — and Gates is the row of ours it holds back. Like
// ServedFromNewLane it establishes a claim on a lane this checkout does not
// hold, so Lane is carried for the same reason. Unlike it, the pick has a
// reason the row cannot show on its own — it unblocks work on our path — and
// without Blocker the announcement would say Row gates a ticket that Row may
// have no edge to.
//
// It is its own outcome rather than a nullable qualifier on ServedFromNewLane
// because the two picks answer different questions. A Gates field hanging off
// the shared type would make "a global-pool pick that gates something" and "a
// dependency pick that gates nothing" both representable, and neither exists.
// Here Gates and Blocker are always set, by construction: gatingDependencies
// only yields a dependency because some in-scope row depends on it. The renderer's switch
// panics on an unhandled outcome, so a new type announces itself at the first
// unhandled call site instead of falling through silently — which is what makes
// the discriminated form cheaper here than the flag. [LAW:types-are-the-program]
//
// Step 2 already qualifies its pick ("a second lane of an epic you already hold
// a lane in"); this pick has the stronger claim to one, and without it the
// hardest pick to predict would be the only unexplained one.
type ServedFromDependency struct {
	Row     annotation.AnnotatedIssue
	Lane    model.LaneID
	Gates   string
	Blocker string
}

// ServedPastExhaustion is routing step 3 with somewhere to go: this checkout's
// own scope has nothing reachable (Exhaustion says why), and the global pool
// has a ready ticket outside it. An epic holding only blocked work used to
// answer with Exhausted alone, and since routing is deterministic every later
// `next` repeated it: the agent stayed in the epic with nothing to do until
// its claim expired (owner ruling, links-next-5sxz, 2026-09-29).
//
// It carries the whole exhaustion rather than a flag, because the renderer
// owes the agent every route out, and only the exhaustion knows whether one
// stays: file the ticket that unblocks the scope, when something blocks it — or
// move on to Row. Leaving is never silent — the pick is announced beside the
// reason the scope stopped.
// [LAW:types-are-the-program] [LAW:no-silent-failure]
type ServedPastExhaustion struct {
	Row        annotation.AnnotatedIssue
	Lane       model.LaneID
	Exhaustion Exhausted
}

// Exhausted is the checkout's own claimed epic(s) having open work with none
// of it reachable: not the held lanes, not an on-path dependency, not another
// lane of the same epic — and the global pool having nothing ready outside it
// either, or routing would have answered ServedPastExhaustion. Loud and
// diagnostic. Blocked names the open dependencies from outside the scope gating
// that work, if any. Held names the scope's own open rows held by a reason about
// the row itself — a reserved label, a missing field — which no other work
// finishing will clear; a scope with neither is waiting only on work already in
// progress. OffPath names the rows outside the scope
// that a focus label kept out of the pool, so "nothing ready outside it" is
// never claimed of rows the pool was not asked about; with no focus active it
// is empty, and on ServedPastExhaustion, whose pool did offer a row.
//
// It is also an error, for the same reason HelpRequestedError is: it is an
// answer, not a failure, and the error channel is the only channel runNext has
// for an answer that hands back no row. Being the error itself — rather than
// being rendered into a separate error type — is what keeps the reason and exit
// sinks reading the routing verdict rather than a copy of it that could drift
// from these fields. [LAW:one-source-of-truth]
type Exhausted struct {
	Epics   []string
	Blocked []rowReach
	Held    []rowReach
	OffPath []rowReach
}

// reachKind is what one row is to this checkout right now — the one fact both
// terminal diagnostics need, and the limit of what either may say. A bool here
// would read "takeable or not", so a row outside this run's filtered view, or
// one not startable itself, would render as the one reason the message names:
// claimed by another checkout. The renderer picks a note per value rather than
// asserting a cause it cannot see.
//
// Exhaustion asks it of the dependencies gating our scope; an empty global pool
// asks it of every row the walk went past. Same question, same answers, so one
// type answers it — a second enum beside this one, saying the same things
// about a different set of rows, is two clocks.
// [LAW:types-are-the-program] [LAW:one-type-per-behavior] [LAW:no-silent-failure]
//
// Every kind but reachOffFocusPath is declared from most to least this
// checkout can do, which is what lets a dependency with several tickets under
// it report the best of them (gatingDependencies).
type reachKind int

const (
	reachTakeable reachKind = iota
	// reachHeldFresh: another checkout holds its lane right now.
	reachHeldFresh
	// reachNotReady: gathered and not held elsewhere, but not startable —
	// blocked by a further dependency.
	reachNotReady
	// reachAwaitingOutside: gathered and not held elsewhere, and held by the
	// external label — it waits on an event outside this repository, so no
	// ticket filed here can clear it. Below reachNotReady because nothing here
	// moves it, above reachOutOfView because this run can see it.
	reachAwaitingOutside
	// reachOutOfView: nothing that would clear it is among the gathered rows,
	// so this run knows nothing about it. --type/--labels/--assignee and
	// leaf-only membership narrow the gather; the dependency annotation is read
	// from the store and does not. Only the exhaustion walk can reach it — that
	// one reads dependency ids off annotations, while the pool walk classifies
	// rows it is already holding.
	reachOutOfView
	// reachOffFocusPath: gathered and possibly startable, but outside the focus
	// scope this run answered over, so the pool walk never offered it. Not a
	// verdict about the row — a statement about the question that was asked —
	// which is why it is classified where the scope is applied (routeNext step
	// 4) rather than inside reachOf, whose other caller walks an unscoped set.
	reachOffFocusPath
	// reachKindCount bounds reachNotes and is never a classification.
	reachKindCount
)

// rowReach is a row a walk went past, carrying what this checkout may do about
// it. Row is the gathered row Kind was read from, for every kind but
// reachOutOfView: the row itself, or for a dependency, the ticket that would
// clear it.
type rowReach struct {
	ID   string
	Row  annotation.AnnotatedIssue
	Kind reachKind
}

// name is how a diagnostic names the entry. A dependency read through a ticket
// under it is named as both, "<ticket> under <epic>": the note's `lit start` or
// `lit show` acts on the ticket, since an epic cannot be started, and the epic
// is what blocks. Every other entry is its own id. [LAW:dataflow-not-control-flow]
// the last inch of rendering, where the two arms are different phrases.
func (r rowReach) name() string {
	if r.Row.ID == "" || r.Row.ID == r.ID {
		return r.ID
	}
	return fmt.Sprintf("%s under %s", r.Row.ID, r.ID)
}

// reachOf classifies one row, consuming capacityFor rather than re-deriving
// takeability so routing and the diagnostics read one authority.
// [LAW:one-source-of-truth]
//
// Total by construction: relationOf covers three lane relations, and the
// fallthrough takes every routeAround reached for a reason other than a
// foreign hold. A row that is both held fresh and not ready reports as
// held — ownership decides whether this checkout may act at all, readiness
// only whether acting would get anywhere. The external label outranks both: a
// row it holds reports as awaiting outside whoever holds its lane and whatever
// else blocks it, because no one's work here would move it.
func reachOf(row annotation.AnnotatedIssue, standing claims.Standing, self model.Attribution) reachKind {
	switch {
	case capacityFor(row, standing, self) != routeAround:
		return reachTakeable
	case ClassifyReadiness(row.Annotations).AwaitsOutside():
		return reachAwaitingOutside
	case relationOf(standing, self) == laneHeldForeign:
		return reachHeldFresh
	}
	return reachNotReady
}

// NoWork is the global pool handing back nothing. Unreachable is every row that
// walk went past, each carrying why — empty exactly when the gather itself came
// back empty, which is the genuinely empty backlog. An error on the same terms
// as Exhausted.
//
// "No ready work" is an answer-shaped void the moment it carries nothing: one
// sentence for "the backlog is empty" and for "the backlog is full of work you
// may not have", two facts a caller can never pull back apart. An agent
// narrowing `next` to --status in_progress is asking what it was already on —
// the question asked after a crash — and the empty-queue wording would tell it
// its work is gone. The walk knows every row and every verdict; keeping them is
// what lets the message name which emptiness this is, instead of the
// remediation listing all three and hoping. [LAW:parse-dont-validate]
// [LAW:types-are-the-program]
type NoWork struct{ Unreachable []rowReach }

func (ServedFromClaim) isNextOutcome()      {}
func (ResumedOwnWork) isNextOutcome()       {}
func (ServedFromEpicLane) isNextOutcome()   {}
func (ServedFromNewLane) isNextOutcome()    {}
func (ServedFromDependency) isNextOutcome() {}
func (ServedPastExhaustion) isNextOutcome() {}
func (Exhausted) isNextOutcome()            {}
func (NoWork) isNextOutcome()               {}

// capacity is the single answer to "may this checkout take this row, and as
// what?" — the eligibility verdict the owner ruling on links-claims-1b0p
// demands. Independent booleans consulted at separate points of one walk would
// each re-derive a piece of this question and could disagree about what an
// aged-out claim means. With the answer in one place a further capacity costs
// one arm of one switch instead of an edit to several predicates and the loop.
// [LAW:types-are-the-program] [LAW:single-enforcer]
type capacity int

const (
	// routeAround: not this checkout's to take right now.
	routeAround capacity = iota
	// serveWork: startable work this checkout may claim or already holds.
	serveWork
	// resumeWork: work already underway in a lane this checkout holds. Handed
	// back rather than started fresh.
	resumeWork
)

// capacityFor derives the verdict from the only three facts that bear on it:
// the row's lifecycle state, its lane's relation to this checkout, and whether
// anything blocks it. Pure and total.
//
// Two rules cover the whole table. A lane another checkout holds right now is
// routed around, whatever is in it. In any other lane — this checkout's own,
// or nobody's — startable work is served, and work in flight is handed back
// when the lane is ours and served when it is nobody's: an in-progress row in
// a lane nobody holds is abandoned by definition, because whoever started it
// no longer holds a claim there (links-claims-y6yz).
//
// The row's own quiet clock — its last write by anyone — does not enter here:
// with the lane's standing the one authority on whether anybody holds it, that
// clock adds nothing routing can act on. `lit backlog` and `lit orphaned` read
// it as a description of the row rather than a verdict about the lane.
// [LAW:single-enforcer]
//
// Readiness is asked only of startable rows, with one exception. An in-flight
// row is not gated by its dependencies — it is already past the point where
// they applied — so a blocked-but-started row of ours is still resumed and a
// blocked-but-started row of nobody's is still served. The external label is
// the exception because it is not a precondition of starting: it says no work
// in this repository moves the row, which is as true in flight as before, so a
// row it holds is routed around in every state.
func capacityFor(row annotation.AnnotatedIssue, standing claims.Standing, self model.Attribution) capacity {
	relation := relationOf(standing, self)
	started := row.State() == model.StateInProgress
	readiness := ClassifyReadiness(row.Annotations)
	switch {
	case relation == laneHeldForeign, readiness.AwaitsOutside():
		return routeAround
	case !started && readiness.IsReady():
		return serveWork
	case !started:
		return routeAround
	case relation == laneOurs:
		return resumeWork
	}
	return serveWork
}

// ownScope reads this checkout's held lanes, and the epics they sit in,
// straight from the standings.
//
// [LAW:one-source-of-truth] It reads standings and NOT the gathered rows. The
// rows are already narrowed by --type/--labels/--assignee, so deriving
// ownership from them would let any display filter empty this set and drop the
// whole self-aware branch — a checkout with a perfectly fresh claim silently
// hopping epics because it asked for one issue type. Ownership is a fact about
// the workspace; a display filter must not be able to change it.
//
// An unidentified self needs no guard here: relationOf owns what a
// public-checkout self may match, so ownership and the start gate cannot drift
// apart on it. [LAW:single-enforcer]
func ownScope(standings claims.Standings, self model.Attribution) (map[model.LaneID]bool, map[string]bool) {
	lanes := map[model.LaneID]bool{}
	epics := map[string]bool{}
	for lane, standing := range standings {
		if relationOf(standing, self) != laneOurs {
			continue
		}
		lanes[lane] = true
		if lane.Epic() != "" {
			epics[lane.Epic()] = true
		}
	}
	return lanes, epics
}

// routeNext applies the claim-aware selection precedence over an already
// gathered, already ordered workable set. Pure — standings and self are
// values the caller derived once from the store and the local filesystem, so
// every branch of the precedence is testable without either.
// [LAW:effects-at-boundaries]
//
// Precedence: this checkout's own lanes first, startable work and work of ours
// already underway alike; then an on-path external dependency that gates one of
// them; then the rest of its epic's lanes (the GRANULARITY RULING's epic-major
// amendment); then, if the epic has open work but none of it is reachable, the
// global pool's pick announced beside why the epic stopped, or the loud
// exhaustion diagnostic when the pool has nothing either. A checkout with no
// lanes of its own starts straight at the global pool — unfocus is the zero
// state, not a hop through the earlier steps.
//
// The focus scope narrows STEP 4 AND NOTHING ELSE. Steps 1-3 route over lanes
// this checkout already holds, and ownScope takes pains to read that from the
// standings rather than the gathered rows precisely so a display narrowing
// cannot empty the self-aware branch. A scope applied to `rows` before routing
// would re-open exactly that hole from the other side: a checkout holding a
// ticket off the focused path would be told to start something else while its
// own work sat in flight. Focus decides where a fresh session goes; it does not
// decide whether your own work is still yours.
//
// [LAW:dataflow-not-control-flow] Every step walks the same rows in the same
// composite-rank order and asks capacityFor the same question; a step differs
// only in which lanes it admits and which verdicts it accepts. No step decides
// eligibility on its own.
//
// epics is the relations of the epics above the rows, keyed by epic id — what
// epicsAbove climbs to tell which rows sit under an epic.
func routeNext(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, epics map[string]storage.IssueRelations, standings claims.Standings, self model.Attribution, scope focusScope) NextOutcome {
	laneOf := func(row annotation.AnnotatedIssue) model.LaneID {
		return model.LaneOf(row.Issue, details[row.ID].Parent)
	}
	verdict := func(row annotation.AnnotatedIssue) capacity {
		return capacityFor(row, standings.Of(laneOf(row)), self)
	}
	reachFor := func(row annotation.AnnotatedIssue) reachKind {
		return reachOf(row, standings.Of(laneOf(row)), self)
	}
	// pick keeps the first row, in rank order, that sits in an admitted lane
	// and carries one of the accepted verdicts.
	//
	// accept is a SET and never a preference order: composite rank is the only
	// tiebreak routing gets to apply, and ranking capacities against each other
	// would pass over the backlog's #1 row, abandoned in flight, for a
	// lower-ranked leaf that is merely ready. [LAW:one-source-of-truth] one
	// ordering, and the gather already established it.
	// pickFrom takes the row set explicitly because the steps do not share
	// one: steps 1-3 walk every gathered row, step 4 walks the focus-scoped
	// pool. Passing the set is what keeps that difference visible at each call
	// instead of hidden in a closure every step reads differently.
	pickFrom := func(from []annotation.AnnotatedIssue, inScope func(model.LaneID) bool, accept ...capacity) (annotation.AnnotatedIssue, capacity, bool) {
		for _, row := range from {
			if how := verdict(row); inScope(laneOf(row)) && slices.Contains(accept, how) {
				return row, how, true
			}
		}
		return annotation.AnnotatedIssue{}, routeAround, false
	}
	pick := func(inScope func(model.LaneID) bool, accept ...capacity) (annotation.AnnotatedIssue, capacity, bool) {
		return pickFrom(rows, inScope, accept...)
	}

	// Step 4's pick, shared by the step that falls through to it. The pool is
	// the focus-scoped one — see the step 4 comment below.
	pool, offPath := scope.partition(rows)
	fromPool := func() (annotation.AnnotatedIssue, bool) {
		row, _, ok := pickFrom(pool, func(model.LaneID) bool { return true }, serveWork)
		return row, ok
	}

	ownLanes, ownEpics := ownScope(standings, self)
	mine := func(lane model.LaneID) bool { return ownLanes[lane] }
	if len(ownLanes) > 0 {
		workToward := workTowardEach(rows, details, epics)
		// Step 1 — our own lanes, startable work and work already underway
		// alike, whichever the backlog ranks first.
		if row, how, ok := pick(mine, serveWork, resumeWork); ok {
			if how == resumeWork {
				return ResumedOwnWork{Row: row}
			}
			return ServedFromClaim{Row: row}
		}
		// Step 1b — a dependency outside our lanes that gates one of them. It
		// establishes a claim on a lane we do not hold, so it is announced as
		// one and its own lane's standing is honoured rather than ignored.
		if served, ok := onPathDependency(gatingDependencies(rows, laneOf, mine, reachFor, workToward), laneOf); ok {
			return served
		}
		// Step 2 — the rest of our epic, in lanes we do not already hold.
		ourEpic := func(lane model.LaneID) bool {
			return lane.Epic() != "" && ownEpics[lane.Epic()]
		}
		if row, _, ok := pick(func(lane model.LaneID) bool { return ourEpic(lane) && !mine(lane) }, serveWork); ok {
			return ServedFromEpicLane{Row: row, Lane: laneOf(row)}
		}
		// Step 2b — a dependency outside our epic that gates any of it. Clearing
		// it is how the epic moves again, so it outranks every ticket the pool
		// could offer; after step 2, because a ready lane of the epic moves it
		// without leaving it. The one walk feeds step 3 too, so what exhaustion
		// reports is exactly what this step declined. [LAW:one-source-of-truth]
		ourScope := func(lane model.LaneID) bool { return mine(lane) || ourEpic(lane) }
		gating := gatingDependencies(rows, laneOf, ourScope, reachFor, workToward)
		if served, ok := onPathDependency(gating, laneOf); ok {
			return served
		}
		// Step 3 — loud, and never a silent hop. The pool's pick is outside our
		// scope by construction: steps 1 and 2 took every serveWork row in our
		// lanes and our epic, and the pool is a subset of the rows they walked.
		ours := func(row annotation.AnnotatedIssue) bool { return ourScope(laneOf(row)) }
		exhausted := Exhausted{
			Epics:   slices.Sorted(maps.Keys(ownEpics)),
			Blocked: blockedRows(gating),
			Held:    heldByThemselves(rows, ours, reachFor),
		}
		if row, ok := fromPool(); ok {
			return ServedPastExhaustion{Row: row, Lane: laneOf(row), Exhaustion: exhausted}
		}
		// The pool offered nothing, so what the focus kept from it is the rest
		// of the answer: the withheld rows outside our scope — ours were walked
		// by steps 1-2b, focus or not.
		exhausted.OffPath = withheldByScope(slices.DeleteFunc(slices.Clone(offPath), ours))
		return exhausted
	}

	// Step 4 — the global pool, and the diagnostic half of the same walk: what
	// the pick declined is what NoWork reports, classified by the verdict the
	// pick itself just read. [LAW:one-source-of-truth]
	//
	// The pool is the focus-scoped one, and the rows the scope withheld travel
	// into the diagnostic rather than vanishing: "nothing is startable" and
	// "nothing on your focus path is startable" are different answers, and a
	// pool that quietly served an off-path row instead would be substituting a
	// similar-looking query for the one asked. [LAW:no-silent-failure]
	if row, ok := fromPool(); ok {
		return ServedFromNewLane{Row: row, Lane: laneOf(row)}
	}
	return NoWork{Unreachable: append(passedOver(pool, reachFor), withheldByScope(offPath)...)}
}

// withheldByScope classifies the rows the focus scope kept out of the pool. They
// are the one group whose kind is decided by the question rather than by the
// row, which is why they are stamped here — at the only place that applied the
// scope — instead of inside reachOf, whose other caller walks an unscoped set
// and would never have a scope to consult. [LAW:single-enforcer]
func withheldByScope(rows []annotation.AnnotatedIssue) []rowReach {
	withheld := make([]rowReach, 0, len(rows))
	for _, row := range rows {
		withheld = append(withheld, rowReach{ID: row.ID, Row: row, Kind: reachOffFocusPath})
	}
	return withheld
}

// passedOver classifies every row in the pool it is handed, in the rank order
// the pool walk went through them. It is called only where that walk found
// nothing, so every row here is routeAround by construction — a takeable one
// would have been served — which lets NoWork say why the pool was empty without
// asking the data a second question.
//
// The pool is every gathered row only when no focus scope narrowed it. Under a
// scope the walk never sees the off-path rows at all: withheldByScope stamps
// those, and the call site appends the two lists, so it is NoWork.Unreachable —
// not this function — that accounts for every gathered row. Saying "all the
// rows" here would hand the next reader the conclusion that the off-path rows
// are already covered, and the separate function that exists to cover them
// would read as redundant. [LAW:one-source-of-truth]
func passedOver(rows []annotation.AnnotatedIssue, reachFor func(annotation.AnnotatedIssue) reachKind) []rowReach {
	passed := make([]rowReach, 0, len(rows))
	for _, row := range rows {
		passed = append(passed, rowReach{ID: row.ID, Row: row, Kind: reachFor(row)})
	}
	return passed
}

// gatedDep is one gating dependency together with the row it gates. The gated
// id is the whole reason step 1b's pick is worth explaining, and this walk is
// the only place it is ever in scope: the loop below holds the blocked row and
// its dependency at the same instant. Keeping only the dependency would leave
// the renderer unable to say why the pick was handed over.
//
// It is a field of this walk's element rather than of rowReach because rowReach
// serves two other walks — withheldByScope and passedOver — for which a gated id
// is meaningless and would always be empty. A field that only one producer ever
// fills is a bag of optionals in miniature, and every later reader would have to
// learn which walks populate it. [LAW:types-are-the-program]
type gatedDep struct {
	rowReach
	Gates string
}

// heldByThemselves collects, in rank order, the rows in scope that routing
// passed over for a reason about the row itself — a reserved label, a missing
// field — each with the reachOf kind that says so. A row another checkout holds
// reads reachHeldFresh and stays out, being that checkout's to move; one the
// external label holds is the exception, as it is in reachOf, since no one's
// work here moves it.
// Blocked cannot carry these rows: it names dependencies from outside the
// scope, and these are inside it.
func heldByThemselves(rows []annotation.AnnotatedIssue, inScope func(annotation.AnnotatedIssue) bool, reachFor func(annotation.AnnotatedIssue) reachKind) []rowReach {
	var held []rowReach
	for _, row := range rows {
		kind := reachFor(row)
		if inScope(row) && (kind == reachNotReady || kind == reachAwaitingOutside) && ClassifyReadiness(row.Annotations).HeldByItself() {
			held = append(held, rowReach{ID: row.ID, Row: row, Kind: kind})
		}
	}
	return held
}

// blockedRows drops the gated id, for Exhausted: that diagnostic reports WHICH
// dependencies gate the epic, not which row each one gates.
// [LAW:polishing-by-subtraction]
func blockedRows(deps []gatedDep) []rowReach {
	rows := make([]rowReach, 0, len(deps))
	for _, dep := range deps {
		rows = append(rows, dep.rowReach)
	}
	return rows
}

// workTowardEach indexes the gathered rows, in rank order, by every dependency
// finishing them is work toward: each row under its own id, and under every
// epic above it at any depth. That makes a leaf dependency and an epic one the
// same lookup — nothing sits under a leaf, and an epic is never a gathered row,
// so a leaf finds itself and an epic finds its tickets. It climbs the same
// epicsAbove the blocker annotator passes an epic's blockers down with, so
// "under this epic" means one thing to readiness and to routing.
// [LAW:one-source-of-truth] [LAW:dataflow-not-control-flow]
func workTowardEach(rows []annotation.AnnotatedIssue, details, epics map[string]storage.IssueRelations) map[string][]annotation.AnnotatedIssue {
	work := make(map[string][]annotation.AnnotatedIssue, len(rows))
	for _, row := range rows {
		work[row.ID] = append(work[row.ID], row)
		for epic := range epicsAbove(details[row.ID], epics) {
			work[epic] = append(work[epic], row)
		}
	}
	return work
}

// gatingDependencies collects the distinct open dependencies that gate the open
// rows whose lane inScope admits, in rank order, each already carrying what
// this checkout may do about it. Both consumers are this walk plus one
// question: onPathDependency offers the first takeable one, and Exhausted
// reports them all so the diagnostic can say which is which. They differ in the
// scope they pass and in what they do with the answer — never in how it is
// found, and neither re-derives it. [LAW:one-source-of-truth]
//
// A dependency is classified by the work toward it, not by its own row, which
// an epic never has: it takes the best kind among that work, carried by the
// first row in rank order to reach it, so an epic whose one ticket another
// checkout holds reads as held, not as outside the view. With no work in view
// it is reachOutOfView, which every row's kind outranks.
//
// Only dependencies from outside the scope are collected. One whose work sits
// inside it is already covered by the scope's own walk: the pick before each
// call served it if it could be served, and when it is itself blocked it is an
// open row in scope, so this walk collects what holds it back instead. Naming
// it would offer a sibling's block to be cleared by a ticket filed beside it,
// an edge lit refuses inside one epic — and a sibling edge can still exist,
// written by an import or before that rule.
func gatingDependencies(rows []annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID, inScope func(model.LaneID) bool, reachFor func(annotation.AnnotatedIssue) reachKind, workToward map[string][]annotation.AnnotatedIssue) []gatedDep {
	seen := map[string]bool{}
	var deps []gatedDep
	for _, row := range rows {
		if !inScope(laneOf(row)) || row.State() != model.StateOpen {
			continue
		}
		for _, id := range ClassifyReadiness(row.Annotations).DependencyIDs() {
			if seen[id] {
				continue
			}
			seen[id] = true
			dep := rowReach{ID: id, Kind: reachOutOfView}
			for _, work := range workToward[id] {
				if kind := reachFor(work); kind < dep.Kind {
					dep.Row, dep.Kind = work, kind
				}
			}
			if dep.Kind != reachOutOfView && inScope(laneOf(dep.Row)) {
				continue
			}
			// row is the in-scope open row whose dependency this is — the fact
			// step 1b needs.
			// `seen` keeps the FIRST row to reach a dependency, so when one
			// dependency gates several of ours, Gates names the one the queue
			// ranks first. That is the intent, not a leftover: the pick owes the
			// reader a reason, not an inventory, and the actionable reason is the
			// highest-ranked thing it unblocks. Naming every gated row would put
			// an unbounded list in a one-line announcement.
			// [LAW:polishing-by-subtraction]
			deps = append(deps, gatedDep{rowReach: dep, Gates: row.ID})
		}
	}
	return deps
}

// onPathDependency finds, among the dependencies one gatingDependencies walk
// found, the first work this checkout may itself take toward closing one — "a
// dependency outside the claimed lane that gates it is offered as on-path"
// (design-docs/work-claims.md, Routing step 1). Step 1b asks it of our own
// lanes and step 2b of our whole epic. A gate inside the scope never reaches
// here: the walk leaves it out, since step 1 or 2 already served it.
//
// The pick is the row gatingDependencies classified the dependency by: the
// dependency itself when it is a leaf, a ticket under it when it is an epic,
// which cannot be started.
//
// Takeability is the shared verdict, not a local readiness test. A local test
// sees no standings, so it would offer a ticket sitting in a lane another
// checkout holds fresh — which `lit start` then refuses, so `next` would
// recommend what `start` blocks. It does not re-check that the gated row is
// unservable: the steps before each call already took every servable row the
// walk's scope admits, so by the time we are here each of them is routeAround
// by construction.
func onPathDependency(deps []gatedDep, laneOf func(annotation.AnnotatedIssue) model.LaneID) (ServedFromDependency, bool) {
	for _, dep := range deps {
		if dep.Kind == reachTakeable {
			return ServedFromDependency{Row: dep.Row, Lane: laneOf(dep.Row), Gates: dep.Gates, Blocker: dep.ID}, true
		}
	}
	return ServedFromDependency{}, false
}

// Error renders Exhausted as the loud diagnostic it is when the pool offered
// nothing outside the scope either, so staying is the only route it can name —
// and only against a block, since that is all a new ticket could clear.
func (o Exhausted) Error() string {
	return strings.Join(append([]string{o.why() + "; " + o.outside()}, o.stay()...), " — ")
}

// why is what both exhaustion outcomes say first: which scope stopped, and
// what stops it — the dependencies from outside it, then its own rows held by
// themselves. Only with neither is the rest of the scope work already in
// progress, and only then does it say so.
func (o Exhausted) why() string {
	stops := slices.DeleteFunc([]string{
		describeReach(o.Blocked, "blocked on ", exhaustedNotes),
		describeReach(o.Held, "held here: ", exhaustedNotes),
	}, func(clause string) bool { return clause == "" })
	if len(stops) == 0 {
		stops = []string{"nothing else is queued behind what's already in progress"}
	}
	return fmt.Sprintf("no ready work in %s — %s", o.scope(), strings.Join(stops, "; "))
}

// outside is what the pool held beyond the scope when it offered nothing: no
// ready row at all, or rows a focus label kept it from asking about, named so
// the agent can widen the question rather than read their absence as none.
// [LAW:no-silent-failure]
func (o Exhausted) outside() string {
	if len(o.OffPath) == 0 {
		return "`lit next` has nothing ready outside it either"
	}
	return "nothing on the focus path outside it is ready either: " + describeReach(o.OffPath, "", poolNotes)
}

// stay is the route that keeps the checkout in its scope, as a list of at most
// one so both renderers join it beside the others unconditionally. It exists
// only against a block a ticket filed here could clear: with Blocked empty the
// rest of the scope is work already underway, and a blocker awaiting an outside
// event is cleared by that event, never by a ticket here. Filing alone clears
// nothing either — the blocked work still waits on its blocker — so the route
// names the edge too, and names it exactly: the blocker waits on the new
// ticket. The blocker to name is one outside the scope; lit refuses a blocks
// edge between two tickets of one epic.
func (o Exhausted) stay() []string {
	clearable := slices.ContainsFunc(o.Blocked, func(blocker rowReach) bool {
		return blocker.Kind != reachAwaitingOutside
	})
	if !clearable {
		return nil
	}
	return []string{"to stay, file the ticket that clears a blocker " + o.home() + ", then make that blocker wait on it with `lit dep add --from <new> --to <blocker>`"}
}

// home is where the unblocking ticket is filed, spelled as the command: under
// the epic, ranked ahead of the work it unblocks, with each epic's command
// spelled out so the agent copies an id rather than filling in a placeholder.
// That holds when the block is declared on the epic itself too: an epic's
// blocker never holds back a child it waits on (heldAncestry), so the edge that
// makes the blocker wait on the new ticket is also what frees it. A lane with
// no epic over it files at the top of the backlog, where the pool serves it.
func (o Exhausted) home() string {
	if len(o.Epics) == 0 {
		return "with `lit new --top`"
	}
	commands := make([]string, len(o.Epics))
	for i, epic := range o.Epics {
		commands[i] = fmt.Sprintf("`lit new --parent %s --top`", epic)
	}
	return "under the epic with " + strings.Join(commands, " or ")
}

// scope names the stopped scope.
func (o Exhausted) scope() string {
	if len(o.Epics) == 0 {
		return "your claimed lane(s)"
	}
	return fmt.Sprintf("epic(s) %s", strings.Join(o.Epics, ", "))
}

// reachNotes is what a diagnostic says about the rows of each kind, indexed by
// the kind itself. Both diagnostics group the same type the same way and differ
// only in this wording and in the lead introducing each clause, so the grouping
// is written once and the words travel as data.
// [LAW:dataflow-not-control-flow]
//
// An array indexed by the kind, not a list of pairs: a pair list can omit a
// kind and silently drop its ids from the message, which is the class of defect
// this whole type exists to end. The array cannot drop them — a kind with no
// words still renders its ids, under an empty parenthetical, which is loud
// rather than silent. It is not total on its own, since Go does not require an
// indexed array literal to fill every slot, so
// TestEveryReachKindHasWordsInBothDiagnostics closes that gap — in both
// directions, because a slot a diagnostic can never reach is its own defect:
// worded text for an impossible kind reads as a capability the walk does not
// have. [LAW:types-are-the-program] [LAW:no-silent-failure]
type reachNotes [reachKindCount]string

// The wording each diagnostic carries, named so the totality test can reach
// them and so neither is rebuilt on every render. Each walk words exactly the
// kinds it can stamp, and the two sets differ at both ends.
//
// reachOffFocusPath is the pool walk's alone, stamped by withheldByScope —
// Exhausted.OffPath reports it too, in the pool's words, because those are
// rows the pool withheld. reachOutOfView is the exhaustion walk's alone:
// passedOver classifies gathered rows only, where gatingDependencies reaches
// deps the gather never returned.
// reachTakeable is neither's. NoWork is answered only to a checkout holding no
// lane, so no pool row is laneOurs and capacityFor cannot answer resumeWork,
// while the pick just declined every serveWork over that same set — leaving
// routeAround as the only verdict passedOver can see. And exhaustion reports
// the very walk step 2b just declined, so it never holds a takeable one.
var (
	exhaustedNotes = reachNotes{
		reachHeldFresh:       "on your path but claimed by another checkout right now",
		reachNotReady:        "on your path but not startable right now — `lit show` it",
		reachAwaitingOutside: "on your path but waiting on an event outside this repository — `lit show` it names the event",
		reachOutOfView:       "on your path but outside this view — `lit show` it",
	}
	poolNotes = reachNotes{
		reachHeldFresh:       "in progress or claimed in a lane another checkout holds right now",
		reachNotReady:        "not startable right now — `lit show` it names what blocks it",
		reachAwaitingOutside: "waiting on an event outside this repository — `lit show` it names the event",
		reachOffFocusPath:    "off the focus path this run answered over — `lit next --all` to route over the whole queue",
	}
)

// maxNamedPerKind caps how many ids one clause names outright. NoWork's rows
// are every row the pool walk went past — the whole filtered backlog, not the
// epic-scoped handful Exhausted reports — so an uncapped join answers a busy
// workspace with a single line of hundreds of ids, which is worse for the agent
// reading it than a dozen and a count. Naming some is what makes the diagnostic
// actionable; naming all of them is what makes it unreadable.
const maxNamedPerKind = 12

// nameIDs names at most maxNamedPerKind ids and states how many it left out, so
// the clause reads the same at any pool size. The remainder is counted rather
// than dropped: an agent told "and 135 more" still learns the true scale of what
// it cannot start. [LAW:no-silent-failure]
func nameIDs(ids []string) string {
	// [LAW:dataflow-not-control-flow] the last inch of rendering, where the two
	// arms are different sentences rather than an operation one of them skips.
	if len(ids) <= maxNamedPerKind {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:maxNamedPerKind], ", "), len(ids)-maxNamedPerKind)
}

// describeReach renders "<lead><ids> (<note>)" for each kind that has rows,
// joined by "; ", in reachKind's own declaration order — one ordering for both
// diagnostics rather than a per-caller one that could disagree. The cap lives
// here for the same reason the ordering does: both diagnostics render through
// this one function, so neither can grow a second answer to how long a clause
// may get. [LAW:one-source-of-truth] [LAW:single-enforcer]
func describeReach(rows []rowReach, lead string, notes reachNotes) string {
	byKind := map[reachKind][]string{}
	for _, row := range rows {
		byKind[row.Kind] = append(byKind[row.Kind], row.name())
	}
	var parts []string
	for kind, note := range notes {
		ids := byKind[reachKind(kind)]
		if len(ids) == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s%s (%s)", lead, nameIDs(ids), note))
	}
	return strings.Join(parts, "; ")
}

// Error names which emptiness this is.
//
// Nothing walked past: the gather came back empty and the backlog really is.
// Rows walked past: they are named and classified, because the fact is then the
// opposite one — the pool was full, of work this checkout may not have. The
// clause leads by refuting the empty reading outright, since that reading tells
// an agent asking, after a crash, what it was already on that its work is gone.
func (o NoWork) Error() string {
	if len(o.Unreachable) == 0 {
		return "no ready work"
	}
	// A third emptiness, and it needs its own lead: with rows withheld by the
	// focus scope, "nothing in it is startable here" is false — those rows were
	// never asked. The lead claims neither startability nor location, because
	// Unreachable mixes two populations here: on-path rows step 4 walked and
	// rejected, and off-path rows it never examined. A header asserting either
	// fact over the whole list is false for one of them — and the on-path half
	// is the news the agent has to act on. Only poolNotes can tell them apart,
	// and it already does, per row.
	if o.withheld() {
		return fmt.Sprintf("no ready work on the focus path — the backlog is not empty, and each row below says why this run did not serve it: %s", describeReach(o.Unreachable, "", poolNotes))
	}
	return fmt.Sprintf("no ready work — the backlog is not empty, but nothing in it is startable here: %s", describeReach(o.Unreachable, "", poolNotes))
}

// withheld reports whether the focus scope kept any row out of the pool this
// walk went through. Read off the rows the outcome already carries, so the
// sentence and the clauses beneath it cannot disagree. [LAW:one-source-of-truth]
func (o NoWork) withheld() bool {
	for _, row := range o.Unreachable {
		if row.Kind == reachOffFocusPath {
			return true
		}
	}
	return false
}
