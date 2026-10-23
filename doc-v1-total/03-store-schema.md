# Dolt store: engine, schema, and core records

lit's shared backend stores every workspace in an embedded [Dolt](https://github.com/dolthub/dolt) database — a MySQL-compatible SQL engine with git-like versioning — living under a per-repo directory, database name `links` (`internal/store/store.go`). Every mutation lands as one SQL transaction followed by one Dolt commit, so the store carries its own full history independent of the issue-event table. This chapter covers the engine lifecycle, commit and locking semantics, the complete SQL schema and its reconciliation machinery, and the core record operations: create, read, update, purge, events, comments, labels, relations, and ranking, plus the shapemap recovery mapping. Import/export, verify/recover, checkpoints, and the adopt/candidate/promote/downgrade workspace flows are in `04-store-operations.md`; sync, compaction, and migration-runner mechanics are in `05-sync-merge-compaction.md`.

## Engine lifecycle

### Open modes

A `Store` wraps exactly one pooled SQL connection (`SetMaxOpenConns(1)` in `openDoltPool`, `store.go`) opened through a vendored embedded-Dolt driver. Two access modes (`store.go`):

- **Write** (`Open`, and the sync-side `OpenSync`): the connector gets the store's one wait policy (`holdWait`, keyed by Dolt's `LOCK` holder directory: a poll every `storeLockPollInterval` = 100ms, giving up once no new holder has arrived in front of it for `coResidentHolderWait` = `mirrorHoldCeiling` + 8 polls = 2.3s, assigned in `newEngineOpenBackOff`, `store.go`) and pings eagerly so lock contention surfaces at open time, reported while it waits (`lit: still waiting for <LOCK> after <d>; held by …`) and named in the refusal (`another process is holding this workspace's Dolt store open, held by pid <pid> (<command>) holding since <RFC3339> (<age>); retry after it completes`). A write engine that opened publishes a holder record for `LOCK` for its life (`recordLockHolder`), retired once by `closeEngine` before the engine releases the lock, so a second close is a no-op; a read engine records nothing.
- **Read** (`OpenForRead`): no backoff; the engine is pinged at construction like a write engine's, and a read open beside a foreign lock holder succeeds via Dolt's read-only fallback (journal wait ~100ms) and serves reads (`store.go`) from the root its engine loaded at open, never the holder's later commits (`doc.go`).

If another process holds Dolt's journal lock (`<root>/links/.dolt/noms/LOCK`), the wrapped error satisfies both `ErrWorkspaceBusy` and `nbs.ErrDatabaseLocked` and reads "another process is holding this workspace's Dolt store open … retry after it completes" (`store.go`).

### Open sequence

`Open(ctx, doltRootDir, workspaceID)` (`store.go`), in order: validate args (both non-blank; the root is `filepath.Clean`ed) → acquire the **shared workspace flock** → refuse if an adopt is pending → make the database exist under the exclusive **bootstrap flock** (if `links/.dolt` is absent: build a whole Dolt root in the sibling `.links-bootstrap` — `CREATE DATABASE IF NOT EXISTS links` through a first pool that closes before the second opens, then the default branch normalized to `master` — and rename it into place: the whole staged root when there is no root, otherwise just its `links/` into the existing root, so the database appears whole or not at all; any `received-refs.last` beside the root is removed first, `store.go`) → open the write connection → under the **commit lock**: normalize the default branch to `master` (renaming a sole non-master branch via `DOLT_BRANCH('-m', …)`, `store.go`) and run migrations. Failure at any point releases everything and returns the error.

`OpenForRead` differs in that it stats the directory first — a missing directory yields "repository not initialized with lit — run 'lit init' first" — never bootstraps, and takes **no** commit lock: it classifies and verifies the schema with reads alone (`assessMigration`, `migration_runner.go`) and serves when nothing would be written. A workspace whose schema trails the binary (a pending version, or applied-version content drift) is closed and reopened through `Open`, which migrates under the commit lock — so a read command still brings a stale workspace forward, but never applies DDL itself, and under a journal-lock holder it fails with the write open's contention refusal rather than a read-only DDL error (`store.go`). Re-opening a current-schema workspace adds **no** Dolt commit; migration is idempotent across opens.

`EnsureDatabase` performs the same validate → lock → adopt-check → bootstrap sequence standalone, returning what it did to make the database exist — `storage.Bootstrap`: `BootstrapNone`, `BootstrapBuilt`, or `BootstrapBuiltOverDeadBuild` (`store.go`). `Close` closes the DB (releasing the journal lock) before releasing the workspace lock (`store.go`). `reconnect` — used by the transient-GC retry — opens a new pool, swaps it in, closes the old one, and pings (`store.go`); it is the one place the journal lock is taken while the commit lock is held.

Two `Open`s on one root serialize: the second blocks until the first `Close`. After a SIGKILL mid-commit or mid-migration, a fresh open succeeds — no stale-lock state survives process death, because all locks are kernel flocks.

### Attribution

`AttributeTo(streamToken)` sets a `(stream, workspace-id)` attribution pair that is stamped on **every** subsequent event row (`store.go`). A blank token yields the absent pair; unattributed events store SQL NULL in both columns. Restoring an export preserves the producer's attribution rather than re-stamping the restorer's.

