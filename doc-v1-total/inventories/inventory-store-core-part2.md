## Import / Export / Export-Delta — raw behavioral inventory

Slice: `internal/store/import_export.go`, `import_bulk.go`, `import_tree.go`, `export_delta.go` and their tests. Supporting types cited from `internal/model`, `internal/storage`, `internal/cli`, `internal/syncfile`, `internal/backup` where the shape of the artifact is defined there.

---

## 1. EXPORT

### 1.1 `Store.Export` — what is collected, in what order

`func (s *Store) Export(ctx context.Context) (model.Export, error)` — `internal/store/import_export.go:15`.

It performs five reads and assembles one value (`import_export.go:16-39`):

1. `s.ListIssues(ctx, storage.ListIssuesFilter{Limit: 0, IncludeArchived: true, IncludeDeleted: true})` (`import_export.go:16`). Limit 0 disables the cap (`capLimit` returns the slice unchanged when `limit <= 0`, `internal/store/store.go:767-772`). `IncludeArchived`/`IncludeDeleted` true means neither `i.archived_at IS NULL` nor `i.deleted_at IS NULL` is added to the WHERE clause (`store.go:575-580`), so **archived and soft-deleted issues are exported**. No other filter is set, so the SQL has no WHERE clause at all. Ordering: with no `SortBy` specs, `buildIssueOrderClause` returns `"i.item_rank ASC, i.id ASC"` (`store.go:1737-1740`), so **issues are ordered by rank ascending, ties broken by id ascending**.
2. `s.listAllRelations(ctx)` (`import_export.go:20`) — `SELECT src_id, dst_id, type, created_at, created_by FROM relations ORDER BY created_at ASC` (`store.go:1880`). Ordered by created_at ascending only (no tiebreak).
3. `s.listAllComments(ctx)` (`import_export.go:24`) — `SELECT id, issue_id, body, created_at, created_by FROM comments ORDER BY created_at ASC` (`store.go:1903`).
4. `s.listAllLabels(ctx)` (`import_export.go:28`) — `SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC` (`store.go:1783`).
5. `s.ListAllEvents(ctx)` (`import_export.go:32`) — `queryEvents(ctx, "")`, i.e. `SELECT e.id, e.issue_id, e.action, e.reason, e.actor, e.created_at, e.stream_id, e.workspace_id, c.field, c.from_value, c.to_value FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id ORDER BY e.created_at ASC, e.id ASC, c.field ASC` (`store.go:1946-1961`). The per-change rows are collapsed back into `IssueEvent.Changes`, so **an event's changes are ordered by field name ascending** and events by (created_at, id).

Any read error is returned with a zero `model.Export{}` (`import_export.go:17-35`).

The returned value (`import_export.go:39`):

```go
model.Export{
    Version:     2,
    WorkspaceID: s.workspaceID,
    ExportedAt:  time.Now().UTC(),
    Issues:      issues, Relations: rels, Comments: comments, Labels: labels, Events: events,
}
```

`Version` is the literal `2`. `ExportedAt` is wall-clock UTC at export time.

Export does **not** re-check hydration; the comment at `import_export.go:36-38` states `hydrateIssues` guarantees every issue is hydrated, and `Issue.MarshalJSON` is the boundary that rejects partial values.

### 1.2 The `model.Export` JSON envelope

`internal/model/model.go:755-764`:

```go
type Export struct {
	Version     int          `json:"version"`
	WorkspaceID string       `json:"workspace_id"`
	ExportedAt  time.Time    `json:"exported_at"`
	Issues      []Issue      `json:"issues"`
	Relations   []Relation   `json:"relations"`
	Comments    []Comment    `json:"comments"`
	Labels      []Label      `json:"labels"`
	Events      []IssueEvent `json:"events"`
}
```

No `omitempty` anywhere on the envelope — all eight keys are always emitted, in this order. Slices from `Store.Export` are never nil (each list helper initializes `out := []model.X{}`, e.g. `store.go:1787`, `1885`, `1908`; `hydrateIssues` returns `[]model.Issue{}` for zero rows, `store.go:2253-2255`), so empty tables serialize as `[]`, not `null`.

### 1.3 The serialized issue object

`Issue` has a custom `MarshalJSON` (`model.go:488-533`) that emits the **wire struct `issueJSON`** (`model.go:438-459`), not the in-memory `Issue` (`model.go:80-111`). The wire struct, in emission order:

```go
type issueJSON struct {
	ID          string                `json:"id"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Prompt      string                `json:"prompt,omitempty"`
	Status      *State                `json:"status,omitempty"`
	Priority    Priority              `json:"priority"`
	IssueType   IssueType             `json:"issue_type"`
	Topic       string                `json:"topic"`
	Assignee    string                `json:"assignee,omitempty"`
	Rank        string                `json:"rank"`
	Lane        string                `json:"lane"`
	Labels      []string              `json:"labels"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
	ClosedAt    *time.Time            `json:"closed_at,omitempty"`
	Resolution  *lifecycle.Resolution `json:"resolution,omitempty"`
	RedirectTarget *string            `json:"redirect_target,omitempty"`
	ArchivedAt     *time.Time         `json:"archived_at,omitempty"`
	DeletedAt      *time.Time         `json:"deleted_at,omitempty"`
}
```

Marshal rules (`model.go:488-533`):

- If `i.pendingHydration` → error `"issue %s requires store hydration"` (`model.go:489-491`).
- If `i.lifecycle == nil` → error `"issue %s has no hydrated lifecycle"` (`model.go:492-496`).
- `status`, `closed_at`, `resolution`, `redirect_target` are populated **only when the lifecycle exposes a Status capability** (`model.go:503-510`). Containers (`epic`) expose none, so an epic's JSON object has **no `status`, no `closed_at`, no `resolution`, no `redirect_target` keys at all**.
- `archived_at`/`deleted_at` come from `lifecycle.RetentionTimestamps(i.Retention())` (`model.go:511`); both omitted when nil (a Live issue).
- `Labels` has no `omitempty`, so `"labels": null` appears when the slice is nil, `[]` when empty-non-nil.
- `Priority` is `type Priority int` (`internal/model/priority.go:12`) with constants `PriorityNormal = 0`, `PriorityUrgent = 1` (`priority.go:14-17`) — serializes as a bare **integer**.
- `IssueType` is `type IssueType string` (`internal/model/issue_type.go:16`) with values `"task"`, `"feature"`, `"bug"`, `"chore"`, `"epic"` (`issue_type.go:18-24`) — serializes as a **string**.
- `State` = `lifecycle.State`, a string (`model.go:15`; `internal/model/lifecycle/lifecycle.go:18-23`), values `"open"`, `"in_progress"`, `"closed"`.
- `Resolution` = `lifecycle.Resolution`, a string (`model.go:18`; `internal/model/lifecycle/resolution.go:20-26`), values `"duplicate"`, `"superseded"`, `"obsolete"`, `"wontfix"`.
- `time.Time` fields serialize as Go's RFC3339 with nanoseconds (encoding/json default).

`model.IssueWireFields()` (`model.go:468-486`) derives the wire key list by reflecting over `issueJSON`, skipping `json:"-"` and falling back to the Go field name for an empty tag.

Fully worked leaf issue:

```json
{
  "id": "links-auth-3f2a",
  "title": "Add login",
  "description": "Body text",
  "prompt": "agent instructions",
  "status": "in_progress",
  "priority": 1,
  "issue_type": "task",
  "topic": "auth",
  "assignee": "brandon",
  "rank": "0|hzzzzz:",
  "lane": "alpha",
  "labels": ["reviewed", "urgent"],
  "created_at": "2026-08-27T10:00:00.123456789Z",
  "updated_at": "2026-08-27T11:00:00Z",
  "closed_at": "2026-08-27T12:00:00Z",
  "resolution": "duplicate",
  "redirect_target": "links-auth-9c11",
  "archived_at": "2026-08-27T13:00:00Z",
  "deleted_at": null
}
```

(In practice `archived_at` and `deleted_at` are mutually exclusive — `retentionColumns`/`RetentionTimestamps` cannot express both, `internal/store/store.go:2401-2405` — and `deleted_at` is omitted entirely rather than `null` when absent.)

Minimal epic (no status axis, Live, no prompt/assignee):

```json
{
  "id": "links-auth-11aa",
  "title": "Auth epic",
  "description": "",
  "priority": 0,
  "issue_type": "epic",
  "topic": "auth",
  "rank": "0|hzzzzz:",
  "lane": "",
  "labels": [],
  "created_at": "2026-08-27T10:00:00Z",
  "updated_at": "2026-08-27T10:00:00Z"
}
```

### 1.4 The other four record shapes

`model.Relation` (`model.go:579-585`) — no omitempty on any field:

```go
SrcID     string       `json:"src_id"`
DstID     string       `json:"dst_id"`
Type      RelationType `json:"type"`
CreatedAt time.Time    `json:"created_at"`
CreatedBy string       `json:"created_by"`
```

```json
{"src_id":"links-a-1","dst_id":"links-b-2","type":"blocks","created_at":"2026-08-27T10:00:00Z","created_by":"links"}
```

`model.Comment` (`model.go:587-593`):

```json
{"id":"cmt-...","issue_id":"links-a-1","body":"text","created_at":"2026-08-27T10:00:00Z","created_by":"tester"}
```

`model.Label` (`model.go:595-600`) — note the JSON key is `name` while the DB column is `label`:

```json
{"issue_id":"links-a-1","name":"urgent","created_at":"2026-08-27T10:00:00Z","created_by":"tester"}
```

`model.IssueEvent` (`model.go:719-728`):

```go
ID          string        `json:"id"`
IssueID     string        `json:"issue_id"`
Action      string        `json:"action,omitempty"`
Reason      string        `json:"reason"`
Actor       string        `json:"actor"`
CreatedAt   time.Time     `json:"created_at"`
Attribution Attribution   `json:"attribution,omitzero"`
Changes     []FieldChange `json:"changes"`
```

`FieldChange` (`model.go:606-610`): `{"field":..,"from":..,"to":..}`, no omitempty.

`Attribution` (`model.go:629-632`) has unexported fields and a custom marshal via `attributionWire` (`model.go:684-692`): `{"stream":"…","workspace":"…"}` with both `omitempty`. `omitzero` on the event field means an absent pair emits **no `attribution` key at all** (`model.go:669-671` — `IsZero` is what encoding/json consults).

```json
{
  "id": "evt-6f4c…",
  "issue_id": "links-a-1",
  "action": "start",
  "reason": "picked up",
  "actor": "agent",
  "created_at": "2026-08-27T10:00:00Z",
  "attribution": {"stream":"s-abc","workspace":"ws-1"},
  "changes": [{"field":"status","from":"open","to":"in_progress"}]
}
```

### 1.5 Export decode (`Export.UnmarshalJSON`) — v1 compatibility

`model.go:794-857`. Decodes into a private `rawExport` that additionally accepts `"history"` (`model.go:797-807`), then copies version/workspace_id/exported_at/issues/relations/comments/labels/events across (`model.go:812-820`).

If `raw.Version < 2 && len(raw.History) > 0` (`model.go:824`), every `v1ExportHistory` row (`model.go:767-775`: `issue_id`, `action`, `from_status`, `to_status`, `reason`, `created_by`, `created_at`) is converted into an `IssueEvent` (`model.go:825-835`) with:
- `ID` = `v1EventID(...)` = `"evt-v1-" + hex(sha256(issueID|action|fromStatus|toStatus|createdBy|createdAt.RFC3339Nano)[:8])` — 16 hex chars after the prefix (`model.go:780-784`).
- `Actor` = the v1 `created_by`.
- `Changes` = exactly one `{"field":"status","from":<from_status>,"to":<to_status>}` (`model.go:833`).

These are **appended** to any already-present `events`.

Issue decode (`Issue.UnmarshalJSON`, `model.go:536-577`): fields copied straight through; `retention` from `lifecycle.RetentionFromTimestamps(archived_at, deleted_at)` (`model.go:554`); then a three-way dispatch (`model.go:556-575`):
- container type → `pendingHydration = true`, `lifecycle = nil` (so it cannot be re-marshaled until the store hydrates it),
- non-container with `status` present → `HydrateStatus` with `closed_at`/`resolution`/`redirect_target`,
- non-container with **no** `status` → error `"issue %s: cannot hydrate lifecycle from JSON (missing status field on non-epic)"` (`model.go:573`).

Attribution decode collapses a half pair to the zero value via `NewAttribution` (`model.go:711-718`, `model.go:656-661`): stream-without-workspace or workspace-without-stream becomes "unattributed", silently.

### 1.6 On-disk layout of exported artifacts

Three write surfaces consume `Store.Export`:

**(a) `lit export` to stdout** — `internal/cli/cli.go:1514-1525`. No flags other than the shared set; `writeJSON(stdout, export)` (`cli.go:1524`), which is `json.NewEncoder(w)` with `SetIndent("", "  ")` then `Encode` (`cli.go:1800-1804`). So: **two-space indent, one trailing newline** (Encoder.Encode appends `\n`), single JSON object, HTML escaping on (encoder default). Comment at `cli.go:1523`: "Export is JSON-only — there is no text representation of a full database export."

**(b) Backup snapshots** — `internal/backup/backup.go`. `Create(storageDir, export)`:
- directory `filepath.Join(storageDir, "backups")`, created with mode `0o755`.
- filename `time.Now().UTC().Format("20060102-150405.000000000") + ".json"` → e.g. `20260827-142530.123456789.json`.
- written via `syncfile.WriteAtomic`.
- returns `Snapshot{Path, Name, Created (mtime UTC), Size}` with tags `json:"path"`, `"name"`, `"created"`, `"size"`.
- `List` reads that dir, skips directories and any entry not ending in `.json`; a missing dir returns `[]Snapshot{}` and no error.
- `Prune(storageDir, keep)` errors on `keep <= 0` (test `internal/backup/backup_test.go:85-90`); `runBackupCreate` defaults `--keep` to `20` (`internal/cli/backup.go:34`), and `restoreFromExportPath` hardcodes `backup.Prune(dir, 20)` (`cli/backup.go:160`).

**(c) Sync file / last-sync base** — `internal/syncfile/syncfile.go`. `WriteAtomic(path, export)`:
- `marshalExport` = `json.MarshalIndent(export, "", "  ")` **plus a trailing `'\n'`** (`syncfile.go:66-72`).
- `os.MkdirAll(dir, 0o755)`, `os.CreateTemp(dir, ".links-sync-*.json")`, write, close, `os.Rename` onto the clean path (`syncfile.go:20-39`); the temp file is removed on any failure path via `defer`.
- returns `hashPayload(payload)` — the content hash of the bytes written.
- The sync base lives at `filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")` (`internal/cli/backup.go:118-120`).

There is **no manifest or index file**: `backup.List` derives the listing by reading the directory (`backup.go:44-70`).

`lit backup list` prints `"%s %d %s\n"` = name, size, path (`cli/backup.go:61`). `lit backup create` prints `"%s %s\n"` = name, path (`cli/backup.go:47`).

---

## 2. EXPORT DELTA (`export_delta.go`)

### 2.1 What "delta" means here

It is **not** a commit range, checkpoint, or timestamp comparison. It is a pure value diff between **two `model.Export` values held in memory**: `diffExports(prev, next model.Export) exportDelta` (`export_delta.go:142`). `prev` is "what the live tables currently hold"; `next` is "what they must hold". No SQL query is issued to compute it (`export_delta.go:141`: "pure: the SQL lives in applyExportDelta").

Who supplies `prev`:
- `writeExportTx` supplies `model.Export{}` — the empty export — after having deleted every row, making the restore the degenerate "everything is an add" case (`import_export.go:172-179`).
- `spineWriter` owns `landed`, seeded by an actual `Store.Export(ctx)` read of the spine branch in `newSpineWriter` (`internal/store/sync_reconcile.go:628-637`), and advanced to `next` only after a successful landing (`sync_reconcile.go:644-652`). The comment at `export_delta.go:17-20` states the previous export is never taken from a caller's belief.

### 2.2 The delta record types

```go
type tableDelta[K comparable, R any] struct {
	remove []K
	add    []R
}
```
`export_delta.go:32-36`. `empty()` is `len(remove)==0 && len(add)==0` (`export_delta.go:39-41`).

```go
type exportDelta struct {
	issues    tableDelta[string, model.Issue]
	relations tableDelta[relationKey, model.Relation]
	comments  tableDelta[string, model.Comment]
	labels    tableDelta[labelKey, model.Label]
	events    tableDelta[string, model.IssueEvent]
}
```
`export_delta.go:111-117`; `empty()` at `export_delta.go:121-123`. These are unexported Go values — **the delta is never serialized to disk or JSON anywhere**.

Key types (`internal/store/row_deletes.go`): `relationKey{srcID, dstID string; kind model.RelationType}` (`row_deletes.go:35-39`) = the relations PRIMARY KEY; `labelKey{issueID, name string}` (`row_deletes.go:42-45`) = labels PRIMARY KEY. Comments, issues and events key on their `string` id.

### 2.3 Add / modify / delete representation

There is **no "modify"**. `diffTable` (`export_delta.go:78-100`):

```go
for _, row := range live {            // iterate the SLICE, so order is the caller's, not map order
    k := key(row)
    if want, ok := wantedByKey[k]; !ok || !reflect.DeepEqual(persisted(row), persisted(want)) {
        delta.remove = append(delta.remove, k)
    }
}
for _, row := range wanted {
    if have, ok := liveByKey[key(row)]; !ok || !reflect.DeepEqual(persisted(have), persisted(row)) {
        delta.add = append(delta.add, row)
    }
}
```

So: **delete** = key in `remove` only; **add** = row in `add` only; **modify** = the same key appears in BOTH `remove` and `add` (delete-then-reinsert). Comment at `export_delta.go:34-36`: this is why no UPDATE statement exists.

Comparison is `reflect.DeepEqual` over a `persisted(row)` projection:
- issues → `issueRowValues(i)` (`export_delta.go:147`), the normalized `[]any` row tuple, *not* the model value.
- relations, comments, labels, events → `wholeRow` (identity) (`export_delta.go:107`, used at `:157`, `:161`, `:165`, `:169`).

Determinism: iteration is over the input slices, not maps (`export_delta.go:46-50`), so identical inputs produce an identical statement sequence.

### 2.4 The cascade rule

`diffExports` computes the issues diff **first**, then `survivors := cascadeSurvivors(prev.Issues, issues.remove)` (`export_delta.go:145-148`). `cascadeSurvivors` (`export_delta.go:189-201`) builds `doomed` from the removal keys and returns the complement over `prev.Issues` — survival is read off the issues diff, never recomputed.

Each child table's `live` side is then `prev`'s rows **filtered to survivors** (`filterRows`, `export_delta.go:203-211`):
- relations survive iff `survivors[r.SrcID] && survivors[r.DstID]` (`export_delta.go:153`) — either endpoint dying kills the edge.
- comments: `survivors[c.IssueID]` (`export_delta.go:159`).
- labels: `survivors[l.IssueID]` (`export_delta.go:163`).
- events: `survivors[e.IssueID]` (`export_delta.go:167`).

Nested `issue_event_changes` get no layer: an event whose `Changes` differ is a changed value under `wholeRow`, so removed+re-added, and its change rows cascade with it (`export_delta.go:137-139`).

### 2.5 Application order and SQL

`applyExportDelta(ctx, tx, delta)` (`export_delta.go:217-231`) runs five table deltas in this fixed order — **issues, relations, comments, labels, events** — and within each table **all removes before all adds** (`applyTableDelta`, `export_delta.go:236-258`). The removal row count is discarded (`export_delta.go:248`).

Statements bound per table:

| table | delete | insert |
|---|---|---|
| issues | `DELETE FROM issues WHERE id = ?` (`row_deletes.go:84`) | `insertIssueStmt` (below) |
| relations | `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?` (`row_deletes.go:88`) | `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)` (`internal/store/relations.go:349`) |
| comments | `DELETE FROM comments WHERE id = ?` (`row_deletes.go:94`) | `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)` (`import_export.go:252`) |
| labels | `DELETE FROM labels WHERE issue_id = ? AND label = ?` (`row_deletes.go:98`) | `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)` (`import_export.go:260`) |
| events | `DELETE FROM issue_events WHERE id = ?` (`row_deletes.go:104`) | `INSERT INTO issue_events(...)` + N `INSERT INTO issue_event_changes(...)` (`import_export.go:287`, `:293`) |

Delete error text: `execDelete` wraps as `"delete %s: %w"` and `"delete %s: rows affected: %w"` with subjects `"issue <id>"`, `"relation <src>-><dst> (<type>)"`, `"comment <id>"`, `"label <issue>:<name>"`, `"issue event <id>"` (`row_deletes.go:84-118`).

### 2.6 Where a delta is committed

`replayDeltaOnScratch(ctx, delta, stamp)` (`sync_reconcile.go:583-597`): under `withCommitLock`, `BeginTx` → `applyExportDelta` → `tx.Commit()` → `commitWorkingSetOnce(ctx, stamp)`. Deliberately a **single attempt** with no self-rotating transient retry (`sync_reconcile.go:564-582`); errors bubble to the outer scratch-rebuilding retry. Error texts: `"begin %s tx: %w"` and `"commit %s tx: %w"` with `stamp.Message` interpolated (`sync_reconcile.go:586`, `:594`).

`spineWriter.land` does `DOLT_CHECKOUT <spine branch>` first, unconditionally (`sync_reconcile.go:645-647`, error `"switch to reconcile spine branch %q: %w"`), then `replayDeltaOnScratch(ctx, diffExports(w.landed, next), stamp)`, then advances `w.landed = next` only on success (`sync_reconcile.go:648-652`).

`commitStamp` (`internal/store/commit_lock.go:104-116`): `Message string`, `Date time.Time` (non-zero → `--date`, second-granular), `Author string` (non-empty → `--author "Name <email>"`), `AllowEmpty bool`.

### 2.7 What the delta tests pin

`internal/store/export_delta_test.go`:

- `TestExportDeltaMatchesFullRewriteAcrossEveryChangeShape` (`:27`) drives two stores through the same state sequence — one via `replaceFromExport(..., commitStamp{Message:"rewrite"})`, one via `applyDeltaForTest` — and after every step asserts `reflect.DeepEqual` on `Issues`, `Relations`, `Comments`, `Labels`, `Events` (`assertSameRows`, `:429-446`). The envelope (`workspace_id`, `exported_at`) is explicitly excluded (`:426-428`).
- The state sequence (`buildDeltaScenarioStates`, `:334-405`), named: `"epic with two children"`, `"one field edited on one issue"`, `"comment added"`, `"label added"`, `"relation spanning two issues added"`, `"relation endpoint rewritten"`, `"issue added"`, `"issue removed outright"`. The last is synthesized by `withoutIssue` (`:409-416`) dropping the issue plus every relation touching it and every comment/label/event referencing it — the shape a merge projection yields.
- `TestExportDeltaRewritesARelationChangedOutsideItsKey` (`:69`): changing only `CreatedBy` from `"first"` to `"second"` on a relation with a stable `(a,b,blocks)` key yields exactly `relations.remove == [relationKey{a,b,blocks}]` and `relations.add == [restamped]`, and **zero** issues work.
- `TestExportDeltaReinsertsChildrenOfARewrittenIssue` (`:101`): retitling issue `touched` yields `issues.remove == ["touched"]`, `issues.add == [retitled]`, and for each of relations/comments/labels/events exactly **1 add and 0 removes** — including a `touched -> untouched` spanning relation.
- `TestExportDeltaLeavesAnUnchangedBacklogAlone` (`:153`): `diffExports(export, export).empty()` must be true.
- `TestExportDeltaLeavesTheIssueRowAloneWhenOnlyALabelMoves` (`:185`): adding label `urgent` to the hydrated `Labels` slice plus a labels row → `issues` delta empty, `labels.add == [{IssueID:"a",Name:"urgent"}]`, `labels.remove` empty, and comments/events deltas empty.
- `TestExportDeltaLeavesAnEpicsRowAloneWhenAChildCloses` (`:229`): closing a child moves the epic's hydrated value but not its row → `issues.remove == ["child"]`, `issues.add == [child]`, comments (hanging off the epic) untouched.
- `TestExportDeltaDropsARemovedIssueWithoutResurrectingItsChildren` (`:264`): removing issue `gone` → `issues.remove == ["gone"]`, `issues.add` empty, comments and events deltas both empty.
- Fixture helpers pin that a bare `model.Issue` literal cannot be diffed: `hydratedIssue` uses `model.HydrateStatus` and `hydratedEpic` uses `model.HydrateAllOf` (`:301-320`), because `issueRowValues`' accessors panic on an unhydrated issue (`:287-298`).

---

## 3. IMPORT — `ReplaceFromExport` / `writeExportTx` (`import_export.go`)

### 3.1 Entry point and transaction

`func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) error` → `s.replaceFromExport(ctx, export, commitStamp{Message: "replace from export"})` (`import_export.go:138-140`). The Dolt commit message for a restore is the literal string **`replace from export`**.

`replaceFromExport` (`import_export.go:149-153`) runs `writeExportTx` under `withStampedMutation`, i.e. under the commit lock, inside one `sql.Tx`, followed by `commitWorkingSetOnce`, with the transient-GC retry wrapping the whole staging+versioning unit (`commit_lock.go:156-177`). So it **is transactional** at the SQL level and **does commit** a Dolt commit.

### 3.2 What it does

`writeExportTx` (`import_export.go:172-179`):

```go
for _, table := range []string{"labels", "comments", "relations", "issues"} {
    if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
        return fmt.Errorf("clear %s: %w", table, err)
    }
}
return applyExportDelta(ctx, tx, diffExports(model.Export{}, export))
```

Deletion order is literally `labels, comments, relations, issues`. `issue_events` and `issue_event_changes` are deliberately **not** named — they cascade from `issues` (`import_export.go:168-171`). Error text: `"clear labels: …"`, `"clear comments: …"`, etc.

Because `prev` is the empty export, every row in the input is an add and nothing is a remove.

### 3.3 Accepted input shape

The input is a `model.Export` value. On the CLI path it arrives from `syncfile.Read(path)` → `json.Unmarshal` into `model.Export` (`internal/syncfile/syncfile.go:43-53`), i.e. the shape in §1.2 with the v1 `history` fallback of §1.5. `json.Unmarshal` here does **not** disallow unknown fields — unrecognized top-level keys are silently ignored (`syncfile.go:49`).

Field-by-field parsing/normalization happens in `issueRowValues` (`import_export.go:218-242`), the tuple bound to `insertIssueStmt`:

```sql
INSERT INTO issues(id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```
(`import_export.go:190-191`.)

Value-by-value (`import_export.go:235-241`):

