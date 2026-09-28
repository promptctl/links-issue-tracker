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

`const doltDatabaseName = "links"` (`internal/store/store.go`). Used as:
- the `Database` field of the embedded-driver config for every non-bootstrap pool (`store.go`);
- the `CREATE DATABASE IF NOT EXISTS links` argument (`store.go`);
- the on-disk directory probed for existence: `<doltRoot>/links/.dolt` (`store.go`).

Tests derive the journal-lock path from it as `<doltRoot>/links/.dolt/noms/LOCK` (`internal/store/engine_open_contract_test.go`) and the chunk journal as `<doltRoot>/links/.dolt/noms/<chunks.JournalFileID>` (`internal/store/dolt_journal_hold_test.go`).

#### 1.2 `engineAccess`

```go
type engineAccess int
const (
    engineRead engineAccess = iota
    engineWrite
)
```
(`store.go`). Two values only. Semantics:
- `engineWrite`: connector is given a `BackOff` (`store.go`), and `openStoreConnection` pings eagerly (`store.go`).
- `engineRead`: no `BackOff`; `openStoreConnection` pings eagerly for both values (`store.go`), and a read engine's ping takes Dolt's read-only fallback past a held journal lock rather than waiting (doc at `store.go`).

#### 1.3 `Store` struct — every field

`store.go`:

| Field | Type | Set where | Meaning per code |
|---|---|---|---|
| `db` | `*sql.DB` | `store.go`; replaced by `reconnect` at `store.go` | the one pooled embedded-Dolt connection |
| `workspaceID` | `string` | `store.go` | the workspace id passed to `Open`/`OpenForRead`; also the Dolt commit author basis (`store.go`) |
| `doltRootDir` | `string` | `store.go` (raw arg, not cleaned) | Dolt root dir |
| `access` | `engineAccess` | `store.go` | reused verbatim by `reconnect` (`store.go`) |
| `commitLockPath` | `string` | `store.go` = `commitLockPathForDolt(doltRootDir)` | flock path, `filepath.Join(filepath.Dir(filepath.Clean(doltRootDir)), ".links-commit-flock.lock")` (`commit_lock.go`) |
| `telemetryDir` | `string` | `store.go` = `filepath.Join(filepath.Clean(doltRootDir), "telemetry")` | never read inside `store.go` |
| `releaseWorkspaceLock` | `func() error` | `store.go` (Open), `store.go` (OpenForRead); cleared to `nil` on failure at `store.go`, and in `Close` at `store.go` | the workspace shared-lock release |
| `attribution` | `model.Attribution` | only by `AttributeTo` (`store.go`) | stamped on every `recordEvent` row (`store.go`) |
| `applyPreMutationHookForTest` | `func()` | nil in production; fired at `store.go` | test seam between planning and `withMutation` in `Apply` |
| `commitWorkingSetHookForTest` | `func() error` | nil in production; fired at `commit_lock.go` | test seam at the top of every `commitWorkingSetOnce` |

Both hooks are per-`Store` instance state, not package globals (`store.go`).

#### 1.4 `coResidentHolderWait`

`var coResidentHolderWait = mirrorHoldCeiling + coResidentWaitHeadroom` (`store.go`), = 1.5s (= `mirrorHoldBudget` 1s + `mirrorHoldCancelLag` 500ms) + 8 × `storeLockPollInterval` (100ms) = 2.3s. A package **variable**, not a const, so tests can shrink it; `engine_open_contract_test.go` sets it to `700 * time.Millisecond` and restores it in cleanup. It is the wait of every store lock acquisition (`acquireStoreLock`, `workspace_lock.go`), of the write-open backoff (`store.go`), of the chunk-store open `RecordPushedHead` runs (`pushed_head.go`), and a term of `commitLockWaiterBudget()` (`commit_lock.go`). It is read through `coResidentHolderWaitNow` at the moment a contender needs it (`store.go`).

#### 1.5 `newEngineOpenBackOff`

`store.go`. `newHoldWait(workspaceStorageDir(doltRootDir), DoltJournalLockPath(doltRootDir), coResidentHolderWaitNow)` per connector: `holdWait` (`lock_holder.go`) polls every `storeLockPollInterval` = 100ms and returns `backoff.Stop` once no holder-record name it has not seen before has appeared under the lock's holder directory for the wait; `Reset` starts the clock and takes the first reading. Only attached for `engineWrite` (`store.go`).

#### 1.6 `wrapEngineOpenContention`

`store.go`. If `err != nil && errors.Is(err, nbs.ErrDatabaseLocked)`, returns exactly:

```
fmt.Errorf("another process is holding this workspace's Dolt store open, %s; retry after it completes: %w (%w)", describeLockHolders(workspaceStorageDir(doltRootDir), DoltJournalLockPath(doltRootDir)), ErrWorkspaceBusy, err)
```

so the result satisfies both `errors.Is(err, ErrWorkspaceBusy)` and `errors.Is(err, nbs.ErrDatabaseLocked)`. Every other error passes through unchanged. `ErrWorkspaceBusy` is defined at `internal/store/workspace_lock.go` as `errors.New("workspace busy")`.

Call sites: `store.go` (eager write ping), `store.go` (reconnect ping), `store.go` (`ensureMasterDefaultBranch` inside Open), `store.go` (bootstrap CREATE DATABASE), `store.go` (bootstrap branch normalization).

Test evidence: a foreign holder of `<doltRoot>/links/.dolt/noms/LOCK` makes `Open` fail with `nbs.ErrDatabaseLocked` in the chain (`engine_open_contract_test.go`) and bounded (< 10s under a 700ms budget, `engine_open_contract_test.go`). `OpenSync` under the same holder carries **both** `ErrWorkspaceBusy` and `nbs.ErrDatabaseLocked` (`engine_open_contract_test.go`).

#### 1.7 Other free helpers defined in store.go

- `dirExists(path string) bool` — `os.Stat` + `IsDir` (`store.go`).
- `scanTime(value string) (time.Time, error)` = `time.Parse(time.RFC3339Nano, value)` (`store.go`). Single parse boundary for every timestamp column.
- `scanNullableTime(sql.NullString) (*time.Time, error)` — invalid → `(nil, nil)` (`store.go`).
- `nullableTime(*time.Time) any` — nil → `nil`, else `RFC3339Nano` string (`store.go`).
- `nullableString(string) any` — `""` → SQL `NULL`, else the string (`store.go`).
- `nullableResolution(*model.Resolution) any` — nil → `NULL` (`store.go`).
- `nullableStringPtr(*string) any` — nil → `NULL` (`store.go`).
- `formatNullableTime(*time.Time) string` — nil → `""` (`store.go`).
- `formatNullableResolution(*model.Resolution) string` — nil → `""` (`store.go`).
- `formatNullableString(*string) string` — nil → `""` (`store.go`).
- `timesEqual(a, b *time.Time) bool` — both nil equal; one nil unequal; else `a.Equal(*b)` (`store.go`).
- `resolutionsEqual(a, b *model.Resolution) bool` — same nil discipline, `*a == *b` (`store.go`).
- `stringPointersEqual(a, b *string) bool` — same (`store.go`).
- `retentionColumns(issue model.Issue) (archivedAt, deletedAt any)` — projects `model.RetentionTimestamps(issue.Retention())` through `nullableTime` (`store.go`). Sole feeder of the `archived_at`/`deleted_at` column pair.
- `statusForStorage(issue model.Issue) sql.NullString` — if `issue.Capabilities().Status != nil` returns `{String: string(status.Value), Valid: true}`, else the zero `NullString` (SQL NULL) (`store.go`). Containers therefore store NULL status.
- `retentionWord(model.Retention) string` — `"live"` / `"archived"` / `"deleted"`; **panics** `fmt.Sprintf("illegal Retention value %T", r)` on anything else (`store.go`).
- `sortIssuesByRank([]model.Issue)` — stable sort on `Rank`, tie-break `ID` ascending (`store.go`).

---

### 2. Open / OpenForRead / EnsureDatabase / Close lifecycle

#### 2.1 `validateOpenArgs` and `validateDoltRootDir`

`validateDoltRootDir(doltRootDir string) (string, error)` (`store.go`):
- `strings.TrimSpace(doltRootDir) == ""` → `errors.New("dolt root dir is required")`
- otherwise returns `filepath.Clean(doltRootDir)`.

`validateOpenArgs(doltRootDir, workspaceID string) error` (`store.go`):
- calls `validateDoltRootDir`, propagating its error;
- `strings.TrimSpace(workspaceID) == ""` → `errors.New("workspace id is required")`;
- performs **no** filesystem I/O (doc `store.go`).

#### 2.2 `Open(ctx, doltRootDir, workspaceID) (*Store, error)`

`store.go`. Ordered steps:

1. `validateOpenArgs` (`store.go`).
2. `acquireWorkspaceShared(ctx, doltRootDir)` (`store.go`; defined `internal/store/workspace_lock.go`). Acquired **before** database bootstrap.
3. `success := false` plus a deferred release-on-failure that `errors.Join`s the release error into the named return (`store.go`).
4. `requireNoPendingAdopt(doltRootDir)` (`store.go`; defined `internal/store/adopt.go`) — runs **after** the lock.
5. `ensureDoltDatabase(ctx, doltRootDir, workspaceID)` (`store.go`).
6. `openStoreConnection(ctx, doltRootDir, workspaceID, engineWrite)` (`store.go`).
7. `s.releaseWorkspaceLock = release` (`store.go`).
8. Under `s.withCommitLock` (`store.go`): `ensureMasterDefaultBranch(ctx, s.db)` wrapped by `wrapEngineOpenContention`, then `s.migrate(ctx)` (`internal/store/migration_runner.go`).
9. On failure of step 8: `s.db.Close()` — a `context.Canceled` close error is dropped, any other is `errors.Join`ed (`store.go`); `s.releaseWorkspaceLock = nil` (`store.go`) so the deferred release still fires; returns `(nil, err)`.
10. `success = true`; return the store.

Behavioral evidence:
- A second concurrent `Open` on the same root blocks while the first store is live and then succeeds after the first `Close` (`engine_serialization_test.go`); it must still be blocked after a 300 ms window (`engine_serialization_test.go`) and must complete within 5 s of the release (`engine_serialization_test.go`).
- `OpenSync` waits on a live foreground `Open` the same way (`engine_serialization_test.go`).
- Re-`Open` on a current schema adds **no** Dolt commit (`store_test.go`, compared via `dolt log --oneline` line counts).
- Migration is idempotent across opens, measured on the commit log (`store_test.go`).
- `Open` preserves an existing `meta.schema_version` value (`store_test.go`).
- After a SIGKILL delivered inside `commitWorkingSetOnce`, a fresh-process `Open` on the same path succeeds, the commit lock is free, and the killed mutation's staged write is visible (`process_kill_test.go`).
- After a SIGKILL inside a goose migration step, `Open` still completes and the schema is usable (`process_kill_test.go`).
- Opening a read-only (frozen) directory fails and mutates nothing (`fixture_residue_test.go`).

#### 2.3 `OpenForRead(ctx, doltRootDir, workspaceID) (*Store, error)`

`store.go`. Steps:

1. `validateOpenArgs` (`store.go`).
2. `acquireWorkspaceShared` **before** the existence stat (`store.go`), with the same `success`-guarded deferred release (`store.go`).
3. `requireInitializedWorkspace(doltRootDir)` (`store.go`):
   - `os.ErrNotExist` → the package sentinel `ErrWorkspaceNotInitialized` (`workspace_initialized.go`), whose text is unchanged: `repository not initialized with lit — run 'lit init' first`;
   - any other stat error → `stat database dir: %w`.
4. `requireNoPendingAdopt` (`store.go`).
5. `openStoreConnection(..., engineRead)` (`store.go`) — pinged eagerly like a write engine.
6. `s.releaseWorkspaceLock = release` (`store.go`).
7. `s.assessMigration(ctx)` (`migration_runner.go`) with **no** commit lock: classify, the schema-ahead baseline check, and applied-version content verification, all reads. On error: `s.Close()` (which releases the workspace hold) joined beside it (`store.go`).
8. `assessment.needsWrite()` false (managed, at registry max, no content drift) → return the read store.
9. Otherwise the read store is closed (`s.Close()`, releasing the workspace hold) and the call returns `Open(ctx, doltRootDir, workspaceID)` — the write open migrates under the commit lock (and normalizes the default branch, as every write open does) and serves the read; its failure is wrapped as `this read open found <assessment> and handed off to the write open to bring it forward: %w` (`store.go`). A read open never applies DDL itself.

Behavioral evidence:
- On a missing directory, `OpenForRead` errors and creates nothing — `<doltRoot>/links` still does not exist (`store_test.go`).
- A read open beside a foreign journal-lock holder succeeds and serves reads (count = 1) via Dolt's read-only fallback (`engine_open_contract_test.go`; `dolt_journal_hold_test.go`).
- A read open does not wait on a live write engine — it completes inside a 1-second context (`engine_serialization_test.go`).
- A read open under a held commit lock serves reads (count = 1) inside a 30-second context (`read_open_lock_free_test.go`).
- A read open on a workspace one migration behind brings it to registry max through `Open` (`read_open_lock_free_test.go`); one whose applied-version content drifted (`lane`/`resolution` dropped) is repaired the same way (`read_open_lock_free_test.go`).
- A read open with a **pending migration** under a held journal lock fails with `ErrWorkspaceBusy` (the write open's contention refusal, budget shrunk to 700 ms in the test), and the same open succeeds — applying the migration — once the holder releases (`dolt_journal_hold_test.go`).
- A read open on a current schema creates no Dolt commit (`store_test.go`).
- Under a held `LockDoltJournalExclusive`, a read open performs **no** journal crash-recovery I/O — the dirtied journal stays byte-identical; with the lock free the same open truncates it (`dolt_journal_hold_test.go`).
- On an unreconcilable schema (`issues` with only an `id` column) `OpenForRead` fails with an error naming the missing `status` column (`store_test.go`).

#### 2.4 `EnsureDatabase(ctx, doltRootDir, workspaceID) (bool, error)`

`store.go`. `validateOpenArgs` → `acquireWorkspaceShared` (deferred unconditional release, `errors.Join`ed into the named return, `store.go`) → `requireNoPendingAdopt` → `ensureDoltDatabase`. Returns `ensureDoltDatabase`'s created-flag. Doc states `Open`/`OpenSync` do **not** call it because they already hold the lock (`store.go`).

#### 2.5 `ensureDoltDatabase(ctx, doltRootDir, workspaceID) (bool, error)`

`store.go`:
1. `root := filepath.Clean(doltRootDir)` (`store.go`).
2. If `dirExists(filepath.Join(root, "links", ".dolt"))` → returns `(false, nil)` immediately, doing nothing (`store.go`).
3. `created := !dirExists(root)` (`store.go`).
4. `os.MkdirAll(root, 0o755)`; on failure `fmt.Errorf("create dolt root dir: %w", err)` (`store.go`).
5. First bootstrap pool: `openDoltPool(root, workspaceID, "", engineWrite)` (empty database name), `defer db.Close()` inside a closure so it closes before the next open (`store.go`); runs `CREATE DATABASE IF NOT EXISTS links` (`store.go`); failure → `fmt.Errorf("create dolt database: %w", err)` then `wrapEngineOpenContention` (`store.go`).
6. Second pool: `openDoltPool(root, workspaceID, "links", engineWrite)`, `defer db.Close()`, then `ensureMasterDefaultBranch` wrapped in `wrapEngineOpenContention` (`store.go`).
7. Returns `(created, nil)`.

The two pools run strictly sequentially — the explicit close of the first is the ordering owner (`store.go`).

#### 2.6 `openStoreConnection(ctx, doltRootDir, workspaceID, access) (*Store, error)`

`store.go`:
- `openDoltPool(doltRootDir, workspaceID, doltDatabaseName, access)` (`store.go`);
- `awaitEngineOpen(ctx, doltRootDir, db.PingContext)` for both access values; on failure returns `errors.Join(err, db.Close())` (`store.go`); a write engine then publishes its LOCK holder record via `recordLockHolder` with a no-op release (`store.go`);
- builds the `Store` with the field assignments listed in §1.3 (`store.go`). `doltRootDir` is stored **unmodified**; only `commitLockPath` and `telemetryDir` clean it.

A read engine's ping falls back to Dolt's read-only mode past a held journal lock rather than waiting; the fallback being permanent costs a reader nothing because a read open never applies DDL (`store.go`).

#### 2.7 `newDoltConnector` / `openDoltPool`

`newDoltConnector(doltRootDir, workspaceID, database string, access engineAccess) (*embedded.Connector, error)` (`store.go`):
- `author := strings.TrimSpace(workspaceID)`; if empty → `"links"` (`store.go`);
- `author = strings.ReplaceAll(author, "@", "_")` (`store.go`);
- `embedded.Config{ Directory: filepath.Clean(doltRootDir), CommitName: author, CommitEmail: fmt.Sprintf("%s@links.local", author), Database: database, DisableSingletonCache: true }` (`store.go`);
- `if access == engineWrite { cfg.BackOff = newEngineOpenBackOff() }` (`store.go`);
- connector construction failure → `fmt.Errorf("open dolt: %w", err)` (`store.go`).

**Dolt commit identity** therefore comes entirely from `workspaceID`: name = workspace id with `@`→`_`, email = `<name>@links.local`. `DisableSingletonCache: true` ties engine (and journal-lock) lifetime to the pool's lifetime.

`openDoltPool` (`store.go`): `sql.OpenDB(connector)`, then `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)` — exactly one connection per Store.

#### 2.8 `reconnect(ctx) error`

`store.go`. Unconditional rotation:
1. `openDoltPool(s.doltRootDir, s.workspaceID, doltDatabaseName, s.access)`; failure → `fmt.Errorf("reopen dolt: %w", err)` (`store.go`).
2. `prev := s.db; s.db = next` (`store.go`) — swap **before** closing.
3. `prev.Close()`; a `context.Canceled` is tolerated, anything else → `fmt.Errorf("close prior dolt connection after reconnect: %w", err)` (`store.go`).
4. `awaitEngineOpen(ctx, s.doltRootDir, next.PingContext)`; failure → `fmt.Errorf("reopen dolt: %w", err)` (`store.go`).

Doc: must be called under the commit lock; it is the one site where the journal lock is taken while the commit lock is held, bounded by `coResidentHolderWait` (2.3s) against the commit-lock waiter budget of `coResidentHolderWait` + `rotationReserve()` (`store.go`). `reconnect` is the `connectionRotator` passed into every retry loop (`commit_lock.go`).

#### 2.9 `Close() error`

`store.go`:
1. `err := s.db.Close()`; if `errors.Is(err, context.Canceled)` → `err = nil` (`store.go`).
2. If `s.releaseWorkspaceLock != nil`: capture, set field to `nil`, call it, `errors.Join` any release error onto `err` (`store.go`).
3. Return `err`.

Ordering: `db.Close()` (which releases Dolt's journal lock) runs before the workspace release (`store.go`).

#### 2.10 `AttributeTo(streamToken string)`

`store.go`: `s.attribution = model.NewAttribution(streamToken, s.workspaceID)`. No return value, no validation here — a blank token yields an absent attribution by `NewAttribution`'s contract (`store.go`).

Evidence:
- An unattributed store writes SQL `NULL` in both `stream_id` and `workspace_id` for every event kind (`event_attribution_test.go`).
- After `AttributeTo(token)`, **every** event kind (created, field update, start) carries `model.NewAttribution(token, workspaceID)` (`event_attribution_test.go`).
- `AttributeTo("")` writes no half pair — `workspace_id` stays NULL (`event_attribution_test.go`).
- Replaying an export preserves the producer's attribution rather than re-stamping the restorer's (`event_attribution_test.go`).

#### 2.11 `ExecRawForTest(ctx, query string, args ...any) error`

`store.go`: `s.db.ExecContext(ctx, query, args...)`, returning only the error. No commit lock, no `commitWorkingSet` (doc `store.go`). Used by tests to probe schema CHECK constraints (`store_test.go`) and to hard-delete a row (`store_test.go`).

---

### 3. Commit semantics (owned by `commit_lock.go`, reached from every store.go mutation)

#### 3.1 `withMutation` / `withStampedMutation`

`func (s *Store) withMutation(ctx context.Context, message string, fn func(ctx context.Context, tx *sql.Tx) error) error` (`commit_lock.go`) delegates to `withStampedMutation(ctx, commitStamp{Message: message}, fn)`.

`withStampedMutation` (`commit_lock.go`) runs, under `withCommitLock`, inside `retryTransientGCContention`:
1. If `!staged`: `s.db.BeginTx(ctx, nil)` — failure → `fmt.Errorf("begin %s tx: %w", stamp.Message, err)` (`commit_lock.go`);
2. `defer tx.Rollback()` (`commit_lock.go`);
3. `fn(ctx, tx)` — its error is returned verbatim (`commit_lock.go`);
4. `tx.Commit()` — failure → `fmt.Errorf("commit %s tx: %w", stamp.Message, err)` (`commit_lock.go`);
5. `staged = true` (`commit_lock.go`);
6. `s.commitWorkingSetOnce(ctx, stamp)` (`commit_lock.go`).

The `staged` flag is the phase marker: a retry after a successful `tx.Commit` resumes at the DOLT_COMMIT step and does **not** re-run `fn` (`commit_lock.go`).

`commitStamp` (`commit_lock.go`) fields: `Message string`, `Date time.Time` (rendered as `--date` RFC3339 UTC; second granularity), `Author string` (rendered `--author`), `AllowEmpty bool`. `store.go` mutations always use the zero-beyond-Message form.

#### 3.2 `commitWorkingSetOnce` — the exact Dolt commit

`commit_lock.go`:
- fires `s.commitWorkingSetHookForTest` first if non-nil, returning its error (`commit_lock.go`);
- `trimmed := strings.TrimSpace(stamp.Message)`; if empty → `"links mutation"` (`commit_lock.go`);
- `args := []any{"-Am", trimmed}` (`commit_lock.go`) — i.e. **`DOLT_COMMIT('-Am', <message>)`**, the `-A` doing the staging so there is no separate `DOLT_ADD` call anywhere on this path;
- appends `"--allow-empty"` when `stamp.AllowEmpty` (`commit_lock.go`);
- appends `"--date", stamp.Date.UTC().Format(time.RFC3339)` when `Date` is non-zero (`commit_lock.go`);
- appends `"--author", stamp.Author` when non-empty (`commit_lock.go`);
- executes `buildProcedureCall("DOLT_COMMIT", len(args))` via `QueryRowContext(...).Scan(&commitHash)` (`commit_lock.go`);
- `err == nil` → success (`commit_lock.go`);
- if `strings.Contains(strings.ToLower(err.Error()), "nothing to commit")` → returns `nil` (success-with-no-commit) (`commit_lock.go`);
- otherwise `wrapCommitWorkingSetError(err)` (`commit_lock.go`).

There is **no `--skip-empty`** flag anywhere; "nothing to commit" is absorbed by the string check above.

**Commit message format strings actually used by store.go mutations** (the literal passed to `withMutation`):
- `"record sync state"` (`store.go`)
- `"create issue"` (`store.go`)
- `"apply update"` (`store.go`)
- `"add comment"` (`store.go`)
- `"delete comment"` (`store.go`)

Each is used verbatim as the Dolt commit message (`commit_lock.go`) and inside tx error text (`commit_lock.go`).

`commitWorkingSet(ctx, message)` (`commit_lock.go`) is the standalone version: `withCommitLock` → `retryTransientGCContention` → `commitWorkingSetOnce(commitStamp{Message: message})`. Exercised at `store_test.go`.

Test evidence for one-commit-per-mutation: a combined transition+field `Apply` adds exactly **1** row to `dolt_log()` (`update_atomicity_test.go`, count query at `update_atomicity_test.go`).

#### 3.3 `withCommitLock`, re-entrancy, release settlement

`withCommitLock(ctx, operation retryOperation) (err error)` (`commit_lock.go`): acquire → `defer func(){ err = SettleCommitLockRelease(err, release()) }()` (fires on panic too) → `operation(lockedCtx)`.

`acquireCommitLock` (`commit_lock.go`): if `ctx.Value(commitLockContextKey{})` is `true`, returns the same ctx and a no-op release (re-entrant short-circuit); otherwise `acquireCommitLockAtPath(ctx, s.commitLockPath)` and returns `context.WithValue(ctx, commitLockContextKey{}, true)`.

`acquireCommitLockAtPath` (`commit_lock.go`): `acquireStoreLock(ctx, storageDir, lockPath, true /*exclusive*/, commitLockWaiterBudget())`, errors passed through `wrapCommitLockContention`.

Budget (`commit_lock.go`): `commitLockWaiterBudget()` = `coResidentHolderWait` + `rotationReserve()`, where `rotationReserve()` = `coResidentHolderWait` + `rotationCloseReserve` (1s) → 5.6s of unchanged holders.

`wrapCommitLockContention` (`commit_lock.go`): when `errors.Is(err, ErrWorkspaceBusy)` returns
```
fmt.Errorf("another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes: %w", err)
```
Every other error, cancellation included, passes through.

`SettleCommitLockRelease(opErr, releaseErr error) error` (`commit_lock.go`): `releaseErr == nil` → `opErr`; both non-nil → `errors.Join(opErr, releaseErr)`; op succeeded but release failed → prints to stderr
```
lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): %v
```
and returns `nil`.

Test evidence:
- Two `withCommitLock` calls serialize; the second cannot enter within a 25 ms window while the first holds (`retry_test.go`).
- A panic inside a `withMutation` fn releases the lock, and a subsequent `CreateIssue` succeeds (`crash_safety_test.go`).
- A panic inside `withCommitLock`'s operation releases the lock (`crash_safety_test.go`).
- Nested `withCommitLock` short-circuits and the inner ctx still carries `commitLockContextKey{} == true` (`crash_safety_test.go`).
- A cancelled ctx against a live holder returns `context.Canceled` rather than burning the budget (`crash_safety_test.go`).
- Ten concurrent `CreateIssue` goroutines all succeed with unique ids, all readable, and the lock is free afterwards (`concurrent_test.go`).
- Mixed concurrent creates/comments/priority-updates/transitions all persist and the lock is free (`concurrent_test.go`).

#### 3.4 Retry classification and budgets

- `ErrTransientGCContention = errors.New("transient online-gc contention")` (`commit_lock.go`).
- `transientRetryMaxAttempts = 30` — a **variable** so tests can shrink it (`commit_lock.go`; shrunk to 2 at `dolt_journal_hold_test.go`).
- `transientRetryBaseDelay = 50 * time.Millisecond`, `transientRetryMaxDelay = 1 * time.Second` (`commit_lock.go`).
- `transientRetryDelay(attempt)` = `base << (attempt-1)`, clamped to `maxDelay`; attempts < 1 treated as 1 (`commit_lock.go`). Bounded between base and max for attempts 1..10 (`retry_test.go`).

`retryTransientGCContention(ctx, operation, rotate, delayForAttempt, sleep)` (`commit_lock.go`): loop `attempt := 1; attempt <= transientRetryMaxAttempts`:
- `classifyTransientGCError(operation(ctx))`; nil → return nil;
- if not `ErrTransientGCContention` **or** last attempt → break;
- `sleep(ctx, delayForAttempt(attempt))` — its error is returned immediately;
- `rotate(ctx)` — its error is returned immediately;
- final: `exhaustedContentionError(lastErr)`.

Retryable/not:
- `isManifestReadOnlyError`: lowercased message contains both `"cannot update manifest"` and `"read only"` (`commit_lock.go`).
- `isOnlineGCResetError`: lowercased message contains both `"online garbage collection"` and `"reconnect"` (`commit_lock.go`).
- `isTransientGCContentionError` = either of those (`commit_lock.go`).
- `exhaustedContentionError` (`commit_lock.go`): if the exhausted error is manifest-read-only → `WorkspaceWriteBlockedError{Cause: err}`; otherwise unchanged.
- `WorkspaceWriteBlockedError.Error()` (`commit_lock.go`):
```
another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)
```
with `Unwrap() → Cause` (`commit_lock.go`).
- `wrapCommitWorkingSetError` (`commit_lock.go`): always `fmt.Errorf("dolt commit working set: %w", err)`; marks it transient only when `isTransientGCContentionError`.

Test evidence:
- One transient then success = 2 calls (`retry_test.go`).
- Full exhaustion returns the last error, still `ErrTransientGCContention`, with exactly `transientRetryMaxAttempts` calls (`retry_test.go`).
- Exhausted manifest-read-only promotes to `WorkspaceWriteBlockedError` naming "another lit process" while keeping the transient cause chain (`retry_test.go`).
- Exhausted GC-reset does **not** promote (`retry_test.go`).
- A non-transient error is not retried — exactly 1 call (`retry_test.go`).
- Context deadline during backoff surfaces `context.DeadlineExceeded` after 1 call (`retry_test.go`).
- Rotation happens once per backoff, never after the succeeding call (2 rotations for 3 calls) (`retry_test.go`).
- A failing rotator aborts the loop with its error and no re-attempt (`retry_test.go`).
- A rotator blocked on ctx returns `context.Canceled` on cancellation (`retry_test.go`).
- Cluster-role "please reconnect" is **not** misclassified as GC contention (`retry_test.go`).
- `wrapCommitWorkingSetError(errors.New("permission denied")).Error() == "dolt commit working set: permission denied"` (`retry_test.go`).

---

### 4. Issue creation

#### 4.1 `CreateIssue(ctx, in storage.CreateIssueInput) (model.Issue, error)`

`store.go`. Pre-transaction (pure/validation) phase:
1. `strings.TrimSpace(in.Title) == ""` → `errors.New("title is required")` (`store.go`).
2. `issueType := in.IssueType`; if `""` → `model.TypeTask` (`store.go`).
3. `now := time.Now().UTC()` (`store.go`).
4. `canonicalizeLabels(in.Labels)` (`store.go`; `internal/store/labels.go`) — error propagated.
5. `issueid.NormalizeTopicForCreate(in.Topic)` (`store.go`) — error propagated. Missing topic yields an error containing `"topic is required"` (`store_test.go`).
6. `createdBy := "links"` — a hardcoded literal used as the event actor, the label creator, and the parent-edge creator (`store.go`).
7. Builds `model.Issue` with **`strings.TrimSpace` applied to** Title, Description, Prompt, Lane, Assignee; `Priority`, `IssueType`, `Topic`, `Labels` copied; `CreatedAt = UpdatedAt = now` (`store.go`).
8. `model.HydrateRow(issue, model.StatusView{Value: model.StateOpen}, nil)` — new issues start `open` (`store.go`). An invalid issue type is rejected here or downstream (`store_test.go`).
9. `parentID := strings.TrimSpace(in.ParentID)` (`store.go`).

Inside `withMutation(ctx, "create issue"...)` (`store.go`):
1. `frame, err := filingFrameTx(ctx, tx, parentID)` (`store.go`; defined `internal/store/ranking.go`): an empty `parentID` → `storage.TopLevel`; otherwise `SELECT deleted_at FROM issues WHERE id = ?`, with `sql.ErrNoRows` → `storage.NotFoundError{Entity: "issue", ID: parentID}`, other error → `fmt.Errorf("lookup parent issue %q: %w", parentID, err)`, a non-NULL `deleted_at` → `storage.TopLevel`, and a live parent → `storage.Frame(parentID)`.
2. `issueid.NormalizeConfiguredPrefix(in.Prefix)`; failure → `fmt.Errorf("normalize issue prefix: %w", err)` (`store.go`).
3. `issue.ID, err = newIssueID(ctx, tx, prefix, issue.Topic, issue.Title, issue.Description, createdBy, issue.CreatedAt, parentID)` (`store.go`; defined `internal/store/issue_ids.go`).
4. `issue.Rank, err = nextRankForPlacement(ctx, tx, in.Placement, frame)` (`store.go`).
5. `archivedCol, deletedCol := retentionColumns(issue)` (`store.go`).
6. The INSERT (`store.go`), verbatim:
```sql
INSERT INTO issues(
    id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, archived_at, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
```
   Bound values in order: `issue.ID`, `issue.Title`, `issue.Description`, `nullableString(issue.Prompt)`, `statusForStorage(issue)`, `issue.Priority`, `issue.IssueType`, `issue.Topic`, `issue.AssigneeValue()`, `issue.Rank`, `issue.Lane`, `issue.CreatedAt.Format(time.RFC3339Nano)`, `issue.UpdatedAt.Format(time.RFC3339Nano)`, `archivedCol`, `deletedCol`. `closed_at` is a literal `NULL`. Columns `resolution` and `redirect_target` are **not** in the insert list (left to their defaults). Failure → `fmt.Errorf("insert issue: %w", err)` (`store.go`).
7. If `parentID != ""`: builds `model.Relation{SrcID: issue.ID, DstID: parentID, Type: model.RelParentChild, CreatedAt: issue.CreatedAt, CreatedBy: "links"}` and routes it through `insertRelationTx` (`store.go`; `internal/store/relations.go`).
8. `s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, createdBy)` (`store.go`; `internal/store/labels.go`).
9. Event: `createChanges := []model.FieldChange{}`; for **non-container** types appends `{Field:"status", From:"", To:"open"}`; containers get none (`store.go`). Then `s.recordEvent(ctx, tx, issue.ID, "created", "issue created", "links", createChanges)` (`store.go`) — action `"created"`, reason `"issue created"`, actor `"links"`.
10. `smoothRanksIfNeededTx(ctx, tx, issue.Rank)` (`store.go`; `internal/store/ranking.go`).

Returns the **in-memory** `issue` value (not a re-read) (`store.go`).

Defaults, restated as a table for every column the create path touches:

| Column | Value on create |
|---|---|
| `id` | `newIssueID(...)` — `<prefix>-<topic>-<3..8 base36>` for roots (`store_test.go`), `<parentID>.N` for children (`store_test.go`) |
| `title` | trimmed input, required non-empty |
| `description` | trimmed input |
| `agent_prompt` | trimmed prompt, `""` stored as SQL NULL |
| `status` | `"open"` for leaves; SQL NULL for containers (`store_test.go`) |
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

Evidence: id shape `^test-renderer-[0-9a-z]{3,8}$` (`store_test.go`); prefix `"Renderer Platform Team"` normalizes/clamps to `renderer-pla-` (`store_test.go`); child ids `parent.ID+".1"`, `".2"` (`store_test.go`); id collisions advance a nonce so a second identical input yields a different id (`store_test.go`); labels come back canonicalized and sorted (`{"Renderer","gpu"}` → `["gpu","renderer"]`, `store_test.go`); prompt round-trips and is searchable (`store_test.go`); the schema CHECK rejects a non-NULL status on an epic and a NULL status on a leaf (`store_test.go`).

#### 4.2 Rank placement

`nextRankForPlacement(ctx, tx, p storage.RankPlacement, f storage.Frame) (string, error)` (`store.go`): `edgeFor(p)` resolves the end — `storage.RankTop` → `topEdge`; `storage.RankBottom` → `bottomEdge`; anything else → `fmt.Errorf("unknown rank placement: %d", p)` (`internal/store/ranking.go`) — then `rankBetweenTx` returns a key between the bounds `edge.filingBoundsTx(ctx, tx, f)` reads (`internal/store/ranking.go`). `filingBoundsTx` reads that end's filing rank — frame `f`'s leading rank for the top (`frameEdgeRankTx`), the whole workspace's last rank for the bottom (`workspaceEdgeRankTx`) — and hands it to `e.roomBesideTx(ctx, tx, anchorRank)`, which pairs it with the nearest rank the **whole workspace** holds on its far side; a create names no moving ids, so the variadic `moving` is empty; the statement carries no membership clause either way, because the exclusion is applied in Go by `nearestRankOutside` and an empty exclusion makes its read `LIMIT 1` over the ordered rows. A create at the top therefore takes the midpoint between the frame's leading rank and the nearest rank below it anywhere in the workspace; with no rank below it that read comes back `""` and the lower bound is open. An **empty** filing rank — a frame with nothing ranked in it — never reaches that read: `roomBesideTx` refuses an empty anchor outright with `fmt.Errorf("no room beside the %s of this frame: the key it was read from is empty", e.name)`, and `filingBoundsTx` routes the case to `firstInFrameBoundsTx(ctx, tx, f)` instead. For a frame other than `storage.TopLevel` that reads the rank of the issue `f` names — `SELECT item_rank FROM issues WHERE id = ? AND deleted_at IS NULL AND item_rank != ''`, error → `fmt.Errorf("query the rank of frame %q: %w", f, err)` — and a non-empty result returns `bottomEdge.roomBesideTx(ctx, tx, containerRank, moving...)`, so the frame's first issue lands just past its container's own key; a create reaches that call through the `filingBoundsTx` arm above, which names no moving ids, so `moving` is empty on this path. Everything else — the top level, which names no containing issue, and a container carrying no rank of its own — falls through to one shared arm at the end: `workspaceEdgeRankTx(ctx, tx, storage.TopLevel, bottomEdge)`, then `("", "")` when that read is empty, otherwise `bottomEdge.roomBesideTx(ctx, tx, lastRank)`. That workspace read does not exclude `moving`, so its emptiness reports the table rather than this write: it comes back empty only when nothing at all is ranked, the one case where open bounds hold and `rank.Initial()` ("V") is a key no issue holds.

`nextRankAtBottom` (`store.go`):
```sql
SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' ORDER BY item_rank DESC LIMIT 1
```
Non-`ErrNoRows` error → `fmt.Errorf("query last rank: %w", err)`. Invalid/empty result → `rank.Initial()`; else `rank.After(lastRank)`.

`nextRankAtTop` (`store.go`): same query with `ORDER BY item_rank ASC`; error text `"query first rank: %w"`; empty → `rank.Initial()`; else `rank.Before(firstRank)`.

The **zero value** of `storage.RankPlacement` behaves as bottom/append: consecutive default creates keep authoring order, and an explicit `RankTop` create sorts ahead of them (`store_test.go`).

---

### 5. Reads

#### 5.1 The issue projection

`issueColumns` (`store.go`), the single ordered projection, 18 columns:
```
id, title, description, agent_prompt, status, priority,
issue_type, topic, assignee, item_rank, lane, created_at,
updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at
```
`issueProjection(alias)` (`store.go`) joins them with `", "`, prefixing `alias+"."` when alias is non-empty. Derived once: `issueColumnsBare = issueProjection("")` and `issueColumnsQualified = issueProjection("i")` (`store.go`).

#### 5.2 Row scanners

`issueScanner interface{ Scan(dest ...any) error }` (`store.go`).

`issueRow struct { Issue partialIssue; Status model.StatusView }` (`store.go`).

`partialIssue` (`store.go`): `ID, Title, Description, Prompt string; Priority model.Priority; IssueType model.IssueType; Topic, Assignee, Rank, Lane string; Labels []string; CreatedAt, UpdatedAt time.Time; Retention model.Retention`.

`scanIssue(row)` (`store.go`) scans the 18 columns positionally in `issueColumns` order, with `prompt`, `status`, `closedAt`, `resolution`, `redirectTarget`, `archivedAt`, `deletedAt` as `sql.NullString`; sets `issue.Prompt = prompt.String` (NULL → `""`); delegates to `parsedIssueRow`.

`scanIssueWithParent(row)` (`store.go`) — identical but with a leading `parentID string` column.

`parsedIssueRow(...)` (`store.go`):
- parses `created_at`/`updated_at` via `scanTime` (errors propagate);
- `statusView := model.StatusView{Value: model.State(status.String)}` — NULL status becomes `model.State("")`;
- valid `closed_at` → parsed into `statusView.ClosedAt`;
- valid `resolution` → `model.Resolution(resolution.String)` raw-converted (no re-parse) into `statusView.Resolution` (`store.go`);
- valid `redirect_target` → `statusView.RedirectTarget` (`store.go`);
- `archived_at`/`deleted_at` parsed into `*time.Time` and folded through `model.RetentionFromTimestamps` (`store.go`);
- `issue.Labels = []string{}` (`store.go`).

#### 5.3 `hydrateIssues(ctx, rows []issueRow) ([]model.Issue, error)`

`store.go`. Query count is **fixed per recursion level**, not per epic:
1. Empty input → `([]model.Issue{}, nil)` with no query (`store.go`).
2. One `loadLabelsByIssueIDs` query for all ids (`store.go`).
3. Collects container ids; one `lifecycleChildrenByEpicIDs` query for all of them (`store.go`).
4. Per row, builds a `model.Issue` copying every `partialIssue` field, `SetRetention(row.Issue.Retention)`, `Labels` defaulted to `[]string{}` when the map has no entry, and calls `model.HydrateRow(base, row.Status, childrenByEpicID[id])` (`store.go`).
5. Post-condition: `!issue.IsHydrated()` → `fmt.Errorf("hydrateIssues: produced unhydrated issue %s", issue.ID)` (`store.go`).

`loadLabelsByIssueIDs` (`store.go`):
```sql
SELECT issue_id, label FROM labels WHERE issue_id IN (?, ?, ...) ORDER BY label ASC
```
failure → `fmt.Errorf("load labels by issue ids: %w", err)`.

`lifecycleChildrenByEpicIDs(ctx, epicIDs)` (`store.go`) — empty input returns an empty map without querying; otherwise one query:
```sql
SELECT r.dst_id, <issueColumnsQualified>
FROM relations r
JOIN issues i ON i.id = r.src_id
JOIN issues p ON p.id = r.dst_id
WHERE r.dst_id IN (?, ...) AND r.type = 'parent-child'
    AND (p.archived_at IS NOT NULL OR p.deleted_at IS NOT NULL OR (i.archived_at IS NULL AND i.deleted_at IS NULL))
ORDER BY r.dst_id ASC, i.item_rank ASC
```
failure → `fmt.Errorf("load lifecycle children: %w", err)`. Visibility truth table (`store.go`): parent live + child live → include; parent live + child dead → exclude; parent dead (archived or deleted) + child either → include. Rows are scanned with `scanIssueWithParent`, hydrated in **one** recursive `hydrateIssues` call, and re-bucketed by the parallel `parentIDs` slice (`store.go`).

Evidence: listing query count for 1 epic equals that for 5 epics, measured by a counting `driver.Conn` that forces every query through `Prepare` (`lifecycle_hydration_query_count_test.go`, wrapper). An active epic's `Progress()` excludes archived children (`Total == 0`); the same epic once archived includes them (`Total == 1, Open == 1`) (`store_test.go`).

#### 5.4 `GetIssue(ctx, id) (model.Issue, error)`

`store.go`:
```sql
SELECT <issueColumnsBare> FROM issues WHERE id = ?
```
`sql.ErrNoRows` → `storage.NotFoundError{Entity: "issue", ID: id}`; any other scan error is returned raw; then `hydrateIssues([]issueRow{scanned})` and `hydrated[0]`. Total queries: 1 + 1 (labels) + 0-or-1 (children of a container).

#### 5.5 `getIssuesByIDs(ctx, ids) (map[string]model.Issue, error)`

`store.go`. Empty input → empty map, no query. Otherwise:
```sql
SELECT <issueColumnsBare> FROM issues WHERE id IN (?, ?, ...)
```
Errors: `"batch load issues: %w"`, `"scan batch-loaded issue: %w"`, `"iterate batch-loaded issues: %w"`. Missing ids are simply absent from the map (`store.go`).

#### 5.6 `GetIssueDetail(ctx, id) (model.IssueDetail, error)`

`store.go`, in order:
1. `GetIssue(ctx, id)` (`store.go`).
2. `listRelations(ctx, id)` (`store.go`).
3. `listComments(ctx, id)` (`store.go`).
4. `listEvents(ctx, id)` (`store.go`).
5. `collectRelatedIssueIDs(id, relations)` (`store.go`; defined `store.go`) — distinct counterparties of both `SrcID` and `DstID`, excluding `""` and the focal id, in first-seen order.
6. If `issue.RedirectTargetValue()` is non-nil and not already in the list, it is appended (`store.go`).
7. `getIssuesByIDs(ctx, relatedIDs)` — one batch hydrate (`store.go`).
8. `bucketRelations(id, relations, relatedByID)` → `structural` with `Parent`, `Children`, `DependsOn`, `Blocks` (`store.go`; `internal/store/relations.go`).
9. If `structural.Parent != nil`: `ListIssues(ctx, ListIssuesFilter{ParentIDs: [structural.Parent.ID], IncludeArchived: true, IncludeDeleted: true})` — the parent's children in every retention state, rank order then id — then `siblingsOf(id, parentChildren)`; otherwise `siblings := []model.Issue{}` (`store.go`).
10. `redirectTarget` is set only if the target id is present in `relatedByID`; a vanished target hydrates as absent (`store.go`).
11. `related := relatedFrom(id, relations, relatedByID)` (`store.go`).
12. Assembles `model.IssueDetail{Issue, Relations, Comments, Events, Children, Siblings, DependsOn, Blocks, Parent, Related, RedirectTarget}` (`store.go`).

Evidence: `DependsOn`, `Blocks`, `Children`, `Related` all come back in rank order (`store_test.go`); relation counterparties are fully hydrated including container progress and label slices (`store_test.go`); siblings are the parent's other children in rank order excluding self, and empty for parentless issues and only children (`store_test.go`); the redirect target is exposed via `detail.RedirectTarget` with **no** `related-to` edge written (`store_test.go`).

#### 5.7 `ListIssues(ctx, filter storage.ListIssuesFilter) ([]model.Issue, error)`

`store.go`. Base query: `SELECT <issueColumnsQualified> FROM issues i` (`store.go`).

WHERE clauses, appended in this exact order:

| Condition | Clause | Line |
|---|---|---|
| `!filter.IncludeArchived` | `i.archived_at IS NULL` | `store.go` |
| `!filter.IncludeDeleted` | `i.deleted_at IS NULL` | `store.go` |
| `len(filter.IssueTypes) > 0` | `i.issue_type IN (?...)` | `store.go` |
| `len(filter.ExcludeIssueTypes) > 0` | `i.issue_type NOT IN (?...)` | `store.go` |
| `len(filter.Assignees) > 0` (blank entries skipped; clause omitted if all blank) | `i.assignee IN (?...)` | `store.go` |
| `filter.UpdatedAfter != nil` | `i.updated_at >= ?` bound with `.UTC().Format(time.RFC3339Nano)` | `store.go` |
| `filter.UpdatedBefore != nil` | `i.updated_at <= ?` same formatting | `store.go` |
| `filter.HasComments != nil`, true | `EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)` | `store.go` |
| `filter.HasComments != nil`, false | `NOT EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)` | `store.go` |
| each canonicalized label in `filter.LabelsAll` | `EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)` (one clause per label — AND semantics) | `store.go` |
| `len(filter.ParentIDs) > 0` (after `requireIssues(ctx, filter.ParentIDs)` passes) | no clause of its own: `selectedIssueIDs` resolves the parents to their children's ids and folds them into the id filter below | `store.go` |
| the id set `selectedIssueIDs` returns is non-empty | `i.id IN (?, ?)` joined with `", "`, one query per batch of at most `idBatchSize` ids | `store.go` |
| each non-blank `filter.SearchTerms` term, lowercased & trimmed | `(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ? OR LOWER(COALESCE(i.agent_prompt, '')) LIKE ? OR LOWER(i.topic) LIKE ?)` with `%term%` bound four times | `store.go` |

Clauses are joined with `" AND "` (`store.go`).

**Status and resolution are NOT filtered in SQL.** `parseStatusFilter(filter.Statuses)` (`store.go`, defined `store.go`) only maps each raw value through `model.DefaultOpen(string(raw))` and never errors; the actual filtering happens post-hydration.

Ordering: `buildIssueOrderClause(filter.SortBy)` (`store.go`, defined `store.go`):
- no specs → `"i.item_rank ASC, i.id ASC"`;
- allowed sort fields (case-insensitive, trimmed) and their columns: `id→i.id`, `title→i.title`, `status→i.status`, `priority→i.priority`, `rank→i.item_rank`, `type→i.issue_type`, `topic→i.topic`, `assignee→i.assignee`, `created_at→i.created_at`, `updated_at→i.updated_at`;
- unknown field → `fmt.Errorf("unsupported sort field %q", spec.Field)`;
- direction `DESC` when `spec.Desc`, else `ASC`;
- `"i.id ASC"` is always appended as the final tiebreaker.

Execution and post-processing:
- query failure → `fmt.Errorf("list issues: %w (query=%s)", err, query)` — the full SQL is included (`store.go`);
- rows scanned via `scanIssue`, `rows.Err()` checked (`store.go`);
- `hydrateIssues` (`store.go`);
- return `capLimit(filterByResolution(filterByState(hydrated, allowedStates), filter.Resolutions), filter.Limit)` (`store.go`).

`filterByState` (`store.go`): empty allow-list passes everything; otherwise keeps issues whose **derived** `issue.State()` is in the set.
`filterByResolution` (`store.go`): empty allow-list passes everything; otherwise drops every issue whose `ResolutionValue()` is nil and keeps only matching resolutions.
`capLimit` (`store.go`): `limit <= 0` means uncapped; else truncates to the first `limit`.

Evidence: epic state is filtered by derived lifecycle, not the dead `i.status` column, across open/mixed/closed epic shapes (`store_test.go`); all advanced filters combine correctly to a single result (`store_test.go`); archived issues disappear from the default list and reappear with `IncludeArchived` (`store_test.go`).

#### 5.8 `ListTopics(ctx) ([]string, error)`

`store.go`:
```sql
SELECT DISTINCT topic FROM issues WHERE deleted_at IS NULL AND topic <> '' ORDER BY topic ASC
```
failure → `fmt.Errorf("list topics: %w", err)`. Returns `[]string{}` (never nil) plus `rows.Err()`.

#### 5.9 Relation, comment, and label reads

`listRelations(ctx, issueID)` (`store.go`):
```sql
SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? OR dst_id = ? ORDER BY created_at ASC
```
error → `fmt.Errorf("list relations: %w", err)`; `created_at` parsed via `scanTime`.

`listAllRelations(ctx)` (`store.go`): same projection, no WHERE, `ORDER BY created_at ASC`; error → `"list all relations: %w"`.

`listComments(ctx, issueID)` (`store.go`):
```sql
SELECT id, issue_id, body, created_at, created_by FROM comments WHERE issue_id = ? ORDER BY created_at ASC
```
error → `"list comments: %w"`.

`listAllComments(ctx)` (`store.go`): same without the WHERE; error → `"list all comments: %w"`.

`listAllLabels(ctx)` (`store.go`):
```sql
SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC
```
error → `"list all labels: %w"`.

#### 5.10 Event reads

`listEvents(ctx, issueID)` (`store.go`): `queryEvents(ctx, "e.issue_id = ?", issueID)`; error → `fmt.Errorf("list issue events: %w", err)`.

`ListAllEvents(ctx)` (`store.go`): `queryEvents(ctx, "")`; error → `fmt.Errorf("list all issue events: %w", err)`. Doc explains no recency cutoff is applied because claim derivation needs arbitrarily old establishing events (`store.go`).

`queryEvents(ctx, whereClause string, args ...any)` (`store.go`):
```sql
SELECT e.id, e.issue_id, e.action, e.reason, e.actor, e.created_at, e.stream_id, e.workspace_id, c.field, c.from_value, c.to_value
    FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id
[ WHERE <whereClause> ]
 ORDER BY e.created_at ASC, e.id ASC, c.field ASC
```
(`store.go`). Exactly one query. Nullable columns: `action`, `stream_id`, `workspace_id`, `c.field`, `c.from_value`, `c.to_value`. Collapsing rules:
- an event is materialized on first sight, keyed by id in `idx` (`store.go`);
- `Attribution: model.NewAttribution(evtStream.String, evtWorkspace.String)` — NULL becomes `""` which the constructor collapses to absent (`store.go`);
- `Changes` starts as `[]model.FieldChange{}` (`store.go`);
- `Action` set only when the column is valid (`store.go`);
- a change row is appended only when `c.field` is valid; `From`/`To` only when their columns are valid, otherwise left `""` (`store.go`).
The `c.field ASC` sort is deliberate so two reads of an unchanged event compare identical (`store.go`).

---

### 6. Update path

#### 6.1 `Apply(ctx, id string, c storage.Change) (model.Issue, error)`

`store.go`, the single execution path for issue-record changes:
1. `current, err := s.GetIssue(ctx, id)` — a not-found id fails here (`store.go`).
2. `actor := strings.TrimSpace(c.Actor)`; if empty → `"unknown"` (`store.go`).
3. `baseline := current` (`store.go`).
4. If `c.Action != nil`: `lw, err = s.planLifecycleAction(ctx, current, actor, strings.TrimSpace(c.Reason), c.Action)`; on error, returns immediately with **no** writes; then `baseline = lw.postIssue()` so a following field write diffs against the post-action issue (`store.go`).
5. `hasFields := !c.Fields.IsEmpty()`; if true, `fw, err = planFieldUpdate(baseline, c.Fields, actor)` — a validation error returns before any write (`store.go`).
6. `needsActionWrite := lw != nil && !lw.isNoop()` (`store.go`).
7. `applyPreMutationHookForTest` fires here if set (`store.go`).
8. If `needsActionWrite || hasFields`: one `withMutation(ctx, "apply update"...)` running `lw.applyTx` then `s.applyFieldsTx`, both in the **same** tx and therefore one Dolt commit (`store.go`).
9. Returns `s.GetIssue(ctx, id)` — a fresh re-read, always (`store.go`).

Evidence: transition + field lands as exactly one Dolt commit with both halves visible (`update_atomicity_test.go`); an invalid field (empty title) paired with a valid transition leaves state, title, and event count **wholly** unchanged (`update_atomicity_test.go`); the full IssueType × flag-combination matrix shows container transitions rejected with `model.ContainerActionError` and nothing written, field writes succeeding on every type, and zero transition events for field-only cells (`update_matrix_test.go`).

#### 6.2 `planLifecycleAction`

`store.go`. Type switch on `model.Action`:
- `model.StatusAction` → `s.planStatusTransition(...)`;
- `model.RetentionAction` → `planRetentionTransition(...)` (a free function, no store);
- anything else → **panic** `fmt.Sprintf("illegal Action value %T", action)` (`store.go`).

`lifecycleWrite` interface (`store.go`): `applyTx(ctx, s *Store, tx *sql.Tx) error`, `postIssue() model.Issue`, `isNoop() bool`.

#### 6.3 `applyTransition` (the guard)

`store.go`: if `model.Frozen(issue.Retention())` → `fmt.Errorf("cannot %s archived or deleted issue", action.Name().Verb())`; otherwise `issue.Apply(action)`. Never mutates the store.

Evidence: a container refuses `Reopen`; a live leaf accepts `Start`; an archived leaf refuses `Close` (`store_test.go`).

#### 6.4 `transitionWrite` and `planStatusTransition`

`transitionWrite` fields (`store.go`): `issueID, fromStatus, toStatus, postAssignee string; now time.Time; closedAtArg, resolutionArg, redirectTargetArg any; action model.ActionName; reason, actor string; changes []model.FieldChange; post model.Issue; noop bool`. Methods at `store.go`.

`planStatusTransition(ctx, issue, actor, reason, action) (transitionWrite, error)` (`store.go`):
1. `applyTransition(issue, action)` → `updated` or the rejection.
2. `priorAssignee := issue.AssigneeValue()`; `postAssignee := priorAssignee` unless the action is `model.Start`, in which case `postAssignee = strings.TrimSpace(start.Assignee)` (`store.go`). Only `Start` rewrites the assignee.
3. `fromStatus := issue.StatusValue()`, `toStatus := updated.StatusValue()` (`store.go`).
4. **No-op rule**: `toStatus == fromStatus && postAssignee == priorAssignee` → `transitionWrite{noop: true, post: issue}` — no write, no event (`store.go`).
5. `now := time.Now().UTC()` (`store.go`).
6. `closedAtArg` = `updated.ClosedAtValue().Format(time.RFC3339Nano)` when non-nil, else nil (`store.go`).
7. `resolutionArg` = `string(*updated.ResolutionValue())` when non-nil, else nil (`store.go`).
8. `redirectTargetArg` = `*updated.RedirectTargetValue()` when non-nil, else nil (`store.go`). Its integrity is deliberately **not** validated here (`store.go`).
9. Change rows, in this order (`store.go`):
   - `status` when `fromStatus != toStatus`;
   - `closed_at` when `!timesEqual(prior, new)`, values via `formatNullableTime`;
   - `resolution` when `!resolutionsEqual(...)`, via `formatNullableResolution`;
   - `redirect_target` when `!stringPointersEqual(...)`, via `formatNullableString`;
   - `assignee` when `priorAssignee != postAssignee`.
10. `updated.UpdatedAt = now` (`store.go`) and the struct is returned with `post: updated`.

Evidence: each of the six non-identity (from→to) pairs records exactly one event carrying the action's own name (`store_test.go`); a same-state `Start` with a new assignee records one `start` event with the calling actor and **no** status change row, and persists the new assignee (`store_test.go`); a same-state, same-assignee `Start` records zero events and does not bump `UpdatedAt` (`store_test.go`).

#### 6.5 `applyTransitionTx`

`store.go`:
1. `validateRedirectTarget(ctx, tx, w.post.ID, w.post.ResolutionValue(), w.post.RedirectTargetValue())` — on the **same tx** as the write (`store.go`).
2. The guarded UPDATE (`store.go`):
```sql
UPDATE issues SET status = ?, assignee = ?, updated_at = ?, closed_at = ?, resolution = ?, redirect_target = ? WHERE id = ? AND status = ?
```
bound `w.toStatus, w.postAssignee, w.now.Format(time.RFC3339Nano), w.closedAtArg, w.resolutionArg, w.redirectTargetArg, w.issueID, w.fromStatus`. Failure → `fmt.Errorf("update issue status: %w", err)`.
3. `result.RowsAffected()` failure → `fmt.Errorf("read status transition result: %w", err)` (`store.go`).
4. `affected == 0` → look up the live status via `currentStatusTx` and return `fmt.Errorf("%s conflict: issue status is %q", w.action.Verb(), currentStatus)` (`store.go`). Exact observed text: `close conflict: issue status is "closed"` (`store_test.go`).
5. `s.recordEvent(ctx, tx, w.issueID, string(w.action), w.reason, w.actor, w.changes)` (`store.go`).

The UPDATE touches only the status-axis columns — a stale transition cannot clobber the retention pair (`store.go`).

#### 6.6 `retentionWrite` and `planRetentionTransition`

`retentionWrite` fields (`store.go`): `issueID string; now time.Time; priorArchived, priorDeleted, nextArchived, nextDeleted any; action model.ActionName; reason, actor string; changes []model.FieldChange; post model.Issue`. `isNoop()` is hardcoded `false` — the Retain table has no same-state success cell (`store.go`).

`planRetentionTransition(issue, actor, reason, action)` (`store.go`):
- `now := time.Now().UTC()`;
- reads `model.RetentionTimestamps(issue.Retention())` and `retentionColumns(issue)` as the CAS guard;
- `model.Retain(issue.Retention(), action, now)` — its error is the rejection (e.g. `"issue is already archived"`, observed at `store_test.go`);
- `post := issue; post.SetRetention(next); post.UpdatedAt = now`;
- change rows: `archived_at` when the timestamps differ, `deleted_at` when they differ, both via `formatNullableTime` (`store.go`).

`retentionWrite.applyTx` (`store.go`):
```sql
UPDATE issues SET updated_at = ?, archived_at = ?, deleted_at = ? WHERE id = ? AND archived_at <=> ? AND deleted_at <=> ?
```
bound `w.now.Format(time.RFC3339Nano), w.nextArchived, w.nextDeleted, w.issueID, w.priorArchived, w.priorDeleted`. Uses MySQL null-safe equality `<=>`. Errors:
- exec failure → `fmt.Errorf("update issue retention: %w", err)`;
- `RowsAffected` failure → `fmt.Errorf("read retention transition result: %w", err)`;
- `affected == 0` → `currentRetentionTx` then `fmt.Errorf("%s conflict: issue retention is %q", w.action.Verb(), model.RetentionName(current))`. Observed text: `archive conflict: issue retention is "archived"` (`store_test.go`).
Then `recordEvent` with the action name, reason, actor, and change rows (`store.go`).

Evidence: a stale archive plan loses to a competing archive with the conflict error (`store_test.go`); delete-on-archived drops the archive stamp and a later restore lands on `Live`, not `Archived` (`store_test.go`); an archive event records exactly one `archived_at` change row and no fake status row (`store_test.go`).

#### 6.7 `validateRedirectTarget`

`store.go` — a free function of `(ctx, tx, closingID string, resolution *model.Resolution, target *string)`:
- `target == nil` and `resolution != nil && resolution.RedirectsToCanonical()` → `fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)`;
- `target == nil` otherwise → `nil`;
- `*target == closingID` → `fmt.Errorf("cannot redirect %s to itself", closingID)`;
- `currentRetentionTx(ctx, tx, *target)` — a missing row surfaces as `storage.NotFoundError`;
- target retention is `model.Deleted` → `fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted", closingID, *target)`;
- `Archived` targets are accepted (matched as the specific `Deleted` variant only, `store.go`).

Evidence: duplicate close records the redirect target on the issue's own column with no graph edge (`store_test.go`); a terminal `Obsolete` close records the resolution and no redirect (`store_test.go`); a redirect to a nonexistent target rolls the whole close back — status stays `open`, resolution and closed_at nil (`store_test.go`); self-redirect rejected (`store_test.go`); redirect to an **archived** canonical succeeds (`store_test.go`); redirect to a **deleted** canonical is rejected with the target id and `"deleted"` in the message and nothing persisted (`store_test.go`); a delete of the canonical injected in the plan→write window via `applyPreMutationHookForTest` is still observed and the close rejected (`store_test.go`); a redirecting outcome with an empty target is rejected (`store_test.go`).

#### 6.8 In-tx read helpers

`currentStatusTx(ctx, tx, issueID) (string, error)` (`store.go`): `SELECT status FROM issues WHERE id = ?` scanned into `sql.NullString` (status is nullable since containers store NULL). `ErrNoRows` → `storage.NotFoundError{Entity:"issue", ID: issueID}`; other → `fmt.Errorf("read issue status: %w", err)`; returns `status.String` (NULL → `""`).

`currentRetentionTx(ctx, tx, issueID) (model.Retention, error)` (`store.go`): `SELECT archived_at, deleted_at FROM issues WHERE id = ?`; `ErrNoRows` → `storage.NotFoundError`; other → `fmt.Errorf("read issue retention: %w", err)`; both columns through `scanNullableTime` then `model.RetentionFromTimestamps`.

`requireIssueExistsTx(ctx, tx, issueID) error` (`store.go`): `SELECT 1 FROM issues WHERE id = ?`; `ErrNoRows` → `storage.NotFoundError`; other → `fmt.Errorf("check issue exists: %w", err)`. Accepts archived/deleted rows — no `deleted_at` filter (`store.go`).

Evidence: a hard-deleted endpoint makes both `AddRelation` and `SetParent` fail with `storage.NotFoundError` naming that id, writing no edge (`store_test.go`).

#### 6.9 `fieldWrite`, `planFieldUpdate`, `applyFieldsTx`

`fieldWrite` (`store.go`): `issue model.Issue; replaceLabels bool; actor, reason string; changes []model.FieldChange`.

`planFieldUpdate(baseline model.Issue, in storage.UpdateIssueInput, actor string) (fieldWrite, error)` (`store.go`) — pure, no clock, no IO:
- `Title != nil` → `strings.TrimSpace(*in.Title)`; empty result → `errors.New("title cannot be empty")` (`store.go`);
- `Description != nil` → trimmed (`store.go`);
- `Prompt != nil` → trimmed (`store.go`);
- `IssueType != nil` → if `issue.IssueType.IsContainer() != in.IssueType.IsContainer()` → `fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerTypes())` (`store.go`);
- `Priority != nil` → assigned as-is (`store.go`);
- `Assignee != nil` → trimmed (`store.go`);
- `Lane != nil` → trimmed (`store.go`);
- `Labels != nil` → `canonicalizeLabels(*in.Labels)`, error propagated (`store.go`).

Change rows, emitted only for fields that actually moved, in this order (`store.go`): `title`, `description`, `issue_type` (string cast), `priority` (`strconv.Itoa(int(...))` — the numeric wire encoding, not the display name), `assignee` (compared via `AssigneeValue()`), `lane`, `labels` (compared as `strings.Join(labels, ",")` on both sides).

Return: `fieldWrite{issue, replaceLabels: in.Labels != nil, actor, reason: in.Reason, changes}` (`store.go`).

`applyFieldsTx(ctx, tx, w fieldWrite)` (`store.go`):
1. `issue.UpdatedAt = time.Now().UTC()` — the clock is read here, at the write boundary (`store.go`).
2. The UPDATE (`store.go`):
```sql
UPDATE issues SET
    title = ?, description = ?, agent_prompt = ?, priority = ?, issue_type = ?, assignee = ?, lane = ?, updated_at = ?
    WHERE id = ?
```
bound `issue.Title, issue.Description, nullableString(issue.Prompt), issue.Priority, issue.IssueType, issue.AssigneeValue(), issue.Lane, issue.UpdatedAt.Format(time.RFC3339Nano), issue.ID`. Failure → `fmt.Errorf("update issue: %w", err)`. It is **unguarded** (no CAS) but touches no lifecycle column — `status`, `closed_at`, `resolution`, `redirect_target`, `archived_at`, `deleted_at` are all absent from the SET list (`store.go`).
3. `if w.replaceLabels` → `s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, w.actor)` (`store.go`).
4. `if len(w.changes) > 0` → `s.recordEvent(ctx, tx, issue.ID, "" /* empty action */, w.reason, w.actor, w.changes)` (`store.go`). A field-only update writes an event with a **NULL** `action` column (see §7).

Evidence: a field plan taken against a stale snapshot lands its title change while a concurrently-applied close and archive both survive untouched (`store_test.go`); container↔leaf type changes are refused in both directions while a same-kind change (`task`→`bug`) succeeds (`store_test.go`); label replacement through `Apply` replaces the whole set (`store_test.go`).

---

### 7. Event / attribution rows

`recordEvent(ctx, tx, issueID, action, reason, actor string, changes []model.FieldChange) error` — the single insertion point for issue history (`store.go`).

Constructed event (`store.go`):
- `ID = "evt-" + uuid.NewString()`;
- `Action`, `Reason`, `Actor` each `strings.TrimSpace`d;
- `CreatedAt = time.Now().UTC()`;
- `Attribution = s.attribution` — read off the store, never passed in;
- `Changes = changes`.

Then: `if event.Actor == "" { event.Actor = "unknown" }` (`store.go`); `actionArg` is `nil` when the trimmed action is empty, otherwise the string (`store.go`).

The event insert (`store.go`):
```sql
INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at, stream_id, workspace_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
```
bound `event.ID, event.IssueID, actionArg, event.Reason, event.Actor, event.CreatedAt.Format(time.RFC3339Nano), nullableString(event.Attribution.Stream()), nullableString(event.Attribution.Workspace())`. Failure → `fmt.Errorf("insert issue event: %w", err)`.

Per change row (`store.go`):
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
| `AddComment` | — no event at all (`store.go`) | | | |
| `DeleteComment` | — no event at all (`store.go`) | | | |
| `RecordSyncState` | — no event at all (`store.go`) | | | |

Evidence: a create/close/reopen/archive sequence yields exactly 4 events with actions `""(created)`, `close`, `reopen`, `archive` and the reasons given (`store_test.go`); an empty close reason is stored as `""` (`store_test.go`); attribution stamping/absence is covered by the four tests in `event_attribution_test.go` (§2.10). Deriving claims from `ListIssues`+`GetRelationsByIDs`+`ListAllEvents` leaves both the Dolt HEAD and `dolt_status` unchanged — reads write nothing (`claims_readonly_test.go`), and attribution written by the real write path derives back into a `claims.Held` for the right checkout (`claims_readonly_test.go`).

---

### 8. Comments

#### 8.1 `AddComment(ctx, in storage.AddCommentInput) (model.Comment, model.Issue, error)`

`store.go`:
1. `s.GetIssue(ctx, in.IssueID)` — validates existence and doubles as the returned issue, avoiding a second read (`store.go`, doc).
2. `body := strings.TrimSpace(in.Body)`; empty → `errors.New("comment body is required")` (`store.go`).
3. `now := time.Now().UTC()`; `comment := model.Comment{ID: "cmt-" + uuid.NewString(), IssueID: in.IssueID, Body: body, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}`; blank `CreatedBy` → `"unknown"` (`store.go`).
4. `withMutation(ctx, "add comment", ...)` runs:
```sql
INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)
```
with `created_at` as RFC3339Nano; failure → `fmt.Errorf("insert comment: %w", err)` (`store.go`).
5. Returns `(comment, issue, nil)` — the issue is the pre-comment read; a comment never changes the issue row.

#### 8.2 `DeleteComment(ctx, commentID string) (model.Comment, error)`

`store.go`:
1. `id := strings.TrimSpace(commentID)`; empty → `errors.New("comment id is required")` (`store.go`).
2. Inside `withMutation(ctx, "delete comment", ...)`:
   - `SELECT id, issue_id, body, created_at, created_by FROM comments WHERE id = ?` (`store.go`);
   - `sql.ErrNoRows` → `storage.NotFoundError{Entity: "comment", ID: id}` (`store.go`); other → `fmt.Errorf("read comment: %w", err)`;
   - `scanTime(createdAt)` into `deleted.CreatedAt` (`store.go`);
   - `deleteCommentTx(ctx, tx, id)` (`store.go`; `internal/store/row_deletes.go`).
   Existence and deletion share the tx — no TOCTOU gap (`store.go`).
3. Returns the fully-populated deleted comment.

---

### 9. Meta and sync state

`getMeta(ctx, tx *sql.Tx, key string) (string, error)` (`store.go`): uses `tx` when non-nil, else `s.db`:
```sql
SELECT meta_value FROM meta WHERE meta_key = ?
```
`sql.ErrNoRows` → `("", nil)` (absence is not an error); other → `fmt.Errorf("get meta %q: %w", key, err)`.

`setMeta(ctx, tx, key, value string) error` (`store.go`): picks `tx` or `s.db` as the execer, then:
```sql
INSERT INTO meta(meta_key, meta_value) VALUES (?, ?)
        ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)
```
failure → `fmt.Errorf("set meta %q: %w", key, err)`. Note: called with `tx == nil` from `ensureMetaValue`/`ensureMetaDefault`, i.e. **outside** any transaction.

`ensureMetaValue(ctx, guard *snapshotGuard, key, value string) (bool, error)` (`store.go`): reads current; equal → `(false, nil)` with no write; else `guard.ensure(ctx)` (failure → `fmt.Errorf("ensure meta %s: %w", key, err)`), then `setMeta`, returning `(true, nil)`.

`ensureMetaDefault(ctx, guard, key, value)` (`store.go`): identical except the skip condition is `strings.TrimSpace(current) != ""` — any existing non-blank value is preserved.

`GetSyncState(ctx) (storage.SyncState, error)` (`store.go`): two `getMeta` reads — `last_sync_path` into `state.Path` and `last_sync_hash` into `state.ContentHash`; on either error returns `(storage.SyncState{}, err)`.

`RecordSyncState(ctx, state storage.SyncState) error` (`store.go`): one `withMutation(ctx, "record sync state"...)` that `setMeta`s both keys from a map literal — `last_sync_path: strings.TrimSpace(state.Path)` and `last_sync_hash: strings.TrimSpace(state.ContentHash)` — via the tx. Map iteration order means the two writes are unordered relative to each other.

Round-trip evidence: `store_test.go`.

---

### 10. Branch normalization

`masterRenameSource(ctx, db *sql.DB) (string, error)` (`store.go`), lock-free:
- `SELECT active_branch()`; failure → `fmt.Errorf("query dolt active branch: %w", err)`;
- `SELECT name FROM dolt_branches ORDER BY name`; failure → `fmt.Errorf("query dolt branches: %w", err)`; scan failure → `"scan dolt branch: %w"`; iteration failure → `"iterate dolt branches: %w"`;
- counts branches and notes whether `"master"` exists;
- returns `""` (nothing to rename) when `activeBranch == "master"` **or** master already exists **or** `branchCount != 1`;
- otherwise returns the active branch name.

`ensureMasterDefaultBranch(ctx, db)` (`store.go`): consults `masterRenameSource`; on error or empty answer returns immediately; otherwise runs
```sql
CALL DOLT_BRANCH('-m', '<activeBranch with ' doubled>', 'master')
```
built by `fmt.Sprintf` with `strings.ReplaceAll(activeBranch, "'", "''")` (`store.go`); failure → `fmt.Errorf("rename dolt default branch to master: %w", err)`.

Called on every write open (`store.go`) and by the bootstrap (`store.go`).

---

### 11. Cross-file calls made from store.go (noted, not owned here)

| Symbol | Defined at | Called from store.go |
|---|---|---|
| `acquireWorkspaceShared` | `workspace_lock.go` | `store.go` |
| `ErrWorkspaceBusy` | `workspace_lock.go` | `store.go` |
| `requireNoPendingAdopt` | `adopt.go` | `store.go` |
| `withCommitLock` / `withMutation` / `commitWorkingSet` / `isManifestReadOnlyError` | `commit_lock.go` | `store.go` |
| `commitLockPathForDolt` | `commit_lock.go` | `store.go` |
| `s.migrate` | `migration_runner.go` | `store.go` |
| `snapshotGuard` | `migrate_snapshot.go` | `store.go` |
| `newIssueID` | `issue_ids.go` | `store.go` |
| `canonicalizeLabels` / `replaceLabelsTx` | `labels.go` | `store.go` |
| `insertRelationTx` / `bucketRelations` / `relatedFrom` / `siblingsOf` | `relations.go` | `store.go` |
| `smoothRanksIfNeededTx` | `ranking.go` | `store.go` |
| `deleteCommentTx` | `row_deletes.go` | `store.go` |
| `rank.Initial/After/Before` | `internal/rank` | `store.go` |

---

### 12. Test-fixture behavior that constrains store.go semantics

- The package builds **two** migrated-store templates via the real `Open` (`fixture_test.go`), copies them per test (`fixture_test.go`), and freezes the originals read-only (files `0o444`, dirs `0o555`, `fixture_test.go`) — directories are frozen too because Dolt swaps files via `rename(2)`.
- Slot 1 is asserted to share **no** commit hash with slot 0, i.e. two independently created stores have unrelated Dolt histories (`fixture_test.go`, evidence read at `fixture_test.go`).
- Writes through one copy leave no residue in the template or in another copy, and a copy's goose schema version equals a from-scratch `Open`'s (`fixture_residue_test.go`).
- `TestDoltEngineConformance` runs `internal/storage/conformance.Run` against a store from `openIssueStore` (`conformance_test.go`), and `TestDoltEngineOffersEveryCapability` asserts `storage.Offered(engine)` equals `storage.Capabilities()` exactly, in order (`conformance_test.go`).
- `LockDoltJournalExclusive` on an uninitialized workspace refuses with a message containing `"not initialized"` and creates nothing on disk (`dolt_journal_hold_test.go`).


---

## SQL Schema and Schema Reconciliation — Raw Behavioral Inventory

Every claim below carries a `file:line` citation. All paths are relative to
`/Users/bmf/code/links-issue-tracker`.

The engine is Dolt speaking the MySQL wire protocol
(`internal/store/migration_runner.go`: `goose.NewProvider(goose.DialectMySQL, db, migrations.FS)`).

---

## 1. Where schema comes from

There are exactly four producers of DDL that reaches a live workspace:

| Producer | File | Cited at |
|---|---|---|
| goose baseline migration (v1) | `internal/store/migrations/00001_baseline.sql` | `internal/store/migrations/00001_baseline.sql` |
| goose numbered migrations v2–v5 | `00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql` | `internal/store/migrations/00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql` |
| pre-goose reconcile (`reconcileToBaseline`) | `internal/store/schema_reconcile.go` | `internal/store/schema_reconcile.go` |
| quarantine bootstrap (`ensureQuarantineTable`) | `internal/store/migration_runner.go` | `internal/store/migration_runner.go` |

Plus goose's own bookkeeping table, created by the goose library's MySQL
dialect (`internal/store/migration_runner.go` names it; the DDL text is
goose v3.27.1's, `go.mod`).

No other `CREATE TABLE` / `CREATE INDEX` exists in production code — the
repo-wide grep for those statements outside `_test.go`, `.claude/worktrees/`,
and the vendored Dolt driver example returns only the files above
(verified by grep over `internal/` and `cmd/`).

Registry constants:
- Baseline version is `1` (`internal/store/migrations/bounds.go`).
- HEAD version is derived at runtime as the max numeric filename prefix in the
  embedded registry (`internal/store/migrations/bounds.go`); with the
  five files present, HEAD = 5.

---

## 2. THE CONVERGED SCHEMA (v1 + v2..v5), table by table

The authoritative post-migration shape is the byte-compared golden file
`internal/store/schema_snapshot.sql`, which is the verbatim
`SHOW CREATE TABLE` of every table in a freshly-migrated workspace, sorted by
table name, with `goose_db_version` excluded
(`internal/store/schema_drift_test.go`).

Every application table is `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
COLLATE=utf8mb4_0900_bin` (`internal/store/schema_snapshot.sql`).
No `CREATE TABLE` statement in the repo declares an engine, charset, or
collation explicitly — these are Dolt's defaults as rendered back by
`SHOW CREATE TABLE`.

No application table uses `AUTO_INCREMENT`
(`internal/store/schema_snapshot.sql` — no occurrence). The only
`AUTO_INCREMENT` column in the database is `goose_db_version.id` (§2.9), which
is why that table is excluded from the snapshot
(`internal/store/schema_snapshot.sql`).

### 2.1 `issues`

Effective shape (`internal/store/schema_snapshot.sql`):

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
| `id` | `VARCHAR(191)` | NOT NULL (PRIMARY KEY) | none | `00001_baseline.sql` |
| `title` | `TEXT` | NOT NULL | none | `00001_baseline.sql` |
| `description` | `TEXT` | NOT NULL | none | `00001_baseline.sql` |
| `agent_prompt` | `TEXT` | NULL | none | `00001_baseline.sql` |
| `status` | `VARCHAR(32)` | NULL | none | `00001_baseline.sql` |
| `priority` | `INT` | NOT NULL | none | `00001_baseline.sql` |
| `issue_type` | `VARCHAR(32)` | NOT NULL | none | `00001_baseline.sql` |
| `topic` | `VARCHAR(191)` | NOT NULL | none | `00001_baseline.sql` |
| `assignee` | `TEXT` | NOT NULL | none | `00001_baseline.sql` |
| `created_at` | `VARCHAR(64)` | NOT NULL | none | `00001_baseline.sql` |
| `updated_at` | `VARCHAR(64)` | NOT NULL | none | `00001_baseline.sql` |
| `closed_at` | `VARCHAR(64)` | NULL | none | `00001_baseline.sql` |
| `archived_at` | `VARCHAR(64)` | NULL | none | `00001_baseline.sql` |
| `deleted_at` | `VARCHAR(64)` | NULL | none | `00001_baseline.sql` |
| `item_rank` | `TEXT` | NOT NULL | `''` | `00001_baseline.sql` |
| `lane` | `text` | NOT NULL | `''` | `00002_add_lane.sql` |
| `resolution` | `VARCHAR(32)` | NULL | none | `00003_add_resolution.sql` |
| `redirect_target` | `VARCHAR(191)` | NULL | none | `00004_add_redirect_target.sql` |

- PRIMARY KEY: `(id)` — declared inline as `PRIMARY KEY` on the column (`00001_baseline.sql`).
- No UNIQUE constraints.
- Secondary indexes:
  - `idx_issues_status_priority (status, priority, updated_at)` — `00001_baseline.sql`, also created by reconcile at `internal/store/schema_reconcile.go`.
  - `idx_issues_rank (item_rank(191))` — prefix index of length 191 on a `TEXT` column — `00001_baseline.sql`, also `internal/store/schema_reconcile.go`.
- No FOREIGN KEYs on `issues`. `redirect_target` deliberately has **no** FK to `issues(id)` (`00004_add_redirect_target.sql`).
- CHECK constraints (five, all explicitly named so `SHOW CREATE TABLE` is deterministic — `00001_baseline.sql`):
  - `issues_status_check` — `00001_baseline.sql`; the identical clause is generated in Go at `internal/store/schema_reconcile.go` and installed by `ensureStatusConstraint` at `internal/store/schema_reconcile.go`.
  - `issues_priority_check` — `00001_baseline.sql`; Go form at `internal/store/schema_reconcile.go`, installed at `internal/store/schema_reconcile.go`.
  - `issues_type_check` — `00001_baseline.sql`; Go form at `internal/store/schema_reconcile.go`.
  - `issues_resolution_check` — `00003_add_resolution.sql`.
  - `issues_redirect_target_check` — `00004_add_redirect_target.sql`.

Note the ordering difference: the file declares the indexes as
`idx_issues_status_priority` then `idx_issues_rank`
(`00001_baseline.sql`), but `SHOW CREATE TABLE` renders them
alphabetically, `idx_issues_rank` first
(`internal/store/schema_snapshot.sql`).

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

`internal/store/schema_snapshot.sql`. Source declaration:
`00001_baseline.sql`; reconcile's identical copy:
`internal/store/schema_reconcile.go`.

- PRIMARY KEY `(src_id, dst_id, type)` — `00001_baseline.sql`.
- No UNIQUE constraints.
- Indexes: `idx_relations_src_type (src_id, type)` (`00001_baseline.sql`; reconcile `schema_reconcile.go`), `idx_relations_dst_type (dst_id, type)` (`00001_baseline.sql`; reconcile `schema_reconcile.go`), plus the Dolt-auto-generated FK backing index `KEY dst_id (dst_id)` (`internal/store/schema_snapshot.sql`) — no such statement exists in any migration file; it materializes from the `dst_id` FK.
- FOREIGN KEYs, both `ON DELETE CASCADE`, no `ON UPDATE` clause declared: `src_id → issues(id)` and `dst_id → issues(id)` (`00001_baseline.sql`), auto-named `relations_ibfk_1` / `relations_ibfk_2` (`internal/store/schema_snapshot.sql`).
- CHECK `relations_type_check` on `type IN ('blocks','parent-child','related-to')` (`00001_baseline.sql`).

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

`internal/store/schema_snapshot.sql`. Source: `00001_baseline.sql`;
reconcile copy: `internal/store/schema_reconcile.go`.

- PRIMARY KEY `(id)` — `00001_baseline.sql`.
- Index `idx_comments_issue_created (issue_id, created_at)` — `00001_baseline.sql`; reconcile `schema_reconcile.go`.
- Auto FK-backing index `KEY issue_id (issue_id)` (`internal/store/schema_snapshot.sql`), not declared anywhere.
- FK `issue_id → issues(id) ON DELETE CASCADE` (`00001_baseline.sql`), auto-named `comments_ibfk_1`.
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

`internal/store/schema_snapshot.sql`. Source: `00001_baseline.sql`;
reconcile copy: `internal/store/schema_reconcile.go`.

- PRIMARY KEY `(issue_id, label)` — `00001_baseline.sql`.
- Indexes `idx_labels_issue (issue_id, label)` (`00001_baseline.sql`; reconcile) and `idx_labels_name (label, issue_id)` (`00001_baseline.sql`; reconcile).
- No separate auto FK index appears — the PK's leading `issue_id` covers it (`internal/store/schema_snapshot.sql`).
- FK `issue_id → issues(id) ON DELETE CASCADE` (`00001_baseline.sql`), auto-named `labels_ibfk_1`.

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

`internal/store/schema_snapshot.sql`. Baseline columns:
`00001_baseline.sql`; reconcile copy:
`internal/store/schema_reconcile.go`.
`stream_id VARCHAR(64) NULL` added at `00005_add_event_attribution.sql`;
`workspace_id VARCHAR(191) NULL` at `00005_add_event_attribution.sql`.

- PRIMARY KEY `(id)` — `00001_baseline.sql`.
- Index `idx_issue_events_issue_created (issue_id, created_at)` — `00001_baseline.sql`; reconcile `schema_reconcile.go`.
- Auto FK-backing index `KEY issue_id (issue_id)` (`internal/store/schema_snapshot.sql`).
- FK `issue_id → issues(id) ON DELETE CASCADE` (`00001_baseline.sql`), auto-named `issue_events_ibfk_1`.
- No CHECK, no UNIQUE.
- Historical column name: `assignee` was the pre-v1 name of `actor`; reconcile renames it (`internal/store/schema_reconcile.go`), and the shapemap records both spellings mapping to the same domain field (`internal/store/shapemap_known.go`).

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

`internal/store/schema_snapshot.sql`. Source: `00001_baseline.sql`;
reconcile copy: `internal/store/schema_reconcile.go`.

- PRIMARY KEY `(event_id, field)` — `00001_baseline.sql`.
- No secondary index (the PK's leading `event_id` covers the FK — no `KEY event_id` row appears, `internal/store/schema_snapshot.sql`).
- FK `event_id → issue_events(id) ON DELETE CASCADE` (`00001_baseline.sql`), auto-named `issue_event_changes_ibfk_1`.

### 2.7 `meta`

```sql
CREATE TABLE `meta` (
  `meta_key` varchar(191) NOT NULL,
  `meta_value` text NOT NULL,
  PRIMARY KEY (`meta_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
```

`internal/store/schema_snapshot.sql`. Source: `00001_baseline.sql`;
reconcile copy: `internal/store/schema_reconcile.go` (the **first**
step in reconcile's list).

- PRIMARY KEY `(meta_key)`; no indexes, no FKs, no CHECKs.
- Written through `INSERT ... ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)` (`internal/store/store.go`); read at `internal/store/store.go`, absent key yields `""` not an error (`internal/store/store.go`).
- Keys observed in production code:
  - `workspace_id` — written by reconcile via `ensureMetaValue` (`internal/store/schema_reconcile.go`).
  - `producer_binary_version` — const at `internal/store/migration_runner.go`, written at `internal/store/migration_runner.go`; nothing in the tree reads it.
  - `last_sync_path`, `last_sync_hash` — read at `internal/store/store.go`, written at `internal/store/store.go`.

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

`internal/store/schema_snapshot.sql`. The DDL executed is the Go
constant at `internal/store/migration_runner.go` — declared as
`version BIGINT NOT NULL`, `name TEXT NOT NULL`, `error_text TEXT NOT NULL`,
`created_at VARCHAR(64) NOT NULL`, `PRIMARY KEY (version)`.

- Created **outside** the goose batch, before the snapshot guard, so a goose rollback cannot erase it (`internal/store/migration_runner.go`).
- Canonical column set pinned in Go: `{"version","name","error_text","created_at"}` (`internal/store/migration_runner.go`).
- Not defined in any migration file — grep of `internal/store/migrations/*.sql` shows no `migration_quarantine`.

### 2.9 `goose_db_version`

Created by the goose library, not by this repo. MySQL dialect DDL
(`/Users/bmf/go/pkg/mod/github.com/pressly/goose/v3@v3.27.1/internal/dialects/mysql.go`,
selected by `internal/store/migration_runner.go`):

```sql
CREATE TABLE goose_db_version (
  id bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  version_id bigint NOT NULL,
  is_applied boolean NOT NULL,
  tstamp timestamp NULL default now(),
  PRIMARY KEY(id)
)
```

- Table name constant: `internal/store/migration_runner.go`.
- Rows are inserted as `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, ?)` (`.../mysql.go`).
- Excluded from the snapshot golden file because it is bookkeeping and its `AUTO_INCREMENT` counter is nondeterministic (`internal/store/schema_snapshot.sql`; enforced in code at `internal/store/schema_drift_test.go`).
- Classified as a bookkeeping table with no domain mapping (`internal/store/shapemap_known.go`).

### 2.10 `issue_history` — the legacy table that no longer exists

Never created by any current producer. Its presence is the disk-truth marker
of a pre-goose workspace (`internal/store/migration_runner.go`), and
reconcile drops it (`internal/store/schema_reconcile.go`). The column
set the translation requires is
`{id, issue_id, action, reason, from_status, to_status, created_at, created_by}`
(`internal/store/schema_reconcile.go`); the test fixture that
reproduces the historical shape declares
`id VARCHAR(191) PRIMARY KEY, issue_id VARCHAR(191) NOT NULL,
action VARCHAR(64) NULL, reason TEXT NULL, from_status VARCHAR(32) NULL,
to_status VARCHAR(32) NULL, created_at VARCHAR(64) NOT NULL,
created_by TEXT NOT NULL`
(`internal/store/schema_reconcile_test.go`).

---

## 3. Baseline-only shape (what v1 alone produces)

`baselineSchema()` parses the embedded `00001_baseline.sql` Up section into a
table→columns map (`internal/store/migration_runner.go`), reading
only the section between `-- +goose up` and `-- +goose down`
(`internal/store/migration_runner.go`) and only the first identifier
of each top-level comma-separated item that is not a constraint keyword
(`internal/store/migration_runner.go`). `CREATE INDEX` statements are
ignored entirely (`internal/store/migration_runner.go`).

The exact parsed result is pinned by test
(`internal/store/baseline_schema_test.go`):

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
pseudo-column (`internal/store/baseline_schema_test.go`).

---

## 4. The baseline file is byte-frozen

`00001_baseline.sql` is pinned by SHA-256:
`e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb`
(`internal/store/migrations/baseline_frozen_test.go`). `TestBaselineFileIsFrozen`
reads the embedded file and compares
(`internal/store/migrations/baseline_frozen_test.go`). The failure
message forbids updating the constant and forbids even comment/whitespace
edits (`internal/store/migrations/baseline_frozen_test.go`).

This hash is the only content fingerprint in the schema system. There is **no**
schema-version hash or fingerprint stored in the database. The recorded schema
version in the database is the integer `version_id` in `goose_db_version`
(§2.9), plus the `producer_binary_version` string row in `meta` (§2.7).

---

## 5. `reconcileToBaseline` — the pre-goose → v1 forward migrator

Entry point: `internal/store/schema_reconcile.go`. Runs only in
`phaseAdopt`, after the snapshot guard fires, before the v1 stamp
(`internal/store/migration_runner.go`).

Signature returns `(changed bool, err error)`; `changed` is true iff any step
performed a write (`internal/store/schema_reconcile.go`).

### 5.1 Phase classification that decides whether reconcile runs at all

`classifyMigrationState` (`internal/store/migration_runner.go`):

1. Read registry max version.
2. If table `issue_history` exists → `phaseAdopt` unconditionally, regardless of `goose_db_version` (`internal/store/migration_runner.go`).
3. Else if `goose_db_version` exists → `phaseManaged` with the recorded version (`internal/store/migration_runner.go`).
4. Else run `verifyBaselineShape`; if `present == 0` → `phaseFresh`, otherwise `phaseAdopt` (`internal/store/migration_runner.go`).

So: **empty database** → `phaseFresh` → reconcile never runs; goose applies
`00001_baseline.sql` then v2..v5. **Populated pre-goose database** (any
canonical table present, no goose log) → `phaseAdopt` → reconcile runs.

### 5.2 The step ordering, exactly

Ordered list, all inside `reconcileToBaseline`:

**Stage A — declarative DDL list** (`internal/store/schema_reconcile.go`), run in slice order by the loop:

1. `CREATE TABLE meta`
2. `CREATE TABLE issues` via `createIssuesTableStmt()` (body)
3. `CREATE TABLE relations`
4. `CREATE TABLE comments`
5. `CREATE TABLE labels`
6. `CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at)`
7. `CREATE INDEX idx_relations_src_type ON relations(src_id, type)`
8. `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type)`
9. `CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at)`
10. `CREATE INDEX idx_labels_issue ON labels(issue_id, label)`
11. `CREATE INDEX idx_labels_name ON labels(label, issue_id)`
12. `CREATE TABLE issue_events`
13. `CREATE TABLE issue_event_changes`
14. `CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at)`

Note the FK-correct ordering: `issues` precedes `relations`/`comments`/`labels`/`issue_events`;
`issue_events` precedes `issue_event_changes`. `meta` (no FKs) is first.

`createIssuesTableStmt()` emits the v1 issues shape **without** `lane`,
`resolution`, or `redirect_target` (`internal/store/schema_reconcile.go`)
— those arrive later from goose migrations v2–v4 after adoption stamps v1.

**Stage B — mutations, in this exact order:**

15. Drop `goose_db_version` if present — `DROP TABLE goose_db_version`, label `"drop fabricated goose_db_version (legacy workspace carried lying bookkeeping)"` (`internal/store/schema_reconcile.go`).
16. Rename `issue_events.assignee` → `actor` if the `assignee` column exists — `ALTER TABLE issue_events RENAME COLUMN assignee TO actor` (`internal/store/schema_reconcile.go`). Must precede step 17.
17. `translateIssueHistoryToEvents` (`internal/store/schema_reconcile.go`, implementation) — §5.5.
18. Drop `issue_history` if present — `DROP TABLE IF EXISTS issue_history`, label `"drop legacy issue_history table"` (`internal/store/schema_reconcile.go`).
19. Add `issues.item_rank` if missing — `ALTER TABLE issues ADD COLUMN item_rank TEXT NOT NULL DEFAULT ''` (`internal/store/schema_reconcile.go`).
20. Create index `idx_issues_rank ON issues(item_rank(191))` if absent (`internal/store/schema_reconcile.go`).
21. Add `issues.topic` if missing — `ALTER TABLE issues ADD COLUMN topic VARCHAR(191) NOT NULL DEFAULT 'misc' AFTER issue_type` (`internal/store/schema_reconcile.go`).
22. Drop that default if `column_default IS NOT NULL` — `ALTER TABLE issues MODIFY topic VARCHAR(191) NOT NULL`, label `"drop topic default to match baseline shape"` (`internal/store/schema_reconcile.go`). Rationale: baseline declares `topic` with no default, so a reconcile-built column would otherwise differ.
23. Rename `issues.prompt` → `agent_prompt` if the `prompt` column exists — ``ALTER TABLE issues RENAME COLUMN `prompt` TO agent_prompt`` (`internal/store/schema_reconcile.go`); `prompt` is backtick-quoted because it is reserved in Dolt's MySQL parser.
24. Add `issues.agent_prompt` if missing — ``ALTER TABLE issues ADD COLUMN agent_prompt TEXT NULL AFTER `description` `` (`internal/store/schema_reconcile.go`).
25. Relax `issues.agent_prompt` to nullable if `is_nullable='NO'` — `ALTER TABLE issues MODIFY agent_prompt TEXT NULL` (`internal/store/schema_reconcile.go`).
26. `ensureUnifiedStatusSchema` (`internal/store/schema_reconcile.go`, implementation) — §5.6.
27. `ensureIssueTopics` (`internal/store/schema_reconcile.go`, implementation) — §5.7.
28. `ensureIssueRanks` (`internal/store/schema_reconcile.go`, implementation) — §5.8.
29. `resetPrioritiesToNormal` (`internal/store/schema_reconcile.go`, implementation) — §5.9.
30. `ensureMetaValue(ctx, guard, "workspace_id", s.workspaceID)` (`internal/store/schema_reconcile.go`; helper at `internal/store/store.go`).

Any step returning an error aborts immediately, returning the `changed` value
accumulated so far (`internal/store/schema_reconcile.go` and each
subsequent `if err != nil { return changed, err }` block).

### 5.3 How each step decides skip-vs-execute (the drift-detection primitives)

Three gate helpers, all built on `probeYields`:

- `probeYields(ctx, probe, label)` (`internal/store/schema_reconcile.go`): runs the probe as `QueryRow(...).Scan(&int)`. `err == nil` → true; `sql.ErrNoRows` → false; any other driver error → `fmt.Errorf("%s: probe: %w", label, err)`.

- `execGatedCreate(ctx, guard, probe, stmt, label)` (`internal/store/schema_reconcile.go`): if probe yields → skip, return `(false, nil)`. Otherwise `guard.ensure(ctx)`, then `ExecContext(stmt)`. On exec error the message is lowercased and if it contains `"already exists"`, `"duplicate column"`, or `"duplicate key name"` the error is **swallowed** and `(false, nil)` returned; any other error → `fmt.Errorf("%s: %w", label, err)`. A `guard.ensure` failure → `fmt.Errorf("%s: %w", label, snapErr)`.

- `execGatedMutation(ctx, guard, probe, stmt, label)` (`internal/store/schema_reconcile.go`): if probe does **not** yield → skip. Otherwise `guard.ensure`, then exec; **no swallow** — any exec error becomes `fmt.Errorf("%s: %w", label, err)`.

Probe SQL by step class:

- **Table existence** (`ddlStep` with empty `parent`) — `SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '<target>' LIMIT 1` (`internal/store/schema_reconcile.go`).
- **Index existence** (`ddlStep` with non-empty `parent`) — `SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = '<parent>' AND index_name = '<target>' LIMIT 1` (`internal/store/schema_reconcile.go`).
- **Column presence** (`execGatedColumnAdd`) — `SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '<t>' AND column_name = '<c>' LIMIT 1`, label `"add column <t>.<c>"` (`internal/store/schema_reconcile.go`).
- **Column nullability** (`execGatedColumnRelax`) — same query plus `AND is_nullable = 'NO'`, label `"relax column <t>.<c> to nullable"`, routed through `execGatedMutation` (propagate, not swallow) (`internal/store/schema_reconcile.go`).
- **Column default presence** — `... AND column_name = 'topic' AND column_default IS NOT NULL LIMIT 1` (`internal/store/schema_reconcile.go`).
- **CHECK-constraint shape** — see §5.9/§5.10.
- **Row-level data predicates** — plain `SELECT 1 FROM issues WHERE <predicate> LIMIT 1` (`internal/store/schema_reconcile.go`).

Explicitly **not** compared anywhere in reconcile: column SQL type, column
length/precision, index column list, index uniqueness, foreign-key presence or
actions, charset/collation, engine. The only type-shaped things compared are
(a) nullability of two specific columns, (b) the presence of a default on
`issues.topic`, (c) the normalized text of `issues` CHECK clauses. A table that
exists with wrong columns is skipped by the CREATE step
(`internal/store/schema_reconcile_test.go`); the post-reconcile
baseline verification is the net that catches it (§5.11).

### 5.4 The `ddlStep` type

`type ddlStep struct { target, parent, stmt string }`
(`internal/store/schema_reconcile.go`); `parent` empty means CREATE
TABLE, non-empty names the table an index lives on
(`internal/store/schema_reconcile.go`). `runGatedCreate` derives the
probe from the step and labels it `"create <target>"`
(`internal/store/schema_reconcile.go`).

### 5.5 `translateIssueHistoryToEvents`

`internal/store/schema_reconcile.go`.

Preconditions, in order:

1. `tableExists("issue_history")`; false → `(false, nil)`. Probe error → `"translate issue_history: probe table: %w"`.
2. `tableColumns("issue_history")`; error → `"translate issue_history: probe columns: %w"`. If any of `id, issue_id, action, reason, from_status, to_status, created_at, created_by` is absent → `(false, nil)`, table left for the drop step.
3. Existence pre-check (no snapshot taken) — `SELECT 1 FROM issue_history h WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = h.issue_id) AND NOT EXISTS (SELECT 1 FROM issue_events e WHERE e.id = h.id) LIMIT 1`, label `"translate issue_history: pending probe"`. No rows → `(false, nil)`.

Then `guard.ensure` (error → `"translate issue_history: %w"`),
`BeginTx` (error → `"translate issue_history: begin tx: %w"`), with
a deferred `tx.Rollback()`.

Inside the tx it SELECTs `h.id, h.issue_id, h.action, h.reason, h.created_by,
h.created_at, h.from_status, h.to_status` under the same
EXISTS/NOT-EXISTS filter, buffers all rows into a slice
, and if the buffer is empty returns `(false, nil)`.

Prepared statements:
- `INSERT INTO issue_events (id, issue_id, action, reason, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`
- `INSERT INTO issue_event_changes (event_id, field, from_value, to_value) VALUES (?, 'status', ?, ?)`

Per row: insert the event with canonicalized `action`, `reason`,
`actor`; then normalize both statuses and, if `isLegacyStatusTransition`, insert
one `issue_event_changes` row with `field = 'status'`.

Canonicalization functions:
- `canonicalEventAction` — NULL → `nil`; TrimSpace; empty → `nil`; else trimmed (`internal/store/schema_reconcile.go`).
- `canonicalEventReason` — NULL → `""`; else TrimSpace.
- `canonicalEventActor` — NULL → `"unknown"`; TrimSpace; empty → `"unknown"`; else trimmed.
- `canonicalLegacyStatus` — NULL stays NULL; `open`/`in_progress`/`closed` pass through; `in-progress`→`in_progress`; `todo`→`open`; `done`→`closed`; anything else → `open`.
- `isLegacyStatusTransition` — both NULL → false; both valid and equal → false; otherwise true.
- `nullableSQLString` — invalid → `nil`, valid → the string.

Commit error → `"translate issue_history: commit tx: %w"`;
success returns `(true, nil)`.

### 5.6 `ensureUnifiedStatusSchema`

`internal/store/schema_reconcile.go`.

1. Relax `issues.status` to nullable if `is_nullable='NO'` — `ALTER TABLE issues MODIFY status VARCHAR(32) NULL`.
2. Seven probe/UPDATE pairs, in list order, each run via `execGatedMutation`:

| # | Probe | Statement | Label / context |
|---|---|---|---|
| 1 | `SELECT 1 FROM issues WHERE status = 'in-progress' LIMIT 1` | `UPDATE issues SET status = 'in_progress' WHERE status = 'in-progress'` | `normalize legacy in-progress status` |
| 2 | `... WHERE status = 'todo' LIMIT 1` | `UPDATE issues SET status = 'open' WHERE status = 'todo'` | `normalize legacy todo status` |
| 3 | `... WHERE status = 'done' LIMIT 1` | `UPDATE issues SET status = 'closed' WHERE status = 'done'` | `normalize legacy done status` |
| 4 | `... WHERE status NOT IN ('open','in_progress','closed') LIMIT 1` | `UPDATE issues SET status = 'open' WHERE status NOT IN ('open','in_progress','closed')` | `normalize invalid status` |
| 5 | `... WHERE closed_at IS NOT NULL AND status <> 'closed' LIMIT 1` | `UPDATE issues SET status = 'closed' WHERE closed_at IS NOT NULL AND status <> 'closed'` | `normalize closed_at status` |
| 6 | `... WHERE status <> 'closed' AND closed_at IS NOT NULL LIMIT 1` | `UPDATE issues SET closed_at = NULL WHERE status <> 'closed' AND closed_at IS NOT NULL` | `normalize non-closed closed_at` |
| 7 | `SELECT 1 FROM issues WHERE issue_type IN ('epic') AND status IS NOT NULL LIMIT 1` | `UPDATE issues SET status = NULL WHERE issue_type IN ('epic') AND status IS NOT NULL` | `null out container status` |

Pair 7's `issue_type IN ('epic')` text is generated from
`model.ContainerTypes()` (`internal/store/schema_reconcile.go`).

3. `ensureStatusConstraint` — §5.10.

### 5.7 `ensureIssueTopics`

`internal/store/schema_reconcile.go`. One gated mutation:
probe `SELECT 1 FROM issues WHERE TRIM(COALESCE(topic, '')) = '' LIMIT 1`,
statement `UPDATE issues SET topic = 'misc' WHERE TRIM(COALESCE(topic, '')) = ''`,
label `"backfill legacy issue topics"`.

### 5.8 `ensureIssueRanks`

`internal/store/schema_reconcile.go`.

- Query: `SELECT id FROM issues WHERE item_rank = '' ORDER BY status ASC, priority ASC, updated_at DESC, id ASC`. Errors: `"ensureIssueRanks: query unranked: %w"`, `"ensureIssueRanks: scan: %w"`, `"ensureIssueRanks: rows: %w"`.
- Zero unranked rows → `(false, nil)`.
- `guard.ensure` error → `"ensureIssueRanks: %w"`.
- `BeginTx` error → `"ensureIssueRanks: begin tx: %w"`; deferred rollback.
- Seed: `SELECT MAX(item_rank) FROM issues WHERE item_rank != ''`; error → `"ensureIssueRanks: read max existing rank: %w"`. If a max exists and is non-empty, `current = rank.After(max)`, else `current = rank.Initial()`.
- Prepared `UPDATE issues SET item_rank = ? WHERE id = ?`; prepare error → `"ensureIssueRanks: prepare: %w"`; per-row exec error → `"ensureIssueRanks: update %s: %w"`; `current = rank.After(current)` after each.
- Commit error → `"ensureIssueRanks: commit tx: %w"`; success `(true, nil)`.

### 5.9 `resetPrioritiesToNormal`

`internal/store/schema_reconcile.go`.

- `listIssuePriorityCheckConstraints` queries
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
 and keeps rows whose normalized clause contains the substring `"priority"`. Errors: `"query issue check constraints: %w"`, `"scan issue check constraint: %w"`, `"iterate issue check constraints: %w"`.
- `hasCanonicalPriorityConstraint`: requires **exactly one** matching constraint, and its normalized clause must contain `"priority<=1"` (formatted from `model.PriorityUrgent`). If true → skip, `(false, nil)`.
- Otherwise: `guard.ensure` (error → `"reset priorities to normal: %w"`); `UPDATE issues SET priority = 0` (error → `"reset priorities to normal: %w"`); for every matched constraint `ALTER TABLE issues DROP CHECK \`<name>\`` with backticks in the name doubled (error → `"drop priority check %s: %w"`); then `ALTER TABLE issues ADD CONSTRAINT issues_priority_check CHECK (priority >= 0 AND priority <= 1)` (error → `"add priority check: %w"`). Returns `(true, nil)`.

### 5.10 `ensureStatusConstraint`

`internal/store/schema_reconcile.go`.

- `listIssueStatusCheckConstraints` runs the same
  `information_schema` join as §5.9 and keeps rows whose
  normalized clause contains `"statusin("`. Same three error
  strings as §5.9.
- `hasCanonicalStatusConstraint` requires **exactly one** constraint and all five of these on the normalized clause:
  1. contains `issue_typein('epic')`
  2. contains `statusin('open','in_progress','closed')`
  3. contains `statusisnotnull` **or** `not(statusisnull)`
  4. contains `andstatusisnull`
  5. `hasNegatedEpicGuard` is true
- `hasNegatedEpicGuard`: true if the clause contains `issue_typenotin('epic')`, or if any occurrence of `not(` — after skipping any run of further `(` characters — is immediately followed by `issue_typein('epic')`.
- `normalizeConstraintClause`: strips spaces, tabs, newlines, and backticks, then lowercases.
- If not canonical: `guard.ensure` (error → `"ensure status constraint: %w"`); drop each matched constraint via `ALTER TABLE issues DROP CHECK \`<name>\`` with doubled backticks (error → `"drop status check %s: %w"`); then `ALTER TABLE issues ADD CONSTRAINT issues_status_check CHECK ((issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed')))` (error → `"add canonical status check: %w"`).

The tolerance for Dolt's rewriting (`NOT IN` → `NOT(... IN ...)`,
`IS NOT NULL` → `NOT(... IS NULL)`, added backticks) is documented at
`internal/store/schema_reconcile.go` and visible in the snapshot's
rendered clause (`internal/store/schema_snapshot.sql`).

### 5.11 Pre- and post-reconcile gates

**Pre-gate — `verifyIssuesReconcilable`** (`internal/store/schema_reconcile.go`),
called from `runMigration` before the snapshot guard, only in `phaseAdopt`
(`internal/store/migration_runner.go`):

- Reads `tableColumns("issues")`. Zero columns (table absent) → `nil` (reconcile will CREATE it).
- Required set: `{"status","priority","updated_at","issue_type","closed_at","description"}` (`internal/store/schema_reconcile.go`).
- Any missing → error text:
  ```
  workspace's issues table is missing reconcile prerequisites (<comma-joined missing>); the shape is structurally beyond what pre-goose reconcile can recover — this is not a known historical shape
  ```
  (`internal/store/schema_reconcile.go`). Wrapped by the caller as `"reconcile pre-goose workspace: %w"` (`internal/store/migration_runner.go`).

**Post-gate — `verifyBaselineShape`** (`internal/store/migration_runner.go`):
for each baseline table (sorted), read `tableColumns`; absent table appends the
table name to `missing`; present table increments `present` and appends
`table.column` for each baseline column not found. Column names are lowercased
on read (`internal/store/migration_runner.go`). It compares **column
presence only** — never types, nullability, defaults, indexes, or keys.

Called after reconcile at `internal/store/migration_runner.go`; any
remaining gap aborts before the stamp with:
```
post-reconcile workspace shape still differs from baseline (remaining gaps: <comma-joined>); reconcile cannot bring this workspace to v1 — the shape is structurally beyond what pre-goose reconcile can recover
```
(`internal/store/migration_runner.go`). Wrapping error for a probe
failure: `"verify post-reconcile baseline shape: %w"`
(`internal/store/migration_runner.go`). The whole reconcile call is
wrapped `"reconcile pre-goose workspace: %w"`
(`internal/store/migration_runner.go`).

`refuseIfBaselineMissing` (`internal/store/migration_runner.go`) uses
the same shape check when the goose log records a version above the registry
max, returning `UnsupportedSchemaVersionError` carrying `MissingBaseline`.

### 5.12 Tables deliberately excluded from reconcile

- `migration_quarantine` — created outside reconcile and outside the goose batch (`internal/store/migration_runner.go`, DDL).
- `goose_db_version` — reconcile *drops* it rather than creating it (`internal/store/schema_reconcile.go`); adoption recreates and stamps it (`internal/store/schema_reconcile.go`).
- `issue_history` — dropped, never created (`internal/store/schema_reconcile.go`).
- The post-baseline columns `lane`, `resolution`, `redirect_target` and `issue_events.stream_id`/`workspace_id` are absent from reconcile's `CREATE TABLE issues` / `CREATE TABLE issue_events` (`internal/store/schema_reconcile.go`) — goose adds them after adoption.
- The drift-canary dump excludes only `goose_db_version` (`internal/store/schema_drift_test.go`).
- The shapemap's "bookkeeping, no domain field" set is `goose_db_version`, `migration_quarantine`, `meta` (`internal/store/shapemap_known.go`).

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
`create <target>`, `add column <table>.<column>`,
`relax column <table>.<column> to nullable`,
`drop fabricated goose_db_version (legacy workspace carried lying bookkeeping)`,
`rename issue_events.assignee to actor`,
`drop legacy issue_history table`,
`drop topic default to match baseline shape`,
`rename prompt column to agent_prompt`,
plus the seven `ensureUnifiedStatusSchema` contexts
and `backfill legacy issue topics`.

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

And from `internal/store/downgrade.go`:
`downgrade: workspace is not goose-managed (no goose_db_version table); run Open first to adopt or initialize`.

---

## 7. Derived-from-Go schema literals

Three CHECK clause fragments are generated in Go from the sealed model
vocabularies rather than hand-written:

- `priorityCheckClause = fmt.Sprintf("priority >= %d AND priority <= %d", model.PriorityNormal, model.PriorityUrgent)` (`internal/store/schema_reconcile.go`), with `PriorityNormal = 0`, `PriorityUrgent = 1` (`internal/model/priority.go`).
- `issueTypeCheckClause = "issue_type IN (" + quoted(model.IssueTypes()) + ")"` (`internal/store/schema_reconcile.go`), where `IssueTypes()` returns `task, feature, bug, chore, epic` in that order (`internal/model/issue_type.go`).
- `containerTypeMembership = "issue_type IN (" + quoted(model.ContainerTypes()) + ")"` (`internal/store/schema_reconcile.go`), where `ContainerTypes()` is the `IsContainer()` subset — only `epic` (`internal/model/issue_type.go`).
- `canonicalStatusCheckClause` composes the container list twice (`internal/store/schema_reconcile.go`).
- `quotedIssueTypeList` renders `'a','b','c'` with no spaces (`internal/store/schema_reconcile.go`).

---

## 8. What the tests pin about the schema — exact assertions

### `internal/store/schema_drift_test.go`

- **`TestSchemaSnapshotMatchesConvergedSchema`**: opens a fresh workspace, dumps `SHOW CREATE TABLE` for every table in `information_schema.tables WHERE table_schema = DATABASE()` except `goose_db_version`, sorted, each terminated with `;`, joined by `\n\n`, prefixed with the header constant and `\n\n`, suffixed `\n`, and byte-compares against `schema_snapshot.sql`. The header text itself is part of the compared document, so stripping the warning is drift. `-update-schema-snapshot` rewrites the file at mode `0o644`. No normalization is applied to the DDL — a Dolt formatting change is treated as real drift.
- **`TestConvergedSchemaDumpIsDeterministic`**: two independently-migrated fresh workspaces must serialize byte-identically.
- **`TestConvergedSchemaDumpIncludesUnexpectedTables`**: creates `leftover_legacy (id VARCHAR(191) PRIMARY KEY)` and asserts the dump string contains `leftover_legacy` — i.e. the canary enumerates from the live DB, not a fixed list.

### `internal/store/migrations/baseline_frozen_test.go`

- **`TestBaselineFileIsFrozen`**: SHA-256 of `00001_baseline.sql` must equal `e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb`.

### `internal/store/baseline_schema_test.go`

- **`TestBaselineSchemaParsesEmbeddedMigration`**: the parsed baseline is exactly seven tables with exactly the column lists reproduced in §3; table count must match and each table's sorted column list must match exactly.
- **`TestOpenForwardMigratesPreConvergedColumnShape`**: after `ALTER TABLE issues DROP COLUMN topic` + revert-to-baseline + drop goose log, reopening must succeed, `verifyBaselineShape` must report zero missing, `recordedMigrationVersion` must equal HEAD, and the seeded issue must still be findable with its title intact.

### `internal/store/schema_reconcile_test.go`

Shared harness: `hijackToPreGoose` runs the post-baseline migrations' Down
sections via `provider.DownTo(ctx, baselineVersion)` then `DROP TABLE
goose_db_version` and commits. `assertReachedBaseline` asserts
`Open` succeeds, `verifyBaselineShape` returns zero missing, and
`recordedMigrationVersion() == headVersion(t)`.

- **`TestReconcileAddsMissingIssueEventsTables`**: with `issue_event_changes`, `issue_events` dropped and `issues.agent_prompt` dropped, Open converges and the seeded row survives with its title.
- **`TestReconcileRenamesPromptToAgentPrompt`**: after renaming `agent_prompt` back to `` `prompt` ``, the reconcile renames it forward and the stored prompt body `"the historical prompt body"` survives.
- **`TestReconcileNormalizesLegacyStatusValues`**: rows inserted with `todo`/`in-progress`/`done` come out as `open`/`in_progress`/`closed` and all three rows survive.
- **`TestReconcileNullsEpicStatus`**: an epic row with `status='open'` has `status IS NULL` in the column after reconcile (queried directly), and its title survives.
- **`TestReconcileBackfillsTopicDefault`**: a row inserted with `topic=''` reads back `topic == "misc"`.
- **`TestReconcileResetsLegacyPriorities`**: with the legacy `CHECK (priority >= 0 AND priority <= 4)` installed and a `priority=3` row, after reconcile the row's priority is `0`.
- **`TestReconcileDropsLegacyIssueHistory`**: a partial-shape `issue_history (id VARCHAR(191) PRIMARY KEY, issue_id VARCHAR(191) NOT NULL)` is dropped; `tableExists("issue_history")` is false afterwards; the seeded issue survives.
- **`TestIsLegacyStatusTransition`**: five cases — null→null false; open→open false; null→open true; open→null true; open→closed true.
- **`TestCanonicalEventCanonicalization`**: action NULL→nil, `"   "`→nil, `"  start  "`→`"start"`; reason NULL→`""`, `"  began work  "`→`"began work"`; actor NULL→`"unknown"`, `"   "`→`"unknown"`, `"  alice  "`→`"alice"`.
- **`TestCanonicalLegacyStatus`**: null→null; `open`/`in_progress`/`closed` pass through; `in-progress`→`in_progress`; `todo`→`open`; `done`→`closed`; `weird`→`open`.
- **`TestReconcileTranslatesLegacyIssueHistoryToEvents`**: eight canonical-shape `issue_history` rows produce exactly eight `issue_events` rows with the mapped `action`/`reason`/`actor`/`issue_id`; `created_by`→`actor`; empty-string and NULL actions both land as SQL NULL; whitespace is trimmed. Exactly three `issue_event_changes` rows are produced — `hist-start {status, open, in_progress}`, `hist-close {status, in_progress, closed}`, `hist-legacy-transition {status, open, closed}` (raw `todo`→`done` normalized) — and the five non-transition rows produce none.
- **`TestReconcileTranslateSkipsOrphanedHistoryRows`**: a history row whose `issue_id` does not exist produces zero events; the valid row produces exactly one.
- **`TestReconcileTranslateRunsAfterActorRename`**: with `issue_events` reshaped to the pre-rename `assignee TEXT NOT NULL` layout, translation still lands and `actor == "alice"`.
- **`TestReconcileTranslateIsIdempotentWithExistingEvents`**: a pre-existing `issue_events` row with a colliding id keeps its own `action`/`reason`/`actor` values, is not duplicated, and gains no change row; a genuinely-new row does get its event and its `{status, open, in_progress}` change row.
- **`TestReconcileRecoversFromFabricatedGooseRows`**: with `issue_history` present and `goose_db_version` carrying three fabricated rows at a single tstamp (versions `0`, `1`, and HEAD+1), Open drops `issue_history`, leaves zero rows with `version_id > HEAD`, and preserves the seeded issue.
- **`TestReconcileIsIdempotent`**: a workspace already at v1 converges with the seeded issue untouched.
- **`TestReconcileCreatedTablesMatchBaselineConstraintNames`**: after dropping every canonical table except `meta` and forcing adoption, `information_schema.table_constraints` must contain CHECK constraints named exactly `issues_status_check`, `issues_priority_check`, `issues_type_check`, `relations_type_check`.
- **`TestReconcileTopicHasNoDefault`**: after reconcile re-adds `issues.topic`, `information_schema.columns.column_default` for it must be NULL.
- **`TestReconcileRankBackfillCoexistsWithExistingRanks`**: with one already-ranked row and one `item_rank=''` row, both end non-empty and distinct.
- **`TestPostReconcileBaselineVerificationCatchesNonIssuesGaps`**: dropping `relations.created_by` makes Open fail with an error containing the literal `"relations.created_by"`, and `goose_db_version` must **not** exist afterwards.
- **`TestReconcileErrorMessageIsActionable`**: against `CREATE TABLE issues (id VARCHAR(191) PRIMARY KEY)`, Open's error must contain `"reconcile pre-goose workspace"`, `"status"`, and `"not a known historical shape"`, and must **not** contain `"restore it from a snapshot or recreate"`.
- **`TestDerivedTypeCheckClausesMatchHistoricalLiterals`** pins the generated clause text byte-for-byte:
  - `issueTypeCheckClause == "issue_type IN ('task','feature','bug','chore','epic')"`
  - `containerTypeMembership == "issue_type IN ('epic')"`
  - `canonicalStatusCheckClause == "(issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))"`
  - `priorityCheckClause == "priority >= 0 AND priority <= 1"`

---

## 9. Down-migration schema effects (for completeness)

- v2 Down: `ALTER TABLE issues DROP COLUMN lane` (`00002_add_lane.sql`).
- v3 Down: `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check` then `DROP COLUMN resolution` (`00003_add_resolution.sql`).
- v4 Down: `INSERT IGNORE INTO relations(src_id, dst_id, type, created_at, created_by)` re-materializing one `related-to` edge per non-NULL `redirect_target` with `LEAST/GREATEST` ordering, `created_at = COALESCE(closed_at, updated_at)`, `created_by = 'unknown'` (`00004_add_redirect_target.sql`); then `DROP CONSTRAINT issues_redirect_target_check` and `DROP COLUMN redirect_target`.
- v5 Down: `ALTER TABLE issue_events DROP COLUMN stream_id` then `DROP COLUMN workspace_id` (`00005_add_event_attribution.sql`).
- v1 Down: `DROP TABLE IF EXISTS` for `issue_event_changes`, `issue_events`, `labels`, `comments`, `relations`, `issues`, `meta` — in that FK-safe order (`00001_baseline.sql`).

v4 Up also performs data mutations that change `relations` content: a backfill
`UPDATE issues SET redirect_target = (...)` for rows with
`resolution IN ('duplicate','superseded')` and exactly one incident
`related-to` edge (`00004_add_redirect_target.sql`), followed by
`DELETE r FROM relations r JOIN issues i ON i.redirect_target IS NOT NULL ...
WHERE r.type = 'related-to'`.


---

## The shapemap mechanism

Package `store`. Files: `internal/store/shapemap.go`, `internal/store/shapemap_json.go`, `internal/store/shapemap_known.go`. Tests: `shapemap_test.go`, `shapemap_json_test.go`, `shapemap_fanout_test.go`.

### 1. What a "shape" is: every type and field

#### 1.1 Input data type (defined outside the slice, consumed by it)

`RawDump` — `internal/store/rawdump.go`:
- `WorkspaceID string` json:`workspace_id` (rawdump.go)
- `DoltHead string` json:`dolt_head` (rawdump.go)
- `Tables []RawTable` json:`tables` (rawdump.go)

`RawTable` — `internal/store/rawdump.go`:
- `Name string` json:`name` (rawdump.go)
- `Columns []string` json:`columns` — catalog order (rawdump.go)
- `Rows [][]any` json:`rows` — positional cells; always non-nil, `[]` for an empty table (rawdump.go, initialized at rawdump.go)

Cell Go types as produced by `dumpTable`: driver `[]byte` is normalized to `string` (`internal/store/rawdump.go`); SQL NULL scans as `nil` (rawdump.go comment, scan at rawdump.go).

#### 1.2 Mapping types (shapemap.go)

`ShapeMapping` — shapemap.go:
- `Tables []TableMapping` (shapemap.go)

`TableMapping` — shapemap.go:
- `Table string` (shapemap.go)
- `Emitters []Emitter` (shapemap.go)
- `Drops map[string]Dropped` — keyed by column name (shapemap.go)

`Emitter` — shapemap.go:
- `Collection collection` (shapemap.go)
- `Fields map[string]FieldSource` — keyed by domain field name (shapemap.go)
- `When EmitCondition` (shapemap.go)

`FieldSource interface{ isFieldSource() }` — sealed, package-private method (shapemap.go). Exactly two implementations:
- `FromColumn{ Column string; Transform Transform }` (shapemap.go); `func (FromColumn) isFieldSource() {}` (shapemap.go)
- `Constant{ Value any }` (shapemap.go); `func (Constant) isFieldSource() {}` (shapemap.go)

`EmitCondition interface{ isEmitCondition() }` — sealed (shapemap.go). Exactly two implementations:
- `Always struct{}` (shapemap.go); `func (Always) isEmitCondition() {}` (shapemap.go)
- `WhenChanged{ FieldA string; FieldB string }` (shapemap.go); `func (WhenChanged) isEmitCondition() {}` (shapemap.go)

`Dropped` — shapemap.go:
- `Provenance DropProvenance` (shapemap.go)
- `Reason string` (shapemap.go)

`DropProvenance string` (shapemap.go) with exactly two constants:

| Constant | Literal |
|---|---|
| `DropIntended` | `"intended"` (shapemap.go) |
| `DropUnexplained` | `"unexplained"` (shapemap.go) |

`Transform string` (shapemap.go) with exactly six constants:

| Constant | Literal | shapemap.go line |
|---|---|---|
| `TransformIdentity` | `"identity"` | 152 |
| `TransformLegacyStatus` | `"legacy_status_value"` | 153 |
| `TransformTimestamp` | `"timestamp"` | 154 |
| `TransformEventAction` | `"event_action"` | 159 |
| `TransformEventReason` | `"event_reason"` | 160 |
| `TransformEventActor` | `"event_actor"` | 161 |

`TargetKey string` — the string `"<collection>.<field>"` (shapemap.go).

`collection string` (shapemap.go) with exactly six constants:

| Constant | Literal | line |
|---|---|---|
| `collIssues` | `"issues"` | 170 |
| `collRelations` | `"relations"` | 171 |
| `collComments` | `"comments"` | 172 |
| `collLabels` | `"labels"` | 173 |
| `collEvents` | `"events"` | 174 |
| `collEventChanges` | `"event_changes"` | 175 |

`targetField` (unexported) — shapemap.go:
- `coll collection`, `field string`, `canonical Transform`, `admits map[Transform]bool`, `optional bool`

Package constants `required = false`, `optional = true` (shapemap.go).

`ColumnRef` — shapemap_known.go: `Table string`, `Column string`; `func (c ColumnRef) String() string { return c.Table + "." + c.Column }` (shapemap_known.go).

### 2. The target registry (the closed set of legal mapping targets)

Built once at package init by `buildTargetRegistry()` into `var targetRegistry map[TargetKey]targetField` (shapemap.go). Key is `string(collection)+"."+field` (shapemap.go).

Helper `add(c, canonical, opt, fields...)` sets `admits = {canonical: true}` only (shapemap.go). Helper `addMulti(c, canonical, extra, opt, field)` sets `admits = {canonical} ∪ extra` (shapemap.go).

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

`knownCollections map[collection]bool` is derived from `targetRegistry` values' `coll` (shapemap.go), i.e. exactly the six collections above.

Per-collection required-field sets (what `emitterProblems` enforces at shapemap.go):
- issues: id, title, description, priority, issue_type, created_at, updated_at, closed_at, status
- relations: src_id, dst_id, type, created_by, created_at
- comments: id, issue_id, body, created_by, created_at
- labels: issue_id, name, created_by, created_at
- events: id, issue_id, reason, actor, created_at
- event_changes: event_id, field

### 3. The KNOWN shapes registry (shapemap_known.go)

#### 3.1 `knownSourceColumns` — source column name → TargetKey, verbatim

`var knownSourceColumns = map[string]map[string]TargetKey` (shapemap_known.go). The transform is NOT stored here; it is the target's `canonical` transform read from `targetRegistry` (shapemap_known.go).

Table `"issues"` (shapemap_known.go):

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

Table `"relations"`: `src_id`→`relations.src_id`, `dst_id`→`relations.dst_id`, `type`→`relations.type`, `created_at`→`relations.created_at`, `created_by`→`relations.created_by`.

Table `"comments"`: `id`→`comments.id`, `issue_id`→`comments.issue_id`, `body`→`comments.body`, `created_at`→`comments.created_at`, `created_by`→`comments.created_by`.

Table `"labels"`: `issue_id`→`labels.issue_id`, `label`→`labels.name`, `created_at`→`labels.created_at`, `created_by`→`labels.created_by`. (Note the source name `label` → domain field `name`.)

Table `"issue_events"`:

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

Table `"issue_event_changes"`: `event_id`→`event_changes.event_id`, `field`→`event_changes.field`, `from_value`→`event_changes.from`, `to_value`→`event_changes.to`.

`issue_history` is deliberately absent from this map (shapemap_known.go); it is handled by `issueHistoryFanOut`.

Because `simpleEmitter` reads `tf.canonical`, the effective transform per recognized column is: `timestamp` for `created_at`/`updated_at`/`closed_at`/`archived_at`/`deleted_at` (all tables), `legacy_status_value` for `issues.status`, `identity` for everything else — including `issue_events.action/reason/actor`, whose canonical is `identity` (shapemap.go).

#### 3.2 `bookkeepingTables` — table name → drop reason, verbatim

`var bookkeepingTables = map[string]string` (shapemap_known.go):

| table | reason string |
|---|---|
| `goose_db_version` | `"goose migration registry — schema bookkeeping, no domain field"` |
| `migration_quarantine` | `"migration quarantine ledger — schema bookkeeping, no domain field"` |
| `meta` | `"schema metadata table — no domain field"` |

#### 3.3 `legacyIssueHistoryColumns` (the fan-out gate, defined in schema_reconcile.go)

`[]string{"id", "issue_id", "action", "reason", "from_status", "to_status", "created_at", "created_by"}`.

#### 3.4 `migrationDroppedCols` — scanned from the embedded migration corpus

`var migrationDroppedCols = scanMigrationDrops()` (shapemap_known.go). `scanMigrationDrops` reads every non-directory entry of `migrations.FS` (shapemap_known.go), takes `gooseUpSection(string(data))` of each file (shapemap_known.go), runs `parseDroppedColumns` on it, and maps each `ColumnRef` to the migration **file name** (shapemap_known.go).

Regexes (shapemap_known.go):
- `alterTableRE = (?is)ALTER\s+TABLE\s+`?(\w+)`?([^;]*)` — group 1 = table, group 2 = statement body up to `;` or EOF.
- `dropColumnRE = (?is)DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?`?(\w+)`?` — group 1 = column; matched repeatedly within one ALTER body, so multi-drop statements are fully captured (shapemap_known.go).

`gooseUpSection` (migration_runner.go): case-insensitively finds `-- +goose up`; if absent returns the whole SQL; otherwise slices from that index to the next `-- +goose down` (or to EOF if there is none).

Current corpus content of this map: **empty**. Every `DROP COLUMN` in `internal/store/migrations/` occurs in a `-- +goose Down` section — `00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql` — and `README.md` is not a `.sql` migration but is still read (the loop does not filter by extension, shapemap_known.go); its `ALTER TABLE issues DROP COLUMN priority_band;` lies outside any `-- +goose Up` marker, and `gooseUpSection` returns the whole text when no marker is found, so whether it contributes depends on that file's marker content.

On `fs.ReadDir` or `ReadFile` failure, the package `panic`s: `"scan migration drops: read embedded registry: " + err.Error()` (shapemap_known.go) and `"scan migration drops: read " + entry.Name() + ": " + err.Error()` (shapemap_known.go).

### 4. Deterministic mapping: how a dump becomes a ShapeMapping

`DeterministicMap(dump RawDump) (ShapeMapping, bool)` — shapemap_known.go:
1. For each `dump.Tables` in order, call `mapTable(table)`; on `ok=false` return `(ShapeMapping{}, false)` immediately (shapemap_known.go).
2. Append each `TableMapping` in dump table order (shapemap_known.go).
3. Then `if Validate(dump, out) != nil { return ShapeMapping{}, false }` (shapemap_known.go). No error text escapes; the only signal is the bool.

`mapTable(table RawTable) (TableMapping, bool)` — shapemap_known.go, in this order:
1. If `table.Name` is a key of `bookkeepingTables` → `dropAllColumns(table), true`.
2. If `table.Name == "issue_history"` → `issueHistoryFanOut(table)`.
3. Else look up `knownSourceColumns[table.Name]`; absent → `(TableMapping{}, false)`.
4. Else `simpleEmitter(table, rules)`.

`simpleEmitter(table, rules)` — shapemap_known.go:
- Iterates `table.Columns` in order; any column not in `rules` returns `(TableMapping{}, false)` — the whole table (and via DeterministicMap, the whole dump) declines (72-74).
- `tf := targetRegistry[target]`; `coll = tf.coll` (reassigned per column — last column wins, but all rules for one table point at one collection); `fields[tf.field] = FromColumn{Column: col, Transform: tf.canonical}`.
- Returns one `TableMapping{Table: table.Name, Emitters: []Emitter{{Collection: coll, When: Always{}, Fields: fields}}}` with `Drops` nil (81-84).
- Two source columns aliasing the same target field (e.g. `prompt` and `agent_prompt`) collapse into one map entry; the loser column ends up referenced by nobody and Validate then fails on totality — the decline path pinned by `TestRejectsAmbiguousAlias` (shapemap_test.go).

`dropAllColumns(table)` — shapemap_known.go: for every column, `classifyDrop(table.Name, col)` and store `Dropped{Provenance, Reason}`; returns `TableMapping{Table, Drops}` with no emitters.

`classifyDrop(table, column) (DropProvenance, string)` — shapemap_known.go, in order:
1. `bookkeepingTables[table]` hit → `(DropIntended, <that table's reason string>)`.
2. `migrationDroppedCols[ColumnRef{table, column}]` hit → `(DropIntended, "removed by migration "+file)`.
3. Otherwise → `(DropUnexplained, "")`.

`issueHistoryFanOut(table)` — shapemap_known.go. See §7.

### 5. Validate: the algorithm and every error message

`Validate(dump RawDump, m ShapeMapping) error` — shapemap.go. Order of checks (first failure returns; only within the "problems" phase are faults aggregated):

**Step 0 — indexing.**
- `indexDumpTables(dump)` (shapemap.go): builds `map[string]RawTable`; a repeated table name returns
  `dump lists table %q more than once` (shapemap.go).
- `indexMapping(m)` (shapemap.go): builds `map[string]TableMapping`; a repeated `tm.Table` returns
  `mapping dispositions table %q more than once` (shapemap.go).

**Step 1 — totality** (shapemap.go). For every dump table, `referenced := referencedColumns(tm)` (the set of `FromColumn.Column` over all emitters of that table's mapping, shapemap.go). For each `col` of `table.Columns`, if it is neither referenced nor a key of `tm.Drops`, append `"<table>.<col>"` to `unaccounted`. A dump table with no mapping at all yields all its columns here. If non-empty: sort ascending, then return

```
mapping is not total: %d source column(s) unaccounted for: %s
```
with `%d` = count and `%s` = the sorted names joined by `", "` (shapemap.go).

**Step 2 — aggregated problems** (shapemap.go). Collected, sorted ascending, joined with `"; "`, returned as

```
mapping is malformed: %s
```
(shapemap.go).

Problem sources:

a) A mapping table absent from the dump (shapemap.go):
```
table %q: mapping references a table the dump does not have
```

b) Per dump table that has a mapping, `tableProblems(table, tm)` (shapemap.go):
- For each drop column not in the table's columns:
  `table %q: drop names column %q the dump does not have` (shapemap.go)
- For each drop column also referenced by an emitter:
  `table %q column %q is both mapped and dropped` (shapemap.go)
- For each drop whose `Provenance` is neither `DropIntended` nor `DropUnexplained`:
  `table %q column %q: unknown drop provenance %q` (shapemap.go)

c) Per emitter, `emitterProblems(tableName, cols, em)` (shapemap.go):
- Unknown collection (`!knownCollections[em.Collection]`) — reports and **returns early**, skipping all other emitter checks (shapemap.go):
  `table %q: emitter into unknown collection %q`
- For each `field → src` in `em.Fields`, key `= string(collection)+"."+field`; if not in `targetRegistry` (shapemap.go), and `continue`:
  `table %q: emitter into %q targets unknown field %q`
- `FromColumn` case (shapemap.go): if `s.Column` not among the dump table's columns:
  `table %q: %q.%q maps from column %q the dump does not have`
  ; if `!tf.admits[s.Transform]`:
  `table %q: %q.%q does not admit transform %q`
  (both can fire for the same field)
- `Constant` case (shapemap.go): if `tf.canonical != TransformIdentity`:
  `table %q: %q.%q is not a passthrough field; a constant cannot land here`
  ; if `s.Value` is not a `string`:
  `table %q: %q.%q constant must be a string, got %T`
- Default case (a third FieldSource implementation, unreachable from outside the package) (shapemap.go):
  `table %q: %q.%q has unknown field source %T`
- Required coverage (shapemap.go): for every registry entry whose `coll == em.Collection` and `!optional`, if `em.Fields[tf.field]` is absent:
  `table %q: emitter into %q does not cover required field %q` — the `%q` is the full **TargetKey** (e.g. `"comments.body"`), not the bare field name.
- `WhenChanged` condition (shapemap.go): for each of `FieldA`, `FieldB` not present in `em.Fields`:
  `table %q: emitter condition references field %q the emitter does not produce`
  (`Always` and any other condition value are unchecked.)

**Step 3 — row arity** (shapemap.go), iterating `dump.Tables` in order and rows by index; first mismatch returns:
```
table %q row %d has %d cells, want %d (one per column)
```
This runs after the totality and problem phases, so a shape fault masks an arity fault.

`tablesByName(m)` (shapemap.go) is the post-Validate, error-free index (last-wins on duplicates).

### 6. Apply: the fold, and per-SQL-type coercion

`Apply(dump RawDump, m ShapeMapping) (model.Export, error)` — shapemap.go:
1. Calls `Validate(dump, m)` itself; on error returns `model.Export{}, err` (shapemap.go).
2. `mapTables := tablesByName(m)`; `records := map[collection][]map[string]any{}` (shapemap.go).
3. Iterates `dump.Tables` in order → `colIndex := rowColumnIndex(table)` (name → positional index, shapemap.go) → each row in order → **each emitter of that table in mapping order**: `buildRecord`, then `if emits(em.When, rec)` append to `records[em.Collection]` (shapemap.go). Record order is therefore table order, then row order, then emitter order.
4. Any `buildRecord` error is wrapped: `table %q: %w` (shapemap.go).
5. `assembleExport(dump.WorkspaceID, records)` (shapemap.go). Note: `DoltHead` is not carried into the Export.

`buildRecord(em, tableName, colIndex, row)` — shapemap.go: for each field,
- `FromColumn` → `applyTransform(s.Transform, row[colIndex[s.Column]])`; error wrapped `column %q: %w` (shapemap.go). Combined with Apply's wrapper the surfaced text is e.g. `table "issues": column "created_at": invalid timestamp "not-a-timestamp"`.
- `Constant` → `rec[field] = s.Value` verbatim (shapemap.go).
- A third `FieldSource` type silently produces no entry (no default arm, shapemap.go).
- `tableName` parameter is unused in the body.

`emits(when, rec)` — shapemap.go:
- `WhenChanged` → `cellsDiffer(rec[FieldA], rec[FieldB])`.
- **Every other value, including `Always` and `nil`** → `true` (the `default` arm, shapemap.go).

`cellsDiffer(a, b any) bool` — shapemap.go:
- If exactly one of them is `nil` → `true`.
- If both `nil` → `false`.
- Else compare `cellString(a) != cellString(b)`.

#### 6.1 Transform semantics — `applyTransform(t Transform, cell any) (any, error)` (shapemap.go)

| Transform | NULL (`nil`) input | `string` input | other Go type input |
|---|---|---|---|
| `identity` | returns `nil` | returns the value unchanged | returns the value unchanged (shapemap.go) |
| `timestamp` | returns `nil, nil` (shapemap.go) | `parseTimestamp` → `time.Time`, or error `invalid timestamp %q` (shapemap.go) | error: `%s requires a string cell, got %T` with `%s` = `"timestamp"` (shapemap.go) |
| `legacy_status_value` | `cellNullString` → invalid → `canonicalLegacyStatus` keeps invalid → `nullableSQLString` → `nil` | canonicalized string (table below) | error `legacy_status_value: expected a string or NULL cell, got %T` (shapemap.go) |
| `event_action` | `canonicalEventAction(invalid)` → `nil` | `strings.TrimSpace`; empty after trim → `nil`; else trimmed string | error `event_action: expected a string or NULL cell, got %T` |
| `event_reason` | → `""` | `strings.TrimSpace(v)` | error `event_reason: expected a string or NULL cell, got %T` |
| `event_actor` | → `"unknown"` | `strings.TrimSpace`; empty after trim → `"unknown"`; else trimmed | error `event_actor: expected a string or NULL cell, got %T` |
| any other value | — | — | error `unknown transform %q` (shapemap.go) |

`cellNullString` (shapemap.go): `nil` → `sql.NullString{}`; `string` → `{Valid:true, String:v}`; anything else → error `expected a string or NULL cell, got %T`.

`parseTimestamp(s)` (shapemap.go): tries `time.RFC3339Nano` then `time.RFC3339`; on both failing returns `time.Time{}, fmt.Errorf("invalid timestamp %q", s)`.

`canonicalLegacyStatus` (schema_reconcile.go), exact-match switch:

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

`canonicalEventAction` (schema_reconcile.go) returns `any` — `nil` or trimmed string. `canonicalEventReason` returns `string`. `canonicalEventActor` returns `string`, fallback `"unknown"`. `nullableSQLString`: invalid → `nil`, valid → the string.

#### 6.2 Cell readers

`cellString(cell any) string` (shapemap.go): `nil` → `""`; `string` → itself; anything else → `fmt.Sprint(v)`. A missing map key yields `nil` → `""`.

`cellInt(cell any) int` (shapemap.go): `nil`→0; `int`/`int32`/`int64`/`uint64`/`float64` → converted via `int(v)` (float truncates); `string` → `strconv.Atoi(strings.TrimSpace(v))` with the error discarded (so unparseable → 0); any other type → 0.

`cellTime(cell any) time.Time` (shapemap.go): `time.Time` → itself; anything else (including `nil` and a string) → `time.Time{}` zero value.

`cellTimePtr(cell any) *time.Time` (shapemap.go): `time.Time` → pointer to it; anything else → `nil`.

#### 6.3 `assembleExport` — the exact model.Export produced

shapemap.go. Starts from:
```go
model.Export{Version: 2, WorkspaceID: workspaceID,
  Issues: []model.Issue{}, Relations: []model.Relation{}, Comments: []model.Comment{},
  Labels: []model.Label{}, Events: []model.IssueEvent{}}
```
(shapemap.go) — `Version` is hardcoded `2`; all slices non-nil.

- **Issues** (596-602): `buildIssue(rec)` per record; error propagated as `model.Export{}, err`.
- **Relations** (603-611): `model.Relation{SrcID: cellString(rec["src_id"]), DstID: cellString(rec["dst_id"]), Type: model.RelationType(cellString(rec["type"])), CreatedAt: cellTime(rec["created_at"]), CreatedBy: cellString(rec["created_by"])}` — the `type` value is cast without validation.
- **Comments** (612-620): `model.Comment{ID, IssueID, Body, CreatedAt: cellTime, CreatedBy}` from `rec["id"]`, `rec["issue_id"]`, `rec["body"]`, `rec["created_at"]`, `rec["created_by"]`.
- **Labels** (621-628): `model.Label{IssueID, Name: cellString(rec["name"]), CreatedAt: cellTime, CreatedBy}`.
- **Events** (632-650): `model.IssueEvent{ID: cellString(rec["id"]), IssueID, Action: cellString(rec["action"]), Reason, Actor, CreatedAt: cellTime(rec["created_at"]), Attribution: model.NewAttribution(cellString(rec["stream"]), cellString(rec["workspace"])), Changes: []model.FieldChange{}}`. An index `byID[ev.ID] = len(events)` is built (shapemap.go) — **last event with a duplicate id wins the index**.
- **Event changes** (651-662): for each record, `eventID := cellString(rec["event_id"])`; if `byID` has no such id, `assembleExport` returns
  ```
  event change references unknown event_id %q
  ```
  (shapemap.go). Otherwise appends `model.FieldChange{Field: cellString(rec["field"]), From: cellString(rec["from"]), To: cellString(rec["to"])}` to that event's `Changes` (shapemap.go). Note NULL `from`/`to` become `""` here even though the transform preserved NULL.

`buildIssue(rec)` — shapemap.go:
- `model.Issue{ID: cellString(rec["id"]), Title, Description, Prompt: cellString(rec["prompt"]), Priority: model.Priority(cellInt(rec["priority"])), IssueType: model.IssueType(cellString(rec["issue_type"])), Topic, Assignee, Rank: cellString(rec["rank"]), CreatedAt: cellTime(rec["created_at"]), UpdatedAt: cellTime(rec["updated_at"])}` (shapemap.go). **`rec["lane"]` is never read**, despite `issues.lane` being a registered target and `lane` being in `knownSourceColumns`.
- `issue.SetRetention(model.RetentionFromTimestamps(cellTimePtr(rec["archived_at"]), cellTimePtr(rec["deleted_at"])))` (shapemap.go).
- `view := model.StatusView{}`; only if `!issue.IssueType.IsContainer()` (shapemap.go) is it populated:
  - `view.Value = model.DefaultOpen(cellString(rec["status"]))`
  - `view.ClosedAt = cellTimePtr(rec["closed_at"])`
  - if `cellString(rec["resolution"]) != ""`, `view.Resolution = &model.Resolution(raw)`
  - if `cellString(rec["redirect_target"]) != ""`, `view.RedirectTarget = &raw`
- Returns `model.HydrateRow(issue, view, nil)` (shapemap.go) — its error is propagated up through `assembleExport`.

### 7. Fan-out: `issue_history` → events + conditional event_changes

`issueHistoryFanOut(table RawTable) (TableMapping, bool)` — shapemap_known.go.

**Gate** (114-121): build the column set; for each of `legacyIssueHistoryColumns` (`id, issue_id, action, reason, from_status, to_status, created_at, created_by`), if missing → `(TableMapping{}, false)`. Presence-only: extra columns pass this gate but then fall out as unaccounted at Validate (comment at shapemap_known.go), so a strict superset makes `DeterministicMap` return `ok=false`.

**Expansion rule** — one `TableMapping{Table: "issue_history"}` with `Drops` nil and exactly two emitters (shapemap_known.go):

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

`TestFanOutConservesIssueHistoryAgainstReconcile` (shapemap_fanout_test.go):
- Seeds a Dolt workspace, inserts 8 legacy `issue_history` rows with these `(id, action, reason, from_status, to_status, created_at, created_by)`:
  - `hist-start`, `"start"`, `"began work"`, `"open"`→`"in_progress"`, `2026-01-01T10:00:00Z`, `alice`
  - `hist-comment-null`, action `nil`, `"added context"`, `nil`→`nil`, `10:05:00Z`, `alice`
  - `hist-comment-empty`, action `""`, `"added more context"`, `nil`→`nil`, `10:06:00Z`, `alice`
  - `hist-close`, `"close"`, `"shipped"`, `"in_progress"`→`"closed"`, `11:00:00Z`, `bob`
  - `hist-same-status`, `"touch"`, `"no movement"`, `"closed"`→`"closed"`, `11:30:00Z`, `bob`
  - `hist-whitespace`, `"  start  "`, `"  padded reason  "`, `nil`→`nil`, `12:00:00Z`, `"  carol  "`
  - `hist-legacy-transition`, `"close"`, `"legacy close"`, `"todo"`→`"done"`, `12:30:00Z`, `carol` (inserted after dropping `issues_status_check`)
  - `hist-legacy-nontransition`, `"touch"`, `"spelling differs only"`, `"in-progress"`→`"in_progress"`, `12:45:00Z`, `carol`
- Dumps below the migration gate (`DumpRaw`), extracts only the `issue_history` table into a single-table `RawDump`, and asserts `DeterministicMap` accepts it.
- Applies, then compares against the oracle: the in-place forward migration's `translateIssueHistoryToEvents` output read via `store.Export`.
- Asserts the oracle produced exactly **8** events whose ids start with `hist-`, and `reflect.DeepEqual` of the two normalized maps.
- `normEvent` normalization compares `IssueID, Action, Reason, Actor, CreatedAt` (as `ev.CreatedAt.UTC().Format(time.RFC3339Nano)`) and `Changes` sorted by `(Field, From, To)`; event `ID` is the map key. Attribution is deliberately excluded.

### 8. JSON wire form (shapemap_json.go)

#### 8.1 Wire discriminator constants

`fieldSourceKind string` (shapemap_json.go): `sourceColumn = "column"`, `sourceConst = "const"`.
`whenKind string` (shapemap_json.go): `whenAlways = "always"`, `whenChanged = "changed"`.

#### 8.2 Wire structs — exact key names, types, omitempty

`fieldWire` (shapemap_json.go):
- `field` string (no omitempty)
- `source` string (`fieldSourceKind`; no omitempty)
- `column` string, `omitempty`
- `transform` string (`Transform`), `omitempty`
- `value` string, `omitempty`

`whenWire`: `kind` string (no omitempty), `fieldA` string `omitempty`, `fieldB` string `omitempty`.

`emitterWire`: `collection` string (no omitempty), `when` object (no omitempty), `fields` array of `fieldWire` (no omitempty → encodes as `null` when the emitter has no fields).

`dropWire`: `column` string, `provenance` string (`DropProvenance`, no omitempty), `reason` string `omitempty`.

`tableWire`: `table` string (no omitempty), `emitters` array `omitempty`, `drops` array `omitempty`.

`mappingWire`: `tables` array (no omitempty).

Concrete document shape:
```json
{"tables":[{"table":"issues",
  "emitters":[{"collection":"issues","when":{"kind":"always"},
    "fields":[{"field":"closed_at","source":"column","column":"closed_at","transform":"timestamp"},
              {"field":"field","source":"const","value":"status"}]}],
  "drops":[{"column":"obsolete","provenance":"unexplained","reason":"no target in baseline"}]}]}
```

#### 8.3 `MarshalJSON` — value receiver on `ShapeMapping` (shapemap_json.go)

- `wire.Tables` is `make([]tableWire, 0, len(m.Tables))`, so an empty mapping encodes as `{"tables":[]}`, never `null`.
- Per table: emitters encoded in slice order then sorted; drops enumerated from the map then sorted by `Column` ascending (shapemap_json.go).
- Tables sorted by `Table` ascending (shapemap_json.go).
- Emitters sorted by `emitterSortKey` (shapemap_json.go), which is `collection \0 when.kind \0 when.fieldA \0 when.fieldB` then, per already-name-sorted field, `\x01 field \0 source \0 column \0 transform \0 value` (shapemap_json.go).
- Fields within an emitter sorted by `Field` ascending (shapemap_json.go).
- `FromColumn` → `source:"column"`, `column`, `transform` (shapemap_json.go). `Constant` → `source:"const"`, `value`. A `Constant` whose `Value` is not a `string`:
  ```
  shapemapping: table %q field %q constant must be a string, got %T
  ```
  (shapemap_json.go). A third `FieldSource` type:
  ```
  shapemapping: table %q field %q has unencodable source %T
  ```
  (shapemap_json.go).
- `Always` → `{"kind":"always"}`; `WhenChanged` → `{"kind":"changed","fieldA":…,"fieldB":…}` (fieldA/fieldB omitted when empty). Any other `EmitCondition` — including a **nil** `When`:
  ```
  shapemapping: table %q emitter has unencodable condition %T
  ```
  (shapemap_json.go).

#### 8.4 `UnmarshalJSON` — pointer receiver (shapemap_json.go)

Rules, in order:
1. Decoder with `DisallowUnknownFields()` (shapemap_json.go); any unrecognized JSON key anywhere in the document is a decode error from `encoding/json` (message contains `unknown field`).
2. Trailing-data guard (shapemap_json.go): a second `dec.Decode` must yield `io.EOF`. If it returns `nil` (another value present):
   ```
   shapemapping: unexpected trailing data after the mapping document
   ```
   If it returns any other error (unparseable junk):
   ```
   shapemapping: malformed trailing data after the mapping document: %w
   ```
3. Duplicate table (shapemap_json.go):
   ```
   shapemapping: duplicate disposition for table %q
   ```
4. `tableFromWire`: `Drops` map is only allocated when `len(tw.Drops) > 0` (so a table with no drops decodes with `Drops == nil`); a repeated drop column:
   ```
   shapemapping: table %q drops column %q more than once
   ```
   Drop `provenance` and `reason` are copied verbatim with no validation here.
5. `emitterFromWire`: `Collection` is `collection(ew.Collection)` verbatim, unvalidated. Condition kind switch: `"always"` → `Always{}`, `"changed"` → `WhenChanged{FieldA, FieldB}`, anything else (including empty):
   ```
   shapemapping: table %q emitter has unknown condition kind %q (want %q or %q)
   ```
   with the last two `%q` rendering `always` and `changed` (shapemap_json.go).
6. Duplicate field within an emitter (shapemap_json.go):
   ```
   shapemapping: table %q emitter into %q assigns field %q more than once
   ```
7. Source kind switch (270-278): `"column"` → `FromColumn{Column: fw.Column, Transform: fw.Transform}` (transform string copied verbatim, unvalidated); `"const"` → `Constant{Value: fw.Value}` (always a Go `string`); anything else:
   ```
   shapemapping: table %q field %q has unknown source %q (want %q or %q)
   ```
   with the last two rendering `column` and `const`.
8. `m.Tables = tables` where `tables` is `make([]TableMapping, 0, len(wire.Tables))` — a `{"tables":[]}` document yields a non-nil empty slice.

Explicitly NOT checked at decode (documented at shapemap_json.go and observable in the code): totality, target resolution, transform admissibility, collection validity, required coverage, `WhenChanged` field existence, drop provenance validity. All of those are Validate's.

#### 8.5 What `shapemap_json_test.go` pins

- `TestShapeMappingJSONRoundTrip`: `Marshal → Unmarshal → Marshal` is byte-identical for a 6-table mapping plus a drop `{"obsolete": {Provenance: DropUnexplained, Reason: "no target in baseline"}}`.
- `TestShapeMappingJSONStableOrder`: two independent `Marshal` calls on the same mapping produce identical bytes.
- `TestShapeMappingJSONStableOrderMultiEmitter`: two emitters into `event_changes` sharing field names `event_id`/`field` but differing in source (`col("id")` vs `col("other")`, `Constant{"status"}` vs `Constant{"priority"}`) and condition (`Always{}` vs `WhenChanged{FieldA:"field", FieldB:"event_id"}`) encode identically regardless of in-memory order.
- `TestShapeMappingJSONRejectsUnknownSource`: input `{"tables":[{"table":"issues","emitters":[{"collection":"issues","when":{"kind":"always"},"fields":[{"field":"id","source":"transmute","column":"id"}]}]}]}` errors containing `unknown source`.
- `TestShapeMappingJSONRejectsUnknownConditionKind`: `"when":{"kind":"sometimes"}` errors containing `unknown condition kind`.
- `TestShapeMappingJSONRejectsDuplicateField`: two `{"field":"id"...}` entries error containing `more than once`.
- `TestShapeMappingJSONRejectsDuplicateTable`: `{"tables":[{"table":"issues","emitters":[]},{"table":"issues","emitters":[]}]}` errors containing `duplicate`.
- `TestShapeMappingJSONRejectsUnknownField`: `{"tables":[{"table":"issues","drops":[{"column":"x","prov":"unexplained"}]}]}` errors containing `unknown field`.
- `TestShapeMappingJSONRejectsTrailingData`: `{"tables":[]}{"tables":[]}` is rejected by `json.Unmarshal` and, when handed directly to `UnmarshalJSON`, errors containing `trailing data`; `{"tables":[]}@@@nonsense` errors containing both `trailing data` **and** `invalid character`.
- `TestDecodedMappingRecoversNovelAheadShape`: `DeterministicMap` must **decline** `novelAheadDump()` (an `issues` table whose `title` column is named `summary`, shapemap_json_test.go); the operator mapping (`aheadColumnMapping()`, mapping `title ← summary`) marshaled and re-decoded, then passed to `Recover(ctx, canonicalUnder(t), dump, staticMapper(mapping), 1)`, yields a `Reconciled` outcome whose candidate store is Doctor-clean, exports exactly 2 issues, and preserves titles `"Open task"` and `"Done task"`.

Note `aheadColumnMapping()` uses `identity` on `issue_events.action/reason/actor` and on `event_changes.from/to`, and `legacy_status_value` on `issues.status` — all within the registry's `admits` sets.

### 9. What `shapemap_test.go` pins

- `TestValidateRejectsIncompleteMapping`: dump `issues(id,title)` with an emitter mapping only `id` → error text contains both `title` and `unaccounted`.
- `TestValidateRejectsStaleAndMalformedKeys`, dump `issues(id)`, four sub-cases and the substring each error must contain:
  | case | mapping fault | required substring |
  |---|---|---|
  | source column the dump lacks | `title ← column "ghost"` | `does not have` |
  | unknown target field | field name `nope` | `unknown field` |
  | unknown collection | `Collection: "ghosts"` | `unknown collection` |
  | unknown drop provenance | `Drops{"id": {Provenance:"guessed", Reason:"x"}}` | `unknown drop provenance` |
- `TestRejectsAmbiguousAlias`: an `issues` table carrying **both** `prompt` and `agent_prompt` — `DeterministicMap` must return `ok=false`; and a hand-built mapping covering only `agent_prompt` must fail `Validate` with `unaccounted`.
- `TestDeterministicMapDeclinesThinNonIssuesTable`: complete `issues` + `comments(issue_id)` only → `DeterministicMap` declines; the equivalent hand-built mapping fails `Validate` with `does not cover required field`.
- `fullIssuesMapping` helper (173-179) routes through `simpleEmitter(table, knownSourceColumns["issues"])` and panics `"fullIssuesMapping: issues table not recognized"` if it declines.
- `TestDeterministicMapDeclinesThinRequiredTargets`: declines `issues` missing `id`/`title` (columns `description,status,priority,issue_type,closed_at,created_at,updated_at`) and `issue_events` missing `reason`/`actor` (columns `id,issue_id,action,created_at`).
- `TestValidateRejectsRowArityMismatch`: 9 columns, an 8-cell row → error contains `cells, want`.
- `TestClassifyDropDistinguishesProvenance`: `classifyDrop("goose_db_version","version_id")` → `DropIntended` with a non-empty reason; `classifyDrop("issues","a_column_no_migration_ever_made")` → `DropUnexplained`.
- `TestParseDroppedColumnsReadsMigrationHistory`: from `"ALTER TABLE issues DROP COLUMN legacy_field;\nALTER TABLE \`relations\` DROP COLUMN IF EXISTS \`old_col\`;\nALTER TABLE issues DROP COLUMN a, DROP COLUMN b;"` the results must include `{issues,legacy_field}`, `{relations,old_col}`, `{issues,a}`, `{issues,b}`.
- `TestGooseUpSectionExcludesDownDrops`: an Up-`ADD`/Down-`DROP` migration yields zero refs; an Up-`DROP legacy`/Down-`ADD` migration yields exactly `[{issues, legacy}]`.
- `TestDeterministicMapCleanAhead`: a real v1 workspace (epic + child task + solo task, one comment, one label, one `Start` transition) is dumped with `DumpRaw`, mapped, validated, applied, and `ReplaceFromExport`ed into a fresh store; `Doctor` must report `IntegrityCheck == "ok"` with zero errors (`mustClean`, 289-294), every original issue id must survive, and the epic's status must round-trip as `""` (NULL), i.e. `epic.StatusValue() == ""`.
- `TestDeterministicMapPreGoose`: a hand-built pre-goose dump — `issues` with `prompt` (not `agent_prompt`), no `topic`/`item_rank`/`lane`/`resolution`/`redirect_target`/`archived_at`/`deleted_at`; `labels` with column `label`; `issue_events` with `assignee` (not `actor`); statuses `"todo"`/`"done"`; no `goose_db_version` table — is accepted by `DeterministicMap`, validates, and applies to: 2 issues; exactly 1 event with exactly 1 nested change; `export.Events[0].Actor == "claude"` (proving `issue_events.assignee → events.actor`). After `ReplaceFromExport`: Doctor-clean; `i1.Prompt == "the historical prompt"`; `i1.StatusValue() == "open"` (from `"todo"`); `i1.Topic == "misc"` (defaulted by the write path, not by the mapper); `i2.StatusValue() == "closed"` (from `"done"`).
- `TestApplyTransformPreservesNull`: `applyTransform(tf, nil)` returns `(nil, nil)` for `identity`, `legacy_status_value`, and `timestamp`; `applyTransform(TransformLegacyStatus, "todo")` returns `"open"`.
- `TestApplyRejectsCorruptTimestamp`: an `issues` row with `created_at = "not-a-timestamp"` makes `Apply` fail with an error containing all of `issues`, `created_at`, and `not-a-timestamp`.

### 10. Complete error-message inventory

Produced in `shapemap.go`:
1. `dump lists table %q more than once`
2. `mapping dispositions table %q more than once`
3. `mapping is not total: %d source column(s) unaccounted for: %s`
4. `mapping is malformed: %s` — wrapping any of:
   - `table %q: mapping references a table the dump does not have`
   - `table %q: drop names column %q the dump does not have`
   - `table %q column %q is both mapped and dropped`
   - `table %q column %q: unknown drop provenance %q`
   - `table %q: emitter into unknown collection %q`
   - `table %q: emitter into %q targets unknown field %q`
   - `table %q: %q.%q maps from column %q the dump does not have`
   - `table %q: %q.%q does not admit transform %q`
   - `table %q: %q.%q is not a passthrough field; a constant cannot land here`
   - `table %q: %q.%q constant must be a string, got %T`
   - `table %q: %q.%q has unknown field source %T`
   - `table %q: emitter into %q does not cover required field %q`
   - `table %q: emitter condition references field %q the emitter does not produce`
5. `table %q row %d has %d cells, want %d (one per column)`
6. `table %q: %w` (Apply's per-table wrapper, 516)
7. `column %q: %w` (buildRecord's per-column wrapper, 545)
8. `%s requires a string cell, got %T`
9. `%s: %w` for the four canonicalizing transforms (738, 745, 751, 757)
10. `unknown transform %q`
11. `expected a string or NULL cell, got %T`
12. `invalid timestamp %q`
13. `event change references unknown event_id %q`

Produced in `shapemap_json.go`:
14. `shapemapping: table %q emitter has unencodable condition %T`
15. `shapemapping: table %q field %q constant must be a string, got %T`
16. `shapemapping: table %q field %q has unencodable source %T`
17. `shapemapping: unexpected trailing data after the mapping document`
18. `shapemapping: malformed trailing data after the mapping document: %w`
19. `shapemapping: duplicate disposition for table %q`
20. `shapemapping: table %q drops column %q more than once`
21. `shapemapping: table %q emitter has unknown condition kind %q (want %q or %q)`
22. `shapemapping: table %q emitter into %q assigns field %q more than once`
23. `shapemapping: table %q field %q has unknown source %q (want %q or %q)`

Panics in `shapemap_known.go`:
24. `scan migration drops: read embedded registry: <err>`
25. `scan migration drops: read <file>: <err>`

`DeterministicMap` itself produces no error text — it returns `(ShapeMapping{}, false)` (shapemap_known.go).

### 11. Behavioral edges observable in the code

- `issues.lane` is a registered target and `lane` is in `knownSourceColumns["issues"]` (shapemap_known.go, shapemap.go), but `buildIssue` never reads `rec["lane"]` (shapemap.go): the value satisfies totality and is then discarded at assembly.
- `event_changes.from`/`.to` are `optional` targets (shapemap.go), so an `event_changes` emitter may omit them; `cellString(nil)` then yields `""` in the `model.FieldChange` (shapemap.go).
- `emits` returns `true` for a `nil` `When` (shapemap.go), and `Validate` never requires `When` to be non-nil; only `MarshalJSON` rejects it (shapemap_json.go).
- `Constant` is accepted by `Validate` on any field whose `canonical` is `identity` (shapemap.go) — including optional ones — but `MarshalJSON`/decode restrict the value to `string`.
- A `Constant` value passes through `buildRecord` untransformed (shapemap.go), then through `cellString`/`cellInt`/`cellTime` at assembly; a constant on a `timestamp`-canonical field is rejected by `Validate` (shapemap.go).
- `simpleEmitter` reassigns `coll` on every column (shapemap_known.go); an empty-column table would produce an emitter with `Collection: ""`, which `Validate` rejects as an unknown collection.
- `Apply` re-runs `Validate` internally (shapemap.go), so callers that already validated pay the cost twice; `DeterministicMap` also validates (shapemap_known.go).


---

## Issue IDs, Labels, Relations, Ranking — raw behavioral inventory

Scope: `internal/store/issue_ids.go`, `internal/store/labels.go`, `internal/store/relations.go`, `internal/store/ranking.go`, plus the collaborators those files bind to (`internal/issueid`, `internal/rank`, `internal/model/label.go`, `internal/model/relation_type.go`, `internal/store/row_deletes.go`, schema in `internal/store/migrations/00001_baseline.sql`) and the named tests. Every claim cites file:line.

---

## 1. Schema facts these subsystems write to

- `issues.item_rank TEXT NOT NULL DEFAULT ''` — `internal/store/migrations/00001_baseline.sql`.
- `issues.id VARCHAR(191) PRIMARY KEY` — `internal/store/migrations/00001_baseline.sql`.
- `relations` table: `src_id VARCHAR(191) NOT NULL`, `dst_id VARCHAR(191) NOT NULL`, `type VARCHAR(32) NOT NULL`, `created_at VARCHAR(64) NOT NULL`, `created_by TEXT NOT NULL`, `PRIMARY KEY (src_id, dst_id, type)`, `FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE`, `FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE`, `CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'))` — `internal/store/migrations/00001_baseline.sql`.
- `labels` table: `issue_id VARCHAR(191) NOT NULL`, `label VARCHAR(191) NOT NULL`, `created_at VARCHAR(64) NOT NULL`, `created_by TEXT NOT NULL`, `PRIMARY KEY (issue_id, label)`, `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE` — `internal/store/migrations/00001_baseline.sql`.
- Indexes: `CREATE INDEX idx_issues_rank ON issues(item_rank(191));`, `CREATE INDEX idx_relations_src_type ON relations(src_id, type);`, `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);`, `CREATE INDEX idx_labels_issue ON labels(issue_id, label);`, `CREATE INDEX idx_labels_name ON labels(label, issue_id);`.
- Down section drops `labels` and `relations`.
- All timestamps written by these subsystems use `time.RFC3339Nano` and are read back through `scanTime` = `time.Parse(time.RFC3339Nano, value)` — `internal/store/store.go`.

---

## 2. ISSUE IDs

### 2.1 Constants (literal values)

`internal/issueid/generate.go`:
- `CollisionProbabilityThreshold = 0.25`
- `MinHashLength = 3`
- `MaxHashLength = 8`
- `NonceAttempts = 10`
- `Base36Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"`

`internal/issueid/slug.go`:
- `PrefixMinLength = 3`
- `PrefixMaxLength = 12`
- `TopicMinLength = 3`
- `TopicMaxLength = 30`

### 2.2 ID grammar

Top-level ID is built by `fmt.Sprintf("%s-%s-%s", prefix, topic, shortHash)` — `internal/issueid/generate.go`. So: `<prefix>-<topic>-<hash>` where prefix and topic are normalized slugs and hash is `length` base-36 lowercase characters (`Base36Alphabet`, `internal/issueid/generate.go`, used).

Child ID is `fmt.Sprintf("%s.%d", parentID, maxChildNumber+1)` — `internal/store/issue_ids.go`. Children are therefore dotted decimal suffixes appended to the full parent ID, and grandchildren nest (`parent.1.2`) because the child of a child re-applies the same rule at `internal/store/issue_ids.go`.

"Top-level" is defined in SQL as an id with no dot: `SELECT COUNT(*) FROM issues WHERE id NOT LIKE ?` with argument `"%.%"` — `internal/store/issue_ids.go`.

The test locking the shape: `GenerateHashID("proj","storage"...)` must start with `"proj-storage-"` and the remainder must have exactly `length` characters, checked for lengths `MinHashLength`, `5`, `MaxHashLength` — `internal/issueid/generate_test.go`.

### 2.3 Slug normalization (prefix and topic)

`NormalizeSlug` — `internal/issueid/slug.go`:
- Input is `strings.TrimSpace` then `strings.ToLower`d before iteration.
- Runes in `a-z` or `0-9` are kept verbatim.
- Any other rune becomes a single `-`, and consecutive non-alphanumerics collapse to one `-` (`previousDash` guard).
- Leading/trailing `-` are trimmed: `strings.Trim(builder.String(), "-")`.

`NormalizeConfiguredPrefix` — `internal/issueid/slug.go`:
- Normalizes via `NormalizeSlug`.
- Empty result → error `"issue prefix is required"`.
- Longer than `PrefixMaxLength` (12) → truncated to 12 bytes then re-trimmed of `-` — no error.
- Shorter than `PrefixMinLength` (3) after normalization → error `fmt.Errorf("issue prefix must be at least %d characters after normalization", PrefixMinLength)`, i.e. `"issue prefix must be at least 3 characters after normalization"`.

`NormalizeTopicForCreate` — `internal/issueid/slug.go`:
- Empty result → `"topic is required"`.
- Shorter than 3 → `"topic must be at least 3 characters after normalization"`.
- Longer than 30 → `"topic must be at most 30 characters after normalization"`. Note the asymmetry with prefix: topic over-length is an error, prefix over-length is silently truncated.

Callers at creation: topic normalized before the transaction (`internal/store/store.go`), prefix normalized inside the transaction with error wrapped as `fmt.Errorf("normalize issue prefix: %w", err)` (`internal/store/store.go`).

### 2.4 Hash minting

`GenerateHashID(prefix, topic, title, description, creator, createdAt, length, nonce)` — `internal/issueid/generate.go`:
- Content string: `fmt.Sprintf("%s|%s|%s|%s|%d|%d", topic, title, description, creator, createdAt.UnixNano(), nonce)`. Note the prefix is NOT part of the hashed content — only of the rendered ID.
- `sha256.Sum256` of that content.
- Takes the first `hashBytesForLength(length)` bytes and base-36 encodes to exactly `length` chars.

`hashBytesForLength` — `internal/issueid/generate.go`: `3→2`, `4→3`, `5→4`, `6→4`, `7→5`, `8→5`, default `→3`. Test pins these plus `99→3` — `internal/issueid/generate_test.go`.

`encodeBase36(data, length)` — `internal/issueid/generate.go`:
- Big-int division by 36, digits from `Base36Alphabet`, most-significant first.
- Left-pads with `"0"` when shorter than `length`; tests: all-zero bytes → `"000000"` at length 6, `[]byte{1}` → `"000001"` — `internal/issueid/generate_test.go`.
- Truncates by taking the **tail** when longer: `value = value[len(value)-length:]`; test asserts the clamped value equals the tail of the full encoding — `internal/issueid/generate_test.go`.

Determinism: same inputs + same nonce → same ID; different nonce → different ID — `internal/issueid/generate_test.go`.

### 2.5 Adaptive length

`ComputeAdaptiveLength(numIssues)` — `internal/issueid/generate.go`: returns the smallest `length` in `[3,8]` with `CollisionProbability(numIssues, length) <= 0.25`; falls through to `MaxHashLength` (8).

`CollisionProbability(numIssues, idLength)` — `internal/issueid/generate.go`: `1 - exp(-(n*n) / (2 * 36^idLength))` (birthday bound).

`getAdaptiveIssueIDLength` — `internal/store/issue_ids.go`: counts top-level issues, returns `issueid.ComputeAdaptiveLength(count)`; on count error returns `(6, err)`.

`countTopLevelIssues` — `internal/store/issue_ids.go`: `SELECT COUNT(*) FROM issues WHERE id NOT LIKE ?` with `"%.%"`. Counts across all prefixes and includes soft-deleted/archived rows (no `deleted_at` filter).

### 2.6 Minting algorithm and collision handling

`newIssueID` — `internal/store/issue_ids.go`: if `strings.TrimSpace(parentID) != ""` → child path, else top-level path.

`newTopLevelIssueID` — `internal/store/issue_ids.go`:
1. `baseLength, err := getAdaptiveIssueIDLength(...)`; on error `baseLength = 6`.
2. Clamp: `if baseLength > issueid.MaxHashLength { baseLength = issueid.MaxHashLength }`.
3. For `length` from `baseLength` to `MaxHashLength` (8) inclusive, for `nonce` from `0` to `NonceAttempts-1` (0..9): generate candidate and run `SELECT COUNT(*) FROM issues WHERE id = ?`. Query error → `fmt.Errorf("check issue id collision: %w", err)`.
4. First candidate with `count == 0` is returned. The existence check is over ALL issues including soft-deleted ones.
5. Exhaustion error: `fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, issueid.MaxHashLength)`.

`newChildIssueID` — `internal/store/issue_ids.go`:
1. `SELECT id FROM issues WHERE id LIKE ?` with `parentID + ".%"`; error → `fmt.Errorf("query child ids: %w", err)`.
2. For each row, `suffix := strings.TrimPrefix(candidate, parentID+".")`; skip if suffix is empty or contains a `.` (i.e., only direct children count).
3. `strconv.Atoi(suffix)`; non-numeric suffixes are skipped silently.
4. Track `maxChildNumber`; scan error → `fmt.Errorf("scan child id: %w", err)`; iteration error → `fmt.Errorf("iterate child ids: %w", err)`.
5. Return `fmt.Sprintf("%s.%d", parentID, maxChildNumber+1)` — first child is `.1` since `maxChildNumber` starts at 0. Deleted children still occupy their number because the LIKE query has no `deleted_at` filter, so numbers are never reused while the row exists.

### 2.7 Parsing / validation of an existing ID

There is no ID parser or validator in this slice: lookups bind the id verbatim (e.g. `requireIssueExistsTx` runs `SELECT 1 FROM issues WHERE id = ?` — `internal/store/store.go`), and `ListIssues`' `IDs` filter only trims and drops empties, through `storage.TrimmedNonEmpty`, before `i.id IN (...)` — `internal/store/store.go`. No case normalization is applied to a supplied ID anywhere in these files.

---

## 3. LABELS

### 3.1 Canonical form

`model.NormalizeLabel` — `internal/model/label.go`:
- `strings.ToLower(strings.TrimSpace(label))`.
- Empty after trim → `errors.New("label is required")`.
- Contains a comma → `errors.New("label cannot contain commas")`.
- No other characters are rejected; no length cap in code (the column is `VARCHAR(191)`, `migrations/00001_baseline.sql`).

`store.normalizeLabel` is a pass-through wrapper — `internal/store/labels.go`.

`canonicalizeLabels(labels)` — `internal/store/labels.go`: normalizes each (propagating the first error), de-duplicates on the normalized value keeping first occurrence, then `sort.Strings(out)`. Result is ascending-sorted, unique, lowercase.

### 3.2 AddLabel

`Store.AddLabel(ctx, storage.AddLabelInput{IssueID, Name, CreatedBy})` — `internal/store/labels.go`:
1. `s.GetIssue(ctx, in.IssueID)`; error returned as-is (a missing issue yields `storage.NotFoundError` from GetIssue).
2. `normalizeLabel(in.Name)`; error returned.
3. `createdBy := strings.TrimSpace(in.CreatedBy)`; empty → `"unknown"`.
4. Inside `s.withMutation(ctx, "add label", ...)`:
   ```sql
   INSERT INTO labels(issue_id, label, created_at, created_by)
   VALUES (?, ?, ?, ?)
   ON DUPLICATE KEY UPDATE issue_id = issue_id
   ```
   bound with `in.IssueID`, normalized label, `time.Now().UTC().Format(time.RFC3339Nano)`, `createdBy` — `internal/store/labels.go`. The `ON DUPLICATE KEY UPDATE issue_id = issue_id` makes a re-add a no-op that preserves the original `created_at`/`created_by`. Error → `fmt.Errorf("insert label: %w", err)`.
5. Returns `s.ListLabels(ctx, in.IssueID)` — the full label set after the add.

### 3.3 RemoveLabel

`Store.RemoveLabel(ctx, issueID, labelName)` — `internal/store/labels.go`:
1. `GetIssue` existence check; 2. `normalizeLabel(labelName)`.
3. Inside `s.withMutation(ctx, "remove label"...)`: `deleteLabelTx(ctx, tx, labelKey{issueID, label})`, which runs `DELETE FROM labels WHERE issue_id = ? AND label = ?` — `internal/store/row_deletes.go`.
4. `affected == 0` → `storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, label)}`, rendering as `label "<issueID>/<label>" not found` (`internal/storage/errors.go`).
5. Returns `s.ListLabels(ctx, issueID)`.

Error-vs-not-found distinction: `execDelete` wraps a delete failure as `fmt.Errorf("delete %s: %w", subject, err)` and a `RowsAffected()` failure as `fmt.Errorf("delete %s: rows affected: %w", subject, err)`, where subject is `fmt.Sprintf("label %s:%s", key.issueID, key.name)` — `internal/store/row_deletes.go`. `TestRemoveLabelSurfacesGenuineRowsAffectedError` injects a driver whose `RowsAffected()` always fails and asserts the returned error is NOT a `storage.NotFoundError` and wraps the injected cause — `internal/store/relations_rows_affected_test.go`, fault harness.

### 3.4 ReplaceLabels / replaceLabelsTx

`Store.ReplaceLabels(ctx, issueID, labels, createdBy)` — `internal/store/labels.go`: `GetIssue` check, `canonicalizeLabels`, then `s.withMutation(ctx, "replace labels"...)` calling `replaceLabelsTx`.

`replaceLabelsTx` — `internal/store/labels.go`:
- `DELETE FROM labels WHERE issue_id = ?` — clears the whole set; error → `fmt.Errorf("clear labels: %w", err)`.
- `author := strings.TrimSpace(createdBy)`; empty → `"unknown"`.
- One `timestamp := time.Now().UTC().Format(time.RFC3339Nano)` shared by every inserted row.
- Per label: `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)`; error → `fmt.Errorf("insert label %q: %w", label, err)`.
- Consequence: replacing a set rewrites `created_at`/`created_by` for labels that survive the replace.

`replaceLabelsTx` is also the label writer during `CreateIssue` — `internal/store/store.go`, fed by `canonicalizeLabels(in.Labels)` at `internal/store/store.go`.

### 3.5 ListLabels and other read paths

- `Store.ListLabels` — `internal/store/labels.go`: `SELECT label FROM labels WHERE issue_id = ? ORDER BY label ASC`; query error → `fmt.Errorf("list labels: %w", err)`; scan errors returned bare; returns `nil` slice when there are no rows (the slice is never pre-allocated).
- `loadLabelsByIssueIDs` — `internal/store/store.go`: `SELECT issue_id, label FROM labels WHERE issue_id IN (?, ?, …) ORDER BY label ASC`; error → `fmt.Errorf("load labels by issue ids: %w", err)`.
- `listAllLabels` — `internal/store/store.go`: `SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC`; error → `fmt.Errorf("list all labels: %w", err)`.
- List filtering by label — `internal/store/store.go`: `filter.LabelsAll` is run through `canonicalizeLabels` and each label adds a conjunct `EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)` (AND semantics, one clause per label).
- Import/restore insert: `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)` with error `fmt.Errorf("restore label %s:%s: %w", label.IssueID, label.Name, err)` — `internal/store/import_export.go`.
- Orphan check: `SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL` — `internal/store/import_export.go`.
- Delta replay uses `deleteLabelTx` + `insertLabelTx` keyed on `labelKey{issueID, name}` — `internal/store/export_delta.go`.

### 3.6 Uniqueness / cascade

- Uniqueness is the `(issue_id, label)` primary key (`migrations/00001_baseline.sql`); `AddLabel` absorbs the duplicate via `ON DUPLICATE KEY UPDATE` (`internal/store/labels.go`), while `replaceLabelsTx`'s plain INSERT would surface a duplicate-key error — canonicalizeLabels' dedupe is what prevents that (`internal/store/labels.go`).
- Deleting an issue row removes its labels via `ON DELETE CASCADE` (`migrations/00001_baseline.sql`; noted at `internal/store/row_deletes.go`). Ordinary issue deletion is a soft `deleted_at` stamp, so no CRUD path triggers this cascade (`internal/store/row_deletes.go`).
- There is no label rename operation anywhere in the Go source (no rename symbol under `internal/` touching labels).

### 3.7 Test-pinned label behavior

`TestStoreLabelsAreWritableFirstClassData` — `internal/store/store_test.go`: creating with `Labels: []string{"Renderer", "gpu"}` yields `["gpu","renderer"]` (lowercased and sorted); `AddLabel("contracts")` returns 3 labels; `Apply` with `Labels: &[]string{"critical","renderer"}` replaces the set to exactly those two; `RemoveLabel("critical")` returns `["renderer"]`; the surviving label appears in `GetIssueDetail` and in export as `export.Labels[0].Name == "renderer"`.

---

## 4. RELATIONS

### 4.1 The sealed kind set

`internal/model/relation_type.go`:
- `type RelationType string`
- `RelBlocks RelationType = "blocks"`
- `RelParentChild RelationType = "parent-child"`
- `RelRelatedTo RelationType = "related-to"`

`ParseRelationType(s)` — `internal/model/relation_type.go`: `strings.TrimSpace(s)` then matches the three constants; anything else → `errors.New("relation type must be blocks, parent-child, or related-to")`. This is the only string→type gate; the store does no re-validation (`internal/store/relations.go` comment).

### 4.2 Direction and canonicalization rules

- `StoreEndpoints(from, to)` — `internal/model/relation_type.go`: for `blocks` returns `(to, from)`; all other types pass through. It is its own inverse. Stored orientation for blocks is therefore **src = dependent, dst = dependency**, the reverse of the human "<blocker> blocks <blocked>" reading.
- `SingleValuedFromSrc()` — `internal/model/relation_type.go`: true only for `parent-child`. blocks and related-to are many-valued from src.
- `CanonicalEndpoints(src, dst)` — `internal/model/relation_type.go`: for `related-to`, if `dst < src` returns `(dst, src)` (lexicographic sort of the endpoint pair, giving an undirected edge exactly one representation); directed types unchanged.

Bucketing convention, `bucketRelations(focalID, relations, issuesByID)` — `internal/store/relations.go`:
- `RelBlocks` with `rel.SrcID == focalID` → the counterpart lands in `DependsOn`; with `rel.DstID == focalID` → counterpart lands in `Blocks`. Both branches run, so a self-blocks row would populate both.
- `RelParentChild` with `rel.SrcID == focalID` → `Parent = &<dst issue>`; with `rel.DstID == focalID` → append to `Children`.
- Counterparts absent from `issuesByID` are silently skipped (every `if …, ok :=` guard).
- `Children`, `DependsOn`, `Blocks` are initialized to empty (non-nil) slices; `Parent` stays nil.
- All three slices are sorted by `sortIssuesByRank`, which is a stable sort on `Rank` ascending with `ID` ascending as tiebreak — `internal/store/store.go`.
- `related-to` is not bucketed here at all.

`relatedFrom(focalID, relations, issuesByID)` — `internal/store/relations.go`: keeps only `RelRelatedTo` rows; the counterpart is `rel.SrcID`, or `rel.DstID` when `rel.SrcID == focalID`; result sorted by rank; returns an empty non-nil slice.

`siblingsOf(focalID, parentChildren)` — `internal/store/relations.go`: returns the input minus the focal ID, order preserved; an only child yields an empty slice.

### 4.3 AddRelation

`Store.AddRelation(ctx, storage.AddRelationInput{SrcID, DstID, Type, CreatedBy})` — `internal/store/relations.go`:
1. Pre-tx: `in.Type == model.RelRelatedTo && in.SrcID == in.DstID` → `errors.New("related-to cannot target itself")`. Note this self-check is only for related-to at this point.
2. `srcID, dstID := in.Type.CanonicalEndpoints(in.SrcID, in.DstID)` — related-to endpoints get sorted.
3. `now := time.Now().UTC()`; the returned `model.Relation` is built with the canonical endpoints, the type, `now`, and `strings.TrimSpace(in.CreatedBy)`; empty createdBy → `"unknown"`.
4. Inside `s.withMutation(ctx, "add relation", ...)`:
   - `requireIssueExistsTx(ctx, tx, in.SrcID)` then `requireIssueExistsTx(ctx, tx, in.DstID)` — note these use the **input** ids, not the canonicalized ones. `requireIssueExistsTx` runs `SELECT 1 FROM issues WHERE id = ?` and maps `sql.ErrNoRows` to `storage.NotFoundError{Entity: "issue", ID: issueID}`, other errors to `fmt.Errorf("check issue exists: %w", err)` — `internal/store/store.go`. It deliberately does not filter on `deleted_at`, so archived/soft-deleted rows count as existing (`internal/store/store.go` comment).
   - If type is `blocks`: `rejectBlocksCycle(ctx, tx, rel.SrcID, rel.DstID)`.
   - If `rel.Type.SingleValuedFromSrc()` (parent-child only): `setSingleValuedEdgeTx`; otherwise `insertRelationTx`.
5. Returns the constructed `model.Relation` (with canonical endpoints and the pre-tx timestamp) on success.

Duplicate handling: a repeat of the same `(src,dst,type)` for `blocks`/`related-to` goes through the raw INSERT and hits the primary key, surfacing as `fmt.Errorf("insert relation %s->%s (%s): %w"...)` from `insertRelationTx` (`internal/store/relations.go`). There is no upsert.

Endpoint-vanished behavior is pinned by `TestRelationEndpointVanishedRejected` — `internal/store/store_test.go`.

### 4.4 The write statements

`insertRelationTx` — `internal/store/relations.go`:
```sql
INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)
```
bound with `rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy`. Error: `fmt.Errorf("insert relation %s->%s (%s): %w", rel.SrcID, rel.DstID, rel.Type, err)`. This is the only relations INSERT in the package (also used by `CreateIssue`'s parent edge, `internal/store/store.go`, and by delta replay, `internal/store/export_delta.go`).

`setSingleValuedEdgeTx` — `internal/store/relations.go`:
```sql
DELETE FROM relations WHERE src_id = ? AND type = ?
```
bound `rel.SrcID, string(rel.Type)`; error → `fmt.Errorf("clear single-valued relation: %w", err)`; then `insertRelationTx`. Result: at most one edge of that type out of the src.

`deleteRelationRowTx(key relationKey{srcID,dstID,kind})` — `internal/store/row_deletes.go`:
```sql
DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?
```
subject string `fmt.Sprintf("relation %s->%s (%s)", key.srcID, key.dstID, key.kind)`; errors from `execDelete` are `delete relation a->b (blocks): <err>` and `delete relation a->b (blocks): rows affected: <err>` (`internal/store/row_deletes.go`).

`relationKey` is exactly the schema primary key `(src_id, dst_id, type)` — `internal/store/row_deletes.go`.

### 4.5 Cycle detection on write

`rejectBlocksCycle(ctx, tx, dependent, dependency)` — `internal/store/relations.go`:
- Self-edge: `dependent == dependency` → `fmt.Errorf("blocks: %s cannot block itself", dependent)`.
- Loads every live blocks edge with `loadBlocksEdges` (see §5.9); load error → `fmt.Errorf("blocks cycle check: %w", err)`.
- `blocksPrecedes(blocksPrecedenceAdj(edges), dependent, dependency)` true → error text:
  `"blocks: cannot add %s depends-on %s — %s already depends on %s (directly or transitively), so this edge would close a dependency cycle, which has no valid rank order"` with args `(dependent, dependency, dependency, dependent)`.

Tests: `TestAddRelationRejectsBlocksCycle` (A→B then B→A rejected) — `internal/store/store_test.go`; `TestAddRelationRejectsTransitiveBlocksCycle` (A→B, B→C, C→A rejected) —; `TestAddRelationRejectsSelfBlock` —.

### 4.6 RemoveRelation

`Store.RemoveRelation(ctx, srcID, dstID, relType)` — `internal/store/relations.go`:
- `srcID, dstID = relType.CanonicalEndpoints(srcID, dstID)` first (so related-to removal is order-insensitive).
- Inside `s.withMutation(ctx, "remove relation", ...)`: `deleteRelationRowTx` with that key.
- `affected == 0` → `storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}`, rendering as `relation "src=… dst=… type=…" not found`.
- No existence check on the endpoints; no `GetIssue` precheck.

`TestRemovePerChildBlockAfterRankReorder` — `internal/store/store_test.go` — removes per-child and epic-level blocks edges after a `RankAbove`, asserting store orientation `src=dependent, dst=dependency` holds after reordering.

### 4.7 ClearParent

`Store.ClearParent(ctx, childID)` — `internal/store/relations.go`:
- `GetIssue(ctx, childID)` precheck.
- Inside `s.withMutation(ctx, "clear parent", ...)`:
  ```sql
  DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'
  ```
  (type literal inlined, not a placeholder) —. Error → `fmt.Errorf("delete parent relation: %w", err)`.
- `res.RowsAffected()` error → `fmt.Errorf("rows affected: %w", err)`.
- `affected == 0` → `storage.NotFoundError{Entity: "parent relation", ID: childID}`.
- `TestClearParentSurfacesGenuineRowsAffectedError` proves a failing `RowsAffected()` is not masked as NotFound and wraps the cause — `internal/store/relations_rows_affected_test.go`.

### 4.8 SetParent

`Store.SetParent(ctx, storage.SetParentInput{ChildID, ParentID, CreatedBy})` — `internal/store/relations.go`:
- Blank check: either id empty after `strings.TrimSpace` → `errors.New("child and parent ids are required")`.
- `in.ChildID == in.ParentID` → `errors.New("child and parent cannot be the same issue")`.
- Builds `model.Relation{SrcID: ChildID, DstID: ParentID, Type: RelParentChild, CreatedAt: time.Now().UTC(), CreatedBy: trimmed}`; empty CreatedBy → `"unknown"`.
- In `s.withMutation(ctx, "set parent"...)`: `requireIssueExistsTx` for child then parent, then `setSingleValuedEdgeTx`. No ancestry/cycle check on parent-child — a parent cycle is only detected later at read time by `ancestorChain` (§5.3).
- Returns the relation value.

`TestAddRelationEnforcesSingleParentCardinality` — `internal/store/store_test.go`: adding a second `parent-child` edge for a child through `AddRelation` succeeds and leaves exactly one parent edge (the newer one), same as `SetParent`.

### 4.9 ListRelationsForIssue and listRelations

`Store.ListRelationsForIssue(ctx, issueID, types...)` — `internal/store/relations.go`:
- `GetIssue` precheck.
- `s.listRelations(ctx, issueID)`; with no `types` the full incident set is returned.
- Otherwise filters in Go against a set of wanted types; order is preserved from the SQL.

`Store.listRelations` — `internal/store/store.go`:
```sql
SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? OR dst_id = ? ORDER BY created_at ASC
```
Query error → `fmt.Errorf("list relations: %w", err)`; created_at parsed with `scanTime`; returns an empty non-nil slice.

### 4.10 Batch relations

`structuralRelationTypes = []model.RelationType{model.RelBlocks, model.RelParentChild}` — `internal/store/relations.go`. `related-to` is excluded from every batch path.

`relationEndpointColumns = map[string]struct{}{"src_id": {}, "dst_id": {}}` — `internal/store/relations.go`; a column not in that set → `fmt.Errorf("list relations by endpoint: unknown column %q", column)` (`internal/store/relations.go`).

`relationsByEndpoint(ctx, column, ids)` — `internal/store/relations.go`: builds
```sql
SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE <column> IN (?,…) AND type IN (?,?)
```
via `fmt.Sprintf` with placeholder lists from `repeatPlaceholder` (`internal/store/relations.go`); args are the ids then the two type strings. No ORDER BY (ordering is `mergeRelations`' job). Reading the rows is `scanRelationRows`' job, called once per id batch: query error → `fmt.Errorf("list relations for ids: %w", err)`, and the scan of `created_at` goes through `scanTime`.

`listRelationsForIDs(ctx, ids)` — `internal/store/relations.go`: empty ids → `(nil, nil)`; otherwise one query per endpoint column and `mergeRelations(bySrc, byDst)`. The documented reason for two conjunctive queries rather than one `src_id IN (…) OR dst_id IN (…)` is engine index-analysis blowup.

`mergeRelations(bySrc, byDst)` — `internal/store/relations.go`: dedupes on `relationKey{srcID,dstID,kind}` keeping first occurrence, then `slices.SortFunc` by `CreatedAt` ascending, then `SrcID`, then `DstID`, then `string(Type)`. `TestMergeRelationsDedupesAndOrders` pins both legs, including a created_at tie broken by key — `internal/store/relations_batch_test.go`.

`Store.GetRelationsByIDs(ctx, ids)` — `internal/store/relations.go`:
- `dedupeStrings(ids)` preserving first-seen order (helper); empty → empty map, nil error.
- Loads structural relations for the subjects.
- Builds a `subjectSet` and a `needed` set seeded with the subjects; every relation endpoint is added to `needed`.
- Buckets rows per subject; a row whose src and dst are both the same subject is added once (`rel.DstID != rel.SrcID` guard).
- Hydrates `s.getIssuesByIDs(ctx, mapKeys(needed))` (`mapKeys`, unspecified order).
- For each subject present in `issuesByID`, produces `bucketRelations(id, bySubject[id], issuesByID)` with `.Issue` set; subjects that no longer exist are simply omitted from the result map.
- `TestGetRelationsByIDsMatchesIssueDetail` asserts parity with `GetIssueDetail` for Children/DependsOn/Blocks/Parent, absence of a nonexistent subject, epic children in rank order, and the DependsOn/Blocks orientation — `internal/store/relations_batch_test.go`.

### 4.11 Listing by parent

Children are read through `Store.ListIssues` with `filter.ParentIDs` set (§5.7); there is no separate children query.
- `requireIssues(ctx, filter.ParentIDs)` runs before the parents are resolved — `internal/store/store.go`, defined. Empty ids → no batches and so no query at all. Otherwise one `SELECT id FROM issues WHERE id IN (?,…)` per batch of at most `idBatchSize` ids; query error → `fmt.Errorf("check issues exist: %w", err)`. The batches' answers are unioned into one found-set, which is the only thing the verdict reads — a membership test is the shape batching is trivially sound for. Walking the caller's own ids in the order given, the first one absent from that set → `storage.NotFoundError{Entity: "issue", ID: id}`, so a repeated id is still named once and the batching is invisible in the message. The query reads existence only, so a soft-deleted or archived parent passes.
- Parent clause: there is none. `selectedIssueIDs` (`internal/store/store.go`) resolves the parents through `childIDsOfParents`, which runs `SELECT src_id FROM relations WHERE dst_id IN (?,…) AND type = ?` one batch of at most `idBatchSize` parent ids at a time, and folds the result into the id filter — intersecting it with `filter.IDs` when both are set. Only direct children match, and several parent ids union together. The child rows go through the listing's other clauses, so `archived_at IS NULL` and `deleted_at IS NULL` apply unless `IncludeArchived`/`IncludeDeleted` is set, and ordering is `buildIssueOrderClause` (rank ascending, then id, when no `SortBy`).
- `TestStoreListByParentDefaultsToRankOrder` — two children wired by `SetParent` list in rank order — `internal/store/store_test.go`.
- `TestListByParentReturnsEpicChildrenWithDerivedLifecycle` — a sub-epic listed under its root carries container progress derived from its closed leaf (closed 1, total 1) — `internal/store/store_test.go`.

---

## 5. RANKING

### 5.1 Rank representation

`internal/rank/rank.go` header — lexicographic fractional indexing; ranks are strings compared bytewise, stored in `issues.item_rank`.

- `alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"` — `internal/rank/rank.go` (base-62, digits then uppercase then lowercase, so ASCII order equals alphabet order).
- `base = len(alphabet)` = 62 — `internal/rank/rank.go`.
- `charIndex [256]int` maps byte→ordinal, `-1` for non-members, initialized in `init()` — `internal/rank/rank.go`.
- `SmoothingThreshold = 8` — `internal/rank/rank.go`.
- `SmoothingWindow = 32` — `internal/rank/rank.go`.
- `minGap = 16` (local const inside `spacedRanks`) — `internal/rank/rank.go`.
- The empty string is the "unranked" sentinel: `Valid("")` is false — `internal/rank/rank.go`. Every rank query in the store excludes `item_rank != ''`.

`Initial()` returns `string(alphabet[base/2])` = `"V"` — `internal/rank/rank.go`.

`Valid(s)` — `internal/rank/rank.go`: false for empty; false if any byte is not in the alphabet; documented as a check for *persisted* values only, explicitly not the input contract of Midpoint/Before/After which accept an empty bound as a sentinel.

### 5.2 Midpoint / Before / After

`Midpoint(a, b)` — `internal/rank/rank.go`:
- `a == b` with `a` non-empty → `errors.New("rank: a and b are equal")`; `Midpoint("", "")` is not refused.
- both non-empty and `a >= b` → `errors.New("rank: a must be less than b")`.
- `b` non-empty and `rank.Significant(a) == rank.Significant(b)` → `fmt.Errorf("%w: %q and %q pad to the same value", ErrNoRoom, a, b)`, where `ErrNoRoom = errors.New("rank: no room between the bounds")`. This refuses `b` being `a` extended by zeros (`"10"`, `"100"`) and an empty `a` with an all-zero `b`.
- Walks positions: missing/short `a` contributes virtual char index 0 ("below the floor"), missing/short `b` contributes virtual index `base` ("above the ceiling"). An out-of-alphabet byte gives `errors.New("rank: invalid character in a")` or `"rank: invalid character in b"`.
- If `bChar - aChar > 1`, emits `alphabet[aChar + (bChar-aChar)/2]` and returns.
- Otherwise emits `alphabet[aChar]` and advances a position, growing the string by one char per adjacent/equal position.
- Empty `a` means "before everything"; empty `b` means "after everything"; both empty is the whole keyspace, whose midpoint is `Initial()`'s `"V"`.

`Before(a)` refuses an empty `a` with `errors.New("rank: Before needs a rank, not the empty string")`; otherwise it returns `Midpoint("", a)` and its error, so an all-zero `a` fails with `ErrNoRoom` — `internal/rank/rank.go`.
`After(a)` = `Midpoint(a, "")`, panicking `"rank.After called with empty string"` when `a` is empty or `Midpoint` errors — `internal/rank/rank.go`.

### 5.3 Spaced ranks (used by smoothing)

`SpacedRanks(n)` — `internal/rank/rank.go`: `spacedRanks(n, "", "")`; panics `fmt.Sprintf("rank: spaced ranks with empty bounds failed: %v", err)` if that ever errors.

`SpacedRanksBetween(lower, upper, n)` — `internal/rank/rank.go`: both bounds non-empty with `lower >= upper` → `errors.New("rank: lower must be less than upper")`; else `spacedRanks`. That guard admits bounds nothing can sort between (`"10"` and `"100"`); `spacedRanks` enforces the rest.

`spacedRanks(n, lower, upper)` — `internal/rank/rank.go`:
- `n < 0` → `errors.New("rank: n must be non-negative")`; `n == 0` → `(nil, nil)`.
- Starting at `length = max(len(lower), len(upper)) + 1`, increments length until the integer span between the bounds divided by `n+1` is at least `minGap` (16).
- A **negative** span ends the search instead of skipping the length: `fmt.Errorf("%w: %q and %q pad to the same value, so no rank longer than both sorts between them", ErrNoRoom, lower, upper)`. Every candidate is longer than both bounds, so `span(L+1) = 62*(span(L)+1) - 1`; a negative span stays negative at every greater length, while a non-negative one grows 62-fold, so this is the only case that would not terminate.
- Emits `n` values at `lo + step*(i+1)` encoded fixed-width via `encodeBase62`. All returned ranks share one length.
- `lowerBoundInt(s, length)` —: empty → 0; else `stringToInt(s, length)`, plus 1 when `len(s) >= length`.
- `upperBoundInt(s, length)` —: empty → `pow62(length)`; else `stringToInt(s,length) - 1`, unchecked; an all-zero bound yields `-1`, which the negative-span arm above reports.
- `stringToInt` —: base-62 accumulation, right-padded with index 0; invalid byte → `errors.New("rank: invalid character in bounds")`.
- `encodeBase62(value, length)` —: negative → `errors.New("rank: cannot encode negative value")`; remainder out of `[0,62)` → `errors.New("rank: base62 remainder out of range")`; leftover quotient → `errors.New("rank: value does not fit fixed-width encoding")`.

### 5.4 Rank at creation

`nextRankForPlacement(ctx, tx, p, f)` — `internal/store/store.go`: `edgeFor(p)` → `topEdge` for `storage.RankTop`, `bottomEdge` for `storage.RankBottom`, `fmt.Errorf("unknown rank placement: %d", p)` otherwise; then `rankBetweenTx` between the bounds `edge.filingBoundsTx(ctx, tx, f)` reads — the end's filing rank paired with the nearest rank the whole workspace holds on its far side, or, when that filing rank is empty, the pair `firstInFrameBoundsTx` reads just past the rank of the issue the frame names, or past the workspace's last rank when the frame names no ranked issue — `("", "")` only when nothing in the workspace is ranked (`internal/store/ranking.go`).

`storage.RankPlacement` is an `int` with `RankBottom = iota` (0, the zero value and default) and `RankTop` (1) — `internal/storage/issues.go`.

`nextRankAtBottom` — `internal/store/store.go`:
```sql
SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' ORDER BY item_rank DESC LIMIT 1
```
non-`ErrNoRows` error → `fmt.Errorf("query last rank: %w", err)`; no row or empty → `rank.Initial()` ("V"); otherwise `rank.After(lastRank)`.

`nextRankAtTop` — `internal/store/store.go`: same query with `ORDER BY item_rank ASC`; error → `fmt.Errorf("query first rank: %w", err)`; no row → `rank.Initial()`; else `rank.Before(firstRank)`.

Called inside `CreateIssue`'s mutation right after ID minting — `internal/store/store.go`. The keyspace stays one flat space of rank strings across all issues; what is scoped to a frame is the existing key a new key lands beside — the filing frame's leading key at the top edge, the whole workspace's last key at the bottom. The key bounding that one on its far side is read from the whole workspace at both ends.

### 5.5 The frame concept

Rank meaning is frame-local: an issue's rank is only compared against frame-mates (siblings under the same container, or fellow top-level items) — `internal/store/ranking.go`.

`ancestorChain(ctx, id)` — `internal/store/ranking.go`:
```sql
SELECT r.dst_id FROM relations r JOIN issues p ON p.id = r.dst_id
WHERE r.src_id = ? AND r.type = 'parent-child' AND p.deleted_at IS NULL
```
- Chain is self-first, root-last, starting `[id]`.
- `sql.ErrNoRows` terminates the walk; other error → `fmt.Errorf("ancestor chain of %s: %w", id, err)`.
- Revisiting a seen node → `fmt.Errorf("ancestor chain of %s: parent cycle at %s", id, parent)`.
- Only non-deleted parents are followed, so a soft-deleted parent truncates the chain.

`frameContainmentError` — `internal/store/ranking.go`: fields `containerID`, `containedID`; `Error()` = `fmt.Sprintf("%s is inside %s; no comparable frame contains both — rank it against a sibling instead", e.containedID, e.containerID)`.

`resolveFrameRepresentatives(chains)` — `internal/store/ranking.go`:
1. Builds per-chain membership maps id→index.
2. Containment check: if any chain's head (`chain[0]`) appears in another chain at index > 0, returns `&frameContainmentError{containerID: chain[0], containedID: chains[j][0]}`.
3. LCA: the first element of `chains[0]` present in every other chain; `lcaID == ""` means no shared ancestor.
4. Representatives: with no LCA, each chain's **root** (`chain[len-1]`); with an LCA, the element **one level below** the LCA in each chain: `chain[memberships[i][lcaID]-1]`. Frame-mates therefore resolve to themselves.

Pinned cases — `internal/store/ranking_frame_test.go`:
- `{{x},{y},{z}}` → `[x y z]`
- `{{c1,e},{c2,e},{c3,e}}` → `[c1 c2 c3]` (siblings rank directly)
- `{{c1,e},{x},{c2,e}}` → `[e x e]`
- `{{c1,e1,p},{c2,e2,p},{x}}` → `[p p x]`
- `{{c1,e1,p},{c2,e2,p}}` → `[e1 e2]`
- `{{g,s,e},{c,e}}` → `[s c]`
- `{{c,e},{e}}`, `{{e},{g,s,e}}`, `{{e},{x},{c,e}}` → `frameContainmentError`

`resolveComparableFrame(issueChain, targetChain)` — `internal/store/ranking.go`: wraps `resolveFrameRepresentatives` for the pair and rewrites containment errors:
- when the container is the issue itself: `fmt.Errorf("cannot rank %s relative to %s: %s contains it; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0])`;
- otherwise: `fmt.Errorf("cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0], targetChain[0])`.
Returns `(reps[0], reps[1])` as `(movedID, anchorID)`.

Pinned pair cases — `internal/store/ranking_frame_test.go`: top-level pair unchanged; same-epic siblings unchanged; standalone vs child → `(x, e)`; child vs standalone → `(e, x)`; two epics' children → `(e1, e2)`; grandchild vs child of shared epic → `(s, c)`; either containment direction errors.

`resolveRankPair(ctx, issueID, targetID)` — `internal/store/ranking.go`:
- `issueID == targetID` → `errors.New("cannot rank an issue relative to itself")`.
- `GetIssue(targetID)` then `GetIssue(issueID)` — in that order.
- Both ancestor chains, then `resolveComparableFrame`.
- `GetIssue(anchorID)` for the hydrated anchor whose `Rank` seeds midpoint math.
- Returns `(anchor, storage.RankMove{MovedID: movedID, AnchorID: anchorID})`.

`storage.RankMove{MovedID, AnchorID}` — `internal/storage/rank.go`; `storage.RankSetResolution{NamedID, RankedID}` with json tags `named_id`/`ranked_id` — `internal/storage/rank.go`.

### 5.6 The five rank verbs

**RankToTop(issueID)** — `internal/store/ranking.go`:
- `GetIssue` precheck.
- In `withMutation(ctx, "rank to top", …)`:
  ```sql
  SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id != ? ORDER BY item_rank ASC LIMIT 1
  ```
  non-`ErrNoRows` error → `fmt.Errorf("rank-to-top: query first: %w", err)`.
- No/blank first rank → `rank.Initial()`; else `rank.Before(firstRank)`.
- `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` with `time.Now().UTC().Format(time.RFC3339Nano)`; error → `fmt.Errorf("rank-to-top: update: %w", err)`.
- Then `smoothRanksIfNeededTx(ctx, tx, newRank)`.
- No frame resolution: the top is the global top.

**RankToBottom(issueID)** — `internal/store/ranking.go`: identical shape with `ORDER BY item_rank DESC`, errors `"rank-to-bottom: query last: %w"` and `"rank-to-bottom: update: %w"`, and `rank.After(lastRank)`.

**RankAbove(issueID, targetID)** — `internal/store/ranking.go`:
- `resolveRankPair` first (all its errors propagate).
- In `withMutation(ctx, "rank above", …)`, the new rank comes from `rankBetweenTx`, whose `bounds` closure reads the anchor's rank through `anchorRankTx` and then returns `topEdge.roomBesideTx(ctx, tx, anchorRank, move.MovedID)`. The closure's error → `fmt.Errorf("rank-above: %w", err)`.
- `roomBesideTx` splices the edge's `outside` fragment into one shared statement:
  ```sql
  SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND item_rank < ? ORDER BY item_rank DESC
  ```
  bound `(anchorRank)`, with no `LIMIT` of its own — `nearestRankOutside` appends `LIMIT len(exclude)+1` and returns the first row whose id is not one this write is about to vacate; `item_rank < ? ORDER BY item_rank DESC` is `topEdge.outside`. Error → `fmt.Errorf("query the key outside the %s: %w", e.name, err)`, with `e.name` `"top"`, which the `rank-above: %w` wrap prefixes. The read is **not** scoped to the pair's frame — it searches the whole workspace. The frame's say in the move is choosing the anchor, which `rankPairTx` already did. The exclusion is the moving issue, which leaves the key it is about to vacate out of its own bounds, and `item_rank != ''` keeps unranked rows out. It is applied in Go rather than as `id NOT IN (?,…)` because the set is as large as a rank-set stack, and a membership list of caller-chosen length is the quadratic planner shape `idBatchSize` describes — the one shape batching cannot repair, since a negated membership split across batches returns from each batch exactly the rows the others meant to exclude.
- `topEdge.beside` orders the pair `(outsideRank, anchorRank)`, so the key found is the lower bound and the anchor the upper. Selecting nothing — or selecting only rows this write is vacating — reads as `""` (`nearestRankOutside`), which happens only when nothing in the whole workspace sorts before the anchor; the lower bound is then open and the new rank is `rank.Midpoint("", anchorRank)`.
- The anchor's rank is read through `anchorRankTx` (`internal/store/ranking.go`): `SELECT item_rank FROM issues WHERE id = ? AND deleted_at IS NULL AND item_rank != ''`, so only a live, ranked anchor returns a row. No row → `fmt.Errorf("cannot rank against %s: it was deleted while the move was being applied, or it has no rank", anchorID)`, which `bounds` and `rankBetweenTx` return unwrapped, so the `rank-above: %w` wrap prefixes it. An unranked or deleted anchor therefore fails the move.
- `writeRankTx(ctx, tx, move.MovedID, newRank, now)` with `now` from `s.clock`; it runs `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, error → `fmt.Errorf("set rank of %s: %w", id, err)`.
- `smoothRanksIfNeededTx(ctx, tx, newRank)`. Returns the `RankMove` regardless of which id the caller named.

**RankBelow(issueID, targetID)** — `internal/store/ranking.go`: mirror image — the `bounds` closure returns `bottomEdge.roomBesideTx(ctx, tx, anchorRank, move.MovedID)`, whose `outside` fragment is `item_rank > ? ORDER BY item_rank ASC` and whose `e.name` is `"bottom"`; `bottomEdge.beside` orders the pair `(anchorRank, outsideRank)`, so the anchor is the lower bound and the key found the upper. The closure's error → `fmt.Errorf("rank-below: %w", err)`; the write is the same `writeRankTx` and the smoothing the same `smoothRanksIfNeededTx`. This read is likewise unscoped to the frame, so it reads `""` only when nothing in the whole workspace sorts after the anchor, and the new rank is then `rank.Midpoint(anchorRank, "")`. The anchor's rank is read through the same `anchorRankTx`, so an unranked or deleted anchor fails the move with `"rank-below: cannot rank against %s: it was deleted while the move was being applied, or it has no rank"`.

`rankBetweenTx(ctx, tx, bounds)` — `internal/store/ranking.go`: reads the pair through `bounds()` and returns `rank.Midpoint(lower, upper)` unless it fails with `rank.ErrNoRoom`. On `ErrNoRoom` it runs `smoothRanksTx(ctx, tx, upper)`, which respaces the smoothing window around `upper` with no length threshold; error → `fmt.Errorf("make room between %q and %q: %w", lower, upper, err)`. It then reads the pair through `bounds()` again and returns the second `rank.Midpoint` result, error included; a second `ErrNoRoom` is not retried. The `bounds` functions of `RankAbove` and `RankBelow` read the anchor's rank (`anchorRankTx`) and the neighbor (`nearestRank`) on every call, so the second read sees the respaced ranks.

`rankEdge.rankBeyondTx(ctx, tx, f, moving, readEdge)` — `internal/store/ranking.go`: the key past a frame's edge. Its body is one `rankBetweenTx` call, whose `bounds` reads the edge's rank through `readEdge()` on every call and returns a `readEdge` error as is. A non-empty rank goes to `e.roomBesideTx(ctx, tx, edgeRank, moving...)`, which runs `SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND <e.outside>` with `edgeRank` as its only bound arg — `item_rank < ? ORDER BY item_rank DESC` for `topEdge`, `item_rank > ? ORDER BY item_rank ASC` for `bottomEdge` — and orders the pair through the edge's `beside`: `topEdge` gives `(outsideRank, edgeRank)` and `bottomEdge` gives `(edgeRank, outsideRank)`, the two values `edgeFor` returns. `moving` is variadic and lists every key this write is about to vacate, so a whole stack is left out of its own bounds, and it never reaches the statement: `nearestRankOutside` appends `LIMIT len(moving)+1` and returns the first row the order yields whose id is not in the set. Reading one row more than can be skipped is what makes that complete rather than a sample, and it keeps the read bounded by a constant even though the exclusion set is not — which `AND id NOT IN (?,…)` would not, and which batching cannot repair for a negated membership. An empty `moving` therefore reads exactly one row; a failed read is wrapped `fmt.Errorf("query the key outside the %s: %w", e.name, err)`. Nothing on the far side reads as `""`, the open bound. A pair at either end that pads to the same value gets room made the same way a relative move's pair does, and because the edge is not read ahead of `rankBetweenTx` the second read sees the respaced ranks. An **empty** edge rank takes the other arm: `bounds` returns `firstInFrameBoundsTx(ctx, tx, f, moving...)`, which carries the same moving set through to `bottomEdge.roomBesideTx(ctx, tx, containerRank, moving...)`, so a frame holding nothing ranked but the issues being moved lands its key just past the rank of the issue `f` names, with the keys that stack is vacating left out of the bounds at this end too. A frame naming no ranked issue — `storage.TopLevel`, which names no containing issue, or a container carrying no rank — takes the shared arm at the end instead: the workspace's last rank, and `("", "")` only when that read is empty, where `rank.Midpoint("", "")` is `rank.Initial()`'s `"V"` (`internal/rank/rank.go`). That workspace read does not exclude `moving`, so it is empty only when nothing at all is ranked — not merely when this write excludes everything the frame's own read could see. The empty rank never reaches `roomBesideTx`, which refuses an empty anchor with `fmt.Errorf("no room beside the %s of this frame: the key it was read from is empty", e.name)`. Both callers get that arm: `rankToEdge`, passing `f` and `[]string{issueID}`, reading the frame's edge holder through `frameEdgeHolderTx` and wrapping the error `fmt.Errorf("rank to %s: %w", edge.name, err)`; `RankSet`, passing `f` and the whole `ranked` stack, for the key of its last id.

Frame behavior pinned by tests — `internal/store/ranking_frame_test.go`:
- Standalone above an epic child anchors to the epic; epic and all children keep their exact rank strings; standalone ends above the epic.
- Child above a standalone moves the **epic**; children and anchor unchanged.
- Across two epics, `RankBelow(child1, child2)` moves epic1 relative to epic2.
- Same-epic siblings rank directly.
- `RankAbove(child, own epic)` errors containing `"inside"`; `RankBelow(epic, own child)` errors containing `"contains"`.

**RankSet(ids)** — `internal/store/ranking.go`:
- Validation `rankSetValidateIDs` — `internal/store/ranking.go`: fewer than 2 ids → `errors.New("rank set: need at least 2 IDs to establish order")`; an empty-string id → `errors.New("rank set: empty ID in input")`; a repeated id → `fmt.Errorf("rank set: duplicate ID %q in input", id)`.
- `resolveRankSet` — `internal/store/ranking.go`: per id, `GetIssue` then `ancestorChain`; `resolveFrameRepresentatives` errors are wrapped `fmt.Errorf("rank set: %w", err)`; two named ids collapsing to the same representative →
  `fmt.Errorf("rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead", prior, id, reps[i], reps[i])`.
  Returns `[]storage.RankSetResolution{{NamedID, RankedID}}` parallel to the input order.
- In `withMutation(ctx, "rank set", …)`:
  ```sql
  SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND <frame> = ? ORDER BY item_rank ASC
  ```
  bound with the frame alone; `nearestRankOutside` appends `LIMIT len(ranked)+1` and skips the ids being reassigned in Go, so the ranked set never appears in the statement; non-`ErrNoRows` error → `fmt.Errorf("query top: %w", err)`, which the `rank set: %w` wrap below prefixes.
- The last ranked id's key comes from `topEdge.rankBeyondTx`, whose `readEdge` runs the query above through `nearestRankOutside`, so the key is made through `rankBetweenTx`: `rank.Midpoint("", "")`, which is `rank.Initial()`, when the frame has no ranked issue outside the set, otherwise a key sorting before the frame's top rank, with room made when that rank is all zeros. Walking the remaining ids in reverse, each gets `rank.Before(cursor)`. An error from either → `fmt.Errorf("rank set: %w", err)`; cursor becomes the just-assigned rank. Final order: `ids[0] < ids[1] < … < ids[N-1] < existing top` — the whole set is stacked at the top of the keyspace.
- One `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` per id, sharing a single `now` timestamp; error → `fmt.Errorf("rank-set: update %s: %w", id, err)`.
- `smoothRanksIfNeededTx(ctx, tx, newRanks[0])` when the set is non-empty.
- Atomic: all assignments in one mutation.
- Note: `RankSet` returns `resolutions` alongside the mutation error, so resolutions are non-nil even when the write fails.

Tests: absolute top ordering — `internal/store/store_test.go`; duplicates rejected with an error containing `"duplicate"` —; single id rejected with `"at least 2"` —; mixed-frame resolves child→epic with children unmoved — `internal/store/ranking_frame_test.go`; two epics' children resolve to their epics —; same-epic siblings resolve to themselves and only they move —; duplicate representatives rejected with `"both resolve to"` and **no** rank changed —; naming an epic with its own child rejected with `"inside"` in both orders —.

### 5.7 Smoothing (rebalancing)

`smoothRanksIfNeededTx(ctx, tx, triggerRank)` — `internal/store/ranking.go`:
1. Trigger: `len(triggerRank) < rank.SmoothingThreshold` (8) → no-op; otherwise it calls `smoothRanksTx(ctx, tx, triggerRank)` (`internal/store/ranking.go`), which performs steps 2-10 and has no threshold of its own. So smoothing through this function fires only once a rank string reaches 8 characters; `rankBetweenTx` calls `smoothRanksTx` directly.
2. `half := rank.SmoothingWindow / 2` = 16.
3. Below half:
   ```sql
   SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank <= ? ORDER BY item_rank DESC LIMIT ?
   ```
   bound `(triggerRank, half)`; error → `"smooth: below: %w"`. The rows come back descending and are reversed to ascending.
4. Above half:
   ```sql
   SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT ?
   ```
   bound `(triggerRank, half)`; error → `"smooth: above: %w"`. The two halves concatenate into the window.
5. Empty window → no-op; a window of one entry is respaced like any other.
6. The run bounds are computed from the window's own ends rather than scanned for: `runFloor` is `rank.Significant(window[0].rank)`, the least rank sharing the bottom end's significant part; `runCeiling` is `rank.Significant(window[len-1].rank) + "1"`, the least rank sorting above every rank sharing the top end's. Ranks sharing a significant part sort contiguously, so these two values delimit exactly the runs the window's ends sit in.
7. Two bounded range queries pick up the rest of each run:
   ```sql
   SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank >= ? AND item_rank < ? ORDER BY item_rank ASC
   ```
   bound `(runFloor, window[0].rank)`, error → `"smooth: lower run: %w"`; and
   ```sql
   SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? AND item_rank < ? ORDER BY item_rank ASC
   ```
   bound `(window[len-1].rank, runCeiling)`, error → `"smooth: upper run: %w"`. Both already ascend, so they concatenate around the window in order. Both ranges are bounded on each side, so a run costs the rows it holds rather than a scan of the sorted set.
8. The bounds themselves, one row each: the greatest rank below `runFloor` (`ORDER BY item_rank DESC LIMIT 1`), error → `"smooth: lower bound: %w"`; and the least rank at or above `runCeiling` (`ORDER BY item_rank ASC LIMIT 1`), error → `"smooth: upper bound: %w"`. A side with no such row leaves the bound `""` (meaning open-ended).
9. `rank.SpacedRanksBetween(lowerBound, upperBound, len(window))`; error → `fmt.Errorf("smooth: compute ranks: %w", err)`. The two bounds cannot pad to the same value — the one pair that primitive rejects (`internal/rank/rank.go`) is unreachable from here. `lowerBound` sorts below `runFloor`, so it cannot share the bottom end's significant part; and were the two bounds to share one with each other, every rank between them would belong to that single run, while `window[0]` lies between them with a different significant part.
10. `UPDATE issues SET item_rank = ? WHERE id = ?` for each window entry whose new rank differs from the old — `updated_at` is **not** touched here; error → `fmt.Errorf("smooth: update %s: %w", item.id, err)`.

An all-zero rank at the bottom end takes the same path with no special case: its `rank.Significant` is `""`, so `runFloor` is `""`; `item_rank < ''` matches nothing and leaves `lowerBound` the open end, which is correct, while `item_rank >= '' AND item_rank < window[0].rank` is exactly the all-zero ranks below the window, everything sorting below an all-zero rank being itself all-zero.

`rankRowsTx(ctx, tx, query, args…)` — `internal/store/ranking.go`: runs a rank query and returns every row it matches, in the order the query asks for. Every caller bounds its own query — by range or by `LIMIT` — so the rows read stay proportional to the window rather than to the backlog.

`nearestRankTx(ctx, tx, query, args…)` — `internal/store/ranking.go`: returns the single rank a `LIMIT 1` query selects, mapping `sql.ErrNoRows` to `""`, the open end of the keyspace.

[LAW:one-source-of-truth] `rank.Significant` is the one definition of room here, the same one `anchorRun` compares anchors by: ranks sharing a significant part leave nothing between them, so a bound sharing the window's would leave the window nowhere to go.

Smoothing is invoked from `RankToTop`, `RankSet`, `RankToBottom`, `RankAbove`, `RankBelow`, `rankBetweenTx` (with no length threshold, when a placement's pair of bounds has no room), and `FixRankInversions` (once per rewritten rank after every repair write has landed). It ignores parent/epic frames entirely: the window is whatever is adjacent in the global rank keyspace.

### 5.8 Rank inversions

`rowQueryer` interface with just `QueryContext` so the same loaders run on `*sql.DB` and `*sql.Tx` — `internal/store/ranking.go`.

`liveIssueIDs(ctx)` — `internal/store/ranking.go`: `s.ListIssues(ctx, storage.ListIssuesFilter{Statuses: []model.State{model.StateOpen, model.StateInProgress}})`; error → `fmt.Errorf("list live issues: %w", err)`. Archived and deleted issues are excluded by that listing; epics get their state by rollup over children rather than a column peek.

`loadRankOrder(ctx, q, liveIDs)` — `internal/store/ranking.go`: `SELECT id, item_rank FROM issues WHERE deleted_at IS NULL ORDER BY item_rank ASC, id ASC`, appending only rows present in `liveIDs`; errors `fmt.Errorf("query rank order: %w", err)`, `fmt.Errorf("scan rank order: %w", err)`, `fmt.Errorf("rank order rows: %w", err)`. The `id ASC` tiebreak makes the sequence deterministic where two rows share a rank. This sequence is both the repair's input order and its membership test for which blocks edges constrain live work.

`rankedIssue{id, rank}` — `internal/store/rank_repair.go`: one row of that sequence.

`projectEdges(edges, position)` — `internal/store/rank_repair.go`: drops any edge with an endpoint outside `position` and dedupes the rest, so every surviving edge indexes in range and is counted exactly once.

`positions(order)` — `internal/store/rank_repair.go`: maps each id in `order` to its index; `stableTopoOrder`, `invertedEdges` and `blocksCycle` all index through it.

`invertedEdges(order, edges)` — `internal/store/rank_repair.go`: over `projectEdges(edges, positions(order))` keeps every edge with `position[dependency] > position[dependent]`. Pure; no DB access. Doctor counts `len(...)` of this; `FixRankInversions` produces an order for which it is empty, so the reported count and the repaired state are one predicate rather than two kept in agreement.

`blocksCycle(order, edges)` — `internal/store/rank_repair.go`: `findBlocksCycle(projectEdges(edges, positions(order)))`, the constraints `stableTopoOrder` sorts, so Doctor reports a cycle exactly when the repair refuses on one, and names the same path. Pure; no DB access.

### 5.9 Blocks graph helpers

`blocksEdge{dependent, dependency}` — `internal/store/ranking.go` (src = dependent, ranked below; dst = dependency, ranked above).

`loadBlocksEdges(ctx, q)` — `internal/store/ranking.go`:
```sql
SELECT r.src_id, r.dst_id FROM relations r
JOIN issues src ON src.id = r.src_id
JOIN issues dst ON dst.id = r.dst_id
WHERE r.type = 'blocks'
AND src.deleted_at IS NULL AND dst.deleted_at IS NULL
ORDER BY r.src_id, r.dst_id
```
No rank filter; the ORDER BY exists to make DFS adjacency order — and therefore the reported cycle path — deterministic. Errors: `"query blocks edges: %w"`, `"scan blocks edge: %w"`, `"blocks edges rows: %w"`.

`blocksPrecedenceAdj(edges)` — `internal/store/ranking.go`: adjacency `dependency -> []dependent`.

`blocksPrecedes(adj, from, to)` — `internal/store/ranking.go`: recursive DFS with a `seen` set; returns true as soon as `to` is reached. (The `seen` set is populated after the `next == to` check, so a node is compared before being marked.)

`findBlocksCycle(edges)` — `internal/store/ranking.go`: three-color DFS (`white = 0`, `gray = 1`, `black = 2`) over adjacency keys sorted with `sort.Strings`; on hitting a gray node it slices the current stack from that node and appends it again, returning a repeated-endpoint path `a -> b -> … -> a`; returns nil for an acyclic graph.

### 5.10 The rank repair

Pure and DB-free, in `internal/store/rank_repair.go`; `FixRankInversions` supplies its inputs and applies its outputs.

`rankRewrite{id, newRank}` — `internal/store/rank_repair.go`: one issue whose rank must change, and the value to write.

`blocksCycleError{path}` — `internal/store/rank_repair.go`: `Error()` renders `blocks dependency cycle <a -> b -> … -> a> — a cycle has no valid rank order; break it by removing one edge with 'lit dep rm'`.

`repairRankOrder(order, edges)` — `internal/store/rank_repair.go`: `stableTopoOrder(order, edges)`, then `rankRewrites(order, target)`.

`stableTopoOrder(order, edges)` — `internal/store/rank_repair.go`: Kahn's algorithm over `projectEdges`, with the ready set held ascending by position in `order` (`slices.Insert` at `sort.SearchInts`), so each step emits the issue that stood earliest in the backlog among those whose dependencies are all placed. An issue falls behind one that stood after it only while it waits on a dependency, so an order that already satisfies every edge comes back unchanged. Ordering by position rather than by id is what keeps the tiebreak off id spelling. When Kahn stalls (`len(sorted) < len(order)`) it returns `&blocksCycleError{path: findBlocksCycle(constraints)}` rather than the partial sequence.

`rankRewrites(order, target)` — `internal/store/rank_repair.go`: marks the indices `anchorRun` returns and walks `target` as runs of movers delimited by anchors, closing the final run at the sentinel index past the end. Each run is spaced into the open interval its neighbouring anchors bound, via `rank.SpacedRanksBetween(lower, upper, len(movers))`; an absent bound is `""`, which that primitive reads as "past that end", so a run at either extreme — and a target with no anchors at all — needs no special case. Error → `fmt.Errorf("space %d rank(s) between %q and %q: %w", len(movers), lower, upper, err)`.

`anchorRun(target, rankOf)` — `internal/store/rank_repair.go`: a longest subsequence of `target` strictly increasing by `rank.Significant` of the stored rank, by patience sort (`keys` computed once, `tails` + `prev`, `sort.Search` over `tails`). The repair does not write issues in the run, so they keep their place and their `updated_at`, and the rewrite count is the count of issues the new order actually moved; smoothing may still re-space their rank strings (see `FixRankInversions`). Movers are spaced into the gap two anchors bound, and two ranks with the same significant part (`"V"` and `"V0"`) leave none, so an anchor's significant rank must sort strictly above the one before it. It must also satisfy `rank.Valid`, which the empty significant rank of an unranked or all-zero issue does not; those issues are therefore always movers, which hands them a real rank on the way past.

`Store.FixRankInversions(ctx) (int, error)` — `internal/store/ranking.go`:
1. `liveIssueIDs(ctx)` **before** the transaction; error → `fmt.Errorf("fix rank inversions: snapshot live set: %w", err)`. The repair mutates only `item_rank`, so closure status is invariant across the write.
2. In `withMutation(ctx, "fix rank inversions", …)`:
   - `loadRankOrder(ctx, tx, liveIDs)`; error → `fmt.Errorf("fix rank inversions: %w", err)`.
   - `loadBlocksEdges(ctx, tx)`; error → `fmt.Errorf("fix rank inversions: load blocks edges: %w", err)`.
   - `repairRankOrder(order, edges)`; error → `fmt.Errorf("fix rank inversions: %w", err)`. This is the arm a dependency cycle takes.
   - `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?` per rewrite, stamped `s.clock.Now().Format(time.RFC3339Nano)`; error → `fmt.Errorf("fix rank inversions: update %s: %w", rewrite.id, err)`.
   - `smoothRanksIfNeededTx` per rewritten rank, in a second loop after every write has landed rather than interleaved with them: a pass that re-spaced a window mid-repair would move the anchor ranks the remaining placements were computed against. Error → `fmt.Errorf("fix rank inversions: smooth ranks: %w", err)`.
   - `rerankedCount = len(rewrites)`, assigned rather than accumulated, because `withStampedMutation` may re-run the function after a transient failure rolls its writes back.
3. On mutation error returns `(0, err)`; otherwise `(rerankedCount, nil)`. The count is issues whose rank was rewritten by the repair, one per issue. Smoothing may rewrite further ranks, anchors included, without changing order or `updated_at`, and those are not counted.

Test-pinned behavior in `internal/store/store_test.go`:
- One dependency blocking two dependents: Doctor reports 2 inversions before, `FixRankInversions` reports 1, 0 after —.
- A pass that creates a new inversion still resolves: 1 before, `>= 1` fixed, 0 after —.
- An epic dependency ranked below its dependent counts as 1 inversion and is fixed —.
- A closed epic dependency yields 0 inversions —.
- Deleted issues yield 0 inversions and 0 fixes —.
- A cycle injected past `AddRelation` makes `FixRankInversions` fail with a message naming the cycle —.

Test-pinned behavior in `internal/store/rank_repair_test.go`, against the pure repair with no database: a band that waits on nothing keeps its ranked order while the dependent it blocks sinks below it; one edge moves only its dependent; an already-ordered sequence produces no writes; duplicate edges change nothing; a cycle returns `blocksCycleError`; unranked issues are always movers and come back with a real rank; ranks that differ only by trailing zeros, and an all-zero rank, never both bound a gap a mover must fill. Two tests cover the same shapes through the store end to end, and a 300-trial property run over random acyclic layerings asserts the repair permutes the backlog without resizing it, leaves no inverted edge, lets an issue fall behind one that stood after it only if a dependency of it is placed no earlier than that one, and writes nothing on a second pass. That property run never reaches smoothing, so a last store test plants every rank longer than `rank.SmoothingThreshold` via `ExecRawForTest` and adds three inverted edges; one `FixRankInversions` call moves three issues, each to a rank past the threshold, and must leave the expected order with no inversions; a second call must return 0 and leave every rank byte-identical.

`seq(ids...)` builds those fixtures' ranks with a field width derived from the length of the run, because ranks compare lexicographically: a fixed two-digit width stops ascending at the tenth id, where `"100"` sorts between `"10"` and `"20"`, which violates the sorted-by-rank precondition `rankRewrites` reads its anchors under.

---

## 6. Cross-cutting notes on these four subsystems

- Every mutation in labels.go, relations.go and ranking.go runs through `s.withMutation(ctx, <label>, fn)` with labels: `"add label"`, `"remove label"`, `"replace labels"` (`internal/store/labels.go`), `"add relation"`, `"remove relation"`, `"set parent"`, `"clear parent"` (`internal/store/relations.go`), `"rank to top"`, `"rank set"`, `"rank to bottom"`, `"rank above"`, `"rank below"`, `"fix rank inversions"` (`internal/store/ranking.go`).
- Author attribution defaults to the literal `"unknown"` in `AddLabel` (`internal/store/labels.go`), `replaceLabelsTx` (`internal/store/labels.go`), `AddRelation` (`internal/store/relations.go`) and `SetParent` (`internal/store/relations.go`). `CreateIssue` uses `createdBy := "links"` for both the parent edge and the initial labels (`internal/store/store.go`).
- Every rank query filters `deleted_at IS NULL`, but they split on `item_rank != ''`: the `RankToTop`, `RankSet` and `RankToBottom` end queries and the creation placement queries include it (`internal/store/ranking.go`; `internal/store/store.go`); the `RankAbove`/`RankBelow` neighbor queries, the smoothing window, run and bound queries, and `loadRankOrder` do not (`internal/store/ranking.go`).


---

## Import / Export / Export-Delta — raw behavioral inventory

Slice: `internal/store/import_export.go`, `import_bulk.go`, `import_tree.go`, `export_delta.go` and their tests. Supporting types cited from `internal/model`, `internal/storage`, `internal/cli`, `internal/syncfile`, `internal/backup` where the shape of the artifact is defined there.

---

## 1. EXPORT

### 1.1 `Store.Export` — what is collected, in what order

`func (s *Store) Export(ctx context.Context) (model.Export, error)` — `internal/store/import_export.go`.

It performs five reads and assembles one value (`import_export.go`):

1. `s.ListIssues(ctx, storage.ListIssuesFilter{Limit: 0, IncludeArchived: true, IncludeDeleted: true})` (`import_export.go`). Limit 0 disables the cap (`capLimit` returns the slice unchanged when `limit <= 0`, `internal/store/store.go`). `IncludeArchived`/`IncludeDeleted` true means neither `i.archived_at IS NULL` nor `i.deleted_at IS NULL` is added to the WHERE clause (`store.go`), so **archived and soft-deleted issues are exported**. No other filter is set, so the SQL has no WHERE clause at all. Ordering: with no `SortBy` specs, `buildIssueOrderClause` returns `"i.item_rank ASC, i.id ASC"` (`store.go`), so **issues are ordered by rank ascending, ties broken by id ascending**.
2. `s.listAllRelations(ctx)` (`import_export.go`) — `SELECT src_id, dst_id, type, created_at, created_by FROM relations ORDER BY created_at ASC` (`store.go`). Ordered by created_at ascending only (no tiebreak).
3. `s.listAllComments(ctx)` (`import_export.go`) — `SELECT id, issue_id, body, created_at, created_by FROM comments ORDER BY created_at ASC` (`store.go`).
4. `s.listAllLabels(ctx)` (`import_export.go`) — `SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC` (`store.go`).
5. `s.ListAllEvents(ctx)` (`import_export.go`) — `queryEvents(ctx, "")`, i.e. `SELECT e.id, e.issue_id, e.action, e.reason, e.actor, e.created_at, e.stream_id, e.workspace_id, c.field, c.from_value, c.to_value FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id ORDER BY e.created_at ASC, e.id ASC, c.field ASC` (`store.go`). The per-change rows are collapsed back into `IssueEvent.Changes`, so **an event's changes are ordered by field name ascending** and events by (created_at, id).

Any read error is returned with a zero `model.Export{}` (`import_export.go`).

The returned value (`import_export.go`):

```go
model.Export{
    Version:     2,
    WorkspaceID: s.workspaceID,
    ExportedAt:  time.Now().UTC(),
    Issues:      issues, Relations: rels, Comments: comments, Labels: labels, Events: events,
}
```

`Version` is the literal `2`. `ExportedAt` is wall-clock UTC at export time.

Export does **not** re-check hydration; the comment at `import_export.go` states `hydrateIssues` guarantees every issue is hydrated, and `Issue.MarshalJSON` is the boundary that rejects partial values.

### 1.2 The `model.Export` JSON envelope

`internal/model/model.go`:

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

No `omitempty` anywhere on the envelope — all eight keys are always emitted, in this order. Slices from `Store.Export` are never nil (each list helper initializes `out := []model.X{}`, e.g. `store.go`, `1885`, `1908`; `hydrateIssues` returns `[]model.Issue{}` for zero rows, `store.go`), so empty tables serialize as `[]`, not `null`.

### 1.3 The serialized issue object

`Issue` has a custom `MarshalJSON` (`model.go`) that emits the **wire struct `issueJSON`** (`model.go`), not the in-memory `Issue` (`model.go`). The wire struct, in emission order:

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

Marshal rules (`model.go`):

- If `i.pendingHydration` → error `"issue %s requires store hydration"` (`model.go`).
- If `i.lifecycle == nil` → error `"issue %s has no hydrated lifecycle"` (`model.go`).
- `status`, `closed_at`, `resolution`, `redirect_target` are populated **only when the lifecycle exposes a Status capability** (`model.go`). Containers (`epic`) expose none, so an epic's JSON object has **no `status`, no `closed_at`, no `resolution`, no `redirect_target` keys at all**.
- `archived_at`/`deleted_at` come from `lifecycle.RetentionTimestamps(i.Retention())` (`model.go`); both omitted when nil (a Live issue).
- `Labels` has no `omitempty`, so `"labels": null` appears when the slice is nil, `[]` when empty-non-nil.
- `Priority` is `type Priority int` (`internal/model/priority.go`) with constants `PriorityNormal = 0`, `PriorityUrgent = 1` (`priority.go`) — serializes as a bare **integer**.
- `IssueType` is `type IssueType string` (`internal/model/issue_type.go`) with values `"task"`, `"feature"`, `"bug"`, `"chore"`, `"epic"` (`issue_type.go`) — serializes as a **string**.
- `State` = `lifecycle.State`, a string (`model.go`; `internal/model/lifecycle/lifecycle.go`), values `"open"`, `"in_progress"`, `"closed"`.
- `Resolution` = `lifecycle.Resolution`, a string (`model.go`; `internal/model/lifecycle/resolution.go`), values `"duplicate"`, `"superseded"`, `"obsolete"`, `"wontfix"`.
- `time.Time` fields serialize as Go's RFC3339 with nanoseconds (encoding/json default).

`model.IssueWireFields()` (`model.go`) derives the wire key list by reflecting over `issueJSON`, skipping `json:"-"` and falling back to the Go field name for an empty tag.

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

(In practice `archived_at` and `deleted_at` are mutually exclusive — `retentionColumns`/`RetentionTimestamps` cannot express both, `internal/store/store.go` — and `deleted_at` is omitted entirely rather than `null` when absent.)

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

`model.Relation` (`model.go`) — no omitempty on any field:

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

`model.Comment` (`model.go`):

```json
{"id":"cmt-...","issue_id":"links-a-1","body":"text","created_at":"2026-08-27T10:00:00Z","created_by":"tester"}
```

`model.Label` (`model.go`) — note the JSON key is `name` while the DB column is `label`:

```json
{"issue_id":"links-a-1","name":"urgent","created_at":"2026-08-27T10:00:00Z","created_by":"tester"}
```

`model.IssueEvent` (`model.go`):

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

`FieldChange` (`model.go`): `{"field":..,"from":..,"to":..}`, no omitempty.

`Attribution` (`model.go`) has unexported fields and a custom marshal via `attributionWire` (`model.go`): `{"stream":"…","workspace":"…"}` with both `omitempty`. `omitzero` on the event field means an absent pair emits **no `attribution` key at all** (`model.go` — `IsZero` is what encoding/json consults).

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

`model.go`. Decodes into a private `rawExport` that additionally accepts `"history"` (`model.go`), then copies version/workspace_id/exported_at/issues/relations/comments/labels/events across (`model.go`).

If `raw.Version < 2 && len(raw.History) > 0` (`model.go`), every `v1ExportHistory` row (`model.go`: `issue_id`, `action`, `from_status`, `to_status`, `reason`, `created_by`, `created_at`) is converted into an `IssueEvent` (`model.go`) with:
- `ID` = `v1EventID(...)` = `"evt-v1-" + hex(sha256(issueID|action|fromStatus|toStatus|createdBy|createdAt.RFC3339Nano)[:8])` — 16 hex chars after the prefix (`model.go`).
- `Actor` = the v1 `created_by`.
- `Changes` = exactly one `{"field":"status","from":<from_status>,"to":<to_status>}` (`model.go`).

These are **appended** to any already-present `events`.

Issue decode (`Issue.UnmarshalJSON`, `model.go`): fields copied straight through; `retention` from `lifecycle.RetentionFromTimestamps(archived_at, deleted_at)` (`model.go`); then a three-way dispatch (`model.go`):
- container type → `pendingHydration = true`, `lifecycle = nil` (so it cannot be re-marshaled until the store hydrates it),
- non-container with `status` present → `HydrateStatus` with `closed_at`/`resolution`/`redirect_target`,
- non-container with **no** `status` → error `"issue %s: cannot hydrate lifecycle from JSON (missing status field on non-epic)"` (`model.go`).

Attribution decode collapses a half pair to the zero value via `NewAttribution` (`model.go`): stream-without-workspace or workspace-without-stream becomes "unattributed", silently.

### 1.6 On-disk layout of exported artifacts

Three write surfaces consume `Store.Export`:

**(a) `lit export` to stdout** — `internal/cli/cli.go`. No flags other than the shared set; `writeJSON(stdout, export)` (`cli.go`), which is `json.NewEncoder(w)` with `SetIndent("", "  ")` then `Encode` (`cli.go`). So: **two-space indent, one trailing newline** (Encoder.Encode appends `\n`), single JSON object, HTML escaping on (encoder default). Comment at `cli.go`: "Export is JSON-only — there is no text representation of a full database export."

**(b) Backup snapshots** — `internal/backup/backup.go`. `Create(storageDir, export)`:
- directory `filepath.Join(storageDir, "backups")`, created with mode `0o755`.
- filename `time.Now().UTC().Format("20060102-150405.000000000") + ".json"` → e.g. `20260827-142530.123456789.json`.
- written via `syncfile.WriteAtomic`.
- returns `Snapshot{Path, Name, Created (mtime UTC), Size}` with tags `json:"path"`, `"name"`, `"created"`, `"size"`.
- `List` reads that dir, skips directories and any entry not ending in `.json`; a missing dir returns `[]Snapshot{}` and no error.
- `Prune(storageDir, keep)` errors on `keep <= 0` (test `internal/backup/backup_test.go`); `runBackupCreate` defaults `--keep` to `20` (`internal/cli/backup.go`), and `restoreFromExportPath` hardcodes `backup.Prune(dir, 20)` (`cli/backup.go`).

**(c) Sync file / last-sync base** — `internal/syncfile/syncfile.go`. `WriteAtomic(path, export)`:
- `marshalExport` = `json.MarshalIndent(export, "", "  ")` **plus a trailing `'\n'`** (`syncfile.go`).
- `os.MkdirAll(dir, 0o755)`, `os.CreateTemp(dir, ".links-sync-*.json")`, write, close, `os.Rename` onto the clean path (`syncfile.go`); the temp file is removed on any failure path via `defer`.
- returns `hashPayload(payload)` — the content hash of the bytes written.
- The sync base lives at `filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")` (`internal/cli/backup.go`).

There is **no manifest or index file**: `backup.List` derives the listing by reading the directory (`backup.go`).

`lit backup list` prints `"%s %d %s\n"` = name, size, path (`cli/backup.go`). `lit backup create` prints `"%s %s\n"` = name, path (`cli/backup.go`).

---

## 2. EXPORT DELTA (`export_delta.go`)

### 2.1 What "delta" means here

It is **not** a commit range, checkpoint, or timestamp comparison. It is a pure value diff between **two `model.Export` values held in memory**: `diffExports(prev, next model.Export) exportDelta` (`export_delta.go`). `prev` is "what the live tables currently hold"; `next` is "what they must hold". No SQL query is issued to compute it (`export_delta.go`: "pure: the SQL lives in applyExportDelta").

Who supplies `prev`:
- `writeExportTx` supplies `model.Export{}` — the empty export — after having deleted every row, making the restore the degenerate "everything is an add" case (`import_export.go`).
- `spineWriter` owns `landed`, seeded by an actual `Store.Export(ctx)` read of the spine branch in `newSpineWriter` (`internal/store/sync_reconcile.go`), and advanced to `next` only after a successful landing (`sync_reconcile.go`). The comment at `export_delta.go` states the previous export is never taken from a caller's belief.

### 2.2 The delta record types

```go
type tableDelta[K comparable, R any] struct {
	remove []K
	add    []R
}
```
`export_delta.go`. `empty()` is `len(remove)==0 && len(add)==0` (`export_delta.go`).

```go
type exportDelta struct {
	issues    tableDelta[string, model.Issue]
	relations tableDelta[relationKey, model.Relation]
	comments  tableDelta[string, model.Comment]
	labels    tableDelta[labelKey, model.Label]
	events    tableDelta[string, model.IssueEvent]
}
```
`export_delta.go`; `empty()` at `export_delta.go`. These are unexported Go values — **the delta is never serialized to disk or JSON anywhere**.

Key types (`internal/store/row_deletes.go`): `relationKey{srcID, dstID string; kind model.RelationType}` (`row_deletes.go`) = the relations PRIMARY KEY; `labelKey{issueID, name string}` (`row_deletes.go`) = labels PRIMARY KEY. Comments, issues and events key on their `string` id.

### 2.3 Add / modify / delete representation

There is **no "modify"**. `diffTable` (`export_delta.go`):

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

So: **delete** = key in `remove` only; **add** = row in `add` only; **modify** = the same key appears in BOTH `remove` and `add` (delete-then-reinsert). Comment at `export_delta.go`: this is why no UPDATE statement exists.

Comparison is `reflect.DeepEqual` over a `persisted(row)` projection:
- issues → `issueRowValues(i)` (`export_delta.go`), the normalized `[]any` row tuple, *not* the model value.
- relations, comments, labels, events → `wholeRow` (identity) (`export_delta.go`, used).

Determinism: iteration is over the input slices, not maps (`export_delta.go`), so identical inputs produce an identical statement sequence.

### 2.4 The cascade rule

`diffExports` computes the issues diff **first**, then `survivors := cascadeSurvivors(prev.Issues, issues.remove)` (`export_delta.go`). `cascadeSurvivors` (`export_delta.go`) builds `doomed` from the removal keys and returns the complement over `prev.Issues` — survival is read off the issues diff, never recomputed.

Each child table's `live` side is then `prev`'s rows **filtered to survivors** (`filterRows`, `export_delta.go`):
- relations survive iff `survivors[r.SrcID] && survivors[r.DstID]` (`export_delta.go`) — either endpoint dying kills the edge.
- comments: `survivors[c.IssueID]` (`export_delta.go`).
- labels: `survivors[l.IssueID]` (`export_delta.go`).
- events: `survivors[e.IssueID]` (`export_delta.go`).

Nested `issue_event_changes` get no layer: an event whose `Changes` differ is a changed value under `wholeRow`, so removed+re-added, and its change rows cascade with it (`export_delta.go`).

### 2.5 Application order and SQL

`applyExportDelta(ctx, tx, delta)` (`export_delta.go`) runs five table deltas in this fixed order — **issues, relations, comments, labels, events** — and within each table **all removes before all adds** (`applyTableDelta`, `export_delta.go`). The removal row count is discarded (`export_delta.go`).

Statements bound per table:

| table | delete | insert |
|---|---|---|
| issues | `DELETE FROM issues WHERE id = ?` (`row_deletes.go`) | `insertIssueStmt` (below) |
| relations | `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?` (`row_deletes.go`) | `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)` (`internal/store/relations.go`) |
| comments | `DELETE FROM comments WHERE id = ?` (`row_deletes.go`) | `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)` (`import_export.go`) |
| labels | `DELETE FROM labels WHERE issue_id = ? AND label = ?` (`row_deletes.go`) | `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)` (`import_export.go`) |
| events | `DELETE FROM issue_events WHERE id = ?` (`row_deletes.go`) | `INSERT INTO issue_events(...)` + N `INSERT INTO issue_event_changes(...)` (`import_export.go`) |

Delete error text: `execDelete` wraps as `"delete %s: %w"` and `"delete %s: rows affected: %w"` with subjects `"issue <id>"`, `"relation <src>-><dst> (<type>)"`, `"comment <id>"`, `"label <issue>:<name>"`, `"issue event <id>"` (`row_deletes.go`).

### 2.6 Where a delta is committed

`replayDeltaOnScratch(ctx, delta, stamp)` (`sync_reconcile.go`): under `withCommitLock`, `BeginTx` → `applyExportDelta` → `tx.Commit()` → `commitWorkingSetOnce(ctx, stamp)`. Deliberately a **single attempt** with no self-rotating transient retry (`sync_reconcile.go`); errors bubble to the outer scratch-rebuilding retry. Error texts: `"begin %s tx: %w"` and `"commit %s tx: %w"` with `stamp.Message` interpolated (`sync_reconcile.go`).

`spineWriter.land` does `DOLT_CHECKOUT <spine branch>` first, unconditionally (`sync_reconcile.go`, error `"switch to reconcile spine branch %q: %w"`), then `replayDeltaOnScratch(ctx, diffExports(w.landed, next), stamp)`, then advances `w.landed = next` only on success (`sync_reconcile.go`).

`commitStamp` (`internal/store/commit_lock.go`): `Message string`, `Date time.Time` (non-zero → `--date`, second-granular), `Author string` (non-empty → `--author "Name <email>"`), `AllowEmpty bool`.

### 2.7 What the delta tests pin

`internal/store/export_delta_test.go`:

- `TestExportDeltaMatchesFullRewriteAcrossEveryChangeShape` drives two stores through the same state sequence — one via `replaceFromExport(..., commitStamp{Message:"rewrite"})`, one via `applyDeltaForTest` — and after every step asserts `reflect.DeepEqual` on `Issues`, `Relations`, `Comments`, `Labels`, `Events` (`assertSameRows`). The envelope (`workspace_id`, `exported_at`) is explicitly excluded.
- The state sequence (`buildDeltaScenarioStates`), named: `"epic with two children"`, `"one field edited on one issue"`, `"comment added"`, `"label added"`, `"relation spanning two issues added"`, `"relation endpoint rewritten"`, `"issue added"`, `"issue removed outright"`. The last is synthesized by `withoutIssue` dropping the issue plus every relation touching it and every comment/label/event referencing it — the shape a merge projection yields.
- `TestExportDeltaRewritesARelationChangedOutsideItsKey`: changing only `CreatedBy` from `"first"` to `"second"` on a relation with a stable `(a,b,blocks)` key yields exactly `relations.remove == [relationKey{a,b,blocks}]` and `relations.add == [restamped]`, and **zero** issues work.
- `TestExportDeltaReinsertsChildrenOfARewrittenIssue`: retitling issue `touched` yields `issues.remove == ["touched"]`, `issues.add == [retitled]`, and for each of relations/comments/labels/events exactly **1 add and 0 removes** — including a `touched -> untouched` spanning relation.
- `TestExportDeltaLeavesAnUnchangedBacklogAlone`: `diffExports(export, export).empty()` must be true.
- `TestExportDeltaLeavesTheIssueRowAloneWhenOnlyALabelMoves`: adding label `urgent` to the hydrated `Labels` slice plus a labels row → `issues` delta empty, `labels.add == [{IssueID:"a",Name:"urgent"}]`, `labels.remove` empty, and comments/events deltas empty.
- `TestExportDeltaLeavesAnEpicsRowAloneWhenAChildCloses`: closing a child moves the epic's hydrated value but not its row → `issues.remove == ["child"]`, `issues.add == [child]`, comments (hanging off the epic) untouched.
- `TestExportDeltaDropsARemovedIssueWithoutResurrectingItsChildren`: removing issue `gone` → `issues.remove == ["gone"]`, `issues.add` empty, comments and events deltas both empty.
- Fixture helpers pin that a bare `model.Issue` literal cannot be diffed: `hydratedIssue` uses `model.HydrateStatus` and `hydratedEpic` uses `model.HydrateAllOf`, because `issueRowValues`' accessors panic on an unhydrated issue.

---

## 3. IMPORT — `ReplaceFromExport` / `writeExportTx` (`import_export.go`)

### 3.1 Entry point and transaction

`func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) error` → `s.replaceFromExport(ctx, export, commitStamp{Message: "replace from export"})` (`import_export.go`). The Dolt commit message for a restore is the literal string **`replace from export`**.

`replaceFromExport` (`import_export.go`) runs `writeExportTx` under `withStampedMutation`, i.e. under the commit lock, inside one `sql.Tx`, followed by `commitWorkingSetOnce`, with the transient-GC retry wrapping the whole staging+versioning unit (`commit_lock.go`). So it **is transactional** at the SQL level and **does commit** a Dolt commit.

### 3.2 What it does

`writeExportTx` (`import_export.go`):

```go
for _, table := range []string{"labels", "comments", "relations", "issues"} {
    if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
        return fmt.Errorf("clear %s: %w", table, err)
    }
}
return applyExportDelta(ctx, tx, diffExports(model.Export{}, export))
```

Deletion order is literally `labels, comments, relations, issues`. `issue_events` and `issue_event_changes` are deliberately **not** named — they cascade from `issues` (`import_export.go`). Error text: `"clear labels: …"`, `"clear comments: …"`, etc.

Because `prev` is the empty export, every row in the input is an add and nothing is a remove.

### 3.3 Accepted input shape

The input is a `model.Export` value. On the CLI path it arrives from `syncfile.Read(path)` → `json.Unmarshal` into `model.Export` (`internal/syncfile/syncfile.go`), i.e. the shape in §1.2 with the v1 `history` fallback of §1.5. `json.Unmarshal` here does **not** disallow unknown fields — unrecognized top-level keys are silently ignored (`syncfile.go`).

Field-by-field parsing/normalization happens in `issueRowValues` (`import_export.go`), the tuple bound to `insertIssueStmt`:

```sql
INSERT INTO issues(id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```
(`import_export.go`.)

Value-by-value (`import_export.go`):

| column | value | rule |
|---|---|---|
| `id` | `issue.ID` | verbatim |
| `title` | `issue.Title` | verbatim |
| `description` | `issue.Description` | verbatim |
| `agent_prompt` | `nullableString(issue.Prompt)` | `""` → SQL NULL (`store.go`) |
| `status` | `statusForStorage(issue)` | leaf → `sql.NullString{string(status.Value), Valid:true}`; container (no Status capability) → **NULL** (`store.go`) |
| `priority` | `model.CanonicalPriority(int(issue.Priority))` | any int ≠ 1 coerces to 0; 1 stays 1 (`priority.go`). **Never rejects** — legacy out-of-range priorities are coerced so the CHECK constraint cannot fail a restore (`import_export.go`) |
| `issue_type` | `issue.IssueType` | verbatim, no parse gate on this path |
| `topic` | `issueid.NormalizeSlug(issue.Topic)` then `COALESCE(NULLIF(?, ''), 'misc')` | lowercased, non-`[a-z0-9]` runs collapsed to single `-`, trimmed of `-` (`internal/issueid/slug.go`); an empty result becomes the literal **`misc`** |
| `assignee` | `issue.AssigneeValue()` | |
| `item_rank` | `issue.Rank` | verbatim |
| `lane` | `issue.Lane` | verbatim |
| `created_at` | `issue.CreatedAt.Format(time.RFC3339Nano)` | |
| `updated_at` | `issue.UpdatedAt.Format(time.RFC3339Nano)` | |
| `closed_at` | RFC3339Nano of `issue.ClosedAtValue()`, else nil | (`import_export.go`) |
| `resolution` | `nullableResolution(issue.ResolutionValue())` | nil → NULL, else the string (`store.go`) |
| `redirect_target` | `nullableStringPtr(issue.RedirectTargetValue())` | nil → NULL (`store.go`) |
| `archived_at`, `deleted_at` | `retentionColumns(issue)` | projected from the sealed Retention; archived-and-deleted is unrepresentable (`store.go`) |

`insertIssueTx` error text: `"restore issue %s: %w"` (`import_export.go`).

Comments (`insertCommentTx`, `import_export.go`): `id, issue_id, body, created_at (RFC3339Nano), created_by` verbatim; error `"restore comment %s: %w"`.

Labels (`insertLabelTx`, `import_export.go`): `issue_id, label(=Name), created_at (RFC3339Nano), created_by`; error `"restore label %s:%s: %w"` (issue id, name).

Events (`insertEventTx`, `import_export.go`):
- `action` binds `nil` when `event.Action == ""`, else the string (`import_export.go`).
- `created_at` RFC3339Nano.
- `stream_id` = `nullableString(event.Attribution.Stream())`, `workspace_id` = `nullableString(event.Attribution.Workspace())` — **attribution is replayed verbatim from the dump; the restoring checkout never substitutes its own** (`import_export.go`). No re-validation of the pair here: `Attribution.UnmarshalJSON` already collapsed half pairs.
- error `"restore issue event %s: %w"`.
- Then one `INSERT INTO issue_event_changes(event_id, field, from_value, to_value)` per `event.Changes` entry, with `from`/`to` through `nullableString` (`""` → NULL); error `"restore issue event change %s.%s: %w"` (event id, field).

### 3.4 ID remapping / conflict policy

**None.** IDs are written verbatim; there is no remapping, no dedup, no conflict resolution. Duplicate ids in the input reach the INSERT and fail on the primary key — `export_delta.go` states this explicitly ("the add loop walks the slice and every duplicate still reaches the INSERT and still fails loudly"). Any failure aborts the transaction (deferred `tx.Rollback`, `commit_lock.go`), so the restore is all-or-nothing.

### 3.5 The surrounding CLI restore flow

`restoreFromExportPath` (`internal/cli/backup.go`), in order: acquire `storage.Sync.Of(ap.Store)` and `storage.Import.Of(ap.Store)` capabilities up front; `syncfile.Read(restorePath)`; `ap.Store.Export(ctx)` for the local state; `syncer.GetSyncState`; if a sync state exists and `--force` was not passed, hash `last-sync-base.json` and compare against `hashExport(localExport)` — mismatch → `MergeConflictError{Message: "restore conflict: local workspace has unsynced changes since last sync base"}` (`backup.go`); `backup.Create` a pre-restore snapshot; `backup.Prune(dir, 20)`; `importer.ReplaceFromExport`; re-`Export` and `syncfile.WriteAtomic(syncBasePath(ap), restoredExport)`; `syncfile.HashFile(restorePath)`; `syncer.RecordSyncState({Path, ContentHash})`.

`hashExport` uses `json.MarshalIndent(export, "", "  ")` (`backup.go`) — note: **no trailing newline**, unlike `syncfile.marshalExport`.

Usage strings: `restoreUsage = "usage: lit backup restore (--latest | --path <export.json>) [--force]"` (`backup.go`); passing both → that string + `" — --latest and --path are mutually exclusive"` (`backup.go`); `--latest` with no snapshots → `errors.New("no backups available")` (`backup.go`).

### 3.6 Doctor / FixIntegrity (same file)

`Doctor` (`import_export.go`) initializes `DependencyCycle`, `Errors`, `Warnings` to empty slices and `IntegrityCheck = "ok"`, then:
- `CALL DOLT_VERIFY_CONSTRAINTS()` scanned into `violations`; error wrap `"verify constraints: %w"`. `violations > 0` → `IntegrityCheck = "constraint_violations"` and error line `fmt.Sprintf("constraint violations: %d", violations)`.
- Three FK-orphan counts summed into `ForeignKeyIssues` (error wrap `"count foreign key issues: %w"`):
  - `SELECT COUNT(*) FROM relations r LEFT JOIN issues s ON s.id = r.src_id LEFT JOIN issues d ON d.id = r.dst_id WHERE s.id IS NULL OR d.id IS NULL`
  - `SELECT COUNT(*) FROM comments c LEFT JOIN issues i ON i.id = c.issue_id WHERE i.id IS NULL`
  - `SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL`
  - `> 0` → error line `"foreign key violations: %d"`.
- `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id >= dst_id` → `InvalidRelatedRows`; wrap `"count invalid related rows: %w"`; warning `"invalid related-to ordering rows: %d"`.
- `SELECT COUNT(*) FROM issue_events e LEFT JOIN issues i ON i.id = e.issue_id WHERE i.id IS NULL` → `OrphanHistoryRows`; wrap `"count orphan event rows: %w"`; warning `"orphan issue event rows: %d"`.
- `s.liveIssueIDs(ctx)`, `loadRankOrder(ctx, s.db, liveIDs)` and `loadBlocksEdges(ctx, s.db)`, loaded once for both rank checks; each error wraps `"rank checks: %w"`.
- `RankInversions = len(invertedEdges(order, edges))`; warning `"rank inversions: %d (dependencies ranked below dependents)"`.
- `blocksCycle(order, edges)` non-empty → `DependencyCycle`; warning `"blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)"` with members joined by `" -> "`.

`FixIntegrity` (`import_export.go`) always runs the repair under `withMutation(ctx, "fix integrity", …)` — Dolt commit message literal **`fix integrity`** — executing exactly three statements:
```sql
DELETE FROM issue_events WHERE issue_id NOT IN (SELECT id FROM issues)   -- "repair orphan events: %w"
DELETE FROM relations WHERE type='related-to' AND src_id = dst_id        -- "repair self related rows: %w"
UPDATE relations SET src_id = dst_id, dst_id = src_id WHERE type='related-to' AND src_id > dst_id  -- "repair related ordering: %w"
```
then returns `s.Doctor(ctx)`. A mutation failure returns `storage.HealthReport{}` plus the error.

`internal/store/import_export_test.go` holds the one test over this repair. `TestFixIntegrityStampsItsCommitLabel` (`t.Parallel()`) creates a single issue and then seeds a self-referential `related-to` row through `st.db` directly (`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'related-to', ?, 'seed')`, both endpoints that issue's id) because `AddRelation` refuses a self-targeting `related-to` (`internal/store/relations.go`, `"related-to cannot target itself"`), so only a path bypassing that guard leaves the row this repair deletes. The seed is committed on its own (`st.commitWorkingSetOnce(ctx, commitStamp{Message: "seed self related-to"})`) before `st.FixIntegrity(ctx)` runs. Three things are pinned after it: `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id = dst_id` is 0; `commit_hash` from `dolt_log('HEAD')` differs from the pre-repair read (both via helper `headCommitHash`); and that commit's message from `dolt_log('HEAD')` equals `fix integrity`, the literal at `import_export.go`. The HEAD-moved assertion is what makes the message assertion mean anything: `withMutation` builds `commitStamp{Message: message}` and nothing else (`internal/store/commit_lock.go`), so with `AllowEmpty` unset a repair that produces no diff lands no commit and HEAD still carries the message of whatever preceded it — committing the seed first is what makes the repair a real diff, since otherwise the seed's insert and the repair's delete cancel inside one working set and the message assertion reads a commit the repair never wrote.

---

## 4. IMPORT TREE (`import_tree.go`)

### 4.1 The input document

`storage.ImportTreeSpec` (`internal/storage/specs.go` is the parser; the type is `internal/storage/bulk.go`):

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

The file is **one JSON array of these objects** (`ParseImportTreeSpecs`, `storage/specs.go`):
- `json.NewDecoder` with `DisallowUnknownFields()` — any key not listed above is an error, wrapped as `"import: parse spec: %w"` around a `ValidationError` carrying the decoder's message. A top-level value that is not an array is instead reported by its JSON kind, with a pointer to `lit backup restore` for an export (`treeSpecRefusal`).
- After decoding, `dec.More()` → `ValidationError{Message: "import: unexpected trailing data after spec array"}`.
- Every parse refusal is a `storage.ValidationError`, so it exits 3 with the `validation_refused` remediation.

Hand-writable example (pinned verbatim by `import_tree_test.go`):

```json
[
	{"local_id":"e1","title":"Epic","type":"epic","topic":"tree","priority":0},
	{"local_id":"t1","title":"First","type":"task","topic":"tree","priority":0,"parent":"e1"},
	{"local_id":"t2","title":"Second","type":"task","topic":"tree","priority":0,"parent":"e1","depends_on":["t1"]}
]
```

Defaults for absent fields: every field is a non-pointer, so an absent key is the Go zero value. `priority` absent → `0` → `PriorityNormal` (and `ParsePriority(0)` succeeds). `description`/`prompt`/`assignee`/`parent` absent → `""`. `labels`/`depends_on` absent → nil slice. **`title`, `type`, `topic` and `local_id` have no defaults** — absent `local_id`/`title`/`type` are rejected by validation; absent `topic` is `""` and is *not* rejected here (it reaches `CreateIssue`).

### 4.2 "Tree" = local_id graph over parent + depends_on

`ImportTree(ctx, prefix, specs)` (`import_tree.go`):

1. `validateImportTreeSpecs(specs)` — full pre-flight, no store writes.
2. `topoSortImportSpecs(specs)` → indices.
3. Create loop in topo order (`import_tree.go`):
   - `parentID = idMap[spec.Parent]` when `spec.Parent != ""` — resolved **only** from this batch's map, so an external/real parent id resolves to `""` (see §4.5).
   - `model.ParseIssueType(spec.IssueType)` — error `"import: spec %q: %w"` (local id).
   - `model.ParsePriority(spec.Priority)` — error `"import: spec %q: %w"`.
   - `s.CreateIssue(ctx, storage.CreateIssueInput{Title, Description, Prompt, IssueType, Topic, Priority, Assignee, Labels, ParentID, Prefix})`. **`Lane` is never set** on this path (contrast bulk). `Placement` is left at its zero value = `RankBottom` (`internal/storage/issues.go`), so creates append in file order.
   - On error: `leaked := s.rollbackCreatedIssues(ctx, createdIDs)` then `"import: create %q: %w (rollback leaked %d: %s)"` with the leaked ids comma-joined.
   - `idMap[spec.LocalID] = issue.ID`; append to `createdIDs`.
4. Second pass over `specs` **in file order** (not topo order) wiring `depends_on` (`import_tree.go`): for each dep, `AddRelation(storage.AddRelationInput{SrcID: idMap[spec.LocalID], DstID: idMap[dep], Type: "blocks", CreatedBy: "links"})`. The convention is stated at `import_tree.go`: **src is the dependent, dst is the dependency**. On error: rollback, then `"import: depends_on %q -> %q: %w (rollback leaked %d: %s)"`.
5. Returns `storage.ImportTreeResult{IDMap: idMap}` (`import_tree.go`), whose field tag is `json:"id_map"` (`storage/bulk.go`).

### 4.3 Validation and every rejection text

`validateImportTreeSpecs` (`import_tree.go`). First pass, per spec index `i`:

| condition | error |
|---|---|
| `len(specs) == 0` | `import: no issues in input` |
| `strings.TrimSpace(LocalID) == ""` | `import: spec %d missing local_id` |
| `LocalID != TrimSpace(LocalID)` | `import: spec %d local_id %q has surrounding whitespace` |
| `strings.TrimSpace(Title) == ""` | `import: spec %q missing title` (local id) |
| `ParseIssueType` fails | `import: spec %q has invalid type %q` |
| `ParsePriority` fails | `import: spec %q has invalid priority %d` |
| local_id already seen | `import: duplicate local_id %q` |

Second pass, over all specs (`import_tree.go`) — i.e. **forward references are legal**, because references are checked against the complete `seen` set built in the first pass:

| condition | error |
|---|---|
| `Parent != TrimSpace(Parent)` | `import: spec %q parent %q has surrounding whitespace` |
| `Parent` not in `seen` | `import: spec %q references missing parent %q` |
| a `dep != TrimSpace(dep)` | `import: spec %q depends_on entry %q has surrounding whitespace` |
| `dep` not in `seen` | `import: spec %q references missing depends_on %q` |
| `dep == spec.LocalID` | `import: spec %q cannot depend on itself` |

Consequence: on the tree path **every parent/depends_on reference must be internal to the file** — naming a pre-existing real issue id is rejected as "missing parent". (Test `TestImportTreeRejectsMissingReference`, `import_tree_test.go`, uses `parent:"ghost"` and asserts the error contains `"missing parent"`.)

### 4.4 Topological order and cycles

`topoSortImportSpecs` (`import_tree.go`) flattens the specs into three parallel slices and calls `topoSortLocalGraph`, wrapping any error as `"import: %w"`.

`topoSortLocalGraph(localID, parent, dependsOn)` (`import_tree.go`) — shared with `BulkApply`:
- builds `indexByLocal`, **skipping entries whose localID is `""`** (`import_tree.go`) — an empty local id is never a referable name.
- three-state DFS with literal constants `stateUnvisited = 0`, `stateVisiting = 1`, `stateDone = 2` (`import_tree.go`).
- `visit(i)`: `stateDone` → return; `stateVisiting` → `fmt.Errorf("cycle detected involving %q", localID[i])`; otherwise mark visiting, recurse into `parent[i]` if non-empty **and** present in the map, then into each `dependsOn[i]` entry present in the map, mark done, append `i` to `order`.
- the outer loop visits indices `0..n-1` in file order (`import_tree.go`), so the emitted order is post-order DFS seeded by file order — an unconstrained batch keeps file order.
- **References that match no localID are simply not edges** (`import_tree.go`); they neither create an ordering constraint nor an error at this layer.

Test `TestImportTreeRejectsCycle` (`import_tree_test.go`) uses `a depends_on b`, `b depends_on a` and asserts the error contains `"cycle"`.

### 4.5 Rollback

`rollbackCreatedIssues` (`import_tree.go`), shared by ImportTree and BulkApply: for each created real id, `s.Apply(ctx, realID, storage.Change{Action: model.Delete{}, Actor: "links", Reason: "import rollback"})`. Ids whose Apply fails are collected and returned as `leaked`. **It is a soft delete (`model.Delete{}` stamping `deleted_at`), not a row removal.** `leaked` is initialized to `[]string{}`, so the `%d`/`%s` in error messages read `0`/`` on a clean rollback. The caller returns the original error, decorated (`import_tree.go`).

Atomicity: best-effort only. Doc comment `import_tree.go` states partial state may remain and the error names every dangling step; the surviving surface is `lit doctor`.

### 4.6 CLI surface

`lit import --path <file>` (`internal/cli/cli.go`). `importUsage = "usage: lit import --path <tree-spec.json | bulk-file.yaml> (run `lit import --help` for both formats)"` (`cli.go`) — raised for an empty `--path` or any positional argument. The file is read with `os.ReadFile`, error `"read import spec: %w"`. Dispatch is on `strings.ToLower(filepath.Ext(path))`: `.yaml`/`.yml` → bulk; **anything else** (including `.json` and no extension) → tree JSON (`cli.go`).

On the JSON branch, a set `--by` flag is an error: `"usage: --by only applies to a YAML bulk-update file (--path *.yaml|*.yml); JSON tree-spec import always attributes creates to \"links\""` (`cli.go`).

Output (`runImportTreeJSON`, `cli.go`): `"imported %d issues\n"` with `len(result.IDMap)`, then one line per map entry `"  %s -> %s\n"` — **iterated over a Go map, so the mapping lines are in nondeterministic order**.

---

## 5. BULK IMPORT (`import_bulk.go`)

### 5.1 Input format

`storage.BulkIssueSpec` (`internal/storage/bulk.go`) — **YAML**, one document per issue, documents separated by `---`:

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

Pointer fields carry the patch distinction: nil = "leave unchanged / unspecified", set = "write this value" (`bulk.go`).

`ParseBulkSpecs` (`internal/storage/specs.go`): `yaml.NewDecoder` with `dec.KnownFields(true)` — unknown keys are an error. It loops `dec.Decode(&spec)` until `io.EOF`, appending each document; any other error → `"bulk: parse spec: %w"` around a `ValidationError` carrying the decoder's message, so it exits 3 with the `validation_refused` remediation. **A file with zero documents parses to a nil slice**, which `validateBulkSpecs` then rejects.

Example (from `cli.go`):

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

The full accept/reject enumeration is written out at `import_bulk.go` and enforced by `validateBulkSpecs` (`import_bulk.go`).

Per-document checks that run for **both** shapes (`import_bulk.go`):

| condition | error |
|---|---|
| `len(specs) == 0` | `bulk: no issues in input` |
| `ID != TrimSpace(ID)` | `bulk: doc %d id %q has surrounding whitespace` |
| `LocalID != TrimSpace(LocalID)` | `bulk: doc %d local_id %q has surrounding whitespace` |
| `Parent != TrimSpace(Parent)` | `bulk: doc %d parent %q has surrounding whitespace` |
| a `dep != TrimSpace(dep)` | `bulk: doc %d depends_on entry %q has surrounding whitespace` |
| `LocalID != "" && dep == LocalID` | `bulk: doc %d (local_id %q) cannot depend on itself` |

Update-document checks (`validateBulkUpdateDoc`, `import_bulk.go`), in order:

| condition | error |
|---|---|
| `LocalID != ""` | `bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets` |
| `Topic != nil` | `bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it` |
| `Parent != ""` | ``bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead`` |
| `len(DependsOn) > 0` | ``bulk: doc %d (id %q) sets depends_on; wire dependencies with `lit dep add` instead`` |
| `IssueType` set and unparseable | `bulk: doc %d (id %q) has invalid type %q` |
| `Priority` set and unparseable | `bulk: doc %d (id %q) has invalid priority %d` |
| `!bulkUpdateHasField(spec)` | `bulk: doc %d (id %q) has no fields to update` |
| id already seen | `bulk: duplicate id %q` (`import_bulk.go`) |

`bulkUpdateHasField` (`import_bulk.go`) is true iff any of `Title, Description, Prompt, IssueType, Priority, Assignee, Labels, Lane` is non-nil. **`Reason` alone does not count** — hence "id set and reason set with no other field set" is rejected.

Create-document checks (`validateBulkCreateDoc`, `import_bulk.go`), in order:

| condition | error |
|---|---|
| `Title == nil` or trims to `""` | `bulk: doc %d missing title` |
| `Topic == nil` or trims to `""` | `bulk: doc %d missing topic` |
| `IssueType == nil` | `bulk: doc %d missing type` |
| `ParseIssueType` fails | `bulk: doc %d has invalid type %q` |
| `Priority` set and `ParsePriority` fails | `bulk: doc %d has invalid priority %d` |
| `Reason != ""` | `bulk: doc %d sets reason without id (reason only applies to updates)` |
| local_id already seen (non-empty) | `bulk: duplicate local_id %q` (`import_bulk.go`) |

Note the create branch does **not** require `LocalID` (unlike ImportTree), and does not reject an unresolvable `Parent`/`depends_on` — those pass through as presumed real ids.

### 5.3 Execution

`BulkApply(ctx, prefix, actor string, specs)` (`import_bulk.go`):

1. `validateBulkSpecs` — whole file validated before anything is written (`import_bulk.go`).
2. Flatten to `localID/parent/dependsOn` slices and `topoSortLocalGraph` (`import_bulk.go`); error wrapped as `"bulk: %w"` (so a cycle reads `bulk: cycle detected involving "x"`).
3. `result := storage.BulkApplyResult{Created: map[string]string{}}`; `createdRealID` (index-parallel), `createdIDs` (ordered), `localRealID` map (`import_bulk.go`).
4. Loop in topo order (`import_bulk.go`):
   - **Update branch** (`spec.ID != ""`): `bulkUpdateChange(spec, actor)` then `s.Apply(ctx, spec.ID, change)`. `bulkUpdateChange` error is returned **without** a rollback (`import_bulk.go`). `Apply` error → rollback then `"bulk: update %q: %w (rollback leaked %d: %s)"`. On success append `issue.ID` to `result.Updated`.
   - **Create branch**: re-parse type (`"bulk: doc %d: %w"`, no rollback, `import_bulk.go`); priority defaults to `model.PriorityNormal` and is re-parsed only if set (`"bulk: doc %d: %w"`, no rollback); then `CreateIssue` with `Title: TrimSpace(*spec.Title)`, `Description: derefOr(spec.Description, "")`, `Prompt: derefOr(spec.Prompt, "")`, `IssueType`, `Topic: TrimSpace(*spec.Topic)`, `ParentID: resolveBulkRef(spec.Parent, localRealID)`, `Priority`, `Assignee: derefOr(spec.Assignee, "")`, `Lane: derefOr(spec.Lane, "")`, `Labels: derefOr(spec.Labels, nil)`, `Prefix: prefix` (`import_bulk.go`). `Placement` deliberately left at its zero value `RankBottom` so file order is preserved, matching ImportTree (`import_bulk.go`). Failure → rollback then `"bulk: create doc %d: %w (rollback leaked %d: %s)"`.
   - Record `createdRealID[idx]`, append `createdIDs`. If `spec.LocalID != ""` → `localRealID[LocalID] = issue.ID` and `result.Created[LocalID] = issue.ID`; **else `result.Created[issue.ID] = issue.ID`** (self-keyed) (`import_bulk.go`).
5. Second pass over `specs` in **file order**, skipping update docs (`import_bulk.go`): for each `dep`, `AddRelation({SrcID: createdRealID[i], DstID: resolveBulkRef(dep, localRealID), Type: "blocks", CreatedBy: "links"})`. Failure → rollback then `"bulk: depends_on doc %d -> %q: %w (rollback leaked %d: %s)"`.

`derefOr[T](p *T, fallback T) T` (`import_bulk.go`) is the nil-to-default helper.

`resolveBulkRef(ref, localRealID)` (`import_bulk.go`): a map hit returns the real id; **a miss returns `ref` unchanged**, to be validated downstream by `CreateIssue`/`AddRelation` as a real pre-existing issue id.

`bulkUpdateChange(spec, actor)` (`import_bulk.go`) builds `storage.Change{Actor: actor, Fields: storage.UpdateIssueInput{...}}` with `Reason: strings.TrimSpace(spec.Reason)`. Each set pointer is copied into a fresh local and its address taken; `Title`, `Description`, `Prompt`, `Assignee`, `Lane` are `strings.TrimSpace`'d; `IssueType` and `Priority` go through `ParseIssueType`/`ParsePriority` again with errors `"bulk: update %q: %w"`; `Labels` is copied by value (`v := *spec.Labels; fields.Labels = &v`). **`Topic` is never carried** — it is unrepresentable in `UpdateIssueInput` (`internal/storage/issues.go`).

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

**There is no batching.** Every document is applied through the ordinary per-issue `CreateIssue`/`Apply`/`AddRelation` calls one at a time (`import_bulk.go`) — each of which is its own `withMutation` transaction and its own Dolt commit. There are no literal batch-size constants anywhere in these files, and `BulkApply`/`ImportTree` open no transaction of their own.

**No progress reporting** exists at the store layer; the CLI prints only after the whole call returns (`cli.go`).

**Partial failure**: not transactional. On a mid-batch error, `rollbackCreatedIssues` best-effort soft-deletes only the issues **created in this call** — never updated ones, which have "no prior create to unwind" (`import_bulk.go`). Ids that fail to roll back are named in the error as `(rollback leaked %d: %s)`. Updates that already landed **stay applied**. The doc comment directs the operator to `lit doctor` after a failed batch (`import_bulk.go`).

### 5.6 CLI surface for bulk

`runImportBulk` (`cli.go`): parse; if `--by` was set but no document has an `id` → `UsageError{Message: "usage: --by only applies when the file has at least one update document (a document with `id` set); this file has none"}` (`cli.go`), determined by `bulkSpecsHaveUpdate` (`cli.go`). Then `ap.Store.BulkApply(ctx, ap.Workspace.IssuePrefix.Value(), actor, specs)`.

Output, in order (`cli.go`):
```
created %d issues\n         (len(result.Created))
  %s -> %s\n                (per Created entry — map iteration, nondeterministic order)
updated %d issues\n         (len(result.Updated))
  %s\n                      (per Updated id, in apply order)
```

### 5.7 What the bulk tests pin

`internal/store/import_bulk_test.go`:

- `TestBulkApplyCreatesEpicWithChildAndDep`: three specs (`e1` epic, `t1` task parent e1, `t2` task parent e1 depends_on t1) → `len(result.Created) == 3`; `t2`'s detail has `Parent.ID == Created["e1"]` and `DependsOn` contains `Created["t1"]`.
- `TestBulkApplyCreatesLandInFileOrder`: two specs with no placement → `first.Rank < second.Rank`.
- `TestBulkApplyCreateWithoutLocalIDIsReportedByRealID`: one create with no `local_id` → the single `Created` entry is self-keyed (`ref == real`).
- `TestBulkApplyUpdatesExistingIssueByID`: `{ID, Title:"After"}` → `Updated == [id]` and the stored title is `"After"`.
- `TestBulkApplyMixedCreateAndUpdate`: one create + one update → `len(Created)==1 && len(Updated)==1`.
- `TestBulkApplyRejectsUnknownID`: `id: "ghost-1"` → error contains `"not found"` (raised by `Apply`, not by validation).
- `TestBulkApplyRejectsUpdateWithNoFields`: error contains `"no fields to update"`.
- `TestBulkApplyRejectsUpdateWithTopic`: error contains `"immutable"`.
- `TestBulkApplyRejectsUpdateWithParentOrDependsOn`: error contains `"lit parent set"`.
- `TestBulkApplyRejectsInvalidTypeOrPriorityOnUpdate`: `type:"ghost"` → contains `"invalid type"`; `priority: 7` → contains `"invalid priority"`.
- `TestBulkApplyRejectsMissingCreateFields`: title only → contains `"missing topic"`.
- `TestBulkApplyRejectsDuplicateID`: contains `"duplicate id"`.
- `TestBulkApplyRejectsIDAndLocalIDTogether`: contains `"local_id"`.
- `TestBulkApplyCreateChildOfExistingIssue`: `parent: <real epic id>` (matching no local_id) resolves as an external reference and the created child's `Parent.ID` is that epic.
- `TestBulkApplyRollsBackCreatesOnLaterFailure`: doc `a` creates, doc `b` has `parent:"ghost-does-not-exist"` which passes validation and fails inside `CreateIssue`; afterwards a default `ListIssues` (which excludes deleted) must not contain doc `a`'s title — pinning that the rollback's soft delete removes it from the default listing.
- `TestBulkApplyRejectsEmptyInput`: `nil` specs → contains `"no issues in input"`.

### 5.8 Tree-import test assertions

`internal/store/import_tree_test.go`: `TestImportTreeCreatesEpicWithChildAndDep` pins `len(IDMap)==3`, t2's parent = e1's real id, t2 depends on t1. `TestImportTreeRejectsCycle` → `"cycle"`. `TestImportTreeRejectsMissingReference` → `"missing parent"`. `TestImportTreeRejectsInvalidType` → `"invalid type"`. `TestParseImportTreeSpecsValidFlatFormImports` round-trips the literal JSON array quoted in §4.1 through `storage.ParseImportTreeSpecs` and then `ImportTree`, asserting the same wiring.

---

## 6. Constants and literal values appearing in this slice

| constant / literal | value | site |
|---|---|---|
| export `Version` | `2` | `import_export.go` |
| restore Dolt commit message | `"replace from export"` | `import_export.go` |
| `FixIntegrity` Dolt commit message | `"fix integrity"` | `import_export.go` |
| Doctor healthy `IntegrityCheck` | `"ok"` | `import_export.go` |
| Doctor failing `IntegrityCheck` | `"constraint_violations"` | `import_export.go` |
| clear order in `writeExportTx` | `labels, comments, relations, issues` | `import_export.go` |
| `insertIssueStmt` topic default | `COALESCE(NULLIF(?, ''), 'misc')` → `misc` | `import_export.go` |
| relation type written by both importers | `"blocks"` | `import_bulk.go`, `import_tree.go` |
| relation `CreatedBy` written by both importers | `"links"` | `import_bulk.go`, `import_tree.go` |
| rollback Change actor / reason | `"links"` / `"import rollback"` | `import_tree.go` |
| `CreateIssue` createdBy | `"links"` | `internal/store/store.go` |
| `stateUnvisited / stateVisiting / stateDone` | `0 / 1 / 2` | `import_tree.go` |
| create `Placement` default | `RankBottom` (iota 0) | `internal/storage/issues.go` |
| bulk create priority default | `model.PriorityNormal` (0) | `import_bulk.go` |
| backup dir | `<StorageDir>/backups` | `internal/backup/backup.go` |
| backup filename format | `20060102-150405.000000000` + `.json` | `internal/backup/backup.go` |
| backup dir mode | `0o755` | `internal/backup/backup.go` |
| sync-base path | `<StorageDir>/last-sync-base.json` | `internal/cli/backup.go` |
| syncfile temp pattern | `.links-sync-*.json` | `internal/syncfile/syncfile.go` |
| JSON indent (export/stdout, syncfile, hashExport) | `"", "  "` (two spaces) | `cli.go`, `syncfile.go`, `cli/backup.go` |
| default `--keep` for backup create / restore prune | `20` / `20` | `cli/backup.go` |

## 7. Cross-cutting error-message index for this slice

`import_export.go`: `verify constraints: %w`, `count foreign key issues: %w`, `count invalid related rows: %w`, `count orphan event rows: %w`, `count rank inversions: %w`, `detect blocks dependency cycle: %w`, `constraint violations: %d`, `foreign key violations: %d`, `invalid related-to ordering rows: %d`, `orphan issue event rows: %d`, `rank inversions: %d (dependencies ranked below dependents)`, `blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)`, `repair orphan events: %w`, `repair self related rows: %w`, `repair related ordering: %w`, `clear %s: %w`, `restore issue %s: %w`, `restore comment %s: %w`, `restore label %s:%s: %w`, `restore issue event %s: %w`, `restore issue event change %s.%s: %w`.

`import_tree.go`: `import: no issues in input`, `import: spec %d missing local_id`, `import: spec %d local_id %q has surrounding whitespace`, `import: spec %q missing title`, `import: spec %q has invalid type %q`, `import: spec %q has invalid priority %d`, `import: duplicate local_id %q`, `import: spec %q parent %q has surrounding whitespace`, `import: spec %q references missing parent %q`, `import: spec %q depends_on entry %q has surrounding whitespace`, `import: spec %q references missing depends_on %q`, `import: spec %q cannot depend on itself`, `import: spec %q: %w`, `import: create %q: %w (rollback leaked %d: %s)`, `import: depends_on %q -> %q: %w (rollback leaked %d: %s)`, `import: %w`, `cycle detected involving %q`.

`import_bulk.go`: `bulk: no issues in input`, `bulk: doc %d id %q has surrounding whitespace`, `bulk: doc %d local_id %q has surrounding whitespace`, `bulk: doc %d parent %q has surrounding whitespace`, `bulk: doc %d depends_on entry %q has surrounding whitespace`, `bulk: doc %d (local_id %q) cannot depend on itself`, `bulk: duplicate id %q`, `bulk: duplicate local_id %q`, `bulk: doc %d missing title`, `bulk: doc %d missing topic`, `bulk: doc %d missing type`, `bulk: doc %d has invalid type %q`, `bulk: doc %d has invalid priority %d`, `bulk: doc %d sets reason without id (reason only applies to updates)`, `bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets`, `bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it`, ``bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead``, ``bulk: doc %d (id %q) sets depends_on; wire dependencies with `lit dep add` instead``, `bulk: doc %d (id %q) has invalid type %q`, `bulk: doc %d (id %q) has invalid priority %d`, `bulk: doc %d (id %q) has no fields to update`, `bulk: %w`, `bulk: doc %d: %w`, `bulk: update %q: %w`, `bulk: update %q: %w (rollback leaked %d: %s)`, `bulk: create doc %d: %w (rollback leaked %d: %s)`, `bulk: depends_on doc %d -> %q: %w (rollback leaked %d: %s)`.

Parsers (`internal/storage/specs.go`): `bulk: parse spec: %w`, `import: parse spec: %w`, `import: unexpected trailing data after spec array`.

Shared parse gates: `issue type must be task, feature, bug, chore, or epic` (`internal/model/issue_type.go` + `oxfordOr`), `priority must be normal (0) or urgent (1)` (`internal/model/priority.go`, built from `priorityTokens()` through the same `oxfordOr`, and returned by both `ParsePriority` and `ParsePriorityName`).


---

## Behavioral inventory: `internal/store` — verify, recover, rawdump, row_deletes, checkpoint

All paths are relative to `/Users/bmf/code/links-issue-tracker`. Every claim carries a `file:line` citation. Derived from Go source and `_test.go` files only.

---

## 1. VERIFY (`internal/store/verify.go`, 372 lines; `verify_test.go`, 244 lines)

### 1.1 Types and constants

`ConservationLaw` is a `string` type (`internal/store/verify.go`). Its four literal values:

| Go const | Literal value | Declared at |
|---|---|---|
| `LawHealth` | `"health"` | `internal/store/verify.go` |
| `LawCount` | `"count"` | `internal/store/verify.go` |
| `LawIDStability` | `"id_stability"` | `internal/store/verify.go` |
| `LawRank` | `"rank_permutation"` | `internal/store/verify.go` |

`VerifyFinding` — exactly two fields (`internal/store/verify.go`):
- `Law ConservationLaw` with JSON tag `"law"` (`verify.go`)
- `Detail string` with JSON tag `"detail"` (`verify.go`)

`VerifyReport` — exactly one field (`internal/store/verify.go`):
- `Findings []VerifyFinding` with JSON tag `"findings"` (`verify.go`)

There is no separate boolean/status field; `Reconciled()` is derived: `return len(r.Findings) == 0` (`internal/store/verify.go`).

`conservationCollections` is the fixed report order (`internal/store/verify.go`):
`collIssues, collRelations, collComments, collLabels, collEvents, collEventChanges`.
The underlying collection literals (`internal/store/shapemap.go`): `collIssues = "issues"`, `collRelations = "relations"`, `collComments = "comments"`, `collLabels = "labels"`, `collEvents = "events"`, `collEventChanges = "event_changes"`.

### 1.2 Report rendering — `VerifyReport.String()` (`verify.go`)

- Reconciled case returns exactly (`verify.go`):
  `verify: reconciled — Doctor-clean and all conservation laws hold`
- Otherwise, header line (`verify.go`):
  `verify: %d discrepancy(ies) — the rebuild does not conserve the source and cannot be trusted:\n` where `%d` = `len(r.Findings)`
- Then one line per finding (`verify.go`):
  `  %d. [%s] %s\n` = 1-based index, the law string, the detail. Two leading spaces.

### 1.3 `VerifyCandidate` — entry point and error paths (`verify.go`)

Signature: `VerifyCandidate(ctx context.Context, dump RawDump, mapping ShapeMapping, st *Store) (VerifyReport, error)` (`verify.go`).

Order of operations:
1. `st.Doctor(ctx)` (`verify.go`). On error returns a **zero** `VerifyReport{}` and `fmt.Errorf("verify health gate (doctor): %w", err)` (`verify.go`).
2. `st.Export(ctx)` (`verify.go`). On error returns zero report and `fmt.Errorf("verify conservation gate (export): %w", err)` (`verify.go`).
3. Findings accumulate, in this fixed order, with no early exit (`verify.go`):
   - `healthFindings(health)` (`verify.go`)
   - `countFindings(dump, mapping, export)` (`verify.go`)
   - `idStabilityFindings(dump, mapping, export)` (`verify.go`)
   - `rankFindings(export)` (`verify.go`)
4. Returns `VerifyReport{Findings: findings}, nil` (`verify.go`).

**No repair-on-verify.** `VerifyCandidate` performs no writes: it calls only `Doctor` (read-only queries — `internal/store/import_export.go`) and `Export` (read-only lists — `import_export.go`). The repair sibling `FixIntegrity` (`import_export.go`) exists but is never called from verify.go. There is no re-validation of the mapping — the comment at `verify.go` states the gate deliberately does not re-run `Validate`.

Findings are **all treated identically** — any finding at all makes `Reconciled()` false (`verify.go`). There is no advisory/warning tier inside the report: the only severity filtering happens when folding Doctor's output (§1.4).

### 1.4 Check family 1 — HEALTH (`healthFindings`, `verify.go`)

- Input is `storage.HealthReport` (`internal/storage/maintenance.go`), whose fields are:
  `IntegrityCheck string` (json `integrity_check`), `ForeignKeyIssues int` (`foreign_key_issues`), `InvalidRelatedRows int` (`invalid_related_rows`), `OrphanHistoryRows int` (`orphan_history_rows`), `RankInversions int` (`rank_inversions`), `DependencyCycle []string` (`dependency_cycle`), `ParentCycle []string` (`parent_cycle`), `Unchecked []string` (`unchecked`), `Errors []string` (`errors`), `Warnings []string` (`warnings`).
- Only `h.Errors` become findings; each error string becomes `VerifyFinding{Law: LawHealth, Detail: e}` verbatim (`verify.go`).
- `h.Warnings` are **discarded** — explicitly, so a faithful rebuild of messy source is not rejected (`verify.go`). This is the one advisory-vs-fatal split in the whole gate.
- The slice is preallocated `make([]VerifyFinding, 0, len(h.Errors))` — non-nil even when empty (`verify.go`).

The concrete checks whose text can appear as a `health` finding, in `Doctor`'s execution order (`internal/store/import_export.go`):

1. **Constraint verification.** SQL: `CALL DOLT_VERIFY_CONSTRAINTS()` scanned into an int (`import_export.go`). Query error → `Doctor` returns `fmt.Errorf("verify constraints: %w", err)` (`import_export.go`), which `VerifyCandidate` wraps as `verify health gate (doctor): ...`. If `violations > 0`: sets `IntegrityCheck = "constraint_violations"` and appends **error** `constraint violations: %d` (`import_export.go`). Default `IntegrityCheck` is `"ok"` (`import_export.go`).
2. **Foreign-key counts** — three queries summed into `ForeignKeyIssues` (`import_export.go`):
   - `SELECT COUNT(*) FROM relations r LEFT JOIN issues s ON s.id = r.src_id LEFT JOIN issues d ON d.id = r.dst_id WHERE s.id IS NULL OR d.id IS NULL` (`import_export.go`)
   - `SELECT COUNT(*) FROM comments c LEFT JOIN issues i ON i.id = c.issue_id WHERE i.id IS NULL` (`import_export.go`)
   - `SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL` (`import_export.go`)
   Query error → `fmt.Errorf("count foreign key issues: %w", err)` (`import_export.go`). If sum > 0, appends **error** `foreign key violations: %d` (`import_export.go`).
3. **Invalid related-to ordering.** SQL: `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id >= dst_id` (`import_export.go`). Error → `count invalid related rows: %w`. If > 0, appends **warning** `invalid related-to ordering rows: %d` (`import_export.go`) — a warning, therefore **ignored by verify**.
4. **Orphan event rows.** SQL: `SELECT COUNT(*) FROM issue_events e LEFT JOIN issues i ON i.id = e.issue_id WHERE i.id IS NULL` (`import_export.go`). Error → `count orphan event rows: %w`. If > 0, **warning** `orphan issue event rows: %d` (`import_export.go`) — ignored by verify.
5. **Rank inversions.** Computed in Go: `s.liveIssueIDs(ctx)`, `loadRankOrder(ctx, s.db, liveIDs)` and `loadBlocksEdges(ctx, s.db)` load once for this check and the next (`import_export.go`); each error → `rank checks: %w`. `RankInversions = len(invertedEdges(order, edges))` (`import_export.go`); if > 0, **warning** `rank inversions: %d (dependencies ranked below dependents)` (`import_export.go`) — ignored by verify.
6. **Blocks dependency cycle.** `blocksCycle(order, edges)` over the same load (`import_export.go`). If non-empty, sets `DependencyCycle` and appends **warning** `blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)` with members joined by `" -> "` (`import_export.go`) — ignored by verify.

Net effect: only checks 1 and 2 (constraint violations, foreign-key violations) can ever fail the verify health half.

### 1.5 Check family 2 — COUNT conservation (`countFindings`, `verify.go`)

Expected counts are computed from the **raw dump** row counts, never re-derived through the mapping:
- `mapTables := tablesByName(m)` (`verify.go`; helper at `shapemap.go`, last-wins index by table name).
- Every collection in `conservationCollections` starts `exact[c] = true` (`verify.go`).
- For each dumped table, for each emitter of that table's mapping (`verify.go`):
  - If `em.When` type-asserts to `Always` → `expected[em.Collection] += len(table.Rows)` (`verify.go`).
  - Otherwise (any conditional `When`) → `exact[em.Collection] = false`, permanently excluding that collection from the count law (`verify.go`).
- Actual counts read from `model.Export` (`verify.go`): `collIssues = len(export.Issues)`, `collRelations = len(export.Relations)`, `collComments = len(export.Comments)`, `collLabels = len(export.Labels)`, `collEvents = len(export.Events)`, `collEventChanges =` sum of `len(ev.Changes)` over `export.Events` (`verify.go`).
- Iteration for reporting is over `conservationCollections` (fixed order), skipping any collection with `exact[coll] == false` (`verify.go`).
- Failure condition: `expected[coll] != actual[coll]` (`verify.go`). A collection with no emitter at all has `expected = 0`, so a non-empty rebuild of an unmapped collection also fires.
- Message (`verify.go`): `collection %q: source dump carries %d row(s) mapped here, rebuild has %d` — collection name quoted via `%q`, then expected, then actual.

Tests pin:
- `TestCountFindingsDetectsRowLoss` — 2 source issue rows vs 1 exported issue yields exactly one finding with `Law == LawCount` whose detail contains `issues`, `2`, and `1` (`verify_test.go`).
- `TestCountFindingsExcludesConditionalFanOutChildren` — with a 2-row `issue_history` dump mapped by `DeterministicMap`, an export with 3 nested `Changes` produces **zero** findings (conditional child excluded), while dropping an event to 1 produces exactly one `LawCount` finding containing `events` (`verify_test.go`).

### 1.6 Check family 3 — ID STABILITY (`idStabilityFindings`, `verify.go`)

- Reference set built by `sourceValuesFor(dump, m, "issues.id")` (`verify.go`). Target key literal is `"issues.id"`.
- If no source column maps to `issues.id`, returns `nil` — no findings at all (`verify.go`).
- Source set = set of raw cell strings; rebuilt set = set of `issue.ID` over `export.Issues` (`verify.go`).
- `missing = setDifference(source, rebuilt)`, `extra = setDifference(rebuilt, source)` (`verify.go`); `setDifference` returns sorted keys of `a` absent from `b` (`verify.go`).
- Two possible findings, both `LawIDStability`, emitted in this order:
  - missing (`verify.go`): `%d issue id(s) present in the source dump but absent from the rebuild: %s` — count then ids joined with `", "`.
  - extra (`verify.go`): `%d issue id(s) present in the rebuild but absent from the source dump: %s`.

`sourceValuesFor` (`verify.go`) semantics:
- Iterates every dumped table; builds `colIndex := rowColumnIndex(table)` (`verify.go`; helper at `shapemap.go`).
- For each emitter and each `field → src` pair, only `FromColumn` sources count (`verify.go`), and the match test is a string equality: `TargetKey(string(em.Collection)+"."+field) == target` (`verify.go`).
- Sets `found = true` on the first match but keeps going — it aggregates the **union** across all tables/columns rather than returning early (`verify.go`; the union behavior is pinned by `TestSourceValuesForAggregatesAcrossTables`, `verify_test.go`, expecting `i1,i2,i3` across two tables both mapping into `issues.id`).
- Cell values are rendered by `cellString` (`verify.go`; `shapemap.go`): `nil → ""`, `string → itself`, anything else → `fmt.Sprint(v)`.

`setIntersection` (`verify.go`) is declared in verify.go but has no caller in this file; its only use is `internal/store/sync_unrelated.go`.

Test pin: `TestIDStabilityFindingsDetectsLostAndExtraIDs` requires exactly 2 findings (one missing `i2`, one extra `i9`), both `LawIDStability` (`verify_test.go`).

### 1.7 Check family 4 — RANK PERMUTATION (`rankFindings`, `verify.go`)

Operates purely on `export.Issues`; the dump/mapping are not consulted (`verify.go`).
- Issues with `Rank == ""` are skipped entirely — unranked is legal (`verify.go`).
- Well-formedness: `rank.Valid(issue.Rank)` (`verify.go`). `rank.Valid` returns false for the empty string and for any string containing a byte outside the base-62 alphabet `0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz` (`internal/rank/rank.go`, `rank.go`). Malformed entries are recorded as `%s=%q` (id, rank) (`verify.go`).
- Distinctness: ranks are grouped into `idsByRank`; any rank with more than one id is a collision, rendered as `%q shared by %s` with the ids sorted and joined by `", "` (`verify.go`).
- Both `collisions` and `malformed` are sorted before rendering, for determinism (`verify.go`).
- Findings emitted in this order, both `LawRank`:
  - collisions (`verify.go`): `ranks are not distinct (a valid order needs one rank per issue): %s` — collision clauses joined by `"; "`.
  - malformed (`verify.go`): `%d issue(s) carry a value that is not a well-formed rank: %s` — entries joined by `", "`.

Test pins: `TestVerifyCandidateRejectsMisMappedRank` builds a candidate from a priority↔rank swapped mapping, requires `VerifyCandidate` to return no error, `Reconciled() == false`, at least one `LawRank` finding, and the rendered report to contain both colliding ids `i1` and `i2` (`verify_test.go`). `TestVerifyCandidateReconciledOnFaithfulRebuild` requires a faithful rebuild of `rankedDump()` to be `Reconciled()` (`verify_test.go`). The fixture `rankedDump()` is a single `issues` table with columns `id,title,description,status,priority,issue_type,created_at,updated_at,closed_at,item_rank` and two rows with ranks `"V"` and `"h"` and both priorities `int64(0)` (`verify_test.go`).

### 1.8 Fatal vs advisory summary

- **Gate-cannot-run errors** (returned as Go `error`, report is the zero value): Doctor failure, Export failure (`verify.go`).
- **Rejecting findings** (all equal weight, none advisory): Doctor `Errors` only, count mismatch, id missing, id extra, rank collision, rank malformed.
- **Silently tolerated**: every Doctor `Warning` (`verify.go`), any collection fed by a conditional emitter (`verify.go`), empty ranks (`verify.go`), an absent `issues.id` mapping (`verify.go`).

---

## 2. RECOVER (`internal/store/recover.go`, 208 lines; `recover_test.go`, 290 lines)

### 2.1 The `Mapper` seam

`type Mapper func(dump RawDump, feedback string) (ShapeMapping, error)` (`recover.go`).

`DeterministicMapper(dump RawDump, _ string)` ignores feedback; delegates to `DeterministicMap(dump)`, and on `!ok` returns the exact error text (`recover.go`):
`workspace shape not recognized by any built-in mapper; the LLM mapping path is required (feed `+"`lit lifeboat dump`"+` to the mapper, then apply+verify)` (`recover.go`).

### 2.2 The three outcome variants

`RecoveryOutcome` is a sealed interface with the unexported marker `isRecoveryOutcome()` (`recover.go`), implemented by exactly three types (`recover.go`):

- `Reconciled{Candidate *Candidate; Mapping ShapeMapping}` (`recover.go`)
- `RequiresDrop{Candidate *Candidate; Mapping ShapeMapping; Drops []UnexplainedDrop}` (`recover.go`)
- `Unconverged{Residual string; Attempts int}` (`recover.go`)

`UnexplainedDrop{Column ColumnRef}` — one field (`recover.go`).

Caller owns and must `Discard()` the candidate in the first two variants (`recover.go`).

### 2.3 Trigger conditions and preconditions of `Recover`

Signature: `Recover(ctx, canonicalDoltDir string, dump RawDump, mapper Mapper, maxAttempts int) (RecoveryOutcome, error)` (`recover.go`).

1. **Budget precondition**: `maxAttempts < 1` → returns `(nil, fmt.Errorf("recovery attempt budget must be at least 1, got %d", maxAttempts))` (`recover.go`). Pinned for 0 and -1, and that the outcome must be nil, by `TestRecoverRejectsNonPositiveBudget` (`recover_test.go`).
2. **Path validation**: `validateDoltRootDir(canonicalDoltDir)` (`recover.go`). That helper rejects whitespace-only/empty with `errors.New("dolt root dir is required")` and otherwise returns `filepath.Clean(path)` (`internal/store/store.go`). Pinned by `TestRecoveryEntryPointsRejectEmptyPath` (`recover_test.go`, input `"  "`) and `TestValidateDoltRootDirCleansPath` (`recover_test.go`).
3. **Staging location**: `parentDir := filepath.Dir(canonicalDoltDir)` (`recover.go`) — candidates are staged as siblings of the canonical Dolt directory, so a later promotion is a same-filesystem rename (`recover.go`).

`Recover` itself does **not** read or write the canonical directory: it only derives the parent path. It never touches the Dolt working set of the live workspace, never resets, never checks out, never stashes, and never commits. All of its on-disk effect is inside candidate scratch trees (§2.6).

### 2.4 The loop (`recover.go`)

- `feedback := ""` initially (`recover.go`); the first pass therefore always receives an empty feedback string — pinned at `recover_test.go`.
- `for attempt := 1; attempt <= maxAttempts; attempt++` calls `runAttempt(ctx, parentDir, dump, mapper, feedback)` (`recover.go`).
- A hard error from `runAttempt` aborts the whole loop and is returned with a nil outcome (`recover.go`).
- A non-nil outcome returns immediately (`recover.go`).
- Otherwise the returned string becomes the next pass's feedback (`recover.go`).
- Budget exhaustion: `return Unconverged{Residual: feedback, Attempts: maxAttempts}, nil` (`recover.go`). `Attempts` is the **budget**, not the number of passes that actually ran (they are equal by construction).
- `dump` is passed unchanged to every pass; it is never mutated (`recover.go`).

### 2.5 One pass — `runAttempt` (`recover.go`), exact step order

1. `mapping, err := mapper(dump, feedback)` (`recover.go`). On error → **not** a hard error; returns feedback `the mapper could not propose a mapping: %v` and a nil outcome (`recover.go`). Pinned by `TestRecoverUnconvergedOnPersistentMapperDecline`, which asserts `Attempts == 2` and the residual contains the mapper's message (`recover_test.go`).
2. `cand, err := RebuildCandidate(ctx, parentDir, dump, mapping)` (`recover.go`). Error handling branches on the sentinel:
   - `errors.Is(err, ErrInvalidMapping)` → feedback `the proposed mapping was rejected by the applier: %v` (`recover.go`), loop continues.
   - Any other error → hard error `rebuild candidate from a valid mapping failed: %w` (`recover.go`), loop aborts. `ErrInvalidMapping = errors.New("mapping is not applicable to the dump")` (`internal/store/candidate.go`); `RebuildCandidate` tags only `Apply`-stage rejections with it (`candidate.go`), pinned by `TestRebuildCandidateTagsMappingRejection` (`recover_test.go`).
3. `report, err := VerifyCandidate(ctx, dump, mapping, cand.store)` (`recover.go`). On error → hard error `errors.Join(fmt.Errorf("verify gate could not run: %w", err), cand.Discard())` — the candidate is discarded and any discard error is joined in (`recover.go`).
4. If `!report.Reconciled()` → `cand.Discard()`; a discard failure becomes the hard error `discard rejected candidate: %w` (`recover.go`). Otherwise returns `report.String()` as the next feedback with a nil outcome (`recover.go`). Every rejected candidate is therefore removed before the next pass starts (`recover.go`).
5. Reconciled → `classifyConverged(cand, mapping)` (`recover.go`); the candidate is **not** discarded and is handed to the caller.

### 2.6 On-disk state written/read by a pass (via `RebuildCandidate`, `internal/store/candidate.go`)

- `Apply(dump, mapping)` runs **first and purely** — a mapping rejection touches no filesystem resource at all (`candidate.go`).
- `os.MkdirTemp(parentDir, "lit-candidate-*")` creates the candidate root (`candidate.go`); the literal pattern is `lit-candidate-*`.
- On any failure after that point, a deferred cleanup closes the store (if opened) and runs `os.RemoveAll(root)`, joining errors into the return (`candidate.go`).
- `Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)` — the Dolt workspace is nested at `<root>/workspace` so the workspace lock and migration snapshots land inside the owned root (`candidate.go`). Error: `open candidate workspace: %w`.
- `st.ReplaceFromExport(ctx, export)` loads the data (`candidate.go`), error `load export into candidate: %w`. That path commits inside the candidate's own Dolt database with commit message `"replace from export"` (`internal/store/import_export.go`) — the only commit any recovery pass makes, and it is made in the throwaway candidate, never in the canonical workspace.
- The candidate stamps `expectedHead: dump.DoltHead` and `workspaceID: dump.WorkspaceID` for a later promotion's lost-update check (`candidate.go`).
- `Candidate.Discard()` closes the store then `os.RemoveAll(c.root)`; `root` is cleared only on successful removal, so a later `Discard` retries. It is documented and implemented as **idempotent** (`candidate.go`).

### 2.7 `classifyConverged` and `unexplainedDrops`

- `classifyConverged` returns `RequiresDrop` if `len(unexplainedDrops(mapping)) > 0`, else `Reconciled` (`recover.go`).
- `unexplainedDrops` walks `m.Tables`, and for each `col → d` in `tm.Drops` includes it when `d.Provenance == DropUnexplained` (`recover.go`). Results are sorted by `out[i].Column.String()` (`recover.go`), giving deterministic order.

### 2.8 Idempotency / repeatability

- Every pass is the same pipeline; the only carried state is the `feedback` string (`recover.go`).
- The dump is read-only across all passes, so attempt N cannot be contaminated by N-1 (`recover.go`); each rejected candidate is a separate temp tree removed whole (`candidate.go`).
- `DeterministicMapper` is a pure function of the dump and ignores feedback, so re-running it cannot self-repair; the doc directs running it at `maxAttempts=1` (`recover.go`). The CLI does exactly that: `const recoverAttempts = 1` (`internal/cli/lifeboat.go`).

### 2.9 CLI consumption of the outcomes (`internal/cli/lifeboat.go`)

- `lit lifeboat recover [--mapping <file>]`; wrong arg count → `UsageError{Message: "usage: lit lifeboat recover [--mapping <file>]"}` (`lifeboat.go`).
- With no `--mapping`, the mapper is `store.DeterministicMapper`; with one, the file is read and `json.Unmarshal`ed into a `store.ShapeMapping` and wrapped as a constant mapper (`lifeboat.go`). Errors: `read mapping %s: %w` (`lifeboat.go`), `parse mapping %s: %w` (`lifeboat.go`).
- Sequence: `store.HealWorkspace(ctx, ws.DatabasePath)` → `store.DumpRaw(...)` → `store.Recover(..., recoverAttempts)` (`lifeboat.go`).
- `Reconciled` → `promoteReconciled`: `store.PromoteCandidate`, then prints `recovered: rebuilt workspace promoted to %s (%s)\n` where the parenthetical is `previous contents preserved at %s` or, when `result.Backup == ""`, the literal `no previous contents to preserve` (`lifeboat.go`). The candidate is discarded in a defer, joining `discard candidate scratch after promotion: %w` on failure (`lifeboat.go`).
- `RequiresDrop` → candidate discarded, and error: `recovery needs a human decision: the mapping discards %d source column(s) with no recorded justification:\n%s\nnothing was changed; supply a mapping that maps or intentionally drops these before recovering` with the drops rendered one per line as `  - %s` (`lifeboat.go`, `formatDrops` at `lifeboat.go`).
- `Unconverged` → `recovery did not converge after %d attempt(s); nothing was changed:\n%s` (`lifeboat.go`).
- Unknown type → `unknown recovery outcome %T` (`lifeboat.go`).

### 2.10 What the recover tests pin

- `TestRecoverReconcilesKnownShape`: `preGooseDump()` + `DeterministicMapper` + budget 1 reaches `Reconciled`, the candidate's `Doctor` is clean, and `Export` has exactly 2 issues (`recover_test.go`).
- `TestRecoverSelfRepairsAcrossAttempts`: pass 1 returns an empty `ShapeMapping{}` (applier-rejected); pass 2 receives non-empty feedback and returns the good mapping; the result is `Reconciled` after exactly 2 mapper calls (`recover_test.go`).
- `TestRecoverRequiresDropOnUnexplainedDrop`: dropping the optional `issues.assignee` column with `Dropped{Provenance: DropUnexplained}` yields `RequiresDrop` with exactly one drop equal to `ColumnRef{Table:"issues", Column:"assignee"}` (`recover_test.go`, helper `withUnexplainedDrop` at `recover_test.go`).
- `TestRecoverUnconvergedSurfacesResidual`: priority↔rank swap with budget 3 gives `Unconverged` with `Attempts == 3` and a residual containing the string `rank_permutation` (`recover_test.go`).

---

## 3. RAWDUMP (`internal/store/rawdump.go`, 206 lines; `rawdump_test.go`, 177 lines)

### 3.1 The artifact shape

`RawDump` fields (`rawdump.go`):
- `WorkspaceID string` json `workspace_id` (`rawdump.go`)
- `DoltHead string` json `dolt_head` (`rawdump.go`)
- `Tables []RawTable` json `tables` (`rawdump.go`)

`RawTable` fields (`rawdump.go`):
- `Name string` json `name`, `Columns []string` json `columns`, `Rows [][]any` json `rows`.

`Rows` is always initialized to `[][]any{}` so an empty table serializes as `[]`, never `null` (`rawdump.go`; pinned at `rawdump_test.go` against the goose bookkeeping table).

### 3.2 Output format and destination

The dump is a **JSON** document, not SQL and not TSV. The only producer path to a file/stdout is `lit lifeboat dump`, which writes the `RawDump` value to stdout via `writeJSON` (`internal/cli/lifeboat.go`), which uses `json.NewEncoder(w)` with `enc.SetIndent("", "  ")` — two-space indentation, one trailing newline from `Encode` (`internal/cli/cli.go`). There is **no header line, no footer line, and no SQL quoting/escaping layer** — escaping is entirely `encoding/json`'s. `runLifeboatDump` takes no flags and rejects extra args with `UsageError{Message: "usage: lit lifeboat dump"}` (`internal/cli/lifeboat.go`).

### 3.3 `DumpRaw` — exact step order and every error (`rawdump.go`)

Signature `DumpRaw(ctx, doltRootDir string, workspaceID string) (RawDump, error)` with a named error return (`rawdump.go`).

1. `validateOpenArgs(doltRootDir, workspaceID)` (`rawdump.go`) — rejects an empty/whitespace root dir with `dolt root dir is required` (`store.go`) and an empty/whitespace workspace id with `workspace id is required` (`store.go`).
2. `acquireWorkspaceShared(ctx, doltRootDir)` — a **shared** workspace lock, excluding directory rotators such as `lit snapshots restore` (`rawdump.go`). On contention the error text is `a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w` wrapping `ErrWorkspaceBusy` (`internal/store/workspace_lock.go`).
3. A deferred release joins any release error into the returned error (`rawdump.go`).
4. `requireInitializedWorkspace(doltRootDir)` (`rawdump.go`) — the same shared helper `OpenForRead` calls: `os.ErrNotExist` → the package sentinel `ErrWorkspaceNotInitialized` (`workspace_initialized.go`), `repository not initialized with lit — run 'lit init' first`; any other stat error → `stat database dir: %w`.
5. `requireNoPendingAdopt(doltRootDir)` — run **after** the lock is taken (`rawdump.go`). Its message (`internal/store/adopt.go`): `%w: a `+"`lit init`"+` backlog adopt was interrupted before completing (%s; marker %s), so the on-disk store is that adopt's leftover partial state, not a usable backlog. Run `+"`lit init`"+` to retry: it sets the leftover aside and re-clones the remote backlog. If the remote no longer carries the backlog, delete %s to abandon the adopt and start fresh`, with the `%s` context either the literal `a backlog adopt` or `the adopt of %s/%s started %s` (`adopt.go`); a marker read failure yields `read adopt-pending marker: %w` (`adopt.go`).
6. `openStoreConnection(ctx, doltRootDir, workspaceID, engineRead)` — **no `migrate()` call**, which is what lets it read a workspace `store.Open` refuses (`rawdump.go`, doc at `rawdump.go`). `engineRead` is the first `engineAccess` value (`store.go`).
7. Deferred `s.db.Close()`, whose error is joined into the return unless it is `context.Canceled` (`rawdump.go`).
8. `readDoltHead(ctx, s.db)` — mandatory, not best-effort (`rawdump.go`, rationale `rawdump.go`).
9. `listTables(ctx, s.db)` (`rawdump.go`).
10. `dumpTable` per table, in the order `listTables` returned; the first table error aborts the whole dump (`rawdump.go`).
11. Returns `RawDump{WorkspaceID: workspaceID, DoltHead: head, Tables: tables}` (`rawdump.go`).

The dump is **read-only** on the database — the only SQL issued is the three SELECT-family statements below; nothing is written, committed, reset, or checked out (`rawdump.go`).

### 3.4 The three SQL statements

- Head: `SELECT commit_hash FROM dolt_log() LIMIT 1` (`rawdump.go`), error `read dolt head: %w` (`rawdump.go`). This is the single HEAD reader shared with promote-time re-checks and migration checkpointing (`rawdump.go`).
- Table list: `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name` (`rawdump.go`) — i.e. **every base table in the database in ascending catalog-name order**, including Dolt/goose bookkeeping tables; there is no hand-maintained include/exclude list (`rawdump.go`). Errors: `list tables: %w` (`rawdump.go`), `scan table name: %w` (`rawdump.go`), `iterate tables: %w` (`rawdump.go`).
- Per table: `"SELECT * FROM `" + name + "`"` — the table name is interpolated inside backticks, not parameterized (`rawdump.go`). Errors: `select %q: %w` (`rawdump.go`), `columns %q: %w` (`rawdump.go`), `scan row of %q: %w` (`rawdump.go`), `iterate rows of %q: %w` (`rawdump.go`).

### 3.5 Cell value rules (`dumpTable`, `rawdump.go`)

- Columns come from `rows.Columns()` on the live result set — never assumed (`rawdump.go`).
- Each row is scanned into `[]any` of `len(cols)`; positional order matches `Columns` (`rawdump.go`).
- The only type normalization: any `[]byte` cell is converted to `string` (`rawdump.go`).
- SQL `NULL` scans to `nil` and serializes as JSON `null`, distinct from `""` (`rawdump.go`; pinned by the `closed_at` assertion at `rawdump_test.go`).

### 3.6 What the rawdump tests pin

- `TestDumpRawReleasesDeadendedWorkspace`: after dropping the `issues.title` column and stamping goose ahead of the registry, `Open` fails with `*UnsupportedSchemaVersionError` while `DumpRaw` succeeds; `dump.WorkspaceID` equals the passed id; the `issues` table's rows match the seeded issue ids as `string` cells; the dropped `title` column is **absent** from `Columns` (not faked); and the goose bookkeeping table appears with non-nil `Rows` (`rawdump_test.go`).
- `TestDumpRawHealthyWorkspaceRoundTripsValues`: on a healthy workspace, `title` and `id` cells are Go `string`s with the exact seeded values, and the unset `closed_at` is `nil` (`rawdump_test.go`).

---

## 4. ROW_DELETES (`internal/store/row_deletes.go`, 119 lines; no dedicated test file)

### 4.1 Key types

- `relationKey{srcID string; dstID string; kind model.RelationType}` — exactly the schema PK `(src_id, dst_id, type)` (`row_deletes.go`).
- `labelKey{issueID string; name string}` — the labels PK `(issue_id, label)` (`row_deletes.go`).

### 4.2 The five deletes — all HARD deletes, all single-row by full primary key

| Function | Exact SQL | Subject string in errors | Site |
|---|---|---|---|
| `deleteIssueTx(ctx, tx, id)` | `DELETE FROM issues WHERE id = ?` | `fmt.Sprintf("issue %s", id)` | `row_deletes.go` |
| `deleteRelationRowTx(ctx, tx, key)` | `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?` | `fmt.Sprintf("relation %s->%s (%s)", key.srcID, key.dstID, key.kind)` | `row_deletes.go` |
| `deleteCommentTx(ctx, tx, id)` | `DELETE FROM comments WHERE id = ?` | `fmt.Sprintf("comment %s", id)` | `row_deletes.go` |
| `deleteLabelTx(ctx, tx, key)` | `DELETE FROM labels WHERE issue_id = ? AND label = ?` | `fmt.Sprintf("label %s:%s", key.issueID, key.name)` | `row_deletes.go` |
| `deleteEventTx(ctx, tx, id)` | `DELETE FROM issue_events WHERE id = ?` | `fmt.Sprintf("issue event %s", id)` | `row_deletes.go` |

Bind order for relations is `key.srcID, key.dstID, string(key.kind)` (`row_deletes.go`); for labels `key.issueID, key.name` (`row_deletes.go`).

### 4.3 Tombstone vs hard delete

- These are **hard** row removals. The soft-delete path is separate: ordinary issue deletion is a `DeletedAt` stamp, and **no CRUD path hard-deletes an issue row** — `deleteIssueTx` and `deleteEventTx` have only the reconcile delta as caller, deliberately (`row_deletes.go`).
- Cascade is owned by the schema, not by this code: deleting an issue takes its relations, comments, labels, events and event changes via `ON DELETE CASCADE` (`row_deletes.go`); deleting an event takes its `issue_event_changes` rows (`row_deletes.go`). No cascading DELETE statements are issued here.

### 4.4 Rows-affected contract and error text (`execDelete`, `row_deletes.go`)

- Runs `tx.ExecContext(ctx, stmt, args...)`; on error returns `(0, fmt.Errorf("delete %s: %w", subject, err))` (`row_deletes.go`).
- Then `res.RowsAffected()`; on error returns `(0, fmt.Errorf("delete %s: rows affected: %w", subject, err))` (`row_deletes.go`).
- Otherwise returns the affected count and nil (`row_deletes.go`).
- The count exists for the CRUD callers to distinguish "removed" from "there was nothing there"; the reconcile delta ignores it (`row_deletes.go`).

### 4.5 Callers and the affected-count decisions they make

- `RemoveLabel` — `deleteLabelTx(... labelKey{issueID, name: label})`; `affected == 0` → `storage.NotFoundError{Entity: "label", ID: fmt.Sprintf("%s/%s", issueID, label)}` (`internal/store/labels.go`).
- `RemoveRelation` — endpoints canonicalized first via `relType.CanonicalEndpoints`; `affected == 0` → `storage.NotFoundError{Entity: "relation", ID: fmt.Sprintf("src=%s dst=%s type=%s", srcID, dstID, relType)}` (`internal/store/relations.go`).
- `DeleteComment` — calls `deleteCommentTx` and **discards** the count (`internal/store/store.go`).
- The reconcile replay's delta — `applyExportDelta` runs the five tables in this exact order, deleting before inserting within each table: **issues, relations, comments, labels, events** (`internal/store/export_delta.go`). Issues go first so child rows inserted afterwards have their foreign key satisfied (`export_delta.go`). `applyTableDelta` explicitly ignores the affected count (`export_delta.go`).

### 4.6 Deletes deliberately NOT routed here

Set-matching deletes are separate statements on purpose (`row_deletes.go`): `setSingleValuedEdgeTx` (relations.go), `ClearParent` (relations.go), `replaceLabelsTx` (labels.go), and the self-edge sweep in import_export.go. Also outside: `writeExportTx`'s wholesale clear, which issues `DELETE FROM` for `labels, comments, relations, issues` in that order and deliberately does not name `issue_events`/`issue_event_changes` because they cascade from issues (`internal/store/import_export.go`), and `FixIntegrity`'s repairs (`import_export.go`).

---

## 5. CHECKPOINT (`internal/store/checkpoint.go`, 121 lines; `checkpoint_test.go`, 342 lines)

### 5.1 What a checkpoint IS

Concretely, **a Dolt branch created at the current HEAD** — not a tag, not a file, not a Dolt commit of its own (`checkpoint.go`). The value type is `storage.Checkpoint` (`internal/storage/maintenance.go`) with four fields:
- `Name string` — documented as `"<prefix>-<unix-nano>"` (`maintenance.go`)
- `Prefix string` — caller label, e.g. `"pre-migrate"` (`maintenance.go`)
- `CreatedAt time.Time` — parsed from the unix-nano suffix in Name (`maintenance.go`)
- `Anchor string` — opaque engine-side identity of the captured state; the Dolt engine fills it with a commit hash (`maintenance.go`, `checkpoint.go`).

The capability interface is `storage.Checkpointer` with exactly `CreateCheckpoint`, `ListCheckpoints`, `PruneCheckpoints`, `ResetToCheckpoint` (`internal/storage/capabilities.go`); the capability token is `Checkpoints = capability[Checkpointer]{name: "checkpoints"}` (`capabilities.go`).

### 5.2 Naming format string

`name := fmt.Sprintf("%s-%d", prefix, ts.UnixNano())` where `ts := time.Now().UTC()` (`checkpoint.go`). The suffix is nanoseconds since epoch as a decimal integer.

### 5.3 `CreateCheckpoint(ctx, prefix)` (`checkpoint.go`)

1. `readDoltHead(ctx, s.db)` — the shared HEAD reader (`checkpoint.go`, defined `rawdump.go`). Error → `checkpoint: %w` (`checkpoint.go`).
2. Timestamp captured (`checkpoint.go`), name formatted (`checkpoint.go`).
3. `s.db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", name)` — parameterized (`checkpoint.go`). Error → `checkpoint: create branch %q: %w` (`checkpoint.go`).
4. Returns `storage.Checkpoint{Name: name, Prefix: prefix, CreatedAt: ts, Anchor: commitSHA}` (`checkpoint.go`).

No pre-existence check, no retry on duplicate name; two creations within the same nanosecond would collide at `DOLT_BRANCH`. Tests sleep 1ms between creations to guarantee unique suffixes (`checkpoint_test.go`).

### 5.4 `ResetToCheckpoint(ctx, name)` (`checkpoint.go`)

- SQL: `CALL DOLT_RESET('--hard', ?)` with the branch name bound (`checkpoint.go`).
- Error → `checkpoint: reset to %q: %w` (`checkpoint.go`).
- Semantics per doc: hard-resets the **current branch** to the commit the named checkpoint branch points to, discarding all working-set changes and any Dolt commits made after the checkpoint (`checkpoint.go`). No checkout, no stash, no branch switching. Deleting the checkpoint branch is not part of reset.
- Pinned by `TestCheckpointResetReverts`: a row committed before the checkpoint survives; a row committed after it is gone (`checkpoint_test.go`).

### 5.5 `ListCheckpoints(ctx, prefix)` (`checkpoint.go`)

- SQL: `SELECT name, hash FROM dolt_branches WHERE name LIKE ? ORDER BY name` with the bind value `prefix + "-%"` (`checkpoint.go`).
- Errors: `checkpoint: list branches: %w` (`checkpoint.go`), `checkpoint: scan branch: %w` (`checkpoint.go`), `checkpoint: iterate branches: %w` (`checkpoint.go`).
- Each row is passed to `parseCheckpointName(name, prefix)`; rows that do not parse are **silently skipped**, not errors (`checkpoint.go`).
- `cp.Anchor` is overwritten with the branch's current `hash` column from `dolt_branches` (`checkpoint.go`) — so a listed checkpoint's Anchor reflects where the branch points now, while a freshly created one's Anchor is the HEAD read at creation.
- Final ordering is a `sort.Slice` by `CreatedAt.Before` — **oldest first** (`checkpoint.go`), pinned by `TestCheckpointSortedOldestFirst` (`checkpoint_test.go`).
- Prefix isolation pinned by `TestCheckpointListExcludesOtherPrefixes` (`checkpoint_test.go`).

### 5.6 `PruneCheckpoints(ctx, prefix, retain)` — retention (`checkpoint.go`)

- `retain < 0` → `checkpoint: retain must be non-negative, got %d` (`checkpoint.go`).
- Lists via `ListCheckpoints` (error propagated unchanged) (`checkpoint.go`).
- `len(cps) <= retain` → no-op, returns nil (`checkpoint.go`).
- Otherwise deletes `cps[:len(cps)-retain]` — the **oldest** ones, since the list is oldest-first (`checkpoint.go`).
- Delete SQL: `CALL DOLT_BRANCH('-d', '-f', ?)` — forced branch delete (`checkpoint.go`). Error → `checkpoint: delete branch %q: %w` and the loop aborts immediately (`checkpoint.go`).
- `retain = 0` deletes all (`checkpoint.go`), pinned by `TestCheckpointPruneZeroDeletesAll` (`checkpoint_test.go`). `TestCheckpointPruneEnforcesRetention` creates 7 and retains 3, asserting the surviving names are exactly the newest 3 (`checkpoint_test.go`).

### 5.7 `parseCheckpointName(name, prefix)` (`checkpoint.go`)

- `needle := prefix + "-"`; rejects when `len(name) <= len(needle)` or the prefix does not match exactly (`checkpoint.go`).
- Suffix must parse via `fmt.Sscanf(suffix, "%d", &ns)` **and** round-trip: `fmt.Sprintf("%d", ns) == suffix` — this rejects leading zeros, `+`-signs, and trailing garbage (`checkpoint.go`).
- On success returns `Checkpoint{Name: name, Prefix: prefix, CreatedAt: time.Unix(0, ns).UTC()}` — **`Anchor` is left empty** here; only `ListCheckpoints` fills it (`checkpoint.go`).
- `TestParseCheckpointName` pins the accept/reject table (`checkpoint_test.go`): accepts `("pre-migrate-1716998765000000000","pre-migrate")` and `("other-123456789","other")`; rejects non-numeric suffix `pre-migrate-abc`, empty suffix `pre-migrate-`, wrong prefix `not-matching-123`, mismatched prefix arg `("pre-migrate-123","other")`, and no suffix `("pre-migrate","pre-migrate")`.

### 5.8 Who creates/consumes checkpoints, and the retention constants

Constants (`internal/store/migration_runner.go`):
- `migrationCheckpointPrefix = "pre-migrate"` (`migration_runner.go`)
- `migrationCheckpointRetention = 5` (`migration_runner.go`)
- `migrationDriftRepairCheckpointPrefix = "pre-drift-repair"` (`migration_runner.go`) — deliberately distinct so the two retained sets prune independently (`migration_runner.go`).

**Startup migration path** `applyPendingMigrations` (`migration_runner.go`): builds the goose provider first so a provider failure leaves no orphan branch (`migration_runner.go`, error `construct migration provider: %w`); then `CreateCheckpoint(ctx, "pre-migrate")` before any mutation, error `create migration checkpoint: %w` (`migration_runner.go`). On `goose.ErrNoNextVersion` (success) it calls `PruneCheckpoints(ctx, "pre-migrate", 5)`, error `prune migration checkpoints: %w` (`migration_runner.go`). On a goose failure it calls `handleMigrationFailure` and also prunes, **ignoring** that prune's error (`migration_runner.go`). Each successful step commits with `migrationCommitMessage(result)` and, on failure, `commit migration v%d: %w` (`migration_runner.go`).

**Failure handling** `handleMigrationFailure` (`migration_runner.go`): reset first, quarantine second (`migration_runner.go`). `ResetToCheckpoint(checkpoint.Name)` failure yields `migration v%d failed and Dolt reset to %q failed (%v); restore from dbsnapshot. Root cause: %w` (`migration_runner.go`). A quarantine insert failure yields `migration v%d failed (reset to %q); quarantine insert failed (%v); restore from dbsnapshot. Root cause: %w` (`migration_runner.go`). The quarantine record is committed with the message `fmt.Sprintf("migrate: quarantine v%d %s", version, name)` (`migration_runner.go`), whose failure yields `migration v%d failed (reset to %q); quarantine commit failed (%v); ...` (`migration_runner.go`).

**Drift-repair path** `repairVersionContentDriftWithRollback` (`migration_runner.go`): `CreateCheckpoint(ctx, "pre-drift-repair")`, error `create version-content drift repair checkpoint: %w` (`migration_runner.go`); on repair failure it resets, and a reset failure gives `repair version-content drift (detected at v%d %q) failed (%v) and reset to checkpoint %q failed (%v); restore from dbsnapshot` (`migration_runner.go`), while a successful reset gives `repair version-content drift (detected at v%d %q) failed: %w (working set reset to checkpoint %q)` (`migration_runner.go`). On success it commits with `migrationDriftRepairCommitMessage(repaired)` (error `commit version-content drift repair: %w`, `migration_runner.go`) then `PruneCheckpoints(ctx, "pre-drift-repair", 5)` with error `prune version-content drift repair checkpoints: %w` (`migration_runner.go`).

Checkpoint branches are never garbage-collected other than by `PruneCheckpoints`; there is no expiry by age, only by count under a prefix (`checkpoint.go`).


---

## Behavioral inventory — `internal/store/{adopt,candidate,promote,downgrade}.go`

All claims cite `file:line` in `/Users/bmf/code/links-issue-tracker`. Derived from Go source and `_test.go` files only.

---

## 0. Shared primitives these four flows depend on (cited for completeness)

| Fact | Value | Citation |
|---|---|---|
| Canonical database directory name | `const doltDatabaseName = "links"` | `internal/store/store.go` |
| Engine access enum | `engineRead engineAccess = iota`, `engineWrite` | `internal/store/store.go` |
| Path validation | `validateDoltRootDir` rejects `strings.TrimSpace(doltRootDir) == ""` with error text `"dolt root dir is required"`, otherwise returns `filepath.Clean(doltRootDir)` | `internal/store/store.go` |
| Workspace lock file path | `filepath.Join(filepath.Dir(filepath.Clean(databasePath)), ".links-workspace.lock")` — a **sibling** of the dolt dir | `internal/store/workspace_lock.go` |
| Exclusive lock | `LockWorkspaceExclusive` → `acquireWorkspaceLock(ctx, doltRootDir, true, 0)` — **a zero wait, one attempt, no retry**; on `ErrWorkspaceBusy` wraps with `"another lit process is using this workspace; close other lit commands and retry: %w"`; once held, `forgetReceivedRefs` removes `<StorageDir>/received-refs.last` (`received_refs.go`: `ReceivedRefsPath`, `ReadReceivedRefs` — nil, nil when absent — and the atomic `WriteReceivedRefs`), and a removal failure other than absence releases the hold and returns `forget received-refs record before rotating the Dolt directory: %w` | `internal/store/workspace_lock.go` |
| Shared lock | `acquireWorkspaceShared` → `acquireWorkspaceLock(ctx, doltRootDir, false, coResidentHolderWait)` (2.3s of unchanged holders, polled every 100ms); busy message: `"a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w"` | `internal/store/workspace_lock.go` |
| Busy sentinel | `var ErrWorkspaceBusy = errors.New("workspace busy")` | `internal/store/workspace_lock.go` |
| `dirExists` | `info, err := os.Stat(path); return err == nil && info.IsDir()` | `internal/store/store.go` |
| Dolt pool shape | `sql.OpenDB(connector)` with `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)` | `internal/store/store.go` |
| Connector config | `embedded.Config{Directory: filepath.Clean(doltRootDir), CommitName: author, CommitEmail: fmt.Sprintf("%s@links.local", author), Database: database, DisableSingletonCache: true}`; author = trimmed workspaceID, `""`→`"links"`, `@`→`_`; `engineWrite` also sets `cfg.BackOff = newEngineOpenBackOff()` | `internal/store/store.go` |
| Procedure call builder | `CALL <PROC>()` when no args, else `CALL <PROC>(?,?,…)`; `callIntProcedure` scans **one int64 status column** | `internal/store/sync.go` |
| Snapshots dir | `filepath.Join(filepath.Dir(filepath.Clean(databaseDir)), "snapshots")` | `internal/store/migrate_snapshot.go` |
| Stamped-snapshot shape | `<all-digits>-<label>-<all-digits>` | `internal/store/migrate_snapshot.go` |
| Baseline | `const baselineVersion = migrations.Baseline`; `const Baseline int64 = 1` | `internal/store/migration_runner.go`, `internal/store/migrations/bounds.go` |
| Goose table | `const gooseVersionTable = "goose_db_version"` | `internal/store/migration_runner.go` |

---

## 1. ADOPT (`internal/store/adopt.go`, `adopt_test.go`)

### 1.1 What is adopted

A **remote Dolt database is cloned wholesale into the local dolt root**. It is not a fetch and not an in-place adoption of an existing dolt directory: `AdoptRemoteByClone` "bootstraps the local store by CLONING the remote's history wholesale, writing it directly into doltRootDir as the database's first on-disk state" (`adopt.go`). The clone primitive is chosen because on a git-backed remote the fetch path re-inflates the archive blob per chunk read (`adopt.go`).

Any **pre-existing** database directory at the target (an empty bootstrap store, or an interrupted adopt's residue) is **set aside by rename, never deleted** (`adopt.go`).

### 1.2 Constants and paths

| Item | Literal | Citation |
|---|---|---|
| Marker filename | `const adoptPendingMarkerName = ".links-adopt-pending"` | `adopt.go` |
| Marker path | `func AdoptPendingMarkerPath(databasePath string) string { return filepath.Join(filepath.Clean(databasePath), adoptPendingMarkerName) }` — **inside** the dolt root, sibling of the `links` database dir (not at the dirname position the locks use) | `adopt.go`, rationale `adopt.go` |
| Marker temp-file pattern | `os.CreateTemp(cleanRoot, adoptPendingMarkerName+".tmp-*")` → `.links-adopt-pending.tmp-*` | `adopt.go` |
| Database dir | `dbDir := filepath.Join(cleanRoot, doltDatabaseName)` → `<root>/links` | `adopt.go` |
| Displacement dir | `displaced := fmt.Sprintf("%s.adopt-displaced-%d", cleanRoot, time.Now().UTC().UnixNano())` — a **sibling of the dolt root** (prefix is `cleanRoot`, not `dbDir`) | `adopt.go` |
| Singleton cache keys | `filepath.ToSlash(filepath.Join(dbDir, ".dolt", "noms"))` and `filepath.ToSlash(filepath.Join(dbDir, ".dolt", "stats", ".dolt", "noms"))` | `adopt.go` |

### 1.3 Marker payload

```go
type adoptPendingMarker struct {
    StartedAt string `json:"started_at"`
    Remote    string `json:"remote"`
    Branch    string `json:"branch"`
}
```
(`adopt.go`). `StartedAt` is `now.UTC().Format(time.RFC3339)` (`adopt.go`). Doc states **presence** is the semantic; unreadable/garbage content still condemns (`adopt.go`).

Sentinel: `var errAdoptPending = errors.New("adopt pending")` — unexported, wrapped by every marker-present refusal so `LocalHasTickets` can discriminate (`adopt.go`).

### 1.4 `writeAdoptPendingMarker(cleanRoot, remote, branch string, now time.Time) error` — `adopt.go`

Exact ordered steps:
1. `json.Marshal(adoptPendingMarker{...})`; on error → `"encode adopt-pending marker: %w"` (`adopt.go`).
2. `os.CreateTemp(cleanRoot, ".links-adopt-pending.tmp-*")`; on error → `"write adopt-pending marker: %w"` (`adopt.go`).
3. `f.Write(payload)`; on error → `errors.Join(fmt.Errorf("write adopt-pending marker: %w", err), f.Close(), os.Remove(f.Name()))` (`adopt.go`).
4. `f.Sync()`; on error → `errors.Join(fmt.Errorf("sync adopt-pending marker: %w", err), f.Close(), os.Remove(f.Name()))` (`adopt.go`).
5. `f.Close()`; on error → `errors.Join(fmt.Errorf("close adopt-pending marker: %w", err), os.Remove(f.Name()))` (`adopt.go`).
6. `os.Rename(f.Name(), AdoptPendingMarkerPath(cleanRoot))`; on error → `errors.Join(fmt.Errorf("install adopt-pending marker: %w", err), os.Remove(f.Name()))` (`adopt.go`).
7. Best-effort directory fsync: `os.Open(cleanRoot)` then `_ = dir.Sync(); _ = dir.Close()` — **errors deliberately ignored**, with the stated reason that Windows cannot fsync a directory handle and there is no recovery action (`adopt.go`).

### 1.5 `clearAdoptPendingMarker(cleanRoot string) error` — `adopt.go`

`os.Remove(AdoptPendingMarkerPath(cleanRoot))`; `os.ErrNotExist` is treated as success; any other error → `"clear adopt-pending marker: %w"`.

### 1.6 `requireNoPendingAdopt(cleanRoot string) error` — `adopt.go`

1. `os.ReadFile(AdoptPendingMarkerPath(cleanRoot))`.
2. `errors.Is(err, os.ErrNotExist)` → `nil` (the **only** nil path).
3. Any other read error → `"read adopt-pending marker: %w"`.
4. Default description `interrupted := "a backlog adopt"`; if `json.Unmarshal` succeeds **and** `marker.Remote != ""` **and** `marker.Branch != ""`, it becomes `fmt.Sprintf("the adopt of %s/%s started %s", marker.Remote, marker.Branch, marker.StartedAt)` (`adopt.go`).
5. Returns, verbatim (`adopt.go`):

```
%w: a `lit init` backlog adopt was interrupted before completing (%s; marker %s), so the on-disk store is that adopt's leftover partial state, not a usable backlog. Run `lit init` to retry: it sets the leftover aside and re-clones the remote backlog. If the remote no longer carries the backlog, delete %s to abandon the adopt and start fresh
```
with args `errAdoptPending, interrupted, path, cleanRoot`.

`PendingAdopt(databasePath string) error` is the exported passthrough (`adopt.go`), documented as advisory/outside the workspace lock; the binding refusal is the post-lock check inside each open (`adopt.go`).

### 1.7 Where the refusal is enforced (post-lock, five entry points)

| Entry point | Call site |
|---|---|
| `Open` | `internal/store/store.go` (after `acquireWorkspaceShared`, before `ensureDoltDatabase`) |
| `OpenForRead` | `internal/store/store.go` |
| `EnsureDatabase` | `internal/store/store.go` |
| `OpenSync` | `internal/store/sync.go` |
| `DumpRaw` | `internal/store/rawdump.go` |

The placement rule is documented at `store.go`: a pre-lock check is stale because a live adopt holds the workspace lock exclusively, so "marker-with-acquirable-lock always means a DEAD adopt". `validateOpenArgs` deliberately does **not** contain the check (`store.go`).

External callers: `internal/cli/snapshots.go`, `internal/cli/init_sync.go`.

### 1.8 `LocalHasTickets(ctx, doltRootDir, workspaceID) (bool, error)` — `adopt.go`

Ordered:
1. `validateDoltRootDir(doltRootDir)`; error returns `(false, err)` (`adopt.go`).
2. `requireNoPendingAdopt(cleanRoot)`: if the error `errors.Is(err, errAdoptPending)` → returns `(false, nil)` — residue is "nothing to lose" **without opening it**; any other (I/O) error → `(false, err)` (`adopt.go`).
3. `if !dirExists(filepath.Join(cleanRoot, doltDatabaseName))` → `(false, nil)`; **does not create the store** (`adopt.go`).
4. `OpenForRead(ctx, cleanRoot, workspaceID)`, `defer s.Close()` (`adopt.go`).
5. `s.LocalIssueCount(ctx)` (defined `internal/store/sync.go`); returns `(count > 0, nil)` (`adopt.go`).

External caller: `internal/cli/init_sync.go`.

### 1.9 `AdoptRemoteByClone(ctx, doltRootDir, workspaceID, remoteName, remoteURL, branch string) (err error)` — `adopt.go`

**Full step sequence, in order:**

1. `cleanRoot, err = validateDoltRootDir(doltRootDir)` (`adopt.go`).
2. `if strings.TrimSpace(workspaceID) == ""` → `errors.New("workspace id is required")` (`adopt.go`).
3. `remoteName`, `remoteURL`, `branch` each `strings.TrimSpace`d (`adopt.go`).
4. If any of the three is empty → `fmt.Errorf("adopt by clone requires a remote name, url, and branch (got name=%q url=%q branch=%q)", remoteName, remoteURL, branch)` (`adopt.go`).
5. `os.MkdirAll(cleanRoot, 0o755)`; on error → `"create dolt root dir: %w"`. Reason given: the server root must exist before the sibling lock file can be taken and before the clone engine opens (`adopt.go`).
6. `release, err := LockWorkspaceExclusive(ctx, cleanRoot)` — returns the error unchanged on failure; `defer` joins any release error into the named return `err` (`adopt.go`).
7. `writeAdoptPendingMarker(cleanRoot, remoteName, branch, time.Now())` — **before the first destructive act**; on error returns immediately, nothing destructive has run (`adopt.go`).
8. `dbDir := filepath.Join(cleanRoot, doltDatabaseName)`; `evictSingleton(dbDir)` — eviction precedes the rename so no cached handle serves the displaced or re-cloned store stale (`adopt.go`, rationale).
9. `displaced := fmt.Sprintf("%s.adopt-displaced-%d", cleanRoot, time.Now().UTC().UnixNano())`; `os.Rename(dbDir, displaced)`. `os.ErrNotExist` = nothing to displace (proceed); any other error → `"set aside database before adopt: %w"`, **aborting with the marker still in place** (`adopt.go`).
10. `cloneRemoteDatabase(ctx, cleanRoot, workspaceID, remoteName, remoteURL, branch)` (§1.10).
11. **Clone-failure arm** (`adopt.go`): `evictSingleton(dbDir)`; then `os.RemoveAll(dbDir)` **unconditionally** (no stat gate — a transient stat error must not let the removal be skipped while the marker is cleared). Note this arm **deletes** rather than displaces, because the dbDir is this run's own partial clone.
    - `RemoveAll` fails → `errors.Join(cloneErr, fmt.Errorf("clean up partial clone: %w", rmErr))` — marker stays.
    - `clearAdoptPendingMarker` fails → `errors.Join(cloneErr, clearErr)`.
    - else → returns `cloneErr` alone.
12. **Post-clone validation**: `if !dirExists(dbDir)` → `fmt.Errorf("clone of remote %q produced no %q database", remoteName, doltDatabaseName)`; **the marker is deliberately left in place** (`adopt.go`).
13. `clearAdoptPendingMarker(cleanRoot)` is the last act; on failure returns (`adopt.go`):
```
the backlog cloned completely, but the adopt-completion marker could not be cleared, so the store stays refused until a retry of `lit init` completes (the retry sets this download aside and re-clones; no remote data is at risk): %w
```
14. `return nil`.

**Stated postcondition (two states, never three)** (`adopt.go`): nil return ⇒ database dir holds the complete cloned backlog and no marker remains; error return ⇒ no partial database remains at the canonical path either — *or*, when cleanup/marker-clear could not complete, the durable marker remains so the leftover is never opened as a store.

**Caller precondition** (`adopt.go`): the caller must NOT open the store before calling, because cloning straight into the canonical path keeps dolt's in-process singleton chunk-store cache honest. External caller: `internal/cli/init_sync.go`.

### 1.10 `cloneRemoteDatabase` — the exact Dolt invocation (`adopt.go`)

```go
db, err := openDoltPool(serverRoot, workspaceID, "", engineWrite)   // no current database
...
callIntProcedure(ctx, db, "DOLT_CLONE",
    "--remote", remoteName, "--branch", branch, remoteURL, doltDatabaseName)
```
i.e. the SQL executed is `CALL DOLT_CLONE(?,?,?,?,?,?)` with args `["--remote", remoteName, "--branch", branch, remoteURL, "links"]` — wait, six placeholders for six args: `--remote`, `<remoteName>`, `--branch`, `<branch>`, `<remoteURL>`, `links` (`adopt.go`; builder at `sync.go`). `defer db.Close()` (`adopt.go`).

Errors:
- pool open → `"open dolt for clone: %w"` (`adopt.go`).
- procedure → `fmt.Errorf("clone remote %q (%s) branch %q: %w", remoteName, remoteURL, branch, err)` (`adopt.go`).

Documented behavior: the git-backed remote defaults to the `refs/dolt/data` ref — the ref lit's sync push writes — so no explicit ref is passed (`adopt.go`).

### 1.11 `evictSingleton(dbDir string)` — `adopt.go`

Two best-effort calls, both return values discarded:
```go
_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "noms")), false)
_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "stats", ".dolt", "noms")), false)
```
Documented: lit's own opens bypass the cache (`DisableSingletonCache: true`, `store.go`), so any entry found was left by a dolt-internal load path (e.g. during `DOLT_CLONE`); the entry is **dropped, not closed**, because closing the carcass a second time trips dolt's refcount assert on shared archive readers (`adopt.go`).

### 1.12 Idempotency / re-run behavior

- Re-running over a **successfully adopted** store: the store exists, so step 9 renames it to a new `.adopt-displaced-<ns>` sibling and re-clones. Pinned by `TestAdoptRemoteByCloneBootstrapsAndReAdopts` (`adopt_test.go`), which adopts twice into the same `consumer` root and asserts the seeded issue is readable after each (`adopt_test.go`).
- Re-running after a **returned** clone failure: no residue, so nothing to displace; pinned by `TestAdoptRemoteByCloneFailedCloneLeavesNoResidue` (`adopt_test.go`).
- Re-running over **abandoned residue**: pinned by `TestAdoptRemoteByCloneHealsAbandonedAdoptResidue` (`adopt_test.go`).

### 1.13 Exact test assertions (adopt)

`TestLocalHasTicketsDoesNotCreateStore` (`adopt_test.go`):
- `LocalHasTickets(ctx, root, "ws")` on an absent root returns `(false, nil)`.
- `dirExists(filepath.Join(root, doltDatabaseName))` must be false afterwards — "LocalHasTickets created the store; it must only observe, never create".
- After `EnsureDatabase(ctx, root, "ws")`, `LocalHasTickets` still returns `(false, nil)`.

`TestAdoptRemoteByCloneBootstrapsAndReAdopts` (`adopt_test.go`): remote URL is `"file://" + filepath.Join(base, "remote")`, branch `"master"`, remote name `"origin"`; `LocalHasTickets` after adopt = `true`. Helper `assertHasIssueAfterAdopt` does `OpenForRead(ctx, root, "ws")` + `st.GetIssue(ctx, id)`.

`TestAdoptRemoteByCloneFailedCloneLeavesNoResidue` (`adopt_test.go`):
- Adopt with branch `"branch-the-remote-does-not-have"` must error.
- `dirExists(filepath.Join(consumer, doltDatabaseName))` must be false.
- `os.Stat(AdoptPendingMarkerPath(consumer))` must be `IsNotExist`.
- The retry with `"master"` succeeds and again leaves no marker.

`TestAdoptRemoteByCloneHealsAbandonedAdoptResidue` (`adopt_test.go`):
- Fabricates residue: `os.MkdirAll(<consumer>/links, 0o755)` + `os.WriteFile(<consumer>/links/not-a-database, []byte("junk"), 0o644)`.
- Writes **garbage** marker content `[]byte("not json")` at `AdoptPendingMarkerPath(consumer)` mode `0o644` — pins that presence, not parseability, condemns.
- `LocalHasTickets` returns `(false, nil)` over that residue.
- Adopt over the residue succeeds, marker gone, issue readable.
- `filepath.Glob(consumer + ".adopt-displaced-*")` must return **exactly one** entry, and `<displaced>/not-a-database` must still exist — "displacement must preserve bytes".

`TestEnsureDatabaseContendsWithWorkspaceExclusiveHolder` (`adopt_test.go`): while `LockWorkspaceExclusive` is held, `EnsureDatabase(ctx, root, "ws")` returns an error satisfying `errors.Is(err, ErrWorkspaceBusy)`.

`TestMarkerRefusalIsReservedForDeadAdopts` (`adopt_test.go`): with the marker written and the exclusive hold **live**, `OpenForRead` must be `ErrWorkspaceBusy` and must **not** contain the substring `"interrupted"`; after `release()`, `OpenForRead` must error containing `"interrupted"`.

`TestPendingAdoptMarkerCondemnsEveryNormalOpen` (`adopt_test.go`): with the marker present, each of `Open`, `OpenForRead`, `EnsureDatabase`, `OpenSync`, `DumpRaw` must return a non-nil error whose text contains all three substrings `"interrupted"`, `"origin/master"`, `"lit init"`. After `os.Remove(AdoptPendingMarkerPath(root))`, `OpenForRead` succeeds — "the marker must be the sole condemner".

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
(`candidate.go`). It is "one disposable, fully isolated rebuild of a workspace: a fresh Dolt directory at the current baseline, loaded with the domain data a validated (dump, mapping) produced" (`candidate.go`).

A candidate **owns one directory TREE**, not just the dolt dir: the dolt workspace is nested one level **inside** `root` because `Open` writes the workspace lock and migration snapshots as siblings of the dolt directory; rooting at the parent brings those siblings inside the owned tree so one `RemoveAll(root)` is total (`candidate.go`).

`expectedHead` and `workspaceID` are the dump's provenance, stamped from the one dump at build time (`candidate.go`).

There is **no exported accessor** for the store — deliberately, so no caller above `internal/store` holds a concrete engine (`candidate.go`).

### 2.2 Naming scheme (quoted)

```go
root, err := os.MkdirTemp(parentDir, "lit-candidate-*")
```
(`candidate.go`). The `*` is replaced by `os.MkdirTemp` with a random number, so directories are `lit-candidate-<random>`. `parentDir == ""` means the system temp dir (`candidate.go`).

The dolt workspace is at a fixed child name:
```go
st, err = Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)
```
(`candidate.go`), and `detachForPromotion` re-derives the same literal: `doltDir := filepath.Join(c.root, "workspace")` (`candidate.go`).

`parentDir` for the recovery loop is `filepath.Dir(canonicalDoltDir)` (`internal/store/recover.go`).

### 2.3 `ErrInvalidMapping`

`var ErrInvalidMapping = errors.New("mapping is not applicable to the dump")` (`candidate.go`). It tags mapping rejections distinctly from filesystem/store I/O failures so the recovery loop routes them as repair feedback (`candidate.go`); `Recover`'s attempt loop branches on `errors.Is(err, ErrInvalidMapping)` (`internal/store/recover.go`).

### 2.4 `RebuildCandidate(ctx, parentDir string, dump RawDump, mapping ShapeMapping) (*Candidate, error)` — `candidate.go`

Ordered:
1. `export, err := Apply(dump, mapping)` — **pure, runs first**, so an invalid mapping is rejected before any directory or handle exists (`candidate.go`, rationale). On error → `fmt.Errorf("%w: %w", ErrInvalidMapping, err)` (`candidate.go`). `Apply` is at `internal/store/shapemap.go`.
2. `os.MkdirTemp(parentDir, "lit-candidate-*")`; on error → `"create candidate workspace dir: %w"` (`candidate.go`).
3. Installs the unconditional cleanup defer keyed on a `success bool` (`candidate.go`): if not successful, `err = errors.Join(err, st.Close())` when `st != nil`, then `err = errors.Join(err, os.RemoveAll(root))`.
4. `Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)`; on error → `"open candidate workspace: %w"` (`candidate.go`).
5. `st.ReplaceFromExport(ctx, export)` (defined `internal/store/import_export.go`); on error → `"load export into candidate: %w"` (`candidate.go`).
6. `success = true`; returns `&Candidate{store: st, root: root, expectedHead: dump.DoltHead, workspaceID: dump.WorkspaceID}` (`candidate.go`).

Documented: `dump` is read-only and reusable unchanged across attempts; `Apply` never mutates it, so two attempts from one dump yield identical candidates (`candidate.go`).

### 2.5 `detachForPromotion() (string, error)` — `candidate.go`

1. `if c.root == ""` → `errors.New("candidate has no workspace to promote (already discarded or never built)")` — the stated reason is that `filepath.Join("", "workspace")` would yield a cwd-relative `"workspace"` a promotion would rename into the canonical location (`candidate.go`).
2. `doltDir := filepath.Join(c.root, "workspace")`.
3. If `c.store != nil`: `err = c.store.Close()`, then `c.store = nil` (so a later `Discard`'s close is a no-op by its own state).
4. Returns `(doltDir, err)` — **the doltDir is returned even when Close errored**.

`c.root` is **not** cleared: the candidate still owns the scratch siblings, which a later `Discard` removes; only the dolt directory leaves ownership (`candidate.go`).

### 2.6 `Discard() error` — `candidate.go`

Two independently-tracked resources, each released against **its own field**, not a shared flag (`candidate.go`):
1. If `c.store != nil`: `err = c.store.Close()`; `c.store = nil`.
2. If `c.root != ""`: `os.RemoveAll(c.root)`. On removal error → `errors.Join(err, rmErr)` and **`c.root` is left set**, so a later `Discard` retries. Only on success is `c.root = ""`.
3. Returns `err`.

Idempotent: a caller may `defer Discard` and still discard explicitly on the reject path (`candidate.go`).

### 2.7 Discovery / enumeration / GC

There is **no enumeration or garbage-collection of candidate directories in this file**. Cleanup is per-candidate only: the deferred `RemoveAll(root)` on the build-failure path (`candidate.go`) and `Discard`'s `RemoveAll` (`candidate.go`). No code scans `parentDir` for `lit-candidate-*`; the guarantee asserted instead is zero residue per attempt (`candidate.go`). The recovery loop discards each non-reconciling candidate before the next pass (`internal/store/recover.go`).

### 2.8 Exact test assertions (candidate)

Fixture `preGooseDump()` (`candidate_test.go`): `WorkspaceID: "legacy-ws"`, **no `DoltHead` field set** (this is what makes it the missing-provenance shape used in promote tests); tables `issues` (2 rows, `i1` todo/`i2` done), `relations` (0 rows), `comments` (1), `labels` (1), `issue_events` (1), `issue_event_changes` (1); timestamps `"2026-01-01T00:00:00Z"` / `"2026-01-02T00:00:00Z"`.

`TestRebuildCandidateValidMappingYieldsFreshWorkspace` (`candidate_test.go`): `RebuildCandidate(ctx, t.TempDir(), dump, mustMap(t, dump))` succeeds; `cand.store.Doctor(ctx)` must be clean (`mustClean`); `cand.store.Export(ctx)` must carry exactly 2 issues.

`TestRebuildCandidateRejectLeavesZeroResidue` (`candidate_test.go`):
- `RebuildCandidate(ctx, parent, dump, ShapeMapping{})` must error — an empty mapping is not total over the dump's columns.
- `dirEntryCount(t, parent)` must be **0** after the rejection.
- A subsequent valid attempt under the same parent yields 2 issues.
- After `cand.Discard()`, `dirEntryCount(t, parent)` must again be **0** — explicitly including "the workspace lock and migration snapshots Open writes as siblings of the dolt directory. This is the guarantee a flat dolt-dir layout silently broke".

`TestRebuildCandidateAttemptsAreIsolated` (`candidate_test.go`): two candidates from one dump under one parent; `first.Discard()` twice must both return nil (idempotence); `second.store.Export(ctx)` still yields 2 issues.

### 2.9 POSIX-specific behavior (`candidate_posix_test.go`)

Build tag: `//go:build !windows` (`candidate_posix_test.go`).

`TestDiscardRetriesDirectoryRemoval` (`candidate_posix_test.go`):
- Skips when `os.Geteuid() == 0` with reason `"removal-permission injection has no effect as root"` — root bypasses the permission check so the injection cannot fail there.
- Injection mechanism is **POSIX directory-write-bit semantics**: `os.Chmod(parent, 0o555)` makes the parent unwritable, and removing an entry requires write permission on its parent. This is permission semantics, not atomicity or rename semantics — no rename or fsync behavior is exercised in this file.
- Under the unwritable parent, the first `cand.Discard()` **must error**.
- After `os.Chmod(parent, 0o755)`, the second `cand.Discard()` must return nil.
- `dirEntryCount(t, parent)` must then be **0**.
- Rationale pinned: "A single shared release flag would have nulled everything on the first attempt and the retry would no-op, stranding the directory".
- Both chmods are also registered via `t.Cleanup` so an early `Fatalf` cannot strand an unwritable parent.

---

## 3. PROMOTE (`internal/store/promote.go`, `promote_test.go`)

### 3.1 What is promoted, from where to where

The candidate's rebuilt **Dolt directory** (`<candidate root>/workspace`) is installed **at the workspace's canonical Dolt path**, in place. The store "lives at a fixed path that consumers cannot be repointed at, so promotion is an in-place swap at that path — never a wipe, never a repoint. Every step is an atomic rename(2)" (`promote.go`).

```go
type PromotionResult struct {
    Canonical string
    Backup    string
}
```
(`promote.go`) — "Backup is persisted, not pruned: the pre-recovery copy is the most precious artifact in the flow" (`promote.go`).

External caller: `internal/cli/lifeboat.go`.

### 3.2 `PromoteCandidate(ctx, canonicalDoltDir string, cand *Candidate) (PromotionResult, error)` — `promote.go`

Exact ordering:

1. `canonicalDoltDir, err = validateDoltRootDir(canonicalDoltDir)` — done **before** deriving the lock path, backup names, and rename target, so a trailing separator cannot make backup naming and backup scanning target different directories (`promote.go`).
2. `src, err := cand.detachForPromotion()` — **closes the candidate store before any rename**, because an open handle blocks a directory rename on Windows and the promoted store is reopened fresh regardless (`promote.go`). Error → `"surrender candidate workspace for promotion: %w"`.
3. `release, err := LockWorkspaceExclusive(ctx, canonicalDoltDir)`; the deferred release joins its error into the named return (`promote.go`). The lock file is a **sibling** of the dolt directory so it is held continuously while the guarded directory is briefly absent (`promote.go`).
4. `healCanonical(canonicalDoltDir)` — heals a prior crash *before* swapping, so this swap starts from the invariant "canonical present" (`promote.go`).
5. `verifyHeadUnchanged(ctx, canonicalDoltDir, cand.workspaceID, cand.expectedHead)` — the lost-update gate, run **under the same exclusive lock**, **after heal**, and **before the first rename**, so an abort changes nothing on disk (`promote.go`).
6. Installs the rollback defer: on any `err != nil` after this point, `healCanonical(canonicalDoltDir)` runs and its error is joined. "Roll BACK, never forward" (`promote.go`).
7. `backup, err := uniqueBackupPath(canonicalDoltDir, time.Now().UTC().UnixNano())` (`promote.go`).
8. **Rename 1**: `preserved, err = moveAside(canonicalDoltDir, backup)` (`promote.go`).
9. **Rename 2**: `os.Rename(src, canonicalDoltDir)`; on error → `"install rebuilt workspace at canonical path: %w"` (`promote.go`).
10. Returns `PromotionResult{Canonical: canonicalDoltDir, Backup: preserved}` — `Backup` is `""` when nothing pre-existed, "never a phantom path" (`promote.go`).

**No fsync anywhere in promote.go.** Crash-safety rests entirely on rename atomicity: the only interrupted-at-rest state is "canonical absent, backup present" (`promote.go`).

### 3.3 Sentinels and the head gate

- `var ErrWorkspaceAdvanced = errors.New("workspace advanced since dump")` (`promote.go`).
- `var ErrMissingDumpProvenance = errors.New("dump has no recorded head commit")` (`promote.go`).

`verifyHeadUnchanged(ctx, canonicalDoltDir, workspaceID, expectedHead) error` — `promote.go`:
1. `if expectedHead == ""` → `fmt.Errorf("%w: cannot verify the live workspace has not advanced; re-run `lit lifeboat dump` against the current workspace and recover from that artifact", ErrMissingDumpProvenance)` (`promote.go`).
2. `openStoreConnection(ctx, canonicalDoltDir, workspaceID, engineRead)`; error → `"re-read live workspace head: %w"` (`promote.go`). **Takes no workspace lock** — the caller already holds the exclusive hold (`promote.go`).
3. Deferred `s.db.Close()`, joined into the named return unless `errors.Is(closeErr, context.Canceled)` (`promote.go`).
4. `live, err := readDoltHead(ctx, s.db)` (defined `internal/store/rawdump.go`); error returned unwrapped (`promote.go`).
5. `if live != expectedHead` → (`promote.go`):
```
%w: the candidate was rebuilt from %s but the live workspace is now at %s; a concurrent commit landed during recovery — nothing was changed, re-run recovery against the current state
```
with args `ErrWorkspaceAdvanced, expectedHead, live`.

### 3.4 `HealWorkspace(ctx, canonicalDoltDir string) error` — `promote.go`

1. `validateDoltRootDir` (else an empty path would put `.links-workspace.lock` in cwd and scan cwd for backups) (`promote.go`).
2. `LockWorkspaceExclusive`, deferred release joined into `err` (`promote.go`).
3. `return healCanonical(canonicalDoltDir)`.

No-op when the canonical directory is present, so it is safe to run unconditionally before any recovery (`promote.go`). External caller: `internal/cli/lifeboat.go`.

### 3.5 `moveAside(canonicalDoltDir, backup string) (string, error)` — `promote.go`

`os.Stat(canonicalDoltDir)`:
- nil error → `os.Rename(canonicalDoltDir, backup)`; on error → `("", "move existing workspace aside: %w")`; else returns `(backup, nil)`.
- `errors.Is(statErr, os.ErrNotExist)` → `("", nil)` — a legitimate no-op; the install proceeds with no backup.
- any other stat error → `("", "stat canonical workspace: %w")`.

### 3.6 `healCanonical(canonicalDoltDir string) error` — `promote.go`

1. `os.Stat(canonicalDoltDir)`: nil → return `nil` (present, nothing to do); `os.ErrNotExist` → fall through; other → `"stat canonical workspace: %w"`.
2. `backup, err := newestBackup(canonicalDoltDir)`; error propagated.
3. `if backup == ""` → return `nil` (canonical absent and no backup; "this process holds no copy to put back").
4. `os.Rename(backup, canonicalDoltDir)`; on error → `fmt.Errorf("restore canonical workspace from backup %q: %w", backup, err)`.

The restore **consumes** the backup (it is renamed, not copied) — pinned by test at `promote_test.go`.

### 3.7 Backup naming

```go
path := fmt.Sprintf("%s.backup-%0*d", canonicalDoltDir, promotionStampWidth, nanos)
```
(`promote.go`) — i.e. `<canonicalDoltDir>.backup-<19-digit zero-padded UnixNano>`.

`const promotionStampWidth = 19` (`promote.go`) — "19 digits holds every int64 UnixNano value (the type overflows in 2262, still 19 digits), so the stamps are equal-width and lexical order equals chronological order" (`promote.go`).

`uniqueBackupPath(canonicalDoltDir string, nanos int64) (string, error)` — `promote.go`: loops; `os.Stat(path)`; `os.ErrNotExist` → return path; any other non-nil error → `"probe backup path: %w"`; otherwise `nanos++` and retry. Uniqueness is by construction, not "assumed-unique-because-nanoseconds"; the exclusive lock held across probe and rename keeps a found-free path free (`promote.go`).

`isPromotionBackup(name, prefix string) bool` — `promote.go`: `strings.CutPrefix(name, prefix)` must succeed, the suffix must be **exactly** `promotionStampWidth` long, and every rune must be `'0'..'9'`.

`newestBackup(canonicalDoltDir string) (string, error)` — `promote.go`:
- `dir := filepath.Dir(canonicalDoltDir)`; `prefix := filepath.Base(canonicalDoltDir) + ".backup-"`.
- `os.ReadDir(dir)`; error → `"scan workspace backups: %w"`.
- Selects entries where `e.IsDir() && isPromotionBackup(e.Name(), prefix)` — a stray regular file or a hand-named directory like `"<prefix>manual"` is **not** a workspace and must not be selected (`promote.go`).
- Empty → `("", nil)`; else `sort.Strings(names)` and return `filepath.Join(dir, names[len(names)-1])`.
- Scan-not-glob is deliberate so a path containing glob metacharacters cannot silently skip a real backup (`promote.go`).

### 3.8 Locks held during the window

Exactly one: the exclusive workspace hold from `LockWorkspaceExclusive` (`promote.go`), held from before `healCanonical` through both renames until the deferred release (`promote.go`). It is the same hold `lit snapshots restore` takes (`promote.go`). No commit lock is taken in `promote.go`.

### 3.9 Exact test assertions (promote)

Helpers: `copyTree` (pure-Go recursive copy preserving `info.Mode().Perm()`, `promote_test.go`); `seedRealWorkspace` (creates issues then `DumpRaw`, so `dump.DoltHead` is the live head, `promote_test.go`); `freshExportIDs` (copies the tree to a never-before-opened path before opening, because the embedded Dolt driver caches engine state per path within a process and a reopened-then-swapped path can return **stale rows**, `promote_test.go`); `hasPromotionBackup` (calls `newestBackup`, `promote_test.go`); `markerDir`/`readMarker` (write/read a `marker` file inside a stand-in directory, `promote_test.go`).

`TestPromoteCandidateEndToEnd` (`promote_test.go`): seeds 2 issues; `Recover(ctx, canonical, dump, DeterministicMapper, 1)` yields `Reconciled`; `PromoteCandidate` succeeds; `freshExportIDs(result.Backup)` contains every original ID; `freshExportIDs(canonical)` has exactly 2 and contains every original ID; reopened canonical is `Doctor`-clean.

`TestPromoteCandidateAbortsOnConcurrentCommit` (`promote_test.go`): after a concurrent `CreateIssue` on the live workspace, `PromoteCandidate` must return an error with `errors.Is(err, ErrWorkspaceAdvanced)`; `hasPromotionBackup(canonical)` must be **false** — "an aborted promotion made a backup; nothing should have moved"; the concurrent issue ID is still live.

`TestHealCanonicalRestoresInterruptedSwap` (`promote_test.go`): backup literal is `canonical + ".backup-1700000000000000001"`; canonical deliberately absent; after `healCanonical`, `readMarker(canonical) == "original"` and `os.Stat(backup)` must be `IsNotExist` (backup consumed by the restore).

`TestHealCanonicalPicksNewestBackup` (`promote_test.go`): `.backup-1700000000000000001` = `"older"`, `.backup-1700000000000000002` = `"newer"`; heal restores `"newer"`.

`TestUniqueBackupPathStepsPastCollision` (`promote_test.go`): with `fmt.Sprintf("%s.backup-%019d", canonical, 1700000000000000001)` already existing, `uniqueBackupPath(canonical, stamp)` must not return that path and the stepped path must still satisfy `isPromotionBackup(filepath.Base(got), filepath.Base(canonical)+".backup-")`.

`TestHealCanonicalIgnoresForeignBackupNames` (`promote_test.go`): `.backup-1700000000000000001` = `"real"` and `.backup-manual` = `"foreign"` (which sorts lexicographically **after** the numeric stamps); heal must restore `"real"`.

`TestHealWorkspaceRestoresAfterCrash` (`promote_test.go`): `.backup-1700000000000000007` = `"pre-crash"`; `HealWorkspace` restores it; a second `HealWorkspace` on the now-healthy workspace is a no-op and leaves the marker unchanged.

`TestPromoteCandidateRefusesDumpWithoutProvenance` (`promote_test.go`): a candidate built from `preGooseDump()` (no `DoltHead`) must fail `errors.Is(err, ErrMissingDumpProvenance)` and must **not** satisfy `errors.Is(err, ErrWorkspaceAdvanced)`; no backup made; live workspace retains its issues.

`TestPromoteCandidateRejectsDiscardedCandidate` (`promote_test.go`): after `cand.Discard()`, `PromoteCandidate` must error, and the canonical workspace marker must still read `"original"` — no swap attempted.

`TestPromoteCandidateRollsBackOnInstallFailure` (`promote_test.go`): the install is made to fail deterministically by calling `cand.detachForPromotion()` then `os.RemoveAll(src)`, so the second rename hits a missing source; `PromoteCandidate` must error; afterwards `freshExportIDs(canonical)` must contain every original issue — the moved-aside original was restored.

---

## 4. DOWNGRADE (`internal/store/downgrade.go`, `downgrade_test.go`)

### 4.1 What "downgrade" means concretely

**Schema-version rollback via goose Down migrations**, one Dolt commit per reversed migration, preceded by a recovery snapshot. `Downgrade` "reverses migrations to bring the workspace to targetSchemaVersion, taking a recovery snapshot first and committing one Dolt commit per reversed migration" (`downgrade.go`). It is invoked only by the `lit downgrade` command; no Open-path code reaches it (`downgrade.go`). External caller: `internal/cli/downgrade.go` with `target.Manifest.Schema.Max`.

### 4.2 Constants

| Constant | Literal | Citation |
|---|---|---|
| `downgradeSnapshotLabel` | `"lit-downgrade"` | `downgrade.go` |
| `downgradeSnapshotRetention` | `10` | `downgrade.go` |
| (comparison) `migrationSnapshotLabel` | `"pre-migrate"` | `internal/store/migrate_snapshot.go` |
| (comparison) `migrationSnapshotRetention` | `10` | `internal/store/migrate_snapshot.go` |

Test hook: `var migrationDownForTest func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error)` — when non-nil it replaces `provider.Down(ctx)` inside `applyDownMigrations` (`downgrade.go`).

### 4.3 Snapshot naming and classification

`formatDowngradeSnapshotLabel(t time.Time) string` = `fmt.Sprintf("%s-%d", downgradeSnapshotLabel, t.UTC().UnixNano())` → `lit-downgrade-<unix-ns>` (`downgrade.go`). The trailing timestamp is documented as cosmetic — `dbsnapshot.Take` encodes take-time in the directory name (`downgrade.go`).

`IsDowngradeSnapshotName(name string) bool` = `isStampedSnapshotName(name, downgradeSnapshotLabel)` (`downgrade.go`), matching `<unix-ns>-lit-downgrade-<unix-ns>` (`migrate_snapshot.go`). Documented as disjoint from `IsMigrationSnapshotName` so each producer's retention budget governs only its own snapshots (`downgrade.go`).

### 4.4 Error types (every message text)

| Type | Fields | `Error()` format | Citation |
|---|---|---|---|
| `downgradeMigrationFailedError` (unexported) | `Version int64`, `Cause error` | `"down-migrate v%d: %v"` (Version, Cause); `Unwrap() → Cause` | `downgrade.go` |
| `DowngradeTargetAheadError` | `Current int64`, `Target int64` | `"cannot downgrade to v%d: workspace is already at v%d — that is a forward move; use ` + "`lit upgrade`" + ` instead"` (Target, Current) | `downgrade.go` |
| `DowngradeBelowBaselineError` | `Target int64` | `"cannot downgrade to v%d: baseline is v%d — going below it would destroy the workspace; restore a pre-upgrade snapshot via ` + "`lit snapshots restore <name>`" + ` instead"` (Target, `baselineVersion`) | `downgrade.go` |
| `DowngradeRollbackError` | `Snapshot dbsnapshot.Snapshot`, `Cause error` | `"downgrade: %v\n\nthe workspace state before this downgrade is preserved at:\n  %s\n\nto restore, run:\n  lit snapshots restore %s"` (Cause, Snapshot.Path, Snapshot.Name); `Unwrap() → Cause` | `downgrade.go` |
| `DowngradeIncompleteError` | `Current int64`, `Target int64` | `"downgrade incomplete: goose has no more reversible migrations but recorded version v%d still above target v%d"` (Current, Target) | `downgrade.go` |

Additional inline error strings:
- Phase guard: `"downgrade: workspace is not goose-managed (no goose_db_version table); run Open first to adopt or initialize"` (`downgrade.go`).
- Snapshot failure: `fmt.Errorf("downgrade: %w", err)` (`downgrade.go`).
- Prune failure: `"prune downgrade snapshots: %w"` (`downgrade.go`).
- Provider construction: `"construct downgrade provider: %w"` (`downgrade.go`).
- `"down-migrate v%d: goose returned nil result"` (`downgrade.go`).
- `"down-migrate v%d: goose result has nil Source"` (`downgrade.go`).
- `"commit downgrade revert of v%d: %w"` (`downgrade.go`).

### 4.5 Locking

`Downgrade` wraps the entire pipeline in `s.withCommitLock(ctx...)` (`downgrade.go`; lock impl `internal/store/commit_lock.go`). Documented: "classify, snapshot, and the Down loop are serialized against every other writer just like migrate()'s mutations are. Acquisition is reentrant: the per-step commitWorkingSet calls inside applyDownMigrations short-circuit because the lock is already held" (`downgrade.go`). No workspace lock is taken here.

### 4.6 `downgradeLocked(ctx, targetSchemaVersion int64) error` — `downgrade.go`

Ordered:
1. `state, err := s.classifyMigrationState(ctx)` (impl `internal/store/migration_runner.go`); error propagated unwrapped (`downgrade.go`).
2. **Refusal**: `state.phase != phaseManaged` → the plain "not goose-managed" error, **no snapshot taken** (`downgrade.go`).
3. **No-op**: `targetSchemaVersion == state.appliedVersion` → `return nil`, no snapshot (`downgrade.go`).
4. **Refusal**: `targetSchemaVersion > state.appliedVersion` → `&DowngradeTargetAheadError{Current: state.appliedVersion, Target: targetSchemaVersion}` (`downgrade.go`).
5. **Refusal**: `targetSchemaVersion < baselineVersion` → `&DowngradeBelowBaselineError{Target: targetSchemaVersion}` — refused **before invoking goose**, so the destructive baseline Down is unreachable from this entry point (`downgrade.go`, rationale).
6. `snapshotsDir := migrationSnapshotsDir(s.doltRootDir)` — the **same directory** migration snapshots use (`downgrade.go`).
7. `guard := newSnapshotGuard(s.doltRootDir, snapshotsDir, formatDowngradeSnapshotLabel(time.Now()))`; `snap, err := guard.ensure(ctx)` → `dbsnapshot.Take(ctx, databaseDir, snapshotsDir, label)` (`downgrade.go`; guard at `migrate_snapshot.go`). Error → `"downgrade: %w"` (which itself wraps `"snapshot before migration: %w"`, `migrate_snapshot.go`).
8. `s.applyDownMigrations(ctx, targetSchemaVersion)`; on **any** error → `&DowngradeRollbackError{Snapshot: snap, Cause: err}` (`downgrade.go`).
9. `dbsnapshot.PruneMatching(snapshotsDir, downgradeSnapshotRetention, IsDowngradeSnapshotName)` (impl `internal/dbsnapshot/snapshot.go`); error → `"prune downgrade snapshots: %w"` (`downgrade.go`).
10. `return nil`.

Summarized refusal contract, verbatim from the doc comment (`downgrade.go`): "Refusals (no snapshot taken): target == current applied: no-op, returns nil. target > current applied: DowngradeTargetAheadError. target < baselineVersion: DowngradeBelowBaselineError. workspace not in phaseManaged: a plain error (no goose log to reverse)."

### 4.7 `applyDownMigrations(ctx, target int64) error` — `downgrade.go`

1. `provider, err := newGooseProvider(s.db)` (impl `internal/store/migration_runner.go`); error → `"construct downgrade provider: %w"`.
2. Unbounded loop:
   a. `current, err := s.recordedMigrationVersion(ctx)` (impl `migration_runner.go`); error propagated (`downgrade.go`).
   b. `if current <= target` → `return nil` (`downgrade.go`).
   c. `downOne := provider.Down`, replaced by `migrationDownForTest` when non-nil (`downgrade.go`).
   d. `result, err := downOne(ctx)`.
      - `errors.Is(err, goose.ErrNoNextVersion)` → `&DowngradeIncompleteError{Current: current, Target: target}` (`downgrade.go`).
      - any other error → `&downgradeMigrationFailedError{Version: current, Cause: err}` (`downgrade.go`).
   e. `result == nil` → `"down-migrate v%d: goose returned nil result"` (current) (`downgrade.go`).
   f. `result.Source == nil` → `"down-migrate v%d: goose result has nil Source"` (current) (`downgrade.go`).
   g. `s.commitWorkingSet(ctx, downgradeCommitMessage(result))` (impl `internal/store/commit_lock.go`); error → `"commit downgrade revert of v%d: %w"` (`result.Source.Version`, err) (`downgrade.go`).
   h. Loop repeats — the loop re-reads the recorded version each iteration rather than counting.

Commit message: `downgradeCommitMessage(result) = fmt.Sprintf("downgrade: revert v%d %s", result.Source.Version, filepath.Base(result.Source.Path))` (`downgrade.go`), stated as symmetric with `migrationCommitMessage`'s `migrate: v<N> <file>` shape (`downgrade.go`).

### 4.8 What downgrade modifies

- `goose_db_version` rows (mutated by goose's `Down`, "the same way Up does", `downgrade.go`; the test hook mutates it via `database.NewStore(goose.DialectMySQL, gooseVersionTable).Delete`, `downgrade_test.go`).
- Whatever DDL/DML each migration's Down section performs.
- One Dolt commit per reversed step via `commitWorkingSet`.
- One new snapshot directory under `<storageDir>/snapshots`, plus pruning of downgrade-labeled snapshots beyond 10.

### 4.9 Exact test assertions (downgrade)

Fixture `openWorkspaceForDowngrade` opens a fresh workspace at registry-max via `Open(ctx, <tmp>/dolt, "test-workspace-id")` (`downgrade_test.go`). `snapshotCount` counts via `dbsnapshot.List` and **fails the test** if any `dbsnapshot.IsProducerArtifactName(e.Name())` entry (stranded `.tmp`/`.reserve`) is present (`downgrade_test.go`). `stampGooseVersion` inserts a fake applied row via `gs.Insert(ctx, st.db, database.InsertRequest{Version: version})` (`downgrade_test.go`).

- `TestAppliedSchemaVersionMatchesRecorded`: `AppliedSchemaVersion` == `recordedMigrationVersion`, and the recorded version is `> 0`.
- `TestAppliedSchemaVersionZeroForNonManaged`: after `DROP TABLE goose_db_version`, `AppliedSchemaVersion` must be **0**.
- `TestDowngradeTargetEqualIsNoOp`: `Downgrade(ctx, current)` returns nil and the snapshot-count delta is **0**.
- `TestDowngradeTargetAheadRefused`: `Downgrade(ctx, current+5)` → `errors.As` a `*DowngradeTargetAheadError` with `Current == current` and `Target == current+5`; snapshot delta **0**.
- `TestDowngradeBelowBaselineRefused`: `Downgrade(ctx, baselineVersion-1)` → `*DowngradeBelowBaselineError` with `Target == baselineVersion-1`; message must contain `"would destroy the workspace"`; snapshot delta **0**.
- `TestDowngradeRollbackOnFailure` (**not parallel** — installs the package-level hook): stamps `registryMax+1`, hook returns `errors.New("synthetic down failure")`; result must be `*DowngradeRollbackError` that `errors.Is` the synthetic cause; `rb.Snapshot.Name` and `rb.Snapshot.Path` non-empty; `os.Stat(rb.Snapshot.Path)` must succeed; `rb.Error()` must contain the literal `"lit snapshots restore "+rb.Snapshot.Name`.
- `TestDowngradeHappyPathSteppedAndCommitted` (**not parallel**): stamps `vA=registryMax+1` and `vB=registryMax+2`; hook deletes the highest recorded row and returns a `*goose.MigrationResult` whose `Source.Path` is `fmt.Sprintf("/test/%05d_fake.sql", current)`. Asserts: recorded version lands exactly at `registryMax`; snapshot count minus Open's one migration snapshot equals exactly **1**; and `doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")` output contains, for each of vA and vB, `fmt.Sprintf("downgrade: revert v%d %05d_fake.sql", v, v)`.
- `TestDowngradeIncompleteWhenGooseExhausted` (**not parallel**): hook returns `goose.ErrNoNextVersion`; the outer error is a `*DowngradeRollbackError` whose `Cause` is `errors.As`-able to `*DowngradeIncompleteError` with `Current == registryMax+1`, `Target == registryMax`.
- `TestIsDowngradeSnapshotNameSymmetry`: with `migName = "1700000000000000000-pre-migrate-1700000000000000001"` and `dgName = "1700000000000000000-lit-downgrade-1700000000000000001"` — `IsMigrationSnapshotName(migName)` true / `IsDowngradeSnapshotName(migName)` false; `IsDowngradeSnapshotName(dgName)` true / `IsMigrationSnapshotName(dgName)` false.
- `TestDowngradeUntouchedOpen`: a no-op `Downgrade`, `Close`, then re-`Open` yields the identical recorded version.
- `TestDowngradeRequiresGooseManaged`: after `DROP TABLE goose_db_version` + a commit, `Downgrade(ctx, baselineVersion)` errors with a message containing `"not goose-managed"`; snapshot delta **0**; and the error must **not** be `errors.As`-able to `*DowngradeRollbackError`.
- Compile-time guards: `_ error = (*DowngradeTargetAheadError)(nil)`, `(*DowngradeBelowBaselineError)(nil)`, `(*DowngradeRollbackError)(nil)` (`downgrade_test.go`).


---

## Locking, Remote Cache, and the Vendored Dolt Driver

All paths below are relative to `/Users/bmf/code/links-issue-tracker`.

---

### 1. The lock primitive: `github.com/promptctl/primitives/filelock`

Vendored in the module cache at `/Users/bmf/go/pkg/mod/github.com/promptctl/primitives@v0.2.0/filelock/`. Every lit-minted lock in `internal/store` goes through `filelock.Acquire`.

#### 1.1 `Acquire` contract

`filelock/filelock.go` — `func Acquire(ctx context.Context, lockPath string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, bool, error)`

Sequence, in order:

1. `filelock/filelock.go` — if `maxAttempts < 1`, returns `(nil, false, error)` with message `filelock: maxAttempts must be >= 1, got %d`. It does **not** read as contention.
2. `filelock/filelock.go` — if `ctx.Err() != nil` at entry, returns `(nil, false, ctx.Err())` **before any attempt**, on a free lock as well as a held one.
3. `filelock/filelock.go` — `os.MkdirAll(filepath.Dir(lockPath), 0o755)`; failure returns `ensure lock dir: %w`.
4. `filelock/filelock.go` — `os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)`; failure returns `open lock file: %w`. **The lock file is created if absent and its content is never written or read — it is a zero-byte file. No PID, no timestamp, no holder metadata is ever stored in any lit lock file** (`filelock/filelock.go` is the only write-mode open, and nothing writes to `file`).
5. `filelock/filelock.go` — loop `attempt` from `0` to `maxAttempts-1`:
   - `filelock/filelock.go` `tryLockFile(file, exclusive)`.
   - On success (`filelock/filelock.go`): builds a `release` closure (`filelock/filelock.go`) that runs `unlockFile(fd)` then `fd.Close()` and returns `errors.Join` of `release file lock: %w` and `close file lock fd: %w`. Then re-checks `ctx.Err()` (`filelock/filelock.go`); if done, releases the just-taken hold and returns `errors.Join(ctxErr, release())`. Otherwise returns `(release, true, nil)`.
   - On a non-would-block error (`filelock/filelock.go`): returns `lock %s: %w` joined with any FD-close error (`joinWithClose`, `filelock/filelock.go`).
   - `filelock/filelock.go` — the sleep is **skipped after the final attempt**.
   - `filelock/filelock.go` — `SleepWithContext(ctx, delay)`; a cancellation mid-sleep returns `(nil, false, ctx.Err())` joined with any close error.
6. `filelock/filelock.go` — after exhaustion, closes the FD; a close failure returns `close file lock fd: %w`.
7. `filelock/filelock.go` — if `ctx.Err() != nil`, returns it (cancellation wins over contention).
8. `filelock/filelock.go` — otherwise returns `(nil, false, nil)`: **contention is a value, not an error**.

`maxAttempts == 1` is the non-blocking probe and never sleeps (`filelock/filelock.go` doc, enforced by the `attempt+1 == maxAttempts` break at `filelock/filelock.go`).

#### 1.2 Platform implementation

- POSIX (`filelock/filelock_posix.go`): `syscall.Flock(fd, LOCK_SH|LOCK_NB)` for shared, `LOCK_EX|LOCK_NB` for exclusive. `EWOULDBLOCK` maps to the internal `errWouldBlock` sentinel (`filelock/filelock.go`). Unlock is `syscall.Flock(fd, LOCK_UN)` (`filelock/filelock_posix.go`).
- Windows (`filelock/filelock_windows.go`): `LockFileEx` over the whole address space (`low=0xFFFFFFFF, high=0xFFFFFFFF`, `filelock/filelock_windows.go`) with `LOCKFILE_FAIL_IMMEDIATELY = 0x1` (`filelock/filelock_windows.go`) and `LOCKFILE_EXCLUSIVE_LOCK = 0x2` (`filelock/filelock_windows.go`). `ERROR_LOCK_VIOLATION = 33` (`filelock/filelock_windows.go`) maps to `errWouldBlock`.

**Stale handling: there is none, by design.** A hold lives on the open file description, so any process death (SIGKILL included) releases it in the kernel; nothing in the package or in `internal/store` inspects mtime, PID, or age (`filelock/filelock.go`; `internal/store/doc.go`).

#### 1.3 The second surface, `filelock.Lock` (not used by the store locks)

`filelock/lock.go` — a reusable exclusive handle. Sentinels: `ErrLocked = errors.New("filelock: lock is held")` (`filelock/lock.go`), `ErrTimeout = errors.New("filelock: timed out waiting for lock")` (`filelock/lock.go`). Poll interval `10 * time.Millisecond` (`filelock/lock.go`). `TryLock` (`filelock/lock.go`) is one attempt; `Lock` (`filelock/lock.go`) polls forever; `LockWithTimeout` (`filelock/lock.go`) polls until the deadline then returns `ErrTimeout` — a non-positive timeout is a single immediate attempt. Re-locking a handle that already holds returns `ErrLocked` (`filelock/lock.go`). `Unlock` of an unheld handle returns `filelock: unlock of an unheld lock` (`filelock/lock.go`).

#### 1.4 Tests pinning the primitive

- `filelock/filelock_test.go` shared holders coexist on independent FDs.
- `filelock/filelock_test.go` a single-attempt exclusive probe against a live shared holder returns `(acquired=false, err=nil)`, and acquires once the holder releases.
- `filelock/filelock_test.go` an already-done context is refused on a free lock and a held lock alike, with `context.Canceled`, and the free lock is left free.
- `filelock/filelock_test.go` cancellation during a retry sleep surfaces `context.Canceled` (budget used: 500 attempts × 20ms, cancel at 50ms).
- `filelock/filelock_test.go` `maxAttempts` of `0` and `-1` are loud errors.

---

### 2. The store's lock stamping boundary

`internal/store/workspace_lock.go` — `acquireStoreLock(ctx, lockPath, exclusive, maxAttempts, delay)` is the **single** place `filelock.Acquire`'s `acquired=false` value becomes a domain error:

```go
release, acquired, err := filelock.Acquire(ctx, lockPath, exclusive, maxAttempts, delay)
if err != nil { return nil, err }
if !acquired { return nil, ErrWorkspaceBusy }
return release, nil
```

`internal/store/workspace_lock.go` — `var ErrWorkspaceBusy = errors.New("workspace busy")`. Every store lock's contention wraps this sentinel; `errors.Is(err, ErrWorkspaceBusy)` is the uniform discriminator.

`internal/store/workspace_lock_test.go` (`TestWorkspaceBusyErrorsWrapSentinel`) pins that both `acquireWorkspaceShared` and `LockWorkspaceExclusive` refusals satisfy `errors.Is(err, ErrWorkspaceBusy)` (the shared arm also accepts a `"deadline exceeded"` string when the test's 200ms context expires first).

#### 2.1 Declared acquisition order

`internal/store/doc.go` — outermost to innermost:

```
workspace → Dolt's own .dolt/noms/LOCK → commit → snapshot producer beacon
```

A holder of an inner lock never waits on an outer one (`internal/store/doc.go`). Two locks sit outside the order: the sync-push lock, because every acquisition of it is a non-blocking probe so nothing ever waits on it (`internal/store/doc.go`); and the mirror liveness beacon, whose acquisitions all happen holding nothing (`internal/store/doc.go`).

`internal/store/doc.go` states one tolerated deviation: a GC-contention retry rotates the store's connection mid-mutation, re-acquiring Dolt's LOCK under the held commit lock, bounded at `coResidentHolderWait` strictly inside every commit-lock waiter's budget (`commitLockWaiterBudget()`).

`internal/store/doc.go` — ONE HOME: every lit-minted lock file sits at `dirname(databasePath)`, so a `lit snapshots restore` that rotates the dolt directory cannot move the lock out from under acquirers. Three stated exceptions: the snapshot producer beacon (inside `snapshots/`), the adopt-pending marker (inside the dolt root), and Dolt's own journal `LOCK`.

`internal/store/doc.go` — lit formerly minted a second lock `.links-engine.lock` for "one write-capable engine per path"; it is retired, and the name is deliberately not reused.

---

### 3. The workspace lock

#### 3.1 Path

`internal/store/workspace_lock.go`:

```go
func WorkspaceLockPath(databasePath string) string {
    cleaned := filepath.Clean(databasePath)
    return filepath.Join(filepath.Dir(cleaned), ".links-workspace.lock")
}
```

Literal filename: `.links-workspace.lock`, a **sibling** of the dolt root directory.

#### 3.2 Modes, budgets, and errors

| Function | Mode | wait | poll | Total budget | On contention |
|---|---|---|---|---|---|
| `acquireWorkspaceShared` (`workspace_lock.go`) | shared | `coResidentHolderWait` (`store.go`) | `storeLockPollInterval` = 100ms (`store.go`) | 2.3s of unchanged holders (`holdWait`, `lock_holder.go`) | wrapped error, see below |
| `LockWorkspaceShared` (`workspace_lock.go`) | shared | same — delegates to `acquireWorkspaceShared` | same | same | same |
| `LockWorkspaceExclusive` (`workspace_lock.go`) | exclusive | `0` (`workspace_lock.go`) | — | single non-blocking attempt | wrapped error, see below |

Exact contention messages:

- Shared (`workspace_lock.go`):
  `a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w`
- Exclusive (`workspace_lock.go`):
  `another lit process is using this workspace; close other lit commands and retry: %w`

Both wrap `ErrWorkspaceBusy` via `%w`.

Meaning of the two modes (`workspace_lock.go`): shared marks **directory readers** (store opens, raw dumps, snapshot file walks); exclusive marks **directory rotators** (operations that displace, swap, or rebuild the dolt directory). The exclusive mode refuses immediately on contention with any shared holder rather than waiting (`workspace_lock.go`).

`acquireWorkspaceLock` (`workspace_lock.go`) is the shared body: `acquireStoreLock(ctx, WorkspaceLockPath(doltRootDir), exclusive, maxAttempts, delay)`.

#### 3.3 Tests pinning workspace-lock behavior

- `workspace_lock_test.go` shared holders coexist.
- `workspace_lock_test.go` exclusive refuses while shared is held.
- `workspace_lock_test.go` shared refuses while exclusive is held.
- `workspace_lock_test.go` exclusive is released after `Store.Close()`.
- `workspace_lock_test.go` (`TestOpenForReadAcquiresLockBeforeStat`) — `OpenForRead` takes the shared lock **before** its database-exists stat, so a concurrent restore that transiently renames the database dir away yields a workspace-busy refusal, never a false "repository not initialized".
- `workspace_lock_test.go` `OpenSync` holds the workspace lock.
- `workspace_lock_test.go` exclusive holds serialize.

---

### 4. The commit lock

#### 4.1 Path

`internal/store/commit_lock.go`:

```go
func commitLockPathForDolt(databasePath string) string {
    cleaned := filepath.Clean(databasePath)
    return filepath.Join(filepath.Dir(cleaned), ".links-commit-flock.lock")
}
```

Literal filename: `.links-commit-flock.lock`, sibling of the dolt directory. Exported as `CommitLockPath(databasePath)` (`commit_lock.go`).

`commit_lock.go` records why the historical name `.links-commit.lock` is **burned and must not be restored**: O_EXCL-era binaries `os.Remove` that path on release and on 10-minute age eviction, and an unlink under a live flock splits the lock across two inodes so the next acquirer runs concurrently with the orphaned holder.

#### 4.2 Budget and errors

`commit_lock.go`:
```go
func commitLockWaiterBudget() time.Duration {
	return coResidentHolderWait + rotationReserve()
}
```
= 2.3s + (2.3s + 1s) = **5.6s** of unchanged holders, always **exclusive** (`commit_lock.go`). `commit_lock.go` states the sizing rationale: the ordinary holder is a mutation, the longest one a mutation that suffered a GC-contention rotation, and the budget must strictly exceed `rotationReserve()` so the rotation's re-open gives up before the peer waiting on the commit lock does; a snapshot copy, a restore or a retention prune holding longer fails a waiter naming it.

`acquireCommitLockAtPath` (`commit_lock.go`):
```go
release, err := acquireStoreLock(ctx, storageDir, lockPath, true, commitLockWaiterBudget())
if err != nil { return nil, wrapCommitLockContention(err) }
```

`wrapCommitLockContention` (`commit_lock.go`) attaches guidance **only** when `errors.Is(err, ErrWorkspaceBusy)`; every other error, cancellation included, passes through untouched:

> `another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes: %w`

`LockCommitPath(ctx, lockPath)` (`commit_lock.go`) is the external entry for callers with no open Store (e.g. `lit snapshots new`/`restore`); it routes to the same `acquireCommitLockAtPath`.

#### 4.3 Re-entrancy

`commit_lock.go` — `type commitLockContextKey struct{}`. `acquireCommitLock` (`commit_lock.go`) checks `ctx.Value(commitLockContextKey{}).(bool)`; if already true it returns the ctx unchanged with a no-op release (`commit_lock.go`), so a nested `commitWorkingSet` inside a held mutation never queues behind its own hold. On a fresh acquire it returns `context.WithValue(ctx, commitLockContextKey{}, true)` (`commit_lock.go`).

#### 4.4 Release settlement

`SettleCommitLockRelease(opErr, releaseErr)` (`commit_lock.go`):
- `releaseErr == nil` → return `opErr`.
- `opErr != nil` → `errors.Join(opErr, releaseErr)`.
- `opErr == nil, releaseErr != nil` → prints to **stderr**:
  `lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): %v\n` (`commit_lock.go`)
  and returns `nil` — a durable success is never retroactively failed.

`withCommitLock` (`commit_lock.go`) defers the release so it fires on panic too (`commit_lock.go`).

#### 4.5 Tests pinning commit-lock behavior

- `commit_lock_test.go` `TestAcquireCommitLockNeverEvictsLiveHolderByAge`.
- `commit_lock_test.go` `TestAcquireCommitLockIgnoresDeadResidue`.
- `commit_lock_test.go` `TestWrapCommitLockContention`.
- `commit_lock_test.go` `TestSettleCommitLockRelease`.
- `commit_lock_test.go` `TestWithMutationResumesAtVersioningAfterStagedCommit`.
- `commit_lock_test.go` `TestCommitWorkingSetOnceRendersStamp` — see §5.4.

---

### 5. Mutation sequencing and Dolt commit rendering (commit_lock.go)

#### 5.1 `commitStamp`

`commit_lock.go`:
```go
type commitStamp struct {
    Message    string
    Date       time.Time // non-zero → --date; Dolt parses to second granularity, sub-second truncates
    Author     string    // non-empty → --author "Name <email>", replacing the session identity
    AllowEmpty bool      // → --allow-empty
}
```

#### 5.2 `withMutation` / `withStampedMutation`

`commit_lock.go` — `withMutation(ctx, message, fn)` = `withStampedMutation(ctx, commitStamp{Message: message}, fn)`.

`commit_lock.go` — `withStampedMutation` runs, under one held commit lock, inside `retryTransientGCContention`:

1. If not yet staged: `s.db.BeginTx(ctx, nil)` — error wrapped `begin %s tx: %w` with the message (`commit_lock.go`).
2. `defer tx.Rollback()` (`commit_lock.go`).
3. `fn(ctx, tx)` — error returned unwrapped (`commit_lock.go`).
4. `tx.Commit()` — error wrapped `commit %s tx: %w` (`commit_lock.go`).
5. `staged = true` (`commit_lock.go`).
6. `s.commitWorkingSetOnce(ctx, stamp)` (`commit_lock.go`).

The `staged` flag is the resume point: once `tx.Commit()` has succeeded, a retry resumes **at versioning** and never re-runs `fn` (`commit_lock.go`). The lock is acquired and released exactly once (`commit_lock.go`).

#### 5.3 `commitWorkingSet`

`commit_lock.go` — `commitWorkingSet(ctx, message)` takes the commit lock and runs `commitWorkingSetOnce(ctx, commitStamp{Message: message})` under `retryTransientGCContention`.

#### 5.4 `commitWorkingSetOnce` — the single Dolt-commit boundary

`commit_lock.go`:

1. `commit_lock.go` — optional `s.commitWorkingSetHookForTest` runs first; its error aborts.
2. `commit_lock.go` — `trimmed := strings.TrimSpace(stamp.Message)`; if empty, **defaults to the literal `"links mutation"`** (`commit_lock.go`).
3. `commit_lock.go` — base args: `{"-Am", trimmed}` (i.e. `DOLT_COMMIT` always stages all with `-A`).
4. `commit_lock.go` — `if stamp.AllowEmpty` append `"--allow-empty"`.
5. `commit_lock.go` — `if !stamp.Date.IsZero()` append `"--date", stamp.Date.UTC().Format(time.RFC3339)`.
6. `commit_lock.go` — `if stamp.Author != ""` append `"--author", stamp.Author`.
7. `commit_lock.go` — `s.db.QueryRowContext(ctx, buildProcedureCall("DOLT_COMMIT", len(args)), args...).Scan(&commitHash)`.
8. `commit_lock.go` — an error whose lower-cased text contains `"nothing to commit"` is **success with no commit** (returns `nil`).
9. `commit_lock.go` — anything else goes to `wrapCommitWorkingSetError`.

Argument order is therefore always: `-Am <message> [--allow-empty] [--date <RFC3339 UTC>] [--author "Name <email>"]`.

`TestCommitWorkingSetOnceRendersStamp` (`commit_lock_test.go`) pins the effect with `Date = 2025-03-07T09:30:45Z`, `Author = "prov-author <prov@example.test>"`, `AllowEmpty = true`, `Message = "stamped provenance probe"`, then asserts via `SELECT committer, email, date, message FROM dolt_log('HEAD') LIMIT 1` (`commit_lock_test.go`) that `committer == "prov-author"`, `email == "prov@example.test"`, the date equals the stamp to the second, and the message matches verbatim.

#### 5.5 Transient online-GC contention

`commit_lock.go` — `var ErrTransientGCContention = errors.New("transient online-gc contention")`.

Budget (`commit_lock.go`):
```go
var transientRetryMaxAttempts = 30      // package var so tests can shrink it
const transientRetryBaseDelay = 50 * time.Millisecond
const transientRetryMaxDelay  = 1 * time.Second
```
`transientRetryDelay(attempt)` (`commit_lock.go`) = `transientRetryBaseDelay << (attempt-1)`, capped at `transientRetryMaxDelay`. Total ≈ 25s: five uncapped doublings (50, 100, 200, 400, 800ms) then 25 more attempts at the 1s cap (`commit_lock.go`).

`retryTransientGCContention` (`commit_lock.go`) loop, attempts 1..`transientRetryMaxAttempts`:
- `classifyTransientGCError(operation(ctx))`; `nil` → return `nil` (`commit_lock.go`).
- If the error is not `ErrTransientGCContention`, or this was the final attempt → break (`commit_lock.go`).
- `sleep(ctx, delayForAttempt(attempt))` — a wait error returns immediately (`commit_lock.go`).
- `rotate(ctx)` — the connection rotator (`s.reconnect`); a rotate error returns immediately (`commit_lock.go`).
- On exit: `exhaustedContentionError(lastErr)`.

`waitWithContext` (`commit_lock.go`) delegates to `filelock.SleepWithContext`.

Classification predicates:
- `isManifestReadOnlyError` (`commit_lock.go`): lower-cased error text contains **both** `"cannot update manifest"` **and** `"read only"`.
- `isOnlineGCResetError` (`commit_lock.go`): lower-cased text contains **both** `"online garbage collection"` **and** `"reconnect"`. The GC-specific phrase is required so the unrelated cluster-role-transition error (which also says "please reconnect") is not misclassified (`commit_lock.go`).
- `isTransientGCContentionError` (`commit_lock.go`) = either of the two.

`transientGCContentionError` (`commit_lock.go`) wraps and its `Is(target)` returns `target == ErrTransientGCContention` (`commit_lock.go`).

`wrapCommitWorkingSetError` (`commit_lock.go`) wraps every commit error as `dolt commit working set: %w`, and additionally tags it as transient when `isTransientGCContentionError`.

`exhaustedContentionError` (`commit_lock.go`): if the surviving error is a manifest-read-only, promote it to `WorkspaceWriteBlockedError{Cause: err}`; otherwise pass through unchanged. A persistent GC-reset (not manifest-read-only) is **not** reclassified.

`WorkspaceWriteBlockedError` (`commit_lock.go`) has one field `Cause error`; `Error()` (`commit_lock.go`) is exactly:

> `another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)`

`Unwrap()` returns `Cause` (`commit_lock.go`).

---

### 6. Other lit-minted lock paths in workspace_lock.go

#### 6.1 Sync-push single-flight lock

- Path (`workspace_lock.go`): `<dirname(databasePath)>/.links-sync-push.lock`.
- `TryAcquireSyncPushLock(databasePath)` (`workspace_lock.go`): `filelock.Acquire(context.Background(), path, true /*exclusive*/, 1, 0)` — a **non-blocking, single-attempt** exclusive probe returning `(release, acquired bool, err)`. `acquired == false` means another mirror holds it and the caller coalesces by doing nothing (`workspace_lock.go`). Note it uses `context.Background()`, not a caller ctx.
- Pinned by `workspace_lock_test.go` (`TestTryAcquireSyncPushLockIsSingleFlight`) and `workspace_lock_test.go` (path is a sibling of dolt).

#### 6.1a Receive single-flight lock

- Path (`workspace_lock.go`): `<dirname(databasePath)>/.links-sync-receive.lock` (`receiveLockPath`).
- `TryAcquireReceiveLock(databasePath)` (`workspace_lock.go`): `filelock.Acquire(context.Background(), path, true /*exclusive*/, 1, 0)` — non-blocking, single attempt; `acquired == false` means another automatic receive is running and the caller does nothing.

#### 6.2 Mirror liveness beacon

- Path (`workspace_lock.go`): `<dirname(databasePath)>/.links-sync-mirror.lock`.
- Budget (`workspace_lock.go`): `coResidentHolderWait` (`store.go`), 2.3s of unchanged holders.
- `HoldMirrorBeacon(ctx, databasePath)` (`workspace_lock.go`): `acquireStoreLock(ctx, storageDir, path, false /*shared*/, coResidentHolderWait)`. On `ErrWorkspaceBusy` it deliberately **does not propagate the sentinel** (`workspace_lock.go`), returning instead:
  > `mirror liveness beacon held exclusively past every probe window (a foreign process holding %s?)`
  with the beacon path interpolated.
- `MirrorBeaconVerdict` (`workspace_lock.go`) is an `int` enum: `BeaconUnheld = 0`, `BeaconAnswered = 1`, `BeaconObstructed = 2` (`workspace_lock.go`). `String()` (`workspace_lock.go`) returns `"unheld"`, `"answered"`, `"obstructed"`, and for any other value `fmt.Sprintf("unnamed MirrorBeaconVerdict(%d)", int(v))`.
- `ProbeMirrorBeacon(databasePath)` (`workspace_lock.go`) — two single-attempt probes with `context.Background()`, **shared first, exclusive last**:
  1. `filelock.Acquire(ctx, path, false, 1, 0)`. Error → `(BeaconUnheld, "probe mirror liveness beacon (shared step): %w")` (`workspace_lock.go`). Not acquired → `(BeaconObstructed, nil)` (`workspace_lock.go`). Release failure → `(BeaconUnheld, "release mirror liveness beacon probe (shared step): %w")` (`workspace_lock.go`).
  2. `filelock.Acquire(ctx, path, true, 1, 0)`. Error → `(BeaconUnheld, "probe mirror liveness beacon: %w")` (`workspace_lock.go`). Not acquired → `(BeaconAnswered, nil)` (`workspace_lock.go`). Release failure → `(BeaconUnheld, "release mirror liveness beacon probe: %w")` (`workspace_lock.go`).
  3. Otherwise `(BeaconUnheld, nil)`.
- Pinned by `workspace_lock_test.go` (`TestMirrorBeaconLivenessProof`) and `workspace_lock_test.go` (path is a sibling of dolt).

#### 6.3 Dolt's own journal lock

- Path (`workspace_lock.go`): `filepath.Join(filepath.Clean(databasePath), doltDatabaseName, ".dolt", "noms", "LOCK")` — i.e. `<databasePath>/<doltDatabaseName>/.dolt/noms/LOCK`. This is **Dolt's** file, not lit-minted, and is the ONE HOME exception stated at the mint site (`workspace_lock.go`).
- Budget (`workspace_lock.go`): `coResidentHolderWait` (`store.go`) → **2.3s** of unchanged holders, polled every `storeLockPollInterval` = 100ms. Not a figure of its own: it is the same wait the write engine open gives the same holder.
- `LockDoltJournalExclusive(ctx, databasePath)` (`workspace_lock.go`):
  1. `requireInitializedWorkspace(databasePath)` **first**, before any acquisition (`workspace_lock.go`). It reads the workspace root, not the journal directory: on `os.ErrNotExist` it returns the package sentinel `ErrWorkspaceNotInitialized` (`workspace_initialized.go`), whose text is exactly:
     `repository not initialized with lit — run 'lit init' first`.
     Any other stat failure returns `stat database dir: %w`.
  2. `os.Stat(filepath.Dir(lockPath))` second (`workspace_lock.go`) — this helper contends on Dolt's lock and never mints Dolt's tree, so an absent noms directory is refused rather than created. Under an existing root that absence is a damaged or half-deleted Dolt tree, **not** an uninitialized repository, so it is never reported as the sentinel; every failure here returns `stat dolt journal dir: %w` and reaches the unclassified-fault default.
  3. `acquireStoreLock(ctx, lockPath, true /*exclusive*/, 700, 100ms)`.
  4. On `ErrWorkspaceBusy`, wraps (preserving the sentinel):
     `another process is holding this workspace's Dolt store open; retry after it completes: %w` (`workspace_lock.go`).
- Engine-open interaction stated at `workspace_lock.go` and `internal/store/doc.go`: a **read** engine opens eagerly inside `openStoreConnection`, attempts the journal lock for **100ms**, and falls back to Dolt's read-only mode; a **write** engine opens eagerly inside `openStoreConnection`, **refuses** the read-only fallback, and retries boundedly (`coResidentHolderWait`, 2.3s of unchanged holders). A live write Store holds the journal lock for its entire lifetime.
- `workspace_lock.go` records the one lifecycle write this hold does not stop: `journal.idx` is opened `O_RDWR` and truncated on every engine bootstrap with no can-write gate, so a snapshot copy can capture a torn index; Dolt's `corruptIndexRecovery` truncates it to zero and rebuilds from the journal on next open.

---

### 7. Remote cache (`internal/store/remotecache.go`)

#### 7.1 What is cached and where

Dolt gives every **git-backed** remote its own bare-repo mirror at
`<db>/.dolt/git-remote-cache/<sha256(url|ref)>/repo.git`, and never deletes one (`remotecache.go`).

Constants:
- `remoteCacheDirName = "git-remote-cache"` (`remotecache.go`)
- `defaultGitRemoteRef = "refs/dolt/data"` (`remotecache.go`) — mirrors dbfactory's `defaultGitRef`; lit never supplies `GitRefParam`.
- `gitBackedURLSchemePrefix = "git+"` (`remotecache.go`)

Base path (`remotecache.go`):
```go
func (s *Store) remoteCacheBase() string {
    return filepath.Join(s.doltRootDir, doltDatabaseName, dbfactory.DoltDir, remoteCacheDirName)
}
```
The middle segment is `doltDatabaseName`, **not** the workspace id (`remotecache.go`).

#### 7.2 Key derivation

`remoteCacheKey(remoteURL) (key string, gitBacked bool, err error)` (`remotecache.go`):
1. `url.Parse(strings.TrimSpace(remoteURL))`; failure → `parse dolt remote url %q: %w` (`remotecache.go`).
2. Lower-case the scheme (`remotecache.go`). If it does **not** start with `git+`, return `("", false, nil)` — not an error, just no mirror (`remotecache.go`).
3. Copy the URL, strip `git+` from the scheme, clear `RawQuery` and `Fragment` (`remotecache.go`).
4. `sha256.Sum256([]byte(underlying.String() + "|" + defaultGitRemoteRef))`, hex-encoded lowercase (`remotecache.go`).

Three distinct outcomes by design (`remotecache.go`): git-backed → key; non-`git+` → no key, prune carries on; unparseable → failure.

`isRemoteCacheKey(name)` (`remotecache.go`): length must equal `sha256.Size*2` = **64**, the name must equal its own lower-casing, and it must hex-decode. Anything else was not written by dbfactory and is never deleted.

`expectedRemoteCacheKeys(remotes []storage.SyncRemote)` (`remotecache.go`) maps key → remote **name**; non-git-backed remotes are skipped; a parse error aborts.

`listRemoteCacheKeys(base)` (`remotecache.go`): `os.ReadDir(base)`; `fs.ErrNotExist` → `(nil, nil)` (a store that never opened a git remote); other errors → `read git remote cache %s: %w`. Only entries that are directories **and** pass `isRemoteCacheKey` are returned.

#### 7.3 The plan

`type remoteCachePlan struct { abandoned []string }` (`remotecache.go`).

`planRemoteCachePrune(expected map[string]string, onDisk []string) (remoteCachePlan, error)` (`remotecache.go`):
- One rule: a directory is abandoned when no configured remote derives its key (`remotecache.go`). `abandoned` is sorted (`remotecache.go`).
- `unaccounted` is every expected key with no directory on disk, rendered `"<remoteName>→<key>"` and sorted (`remotecache.go`).
- **Refusal**: if `len(abandoned) > 0 && len(unaccounted) > 0`, returns a zero plan and the error (`remotecache.go`):

> `declining to prune: %d cache director%s match no configured remote, but %d configured remote%s also %s no directory (%s). That is two possible facts wearing one shape, and this code cannot tell which it is looking at: either the key derivation disagrees with what Dolt actually wrote, or those remotes have simply never been opened — Dolt writes a mirror on first use, never when a remote is configured. While both readings stand an unmatched directory cannot be told apart from a live mirror this code failed to find, so nothing was deleted. One \`lit sync push --remote <name>\` or \`lit sync fetch --remote <name>\` through each remote named above creates its directory and settles it`

with pluralizations `y`/`ies`, ``/`s`, `has`/`have` supplied by `plural(n, one, many)` (`remotecache.go`).

The refusal is deliberately **not** narrowed to the remote being pushed (`remotecache.go`).

A store that has never pushed trips nothing: no directories → nothing to delete (`remotecache.go`; pinned at `remotecache_plan_test.go`).

#### 7.4 Execution

`(*Store).pruneRemoteCache(ctx)` (`remotecache.go`) — **runs without the commit lock** (`remotecache.go`):
1. `s.SyncListRemotes(ctx)` → on error, outcome with `Problem = err.Error()`.
2. `expectedRemoteCacheKeys(remotes)` → same.
3. `s.remoteCacheBase()`, `listRemoteCacheKeys(base)` → same.
4. `planRemoteCachePrune(expected, onDisk)` → same.
5. For each key in `plan.abandoned` (sorted): re-ask `s.remoteCacheKeyIsStillAbandoned(ctx, key)`; skip if it has come back to life; else `collectAbandonedMirror(base, key)`; increment `Removed` and add to `Reclaimed` when collected.
6. **Every entry is attempted**; a failure appends to `problems` and the loop continues, so one permanently unremovable directory is not a head-of-line blocker (`remotecache.go`). `outcome.Problem = strings.Join(problems, "; ")` (`remotecache.go`).

`remoteCacheKeyIsStillAbandoned(ctx, key)` (`remotecache.go`) re-lists remotes and re-derives keys; errors wrap as `re-check abandoned mirror %s: %w` (`remotecache.go`). Pinned at `remotecache_test.go`.

`collectAbandonedMirror(base, key) (reclaimed int64, collected bool, err error)` (`remotecache.go`):
- `dirSize(dir)` **first** so the reclaim figure is measurable (`remotecache.go`).
- `fs.ErrNotExist` → `(0, false, nil)` — already gone is **not** an error (`remotecache.go`).
- Other measure failure → `measure abandoned mirror %s: %w` (`remotecache.go`).
- `os.RemoveAll(dir)` failure → `remove abandoned mirror %s: %w` (`remotecache.go`).
- Success → `(size, true, nil)`.
- Known open window (`remotecache.go`): a sibling taking the directory between the walk and the unlink means both prunes report the same bytes; the reclaim figure is knowingly approximate across concurrent prunes.

`dirSize(root)` (`remotecache.go`) walks with `filepath.WalkDir` and sums `info.Size()` of non-directory entries.

`(*Store).SyncRemoteMirrorHolds(ctx, remote, commits)` (`remotecache.go`) — no network:
1. Empty `commits` → `(false, nil)`.
2. `s.SyncListRemotes(ctx)`; a name not configured → `remote %q is not configured on this store`.
3. `remoteCacheKey(url)`: parse error → the error; not git-backed → `(false, nil)`.
4. `<remoteCacheBase>/<key>/repo.git` absent → `(false, nil)`; other stat failure → `stat git mirror of remote %q: %w`.
5. `gitDirHoldsCommits`: one `git --git-dir <dir> cat-file --batch-check` over the commits, one per stdin line; git failing → `ask git mirror %s for %d commit(s): %w`; an answer count that differs from the input → `ask git mirror %s: %d answer(s) for %d commit(s): %q`; true only when every answer starts `<commit> commit `.

#### 7.5 Outcome and reporting

```go
type remoteCachePruneOutcome struct {
    Removed   int
    Reclaimed int64
    Problem   string
}
```
(`remotecache.go`)

`Report()` (`remotecache.go`) — four exact branches:
- `Problem != "" && Removed > 0`: `remote-cache prune: removed %d abandoned mirror%s (%s), then failed: %s`
- `Problem != ""`: `"remote-cache prune: " + o.Problem`
- `Removed > 0`: `remote-cache prune: removed %d abandoned mirror%s, reclaimed %s`
- default: `""` (empty exactly when the prune looked and found nothing to do)

`humanBytes(n int64)` (`remotecache.go`): below `1024` renders `%d B`; otherwise divides by 1024 repeatedly and renders `%.1f %ciB` with the unit letter drawn from `"KMGTPE"` — i.e. `KiB`, `MiB`, `GiB`, `TiB`, `PiB`, `EiB`. Shared with compaction's `footprintDelta` so both maintenance reporters spell sizes identically.

#### 7.6 Tests pinning remote-cache behavior

- `remotecache_test.go` `TestRemoteCacheKeyMatchesDoltLayout` — pins the derivation against a cache directory Dolt itself created.
- `remotecache_test.go` keeps the live mirror.
- `remotecache_test.go` collects abandoned mirrors.
- `remotecache_test.go` is not blocked by one stuck mirror.
- `remotecache_test.go` re-check follows the remotes, not a snapshot.
- `remotecache_plan_test.go` collects only unmatched dirs.
- `remotecache_plan_test.go` declines when the derivation misses the live mirror.
- `remotecache_plan_test.go` the refusal names up to both causes.
- `remotecache_plan_test.go` already-gone directory is treated as done.
- `remotecache_plan_test.go` reclaims what it removes.
- `remotecache_plan_test.go` a failure names the key.
- `remotecache_plan_test.go` collects when no remote is configured.
- `remotecache_plan_test.go` `TestRemoteCacheKeyPreservesHomeRelativePath` — a home-relative scp URL normalizes to `ssh://git@host/./path` and the `/./` must be preserved (`remotecache.go`).
- `remotecache_plan_test.go` separates non-git remotes from bad URLs.
- `remotecache_plan_test.go` `isRemoteCacheKey` rejects foreign names.
- `remotecache_plan_test.go` `Report()` semantics.

---

### 8. The vendored Dolt driver (`internal/vendor/dolthub-driver`)

#### 8.1 What it is

A vendored copy of `github.com/dolthub/driver` — a `database/sql` driver for an **embedded** Dolt engine (no server process). Package name `embedded` (`driver.go`). Module path is still `github.com/dolthub/driver` (`go.mod`). Registered under the driver name `"dolt"` in `init()` (`driver.go`).

`go.mod` records the vendored-from baselines (`go.mod`) and mirrors the top-level fork replaces so a standalone build also uses the promptctl forks (`go.mod` replace lines): `github.com/dolthub/dolt/go => github.com/promptctl/dolt/go v0.40.5-0.20260821231005-4b80eac34485` and `github.com/dolthub/go-mysql-server => github.com/promptctl/go-mysql-server v0.20.1-0.20260821032251-ab5cb9ec3b69`, plus `github.com/google/flatbuffers => github.com/dolthub/flatbuffers v1.13.0-dh.1`.

#### 8.2 DSN grammar

`ParseDataSource(dataSource)` (`data_source.go`):
- Must start with the literal `file://` (`data_source.go`); otherwise `datasource url '%s' must have a file url scheme` (`data_source.go`).
- Everything after `file://` up to the first `?` is the **directory**; the rest is parsed with `url.ParseQuery` (`data_source.go`).
- Param **names are lower-cased**; values are not (`data_source.go`).
- `ParamIsTrue(name)` (`data_source.go`) is true only when the param exists, has exactly one value, and that value lower-cases to `"true"`.

`ParseDSN(dsn) (Config, error)` (`parse_dsn.go`):
1. `ParseDataSource`.
2. Directory must exist and be a directory: `'%s' does not exist` (`parse_dsn.go`) or `%s: is a file. need to specify a directory` (`parse_dsn.go`).
3. `commitname` required: `datasource %q must include the parameter %q` (`parse_dsn.go`); exactly one value: `param %q must have exactly one value` (`parse_dsn.go`).
4. `commitemail` required, same two messages (`parse_dsn.go`).
5. `database` optional, but if present must have exactly one value (`parse_dsn.go`).
6. `multistatements` and `clientfoundrows` via `ParamIsTrue` (`parse_dsn.go`).
7. The full lower-cased param map is preserved in `Config.Params` (`parse_dsn.go`).

Recognized param names (`driver.go`):
`commitname`, `commitemail`, `database`, `multistatements`, `clientfoundrows`, plus two presence-based flags passed through to Dolt's DB loading layer: `disable_singleton_cache`, `fail_on_journal_lock_timeout`.

Example DSN from the doc comment (`driver.go`):
`file:///User/brian/driver/example/path?commitname=Billy%20Bob&commitemail=bb@gmail.com&database=dbname`

Tests: `parse_dsn_test.go` basics param names are case-insensitive requires commitname and commitemail validates directory exists and is a dir; `data_source_test.go`.

#### 8.3 `Config`

`config.go`. Fields: `DSN`, `Directory` (required), `CommitName`/`CommitEmail` (required — used as Dolt commit metadata), `Database`, `MultiStatements`, `ClientFoundRows`, `Params`, `BackOff backoff.BackOff`, `DisableSingletonCache`, `FailOnJournalLockTimeout`, `Version`.

`BackOff` semantics (`config.go`): nil → engine open attempted **once**; non-nil → retries on retryable errors, and **implies both** `DisableSingletonCache` and `FailOnJournalLockTimeout`. Implementations are stateful; the connector calls `Reset()` before use.

#### 8.4 Connector

`NewConnector(cfg)` (`connector.go`) validates: `config.Directory is required` (`connector.go`), `config.CommitName is required` (`connector.go`), `config.CommitEmail is required` (`connector.go`); defaults `cfg.Version` to `defaultDoltVersion = "0.40.17"` (`connector.go`); re-validates the directory with the same two messages (`connector.go`).

`Connect(ctx)` (`connector.go`): `getOrOpenEngine` → `newLocalContext` → `SetCurrentDatabase(cfg.Database)` when non-empty (`connector.go`) → if `ClientFoundRows`, OR `mysql.CapabilityClientFoundRows` into the session client capabilities (`connector.go`) → returns a `*DoltConn`.

`getOrOpenEngine` (`connector.go`): a single shared engine per connector, guarded by `c.mu` plus an `openCh` channel so concurrent Connects wait on the in-flight open rather than racing. `connector is closed` (`connector.go`) if `Close` already ran; a waiter aborts on `ctx.Done()` returning `ctx.Err()` (`connector.go`). If the open succeeds after `Close`, the engine is immediately closed (`connector.go`).

`Close()` (`connector.go`) sets `closed`, nils the engine and channel, does **not** block on an in-flight open, and closes the engine if one exists.

`openEngineWithRetry` (`connector.go`):
- Dolt user config is a map with `config.UserNameKey → cfg.CommitName` and `config.UserEmailKey → cfg.CommitEmail` (`connector.go`) — **this is the commit author identity the embedded engine stamps**.
- `engine.SqlEngineConfig{IsReadOnly: false, ServerUser: "root", Autocommit: true}` (`connector.go`).
- `disableCache := cfg.BackOff != nil || cfg.DisableSingletonCache`; `failOnLockTimeout := cfg.BackOff != nil || cfg.FailOnJournalLockTimeout` (`connector.go`); each sets the corresponding `dbfactory` key in `seCfg.DBLoadParams` as a presence flag `struct{}{}` (`connector.go`).
- `fs.WithWorkingDir(cfg.Directory)` (`connector.go`).
- If `BackOff == nil`, one call to `open(ctx)` (`connector.go`).
- Else `BackOff.Reset()`, wrap with `backoff.WithContext(bo, ctx)`, and `backoff.Retry`: a retryable error is returned for retry, anything else is wrapped `backoff.Permanent` (`connector.go`). On failure the **last underlying error** is returned in preference to backoff's own (`connector.go`).

`isRetryableOpenErr(err)` (`retryable_open_err.go`) — exactly two shapes: `errors.Is(err, nbs.ErrDatabaseLocked)` (`retryable_open_err.go`) and `errors.Is(err, os.ErrDeadlineExceeded)` (`retryable_open_err.go`). Everything else is permanent.

`openSqlEngine` (`driver.go`):
- Builds a **carrier** `env.DoltEnv{Version: version, DBLoadParams: maps.Clone(seCfg.DBLoadParams)}` when params exist (`driver.go`), because `NewSqlEngine`'s own threading of `DBLoadParams` happens after `MultiEnvForDirectory` has already loaded the databases — too late for params that shape the storage open itself (`driver.go`).
- `loadMultiEnvFromDirWithParams(ctx, cfg, fs, ".", version, carrier)` — passes `"."` because `fs` is already rooted at `dir` (`driver.go`).
- **Forces each env's lazy database load** and surfaces its failure as *the* open error (`driver.go`): iterates `mrEnv`, and if `dEnv.DoltDB(ctx) == nil`, takes `dEnv.DBLoadError` or synthesizes `database %q failed to load`. Without this, `CollectDBs` inside `NewSqlEngine` would panic on a nil DB instead of the retryable `nbs.ErrDatabaseLocked` reaching the backoff.
- `engineConstructMu.Lock()` around `engine.NewSqlEngine` (`driver.go`).

`(*doltDriver).Open(dsn)` (`driver.go`) always returns `dolt SQL driver does not support Open()`; only `OpenConnector` (`driver.go`) works.

#### 8.5 Local modifications versus upstream (behavior-affecting)

1. **Telemetry removed outright** — `connector.go`: upstream fired an unconditional goroutine (`emitUsageEvent`) that dialed `eventsapi.dolthub.com` over gRPC on every engine open, gated only by an env var read at package init. The emission path, its env-gated opt-out, its once-per-24h rate-limit file, and every import that served it are **deleted**, not defaulted off.

2. **Process-wide engine-construction mutex** ("lit patch 5") — `driver.go`: `var engineConstructMu sync.Mutex` serializes `engine.NewSqlEngine` because go-mysql-server's `InitStatusVariables` rewrites the global status-variable table and `NewSqlEngine` re-points the global binlog-consumer singleton. Two concurrent constructions race on those globals **even for unrelated database paths**. Queries against already-constructed engines are unaffected.

3. **`MySQLError` replaces `github.com/go-sql-driver/mysql`'s** ("Patch 4") — `mysql_error.go` is original promptctl work, MIT (`mysql_error.go`), removing an MPL-2.0 SBOM coordinate. Two fields only: `Number uint16` (the protocol's own width) and `Message string`; **no SQL state field** (`mysql_error.go`). `Error()` renders `"Error " + strconv.FormatUint(uint64(Number),10) + ": " + Message` (`mysql_error.go`), MySQL's conventional form. `translateError` (`errors.go`) is its only producer: `sql.CastSQLError(err)` → `&MySQLError{Number: uint16(vitessErr.Num), Message: vitessErr.Message}`; `nil` in, `nil` out. Tests: `errors_test.go`.

4. **Retryable-open plumbing** — `retryable_open_err.go`, the `BackOff`/`DisableSingletonCache`/`FailOnJournalLockTimeout` `Config` knobs (`config.go`), and the `DBLoadParams` mapping (`connector.go`). Pinned by `config_load_params_test.go` (`TestConfigDBLoadParamMapping`), a table with exactly four cases: `{"neither by default", …, false, false}`, `{"backoff implies both", …, true, true}`, `{"cache disable alone", …, true, false}`, `{"fail-fast alone", …, false, true}` (`config_load_params_test.go`). `openconnector_retry_test.go` pins that with `backoff.WithMaxRetries(backoff.NewConstantBackOff(0), 10)` an open failing 3× with `nbs.ErrDatabaseLocked` eventually succeeds and calls ≥ 4 pins that with no BackOff the open is attempted **exactly once** and the error satisfies `errors.Is(err, nbs.ErrDatabaseLocked)` pins that a 150ms `Connect` context bounds the retry (elapsed < 2s).

5. **Forced eager DB load in `openSqlEngine`** — `driver.go` (see §8.4); without it the retryable lock error would surface as a nil-pointer panic.

6. **Relative-path fix** — `openSqlEngine` passes `"."` rather than `dir` because the connector already rooted the filesystem at `cfg.Directory` (`driver.go`, `connector.go`). Pinned by `relative_path_test.go`, both of which assert that re-applying the directory (`LoadMultiEnvFromDir(..., "data/myapp"...)` / `..., cfg.Directory...`) **fails** because the doubled path does not exist.

7. **Peek error must not be dropped** — `statement.go` `peekResultError(peekErr)`: `nil` and `io.EOF` yield a nil `doltRows.err`; anything else is translated and carried, to be surfaced from `Next()` rather than re-driving the iterator (which could return a different outcome and silently convert a real error into an empty result set). `rows.go` returns that carried error from `Next`. Pinned by `peek_error_test.go`.

8. **Test seams left as package vars** (production leaves them nil): `newLocalContextForConnector` (`connector.go`) and `openSqlEngineForConnector` (`driver.go`).

9. **`newResult` error precedence** — `result.go`: the iteration error wins over a `Close` failure; a close failure is only reported when iteration succeeded.

#### 8.6 Query, statement, and rows behavior

`DoltConn.Prepare(query)` (`conn.go`): updates `gmsCtx.SetQueryTime(time.Now())` (safe because statements execute serially on a connection, `conn.go`), then picks multi- vs single-statement from `cfg.MultiStatements`, falling back to `DataSource.ParamIsTrue(MultiStatementsParam)` when `cfg` is nil (`conn.go`).

`prepareMultiStatement` (`conn.go`) splits with `gms.NewMysqlParser()`'s `Parse(ctx, remainder, true)` loop, **skipping** `sqlparser.ErrEmpty` statements (`conn.go`), and wraps every error with `translateError`.

`DoltConn.Close()` (`conn.go`) returns `nil` — it releases nothing; the engine belongs to the connector.

`DoltConn.Begin()` (`conn.go`) delegates to `BeginTx` with `LevelSerializable`, `ReadOnly: false`. `BeginTx` (`conn.go`) accepts **only** `LevelSerializable` or `LevelDefault`; anything else returns `isolation level not supported '%d'` (`conn.go`). It then runs the literal SQL `BEGIN;` (`conn.go`). Pinned by `smoke_test.go`.

`doltTx.Commit()` runs `COMMIT;` (`transaction.go`); `Rollback()` runs `ROLLBACK;` (`transaction.go`); both translate errors.

`doltStmt` (`statement.go`): `Close()` returns nil (`statement.go`); `NumInput()` returns `-1` (`statement.go`).

`argsToBindings(args)` (`statement.go`): positional args become named bindings `v1`, `v2`, … (`statement.go`) via `sqltypes.BuildBindVariable` → `BindVariableToValue` → `sqlparser.ExprFromValue`.

`doltStmt.Exec` (`statement.go`) runs `QueryWithBindings` and drains the iterator through `newResult`. `newResult` (`result.go`) sums `types.OkResult.RowsAffected` into `affected` and takes the **last** `InsertID` into `last` (`result.go`). `LastInsertId`/`RowsAffected` return the stored error if any (`result.go`).

`doltStmt.Query` (`statement.go`): with args it goes through `execWithArgs`; with none through `se.Query`. It then wraps the iterator in a `peekableRowIter` and calls `Peek` **eagerly** — required because inserts and some DML (e.g. `CREATE PROCEDURE`) execute inside the iterator, so a later statement in a multi-statement query would otherwise see un-applied results (`statement.go`).

`isQueryResultSet(row)` (`statement.go`): `nil` row → `true` (a valid empty result set); a one-column row holding a `types.OkResult` → `false`; a zero-column row → `false`; otherwise `true`.

`doltRows.Next` (`rows.go`) type conversions, in order (`rows.go`): `driver.Valuer` → `v.Value()` (error → `error processing column %d: %w`); `types.GeometryValue` → `Serialize()`; schema column of `gms.EnumType` → `Convert` then `At(int(v.(uint16)))`, with errors `could not convert to expected enum type for column %d: %w` and `not a valid enum index for column %d: %v`; schema column of `gms.SetType` → `Convert` then `BitsToString(v.(uint64))`, errors `could not convert to expected set type for column %d: %w` and `could not convert value to set string for column %d: %w`; otherwise the raw value. A column-count mismatch returns `mismatch between expected column count and actual column count` (`rows.go`).

`doltMultiRows` (`rows.go`) implements `driver.RowsNextResultSet`: `HasNextResultSet()` is `(currentIdx+1) < len(rowSets)` (`rows.go`); `NextResultSet()` closes the current set and advances past non-result-set statements, returning `io.EOF` when exhausted (`rows.go`).

`doltMultiStmt.Exec` (`statement.go`) stops at the first error and otherwise returns the **last** result, matching the MySQL driver (`statement.go`). `doltMultiStmt.Query` (`statement.go`) builds lazy producers and advances to the first statement that actually yields a result set (`statement.go`).

#### 8.7 Standalone query splitter

`query_splitter.go` provides `QuerySplitter` (`query_splitter.go`) with `Next()` (`query_splitter.go`, returns `io.EOF` when exhausted, trims whitespace) and `HasMore()` (`query_splitter.go`). `parseNext` (`query_splitter.go`) splits on `;` while tracking a `RuneStack` of open delimiters `(`, `"`, `'`, `` ` `` (`query_splitter.go`): inside a quote, the matching close pops **unless** preceded by a literal backslash (`query_splitter.go`); inside `(`, a `)` pops and any open rune pushes (`query_splitter.go`). Unterminated input returns the whole remaining length (`query_splitter.go`). Pinned by `query_splitter_test.go`. Note: `DoltConn.prepareMultiStatement` uses the gms parser (`conn.go`), not this splitter.

#### 8.8 Suppression of Dolt's human output (lit-side)

`internal/store/dolt_output.go` — a package `init()` sets `doltcli.CliOut = io.Discard`. Rationale stated at `dolt_output.go`: the embedded engine's "N of M chunks complete" redraw (with cursor-control escapes) that `DOLT_CLONE` and `DOLT_FETCH` emit during `init adopt` and `sync pull/fetch` defaults to `os.Stdout`, which is lit's parseable result channel. It is **suppressed**, not relocated to stderr, because lit already owns a single progress voice (`progressf` in `internal/cli/progress.go`). Dolt's error channel `cli.CliErr` is **left untouched** (`dolt_output.go`). Tests: `internal/store/dolt_output_test.go`.

---

### 9. The storage contract assertions (`internal/store/contract.go`)

`contract.go` — compile-time assertions that make the contract a constraint on the engine:

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

Dolt offers all seven capabilities; `storage.Offered` reads this set back at runtime (`contract.go`). The package exports **no** storage vocabulary aliases — this engine's files spell those types `storage.X`, so the engine is reached through the contract or not at all (`contract.go`). What remains exported beyond the `Store` methods is Dolt-era workspace machinery addressed by **filesystem path** rather than engine handle: the workspace and commit flocks, the mirror beacons, bootstrap and remote adoption, snapshot naming, and lifeboat recovery (`contract.go`).


---

