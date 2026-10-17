# lit: every dependency on Dolt commit history (inventory)

Tree: `/Users/bmf/code/links-issue-tracker/.claude/worktrees/spike-3k6u-8ka` at `7a72b8cf` (v0.20.1). All line numbers are from that tree. The Dolt behaviour I cite comes from the pinned fork `github.com/promptctl/dolt/go@v0.40.5-0.20261017180803-c1afca53026d` (abbreviated `dolt:` below). I read that source. I did not run anything.

## Findings that bear on the design

1. **A missing merge base looks exactly like unrelated histories.** `mergeBase` (`internal/store/sync_reconcile.go:1216-1234`) folds "no common ancestor" (and `sql.ErrNoRows`/NULL) into `hasBase=false`. Nothing distinguishes "the base was cut off by the bound" from "the two stores never shared history". So if history is cut below a live divergence's merge base, that shared-history divergence goes down the unrelated path:
   - The automatic reconcile only detects it (`SyncReconcileUnrelated`, `:406-413`).
   - Resolving it needs an explicit `combine` or an owner-approved `take`.
   - `combine` merges against an *empty* base, so every field is treated as changed on both sides (`internal/merge/resolve.go:76-86`). Merge results therefore change.
2. **Re-rooting the shared remote turns every existing clone into an unrelated history as lit sees it today.**
   - Fetch moves the tracking ref regardless of ancestry: `dolt: sqle/dprocedures/dolt_fetch.go:102` uses `Force: true`, and lit's own clone landing does the same (`internal/store/fetched_head.go:79-86,204`).
   - After that, freshness reports Diverged with whole-history counts, the reconcile reports Unrelated, and push fails with Dolt's `no common ancestor`. All four entry points are traced in the last section.
   - A re-root is invisible to clones only if every clone's local branch ends up descending from the *same* new root commit hash. A Dolt commit hash is content-addressed over metadata and parents, so that requires a bit-identical commit on every machine.
3. **A merge needs history back to the merge base, and no current bound limits that depth.**
   - The ahead/behind counts and divergence date (`sync.go:986-1000`), the folded-chain provenance replay (`sync_reconcile.go:513-552,622-640`) and the base export (`:472`) all walk from each head back to the merge base.
   - On the unrelated `combine` / `take local` paths they walk the *entire* local chain to its root.
   - The merge base is as old as the longest time a clone has gone without syncing. Nothing in lit caps that.
4. **Nothing in lit reads the commit graph for history.** There are no non-test reads of `dolt_history_*`, `dolt_diff*`, `dolt_commits`, `dolt_commit_ancestors` or `HASHOF`, and no calls to `DOLT_REBASE`, `DOLT_REVERT`, `DOLT_CHERRY_PICK` or `DOLT_PULL`. Event attribution is table data (`issue_events.stream_id/workspace_id`, see `internal/store/event_attribution_test.go:19-26`), not commit metadata. The only commit metadata lit reads is committer, email, date and message, in `readFoldedChain` (`sync_reconcile.go:520-536`). It writes them back onto replayed commits through `--date`/`--author` (`internal/store/commit_lock.go:337-345`).
5. **"pre-migrate" names two different things.**
   - A Dolt checkpoint **branch** prefix: `migration_runner.go:23`, created by `checkpoint.go:28`.
   - A **filesystem snapshot** label: `migrate_snapshot.go:29`, a whole-directory copy made by `dbsnapshot.Take`.
   - `migrate_snapshot.go` contains no Dolt-branch code.
6. **Some local refs keep pre-bound history reachable, so `DOLT_GC` cannot reclaim it:** the `pre-migrate-*` and `pre-drift-repair-*` branches, leftover reconcile scratch branches, and the tracking ref until its next fetch. Filesystem snapshots carry the full pre-bound graph. Restoring one after the remote has been re-rooted produces an unrelated clone.
7. **Two producers of unrelated histories already exist:**
   - The lifeboat promote installs a freshly-bootstrapped store with a new root (`internal/store/candidate.go:108`, `promote.go`).
   - `lit init` on a store that already has tickets skips the adopt (`internal/cli/init_sync.go:202-210`). That store then meets the remote with no shared ancestor.
8. **`SyncResetToRemoteHead` (`sync.go:372-385`) is never called in production.** Only tests call it: `sync_test.go:1145,1278,1488` and `sync_reconcile_test.go:476`. `lit init` adopts by `DOLT_CLONE` instead.

---

## Table 1: sites that read Dolt history or depend on the shape of the commit graph

Depth key: **HEAD** = only the tip of a ref. **last-sync** = back to the commit the tracking ref names (the last push or fetch). **merge-base** = back to the common ancestor of local and remote. **root** = the whole chain.