| column | value | rule |
|---|---|---|
| `id` | `issue.ID` | verbatim |
| `title` | `issue.Title` | verbatim |
| `description` | `issue.Description` | verbatim |
| `agent_prompt` | `nullableString(issue.Prompt)` | `""` → SQL NULL (`store.go:2419-2424`) |
| `status` | `statusForStorage(issue)` | leaf → `sql.NullString{string(status.Value), Valid:true}`; container (no Status capability) → **NULL** (`store.go:2238-2243`) |
| `priority` | `model.CanonicalPriority(int(issue.Priority))` | any int ≠ 1 coerces to 0; 1 stays 1 (`priority.go:24-29`). **Never rejects** — legacy out-of-range priorities are coerced so the CHECK constraint cannot fail a restore (`import_export.go:228-233`) |
| `issue_type` | `issue.IssueType` | verbatim, no parse gate on this path |
| `topic` | `issueid.NormalizeSlug(issue.Topic)` then `COALESCE(NULLIF(?, ''), 'misc')` | lowercased, non-`[a-z0-9]` runs collapsed to single `-`, trimmed of `-` (`internal/issueid/slug.go:15-29`); an empty result becomes the literal **`misc`** |
| `assignee` | `issue.AssigneeValue()` | |
| `item_rank` | `issue.Rank` | verbatim |
| `lane` | `issue.Lane` | verbatim |
| `created_at` | `issue.CreatedAt.Format(time.RFC3339Nano)` | |
| `updated_at` | `issue.UpdatedAt.Format(time.RFC3339Nano)` | |
| `closed_at` | RFC3339Nano of `issue.ClosedAtValue()`, else nil | (`import_export.go:219-222`) |
| `resolution` | `nullableResolution(issue.ResolutionValue())` | nil → NULL, else the string (`store.go:2409-2414`) |
| `redirect_target` | `nullableStringPtr(issue.RedirectTargetValue())` | nil → NULL (`store.go:2490-2495`) |
| `archived_at`, `deleted_at` | `retentionColumns(issue)` | projected from the sealed Retention; archived-and-deleted is unrepresentable (`store.go:2401-2405`) |

`insertIssueTx` error text: `"restore issue %s: %w"` (`import_export.go:246`).

Comments (`insertCommentTx`, `import_export.go:251-257`): `id, issue_id, body, created_at (RFC3339Nano), created_by` verbatim; error `"restore comment %s: %w"`.

Labels (`insertLabelTx`, `import_export.go:259-265`): `issue_id, label(=Name), created_at (RFC3339Nano), created_by`; error `"restore label %s:%s: %w"` (issue id, name).

Events (`insertEventTx`, `import_export.go:282-299`):
- `action` binds `nil` when `event.Action == ""`, else the string (`import_export.go:283-286`).
- `created_at` RFC3339Nano.
- `stream_id` = `nullableString(event.Attribution.Stream())`, `workspace_id` = `nullableString(event.Attribution.Workspace())` — **attribution is replayed verbatim from the dump; the restoring checkout never substitutes its own** (`import_export.go:270-281`). No re-validation of the pair here: `Attribution.UnmarshalJSON` already collapsed half pairs.
- error `"restore issue event %s: %w"`.
- Then one `INSERT INTO issue_event_changes(event_id, field, from_value, to_value)` per `event.Changes` entry, with `from`/`to` through `nullableString` (`""` → NULL); error `"restore issue event change %s.%s: %w"` (event id, field).

### 3.4 ID remapping / conflict policy

**None.** IDs are written verbatim; there is no remapping, no dedup, no conflict resolution. Duplicate ids in the input reach the INSERT and fail on the primary key — `export_delta.go:73-77` states this explicitly ("the add loop walks the slice and every duplicate still reaches the INSERT and still fails loudly"). Any failure aborts the transaction (deferred `tx.Rollback`, `commit_lock.go:165`), so the restore is all-or-nothing.

### 3.5 The surrounding CLI restore flow

`restoreFromExportPath` (`internal/cli/backup.go:122-186`), in order: acquire `storage.Sync.Of(ap.Store)` and `storage.Import.Of(ap.Store)` capabilities up front; `syncfile.Read(restorePath)`; `ap.Store.Export(ctx)` for the local state; `syncer.GetSyncState`; if a sync state exists and `--force` was not passed, hash `last-sync-base.json` and compare against `hashExport(localExport)` — mismatch → `MergeConflictError{Message: "restore conflict: local workspace has unsynced changes since last sync base"}` (`backup.go:152-157`); `backup.Create` a pre-restore snapshot; `backup.Prune(dir, 20)`; `importer.ReplaceFromExport`; re-`Export` and `syncfile.WriteAtomic(syncBasePath(ap), restoredExport)`; `syncfile.HashFile(restorePath)`; `syncer.RecordSyncState({Path, ContentHash})`.

`hashExport` uses `json.MarshalIndent(export, "", "  ")` (`backup.go:188-193`) — note: **no trailing newline**, unlike `syncfile.marshalExport`.

Usage strings: `restoreUsage = "usage: lit backup restore (--latest | --path <export.json>) [--force]"` (`backup.go:73`); passing both → that string + `" — --latest and --path are mutually exclusive"` (`backup.go:85`); `--latest` with no snapshots → `errors.New("no backups available")` (`backup.go:92`).

### 3.6 Doctor / FixIntegrity (same file)

`Doctor` (`import_export.go:42-112`) initializes `DependencyCycle`, `Errors`, `Warnings` to empty slices and `IntegrityCheck = "ok"`, then:
- `CALL DOLT_VERIFY_CONSTRAINTS()` scanned into `violations`; error wrap `"verify constraints: %w"`. `violations > 0` → `IntegrityCheck = "constraint_violations"` and error line `fmt.Sprintf("constraint violations: %d", violations)`.
- Three FK-orphan counts summed into `ForeignKeyIssues` (error wrap `"count foreign key issues: %w"`):
  - `SELECT COUNT(*) FROM relations r LEFT JOIN issues s ON s.id = r.src_id LEFT JOIN issues d ON d.id = r.dst_id WHERE s.id IS NULL OR d.id IS NULL`
  - `SELECT COUNT(*) FROM comments c LEFT JOIN issues i ON i.id = c.issue_id WHERE i.id IS NULL`
  - `SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL`
  - `> 0` → error line `"foreign key violations: %d"`.
- `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id >= dst_id` → `InvalidRelatedRows`; wrap `"count invalid related rows: %w"`; warning `"invalid related-to ordering rows: %d"`.
- `SELECT COUNT(*) FROM issue_events e LEFT JOIN issues i ON i.id = e.issue_id WHERE i.id IS NULL` → `OrphanHistoryRows`; wrap `"count orphan event rows: %w"`; warning `"orphan issue event rows: %d"`.
- `s.liveRankInversions(ctx)` → `RankInversions = len(...)`; wrap `"count rank inversions: %w"`; warning `"rank inversions: %d (dependencies ranked below dependents)"`.
- `s.liveBlocksCycle(ctx)` → `DependencyCycle`; wrap `"detect blocks dependency cycle: %w"`; warning `"blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)"` with members joined by `" -> "`.

`FixIntegrity` (`import_export.go:117-136`) always runs the repair under `withMutation(ctx, "fsck repair", …)` — Dolt commit message literal **`fsck repair`** — executing exactly three statements:
```sql
DELETE FROM issue_events WHERE issue_id NOT IN (SELECT id FROM issues)   -- "repair orphan events: %w"
DELETE FROM relations WHERE type='related-to' AND src_id = dst_id        -- "repair self related rows: %w"
UPDATE relations SET src_id = dst_id, dst_id = src_id WHERE type='related-to' AND src_id > dst_id  -- "repair related ordering: %w"
```
then returns `s.Doctor(ctx)`. A mutation failure returns `storage.HealthReport{}` plus the error.

---

## 4. IMPORT TREE (`import_tree.go`)

### 4.1 The input document

`storage.ImportTreeSpec` (`internal/storage/specs.go` is the parser; the type is `internal/storage/bulk.go:53-65`):

```go
LocalID     string   `json:"local_id"`
Title       string   `json:"title"`
Description string   `json:"description,omitempty"`
Prompt      string   `json:"prompt,omitempty"`
IssueType   string   `json:"type"`
Topic       string   `json:"topic"`
Priority    int      `json:"priority"`
Assignee    string   `json:"assignee,omitempty"`
Labels      []string `json:"labels,omitempty"`
Parent      string   `json:"parent,omitempty"`
DependsOn   []string `json:"depends_on,omitempty"`
```

The file is **one JSON array of these objects** (`ParseImportTreeSpecs`, `storage/specs.go:50-61`):
- `json.NewDecoder` with `DisallowUnknownFields()` — any key not listed above is an error, wrapped as `"import: parse spec: %w"`.
- After decoding, `dec.More()` → `errors.New("import: unexpected trailing data after spec array")`.

Hand-writable example (pinned verbatim by `import_tree_test.go:93-97`):

```json
[
	{"local_id":"e1","title":"Epic","type":"epic","topic":"tree","priority":0},
	{"local_id":"t1","title":"First","type":"task","topic":"tree","priority":0,"parent":"e1"},
	{"local_id":"t2","title":"Second","type":"task","topic":"tree","priority":0,"parent":"e1","depends_on":["t1"]}
]
```

Defaults for absent fields: every field is a non-pointer, so an absent key is the Go zero value. `priority` absent → `0` → `PriorityNormal` (and `ParsePriority(0)` succeeds). `description`/`prompt`/`assignee`/`parent` absent → `""`. `labels`/`depends_on` absent → nil slice. **`title`, `type`, `topic` and `local_id` have no defaults** — absent `local_id`/`title`/`type` are rejected by validation; absent `topic` is `""` and is *not* rejected here (it reaches `CreateIssue`).

### 4.2 "Tree" = local_id graph over parent + depends_on

`ImportTree(ctx, prefix, specs)` (`import_tree.go:26-86`):

1. `validateImportTreeSpecs(specs)` — full pre-flight, no store writes.
2. `topoSortImportSpecs(specs)` → indices.
3. Create loop in topo order (`import_tree.go:37-72`):
   - `parentID = idMap[spec.Parent]` when `spec.Parent != ""` — resolved **only** from this batch's map, so an external/real parent id resolves to `""` (see §4.5).
   - `model.ParseIssueType(spec.IssueType)` — error `"import: spec %q: %w"` (local id).
   - `model.ParsePriority(spec.Priority)` — error `"import: spec %q: %w"`.
   - `s.CreateIssue(ctx, storage.CreateIssueInput{Title, Description, Prompt, IssueType, Topic, Priority, Assignee, Labels, ParentID, Prefix})`. **`Lane` is never set** on this path (contrast bulk). `Placement` is left at its zero value = `RankBottom` (`internal/storage/issues.go:25`), so creates append in file order.
   - On error: `leaked := s.rollbackCreatedIssues(ctx, createdIDs)` then `"import: create %q: %w (rollback leaked %d: %s)"` with the leaked ids comma-joined.
   - `idMap[spec.LocalID] = issue.ID`; append to `createdIDs`.
4. Second pass over `specs` **in file order** (not topo order) wiring `depends_on` (`import_tree.go:73-84`): for each dep, `AddRelation(storage.AddRelationInput{SrcID: idMap[spec.LocalID], DstID: idMap[dep], Type: "blocks", CreatedBy: "links"})`. The convention is stated at `import_tree.go:77-78`: **src is the dependent, dst is the dependency**. On error: rollback, then `"import: depends_on %q -> %q: %w (rollback leaked %d: %s)"`.
5. Returns `storage.ImportTreeResult{IDMap: idMap}` (`import_tree.go:85`), whose field tag is `json:"id_map"` (`storage/bulk.go:69-71`).

### 4.3 Validation and every rejection text

`validateImportTreeSpecs` (`import_tree.go:104-156`). First pass, per spec index `i`:

| condition | error |
|---|---|
| `len(specs) == 0` | `import: no issues in input` |
| `strings.TrimSpace(LocalID) == ""` | `import: spec %d missing local_id` |
| `LocalID != TrimSpace(LocalID)` | `import: spec %d local_id %q has surrounding whitespace` |
| `strings.TrimSpace(Title) == ""` | `import: spec %q missing title` (local id) |
| `ParseIssueType` fails | `import: spec %q has invalid type %q` |
| `ParsePriority` fails | `import: spec %q has invalid priority %d` |
| local_id already seen | `import: duplicate local_id %q` |

Second pass, over all specs (`import_tree.go:134-154`) — i.e. **forward references are legal**, because references are checked against the complete `seen` set built in the first pass:

| condition | error |
|---|---|
| `Parent != TrimSpace(Parent)` | `import: spec %q parent %q has surrounding whitespace` |
| `Parent` not in `seen` | `import: spec %q references missing parent %q` |
| a `dep != TrimSpace(dep)` | `import: spec %q depends_on entry %q has surrounding whitespace` |
| `dep` not in `seen` | `import: spec %q references missing depends_on %q` |
| `dep == spec.LocalID` | `import: spec %q cannot depend on itself` |

Consequence: on the tree path **every parent/depends_on reference must be internal to the file** — naming a pre-existing real issue id is rejected as "missing parent". (Test `TestImportTreeRejectsMissingReference`, `import_tree_test.go:63-73`, uses `parent:"ghost"` and asserts the error contains `"missing parent"`.)

### 4.4 Topological order and cycles

`topoSortImportSpecs` (`import_tree.go:161-175`) flattens the specs into three parallel slices and calls `topoSortLocalGraph`, wrapping any error as `"import: %w"`.

`topoSortLocalGraph(localID, parent, dependsOn)` (`import_tree.go:189-236`) — shared with `BulkApply`:
- builds `indexByLocal`, **skipping entries whose localID is `""`** (`import_tree.go:191-196`) — an empty local id is never a referable name.
- three-state DFS with literal constants `stateUnvisited = 0`, `stateVisiting = 1`, `stateDone = 2` (`import_tree.go:197-201`).
- `visit(i)`: `stateDone` → return; `stateVisiting` → `fmt.Errorf("cycle detected involving %q", localID[i])`; otherwise mark visiting, recurse into `parent[i]` if non-empty **and** present in the map, then into each `dependsOn[i]` entry present in the map, mark done, append `i` to `order`.
- the outer loop visits indices `0..n-1` in file order (`import_tree.go:230-234`), so the emitted order is post-order DFS seeded by file order — an unconstrained batch keeps file order.
- **References that match no localID are simply not edges** (`import_tree.go:177-188`); they neither create an ordering constraint nor an error at this layer.

Test `TestImportTreeRejectsCycle` (`import_tree_test.go:50-61`) uses `a depends_on b`, `b depends_on a` and asserts the error contains `"cycle"`.

### 4.5 Rollback

`rollbackCreatedIssues` (`import_tree.go:94-102`), shared by ImportTree and BulkApply: for each created real id, `s.Apply(ctx, realID, storage.Change{Action: model.Delete{}, Actor: "links", Reason: "import rollback"})`. Ids whose Apply fails are collected and returned as `leaked`. **It is a soft delete (`model.Delete{}` stamping `deleted_at`), not a row removal.** `leaked` is initialized to `[]string{}`, so the `%d`/`%s` in error messages read `0`/`` on a clean rollback. The caller returns the original error, decorated (`import_tree.go:68`, `:81`).

Atomicity: best-effort only. Doc comment `import_tree.go:18-22` states partial state may remain and the error names every dangling step; the surviving surface is `lit doctor`.

### 4.6 CLI surface

`lit import --path <file>` (`internal/cli/cli.go:1537-1568`). `importUsage = "usage: lit import --path <tree-spec.json | bulk-file.yaml> (see docs/cli-reference.md for both formats)"` (`cli.go:1529`) — raised for an empty `--path` or any positional argument. The file is read with `os.ReadFile`, error `"read import spec: %w"`. Dispatch is on `strings.ToLower(filepath.Ext(path))`: `.yaml`/`.yml` → bulk; **anything else** (including `.json` and no extension) → tree JSON (`cli.go:1554-1568`).

On the JSON branch, a set `--by` flag is an error: `"usage: --by only applies to a YAML bulk-update file (--path *.yaml|*.yml); JSON tree-spec import always attributes creates to \"links\""` (`cli.go:1564`).

Output (`runImportTreeJSON`, `cli.go:1588-1605`): `"imported %d issues\n"` with `len(result.IDMap)`, then one line per map entry `"  %s -> %s\n"` — **iterated over a Go map, so the mapping lines are in nondeterministic order**.

---

## 5. BULK IMPORT (`import_bulk.go`)

### 5.1 Input format

`storage.BulkIssueSpec` (`internal/storage/bulk.go:19-37`) — **YAML**, one document per issue, documents separated by `---`:

```go
LocalID     string    `yaml:"local_id,omitempty"`
ID          string    `yaml:"id,omitempty"`
Title       *string   `yaml:"title,omitempty"`
Description *string   `yaml:"description,omitempty"`
Prompt      *string   `yaml:"prompt,omitempty"`
IssueType   *string   `yaml:"type,omitempty"`
Topic       *string   `yaml:"topic,omitempty"`
Priority    *int      `yaml:"priority,omitempty"`
Assignee    *string   `yaml:"assignee,omitempty"`
Labels      *[]string `yaml:"labels,omitempty"`
Lane        *string   `yaml:"lane,omitempty"`
Parent      string    `yaml:"parent,omitempty"`
DependsOn   []string  `yaml:"depends_on,omitempty"`
Reason      string    `yaml:"reason,omitempty"`
```

Pointer fields carry the patch distinction: nil = "leave unchanged / unspecified", set = "write this value" (`bulk.go:10-15`).

`ParseBulkSpecs` (`internal/storage/specs.go:25-40`): `yaml.NewDecoder` with `dec.KnownFields(true)` — unknown keys are an error. It loops `dec.Decode(&spec)` until `io.EOF`, appending each document; any other error → `"bulk: parse spec: %w"`. **A file with zero documents parses to a nil slice**, which `validateBulkSpecs` then rejects.

Example (from `cli.go:1610-1624`):

```yaml
local_id: epic-x
title: Build X
type: epic
topic: x
---
title: Design
type: task
topic: x
parent: epic-x
---
id: existing-issue-7
title: Renamed
labels: [reviewed]
```

### 5.2 Two document shapes — `id` is the selector

`ID` present → **update patch** of an existing issue. `ID` absent → **create**, behaving like the tree flat form.

The full accept/reject enumeration is written out at `import_bulk.go:191-223` and enforced by `validateBulkSpecs` (`import_bulk.go:224-271`).

Per-document checks that run for **both** shapes (`import_bulk.go:230-247`):

| condition | error |
|---|---|
| `len(specs) == 0` | `bulk: no issues in input` |
| `ID != TrimSpace(ID)` | `bulk: doc %d id %q has surrounding whitespace` |
| `LocalID != TrimSpace(LocalID)` | `bulk: doc %d local_id %q has surrounding whitespace` |
| `Parent != TrimSpace(Parent)` | `bulk: doc %d parent %q has surrounding whitespace` |
| a `dep != TrimSpace(dep)` | `bulk: doc %d depends_on entry %q has surrounding whitespace` |
| `LocalID != "" && dep == LocalID` | `bulk: doc %d (local_id %q) cannot depend on itself` |

Update-document checks (`validateBulkUpdateDoc`, `import_bulk.go:297-324`), in order:

| condition | error |
|---|---|
| `LocalID != ""` | `bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets` |
| `Topic != nil` | `bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it` |
| `Parent != ""` | ``bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead`` |
| `len(DependsOn) > 0` | ``bulk: doc %d (id %q) sets depends_on; wire dependencies with `lit dep add` instead`` |
| `IssueType` set and unparseable | `bulk: doc %d (id %q) has invalid type %q` |
| `Priority` set and unparseable | `bulk: doc %d (id %q) has invalid priority %d` |
| `!bulkUpdateHasField(spec)` | `bulk: doc %d (id %q) has no fields to update` |
| id already seen | `bulk: duplicate id %q` (`import_bulk.go:253-256`) |

`bulkUpdateHasField` (`import_bulk.go:326-330`) is true iff any of `Title, Description, Prompt, IssueType, Priority, Assignee, Labels, Lane` is non-nil. **`Reason` alone does not count** — hence "id set and reason set with no other field set" is rejected.

Create-document checks (`validateBulkCreateDoc`, `import_bulk.go:273-295`), in order:

| condition | error |
|---|---|
| `Title == nil` or trims to `""` | `bulk: doc %d missing title` |
| `Topic == nil` or trims to `""` | `bulk: doc %d missing topic` |
| `IssueType == nil` | `bulk: doc %d missing type` |
| `ParseIssueType` fails | `bulk: doc %d has invalid type %q` |
| `Priority` set and `ParsePriority` fails | `bulk: doc %d has invalid priority %d` |
| `Reason != ""` | `bulk: doc %d sets reason without id (reason only applies to updates)` |
| local_id already seen (non-empty) | `bulk: duplicate local_id %q` (`import_bulk.go:263-268`) |

Note the create branch does **not** require `LocalID` (unlike ImportTree), and does not reject an unresolvable `Parent`/`depends_on` — those pass through as presumed real ids.

### 5.3 Execution

`BulkApply(ctx, prefix, actor string, specs)` (`import_bulk.go:21-119`):

1. `validateBulkSpecs` — whole file validated before anything is written (`import_bulk.go:22-24`).
2. Flatten to `localID/parent/dependsOn` slices and `topoSortLocalGraph` (`import_bulk.go:25-33`); error wrapped as `"bulk: %w"` (so a cycle reads `bulk: cycle detected involving "x"`).
3. `result := storage.BulkApplyResult{Created: map[string]string{}}`; `createdRealID` (index-parallel), `createdIDs` (ordered), `localRealID` map (`import_bulk.go:38-41`).
4. Loop in topo order (`import_bulk.go:43-102`):
   - **Update branch** (`spec.ID != ""`): `bulkUpdateChange(spec, actor)` then `s.Apply(ctx, spec.ID, change)`. `bulkUpdateChange` error is returned **without** a rollback (`import_bulk.go:46-49`). `Apply` error → rollback then `"bulk: update %q: %w (rollback leaked %d: %s)"`. On success append `issue.ID` to `result.Updated`.
   - **Create branch**: re-parse type (`"bulk: doc %d: %w"`, no rollback, `import_bulk.go:61-64`); priority defaults to `model.PriorityNormal` and is re-parsed only if set (`"bulk: doc %d: %w"`, no rollback); then `CreateIssue` with `Title: TrimSpace(*spec.Title)`, `Description: derefOr(spec.Description, "")`, `Prompt: derefOr(spec.Prompt, "")`, `IssueType`, `Topic: TrimSpace(*spec.Topic)`, `ParentID: resolveBulkRef(spec.Parent, localRealID)`, `Priority`, `Assignee: derefOr(spec.Assignee, "")`, `Lane: derefOr(spec.Lane, "")`, `Labels: derefOr(spec.Labels, nil)`, `Prefix: prefix` (`import_bulk.go:72-89`). `Placement` deliberately left at its zero value `RankBottom` so file order is preserved, matching ImportTree (`import_bulk.go:83-87`). Failure → rollback then `"bulk: create doc %d: %w (rollback leaked %d: %s)"`.
   - Record `createdRealID[idx]`, append `createdIDs`. If `spec.LocalID != ""` → `localRealID[LocalID] = issue.ID` and `result.Created[LocalID] = issue.ID`; **else `result.Created[issue.ID] = issue.ID`** (self-keyed) (`import_bulk.go:94-101`).
5. Second pass over `specs` in **file order**, skipping update docs (`import_bulk.go:103-117`): for each `dep`, `AddRelation({SrcID: createdRealID[i], DstID: resolveBulkRef(dep, localRealID), Type: "blocks", CreatedBy: "links"})`. Failure → rollback then `"bulk: depends_on doc %d -> %q: %w (rollback leaked %d: %s)"`.

`derefOr[T](p *T, fallback T) T` (`import_bulk.go:184-189`) is the nil-to-default helper.

`resolveBulkRef(ref, localRealID)` (`import_bulk.go:127-132`): a map hit returns the real id; **a miss returns `ref` unchanged**, to be validated downstream by `CreateIssue`/`AddRelation` as a real pre-existing issue id.

`bulkUpdateChange(spec, actor)` (`import_bulk.go:141-182`) builds `storage.Change{Actor: actor, Fields: storage.UpdateIssueInput{...}}` with `Reason: strings.TrimSpace(spec.Reason)`. Each set pointer is copied into a fresh local and its address taken; `Title`, `Description`, `Prompt`, `Assignee`, `Lane` are `strings.TrimSpace`'d; `IssueType` and `Priority` go through `ParseIssueType`/`ParsePriority` again with errors `"bulk: update %q: %w"`; `Labels` is copied by value (`v := *spec.Labels; fields.Labels = &v`). **`Topic` is never carried** — it is unrepresentable in `UpdateIssueInput` (`internal/storage/issues.go:61-69`).

### 5.4 Differences from the non-bulk (tree) path

| axis | ImportTree | BulkApply |
|---|---|---|
| file format | JSON array, `DisallowUnknownFields` + no-trailing-data | multi-document YAML, `KnownFields(true)` |
| updates | not supported (create-only) | `id` selects an existing issue and patches it |
| `local_id` | **required** on every spec | optional; create-only (illegal alongside `id`) |
| references | must resolve inside the file | resolve inside the file, else pass through as real ids |
| `lane` | not settable | settable on create and update |
| `reason` | absent from the schema | update-only, rejected on a create |
| priority default | `0` (the int zero value, parsed strictly) | `PriorityNormal` when the pointer is nil; parsed only when set |
| topic | required by `CreateIssue`, not by the tree validator | required by the validator (`missing topic`) on create; forbidden on update |
| actor | always `"links"` (CLI rejects `--by`) | `--by` actor threaded into update Changes only |
| result | `ImportTreeResult{IDMap}` | `BulkApplyResult{Created map, Updated []string}` |

### 5.5 Batching, progress, partial failure

**There is no batching.** Every document is applied through the ordinary per-issue `CreateIssue`/`Apply`/`AddRelation` calls one at a time (`import_bulk.go:50`, `:72`, `:112`) — each of which is its own `withMutation` transaction and its own Dolt commit. There are no literal batch-size constants anywhere in these files, and `BulkApply`/`ImportTree` open no transaction of their own.

**No progress reporting** exists at the store layer; the CLI prints only after the whole call returns (`cli.go:1644-1660`).

**Partial failure**: not transactional. On a mid-batch error, `rollbackCreatedIssues` best-effort soft-deletes only the issues **created in this call** — never updated ones, which have "no prior create to unwind" (`import_bulk.go:13-20`). Ids that fail to roll back are named in the error as `(rollback leaked %d: %s)`. Updates that already landed **stay applied**. The doc comment directs the operator to `lit doctor` after a failed batch (`import_bulk.go:18-19`).

### 5.6 CLI surface for bulk

`runImportBulk` (`cli.go:1626-1662`): parse; if `--by` was set but no document has an `id` → `UsageError{Message: "usage: --by only applies when the file has at least one update document (a document with `id` set); this file has none"}` (`cli.go:1637`), determined by `bulkSpecsHaveUpdate` (`cli.go:1666-1673`). Then `ap.Store.BulkApply(ctx, ap.Workspace.IssuePrefix.Value(), actor, specs)`.

Output, in order (`cli.go:1644-1660`):
```
created %d issues\n         (len(result.Created))
  %s -> %s\n                (per Created entry — map iteration, nondeterministic order)
updated %d issues\n         (len(result.Updated))
  %s\n                      (per Updated id, in apply order)
```

### 5.7 What the bulk tests pin

`internal/store/import_bulk_test.go`:

