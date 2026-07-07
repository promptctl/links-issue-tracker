package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
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
// issue is flagged as orphaned. Both `lit ready`'s in-progress section
// and `lit orphaned` read from this single value so the two surfaces
// cannot drift.
// [LAW:one-source-of-truth] Single threshold for orphan detection.
const orphanedThreshold = 6 * time.Hour

// newFieldAnnotator validates requiredFields against model.Issue JSON fields,
// then returns an annotator that checks those fields on each issue.
func newFieldAnnotator(requiredFields []string) (annotation.Annotator, error) {
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
func fetchIssueRelations(ctx context.Context, st *store.Store, issues []model.Issue) (map[string]store.IssueRelations, error) {
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
			return nil, store.NotFoundError{Entity: "issue", ID: issue.ID}
		}
	}
	return relations, nil
}

// newBlockerAnnotator returns an annotator that checks open dependency blockers
// and flags rank inversions where a dependency is ranked below the dependent.
// The annotator is pure: it reads from the shared relations map rather than
// fetching from the store, so fetch cost is paid once upstream in
// fetchIssueRelations.
func newBlockerAnnotator(details map[string]store.IssueRelations) annotation.Annotator {
	// [LAW:dataflow-not-control-flow] Dependency lookup runs for every issue;
	// empty blockers list means no annotations, not a skipped operation.
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		detail := details[issue.ID]
		// Collect open blockers and sort by ID for stable annotation ordering.
		var openDeps []model.Issue
		for _, dep := range detail.DependsOn {
			if dep.State() != model.StateClosed {
				openDeps = append(openDeps, dep)
			}
		}
		sort.Slice(openDeps, func(i, j int) bool { return openDeps[i].ID < openDeps[j].ID })
		var annotations []annotation.Annotation
		for _, dep := range openDeps {
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
		return annotations, nil
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
// "has a lane". The annotator runs for every issue; a leaf with no epic parent
// or no earlier same-lane sibling yields nil, not a skipped operation.
// [LAW:one-type-per-behavior] "explicit dep unfinished" and "earlier same-lane
// sibling unfinished" are the same blocking fact behind the same enforcer.
//
// siblingsByEpic holds only unfinished siblings (the index builder applies that
// predicate once), so the annotator compares lane and rank alone.
func newSiblingGateAnnotator(details map[string]store.IssueRelations, siblingsByEpic map[string][]model.Issue) annotation.Annotator {
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		parent := details[issue.ID].Parent
		if parent == nil || !parent.IsContainer() {
			return nil, nil
		}
		var annotations []annotation.Annotation
		for _, sib := range siblingsByEpic[parent.ID] {
			if isEarlierSameLaneSibling(sib, issue) {
				annotations = append(annotations, annotation.Annotation{
					Kind:    annotation.EarlierSiblingPending,
					Message: sib.ID,
				})
			}
		}
		return annotations, nil
	}
}

// isEarlierSameLaneSibling reports whether sib precedes leaf within the same
// lane — the ONE intra-epic implicit-prerequisite rule. Both the membership
// gate (newSiblingGateAnnotator) and the focus-path derivation
// (fetchFocusPathGoals) read it, so "earlier sibling" cannot drift between
// the membership and ordering consumers.
// [LAW:single-enforcer] Single definition of the intra-epic prerequisite edge.
func isEarlierSameLaneSibling(sib, leaf model.Issue) bool {
	return sib.ID != leaf.ID && sib.Lane == leaf.Lane && sib.Rank < leaf.Rank
}

// parentEpicIDs returns the distinct ids of the container parents referenced by
// the workable leaves. These are the epics whose full child set the lane gate
// must inspect.
func parentEpicIDs(details map[string]store.IssueRelations) []string {
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
func pendingSiblingsByEpic(relations map[string]store.IssueRelations) map[string][]model.Issue {
	out := make(map[string][]model.Issue, len(relations))
	for epicID, rel := range relations {
		for _, child := range rel.Children {
			if isUnfinished(child) {
				out[epicID] = append(out[epicID], child)
			}
		}
	}
	return out
}

// isUnfinished reports whether an issue still represents pending work — the
// predicate that decides whether a sibling gates its later lane-mates and
// whether an edge belongs on a focused goal's prerequisite path. Unfiltered
// relation fetches are a trust boundary: unlike the store-filtered workable
// list, they carry archived and deleted rows, which have left the flow and
// must neither block nor be traversed.
// [LAW:no-defensive-null-guards] The archived/deleted checks translate the raw
// relation rows into the workable population; they are boundary translation,
// not a guard against a should-not-happen state.
func isUnfinished(issue model.Issue) bool {
	_, live := issue.Retention().(model.Live)
	return live && issue.State() != model.StateClosed
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
// walk depends on this two-method seam rather than the whole *store.Store, so
// its real input is nameable and a memo/decorator can stand in.
// [LAW:decomposition] The seam carries the whole truth of what the part needs.
type focusGraphSource interface {
	ListIssues(ctx context.Context, filter store.ListIssuesFilter) ([]model.Issue, error)
	GetRelationsByIDs(ctx context.Context, ids []string) (map[string]store.IssueRelations, error)
}

// fetchFocusPathGoals returns issueID -> focused-goal ID for every unfinished
// issue on the prerequisite closure of a focus-labeled goal, the goal itself
// included. An issue's prerequisites are its unfinished explicit dependencies,
// the unfinished children of a container, and its earlier same-lane unfinished
// siblings — the same implicit edge the lane gate blocks membership on, read
// through the shared isEarlierSameLaneSibling/isUnfinished predicates.
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
func fetchFocusPathGoals(ctx context.Context, src focusGraphSource, seeds ...map[string]store.IssueRelations) (map[string]string, error) {
	goals, err := src.ListIssues(ctx, store.ListIssuesFilter{
		Statuses:  []model.State{model.StateOpen, model.StateInProgress},
		LabelsAll: []string{FocusLabel},
	})
	if err != nil {
		return nil, err
	}
	cache := make(map[string]store.IssueRelations)
	for _, seed := range seeds {
		for id, rel := range seed {
			cache[id] = rel
		}
	}
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
		rels, err := relationsByID(ctx, src, cache, frontier)
		if err != nil {
			return nil, err
		}
		parentRels, err := relationsByID(ctx, src, cache, parentEpicIDs(rels))
		if err != nil {
			return nil, err
		}
		pending := pendingSiblingsByEpic(parentRels)
		var next []string
		for _, id := range frontier {
			rel, ok := rels[id]
			if !ok {
				// Frontier ids are hydrated issues from this same connection;
				// a hole means the store lied. [LAW:no-silent-failure]
				return nil, store.NotFoundError{Entity: "issue", ID: id}
			}
			var prereqs []model.Issue
			for _, dep := range rel.DependsOn {
				if isUnfinished(dep) {
					prereqs = append(prereqs, dep)
				}
			}
			if rel.Issue.IsContainer() {
				for _, child := range rel.Children {
					if isUnfinished(child) {
						prereqs = append(prereqs, child)
					}
				}
			}
			if rel.Parent != nil && rel.Parent.IsContainer() {
				for _, sib := range pending[rel.Parent.ID] {
					if isEarlierSameLaneSibling(sib, rel.Issue) {
						prereqs = append(prereqs, sib)
					}
				}
			}
			for _, prereq := range prereqs {
				if _, seen := path[prereq.ID]; seen {
					continue
				}
				path[prereq.ID] = path[id]
				next = append(next, prereq.ID)
			}
		}
		frontier = next
	}
	return path, nil
}

// relationsByID returns the relations for ids, fetching only the subjects not
// already in cache and recording new fetches back into it, so a subject is
// loaded at most once per walk. Nonexistent subjects stay absent (mirroring
// GetRelationsByIDs), so callers' presence checks still fire; only positive
// results are memoized.
// [LAW:single-enforcer] Every relation load on the focus walk goes through this
// one memo, so cross-level repeats and caller-donated subjects never re-query.
func relationsByID(ctx context.Context, src focusGraphSource, cache map[string]store.IssueRelations, ids []string) (map[string]store.IssueRelations, error) {
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := cache[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fetched, err := src.GetRelationsByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		for id, rel := range fetched {
			cache[id] = rel
		}
	}
	out := make(map[string]store.IssueRelations, len(ids))
	for _, id := range ids {
		if rel, ok := cache[id]; ok {
			out[id] = rel
		}
	}
	return out, nil
}

// newFocusPathAnnotator returns an annotator that emits a FocusPath annotation
// for any issue on a focused goal's derived prerequisite path; the message is
// the goal's ID. FocusPath is an ORDERING fact and is deliberately invisible
// to ClassifyReadiness — it can never change membership, so a blocked path
// item stays blocked and only the already-ready path items surface.
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
	// [LAW:one-source-of-truth] model.Issue JSON tags are the canonical ready-field schema.
	issueType := reflect.TypeOf(model.Issue{})
	fields := make(map[string]struct{}, issueType.NumField())
	for i := 0; i < issueType.NumField(); i++ {
		field := issueType.Field(i)
		if !field.IsExported() {
			continue
		}
		name := issueJSONFieldName(field)
		if name == "" {
			continue
		}
		fields[name] = struct{}{}
	}
	fields["status"] = struct{}{}
	fields["assignee"] = struct{}{}
	fields["closed_at"] = struct{}{}
	return fields
}

func issueJSONFieldName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "":
		return field.Name
	case "-":
		return ""
	default:
		return name
	}
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
func enrichWithParentEpic(rows []annotation.AnnotatedIssue, details map[string]store.IssueRelations) {
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
func sortByCompositeRank(rows []annotation.AnnotatedIssue, details map[string]store.IssueRelations) {
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

// sortByFocusPath places issues carrying a FocusPath annotation before all
// others, preserving the prior (priority, composite-rank) order within each
// group. Layered LAST in the shared gather so the focus path outranks standing
// urgent priority: focus is the deliberate "get me here now" directive, urgent
// is a standing attribute. Flipping that precedence is a one-line reorder
// against sortByPriority.
//
// This reads the FocusPath fact directly rather than through ClassifyReadiness:
// focus is an ORDERING interpretation, readiness a MEMBERSHIP one, and routing
// the ordering fact through the readiness classifier would re-tangle the two
// concerns the focus design keeps apart. [LAW:decomposition]
// [LAW:dataflow-not-control-flow] Same comparator runs over every pair; the
// derived annotation decides ordering, not whether the comparator runs.
func sortByFocusPath(issues []annotation.AnnotatedIssue) {
	onPath := func(row annotation.AnnotatedIssue) bool {
		return annotation.HasAny(row.Annotations, annotation.FocusPath)
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return onPath(issues[i]) && !onPath(issues[j])
	})
}

// sortByBlockingAnnotations places issues without blocking annotations first,
// preserving the original store ordering within each group. The name is
// deliberate: "readiness" is an interpretation a consumer applies over the
// neutral fact set of annotations, never a property of the annotations
// themselves. Calling this "sortByReadiness" would invite future callers to
// treat ready-ness as an annotation property and violate the "annotations
// are neutral facts" law.
func sortByBlockingAnnotations(issues []annotation.AnnotatedIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		iReady := ClassifyReadiness(issues[i].Annotations).IsReady()
		jReady := ClassifyReadiness(issues[j].Annotations).IsReady()
		return iReady && !jReady
	})
}

