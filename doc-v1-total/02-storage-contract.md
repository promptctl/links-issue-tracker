# Storage contract and the in-memory engine

lit separates *what it needs from a storage engine* from *how any engine provides it*. The contract lives in `internal/storage`; two engines implement it — a pure in-memory engine (`internal/storage/memory`) and the Dolt-backed engine (`internal/store`, covered in `03-store-schema.md`). A shared conformance suite (`internal/storage/conformance`) is stated in the package doc to be the actual specification; the interface is the vocabulary (`internal/storage/doc.go`).

Dependency and vocabulary rules: engine → contract → model, never back; no type crossing the `Store` boundary may name a SQL row, branch, commit, or schema version. Capability interfaces (below) are exempt — naming an engine artifact is what makes something a capability. No type in the core names an engine artifact (`internal/storage/doc.go`).


## The Store interface

`Store` composes eleven role interfaces plus `Close()` (`internal/storage/contract.go`, `PlanReviewStore` in `internal/storage/plan_review.go`) — 36 methods total:

| Role | Methods |
|---|---|
| `IssueReader` | `GetIssue`, `GetIssueDetail`, `ListIssues`, `ListTopics`, `ListIssueIdentities`, `ListAllEvents`, `ListEvents`, `LocalIssueCount` |
| `IssueWriter` | `CreateIssue`, `Apply`, `Purge` |
| `CommentStore` | `AddComment`, `DeleteComment` |
| `PlanReviewStore` | `AddPlanReview` |
| `LabelStore` | `AddLabel`, `RemoveLabel`, `ReplaceLabels`, `ListLabels` |
| `RelationStore` | `AddRelation`, `RemoveRelation`, `ListRelationsForIssue`, `GetRelationsByIDs`, `DependencyOrder`, `SetParent`, `ClearParent` |
| `Ranker` | `RankAbove`, `RankBelow`, `RankToTop`, `RankToBottom`, `RankSet` |
| `BulkWriter` | `BulkApply`, `ImportTree` |
| `Exporter` | `Export` |
| `Attributor` | `AttributeTo` |
| `ChangeObserver` | `ObserveChanges` |

Contract points a reimplementation must not lose:

- **`Apply` is the single mutation verb** for the issue record. A `Change` carries an optional `Action` (a status or retention transition — the sealed sum decides which axis, never a caller-side mode) and an `UpdateIssueInput` field patch (all pointer fields; nil = leave unchanged). A pure no-op records no history; an illegal transition is refused, not ignored (`contract.go`, `issues.go`). The change's `Reason` belongs to the transition event; `Fields.Reason` belongs to the field-change event — a combined change records two events with independent reasons (`issues.go`).
- **`Purge` is the one irreversible removal.** `Purge(ctx, PurgeInput{IssueID, Actor})` removes the issue's row, comments, plan reviews, labels, history, and every edge to or from it in one write, and returns `PurgeResult{Purge, Comments, PlanReviews, Labels, Relations, Events}` — the `model.Purge` record left behind and how many of each row went with it. Other issues are not edited. The record names the ticket by its id and `created_at`. The purged id is never minted again, and every merge and restore drops the ticket (`model.Export.ApplyPurges`). A missing id is `NotFoundError`; an issue whose purge would leave a child leaf with no parent edge — deleted and archived children included — is refused with `model.ValidationError` (`storage.RejectPurgeOfOccupiedContainer`, `containment.go`): "cannot purge <id>: it would leave <ids> outside any epic; move them with lit parent set --child <id> --parent <epic-id>, or purge them first", `<ids>` the stranded children's ids, sorted and comma-separated. A child epic, or a leaf that keeps another parent edge, does not refuse the purge; the child epic is left top-level (`contract.go`, `issues.go`).
- **A plan review is recorded, never edited.** `AddPlanReview(ctx, AddPlanReviewInput{IssueID, Plan, Verdict, Laws, CreatedBy})` takes the reviewer's input as strings (`Laws` a `[]string`) and names the plan text it judged by its digest (`Plan`, as `lit show` prints it), not by a version number and not by taking the current text: a sync that merges in an older edit renumbers the versions, and a plan revised while the review ran is not judged by the text before it. Both engines decide the row through the pure `storage.PlanReviewOf(issue, events, in)`, which parses in this order: the verdict through `model.ParsePlanVerdict`; each law through `model.ParseLaw`, then `model.CanonicalLaws`; a `fail` citing no law is `model.ValidationError` "a failing verdict cites at least one law the plan breaks"; `Plan` through `model.ParsePlanDigest`; a digest equal to `model.DigestPlan` of none of the `TextVersions` of `plan` is `PlanDigestNotFoundError{IssueID, Digest}`; a blank reviewer is recorded as `unknown`. The first refusal is returned. The engine then stamps id `rvw-` + a UUID and its clock's time. A missing issue is `NotFoundError`. The contract has no method that edits or removes a review; a purge of its issue removes it (`plan_review.go`).
- **Mutating reads return the answer.** `AddComment` returns the comment *and* the post-write issue; every label mutation returns the resulting label set; `DeleteComment` returns what was removed — so no follow-up read exists for a concurrent writer to answer differently (`contract.go`).
- **Ordering is contract.** `ListIssues` orders by `SortBy`, then always by id ascending as the final tie-break (descending sorts reverse only the named keys, never the tie-break). No `SortBy` means rank ascending. `ListAllEvents` is oldest-first by `(created_at, id)` — explicitly *not* recording order; event IDs are random, so same-tick events return in an order unrelated to causality, identically in both engines (`contract.go`).
- **`Exporter` is core, `Importer` is a capability**: export is the differential-oracle surface both engines serve; only replace-from-export is optional (`contract.go`, `capabilities.go`).
- **Attribution is the store's**, stamped at the engine's single event-insertion point after `AttributeTo(streamToken)`; an empty token leaves the engine unattributed, never half-attributed (`contract.go`).
- **`LocalIssueCount`** counts everything the store holds — archived and deleted included — and reports 0 for a never-written store; it is the adopt-safety signal `lit init` reads (`contract.go`).

### Error taxonomy

Four error types (`internal/storage/errors.go`, `capabilities.go`, `internal/storage/plan_review.go`, `internal/model/validation.go`):

- `NotFoundError{Entity, ID}` — renders `<entity> "<id>" not found`; matched with `errors.As`; entity strings pinned by conformance: `issue`, `comment`, `label`, `relation`.
- `model.ValidationError{Message}` — a domain-constraint violation; message verbatim. It is defined in `internal/model` beside the rules, so a rule raises it where it is written; conformance pins a blank title and a refused topic to it.
- `UnsupportedError{Capability, Engine}` — renders `<engine> engine does not offer the <capability> capability`.
- `VersionNotFoundError{IssueID, Field, Number, Held}` (`plan_review.go`) — a reference to version `Number` of a long text field past the `Held` versions the issue's record holds; renders `<id> has no <field>@<n>: its record holds <count> of <field>, numbered from 1; ` followed by the `lit show <id> --field <field>` command and `prints the current text`, `<count>` spelled by `DescribeVersions` (`1 version`, `<n> versions`). It is not `NotFoundError`, whose remediation is to check the issue id: here the id resolved.
- `PlanDigestNotFoundError{IssueID, Digest}` (`plan_review.go`) — a plan review naming a digest no text of the issue's plan history holds; renders `<id> has no plan text with digest <digest>; ` followed by the `lit show <id>` command and `prints the current plan beside its digest`.

### Listing filters

`ListIssuesFilter` (`issues.go`): every slice is an OR within itself and an AND against the other criteria; every zero value means "unconstrained". Fields: `Statuses`, `Resolutions`, `IssueTypes`, `ExcludeIssueTypes`, `Assignees`, `SearchTerms`, `IDs`, `HasComments *bool`, `LabelsAll` (conjunctive — every named label must be present), `UpdatedAfter`/`UpdatedBefore` (both inclusive of equality), `IncludeArchived`, `IncludeDeleted`, `SortBy`, `Limit`.

