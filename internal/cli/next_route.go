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
type ServedFromNewLane struct{ poolPick }

// poolPick is a row the global pool served, with the lane starting it would
// claim and the focus scope the pool answered over. The scope is the one
// routeNext partitioned the pool by, carried out of the walk rather than
// re-derived by the renderer, so the pick says which question produced it: the
// focus path, or — the zero scope — the whole queue. Only the outcomes the pool
// builds embed it, so a pick from steps 1-2b, which no scope narrowed, has no
// field through which it could claim one. [LAW:types-are-the-program]
// [LAW:one-source-of-truth]
//
// Lane is the LaneID and not its String(): the rendering belongs to whoever
// knows the reader, and stringifying here would throw away the discriminator
// the renderer needs to tell a lane worth naming from one that would only
// repeat the ticket. [LAW:types-are-the-program]
type poolPick struct {
	Row   annotation.AnnotatedIssue
	Lane  model.LaneID
	Scope focusScope
}

// ServedFromDependency is routing step 1b or 2b: a ready ticket OUTSIDE our
// lanes that is work toward a dependency gating one of our blocked rows — in a
// lane we hold (1b), or anywhere in our epic (2b). Blocker is that dependency —
// Row's own id when it is a leaf, or an epic Row sits under, since an epic
// cannot be started — and Gates is the row of ours it holds back. Via is the
// chain of prerequisites between them, nearest Gates first, when Blocker gates
// Gates only through prerequisites this checkout cannot take itself — not
// ready, or in a lane another checkout holds; it is
// empty when Blocker is one of Gates' own. Like
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
	Via     []hop
	Blocker string
}

// hop is one link of a prerequisite chain: the prerequisite, and the ticket the
// walk went on through toward the next — the prerequisite itself when it is a
// ticket, or one under it when it is an epic, since an epic cannot be started
// and readiness passes its own prerequisites down to the tickets under it as
// inherited ones. Both are kept because each is half of what a reader checks:
// `lit show` on the gated row lists the epic, and `lit show` on the next link
// lists the ticket.
type hop struct {
	Prerequisite string
	Through      string
}

// ids is the hop as the ids it passes, the epic before the ticket under it.
func (h hop) ids() []string {
	if h.Through == h.Prerequisite {
		return []string{h.Prerequisite}
	}
	return []string{h.Prerequisite, h.Through}
}

// name is how an announcement names the hop. [LAW:one-source-of-truth]
// workName is the one phrasing of work under an epic.
func (h hop) name() string { return workName(h.Prerequisite, h.Through) }

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
	poolPick
	Exhaustion Exhausted
}

// Narrowed is a run that hands back nothing only because of its filter: no
// step serves a row the filter selects, but the same steps over the whole queue
// would serve Hidden — the held scope's next ticket at step 3, or the pool's
// top ready ticket at step 4 for a checkout holding no lane. Neither the scope
// nor the backlog is out of work, and saying so would be false — the question
// was narrowed, not the work. Hidden is that unfiltered pick, so the agent
// learns exactly which ticket dropping the filter hands it.
//
// Scope is the focus scope Hidden was drawn under: the pool's when the pool
// offered it, so a focus that also kept rows the filter selects out of the
// pool is said beside it as on a pool pick, and the zero scope for the held
// scope's next ticket, which no scope narrows.
//
// It is an error on the same terms as Exhausted and NoWork, whose answers it
// replaces when the filter is the reason for them.
// [LAW:types-are-the-program] [LAW:no-silent-failure]
type Narrowed struct {
	Hidden annotation.AnnotatedIssue
	Scope  focusScope
}

// ServedPastNarrowing is Narrowed with somewhere to go: the pool's top ready
// ticket the filter selects, which lies outside the scope, served beneath the
// ticket the filter hid. Like ServedPastExhaustion it carries the whole reason
// rather than a flag, so the one sentence that names the hidden ticket is
// written once, on Narrowed.
type ServedPastNarrowing struct {
	poolPick
	Narrowing Narrowed
}

