# Research: lit feature-surface inventory

Evidence base for [../design.md](../design.md), covering charter process steps
2 (feature survey) and feeding 3 (forward constraints). Compiled 2026-08-25 by
a read-only exploration agent from source at HEAD `0d5cd53`. Citations are
file:line at that commit. This is a findings record, not a design position.

Two framing facts up front:

- The whole store lives at `<git-common-dir>/links/` — i.e. **inside `.git/`**,
  never in the worktree (`internal/workspace/workspace.go:223`, `:284-294`).
  Nothing in the store is a tracked file. Data reaches remotes only through
  Dolt's `refs/dolt/data` ref pushed to the *code repo's* git remote
  (`internal/store/adopt.go:355-358`, `internal/workspace/workspace.go:113-124`).
- There is **no auth layer of any kind** today. Every command is gated only by
  `app.AccessMode` (read vs write), which selects a store-open contract and
  whether an identity token is minted — not a permission
  (`internal/app/app.go:36-60`).

## 1. Command surface

The registry is a table: `internal/cli/register.go:254-393` (`commandSpecs`).
Each row is `{Name, Summary, GroupID, Run, Subcommands, Hidden}`. Access mode
is carried per-row (`r.appCmd(app.AccessRead|AccessWrite, …)`) or
per-subcommand-row in a `commandFamily` table.

R = reads store, W = writes store (Dolt commit per mutation), **–** = never
opens the store.

### Group "bootstrap"

| Command | What it does | Store | Entities/fields touched |
|---|---|---|---|
| `lit init [--skip-hooks] [--skip-agents]` | Creates the store, or **adopts an existing remote backlog by full Dolt clone** if the remote advertises `refs/dolt/*`; installs pre-push hook; writes managed sections into repo `AGENTS.md`/`CLAUDE.md` | W (creates/replaces whole DB) | whole database; `config.json` (workspace_id, issue_prefix); repo files. `internal/cli/init.go:27-142`, `internal/cli/init_sync.go:87-159`, `internal/cli/agents_internal.go:67-82` |

### Group "operations"

| Command | What it does | Store | Entities/fields |
|---|---|---|---|
| `lit new --title --description --prompt --type --topic --parent --priority --assignee --labels --lane [--top]` | Create issue | W | `issues` (all cols), `relations` (parent-child), `labels`, `issue_events` ("created"). `internal/cli/cli.go:322-360`, `internal/store/store.go:607-704` |
| `lit followup --on <id> --title …` | Child issue parented to a just-closed ticket; inherits parent's topic, default description quotes **parent title** | W | same as `new`; reads parent Title/Topic. `internal/cli/cli.go:370-431` |
| `lit backlog [--type --status --labels --continue --assignee --limit --columns]` | Full ranked workable queue, blocked items inline | R | `issues`, `relations`, `labels`, `comments` (has-comments), config `ready.required_fields`. `internal/cli/workable.go`, `internal/cli/cli.go:709-772` |
| `lit next [--continue --assignee --columns]` | One workable leaf | R | same pipeline as backlog |
| `lit orphaned [--assignee]` | in_progress issues stale past threshold | R | `issues.updated_at`, status. `internal/cli/cli.go:785-827` |
| `lit ls [--at <dir>] [--status --type --assignee --search --ids --labels --has-comments --include-archived --include-deleted --updated-after --updated-before --query --sort --columns --format --limit]` | Query/list. `--at` opens a **foreign store by path**, outside the current workspace | R | `issues` (all filterable cols), `labels`, `comments`, `relations` (only when a relation column is projected). `internal/cli/cli.go:448-628`, filter SQL `internal/store/store.go:708-820` |
| `lit show <id> [--field a,b]` | Full detail view or bare field values | R | issue + relations + comments + children + siblings + depends_on + related + blocks + parent + redirect_target + events. `internal/cli/cli.go:845-886`, `internal/store/store.go:911` |
| `lit history <id>` | Per-field from→to event trail | R | `issue_events` + `issue_event_changes`. `internal/cli/cli.go:893-916` |
| `lit update <id> [--title --description --prompt --type --priority --assignee --labels --lane --reason]` | Field write. `--status` is **rejected** with a pointer to the verbs | W | `issues` fields, `labels`, `issue_events` + changes. `internal/cli/cli.go:918-1028`, `internal/store/store.go:1210` |
| `lit rank <id> --top\|--bottom\|--above <id>\|--below <id>` | Move fractional rank | W | `issues.item_rank`, `updated_at`; reads global rank order. `internal/cli/cli.go:1030-1114`, `internal/store/ranking.go:16-407` |
| `lit rank set <id1> <id2> …` | Absolute N-way order, atomically, via frame representatives | W | `issues.item_rank` across N rows + ancestor chains. `internal/store/ranking.go:74-168` |
| `lit start <id> [--assignee] [--reason]` | Claim → in_progress; assignee resolves to `claude_<sessionId>` when `CLAUDE_CODE_SESSION_ID` is set | W | status, assignee, `issue_events` (**establishing** for claims). `internal/cli/cli.go:1238-1249`, `:1312` |
| `lit done <id> [--reason]` | Success close | W | status, closed_at, event (**establishing**) |
| `lit close <id> --resolution duplicate\|superseded\|obsolete\|wontfix [--of <id>] [--reason]` | Close unfinished | W | status, closed_at, `resolution`, `redirect_target`, event. `internal/cli/cli.go:1270-1305` |
| `lit open <id>` | Reopen | W | status, event |
| `lit comment add <id> --body <text>` | Add comment | W | `comments` (body, created_by), event via workflows dispatch. `internal/cli/cli.go:1414-1437` |
| `lit comment rm <comment-id>` | Delete comment | W | `comments`. `internal/cli/cli.go:1440-1457` |
| `lit label add\|rm <id> <label>` | Labels | W | `labels`. `internal/cli/issue_relations.go:12-18` |
| `lit bulk label add\|rm --ids --label` | Fan-out labels | W | `labels`. `internal/cli/bulk.go:83-99` |
| `lit bulk close --ids --resolution [--of]` | Fan-out close | W | status + resolution |
| `lit bulk archive --ids` | Fan-out archive | W | `archived_at` |
| `lit ready`, `lit queue`, `lit assign` | **Retired**, hidden but dispatchable; return a pointer error only | – | `internal/cli/register.go:296-301`, `:325`, `:449` |

