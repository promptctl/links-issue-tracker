package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// --- comments -------------------------------------------------------------

// AddComment returns the commented-on issue alongside the comment. It already
// read the issue to prove it exists and a comment never changes the issue row,
// so that read is the caller's answer too — never a second round-trip for what
// this call already holds. [LAW:one-source-of-truth]
func (e *Engine) AddComment(ctx context.Context, in storage.AddCommentInput) (model.Comment, model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	issue, err := e.getIssue(in.IssueID)
	if err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return model.Comment{}, model.Issue{}, errors.New("comment body is required")
	}
	comment := model.Comment{
		ID:        "cmt-" + uuid.NewString(),
		IssueID:   in.IssueID,
		Body:      body,
		CreatedAt: e.now(),
		CreatedBy: authorOr(in.CreatedBy),
	}
	e.comments = append(e.comments, comment)
	return comment, issue, nil
}

// DeleteComment reports what it removed, so a deletion is describable without
// a prior read.
func (e *Engine) DeleteComment(ctx context.Context, commentID string) (model.Comment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	id := strings.TrimSpace(commentID)
	if id == "" {
		return model.Comment{}, errors.New("comment id is required")
	}
	index := slices.IndexFunc(e.comments, func(c model.Comment) bool { return c.ID == id })
	if index < 0 {
		return model.Comment{}, storage.NotFoundError{Entity: "comment", ID: id}
	}
	deleted := e.comments[index]
	e.comments = slices.Delete(e.comments, index, index+1)
	return deleted, nil
}

func (e *Engine) commentsFor(issueID string) []model.Comment {
	out := []model.Comment{}
	for _, comment := range e.comments {
		if comment.IssueID == issueID {
			out = append(out, comment)
		}
	}
	return out
}

// --- labels ---------------------------------------------------------------

// AddLabel asks for a label to be present and reports the resulting set. Adding
// one twice is that same end state rather than an error: the caller asked for
// the label to be there, and it is — and the row keeps the authorship the
// first add gave it.
func (e *Engine) AddLabel(ctx context.Context, in storage.AddLabelInput) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.mustRecord(in.IssueID); err != nil {
		return nil, err
	}
	name, err := model.NormalizeLabel(in.Name)
	if err != nil {
		return nil, err
	}
	rows := e.labels[in.IssueID]
	if !slices.ContainsFunc(rows, func(l model.Label) bool { return l.Name == name }) {
		rows = append(rows, model.Label{IssueID: in.IssueID, Name: name, CreatedAt: e.now(), CreatedBy: authorOr(in.CreatedBy)})
		slices.SortFunc(rows, func(a, b model.Label) int { return strings.Compare(a.Name, b.Name) })
		e.labels[in.IssueID] = rows
	}
	return e.labelNames(in.IssueID), nil
}

// RemoveLabel drops one label and reports the resulting set. Removing what was
// never there is not a silent success — nothing was removed, and saying
// otherwise would be an answer-shaped void. [LAW:no-silent-failure]
func (e *Engine) RemoveLabel(ctx context.Context, issueID, labelName string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.mustRecord(issueID); err != nil {
		return nil, err
	}
	name, err := model.NormalizeLabel(labelName)
	if err != nil {
		return nil, err
	}
	rows := e.labels[issueID]
	index := slices.IndexFunc(rows, func(l model.Label) bool { return l.Name == name })
	if index < 0 {
		return nil, storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, name)}
	}
	e.labels[issueID] = slices.Delete(rows, index, index+1)
	return e.labelNames(issueID), nil
}

// ReplaceLabels states the whole set at once — the authored-document path,
// where the file says what labels an issue has rather than a delta.
func (e *Engine) ReplaceLabels(ctx context.Context, issueID string, labels []string, createdBy string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.mustRecord(issueID); err != nil {
		return err
	}
	canonical, err := canonicalLabels(labels)
	if err != nil {
		return err
	}
	e.setLabels(issueID, canonical, e.now(), createdBy)
	return nil
}

