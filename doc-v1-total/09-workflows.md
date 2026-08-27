# Workflows and events

A **workflow** in lit is not automation. It is a markdown file whose body is printed verbatim to the calling command's stdout when a matching lifecycle moment occurs. Nothing is executed: no subprocess, no environment variables passed, no timeouts, no gating — the package answers only "which guidance is in place, and does it apply to this moment" (`internal/workflows/workflows.go:21-23`). A reader coming from Jira should map this to injected checklist/guidance text, not to automation rules or webhooks. The one subprocess anywhere in the subsystem is `$EDITOR`, launched by `lit workflows edit` (`internal/cli/workflows_edit.go:144`).

This document covers the definition-file format, loading and override, the event catalog, matching, dispatch, firing traces, and the `lit workflows` command surface.

## The definition file

A definition is a markdown file with optional YAML frontmatter (`internal/workflows/workflows.go:5-6`).

**Frontmatter delimiters** (`internal/workflows/parse.go:77-110`): the delimiter is a line equal to `---` after stripping trailing spaces, tabs, and `\r` (so CRLF files work). Frontmatter exists only if the *first line* of the file is a delimiter; otherwise the entire file is body and the definition is inert (see below). An opened-but-unclosed frontmatter block is an error (`unterminated frontmatter: missing closing ---`), which makes the file malformed: skipped entirely, one warning (`parse.go:105,123-126`). The body is everything after the closing delimiter, whitespace-trimmed (`parse.go:142`).

**Frontmatter keys** — the complete set (`parse.go:18-24`). Unknown keys are tolerated and ignored, deliberately, so a file authored for a newer lit still loads (`parse.go:15-17`). The frontmatter must be a YAML mapping; a top-level sequence or scalar — or a non-sequence value for a sequence-typed key like `labels: 17` — is malformed and skips the file (`parse.go:128-130`).

| Key | Type | Semantics |
|---|---|---|
| `id` | string | primary key across layers; trimmed; defaults to the layer-relative path with `.md` removed and spaces replaced by `_` (directory separators preserved: `review tasks/design check.md` → `review_tasks/design_check`) (`parse.go:137,161-163`). Lookup is exact string equality, no case folding (`workflows.go:103-110`) |
| `name` | string | optional display name, trimmed; used only in listings, rendered as `<id> "<name>" (<source>)` (`internal/cli/workflows.go:217-222`) |
| `labels` | list of string | activation dimension 1 |
| `states` | list (see below) | activation dimension 2 |
| `events` | list of string | activation dimension 3 |

**`labels`**: each entry is normalized exactly as stored labels are — lowercased, trimmed, empty rejected, commas forbidden (`internal/model/label.go:15-21`). Entries that trim to empty are silently dropped; a comma-bearing entry is dropped with the warning `label %q can never match: <err>` (`parse.go:175-180`). Because both sides are canonical, downstream matching is exact-string yet case-insensitive by construction (`parse.go:165-170`).

**`states`**: an entry is either a bare state name (meaning `when: enter`) or a mapping `{name: <state>, when: enter|exit}` (`parse.go:26-60`). `when` is lowercased and trimmed; absent means `enter`; any other value is a parse error (`when must be "enter" or "exit", got %q`) that skips the whole file (`parse.go:66-75`). A mapping with an empty name (`state entry mapping requires a non-empty name`) or any other YAML node kind (`state entry must be a state name or a {name, when} mapping`) is likewise a file-skipping parse error (`parse.go:51-59`). State names are lowercased and trimmed (`parse.go:192-194`); bare empty entries are dropped without warning (`parse.go:216-224`). State names are open strings, deliberately not a closed enum — custom stage names (which may contain colons or commas) are legal with no code change (`workflows.go:41-45`); the built-in three are `open`, `in_progress`, `closed`. Duplicate activations for one state with different `when` are preserved as two entries.

