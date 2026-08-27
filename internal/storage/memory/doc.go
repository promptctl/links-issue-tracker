// Package memory implements lit's storage contract with nothing but Go
// values: no SQL, no disk, no schema, no engine artifact of any kind.
//
// # Why a second engine exists
//
// An interface with one implementation is an assertion; with two it is a
// proven boundary. While the Dolt-backed engine was the only thing satisfying
// [storage.Store], "the contract" and "what Dolt happens to do" were the same
// sentence, and nothing could tell which of its behaviors lit needs from which
// ones a versioned SQL database merely has. This package is the second
// reading. It runs the same suite — [internal/storage/conformance] — so every
// statement that suite makes is now a statement two unrelated implementations
// both satisfy.
//
// # Independent by construction
//
// Nothing here is shared with the Dolt engine: not the field-patch diff, not
// the transition planner, not the frame resolution behind the rank intents,
// not the compensating bulk apply. Every one of those is a pure function of
// model values living in internal/store, and lifting them into the contract
// package would look like the obvious [LAW:one-source-of-truth] fix. It would
// also quietly destroy the proof — two engines calling one implementation is
// one implementation wearing two hats, and a green conformance run would then
// say nothing at all about those paths. The suite is the shared definition of
// behavior; the code is deliberately not. [LAW:behavior-not-structure]
//
// Where the suite under-pins a behavior, Dolt's current behavior is the
// tiebreak rather than what would be tidier: S0's whole gate is that nothing
// observable changes (design-docs/event-store/design.md §migration). Two
// places where this engine deliberately answers better than Dolt — because
// Dolt's answer is an artifact of storing what this engine derives — are
// recorded on links-store-seam-q35v.5:
//
//   - Ordering a listing by "status" reads the derived container state here,
//     where Dolt sorts the stored status column that is NULL for every epic.
//   - History comes back in the order it was recorded, where Dolt sorts by
//     timestamp and breaks ties on a random event id — so on a coarse clock
//     Dolt can hand back a title change ahead of the creation that preceded
//     it.
//
// # What it is for
//
// Two things, both downstream. Tests that need storage behavior but not a
// storage engine get one instantly, which is the fixture tax the testperf
// epic measures. And the campaign's differential oracle runs its
// planted-divergence cases against two instances of this engine, which is why
// every read here is deterministic rather than merely reproducible.
//
// # What it declines
//
// All seven capabilities. It has no remote to sync with, no divergence to
// reconcile, no history to check point, no faults of its own making to
// repair, no schema to migrate, and no engine-native language to run a raw
// statement in. [storage.Offered] reports the empty set, and that is a
// complete answer rather than a gap — see [storage.Capability].
package memory
