# Workspace configuration lives in the store

Status: draft / proposal. Written 2026-10-27 for owner review. This is the
foundation the four designs written the same day stand on
([custom-fields.md](custom-fields.md), [grooming.md](grooming.md),
[workflow-sets.md](workflow-sets.md), [phases-and-rework.md](phases-and-rework.md)),
and it answers one rule the owner set that day: *"Whatever we did today should
be cross-repo capable."*

Revised 2026-10-28: the storage design follows the
[event-store design's §migration](event-store/design.md), and this document
was revised to match the decisions recorded there.

## The rule, made concrete

A lit store can be read from outside its repository. `lit ls --at <dir>` lists
a discovered store, and `lit stores --counts` rolls up ready / in-flight /
blocked counts across every store under a root. Both open the storage
directory, `<git-common-dir>/links`, and nothing else: no repository root, no
`.lit/config.toml`, no working tree. Anything lit needs in order to say what a
backlog *is* has to be inside that directory, or the outside reader computes a
different answer than the repository's own `lit backlog` does.

Today one such value lives outside: `ready.required_fields`. The CLI reference
documents the consequence as a caveat on both commands: readiness across the
boundary is "store-intrinsic", so a ticket the repository's own lit calls
blocked for a missing description is ready from outside. Two more values are
about to join it. The hierarchy epic puts level
names in a `[levels]` table and strategy declarations in a `[strategies]` table,
both in `.lit/config.toml`. An outside reader would render `L2` where the
repository says `epic`, and could not tell a strategy label from any other
label, which makes a cross-repository strategy report impossible to compute.
Custom field definitions ([custom-fields.md](custom-fields.md)) would be a
fourth. The owner's ruling is that this class of limitation is not acceptable,
so the question this document settles is: which configuration is workspace
truth, and where does it live so every reader sees it.

## Workspace truth versus local preference

A configuration value is **workspace truth** when two machines that disagree
about it cannot both be right about the backlog. It is a **local preference**
when they can. The test is that sentence and nothing else.

| Value | Today | Class | Reason |
|---|---|---|---|
| `ready.required_fields` | `.lit/config.toml` | workspace truth | decides whether a ticket is ready |
| `[levels]` names | planned for `.lit/config.toml` | workspace truth | the name of a level is a fact about the backlog, not about a machine |
| `[strategies]` | planned for `.lit/config.toml` | workspace truth | which labels are strategies decides what a report counts |
| custom field definitions | new | workspace truth | which fields exist, their kinds, which are required |
| workflow set adoption | new | local preference (see [workflow-sets.md](workflow-sets.md)) | the switch lives in `.lit/config.toml`, with `--global` for the user config |
| plan-review caps | new | local preference | policy for a writer in this clone; the record it produces is in the store either way |
| `sync.*`, `snapshot.retention_budget`, `claims.freshness_window`, `quickstart.soil_mode` | config files | local preference | how this machine behaves |

Everything in the first class moves into the store. Everything in the second
stays in the two config files it uses today, project layer over global layer.
A new setting is classified at birth by the same test, in the ticket that adds
it.

## The design: one document, versioned, in the store

The workspace document is the event store's config stream: workspace
configuration *"is itself an event stream with a monotone version"*
([event-store/design.md](event-store/design.md) §security-and-config). Each
write appends a new version whose body is the whole document, TOML, recorded
with its writer and time like every event. The live configuration is the
current version the fold holds. It adds no Dolt table and no migration, so it
is available once reads come from the fold (S2 in the event-store design's
§migration), and not before.

One document rather than one table per concern, for three reasons. It is the
exact payload the event-store design reserves for its config stream, so the
document is the config event's body and nothing is redesigned. It gives the
parse a single boundary: the body is parsed into one typed value once, on read, and every
consumer takes fields off that value rather than re-reading rows
(`[LAW:parse-dont-validate]`). And it keeps the human's experience whole: the
document reads like `.lit/config.toml`, same syntax, same table names, so a
user who learned one has learned the other.

The document's sections are owned by the designs that need them: `[levels]`
and `[strategies]` by the hierarchy epic, `[levels.<n>.fields.<name>]` by
custom fields. This document owns the container
and the rules below, not the sections.

## Reading and writing it

Every command that opens the store reads the live document once and carries
the typed value, so an outside reader (`--at`, `stores --counts`) and the
repository's own lit evaluate the same definitions. A store with no document is
a store at version 0, which parses to the defaults the binary ships; absent is
a legitimate state, never an error.

Writing is through lit, because the home is the store:

- `lit workspace config show` prints the live body, its version, writer and
  time.
- `lit workspace config edit` opens the live body in `$EDITOR` (the one
  subprocess the workflows subsystem already allows itself), parses the result,
  refuses an invalid document naming the key and the error, and appends version
  N+1.
- `lit workspace config set <key> <value>` changes one key for scripts and
  agents, through the same parse and the same append.

This changes a stance the workspace-schema draft took: *"no lit command mutates
it"*. That stance assumed a file in the repository. With the store as the home,
lit is the only thing that can write it, and the version stream is the review
trail a file would have had in git. The document stays human-authored in the
sense that matters: a person decides its content, lit only validates and
records.

## Moving the three values that exist or are planned

`ready.required_fields` becomes required flags on built-in fields per level
([custom-fields.md](custom-fields.md) says how). The hierarchy epic's two
tickets that write config, level names and strategies, are re-pointed at the
document before they are built; their text today says `.lit/config.toml`. The grooming rule needs nothing here: a label is already
store-resident.

A repository that already carries workspace-truth keys in `.lit/config.toml`
is not left with two sources. The first lit that knows the document reads
those keys, writes them as version 1 if no document exists, and prints that it
did so and which keys moved. From then on lit never reads them from the file,
and `lit doctor` names any that remain there, the way the workflow epic has
doctor name template overrides lit no longer reads. There is no fallback read
of the file: a fallback is a second source wearing a transition's clothes, and
transitions end.

## Observability

The command event records the config version read and whether it came from the
store or from the shipped defaults; a write records the version from and to
and the keys that changed, zero included `[LAW:nothing-unseen]`.

## Decisions for the owner

1. The classification table above.
2. Re-pointing the hierarchy epic's level-name and strategy tickets at the
   store document now, before they are built. Recommended: the alternative is building them against
   the file and moving them in a later ticket.
3. The edit surface: `edit` through `$EDITOR` plus `set` for one key, as
   proposed, or `set` alone.
