# Storage Contract — Raw Behavioral Inventory

Scope: `internal/storage`, `internal/storage/memory`, `internal/storage/conformance`.
Derived exclusively from Go source. Every claim carries `file:line`.

Path prefix for all citations: `/Users/bmf/code/links-issue-tracker/`

---

## PART 1 — `internal/storage` (the contract layer)

### 1.1 Package identity and dependency rule

- Package `storage` declares what lit needs from a storage engine; no engine type appears in any signature (`internal/storage/doc.go:1-4`).
- Vocabulary rule: every type crossing the `Store` boundary is a `model` type or a type declared in this package. `Store` and its constituent interfaces may name no SQL row, branch, commit, or schema version (`internal/storage/doc.go:20-27`).
- Dependency direction: engine → contract → model, never back (`internal/storage/doc.go:25-27`).
- Capability interfaces are deliberately exempt from the vocabulary rule; naming an engine artifact is what makes something a capability rather than storage (`internal/storage/doc.go:29-36`).
- One acknowledged leak: `SyncStatusReport.DoltVersion`, engine-named, kept because it renders as the JSON key `dolt_version` and renaming moves observable output (`internal/storage/doc.go:37-41`; field at `internal/storage/sync.go:44`).
- The conformance suite, not the interface, is stated as the actual specification (`internal/storage/doc.go:48-55`).

---

### 1.2 `Store` — the composed contract

`Store` = `IssueReader` + `IssueWriter` + `CommentStore` + `LabelStore` + `RelationStore` + `Ranker` + `BulkWriter` + `Exporter` + `Attributor` + `Close() error` (`internal/storage/contract.go:231-247`).

- `Close() error` is in the contract so a caller can release an engine's resources without knowing what they are; for Dolt this includes the workspace lock, and discarding the error strands it (`internal/storage/contract.go:242-246`).
- Deliberately absent from `Store` (they are capabilities): sync, schema migration, checkpoints, repair, raw test access — named as `Syncer`, `Reconciler`, `Checkpointer`, `Repairer`, `SchemaMigrator`, `Importer`, `RawExecutor` (`internal/storage/contract.go:225-230`).

---

### 1.3 `IssueReader` (`internal/storage/contract.go:15-79`)

General contract: every method is a pure read from the caller's view; two identical reads with no intervening mutation describe the same state (`internal/storage/contract.go:11-14`).

**`GetIssue(ctx, id string) (model.Issue, error)`** — `internal/storage/contract.go:19`
- A missing id returns `NotFoundError`, never a zero `Issue` (`internal/storage/contract.go:16-18`).

**`GetIssueDetail(ctx, id string) (model.IssueDetail, error)`** — `internal/storage/contract.go:24`
- Returns the issue plus its comments, history, and structural edges — the full single-issue view (`internal/storage/contract.go:21-23`).
- Callers needing only edges for many issues use `GetRelationsByIDs` instead (`internal/storage/contract.go:22-23`).

**`ListIssues(ctx, filter ListIssuesFilter) ([]model.Issue, error)`** — `internal/storage/contract.go:42`
- Ordered by `SortBy`, then — always, as the final key — by id ascending (`internal/storage/contract.go:26-28`).
- With no `SortBy`: rank ascending, ties broken by id (`internal/storage/contract.go:28-29`).
- The trailing id key is contract, not engine convenience: without it, a sort on any duplicated field value leaves tied rows in engine-incidental order and two engines diverge (`internal/storage/contract.go:31-37`).
- `SortBy` may name only a `SortFields` member (`internal/storage/contract.go:39-41`).

**`ListChildren(ctx, parentID string) ([]model.Issue, error)`** — `internal/storage/contract.go:46`
- Returns one epic's children in rank order; an id with no children yields an empty slice; only an unreadable store is an error (`internal/storage/contract.go:44-45`).

**`ListTopics(ctx) ([]string, error)`** — `internal/storage/contract.go:50`
- Distinct non-empty topics that *live* issues carry, ascending. Derived vocabulary, never stored (`internal/storage/contract.go:48-49`).

**`ListAllEvents(ctx) ([]model.IssueEvent, error)`** — `internal/storage/contract.go:71`
- Whole issue history, oldest first by creation time, ties broken by event id ascending (`internal/storage/contract.go:52-53`).
- Used by export and by claim derivation; claim derivation needs the whole history because the establishing event for a lane's holder can be arbitrarily old, so a recency cutoff would drop the claims it was meant to speed up (`internal/storage/contract.go:53-58`).
- The id tie-break is explicitly NOT a happens-before claim: event ids are random, so same-tick events come back in an order unrelated to recording order. Both engines are wrong about causality identically, on purpose (`internal/storage/contract.go:60-70`).

**`LocalIssueCount(ctx) (int64, error)`** — `internal/storage/contract.go:78`
- How many issues the store holds; the adopt-safety signal for `lit init` (`internal/storage/contract.go:73-74`).
- A never-written store reports 0 rather than erroring (`internal/storage/contract.go:75-77`).

---

### 1.4 `IssueWriter` (`internal/storage/contract.go:82-92`)

**`CreateIssue(ctx, in CreateIssueInput) (model.Issue, error)`** — `internal/storage/contract.go:84`
- Records a new issue and returns it as stored, with its id minted (`internal/storage/contract.go:83`).

**`Apply(ctx, id string, c Change) (model.Issue, error)`** — `internal/storage/contract.go:91`
- The single execution path for issue-record changes; the `Change` value carries all variability, so there is no second mutation verb (`internal/storage/contract.go:86-88`).
- A pure no-op records nothing in history (`internal/storage/contract.go:88-89`).
- An illegal transition is refused rather than silently ignored (`internal/storage/contract.go:89-90`).

---

### 1.5 `CommentStore` (`internal/storage/contract.go:96-105`)

**`AddComment(ctx, in AddCommentInput) (model.Comment, model.Issue, error)`** — `internal/storage/contract.go:100`
- Returns both the new comment and the issue as it stands after the write, so a caller rendering both does not re-read (`internal/storage/contract.go:97-99`).

**`DeleteComment(ctx, commentID string) (model.Comment, error)`** — `internal/storage/contract.go:104`
- Removes one comment and returns what was removed, so the deletion is reportable without a prior read (`internal/storage/contract.go:102-103`).

---

### 1.6 `LabelStore` (`internal/storage/contract.go:113-120`)

Interface-wide rule: every mutating method returns the issue's *resulting label set*, so "what labels does it have now" is never a follow-up read a concurrent writer could answer differently (`internal/storage/contract.go:107-112`).

- `AddLabel(ctx, in AddLabelInput) ([]string, error)` — `internal/storage/contract.go:114`
- `RemoveLabel(ctx, issueID, labelName string) ([]string, error)` — `internal/storage/contract.go:115`
- `ReplaceLabels(ctx, issueID string, labels []string, createdBy string) error` — sets the whole set at once; the authored-document path, where the file states the labels an issue has rather than a delta (`internal/storage/contract.go:116-118`)
- `ListLabels(ctx, issueID string) ([]string, error)` — `internal/storage/contract.go:119`

---

### 1.7 `RelationStore` (`internal/storage/contract.go:128-152`)

Parentage gets its own two verbs rather than riding `AddRelation` because it is the one edge with an arity rule — at most one parent — and `SetParent` is where replacement happens as a single act (`internal/storage/contract.go:124-127`).

**`AddRelation(ctx, in AddRelationInput) (model.Relation, error)`** — `internal/storage/contract.go:129`

**`RemoveRelation(ctx, srcID, dstID string, relType model.RelationType) error`** — `internal/storage/contract.go:130`

**`ListRelationsForIssue(ctx, issueID string, types ...model.RelationType) ([]model.Relation, error)`** — `internal/storage/contract.go:134`
- Every edge incident to the issue in *either* direction, oldest first, optionally narrowed to the named types (`internal/storage/contract.go:132-133`).

**`GetRelationsByIDs(ctx, ids []string) (map[string]IssueRelations, error)`** — `internal/storage/contract.go:140`
- The batch neighborhood read: one call for many issues, counterparts hydrated, avoiding `GetIssueDetail`'s per-issue comment and history cost (`internal/storage/contract.go:136-139`).

**`SetParent(ctx, in SetParentInput) (model.Relation, error)`** — `internal/storage/contract.go:145`
- Wires a child under a parent, replacing any existing parent in one act, returning the edge written. Both endpoints must exist and an issue may not be its own parent (`internal/storage/contract.go:142-144`).

**`ClearParent(ctx, childID string) error`** — `internal/storage/contract.go:151`
- Clearing a child that has no parent is `NotFoundError`, not a silent success (`internal/storage/contract.go:147-150`).

---

### 1.8 `Ranker` (`internal/storage/contract.go:168-177`)

- The vocabulary is relative intents only — never stored positions — because "above Y" survives everyone else reordering around it (`internal/storage/contract.go:154-161`).
- The two anchored verbs return `RankMove` because rank frames are nested: an intent naming two issues in different epics is honored against the containing ancestors that are comparable, and the substitution is returned so the caller can surface it (`internal/storage/contract.go:163-167`).

- `RankAbove(ctx, issueID, targetID string) (RankMove, error)` — `internal/storage/contract.go:169`
- `RankBelow(ctx, issueID, targetID string) (RankMove, error)` — `internal/storage/contract.go:170`
- `RankToTop(ctx, issueID string) error` — `internal/storage/contract.go:171`
- `RankToBottom(ctx, issueID string) error` — `internal/storage/contract.go:172`
- `RankSet(ctx, ids []string) ([]RankSetResolution, error)` — imposes a total order on the named issues at once, returning which representative each name resolved to (`internal/storage/contract.go:174-176`)

**`RankMove`** — `internal/storage/rank.go:9-12`
- `MovedID string` — the issue actually re-ranked after frame resolution.
- `AnchorID string` — the issue it was re-ranked relative to.
- For frame-mates these are the inputs unchanged; cross-frame, one or both are containing ancestors (`internal/storage/rank.go:3-8`).

**`RankSetResolution`** — `internal/storage/rank.go:19-22`
- `NamedID string` (json `named_id`), `RankedID string` (json `ranked_id`). Equal for frame-mates; when they differ the caller must surface the substitution (`internal/storage/rank.go:14-18`).

---

### 1.9 `BulkWriter` (`internal/storage/contract.go:189-197`)

Interface-wide failure contract: **compensated, not transactional**. A batch that fails partway undoes the issues it created, and the error names every one it could not undo. Updates already applied in a mixed batch are NOT reverted, which is why the error text is the caller's only complete account (`internal/storage/contract.go:180-188`).

- `BulkApply(ctx, prefix, actor string, specs []BulkIssueSpec) (BulkApplyResult, error)` — mixed create/update batch, resolving intra-batch references by `LocalID` (`internal/storage/contract.go:190-192`)
- `ImportTree(ctx, prefix string, specs []ImportTreeSpec) (ImportTreeResult, error)` — creates a whole issue tree, returning local-ID → real-ID mapping (`internal/storage/contract.go:194-196`)

---

### 1.10 `Exporter` (`internal/storage/contract.go:204-206`)

- `Export(ctx) (model.Export, error)` — serializes the store's entire contents as one value (`internal/storage/contract.go:205`).
- It is core rather than a capability because it is the differential oracle surface: the one full-state read two engines both serve (`internal/storage/contract.go:199-203`).

---

### 1.11 `Attributor` (`internal/storage/contract.go:216-218`)

- `AttributeTo(streamToken string)` — names the checkout whose work the engine is about to record (`internal/storage/contract.go:217`).
- Attribution is the store's, not each mutation's: an engine stamps it at its single event-insertion point (`internal/storage/contract.go:210-213`).
- An empty token leaves the engine **unattributed** rather than half-attributed — the contract for a read-mode open of a checkout that has never mutated, and why the method takes no presence flag (`internal/storage/contract.go:213-215`).

---

### 1.12 Error taxonomy (`internal/storage/errors.go`)

**`NotFoundError{Entity, ID string}`** — `internal/storage/errors.go:13-16`
- `Error()` renders `fmt.Sprintf("%s %q not found", e.Entity, e.ID)` (`internal/storage/errors.go:18-20`).
- Every engine returns it — wrapped or bare, matched with `errors.As` — for a read or mutation against an id that no issue, comment, or relation holds (`internal/storage/errors.go:5-8`).
- Callers dispatch on it; the CLI maps it to its own exit code (`internal/storage/errors.go:8-10`).

**`ValidationError{Message string}`** — `internal/storage/errors.go:24-26`
- `Error()` returns `Message` verbatim (`internal/storage/errors.go:28`).
- Returned when a domain constraint (field value, type, range) is violated (`internal/storage/errors.go:22-23`).

**`UnsupportedError{Capability, Engine string}`** — `internal/storage/capabilities.go:219-225`
- `Error()` renders `fmt.Sprintf("%s engine does not offer the %s capability", e.Engine, e.Capability)` (`internal/storage/capabilities.go:227-229`).
- `Capability` is the capability's name as `Capability.Name()` reports it; `Engine` names the concrete engine asked (`internal/storage/capabilities.go:220-224`).

Other error surfaces in the contract package: `ParseSortSpecs` returns `ValidationError` for an unrecognized direction (`internal/storage/sort.go:41`); `ParseBulkSpecs` returns `fmt.Errorf("bulk: parse spec: %w", err)` (`internal/storage/bulk.go` → `internal/storage/specs.go:35`); `ParseImportTreeSpecs` returns `fmt.Errorf("import: parse spec: %w", err)` (`internal/storage/specs.go:55`) and `errors.New("import: unexpected trailing data after spec array")` (`internal/storage/specs.go:58`).

---

### 1.13 Input/option structs

#### `RankPlacement` (`internal/storage/issues.go:22-27`)
- `RankBottom RankPlacement = iota` — sorts after all existing items; **the zero value and the default** (`internal/storage/issues.go:25`).
- `RankTop` — sorts before all existing items (`internal/storage/issues.go:26`).
- The zero value being bottom is the whole enforcement mechanism for "one default across every creation surface" (`internal/storage/issues.go:9-16`).
- Rationale that bottom-of-order is bottom-of-frame: a child's rank is only compared against siblings', composite rank keyed on the containing epic's rank first (`internal/storage/issues.go:18-21`).

