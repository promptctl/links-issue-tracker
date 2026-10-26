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

	// ListIssues returns the issues the filter selects, ordered by SortBy and
	// then — always, as the last key — by id ascending. With no SortBy the
	// order is rank ascending — tree order, see [RankAncestry] — ties broken by
	// id: the canonical ordering, so an unsorted listing is reproducible rather
	// than merely arbitrary.
	//
	// The trailing id key is contract, not an engine's convenience. Without it
	// a sort on any field with duplicate values (two issues sharing a title, a
	// priority, an assignee) leaves the tied rows in whatever order the engine
	// happened to hold them, and two engines answering the same listing would
	// disagree — which the campaign's differential oracle reads as divergence
	// rather than as the under-specification it would actually be.
	// [LAW:no-silent-failure]
	//
	// SortBy may name only a key [sortKeys] holds; see it for what each key
	// orders. [IssueOrdering] reads that table and applies the two rules above,
	// and it is where an engine gets all of them rather than restating them.
	ListIssues(ctx context.Context, filter ListIssuesFilter) ([]model.Issue, error)

	// ListTopics returns the distinct non-empty topics live issues carry,
	// ascending. It is a derived vocabulary, never a stored one.
	ListTopics(ctx context.Context) ([]string, error)

	// ListIssueIdentities returns every issue's id and topic — archived and
	// deleted included, id ascending — and nothing else. It is the material an
	// id was minted from, for a caller that asks what the ids in a store carry
	// (the prefix census). Nothing is hydrated, so it never walks the
	// hierarchy, and it answers in a store whose parent links loop, where
	// ListIssues refuses (ParentCycleError); `lit doctor` reads it before it has any other
	// way to know the hierarchy is sound.
	ListIssueIdentities(ctx context.Context) ([]IssueIdentity, error)

	// ListAllEvents reads the whole issue history, oldest first — by creation
	// time, ties broken by event id ascending. Export uses it to serialize the
	// history; claim derivation uses it because a claim is a reading of the
	// history and there is no narrower slice that answers the question — the
	// establishing event that decides a lane's holder can be arbitrarily old,
	// so a recency cutoff would silently drop exactly the claims it was meant
	// to speed up.
	//
	// The id tie-break is stated because "oldest first" alone does not settle a
	// tie, and a coarse clock produces ties routinely: two mutations inside one
	// tick share a timestamp, and an engine free to order them as it likes is
	// an engine the oracle cannot diff. It is deliberately NOT a happens-before
	// claim — event ids are random, so same-tick events come back in an order
	// unrelated to the order they were recorded in. Both engines are wrong
	// about causality in the same way on purpose, so that they are wrong
	// identically; ordering that actually carries happens-before arrives with
	// the event store's Lamport positions
	// (design-docs/event-store/design.md §events), which supersedes this rule
	// rather than refining it.
	ListAllEvents(ctx context.Context) ([]model.IssueEvent, error)

	// ListEvents reads one issue's history, ordered exactly as ListAllEvents
	// orders the whole of it. It exists because "who does the record say holds
	// this ticket" is a question about one issue, and the only other reader
	// that carries one issue's history is GetIssueDetail — the single-issue
	// view paid for in full, hydrating relations, comments and children a
	// caller after the claimant never looks at.
	ListEvents(ctx context.Context, issueID string) ([]model.IssueEvent, error)

	// LocalIssueCount reports how many issues this store holds. It is the
	// adopt-safety signal for `lit init`: a store with zero issues has no work
	// to lose. A store that has never been written is a true "no issues yet"
	// state and reports 0 rather than erroring — the absence is a real domain
	// value, not a fault. [LAW:no-defensive-null-guards]
	LocalIssueCount(ctx context.Context) (int64, error)
}

