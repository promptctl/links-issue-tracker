# CLI reference

Every `lit` command: synopsis, the flags that matter, and the behavior `--help` can't
tell you. For a guided tour instead of a reference, start with
[Getting started](introduction/getting-started.md).

## Conventions

These hold across the whole CLI; per-command sections below only note deviations.

### Output

Every command prints human-readable text — the one canonical surface, designed to be
read directly by both humans and agents. There is no `--json` flag; the terse default
text *is* the agent interface. Each output line is simple enough to parse when a script
needs one field (e.g. `lit workspace | sed -n 's/^traces_dir: //p'`).

`lit export` and `lit lifeboat dump` are the exceptions: they emit a JSON data structure
as their sole output, because a full database export / raw dump has no text form. They
take no flag — the structured output *is* the command.

### Exit codes

Exit codes are a contract, not just 0/1:

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Generic failure |
| 2 | Usage error (bad arguments or flags) |
| 3 | Validation error (missing required value, unsupported value) |
| 4 | Issue or resource not found |
| 5 | Conflict (e.g. sync merge conflict) |
| 7 | Data corruption detected |

### Identity

Commands that record an actor resolve it from the `CLAUDE_CODE_SESSION_ID` environment
variable (producing an identity like `claude_<session-id>`). The `--assignee` flag on
lifecycle commands is a *fallback* for when that variable is unset — when both are
present, the environment wins.

This session resolution is claim-time only: it applies to `lit start` (and a bare
`lit update --status in_progress` that says nothing about assignee). Field-writing
commands — `new`, `followup`, `update`, `assign` — honor an explicit `--assignee`
verbatim and never substitute the caller's identity; on `lit update`, an empty
`--assignee ""` clears the assignee, returning the issue to unassigned.

### Transition guidance

Every lifecycle transition is single-phase: one invocation performs the action. `lit done`
additionally prints **post-close guidance** — a prompt to capture, while context is fresh,
whatever the next agent needs (follow-ups, comments on adjacent tickets). It does not gate
the close.

Verifying that finished work is *correct* is not lit's job: `lit done` runs after the change
has merged, so it cannot gate the merge. Pre-merge verification belongs at the boundary that
runs before merge — CI and required PR checks — which lit deliberately does not own. lit
dictates nothing about a repository's CI, PR, or merge process.

The guidance is template-driven: a transition prints guidance for a phase when a
`guidance-<action>-<phase>.md` template exists in the workspace, and `done`'s built-in
`post` template is the only one shipped by default.

### Lifecycle

Statuses are `open`, `in_progress`, and `closed`, with `archived` and `deleted` as
recoverable flags on top. Two distinct paths lead to `closed`: `done` (work finished;
only valid from `in_progress`) and `close` (wontfix / obsolete / duplicate; valid from
any non-closed state). The distinction is recorded in history.

---

## Bootstrap

### `lit init`

```text
lit init [--skip-hooks] [--skip-agents]
```

Initializes the issue store under `$(git rev-parse --git-common-dir)/links/`, adds
managed `lit` sections to `AGENTS.md` / `CLAUDE.md`, and installs the sync git hook.
Idempotent: re-running reconciles the managed files. `--skip-hooks` and `--skip-agents`
suppress the respective side effects.

On a fresh clone, `lit init` detects whether the configured git remote already
carries `lit` ticket data and adopts it automatically, so the clone transparently
picks up the existing backlog (it prints `Pulled existing backlog from <remote>/<branch>`).
The store lives in `.git/links/dolt`, which `git clone` does not transfer, so init is
the place that makes "clone + init = my tickets are here" true. Adoption runs only when
the local store has no tickets of its own — a workspace with local work is never
overwritten — so it is also safe to re-run after a transient network failure.

---

## Working the queue

### `lit ready`

```text
lit ready [--assignee <a>] [--labels <csv>] [--status open|in_progress] [--type <t>] [--limit <n>] [--columns <csv>]
```

The pull view: epics with their top workable ticket, plus in-progress and orphaned
tickets — what should be picked up next, top item first. Blocked items are excluded and
counted in a footer.

### `lit backlog`

```text
lit backlog [filters as for ready]
```

Every workable item in rank order with blocked items shown **inline**, so the shape of
the queue is legible. Use when grooming or re-ranking.

### `lit queue`

```text
lit queue [filters as for ready]
```

Terse rank-ordered list of pullable items only — blocked items dropped, no preamble. The
minimal machine-friendly pull order.

### `lit next`

```text
lit next [--assignee <a>] [--continue]
```

Prints the single next workable leaf to `lit start`. `--continue` biases toward leaves
under epics that are already in progress.

