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

The rules support each other. If anything ever gets a store past where it
should be, rule 1 makes the return lossless: `lit downgrade --schema` back to the
remote's version loses nothing.

## What rule 1 means, precisely

For every migration vN and every store S valid at vN:

- **Shape.** The schema of `Down(S)` equals the schema a fresh run of `Up` to
  vN-1 produces.
- **Round trip.** `Up(Down(S))` equals S on the canonical export, byte for
  byte.
- **Old code works.** The newest release binary whose `Schema.Max` is at
  most vN-1 reads, writes, and syncs `Down(S)` once it is stepped down to
  that binary's `Schema.Max`.

A Down that refuses, or one that recreates a column with a default and
documents the loss, does not meet this. Neither is an option for a migration
written from now on.

### What that allows a migration to do

A migration can add structure whose content is derivable from vN-1 data, as
v12's `is_top_level_id` is. It can add structure that holds new user data only
if the data has somewhere to live at vN-1. That place is the **stash
carrier**: a `schema_stash (table_name, row_key, column_name, value)` table
that sits outside the versioned schema. `row_key` is the row's primary key as
text, a composite key's parts joined by `CHAR(31)` so no engine's JSON
formatting enters it; `value` is the value in the same
type-faithful encoding the canonical export uses. Whichever step first needs it creates
it, no Down drops it, and the shape comparison ignores it, so it serves the
released versions below any migration that could have added it and survives
every step down. It is user data: the recovery shape map carries it
verbatim, never as bookkeeping. Two clients that stash the same row write the
same entry unless the row itself diverged, and then the row's own conflict is
the one to resolve.

- A Down that drops a column holding user data copies the column into the
  stash first. The next Up restores it for every row that still exists.
- An Up that destroys data — drops, narrows, or collapses — stashes what it
  destroys, and its Down restores it. The carrier serves both directions.
- A vN-1 binary never reads the stash. It may delete a row whose stashed
  fields then have no row to return to; that is the vN-1 user's deletion,
  not a loss.
- The stash holds only what the other version cannot represent. A step
  applies it on top of the store's current data, never in place of it, and
  deletes every entry it was written for: entries whose row is gone are
  dropped with it, because a row's absence is a user's deletion. A row is
  its key, so a row re-created under the same key is the same row. Rows an
  Up collapses into a survivor are stashed whole and return only while the
  survivor exists. A stash write records every row's value,
  null included, so a value cleared at one version stays cleared.

### How it is proven

- **Round-trip gate.** `TestEveryMigrationRoundTripsLosslessly` replaces
  `TestEveryMigrationDownIsExercised`. Every migration ships a seed fixture
  that fills every table its Up or Down reads or rewrites, not only the
  structures it adds; a migration without one fails the gate, so the gate
  cannot pass against empty tables. Each version is tested in both
  directions, each starting from data seeded at the version it starts from:
  - from vN: seed at vN, including user data in the structures vN adds;
    export; run Down; check the shape against a fresh Up to vN-1; run Up;
    compare against the export.
  - from vN-1: seed at vN-1; export; run Up then Down; compare against the
    export. This is what catches an Up that destroys data without stashing
    it. It runs for migrations written from now on; a released Up that
    destroyed data, as v4's and v7's did, already destroyed it on every store
    it ran on, so there is nothing for a Down to return.
- **Cross-version release check.** On every PR, and before every publish,
  CI builds the candidate with its own registry max as its
  released ceiling, migrates a fixture with it, steps it down to the `Schema.Max`
  of the newest release whose max is below the candidate's — across every
  migration the candidate adds, however many —
  then runs the previous release's published `lit` against it — read, write,
  sync — and steps it back up with the candidate. This is the only check that
  proves rule 1 against code that actually shipped rather than against our own Down SQL.

### Released migrations: freeze the Up, not the Down

goose never re-runs a version a store is stamped with, so a released Up must
never change. A Down is different: it always runs from the binary doing the
downgrade, never from the one that applied the Up. The content pin therefore
covers the Up section only. A released migration whose Down is not lossless is
fixed in place, and the round-trip gate covers every version from v2 up. The
baseline is excluded: going below it destroys the workspace, and
`DowngradeBelowBaselineError` refuses that.

The fix protects downgrades run by binaries built after it. A release that
shipped a lossy Down still runs that Down; the guarantee holds for a downgrade
run with the first release that carries the fixed one.

## Lowering a remote, and making it stay lowered

A local downgrade alone does not lower a shared remote for long. Every client
still running vN pulls the lowered head, re-migrates it to vN when it opens the
store, and pushes it back up.