### Group "structure"

| Command | What it does | Store | Entities/fields |
|---|---|---|---|
| `lit parent set\|clear` | parent-child edge | W | `relations`. `internal/cli/issue_relations.go:20-26`, `internal/store/relations.go:400-455` |
| `lit children <parent-id>` | List children by rank | R | `relations` + `issues`. `internal/cli/issue_relations.go:124-138` |
| `lit dep add --from --to [--type blocks\|parent-child\|related-to]` / `dep rm` / `dep ls <id> [--type]` | Dependency edges | W/W/R | `relations`. `internal/cli/dependency.go:14-21` |

### Group "data"

| Command | What it does | Store | Entities/fields |
|---|---|---|---|
| `lit export` | Whole backlog as JSON (issues, relations, comments, labels, events, workspace_id) to **stdout** | R | everything. `internal/cli/cli.go:1464-1475`, `internal/store/import_export.go:25-50` |
| `lit import --path <x.json\|x.yaml>` | Bulk ingest. JSON = tree spec (create only); YAML = create-or-update by id selector | W | issues, relations, labels, events. `internal/cli/cli.go:1487-1614`, `internal/store/import_tree.go`, `internal/store/import_bulk.go` |
| `lit backup create [--keep N]` / `list` / `restore (--latest\|--path) [--force]` | Rotating JSON export snapshots on disk; restore **replaces the whole store from a file** | R / R / W | whole export. `internal/cli/backup.go:21-30`, `:104-175` |
| `lit snapshots new [--label]` / `list` / `restore <name>` | Filesystem-level copies of the Dolt directory | filesystem, under workspace+journal+commit locks | whole DB directory. `internal/cli/snapshots.go:18-27`, `:76-190` |
| `lit sync status\|remote ls\|fetch\|pull\|push\|reconcile\|__mirror-bg(hidden)` | Dolt-over-git mirroring | R/W | see §4. `internal/cli/sync.go:47-68` |
| `lit sync reconcile [resolve\|abort\|take local\|remote\|combine]` | Divergence resolution incl. agent-supplied prose merges | W | whole export, history. `internal/cli/sync_reconcile_cmd.go:32-40` |

### Group "retention"

`lit archive` / `unarchive` / `delete` / `restore` — W, set/clear
`archived_at`/`deleted_at` (soft; the row survives).
`internal/cli/register.go:336-343`, retention model
`internal/model/model.go:119-148`.

### Group "maintenance"

| Command | What it does | Store | Notes |
|---|---|---|---|
| `lit workspace` | Prints workspace_id, issue_prefix, git_common_dir, storage_dir, database_path, dolt_repo_path, traces_dir | – (no store open) | `internal/cli/cli.go:1624-1647` |
| `lit stores [roots…] [--counts]` | Discover lit stores under roots; `--counts` opens each **foreign** store read-only for ready/in-flight/blocked | R (many stores) | `internal/cli/stores.go:24-64` |
| `lit prefix set <p> [--apply]` | Cosmetic issue-ID prefix in `config.json` | – (config file only) | `internal/cli/prefix.go:21-80` |
| `lit doctor [--fix all\|integrity,rank]` | Health report; `--fix` runs Fsck repair + rank-inversion repair. Access mode is **dynamically** read vs write from the flag | R or W | `internal/cli/doctor.go:197-288`, `:221-241` |
| `lit hooks install` | Writes the managed pre-push section into `$GIT_COMMON_DIR/hooks/pre-push` | – | `internal/cli/hooks.go:31-70` |
| `lit lifeboat dump` | **Schema-agnostic raw dump of every table/row to stdout, read below the migration gate** | R (raw, no migrate) | `internal/cli/lifeboat.go:152-170`, `internal/store/rawdump.go:61-122` |
| `lit lifeboat recover [--mapping f.json]` | Dump → rebuild at baseline through a shape mapper → verify conservation → promote in place | W (whole DB swap) | `internal/cli/lifeboat.go:75-140`, `internal/store/recover.go`, `internal/store/verify.go` |
| `lit downgrade --to vX` | Runs *down* migrations then atomically installs an older binary | W | `internal/cli/downgrade.go:36-75`, `internal/store/downgrade.go:181` |
| `lit upgrade --to vX` | Installs a newer binary to operate a schema-ahead workspace | – | `internal/cli/upgrade.go:82` |
| `lit ls-at`, `lit overview` | Retired pointers | – | `register.go:371-374` |

### Group "guidance"

`lit quickstart [topic] [--refresh] [--eject[=LIST]] [--force]`
(renders/ejects templates, writes to the **global** config dir,
`internal/cli/cli.go:1677-1748`); `lit workflows [show <id>|edit <id>|dry-run]`
(`internal/cli/workflows.go:34-58`); `lit completion bash|zsh|fish`;
`lit version`. None touch tickets.

## 2. Data model

Canonical schema: `internal/store/schema_snapshot.sql` (generated;
drift-checked). Seven application tables.

### `issues`

