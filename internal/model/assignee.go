package model

import (
	"fmt"
	"strings"
)

// NormalizeAssignee returns an assignee's canonical form: trimmed, with empty
// meaning unassigned. A comma is refused because every assignee filter
// (--assignee and the assignee: query term) reads it as the separator between
// names, so a ticket assigned "Doe, J" could be written but never selected.
// Anything HoldRow would change is refused too: an assignee prints on one row,
// and the write is where a new one stops; HoldRow covers only values stored
// before this gate.
// [LAW:single-enforcer] This is the one definition of a writable assignee;
// create, the field update, start (Issue.Apply), and import all read it here,
// in both engines.
func NormalizeAssignee(assignee string) (string, error) {
	normalized := strings.TrimSpace(assignee)
	if strings.Contains(normalized, ",") {
		return "", ValidationError{Message: fmt.Sprintf("assignee %q cannot contain commas: every assignee filter reads a comma as the separator between names", normalized)}
	}
	if HoldRow(normalized) != normalized {
		return "", ValidationError{Message: fmt.Sprintf("assignee %q cannot contain a line break, tab, control or format character, or the %s token: an assignee prints on one row", normalized, QuotedBreak)}
	}
	return normalized, nil
}
