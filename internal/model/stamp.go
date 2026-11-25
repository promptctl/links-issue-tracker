package model

import (
	"fmt"
	"slices"
	"time"
)

// StampLayout is the one spelling a stored timestamp has: UTC, whole seconds,
// and always nine fractional digits. The width is fixed so that comparing two
// stamps as text compares the instants they spell. time.RFC3339Nano trims
// trailing zeros, which is why it cannot be stored: "…:07Z" would sort after
// "…:07.5Z", and "…:07.000000001Z" before "…:07Z".
//
// The store holds a CHECK on every timestamp column of its data, and on the
// from/to values of the change rows TimestampFields names, that admits
// only this shape, so a writer that spells an instant any other way is refused
// by the store itself. [LAW:types-are-the-program]
const StampLayout = "2006-01-02T15:04:05.000000000Z"

// StampText spells an instant the one way it is stored. Every writer of a
// stored timestamp — a column, a key — goes through it, and a change row's
// from/to value goes through TimestampChange or FieldChange.Stamped, which
// call it. [LAW:single-enforcer]
//
// It lives here because every writer can import model and model imports
// nothing above lifecycle: Purge.Key spells a stored key, and a home in a
// package that imports model would invert that dependency.
// [LAW:one-way-deps]
func StampText(t time.Time) string {
	return t.UTC().Format(StampLayout)
}

// ParseStamp reads a timestamp back into the instant it spells, in UTC. It
// accepts any RFC3339 spelling, not only the stored one: a value arrives here
// from an import, an older export, or a store a migration has not yet reached,
// and rejecting it would strand the data rather than protect the store. The
// CHECKs, not this reader, keep a store's own columns canonical. An instant
// whose UTC year StampText cannot spell in four digits is refused, so no bound
// or value built from it can sort as text out of its instant's order.
// Everything it returns, StampText spells one way.
func ParseStamp(text string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	t = t.UTC()
	if year := t.Year(); year < 0 || year > 9999 {
		return time.Time{}, fmt.Errorf("%q is %s in UTC, outside the years 0000 to 9999 a stamp spells", text, t.Format(time.RFC3339Nano))
	}
	return t, nil
}

// TimestampField is a FieldChange field whose from/to values are instants
// rather than text a person wrote. The set is closed: a writer names one of
// these, so it cannot misspell the field and slip past the respelling and the
// store's CHECKs on issue_event_changes, which name the same three fields.
// [LAW:one-source-of-truth]
type TimestampField int

const (
	ClosedAtField TimestampField = iota + 1
	ArchivedAtField
	DeletedAtField
)

// TimestampFields is every TimestampField.
func TimestampFields() []TimestampField {
	return []TimestampField{ClosedAtField, ArchivedAtField, DeletedAtField}
}

// Name is the field's name in a FieldChange and in the issue_event_changes
// table.
func (f TimestampField) Name() string {
	switch f {
	case ClosedAtField:
		return "closed_at"
	case ArchivedAtField:
		return "archived_at"
	case DeletedAtField:
		return "deleted_at"
	}
	panic(fmt.Sprintf("model: %d is not a TimestampField", int(f)))
}

// TimestampChange is the change of a timestamp field from one instant to
// another, either of which may be absent (""). The values are spelled the
// stored way, so no writer can record an instant any other way.
func TimestampChange(field TimestampField, from, to *time.Time) FieldChange {
	return FieldChange{Field: field.Name(), From: stampOrAbsent(from), To: stampOrAbsent(to)}
}

func stampOrAbsent(t *time.Time) string {
	if t == nil {
		return ""
	}
	return StampText(*t)
}

// Stamped is the change with its values spelled the stored way when its field
// holds instants, and the change unchanged otherwise. It is the checkpoint for
// a change that arrives from outside: an export written by an older release
// spells these values the trimmed way. A value that is no instant is an error,
// never stored. [LAW:parse-dont-validate]
func (c FieldChange) Stamped() (FieldChange, error) {
	if !slices.ContainsFunc(TimestampFields(), func(f TimestampField) bool { return f.Name() == c.Field }) {
		return c, nil
	}
	from, err := respell(c.From)
	if err != nil {
		return FieldChange{}, fmt.Errorf("change %s from %q: %w", c.Field, c.From, err)
	}
	to, err := respell(c.To)
	if err != nil {
		return FieldChange{}, fmt.Errorf("change %s to %q: %w", c.Field, c.To, err)
	}
	return FieldChange{Field: c.Field, From: from, To: to}, nil
}

// respell is a stamp's stored spelling. The empty value is the absence of an
// instant, not a spelling of one, and stays empty.
func respell(text string) (string, error) {
	if text == "" {
		return "", nil
	}
	t, err := ParseStamp(text)
	if err != nil {
		return "", err
	}
	return StampText(t), nil
}
