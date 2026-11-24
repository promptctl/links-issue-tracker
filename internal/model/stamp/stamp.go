// Package stamp is the instant a write records: its row's updated_at, its
// event's created_at, and the close time a close carries.
package stamp

import "time"

// Instant is a write's stamp. Only Next builds one, so every instant a
// transition records came through the stamp rule and is in UTC.
// [LAW:types-are-the-program] A bare time.Time could arrive from any clock in
// any zone; this type admits only the stamp.
type Instant struct {
	at time.Time
}

// Next is the stamp a write taken at now gets when newest is the newest
// instant in the history it was planned against: now, unless newest is at or
// after it, and then the instant just past newest. No history is the zero
// time, which is before every clock in use. It is a hybrid logical clock: the
// wall clock, held back from ever running behind the history a write
// observed. Events are ordered by their stamp, so a clone whose clock runs
// behind would otherwise write an event that sorts before history it already
// synced and planned against, and the phase and the lane holder would both be
// read off the older event. The nanosecond step also parts a write from an
// event stamped at the same instant, which the id tie-break orders
// arbitrarily.
//
// The stamp is in UTC whichever instant it follows: history read back from a
// store, or a clock, can carry an offset, and a stamp is stored as text that
// sorts by its spelling.
// [LAW:single-enforcer] the one statement of the rule both engines stamp by,
// and the one place its result takes the contract's form.
func Next(now, newest time.Time) Instant {
	if newest.Before(now) {
		return Instant{at: now.UTC()}
	}
	return Instant{at: newest.Add(time.Nanosecond).UTC()}
}

// Time is the stamp as an instant.
func (i Instant) Time() time.Time { return i.at }
