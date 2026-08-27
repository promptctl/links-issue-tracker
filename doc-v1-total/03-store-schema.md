# Dolt store: engine, schema, and core records

lit's shared backend stores every workspace in an embedded [Dolt](https://github.com/dolthub/dolt) database — a MySQL-compatible SQL engine with git-like versioning — living under a per-repo directory, database name `links` (`internal/store/store.go:28`). Every mutation lands as one SQL transaction followed by one Dolt commit, so the store carries its own full history independent of the issue-event table. This chapter covers the engine lifecycle, commit and locking semantics, the complete SQL schema and its reconciliation machinery, and the core record operations: create, read, update, events, comments, labels, relations, and ranking, plus the shapemap recovery mapping. Import/export, verify/recover, checkpoints, and the adopt/candidate/promote/downgrade workspace flows are in `04-store-operations.md`; sync, compaction, and migration-runner mechanics are in `05-sync-merge-compaction.md`.

## Engine lifecycle

### Open modes

A `Store` wraps exactly one pooled SQL connection (`SetMaxOpenConns(1)`, `store.go:2672-2683`) opened through a vendored embedded-Dolt driver. Two access modes (`store.go:37-42`):

- **Write** (`Open`, and the sync-side `OpenSync`): the connector gets an exponential backoff (initial 50ms, max interval 1s, max elapsed ~30s, `store.go:2629-2635`) and pings eagerly so lock contention surfaces at open time.
- **Read** (`OpenForRead`): no backoff, no ping — the engine opens lazily at the first SQL statement (`store.go:381-399`). A read open beside a foreign lock holder succeeds via Dolt's read-only fallback (journal wait ~100ms) and serves reads.

If another process holds Dolt's journal lock (`<root>/links/.dolt/noms/LOCK`), the wrapped error satisfies both `ErrWorkspaceBusy` and `nbs.ErrDatabaseLocked` and reads "another process is holding this workspace's Dolt store open … retry after it completes" (`store.go:2619-2624`).

### Open sequence

`Open(ctx, doltRootDir, workspaceID)` (`store.go:98-165`), in order: validate args (both non-blank; the root is `filepath.Clean`ed) → acquire the **shared workspace flock** → refuse if an adopt is pending → bootstrap the database if absent (`CREATE DATABASE IF NOT EXISTS links` through a first pool that closes before the second opens, `store.go:2497-2544`) → open the write connection → under the **commit lock**: normalize the default branch to `master` (renaming a sole non-master branch via `DOLT_BRANCH('-m', …)`, `store.go:2553-2596`) and run migrations. Failure at any point releases everything and returns the error.

`OpenForRead` differs in that it stats the directory first — a missing directory yields "repository not initialized with lit — run 'lit init' first" — never bootstraps, and still runs migrations (a pending migration under a read-only holder fails with a "pending schema migrations" message, `store.go:212-231`). Re-opening a current-schema workspace adds **no** Dolt commit; migration is idempotent across opens.

`EnsureDatabase` performs the same validate → lock → adopt-check → bootstrap sequence standalone, returning whether it created the root (`store.go:276-296`). `Close` closes the DB (releasing the journal lock) before releasing the workspace lock (`store.go:339-360`). `reconnect` — used by retry loops — opens a new pool, swaps it in, closes the old one, and pings (`store.go:425-440`); it is the one place the journal lock is taken while the commit lock is held.

Two `Open`s on one root serialize: the second blocks until the first `Close`. After a SIGKILL mid-commit or mid-migration, a fresh open succeeds — no stale-lock state survives process death, because all locks are kernel flocks.

### Attribution

`AttributeTo(streamToken)` sets a `(stream, workspace-id)` attribution pair that is stamped on **every** subsequent event row (`store.go:260-262`). A blank token yields the absent pair; unattributed events store SQL NULL in both columns. Restoring an export preserves the producer's attribution rather than re-stamping the restorer's.

## Commit semantics and locking

### One mutation, one commit

Every mutation routes through `withMutation(ctx, message, fn)` (`commit_lock.go:122-177`): under the commit lock and a transient-retry loop, `BeginTx` → `fn` → `tx.Commit` → `DOLT_COMMIT('-Am', <message>)`. The `-A` stages everything; there is no separate add step and no `--skip-empty` — a "nothing to commit" error is absorbed as success (`commit_lock.go:286-320`). A retry after a successful `tx.Commit` resumes at the DOLT_COMMIT step without re-running `fn` (the `staged` flag, `commit_lock.go:144-155`). A commit stamp may also carry `--allow-empty`, `--date` (RFC3339 UTC), and `--author`; ordinary store mutations pass only the message. A combined transition+field update is exactly one Dolt commit.