| Column | Kind for encryption purposes |
|---|---|
| `id` varchar(191) PK | **structural + leaky**: `<prefix>-<topic>-<base36 hash>` for top-level, `<parentID>.<n>` for children. The hash is `sha256(topic\|title\|description\|creator\|createdAtNanos\|nonce)` (`internal/issueid/generate.go:41-46`). So **the topic slug is plaintext inside every id**, and the id is a (weak-preimage) commitment to title+description. Child ids leak the parent-child edge and the sibling count. |
| `title` text NOT NULL | **free text** |
| `description` text NOT NULL | **free text** |
| `agent_prompt` text NULL | **free text** (`Issue.Prompt`) |
| `status` varchar(32) | structural; CHECK constraint forces NULL for `epic`, one of open/in_progress/closed otherwise |
| `priority` int (CHECK 0..1) | structural |
| `issue_type` varchar(32) (CHECK task/feature/bug/chore/epic) | structural |
| `topic` varchar(191) NOT NULL | **semi-free** — a 1-2-word slug, required at create (`internal/store/store.go:622`), also embedded in the id, also searched by `--search` |
| `assignee` text NOT NULL | **identity-bearing free text** — by convention `claude_<sessionId>` or a human username |
| `created_at`, `updated_at`, `closed_at`, `archived_at`, `deleted_at` varchar(64) | structural (RFC3339 strings). archived/deleted are the *retention* axis; deletion is soft |
| `item_rank` text NOT NULL DEFAULT '' | **structural, global** — base-62 fractional index, indexed `idx_issues_rank` |
| `lane` text NOT NULL DEFAULT '' | structural — lane key partitioning an epic's children |
| `resolution` varchar(32) NULL (CHECK duplicate/superseded/obsolete/wontfix) | structural |
| `redirect_target` varchar(191) NULL (CHECK: only with duplicate/superseded) | structural (an id) |

Domain type: `internal/model/model.go:80-118`. Note `retention` and the
lifecycle are *sealed* Go values projected onto the
`archived_at`/`deleted_at`/`status` columns (`:119-165`, `:326-368`).

### `comments` — its own table, not a field

`id` PK, `issue_id` FK ON DELETE CASCADE, `body` text (**free text**),
`created_at`, `created_by` text (**identity**). Index
`(issue_id, created_at)`. Model `internal/model/model.go:587-593`.

### `labels`

PK `(issue_id, label)`, plus `created_at`, `created_by`. Two indexes including
`(label, issue_id)` — label *names* are a queryable dimension (`--labels`,
`bulk label`). Model `:595-600`.

### `relations` — the entire graph

PK `(src_id, dst_id, type)`, `type` CHECK in
`('blocks','parent-child','related-to')`, plus `created_at`, `created_by`.
Both endpoints FK CASCADE. Indexes on `(dst_id,type)` and `(src_id,type)`.
Note the parent-child edge is stored **here** with `src=child, dst=parent`
(`internal/store/store.go:675-687`), i.e. parentage is duplicated in the id
string *and* in this table.

### `issue_events` + `issue_event_changes` — the history/attribution spine

`issue_events`: `id` PK, `issue_id` FK CASCADE, `action` varchar(64) NULL,
`reason` text NOT NULL (**free text**), `actor` text NOT NULL (**identity**),
`created_at`, **`stream_id` varchar(64)**, **`workspace_id` varchar(191)**.
Written only through `Store.recordEvent` (`internal/store/store.go:1945-1982`).

`issue_event_changes`: PK `(event_id, field)`, `from_value` / `to_value` text —
**stringified old and new value of every changed field, including
title/description/prompt**. That is: the free-text history is duplicated here
in plaintext. Model `internal/model/model.go:606-613`, `:724-733`.

### `meta`

Key/value. Keys observed: `workspace_id`
(`internal/store/schema_reconcile.go:410`), `producer_binary_version`
(`internal/store/migration_runner.go:28`), legacy `schema_version` (deleted on
adopt, `:1270`), and **`last_sync_path` / `last_sync_hash`**
(`internal/store/store.go:579-605`) — `last_sync_path` is a **local filesystem
path written into the synced database**.

### `migration_quarantine`

`version` PK, `name`, `error_text`, `created_at` — failed-migration
bookkeeping.

### Wire form

`model.Export` (`internal/model/model.go:755-766`): `{version, workspace_id,
exported_at, issues[], relations[], comments[], labels[], events[]}`. v1
(`history[]`) still decodes (`:768-800`). This one struct is the export
command, the backup file, the sync-base file, the restore input, and the merge
unit.

### Free-text vs structural, summarized

- **Free text (ciphertext candidates):** `issues.title`,
  `issues.description`, `issues.agent_prompt`, `comments.body`,
  `issue_events.reason`, `issue_event_changes.from_value`/`to_value`.
- **Identity-bearing:** `issues.assignee`, `comments.created_by`,
  `labels.created_by`, `relations.created_by`, `issue_events.actor`.
  (`created_by` is literally the constant `"links"` for rows created by
  `CreateIssue` — `internal/store/store.go:627`.)
- **Structural, needed plaintext today:** `id` (and therefore topic +
  parentage), `item_rank`, `lane`, `status`, `priority`, `issue_type`,
  `topic`, the timestamps, `resolution`, `redirect_target`, all of
  `relations`, `labels.label`, `issue_events.action`,
  `issue_event_changes.field`, `stream_id`/`workspace_id`.

## 3. Identity & attribution

Three distinct identity notions, deliberately layered
(`design-docs/work-claims.md:315-343`).

**(a) Stream id — per-checkout, opaque, local-only.**
`internal/workspace/stream.go`. 8 random bytes → 13-char unpadded lowercase
base32 (`:22-38`, `:229-238`). Stored write-once at
`<private-git-dir>/lit-stream`, i.e. `.git/lit-stream` in the primary clone
and `.git/worktrees/<name>/lit-stream` in a linked worktree, so git creates it
with the worktree and deletes it with `git worktree remove` (`:13-21`).
Published atomically via temp-file + `os.Link` (EEXIST = someone else won;
never `rename`, which would replace an identity) (`:132-227`). A malformed
token fails **every** command in the checkout, read and write, and is never
self-healed (`:240-282`). Minted only on write access
(`internal/app/app.go:52-60`: `AccessRead → workspace.ReadStream`,
`AccessWrite → workspace.EnsureStream`).

