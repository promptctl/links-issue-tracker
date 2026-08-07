# Workflows

Workflows are user-customizable guidance that `lit` injects into its own output at
declared moments in the work lifecycle — a ticket entering `in_progress`, a `done`
closing it, an agent viewing the backlog. They are markdown files with YAML
frontmatter; the frontmatter says *when* to fire, the markdown body says *what to
inject*. Nothing here is enforced: a workflow definition never blocks, gates, or
changes what a command does — it only adds text to what the command already prints.

If you've used the `guidance-<action>-<phase>.md` templates from earlier `lit`
versions, workflows are their replacement and formalization: same idea (inject text
at a moment in the lifecycle), but declarative, user-authored, and not tied to a
fixed set of command names.

## Where definitions live

A workflow definition is any `*.md` file discovered recursively under:

- `.lit/workflows/` in the project (checked into the repo, shared with the team)
- `<config>/workflows/` globally (your own machine, applies to every project)
- the embedded defaults shipped with `lit` itself

The folder hierarchy under each root is arbitrary — you can organize files however
you like, in one flat directory or nested by topic. **The path carries no activation
meaning.** Only the YAML frontmatter decides when a definition fires.

Resolution follows project → global → embedded precedence, merged by `id`: a
project-layer file with the same `id` as a global or embedded one wins outright: the
farther layer's definition is not read at all, not merged with it field-by-field.

## Format

```markdown
---
id: my-custom-id            # optional; defaults to the file's relative path
name: My Guidance            # optional pretty name shown by `lit workflows`
labels: [needs-design, blocked]
states:
  - open                              # bare name = fires when the ticket ENTERS "open"
  - {name: closed, when: exit}        # fires when the ticket EXITS "closed"
events: [work_started, work_finished]
---
The guidance text injected when this definition fires. `<id>` is replaced with the
acted-on ticket's id when there is one.
```

Every key is optional. A file with **no** activation key at all (`labels`, `states`,
`events` all absent) is *inert*: it loads and is visible in `lit workflows`, but it
never fires. `lit workflows` flags an inert file under its Warnings section, so an
authoring mistake — a typo'd key, a forgotten dimension — is never silently invisible.

### `labels`

Fires when the acted-on ticket carries **any** of the listed labels.

### `states`

