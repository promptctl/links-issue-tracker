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

