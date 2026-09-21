package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// bucketRelations sorts the structural edges incident to focalID into the four
// relation slices, hydrating counterparts from issuesByID. It is the single
// definition of how a relation row maps to parent / child / depends-on / blocks
// — shared by single-issue detail loading and batch relation loading so the
// "blocks convention: src=dependent, dst=dependency" lives in exactly one place.
// [LAW:single-enforcer] Relation-direction semantics decided once, here.
func bucketRelations(focalID string, relations []model.Relation, issuesByID map[string]model.Issue) storage.IssueRelations {
	out := storage.IssueRelations{
		Children:  []model.Issue{},
		DependsOn: []model.Issue{},
		Blocks:    []model.Issue{},
	}
	for _, rel := range relations {
		switch rel.Type {
		case model.RelBlocks:
			// blocks convention: src_id=dependent, dst_id=dependency.
			if rel.SrcID == focalID {
				if dep, ok := issuesByID[rel.DstID]; ok {
					out.DependsOn = append(out.DependsOn, dep)
				}
			}
			if rel.DstID == focalID {
				if dependent, ok := issuesByID[rel.SrcID]; ok {
					out.Blocks = append(out.Blocks, dependent)
				}
			}
		case model.RelParentChild:
			if rel.SrcID == focalID {
				if parent, ok := issuesByID[rel.DstID]; ok {
					out.Parent = &parent
				}
			}
			if rel.DstID == focalID {
				if child, ok := issuesByID[rel.SrcID]; ok {
					out.Children = append(out.Children, child)
				}
			}
		}
	}
	sortIssuesByRank(out.Children)
	sortIssuesByRank(out.DependsOn)
	sortIssuesByRank(out.Blocks)
	return out
}

// relatedFrom returns the hydrated "related-to" counterparts of focalID. It is
// GetIssueDetail's concern only — no batch consumer needs related edges, so it
// stays out of the shared IssueRelations shape.
func relatedFrom(focalID string, relations []model.Relation, issuesByID map[string]model.Issue) []model.Issue {
	out := []model.Issue{}
	for _, rel := range relations {
		if rel.Type != model.RelRelatedTo {
			continue
		}
		other := rel.SrcID
		if other == focalID {
			other = rel.DstID
		}
		if related, ok := issuesByID[other]; ok {
			out = append(out, related)
		}
	}
	sortIssuesByRank(out)
	return out
}

// siblingsOf returns parentChildren with the focal issue removed — the
// "children-of-parent-minus-self" derivation. Order is preserved (callers pass
// already rank-ordered children), so the result needs no resort. A focal issue
// that is an only child yields an empty slice. This is the single definition of
// the sibling set, shared by GetIssueDetail and the done/close adjacency view.
// [LAW:one-source-of-truth] Sibling derivation decided once, here.
func siblingsOf(focalID string, parentChildren []model.Issue) []model.Issue {
	out := make([]model.Issue, 0, len(parentChildren))
	for _, child := range parentChildren {
		if child.ID == focalID {
			continue
		}
		out = append(out, child)
	}
	return out
}

// GetRelationsByIDs batch-loads the structural relations for every listed id in
// a fixed number of queries rather than GetIssueDetail-per-id. Subjects that no
// longer exist are simply absent from the result, mirroring getIssuesByIDs;
// callers iterating known-present ids never observe the hole.
// [LAW:dataflow-not-control-flow] One relations query plus one issue-hydration
// query feed a pure bucketing pass — the per-subject work is map lookups, not
// extra round-trips.
func (s *Store) GetRelationsByIDs(ctx context.Context, ids []string) (map[string]storage.IssueRelations, error) {
	subjects := dedupeStrings(ids)
	if len(subjects) == 0 {
		return map[string]storage.IssueRelations{}, nil
	}
	relations, err := s.listRelationsForIDs(ctx, subjects)
	if err != nil {
		return nil, err
	}
	subjectSet := make(map[string]struct{}, len(subjects))
	needed := make(map[string]struct{}, len(subjects))
	for _, id := range subjects {
		subjectSet[id] = struct{}{}
		needed[id] = struct{}{}
	}
	bySubject := make(map[string][]model.Relation, len(subjects))
	for _, rel := range relations {
		needed[rel.SrcID] = struct{}{}
		needed[rel.DstID] = struct{}{}
		if _, ok := subjectSet[rel.SrcID]; ok {
			bySubject[rel.SrcID] = append(bySubject[rel.SrcID], rel)
		}
		if _, ok := subjectSet[rel.DstID]; ok && rel.DstID != rel.SrcID {
			bySubject[rel.DstID] = append(bySubject[rel.DstID], rel)
		}
	}
	issuesByID, err := s.getIssuesByIDs(ctx, mapKeys(needed))
	if err != nil {
		return nil, err
	}
	out := make(map[string]storage.IssueRelations, len(subjects))
	for _, id := range subjects {
		issue, ok := issuesByID[id]
		if !ok {
			continue
		}
		rel := bucketRelations(id, bySubject[id], issuesByID)
		rel.Issue = issue
		out[id] = rel
	}
	return out, nil
}

