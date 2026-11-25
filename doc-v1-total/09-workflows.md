# Workflows and events

A **workflow** in lit is not automation. It is a markdown file whose body is printed verbatim to the calling command's stdout when a matching lifecycle moment occurs. Nothing is executed: no subprocess, no environment variables passed, no timeouts, no gating — the package answers only "which guidance is in place, and does it apply to this moment" (`internal/workflows/workflows.go`). A reader coming from Jira should map this to injected checklist/guidance text, not to automation rules or webhooks. The one subprocess anywhere in the subsystem is `$EDITOR`, launched by `lit workflows edit` (`internal/cli/workflows_edit.go`).

This document covers the definition-file format, loading and override, the moments a definition fires at (crossings of a ticket's state and the event catalog), matching, dispatch, firing traces, and the `lit workflows` command surface.

## The definition file

A definition is a markdown file with optional YAML frontmatter (`internal/workflows/workflows.go`).

**Frontmatter delimiters** (`internal/workflows/parse.go`): the delimiter is a line equal to `---` after stripping trailing spaces, tabs, and `\r` (so CRLF files work). Frontmatter exists only if the *first line* of the file is a delimiter; otherwise the entire file is body and the definition is inert (see below). An opened-but-unclosed frontmatter block is an error (`unterminated frontmatter: missing closing ---`), which makes the file malformed: skipped entirely, one warning (`parse.go`). The body is everything after the closing delimiter, whitespace-trimmed (`parse.go`).

**Frontmatter keys** — lit reads four, `id`, `name`, `when` and `labels`, each optional (`parse.go`); a file carrying any other key is covered under *Keys lit does not read* below. The frontmatter must be a YAML mapping; a top-level sequence or scalar — or a non-sequence value for a sequence-typed key, like `labels: 17` or an unbracketed `when: enter closed`, or a mapping as a `when:` entry — is malformed and skips the file (`parse.go`, `parse_test.go`).

| Key | Type | Semantics |
|---|---|---|
| `id` | string | primary key across layers; trimmed; defaults to the layer-relative path with `.md` removed and spaces replaced by `_` (directory separators preserved: `review tasks/design check.md` → `review_tasks/design_check`) (`parse.go`). Lookup is exact string equality, no case folding (`workflows.go`) |
| `name` | string | optional display name, trimmed; used only in listings, rendered as `<id> "<name>" (<source>)` (`internal/cli/workflows.go`) |
| `when` | list of string | the moments the definition fires at (see below) |
| `labels` | list of string | the labels the acted-on ticket must carry one of |

**`when`**: each entry is one moment, read by `ParseMoment` (`parse.go`) — the one reader of the spelling, shared with `lit workflows dry-run --when` and `lit workflows edit`. The entry is lowercased and split on whitespace; a first word `enter` or `exit` followed by more words is a `Crossing` of the state the remaining words name, rejoined with single spaces; anything else is an `Event` (`parse.go`). Blank entries are dropped (`parse.go`). So `Exit  Review` reads as `exit review`, `enter deploy:staging` crosses the state `deploy:staging`, and a lone `enter` is an event named `enter` (`parse_test.go`). `Moment` is the sealed union of `Event` and `Crossing{Side, State}`, with `Side` either `Enter` or `Exit`, and `Spellings` renders moments back in this spelling for every display, trace and event fact (`workflows.go`). State names are open strings, deliberately not a closed enum — custom stage names (which may contain colons, commas or spaces) are legal with no code change (`workflows.go`); the built-in states are `open`, `in_progress`, `closed`, and the phases `plan`, `impl`, `review`, `merge`, which are states inside `in_progress` (`internal/cli/workflows.go`). Every crossing is a moment lit can produce. An event not in this binary's catalog still loads, preserved as authored, with the warning `unknown event %q: not in this lit's catalog, will never fire here; a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])`, and the list's other entries still bind (`parse.go`). `work_started`, `phase_set`, `work_finished`, `ticket_closed` and `ticket_reopened` are unknown events of this kind: each loads, warns, and never fires (`parse_test.go`).

**`labels`**: each entry is normalized exactly as stored labels are — lowercased, trimmed, empty rejected, commas forbidden (`internal/model/label.go`). Entries that trim to empty are silently dropped; a comma-bearing entry is dropped with the warning `label %q can never match: <err>` (`parse.go`). Because both sides are canonical, downstream matching is exact-string yet case-insensitive by construction (`parse.go`).

**Keys lit does not read**: the frontmatter struct's inline `Unread` map collects every key besides the four, so its fields are the one list of keys lit reads (`parse.go`). A file with any key in `Unread` — `events:`, `states:`, a `where:` written for a newer lit, a differently cased `When:` — yields a definition that holds its id and reads neither `when` nor `labels`: it never fires, and a farther layer's definition of that id does not fire in its place (`parse.go`). It gets exactly one warning, naming the keys sorted and quoted, and the spelling lit reads: `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on` (`unreadKeysMessage`, `parse.go`).

**Inertness**: a definition with neither `when` nor `labels` is inert — it loads, appears in listings, warns `no activation keys (when/labels): definition is inert and will never fire`, and never matches (`workflows.go`, `parse.go`, `match.go`). This is why a plain markdown file with no frontmatter loads but never fires. A file carrying a key lit does not read is inert as well, with its unread-key warning in place of this one (`parse.go`).

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

**Load can never fail** — `Load` returns a Set and no error; every problem becomes a `{source, path, message}` warning and the file is skipped while its siblings still load (`load.go`, `workflows.go`). The complete warning conditions:

| Condition | Message |
|---|---|
| absent layer root | none — genuine absence, not failure (`load.go`) |
| any other walk error, or file read error | `cannot read: <err>` |
| unterminated frontmatter | `unterminated frontmatter: missing closing ---` |
| YAML unmarshal failure | `invalid frontmatter: <err>` |
| comma-bearing label | `label %q can never match: <err>` |
| key lit does not read | `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on` |
| inert definition | `no activation keys (when/labels): definition is inert and will never fire` |
| unknown event | `unknown event %q: not in this lit's catalog, will never fire here; a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])` |
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
| `comment_added` | a comment lands on a ticket | `lit comment` |

