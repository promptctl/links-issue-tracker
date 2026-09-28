# Raw behavioral inventory — model / lifecycle / issueid / rank / precedence / annotation / lawtokens / pathspec / query / trace / interrupt

Derived exclusively from Go source and `_test.go` files. All paths are relative to
`/Users/bmf/code/links-issue-tracker`. Every claim carries a `file:line` citation.

---

# 1. `internal/model/lifecycle`

Package doc: `internal/model/lifecycle/lifecycle.go`. Declares that callers
outside `internal/model` must not import this package, and that container
`Progress` aggregation is leaf-primitive based: `AllOf.Progress` folds over
`Progresses(a)`, which walks *through* `Container`s and collects every
non-`Container` descendant's `Progress`. A wrapper primitive that does not
implement `Container` contributes only its own `Progress`.

## 1.1 `State` (activity axis)

`type State string` — `lifecycle.go`.

Complete value set (`lifecycle.go`):

| Constant | Wire/storage value |
|---|---|
| `Open` | `"open"` |
| `InProgress` | `"in_progress"` |
| `Closed` | `"closed"` |

`State.Display() string` — `lifecycle.go`. Returns `"in progress"` for
`InProgress`; for every other value returns `string(s)` unchanged (so an unknown
State stringifies as itself). Wire/storage keep the underscored form
(`lifecycle.go`).

### `ParseState(value string) (State, error)` — `lifecycle.go`
- Normalizes: `strings.TrimSpace` then `strings.ToLower` (`lifecycle.go`).
- Alias: the literal normalized string `"in-progress"` (hyphen) is rewritten to
  `"in_progress"` (`lifecycle.go`).
- Accepts exactly `open`, `in_progress`, `closed` (`lifecycle.go`).
- Otherwise error text: `invalid status %q (valid: open, in_progress, closed)`
  where `%q` is the **original** (un-normalized) input (`lifecycle.go`).
- Blank input is rejected (test `TestParseStateRejectsBlank`,
  `lifecycle_test.go`). Case/alias normalization pinned by
  `TestParseStateNormalizes`, `lifecycle_test.go`.

### `DefaultOpen(value string) State` — `lifecycle.go`
Parses via `ParseState`; on any error returns `Open`. Documented as the lenient
boundary (import, hydration, storage); strict boundaries use `ParseState`
(`lifecycle.go`). Pinned by `TestDefaultOpenReturnsOpenForInvalid`,
`lifecycle_test.go`.

## 1.2 `Progress` — `lifecycle.go`

Struct with four `int` fields and JSON tags:
`Open` (`json:"open"`), `InProgress` (`json:"in_progress"`), `Closed`
(`json:"closed"`), `Total` (`json:"total"`).

## 1.3 `ActionName` and the action vocabulary

`type ActionName string` — `lifecycle.go`.

Complete value set (`lifecycle.go`):

| Constant | Value | Axis |
|---|---|---|
| `ActionStart` | `"start"` | status |
| `ActionDone` | `"done"` | status |
| `ActionClose` | `"close"` | status |
| `ActionReopen` | `"reopen"` | status |
| `ActionArchive` | `"archive"` | retention |
| `ActionUnarchive` | `"unarchive"` | retention |
| `ActionDelete` | `"delete"` | retention |
| `ActionRestore` | `"restore"` | retention |

### `(ActionName).Verb() string` — `lifecycle.go`
`ActionName` is documented as the persisted event verb — the encoding written
into the events table. `Verb()` returns a different name for the same action:
the word a caller types to invoke it.

`var actionVerbs = map[ActionName]string` — `lifecycle.go` — has an entry
for every one of the eight actions, including the seven whose two names are
identical: `start`, `done`, `close`, `archive`, `unarchive`, `delete`,
`restore`. `ActionReopen` is the only action whose two names differ: persisted
as `"reopen"`, invoked as `"open"` (the command is `lit open`; there is no
`lit reopen`).

On an `ActionName` that has no entry, `Verb()` panics; it does not fall back to
the persisted encoding.

### `Actions() []ActionName` — `lifecycle.go`
Returns a **fresh slice each call** in canonical order: the four status verbs
then the four retention verbs — `start, done, close, reopen, archive,
unarchive, delete, restore`.

### `ParseAction(value string) (ActionName, error)` — `lifecycle.go`
- Normalizes `TrimSpace` + `ToLower` (`lifecycle.go`).
- Membership tested against `Actions()` (`lifecycle.go`).
- Error text on miss: `unsupported lifecycle action %q` with the original input
  (`lifecycle.go`). Pinned by `TestParseActionValid`
  (`lifecycle_test.go`), `TestParseActionRoundTrips`,
  `TestParseActionRejectsUnknown`.

## 1.4 Core interfaces

- `Lifecycle` — `lifecycle.go`: `State() State`, `Progress() Progress`.
- `Container` — `lifecycle.go`: `Lifecycle` + `Children() []Lifecycle`.
- `Actionable` — `lifecycle.go`: `Lifecycle` + `Apply(action StatusAction) Lifecycle`.
  `Apply` is documented total — no error return, because `StatusAction.Target()`
  always names a real state and a same-state call returns the receiver
  (`lifecycle.go`).
- `StatusPrimitive` — `status_states.go`: `Actionable` +
  `ClosedAt() *time.Time`, `Resolution() *Resolution`, `RedirectTarget() *string`.
  `AllOf` is deliberately not a `StatusPrimitive` (`status_states.go`).

### `Walk(l Lifecycle, visit func(Lifecycle) bool)` — `lifecycle.go`
Depth-first. Returns immediately if `l == nil` **or** `visit(l)` returns false
(`lifecycle.go`). If `l` implements `Container`, recurses into every
`Children()` element (`lifecycle.go`). Pinned by
`TestWalkVisitsAllPrimitives`, `lifecycle_test.go`.

### `Progresses(l Lifecycle) []Progress` — `lifecycle.go`
Walks `l`; skips appending for any node implementing `Container` (but keeps
descending, since it returns `true`), appends `current.Progress()` for every
non-container node (`lifecycle.go`). Returns a non-nil empty slice when
nothing matches (initialized `out := []Progress{}`, `lifecycle.go`).

## 1.5 Sealed action sum — `action.go`

- `Action` interface — `action.go`: `Name() ActionName` plus unexported
  `isAction()`, sealing implementations to this package.
- `StatusAction` — `action.go`: `Action` + `Target() State`.
- `RetentionAction` — `action.go`: `Action` + unexported
  `isRetentionAction()`.
- Retention actions are deliberately NOT `StatusAction`s, so applying one to the
  status machine is unrepresentable (`action.go`).

Complete variant set:

| Variant | Fields | `Name()` | `Target()` | Subset |
|---|---|---|---|---|
| `Start` (`action.go`) | `Assignee string` | `start` | `InProgress` | StatusAction |
| `Done` (`action.go`) | none | `done` | `Closed` | StatusAction |
| `Close` (`action.go`) | `Outcome Outcome` | `close` | `Closed` | StatusAction |
| `Reopen` (`action.go`) | none | `reopen` | `Open` | StatusAction |
| `Archive` (`action.go`) | none | `archive` | — | RetentionAction |
| `Unarchive` (`action.go`) | none | `unarchive` | — | RetentionAction |
| `Delete` (`action.go`) | none | `delete` | — | RetentionAction |
| `Restore` (`action.go`) | none | `restore` | — | RetentionAction |

`Start` is the only variant carrying an assignee — it is described as the only
action that rewrites the assignee (`action.go`). Encoding pinned by
`TestActionSumEncodings`, `lifecycle_test.go`.

## 1.6 `Outcome` (close reason payload) — `action.go`

`Outcome` interface: `Resolution() Resolution` + unexported `isOutcome()`
(`action.go`).

| Variant | Payload field | `Resolution()` |
|---|---|---|
| `Duplicate` (`action.go`) | `Of string` — the canonical ticket | `ResolutionDuplicate` |
| `Superseded` (`action.go`) | `By string` — the replacing ticket | `ResolutionSuperseded` |
| `Obsolete` (`action.go`) | none | `ResolutionObsolete` |
| `Wontfix` (`action.go`) | none | `ResolutionWontfix` |

Terminal outcomes structurally cannot carry a redirect target (`action.go`).
Agreement between outcome encodings and the redirect predicate is pinned by
`TestOutcomeEncodingsAgreeWithRedirectPredicate`, `lifecycle_test.go`.

## 1.7 `Resolution` — `resolution.go`

`type Resolution string` — `resolution.go`.

Complete value set (`resolution.go`):

| Constant | Value | Meaning (per `resolution.go`) |
|---|---|---|
| `ResolutionDuplicate` | `"duplicate"` | redirects to a canonical ticket |
| `ResolutionSuperseded` | `"superseded"` | redirects to a canonical ticket |
| `ResolutionObsolete` | `"obsolete"` | the need is gone; terminal |
| `ResolutionWontfix` | `"wontfix"` | standing decision not to do the work; terminal |

- `(Resolution).RedirectsToCanonical() bool` — `resolution.go`: true for
  exactly `duplicate` and `superseded`. Pinned by `TestRedirectsToCanonical`,
  `lifecycle_test.go`.
- `ParseResolution(s string) (Resolution, error)` — `resolution.go`:
  trims surrounding whitespace only — **no lowercasing** (`resolution.go`);
  accepts the four constants; error text
  `resolution must be one of: duplicate, superseded, obsolete, wontfix`
  (`resolution.go`). Pinned by `TestParseResolutionRoundTrips`
  (`lifecycle_test.go`) and `TestParseResolutionRejectsInvalid`.
- `cloneResolution(*Resolution) *Resolution` — `resolution.go`: nil in →
  nil out; otherwise a fresh pointer to a copy.

## 1.8 Leaf status primitives — `status_states.go`

Three unexported variants, all `StatusPrimitive`:

| Variant | `State()` | `Progress()` | `ClosedAt()` | `Resolution()` | `RedirectTarget()` |
|---|---|---|---|---|---|
| `openState` | `Open` | `{Open:1, Total:1}` | nil | nil | nil |
| `inProgressState` | `InProgress` | `{InProgress:1, Total:1}` | nil | nil | nil |
| `closedState` | `Closed` | `{Closed:1, Total:1}` | clone of `closedAt` | clone of `resolution` | clone of `redirectTarget` |

`closedState` fields are all pointers (`status_states.go`): closed
**may** carry a close time, a resolution, and (for a redirecting resolution) a
target — none is mandatory at the type level (rationale `status_states.go`).

### `NewStatus(state State, closedAt *time.Time, resolution *Resolution, redirectTarget *string) StatusPrimitive` — `status_states.go`
- Dispatches on `DefaultOpen(string(state))`, so blank/unrecognized state →
  `openState` (`status_states.go`).