**Dolt commit identity** derives entirely from the workspace id: author name = workspace id with `@`→`_` (blank → `links`), email = `<name>@links.local` (`store.go:2647-2668`).

Store-level commit messages used verbatim: `record sync state`, `create issue`, `apply update`, `add comment`, `delete comment` (`store.go:457,509,1115,1153,1172`), plus `add label`, `remove label`, `replace labels`, `add relation`, `remove relation`, `set parent`, `clear parent`, `rank to top`, `rank set`, `rank to bottom`, `rank above`, `rank below`, `fix rank inversions` from their subsystems (`labels.go`, `relations.go`, `ranking.go`).

### The two locks

| Lock | Path | Mode | Budget | Notes |
|---|---|---|---|---|
| Workspace lock | `.links-workspace.lock` beside the store | shared on open (exclusive users covered in ch. 04) | — | held for the store's lifetime |
| Commit lock | `.links-commit-flock.lock` in the parent of the dolt root (`commit_lock.go:394-403`) | exclusive | 9000 attempts × 100ms ≈ 15 min (`commit_lock.go:76-77`) | re-entrant via a context key (`commit_lock.go:357-366`) |

Both are zero-byte kernel flocks with **no** stale/PID/mtime heuristics — process death is the only release. Contention on the commit lock wraps as "another lit process is writing to this workspace … retry after it completes". A panic inside a mutation still releases the lock; a release failure after a successful operation prints a warning to stderr and returns success (`commit_lock.go:346-355`). A cancelled context returns `context.Canceled` rather than burning the budget.

### Transient-retry classification

`retryTransientGCContention` retries up to 30 attempts (a variable, test-shrinkable) with exponential delay 50ms→1s, rotating the connection (`reconnect`) between attempts (`commit_lock.go:185-204, 250-259`). Retryable errors are exactly two lowercased-substring matches: "cannot update manifest"+"read only" (manifest read-only) and "online garbage collection"+"reconnect" (GC reset) (`commit_lock.go:479-501`). An exhausted manifest-read-only run promotes to `WorkspaceWriteBlockedError` ("another lit process is holding this workspace open for writing; the store stayed read-only across every retry…"); an exhausted GC-reset does not promote. Non-transient errors are never retried.

## The SQL schema

### Producers and pinning

Exactly four producers of live DDL exist: the goose baseline migration `00001_baseline.sql` (v1), goose migrations v2–v5 (`add_lane`, `add_resolution`, `add_redirect_target`, `add_event_attribution`), the pre-goose reconciler (`schema_reconcile.go`), and the quarantine-table bootstrap (`migration_runner.go:657-696`) — plus goose's own `goose_db_version` bookkeeping table. The baseline file is **byte-frozen** by a SHA-256 canary test (`e86c1aa3…ad1fbb`); no schema hash is stored in the database — the recorded version is the integer in `goose_db_version` plus a `producer_binary_version` string in `meta`. The converged schema is pinned as a byte-compared `SHOW CREATE TABLE` golden file (`schema_snapshot.sql`), regenerated only via a test flag; the dump enumerates live tables, so a leftover table is drift.

All application tables are InnoDB, `utf8mb4_0900_bin`, no `AUTO_INCREMENT` anywhere except `goose_db_version.id`.

### Tables

Nine tables. `issues` (`schema_snapshot.sql:51-78`):

| Column | Type | Null | Default | Since |
|---|---|---|---|---|
| `id` | VARCHAR(191) | PK | — | v1 |
| `title`, `description` | TEXT | NOT NULL | — | v1 |
| `agent_prompt` | TEXT | NULL | — | v1 |
| `status` | VARCHAR(32) | NULL | — | v1 |
| `priority` | INT | NOT NULL | — | v1 |
| `issue_type` | VARCHAR(32) | NOT NULL | — | v1 |
| `topic` | VARCHAR(191) | NOT NULL | — | v1 |
| `assignee` | TEXT | NOT NULL | — | v1 |
| `created_at`, `updated_at` | VARCHAR(64) | NOT NULL | — | v1 |
| `closed_at`, `archived_at`, `deleted_at` | VARCHAR(64) | NULL | — | v1 |
| `item_rank` | TEXT | NOT NULL | `''` | v1 |
| `lane` | TEXT | NOT NULL | `''` | v2 |
| `resolution` | VARCHAR(32) | NULL | — | v3 |
| `redirect_target` | VARCHAR(191) | NULL | — | v4 |