func (e *Engine) ListLabels(ctx context.Context, issueID string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.labelNames(issueID), nil
}

// setLabels writes a canonical label set, replacing whatever was there. It is
// the one label write, shared by the create path and the replace path, so the
// stored rows can only ever be the canonical form. [LAW:single-enforcer]
func (e *Engine) setLabels(issueID string, names []string, now time.Time, createdBy string) {
	author := authorOr(createdBy)
	rows := make([]model.Label, 0, len(names))
	for _, name := range names {
		rows = append(rows, model.Label{IssueID: issueID, Name: name, CreatedAt: now, CreatedBy: author})
	}
	e.labels[issueID] = rows
}

// canonicalLabels reduces an authored list to the stored form: each name
// normalized, duplicates collapsed, the set sorted by name. It is a parser —
// what comes back could not have existed before it ran, so nothing downstream
// re-normalizes. [LAW:parse-dont-validate]
func canonicalLabels(labels []string) ([]string, error) {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		name, err := model.NormalizeLabel(label)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

// --- relations ------------------------------------------------------------

func (e *Engine) AddRelation(ctx context.Context, in storage.AddRelationInput) (model.Relation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.addRelation(in)
}

func (e *Engine) addRelation(in storage.AddRelationInput) (model.Relation, error) {
	if in.Type == model.RelRelatedTo && in.SrcID == in.DstID {
		return model.Relation{}, errors.New("related-to cannot target itself")
	}
	srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)
	if _, err := e.mustRecord(srcID); err != nil {
		return model.Relation{}, err
	}
	if _, err := e.mustRecord(dstID); err != nil {
		return model.Relation{}, err
	}
	// The blocks subgraph must stay acyclic: a rank order is a total order,
	// and one honoring every blocks edge exists exactly when there is no
	// cycle. Rejecting the cycle-closing edge at this write boundary is what
	// makes the unsatisfiable state unrepresentable rather than something a
	// later repair pass has to notice. [LAW:types-are-the-program]
	if in.Type == model.RelBlocks {
		if err := e.rejectBlocksCycle(srcID, dstID); err != nil {
			return model.Relation{}, err
		}
	}
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: in.Type, CreatedAt: e.now(), CreatedBy: authorOr(in.CreatedBy)}
	// A single-valued type's src holds at most one such edge, so writing one
	// replaces any it already had. The cardinality is read off the type, not
	// off which method the caller reached for. [LAW:dataflow-not-control-flow]
	if in.Type.SingleValuedFromSrc() {
		e.dropRelations(func(existing model.Relation) bool {
			return existing.SrcID == rel.SrcID && existing.Type == rel.Type
		})
	}
	if e.hasRelation(rel.SrcID, rel.DstID, rel.Type) {
		return model.Relation{}, fmt.Errorf("relation %s->%s (%s) already exists", rel.SrcID, rel.DstID, rel.Type)
	}
	e.relations = append(e.relations, rel)
	return rel, nil
}

// rejectBlocksCycle refuses an edge that would close a cycle in the precedence
// graph. A self-edge is the degenerate one; a longer one exists when the new
// dependent already precedes the new dependency, since the new edge asserts
// the reverse.
func (e *Engine) rejectBlocksCycle(dependent, dependency string) error {
	if dependent == dependency {
		return fmt.Errorf("blocks: %s cannot block itself", dependent)
	}
	// dependency -> dependents: who is forced to come after whom.
	precedes := map[string][]string{}
	for _, rel := range e.relations {
		if rel.Type != model.RelBlocks {
			continue
		}
		precedes[rel.DstID] = append(precedes[rel.DstID], rel.SrcID)
	}
	seen := map[string]struct{}{}
	var reaches func(from string) bool
	reaches = func(from string) bool {
		for _, next := range precedes[from] {
			if next == dependency {
				return true
			}
			if _, visited := seen[next]; visited {
				continue
			}
			seen[next] = struct{}{}
			if reaches(next) {
				return true
			}
		}
		return false
	}
	if reaches(dependent) {
		return fmt.Errorf("blocks: cannot add %s depends-on %s — %s already depends on %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order", dependent, dependency, dependency, dependent)
	}
	return nil
}

