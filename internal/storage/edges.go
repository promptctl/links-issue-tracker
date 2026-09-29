package storage

import "github.com/promptctl/links-issue-tracker/internal/model"

// SameEpicBlocksRejectionMessage refuses a blocks edge that would order work
// inside one epic, where rank is already the ordering signal.
// [LAW:one-source-of-truth] The text is user-facing contract and asserted
// verbatim in tests; the CLI's sibling check and both engines' ancestry check
// read it from here so the two halves of the rule cannot drift apart.
const SameEpicBlocksRejectionMessage = "Do not set 'blocks' relationships between two issues in the same epic.  Use rank to specify that one issue must be completed before another issue"

// RejectBlocksAlongHierarchy refuses a blocks edge whose endpoints sit one above
// the other in the hierarchy, at any depth. parentOf maps each child to every
// parent it can climb to.
//
// Either direction closes a wait loop through containment: an ancestor's
// blockers hold back everything beneath it, so a descendant blocking its
// ancestor waits on itself, and an ancestor blocking its descendant waits on
// work it contains. Both are intra-epic ordering, which is rank's job.
//
// Both engines call this from the one body every relation edge is written
// through, so `lit dep add` and both import formats meet the same rule.
// [LAW:single-enforcer] [LAW:one-source-of-truth]
func RejectBlocksAlongHierarchy(parentOf map[string][]string, dependent, dependency string) error {
	if climbsTo(parentOf, dependent, dependency) || climbsTo(parentOf, dependency, dependent) {
		return model.ValidationError{Message: SameEpicBlocksRejectionMessage}
	}
	return nil
}

// climbsTo reports whether walking up the hierarchy from `from` reaches `to`.
// seen bounds the walk, because data written before the parent-cycle rule can
// already hold a loop, and this check must not be what hangs on it.
func climbsTo(parentOf map[string][]string, from, to string) bool {
	seen := map[string]struct{}{from: {}}
	for stack := []string{from}; len(stack) > 0; {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, parent := range parentOf[at] {
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