- `TestBulkApplyCreatesEpicWithChildAndDep` (`:12`): three specs (`e1` epic, `t1` task parent e1, `t2` task parent e1 depends_on t1) → `len(result.Created) == 3`; `t2`'s detail has `Parent.ID == Created["e1"]` and `DependsOn` contains `Created["t1"]`.
- `TestBulkApplyCreatesLandInFileOrder` (`:56`): two specs with no placement → `first.Rank < second.Rank`.
- `TestBulkApplyCreateWithoutLocalIDIsReportedByRealID` (`:91`): one create with no `local_id` → the single `Created` entry is self-keyed (`ref == real`).
- `TestBulkApplyUpdatesExistingIssueByID` (`:113`): `{ID, Title:"After"}` → `Updated == [id]` and the stored title is `"After"`.
- `TestBulkApplyMixedCreateAndUpdate` (`:141`): one create + one update → `len(Created)==1 && len(Updated)==1`.
- `TestBulkApplyRejectsUnknownID` (`:171`): `id: "ghost-1"` → error contains `"not found"` (raised by `Apply`, not by validation).
- `TestBulkApplyRejectsUpdateWithNoFields` (`:184`): error contains `"no fields to update"`.
- `TestBulkApplyRejectsUpdateWithTopic` (`:198`): error contains `"immutable"`.
- `TestBulkApplyRejectsUpdateWithParentOrDependsOn` (`:213`): error contains `"lit parent set"`.
- `TestBulkApplyRejectsInvalidTypeOrPriorityOnUpdate` (`:228`): `type:"ghost"` → contains `"invalid type"`; `priority: 7` → contains `"invalid priority"`.
- `TestBulkApplyRejectsMissingCreateFields` (`:246`): title only → contains `"missing topic"`.
- `TestBulkApplyRejectsDuplicateID` (`:257`): contains `"duplicate id"`.
- `TestBulkApplyRejectsIDAndLocalIDTogether` (`:272`): contains `"local_id"`.
- `TestBulkApplyCreateChildOfExistingIssue` (`:287`): `parent: <real epic id>` (matching no local_id) resolves as an external reference and the created child's `Parent.ID` is that epic.
- `TestBulkApplyRollsBackCreatesOnLaterFailure` (`:316`): doc `a` creates, doc `b` has `parent:"ghost-does-not-exist"` which passes validation and fails inside `CreateIssue`; afterwards a default `ListIssues` (which excludes deleted) must not contain doc `a`'s title — pinning that the rollback's soft delete removes it from the default listing.
- `TestBulkApplyRejectsEmptyInput` (`:348`): `nil` specs → contains `"no issues in input"`.

### 5.8 Tree-import test assertions

`internal/store/import_tree_test.go`: `TestImportTreeCreatesEpicWithChildAndDep` (`:11`) pins `len(IDMap)==3`, t2's parent = e1's real id, t2 depends on t1. `TestImportTreeRejectsCycle` (`:50`) → `"cycle"`. `TestImportTreeRejectsMissingReference` (`:63`) → `"missing parent"`. `TestImportTreeRejectsInvalidType` (`:75`) → `"invalid type"`. `TestParseImportTreeSpecsValidFlatFormImports` (`:89`) round-trips the literal JSON array quoted in §4.1 through `storage.ParseImportTreeSpecs` and then `ImportTree`, asserting the same wiring.

---

## 6. Constants and literal values appearing in this slice

| constant / literal | value | site |
|---|---|---|
| export `Version` | `2` | `import_export.go:39` |
| restore Dolt commit message | `"replace from export"` | `import_export.go:139` |
| fsck Dolt commit message | `"fsck repair"` | `import_export.go:118` |
| Doctor healthy `IntegrityCheck` | `"ok"` | `import_export.go:48` |
| Doctor failing `IntegrityCheck` | `"constraint_violations"` | `import_export.go:54` |
| clear order in `writeExportTx` | `labels, comments, relations, issues` | `import_export.go:173` |
| `insertIssueStmt` topic default | `COALESCE(NULLIF(?, ''), 'misc')` → `misc` | `import_export.go:191` |
| relation type written by both importers | `"blocks"` | `import_bulk.go:112`, `import_tree.go:79` |
| relation `CreatedBy` written by both importers | `"links"` | `import_bulk.go:112`, `import_tree.go:79` |
| rollback Change actor / reason | `"links"` / `"import rollback"` | `import_tree.go:97` |
| `CreateIssue` createdBy | `"links"` | `internal/store/store.go:490` |
| `stateUnvisited / stateVisiting / stateDone` | `0 / 1 / 2` | `import_tree.go:197-201` |
| create `Placement` default | `RankBottom` (iota 0) | `internal/storage/issues.go:25` |
| bulk create priority default | `model.PriorityNormal` (0) | `import_bulk.go:65` |
| backup dir | `<StorageDir>/backups` | `internal/backup/backup.go:23` |
| backup filename format | `20060102-150405.000000000` + `.json` | `internal/backup/backup.go:27` |
| backup dir mode | `0o755` | `internal/backup/backup.go:24` |
| sync-base path | `<StorageDir>/last-sync-base.json` | `internal/cli/backup.go:119` |
| syncfile temp pattern | `.links-sync-*.json` | `internal/syncfile/syncfile.go:24` |
| JSON indent (export/stdout, syncfile, hashExport) | `"", "  "` (two spaces) | `cli.go:1802`, `syncfile.go:67`, `cli/backup.go:190` |
| default `--keep` for backup create / restore prune | `20` / `20` | `cli/backup.go:34`, `cli/backup.go:160` |

## 7. Cross-cutting error-message index for this slice

`import_export.go`: `verify constraints: %w`, `count foreign key issues: %w`, `count invalid related rows: %w`, `count orphan event rows: %w`, `count rank inversions: %w`, `detect blocks dependency cycle: %w`, `constraint violations: %d`, `foreign key violations: %d`, `invalid related-to ordering rows: %d`, `orphan issue event rows: %d`, `rank inversions: %d (dependencies ranked below dependents)`, `blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)`, `repair orphan events: %w`, `repair self related rows: %w`, `repair related ordering: %w`, `clear %s: %w`, `restore issue %s: %w`, `restore comment %s: %w`, `restore label %s:%s: %w`, `restore issue event %s: %w`, `restore issue event change %s.%s: %w`.

`import_tree.go`: `import: no issues in input`, `import: spec %d missing local_id`, `import: spec %d local_id %q has surrounding whitespace`, `import: spec %q missing title`, `import: spec %q has invalid type %q`, `import: spec %q has invalid priority %d`, `import: duplicate local_id %q`, `import: spec %q parent %q has surrounding whitespace`, `import: spec %q references missing parent %q`, `import: spec %q depends_on entry %q has surrounding whitespace`, `import: spec %q references missing depends_on %q`, `import: spec %q cannot depend on itself`, `import: spec %q: %w`, `import: create %q: %w (rollback leaked %d: %s)`, `import: depends_on %q -> %q: %w (rollback leaked %d: %s)`, `import: %w`, `cycle detected involving %q`.

`import_bulk.go`: `bulk: no issues in input`, `bulk: doc %d id %q has surrounding whitespace`, `bulk: doc %d local_id %q has surrounding whitespace`, `bulk: doc %d parent %q has surrounding whitespace`, `bulk: doc %d depends_on entry %q has surrounding whitespace`, `bulk: doc %d (local_id %q) cannot depend on itself`, `bulk: duplicate id %q`, `bulk: duplicate local_id %q`, `bulk: doc %d missing title`, `bulk: doc %d missing topic`, `bulk: doc %d missing type`, `bulk: doc %d has invalid type %q`, `bulk: doc %d has invalid priority %d`, `bulk: doc %d sets reason without id (reason only applies to updates)`, `bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets`, `bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it`, ``bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead``, ``bulk: doc %d (id %q) sets depends_on; wire dependencies with `lit dep add` instead``, `bulk: doc %d (id %q) has invalid type %q`, `bulk: doc %d (id %q) has invalid priority %d`, `bulk: doc %d (id %q) has no fields to update`, `bulk: %w`, `bulk: doc %d: %w`, `bulk: update %q: %w`, `bulk: update %q: %w (rollback leaked %d: %s)`, `bulk: create doc %d: %w (rollback leaked %d: %s)`, `bulk: depends_on doc %d -> %q: %w (rollback leaked %d: %s)`.

Parsers (`internal/storage/specs.go`): `bulk: parse spec: %w`, `import: parse spec: %w`, `import: unexpected trailing data after spec array`.

Shared parse gates: `issue type must be task, feature, bug, chore, or epic` (`internal/model/issue_type.go:35` + `oxfordOr`, `:74-87`), `priority must be 0 (normal) or 1 (urgent)` (`internal/model/priority.go:40`).


---

## Behavioral inventory: `internal/store` — verify, recover, rawdump, row_deletes, checkpoint

All paths are relative to `/Users/bmf/code/links-issue-tracker`. Every claim carries a `file:line` citation. Derived from Go source and `_test.go` files only.

---

## 1. VERIFY (`internal/store/verify.go`, 372 lines; `verify_test.go`, 244 lines)

### 1.1 Types and constants

`ConservationLaw` is a `string` type (`internal/store/verify.go:48`). Its four literal values:

| Go const | Literal value | Declared at |
|---|---|---|
| `LawHealth` | `"health"` | `internal/store/verify.go:52` |
| `LawCount` | `"count"` | `internal/store/verify.go:55` |
| `LawIDStability` | `"id_stability"` | `internal/store/verify.go:57` |
| `LawRank` | `"rank_permutation"` | `internal/store/verify.go:60` |

`VerifyFinding` — exactly two fields (`internal/store/verify.go:71-74`):
- `Law ConservationLaw` with JSON tag `"law"` (`verify.go:72`)
- `Detail string` with JSON tag `"detail"` (`verify.go:73`)

`VerifyReport` — exactly one field (`internal/store/verify.go:82-84`):
- `Findings []VerifyFinding` with JSON tag `"findings"` (`verify.go:83`)

There is no separate boolean/status field; `Reconciled()` is derived: `return len(r.Findings) == 0` (`internal/store/verify.go:88`).

`conservationCollections` is the fixed report order (`internal/store/verify.go:157-159`):
`collIssues, collRelations, collComments, collLabels, collEvents, collEventChanges`.
The underlying collection literals (`internal/store/shapemap.go:167-175`): `collIssues = "issues"`, `collRelations = "relations"`, `collComments = "comments"`, `collLabels = "labels"`, `collEvents = "events"`, `collEventChanges = "event_changes"`.

### 1.2 Report rendering — `VerifyReport.String()` (`verify.go:93-103`)

- Reconciled case returns exactly (`verify.go:95`):
  `verify: reconciled — Doctor-clean and all conservation laws hold`
- Otherwise, header line (`verify.go:98`):
  `verify: %d discrepancy(ies) — the rebuild does not conserve the source and cannot be trusted:\n` where `%d` = `len(r.Findings)`
- Then one line per finding (`verify.go:100`):
  `  %d. [%s] %s\n` = 1-based index, the law string, the detail. Two leading spaces.

### 1.3 `VerifyCandidate` — entry point and error paths (`verify.go:122-138`)

Signature: `VerifyCandidate(ctx context.Context, dump RawDump, mapping ShapeMapping, st *Store) (VerifyReport, error)` (`verify.go:122`).

Order of operations:
1. `st.Doctor(ctx)` (`verify.go:123`). On error returns a **zero** `VerifyReport{}` and `fmt.Errorf("verify health gate (doctor): %w", err)` (`verify.go:125`).
2. `st.Export(ctx)` (`verify.go:127`). On error returns zero report and `fmt.Errorf("verify conservation gate (export): %w", err)` (`verify.go:129`).
3. Findings accumulate, in this fixed order, with no early exit (`verify.go:132-136`):
   - `healthFindings(health)` (`verify.go:133`)
   - `countFindings(dump, mapping, export)` (`verify.go:134`)
   - `idStabilityFindings(dump, mapping, export)` (`verify.go:135`)
   - `rankFindings(export)` (`verify.go:136`)
4. Returns `VerifyReport{Findings: findings}, nil` (`verify.go:137`).

**No repair-on-verify.** `VerifyCandidate` performs no writes: it calls only `Doctor` (read-only queries — `internal/store/import_export.go:42-108`) and `Export` (read-only lists — `import_export.go:15-39`). The repair sibling `FixIntegrity` (`import_export.go:113-136`) exists but is never called from verify.go. There is no re-validation of the mapping — the comment at `verify.go:110-116` states the gate deliberately does not re-run `Validate`.

Findings are **all treated identically** — any finding at all makes `Reconciled()` false (`verify.go:88`). There is no advisory/warning tier inside the report: the only severity filtering happens when folding Doctor's output (§1.4).

### 1.4 Check family 1 — HEALTH (`healthFindings`, `verify.go:147-153`)

- Input is `storage.HealthReport` (`internal/storage/maintenance.go:37-47`), whose fields are:
  `IntegrityCheck string` (json `integrity_check`), `ForeignKeyIssues int` (`foreign_key_issues`), `InvalidRelatedRows int` (`invalid_related_rows`), `OrphanHistoryRows int` (`orphan_history_rows`), `RankInversions int` (`rank_inversions`), `DependencyCycle []string` (`dependency_cycle`), `Errors []string` (`errors`), `Warnings []string` (`warnings`).
- Only `h.Errors` become findings; each error string becomes `VerifyFinding{Law: LawHealth, Detail: e}` verbatim (`verify.go:148-151`).
- `h.Warnings` are **discarded** — explicitly, so a faithful rebuild of messy source is not rejected (`verify.go:140-146`). This is the one advisory-vs-fatal split in the whole gate.
- The slice is preallocated `make([]VerifyFinding, 0, len(h.Errors))` — non-nil even when empty (`verify.go:148`).

The concrete checks whose text can appear as a `health` finding, in `Doctor`'s execution order (`internal/store/import_export.go:42-108`):

1. **Constraint verification.** SQL: `CALL DOLT_VERIFY_CONSTRAINTS()` scanned into an int (`import_export.go:49`). Query error → `Doctor` returns `fmt.Errorf("verify constraints: %w", err)` (`import_export.go:50`), which `VerifyCandidate` wraps as `verify health gate (doctor): ...`. If `violations > 0`: sets `IntegrityCheck = "constraint_violations"` and appends **error** `constraint violations: %d` (`import_export.go:52-54`). Default `IntegrityCheck` is `"ok"` (`import_export.go:47`).
2. **Foreign-key counts** — three queries summed into `ForeignKeyIssues` (`import_export.go:56-67`):
   - `SELECT COUNT(*) FROM relations r LEFT JOIN issues s ON s.id = r.src_id LEFT JOIN issues d ON d.id = r.dst_id WHERE s.id IS NULL OR d.id IS NULL` (`import_export.go:57`)
   - `SELECT COUNT(*) FROM comments c LEFT JOIN issues i ON i.id = c.issue_id WHERE i.id IS NULL` (`import_export.go:58`)
   - `SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL` (`import_export.go:59`)
   Query error → `fmt.Errorf("count foreign key issues: %w", err)` (`import_export.go:63`). If sum > 0, appends **error** `foreign key violations: %d` (`import_export.go:67`).
3. **Invalid related-to ordering.** SQL: `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id >= dst_id` (`import_export.go:69`). Error → `count invalid related rows: %w`. If > 0, appends **warning** `invalid related-to ordering rows: %d` (`import_export.go:72`) — a warning, therefore **ignored by verify**.
4. **Orphan event rows.** SQL: `SELECT COUNT(*) FROM issue_events e LEFT JOIN issues i ON i.id = e.issue_id WHERE i.id IS NULL` (`import_export.go:74`). Error → `count orphan event rows: %w`. If > 0, **warning** `orphan issue event rows: %d` (`import_export.go:77`) — ignored by verify.
5. **Rank inversions.** Computed in Go via `s.liveRankInversions(ctx)` (`import_export.go:88`); error → `count rank inversions: %w`. If > 0, **warning** `rank inversions: %d (dependencies ranked below dependents)` (`import_export.go:93`) — ignored by verify.
6. **Blocks dependency cycle.** `s.liveBlocksCycle(ctx)` (`import_export.go:99`); error → `detect blocks dependency cycle: %w`. If non-empty, sets `DependencyCycle` and appends **warning** `blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)` with members joined by `" -> "` (`import_export.go:104-105`) — ignored by verify.

Net effect: only checks 1 and 2 (constraint violations, foreign-key violations) can ever fail the verify health half.

### 1.5 Check family 2 — COUNT conservation (`countFindings`, `verify.go:179-222`)

Expected counts are computed from the **raw dump** row counts, never re-derived through the mapping:
- `mapTables := tablesByName(m)` (`verify.go:180`; helper at `shapemap.go:481-489`, last-wins index by table name).
- Every collection in `conservationCollections` starts `exact[c] = true` (`verify.go:183-185`).
- For each dumped table, for each emitter of that table's mapping (`verify.go:186-194`):
  - If `em.When` type-asserts to `Always` → `expected[em.Collection] += len(table.Rows)` (`verify.go:189`).
  - Otherwise (any conditional `When`) → `exact[em.Collection] = false`, permanently excluding that collection from the count law (`verify.go:192`).
- Actual counts read from `model.Export` (`verify.go:200-207`): `collIssues = len(export.Issues)`, `collRelations = len(export.Relations)`, `collComments = len(export.Comments)`, `collLabels = len(export.Labels)`, `collEvents = len(export.Events)`, `collEventChanges =` sum of `len(ev.Changes)` over `export.Events` (`verify.go:196-199`).
- Iteration for reporting is over `conservationCollections` (fixed order), skipping any collection with `exact[coll] == false` (`verify.go:210-213`).
- Failure condition: `expected[coll] != actual[coll]` (`verify.go:214`). A collection with no emitter at all has `expected = 0`, so a non-empty rebuild of an unmapped collection also fires.
- Message (`verify.go:217`): `collection %q: source dump carries %d row(s) mapped here, rebuild has %d` — collection name quoted via `%q`, then expected, then actual.

Tests pin:
- `TestCountFindingsDetectsRowLoss` — 2 source issue rows vs 1 exported issue yields exactly one finding with `Law == LawCount` whose detail contains `issues`, `2`, and `1` (`verify_test.go:93-107`).
- `TestCountFindingsExcludesConditionalFanOutChildren` — with a 2-row `issue_history` dump mapped by `DeterministicMap`, an export with 3 nested `Changes` produces **zero** findings (conditional child excluded), while dropping an event to 1 produces exactly one `LawCount` finding containing `events` (`verify_test.go:117-149`).

### 1.6 Check family 3 — ID STABILITY (`idStabilityFindings`, `verify.go:233-267`)

- Reference set built by `sourceValuesFor(dump, m, "issues.id")` (`verify.go:234`). Target key literal is `"issues.id"`.
- If no source column maps to `issues.id`, returns `nil` — no findings at all (`verify.go:236-239`).
- Source set = set of raw cell strings; rebuilt set = set of `issue.ID` over `export.Issues` (`verify.go:241-248`).
- `missing = setDifference(source, rebuilt)`, `extra = setDifference(rebuilt, source)` (`verify.go:250-251`); `setDifference` returns sorted keys of `a` absent from `b` (`verify.go:349-358`).
- Two possible findings, both `LawIDStability`, emitted in this order:
  - missing (`verify.go:257`): `%d issue id(s) present in the source dump but absent from the rebuild: %s` — count then ids joined with `", "`.
  - extra (`verify.go:263`): `%d issue id(s) present in the rebuild but absent from the source dump: %s`.

`sourceValuesFor` (`verify.go:326-346`) semantics:
- Iterates every dumped table; builds `colIndex := rowColumnIndex(table)` (`verify.go:331`; helper at `shapemap.go:528-534`).
- For each emitter and each `field → src` pair, only `FromColumn` sources count (`verify.go:334`), and the match test is a string equality: `TargetKey(string(em.Collection)+"."+field) == target` (`verify.go:335`).
- Sets `found = true` on the first match but keeps going — it aggregates the **union** across all tables/columns rather than returning early (`verify.go:337-342`; the union behavior is pinned by `TestSourceValuesForAggregatesAcrossTables`, `verify_test.go:180-202`, expecting `i1,i2,i3` across two tables both mapping into `issues.id`).
- Cell values are rendered by `cellString` (`verify.go:341`; `shapemap.go:795-804`): `nil → ""`, `string → itself`, anything else → `fmt.Sprint(v)`.

`setIntersection` (`verify.go:363-372`) is declared in verify.go but has no caller in this file; its only use is `internal/store/sync_unrelated.go:30`.

Test pin: `TestIDStabilityFindingsDetectsLostAndExtraIDs` requires exactly 2 findings (one missing `i2`, one extra `i9`), both `LawIDStability` (`verify_test.go:154-172`).

### 1.7 Check family 4 — RANK PERMUTATION (`rankFindings`, `verify.go:277-314`)

Operates purely on `export.Issues`; the dump/mapping are not consulted (`verify.go:277`).
- Issues with `Rank == ""` are skipped entirely — unranked is legal (`verify.go:281-283`).
- Well-formedness: `rank.Valid(issue.Rank)` (`verify.go:284`). `rank.Valid` returns false for the empty string and for any string containing a byte outside the base-62 alphabet `0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz` (`internal/rank/rank.go:17`, `rank.go:53-63`). Malformed entries are recorded as `%s=%q` (id, rank) (`verify.go:285`).
- Distinctness: ranks are grouped into `idsByRank`; any rank with more than one id is a collision, rendered as `%q shared by %s` with the ids sorted and joined by `", "` (`verify.go:287`, `verify.go:291-296`).
- Both `collisions` and `malformed` are sorted before rendering, for determinism (`verify.go:297-298`).
- Findings emitted in this order, both `LawRank`:
  - collisions (`verify.go:304`): `ranks are not distinct (a valid order needs one rank per issue): %s` — collision clauses joined by `"; "`.
  - malformed (`verify.go:310`): `%d issue(s) carry a value that is not a well-formed rank: %s` — entries joined by `", "`.

Test pins: `TestVerifyCandidateRejectsMisMappedRank` builds a candidate from a priority↔rank swapped mapping, requires `VerifyCandidate` to return no error, `Reconciled() == false`, at least one `LawRank` finding, and the rendered report to contain both colliding ids `i1` and `i2` (`verify_test.go:58-88`). `TestVerifyCandidateReconciledOnFaithfulRebuild` requires a faithful rebuild of `rankedDump()` to be `Reconciled()` (`verify_test.go:32-50`). The fixture `rankedDump()` is a single `issues` table with columns `id,title,description,status,priority,issue_type,created_at,updated_at,closed_at,item_rank` and two rows with ranks `"V"` and `"h"` and both priorities `int64(0)` (`verify_test.go:17-28`).

### 1.8 Fatal vs advisory summary

- **Gate-cannot-run errors** (returned as Go `error`, report is the zero value): Doctor failure, Export failure (`verify.go:124-130`).
- **Rejecting findings** (all equal weight, none advisory): Doctor `Errors` only, count mismatch, id missing, id extra, rank collision, rank malformed.
- **Silently tolerated**: every Doctor `Warning` (`verify.go:147-153`), any collection fed by a conditional emitter (`verify.go:192`), empty ranks (`verify.go:281`), an absent `issues.id` mapping (`verify.go:236-239`).

---

## 2. RECOVER (`internal/store/recover.go`, 208 lines; `recover_test.go`, 290 lines)

### 2.1 The `Mapper` seam

`type Mapper func(dump RawDump, feedback string) (ShapeMapping, error)` (`recover.go:27`).

`DeterministicMapper(dump RawDump, _ string)` ignores feedback; delegates to `DeterministicMap(dump)`, and on `!ok` returns the exact error text (`recover.go:34-40`):
`workspace shape not recognized by any built-in mapper; the LLM mapping path is required (feed `+"`lit lifeboat dump`"+` to the mapper, then apply+verify)` (`recover.go:37`).

### 2.2 The three outcome variants

`RecoveryOutcome` is a sealed interface with the unexported marker `isRecoveryOutcome()` (`recover.go:52`), implemented by exactly three types (`recover.go:86-88`):

- `Reconciled{Candidate *Candidate; Mapping ShapeMapping}` (`recover.go:58-61`)
- `RequiresDrop{Candidate *Candidate; Mapping ShapeMapping; Drops []UnexplainedDrop}` (`recover.go:72-76`)
- `Unconverged{Residual string; Attempts int}` (`recover.go:81-84`)

`UnexplainedDrop{Column ColumnRef}` — one field (`recover.go:92-94`).

Caller owns and must `Discard()` the candidate in the first two variants (`recover.go:57`, `recover.go:69`).

### 2.3 Trigger conditions and preconditions of `Recover`

Signature: `Recover(ctx, canonicalDoltDir string, dump RawDump, mapper Mapper, maxAttempts int) (RecoveryOutcome, error)` (`recover.go:108`).

1. **Budget precondition**: `maxAttempts < 1` → returns `(nil, fmt.Errorf("recovery attempt budget must be at least 1, got %d", maxAttempts))` (`recover.go:114-116`). Pinned for 0 and -1, and that the outcome must be nil, by `TestRecoverRejectsNonPositiveBudget` (`recover_test.go:203-216`).
2. **Path validation**: `validateDoltRootDir(canonicalDoltDir)` (`recover.go:123`). That helper rejects whitespace-only/empty with `errors.New("dolt root dir is required")` and otherwise returns `filepath.Clean(path)` (`internal/store/store.go:324-329`). Pinned by `TestRecoveryEntryPointsRejectEmptyPath` (`recover_test.go:221-233`, input `"  "`) and `TestValidateDoltRootDirCleansPath` (`recover_test.go:238-251`).
3. **Staging location**: `parentDir := filepath.Dir(canonicalDoltDir)` (`recover.go:127`) — candidates are staged as siblings of the canonical Dolt directory, so a later promotion is a same-filesystem rename (`recover.go:117-122`).

`Recover` itself does **not** read or write the canonical directory: it only derives the parent path. It never touches the Dolt working set of the live workspace, never resets, never checks out, never stashes, and never commits. All of its on-disk effect is inside candidate scratch trees (§2.6).

### 2.4 The loop (`recover.go:128-140`)

- `feedback := ""` initially (`recover.go:128`); the first pass therefore always receives an empty feedback string — pinned at `recover_test.go:120-122`.
- `for attempt := 1; attempt <= maxAttempts; attempt++` calls `runAttempt(ctx, parentDir, dump, mapper, feedback)` (`recover.go:129-130`).
- A hard error from `runAttempt` aborts the whole loop and is returned with a nil outcome (`recover.go:131-133`).
- A non-nil outcome returns immediately (`recover.go:134-136`).
- Otherwise the returned string becomes the next pass's feedback (`recover.go:137`).
- Budget exhaustion: `return Unconverged{Residual: feedback, Attempts: maxAttempts}, nil` (`recover.go:139`). `Attempts` is the **budget**, not the number of passes that actually ran (they are equal by construction).
- `dump` is passed unchanged to every pass; it is never mutated (`recover.go:105-107`).

### 2.5 One pass — `runAttempt` (`recover.go:152-180`), exact step order

1. `mapping, err := mapper(dump, feedback)` (`recover.go:153`). On error → **not** a hard error; returns feedback `the mapper could not propose a mapping: %v` and a nil outcome (`recover.go:155`). Pinned by `TestRecoverUnconvergedOnPersistentMapperDecline`, which asserts `Attempts == 2` and the residual contains the mapper's message (`recover_test.go:272-290`).
2. `cand, err := RebuildCandidate(ctx, parentDir, dump, mapping)` (`recover.go:157`). Error handling branches on the sentinel:
   - `errors.Is(err, ErrInvalidMapping)` → feedback `the proposed mapping was rejected by the applier: %v` (`recover.go:165`), loop continues.
   - Any other error → hard error `rebuild candidate from a valid mapping failed: %w` (`recover.go:167`), loop aborts. `ErrInvalidMapping = errors.New("mapping is not applicable to the dump")` (`internal/store/candidate.go:52`); `RebuildCandidate` tags only `Apply`-stage rejections with it (`candidate.go:77-83`), pinned by `TestRebuildCandidateTagsMappingRejection` (`recover_test.go:257-267`).
