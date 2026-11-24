// Package stamp is the instant a write records: its row's updated_at, its
// event's created_at, and the close or retention time it carries.
package stamp

import "time"

// Instant is a write's stamp, built by Next. What the type guarantees is that
// every instant read out of one came through the stamp rule and is in UTC:
// Go admits the zero literal in any package, so the type cannot keep an
// unbuilt Instant from being written, and instead refuses to be read as one —
// Time panics on an Instant Next did not build, rather than recording the
// year 1 as an instant. [LAW:types-are-the-program] [LAW:no-silent-failure]
type Instant struct {
	at    time.Time
	built bool
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
// The stamp is in UTC whichever instant it follows, the one form an instant a
// write records takes.
// [LAW:single-enforcer] the one statement of the rule both engines stamp by,
// and the one place its result takes that form.
func Next(now, newest time.Time) Instant {
	if newest.Before(now) {
		return Instant{at: now.UTC(), built: true}
	}
	return Instant{at: newest.Add(time.Nanosecond).UTC(), built: true}
}

// Time is the stamp as an instant, read where a write records it.
func (i Instant) Time() time.Time {
	if !i.built {
		panic("stamp: an Instant read that stamp.Next did not build")
	}
	return i.at
}

// Ahead is how far the stamp is past t: zero when the stamp is t itself.
func (i Instant) Ahead(t time.Time) time.Duration { return i.Time().Sub(t) }