Timestamps are **strings** (RFC3339Nano), parsed at one boundary (`scanTime`, `store.go:2215-2217`). Indexes: `idx_issues_rank (item_rank(191))`, `idx_issues_status_priority (status, priority, updated_at)`. Five named CHECK constraints: status is NULL exactly for epics and otherwise in (`open`,`in_progress`,`closed`); priority in [0,1]; type in the five-type set; resolution NULL or in the four-resolution set; `redirect_target` non-NULL only when resolution is `duplicate` or `superseded`. `redirect_target` deliberately has **no** foreign key.

The other tables:

| Table | Columns | Keys / constraints |
|---|---|---|
| `relations` | `src_id`, `dst_id` VARCHAR(191); `type` VARCHAR(32); `created_at` VARCHAR(64); `created_by` TEXT | PK `(src_id,dst_id,type)`; FKs both endpoints → `issues(id)` ON DELETE CASCADE; CHECK type in (`blocks`,`parent-child`,`related-to`); indexes `(src_id,type)`, `(dst_id,type)` |
| `comments` | `id`, `issue_id` VARCHAR(191); `body` TEXT; `created_at`; `created_by` | PK `(id)`; FK → issues CASCADE; index `(issue_id,created_at)` |
| `labels` | `issue_id`, `label` VARCHAR(191); `created_at`; `created_by` | PK `(issue_id,label)`; FK → issues CASCADE; indexes `(issue_id,label)`, `(label,issue_id)` |
| `issue_events` | `id`, `issue_id` VARCHAR(191); `action` VARCHAR(64) NULL; `reason`, `actor` TEXT NOT NULL; `created_at`; `stream_id` VARCHAR(64) NULL (v5); `workspace_id` VARCHAR(191) NULL (v5) | PK `(id)`; FK → issues CASCADE; index `(issue_id,created_at)` |
| `issue_event_changes` | `event_id` VARCHAR(191); `field` VARCHAR(64); `from_value`, `to_value` TEXT NULL | PK `(event_id,field)`; FK → issue_events CASCADE |
| `meta` | `meta_key` VARCHAR(191) PK; `meta_value` TEXT | key-value; upsert via `ON DUPLICATE KEY UPDATE` |
| `migration_quarantine` | `version` BIGINT PK; `name`, `error_text` TEXT; `created_at` | created outside the goose batch so a rollback cannot erase it |
| `goose_db_version` | goose's own (id AUTO_INCREMENT, version_id, is_applied, tstamp) | migration bookkeeping |

Because every issue-row deletion in normal operation is a soft `deleted_at` stamp, the ON DELETE CASCADEs never fire from CRUD paths.

`meta` keys used in production: `workspace_id`, `producer_binary_version`, `last_sync_path`, `last_sync_hash`. `GetSyncState`/`RecordSyncState` read/write the last two (`store.go:442-468`).

A legacy `issue_history` table (columns `id, issue_id, action, reason, from_status, to_status, created_at, created_by`) no longer exists in any current schema; its presence on disk is the marker of a pre-goose workspace.

### Pre-goose reconciliation

When a workspace predates goose management (marker: `issue_history` exists, or canonical tables exist with no goose log — `migration_runner.go:861-907`), `reconcileToBaseline` (`schema_reconcile.go:164-416`) forward-migrates it to the v1 baseline before goose applies v2–v5. Steps, in order: create any missing v1 tables/indexes (FK-safe order, `meta` first); drop a fabricated `goose_db_version`; rename `issue_events.assignee`→`actor`; translate `issue_history` rows into events (one event per row, plus one `status` field-change when the normalized from/to actually differ; orphaned rows skipped; idempotent against existing event ids); drop `issue_history`; add `item_rank` + its index; add `topic` (backfilled `'misc'`, then its temporary default dropped); rename `prompt`→`agent_prompt`, add/relax it as needed; normalize statuses (`in-progress`→`in_progress`, `todo`→`open`, `done`→`closed`, anything invalid→`open`; `closed_at` implies closed and vice versa; epic status forced NULL); backfill missing topics and ranks (unranked rows get sequential ranks appended after the current max, ordered by status/priority/updated_at/id); reset out-of-range priorities to 0 and install the canonical priority CHECK; canonicalize the status CHECK; record `workspace_id` in meta.