### `lit orphaned`

```text
lit orphaned [--assignee <a>]
```

Lists `in_progress` issues with no recent updates — claimed work that went quiet and
needs someone to finish or release it.

### `lit ls`

```text
lit ls [--ids <csv>] [--search <text>] [--query <q>] [--status open|in_progress|closed]
       [--type <t>] [--labels <csv>] [--assignee <a>] [--has-comments]
       [--updated-after <rfc3339>] [--updated-before <rfc3339>]
       [--include-archived] [--include-deleted]
       [--sort rank:asc,updated_at:desc] [--limit <n>] [--columns <csv>]
       [--format lines|table]
```

General-purpose listing, ranked by default. `--search` matches title and description
text; `--query` is a compact query language combining filters and text (e.g.
`status:in_progress type:task has:comments login`). Archived and deleted issues are
hidden unless explicitly included.

### `lit show`

```text
lit show <id>
```

Full detail for one issue: description, status, labels, comments, history. For an issue
inside an epic, also prints the epic plan — siblings in rank order with status and any
cross-epic dependencies. Exits 4 if the ID doesn't exist.

---

## Creating and editing issues

### `lit new`

```text
lit new --title <text> --topic <slug> [--type task|feature|bug|chore|epic]
        [--description <text>] [--parent <id>] [--lane <key>] [--priority 0|1]
        [--labels <csv>] [--assignee <a>] [--prompt <text>] [--bottom]
```

Creates an issue and **prints its generated ID** — capture it; IDs are not guessable.
`--topic` is required and immutable: a 1–2 word slug naming the stable area of work.
New issues rank to the top by default; `--bottom` appends instead (use when authoring a
batch in order). With `--parent`, the child's ID becomes `<parentID>.<n>`. `--lane`
partitions an epic's children into parallel rank-ordered sub-sequences: a shared lane
serializes, distinct lanes parallelize. `--prompt` stores a reusable agent prompt for
the work the issue captures.

### `lit update`

```text
lit update <id> [--title <text>] [--description <text>] [--prompt <text>]
           [--type <t>] [--priority 0|1] [--assignee <a>] [--labels <csv>]
           [--lane <key>] [--status open|in_progress|closed] [--reason <text>]
```

Field-level edit of an existing issue. `--status` performs a lifecycle transition inline
(with `--reason` recorded); prefer the dedicated transition commands, which carry
guidance. `--labels` replaces the full label set — use `lit label add`/`rm` for
incremental changes. `--assignee` is taken verbatim (no session-identity substitution);
`--assignee ""` clears the field, returning the issue to unassigned.

### `lit comment add` / `lit comment rm`

```text
lit comment add <id> --body <text>
lit comment rm <comment-id>
```

Comments are the work trail: plans, findings, hand-off notes. Removal takes the
comment's own ID (shown in `lit show`), not the issue ID.

### `lit label add` / `lit label rm`

```text
lit label add <issue-id> <label>
lit label rm <issue-id> <label>
```

Incremental label edits. Two labels are reserved and carry derived behavior:
`needs-design` marks an issue blocked (membership), and `focus` marks an issue
as a goal whose unfinished prerequisite chain — explicit dependencies, epic
children, and earlier same-lane siblings, transitively — sorts to the top of
`ready`/`queue`/`next` (ordering only; blocked path items stay blocked, and
the path re-derives as items close). Focus outranks urgent priority; urgent
alone never propagates to prerequisites.

### `lit followup`

```text
lit followup --on <closed-id> --title <text> [--description <text>] [--topic <slug>]
             [--type <t>] [--priority 0|1] [--assignee <a>] [--labels <csv>]
             [--bottom]
```

Files a follow-up parented to a just-closed ticket — the way to capture work surfaced
during a ticket while context is fresh. Inherits `--topic` from `--on` when omitted;
the description defaults to a reference back to the source ticket.

### `lit rank`

```text
lit rank <id> --top | --bottom | --above <other-id> | --below <other-id>
```

Moves one issue in the rank order. Exactly one placement flag is required.

Relative placement (`--above`/`--below`) operates between *peers*: two siblings
inside the same epic, or two top-level items. When the named issue and the
anchor live in different epics (or one is standalone), the request is resolved
to the comparable pair — ranking against an epic's child behaves as ranking
against the epic itself, and ranking a child against an outside issue moves its
epic, never reordering anything inside an epic. The output states the
resolution whenever it substitutes an epic for a named issue. Ranking an issue
relative to its own epic (either direction) is an error.

### `lit rank set`

```text
lit rank set <id1> <id2> [<id3> ...]
```