// Exhausted is the checkout's own claimed epic(s) having open work with none
// of it reachable: not the held lanes, not an on-path dependency, not another
// lane of the same epic — and the global pool having nothing ready outside it
// either, or routing would have answered ServedPastExhaustion. Loud and
// diagnostic. Blocked names the open dependencies from outside the scope gating
// that work, if any, in the order the walk reached them. Held names the scope's
// own open rows held by a reason about the row itself — a reserved label, a
// missing field — which no other work finishing will clear; a scope with
// neither is waiting only on work already in progress. OffPath names the rows
// outside the scope that a focus label kept out of the pool, so "nothing ready
// outside it" is never claimed of rows the pool was not asked about; with no
// focus active it is empty, and on ServedPastExhaustion, whose pool did offer a
// row.
//
// Progress is how far the stopped scope has come, counted over every lane of
// it, closed tickets included: "blocked" and "eight-ninths done and blocked"
// are different answers to an agent deciding whether to stay.
//
// Narrowing is set when the pool offered nothing the filter selects but would
// serve a row over the whole queue: the filter, not the backlog, is why nothing
// outside the scope is on offer, and that row is what `lit next` without the
// filter hands over. OffPath is then empty, as on ServedPastExhaustion, since
// the unfiltered pool did offer a row.
//
// It is also an error, for the same reason HelpRequestedError is: it is an
// answer, not a failure, and the error channel is the only channel runNext has
// for an answer that hands back no row. Being the error itself — rather than
// being rendered into a separate error type — is what keeps the error
// classifier reading the routing verdict rather than a copy of it that could drift
// from these fields. [LAW:one-source-of-truth]
type Exhausted struct {
	Epics     []string
	Progress  claims.Progress
	Blocked   []blockerReach
	Held      []rowReach
	OffPath   []string
	Narrowing *Narrowed
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
// The kinds are exactly reachOf's answers, declared from least to most this
// checkout can do, which is what lets a dependency with several tickets under
// it report the best of them (gatingDependencies). What a diagnostic reports
// that is not an answer about a gathered row — a dependency with no work in
// view, a row the focus scope withheld — is its own list on the outcome, never
// a kind, so no table of words can carry a slot for it that its walk cannot
// fill.
type reachKind int

const (
	// reachAwaitingOutside: gathered and not held elsewhere, and held by the
	// external label — it waits on an event outside this repository, so no
	// ticket filed here can clear it. Below reachNotReady because nothing here
	// moves it.
	reachAwaitingOutside reachKind = iota
	// reachNotReady: gathered and not held elsewhere, but not startable —
	// blocked by further open tickets and by nothing about itself.
	reachNotReady
	// reachHeldByItself: gathered and not held elsewhere, but not startable
	// for a reason about the row itself — a reserved label, a missing field —
	// which someone here can settle, whatever else it also waits on. Above
	// reachNotReady because nothing but that settles it, and a ticket that
	// waits only on others is cleared by work already named.
	reachHeldByItself
	// reachHeldFresh: another checkout holds its lane right now.
	reachHeldFresh
	// reachTakeable: this checkout may start it. Declared last because it is
	// also the length of every table that words a kind (reachNotes, the event's
	// names): routing serves a takeable row instead of reporting it, so a word
	// for one is an index past the table's end, which does not compile.
	// [LAW:types-are-the-program]
	reachTakeable
)

// reading is what one gathered row says to this checkout: the row, and what
// this checkout may do about it.
type reading struct {
	Row  annotation.AnnotatedIssue
	Kind reachKind
}

// rowReach is a row a walk went past, carrying what this checkout may do about
// it. Row is the gathered row Kind was read from: the row itself, or for a
// dependency, the ticket that would clear it.
type rowReach struct {
	ID string
	reading
}

// name is how a diagnostic names the entry. A dependency read through a ticket
// under it is named as both, "<ticket> under <epic>": the note's `lit start` or
// `lit show` acts on the ticket, since an epic cannot be started, and the epic
// is what blocks. Every other entry is its own id. [LAW:dataflow-not-control-flow]
// the last inch of rendering, where the two arms are different phrases.
func (r rowReach) name() string { return workName(r.ID, r.Row.ID) }

// workName names a dependency by the work read through it: "<work> under
// <dependency>" when the work is a ticket under it, and the dependency's own id
// when the work is the dependency itself or there is none in view.
func workName(dependency, work string) string {
	if work == "" || work == dependency {
		return dependency
	}
	return fmt.Sprintf("%s under %s", work, dependency)
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
	case ClassifyReadiness(row.Annotations).HeldByItself():
		return reachHeldByItself
	}
	return reachNotReady
}

// NoWork is the global pool handing back nothing. Unreachable is every row that
// walk went past, each carrying why, and OffPath every row the focus scope kept
// out of the pool, which the walk never asked about — both empty exactly when
// the gather itself came back empty, which is the genuinely empty backlog. An
// error on the same terms as Exhausted.
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
type NoWork struct {
	Unreachable []rowReach
	OffPath     []string
}

func (ServedFromClaim) isNextOutcome()      {}
func (ResumedOwnWork) isNextOutcome()       {}
func (ServedFromEpicLane) isNextOutcome()   {}
func (ServedFromNewLane) isNextOutcome()    {}
func (ServedFromDependency) isNextOutcome() {}
func (ServedPastExhaustion) isNextOutcome() {}
func (ServedPastNarrowing) isNextOutcome()  {}
func (Exhausted) isNextOutcome()            {}
func (Narrowed) isNextOutcome()             {}
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
// [LAW:one-source-of-truth] It reads standings and NOT the gathered rows. A
// lane is held whether or not any of its tickets is workable right now: one
// whose tickets are all closed gathers no rows, and deriving ownership from the
// rows would drop the whole self-aware branch — a checkout with a perfectly
// fresh claim silently hopping epics. Ownership is a fact about the workspace,
// and nothing the rows happen to hold may change it.
//
// An unidentified self needs no guard here: relationOf owns what a
// public-checkout self may match, so ownership and the start gate cannot drift
// apart on it. [LAW:single-enforcer]
func ownScope(standings claims.Standings, self model.Attribution) ownWork {
	own := ownWork{lanes: map[model.LaneID]bool{}, epics: map[string]bool{}}
	for lane, standing := range standings {
		if relationOf(standing, self) != laneOurs {
			continue
		}
		own.lanes[lane] = true
		if lane.Epic() != "" {
			own.epics[lane.Epic()] = true
		}
	}
	return own
}

// ownWork is this checkout's own work: the lanes it holds and the epics those
// lanes sit in. `lit next` routes over it before anything else, and the focus
// scope never narrows it away, so both read "ours" from this one value rather
// than each rebuilding it from the standings. [LAW:one-source-of-truth]
//
// The zero value holds nothing, which is the checkout that holds no lane.
type ownWork struct {
	lanes map[model.LaneID]bool
	epics map[string]bool
}

// holds reports whether this checkout holds the lane itself.
func (o ownWork) holds(lane model.LaneID) bool { return o.lanes[lane] }

