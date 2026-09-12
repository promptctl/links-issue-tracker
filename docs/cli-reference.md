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
| 6 | Nothing to hand back (`lit next` had no ticket for you), or nothing to do (the state the command asked for already holds) |
| 7 | Data corruption detected |

Code 6 is not a failure: the command ran correctly and changed nothing. It exists so a
caller looping `lit next` can tell "stop, there is nothing for you" from "lit is broken"
without reading the message, and it carries the same answer for a mutation that was
already satisfied — `lit done` on an epic whose children are all closed, where the epic
closed itself when its last child did and there is no transition left to make.

### Identity

Commands that record an actor resolve it from the `CLAUDE_CODE_SESSION_ID` environment
variable (producing an identity like `claude_<session-id>`). The `--assignee` flag on
lifecycle commands is a *fallback* for when that variable is unset — when both are
present, the environment wins.

This session resolution is claim-time only: it applies to `lit start`. Field-writing
commands — `new`, `followup`, `update` — honor an explicit `--assignee`
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

The guidance comes from workflows: user-customizable definitions bound to the
`work_finished` semantic event (or any label/state/event combination) inject their body
into the command's output. `done`'s post-close reminder above is the only definition
shipped as an embedded default today.

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

The `/next` skill for pulling the next ticket ships from the `lit` Claude Code plugin
(`.claude-plugin/marketplace.json` at the repo root), not written into the target
repo — see [Agent setup](agent-setup.md#4-install-the-next-skill-optional-recommended).

On a fresh clone, `lit init` detects whether the configured git remote already
carries `lit` ticket data and adopts it automatically, so the clone transparently
picks up the existing backlog (it prints `Pulled existing backlog from <remote>/<branch>`).
The store lives in `.git/links/dolt`, which `git clone` does not transfer, so init is
the place that makes "clone + init = my tickets are here" true. Adoption runs only when
the local store has no tickets of its own — a workspace with local work is never
overwritten — so it is also safe to re-run after a transient network failure.

---

## Working the queue

### `lit ready` / `lit queue` (retired → `lit backlog` / `lit next`)

Both workable views are retired. `next` (the single leaf to start) and `backlog`
(the ranked queue, blocked items inline) are the only named workable views; an
old `lit ready` or `lit queue` invocation returns a pointer to them (exit 3). The
retired presentations — ready's blocked-to-bottom re-sort and coaching preamble,
queue's terse pullable-only list — are dropped; `backlog` and `next` carry the
surviving intent.

### `lit backlog`

```text
lit backlog [--assignee <a>] [--labels <csv>] [--status open|in_progress] [--type <t>] [--all] [--limit <n>] [--columns <csv>]
```

Workable items in rank order with blocked items shown **inline**, so the shape of
the queue is legible. Use when grooming or re-ranking. A `focus` label narrows which
rows the view answers over (see `lit label add` / `lit label rm`), and each run states
what it is listing on its own line, just before the separator that opens the row list
— so completeness is read there, not assumed here. `--all` lifts the scope for one run.

`--status` accepts exactly one of `open` or `in_progress` on the workable commands
(`backlog`, `next`); anything else — including `closed`, which could only ever match
nothing — is a usage error (exit 2) naming the legal values. This is narrower than
`lit ls --status`, which takes a set, and deliberately: a workable row can only be
`open` or `in_progress`, so the only set these commands could add is both of them,
which is already what they list when the flag is absent.

`--columns` takes the same vocabulary `lit ls` documents below and rejects an unknown
name the same way, exit 2. Here the rejection lands ahead of the sync-staleness
warning, so a rejected `lit backlog` prints nothing at all. `blocked` marks a ticket
that is not workable, for any of the four reasons the readiness classifier knows — a
still-open dependency, an earlier same-lane sibling still open, a missing required
field, or needs-design. The column agrees with that classification, and so with the
context block printed under the row, but the block splits the reasons across two
lines: `depends on:` names the still-open dependencies as concrete blocker ids, and
`blocked:` carries the other three. A ticket held up by nothing but a dependency
therefore prints `blocked` in the column with a `depends on:` line under it and no
`blocked:` line at all. `lit ls --columns blocked` reads that same verdict and so
answers the same question — except over a foreign store, where `lit ls --at <dir>`
evaluates three of the four reasons (see `lit ls` below); what stays particular to
`backlog` is the context block, which is where the reason is named.

### `lit next`

```text
lit next [--type <t>] [--status open|in_progress] [--labels <csv>] [--assignee <a>] [--all]
```

Prints the single next workable leaf to `lit start`, narrowed by the same filters as
`backlog` — so "the next workable bug" is `lit next --type bug`. `--limit` and
`--columns` do not apply to a single-row summary and are not accepted. `--continue`
is retired — claim routing already keeps a checkout in its own epic first,
unconditionally.

Selection is claims-first. A lane this checkout already holds comes back before
anything else: startable work and work already in flight there alike, the latter
handed back to resume rather than started fresh. Next comes a dependency outside
those lanes that gates one of them. After that the epic continues into any of its
other lanes no other checkout holds fresh, stale claims included: a stale claim is
provenance, not a hold, and taking that lane transfers it to this checkout. Only a
lane another checkout holds right now is passed over, and reaching one takes a
deliberate `lit start` on it, which asks to confirm the takeover or requires
`--take` when there is no terminal to ask. A checkout whose epic has open work it
cannot reach gets a diagnostic naming what blocks it, never a silent hop out of the
epic; a checkout holding no claims of its own starts straight at the global pool.
See design-docs/work-claims.md for the full precedence.

`next` is read-only. It claims nothing and starts nothing — `lit start` does both
— so where a pick's start would establish a claim, the line above the row says so
in the conditional and names that command: "run `lit start <id>` to claim lane
one of epic E". A lane of one is not named, because it is the ticket already on
the line. A pick that the epic-continuation step reached closes on a qualifier
saying so, which the same line from the global pool does not carry.

Both endings that hand back no ticket — the exhaustion diagnostic and an empty
result — exit 6 rather than 1, and their remediation names the deliberate act each
calls for. Neither tells you to retry: both answers are deterministic and repeat
unchanged until the work or the question does.

An empty result says which emptiness it is. A backlog with nothing in it answers
`no ready work`; one whose every row was passed over names those rows and why each
is not yours to start — held by another checkout, or gated by something unfinished;
and one whose focus path is stuck says that instead, naming what gates the path
itself alongside the rows the scope withheld off it, and pointing at
`lit next --all`, rather than quietly serving one of them as if it were next. So `lit next --status in_progress` can tell "you hold nothing"
from "your work is in another checkout's hands" instead of answering both the same
way.

### `lit orphaned`

```text
lit orphaned [--assignee <a>]
```

Lists `in_progress` issues with no recent updates — claimed work that went quiet and
needs someone to finish or release it.

### `lit ls`

```text
lit ls [--at <store-dir>] [--ids <csv>] [--search <text>] [--query <q>] [--status <csv of open|in_progress|closed>]
       [--type <t>] [--labels <csv>] [--assignee <a>] [--has-comments]
       [--updated-after <rfc3339>] [--updated-before <rfc3339>]
       [--include-archived] [--include-deleted]
       [--sort rank:asc,updated_at:desc] [--limit <n>] [--columns <csv>]
       [--format lines|table]
```

General-purpose listing, ranked by default. `--at <store-dir>` points `ls` at a
discovered store by its storage directory (a path from `lit stores`), read-only,
without depending on the current directory being a lit workspace — every filter,
sort, column, and format below applies to that foreign store, with one narrowing:
the `blocked` column evaluates three of its four reasons across a store boundary,
described with that column below. This is the folded-in
former `lit ls-at`; an old `lit ls-at <dir>` invocation returns a pointer to
`lit ls --at <dir>`. `--search` matches title and description
text; `--query` is a compact query language combining filters and text (e.g.
`status:in_progress type:task has:comments login`). It is a strict superset of the
discrete filter and list-shaping flags: every flag above has an equivalent token, so
`--query` alone can express any filter. The token spellings are `status:` (e.g.
`status:closed,in_progress`, comma-separate multiple states),
`resolution:`, `type:`, `assignee:`, `id:`, `label:`, `has:comments`,
`updated>=`/`updated<=`, `sort:` (e.g. `sort:rank:asc`, comma-separate multiple keys),
`limit:` (e.g. `limit:5`), and the bare keywords `archived` and `deleted` (the
`--include-archived` / `--include-deleted` equivalents). Any bare word that is not a
recognized token is a search term. Archived and deleted issues are hidden unless
explicitly included. Output-shaping flags (`--columns`, `--format`) have no token —
they are not filter concerns.

`--status` takes a set, not one state: `--status closed,in_progress` and the
repeated `--status closed --status in_progress` are the same request, and both
return one listing spanning both buckets in a single ordering rather than two
listings the caller merges by hand. The query term widens the same way
(`--query "status:closed,in_progress"`), because the flag and the token share one
parser. Every member must be a legal state — a typo anywhere in the set, or an
empty value, is an error, never a silently narrower listing.

`--columns` projects a chosen subset, default `id,state,topic,title`. Beyond the
issue's own fields (`id`, `state`, `type`, `topic`, `priority`, `rank`, `title`,
`assignee`, `labels`, `created_at`, `updated_at`) two opt-in columns are computed
rather than read off the row, and from different places: `parent` (the parent/epic id
from the canonical graph, `-` if none) and `blocked` (`blocked` when the readiness
classifier says the ticket cannot be pulled, else `-`). That classifier is the one
`lit backlog --columns blocked` reads, over all four reasons it knows — a still-open
dependency, an earlier same-lane sibling still open, a missing required field,
needs-design — so the cell means the same thing on both commands. Across a store
boundary it narrows by one reason: under `--at <dir>` readiness is
**store-intrinsic**, because a discovered store carries no repo root to load a
`required_fields` policy from (the same caveat `lit stores --counts` carries), so
the missing-required-field reason cannot fire there. A `-` from `--at` is
authoritative about the other three reasons and silent about that one. What `ls` reports
is the fact, never which of the four reasons applies: it is a flat projection, one
cell per column and no context block, so a reader who needs the why runs
`lit backlog`, where the `depends on:` and `blocked:` lines name it. The two
commands render different amounts of one verdict; neither holds a second opinion.
And because the cell is a verdict about the row rather than a property of the
listing, `ls` prints `blocked` on rows the workable commands never list at all — a
closed ticket, an epic, or an archived or deleted one surfaced by
`--include-archived` / `--include-deleted` — since `ls` lists whatever you filtered
for. On those rows the cell still answers only what the classifier says; whether
anything can act on the ticket is what `state` reports, which is why such a row
reads `open+archived` beside its verdict. `rank` prints
the issue's own rank string, the key `ls` orders by. Cost follows the projection: the
default and any projection of issue fields alone load nothing, `parent` costs the
relation-graph query, `blocked` costs the annotation pipeline. A name outside that
set is a usage error (exit 2) that quotes the offending word and lists the valid
columns — the same list the flag's `--help` prints — and the command exits before
any row is fetched or printed. Watch for near-misses: the status column is spelled
`state`, `lane` is a `lit show --field` name but not a column, and `description` and
`prompt` are multi-line, so they are read with `--field` rather than projected into
a table.