#### `CreateIssueInput` (`internal/storage/issues.go:29-56`)
| Field | Type | Effect |
|---|---|---|
| `Title` | `string` | `internal/storage/issues.go:30` |
| `Description` | `string` | `internal/storage/issues.go:31` |
| `Prompt` | `string` | `internal/storage/issues.go:32` |
| `IssueType` | `model.IssueType` | Already-parsed vocabulary, never a raw flag string; trust boundaries route through `model.ParseIssueType`. Zero value means "unspecified" and defaults to task (`internal/storage/issues.go:33-38`) |
| `Topic` | `string` | `internal/storage/issues.go:39` |
| `ParentID` | `string` | `internal/storage/issues.go:40` |
| `Priority` | `model.Priority` | Already-parsed domain vocabulary; trust boundaries route through `model.ParsePriority` or `model.CanonicalPriority` (`internal/storage/issues.go:41-45`) |
| `Assignee` | `string` | `internal/storage/issues.go:46` |
| `Lane` | `string` | `internal/storage/issues.go:47` |
| `Labels` | `[]string` | `internal/storage/issues.go:48` |
| `Placement` | `RankPlacement` | Zero value (`RankBottom`) appends, so an authored batch keeps its order for free (`internal/storage/issues.go:49-52`) |
| `Prefix` | `string` | Workspace's cosmetic ID prefix (e.g. `"links"` → `links-foo-abc1`), sourced from workspace config at the call site; not persisted as derived state (`internal/storage/issues.go:53-55`) |

#### `UpdateIssueInput` (`internal/storage/issues.go:61-72`)
The field-axis patch. Only columns the field axis owns are representable, so a status write through the field path is unconstructible (`internal/storage/issues.go:58-60`).

| Field | Type | Semantics |
|---|---|---|
| `Title` | `*string` | nil = leave unchanged (`internal/storage/issues.go:62`) |
| `Description` | `*string` | `internal/storage/issues.go:63` |
| `Prompt` | `*string` | `internal/storage/issues.go:64` |
| `IssueType` | `*model.IssueType` | `internal/storage/issues.go:65` |
| `Priority` | `*model.Priority` | `internal/storage/issues.go:66` |
| `Assignee` | `*string` | `internal/storage/issues.go:67` |
| `Lane` | `*string` | `internal/storage/issues.go:68` |
| `Labels` | `*[]string` | `internal/storage/issues.go:69` |
| `Reason` | `string` | Optional free text recorded on the field-change event (`internal/storage/issues.go:70-71`) |

- `IsEmpty()` is true iff **all eight pointer fields** are nil; `Reason` is deliberately excluded from the emptiness test (`internal/storage/issues.go:74-77`).

#### `Change` (`internal/storage/issues.go:95-100`)
| Field | Type | Semantics |
|---|---|---|
| `Action` | `model.Action` | nil = no transition. Which axis it drives (status machine vs retention) is the sum's own structure — the `StatusAction`/`RetentionAction` partition — never a caller-side mode (`internal/storage/issues.go:79-87`, `:96`) |
| `Fields` | `UpdateIssueInput` | empty = no field mutations (`internal/storage/issues.go:97`) |
| `Actor` | `string` | THE actor for the whole change — one call, one author, recorded on both events it may produce (`internal/storage/issues.go:90-92`, `:98`) |
| `Reason` | `string` | Belongs to the **transition** event; `Fields.Reason` belongs to the **field-change** event. A combined change records two events whose reasons are independently set (`internal/storage/issues.go:92-94`, `:99`) |

- `IsEmpty()` is true iff `Action == nil && Fields.IsEmpty()` (`internal/storage/issues.go:102-104`).

#### `SortSpec` (`internal/storage/issues.go:106-109`)
- `Field string`, `Desc bool`.

#### `SortFields` — the closed set (`internal/storage/issues.go:131-142`)
Exactly ten keys: `"id"`, `"title"`, `"status"`, `"priority"`, `"rank"`, `"type"`, `"topic"`, `"assignee"`, `"created_at"`, `"updated_at"`.
- Exists because each engine binds these names to its own mechanism (Dolt → SQL columns; memory → comparison functions); the set lives in the contract and each engine's binding is checked against it by the conformance suite (`internal/storage/issues.go:113-119`).
- **Documented fault**: `"status"` orders the STORED status encoding, not derived lifecycle state. A container has no stored status, so it carries no value on this axis and orders ahead of every leaf ascending, behind every leaf descending, whatever state it derives to. The same listing's status FILTER reads derived state, so filter and sort disagree about what "status" means. This is stated as shipped behavior so both engines are wrong identically; correcting it is ticket `links-store-seam-q35v.6` (`internal/storage/issues.go:121-130`).

#### `ListIssuesFilter` (`internal/storage/issues.go:155-171`)
Rule for the whole struct: **every slice is an OR within itself and an AND against the other criteria**; every zero value means "do not constrain on this axis" (`internal/storage/issues.go:144-152`).

| Field | Type | Effect |
|---|---|---|
| `Statuses` | `[]model.State` | `internal/storage/issues.go:156` |
| `Resolutions` | `[]model.Resolution` | `internal/storage/issues.go:157` |
| `IssueTypes` | `[]model.IssueType` | include-list (`internal/storage/issues.go:158`) |
| `ExcludeIssueTypes` | `[]model.IssueType` | exclude-list (`internal/storage/issues.go:159`) |
| `Assignees` | `[]string` | `internal/storage/issues.go:160` |
| `SearchTerms` | `[]string` | `internal/storage/issues.go:161` |
| `IDs` | `[]string` | `internal/storage/issues.go:162` |
| `HasComments` | `*bool` | nil = unconstrained (`internal/storage/issues.go:163`) |
| `LabelsAll` | `[]string` | `internal/storage/issues.go:164` |
| `UpdatedAfter` | `*time.Time` | `internal/storage/issues.go:165` |
| `UpdatedBefore` | `*time.Time` | `internal/storage/issues.go:166` |
| `IncludeArchived` | `bool` | one of the two axes whose default is a filter rather than an absence (`internal/storage/issues.go:153-154`, `:167`) |
| `IncludeDeleted` | `bool` | ditto (`internal/storage/issues.go:168`) |
| `SortBy` | `[]SortSpec` | `internal/storage/issues.go:169` |
| `Limit` | `int` | `internal/storage/issues.go:170` |

#### Edge inputs (`internal/storage/edges.go`)
- `AddCommentInput{IssueID, Body, CreatedBy string}` — `internal/storage/edges.go:5-9`
- `AddLabelInput{IssueID, Name, CreatedBy string}` — `internal/storage/edges.go:11-15`
- `AddRelationInput{SrcID, DstID string; Type model.RelationType; CreatedBy string}` — `internal/storage/edges.go:17-22`
- `SetParentInput{ChildID, ParentID, CreatedBy string}` — `internal/storage/edges.go:24-28`

#### `IssueRelations` (`internal/storage/edges.go:43-49`)
- `Issue model.Issue`, `Parent *model.Issue`, `Children []model.Issue`, `DependsOn []model.Issue`, `Blocks []model.Issue`.
- Lightweight per-issue shape WITHOUT the comment/event/related payload `GetIssueDetail` loads (`internal/storage/edges.go:30-35`).
- **Direction convention is contract**: a blocks edge runs `src=dependent`, `dst=dependency`, so `DependsOn` and `Blocks` are the two readings of one edge set. Two engines bucketing differently would disagree about which work is ready (`internal/storage/edges.go:39-42`).

---

### 1.14 Bulk / import spec types (`internal/storage/bulk.go`)

#### `BulkIssueSpec` (`internal/storage/bulk.go:19-37`)
`ID` is the selector: present → the doc is an **update patch** of only the fields it sets; absent → the doc **creates** an issue and behaves like `ImportTreeSpec`'s flat form (`internal/storage/bulk.go:3-9`). Pointer fields are exactly the update-patch fields: nil = leave unchanged, set = write this value (`internal/storage/bulk.go:9-15`). The struct tags are contract, not engine detail (`internal/storage/bulk.go:17-18`).

| Field | Type | YAML tag |
|---|---|---|
| `LocalID` | `string` | `local_id,omitempty` (`:20`) |
| `ID` | `string` | `id,omitempty` (`:21`) |
| `Title` | `*string` | `title,omitempty` (`:22`) |
| `Description` | `*string` | `description,omitempty` (`:23`) |
| `Prompt` | `*string` | `prompt,omitempty` (`:24`) |
| `IssueType` | `*string` | `type,omitempty` (`:25`) |
| `Topic` | `*string` | `topic,omitempty` (`:26`) |
| `Priority` | `*int` | `priority,omitempty` (`:27`) |
| `Assignee` | `*string` | `assignee,omitempty` (`:28`) |
| `Labels` | `*[]string` | `labels,omitempty` (`:29`) |
| `Lane` | `*string` | `lane,omitempty` (`:30`) |
| `Parent` | `string` | `parent,omitempty` (`:31`) |
| `DependsOn` | `[]string` | `depends_on,omitempty` (`:32`) |
| `Reason` | `string` | `reason,omitempty`; applies only to updates — there is no prior state to annotate a create against (`internal/storage/bulk.go:33-36`) |

#### `BulkApplyResult` (`internal/storage/bulk.go:45-48`)
- `Created map[string]string` — maps each create document's own reference (its `LocalID` if set, otherwise its new real ID) to the real ID it was created under, so every create is nameable even when the file gave no `LocalID` (`internal/storage/bulk.go:39-43`).
- `Updated []string` — real IDs of every updated issue, in the order applied (`internal/storage/bulk.go:43-44`).

#### `ImportTreeSpec` (`internal/storage/bulk.go:53-65`)
JSON tags: `local_id`, `title`, `description,omitempty`, `prompt,omitempty`, `type`, `topic`, `priority`, `assignee,omitempty`, `labels,omitempty`, `parent,omitempty`, `depends_on,omitempty`. `LocalID` is opaque — used inside the spec to wire `Parent` and `DependsOn`, replaced with the generated lit issue ID at import time (`internal/storage/bulk.go:50-52`).

#### `ImportTreeResult` (`internal/storage/bulk.go:69-71`)
- `IDMap map[string]string` (json `id_map`) — local-ID → real-issue-ID mapping.

---

### 1.15 Authored-file parsers (`internal/storage/specs.go`)

They live beside the specs rather than in an engine because the schema is the contract's; every engine reads the same authored file the same way (`internal/storage/specs.go:13-19`).

**`ParseBulkSpecs(data []byte) ([]BulkIssueSpec, error)`** — `internal/storage/specs.go:25-40`
- YAML decoder with `KnownFields(true)`: any field the schema does not name is rejected (`internal/storage/specs.go:26-27`, `:22-24`).
- Loops decoding documents until `io.EOF`; **multi-document YAML** (`---`-separated) yields one spec per document (`internal/storage/specs.go:29-38`).
- Any non-EOF decode error → `fmt.Errorf("bulk: parse spec: %w", err)` (`internal/storage/specs.go:35`).
- Test: an unknown field `children` produces an error naming `"children"` (`internal/storage/specs_test.go:13-19`).
- Test: two `---`-separated documents produce 2 specs (`internal/storage/specs_test.go:21-31`).

**`ParseImportTreeSpecs(data []byte) ([]ImportTreeSpec, error)`** — `internal/storage/specs.go:50-61`
- JSON decoder with `DisallowUnknownFields()` (`internal/storage/specs.go:52`).
- Decodes exactly one array; decode error → `fmt.Errorf("import: parse spec: %w", err)` (`internal/storage/specs.go:54-56`).
- Trailing data after the array (`dec.More()`) → `errors.New("import: unexpected trailing data after spec array")` (`internal/storage/specs.go:57-59`).
- Test: a nested `children` array is rejected by name (`internal/storage/specs_test.go:36-43`).
- Test: two concatenated arrays produce a "trailing data" error (`internal/storage/specs_test.go:45-52`).

---

### 1.16 `ParseSortSpecs` (`internal/storage/sort.go:18-50`)

- Input: comma-separated sort expression, e.g. `"rank:asc,updated_at:desc"` (`internal/storage/sort.go:8-9`).
- Splits on `,`; each part is trimmed; empty parts are skipped (`internal/storage/sort.go:20-24`).
- A bare field (`"rank"`) defaults to ascending (`internal/storage/sort.go:10`, `:25-26`).
- With a `:`, the value is split into at most 2 chunks; field is trimmed; direction is lowercased and trimmed (`internal/storage/sort.go:27-30`).
- `"asc"` → `Desc=false`; `"desc"` → `Desc=true`; anything else → `ValidationError{Message: fmt.Sprintf("unsupported sort direction %q", direction)}` (`internal/storage/sort.go:31-42`).
- Empty and whitespace-only expressions yield `nil, nil` (`internal/storage/sort.go:10-11`, `:46-48`).
- It is THE parser from sort expression to `[]SortSpec`; both the `--sort` flag and the `--query sort:` token route through it (`internal/storage/sort.go:12-17`).
- Note: the field name is **not** validated against `SortFields` here — that rejection happens in the engine (`internal/storage/sort.go:44`; cf. `internal/storage/memory/list.go:255-258`).

---

### 1.17 Capabilities system (`internal/storage/capabilities.go`)

**Granularity rule**: two operations share a capability only when no engine could plausibly offer one without the other (`internal/storage/capabilities.go:24-28`).
**Absence rule**: absence is answered, never guessed or faked — a caller asks with `Of`, which returns the interface or an `UnsupportedError` (`internal/storage/capabilities.go:30-34`).

#### `Capability` interface (`internal/storage/capabilities.go:239-251`)
- `Name() string` — stable identifier used in messages and listings (`internal/storage/capabilities.go:241-242`).
- `OfferedBy(engine Store) bool` — the enumeration question (`internal/storage/capabilities.go:244-247`).
- `unexported()` — seals the interface to this package (`internal/storage/capabilities.go:249-250`); type is closed so only this package can mint a capability (`internal/storage/capabilities.go:236-238`).

#### `capability[C any]{name string}` (`internal/storage/capabilities.go:255-285`)
- `Name()` returns `c.name` (`internal/storage/capabilities.go:257`).
- `OfferedBy(engine)` is derived from `Of` (`_, err := c.Of(engine); return err == nil`) so the two cannot disagree (`internal/storage/capabilities.go:264-267`).
- `Of(engine Store) (C, error)`: type-asserts `any(engine).(C)`; on failure returns the zero `C` and `UnsupportedError{Capability: c.name, Engine: fmt.Sprintf("%T", engine)}` (`internal/storage/capabilities.go:278-285`).

