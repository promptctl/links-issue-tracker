package merge

import (
	"reflect"
	"sort"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// MergeResult is the whole-export merge: the converged export plus the prose
// fields that diverged on both sides and need the calling agent's semantic
// merge. [LAW:single-enforcer] One merge policy lives in this package — the
// per-row field-aware decisions are ResolveIssue's; ThreeWay only fans it across
// the export and unions the append-only tables.
type MergeResult struct {
	Export  model.Export   `json:"export"`
	Pending []ProsePending `json:"pending"`
}

func ThreeWay(base model.Export, local model.Export, remote model.Export) MergeResult {
	baseMap := mapIssues(base.Issues)
	localMap := mapIssues(local.Issues)
	remoteMap := mapIssues(remote.Issues)

	allIDs := unionIssueIDs(baseMap, localMap, remoteMap)
	mergedIssues := make([]model.Issue, 0, len(allIDs))
	pending := make([]ProsePending, 0)

	for _, id := range allIDs {
		baseIssue, hasBase := baseMap[id]
		localIssue, hasLocal := localMap[id]
		remoteIssue, hasRemote := remoteMap[id]
		basePtr := optionalIssuePtr(baseIssue, hasBase)
		localPtr := optionalIssuePtr(localIssue, hasLocal)
		remotePtr := optionalIssuePtr(remoteIssue, hasRemote)

		localChanged := issueChanged(basePtr, localPtr)
		remoteChanged := issueChanged(basePtr, remotePtr)

		switch {
		case !localChanged && !remoteChanged:
			if hasBase {
				mergedIssues = append(mergedIssues, baseIssue)
			}
		case localChanged && !remoteChanged:
			if hasLocal {
				mergedIssues = append(mergedIssues, localIssue)
			}
		case !localChanged && remoteChanged:
			if hasRemote {
				mergedIssues = append(mergedIssues, remoteIssue)
			}
		default:
			// Both sides changed. A field-aware merge needs both rows present; a
			// missing side here means one machine removed the whole row while the
			// other edited it. (Routine deletion is soft — a DeletedAt stamp on a
			// still-present row — so this whole-row-absence path is reached only by
			// genuine row removal, and presence is a collection fact, not a field.)
			switch {
			case hasLocal && hasRemote:
				resolution := ResolveIssue(basePtr, localPtr, remotePtr, local.WorkspaceID, remote.WorkspaceID)
				mergedIssues = append(mergedIssues, resolution.Merged)
				pending = append(pending, resolution.Pending...)
			case hasLocal:
				// remote removed it, local edited -> preserve the surviving edit.
				mergedIssues = append(mergedIssues, localIssue)
			case hasRemote:
				// local removed it, remote edited -> preserve the surviving edit.
				mergedIssues = append(mergedIssues, remoteIssue)
				// both removed a base-only row -> converged removal, append nothing.
			}
		}
	}

	sort.Slice(mergedIssues, func(i, j int) bool { return mergedIssues[i].ID < mergedIssues[j].ID })
	issueSet := make(map[string]struct{}, len(mergedIssues))
	for _, issue := range mergedIssues {
		issueSet[issue.ID] = struct{}{}
	}

	merged := model.Export{
		Version:     maxInt(local.Version, remote.Version, base.Version),
		WorkspaceID: local.WorkspaceID,
		ExportedAt:  local.ExportedAt,
		Issues:      mergedIssues,
		Relations:   mergeRelations(issueSet, local.Relations, remote.Relations),
		Comments:    mergeComments(issueSet, local.Comments, remote.Comments),
		Labels:      mergeLabels(issueSet, local.Labels, remote.Labels),
		Events:      mergeEvents(issueSet, local.Events, remote.Events),
	}
	return MergeResult{Export: merged, Pending: pending}
}

func mapIssues(issues []model.Issue) map[string]model.Issue {
	out := make(map[string]model.Issue, len(issues))
	for _, issue := range issues {
		out[issue.ID] = issue
	}
	return out
}

func optionalIssuePtr(issue model.Issue, ok bool) *model.Issue {
	if !ok {
		return nil
	}
	copy := issue
	return &copy
}

func unionIssueIDs(maps ...map[string]model.Issue) []string {
	set := map[string]struct{}{}
	for _, mapped := range maps {
		for id := range mapped {
			set[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func issueChanged(base *model.Issue, current *model.Issue) bool {
	return !issueEqual(base, current)
}

func issueEqual(left *model.Issue, right *model.Issue) bool {
	if left == nil && right == nil {
		return true
	}
	if left == nil || right == nil {
		return false
	}
	return reflect.DeepEqual(issueProjectionFrom(*left), issueProjectionFrom(*right))
}

type issueProjection struct {
	ID          string
	Title       string
	Description string
	Status      string
	Priority    int
	IssueType   string
	Topic       string
	Assignee    string
	Rank        string
	Labels      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
	ArchivedAt  *time.Time
	DeletedAt   *time.Time
}

func issueProjectionFrom(issue model.Issue) issueProjection {
	// [LAW:one-source-of-truth] Merge equality compares the same lossless issue data that the sync wire owns, without depending on lifecycle-derived JSON fields.
	return issueProjection{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Status:      issue.StatusValue(),
		Priority:    issue.Priority,
		IssueType:   issue.IssueType,
		Topic:       issue.Topic,
		Assignee:    issue.AssigneeValue(),
		Rank:        issue.Rank,
		Labels:      append([]string{}, issue.Labels...),
		CreatedAt:   issue.CreatedAt,
		UpdatedAt:   issue.UpdatedAt,
		ClosedAt:    issue.ClosedAtValue(),
		ArchivedAt:  issue.ArchivedAt,
		DeletedAt:   issue.DeletedAt,
	}
}

func mergeRelations(issueSet map[string]struct{}, locals, remotes []model.Relation) []model.Relation {
	type key struct {
		Src, Dst string
		Type     model.RelationType
	}
	merged := map[key]model.Relation{}
	for _, relation := range append(locals, remotes...) {
		if _, ok := issueSet[relation.SrcID]; !ok {
			continue
		}
		if _, ok := issueSet[relation.DstID]; !ok {
			continue
		}
		merged[key{Src: relation.SrcID, Dst: relation.DstID, Type: relation.Type}] = relation
	}
	out := make([]model.Relation, 0, len(merged))
	for _, relation := range merged {
		out = append(out, relation)
	}
	out = enforceSingleParent(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].SrcID != out[j].SrcID {
			return out[i].SrcID < out[j].SrcID
		}
		if out[i].DstID != out[j].DstID {
			return out[i].DstID < out[j].DstID
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// enforceSingleParent collapses concurrent parent-child edges so the graph stays
// a valid forest: each child keeps exactly one parent and no parent chain loops.
// blocks / related-to are additive and pass through; only parent-child is
// single-valued (a child's parent relation is stored src=child, dst=parent, one
// per child). [LAW:decomposition] This is the "choose-only, no semantic winner"
// branch of the merge cut: two parents cannot be combined, so a deterministic,
// symmetric tiebreak keeps the DAG invariant without inventing a clock.
func enforceSingleParent(relations []model.Relation) []model.Relation {
	parentOf := map[string]model.Relation{}
	out := make([]model.Relation, 0, len(relations))
	for _, relation := range relations {
		if relation.Type != model.RelParentChild {
			out = append(out, relation)
			continue
		}
		existing, seen := parentOf[relation.SrcID]
		if !seen || relation.DstID > existing.DstID {
			parentOf[relation.SrcID] = relation
		}
	}
	breakParentCycles(parentOf)
	for _, relation := range parentOf {
		out = append(out, relation)
	}
	return out
}

// breakParentCycles deletes one edge from every parent cycle so the union never
// commits a cycle (the store has no acyclicity guard, so a cycle here would be
// silent corruption — [LAW:no-silent-failure]). With single-parent already
// enforced the parent map is functional, so each cycle is a simple loop; the
// victim is the lexicographically greatest child id in the loop — a choice both
// machines compute identically from the same data.
func breakParentCycles(parentOf map[string]model.Relation) {
	const (
		unvisited = 0
		onPath    = 1
		settled   = 2
	)
	state := map[string]int{}
	for start := range parentOf {
		if state[start] != unvisited {
			continue
		}
		var path []string
		node := start
		for {
			if _, ok := parentOf[node]; !ok || state[node] == settled {
				break
			}
			if state[node] == onPath {
				cycle := path[indexOf(path, node):]
				delete(parentOf, maxString(cycle))
				break
			}
			state[node] = onPath
			path = append(path, node)
			node = parentOf[node].DstID
		}
		for _, n := range path {
			state[n] = settled
		}
	}
}

func indexOf(items []string, target string) int {
	for idx, item := range items {
		if item == target {
			return idx
		}
	}
	return -1
}

func maxString(items []string) string {
	out := items[0]
	for _, item := range items[1:] {
		if item > out {
			out = item
		}
	}
	return out
}

func mergeComments(issueSet map[string]struct{}, locals, remotes []model.Comment) []model.Comment {
	merged := map[string]model.Comment{}
	for _, comment := range append(locals, remotes...) {
		if _, ok := issueSet[comment.IssueID]; !ok {
			continue
		}
		merged[comment.ID] = comment
	}
	out := make([]model.Comment, 0, len(merged))
	for _, comment := range merged {
		out = append(out, comment)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func mergeLabels(issueSet map[string]struct{}, locals, remotes []model.Label) []model.Label {
	type key struct{ IssueID, Name string }
	merged := map[key]model.Label{}
	for _, label := range append(locals, remotes...) {
		if _, ok := issueSet[label.IssueID]; !ok {
			continue
		}
		merged[key{IssueID: label.IssueID, Name: label.Name}] = label
	}
	out := make([]model.Label, 0, len(merged))
	for _, label := range merged {
		out = append(out, label)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssueID != out[j].IssueID {
			return out[i].IssueID < out[j].IssueID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func mergeEvents(issueSet map[string]struct{}, locals, remotes []model.IssueEvent) []model.IssueEvent {
	merged := map[string]model.IssueEvent{}
	for _, event := range append(locals, remotes...) {
		if _, ok := issueSet[event.IssueID]; !ok {
			continue
		}
		merged[event.ID] = event
	}
	out := make([]model.IssueEvent, 0, len(merged))
	for _, event := range merged {
		out = append(out, event)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func maxInt(values ...int) int {
	if len(values) == 0 {
		return 1
	}
	max := values[0]
	for _, value := range values[1:] {
		if value > max {
			max = value
		}
	}
	return max
}