// covers reports whether the lane is this checkout's own work: a lane it holds,
// or any lane of an epic one of those sits in — the epic-major rule, which
// keeps `lit next` serving the epic in flight until it has nothing workable.
// A lane of one has no epic, and epics never holds the empty id, so holding a
// lane of one covers that lane alone.
func (o ownWork) covers(lane model.LaneID) bool {
	return o.holds(lane) || o.epics[lane.Epic()]
}

// holdsAny reports whether this checkout holds a lane at all.
func (o ownWork) holdsAny() bool { return len(o.lanes) > 0 }

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
// rows is the whole workable queue and asked is the filter the run was given
// (--type/--status/--labels/--assignee). The filter decides which rows may be
// SERVED and nothing else: every walk reads every row, so a dependency is found
// through a ticket the filter leaves out, and "is the scope exhausted" — or
// "is anything ready" — is asked of the queue rather than of the part of it the
// filter kept. A filter applied to the rows before routing would make those
// answers false: under --type bug, a ready task of ours would read as an
// exhausted epic, and a backlog of ready tasks as no ready work.
//
// The focus scope narrows STEP 4 AND NOTHING ELSE. Steps 1-3 route over lanes
// this checkout already holds, and ownScope takes pains to read that from the
// standings rather than the gathered rows precisely so a display narrowing
// cannot empty the self-aware branch. A scope applied to `rows` before routing
// would re-open exactly that hole from the other side: a checkout holding a
// ticket off the focused path would be told to start something else while its
// own work sat in flight. Focus decides where a fresh session goes; it does not
// decide whether your own work is still yours. The scope states that rule
// itself — it owns this checkout's work (focusScope.owning) — so `lit backlog`,
// which has no steps to order, lists that work by the same rule.
//
// [LAW:dataflow-not-control-flow] Every step walks the same rows in the same
// rank order and asks capacityFor the same question; a step differs
// only in which lanes it admits and which verdicts it accepts. No step decides
// eligibility on its own.
//
// epics is the relations of the epics above the rows, keyed by epic id — what
// epicsAbove climbs to tell which rows sit under an epic. evidence is the
// reading standings were derived from; routing counts a stopped scope's
// progress from it, since rows hold no closed ticket to count.
func routeNext(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, epics map[string]storage.IssueRelations, standings claims.Standings, evidence claims.Evidence, self model.Attribution, scope focusScope, asked storage.IssueCriteria) NextOutcome {
	laneOf := func(row annotation.AnnotatedIssue) model.LaneID {
		return model.LaneOf(row.Issue, details[row.ID].Parent)
	}
	verdict := func(row annotation.AnnotatedIssue) capacity {
		return capacityFor(row, standings.Of(laneOf(row)), self)
	}
	reachFor := func(row annotation.AnnotatedIssue) reachKind {
		return reachOf(row, standings.Of(laneOf(row)), self)
	}
	selected := func(row annotation.AnnotatedIssue) bool { return asked.Selects(row.Issue) }
	anyRow := func(annotation.AnnotatedIssue) bool { return true }
	// pickFrom keeps the first row, in rank order, that the run may serve, sits
	// in an admitted lane and carries one of the accepted verdicts.
	//
	// accept is a SET and never a preference order: rank order is the only
	// tiebreak routing gets to apply, and ranking capacities against each other
	// would pass over the backlog's #1 row, abandoned in flight, for a
	// lower-ranked leaf that is merely ready. [LAW:one-source-of-truth] one
	// ordering, and the gather already established it.
	// pickFrom takes the row set explicitly because the steps do not share
	// one: steps 1-3 walk every gathered row, step 4 walks the focus-scoped
	// pool. Passing the set is what keeps that difference visible at each call
	// instead of hidden in a closure every step reads differently.
	pickFrom := func(from []annotation.AnnotatedIssue, serves func(annotation.AnnotatedIssue) bool, inScope func(model.LaneID) bool, accept ...capacity) (annotation.AnnotatedIssue, capacity, bool) {
		for _, row := range from {
			if how := verdict(row); serves(row) && inScope(laneOf(row)) && slices.Contains(accept, how) {
				return row, how, true
			}
		}
		return annotation.AnnotatedIssue{}, routeAround, false
	}

	// Step 4's pick, shared by the step that falls through to it. The pool is
	// the focus-scoped one — see the step 4 comment below — and, as at every
	// step, serves admits what may be served from it: the filter for the run's
	// answer, every row for the answer the filter would otherwise have got.
	// The pick carries that scope out, so what the run says it answered over is
	// the scope it did answer over.
	//
	// The pool holds this checkout's own work too, since the scope owns it, and
	// that changes no pick: step 4 is reached directly only when own holds
	// nothing, and the own branch consults the pool only after steps 1-2b took
	// every row of ours it could serve.
	own := ownScope(standings, self)
	scope = scope.owning(own)
	pool, offPath := scope.partition(rows, details)
	fromPool := func(serves func(annotation.AnnotatedIssue) bool) (poolPick, bool) {
		row, _, ok := pickFrom(pool, serves, func(model.LaneID) bool { return true }, serveWork)
		return poolPick{Row: row, Lane: laneOf(row), Scope: scope}, ok
	}
	// The diagnostics name only rows the filter selects: what they report is
	// why this run's question got no row.
	selectedOf := func(from []annotation.AnnotatedIssue) []annotation.AnnotatedIssue {
		return slices.DeleteFunc(slices.Clone(from), func(row annotation.AnnotatedIssue) bool { return !selected(row) })
	}

	if own.holdsAny() {
		workToward := workTowardEach(rows, details, epics)
		// Both dependency walks read every row and are shared by both runs of
		// ownSteps; the walk step 2b declines is the walk step 3 reports, so
		// what exhaustion names is exactly what that step passed over.
		// [LAW:one-source-of-truth]
		gatingMine := gatingDependencies(rows, laneOf, own.holds, reachFor, workToward)
		gating := gatingDependencies(rows, laneOf, own.covers, reachFor, workToward)
		// ownSteps is steps 1-2b for a run that may serve only the rows serves
		// accepts. It runs once with the filter, and — only when that finds
		// nothing — once over the whole queue, which is how step 3 tells a
		// scope with no reachable work from one whose next ticket the filter
		// left out. One function for both runs, so the two cannot disagree about
		// what the scope would serve. [LAW:one-source-of-truth]
		ownSteps := func(serves func(annotation.AnnotatedIssue) bool) (NextOutcome, annotation.AnnotatedIssue, bool) {
			// Step 1 — our own lanes, startable work and work already underway
			// alike, whichever the backlog ranks first.
			if row, how, ok := pickFrom(rows, serves, own.holds, serveWork, resumeWork); ok {
				if how == resumeWork {
					return ResumedOwnWork{Row: row}, row, true
				}
				return ServedFromClaim{Row: row}, row, true
			}
			// Step 1b — a dependency outside our lanes that gates one of them. It
			// establishes a claim on a lane we do not hold, so it is announced as
			// one and its own lane's standing is honoured rather than ignored.
			if served, ok := onPathDependency(gatingMine, serves, reachFor, workToward, laneOf); ok {
				return served, served.Row, true
			}
			// Step 2 — the rest of our epic, in lanes we do not already hold.
			if row, _, ok := pickFrom(rows, serves, func(lane model.LaneID) bool { return own.covers(lane) && !own.holds(lane) }, serveWork); ok {
				return ServedFromEpicLane{Row: row, Lane: laneOf(row)}, row, true
			}
			// Step 2b — a dependency outside our epic that gates any of it.
			// Clearing it is how the epic moves again, so it outranks every
			// ticket the pool could offer; after step 2, because a ready lane of
			// the epic moves it without leaving it.
			if served, ok := onPathDependency(gating, serves, reachFor, workToward, laneOf); ok {
				return served, served.Row, true
			}
			return nil, annotation.AnnotatedIssue{}, false
		}
		if outcome, _, ok := ownSteps(selected); ok {
			return outcome
		}
		// Step 3 — loud, and never a silent hop. The pool's pick is outside our
		// scope by construction: steps 1 and 2 took every serveWork row the
		// filter selects in our lanes and our epic, and the pick serves only
		// rows it selects.
		if _, hidden, ok := ownSteps(anyRow); ok {
			narrowing := Narrowed{Hidden: hidden}
			if pick, ok := fromPool(selected); ok {
				return ServedPastNarrowing{poolPick: pick, Narrowing: narrowing}
			}
			return narrowing
		}
		exhausted := Exhausted{
			Epics:    slices.Sorted(maps.Keys(own.epics)),
			Progress: evidence.ScopeProgress(own.covers),
			Blocked:  blockersOf(gating, workToward),
			Held:     heldByThemselves(rows, func(row annotation.AnnotatedIssue) bool { return own.covers(laneOf(row)) }, reachFor),
		}
		if pick, ok := fromPool(selected); ok {
			return ServedPastExhaustion{poolPick: pick, Exhaustion: exhausted}
		}
		// Nothing outside our scope is ready that the filter selects. Over the
		// whole queue the pool may still serve a row, and then the filter is why
		// nothing outside is on offer: "nothing ready outside it" would be false.
		if hidden, ok := fromPool(anyRow); ok {
			exhausted.Narrowing = &Narrowed{Hidden: hidden.Row, Scope: hidden.Scope}
			return exhausted
		}
		// The pool offered nothing, so what the focus kept from it is the rest
		// of the answer: the withheld rows. None of them is ours — the scope
		// owns our work, which steps 1-2b walked, focus or not.
		exhausted.OffPath = withheldByScope(selectedOf(offPath))
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
	//
	// "No ready work" is asked of the queue, as exhaustion is: when only the
	// filter emptied the pool, the answer is the ticket it hid.
	if pick, ok := fromPool(selected); ok {
		return ServedFromNewLane{pick}
	}
	if hidden, ok := fromPool(anyRow); ok {
		return Narrowed{Hidden: hidden.Row, Scope: hidden.Scope}
	}
	return NoWork{Unreachable: passedOver(selectedOf(pool), reachFor), OffPath: withheldByScope(selectedOf(offPath))}
}

// withheldByScope names the rows the focus scope kept out of the pool. What
// sets them apart is the question rather than the row — the walk never asked
// capacityFor about them — so they travel as ids on a list of their own
// rather than as a kind reachOf could be asked for, and no diagnostic can word
// them as a verdict about the row. [LAW:types-are-the-program]
func withheldByScope(rows []annotation.AnnotatedIssue) []string {
	return rowIDs(rows)
}

// rowIDs names rows by id, in the order given.
func rowIDs(rows []annotation.AnnotatedIssue) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

// passedOver classifies every row in the pool it is handed, in the rank order
// the pool walk went through them. It is called only where that walk found
// nothing, so every row here is routeAround by construction — a takeable one
// would have been served — which lets NoWork say why the pool was empty without
// asking the data a second question.
//
// The pool is every gathered row only when no focus scope narrowed it. Under a
// scope the walk never sees the off-path rows at all: withheldByScope names
// those, on NoWork.OffPath, so it is the two lists together — not this
// function — that account for every gathered row. [LAW:one-source-of-truth]
func passedOver(rows []annotation.AnnotatedIssue, reachFor func(annotation.AnnotatedIssue) reachKind) []rowReach {
	passed := make([]rowReach, 0, len(rows))
	for _, row := range rows {
		passed = append(passed, rowReach{ID: row.ID, reading: reading{Row: row, Kind: reachFor(row)}})
	}
	return passed
}

// gatedDep is one gating dependency together with the row it gates and the
// chain between them. The gated id is the whole reason step 1b's pick is worth
// explaining, and this walk is the only place it is ever in scope: the walk
// holds the blocked row and its dependency at the same instant. Keeping only
// the dependency would leave the renderer unable to say why the pick was handed
// over, and dropping Via would leave it claiming an edge `lit show` cannot find.
//
// It is a field of this walk's element rather than of rowReach because rowReach
// serves two other walks — passedOver and heldByThemselves — for which a gated
// id is meaningless and would always be empty. A field that only one producer
// ever fills is a bag of optionals in miniature, and every later reader would
// have to learn which walks populate it. [LAW:types-are-the-program]
type gatedDep struct {
	depReach
	Gates string
	Via   []hop
}

// depReach is a dependency as the work toward it reads. Best is the work this
// checkout can do the most about, with that kind. It carries no id: the
// dependency's is ID alone, so the entry a diagnostic names and the edge the
// route to stay offers cannot be two different ids. [LAW:one-source-of-truth]
//
// Best is nil when none of that work is among the gathered rows, which puts the
// dependency outside this view: this run knows nothing about it — an epic with
// no open ticket under it, say. Leaf-only membership narrows the gather; the
// dependency annotation is read from the store and does not, and no filter
// narrows what routing reads (routeNext). That is a fact about what the run
// could see, not an answer reachOf gives, so it is the absence of a reading
// rather than a kind of one. [LAW:types-are-the-program]
type depReach struct {
	ID   string
	Best *reading
}

// read is the dependency as the row a diagnostic names it by, and false when
// the run could see none of the work toward it.
func (d depReach) read() (rowReach, bool) {
	if d.Best == nil {
		return rowReach{}, false
	}
	return rowReach{ID: d.ID, reading: *d.Best}, true
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
		if inScope(row) && (kind == reachHeldByItself || kind == reachAwaitingOutside) && ClassifyReadiness(row.Annotations).HeldByItself() {
			held = append(held, rowReach{ID: row.ID, reading: reading{Row: row, Kind: kind}})
		}
	}
	return held
}

