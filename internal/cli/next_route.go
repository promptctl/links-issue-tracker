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
// than startable. Nothing is claimed and nothing is begun, so it announces
// "resuming" and not "starting". Being reachable at all is this ticket's
// headline: while routing gated servability on model.StateOpen, an in_progress
// row was servable to nobody, which hid every orphan (links-claims-1b0p, G2)
// and — with no staleness involved anywhere — the very ticket the checkout was
// working at that moment (N8).
type ResumedOwnWork struct{ Row annotation.AnnotatedIssue }

// ServedFromEpicLane is a pick from a different lane of the same epic this
// checkout already holds a lane in — the GRANULARITY RULING's new step 2,
// epic-major before global. Starting it would establish a fresh claim on Lane,
// which is why it carries the lane to name, and it admits a takeover exactly as
// ServedFromNewLane does.
//
// The epic is Lane.Epic() and is not carried beside it: two fields for one fact
// are two clocks. [LAW:one-source-of-truth]
type ServedFromEpicLane struct {
	Row  annotation.AnnotatedIssue
	Lane model.LaneID
}

// ServedFromNewLane is a ready ticket in a lane this checkout does NOT hold, so
// starting it would establish a claim — which is what Lane is carried to name.
// Two steps produce it: the global pool (step 4) and the on-path dependency
// gating one of our own blocked rows (step 1b). The dependency used to come back
// as ServedFromClaim, whose contract is that no claim is established and nothing
// is announced, which left the one pick an agent is least likely to predict as
// the only silent one (links-claims-1b0p, N3).
//
// A takeover arrives here too. Work abandoned in flight says so — startAdvice
// reads the row's state; a ready ticket in a stale lane reads exactly like a
// fresh start, its provenance carried by the claim line printNextSummary prints
// beneath the row.
//
// Lane is the LaneID and not its String(): the rendering belongs to whoever
// knows the reader, and stringifying here threw away the discriminator the
// renderer needs to tell a lane worth naming from one that would only repeat the
// ticket (links-next-output-5aee). [LAW:types-are-the-program]
type ServedFromNewLane struct {
	Row  annotation.AnnotatedIssue
	Lane model.LaneID
}

// Exhausted is the checkout's own claimed epic(s) having open work with none
// of it reachable: not the held lanes, not an on-path dependency, not another
// lane of the same epic. Routing step 3 — loud and diagnostic, never a
// silent hop to a leaf outside the epic. Blocked names the open dependencies
// gating that work, if any (an in-progress-only lane names none).
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
}

// reachKind is what one row is to this checkout right now — the one fact both
// terminal diagnostics need, and the limit of what either may say. A bool here
// read "takeable or not", so a row outside this run's filtered view, or one not
// startable itself, rendered as the one reason the message named: claimed by
// another checkout. The renderer picks a note per value rather than asserting a
// cause it cannot see.
//
// Exhaustion asks it of the dependencies gating our scope; an empty global pool
// asks it of every row the walk went past. Same question, same four answers, so
// one type answers it — a second enum beside this one, saying the same things
// about a different set of rows, is two clocks.
// [LAW:types-are-the-program] [LAW:one-type-per-behavior] [LAW:no-silent-failure]
type reachKind int

const (
	reachTakeable reachKind = iota
	// reachHeldFresh: another checkout holds its lane right now.
	reachHeldFresh
	// reachNotReady: gathered and not held elsewhere, but not startable —
	// blocked by a further dependency, or in flight and not abandoned. One
	// value for both, because the note says only what both share.
	reachNotReady
	// reachOutOfView: absent from the gathered rows, so this run knows
	// nothing about it. --type/--labels/--assignee and leaf-only membership
	// narrow the gather; the dependency annotation is read from the store and
	// does not. Only the exhaustion walk can reach it — that one reads
	// dependency ids off annotations, while the pool walk classifies rows it is
	// already holding.
	reachOutOfView
	// reachKindCount bounds reachNotes and is never a classification: reachOf
	// returns one of the four above.
	reachKindCount
)

// rowReach is a row a walk went past, carrying what this checkout may do about
// it. Row is the gathered issue for every kind but reachOutOfView.
type rowReach struct {
	ID   string
	Row  annotation.AnnotatedIssue
	Kind reachKind
}

