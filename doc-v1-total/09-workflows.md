# Workflows and events

A **workflow** in lit is not automation. It is a markdown file whose body is printed verbatim to the calling command's stdout when a matching lifecycle moment occurs. Nothing is executed: no subprocess, no environment variables passed, no timeouts, no gating — the package answers only "which guidance is in place, and does it apply to this moment" (`internal/workflows/workflows.go`). A reader coming from Jira should map this to injected checklist/guidance text, not to automation rules or webhooks. The one subprocess anywhere in the subsystem is `$EDITOR`, launched by `lit workflows edit` (`internal/cli/workflows_edit.go`).

This document covers the definition-file format, loading and override, the moments a definition fires at (crossings of a ticket's state and the event catalog), matching, dispatch, firing traces, and the `lit workflows` command surface.

## The definition file

A definition is a markdown file with optional YAML frontmatter (`internal/workflows/workflows.go`).

**Frontmatter delimiters** (`internal/workflows/parse.go`): the delimiter is a line equal to `---` after stripping trailing spaces, tabs, and `\r` (so CRLF files work). Frontmatter exists only if the *first line* of the file is a delimiter; otherwise the entire file is body and the definition never fires (see below). An opened-but-unclosed frontmatter block is an error (`unterminated frontmatter: missing closing ---`), which makes the file malformed: it never fires and holds the id its path gives, with one warning (`parse.go`). The body is everything after the closing delimiter, whitespace-trimmed (`parse.go`).

**Frontmatter keys** — lit reads four, `id`, `name`, `when` and `labels`, each optional (`parse.go`); a file carrying any other key is covered under *Keys lit does not read* below. The frontmatter must be a YAML mapping; a top-level sequence or scalar is malformed: the definition never fires, with one `invalid frontmatter: <err>` warning, and holds the id its path gives. A `when` or `labels` written as anything but a list of strings — `labels: 17`, an unbracketed `when: enter closed`, a mapping as a `when:` entry — never fires either, with one warning `invalid frontmatter: %s %v`, `%s` being the key's name (`when:` or `labels:`) and `%v` the YAML error (`invalid frontmatter: when: yaml: unmarshal errors: …`), and holds the id the header declares (`id: done` beside an unbracketed `when:` holds `done`), else the id its path gives (`parse.go`, `parse_test.go`).

| Key | Type | Semantics |
|---|---|---|
| `id` | string | primary key across layers; trimmed; defaults to the layer-relative path with `.md` removed and spaces replaced by `_` (directory separators preserved: `review tasks/design check.md` → `review_tasks/design_check`) (`parse.go`). Lookup is exact string equality, no case folding (`workflows.go`) |
| `name` | string | optional display name, trimmed; used only in listings, rendered as `<id> "<name>" (<source>)` (`internal/cli/workflows.go`) |
| `when` | list of string | the moments the definition fires at (see below) |
| `labels` | list of string | the labels the acted-on ticket must carry one of |

**`when`**: each entry is one moment, read by `ParseMoment` (`parse.go`) — the one reader of the spelling, shared with `lit workflows dry-run --when` and `lit workflows edit --when`. The entry is lowercased and split on whitespace; a first word `enter` or `exit` followed by more words is a `Crossing` of the state the remaining words name, rejoined with single spaces and read through the lifecycle's one gate, `model.ParseStateNode`; anything else is an `Event` of the catalog (`parse.go`). So `Exit  Review` reads as `exit review`, `enter in-progress` as `enter in_progress` and `enter Archived` as `enter archived` (`parse_test.go`). `Moment` is the sealed union of `Event` and `Crossing{Side, State}`, with `Side` either `Enter` or `Exit` and `State` a `model.StateNode`, and `Spellings` renders moments back in this spelling for every display, trace and event fact (`workflows.go`). The states are a closed set, the lifecycle's state nodes, `model.StateNodes()` (`internal/model/lifecycle/state_node.go`): every status, with the phases after `in_progress` (`PhasedState`, the status they nest inside), then every retention — `open`, `in_progress`, `plan`, `impl`, `review`, `merge`, `closed`, `live`, `archived`, `deleted` — each axis listing itself (`States()`, `Phases()`, `Retentions()`), so a state the lifecycle adds is crossable with no edit here. A `StateNode`'s name is unexported, so a node is built only by `StatusNode`, `PhaseNode` or `RetentionNode`, or parsed by `ParseStateNode`, the only string-to-node gate, which reads a name as `ParseState` does: any case, surrounding space ignored, `in-progress` for `in_progress`. `Moments()` is every moment lit fires, the event catalog and then entering and exiting each state node (`workflows.go`). Text naming no moment lit fires is an error saying why: `a blank entry names no moment`, `unknown state %q: a ticket's states are %s` (`%s` being the state nodes joined by `, `, from `ParseStateNode`), or `unknown event %q: the events are %s, and a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])` — so `enter deploy:staging`, `exit in progress` and a lone `enter` are refused (`parse.go`, `state_node.go`, `parse_test.go`). An entry `ParseMoment` refuses never fires: it is dropped with the warning `when: entry %q never fires: %v`, `%v` being that error, and the list's other entries still bind (`parse.go`). `work_started`, `phase_set`, `work_finished`, `ticket_closed` and `ticket_reopened` are unknown events of this kind: each warns and never fires (`parse_test.go`).

**`labels`**: each entry is normalized exactly as stored labels are — lowercased, trimmed, empty rejected, commas forbidden (`internal/model/label.go`). An entry `model.NormalizeLabel` refuses — blank, or comma-bearing — is dropped with the warning `label %q can never match: %v` (`parse.go`). Because both sides are canonical, downstream matching is exact-string yet case-insensitive by construction (`parse.go`).

**Reading a key** (`readKey`, `parse.go`): the frontmatter holds `when` and `labels` as YAML nodes, so a key left out reads apart from one written empty. A key left out is the zero `Key`, which constrains nothing. A written key is a list of entries read by `workflows.Entries(texts, read)` — `ParseMoment` for `when`, `model.NormalizeLabel` for `labels` — which keeps each value once, in the order first written, and returns each text `read` refuses as a `Refusal{Text, Err}`, the warning above for each. So a repeat names nothing the key lacks: `labels: [x, X, ' x ']` is the one label `x` and gives one reason (`parse_test.go`). A key written with no entries — `[]`, or a bare `when:` — warns `when: lists no entry, so the definition never fires` (or `labels: …`), and a definition with a written key left holding no readable entry never fires (below).

**Keys lit does not read**: the frontmatter struct's inline `Unread` map collects every key besides the four, so its fields are the one list of keys lit reads (`parse.go`). A file with any key in `Unread` — `events:`, `states:`, a `where:` written for a newer lit, a differently cased `When:` — yields a definition that holds its id and never fires, whatever its `when` and `labels` say, and a farther layer's definition of that id does not fire in its place (`parse.go`). One warning names the keys sorted and quoted, and the spelling lit reads: `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on` (`unreadKeysMessage`, `parse.go`); its `when` and `labels` entries still warn as they would anywhere.

**Never firing**: a definition's `Activation` is a struct whose zero value never fires; `Activation.Keys()` returns its `Keys` and true, or false for one that never fires, why being in the load's warnings for its file (`Set.WarningsFor`) (`workflows.go`). `Keys{When Key[Moment]; Labels Key[string]}` has at least one key written; a `Key`'s zero value is a key left out, and `Key.Entries()` lists what a key holds. Only `parseDefinition` builds an `Activation` that fires, so a `Definition{}` — `Set.Lookup`'s miss among them — fires on nothing. A definition lit cannot fire on its keys loads, holds its id, appears in listings, and never matches (`workflows.go`, `parse.go`, `match.go`): one declaring neither `when` nor `labels`, which warns `no activation keys (when/labels): the definition never fires`, and is why a plain markdown file with no frontmatter loads but never fires; one carrying a key lit does not read; one with malformed frontmatter; and one writing a key with no entry lit reads — written empty, written as something other than a list, or every entry refused — since that key can never hold and dropping it would widen the definition to the other key alone; the key's warnings say why, and two keys written empty warn once each (`parse.go`, `parse_test.go`).

**Body substitution**: exactly one substitution exists — every literal `<id>` in the body is replaced with the occasion's issue ID at injection time, unconditionally; when the occasion carries no issue ID (e.g. `show_backlog`) it is replaced with the empty string (`dispatch.go`).

The shipped defaults are embedded in the binary (`internal/workflows/defaults/`). Four are bound to entering a phase — `phase-plan.md` (`when: [enter plan]`), `phase-impl.md`, `phase-review.md`, `phase-merge.md` — and each body names the command that records the next move: `lit phase <id> --to impl`, `--to review`, `--to merge`, and from `merge`, `lit done <id>`. The fifth is `done.md`, bound to entering `closed`, which `lit done` and `lit close` both do:

```
---
id: done
name: Post-close capture reminder
when: [enter closed]
---
Ticket <id> has been closed. Before moving on, take a moment to review related tickets. …
```

## Layers, loading, and override

Definitions load from three layers, nearest first (`load.go`):

1. **Project** — `<workspaceRoot>/.lit/workflows/`
2. **Global** — `<config-dir>/workflows/`, where the config dir is `$XDG_CONFIG_HOME/links-issue-tracker` if set, else `$HOME/.config/links-issue-tracker`; if no home dir can be determined the layer contributes nothing (`internal/config/config.go`, `load.go`)
3. **Embedded** — compiled into the binary via `go:embed`; today five files, `done.md` and the four `phase-*.md` (`load.go`)

Within a layer, discovery walks the root recursively; a file participates iff its path ends in `.md` (case-sensitive — `.MD` does not); the folder hierarchy carries no activation meaning and only seeds the default ID; walk order is lexical, making within-layer conflicts deterministic (`load.go`).

**Load can never fail** — `Load` returns a Set and no error; every problem becomes a `{source, path, message}` warning, and every `*.md` file still yields a definition while its siblings load as usual (`load.go`, `workflows.go`). A file lit cannot read never fires, under the id its path gives (`readDefinition`, `load.go`), and a malformed one never fires, under the id it declares or else its path's (`parse.go`), so either still holds its id and a farther layer's definition of that id does not fire in its place. The complete warning conditions:

| Condition | Message |
|---|---|
| absent layer root | none — genuine absence, not failure (`load.go`) |
| any other walk error, or file read error | `cannot read: <err>` |
| unterminated frontmatter | `unterminated frontmatter: missing closing ---` |
| YAML unmarshal failure of the header | `invalid frontmatter: <err>` |
| `when` or `labels` not a list of strings | `invalid frontmatter: %s %v`, the key's name and the YAML error |
| `when` or `labels` written with no entries | `<key> lists no entry, so the definition never fires`, `<key>` being `when:` or `labels:` |
| blank or comma-bearing label | `label %q can never match: <err>` |
| key lit does not read | `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on` |
| neither key declared | `no activation keys (when/labels): the definition never fires` |
| `when:` entry naming no moment lit fires | `when: entry %q never fires: <err>`, the error `ParseMoment` gave (see `when` above) |
| duplicate ID within one layer | `duplicate id %q (already defined by %s): file ignored` |

**Override semantics** (`load.go`): within a layer, first file in lexical order claims an ID and later claimants are ignored with a warning. Across layers, the nearest layer to claim an ID wins outright and farther layers' definitions with that ID are skipped *silently* — that is the override feature. Override replaces wholly, never merges fields: a project `id: done` file replaces the embedded default including its body. Because the default ID is the layer-relative path, a file at the same relative path in a nearer layer overrides without an explicit `id`. The resolved set is sorted by ID ascending, and warnings accumulate from all layers, including overridden ones.

## The event catalog

Six events, defined as string constants (`internal/workflows/events.go`). An event names a read moment: lit showing or recording something that changes no ticket's state. A change of state is never an event; it reaches definitions as the crossings it makes (see the occasion payload below), whichever command makes it (`events.go`). Event names are a stable contract deliberately decoupled from command names — commands may be renamed; the event name is what definitions bind to (`events.go`). Names are contractually lowercase snake_case, and each reads back through `ParseMoment` as itself, never as a crossing (pinned by test).

| Wire name | Fires when | Command |
|---|---|---|
| `show_backlog` | agent views the workable backlog | `lit backlog` |
| `show_ticket` | agent views one ticket's details | `lit show` |
| `next_pulled` | agent asks for the next workable ticket | `lit next` |
| `ticket_created` | a new ticket is created | `lit new`, `lit followup` |
| `ticket_updated` | an existing ticket's fields change | `lit update` |
| `comment_added` | a comment lands on a ticket | `lit comment add`, `lit bulk comment` |

`Catalog()` returns display order — `show_backlog, next_pulled, show_ticket, ticket_created, ticket_updated, comment_added` — which differs from declaration order and is the order `lit workflows` prints (`events.go`).

### The occasion payload

The single payload type is `Occasion` (`match.go`): `Moments` (what happened — a read's or an edit's one event, a creation's event and the states the new ticket starts in, or every crossing a transition made; empty when a transition left the ticket where it was), `IssueID` (never read by matching — used only for `<id>` interpolation, display, tracing), `Labels` (canonical form; nil when no single ticket). Payloads as actually built (`internal/cli/workflow_events.go`):

| Occasion | Payload set |
|---|---|
| `show_ticket`, `next_pulled`, `ticket_updated`, `comment_added` (`ticketOccasion`) | Moments = the one event, IssueID, Labels; `ticket_updated` never carries a crossing — `lit update` rejects `--status` (`workflow_events.go`) |
| `show_backlog` (`backlogOccasion`) | Moments = the one event — no IssueID, no Labels |
| `ticket_created` (`createdOccasion`) | Moments = `ticket_created`, then the entries into the new ticket's configuration from nothing — `enter open`, `enter live` — IssueID, Labels |
| every transition (`transitionOccasion`) | Moments = the crossings, IssueID, post-transition Labels. Each side is the ticket's configuration, `model.ConfigurationOf` (`internal/model/phase.go`): its status, then the phase `PhaseOf` reads while it has one, then its retention. `crossings(from, to)` makes the moves in the order a statechart makes them: `exit <s>` for each node before that is not after, innermost first, then `enter <s>` for each node after that was not before, outermost first (`workflow_events.go`) |

So `lit start` from open makes `exit open, enter in_progress, enter plan`; `lit done` from `review` makes `exit review, exit in_progress, enter closed`; `lit close` from open makes `exit open, enter closed`, and from `impl` `exit impl, exit in_progress, enter closed`; `lit open` makes `exit closed, enter open`; `lit phase --to review` from `impl` makes `exit impl, enter review`, never touching the status, which the move leaves at `in_progress`. The retention verbs cross retentions and never the status: `lit archive` makes `exit live, enter archived`, `lit unarchive` `exit archived, enter live`, `lit delete` `exit live, enter deleted` (from archived, `exit archived, enter deleted`), and `lit restore` `exit deleted, enter live`. `lit done` and `lit close` both make `enter closed`, and no definition can tell the two commands apart. A call that leaves the ticket where it was — a takeover or reclaim `start` of a ticket already in progress, a move to the current phase — crosses nothing, so only a definition bound by labels alone fires on it (`workflow_events.go`). Both sides are read on the write store, before and after `Apply` (`cli.go`).

### Dispatch call sites

Where each occasion actually fires, and where in the command's output the injected guidance lands:

| Site | Moments | Position |
|---|---|---|
| `lit new` (`cli.go`), `lit followup` (`cli.go`) | `ticket_created`, `enter open`, `enter live` | after create succeeds, before the issue summary and breadcrumb |
| `lit show` (`cli.go`) | `show_ticket` | after detail load, before either `--field` output or the full view — fires for both |
| `lit update` (`cli.go`) | `ticket_updated` | after apply, before summary |
| transitions: `applyTransition` (`cli.go`), run by every transition verb — `lit start`, `lit phase`, `lit done`, `lit close`, `lit open`, `lit archive`, `lit unarchive`, `lit delete`, `lit restore` — and every `lit bulk` transition (`bulk.go`) | the crossings the transition made | after authorize and apply, collected after the transition's own notice (a claim hand-off, a phase move) into `transitionDone.said`; the single verb prints that before the issue summary, and `lit bulk` prints it ahead of the item's `<id> ok` line |
| comments: `addComment` (`cli.go`), run by `lit comment add` and, per ticket, `lit bulk comment` (`bulk.go`) | `comment_added` | after the comment is stored, before its `<issueID> <commentID>` line; `lit bulk` prints both ahead of the item's `<id> ok` line |
| `lit next` (`internal/cli/next.go`) | `next_pulled` | last, after the start advice and summary; only when a row was actually served — exhausted/no-work paths return before any occasion is built |
| `lit backlog` (`internal/cli/workable.go`) | `show_backlog` | last, after the table render |

## Matching

`Definition.Matches(occasion)` (`match.go`) is `len(MatchReasons(occasion)) > 0`. `MatchReasons` returns nil for a definition that never fires (`Activation.Keys()` false), and otherwise its `Keys.reasons`, which asks each key `Key.carried`: the key's entries the occasion carries, in listed order, and whether the key holds — a key left out holds on anything, a written one when the occasion carries one of its entries. The two keys combine with AND, while values within one key combine with OR.

- **When** (`match.go`): left out → unconstrained; otherwise at least one of the definition's moments must be among the occasion's `Moments`, compared by equality — a crossing is its side and state node, an event its name. So a `when:`-bound definition never fires on an occasion that carries no moment, such as a transition that crossed nothing.
- **Labels** (`match.go`): left out → unconstrained; otherwise at least one bound label must appear among the occasion's labels (OR — there is no way to require ALL labels). Exact string comparison; both sides are pre-canonicalized.

There are **no wildcards, globs, regexes, or negation** anywhere in matching; the only "match everything" construct is omitting a key. There is **no precedence between simultaneous matches** — every matching definition fires, in ID-ascending order (`match.go`). The only precedence in the system is the load-time layer/ID override, which guarantees at most one definition per ID.

**Match reasons** (`match.go`): for a matching definition, a fixed-order list of strings explains why — `when:<moment>` for each of the definition's `when:` moments the occasion carries, in the definition's order and in the canonical `when:` spelling, then `label:<l>` for each declared label that overlapped. This one computation backs both the firing trace and dry-run.

## Firing mechanics

`Fire(set, ws, occasion) Firing` (`dispatch.go`) is synchronous and in-process — no bus, no queue, no subscriber registry. Behaviors:

- It matches against a Set the command already loaded. A firing command loads the definitions from disk once, through `loadWorkflows` (`internal/cli/workflow_dispatch.go`), and a bulk command fires every item's occasion against that one load; there is no caching across commands. A transition, `lit comment add` and a bulk command load before their write, so their warnings print even when the write is refused.
- `loadWorkflows` prints every load warning to the command's stderr, one line apiece as `lit: workflow %s` with the warning in place of `%s`, in `WarningsByLayer` order, once per command, so a file lit cannot read or cannot fully fire is announced by every firing command and by `lit workflows dry-run`, not only by the overview. `Warning.String` is the one rendering, `<source> <path>: <message>` with the message's own newlines collapsed, shared with `lit workflows` (`workflows.go`). Warnings never go to stdout, where they would read as guidance. The same lines are the command event's `workflows.warnings`, `[]` when there were none.
- It returns a `Firing{Fired, Guidance, Untraced}`: the definitions that fired, in ID order; the guidance, each one's interpolated body plus a newline; and why the firing trace was not recorded, nil when it was or when nothing fired. The command prints the guidance to stdout where its output puts it (see the call sites above): `dispatchOccasion` writes it for a read, `lit update` and a creation, a transition returns it in `transitionDone.said`, and `addComment` returns it beside the comment.
- Its one effect is the firing trace. A trace it cannot record never fails the command — the definitions fired all the same — and comes back as `Untraced`, which `loadedWorkflows.fire` prints on the command's stderr as `lit: workflow firing trace could not be recorded (%v)` (`dispatch.go`, `internal/cli/workflow_dispatch.go`).
- `Fire` returns no error, so the only error on the path is the command's own write of the guidance to stdout; in practice a workflow can never fail a command, and a malformed file degrades to a load warning rather than breaking any invocation.
- Every firing goes through `loadedWorkflows.fire` (`internal/cli/workflow_dispatch.go`), which records on the event `workflows.when` (the occasion's moments in their `when:` spelling) and `workflows.fired` (the fired definitions' ids), each `[]` when empty, and `workflows.trace_error` (the trace failure's text, null when there was none). A `lit bulk` transition and `lit bulk comment` record them on each item, under `bulk.item.facts`, and `workflows.warnings` once, on the command.
- Exit codes are unaffected by workflow firing. The general CLI mapping is the exit-code table in `06-issue-commands.md`, which is where it is maintained; restating it here is what let this line drift to a set missing exit 6.

## Firing traces

A JSON trace is written only when at least one definition fired — unmatched occasions leave no trace (`dispatch.go`). `trace.Write` refuses a storage dir that is not an absolute path, `write %s trace: storage dir %q is not an absolute path`, so a record never lands relative to the working directory, and `Fire` reports the refusal like any trace it cannot record (`internal/trace/trace.go`, `dispatch.go`). Traces land in `<StorageDir>/traces/workflows/` where StorageDir is `<git-common-dir>/links` (`internal/trace/trace.go`, `internal/workspace/workspace.go`).

Filename: `<UTC 20060102T150405.000000000Z>-<slug>.json`, where the slug is the occasion's moments in their `when:` spelling, joined with spaces, lowercased with runs of non-`[a-z0-9]` replaced by `-` (so `exit review enter closed` → `exit-review-enter-closed`), falling back to `trace` for an occasion with no moments — a labels-only definition firing on a transition that crossed nothing (`internal/workflows/trace.go`, `internal/trace/trace.go`). Files are created `O_WRONLY|O_CREATE|O_EXCL` mode 0644 (dirs 0755); on collision up to 5 attempts with a fresh timestamp and an `-<attempt>` suffix; exhaustion yields `create workflows trace: too many id collisions` (`internal/trace/trace.go`).

Record shape (`internal/workflows/trace.go`), marshaled indented with a trailing newline; `issue_id` and `labels` are omitempty, while `when` (the occasion's moments in their `when:` spelling, `[]` for one that crossed nothing) and `fired` are not; `recorded_at` is RFC3339Nano:

```json
{
  "id": "<filename stem>",
  "recorded_at": "<RFC3339Nano>",
  "workspace_id": "<workspace id>",
  "when": ["show_ticket"],
  "issue_id": "lit-42",
  "labels": ["needs-design"],
  "fired": [
    { "id": "needs-design-note", "source": "project", "path": "needs-design.md",
      "reasons": ["when:show_ticket", "label:needs-design"] }
  ]
}
```

## The `lit workflows` command

Registered as a workspace-only command — it resolves a workspace from the working directory but never opens the store (`internal/cli/register.go`). Usage:

```
lit workflows [show <id> | edit <id> | edit [--when <moment>]... [--label <name>]... | dry-run [--when <moment>]... [--label <name>]... [--issue <id>]]
```

Routing: no positionals → overview; `show <id>`; `edit <id>`, or `edit` with `--when`/`--label`; `dry-run`; anything else is a usage error (exit 2) (`internal/cli/workflows.go`). Overview and show take no flags — any flag-shaped token there is a usage error.

**Overview** (`workflows.go`) prints, in order: the header `lit workflows — work lifecycle guidance (project > global > embedded)`; a `When` section listing every moment a `when:` entry can name, in that spelling (`workflows.Moments()`) — the full event catalog in catalog order, bound or not, then `enter <state>` and `exit <state>` for each state node in `model.StateNodes()` order (`open`, `in_progress`, the four phases in work order, `closed`, then `live`, `archived`, `deleted`); a `Labels` section (every label a definition that fires binds — not every label used on tickets — deduped and sorted, or `(none bound)`); then warnings. Each point shows the bound definitions as `<id> "<name>" (<source>)` refs, least specific layer first (embedded, then global, then project; `DefinitionsByLayer` in `workflows.go`), and a definition appears at every point one of its keys lists (`boundAt`); one that never fires is bound nowhere. Firing does not use this order: `Matching` keeps the ID order of `Set.Definitions`, so injected guidance is unchanged. The warnings section, printed only when non-empty, is headed `Warnings (loaded but not fully active)` in the same layer order (`WarningsByLayer`) with each warning on one line as `Warning.String` renders it.

**`show <id>`** prints exactly `id:`, `name:` (`-` when empty), `source:`, `path:`, then, for a definition that fires, `when:` (the entries in their `when:` spelling, comma-separated) and `labels:` — each `-` when its key is left out — or, for one that never fires, the one line `fires: never`; then the load's warnings for that definition's file (`Set.WarningsFor`) under the overview's `Warnings (loaded but not fully active)` heading when there are any, a `---` separator, then the body (`workflows.go`). Unknown ID → validation error naming the id, exit 3; missing ID → usage error.

**`dry-run`** builds a hypothetical occasion from repeatable `--when`, repeatable `--label`, and `--issue` (`workflows_dryrun.go`). The flags are read by `readKeyFlags` (`workflows.go`), as `edit`'s are and as a definition's own keys read their entries: through `workflows.Entries`, so a repeated value counts once (`--label Foo --label foo --when show_ticket --when SHOW_TICKET` is `when=show_ticket labels=foo`). Each `--when` is read by `ParseMoment`, so it is spelled and canonicalized as a `when:` entry is (`--when "Enter  Review"` is `enter review`), and a start from open is asked as `--when "exit open" --when "enter in_progress" --when "enter plan"`; the first value `ParseMoment` refuses, blank included, is refused with a validation error, exit 3, carrying `ParseMoment`'s reason: `--when %q is not a moment lit fires: %v`. Each `--label` is read by `model.NormalizeLabel`, trimmed and lowercased as a stored label is, so `--label Needs-Design` matches a definition bound to `needs-design`, and the first blank or comma-bearing one is refused, exit 3: `--label %q is not a label a ticket can carry: %v`; `--when` is checked before `--label` (`workflows.go`). Dry-run loads the definitions as a firing command does, through `loadWorkflows`, so each load warning prints on stderr as `lit: workflow <warning>` and lands in the event's `workflows.warnings` (`workflows_dryrun.go`). Output: an `occasion:` line — `when=` the moments' spellings joined by `,`, then `labels=` joined by `,`, then `issue=`, each `-` when unset — then `Fired (<n>)` with `(none)` or, per match, the definition ref, its match reasons, and its interpolated body indented 4 spaces. Dry-run never writes a firing trace and never reads the store (`workflows_dryrun.go`). Stray positionals and unknown flags are usage errors.

## `lit workflows edit` — overriding and scaffolding

`edit` takes a definition's id or the keys of a new definition, never both (`workflowsEditLeaf`, `workflows.go`): one positional and no flag opens or overrides that definition; no positional and at least one `--when` or `--label` scaffolds a new one; an id with flags, or neither, is a usage error (exit 2). The positional only ever names a definition, so no text is read two ways.

**`edit <id>`** (`runWorkflowsEdit`, `workflows_edit.go`): if the definition's source is `project`, the file is the definition itself — it is printed/opened directly. If its source is `global` or `embedded`, the raw default bytes are copied verbatim to `<RootDir>/.lit/workflows/<path>` as the override (refusing if that path exists — see no-clobber), with the notice `scaffolded override for "<id>" (was <source>) -> <path>`, then opened (`workflows_edit.go`). Raw-default read failures surface as `read global workflow default <path>: <err>` / `read embedded workflow default <path>: <err>`; asking for a raw default from the `project` layer is an error by construction — `workflows: no raw default for the project layer (it is the override target, not a source to copy from)` (`scaffold.go`). An id nothing loaded holds is a validation error, exit 3, naming the flags that scaffold a new definition, and nothing is written: ``no workflow definition with id %q (run `lit workflows` to see loaded ids); to scaffold a new one, name when it fires: `lit workflows edit --when "enter closed"`, `--label needs-design`, or both``.

**`edit --when <moment>... --label <name>...`** (`editFreshDefinition`, `workflows_edit.go`) scaffolds a new project-layer definition firing on exactly the keys the flags name. The flags are read by `readKeyFlags`, as `dry-run`'s are, so the first value naming no moment lit fires or no label a ticket can carry is refused, exit 3, with `--when %q is not a moment lit fires: %v` or `--label %q is not a label a ticket can carry: %v`, and nothing is written — `--when "enter reveiw"`, `--when work_finished`, `--when "exit in progress"`, `--label a,b` and a blank `--label` among them. A `--label` is a label whatever it reads like: `--label "exit plan"` binds the label `exit plan`.

The file name is `scaffoldName(when, labels)` plus `.md`: each moment's canonical spelling, then each canonical label, slugged — runs outside `[a-z0-9_-]` replaced with `-` (underscore deliberately kept, so `show_ticket` → `show_ticket.md`), edge dashes trimmed, `definition` when nothing is left — and joined by `+` (`workflows_edit.go`). The name comes from what the file binds, so two spellings of one binding name one file and the second scaffold is refused as a conflict (exit 5). Pinned examples: `--when "enter review"` → `enter-review.md`, `--when "Enter  In_Progress"` → `enter-in_progress.md`, `--when "exit in-progress"` → `exit-in_progress.md`, `--when "enter archived" --when "exit live"` → `enter-archived+exit-live.md`, `--label Needs-Design` → `needs-design.md`, `--label "exit plan"` → `exit-plan.md`, `--when "enter closed" --label needs-design` → `enter-closed+needs-design.md`. stdout says `scaffolded a new definition at %s`, then prints the path (`workflows_edit.go`).

The fresh scaffold's content is `ScaffoldFresh(when, labels)` (`scaffold.go`): frontmatter opening `---`; four comment lines explaining that `when:` lists the moments and `labels:` narrows to tickets carrying a label, OR within a key and AND across, and pointing at `lit workflows --help`; the `when:` line, then the `labels:` line, each written by `keyLine` — a key the edit names live with its entries double-quoted by `strconv.Quote` (`when: ["enter closed"]`, `labels: ["a", "b"]`), a key it leaves out commented as `# when: ` followed by `[enter review, exit plan, show_ticket]  # entering or exiting a state, or an event`, or `# labels: ` followed by `[needs-design, blocked]`; commented `# id:` and `# name:` hints; closing `---`; and the body placeholder ``Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.`` A fresh scaffold loads binding exactly its keys, with no warning.

