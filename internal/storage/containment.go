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
// refuses a leaf, since detaching one would leave it nowhere. Changing a
// ticket's type across the container line is refused elsewhere (ApplyIssueFields),
// so neither half can be broken by retyping an endpoint afterwards. The three
// refusals live here, once, so the two engines render one rule rather than two
// that happen to agree. [LAW:single-enforcer] [LAW:one-source-of-truth]
// [LAW:types-are-the-program]

// RejectLeafWithoutParent refuses creating a ticket of issueType with no
// parent when the type is not a container.
func RejectLeafWithoutParent(issueType model.IssueType, parentID string) error {
	if issueType.IsContainer() || parentID != "" {
		return nil
	}
	return model.ValidationError{Message: fmt.Sprintf("cannot create a %s outside an epic: every ticket except an epic lives inside one, so name the epic it belongs to (lit new --parent <epic-id>) or create an epic for it (--type epic)", issueType)}
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

// RejectDetachingALeaf refuses removing childID's parent edge when the child
// is not a container: with no parent it would be outside every epic. Moving
// it is `lit parent set`, which replaces the parent in one act.
func RejectDetachingALeaf(childID string, childType model.IssueType) error {
	if childType.IsContainer() {
		return nil
	}
	return model.ValidationError{Message: fmt.Sprintf("cannot detach %s from its epic: a %s lives inside an epic, so move it with lit parent set --child %s --parent <epic-id>", childID, childType, childID)}
}