// reachOf classifies one row, consuming capacityFor rather than re-deriving
// takeability so routing and the diagnostics read one authority.
// [LAW:one-source-of-truth]
//
// Total by construction: relationOf covers four lane relations, and the
// fallthrough takes every routeAround reached for a reason other than a
// foreign hold. A row that is both held fresh and not ready reports as
// held — ownership decides whether this checkout may act at all, readiness
// only whether acting would get anywhere.
func reachOf(row annotation.AnnotatedIssue, gathered bool, standing claims.Standing, self model.Attribution) reachKind {
	switch {
	case !gathered:
		return reachOutOfView
	case capacityFor(row, standing, self) != routeAround:
		return reachTakeable
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
// It carried nothing at all before, and "no ready work" is an answer-shaped
// void the moment it does: one sentence for "the backlog is empty" and for "the
// backlog is full of work you may not have", two facts a caller can never pull
// back apart. An agent narrowing `next` to --status in_progress is asking what
// it was already on — the question asked after a crash — and got the empty-queue
// wording back, which tells it its work is gone (links-cli-q7hg). The walk knew
// every row and every verdict at the moment it discarded them; keeping them is
// what lets the message name which emptiness this is, instead of the remediation
// listing all three and hoping. [LAW:parse-dont-validate]
// [LAW:types-are-the-program]
type NoWork struct{ Unreachable []rowReach }

func (ServedFromClaim) isNextOutcome()    {}
func (ResumedOwnWork) isNextOutcome()     {}
func (ServedFromEpicLane) isNextOutcome() {}
func (ServedFromNewLane) isNextOutcome()  {}
func (Exhausted) isNextOutcome()          {}
func (NoWork) isNextOutcome()             {}

// capacity is the single answer to "may this checkout take this row, and as
// what?" — the eligibility verdict the owner ruling on links-claims-1b0p
// demands. It replaces three independent booleans (heldBySelf, isReadyRow,
// isUnclaimed) consulted at three points of one walk, each re-deriving a piece
// of this question and disagreeing about what a stale claim means. With the
// answer in one place a fourth capacity costs one arm of one switch instead of
// an edit to three predicates and the loop.
// [LAW:types-are-the-program] [LAW:single-enforcer]
type capacity int

const (
	// routeAround: not this checkout's to take right now.
	routeAround capacity = iota
	// serveWork: startable work this checkout may claim or already holds.
	serveWork
	// resumeWork: work already underway that belongs to this checkout. Handed
	// back rather than started fresh — staleness of your own lane is evidence
	// you stepped away, never evidence the work stopped being yours.
	resumeWork
	// takeoverWork: something is being displaced — a stale foreign claim, or
	// in-flight work abandoned in a lane nobody holds. Announced, never silent.
	takeoverWork
)

// capacityFor derives the verdict from the only four facts that bear on it:
// the row's lifecycle state, its lane's relation to this checkout, whether
// anything blocks it, and whether it is orphaned. Pure, total, and the one
// consumer of IsOrphaned() in
// routing — the fact was computed on every gather and discarded here before
// (links-claims-1b0p, F1).
//
// Two rules cover the whole table. Our own lane: work in flight is ours to
// resume — staleness there is evidence we stepped away, never that the work
// stopped being ours — and startable work is ours to serve. Any other lane: we
// may take what is takeable, and it counts as a takeover exactly when
// something is being displaced, whether that is a stale holder or an in-flight
// ticket somebody walked away from.
//
// Takeability is where the state asymmetry lives. An OPEN row is takeable when
// nothing blocks it. An IN-PROGRESS row is somebody's work in flight and stays
// untouchable — whosever lane it sits in — until it is orphaned, the orphan
// annotation being the proof that the claim asserting somebody is working it
// is self-refuting.
func capacityFor(row annotation.AnnotatedIssue, standing claims.Standing, self model.Attribution) capacity {
	readiness := ClassifyReadiness(row.Annotations)
	relation := relationOf(standing, self)
	started := row.State() == model.StateInProgress
	if relation == laneOurs {
		if started {
			return resumeWork
		}
		if readiness.IsReady() {
			return serveWork
		}
		return routeAround
	}
	takeable := (started && readiness.IsOrphaned()) || (!started && readiness.IsReady())
	switch {
	case !takeable, relation == laneHeldForeign:
		return routeAround
	case started, relation == laneStaleForeign:
		return takeoverWork
	}
	return serveWork
}

// ownScope reads this checkout's held lanes, and the epics they sit in,
// straight from the standings.
//
// [LAW:one-source-of-truth] It reads standings and NOT the gathered rows. The
// rows are already narrowed by --type/--labels/--assignee, so deriving
// ownership from them let any display filter empty this set and drop the whole
// self-aware branch — a checkout with a perfectly fresh claim silently hopping
// epics because it asked for one issue type (links-claims-1b0p, N1). Ownership
// is a fact about the workspace; a display filter must not be able to change it.
//
// An unidentified self needs no guard here: relationOf owns what a
// public-checkout self may match, so ownership and takeover cannot drift apart
// on it. [LAW:single-enforcer]
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
// amendment); then a loud exhaustion diagnostic if the epic has open work but
// none of it is reachable; then the global pool. A checkout with no lanes of its
// own starts straight at the global pool — unfocus is the zero state, not a hop
// through the earlier steps.
//
// [LAW:dataflow-not-control-flow] Every step walks the same rows in the same
// composite-rank order and asks capacityFor the same question; a step differs
// only in which lanes it admits and which verdicts it accepts. No step decides
// eligibility on its own.
func routeNext(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, standings claims.Standings, self model.Attribution) NextOutcome {
	laneOf := func(row annotation.AnnotatedIssue) model.LaneID {
		return model.LaneOf(row.Issue, details[row.ID].Parent)
	}
	verdict := func(row annotation.AnnotatedIssue) capacity {
		return capacityFor(row, standings.Of(laneOf(row)), self)
	}
	reachFor := func(row annotation.AnnotatedIssue, gathered bool) reachKind {
		return reachOf(row, gathered, standings.Of(laneOf(row)), self)
	}
	// pick keeps the first row, in rank order, that sits in an admitted lane
	// and carries one of the accepted verdicts.
	//
	// accept is a SET and never a preference order: composite rank is the only
	// tiebreak routing gets to apply, and ranking capacities against each other
	// would quietly reintroduce this ticket's headline symptom — the backlog's
	// #1 row, an orphan, passed over for a lower-ranked leaf that happened to
	// need no takeover. [LAW:one-source-of-truth] one ordering, and the gather
	// already established it.
	pick := func(inScope func(model.LaneID) bool, accept ...capacity) (annotation.AnnotatedIssue, capacity, bool) {
		for _, row := range rows {
			if how := verdict(row); inScope(laneOf(row)) && slices.Contains(accept, how) {
				return row, how, true
			}
		}
		return annotation.AnnotatedIssue{}, routeAround, false
	}

	ownLanes, ownEpics := ownScope(standings, self)
	mine := func(lane model.LaneID) bool { return ownLanes[lane] }
	if len(ownLanes) > 0 {
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
		// one (N3) and its own lane's standing is honoured rather than
		// ignored (N2).
		if dep, ok := onPathDependency(rows, laneOf, mine, reachFor); ok {
			return ServedFromNewLane{Row: dep, Lane: laneOf(dep)}
		}
		// Step 2 — the rest of our epic, in lanes we do not already hold.
		ourEpic := func(lane model.LaneID) bool {
			return lane.Epic() != "" && ownEpics[lane.Epic()]
		}
		if row, _, ok := pick(func(lane model.LaneID) bool { return ourEpic(lane) && !mine(lane) }, serveWork, takeoverWork); ok {
			return ServedFromEpicLane{Row: row, Lane: laneOf(row)}
		}
		// Step 3 — loud, and never a hop.
		return Exhausted{
			Epics: slices.Sorted(maps.Keys(ownEpics)),
			Blocked: gatingDependencies(rows, laneOf, func(lane model.LaneID) bool {
				return mine(lane) || ourEpic(lane)
			}, reachFor),
		}
	}

	// Step 4 — the global pool, and the diagnostic half of the same walk: what
	// the pick declined is what NoWork reports, classified by the verdict the
	// pick itself just read. [LAW:one-source-of-truth]
	if row, _, ok := pick(func(model.LaneID) bool { return true }, serveWork, takeoverWork); ok {
		return ServedFromNewLane{Row: row, Lane: laneOf(row)}
	}
	return NoWork{Unreachable: passedOver(rows, reachFor)}
}

// passedOver classifies every gathered row, in the rank order the pool walk
// went through them. It is called only where that walk found nothing, so every
// row here is routeAround by construction — a takeable one would have been
// served — which is what makes "the rows we went past" and "all the rows" the
// same list, and lets NoWork say why the pool was empty without asking the data
// a second question.
func passedOver(rows []annotation.AnnotatedIssue, reachFor func(annotation.AnnotatedIssue, bool) reachKind) []rowReach {
	passed := make([]rowReach, 0, len(rows))
	for _, row := range rows {
		passed = append(passed, rowReach{ID: row.ID, Row: row, Kind: reachFor(row, true)})
	}
	return passed
}

// gatingDependencies collects the distinct open dependencies that gate the open
// rows whose lane inScope admits, in rank order, each already carrying whether
// this checkout may take it. Both consumers are this walk plus one question:
// onPathDependency offers the first takeable one, and Exhausted reports them
// all so the diagnostic can say which is which. They differ in the scope they
// pass and in what they do with the answer — never in how it is found, and
// neither re-derives it. [LAW:one-source-of-truth]
func gatingDependencies(rows []annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID, inScope func(model.LaneID) bool, reachFor func(annotation.AnnotatedIssue, bool) reachKind) []rowReach {
	byID := make(map[string]annotation.AnnotatedIssue, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	seen := map[string]bool{}
	var deps []rowReach
	for _, row := range rows {
		if !inScope(laneOf(row)) || row.State() != model.StateOpen {
			continue
		}
		for _, id := range ClassifyReadiness(row.Annotations).DependencyIDs() {
			if seen[id] {
				continue
			}
			seen[id] = true
			dep, gathered := byID[id]
			deps = append(deps, rowReach{ID: id, Row: dep, Kind: reachFor(dep, gathered)})
		}
	}
	return deps
}

// onPathDependency finds the first dependency gating one of our own lanes that
// this checkout may itself take — "a dependency outside the claimed lane that
// gates it is offered as on-path" (design-docs/work-claims.md, Routing step 1).
// A same-lane gate (an earlier sibling) never reaches here: it shares the
// blocked row's lane, so step 1 already served or resumed it.
//
// Takeability is the shared verdict, not a local readiness test. Before, this
// function saw no standings at all and would happily offer a ticket sitting in
// a lane another checkout holds fresh — which `lit start` then refused, so
// `next` recommended what `start` blocked (links-claims-1b0p, N2). It also no
// longer re-checks that the gated row is unservable: step 1 accepts every
// capacity an own lane can produce, so by the time we are here every row in
// `mine` is routeAround by construction.
func onPathDependency(rows []annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID, mine func(model.LaneID) bool, reachFor func(annotation.AnnotatedIssue, bool) reachKind) (annotation.AnnotatedIssue, bool) {
	for _, dep := range gatingDependencies(rows, laneOf, mine, reachFor) {
		if dep.Kind == reachTakeable {
			return dep.Row, true
		}
	}
	return annotation.AnnotatedIssue{}, false
}

// Error renders Exhausted as the loud diagnostic the design demands in place
// of a silent hop to another epic's leaf.
func (o Exhausted) Error() string {
	scope := "your claimed lane(s)"
	if len(o.Epics) > 0 {
		scope = fmt.Sprintf("epic(s) %s", strings.Join(o.Epics, ", "))
	}
	if len(o.Blocked) == 0 {
		return fmt.Sprintf("no ready work in %s — nothing else is queued behind what's already in progress; picking up other work is a deliberate re-focus, not a bare `next`", scope)
	}
	return fmt.Sprintf("no ready work in %s — %s; picking up other work is a deliberate re-focus, not a bare `next`", scope, describeReach(o.Blocked, "blocked on ", exhaustedNotes))
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
// TestEveryReachKindHasWordsInBothDiagnostics closes that gap.
// [LAW:types-are-the-program] [LAW:no-silent-failure]
type reachNotes [reachKindCount]string

// The wording each diagnostic carries, named so the totality test can reach
// them and so neither is rebuilt on every render.
var (
	exhaustedNotes = reachNotes{
		reachTakeable:  "on your path and yours to take — `lit start` it",
		reachHeldFresh: "on your path but claimed by another checkout right now",
		reachNotReady:  "on your path but not startable right now — `lit show` it",
		reachOutOfView: "on your path but outside this view — `lit show` it",
	}
	poolNotes = reachNotes{
		reachTakeable:  "startable — `lit start` it",
		reachHeldFresh: "in progress or claimed in a lane another checkout holds right now",
		reachNotReady:  "not startable — blocked by a dependency, or in flight and not abandoned",
		reachOutOfView: "outside this view — `lit show` it",
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
		byKind[row.Kind] = append(byKind[row.Kind], row.ID)
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

// Error names which emptiness this is, which the line could not do while the
// type held nothing to tell them apart with.
//
// Nothing walked past: the gather came back empty and the backlog really is, so
// this is the sentence `next` has always printed, unchanged. Rows walked past:
// they are named and classified, because the fact is then the opposite one — the
// pool was full, of work this checkout may not have — and it used to arrive in
// identical words. The clause leads by refuting the empty reading outright,
// since that reading is what the bare sentence cost an agent asking, after a
// crash, what it was already on (links-cli-q7hg).
func (o NoWork) Error() string {
	if len(o.Unreachable) == 0 {
		return "no ready work"
	}
	return fmt.Sprintf("no ready work — the backlog is not empty, but nothing in it is startable here: %s", describeReach(o.Unreachable, "", poolNotes))
}