**No-clobber**: scaffolding refuses to overwrite in two stages — a stat check returning a merge-conflict error (exit 5) with ``cannot scaffold "<subject>": <path> already exists (edit it directly, or run `lit workflows` to see what's already loaded there)``, and the real enforcer, an `O_CREATE|O_EXCL` open that converts `IsExist` to the same error, closing the check-then-write race (`workflows_edit.go`). Scaffolding only ever writes under the project `.lit/workflows/`, never the global or embedded layers.

**Opening the file**: the path is always printed to stdout first. Unless stdin and stdout are both terminals, that is all (`interactive`, `internal/cli/interactive.go`). Otherwise `$EDITOR` is split on whitespace (so `EDITOR="code -w"` works as command plus args); an empty `$EDITOR` means print-only; otherwise the editor runs synchronously with the process's real stdin/stdout/stderr, and a non-zero exit becomes the error `open <path> in $EDITOR (<cmd>): <err>` (`workflows_edit.go`). The only environment variables the subsystem reads are `$EDITOR` here and `$XDG_CONFIG_HOME`/`$HOME` via the config dir.

## Relationship to `lit hooks`

None. `internal/cli/hooks.go` installs a git `pre-push` hook — a managed section of `<git-common-dir>/hooks/pre-push` bounded by `# --- BEGIN LIT INTEGRATION ---`/`END` markers (migrating legacy `LINKS INTEGRATION` markers), refusing to manage a hook whose first line is not a bash shebang (`hooks.go`). It imports nothing from the workflows package, and no workflow event fires on `lit hooks install`.

## What a workflow author can and cannot do

**Can**: bind on `when`, `labels`, or both; bind entering and exiting a state independently; nest files arbitrarily; override a farther layer by ID or by matching relative path; use `<id>` in the body; list an event a newer lit fires (the entry warns and never fires, and the rest of the list still binds).

**Cannot**: run anything; receive environment variables; gate or block a command; set an exit code; bind a state the lifecycle does not have; use wildcards, globs, regexes, or negation; order or prioritize between simultaneous matches (all fire, ID-ascending); require ALL of several labels (label matching is OR); match on issue ID; tell which command made a crossing (`lit done` and `lit close` both make `enter closed`); fire a `when:`-bound definition on a transition that crossed nothing; or have a file fire while it carries a key this lit does not read.