**(b) Workspace id — per-store, opaque, NOT synced.** A UUID in
`<git-common-dir>/links/config.json` (`internal/workspace/workspace.go:21-25`,
`:160-176`; `LocationFromStorageDir` at `:279-292`). It is *not* in git and
does not travel with the Dolt data — but it **is written into rows and
files**: it is stamped on every event's `workspace_id` column, it is
`Export.WorkspaceID`, and it is mirrored into `meta.workspace_id`. It is also
the Dolt commit author (`internal/store/store.go:2758-2760`). So a clone gets
a *different* workspace id than the store it cloned, and both ids coexist in
the synced event table.

**(c) The attribution pair.** `model.Attribution{stream, workspace}` —
unexported fields, `NewAttribution` collapses a half-pair to zero
(`internal/model/model.go:631-684`). Set once per process by
`Store.AttributeTo(streamToken)` (`internal/store/store.go:397-399`), read off
the store inside `recordEvent` so no call site can forget it (`:1957-1969`).
Never backfilled: pre-feature events carry none, permanently, and read as
"derives no claim" (`internal/model/model.go:672-686`).

**(d) Claim derivation — pure, stores nothing.** `internal/claims/`.
`NewEvidence` partitions issues into `model.LaneID`s and files every event
under its issue's lane, **failing loudly if an event names an issue the caller
didn't supply** — closed tickets included
(`internal/claims/evidence.go:52-81`). `Derive(evidence, freshness,
localCheckouts)` applies the four-leg predicate in order 1,4,2,3
(`internal/claims/derive.go:37-116`). Only `start` and `done` establish a
claim; the other six actions and all field edits merely refresh
(`internal/claims/establish.go:36-56`). Freshness window is
`claims.freshness_window`, default `"24h"`, parsed as a duration string on
purpose (`internal/config/config.go:56-98`, `:230`). Local liveness prune:
`LocalCheckouts.Void` drops evidence whose workspace matches this store but
whose stream matches no live local worktree (`internal/claims/local.go:58-64`),
enumerated at the boundary by `App.LocalCheckouts()`
(`internal/app/claims.go:31-36`) over `workspace.LiveCheckouts`
(`internal/workspace/checkouts.go`).

**Wiring status — flag:** PRs #409–#412 landed identity, attribution,
derivation, and prune. Grepping the tree, **nothing in `internal/cli` or
`internal/store` calls `claims.Derive` yet** — the only non-test consumers are
`internal/app/claims.go` and tests. `next`/`backlog` routing on claims is not
yet implemented.

**The privacy invariant (normative).** `design-docs/work-claims.md:284-299`:
*only opaque discriminators enter the shared database* — no hostnames,
usernames, directory names, paths, or device details; resolution from
discriminator to physical context happens only at render time on the owning
machine. The doc itself raises a **standing flag** that "any attribution that
predates it — user-name-shaped actor fields, for example — deserves the same
review."

**Assignee identity contradicts that flag today.** `resolveIdentity` stamps
`claude_<CLAUDE_CODE_SESSION_ID>` into `issues.assignee` and into every
`actor` field whenever the env var is set, otherwise the raw `--by`/`$USER`
value (`internal/cli/cli.go:1167-1192`).
`design-docs/agent-identity-and-ownership.md:56-69` documents
`claude_<sessionId>` / `<tool>_<sessionId>` / bare-string-is-a-human as the
*canonical* shape. So the synced DB carries session ids and usernames in
`assignee`, `actor`, and `created_by`, in flat contradiction with the
opaque-discriminator invariant. `lit ls --assignee` filters on it
(`internal/store/store.go:746-759`).

## 4. Sync & history model

**One Dolt commit per mutation.** `Store.withMutation` →
`withStampedMutation`: acquire commit lock (flock, re-entrant via a context
marker) → `BeginTx` → `fn` → `tx.Commit` (stages into the working set) →
`commitWorkingSetOnce` (`DOLT_COMMIT`), the whole thing under transient-GC
retry with an explicit two-phase resume point
(`internal/store/commit_lock.go:116-170`). `commitStamp{Message, Date, Author,
AllowEmpty}` is the per-commit variability channel; zero value = ordinary
mutation (`:87-108`).

**Transport.** Dolt remotes are re-derived from **git** remotes before each
sync op — `syncDoltRemotesFromGit` maps each `git remote -v` fetch URL through
`store.GitBackedRemoteURL` and add/removes `dolt_remotes` rows to match
(`internal/cli/sync.go:762-790`, `internal/store/sync.go:174-189`). Data lands
in `refs/dolt/data` on the code repo's remote. `RemoteHasDoltData`
(`ls-remote <remote> refs/dolt/*`) is the authoritative "this remote carries a
backlog" signal (`internal/workspace/workspace.go:110-124`).

**Commands.** `sync status` (`internal/store/sync.go:628`), `remote ls`,
`fetch [--remote --prune --verbose]`, `pull`, `push [--set-upstream --force]`,
`reconcile`. `SyncPullState` is a closed set: up_to_date / fast_forwarded /
linearized / prose_pending / unrelated_histories / ahead / never_synced
(`internal/store/sync.go:41-72`).

