# Complexity Audit: links-issue-tracker (`lit`) — 2026-04-19

**Scope:** 62 source files in `cmd/` + `internal/` (11,619 non-test LOC; 6,027 test LOC)
**Excluded:** `artifacts/beads/` (vendored reference copy of upstream beads)
**Mode:** God-module deep-dive (recommended) — `store.go`, `cli.go`, migration code; brief cross-cutting survey of remaining modules.
**Branch:** `template-eject`

---

## Executive Summary

Two files hold **53% of the non-test codebase**:

| File | Lines | % of non-test LOC | Concerns |
|------|-------|-------------------|----------|
| `internal/store/store.go` | 3,320 | 29% | 12 distinct |
| `internal/cli/cli.go` | 2,875 | 25% | 24 command implementations |
| Everything else | 5,424 | 47% | Mostly healthy |

**Headline findings:**

1. **Two god modules** (store.go, cli.go) are the dominant source of structural complexity. Each has clean architectural layering and respects project laws, but has ballooned by accumulation. Both decompose cleanly along existing natural seams.
2. **Migration infrastructure is well-engineered and ready for sunset** (~1,600 lines deletable across ~2 releases once Beads→links transition completes). Missing: explicit sunset date.
3. **Architectural principles are strongly upheld everywhere.** Zero `len(annotations) == 0` semantic-predicate violations anywhere in the codebase. Annotations are consistently treated as neutral facts; predicates live in consumers.
4. **Two binaries** (`cmd/lit` and `cmd/lnks`) are identical passthroughs to `cli.Run()` — in-flight rename; pick one and delete the other.
5. **No significant dead code** — no TODO/FIXME/HACK across the audit scope (except a shell-script fallback in a template).
6. **The highest-impact latent risk** is the commit-lock/transient-retry pattern in store.go: the lock is released between the mutation commit and `commitWorkingSet`'s retry loop, creating a race window during retries.

**Critical issues (HIGH severity):**
- store.go: lock released between mutation and commitWorkingSet retries → race window
- store.go: `retryTransientManifestReadOnly` is control-flow variability, not data-driven
- store.go: 20+ lock-acquisition boilerplate sites
- cli.go: `runSync()` is 258 lines packing 5 sub-commands
- cli.go: `newRootCommand()` is 218 lines of hand-written registrations for 28 commands
- cli.go: 111 flag declarations with no inheritance or sharing

**Quick wins:**
- Extract `sync.go`, `bulk.go`, `backup.go`, `doctor.go`, `dependency.go`, `output.go` from cli.go
- Batch-load related issues in `GetIssueDetail` (fix N+1)
- Extract `scanTime` helper in store.go (8 duplicates)
- Rename `sortByReadiness` → `sortByBlockingAnnotations` (semantic precision)
- Unexport `optionalIssuePtr` in merge.go
- Add `[SUNSET: YYYY-MM-DD]` comments to migration files

---

## 1. `internal/store/store.go` — 3,320 lines, 12 concerns

### Concerns inventory (line ranges approximate)

| # | Concern | Lines | Size |
|---|---------|-------|------|
| 1 | Database schema & initialization | 26–432 | ~406 |
| 2 | Issue lifecycle (CRUD, state machine) | 638–2234 | ~600 core + helpers |
| 3 | Issue ranking & ordering | 1082–1507 | ~425 |
| 4 | Relations (blocks / parent / related-to) | 1561–2360 | ~799 |
| 5 | Comments & labels | 1510–2065 | ~555 |
| 6 | Import / export | 1641–2516 | ~875 |
| 7 | Health & diagnostics (Doctor, Fsck) | 1665–1744 | ~80 |
| 8 | Sync operations | sync.go | 347 |
| 9 | Issue ID generation | issue_ids.go | 203 |
| 10 | Commit-lock & persistence infra | 3107–3320 | ~213 |
| 11 | Metadata & configuration | 596–2615 | scattered |
| 12 | SQL query helpers / filter builders | 2518–2919 | ~400 |

### Key findings

