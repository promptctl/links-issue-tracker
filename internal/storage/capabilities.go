package storage

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/historywindow"
	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
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
// SyncCompact plus a push. Any engine offering this interface therefore
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
	SyncAddRemote(ctx context.Context, name RemoteName, url string) error
	// SyncRemoveRemote takes any name the engine holds, as SyncListRemotes
	// lists it, not only a RemoteName: an older lit wrote names a RemoteName
	// refuses, and removing them is how the reconcile clears them. A name the
	// engine no longer holds is already removed.
	SyncRemoveRemote(ctx context.Context, name string) error
	// SyncSetRemoteURL points the remote name at url. The remote stays the
	// same remote: unlike a removal, it keeps its fork points.
	SyncSetRemoteURL(ctx context.Context, name RemoteName, url string) error
	SyncListRemotes(ctx context.Context) ([]SyncRemote, error)

	// SyncStatus reports the local side only — build, position, pending
	// changes, peers — and contacts no network.
	SyncStatus(ctx context.Context) (SyncStatusReport, error)

	// SyncFreshness reports the local branch's position against a peer as of
	// the last fetch or push. It reads local refs; it never contacts the
	// network, which is what lets a read-only command call it.
	SyncFreshness(ctx context.Context, remote RemoteName, branch string) (SyncFreshness, error)

	// SyncFetch fetches remote and prunes: every tracking ref of a branch the
	// remote no longer has goes, with its fork point, so nothing keeps a
	// deleted branch's history reachable. PrunedRefs says what went.
	SyncFetch(ctx context.Context, remote RemoteName) error

	// PrunedRefs is every branch a fetch since the engine opened found gone
	// from its remote, oldest first.
	PrunedRefs() []PrunedRef

	// SyncPushFromClone is the push of a store that is a frozen clone of a
	// live one — the on-change mirror's push. Nothing serializes it against
	// the live store's own network traffic, so a push the remote rejects is
	// re-checked before it is reported: when the remote already carries this
	// store's HEAD (a concurrent push from the live store landed a descendant
	// of it), the result is Superseded rather than an error, because the goal
	// state holds. A rejection the re-check does not explain is the error.
	SyncPushFromClone(ctx context.Context, remote RemoteName, branch string, setUpstream bool, force bool) (SyncPushResult, error)

	// SyncRemoteMirrorHolds reports whether this store's local mirror of the
	// remote's data holds every one of commits — git commit ids as the remote
	// advertises them. It contacts no network. After a push this store made
	// that landed without being superseded, the mirror holds exactly what this
	// store wrote or had already read, so an advertised commit the mirror holds
	// is this store's own push and not a peer's later one: that is what lets
	// the push record what it left the remote advertising without a fetch.
	SyncRemoteMirrorHolds(ctx context.Context, remote RemoteName, commits []string) (bool, error)
	SyncPull(ctx context.Context, remote RemoteName, branch string) (SyncPullResult, error)

	// SyncReceive fetches and fast-forwards when — and only when — local is
	// strictly behind. It never merges: a divergence it meets is reported for
	// the foreground reconcile, never healed here. [LAW:no-silent-failure]
	SyncReceive(ctx context.Context, remote RemoteName, branch string) (SyncReceiveResult, error)

	// SyncSettleReceived is SyncReceive's second half alone: it reads the
	// tracking ref as it stands — a fetch that ran on a clone and was landed
	// here — and fast-forwards on the same terms. It contacts no network.
	SyncSettleReceived(ctx context.Context, remote RemoteName, branch string) (SyncReceiveResult, error)

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
	SyncCompactAndPush(ctx context.Context, remote RemoteName, branch string, setUpstream bool, force bool) (SyncPushResult, error)

	// SyncFollowReRoot brings the local branch into the remote's history when
	// another checkout re-rooted it, reading the tracking ref as the last fetch
	// left it; it contacts no network. Every sync verb that settles or pushes
	// follows on its own; this is for a caller that fetched and settles
	// nothing, so the freshness a later read reports never shows a re-root it
	// could follow as a divergence. entry names that caller.
	SyncFollowReRoot(ctx context.Context, remote RemoteName, branch string, entry SyncEntry) error

	// FollowedReRoots is every re-root the engine applied to its own branch,
	// since it opened, to follow a remote another checkout re-rooted: the sync
	// verbs follow before they settle, so none of them meets such a remote as
	// unrelated histories. An engine whose history is never re-rooted follows
	// nothing and returns none.
	FollowedReRoots() []FollowedReRoot

	// SyncKeepHistoryBound keeps the sync branch's history inside window
	// once pushed has landed on remote's branch, whichever push landed it,
	// and returns pushed with the bound it kept as Bound and the bound's
	// account joined to its Maintenance.
	// Below the bound it reads the remote head's height and nothing else;
	// once the height reaches it, it publishes a re-root of that head, runs
	// the remote's GC, and follows the re-root. The push landed whatever the
	// bound did, so the bound fails nothing: where it stopped is on its
	// record (HistoryBounds) and in the account.
	SyncKeepHistoryBound(ctx context.Context, remote RemoteName, branch string, window historywindow.Window, pushed SyncPushResult) SyncPushResult

	// HistoryBounds is what every SyncKeepHistoryBound since the engine
	// opened did, oldest first: the remote head's height every time, and the
	// re-root it published once that height reached the bound. An engine
	// whose history has no bound to keep returns none.
	HistoryBounds() []HistoryBound

	// ForkPointRecords is what the engine did to each fork point since it
	// opened, oldest first: every fetch before it moved the tracking ref,
	// every collection below a cut, and every removal of a remote. A
	// fork point is the remote head this checkout last knew while the two
	// still shared history: the base a reconcile merges against once the
	// remote re-roots above every commit this checkout holds.
	ForkPointRecords() []ForkPointRecord

	// RemoteRemovals is every SyncRemoveRemote attempt since the engine
	// opened, oldest first.
	RemoteRemovals() []RemoteRemovalRecord

	// ReconcileBases is the base every reconcile since the engine opened
	// decided on, and where it came from, oldest first.
	ReconcileBases() []ReconcileBase

	// StrandedWritesCommits is every commit the engine landed since it opened
	// over writes a failed commit left in the working set, oldest first.
	StrandedWritesCommits() []StrandedWritesCommit

	// SettledLastWrites is every settle's write of its settled.last record
	// since the engine opened, oldest first.
	SettledLastWrites() []SettledLastWrite

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
	SyncReconcile(ctx context.Context, remote RemoteName, branch string) (SyncReconcileResult, error)

	// SyncReconcileCombine settles an unrelated-history divergence by union,
	// keeping every issue from both sides.
	SyncReconcileCombine(ctx context.Context, remote RemoteName, branch string) (SyncReconcileResult, error)

	// SyncReconcileResolved finishes a reconcile that was held for prose,
	// landing the text a judge merged as actor's edit, recorded in history.
	SyncReconcileResolved(ctx context.Context, remote RemoteName, branch string, resolutions []merge.ProseResolution, actor string) (SyncReconcileResult, error)

	// SyncResolveUnrelated settles an unrelated-history divergence by taking
	// one side wholesale. It destroys the other side's unique issues, which is
	// why it takes an owner approval bound to this exact fork rather than a
	// bare confirmation flag.
	SyncResolveUnrelated(ctx context.Context, remote RemoteName, branch string, choice UnrelatedResolution, ownerApproval string) (SyncReconcileResult, error)

	// SyncResetToRemoteHead abandons local history for the peer's.
	SyncResetToRemoteHead(ctx context.Context, remote RemoteName, branch string) error
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
	// reports how many issues it moved. It exists only because rank is stored
	// as a fractional position that concurrent writers can invert; an engine
	// that derives order from rank intents has nothing to fix.
	//
	// The repair keeps the existing order wherever the edges allow: an issue
	// falls behind one that stood after it only while it waits on a
	// dependency, and a second run over a repaired store writes nothing.
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
	// predates the current one can open it. It reports what it did on every
	// return once it has read the applied version — a success, a refusal, and
	// a failure part-way alike — and nil only when it failed before that read.
	Downgrade(ctx context.Context, targetSchemaVersion int64) (*Downgraded, error)

	// Migrated reports what opening this handle did to the stored shape.
	Migrated() Migrated
}

