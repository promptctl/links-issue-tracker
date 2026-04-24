# REFACTOR_PLAN: cli.go / store.go decomposition + targeted absorbed-variance

**Tracking:** epic `links-refactor-o35`
**Companion:** `docs/COMPLEXITY_AUDIT-2026-04-19.md`

This document is the **contract** that makes each extraction issue reviewable as a single commit: it names the exact symbols that move, the exact files that receive them, and the interfaces at seams. A reviewer of any single extraction PR should be able to answer "is this correct?" by comparing the PR against this document, not by re-deriving the split.

---

## 1. Principles

1. **Mechanical first, absorbed-variance second.** Most of the debt is dimensional (files are too large), not structural. Move code to new files without changing behavior. Only three pieces require absorbed-variance: `commit_lock.go` (closes race window), `register.go` (data-driven command registry), `runUpdate` (unified UpdateInput). Do those *after* the surrounding code has been extracted, so the variance-absorption diff is small and reviewable.
2. **One extraction per PR.** Each of the 14 extractions is its own commit. Tests must pass unchanged between each.
3. **No signature changes during mechanical extractions.** A function that moves to a new file keeps its name, parameter list, and return type. Renames are deferred to a later pass.
4. **Extract imports explicitly.** Each new file declares its own `import` block; do not rely on transitive visibility.
5. **Package `cli` stays one package** (no sub-packages). Same for `store`. The goal is file granularity, not package granularity — existing tests reach into package-private state and should not need to change.
6. **`internal/` stays flat.** No new sub-packages. If a seam requires a new package (e.g., moving `issue_ids.go` to `internal/issueid/`), that's a separate issue, not part of the decomposition.

---

## 2. Target file structure

### internal/cli/ (after refactor)

```
cli.go                   ~1,600 lines   root command + runWith helpers + generic utilities
register.go              ~120   lines   CommandSpec registry; loop-driven command wiring
sync.go                  ~650   lines   runSync + 5 sub-handlers + resolve helpers + payload printers
bulk.go                  ~120   lines   runBulk + 4 sub-handlers
backup.go                ~140   lines   runBackup + runRecover + restore helpers
doctor.go                ~75    lines   runDoctor + fix registry + allDoctorFixNames
dependency.go            ~95    lines   runDep + helpers
issue_relations.go       ~120   lines   runLabel + runParent + runChildren
output.go                ~200   lines   printIssue* + format* + resolveColumns
[existing siblings: init.go, migrate.go, hooks.go, completion.go, ready_state.go,
  error_output.go, automation_trace.go, exit.go, agents_internal.go,
  managed_sections.go, quickstart_eject.go, quickstart_refresh.go, completion.go]
```

### internal/store/ (after refactor)

```
store.go                 ~800  lines   Store struct, Open/Close/reconnect, small shared helpers
schema.go                ~406  lines   migrate + ensureUnifiedStatusSchema + topic/rank/constraint init
labels.go                ~100  lines   AddLabel/RemoveLabel/ReplaceLabels + normalization
ranking.go               ~425  lines   Rank* + smoothRanksIfNeeded + FixRankInversions
relations.go             ~799  lines   AddRelation/RemoveRelation/ListRelationsForIssue/SetParent/ClearParent/ListChildren
import_export.go         ~875  lines   Export/Import* + ReplaceFromExport + Doctor/Fsck
commit_lock.go           ~250  lines   commitWorkingSet + withCommitLock + acquireCommitLock + liveness + withMutation (new)
[existing siblings: sync.go, issue_ids.go (eventual move), store_test.go, sync_test.go, etc.]
```

---

## 3. cli.go decompositions — exact symbols per file

### 3.1 `internal/cli/sync.go` (issue .3 — template extraction, do first)

**Move:**
- `runSync` (cli.go:1335–1591)
- 5 sub-handlers inline in runSync: status, remote, fetch, pull, push
- `resolveSyncRemote` (cli.go ~L1603)
- `resolveSyncBranch` (cli.go ~L1647)
- `readSyncRemoteState`
- `syncDoltRemotesFromGit`
- `buildRemoteSyncChanges`
- `printSyncPullPayload`, `printSyncPushPayload`
- Any helper in cli.go:1593–2012 whose only caller is one of the above
- `missingRemoteBranchPattern` (cli.go:36) and `debugSyncBranchEnvVar` (cli.go:38) — sync-only

