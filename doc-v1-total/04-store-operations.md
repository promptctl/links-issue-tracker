# Dolt store: operations

Day-to-day reads and writes are only half of what the store does; the other half is moving data across its boundary and surviving damage. This chapter covers those operational surfaces, roughly in the order an operator meets them: getting data out (full export, and the delta engine that turns two exports into SQL); getting data in (full-replace restore, tree import, bulk import); checking and repairing what's there (Doctor and FixIntegrity); the disaster-recovery pipeline for a store the normal engine refuses to open (raw dump → shape mapping → candidate rebuild → verify → promote); schema downgrade; checkpoints; every lock the store mints and the order they nest; the git-remote mirror cache; and the vendored embedded-Dolt driver everything runs on. Chapter 03 covers the schema, shapemap internals, and core record operations these mechanisms presume.

## Export

`Store.Export` performs five reads and assembles one `model.Export` value (`internal/store/import_export.go:15-39`):

| # | Data | Query semantics | Order |
|---|---|---|---|
| 1 | issues | `ListIssues` with `Limit: 0` (uncapped), `IncludeArchived` and `IncludeDeleted` both true — no WHERE clause at all, so **archived and soft-deleted issues are exported** | `item_rank ASC, id ASC` (`store.go:1737-1740`) |
| 2 | relations | `SELECT ... FROM relations` | `created_at ASC` (no tiebreak) |
| 3 | comments | `SELECT ... FROM comments` | `created_at ASC` |
| 4 | labels | `SELECT ... FROM labels` | `issue_id ASC, label ASC` |
| 5 | events | `issue_events LEFT JOIN issue_event_changes` | events by `(created_at, id)`, each event's changes by `field ASC` (`store.go:1946-1961`) |

The envelope carries `version: 2` (literal), `workspace_id`, `exported_at` (wall-clock UTC at export time), and the five arrays. No key has `omitempty`; every list helper initializes a non-nil slice, so empty tables serialize as `[]`, never `null` (`import_export.go:39`; `store.go:1787`). The per-record wire shapes (issue, relation, comment, label, event, attribution) and the v1 `history` decode fallback are specified in `01-data-model.md`; two facts worth restating from the export side: a label's JSON key is `name` while its DB column is `label`, and an unattributed event omits its `attribution` key entirely.

Three surfaces write the export to disk or stdout:

- **`lit export`** — the JSON object to stdout with two-space indent and one trailing newline; export is JSON-only, with no text representation (`internal/cli/cli.go:1514-1525, 1800-1804`).
- **Backup snapshots** (`internal/backup/backup.go`) — directory `<StorageDir>/backups` (mode `0o755`), filename `20060102-150405.000000000.json` (UTC), written via `syncfile.WriteAtomic`. `List` reads the directory (no manifest/index file exists), skipping non-`.json` entries; a missing directory lists as empty without error. `Prune(dir, keep)` errors on `keep <= 0`; `lit backup create` defaults `--keep` to 20 and the restore path hardcodes a prune to 20 (`internal/cli/backup.go:34, 160`). `lit backup create` prints `<name> <path>`; `lit backup list` prints `<name> <size> <path>`.
- **Sync base** (`internal/syncfile/syncfile.go`) — `<StorageDir>/last-sync-base.json`, written atomically: `json.MarshalIndent` two-space **plus a trailing newline**, temp file `.links-sync-*.json` in the same directory, then `os.Rename`; the temp file is removed on any failure. `WriteAtomic` returns the content hash of the bytes written (`syncfile.go:20-39, 66-72`).

## The export delta

`diffExports(prev, next)` is a **pure value diff between two in-memory `model.Export` values** — not a commit range, checkpoint, or timestamp comparison; no SQL is issued to compute it (`internal/store/export_delta.go:141-142`). `prev` is never taken from a caller's belief: the restore path supplies the empty export (making everything an add), and the sync reconcile spine writer seeds its `landed` state from an actual `Store.Export` of the spine branch, advancing it only after a successful landing (`import_export.go:172-179`; `sync_reconcile.go:628-652`).

Mechanics (`export_delta.go:32-123`):

- Per table, the delta is `remove []key` + `add []row`. **There is no modify**: a changed row appears in both lists (delete-then-reinsert), which is why no UPDATE statement exists anywhere in this path. Keys are the schema primary keys — `(src_id, dst_id, type)` for relations, `(issue_id, label)` for labels, the `id` string for issues/comments/events (`row_deletes.go:35-45`).
- Comparison is `reflect.DeepEqual` over a persisted projection: issues compare their normalized row tuple (`issueRowValues`), the other four tables compare whole model values. Iteration is over input slices, not maps, so identical inputs produce an identical statement sequence (`export_delta.go:46-50, 78-107`).
- **Cascade**: the issues diff is computed first; each child table's live side is then filtered to surviving issues — a relation dies if *either* endpoint dies; comments, labels, and events die with their issue (`export_delta.go:145-167, 189-211`). Nested `issue_event_changes` get no layer of their own: an event whose changes differ is removed and re-added whole (`export_delta.go:137-139`).
- The delta is an unexported in-memory value — never serialized to disk or JSON (`export_delta.go:111-117`).