- For `Closed`: if `resolution == nil` **or**
  `!resolution.RedirectsToCanonical()`, `redirectTarget` is forced to nil
  (`status_states.go`); the closed leaf stores cloned `closedAt`,
  cloned `resolution`, and `normalizeRedirectTarget(redirectTarget)`
  (`status_states.go`).
- For `InProgress`: returns bare `inProgressState{}` — `closedAt`, `resolution`,
  `redirectTarget` are all discarded (`status_states.go`).
- Default (incl. `Open`): bare `openState{}` (`status_states.go`).
- Pinned by `TestNewStatusStateMirrorsValue` (`lifecycle_test.go`),
  `TestNewStatusClosedAtBelongsOnlyToClosed`,
  `TestNewStatusResolutionBelongsOnlyToClosed`,
  `TestNewStatusRedirectTargetRequiresRedirectingResolution`.

### `applyStatusAction(current Lifecycle, action StatusAction) Lifecycle` — `status_states.go`
The shared transition for all three leaf variants (each `Apply` delegates:
).
- `target := action.Target()`; if `current.State() == target`, returns the
  receiver **unchanged** (`status_states.go`). Consequence: re-closing a
  closed issue keeps the existing resolution/closedAt rather than rewriting it
  (`status_states.go`; pinned by `TestApplySameStateReturnsReceiverUnchanged`,
  `lifecycle_test.go`).
- Target `Closed`: stamps `closedAt = time.Now().UTC()` and attaches
  `closeResolution(action)` and `closeRedirectTarget(action)`
  (`status_states.go`).
- Target `InProgress`: returns `inProgressState{}`.
- Target anything else (i.e. `Open`): returns `openState{}` — so a
  reopen clears closedAt, resolution and redirect target (pinned by
  `TestApplyReopenClearsResolution`, `lifecycle_test.go`).
- Cannot fail — no error channel (`status_states.go`).
- Target matrix pinned by `TestApplyTargetStateMatrix` (`lifecycle_test.go`)
  and close bookkeeping by `TestApplyClosedAtBookkeeping`.

### `closeResolution(action StatusAction) *Resolution` — `status_states.go`
- If the action is a `Close`: `c.Outcome == nil` **panics** with
  `lifecycle: Close action requires an Outcome; use Done for the neutral success close`
  (`status_states.go`); otherwise returns a pointer to
  `c.Outcome.Resolution()`.
- Any other action (including `Done`) → nil, i.e. `done` is the neutral success
  close that records no resolution (`status_states.go`).

### `closeRedirectTarget(action StatusAction) *string` — `status_states.go`
Non-`Close` → nil. `Duplicate` → its `Of`; `Superseded` → its `By`; any other
outcome → nil. Result passes through `normalizeRedirectTarget`.

### `normalizeRedirectTarget(target *string) *string` — `status_states.go`
nil → nil; `strings.TrimSpace`; empty after trim → nil; otherwise a **fresh**
pointer to the trimmed string (never an alias of the input). Pinned by
`TestApplyCloseNormalizesBlankRedirectTarget`, `lifecycle_test.go`, and the
outcome pass-through by `TestApplyCloseCarriesOutcomeThroughMachine`.

Helpers: `cloneTime` (`status_states.go`), `cloneString`
(`status_states.go`) — nil-preserving deep copies.

## 1.9 `AllOf` (container primitive) — `all_of.go`

- `type AllOf struct { Members []Lifecycle }` — `all_of.go`.
- `Children() []Lifecycle` — `all_of.go`: returns a copy of `Members`.
- `State() State` — `all_of.go`, computed from its own `Progress()`:
  - `Total > 0 && Closed == Total` → `Closed`
  - else `InProgress > 0 || Closed > 0` → `InProgress`
  - else → `Open` (so an **empty** container is `Open`).
  Pinned by `TestAllOfState`, `lifecycle_test.go`.
- `Progress() Progress` — `all_of.go`: field-wise sum over
  `Progresses(a)` (i.e. over every non-container descendant). Pinned by
  `TestAllOfProgressAndActions` (`lifecycle_test.go`) and
  `TestAllOfProgressIncludesNonStatusLeafPrimitives`.
- `AllOf` does not implement `Actionable` — no `Apply` method exists
  (`all_of.go`; pinned by `TestAllOfIsNotActionable`, `lifecycle_test.go`).

## 1.10 Retention axis — `retention.go`

`type Retention interface{ isRetention() }` — `retention.go`. Sealed to three
value variants (`retention.go`):

| Variant | Fields | Meaning |
|---|---|---|
| `Live` (`retention.go`) | none | in the flow; the meaning assigned to the axis's zero value (`retention.go`) |
| `Archived` (`retention.go`) | `At time.Time` | soft-hidden since `At`; out of default listings but **retained in rank space**; reversible via unarchive (`retention.go`) |
| `Deleted` (`retention.go`) | `At time.Time` | soft-removed since `At`; **excluded from rank space**; reversible via restore (`retention.go`) |

Archived-and-deleted simultaneously is unrepresentable (`retention.go`).

### `Frozen(r Retention) bool` — `retention.go`
`Live` → false; `Archived`/`Deleted` → true; anything else (raw nil, typed-nil
pointer variant) **panics** `illegal Retention value %T` (`retention.go`).
Pinned by `TestFrozen` (`retention_test.go`) and
`TestFrozenRefusesImpostors`.

### `Retain(cur Retention, action RetentionAction, at time.Time) (Retention, error)` — `retention.go`
Pure (no clock, no store); caller supplies `at`. Complete transition table:

| current \ action | `Archive` | `Unarchive` | `Delete` | `Restore` |
|---|---|---|---|---|
| `Live` | → `Archived{At: at}` | err `issue is not archived` | → `Deleted{At: at}` | err `issue is not deleted` |
| `Archived` | err `issue is already archived` | → `Live{}` | → `Deleted{At: at}` | err `issue is not deleted` |
| `Deleted` | err `cannot archive deleted issue` | err `cannot unarchive deleted issue` | err `issue is already deleted` | → `Live{}` |

- Deleting an `Archived` issue **drops the archive stamp**, so a later restore
  always lands on `Live` (`retention.go`; pinned by
  `TestRetainArchiveDeleteRestoreLandsLive`, `retention_test.go`).
- An impostor `RetentionAction` panics `illegal RetentionAction value %T`
  (`retention.go`); an impostor `Retention` panics
  `illegal Retention value %T` (`retention.go`). Pinned by
  `TestRetainRefusesImpostors` (`retention_test.go`) and
  `TestRetainRefusesImpostorActions`. Full table pinned by
  `TestRetainTransitionTable` (`retention_test.go`).

### `RetentionFromTimestamps(archivedAt, deletedAt *time.Time) Retention` — `retention.go`
Decodes the two-nullable-timestamp encoding. Precedence: `deletedAt != nil` →
`Deleted{*deletedAt}` (deletion dominates, and a legacy both-set row drops its
archive stamp); else `archivedAt != nil` → `Archived{*archivedAt}`; else `Live{}`.
Pinned by `TestRetentionFromTimestamps`, `retention_test.go`.

### `RetentionTimestamps(r Retention) (archivedAt, deletedAt *time.Time)` — `retention.go`
`Live` → `(nil, nil)`; `Archived` → `(&At, nil)`; `Deleted` → `(nil, &At)`;
anything else **panics** `illegal Retention value %T` (`retention.go`).
Round trip pinned by `TestRetentionTimestampsRoundTrip` (`retention_test.go`);
impostor refusal by `TestRetentionTimestampsRefusesImpostors`.

---

# 2. `internal/model`

## 2.1 Re-exported lifecycle surface — `model.go`

Type aliases (`model.go`): `State`, `Progress`, `ActionName`,
`Resolution`, `Retention`, `Live`, `Archived`, `Deleted`, `Action`,
`StatusAction`, `RetentionAction`, `Start`, `Done`, `Close`, `Reopen`,
`Archive`, `Unarchive`, `Delete`, `Restore`, `Outcome`, `Duplicate`,
`Superseded`, `Obsolete`, `Wontfix`.

Constant re-exports (`model.go`): `StateOpen`, `StateInProgress`,
`StateClosed`; `ActionStart/Done/Close/Reopen`;
`ActionArchive/Unarchive/Delete/Restore`;
`ResolutionDuplicate/Superseded/Obsolete/Wontfix`.

Function re-exports (`model.go`): `ParseState`, `ParseAction`, `Actions`,
`DefaultOpen`, `ParseResolution`, `RetentionFromTimestamps`,
`RetentionTimestamps`, `Retain`, `Frozen`.

## 2.2 `IssueType` — `issue_type.go`

`type IssueType string` — `issue_type.go`.

Complete value set (`issue_type.go`): `TypeTask="task"`,
`TypeFeature="feature"`, `TypeBug="bug"`, `TypeChore="chore"`,
`TypeEpic="epic"`.

- `IssueTypes() []IssueType` — `issue_type.go`: fresh slice per call, in
  canonical order `task, feature, bug, chore, epic`.
- `ParseIssueType(s string) (IssueType, error)` — `issue_type.go`:
  `ToLower(TrimSpace(s))`, then exact membership in `IssueTypes()`. On miss
  returns the package-level `errInvalidIssueType` whose text is
  `issue type must be ` + `oxfordOr(IssueTypes())` =
  `"issue type must be task, feature, bug, chore, or epic"`
  (`issue_type.go`). Pinned by `TestParseIssueType`,
  `model_test.go`.
- `ParseIssueTypes(inputs ...string) ([]IssueType, error)` — `issue_type.go`:
  splits every input on `,` and runs each fragment through `ParseIssueType`,
  skipping none, so a blank fragment is an error; the error names the failing
  fragment (`invalid issue type %q: %w`, wrapping `errInvalidIssueType`). No
  inputs → nil. The one type-set parser, shared by `lit ls --type` and the
  `type:` query term.
- `(IssueType).IsContainer() bool` — `issue_type.go`: true **only** for
  `TypeEpic`.
- `ContainerTypes() []IssueType` — `issue_type.go`: the subset of
  `IssueTypes()` for which `IsContainer()` holds (today: `[epic]`); returns nil
  if the subset is empty.
- `oxfordOr[T ~string](values []T) string` — `issue_type.go`: single element
  → that element; two → `a + " or " + b` (no comma); three or more →
  `strings.Join(all but last, ", ") + ", or " + last`. Generic over `~string` so
  one renderer serves every sealed vocabulary in the package — `IssueTypes()`
  passes its own named string type, `priorityTokens()` passes rendered `[]string`
  — instead of each domain reimplementing the phrasing.

## 2.3 `Priority` — `priority.go`

`type Priority int` — `priority.go`. Complete value set (`priority.go`):
`PriorityNormal = 0`, `PriorityUrgent = 1`.

