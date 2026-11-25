package model

import "time"

// StampLayout is the one spelling a stored timestamp has: UTC, whole seconds,
// and always nine fractional digits. The width is fixed so that comparing two
// stamps as text compares the instants they spell. time.RFC3339Nano trims
// trailing zeros, which is why it cannot be stored: "…:07Z" would sort after
// "…:07.5Z", and "…:07.000000001Z" before "…:07Z".
//
// The store holds a CHECK on every timestamp column that admits only this
// shape, so a writer that spells an instant any other way is refused by the
// store itself. [LAW:types-are-the-program]
const StampLayout = "2006-01-02T15:04:05.000000000Z"

// StampText spells an instant the one way it is stored. Every writer of a
// stored timestamp — a column, a change row's from/to value, a key — goes
// through it. [LAW:single-enforcer]
//
// It lives here because every writer can import model and model imports
// nothing above lifecycle: Purge.Key spells a stored key, and a home in a
// package that imports model would invert that dependency.
// [LAW:one-way-deps]
func StampText(t time.Time) string {
	return t.UTC().Format(StampLayout)
}

// ParseStamp reads a stored timestamp back into the instant it spells, in UTC.
// It accepts any RFC3339 spelling, not only the stored one: a value arrives
// here from an import, an older export, or a store a migration has not yet
// reached, and rejecting it would strand the data rather than protect the
// store. Everything it returns, StampText spells one way.
func ParseStamp(text string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