**Linear history via reconcile.** `Store.reconcile`
(`internal/store/sync_reconcile.go:401`) runs entirely on a **scratch branch
pair** (`spine` + `read`) force-created at localHead; the data branch never
moves until one atomic reset at the end (`:815-866`). It exports
ours@localHead and theirs@remoteHead, three-way merges against the merge-base
export, and then **replays the folded local chain forward onto the remote
head, one commit per original commit, under each original commit's own
message, date, and author** (`foldStepper.step`, `:634-650`; `commitStamp`
carrying `Date`/`Author`). Each step is written as a **delta** —
`diffExports` computes per-table remove/add sets with cascade accounting,
`applyExportDelta` applies them in FK order
(`internal/store/export_delta.go:142-231`). `spineWriter` owns "what the spine
currently holds," seeded from a real read, so a delta can never be applied
against an invented predecessor (`internal/store/sync_reconcile.go:697-750`).

**So: reconcile rewrites history.** The local commits are re-authored onto a
new parent chain with fabricated content diffs (the projected merge at each
step, not the original diff), and a terminal marker commit carries the settled
truth. Scratch branches are swept if stranded (`:990-1058`).

**Field-aware merge policy.** `merge.ThreeWay` fans `ResolveIssue` across
issues and unions the append-only tables (`internal/merge/merge.go:41-108`).
`ResolveIssue` (`internal/merge/resolve.go:77-140`) is two-tier: whichever
single side moved a field off base wins; when both moved, a per-field policy
decides — `priority` → higher, `topic`/`lane`/`rank`/`assignee` → symmetric
tiebreak, `labels` → set merge, retention → per-flag with deletion dominating,
status → dominant-state join (closed > in_progress > open), close payload as
an atom.

**Free text is the one thing the engine refuses to decide.** `ProseField` =
`title`, `description`, `agent_prompt` (`internal/merge/resolve.go:17-21`).
Both-sides divergence records a `ProsePending{IssueID, Field, Base, Ours,
Theirs}` and the merge result becomes ungated: `Settled()` returns ok=false
while any prose is pending (`:46-52`, `internal/merge/merge.go:24-38`). The
reconcile then commits **nothing** and hands the conflict to the calling agent
(`internal/store/sync_reconcile.go:765-779`). The agent supplies merged text
via `lit sync reconcile resolve --resolve ID:FIELD:FINGERPRINT=TEXT`; the
fingerprint is `sha256(field‖base‖ours‖theirs)[:6]` and the resolution set
must be an exact bijection with the live pending set or the whole batch is
rejected (`internal/merge/resolve_prose.go:18-20`, `:62-100`). Escapes:
`reconcile take local|remote` (destructive, needs an owner-approval token —
see §5), `reconcile combine` (union, no base), `reconcile abort`.

**Merge tiebreak — uncertainty flagged.** `ResolveIssue` takes `oursWS,
theirsWS` and the tiebreak compares *workspace ids* so both machines pick the
same winner (`internal/merge/resolve.go:229-244`). But both exports are
produced by the *same* local `Store.Export`, which stamps `WorkspaceID:
s.workspaceID` unconditionally (`internal/store/import_export.go:49`), and
`exportAtCommit` just resets the read branch and calls `Export`
(`internal/store/sync_reconcile.go:1081-1090`). So at the reconcile call site
`oursWS == theirsWS` always, and the tiebreak falls through to the value
comparison at `resolve.go:238-242`. I did not find a path that supplies the
*remote's* workspace id. This may be intentional (value comparison is also
symmetric) or a latent defect; flagged rather than asserted.

**On-change mirror.** Default cadence is `on-change`
(`internal/config/config.go:137-141`, default set at `:227`): after every
*mutating* command, a detached background process is spawned — hidden
subcommand `lit sync __mirror-bg --parent-pid N`
(`internal/cli/sync_bg.go:22`, `:50-56`, `:128`), which waits for the parent
to exit, then opens its own store via `OpenSync` and pushes. Alternative
cadence `on-push` fires only from the managed pre-push hook. `sync.receive`
(default true) additionally runs a debounced background fast-forward receive
(`internal/config/config.go:103-112`, `internal/cli/sync_cadence.go:33`,
`:245-261`). Kill switch: `LIT_DISABLE_AUTO_SYNC`
(`internal/cli/sync_cadence.go:27`).

**First-clone adoption.** `lit init` probes the remote *before* creating an
empty store; if `refs/dolt/*` exists and the local store has no tickets, it
**clones the whole Dolt archive** (`DOLT_CLONE` against `refs/dolt/data`)
rather than fetching, with a 120s hard cap; a failed adopt hard-stops init
rather than creating a fresh store (`internal/cli/init_sync.go:24`, `:87-159`;
`internal/store/adopt.go:244-358`; `internal/cli/init.go:44-74`). A
pending-adopt marker file makes a half-finished adopt refuse every later store
open (`internal/store/adopt.go:344-350`, checked in `OpenSync` at
`internal/store/sync.go:129-134`).

**Other history-rewriting / whole-store-replacing operations:**

- `Store.ReplaceFromExport` — restore path, replaces every table from a JSON
  file (`internal/store/import_export.go:144-160`).
- `SyncResetToRemoteHead` (`internal/store/sync.go:408`),
  `SyncResolveUnrelated` take-local/take-remote
  (`internal/store/sync_unrelated_take.go:120-290`).
- Migrations: `migrate()` / goose runner, with a snapshot guard taking a
  recovery snapshot before DDL, plus quarantine of failed versions
  (`internal/store/migration_runner.go:274-330`, `:615-800`).
- Pre-goose `reconcileToBaseline` — reads and rewrites **all rows** of every
  table to bring an ancient shape forward, including
  `translateIssueHistoryToEvents` which synthesizes events from a legacy table
  (`internal/store/schema_reconcile.go:164`, `:541`).
- `lit downgrade` — down-migrations plus binary swap
  (`internal/store/downgrade.go:181-260`).
- `lit lifeboat recover` — dump/rebuild/verify/promote, a full directory swap
  (`internal/store/promote.go:88-160`).

## 5. Escape hatches & side channels

Ranked roughly by how completely they take plaintext out of the store.