// Migrated is what an open did to the stored shape: whether it had to build
// the database first, the recovery snapshot it took before migrating, "" when
// it found nothing to migrate, why rolling older migration snapshots or
// checkpoints off after its work committed failed, if it did, and the newer
// schema it served, if it did.
// [LAW:types-are-the-program] PruneErr is a field, not the open's error: each
// prune runs after the work it trails has committed, so its failure leaves the
// store at the shape this binary reads. An open that failed on it would stop
// the command over a leftover snapshot or checkpoint, and the next open,
// finding nothing to migrate, would succeed without retrying the prune.
type Migrated struct {
	Bootstrap   Bootstrap
	Snapshot    string
	PruneErr    error
	SchemaAhead SchemaAhead
}

// SchemaAhead is a workspace schema newer than the binary that served it: the
// workspace's schema version; the oldest a lit must support to write the
// workspace, 0 when no migration set one; why that minimum could not be read
// as a schema version, if it could not; and whether the binary served it for
// reading only, since it may not write to it. The zero value is a workspace
// that was not ahead.
type SchemaAhead struct {
	WorkspaceSchema    int64
	MinWriterSchema    int64
	MinWriterSchemaErr error
	ReadOnly           bool
}

// EventRecord is the schema as event facts, all zero when it was not ahead.
func (a SchemaAhead) EventRecord() map[string]any {
	return map[string]any{
		"migration.schema_ahead":            a.WorkspaceSchema,
		"migration.min_writer_schema":       a.MinWriterSchema,
		"migration.min_writer_schema_error": telemetry.ErrorText(a.MinWriterSchemaErr),
		"migration.read_only":               a.ReadOnly,
	}
}

