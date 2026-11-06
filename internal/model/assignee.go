package model

import (
	"fmt"
	"strings"
)

// NormalizeAssignee returns an assignee's canonical form: trimmed, with empty
// meaning unassigned. A comma is refused because every assignee filter
// (--assignee and the assignee: query term) reads it as the separator between
// names, so a ticket assigned "Doe, J" could be written but never selected.
// A line break is refused too, by the rule a title and a lane are written
// under: an assignee prints on one row. Anything else a row cannot carry is
// held where it prints, so a name with a ZWNJ or an emoji sequence stays
// writable.
// [LAW:single-enforcer] This is the one definition of a writable assignee;
// create, the field update, start (Issue.Apply), and import all read it here,
// in both engines.
func NormalizeAssignee(assignee string) (string, error) {
	normalized := strings.TrimSpace(assignee)
	if strings.Contains(normalized, ",") {
		return "", ValidationError{Message: fmt.Sprintf("assignee %q cannot contain commas: every assignee filter reads a comma as the separator between names", normalized)}
	}
	if HasLineBreak(normalized) {
		return "", ValidationError{Message: fmt.Sprintf("assignee %q cannot hold a line break: an assignee prints on one row", normalized)}
	}
	return normalized, nil
}
