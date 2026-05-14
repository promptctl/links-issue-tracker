# Migration Recovery Discipline

Background companion to the `links-migrate-heal` fix. The hotfix patches the
specific symptoms; this document is the systemic discipline that prevents the
same class of bug from shipping again.

## What broke

PR #119 ("goose changeset registry + Dolt overlay + compat-window + skew")
introduced `goose_db_version` as the authority on "what migrations are
applied," then asked the rest of the runner to trust that authority.
Three concrete bugs followed, each an instance of the same root mistake:

### Bug 1 — Self-erasing safety net

`migration_quarantine` (the table the runner writes to when a migration is
quarantined) was itself created by a goose migration (`v2`). The pre-migration
safety branch was created **before** the goose batch ran, so if any migration
in the batch failed, the revert undid every applied migration — including the
one that created the quarantine table. The runner then tried to record the
failure into a table that had just been deleted, producing the
`migration_quarantine table absent` event you can see in real-workspace logs.

### Bug 2 — Trust without verification

`runMigrations` short-circuited to "no work" when `goose_db_version` reported
every registered version applied. It never verified that the tables those
versions were supposed to have created actually existed on disk. A workspace
whose schema disagreed with its goose stamp — through any cause: partial
revert, manual `DROP TABLE`, a mid-migration crash — would slip past Open and
1146 on the next mutation.

### Bug 3 — One-time adoption, forever-stale alterations

`reconcileLegacySchema` (probe-gated renames, ADD COLUMN guards, data
backfills) ran **only during adoption**: the one-shot transition from a
pre-goose workspace shape to the new stamped state. New legacy
transformations added to the function after a workspace had already adopted
never applied — including the `issue_events.assignee → actor` rename that
field-history shipped. Workspaces adopted before field-history landed kept
the old column name forever, and the field-history code stopped working
against them.

## Root cause class

**The system treated schema-state truth as a single-source authority
(`goose_db_version`) when the storage backend cannot enforce that
single-source claim atomically.**

Dolt's `DOLT_RESET --hard` resets the working set, but the per-migration
commits in the batch ahead of the safety branch can survive selectively
depending on the path the revert takes. Once disk truth and stamp truth can
diverge, every code path that *trusts* the stamp produces a bug shaped like
one of the three above.

The fix is not to make the trust harder to break. It's to **stop trusting**.
The runner verifies disk shape against canonical statements every Open, with
idempotent CREATE TABLE IF NOT EXISTS and probe-and-execute alterations, and
treats `goose_db_version` as a *hint* for "which versions probably ran"
rather than the truth about "which tables exist."

## Architecture after the fix

Every `Open` now runs this sequence:

```
bootstrapInfraSchema       — CREATE TABLE IF NOT EXISTS for migration_quarantine,
                             migration_log. Runs BEFORE the safety branch is
                             created, so a revert preserves them.
↓
runMigrations              — goose applies pending versions as before, but
                             every migration body is idempotent
                             (CREATE TABLE IF NOT EXISTS, etc.) so re-applying
                             over partial state succeeds.
↓
ensureCanonicalSchema      — CREATE TABLE IF NOT EXISTS for every canonical
                             application table + CREATE INDEX with duplicate-
                             key-name swallow. Heals "goose stamped but table
                             missing" divergence.
↓
convergeLegacyAlterations  — All probe-gated renames, ADD COLUMN guards, and
                             data backfills that legacy workspaces need. Runs
                             every Open so newly-added transformations apply
                             to long-adopted workspaces.
↓
smoke gate                 — runSmokeTests inside migrate(). If any canonical
                             table fails its probe after the heal, refuse to
                             return a store handle. The binary fails loudly
                             instead of handing out a broken connection.
```

Each step is idempotent. A healthy workspace traverses all four with zero
mutations and zero commits.

## Three discipline rules going forward

These rules apply to anyone (human or agent) modifying the schema layer.

### Rule 1 — Every schema-mutating migration ships with a recovery-path test

For each new migration that creates or alters tables, the contributor adds at
least one test in `internal/store/migration_recovery_test.go` covering:

- **Stamped-but-missing:** drop the table the migration created, re-open,
  assert it returns.
- **Orphan-before-stamp:** synthesize the table existing but `goose_db_version`
  not yet at this version, re-open, assert the migration succeeds (idempotent).
- **Same-batch failure preserves infra:** if the migration adds infrastructure
  the runner relies on, verify a sibling failure does not erase it.

The `TestEveryCanonicalTableRecoversIndividually` matrix is the catch-all:
adding a new table to `applicationTables` without updating
`canonicalSchemaStatements` / `infraSchemaStatements` / `smokeProbes` causes
the matrix to fail. The contributor cannot ship a half-wired migration.