| # | Finding | Severity | Type | Location | Quick-win? |
|---|---------|----------|------|----------|------------|
| S1 | 12 concerns in one file — god module | **High** | god-module | whole file | No |
| S2 | Commit lock released between mutation tx and `commitWorkingSet` retry → race window during transient retries | **High** | locking-pattern / dataflow-violation | `commitWorkingSet` L3107–3119; all mutations L742, L1075 etc. | No |
| S3 | `retryTransientManifestReadOnly` is control-flow variability based on Dolt-specific string-matched errors (`"cannot update manifest"` + `"read only"`) | **High** | dataflow-violation / driver coupling | L640, L1512, L2069; classify L3301–3320 | No |
| S4 | 20+ sites of repeated `acquireCommitLock` / `defer releaseCommitLock` boilerplate | Medium | duplication | L611, L688, L1051, L1086, L1125, L1171, L1220, L1363, L1428, L1539, L1586, L1614, L1718, L1769, L1847, L1898, L1938, L1977, L2010, L2045, L2157, L2327, L2366, L2455 | Yes (wrapper) |
| S5 | `GetIssueDetail` N+1: per-relation `GetIssue` calls | Medium | coupling / perf | L920–953 | Yes (batch load) |
| S6 | `ListIssues` filter-build: 13 optional predicates as sequential appends — conditional SQL assembly rather than data-driven | Medium | parameter-threading | L751–884 | Partial |
| S7 | Defensive silent-skip null guards in `GetIssueDetail` — `if err == nil { detail.X = append(...) }` drops errors | Medium | null-guard | L921, L928, L935, L941, L951 | Yes (log or propagate) |
| S8 | Time-string parsing duplicated 8× across row scanners | Low | duplication | L2527, L2670, L2714, L2737, L2760, L2783, L2805, L2844 | Yes (extract `scanTime`) |
| S9 | `smoothRanksIfNeeded` vs `smoothRanksIfNeededTx` — two implementations of the same behavior, chosen by whether caller already holds a tx | Medium | incomplete-refactor / one-type-per-behavior | L1261–1380 | No |
| S10 | Rank-inversion convergence check uses snapshot equality (two identical iterations → abort) | Low | special-case | L1440–1497 | No |
| S11 | `setMeta`/`getMeta` accept both `*sql.Tx` and `*sql.DB` — caller chooses | Low | parameter-threading | L2540–2571 | Partial |
| S12 | Stale commit-lock liveness probes ESRCH/EPERM/ErrProcessDone | Low | defensive-code | L3197–3274 | No (OS-specific) |
| S13 | Legacy status normalization migration still runs every `Store.Open` | Low | legacy-code | L439–491 | Sunset (per line-26 comment) |
| S14 | `issue_ids.go` is physically in `internal/store/` but logically matches `internal/issueid/` (which also exists) | Low | package-placement | issue_ids.go | Yes (move) |

### Proposed decomposition (~6 modules)

| New module | Extracted from | Lines |
|------------|----------------|-------|
| `ranking.go` — RankToTop/Bottom/Above/Below + smoothing + inversion fix | 1082–1507 | ~425 |
| `relations.go` — Add/Remove/List relations, SetParent/ClearParent, ListChildren | 1561–2360 | ~799 |
| `import_export.go` — Export, Import*, ReplaceFromExport, Doctor, Fsck | 1641–2516 | ~875 |
| `labels.go` — AddLabel/RemoveLabel/ReplaceLabels + normalization | 1965–2065 | ~100 |
| `schema.go` — migrate, ensureUnifiedStatusSchema, topics/ranks/constraints | 26–432 | ~406 |
| `commit_lock.go` — commitWorkingSet, withCommitLock, acquireCommitLock, PID-liveness, transient-retry wrappers | 3107–3320 | ~213 |

---

## 2. `internal/cli/cli.go` — 2,875 lines, 24 commands

### Command inventory (top drivers)

| Command | Lines | Size | Notes |
|---------|-------|------|-------|
| `runSync` (5 sub-cmds) | 1335–1591 | **258** | status/remote/fetch/pull/push |
| `newRootCommand` | 93–309 | **218** | 28 `addGroupedPassthrough` calls |
| `runBulk` (4 sub-cmds) | 2213–2330 | **119** | label/close/archive/import |
| `runUpdate` | 827–937 | 112 | status transitions + field updates |
| `runList` | 632–741 | 111 | 13 flags |
| `runDep` (3 sub-cmds) | 1060–1147 | 89 | add/rm/ls |
| `runBackup` | 2090–2171 | 84 | create/list/restore |
| Total: 107 functions, 28 user-facing commands, 111 flag declarations |

By analogy with siblings (`init.go`, `migrate.go`, `hooks.go`, `completion.go`), **20 commands in cli.go should be in their own files.**

