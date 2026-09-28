# Storage Contract — Raw Behavioral Inventory

Scope: `internal/storage`, `internal/storage/memory`, `internal/storage/conformance`.
Derived exclusively from Go source. Every claim carries `file:line`.

Path prefix for all citations: `/Users/bmf/code/links-issue-tracker/`

---

## PART 1 — `internal/storage` (the contract layer)

### 1.1 Package identity and dependency rule

- Package `storage` declares what lit needs from a storage engine; no engine type appears in any signature (`internal/storage/doc.go`).
- Vocabulary rule: every type crossing the `Store` boundary is a `model` type or a type declared in this package. `Store` and its constituent interfaces may name no SQL row, branch, commit, or schema version (`internal/storage/doc.go`).
- Dependency direction: engine → contract → model, never back (`internal/storage/doc.go`).
- Capability interfaces are deliberately exempt from the vocabulary rule; naming an engine artifact is what makes something a capability rather than storage (`internal/storage/doc.go`).
- The core names no engine artifact; the vocabulary rule is stated without exceptions (`internal/storage/doc.go`).
- The conformance suite, not the interface, is stated as the actual specification (`internal/storage/doc.go`).

---

### 1.2 `Store` — the composed contract

`Store` = `IssueReader` + `IssueWriter` + `CommentStore` + `LabelStore` + `RelationStore` + `Ranker` + `BulkWriter` + `Exporter` + `Attributor` + `Close() error` (`internal/storage/contract.go`).

- `Close() error` is in the contract so a caller can release an engine's resources without knowing what they are; for Dolt this includes the workspace lock, and discarding the error strands it (`internal/storage/contract.go`).
- Deliberately absent from `Store` (they are capabilities): sync, schema migration, checkpoints, repair, raw test access — named as `Syncer`, `Reconciler`, `Checkpointer`, `Repairer`, `SchemaMigrator`, `Importer`, `RawExecutor` (`internal/storage/contract.go`).

---

### 1.3 `IssueReader` (`internal/storage/contract.go`)

General contract: every method is a pure read from the caller's view; two identical reads with no intervening mutation describe the same state (`internal/storage/contract.go`).

**`GetIssue(ctx, id string) (model.Issue, error)`** — `internal/storage/contract.go`
- A missing id returns `NotFoundError`, never a zero `Issue` (`internal/storage/contract.go`).

**`GetIssueDetail(ctx, id string) (model.IssueDetail, error)`** — `internal/storage/contract.go`
- Returns the issue plus its comments, history, and structural edges — the full single-issue view (`internal/storage/contract.go`).
- Callers needing only edges for many issues use `GetRelationsByIDs` instead (`internal/storage/contract.go`).

**`ListIssues(ctx, filter ListIssuesFilter) ([]model.Issue, error)`** — `internal/storage/contract.go`
- Ordered by `SortBy`, then — always, as the final key — by id ascending (`internal/storage/contract.go`).
- With no `SortBy`: rank ascending, ties broken by id (`internal/storage/contract.go`).
- The trailing id key is contract, not engine convenience: without it, a sort on any duplicated field value leaves tied rows in engine-incidental order and two engines diverge (`internal/storage/contract.go`).
- `SortBy` may name only a `SortFields` member (`internal/storage/contract.go`).

**`ListTopics(ctx) ([]string, error)`** — `internal/storage/contract.go`
- Distinct non-empty topics that *live* issues carry, ascending. Derived vocabulary, never stored (`internal/storage/contract.go`).

**`ListAllEvents(ctx) ([]model.IssueEvent, error)`** — `internal/storage/contract.go`
- Whole issue history, oldest first by creation time, ties broken by event id ascending (`internal/storage/contract.go`).
- Used by export and by claim derivation; claim derivation needs the whole history because the establishing event for a lane's holder can be arbitrarily old, so a recency cutoff would drop the claims it was meant to speed up (`internal/storage/contract.go`).
- The id tie-break is explicitly NOT a happens-before claim: event ids are random, so same-tick events come back in an order unrelated to recording order. Both engines are wrong about causality identically, on purpose (`internal/storage/contract.go`).

**`LocalIssueCount(ctx) (int64, error)`** — `internal/storage/contract.go`
- How many issues the store holds; the adopt-safety signal for `lit init` (`internal/storage/contract.go`).
- A never-written store reports 0 rather than erroring (`internal/storage/contract.go`).

---

### 1.4 `IssueWriter` (`internal/storage/contract.go`)

**`CreateIssue(ctx, in CreateIssueInput) (model.Issue, error)`** — `internal/storage/contract.go`
- Records a new issue and returns it as stored, with its id minted (`internal/storage/contract.go`).

**`Apply(ctx, id string, c Change) (model.Issue, error)`** — `internal/storage/contract.go`
- The single execution path for issue-record changes; the `Change` value carries all variability, so there is no second mutation verb (`internal/storage/contract.go`).
- A pure no-op records nothing in history (`internal/storage/contract.go`).
- An illegal transition is refused rather than silently ignored (`internal/storage/contract.go`).

---

### 1.5 `CommentStore` (`internal/storage/contract.go`)

**`AddComment(ctx, in AddCommentInput) (model.Comment, model.Issue, error)`** — `internal/storage/contract.go`
- Returns both the new comment and the issue as it stands after the write, so a caller rendering both does not re-read (`internal/storage/contract.go`).

**`DeleteComment(ctx, commentID string) (model.Comment, error)`** — `internal/storage/contract.go`
- Removes one comment and returns what was removed, so the deletion is reportable without a prior read (`internal/storage/contract.go`).

---

### 1.6 `LabelStore` (`internal/storage/contract.go`)

Interface-wide rule: every mutating method returns the issue's *resulting label set*, so "what labels does it have now" is never a follow-up read a concurrent writer could answer differently (`internal/storage/contract.go`).

- `AddLabel(ctx, in AddLabelInput) ([]string, error)` — `internal/storage/contract.go`
- `RemoveLabel(ctx, issueID, labelName string) ([]string, error)` — `internal/storage/contract.go`
- `ReplaceLabels(ctx, issueID string, labels []string, createdBy string) error` — sets the whole set at once; the authored-document path, where the file states the labels an issue has rather than a delta (`internal/storage/contract.go`)
- `ListLabels(ctx, issueID string) ([]string, error)` — `internal/storage/contract.go`

---

### 1.7 `RelationStore` (`internal/storage/contract.go`)

Parentage gets its own two verbs rather than riding `AddRelation` because it is the one edge with an arity rule — at most one parent — and `SetParent` is where replacement happens as a single act (`internal/storage/contract.go`).

**`AddRelation(ctx, in AddRelationInput) (model.Relation, error)`** — `internal/storage/contract.go`

**`RemoveRelation(ctx, srcID, dstID string, relType model.RelationType) error`** — `internal/storage/contract.go`

**`ListRelationsForIssue(ctx, issueID string, types ...model.RelationType) ([]model.Relation, error)`** — `internal/storage/contract.go`
- Every edge incident to the issue in *either* direction, oldest first, optionally narrowed to the named types (`internal/storage/contract.go`).

**`GetRelationsByIDs(ctx, ids []string) (map[string]IssueRelations, error)`** — `internal/storage/contract.go`
- The batch neighborhood read: one call for many issues, counterparts hydrated, avoiding `GetIssueDetail`'s per-issue comment and history cost (`internal/storage/contract.go`).

**`SetParent(ctx, in SetParentInput) (model.Relation, error)`** — `internal/storage/contract.go`
- Wires a child under a parent, replacing any existing parent in one act, returning the edge written. Both endpoints must exist and an issue may not be its own parent (`internal/storage/contract.go`).

**`ClearParent(ctx, childID string) error`** — `internal/storage/contract.go`
- Clearing a child that has no parent is `NotFoundError`, not a silent success (`internal/storage/contract.go`).

---

### 1.8 `Ranker` (`internal/storage/contract.go`)

- The vocabulary is relative intents only — never stored positions — because "above Y" survives everyone else reordering around it (`internal/storage/contract.go`).
- The two anchored verbs return `RankMove` because rank frames are nested: an intent naming two issues in different epics is honored against the containing ancestors that are comparable, and the substitution is returned so the caller can surface it (`internal/storage/contract.go`).

- `RankAbove(ctx, issueID, targetID string) (RankMove, error)` — `internal/storage/contract.go`
- `RankBelow(ctx, issueID, targetID string) (RankMove, error)` — `internal/storage/contract.go`
- `RankToTop(ctx, issueID string) error` — `internal/storage/contract.go`
- `RankToBottom(ctx, issueID string) error` — `internal/storage/contract.go`
- `RankSet(ctx, ids []string) ([]RankSetResolution, error)` — imposes a total order on the named issues at once, returning which representative each name resolved to (`internal/storage/contract.go`)

**`RankMove`** — `internal/storage/rank.go`
- `MovedID string` — the issue actually re-ranked after frame resolution.
- `AnchorID string` — the issue it was re-ranked relative to.
- For frame-mates these are the inputs unchanged; cross-frame, one or both are containing ancestors (`internal/storage/rank.go`).

**`RankSetResolution`** — `internal/storage/rank.go`
- `NamedID string` (json `named_id`), `RankedID string` (json `ranked_id`). Equal for frame-mates; when they differ the caller must surface the substitution (`internal/storage/rank.go`).

---

### 1.9 `BulkWriter` (`internal/storage/contract.go`)

Interface-wide failure contract: **compensated, not transactional**. A batch that fails partway undoes the issues it created, and the error names every one it could not undo. Updates already applied in a mixed batch are NOT reverted, which is why the error text is the caller's only complete account (`internal/storage/contract.go`).

- `BulkApply(ctx, prefix, actor string, specs []BulkIssueSpec) (BulkApplyResult, error)` — mixed create/update batch, resolving intra-batch references by `LocalID` (`internal/storage/contract.go`)
- `ImportTree(ctx, prefix string, specs []ImportTreeSpec) (ImportTreeResult, error)` — creates a whole issue tree, returning local-ID → real-ID mapping (`internal/storage/contract.go`)

---

### 1.10 `Exporter` (`internal/storage/contract.go`)

- `Export(ctx) (model.Export, error)` — serializes the store's entire contents as one value (`internal/storage/contract.go`).
- It is core rather than a capability because it is the differential oracle surface: the one full-state read two engines both serve (`internal/storage/contract.go`).

---

### 1.11 `Attributor` (`internal/storage/contract.go`)

- `AttributeTo(streamToken string)` — names the checkout whose work the engine is about to record (`internal/storage/contract.go`).
- Attribution is the store's, not each mutation's: an engine stamps it at its single event-insertion point (`internal/storage/contract.go`).
- An empty token leaves the engine **unattributed** rather than half-attributed — the contract for a read-mode open of a checkout that has never mutated, and why the method takes no presence flag (`internal/storage/contract.go`).

---

### 1.12 Error taxonomy (`internal/storage/errors.go`)

**`NotFoundError{Entity, ID string}`** — `internal/storage/errors.go`
- `Error()` renders `fmt.Sprintf("%s %q not found", e.Entity, e.ID)` (`internal/storage/errors.go`).
- Every engine returns it — wrapped or bare, matched with `errors.As` — for a read or mutation against an id that no issue, comment, or relation holds (`internal/storage/errors.go`).
- Callers dispatch on it; the CLI maps it to its own exit code (`internal/storage/errors.go`).

**`ValidationError{Message string}`** — `internal/storage/errors.go`
- `Error()` returns `Message` verbatim (`internal/storage/errors.go`).
- Returned when a domain constraint (field value, type, range) is violated (`internal/storage/errors.go`).

**`UnsupportedError{Capability, Engine string}`** — `internal/storage/capabilities.go`
- `Error()` renders `fmt.Sprintf("%s engine does not offer the %s capability", e.Engine, e.Capability)` (`internal/storage/capabilities.go`).
- `Capability` is the capability's name as `Capability.Name()` reports it; `Engine` names the concrete engine asked (`internal/storage/capabilities.go`).

Other error surfaces in the contract package: `ParseSortSpecs` returns `ValidationError` for an unrecognized direction (`internal/storage/sort.go`); `ParseBulkSpecs` returns `fmt.Errorf("bulk: parse spec: %w", err)` (`internal/storage/bulk.go` → `internal/storage/specs.go`); `ParseImportTreeSpecs` returns `fmt.Errorf("import: parse spec: %w", err)` (`internal/storage/specs.go`) and `errors.New("import: unexpected trailing data after spec array")` (`internal/storage/specs.go`).

---

### 1.13 Input/option structs

#### `RankPlacement` (`internal/storage/issues.go`)
- `RankBottom RankPlacement = iota` — sorts after all existing items; **the zero value and the default** (`internal/storage/issues.go`).
- `RankTop` — sorts before every item in the frame it is filed into (`internal/storage/issues.go`).
- The zero value being bottom is the whole enforcement mechanism for "one default across every creation surface" (`internal/storage/issues.go`).
- Rationale that bottom-of-order is bottom-of-frame: a child's rank is only compared against siblings', composite rank keyed on the containing epic's rank first (`internal/storage/issues.go`).

