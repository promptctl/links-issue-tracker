# Behavioral inventory: sync / merge / migration / compaction / backup machinery

Repo: `/Users/bmf/code/links-issue-tracker`. Derived entirely from Go/SQL source and `_test.go` files. Every claim carries a `file:line` citation. Paths are absolute.

---

## PART 1 — Sync entry points (`internal/store/sync.go`)

### 1.1 `OpenSync` — the sync-capable store open

`OpenSync(ctx, doltRootDir, workspaceID) (*Store, error)` at `/Users/bmf/code/links-issue-tracker/internal/store/sync.go:20`. Exact sequence:

1. `validateOpenArgs(doltRootDir, workspaceID)` — shared argument-validation boundary (`sync.go:25`).
2. `requireEmbeddedSyncSupport()` (`sync.go:28`) — version floor check, see §1.2.
3. `acquireWorkspaceShared(ctx, doltRootDir)` (`sync.go:35`) — workspace shared lock acquired **before** database bootstrap. On any later failure the release is invoked and its error joined onto the returned error (`sync.go:40-47`).
4. `requireNoPendingAdopt(doltRootDir)` (`sync.go:53`) — refuses if an adopt marker is present while the workspace lock is held.
5. `ensureDoltDatabase(ctx, doltRootDir, workspaceID)` (`sync.go:57`) — same initializer `Store.Open` uses.
6. `openStoreConnection(ctx, doltRootDir, workspaceID, engineWrite)` (`sync.go:65`) — eager write engine open; waits on Dolt's journal lock bounded by `engineOpenRetryMaxElapsed`.
7. `s.releaseWorkspaceLock = release` (`sync.go:69`).
8. Branch normalization: `masterRenameSource(ctx, s.db)` is read lock-free; only when it returns a non-empty source is `ensureMasterDefaultBranch` run inside `s.withCommitLock` (`sync.go:82-87`). A read-only OpenSync therefore takes no commit lock.
9. On error in step 8: `wrapEngineOpenContention(err)`, then `s.db.Close()` whose error is joined unless it is `context.Canceled`; `s.releaseWorkspaceLock` is set to nil (`sync.go:88-95`).

`engineOpenRetryMaxElapsed` is `30 * time.Second`, a package var (`/Users/bmf/code/links-issue-tracker/internal/store/store.go:2607`), assigned to `bo.MaxElapsedTime` at `store.go:2633`.

### 1.2 Embedded-dependency version floor

Constants at `/Users/bmf/code/links-issue-tracker/internal/store/sync.go:17-18`:
- `minEmbeddedDoltVersion = "v0.40.5-0.20260314011441-62975ef6bf36"`
- `minEmbeddedDriverVersion = "v0.2.1-0.20260314000741-0fe74e7ee31a"`

`requireEmbeddedSyncSupport` (`sync.go:881`) reads `debug.ReadBuildInfo()` via `readEmbeddedModuleVersions` (`sync.go:889`); if build info is absent or the dep map is empty it returns `nil` (no check). `validateEmbeddedSyncSupport` (`sync.go:901`) maps `github.com/dolthub/dolt/go` → min dolt version and `github.com/dolthub/driver` → min driver version (`sync.go:902-905`). A module absent from the map (empty version string) is **skipped** (`sync.go:908-910`). Otherwise `semver.Compare(actual, minimum) < 0` returns error `"embedded sync requires %s %s or newer (found %s)"` (`sync.go:911-918`). Tests: `TestValidateEmbeddedSyncSupportAcceptsRequiredVersions` (`sync_test.go:506`), `TestValidateEmbeddedSyncSupportRejectsOlderVersions` (`sync_test.go:517`).

### 1.3 Remote management

- `SyncListRemotes` (`sync.go:100`): `SELECT name, url FROM dolt_remotes ORDER BY name`. Returns `[]storage.SyncRemote` (never nil — initialized to `[]` at `sync.go:107`). Errors: `"list dolt remotes: %w"`, `"scan dolt remote: %w"`, `"iterate dolt remotes: %w"`.
- `SyncAddRemote(ctx, name, url)` (`sync.go:150`): trims/requires both args via `requireSyncArg` (`sync.go:152,156`), then under `runSyncMutation` calls `CALL DOLT_REMOTE(?, ?, ?)` with `"add", name, url` (`sync.go:161`). Error: `"add dolt remote %q: %w"`.
- `SyncRemoveRemote(ctx, name)` (`sync.go:169`): `CALL DOLT_REMOTE(?, ?)` with `"remove", name` (`sync.go:175`). Error `"remove dolt remote %q: %w"`.
- `SyncFetch(ctx, remote, prune)` (`sync.go:183`): args are `[remote]`, and when `prune` is true `"--prune"` is **prepended** (`sync.go:188-191`), then `CALL DOLT_FETCH(...)`. Error `"fetch remote %q: %w"`.
- Test: `TestSyncRemoteLifecycle` (`sync_test.go:212`), `TestSyncRemoteValidation` (`sync_test.go:259`).

`requireSyncArg(field, value)` (`sync.go:873`) trims whitespace and errors `"%s is required"` when the trimmed value is empty.

### 1.4 `GitBackedRemoteURL` — git remote → Dolt transport URL

`GitBackedRemoteURL(raw string) string` at `sync.go:136`:
1. Trim; empty input returns `""` (`sync.go:137-140`).
2. Try `doltenv.NormalizeGitRemoteUrl(trimmed)`; on `ok && err == nil` return the normalized value (`sync.go:141-143`).
3. Otherwise append a synthetic `".git"`, re-run the normalizer, and on success return the result with a trailing `".git"` stripped via `strings.TrimSuffix` (`sync.go:144-146`).
4. Otherwise return the trimmed input unchanged (`sync.go:147`).

Tests: `TestGitBackedRemoteURL` (`sync_test.go:1115`), `TestGitBackedRemoteURLIsIdempotent` (`sync_test.go:1144`), `TestGitBackedRemoteURLRoundTripsThroughDolt` (`sync_test.go:1164`).

### 1.5 Freshness (`SyncFreshness`) — the divergence classifier

`SyncFreshness(ctx, remote, branch) (storage.SyncFreshness, error)` at `sync.go:699`. Pure local read; never touches the network.

1. Both args required (`sync.go:700-707`).
2. Tracking ref string is `fmt.Sprintf("remotes/%s/%s", remote, branch)` (`sync.go:709`).
3. Existence probe: `SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?` (`sync.go:712-714`). Count 0 → return with `Synced=false`, `Ahead=0`, `Behind=0`, `OldestDivergedUnix=0` — the never-synced state; the range queries are **not** run (`sync.go:717-723`).
4. `Synced = true`; read `SELECT ACTIVE_BRANCH()` (`sync.go:727`).
5. `ahead = commitRangeStats(trackingRef, localBranch)`; `behind = commitRangeStats(localBranch, trackingRef)` (`sync.go:731-738`). Errors wrapped `"summarize commits ahead of %q: %w"` / `"summarize commits behind %q: %w"`.
6. `OldestDivergedUnix` is populated **only** when `ahead > 0 && behind > 0`, as `earlierValidUnix(aheadOldest, behindOldest)` (`sync.go:748-750`). Ahead-only or behind-only leaves it 0.

`commitRangeStats(from, to)` (`sync.go:784`) runs `SELECT COUNT(*), UNIX_TIMESTAMP(MIN(date)) FROM dolt_log(?)` with the bound range expression `"<from>..<to>"` (`sync.go:787-789`). The timestamp is scanned as `sql.NullString` because the driver renders `UNIX_TIMESTAMP` of a Datetime3 column as a fractional decimal string (`sync.go:775-783`), then parsed by `parseUnixSeconds`.

`parseUnixSeconds(raw sql.NullString) (sql.NullInt64, error)` (`sync.go:806`): invalid/blank → `{}, nil`; `strconv.ParseFloat` failure → error (wrapped by caller as `"parse oldest commit time %q: %w"`, `sync.go:795`); otherwise `int64(secs)` — sub-second truncated. Test `TestParseUnixSeconds` (`sync_helpers_test.go:13`).

`earlierValidUnix(a, b sql.NullInt64) int64` (`sync.go:756`): both valid → min; one valid → that one; neither → 0. Test `TestEarlierValidUnix` (`sync_helpers_test.go:51`).

**State mapping** — `storage.SyncFreshness.State()` at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:95`:
- `!Synced` → `SyncNeverSynced` (`"never_synced"`)
- `Ahead==0 && Behind==0` → `SyncUpToDate` (`"up_to_date"`)
- `Behind==0` → `SyncAhead` (`"ahead"`)
- `Ahead==0` → `SyncBehind` (`"behind"`)
- else → `SyncDiverged` (`"diverged"`)

Constants at `storage/sync.go:59-65`. Test `TestSyncFreshnessStateClassification` (`sync_test.go:531`); `TestSyncFreshnessTracksAheadBehindAgainstRemote` (`sync_test.go:574`); `TestSyncFreshnessRequiresRemoteAndBranch` (`sync_test.go:553`).

### 1.6 `SyncReceive` — fetch + fast-forward only

`SyncReceive(ctx, remote, branch)` at `sync.go:394`. Runs inside `runSyncMutation` (commit lock + GC retry):
1. `CALL DOLT_FETCH(?)` with the remote (`sync.go:406`). Error `"fetch remote %q: %w"`.
2. One `SyncFreshness` read (`sync.go:409`). `Ahead`, `Behind`, `OldestDivergedUnix` are copied onto the result (`sync.go:413-414`).
3. Switch on `fresh.State()` (`sync.go:415-430`):
   - `SyncBehind` → `execProcedureDiscard(DOLT_MERGE, "--ff-only", "remotes/<remote>/<branch>")`; error `"fast-forward to %q: %w"`; state `SyncReceiveFastForwarded`.
   - `SyncDiverged` → state `SyncReceiveDiverged`, **no merge performed**.
   - `SyncAhead` → `SyncReceiveAhead`.
   - `SyncNeverSynced` → `SyncReceiveNeverSynced`.
   - default → `SyncReceiveUpToDate`.

Fast-forward is the only local-data-touching outcome. State constants and their string values at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:116-133`: `"up_to_date"`, `"fast_forwarded"`, `"ahead"`, `"diverged"`, `"never_synced"`. Test: `TestSyncReceiveFastForwardsWhenBehindAndDefersDivergence` (`sync_test.go:1214`).

### 1.7 `SyncPull` — receive, then reconcile only on divergence

`SyncPull(ctx, remote, branch) (storage.SyncPullResult, error)` at `sync.go:239`. The whole converge runs under **one** `s.withCommitLock` (`sync.go:241`); nested acquisitions short-circuit because `acquireCommitLock` is context-reentrant (`commit_lock.go:357-366`).

Sequence (`sync.go:242-312`):
1. `s.SyncReceive`. Copy `Ahead`, `Behind`, `OldestDivergedUnix`.
2. Map receive state → pull state:
   - `SyncReceiveUpToDate` → `SyncPullUpToDate`
   - `SyncReceiveFastForwarded` → `SyncPullFastForwarded`
   - `SyncReceiveAhead` → `SyncPullAhead`
   - `SyncReceiveNeverSynced` → `SyncPullNeverSynced`
   - `SyncReceiveDiverged` → run `s.SyncReconcile(ctx, remote, branch)` and map its state:
     - `SyncReconcileLinearized` → `SyncPullLinearized`, then **re-read** `SyncFreshness` and overwrite `Ahead`, `Behind`, `OldestDivergedUnix` from it (`sync.go:264-279`).
     - `SyncReconcileProsePending` → `SyncPullProsePending`, `result.Pending = rec.Pending` (`sync.go:280-282`).
     - `SyncReconcileUnrelated` → `SyncPullUnrelated`, `result.Unrelated = rec.Unrelated`; the receive's ahead/behind and fork timestamp ride along unchanged (`sync.go:283-291`).
     - `SyncReconcileNotDiverged` → `SyncPullUpToDate` **and** `result.OldestDivergedUnix = 0` (`sync.go:292-300`).
     - any other reconcile state → error `"sync pull: unhandled reconcile state %q"` (`sync.go:301-305`).
   - any other receive state → error `"sync pull: unhandled receive state %q"` (`sync.go:307-310`).
3. On any error the zero `SyncPullResult` is returned (`sync.go:314-316`).

`SyncPull` deliberately does not call Dolt's native `DOLT_PULL`; the documented reason (`sync.go:206-213`) is that native pull's three-way working-set merge requires `autocommit` off and aborts with `"@autocommit must be disabled so that merge conflicts can be resolved"` under the driver's default.

Pull state constants at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:153-178`: `"up_to_date"`, `"fast_forwarded"`, `"linearized"`, `"prose_pending"`, `"unrelated_histories"`, `"ahead"`, `"never_synced"`. `SyncPullResult` fields and JSON tags at `storage/sync.go:183-197`.

Tests: `TestSyncPullStateTransitions` (`sync_reconcile_schema_skew_test.go:255`), `TestSyncPullHealsSchemaSkewDivergence` (`sync_reconcile_schema_skew_test.go:198`).

### 1.8 `SyncPush` and `SyncCompactAndPush`

`SyncPush(ctx, remote, branch, setUpstream, force)` (`sync.go:543`) — runs `pushWithinLock` inside `runSyncMutation`. **No compaction.** Rationale in the source: `DOLT_GC` transitions the embedded store read-only mid-run (`sync.go:537-542`).

`SyncCompactAndPush(...)` (`sync.go:562`) — inside one `runSyncMutation`:
1. `depth, depthErr = s.chooseCompactionDepth()` measured **inside** the lock (`sync.go:569`).
2. `s.compactWithinLock(ctx, depth)` (`sync.go:570`).
3. `s.pushWithinLock(...)` (`sync.go:573`).
Then, **after** the push and **outside** the commit lock (`sync.go:602-605`):
`result.Maintenance = joinMaintenance(compactionReport(depth, depthErr), s.pruneRemoteCache(ctx).Report())`. A prune failure never fails the push (`sync.go:596-599`).

`pushWithinLock` (`sync.go:613`):
1. `requireSyncArg("remote", remote)`; branch is only `strings.TrimSpace`d and **may be empty** (`sync.go:614-618`).
2. If branch is non-empty, `s.guardRemoteSchemaAhead(ctx, remote, branch)` runs first (`sync.go:626-630`). An empty branch skips the guard entirely.
3. Args built in order: `"--set-upstream"` if `setUpstream`, `"--force"` if `force`, then the remote, then `fmt.Sprintf("HEAD:%s", branch)` if branch non-empty (`sync.go:631-641`).
4. `CALL DOLT_PUSH(...)` scanned into `(result.Status int64, message sql.NullString)` (`sync.go:642-647`). Error `"push remote %q: %w"`.
5. `result.Message = nullStringValue(message)` — NULL→`""`, otherwise trimmed (`sync.go:648`, `sync.go:866-871`).

Tests: `TestSyncPushDelivers` (`sync_test.go:689`), `TestSyncCompactAndPushDelivers` (`sync_test.go:763`), `TestSyncCompactAndPushDeepensOnAFragmentedOldGeneration` (`sync_test.go:816`).

### 1.9 `SyncStatus`

`SyncStatus(ctx)` (`sync.go:652`) runs, in order:
- `SELECT DOLT_VERSION()` → `report.DoltVersion`; error `"read dolt version: %w"`.
- `SELECT ACTIVE_BRANCH()` → `report.Branch`; error `"read active branch: %w"`.
- `SELECT commit_hash, message FROM dolt_log() LIMIT 1` → `HeadCommit`, `HeadMessage` (NULL→`""` trimmed); error `"read head commit: %w"`.
- `SyncListRemotes`.
- `SELECT table_name, staged, status FROM dolt_status ORDER BY table_name, staged` → `[]SyncStatusRow` (initialized to `[]`, `sync.go:678`). Errors `"read dolt status: %w"`, `"scan dolt status row: %w"`, `"iterate dolt status rows: %w"`.

Report type at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:43-50`.

### 1.10 Reset / adopt primitives

- `LocalIssueCount(ctx)` (`sync.go:328`): first `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'issues'`; count 0 → returns `0, nil` (no error for a pristine store). Otherwise `SELECT COUNT(*) FROM issues`. Test `TestLocalIssueCountAcrossLifecycle` (`sync_test.go:1061`).
- `SyncResetToRemoteHead(ctx, remote, branch)` (`sync.go:355`): both args required; builds `remotes/<remote>/<branch>`; runs `resetHardToRef` under `runSyncMutation`. Destructive of local commits by design (`sync.go:345-354`). Tests `TestSyncResetToRemoteHeadAdoptsUnrelatedHistory` (`sync_test.go:982`), `TestSyncResetToRemoteHeadRequiresRemoteAndBranch` (`sync_test.go:1098`).
- `resetHardToRef(ctx, db, ref)` (`sync.go:376`): `CALL DOLT_RESET('--hard', ref)`; error `"reset to remote head %q: %w"`. Shared by the init-adopt and the take-remote resolution.

### 1.11 Procedure-call plumbing

- `callIntProcedure(ctx, db, procedure, args...)` (`sync.go:823`) — scans a single int64 status column.
- `execProcedureDiscard(ctx, db, procedure, args...)` (`sync.go:838`) — drains all rows and returns `rows.Err()`; column-count agnostic, used for `DOLT_MERGE`, `DOLT_CHECKOUT`, `DOLT_BRANCH`.
- `buildProcedureCall(procedure, argCount)` (`sync.go:850`) — `"CALL P()"` for zero args, else `"CALL P(?,?,…)"`.
- `stringArgsToAny` (`sync.go:858`), `nullStringValue` (`sync.go:866`).
- `runSyncMutation` (`sync.go:817`) = `withCommitLock` → `retryTransientGCContention(op, s.reconnect, transientRetryDelay, waitWithContext)`.

---

## PART 2 — Commit lock, transient-GC retry, and the commit boundary (`internal/store/commit_lock.go`)

### 2.1 Lock mechanics

- The commit lock is an **flock** via `acquireStoreLock` (`commit_lock.go:22-27`, `commit_lock.go:417`). Death of the holder releases it; there is no staleness/eviction heuristic.
- Path: `commitLockPathForDolt` (`commit_lock.go:394`) = `filepath.Join(filepath.Dir(filepath.Clean(databasePath)), ".links-commit-flock.lock")`. The historical name `.links-commit.lock` is deliberately not used because O_EXCL-era binaries unlink it, splitting the lock across inodes (`commit_lock.go:396-402`). Exported as `CommitLockPath` (`commit_lock.go:390`).
- Re-entrancy: `acquireCommitLock` (`commit_lock.go:357`) checks `ctx.Value(commitLockContextKey{})`; if already true it returns a no-op release. Otherwise it acquires and returns a ctx with the marker set.
- Budget: `commitLockRetryAttempts = 9000`, `commitLockRetryDelay = 100 * time.Millisecond` (`commit_lock.go:76-77`) — ~15 minutes, sized for `takeUserSnapshot` holding the lock across an entire snapshot copy (`commit_lock.go:61-75`).
- `wrapCommitLockContention` (`commit_lock.go:430`): only when `errors.Is(err, ErrWorkspaceBusy)` does it prepend `"another lit process is writing to this workspace (a concurrent mutation or snapshot still running); retry after it completes: %w"`. Every other error, cancellation included, passes through untouched.
- `LockCommitPath(ctx, lockPath)` (`commit_lock.go:377`) — the same primitive for callers with no open Store.
- `SettleCommitLockRelease(opErr, releaseErr)` (`commit_lock.go:346`): release error with no op error → printed to stderr as `"lit: commit lock release failed after the operation completed (the hold is gone; nothing to redo): %v"` and the operation returns **nil**; with an op error → `errors.Join`.

### 2.2 Transient online-GC contention

- Sentinel `ErrTransientGCContention = errors.New("transient online-gc contention")` (`commit_lock.go:34`).
- `transientRetryMaxAttempts = 30` (package **var**, so tests can shrink it) (`commit_lock.go:55`).
- `transientRetryBaseDelay = 50ms`, `transientRetryMaxDelay = 1s` (`commit_lock.go:58-59`). Total ~25s.
- `transientRetryDelay(attempt)` (`commit_lock.go:250`): `50ms << (attempt-1)`, capped at 1s; attempts < 1 are clamped to 1.
- `retryTransientGCContention` (`commit_lock.go:185`): loop `attempt=1..30`. Run `classifyTransientGCError(operation(ctx))`. nil → return nil. If not transient, or last attempt → break. Else `sleep(ctx, delay)` (a sleep error is returned immediately), then `rotate(ctx)` (a rotate error is returned immediately). After the loop, `exhaustedContentionError(lastErr)`.
- `exhaustedContentionError` (`commit_lock.go:243`): if the surviving error `isManifestReadOnlyError`, promote to `WorkspaceWriteBlockedError{Cause: err}`; otherwise return unchanged.
- `WorkspaceWriteBlockedError.Error()` (`commit_lock.go:220`): `"another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)"`. `Unwrap` preserves the cause (`commit_lock.go:232`).
- Classification predicates:
  - `isManifestReadOnlyError` (`commit_lock.go:483`): lowercased message contains **both** `"cannot update manifest"` and `"read only"`.
  - `isOnlineGCResetError` (`commit_lock.go:495`): lowercased message contains **both** `"online garbage collection"` and `"reconnect"`. The GC-specific phrase is required so the cluster-role transition error (which also says "please reconnect") is not misclassified.
  - `isTransientGCContentionError` (`commit_lock.go:479`) = either of the above.
- Tests: `TestReconnectRotatorRecoversPoisonedOperation` (`sync_test.go:872`), `TestStagedWorkingSetSurvivesReconnect` (`sync_test.go:923`), and `/Users/bmf/code/links-issue-tracker/internal/store/retry_test.go`.

### 2.3 `commitStamp` and the single commit boundary

`commitStamp` (`commit_lock.go:104-116`): `Message string`, `Date time.Time`, `Author string`, `AllowEmpty bool`.

`commitWorkingSetOnce(ctx, stamp)` (`commit_lock.go:286`) — the only function that hands a commit to Dolt:
1. Optional test hook `s.commitWorkingSetHookForTest` (`commit_lock.go:287-291`).
2. `strings.TrimSpace(stamp.Message)`; empty → default literal `"links mutation"` (`commit_lock.go:292-295`).
3. Args begin `["-Am", trimmed]` (`commit_lock.go:296`).
4. `--allow-empty` appended when `stamp.AllowEmpty` (`commit_lock.go:297-299`).
5. `--date <UTC RFC3339>` appended when `Date` is non-zero — **RFC3339 without fractional seconds; sub-second precision truncates** (`commit_lock.go:300-304`, documented at `commit_lock.go:106-109`).
6. `--author <Author>` appended when `Author != ""` (`commit_lock.go:305-307`).
7. `CALL DOLT_COMMIT(?…)` scanning one commit hash (`commit_lock.go:310-311`).
8. On error: if the lowercased message contains `"nothing to commit"` → return **nil** (success-with-no-commit) (`commit_lock.go:315-318`). Otherwise `wrapCommitWorkingSetError` (`commit_lock.go:453`) wraps as `"dolt commit working set: %w"` and, if transient, boxes it as `transientGCContentionError` so `errors.Is(err, ErrTransientGCContention)` is true (`commit_lock.go:449-451`).

`commitWorkingSet(ctx, message)` (`commit_lock.go:268`) = commit lock + transient retry around `commitWorkingSetOnce`.

`withStampedMutation` (`commit_lock.go:156`) runs the **whole** `BeginTx → fn → tx.Commit → commitWorkingSetOnce` sequence inside the retry, with an explicit two-phase resume marker `staged bool`: once `tx.Commit()` succeeds, `staged=true` and a retry resumes **at versioning only**, never re-running `fn` (`commit_lock.go:157-176`). `withMutation` (`commit_lock.go:122`) is the message-only spelling.

---

## PART 3 — Compaction (`internal/store/compaction.go`)

### 3.1 Depth vocabulary