3. `report, err := VerifyCandidate(ctx, dump, mapping, cand.store)` (`recover.go:169`). On error → hard error `errors.Join(fmt.Errorf("verify gate could not run: %w", err), cand.Discard())` — the candidate is discarded and any discard error is joined in (`recover.go:171`).
4. If `!report.Reconciled()` → `cand.Discard()`; a discard failure becomes the hard error `discard rejected candidate: %w` (`recover.go:174-176`). Otherwise returns `report.String()` as the next feedback with a nil outcome (`recover.go:177`). Every rejected candidate is therefore removed before the next pass starts (`recover.go:149-151`).
5. Reconciled → `classifyConverged(cand, mapping)` (`recover.go:179`); the candidate is **not** discarded and is handed to the caller.

### 2.6 On-disk state written/read by a pass (via `RebuildCandidate`, `internal/store/candidate.go`)

- `Apply(dump, mapping)` runs **first and purely** — a mapping rejection touches no filesystem resource at all (`candidate.go:77-83`).
- `os.MkdirTemp(parentDir, "lit-candidate-*")` creates the candidate root (`candidate.go:85`); the literal pattern is `lit-candidate-*`.
- On any failure after that point, a deferred cleanup closes the store (if opened) and runs `os.RemoveAll(root)`, joining errors into the return (`candidate.go:89-104`).
- `Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)` — the Dolt workspace is nested at `<root>/workspace` so the workspace lock and migration snapshots land inside the owned root (`candidate.go:106-111`). Error: `open candidate workspace: %w`.
- `st.ReplaceFromExport(ctx, export)` loads the data (`candidate.go:112`), error `load export into candidate: %w`. That path commits inside the candidate's own Dolt database with commit message `"replace from export"` (`internal/store/import_export.go:130-132`) — the only commit any recovery pass makes, and it is made in the throwaway candidate, never in the canonical workspace.
- The candidate stamps `expectedHead: dump.DoltHead` and `workspaceID: dump.WorkspaceID` for a later promotion's lost-update check (`candidate.go:117`).
- `Candidate.Discard()` closes the store then `os.RemoveAll(c.root)`; `root` is cleared only on successful removal, so a later `Discard` retries. It is documented and implemented as **idempotent** (`candidate.go:157-179`).

### 2.7 `classifyConverged` and `unexplainedDrops`

- `classifyConverged` returns `RequiresDrop` if `len(unexplainedDrops(mapping)) > 0`, else `Reconciled` (`recover.go:185-191`).
- `unexplainedDrops` walks `m.Tables`, and for each `col → d` in `tm.Drops` includes it when `d.Provenance == DropUnexplained` (`recover.go:197-205`). Results are sorted by `out[i].Column.String()` (`recover.go:206`), giving deterministic order.

### 2.8 Idempotency / repeatability

- Every pass is the same pipeline; the only carried state is the `feedback` string (`recover.go:100-104`, `recover.go:129-138`).
- The dump is read-only across all passes, so attempt N cannot be contaminated by N-1 (`recover.go:105-107`); each rejected candidate is a separate temp tree removed whole (`candidate.go:17-23`).
- `DeterministicMapper` is a pure function of the dump and ignores feedback, so re-running it cannot self-repair; the doc directs running it at `maxAttempts=1` (`recover.go:31-33`). The CLI does exactly that: `const recoverAttempts = 1` (`internal/cli/lifeboat.go:38`).

### 2.9 CLI consumption of the outcomes (`internal/cli/lifeboat.go`)

- `lit lifeboat recover [--mapping <file>]`; wrong arg count → `UsageError{Message: "usage: lit lifeboat recover [--mapping <file>]"}` (`lifeboat.go:82`).
- With no `--mapping`, the mapper is `store.DeterministicMapper`; with one, the file is read and `json.Unmarshal`ed into a `store.ShapeMapping` and wrapped as a constant mapper (`lifeboat.go:48-61`). Errors: `read mapping %s: %w` (`lifeboat.go:54`), `parse mapping %s: %w` (`lifeboat.go:58`).
- Sequence: `store.HealWorkspace(ctx, ws.DatabasePath)` → `store.DumpRaw(...)` → `store.Recover(..., recoverAttempts)` (`lifeboat.go:93-103`).
- `Reconciled` → `promoteReconciled`: `store.PromoteCandidate`, then prints `recovered: rebuilt workspace promoted to %s (%s)\n` where the parenthetical is `previous contents preserved at %s` or, when `result.Backup == ""`, the literal `no previous contents to preserve` (`lifeboat.go:121-144`). The candidate is discarded in a defer, joining `discard candidate scratch after promotion: %w` on failure (`lifeboat.go:126-130`).
- `RequiresDrop` → candidate discarded, and error: `recovery needs a human decision: the mapping discards %d source column(s) with no recorded justification:\n%s\nnothing was changed; supply a mapping that maps or intentionally drops these before recovering` with the drops rendered one per line as `  - %s` (`lifeboat.go:107-113`, `formatDrops` at `lifeboat.go:146-152`).
- `Unconverged` → `recovery did not converge after %d attempt(s); nothing was changed:\n%s` (`lifeboat.go:115`).
- Unknown type → `unknown recovery outcome %T` (`lifeboat.go:117`).

### 2.10 What the recover tests pin

- `TestRecoverReconcilesKnownShape`: `preGooseDump()` + `DeterministicMapper` + budget 1 reaches `Reconciled`, the candidate's `Doctor` is clean, and `Export` has exactly 2 issues (`recover_test.go:76-104`).
- `TestRecoverSelfRepairsAcrossAttempts`: pass 1 returns an empty `ShapeMapping{}` (applier-rejected); pass 2 receives non-empty feedback and returns the good mapping; the result is `Reconciled` after exactly 2 mapper calls (`recover_test.go:110-143`).
- `TestRecoverRequiresDropOnUnexplainedDrop`: dropping the optional `issues.assignee` column with `Dropped{Provenance: DropUnexplained}` yields `RequiresDrop` with exactly one drop equal to `ColumnRef{Table:"issues", Column:"assignee"}` (`recover_test.go:150-171`, helper `withUnexplainedDrop` at `recover_test.go:46-65`).
- `TestRecoverUnconvergedSurfacesResidual`: priority↔rank swap with budget 3 gives `Unconverged` with `Attempts == 3` and a residual containing the string `rank_permutation` (`recover_test.go:177-198`).

---

## 3. RAWDUMP (`internal/store/rawdump.go`, 206 lines; `rawdump_test.go`, 177 lines)

### 3.1 The artifact shape

`RawDump` fields (`rawdump.go:23-34`):
- `WorkspaceID string` json `workspace_id` (`rawdump.go:24`)
- `DoltHead string` json `dolt_head` (`rawdump.go:32`)
- `Tables []RawTable` json `tables` (`rawdump.go:33`)

`RawTable` fields (`rawdump.go:39-43`):
- `Name string` json `name`, `Columns []string` json `columns`, `Rows [][]any` json `rows`.

`Rows` is always initialized to `[][]any{}` so an empty table serializes as `[]`, never `null` (`rawdump.go:185`; pinned at `rawdump_test.go:123-126` against the goose bookkeeping table).

### 3.2 Output format and destination

The dump is a **JSON** document, not SQL and not TSV. The only producer path to a file/stdout is `lit lifeboat dump`, which writes the `RawDump` value to stdout via `writeJSON` (`internal/cli/lifeboat.go:170`), which uses `json.NewEncoder(w)` with `enc.SetIndent("", "  ")` — two-space indentation, one trailing newline from `Encode` (`internal/cli/cli.go:1800-1804`). There is **no header line, no footer line, and no SQL quoting/escaping layer** — escaping is entirely `encoding/json`'s. `runLifeboatDump` takes no flags and rejects extra args with `UsageError{Message: "usage: lit lifeboat dump"}` (`internal/cli/lifeboat.go:158-171`).

### 3.3 `DumpRaw` — exact step order and every error (`rawdump.go:61-123`)

Signature `DumpRaw(ctx, doltRootDir string, workspaceID string) (RawDump, error)` with a named error return (`rawdump.go:61`).

1. `validateOpenArgs(doltRootDir, workspaceID)` (`rawdump.go:62`) — rejects an empty/whitespace root dir with `dolt root dir is required` (`store.go:324-327`) and an empty/whitespace workspace id with `workspace id is required` (`store.go:308-310`).
2. `acquireWorkspaceShared(ctx, doltRootDir)` — a **shared** workspace lock, excluding directory rotators such as `lit snapshots restore` (`rawdump.go:65`, `rawdump.go:53-57`). On contention the error text is `a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w` wrapping `ErrWorkspaceBusy` (`internal/store/workspace_lock.go:83-87`).
3. A deferred release joins any release error into the returned error (`rawdump.go:72-76`).
4. `os.Stat(doltRootDir)` (`rawdump.go:77`): `os.ErrNotExist` → `repository not initialized with lit — run 'lit init' first` (`rawdump.go:81`); any other stat error → `stat database dir: %w` (`rawdump.go:83`).
5. `requireNoPendingAdopt(doltRootDir)` — run **after** the lock is taken (`rawdump.go:90`). Its message (`internal/store/adopt.go:138-144`): `%w: a `+"`lit init`"+` backlog adopt was interrupted before completing (%s; marker %s), so the on-disk store is that adopt's leftover partial state, not a usable backlog. Run `+"`lit init`"+` to retry: it sets the leftover aside and re-clones the remote backlog. If the remote no longer carries the backlog, delete %s to abandon the adopt and start fresh`, with the `%s` context either the literal `a backlog adopt` or `the adopt of %s/%s started %s` (`adopt.go:133-137`); a marker read failure yields `read adopt-pending marker: %w` (`adopt.go:131`).
6. `openStoreConnection(ctx, doltRootDir, workspaceID, engineRead)` — **no `migrate()` call**, which is what lets it read a workspace `store.Open` refuses (`rawdump.go:93`, doc at `rawdump.go:47-52`). `engineRead` is the first `engineAccess` value (`store.go:40`).
7. Deferred `s.db.Close()`, whose error is joined into the return unless it is `context.Canceled` (`rawdump.go:97-101`).
8. `readDoltHead(ctx, s.db)` — mandatory, not best-effort (`rawdump.go:106`, rationale `rawdump.go:102-105`).
9. `listTables(ctx, s.db)` (`rawdump.go:110`).
10. `dumpTable` per table, in the order `listTables` returned; the first table error aborts the whole dump (`rawdump.go:114-121`).
11. Returns `RawDump{WorkspaceID: workspaceID, DoltHead: head, Tables: tables}` (`rawdump.go:122`).

The dump is **read-only** on the database — the only SQL issued is the three SELECT-family statements below; nothing is written, committed, reset, or checked out (`rawdump.go:58-60`).

### 3.4 The three SQL statements

- Head: `SELECT commit_hash FROM dolt_log() LIMIT 1` (`rawdump.go:136`), error `read dolt head: %w` (`rawdump.go:137`). This is the single HEAD reader shared with promote-time re-checks and migration checkpointing (`rawdump.go:129-133`).
- Table list: `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name` (`rawdump.go:148`) — i.e. **every base table in the database in ascending catalog-name order**, including Dolt/goose bookkeeping tables; there is no hand-maintained include/exclude list (`rawdump.go:142-145`). Errors: `list tables: %w` (`rawdump.go:150`), `scan table name: %w` (`rawdump.go:158`), `iterate tables: %w` (`rawdump.go:162`).
- Per table: `"SELECT * FROM `" + name + "`"` — the table name is interpolated inside backticks, not parameterized (`rawdump.go:176`). Errors: `select %q: %w` (`rawdump.go:178`), `columns %q: %w` (`rawdump.go:183`), `scan row of %q: %w` (`rawdump.go:193`), `iterate rows of %q: %w` (`rawdump.go:204`).

### 3.5 Cell value rules (`dumpTable`, `rawdump.go:175-206`)

- Columns come from `rows.Columns()` on the live result set — never assumed (`rawdump.go:181`, `rawdump.go:167-168`).
- Each row is scanned into `[]any` of `len(cols)`; positional order matches `Columns` (`rawdump.go:187-192`).
- The only type normalization: any `[]byte` cell is converted to `string` (`rawdump.go:195-199`).
- SQL `NULL` scans to `nil` and serializes as JSON `null`, distinct from `""` (`rawdump.go:171-174`; pinned by the `closed_at` assertion at `rawdump_test.go:173-176`).

### 3.6 What the rawdump tests pin

- `TestDumpRawReleasesDeadendedWorkspace`: after dropping the `issues.title` column and stamping goose ahead of the registry, `Open` fails with `*UnsupportedSchemaVersionError` while `DumpRaw` succeeds; `dump.WorkspaceID` equals the passed id; the `issues` table's rows match the seeded issue ids as `string` cells; the dropped `title` column is **absent** from `Columns` (not faked); and the goose bookkeeping table appears with non-nil `Rows` (`rawdump_test.go:47-127`).
- `TestDumpRawHealthyWorkspaceRoundTripsValues`: on a healthy workspace, `title` and `id` cells are Go `string`s with the exact seeded values, and the unset `closed_at` is `nil` (`rawdump_test.go:132-177`).

---

## 4. ROW_DELETES (`internal/store/row_deletes.go`, 119 lines; no dedicated test file)

### 4.1 Key types

- `relationKey{srcID string; dstID string; kind model.RelationType}` — exactly the schema PK `(src_id, dst_id, type)` (`row_deletes.go:35-39`).
- `labelKey{issueID string; name string}` — the labels PK `(issue_id, label)` (`row_deletes.go:42-45`).

### 4.2 The five deletes — all HARD deletes, all single-row by full primary key

| Function | Exact SQL | Subject string in errors | Site |
|---|---|---|---|
| `deleteIssueTx(ctx, tx, id)` | `DELETE FROM issues WHERE id = ?` | `fmt.Sprintf("issue %s", id)` | `row_deletes.go:83-85` |
| `deleteRelationRowTx(ctx, tx, key)` | `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?` | `fmt.Sprintf("relation %s->%s (%s)", key.srcID, key.dstID, key.kind)` | `row_deletes.go:87-91` |
| `deleteCommentTx(ctx, tx, id)` | `DELETE FROM comments WHERE id = ?` | `fmt.Sprintf("comment %s", id)` | `row_deletes.go:93-95` |
| `deleteLabelTx(ctx, tx, key)` | `DELETE FROM labels WHERE issue_id = ? AND label = ?` | `fmt.Sprintf("label %s:%s", key.issueID, key.name)` | `row_deletes.go:97-100` |
| `deleteEventTx(ctx, tx, id)` | `DELETE FROM issue_events WHERE id = ?` | `fmt.Sprintf("issue event %s", id)` | `row_deletes.go:103-105` |

Bind order for relations is `key.srcID, key.dstID, string(key.kind)` (`row_deletes.go:90`); for labels `key.issueID, key.name` (`row_deletes.go:99`).

### 4.3 Tombstone vs hard delete

- These are **hard** row removals. The soft-delete path is separate: ordinary issue deletion is a `DeletedAt` stamp, and **no CRUD path hard-deletes an issue row** — `deleteIssueTx` and `deleteEventTx` have only the reconcile delta as caller, deliberately (`row_deletes.go:55-61`).
- Cascade is owned by the schema, not by this code: deleting an issue takes its relations, comments, labels, events and event changes via `ON DELETE CASCADE` (`row_deletes.go:80-82`); deleting an event takes its `issue_event_changes` rows (`row_deletes.go:102`). No cascading DELETE statements are issued here.

### 4.4 Rows-affected contract and error text (`execDelete`, `row_deletes.go:109-119`)

- Runs `tx.ExecContext(ctx, stmt, args...)`; on error returns `(0, fmt.Errorf("delete %s: %w", subject, err))` (`row_deletes.go:111-113`).
- Then `res.RowsAffected()`; on error returns `(0, fmt.Errorf("delete %s: rows affected: %w", subject, err))` (`row_deletes.go:114-117`).
- Otherwise returns the affected count and nil (`row_deletes.go:118`).
- The count exists for the CRUD callers to distinguish "removed" from "there was nothing there"; the reconcile delta ignores it (`row_deletes.go:76-78`).

### 4.5 Callers and the affected-count decisions they make

- `RemoveLabel` — `deleteLabelTx(... labelKey{issueID, name: label})`; `affected == 0` → `storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, label)}` (`internal/store/labels.go:51-57`).
- `RemoveRelation` — endpoints canonicalized first via `relType.CanonicalEndpoints`; `affected == 0` → `storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}` (`internal/store/relations.go:392-406`).
- `DeleteComment` — calls `deleteCommentTx` and **discards** the count (`internal/store/store.go:1189-1191`).
- The reconcile replay's delta — `applyExportDelta` runs the five tables in this exact order, deleting before inserting within each table: **issues, relations, comments, labels, events** (`internal/store/export_delta.go:217-230`). Issues go first so child rows inserted afterwards have their foreign key satisfied (`export_delta.go:213-216`). `applyTableDelta` explicitly ignores the affected count (`export_delta.go:243-248`).

### 4.6 Deletes deliberately NOT routed here

Set-matching deletes are separate statements on purpose (`row_deletes.go:63-74`): `setSingleValuedEdgeTx` (relations.go), `ClearParent` (relations.go), `replaceLabelsTx` (labels.go), and the self-edge sweep in import_export.go. Also outside: `writeExportTx`'s wholesale clear, which issues `DELETE FROM` for `labels, comments, relations, issues` in that order and deliberately does not name `issue_events`/`issue_event_changes` because they cascade from issues (`internal/store/import_export.go:160-172`), and `FixIntegrity`'s repairs (`import_export.go:114-124`).

---

## 5. CHECKPOINT (`internal/store/checkpoint.go`, 121 lines; `checkpoint_test.go`, 342 lines)

### 5.1 What a checkpoint IS

Concretely, **a Dolt branch created at the current HEAD** — not a tag, not a file, not a Dolt commit of its own (`checkpoint.go:12-13`, `checkpoint.go:28`). The value type is `storage.Checkpoint` (`internal/storage/maintenance.go:19-29`) with four fields:
- `Name string` — documented as `"<prefix>-<unix-nano>"` (`maintenance.go:20`)
- `Prefix string` — caller label, e.g. `"pre-migrate"` (`maintenance.go:21`)
- `CreatedAt time.Time` — parsed from the unix-nano suffix in Name (`maintenance.go:22`)
- `Anchor string` — opaque engine-side identity of the captured state; the Dolt engine fills it with a commit hash (`maintenance.go:23-28`, `checkpoint.go:22`).

The capability interface is `storage.Checkpointer` with exactly `CreateCheckpoint`, `ListCheckpoints`, `PruneCheckpoints`, `ResetToCheckpoint` (`internal/storage/capabilities.go:136-144`); the capability token is `Checkpoints = capability[Checkpointer]{name: "checkpoints"}` (`capabilities.go:294`).

### 5.2 Naming format string

`name := fmt.Sprintf("%s-%d", prefix, ts.UnixNano())` where `ts := time.Now().UTC()` (`checkpoint.go:26-27`). The suffix is nanoseconds since epoch as a decimal integer.

### 5.3 `CreateCheckpoint(ctx, prefix)` (`checkpoint.go:17-37`)

1. `readDoltHead(ctx, s.db)` — the shared HEAD reader (`checkpoint.go:22`, defined `rawdump.go:134-140`). Error → `checkpoint: %w` (`checkpoint.go:24`).
2. Timestamp captured (`checkpoint.go:26`), name formatted (`checkpoint.go:27`).
3. `s.db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", name)` — parameterized (`checkpoint.go:28`). Error → `checkpoint: create branch %q: %w` (`checkpoint.go:29`).
4. Returns `storage.Checkpoint{Name: name, Prefix: prefix, CreatedAt: ts, Anchor: commitSHA}` (`checkpoint.go:31-36`).

No pre-existence check, no retry on duplicate name; two creations within the same nanosecond would collide at `DOLT_BRANCH`. Tests sleep 1ms between creations to guarantee unique suffixes (`checkpoint_test.go:201`, `checkpoint_test.go:256`, `checkpoint_test.go:290`).

### 5.4 `ResetToCheckpoint(ctx, name)` (`checkpoint.go:45-50`)

- SQL: `CALL DOLT_RESET('--hard', ?)` with the branch name bound (`checkpoint.go:46`).
- Error → `checkpoint: reset to %q: %w` (`checkpoint.go:47`).
- Semantics per doc: hard-resets the **current branch** to the commit the named checkpoint branch points to, discarding all working-set changes and any Dolt commits made after the checkpoint (`checkpoint.go:39-41`). No checkout, no stash, no branch switching. Deleting the checkpoint branch is not part of reset.
- Pinned by `TestCheckpointResetReverts`: a row committed before the checkpoint survives; a row committed after it is gone (`checkpoint_test.go:115-181`).

### 5.5 `ListCheckpoints(ctx, prefix)` (`checkpoint.go:54-81`)

- SQL: `SELECT name, hash FROM dolt_branches WHERE name LIKE ? ORDER BY name` with the bind value `prefix + "-%"` (`checkpoint.go:55-58`).
- Errors: `checkpoint: list branches: %w` (`checkpoint.go:60`), `checkpoint: scan branch: %w` (`checkpoint.go:67`), `checkpoint: iterate branches: %w` (`checkpoint.go:77`).
- Each row is passed to `parseCheckpointName(name, prefix)`; rows that do not parse are **silently skipped**, not errors (`checkpoint.go:69-72`).
- `cp.Anchor` is overwritten with the branch's current `hash` column from `dolt_branches` (`checkpoint.go:73`) — so a listed checkpoint's Anchor reflects where the branch points now, while a freshly created one's Anchor is the HEAD read at creation.
- Final ordering is a `sort.Slice` by `CreatedAt.Before` — **oldest first** (`checkpoint.go:79`), pinned by `TestCheckpointSortedOldestFirst` (`checkpoint_test.go:277-310`).
- Prefix isolation pinned by `TestCheckpointListExcludesOtherPrefixes` (`checkpoint_test.go:68-111`).

### 5.6 `PruneCheckpoints(ctx, prefix, retain)` — retention (`checkpoint.go:85-102`)

- `retain < 0` → `checkpoint: retain must be non-negative, got %d` (`checkpoint.go:86-88`).
- Lists via `ListCheckpoints` (error propagated unchanged) (`checkpoint.go:89-92`).
- `len(cps) <= retain` → no-op, returns nil (`checkpoint.go:93-95`).
- Otherwise deletes `cps[:len(cps)-retain]` — the **oldest** ones, since the list is oldest-first (`checkpoint.go:96`).
- Delete SQL: `CALL DOLT_BRANCH('-d', '-f', ?)` — forced branch delete (`checkpoint.go:97`). Error → `checkpoint: delete branch %q: %w` and the loop aborts immediately (`checkpoint.go:98`).
- `retain = 0` deletes all (`checkpoint.go:83-84`), pinned by `TestCheckpointPruneZeroDeletesAll` (`checkpoint_test.go:244-273`). `TestCheckpointPruneEnforcesRetention` creates 7 and retains 3, asserting the surviving names are exactly the newest 3 (`checkpoint_test.go:185-240`).

### 5.7 `parseCheckpointName(name, prefix)` (`checkpoint.go:106-121`)

- `needle := prefix + "-"`; rejects when `len(name) <= len(needle)` or the prefix does not match exactly (`checkpoint.go:107-110`).
- Suffix must parse via `fmt.Sscanf(suffix, "%d", &ns)` **and** round-trip: `fmt.Sprintf("%d", ns) == suffix` — this rejects leading zeros, `+`-signs, and trailing garbage (`checkpoint.go:111-115`).
- On success returns `Checkpoint{Name: name, Prefix: prefix, CreatedAt: time.Unix(0, ns).UTC()}` — **`Anchor` is left empty** here; only `ListCheckpoints` fills it (`checkpoint.go:116-120`).
- `TestParseCheckpointName` pins the accept/reject table (`checkpoint_test.go:315-342`): accepts `("pre-migrate-1716998765000000000","pre-migrate")` and `("other-123456789","other")`; rejects non-numeric suffix `pre-migrate-abc`, empty suffix `pre-migrate-`, wrong prefix `not-matching-123`, mismatched prefix arg `("pre-migrate-123","other")`, and no suffix `("pre-migrate","pre-migrate")`.

### 5.8 Who creates/consumes checkpoints, and the retention constants

Constants (`internal/store/migration_runner.go:31-38`):
- `migrationCheckpointPrefix = "pre-migrate"` (`migration_runner.go:31`)
- `migrationCheckpointRetention = 5` (`migration_runner.go:32`)
- `migrationDriftRepairCheckpointPrefix = "pre-drift-repair"` (`migration_runner.go:38`) — deliberately distinct so the two retained sets prune independently (`migration_runner.go:34-37`).

**Startup migration path** `applyPendingMigrations` (`migration_runner.go:553-608`): builds the goose provider first so a provider failure leaves no orphan branch (`migration_runner.go:554-558`, error `construct migration provider: %w`); then `CreateCheckpoint(ctx, "pre-migrate")` before any mutation, error `create migration checkpoint: %w` (`migration_runner.go:564-567`). On `goose.ErrNoNextVersion` (success) it calls `PruneCheckpoints(ctx, "pre-migrate", 5)`, error `prune migration checkpoints: %w` (`migration_runner.go:583-587`). On a goose failure it calls `handleMigrationFailure` and also prunes, **ignoring** that prune's error (`migration_runner.go:589-597`). Each successful step commits with `migrationCommitMessage(result)` and, on failure, `commit migration v%d: %w` (`migration_runner.go:602-605`).

**Failure handling** `handleMigrationFailure` (`migration_runner.go:616-...`): reset first, quarantine second (`migration_runner.go:614-615`). `ResetToCheckpoint(checkpoint.Name)` failure yields `migration v%d failed and Dolt reset to %q failed (%v); restore from dbsnapshot. Root cause: %w` (`migration_runner.go:624-628`). A quarantine insert failure yields `migration v%d failed (reset to %q); quarantine insert failed (%v); restore from dbsnapshot. Root cause: %w` (`migration_runner.go:631-635`). The quarantine record is committed with the message `fmt.Sprintf("migrate: quarantine v%d %s", version, name)` (`migration_runner.go:637`), whose failure yields `migration v%d failed (reset to %q); quarantine commit failed (%v); ...` (`migration_runner.go:639-641`).

**Drift-repair path** `repairVersionContentDriftWithRollback` (`migration_runner.go:1206-1231`): `CreateCheckpoint(ctx, "pre-drift-repair")`, error `create version-content drift repair checkpoint: %w` (`migration_runner.go:1207-1210`); on repair failure it resets, and a reset failure gives `repair version-content drift (detected at v%d %q) failed (%v) and reset to checkpoint %q failed (%v); restore from dbsnapshot` (`migration_runner.go:1214-1217`), while a successful reset gives `repair version-content drift (detected at v%d %q) failed: %w (working set reset to checkpoint %q)` (`migration_runner.go:1219-1222`). On success it commits with `migrationDriftRepairCommitMessage(repaired)` (error `commit version-content drift repair: %w`, `migration_runner.go:1224-1226`) then `PruneCheckpoints(ctx, "pre-drift-repair", 5)` with error `prune version-content drift repair checkpoints: %w` (`migration_runner.go:1227-1229`).