### `lit show`

```text
lit show <id>
```

Full detail for one issue: description, status, labels, comments, history. History
entries include the actor and a human-readable timestamp in the user's current timezone.
For an issue inside an epic, also prints the epic plan — siblings in rank order with
status and any cross-epic dependencies. Exits 4 if the ID doesn't exist.

---

## Creating and editing issues

### `lit new`

```text
lit new --title <text> --topic <slug> [--type task|feature|bug|chore|epic]
        [--description <text>] [--parent <id>] [--lane <key>] [--priority normal|urgent]
        [--labels <csv>] [--assignee <a>] [--prompt <text>] [--top]
```

Creates an issue and **prints its generated ID** — capture it; IDs are not guessable.
`--topic` is required and immutable: a 1–2 word slug naming the stable area of work.
New issues append to the bottom of their frame — the bottom of the epic named by
`--parent`, or of the top-level order without one — so filing work records it rather
than promoting it, and a batch authored in order keeps that order with no flag. `--top`
places the issue at the front of the agenda for the tickets that mean it; `lit rank`
moves one afterwards. With `--parent`, the child's ID becomes `<parentID>.<hash>`. `--lane`
partitions an epic's children into parallel rank-ordered sub-sequences: a shared lane
serializes, distinct lanes parallelize. `--prompt` stores a reusable agent prompt for
the work the issue captures.