func applyLimit(issues []annotation.AnnotatedIssue, limit int) []annotation.AnnotatedIssue {
	if limit <= 0 || len(issues) <= limit {
		return issues
	}
	return issues[:limit]
}

// sortByContinueBias is a stable sort that pulls leaves whose parent epic is
// currently in_progress to the front, preserving composite-rank order within
// each group. The bias absorbs "what is the agent's current focus" as a sort
// key over data, not as a branch in the consumer.
// [LAW:dataflow-not-control-flow] Same comparator runs over every pair; the
// parent-epic state decides ordering, not whether the comparator runs.
func sortByContinueBias(rows []annotation.AnnotatedIssue, details map[string]store.IssueRelations) {
	inProgressEpic := func(row annotation.AnnotatedIssue) bool {
		parent := details[row.ID].Parent
		return parent != nil && parent.IsContainer() && parent.State() == model.StateInProgress
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return inProgressEpic(rows[i]) && !inProgressEpic(rows[j])
	})
}

// pickFirstReady returns the first row in the ready partition: open status
// and no blocking annotations. The predicate is stated positively (StateOpen)
// rather than as "not in_progress and not blocked" so the implementation
// matches the docstring literally and stays correct if the upstream filter
// ever widens the set of statuses it lets through. The agent should not
// `lit start` an in-progress or blocked leaf — those need `lit done` or
// unblocking first.
func pickFirstReady(rows []annotation.AnnotatedIssue) (annotation.AnnotatedIssue, bool) {
	for _, row := range rows {
		if row.State() != model.StateOpen {
			continue
		}
		if !ClassifyReadiness(row.Annotations).IsReady() {
			continue
		}
		return row, true
	}
	return annotation.AnnotatedIssue{}, false
}

