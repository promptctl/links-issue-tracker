# CLI: operations commands and the background sync engine

This chapter covers everything in `lit` that manages the workspace rather than individual issues: `lit init` and its remote-adopt decision, the `lit sync` command family, the automatic background sync engine (spawned mirror processes, inline receive, the compaction backstop, staleness banners, owner notifications), `doctor`, `upgrade`/`downgrade`, `backup`/`snapshots`/`lifeboat`, `stores`, `hooks`, `quickstart`, `version`, and the managed-file/template machinery those commands share. The store-level sync and merge algorithms these commands invoke are specified in `05-sync-merge-compaction.md`; this chapter is the CLI/process level.

## Shared dispatch, flags, and exit codes

### Entrypoint and dispatch

`main` wraps the context in an interrupt guard — SIGINT/SIGTERM cancels the command context and escalates to a hard exit if in-flight work ignores the cancel — then runs the CLI and exits with the code derived from the returned error (`cmd/lit/main.go:19-21`). Bare `lit` inside a git repo prints the quickstart guidance (identical to `lit quickstart`); outside a git repo it prints help instead; a first argument that is not a registered command returns `UnknownCommandError` (`internal/cli/cli.go:53-87`). Every registered command disables cobra flag parsing and parses its own flags (`register.go:420`).

Flag handling common to every command (`cli.go:274-300`):