`lit downgrade --schema <v> --remote <name>` lowers the remote and writes
the **remote hold**, `meta.schema_hold = v`, in the same commit. `meta` exists from the baseline
on, so a store at any version can hold the row. A binary reads the hold from the fetched remote head as well as from the
local store, so a local store stepped down under a hold stays down. It
migrates no store past the hold and refuses to push a raise past it. The check sits
at the one migration stepper that both opening a store and the sync reconcile's
lift go through. Open reads the live store's hold; the reconcile reads the
fetched head's hold once, at that commit, and every lift of that head runs under
it, so sync cannot re-raise what open would not. The reconcile's other lifts, of
the local head and the merge base, are read and discarded, and no hold binds them.
The hold enters each of those sites as a `version.SchemaCeiling`, the type
the released-schema ceiling already is, and the lower of the two binds
(`SchemaCeiling.Lower`); a store with no row yields the unbounded ceiling, so
no site asks whether a hold exists. A binary whose code needs a schema above
the hold refuses to open the store, as it refuses any raise above a ceiling
(`SchemaCeilingError`), naming the hold as the ceiling's source; the store
knows the hold's version, not which release carries it, so the refusal names
no release tag. The remedy is a release at the hold (`lit downgrade --to
<tag>`), whose store steps down with it, which rule 1 makes lossless. The row
is removed only by `lit upgrade --remote <name>`.

A lowering reaches a remote only as a held head. A push whose head is below R
and carries no hold is refused (`SchemaLoweringRequiresHoldError`, the third
verdict beside the ceiling's and the raise's): an unheld lowering is re-raised
by the next client that syncs, and it is what a lift would send when the
lowering before it never landed. The verdict compares schemas, not histories,
so a backlog that is only behind a release's raise is refused the same way; its
remedy, like the raise's, is the pull first, and the remote downgrade only when
the lowering is meant. `lit downgrade --schema <v> --remote <name>` and
`lit upgrade --remote <name>` both run in the sync open, which never migrates:
that is what lets a binary above the hold open a held store to lift it, or to
run the lowering again after a push that failed. The lift's push sends a head
at the hold's schema and so raises nothing; the remote rises when a migrating
open brings a store forward and a release's `lit sync push` sends it.

Binaries released before the row existed do not read it. The guarantee holds
from the first release that does.

A landing adopts the remote head at any schema. The receive's behind
fast-forward and a take-remote with nothing to carry move the branch to the
fetched head and author no commit, so the refusal that keeps a reconcile from
replaying onto a newer remote's head (`RemoteSchemaAheadError`, below) has
nothing to guard there, and neither does the hold. What the binary may then
do with the store it finds is the open's one rule: a held lower head migrates
no further than its hold, and a head above the binary's schema is readable
while its baseline shape is intact and writable while the binary supports the
store's minimum writer schema, refused otherwise with `lit upgrade` named.
The same landing is what lets a lowered remote bring a client down. Every
settle reads the remote head's schema against the binary and reports it, so
the owner channel ends a remote-schema-ahead episode on that read, never on
the state the settle ended in.

A push at the remote's own schema lands. A push is judged against R alone
unless it raises the remote: with H ≤ R, no verdict compares H or R to the
binary that pushes. The open admitted the writes the head carries, under the
store's minimum writer schema: the model's writes name this binary's
columns, an update leaves the columns a newer schema added as it found them,
and a write that re-creates one row (a new row, a single-valued relation
edge) fills only the columns it knows, which is the case a migration whose
column an older writer must fill declares the minimum writer schema for
(`internal/store/migrations/README.md`); so a head at R carries what the
open's rule admits, and pushing it regresses nothing that rule did not
already allow. Whether a binary may write at a schema above its own is the
open's judgement, made once; the push does not make it again under a
stricter rule. The ceiling is
the second condition on a raise, as rule 2 states it, and judges nothing
else: a head at or below R sends no schema the remote does not already hold,
since R reached the remote by a release's push and sits at or below its own
hold. (A head a pre-hold binary re-raised past its hold is the one case
where a push at R above the hold lands; the guarantee was already scoped to
binaries that read the row.) An unheld head below R is the lowering verdict's
to refuse.

