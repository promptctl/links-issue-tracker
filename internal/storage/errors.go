package storage

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// NotFoundError is the contract's answer to "you named something that isn't
// there". Every engine returns it — wrapped or bare, matched with errors.As —
// for a read or a mutation against an id no issue, comment, or relation holds.
// It is part of the contract precisely because callers dispatch on it: the CLI
// maps it to its own exit code, so an engine that reported absence as a plain
// error would change observable behavior without changing a signature.
// [LAW:types-are-the-program] The classification lives in the type, never in
// message text a second engine would have to spell identically.
type NotFoundError struct {
	Entity string
	ID     string
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Entity, e.ID)
}

// RelationExistsError is the contract's answer to adding an edge the graph
// already holds. The edge asked for is in place, so the request is satisfied
// rather than refused, and a rerun gets the same answer: there is nothing to
// add and nothing to retry. Every engine returns it, so the CLI classifies
// the answer by its type and never by an engine's message or SQL error code.
// [LAW:types-are-the-program] [LAW:one-source-of-truth]
type RelationExistsError struct {
	SrcID string
	DstID string
	Type  model.RelationType
}

var _ model.StateRequestError = RelationExistsError{}

func (e RelationExistsError) Error() string {
	return fmt.Sprintf("relation src=%s dst=%s type=%s already exists, so there is nothing to add", e.SrcID, e.DstID, e.Type)
}

// Satisfied is always true: an edge that exists is the state the add asked for.
func (RelationExistsError) Satisfied() bool { return true }
