// Package engine is the one place lit chooses which storage engine backs a
// workspace, and the only package outside internal/store that names the Dolt
// engine's construction surface.
//
// Everything above this package — app, CLI, and every command behind them —
// holds [storage.Store] and the capability interfaces beside it. They cannot
// name a concrete engine, so they cannot come to depend on one having a
// particular shape, and the campaign's later states can change what Open
// returns without touching a caller. [LAW:one-way-deps]
//
// This is the seam design-docs/event-store/design.md §migration presses on:
// S1's dual-write decorator wraps what Open returns, S2's read-flip changes
// which engine is behind the wrapper, and S4 deletes the Dolt arm of the table
// below. That is why construction is one function taking a mode rather than
// three functions named for their modes — a decorator has one seam to wrap
// instead of three, and a mode added later cannot be silently missed by one of
// them. [LAW:dataflow-not-control-flow]
package engine

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// Mode is the open contract a caller needs from the engine: whether an absent
// database is bootstrapped or refused, and whether the handle may write.
//
// It is a fact about what the caller intends to do, which is why it is the
// caller's to supply and the table below is the only thing that translates it
// into an engine. [LAW:types-are-the-program] The zero value is not a mode, so
// a caller that forgets to choose one fails rather than defaulting into write
// access.
type Mode string

const (
	// ReadWrite bootstraps the database when it is absent and takes the
	// exclusive workspace lock. It is what a mutating command opens.
	ReadWrite Mode = "read-write"

	// ReadOnly requires an already-initialized database and takes a shared
	// lock, so concurrent readers do not contend and a reader never creates a
	// workspace by accident.
	ReadOnly Mode = "read-only"

	// Sync is the exchange contract: a handle held across network operations,
	// whose locking admits the mirror running beside it.
	Sync Mode = "sync"
)

// openers is the whole meaning of Mode, and the only place in lit above
// internal/store where the Dolt engine's constructors are named.
//
// [LAW:one-source-of-truth] A mode's engine is decided here once. When S2
// flips reads to the event store, a row changes and no caller does; when S4
// deletes Dolt, this table is where the deletion lands.
var openers = map[Mode]func(ctx context.Context, doltRootDir string, workspaceID string) (*store.Store, error){
	ReadWrite: store.Open,
	ReadOnly:  store.OpenForRead,
	Sync:      store.OpenSync,
}

// Open constructs the engine backing the workspace at doltRootDir.
//
// What comes back is the contract, never the engine that satisfies it: the
// concrete type is erased here so that no caller can reach past the interface,
// which is what makes the seam hold without anyone having to remember it.
// Capabilities beyond [storage.Store] are asked for with storage.<Cap>.Of on
// the returned value.
func Open(ctx context.Context, mode Mode, doltRootDir string, workspaceID string) (storage.Store, error) {
	open, known := openers[mode]
	if !known {
		// [LAW:no-silent-failure] An unknown mode — including the zero value —
		// fails closed rather than falling through to a default arm that would
		// hand out write access nobody asked for.
		return nil, fmt.Errorf("invalid storage engine mode %q", string(mode))
	}
	engine, err := open(ctx, doltRootDir, workspaceID)
	if err != nil {
		// Returning err alone matters: a *store.Store typed nil wrapped in the
		// storage.Store interface would be a non-nil interface holding a nil
		// pointer, so callers checking `if engine != nil` would be lied to.
		// [LAW:no-silent-failure]
		return nil, err
	}
	return engine, nil
}