**Keep in cli.go:**
- The `addGroupedPassthrough(root, "data", "sync", ...)` entry in `newRootCommand` — it just calls `runSync`; the registration itself is moved to `register.go` later.

**Test reference:** `internal/cli/sync_test.go` exists; it must pass unchanged.

### 3.2 `internal/cli/bulk.go` (issue .4)

**Move:**
- `runBulk` (cli.go:2213–2330)
- `validateBulkCommandPath`
- 4 inline sub-handlers (label, close, archive, import)

### 3.3 `internal/cli/backup.go` (issue .5)

**Move:**
- `runBackup` (cli.go:2090–2171)
- `runRecover` (cli.go:2174–2211)
- `validateBackupCommandPath`
- `restoreFromExportPath` (cli.go:2688–2737)
- `hashExport` (cli.go:2739–2747)

### 3.4 `internal/cli/doctor.go` (issue .6)

**Move:**
- `runDoctor` (cli.go:2047–2088)
- `doctorFixes` map (cli.go:2025–2045)
- `allDoctorFixNames` (cli.go:2014–2021)
- `resolveDoctorAccessMode` (helper used by the registration)

### 3.5 `internal/cli/dependency.go` (issue .7)

**Move:**
- `runDep` (cli.go:1060–1147)
- `validateDepCommandPath`
- `depStoreEndpoints`, `depRelationForCLI`, `depRelationLine`

### 3.6 `internal/cli/issue_relations.go` (issue .8)

**Move:**
- `runLabel` (cli.go:1179–1224), `validateLabelCommandPath`
- `runParent` (cli.go:1226–1275), `validateParentCommandPath`
- `runChildren` (cli.go:1277–1294)

These three commands are grouped organizationally, not because they share code.

### 3.7 `internal/cli/output.go` (issue .9)

**Move:**
- `printIssueSummary`, `printIssueTable`, `printIssueLines`, `printIssueDetail`, `printIssueGroup`, `printLabels`
- `formatIssueColumns`, `formatLabels`, `formatIssueState`, `formatOptionalTime`
- `emptyDash`, `resolveColumns`

**Keep in cli.go:**
- `printValue` — the single-enforcer dispatch. It calls the text-printer functions (now in output.go). This preserves the `[LAW:single-enforcer]` annotation at the dispatch site.

**Do not duplicate with `ready_state.go`:** verify during extraction that `printReadyOutput` has no overlapping helpers.

### 3.8 `internal/cli/register.go` (issue .10 — absorbed-variance)

See §5.2 below.

---

## 4. store.go decompositions — exact symbols per file

### 4.1 `internal/store/schema.go` (issue .11 — do first among store extractions)

**Move:**
- `Store.migrate` (store.go:306–432)
- `Store.ensureUnifiedStatusSchema` (L439)
- `Store.ensureIssueTopics` (L493)
- `Store.ensureIssueRanks` (L503)
- `Store.ensureStatusConstraint` (L536)
- `Store.listIssueStatusCheckConstraints` (L555)
- `execReconciliationUpdate` (if present in 26–432 range)
- `issueCheckConstraint` type (if only used here)

**Keep in store.go:**
- `Store` struct, `Open`, `Close`, `reconnect`, `processCommitMutex`, `doltDriverName`/`doltDatabaseName` consts
- `ErrTransientManifestReadOnly`, `commitLockPIDRunning` vars

### 4.2 `internal/store/labels.go` (issue .12)

**Move:**
- `Store.AddLabel`, `Store.RemoveLabel`, `Store.ReplaceLabels`, `Store.ListLabels`
- `replaceLabelsTx` (internal helper)
- `normalizeLabel`, `canonicalizeLabels`

### 4.3 `internal/store/ranking.go` (issue .13)

**Move:**
- `Store.RankToTop` (L1082), `Store.RankToBottom` (L1121), `Store.RankAbove` (L1160), `Store.RankBelow` (L1209)
- `Store.smoothRanksIfNeeded` (L1362), the `*Tx` variant (if present in this range)
- `Store.FixRankInversions` (L1399)
- `rankInversion` struct
- `rankInversionsRelationClause` SQL constant

**Note:** Do NOT try to collapse `smoothRanksIfNeeded` vs `smoothRanksIfNeededTx` in this PR (audit finding S9). That collapse happens in issue .16 once `withMutation` owns tx lifecycle.

### 4.4 `internal/store/relations.go` (issue .14)