## Commit semantics and locking

### One mutation, one commit

Every mutation routes through `withMutation(ctx, message, fn)` (`commit_lock.go`): under the commit lock and the transient-GC retry, `BeginTx` → `fn` → `tx.Commit` → `DOLT_COMMIT('-Am', <message>)`. The `-A` stages everything; there is no separate add step and no `--skip-empty` — a "nothing to commit" error is absorbed as success (`commit_lock.go`). A retry after a successful `tx.Commit` resumes at the DOLT_COMMIT step without re-running `fn` (the `staged` flag, `commit_lock.go`). A commit stamp may also carry `--allow-empty`, `--date` (RFC3339 UTC), and `--author`; ordinary store mutations pass only the message. A combined transition+field update is exactly one Dolt commit.

**Dolt commit identity** derives entirely from the workspace id: author name = workspace id with `@`→`_` (blank → `links`), email = `<name>@links.local` (`store.go`).

Store-level commit messages used verbatim: `record sync state`, `create issue`, `apply update`, `add comment`, `delete comment` (`store.go`), `purge issue` (`purge.go`), `add plan review` (`plan_review.go`), plus `add label`, `remove label`, `replace labels`, `add relation`, `remove relation`, `set parent`, `clear parent`, `rank to top`, `rank set`, `rank to bottom`, `rank above`, `rank below`, `fix rank inversions` from their subsystems (`labels.go`, `relations.go`, `ranking.go`).

### The three locks

| Lock | Path | Mode | Budget | Notes |
|---|---|---|---|---|
| Workspace lock | `.links-workspace.lock` beside the store | shared on open (exclusive users covered in ch. 04) | — | held for the store's lifetime |
| Bootstrap lock | `.links-bootstrap.lock` beside the store | exclusive | `coResidentHolderWait` of unchanged holders | held only across the existence check and, when the database is absent, its build and install |
| Commit lock | `.links-commit-flock.lock` in the parent of the dolt root (`commit_lock.go`) | exclusive | `commitLockWaiterBudget()` = `coResidentHolderWait` + `rotationReserve()` = 5.6s of unchanged holders (`commit_lock.go`) | re-entrant via a context key (`commit_lock.go`) |

All three are zero-byte kernel flocks with **no** stale/PID/mtime heuristics — process death is the only release. Contention on the commit lock wraps as "another lit process is writing to this workspace … retry after it completes". A panic inside a mutation still releases the lock; a release failure after a successful operation prints a warning to stderr and returns success (`commit_lock.go`). A cancelled context returns `context.Canceled` rather than burning the budget.

### Transient-retry classification

`retryTransientGCContention` runs the operation, and on a transient failure rotates the connection (`reconnect`) once, with no sleep, and runs it once more; the rotation is announced on stderr and refused when it would carry the hold past `commitLockWaiterBudget()` (`commit_lock.go`). Retryable errors are exactly two lowercased-substring matches: "cannot update manifest"+"read only" (manifest read-only) and "online garbage collection"+"reconnect" (GC reset) (`commit_lock.go`). A manifest-read-only that survives the retry promotes to `WorkspaceWriteBlockedError` ("another lit process is holding this workspace open for writing; the store stayed read-only across every retry…"); a surviving GC-reset does not promote. Non-transient errors are never retried.

## The SQL schema

### Producers and pinning

Exactly four producers of live DDL exist: the goose baseline migration `00001_baseline.sql` (v1), goose migrations v2–v9 and v11 (`add_lane`, `add_resolution`, `add_redirect_target`, `add_event_attribution`, `add_follows_up`, `every_ticket_inside_a_container`, `blocked_by_edge`, `purges`, `plans_and_plan_reviews`; v10 changes a row and no DDL), the pre-goose reconciler (`schema_reconcile.go`), and the quarantine-table bootstrap (`migration_runner.go`) — plus goose's own `goose_db_version` bookkeeping table. The baseline file is **byte-frozen** by a SHA-256 canary test (`e86c1aa3…ad1fbb`); no schema hash is stored in the database — the recorded version is the integer in `goose_db_version`. The converged schema is pinned as a byte-compared `SHOW CREATE TABLE` golden file (`schema_snapshot.sql`), regenerated only via a test flag; the dump enumerates live tables, so a leftover table is drift.

All application tables are InnoDB, `utf8mb4_0900_bin`, no `AUTO_INCREMENT` anywhere except `goose_db_version.id`.

### Tables

Eleven tables. `issues` (`schema_snapshot.sql`):

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
| `follows_up` | VARCHAR(191) | NULL | — | v6 |
| `work_plan` | TEXT | NOT NULL | `''` | v11 |

Timestamps are **strings** (RFC3339Nano), parsed at one boundary (`scanTime`, `store.go`). Indexes: `idx_issues_rank (item_rank(191))`, `idx_issues_status_priority (status, priority, updated_at)`. Five named CHECK constraints: status is NULL exactly for epics and otherwise in (`open`,`in_progress`,`closed`); priority in [0,1]; type in the five-type set; resolution NULL or in the four-resolution set; `redirect_target` non-NULL only when resolution is `duplicate` or `superseded`. `redirect_target` deliberately has **no** foreign key.

