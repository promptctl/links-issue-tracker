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
| `TestEveryMigrationRoundTripsLosslessly`   | `internal/store/migration_round_trip_test.go`             | Runtime: every Down from v2 up restores the schema and, after Up, the data it started from.   |

A Down that runs but loses data is worse than no Down: the registry would
claim a downgrade the runtime cannot deliver.

## Writing a new migration

1. Create `NNNNN_<short-name>.sql` where `NNNNN` is the next free zero-padded
   number. The baseline is `00001_baseline.sql` (frozen — see header
   comment in that file).
2. Write the `+goose Up` section: the forward-direction schema change.
3. Write the `+goose Down` section: the inverse. It MUST contain at least one
   real SQL statement and that statement MUST execute cleanly when goose runs
   it against a workspace that has just had the Up applied, and it MUST be
   lossless (see "The lossless contract" below).
4. Add a seed, `internal/store/testdata/migration_seeds/<same file name>`: SQL
   that fills a store at your version, covering every table your Up or Down
   reads or rewrites, user data in the structures you add included. The
   round-trip gate fails by name until it exists.
5. Pin the hash of the file's Up section in `pinnedVersionContent`
   (`version_reuse_test.go`), in this same PR. `TestReleasedMigrationsAreContentPinned`
   fails until you do, and its message prints the exact line to add. This is what
   makes the version number's Up immutable from release forward — see
   "Released migrations are Up-frozen" below.

A skeleton, for a column whose every value is derivable from data the version
below already holds, so dropping it loses nothing:

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE issues ADD COLUMN is_closed BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET is_closed = (status = 'closed');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN is_closed;
-- +goose StatementEnd
```

A column that holds new user data needs the stash carrier in its Down; see
below.

## A migration that changes a write rule

A migration can change what may be written: v7 put every ticket inside a
container, and a lit older than v7 still creates tickets with no parent in a
store v7 migrated. Such a migration declares itself the store's **minimum
writer schema**. A lit whose registry stops below that version refuses to
write to the store, naming `lit upgrade`; it still reads the store
(`operability`, `internal/store/schema_operability.go`).

The Up section records the migration's own version:

```sql
-- +goose StatementBegin
INSERT INTO meta (meta_key, meta_value) VALUES ('min_writer_schema', '12')
ON DUPLICATE KEY UPDATE meta_value = '12';
-- +goose StatementEnd
```

The Down section restores the version the previous declaring migration
recorded, or, when no earlier migration declared one, deletes the row:

```sql
-- +goose StatementBegin
DELETE FROM meta WHERE meta_key = 'min_writer_schema';
-- +goose StatementEnd
```

Only a lit that reads the row is held to it. A lit built before the row
existed writes regardless, whatever its registry: every release through
v0.20.1 (schema 9), and builds of the schema 10 and 11 registries made before
it.

## Released migrations are Up-frozen

goose keys migrations by version **number**, not by content: a workspace that has
applied version *N* is stamped on *N* and will never re-run it. So if version *N*'s
file is later refilled with different content, that content silently never reaches
already-stamped workspaces — missing columns, absent tables. This is exactly how the
migrate-drift epic (`links-migrate-drift-hh1t`) bricked workspaces: a baseline rewrite
refilled version slots 2 and 3 with new content while old workspaces stayed stamped on
the deleted content.

Two gates make a version number's content immutable from release forward. For v2+ that content is
the Up section only: goose never re-runs a stamped version's Up, but a Down always
runs from the binary doing the downgrade, so a released Down is fixed in place.

| Gate                                       | Lives in                                                  | What it proves                                                                                   |
| ------------------------------------------ | --------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `TestBaselineFileIsFrozen`                 | `internal/store/migrations/baseline_frozen_test.go`       | v1 (`00001_baseline.sql`) bytes never change — pinned to one sha256.                              |
| `TestReleasedMigrationsAreContentPinned`   | `internal/store/migrations/version_reuse_test.go`         | Every non-baseline released version's Up section and filename match `pinnedVersionContent`, and none is deleted, renamed, duplicated, or left unpinned. |

`[LAW:single-enforcer]` one content enforcer per version number: baseline_frozen owns
v1, the pin manifest owns v2+. To change a released migration's effect, **never edit
its Up or reuse its number** — add the change as the next free version number and pin
that. The gate's failure message says the same thing when it fires.

## The lossless contract

`design-docs/schema-reversibility.md` ("What rule 1 means, precisely") is the
authority. For every migration vN and every store S valid at vN:

- **Shape.** The schema after Down equals the schema a fresh Up to vN-1
  produces.
- **Round trip.** Up after Down gives back S, byte for byte on the export.
  Down after Up, from a store at vN-1, gives back that store.
- **Old code works.** The release binary for vN-1 reads, writes, and syncs the
  stepped-down store.

A Down that refuses, or that recreates a column with a default and documents
the loss, does not meet this.

### The stash carrier

Data the other version cannot represent goes in `schema_stash (table_name,
row_key, column_name, value)`, a table outside the versioned schema. No Down
drops it, and the gate's comparisons leave it out.

- A Down that drops a column holding user data copies it into the stash
  first, and the next Up restores it to every row that still exists.
- An Up that drops, narrows, or collapses data stashes what it destroys, and
  its Down restores it.
- A step applies the stash on top of the current data and deletes the entries
  it used. Entries whose row is gone are dropped with it.

The table's one definition is `stash_carrier.ddl`, embedded as
`migrations.StashCarrierDDL`. Every step that stashes or restores runs it
first, copied byte for byte, so whichever step a store meets first creates the
table. A test fails on any registry migration that defines `schema_stash`
other than by that copy, or alters or drops it.

The carrier admits string columns (`VARCHAR`, `TEXT`), which is every column
a released Down drops. The template below restores columns of rows that still
exist. A Down that drops a whole table (v9 `purges`, v11 `plan_reviews`) meets
an Up that re-creates it empty, where the `UPDATE ... JOIN` matches nothing
and the `DELETE` then discards every entry; its rows need re-inserting from
the stash instead. The design asks for the canonical export's encoding,
but that encoder is Go and a goose step is SQL. So the step encodes in SQL,
and the round-trip gate proves the result against the export. A column of
another type needs its encoding here and a seeded column of that type in
`TestTheStashCarrierRoundTripsAColumnItsDownDrops` first.

For table `<t>`, column `<c>`, and `<key>` =
`CAST(JSON_OBJECT('<pk1>', <t>.<pk1>, ...) AS CHAR)` over the primary key in
key order:

```sql
-- Stash: in a Down before it drops <c>, or in an Up before it destroys <c>.
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT '<t>', <key>, '<c>', JSON_ARRAY(SUBSTRING(<c>, 1)) FROM <t>;