`GCMode` is a type alias for `storage.GCMode` (`compaction.go:32`); `GCNewGen`/`GCFull` re-exported (`compaction.go:34-37`). Contract definition at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:207-218`: `GCNewGen GCMode = iota` (0), `GCFull` (1). `Valid()` accepts exactly those two (`storage/sync.go:231`). `String()` returns `"newgen"`, `"full"`, else `fmt.Sprintf("unknown(%d)", int(m))` (`storage/sync.go:236-244`).

`gcProcedureArgs(m)` (`compaction.go:55`):
- `GCNewGen` → `nil, nil` (i.e. `CALL DOLT_GC()` with no args).
- `GCFull` → `[]string{"--" + cli.FullFlag}, nil` — the flag spelling comes from Dolt's own constant.
- anything else → error `"compaction depth %s has no Dolt spelling"`.
Tests: `TestGCProcedureArgsRendersEachDepth` (`compaction_test.go:179`), `TestGCProcedureArgsRefusesAnUnknownDepth` (`compaction_test.go:202`).

### 3.2 Thresholds and layout constants

At `compaction.go:65-89`:
- `journalDueBytes int64 = 16 << 20` (16 MiB). Rationale recorded in source: Dolt's own auto-GC thresholds at 128 MB; 16 MB bounds a shallow pass's stall below a second (measured 0.26s on a 5.9 MB journal), arrives roughly every 350 mutations, caps journal waste at ~11 MB.
- `archivesDueCount = 64` — count of old-generation archive files. Each shallow pass appends one archive and removes none, so this counts passes since the last deep collection.
- `archiveFileExt = ".darc"` — Dolt's old-generation archive suffix.
- `oldGenDirName = "oldgen"` — the old generation's directory inside the chunk store (Dolt exports no constant for it).

`nomsDir(doltRootDir)` (`compaction.go:107`) = `filepath.Join(doltRootDir, doltDatabaseName, dbfactory.DoltDir, dbfactory.DataDir)`.

### 3.3 `storeFootprint` and measurement

`storeFootprint{JournalBytes int64; OldGenArchives int}` (`compaction.go:94-101`).

`measureFootprint(doltRootDir)` (`compaction.go:126`):
1. `os.Stat(filepath.Join(noms, chunks.JournalFileID))` — Dolt's own exported journal filename constant. `err == nil` → `JournalBytes = info.Size()`; `os.IsNotExist` → left 0; any other error → `"measure chunk journal: %w"` (`compaction.go:132-138`).
2. `os.ReadDir(filepath.Join(noms, "oldgen"))`. Count entries that are **not directories** and whose `filepath.Ext(name) == ".darc"` (`compaction.go:140-147`). `os.IsNotExist` → 0; any other error → `"measure old generation: %w"`.
A store directory that does not exist yields an empty footprint with no error.
`(*Store).measureFootprint()` (`compaction.go:156`) delegates using `s.doltRootDir`.

Tests: `TestMeasureFootprintReadsJournalAndArchives` (`compaction_test.go:126`), `TestMeasureFootprintReadsAnAbsentStoreAsEmpty` (`compaction_test.go:145`), `TestMeasureFootprintCountsOnlyArchives` (`compaction_test.go:160`), `TestMeasureFootprintMatchesDoltsRealOldGenLayout` (`sync_test.go:422`).

### 3.4 The legality/due rule

`dueMode(footprint) (GCMode, bool)` (`compaction.go:173`) — pure:
1. `OldGenArchives >= 64` → `(GCFull, true)`. **Tested first** because the deep pass subsumes the shallow one.
2. `JournalBytes >= 16 MiB` → `(GCNewGen, true)`.
3. otherwise → `(GCNewGen, false)`.
Test `TestDueModeSelectsDepthByFootprint` (`compaction_test.go:17`).

### 3.5 Compaction entry points

`compactWithinLock(ctx, mode)` (`sync.go:448`) — caller must already hold the commit lock:
1. `gcProcedureArgs(mode)`.
2. `CALL DOLT_GC(args…)`; error `"compact dolt store (%s): %w"`.
3. `s.reconnect(ctx)` — mandatory, because online GC poisons the active SQL connection (`sync.go:456-457`).

`SyncCompact(ctx, mode)` (`sync.go:470`):
1. **Door guard**: `!mode.Valid()` → `"compact: illegal depth %d (want %q or %q)"` with `storage.GCNewGen`/`storage.GCFull` named (`sync.go:471-477`). Test `TestSyncCompactRefusesAnIllegalDepth` (`sync_test.go:387`).
2. `before, beforeErr := s.measureFootprint()` — **outside** the lock.
3. `runSyncMutation(compactWithinLock(mode))`.
4. `after, afterErr := s.measureFootprint()` — also outside the lock.
5. Returns `CompactionOutcome{Ran: true, Depth: mode, Detail: footprintDelta(before, after, errors.Join(beforeErr, afterErr))}`.
Test `TestSyncCompactRunsCleanlyAndPreservesData` (`sync_test.go:320`).

`CompactIfDue(ctx)` (`sync.go:501`) — the backstop gate:
1. `s.measureFootprint()`; a measurement error returns `"measure store footprint: %w"` (explicitly *not* "nothing due") (`sync.go:502-508`).
2. `mode, due := dueMode(footprint)`; `!due` → zero `CompactionOutcome{}` and **nil error** (`sync.go:509-512`).
3. else `s.SyncCompact(ctx, mode)`.

`chooseCompactionDepth()` (`sync.go:526`) — the push path's depth selector:
1. Measurement failure → returns `(GCNewGen, err)` — a usable floor **and** the error. The push always compacts at least the new generation; the footprint can only deepen, never cancel (`sync.go:516-525`).
2. `dueMode` due → that mode.
3. otherwise `GCNewGen, nil`.

### 3.6 Reporting vocabulary

- `footprintDelta(before, after, measureErr)` (`compaction.go:191`): on `measureErr != nil` → `"footprint not measured: %v"`. Otherwise exactly `"journal %s -> %s, old-generation archives %d -> %d"` with `humanBytes` for the two sizes.
- `compactionReport(mode, measureErr)` (`compaction.go:209`):
  - `measureErr != nil` → `"compaction: ran %s pass; could not measure whether a deeper one is due: %v"`.
  - `mode == GCFull` → `"compaction: ran full pass, rewriting the old generation"`.
  - default → `""` (a routine shallow pass says nothing).
  Test `TestCompactionReportSpeaksOnlyWhenItHasSomethingToSay` (`compaction_test.go:210`).
- `joinMaintenance(reports…)` (`compaction.go:223`): drops empty strings and joins the rest with `"; "`. Test `TestJoinMaintenanceDropsSilentReports` (`compaction_test.go:227`).
- `humanBytes(n)` (`/Users/bmf/code/links-issue-tracker/internal/store/remotecache.go:242`): `< 1024` → `"%d B"`; otherwise `"%.1f %ciB"` over the unit table `"KMGTPE"`.
- `CompactionOutcome{Ran bool; Depth GCMode; Detail string}` at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:248-272`. `Detail` is empty **only** for the zero outcome that did nothing; a pass that ran but could not measure reports the failure in `Detail`.

### 3.7 Remote-cache prune (the other half of push maintenance)

- Key derivation `remoteCacheKey(remoteURL)` (`remotecache.go:74`): parse URL; if the lowercased scheme lacks the `git+` prefix (`gitBackedURLSchemePrefix`) return `("", false, nil)`. Otherwise strip `git+` from the scheme, clear `RawQuery` and `Fragment`, and key = `hex(sha256(underlying.String() + "|" + defaultGitRemoteRef))` (`remotecache.go:85-90`).
- `isRemoteCacheKey(name)` (`remotecache.go:99`): exactly 64 chars (`sha256.Size*2`), all-lowercase, valid hex.
- `remoteCacheBase()` (`remotecache.go:266`) = `filepath.Join(s.doltRootDir, doltDatabaseName, dbfactory.DoltDir, remoteCacheDirName)`.
- `listRemoteCacheKeys(base)` (`remotecache.go:273`): a non-existent base returns `(nil, nil)`; otherwise directory entries that are dirs and pass `isRemoteCacheKey`.
- **The legality rule** `planRemoteCachePrune(expected, onDisk)` (`remotecache.go:146`): a directory is abandoned when no configured remote derives its key. If `len(abandoned) > 0 && len(unaccounted) > 0` the prune **declines wholesale** with the long error at `remotecache.go:169-181` (names the counts and the `remote→key` pairs). Both lists are sorted (`remotecache.go:158,166`).
- `remoteCachePruneOutcome{Removed int; Reclaimed int64; Problem string}` (`remotecache.go:198-205`). `Report()` (`remotecache.go:220`):
  - problem + removals → `"remote-cache prune: removed %d abandoned mirror%s (%s), then failed: %s"`
  - problem only → `"remote-cache prune: " + problem`
  - removals only → `"remote-cache prune: removed %d abandoned mirror%s, reclaimed %s"`
  - neither → `""`.
- `dirSize(root)` (`remotecache.go:292`) walks and sums file sizes for the `Reclaimed` figure.
- Eligibility is **re-asked per directory** inside the deletion loop rather than trusted from the plan snapshot (`remotecache.go:338-345`).

---

## PART 4 — Schema guard (`internal/store/sync_schema_guard.go`)

### 4.1 `RemoteSchemaAheadError`

Fields (`sync_schema_guard.go:31-37`): `Remote`, `Branch`, `RemoteVersion int64`, `BinarySupportedMax int64`, `RemoteProducerVersion string` (`""` when the remote head records no producer stamp).

`Error()` (`sync_schema_guard.go:39`) renders:
`"remote <remote>/<branch> is at schema version %d but this binary supports only up to %d; refusing to write a commit below the remote head's schema"`, then:
- if `RemoteProducerVersion != ""` → `" — run `lit upgrade --to <version>`"`
- else → `" — upgrade lit to a version that supports this schema"`.

### 4.2 Guard paths

`guardRemoteSchemaAhead(ctx, remote, branch)` (`sync_schema_guard.go:62`) — the **push** entry:
1. Both args required.
2. `trackingHeadHash(remote, branch)`; `!synced` → **no-op, return nil** (a branch that never synced has no remote head to fall behind) (`sync_schema_guard.go:75-77`).
3. else `guardCommitSchemaAhead(remote, branch, head)`.

`guardCommitSchemaAhead(ctx, remote, branch, commitHash)` (`sync_schema_guard.go:88`) — shared core, also called directly by the reconcile with its already-captured `remoteHead`:
1. `migrations.MaxVersion()` → `registryMax`.
2. `remoteHeadSchema(commitHash)` → `(remoteVersion, producer)`.
3. `remoteVersion <= registryMax` → nil.
4. else `&RemoteSchemaAheadError{...}`.

`trackingHeadHash` (`sync_schema_guard.go:115`): `SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?` on `remotes/<remote>/<branch>`; count 0 → `("", false, nil)`; else `commitHashOfRef`.

`remoteHeadSchema(commitHash)` (`sync_schema_guard.go:139`):
1. **Refuses** unless `isDoltCommitHash(commitHash)` — error `"remote head schema: %q is not a Dolt commit hash"` (`sync_schema_guard.go:140-146`). Necessary because `AS OF` cannot take a bound parameter and the hash is interpolated.
2. `schemaVersionAtCommit` then `producerVersionAtCommit`.

`schemaVersionAtCommit` (`sync_schema_guard.go:163`): `SELECT MAX(version_id) FROM goose_db_version AS OF '<hash>'`.
- MySQL 1146 (missing table) → returns `0` (pre-goose remote, never ahead).
- NULL max (empty goose table) → `0`.
- any other error → `"read schema version at %q: %w"`.

`producerVersionAtCommit` (`sync_schema_guard.go:183`): `SELECT meta_value FROM meta AS OF '<hash>' WHERE meta_key = ?` bound to `producerBinaryVersionMetaKey`.
- `sql.ErrNoRows` or missing table → `("", nil)`.
- any other error → `"read producer version at %q: %w"`.
- otherwise `strings.TrimSpace(value.String)`.

`isMissingTableError(err)` (`sync_schema_guard.go:200`): `errors.As` to `*embedded.MySQLError` with `Number == 1146` — matched on the typed error, not message text.