Checkpoint branches are never garbage-collected other than by `PruneCheckpoints`; there is no expiry by age, only by count under a prefix (`checkpoint.go:83-102`).


---

## Behavioral inventory — `internal/store/{adopt,candidate,promote,downgrade}.go`

All claims cite `file:line` in `/Users/bmf/code/links-issue-tracker`. Derived from Go source and `_test.go` files only.

---

## 0. Shared primitives these four flows depend on (cited for completeness)

| Fact | Value | Citation |
|---|---|---|
| Canonical database directory name | `const doltDatabaseName = "links"` | `internal/store/store.go:28` |
| Engine access enum | `engineRead engineAccess = iota`, `engineWrite` | `internal/store/store.go:40-41` |
| Path validation | `validateDoltRootDir` rejects `strings.TrimSpace(doltRootDir) == ""` with error text `"dolt root dir is required"`, otherwise returns `filepath.Clean(doltRootDir)` | `internal/store/store.go:324-329` |
| Workspace lock file path | `filepath.Join(filepath.Dir(filepath.Clean(databasePath)), ".links-workspace.lock")` — a **sibling** of the dolt dir | `internal/store/workspace_lock.go:71-74` |
| Exclusive lock | `LockWorkspaceExclusive` → `acquireWorkspaceLock(ctx, doltRootDir, true, 1, 0)` — **1 attempt, 0 delay, no retry**; on `ErrWorkspaceBusy` wraps with `"another lit process is using this workspace; close other lit commands and retry: %w"` | `internal/store/workspace_lock.go:118-124` |
| Shared lock | `acquireWorkspaceShared` → 100 attempts × 50ms (`workspaceSharedRetryAttempts = 100`, `workspaceSharedRetryDelay = 50 * time.Millisecond`, ~5s cap); busy message: `"a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w"` | `internal/store/workspace_lock.go:55-61`, `:81-89` |
| Busy sentinel | `var ErrWorkspaceBusy = errors.New("workspace busy")` | `internal/store/workspace_lock.go:53` |
| `dirExists` | `info, err := os.Stat(path); return err == nil && info.IsDir()` | `internal/store/store.go:2685-2688` |
| Dolt pool shape | `sql.OpenDB(connector)` with `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)` | `internal/store/store.go:2672-2683` |
| Connector config | `embedded.Config{Directory: filepath.Clean(doltRootDir), CommitName: author, CommitEmail: fmt.Sprintf("%s@links.local", author), Database: database, DisableSingletonCache: true}`; author = trimmed workspaceID, `""`→`"links"`, `@`→`_`; `engineWrite` also sets `cfg.BackOff = newEngineOpenBackOff()` | `internal/store/store.go:2647-2666` |
| Procedure call builder | `CALL <PROC>()` when no args, else `CALL <PROC>(?,?,…)`; `callIntProcedure` scans **one int64 status column** | `internal/store/sync.go:823-830`, `:849-856` |
| Snapshots dir | `filepath.Join(filepath.Dir(filepath.Clean(databaseDir)), "snapshots")` | `internal/store/migrate_snapshot.go:177-180` |
| Stamped-snapshot shape | `<all-digits>-<label>-<all-digits>` | `internal/store/migrate_snapshot.go:67-81` |
| Baseline | `const baselineVersion = migrations.Baseline`; `const Baseline int64 = 1` | `internal/store/migration_runner.go:226`, `internal/store/migrations/bounds.go:19` |
| Goose table | `const gooseVersionTable = "goose_db_version"` | `internal/store/migration_runner.go:217` |

---

## 1. ADOPT (`internal/store/adopt.go`, `adopt_test.go`)

### 1.1 What is adopted

A **remote Dolt database is cloned wholesale into the local dolt root**. It is not a fetch and not an in-place adoption of an existing dolt directory: `AdoptRemoteByClone` "bootstraps the local store by CLONING the remote's history wholesale, writing it directly into doltRootDir as the database's first on-disk state" (`adopt.go:208-210`). The clone primitive is chosen because on a git-backed remote the fetch path re-inflates the archive blob per chunk read (`adopt.go:212-220`).