// structuralRelationTypes are the edge types bucketRelations interprets and
// GetRelationsByIDs returns. related-to is excluded so its endpoints are never
// pulled into the batch's hydration set — that is the whole point of the
// lightweight accessor vs GetIssueDetail.
var structuralRelationTypes = []model.RelationType{model.RelBlocks, model.RelParentChild}

// listRelationsForIDs returns every structural relation row incident to any of
// the given ids — the batch counterpart of listRelations, scoped to the edge
// types GetRelationsByIDs serves.
//
// It runs one query per endpoint column and merges in Go instead of a single
// `src_id IN (…) OR dst_id IN (…)` query. The OR is a performance landmine, not
// a style choice: no index covers both columns, so the engine's costed-index
// analysis carries an unbounded range for the other column's disjuncts on every
// candidate index, and its overlap elimination then does quadratic
// collation-aware compares across the blown-up range set — seconds of pure
// analysis once the id list reaches backlog size. Two single-column
// conjunctive queries keep every range a point and the analysis linear.
// [LAW:carrying-cost] the merge code below is the whole price; the OR's price
// grew with every ticket filed.
func (s *Store) listRelationsForIDs(ctx context.Context, ids []string) ([]model.Relation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	bySrc, err := s.relationsByEndpoint(ctx, "src_id", ids)
	if err != nil {
		return nil, err
	}
	byDst, err := s.relationsByEndpoint(ctx, "dst_id", ids)
	if err != nil {
		return nil, err
	}
	return mergeRelations(bySrc, byDst), nil
}

// relationEndpointColumns is the closed set of column names
// relationsByEndpoint may interpolate. The query text is built with Sprintf, so
// membership here is what keeps that interpolation a choice between two
// constants rather than an injection surface. [LAW:parse-dont-validate]
var relationEndpointColumns = map[string]struct{}{"src_id": {}, "dst_id": {}}

// relationsByEndpoint returns the structural relations whose given endpoint
// column matches any of ids, in no particular order — mergeRelations is the
// sole owner of the final ordering, so an ORDER BY here would be dead work.
// [LAW:one-source-of-truth]
func (s *Store) relationsByEndpoint(ctx context.Context, column string, ids []string) ([]model.Relation, error) {
	if _, ok := relationEndpointColumns[column]; !ok {
		return nil, fmt.Errorf("list relations by endpoint: unknown column %q", column)
	}
	// Bounded by a closed domain, so it is one clause and not a batch loop:
	// structuralRelationTypes is a package-level constant list of two, and no
	// caller can lengthen it. See idBatchSize for why an id list — which a
	// caller CAN lengthen — may not be written this way.
	typeClause := strings.Join(repeatPlaceholder(len(structuralRelationTypes)), ",")
	rels := []model.Relation{}
	for _, batch := range idBatches(ids) {
		idClause, args := batch.inList()
		for _, relType := range structuralRelationTypes {
			args = append(args, string(relType))
		}
		query := fmt.Sprintf(`SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE %s IN (%s) AND type IN (%s)`, column, idClause, typeClause)
		batched, err := s.scanRelationRows(ctx, query, args)
		if err != nil {
			return nil, err
		}
		rels = append(rels, batched...)
	}
	return rels, nil
}

// scanRelationRows runs one endpoint query and reads its rows. It is separate
// from the batching above so that the shape of a relation row is stated once,
// however many queries the id set turns into.
func (s *Store) scanRelationRows(ctx context.Context, query string, args []any) ([]model.Relation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list relations for ids: %w", err)
	}
	defer rows.Close()
	rels := []model.Relation{}
	for rows.Next() {
		var rel model.Relation
		var createdAt string
		if err := rows.Scan(&rel.SrcID, &rel.DstID, &rel.Type, &createdAt, &rel.CreatedBy); err != nil {
			return nil, err
		}
		t, err := scanTime(createdAt)
		if err != nil {
			return nil, err
		}
		rel.CreatedAt = t
		rels = append(rels, rel)
	}
	return rels, rows.Err()
}