The open's admission covers the model's writes and no other. A write that
re-creates rows wholesale from this binary's columns, a delete and an insert
of every row it touches, resets the columns a newer schema added to their
defaults on rows no one changed: a regression within R's schema that no
comparison of H to R would see and no migration's declaration foresees, so
it is refused where it writes, never at the push. The
reconcile's replay is such a write, re-creating every row a folded commit
changed, and the reconcile refuses a head whose schema is above this
binary's (`RemoteSchemaAheadError`) before any write, on its captured read of
R, with `lit upgrade` as the remedy; the owner channel's `remote_schema_ahead`
episode ends on a settle's read of the remote head within the binary's
support, never on a landed push. `lit import` and a snapshot restore rewrite
whole tables the same way, and today the open admits them on a workspace
ahead of the binary; what they may write there is theirs to decide, by the
open's rule, and the push does not stand in for that decision. So across a
release that raises the shared remote, a checkout on the previous release
reads, writes, and pushes, and upgrades before it can reconcile a divergence.

## What rule 2 means, precisely

Let R be the remote head's schema and H the schema of the head a push sends.
A push with H > R — a **raise** — succeeds only when both hold:

- the binary's `version.Origin` is `release`, and
- H is at most the binary's own `Schema.Max`.

The second condition is the released-schema ceiling's own refusal
(`SchemaCeilingPushError`), which runs before the raise check; a release's
ceiling is its `Schema.Max`. A raise that passes it refuses with
`SchemaRaiseRequiresReleaseError`, whose message names R as of the last fetch
and the remedy: fetch, since a release may have raised the remote since, and
push once more; a push still refused is raised by a release's own `lit sync
push`. Stepping the store down is never the remedy: the head refused is one a
release carries, and the next open would only migrate it back.

`lit downgrade --schema <v>` steps the store's schema and leaves the binary in
place; `--to <tag>` installs that release's binary and steps the store to its
`Schema.Max`.

The second condition is checked at the push, against the schema of the head
being sent. Push-from-clone and compact-and-push send refs without opening
the store, so the open-time refusal of a store above `Schema.Max` does not
cover them.

There is no "highest released schema" constant. A release binary's whole
registry is released by definition, because CI cuts the tag at the commit it
was built from. That holds only if `release` means a tagged build: goreleaser's
snapshot mode — the master-push proof in `release-validate.yml`, whose
binaries are uploaded as artifacts, `release-smoke.yml`, a local
`goreleaser build --snapshot` — stamps `Origin=snapshot`, which counts as
non-release. The second condition stops a release binary from pushing a
store that a dev build migrated past it.

Origin fails closed: goreleaser's tagged build is the only producer that
stamps `release`, and a binary with nothing stamped reads as from-source.
`stamp_sites_test.go` pins that.

After a schema-raising release, the shared remote stays at the old schema, and
source builds at the new one cannot push to it, until someone runs the release
binary's `lit sync` once. That is the routine raise, not a failure.

### Every way a raise can reach a remote

| Path | Why it refuses |
|---|---|
| A dev build carrying an unreleased migration pushes a scratch store | Raise from a non-release binary (and an existing store never got there: the released ceiling refuses the open) |
| A dev build at a released schema pushes over an older remote | Raise from a non-release binary — only a release raises a remote |
| Compact-and-push, push-from-clone, the reconcile's replay commit, `--force` | The check sits at the one point every push sends refs from |
| No tracking ref, so R is unknown | Unknown R counts as a raise; fetch first |
| A push that creates the remote | Counts as a raise from nothing; a dev build creates a remote only by having a release push it |

The raise policy is a value the CLI's composition root builds from the
`version.Origin` stamp and hands to the store on the run's `environ.Env`. Store
tests construct the store with whatever policy they exercise, and CLI tests
build the Env with the policy they exercise; there is no runtime test mode.

Out of reach of any "reasonable circumstance", and so out of scope: a build
that deliberately stamps `-X …Origin=release`, and a `dolt push` run by hand
against the remote.

### Unreleased schemas never reach an existing store

The raise rule guards remotes. A second check guards local stores, so a dev
build's unreleased migration never strands a real workspace in the first
place. Every binary carries a stamped **released ceiling**: goreleaser's
tagged build stamps the binary's own registry max; its snapshot builds,
`install.sh` source mode and `just build` stamp the highest migration in the newest release tag as
`scripts/release-tags.sh` and `tools/schema-release-check` already compute it,
and an unstamped binary has ceiling 0. No migration of an existing store moves
it past the ceiling, whether it runs on open or in the sync reconcile's lift
of a fetched head. Only a store created empty, holding no fetched head, is
exempt, so new migrations stay developable in scratch workspaces. Like the raise policy, the
released ceiling enters at the composition root, and tests construct stores with the
full registry.

The two checks answer different questions. The released ceiling asks whether this
schema has been released. The raise rule asks whether this binary is a
release. A dev build at a released schema passes the first and fails the
second, which is the case rule 2 exists for.