Each step is gated by an `information_schema` probe (table/index/column existence, nullability, default presence, CHECK-clause shape, or a row-level predicate); CREATE steps swallow "already exists"-class races, mutation steps don't. Reconcile compares **presence only** — never column types, index shapes, FKs, or collations. A pre-gate refuses shapes missing `status`/`priority`/`updated_at`/`issue_type`/`closed_at`/`description` ("not a known historical shape"); a post-gate re-verifies every baseline column and aborts before stamping v1 if gaps remain. Down-migration effects (v5→v1) drop the added columns in reverse; v4's Down re-materializes one `related-to` edge per redirect target, and v4's Up did the inverse backfill-and-delete.

## Creating issues

`CreateIssue` (`store.go:470-569`) validates before the transaction: title required (trimmed non-empty); type defaults to `task`; labels canonicalized; topic normalized (required). Inside one `create issue` mutation: the parent (if given) must exist; the workspace prefix is normalized; the ID is minted; the rank is placed; the row is inserted (`closed_at` literal NULL; `resolution`/`redirect_target` not in the insert list); a `parent-child` edge is written for a parent; labels are replaced; a `created` event is recorded (reason `issue created`, actor `links` — a hardcoded literal that also signs the parent edge and initial labels); ranks are smoothed if needed. The returned issue is the in-memory value, not a re-read. Leaves store `status='open'`; containers store NULL (enforced by the CHECK).

### ID minting

Top-level IDs are `<prefix>-<topic>-<hash>` (grammar and slug rules in `01-data-model.md`). The store side (`issue_ids.go`): adaptive hash length = smallest 3–8 keeping birthday-collision probability ≤ 0.25 for the current top-level count ("top-level" = id containing no dot; the count includes archived/deleted rows); on a count error the length defaults to 6. For each length from base to 8, up to 10 nonces are tried; the first candidate with zero existing rows (including soft-deleted) wins; exhaustion errors out.

**Child IDs** are sequential dotted suffixes: `<parentID>.N`, N = max existing direct-child number + 1, starting at `.1`; non-numeric and nested suffixes are ignored when computing the max; deleted children keep their numbers reserved (`issue_ids.go:44-73`). Grandchildren nest (`parent.1.2`).

There is no ID parser/validator on lookup — supplied IDs bind verbatim, with no case normalization.

### Rank placement at create

Default placement is bottom (the `RankPlacement` zero value): rank = `After(max live rank)`, or the initial rank `"V"` in an empty workspace; `RankTop` mirrors with `Before(min)` (`store.go:2045-2082`). Consecutive default creates therefore keep authoring order.

## Reads

All issue reads share one 18-column projection (`store.go:2091-2117`) and one hydration path. `hydrateIssues` uses a **fixed query count per recursion level**, not per issue: one labels query for all ids, one children query for all container ids (`store.go:2245-2304`). The children query's visibility rule: a live parent sees only live children; an archived/deleted parent sees all its children — so an active epic's progress excludes archived children, but the same epic once archived counts them (`store.go:2317-2323`).

- `GetIssue`: single-row lookup; missing → `storage.NotFoundError`.
- `getIssuesByIDs`: one `IN` query; missing ids are silently absent from the map.
- `GetIssueDetail` (`store.go:774-847`): the issue + its relations (all incident, ordered by `created_at`), comments, events, then one batch hydrate of every relation counterparty plus the redirect target; buckets into Parent/Children/DependsOn/Blocks; siblings = the parent's other children in rank order (empty for parentless issues); `Related` carries only manual `related-to` edges; the redirect target hydrates independently of the graph and is absent if the target row vanished.
- `ListTopics`: distinct non-empty topics of non-deleted issues, ascending.

### ListIssues filtering