// Bootstrap is what an open did to make its database exist.
// [LAW:types-are-the-program] one value, not a built flag beside a discarded
// flag: a discarded leftover is a fact about a build, so "discarded but built
// nothing" has no value to be.
type Bootstrap int

const (
	// BootstrapNone: the database was already there, or the open is one that
	// never builds.
	BootstrapNone Bootstrap = iota
	// BootstrapBuilt: the open found no database and built it.
	BootstrapBuilt
	// BootstrapBuiltOverDeadBuild: the open built the database after
	// discarding what a builder that died mid-build left behind.
	BootstrapBuiltOverDeadBuild
)

// Built reports whether the open built the database.
func (b Bootstrap) Built() bool {
	return b != BootstrapNone
}

// String is the bootstrap as its event fact value.
func (b Bootstrap) String() string {
	switch b {
	case BootstrapNone:
		return "none"
	case BootstrapBuilt:
		return "built"
	case BootstrapBuiltOverDeadBuild:
		return "built_over_dead_build"
	default:
		return fmt.Sprintf("Bootstrap(%d)", int(b))
	}
}

// EventRecord is the bootstrap as event facts.
func (b Bootstrap) EventRecord() map[string]any {
	return map[string]any{"bootstrap": b.String()}
}

// OpenMigration is what opening st did to its stored shape. An engine whose
// data has no versioned shape has nothing to migrate, so it migrated nothing.
func OpenMigration(st Store) Migrated {
	migrator, err := SchemaMigration.Of(st)
	if err != nil {
		return Migrated{}
	}
	return migrator.Migrated()
}

// EventRecord is the open's migration as event facts.
func (m Migrated) EventRecord() map[string]any {
	facts := m.Bootstrap.EventRecord()
	maps.Copy(facts, MigrationRecoveryFacts(m.Snapshot, m.PruneErr))
	maps.Copy(facts, m.SchemaAhead.EventRecord())
	return facts
}

// MigrationRecoveryFacts is the recovery snapshot an open took, "" when none,
// and why its prunes failed, as event facts. A successful open and a failed
// one record them under these same keys. [LAW:one-source-of-truth]
func MigrationRecoveryFacts(snapshot string, pruneErr error) map[string]any {
	return map[string]any{
		"migration.snapshot":    snapshot,
		"migration.prune_error": telemetry.ErrorText(pruneErr),
	}
}

// Downgraded is what a Downgrade did: the shape version it read under the lock
// and decided from, how many down-migrations it applied, the recovery snapshot
// it took first, and why rolling older recovery snapshots off afterwards
// failed, if it did. A Downgrade that stops before a step leaves that step's
// field zero: a refusal or a target equal to the applied version reverses
// nothing and takes no snapshot.
// [LAW:one-source-of-truth] the read that made the decision is the one
// reported; a caller never reads the version again to describe it.
// [LAW:types-are-the-program] PruneErr is a field, not Downgrade's error: the
// prune runs after the reverse has committed, so its failure leaves the
// workspace at the target shape. Downgrade's error means the shape is not
// where the caller asked for it; a caller that stops on that error must not
// stop on a leftover snapshot.
type Downgraded struct {
	From     int64
	Reversed int
	Snapshot string
	PruneErr error
}

// EventRecord is the downgrade as event facts and counts, so the success and
// every failure after the read name the same facts under the same keys.
func (d Downgraded) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
			"downgrade.workspace_schema":     d.From,
			"downgrade.snapshot":             d.Snapshot,
			"downgrade.snapshot_prune_error": telemetry.ErrorText(d.PruneErr),
		}, map[string]int{
			"downgrade.reversed": d.Reversed,
		}
}

// Importer replaces the store's entire contents with an export, and says what
// it changed on the way in to land it under the store's rules (Landing).
//
// Its counterpart, Export, is core rather than a capability: the campaign's
// differential oracle compares two engines through it, so every engine must
// serialize itself. Only the import half is optional — an engine may be
// readable-out without being replaceable-in.
// (design-docs/event-store/design.md §migration)
type Importer interface {
	ReplaceFromExport(ctx context.Context, export model.Export) (Landing, error)
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