#### `CreateIssueInput` (`internal/storage/issues.go`)
| Field | Type | Effect |
|---|---|---|
| `Title` | `string` | `internal/storage/issues.go` |
| `Description` | `string` | `internal/storage/issues.go` |
| `Prompt` | `string` | `internal/storage/issues.go` |
| `IssueType` | `model.IssueType` | Already-parsed vocabulary, never a raw flag string; trust boundaries route through `model.ParseIssueType`. Zero value means "unspecified" and defaults to task (`internal/storage/issues.go`) |
| `Topic` | `string` | `internal/storage/issues.go` |
| `ParentID` | `string` | `internal/storage/issues.go` |
| `Priority` | `model.Priority` | Already-parsed domain vocabulary; trust boundaries route through `model.ParsePriority` or `model.CanonicalPriority` (`internal/storage/issues.go`) |
| `Assignee` | `string` | `internal/storage/issues.go` |
| `Lane` | `string` | `internal/storage/issues.go` |
| `Labels` | `[]string` | `internal/storage/issues.go` |
| `Placement` | `RankPlacement` | Zero value (`RankBottom`) appends, so an authored batch keeps its order for free (`internal/storage/issues.go`) |
| `Prefix` | `string` | Workspace's cosmetic ID prefix (e.g. `"links"` → `links-foo-abc1`), sourced from workspace config at the call site; not persisted as derived state (`internal/storage/issues.go`) |

#### `UpdateIssueInput` (`internal/storage/issues.go`)
The field-axis patch. Only columns the field axis owns are representable, so a status write through the field path is unconstructible (`internal/storage/issues.go`).

| Field | Type | Semantics |
|---|---|---|
| `Title` | `*string` | nil = leave unchanged (`internal/storage/issues.go`) |
| `Description` | `*string` | `internal/storage/issues.go` |
| `Prompt` | `*string` | `internal/storage/issues.go` |
| `IssueType` | `*model.IssueType` | `internal/storage/issues.go` |
| `Priority` | `*model.Priority` | `internal/storage/issues.go` |
| `Assignee` | `*string` | `internal/storage/issues.go` |
| `Lane` | `*string` | `internal/storage/issues.go` |
| `Labels` | `*[]string` | `internal/storage/issues.go` |
| `Reason` | `string` | Optional free text recorded on the field-change event (`internal/storage/issues.go`) |

- `IsEmpty()` is true iff **all eight pointer fields** are nil; `Reason` is deliberately excluded from the emptiness test (`internal/storage/issues.go`).

#### `Change` (`internal/storage/issues.go`)
| Field | Type | Semantics |
|---|---|---|
| `Action` | `model.Action` | nil = no transition. Which axis it drives (status machine vs retention) is the sum's own structure — the `StatusAction`/`RetentionAction` partition — never a caller-side mode (`internal/storage/issues.go`) |
| `Fields` | `UpdateIssueInput` | empty = no field mutations (`internal/storage/issues.go`) |
| `Actor` | `string` | THE actor for the whole change — one call, one author, recorded on both events it may produce (`internal/storage/issues.go`) |
| `Reason` | `string` | Belongs to the **transition** event; `Fields.Reason` belongs to the **field-change** event. A combined change records two events whose reasons are independently set (`internal/storage/issues.go`) |

- `IsEmpty()` is true iff `Action == nil && Fields.IsEmpty()` (`internal/storage/issues.go`).

#### `SortSpec` (`internal/storage/issues.go`)
- `Field string`, `Desc bool`.

#### `SortFields` — the closed set (`internal/storage/issues.go`)
Exactly ten keys: `"id"`, `"title"`, `"status"`, `"priority"`, `"rank"`, `"type"`, `"topic"`, `"assignee"`, `"created_at"`, `"updated_at"`.
- Exists because each engine binds these names to its own mechanism (Dolt → SQL columns; memory → comparison functions); the set lives in the contract and each engine's binding is checked against it by the conformance suite (`internal/storage/issues.go`).
- **Documented fault**: `"status"` orders the STORED status encoding, not derived lifecycle state. A container has no stored status, so it carries no value on this axis and orders ahead of every leaf ascending, behind every leaf descending, whatever state it derives to. The same listing's status FILTER reads derived state, so filter and sort disagree about what "status" means. This is stated as shipped behavior so both engines are wrong identically; correcting it is ticket `links-store-seam-q35v.6` (`internal/storage/issues.go`).

#### `ListIssuesFilter` (`internal/storage/issues.go`)
Rule for the whole struct: **every slice is an OR within itself and an AND against the other criteria**; every zero value means "do not constrain on this axis" (`internal/storage/issues.go`).

| Field | Type | Effect |
|---|---|---|
| `Statuses` | `[]model.State` | `internal/storage/issues.go` |
| `Resolutions` | `[]model.Resolution` | `internal/storage/issues.go` |
| `IssueTypes` | `[]model.IssueType` | include-list (`internal/storage/issues.go`) |
| `ExcludeIssueTypes` | `[]model.IssueType` | exclude-list (`internal/storage/issues.go`) |
| `Assignees` | `[]string` | `internal/storage/issues.go` |
| `SearchTerms` | `[]string` | `internal/storage/issues.go` |
| `IDs` | `[]string` | `internal/storage/issues.go` |
| `ParentIDs` | `[]string` | direct children of the named issues, read off the parent-child edge, never the id prefix; the one axis whose criteria must exist: an id naming no issue makes `ListIssues` return `NotFoundError` (`internal/storage/issues.go`) |
| `HasComments` | `*bool` | nil = unconstrained (`internal/storage/issues.go`) |
| `LabelsAll` | `[]string` | `internal/storage/issues.go` |
| `UpdatedAfter` | `*time.Time` | `internal/storage/issues.go` |
| `UpdatedBefore` | `*time.Time` | `internal/storage/issues.go` |
| `IncludeArchived` | `bool` | one of the two axes whose default is a filter rather than an absence (`internal/storage/issues.go`) |
| `IncludeDeleted` | `bool` | ditto (`internal/storage/issues.go`) |
| `SortBy` | `[]SortSpec` | `internal/storage/issues.go` |
| `Limit` | `int` | `internal/storage/issues.go` |

#### Edge inputs (`internal/storage/edges.go`)
- `AddCommentInput{IssueID, Body, CreatedBy string}` — `internal/storage/edges.go`
- `AddLabelInput{IssueID, Name, CreatedBy string}` — `internal/storage/edges.go`
- `AddRelationInput{SrcID, DstID string; Type model.RelationType; CreatedBy string}` — `internal/storage/edges.go`
- `SetParentInput{ChildID, ParentID, CreatedBy string}` — `internal/storage/edges.go`

#### `IssueRelations` (`internal/storage/edges.go`)
- `Issue model.Issue`, `Parent *model.Issue`, `Children []model.Issue`, `DependsOn []model.Issue`, `Blocks []model.Issue`.
- Lightweight per-issue shape WITHOUT the comment/event/related payload `GetIssueDetail` loads (`internal/storage/edges.go`).
- **Direction convention is contract**: a blocks edge runs `src=dependent`, `dst=dependency`, so `DependsOn` and `Blocks` are the two readings of one edge set. Two engines bucketing differently would disagree about which work is ready (`internal/storage/edges.go`).

---

### 1.14 Bulk / import spec types (`internal/storage/bulk.go`)

#### `BulkIssueSpec` (`internal/storage/bulk.go`)
`ID` is the selector: present → the doc is an **update patch** of only the fields it sets; absent → the doc **creates** an issue and behaves like `ImportTreeSpec`'s flat form (`internal/storage/bulk.go`). Pointer fields are exactly the update-patch fields: nil = leave unchanged, set = write this value (`internal/storage/bulk.go`). The struct tags are contract, not engine detail (`internal/storage/bulk.go`).

| Field | Type | YAML tag |
|---|---|---|
| `LocalID` | `string` | `local_id,omitempty` |
| `ID` | `string` | `id,omitempty` |
| `Title` | `*string` | `title,omitempty` |
| `Description` | `*string` | `description,omitempty` |
| `Prompt` | `*string` | `prompt,omitempty` |
| `IssueType` | `*string` | `type,omitempty` |
| `Topic` | `*string` | `topic,omitempty` |
| `Priority` | `*int` | `priority,omitempty` |
| `Assignee` | `*string` | `assignee,omitempty` |
| `Labels` | `*[]string` | `labels,omitempty` |
| `Lane` | `*string` | `lane,omitempty` |
| `Parent` | `string` | `parent,omitempty` |
| `DependsOn` | `[]string` | `depends_on,omitempty` |
| `Reason` | `string` | `reason,omitempty`; applies only to updates — there is no prior state to annotate a create against (`internal/storage/bulk.go`) |

#### `BulkApplyResult` (`internal/storage/bulk.go`)
- `Created map[string]string` — maps each create document's own reference (its `LocalID` if set, otherwise its new real ID) to the real ID it was created under, so every create is nameable even when the file gave no `LocalID` (`internal/storage/bulk.go`).
- `Updated []string` — real IDs of every updated issue, in the order applied (`internal/storage/bulk.go`).

#### `ImportTreeSpec` (`internal/storage/bulk.go`)
JSON tags: `local_id`, `title`, `description,omitempty`, `prompt,omitempty`, `type`, `topic`, `priority`, `assignee,omitempty`, `labels,omitempty`, `parent,omitempty`, `depends_on,omitempty`. `LocalID` is opaque — used inside the spec to wire `Parent` and `DependsOn`, replaced with the generated lit issue ID at import time (`internal/storage/bulk.go`).

#### `ImportTreeResult` (`internal/storage/bulk.go`)
- `IDMap map[string]string` (json `id_map`) — local-ID → real-issue-ID mapping.

---

### 1.15 Authored-file parsers (`internal/storage/specs.go`)

They live beside the specs rather than in an engine because the schema is the contract's; every engine reads the same authored file the same way (`internal/storage/specs.go`).

**`ParseBulkSpecs(data []byte) ([]BulkIssueSpec, error)`** — `internal/storage/specs.go`
- YAML decoder with `KnownFields(true)`: any field the schema does not name is rejected (`internal/storage/specs.go`).
- Loops decoding documents until `io.EOF`; **multi-document YAML** (`---`-separated) yields one spec per document (`internal/storage/specs.go`).
- Any non-EOF decode error → `fmt.Errorf("bulk: parse spec: %w", err)` (`internal/storage/specs.go`).
- Test: an unknown field `children` produces an error naming `"children"` (`internal/storage/specs_test.go`).
- Test: two `---`-separated documents produce 2 specs (`internal/storage/specs_test.go`).

**`ParseImportTreeSpecs(data []byte) ([]ImportTreeSpec, error)`** — `internal/storage/specs.go`
- JSON decoder with `DisallowUnknownFields()` (`internal/storage/specs.go`).
- Decodes exactly one array; decode error → `fmt.Errorf("import: parse spec: %w", err)` (`internal/storage/specs.go`).
- Trailing data after the array (`dec.More()`) → `errors.New("import: unexpected trailing data after spec array")` (`internal/storage/specs.go`).
- Test: a nested `children` array is rejected by name (`internal/storage/specs_test.go`).
- Test: two concatenated arrays produce a "trailing data" error (`internal/storage/specs_test.go`).

---

### 1.16 `ParseSortSpecs` (`internal/storage/sort.go`)

- Input: comma-separated sort expression, e.g. `"rank:asc,updated_at:desc"` (`internal/storage/sort.go`).
- Splits on `,`; each part is trimmed; empty parts are skipped (`internal/storage/sort.go`).
- A bare field (`"rank"`) defaults to ascending (`internal/storage/sort.go`).
- With a `:`, the value is split into at most 2 chunks; field is trimmed; direction is lowercased and trimmed (`internal/storage/sort.go`).
- `"asc"` → `Desc=false`; `"desc"` → `Desc=true`; anything else → `ValidationError{Message: fmt.Sprintf("unsupported sort direction %q", direction)}` (`internal/storage/sort.go`).
- Empty and whitespace-only expressions yield `nil, nil` (`internal/storage/sort.go`).
- It is THE parser from sort expression to `[]SortSpec`; both the `--sort` flag and the `--query sort:` token route through it (`internal/storage/sort.go`).
- Note: the field name is **not** validated against `SortFields` here — that rejection happens in the engine (`internal/storage/sort.go`; cf. `internal/storage/memory/list.go`).

---

### 1.17 Capabilities system (`internal/storage/capabilities.go`)

**Granularity rule**: two operations share a capability only when no engine could plausibly offer one without the other (`internal/storage/capabilities.go`).
**Absence rule**: absence is answered, never guessed or faked — a caller asks with `Of`, which returns the interface or an `UnsupportedError` (`internal/storage/capabilities.go`).

#### `Capability` interface (`internal/storage/capabilities.go`)
- `Name() string` — stable identifier used in messages and listings (`internal/storage/capabilities.go`).
- `OfferedBy(engine Store) bool` — the enumeration question (`internal/storage/capabilities.go`).
- `unexported()` — seals the interface to this package (`internal/storage/capabilities.go`); type is closed so only this package can mint a capability (`internal/storage/capabilities.go`).

#### `capability[C any]{name string}` (`internal/storage/capabilities.go`)
- `Name()` returns `c.name` (`internal/storage/capabilities.go`).
- `OfferedBy(engine)` is derived from `Of` (`_, err := c.Of(engine); return err == nil`) so the two cannot disagree (`internal/storage/capabilities.go`).
- `Of(engine Store) (C, error)`: type-asserts `any(engine).(C)`; on failure returns the zero `C` and `UnsupportedError{Capability: c.name, Engine: fmt.Sprintf("%T", engine)}` (`internal/storage/capabilities.go`).

