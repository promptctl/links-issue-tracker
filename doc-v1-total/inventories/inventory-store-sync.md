# Behavioral inventory: sync / merge / migration / compaction / backup machinery

Repo: `/Users/bmf/code/links-issue-tracker`. Derived entirely from Go/SQL source and `_test.go` files. Every claim carries a `file:line` citation. Paths are absolute.

---

## PART 1 — Sync entry points (`internal/store/sync.go`)

### 1.1 `OpenSync` — the sync-capable store open

`OpenSync(ctx, doltRootDir, workspaceID) (*Store, error)` at `/Users/bmf/code/links-issue-tracker/internal/store/sync.go`. Exact sequence:

1. `validateOpenArgs(doltRootDir, workspaceID)` — shared argument-validation boundary (`sync.go`).
2. `requireEmbeddedSyncSupport()` (`sync.go`) — version floor check, see §1.2.
3. `acquireWorkspaceShared(ctx, doltRootDir)` (`sync.go`) — workspace shared lock acquired **before** database bootstrap. On any later failure the release is invoked and its error joined onto the returned error (`sync.go`).
4. `requireNoPendingAdopt(doltRootDir)` (`sync.go`) — refuses if an adopt marker is present while the workspace lock is held.
5. `ensureDoltDatabase(ctx, doltRootDir, workspaceID)` (`sync.go`) — same initializer `Store.Open` uses.
6. `openStoreConnection(ctx, doltRootDir, workspaceID, engineWrite)` (`sync.go`) — eager write engine open; waits on Dolt's journal lock bounded by `coResidentHolderWait`, and records itself as the lock's holder for the engine's life.
7. `s.releaseWorkspaceLock = release` (`sync.go`).
8. Branch normalization: `masterRenameSource(ctx, s.db)` is read lock-free; only when it returns a non-empty source is `ensureMasterDefaultBranch` run inside `s.withCommitLock` (`sync.go`). A read-only OpenSync therefore takes no commit lock.
9. On error in step 8: `wrapEngineOpenContention(err, doltRootDir)`, then `s.closeEngine()` (retires the LOCK holder record, closes the engine, normalizes `context.Canceled` on the close alone) whose error is joined; `s.releaseWorkspaceLock` is set to nil (`sync.go`).

`coResidentHolderWait` = 2.3s, a package var (`store.go`) read by the connector's `holdWait` through `coResidentHolderWaitNow`. It is derived from the longest routine hold on the live store (`store.go`): `mirrorHoldCeiling` 1.5s (= `mirrorHoldBudget` 1s + `mirrorHoldCancelLag` 500ms) + `coResidentWaitHeadroom` 800ms (8 × `storeLockPollInterval` 100ms). The wait counts from the last new holder record to appear under the lock (`holdWait`, `lock_holder.go`), so a queue of short holders is waited out and a holder standing still for the wait fails the contender naming it; the explicit push and the inline receive's fetch, which still hold the store across the network, are such holders. The push the mirror runs from its clone has a separate chain (`store.go`): `mirrorPushObservedTail` 20s × `mirrorPushStallFactor` 2 = `mirrorPushDeadline` 40s (exported as `var MirrorPushDeadline`), with `MirrorPushCancelLagObserved` 22s the lag a cut push takes to unwind; no store wait is derived from it.

### 1.2 Embedded-dependency version floor

Constants at `/Users/bmf/code/links-issue-tracker/internal/store/sync.go`:
- `minEmbeddedDoltVersion = "v0.40.5-0.20260314011441-62975ef6bf36"`
- `minEmbeddedDriverVersion = "v0.2.1-0.20260314000741-0fe74e7ee31a"`

`requireEmbeddedSyncSupport` (`sync.go`) reads `debug.ReadBuildInfo()` via `readEmbeddedModuleVersions` (`sync.go`); if build info is absent or the dep map is empty it returns `nil` (no check). `validateEmbeddedSyncSupport` (`sync.go`) maps `github.com/dolthub/dolt/go` → min dolt version and `github.com/dolthub/driver` → min driver version (`sync.go`). A module absent from the map (empty version string) is **skipped** (`sync.go`). Otherwise `semver.Compare(actual, minimum) < 0` returns error `"embedded sync requires %s %s or newer (found %s)"` (`sync.go`). Tests: `TestValidateEmbeddedSyncSupportAcceptsRequiredVersions` (`sync_test.go`), `TestValidateEmbeddedSyncSupportRejectsOlderVersions` (`sync_test.go`).

### 1.3 Remote management

- `SyncListRemotes` (`sync.go`): `SELECT name, url FROM dolt_remotes ORDER BY name`. Returns `[]storage.SyncRemote` (never nil — initialized to `[]` at `sync.go`). Errors: `"list dolt remotes: %w"`, `"scan dolt remote: %w"`, `"iterate dolt remotes: %w"`.
- `SyncAddRemote(ctx, name, url)` (`sync.go`): trims/requires both args via `requireSyncArg` (`sync.go`), then under `runSyncMutation` calls `CALL DOLT_REMOTE(?, ?, ?)` with `"add", name, url` (`sync.go`). Error: `"add dolt remote %q: %w"`.
- `SyncRemoveRemote(ctx, name)` (`sync.go`): `CALL DOLT_REMOTE(?, ?)` with `"remove", name` (`sync.go`). Error `"remove dolt remote %q: %w"`.
- `SyncFetch(ctx, remote, prune)` (`sync.go`): args are `[remote]`, and when `prune` is true `"--prune"` is **prepended** (`sync.go`), then `CALL DOLT_FETCH(...)`. Error `"fetch remote %q: %w"`.
- Test: `TestSyncRemoteLifecycle` (`sync_test.go`), `TestSyncRemoteValidation` (`sync_test.go`).

`requireSyncArg(field, value)` (`sync.go`) trims whitespace and errors `"%s is required"` when the trimmed value is empty.

### 1.4 `GitBackedRemoteURL` — git remote → Dolt transport URL

`GitBackedRemoteURL(raw string) string` at `sync.go`:
1. Trim; empty input returns `""` (`sync.go`).
2. Try `doltenv.NormalizeGitRemoteUrl(trimmed)`; on `ok && err == nil` return the normalized value (`sync.go`).
3. Otherwise append a synthetic `".git"`, re-run the normalizer, and on success return the result with a trailing `".git"` stripped via `strings.TrimSuffix` (`sync.go`).
4. Otherwise return the trimmed input unchanged (`sync.go`).

Tests: `TestGitBackedRemoteURL` (`sync_test.go`), `TestGitBackedRemoteURLIsIdempotent` (`sync_test.go`), `TestGitBackedRemoteURLRoundTripsThroughDolt` (`sync_test.go`).

### 1.5 Freshness (`SyncFreshness`) — the divergence classifier

`SyncFreshness(ctx, remote, branch) (storage.SyncFreshness, error)` at `sync.go`. Pure local read; never touches the network.

1. Both args required (`sync.go`).
2. Tracking ref string is `fmt.Sprintf("remotes/%s/%s", remote, branch)` (`sync.go`).
3. Existence probe: `SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?` (`sync.go`). Count 0 → return with `Synced=false`, `Ahead=0`, `Behind=0`, `OldestDivergedUnix=0` — the never-synced state; the range queries are **not** run (`sync.go`).
4. `Synced = true`; read `SELECT ACTIVE_BRANCH()` (`sync.go`).
5. `ahead = commitRangeStats(trackingRef, localBranch)`; `behind = commitRangeStats(localBranch, trackingRef)` (`sync.go`). Errors wrapped `"summarize commits ahead of %q: %w"` / `"summarize commits behind %q: %w"`.
6. `OldestDivergedUnix` is populated **only** when `ahead > 0 && behind > 0`, as `earlierValidUnix(aheadOldest, behindOldest)` (`sync.go`). Ahead-only or behind-only leaves it 0.

`commitRangeStats(from, to)` (`sync.go`) runs `SELECT COUNT(*), UNIX_TIMESTAMP(MIN(date)) FROM dolt_log(?)` with the bound range expression `"<from>..<to>"` (`sync.go`). The timestamp is scanned as `sql.NullString` because the driver renders `UNIX_TIMESTAMP` of a Datetime3 column as a fractional decimal string (`sync.go`), then parsed by `parseUnixSeconds`.

`parseUnixSeconds(raw sql.NullString) (sql.NullInt64, error)` (`sync.go`): invalid/blank → `{}, nil`; `strconv.ParseFloat` failure → error (wrapped by caller as `"parse oldest commit time %q: %w"`, `sync.go`); otherwise `int64(secs)` — sub-second truncated. Test `TestParseUnixSeconds` (`sync_helpers_test.go`).

`earlierValidUnix(a, b sql.NullInt64) int64` (`sync.go`): both valid → min; one valid → that one; neither → 0. Test `TestEarlierValidUnix` (`sync_helpers_test.go`).

**State mapping** — `storage.SyncFreshness.State()` at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`:
- `!Synced` → `SyncNeverSynced` (`"never_synced"`)
- `Ahead==0 && Behind==0` → `SyncUpToDate` (`"up_to_date"`)
- `Behind==0` → `SyncAhead` (`"ahead"`)
- `Ahead==0` → `SyncBehind` (`"behind"`)
- else → `SyncDiverged` (`"diverged"`)

Constants at `storage/sync.go`. Test `TestSyncFreshnessStateClassification` (`sync_test.go`); `TestSyncFreshnessTracksAheadBehindAgainstRemote` (`sync_test.go`); `TestSyncFreshnessRequiresRemoteAndBranch` (`sync_test.go`).

### 1.6 `SyncReceive` — fetch + fast-forward only

`SyncReceive(ctx, remote, branch)` at `sync.go`. Runs inside `runSyncMutation` (commit lock + GC retry):
1. `CALL DOLT_FETCH(?)` with the remote (`sync.go`). Error `"fetch remote %q: %w"`.
2. `settleReceivedWithinLock` (`sync.go`), which `SyncSettleReceived` also runs alone inside its own `runSyncMutation`, with no fetch: one `SyncFreshness` read (`sync.go`). `Ahead`, `Behind`, `OldestDivergedUnix` are copied onto the result (`sync.go`).
3. Switch on `fresh.State()` (`sync.go`):
   - `SyncBehind` → `execProcedureDiscard(DOLT_MERGE, "--ff-only", "remotes/<remote>/<branch>")`; error `"fast-forward to %q: %w"`; state `SyncReceiveFastForwarded`.
   - `SyncDiverged` → state `SyncReceiveDiverged`, **no merge performed**.
   - `SyncAhead` → `SyncReceiveAhead`.
   - `SyncNeverSynced` → `SyncReceiveNeverSynced`.
   - default → `SyncReceiveUpToDate`.

Fast-forward is the only local-data-touching outcome. State constants and their string values at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`: `"up_to_date"`, `"fast_forwarded"`, `"ahead"`, `"diverged"`, `"never_synced"`. Test: `TestSyncReceiveFastForwardsWhenBehindAndDefersDivergence` (`sync_test.go`).

### 1.6a `LandFetchedHead` — a clone's fetch carried to the live store

`LandFetchedHead(ctx, doltRootDir, cloneRootDir, remote, branch) (LandedFetch, error)` at `fetched_head.go`; no network, no SQL engine. `LandedFetch` carries `Record` (`FetchedHeadRecord`: `moved` / `unchanged` / `absent`) and `Held`, the landing's work under the hold. Opens the clone's chunk store (`openCloneChunkStore`: journal on, singleton cache off, no lock wait; error `"open the clone's chunk store at %s: %w"`) and reads its `refs/remotes/<remote>/<branch>` (`resolveTrackingHead`); absent → `FetchedHeadAbsent` with nothing written. Before any hold, `planRemoteCacheLanding` lists `<db>/links/.dolt/git-remote-cache` of both stores: a cache key the live store lacks is planned as a whole-directory move; a key both hold is planned as a copy of the clone's `refs/dolt/` refs the live mirror does not hold (two `git for-each-ref` per key); a key with nothing new is skipped. Then, under the workspace shared lock, `requireInitializedWorkspace` and `requireNoPendingAdopt`, runs inside `holdForRefWrite` (`pushed_head.go`; the hold `RecordPushedHead` also takes: chunk-store open with LOCK waited out for `coResidentHolderWait`, then a budget — here `InlineReceiveDeadline`, for `RecordPushedHead` `MirrorHoldBudget` — then the commit lock; a cut wraps `ErrMirrorHoldCut`):
1. `PullChunks` from the clone into the live store for the fetched head, temp files under `<db>/links/.dolt/temptf` (error `"copy the fetched chunks of %s into the live store: %w"`).
2. `landTrackingRef`: the fetched head must read as a non-ghost commit; the ref already naming it → `FetchedHeadUnchanged`; otherwise `SetHead` → `FetchedHeadMoved`, whatever the two heads' relation.
3. The cache plan's `apply`: `os.Rename` for each move; one `git --git-dir <live repo.git> fetch --no-tags --quiet <clone repo.git> <ref>:<ref>…` for each copy. A planning or apply failure is returned after the hold as `ErrRemoteCacheNotLanded` wrapping it, with the result.

### 1.7 `SyncPull` — receive, then reconcile only on divergence

`SyncPull(ctx, remote, branch) (storage.SyncPullResult, error)` at `sync.go`. The whole converge runs under **one** `s.withCommitLock` (`sync.go`); nested acquisitions short-circuit because `acquireCommitLock` is context-reentrant (`commit_lock.go`).

Sequence (`sync.go`):
1. `s.SyncReceive`. Copy `Ahead`, `Behind`, `OldestDivergedUnix`.
2. Map receive state → pull state:
   - `SyncReceiveUpToDate` → `SyncPullUpToDate`
   - `SyncReceiveFastForwarded` → `SyncPullFastForwarded`
   - `SyncReceiveAhead` → `SyncPullAhead`
   - `SyncReceiveNeverSynced` → `SyncPullNeverSynced`
   - `SyncReceiveDiverged` → run `s.SyncReconcile(ctx, remote, branch)` and map its state:
     - `SyncReconcileLinearized` → `SyncPullLinearized`, then **re-read** `SyncFreshness` and overwrite `Ahead`, `Behind`, `OldestDivergedUnix` from it (`sync.go`).
     - `SyncReconcileProsePending` → `SyncPullProsePending`, `result.Pending = rec.Pending` (`sync.go`).
     - `SyncReconcileUnrelated` → `SyncPullUnrelated`, `result.Unrelated = rec.Unrelated`; the receive's ahead/behind and fork timestamp ride along unchanged (`sync.go`).
     - `SyncReconcileNotDiverged` → `SyncPullUpToDate` **and** `result.OldestDivergedUnix = 0` (`sync.go`).
     - any other reconcile state → error `"sync pull: unhandled reconcile state %q"` (`sync.go`).
   - any other receive state → error `"sync pull: unhandled receive state %q"` (`sync.go`).
3. On any error the zero `SyncPullResult` is returned (`sync.go`).

`SyncPull` deliberately does not call Dolt's native `DOLT_PULL`; the documented reason (`sync.go`) is that native pull's three-way working-set merge requires `autocommit` off and aborts with `"@autocommit must be disabled so that merge conflicts can be resolved"` under the driver's default.

Pull state constants at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`: `"up_to_date"`, `"fast_forwarded"`, `"linearized"`, `"prose_pending"`, `"unrelated_histories"`, `"ahead"`, `"never_synced"`. `SyncPullResult` fields and JSON tags at `storage/sync.go`.

Tests: `TestSyncPullStateTransitions` (`sync_reconcile_schema_skew_test.go`), `TestSyncPullHealsSchemaSkewDivergence` (`sync_reconcile_schema_skew_test.go`).

### 1.8 `SyncPush` and `SyncCompactAndPush`

`SyncPush(ctx, remote, branch, setUpstream, force)` (`sync.go`) — runs `pushWithinLock` inside `runSyncMutation`. **No compaction.** Rationale in the source: `DOLT_GC` transitions the embedded store read-only mid-run (`sync.go`).

`SyncCompactAndPush(...)` (`sync.go`) — inside one `runSyncMutation`:
1. `depth, depthErr = s.chooseCompactionDepth()` measured **inside** the lock (`sync.go`).
2. `s.compactWithinLock(ctx, depth)` (`sync.go`).
3. `s.pushWithinLock(...)` (`sync.go`).
Then, **after** the push and **outside** the commit lock (`sync.go`):
`result.Maintenance = joinMaintenance(compactionReport(depth, depthErr), s.pruneRemoteCache(ctx).Report())`. A prune failure never fails the push (`sync.go`).

`pushWithinLock` (`sync.go`):
1. `requireSyncArg("remote", remote)`; branch is only `strings.TrimSpace`d and **may be empty** (`sync.go`).
2. If branch is non-empty, `s.guardRemoteSchemaAhead(ctx, remote, branch)` runs first (`sync.go`). An empty branch skips the guard entirely.
3. Args built in order: `"--set-upstream"` if `setUpstream`, `"--force"` if `force`, then the remote, then `fmt.Sprintf("HEAD:%s", branch)` if branch non-empty (`sync.go`).
4. `head` = `headCommitWithinLock` (`SELECT commit_hash FROM dolt_log() LIMIT 1`, trimmed; error `"read head commit: %w"`), read before the push so a failed read fails an attempt that has sent nothing (`sync.go`).
5. `CALL DOLT_PUSH(...)` scanned into `(result.Status int64, message sql.NullString)` (`sync.go`). Error `"push remote %q: %w"`.
6. `result.Message = nullStringValue(message)` — NULL→`""`, otherwise trimmed; `result.Head = head` (`sync.go`).

`SyncPushFromClone(ctx, remote, branch, setUpstream, force)` (`sync.go`) — requires both remote and branch (`requireSyncArg`), then inside one `runSyncMutation`: `pushWithinLock`; on success returns its result. On a push error it runs `DOLT_FETCH <remote>` under `runRemoteIO` and `SyncFreshness(remote, branch)`; a failed fetch or freshness read returns the push error with the failed check joined (`"%w (and whether a concurrent push superseded it could not be checked …)"`). `!fresh.Synced || fresh.Ahead > 0` returns the push error unchanged. Otherwise the result is `SyncPushResult{Head: <HEAD>, Superseded: <push error text>}` and no error. Test: `TestSyncPushFromCloneReportsARaceItLostAsSuperseded` (`sync_push_from_clone_test.go`).

Tests: `TestSyncPushDelivers` (`sync_test.go`), `TestSyncCompactAndPushDelivers` (`sync_test.go`), `TestSyncCompactAndPushDeepensOnAFragmentedOldGeneration` (`sync_test.go`).

### 1.9 `SyncStatus`

`SyncStatus(ctx)` (`sync.go`) runs, in order:
- `SELECT DOLT_VERSION()` → `report.EngineVersion`; error `"read dolt version: %w"`.
- `SELECT ACTIVE_BRANCH()` → `report.Branch`; error `"read active branch: %w"`.
- `SELECT commit_hash, message FROM dolt_log() LIMIT 1` → `HeadCommit`, `HeadMessage` (NULL→`""` trimmed); error `"read head commit: %w"`.
- `SyncListRemotes`.
- `SELECT table_name, staged, status FROM dolt_status ORDER BY table_name, staged` → `[]SyncStatusRow` (initialized to `[]`, `sync.go`). Errors `"read dolt status: %w"`, `"scan dolt status row: %w"`, `"iterate dolt status rows: %w"`.

Report type at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`.

### 1.10 Reset / adopt primitives

- `LocalIssueCount(ctx)` (`sync.go`): first `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'issues'`; count 0 → returns `0, nil` (no error for a pristine store). Otherwise `SELECT COUNT(*) FROM issues`. Test `TestLocalIssueCountAcrossLifecycle` (`sync_test.go`).
- `SyncResetToRemoteHead(ctx, remote, branch)` (`sync.go`): both args required; builds `remotes/<remote>/<branch>`; runs `resetHardToRef` under `runSyncMutation`. Destructive of local commits by design (`sync.go`). Tests `TestSyncResetToRemoteHeadAdoptsUnrelatedHistory` (`sync_test.go`), `TestSyncResetToRemoteHeadRequiresRemoteAndBranch` (`sync_test.go`).
- `resetHardToRef(ctx, db, ref)` (`sync.go`): `CALL DOLT_RESET('--hard', ref)`; error `"reset to remote head %q: %w"`. Shared by the init-adopt and the take-remote resolution.

### 1.11 Procedure-call plumbing

- `callIntProcedure(ctx, db, procedure, args...)` (`sync.go`) — scans a single int64 status column.
- `execProcedureDiscard(ctx, db, procedure, args...)` (`sync.go`) — drains all rows and returns `rows.Err()`; column-count agnostic, used for `DOLT_MERGE`, `DOLT_CHECKOUT`, `DOLT_BRANCH`.
- `buildProcedureCall(procedure, argCount)` (`sync.go`) — `"CALL P()"` for zero args, else `"CALL P(?,?,…)"`.
- `stringArgsToAny` (`sync.go`), `nullStringValue` (`sync.go`).
- `runSyncMutation` (`sync.go`) = `withCommitLock` → `retryTransientGCContention(op, s.reconnect)`.

---

## PART 2 — Commit lock, transient-GC retry, and the commit boundary (`internal/store/commit_lock.go`)

### 2.1 Lock mechanics

- The commit lock is an **flock** via `acquireStoreLock` (`commit_lock.go`). Death of the holder releases it; there is no staleness/eviction heuristic.
- Path: `commitLockPathForDolt` (`commit_lock.go`) = `filepath.Join(filepath.Dir(filepath.Clean(databasePath)), ".links-commit-flock.lock")`. The historical name `.links-commit.lock` is deliberately not used because O_EXCL-era binaries unlink it, splitting the lock across inodes (`commit_lock.go`). Exported as `CommitLockPath` (`commit_lock.go`).
- Re-entrancy: `acquireCommitLock` (`commit_lock.go`) checks `ctx.Value(commitLockContextKey{})`; if already true it returns a no-op release. Otherwise it acquires and returns a ctx with the marker set.
- Budget: `commitLockWaiterBudget()` = `coResidentHolderWait` + `rotationReserve()` (`commit_lock.go`) — 5.6s of unchanged holders, sized for a mutation that suffered one GC-contention rotation and strictly above `rotationReserve()` (`commit_lock.go`).
- `wrapCommitLockContention` (`commit_lock.go`): only when `errors.Is(err, ErrWorkspaceBusy)` does it prepend `"another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes: %w"`. Every other error, cancellation included, passes through untouched.
- `LockCommitPath(ctx, lockPath)` (`commit_lock.go`) — the same primitive for callers with no open Store.
- `SettleCommitLockRelease(opErr, releaseErr)` (`commit_lock.go`): release error with no op error → printed to stderr as `"lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): %v"` and the operation returns **nil**; with an op error → `errors.Join`.

### 2.2 Transient online-GC contention

- Sentinel `ErrTransientGCContention = errors.New("transient online-gc contention")` (`commit_lock.go`).
- `retryTransientGCContention` (`commit_lock.go`): loop `attempt=1..30`. Run `classifyTransientGCError(operation(ctx))`. nil → return nil. If not transient, or last attempt → break. Else `sleep(ctx, delay)` (a sleep error is returned immediately), then `rotate(ctx)` (a rotate error is returned immediately). After the loop, `exhaustedContentionError(lastErr)`.
- `exhaustedContentionError` (`commit_lock.go`): if the surviving error `isManifestReadOnlyError`, promote to `WorkspaceWriteBlockedError{Cause: err}`; otherwise return unchanged.
- `WorkspaceWriteBlockedError.Error()` (`commit_lock.go`): `"another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)"`. `Unwrap` preserves the cause (`commit_lock.go`).
- Classification predicates:
  - `isManifestReadOnlyError` (`commit_lock.go`): lowercased message contains **both** `"cannot update manifest"` and `"read only"`.
  - `isOnlineGCResetError` (`commit_lock.go`): lowercased message contains **both** `"online garbage collection"` and `"reconnect"`. The GC-specific phrase is required so the cluster-role transition error (which also says "please reconnect") is not misclassified.
  - `isTransientGCContentionError` (`commit_lock.go`) = either of the above.
- Tests: `TestReconnectRotatorRecoversPoisonedOperation` (`sync_test.go`), `TestStagedWorkingSetSurvivesReconnect` (`sync_test.go`), and `/Users/bmf/code/links-issue-tracker/internal/store/retry_test.go`.

### 2.3 `commitStamp` and the single commit boundary

`commitStamp` (`commit_lock.go`): `Message string`, `Date time.Time`, `Author string`, `AllowEmpty bool`.