`--priority` takes the word every read surface prints — `normal` or `urgent`, as
`lit show` and the issue rows render it — so a priority you read back can be pasted
straight into the flag that sets it. The numeric spelling `lit export` writes and the
`lit import` payload carries (`0`, `1`) is accepted too, and the two are the same
value. Anything else is refused: the domain has exactly these two levels, so
`--priority 2` is an error rather than a silent coercion to `normal`.

### `lit update`

```text
lit update <id> [--title <text>] [--description <text>] [--prompt <text>]
           [--type <t>] [--priority normal|urgent] [--assignee <a>] [--labels <csv>]
           [--lane <key>] [--reason <text>]
```

Field-level edit of an existing issue. `update` does **not** change status: the
transition verbs (`lit start` / `lit done` / `lit close` / `lit open`) are the single
enforcer of the transition guardrails, so a `--status` flag is rejected with a pointer
to them. This closes the former `--status closed` back door, which recorded a
resolution-less `done` and bypassed `close`'s required outcome. `--labels` replaces the
full label set — use `lit label add`/`rm` for incremental changes. `--assignee` is taken
verbatim (no session-identity substitution); `--assignee ""` clears the field, returning
the issue to unassigned. `--reason` is recorded on the field-change event.

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
children, and earlier same-lane siblings, transitively — becomes the row set
`lit backlog` lists and `lit next` routes its global pool over. The path
re-derives as items close, and blocked path items stay blocked: the scope
decides which rows a view answers over, never whether one is ready.