// blockerReach is a dependency gating the scope, as Exhausted reports it: what
// this checkout may do about it, and whether a ticket filed here could free it
// — what the route to stay offers to clear. The second is a fact about the
// blocker alone, so it sits beside the reading on the entry, where no other
// list's row could claim it and no id could carry it that Blocked does not.
// [LAW:types-are-the-program]
type blockerReach struct {
	depReach
	Clearable bool
}

// blockersOf reports the dependencies one walk found for Exhausted, in the
// order the walk reached them. It drops the gated id — that diagnostic reports
// WHICH dependencies gate the epic, not which row each one gates — and reads
// off the work toward each whether a ticket filed here could free it.
//
// A dependency closes only when all of that work does — a leaf when it does,
// an epic when every ticket under it does — so one ticket of it awaiting an
// outside event leaves it to that event, never to a ticket here, and one still
// waiting on other open tickets leaves it to them, whoever holds its lane and
// whatever about itself also holds it. The walk is transitive, so each of
// those tickets is named in Blocked beside it, named in Held, or underway in
// our scope in another checkout — a chain whose every end awaits an outside
// event has nothing here to file against. Prerequisites count as capacityFor
// counts them: work in flight is past them, and the walk does not descend it,
// while the external label holds work in every state. Read off all the work,
// not the kind: the kind is the best of that work, and a held kind outranks
// not-ready, so it keeps no trace of what the rest waits on. With no work in
// view nothing seen holds a dependency, so an out-of-view one is clearable.
func blockersOf(deps []gatedDep, workToward map[string][]annotation.AnnotatedIssue) []blockerReach {
	blockers := make([]blockerReach, len(deps))
	for i, dep := range deps {
		waitsElsewhere := slices.ContainsFunc(workToward[dep.ID], func(work annotation.AnnotatedIssue) bool {
			readiness := ClassifyReadiness(work.Annotations)
			return readiness.AwaitsOutside() || work.State() == model.StateOpen && len(readiness.Prerequisites()) > 0
		})
		blockers[i] = blockerReach{depReach: dep.depReach, Clearable: !waitsElsewhere}
	}
	return blockers
}