-- Restore: once <c> exists again, in the Up that re-adds it or the Down of a destructive Up.
UPDATE <t> JOIN schema_stash s ON s.table_name = '<t>' AND s.column_name = '<c>' AND s.row_key = <key>
  SET <t>.<c> = IF(JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) = 'NULL', NULL, JSON_UNQUOTE(JSON_EXTRACT(s.value, '$[0]')));
DELETE FROM schema_stash WHERE table_name = '<t>' AND column_name = '<c>';
```

- Each statement is its own goose statement.
- The one-element array keeps a NULL (`[null]`) apart from a missing entry.
- `SUBSTRING(<c>, 1)` is load-bearing. Dolt's `JSON_ARRAY` over a `TEXT` column
  encodes the column's storage address instead of its text, and
  `CAST(... AS CHAR)` does not avoid it.
- `REPLACE` lets a later stash of the same row overwrite an earlier one.
- An entry whose row is gone matches nothing and is deleted with the rest.

### Exemptions

The gate holds two lists in `migration_round_trip_test.go`, each entry naming
its reason:

- `pendingLosslessDown`: released Downs that lose data today. They are fixed
  in place (the pin covers only the Up), and the list empties as they are.
- `releasedDestructiveUp`: released Ups that destroyed data on every store
  they ran on (v4, v7, v10). No Down can return what is already gone, so the
  check from the version below skips them. This list is permanent and closed.

An exempt version that passes fails the gate, so the lists only shrink. A new
migration is never added to either.

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

`[LAW:no-defensive-null-guards]`: the gates are positive ("has Down" /
"Down round-trips"), not negative ("guards against missing Down at runtime").
A missing Down is a build-time failure, not a runtime defense.

## The baseline's Down

`00001_baseline.sql` ships a Down section that drops every baseline table.
This is **destructive** — running it leaves a workspace with no schema and
no data. The downgrade pipeline (`Downgrade()`) refuses this case by default
*before* invoking goose, so the destructive Down is unreachable from the
user-facing `lit downgrade` command. The Down section exists in the file
because:

1. The static gate requires it, and the round-trip gate checks it removes
   every baseline table.
2. Future tooling (e.g. test fixtures, schema-reset helpers) may want to
   exercise the down-to-zero path explicitly.

The refusal of "downgrade past baseline" is `Downgrade()`'s responsibility,
not the baseline file's. See `internal/store/downgrade.go` (the
`DowngradeBelowBaselineError` path) for the refusal shape — it fires before
goose is invoked, so the baseline's destructive Down is unreachable from
the user-facing `lit downgrade` command.