`ListIssues` (`store.go:571-703`) builds SQL WHERE clauses in a fixed order: exclude archived unless `IncludeArchived`; exclude deleted unless `IncludeDeleted`; `issue_type IN` / `NOT IN`; `assignee IN` (blank entries skipped); `updated_at >= / <=`; comment existence / non-existence; one `EXISTS` clause per label in `LabelsAll` (AND semantics); `id IN`; and per search term a `%term%` LIKE over lowercased title, description, prompt, and topic. **Status and resolution are not filtered in SQL** — they filter post-hydration against the *derived* state, so epic states come from child rollup, never the dead NULL column. Resolution filtering drops issues with no resolution. `Limit ≤ 0` means uncapped.

Sorting: default `item_rank ASC, id ASC`. Allowed sort fields (case-insensitive): `id`, `title`, `status`, `priority`, `rank`, `type`, `topic`, `assignee`, `created_at`, `updated_at`; unknown fields error; `id ASC` is always the final tiebreaker. Note `status` sorts the **stored** encoding, so containers (NULL) lead ascending.

### Event reads

One LEFT JOIN query collapses `issue_events` × `issue_event_changes` into events with ordered change lists, sorted `(created_at, id, field)` so re-reads compare identical (`store.go:1942-2012`). `ListAllEvents` applies **no recency cutoff** — claim derivation needs arbitrarily old establishing events — and reads write nothing (Dolt HEAD unchanged).

## Updates

`Apply(ctx, id, Change)` is the single path for issue-record changes (`store.go:1073-1132`): read current → plan the lifecycle action (if any) → plan field updates against the **post-action** issue → if anything moved, run both in one `apply update` mutation → re-read and return. Planning errors abort before any write: an invalid field paired with a valid transition leaves everything untouched. Actor defaults to `unknown`.

**Status transitions** (`store.go:1274-1412`): the transition is applied in memory (a frozen — archived/deleted — issue refuses with "cannot <action> archived or deleted issue"); only `Start` rewrites the assignee. A same-status, same-assignee result is a **no-op**: no write, no event, no `UpdatedAt` bump. Otherwise a guarded UPDATE (`… WHERE id = ? AND status = ?`) touches only the status-axis columns (`status`, `assignee`, `updated_at`, `closed_at`, `resolution`, `redirect_target`); zero rows affected means a concurrent transition won, surfaced as e.g. `close conflict: issue status is "closed"`. Change rows are recorded per moved field: status, closed_at, resolution, redirect_target, assignee.

**Redirect-target validation** runs in the same transaction as the close (`store.go:1539-1557`): a redirecting resolution requires a target; self-redirect is rejected; the target must exist and not be deleted (archived is fine). A failed validation rolls back the whole close. A concurrent delete of the target between plan and write is still caught.

**Retention transitions** use a null-safe CAS (`… WHERE archived_at <=> ? AND deleted_at <=> ?`) touching only `updated_at` + the retention pair; a lost race surfaces as e.g. `archive conflict: issue retention is "archived"`. There is no retention no-op — the transition table has no same-state success cell.

**Field updates** (`store.go:950-1062`): title trimmed and non-empty; description/prompt/assignee/lane trimmed; container↔leaf type changes refused ("lifecycle capability would change"); labels canonicalized and replaced as a whole set. The UPDATE is unguarded (no CAS) but touches no lifecycle column, so a stale field plan cannot clobber a concurrent close or archive. Change rows record only fields that moved (priority as its numeric string; labels as comma-joined comparison).

## Events

`recordEvent` is the single insertion point (`store.go:1808-1846`): id `evt-<uuid>`, trimmed action/reason/actor (blank actor → `unknown`), `created_at` now-UTC, attribution read off the store. Empty action stores SQL NULL; empty from/to values in change rows store NULL. Which mutation emits what:

| Mutation | `action` | change rows |
|---|---|---|
| create (leaf) | `created` | one `status ""→"open"` |
| create (container) | `created` | none |
| status transition | the verb (`start`/`done`/`close`/`reopen`) | moved fields only |
| retention transition | the verb (`archive`/…) | `archived_at`/`deleted_at` as moved |
| field update | NULL | one per moved field |
| add/delete comment, record sync state | no event | — |

## Comments

`AddComment`: issue must exist; body trimmed and required; id `cmt-<uuid>`; creator defaults `unknown`; one insert, no event, and the issue row is untouched (the returned issue is the pre-comment read). `DeleteComment`: reads and deletes in the same transaction (no TOCTOU gap); missing → `NotFoundError`; returns the fully-populated deleted comment (`store.go:1139-1197`).

## Labels