- `priorityVocabulary` — `priority.go`: the one table the domain is spelled
  in, in canonical order, each entry pairing a `Priority` with its display word;
  its first entry is where out-of-domain ints coerce. Every other function in the
  file is a read of it in some direction, which is what makes the word a read
  surface prints the same word the write flag accepts.
- `priorityEntry(v int) (Priority, string)` — `priority.go`: the table's
  only scan-by-value, resolving any raw int onto its entry and coercing
  out-of-domain values onto the first. `CanonicalPriority` and `String` are each
  one line of it, so a priority's number and its word cannot resolve differently.
- `CanonicalPriority(v int) Priority` — `priority.go`: returns
  `PriorityUrgent` iff `v == 1`; **every other int** (including negatives and
  anything ≥2) maps to `PriorityNormal`. Idempotent — its fixed points are the
  legal priorities (pinned by `TestCanonicalPriorityIsIdempotent`,
  `priority_test.go`).
- `ParsePriority(v int) (Priority, error)` — `priority.go`: the int gate,
  for the import and bulk payloads; accepts only values already canonical
  (0 and 1), otherwise returns `0` and the shared `errInvalidPriority`. Pinned by
  `TestParsePriorityAcceptsExactlyCanonicalFixedPoints` (`priority_test.go`),
  `TestCanonicalizedPriorityAlwaysParses`,
  `TestCanonicalPriorityPreservesRestoreTolerance`.
- `ParsePriorityName(raw string) (Priority, error)` — `priority.go`: the
  string gate, and the only string-to-Priority conversion; backs the `--priority`
  flag on `new`/`followup`/`update`. Lowercases and trims, then accepts either
  spelling a read surface emits — the display word (`lit show`, the issue rows) or
  the decimal (`lit export`, the `lit import` payload) — and refuses every other
  token, so `--priority 7` cannot reach the store and `--priority 2` does not
  inherit `CanonicalPriority`'s salvage coercion. Added by links-cli-bvko, which
  fixed a `--priority` declared as an `fs.Int`: pflag's `strconv.ParseInt` refused
  the very word every read surface printed, and its bare error missed the
  `validation_refused` arm, drawing the default "Retry the command" remediation on
  a refusal no retry can change. Pinned by
  `TestPriorityWordRoundTripsThroughTheWriteGate` (`priority_test.go`),
  `TestPriorityDecimalRoundTripsThroughTheWriteGate`,
  `TestParsePriorityNameAgreesWithParsePriorityOnTheDecimalDomain`,
  `TestParsePriorityNameRejectsEverythingOutsideTheVocabulary`,
  `TestParsePriorityNameCanonicalizesCaseAndSpace`.
- `errInvalidPriority` — `priority.go`: the one refusal both gates return,
  built at init from `priorityTokens()` through `oxfordOr`, so it names every
  accepted token in both spellings (`priority must be normal (0) or urgent (1)`)
  rather than a fixed string that could drift from the domain. Pinned by
  `TestPriorityRefusalNamesEveryAcceptedToken` (`priority_test.go`).
- `Priorities() []Priority` — `priority.go`: the legal priorities in
  canonical order, for callers that render the vocabulary (flag help, usage
  strings) instead of spelling the set again.
- `(Priority).String() string` — `priority.go`: the word for the
  priority `priorityEntry` resolves the receiver to, so `"urgent"` for
  `PriorityUrgent` and `"normal"` for **everything else** — total over raw ints,
  which the salvage paths rely on. Pinned by `TestPriorityString`,
  `priority_test.go`, and — as the round trip against the write gate —
  `TestPriorityWordRoundTripsThroughTheWriteGate`.

## 2.4 `RelationType` — `relation_type.go`

`type RelationType string` — `relation_type.go`. Complete value set
(`relation_type.go`): `RelBlocks="blocks"`,
`RelParentChild="parent-child"`, `RelRelatedTo="related-to"`.

- `ParseRelationType(s string) (RelationType, error)` — `relation_type.go`:
  `strings.TrimSpace` only — **no lowercasing**; accepts the three constants;
  error text `relation type must be blocks, parent-child, or related-to`
  (`relation_type.go`). Pinned by `TestParseRelationType`,
  `relation_type_test.go`.
- `(RelationType).StoreEndpoints(from, to string) (string, string)` —
  `relation_type.go`: for `blocks` returns `(to, from)` — i.e. `blocks` is
  stored **dependent → dependency**, the reverse of the human reading
  "<blocker> blocks <blocked>"; every other type passes through unchanged. The
  swap is an involution, so the same call converts store order back to display
  order (`relation_type.go`). Pinned by
  `TestRelationTypeStoreEndpoints`, `relation_type_test.go`.
- `(RelationType).SingleValuedFromSrc() bool` — `relation_type.go`: true
  only for `parent-child` (a child has at most one parent); `blocks` and
  `related-to` are many-valued.
- `(RelationType).CanonicalEndpoints(src, dst string) (string, string)` —
  `relation_type.go`: for `related-to` (undirected) returns the pair
  sorted ascending — swaps iff `dst < src`; directed types pass through
  unchanged. Pinned by `TestRelationTypeCanonicalEndpoints`,
  `relation_type_test.go`.

## 2.5 Labels — `label.go`

`NormalizeLabel(label string) (string, error)` — `label.go`:
`strings.ToLower(strings.TrimSpace(label))`; empty after normalization →
error `label is required` (`label.go`); containing `,` → error
`label cannot contain commas` (commas are reserved as the list separator on
input surfaces) (`label.go`). Otherwise returns the normalized form.

## 2.6 `Capabilities` / `StatusView` — `capabilities.go`

- `type Capabilities struct { Status *StatusView \`json:"status,omitempty"\` }` —
  `capabilities.go`.
- `type StatusView struct` — `capabilities.go`:
  - `Value State` (`json:"value"`)
  - `ClosedAt *time.Time` (`json:"closed_at,omitempty"`)
  - `Resolution *lifecycle.Resolution` (`json:"resolution,omitempty"`)
  - `RedirectTarget *string` (`json:"redirect_target,omitempty"`)
  - Assignee is explicitly **not** part of the status capability
    (`capabilities.go`).
- `capabilitiesFrom(l lifecycle.Lifecycle) Capabilities` — `capabilities.go`:
  root-only, no recursion. If `l` is a `lifecycle.StatusPrimitive`, returns a
  populated `StatusView` from `State()/ClosedAt()/Resolution()/RedirectTarget()`;
  otherwise returns the empty `Capabilities{}`.
- `cloneTime` / `cloneResolution` / `cloneString` — `capabilities.go`:
  nil-preserving deep copies.

## 2.7 `Issue` — `model.go`

| Field | Type | JSON tag | Semantics |
|---|---|---|---|
| `ID` | string | `id` | issue identifier |
| `Title` | string | `title` | |
| `Description` | string | `description` | |
| `Prompt` | string | `prompt,omitempty` | |
| `Priority` | `Priority` | `priority` | |
| `IssueType` | `IssueType` | `issue_type` | |
| `Topic` | string | `topic` | |
| `Assignee` | string | `assignee,omitempty` | owner, orthogonal to the status machine and preserved across every transition; the lifecycle leaf carries no assignee |
| `Rank` | string | `rank` | |
| `Lane` | string | `lane` | partitions an epic's children into parallel rank-ordered sub-sequences: same lane → sequenced by rank; different lanes → parallel. Empty string is the shared default lane (fully sequential). Meaningful only within an epic |
| `Labels` | `[]string` | `labels` | |
| `CreatedAt` | `time.Time` | `created_at` | |
| `UpdatedAt` | `time.Time` | `updated_at` | |
| `retention` | `lifecycle.Retention` | unexported | sealed retention axis; wire/storage keep the legacy `archived_at`/`deleted_at` pair |
| `lifecycle` | `lifecycle.Lifecycle` | unexported | |
| `pendingHydration` | bool | unexported | |

### Accessors and mutators
- `Retention() lifecycle.Retention` — `model.go`: nil field normalizes to
  `lifecycle.Live{}`.
- `SetRetention(r lifecycle.Retention)` — `model.go`: accepts only
  `Live`, `Archived`, `Deleted` value variants; anything else **panics**
  `issue %q: illegal Retention value %T`. Pinned by
  `TestSetRetentionRefusesImpostors`, `model_test.go`.
- `State() State` — `model.go`: `mustLifecycle().State()`.
- `Progress() Progress` — `model.go`: `mustLifecycle().Progress()`.
  Both fail loud on an unhydrated issue rather than returning a zero value
  (`model.go`).
- `InPlay() bool` — `model.go`: `!lifecycle.Frozen(Retention()) &&
  State() != StateClosed` — the single definition of "unfinished". Pinned by
  `TestInPlayIsTheOneUnfinishedRule`, `lane_test.go`.
- `Capabilities() Capabilities` — `model.go`: a container returns the
  empty `Capabilities{}` **without** requiring hydration; a leaf routes through
  `mustLifecycle()` and therefore panics if unhydrated. Pinned by
  `TestContainerCapabilitiesAreEmptyWithoutHydration` (`model_test.go`) and
  `TestNilLifecycleLeafCapabilitiesPanic`.
- `StatusValue() string` — `model.go`: `""` when no status capability;
  otherwise the state string.
- `AssigneeValue() string` — `model.go`: the `Assignee` field.
- `ClosedAtValue() *time.Time` — `model.go`: nil without a status
  capability; otherwise a clone.
- `ResolutionValue() *lifecycle.Resolution` — `model.go`: clone; nil
  unless closed with a recorded resolution.
- `RedirectTargetValue() *string` — `model.go`: clone; nil unless closed
  with a redirecting resolution carrying a target.
- `IsContainer() bool` — `model.go`: delegates to `IssueType.IsContainer()`
  — decided by type, never by the lifecycle shape. Pinned by
  `TestIsContainerUsesIssueTypeNotLifecycle`, `model_test.go`.
- `IsHydrated() bool` — `model.go`: false if `pendingHydration`; else
  `lifecycle != nil`.
- `mustLifecycle()` — `model.go`: panics
  `issue %q: lifecycle read on unhydrated issue: %v`.
- `lifecycleOrError()` — `model.go`: if `pendingHydration`, returns error
  `issue %s requires store hydration`; if `lifecycle == nil`, **panics**
  `issue %q has no lifecycle (constructed without HydrateStatus/HydrateAllOf)`.
  Pinned by `TestNilLifecycleIssueLifecycleMethodsPanic` (`model_test.go`)
  and `TestNeedsStoreHydrationChildDerivedReadsPanic`.
- `replaceLifecycle(next)` — `model.go`: sets `lifecycle` and clears
  `pendingHydration`; the single centralized mutation path.

### Lane identity
`type LaneID struct { epic, key string; solo bool }` — `model.go`. Fields
unexported so `LaneOf` is the only construction route (`model.go`).

`LaneOf(issue Issue, parent *Issue) LaneID` — `model.go`:
- `parent == nil` **or** `!parent.IsContainer()` → `LaneID{key: issue.ID, solo: true}`
  (a "lane of one"; a non-container parent scopes nothing —
  `model.go`).