**`events`**: entries are lowercased and trimmed; empties dropped (`parse.go:202-210`). An event not in this binary's catalog still loads, preserved as authored, with the warning `unknown event %q: not in this lit's catalog, will never fire here` (`parse.go:149-153`).

**Inertness**: a definition with all three dimensions empty is inert — it loads, appears in listings, warns `no activation keys (labels/states/events): definition is inert and will never fire`, and never matches (`workflows.go:78-80`, `parse.go:146-148`, `match.go:45-47`). This is why a plain markdown file with no frontmatter loads but never fires.

**Body substitution**: exactly one substitution exists — every literal `<id>` in the body is replaced with the occasion's issue ID at injection time, unconditionally; when the occasion carries no issue ID (e.g. `show_backlog`) it is replaced with the empty string (`dispatch.go:82-84`). A prior mechanism's `<token>` substitution is not supported (`dispatch.go:78-81`).

The one shipped default, embedded in the binary (`internal/workflows/defaults/done.md`):

```
---
id: done
name: Post-close capture reminder
events: [work_finished]
---
Ticket <id> has been closed. Before moving on, take a moment to review related tickets. …
```

## Layers, loading, and override

Definitions load from three layers, nearest first (`load.go:50-54`):

1. **Project** — `<workspaceRoot>/.lit/workflows/`
2. **Global** — `<config-dir>/workflows/`, where the config dir is `$XDG_CONFIG_HOME/links-issue-tracker` if set, else `$HOME/.config/links-issue-tracker`; if no home dir can be determined the layer contributes nothing (`internal/config/config.go:175-184`, `load.go:83-88`)
3. **Embedded** — compiled into the binary via `go:embed`; today exactly one file, `done.md` (`load.go:17-22`)

Within a layer, discovery walks the root recursively; a file participates iff its path ends in `.md` (case-sensitive — `.MD` does not); the folder hierarchy carries no activation meaning and only seeds the default ID; walk order is lexical, making within-layer conflicts deterministic (`load.go:95-133`).

**Load can never fail** — `Load` returns a Set and no error; every problem becomes a `{source, path, message}` warning and the file is skipped while its siblings still load (`load.go:45`, `workflows.go:83-89`). The complete warning conditions:

| Condition | Message |
|---|---|
| absent layer root | none — genuine absence, not failure (`load.go:104-107`) |
| any other walk error, or file read error | `cannot read: <err>` |
| unterminated frontmatter | `unterminated frontmatter: missing closing ---` |
| YAML unmarshal failure | `invalid frontmatter: <err>` |
| comma-bearing label | `label %q can never match: <err>` |
| inert definition | `no activation keys (labels/states/events): definition is inert and will never fire` |
| unknown event | `unknown event %q: not in this lit's catalog, will never fire here` |
| duplicate ID within one layer | `duplicate id %q (already defined by %s): file ignored` |

**Override semantics** (`load.go:66-77,124-127`): within a layer, first file in lexical order claims an ID and later claimants are ignored with a warning. Across layers, the nearest layer to claim an ID wins outright and farther layers' definitions with that ID are skipped *silently* — that is the override feature. Override replaces wholly, never merges fields: a project `id: done` file replaces the embedded default including its body. Because the default ID is the layer-relative path, a file at the same relative path in a nearer layer overrides without an explicit `id`. The resolved set is sorted by ID ascending, and warnings accumulate from all layers, including overridden ones.

## The event catalog

Ten events, defined as string constants (`internal/workflows/events.go:13-44`). Event names are a stable contract deliberately decoupled from command names — commands may be renamed; the event name is what definitions bind to (`events.go:5-10`). Names are contractually lowercase snake_case (pinned by test).

| Wire name | Fires when | Command |
|---|---|---|
| `show_backlog` | agent views the workable backlog | `lit backlog` |
| `show_ticket` | agent views one ticket's details | `lit show` |
| `next_pulled` | agent asks for the next workable ticket | `lit next` |
| `work_started` | a ticket is claimed and work begins | `lit start` |
| `work_finished` | claimed work finishes on the success path | `lit done` |
| `ticket_closed` | a ticket is closed without finishing | `lit close` |
| `ticket_reopened` | a closed ticket is reopened | `lit open` |
| `ticket_created` | a new ticket is created | `lit new`, `lit followup` |
| `ticket_updated` | an existing ticket's fields change | `lit update` |
| `comment_added` | a comment lands on a ticket | `lit comment` |

`Catalog()` returns display order — `show_backlog, next_pulled, show_ticket, work_started, work_finished, ticket_closed, ticket_reopened, ticket_created, ticket_updated, comment_added` — which differs from declaration order and is the order `lit workflows` prints (`events.go:48-61`).

### The occasion payload

The single payload type is `Occasion` (`match.go:13-35`): `Event` (zero when none), `IssueID` (never read by matching — used only for `<id>` interpolation, display, tracing), `Labels` (canonical form; nil when no single ticket), `Entered` and `Exited` (states, set only when a transition happened). Per-event payloads as actually built (`internal/cli/workflow_events.go`):

