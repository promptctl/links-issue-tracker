// Package storage is lit's storage contract: what lit needs from a storage
// engine, stated in the model's vocabulary, with no engine anywhere in the
// signatures.
//
// # Why the contract is its own package
//
// lit's de facto storage contract used to be the exported method set of the
// concrete Dolt-backed type in internal/store. That had two costs. A second
// engine could not exist — nothing to implement, since callers were compiled
// against a struct — and nothing distinguished storage from Dolt's biography:
// reconcile, checkpoints, and schema migration sat in the same method set as
// GetIssue, so an engine with no schema and no merge would have inherited them
// as if they were storage. This package is the line between the two. What is
// here, every engine owes; what is not here, an engine may still offer, but it
// offers it as a named capability — see [Capability] and the interfaces beside
// it, and [Offered] for how an engine is asked.
//
// # The vocabulary rule
//
// Every type crossing this boundary is a model type or a type declared here.
// [Store] and the interfaces it composes may name no SQL row, no branch, no
// commit, no schema version, and no other engine artifact. That rule is what
// makes the core a contract rather than a description of Dolt: a signature
// that mentions an engine artifact silently obliges every future engine to
// grow one. [LAW:one-way-deps] The dependency runs engine → contract → model,
// never back; internal/store imports this package, and this package imports no
// engine.
//
// The capability interfaces are held to a different line, and deliberately:
// naming an engine artifact is precisely what makes something a capability
// instead of storage. [SchemaMigrator] must speak of schema versions or it
// describes nothing; [Checkpointer] must hand back an anchor the engine can
// resolve. What that buys is that the artifacts are quarantined behind an
// interface an engine can decline, rather than mixed into the set every engine
// owes.
//
// # The conformance suite is the actual specification
//
// An interface pins shapes; only behavior is a contract. The executable
// meaning of everything declared here lives in
// [internal/storage/conformance], a suite parameterized over "give me a fresh
// engine stamping from this clock" that every implementation runs. An engine
// that satisfies the interface and fails the suite does not implement this
// package. The clock is in that parameterization rather than behind a
// capability because time ordering is a rule every engine owes and only some
// engines can get wrong; see [Clock].
// [LAW:behavior-not-structure]
//
// # Where this sits in the campaign
//
// This is migration state S0 of the event-store campaign
// (design-docs/event-store/design.md §migration): the seam exists, Dolt
// implements it, and observable behavior is unchanged. The states after it
// hang off this package — S1's dual-write is a decorator on [Store], and the
// differential oracle diffs through [Exporter], the one full-state read two
// engines can both serve.
package storage