1. **`lit export`** — whole database as JSON on stdout, read access only
   (`internal/cli/cli.go:1464-1475`). No redaction, no filter.
2. **`lit lifeboat dump`** — whole database as raw table/row JSON, **below the
   migration gate**, so it works on a store that `store.Open` refuses.
   Schema-agnostic by construction: no column allowlist exists
   (`internal/store/rawdump.go:14-42`, `:61-122`). The hardest surface to
   police, because it deliberately does not know what a column *means*.
3. **`lit backup create`** — writes an export JSON into
   `<storageDir>/backups/` and prunes (`internal/cli/backup.go:32-51`). Runs
   under **AccessRead** — writing a full plaintext dump to disk is not treated
   as a write.
4. **`lit backup restore --path <file>`** — ingests any `model.Export` JSON,
   provenance-blind by explicit design (`internal/cli/backup.go:70-79`,
   `:104-175`), and calls `ReplaceFromExport`. Also writes
   `<storageDir>/last-sync-base.json`, a second full plaintext copy
   (`:122-126`, `:164-168`).
5. **`lit snapshots new` / `restore`** — byte copies of the whole Dolt
   directory into `<storageDir>/snapshots/` (`internal/cli/snapshots.go:29-34`,
   `:126-190`), plus system-stamped migration/downgrade/reconcile snapshots
   produced automatically.
6. **`lit import`** — bypasses `new`/`update` entirely: YAML docs can create
   *or* update arbitrary issues by id, with `KnownFields(true)` as the only
   gate (`internal/store/import_bulk.go:16-77`); JSON tree spec creates whole
   subtrees (`internal/store/import_tree.go:14-58`). Both are single-command
   bulk mutation paths.
7. **`lit ls --at <store-dir>`** and **`lit stores --counts`** — open *other
   projects'* stores read-only by path, with no workspace of their own
   (`internal/cli/cli.go:448-480`, `internal/app/app.go:106-127`,
   `internal/cli/stores.go:45-51`).
8. **Trace directories** — `<storageDir>/traces/{automation,workflows,sync}/*.json`
   (`internal/trace/trace.go:20-24`). `sync` traces carry workspace_id,
   command, decision, status, **`reason`, `error` strings, and a metadata
   map** (`internal/cli/sync_trace.go:28-38`; error text injected at
   `internal/cli/sync_receive.go:264`, `internal/cli/sync.go:448`).
   `workflows` firing traces carry **issue_id and labels**
   (`internal/workflows/trace.go:29-38`). Traces live inside `.git/links/`
   and are never synced, but they are plaintext on disk and `lit workspace`
   prints the path for scripting (`internal/cli/cli.go:1639`).
9. **Owner-notify hook** — `sync.owner_notify_cmd` runs an arbitrary shell
   command with `LIT_NOTIFY_KIND/SUMMARY/REMOTE/BRANCH/REPO` in the
   environment (`internal/config/config.go:113-119`,
   `internal/cli/owner_notify.go:187-205`). `LIT_NOTIFY_REPO` is the repo root
   path. Summary text is lit-generated, but this is a general egress channel.
10. **Managed pre-push git hook** — runs `lit sync push` on every `git push`,
    and prints a warning line to stderr on failure
    (`internal/templates/defaults/pre-push-hook.sh`, installed by
    `internal/cli/hooks.go:34-60`).
11. **`meta.last_sync_path`** — a local filesystem path stored in the synced
    database (`internal/store/store.go:596`). Already a privacy-invariant
    violation independent of encryption.
12. **`.git/links/mirror.log`** — the background mirror's log
    (`internal/cli/sync_bg.go:27`).
13. **Issue ids themselves** — the topic slug is plaintext in every id and
    every child id encodes its parent; ids appear in every output, every
    relation row, every trace, and every branch name derived from a ticket.

## 6. Extension points / config

**Global config:** `~/.config/links-issue-tracker/config.toml` (or
`$XDG_CONFIG_HOME/...`), override env `LIT_CONFIG_GLOBAL_PATH`.
**Project config:** `<repoRoot>/.lit/config.toml`, override env
`LIT_CONFIG_PROJECT_PATH`. Project overrides global; layers merged in slice
order (`internal/config/config.go:169-215`, `:274-280`). Note `.lit/` is in
the *worktree*, so project config is a **tracked, shared, plaintext file**
while the store is not.

Keys (`internal/config/config.go:16-119`, defaults `:216-230`):
`logging.verbose`, `logging.file`, `init.install_hooks`,
`init.install_agents`, `migration.auto_apply`, `ready.required_fields` (also
accepted as bare `required_fields`, `:295-297`), `quickstart.soil_mode`,
`snapshot.retention_budget`, `sync.cadence` (`on-push`|`on-change`),
`sync.receive`, `sync.owner_notify_cmd`, `claims.freshness_window`.

**Workflows** — user-authored markdown+YAML-frontmatter guidance injected into
agent-facing output at declared lifecycle moments. Three layers, nearest wins
by ID: `<repoRoot>/.lit/workflows/`, `<globalConfigDir>/workflows/`, embedded
defaults (`internal/workflows/workflows.go:1-24`,
`internal/workflows/load.go:136-142`). Activation dimensions: labels / states
(enter|exit, **open string, not the lifecycle enum**) / events. The event
catalog is 10 named moments: `show_backlog`, `show_ticket`, `next_pulled`,
`work_started`, `work_finished`, `ticket_closed`, `ticket_reopened`,
`ticket_created`, `ticket_updated`, `comment_added`
(`internal/workflows/events.go:13-44`). Bodies are **injected text, never
executed** (`workflows.go:22-23`). `lit workflows edit` scaffolds/opens a
definition; `dry-run` explains a hypothetical.