| file:line | function | what it reads (query/API) | what it NEEDS from history | depth needed | what happens today if the needed commit is gone / no common ancestor |
|---|---|---|---|---|---|
| internal/store/rawdump.go:128-134 | `readDoltHead` | `SELECT commit_hash FROM dolt_log() LIMIT 1` | Only HEAD's hash. Callers: `DumpRaw` (rawdump.go:100), `verifyHeadUnchanged` (promote.go:165), `CreateCheckpoint` (checkpoint.go:22), reconcile `localHead` (sync_reconcile.go:353), `liftedBase`/`replayedCommit` (:924,:955) | HEAD | HEAD always exists. If a re-root changes HEAD's hash, the promote gate refuses with `ErrWorkspaceAdvanced` (promote.go:169-173). Nothing else compares it. |
| internal/store/sync.go:794-800 | `headCommitWithinLock` | `SELECT commit_hash FROM dolt_log() LIMIT 1` | HEAD hash, read before the push. It becomes `SyncPushResult.Head`, which feeds `RecordPushedHead` | HEAD | n/a |
| internal/store/sync.go:854-892 | `SyncStatus` (`lit sync status`) | `SELECT commit_hash, message FROM dolt_log() LIMIT 1` (:863); `dolt_status` (:874) | HEAD hash and message; working set compared with HEAD | HEAD | n/a |
| internal/store/sync_reconcile.go:1163-1169 | `commitHashOfRef` | `SELECT commit_hash FROM dolt_log(?) LIMIT 1` on `remotes/<r>/<b>` | The tip commit of the tracking ref. Callers: `captureReconcilePlan` (:357), `trackingHeadHash` (sync_schema_guard.go:138) | HEAD of tracking ref | If the tip is unreadable: error `read head of %q` (:1166). The caller fails. |
| internal/store/sync_schema_guard.go:127-143, 167-180 | `trackingHeadHash`, `schemaVersionAtCommit` (via `guardRemoteSchemaAhead` :75, `guardCommitSchemaAhead` :101) | `COUNT(*) FROM dolt_remote_branches WHERE name=?`; `SELECT MAX(version_id) FROM goose_db_version AS OF '<hash>'` | Only the tables at the remote head. Called before every push (sync.go:750) and every replay (sync_reconcile.go:447) | HEAD of tracking ref | A missing table (1146) reads as schema 0. Any other read error becomes `read schema version at %q`, and the push or reconcile fails. Ancestry is not checked, so an unrelated head passes. |
| internal/store/sync.go:901-954, 986-1000 | `SyncFreshness` → `commitRangeStats` | `dolt_remote_branches` existence; `ACTIVE_BRANCH()`; `SELECT COUNT(*), UNIX_TIMESTAMP(MIN(date)) FROM dolt_log('<from>..<to>')`, run once for tracking..local (ahead) and once for local..tracking (behind) | Every commit on each side since the merge base: the count, and the date of the oldest (`OldestDivergedUnix`, used for escalation age) | merge-base, on both sides | **No error.** With no common ancestor, each range returns the whole reachable chain: ahead = all local commits, behind = all remote commits. `State()` = Diverged (storage/sync.go:103-116), and the age is the older root's date. If a commit in the range is a ghost (shallow) commit: not verified. |
| internal/cli/doctor.go:130-170, 204-235, 117-119 | `resolveDoctorSyncFreshness`, `printSyncFreshness`, `doctorDivergenceExit` | `SyncFreshness` (above) | The same ahead/behind counts and divergence age | merge-base | Unrelated is shown as an ordinary "diverged … N local / M remote" with whole-history counts. `persistent()` (span > 10 or age ≥ 24h, sync_failure.go:65-66) holds, so doctor exits ExitConflict with class `diverged_unresolved`. Doctor never calls merge-base, so it never says "unrelated". |
| internal/cli/sync_staleness.go:95-115, 195-216 | `syncStalenessLines` / `printStalenessWarning` (read-command banner) | `resolveDoctorSyncFreshness` → `SyncFreshness` | The ahead count when the state is Ahead | merge-base | Only the `SyncAhead` arm prints. Diverged and unrelated print nothing here; they are left to the sync-failure block. |
| internal/store/sync.go:470-495 (ff at :480-481) | `settleReceivedWithinLock` (from `SyncReceive` :411, `SyncSettleReceived` :444) | `SyncFreshness`; `CALL DOLT_MERGE('--ff-only', 'remotes/<r>/<b>')` | The freshness counts; local HEAD must be an ancestor of the tracking ref (Dolt's ff check) | merge-base (counts); last-sync (ff) | Unrelated reads as `SyncReceiveDiverged` (:485-486). The ff never runs. |
| internal/store/sync.go:247-335 | `SyncPull` | `SyncReceive` + `SyncReconcile` + a `SyncFreshness` re-read (:283) | Everything those two need | merge-base | `SyncReconcileUnrelated` → `SyncPullUnrelated`, with the receive's counts and inventory carried over (:292-300) |
| internal/store/sync_reconcile.go:1216-1234 (+1206-1211) | `mergeBase` / `isNoCommonAncestor` | `SELECT DOLT_MERGE_BASE(?, ?)` (localHead, `remotes/<r>/<b>`) | A common ancestor of local HEAD and the tracking ref | merge-base (any age) | "no common ancestor" error, `ErrNoRows`, or NULL/empty all become `hasBase=false`. In `reconcile` (:389-414), detect-only stops with `SyncReconcileUnrelated` and writes nothing. A base lost to truncation is indistinguishable from unrelated. With a shallow clone, the merge base can be a ghost commit (`dolt: doltdb/commit.go:173-175` returns it as an address with no commit). The later reset to it would then fail (next row); not verified at runtime. |
| internal/store/sync_reconcile.go:334-366 | `captureReconcilePlan` | `SyncFreshness`, `active_branch()`, `readDoltHead`, `commitHashOfRef`, `mergeBase` | Both heads, the merge base, the counts. Used by `SyncReconcile`, `SyncReconcileResolved`, `SyncReconcileCombine`, `SyncResolveUnrelated` | merge-base | Not diverged → `SyncReconcileNotDiverged`. No base → `shared=false` → the unrelated branch. |
| internal/store/sync_reconcile.go:470-478 (+1140-1148, 1110-1127) | `reconcileFromAnchors` → `exportAtCommit(base)` → `resetAndLift` | `CALL DOLT_CHECKOUT(<read scratch>)`; `CALL DOLT_RESET('--hard', <baseCommit>)`; `DOLT_CLEAN`; migration lift; `Export` | The full table contents at the merge-base commit (the Tier-1 "who moved this field" signal, merge/resolve.go:83-86) | merge-base | If the base commit is absent or a ghost, `DOLT_RESET` errors and the reconcile returns `read export at %q`. The receive surfaces it as `diverged_unresolved` with the error as cause (sync_receive.go:478-487). The pull returns an error. |
| internal/store/sync_reconcile.go:766-833 | `mergeAndReplay` | `exportAtCommit(localHead)`, `exportAtCommit(remoteHead)` (:767-774), then `readFoldedChain` (:813) | Both heads' contents, then the folded chain | HEAD (heads), merge-base (chain) | See the next two rows |
| internal/store/sync_reconcile.go:513-552 | `foldedChainRange` / `readFoldedChain` | `SELECT commit_hash, committer, email, date, message FROM dolt_log('<remoteHead>..<localHead>')` | Every local commit the remote spine lacks, with author, date and message, to replay as provenance. Callers: `mergeAndReplay` (:813) and `takeLocalOntoRemoteHead` (sync_unrelated_take.go:323) | merge-base (shared); **root of the local chain** (combine / take-local on unrelated) | Gaps are not detected. If the first row ≠ localHead: error `folded chain starts at …` (:541-546). On unrelated histories the range is the whole local chain. |
| internal/store/sync_reconcile.go:589-640 | `foldStepper.step` | `exportAtCommit(readBranch, c.hash)`, i.e. `DOLT_RESET --hard` to each folded commit, lift, export | The full table contents at every folded commit | merge-base (shared); root (combine / take-local) | A missing or ghost commit makes `DOLT_RESET` error. `commitReplayAndAdvance` returns it, and the data branch is untouched (the reset to the replayed commit, :988, comes later). |
| internal/store/sync_reconcile.go:907-1006 | `commitReplayAndAdvance` | `DOLT_CHECKOUT spine`; `resetAndLift(remoteHead)` (:911); commits; `readDoltHead`; `countCommitsInRange` (:962 → :1020-1025, `SELECT COUNT(*) FROM dolt_log('<liftedBase>..<replayed>')`); `DOLT_CHECKOUT data`; `DOLT_RESET('--hard', replayedCommit)` (:988) | The remote head's contents. The count covers only commits this run just created. | HEAD (remote) + this run's commits | n/a for the count. A remote head that is missing fails at :911. |
| internal/store/sync_reconcile.go:854-873, 1040-1062, 1070-1095 | `runOnReconcileScratch`, `cleanupReconcileScratch`, `sweepStaleReconcileScratch` | `DOLT_CHECKOUT -B <scratch> <localHead>`; `DOLT_BRANCH -D`; `SELECT name FROM dolt_branches WHERE name LIKE 'links-reconcile-scratch-%'` | Only localHead | HEAD | n/a. A leftover scratch branch pins old commits until the next reconcile sweeps it. |
| internal/store/sync_unrelated.go:23-67, 77-137 | `unrelatedInventory`, `queryAsOf`, `issuesAtCommit`, `purgesAtCommit` | `SELECT id, created_at FROM issues AS OF '<hash>'`; `SELECT issue_id, issue_created_at, purged_at, purged_by FROM purges AS OF '<hash>'` at localHead and remoteHead | Only the two heads' tables | HEAD (both sides) | A missing table (1146) is treated as empty. Any other error becomes `read at %q`. Reached only on the unrelated path. |
| internal/store/sync_unrelated_take.go:100-168, 176-229 | `SyncResolveUnrelated`, `applyUnrelatedTake` | `captureReconcilePlan` (requires `!shared`, :128-135); `takeApprovalToken(localHead, remoteHead, choice)` (:38-41) | That no merge base exists, and both head hashes (for the approval token) | merge-base (to prove there is none) | Shared history → refused (`take applies only to unrelated histories`). Token mismatch → `OwnerApprovalRequiredError` (:149-160) and nothing mutates. |
| internal/store/sync_unrelated_take.go:238-253 + sync.go:393-398 | `takeRemoteHead` → `resetHardToRef` | `CALL DOLT_RESET('--hard', 'remotes/<r>/<b>')` | The tip of the tracking ref | HEAD of tracking ref | n/a (adopts the remote wholesale) |
| internal/store/sync_unrelated_take.go:262-298 | `takeRemoteCarryingPurges`, `purgesMissingAt` | `exportAtCommit` local and remote heads; `purges AS OF '<remoteHead>'`; `commitReplayAndAdvance` with an empty chain | Only the two heads | HEAD | n/a |
| internal/store/sync_unrelated_take.go:313-343 | `takeLocalOntoRemoteHead` | `exportAtCommit` local and remote; `readFoldedChain(remoteHead, localHead)`; the replay | The entire local chain, for provenance | root (local) | As for `readFoldedChain` / `foldStepper` |
| internal/store/sync_reconcile.go:486-492 | `combineFromAnchors` (`lit sync reconcile combine`, and `resolve` on an unrelated fork) | `mergeAndReplay` with `model.Export{}` as the base | Both heads, plus the entire local chain | root (local) | The same as above. The empty base turns the merge into a two-way union. |
| internal/store/sync.go:736-790 (DOLT_PUSH :774-782) | `pushWithinLock` (`SyncPush` :652, `SyncCompactAndPush` :671, `SyncPushFromClone` :811) | `CALL DOLT_PUSH([--force], <remote>, 'HEAD:<branch>')`. Inside Dolt: `CanFastForward(destRef, HEAD)` (`dolt: env/actions/remotes.go:58-65`), then `PullChunks` of HEAD's closure into the remote (:73-77) | The remote branch head must be an ancestor of local HEAD. The remote must already hold, or be sent, every chunk reachable from HEAD. | last-sync (ancestry); all chunks reachable from HEAD that the remote lacks | No common ancestor: `GetCommitAncestor` returns `ErrNoCommonAncestor` (`dolt: doltdb/commit.go:184-192`), which is not `ErrCantFF`. `DoPush` breaks (:124-126) and `dolt_push.go:125-133` returns it raw. lit wraps it as `push remote %q: no common ancestor` (sync.go:785). From a shallow store with missing chunks: `ErrShallowPushImpossible`. `--force` skips the check. |
| internal/store/sync.go:811-852 | `SyncPushFromClone` (mirror) | `pushWithinLock`; on rejection, `DOLT_FETCH` + `SyncFreshness` + `headCommitWithinLock` | After a rejection, whether the remote already carries HEAD | merge-base (freshness) | Unrelated: `Synced && Ahead>0` → the original `pushErr` is returned (:838-839) |
| internal/store/sync.go:185-207, 422-433, 827-830; internal/cli/sync_receive_clone.go:82; internal/cli/sync_reconcile_cmd.go:624 | `SyncFetch`, `SyncReceive` fetch, push-from-clone recheck, receive-clone fetch, `freshReconcileTarget` | `CALL DOLT_FETCH(<remote>)`, run with `Force: true` (`dolt: sqle/dprocedures/dolt_fetch.go:102`); `PullChunks` of new remote heads; tracking ref set by `SetHeadToCommit` (`dolt: env/actions/remotes.go:649-653`) | Nothing from local history. Downloads the chunks reachable from the remote head that are not already present. | none locally | The tracking ref moves onto any history, related or not, with no error |
| internal/store/fetched_head.go:99-182, 189-224 | `LandFetchedHead`, `landTrackingRef`, `resolveTrackingHead` (receive: clone → live) | Go API: `doltdb.LoadDoltDBWithParams`; `ddb.HasRef`/`ResolveCommitRef`/`HashOf` (:213-221); `ddb.PullChunks(…, []hash.Hash{fetched}, …)` (:165); `ddb.ReadCommit(fetched)` + `ToCommit` (:190-196); `ddb.SetHead(trackingRef, fetched)` (:204) | The fetched head must be a full (non-ghost) commit after the copy. No ancestry is needed: it sets the ref "forwards, backwards, or onto an unrelated history" (:79-86). | HEAD of the clone's tracking ref (+ its chunk closure) | Absent → `FetchedHeadAbsent`. Not a commit → `the copied chunks do not hold it as a commit`. Ghost → `holds it only as a ghost commit` (:191-196). Unrelated → `FetchedHeadMoved`. |
| internal/store/pushed_head.go:175-214 | `settlePushedHead` (`RecordPushedHead` :120; called from internal/cli/sync_bg.go:635) | Go API: `ddb.ReadCommit(head)`/`ToCommit` (:178-185); `ddb.CanFastForward(trackingRef, pushedCommit)` (:193), which walks the graph through `GetCommitAncestor`; `ddb.SetHead` (:206) | The pushed head (from the clone) must be a full commit in the live store, and must share an ancestor with the live tracking ref (normally that ref is an ancestor of the head) | last-sync | Head missing → `this store does not hold it as a commit`. Ghost → `only as a ghost commit`. No common ancestor → `ErrNoCommonAncestor` hits the `ffErr != nil` arm (:200-201): `compare the ref with the pushed head`. Diverged → `the ref has diverged from the pushed head` (:202-203). The mirror records `record the pushed head on the live store (freshness reads say "not pushed" until the next fetch)` (sync_bg.go:647-659). |
| internal/store/checkpoint.go:17-37, 45-50, 54-81, 85-102 | `CreateCheckpoint`, `ResetToCheckpoint`, `ListCheckpoints`, `PruneCheckpoints` | `readDoltHead`; `CALL DOLT_BRANCH(?)`; `CALL DOLT_RESET('--hard', <branch>)`; `SELECT name, hash FROM dolt_branches WHERE name LIKE ?`; `CALL DOLT_BRANCH('-d','-f',?)` | HEAD at migration start, kept as branch `pre-migrate-<ns>` (migration_runner.go:23,676) or `pre-drift-repair-<ns>` (:30,1257). Reset to it only by the same run, on failure (:777, :1276). | HEAD (at checkpoint time) | The branch pins its own commit, so it cannot go missing. If the branch was deleted: `migration vN failed and Dolt reset to %q failed …; restore from dbsnapshot` (:777-782). Five branches are kept per prefix (:24). They are never pushed, but keep old history reachable. |
| internal/store/adopt.go:357-370 (called from internal/cli/init_sync.go:140) | `cloneRemoteDatabase` (`AdoptRemoteByClone`) | `CALL DOLT_CLONE('--remote', r, '--branch', b, url, 'links')`, with no `--depth` | The whole history reachable from the remote branch (a full clone) | root (whatever the remote holds) | lit does not check. The clone gets whatever the remote holds. If the remote were re-rooted, the clone starts on the new root. |
| internal/store/sync.go:520-539 (+compaction.go:83-106) | `compactWithinLock` (`SyncCompact`, `CompactIfDue`, `SyncCompactAndPush`) | `CALL DOLT_GC()` or `DOLT_GC('--full')` | Not a reader. Keeps every chunk reachable from any ref: `master`, `pre-migrate-*`, `pre-drift-repair-*`, leftover scratch branches, `remotes/*/*`, working sets | all refs | Bounded-away history is reclaimed locally only once no local ref reaches it |
| internal/cli/sync_receive.go:238-304, 555-616, 649-718, 464-509 | `receiveOnce`, `performSyncReceive`, `performInlineReconcile`, `inlineSyncFailure` | `SyncSettleReceived` (:584) → `SyncReconcile` (:640) | Everything `SyncFreshness` and `SyncReconcile` need | merge-base | Unrelated → class `unrelated_histories` (:496-503). Traced in the last section. |
| internal/cli/sync.go:205-264, 280-306 | `lit sync pull` leaf, `syncFailureFromPull` | `SyncPull` | As `SyncPull` | merge-base | `SyncPullUnrelated` → `SyncFailureError` (:294-297) → ExitConflict |
| internal/cli/sync_reconcile_cmd.go:68-91, 172-235, 242-267, 442-488 | `lit sync reconcile` / `take` / `combine`, `reportReconcileResult` | `SyncReconcile` / `SyncResolveUnrelated` / `SyncReconcileCombine` | As those functions | merge-base / root | Traced in the last section |

**Tests that cover these sites:**
- Freshness: `internal/store/sync_test.go:621,664`.
- Receive fast-forward: `sync_test.go:1394`.
- Reconcile and replay: `internal/store/sync_reconcile_test.go`, `sync_reconcile_scale_test.go`, `sync_reconcile_schema_skew_test.go`.
- Unrelated detect, take and combine: `internal/store/sync_unrelated_test.go:26-986` and `sync_unrelated_purge_test.go`.
- Schema guard: `sync_schema_guard_test.go`.
- Pushed head: `pushed_head_test.go`. Fetched head: `fetched_head_test.go`.
- Checkpoints: `checkpoint_test.go`. Dump and promote: `rawdump_test.go`, `promote_test.go`. Push from clone: `sync_push_from_clone_test.go`.
- CLI surfaces: `internal/cli/sync_unrelated_test.go`, `doctor_test.go`, `sync_failure_test.go`, `sync_reconcile_inline_test.go`.
- No test exercises a truncated or shallow history.

### Git-level reads (the git-backed remote's own git objects, not the Dolt commit graph)

| file:line | function | reads | needs | depth | if absent |
|---|---|---|---|---|---|
| internal/workspace/workspace.go:247-249; internal/cli/sync_receive_ask.go:105-130 | `RemoteDoltRefs`, `askRemote`/`advertise` | `git ls-remote <remote> refs/dolt/*` | The remote's current git ref ids, to skip a fetch when they are unchanged | tip | Any change (a re-root included) → mismatch → the full fetch runs |
| internal/store/remotecache.go:436-487 | `SyncRemoteMirrorHolds` / `gitDirHoldsCommits` | `git cat-file --batch-check` on the local mirror | The advertised git commits exist in the mirror (the push-proof) | tip | false → the record is not written; the next receive fetches |
| internal/store/fetched_head.go:293-398; internal/store/remotecache_refs.go:130-220 | `planRemoteCacheLanding`, `sweepMirror`, `listMirrorRefs` | `git for-each-ref refs/dolt/`, `git fetch` between mirrors, `git update-ref --stdin`, `git gc --prune=now` | `refs/dolt/last-known/*` is used as the negotiation base. Any value works (:284-292). | tip | The next fetch transfers more; nothing breaks |

---

## Persisted commit hashes (stored outside the commit graph and resolved later)

| What / where | Written by | Resolved by | If the hash stops existing | If it is replaced by a different hash with the same contents |
|---|---|---|---|---|
| Remote-tracking ref `remotes/<remote>/<branch>` (a Dolt ref) | `DOLT_FETCH` (force), `DOLT_PUSH`, `LandFetchedHead` (fetched_head.go:204), `RecordPushedHead` (pushed_head.go:206) | `SyncFreshness` (sync.go:911-939), `mergeBase` (sync_reconcile.go:361), `commitHashOfRef`, the schema guard, `DOLT_MERGE --ff-only`, `takeRemoteHead` | A ref cannot point at a missing commit. A ghost tip makes `LandFetchedHead` refuse (:195). | The next fetch moves it to the new hash with no ancestry check. Freshness then reads Diverged with whole-history counts, the reconcile reads Unrelated, push fails with `no common ancestor`, and `RecordPushedHead` errors. |
| Checkpoint branches `pre-migrate-<unixns>` (migration_runner.go:23) and `pre-drift-repair-<unixns>` (:30) | `CreateCheckpoint` (checkpoint.go:28) | `ResetToCheckpoint` within the same run (migration_runner.go:777, 1276); `ListCheckpoints`/`PruneCheckpoints` (dolt_branches name+hash; :696, :1287). `storage.Checkpoint.Anchor` is filled in (checkpoint.go:35,73), but no production code reads it. | The branch pins its commit. A deleted branch fails the failure-path reset (`restore from dbsnapshot`). | These branches are not re-pointed. They keep the pre-bound graph reachable until a later migration prunes them to 5. A reset to one *after* a bound would put `master` back on the old history, but the reset only happens inside the run that created the branch. |
| Reconcile scratch branches `links-reconcile-scratch-<pid>-<ns>-{spine,read}` (sync_reconcile.go:66,109-112) | `runOnReconcileScratch` (:866-871) | The same run only. Leftovers are deleted by `sweepStaleReconcileScratch` at the next reconcile (:1070-1095). | n/a | A leftover pins old commits until the next reconcile |
| `<StorageDir>/received-refs.last` (received_refs.go:27-33) | `recordReceived` (sync_receive_ask.go:222-234), `recordPushedAdvertisement` (:198-206), `RecordPushedHead` (pushed_head.go:164-168) | `remoteAdvertisement.unmoved` byte-compare (sync_receive_ask.go:80-83; sync_receive.go:266) | It holds **git** ref ids from `ls-remote`, not Dolt hashes. Forgotten on every directory rotation (workspace_lock.go:121-134) and on bootstrap install (store.go:3018-3021). | A mismatch, so the receive fetches (the safe direction). Not written while a divergence (or unrelated state) is unconverged (`settledCleanly`, sync_receive.go:447-458), so every interval refetches. |
| Git mirror `refs/dolt/last-known/<remote>/<ref>` in `<dolt>/.dolt/<remote-cache>/<key>/repo.git` (remotecache_refs.go:29-31) | Dolt blobstore close (fork patch 7); `sweepMirror`; `planRemoteCacheLanding` | Dolt's next fetch negotiation | Fetch costs more | Fetch costs more |
| Take approval token (sync_unrelated_take.go:38-41) = sha256(`take:`+side, localHead, remoteHead)[:6] | Printed in the refusal (sync_take_approval.go:46-49). The owner passes it back with `--owner-approved`. lit stores nothing. | Recomputed from the live heads under the lock (:148-149) | `Stale` refusal; fresh approval needed | `Stale` refusal; fresh approval needed (any head change voids it) |
| `RawDump.DoltHead` (rawdump.go:31) → `Candidate.expectedHead` (candidate.go:40,117) | `DumpRaw` (rawdump.go:100); `lit lifeboat dump` JSON (lifeboat.go:163) | `verifyHeadUnchanged` string equality at promote (promote.go:148-173). Only the in-process dump from `lit lifeboat recover` (lifeboat.go:91-127). No lit command reads a dumped JSON's `dolt_head` back. | `ErrWorkspaceAdvanced` → promote refused (safe; re-run) | Same refusal |
| Filesystem snapshots under `<StorageDir>/snapshots` (dbsnapshot): labels `pre-migrate-<ns>` (migrate_snapshot.go:29; migration_runner.go:321), `pre-downgrade` (downgrade.go:274), `pre-reconcile-<ns>` (sync_reconcile.go:118,452; sync_unrelated_take.go:207), and user snapshots (cli/snapshots.go:125) | `dbsnapshot.Take` through `snapshotGuard.ensure` (migrate_snapshot.go:123-133) | `lit snapshots restore` swaps in the whole directory (cli/snapshots.go:249). `searchRollbackSnapshot` opens them read-only and reads only the goose version (migrate_snapshot.go:196-258). | n/a. Each is a full copy of the graph, branches and tracking ref as of the take. | Restoring a snapshot taken before a re-root gives a store on the old root. The next receive classifies Unrelated. Retention: 10 migration, 10 reconcile, `snapshot.retention_budget` for user snapshots. |
| Durable sync-trace metadata `head` (cli/sync.go:470-472) | `performSyncPush` | Never resolved (record only) | n/a | n/a |

Checked and found to hold no Dolt hash:
- `pushOutcomeRecord` (sync_push_outcome.go:60-71).
- Owner-notify fingerprints (owner_notify.go:84-89).
- The adopt-pending marker (adopt.go:63-68: remote, branch, time).
- `storage.SyncState.ContentHash` (cli/backup.go:208-211). It is a sha of a JSON sync file.

---

## Unrelated-histories path today (local and remote share no ancestor)

### A. Push

**`lit sync push`**

1. Leaf → `performSyncPush(…, SyncCompactAndPush)` (internal/cli/sync.go:391, 516).
2. `SyncCompactAndPush` (store/sync.go:671) runs `DOLT_GC` (:683, via `compactWithinLock` :526).
3. `pushWithinLock` (:688 → :736):
   - The schema guard (:750) reads the tracking head with `AS OF`. Ancestry is not checked; it refuses only if the remote's schema is ahead of this binary.
   - It reads HEAD (:770).
   - `DOLT_PUSH` (:774-782): Dolt's `CanFastForward` → `ErrNoCommonAncestor` → raw error (`dolt: env/actions/remotes.go:58-62`, `dolt_push.go:125-133`).
4. `pushErr = "push remote \"<r>\": no common ancestor"` (sync.go:785). No `--force` is passed unless the user gives it.
5. `performSyncPush`:
   - Records the automation trace and the durable sync trace with status error (cli/sync.go:584-621).
   - The deferred `completePushAttempt` (sync_push_outcome.go:148-152) writes the push-outcome marker with `decision=error` and notifies the owner with kind push-failed (owner_notify.go:245).
6. `asSyncFailure` (cli/sync.go:409-411) converts only schema-ahead errors, so the raw error is returned and the command exits **ExitGeneric (1)**.
7. User-visible result:
   - A raw error with no class, no inventory and no remedy.
   - Later commands show the banner `sync: automatic push … is FAILING …: push remote "origin": no common ancestor — … run 'lit sync push'` (sync_staleness.go:146-158, 232-243), which points at the command that cannot succeed.
   - Push itself never classifies the state as unrelated. No owner approval is involved.

**On-change mirror**

1. `mirrorCycle` (cli/sync_bg.go:577) → `mirrorOnce` (:719) → `performSyncPush(…, SyncPushFromClone)` (:727).
2. `SyncPushFromClone` (store/sync.go:811):
   - The push fails as above.
   - It runs `DOLT_FETCH` on the clone (:827-830, force).
   - `SyncFreshness` reads `Ahead>0`, so the original `pushErr` is returned (:838-839).
3. `mirrorOnce` prints a block only for schema-ahead (:748-750). It returns `pushedHead{}` (:750-752), so `RecordPushedHead` is not reached.
4. The push-outcome marker error and the owner notification are written as above.
5. Every later mutation spawns another mirror that fails the same way. Nothing escalates this to the unrelated class.

### B. Background receive

1. `receiveOnce` (cli/sync_receive.go:238): `askRemote`. The advertisement differs from `received-refs.last`, which an unconverged receive never writes, so it continues.
2. `receiveAndRecord` (:306) → clone → `fetchIntoClone` (sync_receive_clone.go:69-84, `DOLT_FETCH` force).
3. `performSyncReceive` (:555) → `landFetch` → `LandFetchedHead` (store/fetched_head.go:99): `PullChunks` and `landTrackingRef`. `SetHead` onto the unrelated head gives `FetchedHeadMoved` (:204-207).
4. The live store is opened. `SyncSettleReceived` (:584 → store/sync.go:444 → `settleReceivedWithinLock` :470):
   - `SyncFreshness` gives ahead = all local commits and behind = all remote commits, so the state is Diverged.
   - Result: `SyncReceiveDiverged` (:485-486). No fast-forward.
5. `performInlineReconcile` (:618-620 → :649) → `reconcileOnce` → `SyncReconcile` (store/sync_reconcile.go:266):
   - `captureReconcilePlan`: `mergeBase` finds no base, so `shared=false`.
   - `unrelatedInventory` reads both heads `AS OF`.
   - **detectOnly** → `SyncReconcileUnrelated`. Nothing is written: no snapshot, no scratch branch, no reset (:392-413).
6. Traces: the automation trace and the durable sync trace record the decision `unrelated_histories` (sync_receive.go:676-702).
7. `recordReceived` (sync_receive_ask.go:222-234):
   - The fetch-success marker is written.
   - `settledCleanly()` is false (sync_receive.go:447-458), so `received-refs.last` is **not** written and every interval refetches and re-detects.
8. `surfaceReceiveOutcome` (:359):
   - `inlineSyncFailure` gives class `unrelated_histories` with the inventory (:496-503).
   - The block goes to the worker's stderr and to the receive-block pending file.
   - The owner is notified (`ownerNotifyEventForFailure`, owner_notify.go:96-106), de-duplicated per remote/branch episode.
9. The next command prints the block (`deliverPendingReceiveBlock`, :412):
   - Its escalation line is fixed: `ESCALATION — BLOCKED: unrelated histories never merge automatically …` (sync_failure.go:382-384).
   - Its steps list `combine` first, then `take` (:327-340).
10. It needs a human or agent command. `combine` needs no approval; `take` needs the owner's approval.

### C. `lit sync pull`

1. Leaf (cli/sync.go:205) → `SyncPull` (:226 → store/sync.go:247), all under one commit lock.
2. `SyncReceive` (`DOLT_FETCH` force, then settle) gives Diverged. `SyncReconcile` (detectOnly) gives Unrelated, and nothing is written.
3. → `SyncPullUnrelated`, with the counts, the fork timestamp and the inventory (store/sync.go:292-300).
4. CLI: `markFetchSuccess` (:241) → `syncFailureFromPull` (:280, :294-297) gives class `unrelated_histories`.
5. → `recordSyncHeldTrace`, owner notification (:254-257), and the `SyncFailureError` is returned. The block is printed and the command exits **ExitConflict (5)** (exit.go:64-71).
6. `endDivergenceEpisode` is not called. Resolving it needs an explicit command.

### D. `lit sync reconcile` (show / combine / take)

**show** (`syncReconcileShowLeaf`, cli/sync_reconcile_cmd.go:68)

1. `freshReconcileTarget` (:616-631) runs `SyncFetch` (force) and `markFetchSuccess`.
2. `SyncReconcile` (detectOnly) gives Unrelated, and nothing is written.
3. `reportReconcileResult` (:442, :471-488): `SyncFailureError` with class `unrelated_histories` and the inventory, a held trace, and an owner notification. Exits **ExitConflict**.

**combine** (`syncReconcileCombineLeaf`, :242-267). No approval needed.

1. `SyncReconcileCombine` (store/sync_reconcile.go:306) → `reconcile(…, unionCombine)`.
2. `!shared` → `unrelatedInventory` → `replayUnderGuard` (:421-423, :446-456):
   - It refuses a remote whose schema is ahead.
   - It sweeps leftover scratch branches.
   - It arms a `pre-reconcile` snapshot guard.
3. `combineFromAnchors` (:486) → `mergeAndReplay` with an **empty base** (:766):
   - A collision gives `SyncReconcileIDCollision`.
   - Prose conflicts on both sides give `SyncReconcileProsePending`. These are finalized by `lit sync reconcile resolve`, which also uses `unionCombine` (:284-291).
   - Otherwise `readFoldedChain(remoteHead..localHead)` returns **the entire local chain**. Each local commit is replayed onto the remote head (one `DOLT_RESET` + export per commit), then the marker `reconcile: combine unrelated histories (union of both backlogs)`, then the snapshot.
   - `DOLT_RESET --hard` moves the data branch (:988). Result: `SyncReconcileCombined`.
4. The next push fast-forwards.

**take** (`syncReconcileTakeLeaf`, :172-235)

1. `SyncResolveUnrelated` (sync_unrelated_take.go:100). Without a matching `--owner-approved`, it returns `OwnerApprovalRequiredError` (:148-160) and nothing mutates.
2. CLI: durable trace `owner_approval_required`, owner notification, then `ownerApprovalRefusalError` printed with the token. Exits **ExitConflict** (exit.go:83-86).
3. With the token:
   - `take remote` → `takeRemoteHead`: snapshot, then `DOLT_RESET --hard remotes/<r>/<b>`, giving `SyncReconcileTookRemote` (:238-253). If local holds purge records the remote lacks, it runs `takeRemoteCarryingPurges` in the replay envelope instead (:262-282).
   - `take local` → `takeLocalOntoRemoteHead`: the whole local chain is replayed onto the remote head, then the marker `reconcile: take local backlog …`, giving `SyncReconcileTookLocal` (:313-343).
4. Owner approval is required only for `take`. A human or agent has to run some resolve command in every case.

### Also: `lit doctor`

`lit doctor` (internal/cli/doctor.go:130-170, 117-119, 228-230) calls only `SyncFreshness`. It reports unrelated as ordinary `diverged` with whole-history counts, and exits ExitConflict as `diverged_unresolved` once the divergence is persistent. It never names the state as unrelated.

---

## Grep hits deliberately not given a row

- **`internal/docclaims/manifest_gen.go`** (all hits: `:43-45, 103-105, 686-687, 761-778, 1125-1187`). A generated manifest of quoted doc strings, not executed. Every real query it quotes has a row above.
- **Comments and prose only:**
  - `store/sync.go:214` (DOLT_PULL), `:497, 508, 529, 649, 692, 732, 895, 975, 1035`.
  - `store/adopt.go:212, 294, 355, 375`. `store/dolt_output.go:10`. `store/commit_lock.go:201, 216`. `store/compaction.go:62, 83`.
  - `store/store.go:450, 473, 1829, 3177`.
  - `store/sync_reconcile.go:82, 395, 496, 507, 547, 572, 1173, 1193-1198`.
  - `store/sync_schema_guard.go:72, 146, 153-155, 194`. `store/sync_unrelated.go:17, 69, 79-83`. `store/sync_unrelated_take.go:138`.
  - `store/rawdump.go:26, 120`. `store/remotecache.go:228`. `store/doc.go:60-61`. `store/fetched_head.go:186, 284`. `store/pushed_head.go:62, 186`.
  - `store/migration_runner.go:767`. `store/checkpoint.go:16`.
  - `storage/sync.go:74`. `storage/capabilities.go:65`. `storage/maintenance.go:20`.
  - `cli/sync_compact.go:20`. `cli/sync_receive.go:380, 388, 432, 544`. `cli/sync_receive_ask.go:17, 24, 196`. `cli/sync_receive_clone.go:65, 86`.
  - `cli/sync_staleness.go:17, 37-38, 107`. `cli/doctor.go:201, 220-229` (render strings for the doctor row). `cli/init_sync.go:70`.
  - `docnames/docnames.go:16`.
- **Case-insensitive `AS OF` false positives** ("as often", "alias of", and similar): `claims/establish.go:19`, `docclaims/docclaims.go:53`, `docnames/docnames.go:138`, `model/lifecycle/status_states.go:208`, `storage/capabilities.go:461`, `store/idbatch.go:55`.
- **`..` false positives** (path-traversal checks): `dbsnapshot/snapshot.go:433`, `docclaims/docclaims.go:303`, `release/installer.go:353, 434-437`, `tools/lawtokens-check/main.go:64`, `tools/mkmanifest/main.go:191, 251`.
- **Not history (configuration, branch names, working set):**
  - `store/sync.go:103` and `store/remotecache.go:233` (`dolt_remotes`). `store/sync.go:163, 177` (`DOLT_REMOTE add/remove`).
  - `store/store.go:3045, 3048, 3079` (`active_branch`, `dolt_branches`, `DOLT_BRANCH -m` master normalization: branch names only).
  - `store/sync.go:859, 929` and `store/sync_reconcile.go:1154` (`ACTIVE_BRANCH()`).
  - `store/sync.go:874` (`dolt_status`, included in the SyncStatus row).
- **Git code-repo refs, not Dolt:** `workspace/workspace.go:226` (`@{upstream}`), `:272-277` and `cli/sync_branch.go:24, 75` (`refs/remotes/<remote>/HEAD`, the code repo's default branch).
- **`snapshot` hits in `internal/backup/*`, `cli/backup.go`, `cli/register.go:739-763`, `cli/cli.go:981, 1083-1085`, `cli/epic_context.go:212`, `cli/error_output.go`, `cli/downgrade.go`, `cli/upgrade.go`, `cli/open_migration.go`, `cli/stores.go`, `cli/lifeboat.go:121`, `config/config.go`, `dbsnapshot/*`.** These are JSON export backups, or whole-directory filesystem snapshots. They are covered in the persisted-hashes table; none of them reads the commit graph.
- **`internal/storage/memory`:** the in-memory engine, not linked into `cmd/lit` (only `storage/ordering.go` imports it outside tests). It has no history.
- **No hits in non-test code:** `HASHOF`, `dolt_history_`, `dolt_diff`, `dolt_commits`, `dolt_commit_ancestors`, `DOLT_REBASE`, `DOLT_REVERT`, `DOLT_CHERRY_PICK`, `HEAD~`, `pushed_head` / `fetched_head` as SQL names, and any `doltdb`/`datas` commit-walking call outside `fetched_head.go` and `pushed_head.go`. `cmd/` and `tools/` have no Dolt history reads.
