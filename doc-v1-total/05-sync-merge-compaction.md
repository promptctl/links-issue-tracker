# Sync, merge, compaction, migrations, and backup

lit stores its issue database in Dolt — a SQL database with git-like commits, branches, and remotes. Sync between machines is therefore modeled on git: fetch, fast-forward, push, and — when both sides have committed since their common ancestor — a **reconcile** step that performs a field-aware three-way merge of the issue data and replays local history onto the remote head. There is no central server; any git-backed or Dolt remote works. This chapter covers the store-level machinery: sync entry points, the commit lock, the merge rules, compaction (Dolt garbage collection), schema migrations, checkpoints, and recovery snapshots. The CLI-level background sync engine that drives these operations on a cadence is chapter `07-ops-commands-and-sync-engine.md`.

## Opening a sync-capable store

`OpenSync` (`internal/store/sync.go:20`) is the sync-capable variant of store open. In order: validate arguments; check the embedded-dependency version floor; acquire the **workspace shared lock** before any database bootstrap (released, with its error joined, on any later failure); refuse if an adopt marker is pending; ensure the Dolt database exists (same initializer as `Store.Open`); open an eager write engine, waiting on Dolt's journal lock up to `engineOpenRetryMaxElapsed` = 30s (`store.go:2607`). Branch normalization then runs: a lock-free read of `masterRenameSource`, and only when it reports a non-empty source is `ensureMasterDefaultBranch` run under the commit lock — a read-only `OpenSync` takes no commit lock (`sync.go:82-87`).

The version floor (`sync.go:17-18`): embedded sync requires `github.com/dolthub/dolt/go` ≥ `v0.40.5-0.20260314011441-62975ef6bf36` and `github.com/dolthub/driver` ≥ `v0.2.1-0.20260314000741-0fe74e7ee31a`, checked via `debug.ReadBuildInfo` and `semver.Compare`; absent build info or an absent module skips the check (`sync.go:881-918`). Failure: `"embedded sync requires %s %s or newer (found %s)"`.

## Remotes and transport URLs

Remotes live in Dolt's own `dolt_remotes` table, managed by SQL procedures (`sync.go:100-191`):

| Operation | Mechanism |
|---|---|
| list | `SELECT name, url FROM dolt_remotes ORDER BY name`; result never nil |
| add | `CALL DOLT_REMOTE('add', name, url)` under the sync-mutation envelope; both args required (trimmed, `"%s is required"`) |
| remove | `CALL DOLT_REMOTE('remove', name)` |
| fetch | `CALL DOLT_FETCH(remote)`, with `--prune` **prepended** when requested |

`GitBackedRemoteURL` (`sync.go:136`) converts a git remote URL to Dolt's transport form: trim (empty → empty); try Dolt's `NormalizeGitRemoteUrl`; on failure retry with a synthetic `.git` suffix and strip it from the result; otherwise return the input unchanged. The function is idempotent.

## Freshness: classifying local vs. remote

`SyncFreshness` (`sync.go:699`) is a **pure local read** — it never touches the network. It probes `dolt_remote_branches` for the tracking ref `remotes/<remote>/<branch>`; zero rows is the never-synced state (no range queries run). Otherwise it counts commits each way with `SELECT COUNT(*), UNIX_TIMESTAMP(MIN(date)) FROM dolt_log('<from>..<to>')`. `OldestDivergedUnix` — the fork timestamp — is populated only when ahead *and* behind are both > 0, as the earlier of the two sides' oldest divergent commits; sub-second precision truncates (`sync.go:748-806`).

The five states (`internal/storage/sync.go:95`): `never_synced`, `up_to_date`, `ahead` (behind = 0), `behind` (ahead = 0), `diverged`.

## Receive, pull, push

**`SyncReceive`** (`sync.go:394`) = fetch + fast-forward only. After `DOLT_FETCH` and one freshness read: `behind` → `DOLT_MERGE --ff-only` onto the tracking ref (state `fast_forwarded`); `diverged` → state `diverged`, **no merge performed**; `ahead`/`never_synced`/default map to their states. Fast-forward is the only outcome that touches local data.

**`SyncPull`** (`sync.go:239`) = receive, then reconcile only on divergence, all under **one** commit lock (nested acquisitions are context-reentrant). Receive states map 1:1 to pull states; `diverged` runs `SyncReconcile` and maps its result: `linearized` → pull `linearized` with ahead/behind/fork-time **re-read** from a fresh freshness call; `prose_pending` carries the pending set; `unrelated_histories` carries the inventory; `not_diverged` → `up_to_date` with the fork timestamp zeroed. Any unhandled state is an error (`"sync pull: unhandled receive state %q"` / `"…reconcile state %q"`). lit deliberately does not use Dolt's native `DOLT_PULL`: its three-way working-set merge requires `autocommit` off and aborts under the driver's default (`sync.go:206-213`).

**`SyncPush`** (`sync.go:543`) runs `pushWithinLock` under the sync-mutation envelope and performs **no compaction** (`DOLT_GC` turns the embedded store read-only mid-run). The branch argument may be empty; a non-empty branch first runs the remote-schema-ahead guard (an empty branch skips it). Args build in order: `--set-upstream` if requested, `--force` if requested, the remote, then `HEAD:<branch>` if branch non-empty; `CALL DOLT_PUSH` returns a status int and message (NULL → `""`).

**`SyncCompactAndPush`** (`sync.go:562`), inside one envelope: choose the compaction depth (measured inside the lock), compact, push. After the push and **outside** the commit lock, the result's `Maintenance` string is assembled from the compaction report joined with the remote-cache prune report; a prune failure never fails the push.

**`SyncStatus`** (`sync.go:652`) reports Dolt version, active branch, head commit hash/message, the remote list, and the `dolt_status` rows (table, staged, status).