The other tables:

| Table | Columns | Keys / constraints |
|---|---|---|
| `relations` | `src_id`, `dst_id` VARCHAR(191); `type` VARCHAR(32); `created_at` VARCHAR(64); `created_by` TEXT | PK `(src_id,dst_id,type)`; FKs both endpoints → `issues(id)` ON DELETE CASCADE; CHECK type in (`blocked-by`,`parent-child`,`related-to`) (v8); indexes `(src_id,type)`, `(dst_id,type)` |
| `comments` | `id`, `issue_id` VARCHAR(191); `body` TEXT; `created_at`; `created_by` | PK `(id)`; FK → issues CASCADE; index `(issue_id,created_at)` |
| `labels` | `issue_id`, `label` VARCHAR(191); `created_at`; `created_by` | PK `(issue_id,label)`; FK → issues CASCADE; indexes `(issue_id,label)`, `(label,issue_id)` |
| `issue_events` | `id`, `issue_id` VARCHAR(191); `action` VARCHAR(64) NULL; `reason`, `actor` TEXT NOT NULL; `created_at`; `stream_id` VARCHAR(64) NULL (v5); `workspace_id` VARCHAR(191) NULL (v5) | PK `(id)`; FK → issues CASCADE; index `(issue_id,created_at)` |
| `issue_event_changes` | `event_id` VARCHAR(191); `field` VARCHAR(64); `from_value`, `to_value` TEXT NULL | PK `(event_id,field)`; FK → issue_events CASCADE |
| `purges` | `issue_id` VARCHAR(191); `issue_created_at` VARCHAR(64); `purged_at` VARCHAR(64); `purged_by` TEXT — all NOT NULL (v9) | PK `(issue_id, issue_created_at)`; **no foreign key** on either key column — the issue it names no longer exists |
| `plan_reviews` | `id`, `issue_id` VARCHAR(191); `plan_digest` VARCHAR(64); `verdict` VARCHAR(16); `laws` TEXT; `created_at` VARCHAR(64); `created_by` TEXT — all NOT NULL (v11) | PK `(id)`; FK `plan_reviews_issue` → issues CASCADE; CHECK `plan_reviews_verdict_check` verdict in (`pass`,`fail`); index `(issue_id,created_at)` |
| `meta` | `meta_key` VARCHAR(191) PK; `meta_value` TEXT | key-value; upsert via `ON DUPLICATE KEY UPDATE` |
| `migration_quarantine` | `version` BIGINT PK; `name`, `error_text` TEXT; `created_at` | created outside the goose batch so a rollback cannot erase it |
| `goose_db_version` | goose's own (id AUTO_INCREMENT, version_id, is_applied, tstamp) | migration bookkeeping |

`issues.work_plan` holds the issue's `plan`, the empty text meaning no plan; the column is not named `plan` because PLAN is reserved in the SQL dialect (`00011_plans_and_plan_reviews.sql`). In `plan_reviews`, `plan_digest` is the hex SHA-256 of the plan text the review judged and `laws` the cited tokens joined by commas, the empty text for none (`joinLaws`, `splitLaws`, `plan_review.go`).

Ordinary issue deletion is a soft `deleted_at` stamp; the one CRUD path that deletes an issue row is `Purge` (below), and the ON DELETE CASCADEs fire from it.

`meta` keys used in production: `workspace_id`, `last_sync_path`, `last_sync_hash`, `min_writer_schema`. `GetSyncState`/`RecordSyncState` read/write the second and third (`store.go`). `min_writer_schema` is set only by a migration that changes a write rule, and read only from a workspace ahead of the binary (`schema_operability.go`).

A legacy `issue_history` table (columns `id, issue_id, action, reason, from_status, to_status, created_at, created_by`) no longer exists in any current schema; its presence on disk is the marker of a pre-goose workspace.

### Pre-goose reconciliation

When a workspace predates goose management (marker: `issue_history` exists, or canonical tables exist with no goose log — `migration_runner.go`), `reconcileToBaseline` (`schema_reconcile.go`) forward-migrates it to the v1 baseline before goose applies v2–v11. Steps, in order: create any missing v1 tables/indexes (FK-safe order, `meta` first); drop a fabricated `goose_db_version`; rename `issue_events.assignee`→`actor`; translate `issue_history` rows into events (one event per row, plus one `status` field-change when the normalized from/to actually differ; orphaned rows skipped; idempotent against existing event ids); drop `issue_history`; add `item_rank` + its index; add `topic` (backfilled `'misc'`, then its temporary default dropped); rename `prompt`→`agent_prompt`, add/relax it as needed; normalize statuses (`in-progress`→`in_progress`, `todo`→`open`, `done`→`closed`, anything invalid→`open`; `closed_at` implies closed and vice versa; epic status forced NULL); backfill missing topics and ranks (unranked rows get sequential ranks appended after the current max, ordered by status/priority/updated_at/id); reset out-of-range priorities to 0 and install the canonical priority CHECK; canonicalize the status CHECK; record `workspace_id` in meta.

