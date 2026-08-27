# CLI: issue commands

`lit` is a single binary whose subcommands cover the issue workflow: create, list, show, update, rank, transition, relate, comment, label, bulk-edit, and import/export. This chapter specifies every issue-facing command — its flags, refusal conditions, exit codes, side effects, and exact output — plus the shared CLI plumbing they all run through: argument dispatch, error conventions, identity resolution, and the workability predicate behind `backlog` and `next`. Operations commands (`init`, `sync`, `doctor`, backups, etc.) are chapter 07; claims internals are chapter 08; workflow guidance dispatch is chapter 09.

Two global facts shape everything below. First, output is line-oriented text: exactly one command in this scope emits JSON (`lit export`), and the removed `--output` flag is actively rejected everywhere (`internal/cli/cli.go:174-179, 286-289`). Second, issue IDs are always passed verbatim — there is no fuzzy or short-prefix ID resolution anywhere in the CLI; a wrong ID is a not-found error, exit 4 (`cli.go:874, 1372`).

## Command dispatch and flag parsing

The whole command tree is a table of `CommandSpec` rows (`register.go:254-394`). Every cobra command is created with flag parsing disabled (`DisableFlagParsing: true`), so each handler parses its own argv slice (`register.go:409-426`). Consequences a reimplementer must preserve:

- **Global argument scan** (`cli.go:165-186`): a leading `--` stops scanning; a leading `--output` (or `--output=x`) is rejected before any command runs with "`--output` is no longer supported; omit it for text output" → exit 3.
- **Bare `lit`**: outside a git repo prints cobra help; inside a workspace prints the quickstart guidance, byte-identical to `lit quickstart` (`cli.go:64-78`). `lit <unknown-token>` → `unknown command "<x>"` → exit 3 (`cli.go:59-61`).
- **Per-command flag parsing** (`parseFlagSet`, `cli.go:274-308`): `--help` prints `Usage of <cmd>:` plus flag defaults **to stdout** and exits 0. An unknown `--output` or `--continue` maps to a tailored retirement message (exit 3); any other unknown flag is a usage error, exit 2. Flag kinds include repeatable string-array flags (never comma-split) and "string-optional" flags that take one value when present bare (`cli.go:230-241`). Flags can be registered hidden-but-functional (`cli.go:261-263`).
- **Positional/flag splitting** (`splitArgs`, `cli.go:1958-1978`): a `-`-prefixed token with no `=` consumes the following non-`-` token as its value — so a boolean flag written `--flag value` swallows `value`. Extra non-flag tokens beyond the expected positional count land in the flag slice, where they surface only if the command checks `fs.NArg()`. Several commands do not check: `new`, `followup`, `ls`, `rank`, `export`, `children`, `parent clear` silently ignore stray positionals (`cli.go:340-355` et al.).
- **Family dispatch** (`register.go:112-123`): multi-word commands (`comment add`, `dep ls`, …) resolve `args[0]` by exact string match against a family table. Zero args or an unknown subcommand return the usage string as a **plain error → exit 1**, whereas per-command usage refusals are `UsageError` → exit 2. The transition commands' wrong-arity refusal and `completion`'s are also plain errors, exit 1 (`cli.go:1365, 1717`).
- **Store access**: handlers are wrapped with a fixed access mode (read/write). Opening outside a git repository yields "links requires running inside a git repository/worktree" → exit 1 (`cli.go:110-112`). After a successful **write** command, the wrapper prints the mutation sync-staleness warning and then runs post-command auto-sync (`cli.go:135-145`).

Help groups, in order: Human Bootstrap, Agent Operations, Dependencies & Structure, Sync & Data, Setup & Maintenance, Issue Retention, Guidance & Tooling (`register.go:61-76`).

## Exit codes and error output

Exit constants (`exit.go:10-18`): 0 OK, 1 generic, 2 usage, 3 validation, 4 not found, 5 conflict, 7 corruption (6 unused). `ExitCode(err)` dispatches by error type in a fixed order (`exit.go:23-95`):

