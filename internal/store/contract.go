package store

import "github.com/promptctl/links-issue-tracker/internal/storage"

// This file is where the Dolt engine meets lit's storage contract, and it is
// deliberately the only place in this package that knows the contract exists.
//
// The assertions below are the load-bearing lines: they fail the build the
// moment this engine stops satisfying [storage.Store] or stops offering a
// capability it claims, which is what makes the contract a constraint on the
// engine rather than a document about it. [LAW:types-are-the-program]
//
// Dolt offers all seven capabilities, which is exactly why they are capabilities
// and not contract methods: this engine is a versioned SQL database, and sync,
// reconcile, checkpoints, repair, schema migration, import, and a raw query
// surface are things it has because of what it is. Asserting them here rather
// than declaring them in the contract is what leaves room for an engine that
// has fewer. [storage.Offered] reads this set back at runtime.
//
// The alias block that stood here is gone, and its absence is the point.
// links-store-seam-q35v.1 and .2 relocated lit's whole storage vocabulary into
// internal/storage but kept it re-exported under its old `store.X` spelling, so
// that carving the seam rewired no caller. This engine's own files now spell
// those types `storage.X` like everyone else, which leaves the package
// exporting no vocabulary a caller could reach for by habit — the engine is
// reached through the contract or not at all. [LAW:one-source-of-truth]
//
// What remains exported here beyond the [Store] methods is Dolt-era workspace
// machinery addressed by filesystem path rather than by engine handle: the
// workspace and commit flocks, the mirror beacons, bootstrap and remote
// adoption, snapshot naming, and lifeboat recovery. It is not contract
// material and is deliberately not wrapped as if it were —
// design-docs/event-store/design.md §migration schedules it for DELETION at
// S4 rather than for a second implementation, and an interface built over
// condemned machinery is carrying cost with no second implementer coming.
// TestCLIAndAppReachStorageOnlyThroughTheContract in internal/cli enumerates
// exactly what the CLI still reaches for, and that list only shrinks.
var (
	_ storage.Store = (*Store)(nil)

	_ storage.Syncer         = (*Store)(nil)
	_ storage.Reconciler     = (*Store)(nil)
	_ storage.Checkpointer   = (*Store)(nil)
	_ storage.Repairer       = (*Store)(nil)
	_ storage.SchemaMigrator = (*Store)(nil)
	_ storage.Importer       = (*Store)(nil)
	_ storage.RawExecutor    = (*Store)(nil)
)
