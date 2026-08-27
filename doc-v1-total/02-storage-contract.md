# Storage contract and the in-memory engine

lit separates *what it needs from a storage engine* from *how any engine provides it*. The contract lives in `internal/storage`; two engines implement it — a pure in-memory engine (`internal/storage/memory`) and the Dolt-backed engine (`internal/store`, covered in `03-store-schema.md`). A shared conformance suite (`internal/storage/conformance`) is stated in the package doc to be the actual specification; the interface is the vocabulary (`internal/storage/doc.go:48-55`).

Dependency and vocabulary rules: engine → contract → model, never back; no type crossing the `Store` boundary may name a SQL row, branch, commit, or schema version. Capability interfaces (below) are exempt — naming an engine artifact is what makes something a capability. One acknowledged leak: `SyncStatusReport.DoltVersion`, kept because it renders as the JSON key `dolt_version` (`internal/storage/doc.go:20-41`).


## The Store interface

`Store` composes nine role interfaces plus `Close()` (`internal/storage/contract.go:231-247`) — 36 methods total:

| Role | Methods |
|---|---|
| `IssueReader` | `GetIssue`, `GetIssueDetail`, `ListIssues`, `ListChildren`, `ListTopics`, `ListAllEvents`, `LocalIssueCount` |
| `IssueWriter` | `CreateIssue`, `Apply` |
| `CommentStore` | `AddComment`, `DeleteComment` |
| `LabelStore` | `AddLabel`, `RemoveLabel`, `ReplaceLabels`, `ListLabels` |
| `RelationStore` | `AddRelation`, `RemoveRelation`, `ListRelationsForIssue`, `GetRelationsByIDs`, `SetParent`, `ClearParent` |
| `Ranker` | `RankAbove`, `RankBelow`, `RankToTop`, `RankToBottom`, `RankSet` |
| `BulkWriter` | `BulkApply`, `ImportTree` |
| `Exporter` | `Export` |
| `Attributor` | `AttributeTo` |

Contract points a reimplementation must not lose:

- **`Apply` is the single mutation verb** for the issue record. A `Change` carries an optional `Action` (a status or retention transition — the sealed sum decides which axis, never a caller-side mode) and an `UpdateIssueInput` field patch (all pointer fields; nil = leave unchanged). A pure no-op records no history; an illegal transition is refused, not ignored (`contract.go:86-90`, `issues.go:79-104`). The change's `Reason` belongs to the transition event; `Fields.Reason` belongs to the field-change event — a combined change records two events with independent reasons (`issues.go:92-99`).
- **Mutating reads return the answer.** `AddComment` returns the comment *and* the post-write issue; every label mutation returns the resulting label set; `DeleteComment` returns what was removed — so no follow-up read exists for a concurrent writer to answer differently (`contract.go:96-120`).
- **Ordering is contract.** `ListIssues` orders by `SortBy`, then always by id ascending as the final tie-break (descending sorts reverse only the named keys, never the tie-break). No `SortBy` means rank ascending. `ListAllEvents` is oldest-first by `(created_at, id)` — explicitly *not* recording order; event IDs are random, so same-tick events return in an order unrelated to causality, identically in both engines (`contract.go:26-70`).
- **`Exporter` is core, `Importer` is a capability**: export is the differential-oracle surface both engines serve; only replace-from-export is optional (`contract.go:199-206`, `capabilities.go:191-196`).
- **Attribution is the store's**, stamped at the engine's single event-insertion point after `AttributeTo(streamToken)`; an empty token leaves the engine unattributed, never half-attributed (`contract.go:210-218`).
- **`LocalIssueCount`** counts everything the store holds — archived and deleted included — and reports 0 for a never-written store; it is the adopt-safety signal `lit init` reads (`contract.go:73-78`).

### Error taxonomy

Three error types (`internal/storage/errors.go`, `capabilities.go:219-229`):

- `NotFoundError{Entity, ID}` — renders `<entity> "<id>" not found`; matched with `errors.As`; entity strings pinned by conformance: `issue`, `comment`, `label`, `relation`, `parent relation`.
- `ValidationError{Message}` — a domain-constraint violation; message verbatim.
- `UnsupportedError{Capability, Engine}` — renders `<engine> engine does not offer the <capability> capability`.

### Listing filters

`ListIssuesFilter` (`issues.go:155-171`): every slice is an OR within itself and an AND against the other criteria; every zero value means "unconstrained". Fields: `Statuses`, `Resolutions`, `IssueTypes`, `ExcludeIssueTypes`, `Assignees`, `SearchTerms`, `IDs`, `HasComments *bool`, `LabelsAll` (conjunctive — every named label must be present), `UpdatedAfter`/`UpdatedBefore` (both inclusive of equality), `IncludeArchived`, `IncludeDeleted`, `SortBy`, `Limit`.

Semantics fixed by the memory engine and conformance:

- Retention is the one axis whose default is a filter: a listing that says nothing sees only live issues.
- The status *filter* matches the **derived** state (an epic's computed state); search is case-insensitive substring across exactly four fields — title, description, prompt, topic — and multiple search terms are conjunctive (`memory/list.go:118-161`).
- `Limit: 0` means uncapped; a positive limit truncates the head of the ordered result (`memory/list.go:184-192`).

### Sort fields

`SortFields` is a closed ten-key set: `id`, `title`, `status`, `priority`, `rank`, `type`, `topic`, `assignee`, `created_at`, `updated_at` (`issues.go:131-142`). Conformance requires every one accepted and requires rejection of `description`, `lane`, `labels`, `issue_type`, `item_rank`, `state` (`conformance.go:561-591`).

**Documented, deliberately-pinned fault**: sorting by `status` orders the *stored* status encoding, not derived state. A container stores no status, so it sorts ahead of every leaf ascending and behind every leaf descending, regardless of its derived state — while the status *filter* in the same listing reads derived state. Both engines ship this identically; the source names ticket `links-store-seam-q35v.6` as the correction (`issues.go:121-130`, `conformance.go:593-626`).

### Rank intents

The rank vocabulary is relative intents only — never stored positions (`contract.go:154-177`): `RankAbove`, `RankBelow`, `RankToTop`, `RankToBottom`, `RankSet` (total order over the named ids, stacked at the top of the backlog in the order named).

Rank frames are nested: children rank against siblings inside their epic's frame. An intent naming two issues in different frames is resolved to the nearest *comparable* ancestors — the representatives one level below the lowest common ancestor (or the roots when there is none) — and the substitution is returned (`RankMove{MovedID, AnchorID}`; `RankSetResolution{NamedID, RankedID}`) so the caller can surface it. Nothing inside any epic is reordered by a cross-frame request. Two named ids collapsing onto one representative, or a request ranking an issue against its own container, is refused (`memory/rank.go:149-295`). A parent that is soft-deleted frames nothing — the chain skips it (`memory/rank.go:208-226`).

### Capabilities

Seven optional capabilities, each a sealed generic asked for via `Of(engine)` which returns the interface or a typed `UnsupportedError` — absence is answered, never faked (`capabilities.go:24-34, 239-331`):

| Name | Interface | Surface |
|---|---|---|
| `sync` | `Syncer` | remotes add/remove/list; status (local-only, no network); freshness vs. last-fetched refs; fetch, push, pull; receive (fast-forward-only, never merges); compact (`newgen`/`full` GC depths), compact-if-due, compact-and-push (atomic under one lock); get/record sync state (path + content hash — the staleness marker) |
| `reconcile` | `Reconciler` | reconcile a divergence (field-aware merge replayed into linear history, or held on prose conflict); combine unrelated histories by union; finish a held reconcile with prose resolutions; resolve unrelated by taking one side (requires an owner approval bound to the exact fork); reset to remote head |
| `checkpoints` | `Checkpointer` | create/list/prune/reset; checkpoint name format `<prefix>-<unix-nano>`; `Anchor` is an opaque engine-side identity |
| `repair` | `Repairer` | `Doctor` (examine only), `FixIntegrity` (dangling rows, self-edges, wrong-order edges), `FixRankInversions` |
| `schema-migration` | `SchemaMigrator` | applied schema version; downgrade to an older shape |
| `import` | `Importer` | `ReplaceFromExport` |
| `test-support` | `RawExecutor` | engine-native statement for tests |

The full sync/reconcile result vocabulary (freshness states `never_synced`/`up_to_date`/`ahead`/`behind`/`diverged`; receive, pull, and reconcile state enums including `prose_pending`, `unrelated_histories`, `linearized`, `combined`, `took_local`/`took_remote`; `UnrelatedInventory` with its three disjoint sorted id-slices; `CompactionOutcome`; `HealthReport` JSON keys) is defined in `internal/storage/sync.go` and `capabilities.go`. The memory engine offers **none** of the seven capabilities (`memory/doc.go:59-65`).

## The memory engine

`internal/storage/memory` implements the contract with nothing but Go values: no disk, no file format, no schema; `Close()` is a no-op (`memory/doc.go:1-2`, `engine.go:126-129`). It shares no code with the Dolt engine — deliberately, so the conformance suite's proof means something (`memory/doc.go:15-25`). One plain mutex serializes everything; every exported method locks and delegates to unlocked internals.

Behaviors that define the reference semantics (each mirrored by Dolt unless noted in `03-store-schema.md`):

**Create** (`memory/issues.go:18-98`): parent resolved before prefix (so a missing parent reports the missing issue); title required after trim; labels canonicalized (normalize, dedupe, sort); topic normalized; type defaults to `task`; all string fields trimmed; new issues start `open`/live. Top-level ids are minted with the adaptive hash (lengths tried up to 8, 10 nonces each); children of a parent get sequential `<parent>.N` ids counting only direct children. Placement zero-value appends (bottom); `RankTop` prepends. The create event records `status "" → "open"` for a leaf and no changes for a container.

**Apply pipeline** (`memory/apply.go`): plan the transition, then plan the field patch **baselined on the post-action issue** (so `start --assignee X` plus a field patch doesn't double-record the assignee); then write, then record events. Key rules:

- Any status action on an archived/deleted issue is refused (`cannot <action> archived or deleted issue`).
- No-op rule: a status action whose target state and resulting assignee already hold records nothing. A same-state `start` naming a *new* assignee is the reclaim path — it records and rewrites ownership.
- Retention has no same-state no-op: re-archiving an archived issue is an error (the model's transition table).
- Redirect-target validation on close: a redirecting resolution requires a target; self-redirect refused; missing target `NotFoundError`; a *deleted* target refused; an *archived* target legal ("duplicate of something already done" is the common case) (`memory/apply.go:152-176`).
- Field patch: empty title refused; changing `issue_type` across the container/leaf line refused; a patch that states `labels` rewrites the label rows (authorship and timestamps) even if the set is identical, while a patch not mentioning labels leaves them untouched.
- Field-change rows are recorded in fixed order `title, description, issue_type, priority, assignee, lane, labels`; **`prompt` and `topic` changes produce no change row** — a prompt-only edit records no event and does not bump `updated_at` (`memory/apply.go:253-270`). Topic is immutable through `Apply` entirely.
- Status events record `status`, `closed_at`, `resolution`, `redirect_target`, `assignee` rows; retention events record `archived_at`/`deleted_at` rows; timestamps as RFC3339Nano, absence as `""`.
- Actor defaults to `"unknown"` when blank; structural rows the engine writes on the caller's behalf are authored `"links"`.

**Comments/labels/relations** (`memory/edges.go`): comment body required after trim; **adding a comment records no history event**. Adding a duplicate label is success (idempotent, first-add authorship kept); removing an absent one is `NotFoundError`; `ReplaceLabels` rewrites every row's authorship and timestamp; `ListLabels` on an unknown issue returns empty, no error. Relations: endpoints canonicalized per type; duplicate edge refused; `parent-child` is single-valued from the child, so `SetParent` replaces in one act; self-parent and self-related refused; **`blocks` cycles are rejected** with a DFS reachability check (a rank order honoring every blocks edge exists exactly when there is no cycle). Bucketing (`DependsOn`/`Blocks`/`Children`/`Parent`) is the single reading of the edge set; `related-to` counterparts appear only in `GetIssueDetail.Related`.

**Detail assembly** (`memory/issues.go:174-233`): `GetIssueDetail` returns relations (insertion order), comments, events (`created_at, id` order), children/depends-on/blocks (rank order), siblings (parent's other children), related, and `RedirectTarget` hydrated from the issue's own close payload — never from the relations graph.

**Epic visibility rule** (`memory/engine.go:227-234`): a live container derives state from its *live* children only — archiving a child removes it from the epic's progress. A container that is itself archived/deleted keeps its whole child set, so its reading freezes at what it was.

**Topics** (`memory/issues.go:246-267`): the distinct non-empty topics of non-deleted issues, sorted. Deletion removes a topic from the vocabulary; archival does not.

**Bulk and import** (`memory/bulk.go`): both parse-validate the whole file before applying anything, topologically sort intra-batch references, create/update in that order, then wire `depends_on` edges (as `blocks`, dependent → dependency) in a second pass. Failure triggers **compensation, not rollback**: created issues are soft-deleted (reason `import rollback`), un-undoable ids are named in the error, and already-applied updates stay applied. Spec validation rejects surrounding whitespace, duplicates, self-dependency, forbidden fields on updates, and missing required fields on creates (`internal/storage/specs.go`, `memory/bulk.go`). `ImportTree` references must all resolve inside the file; `BulkApply` references may name pre-existing real ids. Bulk files are multi-document YAML with unknown fields rejected; import trees are a single JSON array with unknown fields and trailing data rejected (`internal/storage/specs.go`).

**Export** (`memory/export.go`): the whole store — archived and deleted included — with every collection in a total order (issues by rank, labels by issue+name, events by created_at+id, relations/comments in write order) so two stores holding the same facts serialize to the same bytes. Version 2.

**Rank storage**: the memory engine stores no rank strings at all — the total order is a slice, `Issue.Rank` is rendered at read time as a zero-padded 9-digit index (`%09d`), and a rank inversion is unrepresentable (`memory/engine.go:44-49, 159-164`). The conformance suite never asserts rank encoding, only observable order.

## What conformance requires — and doesn't

The suite runs 36 cases (`conformance.go`), each against a fresh engine, asserting only what a caller can observe through the contract. Where behavior was ambiguous, Dolt's answer was the tie-break (`conformance.go:29-37`).

Not required by the suite (each is engine-tested or unexercised) — relevant when judging what v1 actually guarantees across engines:

- any of the seven capability interfaces
- concurrency safety
- the rank string encoding and the minted-id shape
- `Resolutions` filtering (declared and implemented, but no conformance case exercises it)
- `ReplaceLabels`/`ListLabels` against a missing issue
- `blocks`-cycle rejection (implemented in both engines, unexercised by the suite)