Each step is gated by an `information_schema` probe (table/index/column existence, nullability, default presence, CHECK-clause shape, or a row-level predicate); CREATE steps swallow "already exists"-class races, mutation steps don't. Reconcile compares **presence only** — never column types, index shapes, FKs, or collations. A pre-gate refuses shapes missing `status`/`priority`/`updated_at`/`issue_type`/`closed_at`/`description` ("not a known historical shape"); a post-gate re-verifies every baseline column and aborts before stamping v1 if gaps remain. Down-migration effects (v5→v1) drop the added columns in reverse; v4's Down re-materializes one `related-to` edge per redirect target, and v4's Up did the inverse backfill-and-delete.

## Creating issues

`CreateIssue` (`store.go`) validates before the transaction: title required (trimmed non-empty); type defaults to `task`; labels canonicalized; topic normalized (required); a leaf with no parent refused (`storage.RejectLeafWithoutParent`). Inside one `create issue` mutation: the parent (if given) must exist and be an epic — `filingParentTx` (`ranking.go`) reads its type with its frame, and `storage.RejectParentThatIsNotAContainer` judges it — and so must the ticket `follows_up` names (if given), retained or not — the column has no foreign key, so this is where a dangling link is refused; the workspace prefix is normalized; the ID is minted; the rank is placed; the row is inserted (`closed_at` literal NULL; `follows_up` from the input, NULL when empty; `resolution`/`redirect_target` not in the insert list); a `parent-child` edge is written for a parent; labels are replaced; a `created` event is recorded (reason `issue created`, actor `storage.Actor(in.Actor)`, which also signs the parent edge and initial labels and is hashed into the id); ranks are smoothed if needed. The returned issue is the in-memory value, not a re-read. Leaves store `status='open'`; containers store NULL (enforced by the CHECK).

### ID minting

Every ID is a namespace followed by a base-36 hash (grammar and slug rules in `01-data-model.md`). A top-level ID's namespace is `<prefix>-<topic>-`; a child's is its parent's ID and a dot, so a child ID is `<parentID>.<hash>` and grandchildren nest (`<parentID>.<hash>.<hash>`). Both are minted by one function, `issueid.Mint`; `idSpace` (`issue_ids.go`) chooses the namespace and its population, and nothing else differs.

The population sets the starting hash length: the smallest of 3–8 that keeps the birthday-collision probability ≤ 0.25. For a top-level ID it is the count of IDs containing no dot, across every topic and including archived/deleted rows; for a child it is the count of `parent-child` edges into the parent (`countChildren`). A failed count fails the create. For each length from that start to 8, up to 10 nonces are tried; the first candidate with no `issues` row (soft-deleted included) and no purge record wins — a purged ID is taken for good (`issue_ids.go`). Exhaustion errors out with `generate unique issue id: exhausted lengths <start>-8` (`internal/issueid/generate.go`).

There is no ID parser/validator on lookup — supplied IDs bind verbatim, with no case normalization.

### Rank placement at create

Default placement is bottom (the `RankPlacement` zero value): rank = `After(max live rank)`, or the initial rank `"V"` in an empty workspace; `RankTop` takes the midpoint between the leading rank of the frame the issue is filed into (the live parent named at create, or the top level) and the nearest rank the whole workspace holds below that leader — `Before(leader)` when the workspace holds no rank below it. A frame holding nothing ranked yet offers no leader to measure against, so the key lands just past the rank of the issue that contains it: the midpoint between the container's own rank and the next rank above it, or `After(container rank)` when nothing sorts above it. With no container to sit beside — the top level, or a container carrying no rank — the key goes past the workspace's last rank, and is `rank.Initial()` only when nothing in the workspace is ranked. Bounds leaving no room between them make the store respace the ranks around the upper bound and read both bounds again (`store.go`; `internal/store/ranking.go`). Consecutive default creates therefore keep authoring order.

## Reads

All issue reads share one 20-column projection (`issueColumns`, `store.go`) and one hydration path. `hydrateIssues` uses a **fixed query count per recursion level**, not per issue: one labels query for all ids, one children query for all container ids (`store.go`). The descent carries the chain of epics it came down through; a child already on its epic's chain is a parent loop, and the read returns `storage.ParentCycleError` naming the loop instead of descending into it (`store.go`). The children query's visibility rule, encoded in `scanLifecycleChildRows`'s WHERE clause: a live parent sees only live children; an archived/deleted parent sees all its children — so an active epic's progress excludes archived children, but the same epic once archived counts them (`store.go`).

