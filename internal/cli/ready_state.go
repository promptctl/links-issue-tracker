package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// NeedsDesignLabel is the reserved label that flags an issue as awaiting
// design work. The annotator below converts the label (a neutral fact on the
// issue) into a NeedsDesign annotation; ClassifyReadiness is where the
// consumer decides that this annotation blocks readiness.
// [LAW:one-source-of-truth] Single definition of the needs-design label.
const NeedsDesignLabel = "needs-design"

// newNeedsDesignAnnotator returns an annotator that emits a NeedsDesign
// annotation for any issue carrying NeedsDesignLabel.
// [LAW:dataflow-not-control-flow] The annotator runs unconditionally for
// every issue; absence of the label produces a nil slice (Annotate
// normalizes to an empty slice at the row level), not a skipped operation.
func newNeedsDesignAnnotator() annotation.Annotator {
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		for _, label := range issue.Labels {
			if label == NeedsDesignLabel {
				return []annotation.Annotation{{
					Kind:    annotation.NeedsDesign,
					Message: NeedsDesignLabel,
				}}, nil
			}
		}
		return nil, nil
	}
}

// orphanedThreshold is the staleness window after which an in_progress
// issue is flagged as orphaned. Both `lit backlog`'s in-progress rows
// and `lit orphaned` read from this single value so the two surfaces
// cannot drift.
// [LAW:one-source-of-truth] Single threshold for orphan detection.
const orphanedThreshold = 6 * time.Hour

// newFieldAnnotator validates requiredFields against model.Issue JSON fields,
// then returns an annotator that checks those fields on each issue.
func newFieldAnnotator(requiredFields []string) (annotation.Annotator, error) {
	// [LAW:effects-at-boundaries] With no required fields there is nothing to
	// check, so skip the per-issue JSON marshal (issueFieldValues) entirely and
	// return a no-op annotator. Output is identical — the general closure below
	// returns nil for an empty policy anyway — so this is a pure efficiency
	// short-circuit for the common case (no required_fields configured, and every
	// store the cross-project rollup opens with a nil policy), and it drops the
	// speculative marshal error path that could turn a clean read into an error.
	if len(requiredFields) == 0 {
		return func(context.Context, model.Issue) ([]annotation.Annotation, error) {
			return nil, nil
		}, nil
	}
	validFields := issueJSONFieldNames()
	for _, field := range requiredFields {
		if _, ok := validFields[field]; !ok {
			return nil, ValidationError{Message: fmt.Sprintf("required field %q does not exist on issue", field)}
		}
	}
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		fields, err := issueFieldValues(issue)
		if err != nil {
			return nil, err
		}
		var annotations []annotation.Annotation
		for _, field := range requiredFields {
			if !isRequiredFieldSet(fields[field]) {
				annotations = append(annotations, annotation.Annotation{
					Kind:    annotation.MissingField,
					Message: field,
				})
			}
		}
		return annotations, nil
	}, nil
}

// fetchIssueRelations batch-loads the structural relations for every listed
// issue in a fixed number of queries.
// [LAW:single-enforcer] One pre-pass is the single source of per-row relation
// data for the ready pipeline; both annotation and enrichment read from it.
// [LAW:one-source-of-truth] Uses the same store accessor the epic view does, so
// "an issue's open blockers / parent epic" has one definition across consumers.
// [LAW:dataflow-not-control-flow] The fetch is unconditional and happens once;
// downstream stages are pure map lookups over the result.
func fetchIssueRelations(ctx context.Context, st storage.Store, issues []model.Issue) (map[string]storage.IssueRelations, error) {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	relations, err := st.GetRelationsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	// GetRelationsByIDs omits subjects that don't exist; the ready pipeline
	// requires every workable issue to resolve, so a hole is a NotFound (matching
	// the prior per-issue GetIssueDetail path), not a silent zero-value row.
	// [LAW:no-defensive-null-guards] Fail loudly at the store boundary.
	for _, issue := range issues {
		if _, ok := relations[issue.ID]; !ok {
			return nil, storage.NotFoundError{Entity: "issue", ID: issue.ID}
		}
	}
	return relations, nil
}

