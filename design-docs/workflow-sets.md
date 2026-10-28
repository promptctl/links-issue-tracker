# Workflow sets: shipped guidance that turns on and off as a unit

Status: draft / proposal. Written 2026-10-27 for owner review. The owner's
words: *"create some default workflows, but we need a way to group the
individual workflow files into like a set. You know, like we've got plan,
implementation, review, groom or merge or whatever right now. We should be
able to kind of add and remove those as a unit, maybe. Right now the feature
is cool but it feels a little too flexible. Like it's not clear how to use it."*

Revised 2026-10-28: the storage design follows the
[event-store design's §migration](event-store/design.md), and this document
was revised to match the decisions recorded there. Workflow sets add no schema, so
they are built ahead of the event store, after the strip epic.

## What exists

A workflow is a markdown file with frontmatter that names when it fires: by
label, by state entered or exited, by event. Files load from three layers,
project over global over the binary's embedded defaults, merged by id with the
nearer layer winning. Five pieces ship: one per phase and a post-close
reminder. Everything is documented in
[doc-v1-total/09-workflows.md](../doc-v1-total/09-workflows.md).

The workflow epic in flight turns every remaining
template into pieces as well: the quickstart router and its topics, the
pre-push hook. It also adds, per piece, turning a default off without a warning,
overriding into the global layer, and reporting whether an
override is unchanged, customized or stale. When it lands lit will have
twenty or so pieces, each independently overridable, and one flat `lit
workflows` listing of ids.

That flatness is the "too flexible". Nothing says which pieces belong
together, which a project has chosen to run, or what adopting lit's plan review
means in files. A user who wants the plan-review discipline and not the rest
has to know the five ids; a user who wants to drop it has to turn off five
pieces one at a time; a new discipline shipped as six pieces arrives as six
unrelated ids.

## The set

A piece declares the set it belongs to with one frontmatter key:

```
---
id: phase-plan
set: plan-review
states: [plan]
---
```

A set is a name and the pieces that claim it, nothing more: no file of its
own, no manifest to keep in step with the pieces `[LAW:one-source-of-truth]`.
A piece with no `set` is loose, and loose pieces are always on. The shipped
pieces claim shipped sets; a project's own pieces may claim a set of their
own, `set: release-process`, and that set is listed and switched exactly like
a shipped one.

A set is on or off. On is the default for every set, shipped or not. The state
is recorded in two places with one precedence, the same project-over-global
order the layers already use:

- per project, in `.lit/config.toml`, so every clone of the repository runs
  the same process;
- per user, in `~/.config/links-issue-tracker/config.toml`, so one person can
  turn a set off across every repository they work in.

```toml
[workflows.sets]
plan-review = true
grooming = false
```

A project setting wins over a user setting; an unset name is on.

The switch applies after layering, by the resolved piece's set. A project
override of a shipped piece belongs to whatever set its own frontmatter claims,
so overriding a piece does not move it, and an override that drops the `set`
key makes the piece loose and always on, which is what the author wrote.

## The commands

`lit workflows` groups its listing by set: each set's name, its state and
where the state came from, then its pieces as today; loose pieces last under
their own heading. `lit workflows sets` prints only the sets and their states.
`lit workflows set enable <name>` and `lit workflows set disable <name>` write
the project setting, or the user's with `--global`, and refuse a name no loaded
piece claims. `lit workflows dry-run` reports a piece suppressed by its set as
such, beside the pieces that fired, so a user who wonders why nothing printed
is told.

Dispatch records on the command event the sets consulted, their states, and
each piece suppressed by a set, the way it records `workflows.fired` today
`[LAW:nothing-unseen]`.

## The shipped sets

What lit ships is a proposal for the owner to edit; the mechanism does not
depend on the list.

- `orientation`: the quickstart router and its topics, once the workflow epic
  makes them pieces. On by default; whether it may be turned off is the first
  decision below.
- `phases`: the impl, review and merge pieces, which say which command records
  the next move.
- `plan-review`: the plan-phase piece and the review brief, plus the pieces the
  plan record epic adds for the round cap and the override.
- `grooming`: the three pieces [grooming.md](grooming.md) describes.
- `capture`: the post-close reminder.

A new discipline ships as a new set, and arrives everywhere on upgrade, on,
with one command to decline it. That is the answer to "add and remove as a
unit", and it is what keeps the override story the workflow epic built from
turning into a list of ids a user has to learn.

## Relation to the workflow epic

Sets sit above the per-piece controls the workflow epic adds and change none of
them. Turning a piece off is tailoring one piece; turning a set off is
declining a discipline. Composition into files and staleness
read the resolved set after the switch is applied, so a composed hook never
contains a piece from a set that is off. This design depends on the workflow
epic's per-piece off switch, which settles what "off" means for a piece, and on
its quickstart conversion, which turns the quickstart into pieces and so
creates the `orientation` set's members.

## Cross-repository

The user-level switch covers every repository on the machine; the project-level
switch travels with the committed `.lit/config.toml`. Workflows themselves fire only for the
repository being worked, so no outside reader needs them.

## Sequence, as tickets

The sequence follows the strip epic, so it is built from the primitives the
strip produces.

1. The `set` key, the grouped listing, and `lit workflows sets`.
2. The switch: both homes, the precedence, `set enable|disable`, dry-run and
   the command event.
3. The shipped pieces claim their sets.
4. Checkpoint, in vivo: a person disables `plan-review` in a scratch
   repository and sees `lit start` print no plan guidance; enables it with
   `--global` and sees it return; overrides one piece and sees it stay in its
   set; declares a project set of two pieces and switches it; reads `lit
   workflows dry-run` on a suppressed event and sees the set named.

## Decisions for the owner

1. Whether `orientation` can be turned off. The quickstart is how an agent
   learns lit exists; a project that turns it off has agents working blind.
2. The shipped set list and names.