Focus narrows membership and leaves ordering alone — within the scope, rank is
still the only order, so `lit rank --top` reaches the top of the view. Pass
`--all` to either command to ignore the scope for one run. Two things the scope
deliberately does not reach: `lit next` still serves work in a lane this
checkout already holds even when it is off the path (your own work in flight
stays yours), and the cross-project counts in `lit stores --counts` stay
whole-project.

### `lit followup`

```text
lit followup --on <closed-id> --title <text> [--description <text>] [--topic <slug>]
             [--type <t>] [--priority normal|urgent] [--assignee <a>] [--labels <csv>]
             [--top]
```

Files a follow-up parented to a just-closed ticket — the way to capture work surfaced
during a ticket while context is fresh. Inherits `--topic` from `--on` when omitted;
the description defaults to a reference back to the source ticket. Like `lit new`, it
appends to the bottom of the parent's children unless `--top` says otherwise.

### `lit rank`

```text
lit rank <id> --top | --bottom | --above <other-id> | --below <other-id>
```

Moves one issue in the rank order. Exactly one placement flag is required.

Edge placement (`--top`/`--bottom`) moves the issue to an end of *its own
frame* — the epic that contains it, or the top-level order if nothing does.
`lit rank <child> --top` makes that child first among its siblings; it does not
move it, or its epic, anywhere in the queue at large, and it leaves every issue
outside the epic exactly where it was. The output says which frame the move was
scoped to whenever that frame is an epic. An issue that already holds the end
it was sent to is reported as such and nothing is written — an unchanged order
is never printed as a successful move. To promote a ticket out of its epic's
lane, rank the epic instead, or use `--above`/`--below` against an outside
issue, which resolves to the epic and carries the whole lane with it.

Relative placement (`--above`/`--below`) operates between *peers*: two siblings
inside the same epic, or two top-level items. When the named issue and the
anchor live in different epics (or one is standalone), the request is resolved
to the comparable pair — ranking against an epic's child behaves as ranking
against the epic itself, and ranking a child against an outside issue moves its
epic, never reordering anything inside an epic. The output states the
resolution whenever it substitutes an epic for a named issue. Ranking an issue
relative to its own epic (either direction) is an error.

Naming a deleted issue is an error in every form of the command, including
`lit rank set`: rank is a position among the issues a listing shows, and a
deleted issue holds none. Restore it first (`lit restore <id>`) and rank it
after.

### `lit rank set`

```text
lit rank set <id1> <id2> [<id3> ...]
```

Establishes absolute order across N issues atomically by stacking them at the
top of the representatives' own frame: `id1` becomes topmost, `id2` ranks just
below, and so on. Either every assignment applies or none does.

The anchor is that frame's top, not the workspace's. Naming several children of
one epic leads *that epic's children* and moves nothing outside it — the same
frame-local rule `--top`/`--bottom` follow. The summary line names the frame the
stack landed in (`ranked 3 issues at the top of <epic> in order: …`, or `at the
top of the backlog`), because the top of an epic's children is not the top of
the queue and an unqualified "at top" reads as the latter.

The same peer rule as relative placement applies: each named ID is resolved to
its representative in the comparable frame, so naming an epic's child alongside
outside issues ranks the epic, never reordering anything inside it. Every
substitution is reported in the output. Two requests are rejected as
incoherent: naming an issue together with its own epic (either direction), and
naming two issues that resolve to the same epic — their relative order is
internal to that epic and cannot be set against outside issues (run `rank set`
among the siblings instead).

### `lit assign` (retired → `lit update --assignee`)

Reassigning is a single-field write, so it is folded into `update` rather than
kept as its own verb:

```text
lit update <id> --assignee <new-assignee> [--reason <text>]
```

`--assignee` is taken verbatim (no session-identity substitution); `--assignee ""`
clears the field. An old `lit assign` invocation returns a pointer here (exit 3).

---

## Lifecycle transitions