// newBlockerAnnotator returns an annotator that checks open dependency blockers
// and flags rank inversions where a dependency is ranked below the dependent.
// The annotator is pure: it reads from the shared relations maps rather than
// fetching from the store, so fetch cost is paid once upstream in
// fetchIssueRelations and fetchHeldAncestry.
//
// A blocks edge onto an epic is a dependency of every issue in it or in an epic
// nested in it, in every lane, except an issue the blocker itself waits on
// (heldAncestry), so each blocker an ancestor epic holds this issue back on
// becomes an InheritedDependency here — through the same annotation mechanism,
// and so the same ClassifyReadiness enforcer, a declared edge uses. It used to
// be accepted and then ignored: `lit dep add` stored the edge, the epic's row
// showed it, and every child stayed servable (links-epic-block-xpkz). An id the
// issue already depends on directly is named once, as the direct edge. No
// inherited edge is a rank inversion of this issue: rank hygiene of the epic's
// own edge is a fact about the epic, which lit doctor's inversion pass orders.
func newBlockerAnnotator(details map[string]storage.IssueRelations, ancestry heldAncestry) annotation.Annotator {
	// [LAW:dataflow-not-control-flow] Dependency lookup runs for every issue;
	// empty blockers list means no annotations, not a skipped operation.
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		detail := details[issue.ID]
		// Collect the blocking deps and sort by ID for stable annotation
		// ordering. The annotation kind keeps its registered name
		// (open_dependency); the predicate below decides what it means.
		// [LAW:one-source-of-truth] InPlay is the one definition of
		// "unfinished", and it reads both axes. The prior spelling here —
		// State() != StateClosed — consulted the status sum alone, so a
		// soft-deleted dependency kept emitting OpenDependency while every
		// transition that could discharge it (close/open/start all refuse a
		// frozen issue) was unreachable: a dependent blocked forever by a
		// blocker no listing shows. Read and write must agree on retention.
		var blockingDeps []model.Issue
		for _, dep := range detail.DependsOn {
			if dep.InPlay() {
				blockingDeps = append(blockingDeps, dep)
			}
		}
		sort.Slice(blockingDeps, func(i, j int) bool { return blockingDeps[i].ID < blockingDeps[j].ID })
		var annotations []annotation.Annotation
		for _, dep := range blockingDeps {
			annotations = append(annotations, annotation.Annotation{
				Kind:    annotation.OpenDependency,
				Message: dep.ID,
			})
			// Rank inversion: dependency should be ranked above (lower rank) the dependent.
			if dep.Rank > issue.Rank {
				annotations = append(annotations, annotation.Annotation{
					Kind:    annotation.RankInversion,
					Message: dep.ID,
				})
			}
		}
		direct := make(map[string]bool, len(blockingDeps))
		for _, dep := range blockingDeps {
			direct[dep.ID] = true
		}
		for _, dep := range ancestry.inheritedDependencies(detail) {
			if direct[dep.ID] {
				continue
			}
			annotations = append(annotations, annotation.Annotation{
				Kind:    annotation.InheritedDependency,
				Message: dep.ID,
			})
		}
		return annotations, nil
	}
}

// relationsFetch loads relations by id: the store itself, or a walk's memo
// over it.
type relationsFetch func(context.Context, []string) (map[string]storage.IssueRelations, error)

// epicAncestry is what the readiness gates read about the epics above a set of
// issues: each epic's relations, keyed by epic id, and each epic's unfinished
// blockers. One fetchContainerAncestry call builds both, so the gates always
// describe the epics they are read beside. [LAW:one-source-of-truth]
type epicAncestry struct {
	relations map[string]storage.IssueRelations
	gates     map[string][]model.Issue
}

// fetchContainerAncestry loads the epics above the given issues and their
// unfinished blockers.
func fetchContainerAncestry(ctx context.Context, fetch relationsFetch, subjects map[string]storage.IssueRelations) (epicAncestry, error) {
	relations, err := climbContainers(ctx, fetch, subjects)
	if err != nil {
		return epicAncestry{}, err
	}
	gates := make(map[string][]model.Issue, len(relations))
	for epicID, epic := range relations {
		for _, dep := range epic.DependsOn {
			if dep.InPlay() {
				gates[epicID] = append(gates[epicID], dep)
			}
		}
	}
	return epicAncestry{relations: relations, gates: gates}, nil
}

// climbContainers loads the relations of every epic above the given issues, one
// batched query per nesting level, keyed by epic id. An issue's parent is one
// level; an epic can itself sit under an epic, so the walk climbs until no level
// names a parent epic it has not loaded. A top-level epic has no parent, so the
// common one-level workspace costs exactly the one query it cost before this
// walk existed.
func climbContainers(ctx context.Context, fetch relationsFetch, subjects map[string]storage.IssueRelations) (map[string]storage.IssueRelations, error) {
	ancestry := make(map[string]storage.IssueRelations)
	for level := subjects; ; {
		var ids []string
		for _, id := range parentEpicIDs(level) {
			if _, loaded := ancestry[id]; !loaded {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return ancestry, nil
		}
		fetched, err := fetch(ctx, ids)
		if err != nil {
			return nil, err
		}
		for id, rel := range fetched {
			ancestry[id] = rel
		}
		level = fetched
	}
}

// epicsAbove yields the ids of the epics above rel, nearest first, reading each
// parent's relations from ancestry. A parent that is not an epic ends the walk,
// as `epic: none` in lit backlog says, and so does a parent cycle (6m14).
func epicsAbove(rel storage.IssueRelations, ancestry map[string]storage.IssueRelations) iter.Seq[string] {
	return func(yield func(string) bool) {
		seen := map[string]bool{rel.Issue.ID: true}
		for parent := rel.Parent; parent != nil && parent.IsContainer() && !seen[parent.ID]; parent = ancestry[parent.ID].Parent {
			seen[parent.ID] = true
			if !yield(parent.ID) {
				return
			}
		}
	}
}

// inheritedDependencies returns the blockers of every epic above subject,
// sorted by id, each named once.
func (a epicAncestry) inheritedDependencies(subject storage.IssueRelations) []model.Issue {
	named := map[string]bool{}
	var deps []model.Issue
	for epicID := range epicsAbove(subject, a.relations) {
		for _, dep := range a.gates[epicID] {
			if !named[dep.ID] {
				named[dep.ID] = true
				deps = append(deps, dep)
			}
		}
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].ID < deps[j].ID })
	return deps
}

