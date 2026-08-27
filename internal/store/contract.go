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
// The aliases beneath it are a relocation, not a second copy. Every input and
// result type lit's storage vocabulary needs now lives in internal/storage, so
// the contract can name them without importing an engine; the names stay
// spelled `store.X` here so that carving the seam rewired no caller — which is
// this migration state's whole gate. A Go alias is the same type, not a
// parallel one, so `errors.As(err, &store.NotFoundError{})` at any call site
// still matches an error the contract package minted. [LAW:one-source-of-truth]
//
// These aliases are scaffolding with a demolition date: links-store-seam-q35v.4
// points app and CLI at the contract package directly, and this whole block
// goes with that flip. The authored-file parsers still exported here —
// ParseBulkSpecs, ParseImportTreeSpecs, ParseSortSpecs — produce contract types
// and are the same kind of scaffolding: they move when their callers do, not
// before, because relocating them now would rewire callers this state is
// defined by not rewiring.
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

type (
	NotFoundError     = storage.NotFoundError
	ValidationError   = storage.ValidationError
	RankPlacement     = storage.RankPlacement
	CreateIssueInput  = storage.CreateIssueInput
	UpdateIssueInput  = storage.UpdateIssueInput
	Change            = storage.Change
	SortSpec          = storage.SortSpec
	ListIssuesFilter  = storage.ListIssuesFilter
	AddCommentInput   = storage.AddCommentInput
	AddLabelInput     = storage.AddLabelInput
	AddRelationInput  = storage.AddRelationInput
	SetParentInput    = storage.SetParentInput
	IssueRelations    = storage.IssueRelations
	RankMove          = storage.RankMove
	RankSetResolution = storage.RankSetResolution
	BulkIssueSpec     = storage.BulkIssueSpec
	BulkApplyResult   = storage.BulkApplyResult
	ImportTreeSpec    = storage.ImportTreeSpec
	ImportTreeResult  = storage.ImportTreeResult

	// The capability vocabulary, relocated by links-store-seam-q35v.2 for the
	// same reason and on the same terms as the core vocabulary above: a
	// capability interface can only name types that are the contract's.
	SyncState           = storage.SyncState
	SyncRemote          = storage.SyncRemote
	SyncStatusRow       = storage.SyncStatusRow
	SyncStatusReport    = storage.SyncStatusReport
	SyncFreshnessState  = storage.SyncFreshnessState
	SyncFreshness       = storage.SyncFreshness
	SyncReceiveState    = storage.SyncReceiveState
	SyncReceiveResult   = storage.SyncReceiveResult
	SyncPullState       = storage.SyncPullState
	SyncPullResult      = storage.SyncPullResult
	SyncPushResult      = storage.SyncPushResult
	SyncReconcileState  = storage.SyncReconcileState
	SyncReconcileResult = storage.SyncReconcileResult
	UnrelatedInventory  = storage.UnrelatedInventory
	UnrelatedResolution = storage.UnrelatedResolution
	Checkpoint          = storage.Checkpoint
	HealthReport        = storage.HealthReport
)

// A constant cannot be aliased, so the placement vocabulary is re-exported by
// value. The values are the contract's, never redeclared: writing `iota` here
// would let the two orderings drift silently.
const (
	RankBottom = storage.RankBottom
	RankTop    = storage.RankTop

	SyncNeverSynced = storage.SyncNeverSynced
	SyncUpToDate    = storage.SyncUpToDate
	SyncAhead       = storage.SyncAhead
	SyncBehind      = storage.SyncBehind
	SyncDiverged    = storage.SyncDiverged

	SyncReceiveUpToDate      = storage.SyncReceiveUpToDate
	SyncReceiveFastForwarded = storage.SyncReceiveFastForwarded
	SyncReceiveAhead         = storage.SyncReceiveAhead
	SyncReceiveDiverged      = storage.SyncReceiveDiverged
	SyncReceiveNeverSynced   = storage.SyncReceiveNeverSynced

	SyncPullUpToDate      = storage.SyncPullUpToDate
	SyncPullFastForwarded = storage.SyncPullFastForwarded
	SyncPullLinearized    = storage.SyncPullLinearized
	SyncPullProsePending  = storage.SyncPullProsePending
	SyncPullUnrelated     = storage.SyncPullUnrelated
	SyncPullAhead         = storage.SyncPullAhead
	SyncPullNeverSynced   = storage.SyncPullNeverSynced

	SyncReconcileNotDiverged  = storage.SyncReconcileNotDiverged
	SyncReconcileLinearized   = storage.SyncReconcileLinearized
	SyncReconcileProsePending = storage.SyncReconcileProsePending
	SyncReconcileUnrelated    = storage.SyncReconcileUnrelated
	SyncReconcileTookLocal    = storage.SyncReconcileTookLocal
	SyncReconcileTookRemote   = storage.SyncReconcileTookRemote
	SyncReconcileCombined     = storage.SyncReconcileCombined

	TakeLocal  = storage.TakeLocal
	TakeRemote = storage.TakeRemote
)