`isDoltCommitHash(s)` (`sync_schema_guard.go:209`): exactly **32** characters, each in `0-9` or `a-v` (Dolt's base32 alphabet). Test `TestIsDoltCommitHash` (`sync_schema_guard_test.go:299`).

### 4.3 Who is and is not guarded

- `SyncPush`/`SyncCompactAndPush` with a non-empty branch: guarded (`sync.go:626-630`). Test `TestSyncPushRefusesWhenRemoteSchemaAhead` (`sync_schema_guard_test.go:164`).
- Push with an empty branch: **not** guarded (`sync.go:626`).
- Every reconcile that replays (three-way, combine, take-local): guarded inside `replayUnderGuard` before any write (`sync_reconcile.go:390`). Tests `TestSyncReconcileRefusesWhenRemoteSchemaAhead` (`sync_schema_guard_test.go:214`), `TestSyncResolveUnrelatedTakeLocalRefusesSchemaAheadRemote` (`sync_unrelated_test.go:600`).
- **take-remote is exempt** — it authors no replay commit and adopting an ahead head is a safe recovery (`sync_unrelated_take.go:171-176`).
- Other tests: `TestRemoteHeadSchemaReadsVersionAndProducer` (`sync_schema_guard_test.go:69`), `TestGuardRemoteSchemaAheadDetects` (`sync_schema_guard_test.go:103`), `TestGuardRemoteSchemaNotAheadAtOrBelowMax` (`sync_schema_guard_test.go:134`).

---

## PART 5 — Reconcile (`internal/store/sync_reconcile.go`)

### 5.1 Commit message constants

- `reconcileCommitMessage = "reconcile: field-aware merge of remote divergence"` (`sync_reconcile.go:24`)
- `combineCommitMessage = "reconcile: combine unrelated histories (union of both backlogs)"` (`sync_reconcile.go:30`)
- `reconcileLiftCommitMessage = "reconcile: lift remote head to current schema"` (`sync_reconcile.go:38`)
- `takeLocalCommitMessage = "reconcile: take local backlog over unrelated remote history"` (`/Users/bmf/code/links-issue-tracker/internal/store/sync_unrelated_take.go:21`)

### 5.2 Unrelated-history handling value

`type unrelatedHandling int` (`sync_reconcile.go:45`) with:
- `detectOnly` (=0, `sync_reconcile.go:51`): classify as `SyncReconcileUnrelated`, commit nothing.
- `unionCombine` (=1, `sync_reconcile.go:58`): union both sides via a two-way merge over an empty base.

### 5.3 Scratch branches

- Prefix `reconcileScratchPrefix = "links-reconcile-scratch"` (`sync_reconcile.go:66`).
- `reconcileScratchName()` = `fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())` (`sync_reconcile.go:74`).
- `reconcileScratch{spine, read string}` (`sync_reconcile.go:100-103`); `newReconcileScratch()` derives `base+"-spine"` and `base+"-read"` from one unique base (`sync_reconcile.go:109-112`).
- Role split (`sync_reconcile.go:80-99`): `read` is hard-reset once per folded commit (nothing on it is kept); `spine` is hard-reset exactly once (to adopt the remote head) and thereafter only advances by commit.

### 5.4 Snapshot budget

- `reconcileSnapshotLabel = "pre-reconcile"` (`sync_reconcile.go:118`).
- `reconcileSnapshotRetention = 10` (`sync_reconcile.go:124`).
- `IsReconcileSnapshotName(name)` (`sync_reconcile.go:131`) = `isStampedSnapshotName(name, "pre-reconcile")`.
- `formatReconcileSnapshotLabel(t)` = `fmt.Sprintf("%s-%d", "pre-reconcile", t.UTC().UnixNano())` (`sync_reconcile.go:138`).
- Test `TestIsReconcileSnapshotNameDisjoint` (`sync_reconcile_schema_skew_test.go:342`).

### 5.5 Settle policies (`settleFn`)

`type settleFn func(merge.MergeResult) (model.Export, []merge.ProsePending)` (`sync_reconcile.go:152`).

- `autonomousSettle` (`sync_reconcile.go:158`): if `merged.Settled()` returns `ok` → that export, no pending. Otherwise `(model.Export{}, merged.Pending)`. **Prose is never auto-committed by picking a side.**
- `resolvedSettle(resolutions)` (`sync_reconcile.go:172`): first honor `merged.Settled()` (the divergence converged on its own between the agent reading and finalizing). Then `merge.ApplyProseResolutions(merged, resolutions)` — accepted **only** when the resolutions are an exact bijection with the live pending set. A stale/partial set falls through to `(model.Export{}, merged.Pending)`, re-surfacing the CURRENT pending. Test `TestSyncReconcileResolvedRejectsStaleResolutions` (`sync_reconcile_test.go:246`).

### 5.6 Public reconcile entry points

- `SyncReconcile(ctx, remote, branch)` (`sync_reconcile.go:211`) = `reconcile(..., autonomousSettle, detectOnly)`.
- `SyncReconcileResolved(ctx, remote, branch, resolutions)` (`sync_reconcile.go:228`) = `reconcile(..., resolvedSettle(resolutions), unionCombine)` — one finalize path serving both shared-history three-way and unrelated combine (`sync_reconcile.go:229-234`).
- `SyncReconcileCombine(ctx, remote, branch)` (`sync_reconcile.go:249`) = `reconcile(..., autonomousSettle, unionCombine)`. On a divergence **with** a common base it merges through that base like an ordinary reconcile (`sync_reconcile.go:246-248`).

### 5.7 `reconcilePlan` and its capture

`reconcilePlan{diverged bool; ahead, behind int64; dataBranch, localHead, remoteHead string; base mergeBaseResult}` (`sync_reconcile.go:261-269`).

`captureReconcilePlan(ctx, remote, branch)` (`sync_reconcile.go:277`), run under the already-held commit lock:
1. `SyncFreshness` → `ahead`, `behind`.
2. `fresh.State() != SyncDiverged` → return with `diverged=false` and **no anchors** (`sync_reconcile.go:284-289`).
3. `diverged = true`; `trackingRef = "remotes/<remote>/<branch>"`.
4. `activeBranch(ctx, s.db)` → `dataBranch` (`SELECT active_branch()`, `sync_reconcile.go:1003`).
5. `readDoltHead(ctx, s.db)` → `localHead`; error `"read local head: %w"`.
6. `commitHashOfRef(ctx, s.db, trackingRef)` → `remoteHead` (`SELECT commit_hash FROM dolt_log(?) LIMIT 1`, `sync_reconcile.go:1014`; error `"read head of %q: %w"`).
7. `mergeBase(ctx, s.db, localHead, trackingRef)` → `base`.

### 5.8 Merge-base and the unrelated-history discriminator

`mergeBaseResult{commit string; hasBase bool}` (`sync_reconcile.go:1033-1036`); `shared() (commit, ok)` (`sync_reconcile.go:1040`).

`noCommonAncestorMsg = "no common ancestor"` (`sync_reconcile.go:1047`) — the message `doltdb.ErrNoCommonAncestor` surfaces through the engine as a generic Error 1105.

`isNoCommonAncestor(err)` (`sync_reconcile.go:1057`): true when `errors.Is(err, sql.ErrNoRows)` **or** `strings.Contains(err.Error(), "no common ancestor")`. Deliberately **not** matched on code 1105 (MySQL's catch-all `ER_UNKNOWN_ERROR`, which would over-match).

`mergeBase(ctx, db, ref1, ref2)` (`sync_reconcile.go:1067`):
1. `SELECT DOLT_MERGE_BASE(?, ?)` scanned as `sql.NullString`.
2. `isNoCommonAncestor(err)` → `(mergeBaseResult{}, nil)` — i.e. `hasBase=false`.
3. any other error → `"merge-base of %q and %q: %w"`.
4. Valid, non-blank scalar → `{commit: trimmed, hasBase: true}`.
5. NULL or blank scalar → `hasBase=false` (belt-and-suspenders across backend versions, `sync_reconcile.go:1078-1084`).

### 5.9 The reconcile algorithm (`reconcile`)

`reconcile(ctx, remote, branch, settle, unrelated)` (`sync_reconcile.go:311`):
1. `requireSyncArg` on remote and branch.
2. Whole body inside `s.withCommitLock` (`sync_reconcile.go:322`).
3. `captureReconcilePlan`. Copy `Ahead`/`Behind` onto the result.
4. `!plan.diverged` → `result.State = SyncReconcileNotDiverged`, return (`sync_reconcile.go:328-331`).
5. `baseCommit, shared := plan.base.shared()`; set `result.LocalHead`, `result.RemoteHead`, `result.BaseCommit` (`sync_reconcile.go:332-333`).
6. **If `!shared` (unrelated histories)** (`sync_reconcile.go:335-366`):
   a. `s.unrelatedInventory(localHead, remoteHead)` — pure `AS OF` reads under the lock; assigned to `result.Unrelated`.
   b. `unrelated == detectOnly` → `result.State = SyncReconcileUnrelated`, **return before** the schema guard, the scratch sweep, the snapshot, and every reset. Both stores untouched (`sync_reconcile.go:349-357`).
   c. `unionCombine` → `replayUnderGuard(..., combineFromAnchors)`.
7. **Else (shared base)** → `replayUnderGuard(..., reconcileFromAnchors)` (`sync_reconcile.go:369-371`).
8. Any error → the zero `SyncReconcileResult` is returned (`sync_reconcile.go:373-375`).

### 5.10 `replayUnderGuard` — the mutation envelope

`replayUnderGuard(ctx, remote, branch, remoteHead, body)` (`sync_reconcile.go:389`), in order:
1. `s.guardCommitSchemaAhead(ctx, remote, branch, remoteHead)` — **before any write**, using the captured `remoteHead` so a concurrent fetch cannot shift the decision.
2. `s.sweepStaleReconcileScratch(ctx)`.
3. `scratchBranch := newReconcileScratch()`.
4. `guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatReconcileSnapshotLabel(time.Now()))` — **one** guard carried across all retries, so exactly one snapshot is taken however many attempts run.
5. `retryTransientGCContention(body, s.reconnect, transientRetryDelay, waitWithContext)`.

`sweepStaleReconcileScratch(ctx)` (`sync_reconcile.go:930`): `SELECT name FROM dolt_branches WHERE name LIKE ?` with `"links-reconcile-scratch-%"`; every match is deleted with `CALL DOLT_BRANCH('-D', name)`. Every failure (list, scan, iterate, delete) prints to stderr with the messages at `sync_reconcile.go:933, 940, 947, 952` and **never fails the reconcile**. The commit lock guarantees every such branch is an orphan (`sync_reconcile.go:924-929`).

### 5.11 Scratch lifecycle (`runOnReconcileScratch`)

`runOnReconcileScratch(ctx, dataBranch, scratch, localHead, body)` (`sync_reconcile.go:725`):
1. `var created []string`; the cleanup defer is armed **before** the first branch creation, so a failure creating the second branch does not strand the first (`sync_reconcile.go:726-733, 716-724`).
2. For each of `[scratch.spine, scratch.read]` in that order: `CALL DOLT_CHECKOUT('-B', branch, localHead)`. `-B` recreates if a prior retry left it behind. Failure → `"create reconcile scratch branch %q: %w"`. Each success appends to `created`.
3. `body()`.
4. Deferred `cleanupReconcileScratch(ctx, dataBranch, created)`; its error is promoted to the return value **only if `err == nil`** (`sync_reconcile.go:730-732`).

`cleanupReconcileScratch(ctx, dataBranch, createdBranches)` (`sync_reconcile.go:900`):
1. `CALL DOLT_CHECKOUT(dataBranch)`. On success → delete each created branch with `CALL DOLT_BRANCH('-D', name)`; a delete failure prints `"lit: reconcile cleanup could not delete scratch branch %q: %v"` to stderr and is **not** promoted (`sync_reconcile.go:916-920`).
2. On checkout failure → print `"lit: reconcile could not return to data branch %q (%v); rotating connection to recover"`, then `s.reconnect(ctx)`. If the rotation fails → return the unrecoverable error `"reconcile left the store on the scratch branch and could not recover: checkout %q failed (%v); connection rotation failed: %w"` (`sync_reconcile.go:903-905`).
3. After a successful rotation, checkout the data branch **explicitly** again; failure → `"reconcile could not restore the data branch %q after rotating the connection: %w"` (`sync_reconcile.go:911-913`). The leftover scratch branch is left behind deliberately.

### 5.12 Reading exports at fixed anchors

`resetAndLift(ctx, commit)` (`sync_reconcile.go:970`):
1. `CALL DOLT_RESET('--hard', commit)`; error `"reset scratch to %q: %w"`.
2. `s.liftWorkingSetToRegistry(ctx)`; error `"lift %q to current schema: %w"`.
Only ever run on a scratch branch.

`exportAtCommit(ctx, readBranch, commit)` (`sync_reconcile.go:991`):
1. `CALL DOLT_CHECKOUT(readBranch)` — the branch is an **argument**, never inherited from session state; error `"switch to reconcile read branch %q: %w"`.
2. `resetAndLift(commit)`; error `"read export at %q: %w"`.
3. `s.Export(ctx)`.

### 5.13 `mergeAndReplay` — the shared merge/settle/replay tail

`mergeAndReplay(ctx, result, settle, guard, dataBranch, scratch, localHead, remoteHead, base, message, settledState)` (`sync_reconcile.go:665`):
1. `ours = exportAtCommit(scratch.read, localHead)`.
2. `theirs = exportAtCommit(scratch.read, remoteHead)`.
3. `merged = merge.ThreeWay(base, ours, theirs)`.
4. `export, pending = settle(merged)`.
5. **`len(pending) > 0`** → `result.State = SyncReconcileProsePending`, `result.Pending = pending`, **return with nothing committed**. The data branch is still at `localHead` (`sync_reconcile.go:677-689`).
6. `chain = readFoldedChain(ctx, s.db, remoteHead, localHead)`.
7. `stepper = foldStepper{store, readBranch: scratch.read, chain, base, theirs}`.
8. `replayed = commitReplayAndAdvance(guard, dataBranch, scratch, remoteHead, message, export, stepper)`.
9. `result.State = settledState`; `result.Pending = nil`; `result.Replayed = replayed`.

`reconcileFromAnchors` (`sync_reconcile.go:413`) = `runOnReconcileScratch` + `base := exportAtCommit(scratch.read, baseCommit)` + `mergeAndReplay(..., base, reconcileCommitMessage, SyncReconcileLinearized)`.

`combineFromAnchors` (`sync_reconcile.go:429`) = `runOnReconcileScratch` + `mergeAndReplay(..., model.Export{}, combineCommitMessage, SyncReconcileCombined)` — the **empty export is the base**, which `merge.ThreeWay` reads as "both sides changed every field from empty", i.e. a two-way union (`sync_reconcile.go:431-433`).

### 5.14 Folded-chain provenance

`foldedCommit{hash, message, author string; date time.Time}` (`sync_reconcile.go:443-448`).

`foldedChainRange(remoteHead, localHead)` = `remoteHead + ".." + localHead` (`sync_reconcile.go:456`). One spelling: on unrelated histories it is the entire local chain; on a shared base it is exactly the ahead commits.

`readFoldedChain(ctx, db, remoteHead, localHead)` (`sync_reconcile.go:463`):
1. `SELECT commit_hash, committer, email, date, message FROM dolt_log(?)` with the bound range.
2. `author = fmt.Sprintf("%s <%s>", committer, email)` (`sync_reconcile.go:478`).
3. **Abort condition**: if the chain is non-empty and `chain[0].hash != localHead` → error `"folded chain starts at %q, want local head %q"` (`sync_reconcile.go:484-489`).
4. Reverse in place (dolt_log is newest-first; replay order is oldest-first) (`sync_reconcile.go:490-493`).
Errors: `"read folded chain %s..%s: %w"`, `"scan folded chain commit: %w"`, `"iterate folded chain: %w"`.

`replayStep{export model.Export; stamp commitStamp}` (`sync_reconcile.go:509-512`). Two authorities are explicitly separated (`sync_reconcile.go:501-508`): the Go-side diff decides which **rows** to write; **Dolt** decides whether a commit exists (empty diff → no commit).

`foldStepper{store, readBranch, chain, base, theirs}` (`sync_reconcile.go:532-538`):
- `len()` = `len(chain)` (`sync_reconcile.go:543`).
- `step(ctx, i)` (`sync_reconcile.go:552`): `at := exportAtCommit(readBranch, chain[i].hash)`; returns `replayStep{export: merge.ThreeWay(base, at, theirs).Provisional(), stamp: commitStamp{Message: c.message, Date: c.date, Author: c.author}}`. Every step reads its own commit including the newest — no special case for the last (`sync_reconcile.go:545-551`).

### 5.15 The spine writer

`spineWriter{store, branch, landed model.Export}` (`sync_reconcile.go:613-620`). `landed` is **not** a parameter; the writer owns it.

`newSpineWriter(ctx, branch)` (`sync_reconcile.go:628`): `CALL DOLT_CHECKOUT(branch)` (error `"switch to reconcile spine branch %q: %w"`), then `s.Export(ctx)` to seed `landed` from a real read (error `"read reconcile spine base state: %w"`).

`land(ctx, next, stamp)` (`sync_reconcile.go:644`): unconditional `CALL DOLT_CHECKOUT(w.branch)` first, then `replayDeltaOnScratch(diffExports(w.landed, next), stamp)`; `w.landed = next` **only on success**.

`replayDeltaOnScratch(ctx, delta, stamp)` (`sync_reconcile.go:583`): inside `withCommitLock` (re-entrant): `BeginTx` → `defer tx.Rollback()` → `applyExportDelta(ctx, tx, delta)` → `tx.Commit()` → `commitWorkingSetOnce(ctx, stamp)`. **A single attempt, deliberately without the self-rotating transient retry** — a rotation would open a fresh connection on the DEFAULT branch and resume committing onto the data branch (`sync_reconcile.go:564-582`). A transient failure bubbles to `replayUnderGuard`'s outer retry, whose next attempt re-creates both scratch branches as its first op and rebuilds the whole spine from the fixed anchors. Errors: `"begin %s tx: %w"`, `"commit %s tx: %w"`. Test evidence: `TestSyncReconcileCombineRecoversFromTransientFailureMidReplay` (`sync_unrelated_test.go:949`).

### 5.16 `commitReplayAndAdvance` — the safe replay, step by step

`commitReplayAndAdvance(ctx, guard, dataBranch, scratch, remoteHead, message, export, stepper) (int, error)` (`sync_reconcile.go:778`):

1. `CALL DOLT_CHECKOUT(scratch.spine)`; error `"switch to reconcile spine branch %q: %w"`.
2. `s.resetAndLift(ctx, remoteHead)`; error `"adopt remote head %q on scratch: %w"`. This is the spine's **one and only** reset.
3. `s.commitWorkingSetOnce(ctx, commitStamp{Message: reconcileLiftCommitMessage})` — the schema-lift DDL and bookkeeping land as their own named commit, or **no commit at all** on a current-schema head (empty diff). Error `"commit schema lift of %q: %w"` (`sync_reconcile.go:786-794`).
4. `liftedBase = readDoltHead(ctx, s.db)`; error `"read lifted base: %w"`.
5. `writer = newSpineWriter(ctx, scratch.spine)` — constructed **after** the lift commit.
6. Loop `i = 0 .. stepper.len()-1`: `step = stepper.step(ctx, i)`; `writer.land(step.export, step.stamp)`; landing error wrapped `"replay folded commit %q: %w"` naming the step's message (`sync_reconcile.go:810-818`). One step is read and landed before the next is read — bounded memory.
7. `writer.land(ctx, export, commitStamp{Message: message, AllowEmpty: true})` — the marker commit is **unconditional** (`sync_reconcile.go:819-821`).
8. `replayedCommit = readDoltHead(ctx, s.db)`; error `"read replayed commit: %w"`.
9. `landed = countCommitsInRange(ctx, s.db, liftedBase, replayedCommit)`; `replayed = landed - 1` (subtracting the marker) (`sync_reconcile.go:822-833`). `countCommitsInRange` (`sync_reconcile.go:880`) = `SELECT COUNT(*) FROM dolt_log(?)` over `foldedChainRange(exclusiveBase, head)`; error `"count replayed commits %s..%s: %w"`.
10. **Snapshot-first**: `guard.ensure(ctx)`; failure → `"snapshot before reconcile: %w"`, aborting **before** the data branch moves (`sync_reconcile.go:836-845`).
11. `CALL DOLT_CHECKOUT(dataBranch)`; error `"return to data branch %q: %w"`.
12. `CALL DOLT_RESET('--hard', replayedCommit)` — **the single atomic advance of the data branch**; error `"advance %q to replayed commit: %w"` (`sync_reconcile.go:855-857`).
13. `dbsnapshot.PruneMatching(migrationSnapshotsDir(s.doltRootDir), 10, IsReconcileSnapshotName)`. A failure prints `"lit: reconcile could not prune old recovery snapshots (replay already committed): %v"` to stderr and is **not** promoted — the replay already committed (`sync_reconcile.go:858-872`).
14. Return `replayed`.

Invariant: the data branch is at its pre-reconcile head before step 12 and at the complete replayed spine after — never in between.

### 5.17 Reconcile result vocabulary

`SyncReconcileResult` at `/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:359-384`: `State`, `Ahead`, `Behind`, `LocalHead`, `RemoteHead`, `BaseCommit`, `Pending []merge.ProsePending`, `Unrelated *UnrelatedInventory`, `Replayed int`.

States and their string values (`storage/sync.go:300-354`):
- `SyncReconcileNotDiverged = "not_diverged"`
- `SyncReconcileLinearized = "linearized"`
- `SyncReconcileProsePending = "prose_pending"`
- `SyncReconcileUnrelated = "unrelated_histories"`
- `SyncReconcileTookLocal = "took_local"`
- `SyncReconcileTookRemote = "took_remote"`
- `SyncReconcileCombined = "combined"`

`Replayed` counts folded commits that **actually landed** (read back off the spine), zero for non-mutating outcomes and for a fold whose every projection was already contained in the spine (`storage/sync.go:378-383`).

Tests: `TestSyncReconcileLinearizesDivergenceAndFastForwardPushes` (`sync_reconcile_test.go:21`), `TestSyncReconcileHoldsProseDivergenceForAgent` (`sync_reconcile_test.go:121`), `TestSyncReconcileResolvedFinalizesWithAgentText` (`sync_reconcile_test.go:190`), `TestSyncReconcileHealsSchemaSkew` (`sync_reconcile_schema_skew_test.go:91`), `TestSyncReconcileCombineIsBoundedOnALargeFoldedChain` (`sync_reconcile_scale_test.go:101`), `TestSyncReconcileCombinePreservesFoldedProvenance` (`sync_unrelated_test.go:723`).

---

## PART 6 — Unrelated histories: inventory and the "take" flow

### 6.1 Inventory (`internal/store/sync_unrelated.go`)

`unrelatedInventory(ctx, localHead, remoteHead) (*storage.UnrelatedInventory, error)` (`sync_unrelated.go:18`):
1. `issueIDsAtCommit(localHead)`; error `"read local issue inventory: %w"`.
2. `issueIDsAtCommit(remoteHead)`; error `"read remote issue inventory: %w"`.
3. Returns `{OnlyLocal: setDifference(local, remote), OnlyRemote: setDifference(remote, local), OnBoth: setIntersection(local, remote)}` (`sync_unrelated.go:27-31`).

`issueIDsAtCommit(ctx, db, commitHash)` (`sync_unrelated.go:41`):
- **Refuses** unless `isDoltCommitHash(commitHash)`: `"issue inventory: %q is not a Dolt commit hash"` (`sync_unrelated.go:42-48`).
- `SELECT id FROM issues AS OF '<hash>'` (interpolated literal).
- Missing table (MySQL 1146) → the **empty set**, not an error — a pristine bootstrap root genuinely holds no issues (`sync_unrelated.go:51-54`).
- Errors: `"read issue ids at %q: %w"`, `"scan issue id at %q: %w"`, `"iterate issue ids at %q: %w"`.

Pure `AS OF` reads: no branch moves, no schema lift — preserving the detection's no-write guarantee (`sync_unrelated.go:11-17`).

`UnrelatedInventory{OnlyLocal, OnlyRemote, OnBoth []string}` with JSON tags `only_local`, `only_remote`, `on_both`, all `omitempty` (`/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:394-398`). The three slices are sorted and mutually disjoint by construction (`storage/sync.go:386-393`). Test `TestSyncReconcileUnrelatedInventoryPartitionsAllThreeSides` (`sync_unrelated_test.go:104`).

### 6.2 The owner-approval token

`takeApprovalToken(localHead, remoteHead, choice)` (`sync_unrelated_take.go:33`):
`hex.EncodeToString(sha256.Sum256([]byte("take:" + string(choice) + "\x00" + localHead + "\x00" + remoteHead))[:6])` — a **12-hex-character** digest (first 6 bytes). Deliberately unexported: the only way to obtain a token is to run the take and read its refusal (`sync_unrelated_take.go:29-32`). Any commit on either side, or approving one side and running the other, changes the token.

`OwnerApprovalRequiredError` (`sync_unrelated_take.go:46-56`): `Choice`, `ApprovalToken` (the token that WOULD authorize right now), `Stale bool`, `LocalHead`, `RemoteHead`, `Ahead`, `Behind`, `Inventory *storage.UnrelatedInventory`.
`Error()` (`sync_unrelated_take.go:58`):
- `Stale` → `"sync reconcile take %s: the supplied owner approval no longer matches this divergence (the backlog moved, or it was issued for the other side)"`
- else → `"sync reconcile take %s is destructive and requires explicit owner approval"`.

Test `TestSyncResolveUnrelatedOwnerApprovalBindsForkAndSide` (`sync_unrelated_test.go:285`).

### 6.3 `SyncResolveUnrelated` — the take gate

`SyncResolveUnrelated(ctx, remote, branch, choice storage.UnrelatedResolution, ownerApproval string)` (`sync_unrelated_take.go:93`):
1. `requireSyncArg` on remote and branch.
2. `!choice.Valid()` → `"resolve unrelated histories: unknown side %q (want %q or %q)"` naming `storage.TakeLocal`/`storage.TakeRemote` (`sync_unrelated_take.go:102-106`). `UnrelatedResolution` values: `TakeLocal = "local"`, `TakeRemote = "remote"` (`/Users/bmf/code/links-issue-tracker/internal/storage/sync.go:411-418`); `Valid()` at `storage/sync.go:429`. Test `TestUnrelatedResolutionValid` (`sync_unrelated_test.go:655`).
3. Everything below inside `s.withCommitLock` (`sync_unrelated_take.go:109`).
4. `captureReconcilePlan`; copy `Ahead`/`Behind`.
5. `!plan.diverged` → `SyncReconcileNotDiverged`, return (`sync_unrelated_take.go:115-120`).
6. Set `result.LocalHead`, `result.RemoteHead`.
7. **If the divergence HAS a base** → refuse: `"sync reconcile take applies only to unrelated histories, but %s/%s shares history with the local backlog; run \`lit sync reconcile\` to field-merge it"` (`sync_unrelated_take.go:123-128`). Test `TestSyncResolveUnrelatedRefusesSharedHistory` (`sync_unrelated_test.go:555`).
8. `unrelatedInventory(localHead, remoteHead)` → `result.Unrelated` (read **before** the approval check so the refusal can name what would be destroyed).
9. `expected := takeApprovalToken(plan.localHead, plan.remoteHead, choice)`; if `ownerApproval != expected` → return `OwnerApprovalRequiredError{..., Stale: ownerApproval != ""}` with **nothing mutated** (`sync_unrelated_take.go:141-153`).
10. `s.applyUnrelatedTake(...)`.

### 6.4 `applyUnrelatedTake` dispatch

`applyUnrelatedTake` (`sync_unrelated_take.go:169`):
- `storage.TakeRemote` → its **own** snapshot guard (`newSnapshotGuard(..., formatReconcileSnapshotLabel(time.Now()))`), tracking ref `remotes/<remote>/<branch>`, then `retryTransientGCContention(takeRemoteHead)`. **No scratch envelope, no schema-ahead refusal** (`sync_unrelated_take.go:171-180`).
- `storage.TakeLocal` → `s.replayUnderGuard(ctx, remote, branch, plan.remoteHead, takeLocalOntoRemoteHead)` — the **full** envelope including the schema-ahead refusal (`sync_unrelated_take.go:181-189`).
- default → `"resolve unrelated histories: unhandled side %q"` (`sync_unrelated_take.go:190-197`). The combine resolution never reaches this dispatch; it lives on the reconcile boundary.

### 6.5 `takeRemoteHead`

`takeRemoteHead(ctx, result, guard, trackingRef)` (`sync_unrelated_take.go:207`):
1. `guard.ensure(ctx)`; failure → `"snapshot before take-remote: %w"`.
2. `resetHardToRef(ctx, s.db, trackingRef)`.
3. `dbsnapshot.PruneMatching(migrationSnapshotsDir(...), 10, IsReconcileSnapshotName)`; failure prints `"lit: take-remote could not prune old recovery snapshots (reset already done): %v"` to stderr, **not** promoted.
4. `result.State = SyncReconcileTookRemote`.
Idempotent under retry (cached snapshot, fixed ref). Test `TestSyncResolveUnrelatedTakeRemote` (`sync_unrelated_test.go:159`).

### 6.6 `takeLocalOntoRemoteHead`

`takeLocalOntoRemoteHead(ctx, result, guard, dataBranch, scratch, localHead, remoteHead)` (`sync_unrelated_take.go:237`), inside `runOnReconcileScratch`:
1. `local = exportAtCommit(scratch.read, localHead)`.
2. `theirs = exportAtCommit(scratch.read, remoteHead)`.
3. `chain = readFoldedChain(remoteHead, localHead)`.
4. `stepper = foldStepper{base: model.Export{}, theirs: theirs, ...}` — the **same no-base union projection combine uses**, so mid-chain history stays a whole backlog (`sync_unrelated_take.go:251-254`).
5. `commitReplayAndAdvance(guard, dataBranch, scratch, remoteHead, takeLocalCommitMessage, local, stepper)` — the terminal export is **local's content, not the union**, so the marker commit's diff is exactly the discard of the remote-only issues (`sync_unrelated_take.go:230-236, 255`).
6. `result.State = SyncReconcileTookLocal`; `result.Replayed = replayed`.

Tests: `TestSyncResolveUnrelatedTakeLocal` (`sync_unrelated_test.go:213`), `TestSyncResolveUnrelatedTakeLocalPreservesFoldedProvenance` (`sync_unrelated_test.go:798`).

### 6.7 Combine

Reached only via `SyncReconcileCombine` or `SyncReconcileResolved`. It is **non-destructive**, so unlike the takes it does not refuse shared history (`sync_reconcile.go:246-248`). Tests: `TestSyncReconcileCombineUnionsBothSides` (`sync_unrelated_test.go:347`), `TestSyncReconcileCombineHoldsAndFinalizesProse` (`sync_unrelated_test.go:415`).

Detection-only: `TestSyncReconcileDetectsUnrelatedHistories` (`sync_unrelated_test.go:26`).

---

## PART 7 — Export delta (what the replay actually writes) (`internal/store/export_delta.go`)

### 7.1 Delta types

`tableDelta[K comparable, R any]{remove []K; add []R}` (`export_delta.go:32-35`); `empty()` at `export_delta.go:39`.
`exportDelta{issues, relations, comments, labels, events}` (`export_delta.go:111-117`), ordered so issues precede everything referencing them. `exportDelta.empty()` at `export_delta.go:121`.

### 7.2 `diffTable` — the per-table comparison rule

`diffTable(live, wanted []R, key func(R) K, persisted func(R) any)` (`export_delta.go:78`):
1. Index `live` and `wanted` by key.
2. For each row in `live` **in slice order**: queue its key for removal when the key is absent from `wanted` **or** `!reflect.DeepEqual(persisted(row), persisted(want))`.
3. For each row in `wanted` **in slice order**: queue the row for addition when the key is absent from `live` **or** the persisted projections differ.
A row whose value changed under a stable key appears in **both** lists — removed by key, then re-added — so no UPDATE statement exists (`export_delta.go:27-31`).
Determinism: the input slices are walked, not the maps, so identical inputs produce an identical statement sequence (`export_delta.go:47-51`).
`reflect.DeepEqual` makes the comparison total; its failure direction is a redundant rewrite, never a missed one (`export_delta.go:62-68`).

### 7.3 Projections

- `wholeRow[R](row R) any { return row }` (`export_delta.go:107`) — used for relations, comments, labels, and events. An event's `Changes` are part of its value, so an event whose changes differ is a changed row (`export_delta.go:136-138`).
- `issues` uses `issueRowValues(i)` because `model.Issue` is a hydrated view that also carries labels from another table and, for a container, a lifecycle derived from its children; comparing those made every label edit and every child status change look like an issue-row change (`export_delta.go:53-60, 147`).

### 7.4 Keys

- issues: `i.ID` (string) (`export_delta.go:146`)
- relations: `relationKey{srcID, dstID, kind: r.Type}` (`export_delta.go:155-157`)
- comments: `c.ID` (`export_delta.go:161`)
- labels: `labelKey{issueID, name}` (`export_delta.go:165`)
- events: `e.ID` (`export_delta.go:169`)
Primary-key types live in `/Users/bmf/code/links-issue-tracker/internal/store/row_deletes.go` (`export_delta.go:22-24`).

### 7.5 Cascade rule

`diffExports(prev, next)` (`export_delta.go:142`):
1. The **issues** diff is computed first.
2. `survivors = cascadeSurvivors(prev.Issues, issues.remove)` (`export_delta.go:148`, definition `export_delta.go:189`) — the complement of the removal list, read **off the issues diff** rather than recomputed.
3. Each child table's `live` input is `prev`'s rows **filtered to post-cascade survivors**:
   - relations: kept when `survivors[r.SrcID] && survivors[r.DstID]` — a relation cascades away if **either** endpoint does (`export_delta.go:153`)
   - comments: `survivors[c.IssueID]` (`export_delta.go:159`)
   - labels: `survivors[l.IssueID]` (`export_delta.go:164`)
   - events: `survivors[e.IssueID]` (`export_delta.go:167`)
The schema fact this rests on: every child table is `ON DELETE CASCADE` from issues (and `issue_event_changes` from `issue_events`) (`export_delta.go:128-134`).

### 7.6 Application order

`applyExportDelta(ctx, tx, delta)` (`export_delta.go:217`) applies, in this exact order: issues, relations, comments, labels, events (`export_delta.go:218-230`), each via `applyTableDelta` (`export_delta.go:236`) which performs **all removals before all additions** for that table (`export_delta.go:243-256`). Row counts from deletions are deliberately ignored (`export_delta.go:244-250`).

---

## PART 8 — Migration runner (`internal/store/migration_runner.go`)

### 8.1 Constants and keys

- `producerBinaryVersionMetaKey = "producer_binary_version"` (`migration_runner.go:29`)
- `migrationCheckpointPrefix = "pre-migrate"` (`migration_runner.go:31`)
- `migrationCheckpointRetention = 5` (`migration_runner.go:32`)
- `migrationDriftRepairCheckpointPrefix = "pre-drift-repair"` (`migration_runner.go:38`)
- `gooseVersionTable = "goose_db_version"` (`migration_runner.go:217`)
- `baselineVersion = migrations.Baseline` (`migration_runner.go:226`)
- `migrationSnapshotRetention = 10` (`/Users/bmf/code/links-issue-tracker/internal/store/migrate_snapshot.go:17`)
- `migrationSnapshotLabel = "pre-migrate"` (`migrate_snapshot.go:30`)

Test hooks: `migrationUpByOneForTest` (`migration_runner.go:47`) applies **only** to `applyPendingMigrations`, never the reconcile lift; `migrationPostSnapshotHookForTest` (`migrate_snapshot.go:25`) fires inside `runMigration` after the snapshot guard.

### 8.2 Phases

`migrationPhase` (`migration_runner.go:234`) with `phaseFresh`(0), `phaseAdopt`(1), `phaseManaged`(2) (`migration_runner.go:236-246`).
`migrationState{phase; appliedVersion int64; registryMaxVers int64}` (`migration_runner.go:250-254`).
`willMutate()` (`migration_runner.go:259`): `phaseManaged` → `appliedVersion < registryMaxVers`; every other phase → **true**.

`classifyMigrationState(ctx)` (`migration_runner.go:861`) — reads only, in this order:
1. `migrations.MaxVersion()` → `registryMax`.
2. `tableExists("issue_history")` — the **legacy marker**. Present → `phaseAdopt` immediately, **regardless of what goose_db_version claims** (`migration_runner.go:881-887`). Rationale: `issue_history` is a pre-goose-only table the baseline never creates and `reconcileToBaseline` drops; its presence is conclusive evidence of a pre-goose canonical shape, and this re-routes "buggy older binary fabricated goose rows on a pre-goose workspace" back into `phaseAdopt` (`migration_runner.go:866-880`).
3. `tableExists("goose_db_version")` → present → `phaseManaged` with `appliedVersion = recordedMigrationVersion(ctx)`.
4. `verifyBaselineShape(ctx)` → `present == 0` → `phaseFresh`; otherwise `phaseAdopt`.

`recordedMigrationVersion` (`migration_runner.go:1279`): goose `database.NewStore(goose.DialectMySQL, "goose_db_version").GetLatestVersion`; `database.ErrVersionNotFound` → `0, nil`.

`AppliedSchemaVersion(ctx)` (`migration_runner.go:1245`) is a pure read over `classifyMigrationState`.

### 8.3 `migrate` — the outer boundary

`(*Store).migrate(ctx)` (`migration_runner.go:275`):
1. `guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatMigrationSnapshotLabel(time.Now()))`.
2. `s.runMigration(ctx, guard)`.
3. On error: if `guard.took()` → wrap as `&MigrationRollbackError{Snapshot: snap, Cause: err}`; otherwise return the raw error (`migration_runner.go:281-286`).
4. On success: if a snapshot was taken → `dbsnapshot.PruneMatching(guard.snapshotsDir, 10, IsMigrationSnapshotName)`; a prune failure **is** promoted here: `"prune migration snapshots: %w"` (`migration_runner.go:287-294`).

### 8.4 `runMigration` — the full sequence

`runMigration(ctx, guard)` (`migration_runner.go:308`), in exact order:

1. `state = classifyMigrationState(ctx)`.
2. **Ahead-of-registry check**: `state.appliedVersion > state.registryMaxVers` → `return s.refuseIfBaselineMissing(ctx, state)`. **No bookkeeping mutation ever happens on this path** — trimming the goose log would destroy true information (`migration_runner.go:313-324`).
3. **Version-content drift check** — `state.phase == phaseManaged` only, and run **independently of `willMutate`** (`migration_runner.go:329-359`):
   - `verifyAppliedVersionsMatchRegistry(ctx, state.appliedVersion)`.
   - A non-`*VersionContentMismatchError` error is returned as-is.
   - A mismatch triggers `repairVersionContentDriftWithRollback(ctx, appliedVersion, mismatch)`; on clean repair, **no error reaches the caller**.
4. `!state.willMutate()` → return nil (no snapshot, no writes).
5. `s.ensureQuarantineTable(ctx)` (`migration_runner.go:373`), then `s.commitWorkingSet(ctx, "migrate: ensure migration_quarantine table")`; commit failure → `"commit quarantine table: %w"`. Committed **before** `guard.ensure` so it survives a later checkpoint reset (`migration_runner.go:363-378`).
6. `s.quarantineFastFail(ctx, state)` — **before** the snapshot guard, so a permanently-quarantined workspace does not accumulate a snapshot per Open (`migration_runner.go:379-386`).
7. If `phaseAdopt`: `s.verifyIssuesReconcilable(ctx)`; failure → `"reconcile pre-goose workspace: %w"` — also **before** the snapshot (`migration_runner.go:387-401`).
8. `guard.ensure(ctx)`; failure → `"migrate: %w"` (`migration_runner.go:402-404`).
9. `migrationPostSnapshotHookForTest` if set (`migration_runner.go:405-409`).
10. If `phaseAdopt` (`migration_runner.go:410-459`):
    a. `s.reconcileToBaseline(ctx, guard)`; failure → `"reconcile pre-goose workspace: %w"`. Idempotent probe-driven forward migrator; explicitly a historical artifact to which no new operations get added (`migration_runner.go:411-421`).
    b. `s.verifyBaselineShape(ctx)`; error → `"verify post-reconcile baseline shape: %w"`. If any gaps remain → refuse with `"post-reconcile workspace shape still differs from baseline (remaining gaps: %s); reconcile cannot bring this workspace to v1 — the shape is structurally beyond what pre-goose reconcile can recover"` (`migration_runner.go:437-449`).
    c. `s.adoptPreGooseWorkspace(ctx)`.
    d. `s.commitWorkingSet(ctx, fmt.Sprintf("migrate: adopt pre-goose workspace at v%d", baselineVersion))`; failure → `"commit adoption stamp: %w"`.
11. `s.applyPendingMigrations(ctx)`.
12. `wrote, err := s.recordProducerBinaryVersion(ctx)`; `!wrote` → return nil; else `s.commitWorkingSet(ctx, "migrate: record producer binary version")` (`migration_runner.go:463-476`).

### 8.5 `applyPendingMigrations` — the goose loop

`applyPendingMigrations(ctx)` (`migration_runner.go:553`):
1. `newGooseProvider(s.db)` — constructed **before** the checkpoint so a construction failure leaves no orphan branch. Failure → `"construct migration provider: %w"` (`migration_runner.go:555-559`).
2. `checkpoint, err := s.CreateCheckpoint(ctx, "pre-migrate")`; failure → `"create migration checkpoint: %w"` (`migration_runner.go:564-567`).
3. `step` closure: uses `migrationUpByOneForTest` when set, else `s.upByOne(ctx, provider)` (`migration_runner.go:573-578`).
4. Loop:
   - `errors.Is(gooseErr, goose.ErrNoNextVersion)` → `s.PruneCheckpoints(ctx, "pre-migrate", 5)`; a prune failure IS returned as `"prune migration checkpoints: %w"`; then return nil (`migration_runner.go:582-588`).
   - other `gooseErr != nil` → `cpErr := s.handleMigrationFailure(...)`, then `_ = s.PruneCheckpoints(ctx, "pre-migrate", 5)` (error **deliberately ignored**), return `cpErr` (`migration_runner.go:589-598`).
   - success → `s.commitWorkingSet(ctx, migrationCommitMessage(result))`; failure → `"commit migration v%d: %w"` (`migration_runner.go:599-605`).

`upByOne(ctx, provider)` (`migration_runner.go:490`) is bare `provider.UpByOne(ctx)` — the ONE shared forward-step; the test hook wraps it only at the `applyPendingMigrations` call site (`migration_runner.go:479-489`).

`migrationCommitMessage(result)` = `fmt.Sprintf("migrate: v%d %s", result.Source.Version, filepath.Base(result.Source.Path))` (`migration_runner.go:1384`).

`newGooseProvider(db)` = `goose.NewProvider(goose.DialectMySQL, db, migrations.FS)` (`migration_runner.go:1378-1380`).

### 8.6 Failure handling and quarantine

`handleMigrationFailure(ctx, result, cause, checkpoint)` (`migration_runner.go:616`), ordering **reset first, quarantine second**:
1. Extract `version`/`name` from `result.Source` when non-nil; `name = filepath.Base(result.Source.Path)` (`migration_runner.go:617-622`).
2. `s.ResetToCheckpoint(ctx, checkpoint.Name)`. Failure → `"migration v%d failed and Dolt reset to %q failed (%v); restore from dbsnapshot. Root cause: %w"` (`migration_runner.go:624-629`).
3. If `version > 0`:
   - `s.recordQuarantine(ctx, version, name, cause.Error())`; failure → `"migration v%d failed (reset to %q); quarantine insert failed (%v); restore from dbsnapshot. Root cause: %w"`.
   - `s.commitWorkingSet(ctx, fmt.Sprintf("migrate: quarantine v%d %s", version, name))`; failure → `"...quarantine commit failed (%v)..."` (`migration_runner.go:631-644`).
   A **nil-result failure (version 0) skips quarantine insertion entirely** (`migration_runner.go:591-595`).
4. Return `&CheckpointResetError{Version, Name, Checkpoint, Cause}`.

`recordQuarantine` (`migration_runner.go:790`): `INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE name = VALUES(name), error_text = VALUES(error_text), created_at = VALUES(created_at)` with `created_at = time.Now().UTC().Format(time.RFC3339Nano)`.

`quarantineFastFail(ctx, state)` (`migration_runner.go:760`): `effectiveApplied = state.appliedVersion`, but for `phaseAdopt` it is overridden to `baselineVersion` so a quarantine row for the baseline cannot block after adoption (`migration_runner.go:761-765`).

`checkPendingQuarantine(ctx, appliedVersion)` (`migration_runner.go:770`): `SELECT version, name, error_text FROM migration_quarantine WHERE version > ? ORDER BY version LIMIT 1`. `sql.ErrNoRows` → nil. Any row → `&QuarantineBlockError{...}`.

### 8.7 The quarantine table and its self-heal

`quarantineTableStmt` (`migration_runner.go:657-663`), verbatim:
```
CREATE TABLE migration_quarantine (
	version    BIGINT NOT NULL,
	name       TEXT NOT NULL,
	error_text TEXT NOT NULL,
	created_at VARCHAR(64) NOT NULL,
	PRIMARY KEY (version)
)
```
`canonicalQuarantineColumns = []string{"version", "name", "error_text", "created_at"}` (`migration_runner.go:669`).

`ensureQuarantineTable(ctx)` (`migration_runner.go:681`):
1. `s.tableColumns(ctx, "migration_quarantine")`; error → `"ensure migration_quarantine table: %w"`.
2. `len(cols) == 0` → execute `quarantineTableStmt`.
3. `quarantineShapeMatches(cols)` → nil.
4. else `recreateQuarantineTable(ctx, cols)`.

`quarantineShapeMatches` (`migration_runner.go:702`): exact set equality — **neither a subset nor a superset** passes.

`recreateQuarantineTable` (`migration_runner.go:723`):
1. `SELECT COUNT(*) FROM migration_quarantine`; error → `"ensure migration_quarantine table: count rows in stale-shape table: %w"`.
2. `count > 0` → **refuse**: `"migration_quarantine has a non-canonical shape (columns: %s) and %d row(s) of history; refusing to recreate automatically — this needs manual triage, not self-heal"` with the columns sorted (`migration_runner.go:728-739`).
3. `count == 0` → `DROP TABLE migration_quarantine` (error `"...: drop stale-shape table: %w"`), then re-execute `quarantineTableStmt` (error `"...: recreate: %w"`).

### 8.8 Version-content drift detection

`alterAddColumnRe` (`migration_runner.go:926`), verbatim:
`(?is)ALTER\s+TABLE\s+`?([A-Za-z_][A-Za-z0-9_]*)`?\s+ADD\s+COLUMN\s+`?([A-Za-z_][A-Za-z0-9_]*)`?`

`sqlKeywordAfterAddColumn = map[string]bool{"if": true}` (`migration_runner.go:936`) — discards the `IF` of `ADD COLUMN IF NOT EXISTS` so it registers as unparsed rather than as a bogus column named `if`.

Documented deliberate gap (`migration_runner.go:919-925`): MySQL's `ADD <col> <type>` with no `COLUMN` keyword contains no `"ADD COLUMN"` text for the literal count to catch, and is ambiguous with `ADD CONSTRAINT`/`ADD INDEX`/`ADD KEY`/`ADD UNIQUE`. No migration in this registry uses it.

`parseAddColumnTargets(name, up)` (`migration_runner.go:970`):
1. For each regex match: lowercase the column; skip if it is in `sqlKeywordAfterAddColumn`.
2. `terminatedStatement(up, m[0])`; `!ok` → error `"migration %q: ADD COLUMN statement starting at byte %d has no terminating ';' — cannot isolate it for repair"`.
3. Append `tableColumnTarget{table: lowercased, column, stmt}`.
4. **Loud gate**: `strings.Count(strings.ToUpper(up), "ADD COLUMN") > len(adds)` → error `"migration %q: found %d \"ADD COLUMN\" occurrence(s) in its Up section but alterAddColumnRe recognized only %d — a form such as \"ADD COLUMN IF NOT EXISTS\" or a parenthesized multi-column list is not parsed; widen alterAddColumnRe or rewrite the migration to the plain ADD COLUMN <name> <type> shape"` (`migration_runner.go:987-996`).

Tests: `TestParseAddColumnTargetsRecognizesPlainForm` (`migration_runner_test.go:687`), `...CapturesEachStatementSeparately` (`:708`), `...ToleratesSemicolonInStringLiteral` (`:730`), `...RejectsUnrecognizedForm` (`:757`), `...RejectsMultiColumnForm` (`:771`).

`terminatedStatement(s, start)` (`migration_runner.go:1012`): scans forward tracking single-quote state; returns the trimmed text through the first **unquoted** `;`. Recognizes only doubled-quote (`''`) escaping, **not** backslash escapes (`migration_runner.go:1006-1011`).

`tableColumnTarget{table, column, stmt string}` (`migration_runner.go:946-954`) — `stmt` is the exact statement text, re-executed verbatim by the repair.
`migrationColumnAdds{version int64; name string; adds []tableColumnTarget}` (`migration_runner.go:940-944`).

`registryColumnAddsThroughVersion(maxVersion)` (`migration_runner.go:1036`):
1. `migrations.FS.ReadDir(".")`; error → `"read migration registry: %w"`.
2. Skip directories and non-`.sql` entries.
3. `migrations.ParseVersion(entry.Name())`; `!ok` → error `"migration file %q does not begin with a numeric version"`.
4. **Skip `v <= baselineVersion` and `v > maxVersion`** (`migration_runner.go:1050-1052`).
5. Read the file, `parseAddColumnTargets(name, gooseUpSection(string(data)))`.
6. Sort ascending by version (`migration_runner.go:1063`).

`missingVersionContent(ctx, appliedVersion)` (`migration_runner.go:1089`): for each registered target, look up (and cache per table) `s.tableColumns(target.table)`; a target whose column is absent becomes a `missingContentTarget{version, name, table, column, stmt}` (`migration_runner.go:1069-1075`). Returned in ascending version order with each version's targets contiguous.

`verifyAppliedVersionsMatchRegistry(ctx, appliedVersion)` (`migration_runner.go:1124`): reports only the **earliest** mismatched version — it walks the leading run of targets sharing `missing[0].version` and names them as `"<table>.<column>"` — returning `&VersionContentMismatchError{Version, Name, Missing}`.

`VersionContentMismatchError.Error()` (`migration_runner.go:206`):
`"migration v%d %q is recorded as applied, but its registered content is missing from this workspace's live schema: %s\n\nthis usually means the version number was reused for different historical content after this workspace last migrated — the recorded applied version does not reflect what actually ran here"`.

### 8.9 Version-content drift repair

`repairVersionContentDrift(ctx, appliedVersion)` (`migration_runner.go:1172`): executes **every** missing target's captured `stmt` verbatim, in the returned (ascending-version) order — not just the earliest version's. An `ExecContext` failure → `"repair v%d %q: apply %q: %w"`. Each success appends `fmt.Sprintf("%s.%s (v%d %s)", table, column, version, name)`.

**Documented scope limit** (`migration_runner.go:1162-1171`): only `ADD COLUMN` targets are ever tracked or repaired. A version's `ADD CONSTRAINT` or data-backfill statements — explicitly naming `00003_add_resolution.sql`'s `issues_resolution_check` and `00004`'s `redirect_target` backfill — are **never re-applied**, even when the column they depend on was just repaired. `CREATE TABLE`, `ADD CONSTRAINT`-only, `DROP`/`RENAME COLUMN`, and data-only migrations are not tracked as content to verify or repair.

`repairVersionContentDriftWithRollback(ctx, appliedVersion, mismatch)` (`migration_runner.go:1206`):
1. `s.CreateCheckpoint(ctx, "pre-drift-repair")`; failure → `"create version-content drift repair checkpoint: %w"`.
2. `repairVersionContentDrift`. On failure:
   - reset also fails → `"repair version-content drift (detected at v%d %q) failed (%v) and reset to checkpoint %q failed (%v); restore from dbsnapshot"` (`migration_runner.go:1213-1217`).
   - reset succeeds → `"repair version-content drift (detected at v%d %q) failed: %w (working set reset to checkpoint %q)"` (`migration_runner.go:1219-1222`).
3. `s.commitWorkingSet(ctx, migrationDriftRepairCommitMessage(repaired))`; failure → `"commit version-content drift repair: %w"`.
4. `s.PruneCheckpoints(ctx, "pre-drift-repair", 5)`; failure → `"prune version-content drift repair checkpoints: %w"` (**is** promoted).

`migrationDriftRepairCommitMessage(repaired)` = `fmt.Sprintf("migrate: repair version-content drift (%s)", strings.Join(repaired, ", "))` (`migration_runner.go:1192`).

Tests: `TestOpenRepairsVersionSlotReuseContentMismatch` (`migration_runner_test.go:573`), `TestRepairVersionContentDriftWithRollbackResetsOnFailure` (`migration_runner_test.go:644`).

### 8.10 Baseline shape verification

`baselineSchema()` (`migration_runner.go:1392`): `migrations.BaselineFileName()` → `migrations.FS.ReadFile(name)` (error `"read baseline migration %q: %w"`) → `parseCreateTableColumns(gooseUpSection(string(data)))`. Zero tables → error `"baseline migration %q defines no tables"`.

`gooseUpSection(sql)` (`migration_runner.go:1410`): finds the case-insensitive index of `"-- +goose up"`; if absent returns the whole input. Otherwise takes from that index, and truncates at the case-insensitive index of `"-- +goose down"` when present.

`parseCreateTableColumns(sql)` (`migration_runner.go:1428`): repeatedly finds `"create table"` case-insensitively, reads the first identifier as the table name, finds the `(`, extracts the balanced paren block, and maps lowercased table name → `columnNames(body)`. `CREATE INDEX` and everything else is ignored. ASCII lowercasing preserves byte indices so the keyword scan and the original-text slicing stay aligned (`migration_runner.go:1426-1427`).

`columnNames(body)` (`migration_runner.go:1454`): for each top-level item, take the first identifier; skip empty and `isConstraintKeyword`; lowercase.

`isConstraintKeyword(token)` (`migration_runner.go:1548`) — uppercased match against exactly: `CONSTRAINT`, `PRIMARY`, `FOREIGN`, `KEY`, `CHECK`, `UNIQUE`, `INDEX`.

`splitTopLevel(body)` (`migration_runner.go:1469`): splits at depth-0, unquoted commas; tracks `'` quoting and `(`/`)` depth.
`parenBlock(s)` (`migration_runner.go:1500`): quote- and depth-aware; unbalanced input yields an empty body and consumes `len(s)`.
`firstIdentifier(s)` (`migration_runner.go:1527`): skips leading whitespace, consumes `isIdentByte` bytes and backticks, then strips backticks.
`isIdentByte(b)` (`migration_runner.go:1539`): `_`, `a-z`, `A-Z`, `0-9`.

`verifyBaselineShape(ctx)` (`migration_runner.go:1311`): for each table in `sortedKeys(schema)` (deterministic, `migration_runner.go:1559`): `tableColumns(table)`; empty → append `"<table>"` to `missing` and continue (`present` NOT incremented); otherwise `present++` and for each expected column absent from the actual set append `"<table>.<column>"`. **Column NAMES only** are compared — not types, constraints, or indexes (`migration_runner.go:1305-1310`).

`tableColumns(ctx, table)` (`migration_runner.go:1337`): `SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`, lowercasing each name. An absent table yields an empty set.
`tableExists(ctx, table)` (`migration_runner.go:1361`): `SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1`; `sql.ErrNoRows` → `false, nil`.

`refuseIfBaselineMissing(ctx, state)` (`migration_runner.go:821`): `verifyBaselineShape`; refuses when `present == 0 || len(missing) > 0`, returning `&UnsupportedSchemaVersionError{WorkspaceVersion: state.appliedVersion, MaxSupported: state.registryMaxVers, MissingBaseline: missing, SnapshotName: s.mostRecentMigrationSnapshotName(), ProducerBinaryVersion: s.readProducerBinaryVersion(ctx)}`. **Performs no write.**

Tests: `TestOpenToleratesAheadOfRegistryWhenBaselineIntact` (`migration_runner_test.go:220`), `TestOpenToleratesGooseLogWithOnlyAheadRow` (`:255`), `TestOpenRefusesAheadOfRegistryWhenBaselineCorrupt` (`:292`), `TestOpenAllowsWorkspaceExactlyAtMax` (`:526`), `TestOpenToleratesHealthyManagedWorkspace` (`:784`).

### 8.11 Adoption

`adoptPreGooseWorkspace(ctx)` (`migration_runner.go:1260`), in order:
1. `database.NewStore(goose.DialectMySQL, "goose_db_version")`; error `"adopt: construct goose store: %w"`.
2. `store.CreateVersionTable(ctx, s.db)`; error `"adopt: create version table: %w"`.
3. `store.Insert(ctx, s.db, database.InsertRequest{Version: baselineVersion})`; error `"adopt: stamp baseline version: %w"`.
4. `DELETE FROM meta WHERE meta_key = 'schema_version'`; error `"adopt: drop legacy schema_version key: %w"`.

Tests: `TestPreGooseAdoptionStampsWithoutRerunningBaseline` (`migration_runner_test.go:63`), `TestAdoptionDeletesLegacySchemaVersionKey` (`:113`).

### 8.12 Producer-version stamp

`recordProducerBinaryVersion(ctx)` (`migration_runner.go:1615`): `version.Get()`; error → `"read version info: %w"`. `info.IsDev` → `(false, nil)` — a **dev build records no row**. Otherwise `s.setMeta(ctx, nil, "producer_binary_version", info.Version)` → `(true, nil)`; error → `"record producer binary version: %w"`. Test `TestProducerBinaryVersionUnstampedForDevBuild` (`migration_runner_test.go:502`).

`readProducerBinaryVersion(ctx)` (`migration_runner.go:1599`): `s.getMeta(ctx, nil, key)`; **any error degrades to `""`**.

`mostRecentMigrationSnapshotName()` (`migration_runner.go:1578`): `dbsnapshot.List(migrationSnapshotsDir(s.doltRootDir))`; a listing error degrades to `""`; otherwise the first entry passing `IsMigrationSnapshotName` (the list is newest-first).

### 8.13 Migration error types

`CheckpointResetError{Version int64; Name string; Checkpoint storage.Checkpoint; Cause error}` (`migration_runner.go:56-61`). `Error()` (`migration_runner.go:63`):
- `Version == 0` → `"migration failed (version unknown): %v\n\nthe working set was automatically reset to Dolt checkpoint %q\nrestore from the pre-migration recovery snapshot"`.
- else → `"migration v%d %q failed: %v\n\nthe working set was automatically reset to Dolt checkpoint %q\nto retry after fixing the migration, clear the quarantine:\n  DELETE FROM migration_quarantine WHERE version = %d"`.
`Unwrap()` at `migration_runner.go:81`.

`QuarantineBlockError{Version, Name, ErrorText}` (`migration_runner.go:88-92`). `Error()` (`migration_runner.go:94`):
`"migration v%d %q is quarantined after a previous failure:\n  %s\n\nto recover, either:\n  (a) restore from a dbsnapshot: lit snapshots restore <name>\n  (b) clear the quarantine row (if transient): DELETE FROM migration_quarantine WHERE version = %d"`.

`UnsupportedSchemaVersionError{WorkspaceVersion, MaxSupported int64; MissingBaseline []string; SnapshotName, ProducerBinaryVersion string}` (`migration_runner.go:121-137`). `Error()` (`migration_runner.go:139`) composes:
1. `"please upgrade lit (your workspace is at schema version %d; this binary supports up to %d"`.
2. If `len(MissingBaseline) > 0`: `"; the live schema is also missing baseline shape: %s"` (comma-joined).
3. `")"`.
4. If both `SnapshotName` and `ProducerBinaryVersion` are empty → return here.
5. If `SnapshotName != ""` → `"\n\nif you are stuck and need to roll back this workspace to match this binary:\n  lit snapshots restore %s\n\nthis is a LOSSY recovery — any data written under the newer binary will be discarded."`; else → `"\n\nno pre-upgrade snapshot available; lossy rollback is not possible from this workspace."`.
6. If `ProducerBinaryVersion != ""` → `"\n\nto operate this workspace, upgrade this binary to the version that wrote it:\n  lit upgrade --to %s\n\n(this is the supported path — `lit upgrade` runs even from this too-old binary and installs the newer one, which can then operate this workspace. snapshot-restore above is the unsupported, lossy escape hatch.)"`.
Tests: `TestUnsupportedSchemaVersionMessageShape` (`migration_runner_test.go:339`), `TestRefusalSurfacesRecoveryDataFromWorkspace` (`:444`).

### 8.14 The transient schema lift

`liftWorkingSetToRegistry(ctx)` (`migration_runner.go:513`):
1. `newGooseProvider(s.db)`; failure → `"construct schema-lift provider: %w"`.
2. Infinite loop calling the **bare** `s.upByOne(ctx, provider)` (never the test hook):
   - `goose.ErrNoNextVersion` → return nil.
   - other error → `"lift working set to registry schema: %w"`.
   - `result == nil` with nil error → **fails loud** rather than spinning: `"lift working set to registry schema: goose returned no result and no error"` (`migration_runner.go:526-534`).
It **commits nothing to Dolt, takes no checkpoint, and takes no snapshot** (`migration_runner.go:496-498`). Its only caller is the reconcile, on a throwaway scratch branch. Test `TestLiftWorkingSetToRegistryRecoversDowngradedSchema` (`sync_reconcile_schema_skew_test.go:28`).

---

## PART 9 — Migration snapshot guard (`internal/store/migrate_snapshot.go`)

### 9.1 Snapshot naming and classification

Snapshot names follow dbsnapshot's `<unix-ns>[-<label>]` scheme; the system stamps the label as `"<label>-<unix-ns>"` (`migrate_snapshot.go:38-47`).

`isStampedSnapshotName(name, label)` (`migrate_snapshot.go:67`) — the shared shape rule:
1. Find the first `'-'`; absent → false.
2. `head` (before it) must be all digits.
3. `rest` must have prefix `label + "-"`.
4. The remainder after that prefix must be all digits.
So the full shape is `<unix-ns>-<label>-<unix-ns>`.

`isAllDigits(s)` (`migrate_snapshot.go:87`): non-empty and every rune in `'0'..'9'`.

Three disjoint classifiers share it: `IsMigrationSnapshotName` (label `"pre-migrate"`, `migrate_snapshot.go:55`), `IsDowngradeSnapshotName`, and `IsReconcileSnapshotName` (label `"pre-reconcile"`, `sync_reconcile.go:131`). Because the labels differ, the three retention budgets never collect each other's snapshots (`migrate_snapshot.go:59-66`).

Tests: `TestIsMigrationSnapshotNameRejectsUserCollisions` (`migrate_snapshot_test.go:345`), `TestMigrationPruneSparesUserSnapshots` (`migrate_snapshot_test.go:280`), `TestIsReconcileSnapshotNameDisjoint` (`sync_reconcile_schema_skew_test.go:342`).

### 9.2 `snapshotGuard`

`snapshotGuard{databaseDir, snapshotsDir, label string; taken *dbsnapshot.Snapshot}` (`migrate_snapshot.go:109-114`); `newSnapshotGuard` (`migrate_snapshot.go:116`).

`ensure(ctx)` (`migrate_snapshot.go:126`): if `taken != nil` return the cached snapshot. Otherwise `dbsnapshot.Take(ctx, databaseDir, snapshotsDir, label)`; failure → `"snapshot before migration: %w"`. **Idempotent within one invocation** — this is what makes exactly one snapshot per reconcile survive N GC-contention retries.

`took()` (`migrate_snapshot.go:141`): `(Snapshot, bool)` discriminating "we have a recovery point" from "no mutation happened".

Helpers must not call `dbsnapshot.Take` directly (`migrate_snapshot.go:104-105`).

### 9.3 Paths and labels

`migrationSnapshotsDir(databaseDir)` (`migrate_snapshot.go:177`) = `filepath.Join(filepath.Dir(filepath.Clean(databaseDir)), "snapshots")` — a **sibling** of the dolt directory, matching the commit-lock's sibling convention.

`formatMigrationSnapshotLabel(t)` (`migrate_snapshot.go:186`) = `fmt.Sprintf("pre-migrate-%d", t.UTC().UnixNano())`.

### 9.4 `MigrationRollbackError`

`{Snapshot dbsnapshot.Snapshot; Cause error}` (`migrate_snapshot.go:155-158`). `Error()` (`migrate_snapshot.go:160`):
`"migrate: %v\n\nthe workspace state before this migration is preserved at:\n  %s\n\nto restore, run:\n  lit snapshots restore %s"` with `Snapshot.Path` and `Snapshot.Name`. `Unwrap()` at `migrate_snapshot.go:167`. `asMigrationRollbackError(err)` (`migrate_snapshot.go:193`) is the centralized `errors.As`.

Tests: `TestMigrateSnapshotFreshDBOpenTakesExactlyOneSnapshot` (`migrate_snapshot_test.go:24`), `TestMigrateSnapshotNoOpOpenTakesNoSnapshot` (`:53`), `TestMigrateSnapshotFailureSurfacesRestoreCommand` (`:91`), `TestMigrateSnapshotRestoreRoundTripsPreMutationState` (`:127`), `TestMigrateSnapshotPruneEnforcesRetention` (`:213`), `TestDataSurvivesFailedMigrationSnapshotRestore` (`:381`).

---

## PART 10 — Dolt checkpoints (`internal/store/checkpoint.go`)

- `CreateCheckpoint(ctx, prefix)` (`checkpoint.go:17`): `readDoltHead` → commit SHA (error `"checkpoint: %w"`); `ts := time.Now().UTC()`; `name := fmt.Sprintf("%s-%d", prefix, ts.UnixNano())`; `CALL DOLT_BRANCH(?)` with that name (error `"checkpoint: create branch %q: %w"`). Returns `storage.Checkpoint{Name, Prefix, CreatedAt: ts, Anchor: commitSHA}`.
- `ResetToCheckpoint(ctx, name)` (`checkpoint.go:45`): `CALL DOLT_RESET('--hard', ?)`; error `"checkpoint: reset to %q: %w"`. Discards working-set changes **and** any Dolt commits made after the checkpoint.
- `ListCheckpoints(ctx, prefix)` (`checkpoint.go:54`): `SELECT name, hash FROM dolt_branches WHERE name LIKE ? ORDER BY name` with `prefix+"-%"`. Names failing `parseCheckpointName` are **skipped silently**. `Anchor` set from `hash`. Final sort is by `CreatedAt` ascending (oldest first).
- `PruneCheckpoints(ctx, prefix, retain)` (`checkpoint.go:85`): `retain < 0` → `"checkpoint: retain must be non-negative, got %d"`. `len(cps) <= retain` → nil. Otherwise deletes `cps[:len(cps)-retain]` (the oldest) with `CALL DOLT_BRANCH('-d', '-f', ?)`; error `"checkpoint: delete branch %q: %w"`. `retain=0` deletes all.
- `parseCheckpointName(name, prefix)` (`checkpoint.go:106`): requires the exact prefix + `'-'`, and the suffix must round-trip through `%d` unchanged (`fmt.Sprintf("%d", ns) != suffix` → reject). `CreatedAt = time.Unix(0, ns).UTC()`.

Retention in use: `migrationCheckpointRetention = 5` for both `"pre-migrate"` and `"pre-drift-repair"` prefixes (`migration_runner.go:32`, `:584`, `:596`, `:1227`).

---

## PART 11 — `internal/merge`: every merge and conflict rule

Files: `/Users/bmf/code/links-issue-tracker/internal/merge/merge.go`, `resolve.go`, `resolve_prose.go`.

### 11.1 Signatures

merge.go: `MergeResult struct{ export model.Export; Pending []ProsePending }` (merge.go:19-22); `(MergeResult) Settled() (model.Export, bool)` (merge.go:29); `(MergeResult) Provisional() model.Export` (merge.go:37); `ThreeWay(base, local, remote model.Export) MergeResult` (merge.go:41); `mapIssues` (:115); `optionalIssuePtr` (:123); `unionIssueIDs` (:131); `issueChanged` (:146); `issueEqual` (:150); `issueProjection` struct (:160-179); `issueProjectionFrom` (:181); `mergeRelations` (:207); `enforceSingleParent` (:246); `breakParentCycles` (:272); `maxString` (:304); `mergeComments` (:314); `mergeLabels` (:337); `mergeEvents` (:391); `maxInt` (:407).

resolve.go: `ProseField string` (:15) with `ProseTitle="title"` (:18), `ProseDescription="description"` (:19), `ProsePrompt="agent_prompt"` (:20); `ProsePending{IssueID,Field,Base,Ours,Theirs}` with JSON tags `issue_id/field/base/ours/theirs` (:26-32); `IssueResolution{merged model.Issue; Pending []ProsePending}` (:41-44) with `Settled()` (:50) and `Provisional()` (:60); `ResolveIssue(base, ours, theirs *model.Issue, oursWS, theirsWS string) IssueResolution` (:77); `resolver` struct (:139-147); `twoTier[T comparable]` (:155); `(*resolver).prose` (:174); `(*resolver).resolveStatus` (:187); `(*resolver).tiebreak` (:231); `typedTiebreak[T ~string]` (:247); `resolveClosePayload` (:264); `closePayload` (:310-313); `closePayloadOf` (:319); `(*resolver).derivedFlagTime` (:336); `higher[T cmp.Ordered]` (:346); `stateRank` (:353); `stateFromRank` (:364); `boolBase` (:375); `mergeLabelNames` (:386); `presentOr` (:404); `nameSet` (:406); `unionNameSet` (:414); `earliest` (:424); `latest` (:431); `earliestTime` (:438); `cloneTimePtr` (:451).

resolve_prose.go: `(ProsePending) Fingerprint() string` (:18); `ProseResolution{IssueID, Field, Fingerprint, Text}` — **no JSON tags** (:33-38); `proseKey{IssueID, Field}` (:44-47); `ApplyProseResolutions(result MergeResult, resolutions []ProseResolution) (model.Export, bool)` (:62); `applyIssueProse` (:113); `SortPending` (:128).

### 11.2 `ThreeWay` — export-level algorithm

1. Three id→issue maps from `base.Issues`, `local.Issues`, `remote.Issues`; a duplicate id within one export lets the **last row in slice order** win (merge.go:42-44, :115-121).
2. Candidate id set = union of all three, **sorted ascending** (merge.go:46, :131-144).
3. Per id, a copied `*model.Issue` per side, `nil` when absent (merge.go:54-56, :123-129).
4. `localChanged := issueChanged(base, local)`, `remoteChanged := issueChanged(base, remote)` (merge.go:58-59).

The four-way presence/change branch (merge.go:61-93):

| Condition | Result |
|---|---|
| neither changed | append `baseIssue` if `hasBase`; else nothing (merge.go:62-65) |
| only local changed | append `localIssue` if present; else nothing — local deleted, remote untouched → row dropped (merge.go:66-69) |
| only remote changed | append `remoteIssue` if present; else nothing (merge.go:70-73) |
| both changed, both present | `ResolveIssue(basePtr, localPtr, remotePtr, local.WorkspaceID, remote.WorkspaceID)`; append `resolution.Provisional()` and append its `Pending...` to the export pending list (merge.go:81-84) |
| both changed, local only | append `localIssue` — remote removed it, local edited → the surviving edit is preserved (merge.go:85-87) |
| both changed, remote only | append `remoteIssue` (merge.go:88-90) |
| both changed, neither present | append nothing — converged removal (merge.go:91) |

Note: `ours` = local, `theirs` = remote; `oursWS = local.WorkspaceID`, `theirsWS = remote.WorkspaceID` (merge.go:82). Deletion here is whole-row absence; soft deletion is a `DeletedAt` stamp on a present row and travels through the retention field instead (merge.go:75-79).

### 11.3 Change detection

`issueChanged = !issueEqual` (merge.go:146-148). `issueEqual`: both nil → true; exactly one nil → false; else `reflect.DeepEqual` of the two `issueProjection`s (merge.go:150-158).

`issueProjection` (merge.go:160-179), built by `issueProjectionFrom` (merge.go:181-205), captures: `ID`, `Title`, `Description`, `Prompt`, `Priority`, `IssueType`, `Topic`, `Assignee` (via `issue.AssigneeValue()`), `Rank`, `Lane`, `Labels` (copied via `append([]string{}, issue.Labels...)`, normalizing `nil` and `[]string{}` to the same value), `CreatedAt`, `UpdatedAt`, `Retention` (via `issue.Retention()`), `Capabilities` (via `issue.Capabilities()`).

`Capabilities` carries the whole leaf lifecycle payload — status value, `closed_at`, `resolution`, `redirect_target` (`model.StatusView`, `/Users/bmf/code/links-issue-tracker/internal/model/capabilities.go:20-28`) — so a change to any of those four registers as "this side moved" (merge.go:175-178). `issue.Capabilities()` returns the empty `Capabilities{}` for containers (`/Users/bmf/code/links-issue-tracker/internal/model/model.go:244-249`); for an unhydrated leaf it **panics** (`model.go:432-434`), so `ThreeWay` panics on an unhydrated leaf issue.

Tests: `merge_test.go:115-125` (nil vs empty `Labels` compare equal, so JSON round-trip drift does not synthesize changes); `merge_test.go:148-155` (`issueChanged` is true for a resolution-only re-close `wontfix`→`duplicate` at identical status and `closed_at`); `merge_test.go:88-113` (a JSON-round-tripped hydrated **epic** compares clean); `resolve_test.go:560-583` (a `Prompt`-only or `Lane`-only edit is detected and survives).

### 11.4 Merged export assembly (merge.go:96-113)

- `mergedIssues` sorted ascending by `ID` (merge.go:96).
- `issueSet` = surviving issue ids; it gates every side-table (merge.go:97-100).
- `Version = maxInt(local.Version, remote.Version, base.Version)` (merge.go:103; `maxInt` returns 1 for an empty argument list, merge.go:408-410).
- `WorkspaceID = local.WorkspaceID` — remote's is discarded (merge.go:104).
- `ExportedAt = local.ExportedAt` — remote's is discarded (merge.go:105).
- `Relations = mergeRelations(issueSet, local.Relations, remote.Relations)` — **base not passed** (merge.go:107).
- `Comments = mergeComments(issueSet, local.Comments, remote.Comments)` — base not passed (merge.go:108).
- `Labels = mergeLabels(issueSet, base.Labels, local.Labels, remote.Labels)` — the only three-way side-table (merge.go:109).
- `Events = mergeEvents(issueSet, local.Events, remote.Events)` — base not passed (merge.go:110).

`Settled()` returns `(export, len(Pending)==0)` (merge.go:29-31); `Provisional()` returns the export unconditionally (merge.go:37-39). The export field is unexported, so those two methods are the only access. Test `resolve_test.go:537-558`.

### 11.5 Side-table rules

**Relations** (`mergeRelations`, merge.go:207-237): key is the triple `{SrcID, DstID, Type}` (merge.go:208-211). Iterates `append(locals, remotes...)` — locals first — writing into `merged[key]`, so on an identical key the **remote row wins** its `CreatedAt`/`CreatedBy` (merge.go:213, :220). Referential filter: dropped unless **both** `SrcID` and `DstID` are in `issueSet` (merge.go:214-219). Then `enforceSingleParent` (merge.go:226), then sort by `SrcID`, `DstID`, `Type` (merge.go:227-235). `Type` values: `"blocks"`, `"parent-child"`, `"related-to"` (`/Users/bmf/code/links-issue-tracker/internal/model/relation_type.go:17-19`). `blocks` and `related-to` are purely additive — test `resolve_test.go:172-202` asserts a local `blocks` edge and a remote `related-to` edge both survive (2 relations).

**Single parent** (`enforceSingleParent`, merge.go:246-264): non-`parent-child` relations pass straight through (merge.go:250-253). Parent-child edges are keyed by `SrcID` (the child); the child keeps exactly one edge — the one with the **lexicographically greatest `DstID`** (`!seen || relation.DstID > existing.DstID`) (merge.go:254-257). Order-independent. Then `breakParentCycles(parentOf)` (merge.go:259) and the survivors are appended (merge.go:260-262); the caller sorts. Test `resolve_test.go:660-683`.

**Cycle breaking** (`breakParentCycles`, merge.go:272-302): walks parent edges from each unsettled key (merge.go:279-281); a walk stops at a node with no parent edge or already `settled` (merge.go:286-288). On revisiting a node already on the current path at index `idx`, it **deletes the map entry keyed by `maxString(path[idx:])`** — the lexicographically greatest child id inside the loop — and stops (merge.go:290-293). The victim child becomes a root; nothing is reparented. Every node on the path is then marked `settled` (merge.go:298-300). `maxString` panics on an empty slice (merge.go:304-312) but is only ever called with the non-empty `path[idx:]`. Tests: `merge_test.go:238-258` (tail-entered cycle `a→b→c→b` → `c`'s edge deleted, `a→b`/`b→c` untouched); `merge_test.go:260-279` (clean 3-cycle `a→b→c→a` → exactly one edge removed, victim `c`); `resolve_test.go:630-658` (end-to-end: local `a→b`, remote `b→a` → exactly one parent edge).

**Comments** (`mergeComments`, merge.go:314-328): two-way union keyed by `Comment.ID`; locals then remotes, so **remote wins an id collision** (merge.go:316-321); dropped unless `IssueID ∈ issueSet` (merge.go:317-319); sorted by `ID` (merge.go:326). Test `resolve_test.go:608-628`.

**Events** (`mergeEvents`, merge.go:391-405): identical shape — union keyed by `IssueEvent.ID`, remote wins collisions, dropped unless `IssueID ∈ issueSet`, sorted by `ID`.

**Labels table** (`mergeLabels`, merge.go:337-389) — the only three-way side-table. Key `{IssueID, Name}` (merge.go:338-339). `baseSet`/`localSet`/`remoteSet` computed (merge.go:340-347). Metadata rows: locals first then remotes, so **remote wins ties** on `CreatedAt`/`CreatedBy` (merge.go:351-357). Candidate keys = base ∪ local ∪ remote (merge.go:359-364); dropped unless `IssueID ∈ issueSet` (merge.go:368-370). Membership: `twoTier(true, inBase, inLocal, inRemote, presentOr)` — `hasBase` **hardcoded `true`**, so an empty base makes every present label an add (merge.go:376-377). A key passing the membership test but with no row in `rows` (i.e. only in base) contributes nothing (merge.go:378-380). Sorted by `IssueID`, `Name` (merge.go:382-387). Test `resolve_test.go:482-507`: base `[a,b]`, local `[a]`, remote `[a,b]` → merged table is `[a]`; `b` is not resurrected.

### 11.6 `ResolveIssue` — per-row field merge

`ResolveIssue(base, ours, theirs *model.Issue, oursWS, theirsWS string)` (resolve.go:77). `base == nil` means no merge-base; `hasBase = base != nil` (resolve.go:78-81). `r.id = ours.ID` (resolve.go:84). `ours` and `theirs` are dereferenced unconditionally (resolve.go:82-83), so both must be non-nil.

**The one merge primitive — `twoTier`** (resolve.go:155-168):
```
oursChanged   := !hasBase || ours != base
theirsChanged := !hasBase || theirs != base
```
| Case | Result |
|---|---|
| ours moved, theirs didn't | `ours` (resolve.go:159-160) |
| theirs moved, ours didn't | `theirs` (resolve.go:161-162) |
| neither moved | `ours` (resolve.go:163-164) |
| both moved | `tier2(ours, theirs)` (resolve.go:165-166) |

With `hasBase == false`, both sides count as changed, so **every field goes straight to Tier 2** (resolve.go:156-157). Tier 2 is entered even when both sides moved to the same value, so every tier2 function must be idempotent on equal inputs — all of them are (`higher`, `tiebreak`, `presentOr`, the prose closure).

**Type resolution and basis selection**: `merged.IssueType = twoTier(hasBase, base.IssueType, ours.IssueType, theirs.IssueType, typedTiebreak[model.IssueType](&r))` (resolve.go:86, :97). The merged row is built by copying **`ours`**, except when the resolved type differs from `ours.IssueType` and equals `theirs.IssueType`, in which case **`theirs`** is the basis (resolve.go:91-95). Consequence: every field `ResolveIssue` does not explicitly re-merge is inherited verbatim from that basis side — lifecycle, and (for containers) status/assignee/close payload.

**Field-by-field** (resolve.go:97-134):

| Field | Base operand | Tier 2 policy | Line |
|---|---|---|---|
| `IssueType` | `base.IssueType` | symmetric workspace tiebreak | :86, :97 |
| `Title` | `base.Title` | prose → `ProsePending{Field:"title"}`, returns `ours` provisionally | :98 |
| `Description` | `base.Description` | prose → `ProsePending{Field:"description"}` | :99 |
| `Prompt` | `base.Prompt` | prose → `ProsePending{Field:"agent_prompt"}` | :100 |
| `Priority` | `base.Priority` | `higher` — numerically greater wins; `PriorityUrgent=1` beats `PriorityNormal=0` (`/Users/bmf/code/links-issue-tracker/internal/model/priority.go:15-16`) | :101 |
| `Topic` | `base.Topic` | symmetric workspace tiebreak | :102 |
| `Lane` | `base.Lane` | symmetric workspace tiebreak | :103 |
| `Rank` | `base.Rank` | symmetric workspace tiebreak | :104 |
| `Labels` | `base.Labels` | per-name two-tier, `presentOr` at Tier 2 | :105 |
| `ID` | — | always `ours.ID`; never merged | :110 |
| `CreatedAt` | — | `base.CreatedAt` when `hasBase`; else `earliest(ours, theirs)` | :111-115 |
| `UpdatedAt` | — | always `latest(ours.UpdatedAt, theirs.UpdatedAt)`; never two-tier | :116 |
| retention | per-flag booleans | `presentOr`, timestamp slaved | :122-128 |
| status/assignee/closed_at/resolution/redirect_target | see below | leaves only | :130-134 |

When `hasBase == false`, `r.base` is the zero `model.Issue`, so all base operands are zero values — and they are ignored anyway since both sides count as changed (resolve.go:78-81, :156-157).

**Prose** (`(*resolver).prose`, resolve.go:174-182): Tier 1 takes whichever single side moved the text off base, with **no** agent involvement and no pending entry. Tier 2 (both moved): if `ours == theirs`, the agreed text is returned with **no** pending entry (resolve.go:176-178). Otherwise a `ProsePending{IssueID: r.id, Field: field, Base: base, Ours: ours, Theirs: theirs}` is appended and **`ours` is returned as a provisional value** (resolve.go:179-180). The engine never picks a prose winner. Pending entries are appended in field order title → description → prompt (resolve.go:98-100).

**Label names** (`mergeLabelNames`, resolve.go:386-402): converts base/ours/theirs to sets (:387), iterates the union (:389), applies `twoTier(hasBase, inBase, inOurs, inTheirs, presentOr)` per name (:393). Semantics: added by either → kept; removed by exactly one while the other left it → **stays removed**; removed by both → removed; both add → kept. Returns `nil` (not an empty slice) when nothing survives (:397-399); else sorted ascending (:400). Tests `resolve_test.go:454-468` (base `[keep]`, ours `[keep,ours]`, theirs `[keep,theirs]` → `[keep, ours, theirs]`), `resolve_test.go:470-480`.

**Leaf lifecycle** (`resolveStatus`, resolve.go:187-226) — runs only when `!mergedType.IsContainer()` (resolve.go:132-134; `IsContainer()` is true only for `epic`, `/Users/bmf/code/links-issue-tracker/internal/model/issue_type.go:56-58`):
- Base operands read only when `hasBase`; else `baseState = 0`, `baseAssignee = ""` (resolve.go:191-196).
- **status**: `stateFromRank(twoTier(hasBase, baseRank, rank(ours), rank(theirs), higher))` (resolve.go:198). Ranks: `closed = 2`, `in_progress = 1`, everything else `0` → `open` (resolve.go:353-373). Tier 2 is the dominant-state join.
- **assignee**: `twoTier(hasBase, baseAssignee, ours.AssigneeValue(), theirs.AssigneeValue(), r.tiebreak)` written to `merged.Assignee` (resolve.go:199-201).
- **closed_at / resolution / redirect_target**: all three stay `nil` unless the merged state is `closed` (resolve.go:203-217). When closed: `closedAt = earliestTime(ours.ClosedAtValue(), theirs.ClosedAtValue())` — earliest non-nil, cloned (resolve.go:207, :438-457); `resolution, redirectTarget = resolveClosePayload(ours, theirs, r.tiebreak)` (resolve.go:216).
- Re-hydration via `model.HydrateStatus(merged, model.StatusView{...})` (resolve.go:219); a returned error **panics** (resolve.go:220-224).
- A merged **container** inherits its lifecycle, status, assignee, and close payload untouched from the basis side — never merged.

Status table test `TestResolveIssueStatusTwoTier` (resolve_test.go:36-62), all asserting zero pending:

| base | ours | theirs | want |
|---|---|---|---|
| in_progress | in_progress | in_progress | in_progress |
| closed | open | closed | **open** (reopen via Tier 1) |
| open | open | in_progress | in_progress |
| open | in_progress | closed | closed |
| closed | open | in_progress | in_progress (both moved off closed; higher rank) |

Also: `resolve_test.go:64-72` (priority base normal / ours urgent / theirs normal → urgent); `resolve_test.go:204-229` (both closed at t2/t1 → `closed_at` = t1; reopen → `closed_at` **nil** even though theirs carries one); `resolve_test.go:509-520` (`ID` stays `i1`, `CreatedAt` stays base `t0` even when both sides moved it); `resolve_test.go:522-535` (`base = nil`: `in_progress` vs `closed` → closed, and the diverged title yields exactly one `ProseTitle` pending).

**Tiebreak** (`(*resolver).tiebreak`, resolve.go:231-242): if `oursWS != theirsWS`, the value from the **lexicographically greater workspace id** wins (`oursWS > theirsWS` → `ours`, else `theirs`). If the workspace ids are equal (the code calls this "defensive"), the **lexicographically greater value** wins (`ours >= theirs` → `ours`, else `theirs`). Symmetric by construction. `typedTiebreak[T ~string]` wraps it for named string types (resolve.go:247-251). Tests `resolve_test.go:123-140` (topic base `root`, ours `alice`@wsA, theirs `bob`@wsB → `bob`, same under argument swap), `resolve_test.go:142-170` (same for `Assignee`).

**Close payload atom** (`resolveClosePayload`, resolve.go:264-302; `closePayloadOf`, resolve.go:319-329): `closePayloadOf` projects a side to `{resolution, target}`; if `ResolutionValue()` is nil the whole payload is empty and `target` is read only under a non-nil resolution — so `{resolution:"", target:"x"}` is unconstructible. Branch order (resolve.go:267-292):
1. `o == t` (struct equality) → that payload (:269-270).
2. `o.resolution == ""` → `t` wins (:271-272).
3. `t.resolution == ""` → `o` wins (:273-274).
4. resolutions differ → `tiebreak(o.resolution, t.resolution)`; the winning resolution's **own** payload, target included, is taken whole (:275-280).
5. same resolution, `o.target == ""` → `t` (:283-284).
6. same resolution, `t.target == ""` → `o` (:285-286).
7. same resolution, two real targets → `tiebreak(o.target, t.target)` selects the whole payload (:287-291).
Return `(nil, nil)` when the winning resolution is empty; else `(*model.Resolution, *string)` with `target` nil when the winner's target is empty (:293-301). It never mixes one side's resolution with the other side's target.

`TestResolveClosePayloadAtomicity` (resolve_test.go:246-326) runs each row in **both** argument orders (:320-323) with a "larger string wins" stand-in tiebreak (:251-256):

| ours | theirs | want resolution | want target |
|---|---|---|---|
| closed, none | closed, none | `""` | `""` |
| closed, none | duplicate/`links-canon` | `duplicate` | `links-canon` |
| obsolete/`""` | duplicate/`links-canon` | `obsolete` | `""` |
| duplicate/`""` | duplicate/`links-canon` | `duplicate` | `links-canon` |
| duplicate/`links-aaa` | duplicate/`links-bbb` | `duplicate` | `links-bbb` |
| duplicate/`links-canon` | duplicate/`links-canon` | `duplicate` | `links-canon` |

End-to-end guard `resolve_test.go:332-355`: open-vs-redirecting-close → closed with `redirect_target = links-canon`; a reopen winning on state → `RedirectTargetValue() == nil`.

**Retention** (resolve.go:118-128, :336-342): each side's retention is projected to the two-timestamp wire pair via `model.RetentionTimestamps` (resolve.go:122-124; encoder at `/Users/bmf/code/links-issue-tracker/internal/model/lifecycle/retention.go:136-150`). Each flag merges independently through `derivedFlagTime(base bool, ours, theirs *time.Time)`: `set := twoTier(hasBase, base, ours != nil, theirs != nil, presentOr)`; `!set` → `nil`; else `earliestTime(ours, theirs)` (resolve.go:336-342). The timestamp is **derived** from the resolved flag, never merged on its own. `boolBase(t, hasBase) = hasBase && t != nil` (resolve.go:375-377). The pair is folded back via `model.RetentionFromTimestamps`, whose rule is `deletedAt != nil → Deleted; archivedAt != nil → Archived; else Live` — **deletion dominates** (`/Users/bmf/code/links-issue-tracker/internal/model/lifecycle/retention.go:120-131`). Retention merges for containers as well as leaves — it runs before the `IsContainer` gate (resolve.go:122-134).

`TestResolveIssueRetentionRaces` (resolve_test.go:381-452):

| scenario | result |
|---|---|
| no base; ours Archived@t1, theirs Deleted@t2 | `Deleted{At: t2}` |
| live base; ours Archived@t1, theirs Deleted@t2 | `Deleted{At: t2}` |
| no base; Deleted@t2 vs Deleted@t1 | `Deleted{At: t1}` (earliest) |
| Deleted@t1 base; ours restored, theirs unchanged | `Live` |
| Archived@t1 base; ours unarchived, theirs unchanged | `Live` |
| Archived@t1 base; both unarchived | `Live` (convergent clear via Tier 2) |
| Deleted@t1 base; both restored | `Live` |

Plus `resolve_test.go:357-374`: both archive (t2 vs t1) off a live base → `Archived{At: t1}`; only ours archives at t2 → `Archived{At: t2}`.

**Time helpers**: `earliest(a,b)` = `a.Before(b) ? a : b`, ties yield `b` (resolve.go:424-429). `latest(a,b)` = `a.After(b) ? a : b`, ties yield `b` (resolve.go:431-436). `earliestTime(a,b *time.Time)` is nil-tolerant and always returns a fresh pointer via `cloneTimePtr` (resolve.go:438-457). `higher[T cmp.Ordered]` = `ours >= theirs ? ours : theirs` (resolve.go:346-351).

`IssueResolution.Settled()` → `(merged, len(Pending)==0)` (resolve.go:50-52); `Provisional()` → `merged` unconditionally (resolve.go:60-62); `merged` is unexported. Test `resolve_test.go:87-104`.

### 11.7 Prose resolution surface (`resolve_prose.go`)

**There is no text-diff machinery in this package.** No line-level or hunk-level diffing, no `<<<<<<<`/`=======`/`>>>>>>>` conflict markers, no diff3, no similarity heuristics. A prose conflict is whole-field: the three complete strings travel in `ProsePending{Base, Ours, Theirs}` (resolve.go:26-32, populated at resolve.go:179), and the agent's answer is one complete replacement string `ProseResolution.Text` (resolve_prose.go:37) assigned wholesale to the field (resolve_prose.go:115-117).

**`Fingerprint()`** (resolve_prose.go:18-21): `hex.EncodeToString(sha256.Sum256([]byte(string(p.Field) + "\x00" + p.Base + "\x00" + p.Ours + "\x00" + p.Theirs))[:6])` — a **12-hex-character** truncation of SHA-256 over the field name and the three texts joined by NUL bytes. **`IssueID` is not part of the digest.**

**`ApplyProseResolutions(result, resolutions) (model.Export, bool)`** (resolve_prose.go:62-107):
1. Builds `pendingByKey map[proseKey]string` from `result.Pending`: key `{IssueID, Field}` → live `Fingerprint()` (resolve_prose.go:66-69). Duplicate pending keys collapse (last wins).
2. Per supplied resolution, keyed by `{IssueID, Field}`:
   - **Reject** (`return model.Export{}, false`) if the key is not in the pending set, or `resolution.Fingerprint != liveFingerprint` (resolve_prose.go:78-81).
   - **Reject** if the same key already appeared in this call — a duplicate resolution for one field (resolve_prose.go:87-89).
   - Else record `resolvedByKey[key] = resolution.Text` (resolve_prose.go:90).
3. **Reject** if `len(resolvedByKey) != len(pendingByKey)` — the bijection/completeness gate catching a partial set (resolve_prose.go:95-97).
4. On success: takes `result.Provisional()`, allocates a **fresh** `[]model.Issue` and copies the slice (resolve_prose.go:99-102), applies `applyIssueProse` to each element (:103), assigns the new slice, returns `(export, true)` (:105-106). The original `MergeResult`'s issue slice is not mutated; Relations/Comments/Labels/Events are shared by reference with the provisional export.
5. Pure — no IO, no clock (resolve_prose.go:60-61).

Every rejection returns the **zero** `model.Export{}` paired with `false` (resolve_prose.go:80, :88, :96) — never a partially-spliced export.

**`applyIssueProse`** (resolve_prose.go:113-123): the single `ProseField` → field mapping as a map of setter closures — `ProseTitle → issue.Title`, `ProseDescription → issue.Description`, `ProsePrompt → issue.Prompt`. A field is written only if `resolved[{issue.ID, field}]` exists.

**`SortPending`** (resolve_prose.go:128-138): copies the slice and sorts by `IssueID`, then `Field` (raw string, so `"agent_prompt" < "description" < "title"`). Does not mutate the input. Not called anywhere inside the package.

Prose tests (`resolve_prose_test.go`), fixture `prosePendingFixture` (`:13-29`) = one issue `i1` with concurrent title *and* description rewrites → exactly 2 pending:
- `:44-64` — exact bijection with live fingerprints splices `merged-title`/`merged-desc`, and the original provisional export is asserted **not** mutated in place.
- `:66-73` — one resolution for a two-field pending set → rejected.
- `:75-85` — fingerprint `"deadbeefcafe"` on an otherwise-valid key → whole set rejected.
- `:87-97` — a resolution for `ProsePrompt`, which is not pending → rejected.
- `:99-112` — two resolutions for the same pending field (both correctly fingerprinted) plus the second field → rejected; the comment notes the count gate alone cannot catch this.
- `:114-122` — resolution for `IssueID: "nope"` → rejected.

### 11.8 Errors, sentinels, aborts in `internal/merge`

The package declares **no** error type, no sentinel `var Err...`, and **no function returns `error`**. Every failure is a `bool` second return: `MergeResult.Settled` (merge.go:29), `IssueResolution.Settled` (resolve.go:50), `ApplyProseResolutions` (resolve_prose.go:62).

The only explicit abort is `panic(err)` when `model.HydrateStatus` returns an error inside `resolveStatus` (resolve.go:219-224). Implicit panics reachable from this package: `maxString` on an empty slice (merge.go:305, only called with non-empty `path[idx:]`); `ResolveIssue` dereferencing a nil `ours`/`theirs` (resolve.go:82-84, guarded by `ThreeWay` at merge.go:81-82); `issueProjectionFrom → issue.Capabilities() → mustLifecycle` on an unhydrated non-container (`/Users/bmf/code/links-issue-tracker/internal/model/model.go:244-252, 428-435`), reached from `issueChanged` for every id (merge.go:58-59); `merged.SetRetention(...)` on an illegal retention variant (`model.go:133-140`), called at resolve.go:125.

### 11.9 Ordering and determinism

- Candidate issue ids sorted ascending before iteration (merge.go:142, :46-50) — pending order and merge order are deterministic.
- Merged issues sorted by `ID` (merge.go:96); relations by `(SrcID, DstID, Type)` (merge.go:227-235); comments by `ID` (merge.go:326); events by `ID` (merge.go:403); label table by `(IssueID, Name)` (merge.go:382-387); label names on a row ascending (resolve.go:400); `SortPending` by `(IssueID, Field)` (resolve_prose.go:131-136).
- Every Tier-2 policy is symmetric in its two inputs — `higher` (resolve.go:346), `presentOr` (resolve.go:404), workspace `tiebreak` (resolve.go:231-242), `resolveClosePayload` (resolve.go:264, verified in both orders at resolve_test.go:320-323) — so both machines compute the same winner without a clock and without knowing which side is "ours".
- Causality comes from the merge-base, not timestamps: `UpdatedAt`/`CreatedAt` are never consulted to pick a field winner; they are themselves outputs (resolve.go:111-116, stated at resolve.go:73-76).
- The single-parent winner (`max DstID`, merge.go:255) and the cycle victim (`max child id in the loop`, merge.go:291) are order-independent functions of the data.
- Non-deterministic residue: map-iteration order feeds `enforceSingleParent` (merge.go:223-226) and `breakParentCycles`' start-node choice (merge.go:279); both are followed by an order-independent selection rule and a final sort.

### 11.10 Additional test-pinned merge behaviors

- `merge_test.go:33-51` — concurrent title rewrite through `ThreeWay` yields exactly one pending with `IssueID="i1"`, `Field=ProseTitle`, `Base="issue"`, `Ours="local-change"`, `Theirs="remote-change"`.
- `merge_test.go:57-86` — with a completely **empty base** `model.Export{}`, `ThreeWay` degrades to a two-way union: `only-local`, `only-remote`, and `shared` all appear; the shared id's diverged title is held with `Base=""`, `Ours="from-B"` (local, wsB), `Theirs="from-A"` (remote, wsA), never auto-picked. *(This is exactly the combine path's projection — see §5.13.)*
- `merge_test.go:157-177` — a lone-side resolution-only re-close (`wontfix` base, local `duplicate`, remote unchanged) merges to `duplicate` with zero pending.
- `merge_test.go:179-207` — disjoint edits (local changed `i1`, remote changed `i2`) both survive, zero pending.
- `resolve_test.go:74-85` — Tier-1 prose: only ours rewrote the title (`a`→`b`) → title `b`, zero pending.
- `resolve_test.go:106-121` — Tier-2 prose is limited to the field that actually diverged: title and prompt identical, description divergent → exactly one pending carrying all three description versions.
- `resolve_test.go:585-594` — local removed the whole row, remote edited it → the remote edit survives (`Title == "edited"`).
- `resolve_test.go:596-606` — a base-only id absent on both sides → zero issues; no zero-value row is appended.
- Fixtures/clocks: `issueWithStatus` (merge_test.go:11-18), `jsonRoundTripIssue` (merge_test.go:20-31), `leaf` (resolve_test.go:19-30), `open` (resolve_test.go:32-34), `closedLeaf` (resolve_test.go:233-240), `issueClosedWith` (merge_test.go:130-142), `parentEdges` (merge_test.go:210-216), `hasParentCycle` (merge_test.go:219-236); `t0 = 2026-01-01`, `t1 = 2026-02-01`, `t2 = 2026-03-01` UTC (resolve_test.go:10-14).

## PART 12 — The migration registry (`internal/store/migrations/`)

Base: `/Users/bmf/code/links-issue-tracker/internal/store/migrations/`.

### 12.1 Registry-wide conventions

**Directive syntax.** All five `.sql` files are goose migrations delimited by SQL line comments:
- `-- +goose Up` begins the up section (`00001_baseline.sql:39`, `00002_add_lane.sql:1`, `00003_add_resolution.sql:1`, `00004_add_redirect_target.sql:1`, `00005_add_event_attribution.sql:1`).
- `-- +goose Down` begins the down section (`00001_baseline.sql:154`, `00002_add_lane.sql:12`, `00003_add_resolution.sql:17`, `00004_add_redirect_target.sql:52`, `00005_add_event_attribution.sql:21`).
- `-- +goose StatementBegin` / `-- +goose StatementEnd` wrap each individual statement (e.g. `00001_baseline.sql:40`/`45`, `00005_add_event_attribution.sql:28`/`30`). **Every executable statement in every file, in both sections, is inside exactly one such pair** — there are no bare statements (`00001_baseline.sql:40-175`, `00002_add_lane.sql:2-20`, `00003_add_resolution.sql:2-28`, `00004_add_redirect_target.sql:2-79`, `00005_add_event_attribution.sql:2-33`).

**Naming/numbering.** `<NNNNN>_<name>.sql`, 5-digit zero-padded, `00001_baseline.sql` … `00005_add_event_attribution.sql`; the accept-shape is enforced by `bounds.go:30-47`. `embed.go:1-6` states subsequent migrations append with strictly ascending versions and that only SQL migrations are wired — both the embed and `registryMaxVersion` scan `*.sql`.

**Idempotency, per statement.**
- **Up sections: no idempotency guards anywhere.** Every `CREATE TABLE` is bare, no `IF NOT EXISTS` (`00001_baseline.sql:41, 48, 71, 85, 96, 107, 119`). Every `CREATE INDEX` is bare (`00001_baseline.sql:130, 133, 136, 139, 142, 145, 148, 151`). Every `ALTER TABLE ... ADD COLUMN` is bare (`00002_add_lane.sql:9`, `00003_add_resolution.sql:11`, `00004_add_redirect_target.sql:10`, `00005_add_event_attribution.sql:15, 18`). Every `ADD CONSTRAINT` is bare (`00003_add_resolution.sql:14`, `00004_add_redirect_target.sql:18`).
- **`00001_baseline.sql` down section: every statement uses `IF EXISTS`** — all seven `DROP TABLE IF EXISTS` (`00001_baseline.sql:156, 159, 162, 165, 168, 171, 174`).
- **Down sections of 00002–00005: no `IF EXISTS` on any statement** (`00002_add_lane.sql:19`; `00003_add_resolution.sql:24, 27`; `00004_add_redirect_target.sql:75, 78`; `00005_add_event_attribution.sql:29, 32`).
- The one insert-conflict tolerance in the registry is `INSERT IGNORE INTO relations(...)` in the 00004 down section (`00004_add_redirect_target.sql:69`).

### 12.2 `00001_baseline.sql`

Header (`:1-37`) declares "FROZEN FILE — DO NOT EDIT" (`:1`), "the immutable definition of schema v1" (`:3`), that structural changes go in a new numbered file (`:11`), that the gate is `TestBaselineFileIsFrozen` and updating the pinned hash is **not** the correct response (`:12-15`) including for comment-only or whitespace edits (`:15-18`). `:20-24` records that a fresh workspace applies the file while a pre-goose workspace already at this shape is adopted (stamped v1) without re-running it, that CHECK constraints carry explicit deterministic names for `SHOW CREATE TABLE` stability, and that priority bounds mirror `model.PriorityNormal` (0) and `model.PriorityUrgent` (1).

On-disk sha256: `e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb` (matches `baseline_frozen_test.go:32`).

**Up section (`:39-152`).**

`meta` (`:41-44`): `meta_key VARCHAR(191) PRIMARY KEY` (`:42`); `meta_value TEXT NOT NULL` (`:43`).

`issues` (`:48-67`):

| column | type | null | default | key | line |
|---|---|---|---|---|---|
| `id` | `VARCHAR(191)` | — | none | `PRIMARY KEY` | :49 |
| `title` | `TEXT` | `NOT NULL` | none | | :50 |
| `description` | `TEXT` | `NOT NULL` | none | | :51 |
| `agent_prompt` | `TEXT` | `NULL` | none | | :52 |
| `status` | `VARCHAR(32)` | `NULL` | none | | :53 |
| `priority` | `INT` | `NOT NULL` | none | | :54 |
| `issue_type` | `VARCHAR(32)` | `NOT NULL` | none | | :55 |
| `topic` | `VARCHAR(191)` | `NOT NULL` | none | | :56 |
| `assignee` | `TEXT` | `NOT NULL` | none | | :57 |
| `created_at` | `VARCHAR(64)` | `NOT NULL` | none | | :58 |
| `updated_at` | `VARCHAR(64)` | `NOT NULL` | none | | :59 |
| `closed_at` | `VARCHAR(64)` | `NULL` | none | | :60 |
| `archived_at` | `VARCHAR(64)` | `NULL` | none | | :61 |
| `deleted_at` | `VARCHAR(64)` | `NULL` | none | | :62 |
| `item_rank` | `TEXT` | `NOT NULL` | `DEFAULT ''` | | :63 |

Named CHECK constraints on `issues`:
- `CONSTRAINT issues_status_check CHECK ((issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed')))` (`:64`)
- `CONSTRAINT issues_priority_check CHECK (priority >= 0 AND priority <= 1)` (`:65`)
- `CONSTRAINT issues_type_check CHECK (issue_type IN ('task','feature','bug','chore','epic'))` (`:66`)

`relations` (`:71-81`): `src_id VARCHAR(191) NOT NULL` (`:72`); `dst_id VARCHAR(191) NOT NULL` (`:73`); `type VARCHAR(32) NOT NULL` (`:74`); `created_at VARCHAR(64) NOT NULL` (`:75`); `created_by TEXT NOT NULL` (`:76`); `PRIMARY KEY (src_id, dst_id, type)` (`:77`); `FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE` (`:78`); `FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE` (`:79`); `CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'))` (`:80`).

`comments` (`:85-92`): `id VARCHAR(191) PRIMARY KEY` (`:86`); `issue_id VARCHAR(191) NOT NULL` (`:87`); `body TEXT NOT NULL` (`:88`); `created_at VARCHAR(64) NOT NULL` (`:89`); `created_by TEXT NOT NULL` (`:90`); `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE` (`:91`).

`labels` (`:96-103`): `issue_id VARCHAR(191) NOT NULL` (`:97`); `label VARCHAR(191) NOT NULL` (`:98`); `created_at VARCHAR(64) NOT NULL` (`:99`); `created_by TEXT NOT NULL` (`:100`); `PRIMARY KEY (issue_id, label)` (`:101`); `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE` (`:102`).

`issue_events` (`:107-115`): `id VARCHAR(191) PRIMARY KEY` (`:108`); `issue_id VARCHAR(191) NOT NULL` (`:109`); `action VARCHAR(64) NULL` (`:110`); `reason TEXT NOT NULL` (`:111`); `actor TEXT NOT NULL` (`:112`); `created_at VARCHAR(64) NOT NULL` (`:113`); `FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE` (`:114`).

`issue_event_changes` (`:119-126`): `event_id VARCHAR(191) NOT NULL` (`:120`); `field VARCHAR(64) NOT NULL` (`:121`); `from_value TEXT NULL` (`:122`); `to_value TEXT NULL` (`:123`); `PRIMARY KEY (event_id, field)` (`:124`); `FOREIGN KEY (event_id) REFERENCES issue_events(id) ON DELETE CASCADE` (`:125`).

Indexes, in file order (`:129-152`):
1. `CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);` (`:130`)
2. `CREATE INDEX idx_issues_rank ON issues(item_rank(191));` — prefix index of length 191 on the `TEXT` column (`:133`)
3. `CREATE INDEX idx_relations_src_type ON relations(src_id, type);` (`:136`)
4. `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);` (`:139`)
5. `CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);` (`:142`)
6. `CREATE INDEX idx_labels_issue ON labels(issue_id, label);` (`:145`)
7. `CREATE INDEX idx_labels_name ON labels(label, issue_id);` (`:148`)
8. `CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at);` (`:151`)

No backfill `INSERT`/`UPDATE` exists in this file.

**Down section (`:154-175`)** — seven `IF EXISTS` drops, child-tables first, each in its own statement pair: `issue_event_changes` (`:156`), `issue_events` (`:159`), `labels` (`:162`), `comments` (`:165`), `relations` (`:168`), `issues` (`:171`), `meta` (`:174`). No index drops (indexes go with their tables). No data preservation.

### 12.3 `00002_add_lane.sql`

sha256 `9126cd9e9c3a01137898fb9023c50b3f8741950f1e5d943dad521852939a4b75`.

**Up (`:1-10`)** — one statement: `ALTER TABLE issues ADD COLUMN lane text NOT NULL DEFAULT '';` (`:9`) — column `lane`, type lowercase `text`, `NOT NULL`, `DEFAULT ''`. No index, no constraint, no backfill statement (the default populates existing rows). Comment (`:3-8`): lane partitions an epic's children into parallel rank-ordered sub-sequences; children sharing a lane are sequenced by rank, different lanes run in parallel; the empty-string default is "one lane value like any other"; lane is meaningful only within an epic and the gate predicate enforces that scoping, not the column.

**Down (`:12-20`)** — one statement: `ALTER TABLE issues DROP COLUMN lane;` (`:19`). Declared LOSS CONTRACT (`:13-17`): the down does not restore per-child lane assignments; with no lane column every child falls back to the shared default lane (fully-sequential epic gate); exact lane values require restoring from a pre-upgrade dbsnapshot.

### 12.4 `00003_add_resolution.sql`

sha256 `47b50dea1a1e2b7e31ba6b0fa95f5f77c6411b81ee525b04e1ff5e5fdb469563`.

**Up (`:1-15`)** — two statements:
1. `ALTER TABLE issues ADD COLUMN resolution VARCHAR(32) NULL;` (`:11`) — nullable, no default.
2. `ALTER TABLE issues ADD CONSTRAINT issues_resolution_check CHECK (resolution IS NULL OR resolution IN ('duplicate','superseded','obsolete','wontfix'));` (`:14`)

No index, no backfill. Comment (`:3-10`): resolution is the sealed close reason; NULL on every non-closed row and on a `done` close; the CHECK seals the value set at the DB boundary, mirroring `relations_type_check`, with `ParseResolution` as the primary gate and this as defense in depth.

**Down (`:17-28`)** — two statements, constraint first: `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check;` (`:24`), then `ALTER TABLE issues DROP COLUMN resolution;` (`:27`). LOSS CONTRACT (`:18-22`): recorded close reasons are not preserved; every closed ticket falls back to "closed with no recorded resolution".

### 12.5 `00004_add_redirect_target.sql`

sha256 `e171b9a18f13d67967e6c235499fc35254125ac23b36f9a687eddc7c411b590d`.

**Up (`:1-50`)** — four statements:

1. `ALTER TABLE issues ADD COLUMN redirect_target VARCHAR(191) NULL;` (`:10`). Nullable, no default, **no foreign key** — explicitly noted at `:61-62`.
2. `ALTER TABLE issues ADD CONSTRAINT issues_redirect_target_check CHECK (redirect_target IS NULL OR resolution IN ('duplicate','superseded'));` (`:18`). The comment at `:13-17` states the CHECK is one-directional on purpose: a redirecting resolution with an unknown target stays representable; the write boundary requires a target for every new redirecting close.
3. Backfill UPDATE (`:27-38`), verbatim:
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
For issues whose `resolution` is `'duplicate'` or `'superseded'` **and** which have exactly one incident `related-to` edge (counting both directions), `redirect_target` is set to that edge's counterpart id. Rows with any other edge count are left NULL with edges intact (`:24-26`).
4. Backfill DELETE (`:45-49`), verbatim:
```sql
DELETE r FROM relations r
JOIN issues i ON i.redirect_target IS NOT NULL
  AND ((r.src_id = i.id AND r.dst_id = i.redirect_target)
    OR (r.dst_id = i.id AND r.src_id = i.redirect_target))
WHERE r.type = 'related-to';
```
Deletes only `related-to` edges whose endpoints are exactly (issue, its redirect_target) in either direction (`:41-44`).

**Down (`:52-79`)** — three statements, re-materialize first:
1. (`:69-72`), verbatim:
```sql
INSERT IGNORE INTO relations(src_id, dst_id, type, created_at, created_by)
SELECT LEAST(i.id, i.redirect_target), GREATEST(i.id, i.redirect_target), 'related-to', COALESCE(i.closed_at, i.updated_at), 'unknown'
FROM issues i
WHERE i.redirect_target IS NOT NULL;
```
`src_id` = lexicographic min of the pair, `dst_id` = max, `type` literal `'related-to'`, `created_at` = `closed_at` falling back to `updated_at`, `created_by` = literal `'unknown'`.
2. `ALTER TABLE issues DROP CONSTRAINT issues_redirect_target_check;` (`:75`)
3. `ALTER TABLE issues DROP COLUMN redirect_target;` (`:78`)

LOSS CONTRACT (`:53-67`): the redirect-vs-manual-peer distinction is lost; edge `created_at` is approximated by the close timestamp (fallback `updated_at`) and `created_by` by `'unknown'`; original stamps are not preserved. `INSERT IGNORE` tolerates two enumerated benign classes — (a) a manual edge already linking the same pair, (b) an FK-gap row: since `redirect_target` has no FK, a redirect whose canonical row was hard-deleted cannot re-materialize (relations' FK would reject it) and is silently skipped. The comment explicitly calls this "a silent skip, accepted here only because a migration has no per-row reporting channel".

### 12.6 `00005_add_event_attribution.sql`

sha256 `ed625b2817365ed357ad477cd0691994a93a65a7ab1005d1b05456c39d159a70`.

**Up (`:1-19`)** — two statements: `ALTER TABLE issue_events ADD COLUMN stream_id VARCHAR(64) NULL;` (`:15`) and `ALTER TABLE issue_events ADD COLUMN workspace_id VARCHAR(191) NULL;` (`:18`). Both nullable, no default, no index, no constraint, **no backfill** — the comment states attribution "is historical fact, never backfilled", so a freshly upgraded repository derives zero claims (`:9-12`). Claims are DERIVED from these stamps at read time with no claim table (`:10-11`), and "Nothing user-, host-, or path-shaped may ever enter these columns; the database syncs to shared remotes" (`:13-14`).

**Down (`:21-33`)** — `ALTER TABLE issue_events DROP COLUMN stream_id;` (`:29`) and `ALTER TABLE issue_events DROP COLUMN workspace_id;` (`:32`). LOSS CONTRACT (`:22-27`): attribution collected on this version is not restored; the claim predicate reads unattributed history as zero claims.

### 12.7 `embed.go`

Package `migrations` (`embed.go:7`), imports `"embed"` (`:9`). Embed directive `//go:embed *.sql` (`:14`) on `var FS embed.FS` (`:15`). The glob is `*.sql` only — non-`.sql` files in the directory are not embedded. `FS` is the only exported symbol in the file, described as "the embedded goose migration registry… the one source the runner reads from; nothing applies schema outside this set" (`:11-12`). The package doc records that only SQL migrations are wired and that adding a Go migration would require registering it with goose and widening the embed pattern (`:1-6`).

### 12.8 `bounds.go`

`const Baseline int64 = 1` (`bounds.go:19`) — the only literal version constant. Doc: the version `00001_baseline.sql` stamps; a pre-goose workspace already at the baseline shape is adopted by recording this version without re-running the CREATE TABLEs (`:12-14`); the baseline is a property of the embedded registry (the lowest version in FS), not a parallel constant in consumer packages (`:16-18`).

`ParseVersion(name string) (int64, bool)` (`bounds.go:30-47`):
1. `base := filepath.Base(name)` — directory components stripped (`:31`).
2. `idx := strings.IndexByte(base, '_')`; `idx <= 0` → `(0, false)` (`:32-35`) — no underscore, or underscore at position 0, rejects.
3. Every byte of `base[:idx]` must be in `'0'..'9'`, else `(0, false)` (`:36-41`).
4. `strconv.ParseInt(digits, 10, 64)`; on error `(0, false)` (`:42-45`).
5. `(version, true)` (`:46`).
The doc justifies the exactness against `fmt.Sscanf("%d")`, which would accept leading whitespace, signs, or `"00001a"` (`:25-29`).

`MaxVersion() (int64, error)` (`bounds.go:55-76`): `FS.ReadDir(".")`; error wrapped `"read migration registry: %w"` (`:56-59`). Skips `entry.IsDir()` and non-`.sql` names (`:62-64`). A remaining name `ParseVersion` rejects is a **hard error**: `"migration file %q does not begin with a numeric version"` (`:67`). Empty version list → `errors.New("migration registry is empty")` (`:71-73`). Sorts ascending, returns the last (`:74-75`). With the current registry: **5**.

`BaselineFileName() (string, error)` (`bounds.go:79-93`): same `ReadDir` and wrapped error (`:80-82`), same dir/`.sql` skip (`:85-87`); returns the first entry whose `ParseVersion` equals `Baseline` (`:88-90`) — currently `00001_baseline.sql`. Not found → `"no baseline migration (v%d) found in registry"` (`:92`). **Unlike `MaxVersion`, a non-parseable `.sql` name is silently skipped here rather than erroring** (`:88`).

Nothing in `bounds.go` enforces monotonicity or gap-freeness; version identity is enforced entirely by the tests below.

### 12.9 `baseline_frozen_test.go`

`const baselineFrozenHash = "e86c1aa36ebe70ddbaa2b18f18ee310c33dfce1f07fb3c2811a1d76385ad1fbb"` (`:32`) — equals the current file's sha256.

`TestBaselineFileIsFrozen` (`:37-72`): reads `FS.ReadFile("00001_baseline.sql")` — the filename is **hardcoded**, not obtained via `BaselineFileName()` (`:38`); read error → `"read embedded 00001_baseline.sql: %v"` (`:40`). Computes `sha256.Sum256(data)` hex-encoded (`:42-43`). Passes iff the hex equals the constant exactly (`:44-46`). **The invariant is exact byte equality of the whole file**, comments and whitespace included — not a parsed-schema comparison. On mismatch, `t.Fatalf` with `want sha256:` / `got sha256:`, the instruction to revert and add `00002_<your-change>.sql` "(or the next free number)", an explicit statement that non-structural edits (comment, whitespace, typo) are also forbidden and belong in "a sibling .md, the package doc in embed.go, or schema_reconcile.go", and `DO NOT update baselineFrozenHash to match the new bytes.` (`:47-71`). Design notes at `:22-31`: one enforcer not two; one copy of the hash in Go rather than a duplicate in a workflow yaml; pin bytes rather than a parsed shape.

### 12.10 `down_section_test.go` — the invertibility gate

`hasDownSection(data []byte) bool` (`:19-63`). Contract: reports whether the file contains a `+goose Down` section followed by at least one non-empty, non-comment SQL statement before EOF or the next `+goose Up` (`:8-10`). Algorithm: splits on `"\n"`, tracks `inBlock` (block-comment carry) and `downSeen` (`:26-29`); per line computes `content, exitedBlock := lineContent(line, inBlock)` (`:33`). If not `inBlock` and `isGooseDirective(line,"down")` → `downSeen = true`, carry, continue (`:41-45`). If not `inBlock` and `isGooseDirective(line,"up")` → **if `downSeen` already, return `false`** (the Down section ended without executable content); else carry and continue (`:46-53`). Otherwise carry; skip while `!downSeen`; once `downSeen`, return `true` on the first line whose stripped content is non-blank after `TrimSpace` (`:54-60`). Falls through to `false` (`:62`).

`isGooseDirective(line, kind)` (`:70-77`): trims surrounding whitespace (`:71`); requires the prefix `"-- "` (two dashes plus one space) on the lowercased trimmed line (`:72-74`); the body after `"-- "` is trimmed and compared with `strings.EqualFold(body, "+goose "+kind)` — **the whole remaining line must be exactly `+goose <kind>`**, case-insensitively (`:75-76`). Directives with extra trailing text are rejected.

`lineContent(line string, inBlock bool) (string, bool)` (`:90-123`): if `inBlock` on entry, search `*/`; absent → `("", true)` (`:93-97`); else continue after the closer. Loop: find the earliest of `--`, `#`, `/*` via `earliest` (`:102-105`); none → append remainder and return `(out, false)` (`:106-109`); emit text before the marker (`:110`); for `--` or `#` return immediately with `inBlock=false` (`:112-113`); for `/*` skip to `*/`, and if there is no closer on this line return `(out, true)` (`:115-119`), else continue after it. Documented limitation: quote-string awareness is out of scope; the gate is deliberately conservative — extra stripping means reject (`:86-89`).

`earliest(a, b, c int) (int, string)` (`:125-141`): smallest non-negative index among the three, tagged `"--"`, `"#"`, or `"/*"`; ties resolve to the first in the fixed order `{a,"--"}, {b,"#"}, {c,"/*"}` because the comparison is strict `<` (`:128-138`). All negative → `(-1, "")`.

`TestEveryMigrationHasDownSection` (`:152-186`): `FS.ReadDir(".")`, error → Fatalf (`:153-156`); skips dirs and non-`.sql` (`:159-161`), counting the rest in `sqlFiles`; each must satisfy `hasDownSection`, failure being a `t.Errorf` stating the Down section must contain at least one non-empty non-comment SQL statement between `-- +goose Down` and EOF (or the next `-- +goose Up`), that loss-making migrations should reconstruct the schema with documented loss or document the loss contract, and that "The presence of the Down section itself is non-negotiable" (`:163-181`). `sqlFiles == 0` → `t.Fatal("no *.sql files found in embedded registry")` (`:183-185`). **No count is pinned to a specific number.** The doc names the runtime sibling `TestEveryMigrationDownIsExercised` in `internal/store`, which proves the section actually runs (`:149-151`).

`TestHasDownSectionRejectsMissingShapes` (`:192-281`) — fifteen fixtures (`:198-272`):

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

Each runs as a subtest; mismatch is `t.Errorf("hasDownSection() = %v, want %v\nbody:\n%s", …)` (`:274-280`).

### 12.11 `version_reuse_test.go` — the version-slot-reuse gate

Types: `pinnedMigration{file, sha256 string}` (`:17-20`); `migrationFile{version int64; name, sha256 string}` (`:43-47`); `reuseKind int` with, in iota order (`:55-63`), `kindContentChanged`=0 (a pinned version's bytes no longer match its pin), `kindRenamed`=1 (content matches, filename changed), `kindUnpinned`=2 (an on-disk non-baseline version has no pin), `kindDeleted`=3 (a pinned version is gone from disk), `kindDuplicate`=4 (two on-disk files claim the same version); `reuseFinding{kind; version int64; pinned pinnedMigration; onDisk []migrationFile}` (`:69-74`).

The literal pins, `pinnedVersionContent` (`:33-38`) — all four verified against the on-disk sha256s:
```go
2: {file: "00002_add_lane.sql",              sha256: "9126cd9e9c3a01137898fb9023c50b3f8741950f1e5d943dad521852939a4b75"},
3: {file: "00003_add_resolution.sql",        sha256: "47b50dea1a1e2b7e31ba6b0fa95f5f77c6411b81ee525b04e1ff5e5fdb469563"},
4: {file: "00004_add_redirect_target.sql",   sha256: "e171b9a18f13d67967e6c235499fc35254125ac23b36f9a687eddc7c411b590d"},
5: {file: "00005_add_event_attribution.sql", sha256: "ed625b2817365ed357ad477cd0691994a93a65a7ab1005d1b05456c39d159a70"},
```
Version 1 is deliberately absent — `baseline_frozen_test.go` is its content enforcer (`:28-32`).

`detectVersionReuse(pinned, onDisk) []reuseFinding` (`:84-130`) — pure; precedence in order:
1. Group on-disk files by version (`:85-88`).
2. `len(files) > 1` → `kindDuplicate` for that version, and **no content check on the pair** (`:92-97`).
3. Version absent from `pinned` → `kindUnpinned` (`:99-103`).
4. `f.sha256 != pin.sha256` → `kindContentChanged`; **takes priority over a filename difference on the same version** (`:104-109`).
5. `f.name != pin.file` with content matching → `kindRenamed` (`:110-115`).
6. Every pinned version with no on-disk file → `kindDeleted` (`:117-121`).
7. Findings sorted by version ascending, ties by `kind` ascending (`:123-128`).

`(reuseFinding).explain()` (`:135-194`) — one message per kind, all naming the version. Key literal steering text: `kindContentChanged` — "version %d has been REUSED under different content." with `released as:` / `now on disk:` lines, "goose keys migrations by version NUMBER, not by content", and "DO NOT change this version's pin in pinnedVersionContent to match the new bytes" (`:137-152`). `kindRenamed` — "version %d kept its content but was RENAMED: %s -> %s.", instructing restore-the-name or update-the-pin's-`"file"`-field in the same PR (`:153-161`). `kindUnpinned` — "version %d (%s) is not pinned in pinnedVersionContent." plus the exact line to add, `  %d: {file: %q, sha256: %q},` (`:162-170`). `kindDeleted` — "version %d was released as %s (sha256 %s) but is now MISSING from the registry." / "A released migration must never be deleted" (`:171-178`). `kindDuplicate` — "version %d is claimed by %d files: %s." with names sorted, "Renumber all but one to the next free version(s)." (`:179-190`). `default` — `"unhandled reuse finding kind %d for version %d — add a message in reuseFinding.explain"` (`:191-192`).

`TestReleasedMigrationsAreContentPinned` (`:207-235`): `FS.ReadDir(".")`, error → Fatalf (`:208-211`); skips dirs and non-`.sql` (`:214-216`); a `.sql` name `ParseVersion` rejects → `t.Fatalf("registry file %q does not begin with a numeric version", …)` (`:217-220`); `v == Baseline` skipped entirely (`:221-224`); each remaining file sha256'd into a `migrationFile` (`:225-231`); every `detectVersionReuse` finding becomes a `t.Errorf(finding.explain())` (`:232-234`).

`TestDetectVersionReuse` (`:244-336`) — base pins `2:{"00002_add_lane.sql","aaa"}`, `3:{"00003_add_resolution.sql","bbb"}`, `4:{"00004_add_redirect_target.sql","ccc"}` (`:249-253`); `clean` mirrors them (`:254-258`). Nine cases, compared by **kind + version only** (`:264`, `:328-333`):

| case | on-disk deviation | expected |
|---|---|---|
| clean (`:266-271`) | none | nil |
| content reuse (`:272-277`) | v2 sha `"DIFFERENT"` | `{kindContentChanged, 2}` |
| content reuse under rename (`:278-283`) | v3 renamed to `00003_renamed.sql` **and** sha `"DIFFERENT"` | `{kindContentChanged, 3}` (drift wins over rename) |
| pure rename (`:284-289`) | v3 named `00003_renamed.sql`, sha `"bbb"` | `{kindRenamed, 3}` |
| unpinned new migration (`:290-295`) | extra `00005_new.sql`/`"eee"` at v5, no pin | `{kindUnpinned, 5}` |
| clean append (`:296-301`) | v5 present AND pinned as `00005_new.sql`/`"eee"` | nil |
| deleted (`:302-307`) | v4 missing from disk | `{kindDeleted, 4}` |
| duplicate (`:308-313`) | `00002_add_lane.sql`/`"aaa"` and `00002_add_lane_again.sql`/`"zzz"` both v2 | `{kindDuplicate, 2}` only |
| combined (`:314-319`) | v2 sha `"DIFFERENT"`, v4 absent | `[{kindContentChanged,2},{kindDeleted,4}]` in that order |

Length mismatch → `t.Fatalf` (`:325-327`); per-index kind/version mismatch → `t.Errorf` (`:328-333`).

`TestEveryReuseKindHasMessage` (`:344-372`): fixture pin `{file:"00007_x.sql", sha256:"cafef00d"}`; `one = [{7,"00007_x.sql","deadbeef"}]`; `two = one + {7,"00007_y.sql","beefcafe"}` (`:345-347`). Constructs one finding per declared kind shaped exactly as `detectVersionReuse` would produce it — notably `kindUnpinned` with **no** `pinned`, `kindDeleted` with **no** `onDisk`, `kindDuplicate` with two entries (`:353-362`) — so an over-indexing `explain()` panics here rather than in production (`:349-352`). Asserts for each that `explain()` does not contain `"unhandled reuse finding kind"` (`:365-367`) and does contain `"7"` (`:368-370`).

### 12.12 `bounds_test.go`

`TestParseVersionShape` (`:12-41`) — twelve cases (`:18-30`) asserting `(want, ok)`:

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

ok-mismatch → `t.Errorf("ParseVersion(%q) ok = %v, want %v")` then `continue`; value mismatch when ok → `t.Errorf("ParseVersion(%q) = %d, want %d")` (`:31-40`).

`TestMaxVersionReflectsEmbeddedRegistry` (`:48-76`): calls `MaxVersion()`, error → Fatalf (`:49-52`); independently rescans `FS.ReadDir(".")` skipping dirs and non-`.sql`, taking the max `ParseVersion` result, with an unparseable name → `t.Fatalf("registry contains non-parseable filename %q")` (`:53-69`); asserts `max == expected` (`:70-72`) and `max >= Baseline` (`:73-75`). Deliberately pins agreement with a fresh scan, **not** a hard-coded value (`:43-47`) — no literal `5` appears.

`TestBaselineFileNameMatches` (`:81-96`): `BaselineFileName()` must not error (`:82-85`); its result must be accepted by `ParseVersion` (`:86-89`); the parsed version must equal `Baseline` (`:90-92`); `FS.ReadFile(name)` must succeed (`:93-95`).

### 12.13 Registry invariant summary

| invariant | enforcer | literals |
|---|---|---|
| `00001_baseline.sql` bytes never change | `TestBaselineFileIsFrozen` (`baseline_frozen_test.go:37`) | sha256 `e86c1aa3…d1fbb` (`:32`) |
| v2+ bytes never change; filename never changes; never deleted; never duplicated; always pinned | `TestReleasedMigrationsAreContentPinned` (`version_reuse_test.go:207`) via `detectVersionReuse` (`:84`) | four pins at `version_reuse_test.go:34-37` |
| every `.sql` has a non-empty non-comment Down section | `TestEveryMigrationHasDownSection` (`down_section_test.go:152`) | none pinned; registry must be non-empty (`:183`) |
| filename → version parse shape | `TestParseVersionShape` (`bounds_test.go:12`) over `ParseVersion` (`bounds.go:30`) | 12-row table |
| `MaxVersion()` equals a fresh FS scan and is ≥ `Baseline` | `TestMaxVersionReflectsEmbeddedRegistry` (`bounds_test.go:48`) | `Baseline = 1` (`bounds.go:19`) |
| `BaselineFileName()` round-trips to `Baseline` and is readable | `TestBaselineFileNameMatches` (`bounds_test.go:81`) | — |
| `explain()` is exhaustive over `reuseKind` | `TestEveryReuseKindHasMessage` (`version_reuse_test.go:344`) | version `7`, shas `cafef00d`/`deadbeef`/`beefcafe` |

## PART 13 — `internal/dbsnapshot`: snapshot format and lifecycle

Base: `/Users/bmf/code/links-issue-tracker/internal/dbsnapshot/`.

### 13.1 Files and build tags

| File | Build constraint | Ref |
|---|---|---|
| `snapshot.go` | none | `snapshot.go:35` |
| `residue.go` | none | `residue.go:1` |
| `clone_darwin.go` | `//go:build darwin` | `clone_darwin.go:1` |
| `clone_linux.go` | `//go:build linux` | `clone_linux.go:1` |
| `clone_other.go` | `//go:build !darwin && !linux` | `clone_other.go:1` |
| `snapshot_unix_test.go` | `//go:build unix` | `snapshot_unix_test.go:1` |

Each `clone_*.go` defines exactly one function, `cloneTree(ctx context.Context, src, dst string) error` — Darwin `clone_darwin.go:20`, Linux `clone_linux.go:18`, other `clone_other.go:12`. Platform variance is link-time file selection, not a runtime branch.

External dependency `github.com/promptctl/primitives/filelock` (`snapshot.go:50`, `residue.go:13`): `Acquire(ctx, lockPath string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, bool, error)` returns `(release, true, nil)` on acquisition, `(nil, false, nil)` on contention, `(nil, false, err)` on real failure; `maxAttempts == 1` is a non-blocking probe.

### 13.2 Exported surface

- `type Snapshot struct { Path string \`json:"path"\`; Name string \`json:"name"\`; Created time.Time \`json:"created"\` }` — `snapshot.go:54-58`
- `var ErrSnapshotMissing = errors.New("dbsnapshot: snapshot not found")` — `snapshot.go:61`
- `var ErrSnapshotsBusy = errors.New("snapshot producer beacon busy")` — `snapshot.go:90`
- `func Take(ctx context.Context, databaseDir, snapshotsDir, label string) (Snapshot, error)` — `snapshot.go:111`
- `func List(snapshotsDir string) ([]Snapshot, error)` — `snapshot.go:256`
- `func Restore(databaseDir, snapshotsDir, name string) (string, error)` — `snapshot.go:293`
- `func Prune(snapshotsDir string, keep int) error` — `snapshot.go:334`
- `func PruneMatching(snapshotsDir string, keep int, match func(name string) bool) error` — `snapshot.go:351`
- `func IsProducerArtifactName(name string) bool` — `snapshot.go:440`
- `func CollectOrphanedResidue(snapshotsDir string) error` — `residue.go:51`

Unexported: `reservedPaths{created time.Time; name, finalPath, tmpPath, reservePath string}` (`snapshot.go:176-182`); `producerBeaconPath` (`:82-84`); `reserveSnapshotPaths` (`:198`); `pathFree` (`:238`); `formatName` (`:385`); `validateSnapshotName` (`:404`); `isCollectibleResidue` (`:459`); `isCollectibleArtifactName` (`:469`); `isCollectorCondemnedName` (`:482`); `parseName` (`:503`); `parsePositiveDigits` (`:526`); `isMintableLabel` (`:540`); `sanitizeLabel` (`:567`); `isDoltJournalLockRel` (`:589`); `walkAndCopy` (`:598`); `plainFileCopy` (`:657`); `copyWithContext` (`:695`); `condemnResidue` (`residue.go:97`); `ficloneOrCopy` (Linux, `clone_linux.go:22`).

### 13.3 Constants

| Identifier | Value | Ref |
|---|---|---|
| `producerBeaconName` | `".links-snapshot-producer.lock"` | `snapshot.go:70` |
| `producerBeaconRetryAttempts` | `20` | `snapshot.go:78` |
| `producerBeaconRetryDelay` | `50 * time.Millisecond` | `snapshot.go:79` |
| `maxReserveAttempts` | `1024` | `snapshot.go:196` |
| `tmpSuffix` | `".tmp"` | `snapshot.go:427` |
| `reserveSuffix` | `".reserve"` | `snapshot.go:428` |
| `condemnedSuffix` | `".condemned"` | `snapshot.go:429` |
| `producerArtifactSuffixes` | `[]string{tmpSuffix, reserveSuffix, condemnedSuffix}` | `snapshot.go:432` |
| `maxLabelBytes` | `128` | `snapshot.go:562` |
| `copyContextChunk` | `32 << 20` (32 MiB) | `snapshot.go:688` |

### 13.4 On-disk layout

A snapshot is a **directory** directly under `snapshotsDir`, named `<unix-ns>` or `<unix-ns>-<label>`:
- Base component is `strconv.FormatInt(t.UnixNano(), 10)` — decimal nanoseconds since the Unix epoch, no padding (`snapshot.go:386`).
- `t` is `time.Now().UTC()` at reservation, possibly incremented by whole nanoseconds on collision (`snapshot.go:199`, `:219`, `:230`).
- Non-empty sanitized label → `base + "-" + clean` (`snapshot.go:388-391`).
- Contents are a tree copy of `databaseDir`'s contents, rooted at `databaseDir` itself, so `<snap>/<x>` corresponds to `<databaseDir>/<x>` (`snapshot.go:598-648`; asserted `snapshot_test.go:418-455`, round trip `snapshot_test.go:101-142`).

Sibling artifact names in the same directory:
- `<name>.reserve` — a directory created by `os.Mkdir(reservePath, 0o755)`, the atomic slot claim (`snapshot.go:205`).
- `<name>.tmp` — the in-flight clone destination (`snapshot.go:204`, written by `cloneTree` at `snapshot.go:162`).
- `<artifact>.<unix-ns>.condemned` — a corpse severed by the collector, minted as `fmt.Sprintf("%s.%d%s", path, time.Now().UnixNano(), condemnedSuffix)` (`residue.go:110`), so the full form is e.g. `1700000000000000000-label.tmp.1700000000000000005.condemned`.
- `.links-snapshot-producer.lock` — the flock beacon file (`snapshot.go:70`, `:83`).

Restore-time artifact, created next to the database dir (**not** in `snapshotsDir`): `<databaseDir>.pre-restore-<unix-ns>` via `fmt.Sprintf("%s.pre-restore-%d", databaseDir, time.Now().UTC().UnixNano())` (`snapshot.go:314`).

Where `snapshotsDir` comes from: CLI uses `filepath.Join(ws.StorageDir, "snapshots")` (`/Users/bmf/code/links-issue-tracker/internal/cli/snapshots.go:32-34`); the store's migration path uses `filepath.Join(filepath.Dir(filepath.Clean(databaseDir)), "snapshots")` (`/Users/bmf/code/links-issue-tracker/internal/store/migrate_snapshot.go:177-180`).

### 13.5 Name grammar — mint side

`sanitizeLabel` (`snapshot.go:567-582`) is a lossy normalizer that never errors:
1. Each rune in `[a-z A-Z 0-9 _ -]` is written through; every other rune (multi-byte included) becomes a single `'-'` **byte** (`:570-576`).
2. Truncated to the first `maxLabelBytes` = 128 **bytes** (`:578-580`).
3. `strings.Trim(clean, "-")` strips leading and trailing dashes (`:581`).
4. May be empty ⇒ `formatName` emits the bare timestamp (`:388-390`).

`snapshot_test.go:644-657` asserts a 300-`x` label yields a name beginning `"1700000000000000001-xxx"`, that the worst derived form `len(name)+len(".reserve")+len(".1700000000000000001")+len(".condemned")` is ≤ 255 bytes, and that the truncated name still parses. `snapshot_test.go:399-416` asserts label `"pre-migration #5 / foo!"` yields a name containing `"-"` and an existing directory at `snap.Path`.

### 13.6 Name grammar — parse side

`parseName(name) (time.Time, bool)` (`snapshot.go:503-519`):
1. Reject immediately if `IsProducerArtifactName(name)` (`:504-506`).
2. Split at the **first** `'-'` (`strings.IndexByte`): `head` before, `label` after, `dashed = true` (`:507-510`).
3. `head` must satisfy `parsePositiveDigits` (`:511-514`).
4. If dashed, `label` must satisfy `isMintableLabel` (`:515-517`).
5. Return `time.Unix(0, ns).UTC()` (`:518`).

`parsePositiveDigits(s)` (`snapshot.go:526-532`): `strconv.ParseInt(s, 10, 64)` must succeed, `ns > 0`, and `strconv.FormatInt(ns,10) == s` — the round-trip rejects sign prefixes and leading zeros that `ParseInt` tolerates. The comment records that `"+123.tmp"` was previously classified as lit-minted residue and destroyed (`:521-525`).

`isMintableLabel(label)` (`snapshot.go:540-553`): rejects empty, rejects `label[0]=='-'`, rejects `label[len-1]=='-'`; every remaining **byte** must be in `[a-zA-Z0-9_-]`. **No length bound** — deliberately accepts labels minted by pre-cap binaries (`:534-539`).

`IsProducerArtifactName(name)` (`snapshot.go:440-447`): true if `strings.HasSuffix(name, s)` for any of `.tmp`, `.reserve`, `.condemned`. This is the broad rejection predicate `parseName` uses.

`snapshot_test.go:582-624` pins parse results exactly: `1700000000000000000` true; `1700000000000000000-label` true; `1700000000000000000-pre-migration-foo` true; `snap-1700000000-abc` false; `abc-def` false; `""` false; `0` false; `+1700000000000000000` false; `01700000000000000000` false; `1700000000000000000-my.backup` false; `1700000000000000000-` false; `1700000000000000000--edges-` false; `1700000000000000000.tmp` false; `1700000000000000000-label.tmp` false; `1700000000000000000.reserve` false; `1700000000000000000-label.reserve` false; `1700000000000000000.tmp.condemned` false; `1700000000000000000.tmp.1755250000000000000.condemned` false. `snapshot_test.go:626-637` asserts `formatName`/`parseName` round-trip for `2026-05-14T12:00:00.123456789Z` with an empty label, compared via `parsed.Equal(created)`.

### 13.7 `Take` — full ordered behavior

`Take(ctx, databaseDir, snapshotsDir, label)` (`snapshot.go:111-171`):

1. **ctx gate.** `if ctxErr := ctx.Err(); ctxErr != nil { return Snapshot{}, ctxErr }` (`:115-117`) — the raw ctx error, unwrapped. Exists so Darwin's single-syscall `Clonefile` path cannot mint a snapshot a pre-canceled call should refuse (`:112-114`). `snapshot_test.go:343-360` asserts `errors.Is(err, context.Canceled)` and that `snapshotsDir` was **not created**.
2. **Stat source.** `os.Stat(databaseDir)`; error → `"stat database dir: %w"` (`:118-121`). `snapshot_test.go:14-27` asserts a missing source errors and leaves no `.tmp` entry.
3. `!info.IsDir()` → `"database dir is not a directory: %s"` (`:122-124`).
4. `os.MkdirAll(snapshotsDir, 0o755)`; error → `"create snapshots dir: %w"` (`:125-127`).
5. **Residue collection.** `CollectOrphanedResidue(snapshotsDir)`; on error prints to **stderr** `"lit: could not collect orphaned snapshot residue (the take proceeds; collection retries next take): %v\n"` and continues — never fails the take (`:136-138`). Runs **before** the beacon is held, because holding it shared would make the collector's exclusive probe self-skip (`:128-131`). Pinned by `residue_test.go:146-176` and `residue_test.go:289-317`.
6. **Acquire beacon SHARED.** `filelock.Acquire(ctx, producerBeaconPath(snapshotsDir), false, 20, 50ms)` (`:139`).
   - Real error → `"acquire snapshot producer beacon: %w"` (`:140-142`).
   - Contention after 20 attempts → `"dbsnapshot: residue collection is holding the snapshots directory at %s and did not finish within its budget; retry: %w"` wrapping `ErrSnapshotsBusy` (`:143-145`). Total wait bound ≈ 20 × 50 ms = 1s.
   - Held **shared**, so concurrent Takes coexist: `residue_test.go:403-436` asserts a Take succeeds while another shared holder exists and that residue is spared in that window.
7. **Deferred beacon release.** Release error → stderr `"lit: could not release snapshot producer beacon (residue collection defers until this process exits): %v\n"`, never converted into a returned error (`:152-156`).
8. `reserveSnapshotPaths(snapshotsDir, label)`; error returned verbatim (`:157-160`).
9. **Deferred `os.Remove(reserved.reservePath)`** — LIFO after the beacon-release defer, so the `.reserve` dir is removed *before* the beacon drops (`:161`).
10. `cloneTree(ctx, databaseDir, reserved.tmpPath)`; on error `os.RemoveAll(reserved.tmpPath)` (error discarded) then `"clone tree: %w"` (`:162-165`).
11. `os.Rename(reserved.tmpPath, reserved.finalPath)`; on error `os.RemoveAll(reserved.tmpPath)` (discarded) then `"rename tmp to final: %w"` (`:166-169`).
12. Return `Snapshot{Path: reserved.finalPath, Name: reserved.name, Created: reserved.created}` (`:170`). `Created` is the reservation candidate time (UTC), identical to the timestamp encoded in the name.

Documented (not enforced) preconditions in the package doc (`snapshot.go:7-27`): callers must not hold an open Dolt connection on the destination when calling `Restore`; `Take` on a live workspace requires the caller to hold the workspace SHARED lock, Dolt's journal lock, and the commit lock for the whole call. The package cannot import `store`, so these are documentation only (`:23-25`).

### 13.8 `reserveSnapshotPaths` — slot reservation

`snapshot.go:198-236`: `candidate := time.Now().UTC()` (`:199`); loop up to 1024 (`:200`); per attempt `name = formatName(candidate, label)`, `finalPath = snapshotsDir/name`, `reservePath = finalPath + ".reserve"`, `tmpPath = finalPath + ".tmp"` (`:201-204`). `os.Mkdir(reservePath, 0o755)` is the atomic claim (`:205`):
- **nil** → check `pathFree(finalPath)` and `pathFree(tmpPath)`. A stat error on either → `os.Remove(reservePath)` (discarded) and return the stat error (`:207-216`). Either path occupied → `os.Remove(reservePath)`, `candidate += 1ns`, continue (`:217-221`). Otherwise return the populated `reservedPaths` (`:222-228`).
- **`fs.ErrExist`** → `candidate += 1ns`, continue (`:229-230`).
- **any other error** → `"reserve %s: %w"` (`:231-232`).
Exhaustion → `"dbsnapshot: no free snapshot name after 1024 attempts"` (`:235`).

`pathFree(p)` (`:238-247`): `os.Stat` nil → `(false,nil)`; `fs.ErrNotExist` → `(true,nil)`; other → `(false, "stat %s: %w")`.

The `.reserve` sentinel sits at a **sibling** path specifically so Darwin's `Clonefile` (which requires the destination not to exist) is unaffected (`:192-195`).

Tests: `snapshot_test.go:540-560` (reserve, materialize `finalPath`, reserve again → different `finalPath`); `snapshot_test.go:457-486` (50 rapid consecutive Takes → 50 distinct names, `List` length 50); `snapshot_test.go:488-538` (20 concurrent goroutine Takes → no error, 20 distinct names, `List` length 20).

### 13.9 Copy engine, syscalls, exclusions, permissions

**`cloneTree` per platform.**
- **Darwin** (`clone_darwin.go:20-25`): `unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)` — a single APFS clonefile syscall over the whole tree. On nil, done. **Any** error falls back to `walkAndCopy(ctx, src, dst, plainFileCopy)`. The `Clonefile` call is uncancelable; `ctx` governs only the fallback walk (`:13-16`).
- **Linux** (`clone_linux.go:18-20`): always `walkAndCopy(ctx, src, dst, ficloneOrCopy)` — per-file `FICLONE`.
- **Other** (`clone_other.go:12-14`): `walkAndCopy(ctx, src, dst, plainFileCopy)`.

**`walkAndCopy`** (`snapshot.go:598-648`) — `filepath.WalkDir(src, ...)`, per entry:
1. A walk error is returned as-is (`:600-602`).
2. `ctx.Err()` is checked **per entry** and returned if non-nil (`:603-605`).
3. `rel = filepath.Rel(src, srcPath)`, error returned (`:606-609`); `dstPath = filepath.Join(dst, rel)` (`:610`).
4. **Directory**: `d.Info()` (error returned), `os.MkdirAll(dstPath, info.Mode().Perm())`, then `os.Chmod(dstPath, info.Mode().Perm())` to defeat umask filtering (`:612-622`).
5. **Symlink** (`d.Type()&os.ModeSymlink != 0`): `os.Readlink` then `os.Symlink` — the link is recreated, **never followed** (`:623-628`).
6. **Regular file**: if `isDoltJournalLockRel(rel)` → `return nil` (skip); else `copyFile(ctx, srcPath, dstPath)` (`:629-643`).
7. **Anything else** (FIFOs, sockets, devices) → `"dbsnapshot: unsupported file type at %s: %v"` (`:644-646`).

**The one exclusion — Dolt's journal LOCK.** `isDoltJournalLockRel(rel)` returns `strings.HasSuffix(filepath.ToSlash(rel), "/.dolt/noms/LOCK")` (`snapshot.go:589-591`). Because the check requires a leading `/`, it matches `<anything>/.dolt/noms/LOCK` — a database subdirectory under the copied root — and would **not** match a `rel` of exactly `.dolt/noms/LOCK` at the copy root. Rationale (`:630-639`): the file is contentless, Dolt recreates it at every engine open, and on Windows the mandatory `LockFileEx` hold means reading it through a second handle would fail the copy or drop the hold. It is explicitly noted that **Darwin's `Clonefile` fast path may still carry the file** (`:638-639`). `dolt_lock_skip_test.go:19-43` builds `src/links/.dolt/noms/LOCK` (0600) plus a sibling `manifest` (0644), runs `walkAndCopy(..., plainFileCopy)`, and asserts `LOCK` is absent while `manifest` is present. No other exclusion exists; the beacon lives in `snapshotsDir`, not `databaseDir`, so it is never part of a copy.

**`plainFileCopy`** (`snapshot.go:657-681`): `os.Open(src)` + `defer srcF.Close()` (`:658-662`); `srcF.Stat()` (`:663-666`); `os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())` — **`O_EXCL`**, so an existing destination fails (`:667-670`); `copyWithContext` with `_ = dstF.Close()` on error (`:671-674`); `dstF.Chmod(info.Mode().Perm())` to force exact source perms past umask, closing on error (`:675-679`); `return dstF.Close()` — the close error is part of the copy's outcome, deliberately **not** deferred, because a failed write-back-at-close (NFS commit-on-close, delayed-allocation ENOSPC) is the only signal the file is truncated (`:650-656`, `:680`). Ownership and timestamps are **not** propagated — only permission bits.

**`copyWithContext`** (`snapshot.go:695-707`): infinite loop checking `ctx.Err()` then `io.CopyN(dst, src, 32<<20)`. `io.EOF` ends with nil; any other error is returned. `io.CopyN` is used specifically so `*os.File.ReadFrom` unwraps the `LimitedReader` and keeps kernel fast paths (`copy_file_range`/`sendfile`) live (`:690-694`).

**`ficloneOrCopy`** (Linux, `clone_linux.go:22-56`): `os.Open(src)` + `defer Close` + `Stat` (`:23-31`); `os.OpenFile(dst, O_WRONLY|O_CREATE|O_EXCL, perm)` (`:32-35`); `unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd()))` — the `FICLONE` ioctl. On success: `Chmod(perm)` (close+return on error) then `return dstF.Close()` (`:39-46`). On FICLONE failure: `copyWithContext`, then `Chmod`, then `Close`, each closing and returning on error (`:47-55`).

**Permission evidence**: `snapshot_test.go:562-580` (a 0640 source produces a 0640 destination); `snapshot_unix_test.go:17-54` (with the process umask forced to `0o077`, `cloneTree` produces `dst/nested` at `0755` and `dst/nested/f` at `0644` — umask must not affect the snapshot; not parallel because `syscall.Umask` is process-wide, `:18-19`; unix-only because `syscall.Umask` is undefined on Windows, `:13-16`).

**Cancellation evidence**: `residue_test.go:367-396` — `walkAndCopy` over `src/a` and `src/z` where the copy callback cancels after the first file; asserts `errors.Is(err, context.Canceled)`, exactly 1 file copied (WalkDir order is lexical), and `dst/z` absent.

**Unsupported-entry-type evidence**: `snapshot_unix_test.go:63-93` — source has a regular file plus a FIFO from `syscall.Mkfifo(..., 0o644)`. The test tolerates either outcome (Darwin's `Clonefile` clones FIFOs wholesale and the Take succeeds, logged at `:79-83`; walk-based paths refuse) and asserts only the residue contract: no entry in `snapshotsDir` satisfies `IsProducerArtifactName`.

### 13.10 `List`

`snapshot.go:256-284`: `os.ReadDir(snapshotsDir)`; `fs.ErrNotExist` → `([]Snapshot{}, nil)` (empty, non-nil slice); any other error → `"read snapshots dir: %w"` (`:257-263`). Skips **non-directory** entries (`:266-268`) — so the beacon file and any regular file is skipped. Skips entries where `parseName(name)` is not ok (`:270-273`). Builds `Snapshot{Path: filepath.Join(snapshotsDir, name), Name: name, Created: created}` (`:274-278`). Sorts **newest-first** by `Created.After` via non-stable `sort.Slice` (`:280-282`).

Tests: `snapshot_test.go:29-55` (3 takes, newest-first); `snapshot_test.go:57-80` (`snap-old-junk` dir, `backup_2024` dir, `README.txt` file → 0 results); `snapshot_test.go:82-99` (`1700000000000000000.tmp` and `.reserve` dirs → 0 results).

### 13.11 `Restore`

`snapshot.go:293-327`, in order:
1. `validateSnapshotName(name)`; error returned (`:294-296`).
2. `snapshotPath = filepath.Join(snapshotsDir, name)` (`:297`).
3. **`os.Lstat`**, not `Stat` — a symlink is not followed (`:301`). `fs.ErrNotExist` → **bare `ErrSnapshotMissing`**, not wrapped (`:302-305`); other error → `"stat snapshot: %w"` (`:306`).
4. `!info.Mode().IsDir()` → `"snapshot is not a directory: %s"` — this is what rejects a symlink (`:308-310`).
5. **Rotate the existing database dir.** `os.Stat(databaseDir)`:
   - exists → `rotatedPath = fmt.Sprintf("%s.pre-restore-%d", databaseDir, time.Now().UTC().UnixNano())`, then `os.Rename(databaseDir, rotatedPath)`; rename error → `("", "rotate existing database dir: %w")` (`:313-317`).
   - `fs.ErrNotExist` → no rotation, `rotatedPath` stays `""` (`:318-319`).
   - other stat error → `("", "stat database dir: %w")` (`:320-321`).
6. `os.Rename(snapshotPath, databaseDir)` — this **moves** the snapshot directory; **the snapshot no longer exists in `snapshotsDir` afterwards**. Error → `(rotatedPath, "install snapshot at database dir: %w")` — note the rotated path *is* returned alongside the error so the caller can undo (`:323-325`).
7. Return `(rotatedPath, nil)` (`:326`).

`validateSnapshotName` (`snapshot.go:404-415`): empty → `"dbsnapshot: snapshot name is empty"`; `name != filepath.Base(name)` → `"dbsnapshot: snapshot name must be a single path component: %q"`; `parseName` not ok → `"dbsnapshot: snapshot name does not match the <unix-ns>[-<label>] scheme: %q"`.

Tests: `snapshot_test.go:101-142` (round trip — source mutated after the snapshot, subtree deleted, file rewritten; after restore `top.txt`=="top", `sub/deep.txt`=="deep", the rotated dir holds the mutated `top.txt`=="MUTATED", rotated path non-empty); `snapshot_test.go:144-172` (source removed before restore ⇒ rotated == `""`, content restored); `snapshot_test.go:174-207` (all of `""`, `"."`, `".."`, `"../sibling"`, `"sub/dir"`, `"/etc/passwd"`, `"1700000000-../etc"`, `"snap-1700000000-abc"`, `"1700000000000000000.tmp"`, `"1700000000000000000-label.tmp"`, `"trailing/"` are rejected); `snapshot_test.go:209-242` (a symlink named `1700000000000000000` inside `snapshotsDir` pointing outside is refused; the database dir still exists — no rotation happened — and the symlink is still present, not moved); `snapshot_test.go:244-255` (nonexistent snapshots dir + canonical name → `errors.Is(err, ErrSnapshotMissing)`).

### 13.12 `Prune` / `PruneMatching`

`Prune(snapshotsDir, keep)` = `PruneMatching(snapshotsDir, keep, nil)` (`snapshot.go:334-336`).

`PruneMatching` (`snapshot.go:351-383`):
1. `keep <= 0` → `"dbsnapshot: keep must be > 0"` (`:352-354`).
2. `List(snapshotsDir)`; error propagated verbatim (`:361-364`).
3. `match == nil` ⇒ every snapshot matches; otherwise filter by `match(s.Name)` **preserving newest-first order** (`:365-373`).
4. `len(matched) <= keep` → nil, nothing removed (`:374-376`).
5. For each `matched[keep:]` (the oldest), `os.RemoveAll(snapshot.Path)`; the first error → `"remove snapshot %s: %w"`, aborting the rest (`:377-381`).

Non-matching snapshots are never removed regardless of age (`:338-341`). Residue collection deliberately does **not** run here (`:355-360`).

Tests: `snapshot_test.go:257-293` (7 takes, keep=3 ⇒ `List` len 3 and on-disk non-`.tmp` dir count 3); `snapshot_test.go:295-303` (keep=0 and keep=-1 both error); `snapshot_test.go:305-310` (empty dir, keep=5, no error); `snapshot_test.go:318-366` (6 `kind-a` + 5 `kind-b` takes, `PruneMatching(dir, 2, contains "-kind-a")` ⇒ 2 matching remain, all 5 non-matching untouched); `snapshot_test.go:371-397` (`PruneMatching(..., 2, nil)` over 5 → 2 remain).

This is the mechanism behind the three disjoint store-side retention budgets (`migrationSnapshotRetention=10`, `reconcileSnapshotRetention=10`, and the downgrade budget) described in §5.4 and §9.

### 13.13 Residue: what counts, what is destroyed, what is spared

**Destruction predicates.** `isCollectibleResidue(name)` (`snapshot.go:459-464`) = `isCollectorCondemnedName(name) || isCollectibleArtifactName(name)`.

`isCollectibleArtifactName(name)` (`snapshot.go:469-477`): for suffix in `[".tmp", ".reserve"]` — note **not** `.condemned` — if `strings.CutSuffix` matches, return whether the remaining head satisfies `parseName`; returns on the first matching suffix. Otherwise false.

`isCollectorCondemnedName(name)` (`snapshot.go:482-495`): strip `".condemned"` (must be present); find `strings.LastIndexByte(head, '.')` (must exist); the segment after that dot must satisfy `parsePositiveDigits`; and `head[:dot]` must satisfy `isCollectibleArtifactName`. So the **only** collectible condemned shape is `<unix-ns>[-<label>].{tmp|reserve}.<positive-ns>.condemned`.

The design rule stated in-code (`snapshot.go:449-458`): **reject broadly (`IsProducerArtifactName`), delete narrowly (`isCollectibleResidue`)** — a foreign `backup.tmp` or `backup.condemned` an operator parked in the directory is untouchable, because a suffix is never provenance over a directory lit does not own.

**`CollectOrphanedResidue`** (`residue.go:51-83`):
1. `os.Stat(snapshotsDir)`: `fs.ErrNotExist` → `nil` (no-op; probing the beacon would otherwise create the directory as a side effect, `:52-57`); other error → `"stat snapshots dir: %w"` (`:58`).
2. `filelock.Acquire(context.Background(), producerBeaconPath(snapshotsDir), true /*exclusive*/, 1 /*attempt*/, 0 /*delay*/)` — a single non-sleeping probe (`:60-62`).
   - Real error → `"probe snapshot producer beacon: %w"` (`:63-65`).
   - **Contention → `return nil`, silently skipping collection** (`:66-68`). Rationale: a live producer exists; its own later take collects (`:32-38`).
3. `condemned, condemnErr := condemnResidue(snapshotsDir)` (`:69`).
4. `release()`; if `relErr != nil || condemnErr != nil` → `errors.Join(condemnErr, relErr)` and **the RemoveAll pass is skipped entirely** (`:70-75`).
5. The beacon is released **before** the deletion pass so a multi-gigabyte corpse delete does not extend the window producers retry against (`:43-46`).
6. Per condemned path: `os.RemoveAll(path)`; failures collected as `"remove condemned residue %s: %w"`; returns `errors.Join(removeErrs...)` (`:76-82`).

**Liveness discriminator**: no age thresholds, no PID files — every live `Take` holds the beacon shared for its whole reserve→copy→rename window, so an exclusive acquire proves all producer artifacts present are orphaned (`residue.go:28-41`). Documented caveat: a still-running **pre-beacon** binary's Take holds no beacon and reads as dead (`:38-41`).

**`condemnResidue`** (`residue.go:97-119`): `os.ReadDir(snapshotsDir)`, error → `"read snapshots dir: %w"` (`:98-101`); for every entry — **directories and regular files alike, no `IsDir` filter** — skip unless `isCollectibleResidue(name)` (`:103-107`); if the name does not already end in `.condemned`, rename to `fmt.Sprintf("%s.%d%s", path, time.Now().UnixNano(), condemnedSuffix)`, with a rename error aborting the whole classification as `"condemn residue %s: %w"` (`:109-114`); already-`.condemned` entries (which reached here only by passing `isCollectorCondemnedName`) are added as-is (`:116`). The fresh nanosecond stamp guarantees a rename can never collide with a corpse from an earlier interrupted collection, even if a producer reuses the exact original stamp (`:92-96`).

**Residue tests.**
- `residue_test.go:18-32` `fabricateDeadResidue` plants `<stamp>.tmp/nested/partial` (0644 file, 0755 dirs) plus a `<stamp>.reserve` dir — the exact shape a killed Take leaves.
- `residue_test.go:39-83`: after a real Take (label `keep-me`), plus fabricated residue at `1700000000000000001`, plus a leftover `1700000000000000002.tmp.1700000000000000005.condemned`, plus `snap-old-junk`. Collection removes every `IsProducerArtifactName` entry; the real snapshot and the legacy directory survive.
- `residue_test.go:91-124`: with a simulated live producer holding the beacon shared, collection returns nil and leaves `.tmp` and `.reserve` untouched; after `release()`, the same call removes both.
- `residue_test.go:128-137`: a never-created directory is a no-op, returns nil, and collection must **not** create the directory.
- `residue_test.go:146-176`: `Take` collects residue at entry and still succeeds.
- `residue_test.go:185-230` — the exact **spared** set: `backup.tmp`, `backup.condemned`, `backup.tmp.1700000000000000001.condemned`, `1700000000000000006.tmp.condemned` (stampless), `+1700000000000000006.tmp` (signed head), `1700000000000000006.tmp.+1.condemned` (signed collector stamp), `1700000000000000006-my.backup.tmp` (dotted label), plus a foreign **regular file** `notes.reserve`. Simultaneously **collected**: `1700000000000000006.tmp`/`.reserve` and `1700000000000000007.reserve.1700000000000000002.condemned`.
- `residue_test.go:237-252`: residue whose label is 300 `x` (truncated by `formatName`) is still condemnable and collected — the condemnation rename stays inside NAME_MAX.
- `residue_test.go:259-283`: stamp reuse — a fresh `<stamp>.tmp` plus an old `<stamp>.tmp.1700000000000000001.condemned` are both removed by one collection.
- `residue_test.go:289-317`: with `nested` chmod'd to `0555` so the corpse cannot be removed, `Take` still succeeds and the snapshot exists (skipped when `os.Getuid() == 0`, `:290-292`).
- `residue_test.go:441-469`: the same unremovable-corpse setup makes `CollectOrphanedResidue` return a non-nil error whose text contains `"condemned"`; after fixing permissions, the next collection succeeds (convergence). Also skipped as root (`:442-444`).
- `residue_test.go:321-337` `restoreCondemnedPerms` re-chmods `*.condemned/nested` and `*.tmp/nested` back to 0755 for TempDir cleanup.

### 13.14 Complete error/abort catalog for `dbsnapshot`

| Site | Condition | Result |
|---|---|---|
| `snapshot.go:115` | ctx already canceled/expired | raw `ctx.Err()` |
| `snapshot.go:120` | `os.Stat(databaseDir)` fails | `stat database dir: %w` |
| `snapshot.go:123` | source not a directory | `database dir is not a directory: %s` |
| `snapshot.go:126` | `MkdirAll(snapshotsDir,0755)` fails | `create snapshots dir: %w` |
| `snapshot.go:137` | collection error | stderr only; Take proceeds |
| `snapshot.go:141` | beacon acquire real error | `acquire snapshot producer beacon: %w` |
| `snapshot.go:144` | beacon contention after 20×50ms | wraps `ErrSnapshotsBusy` |
| `snapshot.go:154` | beacon release fails | stderr only |
| `snapshot.go:210/215` | stat of final/tmp path fails | `stat %s: %w`; reservation aborts, `.reserve` removed |
| `snapshot.go:232` | `Mkdir(.reserve)` non-EEXIST error | `reserve %s: %w` |
| `snapshot.go:235` | 1024 attempts exhausted | `dbsnapshot: no free snapshot name after 1024 attempts` |
| `snapshot.go:164` | `cloneTree` fails | `.tmp` RemoveAll'd; `clone tree: %w` |
| `snapshot.go:168` | rename `.tmp`→final fails | `.tmp` RemoveAll'd; `rename tmp to final: %w` |
| `snapshot.go:262` | `ReadDir` fails (not ENOENT) | `read snapshots dir: %w` |
| `snapshot.go:406` | empty restore name | `dbsnapshot: snapshot name is empty` |
| `snapshot.go:409` | multi-component restore name | `dbsnapshot: snapshot name must be a single path component: %q` |
| `snapshot.go:412` | name fails `parseName` | `dbsnapshot: snapshot name does not match the <unix-ns>[-<label>] scheme: %q` |
| `snapshot.go:304` | snapshot path absent | `ErrSnapshotMissing` (bare) |
| `snapshot.go:306` | Lstat other error | `stat snapshot: %w` |
| `snapshot.go:309` | snapshot path not a dir (incl. symlink) | `snapshot is not a directory: %s` |
| `snapshot.go:316` | rotate rename fails | `rotate existing database dir: %w`, rotated returned as `""` |
| `snapshot.go:321` | stat databaseDir other error | `stat database dir: %w` |
| `snapshot.go:324` | install rename fails | `(rotatedPath, install snapshot at database dir: %w)` |
| `snapshot.go:353` | `keep <= 0` | `dbsnapshot: keep must be > 0` |
| `snapshot.go:379` | `RemoveAll` of a snapshot fails | `remove snapshot %s: %w` (aborts loop) |
| `snapshot.go:645` | unsupported file type in walk | `dbsnapshot: unsupported file type at %s: %v` |
| `residue.go:58` | stat snapshotsDir other error | `stat snapshots dir: %w` |
| `residue.go:64` | beacon probe real error | `probe snapshot producer beacon: %w` |
| `residue.go:67` | beacon contention | `nil` (silent skip) |
| `residue.go:74` | condemn or release error | `errors.Join(condemnErr, relErr)`; delete pass skipped |
| `residue.go:100` | ReadDir fails | `read snapshots dir: %w` |
| `residue.go:112` | condemnation rename fails | `condemn residue %s: %w` (aborts classification) |
| `residue.go:79` | RemoveAll of a corpse fails | joined `remove condemned residue %s: %w` |