#### The seven capabilities (`internal/storage/capabilities.go:291-299`)
| Value | Name string | Interface |
|---|---|---|
| `Sync` | `"sync"` | `Syncer` |
| `Reconcile` | `"reconcile"` | `Reconciler` |
| `Checkpoints` | `"checkpoints"` | `Checkpointer` |
| `Repair` | `"repair"` | `Repairer` |
| `SchemaMigration` | `"schema-migration"` | `SchemaMigrator` |
| `Import` | `"import"` | `Importer` |
| `TestSupport` | `"test-support"` | `RawExecutor` |

- `all` is the enumeration every listing derives from (`internal/storage/capabilities.go:304-312`).
- `Capabilities() []Capability` returns `slices.Clone(all)` — the caller's own slice (`internal/storage/capabilities.go:317`).
- `Offered(engine Store) []Capability` returns the capabilities the engine implements, **in `Capabilities()` order** (`internal/storage/capabilities.go:323-331`).

#### `Syncer` (`internal/storage/capabilities.go:55-96`)
- `SyncAddRemote(ctx, name, url string) error` (`:56`)
- `SyncRemoveRemote(ctx, name string) error` (`:57`)
- `SyncListRemotes(ctx) ([]SyncRemote, error)` (`:58`)
- `SyncStatus(ctx) (SyncStatusReport, error)` — local side only: build, position, pending changes, peers; **contacts no network** (`:60-62`)
- `SyncFreshness(ctx, remote, branch string) (SyncFreshness, error)` — local branch position against a peer as of the last fetch or push; reads local refs, never contacts the network, which is what lets a read-only command call it (`:64-67`)
- `SyncFetch(ctx, remote string, prune bool) error` (`:69`)
- `SyncPush(ctx, remote, branch string, setUpstream, force bool) (SyncPushResult, error)` (`:70`)
- `SyncPull(ctx, remote, branch string) (SyncPullResult, error)` (`:71`)
- `SyncReceive(ctx, remote, branch string) (SyncReceiveResult, error)` — fetches and fast-forwards when and only when local is strictly behind; never merges; a divergence it meets is reported, never healed here (`:73-76`)
- `SyncCompact(ctx, mode GCMode) (CompactionOutcome, error)` — reclaims local storage at the requested depth with no remote involved (`:78-80`)
- `CompactIfDue(ctx) (CompactionOutcome, error)` — compacts only when the engine's own accounting says a pass is owed; the engine owns that judgment because what makes a pass due is a fact about how it stores data (`:82-88`)
- `SyncCompactAndPush(ctx, remote, branch string, setUpstream, force bool) (SyncPushResult, error)` (`:89`)
- `GetSyncState(ctx) (SyncState, error)` / `RecordSyncState(ctx, state SyncState) error` — carry the staleness marker across commands (`:91-95`)
- Compaction sits inside `Syncer` because `SyncCompactAndPush` compacts and pushes under a single commit-lock acquisition — one atomic operation not assemblable from `SyncCompact` + `SyncPush` (`internal/storage/capabilities.go:40-50`).
- Nothing in `Syncer` resolves a divergence — that is `Reconciler` (`internal/storage/capabilities.go:52-54`).

#### `Reconciler` (`internal/storage/capabilities.go:106-128`)
- `SyncReconcile(ctx, remote, branch string) (SyncReconcileResult, error)` — merges a divergence field-aware and replays it into linear history, or — when a free-text field moved on both sides — holds the conflict and commits nothing (`:108-110`)
- `SyncReconcileCombine(ctx, remote, branch string) (SyncReconcileResult, error)` — settles an unrelated-history divergence by union, keeping every issue from both sides (`:112-114`)
- `SyncReconcileResolved(ctx, remote, branch string, resolutions []merge.ProseResolution) (SyncReconcileResult, error)` — finishes a reconcile held for prose (`:116-118`)
- `SyncResolveUnrelated(ctx, remote, branch string, choice UnrelatedResolution, ownerApproval string) (SyncReconcileResult, error)` — settles by taking one side wholesale; destroys the other side's unique issues, which is why it takes an owner approval bound to this exact fork rather than a bare confirmation flag (`:120-124`)
- `SyncResetToRemoteHead(ctx, remote, branch string) error` — abandons local history for the peer's (`:126-127`)

#### `Checkpointer` (`internal/storage/capabilities.go:136-144`)
- `CreateCheckpoint(ctx, prefix string) (Checkpoint, error)` (`:137`)
- `ListCheckpoints(ctx, prefix string) ([]Checkpoint, error)` (`:138`)
- `PruneCheckpoints(ctx, prefix string, retain int) error` — keeps the newest `retain` checkpoints under prefix and drops the rest (`:140-142`)
- `ResetToCheckpoint(ctx, name string) error` (`:143`)

#### `Repairer` (`internal/storage/capabilities.go:154-173`)
- `Doctor(ctx) (HealthReport, error)` — examines and reports; changes nothing (`:155-156`)
- `FixIntegrity(ctx) (HealthReport, error)` — repairs dangling rows, self-referential edges, edges stored in the wrong order; reports the state it left behind. Takes **no** "actually repair" flag because the examine-only arm is `Doctor` (`:158-166`)
- `FixRankInversions(ctx) (int, error)` — repairs orderings that contradict themselves and reports how many it corrected; exists only because rank may be stored as a fractional position that concurrent writers can invert (`:168-172`)

#### `SchemaMigrator` (`internal/storage/capabilities.go:182-189`)
- `AppliedSchemaVersion(ctx) (int64, error)` — the shape version the store is currently at (`:183-184`)
- `Downgrade(ctx, targetSchemaVersion int64) error` — moves the store back to an older shape so a binary that predates the current one can open it (`:186-188`)

#### `Importer` (`internal/storage/capabilities.go:198-200`)
- `ReplaceFromExport(ctx, export model.Export) error` — replaces the store's entire contents with an export (`:191`, `:199`). Only the import half is optional; `Export` is core (`:192-196`).

#### `RawExecutor` (`internal/storage/capabilities.go:210-212`)
- `ExecRawForTest(ctx, query string, args ...any) error` — runs an engine-native statement so tests can plant states the contract cannot express (a corrupted row, a stale schema) (`:202-211`).

---

### 1.18 Sync/reconcile vocabulary (`internal/storage/sync.go`)

These types live in the contract, not in an engine, because a capability interface can only name types every engine can name (`internal/storage/sync.go:9-16`).

- **`SyncState{Path, ContentHash string}`** — the store's on-disk content at a point in time: where it lives and a digest of what it held; the staleness signal (`internal/storage/sync.go:20-23`).
- **`SyncRemote{Name, URL string}`** (json `name`, `url`) — `internal/storage/sync.go:26-29`.
- **`SyncStatusRow{TableName string; Staged bool; Status string}`** (json `table_name`, `staged`, `status`) — one unit of pending local change; what a "table" is belongs to the engine, and the contract carries the row through uninterpreted (`internal/storage/sync.go:33-38`).
- **`SyncStatusReport`** (`internal/storage/sync.go:43-50`) — `DoltVersion` (json `dolt_version`), `Branch`, `HeadCommit`, `HeadMessage`, `Status []SyncStatusRow`, `Remotes []SyncRemote`.
- **`SyncFreshnessState`** string enum (`internal/storage/sync.go:57-65`): `never_synced`, `up_to_date`, `ahead`, `behind`, `diverged`.
- **`SyncFreshness`** (`internal/storage/sync.go:74-90`) — `Remote`, `Branch`, `Synced bool`, `Ahead int64`, `Behind int64`, `OldestDivergedUnix int64`.
  - Reports position relative to `remotes/<Remote>/<Branch>`, so `Behind` is "as of last fetch"; computing it never contacts the network (`internal/storage/sync.go:68-71`).
  - `Synced` is false when the ref does not exist; `Ahead`/`Behind` are zero in that state (`internal/storage/sync.go:71-73`).
  - `OldestDivergedUnix` is the Unix seconds of the OLDEST commit in the union of the two divergent ranges — when the fork first happened. Zero when nothing diverged (both counts 0) or never synced. Raw timestamp, not an age (`internal/storage/sync.go:80-89`).
  - **`State()`** derivation (`internal/storage/sync.go:95-109`): `!Synced` → `never_synced`; `Ahead==0 && Behind==0` → `up_to_date`; `Behind==0` → `ahead`; `Ahead==0` → `behind`; otherwise → `diverged`.
- **`SyncReceiveState`** enum (`internal/storage/sync.go:114-133`): `up_to_date` (local already at remote head, fetch found nothing); `fast_forwarded` (local strictly behind, advanced with no merge commit — the only state that mutates local data); `ahead` (local has unpushed commits and remote has nothing new); `diverged` (both moved; fast-forward impossible; the background receive deliberately does NOT merge); `never_synced` (no remote-tracking ref even after a fetch).
- **`SyncReceiveResult{State SyncReceiveState; Ahead, Behind, OldestDivergedUnix int64}`** — `OldestDivergedUnix` is zero unless diverged (`internal/storage/sync.go:135-144`).
- **`SyncPullState`** enum (`internal/storage/sync.go:151-178`): `up_to_date`, `fast_forwarded`, `linearized`, `prose_pending` (every code-owned field settled but a free-text field diverged on both sides; nothing committed), `unrelated_histories` (no common ancestor; nothing committed), `ahead`, `never_synced`.
- **`SyncPullResult`** (`internal/storage/sync.go:183-197`) — `State` (json `state`), `Ahead`, `Behind`, `Pending []merge.ProsePending` (json `pending,omitempty`), `OldestDivergedUnix` (zero unless the pull met a divergence), `Unrelated *UnrelatedInventory` (non-nil only for `SyncPullUnrelated`).
- **`GCMode`** (`internal/storage/sync.go:207-218`): `GCNewGen = iota` — collects recent history not yet archived; the cheap routine depth, cannot reclaim anything already archived. `GCFull` — additionally rewrites archived history; the only depth that reclaims what earlier passes left behind; costs proportionally to the whole store.
  - `Valid()` returns true only for `GCNewGen` and `GCFull`; it is the door guard a `Syncer` runs before collecting, so an out-of-range depth is rejected loudly rather than collapsing to the shallower default (`internal/storage/sync.go:220-233`).
  - `String()` → `"newgen"`, `"full"`, or `fmt.Sprintf("unknown(%d)", int(m))` (`internal/storage/sync.go:236-244`).