// printNextSummary renders one workable leaf for `lit next` text output: the
// standard id+state+topic+title line, indented epic context if any, and inline
// dependency annotations so the agent knows what context to load before
// `lit start`.
func printNextSummary(w io.Writer, row annotation.AnnotatedIssue) error {
	columns := resolveColumns(nil)
	line := formatIssueColumns(row.Issue, columns, "  ", nil)
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}
	return printInlineDeps(w, row, nil)
}

// readyPreamble is printed before the ready list to give agents context about
// how to interpret and act on the backlog.
// [LAW:one-source-of-truth] Single definition of ready preamble text.
const readyPreamble = `This is the backlog. Always pick the top item UNLESS asked to work on a specific ticket.
You MUST carefully read every item so you understand the context for the work.
Dependencies explain the WHY behind what you are building.
You MUST design for the implementers who will build on top of your work. A poor foundation becomes
an immediate liability and should be avoided at all costs.
Downstream tickets are your real acceptance criteria —
not just "does this work in isolation" but "does this set the project up to be successful in the future."
Structure your implementation to make downstream tickets simpler and more robust,
even if the ticket doesn't specify it (but only if it aligns with the downstream tickets).
IMPORTANT: If you haven't run 'lit quickstart' yet, do so NOW to ensure you understand how to use lit.`

