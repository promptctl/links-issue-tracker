# Schema reversibility: every raise steps back losslessly, and only a release raises a remote

Status: accepted design, 2026-11-02. Written tense-independent — it reads as a
specification before implementation and as a record of the architecture after.
If behavior and this document disagree and no superseding design exists, the
divergence is a bug in one of them.

## The two rules

1. **Every schema raise has a safe, lossless way back.** A store, local or
   remote, at schema vN can be stepped back to vN-1, and nothing a user wrote
   is lost. Stepping it up again returns exactly the store it was.
2. **A dev build never raises a remote's schema.** No binary built from a
   working tree can push a head whose schema is above the remote's head,
   whatever the command or flag. Raising a remote is something only a
   published release does.

The rules support each other. Rule 2 refuses at push time, after a dev build
may already have migrated its local store. Rule 1 makes that recoverable:
`lit downgrade` back to the remote's version loses nothing.

## What rule 1 means, precisely

For every migration vN and every store S valid at vN:

- **Shape.** The schema of `Down(S)` equals the schema a fresh run of `Up` to
  vN-1 produces.
- **Round trip.** `Up(Down(S))` equals S on the canonical export, byte for
  byte.
- **Old code works.** The vN-1 release binary reads, writes, and syncs
  `Down(S)`.

A Down that refuses, or one that recreates a column with a default and
documents the loss, does not meet this. Neither is an option for a migration
written from now on.

### What that allows a migration to do

A migration can add structure whose content is derivable from vN-1 data, as
v12's `is_top_level_id` is. It can add structure that holds new user data only
if the data has somewhere to live at vN-1. That place is the **stash
carrier**: a `schema_stash (table_name, row_key, column_name, value)` table
that one migration adds, empty, so adding it is itself lossless.

- A Down that drops a column holding user data copies the column into the
  stash first. The next Up restores it for every row that still exists.
- An Up that destroys data — drops, narrows, or collapses — stashes what it
  destroys, and its Down restores it. The carrier serves both directions.
- A vN-1 binary never reads the stash. It may delete a row whose stashed
  fields then have no row to return to; that is the vN-1 user's deletion,
  not a loss.

### How it is proven

- **Round-trip gate.** `TestEveryMigrationRoundTripsLosslessly` replaces
  `TestEveryMigrationDownIsExercised`. Every migration ships a seed fixture
  that fills the structures it adds; a migration without one fails the gate,
  so the gate cannot pass against empty tables. For each version the gate
  seeds, runs Up, exports, runs Down, checks the shape against a fresh Up to
  vN-1, runs Up again, and compares against the first export.
- **Cross-version release check.** `release-validate.yml` migrates a fixture
  with the candidate binary, steps it down one version, then runs the previous
  release's published `lit` against it — read, write, sync — and steps it back
  up with the candidate. This is the only check that proves rule 1 against code
  that actually shipped rather than against our own Down SQL.

### Released migrations: freeze the Up, not the Down

goose never re-runs a version a store is stamped with, so a released Up must
never change. A Down is different: it always runs from the binary doing the
downgrade, never from the one that applied the Up. The content pin therefore
covers the Up section only. A released migration whose Down is not lossless is
fixed in place, and the round-trip gate covers every version from v2 up. The
baseline is excluded: going below it destroys the workspace, and
`DowngradeBelowBaselineError` refuses that.

## Lowering a remote, and making it stay lowered

A local downgrade alone does not lower a shared remote for long. Every client
still running vN pulls the lowered head, re-migrates it to vN when it opens the
store, and pushes it back up.

`lit downgrade <v> --remote <name>` lowers the remote and writes
`meta.schema_ceiling = v` in the same commit. `meta` exists from the baseline
on, so a store at any version can hold the row. Every binary that reads the row
does not migrate a store past it on open and refuses to push a raise past it;
a binary whose code needs a schema above the ceiling refuses to write and names
the release to install. The row is removed only by
`lit upgrade --remote <name>`.

Binaries released before the row existed do not read it. The guarantee holds
from the first release that does.

## What rule 2 means, precisely

Let R be the remote head's schema and H the schema of the head a push sends.
A push with H > R — a **raise** — succeeds only when both hold:

- the binary's `version.Origin` is `release`, and
- H is at most the binary's own `Schema.Max`.

Anything else refuses with `SchemaRaiseRequiresReleaseError`, whose message
names R and the `lit downgrade R` that returns the local store to it.

There is no "highest released schema" constant. A release binary's whole
registry is released by definition, because CI cuts the tag at the commit it
was built from. The second condition stops a release binary from pushing a
store that a dev build migrated past it.

Origin already fails closed: goreleaser is the only producer that stamps
`release`, and a binary with nothing stamped reads as from-source.
`stamp_sites_test.go` pins that only goreleaser stamps `release`.

### Every way a raise can reach a remote

| Path | Why it refuses |
|---|---|
| A dev build carrying an unreleased migration migrates the local store and pushes | Raise from a non-release binary |
| A dev build at a released schema pushes over an older remote | Raise from a non-release binary — only a release raises a remote |
| Compact-and-push, push-from-clone, the reconcile's replay commit, `--force` | The check sits at the one point every push sends refs from |
| No tracking ref, so R is unknown | Unknown R counts as a raise; fetch first |
| A push that creates the remote | Counts as a raise from nothing; a dev build creates a remote only by having a release push it |

The raise policy is a value the CLI's composition root builds from
`version.Info` and hands to the store. Tests construct the store with whatever
policy they exercise; there is no runtime test mode.

Out of reach of any "reasonable circumstance", and so out of scope: a build
that deliberately stamps `-X …Origin=release`, and a `dolt push` run by hand
against the remote.

### The window between merge and release

A dev build carrying a migration that is merged but not yet released can
migrate a local store but cannot push from it. Refusing to open the store
instead would block every developer from using their own build on their own
workspace during that window. The refusal at push names the lossless way back.
