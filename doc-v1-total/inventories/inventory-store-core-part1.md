# Behavioral Inventory — `internal/store` core (Dolt-backed store)

Repository: `/Users/bmf/code/links-issue-tracker` (the `lit` issue tracker CLI).
Branch at time of review: `links-storage-bd8w.2-compaction-backstop`, HEAD `ddc0673`.

## Scope and method

This document is a raw behavioral inventory of the **core** of `internal/store` — the Dolt-backed storage engine — plus the vendored Dolt SQL driver at `internal/vendor/dolthub-driver`. It is derived **entirely from Go source and `_test.go` files** (plus `.sql` DDL and the vendored `filelock` primitive in the module cache). No Markdown, no `docs/`, no `design-docs/`, no README, CHANGELOG, or CONTRIBUTING content was read or used.

Every claim carries a `file:line` citation. Line numbers refer to the files as of the commit above.

### Files covered

| Area | Files |
|---|---|
| Engine core & contract | `store.go`, `contract.go`, `doc.go`, `dolt_output.go` |
| SQL schema | `schema_reconcile.go`, `schema_snapshot.sql`, DDL under `migrations/` |
| Shapemap | `shapemap.go`, `shapemap_json.go`, `shapemap_known.go` |
| Domain persistence | `issue_ids.go`, `labels.go`, `relations.go`, `ranking.go` |
| Import / export | `import_export.go`, `import_bulk.go`, `import_tree.go`, `export_delta.go` |
| Integrity & dumps | `verify.go`, `recover.go`, `rawdump.go`, `row_deletes.go`, `checkpoint.go` |
| Workspace bootstrap | `adopt.go`, `candidate.go`, `promote.go`, `downgrade.go` |
| Coordination & cache | `workspace_lock.go`, `commit_lock.go`, `remotecache.go` |
| Vendored driver | `internal/vendor/dolthub-driver/*.go` |

### Explicitly out of scope

`sync*.go`, `compaction.go`, `migrations/` runner mechanics, `migrate_snapshot.go`, `migration_runner.go` — covered separately. Where a covered file calls into one of these, the call site is noted and the trail stops there.

---


---

## `internal/store/store.go` — raw behavioral inventory

All citations are `path:line`. Paths are relative to `/Users/bmf/code/links-issue-tracker`.
Where `store.go` depends on a symbol another file owns, the definition site is cited and the
behavior is stated only as far as `store.go`'s contract requires. `commit_lock.go` is cited in
full for commit semantics because every `store.go` mutation routes through it.

---

### 1. Package-level constants, variables, and types

#### 1.1 `doltDatabaseName`

`const doltDatabaseName = "links"` (`internal/store/store.go:28`). Used as:
- the `Database` field of the embedded-driver config for every non-bootstrap pool (`store.go:382`, `store.go:427`, `store.go:2535`);
- the `CREATE DATABASE IF NOT EXISTS links` argument (`store.go:2525`);
- the on-disk directory probed for existence: `<doltRoot>/links/.dolt` (`store.go:2507`).

Tests derive the journal-lock path from it as `<doltRoot>/links/.dolt/noms/LOCK` (`internal/store/engine_open_contract_test.go:20-22`) and the chunk journal as `<doltRoot>/links/.dolt/noms/<chunks.JournalFileID>` (`internal/store/dolt_journal_hold_test.go:44-46`).

#### 1.2 `engineAccess`

```go
type engineAccess int
const (
    engineRead engineAccess = iota
    engineWrite
)
```
(`store.go:37`, `store.go:39-42`). Two values only. Semantics:
- `engineWrite`: connector is given a `BackOff` (`store.go:660-662`), and `openStoreConnection` pings eagerly (`store.go:386-390`).
- `engineRead`: no `BackOff`, no eager ping — the engine opens lazily at the first SQL statement (`store.go:381-399`, doc at `store.go:372-380`).

#### 1.3 `Store` struct — every field

`store.go:44-82`:

| Field | Type | Set where | Meaning per code |
|---|---|---|---|
| `db` | `*sql.DB` | `store.go:392`; replaced by `reconnect` at `store.go:432` | the one pooled embedded-Dolt connection |
| `workspaceID` | `string` | `store.go:393` | the workspace id passed to `Open`/`OpenForRead`; also the Dolt commit author basis (`store.go:2648-2656`) |
| `doltRootDir` | `string` | `store.go:394` (raw arg, not cleaned) | Dolt root dir |
| `access` | `engineAccess` | `store.go:395` | reused verbatim by `reconnect` (`store.go:427`) |
| `commitLockPath` | `string` | `store.go:396` = `commitLockPathForDolt(doltRootDir)` | flock path, `filepath.Join(filepath.Dir(filepath.Clean(doltRootDir)), ".links-commit-flock.lock")` (`commit_lock.go:394-403`) |
| `telemetryDir` | `string` | `store.go:397` = `filepath.Join(filepath.Clean(doltRootDir), "telemetry")` | never read inside `store.go` |
| `releaseWorkspaceLock` | `func() error` | `store.go:144` (Open), `store.go:209` (OpenForRead); cleared to `nil` on failure at `store.go:160`, `store.go:230`, and in `Close` at `store.go:354` | the workspace shared-lock release |
| `attribution` | `model.Attribution` | only by `AttributeTo` (`store.go:261`) | stamped on every `recordEvent` row (`store.go:1820`) |
| `applyPreMutationHookForTest` | `func()` | nil in production; fired at `store.go:1111-1113` | test seam between planning and `withMutation` in `Apply` |
| `commitWorkingSetHookForTest` | `func() error` | nil in production; fired at `commit_lock.go:287-291` | test seam at the top of every `commitWorkingSetOnce` |

Both hooks are per-`Store` instance state, not package globals (`store.go:66-81`).

#### 1.4 `engineOpenRetryMaxElapsed`

`var engineOpenRetryMaxElapsed = 30 * time.Second` (`store.go:2607`). A package **variable**, not a const, so tests can shrink it; `engine_open_contract_test.go:53-55` sets it to `700 * time.Millisecond` and restores it in cleanup. It is `MaxElapsedTime` of the write-open backoff (`store.go:2633`).

#### 1.5 `newEngineOpenBackOff`

`store.go:2629-2635`. Fresh `backoff.NewExponentialBackOff()` per connector with:
- `InitialInterval = 50 * time.Millisecond` (`store.go:2631`)
- `MaxInterval = time.Second` (`store.go:2632`)
- `MaxElapsedTime = engineOpenRetryMaxElapsed` (`store.go:2633`)

All other `ExponentialBackOff` fields keep library defaults. Only attached for `engineWrite` (`store.go:2660-2662`).

#### 1.6 `wrapEngineOpenContention`

`store.go:2619-2624`. If `err != nil && errors.Is(err, nbs.ErrDatabaseLocked)`, returns exactly:

```
fmt.Errorf("another process is holding this workspace's Dolt store open (a background sync mirror, another lit command, or a snapshot copy in progress); retry after it completes: %w (%w)", ErrWorkspaceBusy, err)
```

so the result satisfies both `errors.Is(err, ErrWorkspaceBusy)` and `errors.Is(err, nbs.ErrDatabaseLocked)`. Every other error passes through unchanged. `ErrWorkspaceBusy` is defined at `internal/store/workspace_lock.go:53` as `errors.New("workspace busy")`.

Call sites: `store.go:388` (eager write ping), `store.go:437` (reconnect ping), `store.go:153` (`ensureMasterDefaultBranch` inside Open), `store.go:2533` (bootstrap CREATE DATABASE), `store.go:2541` (bootstrap branch normalization).

Test evidence: a foreign holder of `<doltRoot>/links/.dolt/noms/LOCK` makes `Open` fail with `nbs.ErrDatabaseLocked` in the chain (`engine_open_contract_test.go:64-66`) and bounded (< 10s under a 700ms budget, `engine_open_contract_test.go:69-71`). `OpenSync` under the same holder carries **both** `ErrWorkspaceBusy` and `nbs.ErrDatabaseLocked` (`engine_open_contract_test.go:153-158`).

#### 1.7 Other free helpers defined in store.go

- `dirExists(path string) bool` — `os.Stat` + `IsDir` (`store.go:2685-2688`).
- `scanTime(value string) (time.Time, error)` = `time.Parse(time.RFC3339Nano, value)` (`store.go:2215-2217`). Single parse boundary for every timestamp column.
- `scanNullableTime(sql.NullString) (*time.Time, error)` — invalid → `(nil, nil)` (`store.go:2221-2230`).
- `nullableTime(*time.Time) any` — nil → `nil`, else `RFC3339Nano` string (`store.go:2389-2394`).
- `nullableString(string) any` — `""` → SQL `NULL`, else the string (`store.go:2419-2424`).
- `nullableResolution(*model.Resolution) any` — nil → `NULL` (`store.go:2409-2414`).
- `nullableStringPtr(*string) any` — nil → `NULL` (`store.go:2490-2495`).
- `formatNullableTime(*time.Time) string` — nil → `""` (`store.go:2428-2433`).
- `formatNullableResolution(*model.Resolution) string` — nil → `""` (`store.go:2447-2452`).
- `formatNullableString(*string) string` — nil → `""` (`store.go:2469-2474`).
- `timesEqual(a, b *time.Time) bool` — both nil equal; one nil unequal; else `a.Equal(*b)` (`store.go:2437-2445`).
- `resolutionsEqual(a, b *model.Resolution) bool` — same nil discipline, `*a == *b` (`store.go:2457-2465`).
- `stringPointersEqual(a, b *string) bool` — same (`store.go:2478-2486`).
- `retentionColumns(issue model.Issue) (archivedAt, deletedAt any)` — projects `model.RetentionTimestamps(issue.Retention())` through `nullableTime` (`store.go:2401-2404`). Sole feeder of the `archived_at`/`deleted_at` column pair.
- `statusForStorage(issue model.Issue) sql.NullString` — if `issue.Capabilities().Status != nil` returns `{String: string(status.Value), Valid: true}`, else the zero `NullString` (SQL NULL) (`store.go:2238-2243`). Containers therefore store NULL status.
- `retentionWord(model.Retention) string` — `"live"` / `"archived"` / `"deleted"`; **panics** `fmt.Sprintf("illegal Retention value %T", r)` on anything else (`store.go:1615-1628`).
- `sortIssuesByRank([]model.Issue)` — stable sort on `Rank`, tie-break `ID` ascending (`store.go:1771-1780`).

---

### 2. Open / OpenForRead / EnsureDatabase / Close lifecycle

#### 2.1 `validateOpenArgs` and `validateDoltRootDir`

`validateDoltRootDir(doltRootDir string) (string, error)` (`store.go:324-329`):
- `strings.TrimSpace(doltRootDir) == ""` → `errors.New("dolt root dir is required")`
- otherwise returns `filepath.Clean(doltRootDir)`.

`validateOpenArgs(doltRootDir, workspaceID string) error` (`store.go:304-312`):
- calls `validateDoltRootDir`, propagating its error;
- `strings.TrimSpace(workspaceID) == ""` → `errors.New("workspace id is required")`;
- performs **no** filesystem I/O (doc `store.go:298-303`).

#### 2.2 `Open(ctx, doltRootDir, workspaceID) (*Store, error)`

`store.go:98-165`. Ordered steps:

1. `validateOpenArgs` (`store.go:99-101`).
2. `acquireWorkspaceShared(ctx, doltRootDir)` (`store.go:107`; defined `internal/store/workspace_lock.go:81`). Acquired **before** database bootstrap.
3. `success := false` plus a deferred release-on-failure that `errors.Join`s the release error into the named return (`store.go:116-124`).
4. `requireNoPendingAdopt(doltRootDir)` (`store.go:134`; defined `internal/store/adopt.go:124`) — runs **after** the lock.
5. `ensureDoltDatabase(ctx, doltRootDir, workspaceID)` (`store.go:137`).
6. `openStoreConnection(ctx, doltRootDir, workspaceID, engineWrite)` (`store.go:140`).
7. `s.releaseWorkspaceLock = release` (`store.go:144`).
8. Under `s.withCommitLock` (`store.go:151-156`): `ensureMasterDefaultBranch(ctx, s.db)` wrapped by `wrapEngineOpenContention`, then `s.migrate(ctx)` (`internal/store/migration_runner.go:275`).
9. On failure of step 8: `s.db.Close()` — a `context.Canceled` close error is dropped, any other is `errors.Join`ed (`store.go:157-159`); `s.releaseWorkspaceLock = nil` (`store.go:160`) so the deferred release still fires; returns `(nil, err)`.
10. `success = true`; return the store.

Behavioral evidence:
- A second concurrent `Open` on the same root blocks while the first store is live and then succeeds after the first `Close` (`engine_serialization_test.go:22-68`); it must still be blocked after a 300 ms window (`engine_serialization_test.go:50-54`) and must complete within 5 s of the release (`engine_serialization_test.go:60-67`).
- `OpenSync` waits on a live foreground `Open` the same way (`engine_serialization_test.go:77-118`).
- Re-`Open` on a current schema adds **no** Dolt commit (`store_test.go:1658-1693`, compared via `dolt log --oneline` line counts).
- Migration is idempotent across opens, measured on the commit log (`store_test.go:2043-2072`).
- `Open` preserves an existing `meta.schema_version` value (`store_test.go:1695-1727`).
- After a SIGKILL delivered inside `commitWorkingSetOnce`, a fresh-process `Open` on the same path succeeds, the commit lock is free, and the killed mutation's staged write is visible (`process_kill_test.go:66-122`).
- After a SIGKILL inside a goose migration step, `Open` still completes and the schema is usable (`process_kill_test.go:159-212`).
- Opening a read-only (frozen) directory fails and mutates nothing (`fixture_residue_test.go:138-158`).

#### 2.3 `OpenForRead(ctx, doltRootDir, workspaceID) (*Store, error)`

`store.go:167-235`. Steps:

1. `validateOpenArgs` (`store.go:168-170`).
2. `acquireWorkspaceShared` **before** the existence stat (`store.go:176`), with the same `success`-guarded deferred release (`store.go:180-188`).
3. `os.Stat(doltRootDir)` (`store.go:189`):
   - `errors.Is(statErr, os.ErrNotExist)` → `fmt.Errorf("repository not initialized with lit — run 'lit init' first")` (`store.go:195`);
   - any other stat error → `fmt.Errorf("stat database dir: %w", statErr)` (`store.go:197`).
4. `requireNoPendingAdopt` (`store.go:202`).
5. `openStoreConnection(..., engineRead)` (`store.go:205`) — **lazy** engine, no ping.
6. `s.releaseWorkspaceLock = release` (`store.go:209`).
7. `s.withCommitLock(ctx, s.migrate)` (`store.go:212`). It does **not** call `EnsureDatabase` (comment `store.go:210-211`).
8. On migrate failure: if `isManifestReadOnlyError(err)` (`commit_lock.go:483-489`), the error is re-wrapped as

```
fmt.Errorf("this read open needed to apply pending schema migrations, but another process (a snapshot copy or a live writer) is holding the store read-only; retry after it completes: %w", err)
```
(`store.go:225`). Then `s.db.Close()` (dropping `context.Canceled`), `s.releaseWorkspaceLock = nil`, return the error (`store.go:227-231`).

Behavioral evidence:
- On a missing directory, `OpenForRead` errors and creates nothing — `<doltRoot>/links` still does not exist (`store_test.go:1769-1782`).
- A read open beside a foreign journal-lock holder succeeds and serves reads (count = 1) via Dolt's read-only fallback (`engine_open_contract_test.go:165-198`; `dolt_journal_hold_test.go:185-198`).
- A read open does not wait on a live write engine — it completes inside a 1-second context (`engine_serialization_test.go:127-146`).
- A read open with a **pending migration** under a held journal lock fails with a message containing `"pending schema migrations"`, and the same open succeeds once the holder releases (`dolt_journal_hold_test.go:70-129`). The profile note at `dolt_journal_hold_test.go:60-69` records that read opens configure no BackOff and Dolt's own journal wait is 100 ms before the read-only fallback.
- A read open on a current schema creates no Dolt commit (`store_test.go:1729-1767`).
- Under a held `LockDoltJournalExclusive`, a read open performs **no** journal crash-recovery I/O — the dirtied journal stays byte-identical; with the lock free the same open truncates it (`dolt_journal_hold_test.go:145-229`).
- On an unreconcilable schema (`issues` with only an `id` column) `OpenForRead` fails with an error naming the missing `status` column (`store_test.go:1801-1839`).

#### 2.4 `EnsureDatabase(ctx, doltRootDir, workspaceID) (bool, error)`

`store.go:276-296`. `validateOpenArgs` → `acquireWorkspaceShared` (deferred unconditional release, `errors.Join`ed into the named return, `store.go:284-288`) → `requireNoPendingAdopt` → `ensureDoltDatabase`. Returns `ensureDoltDatabase`'s created-flag. Doc states `Open`/`OpenSync` do **not** call it because they already hold the lock (`store.go:270-273`).

#### 2.5 `ensureDoltDatabase(ctx, doltRootDir, workspaceID) (bool, error)`

`store.go:2497-2544`:
1. `root := filepath.Clean(doltRootDir)` (`store.go:2498`).
2. If `dirExists(filepath.Join(root, "links", ".dolt"))` → returns `(false, nil)` immediately, doing nothing (`store.go:2507-2509`).
3. `created := !dirExists(root)` (`store.go:2510`).
4. `os.MkdirAll(root, 0o755)`; on failure `fmt.Errorf("create dolt root dir: %w", err)` (`store.go:2511-2513`).
5. First bootstrap pool: `openDoltPool(root, workspaceID, "", engineWrite)` (empty database name), `defer db.Close()` inside a closure so it closes before the next open (`store.go:2519-2529`); runs `CREATE DATABASE IF NOT EXISTS links` (`store.go:2525`); failure → `fmt.Errorf("create dolt database: %w", err)` then `wrapEngineOpenContention` (`store.go:2526`, `store.go:2533`).
6. Second pool: `openDoltPool(root, workspaceID, "links", engineWrite)`, `defer db.Close()`, then `ensureMasterDefaultBranch` wrapped in `wrapEngineOpenContention` (`store.go:2535-2542`).
7. Returns `(created, nil)`.

The two pools run strictly sequentially — the explicit close of the first is the ordering owner (`store.go:2514-2518`).

#### 2.6 `openStoreConnection(ctx, doltRootDir, workspaceID, access) (*Store, error)`

`store.go:381-399`:
- `openDoltPool(doltRootDir, workspaceID, doltDatabaseName, access)` (`store.go:382`);
- if `access == engineWrite`: `db.PingContext(ctx)`; on failure returns `errors.Join(wrapEngineOpenContention(err), db.Close())` (`store.go:386-390`);
- builds the `Store` with the field assignments listed in §1.3 (`store.go:391-398`). `doltRootDir` is stored **unmodified**; only `commitLockPath` and `telemetryDir` clean it.

Read engines stay lazy deliberately (`store.go:372-380`).

#### 2.7 `newDoltConnector` / `openDoltPool`

`newDoltConnector(doltRootDir, workspaceID, database string, access engineAccess) (*embedded.Connector, error)` (`store.go:2647-2668`):
- `author := strings.TrimSpace(workspaceID)`; if empty → `"links"` (`store.go:2648-2651`);
- `author = strings.ReplaceAll(author, "@", "_")` (`store.go:2652`);
- `embedded.Config{ Directory: filepath.Clean(doltRootDir), CommitName: author, CommitEmail: fmt.Sprintf("%s@links.local", author), Database: database, DisableSingletonCache: true }` (`store.go:2653-2659`);
- `if access == engineWrite { cfg.BackOff = newEngineOpenBackOff() }` (`store.go:2660-2662`);
- connector construction failure → `fmt.Errorf("open dolt: %w", err)` (`store.go:2665`).

**Dolt commit identity** therefore comes entirely from `workspaceID`: name = workspace id with `@`→`_`, email = `<name>@links.local`. `DisableSingletonCache: true` ties engine (and journal-lock) lifetime to the pool's lifetime.

`openDoltPool` (`store.go:2672-2683`): `sql.OpenDB(connector)`, then `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)` — exactly one connection per Store.

#### 2.8 `reconnect(ctx) error`

`store.go:425-440`. Unconditional rotation:
1. `openDoltPool(s.doltRootDir, s.workspaceID, doltDatabaseName, s.access)`; failure → `fmt.Errorf("reopen dolt: %w", err)` (`store.go:427-430`).
2. `prev := s.db; s.db = next` (`store.go:431-432`) — swap **before** closing.
3. `prev.Close()`; a `context.Canceled` is tolerated, anything else → `fmt.Errorf("close prior dolt connection after reconnect: %w", err)` (`store.go:433-435`).
4. `next.PingContext(ctx)`; failure → `fmt.Errorf("reopen dolt: %w", wrapEngineOpenContention(err))` (`store.go:436-438`).

Doc: must be called under the commit lock; it is the one site where the journal lock is taken while the commit lock is held, bounded by `engineOpenRetryMaxElapsed` (~30 s) against the ~15-minute commit-lock budget (`store.go:401-424`). `reconnect` is the `connectionRotator` passed into every retry loop (`commit_lock.go:175`, `commit_lock.go:274`).

#### 2.9 `Close() error`

`store.go:339-360`:
1. `err := s.db.Close()`; if `errors.Is(err, context.Canceled)` → `err = nil` (`store.go:340-344`).
2. If `s.releaseWorkspaceLock != nil`: capture, set field to `nil`, call it, `errors.Join` any release error onto `err` (`store.go:352-358`).
3. Return `err`.

