package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// IssueRelations is one issue together with its structural graph edges —
// parent, children, dependencies (DependsOn), and dependents (Blocks) — each
// hydrated, but WITHOUT the comment/event/related payload GetIssueDetail also
// loads. It is the shared lightweight per-issue shape batch consumers read, so
// neither the ready pipeline nor the epic view pays GetIssueDetail's per-row
// comment/event cost.
// [LAW:one-source-of-truth] One shape for "an issue's open blockers / parent
// epic" across consumers; a second batch type would let them drift.
type IssueRelations struct {
	Issue     model.Issue
	Parent    *model.Issue
	Children  []model.Issue
	DependsOn []model.Issue
	Blocks    []model.Issue
}

// bucketRelations sorts the structural edges incident to focalID into the four
// relation slices, hydrating counterparts from issuesByID. It is the single
// definition of how a relation row maps to parent / child / depends-on / blocks
// — shared by single-issue detail loading and batch relation loading so the
// "blocks convention: src=dependent, dst=dependency" lives in exactly one place.
// [LAW:single-enforcer] Relation-direction semantics decided once, here.
func bucketRelations(focalID string, relations []model.Relation, issuesByID map[string]model.Issue) IssueRelations {
	out := IssueRelations{
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

// GetRelationsByIDs batch-loads the structural relations for every listed id in
// a fixed number of queries rather than GetIssueDetail-per-id. Subjects that no
// longer exist are simply absent from the result, mirroring getIssuesByIDs;
// callers iterating known-present ids never observe the hole.
// [LAW:dataflow-not-control-flow] One relations query plus one issue-hydration
// query feed a pure bucketing pass — the per-subject work is map lookups, not
// extra round-trips.
func (s *Store) GetRelationsByIDs(ctx context.Context, ids []string) (map[string]IssueRelations, error) {
	subjects := dedupeStrings(ids)
	if len(subjects) == 0 {
		return map[string]IssueRelations{}, nil
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
	out := make(map[string]IssueRelations, len(subjects))
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
// the given ids in one query — the batch counterpart of listRelations, scoped to
// the edge types GetRelationsByIDs serves.
func (s *Store) listRelationsForIDs(ctx context.Context, ids []string) ([]model.Relation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idClause := strings.Join(repeatPlaceholder(len(ids)), ",")
	typeClause := strings.Join(repeatPlaceholder(len(structuralRelationTypes)), ",")
	args := make([]any, 0, len(ids)*2+len(structuralRelationTypes))
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	for _, relType := range structuralRelationTypes {
		args = append(args, string(relType))
	}
	query := fmt.Sprintf(`SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE (src_id IN (%s) OR dst_id IN (%s)) AND type IN (%s) ORDER BY created_at ASC`, idClause, idClause, typeClause)
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

type AddRelationInput struct {
	SrcID     string
	DstID     string
	Type      model.RelationType
	CreatedBy string
}

type SetParentInput struct {
	ChildID   string
	ParentID  string
	CreatedBy string
}

func (s *Store) AddRelation(ctx context.Context, in AddRelationInput) (model.Relation, error) {
	if _, err := s.GetIssue(ctx, in.SrcID); err != nil {
		return model.Relation{}, err
	}
	if _, err := s.GetIssue(ctx, in.DstID); err != nil {
		return model.Relation{}, err
	}
	// [LAW:types-are-the-program] in.Type is sealed at the trust boundary by
	// ParseRelationType; no string re-validation here.
	if in.Type == model.RelRelatedTo && in.SrcID == in.DstID {
		return model.Relation{}, errors.New("related-to cannot target itself")
	}
	srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)
	now := time.Now().UTC()
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: in.Type, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	if err := s.withMutation(ctx, "add relation", func(ctx context.Context, tx *sql.Tx) error {
		// [LAW:types-are-the-program] The blocks subgraph must stay acyclic: a
		// rank order is a total order, and one that honors every blocks edge
		// exists iff there is no cycle. Rejecting a cycle-closing edge at this
		// single write boundary makes the unsatisfiable state unrepresentable,
		// so neither Doctor nor FixRankInversions has to compensate for it.
		// [LAW:single-enforcer] AddRelation is the only interactive creator of
		// blocks edges; bulk import is a trust boundary that Doctor re-checks.
		if rel.Type == model.RelBlocks {
			if err := rejectBlocksCycle(ctx, tx, rel.SrcID, rel.DstID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
			return fmt.Errorf("insert relation: %w", err)
		}
		return nil
	}); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
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

func (s *Store) RemoveRelation(ctx context.Context, srcID, dstID string, relType model.RelationType) error {
	srcID, dstID = relType.CanonicalEndpoints(srcID, dstID)
	return s.withMutation(ctx, "remove relation", func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?`, srcID, dstID, string(relType))
		if err != nil {
			return fmt.Errorf("delete relation: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected: %w", err)
		}
		if affected == 0 {
			return NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}
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

func (s *Store) SetParent(ctx context.Context, in SetParentInput) (model.Relation, error) {
	if strings.TrimSpace(in.ChildID) == "" || strings.TrimSpace(in.ParentID) == "" {
		return model.Relation{}, errors.New("child and parent ids are required")
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, errors.New("child and parent cannot be the same issue")
	}
	if _, err := s.GetIssue(ctx, in.ChildID); err != nil {
		return model.Relation{}, err
	}
	if _, err := s.GetIssue(ctx, in.ParentID); err != nil {
		return model.Relation{}, err
	}
	rel := model.Relation{
		SrcID:     in.ChildID,
		DstID:     in.ParentID,
		Type:      model.RelParentChild,
		CreatedAt: time.Now().UTC(),
		CreatedBy: strings.TrimSpace(in.CreatedBy),
	}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	if err := s.withMutation(ctx, "set parent", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, in.ChildID); err != nil {
			return fmt.Errorf("clear parent relation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, ?)`, rel.SrcID, rel.DstID, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
			return fmt.Errorf("insert parent relation: %w", err)
		}
		return nil
	}); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

func (s *Store) ClearParent(ctx context.Context, childID string) error {
	if _, err := s.GetIssue(ctx, childID); err != nil {
		return err
	}
	return s.withMutation(ctx, "clear parent", func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, childID)
		if err != nil {
			return fmt.Errorf("delete parent relation: %w", err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return NotFoundError{Entity: "parent relation", ID: childID}
		}
		return nil
	})
}

func (s *Store) ListChildren(ctx context.Context, parentID string) ([]model.Issue, error) {
	if _, err := s.GetIssue(ctx, parentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.title, i.description, i.agent_prompt, i.status, i.priority, i.issue_type, i.topic, i.assignee, i.item_rank, i.lane, i.created_at, i.updated_at, i.closed_at, i.resolution, i.archived_at, i.deleted_at
		FROM relations r
		JOIN issues i ON i.id = r.src_id
		WHERE r.type = 'parent-child' AND r.dst_id = ?
		ORDER BY i.item_rank ASC, i.id ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()
	children := []issueRow{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.hydrateIssues(ctx, children)
}
