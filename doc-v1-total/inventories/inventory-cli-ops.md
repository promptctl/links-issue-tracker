# `lit` OPERATIONS commands — raw behavioral inventory (source-derived)

Scope: `internal/cli` operations commands, `internal/templates`, and `cmd/lit` sync
acceptance tests. Every claim carries a `file:line` citation. Derived exclusively
from Go source and embedded assets; no `.md` documentation was read as a source of
truth (the files under `internal/templates/defaults` are treated as program output
data — described as assets, not as documentation of behavior).

---

## 0. Shared dispatch, flag, and exit machinery

### 0.1 Entrypoint

- `cmd/lit/main.go:19` wraps `context.Background()` in `interrupt.Guard(ctx, interrupt.DefaultGrace)` — a SIGINT/SIGTERM cancels the command context and escalates to a hard exit if in-flight work ignores the cancel.
- `cmd/lit/main.go:21` runs `cli.Run(ctx, os.Stdout, os.Stderr, os.Args[1:])`; on error exits with `cli.WriteCommandError(os.Stderr, err)`.
- `internal/cli/cli.go:35-50` `Run`: normalizes global args (`parseGlobalArgs`), builds the cobra root, `SilenceErrors`/`SilenceUsage` true; `pflag.ErrHelp` and `errHelpHandled` are swallowed to a nil error (exit 0).
- `internal/cli/cli.go:53-87` root command `lit`: `Long: "Agent-native issue tracker"`. Bare `lit` with no args prints `renderQuickstartGuidance(ws.RootDir)` (identical to `lit quickstart`) — `cli.go:63-75`. Outside a git repo (`workspace.ErrNotGitRepo`) it prints cobra help instead (`cli.go:66-68`). A non-empty first arg that is not a registered command returns `UnknownCommandError` (`cli.go:59-61`).
- `internal/cli/cli.go:82-84` root flag errors are wrapped as `UsageError` (exit 2).
- `internal/cli/register.go:420` every registered command sets `DisableFlagParsing: true` and `Args: cobra.ArbitraryArgs`; each command parses its own flags.

### 0.2 Flag set semantics (applies to every command below)

- `internal/cli/cli.go:192-203` `newCobraFlagSet(use)` builds a cobra command with a default `--help` flag, output discarded.
- `internal/cli/cli.go:274-300` `parseFlagSet`:
  - `--help` → prints `Usage of <use>:` plus `PrintDefaults()` to stdout, returns `errHelpHandled` (exit 0) — `cli.go:277-283`, `cli.go:266-273`.
  - `--output` (any form) → `UnsupportedError{Message: "--output is no longer supported; omit it for text output"}` (exit 3) — `cli.go:287-290`.
  - `--continue` → `UnsupportedError{Message: "--continue is retired; claim routing already keeps \`lit next\` in your checkout's own epic first — run \`lit next\` with no flag"}` — `cli.go:291-295`.
  - Any other `unknown flag:` / `flag provided but not defined:` → `UsageError` (exit 2) — `cli.go:296-298`.
- `internal/cli/cli.go:233-241` `StringOptional(name, defaultIfPresent, defaultIfAbsent, usage)` — used only by `quickstart --eject`.
- `internal/cli/cli.go:1958-1979` `splitArgs(args, n)` splits leading positionals from flags (used by `snapshots restore`).
- `internal/cli/register.go:112-123` `commandFamily.resolve`: a missing / unknown / flag-shaped first argument returns `errors.New(family.usage)` — a plain error → exit 1 (`internal/cli/exit.go:90`), not exit 2. Match is exact (no trimming).
- `internal/cli/register.go:129-138` `visibleSubcommands()` drops `hidden` rows from help/completion.

### 0.3 Exit codes (`internal/cli/exit.go:10-91`)

| Code | Constant | Trigger |
|---|---|---|
| 0 | `ExitOK` (`exit.go:11`) | nil error |
| 1 | `ExitGeneric` (`exit.go:12`) | default; also `OutsideWorkspaceError` (`exit.go:77-80`), `BulkFailureError` (`exit.go:81-88`), `store.ErrTransientGCContention` (`exit.go:88-90`) |
| 2 | `ExitUsage` (`exit.go:13`) | `UsageError` (`exit.go:53-56`) |
| 3 | `ExitValidation` (`exit.go:14`) | `UnknownCommandError` (`exit.go:57-60`), `RetiredCommandError` (`exit.go:63-66`), `ValidationError` (`exit.go:67-70`), `storage.ValidationError` (`exit.go:71-74`), `UnsupportedError` (`exit.go:74-77`) |
| 4 | `ExitNotFound` (`exit.go:15`) | `storage.NotFoundError` (`exit.go:27-30`) |
| 5 | `ExitConflict` (`exit.go:16`) | `MergeConflictError` (`exit.go:31-34`), `SyncFailureError` (`exit.go:38-41`), `ownerApprovalRefusalError` (`exit.go:45-48`) |
| 7 | `ExitCorruption` (`exit.go:17`) | `CorruptionError` (`exit.go:49-52`) |

### 0.4 Command registry rows relevant to operations (`internal/cli/register.go:273-393`)

| Command | Group | Wrapper | Access | Line |
|---|---|---|---|---|
| `init` | bootstrap | `wsCmd(runInit)`; `Long: humanBootstrapHelp` | workspace | `register.go:274-275` |
| `quickstart` | guidance | `wsCmd(runQuickstart)` | workspace | `register.go:276-277` |
| `completion` | guidance | `runCompletion` | none | `register.go:280-281` |
| `version` | guidance | `runVersion` | none | `register.go:282-283` |
| `hooks` | maintenance | `wsFamilyCmd(hooksFamily)` | workspace | `register.go:284-285` |
| `sync` | data | `wsFamilyCmd(syncFamily)` | workspace | `register.go:286-287` |
| `stores` | maintenance | raw `runStores` | none (discovery) | `register.go:366-367` |
| `doctor` | maintenance | `appCmdDynamic(resolveDoctorAccessMode, runDoctor)` | read or write | `register.go:379-380` |
| `backup` | data | `familyCmd(backupFamily)` | per-row | `register.go:381-382` |
| `snapshots` | data | `wsFamilyCmd(snapshotsFamily)` | workspace | `register.go:383-384` |
| `lifeboat` | maintenance | `wsFamilyCmd(lifeboatFamily)` | workspace | `register.go:385-386` |
| `downgrade` | maintenance | `appCmd(app.AccessWrite, runDowngrade)` | write | `register.go:387-388` |
| `upgrade` | maintenance | `wsCmd(runUpgrade)` | workspace only (never opens the app store) | `register.go:389-390` |

Command groups: `bootstrap`/"Human Bootstrap", `operations`/"Agent Operations", `structure`, `data`/"Sync & Data", `maintenance`/"Setup & Maintenance", `retention`, `guidance` (`register.go:61-76`).

Retired-but-dispatchable ops-adjacent commands (`Hidden: true`, return `RetiredCommandError`, exit 3): `ls-at` → "use `lit ls --at <store-dir>`" (`register.go:371-372`, guidance string `register.go:439`), `overview` → "use `lit stores --counts`" (`register.go:373-374`, `register.go:440`).

### 0.5 Workspace resolution

- `internal/cli/cli.go:149-163` `resolveWorkspaceFromWD()`: `os.Getwd()` then `workspace.Resolve(cwd)`; `workspace.ErrNotGitRepo` → `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}` (exit 1).
- `workspace.Resolve` **creates** `<git-common-dir>/links` (`internal/workspace/workspace.go:165`) and loads-or-creates `config.json` (`workspace.go:168`). So even read commands materialize the storage dir.
- Geometry: `StorageDir = <git-common-dir>/links`, `DatabasePath` under it, `GitCommonDir = filepath.Dir(StorageDir)` (`internal/workspace/workspace.go:284-292`).

### 0.6 Post-command automatic behavior (`runWithApp`)

`internal/cli/cli.go:101-147`:
1. `app.Open(ctx, cwd, accessMode)`; `ErrNotGitRepo` → `OutsideWorkspaceError` (`cli.go:108-113`).
2. Handler runs with `defer ap.Close()` (`cli.go:117-120`). A non-nil handler error returns immediately — no banner, no auto-sync (`cli.go:121-123`).
3. On success and `accessMode == app.AccessWrite`: `printMutationSyncStalenessWarning(stdout, ws, time.Now())` (`cli.go:136`), after the engine close, before auto-sync.
4. `maybeAutoSyncAfterCommand(ctx, accessMode, ws)` (`cli.go:145`).

Read commands that additionally print the store-backed banner: `internal/cli/cli.go:870`, `internal/cli/next.go:53`, `internal/cli/workable.go:137` (i.e. `show`-family, `next`, and `backlog`/workable views).

### 0.7 Duration formatting

`internal/cli/output.go:451-462` `humanizeCoarseDuration`: `>=48h` → "N days"; `>=2h` → "N hours"; `>=2m` → "N minutes"; else "under a minute". Used by every age line in this inventory.

---

## 1. `lit init`

Handler `runInit` — `internal/cli/init.go:27`.

