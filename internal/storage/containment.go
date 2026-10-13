package storage

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Containment is the rule the hierarchy obeys: every ticket lives inside a
// container. A task, bug, feature or chore always has a parent, and every
// parent is an epic, so only an epic may be top-level and an epic is the only
// thing a child is ever inside. Together the two halves make the hierarchy
// epics all the way down to leaves: a leaf has a checkpoint it serves, ranks
// among its epic's children rather than against the epics, and "the other
// tickets in its epic" is its siblings.
//
// The rule is enforced where the hierarchy is written, in both engines, so a
// ticket without a home is unrepresentable rather than a stray a later check
// reports: a create refuses a leaf with no parent and any parent that is not
// an epic, a reparent refuses a parent that is not an epic, and a detach
// refuses to leave a leaf with no parent, since that would leave it nowhere. Changing a
// ticket's type across the container line is refused elsewhere (ApplyIssueFields),
// so neither half can be broken by retyping an endpoint afterwards. The three
// refusals live here, once, so the two engines render one rule rather than two
// that happen to agree. [LAW:single-enforcer] [LAW:one-source-of-truth]
// [LAW:types-are-the-program]

// RejectLeafWithoutParent refuses creating a ticket of issueType with no
// parent when the type is not a container. Every create path returns it — `lit
// new`, `lit followup`, a tree file, a bulk file — and each spells the parent
// and the type its own way, so the message names the two ways on in the
// domain's words rather than one path's flags.
func RejectLeafWithoutParent(issueType model.IssueType, parentID string) error {
	if issueType.IsContainer() || parentID != "" {
		return nil
	}
	return model.ValidationError{Message: fmt.Sprintf("cannot create a %s outside an epic: every ticket except an epic lives inside one, so give it the epic it belongs to as its parent, or make it an epic", issueType)}
}

// RejectParentThatIsNotAContainer refuses parentID as a parent when its type
// is not a container, whichever write names it: a create, a reparent, or a
// parent-child edge written through `lit dep add`.
func RejectParentThatIsNotAContainer(parentID string, parentType model.IssueType) error {
	if parentType.IsContainer() {
		return nil
	}
	return model.ValidationError{Message: fmt.Sprintf("cannot file a ticket under %s: it is a %s, and only an epic contains tickets", parentID, parentType)}
}

// RejectNonContainerParentEdge is RejectParentThatIsNotAContainer asked of an
// edge: only parent-child names a parent, so every other type passes. Both
// engines prove the edge's dst exists by reading its type, and hand that type
// here, so the rule is one function rather than one per engine.
// [LAW:dataflow-not-control-flow] The one branch here is the domain's own enum.
func RejectNonContainerParentEdge(relType model.RelationType, parentID string, parentType model.IssueType) error {
	if relType != model.RelParentChild {
		return nil
	}
	return RejectParentThatIsNotAContainer(parentID, parentType)
}

// RejectDetachingALeaf refuses leaving childID with no parent when the child
// is not a container: it would be outside every epic. parentsLeft is how many
// parent edges the child holds once the detach is done, so removing one of two
// edges restored data left on a leaf passes, since the leaf is still inside an
// epic. Moving a leaf is `lit parent set`, which replaces the parent in one act.
func RejectDetachingALeaf(childID string, childType model.IssueType, parentsLeft int) error {
	if childType.IsContainer() || parentsLeft > 0 {
		return nil
	}
	return model.ValidationError{Message: fmt.Sprintf("cannot detach %s from its epic: a %s lives inside an epic, so move it with lit parent set --child %s --parent <epic-id>", childID, childType, childID)}
}

// RejectDetachingParentEdge is RejectDetachingALeaf asked of a removed edge:
// only parent-child names a parent, so every other type passes. readChild
// returns the child's type and the parent edges it keeps, and is called only
// for a parent-child edge, so removing an edge of another type reads nothing
// about its endpoints. Each engine supplies the read; the rule is one function
// rather than one per engine, as on the add side (RejectNonContainerParentEdge).
// [LAW:single-enforcer] [LAW:dataflow-not-control-flow] The one branch here is
// the domain's own enum.
func RejectDetachingParentEdge(relType model.RelationType, childID string, readChild func() (childType model.IssueType, parentsLeft int, err error)) error {
	if relType != model.RelParentChild {
		return nil
	}
	childType, parentsLeft, err := readChild()
	if err != nil {
		return err
	}
	return RejectDetachingALeaf(childID, childType, parentsLeft)
}