`applyExportDelta` runs the five tables in the fixed order **issues, relations, comments, labels, events**, and within each table all removes before all adds; removal row counts are discarded (`export_delta.go:217-258`). Issues go first so child inserts satisfy their foreign keys. The bound statements are single-row deletes by full primary key (§ Row deletes) and the same inserts the import path uses.

Where a delta is committed: `replayDeltaOnScratch` runs, under the commit lock, one `BeginTx` → `applyExportDelta` → `tx.Commit` → `commitWorkingSetOnce(stamp)`, deliberately as a **single attempt** with no transient retry — errors bubble to the outer scratch-rebuilding retry (`sync_reconcile.go:564-597`). The spine writer checks out the spine branch unconditionally before replaying (`sync_reconcile.go:645-652`). A `commitStamp` carries `Message`, optional `Date` (second-granular `--date`), optional `Author` (`--author "Name <email>"`), and `AllowEmpty` (`commit_lock.go:104-116`).

Tests pin, among other shapes: a delta of an export against itself is empty; changing a relation's `CreatedBy` (outside its key) yields exactly one remove + one add and zero issue work; retitling an issue re-inserts its relations/comments/labels/events (one add, zero removes each); adding one label touches only the labels table; a child closing moves the epic's hydrated value but not its row; removing an issue does not resurrect its children; and delta-application produces row-for-row the same tables as a full rewrite across every change shape (`export_delta_test.go:27-446`).

## Full-replace import

`ReplaceFromExport` → `replaceFromExport(ctx, export, commitStamp{Message: "replace from export"})` — the Dolt commit message for a restore is the literal `replace from export` (`import_export.go:138-140`). It runs under the commit lock, inside one SQL transaction, followed by one Dolt commit, with the transient-GC retry wrapping the whole unit — all-or-nothing at the SQL level (`import_export.go:149-153`; `commit_lock.go:156-177`).

`writeExportTx` clears tables in the literal order `labels, comments, relations, issues` (issue_events and issue_event_changes are deliberately not named — they cascade from issues), then applies `diffExports(empty, export)` (`import_export.go:168-179`).

**No ID remapping, no dedup, no conflict policy**: IDs are written verbatim; duplicate ids in the input reach the INSERT and fail on the primary key, aborting the transaction (`export_delta.go:73-77`).

Input normalization on the issue insert (`import_export.go:190-242`), values worth pinning:

| Column | Rule |
|---|---|
| `agent_prompt` | `""` → SQL NULL |
| `status` | leaf → status string; container → NULL |
| `priority` | `CanonicalPriority` — any int ≠ 1 coerces to 0, **never rejects**, so a legacy out-of-range priority cannot fail a restore |
| `topic` | slug-normalized, then `COALESCE(NULLIF(?, ''), 'misc')` — an empty result becomes the literal `misc` |
| `issue_type` | verbatim — no parse gate on this path |
| timestamps | RFC3339Nano strings |
| `archived_at`/`deleted_at` | projected from sealed retention; both-set is unrepresentable |

Events replay attribution **verbatim from the dump** — the restoring checkout never substitutes its own; `Attribution.UnmarshalJSON` has already collapsed half pairs (`import_export.go:270-281`). Unknown top-level keys in the export JSON are silently ignored (`syncfile.go:49`).

The CLI restore flow (`lit backup restore`, `internal/cli/backup.go:73-186`): usage is `lit backup restore (--latest | --path <export.json>) [--force]`; `--latest` and `--path` are mutually exclusive; `--latest` with no snapshots errors `no backups available`. The sequence: acquire the Sync and Import capabilities; read the restore file; export local state; if a sync state exists and `--force` was not passed, hash `last-sync-base.json` and compare against the hash of the local export — mismatch → `MergeConflictError` "restore conflict: local workspace has unsynced changes since last sync base"; take a pre-restore backup snapshot; prune to 20; `ReplaceFromExport`; re-export and atomically write the new sync base; record the sync state with the restore file's path and content hash. Note `hashExport` marshals with no trailing newline, unlike `syncfile.marshalExport` (`backup.go:188-193`).

## Doctor and FixIntegrity

`Doctor` (`import_export.go:42-112`) initializes `IntegrityCheck` to `"ok"` and runs, in order:

| Check | Mechanism | On hit |
|---|---|---|
| constraint violations | `CALL DOLT_VERIFY_CONSTRAINTS()` | `IntegrityCheck = "constraint_violations"`, **error** `constraint violations: %d` |
| FK orphans | three LEFT-JOIN counts (relations' endpoints, comments' issue, labels' issue), summed | **error** `foreign key violations: %d` |
| invalid related-to ordering | `COUNT(*) ... type='related-to' AND src_id >= dst_id` | **warning** `invalid related-to ordering rows: %d` |
| orphan event rows | events joined to missing issues | **warning** `orphan issue event rows: %d` |
| rank inversions | `liveRankInversions` (computed in Go) | **warning** `rank inversions: %d (dependencies ranked below dependents)` |
| blocks cycle | `liveBlocksCycle` | **warning** `blocks dependency cycle: <a -> b -> ...> (no rank order exists; remove one edge with 'lit dep rm' to break it)` |

`FixIntegrity` (`import_export.go:117-136`) runs under a mutation with Dolt commit message `fsck repair`, executing exactly three statements — delete orphan events, delete self-referential related-to rows, swap mis-ordered related-to endpoints — then returns a fresh `Doctor` report. It does not touch FK violations, rank inversions, or cycles.

## Tree import (`lit import`, JSON)

The input is one JSON array of spec objects, decoded with `DisallowUnknownFields` and a trailing-data check (`internal/storage/specs.go:50-61`). Each spec: `local_id` (required), `title` (required), `type` (required), `topic`, `priority`, `description`, `prompt`, `assignee`, `labels`, `parent`, `depends_on` (`internal/storage/bulk.go:53-65`). `parent` and every `depends_on` entry must reference a `local_id` **inside the file** — naming a pre-existing real issue id is rejected as a missing reference (`import_tree.go:134-154`). Forward references are legal (validation checks against the complete set). Rejections cover: empty input, missing/whitespace-padded local_id, missing title, invalid type/priority, duplicate local_id, whitespace-padded or unresolvable parent/depends_on, and self-dependency — each with a distinct `import: ...` message (`import_tree.go:104-156`).

Execution (`import_tree.go:26-86`): validate everything up front (no writes); topologically sort over `parent` + `depends_on` edges (three-state DFS; a cycle errors `cycle detected involving %q`; unresolvable references are simply not edges); create in topo order via ordinary `CreateIssue` calls with `Placement` left at `RankBottom` so creates land in file order; then a second pass **in file order** wires each `depends_on` as a `blocks` relation with `SrcID` = dependent, `DstID` = dependency, `CreatedBy: "links"`. `Lane` is never settable on this path. The result is an `id_map` from local ids to real ids; the CLI prints `imported %d issues` then one `local -> real` line per entry **in nondeterministic map order** (`cli.go:1588-1605`).

On any mid-batch failure, `rollbackCreatedIssues` best-effort **soft-deletes** (retention `delete`, actor `links`, reason `import rollback`) the issues created in this call; ids that fail to roll back are named in the error as `(rollback leaked %d: <ids>)`. Partial state may remain; the surviving surface is `lit doctor` (`import_tree.go:94-102, 18-22`).

CLI dispatch: `lit import --path <file>` routes on lowercased extension — `.yaml`/`.yml` → bulk, **anything else** (including `.json` and no extension) → tree JSON. On the JSON branch a set `--by` flag is a usage error: tree import always attributes creates to `"links"` (`cli.go:1537-1568`).

## Bulk import (`lit import`, YAML)

The input is multi-document YAML (`---`-separated), one document per issue, decoded with `KnownFields(true)` (`specs.go:25-40`). Pointer-typed fields (`title`, `description`, `prompt`, `type`, `topic`, `priority`, `assignee`, `labels`, `lane`) carry the patch distinction: absent = leave unchanged, present = write. `id` is the selector: present → **update patch** of an existing issue; absent → **create** (`import_bulk.go:191-223`).

Validation runs over the whole file before anything is written. Shared checks: non-empty input, no surrounding whitespace on `id`/`local_id`/`parent`/`depends_on`, no self-dependency, no duplicate `id` or `local_id`. Update documents additionally reject: `local_id` (create-only), `topic` (immutable), `parent` (use `lit parent set`), `depends_on` (use `lit dep add`), invalid type/priority, and a document with no updatable field set — `reason` alone does not count. Create documents require title, topic, and type; reject invalid type/priority; and reject `reason` (update-only) (`import_bulk.go:224-330`).

Execution (`import_bulk.go:21-119`): topo-sort creates over `parent`/`depends_on` local references; apply in topo order — updates via `Store.Apply` with the `--by` actor threaded in, creates via `CreateIssue` with `Placement` at `RankBottom` (file order preserved); then wire `depends_on` in file order as `blocks` relations (`CreatedBy: "links"`). A reference that matches no `local_id` **passes through unchanged** as a presumed real pre-existing id, validated downstream — unlike tree import (`import_bulk.go:127-132`). A create with no `local_id` is reported self-keyed (`real -> real`). `Topic` is unrepresentable in `UpdateIssueInput`, so bulk updates cannot carry it.

Differences from tree import, condensed: JSON array vs multi-document YAML; create-only vs create+update; `local_id` required vs optional; internal-only references vs pass-through externals; `lane` unsettable vs settable; `reason` absent vs update-only; actor always `links` vs `--by` on updates; result `id_map` vs `{created map, updated list}`.

**There is no batching**: every document is its own `withMutation` transaction and its own Dolt commit; no store-layer progress reporting exists — the CLI prints only after the whole call returns (`import_bulk.go:50, 72, 112`; `cli.go:1644-1660`). Partial failure is not transactional: rollback soft-deletes only this call's creates, never updates that already landed, which stay applied (`import_bulk.go:13-20`).

CLI: `--by` with a file containing no update document is a usage error. Output: `created %d issues` + per-entry `ref -> real` lines (map order, nondeterministic), then `updated %d issues` + ids in apply order.

## Verify (conservation gate)

`VerifyCandidate(ctx, dump, mapping, store)` checks that a rebuilt store conserves its source dump. It performs **no writes and no repairs** — only `Doctor` and `Export`, and it deliberately does not re-run mapping validation (`verify.go:110-138`). A Doctor or Export failure is a gate-cannot-run error; otherwise findings accumulate across four "conservation laws" with no early exit, and any finding at all makes `Reconciled()` false — there is no advisory tier inside the report (`verify.go:82-88, 132-137`).

| Law | Value | What it checks |
|---|---|---|
| `health` | `"health"` | Doctor **errors** become findings verbatim; Doctor **warnings are discarded**, so a faithful rebuild of messy source is not rejected (`verify.go:140-153`) |
| `count` | `"count"` | per collection (issues, relations, comments, labels, events, event_changes): rows the dump maps into the collection via unconditional (`Always`) emitters must equal the rebuild's count; any conditional emitter permanently excludes its collection from the law; message `collection %q: source dump carries %d row(s) mapped here, rebuild has %d` (`verify.go:179-222`) |
| `id_stability` | `"id_stability"` | the set of source cells mapping into `issues.id` (union across all tables/columns) vs rebuilt issue ids; two findings possible — missing and extra, each listing sorted ids; no `issues.id` mapping at all → no findings (`verify.go:233-267, 326-346`) |
| `rank_permutation` | `"rank_permutation"` | over rebuilt issues only: every non-empty rank must be well-formed base-62 and distinct; empty ranks are skipped as legal (`verify.go:277-314`) |

`VerifyReport.String()` renders `verify: reconciled — Doctor-clean and all conservation laws hold`, or a numbered finding list under `verify: %d discrepancy(ies) — the rebuild does not conserve the source and cannot be trusted:` (`verify.go:93-103`).

## Raw dump (`lit lifeboat dump`)

`DumpRaw` produces a JSON artifact — `{workspace_id, dolt_head, tables: [{name, columns, rows}]}` — of **every base table in the database** in ascending catalog-name order, including Dolt/goose bookkeeping tables; there is no include/exclude list (`rawdump.go:23-43, 142-148`). It is strictly read-only: three SELECT-family statements, nothing written, committed, reset, or checked out.

Sequence (`rawdump.go:61-123`): validate args; take the **shared** workspace lock; stat the dolt root (`os.ErrNotExist` → `repository not initialized with lit — run 'lit init' first`); refuse if an adopt-pending marker is present (post-lock, see Adopt); open a **read** connection **without running migrations** — which is what lets it read a workspace `store.Open` refuses; read the Dolt HEAD (mandatory, not best-effort — it is the promotion gate's provenance); list and dump each table, aborting on the first table error.

Cell rules: columns come from the live result set; each `[]byte` cell converts to `string`; SQL NULL scans to `nil` and serializes as JSON `null`, distinct from `""`; `Rows` is always non-nil so an empty table serializes as `[]` (`rawdump.go:175-206`). The CLI writes it to stdout, two-space indent, no flags; extra args are a usage error (`lifeboat.go:158-171`).

## Row deletes

Five hard single-row deletes by full primary key, used by the reconcile delta and a few CRUD paths (`row_deletes.go:83-105`): issues, relations, comments, labels, issue_events. Cascade is owned by the schema (`ON DELETE CASCADE`), not by code: deleting an issue takes its relations, comments, labels, events, and event changes; deleting an event takes its change rows. **No CRUD path hard-deletes an issue row** — ordinary deletion is the retention stamp; `deleteIssueTx`/`deleteEventTx` have only the reconcile delta as caller, deliberately (`row_deletes.go:55-61`). Errors render `delete <subject>: ...` with per-entity subject strings.

The rows-affected count exists for CRUD callers: `RemoveLabel` and `RemoveRelation` convert 0 rows into typed `NotFoundError`s; `DeleteComment` discards the count; the delta ignores it (`labels.go:51-57`; `relations.go:392-406`; `store.go:1189-1191`). Set-matching deletes (single-valued edge replacement, `ClearParent`, label replacement, the self-edge sweep, `writeExportTx`'s wholesale clear, `FixIntegrity`) are deliberately separate statements (`row_deletes.go:63-74`).

## Checkpoints

A checkpoint is **a Dolt branch created at the current HEAD** — not a tag, file, or commit of its own (`checkpoint.go:12-28`). Name: `<prefix>-<UnixNano>` (UTC, decimal). The `storage.Checkpoint` value carries `Name`, `Prefix`, `CreatedAt` (parsed back out of the name), and `Anchor` (the commit hash: HEAD at creation; the branch's current hash when listed).

- `CreateCheckpoint`: read HEAD, `CALL DOLT_BRANCH(?)`. No pre-existence check or duplicate retry — two creations in the same nanosecond would collide (`checkpoint.go:17-37`).
- `ResetToCheckpoint`: `CALL DOLT_RESET('--hard', ?)` — hard-resets the current branch to the checkpoint's commit, discarding the working set and any later commits; no checkout, no branch deletion (`checkpoint.go:45-50`).
- `ListCheckpoints`: `SELECT name, hash FROM dolt_branches WHERE name LIKE '<prefix>-%' ORDER BY name`; rows that fail the name parse are silently skipped; final order is **oldest first** (`checkpoint.go:54-81`).
- `PruneCheckpoints(prefix, retain)`: negative retain errors; deletes the oldest beyond `retain` via `CALL DOLT_BRANCH('-d', '-f', ?)`, aborting on the first failure; `retain = 0` deletes all. Retention is by count under a prefix only — no age expiry (`checkpoint.go:85-102`).
- Name parsing requires the exact prefix, a `-`, and a decimal suffix that round-trips (rejecting leading zeros, signs, trailing garbage) (`checkpoint.go:106-121`).

Consumers (`migration_runner.go:31-38, 553-608, 1206-1231`): the startup migration path checkpoints as `pre-migrate` before any mutation and prunes to 5 on success; on a migration failure it resets to the checkpoint first, then inserts and commits a quarantine record (`migrate: quarantine v<N> <name>`), with layered error text directing to dbsnapshot restore if reset or quarantine also fail. The version-content drift repair uses a separate `pre-drift-repair` prefix (also retained at 5) so the two sets prune independently.

## Adopt (clone a remote backlog)

`AdoptRemoteByClone` bootstraps the local store by **cloning the remote Dolt database wholesale** into the dolt root — not a fetch, and not an in-place adoption (`adopt.go:208-220`). Any pre-existing database directory (an empty bootstrap store, or an interrupted adopt's residue) is **set aside by rename, never deleted**, to a sibling `<root>.adopt-displaced-<UnixNano>` directory (`adopt.go:228-234, 302`).

The crash-safety mechanism is the **adopt-pending marker**, `.links-adopt-pending`, written *inside* the dolt root before the first destructive act (`adopt.go:27-35, 283-285`). Its JSON payload (`started_at`, `remote`, `branch`) is informational; **presence alone condemns** — garbage content still refuses (`adopt.go:38-44`). It is written atomically (temp file → write → fsync → close → rename, with a best-effort directory fsync whose error is deliberately ignored for Windows) (`adopt.go:62-98`).

While the marker exists, every normal open path refuses — `Open`, `OpenForRead`, `EnsureDatabase`, `OpenSync`, `DumpRaw` — with a message explaining the interrupted adopt and directing to re-run `lit init` (or delete the root to abandon) (`adopt.go:124-145`; enforcement sites `store.go:134, 202, 292`; `sync.go:53`; `rawdump.go:90`). The check runs **after** the workspace lock is taken: a live adopt holds the exclusive lock, so marker-with-acquirable-lock always means a *dead* adopt (`store.go:125-133`).

The full sequence (`adopt.go:244-352`): validate root/workspace-id/remote-name/url/branch (all required); mkdir the root; take the exclusive workspace lock; write the marker; evict Dolt's singleton chunk-store cache entries for the db dir; rename any existing db dir aside; `CALL DOLT_CLONE('--remote', <name>, '--branch', <branch>, <url>, 'links')` (the git-backed remote defaults to the `refs/dolt/data` ref lit's sync push writes). On clone failure the partial clone is **deleted** (it is this run's own product, not user data) and the marker cleared; a clone that "succeeds" but produces no database directory leaves the marker in place; clearing the marker is the last act, and a clear failure returns an error explaining the store stays refused until a retried init completes. Postcondition: success ⇒ complete clone, no marker; failure ⇒ no partial database at the canonical path, or the durable marker still refusing it (`adopt.go:236-243`).

`LocalHasTickets` observes without ever creating the store: marker present → `(false, nil)` without opening; no database dir → `(false, nil)`; otherwise open-for-read and count (`adopt.go:167-203`). Re-running adopt over a successful prior adopt displaces and re-clones; tests pin that displacement preserves bytes and that a failed clone leaves zero residue.

## Candidate (disposable rebuild)

A `Candidate` is one disposable, fully isolated rebuild of a workspace: a fresh Dolt directory at the current schema baseline, loaded from the export a validated (dump, mapping) pair produced (`candidate.go:11-14, 30-42`). It owns a whole directory **tree** — `lit-candidate-<random>` under the canonical dolt dir's parent — with the Dolt workspace nested at `<root>/workspace`, so the workspace lock and migration snapshots `Open` writes as siblings land inside the owned tree and one `RemoveAll` is total (`candidate.go:25-29, 85, 108`).

`RebuildCandidate` order (`candidate.go:74-118`): `Apply(dump, mapping)` runs **first and purely**, so an invalid mapping is rejected before any directory or handle exists, tagged with the sentinel `ErrInvalidMapping` (`mapping is not applicable to the dump`) that the recovery loop routes as repair feedback; then MkdirTemp, `Open` the nested workspace, `ReplaceFromExport` (the only commit any recovery pass makes — inside the throwaway candidate, never the canonical workspace); stamp `expectedHead` (the dump's Dolt HEAD) and `workspaceID` for promotion's lost-update check. Any failure after directory creation triggers a deferred close-and-`RemoveAll`.

`detachForPromotion` closes the candidate's store (an open handle blocks a directory rename on Windows) and returns the dolt dir; `root` remains owned so a later `Discard` removes the scratch siblings. `Discard` is idempotent, releases store and directory against independent fields, and leaves `root` set on a removal failure so a retry can succeed (`candidate.go:137-179`). There is no enumeration or GC of candidate directories; the guarantee is zero residue per attempt.

## Recover (the lifeboat loop)

`Recover(ctx, canonicalDoltDir, dump, mapper, maxAttempts)` drives up to `maxAttempts` passes of mapper → rebuild → verify (`recover.go:108-140`). A `Mapper` is `func(dump, feedback) (ShapeMapping, error)`; `DeterministicMapper` ignores feedback and delegates to the built-in shape recognizer, erroring with a message directing to the LLM mapping path when the shape is unrecognized (`recover.go:27-40`). The budget must be ≥ 1; candidates are staged as siblings of the canonical dolt dir so promotion is a same-filesystem rename. `Recover` itself never touches the canonical workspace.

One pass (`recover.go:152-180`): mapper error → **feedback**, not failure (`the mapper could not propose a mapping: ...`); rebuild error tagged `ErrInvalidMapping` → feedback (`the proposed mapping was rejected by the applier: ...`), any other rebuild error → hard abort; verify-gate error → hard abort with the candidate discarded; not reconciled → discard the candidate, and the rendered verify report becomes the next pass's feedback; reconciled → classify. The only state carried between passes is the feedback string; the dump is read-only throughout.

Three sealed outcomes (`recover.go:52-94`): `Reconciled{Candidate, Mapping}`; `RequiresDrop{Candidate, Mapping, Drops}` when the mapping discards source columns with `DropUnexplained` provenance (drops sorted by column); `Unconverged{Residual, Attempts}` on budget exhaustion. The caller owns and must discard the candidate in the first two.

CLI (`lit lifeboat recover [--mapping <file>]`, `lifeboat.go:38-152`): sequence is `HealWorkspace` → `DumpRaw` → `Recover` with a budget of **1** (`recoverAttempts = 1` — the deterministic mapper cannot self-repair). `Reconciled` promotes and prints `recovered: rebuilt workspace promoted to <path> (previous contents preserved at <backup> | no previous contents to preserve)`. `RequiresDrop` discards and errors: `recovery needs a human decision: the mapping discards %d source column(s) with no recorded justification: ... nothing was changed; supply a mapping that maps or intentionally drops these before recovering`. `Unconverged` errors `recovery did not converge after %d attempt(s); nothing was changed:` plus the residual. A supplied `--mapping` file is JSON-decoded into a `ShapeMapping` and wrapped as a constant mapper.

## Promote (install the rebuild)

`PromoteCandidate` installs the candidate's Dolt directory **at the canonical path, in place** — never a wipe, never a repoint; every step is an atomic rename (`promote.go:14-19, 44-119`). Order: validate the canonical path; detach the candidate (close before any rename); take the exclusive workspace lock (its file is a sibling of the dolt dir, so it is held continuously while the guarded directory is briefly absent); `healCanonical` first (recover any prior crash so this swap starts from "canonical present"); run the **lost-update gate**; install a roll-*back*-never-forward defer; rename the canonical dir to `<canonical>.backup-<19-digit zero-padded UnixNano>` (probing forward on collision under the held lock); rename the candidate's dolt dir into place. The result carries the canonical path and the backup path (`""` when nothing pre-existed). The backup is persisted, never pruned — "the pre-recovery copy is the most precious artifact in the flow". There is **no fsync** in promote; crash-safety rests on rename atomicity, and the only interrupted-at-rest state is "canonical absent, backup present".

The lost-update gate `verifyHeadUnchanged` (`promote.go:148-175`): a dump with no recorded head fails with `ErrMissingDumpProvenance` (re-dump and recover from that artifact); otherwise the live HEAD is re-read under the held lock, and a mismatch fails with `ErrWorkspaceAdvanced` (`the candidate was rebuilt from %s but the live workspace is now at %s; a concurrent commit landed during recovery — nothing was changed, re-run recovery against the current state`).

`HealWorkspace`/`healCanonical` (`promote.go:188-263`): if the canonical dir is present, no-op; if absent, restore the **newest** backup by rename (the restore consumes the backup). Backup discovery scans the parent directory (scan, not glob, so metacharacters in paths cannot skip a real backup) for directories whose name is the exact prefix plus a 19-digit numeric stamp — stray files and hand-named directories are ignored (`promote.go:300-342`).

## Downgrade (schema rollback)

`Downgrade(target)` reverses goose migrations to the target schema version, one Dolt commit per reversed step, preceded by a recovery snapshot; only `lit downgrade` reaches it (`downgrade.go:149-152`; caller `cli/downgrade.go:95` with the target binary's manifest `Schema.Max`). The whole pipeline runs under the commit lock (re-entrant, so the per-step commits short-circuit); no workspace lock is taken.

Refusals, none of which take a snapshot (`downgrade.go:190-208`): not goose-managed (no `goose_db_version` table) → plain error; `target == current` → no-op nil; `target > current` → `DowngradeTargetAheadError` (directs to `lit upgrade`); `target < baseline (v1)` → `DowngradeBelowBaselineError` (going below would destroy the workspace; directs to `lit snapshots restore`), refused before goose is ever invoked.

Otherwise: take a `lit-downgrade-<UnixNano>` snapshot into the same `snapshots/` directory migrations use (label classification is disjoint from `pre-migrate` so each producer's retention of **10** governs only its own); loop — re-read the recorded version each iteration, `provider.Down()` one step, commit with `downgrade: revert v<N> <file>` — until at or below target. Any loop error wraps into `DowngradeRollbackError`, whose message names the snapshot path and the exact `lit snapshots restore <name>` command. Goose running out of reversible migrations while still above target is `DowngradeIncompleteError`. What downgrade modifies: `goose_db_version` rows, each migration's Down DDL/DML, one Dolt commit per step, one new snapshot plus pruning (`downgrade.go:244-295`).

## Locking

All lit-minted locks go through one primitive, `filelock.Acquire` (vendored `github.com/promptctl/primitives/filelock`): a **zero-byte file** locked via `flock` (POSIX) or `LockFileEx` (Windows). No PID, timestamp, or holder metadata is ever written, and **there is no stale-lock handling by design** — the hold lives on the open file description, so any process death (SIGKILL included) releases it in the kernel (`filelock/filelock.go:7-11, 53-130`). Contention is a value, not an error; a pre-cancelled context refuses before any attempt; the sleep is skipped after the final attempt; `maxAttempts == 1` is a non-blocking probe. The store's single stamping boundary converts not-acquired into the sentinel `ErrWorkspaceBusy` (`workspace_lock.go:53, 313`).

The lock inventory — every path is a sibling of the dolt directory unless noted (the "ONE HOME" rule, so a restore that rotates the dolt directory cannot move locks out from under acquirers; exceptions: the snapshot producer beacon inside `snapshots/`, the adopt marker inside the dolt root, and Dolt's own journal LOCK) (`doc.go:100-111`):

| Lock | File | Mode | Budget | Held by / meaning |
|---|---|---|---|---|
| workspace (shared) | `.links-workspace.lock` | shared | 100 × 50ms ≈ 5s | directory readers: store opens, raw dumps, snapshot walks |
| workspace (exclusive) | same file | exclusive | 1 attempt, no wait | directory rotators: snapshots restore, adopt, promote — refuses immediately on contention |
| commit | `.links-commit-flock.lock` | exclusive | 9000 × 100ms ≈ 15min | every mutation and Dolt commit; sized for `takeUserSnapshot` holding across a full snapshot copy |
| sync-push | `.links-sync-push.lock` | exclusive | 1 attempt (non-blocking probe, `context.Background()`) | single-flight: not-acquired means another mirror is pushing and the caller coalesces |
| mirror beacon | `.links-sync-mirror.lock` | shared to hold; probed shared-then-exclusive | hold: 20 × 50ms ≈ 1s | liveness beacon: probe verdicts `unheld` / `answered` (a shared holder exists) / `obstructed` (an exclusive foreign holder) |
| Dolt journal | `<db>/links/.dolt/noms/LOCK` | exclusive | 300 × 100ms ≈ 30s | Dolt's own file; `LockDoltJournalExclusive` stats first (absent → `repository not initialized with lit`) and never mints Dolt's tree |

Declared acquisition order, outermost to innermost: **workspace → Dolt's journal LOCK → commit → snapshot producer beacon**; a holder of an inner lock never waits on an outer one. The sync-push lock sits outside the order (every acquisition is a non-blocking probe) and the mirror beacon is always acquired holding nothing (`doc.go:41-98`). One tolerated deviation: the transient-GC retry rotates the store connection mid-mutation, re-acquiring Dolt's LOCK under the held commit lock, bounded ~30s inside the 15-minute commit-lock budget. A retired second lock name, `.links-engine.lock`, is deliberately never reused; likewise the historical commit-lock name `.links-commit.lock` is burned — old binaries `os.Remove` it, and unlinking under a live flock splits the lock across two inodes (`doc.go:73-78`; `commit_lock.go:396-402`).

Exact contention messages: shared workspace — `a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes`; exclusive — `another lit process is using this workspace; close other lit commands and retry`; commit — `another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes`; journal — `another process is holding this workspace's Dolt store open (a background sync mirror or another lit command still running); retry`. All wrap `ErrWorkspaceBusy` except the beacon-hold failure, which deliberately does not propagate the sentinel (`workspace_lock.go:86, 121, 186-196, 411`; `commit_lock.go:430`).

### Mutation sequencing and commit rendering

`withStampedMutation` runs, under one held commit lock inside the transient-GC retry: `BeginTx` → `fn` → `tx.Commit` → `commitWorkingSetOnce(stamp)`. A `staged` flag makes the retry resume **at versioning** once the SQL transaction has committed — `fn` never re-runs (`commit_lock.go:122-177`). The commit lock is re-entrant via a context key, so nested commits inside a held mutation never queue behind their own hold (`commit_lock.go:92, 357-365`). `commitWorkingSetOnce` renders `DOLT_COMMIT` args in the fixed order `-Am <message> [--allow-empty] [--date <RFC3339 UTC>] [--author "Name <email>"]`; an empty message defaults to the literal `links mutation`; an error containing "nothing to commit" is success with no commit (`commit_lock.go:286-319`). Release settlement: a release failure after a successful operation prints to stderr (`lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): ...`) and returns nil — a durable success is never retroactively failed (`commit_lock.go:346-353`).

The transient-GC retry (`commit_lock.go:34-59, 185-264`): up to 30 attempts, exponential delay 50ms doubling to a 1s cap (≈25s total), rotating the store connection between attempts. Exactly two error shapes classify as transient: text containing both `cannot update manifest` and `read only`, or both `online garbage collection` and `reconnect` (the GC phrase is required so an unrelated cluster-role "please reconnect" is not misclassified). On exhaustion, a persistent manifest-read-only error is promoted to `WorkspaceWriteBlockedError`: `another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)`.

One documented gap in the journal hold: `journal.idx` is opened read-write and truncated on every engine bootstrap with no can-write gate, so a snapshot copy can capture a torn index; Dolt's recovery truncates and rebuilds it from the journal on next open (`workspace_lock.go:384-388`).

## Remote cache prune

Dolt gives every **git-backed** remote its own bare-repo mirror at `<db>/links/.dolt/git-remote-cache/<sha256(url|ref)>/repo.git` and never deletes one; lit prunes mirrors abandoned by remote reconfiguration (`remotecache.go:21-31`). Key derivation: trim, parse the URL, and only schemes prefixed `git+` are cacheable (non-`git+` remotes yield no key, not an error; an unparseable URL is a failure); strip the prefix, clear query/fragment, and hash `url|refs/dolt/data` — lit never supplies a custom ref (`remotecache.go:41-90`). A directory name counts as a cache key only if it is exactly 64 lowercase hex chars; anything else was not written by Dolt and is never deleted (`remotecache.go:99`).

The plan: a directory is abandoned when no configured remote derives its key. **Refusal**: if abandoned directories exist *and* some configured git-backed remote has no directory on disk, nothing is deleted — the two facts wear one shape (a wrong key derivation vs a remote simply never opened), and the long refusal message directs one `lit sync push/fetch --remote <name>` through each unmatched remote to settle it (`remotecache.go:146-182`). Execution runs **without the commit lock**: each abandoned key is re-checked against the live remote list (skip if it came back to life), then measured and removed; every entry is attempted even after failures, so one unremovable directory is not a head-of-line blocker (`remotecache.go:352-403, 415-465`). The outcome reports removed count, reclaimed bytes (rendered `B`/`KiB`/`MiB`/... , shared with compaction's formatter), and joined problems; the report string is empty exactly when there was nothing to do (`remotecache.go:198-248`).

## The vendored Dolt driver

`internal/vendor/dolthub-driver` is a vendored `github.com/dolthub/driver` — a `database/sql` driver over an **embedded** Dolt engine (no server process), registered as `"dolt"`; only `OpenConnector` works (`Open` always errors). Its `go.mod` pins the promptctl forks of `dolthub/dolt` and `go-mysql-server` (`driver.go:15-53, 129-133`).

DSN grammar: `file://<directory>?commitname=...&commitemail=...[&database=...][&multistatements=true][&clientfoundrows=true]` — param names lowercase, `commitname`/`commitemail` required and single-valued, directory must exist and be a directory (`data_source.go:36-69`; `parse_dsn.go:27-73`). Two presence-based flags pass through to the DB-load layer: `disable_singleton_cache`, `fail_on_journal_lock_timeout`. `Config.BackOff` non-nil enables retrying engine open on exactly two retryable shapes — `nbs.ErrDatabaseLocked` and `os.ErrDeadlineExceeded` — and **implies both flags**; nil means one attempt (`config.go:56-65`; `retryable_open_err.go:24-33`). The connector holds a single shared engine, serializing concurrent opens on a channel; commit author identity comes from `commitname`/`commitemail` (`connector.go:103-280`).

Local modifications versus upstream, all behavior-affecting:

1. **Telemetry deleted outright** — upstream dialed `eventsapi.dolthub.com` on every engine open; the emission path, opt-out, and rate-limit file are removed, not defaulted off.
2. **Process-wide `engineConstructMu`** around `engine.NewSqlEngine`, because engine construction rewrites go-mysql-server globals and races even across unrelated database paths.
3. **Original MIT `MySQLError`** (`Number uint16`, `Message`; no SQL-state field) replacing the MPL-2.0 `go-sql-driver/mysql` type in the SBOM.
4. **Retryable-open plumbing** — the `BackOff`/flag knobs and `DBLoadParams` mapping above.
5. **Forced eager DB load** in `openSqlEngine`, so a lock error surfaces as the retryable error instead of a nil-pointer panic inside `NewSqlEngine`.
6. **Relative-path fix** — the filesystem is already rooted at the directory, so `"."` is passed; re-applying the directory would double the path.
7. **Peek errors carried, not dropped** — a non-EOF peek error is surfaced from `Next()` rather than re-driving the iterator, which could silently convert a real error into an empty result set.
8. Package-var test seams (nil in production), and result-construction error precedence (iteration error wins over close failure).

Query behavior worth pinning: transactions accept only `LevelSerializable`/`LevelDefault` and run literal `BEGIN;`/`COMMIT;`/`ROLLBACK;`; positional args become named bindings `v1, v2, ...`; `Query` peeks eagerly because some DML executes inside the iterator; multi-statement support splits with the gms parser (a standalone quote/paren-aware `QuerySplitter` also exists but is unused by the conn); enum/set columns convert to their string forms; `Exec` over multiple statements stops at the first error and returns the last result (`conn.go:41-118`; `statement.go:53-210`; `rows.go:32-200`).

Separately, lit suppresses Dolt's human progress output: a package `init` sets `doltcli.CliOut = io.Discard`, because the clone/fetch progress redraw would land on stdout — lit's parseable result channel. Dolt's error channel is left untouched (`dolt_output.go:9-33`).

## The capability contract

Compile-time assertions bind the Dolt store to the full storage contract: `storage.Store` plus all seven optional capabilities — `Syncer`, `Reconciler`, `Checkpointer`, `Repairer`, `SchemaMigrator`, `Importer`, `RawExecutor` (`contract.go:38-48`; the contract itself is chapter 02). The package exports no storage vocabulary aliases — the engine is reached through the contract or not at all. What remains exported beyond the `Store` methods is workspace machinery addressed by filesystem path rather than engine handle: the workspace and commit flocks, the mirror beacons, bootstrap and remote adoption, snapshot naming, and lifeboat recovery (`contract.go:13-32`).
