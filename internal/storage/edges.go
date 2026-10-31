package storage

import (
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// SameEpicBlocksRejectionMessage refuses a blocked-by edge that would order work
// inside one epic, where rank is already the ordering signal.
// [LAW:one-source-of-truth] The text is user-facing contract and asserted
// verbatim in tests; RejectBlocksWithinEpic is the one rule that refuses with it.
const SameEpicBlocksRejectionMessage = "Do not set blocked-by relationships between two issues in the same epic.  Use rank to specify that one issue must be completed before another issue"

// EpicTree is the containment the same-epic blocks rule reads: every issue
// that sits directly under an epic, with those epics and its own type. An
// issue under no epic has no entry. A parent that is not an epic is left out,
// so a climb ends there, where readiness's own climb (epicsAbove) ends.
type EpicTree map[string]EpicPlacement

// EpicPlacement is one issue's entry in an EpicTree. Epics holds more than one
// id only in restored data, which the single-parent rule never gated.
type EpicPlacement struct {
	Epics []string
	Type  model.IssueType
}

// Place records that child, of childType, sits directly under epic.
func (t EpicTree) Place(child string, childType model.IssueType, epic string) {
	placement := t[child]
	placement.Type = childType
	placement.Epics = append(placement.Epics, epic)
	t[child] = placement
}

// RejectBlocksWithinEpic refuses a blocked-by edge that orders work inside one
// epic, which is rank's job. Two shapes do that:
//
//   - One endpoint sits inside the other's epic, at any depth. Either direction
//     closes a wait loop through containment: an epic's blockers hold back
//     everything beneath it, so an issue blocking an epic it sits under waits
//     on itself, and an epic blocking something it contains waits on its own
//     work.
//   - Both endpoints are leaves directly under one epic. Edges between epics
//     stay legal, sub-epics of one epic included, as does an edge between a
//     leaf and the sub-epic beside it: those order work units, where the
//     leaves of one epic are rank's to order.
//
// Both engines call this where every blocked-by edge is written, and the file
// gate calls it over a file's own hierarchy, so `lit dep add` and both import
// formats meet the same rule. A reparent can still leave the shape behind;
// readiness reads the workspace as it is and stays correct.
// [LAW:single-enforcer] [LAW:one-source-of-truth]
func RejectBlocksWithinEpic(tree EpicTree, dependent, dependency string) error {
	if climbsTo(tree, dependent, dependency) || climbsTo(tree, dependency, dependent) || leafSiblings(tree, dependent, dependency) {
		return model.ValidationError{Message: SameEpicBlocksRejectionMessage}
	}
	return nil
}

// leafSiblings reports whether a and b are two leaves directly under one
// epic. An issue is not its own sibling; a self-edge is the cycle check's to
// refuse. An issue under no epic has the zero placement, which shares no epic.
func leafSiblings(tree EpicTree, a, b string) bool {
	placeA, placeB := tree[a], tree[b]
	return a != b && !placeA.Type.IsContainer() && !placeB.Type.IsContainer() &&
		slices.ContainsFunc(placeA.Epics, func(epic string) bool { return slices.Contains(placeB.Epics, epic) })
}

// climbsTo reports whether walking up tree from `from` reaches `to`.
// seen bounds the walk, because data written before the parent-cycle rule can
// already hold a loop, and this check must not be what hangs on it.
func climbsTo(tree EpicTree, from, to string) bool {
	seen := map[string]struct{}{from: {}}
	for stack := []string{from}; len(stack) > 0; {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, parent := range tree[at].Epics {
			if parent == to {
				return true
			}
			if _, visited := seen[parent]; visited {
				continue
			}
			seen[parent] = struct{}{}
			stack = append(stack, parent)
		}
	}
	return false
}

type AddCommentInput struct {
	IssueID   string
	Body      model.CommentBody
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
// parent, children, dependencies (BlockedBy), and dependents (Blocks) — each
// hydrated, but WITHOUT the comment/event/related payload GetIssueDetail also
// loads. It is the shared lightweight per-issue shape batch consumers read, so
// neither the ready pipeline nor the epic view pays GetIssueDetail's per-row
// comment/event cost.
// [LAW:one-source-of-truth] One shape for "an issue's open blockers / parent
// epic" across consumers; a second batch type would let them drift.
//
// The direction convention it encodes — a blocked-by edge reads "src blocked-by
// dst", so BlockedBy (the edges out) and Blocks (the edges in) are the two
// readings of one edge set — is contract, not engine detail: two engines that bucketed an edge
// differently would disagree about which work is ready.
//
// Children, BlockedBy and Blocks are each in tree order, with one exception
// only restored data can reach: Children compare on their own keys, as siblings
// do, so a child two parents claim lists among the children of its other parent
// too, where [RankAncestry] files it under the lower one. `lit doctor` names it.
type IssueRelations struct {
	Issue     model.Issue
	Parent    *model.Issue
	Children  []model.Issue
	BlockedBy []model.Issue
	Blocks    []model.Issue
}

// IssueAdjacency is what [IssueReader.GetIssueDetail] holds that the issue's own
// row and edges name, each hydrated. It has no follow-ups because no row of the
// issue's names them, so holding one proves the read stayed off the issue
// table's size. [LAW:types-are-the-program]
type IssueAdjacency struct {
	IssueRelations
	Relations      []model.Relation
	Siblings       []model.Issue
	Related        []model.Issue
	RedirectTarget *model.Issue
	FollowsUp      *model.Issue
}

// Detail is the whole single-issue view: this adjacency and the rest of the
// issue's record. [LAW:one-source-of-truth] One assembly for both engines.
func (a IssueAdjacency) Detail(comments []model.Comment, events []model.IssueEvent, planReviews []model.PlanReview, followUps []model.Issue) model.IssueDetail {
	return model.IssueDetail{
		Issue:          a.Issue,
		Relations:      a.Relations,
		Comments:       comments,
		Events:         events,
		PlanReviews:    planReviews,
		Children:       a.Children,
		Siblings:       a.Siblings,
		BlockedBy:      a.BlockedBy,
		Blocks:         a.Blocks,
		Parent:         a.Parent,
		Related:        a.Related,
		RedirectTarget: a.RedirectTarget,
		FollowsUp:      a.FollowsUp,
		FollowUps:      followUps,
	}
}