- `--help` prints usage plus defaults to stdout and exits 0.
- `--output` in any form → `UnsupportedError` ("--output is no longer supported; omit it for text output"), exit 3.
- `--continue` → `UnsupportedError` (retired; claim routing already keeps `lit next` in the checkout's own epic first), exit 3.
- Any other unknown flag → `UsageError`, exit 2.

Family commands (`sync`, `hooks`, `backup`, `snapshots`, `lifeboat`, …) resolve their subcommand by exact match; a missing, unknown, or flag-shaped first argument returns the family usage string as a plain error — exit 1, not 2 (`register.go:112-123`).

### Exit codes

The complete taxonomy (`internal/cli/exit.go:10-91`):

| Code | Meaning | Producing error types |
|---|---|---|
| 0 | success | nil (including handled `--help`) |
| 1 | generic | plain errors, `OutsideWorkspaceError`, `BulkFailureError`, transient GC contention |
| 2 | usage | `UsageError` |
| 3 | validation | `UnknownCommandError`, `RetiredCommandError`, `ValidationError` (CLI and storage), `UnsupportedError` |
| 4 | not found | `storage.NotFoundError` |
| 5 | conflict | `MergeConflictError`, `SyncFailureError`, owner-approval refusal |
| 7 | corruption | `CorruptionError` |

Commands are organized into help groups: `bootstrap` ("Human Bootstrap"), `operations` ("Agent Operations"), `structure`, `data` ("Sync & Data"), `maintenance` ("Setup & Maintenance"), `retention`, and `guidance` (`register.go:61-76`). Access modes vary per command: `upgrade` is workspace-only and never opens the app store, `doctor` resolves read-vs-write from its args, `backup` sets access per row, and `stores`/`completion`/`version` open no workspace at all (`register.go:273-393`).

Two ops commands are retired but still dispatchable (hidden, exit 3 with redirect guidance): `ls-at` → "use `lit ls --at <store-dir>`" and `overview` → "use `lit stores --counts`" (`register.go:371-374, 439-440`).

### Workspace resolution and post-command behavior

Every workspace/app command resolves the workspace from the cwd; outside a git repository this is `OutsideWorkspaceError` ("links requires running inside a git repository/worktree"), exit 1 (`cli.go:149-163`). Resolution itself **creates** `<git-common-dir>/links/` and its `config.json` — even read commands materialize the storage dir (`internal/workspace/workspace.go:165-168`). Geometry: `StorageDir = <git-common-dir>/links`, with the Dolt database under it (`workspace.go:284-292`).

After a successful handler, `runWithApp` (`cli.go:101-147`) — in order, after the engine closes:

1. On write-mode commands: print the mutation staleness banner (§ staleness banners below).
2. `maybeAutoSyncAfterCommand` — the entry to the whole background engine.

A failed handler skips both. Three read surfaces additionally print the store-backed staleness banner: the `show` family, `next`, and the `backlog`/workable views (`cli.go:870`, `next.go:53`, `workable.go:137`).

Durations in every banner and age line render coarsely: ≥48h → "N days", ≥2h → "N hours", ≥2m → "N minutes", else "under a minute" (`output.go:451-462`).

## `lit init`

Flags: `--skip-hooks` (skip git hook installation), `--skip-agents` (skip AGENTS.md/CLAUDE.md update); any positional is a usage error, exit 2 (`init.go:29-36`).

Sequence (`init.go:27-144`):

1. **Remote adopt runs before any store exists** — so a clone of a remote backlog is the path's first writer (`init.go:38-44`).
2. A sync trace is recorded for the adopt decision, always, whatever the outcome (`init_sync.go:333-354`): command `lit init`, decision = the outcome state, status `error` iff failed, plus the build note and `{remote, sync_branch}` metadata. A trace-write failure goes to stderr, non-fatal.
3. If the adopt outcome is `failed`, init hard-stops with **no store created**: "could not confirm the workspace state, so init is refusing to create a fresh store: <error>", exit 1 (`init.go:60-74`).
4. Otherwise, unless a remote backlog was adopted, `store.EnsureDatabase` creates the Dolt store (`init.go:81-88`).
5. Hooks (unless skipped): install the managed `pre-push` hook; an error aborts init (`init.go:101-111`).
6. Agents (unless skipped): write the managed sections of `AGENTS.md` and `CLAUDE.md`; an error aborts. Each file reports `created`/`updated`/`unchanged` plus which template layer supplied the section (`project`/`global`/`embedded`) (`init.go:113-134`).

### The adopt decision machine

Six outcome states (`init_sync.go:32-50`): `has_local_tickets`, `not_configured`, `remote_empty`, `no_remote_data`, `adopted`, `failed`. The decision runs under a 120-second timeout (`adoptRemoteTimeout`, `init_sync.go:24`); on deadline the outcome is `failed` with a message explaining the store is not usable yet and that retrying `lit init` sets the interrupted download's leftovers aside automatically (`init_sync.go:103-116`).

Planning order (`init_sync.go:179-261`): pending-adopt residue check (residue converts every benign terminal into `failed`) → local tickets present? → git remotes readable? → resolve the sync remote (none → `not_configured`) → remote has refs? (no → `remote_empty`) → resolve the sync branch → remote has Dolt data under `refs/dolt/*`? (no → `no_remote_data`) → resolve a git-backed clone URL (unresolvable → `failed`). When a plan exists, `store.AdoptRemoteByClone` downloads the backlog; a clone failure is `failed` with retry guidance. Each benign terminal prints one progress line ("local store already holds tickets; leaving it untouched…", "no eligible git remote; starting with an empty backlog", etc. — `init_sync.go:271-284`); `failed` prints nothing itself, the command error being the sole channel.

### Output and disk footprint

Human output (`init.go:175-226`): `Initialized lit workspace` (or `lit workspace already initialized`); when adopted, `  Pulled existing backlog from <remote>/<branch> (<build note>)`; then `Updated:` / `Up to date:` / `Skipped:` lines over the entries `pre-push hook`, `AGENTS.md`, `CLAUDE.md` (the latter two annotated `via project|global|embedded`); and always a final guidance line pointing at `lit workflows`.

What init writes to disk (`init.go` §1.7): the `links/` storage dir + `config.json` (via workspace resolution, before the handler), the Dolt store, `<git-common-dir>/hooks/pre-push`, the two managed markdown sections, and a sync trace under `<StorageDir>/traces/sync/`. Init sets **no** git config keys. (An `initReport` JSON struct exists — `status`, `workspace_id`, `database_path`, `db_created`, `hooks`, `agents`, `claude`, `agents_source?`, `claude_source?`, `sync` — but no JSON output path renders it; `init.go:14-25`.)

## The `lit sync` family

Subcommands: `status`, `remote ls`, `fetch`, `pull`, `push`, `compact`, `reconcile`, plus the hidden `__mirror-bg` worker (`sync.go:87-100`). Every visible row opens one sync session up front; if the engine cannot sync, the whole family is refused at open, not per-verb (`sync.go:52-84`).

### Remote and branch resolution (shared by every sync surface)

- Remote (`sync.go:654-674`): an explicitly named remote must exist among the configured git remotes; otherwise precedence is the validated upstream remote, then the single configured remote when exactly one exists, else none.
- Branch (`sync.go:689-716`): the env override `LINKS_DEBUG_DOLT_SYNC_BRANCH` beats the remote's default branch; an unresolvable branch is an error naming that override as the escape hatch.
- Dolt remotes are reconciled from git remotes before every operation: added when missing, replaced when the URL differs, removed when the git remote is gone (`sync.go:897-943`).

### `sync status`, `sync remote ls`, `sync fetch`

- `status` (no flags) prints one line: `version=<dolt> branch=<b> head=<commit[ msg]> git=<n> dolt=<n> added=<n> updated=<n> removed=<n>` (`sync.go:622-652`).
- `remote ls` prints `git=<n> dolt=<n> added=<n> updated=<n> removed=<n>` — the add/update/remove sets from comparing git remotes to Dolt remotes (`sync.go:118-137, 945-973`).
- `fetch` (`--remote` default `origin`, `--prune`, `--verbose`): reconciles remotes, fetches, records a sync trace, and on success writes the fetch-success marker (a marker failure is stderr-only). Output `fetched` (or `fetched <remote>` verbose) (`sync.go:139-171`).

### `sync pull`

Flags: `--remote` (default: upstream → single-remote fallback), `--verbose`. Sequence (`sync.go:173-274`): reconcile remotes → resolve remote (none → traced `no_sync_remote`, exit 0, silent unless verbose) → remote has refs? (none → the first-push skip message, always printed: "Skipping lit sync: remote has no refs yet. This is normal ONLY for the very first push to a brand-new empty repo…") → resolve branch → `SyncPull`. Outcomes:

- Pull error → traced, converted by `asSyncFailure` (a remote-schema-ahead becomes the blocking contract, exit 5).
- Held outcomes: prose-pending → `SyncFailureError` class `prose_held`; unrelated histories → class `unrelated_histories`. Both exit 5, record a held trace, and notify the owner (`sync.go:287-308, 260-268`).
- `never_synced` state → skipped with `next_command: lit sync push --remote <r> --set-upstream` guidance. Up-to-date/fast-forwarded/linearized/ahead → `pulled` (verbose: `pulled <r>/<b> (<state>)`). An unrecognized state always prints "…this is a bug — please report it" (`sync.go:727-819`).

### `sync push`

Flags: `--remote`, `--set-upstream`, `--force`, `--verbose` (`sync.go:359-364`). The explicit command pushes via `SyncCompactAndPush` — compaction is atomic with the push; the background mirror uses plain `SyncPush` with no compaction (`sync.go:372`, `sync_bg.go:309`).

The shared orchestration `performSyncPush` (`sync.go:490-620`): clears the mirror-pending marker **at entry** (not on success); guarantees a push-outcome record on every return path including panic; reconciles remotes; resolves remote/branch with the same skip terminals as pull; runs the push; then writes an automation trace (only when `LNKS_AUTOMATION_TRIGGER` is set) and an unconditional durable sync trace (decision `pushed` or `error`). The push outcome's payload always carries `{status, remote, branch, raw}`, plus `reason` when skipped, or `push_status` (int64), optional `maintenance`, `trace_ref` (path), and `trace_error` otherwise (`sync.go:417-439`). Output: any non-empty maintenance report prints first in both modes; then `pushed` (non-verbose) or the engine's raw output / `pushed <r>/<b>` (verbose); skips are silent non-verbose except the first-push message, which always prints (`sync.go:821-867`).

### `sync compact`

Flag: `--full` — rewrite the old generation too, cost proportional to the whole store (`GCFull` vs the default `GCNewGen`) (`sync.go:317-329`). Requires no remote. Success records the compaction trace **before** printing `compacted (<depth>): <detail>`; a stdout write failure is the command's failure (`sync.go:331-356`).

### `sync reconcile` family

`lit sync reconcile [resolve --resolve ID:FIELD:FINGERPRINT=TEXT ... | abort | take local|remote | combine]` (`sync_reconcile_cmd.go:34-40`). All rows share a pre-step (`sync_reconcile_cmd.go:593-623`): reconcile remotes → resolve remote/branch → fetch → mark fetch success; when no remote with shared history exists every row prints `nothing to reconcile: no remote with shared ticket history yet` and traces `nothing_to_reconcile`. Stray positionals on the no-positional rows are usage errors, exit 2.

- **bare `reconcile`** runs the store reconcile and reports (below).
- **`resolve`** requires at least one repeatable `--resolve ID:FIELD:FINGERPRINT=TEXT`; when the divergence changed since the fingerprints were read, the pending render is prefixed "the divergence changed since you read it; your resolutions were not applied. Re-merge the CURRENT conflicts below:" (`sync_reconcile_cmd.go:129-165, 490-494`).
- **`abort`** defers: "reconcile deferred: the clone remains diverged and usable; a later command re-surfaces the divergence…", exit 0 (`sync_reconcile_cmd.go:173-187`).
- **`take <local|remote>`** (side parsing case-insensitive) is the destructive wholesale choice and requires `--owner-approved <token>`; without a valid token the command returns the owner-approval refusal block, exit 5, and notifies the owner (`sync_reconcile_cmd.go:199-259`). On success it prints what was kept and — by design — the DISCARDED issue-id set of the losing side; `take local` replays the local commits with original messages and timestamps and tells the user to push (`sync_reconcile_cmd.go:360-406`).
- **`combine`** unions both backlogs; its report lists `kept local-only:`, `kept remote-only:`, and `field-merged on both:` id sets (`sync_reconcile_cmd.go:266-294, 520-543`).

The shared reporter (`sync_reconcile_cmd.go:450-564`) maps store results: unrelated histories → `SyncFailureError`, exit 5; prose pending → guidance printed + `MergeConflictError` ("reconcile holds N free-text field(s) for inline merge…"), exit 5; linearized → "reconciled: the divergence merged into linear history — N local commit(s) replayed…"; combined → the four-line union block; not diverged → "nothing to reconcile…"; an unrecognized state prints and exits 0 with an error-status trace. Linearized and combined additionally report contested lanes (see `08-claims-and-identity.md`).

## The background sync engine

### Scheduling: `maybeAutoSyncAfterCommand`

Runs only after a **successful** command, after the engine closes (`sync_cadence.go:78-101`):

1. `LIT_DISABLE_AUTO_SYNC` truthy (`1/t/true` case-insensitive; unparseable = false) → nothing happens (`sync_cadence.go:34, 307-313`).
2. Config unreadable → stderr warning, nothing happens.
3. Write-mode command AND `sync.cadence = "on-change"` → ensure mirror coverage (spawn a background push).
4. `sync.receive = true` → inline receive.
5. Write-mode → inline compaction backstop, last, so it collects what the receive brought in.

Config defaults: `sync.cadence = "on-change"` (legal values `on-push`, `on-change`; anything else fails config load), `sync.receive = true`, `sync.owner_notify_cmd = ""` (`internal/config/config.go:127-150, 225-227, 252-254`).

### Mirror coverage and the mirror-pending protocol

`ensureMirrorCoverage` (`sync_cadence.go:122-202`): a remote-absent marker debounces the git-remote check to once per 10 seconds; then the command tries to **claim** the pending marker `<StorageDir>/mirror-pending` (`sync_mirror_pending.go:84-186`):

- Exclusive create succeeds → claimed, and the claimant holds a "beacon" (a shared file lock in the store) so concurrent commands can distinguish "a live process owns this" from "a stale marker".
- Marker exists → probe the beacon: answered → covered, no spawn; unheld or obstructed → refresh the marker's mtime and re-claim.
- With the claim held: no git remote → release, write the remote-absent marker; remote present → clear any stale remote-absent marker and spawn the mirror. Spawn or precondition failures release the claim, warn on stderr, and record a push-outcome failure record.

`clearMirrorPending` removes the marker (missing is normal); `recheckMirrorPending` treats a marker whose mtime predates the current cycle's start as an error — "stopping rather than cycling against a marker that cannot be removed" (`sync_mirror_pending.go:238-296`).

### Spawn and detach

`spawnBackgroundMirror` (`sync_bg.go:67-94`) re-executes the current binary as `lit sync __mirror-bg --parent-pid <pid>` in the repo root, stdin nil, output appended to `<StorageDir>/mirror.log` (falling back to discarded streams if the log can't open), detached via `Setsid` on POSIX. Windows gets an empty attr struct — and lit has no Windows build, because embedded Dolt does not compile there (`detach_posix.go:17-19`, `detach_windows.go:14-16`). The child env strips any inherited `LNKS_AUTOMATION_*` values and sets `LNKS_AUTOMATION_TRIGGER=on-change`, `LNKS_AUTOMATION_REASON="on-change cadence mirrored after a mutating command"` (`sync_bg.go:104-128`).

Timing constants (`sync_bg.go:29-58`): the parent's post-spawn tail is 71s (receive 15s + notify hook 10s + pipe delay 1s + compact 45s); the mirror waits for the parent up to 101s (tail + 30s margin), polling every 20ms.

### The detached worker

`runBackgroundMirror` (`sync_bg.go:145-255`):

1. Holds the mirror beacon from entry until process death; a hold failure completes through the push-outcome seam.
2. Waits for the parent PID to exit (getppid polling); timeout → records "spawning command (pid N) still running after 101s; skipping mirror to avoid racing its engine".
3. Cycle loop: acquire the sync-push single-flight lock — losing it returns silently with **no store opened, no trace, no file created**; run one mirror cycle (open sync session, plain `SyncPush` with no remote/upstream/force flags and no compaction); then re-check the pending marker — if a new mutation re-marked it during the cycle, run another full cycle on a fresh engine.
4. A remote-schema-ahead push error prints the blocking contract to stderr; any other push error stays in the trace for the next push to retry. The mirror **never exits nonzero** (`sync_bg.go:306-332, 394-400`).

### Inline receive

`receiveInline` (`sync_receive.go:31-73`): debounced to once per 5 minutes via `<StorageDir>/receive.last` (the attempt is marked **before** any work); silent when no remote; runs `SyncReceive` under a 15-second timeout. On a nil receive error the fetch-success marker is written whatever the state. When the receive lands `diverged`, an **inline reconcile** runs on the same engine (`sync_receive.go:307-309, 338-392`). The outcome is surfaced with the command's own context: a non-converging outcome prints the sync-failure block to **stderr** and notifies the owner; a clean settle clears the divergence notify markers; the command's exit code is never affected (`sync_receive.go:93-169`).

### Compaction backstop

`compactInline` (`sync_compact.go:81-117`): probes at most every 15 minutes (`compact.last`, marked before running so a store failing every pass is asked once per interval), opens a session under a 45-second timeout, and calls `CompactIfDue` — the engine decides whether a pass is owed. A pass that ran records decision `compacted` with `{depth, detail}` metadata under the trace command `compaction backstop` — deliberately not a runnable command line (`sync_compact.go:137-181`). The on-change mirror is never the compaction host, because mirror coverage short-circuits on remote-less workspaces and compaction must run even there (`sync_compact.go:30-33`).

### Push-outcome marker

Every push attempt — explicit or mirrored — completes through `completePushAttempt`, which derives a record `{decision, reason?, remote?, branch?}` and atomically writes `<StorageDir>/push-outcome.last` (`sync_push_outcome.go:59-153`). Decisions: `pushed`, `error`, `canceled` (context canceled), `workspace_busy`, or a skip reason (`no_sync_remote`/`remote_empty`). Only `error` counts as failed. The record also feeds owner notification (below). Marker read/write failures are stderr-only, never returned.

### Staleness banners

Three signals (`sync_staleness.go`):

- **Push-failure line** — when the last push-outcome record failed: `sync: automatic push[ to <r>/<b>] is FAILING — last attempt <age> ago: <reason> — changes stay on this machine until a push succeeds; run 'lit sync push'`. The reason is first-line-only, capped at 160 runes.
- **Fetch-staleness line** — when the last successful fetch (marker `fetch-success.last`, written by `sync fetch`, `sync pull`, the reconcile pre-step, and inline receive) is ≥ 24 hours old: `sync: last successful fetch[ from <ref>] was <age> ago (over 24h0m0s) — run 'lit sync fetch'`.
- **Ahead line** — read commands with a resolved freshness in state ahead: `sync: <N> local change(s) not pushed to <r>/<b>, as of last fetch — run 'lit sync push'`. Deliberately not emitted for diverged (that has the heavier failure block) and not special-cased for never-synced.

Read commands print push-failure first, then ahead/fetch lines. Write commands, at the `runWithApp` seam, read only the storage-dir markers and print the push-failure line plus a ref-less fetch line; banner write failures never change the exit code (`sync_staleness.go:186-228`).

### The sync-failure contract

Four classes (`sync_failure.go:20-45`): `prose_held`, `diverged_unresolved`, `remote_schema_ahead`, `unrelated_histories`. A divergence is **persistent** when its age ≥ 24h or ahead+behind > 10 commits (`sync_failure.go:53-56`).

`blockString()` renders an `<agent-instructions>` block (`sync_failure.go:185-227`) containing: a fixed must-not-ignore directive ("This is a blocking condition, not ambient noise… Resolve it now, or explicitly surface it to the user as blocking, before continuing ticket work"); a per-class WHAT HAPPENED line (with an explicit unknown-class arm reporting a bug); an optional per-side issue-id inventory (`only on local / only on remote / on both`); the build note; ordered HOW TO RESOLVE steps per class — `prose_held` → `lit sync reconcile`; `diverged_unresolved` → pull then reconcile; `remote_schema_ahead` → `lit upgrade --to <producer>` when the remote head names a producer version, else bare `lit upgrade`; `unrelated_histories` → combine first, then the two destructive takes (marked owner-approval-required), then bare reconcile — and an escalation line: fixed BLOCKED sentences for schema-ahead and unrelated histories, otherwise INCIDENT wording when persistent and routine-window wording when recent.

### Owner-approval refusal for `take`

The refusal block (`sync_take_approval.go:31-93`) states what the take would do (keep one side wholesale, permanently discard the other side's ids, listed), why it is blocked ("choosing which side of a forked backlog survives is the OWNER's decision… Do not self-approve; approval asserted without the owner's explicit instruction is a false claim"), and how to proceed (combine needs no approval; surface to the owner; then take with the token). The token binds to the exact fork — local head, remote head, and side; any new commit on either side voids it, and a stale token gets an explicit re-read instruction. Heads render truncated to 12 chars, `(unknown)` when empty.

### Traces

Two trace streams under `<StorageDir>/traces/`:

- **Sync traces** (`traces/sync/`, `sync_trace.go`): written **unconditionally** for every sync-family decision. Record: `{id, recorded_at, workspace_id, command, decision, status, reason?, trigger?, build_note?, metadata?}`; the trigger is read fresh from `LNKS_AUTOMATION_TRIGGER` at write time. A non-nil error forces decision/status `error`. Held outcomes trace decision = the failure class with status `ok` (the operation completed) and the one-line what-happened as the reason.
- **Automation traces** (`traces/automation/`, `automation_trace.go`): written only when `LNKS_AUTOMATION_TRIGGER` is set. Record: `{id, recorded_at, workspace_id, trigger, command, side_effect, status, reason?, metadata?}`; an empty reason falls back to `LNKS_AUTOMATION_REASON`; when `LNKS_AUTOMATION_TRACE_REF_FILE` is set the trace path is written to that file.

### Owner notifications

A configurable shell hook for surfacing sync problems to a human (`owner_notify.go`). Kinds: the three divergence classes plus `push_failed`. Per-kind marker `<StorageDir>/owner-notify.<kind>.last` holding a fingerprint (`push_failed` keys on the kind alone; divergences on kind+remote/branch); a notification is due when the marker is missing/unreadable, the fingerprint changed, or the 24-hour cooldown elapsed. `maybeNotifyOwner`: skipped under `LIT_DISABLE_AUTO_SYNC`, when not due, or when `sync.owner_notify_cmd` is empty. The hook runs as `sh -c <cmd>` in the repo root with a 10-second deadline and env vars `LIT_NOTIFY_KIND`, `LIT_NOTIFY_SUMMARY`, `LIT_NOTIFY_REMOTE`, `LIT_NOTIFY_BRANCH`, `LIT_NOTIFY_REPO`. Hook failure: stderr warning, error trace, **marker not written** so the next detection retries. Success: marker written, `ok` trace. A `pushed` outcome clears the `push_failed` marker; a failed outcome notifies with "a lit sync push to <target> failed: <reason> — local ticket changes are not reaching the shared backlog."; canceled/busy/skips do neither (`owner_notify.go:148-270`).

### Marker inventory

Files the engine keeps under `<StorageDir>`: `receive.last`, `remote-absent.last`, `compact.last`, `fetch-success.last`, `mirror-pending`, `push-outcome.last`, `owner-notify.<kind>.last`, `mirror.log`, `snapshots/`, `traces/{sync,automation}/`, `last-sync-base.json` (`sync_cadence.go:212-261` for the marker primitives).

### Acceptance evidence (`cmd/lit` tests)

Four end-to-end tests pin engine behavior (`cmd/lit`): with zero sync config, a single mutating command's change reaches a bare git remote with no explicit push (`eager_push_test.go:49`); a mutation-only session sees the push-failure banner within a bounded window against an unreachable remote, and a healthy remote produces no banner even while momentarily ahead (`mutation_staleness_banner_test.go:34`); a burst of mutating commands never surfaces Dolt's "database is read only" error and every commit reaches the remote without a sweep push (`sync_engine_race_test.go:49`); and mirror quiescence is provable by reading the owed-marker first and probing the beacon last, with a 60s patience budget (`mirror_quiescence_test.go:20-76`).

## `lit doctor`

Access mode is decided from the args before the app opens: any `--fix` ⇒ write, else read (`doctor.go:206-217`). Flag: `--fix` with optional value, bare `--fix` meaning `all` (`doctor.go:254-255`).

Fixes — the registry is the single authority (`doctor.go:230-250`): `integrity` (foreign-key/related-row/orphan-history repair, printing the counts) and `rank` (re-rank dependency issues to repair rank order, printed only when >0 were moved). Bare `--fix` runs all in sorted order; a comma list runs the named ones in the given order; an unknown name errors listing `integrity, rank`, exit 1. Fix progress writes to **stderr** (`doctor.go:262-278`).

Checks, printed to stdout in order (`doctor.go:252-331`):

1. Workspace identity: `workspace: storage_dir=… workspace_id=… issue_prefix=… issue_prefix_source=configured|derived git_common_dir=…`.
2. The build-status note.
3. `integrity_check=<v> foreign_key_issues=<n> invalid_related_rows=<n> orphan_history_rows=<n> rank_inversions=<n> dependency_cycle=<none|a->b->c>`.
4. One sync-freshness line — a distinct message per state: no remote, unresolved (with detail), never synced, up to date, ahead (with `[ahead=n behind=0]`), behind, diverged (with both counts); an unhandled state is an error, exit 1. Freshness resolution never errors — every failure becomes an "unresolved" report with a reason (`doctor.go:105-204`).
5. Only when the last push attempt failed: `sync: last push attempt FAILED <age> ago: <reason>[ — mirror log: <path> (last written <age> ago)]`.

Exit: any integrity errors → `CorruptionError`, exit 7, which wins over the divergence exit; a persistent divergence (≥24h or >10 commits) → `SyncFailureError` class `diverged_unresolved`, exit 5, with owner notification; otherwise 0 (`doctor.go:312-329`).

## `lit upgrade` and `lit downgrade`

Both take exactly `--to <tag>` and no positionals; the shared tag normalizer adds a missing `v` prefix and rejects empty values or tags containing `/`, `\`, `..`, or whitespace (`ValidationError`, exit 3) (`downgrade.go:136-156`).

**`upgrade`** is workspace-mode and never opens the app store (`register.go:389-390`). It resolves the target release, reads the workspace schema version through a read-only open (an unsupported-schema error is tolerated — its version is used, flagged non-openable), and refuses a backward move **before any install**: a target whose max supported schema is behind the workspace's applied version gets `UpgradeTargetBehindError` — with distinct wording depending on whether this binary can open the workspace (pointing at `lit downgrade` when it can, at a newer target when it cannot), exit 1 (`upgrade.go:186-255, 45-58`). Install failure includes manual-recovery guidance with the artifact URL. Success: `upgraded to <tag> (schema support through vN) installed at <path>` plus "the next lit run migrates this workspace forward if it trails". Upgrade never touches the schema — forward migrations live in the target binary and run on its next open (`upgrade.go:61-73`).

**`downgrade`** is app-mode write and requires the schema-migration capability up front (`downgrade.go:38-47`). Ordering is the reverse of upgrade's: `store.Downgrade` reverses the **schema first**, then the older binary is installed. Pre-snapshot refusals propagate verbatim; post-snapshot failures arrive as a rollback error carrying the restore instruction. An install failure after the schema reversed prints both recovery routes (manual install, or snapshot restore via `lit snapshots list`/`restore`). Success: `downgraded to <tag> (schema vN) installed at <path>`; no re-exec (`downgrade.go:67-124`).

Neither command has `--dry-run`, `--force`, or `--skip-snapshot` — deliberately (`downgrade.go:36-37`, `upgrade.go:74-75`).

## `lit backup` (JSON export family)

Rows and access: `create` (read), `list` (read), `restore` (write) (`backup.go:22-30`).

- **`create --keep <n>`** (default 20): exports the store to JSON, writes a snapshot under the storage dir, prunes to `--keep`, prints `<name> <path>` (`backup.go:32-51`).
- **`list`**: one `<name> <size> <path>` line per snapshot (`backup.go:53-68`).
- **`restore (--latest | --path <export.json>) [--force]`**: `--latest` and `--path` are mutually exclusive (exit 2); `--latest` with no backups errors (exit 1); neither source is a usage error. The restore requires both the sync and import capabilities up front, then (`backup.go:126-193`): reads the export file; **refuses over unsynced local state** — when a sync state exists and the current export's SHA-256 (over the canonical indented-JSON encoding plus trailing newline, lowercase hex — `backup.go:195-205`) differs from the recorded `last-sync-base.json` hash, `MergeConflictError` ("restore conflict: local workspace has unsynced changes since last sync base"), exit 5, unless `--force`; always takes a pre-restore backup (pruned to a hard-coded 20); replaces the store contents from the export; re-exports and atomically rewrites `last-sync-base.json`; records the new sync state; prints `restored <path>`.

`bulk import` is retired in favor of this path (exit-3 guidance names `lit backup restore --path`) (`register.go:441`).

## `lit snapshots` (Dolt filesystem-level snapshots)

Rows: `new`, `list`, `restore`; snapshot dir `<StorageDir>/snapshots` (`snapshots.go:19-34`).

- **`new [--label <text>]`**: any positional is a usage error (the likely `snapshots new nightly` typo). Takes three holds in order — workspace shared, Dolt journal exclusive, commit lock — released LIFO with release failures joined into the error; refuses when a pending-adopt marker exists; prints `<name> <path>` **the moment the snapshot exists**, before pruning and even beside a later failure. Retention pruning (config `snapshot.retention_budget`, default 5, must be >0) runs after the workspace hold is released, under the commit lock only, and only over *user* snapshots — migration/downgrade/reconcile snapshots are excluded (`snapshots.go:76-189, 46-50`; `config.go:224, 245-247`).
- **`list`**: `<name> <created RFC3339> <path>` per snapshot (`snapshots.go:191-206`).
- **`restore <name>`**: exactly one positional. Holds the workspace exclusively for the whole restore; the swap runs under the commit lock. If the restore failed after the live database directory was rotated aside, the error names the rotated path holding the workspace's data. Success prints `restored <name>` (plus `rotated_to=<path>` when a rotation happened) (`snapshots.go:208-271`).

## `lit lifeboat` (below-the-gate recovery)

For stores the normal engine refuses to open. Rows: `dump`, `recover` (`lifeboat.go:16-28`).

- **`dump`**: raw JSON dump of the database to stdout — JSON only, no text rendering (`lifeboat.go:154-171`).
- **`recover [--mapping <file>]`**: an operator-authored ShapeMapping JSON, defaulting to the built-in deterministic mapper. First heals any promotion that crashed between renames, unconditionally; then dumps raw and runs recovery with exactly one attempt. Outcomes (`lifeboat.go:75-144`): reconciled → the rebuilt candidate is promoted to canonical, output naming where the previous contents were preserved; requires-drop → the candidate is discarded and the command fails listing each source column the mapping would silently discard ("recovery needs a human decision… nothing was changed"); unconverged → fails with the residual ("nothing was changed").

## `lit stores`

Not app-mode; positional args are discovery roots (default: cwd) (`stores.go:24-44`).

- **Default**: streams one canonical storage dir per discovered store; none found is empty output, exit 0.
- **`--counts`**: a cross-project rollup. Each store is opened read-only; its label is the configured issue prefix (falling back to the storage dir); workability is classified with a nil required-fields policy — opting out of per-repo required fields only, while blockers, the lane gate, and needs-design still apply. Output: a `PROJECT / READY / IN-FLIGHT / BLOCKED` table with a TOTAL row summing exactly the shown rows (table omitted when every store errored), then `! <dir>: <err>` per unreadable store and `~ <dir>: close warning: <err>` per close warning (`stores.go:95-221`).

## `lit hooks`

One row: `install`. Prints `installed <hookPath>` whether or not anything changed (`hooks.go:32-56`).

`installHooks` — shared by `init`, `hooks install`, and `quickstart --refresh` (`hooks.go:58-125`): writes the managed section of `<git-common-dir>/hooks/pre-push`. Absent hook → created as `#!/usr/bin/env bash` + section, mode 0755. Existing hook → mode preserved (execute bit forced on); a hook whose first line is not a bash shebang is **left untouched** and reported `incompatible` — no error, exit 0. Legacy `LINKS INTEGRATION` markers migrate to `LIT INTEGRATION` (firing when either marker is present, so partial states converge); the managed region between markers is replaced in place, or the section is appended when markers are absent (`managed_sections.go:10-51`).

## `lit quickstart`

`lit quickstart [work|new|update|done|doctor] [--refresh] [--eject[=LIST]] [--force]` (`quickstart_topics.go:55`). Validation (all exit 2): at most one positional; `--refresh` and `--eject` are mutually exclusive; `--force` only with `--eject`; a topic takes no flags; unknown topics are rejected naming the five valid ones (`cli.go:1737-1759`).

- **Topic mode** renders the topic's template (project > global > embedded), trimmed; topic output never carries the soil section (`quickstart_refresh.go:203-212`).
- **Bare / `--refresh`** renders `quickstart.md`, appending a "soil" section when config `quickstart.soil_mode = true` (default false). `--refresh` additionally runs the same writers `init` uses — hooks and agent files — plus an **inspection-only** pass over the quickstart templates: per template, `absent` (no override), `unchanged` (override identical to embedded), or `skipped`/`customized` (override drifted; left untouched — refresh never overwrites overrides). The human summary groups items into `Refreshed:` / `Skipped:` / `Up to date:`, or `nothing to refresh` (`quickstart_refresh.go:29-165`).
- **`--eject`** copies embedded defaults to the global override path `<config-dir>/templates/<name>`. Selection: bare `--eject` = all eight; otherwise comma-separated short aliases (`agents, hook, quickstart, quickstart-done, quickstart-doctor, quickstart-new, quickstart-update, quickstart-work`), unknown aliases erroring with the list. All targets are planned first; any existing file aborts the entire write phase (nothing written) unless `--force` overwrites everything. Report lines: `exists` / `skipped` / `ejected`; conflicts end in `MergeConflictError`, exit 5 (`quickstart_eject.go:26-146`).

Commands emit **breadcrumbs** — `deeper guidance: lit quickstart <topic>` as a trailing output line; an unknown breadcrumb token panics (`quickstart_topics.go:62-75`).

## `lit version` and the build-status note

`lit version` (no positionals) prints: `lit <version|dev> (commit <sha|unknown>, built <date|unknown>)`; `built <age> ago` when the build date parses; a staleness warning when the age crosses the threshold ("run `just build`…"); and always `schema versions supported: <min>–<max>` (`version.go:17-68`).

The build-status note (`build_status.go:20-54`) renders `build: release <v>`, `build: dev build (build date unknown)`, `build: dev build, built <age> ago` — or the same with `— STALE (at least <threshold> old; run `just build` to refresh)`. A version-read failure yields `build: status unavailable (<err>)` rather than aborting. The note appears in `init` output, the init sync trace, `doctor`, every sync-failure block, and every sync trace.

## Managed sections and embedded templates

`ensureLinksAgentFiles` is the single writer for both agent files (`agents_internal.go:70-89`): it loads the `agents-section.md` template (project > global > embedded) and upserts the managed region — markers `<!-- BEGIN LIT INTEGRATION -->`/`<!-- END LIT INTEGRATION -->`, legacy `LINKS` markers migrated — into `AGENTS.md` (created with a `# AGENTS` header) and `CLAUDE.md` (created with no header). Everything outside the markers is preserved; a marker-only migration counts as a change (`agents_internal.go:35-64`).

Template resolution (`internal/templates/templates.go:95-129`): **project** `<root>/.lit/templates/<name>` > **global** `<config-dir>/templates/<name>` > **embedded**; a layer with an absent or empty file contributes nothing; all-empty is an error.

The eight embedded assets (all also ejectable):

| Asset | Written by | Destination | Role |
|---|---|---|---|
| `agents-section.md` | `ensureLinksAgentFiles` | managed region of `AGENTS.md`/`CLAUDE.md` | 8 lines telling the agent to run `lit quickstart` first |
| `pre-push-hook.sh` | `installHooks` | managed region of the git `pre-push` hook | runs `lit sync push --remote <r>` with `LNKS_AUTOMATION_TRIGGER=git-pre-push` and a trace-ref file; on failure prints an `<agent-instructions>` warning with the trace path; always exits 0 so a sync failure never blocks the git push |
| `quickstart.md` | bare `lit` / `lit quickstart` | stdout | 18-line router over the five topics plus a fastpath (`lit next` / `lit start` / `lit workflows`) |
| `quickstart-work.md` | `lit quickstart work` | stdout | finding/starting work: `ls`, `next`, `backlog`, `show`, claims-first selection, `start --take` |
| `quickstart-new.md` | `lit quickstart new` | stdout | `lit new` flags, description/topic notes, `followup`, `import --path` |
| `quickstart-update.md` | `lit quickstart update` | stdout | `update`, `import`, `rank`, `label add/rm`, `parent set`, `dep add`, `comment add` |
| `quickstart-done.md` | `lit quickstart done` | stdout | `done`, `close --resolution …`, `followup`, `workflows edit done`, commit reminder |
| `quickstart-doctor.md` | `lit quickstart doctor` | stdout | `doctor [--fix]` plus a self-resolve-first note |

## Consolidated refusal table (operations scope)

| Condition | Surface | Result | Exit |
|---|---|---|---|
| Outside a git repo | any workspace/app command | `OutsideWorkspaceError` | 1 |
| Missing/unknown family subcommand | all families | family usage as a plain error | 1 |
| Unknown flag | any command | `UsageError` | 2 |
| `--output` / `--continue` | any command | `UnsupportedError` | 3 |
| Stray positional | `init`, `version`, `hooks install`, `snapshots new`, `lifeboat *`, `upgrade`, `downgrade`, `sync reconcile` rows | `UsageError` | 2 |
| Adopt could not confirm workspace state | `init` | refusal, no store created | 1 |
| Remote schema ahead | push/pull/reconcile, inline receive, mirror | `SyncFailureError` block (mirror: stderr only) | 5 |
| Held prose conflict | `sync pull` / reconcile rows | `SyncFailureError` / `MergeConflictError` | 5 |
| Unrelated histories | `sync pull`, reconcile rows | `SyncFailureError` | 5 |
| `take` without owner approval | `sync reconcile take` | refusal block | 5 |
| Persistent divergence | `doctor` | `SyncFailureError` | 5 |
| Store corruption | `doctor` | `CorruptionError` | 7 |
| Unsynced local changes | `backup restore` without `--force` | `MergeConflictError` | 5 |
| `--latest` + `--path` | `backup restore` | `UsageError` | 2 |
| Existing override without `--force` | `quickstart --eject` | `MergeConflictError`, nothing written | 5 |
| Non-bash existing pre-push hook | hook installers | left untouched, reported `incompatible` | 0 |
| Pending-adopt marker | `snapshots new` | refusal | 1 |
| Backward-move target | `upgrade` | `UpgradeTargetBehindError`, nothing installed | 1 |
| Empty/invalid `--to` | `upgrade`, `downgrade` | `ValidationError` | 3 |
| Unknown `--fix` name | `doctor` | error listing `integrity, rank` | 1 |
| Unknown quickstart topic / template alias | `quickstart` | `UsageError` / alias error | 2 / 1 |