All transitions share one shape, and each verb accepts only the flags its
action consumes — an inapplicable flag is an unknown-flag parse error (see
[Transition guidance](#transition-guidance) and [Identity](#identity) for
guidance and `--assignee` semantics):

```text
lit <verb> <id> [--reason <text>]
```

The verbs are the single enforcer of the transition guardrails — status is not
reachable through `lit update` (its `--status` flag is rejected with a pointer
here). `--help` presents the two groups apart: the high-traffic **status
lifecycle** under *Agent Operations*, and the low-traffic **retention** quartet
under *Issue Retention*, so the core verbs stand out.

Status lifecycle (Agent Operations):

| Command | Transition | Flags | Notes |
|---------|-----------|-------|-------|
| `lit start` | any state → `in_progress` | `[--assignee <fallback>] [--take]` | Claims the issue and assigns it to you; from `closed` it is a reopen-and-claim. `--take` confirms taking over a lane another checkout holds fresh, and is how a caller with no terminal to prompt does it. |
| `lit done` | any non-closed → `closed` | | Success path; prints post-close capture guidance. Transitions are target-state: the verb names the destination, whatever the current status. |
| `lit close` | any non-closed → `closed` | `--resolution <duplicate\|superseded\|obsolete\|wontfix> [--of <canonical-id>]` | Closing without finishing; `--of` names the canonical ticket for the redirecting resolutions (required for those, unrepresentable otherwise). |
| `lit open` | any non-open → `open` | | Returns the issue to the backlog; ownership (assignee) is untouched — only `start` rewrites it. |

Retention (Issue Retention):

| Command | Transition | Flags | Notes |
|---------|-----------|-------|-------|
| `lit archive` / `lit unarchive` | set / clear the archived flag | | Archived issues hide from listings. |
| `lit delete` / `lit restore` | set / clear the deleted flag | | Soft delete; `restore` brings it back. |

---

## Dependencies and structure

### `lit dep add` / `lit dep rm` / `lit dep ls`

```text
lit dep add --from <id> --to <id> [--type blocks|parent-child|related-to]
lit dep rm --from <id> --to <id> [--type <t>]
lit dep ls <issue-id> [--type <t>]
```

Manages relationship edges. `--from`/`--to` are required for `add` and `rm`; there is
no positional form. The default type is `blocks`, where `--from` is the blocker and
`--to` is the blocked issue. `blocks` edges are not allowed between two issues in the
same epic — within an epic, rank is the ordering signal. `related-to` is symmetric
annotation with no scheduling effect.

### `lit parent set` / `lit parent clear`

```text
lit parent set --child <id> --parent <id>
lit parent clear <child-id>
```

Manages epic membership. `--child`/`--parent` are required for `set`; there is no
positional form. Epics contain children; an epic's completion is derived from
its children rather than tracked as its own status.

Parent-child is one of the relation edges `lit dep` manages (`--type parent-child`);
`parent` is the ergonomic face over that same store write, and both render the
edge identically (`<child> --child-of--> <parent>`). Use whichever reads better —
they are two faces of one edge, not two writers.

### `lit children`

```text
lit children <parent-id>
```

Lists an issue's children in rank order — the ergonomic ranked read of the
parent-child edge that `lit dep ls <parent> --type parent-child` also exposes as
raw incident edges. Kept as a convenience distinct from the raw edge list.

---

## Bulk operations

### `lit bulk label` / `lit bulk close` / `lit bulk archive`

```text
lit bulk label <add|rm> --ids <csv> --label <label>
lit bulk close --ids <csv> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]
lit bulk archive --ids <csv> [--reason <text>]
```

Apply one label edit or lifecycle transition across many issues in one call.
`bulk close` takes the same resolution flags as `lit close` — every closed
issue records the shared outcome, so a bulk close cannot produce the
resolution-less close the close command itself forbids.

Each successful item prints `<id> ok` to stdout (the data channel carries
results only). If any item fails, the per-item errors go to stderr and the
command exits non-zero — so `lit bulk close --ids a,b,c && next-step` does **not**
run `next-step` on partial or total failure. Successful items are still applied;
re-run the command for only the failed IDs after addressing each error.

### `lit import`

```text
lit import --path <tree-spec.json>
lit import --path <bulk-file.yaml>
```

The one home for bulk-ingesting issues from a file: `--path` accepts either of two
formats, chosen by the file's extension. `.json` (or any extension other than
`.yaml`/`.yml`) is the JSON **tree spec** below — bulk-*create* only, records wired
by an opaque `local_id`. `.yaml`/`.yml` is the bulk YAML format below —
create-**or**-update, selected per document by whether it carries a real issue `id`.
One command, one mental model for "create/update many issues from a file"; picking
the file's extension is the only thing that varies, not a separate verb or flag.