**Move:**
- `Store.AddRelation` (L1561), `Store.RemoveRelation` (L1608)
- `Store.ListRelationsForIssue`, `Store.SetParent`, `Store.ClearParent`, `Store.ListChildren`
- Relation-specific scan helpers (if used only here)

### 4.5 `internal/store/import_export.go` (issue .15)

**Move:**
- `Store.Export` (L1641)
- `Store.ImportIssue`, `Store.ImportComment`, `Store.ImportRelation`, `Store.ImportLabel`
- `Store.ReplaceFromExport`
- `Store.Doctor` (L1665), `Store.Fsck` (L1716)
- `HealthReport` struct (if defined in this range)

### 4.6 `internal/store/commit_lock.go` (issue .16 — absorbed-variance, do LAST)

See §5.1.

---

## 5. Absorbed-variance interfaces

These three pieces are where the variance-absorption skill applies. They are not mechanical moves; they change the *shape* of the code so that variance lives in data instead of control flow.

### 5.1 `withMutation` combinator (issue .16)

**Problem:** 20+ mutation sites each hand-weave the same sequence. Two of them wrap the sequence in `retryTransientManifestReadOnly` (the retry is control-flow variability). And the commit lock is released between the mutation commit and the retry loop — audit finding S2 race window.

**Current shape at each site (repeated ~20 times):**

```go
func (s *Store) Whatever(ctx context.Context, ...) (..., error) {
    ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
    if err != nil { return ..., err }
    defer releaseCommitLock()
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil { return ..., fmt.Errorf("begin ... tx: %w", err) }
    defer tx.Rollback()
    // ... do work with tx ...
    if err := tx.Commit(); err != nil { return ..., fmt.Errorf("commit ... tx: %w", err) }
    if err := s.commitWorkingSet(ctx, "message"); err != nil { return ..., err }
    return result, nil
}
```

**Target shape (variance absorbed):**

```go
// withMutation is the single enforcer for write-path coordination.
// It acquires the commit lock, opens a tx, runs fn, commits the tx,
// and commits the working set — ALL under the same held lock.
// On transient manifest read-only during working-set commit, it retries
// commitWorkingSet only (with the lock still held) per the documented backoff.
// On transient manifest read-only inside fn, it retries fn from the beginning
// (reopening a new tx) — the outer lock is held throughout.
//
// Contract:
//   - Lock acquired exactly once per call
//   - Lock held across all retries (closes audit finding S2 race window)
//   - fn must be idempotent across retries (document at callsites)
//   - commitMessage is passed through to commitWorkingSet
func (s *Store) withMutation(
    ctx context.Context,
    commitMessage string,
    fn func(ctx context.Context, tx *sql.Tx) error,
) error
```

**Callsite shape after migration:**

```go
func (s *Store) Whatever(ctx context.Context, ...) (..., error) {
    var result ...
    err := s.withMutation(ctx, "do whatever", func(ctx context.Context, tx *sql.Tx) error {
        // ... do work with tx, assign result ...
        return nil
    })
    return result, err
}
```

**What this absorbs:**

| Variance | Before | After |
|----------|--------|-------|
| Lock acquire/release | per-site boilerplate (20+) | inside withMutation |
| Tx open/commit/rollback | per-site boilerplate | inside withMutation |
| commitWorkingSet | per-site explicit call | inside withMutation |
| retryTransientManifestReadOnly | conditional wrapper on 3 sites | unconditional, inside withMutation |
| Race window during retry | lock released before retry | lock held throughout |
| smoothRanksIfNeeded vs smoothRanksIfNeededTx | two variants chosen by caller | one variant — caller always holds tx via combinator |

**Acceptance tests (new, in `commit_lock_test.go`):**

- Two concurrent `withMutation` calls serialize through the lock.
- A `fn` that succeeds but whose `commitWorkingSet` triggers a simulated transient manifest read-only retries with the lock held (observable via a test-only lock-ownership probe).
- Signature: `commitWorkingSet` is not callable from any package outside `store` (ensure via naming: lowercase `commitWorkingSet` already satisfies this).

**Migration step (part of issue .16):**

1. Introduce `withMutation` in `commit_lock.go`. Leave existing sites unchanged.
2. Migrate sites in bulk in the same PR (greppable: `acquireCommitLock` → zero remaining matches outside `commit_lock.go`).
3. Delete `smoothRanksIfNeeded`'s non-Tx variant (ranking.go); update all callers to pass through `withMutation`.
4. Delete `retryTransientManifestReadOnly`'s explicit wrapping at `CreateIssue`, `AddComment`, `TransitionIssue` (now unnecessary — it happens inside withMutation).