Establishes absolute order across N issues atomically by stacking them at the
top of the rank order: `id1` becomes topmost, `id2` ranks just below, and so
on. Either every assignment applies or none does.

The same peer rule as relative placement applies: each named ID is resolved to
its representative in the comparable frame, so naming an epic's child alongside
outside issues ranks the epic, never reordering anything inside it. Every
substitution is reported in the output. Two requests are rejected as
incoherent: naming an issue together with its own epic (either direction), and
naming two issues that resolve to the same epic — their relative order is
internal to that epic and cannot be set against outside issues (run `rank set`
among the siblings instead).

### `lit assign`

```text
lit assign <id> <new-assignee> [--reason <text>]
```

Reassigns without changing status — hand-off of claimed work.

---

## Lifecycle transitions

All seven share one shape (see [Transition guidance](#transition-guidance) and
[Identity](#identity) for guidance and `--assignee` semantics):

```text
lit <verb> <id> [--reason <text>] [--assignee <fallback>]
```

| Command | Transition | Notes |
|---------|-----------|-------|
| `lit start` | `open → in_progress` | Claims the issue and assigns it to you. |
| `lit done` | `in_progress → closed` | Success path; prints post-close capture guidance. Refuses from any status but `in_progress`. |
| `lit close` | any non-closed → `closed` | Wontfix / obsolete / duplicate — closing without finishing. |
| `lit open` | reopen a closed issue | |
| `lit archive` / `lit unarchive` | set / clear the archived flag | Archived issues hide from listings. |
| `lit delete` / `lit restore` | set / clear the deleted flag | Soft delete; `restore` brings it back. |

---

## Dependencies and structure

### `lit dep add` / `lit dep rm` / `lit dep ls`

```text
lit dep add <from-id> <to-id> [--type blocks|parent-child|related-to]
lit dep add --blocker <id> --blocked <id>          # blocks-only alternative spelling
lit dep rm <from-id> <to-id> [--type <t>]
lit dep ls <issue-id> [--type <t>]
```

Manages relationship edges. The default type is `blocks` (first ID blocks the second);
the `--blocker`/`--blocked` spelling makes direction explicit. `blocks` edges are not
allowed between two issues in the same epic — within an epic, rank is the ordering
signal. `related-to` is symmetric annotation with no scheduling effect.

### `lit parent set` / `lit parent clear`

```text
lit parent set <child-id> <parent-id>
lit parent clear <child-id>
```

Manages epic membership. Epics contain children; an epic's completion is derived from
its children rather than tracked as its own status.

### `lit children`

```text
lit children <parent-id>
```

Lists an issue's children in rank order.

---

## Bulk operations

### `lit bulk label` / `lit bulk close` / `lit bulk archive`

```text
lit bulk label <add|rm> --ids <csv> --label <label>
lit bulk close --ids <csv> [--reason <text>]
lit bulk archive --ids <csv> [--reason <text>]
```

Apply one label edit or lifecycle transition across many issues in one call.

### `lit import` / `lit bulk import`

```text
lit import --path <tree-spec.json>
lit bulk import --path <export.json> [--force]
```

Two different inputs: `lit import` bulk-creates issues from a JSON **tree spec**
(nested parent/child authoring format); `lit bulk import` loads a JSON **export**
produced by `lit export`, and refuses to overwrite unsynced local state without
`--force`.

---

## Sync and data safety

### `lit sync`

```text
lit sync status
lit sync remote ls
lit sync fetch [--remote <name>] [--prune] [--verbose]
lit sync pull  [--remote <name>] [--verbose]
lit sync push  [--remote <name>] [--force] [--set-upstream] [--verbose]
lit sync reconcile                                              # run the field-aware reconcile; surface any prose divergence
lit sync reconcile resolve --resolve ID:FIELD:FINGERPRINT=TEXT … # finalize with the agent's merged text
lit sync reconcile abort                                        # leave the clone diverged for now
```

Mirrors issue data through git remotes so one backlog is shared across clones — see
[Sync and remotes](dolt-remote-sync.md). `pull`/`push` default the remote to the
upstream remote, then to the single configured remote. A merge conflict exits 5.

`reconcile` merges a diverged clone into linear history with the field-aware
engine. When both sides rewrote the same free-text field (`title`, `description`,
or `agent_prompt`) the engine cannot pick a winner, so `lit sync reconcile`
prints `base`/`ours`/`theirs` for each field and exits 5; the calling agent merges
both intents into one text and supplies it via `lit sync reconcile resolve
--resolve 'ID:FIELD:FINGERPRINT=<merged text>'` (one `--resolve` per pending field,
all in one command — copy the `ID:FIELD:FINGERPRINT` prefix verbatim from the
guidance). The pending state is re-derived live and never persisted; the
fingerprint pins each merge to the exact conflict it was made against, so a
partial or stale resolution (including one merged against a since-changed
base/ours/theirs) is rejected and re-surfaced. `abort` defers — the clone stays
diverged and usable.

### `lit export`

```text
lit export
```

Writes a complete versioned JSON snapshot of the workspace to stdout (always JSON; no
flags). The input format for `lit bulk import`.

### `lit backup`

```text
lit backup create [--keep <n>]
lit backup list
lit backup restore (--latest | --path <p>) [--force]
```

Logical backup snapshots with rotation (`--keep`, default 20). `restore` refuses to
overwrite unsynced state without `--force`.

### `lit snapshots`

```text
lit snapshots new [--label <text>]
lit snapshots list
lit snapshots restore <name>
```

Filesystem-level workspace snapshots — coarser and lower-level than `lit backup`,
capturing the store directory wholesale.

### `lit recover`

```text
lit recover (--from-backup <p> | --latest-backup | --from-sync <p>) [--force]
```

Single entry point for restoring a workspace from a backup snapshot or a sync file.

### `lit lifeboat`

```text
lit lifeboat dump
lit lifeboat recover [--mapping <shape-mapping.json>]
```

Below-the-gate recovery for a workspace whose schema the binary cannot open: `dump`
emits the raw contents at any schema version (always JSON, to stdout); `recover`
rebuilds a clean workspace from them. The default deterministic mapper handles known
shapes; for an unrecognized shape, author a ShapeMapping (typically by feeding the dump
to an LLM) and pass it via `--mapping`. Recovery is converge-or-change-nothing: a failed
attempt leaves the workspace untouched.

---

## Maintenance

### `lit doctor`

```text
lit doctor [--fix[=<area,...>]]
```

Health check. Bare `--fix` applies all available fixes; `--fix rank` (comma-separated)
scopes them. Run `lit doctor --fix` before escalating any persistent error.

The report also includes a `sync:` line reporting freshness against the configured
remote — ahead (local ticket changes not pushed), behind (remote changes not pulled,
as of the last fetch), diverged, up to date, or never synced — and names the
`lit sync push`/`lit sync pull` command to fix it. The behind direction is read from
the local remote-tracking ref, so it reflects the last fetch; doctor does not reach
the network.

### `lit hooks install`

```text
lit hooks install
```

Installs the shared `pre-push` sync hook into the clone's common git dir, so every
worktree of the clone inherits it.

### `lit workspace`

```text
lit workspace
```

Prints workspace metadata — which store you are actually talking to. The store is
selected by the `git rev-parse --git-common-dir` of your **current directory**; when
listings look unfamiliar, this is the first thing to check.

### `lit prefix set`

```text
lit prefix set <new-prefix> [--apply]
```

Renames the cosmetic issue-ID prefix. Preview-first: without `--apply` it prints what
would change.

### `lit downgrade`

```text
lit downgrade --to <vX.Y.Z>
```

Reverses schema migrations and atomically installs the prior `lit` binary for the given
v-prefixed git tag — the rollback path for a bad upgrade.

### `lit version`

```text
lit version
```

Prints binary version, build metadata, and the supported schema version range. The
schema range is what determines whether this binary can open a given workspace.

---

## Guidance and tooling

### `lit quickstart`

```text
lit quickstart [ready|new|update|done|doctor] [--refresh] [--eject[=LIST]] [--force]
```

Bare `lit quickstart` prints the router: the authoritative, always-current entry point
for the loop documented in [Agent setup](agent-setup.md), listing the topic subcommands
and the `lit ready` → `lit start <id>` fastpath. Each topic prints task-specific
guidance at the moment of need: `ready` (finding and starting work), `new` (creating
tickets), `update` (managing existing tickets), `done` (finishing work), `doctor`
(troubleshooting). `--eject` copies the embedded default templates to the global
override path so you can customize them (`LIST` is comma-separated short names, e.g.
`quickstart,quickstart-ready,agents,hook`; `--force` overwrites existing overrides);
`--refresh` re-syncs managed repo assets and reports override drift without touching
overrides. Topics take no flags.

Mutation commands point back here at the moment of need: the text success output of
`new`/`followup` ends with a one-line breadcrumb at `lit quickstart new`, `start` at
`lit quickstart ready`, `done`/`close` at `lit quickstart done`, and
`update`/`rank`/`label`/`parent`/`dep` at `lit quickstart update`.

### `lit completion`

```text
lit completion <bash|zsh|fish>
```

Writes a shell completion script to stdout. See
[Installation](introduction/installation.md) for where to put it.
