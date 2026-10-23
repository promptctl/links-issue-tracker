package memory

import (
	"context"
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
	body, err := in.Body.Text()
	if err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	issue, err := e.getIssue(in.IssueID)
	if err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	comment := model.Comment{
		ID:        "cmt-" + uuid.NewString(),
		IssueID:   in.IssueID,
		Body:      body,
		CreatedAt: e.clock.Now(),
		CreatedBy: storage.Actor(in.CreatedBy),
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
		return model.Comment{}, model.ValidationError{Message: "comment id is required"}
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
		rows = append(rows, model.Label{IssueID: in.IssueID, Name: name, CreatedAt: e.clock.Now(), CreatedBy: storage.Actor(in.CreatedBy)})
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
	e.setLabels(issueID, canonical, e.clock.Now(), createdBy)
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
	author := storage.Actor(createdBy)
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
	return model.CanonicalizeLabels(labels)
}

// --- relations ------------------------------------------------------------

func (e *Engine) AddRelation(ctx context.Context, in storage.AddRelationInput) (model.Relation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.addRelation(in)
}

func (e *Engine) addRelation(in storage.AddRelationInput) (model.Relation, error) {
	if in.Type == model.RelRelatedTo && in.SrcID == in.DstID {
		return model.Relation{}, model.ValidationError{Message: "related-to cannot target itself"}
	}
	srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)
	if _, err := e.mustRecord(srcID); err != nil {
		return model.Relation{}, err
	}
	dst, err := e.mustRecord(dstID)
	if err != nil {
		return model.Relation{}, err
	}
	// A held state is answered before any rule that judges a write, as the
	// Dolt engine's rejectHeldRelationTx is: it changes nothing, so no write
	// rule has anything to judge. [LAW:one-source-of-truth]
	if e.relationHeld(srcID, dstID, in.Type) {
		return model.Relation{}, storage.RelationExistsError{SrcID: srcID, DstID: dstID, Type: in.Type}
	}
	if err := storage.RejectNonContainerParentEdge(in.Type, dstID, dst.issueType); err != nil {
		return model.Relation{}, err
	}
	if err := e.rejectCycle(in.Type, srcID, dstID); err != nil {
		return model.Relation{}, err
	}
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: in.Type, CreatedAt: e.clock.Now(), CreatedBy: storage.Actor(in.CreatedBy)}
	// A single-valued type's src holds at most one such edge, so writing one
	// replaces any it already had. The cardinality is read off the type, not
	// off which method the caller reached for. [LAW:dataflow-not-control-flow]
	if in.Type.SingleValuedFromSrc() {
		e.dropRelations(func(existing model.Relation) bool {
			return existing.SrcID == rel.SrcID && existing.Type == rel.Type
		})
	}
	e.relations = append(e.relations, rel)
	return rel, nil
}

// rejectCycle refuses an edge whose type may not close a loop. Two types carry
// an acyclicity rule, over different graphs and for different reasons, and
// related-to carries none — which it states by having no arm rather than by a
// guard somewhere deciding it is exempt. The same two arms live in the Dolt
// store's rejectCycleTx; this is one rule rendered twice, not two.
// [LAW:one-source-of-truth] [LAW:dataflow-not-control-flow]
func (e *Engine) rejectCycle(relType model.RelationType, srcID, dstID string) error {
	switch relType {
	case model.RelBlockedBy:
		// A rank order is a total order, and one honoring every blocked-by edge
		// exists exactly when there is no cycle, so a cycle is an unsatisfiable
		// constraint set rather than an awkward shape. Rejecting the
		// cycle-closing edge at this write boundary is what makes the state
		// unrepresentable rather than something a later repair pass notices.
		// The same-epic rule is judged first, as the import gate judges it: an
		// edge inside one epic that also closes a cycle is refused by the rule
		// that names the fix. [LAW:types-are-the-program]
		if err := storage.RejectBlocksWithinEpic(e.epicTree(), srcID, dstID); err != nil {
			return err
		}
		return e.rejectBlocksCycle(srcID, dstID)
	case model.RelParentChild:
		return e.rejectParentCycle(srcID, dstID)
	}
	return nil
}

// epicTree places each child under its parents that are epics — the Dolt
// engine's loadEpicTree, rendered over this engine's slices. An edge with
// either endpoint missing its record is dropped, as that engine's joins on
// issues drop it.
func (e *Engine) epicTree() storage.EpicTree {
	tree := storage.EpicTree{}
	for _, rel := range e.relations {
		child, childOK := e.issues[rel.SrcID]
		parent, parentOK := e.issues[rel.DstID]
		if childOK && parentOK && rel.Type == model.RelParentChild && parent.issueType.IsContainer() {
			tree.Place(rel.SrcID, child.issueType, rel.DstID)
		}
	}
	return tree
}