`commitWorkingSetOnce(ctx, stamp)` (`commit_lock.go`) — the only function that hands a commit to Dolt:
1. Optional test hook `s.commitWorkingSetHookForTest` (`commit_lock.go`).
2. `strings.TrimSpace(stamp.Message)`; empty → default literal `"links mutation"` (`commit_lock.go`).
3. Args begin `["-Am", trimmed]` (`commit_lock.go`).
4. `--allow-empty` appended when `stamp.AllowEmpty` (`commit_lock.go`).
5. `--date <UTC RFC3339>` appended when `Date` is non-zero — **RFC3339 without fractional seconds; sub-second precision truncates** (`commit_lock.go`, documented at `commit_lock.go`).
6. `--author <Author>` appended when `Author != ""` (`commit_lock.go`).
7. `CALL DOLT_COMMIT(?…)` scanning one commit hash (`commit_lock.go`).
8. On error: if the lowercased message contains `"nothing to commit"` → return **nil** (success-with-no-commit) (`commit_lock.go`). Otherwise `wrapCommitWorkingSetError` (`commit_lock.go`) wraps as `"dolt commit working set: %w"` and, if transient, boxes it as `transientGCContentionError` so `errors.Is(err, ErrTransientGCContention)` is true (`commit_lock.go`).

`commitWorkingSet(ctx, message)` (`commit_lock.go`) = commit lock + transient retry around `commitWorkingSetOnce`.

`withStampedMutation` (`commit_lock.go`) runs the **whole** `BeginTx → fn → tx.Commit → commitWorkingSetOnce` sequence inside the retry, with an explicit two-phase resume marker `staged bool`: once `tx.Commit()` succeeds, `staged=true` and a retry resumes **at versioning only**, never re-running `fn` (`commit_lock.go`). `withMutation` (`commit_lock.go`) is the message-only spelling.

---

## PART 3 — Compaction (`internal/store/compaction.go`)

### 3.1 Depth vocabulary

`GCMode` is a type alias for `storage.GCMode` (`compaction.go`); `GCNewGen`/`GCFull` re-exported (`compaction.go`). Contract definition at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`: `GCNewGen GCMode = iota` (0), `GCFull` (1). `Valid()` accepts exactly those two (`storage/sync.go`). `String()` returns `"newgen"`, `"full"`, else `fmt.Sprintf("unknown(%d)", int(m))` (`storage/sync.go`).

`gcProcedureArgs(m)` (`compaction.go`):
- `GCNewGen` → `nil, nil` (i.e. `CALL DOLT_GC()` with no args).
- `GCFull` → `[]string{"--" + cli.FullFlag}, nil` — the flag spelling comes from Dolt's own constant.
- anything else → error `"compaction depth %s has no Dolt spelling"`.
Tests: `TestGCProcedureArgsRendersEachDepth` (`compaction_test.go`), `TestGCProcedureArgsRefusesAnUnknownDepth` (`compaction_test.go`).

### 3.2 Thresholds and layout constants

At `compaction.go`:
- `journalDueBytes int64 = 16 << 20` (16 MiB). Rationale recorded in source: Dolt's own auto-GC thresholds at 128 MB; 16 MB bounds a shallow pass's stall below a second (measured 0.26s on a 5.9 MB journal), arrives roughly every 350 mutations, caps journal waste at ~11 MB.
- `archivesDueCount = 64` — count of old-generation archive files. Each shallow pass appends one archive and removes none, so this counts passes since the last deep collection.
- `archiveFileExt = ".darc"` — Dolt's old-generation archive suffix.
- `oldGenDirName = "oldgen"` — the old generation's directory inside the chunk store (Dolt exports no constant for it).

`nomsDir(doltRootDir)` (`compaction.go`) = `filepath.Join(doltRootDir, doltDatabaseName, dbfactory.DoltDir, dbfactory.DataDir)`.

### 3.3 `storeFootprint` and measurement

`storeFootprint{JournalBytes int64; OldGenArchives int}` (`compaction.go`).

`measureFootprint(doltRootDir)` (`compaction.go`):
1. `os.Stat(filepath.Join(noms, chunks.JournalFileID))` — Dolt's own exported journal filename constant. `err == nil` → `JournalBytes = info.Size()`; `os.IsNotExist` → left 0; any other error → `"measure chunk journal: %w"` (`compaction.go`).
2. `os.ReadDir(filepath.Join(noms, "oldgen"))`. Count entries that are **not directories** and whose `filepath.Ext(name) == ".darc"` (`compaction.go`). `os.IsNotExist` → 0; any other error → `"measure old generation: %w"`.
A store directory that does not exist yields an empty footprint with no error.
`(*Store).measureFootprint()` (`compaction.go`) delegates using `s.doltRootDir`.

Tests: `TestMeasureFootprintReadsJournalAndArchives` (`compaction_test.go`), `TestMeasureFootprintReadsAnAbsentStoreAsEmpty` (`compaction_test.go`), `TestMeasureFootprintCountsOnlyArchives` (`compaction_test.go`), `TestMeasureFootprintMatchesDoltsRealOldGenLayout` (`sync_test.go`).

### 3.4 The legality/due rule

`dueMode(footprint) (GCMode, bool)` (`compaction.go`) — pure:
1. `OldGenArchives >= 64` → `(GCFull, true)`. **Tested first** because the deep pass subsumes the shallow one.
2. `JournalBytes >= 16 MiB` → `(GCNewGen, true)`.
3. otherwise → `(GCNewGen, false)`.
Test `TestDueModeSelectsDepthByFootprint` (`compaction_test.go`).

### 3.5 Compaction entry points

`compactWithinLock(ctx, mode)` (`sync.go`) — caller must already hold the commit lock:
1. `gcProcedureArgs(mode)`.
2. `CALL DOLT_GC(args…)`; error `"compact dolt store (%s): %w"`.
3. `s.reconnect(ctx)` — mandatory, because online GC poisons the active SQL connection (`sync.go`).

`SyncCompact(ctx, mode)` (`sync.go`):
1. **Door guard**: `!mode.Valid()` → `"compact: illegal depth %d (want %q or %q)"` with `storage.GCNewGen`/`storage.GCFull` named (`sync.go`). Test `TestSyncCompactRefusesAnIllegalDepth` (`sync_test.go`).
2. `before, beforeErr := s.measureFootprint()` — **outside** the lock.
3. `runSyncMutation(compactWithinLock(mode))`.
4. `after, afterErr := s.measureFootprint()` — also outside the lock.
5. Returns `CompactionOutcome{Ran: true, Depth: mode, Detail: footprintDelta(before, after, errors.Join(beforeErr, afterErr))}`.
Test `TestSyncCompactRunsCleanlyAndPreservesData` (`sync_test.go`).

`CompactIfDue(ctx)` (`sync.go`) — the backstop gate:
1. `s.measureFootprint()`; a measurement error returns `"measure store footprint: %w"` (explicitly *not* "nothing due") (`sync.go`).
2. `mode, due := dueMode(footprint)`; `!due` → zero `CompactionOutcome{}` and **nil error** (`sync.go`).
3. else `s.SyncCompact(ctx, mode)`.

`chooseCompactionDepth()` (`sync.go`) — the push path's depth selector:
1. Measurement failure → returns `(GCNewGen, err)` — a usable floor **and** the error. The push always compacts at least the new generation; the footprint can only deepen, never cancel (`sync.go`).
2. `dueMode` due → that mode.
3. otherwise `GCNewGen, nil`.

### 3.6 Reporting vocabulary

- `footprintDelta(before, after, measureErr)` (`compaction.go`): on `measureErr != nil` → `"footprint not measured: %v"`. Otherwise exactly `"journal %s -> %s, old-generation archives %d -> %d"` with `humanBytes` for the two sizes.
- `compactionReport(mode, measureErr)` (`compaction.go`):
  - `measureErr != nil` → `"compaction: ran %s pass; could not measure whether a deeper one is due: %v"`.
  - `mode == GCFull` → `"compaction: ran full pass, rewriting the old generation"`.
  - default → `""` (a routine shallow pass says nothing).
  Test `TestCompactionReportSpeaksOnlyWhenItHasSomethingToSay` (`compaction_test.go`).
- `joinMaintenance(reports…)` (`compaction.go`): drops empty strings and joins the rest with `"; "`. Test `TestJoinMaintenanceDropsSilentReports` (`compaction_test.go`).
- `humanBytes(n)` (`/Users/bmf/code/links-issue-tracker/internal/store/remotecache.go`): `< 1024` → `"%d B"`; otherwise `"%.1f %ciB"` over the unit table `"KMGTPE"`.
- `CompactionOutcome{Ran bool; Depth GCMode; Detail string}` at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`. `Detail` is empty **only** for the zero outcome that did nothing; a pass that ran but could not measure reports the failure in `Detail`.

### 3.7 Remote-cache prune (the other half of push maintenance)

- Key derivation `remoteCacheKey(remoteURL)` (`remotecache.go`): parse URL; if the lowercased scheme lacks the `git+` prefix (`gitBackedURLSchemePrefix`) return `("", false, nil)`. Otherwise strip `git+` from the scheme, clear `RawQuery` and `Fragment`, and key = `hex(sha256(underlying.String() + "|" + defaultGitRemoteRef))` (`remotecache.go`).
- `isRemoteCacheKey(name)` (`remotecache.go`): exactly 64 chars (`sha256.Size*2`), all-lowercase, valid hex.
- `remoteCacheBase()` (`remotecache.go`) = `filepath.Join(s.doltRootDir, doltDatabaseName, dbfactory.DoltDir, remoteCacheDirName)`.
- `listRemoteCacheKeys(base)` (`remotecache.go`): a non-existent base returns `(nil, nil)`; otherwise directory entries that are dirs and pass `isRemoteCacheKey`.
- **The legality rule** `planRemoteCachePrune(expected, onDisk)` (`remotecache.go`): a directory is abandoned when no configured remote derives its key. If `len(abandoned) > 0 && len(unaccounted) > 0` the prune **declines wholesale** with the long error at `remotecache.go` (names the counts and the `remote→key` pairs). Both lists are sorted (`remotecache.go`).
- `remoteCachePruneOutcome{Removed int; Reclaimed int64; Problem string}` (`remotecache.go`). `Report()` (`remotecache.go`):
  - problem + removals → `"remote-cache prune: removed %d abandoned mirror%s (%s), then failed: %s"`
  - problem only → `"remote-cache prune: " + problem`
  - removals only → `"remote-cache prune: removed %d abandoned mirror%s, reclaimed %s"`
  - neither → `""`.
- `dirSize(root)` (`remotecache.go`) walks and sums file sizes for the `Reclaimed` figure.
- Eligibility is **re-asked per directory** inside the deletion loop rather than trusted from the plan snapshot (`remotecache.go`).

---

## PART 4 — Schema guard (`internal/store/sync_schema_guard.go`)

### 4.1 `RemoteSchemaAheadError`

Fields (`sync_schema_guard.go`): `Remote`, `Branch`, `RemoteVersion int64`, `BinarySupportedMax int64`.

`Error()` (`sync_schema_guard.go`) renders:
`"remote %s/%s is at schema version %d but this binary supports only up to %d; refusing to write a commit below the remote head's schema — run `lit upgrade` to install a lit that supports schema version %d"` (Remote, Branch, RemoteVersion, BinarySupportedMax, RemoteVersion).

### 4.2 Guard paths

`guardRemoteSchemaAhead(ctx, remote, branch)` (`sync_schema_guard.go`) — the **push** entry:
1. Both args required.
2. `trackingHeadHash(remote, branch)`; `!synced` → **no-op, return nil** (a branch that never synced has no remote head to fall behind) (`sync_schema_guard.go`).
3. else `guardCommitSchemaAhead(remote, branch, head)`.

`guardCommitSchemaAhead(ctx, remote, branch, commitHash)` (`sync_schema_guard.go`) — shared core, also called directly by the reconcile with its already-captured `remoteHead`:
1. `migrations.MaxVersion()` → `registryMax`.
2. `remoteHeadSchema(commitHash)` → `remoteVersion`.
3. `remoteVersion <= registryMax` → nil.
4. else `&RemoteSchemaAheadError{...}`.

`trackingHeadHash` (`sync_schema_guard.go`): `SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?` on `remotes/<remote>/<branch>`; count 0 → `("", false, nil)`; else `commitHashOfRef`.

`remoteHeadSchema(commitHash)` (`sync_schema_guard.go`):
1. **Refuses** unless `isDoltCommitHash(commitHash)` — error `"remote head schema: %q is not a Dolt commit hash"` (`sync_schema_guard.go`). Necessary because `AS OF` cannot take a bound parameter and the hash is interpolated.
2. `schemaVersionAtCommit`.

`schemaVersionAtCommit` (`sync_schema_guard.go`): `SELECT MAX(version_id) FROM goose_db_version AS OF '<hash>'`.
- MySQL 1146 (missing table) → returns `0` (pre-goose remote, never ahead).
- NULL max (empty goose table) → `0`.
- any other error → `"read schema version at %q: %w"`.

`isMissingTableError(err)` (`sync_schema_guard.go`): `errors.As` to `*embedded.MySQLError` with `Number == 1146` — matched on the typed error, not message text.