| Exit | Error types |
|---|---|
| 4 | `storage.NotFoundError` |
| 5 | `MergeConflictError`, `SyncFailureError`, owner-approval refusal |
| 7 | `CorruptionError` |
| 2 | `UsageError` |
| 3 | `UnknownCommandError`, `RetiredCommandError`, `ValidationError` (CLI and storage), `UnsupportedError` |
| 1 | `OutsideWorkspaceError`, `BulkFailureError`, transient GC contention, everything else |

On failure the process prints to stderr: `error (code=N): <message>`, then a `remediation:` line when the reason has one (`error_output.go:17-24`). Each error type maps to a reason string (`entity_not_found`, `usage_error`, `corruption_detected`, …) and each reason to a fixed remediation sentence — e.g. not-found → "Verify the target ID exists with `lit ls` or `lit show <id>`."; corruption → "Run `lit doctor --fix integrity` and retry." Several remediations embed literal `<agent-instructions>` tags telling agent callers the fix is idempotent and safe (`error_output.go:29-133`). Progress/diagnostic text goes to stderr as `lit: <operation>: <text>`; stdout is the result channel (`progress.go:15-27`).

## Identity, breadcrumbs, banners, events

**Actor/assignee identity.** If env `CLAUDE_CODE_SESSION_ID` is set, the identity is `claude_<session-id>`, overriding any explicit value; otherwise the trimmed explicit value (`cli.go:1172-1177`). Most mutating commands register a hidden `--by` flag as the explicit fallback (`cli.go:1202-1206`); it never appears in help. An empty actor is stored as `unknown` (`internal/store/store.go:1087-1090`). The rules deliberately diverge by command: `start --assignee` goes through session-identity resolution (env wins), while `update --assignee`, `new --assignee`, and `followup --assignee` write the trimmed literal — so `update --assignee ""` clears the assignee (`cli.go:1000-1010`). An empty assignee renders as `(unassigned)`.

**Breadcrumbs.** Successful mutations append `deeper guidance: lit quickstart <topic>`: `new`/`followup` → topic `new`; `update`, `rank`, `rank set`, `dep add/rm`, `label add/rm`, `parent set/clear` → `update`; `start` → `work`; `done`/`close` → `done`. The other transitions and `comment` emit none (`quickstart_topics.go:62-75`, `cli.go:1223-1227, 1444-1447`). Emitting an unregistered topic panics.

**Sync-staleness banners.** Read commands `backlog`, `next`, and `show` (full-detail mode only — suppressed under `--field` to keep output machine-parseable) print a staleness warning before their payload; write commands get one after success (`cli.go:135-137, 863-873`).

**Workflow events.** Commands dispatch occasions rendered by the workflows subsystem (chapter 09): `show` → show-ticket, `backlog` → show-backlog, `next` → next-pulled, `new`/`followup` → ticket-created, `update` → ticket-updated, `comment add` → comment-added, and the status transitions map `start`→work-started, `done`→work-finished, `close`→ticket-closed, `open`→ticket-reopened (`workflow_events.go`). Retention actions (archive/unarchive/delete/restore) fire no event (`cli.go:1408-1412`). Bulk operations fire no events at all.

## Output primitives

- One-line success form (used by `new`, `followup`, `update`, `rank`, transitions): `id [state/type/topic/priority] title[labels]` (`output.go:49-52`). State renders with a `+archived` or `+deleted` suffix when frozen — never both (`output.go:526-541`).
- List rendering: default column set `id, state, topic, title`; valid names are `id, state, type, topic, priority, title, assignee, labels, updated_at, created_at, parent, blocked`. Unknown names are silently dropped; if none remain, the default set applies (`output.go:390-411`). `lines` format joins with `" | "`; `table` writes an uppercased tab-aligned header + rows. `parent`/`blocked` columns trigger a batch relation load; `blocked` shows the literal `blocked` when the issue has any live dependency (`cli.go:634-663`).
- Timestamps: RFC3339 in columns and `--field`; history timestamps render `Jan 2, 2006 3:04 PM MST` in local time (`output.go:22, 440-442`). Durations humanize to coarse buckets: days ≥48h, hours ≥2h, minutes ≥2m, else "under a minute" (`output.go:451-462`).
- Vocabulary parsing at the CLI boundary: issue types and priorities on write paths wrap failures as validation errors (exit 3); the read-path `--type`/`--status` filters return bare parse errors. CSV inputs split on `,`, trim, and drop empties (`cli.go:1806-1888`).