- otherwise → `LaneID{epic: parent.ID, key: issue.Lane}` — siblings sharing the
  same `Lane` spelling (including the empty spelling) are one lane.
- Reads no lifecycle, so it answers for an unhydrated issue (`model.go`).
- Pinned by `TestLaneOfGroupsSiblingsBySpelling` (`lane_test.go`),
  `TestLaneOfScopesToTheEpic`,
  `TestLaneOfWithoutAnEpicIsALaneOfOne`,
  `TestSoloLaneCannotCollideWithAnEpicLane`.

Accessors: `Epic()` (`model.go`), `Key()` (`model.go`).
`String()` — `model.go`: solo → the bare key (the issue id); otherwise
`epic + "#" + key`, so an epic's unnamed default lane renders `"epic#"` and is
never mistaken for the epic itself. Pinned by
`TestLaneStringDistinguishesTheEpicFromItsDefaultLane` (`lane_test.go`) and
`TestLaneAccessorsReportBothHalves`.

### Action dispatch
`ContainerActionError` — `model.go`: fields `ID string`,
`Action ActionName`, `Target State`, `State State`, `Progress Progress`.
`Target` is the state the action asked for; `State` is the one the children
establish.
- `Unfinished() int` — `model.go`: `Progress.Total - Progress.Closed`.
- `Satisfied() bool` — `model.go`:
  `Target == State && Progress.Total > 0 && Unfinished() == 0`. The one predicate
  separating a request the children already meet from a refusal; the CLI's
  reason and exit-code mappings read it rather than re-deriving it from counts.
  The target match alone is not sufficient: `AllOf.State` returns `InProgress`
  whenever a child is in progress or closed, so `start` on a part-done epic
  matches its own target with work left, and it returns `Open` for a childless
  epic as a fallback, so `open` matches there too. The two count conjuncts admit
  only the all-children-closed case without naming `Closed`.
- `Error() string` — `model.go`, two exact wordings (the word rendered inside the backticks in both is the action's invocation verb, `ActionName.Verb()`, not its persisted event encoding; the state is substituted as `State.Display()`, so `in_progress` renders `in progress`):
  - `Satisfied()` → ``epic %s is already %s, so `%s` has nothing to do: an epic's state derives from its children (%d of %d done)``
  - else → ``cannot `%s` epic %s: it is %s, and an epic's state derives from its children rather than from this command (%s)``, where the final clause is `childClause()`.
- `childClause() string` — `model.go`, three exact wordings:
  - `Progress.Total == 0` → `it has no children`
  - `Unfinished() == 0` → `all %d are done`
  - else → `%d of %d are not done`

`(Issue).Apply(action lifecycle.StatusAction) (Issue, error)` — `model.go`:
1. `lifecycleOrError()`; on error returns `(Issue{}, err)`.
2. If the root lifecycle is a `lifecycle.Container` → returns
   `ContainerActionError{ID, action.Name(), State(action.Target()), State(root.State()), root.Progress()}`
. Every status action on a container is refused, including one
   whose target the children already establish — the refusal is what keeps a
   `start` that changes nothing from reaching the engines' no-op rule, which
   compares `StatusValue` (vacuously `""` for a container) and would decide on
   the claimant alone. Pinned by `TestApplyRefusesContainerForEveryAction`,
   `model_test.go`.
3. If the root is not `lifecycle.Actionable` → error
   `no %s action available on this issue`.
4. Otherwise replaces the lifecycle with `actionable.Apply(action)` and returns
   the modified copy. Root-only; multi-leaf `AllOf` composition is
   intentionally unsupported (`model.go`). Pinned by
   `TestApplyTargetStateOnLeafProducesTargetState` (`model_test.go`) and
   `TestApplyCloseOutcomeSurfacesThroughResolutionValue`.

### Hydration
- `HydrateStatus(issue Issue, view StatusView) (Issue, error)` — `model.go`:
  replaces the lifecycle with
  `lifecycle.NewStatus(view.Value, view.ClosedAt, view.Resolution, view.RedirectTarget)`.
  Never returns a non-nil error today.
- `HydrateRow(issue Issue, view StatusView, children []Issue) (Issue, error)` —
  `model.go`: dispatches on `issue.IssueType.IsContainer()` →
  `HydrateAllOf(issue, children)`, else `HydrateStatus(issue, view)`.
- `HydrateAllOf(issue Issue, children []Issue) (Issue, error)` — `model.go`:
  collects each child's lifecycle via `child.lifecycleOrError()` (propagating the
  first error, returning `Issue{}`), then sets `lifecycle.AllOf{Members: members}`.

### JSON encoding
Wire struct `issueJSON` — `model.go`. Keys in declaration order:
`id`, `title`, `description`, `prompt` (omitempty), `status` (`*State`,
omitempty), `priority`, `issue_type`, `topic`, `assignee` (omitempty), `rank`,
`lane`, `labels`, `created_at`, `updated_at`, `closed_at` (omitempty),
`resolution` (omitempty), `redirect_target` (omitempty), `archived_at`
(omitempty), `deleted_at` (omitempty). Notably: no `progress` key (pinned by
`TestIssueJSONOmitsProgress`, `model_test.go`).

`IssueWireFields() []string` — `model.go`: reflects over `issueJSON`,
cuts each `json` tag at the first comma; skips fields tagged `-`; falls back to
the Go field name when the tag name is empty. Pinned by
`TestIssueWireFieldsCoverMarshalOutput`, `model_test.go`.

`(Issue).MarshalJSON()` — `model.go`:
- `pendingHydration` → error `issue %s requires store hydration`.
- `lifecycle == nil` → error `issue %s has no hydrated lifecycle`.
  Pinned by `TestNilLifecycleIssueMarshalJSONErrors`, `model_test.go`.
- Status fields (`status`, `closed_at`, `resolution`, `redirect_target`) are
  emitted only when the root exposes a status capability — so a
  hydrated epic emits none of them.
- `archived_at`/`deleted_at` come from `lifecycle.RetentionTimestamps(i.Retention())`
.

`(*Issue).UnmarshalJSON(data)` — `model.go`:
- Copies the plain fields and sets `retention` from
  `RetentionFromTimestamps(payload.ArchivedAt, payload.DeletedAt)`.
- If `IssueType.IsContainer()` → sets `pendingHydration = true`,
  `lifecycle = nil` (JSON may never synthesize container lifecycle).
  Pinned by `TestIssueJSONRoundTripEpicRequiresStoreHydration`,
  `model_test.go`.
- Else if `status` is present → `HydrateStatus` with the decoded
  value/closed_at/resolution/redirect_target (cloned). Pinned by
  `TestIssueJSONRoundTripLeafPreservesStatusFields` (`model_test.go`) and
  `TestIssueJSONRoundTripPreservesPrompt`.
- Else → error
  `issue %s: cannot hydrate lifecycle from JSON (missing status field on non-epic)`
. Pinned by `TestIssueJSONRejectsLeafWithoutStatus`,
  `model_test.go`.

## 2.8 Other record types — `model.go`

- `Relation` — `model.go`: `SrcID` (`src_id`), `DstID` (`dst_id`),
  `Type RelationType` (`type`), `CreatedAt` (`created_at`), `CreatedBy`
  (`created_by`).
- `Comment` — `model.go`: `ID` (`id`), `IssueID` (`issue_id`), `Body`
  (`body`), `CreatedAt` (`created_at`), `CreatedBy` (`created_by`).
- `Label` — `model.go`: `IssueID` (`issue_id`), `Name` (`name`),
  `CreatedAt` (`created_at`), `CreatedBy` (`created_by`).
- `FieldChange` — `model.go`: `Field` (`field`), `From` (`from`), `To`
  (`to`) — all stringified so the schema is field-agnostic (`model.go`).
- `IssueEvent` — `model.go`: `ID` (`id`), `IssueID` (`issue_id`),
  `Action` (`action,omitempty` — optional intent metadata populated by named
  status transitions, empty for plain field updates, `model.go`),
  `Reason` (`reason`), `Actor` (`actor`), `CreatedAt` (`created_at`),
  `Attribution` (`attribution,omitzero`), `Changes []FieldChange` (`changes`).
- `IssueDetail` — `model.go`: `Issue` (`issue`), `Relations`
  (`relations`), `Comments` (`comments`), `Children` (`children`), `Siblings`
  (`siblings`), `DependsOn` (`depends_on`), `Related` (`related`), `Blocks`
  (`blocks`), `Parent *Issue` (`parent,omitempty`), `RedirectTarget *Issue`
  (`redirect_target,omitempty` — hydrated from the issue's own redirect target,
  never from the relations graph; `Related` carries only manual peer links,
  `model.go`), `Events` (`events`).
- `Export` — `model.go`: `Version int` (`version`), `WorkspaceID`
  (`workspace_id`), `ExportedAt` (`exported_at`), `Issues`, `Relations`,
  `Comments`, `Labels`, `Events`.

## 2.9 `Attribution` — `model.go`

Struct with two unexported fields: `stream`, `workspace` (`model.go`).
Both are documented as opaque by mandate: nothing user-, host- or path-shaped
may be carried, because the database syncs to shared remotes
(`model.go`).

- `NewAttribution(stream, workspace string) Attribution` — `model.go`:
  if **either** is `""`, returns the zero `Attribution{}`; otherwise the complete
  pair. Half pairs collapse silently to "unattributed" (`model.go`).
  Pinned by `TestNewAttributionAdmitsOnlyCompleteOrAbsent`,
  `attribution_test.go`.
- `Stream()` / `Workspace()` — `model.go`.
- `IsZero() bool` — `model.go`: `a == Attribution{}`; consulted by
  `encoding/json` for `omitzero`.
- `Present() bool` — `model.go`: `!IsZero()`.
- `attributionWire` — `model.go`: JSON keys `stream,omitempty` and
  `workspace,omitempty`.
- `MarshalJSON` — `model.go`.
- `UnmarshalJSON` — `model.go`: decodes the wire pair then routes through
  `NewAttribution`, so `{"stream":"x"}` with no workspace decodes to the absent
  pair. Pinned by `TestAttributionDecodeCollapsesAHalfPair`
  (`attribution_test.go`), `TestAttributionSurvivesARoundTrip`,
  `TestUnattributedEventOmitsAttributionEntirely`.

## 2.10 Export v1/v2 decoding — `model.go`

- `v1ExportHistory` (legacy v1 "history" row) — `model.go`: `issue_id`,
  `action`, `from_status`, `to_status`, `reason`, `created_by`, `created_at`.
- `v1EventID(issueID, action, fromStatus, toStatus, createdBy, createdAt)` —
  `model.go`: joins the six values with `"|"` (timestamp formatted as
  `time.RFC3339Nano`), SHA-256s the key, and returns
  `"evt-v1-" + hex(first 8 bytes)` — a 16-hex-char suffix. Identical rows produce
  identical IDs (dedup-safe); any differing field produces a distinct ID.