// heldAncestry is an epicAncestry together with what each epic's blocker
// already waits on. An epic's blocker holds back every issue under the epic
// except one it waits on itself, directly or through other issues: holding that
// one back would leave the two waiting on each other forever. That covers the
// blocker itself when it sits under the epic, its earlier lane-mates and
// dependencies there, and every issue under an epic the blocker contains. The
// exception is read from the relations as they are now, so no write can leave a
// loop behind it: not a new edge, a move, a lane or rank change, a reopen, nor
// an import (links-hierarchy-lrz6).
type heldAncestry struct {
	ancestry epicAncestry
	waitsOn  map[string]map[string]bool
}

// fetchHeldAncestry loads the epics above the given issues, their blockers, and
// what each blocker waits on. It costs no further query when no epic has an
// unfinished blocker.
// [LAW:effects-at-boundaries] The wait graph is loaded once; which links hold
// is settled over it in memory.
func fetchHeldAncestry(ctx context.Context, fetch relationsFetch, subjects map[string]storage.IssueRelations) (heldAncestry, error) {
	ancestry, err := fetchContainerAncestry(ctx, fetch, subjects)
	if err != nil {
		return heldAncestry{}, err
	}
	var gates []string
	for _, blockers := range ancestry.gates {
		for _, gate := range blockers {
			gates = append(gates, gate.ID)
		}
	}
	graph, err := fetchWaitGraph(ctx, fetch, gates)
	if err != nil {
		return heldAncestry{}, err
	}
	return heldAncestry{ancestry: ancestry, waitsOn: settleWaits(graph, gates)}, nil
}

// inheritedDependencies returns the blockers of the epics above subject that
// hold subject back, sorted by id, each named once.
func (h heldAncestry) inheritedDependencies(subject storage.IssueRelations) []model.Issue {
	return slices.DeleteFunc(h.ancestry.inheritedDependencies(subject), func(gate model.Issue) bool {
		return h.waitsOn[gate.ID][subject.Issue.ID]
	})
}

// fetchWaitGraph loads the links that can hold of every issue reachable from
// starts through them, keyed by waiter.
func fetchWaitGraph(ctx context.Context, fetch relationsFetch, starts []string) (map[string][]waitLink, error) {
	graph := make(map[string][]waitLink)
	seen := make(map[string]bool)
	var frontier []string
	enqueue := func(id string) {
		if !seen[id] {
			seen[id] = true
			frontier = append(frontier, id)
		}
	}
	for _, id := range starts {
		enqueue(id)
	}
	for len(frontier) > 0 {
		links, err := fetchWaitLinks(ctx, fetch, frontier)
		if err != nil {
			return nil, err
		}
		frontier = nil
		for _, link := range links {
			if link.holds {
				graph[link.waiter] = append(graph[link.waiter], link)
				enqueue(link.prereq)
			}
		}
	}
	return graph, nil
}

// settleWaits returns what each blocker waits on through the links of graph
// that hold, the blocker itself included when a loop leads back to it. The gates are the
// blockers of the subjects' epics; every blocker graph passes down is settled
// alongside them.
//
// A passed-down blocker holds its issue back unless the blocker waits on that
// issue, and what a blocker waits on depends in turn on which passed-down links
// hold. The answer is the alternating fixpoint, which depends on no order:
// upper is what each blocker could wait on, read from the links lower lets
// hold, and lower what it surely waits on, read from the links upper lets hold,
// until upper stops shrinking. A link upper lets hold closes no loop, since an
// enforced loop through it would put its issue in upper. Two links that could
// each hold only while the other does not, as when two epics are each blocked
// by a child of the other, both drop.
func settleWaits(graph map[string][]waitLink, gates []string) map[string]map[string]bool {
	blockers := make(map[string]bool, len(gates))
	for _, gate := range gates {
		blockers[gate] = true
	}
	for _, links := range graph {
		for _, link := range links {
			if link.inherited {
				blockers[link.prereq] = true
			}
		}
	}
	closures := func(follows func(waitLink) bool) map[string]map[string]bool {
		waitsOn := make(map[string]map[string]bool, len(blockers))
		for blocker := range blockers {
			reached := map[string]bool{}
			for frontier := []string{blocker}; len(frontier) > 0; {
				var next []string
				for _, waiter := range frontier {
					for _, link := range graph[waiter] {
						if follows(link) && !reached[link.prereq] {
							reached[link.prereq] = true
							next = append(next, link.prereq)
						}
					}
				}
				frontier = next
			}
			waitsOn[blocker] = reached
		}
		return waitsOn
	}
	heldAgainst := func(waitsOn map[string]map[string]bool) func(waitLink) bool {
		return func(link waitLink) bool {
			return !link.inherited || !waitsOn[link.prereq][link.waiter]
		}
	}
	upper := closures(func(waitLink) bool { return true })
	for {
		lower := closures(heldAgainst(upper))
		next := closures(heldAgainst(lower))
		if maps.EqualFunc(next, upper, maps.Equal) {
			return upper
		}
		upper = next
	}
}