`Catalog()` returns display order — `show_backlog, next_pulled, show_ticket, ticket_created, ticket_updated, comment_added` — which differs from declaration order and is the order `lit workflows` prints (`events.go`).

### The occasion payload

The single payload type is `Occasion` (`match.go`): `Moments` (what happened — a read's one event, or every crossing a transition made; empty when a transition left the ticket where it was), `IssueID` (never read by matching — used only for `<id>` interpolation, display, tracing), `Labels` (canonical form; nil when no single ticket). Payloads as actually built (`internal/cli/workflow_events.go`):

| Occasion | Payload set |
|---|---|
| `show_ticket`, `next_pulled`, `ticket_created`, `comment_added` | Moments = the one event, IssueID, Labels |
| `show_backlog` | Moments = the one event — no IssueID, no Labels |
| `ticket_updated` | Moments = the one event, IssueID, Labels; never a crossing — `lit update` rejects `--status` (`workflow_events.go`) |
| any lifecycle transition (`transitionOccasion`) | Moments = the crossings, IssueID, post-transition Labels. Over the states the ticket is in on each side — its status, then its phase while in progress — `exit <s>` for each state before that is not a state after, then `enter <s>` for each state after that was not a state before, each outermost first (`crossings`, `workflow_events.go`) |

So `lit start` from open makes `exit open, enter in_progress, enter plan`; `lit done` from `review` makes `exit in_progress, exit review, enter closed`; `lit close` from open makes `exit open, enter closed`; `lit open` makes `exit closed, enter open`; `lit phase --to review` from `impl` makes `exit impl, enter review`, never touching the status, which the move leaves at `in_progress`. `lit done` and `lit close` both make `enter closed`, and no definition can tell the two commands apart. A call that leaves the ticket where it was — a reclaim, a move to the current phase — crosses nothing, so only a definition bound by labels alone fires on it. Both sides are read on the write store, the phase through `model.PhaseOf`, before and after `Apply` (`cli.go`). Retention actions (archive/unarchive/delete/restore) fire **no occasion at all** (`internal/cli/cli.go`, `workflow_events.go`).

### Dispatch call sites

Where each occasion actually fires, and where in the command's output the injected guidance lands:

| Site | Moments | Position |
|---|---|---|
| `lit new` (`cli.go`), `lit followup` (`cli.go`) | `ticket_created` | after create succeeds, before the issue summary and breadcrumb |
| `lit show` (`cli.go`) | `show_ticket` | after detail load, before either `--field` output or the full view — fires for both |
| `lit update` (`cli.go`) | `ticket_updated` | after apply, before summary |
| transitions: `applyTransition` (`cli.go`), run by `lit start`, `lit phase`, `lit done`, `lit close`, `lit open`, and `lit bulk start`/`done`/`close`/`open` (`bulk.go`) | the crossings the transition made | after authorize and apply, collected after the transition's own notice (a claim hand-off, a phase move) into `transitionDone.said`; the single verb prints that before the issue summary, and `lit bulk` prints it ahead of the item's `<id> ok` line |
| `lit comment` (`cli.go`) | `comment_added` | after the comment is stored, before it is printed |
| `lit next` (`internal/cli/next.go`) | `next_pulled` | last, after the start advice and summary; only when a row was actually served — exhausted/no-work paths return before any occasion is built |
| `lit backlog` (`internal/cli/workable.go`) | `show_backlog` | last, after the table render |

## Matching

`Definition.Matches(occasion)` (`match.go`): inert → false; otherwise `when` and `labels` are each tested by the one test `matchAny` and combined with AND, while values within one key combine with OR. An undeclared key constrains nothing.

- **When** (`match.go`): empty → unconstrained; otherwise at least one of the definition's moments must be among the occasion's `Moments`, compared by equality — a crossing is its side and state, an event its name. So a `when:`-bound definition never fires on an occasion that carries no moment, such as a transition that crossed nothing.
- **Labels** (`match.go`): empty → unconstrained; otherwise at least one bound label must appear among the occasion's labels (OR — there is no way to require ALL labels). Exact string comparison; both sides are pre-canonicalized.

There are **no wildcards, globs, regexes, or negation** anywhere in matching; the only "match everything" construct is omitting a key. There is **no precedence between simultaneous matches** — every matching definition fires, in ID-ascending order (`match.go`). The only precedence in the system is the load-time layer/ID override, which guarantees at most one definition per ID.

**Match reasons** (`match.go`): for a matching definition, a fixed-order list of strings explains why — `when:<moment>` for each of the definition's `when:` moments the occasion carries, in the definition's order and in the canonical `when:` spelling, then `label:<l>` for each declared label that overlapped. This one computation backs both the firing trace and dry-run.

## Dispatch mechanics

`Dispatch(env, w, errOut, ws, occasion)` (`dispatch.go`) is synchronous and in-process — no bus, no queue, no subscriber registry. Behaviors:

- It **re-loads the full definition set from disk on every call**; there is no caching (`dispatch.go`).
- Every load warning goes to `errOut` — the command's stderr at every call site — one line apiece as `lit: workflow %s` with the warning in place of `%s`, in `WarningsByLayer` order, on every call, so a file lit cannot read or cannot fully fire is announced by every dispatching command, not only by `lit workflows` (`dispatch.go`). `Warning.String` is the one rendering, `<source> <path>: <message>` with the message's own newlines collapsed, shared with `lit workflows` (`workflows.go`). Warnings never go to `w`, where they would read as guidance.
- For each match it writes the interpolated body plus a newline to `w` — the calling command's own stdout, or for a transition the `transitionDone.said` buffer the command then prints (see the call sites above).
- A write failure aborts and propagates as the command's error; since that is the only error path, in practice a workflow can never fail a command, and a malformed file degrades to a load warning rather than breaking any invocation.
- A trace-write failure never fails dispatch: the guidance was already written, and the failure goes to stderr as `lit: workflow firing trace could not be recorded (<err>); guidance was still injected` (`dispatch.go`).
- It returns the definitions that fired and the load's warnings, with a write error too. Every CLI call site reaches it through `dispatchOccasion` (`internal/cli/workflow_dispatch.go`), which records on the command's event `workflows.when` (the occasion's moments in their `when:` spelling), `workflows.fired` (the fired definitions' ids) and `workflows.warnings` (each warning's `Warning.String` line), each `[]` when empty. A `lit bulk` transition records them on each item, under `bulk.item.facts`.
- Exit codes are unaffected by workflow firing. The general CLI mapping is the exit-code table in `06-issue-commands.md`, which is where it is maintained; restating it here is what let this line drift to a set missing exit 6.

## Firing traces

A JSON trace is written only when at least one definition fired — unmatched occasions leave no trace (`dispatch.go`). Recording is skipped outright when the workspace storage dir is not an absolute path (`dispatch.go`). Traces land in `<StorageDir>/traces/workflows/` where StorageDir is `<git-common-dir>/links` (`internal/trace/trace.go`, `internal/workspace/workspace.go`).

Filename: `<UTC 20060102T150405.000000000Z>-<slug>.json`, where the slug is the occasion's moments in their `when:` spelling, joined with spaces, lowercased with runs of non-`[a-z0-9]` replaced by `-` (so `exit review enter closed` → `exit-review-enter-closed`), falling back to `trace` for an occasion with no moments — a labels-only definition firing on a transition that crossed nothing (`internal/workflows/trace.go`, `internal/trace/trace.go`). Files are created `O_WRONLY|O_CREATE|O_EXCL` mode 0644 (dirs 0755); on collision up to 5 attempts with a fresh timestamp and an `-<attempt>` suffix; exhaustion yields `create workflows trace: too many id collisions` (`internal/trace/trace.go`).

Record shape (`internal/workflows/trace.go`), marshaled indented with a trailing newline; `when` (the occasion's moments in their `when:` spelling), `issue_id` and `labels` are omitempty, `fired` is not; `recorded_at` is RFC3339Nano:

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
lit workflows [show <id> | edit <id-or-point> | dry-run [--when <moment>]... [--label <name>]... [--issue <id>]]
```

Routing: no positionals → overview; `show <id>`; `edit <id-or-point>`; `dry-run`; anything else is a usage error (exit 2) (`internal/cli/workflows.go`). Overview, show, and edit take no flags — any flag-shaped token there is a usage error.

**Overview** (`workflows.go`) prints, in order: the header `lit workflows — work lifecycle guidance (project > global > embedded)`; a `When` section listing every moment a `when:` entry can name, in that spelling (`spineMoments`) — the full event catalog in catalog order, bound or not, then `enter <state>` and `exit <state>` for each built-in state in lifecycle order (`open`, `in_progress`, the four phases in work order, `closed`), then the same pair for any custom state a loaded definition crosses, alphabetically; a `Labels` section (every label any definition binds — not every label used on tickets — deduped and sorted, or `(none bound)`); then warnings. Each point shows the bound definitions as `<id> "<name>" (<source>)` refs, least specific layer first (embedded, then global, then project; `DefinitionsByLayer` in `workflows.go`), and a definition appears at every point it binds. Dispatch does not use this order: `Matching` keeps the ID order of `Set.Definitions`, so injected guidance is unchanged. The warnings section, printed only on the overview and only when non-empty, is headed `Warnings (loaded but not fully active)` in the same layer order (`WarningsByLayer`) with each warning on one line as `Warning.String` renders it.

**`show <id>`** prints exactly `id:`, `name:` (`-` when empty), `source:`, `path:`, `when:` (the entries in their `when:` spelling, comma-separated), `labels:`, a `---` separator, then the body (`workflows.go`). Unknown ID → validation error naming the id, exit 3; missing ID → usage error.

**`dry-run`** builds a hypothetical occasion from repeatable `--when`, repeatable `--label`, and `--issue` (`workflows_dryrun.go`). Each `--when` is read by `ParseMoment`, so it is spelled and canonicalized as a `when:` entry is (`--when "Enter  Review"` is `enter review`), and a start from open is asked as `--when "exit open" --when "enter in_progress" --when "enter plan"`; a blank value is skipped, and an event lit never fires is refused with a validation error, exit 3, naming the crossing spelling and the catalog: `--when %q is not a moment lit fires: write enter <state> or exit <state> for a change of a ticket's state, or one of the events %s` (`dryRunMoments`). `--label` values are taken **verbatim, with no canonicalization**, so `--label Needs-Design` will not match a canonicalized definition label (`workflows_dryrun.go`). Output: an `occasion:` line — `when=` the moments as a bracketed list of quoted spellings (`quotedList`; `[]` when none, and quoting keeps one crossing of a state named `foo,bar` apart from two), then `labels=` and `issue=` (`-` for unset) — then `Fired (<n>)` with `(none)` or, per match, the definition ref, its match reasons, and its interpolated body indented 4 spaces. Dry-run never writes a firing trace and never reads the store (`workflows_dryrun.go`). Stray positionals and unknown flags are usage errors.

## `lit workflows edit` — scaffolding

`edit <target>` branches on whether the target is a loaded definition ID (`workflows_edit.go`).

**Existing ID**: if its source is `project`, the file is the override — it is printed/opened directly. If its source is `global` or `embedded`, the raw default bytes are copied verbatim to `<RootDir>/.lit/workflows/<path>` (refusing if that path exists — see no-clobber), with the notice `scaffolded override for "<id>" (was <source>) -> <path>`, then opened (`workflows_edit.go`). Raw-default read failures surface as `read global workflow default <path>: <err>` / `read embedded workflow default <path>: <err>`; asking for a raw default from the `project` layer is an error by construction — `workflows: no raw default for the project layer (it is the override target, not a source to copy from)` (`scaffold.go`).

**Not a loaded ID** — the target is a lifecycle "point", classified by `ParseMoment`, the reader of a `when:` entry (`workflows_edit.go`):

1. a moment this lit fires — `enter <state>` or `exit <state>` for any state, built-in or custom, or an event in the catalog → a `when:` point, live line `when: ["<moment>"]` in the moment's canonical spelling;
2. anything else — a bare state name like `review` and an unknown event among them → a label point, live line `labels: ["<point>"]`.

Every embedded value is uniformly double-quoted with `\` and `"` escaped, which keeps a comma-bearing state name from splitting into two YAML flow entries (`workflows_edit.go`).

The filename slug lowercases, trims, replaces runs outside `[a-z0-9_-]` with `-` (underscore deliberately kept, so `show_ticket` → `show_ticket.md`), trims edge dashes, and falls back to `point` (`workflows_edit.go`). Other pinned examples: `enter review` → `enter-review.md`, `exit closed` → `exit-closed.md`, `Enter  In_Progress` → `enter-in_progress.md`, `enter deploy:staging` → `enter-deploy-staging.md`, `exit foo,bar` → `exit-foo-bar.md`, `needs-design` → `needs-design.md`.

The fresh scaffold's content is fixed (`scaffold.go`): frontmatter opening `---`; four comment lines explaining that `when:` lists the moments and `labels:` narrows to tickets carrying a label, OR within a key and AND across, and pointing at `lit workflows --help`; a commented example of the key *other than* the one being scaffolded (`# when: [enter review, exit plan, show_ticket]  # entering or exiting a state, or an event` or `# labels: [needs-design, blocked]`); the live line; commented `# id:` and `# name:` hints; closing `---`; and the body placeholder ``Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.``

**No-clobber**: scaffolding refuses to overwrite in two stages — a stat check returning a merge-conflict error (exit 5) with ``cannot scaffold "<subject>": <path> already exists (edit it directly, or run `lit workflows` to see what's already loaded there)``, and the real enforcer, an `O_CREATE|O_EXCL` open that converts `IsExist` to the same error, closing the check-then-write race (`workflows_edit.go`). Scaffolding only ever writes under the project `.lit/workflows/`, never the global or embedded layers.

**Opening the file**: the path is always printed to stdout first. Unless stdin and stdout are both terminals, that is all (`interactive`, `internal/cli/interactive.go`). Otherwise `$EDITOR` is split on whitespace (so `EDITOR="code -w"` works as command plus args); an empty `$EDITOR` means print-only; otherwise the editor runs synchronously with the process's real stdin/stdout/stderr, and a non-zero exit becomes the error `open <path> in $EDITOR (<cmd>): <err>` (`workflows_edit.go`). The only environment variables the subsystem reads are `$EDITOR` here and `$XDG_CONFIG_HOME`/`$HOME` via the config dir.

## Relationship to `lit hooks`

None. `internal/cli/hooks.go` installs a git `pre-push` hook — a managed section of `<git-common-dir>/hooks/pre-push` bounded by `# --- BEGIN LIT INTEGRATION ---`/`END` markers (migrating legacy `LINKS INTEGRATION` markers), refusing to manage a hook whose first line is not a bash shebang (`hooks.go`). It imports nothing from the workflows package, and no workflow event fires on `lit hooks install`.

## What a workflow author can and cannot do

**Can**: bind on `when`, `labels`, or both; bind entering and exiting a state independently; bind custom non-lifecycle state names; nest files arbitrarily; override a farther layer by ID or by matching relative path; use `<id>` in the body; list an event a newer lit fires (the entry loads with a warning and the rest of the list still binds).

**Cannot**: run anything; receive environment variables; gate or block a command; set an exit code; use wildcards, globs, regexes, or negation; order or prioritize between simultaneous matches (all fire, ID-ascending); require ALL of several labels (label matching is OR); match on issue ID; tell which command made a crossing (`lit done` and `lit close` both make `enter closed`); fire a `when:`-bound definition on a transition that crossed nothing; or have a file fire while it carries a key this lit does not read.