// mergeRelations combines the two endpoint result sets into the order the
// single query produced: created_at ascending, deduplicated by primary key. A
// row whose src and dst are both subjects arrives from both queries and must
// count once. Ties on created_at break by primary key, so the merged order is
// deterministic where the SQL ordering never was.
func mergeRelations(bySrc, byDst []model.Relation) []model.Relation {
	seen := make(map[relationKey]struct{}, len(bySrc)+len(byDst))
	merged := make([]model.Relation, 0, len(bySrc)+len(byDst))
	for _, rel := range append(bySrc, byDst...) {
		key := relationKey{srcID: rel.SrcID, dstID: rel.DstID, kind: rel.Type}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, rel)
	}
	slices.SortFunc(merged, func(a, b model.Relation) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.SrcID, b.SrcID); c != 0 {
			return c
		}
		if c := strings.Compare(a.DstID, b.DstID); c != 0 {
			return c
		}
		return strings.Compare(string(a.Type), string(b.Type))
	})
	return merged
}

// dedupeStrings returns the distinct values of ids preserving first-seen order.
func dedupeStrings(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// repeatPlaceholder returns n "?" SQL placeholder tokens.
func repeatPlaceholder(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "?"
	}
	return out
}

// mapKeys returns the keys of set in unspecified order.
func mapKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func (s *Store) AddRelation(ctx context.Context, in storage.AddRelationInput) (model.Relation, error) {
	// [LAW:types-are-the-program] in.Type is sealed at the trust boundary by
	// ParseRelationType; no string re-validation here.
	if in.Type == model.RelRelatedTo && in.SrcID == in.DstID {
		return model.Relation{}, errors.New("related-to cannot target itself")
	}
	srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)
	now := s.clock.Now()
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: in.Type, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	if err := s.withMutation(ctx, "add relation", func(ctx context.Context, tx *sql.Tx) error {
		return addRelationTx(ctx, tx, rel)
	}); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

// addRelationTx is the one body every relation edge is written through,
// whichever verb the caller typed. `lit dep add` and `lit parent set` are two
// doors onto this one room, which is why an edge the graph may not hold cannot
// be true of one door and not the other — the rule lives here rather than in
// each method that reaches for it. [LAW:single-enforcer]
//
// It is also where the two engines meet: the in-memory engine funnels its own
// SetParent through addRelation for the same reason, so the rules below are one
// definition rendered twice rather than two that happen to agree.
// [LAW:one-source-of-truth]
//
// [LAW:no-ambient-temporal-coupling] Both endpoints are proven to exist on this
// tx, under the held commit lock, so the edge cannot be written against an
// endpoint a concurrent delete removed between check and write.
func addRelationTx(ctx context.Context, tx *sql.Tx, rel model.Relation) error {
	if err := requireIssueExistsTx(ctx, tx, rel.SrcID); err != nil {
		return err
	}
	if err := requireIssueExistsTx(ctx, tx, rel.DstID); err != nil {
		return err
	}
	if err := rejectCycleTx(ctx, tx, rel); err != nil {
		return err
	}
	// [LAW:single-enforcer] Single-parent cardinality is enforced here for
	// every write path, not only in SetParent: a single-valued type clears any
	// existing edge from this src before inserting, so 'lit dep add --type
	// parent-child' can never leave a child with two parents.
	// [LAW:dataflow-not-control-flow] The clear-or-not choice is driven by the
	// type's cardinality value, not by which store method the caller invoked.
	if rel.Type.SingleValuedFromSrc() {
		return setSingleValuedEdgeTx(ctx, tx, rel)
	}
	return insertRelationTx(ctx, tx, rel)
}

// rejectCycleTx refuses an edge whose type may not close a loop. Two types carry
// an acyclicity rule, over different graphs and for different reasons, and
// related-to carries none — which it states by having no arm rather than by a
// guard somewhere deciding it is exempt.
// [LAW:dataflow-not-control-flow] The one branch here is the domain's own enum.
func rejectCycleTx(ctx context.Context, tx *sql.Tx, rel model.Relation) error {
	switch rel.Type {
	case model.RelBlocks:
		// A rank order is a total order, and one that honors every blocks edge
		// exists iff there is no cycle, so a cycle is an unsatisfiable
		// constraint set rather than an awkward shape.
		return rejectBlocksCycle(ctx, tx, rel.SrcID, rel.DstID)
	case model.RelParentChild:
		return rejectParentCycle(ctx, tx, rel.SrcID, rel.DstID)
	}
	return nil
}