// readingsOf splits the blockers by whether the run could read them: the ones
// with work in view as the rows they are, and the ids of the ones without, each
// in walk order.
func readingsOf(blockers []blockerReach) (inView []rowReach, outOfView []string) {
	for _, blocker := range blockers {
		row, ok := blocker.read()
		if !ok {
			outOfView = append(outOfView, blocker.ID)
			continue
		}
		inView = append(inView, row)
	}
	return inView, outOfView
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
// rows whose lane inScope admits, at any depth, each already carrying what this
// checkout may do about it. Both consumers are this walk plus one question:
// onPathDependency offers the first takeable one, and Exhausted reports them
// all so the diagnostic can say which is which. They differ in the scope they
// pass and in what they do with the answer — never in how it is found, and
// neither re-derives it. [LAW:one-source-of-truth]
//
// A dependency is classified by the work toward it, not by its own row, which
// an epic never has: it takes the best kind among that work, carried by the
// first row in rank order to reach it, so an epic whose one ticket another
// checkout holds reads as held, not as outside the view. With no work in view
// it has no reading at all (gatedDep.Best).
//
// That work is on the path too, so its own prerequisites are walked the same
// way: a blocker that cannot be started is cleared by whatever clears its own
// prerequisites — its dependencies, and the earlier ticket of its lane, which
// readiness holds it behind just the same — and a walk that stopped one hop
// down would report as stuck a path with a ready ticket further along it. Work
// already in flight is past its prerequisites (capacityFor), so the walk does
// not descend it: it is takeable or held, never cleared from below. The rows in
// scope are taken in
// rank order and each one's chain is walked to its end before the next row's
// begins, so the first takeable dependency is the one gating our highest-ranked
// row, however far down its chain it sits: rank is the one ordering routing
// applies, and a nearer dependency of a lower-ranked row does not outrank it.
// Within one row's chain the walk is breadth-first, so its nearest takeable
// dependency comes first. Work toward a takeable dependency is walked too, with
// no branch to stop it: a leaf that is takeable has no open prerequisites, and
// whatever the walk finds under a takeable epic is collected after the epic,
// so onPathDependency takes the epic first, and exhaustion, which only a walk
// with nothing takeable reaches, never sees either. `seen` ends the walk on a
// dependency cycle. [LAW:dataflow-not-control-flow] [LAW:one-source-of-truth]
//
// Only dependencies from outside the scope are collected. One whose work sits
// inside it is already covered by the scope's own walk: the pick before each
// call served it if it could be served, and when it is itself blocked it is an
// open row in scope, so this walk collects what holds it back instead. Naming
// it would offer a sibling's block to be cleared by a ticket filed beside it,
// an edge lit refuses inside one epic — and a sibling edge can still exist,
// written by an import or before that rule. Where the scope can be read is
// the prerequisite's lane: an earlier lane-mate's is the lane of the row
// waiting behind it, known even when the lane-mate is not a gathered row; any
// other's is known only from work toward it in view.
func gatingDependencies(rows []annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID, inScope func(model.LaneID) bool, reachFor func(annotation.AnnotatedIssue) reachKind, workToward map[string][]annotation.AnnotatedIssue) []gatedDep {
	// onPath is a row whose prerequisites gate the scope: an in-scope row, or
	// work toward a prerequisite its chain already collected, carrying the hops
	// between the two.
	type onPath struct {
		row annotation.AnnotatedIssue
		via []hop
	}
	// placed is the lane of each earlier lane-mate a gathered row waits behind —
	// the waiter's own, known even when the lane-mate is not a gathered row.
	// Read off every row before the walk starts, so whether such a lane-mate
	// is inside the scope does not depend on which chain reaches it first.
	placed := map[string]model.LaneID{}
	for _, row := range rows {
		for _, prerequisite := range ClassifyReadiness(row.Annotations).Prerequisites() {
			if prerequisite.inWaitersLane() {
				placed[prerequisite.Detail] = laneOf(row)
			}
		}
	}
	seen := map[string]bool{}
	var deps []gatedDep
	for _, gated := range rows {
		if !inScope(laneOf(gated)) {
			continue
		}
		chain := []onPath{{row: gated}}
		for i := 0; i < len(chain); i++ {
			at := chain[i]
			if at.row.State() != model.StateOpen {
				continue
			}
			for _, prerequisite := range ClassifyReadiness(at.row.Annotations).Prerequisites() {
				id := prerequisite.Detail
				if seen[id] {
					continue
				}
				seen[id] = true
				// gated is the in-scope row the chain started from — the fact
				// step 1b needs.
				// `seen` keeps the FIRST row to reach a dependency, so when one
				// dependency gates several of ours, Gates names the one the queue
				// ranks first. That is the intent, not a leftover: the pick owes
				// the reader a reason, not an inventory, and the actionable reason
				// is the highest-ranked thing it unblocks. Naming every gated row
				// would put an unbounded list in a one-line announcement.
				// [LAW:polishing-by-subtraction]
				dep := gatedDep{depReach: depReach{ID: id}, Gates: gated.ID, Via: at.via}
				for _, work := range workToward[id] {
					if kind := reachFor(work); dep.Best == nil || kind > dep.Best.Kind {
						dep.Best = &reading{Row: work, Kind: kind}
					}
				}
				lane, isPlaced := placed[id]
				inside := isPlaced && inScope(lane) || dep.Best != nil && inScope(laneOf(dep.Best.Row))
				if inside {
					continue
				}
				deps = append(deps, dep)
				for _, work := range workToward[id] {
					chain = append(chain, onPath{row: work, via: append(slices.Clone(at.via), hop{Prerequisite: id, Through: work.ID})})
				}
			}
		}
	}
	return deps
}

// onPathDependency finds, among the dependencies one gatingDependencies walk
// found, the first work this run may itself take toward closing one — "a
// dependency outside the claimed lane that gates it, at any depth, is on the
// path" (design-docs/work-claims.md, Routing step 1). Step 1b asks it of our own
// lanes and step 2b of our whole epic. A gate inside the scope never reaches
// here: the walk leaves it out, since step 1 or 2 already served it.
//
// The pick is the first work toward the dependency, in rank order, that is
// takeable and that serves admits: the dependency itself when it is a leaf, a
// ticket under it when it is an epic, which cannot be started. With every row
// admitted that is the row gatingDependencies classified the dependency by; a
// filter can leave that row out while admitting a later ticket under the same
// epic, and that ticket is still work toward the dependency.
//
// Takeability is the shared verdict, not a local readiness test. A local test
// sees no standings, so it would offer a ticket sitting in a lane another
// checkout holds fresh — which `lit start` then refuses, so `next` would
// recommend what `start` blocks. It does not re-check that the gated row is
// unservable: the steps before each call already took every servable row the
// walk's scope admits, so by the time we are here each of them is routeAround
// by construction.
func onPathDependency(deps []gatedDep, serves func(annotation.AnnotatedIssue) bool, reachFor func(annotation.AnnotatedIssue) reachKind, workToward map[string][]annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID) (ServedFromDependency, bool) {
	for _, dep := range deps {
		for _, work := range workToward[dep.ID] {
			if serves(work) && reachFor(work) == reachTakeable {
				return ServedFromDependency{Row: work, Lane: laneOf(work), Gates: dep.Gates, Via: dep.Via, Blocker: dep.ID}, true
			}
		}
	}
	return ServedFromDependency{}, false
}

// Error names the ticket the filter hid by the run that hands it over. The
// four flags are named rather than "the filter" because routing never learns
// which of them the run was given, and every one of them narrows the same way.
func (o Narrowed) Error() string {
	return fmt.Sprintf("`lit next` without --type, --status, --labels or --assignee hands you %s%s, which this run's filter leaves out", o.Hidden.ID, o.Scope.pickClause())
}

// Error renders Exhausted as the loud diagnostic it is when the pool offered
// nothing outside the scope either, so staying is the only route it can name —
// and only against a block, since that is all a new ticket could clear.
func (o Exhausted) Error() string {
	return strings.Join(append([]string{o.why() + "; " + o.outside()}, o.stay()...), " — ")
}

// why is what both exhaustion outcomes say first: which scope stopped, how far
// it had come, and what stops it — the dependencies from outside it, in view
// and then out of it, then its own rows held by themselves. With none of them,
// nothing stops the scope, and it says what the scope's progress shows instead.
func (o Exhausted) why() string {
	inView, outOfView := readingsOf(o.Blocked)
	stops := joinClauses(
		describeReach(inView, "blocked by ", exhaustedNotes),
		clause("blocked by ", outOfView, outOfViewNote),
		describeReach(o.Held, "held here: ", exhaustedNotes),
	)
	if stops == "" {
		stops = unstopped(o.Progress)
	}
	return fmt.Sprintf("no ready work in %s (%s) — %s", o.scope(), formatProgress(o.Progress), stops)
}

// unstopped is what a scope nothing stops has left: nothing at all, once no
// ticket in it is open — the commonest exhaustion, a checkout that has just
// finished its epic's last ticket, and also a held lane whose tickets were all
// archived or deleted, which is why it never says they were done — or only
// work already underway.
func unstopped(progress claims.Progress) string {
	if progress.Finished() {
		return "nothing in it is left to do"
	}
	return "nothing else is queued behind what's already in progress"
}

// outside is what the pool held beyond the scope when it offered nothing: a
// ready row the filter left out, no ready row at all, or rows a focus label
// kept it from asking about, named so the agent can widen the question rather
// than read their absence as none. [LAW:no-silent-failure]
func (o Exhausted) outside() string {
	if o.Narrowing != nil {
		return o.Narrowing.Error()
	}
	if len(o.OffPath) == 0 {
		return "`lit next` has nothing ready outside it either"
	}
	return "nothing on the focus path outside it is ready either: " + clause("", o.OffPath, offFocusPathNote)
}

// stay is the route that keeps the checkout in its scope, as a list of at most
// one so both renderers join it beside the others unconditionally. It exists
// only against a block a ticket filed here could clear (blockersOf): with
// Blocked empty the rest of the scope is work already underway. Filing alone
// clears nothing — the blocked work still waits on its blocker — so the route
// names the edge too, and names it exactly: the blocker waits on the new
// ticket, spelled as one command per blocker it could free, so the agent
// copies an id rather than picking one of the ids after "blocked by", some of
// which no new ticket frees. Every blocker is one outside the scope; lit
// refuses a blocked-by edge between two tickets of one epic.
func (o Exhausted) stay() []string {
	var edges []string
	for _, blocker := range o.Blocked {
		if blocker.Clearable {
			edges = append(edges, fmt.Sprintf("`lit dep add %s --blocked-by <new>`", blocker.ID))
		}
	}
	if len(edges) == 0 {
		return nil
	}
	return []string{"to stay, file the ticket that clears a blocker " + o.home() + ", then make that blocker blocked by it with " + nameCapped(edges, " or ")}
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
// this whole type exists to end. The array cannot drop them — a reported kind
// with no words still renders its ids, under an empty parenthetical, which is
// loud rather than silent. Its length is reachTakeable, so it has a slot for
// every kind a walk can report and none for a row routing would have served
// instead: words for that are an out-of-range index, which does not compile. No
// reported list holds a takeable row, and one that did would not pass quietly:
// describeReach's grouping and the event's name lookup (reachKindNames) are
// bounded the same way and panic on it. Go does not
// require an indexed array literal to fill every slot, so
// TestEveryReachKindHasWordsInBothDiagnostics holds the other direction.
// [LAW:types-are-the-program] [LAW:no-silent-failure]
type reachNotes [reachTakeable]string

// The wording each diagnostic carries, named so the totality test can reach
// them and so neither is rebuilt on every render. Both walks report every kind
// a reachNotes has a slot for.
//
// Neither reports reachTakeable. NoWork is answered only to a checkout holding
// no lane, so no pool row is laneOurs and capacityFor cannot answer resumeWork,
// while the pick just declined every serveWork over that same set — leaving
// routeAround as the only verdict passedOver can see. And exhaustion reports
// the very walk step 2b just declined, so it never holds a takeable one.
var (
	exhaustedNotes = reachNotes{
		reachHeldFresh:       "on your path but claimed by another checkout right now",
		reachHeldByItself:    "on your path but held by something about the ticket itself — `lit show` it names what",
		reachNotReady:        "on your path but not startable right now — `lit show` it",
		reachAwaitingOutside: "on your path but waiting on an event outside this repository — `lit show` it names the event",
	}
	poolNotes = reachNotes{
		reachHeldFresh:       "in progress or claimed in a lane another checkout holds right now",
		reachHeldByItself:    "held by something about the ticket itself — `lit show` it names what",
		reachNotReady:        "not startable right now — `lit show` it names what blocks it",
		reachAwaitingOutside: "waiting on an event outside this repository — `lit show` it names the event",
	}
)

// The words for the two lists only one outcome carries each, which are not
// kinds: a dependency with no work in view is Exhausted's alone, and a row the
// focus withheld is said of OffPath on both Exhausted and NoWork, in the pool's
// words, because those are rows the pool withheld.
const (
	outOfViewNote    = "on your path but outside this view — `lit show` it"
	offFocusPathNote = "off the focus path this run answered over — `lit next --all` to route over the whole queue"
)

// maxNamedPerKind caps how many ids one clause names outright. NoWork's rows
// are every row the pool walk went past — the whole filtered backlog, not the
// epic-scoped handful Exhausted reports — so an uncapped join answers a busy
// workspace with a single line of hundreds of ids, which is worse for the agent
// reading it than a dozen and a count. Naming some is what makes the diagnostic
// actionable; naming all of them is what makes it unreadable.
const maxNamedPerKind = 12

// nameCapped names at most maxNamedPerKind items, joined by sep, and states how
// many it left out, so a clause reads the same at any pool size. The route to stay
// spells its commands through it too, so no clause grows a second answer to how
// long it may get. The remainder is counted rather
// than dropped: an agent told "and 135 more" still learns the true scale of what
// it cannot start. [LAW:no-silent-failure]
func nameCapped(items []string, sep string) string {
	// [LAW:dataflow-not-control-flow] the last inch of rendering, where the two
	// arms are different sentences rather than an operation one of them skips.
	if len(items) <= maxNamedPerKind {
		return strings.Join(items, sep)
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:maxNamedPerKind], sep), len(items)-maxNamedPerKind)
}

