package model

import (
	"fmt"
	"strings"
)

// NormalizeAssignee returns an assignee's canonical form: trimmed, with empty
// meaning unassigned. A comma is refused because every assignee filter
// (--assignee and the assignee: query term) reads it as the separator between
// names, so a ticket assigned "Doe, J" could be written but never selected.
// [LAW:single-enforcer] This is the one definition of a writable assignee;
// create, the field update, start, and import all read it here, in both
// engines.
func NormalizeAssignee(assignee string) (string, error) {
	normalized := strings.TrimSpace(assignee)
	if strings.Contains(normalized, ",") {
		return "", ValidationError{Message: fmt.Sprintf("assignee %q cannot contain commas: every assignee filter reads a comma as the separator between names", normalized)}
	}
	return normalized, nil
}