**Templates** — embedded defaults under `internal/templates/defaults/`:
`quickstart.md`, `quickstart-{work,new,update,done,doctor}.md`,
`agents-section.md`, `pre-push-hook.sh`. `lit quickstart --eject` copies them
to the global config dir for override; `--refresh` rewrites the repo's managed
assets without touching overrides (`internal/cli/cli.go:1677-1748`,
`internal/cli/quickstart_topics.go`).

**Managed repo files** — `lit init` writes a marker-delimited section into
`AGENTS.md` and `CLAUDE.md`; lit owns only the text between markers
(`internal/cli/agents_internal.go:13`, `:67-82`).

**Env vars** as extension/override points: `CLAUDE_CODE_SESSION_ID`
(identity), `USER` (fallback actor), `LIT_DISABLE_AUTO_SYNC`,
`LIT_CONFIG_GLOBAL_PATH`, `LIT_CONFIG_PROJECT_PATH`, `LNKS_AUTOMATION_TRIGGER`
/ `_REASON` / `_TRACE_REF_FILE` (automation provenance, set by the pre-push
hook), `XDG_CONFIG_HOME`.

**Planned/signalled directions.** The only committed statement of future
direction found is `design-docs/project-intent.md:39-45`: *integrations
bridging agent-native and human-native tracking, e.g. a bidirectional Jira
state mapping* — explicitly "not yet committed work." The stated bias is
otherwise **toward less**: "the project is largely feature-complete; additions
earn their place" (`:36-37`). `design-docs/work-claims.md:396-408` lists open
edges: takeover confirmation shape, **opt-in user-chosen stream labels for
friendlier remote rendering** (relevant — a place where non-opaque data would
deliberately enter the shared DB), and the residual release gap.
`design-docs/agent-identity-and-ownership.md:77-90` defers a phase-2
process-liveness probe. `design-docs/README.md` documents the status
vocabulary; note it says **"when a design doc and the code disagree, the code
wins"** — except `work-claims.md`, which is named normative.

## 7. Strain points for an access-control layer

Each of these is a place where "this row is ciphertext to you" breaks
something that works today. Cited so the design doc can point at the exact
behavior.