// newSiblingGateAnnotator emits an EarlierSiblingPending annotation for a leaf
// whose parent epic contains an unfinished sibling in the same lane ranked
// before it — lifting the intra-epic "earlier sibling still open" prerequisite
// into the same blocking-annotation mechanism explicit deps use, so it gates
// MEMBERSHIP in ready/queue/next through the single ClassifyReadiness enforcer.
//
// The one rule: leaf L is blocked iff ∃ sibling S under the same epic with
// S.Lane == L.Lane, S.Rank < L.Rank, and S unfinished. Lane is a plain string;
// the empty lane is one value among many, so all-keyless children form a single
// fully-sequential lane and a per-child distinct lane is fully parallel — the
// old binary "parallel opt-out" is this mechanism's degenerate case.
// [LAW:dataflow-not-control-flow] Grouping is by lane value, never a branch on
// "has a lane". The annotator runs for every issue; model.LaneOf answers for a
// leaf with no epic parent with a solo lane whose Epic() is "", and
// siblingsByEpic has no such key, so that leaf flows through the same lookup and
// yields nil instead of taking a guarded exit. [LAW:single-enforcer] LaneOf is
// the one answer to "which lane is this issue in".
// [LAW:one-type-per-behavior] "explicit dep unfinished" and "earlier same-lane
// sibling unfinished" are the same blocking fact behind the same enforcer.
//
// siblingsByEpic holds only unfinished siblings (the index builder applies that
// predicate once), so the annotator compares lane and rank alone.
func newSiblingGateAnnotator(details map[string]storage.IssueRelations, siblingsByEpic map[string][]model.Issue) annotation.Annotator {
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		lane := model.LaneOf(issue, details[issue.ID].Parent)
		return nearestPendingLaneMate(siblingsByEpic[lane.Epic()], issue), nil
	}
}

// nearestPendingLaneMate names the ONE unfinished lane-mate standing directly
// between leaf and the front of its lane — the latest-ranked sibling still ahead
// of it — as leaf's single blocking annotation, or nothing when the lane ahead
// is clear.
//
// One witness, not all of them, because the gate is an existential: "∃ an
// earlier unfinished lane-mate" is proved by one, and the rest of the prefix is
// the lane's own rank order.
// [LAW:one-source-of-truth] The lane order is the authority on the prefix; the
// annotation carries the edge, never a second copy of the order. Carrying all of
// them made a sequential lane's blocking text grow quadratically down the epic —
// the tenth child restating the nine facts its nine predecessors had each
// already stated — which is the noise links-listing-x943 was filed for.
//
// The lane order is not always in front of the reader: the pending set is
// deliberately unfiltered (pendingSiblingsByEpic), so under a filtered or
// limited `lit backlog` the named sibling can be absent from the visible list.
// It is still the true prerequisite — a gate that consulted only the rows a
// filter let through would call a blocked leaf ready, which is the worse
// failure — so the edge is named either way and no view promises the chain.
//
// The nearest predecessor, not the earliest, because it is the edge that is
// locally true and locally actionable: close it and this leaf is next. The
// earliest would make every row in a lane carry the identical id, which is the
// same restatement in a shorter costume.
func nearestPendingLaneMate(pending []model.Issue, leaf model.Issue) []annotation.Annotation {
	var nearest *model.Issue
	for i := range pending {
		sib := &pending[i]
		if !isEarlierSameLaneSibling(*sib, leaf) {
			continue
		}
		// Rank is a lexicographic fractional index, so ">" is the same ordering
		// isEarlierSameLaneSibling reads; the id breaks a rank tie so two
		// equally-ranked lane-mates cannot make the rendered row flap.
		if nearest == nil || sib.Rank > nearest.Rank || (sib.Rank == nearest.Rank && sib.ID > nearest.ID) {
			nearest = sib
		}
	}
	if nearest == nil {
		return nil
	}
	return []annotation.Annotation{{Kind: annotation.EarlierSiblingPending, Message: nearest.ID}}
}

// isEarlierSameLaneSibling reports whether sib precedes leaf within the same
// lane — the ONE intra-epic implicit-prerequisite rule. Both the lane gate
// (newSiblingGateAnnotator) and the focus-path derivation
// (fetchFocusPathGoals) read it, so "earlier sibling" cannot drift between the
// consumer that blocks a row and the one that admits it to the focus scope.
// [LAW:single-enforcer] Single definition of the intra-epic prerequisite edge.
func isEarlierSameLaneSibling(sib, leaf model.Issue) bool {
	return sib.ID != leaf.ID && sib.Lane == leaf.Lane && sib.Rank < leaf.Rank
}

// parentEpicIDs returns the distinct ids of the container parents referenced by
// the workable leaves. These are the epics whose full child set the lane gate
// must inspect.
func parentEpicIDs(details map[string]storage.IssueRelations) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, rel := range details {
		parent := rel.Parent
		if parent == nil || !parent.IsContainer() {
			continue
		}
		if _, ok := seen[parent.ID]; ok {
			continue
		}
		seen[parent.ID] = struct{}{}
		ids = append(ids, parent.ID)
	}
	return ids
}

// pendingSiblingsByEpic indexes each epic's UNFINISHED children by epic id. The
// lane gate must see siblings hidden by the CLI's assignee/type/label filters,
// so the source is the unfiltered GetRelationsByIDs child set, not the workable
// list — an unassigned earlier sibling still gates its later lane-mates.
// [LAW:single-enforcer] "an earlier sibling still needs work" is decided over
// every sibling, not only the ones this invocation's filters let through.
func pendingSiblingsByEpic(relations map[string]storage.IssueRelations) map[string][]model.Issue {
	out := make(map[string][]model.Issue, len(relations))
	for epicID, rel := range relations {
		for _, child := range rel.Children {
			if child.InPlay() {
				out[epicID] = append(out[epicID], child)
			}
		}
	}
	return out
}