Normalization is the model rule (lowercase, trimmed, non-empty, no commas — `01-data-model.md`); `canonicalizeLabels` additionally dedupes (first occurrence) and sorts ascending. `AddLabel` upserts (`ON DUPLICATE KEY UPDATE issue_id = issue_id`), so a re-add is a no-op preserving the original `created_at`/`created_by`, and returns the full post-add label set. `RemoveLabel` deletes by key; zero rows → `NotFoundError` (a genuine driver error is not masked as not-found). `ReplaceLabels`/`replaceLabelsTx` clears the whole set and re-inserts with one shared timestamp — **surviving labels get their `created_at`/`created_by` rewritten**. There is no label-rename operation anywhere. List reads sort by label ascending.

## Relations

The three types and their store canonicalizations are in `01-data-model.md`: `blocks` stored dependent→dependency, `related-to` endpoint-sorted, `parent-child` single-valued from the child.

`AddRelation` (`relations.go:293-341`): related-to self-edge rejected pre-transaction; endpoints canonicalized; both endpoints must exist (archived/deleted rows count as existing); blocks edges run **cycle detection** — a self-block or any direct/transitive cycle is rejected with a message explaining that a cycle has no valid rank order. Parent-child routes through a delete-then-insert that enforces at most one parent (adding a second parent silently replaces the first); other types use a plain insert, so an exact duplicate surfaces the primary-key error (no upsert).

`SetParent`: blank ids and self-parenting rejected; both must exist; same single-valued replace; **no ancestry cycle check on write** — a parent cycle is only caught at read time by the ancestor-chain walk. `ClearParent` deletes the child's parent edge (zero rows → `NotFoundError`). `RemoveRelation` canonicalizes first (so related-to removal is order-insensitive) and needs no endpoint existence.

Reads: `ListRelationsForIssue` returns all incident relations ordered by `created_at`, optionally type-filtered in Go. Batch loading (`GetRelationsByIDs`) covers only the **structural** types (blocks, parent-child; related-to excluded), runs one query per endpoint column (deliberately avoiding a large OR), dedupes on the primary key, orders by `(created_at, src, dst, type)`, hydrates all endpoints in one batch, and buckets per subject exactly as `GetIssueDetail` does; vanished subjects are omitted. `ListChildren` joins child rows in `(item_rank, id)` order with no deleted filter.

## Ranking

### Representation

Ranks are base-62 strings (`0-9A-Za-z`, ASCII order = rank order) compared bytewise; `""` means unranked and every rank query excludes it (with three exceptions noted below). Constants: initial rank `"V"` (the alphabet midpoint), smoothing threshold 8 chars, smoothing window 32 rows, minimum spaced gap 16 code points (`internal/rank/rank.go`). `Midpoint(a,b)` produces a string strictly between two bounds (empty bound = before-/after-everything), growing one character when the gap closes, so insertion never renumbers neighbors. `SpacedRanks`/`SpacedRanksBetween` emit n evenly-spaced fixed-width ranks.

### Frames

Rank comparisons are **frame-local**: an issue is only comparable to siblings under the same container or fellow top-level items (`ranking.go:225-235`). Cross-frame rank verbs resolve both ids through their ancestor chains (walking non-deleted parents; a revisit errors "parent cycle at <id>") to representatives one level below the lowest common ancestor — so ranking a child of epic A against a standalone issue actually moves **epic A**. Ranking an issue against its own container (either direction) is refused ("…rank it against a sibling instead"). `resolveRankPair` returns which id actually moves (`RankMove{MovedID, AnchorID}`); this is why `lit rank` can move a different issue than the one named.

### The five verbs

- `RankToTop` / `RankToBottom`: global — new rank before the current min / after the current max live rank; no frame resolution.
- `RankAbove` / `RankBelow`: frame-resolved; the new rank is the midpoint between the anchor and its neighbor (or before/after the anchor at the edge). The neighbor queries filter `deleted_at IS NULL` but **not** `item_rank != ''`, unlike the top/bottom/create queries — unranked rows sort as the empty string there.
- `RankSet(ids)`: ≥2 unique non-blank ids required; each resolves through frames, and two ids collapsing to the same representative are rejected ("their relative order is internal to <epic>…"). The whole resolved set is stacked **at the top** of the keyspace in the given order, atomically, sharing one timestamp. Resolutions are returned even when the mutation fails.

Every rank write also bumps `updated_at`, except smoothing.

### Smoothing