Any **pre-existing** database directory at the target (an empty bootstrap store, or an interrupted adopt's residue) is **set aside by rename, never deleted** (`adopt.go:228-234`).

### 1.2 Constants and paths

| Item | Literal | Citation |
|---|---|---|
| Marker filename | `const adoptPendingMarkerName = ".links-adopt-pending"` | `adopt.go:27` |
| Marker path | `func AdoptPendingMarkerPath(databasePath string) string { return filepath.Join(filepath.Clean(databasePath), adoptPendingMarkerName) }` — **inside** the dolt root, sibling of the `links` database dir (not at the dirname position the locks use) | `adopt.go:33-35`, rationale `adopt.go:16-26` |
| Marker temp-file pattern | `os.CreateTemp(cleanRoot, adoptPendingMarkerName+".tmp-*")` → `.links-adopt-pending.tmp-*` | `adopt.go:71` |
| Database dir | `dbDir := filepath.Join(cleanRoot, doltDatabaseName)` → `<root>/links` | `adopt.go:300` |
| Displacement dir | `displaced := fmt.Sprintf("%s.adopt-displaced-%d", cleanRoot, time.Now().UTC().UnixNano())` — a **sibling of the dolt root** (prefix is `cleanRoot`, not `dbDir`) | `adopt.go:302` |
| Singleton cache keys | `filepath.ToSlash(filepath.Join(dbDir, ".dolt", "noms"))` and `filepath.ToSlash(filepath.Join(dbDir, ".dolt", "stats", ".dolt", "noms"))` | `adopt.go:383-384` |

### 1.3 Marker payload

```go
type adoptPendingMarker struct {
    StartedAt string `json:"started_at"`
    Remote    string `json:"remote"`
    Branch    string `json:"branch"`
}
```
(`adopt.go:40-44`). `StartedAt` is `now.UTC().Format(time.RFC3339)` (`adopt.go:64`). Doc states **presence** is the semantic; unreadable/garbage content still condemns (`adopt.go:38-39`).

Sentinel: `var errAdoptPending = errors.New("adopt pending")` — unexported, wrapped by every marker-present refusal so `LocalHasTickets` can discriminate (`adopt.go:47-49`).

### 1.4 `writeAdoptPendingMarker(cleanRoot, remote, branch string, now time.Time) error` — `adopt.go:62-98`

Exact ordered steps:
1. `json.Marshal(adoptPendingMarker{...})`; on error → `"encode adopt-pending marker: %w"` (`adopt.go:63-70`).
2. `os.CreateTemp(cleanRoot, ".links-adopt-pending.tmp-*")`; on error → `"write adopt-pending marker: %w"` (`adopt.go:71-74`).
3. `f.Write(payload)`; on error → `errors.Join(fmt.Errorf("write adopt-pending marker: %w", err), f.Close(), os.Remove(f.Name()))` (`adopt.go:75-77`).
4. `f.Sync()`; on error → `errors.Join(fmt.Errorf("sync adopt-pending marker: %w", err), f.Close(), os.Remove(f.Name()))` (`adopt.go:78-80`).
5. `f.Close()`; on error → `errors.Join(fmt.Errorf("close adopt-pending marker: %w", err), os.Remove(f.Name()))` (`adopt.go:81-83`).
6. `os.Rename(f.Name(), AdoptPendingMarkerPath(cleanRoot))`; on error → `errors.Join(fmt.Errorf("install adopt-pending marker: %w", err), os.Remove(f.Name()))` (`adopt.go:84-86`).
7. Best-effort directory fsync: `os.Open(cleanRoot)` then `_ = dir.Sync(); _ = dir.Close()` — **errors deliberately ignored**, with the stated reason that Windows cannot fsync a directory handle and there is no recovery action (`adopt.go:87-96`).

### 1.5 `clearAdoptPendingMarker(cleanRoot string) error` — `adopt.go:102-107`

`os.Remove(AdoptPendingMarkerPath(cleanRoot))`; `os.ErrNotExist` is treated as success; any other error → `"clear adopt-pending marker: %w"`.

### 1.6 `requireNoPendingAdopt(cleanRoot string) error` — `adopt.go:124-145`

1. `os.ReadFile(AdoptPendingMarkerPath(cleanRoot))`.
2. `errors.Is(err, os.ErrNotExist)` → `nil` (the **only** nil path).
3. Any other read error → `"read adopt-pending marker: %w"`.
4. Default description `interrupted := "a backlog adopt"`; if `json.Unmarshal` succeeds **and** `marker.Remote != ""` **and** `marker.Branch != ""`, it becomes `fmt.Sprintf("the adopt of %s/%s started %s", marker.Remote, marker.Branch, marker.StartedAt)` (`adopt.go:133-137`).
5. Returns, verbatim (`adopt.go:138-144`):

```
%w: a `lit init` backlog adopt was interrupted before completing (%s; marker %s), so the on-disk store is that adopt's leftover partial state, not a usable backlog. Run `lit init` to retry: it sets the leftover aside and re-clones the remote backlog. If the remote no longer carries the backlog, delete %s to abandon the adopt and start fresh
```
with args `errAdoptPending, interrupted, path, cleanRoot`.

`PendingAdopt(databasePath string) error` is the exported passthrough (`adopt.go:156-158`), documented as advisory/outside the workspace lock; the binding refusal is the post-lock check inside each open (`adopt.go:147-155`).

### 1.7 Where the refusal is enforced (post-lock, five entry points)

| Entry point | Call site |
|---|---|
| `Open` | `internal/store/store.go:134` (after `acquireWorkspaceShared`, before `ensureDoltDatabase`) |
| `OpenForRead` | `internal/store/store.go:202` |
| `EnsureDatabase` | `internal/store/store.go:292` |
| `OpenSync` | `internal/store/sync.go:53` |
| `DumpRaw` | `internal/store/rawdump.go:90` |

The placement rule is documented at `store.go:125-133`: a pre-lock check is stale because a live adopt holds the workspace lock exclusively, so "marker-with-acquirable-lock always means a DEAD adopt". `validateOpenArgs` deliberately does **not** contain the check (`store.go:298-303`).

External callers: `internal/cli/snapshots.go:161`, `internal/cli/init_sync.go:191`.

### 1.8 `LocalHasTickets(ctx, doltRootDir, workspaceID) (bool, error)` — `adopt.go:167-203`

Ordered:
1. `validateDoltRootDir(doltRootDir)`; error returns `(false, err)` (`adopt.go:168-171`).
2. `requireNoPendingAdopt(cleanRoot)`: if the error `errors.Is(err, errAdoptPending)` → returns `(false, nil)` — residue is "nothing to lose" **without opening it**; any other (I/O) error → `(false, err)` (`adopt.go:184-189`).
3. `if !dirExists(filepath.Join(cleanRoot, doltDatabaseName))` → `(false, nil)`; **does not create the store** (`adopt.go:190-192`).
4. `OpenForRead(ctx, cleanRoot, workspaceID)`, `defer s.Close()` (`adopt.go:193-197`).
5. `s.LocalIssueCount(ctx)` (defined `internal/store/sync.go:328`); returns `(count > 0, nil)` (`adopt.go:198-202`).

External caller: `internal/cli/init_sync.go:203`.

### 1.9 `AdoptRemoteByClone(ctx, doltRootDir, workspaceID, remoteName, remoteURL, branch string) (err error)` — `adopt.go:244-352`

**Full step sequence, in order:**

1. `cleanRoot, err = validateDoltRootDir(doltRootDir)` (`adopt.go:245-248`).
2. `if strings.TrimSpace(workspaceID) == ""` → `errors.New("workspace id is required")` (`adopt.go:249-251`).
3. `remoteName`, `remoteURL`, `branch` each `strings.TrimSpace`d (`adopt.go:252-254`).
4. If any of the three is empty → `fmt.Errorf("adopt by clone requires a remote name, url, and branch (got name=%q url=%q branch=%q)", remoteName, remoteURL, branch)` (`adopt.go:255-257`).
5. `os.MkdirAll(cleanRoot, 0o755)`; on error → `"create dolt root dir: %w"`. Reason given: the server root must exist before the sibling lock file can be taken and before the clone engine opens (`adopt.go:259-263`).
6. `release, err := LockWorkspaceExclusive(ctx, cleanRoot)` — returns the error unchanged on failure; `defer` joins any release error into the named return `err` (`adopt.go:264-272`).
7. `writeAdoptPendingMarker(cleanRoot, remoteName, branch, time.Now())` — **before the first destructive act**; on error returns immediately, nothing destructive has run (`adopt.go:283-285`).
8. `dbDir := filepath.Join(cleanRoot, doltDatabaseName)`; `evictSingleton(dbDir)` — eviction precedes the rename so no cached handle serves the displaced or re-cloned store stale (`adopt.go:300-301`, rationale `:296-298`).
9. `displaced := fmt.Sprintf("%s.adopt-displaced-%d", cleanRoot, time.Now().UTC().UnixNano())`; `os.Rename(dbDir, displaced)`. `os.ErrNotExist` = nothing to displace (proceed); any other error → `"set aside database before adopt: %w"`, **aborting with the marker still in place** (`adopt.go:302-305`).
10. `cloneRemoteDatabase(ctx, cleanRoot, workspaceID, remoteName, remoteURL, branch)` (§1.10).
11. **Clone-failure arm** (`adopt.go:307-330`): `evictSingleton(dbDir)`; then `os.RemoveAll(dbDir)` **unconditionally** (no stat gate — a transient stat error must not let the removal be skipped while the marker is cleared, `:314-318`). Note this arm **deletes** rather than displaces, because the dbDir is this run's own partial clone (`:317-319`).
    - `RemoveAll` fails → `errors.Join(cloneErr, fmt.Errorf("clean up partial clone: %w", rmErr))` — marker stays.
    - `clearAdoptPendingMarker` fails → `errors.Join(cloneErr, clearErr)`.
    - else → returns `cloneErr` alone.
12. **Post-clone validation**: `if !dirExists(dbDir)` → `fmt.Errorf("clone of remote %q produced no %q database", remoteName, doltDatabaseName)`; **the marker is deliberately left in place** (`adopt.go:331-336`).
13. `clearAdoptPendingMarker(cleanRoot)` is the last act; on failure returns (`adopt.go:345-350`):
```
the backlog cloned completely, but the adopt-completion marker could not be cleared, so the store stays refused until a retry of `lit init` completes (the retry sets this download aside and re-clones; no remote data is at risk): %w
```
14. `return nil`.

**Stated postcondition (two states, never three)** (`adopt.go:236-243`): nil return ⇒ database dir holds the complete cloned backlog and no marker remains; error return ⇒ no partial database remains at the canonical path either — *or*, when cleanup/marker-clear could not complete, the durable marker remains so the leftover is never opened as a store.

**Caller precondition** (`adopt.go:222-227`): the caller must NOT open the store before calling, because cloning straight into the canonical path keeps dolt's in-process singleton chunk-store cache honest. External caller: `internal/cli/init_sync.go:141`.

### 1.10 `cloneRemoteDatabase` — the exact Dolt invocation (`adopt.go:359-370`)

```go
db, err := openDoltPool(serverRoot, workspaceID, "", engineWrite)   // no current database
...
callIntProcedure(ctx, db, "DOLT_CLONE",
    "--remote", remoteName, "--branch", branch, remoteURL, doltDatabaseName)
```
i.e. the SQL executed is `CALL DOLT_CLONE(?,?,?,?,?,?)` with args `["--remote", remoteName, "--branch", branch, remoteURL, "links"]` — wait, six placeholders for six args: `--remote`, `<remoteName>`, `--branch`, `<branch>`, `<remoteURL>`, `links` (`adopt.go:365-366`; builder at `sync.go:849-856`). `defer db.Close()` (`adopt.go:364`).

Errors:
- pool open → `"open dolt for clone: %w"` (`adopt.go:362`).
- procedure → `fmt.Errorf("clone remote %q (%s) branch %q: %w", remoteName, remoteURL, branch, err)` (`adopt.go:367`).

Documented behavior: the git-backed remote defaults to the `refs/dolt/data` ref — the ref lit's sync push writes — so no explicit ref is passed (`adopt.go:356-358`).

### 1.11 `evictSingleton(dbDir string)` — `adopt.go:382-385`

Two best-effort calls, both return values discarded:
```go
_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "noms")), false)
_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "stats", ".dolt", "noms")), false)
```
Documented: lit's own opens bypass the cache (`DisableSingletonCache: true`, `store.go:2655`), so any entry found was left by a dolt-internal load path (e.g. during `DOLT_CLONE`); the entry is **dropped, not closed**, because closing the carcass a second time trips dolt's refcount assert on shared archive readers (`adopt.go:372-381`).

### 1.12 Idempotency / re-run behavior

- Re-running over a **successfully adopted** store: the store exists, so step 9 renames it to a new `.adopt-displaced-<ns>` sibling and re-clones. Pinned by `TestAdoptRemoteByCloneBootstrapsAndReAdopts` (`adopt_test.go:51-75`), which adopts twice into the same `consumer` root and asserts the seeded issue is readable after each (`adopt_test.go:64`, `:74`).
- Re-running after a **returned** clone failure: no residue, so nothing to displace; pinned by `TestAdoptRemoteByCloneFailedCloneLeavesNoResidue` (`adopt_test.go:94-121`).
- Re-running over **abandoned residue**: pinned by `TestAdoptRemoteByCloneHealsAbandonedAdoptResidue` (`adopt_test.go:132-180`).

### 1.13 Exact test assertions (adopt)

`TestLocalHasTicketsDoesNotCreateStore` (`adopt_test.go:16-42`):
- `LocalHasTickets(ctx, root, "ws")` on an absent root returns `(false, nil)` (`:21-27`).
- `dirExists(filepath.Join(root, doltDatabaseName))` must be false afterwards — "LocalHasTickets created the store; it must only observe, never create" (`:28-30`).
- After `EnsureDatabase(ctx, root, "ws")`, `LocalHasTickets` still returns `(false, nil)` (`:32-41`).

`TestAdoptRemoteByCloneBootstrapsAndReAdopts` (`adopt_test.go:51-75`): remote URL is `"file://" + filepath.Join(base, "remote")` (`:57`), branch `"master"`, remote name `"origin"` (`:61`); `LocalHasTickets` after adopt = `true` (`:65`). Helper `assertHasIssueAfterAdopt` does `OpenForRead(ctx, root, "ws")` + `st.GetIssue(ctx, id)` (`:77-87`).

`TestAdoptRemoteByCloneFailedCloneLeavesNoResidue` (`adopt_test.go:94-121`):
- Adopt with branch `"branch-the-remote-does-not-have"` must error (`:103-105`).
- `dirExists(filepath.Join(consumer, doltDatabaseName))` must be false (`:106-108`).
- `os.Stat(AdoptPendingMarkerPath(consumer))` must be `IsNotExist` (`:109-111`).
- The retry with `"master"` succeeds and again leaves no marker (`:114-119`).

`TestAdoptRemoteByCloneHealsAbandonedAdoptResidue` (`adopt_test.go:132-180`):
- Fabricates residue: `os.MkdirAll(<consumer>/links, 0o755)` + `os.WriteFile(<consumer>/links/not-a-database, []byte("junk"), 0o644)` (`:141-147`).
- Writes **garbage** marker content `[]byte("not json")` at `AdoptPendingMarkerPath(consumer)` mode `0o644` (`:148-152`) — pins that presence, not parseability, condemns.
- `LocalHasTickets` returns `(false, nil)` over that residue (`:154-160`).
- Adopt over the residue succeeds, marker gone (`:162-167`), issue readable (`:168`).
- `filepath.Glob(consumer + ".adopt-displaced-*")` must return **exactly one** entry (`:173-176`), and `<displaced>/not-a-database` must still exist — "displacement must preserve bytes" (`:177-179`).

`TestEnsureDatabaseContendsWithWorkspaceExclusiveHolder` (`adopt_test.go:187-206`): while `LockWorkspaceExclusive` is held, `EnsureDatabase(ctx, root, "ws")` returns an error satisfying `errors.Is(err, ErrWorkspaceBusy)` (`:203-205`).

`TestMarkerRefusalIsReservedForDeadAdopts` (`adopt_test.go:213-243`): with the marker written and the exclusive hold **live**, `OpenForRead` must be `ErrWorkspaceBusy` and must **not** contain the substring `"interrupted"` (`:228-234`); after `release()`, `OpenForRead` must error containing `"interrupted"` (`:239-242`).

`TestPendingAdoptMarkerCondemnsEveryNormalOpen` (`adopt_test.go:252-296`): with the marker present, each of `Open`, `OpenForRead`, `EnsureDatabase`, `OpenSync`, `DumpRaw` must return a non-nil error whose text contains all three substrings `"interrupted"`, `"origin/master"`, `"lit init"` (`:263-284`). After `os.Remove(AdoptPendingMarkerPath(root))`, `OpenForRead` succeeds — "the marker must be the sole condemner" (`:286-295`).

---

## 2. CANDIDATE (`internal/store/candidate.go`, `candidate_test.go`, `candidate_posix_test.go`)

### 2.1 What a candidate IS

```go
type Candidate struct {
    store        *Store
    root         string
    expectedHead string
    workspaceID  string
}
```
(`candidate.go:30-42`). It is "one disposable, fully isolated rebuild of a workspace: a fresh Dolt directory at the current baseline, loaded with the domain data a validated (dump, mapping) produced" (`candidate.go:11-14`).

A candidate **owns one directory TREE**, not just the dolt dir: the dolt workspace is nested one level **inside** `root` because `Open` writes the workspace lock and migration snapshots as siblings of the dolt directory; rooting at the parent brings those siblings inside the owned tree so one `RemoveAll(root)` is total (`candidate.go:25-29`).

`expectedHead` and `workspaceID` are the dump's provenance, stamped from the one dump at build time (`candidate.go:34-41`).

There is **no exported accessor** for the store — deliberately, so no caller above `internal/store` holds a concrete engine (`candidate.go:120-128`).

### 2.2 Naming scheme (quoted)

```go
root, err := os.MkdirTemp(parentDir, "lit-candidate-*")
```
(`candidate.go:85`). The `*` is replaced by `os.MkdirTemp` with a random number, so directories are `lit-candidate-<random>`. `parentDir == ""` means the system temp dir (`candidate.go:70-73`).

The dolt workspace is at a fixed child name:
```go
st, err = Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)
```
(`candidate.go:108`), and `detachForPromotion` re-derives the same literal: `doltDir := filepath.Join(c.root, "workspace")` (`candidate.go:148`).

`parentDir` for the recovery loop is `filepath.Dir(canonicalDoltDir)` (`internal/store/recover.go:128`).

### 2.3 `ErrInvalidMapping`

`var ErrInvalidMapping = errors.New("mapping is not applicable to the dump")` (`candidate.go:52`). It tags mapping rejections distinctly from filesystem/store I/O failures so the recovery loop routes them as repair feedback (`candidate.go:44-51`); `Recover`'s attempt loop branches on `errors.Is(err, ErrInvalidMapping)` (`internal/store/recover.go:166-171`).

### 2.4 `RebuildCandidate(ctx, parentDir string, dump RawDump, mapping ShapeMapping) (*Candidate, error)` — `candidate.go:74-118`

Ordered:
1. `export, err := Apply(dump, mapping)` — **pure, runs first**, so an invalid mapping is rejected before any directory or handle exists (`candidate.go:77`, rationale `:58-64`). On error → `fmt.Errorf("%w: %w", ErrInvalidMapping, err)` (`candidate.go:82`). `Apply` is at `internal/store/shapemap.go:501`.
2. `os.MkdirTemp(parentDir, "lit-candidate-*")`; on error → `"create candidate workspace dir: %w"` (`candidate.go:85-88`).
3. Installs the unconditional cleanup defer keyed on a `success bool` (`candidate.go:94-104`): if not successful, `err = errors.Join(err, st.Close())` when `st != nil`, then `err = errors.Join(err, os.RemoveAll(root))`.
4. `Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)`; on error → `"open candidate workspace: %w"` (`candidate.go:108-111`).
5. `st.ReplaceFromExport(ctx, export)` (defined `internal/store/import_export.go:138`); on error → `"load export into candidate: %w"` (`candidate.go:112-114`).
6. `success = true`; returns `&Candidate{store: st, root: root, expectedHead: dump.DoltHead, workspaceID: dump.WorkspaceID}` (`candidate.go:116-117`).

Documented: `dump` is read-only and reusable unchanged across attempts; `Apply` never mutates it, so two attempts from one dump yield identical candidates (`candidate.go:66-68`).

### 2.5 `detachForPromotion() (string, error)` — `candidate.go:137-155`

1. `if c.root == ""` → `errors.New("candidate has no workspace to promote (already discarded or never built)")` — the stated reason is that `filepath.Join("", "workspace")` would yield a cwd-relative `"workspace"` a promotion would rename into the canonical location (`candidate.go:138-146`).
2. `doltDir := filepath.Join(c.root, "workspace")`.
3. If `c.store != nil`: `err = c.store.Close()`, then `c.store = nil` (so a later `Discard`'s close is a no-op by its own state).
4. Returns `(doltDir, err)` — **the doltDir is returned even when Close errored**.

`c.root` is **not** cleared: the candidate still owns the scratch siblings, which a later `Discard` removes; only the dolt directory leaves ownership (`candidate.go:132-136`).

### 2.6 `Discard() error` — `candidate.go:166-179`

Two independently-tracked resources, each released against **its own field**, not a shared flag (`candidate.go:157-165`):
1. If `c.store != nil`: `err = c.store.Close()`; `c.store = nil`.
2. If `c.root != ""`: `os.RemoveAll(c.root)`. On removal error → `errors.Join(err, rmErr)` and **`c.root` is left set**, so a later `Discard` retries. Only on success is `c.root = ""`.
3. Returns `err`.

Idempotent: a caller may `defer Discard` and still discard explicitly on the reject path (`candidate.go:164-165`).

### 2.7 Discovery / enumeration / GC

There is **no enumeration or garbage-collection of candidate directories in this file**. Cleanup is per-candidate only: the deferred `RemoveAll(root)` on the build-failure path (`candidate.go:103`) and `Discard`'s `RemoveAll` (`candidate.go:173`). No code scans `parentDir` for `lit-candidate-*`; the guarantee asserted instead is zero residue per attempt (`candidate.go:16-23`). The recovery loop discards each non-reconciling candidate before the next pass (`internal/store/recover.go:172-177`).

### 2.8 Exact test assertions (candidate)

Fixture `preGooseDump()` (`candidate_test.go:13-33`): `WorkspaceID: "legacy-ws"`, **no `DoltHead` field set** (this is what makes it the missing-provenance shape used in promote tests); tables `issues` (2 rows, `i1` todo/`i2` done), `relations` (0 rows), `comments` (1), `labels` (1), `issue_events` (1), `issue_event_changes` (1); timestamps `"2026-01-01T00:00:00Z"` / `"2026-01-02T00:00:00Z"`.

`TestRebuildCandidateValidMappingYieldsFreshWorkspace` (`candidate_test.go:57-81`): `RebuildCandidate(ctx, t.TempDir(), dump, mustMap(t, dump))` succeeds; `cand.store.Doctor(ctx)` must be clean (`mustClean`); `cand.store.Export(ctx)` must carry exactly 2 issues (`:78-80`).

`TestRebuildCandidateRejectLeavesZeroResidue` (`candidate_test.go:87-130`):
- `RebuildCandidate(ctx, parent, dump, ShapeMapping{})` must error — an empty mapping is not total over the dump's columns (`:94-96`).
- `dirEntryCount(t, parent)` must be **0** after the rejection (`:97-99`).
- A subsequent valid attempt under the same parent yields 2 issues (`:104-118`).
- After `cand.Discard()`, `dirEntryCount(t, parent)` must again be **0** — explicitly including "the workspace lock and migration snapshots Open writes as siblings of the dolt directory. This is the guarantee a flat dolt-dir layout silently broke" (`:120-129`).

`TestRebuildCandidateAttemptsAreIsolated` (`candidate_test.go:136-168`): two candidates from one dump under one parent; `first.Discard()` twice must both return nil (idempotence, `:153-159`); `second.store.Export(ctx)` still yields 2 issues (`:161-167`).

### 2.9 POSIX-specific behavior (`candidate_posix_test.go`)

Build tag: `//go:build !windows` (`candidate_posix_test.go:1`).

`TestDiscardRetriesDirectoryRemoval` (`candidate_posix_test.go:23-59`):
- Skips when `os.Geteuid() == 0` with reason `"removal-permission injection has no effect as root"` — root bypasses the permission check so the injection cannot fail there (`:25-27`, `:20-22`).
- Injection mechanism is **POSIX directory-write-bit semantics**: `os.Chmod(parent, 0o555)` makes the parent unwritable, and removing an entry requires write permission on its parent (`:38-40`, `:19-20`). This is permission semantics, not atomicity or rename semantics — no rename or fsync behavior is exercised in this file.
- Under the unwritable parent, the first `cand.Discard()` **must error** (`:46-48`).
- After `os.Chmod(parent, 0o755)`, the second `cand.Discard()` must return nil (`:49-55`).
- `dirEntryCount(t, parent)` must then be **0** (`:56-58`).
- Rationale pinned: "A single shared release flag would have nulled everything on the first attempt and the retry would no-op, stranding the directory" (`:16-18`).
- Both chmods are also registered via `t.Cleanup` so an early `Fatalf` cannot strand an unwritable parent (`:36`, `:42-44`).

---

## 3. PROMOTE (`internal/store/promote.go`, `promote_test.go`)

### 3.1 What is promoted, from where to where

The candidate's rebuilt **Dolt directory** (`<candidate root>/workspace`) is installed **at the workspace's canonical Dolt path**, in place. The store "lives at a fixed path that consumers cannot be repointed at, so promotion is an in-place swap at that path — never a wipe, never a repoint. Every step is an atomic rename(2)" (`promote.go:14-19`).

```go
type PromotionResult struct {
    Canonical string
    Backup    string
}
```
(`promote.go:24-27`) — "Backup is persisted, not pruned: the pre-recovery copy is the most precious artifact in the flow" (`promote.go:22-23`).

External caller: `internal/cli/lifeboat.go:131`.

### 3.2 `PromoteCandidate(ctx, canonicalDoltDir string, cand *Candidate) (PromotionResult, error)` — `promote.go:44-119`

Exact ordering:

1. `canonicalDoltDir, err = validateDoltRootDir(canonicalDoltDir)` — done **before** deriving the lock path, backup names, and rename target, so a trailing separator cannot make backup naming and backup scanning target different directories (`promote.go:45-53`).
2. `src, err := cand.detachForPromotion()` — **closes the candidate store before any rename**, because an open handle blocks a directory rename on Windows and the promoted store is reopened fresh regardless (`promote.go:54-60`). Error → `"surrender candidate workspace for promotion: %w"`.
3. `release, err := LockWorkspaceExclusive(ctx, canonicalDoltDir)`; the deferred release joins its error into the named return (`promote.go:62-70`). The lock file is a **sibling** of the dolt directory so it is held continuously while the guarded directory is briefly absent (`promote.go:33-37`).
4. `healCanonical(canonicalDoltDir)` — heals a prior crash *before* swapping, so this swap starts from the invariant "canonical present" (`promote.go:72-79`).
5. `verifyHeadUnchanged(ctx, canonicalDoltDir, cand.workspaceID, cand.expectedHead)` — the lost-update gate, run **under the same exclusive lock**, **after heal**, and **before the first rename**, so an abort changes nothing on disk (`promote.go:81-90`).
6. Installs the rollback defer: on any `err != nil` after this point, `healCanonical(canonicalDoltDir)` runs and its error is joined. "Roll BACK, never forward" (`promote.go:92-102`).
7. `backup, err := uniqueBackupPath(canonicalDoltDir, time.Now().UTC().UnixNano())` (`promote.go:104-107`).
8. **Rename 1**: `preserved, err = moveAside(canonicalDoltDir, backup)` (`promote.go:108-112`).
9. **Rename 2**: `os.Rename(src, canonicalDoltDir)`; on error → `"install rebuilt workspace at canonical path: %w"` (`promote.go:113-115`).
10. Returns `PromotionResult{Canonical: canonicalDoltDir, Backup: preserved}` — `Backup` is `""` when nothing pre-existed, "never a phantom path" (`promote.go:116-118`).

**No fsync anywhere in promote.go.** Crash-safety rests entirely on rename atomicity: the only interrupted-at-rest state is "canonical absent, backup present" (`promote.go:38-43`, `:230-233`).

### 3.3 Sentinels and the head gate

- `var ErrWorkspaceAdvanced = errors.New("workspace advanced since dump")` (`promote.go:124`).
- `var ErrMissingDumpProvenance = errors.New("dump has no recorded head commit")` (`promote.go:133`).

`verifyHeadUnchanged(ctx, canonicalDoltDir, workspaceID, expectedHead) error` — `promote.go:148-175`:
1. `if expectedHead == ""` → `fmt.Errorf("%w: cannot verify the live workspace has not advanced; re-run `lit lifeboat dump` against the current workspace and recover from that artifact", ErrMissingDumpProvenance)` (`promote.go:153-155`).
2. `openStoreConnection(ctx, canonicalDoltDir, workspaceID, engineRead)`; error → `"re-read live workspace head: %w"` (`promote.go:156-159`). **Takes no workspace lock** — the caller already holds the exclusive hold (`promote.go:139-142`).
3. Deferred `s.db.Close()`, joined into the named return unless `errors.Is(closeErr, context.Canceled)` (`promote.go:160-164`).
4. `live, err := readDoltHead(ctx, s.db)` (defined `internal/store/rawdump.go:134`); error returned unwrapped (`promote.go:165-168`).
5. `if live != expectedHead` → (`promote.go:170-172`):
```
%w: the candidate was rebuilt from %s but the live workspace is now at %s; a concurrent commit landed during recovery — nothing was changed, re-run recovery against the current state
```
with args `ErrWorkspaceAdvanced, expectedHead, live`.

### 3.4 `HealWorkspace(ctx, canonicalDoltDir string) error` — `promote.go:188-208`

1. `validateDoltRootDir` (else an empty path would put `.links-workspace.lock` in cwd and scan cwd for backups) (`promote.go:189-197`).
2. `LockWorkspaceExclusive`, deferred release joined into `err` (`promote.go:198-206`).
3. `return healCanonical(canonicalDoltDir)`.

No-op when the canonical directory is present, so it is safe to run unconditionally before any recovery (`promote.go:177-187`). External caller: `internal/cli/lifeboat.go:93`.

### 3.5 `moveAside(canonicalDoltDir, backup string) (string, error)` — `promote.go:216-228`

`os.Stat(canonicalDoltDir)`:
- nil error → `os.Rename(canonicalDoltDir, backup)`; on error → `("", "move existing workspace aside: %w")`; else returns `(backup, nil)`.
- `errors.Is(statErr, os.ErrNotExist)` → `("", nil)` — a legitimate no-op; the install proceeds with no backup.
- any other stat error → `("", "stat canonical workspace: %w")`.

### 3.6 `healCanonical(canonicalDoltDir string) error` — `promote.go:239-263`

1. `os.Stat(canonicalDoltDir)`: nil → return `nil` (present, nothing to do); `os.ErrNotExist` → fall through; other → `"stat canonical workspace: %w"`.
2. `backup, err := newestBackup(canonicalDoltDir)`; error propagated.
3. `if backup == ""` → return `nil` (canonical absent and no backup; "this process holds no copy to put back").
4. `os.Rename(backup, canonicalDoltDir)`; on error → `fmt.Errorf("restore canonical workspace from backup %q: %w", backup, err)`.

The restore **consumes** the backup (it is renamed, not copied) — pinned by test at `promote_test.go:276-278`.

### 3.7 Backup naming

```go
path := fmt.Sprintf("%s.backup-%0*d", canonicalDoltDir, promotionStampWidth, nanos)
```
(`promote.go:279`) — i.e. `<canonicalDoltDir>.backup-<19-digit zero-padded UnixNano>`.

`const promotionStampWidth = 19` (`promote.go:296`) — "19 digits holds every int64 UnixNano value (the type overflows in 2262, still 19 digits), so the stamps are equal-width and lexical order equals chronological order" (`promote.go:293-295`).

`uniqueBackupPath(canonicalDoltDir string, nanos int64) (string, error)` — `promote.go:277-288`: loops; `os.Stat(path)`; `os.ErrNotExist` → return path; any other non-nil error → `"probe backup path: %w"`; otherwise `nanos++` and retry. Uniqueness is by construction, not "assumed-unique-because-nanoseconds"; the exclusive lock held across probe and rename keeps a found-free path free (`promote.go:265-276`).

`isPromotionBackup(name, prefix string) bool` — `promote.go:300-311`: `strings.CutPrefix(name, prefix)` must succeed, the suffix must be **exactly** `promotionStampWidth` long, and every rune must be `'0'..'9'`.

`newestBackup(canonicalDoltDir string) (string, error)` — `promote.go:318-342`:
- `dir := filepath.Dir(canonicalDoltDir)`; `prefix := filepath.Base(canonicalDoltDir) + ".backup-"`.
- `os.ReadDir(dir)`; error → `"scan workspace backups: %w"`.
- Selects entries where `e.IsDir() && isPromotionBackup(e.Name(), prefix)` — a stray regular file or a hand-named directory like `"<prefix>manual"` is **not** a workspace and must not be selected (`promote.go:327-334`).
- Empty → `("", nil)`; else `sort.Strings(names)` and return `filepath.Join(dir, names[len(names)-1])`.
- Scan-not-glob is deliberate so a path containing glob metacharacters cannot silently skip a real backup (`promote.go:315-317`).

### 3.8 Locks held during the window

Exactly one: the exclusive workspace hold from `LockWorkspaceExclusive` (`promote.go:62`), held from before `healCanonical` through both renames until the deferred release (`promote.go:66-70`). It is the same hold `lit snapshots restore` takes (`promote.go:33-35`). No commit lock is taken in `promote.go`.

### 3.9 Exact test assertions (promote)

Helpers: `copyTree` (pure-Go recursive copy preserving `info.Mode().Perm()`, `promote_test.go:20-56`); `seedRealWorkspace` (creates issues then `DumpRaw`, so `dump.DoltHead` is the live head, `promote_test.go:63-79`); `freshExportIDs` (copies the tree to a never-before-opened path before opening, because the embedded Dolt driver caches engine state per path within a process and a reopened-then-swapped path can return **stale rows**, `promote_test.go:82-105`); `hasPromotionBackup` (calls `newestBackup`, `promote_test.go:109-116`); `markerDir`/`readMarker` (write/read a `marker` file inside a stand-in directory, `promote_test.go:120-137`).

`TestPromoteCandidateEndToEnd` (`promote_test.go:146-205`): seeds 2 issues; `Recover(ctx, canonical, dump, DeterministicMapper, 1)` yields `Reconciled`; `PromoteCandidate` succeeds; `freshExportIDs(result.Backup)` contains every original ID (`:176-181`); `freshExportIDs(canonical)` has exactly 2 and contains every original ID (`:186-194`); reopened canonical is `Doctor`-clean (`:195-204`).

`TestPromoteCandidateAbortsOnConcurrentCommit` (`promote_test.go:212-257`): after a concurrent `CreateIssue` on the live workspace, `PromoteCandidate` must return an error with `errors.Is(err, ErrWorkspaceAdvanced)` (`:240-243`); `hasPromotionBackup(canonical)` must be **false** — "an aborted promotion made a backup; nothing should have moved" (`:250-252`); the concurrent issue ID is still live (`:253-256`).

`TestHealCanonicalRestoresInterruptedSwap` (`promote_test.go:262-279`): backup literal is `canonical + ".backup-1700000000000000001"`; canonical deliberately absent; after `healCanonical`, `readMarker(canonical) == "original"` and `os.Stat(backup)` must be `IsNotExist` (backup consumed by the restore).

`TestHealCanonicalPicksNewestBackup` (`promote_test.go:283-296`): `.backup-1700000000000000001` = `"older"`, `.backup-1700000000000000002` = `"newer"`; heal restores `"newer"`.

`TestUniqueBackupPathStepsPastCollision` (`promote_test.go:301-319`): with `fmt.Sprintf("%s.backup-%019d", canonical, 1700000000000000001)` already existing, `uniqueBackupPath(canonical, stamp)` must not return that path and the stepped path must still satisfy `isPromotionBackup(filepath.Base(got), filepath.Base(canonical)+".backup-")`.

`TestHealCanonicalIgnoresForeignBackupNames` (`promote_test.go:325-338`): `.backup-1700000000000000001` = `"real"` and `.backup-manual` = `"foreign"` (which sorts lexicographically **after** the numeric stamps); heal must restore `"real"`.

`TestHealWorkspaceRestoresAfterCrash` (`promote_test.go:344-365`): `.backup-1700000000000000007` = `"pre-crash"`; `HealWorkspace` restores it; a second `HealWorkspace` on the now-healthy workspace is a no-op and leaves the marker unchanged.

`TestPromoteCandidateRefusesDumpWithoutProvenance` (`promote_test.go:372-409`): a candidate built from `preGooseDump()` (no `DoltHead`) must fail `errors.Is(err, ErrMissingDumpProvenance)` and must **not** satisfy `errors.Is(err, ErrWorkspaceAdvanced)` (`:391-397`); no backup made; live workspace retains its issues (`:399-408`).

`TestPromoteCandidateRejectsDiscardedCandidate` (`promote_test.go:415-437`): after `cand.Discard()`, `PromoteCandidate` must error, and the canonical workspace marker must still read `"original"` — no swap attempted (`:430-436`).

`TestPromoteCandidateRollsBackOnInstallFailure` (`promote_test.go:443-484`): the install is made to fail deterministically by calling `cand.detachForPromotion()` then `os.RemoveAll(src)`, so the second rename hits a missing source (`:460-469`); `PromoteCandidate` must error (`:471-473`); afterwards `freshExportIDs(canonical)` must contain every original issue — the moved-aside original was restored (`:475-483`).

---

## 4. DOWNGRADE (`internal/store/downgrade.go`, `downgrade_test.go`)

### 4.1 What "downgrade" means concretely

**Schema-version rollback via goose Down migrations**, one Dolt commit per reversed migration, preceded by a recovery snapshot. `Downgrade` "reverses migrations to bring the workspace to targetSchemaVersion, taking a recovery snapshot first and committing one Dolt commit per reversed migration" (`downgrade.go:149-151`). It is invoked only by the `lit downgrade` command; no Open-path code reaches it (`downgrade.go:151-152`). External caller: `internal/cli/downgrade.go:95` with `target.Manifest.Schema.Max`.

### 4.2 Constants

| Constant | Literal | Citation |
|---|---|---|
| `downgradeSnapshotLabel` | `"lit-downgrade"` | `downgrade.go:29` |
| `downgradeSnapshotRetention` | `10` | `downgrade.go:35` |
| (comparison) `migrationSnapshotLabel` | `"pre-migrate"` | `internal/store/migrate_snapshot.go:30` |
| (comparison) `migrationSnapshotRetention` | `10` | `internal/store/migrate_snapshot.go:17` |

Test hook: `var migrationDownForTest func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error)` — when non-nil it replaces `provider.Down(ctx)` inside `applyDownMigrations` (`downgrade.go:18`).

### 4.3 Snapshot naming and classification

`formatDowngradeSnapshotLabel(t time.Time) string` = `fmt.Sprintf("%s-%d", downgradeSnapshotLabel, t.UTC().UnixNano())` → `lit-downgrade-<unix-ns>` (`downgrade.go:293-295`). The trailing timestamp is documented as cosmetic — `dbsnapshot.Take` encodes take-time in the directory name (`downgrade.go:289-292`).

`IsDowngradeSnapshotName(name string) bool` = `isStampedSnapshotName(name, downgradeSnapshotLabel)` (`downgrade.go:51-53`), matching `<unix-ns>-lit-downgrade-<unix-ns>` (`migrate_snapshot.go:67-81`). Documented as disjoint from `IsMigrationSnapshotName` so each producer's retention budget governs only its own snapshots (`downgrade.go:24-28`, `:43-47`).

### 4.4 Error types (every message text)

| Type | Fields | `Error()` format | Citation |
|---|---|---|---|
| `downgradeMigrationFailedError` (unexported) | `Version int64`, `Cause error` | `"down-migrate v%d: %v"` (Version, Cause); `Unwrap() → Cause` | `downgrade.go:58-67` |
| `DowngradeTargetAheadError` | `Current int64`, `Target int64` | `"cannot downgrade to v%d: workspace is already at v%d — that is a forward move; use ` + "`lit upgrade`" + ` instead"` (Target, Current) | `downgrade.go:79-89` |
| `DowngradeBelowBaselineError` | `Target int64` | `"cannot downgrade to v%d: baseline is v%d — going below it would destroy the workspace; restore a pre-upgrade snapshot via ` + "`lit snapshots restore <name>`" + ` instead"` (Target, `baselineVersion`) | `downgrade.go:96-106` |
| `DowngradeRollbackError` | `Snapshot dbsnapshot.Snapshot`, `Cause error` | `"downgrade: %v\n\nthe workspace state before this downgrade is preserved at:\n  %s\n\nto restore, run:\n  lit snapshots restore %s"` (Cause, Snapshot.Path, Snapshot.Name); `Unwrap() → Cause` | `downgrade.go:115-127` |
| `DowngradeIncompleteError` | `Current int64`, `Target int64` | `"downgrade incomplete: goose has no more reversible migrations but recorded version v%d still above target v%d"` (Current, Target) | `downgrade.go:137-147` |

Additional inline error strings:
- Phase guard: `"downgrade: workspace is not goose-managed (no goose_db_version table); run Open first to adopt or initialize"` (`downgrade.go:196-198`).
- Snapshot failure: `fmt.Errorf("downgrade: %w", err)` (`downgrade.go:218`).
- Prune failure: `"prune downgrade snapshots: %w"` (`downgrade.go:229`).
- Provider construction: `"construct downgrade provider: %w"` (`downgrade.go:247`).
- `"down-migrate v%d: goose returned nil result"` (`downgrade.go:271`).
- `"down-migrate v%d: goose result has nil Source"` (`downgrade.go:274`).
- `"commit downgrade revert of v%d: %w"` (`downgrade.go:277`).

### 4.5 Locking

`Downgrade` wraps the entire pipeline in `s.withCommitLock(ctx, ...)` (`downgrade.go:181-185`; lock impl `internal/store/commit_lock.go:322`). Documented: "classify, snapshot, and the Down loop are serialized against every other writer just like migrate()'s mutations are. Acquisition is reentrant: the per-step commitWorkingSet calls inside applyDownMigrations short-circuit because the lock is already held" (`downgrade.go:156-159`). No workspace lock is taken here.

### 4.6 `downgradeLocked(ctx, targetSchemaVersion int64) error` — `downgrade.go:190-232`

Ordered:
1. `state, err := s.classifyMigrationState(ctx)` (impl `internal/store/migration_runner.go:861`); error propagated unwrapped (`downgrade.go:191-194`).
2. **Refusal**: `state.phase != phaseManaged` → the plain "not goose-managed" error, **no snapshot taken** (`downgrade.go:195-199`).
3. **No-op**: `targetSchemaVersion == state.appliedVersion` → `return nil`, no snapshot (`downgrade.go:200-202`).
4. **Refusal**: `targetSchemaVersion > state.appliedVersion` → `&DowngradeTargetAheadError{Current: state.appliedVersion, Target: targetSchemaVersion}` (`downgrade.go:203-205`).
5. **Refusal**: `targetSchemaVersion < baselineVersion` → `&DowngradeBelowBaselineError{Target: targetSchemaVersion}` — refused **before invoking goose**, so the destructive baseline Down is unreachable from this entry point (`downgrade.go:206-208`, rationale `:91-95`).
6. `snapshotsDir := migrationSnapshotsDir(s.doltRootDir)` — the **same directory** migration snapshots use (`downgrade.go:210`).
7. `guard := newSnapshotGuard(s.doltRootDir, snapshotsDir, formatDowngradeSnapshotLabel(time.Now()))`; `snap, err := guard.ensure(ctx)` → `dbsnapshot.Take(ctx, databaseDir, snapshotsDir, label)` (`downgrade.go:211-219`; guard at `migrate_snapshot.go:116-136`). Error → `"downgrade: %w"` (which itself wraps `"snapshot before migration: %w"`, `migrate_snapshot.go:132`).
8. `s.applyDownMigrations(ctx, targetSchemaVersion)`; on **any** error → `&DowngradeRollbackError{Snapshot: snap, Cause: err}` (`downgrade.go:221-223`).
9. `dbsnapshot.PruneMatching(snapshotsDir, downgradeSnapshotRetention, IsDowngradeSnapshotName)` (impl `internal/dbsnapshot/snapshot.go:351`); error → `"prune downgrade snapshots: %w"` (`downgrade.go:228-230`).
10. `return nil`.

Summarized refusal contract, verbatim from the doc comment (`downgrade.go:162-168`): "Refusals (no snapshot taken): target == current applied: no-op, returns nil. target > current applied: DowngradeTargetAheadError. target < baselineVersion: DowngradeBelowBaselineError. workspace not in phaseManaged: a plain error (no goose log to reverse)."

### 4.7 `applyDownMigrations(ctx, target int64) error` — `downgrade.go:244-280`

1. `provider, err := newGooseProvider(s.db)` (impl `internal/store/migration_runner.go:1378`); error → `"construct downgrade provider: %w"`.
2. Unbounded loop:
   a. `current, err := s.recordedMigrationVersion(ctx)` (impl `migration_runner.go:1279`); error propagated (`downgrade.go:250-253`).
   b. `if current <= target` → `return nil` (`downgrade.go:254-256`).
   c. `downOne := provider.Down`, replaced by `migrationDownForTest` when non-nil (`downgrade.go:257-262`).
   d. `result, err := downOne(ctx)`.
      - `errors.Is(err, goose.ErrNoNextVersion)` → `&DowngradeIncompleteError{Current: current, Target: target}` (`downgrade.go:265-267`).
      - any other error → `&downgradeMigrationFailedError{Version: current, Cause: err}` (`downgrade.go:268`).
   e. `result == nil` → `"down-migrate v%d: goose returned nil result"` (current) (`downgrade.go:270-272`).
   f. `result.Source == nil` → `"down-migrate v%d: goose result has nil Source"` (current) (`downgrade.go:273-275`).
   g. `s.commitWorkingSet(ctx, downgradeCommitMessage(result))` (impl `internal/store/commit_lock.go:268`); error → `"commit downgrade revert of v%d: %w"` (`result.Source.Version`, err) (`downgrade.go:276-278`).
   h. Loop repeats — the loop re-reads the recorded version each iteration rather than counting.

Commit message: `downgradeCommitMessage(result) = fmt.Sprintf("downgrade: revert v%d %s", result.Source.Version, filepath.Base(result.Source.Path))` (`downgrade.go:285-287`), stated as symmetric with `migrationCommitMessage`'s `migrate: v<N> <file>` shape (`downgrade.go:282-284`).

### 4.8 What downgrade modifies

- `goose_db_version` rows (mutated by goose's `Down`, "the same way Up does", `downgrade.go:178-180`; the test hook mutates it via `database.NewStore(goose.DialectMySQL, gooseVersionTable).Delete`, `downgrade_test.go:273-279`).
- Whatever DDL/DML each migration's Down section performs.
- One Dolt commit per reversed step via `commitWorkingSet`.
- One new snapshot directory under `<storageDir>/snapshots`, plus pruning of downgrade-labeled snapshots beyond 10.

### 4.9 Exact test assertions (downgrade)

Fixture `openWorkspaceForDowngrade` opens a fresh workspace at registry-max via `Open(ctx, <tmp>/dolt, "test-workspace-id")` (`downgrade_test.go:22-32`). `snapshotCount` counts via `dbsnapshot.List` and **fails the test** if any `dbsnapshot.IsProducerArtifactName(e.Name())` entry (stranded `.tmp`/`.reserve`) is present (`downgrade_test.go:44-61`). `stampGooseVersion` inserts a fake applied row via `gs.Insert(ctx, st.db, database.InsertRequest{Version: version})` (`downgrade_test.go:466-475`).

- `TestAppliedSchemaVersionMatchesRecorded` (`:66-85`): `AppliedSchemaVersion` == `recordedMigrationVersion`, and the recorded version is `> 0`.
- `TestAppliedSchemaVersionZeroForNonManaged` (`:96-121`): after `DROP TABLE goose_db_version`, `AppliedSchemaVersion` must be **0**.
- `TestDowngradeTargetEqualIsNoOp` (`:125-141`): `Downgrade(ctx, current)` returns nil and the snapshot-count delta is **0**.
- `TestDowngradeTargetAheadRefused` (`:145-167`): `Downgrade(ctx, current+5)` → `errors.As` a `*DowngradeTargetAheadError` with `Current == current` and `Target == current+5`; snapshot delta **0**.
- `TestDowngradeBelowBaselineRefused` (`:172-192`): `Downgrade(ctx, baselineVersion-1)` → `*DowngradeBelowBaselineError` with `Target == baselineVersion-1`; message must contain `"would destroy the workspace"`; snapshot delta **0**.
- `TestDowngradeRollbackOnFailure` (`:204-241`, **not parallel** — installs the package-level hook): stamps `registryMax+1`, hook returns `errors.New("synthetic down failure")`; result must be `*DowngradeRollbackError` that `errors.Is` the synthetic cause; `rb.Snapshot.Name` and `rb.Snapshot.Path` non-empty; `os.Stat(rb.Snapshot.Path)` must succeed; `rb.Error()` must contain the literal `"lit snapshots restore "+rb.Snapshot.Name`.
- `TestDowngradeHappyPathSteppedAndCommitted` (`:251-315`, **not parallel**): stamps `vA=registryMax+1` and `vB=registryMax+2`; hook deletes the highest recorded row and returns a `*goose.MigrationResult` whose `Source.Path` is `fmt.Sprintf("/test/%05d_fake.sql", current)`. Asserts: recorded version lands exactly at `registryMax`; snapshot count minus Open's one migration snapshot equals exactly **1**; and `doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")` output contains, for each of vA and vB, `fmt.Sprintf("downgrade: revert v%d %05d_fake.sql", v, v)`.
- `TestDowngradeIncompleteWhenGooseExhausted` (`:322-353`, **not parallel**): hook returns `goose.ErrNoNextVersion`; the outer error is a `*DowngradeRollbackError` whose `Cause` is `errors.As`-able to `*DowngradeIncompleteError` with `Current == registryMax+1`, `Target == registryMax`.
- `TestIsDowngradeSnapshotNameSymmetry` (`:361-377`): with `migName = "1700000000000000000-pre-migrate-1700000000000000001"` and `dgName = "1700000000000000000-lit-downgrade-1700000000000000001"` — `IsMigrationSnapshotName(migName)` true / `IsDowngradeSnapshotName(migName)` false; `IsDowngradeSnapshotName(dgName)` true / `IsMigrationSnapshotName(dgName)` false.
- `TestDowngradeUntouchedOpen` (`:384-419`): a no-op `Downgrade`, `Close`, then re-`Open` yields the identical recorded version.
- `TestDowngradeRequiresGooseManaged` (`:427-461`): after `DROP TABLE goose_db_version` + a commit, `Downgrade(ctx, baselineVersion)` errors with a message containing `"not goose-managed"`; snapshot delta **0**; and the error must **not** be `errors.As`-able to `*DowngradeRollbackError`.
- Compile-time guards: `_ error = (*DowngradeTargetAheadError)(nil)`, `(*DowngradeBelowBaselineError)(nil)`, `(*DowngradeRollbackError)(nil)` (`downgrade_test.go:478-483`).


---

## Locking, Remote Cache, and the Vendored Dolt Driver

All paths below are relative to `/Users/bmf/code/links-issue-tracker`.

---

### 1. The lock primitive: `github.com/promptctl/primitives/filelock`

Vendored in the module cache at `/Users/bmf/go/pkg/mod/github.com/promptctl/primitives@v0.2.0/filelock/`. Every lit-minted lock in `internal/store` goes through `filelock.Acquire`.

#### 1.1 `Acquire` contract

`filelock/filelock.go:53` — `func Acquire(ctx context.Context, lockPath string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, bool, error)`

Sequence, in order:

1. `filelock/filelock.go:57` — if `maxAttempts < 1`, returns `(nil, false, error)` with message `filelock: maxAttempts must be >= 1, got %d`. It does **not** read as contention.
2. `filelock/filelock.go:65` — if `ctx.Err() != nil` at entry, returns `(nil, false, ctx.Err())` **before any attempt**, on a free lock as well as a held one.
3. `filelock/filelock.go:68` — `os.MkdirAll(filepath.Dir(lockPath), 0o755)`; failure returns `ensure lock dir: %w`.
4. `filelock/filelock.go:71` — `os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)`; failure returns `open lock file: %w`. **The lock file is created if absent and its content is never written or read — it is a zero-byte file. No PID, no timestamp, no holder metadata is ever stored in any lit lock file** (`filelock/filelock.go:71` is the only write-mode open, and nothing writes to `file`).
5. `filelock/filelock.go:75-116` — loop `attempt` from `0` to `maxAttempts-1`:
   - `filelock/filelock.go:76` `tryLockFile(file, exclusive)`.
   - On success (`filelock/filelock.go:77`): builds a `release` closure (`filelock/filelock.go:82-92`) that runs `unlockFile(fd)` then `fd.Close()` and returns `errors.Join` of `release file lock: %w` and `close file lock fd: %w`. Then re-checks `ctx.Err()` (`filelock/filelock.go:102`); if done, releases the just-taken hold and returns `errors.Join(ctxErr, release())`. Otherwise returns `(release, true, nil)`.
   - On a non-would-block error (`filelock/filelock.go:107`): returns `lock %s: %w` joined with any FD-close error (`joinWithClose`, `filelock/filelock.go:136`).
   - `filelock/filelock.go:110` — the sleep is **skipped after the final attempt**.
   - `filelock/filelock.go:113` — `SleepWithContext(ctx, delay)`; a cancellation mid-sleep returns `(nil, false, ctx.Err())` joined with any close error.
6. `filelock/filelock.go:117` — after exhaustion, closes the FD; a close failure returns `close file lock fd: %w`.
7. `filelock/filelock.go:127` — if `ctx.Err() != nil`, returns it (cancellation wins over contention).
8. `filelock/filelock.go:130` — otherwise returns `(nil, false, nil)`: **contention is a value, not an error**.

`maxAttempts == 1` is the non-blocking probe and never sleeps (`filelock/filelock.go:38-39` doc, enforced by the `attempt+1 == maxAttempts` break at `filelock/filelock.go:110`).

#### 1.2 Platform implementation

- POSIX (`filelock/filelock_posix.go:21`): `syscall.Flock(fd, LOCK_SH|LOCK_NB)` for shared, `LOCK_EX|LOCK_NB` for exclusive. `EWOULDBLOCK` maps to the internal `errWouldBlock` sentinel (`filelock/filelock.go:35`). Unlock is `syscall.Flock(fd, LOCK_UN)` (`filelock/filelock_posix.go:33`).
- Windows (`filelock/filelock_windows.go:81`): `LockFileEx` over the whole address space (`low=0xFFFFFFFF, high=0xFFFFFFFF`, `filelock/filelock_windows.go:32-33`) with `LOCKFILE_FAIL_IMMEDIATELY = 0x1` (`filelock/filelock_windows.go:35`) and `LOCKFILE_EXCLUSIVE_LOCK = 0x2` (`filelock/filelock_windows.go:36`). `ERROR_LOCK_VIOLATION = 33` (`filelock/filelock_windows.go:38`) maps to `errWouldBlock`.

**Stale handling: there is none, by design.** A hold lives on the open file description, so any process death (SIGKILL included) releases it in the kernel; nothing in the package or in `internal/store` inspects mtime, PID, or age (`filelock/filelock.go:7-11`; `internal/store/doc.go:20-27`).

#### 1.3 The second surface, `filelock.Lock` (not used by the store locks)

`filelock/lock.go:44` — a reusable exclusive handle. Sentinels: `ErrLocked = errors.New("filelock: lock is held")` (`filelock/lock.go:15`), `ErrTimeout = errors.New("filelock: timed out waiting for lock")` (`filelock/lock.go:19`). Poll interval `10 * time.Millisecond` (`filelock/lock.go:25`). `TryLock` (`filelock/lock.go:60`) is one attempt; `Lock` (`filelock/lock.go:68`) polls forever; `LockWithTimeout` (`filelock/lock.go:76`) polls until the deadline then returns `ErrTimeout` — a non-positive timeout is a single immediate attempt. Re-locking a handle that already holds returns `ErrLocked` (`filelock/lock.go:99`). `Unlock` of an unheld handle returns `filelock: unlock of an unheld lock` (`filelock/lock.go:138`).

#### 1.4 Tests pinning the primitive

- `filelock/filelock_test.go:13` shared holders coexist on independent FDs.
- `filelock/filelock_test.go:40` a single-attempt exclusive probe against a live shared holder returns `(acquired=false, err=nil)`, and acquires once the holder releases.
- `filelock/filelock_test.go:75` an already-done context is refused on a free lock and a held lock alike, with `context.Canceled`, and the free lock is left free.
- `filelock/filelock_test.go:123` cancellation during a retry sleep surfaces `context.Canceled` (budget used: 500 attempts × 20ms, cancel at 50ms).
- `filelock/filelock_test.go:156` `maxAttempts` of `0` and `-1` are loud errors.

---

### 2. The store's lock stamping boundary

`internal/store/workspace_lock.go:313` — `acquireStoreLock(ctx, lockPath, exclusive, maxAttempts, delay)` is the **single** place `filelock.Acquire`'s `acquired=false` value becomes a domain error:

```go
release, acquired, err := filelock.Acquire(ctx, lockPath, exclusive, maxAttempts, delay)
if err != nil { return nil, err }
if !acquired { return nil, ErrWorkspaceBusy }
return release, nil
```

`internal/store/workspace_lock.go:53` — `var ErrWorkspaceBusy = errors.New("workspace busy")`. Every store lock's contention wraps this sentinel; `errors.Is(err, ErrWorkspaceBusy)` is the uniform discriminator.

`internal/store/workspace_lock_test.go:176` (`TestWorkspaceBusyErrorsWrapSentinel`) pins that both `acquireWorkspaceShared` and `LockWorkspaceExclusive` refusals satisfy `errors.Is(err, ErrWorkspaceBusy)` (the shared arm also accepts a `"deadline exceeded"` string when the test's 200ms context expires first).

#### 2.1 Declared acquisition order

`internal/store/doc.go:41` — outermost to innermost:

```
workspace → Dolt's own .dolt/noms/LOCK → commit → snapshot producer beacon
```

A holder of an inner lock never waits on an outer one (`internal/store/doc.go:44`). Two locks sit outside the order: the sync-push lock, because every acquisition of it is a non-blocking probe so nothing ever waits on it (`internal/store/doc.go:80-84`); and the mirror liveness beacon, whose acquisitions all happen holding nothing (`internal/store/doc.go:86-98`).

`internal/store/doc.go:60-71` states one tolerated deviation: a GC-contention retry rotates the store's connection mid-mutation, re-acquiring Dolt's LOCK under the held commit lock, bounded at ~30s (`engineOpenRetryMaxElapsed`) strictly inside every commit-lock waiter's ~15-minute budget.

`internal/store/doc.go:100-111` — ONE HOME: every lit-minted lock file sits at `dirname(databasePath)`, so a `lit snapshots restore` that rotates the dolt directory cannot move the lock out from under acquirers. Three stated exceptions: the snapshot producer beacon (inside `snapshots/`), the adopt-pending marker (inside the dolt root), and Dolt's own journal `LOCK`.

`internal/store/doc.go:73-78` — lit formerly minted a second lock `.links-engine.lock` for "one write-capable engine per path"; it is retired, and the name is deliberately not reused.

---

### 3. The workspace lock

#### 3.1 Path

`internal/store/workspace_lock.go:71`:

```go
func WorkspaceLockPath(databasePath string) string {
    cleaned := filepath.Clean(databasePath)
    return filepath.Join(filepath.Dir(cleaned), ".links-workspace.lock")
}
```

Literal filename: `.links-workspace.lock`, a **sibling** of the dolt root directory.

#### 3.2 Modes, budgets, and errors

| Function | Mode | maxAttempts | delay | Total budget | On contention |
|---|---|---|---|---|---|
| `acquireWorkspaceShared` (`workspace_lock.go:81`) | shared | `workspaceSharedRetryAttempts = 100` (`workspace_lock.go:59`) | `workspaceSharedRetryDelay = 50 * time.Millisecond` (`workspace_lock.go:60`) | ~5s (100 attempts, 99 sleeps — `workspace_lock.go:56-58`) | wrapped error, see below |
| `LockWorkspaceShared` (`workspace_lock.go:104`) | shared | same — delegates to `acquireWorkspaceShared` | same | ~5s | same |
| `LockWorkspaceExclusive` (`workspace_lock.go:118`) | exclusive | `1` (`workspace_lock.go:119`) | `0` | single non-blocking attempt | wrapped error, see below |

Exact contention messages:

- Shared (`workspace_lock.go:86`):
  `a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w`
- Exclusive (`workspace_lock.go:121`):
  `another lit process is using this workspace; close other lit commands and retry: %w`

Both wrap `ErrWorkspaceBusy` via `%w`.

Meaning of the two modes (`workspace_lock.go:14-22`): shared marks **directory readers** (store opens, raw dumps, snapshot file walks); exclusive marks **directory rotators** (operations that displace, swap, or rebuild the dolt directory). The exclusive mode refuses immediately on contention with any shared holder rather than waiting (`workspace_lock.go:110-112`).

`acquireWorkspaceLock` (`workspace_lock.go:302`) is the shared body: `acquireStoreLock(ctx, WorkspaceLockPath(doltRootDir), exclusive, maxAttempts, delay)`.

#### 3.3 Tests pinning workspace-lock behavior

- `workspace_lock_test.go:30` shared holders coexist.
- `workspace_lock_test.go:84` exclusive refuses while shared is held.
- `workspace_lock_test.go:109` shared refuses while exclusive is held.
- `workspace_lock_test.go:147` exclusive is released after `Store.Close()`.
- `workspace_lock_test.go:232` (`TestOpenForReadAcquiresLockBeforeStat`) — `OpenForRead` takes the shared lock **before** its database-exists stat, so a concurrent restore that transiently renames the database dir away yields a workspace-busy refusal, never a false "repository not initialized".
- `workspace_lock_test.go:285` `OpenSync` holds the workspace lock.
- `workspace_lock_test.go:477` exclusive holds serialize.

---

### 4. The commit lock

#### 4.1 Path

`internal/store/commit_lock.go:394`:

```go
func commitLockPathForDolt(databasePath string) string {
    cleaned := filepath.Clean(databasePath)
    return filepath.Join(filepath.Dir(cleaned), ".links-commit-flock.lock")
}
```

Literal filename: `.links-commit-flock.lock`, sibling of the dolt directory. Exported as `CommitLockPath(databasePath)` (`commit_lock.go:390`).

`commit_lock.go:396-402` records why the historical name `.links-commit.lock` is **burned and must not be restored**: O_EXCL-era binaries `os.Remove` that path on release and on 10-minute age eviction, and an unlink under a live flock splits the lock across two inodes so the next acquirer runs concurrently with the orphaned holder.

#### 4.2 Budget and errors

`commit_lock.go:76-77`:
```go
commitLockRetryAttempts = 9000
commitLockRetryDelay    = 100 * time.Millisecond
```
= **~15 minutes** wall-clock, always **exclusive** (`commit_lock.go:417`). `commit_lock.go:61-75` states the sizing rationale: the dominant legitimate holder is `takeUserSnapshot` holding across an entire snapshot copy, measured past ten minutes on large stores without reflink.

`acquireCommitLockAtPath` (`commit_lock.go:416`):
```go
release, err := acquireStoreLock(ctx, lockPath, true, commitLockRetryAttempts, commitLockRetryDelay)
if err != nil { return nil, wrapCommitLockContention(err) }
```

`wrapCommitLockContention` (`commit_lock.go:430`) attaches guidance **only** when `errors.Is(err, ErrWorkspaceBusy)`; every other error, cancellation included, passes through untouched:

> `another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes: %w`

`LockCommitPath(ctx, lockPath)` (`commit_lock.go:377`) is the external entry for callers with no open Store (e.g. `lit snapshots new`/`restore`); it routes to the same `acquireCommitLockAtPath`.

#### 4.3 Re-entrancy

`commit_lock.go:92` — `type commitLockContextKey struct{}`. `acquireCommitLock` (`commit_lock.go:357`) checks `ctx.Value(commitLockContextKey{}).(bool)`; if already true it returns the ctx unchanged with a no-op release (`commit_lock.go:359`), so a nested `commitWorkingSet` inside a held mutation never queues behind its own hold. On a fresh acquire it returns `context.WithValue(ctx, commitLockContextKey{}, true)` (`commit_lock.go:365`).

#### 4.4 Release settlement

`SettleCommitLockRelease(opErr, releaseErr)` (`commit_lock.go:346`):
- `releaseErr == nil` → return `opErr`.
- `opErr != nil` → `errors.Join(opErr, releaseErr)`.
- `opErr == nil, releaseErr != nil` → prints to **stderr**:
  `lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): %v\n` (`commit_lock.go:353`)
  and returns `nil` — a durable success is never retroactively failed.

`withCommitLock` (`commit_lock.go:322`) defers the release so it fires on panic too (`commit_lock.go:329-331`).

#### 4.5 Tests pinning commit-lock behavior

- `commit_lock_test.go:22` `TestAcquireCommitLockNeverEvictsLiveHolderByAge`.
- `commit_lock_test.go:70` `TestAcquireCommitLockIgnoresDeadResidue`.
- `commit_lock_test.go:116` `TestWrapCommitLockContention`.
- `commit_lock_test.go:137` `TestSettleCommitLockRelease`.
- `commit_lock_test.go:164` `TestWithMutationResumesAtVersioningAfterStagedCommit`.
- `commit_lock_test.go:206` `TestCommitWorkingSetOnceRendersStamp` — see §5.4.

---

### 5. Mutation sequencing and Dolt commit rendering (commit_lock.go)

#### 5.1 `commitStamp`

`commit_lock.go:104-116`:
```go
type commitStamp struct {
    Message    string
    Date       time.Time // non-zero → --date; Dolt parses to second granularity, sub-second truncates
    Author     string    // non-empty → --author "Name <email>", replacing the session identity
    AllowEmpty bool      // → --allow-empty
}
```

#### 5.2 `withMutation` / `withStampedMutation`

`commit_lock.go:122` — `withMutation(ctx, message, fn)` = `withStampedMutation(ctx, commitStamp{Message: message}, fn)`.

`commit_lock.go:156` — `withStampedMutation` runs, under one held commit lock, inside `retryTransientGCContention`:

1. If not yet staged: `s.db.BeginTx(ctx, nil)` — error wrapped `begin %s tx: %w` with the message (`commit_lock.go:163`).
2. `defer tx.Rollback()` (`commit_lock.go:165`).
3. `fn(ctx, tx)` — error returned unwrapped (`commit_lock.go:166`).
4. `tx.Commit()` — error wrapped `commit %s tx: %w` (`commit_lock.go:170`).
5. `staged = true` (`commit_lock.go:172`).
6. `s.commitWorkingSetOnce(ctx, stamp)` (`commit_lock.go:174`).

The `staged` flag is the resume point: once `tx.Commit()` has succeeded, a retry resumes **at versioning** and never re-runs `fn` (`commit_lock.go:143-155`). The lock is acquired and released exactly once (`commit_lock.go:129`).

#### 5.3 `commitWorkingSet`

`commit_lock.go:268` — `commitWorkingSet(ctx, message)` takes the commit lock and runs `commitWorkingSetOnce(ctx, commitStamp{Message: message})` under `retryTransientGCContention`.

#### 5.4 `commitWorkingSetOnce` — the single Dolt-commit boundary

`commit_lock.go:286`:

1. `commit_lock.go:287` — optional `s.commitWorkingSetHookForTest` runs first; its error aborts.
2. `commit_lock.go:292` — `trimmed := strings.TrimSpace(stamp.Message)`; if empty, **defaults to the literal `"links mutation"`** (`commit_lock.go:294`).
3. `commit_lock.go:296` — base args: `{"-Am", trimmed}` (i.e. `DOLT_COMMIT` always stages all with `-A`).
4. `commit_lock.go:297` — `if stamp.AllowEmpty` append `"--allow-empty"`.
5. `commit_lock.go:300` — `if !stamp.Date.IsZero()` append `"--date", stamp.Date.UTC().Format(time.RFC3339)`.
6. `commit_lock.go:305` — `if stamp.Author != ""` append `"--author", stamp.Author`.
7. `commit_lock.go:311` — `s.db.QueryRowContext(ctx, buildProcedureCall("DOLT_COMMIT", len(args)), args...).Scan(&commitHash)`.
8. `commit_lock.go:316` — an error whose lower-cased text contains `"nothing to commit"` is **success with no commit** (returns `nil`).
9. `commit_lock.go:319` — anything else goes to `wrapCommitWorkingSetError`.

Argument order is therefore always: `-Am <message> [--allow-empty] [--date <RFC3339 UTC>] [--author "Name <email>"]`.

`TestCommitWorkingSetOnceRendersStamp` (`commit_lock_test.go:206`) pins the effect with `Date = 2025-03-07T09:30:45Z`, `Author = "prov-author <prov@example.test>"`, `AllowEmpty = true`, `Message = "stamped provenance probe"`, then asserts via `SELECT committer, email, date, message FROM dolt_log('HEAD') LIMIT 1` (`commit_lock_test.go:224`) that `committer == "prov-author"`, `email == "prov@example.test"`, the date equals the stamp to the second, and the message matches verbatim.

#### 5.5 Transient online-GC contention

`commit_lock.go:34` — `var ErrTransientGCContention = errors.New("transient online-gc contention")`.

Budget (`commit_lock.go:55-59`):
```go
var transientRetryMaxAttempts = 30      // package var so tests can shrink it
const transientRetryBaseDelay = 50 * time.Millisecond
const transientRetryMaxDelay  = 1 * time.Second
```
`transientRetryDelay(attempt)` (`commit_lock.go:250`) = `transientRetryBaseDelay << (attempt-1)`, capped at `transientRetryMaxDelay`. Total ≈ 25s: five uncapped doublings (50, 100, 200, 400, 800ms) then 25 more attempts at the 1s cap (`commit_lock.go:37-39`).

`retryTransientGCContention` (`commit_lock.go:185`) loop, attempts 1..`transientRetryMaxAttempts`:
- `classifyTransientGCError(operation(ctx))`; `nil` → return `nil` (`commit_lock.go:189`).
- If the error is not `ErrTransientGCContention`, or this was the final attempt → break (`commit_lock.go:193`).
- `sleep(ctx, delayForAttempt(attempt))` — a wait error returns immediately (`commit_lock.go:196`).
- `rotate(ctx)` — the connection rotator (`s.reconnect`); a rotate error returns immediately (`commit_lock.go:199`).
- On exit: `exhaustedContentionError(lastErr)`.

`waitWithContext` (`commit_lock.go:264`) delegates to `filelock.SleepWithContext`.

Classification predicates:
- `isManifestReadOnlyError` (`commit_lock.go:483`): lower-cased error text contains **both** `"cannot update manifest"` **and** `"read only"`.
- `isOnlineGCResetError` (`commit_lock.go:495`): lower-cased text contains **both** `"online garbage collection"` **and** `"reconnect"`. The GC-specific phrase is required so the unrelated cluster-role-transition error (which also says "please reconnect") is not misclassified (`commit_lock.go:491-493`).
- `isTransientGCContentionError` (`commit_lock.go:479`) = either of the two.

`transientGCContentionError` (`commit_lock.go:437`) wraps and its `Is(target)` returns `target == ErrTransientGCContention` (`commit_lock.go:449`).

`wrapCommitWorkingSetError` (`commit_lock.go:453`) wraps every commit error as `dolt commit working set: %w`, and additionally tags it as transient when `isTransientGCContentionError`.

`exhaustedContentionError` (`commit_lock.go:243`): if the surviving error is a manifest-read-only, promote it to `WorkspaceWriteBlockedError{Cause: err}`; otherwise pass through unchanged. A persistent GC-reset (not manifest-read-only) is **not** reclassified.

`WorkspaceWriteBlockedError` (`commit_lock.go:216`) has one field `Cause error`; `Error()` (`commit_lock.go:220`) is exactly:

> `another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)`

`Unwrap()` returns `Cause` (`commit_lock.go:232`).

---

### 6. Other lit-minted lock paths in workspace_lock.go

#### 6.1 Sync-push single-flight lock

- Path (`workspace_lock.go:132`): `<dirname(databasePath)>/.links-sync-push.lock`.
- `TryAcquireSyncPushLock(databasePath)` (`workspace_lock.go:146`): `filelock.Acquire(context.Background(), path, true /*exclusive*/, 1, 0)` — a **non-blocking, single-attempt** exclusive probe returning `(release, acquired bool, err)`. `acquired == false` means another mirror holds it and the caller coalesces by doing nothing (`workspace_lock.go:137-145`). Note it uses `context.Background()`, not a caller ctx.
- Pinned by `workspace_lock_test.go:318` (`TestTryAcquireSyncPushLockIsSingleFlight`) and `workspace_lock_test.go:363` (path is a sibling of dolt).

#### 6.2 Mirror liveness beacon

- Path (`workspace_lock.go:155`): `<dirname(databasePath)>/.links-sync-mirror.lock`.
- Budget (`workspace_lock.go:168-169`): `mirrorBeaconRetryAttempts = 20`, `mirrorBeaconRetryDelay = 50 * time.Millisecond` → ~1s.
- `HoldMirrorBeacon(ctx, databasePath)` (`workspace_lock.go:184`): `acquireStoreLock(ctx, path, false /*shared*/, 20, 50ms)`. On `ErrWorkspaceBusy` it deliberately **does not propagate the sentinel** (`workspace_lock.go:186-196`), returning instead:
  > `mirror liveness beacon held exclusively past every probe window (a foreign process holding %s?)`
  with the beacon path interpolated.
- `MirrorBeaconVerdict` (`workspace_lock.go:205`) is an `int` enum: `BeaconUnheld = 0`, `BeaconAnswered = 1`, `BeaconObstructed = 2` (`workspace_lock.go:211-229`). `String()` (`workspace_lock.go:238`) returns `"unheld"`, `"answered"`, `"obstructed"`, and for any other value `fmt.Sprintf("unnamed MirrorBeaconVerdict(%d)", int(v))`.
- `ProbeMirrorBeacon(databasePath)` (`workspace_lock.go:269`) — two single-attempt probes with `context.Background()`, **shared first, exclusive last**:
  1. `filelock.Acquire(ctx, path, false, 1, 0)`. Error → `(BeaconUnheld, "probe mirror liveness beacon (shared step): %w")` (`workspace_lock.go:275`). Not acquired → `(BeaconObstructed, nil)` (`workspace_lock.go:278`). Release failure → `(BeaconUnheld, "release mirror liveness beacon probe (shared step): %w")` (`workspace_lock.go:284`).
  2. `filelock.Acquire(ctx, path, true, 1, 0)`. Error → `(BeaconUnheld, "probe mirror liveness beacon: %w")` (`workspace_lock.go:288`). Not acquired → `(BeaconAnswered, nil)` (`workspace_lock.go:291`). Release failure → `(BeaconUnheld, "release mirror liveness beacon probe: %w")` (`workspace_lock.go:297`).
  3. Otherwise `(BeaconUnheld, nil)`.
- Pinned by `workspace_lock_test.go:383` (`TestMirrorBeaconLivenessProof`) and `workspace_lock_test.go:468` (path is a sibling of dolt).

#### 6.3 Dolt's own journal lock

- Path (`workspace_lock.go:351`): `filepath.Join(filepath.Clean(databasePath), doltDatabaseName, ".dolt", "noms", "LOCK")` — i.e. `<databasePath>/<doltDatabaseName>/.dolt/noms/LOCK`. This is **Dolt's** file, not lit-minted, and is the ONE HOME exception stated at the mint site (`workspace_lock.go:335-343`).
- Budget (`workspace_lock.go:365-366`): `doltJournalRetryDelay = 100 * time.Millisecond`, `doltJournalRetryAttempts = 300` → **~30s**, matching `engineOpenRetryMaxElapsed`.
- `LockDoltJournalExclusive(ctx, databasePath)` (`workspace_lock.go:389`):
  1. `os.Stat(filepath.Dir(lockPath))` **first** — this helper contends on Dolt's lock and never mints Dolt's tree (`workspace_lock.go:391-399`). On `os.ErrNotExist` returns exactly:
     `repository not initialized with lit — run 'lit init' first` (`workspace_lock.go:402`).
     Any other stat failure returns `stat dolt journal dir: %w` (`workspace_lock.go:404`).
  2. `acquireStoreLock(ctx, lockPath, true /*exclusive*/, 300, 100ms)`.
  3. On `ErrWorkspaceBusy`, wraps (preserving the sentinel):
     `another process is holding this workspace's Dolt store open (a background sync mirror or another lit command still running); retry: %w` (`workspace_lock.go:411`).
- Engine-open interaction stated at `workspace_lock.go:326-333` and `internal/store/doc.go:46-57`: a **read** engine opens lazily at first SQL, attempts the journal lock for **100ms**, and falls back to Dolt's read-only mode; a **write** engine opens eagerly inside `openStoreConnection`, **refuses** the read-only fallback, and retries boundedly (~30s, `engineOpenRetryMaxElapsed`). A live write Store holds the journal lock for its entire lifetime.
- `workspace_lock.go:384-388` records the one lifecycle write this hold does not stop: `journal.idx` is opened `O_RDWR` and truncated on every engine bootstrap with no can-write gate, so a snapshot copy can capture a torn index; Dolt's `corruptIndexRecovery` truncates it to zero and rebuilds from the journal on next open.

---

### 7. Remote cache (`internal/store/remotecache.go`)

#### 7.1 What is cached and where

Dolt gives every **git-backed** remote its own bare-repo mirror at
`<db>/.dolt/git-remote-cache/<sha256(url|ref)>/repo.git`, and never deletes one (`remotecache.go:21-31`).

Constants:
- `remoteCacheDirName = "git-remote-cache"` (`remotecache.go:39`)
- `defaultGitRemoteRef = "refs/dolt/data"` (`remotecache.go:41`) — mirrors dbfactory's `defaultGitRef`; lit never supplies `GitRefParam`.
- `gitBackedURLSchemePrefix = "git+"` (`remotecache.go:49`)

Base path (`remotecache.go:266`):
```go
func (s *Store) remoteCacheBase() string {
    return filepath.Join(s.doltRootDir, doltDatabaseName, dbfactory.DoltDir, remoteCacheDirName)
}
```
The middle segment is `doltDatabaseName`, **not** the workspace id (`remotecache.go:257-265`).

#### 7.2 Key derivation

`remoteCacheKey(remoteURL) (key string, gitBacked bool, err error)` (`remotecache.go:74`):
1. `url.Parse(strings.TrimSpace(remoteURL))`; failure → `parse dolt remote url %q: %w` (`remotecache.go:77`).
2. Lower-case the scheme (`remotecache.go:79`). If it does **not** start with `git+`, return `("", false, nil)` — not an error, just no mirror (`remotecache.go:80-82`).
3. Copy the URL, strip `git+` from the scheme, clear `RawQuery` and `Fragment` (`remotecache.go:85-88`).
4. `sha256.Sum256([]byte(underlying.String() + "|" + defaultGitRemoteRef))`, hex-encoded lowercase (`remotecache.go:89-90`).

Three distinct outcomes by design (`remotecache.go:56-63`): git-backed → key; non-`git+` → no key, prune carries on; unparseable → failure.

`isRemoteCacheKey(name)` (`remotecache.go:99`): length must equal `sha256.Size*2` = **64**, the name must equal its own lower-casing, and it must hex-decode. Anything else was not written by dbfactory and is never deleted.

`expectedRemoteCacheKeys(remotes []storage.SyncRemote)` (`remotecache.go:314`) maps key → remote **name**; non-git-backed remotes are skipped; a parse error aborts.

`listRemoteCacheKeys(base)` (`remotecache.go:273`): `os.ReadDir(base)`; `fs.ErrNotExist` → `(nil, nil)` (a store that never opened a git remote); other errors → `read git remote cache %s: %w`. Only entries that are directories **and** pass `isRemoteCacheKey` are returned.

#### 7.3 The plan

`type remoteCachePlan struct { abandoned []string }` (`remotecache.go:116`).

`planRemoteCachePrune(expected map[string]string, onDisk []string) (remoteCachePlan, error)` (`remotecache.go:146`):
- One rule: a directory is abandoned when no configured remote derives its key (`remotecache.go:153-157`). `abandoned` is sorted (`remotecache.go:158`).
- `unaccounted` is every expected key with no directory on disk, rendered `"<remoteName>→<key>"` and sorted (`remotecache.go:160-166`).
- **Refusal**: if `len(abandoned) > 0 && len(unaccounted) > 0`, returns a zero plan and the error (`remotecache.go:168-182`):

> `declining to prune: %d cache director%s match no configured remote, but %d configured remote%s also %s no directory (%s). That is two possible facts wearing one shape, and this code cannot tell which it is looking at: either the key derivation disagrees with what Dolt actually wrote, or those remotes have simply never been opened — Dolt writes a mirror on first use, never when a remote is configured. While both readings stand an unmatched directory cannot be told apart from a live mirror this code failed to find, so nothing was deleted. One \`lit sync push --remote <name>\` or \`lit sync fetch --remote <name>\` through each remote named above creates its directory and settles it`

with pluralizations `y`/`ies`, ``/`s`, `has`/`have` supplied by `plural(n, one, many)` (`remotecache.go:186`).

The refusal is deliberately **not** narrowed to the remote being pushed (`remotecache.go:134-142`).

A store that has never pushed trips nothing: no directories → nothing to delete (`remotecache.go:144-145`; pinned at `remotecache_plan_test.go:168`).

#### 7.4 Execution

`(*Store).pruneRemoteCache(ctx)` (`remotecache.go:352`) — **runs without the commit lock** (`remotecache.go:331-336`):
1. `s.SyncListRemotes(ctx)` → on error, outcome with `Problem = err.Error()`.
2. `expectedRemoteCacheKeys(remotes)` → same.
3. `s.remoteCacheBase()`, `listRemoteCacheKeys(base)` → same.
4. `planRemoteCachePrune(expected, onDisk)` → same.
5. For each key in `plan.abandoned` (sorted): re-ask `s.remoteCacheKeyIsStillAbandoned(ctx, key)`; skip if it has come back to life; else `collectAbandonedMirror(base, key)`; increment `Removed` and add to `Reclaimed` when collected.
6. **Every entry is attempted**; a failure appends to `problems` and the loop continues, so one permanently unremovable directory is not a head-of-line blocker (`remotecache.go:396-402`). `outcome.Problem = strings.Join(problems, "; ")` (`remotecache.go:403`).

`remoteCacheKeyIsStillAbandoned(ctx, key)` (`remotecache.go:415`) re-lists remotes and re-derives keys; errors wrap as `re-check abandoned mirror %s: %w` (`remotecache.go:418`, `remotecache.go:422`). Pinned at `remotecache_test.go:330`.

`collectAbandonedMirror(base, key) (reclaimed int64, collected bool, err error)` (`remotecache.go:451`):
- `dirSize(dir)` **first** so the reclaim figure is measurable (`remotecache.go:454`).
- `fs.ErrNotExist` → `(0, false, nil)` — already gone is **not** an error (`remotecache.go:456`).
- Other measure failure → `measure abandoned mirror %s: %w` (`remotecache.go:460`).
- `os.RemoveAll(dir)` failure → `remove abandoned mirror %s: %w` (`remotecache.go:463`).
- Success → `(size, true, nil)`.
- Known open window (`remotecache.go:442-450`): a sibling taking the directory between the walk and the unlink means both prunes report the same bytes; the reclaim figure is knowingly approximate across concurrent prunes.

`dirSize(root)` (`remotecache.go:292`) walks with `filepath.WalkDir` and sums `info.Size()` of non-directory entries.

#### 7.5 Outcome and reporting

```go
type remoteCachePruneOutcome struct {
    Removed   int
    Reclaimed int64
    Problem   string
}
```
(`remotecache.go:198-205`)

`Report()` (`remotecache.go:220`) — four exact branches:
- `Problem != "" && Removed > 0`: `remote-cache prune: removed %d abandoned mirror%s (%s), then failed: %s`
- `Problem != ""`: `"remote-cache prune: " + o.Problem`
- `Removed > 0`: `remote-cache prune: removed %d abandoned mirror%s, reclaimed %s`
- default: `""` (empty exactly when the prune looked and found nothing to do)

`humanBytes(n int64)` (`remotecache.go:242`): below `1024` renders `%d B`; otherwise divides by 1024 repeatedly and renders `%.1f %ciB` with the unit letter drawn from `"KMGTPE"` — i.e. `KiB`, `MiB`, `GiB`, `TiB`, `PiB`, `EiB`. Shared with compaction's `footprintDelta` so both maintenance reporters spell sizes identically.

#### 7.6 Tests pinning remote-cache behavior

- `remotecache_test.go:22` `TestRemoteCacheKeyMatchesDoltLayout` — pins the derivation against a cache directory Dolt itself created.
- `remotecache_test.go:96` keeps the live mirror.
- `remotecache_test.go:147` collects abandoned mirrors.
- `remotecache_test.go:222` is not blocked by one stuck mirror.
- `remotecache_test.go:330` re-check follows the remotes, not a snapshot.
- `remotecache_plan_test.go:22` collects only unmatched dirs.
- `remotecache_plan_test.go:43` declines when the derivation misses the live mirror.
- `remotecache_plan_test.go:68` the refusal names up to both causes.
- `remotecache_plan_test.go:95` already-gone directory is treated as done.
- `remotecache_plan_test.go:111` reclaims what it removes.
- `remotecache_plan_test.go:137` a failure names the key.
- `remotecache_plan_test.go:182` collects when no remote is configured.
- `remotecache_plan_test.go:197` `TestRemoteCacheKeyPreservesHomeRelativePath` — a home-relative scp URL normalizes to `ssh://git@host/./path` and the `/./` must be preserved (`remotecache.go:70-73`).
- `remotecache_plan_test.go:215` separates non-git remotes from bad URLs.
- `remotecache_plan_test.go:233` `isRemoteCacheKey` rejects foreign names.
- `remotecache_plan_test.go:259`, `:272` `Report()` semantics.

---

### 8. The vendored Dolt driver (`internal/vendor/dolthub-driver`)

#### 8.1 What it is

A vendored copy of `github.com/dolthub/driver` — a `database/sql` driver for an **embedded** Dolt engine (no server process). Package name `embedded` (`driver.go:15`). Module path is still `github.com/dolthub/driver` (`go.mod:1`). Registered under the driver name `"dolt"` in `init()` (`driver.go:34`, `driver.go:51-53`).

`go.mod` records the vendored-from baselines (`go.mod:8-14`) and mirrors the top-level fork replaces so a standalone build also uses the promptctl forks (`go.mod` replace lines): `github.com/dolthub/dolt/go => github.com/promptctl/dolt/go v0.40.5-0.20260821231005-4b80eac34485` and `github.com/dolthub/go-mysql-server => github.com/promptctl/go-mysql-server v0.20.1-0.20260821032251-ab5cb9ec3b69`, plus `github.com/google/flatbuffers => github.com/dolthub/flatbuffers v1.13.0-dh.1`.

#### 8.2 DSN grammar

`ParseDataSource(dataSource)` (`data_source.go:36`):
- Must start with the literal `file://` (`data_source.go:24`, `data_source.go:37`); otherwise `datasource url '%s' must have a file url scheme` (`data_source.go:38`).
- Everything after `file://` up to the first `?` is the **directory**; the rest is parsed with `url.ParseQuery` (`data_source.go:41-55`).
- Param **names are lower-cased**; values are not (`data_source.go:58-61`).
- `ParamIsTrue(name)` (`data_source.go:69`) is true only when the param exists, has exactly one value, and that value lower-cases to `"true"`.

`ParseDSN(dsn) (Config, error)` (`parse_dsn.go:27`):
1. `ParseDataSource`.
2. Directory must exist and be a directory: `'%s' does not exist` (`parse_dsn.go:36`) or `%s: is a file. need to specify a directory` (`parse_dsn.go:38`).
3. `commitname` required: `datasource %q must include the parameter %q` (`parse_dsn.go:43`); exactly one value: `param %q must have exactly one value` (`parse_dsn.go:46`).
4. `commitemail` required, same two messages (`parse_dsn.go:51`, `parse_dsn.go:54`).
5. `database` optional, but if present must have exactly one value (`parse_dsn.go:60`).
6. `multistatements` and `clientfoundrows` via `ParamIsTrue` (`parse_dsn.go:71-72`).
7. The full lower-cased param map is preserved in `Config.Params` (`parse_dsn.go:73`).

Recognized param names (`driver.go:36-45`):
`commitname`, `commitemail`, `database`, `multistatements`, `clientfoundrows`, plus two presence-based flags passed through to Dolt's DB loading layer: `disable_singleton_cache`, `fail_on_journal_lock_timeout`.

Example DSN from the doc comment (`driver.go:125`):
`file:///User/brian/driver/example/path?commitname=Billy%20Bob&commitemail=bb@gmail.com&database=dbname`

Tests: `parse_dsn_test.go:27` basics, `:48` param names are case-insensitive, `:61` requires commitname and commitemail, `:71` validates directory exists and is a dir; `data_source_test.go:23`.

#### 8.3 `Config`

`config.go:30-85`. Fields: `DSN`, `Directory` (required), `CommitName`/`CommitEmail` (required — used as Dolt commit metadata), `Database`, `MultiStatements`, `ClientFoundRows`, `Params`, `BackOff backoff.BackOff`, `DisableSingletonCache`, `FailOnJournalLockTimeout`, `Version`.

`BackOff` semantics (`config.go:56-65`): nil → engine open attempted **once**; non-nil → retries on retryable errors, and **implies both** `DisableSingletonCache` and `FailOnJournalLockTimeout`. Implementations are stateful; the connector calls `Reset()` before use.

#### 8.4 Connector

`NewConnector(cfg)` (`connector.go:69`) validates: `config.Directory is required` (`connector.go:71`), `config.CommitName is required` (`connector.go:74`), `config.CommitEmail is required` (`connector.go:77`); defaults `cfg.Version` to `defaultDoltVersion = "0.40.17"` (`connector.go:35`, `connector.go:80`); re-validates the directory with the same two messages (`connector.go:86`, `connector.go:88`).

`Connect(ctx)` (`connector.go:103`): `getOrOpenEngine` → `newLocalContext` → `SetCurrentDatabase(cfg.Database)` when non-empty (`connector.go:115`) → if `ClientFoundRows`, OR `mysql.CapabilityClientFoundRows` into the session client capabilities (`connector.go:118-125`) → returns a `*DoltConn`.

`getOrOpenEngine` (`connector.go:161`): a single shared engine per connector, guarded by `c.mu` plus an `openCh` channel so concurrent Connects wait on the in-flight open rather than racing. `connector is closed` (`connector.go:166`) if `Close` already ran; a waiter aborts on `ctx.Done()` returning `ctx.Err()` (`connector.go:181`). If the open succeeds after `Close`, the engine is immediately closed (`connector.go:198`).

`Close()` (`connector.go:137`) sets `closed`, nils the engine and channel, does **not** block on an in-flight open, and closes the engine if one exists.

`openEngineWithRetry` (`connector.go:211`):
- Dolt user config is a map with `config.UserNameKey → cfg.CommitName` and `config.UserEmailKey → cfg.CommitEmail` (`connector.go:213-216`) — **this is the commit author identity the embedded engine stamps**.
- `engine.SqlEngineConfig{IsReadOnly: false, ServerUser: "root", Autocommit: true}` (`connector.go:218-222`).
- `disableCache := cfg.BackOff != nil || cfg.DisableSingletonCache`; `failOnLockTimeout := cfg.BackOff != nil || cfg.FailOnJournalLockTimeout` (`connector.go:228-229`); each sets the corresponding `dbfactory` key in `seCfg.DBLoadParams` as a presence flag `struct{}{}` (`connector.go:234-239`).
- `fs.WithWorkingDir(cfg.Directory)` (`connector.go:243`).
- If `BackOff == nil`, one call to `open(ctx)` (`connector.go:252`).
- Else `BackOff.Reset()`, wrap with `backoff.WithContext(bo, ctx)`, and `backoff.Retry`: a retryable error is returned for retry, anything else is wrapped `backoff.Permanent` (`connector.go:257-280`). On failure the **last underlying error** is returned in preference to backoff's own (`connector.go:276-279`).

`isRetryableOpenErr(err)` (`retryable_open_err.go:24`) — exactly two shapes: `errors.Is(err, nbs.ErrDatabaseLocked)` (`retryable_open_err.go:29`) and `errors.Is(err, os.ErrDeadlineExceeded)` (`retryable_open_err.go:33`). Everything else is permanent.

`openSqlEngine` (`driver.go:74`):
- Builds a **carrier** `env.DoltEnv{Version: version, DBLoadParams: maps.Clone(seCfg.DBLoadParams)}` when params exist (`driver.go:87-90`), because `NewSqlEngine`'s own threading of `DBLoadParams` happens after `MultiEnvForDirectory` has already loaded the databases — too late for params that shape the storage open itself (`driver.go:80-86`).
- `loadMultiEnvFromDirWithParams(ctx, cfg, fs, ".", version, carrier)` — passes `"."` because `fs` is already rooted at `dir` (`driver.go:78`, `driver.go:91`).
- **Forces each env's lazy database load** and surfaces its failure as *the* open error (`driver.go:96-115`): iterates `mrEnv`, and if `dEnv.DoltDB(ctx) == nil`, takes `dEnv.DBLoadError` or synthesizes `database %q failed to load`. Without this, `CollectDBs` inside `NewSqlEngine` would panic on a nil DB instead of the retryable `nbs.ErrDatabaseLocked` reaching the backoff.
- `engineConstructMu.Lock()` around `engine.NewSqlEngine` (`driver.go:117-119`).

`(*doltDriver).Open(dsn)` (`driver.go:129`) always returns `dolt SQL driver does not support Open()`; only `OpenConnector` (`driver.go:133`) works.

#### 8.5 Local modifications versus upstream (behavior-affecting)

1. **Telemetry removed outright** — `connector.go:284-294`: upstream fired an unconditional goroutine (`emitUsageEvent`) that dialed `eventsapi.dolthub.com` over gRPC on every engine open, gated only by an env var read at package init. The emission path, its env-gated opt-out, its once-per-24h rate-limit file, and every import that served it are **deleted**, not defaulted off.

2. **Process-wide engine-construction mutex** ("lit patch 5") — `driver.go:63-72`: `var engineConstructMu sync.Mutex` serializes `engine.NewSqlEngine` because go-mysql-server's `InitStatusVariables` rewrites the global status-variable table and `NewSqlEngine` re-points the global binlog-consumer singleton. Two concurrent constructions race on those globals **even for unrelated database paths**. Queries against already-constructed engines are unaffected.

3. **`MySQLError` replaces `github.com/go-sql-driver/mysql`'s** ("Patch 4") — `mysql_error.go` is original promptctl work, MIT (`mysql_error.go:1-8`), removing an MPL-2.0 SBOM coordinate. Two fields only: `Number uint16` (the protocol's own width) and `Message string`; **no SQL state field** (`mysql_error.go:28-38`). `Error()` renders `"Error " + strconv.FormatUint(uint64(Number),10) + ": " + Message` (`mysql_error.go:47`), MySQL's conventional form. `translateError` (`errors.go:30`) is its only producer: `sql.CastSQLError(err)` → `&MySQLError{Number: uint16(vitessErr.Num), Message: vitessErr.Message}`; `nil` in, `nil` out. Tests: `errors_test.go:25`, `errors_test.go:55`.

4. **Retryable-open plumbing** — `retryable_open_err.go`, the `BackOff`/`DisableSingletonCache`/`FailOnJournalLockTimeout` `Config` knobs (`config.go:56-80`), and the `DBLoadParams` mapping (`connector.go:228-239`). Pinned by `config_load_params_test.go:33` (`TestConfigDBLoadParamMapping`), a table with exactly four cases: `{"neither by default", …, false, false}`, `{"backoff implies both", …, true, true}`, `{"cache disable alone", …, true, false}`, `{"fail-fast alone", …, false, true}` (`config_load_params_test.go:38-45`). `openconnector_retry_test.go:34` pins that with `backoff.WithMaxRetries(backoff.NewConstantBackOff(0), 10)` an open failing 3× with `nbs.ErrDatabaseLocked` eventually succeeds and calls ≥ 4; `:68` pins that with no BackOff the open is attempted **exactly once** and the error satisfies `errors.Is(err, nbs.ErrDatabaseLocked)`; `:96` pins that a 150ms `Connect` context bounds the retry (elapsed < 2s).

5. **Forced eager DB load in `openSqlEngine`** — `driver.go:96-115` (see §8.4); without it the retryable lock error would surface as a nil-pointer panic.

6. **Relative-path fix** — `openSqlEngine` passes `"."` rather than `dir` because the connector already rooted the filesystem at `cfg.Directory` (`driver.go:78`, `connector.go:243`). Pinned by `relative_path_test.go:35` and `relative_path_test.go:64`, both of which assert that re-applying the directory (`LoadMultiEnvFromDir(..., "data/myapp", ...)` / `..., cfg.Directory, ...`) **fails** because the doubled path does not exist.

7. **Peek error must not be dropped** — `statement.go:200` `peekResultError(peekErr)`: `nil` and `io.EOF` yield a nil `doltRows.err`; anything else is translated and carried, to be surfaced from `Next()` rather than re-driving the iterator (which could return a different outcome and silently convert a real error into an empty result set). `rows.go:151-158` returns that carried error from `Next`. Pinned by `peek_error_test.go:56`, `:76`, `:103`.

8. **Test seams left as package vars** (production leaves them nil): `newLocalContextForConnector` (`connector.go:39`) and `openSqlEngineForConnector` (`driver.go:61`).

9. **`newResult` error precedence** — `result.go:55-64`: the iteration error wins over a `Close` failure; a close failure is only reported when iteration succeeded.

#### 8.6 Query, statement, and rows behavior

`DoltConn.Prepare(query)` (`conn.go:41`): updates `gmsCtx.SetQueryTime(time.Now())` (safe because statements execute serially on a connection, `conn.go:42-44`), then picks multi- vs single-statement from `cfg.MultiStatements`, falling back to `DataSource.ParamIsTrue(MultiStatementsParam)` when `cfg` is nil (`conn.go:47-52`).

`prepareMultiStatement` (`conn.go:71`) splits with `gms.NewMysqlParser()`'s `Parse(ctx, remainder, true)` loop, **skipping** `sqlparser.ErrEmpty` statements (`conn.go:79-81`), and wraps every error with `translateError`.

`DoltConn.Close()` (`conn.go:97`) returns `nil` — it releases nothing; the engine belongs to the connector.

`DoltConn.Begin()` (`conn.go:104`) delegates to `BeginTx` with `LevelSerializable`, `ReadOnly: false`. `BeginTx` (`conn.go:113`) accepts **only** `LevelSerializable` or `LevelDefault`; anything else returns `isolation level not supported '%d'` (`conn.go:115`). It then runs the literal SQL `BEGIN;` (`conn.go:118`). Pinned by `smoke_test.go:690`.

`doltTx.Commit()` runs `COMMIT;` (`transaction.go:32`); `Rollback()` runs `ROLLBACK;` (`transaction.go:38`); both translate errors.

`doltStmt` (`statement.go:95`): `Close()` returns nil (`statement.go:104`); `NumInput()` returns `-1` (`statement.go:109`).

`argsToBindings(args)` (`statement.go:113`): positional args become named bindings `v1`, `v2`, … (`statement.go:116`) via `sqltypes.BuildBindVariable` → `BindVariableToValue` → `sqlparser.ExprFromValue`.

`doltStmt.Exec` (`statement.go:135`) runs `QueryWithBindings` and drains the iterator through `newResult`. `newResult` (`result.go:33`) sums `types.OkResult.RowsAffected` into `affected` and takes the **last** `InsertID` into `last` (`result.go:47-52`). `LastInsertId`/`RowsAffected` return the stored error if any (`result.go:73`, `result.go:82`).

`doltStmt.Query` (`statement.go:163`): with args it goes through `execWithArgs`; with none through `se.Query`. It then wraps the iterator in a `peekableRowIter` and calls `Peek` **eagerly** — required because inserts and some DML (e.g. `CREATE PROCEDURE`) execute inside the iterator, so a later statement in a multi-statement query would otherwise see un-applied results (`statement.go:177-181`).

`isQueryResultSet(row)` (`statement.go:210`): `nil` row → `true` (a valid empty result set); a one-column row holding a `types.OkResult` → `false`; a zero-column row → `false`; otherwise `true`.

`doltRows.Next` (`rows.go:151`) type conversions, in order (`rows.go:172-200`): `driver.Valuer` → `v.Value()` (error → `error processing column %d: %w`); `types.GeometryValue` → `Serialize()`; schema column of `gms.EnumType` → `Convert` then `At(int(v.(uint16)))`, with errors `could not convert to expected enum type for column %d: %w` and `not a valid enum index for column %d: %v`; schema column of `gms.SetType` → `Convert` then `BitsToString(v.(uint64))`, errors `could not convert to expected set type for column %d: %w` and `could not convert value to set string for column %d: %w`; otherwise the raw value. A column-count mismatch returns `mismatch between expected column count and actual column count` (`rows.go:169`).

`doltMultiRows` (`rows.go:32`) implements `driver.RowsNextResultSet`: `HasNextResultSet()` is `(currentIdx+1) < len(rowSets)` (`rows.go:75`); `NextResultSet()` closes the current set and advances past non-result-set statements, returning `io.EOF` when exhausted (`rows.go:88-105`).

`doltMultiStmt.Exec` (`statement.go:53`) stops at the first error and otherwise returns the **last** result, matching the MySQL driver (`statement.go:62`). `doltMultiStmt.Query` (`statement.go:66`) builds lazy producers and advances to the first statement that actually yields a result set (`statement.go:77-90`).

#### 8.7 Standalone query splitter

`query_splitter.go` provides `QuerySplitter` (`query_splitter.go:68`) with `Next()` (`query_splitter.go:80`, returns `io.EOF` when exhausted, trims whitespace) and `HasMore()` (`query_splitter.go:96`). `parseNext` (`query_splitter.go:100`) splits on `;` while tracking a `RuneStack` of open delimiters `(`, `"`, `'`, `` ` `` (`query_splitter.go:22-27`): inside a quote, the matching close pops **unless** preceded by a literal backslash (`query_splitter.go:114`); inside `(`, a `)` pops and any open rune pushes (`query_splitter.go:117-122`). Unterminated input returns the whole remaining length (`query_splitter.go:128`). Pinned by `query_splitter_test.go:24`. Note: `DoltConn.prepareMultiStatement` uses the gms parser (`conn.go:73`), not this splitter.

#### 8.8 Suppression of Dolt's human output (lit-side)

`internal/store/dolt_output.go:31-33` — a package `init()` sets `doltcli.CliOut = io.Discard`. Rationale stated at `dolt_output.go:9-30`: the embedded engine's "N of M chunks complete" redraw (with cursor-control escapes) that `DOLT_CLONE` and `DOLT_FETCH` emit during `init adopt` and `sync pull/fetch` defaults to `os.Stdout`, which is lit's parseable result channel. It is **suppressed**, not relocated to stderr, because lit already owns a single progress voice (`progressf` in `internal/cli/progress.go`). Dolt's error channel `cli.CliErr` is **left untouched** (`dolt_output.go:30`). Tests: `internal/store/dolt_output_test.go`.

---

### 9. The storage contract assertions (`internal/store/contract.go`)

`contract.go:38-48` — compile-time assertions that make the contract a constraint on the engine:

```go
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
```

Dolt offers all seven capabilities; `storage.Offered` reads this set back at runtime (`contract.go:13-18`). The package exports **no** storage vocabulary aliases — this engine's files spell those types `storage.X`, so the engine is reached through the contract or not at all (`contract.go:20-26`). What remains exported beyond the `Store` methods is Dolt-era workspace machinery addressed by **filesystem path** rather than engine handle: the workspace and commit flocks, the mirror beacons, bootstrap and remote adoption, snapshot naming, and lifeboat recovery (`contract.go:28-32`).


---