const readyMaxItems = 10

// buildUnblocksMap derives a reverse dependency index from the classified
// open-dependency facts. For each dependency ID, it returns the IDs of open
// issues that depend on it.
// [LAW:dataflow-not-control-flow] The map is derived from existing annotation data;
// no extra store queries needed.
func buildUnblocksMap(issues []annotation.AnnotatedIssue) map[string][]string {
	m := make(map[string][]string)
	for _, issue := range issues {
		for _, dep := range ClassifyReadiness(issue.Annotations).DependencyIDs() {
			m[dep] = append(m[dep], issue.ID)
		}
	}
	return m
}

// printReadyOutput partitions annotated issues into in-progress, ready, and blocked
// sections. Ready issues are shown with a preamble and inline dependency context,
// followed by in-progress work, then a count-by-reason summary for blocked issues.
func printReadyOutput(w io.Writer, columns []string, issues []annotation.AnnotatedIssue) error {
	resolved := resolveColumns(columns)
	var inProgress, ready []annotation.AnnotatedIssue
	var blocked []IssueReadiness
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

	unblocksMap := buildUnblocksMap(issues)

	if err := printReadySection(w, resolved, ready, unblocksMap); err != nil {
		return err
	}
	if err := printInProgressSection(w, resolved, inProgress); err != nil {
		return err
	}
	if err := printBlockedSummary(w, blocked); err != nil {
		return err
	}
	return printRankInversions(w, issues)
}