// rejectParentCycle refuses an edge that would close a loop in the hierarchy.
//
// A hierarchy is a tree: every issue reaches a root by walking up, and every
// consumer that derives container state or resolves a top-level ancestor is
// built on that walk terminating. A cycle has no root, so such a walk does not
// return a wrong answer — it does not return. [LAW:types-are-the-program]
//
// The edge runs child -> parent, so the loop closes exactly when the parent is
// already at or below the child.
func (e *Engine) rejectParentCycle(childID, parentID string) error {
	if childID == parentID {
		return model.ValidationError{Message: fmt.Sprintf("parent-child: %s cannot be its own parent", childID)}
	}
	parentOf := map[string]string{}
	for _, rel := range e.relations {
		if rel.Type == model.RelParentChild {
			parentOf[rel.SrcID] = rel.DstID
		}
	}
	// seen bounds the walk on data that already holds a cycle, so a store
	// written before this rule existed reports it rather than hanging here.
	seen := map[string]struct{}{parentID: {}}
	for at := parentOf[parentID]; at != ""; at = parentOf[at] {
		if at == childID {
			return model.ValidationError{Message: fmt.Sprintf("parent-child: cannot make %s a child of %s — %s is already below %s in the hierarchy, so this edge would close a parent cycle, which has no root", childID, parentID, parentID, childID)}
		}
		if _, visited := seen[at]; visited {
			return fmt.Errorf("parent-child: cannot make %s a child of %s — the hierarchy above %s already holds a cycle; run 'lit doctor' to find it", childID, parentID, parentID)
		}
		seen[at] = struct{}{}
	}
	return nil
}

// rejectBlocksCycle refuses an edge that would close a cycle in the precedence
// graph. A self-edge is the degenerate one; a longer one exists when the new
// dependent already precedes the new dependency, since the new edge asserts
// the reverse.
func (e *Engine) rejectBlocksCycle(dependent, dependency string) error {
	if dependent == dependency {
		return model.ValidationError{Message: fmt.Sprintf("blocked-by: %s cannot be blocked by itself", dependent)}
	}
	// dependency -> dependents: who is forced to come after whom.
	precedes := map[string][]string{}
	for _, rel := range e.relations {
		if rel.Type != model.RelBlockedBy {
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
		return model.ValidationError{Message: fmt.Sprintf("blocked-by: cannot add %s blocked by %s — %s is already blocked by %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order", dependent, dependency, dependency, dependent)}
	}
	return nil
}

func (e *Engine) RemoveRelation(ctx context.Context, srcID, dstID string, relType model.RelationType) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	srcID, dstID = relType.CanonicalEndpoints(srcID, dstID)
	if !e.hasRelation(srcID, dstID, relType) {
		return storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}
	}
	// Removing a parent-child edge is a detach whatever the caller, so it
	// meets ClearParent's rule: a leaf keeps a parent, judged against the
	// parent edges left once this one goes. The edge exists, so its src does.
	// [LAW:single-enforcer]
	if err := storage.RejectDetachingParentEdge(relType, srcID, func() (model.IssueType, int, error) {
		child, err := e.mustRecord(srcID)
		if err != nil {
			return "", 0, err
		}
		return child.issueType, e.parentsLeftWithout(srcID, dstID), nil
	}); err != nil {
		return err
	}
	e.dropRelations(func(rel model.Relation) bool {
		return rel.SrcID == srcID && rel.DstID == dstID && rel.Type == relType
	})
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
	ancestry := e.rankAncestry(pos)
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
		bucketed, err := e.bucketRelations(id, e.incidentRelations(id), pos, ancestry)
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
		return model.Relation{}, model.ValidationError{Message: "child and parent ids are required"}
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, model.ValidationError{Message: "child and parent cannot be the same issue"}
	}
	// SetParent is one validated caller of the single-valued write, not a
	// second copy of the cardinality rule: reparenting replaces in one act
	// because that is what "at most one parent" means. [LAW:single-enforcer]
	return e.addRelation(storage.AddRelationInput{
		SrcID: in.ChildID, DstID: in.ParentID, Type: model.RelParentChild, CreatedBy: in.CreatedBy,
	})
}

// ClearParent detaches a child from its parent. Only a container can be
// detached: a leaf lives inside an epic (storage.RejectDetachingALeaf), so
// the one thing this verb does is lift a nested epic to the top level.
// Clearing a child that has no parent is the state asked for, answered with
// storage.RelationAbsentError, which is satisfied rather than a removal it
// did not make. [LAW:no-silent-failure]
func (e *Engine) ClearParent(ctx context.Context, childID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.clearParent(childID)
}