- `(*Export).UnmarshalJSON` — `model.go`: decodes into a raw struct
  carrying both `events` and `history`. If `raw.Version < 2` **and**
  `len(raw.History) > 0`, each history row is appended to `Events` as an
  `IssueEvent` with `ID = v1EventID(...)`, `Actor = h.CreatedBy`, and exactly one
  `FieldChange{Field: "status", From: h.FromStatus, To: h.ToStatus}`
  (`model.go`). v2+ exports ignore any `history` array.

---

# 3. `internal/issueid`

## 3.1 Constants — `generate.go`
`CollisionProbabilityThreshold = 0.25`; `MinHashLength = 3`;
`MaxHashLength = 8`; `NonceAttempts = 10`;
`Base36Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"` (lowercase base-36).

Slug bounds — `slug.go`: `PrefixMinLength = 3`, `PrefixMaxLength = 12`,
`TopicMinLength = 3`, `TopicMaxLength = 30`.

## 3.2 ID format
`GenerateHashID(prefix, topic, title, description, creator string, createdAt time.Time, length, nonce int) string`
— `generate.go`.
- Content key: `fmt.Sprintf("%s|%s|%s|%s|%d|%d", topic, title, description,
  creator, createdAt.UnixNano(), nonce)` (`generate.go`) — note `prefix` is
  **not** part of the hashed content.
- `sha256.Sum256(content)`; the first `hashBytesForLength(length)` bytes are
  base-36 encoded to exactly `length` characters (`generate.go`).
- Result grammar: `<prefix>-<topic>-<hash>` (`generate.go`), where `hash` is
  `length` characters from `Base36Alphabet`. Deterministic for identical inputs;
  a different nonce yields a different ID (pinned by `TestGenerateHashID`,
  `generate_test.go`, subtests).

`hashBytesForLength(length int) int` — `generate.go`:
`3→2`, `4→3`, `5→4`, `6→4`, `7→5`, `8→5`, any other length `→3`.
Pinned by `TestHashBytesForLength`, `generate_test.go`.

`encodeBase36(data []byte, length int) string` — `generate.go`:
- Interprets `data` as a big-endian unsigned big.Int; repeatedly divmod by 36,
  emitting `Base36Alphabet[remainder]`, then reverses (`generate.go`).
  A zero value produces the empty string from the loop.
- If shorter than `length`: **left-padded** with `'0'` (`generate.go`) — so
  an all-zero hash yields all `'0'` characters.
- If longer than `length`: clamped by taking the **tail** (`value[len-length:]`)
  (`generate.go`).
- Pinned by `TestEncodeBase36`, `generate_test.go` (subtests).

## 3.3 Collision sizing
`CollisionProbability(numIssues, idLength int) float64` — `generate.go`:
birthday bound `1 - exp(-(numIssues²) / (2 · 36^idLength))`. Zero issues → 0
(pinned `generate_test.go`); monotone increasing in `numIssues` and
decreasing in `idLength`.

`ComputeAdaptiveLength(numIssues int) int` — `generate.go`: returns the
smallest `length` in `[MinHashLength=3, MaxHashLength=8]` whose
`CollisionProbability(numIssues, length) <= 0.25`; if none qualifies returns
`MaxHashLength = 8`. Pinned by `TestComputeAdaptiveLength`,
`generate_test.go` (bounds, minimum for small counts,
monotonic non-decreasing, clamp at max).

## 3.4 Slug normalization — `slug.go`

`NormalizeSlug(input string) string` — `slug.go`:
- Lowercases and trims the whole input first (`slug.go`).
- Iterates runes: `a-z` and `0-9` pass through verbatim; **every other rune**
  (including Unicode) is collapsed into a single `-`, with consecutive
  non-alphanumerics producing exactly one dash (`slug.go`).
- Trims leading and trailing `-` from the result (`slug.go`).
- Pinned by `TestNormalizeSlug`, `slug_test.go`.

`NormalizeConfiguredPrefix(input string) (string, error)` — `slug.go`:
- Normalizes via `NormalizeSlug`.
- Empty → error `issue prefix is required` (`slug.go`).
- Longer than `PrefixMaxLength = 12` → **truncated** to 12 bytes, then
  re-trimmed of `-` (so truncation landing on a dash drops it)
  (`slug.go`).
- Shorter than `PrefixMinLength = 3` after that → error
  `issue prefix must be at least 3 characters after normalization`
  (`slug.go`).
- Pinned by `TestNormalizeConfiguredPrefix`, `slug_test.go` (subtests,
).

`NormalizeTopicForCreate(input string) (string, error)` — `slug.go`:
- Normalizes via `NormalizeSlug`.
- Empty → error `topic is required` (`slug.go`).
- `< TopicMinLength = 3` → error
  `topic must be at least 3 characters after normalization` (`slug.go`).
- `> TopicMaxLength = 30` → error (**rejected, not truncated** — unlike prefix)
  `topic must be at most 30 characters after normalization` (`slug.go`).
- Pinned by `TestNormalizeTopicForCreate`, `slug_test.go` (subtests,
).

---

# 4. `internal/rank`

Package purpose: lexicographic fractional indexing (`rank.go`).

## 4.1 Alphabet and ordering
`alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"` —
`rank.go`; `base = 62` — `rank.go`. Ordinal position of each character IS
its index in that string, so **plain byte-wise string comparison matches rank
order** (digits < uppercase < lowercase, matching ASCII). `charIndex [256]int`
maps byte → ordinal, `-1` for non-members, built in `init()` (`rank.go`).

`Initial() string` — `rank.go`: `alphabet[base/2]` = `alphabet[31]` = `"V"`.

`Valid(s string) bool` — `rank.go`: false for `""` (empty means
"unranked", not a stored rank); otherwise true iff every byte is in the
alphabet. Explicitly NOT the input contract of `Midpoint`/`Before`/`After`,
which accept an empty bound as a sentinel (`rank.go`). Pinned by
`TestValid`, `rank_test.go`, with cases: `""`→false, `"V"`→true, `"0z"`→true,
`"aZ09"`→true, `"-"`→false, `"V!"`→false, `" "`→false, `"hello world"`→false
(`rank_test.go`).

`Significant(s string) string` — `rank.go`: `strings.TrimRight(s, "0")`, the
part that decides where `s` sorts once padded. Two ranks with the same
significant part leave `SpacedRanksBetween` and `Midpoint` no room between them
(`rank.go`). Pinned by `TestSignificant`, `rank_test.go`: `"V"`, `"V0"`
and `"V00"`→`"V"`, `"V0z"`→`"V0z"`, `"100"`→`"1"`, `"0"`→`""`, `""`→`""`.

## 4.2 `Midpoint(a, b string) (string, error)` — `rank.go`
Contract: returns a string strictly between `a` and `b`. Either bound may be
empty: empty `a` = "before everything", empty `b` = "after everything"; both
empty is the whole keyspace, whose midpoint is `Initial()`'s `"V"`
(`rank.go`).

Validation:
- `a == b` with `a` non-empty → error `rank: a and b are equal`
  (`rank.go`). `Midpoint("", "")` passes this check and the walk returns
  `"V"`.
- both non-empty and `a >= b` → error `rank: a must be less than b`
  (`rank.go`).
- `b` non-empty and `Significant(a) == Significant(b)` → error wrapping
  `ErrNoRoom` (`errors.New("rank: no room between the bounds")`, `rank.go`):
  `%w: %q and %q pad to the same value` (`rank.go`). This covers `b`
  being `a` extended by zeros (`"10"`, `"100"`) and an empty `a` with an
  all-zero `b`. An empty `b` never matches.
- an out-of-alphabet byte in `a` → `rank: invalid character in a` (`rank.go`);
  in `b` → `rank: invalid character in b` (`rank.go`).

Algorithm (`rank.go`), per position `i` from 0:
- `aChar` = `charIndex[a[i]]` if `i < len(a)`, else the virtual value `0`
  ("below the alphabet floor") (`rank.go`).
- `bChar` = `charIndex[b[i]]` if `i < len(b)`, else the virtual value `base = 62`
  ("above the ceiling") (`rank.go`).
- If `bChar - aChar > 1`: emit `alphabet[aChar + (bChar-aChar)/2]` and return
  (`rank.go`).
- Else (adjacent or equal): emit `alphabet[aChar]` and advance to the next
  position (`rank.go`), looping unconditionally.

Consequences pinned by tests: adjacent characters force a longer result
(`TestMidpointAdjacentChars`, `rank_test.go`); `Midpoint("B","A")` and
`Midpoint("A","A")` error (`TestMidpointErrors`); empty-bound behavior
(`TestMidpointEmptyBounds`); repeated insertion stays strictly ordered
(`TestRepeatedMidpointInsertion`); multi-char strings
(`TestMidpointMultiCharStrings`); over every ordered pair of distinct
strings of up to four characters from `0`, `1`, `y`, `z`, the empty string
included as either open end, and over the pair of two empty strings,
`Midpoint` returns `ErrNoRoom` exactly for the pairs sharing a significant part
and a valid rank strictly between the bounds for every other pair
(`TestMidpointStaysStrictlyBetweenItsBounds`); `Midpoint("", "")`
returns `Initial()` (`TestMidpointOfTheWholeKeyspaceIsInitial`).

`Before(a string) (string, error)` — `rank.go`: an empty `a` → error
`rank: Before needs a rank, not the empty string` (`rank.go`); otherwise
returns `Midpoint("", a)`, error included, so it fails with `ErrNoRoom` on an
all-zero `a` (`TestBeforeAnAllZeroRankHasNoRoom`, `rank_test.go`, for `"0"`
and `"000"`).
`After(a string) string` — `rank.go`: `Midpoint(a, "")`; **panics**
`rank.After called with empty string` when `a` is empty or `Midpoint` errors
(`rank.go`). `TestBeforeAndAfterRefuseTheEmptyRank` (`rank_test.go`)
requires `Before("")` to return an error and `After("")` to panic. 1000-step
monotonicity pinned by `TestSequentialAfter` (`rank_test.go`) and `TestSequentialBefore`.

## 4.3 Smoothing constants
`SmoothingThreshold = 8` — `rank.go`: the rank string length that triggers
local smoothing; normal ranks are 1–6 chars (`rank.go`).
`SmoothingWindow = 32` — `rank.go`: number of items re-spaced during local
smoothing.

## 4.4 Spaced-rank generation

`SpacedRanks(n int) []string` — `rank.go`: `spacedRanks(n, "", "")`
across the full keyspace; **panics** `rank: spaced ranks with empty bounds
failed: %v` on error (pinned for negative n by `TestSpacedRanksPanicsOnNegativeN`,
`rank_test.go`).