`lit bulk import` is retired. It loaded a JSON **export** — the same restore that
`lit backup restore --path <export.json>` already owns — so it was a duplicate
under a name that also collided with tree-spec `import`. Use `lit backup restore`
to load an export; an old `lit bulk import` invocation returns a pointer there.

#### JSON tree spec (bulk create)

```json
[
  {"local_id": "epic-x", "title": "Build X", "type": "epic", "topic": "x", "priority": 0},
  {"local_id": "task-1", "parent": "epic-x", "title": "Design", "type": "task", "topic": "x", "priority": 0},
  {"local_id": "task-2", "parent": "epic-x", "depends_on": ["task-1"], "title": "Build", "type": "task", "topic": "x", "priority": 0}
]
```

An array of records. `local_id` is opaque to the store — it exists only to let a
later record's `parent`/`depends_on` name an earlier one before real IDs are
minted — and is replaced by the generated issue ID at create time (returned in
the printed `local_id -> real_id` mapping). Records are created in dependency
order; a cycle or a `parent`/`depends_on` naming no record in the file is
rejected before anything is created. `title`, `type`, and `topic` are required
on every record. On any failure mid-import, already-created records in this
call are best-effort rolled back (transitioned to deleted); run `lit doctor`
afterward if the error reports a leaked ID rollback couldn't clean up.

#### YAML bulk file (create or update)

```yaml
local_id: epic-x        # optional; lets a later document's parent/depends_on name this one
title: Build X
type: epic
topic: x
---
title: Design
type: task
topic: x
parent: epic-x           # a local_id above, or an existing real issue ID
---
id: existing-issue-7      # present => update that issue instead of creating
title: Renamed
labels: [reviewed]
```

One file, multiple YAML documents separated by `---`, one issue per document.
Whether a document creates or updates is decided entirely by the presence of
`id` — never a flag:

- **No `id`: create.** `title`, `topic`, and `type` are required (same as `lit new`);
  `priority`, `assignee`, `labels`, `lane`, `description`, and `prompt` are optional.
  `local_id` (optional) and `parent`/`depends_on` work exactly as in the JSON tree
  spec above, with one addition: `parent`/`depends_on` may also name a real,
  already-existing issue ID (not just a `local_id` in this file) — e.g. to bulk-add
  several new children under an epic that already exists. A name that matches
  neither a `local_id` in the file nor a real issue is an error.
- **`id` present: update.** The document is a patch applied to that existing
  issue via the same field set `lit update` exposes: `title`, `description`,
  `prompt`, `type`, `priority`, `assignee`, `labels`, `lane`. At least one of
  those must be set. `reason` (recorded on the field-change event) is
  optional annotation, not itself a change — a document with only `id` and
  `reason` is rejected as having nothing to update, the same as `lit update
  --reason "..."` with no field flags. `topic`, `parent`, `depends_on`, and
  `local_id` **cannot** appear on an update document — topic is immutable,
  and reparenting/dependency wiring are `lit parent set`/`lit dep add`'s job,
  not `import`'s. An `id` that matches no existing issue is a hard error —
  never a silent create.

A mixed file (some documents creating, some updating) is legal. Duplicate `id`s
or duplicate `local_id`s across documents in one file are rejected, as is any
unknown YAML field. Rollback on a mid-batch failure only ever undoes *creates*
made in that same call; an update that already succeeded is left applied — there
is no "prior state" to roll an update back to.

