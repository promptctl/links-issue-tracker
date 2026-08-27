package storage

import "github.com/promptctl/links-issue-tracker/internal/model"

type AddCommentInput struct {
	IssueID   string
	Body      string
	CreatedBy string
}

type AddLabelInput struct {
	IssueID   string
	Name      string
	CreatedBy string
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

// IssueRelations is one issue together with its structural graph edges —
// parent, children, dependencies (DependsOn), and dependents (Blocks) — each
// hydrated, but WITHOUT the comment/event/related payload GetIssueDetail also
// loads. It is the shared lightweight per-issue shape batch consumers read, so
// neither the ready pipeline nor the epic view pays GetIssueDetail's per-row
// comment/event cost.
// [LAW:one-source-of-truth] One shape for "an issue's open blockers / parent
// epic" across consumers; a second batch type would let them drift.
//
// The direction convention it encodes — a blocks edge runs src=dependent,
// dst=dependency, so DependsOn and Blocks are the two readings of one edge set
// — is contract, not engine detail: two engines that bucketed an edge
// differently would disagree about which work is ready.
type IssueRelations struct {
	Issue     model.Issue
	Parent    *model.Issue
	Children  []model.Issue
	DependsOn []model.Issue
	Blocks    []model.Issue
}
