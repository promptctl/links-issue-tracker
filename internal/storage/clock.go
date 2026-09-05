package storage

import "time"

// Clock is where an engine reads the instant it stamps a write with.
//
// It exists because a timestamp an engine reaches for is a hidden input: no
// signature admits it, no caller can choose it, and nothing outside the engine
// can construct a pair of instants the real clock would never hand out in that
// combination. Two engine defects escaped through that gap — an export-ordering
// assertion that passed five runs out of five against a bug reintroduced on
// purpose, and a listing that ordered created_at by its RFC3339Nano spelling
// rather than by the instant it denotes — and neither could be stated where
// they belonged, because the conformance suite reaches an engine only through
// this package. [LAW:effects-at-boundaries] the clock is supplied at the edge
// where the engine is built, not read from inside the write path.
//
// # Why this is core and not a capability
//
// A capability names what an engine has no meaning for — a divergence an
// append-only log never reaches, a statement language a map has no grammar for.
// Every engine stamps CreatedAt and UpdatedAt and orders listings by them, so
// none of them LACKS a clock; they differ only in whether they were built to be
// told what it reads, which is a fact about an implementation rather than about
// the domain. An engine allowed to decline would be an engine the time-ordering
// cases skip, and the engines that can sort a timestamp by its spelling — the
// ones that round-trip an instant through text — are exactly the ones that
// would skip them. So the suite would gain cases that cannot reach the engines
// that need them. [LAW:types-are-the-program]
type Clock func() time.Time

// Now reads the clock and normalizes the instant to UTC, which is the form
// every timestamp in the contract carries.
//
// Engines call this rather than the function itself, so the normalization has
// one home instead of one per stamp — there were seventeen of them across the
// two engines before this existed. [LAW:single-enforcer]
func (c Clock) Now() time.Time { return c().UTC() }

// SystemClock is the real clock, and the one every engine a person opens is
// built with. It is a function rather than a package-level Clock value so that
// nothing can reassign what "the real clock" means for the whole process.
// [LAW:no-shared-mutable-globals]
func SystemClock() time.Time { return time.Now() }