### Rule 2 — Auto-heal logic is unconditional, never adoption-only

Probe-gated alterations (ALTER ADD COLUMN, RENAME COLUMN, data backfills)
go in `convergeLegacyAlterations`, which runs every Open. Anything placed in
an adoption-only path (where it runs once and never again) will rot in the
field: the next workspace generation needing the transformation will never
get it.

If a transformation truly is one-shot (e.g., data migration that runs once
and changes a row's identity), it ships as a numbered goose migration, not
as a probe in `convergeLegacyAlterations`.

### Rule 3 — Smoke probes are part of the type system, not a doctor convenience

`smokeProbes` enumerates the canonical schema. Every entry in
`applicationTables` must have a probe (verified by
`TestCanonicalSchemaCoversEverySmokeTable`). `runSmokeTests` runs at the end
of every `migrate()`. A workspace that fails smoke cannot be opened — period.
The gate's *only* job is to prevent the binary from handing out broken
handles when the canonical reconcile couldn't heal what it found.

## Concrete CI gates added by this PR

1. **`TestEveryCanonicalTableRecoversIndividually`** — drops each canonical
   table individually and reopens. Catches any future migration that adds a
   table to `applicationTables` without wiring it into auto-heal.

2. **`TestCanonicalSchemaCoversEverySmokeTable`** — static coverage check
   linking `applicationTables` to `canonicalSchemaStatements` / `infraSchemaStatements`
   to `smokeProbes`. New tables must be wired in all three places.

3. **`TestInfraBootstrapPrecedesSafetyBranch`** — pins the ordering invariant
   that infrastructure tables exist before any safety branch is created.

4. **`TestSecondOpenAfterHealMakesNoCommit`** — idempotence guard. The heal
   path must not generate commits on already-healthy workspaces.

5. **`TestSmokeRunsAtOpenAndRefusesBrokenWorkspace`** — verifies the smoke
   gate fires on unhealable divergence (dropped column that no auto-heal step
   re-adds).

6. **`TestAutoHealRecoversTopicColumnDrop`** — pairs with #5: verifies the
   contrasting case where auto-heal *should* succeed and the gate stays out
   of the way.

7. **`TestStampedButTableMissingHeals` / `TestOrphanTableSelfHeals` /
   `TestFirstOpenFailureSurvivesQuarantineErase`** — the three direct
   reproductions of the user's bug report, kept as regression guards.

## Why the PR #119 test suite missed this

The existing migration tests covered three workspace shapes:
- Fresh (empty)
- Pre-goose (full schema, no `goose_db_version`)
- Already-on-goose (everything stamped, everything present)

None of those shapes had **stamp truth diverging from disk truth.** Every
test set up a workspace that was internally consistent before running the
runner. The "what if `goose_db_version` says applied but the table is missing"
shape was never constructed — because constructing it requires deliberate
synthesis of a corrupted state, and the test author was thinking "valid
states the runner needs to handle," not "invalid states the runner will
encounter in production because Dolt's revert is non-atomic."

The auto-revert test `TestRunnerAutoRevertsBrokenMigration` came close —
it injected a failing migration and verified revert + quarantine. But it
artificially pre-applied `v1`, `v2`, `v3` in a separate first Open before
injecting the failure on the second Open. That made the quarantine table
always present from the prior open's history. The user's actual failure
mode — the failure landing in the same batch that creates the quarantine
table — was never exercised.

The systemic lesson: **happy-path tests verify the contract; recovery-path
tests verify the system.** A migration system that has only happy-path tests
will ship bugs at exactly the recovery boundaries it was designed to protect.

## What this prevention plan does NOT do

- It does not eliminate Dolt-specific revert non-atomicity. That requires a
  conversation with Dolt about transactional DDL guarantees, and is out of
  scope here. The fix is to never depend on revert being atomic.
- It does not catch *data* drift between workspaces (only schema). A
  follow-up effort might extend the smoke probes to cover invariants on
  row content (e.g., "every issue with `status='closed'` has a non-NULL
  `closed_at`"). That is checkable today via `lit doctor`, but not gated
  at Open.
- It does not protect against malicious schema corruption. The recovery
  posture is "automatic fixes for accidental divergence"; deliberate
  tampering still surfaces via `lit doctor`.

## When this discipline gets revisited

When goose migrations start carrying meaningful data changes (not just
schema), the "probe and execute" idiom in `convergeLegacyAlterations`
will need expansion to cover row-level state. The current design assumes
that the canonical workspace shape is a pure function of "tables exist
with the right columns" — once data-shape invariants enter the picture,
the heal path needs more structure than CREATE TABLE IF NOT EXISTS gives.
