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
// offers it as a named capability rather than as storage (see
// links-store-seam-q35v.2).
//
// # The vocabulary rule
//
// Every type crossing this boundary is a model type or a type declared here.
// Nothing in this package may name a SQL row, a Dolt branch, a commit, a
// schema version, or any other engine artifact. That rule is what makes the
// interface a contract rather than a description of Dolt: a signature that
// mentions an engine artifact silently obliges every future engine to grow
// one. [LAW:one-way-deps] The dependency runs engine → contract → model, never
// back; internal/store imports this package, and this package imports no
// engine.
//
// # The conformance suite is the actual specification
//
// An interface pins shapes; only behavior is a contract. The executable
// meaning of everything declared here lives in
// [internal/storage/conformance], a suite parameterized over "give me a fresh
// engine" that every implementation runs. An engine that satisfies the
// interface and fails the suite does not implement this package.
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