Fires when the acted-on ticket **enters or exits** a state. A transition is
expressible as one definition's exit-of-A plus another's (or the same one's)
enter-of-B — there is no separate "transition" concept to learn. Two authored shapes:

- a bare state name (`open`) — shorthand for `{name: open, when: enter}`
- `{name: <state>, when: enter|exit}` — explicit

State names are open strings, not a closed enum: the three built-in lifecycle states
(`open`, `in_progress`, `closed`) and any future custom stage a project adopts are the
same shape to a workflow definition. Bind to a custom stage name today and it works
the moment your ticket data starts using it — no new `lit` release required.

### `events`

Fires when a **semantic event** is dispatched. Commands dispatch events, and
definitions bind to events — never to command names — so `lit` can rename or
restructure a command freely without breaking an authored definition, as long as the
event it dispatches stays the same. See [the event catalog](#event-catalog) below for
the full list and which command dispatches each one today.

## Composition: OR within, AND across

Within one dimension, listed values are alternatives (OR): `labels: [a, b]` fires on
a ticket carrying `a` *or* `b`. Across dimensions, every dimension the definition
declares must be satisfied (AND): a definition with both `labels` and `events` set
fires only when the acted-on ticket carries one of the listed labels **and** the
listed event fired. A dimension left out of the frontmatter entirely constrains
nothing — it's as if that dimension were always satisfied.

## Event catalog

| Event | Fires on (today) |
|---|---|
| `show_backlog` | `lit backlog` |
| `next_pulled` | `lit next` (only when it has a ticket to hand back) |
| `show_ticket` | `lit show` |
| `work_started` | `lit start` |
| `work_finished` | `lit done` |
| `ticket_closed` | `lit close` |
| `ticket_reopened` | `lit open` |
| `ticket_created` | `lit new`, `lit followup` |
| `ticket_updated` | `lit update` |
| `comment_added` | `lit comment add` |

`lit workflows` always lists the full catalog in this order, whether or not any
definition binds to a given event — so you can see everything available, not just
what's already in use. A definition bound to a name outside this catalog still loads
(it may target a newer `lit`) but can never fire here; `lit workflows` flags it under
Warnings as an unknown event.

## The commands

### `lit workflows`

The see-it surface: a static view of the lifecycle spine — every catalog event, the
three built-in states (each with its enter/exit sides) plus any custom stage a loaded
definition binds to, and every label a loaded definition binds to — annotated with
the definitions active at each point and their source layer (project/global/
embedded). A file that's inert, targets an unknown event, or failed to parse never
vanishes silently; it's flagged under a `Warnings` section naming its layer, path, and
reason.

`lit workflows show <id>` prints one definition fully resolved: its frontmatter,
source, path, and body.

### `lit workflows edit <id-or-point>`

Scaffolds a file to customize guidance, then hands it to you. `<id-or-point>` resolves
in one of two ways:

- **An existing definition's id** — override it. If it's already a project-layer
  file, `edit` just opens it (nothing to scaffold — that file already *is* the
  override). If it's currently resolving from the global or embedded layer, `edit`
  copies that definition's exact content into a new file at the same relative path
  under `.lit/workflows/`, ready to customize.
- **Anything else** — treated as a lifecycle *point* with no definition bound there
  yet, in this order:
  1. `<state>:enter` or `<state>:exit` — an explicit state activation.
  2. A name in [the event catalog](#event-catalog) — an event.
  3. One of the three built-in state names, bare (defaults to `enter`).
  4. Anything else — a label.

  A fresh file is scaffolded under `.lit/workflows/`, commented to show every
  activation dimension, with the requested one live and ready to fire.

`edit` never overwrites an existing file. If the target path is already occupied —
by an unrelated definition that happens to share the same default filename — it
fails with a clear conflict message instead of silently clobbering it; edit that file
directly, or pick a different point.

`edit` always prints the scaffolded (or existing) file's path to stdout. When stdout
is a real terminal and `$EDITOR` is set, it additionally opens that file in it. A
script, an agent, a pipe, or a terminal with no `$EDITOR` configured gets just the
printed path — enough to open the file yourself.

### `lit workflows dry-run`

Answers "what would inject on `<event>` for a ticket labeled `<label>`?" without a
real occasion happening. Flags describe a hypothetical occasion:

```text
lit workflows dry-run [--event <name>] [--label <name>]... [--enter <state>] [--exit <state>] [--issue <id>]
```

The output names every definition that would fire, **why** (which specific label,
event, or state activation matched), and previews the body that would be injected
(with `<id>` interpolated from `--issue` when given). A hypothetical that matches
nothing still exits cleanly, reporting `Fired (0)`.

`dry-run` never writes anything — no scaffold, no trace record. It's read-only end to
end.

## Debugging: the firing trace

Every real occasion that matches at least one definition — a real `lit start`, `lit
done`, and so on, not a `dry-run` — records a JSON trace file alongside the automation
traces `lit doctor` already points at (`traces_dir` in its output), under a
`workflows` subdirectory. Each record names the occasion (event, ticket, labels,
transition) and every definition that fired, with the same "why" reasoning
`dry-run` prints. An occasion that matches nothing leaves no trace — the directory
stays proportional to guidance actually injected, not to every `lit` invocation.

## Worked example: override the done guidance

`lit done` ships one embedded default today: a post-close reminder to capture
follow-ups while context is fresh (bound to `events: [work_finished]`, `id: done`).
Say you want your own wording, or you want it to only fire for tickets labeled
`user-facing`.

1. Run `lit workflows edit done`. Since `done` currently resolves from the embedded
   layer, this copies its exact frontmatter and body into
   `.lit/workflows/done.md` and opens it in `$EDITOR` (or prints the path).
2. Edit the body to say what you want. Add `labels: [user-facing]` if you only want
   it for user-facing work — leaving `events: [work_finished]` in place keeps it tied
   to the same close moment, now AND'd with the label.
3. Save. The next `lit done` on a matching ticket injects your text — confirm with
   `lit workflows dry-run --event work_finished --label user-facing` before it
   actually happens, or `lit workflows show done` to see the resolved definition your
   edit produced.
4. `lit workflows` now shows `done` at `work_finished` with `(project)` as its
   source — the override is active, and visible as one, not a mystery divergence
   from what's documented here.

Delete the file to fall back to the embedded default at any time — there is no
"reset" command, because there's nothing to reset: the embedded layer is always
there underneath, and removing the override just stops shadowing it.
