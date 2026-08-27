package store

import "github.com/promptctl/links-issue-tracker/internal/storage"

// This file is where the Dolt engine meets lit's storage contract, and it is
// deliberately the only place in this package that knows the contract exists.
//
// The assertion below is the load-bearing line: it fails the build the moment
// this engine stops satisfying [storage.Store], which is what makes the
// contract a constraint on the engine rather than a document about it.
// [LAW:types-are-the-program]
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
var _ storage.Store = (*Store)(nil)

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
)

// A constant cannot be aliased, so the placement vocabulary is re-exported by
// value. The values are the contract's, never redeclared: writing `iota` here
// would let the two orderings drift silently.
const (
	RankBottom = storage.RankBottom
	RankTop    = storage.RankTop
)