// clearParent is `lit parent clear`: it refuses a leaf and removes every
// parent edge of childID, answering a child with no parent with
// storage.RelationAbsentError — the Dolt engine's clearParentTx, rendered over
// this engine's slices. The record is read for its type alone, so this
// answers on a hierarchy holding a loop. [LAW:one-source-of-truth]
func (e *Engine) clearParent(childID string) error {
	child, err := e.mustRecord(childID)
	if err != nil {
		return err
	}
	if err := storage.RejectDetachingALeaf(childID, child.issueType, 0); err != nil {
		return err
	}
	if e.detachParents(childID) == 0 {
		return storage.RelationAbsentError{SrcID: childID, Type: model.RelParentChild}
	}
	return nil
}

// detachParents removes every parent edge of childID, judging nothing: the
// raw half of a clear, and a bulk reparent's detach before the attach that
// judges the child's new place.
func (e *Engine) detachParents(childID string) int {
	return e.dropRelations(func(rel model.Relation) bool {
		return rel.SrcID == childID && rel.Type == model.RelParentChild
	})
}

// DependencyOrder reads the blocked-by edges between live issues in tree order:
// live as the store's open and in-progress listing classifies it, the same
// population the SQL engine reads.
func (e *Engine) DependencyOrder(ctx context.Context) (storage.DependencyOrder, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	listed, err := e.listIssues(storage.ListIssuesFilter{Statuses: []model.State{model.StateOpen, model.StateInProgress}})
	if err != nil {
		return storage.DependencyOrder{}, err
	}
	live := make(map[string]struct{}, len(listed))
	for _, issue := range listed {
		live[issue.ID] = struct{}{}
	}
	var edges []storage.DependencyEdge
	for _, rel := range e.relations {
		_, srcLive := live[rel.SrcID]
		_, dstLive := live[rel.DstID]
		if rel.Type == model.RelBlockedBy && srcLive && dstLive {
			edges = append(edges, storage.DependencyEdge{Dependent: rel.SrcID, Dependency: rel.DstID})
		}
	}
	return storage.NewDependencyOrder(e.rankAncestry(e.positions()), edges), nil
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

// relationHeld reports whether the state an add of src->dst asks for already
// holds: the edges in its write's scope are exactly that one. The type's
// cardinality sets the scope, every edge of the type from src for a
// single-valued type and the identical edge otherwise, as the Dolt engine's
// rejectHeldRelationTx reads it. [LAW:dataflow-not-control-flow]
func (e *Engine) relationHeld(srcID, dstID string, relType model.RelationType) bool {
	var inScope []string
	for _, rel := range e.relations {
		if rel.SrcID == srcID && rel.Type == relType && (relType.SingleValuedFromSrc() || rel.DstID == dstID) {
			inScope = append(inScope, rel.DstID)
		}
	}
	return slices.Equal(inScope, []string{dstID})
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
	var dropped int
	e.relations, dropped = removeWhere(e.relations, matches)
	return dropped
}

// parentsLeftWithout counts childID's parent-child edges other than the one to
// parentID: the parents it keeps once that edge is gone, which a detach and a
// purge of parentID both judge it by.
func (e *Engine) parentsLeftWithout(childID, parentID string) int {
	count := 0
	for _, rel := range e.relations {
		if rel.SrcID == childID && rel.Type == model.RelParentChild && rel.DstID != parentID {
			count++
		}
	}
	return count
}

// bucketRelations sorts the edges incident to focalID into the four structural
// slices, hydrating counterparts. It is the single definition of how an edge
// maps to parent / child / blocked-by / blocks — so the direction convention
// (a blocked-by edge reads "src blocked-by dst", which makes BlockedBy and
// Blocks the two readings of one edge set) lives in exactly one place.
// [LAW:single-enforcer]
func (e *Engine) bucketRelations(focalID string, relations []model.Relation, pos map[string]int, ancestry storage.RankAncestry) (storage.IssueRelations, error) {
	out := storage.IssueRelations{Children: []model.Issue{}, BlockedBy: []model.Issue{}, Blocks: []model.Issue{}}
	for _, rel := range relations {
		// Which bucket an edge lands in is a function of its type and which
		// end the focal issue is on — the four readings of two edge types,
		// enumerated once. An edge whose counterpart has vanished is simply
		// not in the result; callers iterating known-present ids never see
		// the hole.
		bucket, otherID := (*[]model.Issue)(nil), ""
		switch {
		case rel.Type == model.RelBlockedBy && rel.SrcID == focalID:
			bucket, otherID = &out.BlockedBy, rel.DstID
		case rel.Type == model.RelBlockedBy && rel.DstID == focalID:
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
	ancestry.Sort(out.Children)
	ancestry.Sort(out.BlockedBy)
	ancestry.Sort(out.Blocks)
	return out, nil
}

// relatedIssues returns the hydrated related-to counterparts of focalID. It is
// GetIssueDetail's concern alone — no batch consumer wants peer links, which
// is why they stay out of the shared IssueRelations shape.
func (e *Engine) relatedIssues(focalID string, relations []model.Relation, pos map[string]int, ancestry storage.RankAncestry) ([]model.Issue, error) {
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
	ancestry.Sort(out)
	return out, nil
}