// insertRelationTx writes one relation row on the given transaction. It runs on
// a caller-supplied tx so the write can join a larger atomic unit rather than
// opening its own transaction. Endpoint canonicalization and validation are the
// caller's responsibility; this is the raw write.
// [LAW:one-source-of-truth] The relations INSERT statement lives only here.
func insertRelationTx(ctx context.Context, tx *sql.Tx, rel model.Relation) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
		return fmt.Errorf("insert relation %s->%s (%s): %w", rel.SrcID, rel.DstID, rel.Type, err)
	}
	return nil
}

// setSingleValuedEdgeTx writes rel while enforcing its src's outgoing
// cardinality: it first deletes any existing edge of rel.Type sharing rel.SrcID,
// so the src ends with exactly one such edge. Keyed on rel.Type rather than a
// hardcoded type, it is the single owner of the clear-then-insert that
// single-valued relation types (RelationType.SingleValuedFromSrc) require — every
// write path routes here, so the cardinality is enforced once, not per-caller.
// It sits above insertRelationTx, which stays the raw replay-faithful write that
// import restore depends on; the clear belongs to the relation boundary, not the
// raw insert.
// [LAW:single-enforcer] The single-valued clear-then-insert lives only here.
func setSingleValuedEdgeTx(ctx context.Context, tx *sql.Tx, rel model.Relation) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = ?`, rel.SrcID, string(rel.Type)); err != nil {
		return fmt.Errorf("clear single-valued relation: %w", err)
	}
	return insertRelationTx(ctx, tx, rel)
}

// rejectBlocksCycle errors if inserting the blocks edge dependent->dependency
// would close a cycle in the precedence graph. A self-edge is the degenerate
// 1-cycle; a longer cycle exists when the new dependent already precedes the
// new dependency through existing blocks edges, since the new edge asserts the
// reverse. The check runs inside the mutation tx so it sees a consistent
// snapshot of existing edges.
func rejectBlocksCycle(ctx context.Context, tx *sql.Tx, dependent, dependency string) error {
	if dependent == dependency {
		return fmt.Errorf("blocks: %s cannot block itself", dependent)
	}
	edges, err := loadBlocksEdges(ctx, tx)
	if err != nil {
		return fmt.Errorf("blocks cycle check: %w", err)
	}
	if blocksPrecedes(blocksPrecedenceAdj(edges), dependent, dependency) {
		return fmt.Errorf("blocks: cannot add %s depends-on %s — %s already depends on %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order", dependent, dependency, dependency, dependent)
	}
	return nil
}

// rejectParentCycle errors if making child a child of parent would close a loop
// in the hierarchy.
//
// A hierarchy is a tree: every issue reaches a root by walking up, and that walk
// is what container state derivation, the ancestor walk gating a leaf on its
// container's blocks edges, and rank's top-level-ancestor resolution are all
// built on. A cycle has no root, so those walks do not return a wrong answer —
// they do not terminate, and hydrating any issue in the loop overflows the
// stack. That is incoherent state rather than an unusual hierarchy, so the edge
// that would create it is refused here instead of guarded against by every
// consumer that walks up. [LAW:types-are-the-program]
//
// The edge runs child -> parent, so the loop closes exactly when parent is
// already at or below child: walking up from parent reaches child. Reading the
// edges as a child -> parent map keeps that walk to one step per ancestor.
func rejectParentCycle(ctx context.Context, tx *sql.Tx, childID, parentID string) error {
	if childID == parentID {
		return fmt.Errorf("parent-child: %s cannot be its own parent", childID)
	}
	parentOf, err := loadParentEdges(ctx, tx)
	if err != nil {
		return fmt.Errorf("parent cycle check: %w", err)
	}
	// Everything the hierarchy can already climb to from the proposed parent.
	// The edge runs child -> parent, so it closes a loop exactly when the child
	// is somewhere in here. seen bounds the walk, because data written before
	// this rule can already hold a loop, and the check meant to prevent one must
	// not be the thing that hangs on it.
	above := map[string][]string{}
	seen := map[string]struct{}{parentID: {}}
	for stack := []string{parentID}; len(stack) > 0; {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		parents := parentOf[at]
		if len(parents) == 0 {
			continue
		}
		above[at] = parents
		for _, parent := range parents {
			if parent == childID {
				return fmt.Errorf("parent-child: cannot make %s a child of %s — %s is already below %s in the hierarchy, so this edge would close a parent cycle, which has no root", childID, parentID, parentID, childID)
			}
			if _, visited := seen[parent]; visited {
				continue
			}
			seen[parent] = struct{}{}
			stack = append(stack, parent)
		}
	}
	// A loop already sitting above the parent is refused too, and named rather
	// than walked into. The edge would be legal in itself; the hierarchy it
	// would join is one no consumer can climb, so accepting it would bury a
	// second issue under the first. [LAW:no-silent-failure]
	// [LAW:one-source-of-truth] The same detector Doctor reports with, asked
	// about the subgraph this edge would attach to rather than the workspace.
	if cycle := parentCycle(above); len(cycle) > 0 {
		return fmt.Errorf("parent-child: cannot make %s a child of %s — the hierarchy above %s already holds a cycle (%s); break it with 'lit dep rm' on one of those edges, then retry", childID, parentID, parentID, strings.Join(cycle, " -> "))
	}
	return nil
}

// parentCycle returns the members of one loop in the hierarchy, in walk order,
// or nothing when the parent graph is a forest.
//
// The write boundary refuses the edge that would close a loop, so a workspace
// written under that rule cannot grow one — but data written before it, or
// restored from an export, still can, and every walk up the parent chain
// hangs on it. Doctor is where that pre-existing state is named, the same
// division the blocks cycle already uses: refuse at the boundary, report what
// the boundary was not there to refuse. [LAW:single-enforcer]
//
// A child can hold more than one parent edge in restored data, so the walk is a
// depth-first search rather than a single chain, and it carries its path so the
// loop can be named by its members rather than merely detected. A node still on
// the current path is the loop; a node already settled reaches a root, and its
// ancestry is never walked twice, which keeps the whole scan linear in edges.
func parentCycle(parentOf map[string][]string) []string {
	const (
		unvisited = iota
		onPath
		settled
	)
	state := make(map[string]int, len(parentOf))
	starts := make([]string, 0, len(parentOf))
	for child := range parentOf {
		starts = append(starts, child)
	}
	// The map's iteration order is random; a cycle report that names the same
	// members in a different order on every run is a fact nobody can act on.
	slices.Sort(starts)
	for _, start := range starts {
		if state[start] != unvisited {
			continue
		}
		// path is the chain of nodes currently being descended; taken[i] counts
		// how many of path[i]'s parents have been followed already.
		path := []string{start}
		taken := []int{0}
		state[start] = onPath
		for len(path) > 0 {
			at := len(path) - 1
			parents := parentOf[path[at]]
			if taken[at] >= len(parents) {
				state[path[at]] = settled
				path, taken = path[:at], taken[:at]
				continue
			}
			parent := parents[taken[at]]
			taken[at]++
			switch state[parent] {
			case onPath:
				for i, id := range path {
					if id == parent {
						return append([]string{}, path[i:]...)
					}
				}
			case unvisited:
				state[parent] = onPath
				path = append(path, parent)
				taken = append(taken, 0)
			}
		}
	}
	return nil
}

// loadParentEdges returns the parent edges as the hierarchy is actually walked:
// child -> every parent it can climb to. Two details of that are load-bearing.
//
// No lifecycle predicate narrows it. Consumers each climb their own subset —
// lifecycleChildrenByEpicIDs, for one, keeps a dead container's membership and
// so traverses edges whose parent is archived or deleted — and a detector
// scoped to any single consumer's subset reports a clean hierarchy while some
// other walk still runs forever. Reading every stored edge makes this a
// superset of all of them, which is the only version that cannot drift as
// consumers change. [LAW:one-source-of-truth]
//
// It is also the only version that survives a restore. Soft delete is
// reversible, so a loop tolerated because one member is currently deleted
// becomes a live loop the moment that member comes back — a guard that allowed
// it would have promised an invariant it does not hold. The rule is therefore
// about the edges, not about the lifecycle of their endpoints: the relations
// table never holds a parent cycle.
//
// The map is multi-valued because single-parent cardinality is enforced at the
// write boundary, and this reads the rows the boundary was not there to gate:
// the reconcile replay writes relation rows verbatim through insertRelationTx,
// so restored data can hold two parents for one child. Keeping one parent per
// child here would be a theorem about the data that the restore path falsifies,
// and the detector exists for precisely that data.
// [LAW:types-are-the-program] The strongest theorem that is still true.
func loadParentEdges(ctx context.Context, q rowQueryer) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.src_id, r.dst_id FROM relations r
		JOIN issues i ON i.id = r.src_id
		JOIN issues p ON p.id = r.dst_id
		WHERE r.type = 'parent-child'
		ORDER BY r.src_id, r.dst_id`)
	if err != nil {
		return nil, fmt.Errorf("query parent edges: %w", err)
	}
	defer rows.Close()
	parentOf := make(map[string][]string)
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, fmt.Errorf("scan parent edge: %w", err)
		}
		parentOf[child] = append(parentOf[child], parent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parent edge rows: %w", err)
	}
	return parentOf, nil
}