| Event | Payload set |
|---|---|
| `show_ticket`, `next_pulled`, `ticket_created`, `comment_added` | Event, IssueID, Labels |
| `show_backlog` | Event only — no IssueID, no Labels, no transition |
| `ticket_updated` | Event, IssueID, Labels; never a transition — `lit update` rejects `--status` (`workflow_events.go:57-59`) |
| `work_started`/`work_finished`/`ticket_closed`/`ticket_reopened` | Event, IssueID, post-transition Labels, `Entered` = post state, `Exited` = pre state (`workflow_events.go:103-115`) |

The status-action→event map is `start→work_started`, `done→work_finished`, `close→ticket_closed`, `reopen→ticket_reopened`; an unmapped status action panics (`workflow_events.go:83-88,105-107`). Retention actions (archive/unarchive/delete/restore) are not status actions and fire **no event at all** (`internal/cli/cli.go:1408`, `workflow_events.go:78-82`).

### Dispatch call sites

Where each event actually fires, and where in the command's output the injected guidance lands:

| Site | Event | Position |
|---|---|---|
| `lit new` (`cli.go:359`), `lit followup` (`cli.go:431`) | `ticket_created` | after create succeeds, before the issue summary and breadcrumb |
| `lit show` (`cli.go:879`) | `show_ticket` | after detail load, before either `--field` output or the full view — fires for both |
| `lit update` (`cli.go:1026`) | `ticket_updated` | after apply, before summary |
| transitions (`cli.go:1409`) | one of the four transition events | after apply and authorize, before the claim-transfer notice |
| `lit comment` (`cli.go:1484`) | `comment_added` | after the comment is stored, before it is printed |
| `lit next` (`internal/cli/next.go:73`) | `next_pulled` | last, after the claim announcement and summary; only when a row was actually served — exhausted/no-work paths return before any occasion is built |
| `lit backlog` (`internal/cli/workable.go:167`) | `show_backlog` | last, after the table render |

## Matching

`Definition.Matches(occasion)` (`match.go:44-51`): inert → false; otherwise the three dimensions are tested and combined with AND, while values within one dimension combine with OR. An undeclared dimension constrains nothing.

- **Events** (`match.go:65-70`): empty bound list → unconstrained; otherwise the fired event must be non-empty and in the bound list, so an event-bound definition can never fire on an eventless occasion.
- **Labels** (`match.go:72-79`): empty → unconstrained; otherwise at least one bound label must appear among the occasion's labels (OR — there is no way to require ALL labels). Exact string comparison; both sides are pre-canonicalized.
- **States** (`match.go:81-96`): each activation reads the occasion side its `when` selects (`Entered` for enter, `Exited` for exit) and requires it to be non-empty and exactly equal to the activation's state name. An occasion with no transition never satisfies a state binding.

There are **no wildcards, globs, regexes, or negation** anywhere in matching; the only "match everything" construct is omitting a dimension. There is **no precedence between simultaneous matches** — every matching definition fires, in ID-ascending order (`match.go:55-63`). The only precedence in the system is the load-time layer/ID override, which guarantees at most one definition per ID.

**Match reasons** (`match.go:106-124`): for a matching definition, a fixed-order list of strings explains why — `event:<occasion's event>` (once, if the definition declared any events), `label:<l>` for each declared label that overlapped, `state:<s>(<when>)` for each activation that matched. This one computation backs both the firing trace and dry-run.

## Dispatch mechanics

`Dispatch(w, errOut, ws, occasion)` (`dispatch.go:60-74`) is synchronous and in-process — no bus, no queue, no subscriber registry. Behaviors:

- It **re-loads the full definition set from disk on every call**; there is no caching (`dispatch.go:61`).
- For each match it writes the interpolated body plus a newline to `w` — the calling command's own stdout at every call site.
- A write failure aborts and propagates as the command's error; since that is the only error path, in practice a workflow can never fail a command, and a malformed file degrades to a load warning rather than breaking any invocation.
- Load warnings are deliberately never printed by dispatch (so authoring diagnostics don't appear on every command); they surface only in `lit workflows` (`dispatch.go:48-59`).
- A trace-write failure never fails dispatch: the guidance was already written, and the failure goes to stderr as `lit: workflow firing trace could not be recorded (<err>); guidance was still injected` (`dispatch.go:68-72`).
- Exit codes are unaffected by workflow firing. (The general CLI mapping, for reference: 0 OK, 1 generic, 2 usage, 3 validation, 4 not-found, 5 conflict, 7 corruption — `internal/cli/exit.go:10-18`.)

## Firing traces

A JSON trace is written only when at least one definition fired — unmatched occasions leave no trace (`dispatch.go:29-33`). Recording is skipped outright when the workspace storage dir is not an absolute path (`dispatch.go:68`). Traces land in `<StorageDir>/traces/workflows/` where StorageDir is `<git-common-dir>/links` (`internal/trace/trace.go:23-25`, `internal/workspace/workspace.go:223`).

Filename: `<UTC 20060102T150405.000000000Z>-<slug>.json`, where the slug is the event name lowercased with runs of non-`[a-z0-9]` replaced by `-` (so `work_finished` → `work-finished`), falling back to `trace` for an eventless occasion (`trace.go:41-81`). Files are created `O_WRONLY|O_CREATE|O_EXCL` mode 0644 (dirs 0755); on collision up to 5 attempts with a fresh timestamp and an `-<attempt>` suffix; exhaustion yields `create workflows trace: too many id collisions` (`internal/trace/trace.go:36-66`).

Record shape (`internal/workflows/trace.go:29-39`), marshaled indented with a trailing newline; `event`, `issue_id`, `labels`, `entered`, `exited` are omitempty, `fired` is not; `recorded_at` is RFC3339Nano:

```json
{
  "id": "<filename stem>",
  "recorded_at": "<RFC3339Nano>",
  "workspace_id": "<workspace id>",
  "event": "show_ticket",
  "issue_id": "lit-42",
  "labels": ["needs-design"],
  "entered": "closed",
  "exited": "in_progress",
  "fired": [
    { "id": "needs-design-note", "source": "project", "path": "needs-design.md",
      "reasons": ["event:show_ticket", "label:needs-design"] }
  ]
}
```

## The `lit workflows` command

Registered as a workspace-only command — it resolves a workspace from the working directory but never opens the store (`internal/cli/register.go:278-279`). Usage:

```
lit workflows [show <id> | edit <id-or-point> | dry-run [--event <name>] [--label <name>]... [--enter <state>] [--exit <state>] [--issue <id>]]
```

Routing: no positionals → overview; `show <id>`; `edit <id-or-point>`; `dry-run`; anything else is a usage error (exit 2) (`internal/cli/workflows.go:34-58`). Overview, show, and edit take no flags — any flag-shaped token there is a usage error.

**Overview** (`workflows.go:123-194`) prints, in order: the header `lit workflows — work lifecycle guidance (project > global > embedded)`; an `Events` spine (the full catalog, always, bound or not, in catalog order); a `States` spine (the three built-ins in lifecycle order, then any custom state a loaded definition binds, alphabetically, each with `enter`/`exit` sub-lines); a `Labels` spine (every label any definition binds — not every label used on tickets — deduped and sorted, or `(none bound)`); then warnings. Each spine point shows the bound definitions as `<id> "<name>" (<source>)` refs; a definition appears at every point it binds. The warnings section, printed only on the overview and only when non-empty, is headed `Warnings (loaded but not fully active)` with each warning collapsed to one line.

**`show <id>`** prints exactly `id:`, `name:` (`-` when empty), `source:`, `path:`, `labels:`, `states:` (as `state(when)`), `events:`, a `---` separator, then the body (`workflows.go:249-273`). Unknown ID → validation error naming the id, exit 3; missing ID → usage error.

**`dry-run`** builds a hypothetical occasion from `--event`, repeatable `--label`, `--enter`, `--exit`, `--issue` — **verbatim, with no canonicalization**, so `--label Needs-Design` will not match a canonicalized definition label (`workflows_dryrun.go:37-43`). Output: an `occasion:` line summarizing the inputs (`-` for unset), then `Fired (<n>)` with `(none)` or, per match, the definition ref, its match reasons, and its interpolated body indented 4 spaces. Dry-run never writes a firing trace and never reads the store (`workflows_dryrun.go:20-22`). Stray positionals and unknown flags are usage errors.

## `lit workflows edit` — scaffolding

`edit <target>` branches on whether the target is a loaded definition ID (`workflows_edit.go:37-43`).

**Existing ID**: if its source is `project`, the file is the override — it is printed/opened directly. If its source is `global` or `embedded`, the raw default bytes are copied verbatim to `<RootDir>/.lit/workflows/<path>` (refusing if that path exists — see no-clobber), with the notice `scaffolded override for "<id>" (was <source>) -> <path>`, then opened (`workflows_edit.go:45-64`). Raw-default read failures surface as `read global workflow default <path>: <err>` / `read embedded workflow default <path>: <err>`; asking for a raw default from the `project` layer is an error by construction — `workflows: no raw default for the project layer (it is the override target, not a source to copy from)` (`scaffold.go:22-40`).

**Not a loaded ID** — the target is classified as a lifecycle "point", in fixed order (`workflows_edit.go:201-212`):

1. a `<state>:enter` / `<state>:exit` suffix → a state point; the split is at the **last** colon, so `deploy:staging:enter` names the state `deploy:staging`;
2. a name in the event catalog → an event point (`events: ["<point>"]`);
3. a bare built-in state → a state point defaulting to `enter`;
4. anything else → a label point (`labels: ["<point>"]`).

The scaffold's live line for a state point is `states: ["<state>"]` (enter) or `states: [{name: "<state>", when: exit}]` (exit); every embedded value is uniformly double-quoted with `\` and `"` escaped, which keeps a comma-bearing state name from splitting into two YAML flow entries (`workflows_edit.go:222-263`).

The filename slug lowercases, trims, replaces runs outside `[a-z0-9_-]` with `-` (underscore deliberately kept, so `work_started` → `work_started.md`), trims edge dashes, falls back to `point`, and appends `_exit` for exit-side state points (`closed:exit` → `closed_exit.md`) (`workflows_edit.go:176-186,265-270`). Other pinned examples: `deploy:staging:enter` → `deploy-staging.md`, `needs-design` → `needs-design.md`, `foo,bar:enter` → `foo-bar.md`.

The fresh scaffold's content is fixed (`scaffold.go:49-71`): frontmatter opening `---`; two comment lines explaining AND-across/OR-within; commented example lines for each dimension *other than* the one being scaffolded (`# labels: [needs-design, blocked]`, both `# states:` forms with enter/exit comments, `# events: [work_started]`); the live line; commented `# id:` and `# name:` hints; closing `---`; and the body placeholder ``Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.``

**No-clobber**: scaffolding refuses to overwrite in two stages — a stat check returning a merge-conflict error (exit 5) with `cannot scaffold "<subject>": <path> already exists (edit it directly, or run 'lit workflows' to see what's already loaded there)`, and the real enforcer, an `O_CREATE|O_EXCL` open that converts `IsExist` to the same error, closing the check-then-write race (`workflows_edit.go:85-125`). Scaffolding only ever writes under the project `.lit/workflows/`, never the global or embedded layers.

**Opening the file**: the path is always printed to stdout first. If stdout is not a character-device terminal, that is all. Otherwise `$EDITOR` is split on whitespace (so `EDITOR="code -w"` works as command plus args); an empty `$EDITOR` means print-only; otherwise the editor runs synchronously with the process's real stdin/stdout/stderr, and a non-zero exit becomes the error `open <path> in $EDITOR (<cmd>): <err>` (`workflows_edit.go:133-150`). The only environment variables the subsystem reads are `$EDITOR` here and `$XDG_CONFIG_HOME`/`$HOME` via the config dir.

## Relationship to `lit hooks`

None. `internal/cli/hooks.go` installs a git `pre-push` hook — a managed section of `<git-common-dir>/hooks/pre-push` bounded by `# --- BEGIN LIT INTEGRATION ---`/`END` markers (migrating legacy `LINKS INTEGRATION` markers), refusing to manage a hook whose first line is not a bash shebang (`hooks.go:18-21,59-60,92-115`). It imports nothing from the workflows package, and no workflow event fires on `lit hooks install`.

## What a workflow author can and cannot do

**Can**: bind on any subset of {labels, states, events}; use enter/exit sides independently; bind custom non-lifecycle state names; nest files arbitrarily; override a farther layer by ID or by matching relative path; use `<id>` in the body; author for a future lit (unknown events and unknown YAML keys both load).

**Cannot**: run anything; receive environment variables; gate or block a command; set an exit code; use wildcards, globs, regexes, or negation; order or prioritize between simultaneous matches (all fire, ID-ascending); require ALL of several labels (label matching is OR); match on issue ID; or match a state binding on a moment that carries no transition.