`SpacedRanksBetween(lower, upper string, n int) ([]string, error)` —
`rank.go`: both bounds non-empty with
`lower >= upper` → error `rank: lower must be less than upper`, whatever `n`
is; otherwise delegates. That guard is not the whole precondition — it admits
bounds that nothing can sort between, such as `"10"` and `"100"` — and the rest is
enforced in `spacedRanks` below. Pinned by `TestSpacedRanksBetween`
(`rank_test.go`), `TestSpacedRanksBetweenEdges`,
`TestSpacedRanksBetweenAllowsFurtherMidpoints`,
`TestSpacedRanksBetweenLongLowerBound`,
`TestSpacedRanksBetweenRejectsNegativeN`,
`TestSpacedRanksBetweenRejectsBoundsWithNoRoom`,
`TestSpacedRanksBetweenRejectsInvertedBoundsForAnyN`.

`spacedRanks(n int, lower, upper string) ([]string, error)` — `rank.go`:
- `n < 0` → error `rank: n must be non-negative`; `n == 0` →
  `(nil, nil)`.
- `minGap = 16` (`rank.go`); `denominator = n+1`.
- Starts at `length = max(len(lower), len(upper)) + 1` and increments until a
  length works.
- For each candidate length: `lo = lowerBoundInt(lower, length)`,
  `hi = upperBoundInt(upper, length)`; `span = hi - lo`. A **negative** span →
  error wrapping `ErrNoRoom`: `%w: %q and %q pad to the same value, so no rank
  longer than both sorts between them`; `step = span / (n+1)`;
  if `step < 16` try the next length.
- The negative-span arm terminates the search rather than continuing it because
  every emitted string is longer than both bounds, so `lo` and `hi` are the two
  bounds right-padded with `'0'` and `span(L+1) = 62*(span(L)+1) - 1`. Bounds
  that pad to the same value give `span = -1`, which maps to `-1` at every
  greater length; a non-negative span instead grows 62-fold per length, so it is
  the only case that could not terminate and it is decided at the first length
  tried. Since `lower < upper` holds by the caller's guard, a negative span is
  always exactly `-1`.
- Emits `out[i] = encodeBase62(lo + step*(i+1), length)` for `i` in `[0, n)` —
  all outputs are the **same fixed width** (pinned by
  `TestSpacedRanksUniformLength`, `rank_test.go`) and strictly increasing
  (`TestSpacedRanksOrdering`), with room for further midpoint insertion
  (`TestSpacedRanksAllowMidpointInsertion`).

`lowerBoundInt(s string, length int) (*big.Int, error)` — `rank.go`:
empty `s` → `0`; else `stringToInt(s, length)`, plus 1 when `len(s) >= length`
(padding already makes the value `> s` when shorter).

`upperBoundInt(s string, length int) (*big.Int, error)` — `rank.go`:
empty `s` → `pow62(length)` (absolute maximum); else `stringToInt(s, length) - 1`,
with no check of its own. An all-zero upper bound yields `-1`, which `spacedRanks`
reads as the negative span it already reports, so one place decides that a pair of
bounds admits nothing.

`stringToInt(s string, length int) (*big.Int, error)` — `rank.go`:
base-62 accumulate over `length` positions, right-padding with index 0 (`'0'`);
an out-of-alphabet byte → error `rank: invalid character in bounds`.

`pow62(n int) *big.Int` — `rank.go`.

`encodeBase62(value *big.Int, length int) (string, error)` — `rank.go`:
negative value → `rank: cannot encode negative value`; remainder outside
`[0, 62)` → `rank: base62 remainder out of range`; a value that does not fit the
fixed width (non-zero quotient after `length` digits) →
`rank: value does not fit fixed-width encoding`.

---

# 5. `internal/precedence`

Package doc — `precedence.go`: the one definition of "first non-empty
candidate in order"; deliberately has no trim mode (trimming is the value's own
property, enforced where produced — see `pathspec`).

`First(candidates ...string) string` — `precedence.go`: returns the first
candidate `!= ""`, or `""` if all are empty (or the list is empty). Pinned by
`TestFirstResolvesOrderedCandidates`, `precedence_test.go`.

---

# 6. `internal/pathspec`

Package doc — `pathspec.go`: the value is whitespace-trimmed and absence is
the zero value; trimming happens exactly once in `New`.

- `type PathSpec struct { value string }` — `pathspec.go`. Zero value is
  the absent path.
- `New(raw string) PathSpec` — `pathspec.go`: `strings.TrimSpace(raw)`;
  whitespace-only input yields the absent PathSpec. Pinned by `TestNewTrims`,
  `pathspec_test.go`.
- `String() string` — `pathspec.go`: the trimmed path, `""` when absent.
- `IsEmpty() bool` — `pathspec.go`. Pinned by `TestIsEmpty`,
  `pathspec_test.go`.
- `Or(fallback PathSpec) PathSpec` — `pathspec.go`: returns the receiver
  when present, otherwise `fallback`. Pinned by `TestOr`, `pathspec_test.go`.
- `Join(elem ...string) PathSpec` — `pathspec.go`: an absent path stays
  absent (returns the receiver untouched, never inventing a relative path);
  otherwise `filepath.Join(value, elem...)`. Pinned by
  `TestJoinPropagatesAbsence`, `pathspec_test.go`.

---

# 7. `internal/annotation`

## 7.1 `ReadinessRole` — `annotation.go`

`type ReadinessRole int`. Complete value set (iota order, `annotation.go`):

| Constant | Value | Meaning |
|---|---|---|
| `roleInvalid` (unexported) | 0 | zero value: never a valid classification |
| `RoleBlocking` | 1 | prevents pulling the issue now |
| `RoleOrphaned` | 2 | staleness signal, not a blocker |
| `RoleRankInversion` | 3 | rank-hygiene signal, not a blocker |
| `RoleNone` | 4 | ordering/advisory only, invisible to readiness |

## 7.2 `Kind` — `annotation.go`

`type Kind struct { def *kindDef }` where `kindDef{key string; role ReadinessRole}`
(`annotation.go`). The zero `Kind` is invalid; only the registry produces
valid kinds.

- `(Kind).ReadinessRole() ReadinessRole` — `annotation.go`: `roleInvalid`
  when `def == nil`.
- `(Kind).String() string` — `annotation.go`: the serialization key, `""`
  for the zero kind. Pinned by `TestKindStringReturnsKey`,
  `annotation_test.go`.
- `(Kind).MarshalJSON()` — `annotation.go`: encodes the key as a JSON
  string; the zero kind errors `marshal annotation kind: invalid kind`. Pinned by
  `TestKindMarshalJSONRejectsInvalidKind`, `annotation_test.go`.
- `(*Kind).UnmarshalJSON(data)` — `annotation.go`: decodes a JSON string,
  looks it up in the registry, and errors `unknown annotation kind %q` on a miss.
  Pinned by `TestKindJSONRoundTrip` (`annotation_test.go`) and
  `TestKindUnmarshalJSONRejectsUnknownKind`.

## 7.3 Registry — `annotation.go`

`kindRegistry map[string]Kind` and `registeredKinds []Kind` (`annotation.go`).

`register(key string, role ReadinessRole) Kind` — `annotation.go`:
**panics** `annotation: kind <key> registered without a readiness role` when
`role == roleInvalid`, and **panics**
`annotation: duplicate kind key <key>` on a repeated key. Otherwise
mints the kind, adds it to the registry and to the declaration-order list.

Complete registered kind set (`annotation.go`):

| Var | Key | Role | Meaning (per source comment) |
|---|---|---|---|
| `MissingField` | `missing_field` | `RoleBlocking` | a required field is empty or unset |
| `OpenDependency` | `open_dependency` | `RoleBlocking` | issue depends on an open ticket |
| `InheritedDependency` | `inherited_dependency` | `RoleBlocking` | an open ticket blocks an epic this issue sits under |
| `RankInversion` | `rank_inversion` | `RoleRankInversion` | dependency is ranked below the dependent |
| `Orphaned` | `orphaned` | `RoleOrphaned` | in_progress with no update past the orphaned threshold |
| `NeedsDesign` | `needs_design` | `RoleBlocking` | carries the needs-design label |
| `EarlierSiblingPending` | `earlier_sibling_pending` | `RoleBlocking` | an earlier same-lane sibling under the parent epic is still open |
| `FocusPath` | `focus_path` | `RoleNone` | a focused goal or a derived prerequisite of one; an ordering signal |

Alias (`annotation.go`): `init()` maps the registry key `"blocked_by"` to
the **same** `OpenDependency` kind — a deserialization alias for data written
before the rename. It is never minted as its own kind.

- `Kinds() []Kind` — `annotation.go`: a fresh copy of the canonical kinds
  in declaration order; aliases excluded. Pinned by `TestKindsExcludesAliases`
  (`annotation_test.go`) and `TestEveryRegisteredKindHasReadinessRole`.
- `parseKind(key string) (Kind, bool)` — `annotation.go`: registry lookup
  (so aliases resolve).

## 7.4 Data types

- `Annotation` — `annotation.go`: `Kind Kind` (`json:"kind"`),
  `Message string` (`json:"message"`).
- `ParentEpicRef` — `annotation.go`: `ID` (`json:"id"`), `Title`
  (`json:"title"`). Present only when the issue has a parent AND that parent is
  type=epic (`annotation.go`).
- `AnnotatedIssue` — `annotation.go`: embeds `model.Issue`, plus
  `Annotations []Annotation` (`json:"annotations"`) and
  `ParentEpic *ParentEpicRef` (`json:"parent_epic,omitempty"`).
  - `MarshalJSON` — `annotation.go`: marshals the embedded issue, decodes
    it into a `map[string]any`, then sets `annotations` and (only when non-nil)
    `parent_epic`, and re-marshals — so the output is the flat issue object plus
    those keys. Pinned by `TestAnnotatedIssueJSONShape`, `annotation_test.go`.
  - `UnmarshalJSON` — `annotation.go`: decodes the same bytes twice, once
    as `model.Issue` and once for the two extra keys.

## 7.5 Annotation pipeline

- `type Annotator func(ctx context.Context, issue model.Issue) ([]Annotation, error)`
  — `annotation.go`.
- `Annotate(ctx, issues []model.Issue, annotators ...Annotator) ([]AnnotatedIssue, error)`
  — `annotation.go`: every annotator runs against every issue
  unconditionally, in argument order; annotations are concatenated in that order;
  the first annotator error aborts and returns `(nil, err)`; when an
  issue accumulates no annotations, the field is set to a **non-nil empty slice**
  `[]Annotation{}`. `ParentEpic` is never populated here.
  Pinned by `TestAnnotateRunsAllAnnotators` (`annotation_test.go`),
  `TestAnnotateEmptyAnnotatorsProducesEmptySlice`,
  `TestAnnotateAnnotatorError`.
