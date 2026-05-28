# Migration registry — `+goose Down` discipline

Every `*.sql` file in this directory is part of the goose changeset registry
that defines lit's schema. The bytes embedded here ARE the schema lit produces;
there is no parallel schema authority.

## The invariant

**Every migration in this directory ships a tested `+goose Down` section.**

This is a non-negotiable architectural invariant of the lit-downgrade pipeline
(see `lit downgrade` / epic `links-downgrade-t244`). The pipeline commits to
**lossless downgrade**: stepping a workspace back to an earlier schema version
must preserve user data. Lossless is impossible without inverse migrations.
Hence: every Up has a Down, in the same file.

Two CI gates enforce the invariant — neither alone is sufficient:

| Gate                                       | Lives in                                                  | What it proves                                                                               |
| ------------------------------------------ | --------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `TestEveryMigrationHasDownSection`         | `internal/store/migrations/down_section_test.go`          | Static: every `*.sql` contains a `-- +goose Down` marker followed by at least one statement. |
| `TestEveryMigrationDownIsExercised`        | `internal/store/migration_down_exercised_test.go`         | Runtime: every Down section actually runs (`goose.DownTo`) against a real workspace.         |

An unexercised Down is worse than no Down — the registry would claim
invertibility that the runtime cannot deliver.

## Writing a new migration

1. Create `NNNNN_<short-name>.sql` where `NNNNN` is the next free zero-padded
   number. The baseline is `00001_baseline.sql` (frozen — see header
   comment in that file).
2. Write the `+goose Up` section: the forward-direction schema change.
3. Write the `+goose Down` section: the inverse. It MUST contain at least one
   real SQL statement and that statement MUST execute cleanly when goose runs
   it against a workspace that has just had the Up applied.

A skeleton:

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE issues ADD COLUMN priority_band VARCHAR(16) NOT NULL DEFAULT 'normal';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN priority_band;
-- +goose StatementEnd
```

## Non-losslessly-invertible migrations

Some up-migrations lose information by design — dropping a column, narrowing
a type, collapsing rows. The `+goose Down` section is still mandatory; what
varies is **how it represents the loss**:

### Option A — refuse with a typed error

If the loss is irrecoverable (the dropped column had no derivable replacement
on disk), the refusal lives at the user-invoked boundary `Downgrade()` (see
`internal/store/downgrade.go`) — so it is symmetric with
other downgrade refusals (e.g. "downgrade past baseline would destroy the
workspace"). That boundary refuses *before* invoking goose, so the file's
Down section is unreachable from the user-facing `lit downgrade` command.

The Down section in the file is still required (both CI gates demand a
non-empty, cleanly-executing Down). Write the "would-be" SQL that goose would
run if it were ever reached — for a column drop, the `ALTER TABLE ... ADD
COLUMN ...` that restores it (defaulted however makes sense for an
unreachable branch) — and add a comment above the section pointing readers at
the `Downgrade()` refusal so they understand the file's Down is never the
operator-facing answer. The baseline file itself is the canonical example:
its Down drops every table, and `Downgrade()` refuses to invoke it.

### Option B — restore with documented loss

If the loss is acceptable to the operator (e.g. the dropped column was a
derived cache that the operator does not need restored), the Down may
recreate the column with a default value, documenting the loss with a
comment block above `-- +goose StatementBegin`:

```sql
-- +goose Down
-- LOSS CONTRACT: this Down recreates priority_band but does NOT restore the
-- per-issue value that was in place before the Up; every row gets 'normal'.
-- Operators who need exact values must restore from a pre-upgrade dbsnapshot.
-- +goose StatementBegin
ALTER TABLE issues ADD COLUMN priority_band VARCHAR(16) NOT NULL DEFAULT 'normal';
-- +goose StatementEnd
```

The CI gate does not police *which* option a migration chooses — it polices
that a non-empty Down section is present and runs. The choice itself is a
design decision that lives in the PR that introduces the migration.

## Why this lives at the registry, not at the call site

`[LAW:one-source-of-truth]`: the migration file is the single source for
both directions. There is no parallel "down scripts" directory, no helper
package keeping a map of file → inverse; both Up and Down live in the same
file that goose reads. The gate checks the same bytes goose reads, so the
"is this migration invertible" claim cannot drift from the bytes that make
the claim true.

`[LAW:types-are-the-program]`: "this migration is invertible" is encoded by
the presence of an exercised `+goose Down` section. The CI gates are the
type-checker. A migration that fails either gate is a migration the
downgrade pipeline cannot use — and the build fails, not Open, not
production.

`[LAW:single-enforcer]`: the refusal for "downgrade would destroy data"
lives in exactly one place (`Downgrade()`); the Down section communicates
the impossibility via that channel, never via scattered runtime checks.

`[LAW:no-defensive-null-guards]`: the gates are positive ("has Down" /
"Down runs"), not negative ("guards against missing Down at runtime").
A missing Down is a build-time failure, not a runtime defense.

## The baseline's Down

`00001_baseline.sql` ships a Down section that drops every baseline table.
This is **destructive** — running it leaves a workspace with no schema and
no data. The downgrade pipeline (`Downgrade()`) refuses this case by default
*before* invoking goose, so the destructive Down is unreachable from the
user-facing `lit downgrade` command. The Down section exists in the file
because:

1. The CI gate requires it.
2. Future tooling (e.g. test fixtures, schema-reset helpers) may want to
   exercise the down-to-zero path explicitly.

The refusal of "downgrade past baseline" is `Downgrade()`'s responsibility,
not the baseline file's. See `internal/store/downgrade.go` (the
`DowngradeBelowBaselineError` path) for the refusal shape — it fires before
goose is invoked, so the baseline's destructive Down is unreachable from
the user-facing `lit downgrade` command.