func (e *Engine) RemoveRelation(ctx context.Context, srcID, dstID string, relType model.RelationType) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	srcID, dstID = relType.CanonicalEndpoints(srcID, dstID)
	removed := e.dropRelations(func(rel model.Relation) bool {
		return rel.SrcID == srcID && rel.DstID == dstID && rel.Type == relType
	})
	if removed == 0 {
		return storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}
	}
	return nil
}

// ListRelationsForIssue returns every edge incident to the issue in either
// direction, oldest first. The variadic types argument narrows rather than
// switching behavior: naming no type means every type, never no types.
// [LAW:dataflow-not-control-flow]
func (e *Engine) ListRelationsForIssue(ctx context.Context, issueID string, types ...model.RelationType) ([]model.Relation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.mustRecord(issueID); err != nil {
		return nil, err
	}
	out := []model.Relation{}
	for _, rel := range e.incidentRelations(issueID) {
		if len(types) == 0 || slices.Contains(types, rel.Type) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// GetRelationsByIDs is the batch neighborhood read: one call for many issues,
// counterparts hydrated, without the per-issue comment and history cost
// GetIssueDetail pays. An id nobody could find is simply absent from the map.
func (e *Engine) GetRelationsByIDs(ctx context.Context, ids []string) (map[string]storage.IssueRelations, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pos := e.positions()
	out := map[string]storage.IssueRelations{}
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		rec, ok := e.issues[id]
		if !ok {
			continue
		}
		issue, err := e.hydrate(rec, pos)
		if err != nil {
			return nil, err
		}
		bucketed, err := e.bucketRelations(id, e.incidentRelations(id), pos)
		if err != nil {
			return nil, err
		}
		bucketed.Issue = issue
		out[id] = bucketed
	}
	return out, nil
}

func (e *Engine) SetParent(ctx context.Context, in storage.SetParentInput) (model.Relation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if strings.TrimSpace(in.ChildID) == "" || strings.TrimSpace(in.ParentID) == "" {
		return model.Relation{}, errors.New("child and parent ids are required")
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, errors.New("child and parent cannot be the same issue")
	}
	// SetParent is one validated caller of the single-valued write, not a
	// second copy of the cardinality rule: reparenting replaces in one act
	// because that is what "at most one parent" means. [LAW:single-enforcer]
	return e.addRelation(storage.AddRelationInput{
		SrcID: in.ChildID, DstID: in.ParentID, Type: model.RelParentChild, CreatedBy: in.CreatedBy,
	})
}

// ClearParent detaches a child from its parent. Clearing a child that has no
// parent is an absence report, not a silent success: the caller asked to
// remove an edge, and reporting removal of one that was never there would be
// an answer-shaped void. [LAW:no-silent-failure]
func (e *Engine) ClearParent(ctx context.Context, childID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.mustRecord(childID); err != nil {
		return err
	}
	removed := e.dropRelations(func(rel model.Relation) bool {
		return rel.SrcID == childID && rel.Type == model.RelParentChild
	})
	if removed == 0 {
		return storage.NotFoundError{Entity: "parent relation", ID: childID}
	}
	return nil
}

// incidentRelations returns every edge touching issueID in either direction,
// in the order the edges were written.
func (e *Engine) incidentRelations(issueID string) []model.Relation {
	out := []model.Relation{}
	for _, rel := range e.relations {
		if rel.SrcID == issueID || rel.DstID == issueID {
			out = append(out, rel)
		}
	}
	return out
}

func (e *Engine) hasRelation(srcID, dstID string, relType model.RelationType) bool {
	return slices.ContainsFunc(e.relations, func(rel model.Relation) bool {
		return rel.SrcID == srcID && rel.DstID == dstID && rel.Type == relType
	})
}

// dropRelations removes every edge matching the predicate and reports how many
// went. The count is what lets a caller tell "removed it" from "there was
// nothing to remove" without reading the table twice.
func (e *Engine) dropRelations(matches func(model.Relation) bool) int {
	before := len(e.relations)
	e.relations = slices.DeleteFunc(e.relations, matches)
	return before - len(e.relations)
}

// bucketRelations sorts the edges incident to focalID into the four structural
// slices, hydrating counterparts. It is the single definition of how an edge
// maps to parent / child / depends-on / blocks — so the direction convention
// (a blocks edge runs src=dependent, dst=dependency, which makes DependsOn and
// Blocks the two readings of one edge set) lives in exactly one place.
// [LAW:single-enforcer]
func (e *Engine) bucketRelations(focalID string, relations []model.Relation, pos map[string]int) (storage.IssueRelations, error) {
	out := storage.IssueRelations{Children: []model.Issue{}, DependsOn: []model.Issue{}, Blocks: []model.Issue{}}
	for _, rel := range relations {
		// Which bucket an edge lands in is a function of its type and which
		// end the focal issue is on — the four readings of two edge types,
		// enumerated once. An edge whose counterpart has vanished is simply
		// not in the result; callers iterating known-present ids never see
		// the hole.
		bucket, otherID := (*[]model.Issue)(nil), ""
		switch {
		case rel.Type == model.RelBlocks && rel.SrcID == focalID:
			bucket, otherID = &out.DependsOn, rel.DstID
		case rel.Type == model.RelBlocks && rel.DstID == focalID:
			bucket, otherID = &out.Blocks, rel.SrcID
		case rel.Type == model.RelParentChild && rel.DstID == focalID:
			bucket, otherID = &out.Children, rel.SrcID
		case rel.Type == model.RelParentChild && rel.SrcID == focalID:
			otherID = rel.DstID
		default:
			continue
		}
		rec, present := e.issues[otherID]
		if !present {
			continue
		}
		issue, err := e.hydrate(rec, pos)
		if err != nil {
			return storage.IssueRelations{}, err
		}
		if bucket == nil {
			out.Parent = &issue
			continue
		}
		*bucket = append(*bucket, issue)
	}
	sortByRank(out.Children, pos)
	sortByRank(out.DependsOn, pos)
	sortByRank(out.Blocks, pos)
	return out, nil
}

// relatedIssues returns the hydrated related-to counterparts of focalID. It is
// GetIssueDetail's concern alone — no batch consumer wants peer links, which
// is why they stay out of the shared IssueRelations shape.
func (e *Engine) relatedIssues(focalID string, relations []model.Relation, pos map[string]int) ([]model.Issue, error) {
	out := []model.Issue{}
	for _, rel := range relations {
		if rel.Type != model.RelRelatedTo {
			continue
		}
		otherID := rel.SrcID
		if otherID == focalID {
			otherID = rel.DstID
		}
		rec, ok := e.issues[otherID]
		if !ok {
			continue
		}
		issue, err := e.hydrate(rec, pos)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	sortByRank(out, pos)
	return out, nil
}

func sortByRank(issues []model.Issue, pos map[string]int) {
	slices.SortStableFunc(issues, func(a, b model.Issue) int { return pos[a.ID] - pos[b.ID] })
}

// authorOr names the writer of a row, falling back to the unknown author when
// the caller named none. Attribution of a row to nobody is a real state — an
// engine cannot invent who did something — and it is spelled once here so
// every row spells it the same way. [LAW:one-source-of-truth]
func authorOr(createdBy string) string {
	if author := strings.TrimSpace(createdBy); author != "" {
		return author
	}
	return "unknown"
}