// FocusLabel is the reserved label that marks an issue as a focused goal.
// The focus fact is stored on the ONE goal ticket only; "what is on the path
// to it" is derived from the dependency DAG on every gather
// (fetchFocusPathGoals), never written onto chain members — derived state
// auto-advances as items close, with nothing to synchronize.
// [LAW:one-source-of-truth] Single definition of the focus label.
const FocusLabel = "focus"

// focusGraphSource is the exact store surface the focus-path walk consumes:
// listing the focus-labeled goals and batch-loading structural relations. The
// walk depends on this two-method seam rather than the whole storage.Store, so
// its real input is nameable and a memo/decorator can stand in.
// [LAW:decomposition] The seam carries the whole truth of what the part needs.
type focusGraphSource interface {
	ListIssues(ctx context.Context, filter storage.ListIssuesFilter) ([]model.Issue, error)
	GetRelationsByIDs(ctx context.Context, ids []string) (map[string]storage.IssueRelations, error)
}

// fetchFocusPathGoals returns issueID -> focused-goal ID for every unfinished
// issue on the prerequisite closure of a focus-labeled goal, the goal itself
// included. An issue's prerequisites are its links from fetchWaitLinks — the
// same implicit edges the membership gate blocks on — every one of them, an
// epic blocker that closes a loop included, since the path scopes a view rather
// than proving a wait.
// [LAW:one-type-per-behavior] Explicit deps and intra-epic rank order are the
// same prerequisite fact here, exactly as they are for the membership gate.
// [LAW:dataflow-not-control-flow] The walk is a pure expansion over relation
// values; no caller mode decides whether it runs — an empty focus set yields
// an empty map through the same code path.
//
// seeds donate relations the caller already fetched (the listing pipeline loads
// the workable leaves and their parent epics before this runs). They prime a
// per-invocation memo so subjects fetched once — by the caller or an earlier
// BFS level — are never re-queried. The memo is a derived cache over one
// read-only pass with no intervening writes, so a hit is byte-identical to a
// refetch; an empty seed set leaves behavior unchanged.
// [LAW:one-source-of-truth] The memo is derived, never authoritative.
func fetchFocusPathGoals(ctx context.Context, src focusGraphSource, seeds ...map[string]storage.IssueRelations) (map[string]string, error) {
	goals, err := src.ListIssues(ctx, storage.ListIssuesFilter{
		Statuses:  []model.State{model.StateOpen, model.StateInProgress},
		LabelsAll: []string{FocusLabel},
	})
	if err != nil {
		return nil, err
	}
	memo, _ := memoizeRelations(src.GetRelationsByIDs, seeds...)
	path := make(map[string]string, len(goals))
	frontier := make([]string, 0, len(goals))
	for _, goal := range goals {
		path[goal.ID] = goal.ID
		frontier = append(frontier, goal.ID)
	}
	// Breadth-first over the prerequisite DAG, one batched fetch per level.
	// The path map doubles as the visited set, so shared prerequisites are
	// attributed to the first goal that reaches them and cycles terminate.
	for len(frontier) > 0 {
		links, err := fetchWaitLinks(ctx, memo, frontier)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, link := range links {
			if _, seen := path[link.prereq]; seen {
				continue
			}
			path[link.prereq] = path[link.waiter]
			next = append(next, link.prereq)
		}
		frontier = next
	}
	return path, nil
}

// waitLink is one prerequisite edge: waiter cannot finish before prereq does.
// holds reports whether the link can keep its waiter from ever finishing. An
// epic is finished by its children alone, and what blocks an epic holds back its
// children instead, so an epic's other links scope a focus path but never hold
// the epic itself.
type waitLink struct {
	waiter, prereq string
	holds          bool
	// inherited marks a blocker an epic above the waiter passes down, which
	// heldAncestry drops when it would close a loop.
	inherited bool
}

// fetchWaitLinks returns the prerequisite links of every issue in frontier, in
// frontier order: its unfinished explicit dependencies, the unfinished blockers
// of the epics above it, the unfinished children of a container, and its earlier
// same-lane unfinished siblings. These are the edges readiness gates on, read
// through the same epicAncestry, isEarlierSameLaneSibling and
// model.Issue.InPlay, before heldAncestry drops the epic blockers that would
// close a loop. [LAW:one-source-of-truth] The focus walk and each blocker's
// wait walk both expand prerequisites here.
func fetchWaitLinks(ctx context.Context, fetch relationsFetch, frontier []string) ([]waitLink, error) {
	rels, err := fetch(ctx, frontier)
	if err != nil {
		return nil, err
	}
	ancestry, err := fetchContainerAncestry(ctx, fetch, rels)
	if err != nil {
		return nil, err
	}
	pending := pendingSiblingsByEpic(ancestry.relations)
	var links []waitLink
	for _, id := range frontier {
		rel, ok := rels[id]
		if !ok {
			// Frontier ids are hydrated issues from this same connection; a
			// hole means the store lied. [LAW:no-silent-failure]
			return nil, storage.NotFoundError{Entity: "issue", ID: id}
		}
		leaf := !rel.Issue.IsContainer()
		link := func(prereq string, holds bool) {
			links = append(links, waitLink{waiter: id, prereq: prereq, holds: holds})
		}
		for _, dep := range rel.DependsOn {
			if dep.InPlay() {
				link(dep.ID, leaf)
			}
		}
		for _, dep := range ancestry.inheritedDependencies(rel) {
			links = append(links, waitLink{waiter: id, prereq: dep.ID, holds: leaf, inherited: true})
		}
		if !leaf {
			for _, child := range rel.Children {
				if child.InPlay() {
					link(child.ID, true)
				}
			}
		}
		if rel.Parent != nil && rel.Parent.IsContainer() {
			for _, sib := range pending[rel.Parent.ID] {
				if isEarlierSameLaneSibling(sib, rel.Issue) {
					link(sib.ID, leaf)
				}
			}
		}
	}
	return links, nil
}

