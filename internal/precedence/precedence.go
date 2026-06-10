// Package precedence resolves ordered fallback chains: the first candidate
// that carries a value wins.
//
// [LAW:single-enforcer] This is the one definition of "first non-empty
// candidate in order"; callsites must not re-derive it with local helpers.
// [LAW:no-mode-explosion] There is deliberately no trim mode: whether a
// candidate is trimmed is a property of the value, enforced where the value
// is produced (see pathspec for path-valued candidates), not a flag here.
package precedence

// First returns the first non-empty candidate in order, or "" when every
// candidate is empty.
func First(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}