This is additive to `lit import`'s existing JSON tree spec, not a replacement:
JSON stays create-only and tree-shaped; YAML adds update-by-id and is flat
(dependency wiring is opt-in per document via `parent`/`depends_on`, not the
file's whole shape). Both are `lit import --path <file>`; only the extension
changes which one runs.

---

## Sync and data safety

### `lit sync`

```text
lit sync status
lit sync remote ls
lit sync fetch [--remote <name>] [--prune] [--verbose]
lit sync pull  [--remote <name>] [--verbose]
lit sync push  [--remote <name>] [--force] [--set-upstream] [--verbose]
lit sync compact [--full]                                       # reclaim local storage; needs no remote
lit sync reconcile                                              # run the field-aware reconcile; surface any prose divergence
lit sync reconcile resolve --resolve FINGERPRINT=TEXT …         # finalize with the agent's merged text
lit sync reconcile abort                                        # leave the clone diverged for now
lit sync reconcile combine                                      # unrelated histories: union both backlogs, keeping every issue
lit sync reconcile take local|remote [--owner-approved TOKEN]   # unrelated histories: adopt one side wholesale — DESTRUCTIVE, refuses without owner approval
```

Mirrors issue data through git remotes so one backlog is shared across clones — see
[Sync and remotes](dolt-remote-sync.md). `pull`/`push` default the remote to the
upstream remote, then to the single configured remote. A merge conflict exits 5.

`compact` reclaims local storage and contacts no remote, so a solo workspace can
run it. Two depths: the default collects recent history, while `--full` also
rewrites the archived generation, which is the only way to reclaim what earlier
passes left behind — at a cost proportional to the whole store rather than to
recent activity. You rarely need to run it: a mutating command compacts on its
own once the store's footprint warrants it, and `lit sync push` picks the depth
its own accounting calls for. Run it explicitly to reclaim on demand, or to
schedule maintenance on a workspace that goes long stretches without pushing.

`reconcile` merges a diverged clone into linear history with the field-aware
engine. When both sides rewrote the same free-text field (`title`, `description`,
or `agent_prompt`) the engine cannot pick a winner, so `lit sync reconcile`
prints `base`/`ours`/`theirs` for each field and exits 5. Each field's heading
names the issue, the field, and a fingerprint, and the ticket text below it is
printed as quoted data, not as instructions. The calling agent merges both intents
into one text and supplies it via `lit sync reconcile resolve --resolve
'FINGERPRINT=<merged text>'` (one `--resolve` per pending field, all in one
command — copy each fingerprint from its field's heading). The fingerprint is 12
lowercase hex characters computed over the issue id, the field, and the three
texts, so it is the whole address and the command never repeats the issue id:
the id may have been written on another machine, and nothing constrains its
characters. A `--resolve` value whose part before the first `=` is not a
fingerprint is a usage error and exits 2. The pending state is
re-derived live and never persisted; the fingerprint pins each merge to the exact
conflict it was made against, so a partial or stale resolution (including one
merged against a since-changed base/ours/theirs) is rejected and re-surfaced.
`abort` defers — the clone stays diverged and usable.

When two partitioned checkouts started the same lane before either saw the
other's push, a reconcile that lands in linear or combined history also names
the contest: it prints the lane, its current holder, and every other checkout
whose evidence is still live there — the same claim line `next`/`backlog`
render — right under the reconcile's own success line. This is reporting
only; routing and evidence are unaffected, and a reconcile with no contested
lane prints nothing extra. See `design-docs/work-claims.md` in the repository.

For **unrelated histories** (no common ancestor, so the field-aware merge has no
base), `combine` unions both backlogs with nothing dropped and needs no
approval, while `take local|remote` adopts one side wholesale and **permanently
discards the other side's unique issues** — run bare it exits 5 with a refusal
naming what would be destroyed and a one-time token; only the owner's explicit
go-ahead, asserted via `--owner-approved <token>`, runs it. See
[Sync and remotes](dolt-remote-sync.md) for the owner-notification hook
(`sync.owner_notify_cmd`) that carries divergence and push-failure events out
of band.

### `lit export`

```text
lit export
```

Writes a complete versioned JSON **data export** of the workspace to stdout (always
JSON; no flags) — the portable tree, `import`'s inverse. This is the export
`lit backup restore` reads. ("snapshot" is reserved for the filesystem-level
`lit snapshots` mechanism below, a different thing.)

### `lit backup`

```text
lit backup create [--keep <n>]
lit backup list
lit backup restore (--latest | --path <p>) [--force]
```

Rotating JSON **data-export** backups (`--keep`, default 20) — the data-recovery
family, wrapping `lit export`; distinct from the filesystem-level `lit snapshots`.
`restore` refuses to overwrite unsynced state without `--force`, and is the one
home for loading an export JSON (the retired `lit bulk import` pointed here).
`--path` accepts any export JSON — a backup file or a sync file; provenance does
not change behavior. You must name a source; there is no implicit default, and
`--latest` with `--path` is an error rather than a silent precedence.

### `lit snapshots`

```text
lit snapshots new [--label <text>]
lit snapshots list
lit snapshots restore <name>
```

Dolt filesystem-level database snapshots — the whole-database mechanism, coarser
and lower-level than the JSON `lit backup` data-export family, capturing the store
directory wholesale.

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

### `lit stores`

```text
lit stores [<root> ...] [--counts]
```

Discovers every lit store beneath the given roots (default: current directory). By
default it prints one canonical storage directory per line — the paths that feed
`lit ls --at <dir>`. With `--counts` it instead reports each discovered store's
ready / in-flight / blocked counts as a cross-project rollup (an aligned table with a
`TOTAL` row and clearly marked error rows for stores that could not be read) — the
folded-in former `lit overview`; an old `lit overview` invocation returns a pointer
here.

Every store opens strictly read-only, never contending with a project's own writer.
Readiness under `--counts` is **store-intrinsic**: a repo's own `required_fields`
policy is not applied across the boundary (a discovered store carries no repo root to
load it from), so counts can differ from that project's own `lit backlog` when it
configures `required_fields`. A `focus` label is the other source of divergence: it
narrows that project's `lit backlog` but never these counts (see `lit label add`).

### `lit prefix set`

```text
lit prefix set <new-prefix> [--apply]
```

Renames the cosmetic issue-ID prefix. Preview-first: without `--apply` it prints what
would change.

### `lit upgrade`

```text
lit upgrade [--to <vX.Y.Z>]
```

Atomically installs a `lit` binary, checksum-verified against the release manifest.
Bare `lit upgrade` resolves its own target — the latest published release — so every
upgrade suggestion lit prints runs exactly as printed; `--to` pins any specific
schema-compatible v-prefixed git tag instead, not necessarily a newer one (at the
running version it doubles as a reinstall of a damaged binary). A bare invocation
already at or ahead of the latest release is a no-op that names the version it kept —
the feed names the most recently created release, so a backport tag can be "latest"
while the installed binary is newer, and the default path never moves backward. A
target whose schema support ends below the workspace's applied version is refused
before anything is installed, with both schema ranges named. Upgrade never touches
the workspace schema: the installed binary migrates the workspace forward on its
next run if it trails.

### `lit downgrade`

```text
lit downgrade --to <vX.Y.Z>
```

Reverses schema migrations and atomically installs the prior `lit` binary for the given
v-prefixed git tag — the rollback path for a bad upgrade. Unlike `lit upgrade`, the
target is always explicit: a backward move stays deliberate, so there is no "latest"
to resolve.

### `lit version`

```text
lit version
```

Prints binary version, build metadata, and the supported schema version range. The
schema range is what determines whether this binary can open a given workspace. A
build stamped with a commit and date (every `just build` and `scripts/install.sh`
source build) also reports how long ago it was built, and warns when that's past
`internal/version.StaleBuildThreshold` — a nudge to rebuild before a landed fix goes
unnoticed.

---

## Guidance and tooling

### `lit quickstart`

```text
lit quickstart [work|new|update|done|doctor] [--refresh] [--eject[=LIST]] [--force]
```

Bare `lit quickstart` prints the router: the authoritative, always-current entry point
for the loop documented in [Agent setup](agent-setup.md), listing the topic subcommands
and the `lit next` → `lit start <id>` fastpath. Each topic prints task-specific
guidance at the moment of need: `work` (finding and starting work), `new` (creating
tickets), `update` (managing existing tickets), `done` (finishing work), `doctor`
(troubleshooting). `--eject` copies the embedded default templates to the global
override path so you can customize them (`LIST` is comma-separated short names, e.g.
`quickstart,quickstart-work,agents,hook`; `--force` overwrites existing
overrides);
`--refresh` re-syncs managed repo assets and reports override drift without touching
overrides. Topics take no flags.

Mutation commands point back here at the moment of need: the text success output of
`new`/`followup` ends with a one-line breadcrumb at `lit quickstart new`, `start` at
`lit quickstart work`, `done`/`close` at `lit quickstart done`, and
`update`/`rank`/`label`/`parent`/`dep` at `lit quickstart update`.

### `lit workflows`

```text
lit workflows [show <id> | edit <id-or-point> | dry-run [--event <name>] [--label <name>]... [--enter <state>] [--exit <state>] [--issue <id>]]
```

See [Workflows](workflows.md) for the full format, semantics, and a worked example.
Bare `lit workflows` prints the lifecycle spine (every dispatched event, the three
built-in states with enter/exit, and any bound labels) annotated with the definitions
active at each point and their source layer; `show <id>` resolves one definition
fully. `edit <id-or-point>` scaffolds a project-layer override (an existing
definition's id) or a fresh definition (an event name, a state name optionally
suffixed `:enter`/`:exit`, or — falling through — a label), always prints its path,
and additionally opens it in `$EDITOR` when stdout is a terminal and `$EDITOR` is set.
`dry-run` explains a hypothetical occasion built from flags: which definitions would
fire, why, and the body each would inject — without anything actually firing.

### `lit completion`

```text
lit completion <bash|zsh|fish>
```

Writes a shell completion script to stdout. See
[Installation](introduction/installation.md) for where to put it.