**7.1 Claims are derived from evidence — encrypting the evidence deletes the
feature.** The claim predicate reads `issue_events.action`, `created_at`, and
the attribution pair, plus `Issue.InPlay()` for every lane member
(`internal/claims/derive.go:58-116`). If `action` or the attribution stays
plaintext, claims survive; if the *lane membership* is invisible (leg 1 needs
every member's status), a tier boundary silently makes a held lane read
"unclaimed." Worse: `NewEvidence` **hard-fails** when an event names an issue
that wasn't supplied (`internal/claims/evidence.go:64-67`) — precisely so a
partial read can't produce a wrong claim. An RBAC layer that filters rows *is*
a partial read, so it will hit that error rather than degrade. The design must
decide whether claims are computed over the full ciphertext-bearing row set
(possible, since only structural columns are read) or are scoped per tier.

**7.2 Reconcile needs plaintext free text — and hands it to the *calling
agent*.** `ProsePending` carries `Base`, `Ours`, `Theirs` — three full
plaintext versions of a title/description/prompt — out to the agent surface
for a semantic merge, and the answer comes back as
`--resolve ID:FIELD:FINGERPRINT=TEXT` on a command line
(`internal/merge/resolve.go:23-33`, `internal/cli/sync_reconcile_cmd.go:33`).
If a checkout cannot decrypt a field, it cannot reconcile that issue at all,
and the fingerprint (`sha256` over the three plaintexts,
`internal/merge/resolve_prose.go:18-20`) has to be redefined over ciphertext
or become uncomputable. Note the bijection rule: an *incomplete* resolution
set rejects the **entire batch** (`:62-100`) — so one undecryptable field
blocks reconcile for every other conflicted field too.

**7.3 The three-way merge is field-aware over the *whole* export.**
`ThreeWay` walks the union of all issue ids across base/local/remote and
unions all child tables (`internal/merge/merge.go:41-108`). A machine that
cannot read tier-B rows cannot merge them — and if it writes a merged export
without them, `diffExports` will compute *deletions* for every row absent from
`next` (`internal/store/export_delta.go:142-171`, `cascadeSurvivors` at
`:189-201`). Silent data destruction is the failure mode. Any tier-blind
client must be prevented from producing an export at all, or exports must be
tier-partitioned.

**7.4 Rank is one global fractional index, and rank operations read rows you
may not see.** `RankToTop` queries `ORDER BY item_rank ASC LIMIT 1` across all
live issues (`internal/store/ranking.go:22-24`); `rank set` resolves ancestor
chains and frame representatives across the whole tree (`:74-168`);
`smoothRanksIfNeededTx` rebalances neighbors. Composite/hierarchical rank
means an epic's position carries its children
(`design-docs/work-claims.md:36-38`). If a user cannot see the ticket
currently at the top, "rank to top" either fails or produces a rank that
isn't top. And `doctor --fix rank` repairs inversions across the whole set
(`internal/store/ranking.go:624-830`).

**7.5 Backlog/next need titles, and the query is SQL over plaintext.** The
workable pipeline lists issues, annotates, sorts by composite rank + priority
+ focus path, and renders `id/state/title` columns
(`internal/cli/cli.go:709-772`). Encrypted titles turn the backlog into a
list of opaque ids. Worse, several filters are SQL predicates over the
plaintext: `--search` is `LOWER(title|description|agent_prompt|topic) LIKE ?`
(`internal/store/store.go:798-805`), `--assignee` is `assignee IN (…)`
(`:746-759`), `--labels` is an EXISTS over `labels.label` (`:775-783`).
Ciphertext kills full-text search entirely unless titles stay plaintext or a
searchable-encryption scheme is adopted.

**7.6 Parent/child links cross tiers structurally, and the id encodes the
crossing.** Parentage exists in three places at once: the child's **id
string** (`parent.7`, `internal/store/issue_ids.go:44-72`), a `relations` row
with `type='parent-child'` (`internal/store/store.go:675-687`), and the FK
cascade. A private child of a public epic is visible-by-id no matter what; a
public child of a private epic reveals the private epic's id. The lane gate
reads the parent epic's **full, unfiltered child set** so a hidden sibling
still gates its lane-mates (`internal/cli/cli.go:739-744`) — deliberate, and
it means readiness for a visible ticket depends on invisible ones. Container
(epic) state is *derived from children* (`internal/model/model.go:150-165`,
`HydrateAllOf` at `:415`), so an epic's status is literally uncomputable
without reading every child.

**7.7 Every child table is `ON DELETE CASCADE` from `issues`, and delete is
soft-except-when-it-isn't.** `archive`/`delete` set timestamps on a surviving
row (`internal/model/model.go:119-148`) — so the row, its id, its rank, and
its relations remain readable. But `applyExportDelta` expresses a *changed*
issue as DELETE+INSERT, which cascades its comments/labels/relations/events
away and re-inserts them (`internal/store/export_delta.go:26-35`,
`:126-141`). Any per-row key or ciphertext envelope must survive that cycle
intact, and the cascade accounting must not treat "rows I couldn't decrypt"
as "rows that should be dropped."

**7.8 Migrations and schema reconcile read and rewrite every row.**
`reconcileToBaseline` and its steps (`internal/store/schema_reconcile.go:164`,
`:454`, `:541`, `:901-1200`) probe and rewrite whole tables, including
translating a legacy history table into `issue_events`. Goose migrations run
under a snapshot guard on every `store.Open` for a write
(`internal/store/migration_runner.go:274-330`). `verify.go`'s conservation
laws compare a rebuilt candidate against the **raw** dump — row counts, raw
id cells, rank permutation validity — and explicitly acknowledge that "a swap
between two same-typed free-text fields is undetectable without guessing"
(`internal/store/verify.go:30-42`). Under encryption every free-text column is
same-typed opaque bytes, so that ceiling gets much lower: the recovery gate
loses most of its ability to detect a mis-map.

**7.9 The lifeboat is a deliberate below-the-gate bypass.** `DumpRaw` reuses
the connection primitive but **never calls `migrate()`**, by design, so a
workspace the gate refuses can still be released
(`internal/store/rawdump.go:44-58`). Its whole type discipline is "there is no
known-columns list" (`:14-22`). Any RBAC enforcement that lives above the
migration gate is, by construction, not enforced here. The single largest
"explicitly exempt or redesign" decision in the inventory.

**7.10 Attribution already carries session ids and usernames in the synced
DB.** `assignee`, `actor`, `created_by` are free-text identity fields
(schema; `internal/cli/cli.go:1167-1172`), while the claims design mandates
opaque discriminators only and flags exactly these fields for review
(`design-docs/work-claims.md:284-299`). An RBAC design that binds capabilities
to identities must decide whether identity in the DB becomes a key-bound
opaque principal (consistent with the invariant) or stays human-readable
(consistent with today's `--assignee` filtering and `lit orphaned`).

**7.11 `--at` and `--counts` open foreign stores with no workspace and no
identity.** `app.OpenLocationForRead` reads the foreign store's `config.json`
for its workspace id and opens read-only, bypassing cwd git resolution
entirely (`internal/app/app.go:106-127`). There is no place in that path for a
per-project policy or key to be consulted — `lit stores --counts` even notes
it deliberately does *not* apply per-repo `required_fields`
(`internal/cli/register.go:366`). Cross-project reads need an explicit story.

**7.12 Two automatic, unattended write paths run outside any interactive
session.** The detached on-change mirror (`lit sync __mirror-bg`,
`internal/cli/sync_bg.go:50-56`, `:128`) and the debounced background receive
(`internal/cli/sync_cadence.go:66-190`) both open the store and push/pull
without a user present. Whatever key material signing or decryption needs must
be available to a detached process spawned from an arbitrary command — a hard
UX constraint under charter rule 3 (transparent to the user).

**7.13 The existing "owner approval" gate is a useful precedent — and shows
the gap.** `lit sync reconcile take local|remote` already refuses without an
approval token derived from the exact fork + side, voided if the backlog
moves (`internal/store/sync_unrelated_take.go:50-90`, `:169-175`), with
agent-facing text saying "do not self-approve; approval asserted without the
owner's explicit instruction is a false claim"
(`internal/cli/sync_take_approval.go:45`). This is an *advisory* gate enforced
by a token the same client computes — exactly the pattern that cryptographic
client-side enforcement would have to replace with something a malicious
client cannot mint.

**7.14 The `issues` CHECK constraints and the `status`/`issue_type` coupling
are enforced by the database.** E.g. `issues_status_check` forces
`status IS NULL` iff `issue_type='epic'`; `issues_redirect_target_check` ties
`redirect_target` to a redirecting `resolution`. Any scheme that encrypts
`issue_type` or `resolution` breaks these constraints, and they are
byte-compared by the drift canary (`internal/store/schema_snapshot.sql`
header). Adding columns means a numbered goose migration plus a regenerated
snapshot — the schema is deliberately hard to change quietly, which cuts both
ways for an encryption envelope.

## Things the inventory could not determine

- Whether the merge tiebreak's workspace-id comparison is ever reached with
  two *different* workspace ids (§4). No call path found that supplies the
  remote's id.
- Whether there is an open backlog ticket describing planned features beyond
  `project-intent.md`'s Jira note — no `lit` command was run, so the live
  backlog is unread.
- The exact retention/pruning of `<storageDir>/traces/` — the writer exists
  (`internal/trace/trace.go`) but no pruner was found; traces may accumulate
  indefinitely. Not verified.