- `HasAny(annotations []Annotation, kinds ...Kind) bool` — `annotation.go`:
  true if any annotation's `Kind` equals any of the given kinds (struct equality
  on the `*kindDef` pointer, so registry-minted kinds compare identically, and an
  alias-decoded `blocked_by` equals `OpenDependency`). Pinned by
  `TestHasAnyMatchesKind` (`annotation_test.go`), `TestHasAnyNoMatch`,
  `TestHasAnyEmptyAnnotations`.

Note on consumers (outside this package's scope but determined by the roles
above): `ClassifyReadiness` lives in `internal/cli/ready_state.go` and routes each
annotation by its declared role; its per-kind contract is pinned by
`internal/cli/readiness_test.go` and its exhaustiveness over `Kinds()` by
`internal/cli/readiness_test.go`.

---

# 8. `internal/lawtokens`

## 8.1 `Canonical` token index — `tokens.go`, `canonical_gen.go`

`var Canonical = newMarkerSet(canonicalKeys...)` — a membership set keyed by the
**full** `"NAMESPACE:token"` string, so a right-token/wrong-namespace citation is
non-canonical by construction (`tokens.go`).

`canonicalKeys` is declared in the generated file `canonical_gen.go`, whose
header comment marks it as generated by `tools/lawtokens-sync` (DO NOT EDIT)
and names `tokenindex.UpstreamURL` as its source. Its keys, in upstream order,
2 FRAMING + 21 LAW:

FRAMING: `FRAMING:parts-and-seams`, `FRAMING:representation`.

LAW: `LAW:decomposition`, `LAW:types-are-the-program`, `LAW:composability`,
`LAW:carrying-cost`, `LAW:polishing-by-subtraction`,
`LAW:no-ambient-temporal-coupling`, `LAW:effects-at-boundaries`,
`LAW:one-source-of-truth`, `LAW:single-enforcer`, `LAW:comments-carry-meaning`,
`LAW:dataflow-not-control-flow`, `LAW:one-type-per-behavior`,
`LAW:no-mode-explosion`, `LAW:parse-dont-validate`,
`LAW:no-defensive-null-guards`, `LAW:locality-or-seam`, `LAW:one-way-deps`,
`LAW:no-shared-mutable-globals`, `LAW:verifiable-goals`,
`LAW:behavior-not-structure`, `LAW:no-silent-failure`.

- `type markerSet map[string]struct{}` — `tokens.go`;
  `newMarkerSet(keys ...string) markerSet` — `tokens.go`.
- `(markerSet).Has(key string) bool` — `tokens.go`: exact membership.
- `(markerSet).Sorted() []string` — `tokens.go`: keys sorted with
  `sort.Strings`, for diagnostics.

## 8.2 `Marker` and scanning — `markers.go`

`type Marker struct { Namespace string; Token string; Line int }` —
`markers.go`; `Line` is 1-based.
- `(Marker).Key() string` — `markers.go`: `Namespace + ":" + Token`.
- `(Marker).String() string` — `markers.go`: `"[" + Key() + "]"`.

`markerPattern` — `markers.go`: regexp `\[(FRAMING|LAW):([^\]\n]+)\]`, its
namespace alternation built by `namespaceAlternation` (`markers.go`) from
`tokenindex.Namespaces()` (8.3), in that table's order. The
token group is captured loosely (any run of non-`]`, non-newline characters) on
purpose, so a miscased/malformed token is still *recognized* as a marker and can
be reported as non-canonical rather than silently unmatched
(`markers.go`).

`ScanMarkers(content string) []Marker` — `markers.go`: splits `content` on
`"\n"`, finds all matches per line, and returns markers in order with their
1-based line numbers. Pure — no IO, no globals. Returns nil when there are no
matches. Pinned by `TestScanMarkersRecognizesShapeRegardlessOfCanonicity`,
`markers_test.go`.

`NonCanonical(markers []Marker) []Marker` — `markers.go`: the subset whose
`Key()` is absent from `Canonical`; nil when all are canonical. Pinned by
`TestNonCanonicalRejectsExactlyTheDrift` (`markers_test.go`),
`TestEveryCanonicalKeyIsAccepted`, and the repo-wide gate
`TestRepoMarkersAreCanonical`, which passes every file `git ls-files`
lists to `CheckFiles` (8.4) and fails with `Report`.

## 8.3 Upstream index parsing — `internal/lawtokens/tokenindex`

Package `tokenindex` imports no other package of this module; `lawtokens`
imports it, and `tools/lawtokens-sync` imports it but not `lawtokens`
(pinned by `TestToolDoesNotCompileTheFileItWrites`,
`tools/lawtokens-sync/main_test.go`).

`UpstreamURL` — `tokenindex.go`:
`https://raw.githubusercontent.com/promptctl/laws/master/plugins/laws/skills/code/SKILL.md`.

`namespaces` — `tokenindex.go`, returned by name through
`Namespaces() []string`: `FRAMING` under the paragraph header prefix
`Framings (`, `LAW` under `Laws (`.

`type Index struct { keys []string }` — `tokenindex.go`; `(Index).Keys()`
 returns a copy of the keys in upstream order.

`Parse(doc string) (Index, error)` — `tokenindex.go`. Reads the lines
after the single `## The token index` heading up to the next line starting with
`#` or equal to `---`, and splits them into blank-line-separated paragraphs. A
paragraph whose first line does not start with a known namespace header is
prose and is skipped, unless it contains a backtick. A namespace paragraph's
header line must end with `:`; its remaining lines may hold only backticked
spans and `·` separators, and each span is a bare token or `[NS:token]` in that
paragraph's namespace, with the token matching `^[a-z]+(-[a-z]+)*$`. Errors: no
heading; heading twice; a backtick in a non-namespace paragraph; text after a
header's colon; namespace listed twice; text outside spans; unclosed marker;
malformed token; a namespace with no tokens; a key listed twice. Keys are
returned FRAMING first, then LAW. Pinned by
`TestParseReadsBothNamespacesInUpstreamOrder` (`tokenindex_test.go`) and
`TestParseRefusesAnIndexItCannotReadWhole`.

`(Index).Render() []byte` — `tokenindex.go`: the Go source of
`canonical_gen.go`, one quoted key per line. Pinned by
`TestRenderListsKeysInOrderAsGoSource` (`tokenindex_test.go`).

`tools/lawtokens-sync` fetches `tokenindex.UpstreamURL`, parses and renders it, and
writes `internal/lawtokens/canonical_gen.go`; with `-check` it exits 1 when the
file differs from the rendered bytes, naming keys added upstream and keys no
longer upstream. A missing file, or one that does not parse as Go, is
rewritten like any other stale file. `.github/workflows/nightly.yml` runs the
`-check` form.

## 8.4 Checking files — `check.go`, `tools/lawtokens-check`

`type Violation struct { Path string; Marker Marker }` — `check.go`;
`(Violation).String()` renders `path:line: [NAMESPACE:token]`.

`CheckFiles(fsys fs.FS, paths []string) ([]Violation, error)` — `check.go`:
reads each path from `fsys` in the order given and returns the
`NonCanonical(ScanMarkers(content))` markers of each as violations. A path that
cannot be read returns an error naming it and no violations. Pinned by
`TestCheckFilesNamesEveryInventedTokenByFileAndLine` and
`TestCheckFilesRefusesAFileItCannotRead` (`check_test.go`).

`Report(violations []Violation) string` — `check.go`: the count, one
indented line per violation, then the remediation text naming
`tokenindex.UpstreamURL` and `just lawtokens-sync`.

`tools/lawtokens-check` takes file paths as arguments. `fsNames`
(`main.go`) resolves each against the working directory into a clean,
slash-separated name inside it and refuses a path outside it. The tool exits 0
when `CheckFiles` finds no violation, and 1 after printing `Report`, a read
error or the refused path to stderr. `.pre-commit-config.yaml`
declares it as the local hook `lawtokens` (`entry: go run
./tools/lawtokens-check`, `language: system`, `types: [text]`), which the
pre-commit framework runs with the staged text files.

---

# 9. `internal/query`

The query grammar produces a `storage.ListIssuesFilter`
(`internal/storage/issues.go`) whose fields are: `Statuses
[]model.State`, `Resolutions []model.Resolution`, `IssueTypes
[]model.IssueType`, `ExcludeIssueTypes []model.IssueType`, `Assignees
[]string`, `SearchTerms []string`, `IDs []string`, `HasComments *bool`,
`LabelsAll []string`, `UpdatedAfter *time.Time`, `UpdatedBefore *time.Time`,
`IncludeArchived bool`, `IncludeDeleted bool`, `SortBy []SortSpec`, `Limit int`.
A listing that says nothing about retention sees only live issues
(`internal/storage/issues.go`).

## 9.1 `ParseResult` and `Parse`
- `type ParseResult struct { Filter storage.ListIssuesFilter }` — `query.go`.
- `Parse(input string) (ParseResult, error)` — `query.go`: trims the input,
  tokenizes, then applies each term to a fresh zero filter; the first term error
  aborts with an empty `ParseResult`. `Parse` does **not** run `validateFilter`.
  Pinned by `TestParseBuildsFilterFromQueryExpression`, `query_test.go`.

## 9.2 Tokenizer — `query.go`
- Empty input → `(nil, nil)`.
- Whitespace separators: space, `\n`, `\t`.
- Quoting: both `"` and `'` open a quoted run; the quote characters are dropped
  from the token; a run continues until the *same* quote character
. Quoted content may contain separators.
- Unterminated quote → error `unterminated quote in query`.
- No escape sequences exist.

## 9.3 Term grammar — `applyTerm`, `query.go`

| Term prefix | Behavior | Cites |
|---|---|---|
| `status:<v>[,<v>...]` | `model.ParseStates` (comma-split, each fragment lowercased with the `in-progress` alias); appended to `Statuses`; parse error propagates | |
| `resolution:<v>` | `model.ParseResolution` (trim only); appended to `Resolutions` | |
| `type:<v>[,<v>...]` | `model.ParseIssueTypes`; appended to `IssueTypes`; a typo or blank member is an error, never an empty result | |
| `assignee:<v>` | value trimmed, appended to `Assignees` (no validation, empty allowed) | |
| `id:<v>[,<v>...]` | `storage.ParseNames`: comma-split, fragments trimmed; appended to `IDs`; any blank slot → `storage.ValidationError` | |
| `parent:<v>[,<v>...]` | `storage.ParseNames`, as `id:`; appended to `ParentIDs` | |
| `label:<v>[,<v>...]` | `storage.ParseNames`, as `id:`; appended to `LabelsAll` (AND semantics) | |
| `has:comments` | sets `HasComments` to `true` via `mergeBoolPointer("has-comments", …)` | |
| `has:<other>` | error `unsupported has: filter %q` (quoting the **whole** term) | |
| `sort:<expr>` | `storage.ParseSortSpecs`; specs appended to `SortBy` | |
| `limit:<n>` | `strconv.Atoi`; non-integer → `limit must be an integer, got %q`; negative → `limit must be non-negative, got %q`; **0 is legal** and means uncapped | |
| `archived` (exact) | sets `IncludeArchived = true` | |
| `deleted` (exact) | sets `IncludeDeleted = true` | |
| `updated<expr>` | delegates to `applyTimeTerm` with the remainder after `"updated"` | |
| anything else | appended verbatim to `SearchTerms` (free-text) | |

