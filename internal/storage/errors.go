package storage

import "fmt"

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

// ValidationError is returned when a domain constraint (field value, type, range) is violated.
// [LAW:types-are-the-program] The type carries the classification so callers dispatch on type, not message text.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string { return e.Message }