When a written rank reaches 8 characters, the store rebalances a window of up to 32 adjacent rows (16 below, 16 above, in the **global** keyspace — frames are ignored) into evenly spaced fixed-width ranks between the window's outside neighbors (`ranking.go:401-500`). Smoothing does not touch `updated_at`.

### Rank inversions

An inversion is a live `blocks` edge whose dependency ranks below its dependent. `liveRankInversions` counts them (liveness by derived state — open/in_progress, excluding archived/deleted — so epics roll up correctly); Doctor reports the count. `FixRankInversions` (`ranking.go:778-882`) snapshots the live set, refuses if the live blocks graph has a cycle (naming the cycle path and suggesting `lit dep rm`), then loops: pick the first inversion per distinct dependency, move the dependency to just above its highest-ranked dependent (midpoint), smooth, repeat until no inversions remain — detecting non-convergence by a repeated inversion-set snapshot. Returns the number of re-rank operations.

## The shapemap (schema-drift recovery mapping)

The shapemap is the declarative bridge used by recovery (chapter 04): it maps a **raw dump** (tables → columns → positional rows, as produced by `DumpRaw`) into a `model.Export`, so data can be lifted out of a workspace whose schema this binary does not manage.

- A `ShapeMapping` lists per-table **emitters** (into one of six collections: issues, relations, comments, labels, events, event_changes) and **drops** (columns discarded with a provenance: `intended` — bookkeeping tables or columns dropped by a known migration — or `unexplained`). A closed **target registry** (44 entries, `shapemap.go:216-265`) fixes each collection's fields, which transform each admits (`identity`, `timestamp`, `legacy_status_value`, `event_action`, `event_reason`, `event_actor`), and which fields are required.
- `DeterministicMap` auto-derives a mapping with no operator input: bookkeeping tables (`goose_db_version`, `migration_quarantine`, `meta`) drop wholesale; `issue_history` fans out into an events emitter plus a conditional event_changes emitter (change row only when normalized from/to differ; `created_by` becomes the actor); any other table must match the known-columns registry exactly — historical aliases included (`prompt`→prompt, `issue_events.assignee`→actor, `labels.label`→name). Any unknown table or column makes it decline (return false) rather than guess; an operator-authored mapping (JSON) then covers the gap.
- `Validate` enforces: totality (every dump column mapped or dropped), known collections/fields, admissible transforms, required-field coverage, string-only constants on identity fields, condition fields produced, and row arity. `Apply` re-validates, folds rows through the transforms (NULL-preserving; timestamps parse RFC3339Nano/RFC3339; legacy statuses canonicalize; actor blank→`unknown`), and assembles a Version-2 export — hydrating issues exactly as the live read path does (container status ignored, retention from the timestamp pair). An event-change referencing an unknown event id errors.
- The JSON wire form is strict: unknown keys, trailing data, duplicate tables/fields/drops, and unknown source/condition kinds are all decode errors; encoding is deterministically sorted so re-marshal is byte-stable.
- Behavioral edges pinned in code: `issues.lane` is mapped and validated but **discarded at assembly** (`buildIssue` never reads it); `migrationDroppedCols` is currently empty (every DROP COLUMN in the corpus lives in a Down section); `DoltHead` from the dump is not carried into the export.

## The vendored Dolt driver

`internal/vendor/dolthub-driver` is a vendored copy of DoltHub's embedded SQL driver with local modifications (inventory, final section): DoltHub telemetry deleted outright; a process-wide mutex fencing engine construction against dolt global state; an MIT-licensed `MySQLError` replacing the MPL-2.0 `go-sql-driver/mysql` type; retryable-open plumbing (backoff on `nbs.ErrDatabaseLocked` / deadline errors); a forced eager DB load so lock errors reach the backoff instead of panicking; a relative-path fix; and iterator error-carry fixes.

## Test-fixture constraints worth knowing

The store test suite builds migrated-store templates once, copies them per test, and freezes the originals read-only (dirs too, since Dolt renames files); two independently created stores share no commit history. The Dolt engine passes the full storage conformance suite and offers exactly the declared capability set (`conformance_test.go:21-51`). Two per-instance test seams exist on `Store` (a pre-mutation hook in `Apply` and a hook at the top of every working-set commit, `store.go:66-81`), plus `ExecRawForTest`, which executes SQL with no commit lock and no Dolt commit (`store.go:334-337`).