// memoizeRelations returns a fetch that loads each subject at most once, and
// the map it loads into, primed with seeds so a subject the caller already holds
// is never queried.
func memoizeRelations(fetch relationsFetch, seeds ...map[string]storage.IssueRelations) (relationsFetch, map[string]storage.IssueRelations) {
	cache := make(map[string]storage.IssueRelations)
	for _, seed := range seeds {
		maps.Copy(cache, seed)
	}
	return func(ctx context.Context, ids []string) (map[string]storage.IssueRelations, error) {
		return relationsByID(ctx, fetch, cache, ids)
	}, cache
}

// relationsByID returns the relations for ids, fetching only the subjects not
// already in cache and recording new fetches back into it, so a subject is
// loaded at most once per walk. Nonexistent subjects stay absent (mirroring
// GetRelationsByIDs), so callers' presence checks still fire; only positive
// results are memoized.
// [LAW:single-enforcer] Every relation load on a walk goes through this one
// memo, so cross-level repeats and caller-donated subjects never re-query.
func relationsByID(ctx context.Context, fetch relationsFetch, cache map[string]storage.IssueRelations, ids []string) (map[string]storage.IssueRelations, error) {
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := cache[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fetched, err := fetch(ctx, missing)
		if err != nil {
			return nil, err
		}
		for id, rel := range fetched {
			cache[id] = rel
		}
	}
	out := make(map[string]storage.IssueRelations, len(ids))
	for _, id := range ids {
		if rel, ok := cache[id]; ok {
			out[id] = rel
		}
	}
	return out, nil
}

// newFocusPathAnnotator returns an annotator that emits a FocusPath annotation
// for any issue on a focused goal's derived prerequisite path; the message is
// the goal's ID. FocusPath is the fact focusScope.holds reads to decide which
// rows a VIEW answers over, and it stays deliberately invisible to
// ClassifyReadiness: scoping a view and gating readiness are different
// memberships, so a blocked path item is still listed and still blocked.
// [LAW:dataflow-not-control-flow] Pure map lookup for every issue; absence
// yields nil, not a skipped operation.
func newFocusPathAnnotator(pathGoals map[string]string) annotation.Annotator {
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		goalID, ok := pathGoals[issue.ID]
		if !ok {
			return nil, nil
		}
		return []annotation.Annotation{{
			Kind:    annotation.FocusPath,
			Message: goalID,
		}}, nil
	}
}

// focusScope is the row set a focused view answers over: the prerequisite
// closure of every focus-labeled goal, or the whole queue when nothing is
// labeled. It is what replaced sortByFocusPath, which hoisted every path row
// above every other row BEFORE rank was consulted — a second ordering authority
// competing with the stored rank, while `lit backlog` went on describing itself
// as "priority/rank order". With ~38 rows wired to one focused goal the hoisted
// set simply was the top of the view, so a `lit rank <id> --top` that the store
// honored landed at position 39 and no surface said why (links-listing-ju7i).
//
// A scope changes MEMBERSHIP and leaves ordering alone, which is why it fixes
// what a reworded preamble could only have documented: there is no second answer
// to "what order is this in" left to keep in agreement, and `--top` reaches the
// top of whatever view it is aimed at. [LAW:one-source-of-truth] rank is the one
// ordering authority.
//
// goals is read from the walk's own output and never from the gathered rows,
// because "focus is on" and "some path row survived" are different facts. A goal
// whose path rows are all narrowed away by --type/--labels, or a childless epic
// goal whose only path row is the container that leaf-only membership drops,
// leaves no annotated row behind; a scope derived from the rows would read that
// as "nothing is focused" and silently serve the whole queue back — the exact
// substitution this type exists to make unrepresentable.
// [LAW:parse-dont-validate] the walk's answer is kept, not re-derived downstream.
type focusScope struct{ goals []string }

// focusScopeOf reads the goals out of the walk's path map, where a goal is
// exactly an issue attributed to itself: fetchFocusPathGoals seeds every goal as
// path[id] = id before the BFS and attributes each prerequisite to the goal that
// reached it, so self-attribution is the goal set with no second query and
// nothing to drift. [LAW:one-source-of-truth]
func focusScopeOf(pathGoals map[string]string) focusScope {
	var goals []string
	for id, goal := range pathGoals {
		if id == goal {
			goals = append(goals, id)
		}
	}
	sort.Strings(goals)
	return focusScope{goals: goals}
}

// active reports whether any goal carries the focus label. The zero value is
// the unfocused workspace, whose scope is the whole queue.
func (s focusScope) active() bool { return len(s.goals) > 0 }

// holds reports whether a row belongs to the scope. An inactive scope holds
// every row, so callers run the same partition over focused and unfocused
// workspaces alike rather than asking first whether focus is on.
// [LAW:dataflow-not-control-flow]
//
// Membership is read off the FocusPath annotation the walk already emitted, so
// the rule that decides what is on the path lives in fetchFocusPathGoals alone.
// [LAW:single-enforcer]
func (s focusScope) holds(row annotation.AnnotatedIssue) bool {
	return !s.active() || annotation.HasAny(row.Annotations, annotation.FocusPath)
}