`isDoltCommitHash(s)` (`sync_schema_guard.go`): exactly **32** characters, each in `0-9` or `a-v` (Dolt's base32 alphabet). Test `TestIsDoltCommitHash` (`sync_schema_guard_test.go`).

### 4.3 Who is and is not guarded

- `SyncPush`/`SyncCompactAndPush` with a non-empty branch: guarded (`sync.go`). Test `TestSyncPushRefusesWhenRemoteSchemaAhead` (`sync_schema_guard_test.go`).
- Push with an empty branch: **not** guarded (`sync.go`).
- Every reconcile that replays (three-way, combine, take-local): guarded inside `replayUnderGuard` before any write (`sync_reconcile.go`). Tests `TestSyncReconcileRefusesWhenRemoteSchemaAhead` (`sync_schema_guard_test.go`), `TestSyncResolveUnrelatedTakeLocalRefusesSchemaAheadRemote` (`sync_unrelated_test.go`).
- **take-remote is exempt** — it authors no replay commit and adopting an ahead head is a safe recovery (`sync_unrelated_take.go`).
- Other tests: `TestRemoteHeadSchemaReadsVersionAndProducer` (`sync_schema_guard_test.go`), `TestGuardRemoteSchemaAheadDetects` (`sync_schema_guard_test.go`), `TestGuardRemoteSchemaNotAheadAtOrBelowMax` (`sync_schema_guard_test.go`).

---

## PART 5 — Reconcile (`internal/store/sync_reconcile.go`)

### 5.1 Commit message constants

- `reconcileCommitMessage = "reconcile: field-aware merge of remote divergence"` (`sync_reconcile.go`)
- `combineCommitMessage = "reconcile: combine unrelated histories (union of both backlogs)"` (`sync_reconcile.go`)
- `reconcileLiftCommitMessage = "reconcile: lift remote head to current schema"` (`sync_reconcile.go`)
- `takeLocalCommitMessage = "reconcile: take local backlog over unrelated remote history"` (`/Users/bmf/code/links-issue-tracker/internal/store/sync_unrelated_take.go`)

### 5.2 Unrelated-history handling value

`type unrelatedHandling int` (`sync_reconcile.go`) with:
- `detectOnly` (=0, `sync_reconcile.go`): classify as `SyncReconcileUnrelated`, commit nothing.
- `unionCombine` (=1, `sync_reconcile.go`): union both sides via a two-way merge over an empty base.

### 5.3 Scratch branches

- Prefix `reconcileScratchPrefix = "links-reconcile-scratch"` (`sync_reconcile.go`).
- `reconcileScratchName()` = `fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())` (`sync_reconcile.go`).
- `reconcileScratch{spine, read string}` (`sync_reconcile.go`); `newReconcileScratch()` derives `base+"-spine"` and `base+"-read"` from one unique base (`sync_reconcile.go`).
- Role split (`sync_reconcile.go`): `read` is hard-reset once per folded commit (nothing on it is kept); `spine` is hard-reset exactly once (to adopt the remote head) and thereafter only advances by commit.

### 5.4 Snapshot budget

- `reconcileSnapshotLabel = "pre-reconcile"` (`sync_reconcile.go`).
- `reconcileSnapshotRetention = 10` (`sync_reconcile.go`).
- `IsReconcileSnapshotName(name)` (`sync_reconcile.go`) = `isStampedSnapshotName(name, "pre-reconcile")`.
- `formatReconcileSnapshotLabel(t)` = `fmt.Sprintf("%s-%d", "pre-reconcile", t.UTC().UnixNano())` (`sync_reconcile.go`).
- Test `TestIsReconcileSnapshotNameDisjoint` (`sync_reconcile_schema_skew_test.go`).

### 5.5 Settle policies (`settleFn`)

`type settleFn func(merge.MergeResult) (model.Export, []merge.ProsePending)` (`sync_reconcile.go`).

- `autonomousSettle` (`sync_reconcile.go`): if `merged.Settled()` returns `ok` → that export, no pending. Otherwise `(model.Export{}, merged.Pending)`. **Prose is never auto-committed by picking a side.**
- `resolvedSettle(resolutions)` (`sync_reconcile.go`): first honor `merged.Settled()` (the divergence converged on its own between the agent reading and finalizing). Then `merge.ApplyProseResolutions(merged, resolutions)` — accepted **only** when the resolutions are an exact bijection with the live pending set. A stale/partial set falls through to `(model.Export{}, merged.Pending)`, re-surfacing the CURRENT pending. Test `TestSyncReconcileResolvedRejectsStaleResolutions` (`sync_reconcile_test.go`).

### 5.6 Public reconcile entry points

- `SyncReconcile(ctx, remote, branch)` (`sync_reconcile.go`) = `reconcile(..., autonomousSettle, detectOnly)`.
- `SyncReconcileResolved(ctx, remote, branch, resolutions)` (`sync_reconcile.go`) = `reconcile(..., resolvedSettle(resolutions), unionCombine)` — one finalize path serving both shared-history three-way and unrelated combine (`sync_reconcile.go`).
- `SyncReconcileCombine(ctx, remote, branch)` (`sync_reconcile.go`) = `reconcile(..., autonomousSettle, unionCombine)`. On a divergence **with** a common base it merges through that base like an ordinary reconcile (`sync_reconcile.go`).

### 5.7 `reconcilePlan` and its capture

`reconcilePlan{diverged bool; ahead, behind int64; dataBranch, localHead, remoteHead string; base mergeBaseResult}` (`sync_reconcile.go`).

`captureReconcilePlan(ctx, remote, branch)` (`sync_reconcile.go`), run under the already-held commit lock:
1. `SyncFreshness` → `ahead`, `behind`.
2. `fresh.State() != SyncDiverged` → return with `diverged=false` and **no anchors** (`sync_reconcile.go`).
3. `diverged = true`; `trackingRef = "remotes/<remote>/<branch>"`.
4. `activeBranch(ctx, s.db)` → `dataBranch` (`SELECT active_branch()`, `sync_reconcile.go`).
5. `readDoltHead(ctx, s.db)` → `localHead`; error `"read local head: %w"`.
6. `commitHashOfRef(ctx, s.db, trackingRef)` → `remoteHead` (`SELECT commit_hash FROM dolt_log(?) LIMIT 1`, `sync_reconcile.go`; error `"read head of %q: %w"`).
7. `mergeBase(ctx, s.db, localHead, trackingRef)` → `base`.

### 5.8 Merge-base and the unrelated-history discriminator

`mergeBaseResult{commit string; hasBase bool}` (`sync_reconcile.go`); `shared() (commit, ok)` (`sync_reconcile.go`).

`noCommonAncestorMsg = "no common ancestor"` (`sync_reconcile.go`) — the message `doltdb.ErrNoCommonAncestor` surfaces through the engine as a generic Error 1105.

`isNoCommonAncestor(err)` (`sync_reconcile.go`): true when `errors.Is(err, sql.ErrNoRows)` **or** `strings.Contains(err.Error(), "no common ancestor")`. Deliberately **not** matched on code 1105 (MySQL's catch-all `ER_UNKNOWN_ERROR`, which would over-match).

`mergeBase(ctx, db, ref1, ref2)` (`sync_reconcile.go`):
1. `SELECT DOLT_MERGE_BASE(?, ?)` scanned as `sql.NullString`.
2. `isNoCommonAncestor(err)` → `(mergeBaseResult{}, nil)` — i.e. `hasBase=false`.
3. any other error → `"merge-base of %q and %q: %w"`.
4. Valid, non-blank scalar → `{commit: trimmed, hasBase: true}`.
5. NULL or blank scalar → `hasBase=false` (belt-and-suspenders across backend versions, `sync_reconcile.go`).

### 5.9 The reconcile algorithm (`reconcile`)

`reconcile(ctx, remote, branch, settle, unrelated)` (`sync_reconcile.go`):
1. `requireSyncArg` on remote and branch.
2. Whole body inside `s.withCommitLock` (`sync_reconcile.go`).
3. `captureReconcilePlan`. Copy `Ahead`/`Behind` onto the result.
4. `!plan.diverged` → `result.State = SyncReconcileNotDiverged`, return (`sync_reconcile.go`).
5. `baseCommit, shared := plan.base.shared()`; set `result.LocalHead`, `result.RemoteHead`, `result.BaseCommit` (`sync_reconcile.go`).
6. **If `!shared` (unrelated histories)** (`sync_reconcile.go`):
   a. `s.unrelatedInventory(localHead, remoteHead)` — pure `AS OF` reads under the lock; assigned to `result.Unrelated`.
   b. `unrelated == detectOnly` → `result.State = SyncReconcileUnrelated`, **return before** the schema guard, the scratch sweep, the snapshot, and every reset. Both stores untouched (`sync_reconcile.go`).
   c. `unionCombine` → `replayUnderGuard(..., combineFromAnchors)`.
7. **Else (shared base)** → `replayUnderGuard(..., reconcileFromAnchors)` (`sync_reconcile.go`).
8. Any error → the zero `SyncReconcileResult` is returned (`sync_reconcile.go`).

### 5.10 `replayUnderGuard` — the mutation envelope

`replayUnderGuard(ctx, remote, branch, remoteHead, body)` (`sync_reconcile.go`), in order:
1. `s.guardCommitSchemaAhead(ctx, remote, branch, remoteHead)` — **before any write**, using the captured `remoteHead` so a concurrent fetch cannot shift the decision.
2. `s.sweepStaleReconcileScratch(ctx)`.
3. `scratchBranch := newReconcileScratch()`.
4. `guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatReconcileSnapshotLabel(time.Now()))` — **one** guard carried across all retries, so exactly one snapshot is taken however many attempts run.
5. `retryTransientGCContention(body, s.reconnect)`.

`sweepStaleReconcileScratch(ctx)` (`sync_reconcile.go`): `SELECT name FROM dolt_branches WHERE name LIKE ?` with `"links-reconcile-scratch-%"`; every match is deleted with `CALL DOLT_BRANCH('-D', name)`. Every failure (list, scan, iterate, delete) prints to stderr with the messages at `sync_reconcile.go` and **never fails the reconcile**. The commit lock guarantees every such branch is an orphan (`sync_reconcile.go`).

### 5.11 Scratch lifecycle (`runOnReconcileScratch`)

`runOnReconcileScratch(ctx, dataBranch, scratch, localHead, body)` (`sync_reconcile.go`):
1. `var created []string`; the cleanup defer is armed **before** the first branch creation, so a failure creating the second branch does not strand the first (`sync_reconcile.go`).
2. For each of `[scratch.spine, scratch.read]` in that order: `CALL DOLT_CHECKOUT('-B', branch, localHead)`. `-B` recreates if a prior retry left it behind. Failure → `"create reconcile scratch branch %q: %w"`. Each success appends to `created`.
3. `body()`.
4. Deferred `cleanupReconcileScratch(ctx, dataBranch, created)`; its error is promoted to the return value **only if `err == nil`** (`sync_reconcile.go`).

`cleanupReconcileScratch(ctx, dataBranch, createdBranches)` (`sync_reconcile.go`):
1. `CALL DOLT_CHECKOUT(dataBranch)`. On success → delete each created branch with `CALL DOLT_BRANCH('-D', name)`; a delete failure prints `"lit: reconcile cleanup could not delete scratch branch %q: %v"` to stderr and is **not** promoted (`sync_reconcile.go`).
2. On checkout failure → print `"lit: reconcile could not return to data branch %q (%v); rotating connection to recover"`, then `s.reconnect(ctx)`. If the rotation fails → return the unrecoverable error `"reconcile left the store on the scratch branch and could not recover: checkout %q failed (%v); connection rotation failed: %w"` (`sync_reconcile.go`).
3. After a successful rotation, checkout the data branch **explicitly** again; failure → `"reconcile could not restore the data branch %q after rotating the connection: %w"` (`sync_reconcile.go`). The leftover scratch branch is left behind deliberately.

### 5.12 Reading exports at fixed anchors

`resetAndLift(ctx, commit)` (`sync_reconcile.go`):
1. `CALL DOLT_RESET('--hard', commit)`; error `"reset scratch to %q: %w"`.
2. `s.liftWorkingSetToRegistry(ctx)`; error `"lift %q to current schema: %w"`.
Only ever run on a scratch branch.

`exportAtCommit(ctx, readBranch, commit)` (`sync_reconcile.go`):
1. `CALL DOLT_CHECKOUT(readBranch)` — the branch is an **argument**, never inherited from session state; error `"switch to reconcile read branch %q: %w"`.
2. `resetAndLift(commit)`; error `"read export at %q: %w"`.
3. `s.Export(ctx)`.

### 5.13 `mergeAndReplay` — the shared merge/settle/replay tail

`mergeAndReplay(ctx, result, settle, guard, dataBranch, scratch, localHead, remoteHead, base, message, settledState)` (`sync_reconcile.go`):
1. `ours = exportAtCommit(scratch.read, localHead)`.
2. `theirs = exportAtCommit(scratch.read, remoteHead)`.
3. `merged = merge.ThreeWay(base, ours, theirs)`.
4. `export, pending = settle(merged)`.
5. **`len(pending) > 0`** → `result.State = SyncReconcileProsePending`, `result.Pending = pending`, **return with nothing committed**. The data branch is still at `localHead` (`sync_reconcile.go`).
6. `chain = readFoldedChain(ctx, s.db, remoteHead, localHead)`.
7. `stepper = foldStepper{store, readBranch: scratch.read, chain, base, theirs}`.
8. `replayed = commitReplayAndAdvance(guard, dataBranch, scratch, remoteHead, message, export, stepper)`.
9. `result.State = settledState`; `result.Pending = nil`; `result.Replayed = replayed`.

`reconcileFromAnchors` (`sync_reconcile.go`) = `runOnReconcileScratch` + `base := exportAtCommit(scratch.read, baseCommit)` + `mergeAndReplay(..., base, reconcileCommitMessage, SyncReconcileLinearized)`.

`combineFromAnchors` (`sync_reconcile.go`) = `runOnReconcileScratch` + `mergeAndReplay(..., model.Export{}, combineCommitMessage, SyncReconcileCombined)` — the **empty export is the base**, which `merge.ThreeWay` reads as "both sides changed every field from empty", i.e. a two-way union (`sync_reconcile.go`).

### 5.14 Folded-chain provenance

`foldedCommit{hash, message, author string; date time.Time}` (`sync_reconcile.go`).

`foldedChainRange(remoteHead, localHead)` = `remoteHead + ".." + localHead` (`sync_reconcile.go`). One spelling: on unrelated histories it is the entire local chain; on a shared base it is exactly the ahead commits.

`readFoldedChain(ctx, db, remoteHead, localHead)` (`sync_reconcile.go`):
1. `SELECT commit_hash, committer, email, date, message FROM dolt_log(?)` with the bound range.
2. `author = fmt.Sprintf("%s <%s>", committer, email)` (`sync_reconcile.go`).
3. **Abort condition**: if the chain is non-empty and `chain[0].hash != localHead` → error `"folded chain starts at %q, want local head %q"` (`sync_reconcile.go`).
4. Reverse in place (dolt_log is newest-first; replay order is oldest-first) (`sync_reconcile.go`).
Errors: `"read folded chain %s..%s: %w"`, `"scan folded chain commit: %w"`, `"iterate folded chain: %w"`.

`replayStep{export model.Export; stamp commitStamp}` (`sync_reconcile.go`). Two authorities are explicitly separated (`sync_reconcile.go`): the Go-side diff decides which **rows** to write; **Dolt** decides whether a commit exists (empty diff → no commit).

`foldStepper{store, readBranch, chain, base, theirs}` (`sync_reconcile.go`):
- `len()` = `len(chain)` (`sync_reconcile.go`).
- `step(ctx, i)` (`sync_reconcile.go`): `at := exportAtCommit(readBranch, chain[i].hash)`; returns `replayStep{export: merge.ThreeWay(base, at, theirs).Provisional(), stamp: commitStamp{Message: c.message, Date: c.date, Author: c.author}}`. Every step reads its own commit including the newest — no special case for the last (`sync_reconcile.go`).

### 5.15 The spine writer

`spineWriter{store, branch, landed model.Export}` (`sync_reconcile.go`). `landed` is **not** a parameter; the writer owns it.

`newSpineWriter(ctx, branch)` (`sync_reconcile.go`): `CALL DOLT_CHECKOUT(branch)` (error `"switch to reconcile spine branch %q: %w"`), then `s.Export(ctx)` to seed `landed` from a real read (error `"read reconcile spine base state: %w"`).

`land(ctx, next, stamp)` (`sync_reconcile.go`): unconditional `CALL DOLT_CHECKOUT(w.branch)` first, then `replayDeltaOnScratch(diffExports(w.landed, next), stamp)`; `w.landed = next` **only on success**.

`replayDeltaOnScratch(ctx, delta, stamp)` (`sync_reconcile.go`): inside `withCommitLock` (re-entrant): `BeginTx` → `defer tx.Rollback()` → `applyExportDelta(ctx, tx, delta)` → `tx.Commit()` → `commitWorkingSetOnce(ctx, stamp)`. **A single attempt, deliberately without the self-rotating transient retry** — a rotation would open a fresh connection on the DEFAULT branch and resume committing onto the data branch (`sync_reconcile.go`). A transient failure bubbles to `replayUnderGuard`'s outer retry, whose next attempt re-creates both scratch branches as its first op and rebuilds the whole spine from the fixed anchors. Errors: `"begin %s tx: %w"`, `"commit %s tx: %w"`. Test evidence: `TestSyncReconcileCombineRecoversFromTransientFailureMidReplay` (`sync_unrelated_test.go`).

### 5.16 `commitReplayAndAdvance` — the safe replay, step by step

`commitReplayAndAdvance(ctx, guard, dataBranch, scratch, remoteHead, message, export, stepper) (int, error)` (`sync_reconcile.go`):

1. `CALL DOLT_CHECKOUT(scratch.spine)`; error `"switch to reconcile spine branch %q: %w"`.
2. `s.resetAndLift(ctx, remoteHead)`; error `"adopt remote head %q on scratch: %w"`. This is the spine's **one and only** reset.
3. `s.commitWorkingSetOnce(ctx, commitStamp{Message: reconcileLiftCommitMessage})` — the schema-lift DDL and bookkeeping land as their own named commit, or **no commit at all** on a current-schema head (empty diff). Error `"commit schema lift of %q: %w"` (`sync_reconcile.go`).
4. `liftedBase = readDoltHead(ctx, s.db)`; error `"read lifted base: %w"`.
5. `writer = newSpineWriter(ctx, scratch.spine)` — constructed **after** the lift commit.
6. Loop `i = 0 .. stepper.len()-1`: `step = stepper.step(ctx, i)`; `writer.land(step.export, step.stamp)`; landing error wrapped `"replay folded commit %q: %w"` naming the step's message (`sync_reconcile.go`). One step is read and landed before the next is read — bounded memory.
7. `writer.land(ctx, export, commitStamp{Message: message, AllowEmpty: true})` — the marker commit is **unconditional** (`sync_reconcile.go`).
8. `replayedCommit = readDoltHead(ctx, s.db)`; error `"read replayed commit: %w"`.
9. `landed = countCommitsInRange(ctx, s.db, liftedBase, replayedCommit)`; `replayed = landed - 1` (subtracting the marker) (`sync_reconcile.go`). `countCommitsInRange` (`sync_reconcile.go`) = `SELECT COUNT(*) FROM dolt_log(?)` over `foldedChainRange(exclusiveBase, head)`; error `"count replayed commits %s..%s: %w"`.
10. **Snapshot-first**: `guard.ensure(ctx)`; failure → `"snapshot before reconcile: %w"`, aborting **before** the data branch moves (`sync_reconcile.go`).
11. `CALL DOLT_CHECKOUT(dataBranch)`; error `"return to data branch %q: %w"`.
12. `CALL DOLT_RESET('--hard', replayedCommit)` — **the single atomic advance of the data branch**; error `"advance %q to replayed commit: %w"` (`sync_reconcile.go`).
13. `dbsnapshot.PruneMatching(migrationSnapshotsDir(s.doltRootDir), 10, IsReconcileSnapshotName)`. A failure prints `"lit: reconcile could not prune old recovery snapshots (replay already committed): %v"` to stderr and is **not** promoted — the replay already committed (`sync_reconcile.go`).
14. Return `replayed`.

Invariant: the data branch is at its pre-reconcile head before step 12 and at the complete replayed spine after — never in between.

### 5.17 Reconcile result vocabulary

`SyncReconcileResult` at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`: `State`, `Ahead`, `Behind`, `LocalHead`, `RemoteHead`, `BaseCommit`, `Pending []merge.ProsePending`, `Unrelated *UnrelatedInventory`, `Replayed int`.

States and their string values (`storage/sync.go`):
- `SyncReconcileNotDiverged = "not_diverged"`
- `SyncReconcileLinearized = "linearized"`
- `SyncReconcileProsePending = "prose_pending"`
- `SyncReconcileUnrelated = "unrelated_histories"`
- `SyncReconcileTookLocal = "took_local"`
- `SyncReconcileTookRemote = "took_remote"`
- `SyncReconcileCombined = "combined"`

`Replayed` counts folded commits that **actually landed** (read back off the spine), zero for non-mutating outcomes and for a fold whose every projection was already contained in the spine (`storage/sync.go`).

Tests: `TestSyncReconcileLinearizesDivergenceAndFastForwardPushes` (`sync_reconcile_test.go`), `TestSyncReconcileHoldsProseDivergenceForAgent` (`sync_reconcile_test.go`), `TestSyncReconcileResolvedFinalizesWithAgentText` (`sync_reconcile_test.go`), `TestSyncReconcileHealsSchemaSkew` (`sync_reconcile_schema_skew_test.go`), `TestSyncReconcileCombineIsBoundedOnALargeFoldedChain` (`sync_reconcile_scale_test.go`), `TestSyncReconcileCombinePreservesFoldedProvenance` (`sync_unrelated_test.go`).

---

## PART 6 — Unrelated histories: inventory and the "take" flow

### 6.1 Inventory (`internal/store/sync_unrelated.go`)

`unrelatedInventory(ctx, localHead, remoteHead) (*storage.UnrelatedInventory, error)` (`sync_unrelated.go`):
1. `issueIDsAtCommit(localHead)`; error `"read local issue inventory: %w"`.
2. `issueIDsAtCommit(remoteHead)`; error `"read remote issue inventory: %w"`.
3. Returns `{OnlyLocal: setDifference(local, remote), OnlyRemote: setDifference(remote, local), OnBoth: setIntersection(local, remote)}` (`sync_unrelated.go`).

`issueIDsAtCommit(ctx, db, commitHash)` (`sync_unrelated.go`):
- **Refuses** unless `isDoltCommitHash(commitHash)`: `"issue inventory: %q is not a Dolt commit hash"` (`sync_unrelated.go`).
- `SELECT id FROM issues AS OF '<hash>'` (interpolated literal).
- Missing table (MySQL 1146) → the **empty set**, not an error — a pristine bootstrap root genuinely holds no issues (`sync_unrelated.go`).
- Errors: `"read issue ids at %q: %w"`, `"scan issue id at %q: %w"`, `"iterate issue ids at %q: %w"`.

Pure `AS OF` reads: no branch moves, no schema lift — preserving the detection's no-write guarantee (`sync_unrelated.go`).

`UnrelatedInventory{OnlyLocal, OnlyRemote, OnBoth []string}` with JSON tags `only_local`, `only_remote`, `on_both`, all `omitempty` (`/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`). The three slices are sorted and mutually disjoint by construction (`storage/sync.go`). Test `TestSyncReconcileUnrelatedInventoryPartitionsAllThreeSides` (`sync_unrelated_test.go`).

### 6.2 The owner-approval token

`takeApprovalToken(localHead, remoteHead, choice)` (`sync_unrelated_take.go`):
`hex.EncodeToString(sha256.Sum256([]byte("take:" + string(choice) + "\x00" + localHead + "\x00" + remoteHead))[:6])` — a **12-hex-character** digest (first 6 bytes). Deliberately unexported: the only way to obtain a token is to run the take and read its refusal (`sync_unrelated_take.go`). Any commit on either side, or approving one side and running the other, changes the token.

`OwnerApprovalRequiredError` (`sync_unrelated_take.go`): `Choice`, `ApprovalToken` (the token that WOULD authorize right now), `Stale bool`, `LocalHead`, `RemoteHead`, `Ahead`, `Behind`, `Inventory *storage.UnrelatedInventory`.
`Error()` (`sync_unrelated_take.go`):
- `Stale` → `"sync reconcile take %s: the supplied owner approval no longer matches this divergence (the backlog moved, or it was issued for the other side)"`
- else → `"sync reconcile take %s is destructive and requires explicit owner approval"`.

Test `TestSyncResolveUnrelatedOwnerApprovalBindsForkAndSide` (`sync_unrelated_test.go`).

### 6.3 `SyncResolveUnrelated` — the take gate

`SyncResolveUnrelated(ctx, remote, branch, choice storage.UnrelatedResolution, ownerApproval string)` (`sync_unrelated_take.go`):
1. `requireSyncArg` on remote and branch.
2. `!choice.Valid()` → `"resolve unrelated histories: unknown side %q (want %q or %q)"` naming `storage.TakeLocal`/`storage.TakeRemote` (`sync_unrelated_take.go`). `UnrelatedResolution` values: `TakeLocal = "local"`, `TakeRemote = "remote"` (`/Users/bmf/code/links-issue-tracker/internal/storage/sync.go`); `Valid()` at `storage/sync.go`. Test `TestUnrelatedResolutionValid` (`sync_unrelated_test.go`).
3. Everything below inside `s.withCommitLock` (`sync_unrelated_take.go`).
4. `captureReconcilePlan`; copy `Ahead`/`Behind`.
5. `!plan.diverged` → `SyncReconcileNotDiverged`, return (`sync_unrelated_take.go`).
6. Set `result.LocalHead`, `result.RemoteHead`.
7. **If the divergence HAS a base** → refuse: `"sync reconcile take applies only to unrelated histories, but %s/%s shares history with the local backlog; run \`lit sync reconcile\` to field-merge it"` (`sync_unrelated_take.go`). Test `TestSyncResolveUnrelatedRefusesSharedHistory` (`sync_unrelated_test.go`).
8. `unrelatedInventory(localHead, remoteHead)` → `result.Unrelated` (read **before** the approval check so the refusal can name what would be destroyed).
9. `expected := takeApprovalToken(plan.localHead, plan.remoteHead, choice)`; if `ownerApproval != expected` → return `OwnerApprovalRequiredError{..., Stale: ownerApproval != ""}` with **nothing mutated** (`sync_unrelated_take.go`).
10. `s.applyUnrelatedTake(...)`.

### 6.4 `applyUnrelatedTake` dispatch

`applyUnrelatedTake` (`sync_unrelated_take.go`):
- `storage.TakeRemote` → its **own** snapshot guard (`newSnapshotGuard(..., formatReconcileSnapshotLabel(time.Now()))`), tracking ref `remotes/<remote>/<branch>`, then `retryTransientGCContention(takeRemoteHead)`. **No scratch envelope, no schema-ahead refusal** (`sync_unrelated_take.go`).
- `storage.TakeLocal` → `s.replayUnderGuard(ctx, remote, branch, plan.remoteHead, takeLocalOntoRemoteHead)` — the **full** envelope including the schema-ahead refusal (`sync_unrelated_take.go`).
- default → `"resolve unrelated histories: unhandled side %q"` (`sync_unrelated_take.go`). The combine resolution never reaches this dispatch; it lives on the reconcile boundary.

### 6.5 `takeRemoteHead`

`takeRemoteHead(ctx, result, guard, trackingRef)` (`sync_unrelated_take.go`):
1. `guard.ensure(ctx)`; failure → `"snapshot before take-remote: %w"`.
2. `resetHardToRef(ctx, s.db, trackingRef)`.
3. `dbsnapshot.PruneMatching(migrationSnapshotsDir(...), 10, IsReconcileSnapshotName)`; failure prints `"lit: take-remote could not prune old recovery snapshots (reset already done): %v"` to stderr, **not** promoted.
4. `result.State = SyncReconcileTookRemote`.
Idempotent under retry (cached snapshot, fixed ref). Test `TestSyncResolveUnrelatedTakeRemote` (`sync_unrelated_test.go`).

### 6.6 `takeLocalOntoRemoteHead`

`takeLocalOntoRemoteHead(ctx, result, guard, dataBranch, scratch, localHead, remoteHead)` (`sync_unrelated_take.go`), inside `runOnReconcileScratch`:
1. `local = exportAtCommit(scratch.read, localHead)`.
2. `theirs = exportAtCommit(scratch.read, remoteHead)`.
3. `chain = readFoldedChain(remoteHead, localHead)`.
4. `stepper = foldStepper{base: model.Export{}, theirs: theirs...}` — the **same no-base union projection combine uses**, so mid-chain history stays a whole backlog (`sync_unrelated_take.go`).
5. `commitReplayAndAdvance(guard, dataBranch, scratch, remoteHead, takeLocalCommitMessage, local, stepper)` — the terminal export is **local's content, not the union**, so the marker commit's diff is exactly the discard of the remote-only issues (`sync_unrelated_take.go`).
6. `result.State = SyncReconcileTookLocal`; `result.Replayed = replayed`.

Tests: `TestSyncResolveUnrelatedTakeLocal` (`sync_unrelated_test.go`), `TestSyncResolveUnrelatedTakeLocalPreservesFoldedProvenance` (`sync_unrelated_test.go`).

### 6.7 Combine

Reached only via `SyncReconcileCombine` or `SyncReconcileResolved`. It is **non-destructive**, so unlike the takes it does not refuse shared history (`sync_reconcile.go`). Tests: `TestSyncReconcileCombineUnionsBothSides` (`sync_unrelated_test.go`), `TestSyncReconcileCombineHoldsAndFinalizesProse` (`sync_unrelated_test.go`).

Detection-only: `TestSyncReconcileDetectsUnrelatedHistories` (`sync_unrelated_test.go`).

---

## PART 7 — Export delta (what the replay actually writes) (`internal/store/export_delta.go`)

### 7.1 Delta types

`tableDelta[K comparable, R any]{remove []K; add []R}` (`export_delta.go`); `empty()` at `export_delta.go`.
`exportDelta{issues, relations, comments, labels, events}` (`export_delta.go`), ordered so issues precede everything referencing them. `exportDelta.empty()` at `export_delta.go`.

### 7.2 `diffTable` — the per-table comparison rule

`diffTable(live, wanted []R, key func(R) K, persisted func(R) any)` (`export_delta.go`):
1. Index `live` and `wanted` by key.
2. For each row in `live` **in slice order**: queue its key for removal when the key is absent from `wanted` **or** `!reflect.DeepEqual(persisted(row), persisted(want))`.
3. For each row in `wanted` **in slice order**: queue the row for addition when the key is absent from `live` **or** the persisted projections differ.
A row whose value changed under a stable key appears in **both** lists — removed by key, then re-added — so no UPDATE statement exists (`export_delta.go`).
Determinism: the input slices are walked, not the maps, so identical inputs produce an identical statement sequence (`export_delta.go`).
`reflect.DeepEqual` makes the comparison total; its failure direction is a redundant rewrite, never a missed one (`export_delta.go`).

### 7.3 Projections

- `wholeRow[R](row R) any { return row }` (`export_delta.go`) — used for relations, comments, labels, and events. An event's `Changes` are part of its value, so an event whose changes differ is a changed row (`export_delta.go`).
- `issues` uses `issueRowValues(i)` because `model.Issue` is a hydrated view that also carries labels from another table and, for a container, a lifecycle derived from its children; comparing those made every label edit and every child status change look like an issue-row change (`export_delta.go`).

### 7.4 Keys

- issues: `i.ID` (string) (`export_delta.go`)
- relations: `relationKey{srcID, dstID, kind: r.Type}` (`export_delta.go`)
- comments: `c.ID` (`export_delta.go`)
- labels: `labelKey{issueID, name}` (`export_delta.go`)
- events: `e.ID` (`export_delta.go`)
Primary-key types live in `/Users/bmf/code/links-issue-tracker/internal/store/row_deletes.go` (`export_delta.go`).

### 7.5 Cascade rule

`diffExports(prev, next)` (`export_delta.go`):
1. The **issues** diff is computed first.
2. `survivors = cascadeSurvivors(prev.Issues, issues.remove)` (`export_delta.go`, definition `export_delta.go`) — the complement of the removal list, read **off the issues diff** rather than recomputed.
3. Each child table's `live` input is `prev`'s rows **filtered to post-cascade survivors**:
   - relations: kept when `survivors[r.SrcID] && survivors[r.DstID]` — a relation cascades away if **either** endpoint does (`export_delta.go`)
   - comments: `survivors[c.IssueID]` (`export_delta.go`)
   - labels: `survivors[l.IssueID]` (`export_delta.go`)
   - events: `survivors[e.IssueID]` (`export_delta.go`)
The schema fact this rests on: every child table is `ON DELETE CASCADE` from issues (and `issue_event_changes` from `issue_events`) (`export_delta.go`).

### 7.6 Application order

`applyExportDelta(ctx, tx, delta)` (`export_delta.go`) applies, in this exact order: issues, relations, comments, labels, events (`export_delta.go`), each via `applyTableDelta` (`export_delta.go`) which performs **all removals before all additions** for that table (`export_delta.go`). Row counts from deletions are deliberately ignored (`export_delta.go`).

---

## PART 8 — Migration runner (`internal/store/migration_runner.go`)

### 8.1 Constants and keys

- `producerBinaryVersionMetaKey = "producer_binary_version"` (`migration_runner.go`)
- `migrationCheckpointPrefix = "pre-migrate"` (`migration_runner.go`)
- `migrationCheckpointRetention = 5` (`migration_runner.go`)
- `migrationDriftRepairCheckpointPrefix = "pre-drift-repair"` (`migration_runner.go`)
- `gooseVersionTable = "goose_db_version"` (`migration_runner.go`)
- `baselineVersion = migrations.Baseline` (`migration_runner.go`)
- `migrationSnapshotRetention = 10` (`/Users/bmf/code/links-issue-tracker/internal/store/migrate_snapshot.go`)
- `migrationSnapshotLabel = "pre-migrate"` (`migrate_snapshot.go`)

Test hooks: `migrationUpByOneForTest` (`migration_runner.go`) applies **only** to `applyPendingMigrations`, never the reconcile lift; `migrationPostSnapshotHookForTest` (`migrate_snapshot.go`) fires inside `runMigration` after the snapshot guard.

### 8.2 Phases

`migrationPhase` (`migration_runner.go`) with `phaseFresh`(0), `phaseAdopt`(1), `phaseManaged`(2) (`migration_runner.go`).
`migrationState{phase; appliedVersion int64; registryMaxVers int64}` (`migration_runner.go`).
`willMutate()` (`migration_runner.go`): `phaseManaged` → `appliedVersion < registryMaxVers`; every other phase → **true**.

`classifyMigrationState(ctx)` (`migration_runner.go`) — reads only, in this order:
1. `migrations.MaxVersion()` → `registryMax`.
2. `tableExists("issue_history")` — the **legacy marker**. Present → `phaseAdopt` immediately, **regardless of what goose_db_version claims** (`migration_runner.go`). Rationale: `issue_history` is a pre-goose-only table the baseline never creates and `reconcileToBaseline` drops; its presence is conclusive evidence of a pre-goose canonical shape, and this re-routes "buggy older binary fabricated goose rows on a pre-goose workspace" back into `phaseAdopt` (`migration_runner.go`).
3. `tableExists("goose_db_version")` → present → `phaseManaged` with `appliedVersion = recordedMigrationVersion(ctx)`.
4. `verifyBaselineShape(ctx)` → `present == 0` → `phaseFresh`; otherwise `phaseAdopt`.

`recordedMigrationVersion` (`migration_runner.go`): goose `database.NewStore(goose.DialectMySQL, "goose_db_version").GetLatestVersion`; `database.ErrVersionNotFound` → `0, nil`.

`AppliedSchemaVersion(ctx)` (`migration_runner.go`) is a pure read over `classifyMigrationState`.

### 8.3 `migrate` — the outer boundary

`(*Store).migrate(ctx)` (`migration_runner.go`):
1. `guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatMigrationSnapshotLabel(time.Now()))`.
2. `s.runMigration(ctx, guard)`.
3. On error: if `guard.took()` → wrap as `&MigrationRollbackError{Snapshot: snap, Cause: err}`; otherwise return the raw error (`migration_runner.go`).
4. On success: if a snapshot was taken → `dbsnapshot.PruneMatching(guard.snapshotsDir, 10, IsMigrationSnapshotName)`; a prune failure **is** promoted here: `"prune migration snapshots: %w"` (`migration_runner.go`).

### 8.4 `runMigration` — the full sequence

`runMigration(ctx, guard)` (`migration_runner.go`), in exact order:

1. `state = classifyMigrationState(ctx)`.
2. **Ahead-of-registry check**: `state.appliedVersion > state.registryMaxVers` → `return s.refuseIfBaselineMissing(ctx, state)`. **No bookkeeping mutation ever happens on this path** — trimming the goose log would destroy true information (`migration_runner.go`).
3. **Version-content drift check** — `state.phase == phaseManaged` only, and run **independently of `willMutate`** (`migration_runner.go`):
   - `verifyAppliedVersionsMatchRegistry(ctx, state.appliedVersion)`.
   - A non-`*VersionContentMismatchError` error is returned as-is.
   - A mismatch triggers `repairVersionContentDriftWithRollback(ctx, appliedVersion, mismatch)`; on clean repair, **no error reaches the caller**.
4. `!state.willMutate()` → return nil (no snapshot, no writes).
5. `s.ensureQuarantineTable(ctx)` (`migration_runner.go`), then `s.commitWorkingSet(ctx, "migrate: ensure migration_quarantine table")`; commit failure → `"commit quarantine table: %w"`. Committed **before** `guard.ensure` so it survives a later checkpoint reset (`migration_runner.go`).
6. `s.quarantineFastFail(ctx, state)` — **before** the snapshot guard, so a permanently-quarantined workspace does not accumulate a snapshot per Open (`migration_runner.go`).
7. If `phaseAdopt`: `s.verifyIssuesReconcilable(ctx)`; failure → `"reconcile pre-goose workspace: %w"` — also **before** the snapshot (`migration_runner.go`).
8. `guard.ensure(ctx)`; failure → `"migrate: %w"` (`migration_runner.go`).
9. `migrationPostSnapshotHookForTest` if set (`migration_runner.go`).
10. If `phaseAdopt` (`migration_runner.go`):
    a. `s.reconcileToBaseline(ctx, guard)`; failure → `"reconcile pre-goose workspace: %w"`. Idempotent probe-driven forward migrator; explicitly a historical artifact to which no new operations get added (`migration_runner.go`).
    b. `s.verifyBaselineShape(ctx)`; error → `"verify post-reconcile baseline shape: %w"`. If any gaps remain → refuse with `"post-reconcile workspace shape still differs from baseline (remaining gaps: %s); reconcile cannot bring this workspace to v1 — the shape is structurally beyond what pre-goose reconcile can recover"` (`migration_runner.go`).
    c. `s.adoptPreGooseWorkspace(ctx)`.
    d. `s.commitWorkingSet(ctx, fmt.Sprintf("migrate: adopt pre-goose workspace at v%d", baselineVersion))`; failure → `"commit adoption stamp: %w"`.
11. `s.applyPendingMigrations(ctx)`.
12. `wrote, err := s.recordProducerBinaryVersion(ctx)`; `!wrote` → return nil; else `s.commitWorkingSet(ctx, "migrate: record producer binary version")` (`migration_runner.go`).

### 8.5 `applyPendingMigrations` — the goose loop

`applyPendingMigrations(ctx)` (`migration_runner.go`):
1. `newGooseProvider(s.db)` — constructed **before** the checkpoint so a construction failure leaves no orphan branch. Failure → `"construct migration provider: %w"` (`migration_runner.go`).
2. `checkpoint, err := s.CreateCheckpoint(ctx, "pre-migrate")`; failure → `"create migration checkpoint: %w"` (`migration_runner.go`).
3. `step` closure: uses `migrationUpByOneForTest` when set, else `s.upByOne(ctx, provider)` (`migration_runner.go`).
4. Loop:
   - `errors.Is(gooseErr, goose.ErrNoNextVersion)` → `s.PruneCheckpoints(ctx, "pre-migrate", 5)`; a prune failure IS returned as `"prune migration checkpoints: %w"`; then return nil (`migration_runner.go`).
   - other `gooseErr != nil` → `cpErr := s.handleMigrationFailure(...)`, then `_ = s.PruneCheckpoints(ctx, "pre-migrate", 5)` (error **deliberately ignored**), return `cpErr` (`migration_runner.go`).
   - success → `s.commitWorkingSet(ctx, migrationCommitMessage(result))`; failure → `"commit migration v%d: %w"` (`migration_runner.go`).

`upByOne(ctx, provider)` (`migration_runner.go`) is bare `provider.UpByOne(ctx)` — the ONE shared forward-step; the test hook wraps it only at the `applyPendingMigrations` call site (`migration_runner.go`).

`migrationCommitMessage(result)` = `fmt.Sprintf("migrate: v%d %s", result.Source.Version, filepath.Base(result.Source.Path))` (`migration_runner.go`).

`newGooseProvider(db)` = `goose.NewProvider(goose.DialectMySQL, db, migrations.FS)` (`migration_runner.go`).

### 8.6 Failure handling and quarantine

`handleMigrationFailure(ctx, result, cause, checkpoint)` (`migration_runner.go`), ordering **reset first, quarantine second**:
1. Extract `version`/`name` from `result.Source` when non-nil; `name = filepath.Base(result.Source.Path)` (`migration_runner.go`).
2. `s.ResetToCheckpoint(ctx, checkpoint.Name)`. Failure → `"migration v%d failed and Dolt reset to %q failed (%v); restore from dbsnapshot. Root cause: %w"` (`migration_runner.go`).
3. If `version > 0`:
   - `s.recordQuarantine(ctx, version, name, cause.Error())`; failure → `"migration v%d failed (reset to %q); quarantine insert failed (%v); restore from dbsnapshot. Root cause: %w"`.
   - `s.commitWorkingSet(ctx, fmt.Sprintf("migrate: quarantine v%d %s", version, name))`; failure → `"...quarantine commit failed (%v)..."` (`migration_runner.go`).
   A **nil-result failure (version 0) skips quarantine insertion entirely** (`migration_runner.go`).
4. Return `&CheckpointResetError{Version, Name, Checkpoint, Cause}`.

`recordQuarantine` (`migration_runner.go`): `INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE name = VALUES(name), error_text = VALUES(error_text), created_at = VALUES(created_at)` with `created_at = time.Now().UTC().Format(time.RFC3339Nano)`.

`quarantineFastFail(ctx, state)` (`migration_runner.go`): `effectiveApplied = state.appliedVersion`, but for `phaseAdopt` it is overridden to `baselineVersion` so a quarantine row for the baseline cannot block after adoption (`migration_runner.go`).

`checkPendingQuarantine(ctx, appliedVersion)` (`migration_runner.go`): `SELECT version, name, error_text FROM migration_quarantine WHERE version > ? ORDER BY version LIMIT 1`. `sql.ErrNoRows` → nil. Any row → `&QuarantineBlockError{...}`.

### 8.7 The quarantine table and its self-heal

`quarantineTableStmt` (`migration_runner.go`), verbatim:
```
CREATE TABLE migration_quarantine (
	version    BIGINT NOT NULL,
	name       TEXT NOT NULL,
	error_text TEXT NOT NULL,
	created_at VARCHAR(64) NOT NULL,
	PRIMARY KEY (version)
)
```
`canonicalQuarantineColumns = []string{"version", "name", "error_text", "created_at"}` (`migration_runner.go`).

`ensureQuarantineTable(ctx)` (`migration_runner.go`):
1. `s.tableColumns(ctx, "migration_quarantine")`; error → `"ensure migration_quarantine table: %w"`.
2. `len(cols) == 0` → execute `quarantineTableStmt`.
3. `quarantineShapeMatches(cols)` → nil.
4. else `recreateQuarantineTable(ctx, cols)`.

`quarantineShapeMatches` (`migration_runner.go`): exact set equality — **neither a subset nor a superset** passes.

`recreateQuarantineTable` (`migration_runner.go`):
1. `SELECT COUNT(*) FROM migration_quarantine`; error → `"ensure migration_quarantine table: count rows in stale-shape table: %w"`.
2. `count > 0` → **refuse**: `"migration_quarantine has a non-canonical shape (columns: %s) and %d row(s) of history; refusing to recreate automatically — this needs manual triage, not self-heal"` with the columns sorted (`migration_runner.go`).
3. `count == 0` → `DROP TABLE migration_quarantine` (error `"...: drop stale-shape table: %w"`), then re-execute `quarantineTableStmt` (error `"...: recreate: %w"`).

### 8.8 Version-content drift detection

`alterAddColumnRe` (`migration_runner.go`), verbatim:
`(?is)ALTER\s+TABLE\s+`?([A-Za-z_][A-Za-z0-9_]*)`?\s+ADD\s+COLUMN\s+`?([A-Za-z_][A-Za-z0-9_]*)`?`

`sqlKeywordAfterAddColumn = map[string]bool{"if": true}` (`migration_runner.go`) — discards the `IF` of `ADD COLUMN IF NOT EXISTS` so it registers as unparsed rather than as a bogus column named `if`.

Documented deliberate gap (`migration_runner.go`): MySQL's `ADD <col> <type>` with no `COLUMN` keyword contains no `"ADD COLUMN"` text for the literal count to catch, and is ambiguous with `ADD CONSTRAINT`/`ADD INDEX`/`ADD KEY`/`ADD UNIQUE`. No migration in this registry uses it.

`parseAddColumnTargets(name, up)` (`migration_runner.go`):
1. For each regex match: lowercase the column; skip if it is in `sqlKeywordAfterAddColumn`.
2. `terminatedStatement(up, m[0])`; `!ok` → error `"migration %q: ADD COLUMN statement starting at byte %d has no terminating ';' — cannot isolate it for repair"`.
3. Append `tableColumnTarget{table: lowercased, column, stmt}`.
4. **Loud gate**: `strings.Count(strings.ToUpper(up), "ADD COLUMN") > len(adds)` → error `"migration %q: found %d \"ADD COLUMN\" occurrence(s) in its Up section but alterAddColumnRe recognized only %d — a form such as \"ADD COLUMN IF NOT EXISTS\" or a parenthesized multi-column list is not parsed; widen alterAddColumnRe or rewrite the migration to the plain ADD COLUMN <name> <type> shape"` (`migration_runner.go`).

Tests: `TestParseAddColumnTargetsRecognizesPlainForm` (`migration_runner_test.go`), `...CapturesEachStatementSeparately`, `...ToleratesSemicolonInStringLiteral`, `...RejectsUnrecognizedForm`, `...RejectsMultiColumnForm`.

`terminatedStatement(s, start)` (`migration_runner.go`): scans forward tracking single-quote state; returns the trimmed text through the first **unquoted** `;`. Recognizes only doubled-quote (`''`) escaping, **not** backslash escapes (`migration_runner.go`).

`tableColumnTarget{table, column, stmt string}` (`migration_runner.go`) — `stmt` is the exact statement text, re-executed verbatim by the repair.
`migrationColumnAdds{version int64; name string; adds []tableColumnTarget}` (`migration_runner.go`).

`registryColumnAddsThroughVersion(maxVersion)` (`migration_runner.go`):
1. `migrations.FS.ReadDir(".")`; error → `"read migration registry: %w"`.
2. Skip directories and non-`.sql` entries.
3. `migrations.ParseVersion(entry.Name())`; `!ok` → error `"migration file %q does not begin with a numeric version"`.
4. **Skip `v <= baselineVersion` and `v > maxVersion`** (`migration_runner.go`).
5. Read the file, `parseAddColumnTargets(name, gooseUpSection(string(data)))`.
6. Sort ascending by version (`migration_runner.go`).

`missingVersionContent(ctx, appliedVersion)` (`migration_runner.go`): for each registered target, look up (and cache per table) `s.tableColumns(target.table)`; a target whose column is absent becomes a `missingContentTarget{version, name, table, column, stmt}` (`migration_runner.go`). Returned in ascending version order with each version's targets contiguous.

`verifyAppliedVersionsMatchRegistry(ctx, appliedVersion)` (`migration_runner.go`): reports only the **earliest** mismatched version — it walks the leading run of targets sharing `missing[0].version` and names them as `"<table>.<column>"` — returning `&VersionContentMismatchError{Version, Name, Missing}`.

`VersionContentMismatchError.Error()` (`migration_runner.go`):
`"migration v%d %q is recorded as applied, but its registered content is missing from this workspace's live schema: %s\n\nthis usually means the version number was reused for different historical content after this workspace last migrated — the recorded applied version does not reflect what actually ran here"`.

### 8.9 Version-content drift repair

`repairVersionContentDrift(ctx, appliedVersion)` (`migration_runner.go`): executes **every** missing target's captured `stmt` verbatim, in the returned (ascending-version) order — not just the earliest version's. An `ExecContext` failure → `"repair v%d %q: apply %q: %w"`. Each success appends `fmt.Sprintf("%s.%s (v%d %s)", table, column, version, name)`.

**Documented scope limit** (`migration_runner.go`): only `ADD COLUMN` targets are ever tracked or repaired. A version's `ADD CONSTRAINT` or data-backfill statements — explicitly naming `00003_add_resolution.sql`'s `issues_resolution_check` and `00004`'s `redirect_target` backfill — are **never re-applied**, even when the column they depend on was just repaired. `CREATE TABLE`, `ADD CONSTRAINT`-only, `DROP`/`RENAME COLUMN`, and data-only migrations are not tracked as content to verify or repair.

`repairVersionContentDriftWithRollback(ctx, appliedVersion, mismatch)` (`migration_runner.go`):
1. `s.CreateCheckpoint(ctx, "pre-drift-repair")`; failure → `"create version-content drift repair checkpoint: %w"`.
2. `repairVersionContentDrift`. On failure:
   - reset also fails → `"repair version-content drift (detected at v%d %q) failed (%v) and reset to checkpoint %q failed (%v); restore from dbsnapshot"` (`migration_runner.go`).
   - reset succeeds → `"repair version-content drift (detected at v%d %q) failed: %w (working set reset to checkpoint %q)"` (`migration_runner.go`).
3. `s.commitWorkingSet(ctx, migrationDriftRepairCommitMessage(repaired))`; failure → `"commit version-content drift repair: %w"`.
4. `s.PruneCheckpoints(ctx, "pre-drift-repair", 5)`; failure → `"prune version-content drift repair checkpoints: %w"` (**is** promoted).

`migrationDriftRepairCommitMessage(repaired)` = `fmt.Sprintf("migrate: repair version-content drift (%s)", strings.Join(repaired, ", "))` (`migration_runner.go`).

Tests: `TestOpenRepairsVersionSlotReuseContentMismatch` (`migration_runner_test.go`), `TestRepairVersionContentDriftWithRollbackResetsOnFailure` (`migration_runner_test.go`).

### 8.10 Baseline shape verification

`baselineSchema()` (`migration_runner.go`): `migrations.BaselineFileName()` → `migrations.FS.ReadFile(name)` (error `"read baseline migration %q: %w"`) → `parseCreateTableColumns(gooseUpSection(string(data)))`. Zero tables → error `"baseline migration %q defines no tables"`.

`gooseUpSection(sql)` (`migration_runner.go`): finds the case-insensitive index of `"-- +goose up"`; if absent returns the whole input. Otherwise takes from that index, and truncates at the case-insensitive index of `"-- +goose down"` when present.

`parseCreateTableColumns(sql)` (`migration_runner.go`): repeatedly finds `"create table"` case-insensitively, reads the first identifier as the table name, finds the `(`, extracts the balanced paren block, and maps lowercased table name → `columnNames(body)`. `CREATE INDEX` and everything else is ignored. ASCII lowercasing preserves byte indices so the keyword scan and the original-text slicing stay aligned (`migration_runner.go`).

`columnNames(body)` (`migration_runner.go`): for each top-level item, take the first identifier; skip empty and `isConstraintKeyword`; lowercase.

`isConstraintKeyword(token)` (`migration_runner.go`) — uppercased match against exactly: `CONSTRAINT`, `PRIMARY`, `FOREIGN`, `KEY`, `CHECK`, `UNIQUE`, `INDEX`.

`splitTopLevel(body)` (`migration_runner.go`): splits at depth-0, unquoted commas; tracks `'` quoting and `(`/`)` depth.
`parenBlock(s)` (`migration_runner.go`): quote- and depth-aware; unbalanced input yields an empty body and consumes `len(s)`.
`firstIdentifier(s)` (`migration_runner.go`): skips leading whitespace, consumes `isIdentByte` bytes and backticks, then strips backticks.
`isIdentByte(b)` (`migration_runner.go`): `_`, `a-z`, `A-Z`, `0-9`.

`verifyBaselineShape(ctx)` (`migration_runner.go`): for each table in `sortedKeys(schema)` (deterministic, `migration_runner.go`): `tableColumns(table)`; empty → append `"<table>"` to `missing` and continue (`present` NOT incremented); otherwise `present++` and for each expected column absent from the actual set append `"<table>.<column>"`. **Column NAMES only** are compared — not types, constraints, or indexes (`migration_runner.go`).

`tableColumns(ctx, table)` (`migration_runner.go`): `SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`, lowercasing each name. An absent table yields an empty set.
`tableExists(ctx, table)` (`migration_runner.go`): `SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1`; `sql.ErrNoRows` → `false, nil`.

`refuseIfBaselineMissing(ctx, state)` (`migration_runner.go`): `verifyBaselineShape`; refuses when `present == 0 || len(missing) > 0`, returning `&UnsupportedSchemaVersionError{WorkspaceVersion: state.appliedVersion, MaxSupported: state.registryMaxVers, MissingBaseline: missing, SnapshotName: s.mostRecentMigrationSnapshotName()}`. **Performs no write.**

Tests: `TestOpenToleratesAheadOfRegistryWhenBaselineIntact` (`migration_runner_test.go`), `TestOpenToleratesGooseLogWithOnlyAheadRow`, `TestOpenRefusesAheadOfRegistryWhenBaselineCorrupt`, `TestOpenAllowsWorkspaceExactlyAtMax`, `TestOpenToleratesHealthyManagedWorkspace`.

### 8.11 Adoption

`adoptPreGooseWorkspace(ctx)` (`migration_runner.go`), in order:
1. `database.NewStore(goose.DialectMySQL, "goose_db_version")`; error `"adopt: construct goose store: %w"`.
2. `store.CreateVersionTable(ctx, s.db)`; error `"adopt: create version table: %w"`.
3. `store.Insert(ctx, s.db, database.InsertRequest{Version: baselineVersion})`; error `"adopt: stamp baseline version: %w"`.
4. `DELETE FROM meta WHERE meta_key = 'schema_version'`; error `"adopt: drop legacy schema_version key: %w"`.

Tests: `TestPreGooseAdoptionStampsWithoutRerunningBaseline` (`migration_runner_test.go`), `TestAdoptionDeletesLegacySchemaVersionKey`.

### 8.12 Producer-version stamp

`recordProducerBinaryVersion(ctx)` (`migration_runner.go`): `version.Get()`; error → `"read version info: %w"`. `info.IsDev` → `(false, nil)` — a **dev build records no row**. Otherwise `s.setMeta(ctx, nil, "producer_binary_version", info.Version)` → `(true, nil)`; error → `"record producer binary version: %w"`. Test `TestProducerBinaryVersionUnstampedForDevBuild` (`migration_runner_test.go`). Nothing in this binary reads the row.

`mostRecentMigrationSnapshotName()` (`migration_runner.go`): `dbsnapshot.List(migrationSnapshotsDir(s.doltRootDir))`; a listing error degrades to `""`; otherwise the first entry passing `IsMigrationSnapshotName` (the list is newest-first).

### 8.13 Migration error types

`CheckpointResetError{Version int64; Name string; Checkpoint storage.Checkpoint; Cause error}` (`migration_runner.go`). `Error()` (`migration_runner.go`):
- `Version == 0` → `"migration failed (version unknown): %v\n\nthe working set was automatically reset to Dolt checkpoint %q\nrestore from the pre-migration recovery snapshot"`.
- else → `"migration v%d %q failed: %v\n\nthe working set was automatically reset to Dolt checkpoint %q\nto retry after fixing the migration, clear the quarantine:\n  DELETE FROM migration_quarantine WHERE version = %d"`.
`Unwrap()` at `migration_runner.go`.

`QuarantineBlockError{Version, Name, ErrorText}` (`migration_runner.go`). `Error()` (`migration_runner.go`):
`"migration v%d %q is quarantined after a previous failure:\n  %s\n\nto recover, either:\n  (a) restore from a dbsnapshot: lit snapshots restore <name>\n  (b) clear the quarantine row (if transient): DELETE FROM migration_quarantine WHERE version = %d"`.

`UnsupportedSchemaVersionError{WorkspaceVersion, MaxSupported int64; MissingBaseline []string; SnapshotName string}` (`migration_runner.go`). `Error()` (`migration_runner.go`) composes:
1. `"please upgrade lit (your workspace is at schema version %d; this binary supports up to %d"`.
2. If `len(MissingBaseline) > 0`: `"; the live schema is also missing baseline shape: %s"` (comma-joined).
3. `")"`.
4. Always: `"\n\nto operate this workspace, install a lit that supports schema version %d:\n  lit upgrade\n\n(this is the supported path — `lit upgrade` runs even from this too-old binary, installs the latest release, and refuses a release that cannot operate this workspace.)"` (`WorkspaceVersion`).
5. If `SnapshotName != ""` → `"\n\nif you are stuck and need to roll back this workspace to match this binary:\n  lit snapshots restore %s\n\nthis is a LOSSY recovery — any data written under the newer binary will be discarded."`; else → `"\n\nno pre-upgrade snapshot available; lossy rollback is not possible from this workspace."`.
Tests: `TestUnsupportedSchemaVersionMessageShape` (`migration_runner_test.go`), `TestRefusalSurfacesRecoveryDataFromWorkspace`.

### 8.14 The transient schema lift

`liftWorkingSetToRegistry(ctx)` (`migration_runner.go`):
1. `newGooseProvider(s.db)`; failure → `"construct schema-lift provider: %w"`.
2. Infinite loop calling the **bare** `s.upByOne(ctx, provider)` (never the test hook):
   - `goose.ErrNoNextVersion` → return nil.
   - other error → `"lift working set to registry schema: %w"`.
   - `result == nil` with nil error → **fails loud** rather than spinning: `"lift working set to registry schema: goose returned no result and no error"` (`migration_runner.go`).
It **commits nothing to Dolt, takes no checkpoint, and takes no snapshot** (`migration_runner.go`). Its only caller is the reconcile, on a throwaway scratch branch. Test `TestLiftWorkingSetToRegistryRecoversDowngradedSchema` (`sync_reconcile_schema_skew_test.go`).

---

## PART 9 — Migration snapshot guard (`internal/store/migrate_snapshot.go`)

### 9.1 Snapshot naming and classification

Snapshot names follow dbsnapshot's `<unix-ns>[-<label>]` scheme; the system stamps the label as `"<label>-<unix-ns>"` (`migrate_snapshot.go`).

`isStampedSnapshotName(name, label)` (`migrate_snapshot.go`) — the shared shape rule:
1. Find the first `'-'`; absent → false.
2. `head` (before it) must be all digits.
3. `rest` must have prefix `label + "-"`.
4. The remainder after that prefix must be all digits.
So the full shape is `<unix-ns>-<label>-<unix-ns>`.

`isAllDigits(s)` (`migrate_snapshot.go`): non-empty and every rune in `'0'..'9'`.

Three disjoint classifiers share it: `IsMigrationSnapshotName` (label `"pre-migrate"`, `migrate_snapshot.go`), `IsDowngradeSnapshotName`, and `IsReconcileSnapshotName` (label `"pre-reconcile"`, `sync_reconcile.go`). Because the labels differ, the three retention budgets never collect each other's snapshots (`migrate_snapshot.go`).

Tests: `TestIsMigrationSnapshotNameRejectsUserCollisions` (`migrate_snapshot_test.go`), `TestMigrationPruneSparesUserSnapshots` (`migrate_snapshot_test.go`), `TestIsReconcileSnapshotNameDisjoint` (`sync_reconcile_schema_skew_test.go`).

### 9.2 `snapshotGuard`

`snapshotGuard{databaseDir, snapshotsDir, label string; taken *dbsnapshot.Snapshot}` (`migrate_snapshot.go`); `newSnapshotGuard` (`migrate_snapshot.go`).

`ensure(ctx)` (`migrate_snapshot.go`): if `taken != nil` return the cached snapshot. Otherwise `dbsnapshot.Take(ctx, databaseDir, snapshotsDir, label)`; failure → `"snapshot before migration: %w"`. **Idempotent within one invocation** — this is what makes exactly one snapshot per reconcile survive N GC-contention retries.

`took()` (`migrate_snapshot.go`): `(Snapshot, bool)` discriminating "we have a recovery point" from "no mutation happened".

Helpers must not call `dbsnapshot.Take` directly (`migrate_snapshot.go`).

### 9.3 Paths and labels

`migrationSnapshotsDir(databaseDir)` (`migrate_snapshot.go`) = `filepath.Join(filepath.Dir(filepath.Clean(databaseDir)), "snapshots")` — a **sibling** of the dolt directory, matching the commit-lock's sibling convention.

`formatMigrationSnapshotLabel(t)` (`migrate_snapshot.go`) = `fmt.Sprintf("pre-migrate-%d", t.UTC().UnixNano())`.

### 9.4 `MigrationRollbackError`

`{Snapshot dbsnapshot.Snapshot; Cause error}` (`migrate_snapshot.go`). `Error()` (`migrate_snapshot.go`):
`"migrate: %v\n\nthe workspace state before this migration is preserved at:\n  %s\n\nto restore, run:\n  lit snapshots restore %s"` with `Snapshot.Path` and `Snapshot.Name`. `Unwrap()` at `migrate_snapshot.go`. `asMigrationRollbackError(err)` (`migrate_snapshot.go`) is the centralized `errors.As`.

Tests: `TestMigrateSnapshotFreshDBOpenTakesExactlyOneSnapshot` (`migrate_snapshot_test.go`), `TestMigrateSnapshotNoOpOpenTakesNoSnapshot`, `TestMigrateSnapshotFailureSurfacesRestoreCommand`, `TestMigrateSnapshotRestoreRoundTripsPreMutationState`, `TestMigrateSnapshotPruneEnforcesRetention`, `TestDataSurvivesFailedMigrationSnapshotRestore`.

---

## PART 10 — Dolt checkpoints (`internal/store/checkpoint.go`)

- `CreateCheckpoint(ctx, prefix)` (`checkpoint.go`): `readDoltHead` → commit SHA (error `"checkpoint: %w"`); `ts := time.Now().UTC()`; `name := fmt.Sprintf("%s-%d", prefix, ts.UnixNano())`; `CALL DOLT_BRANCH(?)` with that name (error `"checkpoint: create branch %q: %w"`). Returns `storage.Checkpoint{Name, Prefix, CreatedAt: ts, Anchor: commitSHA}`.
- `ResetToCheckpoint(ctx, name)` (`checkpoint.go`): `CALL DOLT_RESET('--hard', ?)`; error `"checkpoint: reset to %q: %w"`. Discards working-set changes **and** any Dolt commits made after the checkpoint.
- `ListCheckpoints(ctx, prefix)` (`checkpoint.go`): `SELECT name, hash FROM dolt_branches WHERE name LIKE ? ORDER BY name` with `prefix+"-%"`. Names failing `parseCheckpointName` are **skipped silently**. `Anchor` set from `hash`. Final sort is by `CreatedAt` ascending (oldest first).
- `PruneCheckpoints(ctx, prefix, retain)` (`checkpoint.go`): `retain < 0` → `"checkpoint: retain must be non-negative, got %d"`. `len(cps) <= retain` → nil. Otherwise deletes `cps[:len(cps)-retain]` (the oldest) with `CALL DOLT_BRANCH('-d', '-f', ?)`; error `"checkpoint: delete branch %q: %w"`. `retain=0` deletes all.
- `parseCheckpointName(name, prefix)` (`checkpoint.go`): requires the exact prefix + `'-'`, and the suffix must round-trip through `%d` unchanged (`fmt.Sprintf("%d", ns) != suffix` → reject). `CreatedAt = time.Unix(0, ns).UTC()`.

Retention in use: `migrationCheckpointRetention = 5` for both `"pre-migrate"` and `"pre-drift-repair"` prefixes (`migration_runner.go`).

---

## PART 11 — `internal/merge`: every merge and conflict rule

Files: `/Users/bmf/code/links-issue-tracker/internal/merge/merge.go`, `collision.go`, `resolve.go`, `resolve_prose.go`.

### 11.1 Signatures

merge.go: `MergeResult struct{ export model.Export; Pending []ProsePending; Collisions []Collision }` (merge.go; `Collision` is declared at collision.go); `(MergeResult) Settled() (model.Export, bool)`, ok only when `Pending` and `Collisions` are both empty (merge.go); `(MergeResult) Provisional() (model.Export, bool)`, ok only when `Collisions` is empty (merge.go); `side uint8`; `issueScope map[string]side` with `admits`; `sidedRows[T any]`; `bothSidesOf[T any]`; `ThreeWay(base model.Export, local model.Export, remote model.Export) MergeResult` (merge.go); `mapIssues`; `optionalIssuePtr`; `unionIssueIDs`; `issueChanged`; `issueEqual`; `issueProjection` struct; `issueProjectionFrom`; `mergeRelations`; `enforceSingleParent`; `breakParentCycles`; `maxString`; `mergeComments`; `mergeLabels`; `mergeEvents`; `maxInt`.

collision.go: `Collision{IssueID string; Ours model.Issue; Theirs model.Issue}` with JSON tags `issue_id/ours/theirs`; `SameEntity{base *model.Issue; ours, theirs model.Issue; oursWS, theirsWS string}` — every field unexported, so outside the package `Classify` is the only way to obtain one; `Classify(base *model.Issue, ours, theirs model.Issue, oursWS, theirsWS string) (SameEntity, *Collision)`; `ancestorOf(base *model.Issue, ours model.Issue) *model.Issue`; `SortCollisions(collisions []Collision) []Collision`.

resolve.go: `ProseField string` with `ProseTitle="title"`, `ProseDescription="description"`, `ProsePrompt="agent_prompt"`; `ProsePending{IssueID,Field,Base,Ours,Theirs}` with JSON tags `issue_id/field/base/ours/theirs`; `IssueResolution{merged model.Issue; Pending []ProsePending}` with `Settled() (model.Issue, bool)` and `Provisional() model.Issue`; `ResolveIssue(in SameEntity) IssueResolution`; `resolver` struct; `twoTier[T comparable]`; `(*resolver).prose`; `(*resolver).resolveStatus`; `(*resolver).tiebreak`; `typedTiebreak[T ~string]`; `resolveClosePayload`; `closePayload`; `closePayloadOf`; `(*resolver).derivedFlagTime`; `higher[T cmp.Ordered]`; `stateRank`; `stateFromRank`; `boolBase`; `mergeLabelNames`; `presentOr`; `nameSet`; `unionNameSet`; `latest`; `earliestTime`; `cloneTimePtr`.

resolve_prose.go: `type Fingerprint string`; `fingerprintBytes = 6`; `(ProsePending) Fingerprint() Fingerprint`; `ParseFingerprint(text string) (Fingerprint, bool)`; `ProseResolution{Fingerprint, Text}` — **no JSON tags**; `proseKey{IssueID, Field}`; `ApplyProseResolutions(result MergeResult, resolutions []ProseResolution) (model.Export, bool)`; `applyIssueProse`; `SortPending`.

### 11.2 `ThreeWay` — export-level algorithm

1. Three id→issue maps from `base.Issues`, `local.Issues`, `remote.Issues`; a duplicate id within one export lets the **last row in slice order** win (merge.go).
2. Candidate id set = union of all three, **sorted ascending** (merge.go).
3. Per id, a copied `*model.Issue` per side, `nil` when absent (merge.go).
4. `localChanged := issueChanged(basePtr, localPtr)`, `remoteChanged := issueChanged(basePtr, remotePtr)` (merge.go).

The four-way presence/change branch (merge.go):

| Condition | Result |
|---|---|
| neither changed | append `baseIssue` if `hasBase`; else nothing (merge.go) |
| only local changed | append `localIssue` if present; else nothing — local deleted, remote untouched → row dropped (merge.go) |
| only remote changed | append `remoteIssue` if present; else nothing (merge.go) |
| both changed, both present, `Classify` returns a `*Collision` | append the collision to the collision list and append `localIssue` unchanged; `ResolveIssue` is not called and no pending entry is added (merge.go) |
| both changed, both present, `Classify` returns a `SameEntity` | `ResolveIssue(same)`; append `resolution.Provisional()` and append its `Pending...` to the export pending list (merge.go) |
| both changed, local only | append `localIssue` — remote removed it, local edited → the surviving edit is preserved (merge.go) |
| both changed, remote only | append `remoteIssue` (merge.go) |
| both changed, neither present | append nothing — converged removal (merge.go) |

`Classify` is called as `Classify(basePtr, localIssue, remoteIssue, local.WorkspaceID, remote.WorkspaceID)` (merge.go), so `ours` = local, `theirs` = remote, `oursWS = local.WorkspaceID`, `theirsWS = remote.WorkspaceID`. Deletion here is whole-row absence; soft deletion is a `DeletedAt` stamp on a present row and travels through the retention field instead (merge.go). The result's `Collisions` is `SortCollisions(collisions)` (merge.go).

**`Classify`** (collision.go): when `ours.CreatedAt.Equal(theirs.CreatedAt)` is false it returns `(SameEntity{}, &Collision{IssueID: ours.ID, Ours: ours, Theirs: theirs})`; otherwise it returns a `SameEntity` carrying `ours`, `theirs`, both workspace ids and `ancestorOf(base, ours)`, with a nil `*Collision`. The test is `time.Time.Equal`, so two encodings of one instant in different offsets compare equal (collision.go). The base row takes no part in deciding a collision.

**`ancestorOf`** (collision.go): returns `nil` when `base` is nil or `base.CreatedAt` is not `Equal` to `ours.CreatedAt`; otherwise returns `base`. A base row whose `CreatedAt` differs from the shared instant reaches `ResolveIssue` as no base.

**`SortCollisions`** (collision.go): copies the slice and sorts it by `IssueID`; the input is not mutated.

Collision tests (`collision_test.go`), fixture `plantCollision` = a shared `epic.1` plus an `epic.16` created at a different instant on local (`wsA`) and remote (`wsB`), with different titles and descriptions:
- — `ThreeWay` reports one collision on `epic.16` carrying both rows whole; `Settled()` and `Provisional()` both return `ok=false`; the provisional export's `epic.16` is the local row with its own title and description.
- — identical title and description on both sides, different `CreatedAt`, empty base → zero pending, one collision, `Settled()` `ok=false`.
- — one `CreatedAt` on both sides and no base → `Classify` reports no collision, and `ThreeWay` returns no collisions with `Settled()` `ok=true`.
- — base and ours share `CreatedAt`, theirs differs → collision.
- — ours and theirs share `CreatedAt`, base differs → no collision, and the `SameEntity` has a nil base.
- — base, ours and theirs share `CreatedAt` → no collision, and the base is kept.
- — base, ours and theirs carry three different instants → collision carrying both rows whole.
- — a collided `epic.16` with one relation, comment, label and event per side → the provisional export holds exactly the local side's four rows.

### 11.3 Change detection

`issueChanged = !issueEqual` (merge.go). `issueEqual`: both nil → true; exactly one nil → false; else `reflect.DeepEqual` of the two `issueProjection`s (merge.go).

`issueProjection` (merge.go), built by `issueProjectionFrom` (merge.go), captures: `ID`, `Title`, `Description`, `Prompt`, `Priority`, `IssueType`, `Topic`, `Assignee` (via `issue.AssigneeValue()`), `Rank`, `Lane`, `Labels` (copied via `append([]string{}, issue.Labels...)`, normalizing `nil` and `[]string{}` to the same value), `CreatedAt`, `UpdatedAt`, `Retention` (via `issue.Retention()`), `Capabilities` (via `issue.Capabilities()`).

`Capabilities` carries the whole leaf lifecycle payload — status value, `closed_at`, `resolution`, `redirect_target` (`model.StatusView`, `/Users/bmf/code/links-issue-tracker/internal/model/capabilities.go`) — so a change to any of those four registers as "this side moved" (merge.go). `issue.Capabilities()` returns the empty `Capabilities{}` for containers (`/Users/bmf/code/links-issue-tracker/internal/model/model.go`); for an unhydrated leaf it **panics** through `mustLifecycle` (`model.go`), so `ThreeWay` panics on an unhydrated leaf issue.

Tests: `merge_test.go` (nil vs empty `Labels` compare equal, so JSON round-trip drift does not synthesize changes); `merge_test.go` (`issueChanged` is true for a resolution-only re-close `wontfix`→`duplicate` at identical status and `closed_at`); `merge_test.go` (a JSON-round-tripped hydrated **epic** compares clean); `resolve_test.go` (a `Prompt`-only or `Lane`-only edit is detected and survives).

### 11.4 Merged export assembly (merge.go)

- `mergedIssues` sorted ascending by `ID` (merge.go).
- `scope` is an `issueScope` (`map[string]side`, where `side` is the bit set `fromLocal = 1`, `fromRemote = 2`, `bothSides = 3`, merge.go). Every surviving issue id is set to `bothSides` (merge.go); each collided id is then set to `fromLocal` (merge.go). `scope.admits(id, from)` is `scope[id]&from != 0`, so an id absent from the merged issues admits no side (merge.go). The scope gates every side-table.
- `Version = maxInt(local.Version, remote.Version, base.Version)` (merge.go; `maxInt` returns 1 for an empty argument list, merge.go).
- `WorkspaceID = local.WorkspaceID` — remote's is discarded (merge.go).
- `ExportedAt = local.ExportedAt` — remote's is discarded (merge.go).
- `Relations = mergeRelations(scope, local.Relations, remote.Relations)` — **base not passed** (merge.go).
- `Comments = mergeComments(scope, local.Comments, remote.Comments)` — base not passed (merge.go).
- `Labels = mergeLabels(scope, base.Labels, local.Labels, remote.Labels)` — the only three-way side-table (merge.go).
- `Events = mergeEvents(scope, local.Events, remote.Events)` — base not passed (merge.go).

`Settled()` returns `(export, len(Pending)==0 && len(Collisions)==0)` (merge.go); `Provisional()` returns `(export, len(Collisions)==0)` (merge.go). Both return the export value alongside `ok`, including when `ok` is false. The export field is unexported, so those two methods are the only access. Tests `resolve_test.go` (pending prose → `Settled()` `ok=false`; a clean merge → `ok=true`) and `collision_test.go` (a collision → `Settled()` and `Provisional()` both `ok=false`).

### 11.5 Side-table rules

**Relations** (`mergeRelations`, merge.go): key is the triple `{Src, Dst, Type}` (merge.go). Iterates `bothSidesOf(locals, remotes)` — locals first, then remotes (merge.go) — writing into `merged[key]`, so on an identical key the **remote row wins** its `CreatedAt`/`CreatedBy` (merge.go). Referential filter: a row is dropped unless `scope` admits **both** `SrcID` and `DstID` from the side the row was read from (merge.go), so a relation touching a collided id survives only from the local side. Then `enforceSingleParent` (merge.go), then sort by `SrcID`, `DstID`, `Type` (merge.go). `Type` values: `"blocks"`, `"parent-child"`, `"related-to"` (`/Users/bmf/code/links-issue-tracker/internal/model/relation_type.go`). `blocks` and `related-to` are purely additive — test `resolve_test.go` asserts a local `blocks` edge and a remote `related-to` edge both survive (2 relations).

**Single parent** (`enforceSingleParent`, merge.go): non-`parent-child` relations pass straight through (merge.go). Parent-child edges are keyed by `SrcID` (the child); the child keeps exactly one edge — the one with the **lexicographically greatest `DstID`** (`!seen || relation.DstID > existing.DstID`) (merge.go). Order-independent. Then `breakParentCycles(parentOf)` (merge.go) and the survivors are appended (merge.go); the caller sorts. Test `resolve_test.go`.

**Cycle breaking** (`breakParentCycles`, merge.go): walks parent edges from each unsettled key (merge.go); a walk stops at a node with no parent edge or already `settled` (merge.go). On revisiting a node already on the current path at index `idx`, it **deletes the map entry keyed by `maxString(path[idx:])`** — the lexicographically greatest child id inside the loop — and stops (merge.go). The victim child becomes a root; nothing is reparented. Every node on the path is then marked `settled` (merge.go). `maxString` panics on an empty slice (merge.go) but is only ever called with the non-empty `path[idx:]`. Tests: `merge_test.go` (tail-entered cycle `a→b→c→b` → `c`'s edge deleted, `a→b`/`b→c` untouched); `merge_test.go` (clean 3-cycle `a→b→c→a` → exactly one edge removed, victim `c`); `resolve_test.go` (end-to-end: local `a→b`, remote `b→a` → exactly one parent edge).

**Comments** (`mergeComments`, merge.go): two-way union keyed by `Comment.ID` over `bothSidesOf(locals, remotes)`, so when both sides carry one comment id the **remote row wins** (merge.go); a row is dropped unless `scope` admits its `IssueID` from the side it was read from (merge.go); sorted by `ID` (merge.go). Test `resolve_test.go`.

**Events** (`mergeEvents`, merge.go): identical shape — union keyed by `IssueEvent.ID`, remote row wins when both sides carry one event id, dropped unless `scope` admits its `IssueID` from the side it was read from, sorted by `ID`.

**Labels table** (`mergeLabels`, merge.go) — the only three-way side-table. Key `{IssueID, Name}` (merge.go). Local rows are kept only when `scope` admits their `IssueID` from `fromLocal`, remote rows only from `fromRemote`; base rows are not filtered (merge.go). `baseSet`/`localSet`/`remoteSet` are computed from the base rows and the filtered side rows (merge.go). Metadata rows: filtered locals first then filtered remotes, so **remote wins ties** on `CreatedAt`/`CreatedBy` (merge.go). Candidate keys = base ∪ local ∪ remote (merge.go); dropped unless `scope.admits(IssueID, bothSides)`, which holds for every id in the scope (merge.go). Membership: `twoTier(true, inBase, inLocal, inRemote, presentOr)` — `hasBase` **hardcoded `true`**, so an empty base makes every present label an add (merge.go). A key passing the membership test but with no row in `rows` (i.e. only in base) contributes nothing (merge.go). Sorted by `IssueID`, `Name` (merge.go). Test `resolve_test.go`: base `[a,b]`, local `[a]`, remote `[a,b]` → merged table is `[a]`; `b` is not resurrected.

Test `collision_test.go` pins the side filter on all four side-tables: for a collided id, only the local side's relation, comment, label and event survive, and none of the local rows is dropped.

### 11.6 `ResolveIssue` — per-row field merge

`ResolveIssue(in SameEntity) IssueResolution` (resolve.go). `base, ours, theirs := in.base, &in.ours, &in.theirs`; `hasBase = base != nil`, and `r.base` is assigned only when `hasBase` (resolve.go). `r.id = ours.ID` (resolve.go). `ours` and `theirs` point at values inside the `SameEntity`, so neither is nil. Outside the package a `SameEntity` comes only from `Classify` (collision.go), so `ours.CreatedAt` and `theirs.CreatedAt` are `Equal`, and `base` is nil unless its `CreatedAt` is `Equal` to that instant too (collision.go).

**The one merge primitive — `twoTier`** (resolve.go):
```
oursChanged   := !hasBase || ours != base
theirsChanged := !hasBase || theirs != base
```
| Case | Result |
|---|---|
| ours moved, theirs didn't | `ours` (resolve.go) |
| theirs moved, ours didn't | `theirs` (resolve.go) |
| neither moved | `ours` (resolve.go) |
| both moved | `tier2(ours, theirs)` (resolve.go) |

With `hasBase == false`, both sides count as changed, so **every field goes straight to Tier 2** (resolve.go). Tier 2 is entered even when both sides moved to the same value, so every tier2 function must be idempotent on equal inputs — all of them are (`higher`, `tiebreak`, `presentOr`, the prose closure).

**Type resolution and basis selection**: `mergedType := twoTier(hasBase, base.IssueType, ours.IssueType, theirs.IssueType, typedTiebreak[model.IssueType](&r))` (resolve.go), assigned with `merged.IssueType = mergedType` (resolve.go). The merged row is built by copying **`ours`**, except when the resolved type differs from `ours.IssueType` and equals `theirs.IssueType`, in which case **`theirs`** is the basis (resolve.go). Consequence: every field `ResolveIssue` does not explicitly re-merge is inherited verbatim from that basis side — lifecycle, and (for containers) status/assignee/close payload.

**Field-by-field** (resolve.go):

| Field | Base operand | Tier 2 policy | Line |
|---|---|---|---|
| `IssueType` | `base.IssueType` | symmetric workspace tiebreak |, |
| `Title` | `base.Title` | prose → `ProsePending{Field:"title"}`, returns `ours` provisionally | |
| `Description` | `base.Description` | prose → `ProsePending{Field:"description"}` | |
| `Prompt` | `base.Prompt` | prose → `ProsePending{Field:"agent_prompt"}` | |
| `Priority` | `base.Priority` | `higher` — numerically greater wins; `PriorityUrgent=1` beats `PriorityNormal=0` (`/Users/bmf/code/links-issue-tracker/internal/model/priority.go`) | |
| `Topic` | `base.Topic` | symmetric workspace tiebreak | |
| `Lane` | `base.Lane` | symmetric workspace tiebreak | |
| `Rank` | `base.Rank` | symmetric workspace tiebreak | |
| `Labels` | `base.Labels` | per-name two-tier, `presentOr` at Tier 2 | |
| `ID` | — | always `ours.ID`; never merged | |
| `CreatedAt` | — | `base.CreatedAt` when `hasBase`; else `ours.CreatedAt`, which `Classify` has proven `Equal` to `theirs.CreatedAt` | |
| `UpdatedAt` | — | always `latest(ours.UpdatedAt, theirs.UpdatedAt)`; never two-tier | |
| retention | per-flag booleans | `presentOr`, timestamp slaved | |
| status/assignee/closed_at/resolution/redirect_target | see below | leaves only | |

When `hasBase == false`, `r.base` is the zero `model.Issue`, so all base operands are zero values — and they are ignored anyway since both sides count as changed (resolve.go).

**Prose** (`(*resolver).prose`, resolve.go): Tier 1 takes whichever single side moved the text off base, with **no** agent involvement and no pending entry. Tier 2 (both moved): if `ours == theirs`, the agreed text is returned with **no** pending entry (resolve.go). Otherwise a `ProsePending{IssueID: r.id, Field: field, Base: base, Ours: ours, Theirs: theirs}` is appended and **`ours` is returned as a provisional value** (resolve.go). The engine never picks a prose winner. Pending entries are appended in field order title → description → prompt (resolve.go).

**Label names** (`mergeLabelNames`, resolve.go): converts base/ours/theirs to sets, iterates the union, applies `twoTier(hasBase, inBase, inOurs, inTheirs, presentOr)` per name. Semantics: added by either → kept; removed by exactly one while the other left it → **stays removed**; removed by both → removed; both add → kept. Returns `nil` (not an empty slice) when nothing survives; else sorted ascending. Tests `resolve_test.go` (base `[keep]`, ours `[keep,ours]`, theirs `[keep,theirs]` → `[keep, ours, theirs]`), `resolve_test.go` (base `[a,b]`, ours `[a]`, theirs `[a,b]` → `[a]`).

**Leaf lifecycle** (`resolveStatus`, resolve.go) — runs only when `!mergedType.IsContainer()` (resolve.go; `IsContainer()` is true only for `epic`, `/Users/bmf/code/links-issue-tracker/internal/model/issue_type.go`):
- Base operands read only when `hasBase`; else `baseState = 0`, `baseAssignee = ""` (resolve.go).
- **status**: `stateFromRank(twoTier(hasBase, baseState, stateRank(ours), stateRank(theirs), higher))` (resolve.go). Ranks: `closed = 2`, `in_progress = 1`, everything else `0` → `open` (resolve.go). Tier 2 is the dominant-state join.
- **assignee**: `twoTier(hasBase, baseAssignee, ours.AssigneeValue(), theirs.AssigneeValue(), r.tiebreak)` written to `merged.Assignee` (resolve.go).
- **closed_at / resolution / redirect_target**: all three stay `nil` unless the merged state is `closed` (resolve.go). When closed: `closedAt = earliestTime(ours.ClosedAtValue(), theirs.ClosedAtValue())` — earliest non-nil, cloned (resolve.go); `resolution, redirectTarget = resolveClosePayload(ours, theirs, r.tiebreak)` (resolve.go).
- Re-hydration via `model.HydrateStatus(merged, model.StatusView{...})` (resolve.go); a returned error **panics** (resolve.go).
- A merged **container** inherits its lifecycle, status, assignee, and close payload untouched from the basis side — never merged.

Status table test `TestResolveIssueStatusTwoTier` (resolve_test.go), all asserting zero pending:

| base | ours | theirs | want |
|---|---|---|---|
| in_progress | in_progress | in_progress | in_progress |
| closed | open | closed | **open** (reopen via Tier 1) |
| open | open | in_progress | in_progress |
| open | in_progress | closed | closed |
| closed | open | in_progress | in_progress (both moved off closed; higher rank) |

Also: `resolve_test.go` (priority base normal / ours urgent / theirs normal → urgent); `resolve_test.go` (both closed at t2/t1 → `closed_at` = t1; reopen → `closed_at` **nil** even though theirs carries one); `resolve_test.go` (`ID` stays `i1` and `CreatedAt` is the shared instant `t0`, both with a base carrying `t0` and with a base carrying `t2`, which `Classify` drops); `resolve_test.go` (`base = nil`: `in_progress` vs `closed` → closed, and the diverged title yields exactly one `ProseTitle` pending).

**Tiebreak** (`(*resolver).tiebreak`, resolve.go): if `oursWS != theirsWS`, the value from the **lexicographically greater workspace id** wins (`oursWS > theirsWS` → `ours`, else `theirs`). If the workspace ids are equal (the code calls this "defensive"), the **lexicographically greater value** wins (`ours >= theirs` → `ours`, else `theirs`). Symmetric by construction. `typedTiebreak[T ~string]` wraps it for named string types (resolve.go). Tests `resolve_test.go` (topic base `root`, ours `alice`@wsA, theirs `bob`@wsB → `bob`, same under argument swap), `resolve_test.go` (same for `Assignee`).

**Close payload atom** (`resolveClosePayload`, resolve.go; `closePayloadOf`, resolve.go): `closePayloadOf` projects a side to `{resolution, target}`; if `ResolutionValue()` is nil the whole payload is empty and `target` is read only under a non-nil resolution — so `{resolution:"", target:"x"}` is unconstructible. Branch order (resolve.go):
1. `o == t` (struct equality) → that payload.
2. `o.resolution == ""` → `t` wins.
3. `t.resolution == ""` → `o` wins.
4. resolutions differ → `tiebreak(o.resolution, t.resolution)`; the winning resolution's **own** payload, target included, is taken whole.
5. same resolution, `o.target == ""` → `t`.
6. same resolution, `t.target == ""` → `o`.
7. same resolution, two real targets → `tiebreak(o.target, t.target)` selects the whole payload.
Return `(nil, nil)` when the winning resolution is empty; else `(*model.Resolution, *string)` with `target` nil when the winner's target is empty. It never mixes one side's resolution with the other side's target.

`TestResolveClosePayloadAtomicity` (resolve_test.go) runs each row in **both** argument orders with a "larger string wins" stand-in tiebreak:

| ours | theirs | want resolution | want target |
|---|---|---|---|
| closed, none | closed, none | `""` | `""` |
| closed, none | duplicate/`links-canon` | `duplicate` | `links-canon` |
| obsolete/`""` | duplicate/`links-canon` | `obsolete` | `""` |
| duplicate/`""` | duplicate/`links-canon` | `duplicate` | `links-canon` |
| duplicate/`links-aaa` | duplicate/`links-bbb` | `duplicate` | `links-bbb` |
| duplicate/`links-canon` | duplicate/`links-canon` | `duplicate` | `links-canon` |

End-to-end guard `resolve_test.go`: open-vs-redirecting-close → closed with `redirect_target = links-canon`; a reopen winning on state → `RedirectTargetValue() == nil`.

**Retention** (resolve.go): each side's retention is projected to the two-timestamp wire pair via `model.RetentionTimestamps` (resolve.go; encoder at `/Users/bmf/code/links-issue-tracker/internal/model/lifecycle/retention.go`). Each flag merges independently through `derivedFlagTime(base bool, ours, theirs *time.Time)`: `set := twoTier(hasBase, base, ours != nil, theirs != nil, presentOr)`; `!set` → `nil`; else `earliestTime(ours, theirs)` (resolve.go). The timestamp is **derived** from the resolved flag, never merged on its own. `boolBase(t, hasBase) = hasBase && t != nil` (resolve.go). The pair is folded back via `model.RetentionFromTimestamps`, whose rule is `deletedAt != nil → Deleted; archivedAt != nil → Archived; else Live` — **deletion dominates** (`/Users/bmf/code/links-issue-tracker/internal/model/lifecycle/retention.go`). Retention merges for containers as well as leaves — it runs before the `IsContainer` gate (resolve.go).

`TestResolveIssueRetentionRaces` (resolve_test.go):

| scenario | result |
|---|---|
| no base; ours Archived@t1, theirs Deleted@t2 | `Deleted{At: t2}` |
| live base; ours Archived@t1, theirs Deleted@t2 | `Deleted{At: t2}` |
| no base; Deleted@t2 vs Deleted@t1 | `Deleted{At: t1}` (earliest) |
| Deleted@t1 base; ours restored, theirs unchanged | `Live` |
| Archived@t1 base; ours unarchived, theirs unchanged | `Live` |
| Archived@t1 base; both unarchived | `Live` (convergent clear via Tier 2) |
| Deleted@t1 base; both restored | `Live` |

Plus `resolve_test.go`: both archive (t2 vs t1) off a live base → `Archived{At: t1}`; only ours archives at t2 → `Archived{At: t2}`.

**Time helpers**: `latest(a,b)` = `a.After(b) ? a : b`, ties yield `b` (resolve.go). `earliestTime(a,b *time.Time)`: `a == nil` → clone of `b`; `b == nil` → clone of `a`; `a.Before(*b)` → clone of `a`; else clone of `b`, so ties yield `b`; every result is a fresh pointer via `cloneTimePtr`, which returns nil for nil (resolve.go). `higher[T cmp.Ordered]` = `ours >= theirs ? ours : theirs` (resolve.go).

`IssueResolution.Settled()` → `(merged, len(Pending)==0)` (resolve.go); `Provisional()` → `merged` unconditionally (resolve.go); `merged` is unexported. Test `resolve_test.go`.

### 11.7 Prose resolution surface (`resolve_prose.go`)

**There is no text-diff machinery in this package.** No line-level or hunk-level diffing, no `<<<<<<<`/`=======`/`>>>>>>>` conflict markers, no diff3, no similarity heuristics. A prose conflict is whole-field: the three complete strings travel in `ProsePending{Base, Ours, Theirs}` (resolve.go, populated by `resolver.prose` at resolve.go, the append at resolve.go), and the agent's answer is one complete replacement string `ProseResolution.Text` (resolve_prose.go) assigned wholesale to the field (resolve_prose.go).

**`Fingerprint()`** (resolve_prose.go): `Fingerprint(hex.EncodeToString(sha256.Sum256([]byte(p.IssueID + "\x00" + string(p.Field) + "\x00" + p.Base + "\x00" + p.Ours + "\x00" + p.Theirs))[:fingerprintBytes]))` with `fingerprintBytes = 6` (resolve_prose.go) — a **12-lowercase-hex-character** truncation of SHA-256 over the issue id, the field name and the three texts joined by NUL bytes. The same field text diverged identically on two issues yields two different fingerprints.

**`ParseFingerprint(text)`** (resolve_prose.go): returns `(Fingerprint(text), true)` only when `hex.DecodeString(text)` succeeds, yields exactly `fingerprintBytes` bytes, and `hex.EncodeToString` of those bytes equals `text`, so only exactly 12 lowercase hex characters pass; anything else returns `("", false)`.

**`ApplyProseResolutions(result, resolutions) (model.Export, bool)`** (resolve_prose.go):
1. Builds `pendingByFingerprint map[Fingerprint]proseKey` from `result.Pending`: live `Fingerprint()` → `{IssueID, Field}` (resolve_prose.go). Pending fields that share a fingerprint collapse to one entry (last wins).
2. Per supplied resolution, looked up by `resolution.Fingerprint`:
   - **Reject** (`return model.Export{}, false`) if the fingerprint is not in `pendingByFingerprint` (resolve_prose.go).
   - **Reject** if the looked-up `{IssueID, Field}` already has a text in this call — a second resolution for one field (resolve_prose.go).
   - Else record `resolvedByKey[key] = resolution.Text` (resolve_prose.go).
3. **Reject** if `len(resolvedByKey) != len(result.Pending)` — the completeness gate, counted against the pending slice rather than `pendingByFingerprint`, so pending fields that collapsed to one fingerprint entry fail it (resolve_prose.go).
4. **Reject** if `result.Provisional()` returns `ok=false`, which it does when the merge holds an id collision (resolve_prose.go; merge.go).
5. On success: allocates a **fresh** `[]model.Issue` and copies the provisional slice (resolve_prose.go), applies `applyIssueProse` to each element, assigns the new slice, returns `(export, true)`. The original `MergeResult`'s issue slice is not mutated; Relations/Comments/Labels/Events are shared by reference with the provisional export.
6. Pure — no IO, no clock (resolve_prose.go).

Every rejection returns the **zero** `model.Export{}` paired with `false` (resolve_prose.go) — never a partially-spliced export.

**`applyIssueProse`** (resolve_prose.go): the single `ProseField` → field mapping as a map of setter closures — `ProseTitle → issue.Title`, `ProseDescription → issue.Description`, `ProsePrompt → issue.Prompt`. A field is written only if `resolved[{issue.ID, field}]` exists.

**`SortPending`** (resolve_prose.go): copies the slice and sorts by `IssueID`, then `Field` (raw string, so `"agent_prompt" < "description" < "title"`). Does not mutate the input. Not called anywhere inside the package.

Prose tests (`resolve_prose_test.go`), fixture `prosePendingFixture` = one issue `i1` with concurrent title *and* description rewrites → exactly 2 pending; `fingerprintOf` returns a pending field's live fingerprint:
- — exact bijection with live fingerprints splices `merged-title`/`merged-desc`, and the original provisional export is asserted **not** mutated in place.
- — one resolution for a two-field pending set → rejected.
- — fingerprint `"deadbeefcafe"`, which names no live conflict, plus a valid description resolution → whole set rejected.
- — two resolutions carrying the title's fingerprint plus the description's → rejected; the comment notes the count gate alone cannot catch this.
- — issues `i1` and `i2` with the same title rewrite on each → 2 pending fields; resolving each by its own fingerprint succeeds and gives each issue its own merged text.
- — `ParseFingerprint` returns a live fingerprint unchanged and refuses `""`, `"deadbeef"`, `"deadbeefcafe00"`, `"DEADBEEFCAFE"`, `"deadbeefcafg"`, and `"i1:title:<live fingerprint>"`.
- — fixture `proseAndCollisionFixture` holds one pending title and one id collision; an exact bijection is rejected and the returned export has no issues.

### 11.8 Errors, sentinels, aborts in `internal/merge`

The package declares **no** error type, no sentinel `var Err...`, and **no function returns `error`**. Every failure is a `bool` second return: `MergeResult.Settled` (merge.go), `MergeResult.Provisional` (merge.go), `IssueResolution.Settled` (resolve.go), `ParseFingerprint` (resolve_prose.go), `ApplyProseResolutions` (resolve_prose.go).

The only explicit abort is `panic(err)` when `model.HydrateStatus` returns an error inside `resolveStatus` (resolve.go). Implicit panics reachable from this package: `maxString` on an empty slice (merge.go, only called at merge.go with the non-empty `path[idx:]`); `issueProjectionFrom → issue.Capabilities() → mustLifecycle` on an unhydrated non-container (`/Users/bmf/code/links-issue-tracker/internal/model/model.go`), reached from `issueChanged` for every id (merge.go); `model.RetentionTimestamps` on an illegal retention variant (`/Users/bmf/code/links-issue-tracker/internal/model/lifecycle/retention.go`), called at resolve.go; `merged.SetRetention(...)` on an illegal retention variant (`model.go`), called at resolve.go. `ResolveIssue` dereferences no caller-supplied pointer that can be nil: `ours` and `theirs` are addresses of `SameEntity` fields, and `base` is dereferenced only when `hasBase` (resolve.go).

### 11.9 Ordering and determinism

- Candidate issue ids sorted ascending before iteration (merge.go) — pending order, collision discovery order and merge order are deterministic.
- Merged issues sorted by `ID` (merge.go); collisions by `IssueID` (merge.go, collision.go); relations by `(SrcID, DstID, Type)` (merge.go); comments by `ID` (merge.go); events by `ID` (merge.go); label table by `(IssueID, Name)` (merge.go); label names on a row ascending (resolve.go); `SortPending` by `(IssueID, Field)` (resolve_prose.go).
- Every Tier-2 policy is symmetric in its two inputs — `higher` (resolve.go), `presentOr` (resolve.go), workspace `tiebreak` (resolve.go), `resolveClosePayload` (resolve.go, verified in both orders at resolve_test.go) — so both machines compute the same winner without a clock and without knowing which side is "ours".
- Causality comes from the merge-base, not timestamps: `UpdatedAt` is never consulted to pick a field winner and is itself an output (resolve.go). `CreatedAt` is consulted only by `Classify` and `ancestorOf`, to decide whether two rows are one ticket and whether the base row is its ancestor (collision.go); the merged `CreatedAt` is an output (resolve.go). The merge-base rule is stated at resolve.go.
- The single-parent winner (`max DstID`, merge.go) and the cycle victim (`max child id in the loop`, merge.go) are order-independent functions of the data.
- Non-deterministic residue: map-iteration order feeds `enforceSingleParent` (merge.go) and `breakParentCycles`' start-node choice (merge.go); both are followed by an order-independent selection rule and a final sort.

### 11.10 Additional test-pinned merge behaviors

- `merge_test.go` — concurrent title rewrite through `ThreeWay` yields exactly one pending with `IssueID="i1"`, `Field=ProseTitle`, `Base="issue"`, `Ours="local-change"`, `Theirs="remote-change"`.
- `merge_test.go` — with a completely **empty base** `model.Export{}`, `ThreeWay` degrades to a two-way union: `only-local`, `only-remote`, and `shared` all appear; the shared id's diverged title is held with `Base=""`, `Ours="from-B"` (local, wsB), `Theirs="from-A"` (remote, wsA), never auto-picked. *(This is exactly the combine path's projection — see §5.13.)*
- `merge_test.go` — a lone-side resolution-only re-close (`wontfix` base, local `duplicate`, remote unchanged) merges to `duplicate` with zero pending.
- `merge_test.go` — disjoint edits (local changed `i1`, remote changed `i2`) both survive, zero pending.
- `resolve_test.go` — Tier-1 prose: only ours rewrote the title (`a`→`b`) → title `b`, zero pending.
- `resolve_test.go` — Tier-2 prose is limited to the field that actually diverged: title and prompt identical, description divergent → exactly one pending carrying all three description versions.
- `resolve_test.go` — local removed the whole row, remote edited it → the remote edit survives (`Title == "edited"`).
- `resolve_test.go` — a base-only id absent on both sides → zero issues; no zero-value row is appended.
- Fixtures/clocks: `issueWithStatus` (merge_test.go), `jsonRoundTripIssue` (merge_test.go), `provisional` (merge_test.go), `leaf` (resolve_test.go), `open` (resolve_test.go), `same` (resolve_test.go; routes a fixture through `Classify` and fails the test if it is a collision), `closedLeaf` (resolve_test.go), `issueClosedWith` (merge_test.go), `parentEdges` (merge_test.go), `hasParentCycle` (merge_test.go), `plantCollision` (collision_test.go); `t0 = 2026-01-01`, `t1 = 2026-02-01`, `t2 = 2026-03-01` UTC (resolve_test.go).

## PART 12 — The migration registry (`internal/store/migrations/`)

Base: `/Users/bmf/code/links-issue-tracker/internal/store/migrations/`.

### 12.1 Registry-wide conventions

**Directive syntax.** All five `.sql` files are goose migrations delimited by SQL line comments:
- `-- +goose Up` begins the up section (`00001_baseline.sql`, `00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql`).
- `-- +goose Down` begins the down section (`00001_baseline.sql`, `00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql`).
- `-- +goose StatementBegin` / `-- +goose StatementEnd` wrap each individual statement (e.g. `00001_baseline.sql`, `00005_add_event_attribution.sql`). **Every executable statement in every file, in both sections, is inside exactly one such pair** — there are no bare statements (`00001_baseline.sql`, `00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql`).

**Naming/numbering.** `<NNNNN>_<name>.sql`, 5-digit zero-padded, `00001_baseline.sql` … `00005_add_event_attribution.sql`; the accept-shape is enforced by `bounds.go`. `embed.go` states subsequent migrations append with strictly ascending versions and that only SQL migrations are wired — both the embed and `registryMaxVersion` scan `*.sql`.

**Idempotency, per statement.**
- **Up sections: no idempotency guards anywhere.** Every `CREATE TABLE` is bare, no `IF NOT EXISTS` (`00001_baseline.sql`). Every `CREATE INDEX` is bare (`00001_baseline.sql`). Every `ALTER TABLE ... ADD COLUMN` is bare (`00002_add_lane.sql`, `00003_add_resolution.sql`, `00004_add_redirect_target.sql`, `00005_add_event_attribution.sql`). Every `ADD CONSTRAINT` is bare (`00003_add_resolution.sql`, `00004_add_redirect_target.sql`).
- **`00001_baseline.sql` down section: every statement uses `IF EXISTS`** — all seven `DROP TABLE IF EXISTS` (`00001_baseline.sql`).
- **Down sections of 00002–00005: no `IF EXISTS` on any statement** (`00002_add_lane.sql`; `00003_add_resolution.sql`; `00004_add_redirect_target.sql`; `00005_add_event_attribution.sql`).
- The one insert-conflict tolerance in the registry is `INSERT IGNORE INTO relations(...)` in the 00004 down section (`00004_add_redirect_target.sql`).

### 12.2 `00001_baseline.sql`

Header declares "FROZEN FILE — DO NOT EDIT", "the immutable definition of schema v1", that structural changes go in a new numbered file, that the gate is `TestBaselineFileIsFrozen` and updating the pinned hash is **not** the correct response including for comment-only or whitespace edits. records that a fresh workspace applies the file while a pre-goose workspace already at this shape is adopted (stamped v1) without re-running it, that CHECK constraints carry explicit deterministic names for `SHOW CREATE TABLE` stability, and that priority bounds mirror `model.PriorityNormal` (0) and `model.PriorityUrgent` (1).

On-disk sha256: `e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb` (matches `baseline_frozen_test.go`).

**Up section.**

`meta`: `meta_key VARCHAR(191) PRIMARY KEY`; `meta_value TEXT NOT NULL`.

`issues`:

| column | type | null | default | key | line |
|---|---|---|---|---|---|
| `id` | `VARCHAR(191)` | — | none | `PRIMARY KEY` | |
| `title` | `TEXT` | `NOT NULL` | none | | |
| `description` | `TEXT` | `NOT NULL` | none | | |
| `agent_prompt` | `TEXT` | `NULL` | none | | |
| `status` | `VARCHAR(32)` | `NULL` | none | | |
| `priority` | `INT` | `NOT NULL` | none | | |
| `issue_type` | `VARCHAR(32)` | `NOT NULL` | none | | |
| `topic` | `VARCHAR(191)` | `NOT NULL` | none | | |
| `assignee` | `TEXT` | `NOT NULL` | none | | |
| `created_at` | `VARCHAR(64)` | `NOT NULL` | none | | |
| `updated_at` | `VARCHAR(64)` | `NOT NULL` | none | | |
| `closed_at` | `VARCHAR(64)` | `NULL` | none | | |
| `archived_at` | `VARCHAR(64)` | `NULL` | none | | |
| `deleted_at` | `VARCHAR(64)` | `NULL` | none | | |
| `item_rank` | `TEXT` | `NOT NULL` | `DEFAULT ''` | | |

Named CHECK constraints on `issues`:
- `CONSTRAINT issues_status_check CHECK ((issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed')))`
- `CONSTRAINT issues_priority_check CHECK (priority >= 0 AND priority <= 1)`
- `CONSTRAINT issues_type_check CHECK (issue_type IN ('task','feature','bug','chore','epic'))`

`relations`: `src_id VARCHAR(191) NOT NULL`; `dst_id VARCHAR(191) NOT NULL`; `type VARCHAR(32) NOT NULL`; `created_at VARCHAR(64) NOT NULL`; `created_by TEXT NOT NULL`; `PRIMARY KEY (src_id, dst_id, type)`; `FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE`; `FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE`; `CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'))`.

`comments`: `id VARCHAR(191) PRIMARY KEY`; `issue_id VARCHAR(191) NOT NULL`; `body TEXT NOT NULL`; `created_at VARCHAR(64) NOT NULL`; `created_by TEXT NOT NULL`; `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE`.

`labels`: `issue_id VARCHAR(191) NOT NULL`; `label VARCHAR(191) NOT NULL`; `created_at VARCHAR(64) NOT NULL`; `created_by TEXT NOT NULL`; `PRIMARY KEY (issue_id, label)`; `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE`.

`issue_events`: `id VARCHAR(191) PRIMARY KEY`; `issue_id VARCHAR(191) NOT NULL`; `action VARCHAR(64) NULL`; `reason TEXT NOT NULL`; `actor TEXT NOT NULL`; `created_at VARCHAR(64) NOT NULL`; `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE`.

`issue_event_changes`: `event_id VARCHAR(191) NOT NULL`; `field VARCHAR(64) NOT NULL`; `from_value TEXT NULL`; `to_value TEXT NULL`; `PRIMARY KEY (event_id, field)`; `FOREIGN KEY (event_id) REFERENCES issue_events(id) ON DELETE CASCADE`.

Indexes, in file order:
1. `CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);`
2. `CREATE INDEX idx_issues_rank ON issues(item_rank(191));` — prefix index of length 191 on the `TEXT` column
3. `CREATE INDEX idx_relations_src_type ON relations(src_id, type);`
4. `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);`
5. `CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);`
6. `CREATE INDEX idx_labels_issue ON labels(issue_id, label);`
7. `CREATE INDEX idx_labels_name ON labels(label, issue_id);`
8. `CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at);`

No backfill `INSERT`/`UPDATE` exists in this file.

**Down section** — seven `IF EXISTS` drops, child-tables first, each in its own statement pair: `issue_event_changes`, `issue_events`, `labels`, `comments`, `relations`, `issues`, `meta`. No index drops (indexes go with their tables). No data preservation.

### 12.3 `00002_add_lane.sql`

sha256 `9126cd9e9c3a01137898fb9023c50b3f8741950f1e5d943dad521852939a4b75`.

**Up** — one statement: `ALTER TABLE issues ADD COLUMN lane text NOT NULL DEFAULT '';` — column `lane`, type lowercase `text`, `NOT NULL`, `DEFAULT ''`. No index, no constraint, no backfill statement (the default populates existing rows). Comment: lane partitions an epic's children into parallel rank-ordered sub-sequences; children sharing a lane are sequenced by rank, different lanes run in parallel; the empty-string default is "one lane value like any other"; lane is meaningful only within an epic and the gate predicate enforces that scoping, not the column.

**Down** — one statement: `ALTER TABLE issues DROP COLUMN lane;`. Declared LOSS CONTRACT: the down does not restore per-child lane assignments; with no lane column every child falls back to the shared default lane (fully-sequential epic gate); exact lane values require restoring from a pre-upgrade dbsnapshot.

### 12.4 `00003_add_resolution.sql`

sha256 `47b50dea1a1e2b7e31ba6b0fa95f5f77c6411b81ee525b04e1ff5e5fdb469563`.

**Up** — two statements:
1. `ALTER TABLE issues ADD COLUMN resolution VARCHAR(32) NULL;` — nullable, no default.
2. `ALTER TABLE issues ADD CONSTRAINT issues_resolution_check CHECK (resolution IS NULL OR resolution IN ('duplicate','superseded','obsolete','wontfix'));`

No index, no backfill. Comment: resolution is the sealed close reason; NULL on every non-closed row and on a `done` close; the CHECK seals the value set at the DB boundary, mirroring `relations_type_check`, with `ParseResolution` as the primary gate and this as defense in depth.

**Down** — two statements, constraint first: `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check;`, then `ALTER TABLE issues DROP COLUMN resolution;`. LOSS CONTRACT: recorded close reasons are not preserved; every closed ticket falls back to "closed with no recorded resolution".

### 12.5 `00004_add_redirect_target.sql`

sha256 `e171b9a18f13d67967e6c235499fc35254125ac23b36f9a687eddc7c411b590d`.

**Up** — four statements:

1. `ALTER TABLE issues ADD COLUMN redirect_target VARCHAR(191) NULL;`. Nullable, no default, **no foreign key** — explicitly noted.
2. `ALTER TABLE issues ADD CONSTRAINT issues_redirect_target_check CHECK (redirect_target IS NULL OR resolution IN ('duplicate','superseded'));`. The comment at states the CHECK is one-directional on purpose: a redirecting resolution with an unknown target stays representable; the write boundary requires a target for every new redirecting close.
3. Backfill UPDATE, verbatim:
```sql
UPDATE issues i
SET redirect_target = (
  SELECT IF(r.src_id = i.id, r.dst_id, r.src_id)
  FROM relations r
  WHERE r.type = 'related-to' AND (r.src_id = i.id OR r.dst_id = i.id)
)
WHERE i.resolution IN ('duplicate','superseded')
  AND (
    SELECT COUNT(*)
    FROM relations r2
    WHERE r2.type = 'related-to' AND (r2.src_id = i.id OR r2.dst_id = i.id)
  ) = 1;
```
For issues whose `resolution` is `'duplicate'` or `'superseded'` **and** which have exactly one incident `related-to` edge (counting both directions), `redirect_target` is set to that edge's counterpart id. Rows with any other edge count are left NULL with edges intact.
4. Backfill DELETE, verbatim:
```sql
DELETE r FROM relations r
JOIN issues i ON i.redirect_target IS NOT NULL
  AND ((r.src_id = i.id AND r.dst_id = i.redirect_target)
    OR (r.dst_id = i.id AND r.src_id = i.redirect_target))
WHERE r.type = 'related-to';
```
Deletes only `related-to` edges whose endpoints are exactly (issue, its redirect_target) in either direction.

**Down** — three statements, re-materialize first:
1., verbatim:
```sql
INSERT IGNORE INTO relations(src_id, dst_id, type, created_at, created_by)
SELECT LEAST(i.id, i.redirect_target), GREATEST(i.id, i.redirect_target), 'related-to', COALESCE(i.closed_at, i.updated_at), 'unknown'
FROM issues i
WHERE i.redirect_target IS NOT NULL;
```
`src_id` = lexicographic min of the pair, `dst_id` = max, `type` literal `'related-to'`, `created_at` = `closed_at` falling back to `updated_at`, `created_by` = literal `'unknown'`.
2. `ALTER TABLE issues DROP CONSTRAINT issues_redirect_target_check;`
3. `ALTER TABLE issues DROP COLUMN redirect_target;`

LOSS CONTRACT: the redirect-vs-manual-peer distinction is lost; edge `created_at` is approximated by the close timestamp (fallback `updated_at`) and `created_by` by `'unknown'`; original stamps are not preserved. `INSERT IGNORE` tolerates two enumerated benign classes — (a) a manual edge already linking the same pair, (b) an FK-gap row: since `redirect_target` has no FK, a redirect whose canonical row was hard-deleted cannot re-materialize (relations' FK would reject it) and is silently skipped. The comment explicitly calls this "a silent skip, accepted here only because a migration has no per-row reporting channel".

### 12.6 `00005_add_event_attribution.sql`

sha256 `ed625b2817365ed357ad477cd0691994a93a65a7ab1005d1b05456c39d159a70`.

**Up** — two statements: `ALTER TABLE issue_events ADD COLUMN stream_id VARCHAR(64) NULL;` and `ALTER TABLE issue_events ADD COLUMN workspace_id VARCHAR(191) NULL;`. Both nullable, no default, no index, no constraint, **no backfill** — the comment states attribution "is historical fact, never backfilled", so a freshly upgraded repository derives zero claims. Claims are DERIVED from these stamps at read time with no claim table, and "Nothing user-, host-, or path-shaped may ever enter these columns; the database syncs to shared remotes".

**Down** — `ALTER TABLE issue_events DROP COLUMN stream_id;` and `ALTER TABLE issue_events DROP COLUMN workspace_id;`. LOSS CONTRACT: attribution collected on this version is not restored; the claim predicate reads unattributed history as zero claims.

### 12.7 `embed.go`

Package `migrations` (`embed.go`), imports `"embed"`. Embed directive `//go:embed *.sql` on `var FS embed.FS`. The glob is `*.sql` only — non-`.sql` files in the directory are not embedded. `FS` is the only exported symbol in the file, described as "the embedded goose migration registry… the one source the runner reads from; nothing applies schema outside this set". The package doc records that only SQL migrations are wired and that adding a Go migration would require registering it with goose and widening the embed pattern.

### 12.8 `bounds.go`

`const Baseline int64 = 1` (`bounds.go`) — the only literal version constant. Doc: the version `00001_baseline.sql` stamps; a pre-goose workspace already at the baseline shape is adopted by recording this version without re-running the CREATE TABLEs; the baseline is a property of the embedded registry (the lowest version in FS), not a parallel constant in consumer packages.

`ParseVersion(name string) (int64, bool)` (`bounds.go`):
1. `base := filepath.Base(name)` — directory components stripped.
2. `idx := strings.IndexByte(base, '_')`; `idx <= 0` → `(0, false)` — no underscore, or underscore at position 0, rejects.
3. Every byte of `base[:idx]` must be in `'0'..'9'`, else `(0, false)`.
4. `strconv.ParseInt(digits, 10, 64)`; on error `(0, false)`.
5. `(version, true)`.
The doc justifies the exactness against `fmt.Sscanf("%d")`, which would accept leading whitespace, signs, or `"00001a"`.

`MaxVersion() (int64, error)` (`bounds.go`): `FS.ReadDir(".")`; error wrapped `"read migration registry: %w"`. Skips `entry.IsDir()` and non-`.sql` names. A remaining name `ParseVersion` rejects is a **hard error**: `"migration file %q does not begin with a numeric version"`. Empty version list → `errors.New("migration registry is empty")`. Sorts ascending, returns the last. With the current registry: **5**.

`BaselineFileName() (string, error)` (`bounds.go`): same `ReadDir` and wrapped error, same dir/`.sql` skip; returns the first entry whose `ParseVersion` equals `Baseline` — currently `00001_baseline.sql`. Not found → `"no baseline migration (v%d) found in registry"`. **Unlike `MaxVersion`, a non-parseable `.sql` name is silently skipped here rather than erroring**.

Nothing in `bounds.go` enforces monotonicity or gap-freeness; version identity is enforced entirely by the tests below.

### 12.9 `baseline_frozen_test.go`

`const baselineFrozenHash = "e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb"` — equals the current file's sha256.

`TestBaselineFileIsFrozen`: reads `FS.ReadFile("00001_baseline.sql")` — the filename is **hardcoded**, not obtained via `BaselineFileName()`; read error → `"read embedded 00001_baseline.sql: %v"`. Computes `sha256.Sum256(data)` hex-encoded. Passes iff the hex equals the constant exactly. **The invariant is exact byte equality of the whole file**, comments and whitespace included — not a parsed-schema comparison. On mismatch, `t.Fatalf` with `want sha256:` / `got sha256:`, the instruction to revert and add `00002_<your-change>.sql` "(or the next free number)", an explicit statement that non-structural edits (comment, whitespace, typo) are also forbidden and belong in "a sibling .md, the package doc in embed.go, or schema_reconcile.go", and `DO NOT update baselineFrozenHash to match the new bytes.`. Design notes: one enforcer not two; one copy of the hash in Go rather than a duplicate in a workflow yaml; pin bytes rather than a parsed shape.

### 12.10 `down_section_test.go` — the invertibility gate

`hasDownSection(data []byte) bool`. Contract: reports whether the file contains a `+goose Down` section followed by at least one non-empty, non-comment SQL statement before EOF or the next `+goose Up`. Algorithm: splits on `"\n"`, tracks `inBlock` (block-comment carry) and `downSeen`; per line computes `content, exitedBlock := lineContent(line, inBlock)`. If not `inBlock` and `isGooseDirective(line,"down")` → `downSeen = true`, carry, continue. If not `inBlock` and `isGooseDirective(line,"up")` → **if `downSeen` already, return `false`** (the Down section ended without executable content); else carry and continue. Otherwise carry; skip while `!downSeen`; once `downSeen`, return `true` on the first line whose stripped content is non-blank after `TrimSpace`. Falls through to `false`.

`isGooseDirective(line, kind)`: trims surrounding whitespace; requires the prefix `"-- "` (two dashes plus one space) on the lowercased trimmed line; the body after `"-- "` is trimmed and compared with `strings.EqualFold(body, "+goose "+kind)` — **the whole remaining line must be exactly `+goose <kind>`**, case-insensitively. Directives with extra trailing text are rejected.

`lineContent(line string, inBlock bool) (string, bool)`: if `inBlock` on entry, search `*/`; absent → `("", true)`; else continue after the closer. Loop: find the earliest of `--`, `#`, `/*` via `earliest`; none → append remainder and return `(out, false)`; emit text before the marker; for `--` or `#` return immediately with `inBlock=false`; for `/*` skip to `*/`, and if there is no closer on this line return `(out, true)`, else continue after it. Documented limitation: quote-string awareness is out of scope; the gate is deliberately conservative — extra stripping means reject.

`earliest(a, b, c int) (int, string)`: smallest non-negative index among the three, tagged `"--"`, `"#"`, or `"/*"`; ties resolve to the first in the fixed order `{a,"--"}, {b,"#"}, {c,"/*"}` because the comparison is strict `<`. All negative → `(-1, "")`.

`TestEveryMigrationHasDownSection`: `FS.ReadDir(".")`, error → Fatalf; skips dirs and non-`.sql`, counting the rest in `sqlFiles`; each must satisfy `hasDownSection`, failure being a `t.Errorf` stating the Down section must contain at least one non-empty non-comment SQL statement between `-- +goose Down` and EOF (or the next `-- +goose Up`), that loss-making migrations should reconstruct the schema with documented loss or document the loss contract, and that "The presence of the Down section itself is non-negotiable". `sqlFiles == 0` → `t.Fatal("no *.sql files found in embedded registry")`. **No count is pinned to a specific number.** The doc names the runtime sibling `TestEveryMigrationDownIsExercised` in `internal/store`, which proves the section actually runs.

`TestHasDownSectionRejectsMissingShapes` — fifteen fixtures:

| # | fixture | body (escaped) | want |
|---|---|---|---|
| 1 | up only — no down marker | `-- +goose Up\nCREATE TABLE x (id INT);\n` | false |
| 2 | down marker, empty body | `…\n-- +goose Down\n` | false |
| 3 | down + line comments only | `…-- +goose Down\n-- nothing here\n-- still nothing\n` | false |
| 4 | hash comments only | `…-- +goose Down\n# nothing here\n# still nothing\n` | false |
| 5 | block comments only | `…-- +goose Down\n/* placeholder\n   spanning lines */\n` | false |
| 6 | mix of all comment styles | `…-- +goose Down\n-- line\n# hash\n/* block */\n` | false |
| 7 | only goose statement markers | `…-- +goose Down\n-- +goose StatementBegin\n-- +goose StatementEnd\n` | false |
| 8 | real DROP | `…-- +goose Down\nDROP TABLE x;\n` | true |
| 9 | statement-block-wrapped DROP | `…-- +goose Down\n-- +goose StatementBegin\nDROP TABLE x;\n-- +goose StatementEnd\n` | true |
| 10 | directive substring in block comment | `/* this block mentions -- +goose Down but is not a directive */` | false |
| 11 | directive substring in longer comment | `-- TODO: someday add a -- +goose Down section` | false |
| 12 | block-comment-spliced fake directive | `-- +goose D/*x*/own\nDROP TABLE x;\n` | false |
| 13 | unterminated block after down marker | `…-- +goose Down\n/* unterminated\nDROP TABLE x;\n` | false |
| 14 | multi-line block then DROP after closer | `…-- +goose Down\n/* multi\n line block */\nDROP TABLE x;\n` | true |
| 15 | case-insensitive markers | `-- +GOOSE UP\nCREATE TABLE x (id INT);\n-- +Goose Down\nDROP TABLE x;\n` | true |

Each runs as a subtest; mismatch is `t.Errorf("hasDownSection() = %v, want %v\nbody:\n%s", …)`.

### 12.11 `version_reuse_test.go` — the version-slot-reuse gate

Types: `pinnedMigration{file, sha256 string}`; `migrationFile{version int64; name, sha256 string}`; `reuseKind int` with, in iota order, `kindContentChanged`=0 (a pinned version's bytes no longer match its pin), `kindRenamed`=1 (content matches, filename changed), `kindUnpinned`=2 (an on-disk non-baseline version has no pin), `kindDeleted`=3 (a pinned version is gone from disk), `kindDuplicate`=4 (two on-disk files claim the same version); `reuseFinding{kind; version int64; pinned pinnedMigration; onDisk []migrationFile}`.

The literal pins, `pinnedVersionContent` — all four verified against the on-disk sha256s:
```go
2: {file: "00002_add_lane.sql",              sha256: "9126cd9e9c3a01137898fb9023c50b3f8741950f1e5d943dad521852939a4b75"},
3: {file: "00003_add_resolution.sql",        sha256: "47b50dea1a1e2b7e31ba6b0fa95f5f77c6411b81ee525b04e1ff5e5fdb469563"},
4: {file: "00004_add_redirect_target.sql",   sha256: "e171b9a18f13d67967e6c235499fc35254125ac23b36f9a687eddc7c411b590d"},
5: {file: "00005_add_event_attribution.sql", sha256: "ed625b2817365ed357ad477cd0691994a93a65a7ab1005d1b05456c39d159a70"},
```
Version 1 is deliberately absent — `baseline_frozen_test.go` is its content enforcer.

`detectVersionReuse(pinned, onDisk) []reuseFinding` — pure; precedence in order:
1. Group on-disk files by version.
2. `len(files) > 1` → `kindDuplicate` for that version, and **no content check on the pair**.
3. Version absent from `pinned` → `kindUnpinned`.
4. `f.sha256 != pin.sha256` → `kindContentChanged`; **takes priority over a filename difference on the same version**.
5. `f.name != pin.file` with content matching → `kindRenamed`.
6. Every pinned version with no on-disk file → `kindDeleted`.
7. Findings sorted by version ascending, ties by `kind` ascending.

`(reuseFinding).explain()` — one message per kind, all naming the version. Key literal steering text: `kindContentChanged` — "version %d has been REUSED under different content." with `released as:` / `now on disk:` lines, "goose keys migrations by version NUMBER, not by content", and "DO NOT change this version's pin in pinnedVersionContent to match the new bytes". `kindRenamed` — "version %d kept its content but was RENAMED: %s -> %s.", instructing restore-the-name or update-the-pin's-`"file"`-field in the same PR. `kindUnpinned` — "version %d (%s) is not pinned in pinnedVersionContent." plus the exact line to add, `  %d: {file: %q, sha256: %q},`. `kindDeleted` — "version %d was released as %s (sha256 %s) but is now MISSING from the registry." / "A released migration must never be deleted". `kindDuplicate` — "version %d is claimed by %d files: %s." with names sorted, "Renumber all but one to the next free version(s).". `default` — `"unhandled reuse finding kind %d for version %d — add a message in reuseFinding.explain"`.

`TestReleasedMigrationsAreContentPinned`: `FS.ReadDir(".")`, error → Fatalf; skips dirs and non-`.sql`; a `.sql` name `ParseVersion` rejects → `t.Fatalf("registry file %q does not begin with a numeric version", …)`; `v == Baseline` skipped entirely; each remaining file sha256'd into a `migrationFile`; every `detectVersionReuse` finding becomes a `t.Errorf(finding.explain())`.

`TestDetectVersionReuse` — base pins `2:{"00002_add_lane.sql","aaa"}`, `3:{"00003_add_resolution.sql","bbb"}`, `4:{"00004_add_redirect_target.sql","ccc"}`; `clean` mirrors them. Nine cases, compared by **kind + version only**:

| case | on-disk deviation | expected |
|---|---|---|
| clean | none | nil |
| content reuse | v2 sha `"DIFFERENT"` | `{kindContentChanged, 2}` |
| content reuse under rename | v3 renamed to `00003_renamed.sql` **and** sha `"DIFFERENT"` | `{kindContentChanged, 3}` (drift wins over rename) |
| pure rename | v3 named `00003_renamed.sql`, sha `"bbb"` | `{kindRenamed, 3}` |
| unpinned new migration | extra `00005_new.sql`/`"eee"` at v5, no pin | `{kindUnpinned, 5}` |
| clean append | v5 present AND pinned as `00005_new.sql`/`"eee"` | nil |
| deleted | v4 missing from disk | `{kindDeleted, 4}` |
| duplicate | `00002_add_lane.sql`/`"aaa"` and `00002_add_lane_again.sql`/`"zzz"` both v2 | `{kindDuplicate, 2}` only |
| combined | v2 sha `"DIFFERENT"`, v4 absent | `[{kindContentChanged,2},{kindDeleted,4}]` in that order |

Length mismatch → `t.Fatalf`; per-index kind/version mismatch → `t.Errorf`.

`TestEveryReuseKindHasMessage`: fixture pin `{file:"00007_x.sql", sha256:"cafef00d"}`; `one = [{7,"00007_x.sql","deadbeef"}]`; `two = one + {7,"00007_y.sql","beefcafe"}`. Constructs one finding per declared kind shaped exactly as `detectVersionReuse` would produce it — notably `kindUnpinned` with **no** `pinned`, `kindDeleted` with **no** `onDisk`, `kindDuplicate` with two entries — so an over-indexing `explain()` panics here rather than in production. Asserts for each that `explain()` does not contain `"unhandled reuse finding kind"` and does contain `"7"`.

### 12.12 `bounds_test.go`

`TestParseVersionShape` — twelve cases asserting `(want, ok)`:

| input | want | ok | note |
|---|---|---|---|
| `"00001_baseline.sql"` | 1 | true | |
| `"00042_add_foo.sql"` | 42 | true | |
| `"00002_x.sql"` | 2 | true | |
| `"path/to/00003_nested.sql"` | 3 | true | `filepath.Base` strips the dir |
| `"_no_digits.sql"` | 0 | false | missing leading digits |
| `"baseline.sql"` | 0 | false | no underscore |
| `"abc_baseline.sql"` | 0 | false | non-numeric prefix |
| `"00001.sql"` | 0 | false | no underscore (`idx <= 0`) |
| `"00001a_foo.sql"` | 0 | false | `Sscanf("%d")` would accept (returns 1) |
| `"-1_foo.sql"` | 0 | false | leading sign; Sscanf would return -1 |
| `" 1_foo.sql"` | 0 | false | leading whitespace |
| `"+1_foo.sql"` | 0 | false | explicit `+` sign |

ok-mismatch → `t.Errorf("ParseVersion(%q) ok = %v, want %v")` then `continue`; value mismatch when ok → `t.Errorf("ParseVersion(%q) = %d, want %d")`.

`TestMaxVersionReflectsEmbeddedRegistry`: calls `MaxVersion()`, error → Fatalf; independently rescans `FS.ReadDir(".")` skipping dirs and non-`.sql`, taking the max `ParseVersion` result, with an unparseable name → `t.Fatalf("registry contains non-parseable filename %q")`; asserts `max == expected` and `max >= Baseline`. Deliberately pins agreement with a fresh scan, **not** a hard-coded value — no literal `5` appears.

`TestBaselineFileNameMatches`: `BaselineFileName()` must not error; its result must be accepted by `ParseVersion`; the parsed version must equal `Baseline`; `FS.ReadFile(name)` must succeed.

### 12.13 Registry invariant summary

| invariant | enforcer | literals |
|---|---|---|
| `00001_baseline.sql` bytes never change | `TestBaselineFileIsFrozen` (`baseline_frozen_test.go`) | sha256 `e86c1aa3…d1fbb` |
| v2+ bytes never change; filename never changes; never deleted; never duplicated; always pinned | `TestReleasedMigrationsAreContentPinned` (`version_reuse_test.go`) via `detectVersionReuse` | four pins at `version_reuse_test.go` |
| every `.sql` has a non-empty non-comment Down section | `TestEveryMigrationHasDownSection` (`down_section_test.go`) | none pinned; registry must be non-empty |
| filename → version parse shape | `TestParseVersionShape` (`bounds_test.go`) over `ParseVersion` (`bounds.go`) | 12-row table |
| `MaxVersion()` equals a fresh FS scan and is ≥ `Baseline` | `TestMaxVersionReflectsEmbeddedRegistry` (`bounds_test.go`) | `Baseline = 1` (`bounds.go`) |
| `BaselineFileName()` round-trips to `Baseline` and is readable | `TestBaselineFileNameMatches` (`bounds_test.go`) | — |
| `explain()` is exhaustive over `reuseKind` | `TestEveryReuseKindHasMessage` (`version_reuse_test.go`) | version `7`, shas `cafef00d`/`deadbeef`/`beefcafe` |

## PART 13 — `internal/dbsnapshot`: snapshot format and lifecycle

Base: `/Users/bmf/code/links-issue-tracker/internal/dbsnapshot/`.

### 13.1 Files and build tags

| File | Build constraint | Ref |
|---|---|---|
| `snapshot.go` | none | `snapshot.go` |
| `residue.go` | none | `residue.go` |
| `clone_darwin.go` | `//go:build darwin` | `clone_darwin.go` |
| `clone_linux.go` | `//go:build linux` | `clone_linux.go` |
| `clone_other.go` | `//go:build !darwin && !linux` | `clone_other.go` |
| `snapshot_unix_test.go` | `//go:build unix` | `snapshot_unix_test.go` |

Each `clone_*.go` defines exactly one function, `cloneTree(ctx context.Context, src, dst string) error` — Darwin `clone_darwin.go`, Linux `clone_linux.go`, other `clone_other.go`. Platform variance is link-time file selection, not a runtime branch.

External dependency `github.com/promptctl/primitives/filelock` (`snapshot.go`, `residue.go`): `Acquire(ctx, lockPath string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, bool, error)` returns `(release, true, nil)` on acquisition, `(nil, false, nil)` on contention, `(nil, false, err)` on real failure; `maxAttempts == 1` is a non-blocking probe.

### 13.2 Exported surface

- `type Snapshot struct { Path string \`json:"path"\`; Name string \`json:"name"\`; Created time.Time \`json:"created"\` }` — `snapshot.go`
- `var ErrSnapshotMissing = errors.New("dbsnapshot: snapshot not found")` — `snapshot.go`
- `var ErrSnapshotsBusy = errors.New("snapshot producer beacon busy")` — `snapshot.go`
- `func Take(ctx context.Context, databaseDir, snapshotsDir, label string) (Snapshot, error)` — `snapshot.go`
- `func List(snapshotsDir string) ([]Snapshot, error)` — `snapshot.go`
- `func Restore(databaseDir, snapshotsDir, name string) (string, error)` — `snapshot.go`
- `func Prune(snapshotsDir string, keep int) error` — `snapshot.go`
- `func PruneMatching(snapshotsDir string, keep int, match func(name string) bool) error` — `snapshot.go`
- `func IsProducerArtifactName(name string) bool` — `snapshot.go`
- `func CollectOrphanedResidue(snapshotsDir string) error` — `residue.go`

Unexported: `reservedPaths{created time.Time; name, finalPath, tmpPath, reservePath string}` (`snapshot.go`); `producerBeaconPath`; `reserveSnapshotPaths`; `pathFree`; `formatName`; `validateSnapshotName`; `isCollectibleResidue`; `isCollectibleArtifactName`; `isCollectorCondemnedName`; `parseName`; `parsePositiveDigits`; `isMintableLabel`; `sanitizeLabel`; `isDoltJournalLockRel`; `walkAndCopy`; `plainFileCopy`; `copyWithContext`; `condemnResidue` (`residue.go`); `ficloneOrCopy` (Linux, `clone_linux.go`).

### 13.3 Constants

| Identifier | Value | Ref |
|---|---|---|
| `producerBeaconName` | `".links-snapshot-producer.lock"` | `snapshot.go` |
| `producerBeaconRetryAttempts` | `20` | `snapshot.go` |
| `producerBeaconRetryDelay` | `50 * time.Millisecond` | `snapshot.go` |
| `maxReserveAttempts` | `1024` | `snapshot.go` |
| `tmpSuffix` | `".tmp"` | `snapshot.go` |
| `reserveSuffix` | `".reserve"` | `snapshot.go` |
| `condemnedSuffix` | `".condemned"` | `snapshot.go` |
| `producerArtifactSuffixes` | `[]string{tmpSuffix, reserveSuffix, condemnedSuffix}` | `snapshot.go` |
| `maxLabelBytes` | `128` | `snapshot.go` |
| `copyContextChunk` | `32 << 20` (32 MiB) | `snapshot.go` |

### 13.4 On-disk layout

A snapshot is a **directory** directly under `snapshotsDir`, named `<unix-ns>` or `<unix-ns>-<label>`:
- Base component is `strconv.FormatInt(t.UnixNano(), 10)` — decimal nanoseconds since the Unix epoch, no padding (`snapshot.go`).
- `t` is `time.Now().UTC()` at reservation, possibly incremented by whole nanoseconds on collision (`snapshot.go`).
- Non-empty sanitized label → `base + "-" + clean` (`snapshot.go`).
- Contents are a tree copy of `databaseDir`'s contents, rooted at `databaseDir` itself, so `<snap>/<x>` corresponds to `<databaseDir>/<x>` (`snapshot.go`; asserted `snapshot_test.go`, round trip `snapshot_test.go`).

Sibling artifact names in the same directory:
- `<name>.reserve` — a directory created by `os.Mkdir(reservePath, 0o755)`, the atomic slot claim (`snapshot.go`).
- `<name>.tmp` — the in-flight clone destination (`snapshot.go`, written by `cloneTree` at `snapshot.go`).
- `<artifact>.<unix-ns>.condemned` — a corpse severed by the collector, minted as `fmt.Sprintf("%s.%d%s", path, time.Now().UnixNano(), condemnedSuffix)` (`residue.go`), so the full form is e.g. `1700000000000000000-label.tmp.1700000000000000005.condemned`.
- `.links-snapshot-producer.lock` — the flock beacon file (`snapshot.go`).

Restore-time artifact, created next to the database dir (**not** in `snapshotsDir`): `<databaseDir>.pre-restore-<unix-ns>` via `fmt.Sprintf("%s.pre-restore-%d", databaseDir, time.Now().UTC().UnixNano())` (`snapshot.go`).

Where `snapshotsDir` comes from: CLI uses `filepath.Join(ws.StorageDir, "snapshots")` (`/Users/bmf/code/links-issue-tracker/internal/cli/snapshots.go`); the store's migration path uses `filepath.Join(filepath.Dir(filepath.Clean(databaseDir)), "snapshots")` (`/Users/bmf/code/links-issue-tracker/internal/store/migrate_snapshot.go`).

### 13.5 Name grammar — mint side

`sanitizeLabel` (`snapshot.go`) is a lossy normalizer that never errors:
1. Each rune in `[a-z A-Z 0-9 _ -]` is written through; every other rune (multi-byte included) becomes a single `'-'` **byte**.
2. Truncated to the first `maxLabelBytes` = 128 **bytes**.
3. `strings.Trim(clean, "-")` strips leading and trailing dashes.
4. May be empty ⇒ `formatName` emits the bare timestamp.

`snapshot_test.go` asserts a 300-`x` label yields a name beginning `"1700000000000000001-xxx"`, that the worst derived form `len(name)+len(".reserve")+len(".1700000000000000001")+len(".condemned")` is ≤ 255 bytes, and that the truncated name still parses. `snapshot_test.go` asserts label `"pre-migration #5 / foo!"` yields a name containing `"-"` and an existing directory at `snap.Path`.

### 13.6 Name grammar — parse side

`parseName(name) (time.Time, bool)` (`snapshot.go`):
1. Reject immediately if `IsProducerArtifactName(name)`.
2. Split at the **first** `'-'` (`strings.IndexByte`): `head` before, `label` after, `dashed = true`.
3. `head` must satisfy `parsePositiveDigits`.
4. If dashed, `label` must satisfy `isMintableLabel`.
5. Return `time.Unix(0, ns).UTC()`.

`parsePositiveDigits(s)` (`snapshot.go`): `strconv.ParseInt(s, 10, 64)` must succeed, `ns > 0`, and `strconv.FormatInt(ns,10) == s` — the round-trip rejects sign prefixes and leading zeros that `ParseInt` tolerates. The comment records that `"+123.tmp"` was previously classified as lit-minted residue and destroyed.

`isMintableLabel(label)` (`snapshot.go`): rejects empty, rejects `label[0]=='-'`, rejects `label[len-1]=='-'`; every remaining **byte** must be in `[a-zA-Z0-9_-]`. **No length bound** — deliberately accepts labels minted by pre-cap binaries.

`IsProducerArtifactName(name)` (`snapshot.go`): true if `strings.HasSuffix(name, s)` for any of `.tmp`, `.reserve`, `.condemned`. This is the broad rejection predicate `parseName` uses.

`snapshot_test.go` pins parse results exactly: `1700000000000000000` true; `1700000000000000000-label` true; `1700000000000000000-pre-migration-foo` true; `snap-1700000000-abc` false; `abc-def` false; `""` false; `0` false; `+1700000000000000000` false; `01700000000000000000` false; `1700000000000000000-my.backup` false; `1700000000000000000-` false; `1700000000000000000--edges-` false; `1700000000000000000.tmp` false; `1700000000000000000-label.tmp` false; `1700000000000000000.reserve` false; `1700000000000000000-label.reserve` false; `1700000000000000000.tmp.condemned` false; `1700000000000000000.tmp.1755250000000000000.condemned` false. `snapshot_test.go` asserts `formatName`/`parseName` round-trip for `2026-05-14T12:00:00.123456789Z` with an empty label, compared via `parsed.Equal(created)`.

### 13.7 `Take` — full ordered behavior

`Take(ctx, databaseDir, snapshotsDir, label)` (`snapshot.go`):

1. **ctx gate.** `if ctxErr := ctx.Err(); ctxErr != nil { return Snapshot{}, ctxErr }` — the raw ctx error, unwrapped. Exists so Darwin's single-syscall `Clonefile` path cannot mint a snapshot a pre-canceled call should refuse. `snapshot_test.go` asserts `errors.Is(err, context.Canceled)` and that `snapshotsDir` was **not created**.
2. **Stat source.** `os.Stat(databaseDir)`; error → `"stat database dir: %w"`. `snapshot_test.go` asserts a missing source errors and leaves no `.tmp` entry.
3. `!info.IsDir()` → `"database dir is not a directory: %s"`.
4. `os.MkdirAll(snapshotsDir, 0o755)`; error → `"create snapshots dir: %w"`.
5. **Residue collection.** `CollectOrphanedResidue(snapshotsDir)`; on error prints to **stderr** `"lit: could not collect orphaned snapshot residue (the take proceeds; collection retries next take): %v\n"` and continues — never fails the take. Runs **before** the beacon is held, because holding it shared would make the collector's exclusive probe self-skip. Pinned by `residue_test.go`.
6. **Acquire beacon SHARED.** `filelock.Acquire(ctx, producerBeaconPath(snapshotsDir), false, 20, 50ms)`.
   - Real error → `"acquire snapshot producer beacon: %w"`.
   - Contention after 20 attempts → `"dbsnapshot: residue collection is holding the snapshots directory at %s and did not finish within its budget; retry: %w"` wrapping `ErrSnapshotsBusy`. Total wait bound ≈ 20 × 50 ms = 1s.
   - Held **shared**, so concurrent Takes coexist: `residue_test.go` asserts a Take succeeds while another shared holder exists and that residue is spared in that window.
7. **Deferred beacon release.** Release error → stderr `"lit: could not release snapshot producer beacon (residue collection defers until this process exits): %v\n"`, never converted into a returned error.
8. `reserveSnapshotPaths(snapshotsDir, label)`; error returned verbatim.
9. **Deferred `os.Remove(reserved.reservePath)`** — LIFO after the beacon-release defer, so the `.reserve` dir is removed *before* the beacon drops.
10. `cloneTree(ctx, databaseDir, reserved.tmpPath)`; on error `os.RemoveAll(reserved.tmpPath)` (error discarded) then `"clone tree: %w"`.
11. `os.Rename(reserved.tmpPath, reserved.finalPath)`; on error `os.RemoveAll(reserved.tmpPath)` (discarded) then `"rename tmp to final: %w"`.
12. Return `Snapshot{Path: reserved.finalPath, Name: reserved.name, Created: reserved.created}`. `Created` is the reservation candidate time (UTC), identical to the timestamp encoded in the name.

Documented (not enforced) preconditions in the package doc (`snapshot.go`): callers must not hold an open Dolt connection on the destination when calling `Restore`; `Take` on a live workspace requires the caller to hold the workspace SHARED lock, Dolt's journal lock, and the commit lock for the whole call. The package cannot import `store`, so these are documentation only.

### 13.8 `reserveSnapshotPaths` — slot reservation

`snapshot.go`: `candidate := time.Now().UTC()`; loop up to 1024; per attempt `name = formatName(candidate, label)`, `finalPath = snapshotsDir/name`, `reservePath = finalPath + ".reserve"`, `tmpPath = finalPath + ".tmp"`. `os.Mkdir(reservePath, 0o755)` is the atomic claim:
- **nil** → check `pathFree(finalPath)` and `pathFree(tmpPath)`. A stat error on either → `os.Remove(reservePath)` (discarded) and return the stat error. Either path occupied → `os.Remove(reservePath)`, `candidate += 1ns`, continue. Otherwise return the populated `reservedPaths`.
- **`fs.ErrExist`** → `candidate += 1ns`, continue.
- **any other error** → `"reserve %s: %w"`.
Exhaustion → `"dbsnapshot: no free snapshot name after 1024 attempts"`.

`pathFree(p)`: `os.Stat` nil → `(false,nil)`; `fs.ErrNotExist` → `(true,nil)`; other → `(false, "stat %s: %w")`.

The `.reserve` sentinel sits at a **sibling** path specifically so Darwin's `Clonefile` (which requires the destination not to exist) is unaffected.

Tests: `snapshot_test.go` (reserve, materialize `finalPath`, reserve again → different `finalPath`); `snapshot_test.go` (50 rapid consecutive Takes → 50 distinct names, `List` length 50); `snapshot_test.go` (20 concurrent goroutine Takes → no error, 20 distinct names, `List` length 20).

### 13.9 Copy engine, syscalls, exclusions, permissions

**`cloneTree` per platform.**
- **Darwin** (`clone_darwin.go`): `unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)` — a single APFS clonefile syscall over the whole tree. On nil, done. **Any** error falls back to `walkAndCopy(ctx, src, dst, plainFileCopy)`. The `Clonefile` call is uncancelable; `ctx` governs only the fallback walk.
- **Linux** (`clone_linux.go`): always `walkAndCopy(ctx, src, dst, ficloneOrCopy)` — per-file `FICLONE`.
- **Other** (`clone_other.go`): `walkAndCopy(ctx, src, dst, plainFileCopy)`.

**`walkAndCopy`** (`snapshot.go`) — `filepath.WalkDir(src...)`, per entry:
1. A walk error is returned as-is.
2. `ctx.Err()` is checked **per entry** and returned if non-nil.
3. `rel = filepath.Rel(src, srcPath)`, error returned; `dstPath = filepath.Join(dst, rel)`.
4. **Directory**: `d.Info()` (error returned), `os.MkdirAll(dstPath, info.Mode().Perm())`, then `os.Chmod(dstPath, info.Mode().Perm())` to defeat umask filtering.
5. **Symlink** (`d.Type()&os.ModeSymlink != 0`): `os.Readlink` then `os.Symlink` — the link is recreated, **never followed**.
6. **Regular file**: if `isDoltJournalLockRel(rel)` → `return nil` (skip); else `copyFile(ctx, srcPath, dstPath)`.
7. **Anything else** (FIFOs, sockets, devices) → `"dbsnapshot: unsupported file type at %s: %v"`.

**The one exclusion — Dolt's journal LOCK.** `isDoltJournalLockRel(rel)` returns `strings.HasSuffix(filepath.ToSlash(rel), "/.dolt/noms/LOCK")` (`snapshot.go`). Because the check requires a leading `/`, it matches `<anything>/.dolt/noms/LOCK` — a database subdirectory under the copied root — and would **not** match a `rel` of exactly `.dolt/noms/LOCK` at the copy root. Rationale: the file is contentless, Dolt recreates it at every engine open, and on Windows the mandatory `LockFileEx` hold means reading it through a second handle would fail the copy or drop the hold. It is explicitly noted that **Darwin's `Clonefile` fast path may still carry the file**. `dolt_lock_skip_test.go` builds `src/links/.dolt/noms/LOCK` (0600) plus a sibling `manifest` (0644), runs `walkAndCopy(..., plainFileCopy)`, and asserts `LOCK` is absent while `manifest` is present. No other exclusion exists; the beacon lives in `snapshotsDir`, not `databaseDir`, so it is never part of a copy.

**`plainFileCopy`** (`snapshot.go`): `os.Open(src)` + `defer srcF.Close()`; `srcF.Stat()`; `os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())` — **`O_EXCL`**, so an existing destination fails; `copyWithContext` with `_ = dstF.Close()` on error; `dstF.Chmod(info.Mode().Perm())` to force exact source perms past umask, closing on error; `return dstF.Close()` — the close error is part of the copy's outcome, deliberately **not** deferred, because a failed write-back-at-close (NFS commit-on-close, delayed-allocation ENOSPC) is the only signal the file is truncated. Ownership and timestamps are **not** propagated — only permission bits.

**`copyWithContext`** (`snapshot.go`): infinite loop checking `ctx.Err()` then `io.CopyN(dst, src, 32<<20)`. `io.EOF` ends with nil; any other error is returned. `io.CopyN` is used specifically so `*os.File.ReadFrom` unwraps the `LimitedReader` and keeps kernel fast paths (`copy_file_range`/`sendfile`) live.

**`ficloneOrCopy`** (Linux, `clone_linux.go`): `os.Open(src)` + `defer Close` + `Stat`; `os.OpenFile(dst, O_WRONLY|O_CREATE|O_EXCL, perm)`; `unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd()))` — the `FICLONE` ioctl. On success: `Chmod(perm)` (close+return on error) then `return dstF.Close()`. On FICLONE failure: `copyWithContext`, then `Chmod`, then `Close`, each closing and returning on error.

**Permission evidence**: `snapshot_test.go` (a 0640 source produces a 0640 destination); `snapshot_unix_test.go` (with the process umask forced to `0o077`, `cloneTree` produces `dst/nested` at `0755` and `dst/nested/f` at `0644` — umask must not affect the snapshot; not parallel because `syscall.Umask` is process-wide; unix-only because `syscall.Umask` is undefined on Windows).

**Cancellation evidence**: `residue_test.go` — `walkAndCopy` over `src/a` and `src/z` where the copy callback cancels after the first file; asserts `errors.Is(err, context.Canceled)`, exactly 1 file copied (WalkDir order is lexical), and `dst/z` absent.

**Unsupported-entry-type evidence**: `snapshot_unix_test.go` — source has a regular file plus a FIFO from `syscall.Mkfifo(..., 0o644)`. The test tolerates either outcome (Darwin's `Clonefile` clones FIFOs wholesale and the Take succeeds, logged; walk-based paths refuse) and asserts only the residue contract: no entry in `snapshotsDir` satisfies `IsProducerArtifactName`.

### 13.10 `List`

`snapshot.go`: `os.ReadDir(snapshotsDir)`; `fs.ErrNotExist` → `([]Snapshot{}, nil)` (empty, non-nil slice); any other error → `"read snapshots dir: %w"`. Skips **non-directory** entries — so the beacon file and any regular file is skipped. Skips entries where `parseName(name)` is not ok. Builds `Snapshot{Path: filepath.Join(snapshotsDir, name), Name: name, Created: created}`. Sorts **newest-first** by `Created.After` via non-stable `sort.Slice`.

Tests: `snapshot_test.go` (3 takes, newest-first); `snapshot_test.go` (`snap-old-junk` dir, `backup_2024` dir, `README.txt` file → 0 results); `snapshot_test.go` (`1700000000000000000.tmp` and `.reserve` dirs → 0 results).

### 13.11 `Restore`

`snapshot.go`, in order:
1. `validateSnapshotName(name)`; error returned.
2. `snapshotPath = filepath.Join(snapshotsDir, name)`.
3. **`os.Lstat`**, not `Stat` — a symlink is not followed. `fs.ErrNotExist` → **bare `ErrSnapshotMissing`**, not wrapped; other error → `"stat snapshot: %w"`.
4. `!info.Mode().IsDir()` → `"snapshot is not a directory: %s"` — this is what rejects a symlink.
5. **Rotate the existing database dir.** `os.Stat(databaseDir)`:
   - exists → `rotatedPath = fmt.Sprintf("%s.pre-restore-%d", databaseDir, time.Now().UTC().UnixNano())`, then `os.Rename(databaseDir, rotatedPath)`; rename error → `("", "rotate existing database dir: %w")`.
   - `fs.ErrNotExist` → no rotation, `rotatedPath` stays `""`.
   - other stat error → `("", "stat database dir: %w")`.
6. `os.Rename(snapshotPath, databaseDir)` — this **moves** the snapshot directory; **the snapshot no longer exists in `snapshotsDir` afterwards**. Error → `(rotatedPath, "install snapshot at database dir: %w")` — note the rotated path *is* returned alongside the error so the caller can undo.
7. Return `(rotatedPath, nil)`.

`validateSnapshotName` (`snapshot.go`): empty → `"dbsnapshot: snapshot name is empty"`; `name != filepath.Base(name)` → `"dbsnapshot: snapshot name must be a single path component: %q"`; `parseName` not ok → `"dbsnapshot: snapshot name does not match the <unix-ns>[-<label>] scheme: %q"`.

Tests: `snapshot_test.go` (round trip — source mutated after the snapshot, subtree deleted, file rewritten; after restore `top.txt`=="top", `sub/deep.txt`=="deep", the rotated dir holds the mutated `top.txt`=="MUTATED", rotated path non-empty); `snapshot_test.go` (source removed before restore ⇒ rotated == `""`, content restored); `snapshot_test.go` (all of `""`, `"."`, `".."`, `"../sibling"`, `"sub/dir"`, `"/etc/passwd"`, `"1700000000-../etc"`, `"snap-1700000000-abc"`, `"1700000000000000000.tmp"`, `"1700000000000000000-label.tmp"`, `"trailing/"` are rejected); `snapshot_test.go` (a symlink named `1700000000000000000` inside `snapshotsDir` pointing outside is refused; the database dir still exists — no rotation happened — and the symlink is still present, not moved); `snapshot_test.go` (nonexistent snapshots dir + canonical name → `errors.Is(err, ErrSnapshotMissing)`).

### 13.12 `Prune` / `PruneMatching`

`Prune(snapshotsDir, keep)` = `PruneMatching(snapshotsDir, keep, nil)` (`snapshot.go`).

`PruneMatching` (`snapshot.go`):
1. `keep <= 0` → `"dbsnapshot: keep must be > 0"`.
2. `List(snapshotsDir)`; error propagated verbatim.
3. `match == nil` ⇒ every snapshot matches; otherwise filter by `match(s.Name)` **preserving newest-first order**.
4. `len(matched) <= keep` → nil, nothing removed.
5. For each `matched[keep:]` (the oldest), `os.RemoveAll(snapshot.Path)`; the first error → `"remove snapshot %s: %w"`, aborting the rest.

Non-matching snapshots are never removed regardless of age. Residue collection deliberately does **not** run here.

Tests: `snapshot_test.go` (7 takes, keep=3 ⇒ `List` len 3 and on-disk non-`.tmp` dir count 3); `snapshot_test.go` (keep=0 and keep=-1 both error); `snapshot_test.go` (empty dir, keep=5, no error); `snapshot_test.go` (6 `kind-a` + 5 `kind-b` takes, `PruneMatching(dir, 2, contains "-kind-a")` ⇒ 2 matching remain, all 5 non-matching untouched); `snapshot_test.go` (`PruneMatching(..., 2, nil)` over 5 → 2 remain).

This is the mechanism behind the three disjoint store-side retention budgets (`migrationSnapshotRetention=10`, `reconcileSnapshotRetention=10`, and the downgrade budget) described in §5.4 and §9.

### 13.13 Residue: what counts, what is destroyed, what is spared

**Destruction predicates.** `isCollectibleResidue(name)` (`snapshot.go`) = `isCollectorCondemnedName(name) || isCollectibleArtifactName(name)`.

`isCollectibleArtifactName(name)` (`snapshot.go`): for suffix in `[".tmp", ".reserve"]` — note **not** `.condemned` — if `strings.CutSuffix` matches, return whether the remaining head satisfies `parseName`; returns on the first matching suffix. Otherwise false.

`isCollectorCondemnedName(name)` (`snapshot.go`): strip `".condemned"` (must be present); find `strings.LastIndexByte(head, '.')` (must exist); the segment after that dot must satisfy `parsePositiveDigits`; and `head[:dot]` must satisfy `isCollectibleArtifactName`. So the **only** collectible condemned shape is `<unix-ns>[-<label>].{tmp|reserve}.<positive-ns>.condemned`.

The design rule stated in-code (`snapshot.go`): **reject broadly (`IsProducerArtifactName`), delete narrowly (`isCollectibleResidue`)** — a foreign `backup.tmp` or `backup.condemned` an operator parked in the directory is untouchable, because a suffix is never provenance over a directory lit does not own.

**`CollectOrphanedResidue`** (`residue.go`):
1. `os.Stat(snapshotsDir)`: `fs.ErrNotExist` → `nil` (no-op; probing the beacon would otherwise create the directory as a side effect); other error → `"stat snapshots dir: %w"`.
2. `filelock.Acquire(context.Background(), producerBeaconPath(snapshotsDir), true /*exclusive*/, 1 /*attempt*/, 0 /*delay*/)` — a single non-sleeping probe.
   - Real error → `"probe snapshot producer beacon: %w"`.
   - **Contention → `return nil`, silently skipping collection**. Rationale: a live producer exists; its own later take collects.
3. `condemned, condemnErr := condemnResidue(snapshotsDir)`.
4. `release()`; if `relErr != nil || condemnErr != nil` → `errors.Join(condemnErr, relErr)` and **the RemoveAll pass is skipped entirely**.
5. The beacon is released **before** the deletion pass so a multi-gigabyte corpse delete does not extend the window producers retry against.
6. Per condemned path: `os.RemoveAll(path)`; failures collected as `"remove condemned residue %s: %w"`; returns `errors.Join(removeErrs...)`.

**Liveness discriminator**: no age thresholds, no PID files — every live `Take` holds the beacon shared for its whole reserve→copy→rename window, so an exclusive acquire proves all producer artifacts present are orphaned (`residue.go`). Documented caveat: a still-running **pre-beacon** binary's Take holds no beacon and reads as dead.

**`condemnResidue`** (`residue.go`): `os.ReadDir(snapshotsDir)`, error → `"read snapshots dir: %w"`; for every entry — **directories and regular files alike, no `IsDir` filter** — skip unless `isCollectibleResidue(name)`; if the name does not already end in `.condemned`, rename to `fmt.Sprintf("%s.%d%s", path, time.Now().UnixNano(), condemnedSuffix)`, with a rename error aborting the whole classification as `"condemn residue %s: %w"`; already-`.condemned` entries (which reached here only by passing `isCollectorCondemnedName`) are added as-is. The fresh nanosecond stamp guarantees a rename can never collide with a corpse from an earlier interrupted collection, even if a producer reuses the exact original stamp.

**Residue tests.**
- `residue_test.go` `fabricateDeadResidue` plants `<stamp>.tmp/nested/partial` (0644 file, 0755 dirs) plus a `<stamp>.reserve` dir — the exact shape a killed Take leaves.
- `residue_test.go`: after a real Take (label `keep-me`), plus fabricated residue at `1700000000000000001`, plus a leftover `1700000000000000002.tmp.1700000000000000005.condemned`, plus `snap-old-junk`. Collection removes every `IsProducerArtifactName` entry; the real snapshot and the legacy directory survive.
- `residue_test.go`: with a simulated live producer holding the beacon shared, collection returns nil and leaves `.tmp` and `.reserve` untouched; after `release()`, the same call removes both.
- `residue_test.go`: a never-created directory is a no-op, returns nil, and collection must **not** create the directory.
- `residue_test.go`: `Take` collects residue at entry and still succeeds.
- `residue_test.go` — the exact **spared** set: `backup.tmp`, `backup.condemned`, `backup.tmp.1700000000000000001.condemned`, `1700000000000000006.tmp.condemned` (stampless), `+1700000000000000006.tmp` (signed head), `1700000000000000006.tmp.+1.condemned` (signed collector stamp), `1700000000000000006-my.backup.tmp` (dotted label), plus a foreign **regular file** `notes.reserve`. Simultaneously **collected**: `1700000000000000006.tmp`/`.reserve` and `1700000000000000007.reserve.1700000000000000002.condemned`.
- `residue_test.go`: residue whose label is 300 `x` (truncated by `formatName`) is still condemnable and collected — the condemnation rename stays inside NAME_MAX.
- `residue_test.go`: stamp reuse — a fresh `<stamp>.tmp` plus an old `<stamp>.tmp.1700000000000000001.condemned` are both removed by one collection.
- `residue_test.go`: with `nested` chmod'd to `0555` so the corpse cannot be removed, `Take` still succeeds and the snapshot exists (skipped when `os.Getuid() == 0`).
- `residue_test.go`: the same unremovable-corpse setup makes `CollectOrphanedResidue` return a non-nil error whose text contains `"condemned"`; after fixing permissions, the next collection succeeds (convergence). Also skipped as root.
- `residue_test.go` `restoreCondemnedPerms` re-chmods `*.condemned/nested` and `*.tmp/nested` back to 0755 for TempDir cleanup.

### 13.14 Complete error/abort catalog for `dbsnapshot`

| Site | Condition | Result |
|---|---|---|
| `snapshot.go` | ctx already canceled/expired | raw `ctx.Err()` |
| `snapshot.go` | `os.Stat(databaseDir)` fails | `stat database dir: %w` |
| `snapshot.go` | source not a directory | `database dir is not a directory: %s` |
| `snapshot.go` | `MkdirAll(snapshotsDir,0755)` fails | `create snapshots dir: %w` |
| `snapshot.go` | collection error | stderr only; Take proceeds |
| `snapshot.go` | beacon acquire real error | `acquire snapshot producer beacon: %w` |
| `snapshot.go` | beacon contention after 20×50ms | wraps `ErrSnapshotsBusy` |
| `snapshot.go` | beacon release fails | stderr only |
| `snapshot.go/215` | stat of final/tmp path fails | `stat %s: %w`; reservation aborts, `.reserve` removed |
| `snapshot.go` | `Mkdir(.reserve)` non-EEXIST error | `reserve %s: %w` |
| `snapshot.go` | 1024 attempts exhausted | `dbsnapshot: no free snapshot name after 1024 attempts` |
| `snapshot.go` | `cloneTree` fails | `.tmp` RemoveAll'd; `clone tree: %w` |
| `snapshot.go` | rename `.tmp`→final fails | `.tmp` RemoveAll'd; `rename tmp to final: %w` |
| `snapshot.go` | `ReadDir` fails (not ENOENT) | `read snapshots dir: %w` |
| `snapshot.go` | empty restore name | `dbsnapshot: snapshot name is empty` |
| `snapshot.go` | multi-component restore name | `dbsnapshot: snapshot name must be a single path component: %q` |
| `snapshot.go` | name fails `parseName` | `dbsnapshot: snapshot name does not match the <unix-ns>[-<label>] scheme: %q` |
| `snapshot.go` | snapshot path absent | `ErrSnapshotMissing` (bare) |
| `snapshot.go` | Lstat other error | `stat snapshot: %w` |
| `snapshot.go` | snapshot path not a dir (incl. symlink) | `snapshot is not a directory: %s` |
| `snapshot.go` | rotate rename fails | `rotate existing database dir: %w`, rotated returned as `""` |
| `snapshot.go` | stat databaseDir other error | `stat database dir: %w` |
| `snapshot.go` | install rename fails | `(rotatedPath, install snapshot at database dir: %w)` |
| `snapshot.go` | `keep <= 0` | `dbsnapshot: keep must be > 0` |
| `snapshot.go` | `RemoveAll` of a snapshot fails | `remove snapshot %s: %w` (aborts loop) |
| `snapshot.go` | unsupported file type in walk | `dbsnapshot: unsupported file type at %s: %v` |
| `residue.go` | stat snapshotsDir other error | `stat snapshots dir: %w` |
| `residue.go` | beacon probe real error | `probe snapshot producer beacon: %w` |
| `residue.go` | beacon contention | `nil` (silent skip) |
| `residue.go` | condemn or release error | `errors.Join(condemnErr, relErr)`; delete pass skipped |
| `residue.go` | ReadDir fails | `read snapshots dir: %w` |
| `residue.go` | condemnation rename fails | `condemn residue %s: %w` (aborts classification) |
| `residue.go` | RemoveAll of a corpse fails | joined `remove condemned residue %s: %w` |
