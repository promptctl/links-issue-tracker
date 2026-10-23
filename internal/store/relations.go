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
// definition of how a relation row maps to parent / child / blocked-by / blocks
// — shared by single-issue detail loading and batch relation loading so the
// reading "src blocked-by dst" lives in exactly one place.
// [LAW:single-enforcer] Relation-direction semantics decided once, here.
func bucketRelations(focalID string, relations []model.Relation, issuesByID map[string]model.Issue, ancestry storage.RankAncestry) storage.IssueRelations {
	out := storage.IssueRelations{
		Children:  []model.Issue{},
		BlockedBy: []model.Issue{},
		Blocks:    []model.Issue{},
	}
	for _, rel := range relations {
		switch rel.Type {
		case model.RelBlockedBy:
			// src blocked-by dst: src waits, dst is what it waits for.
			if rel.SrcID == focalID {
				if dep, ok := issuesByID[rel.DstID]; ok {
					out.BlockedBy = append(out.BlockedBy, dep)
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
	ancestry.Sort(out.Children)
	ancestry.Sort(out.BlockedBy)
	ancestry.Sort(out.Blocks)
	return out
}

// relatedFrom returns the hydrated "related-to" counterparts of focalID. It is
// GetIssueDetail's concern only — no batch consumer needs related edges, so it
// stays out of the shared IssueRelations shape.
func relatedFrom(focalID string, relations []model.Relation, issuesByID map[string]model.Issue, ancestry storage.RankAncestry) []model.Issue {
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
	ancestry.Sort(out)
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
	ancestry, err := loadRankAncestry(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]storage.IssueRelations, len(subjects))
	for _, id := range subjects {
		issue, ok := issuesByID[id]
		if !ok {
			continue
		}
		rel := bucketRelations(id, bySubject[id], issuesByID, ancestry)
		rel.Issue = issue
		out[id] = rel
	}
	return out, nil
}

// ParentByChild is one query over the relations table: no id list to batch and no
// issue to hydrate, which is the whole of its advantage over GetRelationsByIDs.
// MIN is the two-parent rule the contract states. [LAW:one-source-of-truth]
func (s *Store) ParentByChild(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, MIN(dst_id) FROM relations WHERE type = ? GROUP BY src_id`, string(model.RelParentChild))
	if err != nil {
		return nil, fmt.Errorf("list parent ids: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var childID, parentID string
		if err := rows.Scan(&childID, &parentID); err != nil {
			return nil, fmt.Errorf("scan parent ids: %w", err)
		}
		out[childID] = parentID
	}
	return out, rows.Err()
}

// structuralRelationTypes are the edge types bucketRelations interprets and
// GetRelationsByIDs returns. related-to is excluded so its endpoints are never
// pulled into the batch's hydration set — that is the whole point of the
// lightweight accessor vs GetIssueDetail.
var structuralRelationTypes = []model.RelationType{model.RelBlockedBy, model.RelParentChild}

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
// would grow with every ticket filed.
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
	// The type list is bounded by a closed domain, so it stays an IN clause:
	// structuralRelationTypes is a package-level constant list of two, and no
	// caller can lengthen it. The id list is not, so it is a join; see idSet.
	ends, arg := newIDSet(ids).rows("ends")
	args := []any{arg}
	for _, relType := range structuralRelationTypes {
		args = append(args, string(relType))
	}
	query := fmt.Sprintf(`SELECT r.src_id, r.dst_id, r.type, r.created_at, r.created_by FROM %s JOIN relations r ON r.%s = ends.id WHERE r.type IN (%s)`, ends, column, placeholders(len(structuralRelationTypes)))
	return s.scanRelationRows(ctx, query, args)
}

// scanRelationRows runs an endpoint query and reads its rows.
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

// mergeRelations combines the two endpoint result sets into one order:
// created_at ascending, deduplicated by primary key. A row whose src and dst
// are both subjects arrives from both queries and must count once. Ties on
// created_at break by primary key, so the merged order is deterministic.
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
		return model.Relation{}, model.ValidationError{Message: "related-to cannot target itself"}
	}
	srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)
	now := s.clock.Now()
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: in.Type, CreatedAt: now, CreatedBy: storage.Actor(in.CreatedBy)}
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
	// The dst's type is the dst's existence proof: one read answers both,
	// as the memory engine's record lookup does. [LAW:one-source-of-truth]
	dstType, err := issueTypeTx(ctx, tx, rel.DstID)
	if err != nil {
		return err
	}
	if err := rejectHeldRelationTx(ctx, tx, rel); err != nil {
		return err
	}
	if err := storage.RejectNonContainerParentEdge(rel.Type, rel.DstID, dstType); err != nil {
		return err
	}
	if err := rejectCycleTx(ctx, tx, rel); err != nil {
		return err
	}
	// [LAW:single-enforcer] Single-parent cardinality is enforced here for
	// every write path, not only in SetParent: a single-valued type clears any
	// existing edge from this src before inserting, so a parent-child edge
	// written through AddRelation (an import, say) can never leave a child with
	// two parents.
	// [LAW:dataflow-not-control-flow] The clear-or-not choice is driven by the
	// type's cardinality value, not by which store method the caller invoked.
	if rel.Type.SingleValuedFromSrc() {
		return setSingleValuedEdgeTx(ctx, tx, rel)
	}
	return insertRelationTx(ctx, tx, rel)
}

// rejectHeldRelationTx answers an add whose state already holds with the
// contract's satisfied answer, as the memory engine's relationHeld does, rather
// than letting the raw INSERT surface the primary-key violation or the
// single-valued replace rewrite the row. The state an add asks for is that
// the edges in its write's scope are exactly the one asked for, and the type's
// cardinality sets the scope: every edge of the type from src for a
// single-valued type, so a child holding a second parent is a replace still to
// make, which is the repair doctor names; the identical edge alone otherwise.
// It runs before every rule that judges a write: the containment, same-epic
// and cycle rules govern what a new edge may change, and a held state changes
// nothing, so a shape a reparent left behind is answered as held rather than
// refused as a write. It sits here and not in insertRelationTx, which stays
// the raw replay-faithful write.
// [LAW:one-source-of-truth] [LAW:dataflow-not-control-flow]
func rejectHeldRelationTx(ctx context.Context, tx *sql.Tx, rel model.Relation) error {
	rows, err := tx.QueryContext(ctx, `SELECT dst_id FROM relations WHERE src_id = ? AND type = ? AND (? OR dst_id = ?)`, rel.SrcID, string(rel.Type), rel.Type.SingleValuedFromSrc(), rel.DstID)
	if err != nil {
		return fmt.Errorf("read relations %s (%s): %w", rel.SrcID, rel.Type, err)
	}
	defer rows.Close()
	var inScope []string
	for rows.Next() {
		var dstID string
		if err := rows.Scan(&dstID); err != nil {
			return fmt.Errorf("scan relation %s (%s): %w", rel.SrcID, rel.Type, err)
		}
		inScope = append(inScope, dstID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read relations %s (%s): %w", rel.SrcID, rel.Type, err)
	}
	if slices.Equal(inScope, []string{rel.DstID}) {
		return storage.RelationExistsError{SrcID: rel.SrcID, DstID: rel.DstID, Type: rel.Type}
	}
	return nil
}

// rejectCycleTx refuses an edge whose type may not close a loop. Two types carry
// an acyclicity rule, over different graphs and for different reasons, and
// related-to carries none — which it states by having no arm rather than by a
// guard somewhere deciding it is exempt.
// [LAW:dataflow-not-control-flow] The one branch here is the domain's own enum.
func rejectCycleTx(ctx context.Context, tx *sql.Tx, rel model.Relation) error {
	switch rel.Type {
	case model.RelBlockedBy:
		// A rank order is a total order, and one that honors every blocked-by edge
		// exists iff there is no cycle, so a cycle is an unsatisfiable
		// constraint set rather than an awkward shape. The same-epic rule is
		// judged first, as the import gate judges it: an edge inside one epic
		// that also closes a cycle is refused by the rule that names the fix.
		tree, err := loadEpicTree(ctx, tx)
		if err != nil {
			return fmt.Errorf("blocks hierarchy check: %w", err)
		}
		if err := storage.RejectBlocksWithinEpic(tree, rel.SrcID, rel.DstID); err != nil {
			return err
		}
		return rejectBlocksCycle(ctx, tx, rel.SrcID, rel.DstID)
	case model.RelParentChild:
		return rejectParentCycle(ctx, tx, rel.SrcID, rel.DstID)
	}
	return nil
}

// issueTypeTx reads an issue's type on the tx, proving the row exists in the
// same read: the containment rules judge an endpoint by its type, so this is
// the existence proof the relation writes take for it. It hydrates nothing —
// no parent chain is climbed — so it is safe on a hierarchy holding a loop,
// which is what ClearParent needs it for.
func issueTypeTx(ctx context.Context, tx *sql.Tx, issueID string) (model.IssueType, error) {
	var issueType model.IssueType
	if err := tx.QueryRowContext(ctx, `SELECT issue_type FROM issues WHERE id = ?`, issueID).Scan(&issueType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", storage.NotFoundError{Entity: "issue", ID: issueID}
		}
		return "", fmt.Errorf("read issue type: %w", err)
	}
	return issueType, nil
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

// rejectBlocksCycle errors if inserting the blocked-by edge dependent->dependency
// would close a cycle in the precedence graph. A self-edge is the degenerate
// 1-cycle; a longer cycle exists when the new dependent already precedes the
// new dependency through existing blocked-by edges, since the new edge asserts the
// reverse. The check runs inside the mutation tx so it sees a consistent
// snapshot of existing edges.
func rejectBlocksCycle(ctx context.Context, tx *sql.Tx, dependent, dependency string) error {
	if dependent == dependency {
		return model.ValidationError{Message: fmt.Sprintf("blocked-by: %s cannot be blocked by itself", dependent)}
	}
	edges, err := loadBlocksEdges(ctx, tx)
	if err != nil {
		return fmt.Errorf("blocked-by cycle check: %w", err)
	}
	if blocksPrecedes(blocksPrecedenceAdj(edges), dependent, dependency) {
		return model.ValidationError{Message: fmt.Sprintf("blocked-by: cannot add %s blocked by %s — %s is already blocked by %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order", dependent, dependency, dependency, dependent)}
	}
	return nil
}

// rejectParentCycle errors if making child a child of parent would close a loop
// in the hierarchy.
//
// A hierarchy is a tree: every issue reaches a root by walking up, and that walk
// is what container state derivation, the ancestor walk gating a leaf on its
// container's blocked-by edges, and rank's top-level-ancestor resolution are all
// built on. A cycle has no root, so those walks have no answer to return, and
// a read that reaches one refuses it (storage.ParentCycleError). That is
// incoherent state rather than an unusual hierarchy, so the edge that would
// create it is refused here instead of guarded against by every consumer that
// walks up. [LAW:types-are-the-program]
//
// The edge runs child -> parent, so the loop closes exactly when parent is
// already at or below child: walking up from parent reaches child. Reading the
// edges as a child -> parent map keeps that walk to one step per ancestor.
func rejectParentCycle(ctx context.Context, tx *sql.Tx, childID, parentID string) error {
	if childID == parentID {
		return model.ValidationError{Message: fmt.Sprintf("parent-child: %s cannot be its own parent", childID)}
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
				return model.ValidationError{Message: fmt.Sprintf("parent-child: cannot make %s a child of %s — %s is already below %s in the hierarchy, so this edge would close a parent cycle, which has no root", childID, parentID, parentID, childID)}
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
	if cycle := storage.ParentCycle(above); len(cycle) > 0 {
		return fmt.Errorf("parent-child: cannot make %s a child of %s — the hierarchy above %s already holds a cycle (%s); %s, then retry", childID, parentID, parentID, strings.Join(cycle, " -> "), storage.ParentCycleRepair)
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
// other walk still meets a loop. Reading every stored edge makes this a
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
	edges, err := queryParentEdges(ctx, q, "TRUE")
	if err != nil {
		return nil, err
	}
	parentOf := make(map[string][]string, len(edges))
	for _, edge := range edges {
		parentOf[edge.child] = append(parentOf[edge.child], edge.parent)
	}
	return parentOf, nil
}

// epicParentPredicate keeps the parent edges whose parent is an epic.
var epicParentPredicate = fmt.Sprintf("p.issue_type IN (%s)", quotedIssueTypeList(model.ContainerTypes()))

// loadEpicTree is loadParentEdges narrowed to edges whose parent is an epic,
// with each child's type: the containment an epic's blockers reach down
// through, and what storage.RejectBlocksWithinEpic reads.
func loadEpicTree(ctx context.Context, q rowQueryer) (storage.EpicTree, error) {
	edges, err := queryParentEdges(ctx, q, epicParentPredicate)
	if err != nil {
		return nil, err
	}
	tree := make(storage.EpicTree, len(edges))
	for _, edge := range edges {
		tree.Place(edge.child, edge.childType, edge.parent)
	}
	return tree, nil
}

// parentEdge is one stored parent-child row, with its child's type.
type parentEdge struct {
	child, parent string
	childType     model.IssueType
}

// queryParentEdges reads the parent edges parentFilter keeps; `p` is the
// parent's issues row. [LAW:one-source-of-truth] One query body, so the full
// and epic-only readings cannot drift apart.
func queryParentEdges(ctx context.Context, q rowQueryer, parentFilter string) ([]parentEdge, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.src_id, i.issue_type, r.dst_id FROM relations r
		JOIN issues i ON i.id = r.src_id
		JOIN issues p ON p.id = r.dst_id
		WHERE r.type = 'parent-child' AND `+parentFilter+`
		ORDER BY r.src_id, r.dst_id`)
	if err != nil {
		return nil, fmt.Errorf("query parent edges: %w", err)
	}
	defer rows.Close()
	var edges []parentEdge
	for rows.Next() {
		var edge parentEdge
		if err := rows.Scan(&edge.child, &edge.childType, &edge.parent); err != nil {
			return nil, fmt.Errorf("scan parent edge: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parent edge rows: %w", err)
	}
	return edges, nil
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
		// Removing a parent-child edge is a detach whatever the caller, so it
		// meets ClearParent's rule: a leaf keeps a parent. Judged after
		// the delete, against the parent edges left, so a missing edge is
		// still reported as missing; the refusal rolls the delete back.
		// [LAW:single-enforcer]
		return storage.RejectDetachingParentEdge(relType, srcID, func() (model.IssueType, int, error) {
			childType, err := issueTypeTx(ctx, tx, srcID)
			if err != nil {
				return "", 0, err
			}
			var parentsLeft int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations WHERE src_id = ? AND type = 'parent-child'`, srcID).Scan(&parentsLeft); err != nil {
				return "", 0, fmt.Errorf("count parent edges of %s: %w", srcID, err)
			}
			return childType, parentsLeft, nil
		})
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
		return model.Relation{}, model.ValidationError{Message: "child and parent ids are required"}
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, model.ValidationError{Message: "child and parent cannot be the same issue"}
	}
	rel := model.Relation{
		SrcID:     in.ChildID,
		DstID:     in.ParentID,
		Type:      model.RelParentChild,
		CreatedAt: s.clock.Now(),
		CreatedBy: storage.Actor(in.CreatedBy),
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

// ClearParent detaches a child from its parent. Only a container can be
// detached: a leaf lives inside an epic (storage.RejectDetachingALeaf), so
// the one thing this verb does is lift a nested epic to the top level.
//
// It hydrates nothing before the DELETE. That is what makes it usable on the
// one workspace that needs it most: a hierarchy holding a loop, which `lit
// doctor` names and tells the operator to break here. Hydrating the child
// first — GetIssue derives an epic's state from its children — would refuse
// exactly that state (storage.ParentCycleError), so the repair would fail on
// the fault it is the repair for. The child's type is read as a column on the tx, which also proves it
// exists, because the two absences are different diagnoses: "no such issue"
// and "that issue has no parent" send the operator to different places, and
// collapsing them into the DELETE's rows-affected would report a typo'd id as
// a missing edge. [LAW:no-silent-failure]
func (s *Store) ClearParent(ctx context.Context, childID string) error {
	return s.withMutation(ctx, "clear parent", func(ctx context.Context, tx *sql.Tx) error {
		return clearParentTx(ctx, tx, childID)
	})
}

// clearParentTx is `lit parent clear` on tx: it refuses a leaf, which lives
// inside an epic, and removes every parent edge of childID. A child with no
// parent is the state asked for, answered with storage.RelationAbsentError.
// The type read also proves the child exists, so an unknown id is reported as
// a missing issue, not as a parent already absent. [LAW:single-enforcer]
func clearParentTx(ctx context.Context, tx *sql.Tx, childID string) error {
	childType, err := issueTypeTx(ctx, tx, childID)
	if err != nil {
		return err
	}
	if err := storage.RejectDetachingALeaf(childID, childType, 0); err != nil {
		return err
	}
	removed, err := detachParentsTx(ctx, tx, childID)
	if err != nil {
		return err
	}
	if removed == 0 {
		return storage.RelationAbsentError{SrcID: childID, Type: model.RelParentChild}
	}
	return nil
}

// detachParentsTx removes every parent edge of childID on tx, judging nothing:
// it is the raw half of a clear.
func detachParentsTx(ctx context.Context, tx *sql.Tx, childID string) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, childID)
	if err != nil {
		return 0, fmt.Errorf("delete parent relation: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return affected, nil
}