### 1.1 Flags

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--skip-hooks` | `false` | Skip git hook installation | `init.go:29` |
| `--skip-agents` | `false` | Skip AGENTS.md integration update | `init.go:30` |

Any positional argument → `UsageError{"usage: lit init [--skip-hooks] [--skip-agents]"}`, exit 2 (`init.go:34-36`).

### 1.2 Sequence

1. **Remote adopt decision runs BEFORE any store is created** — `adoptRemoteTicketsOnInit(ctx, ws)` (`init.go:44`). Comment at `init.go:38-43` states the store must not pre-exist so a clone is the path's first writer.
2. `recordInitSyncTrace(ws, syncOutcome, time.Now())` (`init.go:45`) — always, for every outcome.
3. If outcome state is `initSyncFailed`, **hard stop with no store created** (`init.go:60-74`): error text
   `"could not confirm the workspace state, so init is refusing to create a fresh store: <error> (<buildNote>)"` → exit 1.
4. Otherwise, unless adopted, `store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID)`; `dbCreated` is its `created` result (`init.go:81-88`). Adopted ⇒ `dbCreated` stays `true` (`init.go:81`).
5. Hooks (unless `--skip-hooks`): `installHooks(ws)`; error aborts init (`init.go:101-111`). Report field is `"installed"` when `Changed`, else `"unchanged"`.
6. Agents (unless `--skip-agents`): `ensureLinksAgentFiles(ws.RootDir)`; error aborts (`init.go:113-134`). Per-file status `"created"` / `"updated"` / `"unchanged"`, plus `AgentsSource` / `ClaudeSource` = the template layer (`project`/`global`/`embedded`).
7. `buildNote := resolveBuildStatusNote(time.Now())` then `writeInitHumanOutput` (`init.go:143-144`).

### 1.3 Adopt decision machine (`internal/cli/init_sync.go`)

Outcome states (`init_sync.go:32-50`): `has_local_tickets`, `not_configured`, `remote_empty`, `no_remote_data`, `adopted`, `failed`.
Outcome struct fields JSON-tagged `state`, `remote`, `branch`, `error` (`init_sync.go:55-60`).

- Timeout: `adoptRemoteTimeout = 120 * time.Second` (`init_sync.go:24`), a `var` so tests can shorten it.
- `adoptRemoteTicketsOnInit` prints progress `init: checking whether a git remote already carries a lit backlog` (`init_sync.go:95`), runs the adopt in a goroutine, and on deadline returns `initSyncFailed` with the message beginning `"adopting the remote backlog exceeded 120s and was aborted — the store is not usable yet…"` including "Pushing from this workspace would risk the remote backlog…", "Retry `lit init` — the interrupted download's leftovers are set aside automatically on retry" (`init_sync.go:103-116`). Abandoned goroutine is reclaimed at process exit (`init_sync.go:91-93`).
- Planning (`planRemoteAdopt`, `init_sync.go:179-261`), in order:
  1. `store.PendingAdopt(ws.DatabasePath)` read; if residue exists, every *benign* terminal below is converted into `initSyncFailed` carrying the residue error (`init_sync.go:191-197`).
  2. `store.LocalHasTickets` error → `failed`; `true` → `has_local_tickets` (`init_sync.go:203-209`).
  3. `workspace.GitRemotes` error → `failed` (`init_sync.go:213-216`).
  4. `resolveSyncRemote("", workspace.UpstreamRemote(...), gitRemotes)` error → `failed`; empty → `not_configured` (`init_sync.go:217-223`).
  5. `workspace.RemoteHasRefs` error → `failed`; false → `remote_empty` (`init_sync.go:226-232`).
  6. `resolveSyncBranch` error → `failed` (`init_sync.go:233-236`).
  7. `workspace.RemoteHasDoltData` (checks `refs/dolt/*`) error → `failed`; false → `no_remote_data` (`init_sync.go:244-250`).
  8. `gitBackedURLForRemote` empty → `failed` with `"remote %q carries lit data but its git URL could not be resolved for clone"` (`init_sync.go:251-259`).
- When a plan exists: progress `init: remote <remote>/<branch> carries lit data (refs/dolt/*); downloading the backlog now` (`init_sync.go:134`), then `store.AdoptRemoteByClone(ctx, DatabasePath, WorkspaceID, remote, url, branch)` (`init_sync.go:141`). A clone failure returns `initSyncFailed` with `"remote %q carries lit ticket data (refs/dolt/*) but adopting it did not complete, so the local store is not usable yet… Retry \`lit init\`; underlying error: %v"` (`init_sync.go:146-157`).
- Benign terminals also emit one progress line plus the build note (`init_sync.go:125-131`), text from `remoteSituationLine` (`init_sync.go:271-284`):
  - `has_local_tickets` → "local store already holds tickets; leaving it untouched (ongoing sync handles updates)"
  - `not_configured` → "no eligible git remote; starting with an empty backlog"
  - `remote_empty` → "remote has no refs yet (brand-new repo); starting with an empty backlog"
  - `no_remote_data` → "remote has git refs but no lit data; starting with an empty backlog"
  - `failed` → "" (deliberately silent; the command error is the sole channel).

### 1.4 Init sync trace

`recordInitSyncTrace` (`init_sync.go:333-354`) always writes a sync trace with `Command: "lit init"`, `Decision: <state>`, `Status: "error"` iff state is `failed` else `"ok"`, `Reason: outcome.Error`, `BuildNote`, metadata `{remote, sync_branch}`. Written before `EnsureDatabase`/hooks/agents, so it records only the adopt decision (`init_sync.go:326-329`). A trace write failure goes to stderr, not fatal.

### 1.5 Human output (`writeInitHumanOutput`, `init.go:175-226`)

- Line 1: `Initialized lit workspace` when `DBCreated`, else `lit workspace already initialized` (`init.go:195-203`).
- Line 2 (only when adopted): `  Pulled existing backlog from <remote>/<branch> (<buildNote>)` (`init_sync.go:310-316`). No line for any other state.
- Then, as applicable:
  - `  Updated: <entries>` for statuses `created|updated|installed` (`init.go:186-187`, `init.go:207-211`)
  - `  Up to date: <entries>` for `unchanged` (`init.go:213-216`)
  - `  Skipped: <entries>` for `skipped` (`init.go:217-221`)
  - Entry labels: `pre-push hook`, `AGENTS.md`, `CLAUDE.md` (`init.go:176-180`); AGENTS/CLAUDE entries append ` (via project|global|embedded)` (`init.go:153-165`, `init.go:167-173`) — suppressed when status is `skipped` (`init.go:158-160`).
- Final line, always: `  Guidance: \`lit workflows\` shows the work lifecycle and the guidance active at each point (\`lit workflows edit <id-or-point>\` to customize)` (`init.go:222`).

### 1.6 `initReport` JSON shape (struct only; no JSON output path in this command)

`init.go:14-25`: `status`, `workspace_id`, `database_path`, `db_created`, `hooks`, `agents`, `claude`, `agents_source?`, `claude_source?`, `sync` (the `initSyncOutcome`).

### 1.7 What `lit init` writes to disk

1. `<git-common-dir>/links/` and `config.json` — via `workspace.Resolve` before the handler (`internal/workspace/workspace.go:165-168`).
2. Dolt store at `ws.DatabasePath` — via `store.EnsureDatabase` (`init.go:83`) or `store.AdoptRemoteByClone` (`init_sync.go:141`).
3. `<GitCommonDir>/hooks/pre-push` (dir created `0o755`; see §9).
4. `<RootDir>/AGENTS.md` and `<RootDir>/CLAUDE.md` managed sections (see §10).
5. A sync trace file under `<StorageDir>/traces/sync/` (see §7.6).
6. No git config keys are set by `init` (no `git config` write exists on this path).

---

## 2. `lit sync` family

Family usage: `usage: lit sync <status|remote|fetch|pull|push|compact|reconcile> ...` (`internal/cli/sync.go:87`).
Rows (`sync.go:88-100`): `status`, `remote`, `fetch`, `pull`, `push`, `compact`, `reconcile`, plus hidden `__mirror-bg` (`sync.go:99`, name constant `sync_bg.go:22`).

Every visible row is wrapped in `withSyncStore` (`sync.go:75-84`) which opens one sync session and defers close.

### 2.1 Session opening

`openSyncSession` (`sync.go:52-66`): `engine.Open(ctx, engine.Sync, DatabasePath, WorkspaceID)`, then `storage.Sync.Of(st)`. If the engine cannot sync, the store is closed and the joined error is returned — the whole family is refused at this point, not per-verb (`sync.go:57-64`).

### 2.2 `lit sync status`

`runSyncStatus` — `sync.go:622-652`. No flags. Reads remote state (`readSyncRemoteState`) and `syncer.SyncStatus(ctx)`.
Output, one line:
`version=<doltVersion> branch=<branch> head=<headCommit[ headMessage]> git=<n> dolt=<n> added=<n> updated=<n> removed=<n>` (`sync.go:639-650`). `head` is `HeadCommit` alone when `HeadMessage` is blank (`sync.go:635-638`).

### 2.3 `lit sync remote ls`

Family usage `usage: lit sync remote ls` (`sync.go:104`); only row is `ls` (`sync.go:106`).
`runSyncRemoteLs` — `sync.go:118-137`. No flags. Output:
`git=<n> dolt=<n> added=<n> updated=<n> removed=<n>` (`sync.go:129-135`).
`added/updated/removed` come from `buildRemoteSyncChanges` (`sync.go:945-973`) — sorted name lists comparing git remotes (translated via `store.GitBackedRemoteURL`) to Dolt remotes.

### 2.4 `lit sync fetch`

`runSyncFetch` — `sync.go:139-171`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--remote` | `"origin"` | Remote name | `sync.go:141` |
| `--prune` | `false` | Pass `--prune` to dolt fetch | `sync.go:142` |
| `--verbose` | `false` | Include detailed remote output | `sync.go:143` |

Behavior: reconciles Dolt remotes from git first; a reconcile failure records trace `lit sync fetch`/`error` and returns (`sync.go:147-155`). Then `syncer.SyncFetch(ctx, remote, prune)`, traced as `lit sync fetch` decision `"fetched"` with metadata `{remote}` (`sync.go:157-158`). On success, `markFetchSuccess(ws)`; a marker write failure prints `lit: fetch-success marker not written: <err>` to stderr and is non-fatal (`sync.go:162-164`).
Output: `fetched` (non-verbose) or `fetched <remote>` (verbose) (`sync.go:165-170`).

### 2.5 `lit sync pull`

`runSyncPull` — `sync.go:173-274`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--remote` | `""` | Remote name (defaults to upstream remote, then single configured remote) | `sync.go:175` |
| `--verbose` | `false` | Include detailed remote output | `sync.go:176` |

Sequence and refusals:
1. Progress: `sync pull: starting: reconciling remotes and resolving the sync source` (`sync.go:180`).
2. Remote reconcile failure → trace `lit sync pull`/`error`, return error (`sync.go:181-189`).
3. `resolveSyncRemote` error → trace, return (`sync.go:190-198`).
4. No eligible remote → trace decision `no_sync_remote`; payload `{status: skipped, reason: no_sync_remote, raw: "no upstream remote and no single configured remote; skipping sync pull"}` (`sync.go:199-207`); exit 0.
5. `RemoteHasRefs` error → error `check remote refs %q: %w`, traced (`sync.go:210-220`).
6. No refs → trace `remote_empty`; payload prints `firstPushSkipMessage` (`sync.go:221-230`).
7. `resolveSyncBranch` error → traced, returned (`sync.go:231-235`).
8. Progress `sync pull: pulling lit data from <remote>/<branch> (transfer and apply may take a moment)` (`sync.go:236`).
9. `syncer.SyncPull(ctx, remote, branch)`. Error → trace `error`, return `asSyncFailure(err)` (remote-schema-ahead becomes the contract block, exit 5) (`sync.go:237-247`).
10. On success `markFetchSuccess(ws)` (stderr on failure) (`sync.go:251-253`).
11. Held outcomes (`syncFailureFromPull`, `sync.go:287-308`): `storage.SyncPullProsePending` → class `prose_held`; `storage.SyncPullUnrelated` → class `unrelated_histories`. Both are RETURNED as `SyncFailureError` (exit 5), recorded via `recordSyncHeldTrace`, and notify the owner if the class maps to a notify kind (`sync.go:260-268`).
12. Otherwise trace with decision `= result.State`, `clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)`, print the payload (`sync.go:269-273`).

`firstPushSkipMessage` (`sync.go:27-30`): `"Skipping lit sync: remote has no refs yet. This is normal ONLY for the very first push to a brand-new empty repo. If you have pushed to this remote before, this message means something is wrong — check the remote URL, credentials, or run \`git ls-remote <remote>\`."`

Payload builder (`buildSyncPullPayload`, `sync.go:727-758`):
- `SyncPullNeverSynced` → `{status: skipped, reason: remote_branch_missing, remote, branch, next_command: "lit sync push --remote <r> --set-upstream", retry_command: "lit sync pull --remote <r>"}`.
- `SyncPullUpToDate|FastForwarded|Linearized|Ahead` → `{status: ok, state, remote, branch}`.
- Anything else → `{status: unknown, state, remote, branch}`.

Printer (`printSyncPullPayload`, `sync.go:760-819`):
- `skipped`+`no_sync_remote`: nothing when not verbose; `skipped sync pull: no eligible git remote` when verbose.
- `skipped`+`remote_empty`: always prints `firstPushSkipMessage`.
- `skipped` (branch missing): non-verbose `sync pull skipped; run \`<next>\`, then retry \`<retry>\``; verbose `skipped pull <r>/<b>: remote branch missing; run \`<next>\`, then retry \`<retry>\``.
- `unknown`: always prints `sync pull produced an unrecognized state "<state>" on <r>/<b>; this is a bug — please report it`.
- default: non-verbose `pulled`; verbose `pulled <r>/<b> (<state>)` or `pulled <r> (<state>)` when the branch is empty.

### 2.6 `lit sync push`

`runSyncPush` — `sync.go:359-392`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--remote` | `""` | Remote name (upstream → single-remote fallback) | `sync.go:361` |
| `--set-upstream` | `false` | Pass `-u` to dolt push | `sync.go:362` |
| `--force` | `false` | Pass `--force` to dolt push | `sync.go:363` |
| `--verbose` | `false` | Include detailed remote output | `sync.go:364` |

The explicit push uses `session.syncer.SyncCompactAndPush` — compaction is atomic with the push (`sync.go:372`); the on-change mirror uses plain `SyncPush` (`sync_bg.go:309`).
A could-not-attempt error is traced as `lit sync push`/`error` and returned (`sync.go:373-381`). A push error is passed through `asSyncFailure` (`sync.go:385-390`) — a remote-schema-ahead becomes the contract block, exit 5.

`performSyncPush` (`sync.go:490-620`), the shared orchestration for `lit sync push` and the mirror:
1. `clearMirrorPending(ws)` at **entry**, not on success (`sync.go:498`).
2. Deferred completion: on panic, records `sync push panicked: %v` through `completePushAttempt` and re-panics; otherwise records `completePushAttempt(ctx, ws, outcome, retErr)` on every return path (`sync.go:511-517`).
3. Reconcile remotes; error → return (`sync.go:518-521`).
4. `resolveSyncRemote`; error → return (`sync.go:522-529`).
5. Empty remote → trace `no_sync_remote`, outcome `{status: skipped, reason: no_sync_remote, message: "no upstream remote and no single configured remote; skipping sync push"}` (`sync.go:530-538`).
6. `RemoteHasRefs` error → `check remote refs %q: %w` (`sync.go:540-547`).
7. No refs → trace `remote_empty`, outcome `{status: skipped, reason: remote_empty, remote, message: firstPushSkipMessage}` (`sync.go:548-556`).
8. `resolveSyncBranch`; error → return (`sync.go:557-560`).
9. Run the supplied push step (`sync.go:562`).
10. Metadata via `syncPushTraceMetadata` (`sync.go:457-477`): `{remote, sync_branch}` plus `message` (trimmed `result.Message`), `maintenance` (trimmed `result.Maintenance`), `error` (pushErr) when non-empty.
11. Automation trace (only when `LNKS_AUTOMATION_TRIGGER` is set) with command `formatCommand(["sync","push","--remote",r(,"--set-upstream")(,"--force")])`, side effect `"mirror Dolt data to the configured git remote"`, status `ok|error`, reason `"managed automation requested sync push"` or the push error (`sync.go:570-585`).
12. Durable sync trace, unconditional: decision `pushed` or `error`, reason empty or the push error (`sync.go:595-608`).

Push payload (`syncPushOutcome.payload`, `sync.go:417-439`): always `{status, remote, branch, raw}`; if skipped adds `reason` and returns; else adds `push_status` (int64), optional `maintenance`, optional `trace_ref` (path), optional `trace_error`.

Printer (`printSyncPushPayload`, `sync.go:821-867`):
- Any non-empty `maintenance` is printed FIRST, in both verbose and non-verbose modes (`sync.go:834-838`).
- `skipped`+`remote_empty` → always `firstPushSkipMessage`.
- Non-verbose skipped → nothing.
- Non-verbose otherwise → `pushed`.
- Verbose: the trimmed `raw` engine output if non-empty; else `skipped sync push: no eligible git remote` for skipped; else `pushed <r>/<b>` or `pushed <r>`.

### 2.7 `lit sync compact`

`runSyncCompact` — `sync.go:317-357`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--full` | `false` | "Rewrite the old generation too — reclaims what earlier passes archived, at a cost proportional to the whole store" → selects `storage.GCFull` instead of `storage.GCNewGen` | `sync.go:319`, `sync.go:326-329` |

Requires no remote by design (`sync.go:312-316`). On error: trace `lit sync compact`/`error`, return the error (`sync.go:331-334`). On success: `recordCompactionSuccess(ws, "lit sync compact", outcome)` recorded BEFORE printing (`sync.go:347`), then `compacted (<depth>): <detail>` to stdout; a stdout write failure is the command's failure (`sync.go:355-356`).

### 2.8 `lit sync reconcile` family

Family usage: `usage: lit sync reconcile [resolve --resolve ID:FIELD:FINGERPRINT=TEXT ... | abort | take local|remote | combine]` (`sync_reconcile_cmd.go:34`). Rows: `resolve`, `abort`, `take`, `combine` (`sync_reconcile_cmd.go:35-40`).
Dispatch (`sync_reconcile_cmd.go:48-57`): a first arg not starting with `-` routes to a subcommand; otherwise (no args, or a leading flag) the bare show path runs.

`reconcilerFor` (`sync_reconcile_cmd.go:69-76`) resolves `storage.Reconcile.Of(session.engine)`; a decline is traced under the requesting command and returned.
`guardReconcileInput` (`sync_reconcile_cmd.go:82-87`): any positional → `UsageError{"<cmd> takes no positional arguments; got \"<arg>\""}` (exit 2) — applied to bare show, `resolve`, `abort`, `combine`.

`freshReconcileTarget` (`sync_reconcile_cmd.go:593-623`) — shared pre-step: reconcile remotes → resolve remote (empty ⇒ ok=false) → `RemoteHasRefs` (error wrapped `check remote refs %q`; false ⇒ ok=false) → resolve branch → `syncer.SyncFetch(ctx, remote, false)` (error wrapped `fetch %q before reconcile`) → `markFetchSuccess`.
`ok=false` at every command prints `nothing to reconcile: no remote with shared ticket history yet` and traces decision `nothing_to_reconcile` (e.g. `sync_reconcile_cmd.go:112-116`).

#### 2.8.1 bare `lit sync reconcile`
`runSyncReconcileShow` — `sync_reconcile_cmd.go:95-123`. No flags. Trace command constant `"lit sync reconcile"` (`sync_reconcile_cmd.go:24`). `reconciler.SyncReconcile(ctx, remote, branch)`; error → traced, `asSyncFailure(err)`. Otherwise `reportReconcileResult(..., resolved=false)`.

#### 2.8.2 `lit sync reconcile resolve`
`runSyncReconcileResolve` — `sync_reconcile_cmd.go:129-165`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--resolve` (repeatable `StringArray`) | none | "Merged text for one diverged field, as ISSUE_ID:FIELD:FINGERPRINT=TEXT (repeat for every pending field)" | `sync_reconcile_cmd.go:131` |

Zero `--resolve` values → `UsageError{"sync reconcile resolve needs at least one --resolve ID:FIELD:FINGERPRINT=TEXT"}` (`sync_reconcile_cmd.go:138-140`). Parsed by `parseProseResolutions`. Trace command is `proseResolveCommand` (defined in `prose_pending.go`). Calls `SyncReconcileResolved`; then `reportReconcileResult(..., resolved=true)`, which prefixes the pending render with `the divergence changed since you read it; your resolutions were not applied. Re-merge the CURRENT conflicts below:` (`sync_reconcile_cmd.go:490-494`).

#### 2.8.3 `lit sync reconcile abort`
`runSyncReconcileAbort` — `sync_reconcile_cmd.go:173-187`. No flags. Traces `lit sync reconcile abort`/`aborted`. Prints and exits 0:
`reconcile deferred: the clone remains diverged and usable; a later command re-surfaces the divergence, or run \`lit sync reconcile\` when ready` (`sync_reconcile_cmd.go:185`).

#### 2.8.4 `lit sync reconcile take <local|remote>`
`runSyncReconcileTake` — `sync_reconcile_cmd.go:199-259`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--owner-approved` | `""` | "Owner-issued approval token for this exact divergence and side (printed by the refusal this command gives without it)" | `sync_reconcile_cmd.go:201` |

- Exactly one positional required, else `UsageError{"sync reconcile take needs exactly one side: 'local' (keep your backlog) or 'remote' (adopt theirs)"}` (`sync_reconcile_cmd.go:205-207`).
- Side parsing is case-insensitive/trimmed; anything else → `UsageError{"sync reconcile take: unknown side %q; want 'local' or 'remote'"}` (`sync_reconcile_cmd.go:300-309`).
- Trace command is `"lit sync reconcile take local"` / `"… take remote"` (`sync_reconcile_cmd.go:214`).
- `SyncResolveUnrelated(ctx, remote, branch, choice, trimmed token)`. A `store.OwnerApprovalRequiredError` → trace decision `owner_approval_required` with status `ok`, notifies the owner with an `unrelated_histories` event, returns `ownerApprovalRefusalError` → exit 5 (`sync_reconcile_cmd.go:230-254`). Any other error → traced, `asSyncFailure`.

Outcome rendering (`reportTakeOutcome`, `sync_reconcile_cmd.go:360-406`). Durable trace metadata `{remote, sync_branch, replayed}`; reason from `takeReasonForState` (`sync_reconcile_cmd.go:316-327`); an unmapped state becomes decision/status `error` with reason `unexpected result state %q`.
- `TookRemote`: clears divergence notify kinds; prints
  `took remote: the local backlog now equals <r>/<b> and sync is clean (no push needed).` newline `DISCARDED the local-only issue(s), by design: <idset>`.
- `TookLocal`: clears; prints `took local: your backlog now sits on top of <r>/<b> — <N local commit(s)> replayed with original messages and timestamps; run \`lit sync push\` (or let auto-sync) to fast-forward the remote onto it.` newline `DISCARDED the remote-only issue(s), by design: <idset>`.
- `NotDiverged`: clears; prints `nothing to reconcile: the clone is not diverged from the remote`.
- Any other state → `fmt.Errorf("sync reconcile take: unexpected result state %q — this is a bug; please report it")`, exit 1.

`describeIDSet` renders `(0)` for an empty set, else `(N): id, id…` (`sync_failure.go:380-385`). `describeReplayed` renders `1 local commit` or `N local commits` (`sync_reconcile_cmd.go:412-417`). `discardedIDs` maps `TakeRemote→OnlyLocal`, `TakeLocal→OnlyRemote` (`sync_reconcile_cmd.go:422-434`).

#### 2.8.5 `lit sync reconcile combine`
`runSyncReconcileCombine` — `sync_reconcile_cmd.go:266-294`. No flags. Trace command `"lit sync reconcile combine"` (`sync_reconcile_cmd.go:25`). Calls `SyncReconcileCombine`, then `reportReconcileResult(..., resolved=false)`.

#### 2.8.6 Shared reconcile reporter
`reportReconcileResult` — `sync_reconcile_cmd.go:450-564`. Metadata always `{remote, sync_branch, replayed}`.
- `SyncReconcileUnrelated` → builds `SyncFailureError{Class: unrelated_histories, Remote, Branch, Ahead, Behind, Inventory, BuildNote}`, records a held trace, notifies the owner, RETURNS it (exit 5, block printed by the error sink) (`sync_reconcile_cmd.go:455-473`).
- `SyncReconcileProsePending` → metadata gains `pending: <count>`; trace decision `prose_pending` status `ok`; notifies owner; prints `renderProsePendingGuidance(stdout, result.Pending, buildNote)`; returns `MergeConflictError{"reconcile holds N free-text field(s) for inline merge; run \`<proseResolveCommand>\` with your merged text"}` → exit 5 (`sync_reconcile_cmd.go:474-509`).
- `SyncReconcileLinearized` → trace, clear notify, print `reconciled: the divergence merged into linear history — <N local commit(s)> replayed with original messages and timestamps; the next push fast-forwards`, then `reportContestedLanes` (`sync_reconcile_cmd.go:510-519`).
- `SyncReconcileCombined` → trace, clear notify, print a 4-line block: `combined: unioned both backlogs onto <r>/<b> — <N local commit(s)> replayed …; run \`lit sync push\` (or let auto-sync) to fast-forward the remote onto it.` then `  kept local-only:  …`, `  kept remote-only: …`, `  field-merged on both: …`; then `reportContestedLanes` (`sync_reconcile_cmd.go:520-543`).
- `SyncReconcileNotDiverged` → trace, clear notify, `nothing to reconcile: the clone is not diverged from the remote` (`sync_reconcile_cmd.go:544-548`).
- default → trace decision/status `error`, reason `unrecognized reconcile result state %q`; prints `reconcile completed with state <state>`; returns nil (exit 0) (`sync_reconcile_cmd.go:549-562`).

Trace reasons for the explicit commands (`reconcileCommandReasonForState`, `sync_reconcile_cmd.go:337-350`): linearized → "reconciled: the divergence merged into linear history"; prose_pending → "every field resolved but free-text diverged on both sides; held for inline merge"; combined → "combined: unioned both backlogs, replaying the local commits with their provenance"; not_diverged → "the clone is not diverged from the remote; nothing to reconcile"; default → "reconcile completed with state <s>".

### 2.9 Remote and branch resolution (shared by every sync surface)

`resolveSyncRemote` — `sync.go:654-674`:
- Explicit remote that is not among the configured git remotes → error `requested remote %q not found in configured git remotes` (`sync.go:658-660`).
- Otherwise precedence: validated upstream remote (from `workspace.UpstreamRemote`), then the single configured remote when exactly one exists; else `""` (`sync.go:663-673`).

`resolveSyncBranch` — `sync.go:689-716`:
- Env override `LINKS_DEBUG_DOLT_SYNC_BRANCH` (`sync.go:20`, `sync.go:690`) takes precedence over `workspace.DefaultRemoteBranch`.
- Empty result: if `ctx.Err() != nil` → `resolve sync branch for remote %q: <ctx err>`; else `resolve sync branch for remote %q: default branch unavailable; configure LINKS_DEBUG_DOLT_SYNC_BRANCH to override` (`sync.go:706-714`).

`syncDoltRemotesFromGit` — `sync.go:897-943`: for every git remote, adds a Dolt remote (`store.GitBackedRemoteURL(url)`) when missing, or removes+re-adds when the URL differs; removes any Dolt remote with no matching git remote; re-lists at the end.

---

## 3. Background sync engine

### 3.1 The scheduling owner

`maybeAutoSyncAfterCommand` — `sync_cadence.go:78-101`. Called only from `runWithApp` after a **successful** command and after the engine is closed (`cli.go:145`).
1. `LIT_DISABLE_AUTO_SYNC` truthy → return, nothing scheduled (`sync_cadence.go:34`, `sync_cadence.go:79-81`). Truthiness accepts `1/0/t/f/true/false` case-insensitive; anything unparseable (including empty) is false (`sync_cadence.go:307-313`).
2. `config.Load` failure → stderr `lit: automatic sync skipped, config unreadable: <err>` and return (`sync_cadence.go:82-86`).
3. `shouldSyncAfterMutation(accessMode, cfg.Sync.Cadence)` — true only when `accessMode == app.AccessWrite` AND cadence == `on-change` (`sync_cadence.go:62-64`) → `ensureMirrorCoverage`.
4. `cfg.Sync.Receive` → `receiveInline` (`sync_cadence.go:90-92`).
5. `accessMode == app.AccessWrite` → `compactInline`, last, so it collects what the receive brought in (`sync_cadence.go:93-100`).

Config defaults (`internal/config/config.go:225-227`): `sync.cadence = "on-change"`, `sync.receive = true`, `sync.owner_notify_cmd = ""`. Legal cadences are `on-push` and `on-change` (`config.go:127-150`); an unknown value fails config load with `config: sync.cadence must be one of …, got %q` (`config.go:252-254`).

### 3.2 Mirror coverage / spawn decision

`ensureMirrorCoverage` — `sync_cadence.go:122-202`:
1. Remote-absent debounce: `shouldRunNow(remoteAbsentMarkerPath(ws), now, remoteAbsentRecheckInterval)`; `remoteAbsentRecheckInterval = 10 * time.Second` (`sync_cadence.go:55`). A recently-confirmed remote-less workspace short-circuits with no marker churn and no git subprocess (`sync_cadence.go:127-129`).
2. `claimMirrorPending(ctx, ws, time.Now())`:
   - error → stderr `lit: mirror-pending marker unavailable (<err>); spawning a mirror regardless`, proceeds without owning the claim (`sync_cadence.go:134-137`).
   - `pendingCovered` → return, no spawn (`sync_cadence.go:138-139`).
   - else the command owns the claim (`sync_cadence.go:140-141`).
3. `workspaceHasGitRemote` error → release claim, stderr `lit: on-change background push not started, could not check git remotes: <err>`, and `completePushAttempt(ctx, ws, {}, fmt.Errorf("check git remotes before on-change mirror spawn: %w", err))` (`sync_cadence.go:172-178`).
4. No remote → release claim, write the remote-absent marker (stderr `lit: remote-absent marker not written: <err>` on failure); the push-outcome marker is untouched (`sync_cadence.go:179-190`).
5. Has a remote → remove any stale remote-absent marker (stderr `lit: remote-absent marker not cleared: <err>` for non-ENOENT) (`sync_cadence.go:192-196`).
6. `spawnBackgroundMirror(ws, os.Getpid())`; failure → release claim, stderr `lit: on-change background push not started: <err>`, and `completePushAttempt(..., fmt.Errorf("spawn on-change mirror: %w", err))` (`sync_cadence.go:197-201`).
7. On a successful spawn the answering beacon hold is deliberately abandoned to process exit (`sync_cadence.go:150-159`).

### 3.3 Mirror-pending marker protocol (`sync_mirror_pending.go`)

- Path `<StorageDir>/mirror-pending` (`sync_mirror_pending.go:84-86`).
- `claimMirrorPending` (`sync_mirror_pending.go:117-185`):
  - `MkdirAll(StorageDir, 0o755)`; failure → `pendingClaimed` + error `ensure storage dir for mirror-pending marker` (`:118-120`).
  - `O_CREATE|O_EXCL|O_WRONLY, 0o644` create succeeds → `pendingClaimed` + a beacon answering hold (`:122-136`). A close failure removes the just-created marker and returns an error `close mirror-pending marker: %w` (`:124-135`).
  - A non-`ErrExist` create error → `claim mirror-pending marker: %w` (`:138-140`).
  - Marker exists → `store.ProbeMirrorBeacon(DatabasePath)`. Probe error is returned (`:141-144`). `store.BeaconAnswered` → `pendingCovered` (`:145-159`). `BeaconUnheld` or `BeaconObstructed` → re-claim: `os.Chtimes(path, now, now)`; a marker that vanished under the refresh yields `pendingCovered` (`:178-181`); any other chtimes error → `refresh reclaimed mirror-pending marker: %w` (`:182`); success → `pendingClaimed` + answering hold (`:184`).
- `answerForClaim` (`sync_mirror_pending.go:211-225`): `store.HoldMirrorBeacon`; failure prints `lit: mirror beacon not held by claimant (<err>); racing claims may spawn redundant mirrors` and returns a no-op release. Successful holds are pinned in a package slice so GC cannot close the fd (`sync_mirror_pending.go:199-202`, rationale `:186-198`).
- `clearMirrorPending` (`sync_mirror_pending.go:238-242`): removes the marker; `ErrNotExist` is normal; any other error → stderr `lit: mirror-pending marker not cleared: <err>`.
- `MirrorOwed(ws) (bool, error)` — exported (`sync_mirror_pending.go:256-265`): marker existence; an unreadable marker is an error (`read mirror-pending marker: %w`), never a false.
- `recheckMirrorPending(ws, cycleStart)` (`sync_mirror_pending.go:282-296`): absent → `(false,nil)`; stat error → `re-check mirror-pending marker: %w`; a marker whose mtime is BEFORE `cycleStart` → error `mirror-pending marker from <t> survived this cycle's clear (started <t>); stopping rather than cycling against a marker that cannot be removed`; otherwise `(true,nil)`.

### 3.4 Spawn and detach mechanics (`sync_bg.go`)

- Hidden subcommand name `__mirror-bg` (`sync_bg.go:22`); registered hidden in `syncFamily` (`sync.go:99`), absent from usage and completion.
- `spawnBackgroundMirror(ws, parentPID)` — `sync_bg.go:67-94`:
  - `os.Executable()`; failure → `resolve lit binary: %w`.
  - `exec.Command(self, "sync", "__mirror-bg", "--parent-pid", <pid>)`, `cmd.Dir = ws.RootDir`, `cmd.Stdin = nil`.
  - Log sink `<StorageDir>/mirror.log` opened `O_CREATE|O_WRONLY|O_APPEND, 0o644` (`sync_bg.go:27`, `sync_bg.go:79`); on failure prints `lit: on-change mirror log unavailable (<err>); worker output will be discarded` and spawns anyway with discarded streams (`sync_bg.go:80-84`).
  - `cmd.SysProcAttr = detachSysProcAttr()` — POSIX `&syscall.SysProcAttr{Setsid: true}` (`detach_posix.go:17-19`); Windows returns an empty struct, and the file notes lit has no Windows build because embedded Dolt does not compile there (`detach_windows.go:14-16`).
  - `cmd.Env = mirrorEnv()`; then `cmd.Start()`; the parent's log fd is closed after start (`sync_bg.go:87-93`).
- `mirrorEnv()` — `sync_bg.go:104-128`: copies the parent env with `LNKS_AUTOMATION_TRIGGER=`, `LNKS_AUTOMATION_REASON=`, `LNKS_AUTOMATION_TRACE_REF_FILE=` prefixes stripped, then appends `LNKS_AUTOMATION_TRIGGER=on-change` and `LNKS_AUTOMATION_REASON=on-change cadence mirrored after a mutating command`. The mirror carries no trace-ref file.
- Timing constants (`sync_bg.go:29-58`):
  - `parentPostSpawnTail = receiveTimeout(15s) + ownerNotifyHookTimeout(10s) + ownerNotifyPipeWaitDelay(1s) + compactTimeout(45s)` = 71s.
  - `mirrorParentWaitMargin = 30 * time.Second`.
  - `mirrorParentWaitTimeout = parentPostSpawnTail + mirrorParentWaitMargin` (101s).
  - `mirrorParentPollDelay = 20 * time.Millisecond`.

### 3.5 The detached worker

`runBackgroundMirror` — `sync_bg.go:145-255`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--parent-pid` | `0` | "PID of the spawning command; the mirror waits for it to exit" | `sync_bg.go:147` |

Flag parse output is `io.Discard` (`sync_bg.go:148`).
1. `store.HoldMirrorBeacon(ctx, DatabasePath)` — shared hold from entry until process death. Failure with `ctx.Err()` set → `teardownMirror`; otherwise `completeMirrorWithoutAttempt(..., "hold mirror liveness beacon: %w")` (`sync_bg.go:162-167`).
2. `stopAnswering` is a `sync.Once` release; a release failure prints `lit: mirror beacon not released (<err>); concurrent claims may read this dying mirror as live until process exit` (`sync_bg.go:169-183`).
3. `waitForParentExit(ctx, parentPID, os.Getppid, 101s, 20ms)` (`sync_bg.go:191`). Semantics (`sync_bg.go:350-362`): `parentPID <= 0` → true immediately; loops while `getppid() == parentPID`; returns false on `ctx.Err()` or deadline. Timeout (ctx not done) → `completeMirrorWithoutAttempt` with `spawning command (pid %d) still running after %s; skipping mirror to avoid racing its engine` (`sync_bg.go:191-198`).
4. Cycle loop (`sync_bg.go:200-254`):
   - `ctx.Err()` non-nil at loop top → `teardownMirror`.
   - `store.TryAcquireSyncPushLock(DatabasePath)`; error → `completeMirrorWithoutAttempt("acquire sync-push lock: %w")`.
   - Not acquired (lost single-flight) → return nil **with no store opened, no trace, no file created** (`sync_bg.go:203-215`, rationale `sync_bg.go:133-144`).
   - `cycleStart := time.Now()`; `mirrorCycle`; release the lock (unlock error ignored).
   - `mirrorCycle` false (failure already completed through the push-outcome seam) → return nil, no hot-spin (`sync_bg.go:227-232`).
   - `recheckMirrorPending(ws, cycleStart)`: error → `recordMirrorTraceError` and stop; `again == false` → stop; `true` → another full cycle on a fresh engine.
5. `mirrorCycle` (`sync_bg.go:283-292`): opens a sync session; open failure → `completeMirrorWithoutAttempt("open sync store: %w")` and returns false; else runs `mirrorOnce` and returns true.
6. `mirrorOnce` (`sync_bg.go:306-332`): `performSyncPush(ctx, session, ws, "", false, false, session.syncer.SyncPush)` — no `--remote`, no `-u`, no `--force`, no compaction. A could-not-attempt error → `recordMirrorTraceError`. A non-nil `outcome.traceErr` → stderr `lit: on-change mirror trace not recorded: <err>`. A remote-schema-ahead push error prints `failure.blockString()` to stderr (`sync_bg.go:329-331`); any other push error is left in the trace (retried by the next push).
7. `teardownMirror` (`sync_bg.go:270-275`): `stopAnswering()` FIRST, then `clearMirrorPending`, then `recordMirrorTraceError(cause)`; deliberately writes NO push-outcome record.
8. `completeMirrorWithoutAttempt` (`sync_bg.go:394-400`): `stopAnswering()`, `clearMirrorPending`, `completePushAttempt(ctx, ws, syncPushOutcome{}, cause)`, `recordMirrorTraceError(cause)`; always returns nil (the mirror never exits nonzero).
9. `recordMirrorTraceError` (`sync_bg.go:411-427`): automation trace `lit sync push` / side effect `mirror Dolt data to the configured git remote` / status `error` / metadata `{error}`; a trace-write failure prints `lit: on-change mirror could not record failure trace (<traceErr>); original error: <cause>`; plus the durable sync trace `lit sync push`/`error`.

### 3.6 Inline receive (`sync_receive.go`)

`receiveTimeout = 15 * time.Second` (`sync_receive.go:19`). `receiveDebounceInterval = 5 * time.Minute` (`sync_cadence.go:45`).

`receiveInline` — `sync_receive.go:31-73`:
1. Debounce on `<StorageDir>/receive.last` mtime (`sync_cadence.go:265-273`); not due → return.
2. Mark the attempt BEFORE any work; failure → stderr `lit: automatic receive debounce marker not written: <err>` (`sync_receive.go:38-40`).
3. `workspaceHasGitRemote` error → `recordReceiveError("check git remotes: %w")`; no remote → silent return (`sync_receive.go:41-50`).
4. Open a sync session under a 15s timeout ctx; open failure → `recordReceiveError("open sync store: %w")` (`sync_receive.go:52-58`).
5. `performSyncReceive`; a could-not-attempt error → `recordReceiveError`. `outcome.traceErr` → stderr `lit: automatic receive trace not recorded: <err>` (`sync_receive.go:61-71`).
6. `surfaceInlineOutcome(ctx, ws, outcome, time.Now())` — passed the COMMAND ctx, not the 15s one (`sync_receive.go:72`, rationale `:89-92`).

`performSyncReceive` — `sync_receive.go:209-311`: reconcile remotes → resolve remote (`""` ⇒ `{skipped, no_sync_remote}`) → `RemoteHasRefs` (error ⇒ `check remote refs %q: %w`; false ⇒ `{skipped, remote_empty, remote}`) → resolve branch → `syncer.SyncReceive(ctx, remote, branch)`.
- `markFetchSuccess` on any nil receive error, whatever the resulting state (`sync_receive.go:244-251`).
- Trace metadata `{remote, sync_branch, state, ahead, behind}` plus `error` on failure (`sync_receive.go:252-265`); automation trace command `lit sync receive`, side effect `receive Dolt data from the configured git remote`; then the unconditional durable trace with decision `= state` or `"error"` (`sync_receive.go:268-292`).
- Reason strings (`receiveReasonForState`, `sync_receive.go:415-430`): fast_forwarded → "automatic receive fast-forwarded the local store to the remote head"; up_to_date → "…already up to date with the remote"; ahead → "…found local ahead of the remote; nothing to receive"; diverged → "…found local diverged from the remote; left for foreground reconcile"; never_synced → "…found no remote-tracking data on this branch yet"; default → "automatic receive completed with state <s>".
- When `receiveErr == nil` and state is `SyncReceiveDiverged`, an inline reconcile runs on the SAME engine (`sync_receive.go:307-309`).

`performInlineReconcile` — `sync_receive.go:338-392`: `storage.Reconcile.Of(session.engine)` then `SyncReconcile` (`reconcileOnce`, `sync_receive.go:324-330`); a capability decline surfaces as an ordinary reconcile error. Trace metadata `{remote, sync_branch, state, replayed}` plus `error` or `pending`. Automation trace command `lit sync reconcile`, side effect `reconcile a diverged clone into linear history with the field-aware merge engine`; trace-write failure → stderr `lit: automatic reconcile trace not recorded: <err>`. Plus the unconditional durable trace. Reasons from `reconcileReasonForState` (`sync_receive.go:396-409`): linearized → "automatic reconcile merged the divergence into linear history"; prose_pending → "automatic reconcile resolved every field but free-text diverged on both sides; held for the agent surface"; unrelated → "automatic reconcile found unrelated histories (no common ancestor); held for wholesale/union resolution"; not_diverged → "automatic reconcile found the branch no longer diverged; nothing to do".

`surfaceInlineOutcome` — `sync_receive.go:93-104`: a non-converging outcome prints `failure.blockString()` to **stderr** and notifies the owner; a cleanly settled outcome clears the divergence notify kinds. The command's exit code is unaffected.
`inlineSyncFailure` — `sync_receive.go:129-169`: no reconcile → not a failure; reconcile error that is a remote-schema-ahead → that class; other reconcile error → `diverged_unresolved` with `Cause`; `SyncReconcileProsePending` → `prose_held` with `Fields`; `SyncReconcileUnrelated` → `unrelated_histories` with `Inventory`.
`settledCleanly` — `sync_receive.go:111-120`: `status == "ok"`, no receive error, and either no reconcile or a reconcile with no error whose state is `Linearized` or `NotDiverged`.

### 3.7 Compaction backstop (`sync_compact.go`)

- `compactProbeInterval = 15 * time.Minute` (`sync_compact.go:50`); `compactTimeout = 45 * time.Second` (`sync_compact.go:61`).
- Marker `<StorageDir>/compact.last` (`sync_compact.go:67-69`).
- `compactInline` — `sync_compact.go:81-117`: not due → return. Marks the attempt BEFORE running (so a store failing every pass is asked once per interval); a marker failure prints `lit: compaction probe interval not recorded: <err>` and proceeds anyway (`sync_compact.go:88-94`). Opens a sync session under a 45s timeout; open failure → `recordCompactError("open store for compaction: %w")`. `syncer.CompactIfDue(ctx)` — the engine decides whether a pass is owed; error → `recordCompactError`; `!outcome.Ran` → silent return (no trace); else `recordCompactionSuccess(ws, "compaction backstop", outcome)`.
- `compactTraceCommand = "compaction backstop"` — deliberately not a runnable command line (`sync_compact.go:173`).
- `recordCompactionSuccess` (`sync_compact.go:137-139`) records decision `"compacted"` with metadata from `compactionTraceMetadata` = `{depth: outcome.Depth.String(), detail: outcome.Detail}` (`sync_compact.go:162-167`); an empty `detail` is dropped by `compactTraceMetadata` (`automation_trace.go:122-139`).
- `recordCompactError` (`sync_compact.go:179-181`) traces `compaction backstop`/`error`.
- Note (`sync_compact.go:30-33`): the on-change mirror is never the host for compaction because `ensureMirrorCoverage` short-circuits on a remote-less workspace.

### 3.8 Push-outcome marker (`sync_push_outcome.go`)

- Decisions: `pushed`, `error`, `canceled`, `workspace_busy` (`sync_push_outcome.go:44-49`), plus the skip reasons `no_sync_remote` / `remote_empty` carried through from `syncPushOutcome.reason`.
- Record JSON: `{decision, reason?, remote?, branch?}` (`sync_push_outcome.go:59-64`); marker `<StorageDir>/push-outcome.last` (`sync_push_outcome.go:79-81`); mtime = attempt completion time.
- `failed()` is `Decision == "error"` only (`sync_push_outcome.go:72-74`).
- `pushOutcomeOf(outcome, err)` — `sync_push_outcome.go:92-121`, in order:
  1. `context.Canceled` in `err` or `outcome.pushErr` → `canceled` with the first non-nil error's text, plus remote/branch.
  2. `store.ErrWorkspaceBusy` in `err` → `workspace_busy` with err text (no remote/branch).
  3. any other `err` → `error` with err text.
  4. `outcome.pushErr != nil` → `error` with its text plus remote/branch.
  5. `outcome.status == "skipped"` → decision = `outcome.reason`, remote only.
  6. default → `pushed` with remote/branch.
- `completePushAttempt` — `sync_push_outcome.go:131-135`: derives the record, writes it, and feeds `observePushOutcomeForOwner`.
- `recordPushOutcome` — `sync_push_outcome.go:142-153`: atomic temp-and-rename write of the JSON plus a newline; failure → stderr `lit: push-outcome marker not written: <err>`, never returned.
- `lastPushOutcome` — `sync_push_outcome.go:162-181`: missing marker → `ok=false` silently; other stat/read failure → stderr `lit: push-outcome marker unreadable: <err>`; bad JSON → stderr `lit: push-outcome marker corrupt: <err>`; returns record + age from mtime.

### 3.9 Staleness banners (`sync_staleness.go`)

- `unfetchedStalenessThreshold = 24 * time.Hour` (`sync_staleness.go:23`).
- Fetch-success marker `<StorageDir>/fetch-success.last` (`sync_staleness.go:31-33`), written by every successful DOLT_FETCH call site (`markFetchSuccess`, `sync_staleness.go:40-48`): `lit sync fetch`, `lit sync pull`, `freshReconcileTarget`, and the inline receive.
- `lastFetchSuccessAge` (`sync_staleness.go:62-71`): missing → `ok=false` silently; other stat error → stderr `lit: fetch-success marker unreadable: <err>` and `ok=false`.
- `syncPushFailureLines` (`sync_staleness.go:142-154`) — only when a record is known AND `failed()`:
  `sync: automatic push[ to <r>/<b>] is FAILING — last attempt <age> ago: <reason> — changes stay on this machine until a push succeeds; run 'lit sync push'`.
- `oneLineReason` (`sync_staleness.go:160-174`): first line only, trimmed, capped at 160 runes with a `…` suffix; empty → `(no reason recorded)`.
- `fetchStalenessLines` (`sync_staleness.go:119-131`) — only when the age is known and `>= 24h`:
  `sync: last successful fetch[ from <ref>] was <age> ago (over <threshold>) — run 'lit sync fetch'`.
- `syncStalenessLines` (`sync_staleness.go:96-111`) — only for a RESOLVED doctor sync report; when `State() == storage.SyncAhead`:
  `sync: <N> local change(s) not pushed to <r>/<b>, as of last fetch — run 'lit sync push'`; then the fetch-staleness line. Deliberately does NOT fire on `SyncDiverged` (that has the heavier failure block) nor special-case `SyncNeverSynced` (`sync_staleness.go:83-95`).
- `printSyncStalenessWarning` (read commands) — `sync_staleness.go:186-200`: push-failure line FIRST, then the ahead/fetch lines. Write errors are returned to the caller.
- `printMutationSyncStalenessWarning` (every write command, at the `runWithApp` seam) — `sync_staleness.go:217-228`: reads ONLY the storage-dir markers (push outcome, fetch success), emits the push-failure line then a ref-less fetch-staleness line. Write failures print `lit: staleness banner not written: <err>` to stderr and never change the exit code.

### 3.10 Sync-failure contract (`sync_failure.go`)

Classes (`sync_failure.go:20-45`): `prose_held`, `diverged_unresolved`, `remote_schema_ahead`, `unrelated_histories`.
Persistence thresholds: `persistentDivergenceAge = 24h`, `persistentDivergenceCommits = 10`; `persistent()` is `Age >= 24h || Ahead+Behind > 10` (`sync_failure.go:53-56`, `:160-164`).

`blockString()` (`sync_failure.go:185-227`) renders, in order:
```
<agent-instructions>
lit sync could not resolve a backlog divergence automatically and needs you.

<syncFailureMustNotIgnore>

WHAT HAPPENED: <whatLine>

[WHAT EACH SIDE HOLDS (issue ids):
  only on local:  …
  only on remote: …
  on both:        …
]
[<BuildNote>]

HOW TO RESOLVE (run in order):
  <step>…

<escalationLine>

[cause (backend detail, for diagnosis only — the steps above are the fix): <cause>]
</agent-instructions>
```
- The constant directive (`sync_failure.go:65`): "This is a blocking condition, not ambient noise or a routine quirk — retrying past it or routing around it will not resolve it. Resolve it now, or explicitly surface it to the user as blocking, before continuing ticket work."
- `whatLine` per class (`sync_failure.go:232-263`), including an explicit unknown-class arm: `an unrecognized sync-failure class %q on <ref> — this is a bug; please report it.`
- `resolutionSteps` (`sync_failure.go:271-314`):
  - `prose_held` → `lit sync reconcile        # shows base/ours/theirs for each held field and how to merge them inline`
  - `diverged_unresolved` → `lit sync pull …` then `lit sync reconcile …`
  - `remote_schema_ahead` → `lit upgrade --to <producer>   # install the binary that advanced the remote to schema v<N>, then retry` when a producer is named, else `lit upgrade               # install a newer lit that supports schema v<N>, then retry (the remote head names no producer version to target)`
  - `unrelated_histories` → four steps, `combine` first, then `take remote`, `take local` (both marked "DESTRUCTIVE, owner approval required"), then bare `reconcile`
  - default → `lit doctor                # unrecognized sync-failure class; report this`
- `escalationLine` (`sync_failure.go:320-345`): `remote_schema_ahead` and `unrelated_histories` have fixed BLOCKED sentences; otherwise `ESCALATION — INCIDENT: …persisted for <age> across <span> commit(s)…` when `persistent()`, else `ESCALATION — recent (<age>, <span> commit(s)): still within the window where a divergence is routine…`.
- `agePhrase` renders `an unknown duration` for zero/negative age (`sync_failure.go:350-355`).
- `ageFromOldestDivergedUnix` (`sync_failure.go:410-419`): `<= 0` or a future timestamp → 0 (unknown).
- `remoteSchemaAheadFailure` (`sync_failure.go:127-141`) adapts `*store.RemoteSchemaAheadError` (no message parsing); `asSyncFailure` (`sync_failure.go:148-153`) wraps it as a returnable `SyncFailureError` and otherwise passes the error through.
- `describeHeldFields` (`sync_failure.go:389-403`): `one or more free-text fields` / `the free-text field <id>·<field>` / `N free-text fields (…)`.

### 3.11 Owner-approval take refusal (`sync_take_approval.go`)

`ownerApprovalRefusalError.blockString()` (`sync_take_approval.go:31-57`) renders an `<agent-instructions>` block:
- Header: `lit sync reconcile take <side> is DESTRUCTIVE and did not run: it needs the owner's explicit approval.`
- `WHAT IT WOULD DO: keep the <kept> backlog wholesale and permanently discard every issue only the <dropped> side holds — <idset>.`
- The `WHAT EACH SIDE HOLDS` inventory lines.
- `WHY YOU ARE BLOCKED: choosing which side of a forked backlog survives is the OWNER's decision … Do not self-approve; approval asserted without the owner's explicit instruction is a false claim.`
- `HOW TO PROCEED (in order):` 1. `lit sync reconcile combine   # NO approval needed…` 2. `Surface this fork to the owner…` 3. `lit sync reconcile take <side> --owner-approved <token>`
- Binding line (`sync_take_approval.go:63-71`): `The token approves destroying exactly this fork (local <shortHead> vs remote <r>/<b> at <shortHead>, side <side>); any new commit on either side voids it.` plus, when `Stale`, `The token you supplied no longer matches — the backlog moved since it was issued, or it was issued for the other side. Re-read the state above and get fresh owner approval.`
- `shortHead` renders `(unknown)` for empty and truncates to 12 chars (`sync_take_approval.go:86-93`); `takeSideEffects` maps `TakeRemote → (kept=remote, dropped=local)` else `(local, remote)` (`sync_take_approval.go:76-81`).

### 3.12 Sync traces (`sync_trace.go`)

- Kind directory `<StorageDir>/traces/sync/` (`sync_trace.go:17`, `:43-45`).
- Record JSON keys (`sync_trace.go:27-38`): `id`, `recorded_at` (RFC3339Nano), `workspace_id`, `command`, `decision`, `status`, `reason?`, `trigger?`, `build_note?`, `metadata?`. `Trigger` is read fresh from `LNKS_AUTOMATION_TRIGGER` on every write (`sync_trace.go:64`).
- `recordSyncTrace` writes UNCONDITIONALLY (unlike the automation trace) via `trace.Write` with slug `trace.Slug(record.Command)` (`sync_trace.go:59-78`).
- `recordSyncTraceLogged` prints `lit: <command> trace not recorded: <err>` on write failure and never returns it (`sync_trace.go:87-91`).
- `recordSyncCommandTrace(ws, command, decision, err, metadata)` (`sync_trace.go:99-115`): on a non-nil err it forces `status="error"`, `decision="error"`, `reason=err.Error()`.
- `recordSyncHeldTrace` (`sync_trace.go:127-136`): decision = the failure class, status = `"ok"` (the operation completed), reason = `whatLine()` (never the full block), BuildNote read off the failure.

### 3.13 Automation traces (`automation_trace.go`)

- Env vars: `LNKS_AUTOMATION_TRIGGER`, `LNKS_AUTOMATION_REASON`, `LNKS_AUTOMATION_TRACE_REF_FILE` (`automation_trace.go:15-17`).
- Kind directory `<StorageDir>/traces/automation/` (`automation_trace.go:21`, `:47-49`).
- Record JSON (`automation_trace.go:24-34`): `id`, `recorded_at`, `workspace_id`, `trigger`, `command`, `side_effect`, `status`, `reason?`, `metadata?`.
- `maybeRecordAutomatedCommandTrace` (`automation_trace.go:59-84`): returns `(nil, nil)` when no trigger is set. An empty reason falls back to `LNKS_AUTOMATION_REASON`. When `LNKS_AUTOMATION_TRACE_REF_FILE` is set, the trace path plus newline is written to that file `0o644`; a write failure returns `write automation trace ref: %w`.
- `compactTraceMetadata` drops entries with an empty trimmed key or value, and returns nil for an empty map (`automation_trace.go:122-139`).
- `formatCommand(args)` produces `lit <arg> <arg>…`, skipping blanks (`automation_trace.go:110-120`).

### 3.14 Owner notifications (`owner_notify.go`)

- Constants (`owner_notify.go:26-47`): `ownerNotifyHookTimeout = 10s`, `ownerNotifyPipeWaitDelay = 1s`, `ownerNotifyCooldown = 24h`, `ownerNotifyTraceCommand = "owner-notify"`.
- Kinds: the three divergence classes plus `push_failed` (`owner_notify.go:56-65`).
- Fingerprint (`owner_notify.go:83-88`): `push_failed` keys on the kind alone; a divergence keys on `kind + " " + remote + "/" + branch`.
- Marker per kind: `<StorageDir>/owner-notify.<kind>.last`, content = fingerprint, mtime = last notify (`owner_notify.go:114-116`).
- `ownerNotifyDue` (`owner_notify.go:123-136`): true when the marker is missing, unreadable, has a different fingerprint, or the cooldown elapsed.
- `maybeNotifyOwner` (`owner_notify.go:148-189`):
  1. `LIT_DISABLE_AUTO_SYNC` truthy → return.
  2. Not due → return.
  3. `config.Load` failure → stderr `lit: owner notification skipped, config unreadable: <err>` and return.
  4. Empty `sync.owner_notify_cmd` → return.
  5. Hook failure → stderr `lit: owner notification hook failed (retries on the next detection): <err>` and a sync trace `owner-notify`/`<kind>`/status `error`; **marker is not written**, so the next detection retries.
  6. Hook success → write the marker atomically (`lit: owner-notify marker not written: <err>` on failure), then a sync trace `owner-notify`/`<kind>`/status `ok` with reason = the event summary. Metadata `{remote, sync_branch}`.
- `runOwnerNotifyHook` (`owner_notify.go:194-220`): `sh -c <hook>` via `exec.CommandContext` with a 10s deadline, `cmd.WaitDelay = 1s`, `cmd.Dir = repoRoot`, env = `os.Environ()` plus `LIT_NOTIFY_KIND`, `LIT_NOTIFY_SUMMARY`, `LIT_NOTIFY_REMOTE`, `LIT_NOTIFY_BRANCH`, `LIT_NOTIFY_REPO`. `CombinedOutput`; failure returns `<err>: <trimmed output>` when output is non-empty.
- `clearOwnerNotify(ws, kinds...)` (`owner_notify.go:227-233`): removes each kind's marker; non-ENOENT failures print `lit: owner-notify marker not cleared: <err>`.
- `observePushOutcomeForOwner` (`owner_notify.go:244-256`): decision `pushed` → clear the `push_failed` marker; `failed()` → notify with summary `a lit sync push to <target> failed: <reason> — local ticket changes are not reaching the shared backlog.` `canceled`/`workspace_busy`/skip decisions reach neither arm.
- `pushTarget` (`owner_notify.go:261-270`): `the configured remote` when the remote is empty; the remote alone when the branch is empty; else `remote/branch`.

### 3.15 Marker primitives (`sync_cadence.go`)

- `shouldRunNow(markerPath, now, interval)` (`sync_cadence.go:212-218`): a missing or unstattable marker means "allow".
- `markRunAttempt` (`sync_cadence.go:222-230`): `MkdirAll(StorageDir, 0o755)` then `os.WriteFile(marker, nil, 0o644)`; errors wrapped `ensure storage dir for debounce marker` / `write debounce marker <base>`.
- `writeMarkerAtomic` (`sync_cadence.go:243-261`): `MkdirAll`, `CreateTemp(StorageDir, base+"-*")`, write, close, rename. Used by the push-outcome and owner-notify markers.
- Marker inventory under `<StorageDir>`: `receive.last`, `remote-absent.last`, `compact.last`, `fetch-success.last`, `mirror-pending`, `push-outcome.last`, `owner-notify.<kind>.last`, `mirror.log`, `snapshots/`, `traces/{sync,automation,…}/`, `last-sync-base.json`.

### 3.16 Acceptance evidence in `cmd/lit`

- `cmd/lit/eager_push_test.go:49` `TestEagerPushOnDefaultCadenceReachesRemoteWithoutExplicitPush` — with no `[sync]` config at all, a single mutating command's change reaches a bare git remote with no explicit push; verified by an independent `dolt clone` oracle polled with a bounded deadline (`eager_push_test.go:26-47`). Skips when `git` or `dolt` is missing (`eager_push_test.go:50-55`).
- `cmd/lit/mutation_staleness_banner_test.go:34` `TestPushFailureBannerReachesMutationOnlySession` — pins that a mutation-only session sees the push-failure banner within a bounded window against an unreachable remote, and that a healthy remote produces NO banner despite the command being momentarily "ahead" (`mutation_staleness_banner_test.go:12-33`).
- `cmd/lit/sync_engine_race_test.go:49` `TestBurstOfMutationsNeverHitsEngineReadOnlyCollision` — a back-to-back burst of mutating commands must never surface Dolt's "database is read only" error, every command must exit 0, and every commit (including the last) must reach the remote with no sweep push (`sync_engine_race_test.go:15-24`).
- `cmd/lit/mirror_quiescence_test.go:76` `awaitMirrorQuiescence` — a test helper whose ordering is itself a behavioral statement: read `cli.MirrorOwed` FIRST, probe `store.ProbeMirrorBeacon` LAST; `BeaconUnheld` is kernel proof no mirror or claimant was running. Budget `mirrorQuiescencePatience = 60s`, poll `20ms` (`mirror_quiescence_test.go:20-22`). Notes that even a mirror that only loses the single-flight race creates two lock files (`mirror_quiescence_test.go:33-40`).

---

## 4. `lit doctor`

Handler `runDoctor` — `doctor.go:252-331`. Access mode is resolved from the args BEFORE the app opens: `--fix` with any value ⇒ `app.AccessWrite`, otherwise `app.AccessRead`; a flag-parse failure defaults to write (`doctor.go:206-217`).

| Flag | Default | `NoOptDefVal` | Effect | Line |
|---|---|---|---|---|
| `--fix` | `""` | `"all"` | "Apply fixes: --fix (all) or --fix rank,thingA" | `doctor.go:254-255` |

### 4.1 Fix registry

`doctorFixes` (`doctor.go:230-250`) — the single authority for valid fix names:
- `integrity` → `repairer.FixIntegrity(ctx)`; prints `Integrity repair: foreign_key_issues=<n> invalid_related_rows=<n> orphan_history_rows=<n>` (`doctor.go:231-239`).
- `rank` → `repairer.FixRankInversions(ctx)`; prints `Re-ranked <n> dependency issue(s) to repair rank order.` only when `n > 0` (`doctor.go:240-249`).

`allDoctorFixNames()` returns them sorted (`doctor.go:219-226`) → `integrity, rank`.
`--fix` (bare, i.e. `all`) runs every fix in sorted order; a comma list runs the named ones in the given order (`doctor.go:262-271`). An unknown name → `fmt.Errorf("unknown fix %q; available: integrity, rank")`, exit 1 (`doctor.go:274-276`). **Fix progress writes to `os.Stderr`, not stdout** (`doctor.go:272-278`).
The repair capability is asked once, up front: `storage.Repair.Of(ap.Store)`; a decline aborts the whole command (`doctor.go:263-266`).

### 4.2 Checks and output (stdout, in order)

1. `printWorkspaceIdentity` (`doctor.go:25-35`):
   `workspace: storage_dir="<dir>" workspace_id=<id> issue_prefix=<p> issue_prefix_source=configured|derived git_common_dir="<dir>"` — path fields quoted with `%q`; source is `derived` when `ws.IssuePrefix.Derived()`.
2. `resolveBuildStatusNote(time.Now())` on its own line (`doctor.go:296`).
3. `integrity_check=<v> foreign_key_issues=<n> invalid_related_rows=<n> orphan_history_rows=<n> rank_inversions=<n> dependency_cycle=<none|a->b->c>` (`doctor.go:299-305`).
4. `printSyncFreshness` (`doctor.go:173-204`) — one line:
   - no remote → `sync: no git remote configured — ticket history stays on this machine; add a remote and run 'lit sync push' to share it`
   - unresolved → `sync: freshness unavailable — <detail>`
   - `SyncNeverSynced` → `sync: never synced with <r>/<b> — run 'lit sync push' to publish local tickets ('lit sync pull' to receive remote ones)`
   - `SyncUpToDate` → `sync: up to date with <r>/<b> (as of last fetch)`
   - `SyncAhead` → `sync: ahead of <r>/<b> by <n> local change(s) not pushed, as of last fetch — run 'lit sync push' [ahead=<n> behind=0]`
   - `SyncBehind` → `sync: behind <r>/<b> by <n> change(s) not pulled, as of last fetch — run 'lit sync pull' [ahead=0 behind=<n>]`
   - `SyncDiverged` → `sync: diverged from <r>/<b> as of last fetch — <a> local change(s) not pushed, <b> remote change(s) not pulled; run 'lit sync pull' to reconcile [ahead=<a> behind=<b>]`
   - an unhandled state → `fmt.Errorf("unhandled sync freshness state %q")`, exit 1 (`doctor.go:201-203`).
5. `printPushOutcomeHealth` (`doctor.go:154-167`) — printed only when the last push attempt `failed()`:
   `sync: last push attempt FAILED <age> ago: <oneLineReason>[ — mirror log: <StorageDir>/mirror.log (last written <age> ago)]`. The log clause appears only if `mirror.log` stats successfully.

### 4.3 Freshness resolution and refusals

`resolveDoctorSyncFreshness` (`doctor.go:105-144`) never errors; every failure becomes a `doctorSyncUnresolved` report carrying the reason: sync-capability decline (`doctor.go:111-114`), `read git remotes: <err>` (`doctor.go:115-118`), remote-resolution error (`:121-124`), branch-resolution error (`:128-131`), `SyncFreshness` error (`:132-135`). No configured remote → `doctorSyncNoRemote` (`:125-127`). The divergence age is computed here from `freshness.OldestDivergedUnix` (`doctor.go:139-143`).

### 4.4 Exit behavior

- Any `report.Errors` → `CorruptionError{Message: strings.Join(report.Errors, "; ")}` → **exit 7**, and it wins over the divergence exit (`doctor.go:312-315`).
- A divergence whose failure is `persistent()` (age ≥ 24h or ahead+behind > 10) → `SyncFailureError{Class: diverged_unresolved, …}` → **exit 5**, block printed by the error sink, and the owner is notified for that class (`doctor.go:71-97`, `doctor.go:323-329`).
- Otherwise nil → exit 0.

---

## 5. `lit upgrade`

Handler `runUpgrade` — `upgrade.go:84-86`; production deps `workspaceSchemaReader`, `release.HTTPResolver{}`, `release.HTTPInstaller{}`, `currentBinaryPath`. It is a WORKSPACE-mode command and never opens the app store (`upgrade.go:76-83`, registered at `register.go:389-390`).

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--to` | `""` | "Target binary version (v-prefixed git tag, e.g. v0.9.0)" | `upgrade.go:196` |

Refusals and sequence (`runUpgradeWith`, `upgrade.go:186-255`):
1. Any positional → `UsageError{"usage: lit upgrade --to <version>"}`, exit 2 (`upgrade.go:200-202`).
2. `normalizeReleaseTag(*to, "upgrade")` (shared with downgrade, `downgrade.go:136-156`): empty/whitespace → `ValidationError{"upgrade: --to requires a non-empty version"}` (exit 3); a missing `v` prefix is added; a tag containing `/`, `\`, `..`, or any whitespace → `ValidationError{"upgrade: --to %q is not a valid release tag"}` (exit 3).
3. `resolver.Resolve(ctx, tag, release.CurrentPlatform())`; error returned as-is (`upgrade.go:208-212`).
4. `schema.ReadWorkspaceSchema(ctx)`; error → `upgrade: read workspace schema version: %w` (`upgrade.go:219-222`). The reader opens the store `engine.ReadOnly` (`upgrade.go:127`); a `*store.UnsupportedSchemaVersionError` is tolerated — its `WorkspaceVersion` becomes `AppliedVersion` with `Openable=false` (`upgrade.go:128-132`, `upgrade.go:161-167`). Any other open error propagates. On a clean open the store's close error is surfaced only when no read error already occurred (`upgrade.go:138-142`).
5. **Backward-move refusal, before any install**: `target.Manifest.Schema.Max < ws.AppliedVersion` → `*UpgradeTargetBehindError` (`upgrade.go:223-230`), whose message depends on `WorkspaceOpenable` (`upgrade.go:45-58`):
   - openable → `cannot upgrade to <tag>: its schema support ends at v<target> but this workspace is already at v<current> — that is a backward move; use \`lit downgrade --to <tag>\` instead (it reverses the schema before installing the older binary)`
   - not openable → `cannot upgrade to <tag>: it supports only through schema v<target> but this workspace is at v<current>, which this binary cannot open — pick an upgrade target whose schema support reaches v<current> or newer (this binary is too old to reverse the schema here, so an older target is not an option)`
   Exit 1 (plain error type).
6. `currentBinaryPath()` (`os.Executable` + `filepath.EvalSymlinks`, `downgrade.go:162-172`); error → `upgrade: resolve current binary: %w`.
7. `installer.Install(ctx, target, binPath)`; error → `upgrade: installing <tag> failed: <err>\n\nrecover by installing <tag> manually (download from <artifactURL>), then re-running lit` (`upgrade.go:237-245`).
8. Success stdout:
   `upgraded to <tag> (schema support through v<N>) installed at <binPath>` newline `the next lit run migrates this workspace forward if it trails; re-run \`lit version\` to confirm.` (`upgrade.go:250-253`).

Upgrade never touches the schema — forward migrations live in the target binary and run on its next `Open()` (`upgrade.go:61-73`).

---

## 6. `lit downgrade`

Handler `runDowngrade` — `downgrade.go:38-49`. App-mode, `app.AccessWrite` (`register.go:387-388`). Requires the `storage.SchemaMigration` capability up front; a decline aborts (`downgrade.go:44-47`).

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--to` | `""` | "Target binary version (v-prefixed git tag, e.g. v0.4.1)" | `downgrade.go:77` |

Sequence (`runDowngradeWith`, `downgrade.go:67-124`):
1. Any positional → `UsageError{"usage: lit downgrade --to <version>"}` (`downgrade.go:81-83`).
2. `normalizeReleaseTag(*to, "downgrade")` — same rules as §5 step 2 (`downgrade.go:84-87`).
3. `resolver.Resolve(ctx, tag, release.CurrentPlatform())` (`downgrade.go:89-93`).
4. `store.Downgrade(ctx, target.Manifest.Schema.Max)` — **schema is reversed before the binary is installed**; pre-snapshot refusals propagate verbatim, post-snapshot failures arrive as `*DowngradeRollbackError` whose message carries the restore instruction (`downgrade.go:21-27`, `downgrade.go:95-97`).
5. `currentBinaryPath()`; error → `downgrade: resolve current binary: %w` (`downgrade.go:99-102`).
6. `installer.Install`; error → `downgrade: schema reversed to v<N> but installing prior binary failed: <err>\n\nrecover by either:\n  - installing <tag> manually (download from <url>), then re-running lit; or\n  - restoring the pre-downgrade snapshot via \`lit snapshots list\` + \`lit snapshots restore <name>\`` (`downgrade.go:104-112`).
7. Success stdout: `downgraded to <tag> (schema v<N>) installed at <binPath>` newline `re-run \`lit version\` to confirm.` (`downgrade.go:119-122`). No re-exec (`downgrade.go:114-118`).

There is deliberately no `--dry-run`, `--force`, or `--skip-snapshot` (`downgrade.go:36-37`; upgrade the same, `upgrade.go:74-75`).

---

## 7. `lit backup` (JSON data-export family)

Family usage `usage: lit backup <create|list|restore> ...` (`backup.go:22`). Access modes per row (`backup.go:23-30`): `create` → read, `list` → read, `restore` → write.

### 7.1 `lit backup create`

`runBackupCreate` — `backup.go:32-51`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--keep` | `20` | "Snapshots to keep after rotation" | `backup.go:34` |

`ap.Store.Export(ctx)` → `backup.Create(StorageDir, export)` → `backup.Prune(StorageDir, keep)` → stdout `<name> <path>` (`backup.go:38-50`).

### 7.2 `lit backup list`

`runBackupList` — `backup.go:53-68`. No flags. One line per snapshot: `<name> <size> <path>` (`backup.go:62-66`).

### 7.3 `lit backup restore`

`runBackupRestore` — `backup.go:103-120`. Canonical usage constant (`backup.go:74`): `usage: lit backup restore (--latest | --path <export.json>) [--force]`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--path` | `""` | "Path to an export JSON (backup snapshot or sync file)" | `backup.go:105` |
| `--latest` | `false` | "Restore latest backup snapshot" | `backup.go:106` |
| `--force` | `false` | "Force restore over unsynced state" | `backup.go:107` |

`resolveRestorePath` (`backup.go:82-101`):
- `--latest` with a non-empty `--path` → `UsageError{restoreUsage + " — --latest and --path are mutually exclusive"}` (exit 2).
- `--latest` with no snapshots → `errors.New("no backups available")` (exit 1).
- Neither source → `UsageError{restoreUsage}` (exit 2).

`restoreFromExportPath` (`backup.go:126-193`):
1. Requires BOTH the `storage.Sync` and `storage.Import` capabilities up front, before any read/write (`backup.go:133-140`).
2. `syncfile.Read(path)`, `ap.Store.Export(ctx)`, `syncer.GetSyncState(ctx)`.
3. **Unsynced-state refusal** (`backup.go:153-167`): when `state.ContentHash != ""` and `--force` is absent, hash `<StorageDir>/last-sync-base.json` (`syncBasePath`, `backup.go:122-124`); if that base hash is non-empty and differs from the SHA-256 of the current export (`hashExport`, `backup.go:195-205` — canonical `MarshalIndent` + trailing newline, lowercase hex), return `MergeConflictError{"restore conflict: local workspace has unsynced changes since last sync base"}` → **exit 5**.
4. Always takes a pre-restore backup: `backup.Create(StorageDir, localExport)` then `backup.Prune(StorageDir, 20)` — the 20 is hard-coded here (`backup.go:168-172`).
5. `importer.ReplaceFromExport(ctx, targetExport)`.
6. Re-exports and writes `last-sync-base.json` atomically (`backup.go:177-184`).
7. `syncer.RecordSyncState(ctx, {Path: restorePath, ContentHash: HashFile(restorePath)})` (`backup.go:185-192`).
8. stdout: `restored <path>` (`backup.go:118`).

`bulk import` is retired in favor of this path: guidance string `use \`lit backup restore --path <export.json>\` — it owns the same export-restore mechanism \`bulk import\` duplicated` (`register.go:441`).

---

## 8. `lit snapshots` (Dolt filesystem-level database snapshots)

Family usage `usage: lit snapshots <new|list|restore> ...` (`snapshots.go:19`); rows `new`, `list`, `restore` (`snapshots.go:20-26`). Workspace-mode (`register.go:383-384`).
Snapshot directory `<StorageDir>/snapshots` (`snapshots.go:32-34`).

### 8.1 `lit snapshots new`

`runSnapshotsNew` — `snapshots.go:76-124`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--label` | `""` | "Optional human-readable label appended to the snapshot name" | `snapshots.go:78` |

- Any positional → `UsageError{"usage: lit snapshots new [--label <text>]"}` (exit 2); rationale: `snapshots new nightly` is a natural typo for `--label nightly` (`snapshots.go:82-89`).
- `config.Load(pathspec.New(ws.RootDir))` for the retention budget (`snapshots.go:90-93`); default `snapshot.retention_budget = 5` (`internal/config/config.go:224`), validated `> 0` at load (`config.go:245-247`).
- `takeUserSnapshot` (`snapshots.go:142-189`) takes three holds in order — workspace shared (`store.LockWorkspaceShared`), Dolt journal exclusive (`store.LockDoltJournalExclusive`), commit lock (`withCommitLock`) — releasing LIFO; every release failure is joined into the returned error (`snapshots.go:143-188`).
- Between the workspace hold and the journal hold it checks `store.PendingAdopt(ws.DatabasePath)`; a pending-adopt marker refuses the snapshot (`snapshots.go:155-163`).
- **The record prints the moment it exists** — before the prune and even beside a later failure: `<name> <path>` to stdout; a print error is joined to `err` (`snapshots.go:96-105`).
- Retention prune runs AFTER the workspace hold is released, under the commit lock only, and only over user snapshots: `dbsnapshot.PruneMatching(snapshotsDir, cfg.Snapshot.RetentionBudget, isUserSnapshotName)` (`snapshots.go:109-123`). `isUserSnapshotName` excludes migration, downgrade, and reconcile snapshot names (`snapshots.go:46-50`).

### 8.2 `lit snapshots list`

`runSnapshotsList` — `snapshots.go:191-206`. No flags. One line per snapshot: `<name> <created RFC3339 "2006-01-02T15:04:05Z"> <path>` (`snapshots.go:200-205`).

### 8.3 `lit snapshots restore <name>`

`runSnapshotsRestore` — `snapshots.go:208-271`. Positional name split off before flag parsing via `splitArgs(args, 1)` (`snapshots.go:209`).
- Not exactly one positional, or leftover args → `UsageError{"usage: lit snapshots restore <name>"}` (`snapshots.go:214-216`); an all-whitespace name gets the same error (`snapshots.go:217-220`).
- Holds `store.LockWorkspaceExclusive` for the whole restore; a release failure is joined into the return via `errors.Join` (`snapshots.go:225-239`).
- `dbsnapshot.Restore(DatabasePath, snapshotsDir, name)` runs under `withCommitLock` (`snapshots.go:242-251`).
- If the restore failed but a directory was rotated aside, the error is wrapped: `the pre-restore database directory was moved aside to <rotated> and holds the workspace's data: <err>` (`snapshots.go:252-254`).
- On success prints `restored <name>` or, when a rotation happened, `restored <name> rotated_to=<path>`; a print error is joined (`snapshots.go:261-269`).

`withCommitLock` (`snapshots.go:61-74`): `store.LockCommitPath(ctx, store.CommitLockPath(DatabasePath))`; release settled by `store.SettleCommitLockRelease` (joined beside a failure, demoted to stderr after a durable success).

---

## 9. `lit lifeboat` (below-the-gate recovery)

Family usage `usage: lit lifeboat <dump|recover> ...` (`lifeboat.go:16`, `:22-28`). Workspace-mode.

### 9.1 `lit lifeboat dump`

`runLifeboatDump` — `lifeboat.go:158-171`. No flags. Any positional → `UsageError{"usage: lit lifeboat dump"}` (`lifeboat.go:163-165`). `store.DumpRaw(ctx, DatabasePath, WorkspaceID)` then `writeJSON(stdout, dump)` — **JSON only**, no text rendering (`lifeboat.go:154-157`).

### 9.2 `lit lifeboat recover`

`runLifeboatRecover` — `lifeboat.go:75-119`.

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--mapping` | `""` | "Path to an operator-authored ShapeMapping JSON; default uses the built-in deterministic mapper" | `lifeboat.go:77` |

- Any positional → `UsageError{"usage: lit lifeboat recover [--mapping <file>]"}` (`lifeboat.go:81-83`).
- `recoverMapper` (`lifeboat.go:48-61`): empty path → `store.DeterministicMapper`; else `os.ReadFile` (error `read mapping <path>: %w`) and `json.Unmarshal` into `store.ShapeMapping` (error `parse mapping <path>: %w`), wrapped as a constant mapper.
- `store.HealWorkspace(ctx, DatabasePath)` runs FIRST, unconditionally — heals a promotion that crashed between renames (`lifeboat.go:88-95`).
- `store.DumpRaw` → `store.Recover(ctx, DatabasePath, dump, mapper, recoverAttempts)` with `recoverAttempts = 1` (`lifeboat.go:38`, rationale `:31-37`).
- Three outcomes (`lifeboat.go:104-118`):
  - `store.Reconciled` → `promoteReconciled`.
  - `store.RequiresDrop` → the candidate is discarded and the command fails: `recovery needs a human decision: the mapping discards <N> source column(s) with no recorded justification:\n<  - column…>\nnothing was changed; supply a mapping that maps or intentionally drops these before recovering`, joined with any discard error (exit 1).
  - `store.Unconverged` → `recovery did not converge after <N> attempt(s); nothing was changed:\n<residual>` (exit 1).
  - Any other type → `unknown recovery outcome %T`.
- `promoteReconciled` (`lifeboat.go:121-144`): `store.PromoteCandidate`; a deferred `Candidate.Discard()` failure is joined as `discard candidate scratch after promotion: %w`. stdout: `recovered: rebuilt workspace promoted to <canonical> (<previous contents preserved at <backup> | no previous contents to preserve>)`.
- `formatDrops` renders `  - <column>` per line (`lifeboat.go:146-152`).

---

## 10. `lit stores`

Handler `runStores` — `stores.go:24-64`. Not app-mode; called raw with ctx and stdout (`register.go:366-367`).

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--counts` | `false` | "Report each discovered store's ready / in-flight / blocked counts (cross-project rollup) instead of listing storage paths" | `stores.go:30` |

Positional args are the roots; with none, the cwd is used (`os.Getwd`, error `get cwd: %w`) (`stores.go:34-44`).

**Default (path list)**: `workspace.Discover(roots)`; error wrapped `discover stores: %w` (`stores.go:52-57`). One canonical `StorageDir` per line, streaming; a write failure stops with non-zero exit after the lines already emitted (`stores.go:17-23`, `:58-62`). No stores found ⇒ empty output, exit 0.

**`--counts`**: `gatherCrossProjectRollup` (`stores.go:95-105`) discovers, then `rollupLocation` per store (`stores.go:116-151`):
- Label = `cfg.IssuePrefix` from `workspace.ReadConfig(loc.ConfigPath)` when non-empty, else the `StorageDir` (`stores.go:117-120`).
- `app.OpenLocationForRead`; failure → `row.Err`, no counts (`stores.go:121-125`).
- Read-only close failure → `row.CloseErr`, set only when `row.Err` is nil (`stores.go:130-134`).
- `classifyWorkable(ctx, st, nil, workableFilter{})` — a **nil required-fields** argument opts out of the per-repo `required_fields` policy only; blockers, the lane gate, and needs-design still apply (`stores.go:135-141`). Counts come from `partitionWorkable` (`stores.go:142-150`).

`printCrossProjectRollup` (`stores.go:159-221`):
- Zero rows → `(no stores discovered)` and return (`stores.go:164-167`).
- A tabwriter table (`NewWriter(w, 2, 2, 2, ' ', 0)`) headed `PROJECT  READY  IN-FLIGHT  BLOCKED` with one row per readable store and a `TOTAL` row summing exactly the shown rows; the whole table is omitted when every store errored (`stores.go:183-203`).
- Then `! <storageDir>: <err>` per unreadable store (`stores.go:205-209`).
- Then `~ <storageDir>: close warning: <err>` per readable store with a close warning (`stores.go:213-219`).

---

## 11. `lit hooks`

Family usage `usage: lit hooks install` (`hooks.go:32`); the only row is `install` (`hooks.go:34-37`).

`runHooksInstall` — `hooks.go:40-56`. No flags. Any positional → `UsageError{"usage: lit hooks install"}` (`hooks.go:45-47`). Prints `installed <hookPath>` (`hooks.go:54`) regardless of whether anything changed.

`installHooks(ws)` — `hooks.go:58-125`, shared by `lit init`, `lit hooks install`, and `lit quickstart --refresh`:
1. `MkdirAll(<GitCommonDir>/hooks, 0o755)`; failure → `create hooks dir: %w` (`hooks.go:59-63`).
2. Target path `<GitCommonDir>/hooks/pre-push` (`hooks.go:60`).
3. Section from `templates.Load(templates.PrePushHookTemplateName, ws.RootDir)` (`hooks.go:65-68`, `:127-129`); load failure → `load pre-push hook template: %w`.
4. Read the existing hook; a non-ENOENT read error → `read existing pre-push hook: %w` (`hooks.go:69-72`).
5. **Absent** → write `#!/usr/bin/env bash\n` + section at mode `0o755`; result `{Changed: true, Managed: true}` (`hooks.go:74-81`).
6. **Present** → mode is preserved from the existing file's permission bits, forced to `0o755` if no execute bit is set (`hooks.go:83-88`).
7. **Compatibility gate** (`hooks.go:92-114`): the first line must start with `#!` AND contain `bash`. Otherwise the hook is left untouched and the result is `{Changed: false, Managed: false, Reason: "incompatible"}` — no error, exit 0.
8. Legacy marker migration: `# --- BEGIN LINKS INTEGRATION ---` / `# --- END LINKS INTEGRATION ---` → `# --- BEGIN LIT INTEGRATION ---` / `# --- END LIT INTEGRATION ---` (`hooks.go:18-22`, `:115`). `migrateMarkers` (`managed_sections.go:10-17`) fires when EITHER legacy marker is present, so partial-state files converge.
9. `upsertManagedSection` (`managed_sections.go:21-51`): replaces the region between the markers (from the start of the begin-marker's line through the end of the end-marker's line) when both markers exist and `start < end`; otherwise appends the section (with a separating blank line) to the end, or replaces the whole content when it is blank. Returns `changed = (updated != content)`.
10. No change → `{Changed: false, Managed: true}`; else write at the preserved mode and return `{Changed: true, Managed: true}` (`hooks.go:116-124`).

---

## 12. `lit quickstart`

Handler `runQuickstart` — `cli.go:1728-1795`.
Usage string, derived from the topic table: `usage: lit quickstart [work|new|update|done|doctor] [--refresh] [--eject[=LIST]] [--force]` (`quickstart_topics.go:55`, tokens from `quickstart_topics.go:16-25`).

| Flag | Default | Effect | Line |
|---|---|---|---|
| `--refresh` | `false` | "Refresh managed repo assets and report quickstart override status (never overwrites overrides)" | `cli.go:1730` |
| `--eject[=LIST]` | absent `""` / present `"all"` | "Eject embedded default(s) to the global override path (comma-separated short names; empty = all)" | `cli.go:1731` |
| `--force` | `false` | "With --eject, overwrite existing override files" | `cli.go:1732` |

### 12.1 Validation (all `UsageError`, exit 2)

- More than one positional → `quickstartUsage` (`cli.go:1737-1739`).
- `--eject` (empty value) is normalized to `all` (`cli.go:1741-1744`).
- `--refresh` together with `--eject` → `usage: --refresh and --eject are mutually exclusive` (`cli.go:1745-1747`).
- `--force` without `--eject` → `usage: --force is only valid with --eject` (`cli.go:1748-1750`).
- Exactly one positional (a topic) with ANY of `--refresh`/`--eject`/`--force` → `usage: lit quickstart <topic> takes no flags` (`cli.go:1754-1755`).
- An unknown topic → `usage: unknown quickstart topic "<t>" (must be one of: work, new, update, done, doctor)` (`cli.go:1756-1759`).

### 12.2 Modes

**Topic mode** (`cli.go:1752-1767`): `renderQuickstartTopic(ws.RootDir, template)` → `templates.Load` (project > global > embedded) with `strings.TrimSpace` (`quickstart_refresh.go:206-212`); printed with a trailing newline. Topic output **never** carries the soil section (`quickstart_refresh.go:203-205`).

**Eject mode** (`cli.go:1769-1774`) → `ejectTemplates(selection, force)` then `writeEjectReport`.

**Bare / `--refresh` mode** (`cli.go:1776-1794`): renders `renderQuickstartGuidance(ws.RootDir)` = the `quickstart.md` template trimmed, plus `soilSection` appended when `quickstart.soil_mode = true` in config (`quickstart_refresh.go:187-201`; the section text is at `quickstart_refresh.go:183-185`; config default `false` at `internal/config/config.go:223`). With `--refresh`, the workspace is re-resolved via `workspace.Resolve(".")` and the refresh summary plus a blank line is prepended (`cli.go:1785-1791`).

### 12.3 `--refresh` behavior (`quickstart_refresh.go`)

`refreshQuickstartManagedAssets` (`quickstart_refresh.go:29-59`) runs the same writers `lit init` uses:
1. `installHooks(ws)` — rewrites the managed pre-push section (error aborts).
2. `ensureLinksAgentFiles(ws.RootDir)` — rewrites the AGENTS.md/CLAUDE.md managed sections (error aborts).
3. `refreshQuickstartTemplates(ws.RootDir)` — **inspection only, never writes** (`quickstart_refresh.go:61-81`), over `quickstartGuidanceTemplateNames()` = `quickstart.md` then each topic template in router order (`quickstart_topics.go:46-53`).

Per-template status (`refreshQuickstartTemplate`, `quickstart_refresh.go:83-114`):
- No override at either layer → `{status: "absent", managed: false}` (no path).
- Override content identical to the embedded default → `{status: "unchanged", managed: true, path}`.
- Override drifted → `{status: "skipped", managed: true, path, reason: "customized"}` — the file is left untouched.

Hook item (`quickstartHookRefreshItem`, `quickstart_refresh.go:167-178`): status from `managedAssetStatus(Changed, false)` = `unchanged`/`updated`; forced to `skipped` when `!Managed && Reason != ""` (the incompatible-hook case).
`managedAssetStatus(changed, created)` (`quickstart_refresh.go:116-126`): `created` wins over `changed` wins over `unchanged`.

JSON report shape (structs only; no JSON output path): `{agents, claude, hooks, quickstart[]}` with items `{name?, path, status, managed, reason?, source?}` (`quickstart_refresh.go:13-27`).

Human summary (`formatQuickstartRefreshSummary`, `quickstart_refresh.go:128-165`): labels `pre-push hook`, `AGENTS.md`, `CLAUDE.md`, then `<template-basename> template` per quickstart item; grouped into
`  Refreshed: …` (created/updated), `  Skipped: …`, `  Up to date: …`; when all three groups are empty the summary is `  nothing to refresh`. AGENTS/CLAUDE reasons compose with the source: `composeSourceReason` yields `"<reason>, via <source>"` or `"via <source>"`, and nothing when the status is `skipped` (`init.go:157-165`).

### 12.4 `--eject` behavior (`quickstart_eject.go`)

`resolveEjectSelection` (`quickstart_eject.go:90-113`): `""` → `eject: empty selection` (never reachable from the CLI, which normalizes to `all`); `"all"` → `templates.Names()`; otherwise a comma-separated list of short aliases de-duplicated in order, resolved via `templates.ResolveShortName` — an unknown alias errors with `usage: unknown template "<a>" (must be one of: agents, hook, quickstart, quickstart-done, quickstart-doctor, quickstart-new, quickstart-update, quickstart-work)` (`internal/templates/templates.go:65-72`, alias map `templates.go:44-53`).

`ejectTemplates` (`quickstart_eject.go:26-53`): plans EVERY target first; if any plan is `Skipped: "exists"`, the entire write phase is skipped (atomic abort). With `--force`, every target is (over)written; per-target write failures can leave partial state (`quickstart_eject.go:21-25`).
`planEject` (`quickstart_eject.go:55-69`): the target is `templates.GlobalPath(name)`; an absent global config directory → `eject <name>: no global config directory configured`; a stat error other than not-exist → `eject <name>: stat <path>: <err>`.
`writeEject` (`quickstart_eject.go:71-84`): `templates.EmbeddedDefault(name)` (error `eject <name>: read embedded default: %w`), `MkdirAll(dir, 0o755)` (error `eject <name>: create dir: %w`), `os.WriteFile(path, content, 0o644)` (error `eject <name>: write <path>: %w`).

`writeEjectReport` (`quickstart_eject.go:115-146`), per result line:
- `exists  <name> (<path>; pass --force to overwrite)`
- `skipped <name> (not written; <N> conflict(s) aborted the operation)`
- `ejected <name> -> <path>`
Then, when conflicts exist: with `--force` → `MergeConflictError{"eject aborted: unexpected conflicts reported with --force"}`; without → `MergeConflictError{"conflict: <N> template(s) already exist; re-run with --force to overwrite"}`. Both exit **5**.

### 12.5 Breadcrumbs

`quickstartBreadcrumb(token)` returns `deeper guidance: lit quickstart <token>` and **panics** on a token not in the topic table (`quickstart_topics.go:62-67`). `emitBreadcrumb(w, token)` writes it as its own line after a command's success output (`quickstart_topics.go:72-75`).

---

## 13. `lit version`

`runVersion` — `version.go:17-68`. No flags beyond `--help`. Any positional → `UsageError{"usage: lit version"}` (`version.go:22-24`).
`version.Get()` error is returned (`version.go:26-29`).

Output lines:
1. `lit <version|"dev"> (commit <commit|"unknown">, built <date|"unknown">)` — `dev` substituted when `info.IsDev`; `commit`/`date` fall back to `unknown` when blank (`version.go:31-45`).
2. Only when `info.BuildAge(now)` reports `ok` (a real, past, parsed date): `built <coarse duration> ago` (`version.go:52-55`).
3. Only when the age is `>= version.StaleBuildThreshold`: `WARNING: binary is older than <threshold> — run \`just build\` (or \`just install\`) to pick up recent fixes` (`version.go:56-63`).
4. Always: `schema versions supported: <min>–<max>` (en dash) (`version.go:66`).

---

## 14. Build status note (`build_status.go`)

`buildStatusNote(info, now)` — `build_status.go:20-38`:
- Release build → `build: release <version>`.
- Dev build, no parsable date → `build: dev build (build date unknown)`.
- Dev build, age `>= version.StaleBuildThreshold` → `build: dev build, built <age> ago — STALE (at least <threshold> old; run \`just build\` to refresh)` — "at least", because the guard is `>=` (`build_status.go:29-31`).
- Dev build, fresh → `build: dev build, built <age> ago`.

`resolveBuildStatusNote(now)` — `build_status.go:48-54`: `version.Get()` failure yields `build: status unavailable (<err>)` rather than aborting the caller.

Consumers: `lit init` human output and its adopt progress line (`init.go:143`, `init_sync.go:130`), the init sync trace (`init_sync.go:351`), `lit doctor`'s second output line (`doctor.go:296`), every `SyncFailure.BuildNote` boundary (`sync_failure.go:139`, `sync.go:294`, `sync_receive.go:139`, `doctor.go:82`, `sync_reconcile_cmd.go:467`), and every sync trace record (`sync_trace.go:112`, `:143`, etc.).

---

## 15. Managed-section machinery (`agents_internal.go`, `managed_sections.go`)

Markers (`agents_internal.go:12-18`): current `<!-- BEGIN LIT INTEGRATION -->` / `<!-- END LIT INTEGRATION -->`; legacy `<!-- BEGIN LINKS INTEGRATION -->` / `<!-- END LINKS INTEGRATION -->`.

`ensureLinksAgentFiles(rootDir)` — `agents_internal.go:70-89`, the single writer for both files:
1. `templates.LoadWithSource(templates.AgentsSectionTemplateName, rootDir)` (project > global > embedded); failure → `load agent section template: %w`.
2. `writeManagedFile(rootDir, "AGENTS.md", headerPrefix: "# AGENTS\n\n", section, markers)`.
3. `writeManagedFile(rootDir, "CLAUDE.md", headerPrefix: "", section, markers)` — CLAUDE.md gets **no** header prefix on creation.
4. Both results carry the resolved `Source`.

`writeManagedFile` — `agents_internal.go:35-64`:
- File absent → write `headerPrefix + section` at `0o644`; result `{Created: true, Changed: true}`. A non-ENOENT read error → `read <filename>: %w`; a write error → `write <filename>: %w`.
- File present → `migrateMarkers` then `upsertManagedSection`; the change signal compares the final content against the ORIGINAL bytes, so a marker-only migration counts as changed (`agents_internal.go:49-59`). Unchanged → no write. Changed → `os.WriteFile(..., 0o644)`; result `{Created: false, Changed: true}`.
- Everything outside the markers is preserved (`agents_internal.go:31-34`, `:66-69`).

---

## 16. `internal/templates` — the embed mechanism and asset list

- `//go:embed defaults/*` into `defaultsFS embed.FS` (`templates.go:28-29`).
- Canonical names (`templates.go:16-25`): `agents-section.md`, `pre-push-hook.sh`, `quickstart.md`, `quickstart-work.md`, `quickstart-new.md`, `quickstart-update.md`, `quickstart-done.md`, `quickstart-doctor.md`. `Names()` returns this list in that order (`templates.go:31-40`, `:57-61`).
- Short aliases (`templates.go:44-53`): `quickstart`, `quickstart-work`, `quickstart-new`, `quickstart-update`, `quickstart-done`, `quickstart-doctor`, `agents`, `hook`. `sortedAliasNames()` sorts them for error messages (`templates.go:74-81`).
- Resolution precedence (`Load`/`LoadWithSource`, `templates.go:95-129`): **project** `<workspaceRoot>/.lit/templates/<name>` (`templates.go:190-192`) > **global** `<config.ConfigDir()>/templates/<name>` (`templates.go:138-140`, `:194-197`) > **embedded**. A layer contributes nothing when its file is absent or empty; a read error at a layer is wrapped `load project template <path>: %w` / `load global template <path>: %w`. Sources are `project` / `global` / `embedded` (`templates.go:86-90`). All three empty → `load template <name>: no non-empty source`.
- `EmbeddedDefault(name)` reads `defaults/<name>` raw (`templates.go:132-134`).
- `ActiveOverride(root, name)` returns the highest-priority EXISTING override (project then global) with its content, or an absent path when neither exists; non-not-exist errors propagate (`templates.go:154-174`).

### 16.1 Embedded assets (program output data)

| Asset | Written by | Destination | Role / structure |
|---|---|---|---|
| `agents-section.md` | `ensureLinksAgentFiles` (from `lit init`, `lit quickstart --refresh`) | the managed region of `<root>/AGENTS.md` and `<root>/CLAUDE.md` | 8 lines wrapped in `<!-- BEGIN/END LIT INTEGRATION -->`; a heading plus one paragraph telling the agent to run `lit quickstart` first |
| `pre-push-hook.sh` | `installHooks` (from `lit init`, `lit hooks install`, `lit quickstart --refresh`) | the managed region of `<git-common-dir>/hooks/pre-push` | 25-line bash fragment wrapped in `# --- BEGIN/END LIT INTEGRATION ---`; `set -u`, takes `$1` as the remote name (default `origin`), mktemps a trace-ref file, runs `lit sync push --remote "$remote"` with `LNKS_AUTOMATION_TRIGGER=git-pre-push`, `LNKS_AUTOMATION_REASON="git push triggered the managed pre-push sync"`, and `LNKS_AUTOMATION_TRACE_REF_FILE`; on failure prints a `[links] warning:` line to stderr carrying the trace path (or `unavailable`) inside an `<agent-instructions>` element; has a no-mktemp fallback branch; always `exit 0` so a sync failure never blocks the git push |
| `quickstart.md` | `lit quickstart` (bare), bare `lit`, `lit quickstart --refresh` (rendered, never written to the repo) | stdout | 18-line router: an `<agent-instructions>` framing note, a paragraph on ticket provenance, a bulleted list of the five topic subcommands, and a "Fastpath" of `lit next` / `lit start <id>` / `lit workflows` |
| `quickstart-work.md` | `lit quickstart work` | stdout | 11 lines on finding/starting work: `lit ls --limit --search`, `lit next`, `lit backlog`, `lit show`, claims-first selection, `lit start` and `--take` |
| `quickstart-new.md` | `lit quickstart new` | stdout | 15 lines on `lit new` flags (`--title/--topic/--type/--parent/--top`), `<agent-instructions>` notes on `--description`, `--topic`, and default bottom-of-frame ranking; `lit followup`; `lit import --path` for batches |
| `quickstart-update.md` | `lit quickstart update` | stdout | 13 lines: `lit update`, `lit import`, `lit rank`, `lit label add/rm` (`needs-design`, `focus`), `lit parent set`, `lit dep add` (`blocks`, `related-to`), `lit comment add` |
| `quickstart-done.md` | `lit quickstart done` | stdout | 9 lines: `lit done`, `lit close --resolution <duplicate\|superseded\|obsolete\|wontfix>`, `lit followup`, `lit workflows edit done`, and a commit reminder |
| `quickstart-doctor.md` | `lit quickstart doctor` | stdout | 5 lines: `lit doctor [--fix]` plus an `<agent-instructions>` note to self-resolve first |

All eight are also the payload of `lit quickstart --eject`, written to `<config.ConfigDir()>/templates/<name>` at mode `0o644` (`quickstart_eject.go:79`).

---

## 17. Cross-cutting refusal summary (operations scope)

| Condition | Surface | Result | Line |
|---|---|---|---|
| Outside a git repo | any workspace/app command | `OutsideWorkspaceError{"links requires running inside a git repository/worktree"}`, exit 1 | `cli.go:110-112`, `cli.go:156-160` |
| Missing/unknown family subcommand | `sync`, `hooks`, `backup`, `snapshots`, `lifeboat`, `sync remote`, `sync reconcile` | the family usage string as a plain error, exit 1 | `register.go:112-123` |
| Unknown flag | any command | `UsageError`, exit 2 | `cli.go:296-298` |
| `--output` / `--continue` | any command | `UnsupportedError`, exit 3 | `cli.go:287-295` |
| Stray positional | `init`, `version`, `hooks install`, `snapshots new`, `lifeboat dump`, `lifeboat recover`, `upgrade`, `downgrade`, `sync reconcile`/`resolve`/`abort`/`combine` | `UsageError`, exit 2 | `init.go:34`, `version.go:22`, `hooks.go:45`, `snapshots.go:87`, `lifeboat.go:163`, `lifeboat.go:81`, `upgrade.go:200`, `downgrade.go:81`, `sync_reconcile_cmd.go:82-87` |
| Adopt could not confirm workspace state | `init` | refuse to create a store, exit 1 | `init.go:60-74` |
| Remote-schema-ahead | `sync push/pull/reconcile*`, inline receive, mirror | `SyncFailureError` block, exit 5 (mirror: stderr only) | `sync.go:246`, `sync.go:389`, `sync_reconcile_cmd.go:120`, `sync_receive.go:147`, `sync_bg.go:329` |
| Held prose conflict | `sync pull` | `SyncFailureError`, exit 5 | `sync.go:260-267` |
| Held prose conflict | `sync reconcile`/`resolve`/`combine` | guidance printed + `MergeConflictError`, exit 5 | `sync_reconcile_cmd.go:474-509` |
| Unrelated histories | `sync pull`, `sync reconcile*` | `SyncFailureError`, exit 5 | `sync.go:302-304`, `sync_reconcile_cmd.go:455-473` |
| Take without owner approval | `sync reconcile take` | `ownerApprovalRefusalError` block, exit 5 | `sync_reconcile_cmd.go:230-253` |
| Persistent divergence (≥24h or >10 commits) | `doctor` | `SyncFailureError`, exit 5 | `doctor.go:92-97`, `doctor.go:323` |
| Store corruption | `doctor` | `CorruptionError`, exit 7 | `doctor.go:312-315` |
| Unsynced local changes | `backup restore` without `--force` | `MergeConflictError`, exit 5 | `backup.go:163-166` |
| `--latest` + `--path` together | `backup restore` | `UsageError`, exit 2 | `backup.go:86-87` |
| Existing global override without `--force` | `quickstart --eject` | `MergeConflictError`, exit 5, nothing written | `quickstart_eject.go:139-144` |
| Non-bash existing pre-push hook | `hooks install`, `init`, `quickstart --refresh` | left untouched, reported `incompatible`, exit 0 | `hooks.go:107-114` |
| Pending-adopt marker | `snapshots new` | refused via `store.PendingAdopt` | `snapshots.go:161-163` |
| Backward-move upgrade target | `upgrade` | `*UpgradeTargetBehindError`, exit 1, nothing installed | `upgrade.go:223-230` |
| Empty/invalid `--to` | `upgrade`, `downgrade` | `ValidationError`, exit 3 | `downgrade.go:139-155` |
| Unknown `--fix` name | `doctor` | `unknown fix %q; available: integrity, rank`, exit 1 | `doctor.go:274-276` |
| Unknown quickstart topic | `quickstart` | `UsageError`, exit 2 | `cli.go:1756-1759` |
| Unknown template alias | `quickstart --eject` | error listing valid aliases, exit 1 | `templates.go:68-70` |