// printReadySection prints the preamble, separator, and numbered ready items
// with inline dependency info. Caps output at readyMaxItems. Agent coaching
// output belongs on stdout — stderr is for errors only.
// [LAW:single-enforcer] Single point of preamble emission.
func printReadySection(w io.Writer, columns []string, ready []annotation.AnnotatedIssue, unblocksMap map[string][]string) error {
	if _, err := fmt.Fprintln(w, readyPreamble); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, strings.Repeat("─", 80)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	display := ready
	if len(display) > readyMaxItems {
		display = display[:readyMaxItems]
	}

	if len(display) == 0 {
		_, err := fmt.Fprintln(w, "(none ready)")
		return err
	}

	// [LAW:dataflow-not-control-flow] Every ready issue flows through the same
	// numbered-line + dependency rendering path. Empty dependency slices produce
	// no output lines, not skipped operations.
	for i, entry := range display {
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, columns, "  ", nil))
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if err := printInlineDeps(w, entry, unblocksMap); err != nil {
			return err
		}
	}

	if len(ready) > readyMaxItems {
		if _, err := fmt.Fprintf(w, "\n(%d more ready tickets not shown)\n", len(ready)-readyMaxItems); err != nil {
			return err
		}
	}
	return nil
}

// printInlineDeps prints the shared epic/depends-on/unblocks context lines
// indented under a ready item. The ready view shows exactly the common core;
// the backlog view (printBacklogContext) composes its extra lines around the
// same emitters. [LAW:single-enforcer]
func printInlineDeps(w io.Writer, entry annotation.AnnotatedIssue, unblocksMap map[string][]string) error {
	if err := printEpicLine(w, contextIndent, entry.ParentEpic); err != nil {
		return err
	}
	if err := printIDListLine(w, contextIndent, "depends on", ClassifyReadiness(entry.Annotations).DependencyIDs()); err != nil {
		return err
	}
	return printIDListLine(w, contextIndent, "unblocks", unblocksMap[entry.ID])
}

func printInProgressSection(w io.Writer, columns []string, issues []annotation.AnnotatedIssue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nIn Progress"); err != nil {
		return err
	}
	for _, entry := range issues {
		line := formatIssueColumns(entry.Issue, columns, " | ", nil)
		line += " | Last Update: " + inProgressSuffix(entry)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func inProgressSuffix(entry annotation.AnnotatedIssue) string {
	age := time.Since(entry.UpdatedAt).Truncate(time.Minute)
	suffix := fmt.Sprintf("%s", age)
	if ClassifyReadiness(entry.Annotations).IsOrphaned() {
		suffix += " (ORPHANED)"
	}
	return suffix
}

// printBlockedSummary prints a compact count-by-reason summary of blocked issues.
func printBlockedSummary(w io.Writer, blocked []IssueReadiness) error {
	if len(blocked) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\nBlocked tickets: %d (blocked tickets are not displayed above)\n", len(blocked)); err != nil {
		return err
	}
	for _, kc := range blockingKindCounts(blocked) {
		if _, err := fmt.Fprintf(w, "  %d: %s\n", kc.Count, kc.Kind.String()); err != nil {
			return err
		}
	}
	return nil
}

// printRankInversions prints a count-only warning when dependencies are ranked
// below the issues they block, with instructions to fix.
func printRankInversions(w io.Writer, issues []annotation.AnnotatedIssue) error {
	count := 0
	for _, issue := range issues {
		count += len(ClassifyReadiness(issue.Annotations).RankInversions())
	}
	if count == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "\nWarning: %d rank inversion(s) — dependencies ranked below their dependents. Run `lit doctor --fix` to repair. <agent-instructions>Run it yourself, do NOT ask the user — it is idempotent and safe.</agent-instructions>\n", count)
	return err
}
