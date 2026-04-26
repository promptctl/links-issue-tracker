package merge

import (
	"reflect"
	"sort"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

type IssueConflict struct {
	IssueID string       `json:"issue_id"`
	Base    *model.Issue `json:"base,omitempty"`
	Local   *model.Issue `json:"local,omitempty"`
	Remote  *model.Issue `json:"remote,omitempty"`
}

type MergeResult struct {
	Export    model.Export    `json:"export"`
	Conflicts []IssueConflict `json:"conflicts"`
}

func ThreeWay(base model.Export, local model.Export, remote model.Export) MergeResult {
	baseMap := mapIssues(base.Issues)
	localMap := mapIssues(local.Issues)
	remoteMap := mapIssues(remote.Issues)

	allIDs := unionIssueIDs(baseMap, localMap, remoteMap)
	mergedIssues := make([]model.Issue, 0, len(allIDs))
	conflicts := make([]IssueConflict, 0)

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
			if issueEqual(localPtr, remotePtr) {
				if hasLocal {
					mergedIssues = append(mergedIssues, localIssue)
				}
				continue
			}
			conflicts = append(conflicts, IssueConflict{
				IssueID: id,
				Base:    basePtr,
				Local:   localPtr,
				Remote:  remotePtr,
			})
			if hasLocal {
				mergedIssues = append(mergedIssues, localIssue)
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
		History:     mergeHistory(issueSet, local.History, remote.History),
	}
	return MergeResult{Export: merged, Conflicts: conflicts}
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
	type key struct{ Src, Dst, Type string }
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

func mergeHistory(issueSet map[string]struct{}, locals, remotes []model.IssueHistory) []model.IssueHistory {
	merged := map[string]model.IssueHistory{}
	for _, event := range append(locals, remotes...) {
		if _, ok := issueSet[event.IssueID]; !ok {
			continue
		}
		merged[event.ID] = event
	}
	out := make([]model.IssueHistory, 0, len(merged))
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