// scopeFor answers which scope a run narrows by. --all asks for the whole
// queue, which is exactly the value an unfocused workspace already produces, so
// the flag picks a VALUE here and every stage after it stays unconditional.
// [LAW:dataflow-not-control-flow] the mode dies at the boundary that parsed it.
func (s focusScope) scopeFor(all bool) focusScope {
	if all {
		return focusScope{}
	}
	return s
}

// partition splits rows into the ones the scope answers over and the ones it
// excludes, preserving rank order within each. Both halves come back because
// the excluded half is not discardable: a view that drops it silently cannot
// tell "there is no work" from "there is no work ON YOUR PATH", and those are
// the two facts an agent most needs kept apart. [LAW:parse-dont-validate]
func (s focusScope) partition(rows []annotation.AnnotatedIssue) (inScope, excluded []annotation.AnnotatedIssue) {
	for _, row := range rows {
		if s.holds(row) {
			inScope = append(inScope, row)
			continue
		}
		excluded = append(excluded, row)
	}
	return inScope, excluded
}

// describe names the focused goals for a reader, in the fixed rendering both
// views use. [LAW:one-source-of-truth] one wording for one fact.
func (s focusScope) describe() string { return strings.Join(s.goals, ", ") }

// newOrphanedAnnotator returns an annotator that flags in_progress issues
// with no update in the given threshold as orphaned.
func newOrphanedAnnotator(threshold time.Duration) annotation.Annotator {
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		if issue.State() != model.StateInProgress {
			return nil, nil
		}
		age := time.Since(issue.UpdatedAt)
		if age < threshold {
			return nil, nil
		}
		return []annotation.Annotation{{
			Kind:    annotation.Orphaned,
			Message: fmt.Sprintf("in_progress for %s with no update", age.Truncate(time.Minute)),
		}}, nil
	}
}

func issueJSONFieldNames() map[string]struct{} {
	// [LAW:one-source-of-truth] The wire struct behind Issue.MarshalJSON is the
	// canonical ready-field schema. Issue's struct fields are the in-memory
	// shape and diverge from the wire (status, closed_at, resolution, and the
	// retention pair exist only on the wire), so validation reads the same set
	// serialization writes and the two cannot drift.
	names := model.IssueWireFields()
	fields := make(map[string]struct{}, len(names))
	for _, name := range names {
		fields[name] = struct{}{}
	}
	return fields
}

func issueFieldValues(issue model.Issue) (map[string]any, error) {
	payload, err := json.Marshal(issue)
	if err != nil {
		return nil, fmt.Errorf("marshal issue fields: %w", err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(payload, &values); err != nil {
		return nil, fmt.Errorf("unmarshal issue fields: %w", err)
	}
	return values, nil
}

func isRequiredFieldSet(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// enrichWithParentEpic populates ParentEpic on every row whose parent is
// type=epic. Rows with no parent or a non-epic parent get nil — the omitempty
// tag drops them from JSON output and the renderer skips them.
// [LAW:dataflow-not-control-flow] Every row flows through the same lookup;
// variability lives in whether the parent exists and its type, not in whether
// the enrichment step runs. (links-agent-epic-model-uew.2)
func enrichWithParentEpic(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations) {
	for i := range rows {
		detail := details[rows[i].ID]
		if detail.Parent == nil || !detail.Parent.IsContainer() {
			continue
		}
		rows[i].ParentEpic = &annotation.ParentEpicRef{
			ID:    detail.Parent.ID,
			Title: detail.Parent.Title,
		}
	}
}

// sortByCompositeRank orders rows by (effective_epic_rank, own_rank) so all
// leaves under a higher-ranked epic appear before any leaves under a
// lower-ranked epic — staying in one epic's context before moving to the
// next. A leaf with no parent, or a parent that is not an epic, uses its
// own rank as its epic-position, which interleaves it with epic groups at
// the correct position.
// [LAW:dataflow-not-control-flow] The sort key is a pure function of each
// row and the shared details map; variability lives in the values, not in
// whether some rows skip the sort. (links-agent-epic-model-uew.4)
func sortByCompositeRank(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations) {
	epicRank := func(issue model.Issue) string {
		parent := details[issue.ID].Parent
		if parent != nil && parent.IsContainer() {
			return parent.Rank
		}
		return issue.Rank
	}
	sort.SliceStable(rows, func(i, j int) bool {
		iEpic, jEpic := epicRank(rows[i].Issue), epicRank(rows[j].Issue)
		if iEpic != jEpic {
			return iEpic < jEpic
		}
		return rows[i].Rank < rows[j].Rank
	})
}

// sortByPriority places urgent issues before normal issues, preserving the
// existing ordering within each priority group via stable sort.
// [LAW:dataflow-not-control-flow] Every issue flows through the same comparator;
// the priority value decides ordering, not whether the comparator runs.
func sortByPriority(issues []annotation.AnnotatedIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		return issues[i].Priority > issues[j].Priority
	})
}

func applyLimit(issues []annotation.AnnotatedIssue, limit int) []annotation.AnnotatedIssue {
	if limit <= 0 || len(issues) <= limit {
		return issues
	}
	return issues[:limit]
}

// printNextSummary renders one workable leaf for `lit next` text output: the
// standard id+state+topic+title line, indented epic context if any, and inline
// dependency annotations so the agent knows what context to load before
// `lit start`.
func printNextSummary(w io.Writer, row annotation.AnnotatedIssue, cc claimContext, lane model.LaneID) error {
	line := formatIssueColumns(row.Issue, defaultColumns(), "  ", nil)
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}
	return printInlineDeps(w, row, nil, cc, lane)
}