Prefix matching is `strings.HasPrefix` and the branches are evaluated in the
order listed, so e.g. `updatedfoo` routes to the time branch.

Pinned by: `TestQueryTokenSupersetOfDiscreteFlags` (`query_test.go`),
`TestQueryMultiTokenAppliesAllFourNewTokens`,
`TestQueryLimitRejectsNonInteger`, `TestQuerySortRejectsBadDirection`
, `TestQueryLimitRejectsNegative`,
`TestQueryLimitZeroIsUncappedNotRejected`,
`TestParseRejectsInvalidStatus`, `TestParseRejectsInvalidType`,
`TestStatusAliasInProgressNormalizesToBeadsValue`.

### `updated` sub-grammar
`applyTimeTerm(filter, expr)` — `query.go`:
- `splitComparator(expr)` — `query.go`: recognized comparator prefixes,
  tried in this order: `>=`, `<=`, `>`, `<`, `:`. Missing comparator → error
  `missing comparator`; empty payload after the comparator → `missing value`;
  both are wrapped as `parse updated term "updated<expr>": <err>`.
- Timestamp parsed as `time.RFC3339`, falling back to `time.RFC3339Nano`; on
  failure error `updated timestamp must be RFC3339`.
- `>=` and `>` both set `UpdatedAfter`; `<=` and `<` both set `UpdatedBefore`
  (inclusive/exclusive is not distinguished).
- The `:` comparator parses but then falls to the default arm and errors
  `updated supports only >=, >, <=, <`.

## 9.4 `Merge(base, incoming storage.ListIssuesFilter) (storage.ListIssuesFilter, error)` — `query.go`
- `Statuses`: dedup-merged, no re-validation — both sides are already
  `[]model.State`, minted by `model.ParseStates` at the flag and grammar
  boundaries.
- `Resolutions`: plain append, no dedup (duplicates are absorbed downstream by
  the store's allow-map).
- `IssueTypes`, `Assignees`: dedup-merged.
- `SearchTerms`, `IDs`, `LabelsAll`: plain append.
- `HasComments`: `mergeBoolPointer("has-comments", …)` — conflicting non-equal
  values error `conflicting has-comments filters`.
- `UpdatedAfter`/`UpdatedBefore`: `mergeTimePointer` — non-equal values error
  `conflicting updated-after filters <RFC3339> and <RFC3339>` (same shape for
  `updated-before`); the stored value is converted to UTC.
- `Limit`: `incoming.Limit` wins only when `> 0`.
- `SortBy`: dedup-merged, flag-supplied (base) keys first, then query keys;
  distinct keys (including same field, different direction) stay in order,
  forming one multi-key ordering (pinned by
  `TestMergeSortByDedupsExactDuplicates`, `query_test.go`).
- `IncludeArchived`/`IncludeDeleted`: plain boolean OR — visibility is monotonic,
  no conflict detection.
- Returns `validateFilter(filter)` as the error.
- `ExcludeIssueTypes` is **not** merged at all (absent from `Merge`).
- Pinned by `TestMergeMultipleStatusesCombines`, `query_test.go`.

## 9.5 Helpers
- Statuses merge through `mergeSlice` like every other filter slice. Both sides
  are already `[]model.State`, a type only `model.ParseStates` mints at the flag
  and grammar boundaries, so `Merge` re-parses nothing; nil survives as nil
  because `mergeSlice` returns `base` untouched when `incoming` is empty.
- `mergeSlice[T comparable](base, incoming []T) []T` — `query.go`: if
  `incoming` is empty, returns `base` unchanged (nil stays nil); otherwise
  returns a new slice = base plus incoming values not already present in base.
- `validateFilter(filter)` — `query.go`: errors
  `updated-after cannot be greater than updated-before` when both are set and
  `UpdatedAfter.After(*UpdatedBefore)`.

## 9.6 Sort expression grammar (`storage.ParseSortSpecs`, consumed by `sort:`)
`internal/storage/sort.go`: comma-separated; each part trimmed; empty parts
skipped; a bare field defaults to ascending; `field:asc` / `field:desc`
(direction lowercased and trimmed); any other direction →
`ValidationError{Message: "unsupported sort direction \"<d>\""}`. Splitting is
`SplitN(spec, ":", 2)`. If nothing was produced, returns `(nil, nil)`. Field
names are not validated here.

---

# 10. `internal/trace`

Package doc — `trace.go`: owns only filename/collision mechanics (directory
layout, unique id minting, atomic create-exclusive write, retry on collision) —
never the record shape.

- `Dir(storageDir, kind string) string` — `trace.go`:
  `filepath.Join(storageDir, "traces", kind)`.
- `Write(storageDir, kind, slug string, build func(id string, recordedAt time.Time) ([]byte, error)) (id, path string, err error)`
  — `trace.go`:
  - `os.MkdirAll(dir, 0o755)`; on failure error `create %s trace dir: %w`
.
  - Up to **5** attempts. Each attempt takes a fresh
    `time.Now().UTC()` and forms the candidate id
    `<timestamp>-<slug>` with the timestamp layout
    `20060102T150405.000000000Z` (i.e. `YYYYMMDDThhmmss.nnnnnnnnnZ`); on
    attempts after the first, `-<attempt>` (1..4) is appended.
  - `build(candidate, timestamp)` runs **each attempt**, so the record's stamped
    id/timestamp always matches the file it lands in; a build error
    aborts with `marshal %s trace: %w`.
  - File path is `<dir>/<candidate>.json`, opened
    `O_WRONLY|O_CREATE|O_EXCL` with mode `0o644`.
  - `os.IsExist` → retry the loop; any other open error →
    `create %s trace: %w`.
  - Write error → `write %s trace: %w` (after closing); close error →
    `close %s trace: %w`.
  - Success returns `(candidate, targetPath, nil)`.
  - Exhausting 5 attempts → error `create %s trace: too many id collisions`
.
  - Pinned by `TestWriteWritesUnderKindDirAndStampsIDIntoBuild`
    (`trace_test.go`) and `TestWriteRetriesOnFilenameCollision`.
- `Slug(input string) string` — `trace.go`: lowercase + trim, then replace
  every run matching `[^a-z0-9]+` (`trace.go`) with a single `-`, then trim
  leading/trailing `-`; an empty result becomes the literal `"trace"`. Pinned by
  `TestSlugCanonicalizesAndFallsBackOnEmpty`, `trace_test.go`.

---

# 11. `internal/interrupt`

Package doc — `interrupt.go`: turns SIGINT/SIGTERM into context
cancellation AND guarantees process termination even when in-flight work ignores
cancellation.

- `DefaultGrace = 5 * time.Second` — `interrupt.go`: bounds how long the clean
  cancellation path may run before hard exit; chosen shorter than the inline
  receive's fetch budget and shorter than Docker's 10s / Kubernetes' 30s SIGKILL
  deadlines (`interrupt.go`).
- `interruptSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}` —
  `interrupt.go`. SIGKILL is deliberately absent (`interrupt.go`).

`Guard(parent context.Context, grace time.Duration) (context.Context, func())` —
`interrupt.go`:
- Creates a buffered (cap 1) signal channel, `signal.Notify` for the two
  signals, derives a cancellable context from `parent`, and creates a `done`
  channel.
- Spawns `watch` with `restoreDefault = signal.Stop(sigs)` and
  `escalate = os.Exit(exitCode(sig))`.
- Returns the derived context plus a `stop` func that is idempotent via a
  captured `stopped` bool: on first call it does `signal.Stop`, `cancel()`, and
  `close(done)`; subsequent calls are no-ops. `stop()` must be called
  on the normal exit path.

`watch(sigs, done, cancel, restoreDefault, grace, escalate)` —
`interrupt.go`:
- First `select`: `done` → return immediately, nothing to escalate;
  `sigs` → capture the signal and continue.
- On interrupt: `cancel()`, then `restoreDefault()` so a **second**
  interrupt terminates the process at once via the OS default disposition
.
- Arms `time.NewTimer(grace)` (deferred `Stop`) and a second `select`
: `done` → clean path completed within grace, `main()` exits with
  its own code; `timer.C` → `escalate(sig)`.
- The clean-vs-hard decision is one atomic select in this goroutine, so there is
  no check-then-act window (`interrupt.go`).
- Pinned by `TestWatchFirstInterruptCancels` (`interrupt_test.go`),
  `TestWatchGraceEscalates`, `TestWatchNoEscalateWhenDoneRacesGrace`
, `TestWatchRestoresDefaultDisposition`,
  `TestWatchNormalExitDoesNotEscalate`.

`exitCode(sig os.Signal) int` — `interrupt.go`: `128 + int(signum)` for a
`syscall.Signal` (SIGINT→130, SIGTERM→143); `1` for anything else. Pinned by
`TestExitCode`, `interrupt_test.go`.

---

# 12. Cross-cutting invariants observed

1. Every sealed vocabulary exposes its enumeration as a **fresh slice per call**
   rather than an exported slice variable: `model.IssueTypes()`
   (`issue_type.go`), `lifecycle.Actions()` (`lifecycle.go`),
   `annotation.Kinds()` (`annotation.go`).
2. Parsers differ in normalization: `ParseState` and `ParseIssueType` and
   `ParseAction` lowercase + trim (`lifecycle.go`, `issue_type.go`,
   `lifecycle.go`); `ParseResolution` and `ParseRelationType` trim only
   (`resolution.go`, `relation_type.go`).
3. Illegal sealed-interface values (raw nil / typed-nil pointer variants) panic
   rather than default: `Issue.SetRetention` (`model.go`),
   `lifecycle.Frozen` (`retention.go`), `lifecycle.Retain`
   (`retention.go`), `lifecycle.RetentionTimestamps`
   (`retention.go`), `closeResolution` on a `Close` with no outcome
   (`status_states.go`).
4. Unhydrated lifecycle reads panic on the accessor path
   (`model.go`) but become errors at the JSON boundary
   (`model.go`) and at `Apply` (`model.go`).
5. Container-ness is decided by `IssueType` alone (`issue_type.go`,
   `model.go`), never by the lifecycle shape.
6. Two orthogonal axes: activity (`State`, driven by `StatusAction`) and
   retention (`Retention`, driven by `RetentionAction`); the two action subsets
   partition the sealed `Action` sum, so cross-axis application is
   unrepresentable (`action.go`).