### 5.2 CommandSpec registry (issue .10)

**Problem:** 28 hand-written `addGroupedPassthrough(...)` calls differ only in `{name, summary, groupID, accessMode, runFn}`. This is variance encoded in call sequence.

**Target:**

```go
// CommandSpec describes one registered subcommand. It is pure data;
// newRootCommand turns a []CommandSpec into a cobra tree.
type CommandSpec struct {
    Name       string
    Summary    string
    GroupID    string
    AccessMode appAccessMode    // unused for commands like quickstart; zero value = "no app"
    NeedsApp   bool             // if false, Handler is called without app.App
    Validator  func(args []string) error // optional; e.g., validateDepCommandPath
    Handler    CommandHandler
}

type CommandHandler func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error
```

**Registry:**

```go
func allCommands(ctx context.Context, stdout, stderr io.Writer) []CommandSpec {
    return []CommandSpec{
        {Name: "init", GroupID: "bootstrap", Summary: "Initialize links", NeedsApp: false, Handler: wrapNoApp(runInit)},
        {Name: "quickstart", GroupID: "guidance", Summary: "Agent quickstart workflow", NeedsApp: false, Handler: wrapNoApp(runQuickstart)},
        {Name: "new",  GroupID: "operations", AccessMode: appAccessWrite, NeedsApp: true, Summary: "Create an issue", Handler: runNew},
        {Name: "ls",   GroupID: "operations", AccessMode: appAccessRead,  NeedsApp: true, Summary: "List issues (rank by default)", Handler: runList},
        {Name: "dep",  GroupID: "structure",  AccessMode: appAccessWrite, NeedsApp: true, Summary: "Manage dependency edges",
            Validator: validateDepCommandPath,
            // dep ls is read-only; dynamic access mode:
            Handler: runDepDispatch},
        // ... 28 total
    }
}
```

**newRootCommand becomes:**

```go
func newRootCommand(ctx context.Context, stdout io.Writer, stderr io.Writer) *cobra.Command {
    root := &cobra.Command{Use: "lit", Long: /* unchanged */}
    registerCommandGroups(root) // static groups list — also data-driven
    for _, spec := range allCommands(ctx, stdout, stderr) {
        root.AddCommand(buildCobraCommand(ctx, stdout, stderr, spec))
    }
    return root
}
```

**Dynamic access modes (dep ls / backup list / doctor non-fix) — the real variance:**

The current code has conditional `accessMode` selection inside a few handlers:

```go
accessMode := appAccessWrite
if len(args) > 0 && args[0] == "ls" { accessMode = appAccessRead }
```

Absorbed-variance answer: add `AccessModeResolver func(args []string) appAccessMode` to CommandSpec. If nil, use `AccessMode`. The few commands that need it (dep, backup, doctor) set the resolver; the rest don't. The *same* code path runs every time — the resolver is just data (a function or nil) that lives on the spec.

```go
type CommandSpec struct {
    // ...
    AccessMode         appAccessMode
    AccessModeResolver func(args []string) appAccessMode // optional; wins over AccessMode if set
}
```

### 5.3 `UpdateInput` unification (issue .17)

**Problem:** `runUpdate` (cli.go:827–937) has two separate execution branches: one for field updates, one for status transitions. Flags decide which branch runs.

**Target:**

```go
type UpdateInput struct {
    Fields      FieldPatch        // zero value = no field changes
    Transitions []TransitionAction // nil or empty = no transitions
}

type FieldPatch struct {
    Title       *string  // nil = unchanged
    Description *string
    Priority    *int
    Assignee    *string
    Labels      *[]string
}

type TransitionAction struct {
    Action string // "close", "start", "done", etc.
    Reason string
}
```

**runUpdate shape:**

```go
func runUpdate(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
    id, input, err := parseUpdateArgs(args)
    if err != nil { return err }

    // One execution path. Empty Fields = UpdateIssue is a no-op commit.
    // Empty Transitions = no transition loop iterations.
    issue, err := ap.Store.Apply(ctx, id, input)
    if err != nil { return err }

    return printValue(stdout, issue, printIssueSummary)
}
```

Or, if `Apply` on the store side is too invasive:

```go
issue, err := ap.Store.UpdateIssue(ctx, id, input.Fields)  // no-op if patch is empty
if err != nil { return err }
for _, action := range input.Transitions {
    issue, _, err = ap.Store.TransitionIssue(ctx, id, action)
    if err != nil { return err }
}
```

The key: **no `if mutatesFields`/`if mutatesStatus`**. The loop runs `len(Transitions)` times (possibly zero). `UpdateIssue` runs once regardless; inside it, a zero-value patch is a no-op.

**Input validation** ("must provide something"): enforce in `parseUpdateArgs` — returns an error if both `Fields` and `Transitions` are empty. That's a parse-time check, not an execution branch.

---

## 6. Sequencing

Dependency graph in `lit` (already wired via `lit dep add`):

```
.1 design doc (blocks all extractions)
├── .3 sync.go (cli template — blocks other cli extractions)
│   ├── .4 bulk.go
│   ├── .5 backup.go
│   ├── .6 doctor.go
│   ├── .7 dependency.go
│   ├── .8 issue_relations.go
│   └── .9 output.go
├── .10 register.go (absorbed-variance)
├── .11 schema.go (store, any order)
├── .12 labels.go (store, any order)
├── .13 ranking.go ──┐
├── .14 relations.go ─┼── .16 commit_lock.go (absorbed-variance)
├── .15 import_export.go ┘
└── .17 runUpdate unification (absorbed-variance; depends on .9 output.go to reduce merge conflicts)
```

**Recommended execution order:**

1. `.1` design doc (this file)
2. `.2` preliminary cleanups (independent; can land any time)
3. `.3` sync.go (cli template)
4. `.4`–`.9` cli extractions (can parallelize once sync.go lands)
5. `.11`–`.15` store extractions (can parallelize; order among them doesn't matter)
6. `.16` commit_lock.go + `withMutation` (must come after .13/.14/.15)
7. `.17` runUpdate unification
8. `.10` register.go (can land any time after .1; deferred to avoid merge conflicts with other cli extractions)
9. `.18` migration sunset (independent; blocked on team cutover date)

---

## 7. Test strategy

**For mechanical extractions (.3–.9, .11–.15):**

- Before extraction: `go build ./... && go test ./... -count=1` must pass from a clean tree.
- After extraction: same command must pass with zero test changes.
- If a test file needs changes, the extraction is not purely mechanical — investigate before merging.
- If a symbol moves from one file to another in the same package, no import changes should be needed. If imports need to change, flag it — it means a previously-private helper leaked across a boundary.

**For absorbed-variance (.10, .16, .17):**

- All existing tests must still pass (contract tests).
- Add new targeted tests:
  - `.16`: concurrent mutation + simulated transient error holds lock throughout.
  - `.10`: new command added via CommandSpec entry is reachable through cobra.
  - `.17`: `lit update X --title Y` (fields only), `lit update X --close` (transition only), `lit update X --title Y --close` (both) all produce the same behavior as before.

**Regression net:**

- `internal/cli/mutation_commands_test.go`
- `internal/cli/preflight_test.go`
- `internal/cli/ready_test.go`
- `internal/store/store_test.go`
- `internal/store/sync_test.go`
- `internal/store/commit_lock_test.go` (exists)

These cover the major code paths affected by the refactor.

---

## 8. Out of scope (explicitly)

- Changing the Dolt-specific string-matched transient error classification (audit finding S3). Deferred; would require a backend-shaped interface.
- Moving `internal/store/issue_ids.go` → `internal/issueid/` (tracked in prelim-cleanups issue .2).
- Migration sunset deletions (tracked in issue .18; awaits cutover date).
- Any changes to `artifacts/beads/` (vendored; not part of this codebase).
- Adding new commands or features.
- Renames beyond what's explicitly called out here (sortByReadiness rename is in prelim-cleanups).

---

## 9. Done criteria (epic-level)

- `internal/cli/cli.go` ≤ 1,700 lines
- `internal/store/store.go` ≤ 900 lines
- `go build ./... && go test ./...` passes with zero new test changes in mechanical PRs
- `commitWorkingSet` and `acquireCommitLock` are called only from `commit_lock.go`
- `newRootCommand` is ≤ 30 lines; adding a new command is a single `CommandSpec{}` entry
- `runUpdate` has no `if mutatesFields` / `if mutatesStatus` branches
- Audit findings S2, S4, C3, C5 are verifiably resolved (S3, S1 remain as deferred)