// buildUnblocksMap derives a reverse dependency index from the classified
// open-dependency facts. For each dependency ID, it returns the IDs of open
// issues that depend on it.
// [LAW:dataflow-not-control-flow] The map is derived from existing annotation data;
// no extra store queries needed.
//
// issues must be the whole gathered workable set, never the rows a view prints.
// The reverse index is a fact about the backlog: a dependent the caller filtered
// out still gets unblocked by closing its prerequisite, and handing this the
// narrowed rows deletes that line from the prerequisite's own row instead of
// from the dependent's. Both narrowings reached it — the focus scope and
// --limit (links-listing-85sd).
func buildUnblocksMap(issues []annotation.AnnotatedIssue) map[string][]string {
	m := make(map[string][]string)
	for _, issue := range issues {
		for _, dep := range ClassifyReadiness(issue.Annotations).DependencyIDs() {
			m[dep] = append(m[dep], issue.ID)
		}
	}
	return m
}

// partitionWorkable splits the workable rows into the three buckets the
// cross-project rollup counts: in-progress work, ready leaves (open and
// unblocked), and blocked leaves (carried as their readiness so a caller can
// summarize by reason). An in-progress leaf is in-progress even if it also has
// blockers.
// [LAW:one-source-of-truth] The ready/in-flight/blocked partition is defined
// here once, so a per-project count can never disagree with the classification
// every workable view applies.
func partitionWorkable(issues []annotation.AnnotatedIssue) (inProgress, ready []annotation.AnnotatedIssue, blocked []IssueReadiness) {
	for i := range issues {
		readiness := ClassifyReadiness(issues[i].Annotations)
		switch {
		case issues[i].State() == model.StateInProgress:
			inProgress = append(inProgress, issues[i])
		case !readiness.IsReady():
			blocked = append(blocked, readiness)
		default:
			ready = append(ready, issues[i])
		}
	}
	return inProgress, ready, blocked
}

// printInlineDeps prints the shared epic/depends-on/unblocks context lines
// indented under a workable item. `lit next` shows exactly this common core;
// the backlog view (printBacklogContext) composes its extra lines around the
// same emitters. [LAW:single-enforcer]
func printInlineDeps(w io.Writer, entry annotation.AnnotatedIssue, unblocksMap map[string][]string, cc claimContext, lane model.LaneID) error {
	if err := printEpicLine(w, contextIndent, entry.ParentEpic); err != nil {
		return err
	}
	if err := printIDListLine(w, contextIndent, "depends on", ClassifyReadiness(entry.Annotations).DependencyLabels()); err != nil {
		return err
	}
	if line, ok := formatClaimLine(cc, lane, time.Now()); ok {
		if _, err := fmt.Fprintf(w, "%s%s\n", contextIndent, line); err != nil {
			return err
		}
	}
	return printIDListLine(w, contextIndent, "unblocks", unblocksMap[entry.ID])
}

// inProgressSuffix renders the age of an in-flight row and what that age means.
//
// The orphan annotation is a clock and only a clock: in_progress, no update in
// the threshold. It is enough to say a ticket has gone quiet and was never
// enough to say its holder left, so where this machine has ENUMERATED that
// holder's worktree and found it, the louder word is withdrawn — "(ORPHANED)"
// reads as permission, and it was reading that way over a worktree sitting
// locked on an open PR (links-claims-2wk2). What replaces it still says the
// claim lapsed; the claim line printed directly beneath names the holder and
// its path, which is the part a reader was previously told not to bother with.
//
// A holder this machine cannot see keeps the original word. Nothing was
// learned about it, so nothing about the old reading was wrong.
func inProgressSuffix(entry annotation.AnnotatedIssue, holder claims.Presence) string {
	age := time.Since(entry.UpdatedAt).Truncate(time.Minute)
	suffix := fmt.Sprintf("%s", age)
	if !ClassifyReadiness(entry.Annotations).IsOrphaned() {
		return suffix
	}
	switch holder {
	case claims.Locked:
		return suffix + " (stale, worktree locked)"
	case claims.Present:
		return suffix + " (stale, worktree present)"
	}
	return suffix + " (ORPHANED)"
}

// printRankInversions prints a count-only warning when dependencies are ranked
// below the issues they block, with instructions to fix.
//
// issues must be the whole gathered workable set, for the same reason
// buildUnblocksMap needs it: rank is stored globally, so an inversion is a
// property of the backlog rather than of whichever slice of it is on screen,
// and counting over the narrowed rows under-reports the repair `lit doctor
// --fix` would make.
func printRankInversions(w io.Writer, issues []annotation.AnnotatedIssue) error {
	count := 0
	for _, issue := range issues {
		count += len(ClassifyReadiness(issue.Annotations).RankInversions())
	}
	if count == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "\nWarning: %d rank inversion(s) — dependencies ranked below their dependents. Run `lit doctor --fix` to repair. "+agentInstructionsOpen+"This command is idempotent and safe to run without confirmation."+agentInstructionsClose+"\n", count)
	return err
}