// IssueWriter creates, changes, and purges issue records.
type IssueWriter interface {
	// CreateIssue records a new issue and returns it as stored, id minted.
	CreateIssue(ctx context.Context, in CreateIssueInput) (model.Issue, error)

	// Apply is the single execution path for issue-record changes: the Change
	// the plan decides carries all the variability, so there is no second
	// mutation verb to drift from this one. The plan is decided on the issue as
	// the write finds it, under the write's own hold, so a change derived from
	// the issue's current state cannot be planned against a state another
	// writer has since moved. A pure no-op records nothing — history reflects
	// actual mutations — and an illegal transition is refused rather than
	// silently ignored. [LAW:dataflow-not-control-flow]
	// [LAW:no-ambient-temporal-coupling]
	Apply(ctx context.Context, id string, plan ChangePlan) (Applied, error)

	// Purge permanently removes an issue: its row, comments, labels, history,
	// and every edge to or from it, in one write, leaving a model.Purge record
	// that names the ticket by its id and created_at and holds nothing of its
	// content. Delete is the reversible removal; Purge is the one that cannot
	// be undone. Other issues are not edited — a mention of the id in their
	// text, follows_up, or redirect target stays. The id is never minted
	// again, and every merge drops the ticket from both sides
	// (model.Export.ApplyPurges), so the purge reaches every store the record
	// reaches. A missing id is NotFoundError, and a purge that would leave a
	// ticket under the issue outside any epic is refused
	// (RejectPurgeOfOccupiedContainer); a child epic becomes top-level.
	Purge(ctx context.Context, in PurgeInput) (PurgeResult, error)
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

// LabelStore reads the label set attached to an issue. The set is an issue
// field, so it is written through Apply — AddLabel and RemoveLabel are the
// plans the label verbs apply — and every write of it is recorded.
// [LAW:single-enforcer]
type LabelStore interface {
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

	// ParentByChild maps every issue that sits under a parent to that parent's
	// id, archived and deleted issues included at either end; a parentless
	// issue is absent. It reads the parent-child edges and nothing else, for a
	// caller that already holds every issue and needs only to know what each
	// one is under — claim evidence, which places every issue in its lane.
	// Asking GetRelationsByIDs that of the whole store re-reads and re-hydrates
	// every issue and every edge, and at a few thousand issues that is a third
	// of `lit next`. A child restored data gives two parents maps to the lower
	// parent id, deleted or not. That is not always the parent [RankAncestry]
	// frames it by, since a deleted parent frames nothing; doctor reports the
	// two-parent child either way.
	ParentByChild(ctx context.Context) (map[string]string, error)

	// DependencyOrder reads the blocked-by edges between live issues in tree
	// order: the one answer to which of them are rank inversions.
	DependencyOrder(ctx context.Context) (DependencyOrder, error)

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
// Every verb here reports what it resolved to, because rank frames are nested
// and the naive reading of each verb is the wrong one. The two anchored verbs
// report a RankMove: an intent naming two issues in different epics is honored
// against the containing ancestors that ARE comparable. The two edge verbs
// report a RankEnd: "the top" is the top of the issue's own frame, so an epic's
// child sent there leads its siblings and moves nowhere in the queue at large.
// Both substitutions are returned rather than assumed, so a caller can surface
// them. [LAW:no-silent-failure]
type Ranker interface {
	RankAbove(ctx context.Context, issueID, targetID string) (RankMove, error)
	RankBelow(ctx context.Context, issueID, targetID string) (RankMove, error)
	RankToTop(ctx context.Context, issueID string) (RankEnd, error)
	RankToBottom(ctx context.Context, issueID string) (RankEnd, error)

	// RankSet imposes a total order on the named issues at once, returning
	// which representative each name resolved to.
	RankSet(ctx context.Context, ids []string) (RankSetResult, error)
}

// BulkWriter applies a batch of authored issue documents in one call,
// resolving the batch's internal references before anything is written.
//
// A refused batch writes nothing: a refusal at any write — whichever rule
// refused it, judged before the first write or by the write itself — leaves
// the store as it found it, with no create left behind as a deleted
// issue and no update, edge or rank key kept. A refused batch returns the zero
// result beside its error, and observers hear a batch's changes only once it
// has landed. An engine that cannot put the store back says so in its error
// and returns what stays written beside it, never the zero result.
// [LAW:no-silent-failure]
type BulkWriter interface {
	// BulkApply applies a mixed create/update batch, resolving intra-batch
	// references by LocalID.
	BulkApply(ctx context.Context, prefix, actor string, specs []BulkIssueSpec) (BulkApplyResult, error)

	// ImportTree creates a whole issue tree, returning the local-ID → real-ID
	// mapping the caller needs to talk about what it just made.
	ImportTree(ctx context.Context, prefix, actor string, specs []ImportTreeSpec) (ImportTreeResult, error)
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

// ChangeObserver lets a caller hear every Change the engine applies, with the
// issue as that Change left it. Writes that are not a Change, such as a create,
// a purge or a restore, are not heard.
//
// Like attribution, this is the store's and not each caller's: an engine calls
// its observers from its single change path, so a Change made inside a bulk
// batch is heard exactly as a lone Apply is, once the batch has landed. A
// refused batch made no change, so nothing of it is heard. Hearing every Change
// is then a property of the write path rather than an obligation on every
// command that writes. An observer is called from inside that path, which may
// still hold the engine's lock, so it must not call back into the store.
// [LAW:single-enforcer]
type ChangeObserver interface {
	// ObserveChanges adds observe to the observers called after each change
	// this engine applies from here on.
	ObserveChanges(observe func(Change, model.Issue))
}

// Store is the whole storage contract: everything lit needs from any engine,
// and nothing an engine happens to have.
//
// The composition is not decoration. A caller that only reads declares
// IssueReader and cannot mutate; the S1 dual-write decorator wraps this whole
// interface; a new engine implements these pieces one joint at a time. What
// is deliberately absent is as load-bearing as what is here — sync, schema
// migration, checkpoints, repair, and raw test access are engine capabilities,
// named separately as [Syncer], [Reconciler], [Checkpointer], [Repairer],
// [SchemaMigrator], [Importer], and [RawExecutor], so an engine with no schema
// and no remote does not inherit obligations it has no meaning for.
type Store interface {
	IssueReader
	IssueWriter
	CommentStore
	PlanReviewStore
	LabelStore
	RelationStore
	Ranker
	BulkWriter
	Exporter
	Attributor
	ChangeObserver
	StampReporter
	StrandedWritesReporter

	// Close releases whatever the engine holds. It is in the contract because
	// callers must always be able to hand the resource back without knowing
	// what it was — for the Dolt engine that includes the workspace lock, and a
	// close whose error is discarded strands it.
	Close() error
}