func (s *Store) RemoveRelation(ctx context.Context, srcID, dstID string, relType model.RelationType) error {
	srcID, dstID = relType.CanonicalEndpoints(srcID, dstID)
	return s.withMutation(ctx, "remove relation", func(ctx context.Context, tx *sql.Tx) error {
		// [LAW:single-enforcer] the relations DELETE lives in deleteRelationRowTx,
		// shared with the reconcile replay's delta, exactly as the INSERT lives
		// only in insertRelationTx.
		affected, err := deleteRelationRowTx(ctx, tx, relationKey{srcID: srcID, dstID: dstID, kind: relType})
		if err != nil {
			return err
		}
		if affected == 0 {
			return storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}
		}
		return nil
	})
}

// ListRelationsForIssue returns the relations incident to issueID, optionally
// restricted to the given types; no types means no restriction.
// [LAW:dataflow-not-control-flow] The absent-filter case is the empty filter
// set, not a sentinel string.
func (s *Store) ListRelationsForIssue(ctx context.Context, issueID string, types ...model.RelationType) ([]model.Relation, error) {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return nil, err
	}
	rels, err := s.listRelations(ctx, issueID)
	if err != nil {
		return nil, err
	}
	if len(types) == 0 {
		return rels, nil
	}
	wanted := make(map[model.RelationType]struct{}, len(types))
	for _, t := range types {
		wanted[t] = struct{}{}
	}
	out := make([]model.Relation, 0, len(rels))
	for _, rel := range rels {
		if _, ok := wanted[rel.Type]; ok {
			out = append(out, rel)
		}
	}
	return out, nil
}