- **`CompactionOutcome`** (`internal/storage/sync.go:248-272`) — `Ran bool` (whether a pass was actually performed; a due-check that found nothing owing returns false, an ordinary outcome and not a failure); `Depth GCMode` (meaningful only when `Ran`); `Detail string` (the engine's own already-rendered account; an engine that could not measure its own reclaim says so here rather than blanking the field — empty belongs only to the outcome that did nothing).
- **`SyncPushResult`** (`internal/storage/sync.go:275-293`) — `Status int64` (json `status`), `Message string` (json `message`, the engine's verbatim push output, rendered as `raw`), `Maintenance string` (json `maintenance,omitempty`; deliberately separate from `Message` so `raw` stays raw; empty when nothing worth reporting, and every state a reader would act on — work performed, work declined, an I/O failure — is non-empty).
- **`SyncReconcileState`** enum (`internal/storage/sync.go:298-354`): `not_diverged`, `linearized`, `prose_pending`, `unrelated_histories`, `took_local`, `took_remote`, `combined`. `took_local`/`took_remote` are produced only by `SyncResolveUnrelated`; the autonomous reconcile never picks a side. `combined` is produced only by the combine resolution, and only when every prose field settled — an on-both prose divergence lands `prose_pending` instead.
- **`SyncReconcileResult`** (`internal/storage/sync.go:359-384`) — `State`, `Ahead`, `Behind`, `LocalHead`, `RemoteHead`, `BaseCommit`, `Pending []merge.ProsePending` (empty unless `prose_pending`), `Unrelated *UnrelatedInventory` (non-nil only for `unrelated_histories`), `Replayed int` (counts the folded side's commits that landed individually; zero for every non-mutating outcome and for a fold whose every per-commit projection was already contained in the spine; read back off the spine after the replay).
- **`UnrelatedInventory`** (`internal/storage/sync.go:394-398`) — `OnlyLocal`, `OnlyRemote`, `OnBoth []string` (json `only_local,omitempty` etc.). The three slices are sorted and mutually disjoint by construction; every id present on either side lands in exactly one (`internal/storage/sync.go:386-393`).
- **`UnrelatedResolution`** string (`internal/storage/sync.go:409-418`): `TakeLocal = "local"` (keeps local backlog, discards remote-only issues), `TakeRemote = "remote"` (keeps remote backlog, discards local-only issues).
  - `Valid()` returns true only for those two; the door guard every `Reconciler` runs before touching the store (`internal/storage/sync.go:420-431`).

---

### 1.19 Checkpoint/repair vocabulary (`internal/storage/maintenance.go`)

- **`Checkpoint`** (`internal/storage/maintenance.go:19-30`) — `Name string` (format `"<prefix>-<unix-nano>"`), `Prefix string` (caller label, e.g. `"pre-migrate"`), `CreatedAt time.Time` (parsed from the unix-nano suffix in `Name`), `Anchor string` (opaque engine-side identity of the captured state; the contract requires only that handing it back names the same state, never that it is a hash or a commit).
  - The name encodes the prefix and timestamp so `ListCheckpoints` can reconstruct the set without external metadata storage (`internal/storage/maintenance.go:16-18`).
- **`HealthReport`** (`internal/storage/maintenance.go:37-46`) — json keys: `integrity_check` (string), `foreign_key_issues` (int), `invalid_related_rows` (int), `orphan_history_rows` (int), `rank_inversions` (int), `dependency_cycle` ([]string), `errors` ([]string), `warnings` ([]string).
  - An engine reports **zeros** for checks it has no analogue for rather than omitting them (`internal/storage/maintenance.go:33-36`).

---

### 1.20 Contract-package tests (`internal/storage/capabilities_test.go`)

- Fakes satisfy interfaces by embedding a nil interface, so a fake declares what it offers in one line and panics only if a test calls a method it never claimed (`internal/storage/capabilities_test.go:18-23`).
- `TestEngineOffersOnlyWhatItImplements`: for each of the seven capabilities, an engine offering only that one reports exactly one capability from `Offered`, and every other capability's `OfferedBy` returns false (`internal/storage/capabilities_test.go:96-112`).
- `TestAbsentCapabilityAnswersWithTypedAbsence`: an engine implementing only `Store` reports zero offered capabilities; all seven `.Of()` calls return `UnsupportedError` with the correct `Capability` name and a non-empty `Engine` (`internal/storage/capabilities_test.go:114-150`).
- `TestSyncWithoutReconcileIsRepresentable`: an engine offering `Syncer` but not `Reconciler` is representable; `Of` hands back the engine itself, not a wrapper, and the `GCMode` depth survives the crossing (`internal/storage/capabilities_test.go:157-199`).
- `TestOfferedFollowsCapabilitiesOrder`: an engine offering everything reports the same order as `Capabilities()` (`internal/storage/capabilities_test.go:201-207`).
- `TestCapabilitiesIsTheCallersOwnSlice`: mutating a returned slice does not change the enumeration (`internal/storage/capabilities_test.go:209-217`).
- `TestCapabilityNamesAreDistinctAndSpoken`: no capability has an empty name; no two share a name (`internal/storage/capabilities_test.go:219-230`).
- `TestEveryCapabilityIsEnumerated`: parses the package's own AST for `capability[...]` composite literals and asserts the declared names equal `Capabilities()` (`internal/storage/capabilities_test.go:237-276`).
- `TestGCModeValidAcceptsOnlyTheContractsDepths`: `GCNewGen` and `GCFull` are valid; `GCMode(-1)`, `GCMode(2)`, `GCMode(99)` are not (`internal/storage/capabilities_test.go:316-329`).

---

## PART 2 — `internal/storage/memory` (the in-memory backend)

### 2.1 Persistence: how it persists (short answer — it does not)

- The package implements the contract with nothing but Go values: **no SQL, no disk, no schema, no engine artifact of any kind** (`internal/storage/memory/doc.go:1-2`).
- `Close()` releases what the engine holds, which is nothing: its state is Go memory the garbage collector owns (`internal/storage/memory/engine.go:126-129`).
- **There is no on-disk format.** All state lives in the `Engine` struct's fields (`internal/storage/memory/engine.go:33-62`). Nothing in the package opens, reads, or writes a file.

### 2.2 `Engine` state layout (`internal/storage/memory/engine.go:33-62`)

| Field | Type | Role |
|---|---|---|
| `mu` | `sync.Mutex` | Makes the engine safe to share (`:34`) |
| `workspaceID` | `string` | Scopes the attribution stamp; taken at construction because a stream token with no workspace is a half-fact `model.NewAttribution` refuses to carry (`:36-39`) |
| `attribution` | `model.Attribution` | Current attribution stamp (`:40`) |
| `issues` | `map[string]*record` | The issue table (`:42`) |
| `order` | `[]string` | **THE** total rank order, top first. `Issue.Rank` is rendered from a position in this slice at read time and stored nowhere, so a rank that contradicts the order is unrepresentable — which is why nothing here needs the inversion repair Dolt offers (`:44-49`) |
| `relations` | `[]model.Relation` | Edge table, insertion order (oldest-first by construction) (`:51-54`) |
| `comments` | `[]model.Comment` | Comment table, insertion order (`:55`) |
| `events` | `[]model.IssueEvent` | History, insertion order (`:56`) |
| `labels` | `map[string][]model.Label` | Keyed by issue, kept **sorted by name** because the sorted set is what every label read returns (`:58-61`) |

**`record`** (`internal/storage/memory/engine.go:71-89`): `id`, `title`, `description`, `prompt`, `issueType model.IssueType`, `topic`, `assignee`, `lane`, `priority model.Priority`, `createdAt`, `updatedAt time.Time`, `status model.StatusView`, `retention model.Retention`.
- **Rank is deliberately absent** from `record`; position lives in `Engine.order`, and a rank string beside it would be a second representation of one fact (`internal/storage/memory/engine.go:66-70`).
- `status` is the leaf status view; a container's state derives from its children, so its view stays zero and hydration never reads it — the same place Dolt writes a NULL status column (`internal/storage/memory/engine.go:85-87`).

**`const createdBy = "links"`** (`internal/storage/memory/engine.go:95`) — the author every structural row the engine writes on a caller's behalf carries: the parent edge and initial labels a create wires, and the create event itself. It matches Dolt's spelling because it lands in exported history a user reads (`internal/storage/memory/engine.go:91-95`).

**`exportVersion = 2`** (`internal/storage/memory/export.go:16`) — the export schema version; it is the contract's current version, not the engine's, because a differing version number would make two identical stores compare unequal (`internal/storage/memory/export.go:12-16`).

### 2.3 Construction and concurrency

**`New(workspaceID string) (*Engine, error)`** (`internal/storage/memory/engine.go:105-115`)
- Trims `workspaceID`; empty (after trim) → `errors.New("workspace id is required")` (`internal/storage/memory/engine.go:106-109`).
- Initializes `issues` and `labels` maps; `order`, `relations`, `comments`, `events` start nil (`internal/storage/memory/engine.go:110-114`).
- The workspace id is required rather than defaulted because attribution is a complete pair or nothing (`internal/storage/memory/engine.go:98-104`).
- Tested: `New("")` and `New("   ")` both error (`internal/storage/memory/capabilities_test.go:55-62`).

**Concurrency discipline** (`internal/storage/memory/engine.go:28-32`):
- Every exported method locks the mutex and immediately delegates to an unexported one; no unexported method ever locks. That is what lets `BulkApply` drive `CreateIssue` and `Apply` for a whole batch under one hold without deadlocking on itself.
- The mutex is a plain `sync.Mutex` (not RWMutex), so reads serialize with writes.
- `Export` calls `sortEvents(cloneEvents(...))` rather than `ListAllEvents` precisely because the latter would deadlock on the non-reentrant mutex (`internal/storage/memory/export.go:49-55`).
- `TestConcurrentUseIsSerialized` runs 8 goroutines × 6 iterations of create/apply/list, then asserts `LocalIssueCount == 48`, that the listing holds the same count, and that no two issues share a `Rank` (`internal/storage/memory/engine_test.go:23-84`).

**Ownership**: the engine owns every byte of its state and hands none out; each read composes a fresh model value, so a caller holding a previously-read issue cannot mutate the engine through it (`internal/storage/memory/engine.go:23-27`). The one value that would have aliased is `model.IssueEvent.Changes`, which `cloneEvents` copies one level deeper than the slice (`internal/storage/memory/engine.go:308-323`).

**`AttributeTo(streamToken string)`** (`internal/storage/memory/engine.go:120-124`) — locks, then sets `e.attribution = model.NewAttribution(streamToken, e.workspaceID)`. An empty token leaves it unattributed rather than half-attributed (`internal/storage/memory/engine.go:117-119`).

**`e.now()`** returns `time.Now().UTC()` — the engine's clock, read at the write boundary (`internal/storage/memory/engine.go:325-327`).

### 2.4 Internal derivations

- **`mustRecord(id)`** — the one place "you named an issue that isn't here" is decided; returns `storage.NotFoundError{Entity: "issue", ID: id}` (`internal/storage/memory/engine.go:136-142`).
- **`positions()`** — derives an id→index map from `order` on every read rather than maintaining a cached index (`internal/storage/memory/engine.go:151-157`).
- **`rankAt(index int) string`** — renders a position as `fmt.Sprintf("%09d", index)`. Any encoding whose ascending string order matches the engine's order satisfies the contract; the width is fixed so the comparison stays lexicographic (`internal/storage/memory/engine.go:159-164`).
- **`hydrate(rec, pos)`** (`internal/storage/memory/engine.go:172-194`) — composes `model.Issue` from the record: `ID`, `Title`, `Description`, `Prompt`, `Priority`, `IssueType`, `Topic`, `Assignee`, `Rank: rankAt(pos[rec.id])`, `Lane`, `Labels: e.labelNames(rec.id)`, `CreatedAt`, `UpdatedAt`; then `SetRetention(rec.retention)`; then `model.HydrateRow(issue, rec.status, children)` with `children` = lifecycle children.
- **`lifecycleChildren`** (`internal/storage/memory/engine.go:214-225`) — a non-container returns nil; a container returns its rank-ordered children filtered by `visibleUnder`.
- **`visibleUnder(parent, child model.Retention) bool`** (`internal/storage/memory/engine.go:232-234`) — `model.Frozen(parent) || !model.Frozen(child)`. A live container shows only live children, so archiving a child removes it from the epic's progress; a container itself out of the flow keeps its whole child set (`internal/storage/memory/engine.go:227-231`).
- **`childRecords(parentID, pos)`** (`internal/storage/memory/engine.go:237-249`) — scans `relations` for `RelParentChild` edges with `DstID == parentID`, collects existing `SrcID` records, then `slices.SortStableFunc` by `pos[a.id] - pos[b.id]` (rank order).
- **`labelNames(issueID)`** (`internal/storage/memory/engine.go:254-261`) — returns the stored rows' names in stored (sorted-by-name) order.
- **`recordEvent(issueID, spec, now)`** (`internal/storage/memory/engine.go:282-297`) — the ONE place history is written. Sets `ID: "evt-" + uuid.NewString()`, `IssueID`, `Action: strings.TrimSpace(spec.action)`, `Reason: strings.TrimSpace(spec.reason)`, `Actor` (trimmed; empty → `"unknown"`, `:283-286`), `CreatedAt: now`, `Attribution: e.attribution` (read off the engine here, not passed in), `Changes: spec.changes`.
- **`recordEvents`** writes every event a mutation owed, in order; a mutation that moved nothing owes none (`internal/storage/memory/engine.go:299-306`).

### 2.5 `CreateIssue` (`internal/storage/memory/issues.go:18-98`)

Order of checks is stated as contract: the parent must be resolved before the cosmetic prefix, so naming a missing parent reports the missing issue (`internal/storage/memory/issues.go:24-29`).

1. `title = strings.TrimSpace(in.Title)`; empty → `errors.New("title is required")` (`:31-34`).
2. `canonicalLabels(in.Labels)` — normalize/dedupe/sort; error propagates (`:35-38`).
3. `issueid.NormalizeTopicForCreate(in.Topic)` — error propagates (`:39-42`).
4. `issueType`: if `in.IssueType == ""` → `model.TypeTask` (`:43-49`).
5. `parentID = strings.TrimSpace(in.ParentID)`; if non-empty, `mustRecord(parentID)` → `NotFoundError` on miss (`:50-55`).
6. `issueid.NormalizeConfiguredPrefix(in.Prefix)`; error → `fmt.Errorf("normalize issue prefix: %w", err)` (`:56-59`).
7. `now := e.now()`; `mintID(...)` (`:60-64`).
8. Builds the record with all string fields `strings.TrimSpace`'d (description, prompt, assignee, lane), `status: model.StatusView{Value: model.StateOpen}`, `retention: model.Live{}` (`:66-80`).
9. `e.issues[id] = rec`; `e.place(id, in.Placement)`; `e.setLabels(id, labels, now, createdBy)` (`:81-83`).
10. If `parentID != ""`, appends `model.Relation{SrcID: id, DstID: parentID, Type: model.RelParentChild, CreatedAt: now, CreatedBy: createdBy}` (`:84-88`).
11. Records one `created` event, `reason: "issue created"`, `actor: createdBy`. Changes: a leaf records one `FieldChange{Field:"status", From:"", To:"open"}`; **a container records none** (`:89-95`).
12. Returns `e.hydrate(rec, e.positions())` (`:97`).

**`place(id, placement)`** (`internal/storage/memory/issues.go:103-109`) — `RankTop` prepends; anything else (i.e. `RankBottom`, the zero value) appends.

**`mintID`** (`internal/storage/memory/issues.go:115-129`)
- With a parent: `nextChildID(parentID)`.
- Without: `baseLength = min(issueid.ComputeAdaptiveLength(topLevelCount()), issueid.MaxHashLength)`; then for `length` from `baseLength` to `issueid.MaxHashLength`, for `nonce` in `[0, issueid.NonceAttempts)`, generates `issueid.GenerateHashID(prefix, topic, title, description, createdBy, createdAt, length, nonce)` and returns the first candidate not already in `issues`.
- Exhaustion → `fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, issueid.MaxHashLength)` (`:128`).

**`topLevelCount()`** counts ids not containing `"."` (`internal/storage/memory/issues.go:131-139`).

**`nextChildID(parentID)`** (`internal/storage/memory/issues.go:144-158`) — finds the highest integer suffix among **direct** children (a suffix containing another `.` is skipped, so grandchildren do not count), returns `fmt.Sprintf("%s.%d", parentID, highest+1)`.

### 2.6 Reads

**`GetIssue`** (`internal/storage/memory/issues.go:160-172`) — `mustRecord` then `hydrate`.

**`GetIssueDetail`** (`internal/storage/memory/issues.go:174-233`) — returns `model.IssueDetail` with:
- `Issue` — from `getIssue` (`:178`)
- `Relations` — `incidentRelations(id)`, insertion order (`:183`)
- `Comments` — `commentsFor(id)`, insertion order (`:223`)
- `Events` — `eventsFor(id)`, sorted (created_at, id) (`:224`)
- `Children`, `DependsOn`, `Blocks`, `Parent` — from `bucketRelations` (`:184-187`, `:225-228`)
- `Siblings` — the parent's other children (same rank-ordered child set, minus self); an only child yields the empty group (`:192-206`)
- `Related` — `relatedIssues` (`:188-191`, `:229`)
- `RedirectTarget` — hydrated from the issue's own close payload (`issue.RedirectTargetValue()`), **never** from the relations graph; nil if the target record is absent (`:208-219`, `:230`)

**`ListChildren`** (`internal/storage/memory/issues.go:235-244`) — `mustRecord(parentID)` (so a missing parent is `NotFoundError`), then hydrated `childRecords` in rank order.

**`ListTopics`** (`internal/storage/memory/issues.go:246-267`) — iterates `issues`; skips records whose retention is `model.Deleted` and records with an empty topic; dedupes; `slices.Sort`. **Deletion removes an issue's topic from the vocabulary; archival does not** (`:254-256`).

**`ListAllEvents`** (`internal/storage/memory/issues.go:279-283`) — `sortEvents(cloneEvents(e.events))`. The append-only slice already holds true recording order, which is a better answer, and it is deliberately not the one given, because a same-tick tie is where two engines would part company (`:269-278`).

**`sortEvents`** (`internal/storage/memory/issues.go:288-293`) — `slices.SortStableFunc` on `cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))`. It is the one place this engine orders history (`:285-287`).

**`LocalIssueCount`** (`internal/storage/memory/issues.go:297-301`) — `int64(len(e.issues))`; counts what would be lost, including archived and deleted (`:295-296`).

**`eventsFor(issueID)`** (`internal/storage/memory/issues.go:303-311`) — filters `events` by `IssueID`, then `sortEvents(cloneEvents(...))`.

### 2.7 `ListIssues` (`internal/storage/memory/list.go`)

Pipeline is fixed and every stage always runs: **hydrate → select → order → cap** (`internal/storage/memory/list.go:14-19`).

1. `issueOrdering(filter.SortBy)` — parsed first, so an unknown sort field errors before any work (`internal/storage/memory/list.go:27-30`).
2. `canonicalLabels(filter.LabelsAll)` — the label criteria are normalized the same way stored labels are (`:31-34`).
3. Hydrates **all** issues in `e.order` sequence (`:35-43`).
4. `e.selects(issue, filter, labelCriteria)` per issue (`:44-49`).
5. `slices.SortStableFunc(selected, order)` — the ordering is total (every comparison ends in a distinct id), so the result does not depend on arrival order (`:50-53`).
6. `capLimit(selected, filter.Limit)` (`:54`).

**`selects`** — every criterion ANDs; every slice ORs within itself (`internal/storage/memory/list.go:57-109`):
| Criterion | Semantics | Cite |
|---|---|---|
| Retention | `model.Archived` excluded unless `IncludeArchived`; `model.Deleted` excluded unless `IncludeDeleted`; anything else (Live) always passes | `:61-70` |
| `Statuses` | `matchesStates`: empty = pass; otherwise matches if any `model.DefaultOpen(string(state)) == issue.State()` — compares the **DERIVED** state | `:71-73`, `:118-131` |
| `Resolutions` | `matchesResolutions`: empty = pass; a nil `ResolutionValue()` matches **no** non-empty criteria set; otherwise `slices.Contains(wanted, *resolution)` | `:74-76`, `:133-145` |
| `IssueTypes` | `matchesAny(string(issue.IssueType), ...)`: empty = pass; else exact string membership | `:77-79`, `:111-116` |
| `ExcludeIssueTypes` | if non-empty AND the type is in the list → reject | `:80-82` |
| `Assignees` | `matchesAny(issue.Assignee, trimmedNonEmpty(...))`: exact match after trimming criteria; blanks in the criteria slice are dropped so a whitespace-only filter constrains nothing | `:83-85`, `:171-182` |
| `IDs` | `matchesAny(issue.ID, trimmedNonEmpty(...))`: exact match | `:86-88` |
| `UpdatedAfter` | reject if `issue.UpdatedAt.Before(*UpdatedAfter)` (i.e. inclusive of equality) | `:89-91` |
| `UpdatedBefore` | reject if `issue.UpdatedAt.After(*UpdatedBefore)` (inclusive of equality) | `:92-94` |
| `HasComments` | reject if `*HasComments != (len(commentsFor(issue.ID)) > 0)` | `:95-97` |
| `LabelsAll` | **conjunctive**: every canonical label criterion must be in `issue.Labels` | `:98-102` |
| `SearchTerms` | **conjunctive across terms**: every term must match | `:103-107` |

**`matchesSearch`** (`internal/storage/memory/list.go:150-161`) — lowercases and trims the term; an empty needle matches everything; case-insensitive substring across exactly four fields: `Title`, `Description`, `Prompt`, `Topic`.

**`capLimit`** (`internal/storage/memory/list.go:187-192`) — `limit <= 0` or `len <= limit` → unchanged; else `issues[:limit]`. **A limit of zero is the absence of a limit, not a limit of zero**; truncation, never sampling (`:184-186`).

**`issueSortKeys`** (`internal/storage/memory/list.go:198-209`) — exactly ten entries, matching `storage.SortFields`:
| Key | Comparison |
|---|---|
| `id` | `strings.Compare(a.ID, b.ID)` |
| `title` | `strings.Compare(a.Title, b.Title)` |
| `status` | `compareStoredStatus` |
| `priority` | `cmp.Compare(a.Priority, b.Priority)` |
| `rank` | `strings.Compare(a.Rank, b.Rank)` |
| `type` | `strings.Compare(string(a.IssueType), string(b.IssueType))` |
| `topic` | `strings.Compare(a.Topic, b.Topic)` |
| `assignee` | `strings.Compare(a.Assignee, b.Assignee)` |
| `created_at` | `a.CreatedAt.Compare(b.CreatedAt)` |
| `updated_at` | `a.UpdatedAt.Compare(b.UpdatedAt)` |

**`compareStoredStatus`** (`internal/storage/memory/list.go:219-226`) — `cmp.Or(cmp.Compare(aStored, bStored), strings.Compare(aValue, bValue))`, where `storedStatus(issue)` returns `(0, "")` when `issue.Capabilities().Status == nil` (a container) and `(1, string(status.Value))` otherwise (`:228-236`). SQL orders NULL ahead of every value ascending, so "has no stored status" is the low key. It deliberately does NOT compare `model.Issue.State()` (`:211-218`).

**`issueOrdering`** (`internal/storage/memory/list.go:248-274`)
- No specs → `[]SortSpec{{Field: "rank"}}` — the canonical ordering expressed as the spec list it stands for (`:249-251`).
- Each spec's field is `strings.ToLower(strings.TrimSpace(...))` then looked up in `issueSortKeys`; a miss → `fmt.Errorf("unsupported sort field %q", spec.Field)` (`:253-258`).
- `Desc` negates the ascending comparator (`:259-262`).
- **`strings.Compare(a.ID, b.ID)` ascending is appended as the final key always** — so descending reverses only the named keys, never the tie-break (`:265`).
- The composed comparator returns the first non-zero result, else 0 (`:266-273`).

### 2.8 `Apply` (`internal/storage/memory/apply.go`)

**`apply(id, c)`** (`internal/storage/memory/apply.go:25-60`)
1. `mustRecord(id)` → `NotFoundError` on miss (`:26-29`).
2. Hydrates `current` (`:30-34`).
3. `actor = strings.TrimSpace(c.Actor)`; empty → `"unknown"` (`:35-40`).
4. `now := e.now()` (`:41`).
5. `planLifecycle(current, actor, strings.TrimSpace(c.Reason), c.Action, now)` → `(afterAction, actionEvents, err)` (`:46`).
6. `planFields(afterAction, c.Fields, actor, now)` — **the field write baselines on the POST-action issue**, so a start's new assignee is what the patch diffs against rather than producing a second assignee change row (`:43-50`).
7. `writeIssue`, `writeLabels`, `recordEvents(actionEvents)`, `recordEvents(patch.events)` — in that order (`:55-58`).
8. Returns `hydrate(rec, e.positions())` (`:59`).

**`planLifecycle`** (`internal/storage/memory/apply.go:67-80`) — a type switch on the sealed `model.Action` sum: `nil` → no transition (`current, nil, nil`); `model.StatusAction` → `planStatus`; `model.RetentionAction` → `planRetention`; `default` → **panics** with `fmt.Sprintf("illegal Action value %T", action)` (only an impostor `Action` reaches here) (`:75-79`).

**`planStatus`** (`internal/storage/memory/apply.go:86-127`)
- If `model.Frozen(current.Retention())` → `fmt.Errorf("cannot %s archived or deleted issue", action.Name())` (`:87-89`).
- `current.Apply(action)` — the state machine's own rejections propagate (`:90-93`).
- Assignee rule: `postAssignee = priorAssignee` except for `model.Start`, where it is `strings.TrimSpace(start.Assignee)`. Start is the one variant carrying a new owner (`:94-104`).
- **No-op rule**: if `updated.StatusValue() == current.StatusValue() && postAssignee == priorAssignee` → return `(current, nil, nil)` with no event. A same-state start with a NEW assignee is the agent reclaim path and falls through, recording the ownership change (`:105-116`).
- The no-op is decided BEFORE the redirect target is checked, because the check guards a write and there is no write here to guard (`:110-113`).
- `validateRedirectTarget(updated)` (`:117-119`).
- Sets `updated.UpdatedAt = now`; emits one event with `action: string(action.Name())`, the change's `reason`, the `actor`, and `statusChanges(current, updated)` (`:120-126`).

**`planRetention`** (`internal/storage/memory/apply.go:134-148`)
- `model.Retain(current.Retention(), action, now)` — the legal moves and every rejection reason are the model's transition table (`:135-138`).
- **There is no same-state success cell** — re-archiving an archived issue is a rejection, not a quiet no-op — so every planned retention move owes a write (`:130-133`).
- Sets retention and `UpdatedAt = now`; emits one event with `retentionChanges(...)` (`:139-147`).

**`validateRedirectTarget`** (`internal/storage/memory/apply.go:156-176`)
- Target nil and resolution non-nil and `resolution.RedirectsToCanonical()` → `fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)` (`:157-162`).
- Target nil and not redirect-requiring → nil (`:162-163`).
- `*target == closing.ID` → `fmt.Errorf("cannot redirect %s to itself", closing.ID)` (`:165-167`).
- Target not present → `mustRecord`'s `NotFoundError` (`:168-171`).
- Target's retention is `model.Deleted` → `fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted", ...)` (`:172-174`).
- **Archived stays legal**, because "duplicate of something already done" is the most common real redirect (`:152-154`).

**`planFields`** (`internal/storage/memory/apply.go:199-248`) — a pure function of (baseline, patch, actor, now); no clock beyond the stamp handed in, no store, no writes (`:194-198`).
- `Title` set → `strings.TrimSpace`; if the result is empty → `errors.New("title cannot be empty")` (`:201-206`).
- `Description`, `Prompt`, `Assignee`, `Lane` set → `strings.TrimSpace` (`:207-232`).
- `IssueType` set → **refused if it would cross the container/leaf line**: `fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())` (`:213-223`).
- `Priority` set → assigned as-is (`:224-226`).
- `Labels` set → `canonicalLabels(*in.Labels)` (`:233-239`).
- `patch.statesLabels = (in.Labels != nil)` — **not** the same question as "did the labels change": a patch restating the existing set rewrites the label rows (authorship and timestamps included), while a patch never mentioning labels leaves them as an earlier writer left them (`:181-184`, `:240`).
- If `fieldChanges(baseline, issue)` is empty → returns the patch with **no event and no `UpdatedAt` bump** (`:241-244`).
- Otherwise sets `patch.issue.UpdatedAt = now` and emits one event with `reason: in.Reason`, `actor`, and the changes. **The field-change event has an empty `action` string** (`:245-247`).

**`fieldChanges`** (`internal/storage/memory/apply.go:253-270`) — one row per field that actually moved, in this fixed order: `title`, `description`, `issue_type`, `priority`, `assignee`, `lane`, `labels`.
- `priority` is recorded as `strconv.Itoa(int(...))` — the numeric wire encoding, not the display name (`:263-265`).
- `labels` is recorded as `strings.Join(labels, ",")` (`:268`).
- Note: `prompt` and `topic` are NOT in `fieldChanges`, so a prompt-only edit produces a patch with no event.

**`statusChanges`** (`internal/storage/memory/apply.go:275-299`) — rows, in order, for: `status` (when `StatusValue()` moved), `closed_at` (RFC3339Nano, `""` when absent), `resolution` (`""` when absent), `redirect_target` (`""` when absent), `assignee`.

**`retentionChanges`** (`internal/storage/memory/apply.go:304-315`) — projects both retentions through `model.RetentionTimestamps` and emits `archived_at` and/or `deleted_at` rows (RFC3339Nano) for whichever moved — the same encoding an export carries (`:301-303`).

**`writeIssue`** (`internal/storage/memory/apply.go:320-331`) — the one place a mutation becomes stored state; copies `title`, `description`, `prompt`, `issueType`, `priority`, `assignee`, `lane`, `updatedAt`, `retention`, `status = statusViewOf(issue)`. **`topic`, `id`, and `createdAt` are never written here** — topic is immutable through `Apply`.

**`writeLabels`** (`internal/storage/memory/apply.go:336-341`) — replaces the label set when and only when `patch.statesLabels`.

**`statusViewOf`** (`internal/storage/memory/apply.go:346-356`) — a container projects to the **zero** `model.StatusView` (the same nothing Dolt stores as a NULL column); a leaf projects `{Value: issue.State(), ClosedAt, Resolution, RedirectTarget}`.

**Optional-value helpers** (`internal/storage/memory/compare.go`) — shared contract: two absent values are equal, absence renders as the empty string, and absence is never conflated with a present zero (`:9-13`).
- `timesEqual` (`:15-20`), `resolutionsEqual` (`:22-27`), `stringsEqual` (`:29-34`).
- `formatTime` → `""` for nil, else `time.RFC3339Nano` (`:36-41`); `formatResolution` → `""` for nil (`:43-48`); `formatString` → `""` for nil (`:50-55`).

### 2.9 Comments (`internal/storage/memory/edges.go:17-73`)

**`AddComment`** (`:23-44`)
- `getIssue(in.IssueID)` first → missing issue is `NotFoundError` (`:27-30`).
- `body = strings.TrimSpace(in.Body)`; empty → `errors.New("comment body is required")` (`:31-34`).
- Comment: `ID: "cmt-" + uuid.NewString()`, `IssueID`, trimmed `Body`, `CreatedAt: e.now()`, `CreatedBy: authorOr(in.CreatedBy)` (`:35-41`).
- Appends and returns `(comment, issue, nil)` — the issue read is the caller's answer too, since a comment never changes the issue row (`:19-22`, `:42-43`).
- **`AddComment` records no history event.**

**`DeleteComment`** (`:48-63`)
- `id = strings.TrimSpace(commentID)`; empty → `errors.New("comment id is required")` (`:52-55`).
- Not found → `storage.NotFoundError{Entity: "comment", ID: id}` (`:56-59`).
- Deletes and returns the removed comment (`:60-62`).

**`commentsFor(issueID)`** (`:65-73`) — filters `comments` by `IssueID`, insertion order, returns a non-nil empty slice when there are none.

### 2.10 Labels (`internal/storage/memory/edges.go:75-179`)

**`AddLabel`** (`:81-99`)
- `mustRecord(in.IssueID)` → `NotFoundError` (`:85-87`).
- `model.NormalizeLabel(in.Name)` → error propagates (`:88-91`).
- If the name is not already present, appends a `model.Label{IssueID, Name, CreatedAt: e.now(), CreatedBy: authorOr(in.CreatedBy)}` and re-sorts by name (`:92-97`).
- **Adding the same label twice is not an error**: the caller asked for the label to be there, and it is; the row keeps the authorship the first add gave it (`:77-80`).
- Returns the whole resulting set (`:98`).

**`RemoveLabel`** (`:104-122`)
- `mustRecord(issueID)` → `NotFoundError` (`:108-110`).
- `model.NormalizeLabel(labelName)` → error propagates (`:111-114`).
- Absent → `storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, name)}` (`:115-119`).
- Otherwise deletes and returns the resulting set (`:120-121`).

**`ReplaceLabels`** (`:126-139`)
- `mustRecord(issueID)`; `canonicalLabels(labels)`; `setLabels(issueID, canonical, e.now(), createdBy)`; returns nil (`:130-138`).
- **Rewrites every row's `CreatedAt` and `CreatedBy`.**

**`ListLabels`** (`:141-145`) — returns `labelNames(issueID)` **without** a `mustRecord` check, so an unknown issue id returns an empty slice and no error.

**`setLabels`** (`:150-157`) — the one label write, shared by create and replace; builds fresh rows with one author (`authorOr(createdBy)`) and one timestamp.

**`canonicalLabels`** (`:163-179`) — normalizes each name via `model.NormalizeLabel` (error propagates), collapses duplicates, then `slices.Sort`. It is a parser: nothing downstream re-normalizes (`:159-162`).

**`authorOr(createdBy)`** (`:477-482`) — trimmed non-empty value, else `"unknown"`.

### 2.11 Relations (`internal/storage/memory/edges.go:181-471`)

**`addRelation`** (`:189-224`)
1. `RelRelatedTo` with `SrcID == DstID` → `errors.New("related-to cannot target itself")` (`:190-192`).
2. `in.Type.CanonicalEndpoints(in.SrcID, in.DstID)` normalizes endpoint order (`:193`).
3. `mustRecord(srcID)` then `mustRecord(dstID)` → `NotFoundError` (`:194-199`).
4. For `RelBlocks`, `rejectBlocksCycle(srcID, dstID)` (`:205-209`).
5. Builds `model.Relation{SrcID, DstID, Type, CreatedAt: e.now(), CreatedBy: authorOr(in.CreatedBy)}` (`:210`).
6. If `in.Type.SingleValuedFromSrc()`, drops every existing edge with the same `SrcID` and `Type` — cardinality is read off the type, not off which method the caller reached for (`:211-218`).
7. If an identical `(src, dst, type)` edge still exists → `fmt.Errorf("relation %s->%s (%s) already exists", ...)` (`:219-221`).
8. Appends and returns (`:222-223`).

**`rejectBlocksCycle(dependent, dependency)`** (`:230-263`)
- Self-edge → `fmt.Errorf("blocks: %s cannot block itself", dependent)` (`:231-233`).
- Builds `precedes` = dependency → dependents from all `RelBlocks` edges (`:234-241`).
- DFS from `dependent`; if `dependency` is reachable → long error: `"blocks: cannot add %s depends-on %s — %s already depends on %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order"` (`:242-262`).
- Rationale: a rank order is a total order and one honoring every blocks edge exists exactly when there is no cycle (`:200-204`).

**`RemoveRelation`** (`:265-277`)
- Canonicalizes endpoints, drops matching edges; **removed == 0** → `storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}` (`:269-276`).

**`ListRelationsForIssue`** (`:283-297`)
- `mustRecord(issueID)` → `NotFoundError` (`:287-289`).
- Returns `incidentRelations(issueID)` (either direction, write order) filtered by the variadic types; **naming no type means every type, never no types** (`:279-282`, `:292`).

**`GetRelationsByIDs`** (`:302-328`)
- Deduplicates the input ids (`:309-311`).
- An id with no record is **simply absent from the map**, not an error (`:312-315`).
- For each present id: hydrates the issue, buckets its incident relations, sets `bucketed.Issue = issue`.
- `nil` input yields an empty (non-nil) map (`:307`).

**`SetParent`** (`:330-346`)
- Blank child or parent (after trim) → `errors.New("child and parent ids are required")` (`:334-336`).
- `ChildID == ParentID` → `errors.New("child and parent cannot be the same issue")` (`:337-339`).
- Delegates to `addRelation` with `Type: model.RelParentChild` — one validated caller of the single-valued write, so reparenting replaces in one act (`:340-345`).

**`ClearParent`** (`:352-366`)
- `mustRecord(childID)` → `NotFoundError` (`:356-358`).
- Drops every `RelParentChild` edge with `SrcID == childID`; **removed == 0** → `storage.NotFoundError{Entity: "parent relation", ID: childID}` (`:359-365`).

**`incidentRelations`** (`:370-378`) — every edge touching the id in either direction, in write order.

**`bucketRelations(focalID, relations, pos)`** (`:401-440`) — the single definition of edge → bucket mapping (`:395-400`):
| Condition | Bucket | Counterpart |
|---|---|---|
| `Type == RelBlocks && SrcID == focalID` | `DependsOn` | `DstID` |
| `Type == RelBlocks && DstID == focalID` | `Blocks` | `SrcID` |
| `Type == RelParentChild && DstID == focalID` | `Children` | `SrcID` |
| `Type == RelParentChild && SrcID == focalID` | `Parent` (pointer) | `DstID` |
| anything else (e.g. `RelRelatedTo`) | skipped | — |
- An edge whose counterpart record has vanished is simply not in the result (`:405-408`, `:422-425`).
- `Children`, `DependsOn`, `Blocks` are each sorted by rank position; the returned struct initializes them to non-nil empty slices (`:402`, `:436-438`).

**`relatedIssues`** (`:445-467`) — `RelRelatedTo` counterparts only (the other end of the edge), hydrated, rank-sorted. It is `GetIssueDetail`'s concern alone; peer links stay out of the shared `IssueRelations` shape (`:442-444`).

**`sortByRank`** (`:469-471`) — `slices.SortStableFunc` on `pos[a.ID] - pos[b.ID]`.

### 2.12 Rank (`internal/storage/memory/rank.go`)

Because the order is a slice, an intent is literally what it says: "above Y" removes the issue and puts it back immediately before Y. **No fractional key, no midpoint, no inversion to repair** (`internal/storage/memory/rank.go:13-21`).

**`side`** (`:37-42`) — `above side = 0`, `below side = 1`; a value the one relative-rank path takes rather than two paths (`:34-36`).

**`rankRelative(issueID, targetID, at)`** (`:44-53`)
- `resolveRankPair`; `detach(move.MovedID)`; `anchor := slices.Index(e.order, move.AnchorID)`; `insertAt(anchor + int(at), move.MovedID)`; returns the move.

**`RankToTop` / `RankToBottom`** (`:57-76`) — `rankToEnd(issueID, storage.RankTop|RankBottom)`: `mustRecord` (so a missing id is `NotFoundError`), `detach`, `place`. **They need no anchor and no frame: every issue is comparable with the ends** (`:55-56`).

**`RankSet(ids)`** (`:81-128`)
- `len(ids) < 2` → `errors.New("rank set: need at least 2 IDs to establish order")` (`:85-87`).
- Empty id → `errors.New("rank set: empty ID in input")` (`:89-91`).
- Duplicate id → `fmt.Errorf("rank set: duplicate ID %q in input", id)` (`:92-95`).
- Builds an ancestor chain per id (missing id → `NotFoundError` from `ancestorChain`) (`:98-105`).
- `frameRepresentatives(chains)`; error wrapped as `fmt.Errorf("rank set: %w", err)` (`:106-109`).
- **Two named ids collapsing onto one representative is refused**: `"rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead"` (`:110-119`).
- Detaches every representative and **prepends the representatives to the head of the order** — the named issues are stacked at the TOP, in the order named (`:79-80`, `:123-126`).
- Returns one `RankSetResolution{NamedID, RankedID}` per input, in input order (`:115-121`, `:127`).

**`detach`** (`:134-136`) — `slices.DeleteFunc` on id equality.
**`insertAt`** (`:143-145`) — `slices.Insert`; **clamps nothing**, deliberately: a clamp would turn a resolution bug into a silent placement at the top of the backlog (`:138-142`).

**`resolveRankPair(issueID, targetID)`** (`:153-183`) — both relative verbs route through this one resolution, so cross-frame semantics cannot drift between above and below (`:149-152`).
- `issueID == targetID` → `errors.New("cannot rank an issue relative to itself")` (`:154-156`).
- `mustRecord(targetID)` **before** `mustRecord(issueID)` (`:157-162`).
- Builds both ancestor chains, calls `frameRepresentatives`.
- A `*frameContainmentError` is re-worded by which side contains which: if `containment.containerID == issueID` → `"cannot rank %s relative to %s: %s contains it; rank it against a sibling instead"`; else → `"cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead"` (`:172-178`).
- Returns `RankMove{MovedID: reps[0], AnchorID: reps[1]}` (`:182`).

**`ancestorChain(id)`** (`:188-206`) — self first, root last, following only parents that are still there. Missing id → `NotFoundError`. A parent cycle → `fmt.Errorf("ancestor chain of %s: parent cycle at %s", id, parent)` (`:199-201`).

**`parentOf(childID)`** (`:211-226`) — the first `RelParentChild` edge whose `SrcID == childID` and whose parent record exists and is **not** `model.Deleted`. A deleted parent is skipped: work in the trash frames nothing (`:208-210`).

**`frameContainmentError{containerID, containedID}`** (`:231-238`) — `Error()` renders `"%s is inside %s; no comparable frame contains both — rank it against a sibling instead"`.

**`frameRepresentatives(chains)`** (`:250-295`)
- Builds an id→depth map per chain (`:251-258`).
- If any chain's head appears at depth > 0 in another chain, returns `&frameContainmentError{containerID: chain[0], containedID: chains[j][0]}` (`:259-268`).
- Finds the lowest common ancestor as the first element of chain 0 present in all others (common ancestors form a shared suffix of every chain) (`:269-285`).
- With no common ancestor (`lowestCommon == ""`), each chain's representative is its **root** — the top level is the frame that contains everything (`:288-291`, `:243-247`).
- Otherwise the representative is the element **one level below** the LCA in that chain: `chain[depths[i][lowestCommon]-1]` (`:292`).
- Nothing inside any epic is reordered by a cross-frame request (`:246-247`).

### 2.13 Bulk / import (`internal/storage/memory/bulk.go`)

**`BulkApply(ctx, prefix, actor, specs)`** (`:22-78`)
1. `validateBulkSpecs(specs)` — the entire file is gated before any document is applied (`:26-28`).
2. `creationOrder(bulkGraph(specs))`; a cycle → `fmt.Errorf("bulk: %w", err)` (`:29-32`).
3. Walks documents in topological order:
   - `spec.ID != ""` (update): `bulkUpdateChange(spec, actor)` then `e.apply(spec.ID, change)`. On apply failure → `e.compensate(batch, fmt.Errorf("bulk: update %q: %w", spec.ID, err))`. Appends `issue.ID` to `result.Updated` (`:37-48`).
   - Otherwise (create): `bulkCreateInput(spec, prefix, batch)` then `e.createIssue(in)`. On failure → `e.compensate(batch, fmt.Errorf("bulk: create doc %d: %w", index, err))`. Records into the batch, and into `result.Created` under `spec.LocalID` if set, else under the new real id (`:50-65`).
   - Note: `bulkUpdateChange` and `bulkCreateInput` errors are returned **without** compensation (`:39-42`, `:50-53`).
4. **Second pass, in original spec order (not topological)**: wires every `DependsOn` edge as `blocks` with `src = the document's own issue`, `dst = batch.resolve(dep)`. On failure → `e.compensate(batch, fmt.Errorf("bulk: depends_on doc %d -> %q: %w", index, dep, err))` (`:67-76`).

**`ImportTree(ctx, prefix, specs)`** (`:83-134`)
1. `validateImportSpecs(specs)` (`:87-89`).
2. `creationOrder(importGraph(specs))`; cycle → `fmt.Errorf("import: %w", err)` (`:90-93`).
3. Per spec in topological order: re-parses `IssueType` and `Priority` (errors → `fmt.Errorf("import: spec %q: %w", spec.LocalID, err)`, **without** compensation), then `createIssue` with `ParentID: batch.resolveLocal(spec.Parent)` and `Prefix: prefix`. Create failure → `e.compensate(batch, fmt.Errorf("import: create %q: %w", spec.LocalID, err))` (`:96-125`).
   - Note: `Lane` and `Placement` are not carried from `ImportTreeSpec` (it has no such fields).
4. Second pass in spec order wires `DependsOn` (`:126-132`).
5. Returns `ImportTreeResult{IDMap: batch.idMap()}` — the `byLocalID` map (`:133`, `:201`).

**`wireDependency(dependent, dependency)`** (`:136-141`) — `addRelation` with `Type: model.RelBlocks`, `CreatedBy: createdBy` (`"links"`).

**`compensate(batch, cause)`** (`:146-154`)
- For each created id, applies `storage.Change{Action: model.Delete{}, Actor: createdBy, Reason: "import rollback"}`; ids whose delete failed are collected as "leaked".
- Returns `fmt.Errorf("%w (rollback leaked %d: %s)", cause, len(leaked), strings.Join(leaked, ","))` — the original failure travels unchanged; the rollback only adds an account of what is left behind (`:143-145`).
- **Compensation is a soft delete, not a removal**: the issues remain in the store with `Deleted` retention (hence invisible to a default listing and to `ListTopics`, but counted by `LocalIssueCount`).
- **Updates already applied are not reverted.**

**`batchIDs`** (`:160-201`) — `byIndex []string`, `byLocalID map[string]string`, `createdIDs []string` (oldest first, the compensation order's input).
- `resolve(ref)` — returns the local mapping if present, otherwise **passes the reference through unchanged**; a reference matching nothing local is a real, pre-existing id, and the write that receives it decides whether it is real (`:187-192`).
- `resolveLocal(ref)` — plain map lookup; an unresolved reference yields `""`, which the create path reads as "no parent". Validation, not this lookup, is what makes it total (`:194-199`).

**`bulkCreateInput`** (`:205-233`)
- `model.ParseIssueType(*spec.IssueType)` → error wrapped `fmt.Errorf("bulk: %w", err)` (`:206-209`).
- Priority defaults to `model.PriorityNormal` when the doc sets none; else `model.ParsePriority(*spec.Priority)` (`:210-216`).
- Title and Topic are `strings.TrimSpace`'d; Description/Prompt/Assignee/Lane/Labels come from `valueOr(ptr, zero)` (`:217-227`).
- **`Placement` is left at its zero value**, so a batch keeps its file order in the ranked order, whichever authored format the file is (`:228-231`).

**`bulkUpdateChange`** (`:238-261`)
- Builds `UpdateIssueInput` with `Reason: strings.TrimSpace(spec.Reason)`.
- `Title`, `Description`, `Prompt`, `Assignee`, `Lane` go through `trimmedPointer` (nil stays nil; otherwise a pointer to the trimmed value) (`:240-244`, `:263-269`).
- `Labels` is passed through as the raw `*[]string` (`:245`).
- `IssueType` and `Priority` are parsed into pointers; parse errors → `fmt.Errorf("bulk: update %q: %w", spec.ID, err)` (`:246-259`).
- Returns `storage.Change{Actor: actor, Fields: fields}` — **an update document never carries an Action** (`:260`).

**`creationOrder(graph)`** (`:310-353`) — DFS topological sort over intra-batch references only. States `unvisited`/`visiting`/`done`; re-entering `visiting` → `fmt.Errorf("cycle detected involving %q", graph.localID[i])` (`:328-332`). A reference matching no local name is not an edge (`:334-338`, `:306-309`). Both authored formats reduce to the same `localGraph` shape (`:283-304`).

**`validateBulkSpecs`** (`:362-406`)
- Empty input → `errors.New("bulk: no issues in input")` (`:363-365`).
- Per doc: `id`, `local_id`, `parent` must have no surrounding whitespace → `fmt.Errorf("bulk: doc %d %s %q has surrounding whitespace", ...)` (`:369-375`).
- Each `depends_on` entry: no surrounding whitespace (`:376-379`); and if `LocalID != "" && dep == spec.LocalID` → `fmt.Errorf("bulk: doc %d (local_id %q) cannot depend on itself", ...)` (`:380-382`).
- `ID != ""` → `validateBulkUpdate`; duplicate `ID` → `fmt.Errorf("bulk: duplicate id %q", spec.ID)` (`:384-392`).
- Else → `validateBulkCreate`; duplicate non-empty `LocalID` → `fmt.Errorf("bulk: duplicate local_id %q", spec.LocalID)` (`:394-403`).

**`validateBulkCreate`** (`:408-430`)
- Missing/blank `Title` → `"bulk: doc %d missing title"` (`:409-411`).
- Missing/blank `Topic` → `"bulk: doc %d missing topic"` (`:412-414`).
- Missing `IssueType` → `"bulk: doc %d missing type"` (`:415-417`).
- Unparseable type → `"bulk: doc %d has invalid type %q"` (`:418-420`).
- Unparseable priority (when set) → `"bulk: doc %d has invalid priority %d"` (`:421-425`).
- `Reason != ""` on a create → `"bulk: doc %d sets reason without id (reason only applies to updates)"` (`:426-428`).

**`validateBulkUpdate`** (`:436-463`) — refuses the fields an update document has no business setting; each is somebody else's verb (`:432-435`):
- `LocalID != ""` → `"bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets"` (`:437-439`).
- `Topic != nil` → `"... sets topic; topic is immutable and update cannot change it"` (`:440-442`).
- `Parent != ""` → `"... sets parent; reparent with \`lit parent set\` instead"` (`:443-445`).
- `len(DependsOn) > 0` → `"... sets depends_on; wire dependencies with \`lit dep add\` instead"` (`:446-448`).
- Invalid type / priority → `"... has invalid type %q"` / `"... has invalid priority %d"` (`:449-458`).
- No updatable field stated → `"bulk: doc %d (id %q) has no fields to update"` (`:459-461`). Updatable fields are exactly: `Title`, `Description`, `Prompt`, `IssueType`, `Priority`, `Assignee`, `Labels`, `Lane` (`:465-469`).

**`validateImportSpecs`** (`:475-523`)
- Empty → `errors.New("import: no issues in input")` (`:476-478`).
- Blank `LocalID` → `"import: spec %d missing local_id"` (`:481-483`); untrimmed → `"import: spec %d local_id %q has surrounding whitespace"` (`:484-486`).
- Blank `Title` → `"import: spec %q missing title"` (`:487-489`).
- Invalid type → `"import: spec %q has invalid type %q"` (`:490-492`); invalid priority → `"import: spec %q has invalid priority %d"` (`:493-495`).
- Duplicate `LocalID` → `"import: duplicate local_id %q"` (`:496-499`).
- Second loop: `Parent` untrimmed → error; **`Parent` must name a spec in the file** → `"import: spec %q references missing parent %q"` (`:502-509`).
- Each `DependsOn` entry: untrimmed → error; self-reference → `"import: spec %q cannot depend on itself"`; **must name a spec in the file** → `"import: spec %q references missing depends_on %q"` (`:510-520`).
- So: **`ImportTree` references must all resolve inside the file; `BulkApply` references may resolve to pre-existing real ids.**

### 2.14 `Export` (`internal/storage/memory/export.go:26-57`)

- Locks, then builds issues via `listIssues(ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})` — **the WHOLE store**, out-of-flow work included, because an export honoring the listing default would silently drop exactly the rows a diff exists to notice (`:19-23`, `:30-33`).
- Labels: flattened from the `labels` map, then sorted by `(IssueID, Name)` via `cmpThen` (`:34-40`, `:61-66`).
- Returns `model.Export{Version: exportVersion (2), WorkspaceID: e.workspaceID, ExportedAt: e.now(), Issues, Relations: slices.Clone(e.relations), Comments: slices.Clone(e.comments), Labels, Events: sortEvents(cloneEvents(e.events))}` (`:41-56`).
- `Relations` and `Comments` come back in **write order** (cloned, unsorted); `Issues` in rank order; `Labels` in `(issue, name)` order; `Events` in `(created_at, id)` order.
- Every collection comes back in a total order so two stores holding the same facts serialize to the same bytes rather than to the same multiset (`:23-25`).

### 2.15 Capabilities offered by the memory engine

- **None of the seven.** No remote to sync with, no divergence to reconcile, no history to check point, no faults of its own making to repair, no schema to migrate, no engine-native language for a raw statement. `storage.Offered` reports the empty set (`internal/storage/memory/doc.go:59-65`).
- Tested: `Offered(engine)` is empty; every `Capability.OfferedBy` is false; `storage.Sync.Of(engine)` returns an `UnsupportedError` with `Capability == "sync"` and a non-empty `Engine` (`internal/storage/memory/capabilities_test.go:20-49`).
- `var _ storage.Store = (*Engine)(nil)` — a contract method added or a signature moved stops the engine compiling (`internal/storage/memory/engine.go:17-21`).

### 2.16 Behaviors the memory engine deliberately copies from Dolt rather than improving

Stated in `internal/storage/memory/doc.go:26-49`:
1. Ordering a listing by `"status"` sorts the **stored** status encoding; a container stores none, so it orders ahead of every leaf ascending whatever state it derives to, while the status FILTER reads derived state. Correcting the disagreement is `links-store-seam-q35v.6` (`internal/storage/memory/doc.go:35-39`).
2. History comes back ordered by `(created_at, id)` rather than by recording order. Event ids are random, so on a coarse clock both engines can hand back a title change ahead of the creation that preceded it (`internal/storage/memory/doc.go:40-44`).
- Nothing is shared with the Dolt engine — not the field-patch diff, the transition planner, the frame resolution, or the compensating bulk apply — deliberately, because two engines calling one implementation would destroy the proof the conformance suite provides (`internal/storage/memory/doc.go:15-25`).

---

## PART 3 — `internal/storage/conformance` (what the suite requires of any backend)

### 3.1 Harness

- `NewEngine func(t *testing.T) storage.Store` — mints a fresh, empty engine for one case, already registered for cleanup. Every case gets its own store (`internal/storage/conformance/conformance.go:53-56`).
- `Run(t, newEngine)` walks the `cases` table, running each as a subtest with `context.Background()` and a fresh engine (`internal/storage/conformance/conformance.go:59-66`).
- `engineCase{name string; run func(t, ctx, st)}` — the suite is a table walked by one loop; adding a statement is adding data (`internal/storage/conformance/conformance.go:68-74`).
- `const prefix = "conf"` — every case creates under this cosmetic prefix; **cases assert on ids only by comparing ids the engine returned, never by predicting their shape** (`internal/storage/conformance/conformance.go:76-79`).
- `mustCreate` forcibly sets `in.Prefix = prefix` on every create (`internal/storage/conformance/conformance.go:1320-1328`).
- Rule for what may be asserted: only what a caller can observe through the contract; no case may reach past the interface into engine internals (`internal/storage/conformance/conformance.go:22-27`).
- Dolt's behavior is the tiebreak where a behavior was ambiguous; where the second engine answered better, it was moved to match rather than the contract moved to meet it (`internal/storage/conformance/conformance.go:29-37`).

### 3.2 The 36 registered cases (`internal/storage/conformance/conformance.go:81-118`)

`create_read_roundtrip`, `create_defaults`, `create_requires_title`, `create_normalizes_topic`, `create_under_missing_parent_is_not_found`, `get_missing_issue_is_not_found`, `apply_field_patch`, `apply_status_transition`, `apply_missing_issue_is_not_found`, `apply_to_container_is_refused`, `container_state_follows_live_children`, `history_records_mutations`, `list_defaults_to_rank_order`, `list_filters_select`, `list_hides_archived_and_deleted`, `list_sorts_and_limits`, `list_breaks_sort_ties_by_id`, `list_accepts_exactly_the_contract_sort_fields`, `list_sorts_status_by_stored_encoding`, `events_are_totally_ordered`, `rank_intents_reorder`, `rank_intents_resolve_across_frames`, `rank_set_imposes_order`, `close_redirects_to_a_canonical`, `comments_roundtrip`, `labels_roundtrip`, `relations_roundtrip`, `relations_batch_buckets_edges`, `parent_wiring`, `topics_derive_from_issues`, `export_carries_whole_store`, `bulk_apply_creates_and_updates`, `bulk_apply_compensates_a_failed_batch`, `import_tree_maps_local_ids`, `attribution_stamps_events`, `local_issue_count_tracks_creates`.

### 3.3 Every enforced invariant, by case

**`create_read_roundtrip`** (`:120-178`)
- Surrounding whitespace is stripped on the way in for `Title`, `Description`, `Prompt` (`:135-143`).
- A `GetIssue` read returns the *same* record, field for field: `ID`, `Title`, `Description`, `Prompt`, `Topic`, `Assignee`, `Lane`, `IssueType`, `State()` (`:151-168`).
- A create with `IssueType: model.TypeBug` reads back `bug`, and `State()` is `open` (`:162-163`).
- `Priority` survives as `PriorityUrgent` (`:169-171`).
- `Labels: []string{"perf"}` reads back as `["perf"]` (`:172-174`).
- `Rank` is non-empty — every issue must land somewhere in the order (`:175-177`).

**`create_defaults`** (`:180-200`)
- Unspecified `IssueType` → `model.TypeTask` (`:184-186`).
- New issue's `State()` is `open` (`:187-189`).
- Unspecified `Priority` → `model.PriorityNormal` (`:190-192`).
- Default placement **appends**: a second create files below the first (`:194-196`).
- `Placement: storage.RankTop` leads the whole order (`:198-199`).

**`create_requires_title`** (`:202-210`)
- Both `""` and `"   "` are rejected; the trim happens before the requirement (`:203-209`).

**`create_normalizes_topic`** (`:217-233`)
- `"  Renderer Cleanup  "` is stored as `"renderer-cleanup"` (`:218-221`).
- `ListTopics` then returns exactly `["renderer-cleanup"]` (`:222-226`).
- These topics are refused at create: `""`, `"   "`, `"ab"` (too short), `"-!-"` (`:228-232`).

**`create_under_missing_parent_is_not_found`** (`:235-238`)
- Creating with `ParentID: "no-such-issue"` returns `NotFoundError` with `Entity == "issue"`.

**`get_missing_issue_is_not_found`** (`:240-246`)
- Both `GetIssue` and `GetIssueDetail` on a missing id return `NotFoundError{Entity: "issue"}`.

**`apply_field_patch`** (`:248-276`)
- A `Change` with `Fields{Title, Priority, Reason}` returns the updated values (`:253-262`).
- A field the patch never mentions (`Assignee`) is untouched — the whole reason the patch is pointers (`:263-267`).
- The patch persists: a subsequent `GetIssue` shows the new title (`:269-275`).

**`apply_status_transition`** (`:278-309`)
- `model.Start{Assignee: "ada"}` → `State() == in_progress` and `Assignee == "ada"`; Start is the one action that rewrites ownership (`:281-292`).
- `model.Done{}` → `State() == closed` (`:294-300`).
- `model.Reopen{}` → `State() == open` (`:302-308`).

**`apply_missing_issue_is_not_found`** (`:311-314`)
- `Apply` on a missing id returns `NotFoundError{Entity: "issue"}`.

**`apply_to_container_is_refused`** (`:316-331`)
- Applying `model.Start` to an epic with children returns a `model.ContainerActionError` whose `.ID` is the epic's id (`:323-330`).

**`container_state_follows_live_children`** (`:338-361`)
- Epic with two children, one `Done` → epic derives `in_progress` (`:343-346`).
- Archiving the unfinished child takes it out of the epic's reading → epic derives `closed` (`:348-353`).
- Archiving the epic itself freezes its reading: every child counts again, so the epic reverts to `in_progress` — the state it had when it left (`:355-360`).

**`history_records_mutations`** (`:363-426`)
- A pure no-op `Apply` (no action, no fields) writes **no** events (`:369-378`).
- A status action whose target state AND resulting assignee already hold is the same no-op: a repeated `Start{Assignee:"ada"}` writes no event (`:380-393`).
- A same-state start naming a NEW owner (the reclaim path) **does** record, and the assignee becomes the new owner (`:394-403`).
- Every event's `IssueID` matches the issue mutated (`:407-410`).
- A field write records a change row with `Field == "title"` (`:411-419`).
- Events are oldest-first by `CreatedAt` (`:420-425`).

**`list_defaults_to_rank_order`** (`:428-436`)
- Three creates come back in creation order — an unsorted listing is rank ascending with ties broken by id, and `lit backlog` is this order.

**`list_filters_select`** (`:438-482`) — each filter is asserted to select exactly the listed ids:
| Filter | Expected |
|---|---|
| `Statuses: [in_progress]` | the started task (`:460`) |
| `IssueTypes: [bug]` | the bug (`:461`) |
| `ExcludeIssueTypes: [bug]` | the task (`:462`) |
| `Assignees: ["grace"]` | the task (`:463`) |
| `IDs: [bug.ID]` | the bug (`:464`) |
| `SearchTerms: ["widget"]` | the bug — matches title (`:465`) |
| `SearchTerms: ["parser"]` | the task — **search matches topic too** (`:466`) |
| `LabelsAll: ["perf","ui"]` | the bug — conjunctive (`:467`) |
| `LabelsAll: ["perf","absent"]` | nothing — a label the issue lacks excludes it (`:468`) |
| `HasComments: &true` | the commented bug (`:469`) |
| `UpdatedBefore: now+1h` | both (`:470`) |
| `UpdatedAfter: now+1h` | nothing (`:471`) |
| `UpdatedAfter: now-1h` | both (`:472`) |
| `IssueTypes:[bug] + Assignees:["grace"]` | nothing — **criteria AND across axes**, so no caller can widen a listing by adding a criterion (`:473-475`) |
| `Assignees: ["ada","grace"]` | both — **a slice ORs within itself** (`:476-477`) |

**`list_hides_archived_and_deleted`** (`:484-505`)
- Default listing shows only live issues (`:495-497`).
- `IncludeArchived: true` → live + archived (`:498-499`).
- `IncludeDeleted: true` → live + deleted (`:500-501`).
- Both → all three (`:502-504`).
- Expected order in each case is creation order (live, archived, deleted), i.e. rank order is preserved across the retention filter.

**`list_sorts_and_limits`** (`:507-528`)
- `SortBy: [{title}]` ascending, `{title, Desc:true}` descending (`:512-517`).
- `Limit: 2` returns the **head** of the ordered result, not a sample (`:519-521`).
- `Limit: 0` is the absence of a limit, not a limit of zero (`:522-523`).
- Sorting by an unknown field (`"nonsense"`) is an error (`:525-527`).

**`list_breaks_sort_ties_by_id`** (`:535-553`)
- Three issues sharing one title: the result is ordered by id ascending (`:538-546`).
- Descending on the named key leaves the id tie-break **ascending** (`:548-552`).

**`list_accepts_exactly_the_contract_sort_fields`** (`:561-591`)
- **Every** field in `storage.SortFields` must be accepted (`:565-569`).
- These six must be **rejected**: `"description"`, `"lane"`, `"labels"`, `"issue_type"`, `"item_rank"`, `"state"` — real model fields the contract omits, plus the storage column names an engine binding its own schema would reach for (`:583-590`).
- The case self-guards: if any of those six is ever added to `SortFields`, the test fatals telling the author to move it (`:584-586`).

**`list_sorts_status_by_stored_encoding`** (`:603-626`)
- An epic whose only child is in progress derives `in_progress` (`:610-613`).
- Ascending by `status`: **the epic leads** — its absent stored status is the low key, even though `"in_progress"` would not sort before `"in_progress"` (`:615-619`).
- Descending by `status`: the epic trails (`:621-625`).
- The case pins the wrong answer on purpose; deleting it is the first step of `links-store-seam-q35v.6`, not a cleanup (`:593-602`).

**`events_are_totally_ordered`** (`:636-658`)
- After a create plus three title changes, `ListAllEvents` returns at least 4 events (`:644-650`).
- The sequence never steps backwards under `cmp.Or(CreatedAt.Compare, strings.Compare(ID))` — the property that holds tie or no tie (`:651-657`).

**`rank_intents_reorder`** (`:660-700`)
- Three creates → order a, b, c (`:664`).
- `RankAbove(c, a)` → order c, a, b; the returned `RankMove` is the inputs unchanged for frame-mates (`:666-675`).
- `RankBelow(c, b)` → order a, b, c (`:677-680`).
- `RankToTop(b)` → order b, a, c (`:682-685`).
- `RankToBottom(b)` → order a, c, b (`:687-690`).
- `RankAbove` with a missing anchor is an error; `RankToTop` of a missing issue is an error (`:692-699`).

**`rank_intents_resolve_across_frames`** (`:707-735`)
- `RankAbove(child_of_epic, standalone)` succeeds and reports `MovedID == epic.ID`, `AnchorID == standalone.ID` (`:714-720`).
- Nothing inside the epic is reordered, and the epic precedes the standalone in the listing (`:721-724`).
- `RankAbove(child, its own epic)` is an error (`:729-731`).
- `RankBelow(epic, its own child)` is an error (`:732-734`).

**`rank_set_imposes_order`** (`:737-761`)
- `RankSet([c, a, b])` yields listing order c, a, b (`:742-746`).
- Exactly one resolution per named id, in the order named, with `NamedID == RankedID` for frame-mates (`:748-760`).

**`close_redirects_to_a_canonical`** (`:767-813`)
- `Close{Outcome: Duplicate{Of: canonical}}` → `State() == closed`, `ResolutionValue() == ResolutionDuplicate`, `RedirectTargetValue() == canonical.ID`, `ClosedAtValue() != nil` (`:771-790`).
- `Reopen{}` clears the **whole** close payload together: resolution, redirect target, and closed-at all become nil (`:792-802`).
- Closing as a duplicate of a missing issue → `NotFoundError{Entity: "issue"}` (`:804-807`).
- Closing an issue as a duplicate of **itself** is an error (`:808-812`).

**`comments_roundtrip`** (`:815-862`)
- `AddComment` returns the comment as written (`Body`, `CreatedBy`, `IssueID`) (`:818-824`).
- The second return is the issue as it stands after the write (`:825-829`).
- `GetIssueDetail.Comments` holds exactly the added comment (`:831-837`).
- `DeleteComment` returns the removed comment (id and body) (`:839-847`).
- After delete, `GetIssueDetail.Comments` is empty (`:849-855`).
- `DeleteComment("no-such-comment")` → `NotFoundError{Entity: "comment"}` (`:857-858`).
- `AddComment` on a missing issue → `NotFoundError{Entity: "issue"}` (`:860-861`).

**`labels_roundtrip`** (`:864-913`)
- `AddLabel` returns the resulting set (`:869-873`).
- The set is **ordered by name**, not by arrival: adding `zeta` then `alpha` yields `["alpha","zeta"]` (`:875-880`).
- Adding a label twice is the same end state, **not an error** (`:882-888`).
- `RemoveLabel` returns the resulting set (`:890-894`).
- Removing an absent label → `NotFoundError{Entity: "label"}` (`:896-899`).
- `ReplaceLabels` states the whole set: what was there and is not named is gone (`:901-909`).
- `AddLabel` on a missing issue → `NotFoundError{Entity: "issue"}` (`:911-912`).

**`relations_roundtrip`** (`:915-977`)
- `AddRelation` with `RelBlocks` returns the edge with `src == dependent`, `dst == dependency` — the direction convention is contract (`:920-931`).
- `ListRelationsForIssue(id)` with no type argument returns **every** edge type (2 here) (`:938-944`).
- `ListRelationsForIssue(id, RelBlocks)` narrows to the one blocks edge — naming no type means every type, never no types (`:946-954`).
- Edges are readable **from either end**: the dependency sees its dependent (`:956-963`).
- `RemoveRelation` succeeds once; a second call → `NotFoundError{Entity: "relation"}` (`:965-969`).
- `related-to` is symmetric, so an issue cannot be related to itself (`:971-976`).

**`relations_batch_buckets_edges`** (`:979-1018`)
- `GetRelationsByIDs([epic, child, dependency])` returns 3 entries (`:989-995`).
- The child's `Parent` is the epic (`:997-1000`).
- The child's `DependsOn` is `[dependency]` (`:1001`).
- The epic's `Children` is `[child]` (`:1003`).
- The dependency's `Blocks` is `[child]` — `DependsOn` and `Blocks` are the two readings of one edge set, with no second row existing (`:1005-1007`).
- `GetRelationsByIDs(nil)` returns an empty map, not an error (`:1009-1017`).

**`parent_wiring`** (`:1020-1052`)
- `SetParent` wires a child under an epic; `ListChildren` shows it (`:1025-1028`).
- **Reparenting replaces rather than adds**: after a second `SetParent`, the old epic has no children and the new one has the child (`:1030-1036`).
- `ClearParent` detaches; the parent then has no children (`:1038-1041`).
- `ClearParent` on a parentless child → `NotFoundError{Entity: "parent relation"}` (`:1043-1045`).
- `SetParent` to itself is an error (`:1047-1049`).
- `SetParent` under a missing parent → `NotFoundError{Entity: "issue"}` (`:1050-1051`).

**`topics_derive_from_issues`** (`:1054-1066`)
- Three issues across two topics yield exactly `["parser","renderer"]` — distinct, ascending, never a stored list that could disagree with the issues.

**`export_carries_whole_store`** (`:1068-1142`)
- Export carries the **whole** store, archived work included; an export honoring the listing default would silently drop archived work from every backup and every diff (`:1108-1111`).
- Exported issue ids and their order: epic, child, archived, then the ten tied issues (`:1111`).
- `export.Relations` holds the one parent edge with `SrcID == child.ID` (`:1112-1114`).
- `export.Comments` holds the one comment (`:1115-1117`).
- `export.Labels` holds the one label `"perf"` (`:1118-1120`).
- `export.Events` is non-empty — the history must travel with the state (`:1121-1123`).
- `export.Events` order **equals** `ListAllEvents` order, compared by event id (`:1124-1138`). The case deliberately manufactures ten same-tick tie groups (each a create plus a combined action-and-fields `Apply`, which records several events sharing one timestamp) to make ordering observable; ten groups put agreement-by-coincidence at 2^-10 (`:1079-1102`).
- `export.ExportedAt` is non-zero (`:1139-1141`).

**`bulk_apply_creates_and_updates`** (`:1154-1197`)
- A batch of two create docs wired by `local_id`/`parent` yields `Created` entries keyed `"root"` and `"leaf"`, and the parent relationship is wired (`:1161-1178`).
- A doc naming a real `ID` is an **update**, not a second create: `Updated == [leafID]` and `Created` is empty (`:1180-1189`).
- The update persists (`:1190-1196`).

**`bulk_apply_compensates_a_failed_batch`** (`:1204-1221`)
- A batch whose second doc names a parent that is neither a batch-local name nor a real id passes the file's own validation and fails at the write (`:1210-1219`).
- After the failure, the default listing is **empty** — the issue created before the failure was undone (`:1220`).
- The contract trades atomicity for an account: creates are undone and what could not be undone is named in the error (`:1199-1203`).

**`import_tree_maps_local_ids`** (`:1223-1246`)
- A three-spec tree returns an `IDMap` with an entry per spec (`:1224-1234`).
- `parent: "root"` wires `leaf` as a child of `root` (`:1235`).
- `depends_on: ["leaf"]` on `other` produces exactly one `RelBlocks` edge with `Src == other`, `Dst == leaf` — the dependent is the edge's src (`:1237-1245`).

**`attribution_stamps_events`** (`:1248-1284`)
- Before `AttributeTo` is called, every event of an issue created then has `Attribution.Present() == false` — unattributed rather than half-attributed (`:1249-1264`).
- After `AttributeTo("stream-token")`, an issue created then records at least one event (`:1265-1267`).
- **Every** event that mutation produced carries attribution — it is stamped at the store's one insertion point, not by call sites that remembered (`:1268-1274`).
- `Attribution.Stream() == "stream-token"` (`:1275-1277`).
- `Attribution.Workspace()` is non-empty — the pair is complete or absent (`:1278-1282`).

**`local_issue_count_tracks_creates`** (`:1286-1312`)
- A fresh engine reports 0 rather than failing (`:1287-1295`).
- After two creates, one of which is soft-deleted, the count is **2** — it counts what the store holds, because a soft-deleted issue is still work that would be lost (`:1297-1311`).

### 3.4 Assertion helpers and what they imply

- `assertOrder` pins the order of a **default (unsorted) listing**, which is the only surface through which rank is observable across engines — **the `Rank` value itself is an engine's own encoding and is deliberately never asserted** (`:1357-1363`).
- `assertState` reads an issue back with `GetIssue` and pins its derived `State()`, including out-of-flow issues a default listing would not show (`:1365-1376`).
- `assertPrecedes` pins a relative position without pinning the whole listing (`:1378-1398`).
- `assertStrings` compares by content and order joined with `"|"`; **a nil result and an empty one compare equal on purpose** — an engine spelling "nothing here" as nil rather than a zero-length slice has not behaved differently (`:1409-1418`).
- `assertNotFound` requires `errors.As` to a `storage.NotFoundError` **and** an exact `Entity` match — callers dispatch on the type, and the entity tells a user WHICH thing was missing when an operation touches several (`:1420-1432`).
- Entity strings the suite pins: `"issue"`, `"comment"`, `"label"`, `"relation"`, `"parent relation"`.
- Every helper fails the test rather than returning an error, so a case body reads as the behavioral statement it is (`:1314-1318`).

### 3.5 What the conformance suite does NOT require

Derived from what the suite never exercises (the `cases` table at `:81-118` is the complete list):
- Any capability interface (`Syncer`, `Reconciler`, `Checkpointer`, `Repairer`, `SchemaMigrator`, `Importer`, `RawExecutor`) — capability presence is tested per-engine, not by the suite (`internal/storage/memory/capabilities_test.go:20-49`; `internal/storage/capabilities_test.go:96-150`).
- Concurrency/thread safety — the suite is sequential by construction, so the memory engine tests it separately (`internal/storage/memory/engine_test.go:14-22`).
- The exact `Rank` string encoding (`:1357-1359`).
- The exact minted-id shape (`:76-79`).
- `ListIssuesFilter.Resolutions` filtering (declared at `internal/storage/issues.go:157`, implemented at `internal/storage/memory/list.go:74-76`, but no case in the table exercises it).
- `ReplaceLabels`/`ListLabels` against a missing issue.
- `Close()` behavior beyond the engine factory's own cleanup.
- `AddRelation` cycle rejection for `blocks` (implemented at `internal/storage/memory/edges.go:230-263`, not exercised by any listed case).
- `ParseSortSpecs`, `ParseBulkSpecs`, `ParseImportTreeSpecs` — tested in the contract package instead (`internal/storage/specs_test.go`).