Semantics fixed by the memory engine and conformance:

- Retention is the one axis whose default is a filter: a listing that says nothing sees only live issues.
- The status *filter* matches the **derived** state (an epic's computed state) (`internal/storage/selects.go`); search is case-insensitive substring across exactly five fields — title, description, prompt, plan, topic (`internal/storage/selects.go`); multiple search terms are conjunctive, and the AND across terms is applied by `Selects`, not by the per-term match (`internal/storage/selects.go`).
- `Limit: 0` means uncapped; a positive limit truncates the head of the ordered result (`memory/list.go`).

### Sort fields

The sort keys are a closed ten-key set: `id`, `title`, `status`, `priority`, `rank`, `type`, `topic`, `assignee`, `created_at`, `updated_at`. They and the comparison each one makes are one table, `sortKeys`, which both engines order through by calling `IssueOrdering` with their rank ancestry; any other name is a `ValidationError` (`ordering.go`). Text keys compare byte-wise, so `"Zeta"` sorts before `"alpha"`; `type` compares the token, so `bug` sorts before `task`; `created_at` and `updated_at` compare the instant, not its text (`ordering.go`; `conformance.go`).

Sorting by `status` orders the **derived** lifecycle state, the same reading the status filter takes, compared as its name — ascending `closed`, `in_progress`, `open`. A container stores no status, so it sorts among the leaves by the state its children give it, not at an end of the listing; the key compares `State()` (`ordering.go`; `conformance.go`).

### Rank intents

The rank vocabulary is relative intents only — never stored positions (`contract.go`): `RankAbove`, `RankBelow`, `RankToTop`, `RankToBottom`, `RankSet` (total order over the named ids, stacked at the top of their frame in the order named).

Rank frames are nested: children rank against siblings inside their epic's frame. An intent naming two issues in different frames is resolved to the nearest *comparable* ancestors — the representatives one level below the lowest common ancestor (or the roots when there is none) — and the substitution is returned (`RankMove{MovedID, AnchorID}`; `RankSetResolution{NamedID, RankedID}`) so the caller can surface it. Nothing inside any epic is reordered by a cross-frame request. Two named ids collapsing onto one representative, or a request ranking an issue against its own container, is refused (`memory/rank.go`). A parent that is soft-deleted frames nothing — the chain skips it (`memory/rank.go`).

### Capabilities

Seven optional capabilities, each a sealed generic asked for via `Of(engine)` which returns the interface or a typed `UnsupportedError` — absence is answered, never faked (`capabilities.go`):

| Name | Interface | Surface |
|---|---|---|
| `sync` | `Syncer` | remotes add/remove/list; status (local-only, no network); freshness vs. last-fetched refs; fetch, push, pull; receive (fast-forward-only, never merges); compact (`newgen`/`full` GC depths), compact-if-due, compact-and-push (atomic under one lock); get/record sync state (path + content hash — the staleness marker) |
| `reconcile` | `Reconciler` | reconcile a divergence (field-aware merge replayed into linear history, or held on prose conflict); combine unrelated histories by union; finish a held reconcile with prose resolutions; resolve unrelated by taking one side (requires an owner approval bound to the exact fork); reset to remote head |
| `checkpoints` | `Checkpointer` | create/list/prune/reset; checkpoint name format `<prefix>-<unix-nano>`; `Anchor` is an opaque engine-side identity |
| `repair` | `Repairer` | `Doctor` (examine only), `FixIntegrity` (dangling rows, self-edges, wrong-order edges), `FixRankInversions` |
| `schema-migration` | `SchemaMigrator` | applied schema version; downgrade to an older shape |
| `import` | `Importer` | `ReplaceFromExport` |
| `test-support` | `RawExecutor` | engine-native statement for tests |

The full sync/reconcile result vocabulary (freshness states `never_synced`/`up_to_date`/`ahead`/`behind`/`diverged`; receive, pull, and reconcile state enums including `prose_pending`, `unrelated_histories`, `linearized`, `combined`, `took_local`/`took_remote`; `UnrelatedInventory` with its three disjoint sorted id-slices; `CompactionOutcome`; `HealthReport` JSON keys) is defined in `internal/storage/sync.go` and `capabilities.go`. The memory engine offers **none** of the seven capabilities (`memory/doc.go`).

## The memory engine

`internal/storage/memory` implements the contract with nothing but Go values: no disk, no file format, no schema; `Close()` is a no-op (`memory/doc.go`, `engine.go`). It shares no code with the Dolt engine — deliberately, so the conformance suite's proof means something (`memory/doc.go`). One plain mutex serializes everything; every exported method locks and delegates to unlocked internals.

Behaviors that define the reference semantics (each mirrored by Dolt unless noted in `03-store-schema.md`):

**Create** (`memory/issues.go`): a leaf with no parent refused before any lookup (`storage.RejectLeafWithoutParent`); parent resolved before prefix (so a missing parent reports the missing issue) and refused unless it is an epic (`storage.RejectParentThatIsNotAContainer`); title required after trim; labels canonicalized (normalize, dedupe, sort); topic normalized; type defaults to `task`; all string fields trimmed; new issues start `open`/live. Every id is minted through `issueid.Mint`, as the Dolt engine mints it: a top-level id under `<prefix>-<topic>-`, sized by the count of ids with no dot; a child under `<parentID>.`, sized by the count of `parent-child` edges into the parent; a candidate an issue or a purge record holds is skipped. Placement zero-value appends to the whole order (bottom); `RankTop` leads the frame the issue is filed into — the live parent it is created under, or the top level — and, when that frame holds nothing yet, lands immediately after the issue that frames it, or at slot `0` when that frame is the top level. The create event records `status "" → "open"` for a leaf and no changes for a container.

**Apply pipeline** (`memory/apply.go`): plan the transition, then plan the field patch **baselined on the post-action issue** (so `start --by X` plus a field patch doesn't double-record the assignee); then write, then record events. Key rules:

- Any status action on an archived/deleted issue is refused with `model.FrozenIssueError` (``issue <id> is <archived|deleted>, so its status cannot change: run `lit <unarchive|restore> <id>` first``).
- No-op rule: a status action whose target state and resulting assignee already hold records nothing. A same-state `start` naming a *new* assignee is the reclaim path — it records and rewrites ownership.
- Retention has no same-state no-op: re-archiving an archived issue is an error (the model's transition table).
- Redirect-target validation on close: a redirecting resolution requires a target; self-redirect refused; missing target `NotFoundError`; a *deleted* target refused; an *archived* target legal ("duplicate of something already done" is the common case) (`memory/apply.go`).
- Field patch: empty title refused; changing `issue_type` across the container/leaf line refused; a patch that states `labels` rewrites the label rows (authorship and timestamps) even if the set is identical, while a patch not mentioning labels leaves them untouched.
- Field-change rows are recorded in fixed order `title, description, prompt, plan, issue_type, priority, assignee, lane, labels` (`storage.ApplyIssueFields`, `fields.go`); a patch that moves no field records no event and does not bump `updated_at` (`memory/apply.go`). Topic is immutable through `Apply` entirely.
- Status events record `status`, `closed_at`, `resolution`, `redirect_target`, `assignee` rows; retention events record `archived_at`/`deleted_at` rows; timestamps as RFC3339Nano, absence as `""`.
- Actor defaults to `"unknown"` when blank (`storage.Actor`); a create's rows — its created event, parent edge and labels — are authored by `CreateIssueInput.Actor`.

**Comments/labels/relations** (`memory/edges.go`): comment body required after trim; **adding a comment records no history event**. Adding a duplicate label is success (idempotent, first-add authorship kept); removing an absent one is `NotFoundError`; `ReplaceLabels` rewrites every row's authorship and timestamp; `ListLabels` on an unknown issue returns empty, no error. Relations: endpoints canonicalized per type; duplicate edge refused; `parent-child` is single-valued from the child, so `SetParent` replaces in one act; a parent-child edge onto a parent that is not an epic refused, through `SetParent` and `AddRelation` alike (`storage.RejectParentThatIsNotAContainer`); `ClearParent` and `RemoveRelation` of a leaf's parent edge refused (`storage.RejectDetachingALeaf`), judged after a missing edge is reported missing; self-parent and self-related refused; **`blocked-by` cycles are rejected** with a DFS reachability check (a rank order honoring every blocked-by edge exists exactly when there is no cycle). Bucketing (`BlockedBy`/`Blocks`/`Children`/`Parent`) is the single reading of the edge set; `related-to` counterparts appear only in `GetIssueDetail.Related`.

**Detail assembly** (`memory/issues.go`): `GetIssueDetail` returns relations (insertion order), comments, events (`created_at, id` order), plan reviews (`created_at` instant then id, `storage.PlanReviewOrdering` in `internal/storage/ordering.go`, each a copy with its own `Laws` slice, `clonePlanReviews` in `memory/plan_review.go`), children/blocked-by/blocks (rank order), siblings (parent's other children), related, and `RedirectTarget` hydrated from the issue's own close payload — never from the relations graph.

**Epic visibility rule** (`memory/engine.go`): a live container derives state from its *live* children only — archiving a child removes it from the epic's progress. A container that is itself archived/deleted keeps its whole child set, so its reading freezes at what it was.

**Topics** (`memory/issues.go`): the distinct non-empty topics of non-deleted issues, sorted. Deletion removes a topic from the vocabulary; archival does not.

**Bulk and import** (`memory/bulk.go`): both parse-validate the whole file before applying anything, topologically sort intra-batch references, create/update in that order, then wire `blocked_by` edges (as `blocked-by`, dependent → dependency) in a second pass. Failure triggers **compensation, not rollback**: created issues are soft-deleted (reason `import rollback`), un-undoable ids are named in the error, and already-applied updates stay applied. Spec validation rejects surrounding whitespace, duplicates, self-dependency, forbidden fields on updates, and missing required fields on creates (`internal/storage/specs.go`, `memory/bulk.go`). `ImportTree` references must all resolve inside the file; `BulkApply` references may name pre-existing real ids. Bulk files are multi-document YAML with unknown fields rejected; import trees are a single JSON array with unknown fields and trailing data rejected (`internal/storage/specs.go`).

**Purge** (`memory/purge.go`): under the one lock, a missing id is `NotFoundError` and an issue whose purge would leave a child leaf with no other parent-child edge is refused (`storage.RejectPurgeOfOccupiedContainer`, children read by `detachedChildren`); otherwise the comments, the plan reviews, the relations with the issue at either end, the events, the labels, its place in the order, and the record are removed, and the purge record (stamped with the issue's `createdAt`, the engine clock and the input actor) is kept. Minting treats an id as taken when any purge record has it (`memory/issues.go`).

**Export** (`memory/export.go`): the whole store — archived and deleted included — with every collection in a total order (issues by rank, labels by issue+name, events by created_at+id, purges by issue id then `created_at`, plan reviews by `created_at` instant then id through `clonePlanReviews`, relations/comments in write order) so two stores holding the same facts serialize to the same bytes. Version `model.ExportVersion` (5).

**Rank storage**: the memory engine stores no rank strings at all — the total order is a slice, `Issue.Rank` is rendered at read time as a zero-padded 9-digit index (`%09d`), and a rank inversion is unrepresentable (`memory/engine.go`). The conformance suite never asserts rank encoding, only observable order.

## What conformance requires — and doesn't

The suite runs 72 cases (`conformance.go`), each against a fresh engine, asserting only what a caller can observe through the contract. Where behavior was ambiguous, Dolt's answer was the tie-break (`conformance.go`).

Not required by the suite (each is engine-tested or unexercised) — relevant when judging what v1 actually guarantees across engines:

- any of the seven capability interfaces
- concurrency safety
- the rank string encoding and the minted-id shape
- `Resolutions` filtering (declared and implemented, but no conformance case exercises it)
- `ReplaceLabels`/`ListLabels` against a missing issue
- `blocked-by`-cycle rejection (implemented in both engines, unexercised by the suite)