func (s *Store) SetParent(ctx context.Context, in storage.SetParentInput) (model.Relation, error) {
	if strings.TrimSpace(in.ChildID) == "" || strings.TrimSpace(in.ParentID) == "" {
		return model.Relation{}, errors.New("child and parent ids are required")
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, errors.New("child and parent cannot be the same issue")
	}
	rel := model.Relation{
		SrcID:     in.ChildID,
		DstID:     in.ParentID,
		Type:      model.RelParentChild,
		CreatedAt: s.clock.Now(),
		CreatedBy: strings.TrimSpace(in.CreatedBy),
	}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	if err := s.withMutation(ctx, "set parent", func(ctx context.Context, tx *sql.Tx) error {
		// [LAW:single-enforcer] SetParent is one validated caller of the shared
		// relation write, not a second copy of the rules it carries: the
		// endpoint proofs, the cycle refusal and the single-parent
		// clear-then-insert all live in addRelationTx, so reparenting cannot
		// obey a different hierarchy rule than 'lit dep add' does.
		return addRelationTx(ctx, tx, rel)
	}); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

// ClearParent detaches a child from its parent.
//
// It reads nothing before the DELETE. That is what makes it usable on the one
// workspace that needs it most: a hierarchy holding a loop, which `lit doctor`
// names and tells the operator to break here. Hydrating the child first —
// GetIssue climbs the parent chain — overflowed the stack on exactly that
// state, so the repair crashed on the fault it was the repair for.
// Existence is still proven, on the tx and without hydrating, because the two
// absences are different diagnoses: "no such issue" and "that issue has no
// parent" send the operator to different places, and collapsing them into the
// DELETE's rows-affected would report a typo'd id as a missing edge. It is the
// hydration that had to go, not the proof. [LAW:no-silent-failure]
func (s *Store) ClearParent(ctx context.Context, childID string) error {
	return s.withMutation(ctx, "clear parent", func(ctx context.Context, tx *sql.Tx) error {
		if err := requireIssueExistsTx(ctx, tx, childID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, childID)
		if err != nil {
			return fmt.Errorf("delete parent relation: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected: %w", err)
		}
		if affected == 0 {
			return storage.NotFoundError{Entity: "parent relation", ID: childID}
		}
		return nil
	})
}