### Key findings

| # | Finding | Severity | Type | Location | Quick-win? |
|---|---------|----------|------|----------|------------|
| C1 | 24 command implementations in one file (vs 4 sibling files for 4 commands) | **High** | god-module | whole file | Yes (extract) |
| C2 | `runSync` is 5 sub-commands crammed into one 258-line function | **High** | god-function | L1335–1591 | Yes (extract) |
| C3 | `newRootCommand` hand-registers 28 commands with no abstraction | **High** | repetition | L93–309 | Yes (data-driven registry) |
| C4 | 111 flag declarations, no inheritance, `--json` redefined per command | **High** | flag-explosion | scattered | No (requires flag refactor) |
| C5 | `runUpdate` splits status mutations (transition loop) vs field mutations (UpdateIssue) into branches — dual execution path | Medium | dataflow-violation | L827–937, branches at L858, L902 | No |
| C6 | `visited` map pattern (pflag Visit walker) manually repeated per-command for "was this flag explicitly provided?" distinction | Medium | duplication | L657, L850, L905–927 | Yes (helper on cobraFlagSet) |
| C7 | Usage strings duplicated across commands | Low | duplication | L844, L847, L2764–2853 | Yes (centralize) |
| C8 | `cmd/lit` and `cmd/lnks` are byte-identical entrypoints | Low | alias / rename-in-flight | both main.go | Yes (pick one) |