## The workability predicate

`backlog` and `next` share one pipeline (`cli.go:714-779`; annotators in `ready_state.go`):

1. **Candidate set**: list issues with statuses `[open, in_progress]` (or the single `--status` value), plus any type/assignee/labels filters; archived and deleted excluded; store order is rank-ascending.
2. **Leaves only**: containers (epics) and closed issues are dropped — an epic is never a workable row (`cli.go:1151-1160`).
3. **Annotators**, each attaching typed annotations:
   - *Required fields* — from config `ready.required_fields`; a policy naming a non-wire field is a validation error. A field is "set" if non-nil, non-blank (strings), non-empty (arrays/maps). Each unset field → `MissingField`.
   - *Blockers* — each non-closed dependency → `OpenDependency`; additionally `RankInversion` when the dependency ranks below its dependent (`ready_state.go:123-152`).
   - *Sibling gate* — when the parent is an epic: each sibling with the same lane and a lower rank that is still in play → `EarlierSiblingPending`. The sibling set is the epic's **unfiltered** in-play children, so siblings hidden by CLI filters still gate (`ready_state.go:173-238`).
   - *Orphaned* — `in_progress` issues untouched for ≥ 6 hours (constant, `ready_state.go:48`).
   - *Needs-design* — the reserved label `needs-design` → `NeedsDesign`.
   - *Focus path* — issues on the prerequisite closure of any open goal labeled `focus`, computed by BFS over dependencies, container children, and earlier same-lane siblings; shared prerequisites attribute to the first goal reached (`ready_state.go:246-401`).