// describeReach renders a clause for each kind that has rows, most this
// checkout can do first — one ordering for both diagnostics rather than a
// per-caller one that could disagree. [LAW:one-source-of-truth]
//
// The rows are grouped into an array bounded like notes, so a takeable row
// panics here, at the render, rather than being grouped under a kind no clause
// is written for and dropped from the message. [LAW:no-silent-failure]
func describeReach(rows []rowReach, lead string, notes reachNotes) string {
	var byKind [reachTakeable][]string
	for _, row := range rows {
		byKind[row.Kind] = append(byKind[row.Kind], row.name())
	}
	parts := make([]string, 0, len(notes))
	for kind := len(notes) - 1; kind >= 0; kind-- {
		parts = append(parts, clause(lead, byKind[kind], notes[kind]))
	}
	return joinClauses(parts...)
}

// clause renders "<lead><ids> (<note>)", and nothing when there are no ids.
// Every list a diagnostic names is rendered through it, so the cap lives here:
// no clause can grow a second answer to how long it may get.
// [LAW:single-enforcer]
func clause(lead string, ids []string, note string) string {
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf("%s%s (%s)", lead, nameCapped(ids, ", "), note)
}

// joinClauses joins the clauses that say something with "; ".
func joinClauses(clauses ...string) string {
	return strings.Join(slices.DeleteFunc(clauses, func(c string) bool { return c == "" }), "; ")
}