### Architectural notes (positive)
- Output formatting correctly single-enforced through `printValue()` (L2428). 20+ annotations of `[LAW:single-enforcer]` applied correctly.
- `visited`-map usage is architecturally correct (distinguishes "unset" from "zero"); the only issue is the repetition.
- No `len(annotations) == 0` violations in the file.
- Coupling is appropriate for a CLI layer (uses `app.App` facade, doesn't reach into store internals).

### Proposed decomposition (~8 files)

| New file | Extracts | Lines freed |
|----------|----------|-------------|
| `sync.go` | `runSync` + 5 sub-cmds + resolver helpers | ~650 (incl. helpers currently at L1593–2012) |
| `bulk.go` | `runBulk` + 4 sub-cmds | ~120 |
| `backup.go` | `runBackup` + `runRecover` + `restoreFromExportPath` | ~140 |
| `dependency.go` | `runDep` + `depRelation*` helpers | ~95 |
| `issue_relations.go` | `runLabel` + `runParent` + `runChildren` | ~120 |
| `doctor.go` | `runDoctor` + `doctorFixes` registry | ~75 |
| `output.go` | `printIssue*` + `formatIssue*` + `resolveColumns` | ~200 |
| `register.go` | Data-driven command registry replacing `newRootCommand` | ~150 (~100 new, saves ~150) |

Net reduction: ~45% of cli.go (~2,875 → ~1,600).

---

## 3. Migration infrastructure — ready for sunset

### Components and status

| Name | File:Lines | Status | Removable? |
|------|-----------|--------|-----------|
| Beads residue scan | `migrate.go:366–447` | ACTIVE | No — gate is mandatory |
| Beads hook cleanup | `migrate.go:388–395, 454–476` | ACTIVE | No (migration op) |
| Beads config cleanup | `migrate.go:199–207, 425–442, 519–546` | ACTIVE | No (migration op) |
| Beads AGENTS.md cleanup | `migrate.go:405–413` | ACTIVE | No (migration op) |
| Beads data import | `legacydolt.go:37–158` | ACTIVE | No (until cutover) |
| Beads data **export** | `legacydolt.go:160–283` | **TEST-ONLY** | **Yes, with legacydolt retirement** |
| Startup preflight | `init.go:117–151`, `cli.go:448–467` | ACTIVE | No |
| Preflight bypass list | `init.go:105–115` (init/migrate/help/completion) | ACTIVE | No |

### Findings

| # | Finding | Severity | Quick-win? |
|---|---------|----------|-----------|
| M1 | No sunset date documented on any migration file | **Medium** | Yes — add `[SUNSET: YYYY-MM-DD]` comments |
| M2 | `legacydolt.Export()` exists only for test-setup, yet is exported | Low | Yes — unexport or move to testhelpers |
| M3 | `template-eject` branch work appears complete; only awaiting merge | — | — |
| M4 | Migration is correctly decoupled: single import site (`migrate.go:677`), no other modules reference `legacydolt` | — | — |
| M5 | Test coverage gaps: no partial-write-failure test for `migrate --apply` (e.g., SIGTERM mid-write) | Low | No |

### Deletion timeline (estimated)

| Component | Lines | Safe after |
|-----------|-------|-----------|
| `internal/legacydolt/` (entire package) | 406 | v2.0 (1 release) |
| `migrate.go` (body) | 843 | v2.1 (2 releases) |
| `init.go` beads logic | ~60 | v2.1 |
| CLI beads preflight (`cli.go:448–467`) | ~100 | v2.1 |
| `BeadsMigrationRequiredError` type + handlers | ~200 | v2.1 |
| Beads markers in `agents_internal.go` | ~20 | v2.2 |
| **Total deletable** | **~1,600** | **spread across v2.0–v2.2** |

---

## 4. Cross-cutting survey (remaining 24 files)

### Module health

All remaining modules are **healthy** with three minor callouts:

- `internal/cli/ready_state.go` (404 lines) — correctly architected (`isReadyBlocked` is an explicit predicate, `readyBlockingKinds` is the single source of truth). Flag: the exported name `sortByReadiness` (L173) invites future callers to treat "readiness" as an annotation property, which would violate the "annotations are neutral facts" law. **Fix: rename to `sortByBlockingAnnotations`.**
- `internal/merge/merge.go:101` — `optionalIssuePtr` is uppercase-exported but only used within the file.
- `cmd/lit/main.go` vs `cmd/lnks/main.go` — byte-identical.

### Architectural principles — audit results

| Principle | Verdict | Evidence |
|-----------|---------|----------|
| Annotations = neutral facts (no `len(annotations)==0` as proxy) | ✓ Upheld | **Zero** grep matches across the audit scope. All predicates explicit. |
| Dataflow, not control flow | ✓ Mostly upheld | Exceptions: store.go transient-retry (S3); cli.go `runUpdate` branch split (C5) |
| One source of truth | ✓ Upheld | 20+ `[LAW:one-source-of-truth]` annotations in code, none violated |
| Single enforcer | ✓ Upheld | `printValue`, `upsertManagedSection`, `runSyncMutation`, `enforceBeadsPreflight` |
| One-way dependencies | ✓ Upheld | No cycles; `store` has no upward deps; `templates → config` one-way |
| One type per behavior | ⚠ One exception | `smoothRanksIfNeeded` vs `smoothRanksIfNeededTx` in store.go (S9) |
| No defensive null guards (internal) | ⚠ Some exceptions | store.go L921/928/935/941/951 silent-skip relations on error |

### Duplication map (cross-package)

| Concept | Packages | Status |
|---------|----------|--------|
| Managed section upsert/remove | `cli.hooks`, `cli.agents_internal` | ✓ Single impl in `managed_sections.go` |
| Atomic file writes | `syncfile`, `backup` | ✓ Single impl (`syncfile.WriteAtomic`) |
| Template path resolution | `templates` only (uses `config.ConfigDir()`) | ✓ Single impl |
| Issue-ID slug normalization | `issueid`, `workspace`, `cli.automation_trace` | ✓ All share `issueid.slug` rules |
| Time-string parsing | `store.go` (8× inline) | ✗ Duplicated — see S8 |

---

## 5. Complexity blockers for future work

### 5.1 Adding a new CLI command
Today: requires editing `cli.go` in 4+ places (import, registration block in `newRootCommand`, command summary map, runner function body). Command registrations are hand-written duplicates.
**Simplification unlock:** Data-driven registry (decomposition proposal #8 for cli.go) + per-command files would reduce new-command cost to a single file addition + 1 registry entry.

### 5.2 Adding a new store operation
Today: requires manually composing lock acquisition, tx begin, tx commit, `commitWorkingSet` retry, and potentially working around the race window (S2). The lock-coordination pattern is implicit and repeated.
**Simplification unlock:** A `withMutation(ctx, func(tx) error)` helper that holds the commit lock through the commit-working-set retry would eliminate 20+ boilerplate sites AND close the race window.

### 5.3 Changing the backend (away from Dolt)
Today: Dolt-specific knowledge leaks into store.go via string-matched transient error classification (S3) and PID-based lock liveness (S14). Replacing Dolt requires updating error classification throughout.
**Simplification unlock:** Move transient-error classification behind a backend-shaped interface; isolate Dolt-specific subprocess logic in `doltcli`.

### 5.4 Removing Beads migration code
Today: Straightforward — well-decoupled, 1,600 lines removable on a clear timeline. Only blocker is deciding the cutover date.
**Simplification unlock:** Pick a sunset date, document it in migrate.go / legacydolt.go / init.go, schedule removal PRs.

---

## 6. Recommended reduction plan

### Phase 1 — Quick wins (dead weight & naming)

1. Rename `sortByReadiness` → `sortByBlockingAnnotations` (`ready_state.go:173`).
2. Unexport `optionalIssuePtr` in `merge.go:101`.
3. Delete `cmd/lnks/main.go` (or `cmd/lit/main.go`) — pick one and update docs/install scripts.
4. Add `[SUNSET: YYYY-MM-DD]` banner comments to `migrate.go`, `legacydolt.go`, `init.go` (beads sections).
5. Move `issue_ids.go` from `internal/store/` into `internal/issueid/` (it already exists as a package).
6. Extract `scanTime` helper in store.go (eliminates 8 inline parses).
7. Batch-load related issues in `GetIssueDetail` to fix the N+1.

### Phase 2 — cli.go decomposition (highest ROI, low risk)

Split cli.go into the 8 new files above. Each extraction is a self-contained PR. Order:

1. `sync.go` (biggest single win — 258 lines of `runSync` + ~400 lines of helpers)
2. `bulk.go`, `backup.go`, `doctor.go`, `dependency.go`, `issue_relations.go` (parallel, small)
3. `output.go` (touches more of the file but mostly move-only)
4. `register.go` (data-driven registry — the only change with behavioral risk; needs strong test coverage first)

**Outcome:** cli.go shrinks from 2,875 → ~1,600 lines; each new command's home becomes obvious.

### Phase 3 — store.go decomposition

Split store.go into the 6 modules above. Recommended order by risk:

1. `schema.go` (one-time migration code; least-coupled extraction)
2. `labels.go` (small, self-contained)
3. `ranking.go` + `relations.go` (parallel)
4. `import_export.go` (bulky but well-bounded)
5. `commit_lock.go` — **last**, because it's the right place to also fix S2 (race window) and S4 (boilerplate) in a single refactor: introduce `withMutation(ctx, func(tx))` that holds the lock across the commit-working-set retry.

**Outcome:** store.go shrinks from 3,320 → ~800 lines of core Store struct + method dispatch.

### Phase 4 — Migration sunset

At the agreed cutover date:

1. Delete `internal/legacydolt/` (406 lines).
2. Delete `runMigrate` and supporting code in `migrate.go` (843 lines).
3. Remove beads preflight from `cli.go` and `init.go`.
4. Remove `BeadsMigrationRequiredError` type.
5. Remove beads markers from `agents_internal.go`.

**Outcome:** ~1,600 lines deleted in one cleanup PR.

---

## 7. Risk assessment

| Change | Risk | Mitigation |
|--------|------|-----------|
| Rename `sortByReadiness` | Zero | Only one caller |
| Unexport `optionalIssuePtr` | Zero | Not imported externally |
| Delete one of `cmd/lit`/`cmd/lnks` | Low | Update install scripts, release notes |
| Extract cli.go sub-files | Low | Pure refactor; compile + existing test coverage |
| Extract store.go sub-modules | Low-Medium | Pure refactor of methods on `*Store`; preserve signatures |
| Fix commit-lock race (S2) in commit_lock.go extraction | **Medium** | Requires explicit tests for transient-retry scenarios before refactoring |
| Delete migration code | Low (if timed correctly) | Wait for confirmed user cutover; deletion is a single well-bounded PR |

---

## Appendix: Line-count profile

Top 20 non-test files by LOC:

```
3320 internal/store/store.go
2875 internal/cli/cli.go
 843 internal/cli/migrate.go
 406 internal/legacydolt/legacydolt.go
 404 internal/cli/ready_state.go
 347 internal/store/sync.go
 301 internal/query/query.go
 280 internal/rank/rank.go
 259 internal/workspace/workspace.go
 233 internal/merge/merge.go
 203 internal/store/issue_ids.go
 186 internal/cli/error_output.go
 175 internal/cli/completion.go
 175 internal/cli/automation_trace.go
 151 internal/cli/init.go
 149 internal/cli/quickstart_eject.go
 140 internal/cli/hooks.go
 139 internal/config/config.go
 133 internal/annotation/annotation.go
 132 internal/templates/templates.go
```