4. **Classification**: annotations map to roles — blocking (`MissingField`, `OpenDependency`, `EarlierSiblingPending`, `NeedsDesign`), orphaned, rank-inversion, or none (`FocusPath`). **Ready = zero blocking annotations** (`readiness.go:42-90`). An unclassified kind panics.
5. **Ordering**, three stable sorts in sequence: composite rank (a leaf inside an epic sorts by the epic's rank, then its own), then priority (urgent first), then focus-path rows first — so focus outranks urgent (`cli.go:775-777`, `ready_state.go:490-537`).

Rollups partition rows as: `in_progress` first (even if also blocked), else blocked, else ready (`ready_state.go:582-595`).

## Creating issues

**`lit new`** (write; `cli.go:327-366`). Flags: `--title`, `--description`, `--prompt` (reusable agent prompt), `--type` (default `task`), `--topic` (required immutable slug), `--parent` (child IDs become `parentID.<n>`), `--priority` (0/1, default 0), `--assignee` (trimmed literal), `--labels` (CSV), `--lane` (partitions an epic's children into parallel rank-ordered sub-sequences), `--top` (rank at top; default appends to the bottom of its frame). Type/priority validation → exit 3. Store refusals: blank title ("title is required"), missing/short/long topic, nonexistent parent (exit 4). Labels are canonicalized, de-duplicated, sorted. Output: the summary line + breadcrumb. Note `runNew` registers no `--by`; creation attribution is not CLI-settable here.

**`lit followup`** (write; `cli.go:375-438`). Files a follow-up parented to a just-closed ticket. Flags: `--on` (required parent ID), `--title` (required), `--description`, `--prompt`, `--type`, `--topic`, `--priority`, `--assignee`, `--labels`, `--top` — no `--parent`, no `--lane`. Blank `--on` or `--title` → usage error, exit 2; missing parent → exit 4. Defaults inherited from the parent: blank topic takes the parent's; blank description becomes "Follow-up surfaced at the close of `<id>`: `<title>`".

## Querying

**`lit ls`** (`cli.go:440-663`). Lists issues, rank order by default. Uniquely, it is not bound to the current workspace: `--at <store-dir>` opens any discovered store read-only (blank or `-`-prefixed value → usage error). Filter flags: `--status`, `--type` (single values), `--assignee`, `--search`, `--ids` (CSV), `--labels` (CSV, ALL must match), `--has-comments` (tri-state: only applied when the flag is present, so `--has-comments=false` selects issues *without* comments), `--include-archived`, `--include-deleted`, `--updated-after`/`--updated-before` (RFC3339), `--sort` (`field[:asc|desc]` CSV), `--columns`, `--format` (`lines`|`table`; anything else exit 3), `--limit`, and `--query`.

The `--query` language (`internal/query/query.go`) tokenizes with single/double quotes and accepts: `status:<state>`, `resolution:<res>`, `type:<type>`, `assignee:<v>`, `id:<v>`, `label:<v>`, `has:comments`, `sort:<spec>`, `limit:<n>` (non-negative integer), bare `archived`, bare `deleted`, `updated>=|>|<=|<|:<RFC3339>`, and free text as search terms. Query and flag filters merge: slices dedupe or append, archived/deleted OR, a positive query limit overwrites; conflicting has-comments or time bounds, and after > before, are errors (`query.go:31-74, 276-309`).

Default active-work filter: if after merging no statuses **and no resolutions** are set, statuses default to `[open, in_progress]` — so a resolution filter alone reaches closed issues (`cli.go:592-603`).

**`lit show <id> [--field ...]`** (`cli.go:850-891`). Wrong arity → usage error, exit 2. `--field` mode prints selected fields with no context: valid names `id, title, description, prompt, type, topic, priority, status, assignee, labels, rank, lane, created_at, updated_at`; one field prints the bare value, several print `name: value` lines in requested order; an unknown name errors before anything prints. Full-detail mode prints, in order: header (id, title, type, topic, priority, labels, archived, deleted); status+assignee(+resolution) for leaves or child progress counts for containers; `unblocks:` (live dependents); a parent block with its description; description; prompt; then group blocks `children`, `depends_on`, `blocks`, `redirect` (the close-redirect target), `related`, each omitted when empty — there is deliberately no siblings group; comments (`- [author] body`, newlines escaped); **no history** (that is `lit history`); and finally the epic-context block (`output.go:78-176`).

The **epic-context block** (`epic_context.go`): a container shows its own children's plan; a leaf inside an epic shows the parent's plan with itself marked `▶ … (you are here)`; other issues get nothing. Shape: `Epic: <id> — <title>`, `Why: <first non-blank description line>`, then per-child lines with a status marker — `[closed]`, `[in_progress]`, `[blocked-by <first-open-blocker>]`, or `[ready]` — plus `[lane: <key>]` when set, and a `Cross-epic dependencies:` block listing `Blocks externally` / `Blocked externally` edges for open dependencies crossing the epic boundary.

**`lit history <id>`** (`cli.go:898-912`). Prints id, title, then `history:` and per event `- [<actor> @ <local timestamp>] <action> <reason>` (an action-less event displays `update`), then per field change `    <field>: <from> → <to>` with `-` for empty sides.

**`lit children <parent-id>`**: fixed columns `id | state | title`, rank order (`issue_relations.go:124-138`).

**`lit orphaned [--assignee <u>]`** (`cli.go:790-848`): `in_progress` leaves untouched ≥ 6h, oldest first; rows are `id | state | topic | assignee | title | Last Update: <age>`; empty prints "No orphaned issues."

## `lit backlog` and `lit next`

**`lit backlog`** (read; `workable.go`, `backlog.go`). Flags: `--assignee`, `--type`, `--status` (`open`|`in_progress` only — `closed` is refused), `--labels`, `--limit` (applied after ordering), `--columns`. Any positional → usage error. Output: an eight-line fixed preamble explaining the ranked queue, an 80-char rule, then either `(backlog empty)` or numbered rows (`%2d. <columns>`) each followed by an indented context block: `epic:`, `blocked:` (non-dependency reasons only — `missing <field>`, `needs-design`; earlier-sibling gating appears in neither line), `depends on:`, `in_progress: <age>[ (ORPHANED)]`, the claim line when the row's lane is held or stale, and `unblocks:` (derived from the listed rows only). If any row has a rank inversion, a trailing warning tells the user to run `lit doctor --fix`. Claims here are visibility only — they block nothing.

**`lit next`** (read; `next.go`, `next_route.go`). Same filters minus `--limit`/`--columns`; the retired `--continue` flag gets a tailored refusal. Routing precedence over the canonically-ordered rows:

1. With a self identity and lanes currently held by this checkout: (a) first ready row in an own lane; (b) else the first ready open dependency of an own-lane row that is present in the row set ("on-path dependency"); (c) else, within the same epics, the first ready row in an *unclaimed* lane — announced as "continuing epic `<id>`: starting `<row>` claims `<lane>`"; (d) else an exhausted error naming the epics and any unclaimed on-path blockers ("no ready work in … — picking up other work is a deliberate re-focus, not a bare `next`"), exit 1.
2. Otherwise: the first ready row whose lane is unclaimed — "starting `<row>` claims `<lane>`". A stale lane is never routed into.
3. Else "no ready work", exit 1.

A served row prints the default columns plus indented `epic:`, `depends on:`, and claim lines (never `unblocks:`). `lit next` performs **no writes** — the "claims" announcement describes what a subsequent `lit start` would do.

## Mutating fields and rank

**`lit update <id>`** (`cli.go:923-1033`). Flags: `--title`, `--description`, `--prompt`, `--type`, `--priority`, `--assignee`, `--labels` (replaces the whole set), `--lane`, `--reason`, hidden `--by`. Only flags actually present are applied. `--status` is intercepted with guidance to use the transition verbs (exit 2). No field flag at all → "lit update requires at least one field flag", exit 1 (`--reason` alone does not count). One field-change event records all moved fields together.

**`lit rank <id> --top|--bottom|--above <id>|--below <id>`** (`cli.go:1035-1114`). Exactly one mode flag required — counted by presence, so `--top=false` still counts (exit 3 otherwise). **Frame substitution**: ranking a child of an epic moves the whole epic; the command reports "`<id>` is inside `<epic>`; ranked the epic `<epic>` instead, leaving its internal order unchanged", and similarly when the anchor resolves to its epic (`cli.go:1093-1105`).

**`lit rank set <id1> <id2> [...]`** (`cli.go:1121-1149`). Atomically stacks the named issues at the top in the given order (≥2 IDs required); the same frame substitution reporting applies, then "ranked N issues at top in order: …". The literal `set` after `rank` is the only subcommand-vs-ID disambiguation in the CLI, justified because real IDs always carry a prefix (`cli.go:1036-1042`).

## Transitions

Eight verbs — `start`, `done`, `close`, `open`, `archive`, `unarchive`, `delete`, `restore` — share one handler (`cli.go:1353-1448`). All take one issue ID (wrong arity → plain error, exit 1), `--reason`, and hidden `--by`. Extra flags per verb: `start` adds `--assignee` (env wins when set) and `--take`; `close` adds `--resolution` and `--of`. The other six accept no extra flags, so e.g. `lit done --resolution x` is an unknown-flag usage error.

Sequence: read the issue (missing → exit 4) → authorization (only `start` has any — the takeover gate below) → apply the change → dispatch the workflow occasion (status verbs only) → for `start`, print "claim transferred: `<old>` -> `<new>`" when the assignee changed hands → print the summary line → for `done`/`close`, print the **close adjacency block** (parent, in-play siblings, redirect target, related, and `unblocks:` — the still-live dependents) → breadcrumb.

**Close outcomes** (`cli.go:1325-1351`, shared with `bulk close`): `--resolution` is required (`duplicate|superseded|obsolete|wontfix`). `duplicate`/`superseded` require `--of <canonical-id>` ("closing as `<res>` redirects to a canonical ticket — name it with --of"); `--of` on a terminal resolution is refused. Store-side, the redirect target must exist, must not be the issue itself, and must not be deleted.

**Store refusals on every transition**: an archived or deleted issue rejects any status action ("cannot `<action>` archived or deleted issue"); an epic rejects any status action with the container error (chapter 01); `archive`/`unarchive` on a deleted issue are refused. **There is no from-state precondition**: the registry summary for `done` says "requires in_progress" (`register.go:327`), but no code path enforces it — `Store.Apply` checks no prior status, and a same-state transition is a silent no-op that records nothing and preserves an existing resolution (`store.go:1064-1130`, `status_states.go:134-161`).

**The `start` takeover gate** (`claims_takeover.go`). Before applying, `start` derives the issue's lane and the current claim standing: held/stale by self, or unclaimed → proceed silently; **stale foreign** → print the claim line plus "check for unmerged branches or PRs on this lane before building on it" and proceed; **fresh foreign** → non-interactive callers must pass `--take` or get "this lane is claimed and active; pass --take to confirm the takeover" (exit 1); an interactive terminal is prompted `take over this lane? [y/N]` (anything not starting with `y` → "takeover declined", exit 1). The claim line format: `claimed here[ (stale)]: <worktree-path> (<branch>)` when the holder resolves to a live local worktree, else `claimed: stream <8-char token> (elsewhere|stale)`, plus optional contested-by streams, age, and lane progress (`claims_render.go:23-44`). If local-checkout enumeration fails, a warning is printed and freshness alone governs.

## Relations, comments, labels

**`lit dep add --from <id> --to <id> [--type blocks|parent-child|related-to]`** (`dependency.go:23-66`). Default type `blocks`. Refusals in order: blank endpoints → usage (exit 2); bad type → bare error (exit 1); self-loop → error (exit 1; transitive cycles are **not** detected); for `blocks` only, both endpoints in the same epic → validation error (exit 3): "Do not set 'blocks' relationships between two issues in the same epic.  Use rank…" (two floating issues never trip this). Endpoints are swapped to storage orientation for `blocks` (chapter 01). Output lines: `<src> --blocks--> <dst>`, `--child-of-->`, `--related-to-->`.

**`lit dep rm`** — same shape, no self-loop or same-epic check; prints `ok`. **`lit dep ls <id> [--type ...]`** — one line per relation in CLI orientation; empty output for none.

**`lit parent set --child <id> --parent <id>`** / **`lit parent clear <child-id>`** (`issue_relations.go:73-122`): `set` prints the edge as `<child> --child-of--> <parent>`; `clear` prints `ok`.

**`lit comment add <id> --body <text>`**: blank body → "comment body is required" (exit 1); comment IDs are `cmt-<uuid>`; prints `<issueID> <commentID>`; no breadcrumb. **`lit comment rm <comment-id>`**: unknown ID → exit 4; prints the deleted pair (`cli.go:1464-1512`).

**`lit label add|rm <issue-id> <label>`**: normalizes through the label rules (chapter 01), prints the resulting full label set comma-joined. Reserved labels: `needs-design` blocks readiness; `focus` marks a focus-path goal (`issue_relations.go:28-71`).

## Bulk operations

`lit bulk <label|close|archive>` (`bulk.go`). Shared semantics: the operation applies to each ID **in order**; each success prints `<id> ok` to stdout; failures never reach stdout — they are collected into a partial-failure error (exit 1) whose message lists every `<id>: <err>`, with remediation "Re-run the command for only the failed IDs…".

- `bulk label <add|rm> --ids <csv> --label <name>` — refusal precedence: missing `--ids` (exit 3), then missing `--label` (exit 3), then unknown action.
- `bulk close --ids <csv> --resolution <res> [--of <id>] [--reason <t>]` — same outcome gate as `lit close`; one shared outcome/actor/reason applied to every ID. No workflow events, no adjacency block, no breadcrumb.
- `bulk archive --ids <csv> [--reason <t>]`.
- `bulk import` is retired (hidden): points to `lit backup restore --path <export.json>`, exit 3, and answers even outside a git repository.

## Import and export

**`lit export`** (read): writes the full store export as two-space-indented JSON to stdout — the interchange shape of chapter 01 (`cli.go:1514-1525`).

**`lit import --path <file>`** (write; `cli.go:1537-1661`). Format chosen by extension: `.yaml`/`.yml` → bulk create-or-update, anything else → JSON tree spec.

- *JSON tree*: an array of records with `local_id`, optional `parent` (a local_id), optional `depends_on` (local_ids), `title`, `type`, `topic`, `priority`. `--by` is refused here — tree imports always attribute creates to `links`. Output: `imported N issues` then `  <local> -> <real>` mappings (map order unspecified). Rollback on failure is best-effort; the doc comment tells callers to run `lit doctor` after a failed import.
- *YAML bulk*: one document per issue, `---`-separated; optional `local_id` for intra-file references; a document with `id` set **updates** that issue instead of creating; `parent` may name a local_id or a real ID. `--by` requires at least one update document. Output: `created N issues` with mappings, then `updated N issues` with IDs.

## Workspace metadata and shell support

**`lit prefix set <new-prefix> [--apply]`** (workspace-mode, no store; `prefix.go`). Without `--apply`, prints a preview; with it, writes `issue_prefix` to the workspace `config.json`. Existing issue IDs keep their old prefix; only new issues use the new one. An unchanged normalized prefix prints "(prefix unchanged)". Any invocation not starting with the literal `set` → usage error.

**`lit workspace`**: prints `workspace_id`, `issue_prefix`, `git_common_dir`, `storage_dir`, `database_path`, `dolt_repo_path`, `traces_dir` as `key: value` lines (`cli.go:1674-1697`).

**`lit completion <bash|zsh|fish>`** (`completion.go`): generates a completion script from the visible command tree (hidden and retired commands never appear; a synthetic `help` entry is added). Family trigger words are flattened across depths, unioning children when a word appears under two parents (e.g. `label` both top-level and under `bulk`). Wrong arity or unknown shell → usage as plain error, exit 1.

**Bare `lit` / `lit quickstart`** — the guidance printer; flags `--refresh`, `--eject[=LIST]`, `--force`, one optional topic. Refusals: more than one topic; `--refresh` with `--eject`; `--force` without `--eject`; a topic combined with any flag; an unknown topic (all exit 2). Full behavior in chapter 07.

## Retired commands and flags

Retired commands are hidden but dispatchable: they run nothing and return "the `<cmd>` command has been retired; `<guidance>`" → exit 3 (`register.go:449-453`):

| Command | Replacement guidance points to |
|---|---|
| `ready`, `queue` | `lit backlog` / `lit next` |
| `assign` | `lit update <id> --assignee <name>` |
| `ls-at` | `lit ls --at <store-dir>` |
| `overview` | `lit stores --counts` |
| `bulk import` | `lit backup restore --path <export.json>` |

Retired flags, intercepted at the shared parser: `--output` (anywhere), `--continue` (both exit 3), and `lit update --status` (exit 2 with transition-verb guidance).

## Recorded discrepancies and panics

Stated as facts of the current build:

- The help summary for `done` claims "requires in_progress"; no code enforces any from-state precondition (`register.go:327` vs `store.go:1073-1130`).
- Comparable wrong-usage refusals exit differently: family dispatch, transition arity, and `completion` arity are plain errors (exit 1), while per-command usage refusals are exit 2.
- Boolean flags written as `--flag value` swallow the following token (`splitArgs`).
- Seven functions panic on states the code treats as unreachable — unclassified readiness kinds, unmapped next-outcomes, unmapped transition occasions, unknown breadcrumb topics, unknown completion shells, bad registry nesting, impostor store actions (`readiness.go:86`, `next.go:99`, `workflow_events.go:106`, `quickstart_topics.go:64`, `completion.go:54`, `register.go:153`, `store.go:1236`).