**Reset/adopt primitives**: `LocalIssueCount` returns 0 without error for a pristine store (probes `information_schema` first); `SyncResetToRemoteHead` hard-resets to `remotes/<remote>/<branch>` under the mutation envelope — destructive of local commits by design.

## The commit lock and transient-GC retry

Every store mutation funnels through `runSyncMutation` = commit lock + transient-GC retry (`sync.go:817`). The pieces (`internal/store/commit_lock.go`):

- **Lock**: an flock at `<store-parent>/.links-commit-flock.lock` — sibling of the database dir; the historical name `.links-commit.lock` is avoided because O_EXCL-era binaries unlink it, splitting the lock across inodes. Holder death releases it; there is **no** staleness or eviction heuristic. Budget: 9,000 attempts × 100ms ≈ 15 minutes, sized for a snapshot copy holding the lock. Re-entrant via a context marker. Contention (`ErrWorkspaceBusy`) is wrapped: `"another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes"`. A release failure after a successful operation prints to stderr and the operation still returns nil; after a failed operation the two errors are joined.
- **Transient retry**: Dolt's online GC can poison the active connection. Errors classified transient — message contains both `"cannot update manifest"` and `"read only"`, or both `"online garbage collection"` and `"reconnect"` — are retried up to 30 attempts with exponential backoff 50ms→1s cap (~25s total), rotating the connection between attempts. Exhaustion of the manifest-read-only case is promoted to `WorkspaceWriteBlockedError`: `"another lit process is holding this workspace open for writing; the store stayed read-only across every retry…"`.
- **Commit boundary**: `commitWorkingSetOnce` (`commit_lock.go:286`) is the only function that hands a commit to Dolt: `CALL DOLT_COMMIT('-Am', <message>)` — empty message defaults to `"links mutation"` — plus `--allow-empty` when stamped, `--date <UTC RFC3339>` (sub-second precision truncates) when a date is stamped, and `--author` when stamped. A `"nothing to commit"` error is success-with-no-commit. `withStampedMutation` runs `BeginTx → fn → tx.Commit → commit` inside the retry with a two-phase resume marker: once the SQL transaction commits, a retry resumes at the Dolt-commit step only, never re-running `fn`.

## The remote-schema-ahead guard

Before writing any commit below a remote head whose schema is newer than this binary understands, lit refuses (`internal/store/sync_schema_guard.go`). The guard reads the remote head's `MAX(version_id) FROM goose_db_version AS OF '<hash>'` (missing table or NULL → 0: a pre-goose remote is never ahead) and the producer's binary version from `meta AS OF '<hash>'`, and compares against the local migration registry's max. The commit hash is validated first — exactly 32 chars of Dolt's base32 alphabet `0-9a-v` — because `AS OF` cannot take a bound parameter and the hash is interpolated. The error names the versions and steers: with a recorded producer version, `` run `lit upgrade --to <version>` ``; without, "upgrade lit to a version that supports this schema".

Who is guarded: pushes with a non-empty branch; every reconcile that replays (three-way, combine, take-local), checked inside `replayUnderGuard` against the already-captured remote head so a concurrent fetch cannot shift the decision. Not guarded: pushes with an empty branch; **take-remote** — it authors no replay commit, and adopting an ahead head is a safe recovery (`sync_unrelated_take.go:171-176`). A never-synced branch has no remote head, so the push guard no-ops.

## Reconcile: field-aware merge of divergence

Reconcile is the operation that resolves a diverged local/remote pair. Three public entry points (`internal/store/sync_reconcile.go:211-249`):

| Entry | Settle policy | Unrelated-history handling |
|---|---|---|
| `SyncReconcile` | autonomous (never auto-picks prose) | detect only — classify, commit nothing |
| `SyncReconcileResolved` | agent-supplied prose resolutions | union combine |
| `SyncReconcileCombine` | autonomous | union combine (with a shared base, merges through it like an ordinary reconcile) |