#### The seven capabilities (`internal/storage/capabilities.go`)
| Value | Name string | Interface |
|---|---|---|
| `Sync` | `"sync"` | `Syncer` |
| `Reconcile` | `"reconcile"` | `Reconciler` |
| `Checkpoints` | `"checkpoints"` | `Checkpointer` |
| `Repair` | `"repair"` | `Repairer` |
| `SchemaMigration` | `"schema-migration"` | `SchemaMigrator` |
| `Import` | `"import"` | `Importer` |
| `TestSupport` | `"test-support"` | `RawExecutor` |

- `all` is the enumeration every listing derives from (`internal/storage/capabilities.go`).
- `Capabilities() []Capability` returns `slices.Clone(all)` — the caller's own slice (`internal/storage/capabilities.go`).
- `Offered(engine Store) []Capability` returns the capabilities the engine implements, **in `Capabilities()` order** (`internal/storage/capabilities.go`).

#### `Syncer` (`internal/storage/capabilities.go`)
- `SyncAddRemote(ctx, name, url string) error`
- `SyncRemoveRemote(ctx, name string) error`
- `SyncListRemotes(ctx) ([]SyncRemote, error)`
- `SyncStatus(ctx) (SyncStatusReport, error)` — local side only: build, position, pending changes, peers; **contacts no network**
- `SyncFreshness(ctx, remote, branch string) (SyncFreshness, error)` — local branch position against a peer as of the last fetch or push; reads local refs, never contacts the network, which is what lets a read-only command call it
- `SyncFetch(ctx, remote string, prune bool) error`
- `SyncPush(ctx, remote, branch string, setUpstream, force bool) (SyncPushResult, error)`
- `SyncPushFromClone(ctx, remote, branch string, setUpstream, force bool) (SyncPushResult, error)` — `SyncPush` for a store that is a frozen clone of a live one (the on-change mirror's push): a push the remote rejects is re-checked, and a remote already carrying the store's HEAD yields a `Superseded` result rather than an error.
- `SyncRemoteMirrorHolds(ctx, remote string, commits []string) (bool, error)`: whether this store's local mirror of the remote's data holds every one of the given git commit ids; no network. After a push from this store that landed without being superseded, a held advertised commit is this store's own push, never a peer's later one.
- `SyncPull(ctx, remote, branch string) (SyncPullResult, error)`
- `SyncReceive(ctx, remote, branch string) (SyncReceiveResult, error)` — fetches and fast-forwards when and only when local is strictly behind; never merges; a divergence it meets is reported, never healed here
- `SyncSettleReceived(ctx, remote, branch string) (SyncReceiveResult, error)` — `SyncReceive`'s second half alone: reads the tracking ref as it stands (a fetch that ran on a clone and was landed here) and fast-forwards on the same terms; no network
- `SyncCompact(ctx, mode GCMode) (CompactionOutcome, error)` — reclaims local storage at the requested depth with no remote involved
- `CompactIfDue(ctx) (CompactionOutcome, error)` — compacts only when the engine's own accounting says a pass is owed; the engine owns that judgment because what makes a pass due is a fact about how it stores data
- `SyncCompactAndPush(ctx, remote, branch string, setUpstream, force bool) (SyncPushResult, error)`
- `GetSyncState(ctx) (SyncState, error)` / `RecordSyncState(ctx, state SyncState) error` — carry the staleness marker across commands
- Compaction sits inside `Syncer` because `SyncCompactAndPush` compacts and pushes under a single commit-lock acquisition — one atomic operation not assemblable from `SyncCompact` + `SyncPush` (`internal/storage/capabilities.go`).
- Nothing in `Syncer` resolves a divergence — that is `Reconciler` (`internal/storage/capabilities.go`).

#### `Reconciler` (`internal/storage/capabilities.go`)
- `SyncReconcile(ctx, remote, branch string) (SyncReconcileResult, error)` — merges a divergence field-aware and replays it into linear history, or — when a free-text field moved on both sides — holds the conflict and commits nothing
- `SyncReconcileCombine(ctx, remote, branch string) (SyncReconcileResult, error)` — settles an unrelated-history divergence by union, keeping every issue from both sides
- `SyncReconcileResolved(ctx, remote, branch string, resolutions []merge.ProseResolution) (SyncReconcileResult, error)` — finishes a reconcile held for prose
- `SyncResolveUnrelated(ctx, remote, branch string, choice UnrelatedResolution, ownerApproval string) (SyncReconcileResult, error)` — settles by taking one side wholesale; destroys the other side's unique issues, which is why it takes an owner approval bound to this exact fork rather than a bare confirmation flag
- `SyncResetToRemoteHead(ctx, remote, branch string) error` — abandons local history for the peer's

#### `Checkpointer` (`internal/storage/capabilities.go`)
- `CreateCheckpoint(ctx, prefix string) (Checkpoint, error)`
- `ListCheckpoints(ctx, prefix string) ([]Checkpoint, error)`
- `PruneCheckpoints(ctx, prefix string, retain int) error` — keeps the newest `retain` checkpoints under prefix and drops the rest
- `ResetToCheckpoint(ctx, name string) error`

#### `Repairer` (`internal/storage/capabilities.go`)
- `Doctor(ctx) (HealthReport, error)` — examines and reports; changes nothing
- `FixIntegrity(ctx) (HealthReport, error)` — repairs dangling rows, self-referential edges, edges stored in the wrong order; reports the state it left behind. Takes **no** "actually repair" flag because the examine-only arm is `Doctor`
- `FixRankInversions(ctx) (int, error)` — repairs orderings that contradict themselves and reports how many it corrected; exists only because rank may be stored as a fractional position that concurrent writers can invert; keeps the existing order wherever the edges allow — an issue falls behind one that stood after it only while it waits on a dependency — and a second run over a repaired store writes nothing

#### `SchemaMigrator` (`internal/storage/capabilities.go`)
- `AppliedSchemaVersion(ctx) (int64, error)` — the shape version the store is currently at
- `Downgrade(ctx, targetSchemaVersion int64) error` — moves the store back to an older shape so a binary that predates the current one can open it

#### `Importer` (`internal/storage/capabilities.go`)
- `ReplaceFromExport(ctx, export model.Export) error` — replaces the store's entire contents with an export. Only the import half is optional; `Export` is core.

#### `RawExecutor` (`internal/storage/capabilities.go`)
- `ExecRawForTest(ctx, query string, args ...any) error` — runs an engine-native statement so tests can plant states the contract cannot express (a corrupted row, a stale schema).

---

### 1.18 Sync/reconcile vocabulary (`internal/storage/sync.go`)

These types live in the contract, not in an engine, because a capability interface can only name types every engine can name (`internal/storage/sync.go`).

- **`SyncState{Path, ContentHash string}`** — the store's on-disk content at a point in time: where it lives and a digest of what it held; the staleness signal (`internal/storage/sync.go`).
- **`SyncRemote{Name, URL string}`** (json `name`, `url`) — `internal/storage/sync.go`.
- **`SyncStatusRow{TableName string; Staged bool; Status string}`** (json `table_name`, `staged`, `status`) — one unit of pending local change; what a "table" is belongs to the engine, and the contract carries the row through uninterpreted (`internal/storage/sync.go`).
- **`SyncStatusReport`** (`internal/storage/sync.go`) — `EngineVersion` (json `engine_version`; the build of whatever engine answered, singular because the whole report describes one engine), `Branch`, `HeadCommit`, `HeadMessage`, `Status []SyncStatusRow`, `Remotes []SyncRemote`.
- **`SyncFreshnessState`** string enum (`internal/storage/sync.go`): `never_synced`, `up_to_date`, `ahead`, `behind`, `diverged`.
- **`SyncFreshness`** (`internal/storage/sync.go`) — `Remote`, `Branch`, `Synced bool`, `Ahead int64`, `Behind int64`, `OldestDivergedUnix int64`.
  - Reports position relative to `remotes/<Remote>/<Branch>`, so `Behind` is "as of last fetch"; computing it never contacts the network (`internal/storage/sync.go`).
  - `Synced` is false when the ref does not exist; `Ahead`/`Behind` are zero in that state (`internal/storage/sync.go`).
  - `OldestDivergedUnix` is the Unix seconds of the OLDEST commit in the union of the two divergent ranges — when the fork first happened. Zero when nothing diverged (both counts 0) or never synced. Raw timestamp, not an age (`internal/storage/sync.go`).
  - **`State()`** derivation (`internal/storage/sync.go`): `!Synced` → `never_synced`; `Ahead==0 && Behind==0` → `up_to_date`; `Behind==0` → `ahead`; `Ahead==0` → `behind`; otherwise → `diverged`.
- **`SyncReceiveState`** enum (`internal/storage/sync.go`): `up_to_date` (local already at remote head, fetch found nothing); `fast_forwarded` (local strictly behind, advanced with no merge commit — the only state that mutates local data); `ahead` (local has unpushed commits and remote has nothing new); `diverged` (both moved; fast-forward impossible; the background receive deliberately does NOT merge); `never_synced` (no remote-tracking ref even after a fetch).
- **`SyncReceiveResult{State SyncReceiveState; Ahead, Behind, OldestDivergedUnix int64}`** — `OldestDivergedUnix` is zero unless diverged (`internal/storage/sync.go`).
- **`SyncPullState`** enum (`internal/storage/sync.go`): `up_to_date`, `fast_forwarded`, `linearized`, `prose_pending` (every code-owned field settled but a free-text field diverged on both sides; nothing committed), `unrelated_histories` (no common ancestor; nothing committed), `ahead`, `never_synced`.
- **`SyncPullResult`** (`internal/storage/sync.go`) — `State` (json `state`), `Ahead`, `Behind`, `Pending []merge.ProsePending` (json `pending,omitempty`), `OldestDivergedUnix` (zero unless the pull met a divergence), `Unrelated *UnrelatedInventory` (non-nil only for `SyncPullUnrelated`).
- **`GCMode`** (`internal/storage/sync.go`): `GCNewGen = iota` — collects recent history not yet archived; the cheap routine depth, cannot reclaim anything already archived. `GCFull` — additionally rewrites archived history; the only depth that reclaims what earlier passes left behind; costs proportionally to the whole store.
  - `Valid()` returns true only for `GCNewGen` and `GCFull`; it is the door guard a `Syncer` runs before collecting, so an out-of-range depth is rejected loudly rather than collapsing to the shallower default (`internal/storage/sync.go`).
  - `String()` → `"newgen"`, `"full"`, or `fmt.Sprintf("unknown(%d)", int(m))` (`internal/storage/sync.go`).
- **`CompactionOutcome`** (`internal/storage/sync.go`) — `Ran bool` (whether a pass was actually performed; a due-check that found nothing owing returns false, an ordinary outcome and not a failure); `Depth GCMode` (meaningful only when `Ran`); `Detail string` (the engine's own already-rendered account; an engine that could not measure its own reclaim says so here rather than blanking the field — empty belongs only to the outcome that did nothing).
- **`SyncPushResult`** (`internal/storage/sync.go`) — `Status int64` (json `status`), `Message string` (json `message`, the engine's verbatim push output, rendered as `raw`), `Head string` (json `head`; the commit the push sent as HEAD, read under the lock the push ran under), `Superseded string` (json `superseded,omitempty`; the engine's rejection, set when the push was rejected and the re-check found the remote already carrying `Head`; empty for a push that landed itself), `Maintenance string` (json `maintenance,omitempty`; deliberately separate from `Message` so `raw` stays raw; empty when nothing worth reporting, and every state a reader would act on — work performed, work declined, an I/O failure — is non-empty).
- **`SyncReconcileState`** enum (`internal/storage/sync.go`): `not_diverged`, `linearized`, `prose_pending`, `unrelated_histories`, `took_local`, `took_remote`, `combined`. `took_local`/`took_remote` are produced only by `SyncResolveUnrelated`; the autonomous reconcile never picks a side. `combined` is produced only by the combine resolution, and only when every prose field settled — an on-both prose divergence lands `prose_pending` instead.
- **`SyncReconcileResult`** (`internal/storage/sync.go`) — `State`, `Ahead`, `Behind`, `LocalHead`, `RemoteHead`, `BaseCommit`, `Pending []merge.ProsePending` (empty unless `prose_pending`), `Unrelated *UnrelatedInventory` (non-nil only for `unrelated_histories`), `Replayed int` (counts the folded side's commits that landed individually; zero for every non-mutating outcome and for a fold whose every per-commit projection was already contained in the spine; read back off the spine after the replay).
- **`UnrelatedInventory`** (`internal/storage/sync.go`) — `OnlyLocal`, `OnlyRemote`, `OnBoth []string` (json `only_local,omitempty` etc.). The three slices are sorted and mutually disjoint by construction; every id present on either side lands in exactly one (`internal/storage/sync.go`).
- **`UnrelatedResolution`** string (`internal/storage/sync.go`): `TakeLocal = "local"` (keeps local backlog, discards remote-only issues), `TakeRemote = "remote"` (keeps remote backlog, discards local-only issues).
  - `Valid()` returns true only for those two; the door guard every `Reconciler` runs before touching the store (`internal/storage/sync.go`).

---

### 1.19 Checkpoint/repair vocabulary (`internal/storage/maintenance.go`)

- **`Checkpoint`** (`internal/storage/maintenance.go`) — `Name string` (format `"<prefix>-<unix-nano>"`), `Prefix string` (caller label, e.g. `"pre-migrate"`), `CreatedAt time.Time` (parsed from the unix-nano suffix in `Name`), `Anchor string` (opaque engine-side identity of the captured state; the contract requires only that handing it back names the same state, never that it is a hash or a commit).
  - The name encodes the prefix and timestamp so `ListCheckpoints` can reconstruct the set without external metadata storage (`internal/storage/maintenance.go`).
- **`HealthReport`** (`internal/storage/maintenance.go`) — json keys: `integrity_check` (string), `foreign_key_issues` (int), `invalid_related_rows` (int), `orphan_history_rows` (int), `rank_inversions` (int), `dependency_cycle` ([]string), `parent_cycle` ([]string), `unchecked` ([]string), `errors` ([]string), `warnings` ([]string).
  - An engine reports **zeros** for checks it has no analogue for rather than omitting them (`internal/storage/maintenance.go`).

---

### 1.20 Contract-package tests (`internal/storage/capabilities_test.go`)

- Fakes satisfy interfaces by embedding a nil interface, so a fake declares what it offers in one line and panics only if a test calls a method it never claimed (`internal/storage/capabilities_test.go`).
- `TestEngineOffersOnlyWhatItImplements`: for each of the seven capabilities, an engine offering only that one reports exactly one capability from `Offered`, and every other capability's `OfferedBy` returns false (`internal/storage/capabilities_test.go`).
- `TestAbsentCapabilityAnswersWithTypedAbsence`: an engine implementing only `Store` reports zero offered capabilities; all seven `.Of()` calls return `UnsupportedError` with the correct `Capability` name and a non-empty `Engine` (`internal/storage/capabilities_test.go`).
- `TestSyncWithoutReconcileIsRepresentable`: an engine offering `Syncer` but not `Reconciler` is representable; `Of` hands back the engine itself, not a wrapper, and the `GCMode` depth survives the crossing (`internal/storage/capabilities_test.go`).
- `TestOfferedFollowsCapabilitiesOrder`: an engine offering everything reports the same order as `Capabilities()` (`internal/storage/capabilities_test.go`).
- `TestCapabilitiesIsTheCallersOwnSlice`: mutating a returned slice does not change the enumeration (`internal/storage/capabilities_test.go`).
- `TestCapabilityNamesAreDistinctAndSpoken`: no capability has an empty name; no two share a name (`internal/storage/capabilities_test.go`).
- `TestEveryCapabilityIsEnumerated`: parses the package's own AST for `capability[...]` composite literals and asserts the declared names equal `Capabilities()` (`internal/storage/capabilities_test.go`).
- `TestGCModeValidAcceptsOnlyTheContractsDepths`: `GCNewGen` and `GCFull` are valid; `GCMode(-1)`, `GCMode(2)`, `GCMode(99)` are not (`internal/storage/capabilities_test.go`).

---

## PART 2 — `internal/storage/memory` (the in-memory backend)

### 2.1 Persistence: how it persists (short answer — it does not)

- The package implements the contract with nothing but Go values: **no SQL, no disk, no schema, no engine artifact of any kind** (`internal/storage/memory/doc.go`).
- `Close()` releases what the engine holds, which is nothing: its state is Go memory the garbage collector owns (`internal/storage/memory/engine.go`).
- **There is no on-disk format.** All state lives in the `Engine` struct's fields (`internal/storage/memory/engine.go`). Nothing in the package opens, reads, or writes a file.

### 2.2 `Engine` state layout (`internal/storage/memory/engine.go`)

| Field | Type | Role |
|---|---|---|
| `mu` | `sync.Mutex` | Makes the engine safe to share |
| `workspaceID` | `string` | Scopes the attribution stamp; taken at construction because a stream token with no workspace is a half-fact `model.NewAttribution` refuses to carry |
| `attribution` | `model.Attribution` | Current attribution stamp |
| `issues` | `map[string]*record` | The issue table |
| `order` | `[]string` | **THE** total rank order, top first. `Issue.Rank` is rendered from a position in this slice at read time and stored nowhere, so a rank that contradicts the order is unrepresentable — which is why nothing here needs the inversion repair Dolt offers |
| `relations` | `[]model.Relation` | Edge table, insertion order (oldest-first by construction) |
| `comments` | `[]model.Comment` | Comment table, insertion order |
| `events` | `[]model.IssueEvent` | History, insertion order |
| `labels` | `map[string][]model.Label` | Keyed by issue, kept **sorted by name** because the sorted set is what every label read returns |

**`record`** (`internal/storage/memory/engine.go`): `id`, `title`, `description`, `prompt`, `issueType model.IssueType`, `topic`, `assignee`, `lane`, `priority model.Priority`, `createdAt`, `updatedAt time.Time`, `status model.StatusView`, `retention model.Retention`.
- **Rank is deliberately absent** from `record`; position lives in `Engine.order`, and a rank string beside it would be a second representation of one fact (`internal/storage/memory/engine.go`).
- `status` is the leaf status view; a container's state derives from its children, so its view stays zero and hydration never reads it — the same place Dolt writes a NULL status column (`internal/storage/memory/engine.go`).

**`const createdBy = "links"`** (`internal/storage/memory/engine.go`) — the author every structural row the engine writes on a caller's behalf carries: the parent edge and initial labels a create wires, and the create event itself. It matches Dolt's spelling because it lands in exported history a user reads (`internal/storage/memory/engine.go`).

**`exportVersion = 2`** (`internal/storage/memory/export.go`) — the export schema version; it is the contract's current version, not the engine's, because a differing version number would make two identical stores compare unequal (`internal/storage/memory/export.go`).

### 2.3 Construction and concurrency

**`New(workspaceID string) (*Engine, error)`** (`internal/storage/memory/engine.go`)
- Trims `workspaceID`; empty (after trim) → `errors.New("workspace id is required")` (`internal/storage/memory/engine.go`).
- Initializes `issues` and `labels` maps; `order`, `relations`, `comments`, `events` start nil (`internal/storage/memory/engine.go`).
- The workspace id is required rather than defaulted because attribution is a complete pair or nothing (`internal/storage/memory/engine.go`).
- Tested: `New("")` and `New("   ")` both error (`internal/storage/memory/capabilities_test.go`).

**Concurrency discipline** (`internal/storage/memory/engine.go`):
- Every exported method locks the mutex and immediately delegates to an unexported one; no unexported method ever locks. That is what lets `BulkApply` drive `CreateIssue` and `Apply` for a whole batch under one hold without deadlocking on itself.
- The mutex is a plain `sync.Mutex` (not RWMutex), so reads serialize with writes.
- `Export` calls `sortEvents(cloneEvents(...))` rather than `ListAllEvents` precisely because the latter would deadlock on the non-reentrant mutex (`internal/storage/memory/export.go`).
- `TestConcurrentUseIsSerialized` runs 8 goroutines × 6 iterations of create/apply/list, then asserts `LocalIssueCount == 48`, that the listing holds the same count, and that no two issues share a `Rank` (`internal/storage/memory/engine_test.go`).

**Ownership**: the engine owns every byte of its state and hands none out; each read composes a fresh model value, so a caller holding a previously-read issue cannot mutate the engine through it (`internal/storage/memory/engine.go`). The one value that would have aliased is `model.IssueEvent.Changes`, which `cloneEvents` copies one level deeper than the slice (`internal/storage/memory/engine.go`).

**`AttributeTo(streamToken string)`** (`internal/storage/memory/engine.go`) — locks, then sets `e.attribution = model.NewAttribution(streamToken, e.workspaceID)`. An empty token leaves it unattributed rather than half-attributed (`internal/storage/memory/engine.go`).

**`e.now()`** returns `time.Now().UTC()` — the engine's clock, read at the write boundary (`internal/storage/memory/engine.go`).

### 2.4 Internal derivations

- **`mustRecord(id)`** — the one place "you named an issue that isn't here" is decided; returns `storage.NotFoundError{Entity: "issue", ID: id}` (`internal/storage/memory/engine.go`).
- **`positions()`** — derives an id→index map from `order` on every read rather than maintaining a cached index (`internal/storage/memory/engine.go`).
- **`rankAt(index int) string`** — renders a position as `fmt.Sprintf("%09d", index)`. Any encoding whose ascending string order matches the engine's order satisfies the contract; the width is fixed so the comparison stays lexicographic (`internal/storage/memory/engine.go`).
- **`hydrate(rec, pos)`** (`internal/storage/memory/engine.go`) — composes `model.Issue` from the record: `ID`, `Title`, `Description`, `Prompt`, `Priority`, `IssueType`, `Topic`, `Assignee`, `Rank: rankAt(pos[rec.id])`, `Lane`, `Labels: e.labelNames(rec.id)`, `CreatedAt`, `UpdatedAt`; then `SetRetention(rec.retention)`; then `model.HydrateRow(issue, rec.status, children)` with `children` = lifecycle children.
- **`lifecycleChildren`** (`internal/storage/memory/engine.go`) — a non-container returns nil; a container returns its rank-ordered children filtered by `visibleUnder`.
- **`visibleUnder(parent, child model.Retention) bool`** (`internal/storage/memory/engine.go`) — `model.Frozen(parent) || !model.Frozen(child)`. A live container shows only live children, so archiving a child removes it from the epic's progress; a container itself out of the flow keeps its whole child set (`internal/storage/memory/engine.go`).
- **`childRecords(parentID, pos)`** (`internal/storage/memory/engine.go`) — scans `relations` for `RelParentChild` edges with `DstID == parentID`, collects existing `SrcID` records, then `slices.SortStableFunc` by `pos[a.id] - pos[b.id]` (rank order).
- **`labelNames(issueID)`** (`internal/storage/memory/engine.go`) — returns the stored rows' names in stored (sorted-by-name) order.
- **`recordEvent(issueID, spec, now)`** (`internal/storage/memory/engine.go`) — the ONE place history is written. Sets `ID: "evt-" + uuid.NewString()`, `IssueID`, `Action: strings.TrimSpace(spec.action)`, `Reason: strings.TrimSpace(spec.reason)`, `Actor` (trimmed; empty → `"unknown"`), `CreatedAt: now`, `Attribution: e.attribution` (read off the engine here, not passed in), `Changes: spec.changes`.
- **`recordEvents`** writes every event a mutation owed, in order; a mutation that moved nothing owes none (`internal/storage/memory/engine.go`).

### 2.5 `CreateIssue` (`internal/storage/memory/issues.go`)

Order of checks is stated as contract: the parent must be resolved before the cosmetic prefix, so naming a missing parent reports the missing issue (`internal/storage/memory/issues.go`).

1. `title = strings.TrimSpace(in.Title)`; empty → `errors.New("title is required")`.
2. `canonicalLabels(in.Labels)` — normalize/dedupe/sort; error propagates.
3. `issueid.NormalizeTopicForCreate(in.Topic)` — error propagates.
4. `issueType`: if `in.IssueType == ""` → `model.TypeTask`.
5. `parentID = strings.TrimSpace(in.ParentID)`; if non-empty, `mustRecord(parentID)` → `NotFoundError` on miss.
6. `issueid.NormalizeConfiguredPrefix(in.Prefix)`; error → `fmt.Errorf("normalize issue prefix: %w", err)`.
7. `now := e.now()`; `mintID(...)`.
8. Builds the record with all string fields `strings.TrimSpace`'d (description, prompt, assignee, lane), `status: model.StatusView{Value: model.StateOpen}`, `retention: model.Live{}`.
9. `e.place(id, e.filingFrame(parentID), in.Placement)` — error propagates; then `e.issues[id] = rec`; `e.setLabels(id, labels, now, createdBy)`.
10. If `parentID != ""`, appends `model.Relation{SrcID: id, DstID: parentID, Type: model.RelParentChild, CreatedAt: now, CreatedBy: createdBy}`.
11. Records one `created` event, `reason: "issue created"`, `actor: createdBy`. Changes: a leaf records one `FieldChange{Field:"status", From:"", To:"open"}`; **a container records none**.
12. Returns `e.hydrate(rec, e.positions())`.

**`place(id, f, placement)`** (`internal/storage/memory/issues.go`) — `RankTop` takes the first slot among the frame `f`'s members; `RankBottom` (the zero value) appends to the whole order; any other value → `fmt.Errorf("unknown rank placement: %d", p)` from `orderEdgeFor`. A population with no members takes neither end: `place` takes the slot from `e.slotInsideContainer(f)`, propagates its error, and otherwise inserts there, so the first member of a frame lands immediately after the issue that frames it — `slices.Index(e.order, string(f))` plus one. `storage.TopLevel` names no such issue and gives slot `0`; a frame absent from the order is refused with `fmt.Errorf("frame %s holds no position in the order; an issue cannot be filed inside one that is not there", f)`.

**`mintID`** (`internal/storage/memory/issues.go`)
- With a parent: `nextChildID(parentID)`.
- Without: `baseLength = min(issueid.ComputeAdaptiveLength(topLevelCount()), issueid.MaxHashLength)`; then for `length` from `baseLength` to `issueid.MaxHashLength`, for `nonce` in `[0, issueid.NonceAttempts)`, generates `issueid.GenerateHashID(prefix, topic, title, description, createdBy, createdAt, length, nonce)` and returns the first candidate not already in `issues`.
- Exhaustion → `fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, issueid.MaxHashLength)`.

**`topLevelCount()`** counts ids not containing `"."` (`internal/storage/memory/issues.go`).

**`nextChildID(parentID)`** (`internal/storage/memory/issues.go`) — finds the highest integer suffix among **direct** children (a suffix containing another `.` is skipped, so grandchildren do not count), returns `fmt.Sprintf("%s.%d", parentID, highest+1)`.

### 2.6 Reads

**`GetIssue`** (`internal/storage/memory/issues.go`) — `mustRecord` then `hydrate`.

**`GetIssueDetail`** (`internal/storage/memory/issues.go`) — returns `model.IssueDetail` with:
- `Issue` — from `getIssue`
- `Relations` — `incidentRelations(id)`, insertion order
- `Comments` — `commentsFor(id)`, insertion order
- `Events` — `eventsFor(id)`, sorted (created_at, id)
- `Children`, `DependsOn`, `Blocks`, `Parent` — from `bucketRelations`
- `Siblings` — the parent's other children (same rank-ordered child set, minus self); an only child yields the empty group
- `Related` — `relatedIssues`
- `RedirectTarget` — hydrated from the issue's own close payload (`issue.RedirectTargetValue()`), **never** from the relations graph; nil if the target record is absent

**`ListTopics`** (`internal/storage/memory/issues.go`) — iterates `issues`; skips records whose retention is `model.Deleted` and records with an empty topic; dedupes; `slices.Sort`. **Deletion removes an issue's topic from the vocabulary; archival does not**.

**`ListAllEvents`** (`internal/storage/memory/issues.go`) — `sortEvents(cloneEvents(e.events))`. The append-only slice already holds true recording order, which is a better answer, and it is deliberately not the one given, because a same-tick tie is where two engines would part company.

**`sortEvents`** (`internal/storage/memory/issues.go`) — `slices.SortStableFunc` on `cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))`. It is the one place this engine orders history.

**`LocalIssueCount`** (`internal/storage/memory/issues.go`) — `int64(len(e.issues))`; counts what would be lost, including archived and deleted.

**`eventsFor(issueID)`** (`internal/storage/memory/issues.go`) — filters `events` by `IssueID`, then `sortEvents(cloneEvents(...))`.

### 2.7 `ListIssues` (`internal/storage/memory/list.go`)

Pipeline is fixed and every stage always runs: **hydrate → select → order → cap** (`internal/storage/memory/list.go`).

1. `issueOrdering(filter.SortBy)` — parsed first, so an unknown sort field errors before any work (`internal/storage/memory/list.go`).
2. `storage.ParseIssueCriteria(filter)` — canonicalizes the label criteria the same way stored labels are normalized, and is the one step of selection that can fail; everything after it answers yes or no.
3. `e.mustRecord(id)` for each of `filter.ParentIDs`, in order — the first id with no record returns `NotFoundError`; a deleted record still exists.
4. Hydrates **all** issues in `e.order` sequence.
5. `e.selects(issue, filter, criteria)` per issue.
6. `slices.SortStableFunc(selected, order)` — the ordering is total (every comparison ends in a distinct id), so the result does not depend on arrival order.
7. `capLimit(selected, filter.Limit)`.

**`selects`** — the composition, and only the half no issue can answer alone (`internal/storage/memory/list.go`): `criteria.Selects(issue)` first, then `ParentIDs` and `HasComments`, which are readings of the engine's own edge and comment tables.

| Criterion | Semantics | Cite |
|---|---|---|
| `ParentIDs` | `matchesParents(issue.ID, ParentIDs)`: empty = pass; else some `RelParentChild` relation has `SrcID == issue.ID` and `DstID` in the list; the parent's retention is not consulted | |
| `HasComments` | reject if `*HasComments != (len(commentsFor(issue.ID)) > 0)` | |

**`storage.IssueCriteria`** (`internal/storage/selects.go`) — a `ListIssuesFilter` reduced to the criteria readable from an issue alone, with the label canonicalization already taken, so `Selects` is total. It is exported because a caller narrowing rows it already holds applies the **same** rule storage defines rather than a second one of its own (the memory engine narrows with it directly; the SQL store expresses the same selection in its own query) — the workable pipeline reads the whole queue and `keepRows` narrows it at the point of use (`internal/cli/queue_facts.go`).

**`Selects`** — every criterion ANDs; every slice ORs within itself; the zero value selects everything **live**, archived and deleted issues being excluded unless the filter asks for them (`internal/storage/selects.go`):
| Criterion | Semantics | Cite |
|---|---|---|
| Retention | `model.Archived` excluded unless `IncludeArchived`; `model.Deleted` excluded unless `IncludeDeleted`; anything else (Live) always passes | |
| `Statuses` | `matchesStates`: empty = pass; otherwise matches if any `model.DefaultOpen(string(state)) == issue.State()` — compares the **DERIVED** state | |
| `Resolutions` | `matchesResolutions`: empty = pass; a nil `ResolutionValue()` matches **no** non-empty criteria set; otherwise `slices.Contains(wanted, *resolution)` | |
| `IssueTypes` | `matchesAny(string(issue.IssueType)...)`: empty = pass; else exact string membership | |
| `ExcludeIssueTypes` | if non-empty AND the type is in the list → reject | |
| `Assignees` | `matchesAny(issue.Assignee...)`: exact match after the criteria are trimmed | |
| `IDs` | `matchesAny(issue.ID...)`: exact match | |
| `UpdatedAfter` | reject if `issue.UpdatedAt.Before(*UpdatedAfter)` (i.e. inclusive of equality) | |
| `UpdatedBefore` | reject if `issue.UpdatedAt.After(*UpdatedBefore)` (inclusive of equality) | |
| `LabelsAll` | **conjunctive**: every canonical label criterion must be in `issue.Labels` | |
| `SearchTerms` | **conjunctive across terms**: every term must match | |

**`TrimmedNonEmpty`** (`internal/storage/selects.go`) — drops the blanks a caller may have assembled a criteria slice from, so a filter of nothing but whitespace constrains nothing rather than selecting nothing.

**`matchesSearch`** (`internal/storage/selects.go`) — lowercases and trims the term; an empty needle matches everything; case-insensitive substring across exactly four fields: `Title`, `Description`, `Prompt`, `Topic`.

**`capLimit`** (`internal/storage/memory/list.go`) — `limit <= 0` or `len <= limit` → unchanged; else `issues[:limit]`. **A limit of zero is the absence of a limit, not a limit of zero**; truncation, never sampling.

**`issueSortKeys`** (`internal/storage/memory/list.go`) — exactly ten entries, matching `storage.SortFields`:
| Key | Comparison |
|---|---|
| `id` | `strings.Compare(a.ID, b.ID)` |
| `title` | `strings.Compare(a.Title, b.Title)` |
| `status` | `strings.Compare(string(a.State()), string(b.State()))` — the **DERIVED** state, as the `Statuses` filter compares |
| `priority` | `cmp.Compare(a.Priority, b.Priority)` |
| `rank` | `strings.Compare(a.Rank, b.Rank)` |
| `type` | `strings.Compare(string(a.IssueType), string(b.IssueType))` |
| `topic` | `strings.Compare(a.Topic, b.Topic)` |
| `assignee` | `strings.Compare(a.Assignee, b.Assignee)` |
| `created_at` | `a.CreatedAt.Compare(b.CreatedAt)` |
| `updated_at` | `a.UpdatedAt.Compare(b.UpdatedAt)` |

**`status` ordering** — `strings.Compare(string(a.State()), string(b.State()))`. It compares the **derived** state, the same reading `matchesStates` filters on, so an epic orders by the state its children compute rather than by a stored field it does not have. There is no separate stored-status comparator.

**`issueOrdering`** (`internal/storage/memory/list.go`)
- No specs → `[]SortSpec{{Field: "rank"}}` — the canonical ordering expressed as the spec list it stands for.
- Each spec's field is `strings.ToLower(strings.TrimSpace(...))` then looked up in `issueSortKeys`; a miss → `fmt.Errorf("unsupported sort field %q", spec.Field)`.
- `Desc` negates the ascending comparator.
- **`strings.Compare(a.ID, b.ID)` ascending is appended as the final key always** — so descending reverses only the named keys, never the tie-break.
- The composed comparator returns the first non-zero result, else 0.

### 2.8 `Apply` (`internal/storage/memory/apply.go`)

**`apply(id, c)`** (`internal/storage/memory/apply.go`)
1. `mustRecord(id)` → `NotFoundError` on miss.
2. Hydrates `current`.
3. `actor = strings.TrimSpace(c.Actor)`; empty → `"unknown"`.
4. `now := e.now()`.
5. `planLifecycle(current, actor, strings.TrimSpace(c.Reason), c.Action, now)` → `(afterAction, actionEvents, err)`.
6. `planFields(afterAction, c.Fields, actor, now)` — **the field write baselines on the POST-action issue**, so a start's new assignee is what the patch diffs against rather than producing a second assignee change row.
7. `writeIssue`, `writeLabels`, `recordEvents(actionEvents)`, `recordEvents(patch.events)` — in that order.
8. Returns `hydrate(rec, e.positions())`.

**`planLifecycle`** (`internal/storage/memory/apply.go`) — a type switch on the sealed `model.Action` sum: `nil` → no transition (`current, nil, nil`); `model.StatusAction` → `planStatus`; `model.RetentionAction` → `planRetention`; `default` → **panics** with `fmt.Sprintf("illegal Action value %T", action)` (only an impostor `Action` reaches here).

**`planStatus`** (`internal/storage/memory/apply.go`)
- If `model.Frozen(current.Retention())` → `fmt.Errorf("cannot %s archived or deleted issue", action.Name().Verb())`.
- `current.Apply(action)` — the state machine's own rejections propagate.
- Assignee rule: `postAssignee = priorAssignee` except for `model.Start`, where it is `strings.TrimSpace(start.Assignee)`. Start is the one variant carrying a new owner.
- **No-op rule**: if `updated.StatusValue() == current.StatusValue() && postAssignee == priorAssignee` → return `(current, nil, nil)` with no event. A same-state start with a NEW assignee is the agent reclaim path and falls through, recording the ownership change.
- The no-op is decided BEFORE the redirect target is checked, because the check guards a write and there is no write here to guard.
- `validateRedirectTarget(updated)`.
- Sets `updated.UpdatedAt = now`; emits one event with `action: string(action.Name())`, the change's `reason`, the `actor`, and `statusChanges(current, updated)`.

**`planRetention`** (`internal/storage/memory/apply.go`)
- `model.Retain(current.Retention(), action, now)` — the legal moves and every rejection reason are the model's transition table.
- **There is no same-state success cell** — re-archiving an archived issue is a rejection, not a quiet no-op — so every planned retention move owes a write.
- Sets retention and `UpdatedAt = now`; emits one event with `retentionChanges(...)`.

**`validateRedirectTarget`** (`internal/storage/memory/apply.go`)
- Target nil and resolution non-nil and `resolution.RedirectsToCanonical()` → `fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)`.
- Target nil and not redirect-requiring → nil.
- `*target == closing.ID` → `fmt.Errorf("cannot redirect %s to itself", closing.ID)`.
- Target not present → `mustRecord`'s `NotFoundError`.
- Target's retention is `model.Deleted` → `fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted"...)`.
- **Archived stays legal**, because "duplicate of something already done" is the most common real redirect.

**`planFields`** (`internal/storage/memory/apply.go`) — a pure function of (baseline, patch, actor, now); no clock beyond the stamp handed in, no store, no writes.
- `Title` set → `strings.TrimSpace`; if the result is empty → `errors.New("title cannot be empty")`.
- `Description`, `Prompt`, `Assignee`, `Lane` set → `strings.TrimSpace`.
- `IssueType` set → **refused if it would cross the container/leaf line**: `fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())`.
- `Priority` set → assigned as-is.
- `Labels` set → `canonicalLabels(*in.Labels)`.
- `patch.statesLabels = (in.Labels != nil)` — **not** the same question as "did the labels change": a patch restating the existing set rewrites the label rows (authorship and timestamps included), while a patch never mentioning labels leaves them as an earlier writer left them.
- If `fieldChanges(baseline, issue)` is empty → returns the patch with **no event and no `UpdatedAt` bump**.
- Otherwise sets `patch.issue.UpdatedAt = now` and emits one event with `reason: in.Reason`, `actor`, and the changes. **The field-change event has an empty `action` string**.

**`fieldChanges`** (`internal/storage/memory/apply.go`) — one row per field that actually moved, in this fixed order: `title`, `description`, `issue_type`, `priority`, `assignee`, `lane`, `labels`.
- `priority` is recorded as `strconv.Itoa(int(...))` — the numeric wire encoding, not the display name.
- `labels` is recorded as `strings.Join(labels, ",")`.
- Note: `prompt` and `topic` are NOT in `fieldChanges`, so a prompt-only edit produces a patch with no event.

**`statusChanges`** (`internal/storage/memory/apply.go`) — rows, in order, for: `status` (when `StatusValue()` moved), `closed_at` (RFC3339Nano, `""` when absent), `resolution` (`""` when absent), `redirect_target` (`""` when absent), `assignee`.

**`retentionChanges`** (`internal/storage/memory/apply.go`) — projects both retentions through `model.RetentionTimestamps` and emits `archived_at` and/or `deleted_at` rows (RFC3339Nano) for whichever moved — the same encoding an export carries.

**`writeIssue`** (`internal/storage/memory/apply.go`) — the one place a mutation becomes stored state; copies `title`, `description`, `prompt`, `issueType`, `priority`, `assignee`, `lane`, `updatedAt`, `retention`, `status = statusViewOf(issue)`. **`topic`, `id`, and `createdAt` are never written here** — topic is immutable through `Apply`.

**`writeLabels`** (`internal/storage/memory/apply.go`) — replaces the label set when and only when `patch.statesLabels`.

**`statusViewOf`** (`internal/storage/memory/apply.go`) — a container projects to the **zero** `model.StatusView` (the same nothing Dolt stores as a NULL column); a leaf projects `{Value: issue.State(), ClosedAt, Resolution, RedirectTarget}`.

**Optional-value helpers** (`internal/storage/memory/compare.go`) — shared contract: two absent values are equal, absence renders as the empty string, and absence is never conflated with a present zero.
- `timesEqual`, `resolutionsEqual`, `stringsEqual`.
- `formatTime` → `""` for nil, else `time.RFC3339Nano`; `formatResolution` → `""` for nil; `formatString` → `""` for nil.

### 2.9 Comments (`internal/storage/memory/edges.go`)

**`AddComment`**
- `getIssue(in.IssueID)` first → missing issue is `NotFoundError`.
- `body = strings.TrimSpace(in.Body)`; empty → `errors.New("comment body is required")`.
- Comment: `ID: "cmt-" + uuid.NewString()`, `IssueID`, trimmed `Body`, `CreatedAt: e.now()`, `CreatedBy: authorOr(in.CreatedBy)`.
- Appends and returns `(comment, issue, nil)` — the issue read is the caller's answer too, since a comment never changes the issue row.
- **`AddComment` records no history event.**

**`DeleteComment`**
- `id = strings.TrimSpace(commentID)`; empty → `errors.New("comment id is required")`.
- Not found → `storage.NotFoundError{Entity: "comment", ID: id}`.
- Deletes and returns the removed comment.

**`commentsFor(issueID)`** — filters `comments` by `IssueID`, insertion order, returns a non-nil empty slice when there are none.

### 2.10 Labels (`internal/storage/memory/edges.go`)

**`AddLabel`**
- `mustRecord(in.IssueID)` → `NotFoundError`.
- `model.NormalizeLabel(in.Name)` → error propagates.
- If the name is not already present, appends a `model.Label{IssueID, Name, CreatedAt: e.now(), CreatedBy: authorOr(in.CreatedBy)}` and re-sorts by name.
- **Adding the same label twice is not an error**: the caller asked for the label to be there, and it is; the row keeps the authorship the first add gave it.
- Returns the whole resulting set.

**`RemoveLabel`**
- `mustRecord(issueID)` → `NotFoundError`.
- `model.NormalizeLabel(labelName)` → error propagates.
- Absent → `storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, name)}`.
- Otherwise deletes and returns the resulting set.

**`ReplaceLabels`**
- `mustRecord(issueID)`; `canonicalLabels(labels)`; `setLabels(issueID, canonical, e.now(), createdBy)`; returns nil.
- **Rewrites every row's `CreatedAt` and `CreatedBy`.**

**`ListLabels`** — returns `labelNames(issueID)` **without** a `mustRecord` check, so an unknown issue id returns an empty slice and no error.

**`setLabels`** — the one label write, shared by create and replace; builds fresh rows with one author (`authorOr(createdBy)`) and one timestamp.

**`canonicalLabels`** — normalizes each name via `model.NormalizeLabel` (error propagates), collapses duplicates, then `slices.Sort`. It is a parser: nothing downstream re-normalizes.

**`authorOr(createdBy)`** — trimmed non-empty value, else `"unknown"`.

### 2.11 Relations (`internal/storage/memory/edges.go`)

**`addRelation`**
1. `RelRelatedTo` with `SrcID == DstID` → `errors.New("related-to cannot target itself")`.
2. `in.Type.CanonicalEndpoints(in.SrcID, in.DstID)` normalizes endpoint order.
3. `mustRecord(srcID)` then `mustRecord(dstID)` → `NotFoundError`.
4. For `RelBlocks`, `rejectBlocksCycle(srcID, dstID)`.
5. Builds `model.Relation{SrcID, DstID, Type, CreatedAt: e.now(), CreatedBy: authorOr(in.CreatedBy)}`.
6. If `in.Type.SingleValuedFromSrc()`, drops every existing edge with the same `SrcID` and `Type` — cardinality is read off the type, not off which method the caller reached for.
7. If an identical `(src, dst, type)` edge still exists → `fmt.Errorf("relation %s->%s (%s) already exists"...)`.
8. Appends and returns.

**`rejectBlocksCycle(dependent, dependency)`**
- Self-edge → `fmt.Errorf("blocks: %s cannot block itself", dependent)`.
- Builds `precedes` = dependency → dependents from all `RelBlocks` edges.
- DFS from `dependent`; if `dependency` is reachable → long error: `"blocks: cannot add %s depends-on %s — %s already depends on %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order"`.
- Rationale: a rank order is a total order and one honoring every blocks edge exists exactly when there is no cycle.

**`RemoveRelation`**
- Canonicalizes endpoints, drops matching edges; **removed == 0** → `storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}`.

**`ListRelationsForIssue`**
- `mustRecord(issueID)` → `NotFoundError`.
- Returns `incidentRelations(issueID)` (either direction, write order) filtered by the variadic types; **naming no type means every type, never no types**.

**`GetRelationsByIDs`**
- Deduplicates the input ids.
- An id with no record is **simply absent from the map**, not an error.
- For each present id: hydrates the issue, buckets its incident relations, sets `bucketed.Issue = issue`.
- `nil` input yields an empty (non-nil) map.

**`SetParent`**
- Blank child or parent (after trim) → `errors.New("child and parent ids are required")`.
- `ChildID == ParentID` → `errors.New("child and parent cannot be the same issue")`.
- Delegates to `addRelation` with `Type: model.RelParentChild` — one validated caller of the single-valued write, so reparenting replaces in one act.

**`ClearParent`**
- `mustRecord(childID)` → `NotFoundError`.
- Drops every `RelParentChild` edge with `SrcID == childID`; **removed == 0** → `storage.NotFoundError{Entity: "parent relation", ID: childID}`.

**`incidentRelations`** — every edge touching the id in either direction, in write order.

**`bucketRelations(focalID, relations, pos)`** — the single definition of edge → bucket mapping:
| Condition | Bucket | Counterpart |
|---|---|---|
| `Type == RelBlocks && SrcID == focalID` | `DependsOn` | `DstID` |
| `Type == RelBlocks && DstID == focalID` | `Blocks` | `SrcID` |
| `Type == RelParentChild && DstID == focalID` | `Children` | `SrcID` |
| `Type == RelParentChild && SrcID == focalID` | `Parent` (pointer) | `DstID` |
| anything else (e.g. `RelRelatedTo`) | skipped | — |
- An edge whose counterpart record has vanished is simply not in the result.
- `Children`, `DependsOn`, `Blocks` are each sorted by rank position; the returned struct initializes them to non-nil empty slices.

**`relatedIssues`** — `RelRelatedTo` counterparts only (the other end of the edge), hydrated, rank-sorted. It is `GetIssueDetail`'s concern alone; peer links stay out of the shared `IssueRelations` shape.

**`sortByRank`** — `slices.SortStableFunc` on `pos[a.ID] - pos[b.ID]`.

### 2.12 Rank (`internal/storage/memory/rank.go`)

Because the order is a slice, an intent is literally what it says: "above Y" removes the issue and puts it back immediately before Y. **No fractional key, no midpoint, no inversion to repair** (`internal/storage/memory/rank.go`).

**`side`** — `above side = 0`, `below side = 1`; a value the one relative-rank path takes rather than two paths.

**`rankRelative(issueID, targetID, at)`**
- `resolveRankPair`; `detach(move.MovedID)`; `anchor := slices.Index(e.order, move.AnchorID)`; `insertAt(anchor + int(at), move.MovedID)`; returns the move.

**`RankToTop` / `RankToBottom`** — `rankToEnd(issueID, storage.RankTop|RankBottom)`: `mustRecord` (so a missing id is `NotFoundError`), `detach`, `place`. **They need no anchor and no frame: every issue is comparable with the ends**.

**`RankSet(ids)`**
- `len(ids) < 2` → `errors.New("rank set: need at least 2 IDs to establish order")`.
- Empty id → `errors.New("rank set: empty ID in input")`.
- Duplicate id → `fmt.Errorf("rank set: duplicate ID %q in input", id)`.
- Builds an ancestor chain per id (missing id → `NotFoundError` from `ancestorChain`).
- `frameRepresentatives(chains)`; error wrapped as `fmt.Errorf("rank set: %w", err)`.
- **Two named ids collapsing onto one representative is refused**: `"rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead"`.
- Detaches every representative and **prepends the representatives to the head of the order** — the named issues are stacked at the TOP, in the order named.
- Returns one `RankSetResolution{NamedID, RankedID}` per input, in input order.

**`detach`** — `slices.DeleteFunc` on id equality.
**`insertAt`** — `slices.Insert`; **clamps nothing**, deliberately: a clamp would turn a resolution bug into a silent placement at the top of the backlog.

**`resolveRankPair(issueID, targetID)`** — both relative verbs route through this one resolution, so cross-frame semantics cannot drift between above and below.
- `issueID == targetID` → `errors.New("cannot rank an issue relative to itself")`.
- `mustRecord(targetID)` **before** `mustRecord(issueID)`.
- Builds both ancestor chains, calls `frameRepresentatives`.
- A `*frameContainmentError` is re-worded by which side contains which: if `containment.containerID == issueID` → `"cannot rank %s relative to %s: %s contains it; rank it against a sibling instead"`; else → `"cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead"`.
- Returns `RankMove{MovedID: reps[0], AnchorID: reps[1]}`.

**`ancestorChain(id)`** — self first, root last, following only parents that are still there. Missing id → `NotFoundError`. A parent cycle → `fmt.Errorf("ancestor chain of %s: parent cycle at %s", id, parent)`.

**`parentOf(childID)`** — the first `RelParentChild` edge whose `SrcID == childID` and whose parent record exists and is **not** `model.Deleted`. A deleted parent is skipped: work in the trash frames nothing.

**`frameContainmentError{containerID, containedID}`** — `Error()` renders `"%s is inside %s; no comparable frame contains both — rank it against a sibling instead"`.

**`frameRepresentatives(chains)`**
- Builds an id→depth map per chain.
- If any chain's head appears at depth > 0 in another chain, returns `&frameContainmentError{containerID: chain[0], containedID: chains[j][0]}`.
- Finds the lowest common ancestor as the first element of chain 0 present in all others (common ancestors form a shared suffix of every chain).
- With no common ancestor (`lowestCommon == ""`), each chain's representative is its **root** — the top level is the frame that contains everything.
- Otherwise the representative is the element **one level below** the LCA in that chain: `chain[depths[i][lowestCommon]-1]`.
- Nothing inside any epic is reordered by a cross-frame request.

### 2.13 Bulk / import (`internal/storage/memory/bulk.go`)

**`BulkApply(ctx, prefix, actor, specs)`**
1. `validateBulkSpecs(specs)` — the entire file is gated before any document is applied.
2. `creationOrder(bulkGraph(specs))`; a cycle → `fmt.Errorf("bulk: %w", err)`.
3. Walks documents in topological order:
   - `spec.ID != ""` (update): `bulkUpdateChange(spec, actor)` then `e.apply(spec.ID, change)`. On apply failure → `e.compensate(batch, fmt.Errorf("bulk: update %q: %w", spec.ID, err))`. Appends `issue.ID` to `result.Updated`.
   - Otherwise (create): `bulkCreateInput(spec, prefix, batch)` then `e.createIssue(in)`. On failure → `e.compensate(batch, fmt.Errorf("bulk: create doc %d: %w", index, err))`. Records into the batch, and into `result.Created` under `spec.LocalID` if set, else under the new real id.
   - Note: `bulkUpdateChange` and `bulkCreateInput` errors are returned **without** compensation.
4. **Second pass, in original spec order (not topological)**: wires every `DependsOn` edge as `blocks` with `src = the document's own issue`, `dst = batch.resolve(dep)`. On failure → `e.compensate(batch, fmt.Errorf("bulk: depends_on doc %d -> %q: %w", index, dep, err))`.

**`ImportTree(ctx, prefix, specs)`**
1. `validateImportSpecs(specs)`.
2. `creationOrder(importGraph(specs))`; cycle → `fmt.Errorf("import: %w", err)`.
3. Per spec in topological order: re-parses `IssueType` and `Priority` (errors → `fmt.Errorf("import: spec %q: %w", spec.LocalID, err)`, **without** compensation), then `createIssue` with `ParentID: batch.resolveLocal(spec.Parent)` and `Prefix: prefix`. Create failure → `e.compensate(batch, fmt.Errorf("import: create %q: %w", spec.LocalID, err))`.
   - Note: `Lane` and `Placement` are not carried from `ImportTreeSpec` (it has no such fields).
4. Second pass in spec order wires `DependsOn`.
5. Returns `ImportTreeResult{IDMap: batch.idMap()}` — the `byLocalID` map.

**`wireDependency(dependent, dependency)`** — `addRelation` with `Type: model.RelBlocks`, `CreatedBy: createdBy` (`"links"`).

**`compensate(batch, cause)`**
- For each created id, applies `storage.Change{Action: model.Delete{}, Actor: createdBy, Reason: "import rollback"}`; ids whose delete failed are collected as "leaked".
- Returns `fmt.Errorf("%w (rollback leaked %d: %s)", cause, len(leaked), strings.Join(leaked, ","))` — the original failure travels unchanged; the rollback only adds an account of what is left behind.
- **Compensation is a soft delete, not a removal**: the issues remain in the store with `Deleted` retention (hence invisible to a default listing and to `ListTopics`, but counted by `LocalIssueCount`).
- **Updates already applied are not reverted.**

**`batchIDs`** — `byIndex []string`, `byLocalID map[string]string`, `createdIDs []string` (oldest first, the compensation order's input).
- `resolve(ref)` — returns the local mapping if present, otherwise **passes the reference through unchanged**; a reference matching nothing local is a real, pre-existing id, and the write that receives it decides whether it is real.
- `resolveLocal(ref)` — plain map lookup; an unresolved reference yields `""`, which the create path reads as "no parent". Validation, not this lookup, is what makes it total.

**`bulkCreateInput`**
- `model.ParseIssueType(*spec.IssueType)` → error wrapped `fmt.Errorf("bulk: %w", err)`.
- Priority defaults to `model.PriorityNormal` when the doc sets none; else `model.ParsePriority(*spec.Priority)`.
- Title and Topic are `strings.TrimSpace`'d; Description/Prompt/Assignee/Lane/Labels come from `valueOr(ptr, zero)`.
- **`Placement` is left at its zero value**, so a batch keeps its file order in the ranked order, whichever authored format the file is.

**`bulkUpdateChange`**
- Builds `UpdateIssueInput` with `Reason: strings.TrimSpace(spec.Reason)`.
- `Title`, `Description`, `Prompt`, `Assignee`, `Lane` go through `trimmedPointer` (nil stays nil; otherwise a pointer to the trimmed value).
- `Labels` is passed through as the raw `*[]string`.
- `IssueType` and `Priority` are parsed into pointers; parse errors → `fmt.Errorf("bulk: update %q: %w", spec.ID, err)`.
- Returns `storage.Change{Actor: actor, Fields: fields}` — **an update document never carries an Action**.

**`creationOrder(graph)`** — DFS topological sort over intra-batch references only. States `unvisited`/`visiting`/`done`; re-entering `visiting` → `fmt.Errorf("cycle detected involving %q", graph.localID[i])`. A reference matching no local name is not an edge. Both authored formats reduce to the same `localGraph` shape.

**`validateBulkSpecs`**
- Empty input → `errors.New("bulk: no issues in input")`.
- Per doc: `id`, `local_id`, `parent` must have no surrounding whitespace → `fmt.Errorf("bulk: doc %d %s %q has surrounding whitespace"...)`.
- Each `depends_on` entry: no surrounding whitespace; and if `LocalID != "" && dep == spec.LocalID` → `fmt.Errorf("bulk: doc %d (local_id %q) cannot depend on itself"...)`.
- `ID != ""` → `validateBulkUpdate`; duplicate `ID` → `fmt.Errorf("bulk: duplicate id %q", spec.ID)`.
- Else → `validateBulkCreate`; duplicate non-empty `LocalID` → `fmt.Errorf("bulk: duplicate local_id %q", spec.LocalID)`.

**`validateBulkCreate`**
- Missing/blank `Title` → `"bulk: doc %d missing title"`.
- Missing/blank `Topic` → `"bulk: doc %d missing topic"`.
- Missing `IssueType` → `"bulk: doc %d missing type"`.
- Unparseable type → `"bulk: doc %d has invalid type %q"`.
- Unparseable priority (when set) → `"bulk: doc %d has invalid priority %d"`.
- `Reason != ""` on a create → `"bulk: doc %d sets reason without id (reason only applies to updates)"`.

**`validateBulkUpdate`** — refuses the fields an update document has no business setting; each is somebody else's verb:
- `LocalID != ""` → `"bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets"`.
- `Topic != nil` → `"... sets topic; topic is immutable and update cannot change it"`.
- `Parent != ""` → `"... sets parent; reparent with \`lit parent set\` instead"`.
- `len(DependsOn) > 0` → `"... sets depends_on; wire dependencies with \`lit dep add\` instead"`.
- Invalid type / priority → `"... has invalid type %q"` / `"... has invalid priority %d"`.
- No updatable field stated → `"bulk: doc %d (id %q) has no fields to update"`. Updatable fields are exactly: `Title`, `Description`, `Prompt`, `IssueType`, `Priority`, `Assignee`, `Labels`, `Lane`.

**`validateImportSpecs`**
- Empty → `errors.New("import: no issues in input")`.
- Blank `LocalID` → `"import: spec %d missing local_id"`; untrimmed → `"import: spec %d local_id %q has surrounding whitespace"`.
- Blank `Title` → `"import: spec %q missing title"`.
- Invalid type → `"import: spec %q has invalid type %q"`; invalid priority → `"import: spec %q has invalid priority %d"`.
- Duplicate `LocalID` → `"import: duplicate local_id %q"`.
- Second loop: `Parent` untrimmed → error; **`Parent` must name a spec in the file** → `"import: spec %q references missing parent %q"`.
- Each `DependsOn` entry: untrimmed → error; self-reference → `"import: spec %q cannot depend on itself"`; **must name a spec in the file** → `"import: spec %q references missing depends_on %q"`.
- So: **`ImportTree` references must all resolve inside the file; `BulkApply` references may resolve to pre-existing real ids.**

### 2.14 `Export` (`internal/storage/memory/export.go`)

- Locks, then builds issues via `listIssues(ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})` — **the WHOLE store**, out-of-flow work included, because an export honoring the listing default would silently drop exactly the rows a diff exists to notice.
- Labels: flattened from the `labels` map, then sorted by `(IssueID, Name)` via `cmpThen`.
- Returns `model.Export{Version: exportVersion (2), WorkspaceID: e.workspaceID, ExportedAt: e.now(), Issues, Relations: slices.Clone(e.relations), Comments: slices.Clone(e.comments), Labels, Events: sortEvents(cloneEvents(e.events))}`.
- `Relations` and `Comments` come back in **write order** (cloned, unsorted); `Issues` in rank order; `Labels` in `(issue, name)` order; `Events` in `(created_at, id)` order.
- Every collection comes back in a total order so two stores holding the same facts serialize to the same bytes rather than to the same multiset.

### 2.15 Capabilities offered by the memory engine

- **None of the seven.** No remote to sync with, no divergence to reconcile, no history to check point, no faults of its own making to repair, no schema to migrate, no engine-native language for a raw statement. `storage.Offered` reports the empty set (`internal/storage/memory/doc.go`).
- Tested: `Offered(engine)` is empty; every `Capability.OfferedBy` is false; `storage.Sync.Of(engine)` returns an `UnsupportedError` with `Capability == "sync"` and a non-empty `Engine` (`internal/storage/memory/capabilities_test.go`).
- `var _ storage.Store = (*Engine)(nil)` — a contract method added or a signature moved stops the engine compiling (`internal/storage/memory/engine.go`).

### 2.16 Behaviors the memory engine deliberately copies from Dolt rather than improving

Stated in `internal/storage/memory/doc.go`:
1. Ordering a listing by `"status"` sorts the **stored** status encoding; a container stores none, so it orders ahead of every leaf ascending whatever state it derives to, while the status FILTER reads derived state. Correcting the disagreement is `links-store-seam-q35v.6` (`internal/storage/memory/doc.go`).
2. History comes back ordered by `(created_at, id)` rather than by recording order. Event ids are random, so on a coarse clock both engines can hand back a title change ahead of the creation that preceded it (`internal/storage/memory/doc.go`).
- Nothing is shared with the Dolt engine — not the field-patch diff, the transition planner, the frame resolution, or the compensating bulk apply — deliberately, because two engines calling one implementation would destroy the proof the conformance suite provides (`internal/storage/memory/doc.go`).

---

## PART 3 — `internal/storage/conformance` (what the suite requires of any backend)

### 3.1 Harness

- `NewEngine func(t *testing.T) storage.Store` — mints a fresh, empty engine for one case, already registered for cleanup. Every case gets its own store (`internal/storage/conformance/conformance.go`).
- `Run(t, newEngine)` walks the `cases` table, running each as a subtest with `context.Background()` and a fresh engine (`internal/storage/conformance/conformance.go`).
- `engineCase{name string; run func(t, ctx, st)}` — the suite is a table walked by one loop; adding a statement is adding data (`internal/storage/conformance/conformance.go`).
- `const prefix = "conf"` — every case creates under this cosmetic prefix; **cases assert on ids only by comparing ids the engine returned, never by predicting their shape** (`internal/storage/conformance/conformance.go`).
- `mustCreate` forcibly sets `in.Prefix = prefix` on every create (`internal/storage/conformance/conformance.go`).
- Rule for what may be asserted: only what a caller can observe through the contract; no case may reach past the interface into engine internals (`internal/storage/conformance/conformance.go`).
- Dolt's behavior is the tiebreak where a behavior was ambiguous; where the second engine answered better, it was moved to match rather than the contract moved to meet it (`internal/storage/conformance/conformance.go`).

### 3.2 The 36 registered cases (`internal/storage/conformance/conformance.go`)

`create_read_roundtrip`, `create_defaults`, `create_requires_title`, `create_normalizes_topic`, `create_under_missing_parent_is_not_found`, `get_missing_issue_is_not_found`, `apply_field_patch`, `apply_status_transition`, `apply_missing_issue_is_not_found`, `apply_to_container_is_refused`, `container_state_follows_live_children`, `history_records_mutations`, `list_defaults_to_rank_order`, `list_filters_select`, `list_by_parent`, `list_hides_archived_and_deleted`, `list_sorts_and_limits`, `list_breaks_sort_ties_by_id`, `list_accepts_exactly_the_contract_sort_fields`, `list_sorts_status_by_stored_encoding`, `events_are_totally_ordered`, `rank_intents_reorder`, `rank_intents_resolve_across_frames`, `rank_set_imposes_order`, `close_redirects_to_a_canonical`, `comments_roundtrip`, `labels_roundtrip`, `relations_roundtrip`, `relations_batch_buckets_edges`, `parent_wiring`, `topics_derive_from_issues`, `export_carries_whole_store`, `bulk_apply_creates_and_updates`, `bulk_apply_compensates_a_failed_batch`, `import_tree_maps_local_ids`, `attribution_stamps_events`, `local_issue_count_tracks_creates`.

### 3.3 Every enforced invariant, by case

**`create_read_roundtrip`**
- Surrounding whitespace is stripped on the way in for `Title`, `Description`, `Prompt`.
- A `GetIssue` read returns the *same* record, field for field: `ID`, `Title`, `Description`, `Prompt`, `Topic`, `Assignee`, `Lane`, `IssueType`, `State()`.
- A create with `IssueType: model.TypeBug` reads back `bug`, and `State()` is `open`.
- `Priority` survives as `PriorityUrgent`.
- `Labels: []string{"perf"}` reads back as `["perf"]`.
- `Rank` is non-empty — every issue must land somewhere in the order.

**`create_defaults`**
- Unspecified `IssueType` → `model.TypeTask`.
- New issue's `State()` is `open`.
- Unspecified `Priority` → `model.PriorityNormal`.
- Default placement **appends**: a second create files below the first.
- `Placement: storage.RankTop` leads the whole order.

**`create_requires_title`**
- Both `""` and `"   "` are rejected; the trim happens before the requirement.

**`create_normalizes_topic`**
- `"  Renderer Cleanup  "` is stored as `"renderer-cleanup"`.
- `ListTopics` then returns exactly `["renderer-cleanup"]`.
- These topics are refused at create: `""`, `"   "`, `"ab"` (too short), `"-!-"`.

**`create_under_missing_parent_is_not_found`**
- Creating with `ParentID: "no-such-issue"` returns `NotFoundError` with `Entity == "issue"`.

**`get_missing_issue_is_not_found`**
- Both `GetIssue` and `GetIssueDetail` on a missing id return `NotFoundError{Entity: "issue"}`.

**`apply_field_patch`**
- A `Change` with `Fields{Title, Priority, Reason}` returns the updated values.
- A field the patch never mentions (`Assignee`) is untouched — the whole reason the patch is pointers.
- The patch persists: a subsequent `GetIssue` shows the new title.

**`apply_status_transition`**
- `model.Start{Assignee: "ada"}` → `State() == in_progress` and `Assignee == "ada"`; Start is the one action that rewrites ownership.
- `model.Done{}` → `State() == closed`.
- `model.Reopen{}` → `State() == open`.

**`apply_missing_issue_is_not_found`**
- `Apply` on a missing id returns `NotFoundError{Entity: "issue"}`.

**`apply_to_container_is_refused`**
- Applying `model.Start` to an epic with children returns a `model.ContainerActionError` whose `.ID` is the epic's id.

**`container_state_follows_live_children`**
- Epic with two children, one `Done` → epic derives `in_progress`.
- Archiving the unfinished child takes it out of the epic's reading → epic derives `closed`.
- Archiving the epic itself freezes its reading: every child counts again, so the epic reverts to `in_progress` — the state it had when it left.

**`history_records_mutations`**
- A pure no-op `Apply` (no action, no fields) writes **no** events.
- A status action whose target state AND resulting assignee already hold is the same no-op: a repeated `Start{Assignee:"ada"}` writes no event.
- A same-state start naming a NEW owner (the reclaim path) **does** record, and the assignee becomes the new owner.
- Every event's `IssueID` matches the issue mutated.
- A field write records a change row with `Field == "title"`.
- Events are oldest-first by `CreatedAt`.

**`list_defaults_to_rank_order`**
- Three creates come back in creation order — an unsorted listing is rank ascending with ties broken by id, and `lit backlog` is this order.

**`list_filters_select`** — each filter is asserted to select exactly the listed ids:
| Filter | Expected |
|---|---|
| `Statuses: [in_progress]` | the started task |
| `IssueTypes: [bug]` | the bug |
| `ExcludeIssueTypes: [bug]` | the task |
| `Assignees: ["grace"]` | the task |
| `IDs: [bug.ID]` | the bug |
| `SearchTerms: ["widget"]` | the bug — matches title |
| `SearchTerms: ["parser"]` | the task — **search matches topic too** |
| `LabelsAll: ["perf","ui"]` | the bug — conjunctive |
| `LabelsAll: ["perf","absent"]` | nothing — a label the issue lacks excludes it |
| `HasComments: &true` | the commented bug |
| `UpdatedBefore: now+1h` | both |
| `UpdatedAfter: now+1h` | nothing |
| `UpdatedAfter: now-1h` | both |
| `IssueTypes:[bug] + Assignees:["grace"]` | nothing — **criteria AND across axes**, so no caller can widen a listing by adding a criterion |
| `Assignees: ["ada","grace"]` | both — **a slice ORs within itself** |

**`list_by_parent`** — fixture: two epics, two children under the first, a grandchild under the second child, a cousin under the other epic, and one parentless issue; the first child is started:
| Filter | Expected |
|---|---|
| `ParentIDs: [epic]` | the two children, in rank order — not the grandchild |
| `ParentIDs: [second]` | the grandchild |
| `ParentIDs: [cousin]` | nothing |
| `ParentIDs: [epic, other]` | both children and the cousin — **a slice ORs within itself** |
| `ParentIDs: [epic]` + `Statuses: [in_progress]` | the started child — criteria AND across axes |

`ParentIDs: [epic, "no-such-issue"]` → `NotFoundError{Entity: "issue"}`, not an empty listing.

**`list_hides_archived_and_deleted`**
- Default listing shows only live issues.
- `IncludeArchived: true` → live + archived.
- `IncludeDeleted: true` → live + deleted.
- Both → all three.
- Expected order in each case is creation order (live, archived, deleted), i.e. rank order is preserved across the retention filter.

**`list_sorts_and_limits`**
- `SortBy: [{title}]` ascending, `{title, Desc:true}` descending.
- `Limit: 2` returns the **head** of the ordered result, not a sample.
- `Limit: 0` is the absence of a limit, not a limit of zero.
- Sorting by an unknown field (`"nonsense"`) is an error.

**`list_breaks_sort_ties_by_id`**
- Three issues sharing one title: the result is ordered by id ascending.
- Descending on the named key leaves the id tie-break **ascending**.

**`list_accepts_exactly_the_contract_sort_fields`**
- **Every** field in `storage.SortFields` must be accepted.
- These six must be **rejected**: `"description"`, `"lane"`, `"labels"`, `"issue_type"`, `"item_rank"`, `"state"` — real model fields the contract omits, plus the storage column names an engine binding its own schema would reach for.
- The case self-guards: if any of those six is ever added to `SortFields`, the test fatals telling the author to move it.

**`list_sorts_status_by_stored_encoding`**
- An epic whose only child is in progress derives `in_progress`.
- Ascending by `status`: **the epic leads** — its absent stored status is the low key, even though `"in_progress"` would not sort before `"in_progress"`.
- Descending by `status`: the epic trails.
- The case pins the wrong answer on purpose; deleting it is the first step of `links-store-seam-q35v.6`, not a cleanup.

**`events_are_totally_ordered`**
- After a create plus three title changes, `ListAllEvents` returns at least 4 events.
- The sequence never steps backwards under `cmp.Or(CreatedAt.Compare, strings.Compare(ID))` — the property that holds tie or no tie.

**`rank_intents_reorder`**
- Three creates → order a, b, c.
- `RankAbove(c, a)` → order c, a, b; the returned `RankMove` is the inputs unchanged for frame-mates.
- `RankBelow(c, b)` → order a, b, c.
- `RankToTop(b)` → order b, a, c.
- `RankToBottom(b)` → order a, c, b.
- `RankAbove` with a missing anchor is an error; `RankToTop` of a missing issue is an error.

**`rank_intents_resolve_across_frames`**
- `RankAbove(child_of_epic, standalone)` succeeds and reports `MovedID == epic.ID`, `AnchorID == standalone.ID`.
- Nothing inside the epic is reordered, and the epic precedes the standalone in the listing.
- `RankAbove(child, its own epic)` is an error.
- `RankBelow(epic, its own child)` is an error.

**`rank_set_imposes_order`**
- `RankSet([c, a, b])` yields listing order c, a, b.
- Exactly one resolution per named id, in the order named, with `NamedID == RankedID` for frame-mates.

**`close_redirects_to_a_canonical`**
- `Close{Outcome: Duplicate{Of: canonical}}` → `State() == closed`, `ResolutionValue() == ResolutionDuplicate`, `RedirectTargetValue() == canonical.ID`, `ClosedAtValue() != nil`.
- `Reopen{}` clears the **whole** close payload together: resolution, redirect target, and closed-at all become nil.
- Closing as a duplicate of a missing issue → `NotFoundError{Entity: "issue"}`.
- Closing an issue as a duplicate of **itself** is an error.

**`comments_roundtrip`**
- `AddComment` returns the comment as written (`Body`, `CreatedBy`, `IssueID`).
- The second return is the issue as it stands after the write.
- `GetIssueDetail.Comments` holds exactly the added comment.
- `DeleteComment` returns the removed comment (id and body).
- After delete, `GetIssueDetail.Comments` is empty.
- `DeleteComment("no-such-comment")` → `NotFoundError{Entity: "comment"}`.
- `AddComment` on a missing issue → `NotFoundError{Entity: "issue"}`.

**`labels_roundtrip`**
- `AddLabel` returns the resulting set.
- The set is **ordered by name**, not by arrival: adding `zeta` then `alpha` yields `["alpha","zeta"]`.
- Adding a label twice is the same end state, **not an error**.
- `RemoveLabel` returns the resulting set.
- Removing an absent label → `NotFoundError{Entity: "label"}`.
- `ReplaceLabels` states the whole set: what was there and is not named is gone.
- `AddLabel` on a missing issue → `NotFoundError{Entity: "issue"}`.

**`relations_roundtrip`**
- `AddRelation` with `RelBlocks` returns the edge with `src == dependent`, `dst == dependency` — the direction convention is contract.
- `ListRelationsForIssue(id)` with no type argument returns **every** edge type (2 here).
- `ListRelationsForIssue(id, RelBlocks)` narrows to the one blocks edge — naming no type means every type, never no types.
- Edges are readable **from either end**: the dependency sees its dependent.
- `RemoveRelation` succeeds once; a second call → `NotFoundError{Entity: "relation"}`.
- `related-to` is symmetric, so an issue cannot be related to itself.

**`relations_batch_buckets_edges`**
- `GetRelationsByIDs([epic, child, dependency])` returns 3 entries.
- The child's `Parent` is the epic.
- The child's `DependsOn` is `[dependency]`.
- The epic's `Children` is `[child]`.
- The dependency's `Blocks` is `[child]` — `DependsOn` and `Blocks` are the two readings of one edge set, with no second row existing.
- `GetRelationsByIDs(nil)` returns an empty map, not an error.

**`parent_wiring`**
- `SetParent` wires a child under an epic; `mustChildren` (a `ListIssues` with `ParentIDs: [parent]`, `IncludeArchived` and `IncludeDeleted`) shows it.
- **Reparenting replaces rather than adds**: after a second `SetParent`, the old epic has no children and the new one has the child.
- `ClearParent` detaches; the parent then has no children.
- `ClearParent` on a parentless child → `NotFoundError{Entity: "parent relation"}`.
- `SetParent` to itself is an error.
- `SetParent` under a missing parent → `NotFoundError{Entity: "issue"}`.

**`topics_derive_from_issues`**
- Three issues across two topics yield exactly `["parser","renderer"]` — distinct, ascending, never a stored list that could disagree with the issues.

**`export_carries_whole_store`**
- Export carries the **whole** store, archived work included; an export honoring the listing default would silently drop archived work from every backup and every diff.
- Exported issue ids and their order: epic, child, archived, then the ten tied issues.
- `export.Relations` holds the one parent edge with `SrcID == child.ID`.
- `export.Comments` holds the one comment.
- `export.Labels` holds the one label `"perf"`.
- `export.Events` is non-empty — the history must travel with the state.
- `export.Events` order **equals** `ListAllEvents` order, compared by event id. The case deliberately manufactures ten same-tick tie groups (each a create plus a combined action-and-fields `Apply`, which records several events sharing one timestamp) to make ordering observable; ten groups put agreement-by-coincidence at 2^-10.
- `export.ExportedAt` is non-zero.

**`bulk_apply_creates_and_updates`**
- A batch of two create docs wired by `local_id`/`parent` yields `Created` entries keyed `"root"` and `"leaf"`, and the parent relationship is wired.
- A doc naming a real `ID` is an **update**, not a second create: `Updated == [leafID]` and `Created` is empty.
- The update persists.

**`bulk_apply_compensates_a_failed_batch`**
- A batch whose second doc names a parent that is neither a batch-local name nor a real id passes the file's own validation and fails at the write.
- After the failure, the default listing is **empty** — the issue created before the failure was undone.
- The contract trades atomicity for an account: creates are undone and what could not be undone is named in the error.

**`import_tree_maps_local_ids`**
- A three-spec tree returns an `IDMap` with an entry per spec.
- `parent: "root"` wires `leaf` as a child of `root`.
- `depends_on: ["leaf"]` on `other` produces exactly one `RelBlocks` edge with `Src == other`, `Dst == leaf` — the dependent is the edge's src.

**`attribution_stamps_events`**
- Before `AttributeTo` is called, every event of an issue created then has `Attribution.Present() == false` — unattributed rather than half-attributed.
- After `AttributeTo("stream-token")`, an issue created then records at least one event.
- **Every** event that mutation produced carries attribution — it is stamped at the store's one insertion point, not by call sites that remembered.
- `Attribution.Stream() == "stream-token"`.
- `Attribution.Workspace()` is non-empty — the pair is complete or absent.

**`local_issue_count_tracks_creates`**
- A fresh engine reports 0 rather than failing.
- After two creates, one of which is soft-deleted, the count is **2** — it counts what the store holds, because a soft-deleted issue is still work that would be lost.

### 3.4 Assertion helpers and what they imply

- `assertOrder` pins the order of a **default (unsorted) listing**, which is the only surface through which rank is observable across engines — **the `Rank` value itself is an engine's own encoding and is deliberately never asserted**.
- `assertState` reads an issue back with `GetIssue` and pins its derived `State()`, including out-of-flow issues a default listing would not show.
- `assertPrecedes` pins a relative position without pinning the whole listing.
- `assertStrings` compares by content and order joined with `"|"`; **a nil result and an empty one compare equal on purpose** — an engine spelling "nothing here" as nil rather than a zero-length slice has not behaved differently.
- `assertNotFound` requires `errors.As` to a `storage.NotFoundError` **and** an exact `Entity` match — callers dispatch on the type, and the entity tells a user WHICH thing was missing when an operation touches several.
- Entity strings the suite pins: `"issue"`, `"comment"`, `"label"`, `"relation"`, `"parent relation"`.
- Every helper fails the test rather than returning an error, so a case body reads as the behavioral statement it is.

### 3.5 What the conformance suite does NOT require

Derived from what the suite never exercises (the `cases` table at is the complete list):
- Any capability interface (`Syncer`, `Reconciler`, `Checkpointer`, `Repairer`, `SchemaMigrator`, `Importer`, `RawExecutor`) — capability presence is tested per-engine, not by the suite (`internal/storage/memory/capabilities_test.go`; `internal/storage/capabilities_test.go`).
- Concurrency/thread safety — the suite is sequential by construction, so the memory engine tests it separately (`internal/storage/memory/engine_test.go`).
- The exact `Rank` string encoding.
- The exact minted-id shape.
- `ListIssuesFilter.Resolutions` filtering (declared at `internal/storage/issues.go`, implemented at `internal/storage/memory/list.go`, but no case in the table exercises it).
- `ReplaceLabels`/`ListLabels` against a missing issue.
- `Close()` behavior beyond the engine factory's own cleanup.
- `AddRelation` cycle rejection for `blocks` (implemented at `internal/storage/memory/edges.go`, not exercised by any listed case).
- `ParseSortSpecs`, `ParseBulkSpecs`, `ParseImportTreeSpecs` — tested in the contract package instead (`internal/storage/specs_test.go`).