- `GetIssue`: single-row lookup; missing → `storage.NotFoundError`.
- `getIssuesByIDs`: one `IN` query; missing ids are silently absent from the map.
- `GetIssueDetail` (`store.go`): the issue + its relations (all incident, ordered by `created_at`), comments, events, plan reviews (`queryPlanReviews`, `plan_review.go`, sorted in Go by `storage.PlanReviewOrdering` — the `created_at` instant, then `id`; the SQL has no ORDER BY, `created_at` being a varchar), then one batch hydrate of every relation counterparty plus the redirect target; buckets into Parent/Children/BlockedBy/Blocks; siblings = the parent's other children in rank order (empty for parentless issues); `Related` carries only manual `related-to` edges; the redirect target hydrates independently of the graph and is absent if the target row vanished.
- `ListTopics`: distinct non-empty topics of non-deleted issues, ascending.
- `ListIssueIdentities`: `SELECT id, topic FROM issues ORDER BY id ASC` — every row, retention ignored, nothing hydrated, so it answers where a parent loop stops `ListIssues`.

### ListIssues filtering

`ListIssues` (`store.go`) builds SQL WHERE clauses in a fixed order: exclude archived unless `IncludeArchived`; exclude deleted unless `IncludeDeleted`; `issue_type IN` / `NOT IN`; `assignee IN` (blank entries skipped); `updated_at >= / <=`; comment existence / non-existence; one `EXISTS` clause per label in `LabelsAll` (AND semantics); `id IN`; and per search term a `%term%` LIKE over lowercased title, description, prompt, plan, and topic. **Status and resolution are not filtered in SQL** — they filter post-hydration against the *derived* state, so epic states come from child rollup, never the dead NULL column. Resolution filtering drops issues with no resolution. `Limit ≤ 0` means uncapped.

Sorting: default `rank` ascending — tree order (`storage.RankAncestry.Compare`) — then `id` (`storage.IssueOrdering`, `internal/storage/ordering.go`). Allowed sort fields (case-insensitive): `id`, `title`, `status`, `priority`, `rank`, `type`, `topic`, `assignee`, `created_at`, `updated_at`; unknown fields error; `id ASC` is always the final tiebreaker. `status` sorts the derived lifecycle state, so a container sorts by the state its children give it.

### Event reads

One LEFT JOIN query collapses `issue_events` × `issue_event_changes` into events with ordered change lists, sorted `(created_at, id, field)` so re-reads compare identical (`store.go`). `ListAllEvents` applies **no recency cutoff** — claim derivation needs arbitrarily old establishing events — and reads write nothing (Dolt HEAD unchanged).

## Updates

`Apply(ctx, id, Change)` is the single path for issue-record changes (`store.go`): read current → plan the lifecycle action (if any) → plan field updates against the **post-action** issue → if anything moved, run both in one `apply update` mutation → re-read and return. Planning errors abort before any write: an invalid field paired with a valid transition leaves everything untouched. Actor defaults to `unknown`.

**Status transitions** (`store.go`): the transition is applied in memory (a frozen — archived/deleted — issue refuses with `model.FrozenIssueError`, "issue <id> is <archived|deleted>, so its status cannot change: run `lit <unarchive|restore> <id>` first"); only `Start` rewrites the assignee, and `Issue.Apply` (`model.go`) refuses a start whose assignee carries a comma (`model.NormalizeAssignee`), before anything is written and, in `lit start`, before the takeover gate. A same-status, same-assignee result is a **no-op**: no write, no event, no `UpdatedAt` bump. Otherwise a guarded UPDATE (`… WHERE id = ? AND status = ?`) touches only the status-axis columns (`status`, `assignee`, `updated_at`, `closed_at`, `resolution`, `redirect_target`); zero rows affected means a concurrent transition won, surfaced as e.g. `close conflict: issue status is "closed"`. Change rows are recorded per moved field: status, closed_at, resolution, redirect_target, assignee.

**Redirect-target validation** runs in the same transaction as the close (`store.go`): a redirecting resolution requires a target; self-redirect is rejected; the target must exist and not be deleted (archived is fine). A failed validation rolls back the whole close. A concurrent delete of the target between plan and write is still caught.

**Retention transitions** use a null-safe CAS (`… WHERE archived_at <=> ? AND deleted_at <=> ?`) touching only `updated_at` + the retention pair; a lost race surfaces as e.g. `archive conflict: issue retention is "archived"`. There is no retention no-op — the transition table has no same-state success cell.

**Field updates** (`store.go`): title trimmed and non-empty; description/prompt/plan/lane trimmed; assignee through `model.NormalizeAssignee` (trimmed, a comma refused); container↔leaf type changes refused ("lifecycle capability would change"); labels canonicalized and replaced as a whole set. The UPDATE is unguarded (no CAS) but touches no lifecycle column, so a stale field plan cannot clobber a concurrent close or archive. Change rows record only fields that moved (priority as its numeric string; labels as comma-joined comparison).

## Purging

`Purge(ctx, PurgeInput{IssueID, Actor})` (`purge.go`) runs as one `purge issue` mutation — one transaction, one Dolt commit. Inside it: `SELECT id, created_at FROM issues WHERE id = ?` (no row → `storage.NotFoundError`); read each child with a parent-child edge onto the issue, with its type and the count of its other parent-child edges (`detachedChildrenTx`, retention ignored), and refuse when any child that is not an epic would keep no parent edge (`storage.RejectPurgeOfOccupiedContainer`, `model.ValidationError`); count the issue's comments, labels, relations with it at either end, `issue_events` rows, and `plan_reviews` rows in one SELECT of five subqueries; delete the issue row through `deleteIssueTx`, letting ON DELETE CASCADE take the comments, labels, relations, events, event changes, and plan reviews; insert the purge record (`issue_created_at` = the issue's `created_at`, spelled in UTC RFC3339Nano by `model.Purge.Key`; `purged_at` = store clock, RFC3339Nano; `purged_by` = the input actor, verbatim) through `insertPurgeTx`, the only INSERT into `purges`. Returns `storage.PurgeResult` with the record and the five counts. No `issue_events` row is written; other issues' rows are not touched.