**Plan capture** (under the already-held commit lock): freshness → if not diverged, return `not_diverged` with no anchors. Otherwise capture the data branch, local head, remote head, and the merge base via `SELECT DOLT_MERGE_BASE(?, ?)`. "No common ancestor" — detected by `sql.ErrNoRows` or the message substring `"no common ancestor"`, deliberately not error code 1105 (MySQL's catch-all) — means unrelated histories.

**Unrelated histories**: the store reads both sides' issue-ID sets with pure `AS OF` queries (no branch moves, no writes; a missing `issues` table reads as the empty set — a pristine bootstrap root) and partitions them into `only_local` / `only_remote` / `on_both` — sorted, mutually disjoint. Detect-only mode returns `unrelated_histories` with that inventory **before** the schema guard, scratch sweep, snapshot, or any reset. Union-combine mode proceeds to replay with an empty merge base.

**The replay envelope** (`replayUnderGuard`, `sync_reconcile.go:389`): schema guard first; then sweep stale scratch branches (`links-reconcile-scratch-%` — every failure prints to stderr and never fails the reconcile; the commit lock guarantees any such branch is an orphan); mint fresh scratch names `links-reconcile-scratch-<pid>-<unixnano>` with `-spine` and `-read` suffixes; create **one** snapshot guard carried across all retries (exactly one pre-reconcile snapshot however many GC-contention retries run); then run the body under the transient retry.

Scratch roles: `read` is hard-reset once per folded commit and keeps nothing; `spine` is hard-reset exactly once (to adopt the remote head) and thereafter only advances by commit. Both are created with `DOLT_CHECKOUT('-B', name, localHead)` (`-B` recreates leftovers from a prior retry), with a cleanup defer armed before the first creation. Cleanup checks out the data branch and deletes the created branches (a delete failure prints to stderr, not promoted); if the checkout back fails, the connection is rotated and the checkout retried — if that also fails, the error `"reconcile left the store on the scratch branch and could not recover…"` is unrecoverable, and a leftover scratch branch is deliberately left behind.

**The merge/settle/replay tail** (`mergeAndReplay`, `sync_reconcile.go:665`):

1. Export the local head and the remote head at fixed anchors (each read = checkout the read branch, hard-reset to the commit, **lift the working set to the current schema**, then run the normal export — so schema skew between the two sides is healed before comparison).
2. `merge.ThreeWay(base, ours, theirs)` — the base is the export at the merge-base commit for a shared-history reconcile, or the empty `model.Export{}` for combine, which the merge reads as "both sides changed every field from empty", i.e. a two-way union.
3. Apply the settle policy. If any prose conflicts remain pending, return `prose_pending` with the pending set and **nothing committed** — the data branch is still at the local head.
4. Otherwise replay: read the folded chain (`dolt_log('<remoteHead>..<localHead>')` — on unrelated histories this is the entire local chain; on a shared base, exactly the ahead commits; abort if the newest entry isn't the local head: `"folded chain starts at %q, want local head %q"`; reversed to oldest-first), then `commitReplayAndAdvance`.

**Settle policies** (`sync_reconcile.go:152-207`): `autonomousSettle` accepts a merge that settled on its own; otherwise surfaces the pending set — prose is never auto-committed by picking a side. `resolvedSettle` first honors self-settlement (the divergence may have converged between the agent reading and finalizing), then applies the supplied prose resolutions, accepted **only** when they form an exact bijection with the live pending set; a stale or partial set falls through and re-surfaces the current pending.

**`commitReplayAndAdvance`** (`sync_reconcile.go:778`), the safe replay:

1. Checkout the spine; hard-reset it to the remote head (its one and only reset); lift to the current schema; commit the lift as its own named commit `"reconcile: lift remote head to current schema"` — or no commit at all on a current-schema head (empty diff).
2. Seed a spine writer from a real export read. For each folded commit, oldest first: re-merge that commit's export against base and theirs (`ThreeWay(...).Provisional()`), and land the delta on the spine as a commit stamped with the **original commit's message, date, and author** — provenance is preserved. One step is read and landed before the next is read, so memory stays bounded. The Go-side diff decides which rows to write; Dolt decides whether a commit exists (empty diff → no commit).
3. Land the final merged export as an unconditional marker commit (`--allow-empty`) with the operation's message: `"reconcile: field-aware merge of remote divergence"` (three-way), `"reconcile: combine unrelated histories (union of both backlogs)"`, or `"reconcile: take local backlog over unrelated remote history"`.
4. Count the landed commits off the spine; `Replayed` = landed − 1 (excluding the marker).
5. **Snapshot-first**: ensure the pre-reconcile snapshot exists; a snapshot failure aborts before the data branch moves.
6. Checkout the data branch and hard-reset it to the replayed spine head — **the single atomic advance**. The invariant: the data branch is at its pre-reconcile head before this step and at the complete replayed spine after, never in between.
7. Prune pre-reconcile snapshots to 10 (a prune failure prints to stderr, not promoted — the replay already committed).

Each landing on the spine is a single attempt deliberately **without** the self-rotating transient retry — a rotation would reopen on the default branch and resume committing onto the data branch. A transient failure bubbles to the outer retry, which recreates both scratch branches and rebuilds the whole spine from the fixed anchors.

**Result vocabulary** (`internal/storage/sync.go:300-384`): states `not_diverged`, `linearized`, `prose_pending`, `unrelated_histories`, `took_local`, `took_remote`, `combined`; fields `Ahead`, `Behind`, `LocalHead`, `RemoteHead`, `BaseCommit`, `Pending`, `Unrelated`, `Replayed`. `Replayed` counts folded commits that actually landed — zero for non-mutating outcomes and for a fold whose every projection was already contained in the spine.

## Unrelated histories: the take flow

When local and remote share no ancestor, an agent cannot merge autonomously without owner input; picking a side is destructive. `SyncResolveUnrelated` (`sync_unrelated_take.go:93`) gates this behind an **owner-approval token**: the first 6 bytes (12 hex chars) of `SHA-256("take:" + side + NUL + localHead + NUL + remoteHead)`. The token is unexported — the only way to obtain it is to run the take and read the refusal — and any commit on either side, or approving one side and running the other, invalidates it.

Sequence: validate the side (`local` or `remote`; anything else: `"resolve unrelated histories: unknown side %q…"`); under the commit lock, capture the plan; not diverged → `not_diverged`. If the divergence **has** a base, refuse — take applies only to unrelated histories; the message directs to `lit sync reconcile`. Read the inventory **before** the approval check, so the refusal can name what would be destroyed. If the supplied approval doesn't match the expected token, return `OwnerApprovalRequiredError` with nothing mutated — `Stale: true` when an approval was supplied but no longer matches ("the backlog moved, or it was issued for the other side"), plus the token that would authorize right now, both heads, ahead/behind, and the inventory.

With approval:

- **take-remote**: its own snapshot guard, then a hard reset to the tracking ref under the transient retry. No scratch envelope, no schema-ahead refusal. Prunes pre-reconcile snapshots to 10 afterward (stderr on failure). Idempotent under retry (cached snapshot, fixed ref). State `took_remote`.
- **take-local**: the **full** replay envelope (schema guard included). Local history is replayed onto the remote head using the same no-base union projection combine uses — mid-chain commits stay whole backlogs — but the terminal export is **local's content, not the union**, so the marker commit's diff is exactly the discard of the remote-only issues. State `took_local`.
- **combine** is not a take: it is non-destructive, reached only via the reconcile entry points, and does not refuse shared history.

## What the replay writes: export deltas

The replay never issues UPDATEs. `diffExports` (`internal/store/export_delta.go`) compares two whole exports table by table:

- Per table, rows are indexed by key — issues by `id`; relations by `(src_id, dst_id, type)`; comments by `id`; labels by `(issue_id, name)`; events by `id`. A live row absent from the wanted set (or whose persisted projection differs, by `reflect.DeepEqual`) is queued for removal; a wanted row absent from live (or differing) is queued for addition. A changed row therefore appears in both lists — delete then re-insert; there is no UPDATE. Input slices are walked in order, so identical inputs produce an identical statement sequence.
- Issues are compared through `issueRowValues` (row fields only) rather than the whole hydrated struct, because the hydrated view carries labels and (for containers) child-derived lifecycle — comparing those would make every label edit and child status change look like an issue-row change. Other tables compare the whole row; an event's `Changes` are part of its value.
- **Cascade rule**: the issues diff is computed first; each child table's "live" input is filtered to the post-cascade survivors (a relation cascades away if *either* endpoint is removed; comments/labels/events by their issue). This mirrors the schema fact that every child table is `ON DELETE CASCADE` from issues (and `issue_event_changes` from `issue_events`).
- Application order: issues, relations, comments, labels, events; per table, all removals before all additions. Deletion row counts are ignored.

## Merge rules (`internal/merge`)

The merge is pure — no IO, no clock, no error returns (every failure is a `bool`), fully deterministic, and symmetric: both machines compute the same winner without knowing which side is "ours". Causality comes from the merge-base, never from timestamps — `UpdatedAt`/`CreatedAt` are outputs, never used to pick winners.

### Export level (`ThreeWay`)

Issue IDs from base ∪ local ∪ remote, sorted ascending (a duplicate ID within one export lets the last row win). Per ID, with `changed` = differs from base (no base ⇒ both sides count as changed):

| Condition | Result |
|---|---|
| neither changed | keep base row (if it exists) |
| only one side changed | that side's row; if that side deleted it, the row is dropped |
| both changed, both present | field-wise `ResolveIssue` (below) |
| both changed, one present | the surviving edit is preserved (edit beats concurrent delete) |
| both changed, neither present | converged removal |

Deletion here is whole-row absence; soft deletion (retention) travels as a field on a present row. Change detection compares a projection of every persisted field including the whole leaf-lifecycle payload (status, closed_at, resolution, redirect_target — so a resolution-only re-close registers as a change) with nil and empty label slices normalized equal (JSON round-trip drift does not synthesize changes). `ThreeWay` panics on an unhydrated leaf issue.

Merged-export assembly: issues sorted by ID; `Version` = max of the three (empty ⇒ 1); `WorkspaceID` and `ExportedAt` are taken from local, remote's discarded. Side tables:

| Table | Merge | Collision rule | Referential filter |
|---|---|---|---|
| relations | two-way union by `(src, dst, type)` | remote's metadata wins an identical key | dropped unless **both** endpoints survive |
| comments | two-way union by ID | remote wins | issue must survive |
| events | two-way union by ID | remote wins | issue must survive |
| labels | **three-way** by `(issue_id, name)` | remote's metadata wins ties | issue must survive |

Labels are the only three-way side table: membership per key runs the two-tier rule over (in-base, in-local, in-remote) with presence-or at tier 2 — so a label removed by one side while the other left it alone **stays removed** (not resurrected), and the membership call treats the base as always present, making every present label an add when the base is empty. A key present only in base contributes nothing.

After the relation union: **single parent** — parent-child edges are keyed by child; a child with multiple candidate parents keeps the edge with the lexicographically greatest parent ID (order-independent). Then **cycle breaking**: parent chains are walked; on detecting a cycle, the edge belonging to the lexicographically greatest child ID inside the loop is deleted (that child becomes a root; nothing is reparented); exactly one edge is removed per cycle. Final sorts: relations by (src, dst, type); comments/events by ID; label rows by (issue, name).

### Field level (`ResolveIssue`)

The one primitive is `twoTier`: if exactly one side moved a field off the base, that side wins with no conflict (tier 1); if neither moved, ours (= base); if both moved, a tier-2 policy decides. With no base, every field goes straight to tier 2; every tier-2 policy is idempotent on equal inputs. Per field:

| Field | Tier-2 policy |
|---|---|
| issue_type, topic, lane, rank | symmetric workspace tiebreak |
| title, description, prompt | **prose pending** — never auto-picked |
| priority | numerically higher wins (urgent beats normal) |
| labels | per-name membership (presence-or; single-side removal sticks) |
| id | always ours; never merged |
| created_at | base's value; with no base, the earlier of the two |
| updated_at | always the later of the two; never two-tier |
| retention (archive/delete flags) | per-flag presence-or; the timestamp is derived — earliest non-nil — never merged on its own; deletion dominates on decode |
| status (leaves only) | dominant-state join: closed(2) > in_progress(1) > open(0) |
| assignee (leaves only) | workspace tiebreak |
| closed_at / resolution / redirect_target (leaves only) | see close payload below |

The **workspace tiebreak**: the value from the lexicographically greater workspace ID wins; if the workspace IDs are equal (defensive), the lexicographically greater value wins. Symmetric by construction.

The merged row is built by copying ours — except when the resolved type equals theirs' and differs from ours', in which case theirs is the basis; every field not explicitly re-merged is inherited verbatim from the basis side. A merged **container** (epic) inherits lifecycle, status, assignee, and close payload untouched from the basis; retention still merges for containers (it runs before the container gate).

**Status subtleties**: the dominant-state join operates on ranks, but tier 1 still applies — a lone-side reopen off a closed base wins (base closed, ours open, theirs closed → open). `closed_at`/resolution/redirect stay nil unless the merged state is closed; when closed, `closed_at` = earliest non-nil of the two sides.

**Close payload atomicity** (`resolveClosePayload`): resolution and redirect target travel as one atom — the winner's own payload is taken whole; one side's resolution is never mixed with the other side's target. Branch order: equal payloads → either; empty resolution loses to a real one; differing resolutions → tiebreak on the resolution, winner's whole payload; same resolution with one empty target → the real target; same resolution, two real targets → tiebreak on the target selects the whole payload.

**Prose**: there is **no text-diff machinery** — no hunks, no conflict markers, no similarity heuristics. A prose conflict is whole-field: the pending entry carries the three complete strings (base/ours/theirs) for one of `title`, `description`, or `agent_prompt`; if both sides moved to the *same* text there is no conflict. The provisional merged value is ours. The resolution is one complete replacement string per pending field, addressed by `(issue_id, field)` and fingerprinted: 12 hex chars of SHA-256 over field + base + ours + theirs joined by NULs (issue ID is not in the digest). `ApplyProseResolutions` rejects the whole set — returning the zero export, never a partial splice — if any resolution targets a non-pending key, carries a stale fingerprint, duplicates a key, or the set is not an exact bijection with the live pending set. On success it copies the issue slice (the input result is not mutated; other tables are shared by reference) and splices the texts in.

## Compaction (Dolt GC)

Two depths (`internal/storage/sync.go:207-244`): `newgen` (`CALL DOLT_GC()` — shallow, new generation only) and `full` (`CALL DOLT_GC('--full')` — rewrites the old generation). Any other depth is refused: `"compact: illegal depth %d (want %q or %q)"`.

**Footprint and thresholds** (`internal/store/compaction.go:65-147`): the store's footprint is the chunk-journal size plus the count of old-generation `.darc` archive files (an absent store reads as empty). The due rule, tested in order: ≥ **64** old-gen archives → full pass is due (each shallow pass appends one archive and removes none, so the count measures passes since the last deep collection); journal ≥ **16 MiB** → shallow pass is due (bounds the pass's stall below a second; arrives roughly every 350 mutations; caps journal waste at ~11 MB — Dolt's own auto-GC threshold is 128 MB); otherwise nothing is due.

Entry points: `compactWithinLock` (requires the held lock; runs the GC then a **mandatory** connection rotation, because online GC poisons the active connection); `SyncCompact` (validates depth, measures before/after outside the lock, runs under the envelope, reports the delta); `CompactIfDue` (the backstop gate — a measurement error is returned, explicitly not treated as "nothing due"; not-due returns the zero outcome with nil error); `chooseCompactionDepth` (the push path's selector — a measurement failure returns the shallow depth *and* the error: the push always compacts at least the new generation, since the footprint can only deepen, never cancel).

Reporting: the delta reads `"journal %s -> %s, old-generation archives %d -> %d"` (or `"footprint not measured: %v"`); a routine shallow pass says nothing, a full pass says `"compaction: ran full pass, rewriting the old generation"`, a pass that couldn't measure says so; reports are joined with `"; "`, empties dropped. A ran-but-unmeasured pass still reports the failure in `Detail`; `Detail` is empty only for the zero outcome.

**Remote-cache prune** (the other half of push maintenance, `internal/store/remotecache.go`): git-backed remotes keep mirror directories under `.dolt/<cache-dir>`, keyed by 64 lowercase hex chars = SHA-256 of the normalized underlying URL (query/fragment cleared) + `"|"` + the default ref; only `git+`-schemed remotes derive keys. A directory is abandoned when no configured remote derives its key. The legality rule: if there are abandoned directories **and** unaccounted-for configured remotes (remotes whose keys are missing on disk), the prune declines wholesale with an error naming both. Eligibility is re-checked per directory inside the deletion loop rather than trusted from the plan snapshot. The report names removals, reclaimed bytes, and/or the problem; silent when there's nothing to say.

## Schema migrations

Schema is versioned by [goose](https://github.com/pressly/goose) migrations embedded in the binary (`internal/store/migrations/`, embed glob `*.sql` only). Current registry: versions 1–5. The version table is `goose_db_version`; the baseline version constant is 1.

### The migration runner

Every store open runs `migrate` (`internal/store/migration_runner.go:275`): a snapshot guard is created; `runMigration` executes; on error, if a snapshot was taken the error is wrapped as `MigrationRollbackError` naming the snapshot path and the `lit snapshots restore <name>` command; on success, pre-migrate snapshots are pruned to 10 (a prune failure here **is** promoted).

**Phase classification** (read-only, `migration_runner.go:861`): the presence of the legacy `issue_history` table forces `phaseAdopt` **regardless of what `goose_db_version` claims** — that table is pre-goose-only, so its presence is conclusive, and this re-routes workspaces where a buggy older binary fabricated goose rows. Otherwise `goose_db_version` present → `phaseManaged` (with the recorded max version); a workspace with none of the baseline tables → `phaseFresh`; anything else → `phaseAdopt`. `willMutate` is true except for a managed workspace already at the registry max.

**`runMigration`'s exact order** (`migration_runner.go:308`):

1. Classify.
2. **Ahead-of-registry**: applied version > registry max → refuse via `refuseIfBaselineMissing`, which verifies the baseline shape (column names only) and refuses only when the baseline itself is broken; a workspace merely ahead with an intact baseline is tolerated read-compatibly. **No bookkeeping mutation happens on this path** — trimming the goose log would destroy true information. The refusal, `UnsupportedSchemaVersionError`, composes: the version pair; missing baseline shape if any; the lossy rollback path (`lit snapshots restore <name>`, "any data written under the newer binary will be discarded") when a pre-upgrade snapshot exists, or "no pre-upgrade snapshot available"; and the supported path `lit upgrade --to <producer-version>` when the workspace records the producer binary version.
3. **Version-content drift check** (managed phase only, independent of `willMutate`) — see below; a clean repair surfaces no error.
4. Not going to mutate → return (no snapshot, no writes).
5. Ensure the `migration_quarantine` table and commit it — before the snapshot guard, so it survives a later checkpoint reset.
6. **Quarantine fast-fail** — before the snapshot, so a permanently quarantined workspace does not accumulate one snapshot per open. For the adopt phase, the effective applied version is the baseline, so a baseline quarantine row cannot block post-adoption.
7. Adopt phase: verify the pre-goose workspace is reconcilable — also before the snapshot.
8. Take the snapshot (`guard.ensure`).
9. Adopt phase: run `reconcileToBaseline` (the idempotent, probe-driven pre-goose → v1 forward migrator; a historical artifact to which no new operations are added); verify the baseline shape (any remaining gap refuses: "the shape is structurally beyond what pre-goose reconcile can recover"); stamp adoption — create the goose table, insert the baseline version, delete the legacy `meta.schema_version` key; commit `"migrate: adopt pre-goose workspace at v1"`.
10. Apply pending migrations (below).
11. Record the producer binary version in `meta.producer_binary_version` and commit — **dev builds record no row**; the read side degrades any error to `""`.

**The goose loop** (`applyPendingMigrations`): construct the provider (before the checkpoint, so a construction failure leaves no orphan branch); create a `pre-migrate` Dolt checkpoint; then step one migration at a time. Each success is committed as `"migrate: v%d <filename>"`. `ErrNoNextVersion` ends the loop and prunes pre-migrate checkpoints to 5 (that prune failure is returned). Any other failure: **reset to the checkpoint first, quarantine second** — the working set is hard-reset to the checkpoint; then (when the failing version is known) a row is upserted into `migration_quarantine` and committed as `"migrate: quarantine v%d <name>"`; a nil-result failure (version 0) skips quarantine insertion. The returned `CheckpointResetError` names the version, checkpoint, and the recovery: fix the migration, then `DELETE FROM migration_quarantine WHERE version = <v>`. A reset failure escalates to "restore from dbsnapshot".

**Quarantine**: `migration_quarantine(version BIGINT PK, name TEXT, error_text TEXT, created_at VARCHAR(64))`. On open, any quarantine row with version > applied blocks with `QuarantineBlockError`, offering (a) `lit snapshots restore <name>` or (b) deleting the row if transient. The table self-heals: absent → created; exact canonical column set → ok; wrong shape and empty → dropped and recreated; wrong shape **with rows** → refuse ("this needs manual triage, not self-heal"). Set equality is exact — neither subset nor superset passes.

**Version-content drift** — the defense against a version number being reused for different content: for every registry migration above the baseline and at or below the applied version, the runner parses its `ADD COLUMN` statements (regex over the goose Up section; a literal-count gate fails loud if any `ADD COLUMN` occurrence goes unparsed, e.g. `IF NOT EXISTS` or multi-column forms; statement isolation is quote-aware for `''` doubling but not backslash escapes) and verifies each named column exists in the live schema. A miss produces `VersionContentMismatchError` naming the earliest mismatched version and its missing `table.column` targets ("this usually means the version number was reused for different historical content"). The repair executes **every** missing target's captured statement verbatim under its own `pre-drift-repair` checkpoint (retained at 5), committing `"migrate: repair version-content drift (…)"`; failure resets to the checkpoint. Documented scope limit: only `ADD COLUMN` is tracked — a version's `ADD CONSTRAINT` or data backfills (e.g. 00003's check constraint, 00004's redirect backfill) are never re-applied; `CREATE TABLE`, constraint-only, drop/rename, and data-only migrations are not verified.

**The transient schema lift** (`liftWorkingSetToRegistry`): loops goose `UpByOne` to the registry max, commits nothing, takes no checkpoint and no snapshot; a nil-result/nil-error iteration fails loud rather than spinning. Its only caller is the reconcile, on a throwaway scratch branch — this is what heals schema skew between diverged sides.

### The migration registry (v1–v5)

Every statement in every file is wrapped in goose `StatementBegin`/`StatementEnd` pairs; up sections carry **no** idempotency guards (bare `CREATE TABLE` / `ADD COLUMN`); only the baseline's down section uses `IF EXISTS`. Each migration ships a down section with a documented loss contract.

**v1 `00001_baseline.sql`** — byte-frozen: the file's SHA-256 is pinned in `TestBaselineFileIsFrozen`, and comment-only edits are forbidden too; structural change goes in a new numbered file. It creates seven tables:

- `meta(meta_key VARCHAR(191) PK, meta_value TEXT NOT NULL)`
- `issues(id VARCHAR(191) PK, title TEXT NN, description TEXT NN, agent_prompt TEXT NULL, status VARCHAR(32) NULL, priority INT NN, issue_type VARCHAR(32) NN, topic VARCHAR(191) NN, assignee TEXT NN, created_at/updated_at VARCHAR(64) NN, closed_at/archived_at/deleted_at VARCHAR(64) NULL, item_rank TEXT NN DEFAULT '')` with named checks: epics have NULL status and non-epics a status in `open/in_progress/closed` (`issues_status_check`); priority in 0–1; type in the five values.
- `relations(src_id, dst_id, type, created_at, created_by)` PK `(src,dst,type)`, both endpoints FK → issues `ON DELETE CASCADE`, type check in `blocks/parent-child/related-to`.
- `comments(id PK, issue_id FK cascade, body, created_at, created_by)`.
- `labels(issue_id, label)` PK pair, FK cascade.
- `issue_events(id PK, issue_id FK cascade, action VARCHAR(64) NULL, reason, actor, created_at)`.
- `issue_event_changes(event_id FK → issue_events cascade, field, from_value, to_value)` PK `(event_id, field)`.

Plus eight indexes: `issues(status,priority,updated_at)`, `issues(item_rank(191))` (prefix index), `relations(src_id,type)`, `relations(dst_id,type)`, `comments(issue_id,created_at)`, `labels(issue_id,label)`, `labels(label,issue_id)`, `issue_events(issue_id,created_at)`. No backfills. Down: seven `DROP TABLE IF EXISTS`, children first.

**v2 `00002_add_lane.sql`** — `issues.lane text NOT NULL DEFAULT ''`. Down drops it; loss contract: per-child lane assignments are gone, every child falls back to the fully-sequential default lane.

**v3 `00003_add_resolution.sql`** — `issues.resolution VARCHAR(32) NULL` plus check `resolution IS NULL OR IN ('duplicate','superseded','obsolete','wontfix')`. Down drops both; loss contract: close reasons are gone.

**v4 `00004_add_redirect_target.sql`** — `issues.redirect_target VARCHAR(191) NULL`, **no foreign key**, with a one-directional check (`redirect_target IS NULL OR resolution IN ('duplicate','superseded')` — a redirecting resolution with an unknown target stays representable). Two backfills: issues closed duplicate/superseded with **exactly one** incident `related-to` edge get that edge's counterpart as their target (any other edge count is left NULL, edges intact); then the `related-to` edges that exactly mirror a (issue, target) pair are deleted. Down re-materializes those edges (`INSERT IGNORE`, endpoints ordered LEAST/GREATEST, `created_at` approximated by `closed_at` falling back to `updated_at`, `created_by` = `'unknown'`), then drops constraint and column; loss contract: the redirect-vs-manual-peer distinction is lost, and an FK-gap redirect (whose canonical row was hard-deleted) silently fails to re-materialize.

**v5 `00005_add_event_attribution.sql`** — `issue_events.stream_id VARCHAR(64) NULL` and `issue_events.workspace_id VARCHAR(191) NULL`. No backfill, by design: attribution is historical fact, never backfilled, so a freshly upgraded repository derives zero claims; nothing user-, host-, or path-shaped may enter these columns. Down drops both; loss contract: the claim predicate reads unattributed history as zero claims.

**Registry invariants, test-enforced**: baseline bytes frozen by SHA-256 pin; every v2+ migration content-pinned by SHA-256 and filename (`detectVersionReuse` classifies content-changed / renamed / unpinned / deleted / duplicate, with content-change taking priority over rename and duplicates suppressing content checks); every migration must have a non-empty, non-comment down section (comment-aware parser with 15 pinned accept/reject shapes); filename version parsing is exact (`<digits>_<name>.sql`, rejecting signs, whitespace, leading-junk — 12 pinned cases); `MaxVersion` must agree with a fresh registry scan (no hard-coded literal) and be ≥ the baseline.

## Dolt checkpoints

Checkpoints are cheap intra-store recovery points implemented as Dolt branches named `<prefix>-<unix-ns>` (`internal/store/checkpoint.go`):

- **Create**: read the head SHA, `CALL DOLT_BRANCH(name)`; returns name, prefix, creation time, and anchor SHA.
- **Reset**: `CALL DOLT_RESET('--hard', name)` — discards working-set changes **and** any Dolt commits made after the checkpoint.
- **List**: branches matching `<prefix>-%`; names that fail the parse (exact prefix, suffix must round-trip through `%d` unchanged) are skipped silently; sorted oldest-first.
- **Prune**: negative retain is an error; deletes the oldest beyond the retention with `DOLT_BRANCH('-d','-f')`; retain 0 deletes all.

In use: `pre-migrate` and `pre-drift-repair`, both retained at 5.

## Recovery snapshots (`internal/dbsnapshot`)

Snapshots are full directory copies of the database dir, used as the coarse recovery mechanism around migrations, reconciles, takes, and downgrades. They live in a `snapshots` directory that is a **sibling** of the dolt dir (the CLI's user snapshots use `<storage>/snapshots` — same place).

**Naming**: `<unix-ns>` or `<unix-ns>-<label>`; labels are sanitized (only `[a-zA-Z0-9_-]` pass; every other byte becomes a dash; truncated to 128 bytes; edge dashes trimmed; may sanitize to empty). System snapshots stamp the label as `<label>-<unix-ns>`, so the full shape is `<unix-ns>-<prefix>-<unix-ns>`; three disjoint classifiers (`pre-migrate`, `pre-downgrade`, `pre-reconcile`) share that shape rule, so the three retention budgets never collect each other's snapshots — or the user's. Parsing is strict: the head must be positive digits that round-trip exactly (no signs, no leading zeros — a `+123.tmp` was once misclassified as lit residue and destroyed); a dashed label must be mintable-shaped (no length bound, tolerating pre-cap binaries); producer-artifact suffixes (`.tmp`, `.reserve`, `.condemned`) are rejected outright.

**`Take`**, in order: refuse on an already-canceled context (before Darwin's uncancelable clone syscall can run); stat the source (must be a directory); create the snapshots dir; **collect orphaned residue** (failures print to stderr, the take proceeds); acquire the **producer beacon** — a shared flock in the snapshots dir, 20 attempts × 50ms, contention wraps `ErrSnapshotsBusy` ("residue collection is holding the snapshots directory…"); reserve a slot — an atomic `os.Mkdir(<name>.reserve)` claim, bumping the candidate timestamp by 1ns on collision, up to 1024 attempts; clone the tree into `<name>.tmp`; rename to the final name. Failures remove the `.tmp`. The `Created` time equals the timestamp in the name. Documented-but-unenforced preconditions: `Take` on a live workspace requires the caller to hold the workspace shared lock, Dolt's journal lock, and the commit lock; `Restore` requires no open Dolt connection on the destination.

**Copy engine**: Darwin tries a single APFS `Clonefile` syscall for the whole tree (uncancelable; any error falls back to the walking copy); Linux walks with per-file `FICLONE` reflink falling back to a plain copy; other platforms walk with plain copies. The walk recreates directories (chmod'd to defeat umask), recreates symlinks without following them, refuses FIFOs/sockets/devices, checks cancellation per entry, and skips exactly one file: Dolt's contentless `*/.dolt/noms/LOCK` (Windows mandatory locking would fail the copy; Dolt recreates it; note Darwin's fast path may still carry it). Plain copies use `O_EXCL` destinations, copy in 32 MiB context-checked chunks that keep kernel fast paths live, propagate permission bits exactly (never ownership or timestamps), and treat the final `Close` error as part of the copy's outcome (NFS commit-on-close truncation).

**`List`**: missing dir → empty; skips non-directories and unparseable names; newest-first.

**`Restore`**: validate the name (single path component, canonical scheme); `Lstat` — a symlink is refused ("snapshot is not a directory"); a missing snapshot returns the bare `ErrSnapshotMissing`. The existing database dir is rotated aside to `<databaseDir>.pre-restore-<unix-ns>`, then the snapshot directory is **renamed into place** — after a restore, the snapshot no longer exists in the snapshots dir. On an install failure the rotated path is returned alongside the error so the caller can undo.

**`Prune`/`PruneMatching`**: keep must be > 0; matching snapshots beyond the newest `keep` are removed oldest-first, aborting on the first removal error; non-matching snapshots are never removed regardless of age.

**Residue collection**: a `Take` killed mid-flight leaves `.tmp`/`.reserve` corpses. The collector runs at the start of every take: it probes the producer beacon **exclusively, once, without sleeping** — contention means a live producer exists and the collector silently skips (that producer's own next take collects). Liveness is proven entirely by the beacon — no age thresholds, no PID files (caveat: a still-running pre-beacon binary reads as dead). Collection is two-phase: first every collectible entry is **condemned** (renamed to `<artifact>.<fresh-unix-ns>.condemned`, so retried collections never collide even on stamp reuse), then the beacon is released **before** the deletion pass (so a multi-gigabyte delete doesn't extend the window producers retry against), then corpses are `RemoveAll`'d with errors joined. The destruction predicate is deliberately narrow — **reject broadly, delete narrowly**: only `<canonical-name>.tmp`/`.reserve`, and only condemned forms of exactly those, are collectible; a foreign `backup.tmp`, a stampless `.condemned`, a signed head, or a dotted label is spared, because a suffix is never provenance over a directory lit does not own.

## Retention budgets at a glance

| Artifact | Prefix/label | Retained | Pruned by |
|---|---|---|---|
| Dolt checkpoint | `pre-migrate` | 5 | migration loop |
| Dolt checkpoint | `pre-drift-repair` | 5 | drift repair |
| dbsnapshot | `pre-migrate` | 10 | successful `migrate` |
| dbsnapshot | `pre-reconcile` | 10 | reconcile / take (post-advance, stderr on failure) |
| dbsnapshot | `pre-downgrade` | (own budget; see downgrade in `04-store-operations.md`) | downgrade |

## Support packages

Four small packages sit beside the sync machinery.

**`internal/syncfile`** — JSON export files on disk. `WriteAtomic` marshals a `model.Export` as two-space-indented JSON with a trailing newline, writes it to a `.links-sync-*.json` temp file in the destination directory, and renames it over the target, returning the lowercase-hex SHA-256 of the payload bytes (`internal/syncfile/syncfile.go:15-41`). `Read` parses a file back into an export (inheriting the v1→v2 history conversion in `Export.UnmarshalJSON`) and returns the same hash; `HashFile` hashes a file's bytes and returns `""` with no error for a missing file (`syncfile.go:56-65`). Because the hash covers the serialized bytes, any formatting change alters it.

**`internal/doltcli`** — shell-out wrapper for an external `dolt` binary, distinct from the embedded engine. `MinSupportedVersion` is `1.81.10` (`internal/doltcli/doltcli.go:14`). `ParseVersion` extracts the first `\d+\.\d+\.\d+` from its input; `RequireMinimumVersion` runs `dolt version` in a directory and refuses with `"dolt %s+ is required, found %s"` when the installed version is older. `Run` executes arbitrary `dolt <args>` with combined output; failures carry the trimmed output in the error (`doltcli.go:90-99`).

**`internal/engine`** — the one seam mapping an open mode to a store constructor; the only package above `internal/store` that names the Dolt engine's construction surface (`internal/engine/engine.go:1-19`). Three modes: `read-write` → `store.Open` (bootstraps an absent database, exclusive workspace lock), `read-only` → `store.OpenForRead` (requires an initialized database, shared lock), `sync` → `store.OpenSync` (handle held across network operations). An unknown mode — including the zero value — fails with `"invalid storage engine mode %q"` rather than defaulting to write access, and the return is the `storage.Store` interface with the concrete type erased (`engine.go:79-89`).

**`internal/backup`** — timestamped JSON export snapshots under `<storageDir>/backups/`, written through `syncfile.WriteAtomic`. Filenames are `time.Now().UTC().Format("20060102-150405.000000000") + ".json"` (`internal/backup/backup.go:31`). `List` returns snapshots newest-first by file mtime, treating a missing directory as empty; `Prune(keep)` refuses `keep <= 0` and deletes everything past the first `keep`; `Latest` returns the newest or `nil`. Ordering is mtime-based throughout, never parsed from the filename.