Ordering: `db.Close()` (which releases Dolt's journal lock) runs before the workspace release (`store.go:345-351`).

#### 2.10 `AttributeTo(streamToken string)`

`store.go:260-262`: `s.attribution = model.NewAttribution(streamToken, s.workspaceID)`. No return value, no validation here — a blank token yields an absent attribution by `NewAttribution`'s contract (`store.go:248-250`).

Evidence:
- An unattributed store writes SQL `NULL` in both `stream_id` and `workspace_id` for every event kind (`event_attribution_test.go:74-96`).
- After `AttributeTo(token)`, **every** event kind (created, field update, start) carries `model.NewAttribution(token, workspaceID)` (`event_attribution_test.go:103-123`).
- `AttributeTo("")` writes no half pair — `workspace_id` stays NULL (`event_attribution_test.go:130-152`).
- Replaying an export preserves the producer's attribution rather than re-stamping the restorer's (`event_attribution_test.go:159-193`).

#### 2.11 `ExecRawForTest(ctx, query string, args ...any) error`

`store.go:334-337`: `s.db.ExecContext(ctx, query, args...)`, returning only the error. No commit lock, no `commitWorkingSet` (doc `store.go:331-333`). Used by tests to probe schema CHECK constraints (`store_test.go:2083-2090`, `store_test.go:2104-2111`) and to hard-delete a row (`store_test.go:2609`).

---

### 3. Commit semantics (owned by `commit_lock.go`, reached from every store.go mutation)

#### 3.1 `withMutation` / `withStampedMutation`

`func (s *Store) withMutation(ctx context.Context, message string, fn func(ctx context.Context, tx *sql.Tx) error) error` (`commit_lock.go:122-124`) delegates to `withStampedMutation(ctx, commitStamp{Message: message}, fn)`.

`withStampedMutation` (`commit_lock.go:156-177`) runs, under `withCommitLock`, inside `retryTransientGCContention`:
1. If `!staged`: `s.db.BeginTx(ctx, nil)` — failure → `fmt.Errorf("begin %s tx: %w", stamp.Message, err)` (`commit_lock.go:161-164`);
2. `defer tx.Rollback()` (`commit_lock.go:165`);
3. `fn(ctx, tx)` — its error is returned verbatim (`commit_lock.go:166-168`);
4. `tx.Commit()` — failure → `fmt.Errorf("commit %s tx: %w", stamp.Message, err)` (`commit_lock.go:169-171`);
5. `staged = true` (`commit_lock.go:172`);
6. `s.commitWorkingSetOnce(ctx, stamp)` (`commit_lock.go:174`).

The `staged` flag is the phase marker: a retry after a successful `tx.Commit` resumes at the DOLT_COMMIT step and does **not** re-run `fn` (`commit_lock.go:144-155`).

`commitStamp` (`commit_lock.go:104-116`) fields: `Message string`, `Date time.Time` (rendered as `--date` RFC3339 UTC; second granularity), `Author string` (rendered `--author`), `AllowEmpty bool`. `store.go` mutations always use the zero-beyond-Message form.

#### 3.2 `commitWorkingSetOnce` — the exact Dolt commit

`commit_lock.go:286-320`:
- fires `s.commitWorkingSetHookForTest` first if non-nil, returning its error (`commit_lock.go:287-291`);
- `trimmed := strings.TrimSpace(stamp.Message)`; if empty → `"links mutation"` (`commit_lock.go:292-295`);
- `args := []any{"-Am", trimmed}` (`commit_lock.go:296`) — i.e. **`DOLT_COMMIT('-Am', <message>)`**, the `-A` doing the staging so there is no separate `DOLT_ADD` call anywhere on this path;
- appends `"--allow-empty"` when `stamp.AllowEmpty` (`commit_lock.go:297-299`);
- appends `"--date", stamp.Date.UTC().Format(time.RFC3339)` when `Date` is non-zero (`commit_lock.go:300-304`);
- appends `"--author", stamp.Author` when non-empty (`commit_lock.go:305-307`);
- executes `buildProcedureCall("DOLT_COMMIT", len(args))` via `QueryRowContext(...).Scan(&commitHash)` (`commit_lock.go:311`);
- `err == nil` → success (`commit_lock.go:312-314`);
- if `strings.Contains(strings.ToLower(err.Error()), "nothing to commit")` → returns `nil` (success-with-no-commit) (`commit_lock.go:315-318`);
- otherwise `wrapCommitWorkingSetError(err)` (`commit_lock.go:319`).

There is **no `--skip-empty`** flag anywhere; "nothing to commit" is absorbed by the string check above.

**Commit message format strings actually used by store.go mutations** (the literal passed to `withMutation`):
- `"record sync state"` (`store.go:457`)
- `"create issue"` (`store.go:509`)
- `"apply update"` (`store.go:1115`)
- `"add comment"` (`store.go:1153`)
- `"delete comment"` (`store.go:1172`)

Each is used verbatim as the Dolt commit message (`commit_lock.go:292-296`) and inside tx error text (`commit_lock.go:163`, `commit_lock.go:170`).

`commitWorkingSet(ctx, message)` (`commit_lock.go:268-276`) is the standalone version: `withCommitLock` → `retryTransientGCContention` → `commitWorkingSetOnce(commitStamp{Message: message})`. Exercised at `store_test.go:1707`.

Test evidence for one-commit-per-mutation: a combined transition+field `Apply` adds exactly **1** row to `dolt_log()` (`update_atomicity_test.go:14-71`, count query at `update_atomicity_test.go:17`).

#### 3.3 `withCommitLock`, re-entrancy, release settlement

`withCommitLock(ctx, operation retryOperation) (err error)` (`commit_lock.go:322-333`): acquire → `defer func(){ err = SettleCommitLockRelease(err, release()) }()` (fires on panic too) → `operation(lockedCtx)`.

`acquireCommitLock` (`commit_lock.go:357-366`): if `ctx.Value(commitLockContextKey{})` is `true`, returns the same ctx and a no-op release (re-entrant short-circuit); otherwise `acquireCommitLockAtPath(ctx, s.commitLockPath)` and returns `context.WithValue(ctx, commitLockContextKey{}, true)`.

`acquireCommitLockAtPath` (`commit_lock.go:416-422`): `acquireStoreLock(ctx, lockPath, true /*exclusive*/, commitLockRetryAttempts, commitLockRetryDelay)`, errors passed through `wrapCommitLockContention`.

Budgets (`commit_lock.go:76-77`): `commitLockRetryAttempts = 9000`, `commitLockRetryDelay = 100 * time.Millisecond` → ~15 minutes.

`wrapCommitLockContention` (`commit_lock.go:430-435`): when `errors.Is(err, ErrWorkspaceBusy)` returns
```
fmt.Errorf("another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes: %w", err)
```
Every other error, cancellation included, passes through.

`SettleCommitLockRelease(opErr, releaseErr error) error` (`commit_lock.go:346-355`): `releaseErr == nil` → `opErr`; both non-nil → `errors.Join(opErr, releaseErr)`; op succeeded but release failed → prints to stderr
```
lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): %v
```
and returns `nil`.

Test evidence:
- Two `withCommitLock` calls serialize; the second cannot enter within a 25 ms window while the first holds (`retry_test.go:391-442`).
- A panic inside a `withMutation` fn releases the lock, and a subsequent `CreateIssue` succeeds (`crash_safety_test.go:33-59`).
- A panic inside `withCommitLock`'s operation releases the lock (`crash_safety_test.go:63-78`).
- Nested `withCommitLock` short-circuits and the inner ctx still carries `commitLockContextKey{} == true` (`crash_safety_test.go:128-149`).
- A cancelled ctx against a live holder returns `context.Canceled` rather than burning the budget (`crash_safety_test.go:154-181`).
- Ten concurrent `CreateIssue` goroutines all succeed with unique ids, all readable, and the lock is free afterwards (`concurrent_test.go:23-94`).
- Mixed concurrent creates/comments/priority-updates/transitions all persist and the lock is free (`concurrent_test.go:99-266`).

#### 3.4 Retry classification and budgets

- `ErrTransientGCContention = errors.New("transient online-gc contention")` (`commit_lock.go:34`).
- `transientRetryMaxAttempts = 30` — a **variable** so tests can shrink it (`commit_lock.go:55`; shrunk to 2 at `dolt_journal_hold_test.go:73-75`).
- `transientRetryBaseDelay = 50 * time.Millisecond`, `transientRetryMaxDelay = 1 * time.Second` (`commit_lock.go:58-59`).
- `transientRetryDelay(attempt)` = `base << (attempt-1)`, clamped to `maxDelay`; attempts < 1 treated as 1 (`commit_lock.go:250-259`). Bounded between base and max for attempts 1..10 (`retry_test.go:308-319`).

`retryTransientGCContention(ctx, operation, rotate, delayForAttempt, sleep)` (`commit_lock.go:185-204`): loop `attempt := 1; attempt <= transientRetryMaxAttempts`:
- `classifyTransientGCError(operation(ctx))`; nil → return nil;
- if not `ErrTransientGCContention` **or** last attempt → break;
- `sleep(ctx, delayForAttempt(attempt))` — its error is returned immediately;
- `rotate(ctx)` — its error is returned immediately;
- final: `exhaustedContentionError(lastErr)`.

Retryable/not:
- `isManifestReadOnlyError`: lowercased message contains both `"cannot update manifest"` and `"read only"` (`commit_lock.go:483-489`).
- `isOnlineGCResetError`: lowercased message contains both `"online garbage collection"` and `"reconnect"` (`commit_lock.go:495-501`).
- `isTransientGCContentionError` = either of those (`commit_lock.go:479-481`).
- `exhaustedContentionError` (`commit_lock.go:243-248`): if the exhausted error is manifest-read-only → `WorkspaceWriteBlockedError{Cause: err}`; otherwise unchanged.
- `WorkspaceWriteBlockedError.Error()` (`commit_lock.go:220-227`):
```
another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)
```
with `Unwrap() → Cause` (`commit_lock.go:232`).
- `wrapCommitWorkingSetError` (`commit_lock.go:453-460`): always `fmt.Errorf("dolt commit working set: %w", err)`; marks it transient only when `isTransientGCContentionError`.

Test evidence:
- One transient then success = 2 calls (`retry_test.go:31-53`).
- Full exhaustion returns the last error, still `ErrTransientGCContention`, with exactly `transientRetryMaxAttempts` calls (`retry_test.go:55-84`).
- Exhausted manifest-read-only promotes to `WorkspaceWriteBlockedError` naming "another lit process" while keeping the transient cause chain (`retry_test.go:91-124`).
- Exhausted GC-reset does **not** promote (`retry_test.go:130-152`).
- A non-transient error is not retried — exactly 1 call (`retry_test.go:154-176`).
- Context deadline during backoff surfaces `context.DeadlineExceeded` after 1 call (`retry_test.go:178-201`).
- Rotation happens once per backoff, never after the succeeding call (2 rotations for 3 calls) (`retry_test.go:206-233`).
- A failing rotator aborts the loop with its error and no re-attempt (`retry_test.go:238-261`).
- A rotator blocked on ctx returns `context.Canceled` on cancellation (`retry_test.go:270-306`).
- Cluster-role "please reconnect" is **not** misclassified as GC contention (`retry_test.go:373-380`).
- `wrapCommitWorkingSetError(errors.New("permission denied")).Error() == "dolt commit working set: permission denied"` (`retry_test.go:349-351`).

---

### 4. Issue creation

#### 4.1 `CreateIssue(ctx, in storage.CreateIssueInput) (model.Issue, error)`

`store.go:470-569`. Pre-transaction (pure/validation) phase:
1. `strings.TrimSpace(in.Title) == ""` → `errors.New("title is required")` (`store.go:471-473`).
2. `issueType := in.IssueType`; if `""` → `model.TypeTask` (`store.go:477-480`).
3. `now := time.Now().UTC()` (`store.go:481`).
4. `canonicalizeLabels(in.Labels)` (`store.go:482`; `internal/store/labels.go:112`) — error propagated.
5. `issueid.NormalizeTopicForCreate(in.Topic)` (`store.go:486`) — error propagated. Missing topic yields an error containing `"topic is required"` (`store_test.go:750-759`).
6. `createdBy := "links"` — a hardcoded literal used as the event actor, the label creator, and the parent-edge creator (`store.go:490`).
7. Builds `model.Issue` with **`strings.TrimSpace` applied to** Title, Description, Prompt, Lane, Assignee; `Priority`, `IssueType`, `Topic`, `Labels` copied; `CreatedAt = UpdatedAt = now` (`store.go:491-503`).
8. `model.HydrateRow(issue, model.StatusView{Value: model.StateOpen}, nil)` — new issues start `open` (`store.go:504`). An invalid issue type is rejected here or downstream (`store_test.go:741-748`).
9. `parentID := strings.TrimSpace(in.ParentID)` (`store.go:508`).

Inside `withMutation(ctx, "create issue", ...)` (`store.go:509`):
1. If `parentID != ""`: `SELECT id FROM issues WHERE id = ?`; `sql.ErrNoRows` → `storage.NotFoundError{Entity: "issue", ID: parentID}`; other error → `fmt.Errorf("lookup parent issue %q: %w", parentID, err)` (`store.go:510-517`).
2. `issueid.NormalizeConfiguredPrefix(in.Prefix)`; failure → `fmt.Errorf("normalize issue prefix: %w", err)` (`store.go:518-521`).
3. `issue.ID, err = newIssueID(ctx, tx, prefix, issue.Topic, issue.Title, issue.Description, createdBy, issue.CreatedAt, parentID)` (`store.go:522`; defined `internal/store/issue_ids.go:14`).
4. `issue.Rank, err = nextRankForPlacement(ctx, tx, in.Placement)` (`store.go:526`).
5. `archivedCol, deletedCol := retentionColumns(issue)` (`store.go:530`).
6. The INSERT (`store.go:531-535`), verbatim:
```sql
INSERT INTO issues(
    id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, archived_at, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
```
   Bound values in order: `issue.ID`, `issue.Title`, `issue.Description`, `nullableString(issue.Prompt)`, `statusForStorage(issue)`, `issue.Priority`, `issue.IssueType`, `issue.Topic`, `issue.AssigneeValue()`, `issue.Rank`, `issue.Lane`, `issue.CreatedAt.Format(time.RFC3339Nano)`, `issue.UpdatedAt.Format(time.RFC3339Nano)`, `archivedCol`, `deletedCol`. `closed_at` is a literal `NULL`. Columns `resolution` and `redirect_target` are **not** in the insert list (left to their defaults). Failure → `fmt.Errorf("insert issue: %w", err)` (`store.go:536`).
7. If `parentID != ""`: builds `model.Relation{SrcID: issue.ID, DstID: parentID, Type: model.RelParentChild, CreatedAt: issue.CreatedAt, CreatedBy: "links"}` and routes it through `insertRelationTx` (`store.go:538-551`; `internal/store/relations.go:348`).
8. `s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, createdBy)` (`store.go:552`; `internal/store/labels.go:95`).
9. Event: `createChanges := []model.FieldChange{}`; for **non-container** types appends `{Field:"status", From:"", To:"open"}`; containers get none (`store.go:556-560`). Then `s.recordEvent(ctx, tx, issue.ID, "created", "issue created", "links", createChanges)` (`store.go:561`) — action `"created"`, reason `"issue created"`, actor `"links"`.
10. `smoothRanksIfNeededTx(ctx, tx, issue.Rank)` (`store.go:564`; `internal/store/ranking.go:401`).

Returns the **in-memory** `issue` value (not a re-read) (`store.go:568`).

Defaults, restated as a table for every column the create path touches:

| Column | Value on create |
|---|---|
| `id` | `newIssueID(...)` — `<prefix>-<topic>-<3..8 base36>` for roots (`store_test.go:777-780`), `<parentID>.N` for children (`store_test.go:949-953`) |
| `title` | trimmed input, required non-empty |
| `description` | trimmed input |
| `agent_prompt` | trimmed prompt, `""` stored as SQL NULL |
| `status` | `"open"` for leaves; SQL NULL for containers (`store_test.go:1977-2002`) |
| `priority` | `in.Priority` as given (no default beyond the Go zero value) |
| `issue_type` | `in.IssueType`, `""` → `task` |
| `topic` | normalized, required |
| `assignee` | trimmed input via `AssigneeValue()` |
| `item_rank` | `nextRankForPlacement` |
| `lane` | trimmed input |
| `created_at`/`updated_at` | same `time.Now().UTC()` RFC3339Nano |
| `closed_at` | literal `NULL` |
| `archived_at`/`deleted_at` | `retentionColumns` of a Live issue → both `NULL` |
| `resolution`/`redirect_target` | not written |

Evidence: id shape `^test-renderer-[0-9a-z]{3,8}$` (`store_test.go:777-780`); prefix `"Renderer Platform Team"` normalizes/clamps to `renderer-pla-` (`store_test.go:860-878`); child ids `parent.ID+".1"`, `".2"` (`store_test.go:913-962`); id collisions advance a nonce so a second identical input yields a different id (`store_test.go:880-911`); labels come back canonicalized and sorted (`{"Renderer","gpu"}` → `["gpu","renderer"]`, `store_test.go:1184-1186`); prompt round-trips and is searchable (`store_test.go:786-846`); the schema CHECK rejects a non-NULL status on an epic and a NULL status on a leaf (`store_test.go:2078-2112`).

#### 4.2 Rank placement

`nextRankForPlacement(ctx, tx, p storage.RankPlacement) (string, error)` (`store.go:2045-2054`): `storage.RankTop` → `nextRankAtTop`; `storage.RankBottom` → `nextRankAtBottom`; default → `fmt.Errorf("unknown rank placement: %d", p)`.

`nextRankAtBottom` (`store.go:2058-2068`):
```sql
SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' ORDER BY item_rank DESC LIMIT 1
```
Non-`ErrNoRows` error → `fmt.Errorf("query last rank: %w", err)`. Invalid/empty result → `rank.Initial()`; else `rank.After(lastRank)`.

`nextRankAtTop` (`store.go:2072-2082`): same query with `ORDER BY item_rank ASC`; error text `"query first rank: %w"`; empty → `rank.Initial()`; else `rank.Before(firstRank)`.

The **zero value** of `storage.RankPlacement` behaves as bottom/append: consecutive default creates keep authoring order, and an explicit `RankTop` create sorts ahead of them (`store_test.go:1019-1049`).

---

### 5. Reads

#### 5.1 The issue projection

`issueColumns` (`store.go:2091-2095`), the single ordered projection, 18 columns:
```
id, title, description, agent_prompt, status, priority,
issue_type, topic, assignee, item_rank, lane, created_at,
updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at
```
`issueProjection(alias)` (`store.go:2100-2110`) joins them with `", "`, prefixing `alias+"."` when alias is non-empty. Derived once: `issueColumnsBare = issueProjection("")` and `issueColumnsQualified = issueProjection("i")` (`store.go:2114-2117`).

#### 5.2 Row scanners

`issueScanner interface{ Scan(dest ...any) error }` (`store.go:2014`).

`issueRow struct { Issue partialIssue; Status model.StatusView }` (`store.go:2016-2019`).

`partialIssue` (`store.go:2024-2039`): `ID, Title, Description, Prompt string; Priority model.Priority; IssueType model.IssueType; Topic, Assignee, Rank, Lane string; Labels []string; CreatedAt, UpdatedAt time.Time; Retention model.Retention`.

`scanIssue(row)` (`store.go:2122-2134`) scans the 18 columns positionally in `issueColumns` order, with `prompt`, `status`, `closedAt`, `resolution`, `redirectTarget`, `archivedAt`, `deletedAt` as `sql.NullString`; sets `issue.Prompt = prompt.String` (NULL → `""`); delegates to `parsedIssueRow`.

`scanIssueWithParent(row)` (`store.go:2136-2150`) — identical but with a leading `parentID string` column.

`parsedIssueRow(...)` (`store.go:2152-2209`):
- parses `created_at`/`updated_at` via `scanTime` (errors propagate);
- `statusView := model.StatusView{Value: model.State(status.String)}` — NULL status becomes `model.State("")`;
- valid `closed_at` → parsed into `statusView.ClosedAt`;
- valid `resolution` → `model.Resolution(resolution.String)` raw-converted (no re-parse) into `statusView.Resolution` (`store.go:2175-2183`);
- valid `redirect_target` → `statusView.RedirectTarget` (`store.go:2184-2190`);
- `archived_at`/`deleted_at` parsed into `*time.Time` and folded through `model.RetentionFromTimestamps` (`store.go:2191-2206`);
- `issue.Labels = []string{}` (`store.go:2207`).

#### 5.3 `hydrateIssues(ctx, rows []issueRow) ([]model.Issue, error)`

`store.go:2245-2304`. Query count is **fixed per recursion level**, not per epic:
1. Empty input → `([]model.Issue{}, nil)` with no query (`store.go:2246-2248`).
2. One `loadLabelsByIssueIDs` query for all ids (`store.go:2253`).
3. Collects container ids; one `lifecycleChildrenByEpicIDs` query for all of them (`store.go:2257-2266`).
4. Per row, builds a `model.Issue` copying every `partialIssue` field, `SetRetention(row.Issue.Retention)`, `Labels` defaulted to `[]string{}` when the map has no entry, and calls `model.HydrateRow(base, row.Status, childrenByEpicID[id])` (`store.go:2268-2294`).
5. Post-condition: `!issue.IsHydrated()` → `fmt.Errorf("hydrateIssues: produced unhydrated issue %s", issue.ID)` (`store.go:2298-2300`).

`loadLabelsByIssueIDs` (`store.go:2366-2387`):
```sql
SELECT issue_id, label FROM labels WHERE issue_id IN (?, ?, ...) ORDER BY label ASC
```
failure → `fmt.Errorf("load labels by issue ids: %w", err)`.

`lifecycleChildrenByEpicIDs(ctx, epicIDs)` (`store.go:2306-2364`) — empty input returns an empty map without querying; otherwise one query:
```sql
SELECT r.dst_id, <issueColumnsQualified>
FROM relations r
JOIN issues i ON i.id = r.src_id
JOIN issues p ON p.id = r.dst_id
WHERE r.dst_id IN (?, ...) AND r.type = 'parent-child'
    AND (p.archived_at IS NOT NULL OR p.deleted_at IS NOT NULL OR (i.archived_at IS NULL AND i.deleted_at IS NULL))
ORDER BY r.dst_id ASC, i.item_rank ASC
```
failure → `fmt.Errorf("load lifecycle children: %w", err)`. Visibility truth table (`store.go:2317-2323`): parent live + child live → include; parent live + child dead → exclude; parent dead (archived or deleted) + child either → include. Rows are scanned with `scanIssueWithParent`, hydrated in **one** recursive `hydrateIssues` call, and re-bucketed by the parallel `parentIDs` slice (`store.go:2343-2362`).

Evidence: listing query count for 1 epic equals that for 5 epics, measured by a counting `driver.Conn` that forces every query through `Prepare` (`lifecycle_hydration_query_count_test.go:25-41`, wrapper at `:104-147`). An active epic's `Progress()` excludes archived children (`Total == 0`); the same epic once archived includes them (`Total == 1, Open == 1`) (`store_test.go:2282-2311`).

#### 5.4 `GetIssue(ctx, id) (model.Issue, error)`

`store.go:913-927`:
```sql
SELECT <issueColumnsBare> FROM issues WHERE id = ?
```
`sql.ErrNoRows` → `storage.NotFoundError{Entity: "issue", ID: id}`; any other scan error is returned raw; then `hydrateIssues([]issueRow{scanned})` and `hydrated[0]`. Total queries: 1 + 1 (labels) + 0-or-1 (children of a container).

#### 5.5 `getIssuesByIDs(ctx, ids) (map[string]model.Issue, error)`

`store.go:875-911`. Empty input → empty map, no query. Otherwise:
```sql
SELECT <issueColumnsBare> FROM issues WHERE id IN (?, ?, ...)
```
Errors: `"batch load issues: %w"`, `"scan batch-loaded issue: %w"`, `"iterate batch-loaded issues: %w"`. Missing ids are simply absent from the map (`store.go:871-874`).

#### 5.6 `GetIssueDetail(ctx, id) (model.IssueDetail, error)`

`store.go:774-847`, in order:
1. `GetIssue(ctx, id)` (`store.go:775`).
2. `listRelations(ctx, id)` (`store.go:779`).
3. `listComments(ctx, id)` (`store.go:783`).
4. `listEvents(ctx, id)` (`store.go:787`).
5. `collectRelatedIssueIDs(id, relations)` (`store.go:797`; defined `store.go:851-869`) — distinct counterparties of both `SrcID` and `DstID`, excluding `""` and the focal id, in first-seen order.
6. If `issue.RedirectTargetValue()` is non-nil and not already in the list, it is appended (`store.go:798-800`).
7. `getIssuesByIDs(ctx, relatedIDs)` — one batch hydrate (`store.go:801`).
8. `bucketRelations(id, relations, relatedByID)` → `structural` with `Parent`, `Children`, `DependsOn`, `Blocks` (`store.go:809`; `internal/store/relations.go:22`).
9. If `structural.Parent != nil`: `ListChildren(ctx, structural.Parent.ID)` then `siblingsOf(id, parentChildren)`; otherwise `siblings := []model.Issue{}` (`store.go:814-821`).
10. `redirectTarget` is set only if the target id is present in `relatedByID`; a vanished target hydrates as absent (`store.go:826-831`).
11. `related := relatedFrom(id, relations, relatedByID)` (`store.go:832`).
12. Assembles `model.IssueDetail{Issue, Relations, Comments, Events, Children, Siblings, DependsOn, Blocks, Parent, Related, RedirectTarget}` (`store.go:833-845`).

Evidence: `DependsOn`, `Blocks`, `Children`, `Related` all come back in rank order (`store_test.go:1084-1167`); relation counterparties are fully hydrated including container progress and label slices (`store_test.go:1875-1939`); siblings are the parent's other children in rank order excluding self, and empty for parentless issues and only children (`store_test.go:2834-2893`); the redirect target is exposed via `detail.RedirectTarget` with **no** `related-to` edge written (`store_test.go:2347-2388`).

#### 5.7 `ListIssues(ctx, filter storage.ListIssuesFilter) ([]model.Issue, error)`

`store.go:571-703`. Base query: `SELECT <issueColumnsQualified> FROM issues i` (`store.go:572`).

WHERE clauses, appended in this exact order:

| Condition | Clause | Line |
|---|---|---|
| `!filter.IncludeArchived` | `i.archived_at IS NULL` | `store.go:575-577` |
| `!filter.IncludeDeleted` | `i.deleted_at IS NULL` | `store.go:578-580` |
| `len(filter.IssueTypes) > 0` | `i.issue_type IN (?,...)` | `store.go:591-598` |
| `len(filter.ExcludeIssueTypes) > 0` | `i.issue_type NOT IN (?,...)` | `store.go:599-609` |
| `len(filter.Assignees) > 0` (blank entries skipped; clause omitted if all blank) | `i.assignee IN (?,...)` | `store.go:610-623` |
| `filter.UpdatedAfter != nil` | `i.updated_at >= ?` bound with `.UTC().Format(time.RFC3339Nano)` | `store.go:624-627` |
| `filter.UpdatedBefore != nil` | `i.updated_at <= ?` same formatting | `store.go:628-631` |
| `filter.HasComments != nil`, true | `EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)` | `store.go:633-635` |
| `filter.HasComments != nil`, false | `NOT EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)` | `store.go:635-637` |
| each canonicalized label in `filter.LabelsAll` | `EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)` (one clause per label — AND semantics) | `store.go:639-648` |
| `len(filter.IDs) > 0` (blank skipped) | `i.id IN (?, ?)` joined with `", "` | `store.go:649-662` |
| each non-blank `filter.SearchTerms` term, lowercased & trimmed | `(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ? OR LOWER(COALESCE(i.agent_prompt, '')) LIKE ? OR LOWER(i.topic) LIKE ?)` with `%term%` bound four times | `store.go:663-671` |

Clauses are joined with `" AND "` (`store.go:672-674`).

**Status and resolution are NOT filtered in SQL.** `parseStatusFilter(filter.Statuses)` (`store.go:585`, defined `store.go:705-712`) only maps each raw value through `model.DefaultOpen(string(raw))` and never errors; the actual filtering happens post-hydration.

Ordering: `buildIssueOrderClause(filter.SortBy)` (`store.go:675`, defined `store.go:1737-1769`):
- no specs → `"i.item_rank ASC, i.id ASC"`;
- allowed sort fields (case-insensitive, trimmed) and their columns: `id→i.id`, `title→i.title`, `status→i.status`, `priority→i.priority`, `rank→i.item_rank`, `type→i.issue_type`, `topic→i.topic`, `assignee→i.assignee`, `created_at→i.created_at`, `updated_at→i.updated_at`;
- unknown field → `fmt.Errorf("unsupported sort field %q", spec.Field)`;
- direction `DESC` when `spec.Desc`, else `ASC`;
- `"i.id ASC"` is always appended as the final tiebreaker.

Execution and post-processing:
- query failure → `fmt.Errorf("list issues: %w (query=%s)", err, query)` — the full SQL is included (`store.go:682`);
- rows scanned via `scanIssue`, `rows.Err()` checked (`store.go:685-695`);
- `hydrateIssues` (`store.go:696`);
- return `capLimit(filterByResolution(filterByState(hydrated, allowedStates), filter.Resolutions), filter.Limit)` (`store.go:702`).

`filterByState` (`store.go:719-734`): empty allow-list passes everything; otherwise keeps issues whose **derived** `issue.State()` is in the set.
`filterByResolution` (`store.go:742-761`): empty allow-list passes everything; otherwise drops every issue whose `ResolutionValue()` is nil and keeps only matching resolutions.
`capLimit` (`store.go:767-772`): `limit <= 0` means uncapped; else truncates to the first `limit`.

Evidence: epic state is filtered by derived lifecycle, not the dead `i.status` column, across open/mixed/closed epic shapes (`store_test.go:219-292`); all advanced filters combine correctly to a single result (`store_test.go:964-1017`); archived issues disappear from the default list and reappear with `IncludeArchived` (`store_test.go:1344-1358`).

#### 5.8 `ListTopics(ctx) ([]string, error)`

`store.go:1630-1645`:
```sql
SELECT DISTINCT topic FROM issues WHERE deleted_at IS NULL AND topic <> '' ORDER BY topic ASC
```
failure → `fmt.Errorf("list topics: %w", err)`. Returns `[]string{}` (never nil) plus `rows.Err()`.

#### 5.9 Relation, comment, and label reads

`listRelations(ctx, issueID)` (`store.go:1647-1668`):
```sql
SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? OR dst_id = ? ORDER BY created_at ASC
```
error → `fmt.Errorf("list relations: %w", err)`; `created_at` parsed via `scanTime`.

`listAllRelations(ctx)` (`store.go:1879-1900`): same projection, no WHERE, `ORDER BY created_at ASC`; error → `"list all relations: %w"`.

`listComments(ctx, issueID)` (`store.go:1848-1869`):
```sql
SELECT id, issue_id, body, created_at, created_by FROM comments WHERE issue_id = ? ORDER BY created_at ASC
```
error → `"list comments: %w"`.

`listAllComments(ctx)` (`store.go:1902-1923`): same without the WHERE; error → `"list all comments: %w"`.

`listAllLabels(ctx)` (`store.go:1782-1803`):
```sql
SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC
```
error → `"list all labels: %w"`.

#### 5.10 Event reads

`listEvents(ctx, issueID)` (`store.go:1871-1877`): `queryEvents(ctx, "e.issue_id = ?", issueID)`; error → `fmt.Errorf("list issue events: %w", err)`.

`ListAllEvents(ctx)` (`store.go:1930-1936`): `queryEvents(ctx, "")`; error → `fmt.Errorf("list all issue events: %w", err)`. Doc explains no recency cutoff is applied because claim derivation needs arbitrarily old establishing events (`store.go:1925-1929`).

`queryEvents(ctx, whereClause string, args ...any)` (`store.go:1942-2012`):
```sql
SELECT e.id, e.issue_id, e.action, e.reason, e.actor, e.created_at, e.stream_id, e.workspace_id, c.field, c.from_value, c.to_value
    FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id
[ WHERE <whereClause> ]
 ORDER BY e.created_at ASC, e.id ASC, c.field ASC
```
(`store.go:1943-1957`). Exactly one query. Nullable columns: `action`, `stream_id`, `workspace_id`, `c.field`, `c.from_value`, `c.to_value`. Collapsing rules:
- an event is materialized on first sight, keyed by id in `idx` (`store.go:1966`, `:1973-1998`);
- `Attribution: model.NewAttribution(evtStream.String, evtWorkspace.String)` — NULL becomes `""` which the constructor collapses to absent (`store.go:1985-1990`);
- `Changes` starts as `[]model.FieldChange{}` (`store.go:1991`);
- `Action` set only when the column is valid (`store.go:1993-1995`);
- a change row is appended only when `c.field` is valid; `From`/`To` only when their columns are valid, otherwise left `""` (`store.go:2000-2009`).
The `c.field ASC` sort is deliberate so two reads of an unchanged event compare identical (`store.go:1948-1956`).

---

### 6. Update path

#### 6.1 `Apply(ctx, id string, c storage.Change) (model.Issue, error)`

`store.go:1073-1132`, the single execution path for issue-record changes:
1. `current, err := s.GetIssue(ctx, id)` — a not-found id fails here (`store.go:1074-1077`).
2. `actor := strings.TrimSpace(c.Actor)`; if empty → `"unknown"` (`store.go:1086-1089`).
3. `baseline := current` (`store.go:1090`).
4. If `c.Action != nil`: `lw, err = s.planLifecycleAction(ctx, current, actor, strings.TrimSpace(c.Reason), c.Action)`; on error, returns immediately with **no** writes; then `baseline = lw.postIssue()` so a following field write diffs against the post-action issue (`store.go:1092-1100`).
5. `hasFields := !c.Fields.IsEmpty()`; if true, `fw, err = planFieldUpdate(baseline, c.Fields, actor)` — a validation error returns before any write (`store.go:1101-1108`).
6. `needsActionWrite := lw != nil && !lw.isNoop()` (`store.go:1109`).
7. `applyPreMutationHookForTest` fires here if set (`store.go:1111-1113`).
8. If `needsActionWrite || hasFields`: one `withMutation(ctx, "apply update", ...)` running `lw.applyTx` then `s.applyFieldsTx`, both in the **same** tx and therefore one Dolt commit (`store.go:1114-1130`).
9. Returns `s.GetIssue(ctx, id)` — a fresh re-read, always (`store.go:1131`).

Evidence: transition + field lands as exactly one Dolt commit with both halves visible (`update_atomicity_test.go:26-71`); an invalid field (empty title) paired with a valid transition leaves state, title, and event count **wholly** unchanged (`update_atomicity_test.go:80-131`); the full IssueType × flag-combination matrix shows container transitions rejected with `model.ContainerActionError` and nothing written, field writes succeeding on every type, and zero transition events for field-only cells (`update_matrix_test.go:55-198`).

#### 6.2 `planLifecycleAction`

`store.go:1219-1238`. Type switch on `model.Action`:
- `model.StatusAction` → `s.planStatusTransition(...)`;
- `model.RetentionAction` → `planRetentionTransition(...)` (a free function, no store);
- anything else → **panic** `fmt.Sprintf("illegal Action value %T", action)` (`store.go:1236`).

`lifecycleWrite` interface (`store.go:1204-1212`): `applyTx(ctx, s *Store, tx *sql.Tx) error`, `postIssue() model.Issue`, `isNoop() bool`.

#### 6.3 `applyTransition` (the guard)

`store.go:89-96`: if `model.Frozen(issue.Retention())` → `fmt.Errorf("cannot %s archived or deleted issue", action.Name())`; otherwise `issue.Apply(action)`. Never mutates the store.

Evidence: a container refuses `Reopen`; a live leaf accepts `Start`; an archived leaf refuses `Close` (`store_test.go:701-739`).

#### 6.4 `transitionWrite` and `planStatusTransition`

`transitionWrite` fields (`store.go:1251-1266`): `issueID, fromStatus, toStatus, postAssignee string; now time.Time; closedAtArg, resolutionArg, redirectTargetArg any; action model.ActionName; reason, actor string; changes []model.FieldChange; post model.Issue; noop bool`. Methods at `store.go:1268-1272`.

`planStatusTransition(ctx, issue, actor, reason, action) (transitionWrite, error)` (`store.go:1274-1372`):
1. `applyTransition(issue, action)` → `updated` or the rejection.
2. `priorAssignee := issue.AssigneeValue()`; `postAssignee := priorAssignee` unless the action is `model.Start`, in which case `postAssignee = strings.TrimSpace(start.Assignee)` (`store.go:1279-1289`). Only `Start` rewrites the assignee.
3. `fromStatus := issue.StatusValue()`, `toStatus := updated.StatusValue()` (`store.go:1290-1291`).
4. **No-op rule**: `toStatus == fromStatus && postAssignee == priorAssignee` → `transitionWrite{noop: true, post: issue}` — no write, no event (`store.go:1297-1299`).
5. `now := time.Now().UTC()` (`store.go:1300`).
6. `closedAtArg` = `updated.ClosedAtValue().Format(time.RFC3339Nano)` when non-nil, else nil (`store.go:1301-1304`).
7. `resolutionArg` = `string(*updated.ResolutionValue())` when non-nil, else nil (`store.go:1309-1313`).
8. `redirectTargetArg` = `*updated.RedirectTargetValue()` when non-nil, else nil (`store.go:1325-1329`). Its integrity is deliberately **not** validated here (`store.go:1314-1324`).
9. Change rows, in this order (`store.go:1333-1352`):
   - `status` when `fromStatus != toStatus`;
   - `closed_at` when `!timesEqual(prior, new)`, values via `formatNullableTime`;
   - `resolution` when `!resolutionsEqual(...)`, via `formatNullableResolution`;
   - `redirect_target` when `!stringPointersEqual(...)`, via `formatNullableString`;
   - `assignee` when `priorAssignee != postAssignee`.
10. `updated.UpdatedAt = now` (`store.go:1353`) and the struct is returned with `post: updated`.

Evidence: each of the six non-identity (from→to) pairs records exactly one event carrying the action's own name (`store_test.go:1418-1482`); a same-state `Start` with a new assignee records one `start` event with the calling actor and **no** status change row, and persists the new assignee (`store_test.go:1492-1546`); a same-state, same-assignee `Start` records zero events and does not bump `UpdatedAt` (`store_test.go:1553-1589`).

#### 6.5 `applyTransitionTx`

`store.go:1386-1412`:
1. `validateRedirectTarget(ctx, tx, w.post.ID, w.post.ResolutionValue(), w.post.RedirectTargetValue())` — on the **same tx** as the write (`store.go:1391`).
2. The guarded UPDATE (`store.go:1395-1396`):
```sql
UPDATE issues SET status = ?, assignee = ?, updated_at = ?, closed_at = ?, resolution = ?, redirect_target = ? WHERE id = ? AND status = ?
```
bound `w.toStatus, w.postAssignee, w.now.Format(time.RFC3339Nano), w.closedAtArg, w.resolutionArg, w.redirectTargetArg, w.issueID, w.fromStatus`. Failure → `fmt.Errorf("update issue status: %w", err)`.
3. `result.RowsAffected()` failure → `fmt.Errorf("read status transition result: %w", err)` (`store.go:1400-1403`).
4. `affected == 0` → look up the live status via `currentStatusTx` and return `fmt.Errorf("%s conflict: issue status is %q", w.action, currentStatus)` (`store.go:1404-1410`). Exact observed text: `close conflict: issue status is "closed"` (`store_test.go:2180`).
5. `s.recordEvent(ctx, tx, w.issueID, string(w.action), w.reason, w.actor, w.changes)` (`store.go:1411`).

The UPDATE touches only the status-axis columns — a stale transition cannot clobber the retention pair (`store.go:1383-1385`).

#### 6.6 `retentionWrite` and `planRetentionTransition`

`retentionWrite` fields (`store.go:1422-1437`): `issueID string; now time.Time; priorArchived, priorDeleted, nextArchived, nextDeleted any; action model.ActionName; reason, actor string; changes []model.FieldChange; post model.Issue`. `isNoop()` is hardcoded `false` — the Retain table has no same-state success cell (`store.go:1441-1444`).

`planRetentionTransition(issue, actor, reason, action)` (`store.go:1451-1484`):
- `now := time.Now().UTC()`;
- reads `model.RetentionTimestamps(issue.Retention())` and `retentionColumns(issue)` as the CAS guard;
- `model.Retain(issue.Retention(), action, now)` — its error is the rejection (e.g. `"issue is already archived"`, observed at `store_test.go:2695`);
- `post := issue; post.SetRetention(next); post.UpdatedAt = now`;
- change rows: `archived_at` when the timestamps differ, `deleted_at` when they differ, both via `formatNullableTime` (`store.go:1464-1470`).

`retentionWrite.applyTx` (`store.go:1493-1511`):
```sql
UPDATE issues SET updated_at = ?, archived_at = ?, deleted_at = ? WHERE id = ? AND archived_at <=> ? AND deleted_at <=> ?
```
bound `w.now.Format(time.RFC3339Nano), w.nextArchived, w.nextDeleted, w.issueID, w.priorArchived, w.priorDeleted`. Uses MySQL null-safe equality `<=>`. Errors:
- exec failure → `fmt.Errorf("update issue retention: %w", err)`;
- `RowsAffected` failure → `fmt.Errorf("read retention transition result: %w", err)`;
- `affected == 0` → `currentRetentionTx` then `fmt.Errorf("%s conflict: issue retention is %q", w.action, retentionWord(current))`. Observed text: `archive conflict: issue retention is "archived"` (`store_test.go:2211`).
Then `recordEvent` with the action name, reason, actor, and change rows (`store.go:1510`).

Evidence: a stale archive plan loses to a competing archive with the conflict error (`store_test.go:2189-2214`); delete-on-archived drops the archive stamp and a later restore lands on `Live`, not `Archived` (`store_test.go:2900-2926`); an archive event records exactly one `archived_at` change row and no fake status row (`store_test.go:1376-1380`).

#### 6.7 `validateRedirectTarget`

`store.go:1539-1557` — a free function of `(ctx, tx, closingID string, resolution *model.Resolution, target *string)`:
- `target == nil` and `resolution != nil && resolution.RedirectsToCanonical()` → `fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)`;
- `target == nil` otherwise → `nil`;
- `*target == closingID` → `fmt.Errorf("cannot redirect %s to itself", closingID)`;
- `currentRetentionTx(ctx, tx, *target)` — a missing row surfaces as `storage.NotFoundError`;
- target retention is `model.Deleted` → `fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted", closingID, *target)`;
- `Archived` targets are accepted (matched as the specific `Deleted` variant only, `store.go:1553`).

Evidence: duplicate close records the redirect target on the issue's own column with no graph edge (`store_test.go:2347-2388`); a terminal `Obsolete` close records the resolution and no redirect (`store_test.go:2392-2414`); a redirect to a nonexistent target rolls the whole close back — status stays `open`, resolution and closed_at nil (`store_test.go:2420-2442`); self-redirect rejected (`store_test.go:2446-2465`); redirect to an **archived** canonical succeeds (`store_test.go:2471-2493`); redirect to a **deleted** canonical is rejected with the target id and `"deleted"` in the message and nothing persisted (`store_test.go:2500-2533`); a delete of the canonical injected in the plan→write window via `applyPreMutationHookForTest` is still observed and the close rejected (`store_test.go:2544-2586`); a redirecting outcome with an empty target is rejected (`store_test.go:2644-2656`).

#### 6.8 In-tx read helpers

`currentStatusTx(ctx, tx, issueID) (string, error)` (`store.go:1559-1570`): `SELECT status FROM issues WHERE id = ?` scanned into `sql.NullString` (status is nullable since containers store NULL). `ErrNoRows` → `storage.NotFoundError{Entity:"issue", ID: issueID}`; other → `fmt.Errorf("read issue status: %w", err)`; returns `status.String` (NULL → `""`).

`currentRetentionTx(ctx, tx, issueID) (model.Retention, error)` (`store.go:1575-1592`): `SELECT archived_at, deleted_at FROM issues WHERE id = ?`; `ErrNoRows` → `storage.NotFoundError`; other → `fmt.Errorf("read issue retention: %w", err)`; both columns through `scanNullableTime` then `model.RetentionFromTimestamps`.

`requireIssueExistsTx(ctx, tx, issueID) error` (`store.go:1603-1612`): `SELECT 1 FROM issues WHERE id = ?`; `ErrNoRows` → `storage.NotFoundError`; other → `fmt.Errorf("check issue exists: %w", err)`. Accepts archived/deleted rows — no `deleted_at` filter (`store.go:1600-1602`).

Evidence: a hard-deleted endpoint makes both `AddRelation` and `SetParent` fail with `storage.NotFoundError` naming that id, writing no edge (`store_test.go:2596-2637`).

#### 6.9 `fieldWrite`, `planFieldUpdate`, `applyFieldsTx`

`fieldWrite` (`store.go:935-941`): `issue model.Issue; replaceLabels bool; actor, reason string; changes []model.FieldChange`.

`planFieldUpdate(baseline model.Issue, in storage.UpdateIssueInput, actor string) (fieldWrite, error)` (`store.go:950-1030`) — pure, no clock, no IO:
- `Title != nil` → `strings.TrimSpace(*in.Title)`; empty result → `errors.New("title cannot be empty")` (`store.go:959-964`);
- `Description != nil` → trimmed (`store.go:965-967`);
- `Prompt != nil` → trimmed (`store.go:968-970`);
- `IssueType != nil` → if `issue.IssueType.IsContainer() != in.IssueType.IsContainer()` → `fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())` (`store.go:971-982`);
- `Priority != nil` → assigned as-is (`store.go:983-985`);
- `Assignee != nil` → trimmed (`store.go:986-990`);
- `Lane != nil` → trimmed (`store.go:991-993`);
- `Labels != nil` → `canonicalizeLabels(*in.Labels)`, error propagated (`store.go:994-1000`).

Change rows, emitted only for fields that actually moved, in this order (`store.go:1003-1028`): `title`, `description`, `issue_type` (string cast), `priority` (`strconv.Itoa(int(...))` — the numeric wire encoding, not the display name), `assignee` (compared via `AssigneeValue()`), `lane`, `labels` (compared as `strings.Join(labels, ",")` on both sides).

Return: `fieldWrite{issue, replaceLabels: in.Labels != nil, actor, reason: in.Reason, changes}` (`store.go:1029`).

`applyFieldsTx(ctx, tx, w fieldWrite)` (`store.go:1041-1062`):
1. `issue.UpdatedAt = time.Now().UTC()` — the clock is read here, at the write boundary (`store.go:1045`).
2. The UPDATE (`store.go:1046-1048`):
```sql
UPDATE issues SET
    title = ?, description = ?, agent_prompt = ?, priority = ?, issue_type = ?, assignee = ?, lane = ?, updated_at = ?
    WHERE id = ?
```
bound `issue.Title, issue.Description, nullableString(issue.Prompt), issue.Priority, issue.IssueType, issue.AssigneeValue(), issue.Lane, issue.UpdatedAt.Format(time.RFC3339Nano), issue.ID`. Failure → `fmt.Errorf("update issue: %w", err)`. It is **unguarded** (no CAS) but touches no lifecycle column — `status`, `closed_at`, `resolution`, `redirect_target`, `archived_at`, `deleted_at` are all absent from the SET list (`store.go:1036-1040`).
3. `if w.replaceLabels` → `s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, w.actor)` (`store.go:1051-1055`).
4. `if len(w.changes) > 0` → `s.recordEvent(ctx, tx, issue.ID, "" /* empty action */, w.reason, w.actor, w.changes)` (`store.go:1056-1060`). A field-only update writes an event with a **NULL** `action` column (see §7).

Evidence: a field plan taken against a stale snapshot lands its title change while a concurrently-applied close and archive both survive untouched (`store_test.go:2223-2259`); container↔leaf type changes are refused in both directions while a same-kind change (`task`→`bug`) succeeds (`store_test.go:2011-2035`); label replacement through `Apply` replaces the whole set (`store_test.go:1196-1202`).

---

### 7. Event / attribution rows

`recordEvent(ctx, tx, issueID, action, reason, actor string, changes []model.FieldChange) error` — the single insertion point for issue history (`store.go:1808-1846`).

Constructed event (`store.go:1809-1822`):
- `ID = "evt-" + uuid.NewString()`;
- `Action`, `Reason`, `Actor` each `strings.TrimSpace`d;
- `CreatedAt = time.Now().UTC()`;
- `Attribution = s.attribution` — read off the store, never passed in;
- `Changes = changes`.

Then: `if event.Actor == "" { event.Actor = "unknown" }` (`store.go:1823-1825`); `actionArg` is `nil` when the trimmed action is empty, otherwise the string (`store.go:1826-1829`).

The event insert (`store.go:1830-1832`):
```sql
INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at, stream_id, workspace_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
```
bound `event.ID, event.IssueID, actionArg, event.Reason, event.Actor, event.CreatedAt.Format(time.RFC3339Nano), nullableString(event.Attribution.Stream()), nullableString(event.Attribution.Workspace())`. Failure → `fmt.Errorf("insert issue event: %w", err)`.

Per change row (`store.go:1835-1844`):
- a blank trimmed field name → `fmt.Errorf("issue event %s: field name cannot be empty", event.ID)`;
- otherwise:
```sql
INSERT INTO issue_event_changes(event_id, field, from_value, to_value) VALUES (?, ?, ?, ?)
```
bound `event.ID, field, nullableString(change.From), nullableString(change.To)` — empty from/to become SQL NULL. Failure → `fmt.Errorf("insert issue event change %s.%s: %w", event.ID, field, err)`.

Which mutations emit which event:

| Mutation | action column | reason | actor | change rows |
|---|---|---|---|---|
| `CreateIssue` leaf | `"created"` | `"issue created"` | `"links"` | one `status ""→"open"` |
| `CreateIssue` container | `"created"` | `"issue created"` | `"links"` | none |
| status transition | `string(action.Name())` | `strings.TrimSpace(c.Reason)` | normalized actor (`"unknown"` if blank) | status / closed_at / resolution / redirect_target / assignee, only where moved |
| retention transition | `string(action.Name())` | same | same | archived_at / deleted_at, only where moved |
| field update | SQL **NULL** (empty action) | `in.Reason` | same | one per moved field |
| `AddComment` | — no event at all (`store.go:1153-1158`) | | | |
| `DeleteComment` | — no event at all (`store.go:1172-1193`) | | | |
| `RecordSyncState` | — no event at all (`store.go:456-468`) | | | |

Evidence: a create/close/reopen/archive sequence yields exactly 4 events with actions `""(created)`, `close`, `reopen`, `archive` and the reasons given (`store_test.go:1313-1381`); an empty close reason is stored as `""` (`store_test.go:1383-1409`); attribution stamping/absence is covered by the four tests in `event_attribution_test.go` (§2.10). Deriving claims from `ListIssues`+`GetRelationsByIDs`+`ListAllEvents` leaves both the Dolt HEAD and `dolt_status` unchanged — reads write nothing (`claims_readonly_test.go:76-101`), and attribution written by the real write path derives back into a `claims.Held` for the right checkout (`claims_readonly_test.go:108-145`).

---

### 8. Comments

#### 8.1 `AddComment(ctx, in storage.AddCommentInput) (model.Comment, model.Issue, error)`

`store.go:1139-1162`:
1. `s.GetIssue(ctx, in.IssueID)` — validates existence and doubles as the returned issue, avoiding a second read (`store.go:1140-1143`, doc `:1134-1138`).
2. `body := strings.TrimSpace(in.Body)`; empty → `errors.New("comment body is required")` (`store.go:1144-1147`).
3. `now := time.Now().UTC()`; `comment := model.Comment{ID: "cmt-" + uuid.NewString(), IssueID: in.IssueID, Body: body, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}`; blank `CreatedBy` → `"unknown"` (`store.go:1148-1152`).
4. `withMutation(ctx, "add comment", ...)` runs:
```sql
INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)
```
with `created_at` as RFC3339Nano; failure → `fmt.Errorf("insert comment: %w", err)` (`store.go:1153-1157`).
5. Returns `(comment, issue, nil)` — the issue is the pre-comment read; a comment never changes the issue row.

#### 8.2 `DeleteComment(ctx, commentID string) (model.Comment, error)`

`store.go:1164-1197`:
1. `id := strings.TrimSpace(commentID)`; empty → `errors.New("comment id is required")` (`store.go:1165-1168`).
2. Inside `withMutation(ctx, "delete comment", ...)`:
   - `SELECT id, issue_id, body, created_at, created_by FROM comments WHERE id = ?` (`store.go:1174`);
   - `sql.ErrNoRows` → `storage.NotFoundError{Entity: "comment", ID: id}` (`store.go:1176-1180`); other → `fmt.Errorf("read comment: %w", err)`;
   - `scanTime(createdAt)` into `deleted.CreatedAt` (`store.go:1183-1187`);
   - `deleteCommentTx(ctx, tx, id)` (`store.go:1189`; `internal/store/row_deletes.go:93`).
   Existence and deletion share the tx — no TOCTOU gap (`store.go:1170-1171`).
3. Returns the fully-populated deleted comment.

---

### 9. Meta and sync state

`getMeta(ctx, tx *sql.Tx, key string) (string, error)` (`store.go:1669-1684`): uses `tx` when non-nil, else `s.db`:
```sql
SELECT meta_value FROM meta WHERE meta_key = ?
```
`sql.ErrNoRows` → `("", nil)` (absence is not an error); other → `fmt.Errorf("get meta %q: %w", key, err)`.

`setMeta(ctx, tx, key, value string) error` (`store.go:1686-1700`): picks `tx` or `s.db` as the execer, then:
```sql
INSERT INTO meta(meta_key, meta_value) VALUES (?, ?)
        ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)
```
failure → `fmt.Errorf("set meta %q: %w", key, err)`. Note: called with `tx == nil` from `ensureMetaValue`/`ensureMetaDefault`, i.e. **outside** any transaction.

`ensureMetaValue(ctx, guard *snapshotGuard, key, value string) (bool, error)` (`store.go:1702-1717`): reads current; equal → `(false, nil)` with no write; else `guard.ensure(ctx)` (failure → `fmt.Errorf("ensure meta %s: %w", key, err)`), then `setMeta`, returning `(true, nil)`.

`ensureMetaDefault(ctx, guard, key, value)` (`store.go:1719-1735`): identical except the skip condition is `strings.TrimSpace(current) != ""` — any existing non-blank value is preserved.

`GetSyncState(ctx) (storage.SyncState, error)` (`store.go:442-454`): two `getMeta` reads — `last_sync_path` into `state.Path` and `last_sync_hash` into `state.ContentHash`; on either error returns `(storage.SyncState{}, err)`.

`RecordSyncState(ctx, state storage.SyncState) error` (`store.go:456-468`): one `withMutation(ctx, "record sync state", ...)` that `setMeta`s both keys from a map literal — `last_sync_path: strings.TrimSpace(state.Path)` and `last_sync_hash: strings.TrimSpace(state.ContentHash)` — via the tx. Map iteration order means the two writes are unordered relative to each other.

Round-trip evidence: `store_test.go:1295-1306`.

---

### 10. Branch normalization

`masterRenameSource(ctx, db *sql.DB) (string, error)` (`store.go:2553-2580`), lock-free:
- `SELECT active_branch()`; failure → `fmt.Errorf("query dolt active branch: %w", err)`;
- `SELECT name FROM dolt_branches ORDER BY name`; failure → `fmt.Errorf("query dolt branches: %w", err)`; scan failure → `"scan dolt branch: %w"`; iteration failure → `"iterate dolt branches: %w"`;
- counts branches and notes whether `"master"` exists;
- returns `""` (nothing to rename) when `activeBranch == "master"` **or** master already exists **or** `branchCount != 1`;
- otherwise returns the active branch name.

`ensureMasterDefaultBranch(ctx, db)` (`store.go:2582-2596`): consults `masterRenameSource`; on error or empty answer returns immediately; otherwise runs
```sql
CALL DOLT_BRANCH('-m', '<activeBranch with ' doubled>', 'master')
```
built by `fmt.Sprintf` with `strings.ReplaceAll(activeBranch, "'", "''")` (`store.go:2588-2591`); failure → `fmt.Errorf("rename dolt default branch to master: %w", err)`.

Called on every write open (`store.go:152`) and by the bootstrap (`store.go:2540`).

---

### 11. Cross-file calls made from store.go (noted, not owned here)

| Symbol | Defined at | Called from store.go |
|---|---|---|
| `acquireWorkspaceShared` | `workspace_lock.go:81` | `store.go:107`, `:176`, `:280` |
| `ErrWorkspaceBusy` | `workspace_lock.go:53` | `store.go:2621` |
| `requireNoPendingAdopt` | `adopt.go:124` | `store.go:134`, `:202`, `:292` |
| `withCommitLock` / `withMutation` / `commitWorkingSet` / `isManifestReadOnlyError` | `commit_lock.go:322` / `:122` / `:268` / `:483` | `store.go:151`, `:212`; `:457`, `:509`, `:1115`, `:1153`, `:1172`; `:224` |
| `commitLockPathForDolt` | `commit_lock.go:394` | `store.go:396` |
| `s.migrate` | `migration_runner.go:275` | `store.go:155`, `:212` |
| `snapshotGuard` | `migrate_snapshot.go:109` | `store.go:1702`, `:1719` |
| `newIssueID` | `issue_ids.go:14` | `store.go:522` |
| `canonicalizeLabels` / `replaceLabelsTx` | `labels.go:112` / `:95` | `store.go:482`, `:640`, `:995`; `:552`, `:1052` |
| `insertRelationTx` / `bucketRelations` / `relatedFrom` / `siblingsOf` / `ListChildren` | `relations.go:348` / `:22` / `:64` / `:88` / `:494` | `store.go:548`; `:809`; `:832`; `:820`; `:816` |
| `smoothRanksIfNeededTx` | `ranking.go:401` | `store.go:564` |
| `deleteCommentTx` | `row_deletes.go:93` | `store.go:1189` |
| `rank.Initial/After/Before` | `internal/rank` | `store.go:2065`, `:2067`, `:2081` |

---

### 12. Test-fixture behavior that constrains store.go semantics

- The package builds **two** migrated-store templates via the real `Open` (`fixture_test.go:98-144`), copies them per test (`fixture_test.go:60-72`), and freezes the originals read-only (files `0o444`, dirs `0o555`, `fixture_test.go:139`, `:168-178`) — directories are frozen too because Dolt swaps files via `rename(2)`.
- Slot 1 is asserted to share **no** commit hash with slot 0, i.e. two independently created stores have unrelated Dolt histories (`fixture_test.go:84-93`, evidence read at `fixture_test.go:148-163`).
- Writes through one copy leave no residue in the template or in another copy, and a copy's goose schema version equals a from-scratch `Open`'s (`fixture_residue_test.go:58-129`).
- `TestDoltEngineConformance` runs `internal/storage/conformance.Run` against a store from `openIssueStore` (`conformance_test.go:21-26`), and `TestDoltEngineOffersEveryCapability` asserts `storage.Offered(engine)` equals `storage.Capabilities()` exactly, in order (`conformance_test.go:36-51`).
- `LockDoltJournalExclusive` on an uninitialized workspace refuses with a message containing `"not initialized"` and creates nothing on disk (`dolt_journal_hold_test.go:23-38`).


---

## SQL Schema and Schema Reconciliation — Raw Behavioral Inventory

Every claim below carries a `file:line` citation. All paths are relative to
`/Users/bmf/code/links-issue-tracker`.

The engine is Dolt speaking the MySQL wire protocol
(`internal/store/migration_runner.go:1376-1380`: `goose.NewProvider(goose.DialectMySQL, db, migrations.FS)`).

---

## 1. Where schema comes from

There are exactly four producers of DDL that reaches a live workspace:

| Producer | File | Cited at |
|---|---|---|
| goose baseline migration (v1) | `internal/store/migrations/00001_baseline.sql` | `internal/store/migrations/00001_baseline.sql:39-152` |
| goose numbered migrations v2–v5 | `00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql` | `internal/store/migrations/00002_add_lane.sql:9`, `00003_add_resolution.sql:11,14`, `00004_add_redirect_target.sql:10,18`, `00005_add_event_attribution.sql:15,18` |
| pre-goose reconcile (`reconcileToBaseline`) | `internal/store/schema_reconcile.go` | `internal/store/schema_reconcile.go:164-416` |
| quarantine bootstrap (`ensureQuarantineTable`) | `internal/store/migration_runner.go` | `internal/store/migration_runner.go:657-696` |

Plus goose's own bookkeeping table, created by the goose library's MySQL
dialect (`internal/store/migration_runner.go:217` names it; the DDL text is
goose v3.27.1's, `go.mod:14`).

No other `CREATE TABLE` / `CREATE INDEX` exists in production code — the
repo-wide grep for those statements outside `_test.go`, `.claude/worktrees/`,
and the vendored Dolt driver example returns only the files above
(verified by grep over `internal/` and `cmd/`).

Registry constants:
- Baseline version is `1` (`internal/store/migrations/bounds.go:19`).
- HEAD version is derived at runtime as the max numeric filename prefix in the
  embedded registry (`internal/store/migrations/bounds.go:55-76`); with the
  five files present, HEAD = 5.

---

## 2. THE CONVERGED SCHEMA (v1 + v2..v5), table by table

The authoritative post-migration shape is the byte-compared golden file
`internal/store/schema_snapshot.sql`, which is the verbatim
`SHOW CREATE TABLE` of every table in a freshly-migrated workspace, sorted by
table name, with `goose_db_version` excluded
(`internal/store/schema_drift_test.go:118-166`).

Every application table is `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
COLLATE=utf8mb4_0900_bin` (`internal/store/schema_snapshot.sql:25,34,49,78,89,95,103,118`).
No `CREATE TABLE` statement in the repo declares an engine, charset, or
collation explicitly — these are Dolt's defaults as rendered back by
`SHOW CREATE TABLE`.

No application table uses `AUTO_INCREMENT`
(`internal/store/schema_snapshot.sql:15-118` — no occurrence). The only
`AUTO_INCREMENT` column in the database is `goose_db_version.id` (§2.9), which
is why that table is excluded from the snapshot
(`internal/store/schema_snapshot.sql:12-13`).

### 2.1 `issues`

Effective shape (`internal/store/schema_snapshot.sql:51-78`):

```sql
CREATE TABLE `issues` (
  `id` varchar(191) NOT NULL,
  `title` text NOT NULL,
  `description` text NOT NULL,
  `agent_prompt` text,
  `status` varchar(32),
  `priority` int NOT NULL,
  `issue_type` varchar(32) NOT NULL,
  `topic` varchar(191) NOT NULL,
  `assignee` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `updated_at` varchar(64) NOT NULL,
  `closed_at` varchar(64),
  `archived_at` varchar(64),
  `deleted_at` varchar(64),
  `item_rank` text NOT NULL DEFAULT '',
  `lane` text NOT NULL DEFAULT '',
  `resolution` varchar(32),
  `redirect_target` varchar(191),
  PRIMARY KEY (`id`),
  KEY `idx_issues_rank` (`item_rank`(191)),
  KEY `idx_issues_status_priority` (`status`,`priority`,`updated_at`),
  CONSTRAINT `issues_status_check` CHECK ((((`issue_type` IN ('epic')) AND `status` IS NULL) OR (((NOT((`issue_type` IN ('epic')))) AND (NOT(`status` IS NULL))) AND (`status` IN ('open', 'in_progress', 'closed'))))),
  CONSTRAINT `issues_priority_check` CHECK (((`priority` >= 0) AND (`priority` <= 1))),
  CONSTRAINT `issues_type_check` CHECK ((`issue_type` IN ('task', 'feature', 'bug', 'chore', 'epic'))),
  CONSTRAINT `issues_resolution_check` CHECK ((`resolution` IS NULL OR (`resolution` IN ('duplicate', 'superseded', 'obsolete', 'wontfix')))),
  CONSTRAINT `issues_redirect_target_check` CHECK ((`redirect_target` IS NULL OR (`resolution` IN ('duplicate', 'superseded'))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

Column-by-column provenance and declared form:

| Column | Declared type | NULL | Default | Declared at |
|---|---|---|---|---|
| `id` | `VARCHAR(191)` | NOT NULL (PRIMARY KEY) | none | `00001_baseline.sql:49` |
| `title` | `TEXT` | NOT NULL | none | `00001_baseline.sql:50` |
| `description` | `TEXT` | NOT NULL | none | `00001_baseline.sql:51` |
| `agent_prompt` | `TEXT` | NULL | none | `00001_baseline.sql:52` |
| `status` | `VARCHAR(32)` | NULL | none | `00001_baseline.sql:53` |
| `priority` | `INT` | NOT NULL | none | `00001_baseline.sql:54` |
| `issue_type` | `VARCHAR(32)` | NOT NULL | none | `00001_baseline.sql:55` |
| `topic` | `VARCHAR(191)` | NOT NULL | none | `00001_baseline.sql:56` |
| `assignee` | `TEXT` | NOT NULL | none | `00001_baseline.sql:57` |
| `created_at` | `VARCHAR(64)` | NOT NULL | none | `00001_baseline.sql:58` |
| `updated_at` | `VARCHAR(64)` | NOT NULL | none | `00001_baseline.sql:59` |
| `closed_at` | `VARCHAR(64)` | NULL | none | `00001_baseline.sql:60` |
| `archived_at` | `VARCHAR(64)` | NULL | none | `00001_baseline.sql:61` |
| `deleted_at` | `VARCHAR(64)` | NULL | none | `00001_baseline.sql:62` |
| `item_rank` | `TEXT` | NOT NULL | `''` | `00001_baseline.sql:63` |
| `lane` | `text` | NOT NULL | `''` | `00002_add_lane.sql:9` |
| `resolution` | `VARCHAR(32)` | NULL | none | `00003_add_resolution.sql:11` |
| `redirect_target` | `VARCHAR(191)` | NULL | none | `00004_add_redirect_target.sql:10` |

- PRIMARY KEY: `(id)` — declared inline as `PRIMARY KEY` on the column (`00001_baseline.sql:49`).
- No UNIQUE constraints.
- Secondary indexes:
  - `idx_issues_status_priority (status, priority, updated_at)` — `00001_baseline.sql:130`, also created by reconcile at `internal/store/schema_reconcile.go:205`.
  - `idx_issues_rank (item_rank(191))` — prefix index of length 191 on a `TEXT` column — `00001_baseline.sql:133`, also `internal/store/schema_reconcile.go:325`.
- No FOREIGN KEYs on `issues`. `redirect_target` deliberately has **no** FK to `issues(id)` (`00004_add_redirect_target.sql:61-63`).
- CHECK constraints (five, all explicitly named so `SHOW CREATE TABLE` is deterministic — `00001_baseline.sql:21-24`):
  - `issues_status_check` — `00001_baseline.sql:64`; the identical clause is generated in Go at `internal/store/schema_reconcile.go:105-107` and installed by `ensureStatusConstraint` at `internal/store/schema_reconcile.go:1169`.
  - `issues_priority_check` — `00001_baseline.sql:65`; Go form at `internal/store/schema_reconcile.go:77`, installed at `internal/store/schema_reconcile.go:1107`.
  - `issues_type_check` — `00001_baseline.sql:66`; Go form at `internal/store/schema_reconcile.go:96`.
  - `issues_resolution_check` — `00003_add_resolution.sql:14`.
  - `issues_redirect_target_check` — `00004_add_redirect_target.sql:18`.

Note the ordering difference: the file declares the indexes as
`idx_issues_status_priority` then `idx_issues_rank`
(`00001_baseline.sql:130,133`), but `SHOW CREATE TABLE` renders them
alphabetically, `idx_issues_rank` first
(`internal/store/schema_snapshot.sql:71-72`).

### 2.2 `relations`

```sql
CREATE TABLE `relations` (
  `src_id` varchar(191) NOT NULL,
  `dst_id` varchar(191) NOT NULL,
  `type` varchar(32) NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`src_id`,`dst_id`,`type`),
  KEY `dst_id` (`dst_id`),
  KEY `idx_relations_dst_type` (`dst_id`,`type`),
  KEY `idx_relations_src_type` (`src_id`,`type`),
  CONSTRAINT `relations_ibfk_1` FOREIGN KEY (`src_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `relations_ibfk_2` FOREIGN KEY (`dst_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `relations_type_check` CHECK ((`type` IN ('blocks', 'parent-child', 'related-to')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:105-118`. Source declaration:
`00001_baseline.sql:71-81`; reconcile's identical copy:
`internal/store/schema_reconcile.go:178-188`.

- PRIMARY KEY `(src_id, dst_id, type)` — `00001_baseline.sql:77`.
- No UNIQUE constraints.
- Indexes: `idx_relations_src_type (src_id, type)` (`00001_baseline.sql:136`; reconcile `schema_reconcile.go:206`), `idx_relations_dst_type (dst_id, type)` (`00001_baseline.sql:139`; reconcile `schema_reconcile.go:207`), plus the Dolt-auto-generated FK backing index `KEY dst_id (dst_id)` (`internal/store/schema_snapshot.sql:112`) — no such statement exists in any migration file; it materializes from the `dst_id` FK.
- FOREIGN KEYs, both `ON DELETE CASCADE`, no `ON UPDATE` clause declared: `src_id → issues(id)` and `dst_id → issues(id)` (`00001_baseline.sql:78-79`), auto-named `relations_ibfk_1` / `relations_ibfk_2` (`internal/store/schema_snapshot.sql:115-116`).
- CHECK `relations_type_check` on `type IN ('blocks','parent-child','related-to')` (`00001_baseline.sql:80`).

### 2.3 `comments`

```sql
CREATE TABLE `comments` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `body` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_comments_issue_created` (`issue_id`,`created_at`),
  KEY `issue_id` (`issue_id`),
  CONSTRAINT `comments_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:15-25`. Source: `00001_baseline.sql:85-92`;
reconcile copy: `internal/store/schema_reconcile.go:189-196`.

- PRIMARY KEY `(id)` — `00001_baseline.sql:86`.
- Index `idx_comments_issue_created (issue_id, created_at)` — `00001_baseline.sql:142`; reconcile `schema_reconcile.go:208`.
- Auto FK-backing index `KEY issue_id (issue_id)` (`internal/store/schema_snapshot.sql:23`), not declared anywhere.
- FK `issue_id → issues(id) ON DELETE CASCADE` (`00001_baseline.sql:91`), auto-named `comments_ibfk_1`.
- No CHECK, no UNIQUE.

### 2.4 `labels`

```sql
CREATE TABLE `labels` (
  `issue_id` varchar(191) NOT NULL,
  `label` varchar(191) NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`issue_id`,`label`),
  KEY `idx_labels_issue` (`issue_id`,`label`),
  KEY `idx_labels_name` (`label`,`issue_id`),
  CONSTRAINT `labels_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:80-89`. Source: `00001_baseline.sql:96-103`;
reconcile copy: `internal/store/schema_reconcile.go:197-204`.

- PRIMARY KEY `(issue_id, label)` — `00001_baseline.sql:101`.
- Indexes `idx_labels_issue (issue_id, label)` (`00001_baseline.sql:145`; reconcile `:209`) and `idx_labels_name (label, issue_id)` (`00001_baseline.sql:148`; reconcile `:210`).
- No separate auto FK index appears — the PK's leading `issue_id` covers it (`internal/store/schema_snapshot.sql:85-87`).
- FK `issue_id → issues(id) ON DELETE CASCADE` (`00001_baseline.sql:102`), auto-named `labels_ibfk_1`.

### 2.5 `issue_events`

```sql
CREATE TABLE `issue_events` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `action` varchar(64),
  `reason` text NOT NULL,
  `actor` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `stream_id` varchar(64),
  `workspace_id` varchar(191),
  PRIMARY KEY (`id`),
  KEY `idx_issue_events_issue_created` (`issue_id`,`created_at`),
  KEY `issue_id` (`issue_id`),
  CONSTRAINT `issue_events_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:36-49`. Baseline columns:
`00001_baseline.sql:107-115`; reconcile copy:
`internal/store/schema_reconcile.go:218-226`.
`stream_id VARCHAR(64) NULL` added at `00005_add_event_attribution.sql:15`;
`workspace_id VARCHAR(191) NULL` at `00005_add_event_attribution.sql:18`.

- PRIMARY KEY `(id)` — `00001_baseline.sql:108`.
- Index `idx_issue_events_issue_created (issue_id, created_at)` — `00001_baseline.sql:151`; reconcile `schema_reconcile.go:235`.
- Auto FK-backing index `KEY issue_id (issue_id)` (`internal/store/schema_snapshot.sql:47`).
- FK `issue_id → issues(id) ON DELETE CASCADE` (`00001_baseline.sql:114`), auto-named `issue_events_ibfk_1`.
- No CHECK, no UNIQUE.
- Historical column name: `assignee` was the pre-v1 name of `actor`; reconcile renames it (`internal/store/schema_reconcile.go:280`), and the shapemap records both spellings mapping to the same domain field (`internal/store/shapemap_known.go:206-207`).

### 2.6 `issue_event_changes`

```sql
CREATE TABLE `issue_event_changes` (
  `event_id` varchar(191) NOT NULL,
  `field` varchar(64) NOT NULL,
  `from_value` text,
  `to_value` text,
  PRIMARY KEY (`event_id`,`field`),
  CONSTRAINT `issue_event_changes_ibfk_1` FOREIGN KEY (`event_id`) REFERENCES `issue_events` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:27-34`. Source: `00001_baseline.sql:119-126`;
reconcile copy: `internal/store/schema_reconcile.go:227-234`.

- PRIMARY KEY `(event_id, field)` — `00001_baseline.sql:124`.
- No secondary index (the PK's leading `event_id` covers the FK — no `KEY event_id` row appears, `internal/store/schema_snapshot.sql:32-33`).
- FK `event_id → issue_events(id) ON DELETE CASCADE` (`00001_baseline.sql:125`), auto-named `issue_event_changes_ibfk_1`.

### 2.7 `meta`

```sql
CREATE TABLE `meta` (
  `meta_key` varchar(191) NOT NULL,
  `meta_value` text NOT NULL,
  PRIMARY KEY (`meta_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:91-95`. Source: `00001_baseline.sql:41-44`;
reconcile copy: `internal/store/schema_reconcile.go:173-176` (the **first**
step in reconcile's list).

- PRIMARY KEY `(meta_key)`; no indexes, no FKs, no CHECKs.
- Written through `INSERT ... ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)` (`internal/store/store.go:1697-1698`); read at `internal/store/store.go:1671-1673`, absent key yields `""` not an error (`internal/store/store.go:1678-1680`).
- Keys observed in production code:
  - `workspace_id` — written by reconcile via `ensureMetaValue` (`internal/store/schema_reconcile.go:410`).
  - `producer_binary_version` — const at `internal/store/migration_runner.go:29`, written at `internal/store/migration_runner.go:1623`, read at `internal/store/migration_runner.go:1600` and via a Dolt `AS OF` query at `internal/store/sync_schema_guard.go:186`.
  - `last_sync_path`, `last_sync_hash` — read at `internal/store/store.go:445,449`, written at `internal/store/store.go:459-462`.

### 2.8 `migration_quarantine`

```sql
CREATE TABLE `migration_quarantine` (
  `version` bigint NOT NULL,
  `name` text NOT NULL,
  `error_text` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  PRIMARY KEY (`version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql:97-103`. The DDL executed is the Go
constant at `internal/store/migration_runner.go:657-663` — declared as
`version BIGINT NOT NULL`, `name TEXT NOT NULL`, `error_text TEXT NOT NULL`,
`created_at VARCHAR(64) NOT NULL`, `PRIMARY KEY (version)`.

- Created **outside** the goose batch, before the snapshot guard, so a goose rollback cannot erase it (`internal/store/migration_runner.go:363-378`).
- Canonical column set pinned in Go: `{"version","name","error_text","created_at"}` (`internal/store/migration_runner.go:669`).
- Not defined in any migration file — grep of `internal/store/migrations/*.sql` shows no `migration_quarantine`.

### 2.9 `goose_db_version`

Created by the goose library, not by this repo. MySQL dialect DDL
(`/Users/bmf/go/pkg/mod/github.com/pressly/goose/v3@v3.27.1/internal/dialects/mysql.go:18-27`,
selected by `internal/store/migration_runner.go:1379`):

```sql
CREATE TABLE goose_db_version (
  id bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  version_id bigint NOT NULL,
  is_applied boolean NOT NULL,
  tstamp timestamp NULL default now(),
  PRIMARY KEY(id)
)
```

- Table name constant: `internal/store/migration_runner.go:217`.
- Rows are inserted as `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, ?)` (`.../mysql.go:29-32`).
- Excluded from the snapshot golden file because it is bookkeeping and its `AUTO_INCREMENT` counter is nondeterministic (`internal/store/schema_snapshot.sql:12-13`; enforced in code at `internal/store/schema_drift_test.go:156-158`).
- Classified as a bookkeeping table with no domain mapping (`internal/store/shapemap_known.go:225`).

### 2.10 `issue_history` — the legacy table that no longer exists

Never created by any current producer. Its presence is the disk-truth marker
of a pre-goose workspace (`internal/store/migration_runner.go:881-887`), and
reconcile drops it (`internal/store/schema_reconcile.go:305-311`). The column
set the translation requires is
`{id, issue_id, action, reason, from_status, to_status, created_at, created_by}`
(`internal/store/schema_reconcile.go:497-499`); the test fixture that
reproduces the historical shape declares
`id VARCHAR(191) PRIMARY KEY, issue_id VARCHAR(191) NOT NULL,
action VARCHAR(64) NULL, reason TEXT NULL, from_status VARCHAR(32) NULL,
to_status VARCHAR(32) NULL, created_at VARCHAR(64) NOT NULL,
created_by TEXT NOT NULL`
(`internal/store/schema_reconcile_test.go:605-614`).

---

## 3. Baseline-only shape (what v1 alone produces)

`baselineSchema()` parses the embedded `00001_baseline.sql` Up section into a
table→columns map (`internal/store/migration_runner.go:1392-1406`), reading
only the section between `-- +goose up` and `-- +goose down`
(`internal/store/migration_runner.go:1410-1421`) and only the first identifier
of each top-level comma-separated item that is not a constraint keyword
(`internal/store/migration_runner.go:1454-1464`). `CREATE INDEX` statements are
ignored entirely (`internal/store/migration_runner.go:1423-1427`).

The exact parsed result is pinned by test
(`internal/store/baseline_schema_test.go:25-33`):

```
meta:                meta_key, meta_value
issues:              id, title, description, agent_prompt, status, priority,
                     issue_type, topic, assignee, created_at, updated_at,
                     closed_at, archived_at, deleted_at, item_rank
relations:           src_id, dst_id, type, created_at, created_by
comments:            id, issue_id, body, created_at, created_by
labels:              issue_id, label, created_at, created_by
issue_events:        id, issue_id, action, reason, actor, created_at
issue_event_changes: event_id, field, from_value, to_value
```

Exactly seven tables, and no table-level constraint clause may leak in as a
pseudo-column (`internal/store/baseline_schema_test.go:35-37,13-17`).

---

## 4. The baseline file is byte-frozen

`00001_baseline.sql` is pinned by SHA-256:
`e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb`
(`internal/store/migrations/baseline_frozen_test.go:32`). `TestBaselineFileIsFrozen`
reads the embedded file and compares
(`internal/store/migrations/baseline_frozen_test.go:37-46`). The failure
message forbids updating the constant and forbids even comment/whitespace
edits (`internal/store/migrations/baseline_frozen_test.go:47-71`).

This hash is the only content fingerprint in the schema system. There is **no**
schema-version hash or fingerprint stored in the database. The recorded schema
version in the database is the integer `version_id` in `goose_db_version`
(§2.9), plus the `producer_binary_version` string row in `meta` (§2.7).

---

## 5. `reconcileToBaseline` — the pre-goose → v1 forward migrator

Entry point: `internal/store/schema_reconcile.go:164`. Runs only in
`phaseAdopt`, after the snapshot guard fires, before the v1 stamp
(`internal/store/migration_runner.go:410-424`).

Signature returns `(changed bool, err error)`; `changed` is true iff any step
performed a write (`internal/store/schema_reconcile.go:164,237-243`).

### 5.1 Phase classification that decides whether reconcile runs at all

`classifyMigrationState` (`internal/store/migration_runner.go:861-907`):

1. Read registry max version (`:862`).
2. If table `issue_history` exists → `phaseAdopt` unconditionally, regardless of `goose_db_version` (`internal/store/migration_runner.go:881-887`).
3. Else if `goose_db_version` exists → `phaseManaged` with the recorded version (`internal/store/migration_runner.go:888-898`).
4. Else run `verifyBaselineShape`; if `present == 0` → `phaseFresh`, otherwise `phaseAdopt` (`internal/store/migration_runner.go:899-906`).

So: **empty database** → `phaseFresh` → reconcile never runs; goose applies
`00001_baseline.sql` then v2..v5. **Populated pre-goose database** (any
canonical table present, no goose log) → `phaseAdopt` → reconcile runs.

### 5.2 The step ordering, exactly

Ordered list, all inside `reconcileToBaseline`:

**Stage A — declarative DDL list** (`internal/store/schema_reconcile.go:172-236`), run in slice order by the loop at `:238-244`:

1. `CREATE TABLE meta` (`:173-176`)
2. `CREATE TABLE issues` via `createIssuesTableStmt()` (`:177`, body at `:126-147`)
3. `CREATE TABLE relations` (`:178-188`)
4. `CREATE TABLE comments` (`:189-196`)
5. `CREATE TABLE labels` (`:197-204`)
6. `CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at)` (`:205`)
7. `CREATE INDEX idx_relations_src_type ON relations(src_id, type)` (`:206`)
8. `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type)` (`:207`)
9. `CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at)` (`:208`)
10. `CREATE INDEX idx_labels_issue ON labels(issue_id, label)` (`:209`)
11. `CREATE INDEX idx_labels_name ON labels(label, issue_id)` (`:210`)
12. `CREATE TABLE issue_events` (`:218-226`)
13. `CREATE TABLE issue_event_changes` (`:227-234`)
14. `CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at)` (`:235`)

Note the FK-correct ordering: `issues` precedes `relations`/`comments`/`labels`/`issue_events`;
`issue_events` precedes `issue_event_changes`. `meta` (no FKs) is first.

`createIssuesTableStmt()` emits the v1 issues shape **without** `lane`,
`resolution`, or `redirect_target` (`internal/store/schema_reconcile.go:127-146`)
— those arrive later from goose migrations v2–v4 after adoption stamps v1.

**Stage B — mutations, in this exact order:**

15. Drop `goose_db_version` if present — `DROP TABLE goose_db_version`, label `"drop fabricated goose_db_version (legacy workspace carried lying bookkeeping)"` (`internal/store/schema_reconcile.go:260-270`).
16. Rename `issue_events.assignee` → `actor` if the `assignee` column exists — `ALTER TABLE issue_events RENAME COLUMN assignee TO actor` (`internal/store/schema_reconcile.go:276-286`). Must precede step 17 (`:271-275`).
17. `translateIssueHistoryToEvents` (`internal/store/schema_reconcile.go:293-297`, implementation `:541-683`) — §5.5.
18. Drop `issue_history` if present — `DROP TABLE IF EXISTS issue_history`, label `"drop legacy issue_history table"` (`internal/store/schema_reconcile.go:305-315`).
19. Add `issues.item_rank` if missing — `ALTER TABLE issues ADD COLUMN item_rank TEXT NOT NULL DEFAULT ''` (`internal/store/schema_reconcile.go:316-321`).
20. Create index `idx_issues_rank ON issues(item_rank(191))` if absent (`internal/store/schema_reconcile.go:322-330`).
21. Add `issues.topic` if missing — `ALTER TABLE issues ADD COLUMN topic VARCHAR(191) NOT NULL DEFAULT 'misc' AFTER issue_type` (`internal/store/schema_reconcile.go:331-336`).
22. Drop that default if `column_default IS NOT NULL` — `ALTER TABLE issues MODIFY topic VARCHAR(191) NOT NULL`, label `"drop topic default to match baseline shape"` (`internal/store/schema_reconcile.go:346-356`). Rationale: baseline declares `topic` with no default, so a reconcile-built column would otherwise differ (`:337-345`).
23. Rename `issues.prompt` → `agent_prompt` if the `prompt` column exists — ``ALTER TABLE issues RENAME COLUMN `prompt` TO agent_prompt`` (`internal/store/schema_reconcile.go:363-373`); `prompt` is backtick-quoted because it is reserved in Dolt's MySQL parser (`:358-362`).
24. Add `issues.agent_prompt` if missing — ``ALTER TABLE issues ADD COLUMN agent_prompt TEXT NULL AFTER `description` `` (`internal/store/schema_reconcile.go:374-379`).
25. Relax `issues.agent_prompt` to nullable if `is_nullable='NO'` — `ALTER TABLE issues MODIFY agent_prompt TEXT NULL` (`internal/store/schema_reconcile.go:384-389`).
26. `ensureUnifiedStatusSchema` (`internal/store/schema_reconcile.go:390-394`, implementation `:901-985`) — §5.6.
27. `ensureIssueTopics` (`internal/store/schema_reconcile.go:395-399`, implementation `:987-997`) — §5.7.
28. `ensureIssueRanks` (`internal/store/schema_reconcile.go:400-404`, implementation `:999-1076`) — §5.8.
29. `resetPrioritiesToNormal` (`internal/store/schema_reconcile.go:405-409`, implementation `:1088-1111`) — §5.9.
30. `ensureMetaValue(ctx, guard, "workspace_id", s.workspaceID)` (`internal/store/schema_reconcile.go:410-414`; helper at `internal/store/store.go:1702-1717`).

Any step returning an error aborts immediately, returning the `changed` value
accumulated so far (`internal/store/schema_reconcile.go:240-242` and each
subsequent `if err != nil { return changed, err }` block).

### 5.3 How each step decides skip-vs-execute (the drift-detection primitives)

Three gate helpers, all built on `probeYields`:

- `probeYields(ctx, probe, label)` (`internal/store/schema_reconcile.go:884-894`): runs the probe as `QueryRow(...).Scan(&int)`. `err == nil` → true; `sql.ErrNoRows` → false; any other driver error → `fmt.Errorf("%s: probe: %w", label, err)`.

- `execGatedCreate(ctx, guard, probe, stmt, label)` (`internal/store/schema_reconcile.go:830-849`): if probe yields → skip, return `(false, nil)`. Otherwise `guard.ensure(ctx)`, then `ExecContext(stmt)`. On exec error the message is lowercased and if it contains `"already exists"`, `"duplicate column"`, or `"duplicate key name"` the error is **swallowed** and `(false, nil)` returned; any other error → `fmt.Errorf("%s: %w", label, err)`. A `guard.ensure` failure → `fmt.Errorf("%s: %w", label, snapErr)`.

- `execGatedMutation(ctx, guard, probe, stmt, label)` (`internal/store/schema_reconcile.go:864-879`): if probe does **not** yield → skip. Otherwise `guard.ensure`, then exec; **no swallow** — any exec error becomes `fmt.Errorf("%s: %w", label, err)`.

Probe SQL by step class:

- **Table existence** (`ddlStep` with empty `parent`) — `SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '<target>' LIMIT 1` (`internal/store/schema_reconcile.go:60-64`).
- **Index existence** (`ddlStep` with non-empty `parent`) — `SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = '<parent>' AND index_name = '<target>' LIMIT 1` (`internal/store/schema_reconcile.go:66-69`).
- **Column presence** (`execGatedColumnAdd`) — `SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '<t>' AND column_name = '<c>' LIMIT 1`, label `"add column <t>.<c>"` (`internal/store/schema_reconcile.go:796-803`).
- **Column nullability** (`execGatedColumnRelax`) — same query plus `AND is_nullable = 'NO'`, label `"relax column <t>.<c> to nullable"`, routed through `execGatedMutation` (propagate, not swallow) (`internal/store/schema_reconcile.go:810-817`).
- **Column default presence** — `... AND column_name = 'topic' AND column_default IS NOT NULL LIMIT 1` (`internal/store/schema_reconcile.go:349`).
- **CHECK-constraint shape** — see §5.9/§5.10.
- **Row-level data predicates** — plain `SELECT 1 FROM issues WHERE <predicate> LIMIT 1` (`internal/store/schema_reconcile.go:923,928,933,938,943,953,967,993`).

Explicitly **not** compared anywhere in reconcile: column SQL type, column
length/precision, index column list, index uniqueness, foreign-key presence or
actions, charset/collation, engine. The only type-shaped things compared are
(a) nullability of two specific columns, (b) the presence of a default on
`issues.topic`, (c) the normalized text of `issues` CHECK clauses. A table that
exists with wrong columns is skipped by the CREATE step
(`internal/store/schema_reconcile_test.go:1416-1423`); the post-reconcile
baseline verification is the net that catches it (§5.11).

### 5.4 The `ddlStep` type

`type ddlStep struct { target, parent, stmt string }`
(`internal/store/schema_reconcile.go:49-53`); `parent` empty means CREATE
TABLE, non-empty names the table an index lives on
(`internal/store/schema_reconcile.go:46-48`). `runGatedCreate` derives the
probe from the step and labels it `"create <target>"`
(`internal/store/schema_reconcile.go:788-791`).

### 5.5 `translateIssueHistoryToEvents`

`internal/store/schema_reconcile.go:541-683`.

Preconditions, in order:

1. `tableExists("issue_history")`; false → `(false, nil)` (`:542-548`). Probe error → `"translate issue_history: probe table: %w"` (`:544`).
2. `tableColumns("issue_history")`; error → `"translate issue_history: probe columns: %w"` (`:551`). If any of `id, issue_id, action, reason, from_status, to_status, created_at, created_by` (`:497-499`) is absent → `(false, nil)`, table left for the drop step (`:553-559`).
3. Existence pre-check (no snapshot taken) — `SELECT 1 FROM issue_history h WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = h.issue_id) AND NOT EXISTS (SELECT 1 FROM issue_events e WHERE e.id = h.id) LIMIT 1`, label `"translate issue_history: pending probe"` (`:568-574`). No rows → `(false, nil)` (`:578-580`).

Then `guard.ensure` (error → `"translate issue_history: %w"`, `:581-583`),
`BeginTx` (error → `"translate issue_history: begin tx: %w"`, `:584-587`), with
a deferred `tx.Rollback()` (`:594`).

Inside the tx it SELECTs `h.id, h.issue_id, h.action, h.reason, h.created_by,
h.created_at, h.from_status, h.to_status` under the same
EXISTS/NOT-EXISTS filter (`:609-622`), buffers all rows into a slice
(`:633-645`), and if the buffer is empty returns `(false, nil)` (`:646-648`).

Prepared statements:
- `INSERT INTO issue_events (id, issue_id, action, reason, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)` (`:649`)
- `INSERT INTO issue_event_changes (event_id, field, from_value, to_value) VALUES (?, 'status', ?, ?)` (`:654`)

Per row (`:659-678`): insert the event with canonicalized `action`, `reason`,
`actor`; then normalize both statuses and, if `isLegacyStatusTransition`, insert
one `issue_event_changes` row with `field = 'status'`.

Canonicalization functions:
- `canonicalEventAction` — NULL → `nil`; TrimSpace; empty → `nil`; else trimmed (`internal/store/schema_reconcile.go:690-699`).
- `canonicalEventReason` — NULL → `""`; else TrimSpace (`:703-708`).
- `canonicalEventActor` — NULL → `"unknown"`; TrimSpace; empty → `"unknown"`; else trimmed (`:713-722`).
- `canonicalLegacyStatus` — NULL stays NULL; `open`/`in_progress`/`closed` pass through; `in-progress`→`in_progress`; `todo`→`open`; `done`→`closed`; anything else → `open` (`:741-757`).
- `isLegacyStatusTransition` — both NULL → false; both valid and equal → false; otherwise true (`:765-773`).
- `nullableSQLString` — invalid → `nil`, valid → the string (`:777-782`).

Commit error → `"translate issue_history: commit tx: %w"` (`:679-681`);
success returns `(true, nil)` (`:682`).

### 5.6 `ensureUnifiedStatusSchema`

`internal/store/schema_reconcile.go:901-985`.

1. Relax `issues.status` to nullable if `is_nullable='NO'` — `ALTER TABLE issues MODIFY status VARCHAR(32) NULL` (`:911-916`).
2. Seven probe/UPDATE pairs, in list order (`:917-971`), each run via `execGatedMutation` (`:972-978`):

| # | Probe | Statement | Label / context |
|---|---|---|---|
| 1 | `SELECT 1 FROM issues WHERE status = 'in-progress' LIMIT 1` | `UPDATE issues SET status = 'in_progress' WHERE status = 'in-progress'` | `normalize legacy in-progress status` (`:923-925`) |
| 2 | `... WHERE status = 'todo' LIMIT 1` | `UPDATE issues SET status = 'open' WHERE status = 'todo'` | `normalize legacy todo status` (`:928-930`) |
| 3 | `... WHERE status = 'done' LIMIT 1` | `UPDATE issues SET status = 'closed' WHERE status = 'done'` | `normalize legacy done status` (`:933-935`) |
| 4 | `... WHERE status NOT IN ('open','in_progress','closed') LIMIT 1` | `UPDATE issues SET status = 'open' WHERE status NOT IN ('open','in_progress','closed')` | `normalize invalid status` (`:938-940`) |
| 5 | `... WHERE closed_at IS NOT NULL AND status <> 'closed' LIMIT 1` | `UPDATE issues SET status = 'closed' WHERE closed_at IS NOT NULL AND status <> 'closed'` | `normalize closed_at status` (`:943-945`) |
| 6 | `... WHERE status <> 'closed' AND closed_at IS NOT NULL LIMIT 1` | `UPDATE issues SET closed_at = NULL WHERE status <> 'closed' AND closed_at IS NOT NULL` | `normalize non-closed closed_at` (`:953-955`) |
| 7 | `SELECT 1 FROM issues WHERE issue_type IN ('epic') AND status IS NOT NULL LIMIT 1` | `UPDATE issues SET status = NULL WHERE issue_type IN ('epic') AND status IS NOT NULL` | `null out container status` (`:967-969`) |

Pair 7's `issue_type IN ('epic')` text is generated from
`model.ContainerTypes()` (`internal/store/schema_reconcile.go:97`).

3. `ensureStatusConstraint` (`:979-983`) — §5.10.

### 5.7 `ensureIssueTopics`

`internal/store/schema_reconcile.go:987-997`. One gated mutation:
probe `SELECT 1 FROM issues WHERE TRIM(COALESCE(topic, '')) = '' LIMIT 1`,
statement `UPDATE issues SET topic = 'misc' WHERE TRIM(COALESCE(topic, '')) = ''`,
label `"backfill legacy issue topics"`.

### 5.8 `ensureIssueRanks`

`internal/store/schema_reconcile.go:999-1076`.

- Query: `SELECT id FROM issues WHERE item_rank = '' ORDER BY status ASC, priority ASC, updated_at DESC, id ASC` (`:1003`). Errors: `"ensureIssueRanks: query unranked: %w"` (`:1005`), `"ensureIssueRanks: scan: %w"` (`:1012`), `"ensureIssueRanks: rows: %w"` (`:1017`).
- Zero unranked rows → `(false, nil)` (`:1019-1021`).
- `guard.ensure` error → `"ensureIssueRanks: %w"` (`:1023`).
- `BeginTx` error → `"ensureIssueRanks: begin tx: %w"` (`:1035`); deferred rollback at `:1037`.
- Seed: `SELECT MAX(item_rank) FROM issues WHERE item_rank != ''` (`:1046`); error → `"ensureIssueRanks: read max existing rank: %w"` (`:1048`). If a max exists and is non-empty, `current = rank.After(max)`, else `current = rank.Initial()` (`:1050-1053`).
- Prepared `UPDATE issues SET item_rank = ? WHERE id = ?` (`:1061`); prepare error → `"ensureIssueRanks: prepare: %w"` (`:1063`); per-row exec error → `"ensureIssueRanks: update %s: %w"` (`:1068`); `current = rank.After(current)` after each (`:1070`).
- Commit error → `"ensureIssueRanks: commit tx: %w"` (`:1073`); success `(true, nil)` (`:1075`).

### 5.9 `resetPrioritiesToNormal`

`internal/store/schema_reconcile.go:1088-1111`.

- `listIssuePriorityCheckConstraints` (`:1113-1140`) queries
  ```sql
  SELECT tc.constraint_name, cc.check_clause
  FROM information_schema.table_constraints tc
  JOIN information_schema.check_constraints cc
    ON tc.constraint_schema = cc.constraint_schema
   AND tc.constraint_name = cc.constraint_name
  WHERE tc.table_schema = DATABASE()
    AND tc.table_name = 'issues'
    AND tc.constraint_type = 'CHECK'
  ```
  (`:1114-1121`) and keeps rows whose normalized clause contains the substring `"priority"` (`:1132-1134`). Errors: `"query issue check constraints: %w"` (`:1123`), `"scan issue check constraint: %w"` (`:1130`), `"iterate issue check constraints: %w"` (`:1137`).
- `hasCanonicalPriorityConstraint` (`:1142-1151`): requires **exactly one** matching constraint, and its normalized clause must contain `"priority<=1"` (formatted from `model.PriorityUrgent`). If true → skip, `(false, nil)` (`:1093-1095`).
- Otherwise: `guard.ensure` (error → `"reset priorities to normal: %w"`, `:1096-1098`); `UPDATE issues SET priority = 0` (error → `"reset priorities to normal: %w"`, `:1099-1101`); for every matched constraint `ALTER TABLE issues DROP CHECK \`<name>\`` with backticks in the name doubled (error → `"drop priority check %s: %w"`, `:1102-1106`); then `ALTER TABLE issues ADD CONSTRAINT issues_priority_check CHECK (priority >= 0 AND priority <= 1)` (error → `"add priority check: %w"`, `:1107-1109`). Returns `(true, nil)`.

### 5.10 `ensureStatusConstraint`

`internal/store/schema_reconcile.go:1153-1173`.

- `listIssueStatusCheckConstraints` (`:1175-1203`) runs the same
  `information_schema` join as §5.9 (`:1176-1183`) and keeps rows whose
  normalized clause contains `"statusin("` (`:1194-1197`). Same three error
  strings as §5.9 (`:1185`, `:1191`, `:1200`).
- `hasCanonicalStatusConstraint` (`:1205-1238`) requires **exactly one** constraint and all five of these on the normalized clause:
  1. contains `issue_typein('epic')` (`:1222`)
  2. contains `statusin('open','in_progress','closed')` (`:1225`)
  3. contains `statusisnotnull` **or** `not(statusisnull)` (`:1228`)
  4. contains `andstatusisnull` (`:1231`)
  5. `hasNegatedEpicGuard` is true (`:1234`)
- `hasNegatedEpicGuard` (`:1245-1266`): true if the clause contains `issue_typenotin('epic')`, or if any occurrence of `not(` — after skipping any run of further `(` characters — is immediately followed by `issue_typein('epic')`.
- `normalizeConstraintClause` (`:1268-1271`): strips spaces, tabs, newlines, and backticks, then lowercases.
- If not canonical: `guard.ensure` (error → `"ensure status constraint: %w"`, `:1161-1163`); drop each matched constraint via `ALTER TABLE issues DROP CHECK \`<name>\`` with doubled backticks (error → `"drop status check %s: %w"`, `:1164-1168`); then `ALTER TABLE issues ADD CONSTRAINT issues_status_check CHECK ((issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed')))` (error → `"add canonical status check: %w"`, `:1169-1171`).

The tolerance for Dolt's rewriting (`NOT IN` → `NOT(... IN ...)`,
`IS NOT NULL` → `NOT(... IS NULL)`, added backticks) is documented at
`internal/store/schema_reconcile.go:1209-1216` and visible in the snapshot's
rendered clause (`internal/store/schema_snapshot.sql:73`).

### 5.11 Pre- and post-reconcile gates

**Pre-gate — `verifyIssuesReconcilable`** (`internal/store/schema_reconcile.go:454-482`),
called from `runMigration` before the snapshot guard, only in `phaseAdopt`
(`internal/store/migration_runner.go:397-401`):

- Reads `tableColumns("issues")`. Zero columns (table absent) → `nil` (reconcile will CREATE it) (`:459-463`).
- Required set: `{"status","priority","updated_at","issue_type","closed_at","description"}` (`internal/store/schema_reconcile.go:452`).
- Any missing → error text:
  ```
  workspace's issues table is missing reconcile prerequisites (<comma-joined missing>); the shape is structurally beyond what pre-goose reconcile can recover — this is not a known historical shape
  ```
  (`internal/store/schema_reconcile.go:474-479`). Wrapped by the caller as `"reconcile pre-goose workspace: %w"` (`internal/store/migration_runner.go:399`).

**Post-gate — `verifyBaselineShape`** (`internal/store/migration_runner.go:1311-1333`):
for each baseline table (sorted), read `tableColumns`; absent table appends the
table name to `missing`; present table increments `present` and appends
`table.column` for each baseline column not found. Column names are lowercased
on read (`internal/store/migration_runner.go:1352`). It compares **column
presence only** — never types, nullability, defaults, indexes, or keys.

Called after reconcile at `internal/store/migration_runner.go:437-449`; any
remaining gap aborts before the stamp with:
```
post-reconcile workspace shape still differs from baseline (remaining gaps: <comma-joined>); reconcile cannot bring this workspace to v1 — the shape is structurally beyond what pre-goose reconcile can recover
```
(`internal/store/migration_runner.go:442-448`). Wrapping error for a probe
failure: `"verify post-reconcile baseline shape: %w"`
(`internal/store/migration_runner.go:439`). The whole reconcile call is
wrapped `"reconcile pre-goose workspace: %w"`
(`internal/store/migration_runner.go:423`).

`refuseIfBaselineMissing` (`internal/store/migration_runner.go:821-836`) uses
the same shape check when the goose log records a version above the registry
max, returning `UnsupportedSchemaVersionError` carrying `MissingBaseline`.

### 5.12 Tables deliberately excluded from reconcile

- `migration_quarantine` — created outside reconcile and outside the goose batch (`internal/store/migration_runner.go:363-378`, DDL at `:657-663`).
- `goose_db_version` — reconcile *drops* it rather than creating it (`internal/store/schema_reconcile.go:260-266`); adoption recreates and stamps it (`internal/store/schema_reconcile.go:250-253`).
- `issue_history` — dropped, never created (`internal/store/schema_reconcile.go:305-311`).
- The post-baseline columns `lane`, `resolution`, `redirect_target` and `issue_events.stream_id`/`workspace_id` are absent from reconcile's `CREATE TABLE issues` / `CREATE TABLE issue_events` (`internal/store/schema_reconcile.go:127-146`, `:218-226`) — goose adds them after adoption.
- The drift-canary dump excludes only `goose_db_version` (`internal/store/schema_drift_test.go:156-158`).
- The shapemap's "bookkeeping, no domain field" set is `goose_db_version`, `migration_quarantine`, `meta` (`internal/store/shapemap_known.go:222-227`).

---

## 6. Every error message the schema/reconcile path can emit

From `internal/store/schema_reconcile.go` (line cited):

| Text (format) | Line |
|---|---|
| `workspace's issues table is missing reconcile prerequisites (%s); the shape is structurally beyond what pre-goose reconcile can recover — this is not a known historical shape` | 474-479 |
| `translate issue_history: probe table: %w` | 544 |
| `translate issue_history: probe columns: %w` | 551 |
| `translate issue_history: %w` (guard) | 582 |
| `translate issue_history: begin tx: %w` | 586 |
| `translate issue_history: query translatable rows: %w` | 625 |
| `translate issue_history: scan row: %w` | 637 |
| `translate issue_history: iterate rows: %w` | 643 |
| `translate issue_history: prepare event insert: %w` | 651 |
| `translate issue_history: prepare change insert: %w` | 656 |
| `translate issue_history: insert event %s: %w` | 664 |
| `translate issue_history: insert status change for %s: %w` | 675 |
| `translate issue_history: commit tx: %w` | 680 |
| `%s: %w` where `%s` is the step label (guard failure, execGatedCreate) | 839 |
| `%s: %w` where `%s` is the step label (exec failure, execGatedCreate) | 846 |
| `%s: %w` (guard failure, execGatedMutation) | 873 |
| `%s: %w` (exec failure, execGatedMutation) | 876 |
| `%s: probe: %w` | 893 |
| `ensureIssueRanks: query unranked: %w` | 1005 |
| `ensureIssueRanks: scan: %w` | 1012 |
| `ensureIssueRanks: rows: %w` | 1017 |
| `ensureIssueRanks: %w` (guard) | 1023 |
| `ensureIssueRanks: begin tx: %w` | 1035 |
| `ensureIssueRanks: read max existing rank: %w` | 1048 |
| `ensureIssueRanks: prepare: %w` | 1063 |
| `ensureIssueRanks: update %s: %w` | 1068 |
| `ensureIssueRanks: commit tx: %w` | 1073 |
| `reset priorities to normal: %w` (guard) | 1097 |
| `reset priorities to normal: %w` (UPDATE) | 1100 |
| `drop priority check %s: %w` | 1104 |
| `add priority check: %w` | 1108 |
| `query issue check constraints: %w` | 1123, 1185 |
| `scan issue check constraint: %w` | 1130, 1191 |
| `iterate issue check constraints: %w` | 1137, 1200 |
| `ensure status constraint: %w` (guard) | 1162 |
| `drop status check %s: %w` | 1166 |
| `add canonical status check: %w` | 1170 |

The step labels that flow into the `%s: %w` forms above:
`create <target>` (`:790`), `add column <table>.<column>` (`:802`),
`relax column <table>.<column> to nullable` (`:816`),
`drop fabricated goose_db_version (legacy workspace carried lying bookkeeping)` (`:265`),
`rename issue_events.assignee to actor` (`:281`),
`drop legacy issue_history table` (`:310`),
`drop topic default to match baseline shape` (`:351`),
`rename prompt column to agent_prompt` (`:368`),
plus the seven `ensureUnifiedStatusSchema` contexts (`:925,930,935,940,945,955,969`)
and `backfill legacy issue topics` (`:996`).

Adjacent, from `internal/store/migration_runner.go`:

| Text | Line |
|---|---|
| `reconcile pre-goose workspace: %w` (pre-gate) | 399 |
| `reconcile pre-goose workspace: %w` (reconcile itself) | 423 |
| `verify post-reconcile baseline shape: %w` | 439 |
| `post-reconcile workspace shape still differs from baseline (remaining gaps: %s); reconcile cannot bring this workspace to v1 — the shape is structurally beyond what pre-goose reconcile can recover` | 442-448 |
| `probe columns of %q: %w` / `scan column of %q: %w` / `iterate columns of %q: %w` | 1343, 1350, 1355 |
| `probe table %q: %w` | 1373 |
| `read baseline migration %q: %w` | 1399 |
| `baseline migration %q defines no tables` | 1403 |
| `ensure migration_quarantine table: %w` | 684, 688 |
| `ensure migration_quarantine table: count rows in stale-shape table: %w` | 726 |
| `migration_quarantine has a non-canonical shape (columns: %s) and %d row(s) of history; refusing to recreate automatically — this needs manual triage, not self-heal` | 734-738 |
| `migration v%d %q is recorded as applied, but its registered content is missing from this workspace's live schema: %s\n\nthis usually means the version number was reused for different historical content after this workspace last migrated — the recorded applied version does not reflect what actually ran here` | 207-212 |

And from `internal/store/downgrade.go:197`:
`downgrade: workspace is not goose-managed (no goose_db_version table); run Open first to adopt or initialize`.

---

## 7. Derived-from-Go schema literals

Three CHECK clause fragments are generated in Go from the sealed model
vocabularies rather than hand-written:

- `priorityCheckClause = fmt.Sprintf("priority >= %d AND priority <= %d", model.PriorityNormal, model.PriorityUrgent)` (`internal/store/schema_reconcile.go:77`), with `PriorityNormal = 0`, `PriorityUrgent = 1` (`internal/model/priority.go:15-16`).
- `issueTypeCheckClause = "issue_type IN (" + quoted(model.IssueTypes()) + ")"` (`internal/store/schema_reconcile.go:96`), where `IssueTypes()` returns `task, feature, bug, chore, epic` in that order (`internal/model/issue_type.go:31-33`).
- `containerTypeMembership = "issue_type IN (" + quoted(model.ContainerTypes()) + ")"` (`internal/store/schema_reconcile.go:97`), where `ContainerTypes()` is the `IsContainer()` subset — only `epic` (`internal/model/issue_type.go:56-58,63-70`).
- `canonicalStatusCheckClause` composes the container list twice (`internal/store/schema_reconcile.go:105-107`).
- `quotedIssueTypeList` renders `'a','b','c'` with no spaces (`internal/store/schema_reconcile.go:81-87`).

---

## 8. What the tests pin about the schema — exact assertions

### `internal/store/schema_drift_test.go`

- **`TestSchemaSnapshotMatchesConvergedSchema`** (`:54-80`): opens a fresh workspace, dumps `SHOW CREATE TABLE` for every table in `information_schema.tables WHERE table_schema = DATABASE()` except `goose_db_version`, sorted, each terminated with `;`, joined by `\n\n`, prefixed with the header constant and `\n\n`, suffixed `\n` (`:124-138`), and byte-compares against `schema_snapshot.sql`. The header text itself is part of the compared document (`:28-42`), so stripping the warning is drift. `-update-schema-snapshot` rewrites the file at mode `0o644` (`:21-22, 58-64`). No normalization is applied to the DDL — a Dolt formatting change is treated as real drift (`:118-123`).
- **`TestConvergedSchemaDumpIsDeterministic`** (`:87-95`): two independently-migrated fresh workspaces must serialize byte-identically.
- **`TestConvergedSchemaDumpIncludesUnexpectedTables`** (`:102-116`): creates `leftover_legacy (id VARCHAR(191) PRIMARY KEY)` and asserts the dump string contains `leftover_legacy` — i.e. the canary enumerates from the live DB, not a fixed list.

### `internal/store/migrations/baseline_frozen_test.go`

- **`TestBaselineFileIsFrozen`** (`:37-72`): SHA-256 of `00001_baseline.sql` must equal `e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb` (`:32`).

### `internal/store/baseline_schema_test.go`

- **`TestBaselineSchemaParsesEmbeddedMigration`** (`:18-52`): the parsed baseline is exactly seven tables with exactly the column lists reproduced in §3; table count must match (`:35-37`) and each table's sorted column list must match exactly (`:44-50`).
- **`TestOpenForwardMigratesPreConvergedColumnShape`** (`:66-126`): after `ALTER TABLE issues DROP COLUMN topic` + revert-to-baseline + drop goose log, reopening must succeed, `verifyBaselineShape` must report zero missing, `recordedMigrationVersion` must equal HEAD, and the seeded issue must still be findable with its title intact.

### `internal/store/schema_reconcile_test.go`

Shared harness: `hijackToPreGoose` runs the post-baseline migrations' Down
sections via `provider.DownTo(ctx, baselineVersion)` then `DROP TABLE
goose_db_version` and commits (`:50-108`). `assertReachedBaseline` asserts
`Open` succeeds, `verifyBaselineShape` returns zero missing, and
`recordedMigrationVersion() == headVersion(t)` (`:112-136`).

- **`TestReconcileAddsMissingIssueEventsTables`** (`:143-191`): with `issue_event_changes`, `issue_events` dropped and `issues.agent_prompt` dropped, Open converges and the seeded row survives with its title.
- **`TestReconcileRenamesPromptToAgentPrompt`** (`:196-230`): after renaming `agent_prompt` back to `` `prompt` ``, the reconcile renames it forward and the stored prompt body `"the historical prompt body"` survives.
- **`TestReconcileNormalizesLegacyStatusValues`** (`:236-288`): rows inserted with `todo`/`in-progress`/`done` come out as `open`/`in_progress`/`closed` and all three rows survive.
- **`TestReconcileNullsEpicStatus`** (`:293-344`): an epic row with `status='open'` has `status IS NULL` in the column after reconcile (queried directly), and its title survives.
- **`TestReconcileBackfillsTopicDefault`** (`:349-381`): a row inserted with `topic=''` reads back `topic == "misc"`.
- **`TestReconcileResetsLegacyPriorities`** (`:386-425`): with the legacy `CHECK (priority >= 0 AND priority <= 4)` installed and a `priority=3` row, after reconcile the row's priority is `0`.
- **`TestReconcileDropsLegacyIssueHistory`** (`:438-480`): a partial-shape `issue_history (id VARCHAR(191) PRIMARY KEY, issue_id VARCHAR(191) NOT NULL)` is dropped; `tableExists("issue_history")` is false afterwards; the seeded issue survives.
- **`TestIsLegacyStatusTransition`** (`:491-516`): five cases — null→null false; open→open false; null→open true; open→null true; open→closed true.
- **`TestCanonicalEventCanonicalization`** (`:522-565`): action NULL→nil, `"   "`→nil, `"  start  "`→`"start"`; reason NULL→`""`, `"  began work  "`→`"began work"`; actor NULL→`"unknown"`, `"   "`→`"unknown"`, `"  alice  "`→`"alice"`.
- **`TestCanonicalLegacyStatus`** (`:572-596`): null→null; `open`/`in_progress`/`closed` pass through; `in-progress`→`in_progress`; `todo`→`open`; `done`→`closed`; `weird`→`open`.
- **`TestReconcileTranslatesLegacyIssueHistoryToEvents`** (`:658-841`): eight canonical-shape `issue_history` rows produce exactly eight `issue_events` rows with the mapped `action`/`reason`/`actor`/`issue_id`; `created_by`→`actor`; empty-string and NULL actions both land as SQL NULL; whitespace is trimmed. Exactly three `issue_event_changes` rows are produced — `hist-start {status, open, in_progress}`, `hist-close {status, in_progress, closed}`, `hist-legacy-transition {status, open, closed}` (raw `todo`→`done` normalized) — and the five non-transition rows produce none.
- **`TestReconcileTranslateSkipsOrphanedHistoryRows`** (`:856-896`): a history row whose `issue_id` does not exist produces zero events; the valid row produces exactly one.
- **`TestReconcileTranslateRunsAfterActorRename`** (`:909-976`): with `issue_events` reshaped to the pre-rename `assignee TEXT NOT NULL` layout, translation still lands and `actor == "alice"`.
- **`TestReconcileTranslateIsIdempotentWithExistingEvents`** (`:993-1093`): a pre-existing `issue_events` row with a colliding id keeps its own `action`/`reason`/`actor` values, is not duplicated, and gains no change row; a genuinely-new row does get its event and its `{status, open, in_progress}` change row.
- **`TestReconcileRecoversFromFabricatedGooseRows`** (`:1112-1200`): with `issue_history` present and `goose_db_version` carrying three fabricated rows at a single tstamp (versions `0`, `1`, and HEAD+1), Open drops `issue_history`, leaves zero rows with `version_id > HEAD`, and preserves the seeded issue.
- **`TestReconcileIsIdempotent`** (`:1206-1237`): a workspace already at v1 converges with the seeded issue untouched.
- **`TestReconcileCreatedTablesMatchBaselineConstraintNames`** (`:1250-1304`): after dropping every canonical table except `meta` and forcing adoption, `information_schema.table_constraints` must contain CHECK constraints named exactly `issues_status_check`, `issues_priority_check`, `issues_type_check`, `relations_type_check`.
- **`TestReconcileTopicHasNoDefault`** (`:1315-1346`): after reconcile re-adds `issues.topic`, `information_schema.columns.column_default` for it must be NULL.
- **`TestReconcileRankBackfillCoexistsWithExistingRanks`** (`:1358-1414`): with one already-ranked row and one `item_rank=''` row, both end non-empty and distinct.
- **`TestPostReconcileBaselineVerificationCatchesNonIssuesGaps`** (`:1428-1482`): dropping `relations.created_by` makes Open fail with an error containing the literal `"relations.created_by"`, and `goose_db_version` must **not** exist afterwards.
- **`TestReconcileErrorMessageIsActionable`** (`:1490-1532`): against `CREATE TABLE issues (id VARCHAR(191) PRIMARY KEY)`, Open's error must contain `"reconcile pre-goose workspace"`, `"status"`, and `"not a known historical shape"`, and must **not** contain `"restore it from a snapshot or recreate"`.
- **`TestDerivedTypeCheckClausesMatchHistoricalLiterals`** (`:1541-1556`) pins the generated clause text byte-for-byte:
  - `issueTypeCheckClause == "issue_type IN ('task','feature','bug','chore','epic')"`
  - `containerTypeMembership == "issue_type IN ('epic')"`
  - `canonicalStatusCheckClause == "(issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))"`
  - `priorityCheckClause == "priority >= 0 AND priority <= 1"`

---

## 9. Down-migration schema effects (for completeness)

- v2 Down: `ALTER TABLE issues DROP COLUMN lane` (`00002_add_lane.sql:19`).
- v3 Down: `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check` then `DROP COLUMN resolution` (`00003_add_resolution.sql:24,27`).
- v4 Down: `INSERT IGNORE INTO relations(src_id, dst_id, type, created_at, created_by)` re-materializing one `related-to` edge per non-NULL `redirect_target` with `LEAST/GREATEST` ordering, `created_at = COALESCE(closed_at, updated_at)`, `created_by = 'unknown'` (`00004_add_redirect_target.sql:69-72`); then `DROP CONSTRAINT issues_redirect_target_check` and `DROP COLUMN redirect_target` (`:75,78`).
- v5 Down: `ALTER TABLE issue_events DROP COLUMN stream_id` then `DROP COLUMN workspace_id` (`00005_add_event_attribution.sql:29,32`).
- v1 Down: `DROP TABLE IF EXISTS` for `issue_event_changes`, `issue_events`, `labels`, `comments`, `relations`, `issues`, `meta` — in that FK-safe order (`00001_baseline.sql:156-174`).

v4 Up also performs data mutations that change `relations` content: a backfill
`UPDATE issues SET redirect_target = (...)` for rows with
`resolution IN ('duplicate','superseded')` and exactly one incident
`related-to` edge (`00004_add_redirect_target.sql:27-38`), followed by
`DELETE r FROM relations r JOIN issues i ON i.redirect_target IS NOT NULL ...
WHERE r.type = 'related-to'` (`:45-49`).


---

## The shapemap mechanism

Package `store`. Files: `internal/store/shapemap.go` (844 lines), `internal/store/shapemap_json.go` (281), `internal/store/shapemap_known.go` (314). Tests: `shapemap_test.go` (514), `shapemap_json_test.go` (333), `shapemap_fanout_test.go` (146).

### 1. What a "shape" is: every type and field

#### 1.1 Input data type (defined outside the slice, consumed by it)

`RawDump` — `internal/store/rawdump.go:23-33`:
- `WorkspaceID string` json:`workspace_id` (rawdump.go:24)
- `DoltHead string` json:`dolt_head` (rawdump.go:31)
- `Tables []RawTable` json:`tables` (rawdump.go:32)

`RawTable` — `internal/store/rawdump.go:39-43`:
- `Name string` json:`name` (rawdump.go:40)
- `Columns []string` json:`columns` — catalog order (rawdump.go:41)
- `Rows [][]any` json:`rows` — positional cells; always non-nil, `[]` for an empty table (rawdump.go:42, initialized at rawdump.go:181)

Cell Go types as produced by `dumpTable`: driver `[]byte` is normalized to `string` (`internal/store/rawdump.go:190-192`); SQL NULL scans as `nil` (rawdump.go:167 comment, scan at rawdump.go:184).

#### 1.2 Mapping types (shapemap.go)

`ShapeMapping` — shapemap.go:34-36:
- `Tables []TableMapping` (shapemap.go:35)

`TableMapping` — shapemap.go:43-51:
- `Table string` (shapemap.go:44)
- `Emitters []Emitter` (shapemap.go:45)
- `Drops map[string]Dropped` — keyed by column name (shapemap.go:50)

`Emitter` — shapemap.go:58-66:
- `Collection collection` (shapemap.go:59)
- `Fields map[string]FieldSource` — keyed by domain field name (shapemap.go:64)
- `When EmitCondition` (shapemap.go:65)

`FieldSource interface{ isFieldSource() }` — sealed, package-private method (shapemap.go:72). Exactly two implementations:
- `FromColumn{ Column string; Transform Transform }` (shapemap.go:81-84); `func (FromColumn) isFieldSource() {}` (shapemap.go:95)
- `Constant{ Value any }` (shapemap.go:91-93); `func (Constant) isFieldSource() {}` (shapemap.go:96)

`EmitCondition interface{ isEmitCondition() }` — sealed (shapemap.go:102). Exactly two implementations:
- `Always struct{}` (shapemap.go:105); `func (Always) isEmitCondition() {}` (shapemap.go:118)
- `WhenChanged{ FieldA string; FieldB string }` (shapemap.go:113-116); `func (WhenChanged) isEmitCondition() {}` (shapemap.go:119)

`Dropped` — shapemap.go:125-128:
- `Provenance DropProvenance` (shapemap.go:126)
- `Reason string` (shapemap.go:127)

`DropProvenance string` (shapemap.go:131) with exactly two constants:

| Constant | Literal |
|---|---|
| `DropIntended` | `"intended"` (shapemap.go:137) |
| `DropUnexplained` | `"unexplained"` (shapemap.go:140) |

`Transform string` (shapemap.go:149) with exactly six constants:

| Constant | Literal | shapemap.go line |
|---|---|---|
| `TransformIdentity` | `"identity"` | 152 |
| `TransformLegacyStatus` | `"legacy_status_value"` | 153 |
| `TransformTimestamp` | `"timestamp"` | 154 |
| `TransformEventAction` | `"event_action"` | 159 |
| `TransformEventReason` | `"event_reason"` | 160 |
| `TransformEventActor` | `"event_actor"` | 161 |

`TargetKey string` — the string `"<collection>.<field>"` (shapemap.go:165).

`collection string` (shapemap.go:167) with exactly six constants:

| Constant | Literal | line |
|---|---|---|
| `collIssues` | `"issues"` | 170 |
| `collRelations` | `"relations"` | 171 |
| `collComments` | `"comments"` | 172 |
| `collLabels` | `"labels"` | 173 |
| `collEvents` | `"events"` | 174 |
| `collEventChanges` | `"event_changes"` | 175 |

`targetField` (unexported) — shapemap.go:178-194:
- `coll collection` (179), `field string` (180), `canonical Transform` (186), `admits map[Transform]bool` (187), `optional bool` (193)

Package constants `required = false`, `optional = true` (shapemap.go:211-214).

`ColumnRef` — shapemap_known.go:251-254: `Table string`, `Column string`; `func (c ColumnRef) String() string { return c.Table + "." + c.Column }` (shapemap_known.go:256).

### 2. The target registry (the closed set of legal mapping targets)

Built once at package init by `buildTargetRegistry()` into `var targetRegistry map[TargetKey]targetField` (shapemap.go:205, 216-265). Key is `string(collection)+"."+field` (shapemap.go:221, 237).

Helper `add(c, canonical, opt, fields...)` sets `admits = {canonical: true}` only (shapemap.go:219-226). Helper `addMulti(c, canonical, extra, opt, field)` sets `admits = {canonical} ∪ extra` (shapemap.go:232-240).

Complete registry (33 entries):

| TargetKey | collection | field | canonical | admits | optional | line |
|---|---|---|---|---|---|---|
| `issues.id` | issues | id | identity | {identity} | required | 241 |
| `issues.title` | issues | title | identity | {identity} | required | 241 |
| `issues.description` | issues | description | identity | {identity} | required | 241 |
| `issues.priority` | issues | priority | identity | {identity} | required | 241 |
| `issues.issue_type` | issues | issue_type | identity | {identity} | required | 241 |
| `issues.prompt` | issues | prompt | identity | {identity} | optional | 242 |
| `issues.assignee` | issues | assignee | identity | {identity} | optional | 242 |
| `issues.topic` | issues | topic | identity | {identity} | optional | 242 |
| `issues.rank` | issues | rank | identity | {identity} | optional | 242 |
| `issues.lane` | issues | lane | identity | {identity} | optional | 242 |
| `issues.resolution` | issues | resolution | identity | {identity} | optional | 242 |
| `issues.redirect_target` | issues | redirect_target | identity | {identity} | optional | 242 |
| `issues.created_at` | issues | created_at | timestamp | {timestamp} | required | 243 |
| `issues.updated_at` | issues | updated_at | timestamp | {timestamp} | required | 243 |
| `issues.closed_at` | issues | closed_at | timestamp | {timestamp} | required | 243 |
| `issues.archived_at` | issues | archived_at | timestamp | {timestamp} | optional | 244 |
| `issues.deleted_at` | issues | deleted_at | timestamp | {timestamp} | optional | 244 |
| `issues.status` | issues | status | legacy_status_value | {legacy_status_value} | required | 245 |
| `relations.src_id` | relations | src_id | identity | {identity} | required | 246 |
| `relations.dst_id` | relations | dst_id | identity | {identity} | required | 246 |
| `relations.type` | relations | type | identity | {identity} | required | 246 |
| `relations.created_by` | relations | created_by | identity | {identity} | required | 246 |
| `relations.created_at` | relations | created_at | timestamp | {timestamp} | required | 247 |
| `comments.id` | comments | id | identity | {identity} | required | 248 |
| `comments.issue_id` | comments | issue_id | identity | {identity} | required | 248 |
| `comments.body` | comments | body | identity | {identity} | required | 248 |
| `comments.created_by` | comments | created_by | identity | {identity} | required | 248 |
| `comments.created_at` | comments | created_at | timestamp | {timestamp} | required | 249 |
| `labels.issue_id` | labels | issue_id | identity | {identity} | required | 250 |
| `labels.name` | labels | name | identity | {identity} | required | 250 |
| `labels.created_by` | labels | created_by | identity | {identity} | required | 250 |
| `labels.created_at` | labels | created_at | timestamp | {timestamp} | required | 251 |
| `events.id` | events | id | identity | {identity} | required | 252 |
| `events.issue_id` | events | issue_id | identity | {identity} | required | 252 |
| `events.action` | events | action | identity | {identity, event_action} | optional | 253 |
| `events.reason` | events | reason | identity | {identity, event_reason} | required | 254 |
| `events.actor` | events | actor | identity | {identity, event_actor} | required | 255 |
| `events.created_at` | events | created_at | timestamp | {timestamp} | required | 256 |
| `events.stream` | events | stream | identity | {identity} | optional | 260 |
| `events.workspace` | events | workspace | identity | {identity} | optional | 260 |
| `event_changes.event_id` | event_changes | event_id | identity | {identity} | required | 261 |
| `event_changes.field` | event_changes | field | identity | {identity} | required | 261 |
| `event_changes.from` | event_changes | from | identity | {identity, legacy_status_value} | optional | 262 |
| `event_changes.to` | event_changes | to | identity | {identity, legacy_status_value} | optional | 263 |

(44 entries total; the `add(...)` variadic lines register several fields each.)

`knownCollections map[collection]bool` is derived from `targetRegistry` values' `coll` (shapemap.go:209, 267-273), i.e. exactly the six collections above.

Per-collection required-field sets (what `emitterProblems` enforces at shapemap.go:433-439):
- issues: id, title, description, priority, issue_type, created_at, updated_at, closed_at, status
- relations: src_id, dst_id, type, created_by, created_at
- comments: id, issue_id, body, created_by, created_at
- labels: issue_id, name, created_by, created_at
- events: id, issue_id, reason, actor, created_at
- event_changes: event_id, field

### 3. The KNOWN shapes registry (shapemap_known.go)

#### 3.1 `knownSourceColumns` — source column name → TargetKey, verbatim

`var knownSourceColumns = map[string]map[string]TargetKey` (shapemap_known.go:160-219). The transform is NOT stored here; it is the target's `canonical` transform read from `targetRegistry` (shapemap_known.go:77-79).

Table `"issues"` (shapemap_known.go:161-181):

| source column | TargetKey | line | note in source |
|---|---|---|---|
| `id` | `issues.id` | 162 | |
| `title` | `issues.title` | 163 | |
| `description` | `issues.description` | 164 | |
| `agent_prompt` | `issues.prompt` | 165 | `// v1 name` |
| `prompt` | `issues.prompt` | 166 | `// pre-goose, pre-rename` |
| `status` | `issues.status` | 167 | |
| `priority` | `issues.priority` | 168 | |
| `issue_type` | `issues.issue_type` | 169 | |
| `topic` | `issues.topic` | 170 | |
| `assignee` | `issues.assignee` | 171 | |
| `created_at` | `issues.created_at` | 172 | |
| `updated_at` | `issues.updated_at` | 173 | |
| `closed_at` | `issues.closed_at` | 174 | |
| `resolution` | `issues.resolution` | 175 | |
| `redirect_target` | `issues.redirect_target` | 176 | |
| `archived_at` | `issues.archived_at` | 177 | |
| `deleted_at` | `issues.deleted_at` | 178 | |
| `item_rank` | `issues.rank` | 179 | `// v1 name` |
| `lane` | `issues.lane` | 180 | |

Table `"relations"` (182-188): `src_id`→`relations.src_id`, `dst_id`→`relations.dst_id`, `type`→`relations.type`, `created_at`→`relations.created_at`, `created_by`→`relations.created_by`.

Table `"comments"` (189-195): `id`→`comments.id`, `issue_id`→`comments.issue_id`, `body`→`comments.body`, `created_at`→`comments.created_at`, `created_by`→`comments.created_by`.

Table `"labels"` (196-201): `issue_id`→`labels.issue_id`, `label`→`labels.name`, `created_at`→`labels.created_at`, `created_by`→`labels.created_by`. (Note the source name `label` → domain field `name`.)

Table `"issue_events"` (202-212):

| source column | TargetKey | line |
|---|---|---|
| `id` | `events.id` | 203 |
| `issue_id` | `events.issue_id` | 204 |
| `action` | `events.action` | 205 |
| `reason` | `events.reason` | 206 |
| `actor` | `events.actor` (`// v1 name`) | 207 |
| `assignee` | `events.actor` (`// pre-goose, pre-rename`) | 208 |
| `created_at` | `events.created_at` | 209 |
| `stream_id` | `events.stream` | 210 |
| `workspace_id` | `events.workspace` | 211 |

Table `"issue_event_changes"` (213-218): `event_id`→`event_changes.event_id`, `field`→`event_changes.field`, `from_value`→`event_changes.from`, `to_value`→`event_changes.to`.

`issue_history` is deliberately absent from this map (shapemap_known.go:158-159); it is handled by `issueHistoryFanOut`.

Because `simpleEmitter` reads `tf.canonical`, the effective transform per recognized column is: `timestamp` for `created_at`/`updated_at`/`closed_at`/`archived_at`/`deleted_at` (all tables), `legacy_status_value` for `issues.status`, `identity` for everything else — including `issue_events.action/reason/actor`, whose canonical is `identity` (shapemap.go:253-255).

#### 3.2 `bookkeepingTables` — table name → drop reason, verbatim

`var bookkeepingTables = map[string]string` (shapemap_known.go:224-228):

| table | reason string |
|---|---|
| `goose_db_version` | `"goose migration registry — schema bookkeeping, no domain field"` |
| `migration_quarantine` | `"migration quarantine ledger — schema bookkeeping, no domain field"` |
| `meta` | `"schema metadata table — no domain field"` |

#### 3.3 `legacyIssueHistoryColumns` (the fan-out gate, defined in schema_reconcile.go:497-499)

`[]string{"id", "issue_id", "action", "reason", "from_status", "to_status", "created_at", "created_by"}`.

#### 3.4 `migrationDroppedCols` — scanned from the embedded migration corpus

`var migrationDroppedCols = scanMigrationDrops()` (shapemap_known.go:287). `scanMigrationDrops` reads every non-directory entry of `migrations.FS` (shapemap_known.go:291-303), takes `gooseUpSection(string(data))` of each file (shapemap_known.go:309), runs `parseDroppedColumns` on it, and maps each `ColumnRef` to the migration **file name** (shapemap_known.go:310).

Regexes (shapemap_known.go:262-263):
- `alterTableRE = (?is)ALTER\s+TABLE\s+`?(\w+)`?([^;]*)` — group 1 = table, group 2 = statement body up to `;` or EOF.
- `dropColumnRE = (?is)DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?`?(\w+)`?` — group 1 = column; matched repeatedly within one ALTER body, so multi-drop statements are fully captured (shapemap_known.go:272-278).

`gooseUpSection` (migration_runner.go:1410-1421): case-insensitively finds `-- +goose up`; if absent returns the whole SQL; otherwise slices from that index to the next `-- +goose down` (or to EOF if there is none).

Current corpus content of this map: **empty**. Every `DROP COLUMN` in `internal/store/migrations/` occurs in a `-- +goose Down` section — `00002_add_lane.sql:19` (Down starts line 12), `00003_add_resolution.sql:27` (Down at 17), `00004_add_redirect_target.sql:78` (Down at 52), `00005_add_event_attribution.sql:29,32` (Down at 21) — and `README.md` is not a `.sql` migration but is still read (the loop does not filter by extension, shapemap_known.go:299-303); its line 52 `ALTER TABLE issues DROP COLUMN priority_band;` lies outside any `-- +goose Up` marker, and `gooseUpSection` returns the whole text when no marker is found, so whether it contributes depends on that file's marker content.

On `fs.ReadDir` or `ReadFile` failure, the package `panic`s: `"scan migration drops: read embedded registry: " + err.Error()` (shapemap_known.go:295) and `"scan migration drops: read " + entry.Name() + ": " + err.Error()` (shapemap_known.go:302).

### 4. Deterministic mapping: how a dump becomes a ShapeMapping

`DeterministicMap(dump RawDump) (ShapeMapping, bool)` — shapemap_known.go:33-46:
1. For each `dump.Tables` in order, call `mapTable(table)`; on `ok=false` return `(ShapeMapping{}, false)` immediately (shapemap_known.go:35-41).
2. Append each `TableMapping` in dump table order (shapemap_known.go:40).
3. Then `if Validate(dump, out) != nil { return ShapeMapping{}, false }` (shapemap_known.go:42-44). No error text escapes; the only signal is the bool.

`mapTable(table RawTable) (TableMapping, bool)` — shapemap_known.go:51-63, in this order:
1. If `table.Name` is a key of `bookkeepingTables` → `dropAllColumns(table), true` (52-54).
2. If `table.Name == "issue_history"` → `issueHistoryFanOut(table)` (55-57).
3. Else look up `knownSourceColumns[table.Name]`; absent → `(TableMapping{}, false)` (58-61).
4. Else `simpleEmitter(table, rules)` (62).

`simpleEmitter(table, rules)` — shapemap_known.go:69-85:
- Iterates `table.Columns` in order; any column not in `rules` returns `(TableMapping{}, false)` — the whole table (and via DeterministicMap, the whole dump) declines (72-74).
- `tf := targetRegistry[target]`; `coll = tf.coll` (reassigned per column — last column wins, but all rules for one table point at one collection); `fields[tf.field] = FromColumn{Column: col, Transform: tf.canonical}` (76-79).
- Returns one `TableMapping{Table: table.Name, Emitters: []Emitter{{Collection: coll, When: Always{}, Fields: fields}}}` with `Drops` nil (81-84).
- Two source columns aliasing the same target field (e.g. `prompt` and `agent_prompt`) collapse into one map entry; the loser column ends up referenced by nobody and Validate then fails on totality — the decline path pinned by `TestRejectsAmbiguousAlias` (shapemap_test.go:98-134).

`dropAllColumns(table)` — shapemap_known.go:89-96: for every column, `classifyDrop(table.Name, col)` and store `Dropped{Provenance, Reason}`; returns `TableMapping{Table, Drops}` with no emitters.

`classifyDrop(table, column) (DropProvenance, string)` — shapemap_known.go:238-246, in order:
1. `bookkeepingTables[table]` hit → `(DropIntended, <that table's reason string>)`.
2. `migrationDroppedCols[ColumnRef{table, column}]` hit → `(DropIntended, "removed by migration "+file)`.
3. Otherwise → `(DropUnexplained, "")`.

`issueHistoryFanOut(table)` — shapemap_known.go:112-149. See §7.

### 5. Validate: the algorithm and every error message

`Validate(dump RawDump, m ShapeMapping) error` — shapemap.go:289-355. Order of checks (first failure returns; only within the "problems" phase are faults aggregated):

**Step 0 — indexing.**
- `indexDumpTables(dump)` (shapemap.go:452-461): builds `map[string]RawTable`; a repeated table name returns
  `dump lists table %q more than once` (shapemap.go:456).
- `indexMapping(m)` (shapemap.go:466-475): builds `map[string]TableMapping`; a repeated `tm.Table` returns
  `mapping dispositions table %q more than once` (shapemap.go:470).

**Step 1 — totality** (shapemap.go:305-320). For every dump table, `referenced := referencedColumns(tm)` (the set of `FromColumn.Column` over all emitters of that table's mapping, shapemap.go:359-369). For each `col` of `table.Columns`, if it is neither referenced nor a key of `tm.Drops`, append `"<table>.<col>"` to `unaccounted`. A dump table with no mapping at all yields all its columns here. If non-empty: sort ascending, then return

```
mapping is not total: %d source column(s) unaccounted for: %s
```
with `%d` = count and `%s` = the sorted names joined by `", "` (shapemap.go:318-319).

**Step 2 — aggregated problems** (shapemap.go:322-339). Collected, sorted ascending, joined with `"; "`, returned as

```
mapping is malformed: %s
```
(shapemap.go:338).

Problem sources:

a) A mapping table absent from the dump (shapemap.go:323-327):
```
table %q: mapping references a table the dump does not have
```

b) Per dump table that has a mapping, `tableProblems(table, tm)` (shapemap.go:375-397):
- For each drop column not in the table's columns:
  `table %q: drop names column %q the dump does not have` (shapemap.go:387)
- For each drop column also referenced by an emitter:
  `table %q column %q is both mapped and dropped` (shapemap.go:390)
- For each drop whose `Provenance` is neither `DropIntended` nor `DropUnexplained`:
  `table %q column %q: unknown drop provenance %q` (shapemap.go:393)

c) Per emitter, `emitterProblems(tableName, cols, em)` (shapemap.go:401-448):
- Unknown collection (`!knownCollections[em.Collection]`) — reports and **returns early**, skipping all other emitter checks (shapemap.go:403-406):
  `table %q: emitter into unknown collection %q`
- For each `field → src` in `em.Fields`, key `= string(collection)+"."+field`; if not in `targetRegistry` (shapemap.go:410-412), and `continue`:
  `table %q: emitter into %q targets unknown field %q`
- `FromColumn` case (shapemap.go:415-421): if `s.Column` not among the dump table's columns:
  `table %q: %q.%q maps from column %q the dump does not have`
  ; if `!tf.admits[s.Transform]`:
  `table %q: %q.%q does not admit transform %q`
  (both can fire for the same field)
- `Constant` case (shapemap.go:422-428): if `tf.canonical != TransformIdentity`:
  `table %q: %q.%q is not a passthrough field; a constant cannot land here`
  ; if `s.Value` is not a `string`:
  `table %q: %q.%q constant must be a string, got %T`
- Default case (a third FieldSource implementation, unreachable from outside the package) (shapemap.go:430):
  `table %q: %q.%q has unknown field source %T`
- Required coverage (shapemap.go:433-439): for every registry entry whose `coll == em.Collection` and `!optional`, if `em.Fields[tf.field]` is absent:
  `table %q: emitter into %q does not cover required field %q` — the `%q` is the full **TargetKey** (e.g. `"comments.body"`), not the bare field name.
- `WhenChanged` condition (shapemap.go:440-446): for each of `FieldA`, `FieldB` not present in `em.Fields`:
  `table %q: emitter condition references field %q the emitter does not produce`
  (`Always` and any other condition value are unchecked.)

**Step 3 — row arity** (shapemap.go:346-353), iterating `dump.Tables` in order and rows by index; first mismatch returns:
```
table %q row %d has %d cells, want %d (one per column)
```
This runs after the totality and problem phases, so a shape fault masks an arity fault.

`tablesByName(m)` (shapemap.go:481-487) is the post-Validate, error-free index (last-wins on duplicates).

### 6. Apply: the fold, and per-SQL-type coercion

`Apply(dump RawDump, m ShapeMapping) (model.Export, error)` — shapemap.go:501-525:
1. Calls `Validate(dump, m)` itself; on error returns `model.Export{}, err` (shapemap.go:502-504).
2. `mapTables := tablesByName(m)`; `records := map[collection][]map[string]any{}` (shapemap.go:507-508).
3. Iterates `dump.Tables` in order → `colIndex := rowColumnIndex(table)` (name → positional index, shapemap.go:528-534) → each row in order → **each emitter of that table in mapping order**: `buildRecord`, then `if emits(em.When, rec)` append to `records[em.Collection]` (shapemap.go:509-523). Record order is therefore table order, then row order, then emitter order.
4. Any `buildRecord` error is wrapped: `table %q: %w` (shapemap.go:516).
5. `assembleExport(dump.WorkspaceID, records)` (shapemap.go:524). Note: `DoltHead` is not carried into the Export.

`buildRecord(em, tableName, colIndex, row)` — shapemap.go:538-553: for each field,
- `FromColumn` → `applyTransform(s.Transform, row[colIndex[s.Column]])`; error wrapped `column %q: %w` (shapemap.go:545). Combined with Apply's wrapper the surfaced text is e.g. `table "issues": column "created_at": invalid timestamp "not-a-timestamp"`.
- `Constant` → `rec[field] = s.Value` verbatim (shapemap.go:549).
- A third `FieldSource` type silently produces no entry (no default arm, shapemap.go:541-550).
- `tableName` parameter is unused in the body.

`emits(when, rec)` — shapemap.go:558-566:
- `WhenChanged` → `cellsDiffer(rec[FieldA], rec[FieldB])`.
- **Every other value, including `Always` and `nil`** → `true` (the `default` arm, shapemap.go:562-565).

`cellsDiffer(a, b any) bool` — shapemap.go:572-581:
- If exactly one of them is `nil` → `true`.
- If both `nil` → `false`.
- Else compare `cellString(a) != cellString(b)`.

#### 6.1 Transform semantics — `applyTransform(t Transform, cell any) (any, error)` (shapemap.go:713-763)

| Transform | NULL (`nil`) input | `string` input | other Go type input |
|---|---|---|---|
| `identity` | returns `nil` | returns the value unchanged | returns the value unchanged (shapemap.go:715-716) |
| `timestamp` | returns `nil, nil` (shapemap.go:718-720) | `parseTimestamp` → `time.Time`, or error `invalid timestamp %q` (shapemap.go:729-733, 789) | error: `%s requires a string cell, got %T` with `%s` = `"timestamp"` (shapemap.go:723) |
| `legacy_status_value` | `cellNullString` → invalid → `canonicalLegacyStatus` keeps invalid → `nullableSQLString` → `nil` | canonicalized string (table below) | error `legacy_status_value: expected a string or NULL cell, got %T` (shapemap.go:738, 776) |
| `event_action` | `canonicalEventAction(invalid)` → `nil` | `strings.TrimSpace`; empty after trim → `nil`; else trimmed string | error `event_action: expected a string or NULL cell, got %T` |
| `event_reason` | → `""` | `strings.TrimSpace(v)` | error `event_reason: expected a string or NULL cell, got %T` |
| `event_actor` | → `"unknown"` | `strings.TrimSpace`; empty after trim → `"unknown"`; else trimmed | error `event_actor: expected a string or NULL cell, got %T` |
| any other value | — | — | error `unknown transform %q` (shapemap.go:761) |

`cellNullString` (shapemap.go:769-778): `nil` → `sql.NullString{}`; `string` → `{Valid:true, String:v}`; anything else → error `expected a string or NULL cell, got %T`.

`parseTimestamp(s)` (shapemap.go:783-790): tries `time.RFC3339Nano` then `time.RFC3339`; on both failing returns `time.Time{}, fmt.Errorf("invalid timestamp %q", s)`.

`canonicalLegacyStatus` (schema_reconcile.go:741-757), exact-match switch:

| input | output |
|---|---|
| NULL | NULL |
| `"open"` | `"open"` |
| `"in_progress"` | `"in_progress"` |
| `"closed"` | `"closed"` |
| `"in-progress"` | `"in_progress"` |
| `"todo"` | `"open"` |
| `"done"` | `"closed"` |
| anything else (including `""`) | `"open"` |

`canonicalEventAction` (schema_reconcile.go:690-702) returns `any` — `nil` or trimmed string. `canonicalEventReason` (703-709) returns `string`. `canonicalEventActor` (713-724) returns `string`, fallback `"unknown"`. `nullableSQLString` (777-782): invalid → `nil`, valid → the string.

#### 6.2 Cell readers

`cellString(cell any) string` (shapemap.go:795-804): `nil` → `""`; `string` → itself; anything else → `fmt.Sprint(v)`. A missing map key yields `nil` → `""`.

`cellInt(cell any) int` (shapemap.go:806-826): `nil`→0; `int`/`int32`/`int64`/`uint64`/`float64` → converted via `int(v)` (float truncates); `string` → `strconv.Atoi(strings.TrimSpace(v))` with the error discarded (so unparseable → 0); any other type → 0.

`cellTime(cell any) time.Time` (shapemap.go:832-837): `time.Time` → itself; anything else (including `nil` and a string) → `time.Time{}` zero value.

`cellTimePtr(cell any) *time.Time` (shapemap.go:839-844): `time.Time` → pointer to it; anything else → `nil`.

#### 6.3 `assembleExport` — the exact model.Export produced

shapemap.go:586-665. Starts from:
```go
model.Export{Version: 2, WorkspaceID: workspaceID,
  Issues: []model.Issue{}, Relations: []model.Relation{}, Comments: []model.Comment{},
  Labels: []model.Label{}, Events: []model.IssueEvent{}}
```
(shapemap.go:588-595) — `Version` is hardcoded `2`; all slices non-nil.

- **Issues** (596-602): `buildIssue(rec)` per record; error propagated as `model.Export{}, err`.
- **Relations** (603-611): `model.Relation{SrcID: cellString(rec["src_id"]), DstID: cellString(rec["dst_id"]), Type: model.RelationType(cellString(rec["type"])), CreatedAt: cellTime(rec["created_at"]), CreatedBy: cellString(rec["created_by"])}` — the `type` value is cast without validation.
- **Comments** (612-620): `model.Comment{ID, IssueID, Body, CreatedAt: cellTime, CreatedBy}` from `rec["id"]`, `rec["issue_id"]`, `rec["body"]`, `rec["created_at"]`, `rec["created_by"]`.
- **Labels** (621-628): `model.Label{IssueID, Name: cellString(rec["name"]), CreatedAt: cellTime, CreatedBy}`.
- **Events** (632-650): `model.IssueEvent{ID: cellString(rec["id"]), IssueID, Action: cellString(rec["action"]), Reason, Actor, CreatedAt: cellTime(rec["created_at"]), Attribution: model.NewAttribution(cellString(rec["stream"]), cellString(rec["workspace"])), Changes: []model.FieldChange{}}`. An index `byID[ev.ID] = len(events)` is built (shapemap.go:648) — **last event with a duplicate id wins the index**.
- **Event changes** (651-662): for each record, `eventID := cellString(rec["event_id"])`; if `byID` has no such id, `assembleExport` returns
  ```
  event change references unknown event_id %q
  ```
  (shapemap.go:655). Otherwise appends `model.FieldChange{Field: cellString(rec["field"]), From: cellString(rec["from"]), To: cellString(rec["to"])}` to that event's `Changes` (shapemap.go:657-661). Note NULL `from`/`to` become `""` here even though the transform preserved NULL.

`buildIssue(rec)` — shapemap.go:673-706:
- `model.Issue{ID: cellString(rec["id"]), Title, Description, Prompt: cellString(rec["prompt"]), Priority: model.Priority(cellInt(rec["priority"])), IssueType: model.IssueType(cellString(rec["issue_type"])), Topic, Assignee, Rank: cellString(rec["rank"]), CreatedAt: cellTime(rec["created_at"]), UpdatedAt: cellTime(rec["updated_at"])}` (shapemap.go:674-688). **`rec["lane"]` is never read**, despite `issues.lane` being a registered target and `lane` being in `knownSourceColumns`.
- `issue.SetRetention(model.RetentionFromTimestamps(cellTimePtr(rec["archived_at"]), cellTimePtr(rec["deleted_at"])))` (shapemap.go:689).
- `view := model.StatusView{}`; only if `!issue.IssueType.IsContainer()` (shapemap.go:691) is it populated:
  - `view.Value = model.DefaultOpen(cellString(rec["status"]))` (692)
  - `view.ClosedAt = cellTimePtr(rec["closed_at"])` (693)
  - if `cellString(rec["resolution"]) != ""`, `view.Resolution = &model.Resolution(raw)` (697-700)
  - if `cellString(rec["redirect_target"]) != ""`, `view.RedirectTarget = &raw` (701-703)
- Returns `model.HydrateRow(issue, view, nil)` (shapemap.go:705) — its error is propagated up through `assembleExport`.

### 7. Fan-out: `issue_history` → events + conditional event_changes

`issueHistoryFanOut(table RawTable) (TableMapping, bool)` — shapemap_known.go:112-149.

**Gate** (114-121): build the column set; for each of `legacyIssueHistoryColumns` (`id, issue_id, action, reason, from_status, to_status, created_at, created_by`), if missing → `(TableMapping{}, false)`. Presence-only: extra columns pass this gate but then fall out as unaccounted at Validate (comment at shapemap_known.go:108-111), so a strict superset makes `DeterministicMap` return `ok=false`.

**Expansion rule** — one `TableMapping{Table: "issue_history"}` with `Drops` nil and exactly two emitters (shapemap_known.go:122-148):

Emitter 1 — `Collection: collEvents`, `When: Always{}` (one record per row):

| domain field | source column | transform |
|---|---|---|
| `id` | `id` | `identity` |
| `issue_id` | `issue_id` | `identity` |
| `action` | `action` | `event_action` |
| `reason` | `reason` | `event_reason` |
| `actor` | **`created_by`** | `event_actor` |
| `created_at` | `created_at` | `timestamp` |

Emitter 2 — `Collection: collEventChanges`, `When: WhenChanged{FieldA: "from", FieldB: "to"}`:

| domain field | source | transform |
|---|---|---|
| `event_id` | column `id` | `identity` |
| `field` | `Constant{Value: "status"}` | — |
| `from` | column `from_status` | `legacy_status_value` |
| `to` | column `to_status` | `legacy_status_value` |

So each `issue_history` row yields exactly one event record, plus one change record iff the **post-canonicalization** `from`/`to` differ under `cellsDiffer` (NULL counted as distinct from any string; NULL vs NULL equal). Since both are canonicalized first, `"todo"`→`"done"` is a transition (`open`≠`closed`) while `"in-progress"`→`"in_progress"` is **not** (both canonicalize to `in_progress`). The `id` column is consumed by both emitters (events.id and event_changes.event_id), which is legal — totality only requires each column be referenced at least once.

The emitters do not cover `events.stream`/`events.workspace` (optional) so `Attribution` is built from two empty strings; `events.action` is optional but supplied.

#### What `shapemap_fanout_test.go` pins

`TestFanOutConservesIssueHistoryAgainstReconcile` (shapemap_fanout_test.go:65-136):
- Seeds a Dolt workspace, inserts 8 legacy `issue_history` rows (lines 82-93) with these `(id, action, reason, from_status, to_status, created_at, created_by)`:
  - `hist-start`, `"start"`, `"began work"`, `"open"`→`"in_progress"`, `2026-01-01T10:00:00Z`, `alice`
  - `hist-comment-null`, action `nil`, `"added context"`, `nil`→`nil`, `10:05:00Z`, `alice`
  - `hist-comment-empty`, action `""`, `"added more context"`, `nil`→`nil`, `10:06:00Z`, `alice`
  - `hist-close`, `"close"`, `"shipped"`, `"in_progress"`→`"closed"`, `11:00:00Z`, `bob`
  - `hist-same-status`, `"touch"`, `"no movement"`, `"closed"`→`"closed"`, `11:30:00Z`, `bob`
  - `hist-whitespace`, `"  start  "`, `"  padded reason  "`, `nil`→`nil`, `12:00:00Z`, `"  carol  "`
  - `hist-legacy-transition`, `"close"`, `"legacy close"`, `"todo"`→`"done"`, `12:30:00Z`, `carol` (inserted after dropping `issues_status_check`, line 88)
  - `hist-legacy-nontransition`, `"touch"`, `"spelling differs only"`, `"in-progress"`→`"in_progress"`, `12:45:00Z`, `carol`
- Dumps below the migration gate (`DumpRaw`), extracts only the `issue_history` table into a single-table `RawDump`, and asserts `DeterministicMap` accepts it (line 110-112).
- Applies, then compares against the oracle: the in-place forward migration's `translateIssueHistoryToEvents` output read via `store.Export`.
- Asserts the oracle produced exactly **8** events whose ids start with `hist-` (line 130-132), and `reflect.DeepEqual` of the two normalized maps (line 133).
- `normEvent` normalization (lines 18-53) compares `IssueID, Action, Reason, Actor, CreatedAt` (as `ev.CreatedAt.UTC().Format(time.RFC3339Nano)`) and `Changes` sorted by `(Field, From, To)`; event `ID` is the map key. Attribution is deliberately excluded.

### 8. JSON wire form (shapemap_json.go)

#### 8.1 Wire discriminator constants

`fieldSourceKind string` (shapemap_json.go:34): `sourceColumn = "column"` (37), `sourceConst = "const"` (38).
`whenKind string` (shapemap_json.go:42): `whenAlways = "always"` (45), `whenChanged = "changed"` (46).

#### 8.2 Wire structs — exact key names, types, omitempty

`fieldWire` (shapemap_json.go:49-55):
- `field` string (no omitempty)
- `source` string (`fieldSourceKind`; no omitempty)
- `column` string, `omitempty`
- `transform` string (`Transform`), `omitempty`
- `value` string, `omitempty`

`whenWire` (57-61): `kind` string (no omitempty), `fieldA` string `omitempty`, `fieldB` string `omitempty`.

`emitterWire` (63-67): `collection` string (no omitempty), `when` object (no omitempty), `fields` array of `fieldWire` (no omitempty → encodes as `null` when the emitter has no fields).

`dropWire` (69-73): `column` string, `provenance` string (`DropProvenance`, no omitempty), `reason` string `omitempty`.

`tableWire` (75-79): `table` string (no omitempty), `emitters` array `omitempty`, `drops` array `omitempty`.

`mappingWire` (81-83): `tables` array (no omitempty).

Concrete document shape:
```json
{"tables":[{"table":"issues",
  "emitters":[{"collection":"issues","when":{"kind":"always"},
    "fields":[{"field":"closed_at","source":"column","column":"closed_at","transform":"timestamp"},
              {"field":"field","source":"const","value":"status"}]}],
  "drops":[{"column":"obsolete","provenance":"unexplained","reason":"no target in baseline"}]}]}
```

#### 8.3 `MarshalJSON` — value receiver on `ShapeMapping` (shapemap_json.go:88-108)

- `wire.Tables` is `make([]tableWire, 0, len(m.Tables))`, so an empty mapping encodes as `{"tables":[]}`, never `null`.
- Per table: emitters encoded in slice order then sorted; drops enumerated from the map then sorted by `Column` ascending (shapemap_json.go:99-103).
- Tables sorted by `Table` ascending (shapemap_json.go:106).
- Emitters sorted by `emitterSortKey` (shapemap_json.go:158-162), which is `collection \0 when.kind \0 when.fieldA \0 when.fieldB` then, per already-name-sorted field, `\x01 field \0 source \0 column \0 transform \0 value` (shapemap_json.go:167-189).
- Fields within an emitter sorted by `Field` ascending (shapemap_json.go:145).
- `FromColumn` → `source:"column"`, `column`, `transform` (shapemap_json.go:127-129). `Constant` → `source:"const"`, `value` (137-139). A `Constant` whose `Value` is not a `string`:
  ```
  shapemapping: table %q field %q constant must be a string, got %T
  ```
  (shapemap_json.go:136). A third `FieldSource` type:
  ```
  shapemapping: table %q field %q has unencodable source %T
  ```
  (shapemap_json.go:141).
- `Always` → `{"kind":"always"}`; `WhenChanged` → `{"kind":"changed","fieldA":…,"fieldB":…}` (fieldA/fieldB omitted when empty). Any other `EmitCondition` — including a **nil** `When`:
  ```
  shapemapping: table %q emitter has unencodable condition %T
  ```
  (shapemap_json.go:121).

#### 8.4 `UnmarshalJSON` — pointer receiver (shapemap_json.go:194-231)

Rules, in order:
1. Decoder with `DisallowUnknownFields()` (shapemap_json.go:199-200); any unrecognized JSON key anywhere in the document is a decode error from `encoding/json` (message contains `unknown field`).
2. Trailing-data guard (shapemap_json.go:208-214): a second `dec.Decode` must yield `io.EOF`. If it returns `nil` (another value present):
   ```
   shapemapping: unexpected trailing data after the mapping document
   ```
   If it returns any other error (unparseable junk):
   ```
   shapemapping: malformed trailing data after the mapping document: %w
   ```
3. Duplicate table (shapemap_json.go:219-221):
   ```
   shapemapping: duplicate disposition for table %q
   ```
4. `tableFromWire` (233-252): `Drops` map is only allocated when `len(tw.Drops) > 0` (so a table with no drops decodes with `Drops == nil`); a repeated drop column:
   ```
   shapemapping: table %q drops column %q more than once
   ```
   Drop `provenance` and `reason` are copied verbatim with no validation here.
5. `emitterFromWire` (254-281): `Collection` is `collection(ew.Collection)` verbatim, unvalidated. Condition kind switch: `"always"` → `Always{}`, `"changed"` → `WhenChanged{FieldA, FieldB}`, anything else (including empty):
   ```
   shapemapping: table %q emitter has unknown condition kind %q (want %q or %q)
   ```
   with the last two `%q` rendering `always` and `changed` (shapemap_json.go:262-263).
6. Duplicate field within an emitter (shapemap_json.go:267-269):
   ```
   shapemapping: table %q emitter into %q assigns field %q more than once
   ```
7. Source kind switch (270-278): `"column"` → `FromColumn{Column: fw.Column, Transform: fw.Transform}` (transform string copied verbatim, unvalidated); `"const"` → `Constant{Value: fw.Value}` (always a Go `string`); anything else:
   ```
   shapemapping: table %q field %q has unknown source %q (want %q or %q)
   ```
   with the last two rendering `column` and `const`.
8. `m.Tables = tables` where `tables` is `make([]TableMapping, 0, len(wire.Tables))` — a `{"tables":[]}` document yields a non-nil empty slice.

Explicitly NOT checked at decode (documented at shapemap_json.go:24-31 and observable in the code): totality, target resolution, transform admissibility, collection validity, required coverage, `WhenChanged` field existence, drop provenance validity. All of those are Validate's.

#### 8.5 What `shapemap_json_test.go` pins

- `TestShapeMappingJSONRoundTrip` (101-123): `Marshal → Unmarshal → Marshal` is byte-identical for a 6-table mapping plus a drop `{"obsolete": {Provenance: DropUnexplained, Reason: "no target in baseline"}}`.
- `TestShapeMappingJSONStableOrder` (127-140): two independent `Marshal` calls on the same mapping produce identical bytes.
- `TestShapeMappingJSONStableOrderMultiEmitter` (147-171): two emitters into `event_changes` sharing field names `event_id`/`field` but differing in source (`col("id")` vs `col("other")`, `Constant{"status"}` vs `Constant{"priority"}`) and condition (`Always{}` vs `WhenChanged{FieldA:"field", FieldB:"event_id"}`) encode identically regardless of in-memory order.
- `TestShapeMappingJSONRejectsUnknownSource` (177-186): input `{"tables":[{"table":"issues","emitters":[{"collection":"issues","when":{"kind":"always"},"fields":[{"field":"id","source":"transmute","column":"id"}]}]}]}` errors containing `unknown source`.
- `TestShapeMappingJSONRejectsUnknownConditionKind` (190-199): `"when":{"kind":"sometimes"}` errors containing `unknown condition kind`.
- `TestShapeMappingJSONRejectsDuplicateField` (204-216): two `{"field":"id",...}` entries error containing `more than once`.
- `TestShapeMappingJSONRejectsDuplicateTable` (220-229): `{"tables":[{"table":"issues","emitters":[]},{"table":"issues","emitters":[]}]}` errors containing `duplicate`.
- `TestShapeMappingJSONRejectsUnknownField` (235-244): `{"tables":[{"table":"issues","drops":[{"column":"x","prov":"unexplained"}]}]}` errors containing `unknown field`.
- `TestShapeMappingJSONRejectsTrailingData` (249-277): `{"tables":[]}{"tables":[]}` is rejected by `json.Unmarshal` and, when handed directly to `UnmarshalJSON`, errors containing `trailing data`; `{"tables":[]}@@@nonsense` errors containing both `trailing data` **and** `invalid character`.
- `TestDecodedMappingRecoversNovelAheadShape` (284-333): `DeterministicMap` must **decline** `novelAheadDump()` (an `issues` table whose `title` column is named `summary`, shapemap_json_test.go:79); the operator mapping (`aheadColumnMapping()`, lines 18-68, mapping `title ← summary`) marshaled and re-decoded, then passed to `Recover(ctx, canonicalUnder(t), dump, staticMapper(mapping), 1)`, yields a `Reconciled` outcome whose candidate store is Doctor-clean, exports exactly 2 issues, and preserves titles `"Open task"` and `"Done task"`.

Note `aheadColumnMapping()` uses `identity` on `issue_events.action/reason/actor` and on `event_changes.from/to`, and `legacy_status_value` on `issues.status` — all within the registry's `admits` sets.

### 9. What `shapemap_test.go` pins

- `TestValidateRejectsIncompleteMapping` (15-34): dump `issues(id,title)` with an emitter mapping only `id` → error text contains both `title` and `unaccounted`.
- `TestValidateRejectsStaleAndMalformedKeys` (36-90), dump `issues(id)`, four sub-cases and the substring each error must contain:
  | case | mapping fault | required substring |
  |---|---|---|
  | source column the dump lacks | `title ← column "ghost"` | `does not have` |
  | unknown target field | field name `nope` | `unknown field` |
  | unknown collection | `Collection: "ghosts"` | `unknown collection` |
  | unknown drop provenance | `Drops{"id": {Provenance:"guessed", Reason:"x"}}` | `unknown drop provenance` |
- `TestRejectsAmbiguousAlias` (98-134): an `issues` table carrying **both** `prompt` and `agent_prompt` — `DeterministicMap` must return `ok=false`; and a hand-built mapping covering only `agent_prompt` must fail `Validate` with `unaccounted`.
- `TestDeterministicMapDeclinesThinNonIssuesTable` (141-168): complete `issues` + `comments(issue_id)` only → `DeterministicMap` declines; the equivalent hand-built mapping fails `Validate` with `does not cover required field`.
- `fullIssuesMapping` helper (173-179) routes through `simpleEmitter(table, knownSourceColumns["issues"])` and panics `"fullIssuesMapping: issues table not recognized"` if it declines.
- `TestDeterministicMapDeclinesThinRequiredTargets` (185-211): declines `issues` missing `id`/`title` (columns `description,status,priority,issue_type,closed_at,created_at,updated_at`) and `issue_events` missing `reason`/`actor` (columns `id,issue_id,action,created_at`).
- `TestValidateRejectsRowArityMismatch` (217-230): 9 columns, an 8-cell row → error contains `cells, want`.
- `TestClassifyDropDistinguishesProvenance` (234-242): `classifyDrop("goose_db_version","version_id")` → `DropIntended` with a non-empty reason; `classifyDrop("issues","a_column_no_migration_ever_made")` → `DropUnexplained`.
- `TestParseDroppedColumnsReadsMigrationHistory` (244-261): from `"ALTER TABLE issues DROP COLUMN legacy_field;\nALTER TABLE \`relations\` DROP COLUMN IF EXISTS \`old_col\`;\nALTER TABLE issues DROP COLUMN a, DROP COLUMN b;"` the results must include `{issues,legacy_field}`, `{relations,old_col}`, `{issues,a}`, `{issues,b}`.
- `TestGooseUpSectionExcludesDownDrops` (267-285): an Up-`ADD`/Down-`DROP` migration yields zero refs; an Up-`DROP legacy`/Down-`ADD` migration yields exactly `[{issues, legacy}]`.
- `TestDeterministicMapCleanAhead` (299-383): a real v1 workspace (epic + child task + solo task, one comment, one label, one `Start` transition) is dumped with `DumpRaw`, mapped, validated, applied, and `ReplaceFromExport`ed into a fresh store; `Doctor` must report `IntegrityCheck == "ok"` with zero errors (`mustClean`, 289-294), every original issue id must survive, and the epic's status must round-trip as `""` (NULL), i.e. `epic.StatusValue() == ""`.
- `TestDeterministicMapPreGoose` (389-466): a hand-built pre-goose dump — `issues` with `prompt` (not `agent_prompt`), no `topic`/`item_rank`/`lane`/`resolution`/`redirect_target`/`archived_at`/`deleted_at`; `labels` with column `label`; `issue_events` with `assignee` (not `actor`); statuses `"todo"`/`"done"`; no `goose_db_version` table — is accepted by `DeterministicMap`, validates, and applies to: 2 issues; exactly 1 event with exactly 1 nested change; `export.Events[0].Actor == "claude"` (proving `issue_events.assignee → events.actor`). After `ReplaceFromExport`: Doctor-clean; `i1.Prompt == "the historical prompt"`; `i1.StatusValue() == "open"` (from `"todo"`); `i1.Topic == "misc"` (defaulted by the write path, not by the mapper); `i2.StatusValue() == "closed"` (from `"done"`).
- `TestApplyTransformPreservesNull` (470-486): `applyTransform(tf, nil)` returns `(nil, nil)` for `identity`, `legacy_status_value`, and `timestamp`; `applyTransform(TransformLegacyStatus, "todo")` returns `"open"`.
- `TestApplyRejectsCorruptTimestamp` (492-514): an `issues` row with `created_at = "not-a-timestamp"` makes `Apply` fail with an error containing all of `issues`, `created_at`, and `not-a-timestamp`.

### 10. Complete error-message inventory

Produced in `shapemap.go`:
1. `dump lists table %q more than once` (452-456)
2. `mapping dispositions table %q more than once` (466-470)
3. `mapping is not total: %d source column(s) unaccounted for: %s` (318)
4. `mapping is malformed: %s` (338) — wrapping any of:
   - `table %q: mapping references a table the dump does not have` (325)
   - `table %q: drop names column %q the dump does not have` (387)
   - `table %q column %q is both mapped and dropped` (390)
   - `table %q column %q: unknown drop provenance %q` (393)
   - `table %q: emitter into unknown collection %q` (404)
   - `table %q: emitter into %q targets unknown field %q` (411)
   - `table %q: %q.%q maps from column %q the dump does not have` (417)
   - `table %q: %q.%q does not admit transform %q` (420)
   - `table %q: %q.%q is not a passthrough field; a constant cannot land here` (424)
   - `table %q: %q.%q constant must be a string, got %T` (427)
   - `table %q: %q.%q has unknown field source %T` (430)
   - `table %q: emitter into %q does not cover required field %q` (436)
   - `table %q: emitter condition references field %q the emitter does not produce` (443)
5. `table %q row %d has %d cells, want %d (one per column)` (349-350)
6. `table %q: %w` (Apply's per-table wrapper, 516)
7. `column %q: %w` (buildRecord's per-column wrapper, 545)
8. `%s requires a string cell, got %T` (723)
9. `%s: %w` for the four canonicalizing transforms (738, 745, 751, 757)
10. `unknown transform %q` (761)
11. `expected a string or NULL cell, got %T` (776)
12. `invalid timestamp %q` (789)
13. `event change references unknown event_id %q` (655)

Produced in `shapemap_json.go`:
14. `shapemapping: table %q emitter has unencodable condition %T` (121)
15. `shapemapping: table %q field %q constant must be a string, got %T` (136)
16. `shapemapping: table %q field %q has unencodable source %T` (141)
17. `shapemapping: unexpected trailing data after the mapping document` (211)
18. `shapemapping: malformed trailing data after the mapping document: %w` (213)
19. `shapemapping: duplicate disposition for table %q` (220)
20. `shapemapping: table %q drops column %q more than once` (246)
21. `shapemapping: table %q emitter has unknown condition kind %q (want %q or %q)` (262-263)
22. `shapemapping: table %q emitter into %q assigns field %q more than once` (268)
23. `shapemapping: table %q field %q has unknown source %q (want %q or %q)` (276-277)

Panics in `shapemap_known.go`:
24. `scan migration drops: read embedded registry: <err>` (295)
25. `scan migration drops: read <file>: <err>` (302)

`DeterministicMap` itself produces no error text — it returns `(ShapeMapping{}, false)` (shapemap_known.go:38, 43).

### 11. Behavioral edges observable in the code

- `issues.lane` is a registered target and `lane` is in `knownSourceColumns["issues"]` (shapemap_known.go:180, shapemap.go:242), but `buildIssue` never reads `rec["lane"]` (shapemap.go:673-706): the value satisfies totality and is then discarded at assembly.
- `event_changes.from`/`.to` are `optional` targets (shapemap.go:262-263), so an `event_changes` emitter may omit them; `cellString(nil)` then yields `""` in the `model.FieldChange` (shapemap.go:659-660).
- `emits` returns `true` for a `nil` `When` (shapemap.go:562-565), and `Validate` never requires `When` to be non-nil; only `MarshalJSON` rejects it (shapemap_json.go:121).
- `Constant` is accepted by `Validate` on any field whose `canonical` is `identity` (shapemap.go:423) — including optional ones — but `MarshalJSON`/decode restrict the value to `string`.
- A `Constant` value passes through `buildRecord` untransformed (shapemap.go:549), then through `cellString`/`cellInt`/`cellTime` at assembly; a constant on a `timestamp`-canonical field is rejected by `Validate` (shapemap.go:423-425).
- `simpleEmitter` reassigns `coll` on every column (shapemap_known.go:78); an empty-column table would produce an emitter with `Collection: ""`, which `Validate` rejects as an unknown collection.
- `Apply` re-runs `Validate` internally (shapemap.go:502), so callers that already validated pay the cost twice; `DeterministicMap` also validates (shapemap_known.go:42).


---

## Issue IDs, Labels, Relations, Ranking — raw behavioral inventory

Scope: `internal/store/issue_ids.go`, `internal/store/labels.go`, `internal/store/relations.go`, `internal/store/ranking.go`, plus the collaborators those files bind to (`internal/issueid`, `internal/rank`, `internal/model/label.go`, `internal/model/relation_type.go`, `internal/store/row_deletes.go`, schema in `internal/store/migrations/00001_baseline.sql`) and the named tests. Every claim cites file:line.

---

## 1. Schema facts these subsystems write to

- `issues.item_rank TEXT NOT NULL DEFAULT ''` — `internal/store/migrations/00001_baseline.sql:63`.
- `issues.id VARCHAR(191) PRIMARY KEY` — `internal/store/migrations/00001_baseline.sql:50`.
- `relations` table: `src_id VARCHAR(191) NOT NULL`, `dst_id VARCHAR(191) NOT NULL`, `type VARCHAR(32) NOT NULL`, `created_at VARCHAR(64) NOT NULL`, `created_by TEXT NOT NULL`, `PRIMARY KEY (src_id, dst_id, type)`, `FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE`, `FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE`, `CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'))` — `internal/store/migrations/00001_baseline.sql:71-81`.
- `labels` table: `issue_id VARCHAR(191) NOT NULL`, `label VARCHAR(191) NOT NULL`, `created_at VARCHAR(64) NOT NULL`, `created_by TEXT NOT NULL`, `PRIMARY KEY (issue_id, label)`, `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE` — `internal/store/migrations/00001_baseline.sql:96-103`.
- Indexes: `CREATE INDEX idx_issues_rank ON issues(item_rank(191));` (`:133`), `CREATE INDEX idx_relations_src_type ON relations(src_id, type);` (`:136`), `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);` (`:139`), `CREATE INDEX idx_labels_issue ON labels(issue_id, label);` (`:145`), `CREATE INDEX idx_labels_name ON labels(label, issue_id);` (`:148`).
- Down section drops `labels` (`:162`) and `relations` (`:168`).
- All timestamps written by these subsystems use `time.RFC3339Nano` and are read back through `scanTime` = `time.Parse(time.RFC3339Nano, value)` — `internal/store/store.go:2215-2220`.

---

## 2. ISSUE IDs

### 2.1 Constants (literal values)

`internal/issueid/generate.go:12-18`:
- `CollisionProbabilityThreshold = 0.25`
- `MinHashLength = 3`
- `MaxHashLength = 8`
- `NonceAttempts = 10`
- `Base36Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"`

`internal/issueid/slug.go:8-13`:
- `PrefixMinLength = 3`
- `PrefixMaxLength = 12`
- `TopicMinLength = 3`
- `TopicMaxLength = 30`

### 2.2 ID grammar

Top-level ID is built by `fmt.Sprintf("%s-%s-%s", prefix, topic, shortHash)` — `internal/issueid/generate.go:46`. So: `<prefix>-<topic>-<hash>` where prefix and topic are normalized slugs and hash is `length` base-36 lowercase characters (`Base36Alphabet`, `internal/issueid/generate.go:17`, used at `:72`).

Child ID is `fmt.Sprintf("%s.%d", parentID, maxChildNumber+1)` — `internal/store/issue_ids.go:72`. Children are therefore dotted decimal suffixes appended to the full parent ID, and grandchildren nest (`parent.1.2`) because the child of a child re-applies the same rule at `internal/store/issue_ids.go:44-72`.

"Top-level" is defined in SQL as an id with no dot: `SELECT COUNT(*) FROM issues WHERE id NOT LIKE ?` with argument `"%.%"` — `internal/store/issue_ids.go:90`.

The test locking the shape: `GenerateHashID("proj","storage",...)` must start with `"proj-storage-"` and the remainder must have exactly `length` characters, checked for lengths `MinHashLength`, `5`, `MaxHashLength` — `internal/issueid/generate_test.go:27-39`.

### 2.3 Slug normalization (prefix and topic)

`NormalizeSlug` — `internal/issueid/slug.go:15-29`:
- Input is `strings.TrimSpace` then `strings.ToLower`d before iteration (`:18`).
- Runes in `a-z` or `0-9` are kept verbatim (`:20-22`).
- Any other rune becomes a single `-`, and consecutive non-alphanumerics collapse to one `-` (`previousDash` guard, `:23-26`).
- Leading/trailing `-` are trimmed: `strings.Trim(builder.String(), "-")` (`:28`).

`NormalizeConfiguredPrefix` — `internal/issueid/slug.go:31-45`:
- Normalizes via `NormalizeSlug` (`:33`).
- Empty result → error `"issue prefix is required"` (`:35`).
- Longer than `PrefixMaxLength` (12) → truncated to 12 bytes then re-trimmed of `-` (`:37-40`) — no error.
- Shorter than `PrefixMinLength` (3) after normalization → error `fmt.Errorf("issue prefix must be at least %d characters after normalization", PrefixMinLength)`, i.e. `"issue prefix must be at least 3 characters after normalization"` (`:41-43`).

`NormalizeTopicForCreate` — `internal/issueid/slug.go:47-59`:
- Empty result → `"topic is required"` (`:50`).
- Shorter than 3 → `"topic must be at least 3 characters after normalization"` (`:53`).
- Longer than 30 → `"topic must be at most 30 characters after normalization"` (`:56`). Note the asymmetry with prefix: topic over-length is an error, prefix over-length is silently truncated.

Callers at creation: topic normalized before the transaction (`internal/store/store.go:487-490`), prefix normalized inside the transaction with error wrapped as `fmt.Errorf("normalize issue prefix: %w", err)` (`internal/store/store.go:516-519`).

### 2.4 Hash minting

`GenerateHashID(prefix, topic, title, description, creator, createdAt, length, nonce)` — `internal/issueid/generate.go:42-47`:
- Content string: `fmt.Sprintf("%s|%s|%s|%s|%d|%d", topic, title, description, creator, createdAt.UnixNano(), nonce)` (`:43`). Note the prefix is NOT part of the hashed content — only of the rendered ID.
- `sha256.Sum256` of that content (`:44`).
- Takes the first `hashBytesForLength(length)` bytes and base-36 encodes to exactly `length` chars (`:45`).

`hashBytesForLength` — `internal/issueid/generate.go:49-62`: `3→2`, `4→3`, `5→4`, `6→4`, `7→5`, `8→5`, default `→3`. Test pins these plus `99→3` — `internal/issueid/generate_test.go:43-51`.

`encodeBase36(data, length)` — `internal/issueid/generate.go:64-86`:
- Big-int division by 36, digits from `Base36Alphabet`, most-significant first (`:65-78`).
- Left-pads with `"0"` when shorter than `length` (`:79-81`); tests: all-zero bytes → `"000000"` at length 6, `[]byte{1}` → `"000001"` — `internal/issueid/generate_test.go:59-76`.
- Truncates by taking the **tail** when longer: `value = value[len(value)-length:]` (`:82-84`); test asserts the clamped value equals the tail of the full encoding — `internal/issueid/generate_test.go:78-88`.

Determinism: same inputs + same nonce → same ID; different nonce → different ID — `internal/issueid/generate_test.go:11-25`.

### 2.5 Adaptive length

`ComputeAdaptiveLength(numIssues)` — `internal/issueid/generate.go:22-29`: returns the smallest `length` in `[3,8]` with `CollisionProbability(numIssues, length) <= 0.25`; falls through to `MaxHashLength` (8).

`CollisionProbability(numIssues, idLength)` — `internal/issueid/generate.go:32-36`: `1 - exp(-(n*n) / (2 * 36^idLength))` (birthday bound).

`getAdaptiveIssueIDLength` — `internal/store/issue_ids.go:80-86`: counts top-level issues, returns `issueid.ComputeAdaptiveLength(count)`; on count error returns `(6, err)`.

`countTopLevelIssues` — `internal/store/issue_ids.go:88-94`: `SELECT COUNT(*) FROM issues WHERE id NOT LIKE ?` with `"%.%"`. Counts across all prefixes and includes soft-deleted/archived rows (no `deleted_at` filter).

### 2.6 Minting algorithm and collision handling

`newIssueID` — `internal/store/issue_ids.go:14-19`: if `strings.TrimSpace(parentID) != ""` → child path, else top-level path.

`newTopLevelIssueID` — `internal/store/issue_ids.go:21-42`:
1. `baseLength, err := getAdaptiveIssueIDLength(...)`; on error `baseLength = 6` (`:22-25`).
2. Clamp: `if baseLength > issueid.MaxHashLength { baseLength = issueid.MaxHashLength }` (`:26-28`).
3. For `length` from `baseLength` to `MaxHashLength` (8) inclusive, for `nonce` from `0` to `NonceAttempts-1` (0..9): generate candidate and run `SELECT COUNT(*) FROM issues WHERE id = ?` (`:29-35`). Query error → `fmt.Errorf("check issue id collision: %w", err)` (`:34`).
4. First candidate with `count == 0` is returned (`:36-38`). The existence check is over ALL issues including soft-deleted ones.
5. Exhaustion error: `fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, issueid.MaxHashLength)` (`:41`).

`newChildIssueID` — `internal/store/issue_ids.go:44-73`:
1. `SELECT id FROM issues WHERE id LIKE ?` with `parentID + ".%"` (`:45`); error → `fmt.Errorf("query child ids: %w", err)` (`:47`).
2. For each row, `suffix := strings.TrimPrefix(candidate, parentID+".")`; skip if suffix is empty or contains a `.` (i.e., only direct children count) (`:57-60`).
3. `strconv.Atoi(suffix)`; non-numeric suffixes are skipped silently (`:61-64`).
4. Track `maxChildNumber` (`:65-67`); scan error → `fmt.Errorf("scan child id: %w", err)` (`:55`); iteration error → `fmt.Errorf("iterate child ids: %w", err)` (`:70`).
5. Return `fmt.Sprintf("%s.%d", parentID, maxChildNumber+1)` — first child is `.1` since `maxChildNumber` starts at 0 (`:51`, `:72`). Deleted children still occupy their number because the LIKE query has no `deleted_at` filter, so numbers are never reused while the row exists.

### 2.7 Parsing / validation of an existing ID

There is no ID parser or validator in this slice: lookups bind the id verbatim (e.g. `requireIssueExistsTx` runs `SELECT 1 FROM issues WHERE id = ?` — `internal/store/store.go:1603-1613`), and `ListIssues`' `IDs` filter only applies `strings.TrimSpace` and drops empties before `i.id IN (...)` — `internal/store/store.go:648-660`. No case normalization is applied to a supplied ID anywhere in these files.

---

## 3. LABELS

### 3.1 Canonical form

`model.NormalizeLabel` — `internal/model/label.go:14-23`:
- `strings.ToLower(strings.TrimSpace(label))` (`:15`).
- Empty after trim → `errors.New("label is required")` (`:17-19`).
- Contains a comma → `errors.New("label cannot contain commas")` (`:20-22`).
- No other characters are rejected; no length cap in code (the column is `VARCHAR(191)`, `migrations/00001_baseline.sql:98`).

`store.normalizeLabel` is a pass-through wrapper — `internal/store/labels.go:133-135`.

`canonicalizeLabels(labels)` — `internal/store/labels.go:112-128`: normalizes each (propagating the first error), de-duplicates on the normalized value keeping first occurrence (`:120-123`), then `sort.Strings(out)` (`:126`). Result is ascending-sorted, unique, lowercase.

### 3.2 AddLabel

`Store.AddLabel(ctx, storage.AddLabelInput{IssueID, Name, CreatedBy})` — `internal/store/labels.go:15-39`:
1. `s.GetIssue(ctx, in.IssueID)`; error returned as-is (a missing issue yields `storage.NotFoundError` from GetIssue) (`:16-18`).
2. `normalizeLabel(in.Name)`; error returned (`:19-22`).
3. `createdBy := strings.TrimSpace(in.CreatedBy)`; empty → `"unknown"` (`:23-26`).
4. Inside `s.withMutation(ctx, "add label", ...)`:
   ```sql
   INSERT INTO labels(issue_id, label, created_at, created_by)
   VALUES (?, ?, ?, ?)
   ON DUPLICATE KEY UPDATE issue_id = issue_id
   ```
   bound with `in.IssueID`, normalized label, `time.Now().UTC().Format(time.RFC3339Nano)`, `createdBy` — `internal/store/labels.go:28-30`. The `ON DUPLICATE KEY UPDATE issue_id = issue_id` makes a re-add a no-op that preserves the original `created_at`/`created_by`. Error → `fmt.Errorf("insert label: %w", err)` (`:32`).
5. Returns `s.ListLabels(ctx, in.IssueID)` — the full label set after the add (`:38`).

### 3.3 RemoveLabel

`Store.RemoveLabel(ctx, issueID, labelName)` — `internal/store/labels.go:41-63`:
1. `GetIssue` existence check (`:42-44`); 2. `normalizeLabel(labelName)` (`:45-48`).
3. Inside `s.withMutation(ctx, "remove label", ...)`: `deleteLabelTx(ctx, tx, labelKey{issueID, label})` (`:51`), which runs `DELETE FROM labels WHERE issue_id = ? AND label = ?` — `internal/store/row_deletes.go:97-100`.
4. `affected == 0` → `storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, label)}` (`:55-57`), rendering as `label "<issueID>/<label>" not found` (`internal/storage/errors.go:18-20`).
5. Returns `s.ListLabels(ctx, issueID)` (`:62`).

Error-vs-not-found distinction: `execDelete` wraps a delete failure as `fmt.Errorf("delete %s: %w", subject, err)` and a `RowsAffected()` failure as `fmt.Errorf("delete %s: rows affected: %w", subject, err)`, where subject is `fmt.Sprintf("label %s:%s", key.issueID, key.name)` — `internal/store/row_deletes.go:97-99`, `:109-119`. `TestRemoveLabelSurfacesGenuineRowsAffectedError` injects a driver whose `RowsAffected()` always fails and asserts the returned error is NOT a `storage.NotFoundError` and wraps the injected cause — `internal/store/relations_rows_affected_test.go:53-80`, fault harness at `:89-225`.

### 3.4 ReplaceLabels / replaceLabelsTx

`Store.ReplaceLabels(ctx, issueID, labels, createdBy)` — `internal/store/labels.go:65-76`: `GetIssue` check, `canonicalizeLabels`, then `s.withMutation(ctx, "replace labels", ...)` calling `replaceLabelsTx`.

`replaceLabelsTx` — `internal/store/labels.go:95-110`:
- `DELETE FROM labels WHERE issue_id = ?` — clears the whole set; error → `fmt.Errorf("clear labels: %w", err)` (`:96-98`).
- `author := strings.TrimSpace(createdBy)`; empty → `"unknown"` (`:99-102`).
- One `timestamp := time.Now().UTC().Format(time.RFC3339Nano)` shared by every inserted row (`:103`).
- Per label: `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)` (`:105`); error → `fmt.Errorf("insert label %q: %w", label, err)` (`:106`).
- Consequence: replacing a set rewrites `created_at`/`created_by` for labels that survive the replace.

`replaceLabelsTx` is also the label writer during `CreateIssue` — `internal/store/store.go:552-554`, fed by `canonicalizeLabels(in.Labels)` at `internal/store/store.go:482-485`.

### 3.5 ListLabels and other read paths

- `Store.ListLabels` — `internal/store/labels.go:78-93`: `SELECT label FROM labels WHERE issue_id = ? ORDER BY label ASC`; query error → `fmt.Errorf("list labels: %w", err)` (`:81`); scan errors returned bare; returns `nil` slice when there are no rows (the slice is never pre-allocated, `:84`).
- `loadLabelsByIssueIDs` — `internal/store/store.go:2366-2387`: `SELECT issue_id, label FROM labels WHERE issue_id IN (?, ?, …) ORDER BY label ASC`; error → `fmt.Errorf("load labels by issue ids: %w", err)`.
- `listAllLabels` — `internal/store/store.go:1782-1790`: `SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC`; error → `fmt.Errorf("list all labels: %w", err)`.
- List filtering by label — `internal/store/store.go:639-648`: `filter.LabelsAll` is run through `canonicalizeLabels` and each label adds a conjunct `EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)` (AND semantics, one clause per label).
- Import/restore insert: `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)` with error `fmt.Errorf("restore label %s:%s: %w", label.IssueID, label.Name, err)` — `internal/store/import_export.go:259-265`.
- Orphan check: `SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL` — `internal/store/import_export.go:60`.
- Delta replay uses `deleteLabelTx` + `insertLabelTx` keyed on `labelKey{issueID, name}` — `internal/store/export_delta.go:165`, `:227`.

### 3.6 Uniqueness / cascade

- Uniqueness is the `(issue_id, label)` primary key (`migrations/00001_baseline.sql:101`); `AddLabel` absorbs the duplicate via `ON DUPLICATE KEY UPDATE` (`internal/store/labels.go:30`), while `replaceLabelsTx`'s plain INSERT would surface a duplicate-key error — canonicalizeLabels' dedupe is what prevents that (`internal/store/labels.go:120-123`).
- Deleting an issue row removes its labels via `ON DELETE CASCADE` (`migrations/00001_baseline.sql:102`; noted at `internal/store/row_deletes.go:80-82`). Ordinary issue deletion is a soft `deleted_at` stamp, so no CRUD path triggers this cascade (`internal/store/row_deletes.go:55-58`).
- There is no label rename operation anywhere in the Go source (no rename symbol under `internal/` touching labels).

### 3.7 Test-pinned label behavior

`TestStoreLabelsAreWritableFirstClassData` — `internal/store/store_test.go:1169-1226`: creating with `Labels: []string{"Renderer", "gpu"}` yields `["gpu","renderer"]` (lowercased and sorted, `:1179-1185`); `AddLabel("contracts")` returns 3 labels (`:1188-1194`); `Apply` with `Labels: &[]string{"critical","renderer"}` replaces the set to exactly those two (`:1196-1202`); `RemoveLabel("critical")` returns `["renderer"]` (`:1204-1209`); the surviving label appears in `GetIssueDetail` (`:1216`) and in export as `export.Labels[0].Name == "renderer"` (`:1224`).

---

## 4. RELATIONS

### 4.1 The sealed kind set

`internal/model/relation_type.go:14-20`:
- `type RelationType string`
- `RelBlocks RelationType = "blocks"`
- `RelParentChild RelationType = "parent-child"`
- `RelRelatedTo RelationType = "related-to"`

`ParseRelationType(s)` — `internal/model/relation_type.go:26-33`: `strings.TrimSpace(s)` then matches the three constants; anything else → `errors.New("relation type must be blocks, parent-child, or related-to")`. This is the only string→type gate; the store does no re-validation (`internal/store/relations.go:294-296` comment).

### 4.2 Direction and canonicalization rules

- `StoreEndpoints(from, to)` — `internal/model/relation_type.go:40-45`: for `blocks` returns `(to, from)`; all other types pass through. It is its own inverse. Stored orientation for blocks is therefore **src = dependent, dst = dependency**, the reverse of the human "<blocker> blocks <blocked>" reading.
- `SingleValuedFromSrc()` — `internal/model/relation_type.go:54-56`: true only for `parent-child`. blocks and related-to are many-valued from src.
- `CanonicalEndpoints(src, dst)` — `internal/model/relation_type.go:62-67`: for `related-to`, if `dst < src` returns `(dst, src)` (lexicographic sort of the endpoint pair, giving an undirected edge exactly one representation); directed types unchanged.

Bucketing convention, `bucketRelations(focalID, relations, issuesByID)` — `internal/store/relations.go:22-59`:
- `RelBlocks` with `rel.SrcID == focalID` → the counterpart lands in `DependsOn` (`:30-36`); with `rel.DstID == focalID` → counterpart lands in `Blocks` (`:37-41`). Both branches run, so a self-blocks row would populate both.
- `RelParentChild` with `rel.SrcID == focalID` → `Parent = &<dst issue>` (`:42-47`); with `rel.DstID == focalID` → append to `Children` (`:48-52`).
- Counterparts absent from `issuesByID` are silently skipped (every `if …, ok :=` guard).
- `Children`, `DependsOn`, `Blocks` are initialized to empty (non-nil) slices; `Parent` stays nil (`:23-27`).
- All three slices are sorted by `sortIssuesByRank` (`:55-57`), which is a stable sort on `Rank` ascending with `ID` ascending as tiebreak — `internal/store/store.go:1771-1779`.
- `related-to` is not bucketed here at all.

`relatedFrom(focalID, relations, issuesByID)` — `internal/store/relations.go:64-80`: keeps only `RelRelatedTo` rows; the counterpart is `rel.SrcID`, or `rel.DstID` when `rel.SrcID == focalID` (`:69-72`); result sorted by rank (`:78`); returns an empty non-nil slice (`:65`).

`siblingsOf(focalID, parentChildren)` — `internal/store/relations.go:88-97`: returns the input minus the focal ID, order preserved; an only child yields an empty slice.

### 4.3 AddRelation

`Store.AddRelation(ctx, storage.AddRelationInput{SrcID, DstID, Type, CreatedBy})` — `internal/store/relations.go:293-341`:
1. Pre-tx: `in.Type == model.RelRelatedTo && in.SrcID == in.DstID` → `errors.New("related-to cannot target itself")` (`:296-298`). Note this self-check is only for related-to at this point.
2. `srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)` — related-to endpoints get sorted (`:299`).
3. `now := time.Now().UTC()`; the returned `model.Relation` is built with the canonical endpoints, the type, `now`, and `strings.TrimSpace(in.CreatedBy)`; empty createdBy → `"unknown"` (`:300-304`).
4. Inside `s.withMutation(ctx, "add relation", ...)`:
   - `requireIssueExistsTx(ctx, tx, in.SrcID)` then `requireIssueExistsTx(ctx, tx, in.DstID)` — note these use the **input** ids, not the canonicalized ones (`:309-314`). `requireIssueExistsTx` runs `SELECT 1 FROM issues WHERE id = ?` and maps `sql.ErrNoRows` to `storage.NotFoundError{Entity: "issue", ID: issueID}`, other errors to `fmt.Errorf("check issue exists: %w", err)` — `internal/store/store.go:1603-1613`. It deliberately does not filter on `deleted_at`, so archived/soft-deleted rows count as existing (`internal/store/store.go:1596-1602` comment).
   - If type is `blocks`: `rejectBlocksCycle(ctx, tx, rel.SrcID, rel.DstID)` (`:322-326`).
   - If `rel.Type.SingleValuedFromSrc()` (parent-child only): `setSingleValuedEdgeTx` (`:333-335`); otherwise `insertRelationTx` (`:336`).
5. Returns the constructed `model.Relation` (with canonical endpoints and the pre-tx timestamp) on success (`:340`).

Duplicate handling: a repeat of the same `(src,dst,type)` for `blocks`/`related-to` goes through the raw INSERT and hits the primary key, surfacing as `fmt.Errorf("insert relation %s->%s (%s): %w", ...)` from `insertRelationTx` (`internal/store/relations.go:348-353`). There is no upsert.

Endpoint-vanished behavior is pinned by `TestRelationEndpointVanishedRejected` — `internal/store/store_test.go:2588-2632`.

### 4.4 The write statements

`insertRelationTx` — `internal/store/relations.go:348-353`:
```sql
INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)
```
bound with `rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy`. Error: `fmt.Errorf("insert relation %s->%s (%s): %w", rel.SrcID, rel.DstID, rel.Type, err)`. This is the only relations INSERT in the package (also used by `CreateIssue`'s parent edge, `internal/store/store.go:540-551`, and by delta replay, `internal/store/export_delta.go:221`).

`setSingleValuedEdgeTx` — `internal/store/relations.go:365-370`:
```sql
DELETE FROM relations WHERE src_id = ? AND type = ?
```
bound `rel.SrcID, string(rel.Type)`; error → `fmt.Errorf("clear single-valued relation: %w", err)`; then `insertRelationTx`. Result: at most one edge of that type out of the src.

`deleteRelationRowTx(key relationKey{srcID,dstID,kind})` — `internal/store/row_deletes.go:87-91`:
```sql
DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?
```
subject string `fmt.Sprintf("relation %s->%s (%s)", key.srcID, key.dstID, key.kind)`; errors from `execDelete` are `delete relation a->b (blocks): <err>` and `delete relation a->b (blocks): rows affected: <err>` (`internal/store/row_deletes.go:109-119`).

`relationKey` is exactly the schema primary key `(src_id, dst_id, type)` — `internal/store/row_deletes.go:30-39`.

### 4.5 Cycle detection on write

`rejectBlocksCycle(ctx, tx, dependent, dependency)` — `internal/store/relations.go:378-390`:
- Self-edge: `dependent == dependency` → `fmt.Errorf("blocks: %s cannot block itself", dependent)` (`:379-381`).
- Loads every live blocks edge with `loadBlocksEdges` (see §5.7); load error → `fmt.Errorf("blocks cycle check: %w", err)` (`:384`).
- `blocksPrecedes(blocksPrecedenceAdj(edges), dependent, dependency)` true → error text (`:387`):
  `"blocks: cannot add %s depends-on %s — %s already depends on %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order"` with args `(dependent, dependency, dependency, dependent)`.

Tests: `TestAddRelationRejectsBlocksCycle` (A→B then B→A rejected) — `internal/store/store_test.go:580-599`; `TestAddRelationRejectsTransitiveBlocksCycle` (A→B, B→C, C→A rejected) — `:613-634`; `TestAddRelationRejectsSelfBlock` — `:637-647`.

### 4.6 RemoveRelation

`Store.RemoveRelation(ctx, srcID, dstID, relType)` — `internal/store/relations.go:392-407`:
- `srcID, dstID = relType.CanonicalEndpoints(srcID, dstID)` first (so related-to removal is order-insensitive) (`:393`).
- Inside `s.withMutation(ctx, "remove relation", ...)`: `deleteRelationRowTx` with that key.
- `affected == 0` → `storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}` (`:402-404`), rendering as `relation "src=… dst=… type=…" not found`.
- No existence check on the endpoints; no `GetIssue` precheck.

`TestRemovePerChildBlockAfterRankReorder` — `internal/store/store_test.go:2758-2820` — removes per-child and epic-level blocks edges after a `RankAbove`, asserting store orientation `src=dependent, dst=dependency` holds after reordering.

### 4.7 ClearParent

`Store.ClearParent(ctx, childID)` — `internal/store/relations.go:474-492`:
- `GetIssue(ctx, childID)` precheck (`:475-477`).
- Inside `s.withMutation(ctx, "clear parent", ...)`:
  ```sql
  DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'
  ```
  (type literal inlined, not a placeholder) — `:479`. Error → `fmt.Errorf("delete parent relation: %w", err)` (`:481`).
- `res.RowsAffected()` error → `fmt.Errorf("rows affected: %w", err)` (`:485`).
- `affected == 0` → `storage.NotFoundError{Entity: "parent relation", ID: childID}` (`:488`).
- `TestClearParentSurfacesGenuineRowsAffectedError` proves a failing `RowsAffected()` is not masked as NotFound and wraps the cause — `internal/store/relations_rows_affected_test.go:20-48`.

### 4.8 SetParent

`Store.SetParent(ctx, storage.SetParentInput{ChildID, ParentID, CreatedBy})` — `internal/store/relations.go:437-472`:
- Blank check: either id empty after `strings.TrimSpace` → `errors.New("child and parent ids are required")` (`:438-440`).
- `in.ChildID == in.ParentID` → `errors.New("child and parent cannot be the same issue")` (`:441-443`).
- Builds `model.Relation{SrcID: ChildID, DstID: ParentID, Type: RelParentChild, CreatedAt: time.Now().UTC(), CreatedBy: trimmed}`; empty CreatedBy → `"unknown"` (`:444-453`).
- In `s.withMutation(ctx, "set parent", ...)`: `requireIssueExistsTx` for child then parent, then `setSingleValuedEdgeTx` (`:458-467`). No ancestry/cycle check on parent-child — a parent cycle is only detected later at read time by `ancestorChain` (§5.3).
- Returns the relation value (`:471`).

`TestAddRelationEnforcesSingleParentCardinality` — `internal/store/store_test.go:527-576`: adding a second `parent-child` edge for a child through `AddRelation` succeeds and leaves exactly one parent edge (the newer one), same as `SetParent`.

### 4.9 ListRelationsForIssue and listRelations

`Store.ListRelationsForIssue(ctx, issueID, types...)` — `internal/store/relations.go:413-435`:
- `GetIssue` precheck (`:414-416`).
- `s.listRelations(ctx, issueID)`; with no `types` the full incident set is returned (`:417-423`).
- Otherwise filters in Go against a set of wanted types (`:424-433`); order is preserved from the SQL.

`Store.listRelations` — `internal/store/store.go:1647-1668`:
```sql
SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? OR dst_id = ? ORDER BY created_at ASC
```
Query error → `fmt.Errorf("list relations: %w", err)`; created_at parsed with `scanTime`; returns an empty non-nil slice.

### 4.10 Batch relations

`structuralRelationTypes = []model.RelationType{model.RelBlocks, model.RelParentChild}` — `internal/store/relations.go:153`. `related-to` is excluded from every batch path.

`relationEndpointColumns = map[string]struct{}{"src_id": {}, "dst_id": {}}` — `internal/store/relations.go:188`; a column not in that set → `fmt.Errorf("list relations by endpoint: unknown column %q", column)` (`internal/store/relations.go:195-197`).

`relationsByEndpoint(ctx, column, ids)` — `internal/store/relations.go:194-228`: builds
```sql
SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE <column> IN (?,…) AND type IN (?,?)
```
via `fmt.Sprintf` with placeholder lists from `repeatPlaceholder` (`internal/store/relations.go:276-282`); args are the ids then the two type strings (`:200-206`). No ORDER BY (ordering is `mergeRelations`' job, `:190-193`). Query error → `fmt.Errorf("list relations for ids: %w", err)` (`:210`). Scan of `created_at` goes through `scanTime` (`:220`).

`listRelationsForIDs(ctx, ids)` — `internal/store/relations.go:169-182`: empty ids → `(nil, nil)`; otherwise one query per endpoint column and `mergeRelations(bySrc, byDst)`. The documented reason for two conjunctive queries rather than one `src_id IN (…) OR dst_id IN (…)` is engine index-analysis blowup (`:158-168`).

`mergeRelations(bySrc, byDst)` — `internal/store/relations.go:235-259`: dedupes on `relationKey{srcID,dstID,kind}` keeping first occurrence (`:236-245`), then `slices.SortFunc` by `CreatedAt` ascending, then `SrcID`, then `DstID`, then `string(Type)` (`:246-257`). `TestMergeRelationsDedupesAndOrders` pins both legs, including a created_at tie broken by key — `internal/store/relations_batch_test.go:129-155`.

`Store.GetRelationsByIDs(ctx, ids)` — `internal/store/relations.go:106-147`:
- `dedupeStrings(ids)` preserving first-seen order (`:107`, helper at `:262-273`); empty → empty map, nil error (`:108-110`).
- Loads structural relations for the subjects (`:111-114`).
- Builds a `subjectSet` and a `needed` set seeded with the subjects; every relation endpoint is added to `needed` (`:115-124`).
- Buckets rows per subject; a row whose src and dst are both the same subject is added once (`rel.DstID != rel.SrcID` guard at `:128`).
- Hydrates `s.getIssuesByIDs(ctx, mapKeys(needed))` (`:132-135`; `mapKeys` at `:285-291`, unspecified order).
- For each subject present in `issuesByID`, produces `bucketRelations(id, bySubject[id], issuesByID)` with `.Issue` set; subjects that no longer exist are simply omitted from the result map (`:136-145`).
- `TestGetRelationsByIDsMatchesIssueDetail` asserts parity with `GetIssueDetail` for Children/DependsOn/Blocks/Parent, absence of a nonexistent subject, epic children in rank order, and the DependsOn/Blocks orientation — `internal/store/relations_batch_test.go:15-92`.

### 4.11 ListChildren

`Store.ListChildren(ctx, parentID)` — `internal/store/relations.go:494-519`:
- `GetIssue(parentID)` precheck (`:495-497`).
- ```sql
  SELECT <issueColumnsQualified>
  FROM relations r
  JOIN issues i ON i.id = r.src_id
  WHERE r.type = 'parent-child' AND r.dst_id = ?
  ORDER BY i.item_rank ASC, i.id ASC
  ```
  (`:498-502`). Query error → `fmt.Errorf("list children: %w", err)` (`:504`). Rows scanned with `scanIssue`, then `s.hydrateIssues` (`:507-518`). Note: no `deleted_at` filter on the child rows.
- `TestStoreListChildrenDefaultsToRankOrder` — `internal/store/store_test.go:1051-1082`.

---

## 5. RANKING

### 5.1 Rank representation

`internal/rank/rank.go` header (`:1-6`) — lexicographic fractional indexing; ranks are strings compared bytewise, stored in `issues.item_rank`.

- `alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"` — `internal/rank/rank.go:17` (base-62, digits then uppercase then lowercase, so ASCII order equals alphabet order).
- `base = len(alphabet)` = 62 — `internal/rank/rank.go:19`.
- `charIndex [256]int` maps byte→ordinal, `-1` for non-members, initialized in `init()` — `internal/rank/rank.go:22`, `:29-36`.
- `SmoothingThreshold = 8` — `internal/rank/rank.go:123`.
- `SmoothingWindow = 32` — `internal/rank/rank.go:126`.
- `minGap = 16` (local const inside `spacedRanks`) — `internal/rank/rank.go:160`.
- The empty string is the "unranked" sentinel: `Valid("")` is false — `internal/rank/rank.go:53-63`. Every rank query in the store excludes `item_rank != ''`.

`Initial()` returns `string(alphabet[base/2])` = `"V"` — `internal/rank/rank.go:39-41`.

`Valid(s)` — `internal/rank/rank.go:53-63`: false for empty; false if any byte is not in the alphabet; documented as a check for *persisted* values only, explicitly not the input contract of Midpoint/Before/After which accept an empty bound as a sentinel (`:45-52`).

### 5.2 Midpoint / Before / After

`Midpoint(a, b)` — `internal/rank/rank.go:69-118`:
- `a == b` → `errors.New("rank: a and b are equal")` (`:70-72`).
- both non-empty and `a >= b` → `errors.New("rank: a must be less than b")` (`:73-75`).
- Walks positions: missing/short `a` contributes virtual char index 0 ("below the floor"), missing/short `b` contributes virtual index `base` ("above the ceiling") (`:80-93`). An out-of-alphabet byte gives `errors.New("rank: invalid character in a")` (`:84`) or `"rank: invalid character in b"` (`:91`).
- If `bChar - aChar > 1`, emits `alphabet[aChar + (bChar-aChar)/2]` and returns (`:96-100`).
- Otherwise emits `alphabet[aChar]` and advances a position, growing the string by one char per adjacent/equal position (`:106-107`).
- Empty `a` means "before everything"; empty `b` means "after everything" (`:67-68`).

`Before(a)` = `Midpoint("", a)`, panicking `"rank.Before called with empty string"` on error — `internal/rank/rank.go:284-291`.
`After(a)` = `Midpoint(a, "")`, panicking `"rank.After called with empty string"` on error — `internal/rank/rank.go:295-302`.

### 5.3 Spaced ranks (used by smoothing)

`SpacedRanks(n)` — `internal/rank/rank.go:129-136`: `spacedRanks(n, "", "")`; panics `fmt.Sprintf("rank: spaced ranks with empty bounds failed: %v", err)` if that ever errors.

`SpacedRanksBetween(lower, upper, n)` — `internal/rank/rank.go:141-149`: `n == 0` → `(nil, nil)`; both bounds non-empty with `lower >= upper` → `errors.New("rank: lower must be less than upper")`; else `spacedRanks`.

`spacedRanks(n, lower, upper)` — `internal/rank/rank.go:151-196`:
- `n < 0` → `errors.New("rank: n must be non-negative")` (`:153-155`); `n == 0` → `(nil, nil)` (`:156-158`).
- Starting at `length = max(len(lower), len(upper)) + 1`, increments length until the integer span between the bounds divided by `n+1` is at least `minGap` (16) (`:161-183`). Lengths whose span is `<= 0` are skipped (`:177-179`).
- Emits `n` values at `lo + step*(i+1)` encoded fixed-width via `encodeBase62` (`:184-193`). All returned ranks share one length.
- `lowerBoundInt(s, length)` — `:200-212`: empty → 0; else `stringToInt(s, length)`, plus 1 when `len(s) >= length`.
- `upperBoundInt(s, length)` — `:216-232`: empty → `pow62(length)`; else `stringToInt(s,length) - 1`; a zero value → `errors.New("rank: upper bound too low to generate spaced ranks")`.
- `stringToInt` — `:236-250`: base-62 accumulation, right-padded with index 0; invalid byte → `errors.New("rank: invalid character in bounds")`.
- `encodeBase62(value, length)` — `:261-280`: negative → `errors.New("rank: cannot encode negative value")`; remainder out of `[0,62)` → `errors.New("rank: base62 remainder out of range")`; leftover quotient → `errors.New("rank: value does not fit fixed-width encoding")`.

### 5.4 Rank at creation

`nextRankForPlacement(ctx, tx, p)` — `internal/store/store.go:2045-2055`: `storage.RankTop` → `nextRankAtTop`; `storage.RankBottom` → `nextRankAtBottom`; anything else → `fmt.Errorf("unknown rank placement: %d", p)`.

`storage.RankPlacement` is an `int` with `RankBottom = iota` (0, the zero value and default) and `RankTop` (1) — `internal/storage/issues.go:22-27`.

`nextRankAtBottom` — `internal/store/store.go:2058-2068`:
```sql
SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' ORDER BY item_rank DESC LIMIT 1
```
non-`ErrNoRows` error → `fmt.Errorf("query last rank: %w", err)`; no row or empty → `rank.Initial()` ("V"); otherwise `rank.After(lastRank)`.

`nextRankAtTop` — `internal/store/store.go:2070-2081`: same query with `ORDER BY item_rank ASC`; error → `fmt.Errorf("query first rank: %w", err)`; no row → `rank.Initial()`; else `rank.Before(firstRank)`.

Called inside `CreateIssue`'s mutation right after ID minting — `internal/store/store.go:526-529`. Rank assignment is global (one flat keyspace across all issues regardless of parent).

### 5.5 The frame concept

Rank meaning is frame-local: an issue's rank is only compared against frame-mates (siblings under the same container, or fellow top-level items) — `internal/store/ranking.go:225-235`.

`ancestorChain(ctx, id)` — `internal/store/ranking.go:189-210`:
```sql
SELECT r.dst_id FROM relations r JOIN issues p ON p.id = r.dst_id
WHERE r.src_id = ? AND r.type = 'parent-child' AND p.deleted_at IS NULL
```
- Chain is self-first, root-last, starting `[id]` (`:190`).
- `sql.ErrNoRows` terminates the walk (`:197-199`); other error → `fmt.Errorf("ancestor chain of %s: %w", id, err)` (`:200-202`).
- Revisiting a seen node → `fmt.Errorf("ancestor chain of %s: parent cycle at %s", id, parent)` (`:203-205`).
- Only non-deleted parents are followed, so a soft-deleted parent truncates the chain.

`frameContainmentError` — `internal/store/ranking.go:216-223`: fields `containerID`, `containedID`; `Error()` = `fmt.Sprintf("%s is inside %s; no comparable frame contains both — rank it against a sibling instead", e.containedID, e.containerID)`.

`resolveFrameRepresentatives(chains)` — `internal/store/ranking.go:236-280`:
1. Builds per-chain membership maps id→index (`:237-244`).
2. Containment check: if any chain's head (`chain[0]`) appears in another chain at index > 0, returns `&frameContainmentError{containerID: chain[0], containedID: chains[j][0]}` (`:245-254`).
3. LCA: the first element of `chains[0]` present in every other chain (`:257-270`); `lcaID == ""` means no shared ancestor.
4. Representatives: with no LCA, each chain's **root** (`chain[len-1]`) (`:273-276`); with an LCA, the element **one level below** the LCA in each chain: `chain[memberships[i][lcaID]-1]` (`:277`). Frame-mates therefore resolve to themselves.

Pinned cases — `internal/store/ranking_frame_test.go:279-320`:
- `{{x},{y},{z}}` → `[x y z]`
- `{{c1,e},{c2,e},{c3,e}}` → `[c1 c2 c3]` (siblings rank directly)
- `{{c1,e},{x},{c2,e}}` → `[e x e]`
- `{{c1,e1,p},{c2,e2,p},{x}}` → `[p p x]`
- `{{c1,e1,p},{c2,e2,p}}` → `[e1 e2]`
- `{{g,s,e},{c,e}}` → `[s c]`
- `{{c,e},{e}}`, `{{e},{g,s,e}}`, `{{e},{x},{c,e}}` → `frameContainmentError`

`resolveComparableFrame(issueChain, targetChain)` — `internal/store/ranking.go:288-301`: wraps `resolveFrameRepresentatives` for the pair and rewrites containment errors:
- when the container is the issue itself: `fmt.Errorf("cannot rank %s relative to %s: %s contains it; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0])` (`:293`);
- otherwise: `fmt.Errorf("cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0], targetChain[0])` (`:295`).
Returns `(reps[0], reps[1])` as `(movedID, anchorID)`.

Pinned pair cases — `internal/store/ranking_frame_test.go:322-356`: top-level pair unchanged; same-epic siblings unchanged; standalone vs child → `(x, e)`; child vs standalone → `(e, x)`; two epics' children → `(e1, e2)`; grandchild vs child of shared epic → `(s, c)`; either containment direction errors.

`resolveRankPair(ctx, issueID, targetID)` — `internal/store/ranking.go:308-335`:
- `issueID == targetID` → `errors.New("cannot rank an issue relative to itself")` (`:309-311`).
- `GetIssue(targetID)` then `GetIssue(issueID)` — in that order (`:312-317`).
- Both ancestor chains, then `resolveComparableFrame` (`:318-329`).
- `GetIssue(anchorID)` for the hydrated anchor whose `Rank` seeds midpoint math (`:330-333`).
- Returns `(anchor, storage.RankMove{MovedID: movedID, AnchorID: anchorID})`.

`storage.RankMove{MovedID, AnchorID}` — `internal/storage/rank.go:9-12`; `storage.RankSetResolution{NamedID, RankedID}` with json tags `named_id`/`ranked_id` — `internal/storage/rank.go:19-22`.

### 5.6 The five rank verbs

**RankToTop(issueID)** — `internal/store/ranking.go:17-39`:
- `GetIssue` precheck.
- In `withMutation(ctx, "rank to top", …)`:
  ```sql
  SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id != ? ORDER BY item_rank ASC LIMIT 1
  ```
  non-`ErrNoRows` error → `fmt.Errorf("rank-to-top: query first: %w", err)` (`:23-26`).
- No/blank first rank → `rank.Initial()`; else `rank.Before(firstRank)` (`:28-32`).
- `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` with `time.Now().UTC().Format(time.RFC3339Nano)`; error → `fmt.Errorf("rank-to-top: update: %w", err)` (`:33-36`).
- Then `smoothRanksIfNeededTx(ctx, tx, newRank)` (`:37`).
- No frame resolution: the top is the global top.

**RankToBottom(issueID)** — `internal/store/ranking.go:161-183`: identical shape with `ORDER BY item_rank DESC`, errors `"rank-to-bottom: query last: %w"` (`:169`) and `"rank-to-bottom: update: %w"` (`:179`), and `rank.After(lastRank)` (`:175`).

**RankAbove(issueID, targetID)** — `internal/store/ranking.go:339-365`:
- `resolveRankPair` first (all its errors propagate).
- In `withMutation(ctx, "rank above", …)`:
  ```sql
  SELECT item_rank FROM issues WHERE item_rank < ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank DESC LIMIT 1
  ```
  bound `(target.Rank, move.MovedID)`; error → `fmt.Errorf("rank-above: query neighbor: %w", err)` (`:346-349`). Note this neighbor query has no `item_rank != ''` filter, so unranked rows sort as the empty string.
- No neighbor → `rank.Before(target.Rank)`; else `rank.Midpoint(aboveRank, target.Rank)` with error `fmt.Errorf("rank-above: midpoint: %w", err)` (`:350-358`).
- `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` on `move.MovedID`; error → `fmt.Errorf("rank-above: update: %w", err)` (`:359-362`).
- `smoothRanksIfNeededTx(ctx, tx, newRank)` (`:363`). Returns the `RankMove` regardless of which id the caller named.

**RankBelow(issueID, targetID)** — `internal/store/ranking.go:369-395`: mirror image — `item_rank > ? … ORDER BY item_rank ASC LIMIT 1`, errors `"rank-below: query neighbor: %w"` (`:378`), `"rank-below: midpoint: %w"` (`:386`), `"rank-below: update: %w"` (`:390`); no neighbor → `rank.After(target.Rank)`, else `rank.Midpoint(target.Rank, belowRank)`.

Frame behavior pinned by tests — `internal/store/ranking_frame_test.go`:
- Standalone above an epic child anchors to the epic; epic and all children keep their exact rank strings; standalone ends above the epic (`:55-79`).
- Child above a standalone moves the **epic**; children and anchor unchanged (`:81-105`).
- Across two epics, `RankBelow(child1, child2)` moves epic1 relative to epic2 (`:107-125`).
- Same-epic siblings rank directly (`:127-144`).
- `RankAbove(child, own epic)` errors containing `"inside"`; `RankBelow(epic, own child)` errors containing `"contains"` (`:146-158`).

**RankSet(ids)** — `internal/store/ranking.go:102-158`:
- Validation `rankSetValidateIDs` — `internal/store/ranking.go:41-56`: fewer than 2 ids → `errors.New("rank set: need at least 2 IDs to establish order")`; an empty-string id → `errors.New("rank set: empty ID in input")`; a repeated id → `fmt.Errorf("rank set: duplicate ID %q in input", id)`.
- `resolveRankSet` — `internal/store/ranking.go:65-91`: per id, `GetIssue` then `ancestorChain`; `resolveFrameRepresentatives` errors are wrapped `fmt.Errorf("rank set: %w", err)` (`:79`); two named ids collapsing to the same representative →
  `fmt.Errorf("rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead", prior, id, reps[i], reps[i])` (`:85`).
  Returns `[]storage.RankSetResolution{{NamedID, RankedID}}` parallel to the input order.
- In `withMutation(ctx, "rank set", …)`:
  ```sql
  SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id NOT IN (?,…) ORDER BY item_rank ASC LIMIT 1
  ```
  built with one placeholder per ranked id (`:117-127`); non-`ErrNoRows` error → `fmt.Errorf("rank-set: query top: %w", err)` (`:125-127`).
- Walks the ranked ids in reverse, assigning `rank.Initial()` for the first assignment when there is no cursor, else `rank.Before(cursor)`; cursor becomes the just-assigned rank (`:133-147`). Final order: `ids[0] < ids[1] < … < ids[N-1] < existing top` — the whole set is stacked at the top of the keyspace.
- One `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` per id, sharing a single `now` timestamp; error → `fmt.Errorf("rank-set: update %s: %w", id, err)` (`:148-152`).
- `smoothRanksIfNeededTx(ctx, tx, newRanks[0])` when the set is non-empty (`:153-156`).
- Atomic: all assignments in one mutation (`:96-101`).
- Note: `RankSet` returns `resolutions` alongside the mutation error, so resolutions are non-nil even when the write fails (`:114`).

Tests: absolute top ordering — `internal/store/store_test.go:2700-2730`; duplicates rejected with an error containing `"duplicate"` — `:2732-2743`; single id rejected with `"at least 2"` — `:2745-2756`; mixed-frame resolves child→epic with children unmoved — `internal/store/ranking_frame_test.go:160-188`; two epics' children resolve to their epics — `:190-215`; same-epic siblings resolve to themselves and only they move — `:217-243`; duplicate representatives rejected with `"both resolve to"` and **no** rank changed — `:245-263`; naming an epic with its own child rejected with `"inside"` in both orders — `:265-277`.

### 5.7 Smoothing (rebalancing)

`smoothRanksIfNeededTx(ctx, tx, triggerRank)` — `internal/store/ranking.go:401-500`:
1. Trigger: `len(triggerRank) < rank.SmoothingThreshold` (8) → no-op (`:402-404`). So smoothing fires only once a rank string reaches 8 characters.
2. `half := rank.SmoothingWindow / 2` = 16 (`:405`).
3. Below half:
   ```sql
   SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank <= ? ORDER BY item_rank DESC LIMIT ?
   ```
   bound `(triggerRank, half)` (`:415-417`); errors `"smooth: query below: %w"` (`:419`), `"smooth: scan below: %w"` (`:426`), `"smooth: below rows: %w"` (`:432`). The slice is then reversed to ascending (`:435-437`).
4. Above half:
   ```sql
   SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT ?
   ```
   (`:440-442`); errors `"smooth: query above: %w"` (`:444`), `"smooth: scan above: %w"` (`:449`), `"smooth: above rows: %w"` (`:456`).
5. Window fewer than 2 entries → no-op (`:459-461`).
6. Outside bounds:
   ```sql
   SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank < ? ORDER BY item_rank DESC LIMIT 1
   ```
   against `window[0].rank`, error → `"smooth: lower bound: %w"` (`:465-473`); and
   ```sql
   SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT 1
   ```
   against the last window rank, error → `"smooth: upper bound: %w"` (`:476-485`). Missing bounds stay `""` (meaning open-ended).
7. `rank.SpacedRanksBetween(lowerBound, upperBound, len(window))`; error → `fmt.Errorf("smooth: compute ranks: %w", err)` (`:487-490`).
8. `UPDATE issues SET item_rank = ? WHERE id = ?` for each window entry whose new rank differs from the old — `updated_at` is **not** touched here (`:492-498`); error → `fmt.Errorf("smooth: update %s: %w", item.id, err)` (`:495`).

Smoothing is invoked from `RankToTop` (`:37`), `RankSet` (`:154`), `RankToBottom` (`:181`), `RankAbove` (`:363`), `RankBelow` (`:393`), and `FixRankInversions` (`:872`). It ignores parent/epic frames entirely: the window is whatever is adjacent in the global rank keyspace.

### 5.8 Rank inversions

`rankInversionCandidatesClause` — `internal/store/ranking.go:518-523`:
```sql
FROM relations r
JOIN issues src ON src.id = r.src_id
JOIN issues dst ON dst.id = r.dst_id
WHERE r.type = 'blocks'
AND src.deleted_at IS NULL AND dst.deleted_at IS NULL
AND dst.item_rank > src.item_rank
```
Deliberately no status filter, because epics store `status IS NULL` and a SQL `status != 'closed'` would evaluate NULL for them (`:502-517`).

`rankInversion{depID, dependentID}` — `internal/store/ranking.go:525-528` (depID = dependency/blocker, dependentID = blocks-src).

`rowQueryer` interface with just `QueryContext` so the same loader runs on `*sql.DB` and `*sql.Tx` — `internal/store/ranking.go:535-537`.

`loadInversionCandidates(ctx, q)` — `internal/store/ranking.go:543-562`: `SELECT r.dst_id, r.src_id ` + the clause + ` ORDER BY src.item_rank ASC`; errors `fmt.Errorf("query: %w", err)` (`:547`), `fmt.Errorf("scan: %w", err)` (`:553`), `fmt.Errorf("rows: %w", err)` (`:559`).

`filterLiveInversions(candidates, liveIDs)` — `internal/store/ranking.go:568-578`: keeps a candidate only when both endpoints are in the live set.

`liveIssueIDs(ctx)` — `internal/store/ranking.go:588-598`: `s.ListIssues(ctx, storage.ListIssuesFilter{Statuses: []model.State{model.StateOpen, model.StateInProgress}})`; error → `fmt.Errorf("list live issues: %w", err)`. Archived and deleted issues are excluded by that listing; epics get their state by rollup over children rather than a column peek (`:580-587`).

`liveRankInversions(ctx)` — `internal/store/ranking.go:604-614`: live set + `loadInversionCandidates(ctx, s.db)` (error `fmt.Errorf("load inversion candidates: %w", err)`) + filter. Doctor counts `len(...)` of this and `FixRankInversions` consumes the same set (`:600-603`).

### 5.9 Blocks graph helpers

`blocksEdge{dependent, dependency}` — `internal/store/ranking.go:623-626` (src = dependent, ranked below; dst = dependency, ranked above).

`loadBlocksEdges(ctx, q)` — `internal/store/ranking.go:632-658`:
```sql
SELECT r.src_id, r.dst_id FROM relations r
JOIN issues src ON src.id = r.src_id
JOIN issues dst ON dst.id = r.dst_id
WHERE r.type = 'blocks'
AND src.deleted_at IS NULL AND dst.deleted_at IS NULL
ORDER BY r.src_id, r.dst_id
```
No rank pre-filter; the ORDER BY exists to make DFS adjacency order — and therefore the reported cycle path — deterministic (`:633-635`). Errors: `"query blocks edges: %w"` (`:643`), `"scan blocks edge: %w"` (`:650`), `"blocks edges rows: %w"` (`:655`).

`blocksPrecedenceAdj(edges)` — `internal/store/ranking.go:661-667`: adjacency `dependency -> []dependent`.

`blocksPrecedes(adj, from, to)` — `internal/store/ranking.go:673-692`: recursive DFS with a `seen` set; returns true as soon as `to` is reached. (The `seen` set is populated after the `next == to` check, so a node is compared before being marked.)

`filterLiveBlocksEdges(edges, liveIDs)` — `internal/store/ranking.go:697-707`: both endpoints must be live.

`findBlocksCycle(edges)` — `internal/store/ranking.go:712-756`: three-color DFS (`white = 0`, `gray = 1`, `black = 2` — `:719-723`) over adjacency keys sorted with `sort.Strings` (`:715-718`); on hitting a gray node it slices the current stack from that node and appends it again, returning a repeated-endpoint path `a -> b -> … -> a` (`:731-737`); returns nil for an acyclic graph.

`liveBlocksCycle(ctx)` — `internal/store/ranking.go:763-773`: live set, `loadBlocksEdges(ctx, s.db)` (error `fmt.Errorf("load blocks edges: %w", err)`), live filter, `findBlocksCycle`.

### 5.10 FixRankInversions

`Store.FixRankInversions(ctx) (int, error)` — `internal/store/ranking.go:778-882`:
1. Live set snapshotted **before** the transaction; error → `fmt.Errorf("fix rank inversions: snapshot live set: %w", err)` (`:785-788`). Liveness is not recomputed per iteration (`:779-784`).
2. In `withMutation(ctx, "fix rank inversions", …)`:
   - `loadBlocksEdges(ctx, tx)`; error → `fmt.Errorf("fix rank inversions: load blocks edges: %w", err)` (`:812-815`).
   - If `findBlocksCycle(filterLiveBlocksEdges(edges, liveIDs)) != nil` → `fmt.Errorf("fix rank inversions: blocks dependency cycle %s — a cycle has no valid rank order; break it by removing one edge with 'lit dep rm'", strings.Join(cycle, " -> "))` (`:816-818`).
   - Loop:
     a. `loadInversions` = candidates in-tx filtered by the snapshot live set (`:789-795`); error → `fmt.Errorf("fix rank inversions: %w", err)` (`:823`).
     b. Zero inversions → return nil (loop exit) (`:825-827`).
     c. Snapshot key = `strings.Join(parts, "|")` where each part is `inv.depID + "<-" + inv.dependentID` (`:796-802`, `:828`). A repeated snapshot → `fmt.Errorf("fix rank inversions: unable to converge in one run; remaining inversions=%d", len(inversions))` (`:829-831`).
     d. Targets: first inversion per distinct `depID` (later inversions sharing a dependency are skipped in this pass) (`:836-844`). Because candidates are ordered by `src.item_rank ASC`, the retained dependent is the highest-priority one.
     e. Per target: `SELECT item_rank FROM issues WHERE id = ?` for the dependent, error → `fmt.Errorf("fix rank inversions: read target rank %s: %w", target.dependentID, err)` (`:848-851`).
     f. `SELECT item_rank FROM issues WHERE item_rank < ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank DESC LIMIT 1` bound `(targetRank, target.depID)`; non-`ErrNoRows` error → `fmt.Errorf("fix rank inversions: query neighbor: %w", err)` (`:852-858`).
     g. No neighbor → `rank.Before(targetRank)`, else `rank.Midpoint(aboveRank, targetRank)` with error `fmt.Errorf("fix rank inversions: midpoint: %w", err)` (`:859-867`).
     h. `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` on the dependency; error → `fmt.Errorf("fix rank inversions: update %s: %w", target.depID, err)` (`:868-871`).
     i. `smoothRanksIfNeededTx`; error → `fmt.Errorf("fix rank inversions: smooth ranks: %w", err)` (`:872-874`).
     j. `rerankedCount++` per re-ranked dependency (`:875`).
3. On mutation error returns `(0, err)`; otherwise `(rerankedCount, nil)` (`:878-881`). The count counts dependency re-rank operations across all passes, not distinct issues.

Test-pinned behavior in `internal/store/store_test.go`:
- One dependency blocking two dependents: Doctor reports 2 inversions before, `FixRankInversions` returns 1, Doctor reports 0 after — `:294-342`.
- A pass that creates a new inversion still converges: 1 before, `>= 1` fixed, 0 after — `:344-391`.
- An epic dependency ranked below its dependent counts as 1 inversion and is fixed — `:399-444`.
- A closed epic dependency yields 0 inversions — `:447-483`.
- Deleted issues yield 0 inversions and 0 fixes — `:486-519`.
- A cycle injected past `AddRelation` makes `FixRankInversions` fail with a message naming the cycle rather than an opaque non-convergence — `:650-694`.

---

## 6. Cross-cutting notes on these four subsystems

- Every mutation in labels.go, relations.go and ranking.go runs through `s.withMutation(ctx, <label>, fn)` with labels: `"add label"`, `"remove label"`, `"replace labels"` (`internal/store/labels.go:27`, `:49`, `:73`), `"add relation"`, `"remove relation"`, `"set parent"`, `"clear parent"` (`internal/store/relations.go:305`, `:394`, `:454`, `:478`), `"rank to top"`, `"rank set"`, `"rank to bottom"`, `"rank above"`, `"rank below"`, `"fix rank inversions"` (`internal/store/ranking.go:21`, `:114`, `:165`, `:344`, `:374`, `:804`).
- Author attribution defaults to the literal `"unknown"` in `AddLabel` (`internal/store/labels.go:25`), `replaceLabelsTx` (`internal/store/labels.go:101`), `AddRelation` (`internal/store/relations.go:303`) and `SetParent` (`internal/store/relations.go:452`). `CreateIssue` uses `createdBy := "links"` for both the parent edge and the initial labels (`internal/store/store.go:490`, `:544`, `:553`).
- Rank queries split on the `deleted_at IS NULL` filter inconsistently: the top/bottom/creation queries and smoothing include it plus `item_rank != ''`; the `RankAbove`/`RankBelow`/`FixRankInversions` neighbor queries include `deleted_at IS NULL` but not `item_rank != ''` (`internal/store/ranking.go:346`, `:376`, `:853`).


---