## Events

`newEvent` stamps every event the store writes (`store.go`): id `evt-<uuid>`, trimmed action/reason/actor (blank actor → `unknown`), `created_at` now-UTC, attribution read off the store. `insertEventTx` is the one writer of an event and its change rows (`import_export.go`): `recordEvent` routes a mutation's event through it (`store.go`), and the events a resolved reconcile mints reach it inside its settled export (`landProse`, `sync_reconcile.go`). An edit's event and its `updated_at` share one instant (`stampEdit`, `store.go`). In `insertEventTx`, empty action stores SQL NULL, empty from/to values in change rows store NULL, and a blank field name is refused. Which mutation emits what:

| Mutation | `action` | change rows |
|---|---|---|
| create (leaf) | `created` | one `status ""→"open"` |
| create (container) | `created` | none |
| status transition | the verb (`start`/`done`/`close`/`reopen`) | moved fields only |
| retention transition | the verb (`archive`/…) | `archived_at`/`deleted_at` as moved |
| field update | NULL | one per moved field |
| add/delete comment, add plan review, record sync state | no event | — |
| purge | no event (the issue's events are deleted with it) | — |

## Comments

`AddComment`: issue must exist; body trimmed and required; id `cmt-<uuid>`; creator defaults `unknown`; one insert, no event, and the issue row is untouched (the returned issue is the pre-comment read). `DeleteComment`: reads and deletes in the same transaction (no TOCTOU gap); missing → `NotFoundError`; returns the fully-populated deleted comment (`store.go`).

## Labels

Normalization is the model rule (lowercase, trimmed, non-empty, no commas — `01-data-model.md`); `canonicalizeLabels` additionally dedupes (first occurrence) and sorts ascending. `AddLabel` upserts (`ON DUPLICATE KEY UPDATE issue_id = issue_id`), so a re-add is a no-op preserving the original `created_at`/`created_by`, and returns the full post-add label set. `RemoveLabel` deletes by key; zero rows → `NotFoundError` (a genuine driver error is not masked as not-found). `ReplaceLabels`/`replaceLabelsTx` clears the whole set and re-inserts with one shared timestamp — **surviving labels get their `created_at`/`created_by` rewritten**. There is no label-rename operation anywhere. List reads sort by label ascending.

## Relations

The three types and their store canonicalizations are in `01-data-model.md`: `blocked-by` stored dependent→dependency, `related-to` endpoint-sorted, `parent-child` single-valued from the child.

`AddRelation` (`relations.go`): related-to self-edge rejected pre-transaction; endpoints canonicalized; both endpoints must exist (archived/deleted rows count as existing); the dst's type is read in the same query that proves it exists (`issueTypeTx`), and a parent-child edge onto a parent that is not an epic is refused (`storage.RejectNonContainerParentEdge`, `storage.RejectParentThatIsNotAContainer`, shared by both engines); blocked-by edges are refused between an issue and an epic it sits under, at any depth and in either direction, climbing only through epic parents, and between two non-container issues directly under one epic, with the same-epic validation message (`storage.RejectBlocksWithinEpic`, shared by both engines, so it also covers `ImportTree` and `BulkApply` `blocked_by`); a blocked-by edge that passes that rule then runs **cycle detection** — a self-edge or any direct/transitive cycle is rejected with a message explaining that a cycle has no valid rank order. Parent-child routes through a delete-then-insert that enforces at most one parent (adding a second parent silently replaces the first); other types use a plain insert, so an exact duplicate surfaces the primary-key error (no upsert).

`SetParent`: blank ids and self-parenting rejected; then the same write `lit dep add` uses (`addRelationTx`): both must exist, the parent must be an epic, an edge that would close a parent cycle is refused (`model.ValidationError`), and the single-valued replace. `ClearParent` reads the child's type on the tx — a column read, no hydration, so it answers on a hierarchy holding a loop — and refuses a leaf (`storage.RejectDetachingALeaf`); for an epic it deletes the parent edge (zero rows → `storage.RelationAbsentError`, a satisfied answer). `RemoveRelation` canonicalizes first (so related-to removal is order-insensitive), needs no endpoint existence, and after deleting a parent-child edge judges the same refusal against the parent edges the child still holds (`storage.RejectDetachingParentEdge`), rolling the delete back for a leaf left with none, so a missing edge is still reported missing and removing one of two parent edges passes.

Reads: `ListRelationsForIssue` returns all incident relations ordered by `created_at`, optionally type-filtered in Go. Batch loading (`GetRelationsByIDs`) covers only the **structural** types (blocked-by, parent-child; related-to excluded), runs one query per endpoint column (deliberately avoiding a large OR), dedupes on the primary key, orders by `(created_at, src, dst, type)`, hydrates all endpoints in one batch, and buckets per subject exactly as `GetIssueDetail` does; vanished subjects are omitted. Children are read through `ListIssues`, and not by a clause inside the issue scan: a non-empty `ParentIDs` is resolved against `relations` first — one query joining the parent ids, as an `idSet`, to `relations` on `dst_id` — and the resulting child ids become the listing's id filter, intersected with `filter.IDs` when the caller set both (`store.go`). So only direct children match, under the listing's usual retention filters and ordering, and the listing joins one id set rather than filtering on two. Every id-keyed read joins its ids as an `idSet` — a JSON_TABLE over one JSON-array parameter — so it is one query however many ids it carries, where an `IN (?, …)` list would cost the planner O(N²) (`idset.go`). Before that, `requireIssues` looks the parent ids up in `issues` and returns `NotFoundError` for the first one with no row in the caller's own order; a soft-deleted parent still has a row and passes (`store.go`).

## Ranking

### Representation

Ranks are base-62 strings (`0-9A-Za-z`, ASCII order = rank order) compared bytewise; `""` means unranked and every rank query excludes it (with three exceptions noted below). Constants: initial rank `"V"` (the alphabet midpoint), smoothing threshold 8 chars, smoothing window 32 rows, minimum spaced gap 16 code points (`internal/rank/rank.go`). `Midpoint(a,b)` produces a string strictly between two bounds (empty bound = before-/after-everything; both empty gives the initial rank `"V"`), growing one character when the gap closes; it refuses, with `rank.ErrNoRoom`, a pair of bounds that are equal once trailing zeros are removed, such as `"10"` and `"100"`. `SpacedRanks`/`SpacedRanksBetween` emit n evenly-spaced fixed-width ranks.

### Frames

Rank comparisons are **frame-local**: an issue is only comparable to siblings under the same container or fellow top-level items (`ranking.go`). Cross-frame rank verbs resolve both ids through their ancestor chains (walking non-deleted parents; a revisit is refused with `storage.ParentCycleError` naming the loop) to representatives one level below the lowest common ancestor — so ranking a child of epic A against a standalone issue actually moves **epic A**. Ranking an issue against its own container (either direction) is refused ("…rank it against a sibling instead"). `resolveRankPair` returns which id actually moves (`RankMove{MovedID, AnchorID}`); this is why `lit rank` can move a different issue than the one named.

### The five verbs

- `RankToTop` / `RankToBottom`: global — new rank before the current min / after the current max live rank; no frame resolution.
- `RankAbove` / `RankBelow`: frame-resolved; the new rank is the midpoint between the anchor and its neighbor (an open bound at the edge). When that pair has no room, the store respaces the smoothing window around the upper of the two ranks, with no length threshold, reads the anchor and neighbor again, and takes the midpoint of the new pair; a second refusal fails the move. The neighbor queries filter `deleted_at IS NULL` but **not** `item_rank != ''`, unlike the top/bottom/create queries — unranked rows sort as the empty string there.
- `RankSet(ids)`: ≥2 unique non-blank ids required; each resolves through frames, and two ids collapsing to the same representative are rejected ("their relative order is internal to <epic>…"). The whole resolved set is stacked **at the top of its frame** in the given order by permuting the keys the frame already holds, atomically; the representatives whose key changed share one `updated_at` timestamp. A failed write returns no resolutions.

Every rank write also bumps `updated_at`, except smoothing and the keys a `RankSet` passes to the frame members it displaces.

### Smoothing

When a written rank reaches 8 characters, the store rebalances a window of up to 32 adjacent rows (16 below, 16 above, in the **global** keyspace — frames are ignored) into evenly spaced fixed-width ranks (`ranking.go`). Ranks sharing a significant part — the rank with its trailing zeros removed — leave no room between them and sort contiguously, so the window first swallows the whole run each of its ends sits in, then takes each bound from outside that run: the greatest rank below the run's floor, and the least rank at or above its ceiling. Floor and ceiling are computed from the window's own ends rather than scanned for, which keeps every query bounded, and bounds chosen this way can never share a significant part, so there is always room. Smoothing does not touch `updated_at`.

### Rank inversions

An inversion is a `blocked-by` edge between two live issues (liveness by derived state — open/in_progress, excluding archived/deleted — so epics roll up correctly) whose dependency tree order lists after its dependent: the two are compared through their representatives in the one frame that holds both — each issue itself when they share a frame, else its container directly under the deepest container holding both — never by their own keys across frames, and an edge between an issue and its own container or descendant is never one, because tree order lists the container first whatever any key holds. Lifted onto those representatives, edges can run both ways between frame-mates with no loop among the issues — a task in one epic waits on a task in a second, which waits on another task in the first. Such frame-mates are interleaved (each strongly connected set of lifted edges is one group): no order of them lists every dependency first, so an edge inside a group is never an inversion either (`storage.DependencyOrder.Inverted`, `internal/storage/dependency_order.go`). The queue's `rank_inversion` annotation reads the same predicate. A blocked-by dependency cycle is a loop of the issues' own edges, which no rank order satisfies and none of whose work can start. `invertedEdges` (`rank_repair.go`) and `blocksCycle` (`rank_repair.go`) are the store's reading of each; Doctor's report of them, and of the interleaved groups, is in 04-store-operations.md. `FixRankInversions` (`ranking.go`) snapshots the live set, then in one mutation refuses on a dependency cycle, naming the cycle path and suggesting `lit dep rm`, and otherwise computes the stable topological order of the live issues and every issue a constraint names, in key order, under the lifted edges outside every group of frame-mates whose lifted edges loop (`storage.DependencyOrder.Constraints`): at each step, the earliest-ranked issue whose dependencies are all placed (`rank_repair.go`). Each lifted edge joins two frame-mates, so every frame reads its share of that order back; the edges inside a group do not reorder it. The issues whose stored ranks already ascend along that order — a longest increasing run, compared with trailing zeros ignored — are not written, and every other non-deleted issue with a rank keeps its key among them unless that key is one an anchor or another such issue holds, or is malformed or all zeros (`splitRankOrder`, `holdKeys`). Every other issue is spaced into the gaps the kept keys leave and gets a new rank and `updated_at` (`rank_repair.go`). The kept keys increase strictly, so of the non-deleted issues sharing one key at most one keeps it, and the repair leaves every key held once, a closed issue's included. Smoothing then runs once per rewritten rank and may re-space any rank in its window, those unwritten issues included, without changing order or `updated_at`. Returns the number of issues the repair rewrote.

## The shapemap (schema-drift recovery mapping)

The shapemap is the declarative bridge used by recovery (chapter 04): it maps a **raw dump** (tables → columns → positional rows, as produced by `DumpRaw`) into a `model.Export`, so data can be lifted out of a workspace whose schema this binary does not manage.

- A `ShapeMapping` lists per-table **emitters** (into one of eight collections: issues, relations, comments, labels, events, event_changes, purges, plan_reviews) and **drops** (columns discarded with a provenance: `intended` — bookkeeping tables or columns dropped by a known migration — or `unexplained`). A closed **target registry** (57 entries, `shapemap.go`) fixes each collection's fields, which transform each admits (`identity`, `timestamp`, `legacy_status_value`, `event_action`, `event_reason`, `event_actor`), and which fields are required.
- `DeterministicMap` auto-derives a mapping with no operator input: bookkeeping tables (`goose_db_version`, `migration_quarantine`, `meta`) drop wholesale; `issue_history` fans out into an events emitter plus a conditional event_changes emitter (change row only when normalized from/to differ; `created_by` becomes the actor); any other table must match the known-columns registry exactly — historical aliases included (`prompt`→prompt, `issue_events.assignee`→actor, `labels.label`→name), and `issues.work_plan` maps to `issues.plan`. Any unknown table or column makes it decline (return false) rather than guess; an operator-authored mapping (JSON) then covers the gap.
- `Validate` enforces: totality (every dump column mapped or dropped), known collections/fields, admissible transforms, required-field coverage, string-only constants on identity fields, condition fields produced, and row arity. `Apply` re-validates, folds rows through the transforms (NULL-preserving; timestamps parse RFC3339Nano/RFC3339; legacy statuses canonicalize; actor blank→`unknown`), and assembles a `model.ExportVersion` (5) export (purge records and plan reviews included), reading each relation type through `model.RelationTypeFromStored` (a stored `blocks` becomes `blocked-by`) — hydrating issues exactly as the live read path does (container status ignored, retention from the timestamp pair). An event-change referencing an unknown event id errors.
- The JSON wire form is strict: unknown keys, trailing data, duplicate tables/fields/drops, and unknown source/condition kinds are all decode errors; encoding is deterministically sorted so re-marshal is byte-stable.
- Behavioral edges pinned in code: `migrationDroppedCols` is currently empty (every DROP COLUMN in the corpus lives in a Down section); `DoltHead` from the dump is not carried into the export.

## The vendored Dolt driver

`internal/vendor/dolthub-driver` is a vendored copy of DoltHub's embedded SQL driver with local modifications (inventory, final section): DoltHub telemetry deleted outright; a process-wide mutex fencing engine construction against dolt global state; an MIT-licensed `MySQLError` replacing the MPL-2.0 `go-sql-driver/mysql` type; retryable-open plumbing (backoff on `nbs.ErrDatabaseLocked` / deadline errors); a forced eager DB load so lock errors reach the backoff instead of panicking; a relative-path fix; iterator error-carry fixes; and each statement running on its caller's context rather than the one the connection was opened with.

## Test-fixture constraints worth knowing

The store test suite builds migrated-store templates once, copies them per test, and freezes the originals read-only (dirs too, since Dolt renames files); two independently created stores share no commit history. The Dolt engine passes the full storage conformance suite and offers exactly the declared capability set (`conformance_test.go`). Two per-instance test seams exist on `Store` (a pre-mutation hook in `Apply` and a hook at the top of every working-set commit, `store.go`), plus `ExecRawForTest`, which executes SQL with no commit lock and no Dolt commit (`store.go`).
