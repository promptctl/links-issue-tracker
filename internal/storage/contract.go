package storage

import (
	"context"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// IssueReader answers questions about recorded issue state without changing
// any of it.
//
// Every method here is a pure read in the caller's eyes: an engine may warm a
// cache or migrate a schema underneath, but two identical reads with no
// intervening mutation describe the same state.
type IssueReader interface {
	// GetIssue returns one issue's record. A missing id is NotFoundError, never
	// a zero Issue — absence is information, and an answer-shaped zero value
	// would destroy it. [LAW:parse-dont-validate]
	GetIssue(ctx context.Context, id string) (model.Issue, error)

	// GetIssueDetail returns the issue together with its comments, history, and
	// structural edges — the single-issue view, paid for in full. Callers that
	// need only the edges for many issues use GetRelationsByIDs instead.
	GetIssueDetail(ctx context.Context, id string) (model.IssueDetail, error)

	// ListIssues returns the issues the filter selects. With no SortBy the
	// order is rank ascending, ties broken by id — the canonical ordering, so
	// an unsorted listing is reproducible rather than merely arbitrary.
	ListIssues(ctx context.Context, filter ListIssuesFilter) ([]model.Issue, error)

	// ListChildren returns one epic's children in rank order. An id with no
	// children yields an empty slice; only an unreadable store is an error.
	ListChildren(ctx context.Context, parentID string) ([]model.Issue, error)

	// ListTopics returns the distinct non-empty topics live issues carry,
	// ascending. It is a derived vocabulary, never a stored one.
	ListTopics(ctx context.Context) ([]string, error)

	// ListAllEvents reads the whole issue history, oldest first. Export uses it
	// to serialize the history; claim derivation uses it because a claim is a
	// reading of the history and there is no narrower slice that answers the
	// question — the establishing event that decides a lane's holder can be
	// arbitrarily old, so a recency cutoff would silently drop exactly the
	// claims it was meant to speed up.
	ListAllEvents(ctx context.Context) ([]model.IssueEvent, error)

	// LocalIssueCount reports how many issues this store holds. It is the
	// adopt-safety signal for `lit init`: a store with zero issues has no work
	// to lose. A store that has never been written is a true "no issues yet"
	// state and reports 0 rather than erroring — the absence is a real domain
	// value, not a fault. [LAW:no-defensive-null-guards]
	LocalIssueCount(ctx context.Context) (int64, error)
}

// IssueWriter creates and changes issue records.
type IssueWriter interface {
	// CreateIssue records a new issue and returns it as stored, id minted.
	CreateIssue(ctx context.Context, in CreateIssueInput) (model.Issue, error)

	// Apply is the single execution path for issue-record changes: the Change
	// value carries all the variability, so there is no second mutation verb to
	// drift from this one. A pure no-op records nothing — history reflects
	// actual mutations — and an illegal transition is refused rather than
	// silently ignored. [LAW:dataflow-not-control-flow]
	Apply(ctx context.Context, id string, c Change) (model.Issue, error)
}

// CommentStore records and removes the free-text commentary attached to an
// issue.
type CommentStore interface {
	// AddComment returns the new comment and the issue as it stands after the
	// write, so a caller that must render both does not re-read to get the
	// second one.
	AddComment(ctx context.Context, in AddCommentInput) (model.Comment, model.Issue, error)

	// DeleteComment removes one comment and returns what was removed, so the
	// deletion is reportable without a prior read.
	DeleteComment(ctx context.Context, commentID string) (model.Comment, error)
}

// LabelStore maintains the label set attached to an issue.
//
// Every mutating method returns the issue's resulting label set rather than
// nothing: the post-state is what a caller renders, and returning it here is
// what keeps "what labels does it have now" from becoming a follow-up read
// that a concurrent writer could answer differently.
type LabelStore interface {
	AddLabel(ctx context.Context, in AddLabelInput) ([]string, error)
	RemoveLabel(ctx context.Context, issueID, labelName string) ([]string, error)
	// ReplaceLabels sets the whole set at once — the authored-document path,
	// where the file states the labels an issue has rather than a delta.
	ReplaceLabels(ctx context.Context, issueID string, labels []string, createdBy string) error
	ListLabels(ctx context.Context, issueID string) ([]string, error)
}

// RelationStore reads and writes the structural edges between issues,
// parentage included.
//
// Parentage gets its own two verbs rather than riding AddRelation because it
// is the one edge with an arity rule — an issue has at most one parent — and
// SetParent is where replacing an existing one happens as a single act.
type RelationStore interface {
	AddRelation(ctx context.Context, in AddRelationInput) (model.Relation, error)
	RemoveRelation(ctx context.Context, srcID, dstID string, relType model.RelationType) error

	// ListRelationsForIssue returns every edge incident to the issue in either
	// direction, oldest first, optionally narrowed to the named types.
	ListRelationsForIssue(ctx context.Context, issueID string, types ...model.RelationType) ([]model.Relation, error)

	// GetRelationsByIDs is the batch neighborhood read: one call for many
	// issues, counterparts hydrated. It exists so the ready pipeline and the
	// epic view do not pay GetIssueDetail's per-issue comment and history cost
	// for edges they could read together.
	GetRelationsByIDs(ctx context.Context, ids []string) (map[string]IssueRelations, error)

	// SetParent wires a child under a parent, replacing any existing parent in
	// one act, and returns the edge it wrote. Both endpoints must exist and an
	// issue may not be its own parent.
	SetParent(ctx context.Context, in SetParentInput) (model.Relation, error)

	// ClearParent detaches a child from its parent. Clearing a child that has
	// no parent is NotFoundError, not a silent success: the caller asked to
	// remove an edge, and reporting removal of an edge that was never there
	// would be an answer-shaped void. [LAW:no-silent-failure]
	ClearParent(ctx context.Context, childID string) error
}

// Ranker moves issues within lit's one total order, and does it only through
// relative intents.
//
// Intents, never positions, is the load-bearing choice: "above Y" survives
// everyone else reordering around it, where a stored position is a lie the
// moment another writer moves anything. The five verbs here are the whole
// vocabulary — an engine that let a caller write a rank value directly would
// hand back the concurrency problem the intents exist to avoid.
// (design-docs/event-store/design.md §rank)
//
// The two anchored verbs report a RankMove because rank frames are nested:
// an intent naming two issues in different epics is honored against the
// containing ancestors that ARE comparable, and the substitution is returned
// so the caller can surface it. [LAW:no-silent-failure]
type Ranker interface {
	RankAbove(ctx context.Context, issueID, targetID string) (RankMove, error)
	RankBelow(ctx context.Context, issueID, targetID string) (RankMove, error)
	RankToTop(ctx context.Context, issueID string) error
	RankToBottom(ctx context.Context, issueID string) error

	// RankSet imposes a total order on the named issues at once, returning
	// which representative each name resolved to.
	RankSet(ctx context.Context, ids []string) ([]RankSetResolution, error)
}

// BulkWriter applies a batch of authored issue documents in one call,
// resolving the batch's internal references before anything is written.
//
// Failure is compensated, not transactional, and the contract is about what
// the caller is told rather than about atomicity: a batch that fails partway
// undoes the issues it created, and an error names every one it could not undo
// — so a partial application is always reported, never inferred.
// [LAW:no-silent-failure] Updates a mixed batch already applied are not
// reverted, which is why the error text is the caller's only complete account
// of what happened.
type BulkWriter interface {
	// BulkApply applies a mixed create/update batch, resolving intra-batch
	// references by LocalID.
	BulkApply(ctx context.Context, prefix, actor string, specs []BulkIssueSpec) (BulkApplyResult, error)

	// ImportTree creates a whole issue tree, returning the local-ID → real-ID
	// mapping the caller needs to talk about what it just made.
	ImportTree(ctx context.Context, prefix string, specs []ImportTreeSpec) (ImportTreeResult, error)
}

// Exporter serializes the store's entire contents as one value.
//
// It is the campaign's differential oracle surface: the one read through which
// two engines' whole states are comparable, which is why it is core rather
// than an exchange capability. (design-docs/event-store/design.md §migration)
type Exporter interface {
	Export(ctx context.Context) (model.Export, error)
}

// Attributor names the checkout whose work the engine is about to record.
//
// Attribution is the store's, not each mutation's: an engine stamps it at its
// single event-insertion point, so "every work mutation carries its checkout's
// attribution pair" is a property of the write path rather than an obligation
// on every call site. An empty token leaves the engine unattributed rather
// than half-attributed — that is the contract for a read-mode open of a
// checkout that has never mutated, and it is why this takes no presence flag.
type Attributor interface {
	AttributeTo(streamToken string)
}

// Store is the whole storage contract: everything lit needs from any engine,
// and nothing an engine happens to have.
//
// The composition is not decoration. A caller that only reads declares
// IssueReader and cannot mutate; the S1 dual-write decorator wraps this whole
// interface; a new engine implements these pieces one joint at a time. What
// is deliberately absent is as load-bearing as what is here — sync, schema
// migration, checkpoints, fsck, and raw test access are engine capabilities,
// named separately, so an engine with no schema and no remote does not
// inherit obligations it has no meaning for.
type Store interface {
	IssueReader
	IssueWriter
	CommentStore
	LabelStore
	RelationStore
	Ranker
	BulkWriter
	Exporter
	Attributor

	// Close releases whatever the engine holds. It is in the contract because
	// callers must always be able to hand the resource back without knowing
	// what it was — for the Dolt engine that includes the workspace lock, and a
	// close whose error is discarded strands it.
	Close() error
}