// Error names which emptiness this is.
//
// Nothing walked past: the gather came back empty and the backlog really is.
// Rows walked past: they are named and classified, because the fact is then the
// opposite one — the pool was full, of work this checkout may not have. The
// clause leads by refuting the empty reading outright, since that reading tells
// an agent asking, after a crash, what it was already on that its work is gone.
func (o NoWork) Error() string {
	rows := joinClauses(describeReach(o.Unreachable, "", poolNotes), clause("", o.OffPath, offFocusPathNote))
	if rows == "" {
		return "no ready work"
	}
	// A third emptiness, and it needs its own lead: with rows withheld by the
	// focus scope, "nothing in it is startable here" is false — those rows were
	// never asked. The lead claims neither startability nor location, because
	// the message names two populations here: on-path rows step 4 walked and
	// rejected, and off-path rows it never examined. A header asserting either
	// fact over the whole list is false for one of them — and the on-path half
	// is the news the agent has to act on. Each clause's own note tells them
	// apart.
	if len(o.OffPath) > 0 {
		return fmt.Sprintf("no ready work on the focus path — the backlog is not empty, and each row below says why this run did not serve it: %s", rows)
	}
	return fmt.Sprintf("no ready work — the backlog is not empty, but nothing in it is startable here: %s", rows)
}
