package storage

import (
	"context"
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Capabilities are what an engine may offer beyond [Store], and the line
// between the two is the whole point of this file.
//
// [Store] is what lit needs from storage; everything here is machinery one
// particular engine happens to have. The Dolt store offers all seven because
// Dolt is a versioned SQL database with remotes, branches, a schema, and a raw
// query surface. The event store planned in design-docs/event-store/ offers
// sync and not reconcile: its arrival "needs no merge — new events simply
// exist locally" (design.md §sync), so it has no diverged state, no base to
// merge through, and no side to take. An engine cannot honestly implement what
// it has no meaning for, which is why the split runs where it does.
//
// The granularity rule, applied to every family below: two operations share a
// capability only when no engine could plausibly offer one without the other.
// That is why reconcile is not folded into sync, and why checkpoints, repair,
// and schema migration are three answers rather than one "maintenance" answer
// an engine would have to give as a whole or not at all. [LAW:decomposition]
//
// Absence is answered, never guessed and never faked. A caller asks with
// [capability.Of], which hands back the interface or an [UnsupportedError]
// naming what was missing — so "this engine does not do that" is a value that
// flows, rather than a panic, a nil check, or a stub that lies about having
// done the work. [LAW:no-silent-failure]

// Syncer exchanges the store's contents with peers, and reclaims what those
// exchanges leave behind.
//
// Transport and observation are most of it: configure remotes, send, receive,
// and report where local stands. Compaction sits here rather than in a
// capability of its own because SyncCompactAndPush compacts and pushes under a
// single commit-lock acquisition — one atomic operation, so the push reflects
// exactly the compacted state, and not something a caller could assemble from
// SyncCompact plus SyncPush. Any engine offering this interface therefore
// already owes compaction, so splitting the two explicit compaction methods out
// would not let an engine decline one and keep the other; it would only move
// them. Nor could SyncCompactAndPush follow them out, since it pushes and would
// then owe both capabilities — that it fits on neither side of such a line is
// what says the two concerns are genuinely joined here, at the store's write
// path and its commit lock. [LAW:decomposition]
//
// Nothing here resolves a divergence — that is [Reconciler], and keeping the
// two apart is what lets an engine whose arrivals cannot conflict offer this
// and stop.
type Syncer interface {
	SyncAddRemote(ctx context.Context, name string, url string) error
	SyncRemoveRemote(ctx context.Context, name string) error
	SyncListRemotes(ctx context.Context) ([]SyncRemote, error)

	// SyncStatus reports the local side only — build, position, pending
	// changes, peers — and contacts no network.
	SyncStatus(ctx context.Context) (SyncStatusReport, error)

	// SyncFreshness reports the local branch's position against a peer as of
	// the last fetch or push. It reads local refs; it never contacts the
	// network, which is what lets a read-only command call it.
	SyncFreshness(ctx context.Context, remote string, branch string) (SyncFreshness, error)

	SyncFetch(ctx context.Context, remote string, prune bool) error
	SyncPush(ctx context.Context, remote string, branch string, setUpstream bool, force bool) (SyncPushResult, error)
	SyncPull(ctx context.Context, remote string, branch string) (SyncPullResult, error)

	// SyncReceive fetches and fast-forwards when — and only when — local is
	// strictly behind. It never merges: a divergence it meets is reported for
	// the foreground reconcile, never healed here. [LAW:no-silent-failure]
	SyncReceive(ctx context.Context, remote string, branch string) (SyncReceiveResult, error)

	// SyncCompact reclaims local storage at the requested depth, with no
	// remote involved — the entrypoint a workspace that never pushes needs.
	SyncCompact(ctx context.Context, mode GCMode) (CompactionOutcome, error)

	// CompactIfDue compacts only when the engine's own accounting says a pass
	// is owed, and reports what it did. The engine owns that judgment because
	// what makes a pass due is a fact about how it stores data — a caller that
	// re-derived it would have to know the engine's on-disk shape, which is
	// exactly what this contract exists to keep it from needing.
	// [LAW:decomposition]
	CompactIfDue(ctx context.Context) (CompactionOutcome, error)
	SyncCompactAndPush(ctx context.Context, remote string, branch string, setUpstream bool, force bool) (SyncPushResult, error)

	// GetSyncState and RecordSyncState carry the staleness marker across
	// commands: what the store's content was at the last sync, so a later
	// command can tell whether anything has happened since.
	GetSyncState(ctx context.Context) (SyncState, error)
	RecordSyncState(ctx context.Context, state SyncState) error
}

// Reconciler settles a divergence — the state in which local and remote have
// both moved and neither contains the other.
//
// A divergence is not a fact about storage; it is a fact about how an engine
// records history. An engine whose events are independently valid wherever
// they arrive never reaches this state, and offering these verbs would mean
// either panicking or reporting a reconciliation it did not perform. Absence
// here is the honest answer, and [Reconcile] is how a caller gets it.
type Reconciler interface {
	// SyncReconcile merges a divergence field-aware and replays it into linear
	// history, or — when a free-text field moved on both sides — holds the
	// conflict and commits nothing.
	SyncReconcile(ctx context.Context, remote string, branch string) (SyncReconcileResult, error)

	// SyncReconcileCombine settles an unrelated-history divergence by union,
	// keeping every issue from both sides.
	SyncReconcileCombine(ctx context.Context, remote string, branch string) (SyncReconcileResult, error)

	// SyncReconcileResolved finishes a reconcile that was held for prose,
	// splicing in the text a judge merged.
	SyncReconcileResolved(ctx context.Context, remote string, branch string, resolutions []merge.ProseResolution) (SyncReconcileResult, error)

	// SyncResolveUnrelated settles an unrelated-history divergence by taking
	// one side wholesale. It destroys the other side's unique issues, which is
	// why it takes an owner approval bound to this exact fork rather than a
	// bare confirmation flag.
	SyncResolveUnrelated(ctx context.Context, remote string, branch string, choice UnrelatedResolution, ownerApproval string) (SyncReconcileResult, error)

	// SyncResetToRemoteHead abandons local history for the peer's.
	SyncResetToRemoteHead(ctx context.Context, remote string, branch string) error
}

// Checkpointer names revert points and returns the store to them.
//
// It is separable from repair because a snapshot is a property of how an
// engine stores history, while repair is a property of what its data can get
// wrong: an append-only log can rewind to any position without owning a single
// consistency check, and a store with no history at all can still check itself.
type Checkpointer interface {
	CreateCheckpoint(ctx context.Context, prefix string) (Checkpoint, error)
	ListCheckpoints(ctx context.Context, prefix string) ([]Checkpoint, error)

	// PruneCheckpoints keeps the newest retain checkpoints under prefix and
	// drops the rest.
	PruneCheckpoints(ctx context.Context, prefix string, retain int) error
	ResetToCheckpoint(ctx context.Context, name string) error
}

// Repairer examines the store for faults of its own making and, where it can,
// fixes them.
//
// What counts as a fault is engine-specific — dangling references, orphaned
// history rows, and inverted rank positions are things a mutable relational
// store can be wrong about, and an engine that derives its state by folding an
// immutable log cannot be wrong about them in the same way. That is why this is
// a capability and not a duty every engine owes.
type Repairer interface {
	// Doctor examines and reports; it changes nothing.
	Doctor(ctx context.Context) (HealthReport, error)

	// FixIntegrity repairs the structural faults Doctor can name — dangling
	// rows, self-referential edges, edges stored in the wrong order — and
	// reports the state it left behind.
	//
	// [LAW:no-mode-explosion] It takes no "actually repair" flag. The
	// examine-only arm of such a flag is Doctor, so a bool here would be a
	// second spelling of a method that already exists, and the two would be
	// free to drift.
	FixIntegrity(ctx context.Context) (HealthReport, error)

	// FixRankInversions repairs orderings that contradict themselves and
	// reports how many it corrected. It exists only because rank is stored as
	// a fractional position that concurrent writers can invert; an engine that
	// derives order from rank intents has nothing to fix.
	FixRankInversions(ctx context.Context) (int, error)
}

// SchemaMigrator reports and moves the engine's stored shape.
//
// An engine offers this when its data has a shape that is versioned apart from
// the data itself. That is not universal: an engine that stores events as
// self-describing values has nothing to migrate and nothing to downgrade, and
// answering AppliedSchemaVersion with 0 would be a number pretending to be an
// answer.
type SchemaMigrator interface {
	// AppliedSchemaVersion reports the shape version the store is currently at.
	AppliedSchemaVersion(ctx context.Context) (int64, error)

	// Downgrade moves the store back to an older shape, so a binary that
	// predates the current one can open it.
	Downgrade(ctx context.Context, targetSchemaVersion int64) error
}

// Importer replaces the store's entire contents with an export.
//
// Its counterpart, Export, is core rather than a capability: the campaign's
// differential oracle compares two engines through it, so every engine must
// serialize itself. Only the import half is optional — an engine may be
// readable-out without being replaceable-in.
// (design-docs/event-store/design.md §migration)
type Importer interface {
	ReplaceFromExport(ctx context.Context, export model.Export) error
}

// RawExecutor runs an engine-native statement against the store.
//
// It exists so tests can plant states the contract cannot express — a
// corrupted row, a stale schema — and it is a capability rather than a
// contract method because the statement is written in the engine's own
// language. A test that needs it asks for it and skips when the engine does
// not offer it, which is the difference between a test that does not apply and
// a test that fails.
type RawExecutor interface {
	ExecRawForTest(ctx context.Context, query string, args ...any) error
}

// UnsupportedError is what an engine's silence about a capability sounds like.
//
// It is a typed value rather than a message so a caller can dispatch on it —
// skip a test, choose another path, render "not available on this engine" —
// without matching on text. [LAW:types-are-the-program]
type UnsupportedError struct {
	// Capability is the capability's name, as [Capability.Name] reports it.
	Capability string
	// Engine names the concrete engine that was asked, so a message says which
	// one came up short rather than blaming storage in the abstract.
	Engine string
}

func (e UnsupportedError) Error() string {
	return fmt.Sprintf("%s engine does not offer the %s capability", e.Engine, e.Capability)
}

// Capability is one optional engine capability, seen without its interface:
// enough to enumerate and render, not enough to call. Obtaining the interface
// itself needs the type, which is what the package-level capability values
// carry.
//
// The type is closed on purpose — only this package can mint one — so the set
// of things an engine may be asked for is fixed by the contract rather than
// grown by whoever is asking. [LAW:one-source-of-truth]
type Capability interface {
	// Name is the capability's stable identifier, used in messages and in
	// anything that renders which capabilities an engine has.
	Name() string

	// OfferedBy reports whether engine implements this capability. It answers
	// the enumeration question; a caller that intends to USE the capability
	// asks Of instead, and gets the interface with the answer.
	OfferedBy(engine Store) bool

	// unexported seals the interface to this package's implementation.
	unexported()
}

// capability binds a capability's name to the interface that defines it, so the
// name and the type it stands for cannot drift apart. [LAW:one-source-of-truth]
type capability[C any] struct{ name string }

func (c capability[C]) Name() string { return c.name }

func (c capability[C]) unexported() {}

// OfferedBy is derived from Of rather than repeating its type assertion, so
// the two can never disagree about what this engine offers.
// [LAW:single-enforcer]
func (c capability[C]) OfferedBy(engine Store) bool {
	_, err := c.Of(engine)
	return err == nil
}

// Of returns the engine's implementation of this capability, or an
// [UnsupportedError] naming both the capability and the engine that lacks it.
//
// This is the discovery mechanism, and it is a parser rather than a predicate:
// what comes back is a type that could not have existed before the check, so
// nothing downstream re-asks and nothing downstream can hold an unusable
// value. A bare "does it support sync" boolean would throw that proof away and
// leave every call site to assert the interface again and to invent its own
// wording for the absence. [LAW:parse-dont-validate] [LAW:single-enforcer]
func (c capability[C]) Of(engine Store) (C, error) {
	impl, ok := any(engine).(C)
	if !ok {
		var zero C
		return zero, UnsupportedError{Capability: c.name, Engine: fmt.Sprintf("%T", engine)}
	}
	return impl, nil
}

// The capabilities themselves. Each is the one place its name and its
// interface are tied together; ask for one with e.g.
//
//	syncer, err := storage.Sync.Of(engine)
var (
	Sync            = capability[Syncer]{name: "sync"}
	Reconcile       = capability[Reconciler]{name: "reconcile"}
	Checkpoints     = capability[Checkpointer]{name: "checkpoints"}
	Repair          = capability[Repairer]{name: "repair"}
	SchemaMigration = capability[SchemaMigrator]{name: "schema-migration"}
	Import          = capability[Importer]{name: "import"}
	TestSupport     = capability[RawExecutor]{name: "test-support"}
)

// all is the enumeration every listing derives from. A capability declared
// above and missing here would be invisible to Capabilities and Offered, so
// TestEveryCapabilityIsEnumerated checks the two against each other.
var all = []Capability{
	Sync,
	Reconcile,
	Checkpoints,
	Repair,
	SchemaMigration,
	Import,
	TestSupport,
}

// Capabilities returns every capability the contract defines, in a stable
// order. The slice is the caller's own — the enumeration behind it is not
// reachable from outside this package. [LAW:no-shared-mutable-globals]
func Capabilities() []Capability { return slices.Clone(all) }

// Offered returns the capabilities engine implements, in Capabilities order.
// It is derived from that one enumeration rather than declared beside it, so
// an engine cannot be described as offering something no caller can ask for.
// [LAW:one-source-of-truth]
func Offered(engine Store) []Capability {
	offered := make([]Capability, 0, len(all))
	for _, c := range all {
		if c.OfferedBy(engine) {
			offered = append(offered, c)
		}
	}
	return offered
}
