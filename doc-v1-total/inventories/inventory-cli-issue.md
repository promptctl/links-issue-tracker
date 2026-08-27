# `lit` CLI — Issue-Facing Command Behavioral Inventory

Derived exclusively from Go source under `/Users/bmf/code/links-issue-tracker`.
Every claim carries a `file:line` citation. Paths are relative to the repo root.

Out of scope (covered elsewhere): `init`, `sync*`, `doctor`, `upgrade`, `downgrade`,
`backup`, `snapshots`, `stores`, `lifeboat`, `hooks`, `quickstart*`, managed sections,
`version`, build status, `claims*` internals, `workflows*`, workflow events, owner
notify, agents-internal, automation trace, detach. Where an in-scope command *calls
into* one of those, the call and its observable effect are recorded here.

---

## PART 1 — SHARED PLUMBING

### 1.1 Entry point and global argument handling

- `Run(ctx, stdout, stderr, args)` is the package entry (`internal/cli/cli.go:35`).
  It first runs `parseGlobalArgs` (`cli.go:36`), then builds the cobra root
  (`cli.go:40`), sets args/out/err, sets `SilenceErrors = true` and
  `SilenceUsage = true` (`cli.go:44-45`), and executes.
- `pflag.ErrHelp` and the internal `errHelpHandled` sentinel are swallowed and
  converted to a nil return, i.e. exit 0 (`cli.go:47-49`; sentinel defined at
  `cli.go:28`).
- `parseGlobalArgs` (`cli.go:165-186`) scans leading args: a literal `--` is
  consumed and scanning stops (`cli.go:171-173`); a leading `--output` or
  `--output=<x>` returns `unsupportedOutputFlagError()` (`cli.go:174-179`); any
  other token stops the scan. Effect: the removed `--output` flag is rejected in
  *global* position before any command runs.
- `unsupportedOutputFlagError()` returns
  `UnsupportedError{Message: "--output is no longer supported; omit it for text output", Feature: "--output"}`
  (`cli.go:310-312`).

### 1.2 Root command

- `Use: "lit"`, `Long: "Agent-native issue tracker"`, `Args: cobra.ArbitraryArgs`
  (`cli.go:54-57`).
- Bare `lit <unknown-token>` → `UnknownCommandError{Command: args[0]}`
  (`cli.go:59-61`).
- Bare `lit` with no args: resolves the workspace from cwd (`cli.go:64`). If the
  error is `workspace.ErrNotGitRepo` it prints cobra's `Help()` (`cli.go:67-69`);
  any other error is returned (`cli.go:70-72`); otherwise it renders and prints
  the quickstart guidance — byte-identical to `lit quickstart` (`cli.go:73-78`).
- Cobra's default `completion` command is disabled (`cli.go:81`); cobra's built-in
  `help` command remains.
- Root flag errors are wrapped as `UsageError` so an unknown global flag exits
  `ExitUsage` (`cli.go:85-87`).

### 1.3 Command registry

- The whole command tree is a table: `commandSpecs(ctx, stdout, stderr) []CommandSpec`
  (`register.go:254-394`). Each `CommandSpec` carries `Name`, `Summary`, `Long`,
  `GroupID`, `Run`, `Subcommands`, `Hidden` (`register.go:18-38`).
- `applyRegistry` adds every group then every command (`register.go:398-405`).
- `buildPassthroughCommand` (`register.go:409-426`) creates each cobra command with
  `DisableFlagParsing: true` and `Args: cobra.ArbitraryArgs`. **Consequence:** cobra
  does not parse any per-command flags; each handler parses its own argv slice.
  `Long` defaults to `agentCommandHelp` = `"Agent-facing operational command."`
  when the spec sets none (`register.go:410-413`, constant at `cli.go:32`).
  `humanBootstrapHelp` = `"Human bootstrap command. Run once per repository/worktree setup before autonomous agent operations."` (`cli.go:31`), used only by `init`.
- Help groups, in order (`register.go:61-76`):
  `bootstrap` "Human Bootstrap", `operations` "Agent Operations",
  `structure` "Dependencies & Structure", `data` "Sync & Data",
  `maintenance` "Setup & Maintenance", `retention` "Issue Retention",
  `guidance` "Guidance & Tooling".

### 1.4 Wrappers: how a handler gets a store

- `r.appCmd(access, fn)` → `runWithApp` with a fixed access mode
  (`register.go:187-189`); `r.appCmdDynamic` computes the mode from argv
  (`register.go:191-197`).
- `r.wsCmd(fn)` → `runWithWorkspace` (workspace metadata only, no store)
  (`register.go:238-244`).
- `r.familyCmd(family)` resolves `args[0]` against the family table *before*
  opening anything; a row with `skipApp: true` runs with a nil app
  (`register.go:203-219`).
- `r.wsFamilyCmd(family)` same, but workspace-mode (`register.go:226-236`).
- `r.transitionCmd(spec)` → `runTransition` under `app.AccessWrite`
  (`register.go:246-250`).
- `runWithApp` (`cli.go:102-147`):
  - `os.Getwd()` failure → `fmt.Errorf("get cwd: %w", err)` (`cli.go:104-106`).
  - `app.Open` failing with `workspace.ErrNotGitRepo` →
    `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}`
    (`cli.go:110-112`).
  - The handler runs with a deferred `ap.Close()` (`cli.go:120-122`).
  - **After a successful write command** (`accessMode == app.AccessWrite`), the
    mutation sync-staleness warning is printed at this one seam
    (`cli.go:135-137`, `printMutationSyncStalenessWarning` at
    `sync_staleness.go:217`).
  - Then `maybeAutoSyncAfterCommand(ctx, accessMode, ws)` runs (`cli.go:145`).
  - Both only run when the command returned nil (`cli.go:123-125`).
- `runWithWorkspace` / `resolveWorkspaceFromWD` (`cli.go:94-163`): same
  `OutsideWorkspaceError` translation (`cli.go:156-159`).

### 1.5 Family dispatch (`commandFamily[P]`)

- `resolve(args)` (`register.go:112-123`): with **zero args**, or an args[0] that
  matches no row, it returns `errors.New(f.usage)` — a plain error, **not** a
  `UsageError`, so the exit code is `ExitGeneric` (1), not 2. Matching is exact
  string equality; no trimming.
- `visibleSubcommands()` drops `hidden` rows for the completion projection
  (`register.go:129-138`).
- `nestUnder(subs, name, children)` panics if `name` is absent
  (`register.go:146-154`).

### 1.6 Per-command flag parsing (`cobraFlagSet`)

- `newCobraFlagSet(use)` builds a throwaway cobra command with a default `--help`
  flag installed and all output discarded (`cli.go:192-203`).
- Registration helpers: `String`, `Bool`, `Int` (`cli.go:215-225`), `StringArray`
  (repeatable, never comma-split — `cli.go:230-232`), `StringOptional`
  (`--flag` with no value takes `defaultIfPresent`, absent takes
  `defaultIfAbsent` — `cli.go:237-241`), `Hide(name)` marks a flag hidden but
  functional (`cli.go:261-263`).
- `parseFlagSet(fs, args, stdout)` (`cli.go:274-308`) is the single parse boundary:
  - On `pflag.ErrHelp` it prints `"Usage of <use>:\n"` followed by
    `PrintDefaults()` **to stdout** and returns `errHelpHandled` → exit 0
    (`cli.go:277-283`, printer at `cli.go:265-272`).
  - `flag provided but not defined: -output|--output` →
    `UnsupportedError{"--output is no longer supported; omit it for text output", "--output"}`
    (`cli.go:286-289`).
  - `flag provided but not defined: -continue|--continue` or
    `unknown flag: --continue` → `UnsupportedError{Message: "--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag", Feature: "--continue"}`
    (`cli.go:290-294`).
  - Any other `unknown flag:` / `flag provided but not defined:` →
    `UsageError{Message: msg}` → exit 2 (`cli.go:295-297`).
  - A parsed-and-changed `--help` flag also prints help and returns
    `errHelpHandled` (`cli.go:300-306`).

### 1.7 Positional/flag splitting (`splitArgs`)

`splitArgs(args, positionalCount)` (`cli.go:1958-1978`):
- Any token starting with `-` goes to the flag slice; if it contains no `=` and
  the *next* token does not start with `-`, that next token is consumed as its
  value (`cli.go:1963-1969`).
- The first `positionalCount` non-flag tokens become positionals; any extra
  non-flag tokens are appended to the **flag** slice (`cli.go:1971-1975`), where
  they surface as `fs.NArg() > 0` if the command checks it.
- Known consequence: a boolean flag written as `--flag value` swallows `value`.

### 1.8 Exit-code taxonomy

Constants (`exit.go:10-18`):

| Name | Value |
|---|---|
| `ExitOK` | 0 |
| `ExitGeneric` | 1 |
| `ExitUsage` | 2 |
| `ExitValidation` | 3 |
| `ExitNotFound` | 4 |
| `ExitConflict` | 5 |
| `ExitCorruption` | 7 |

(6 is unused.)

`ExitCode(err)` dispatches by `errors.As`, in this order (`exit.go:23-95`):
1. `storage.NotFoundError` → 4 (`exit.go:27-30`)
2. `MergeConflictError` → 5 (`exit.go:31-34`)
3. `SyncFailureError` → 5 (`exit.go:39-42`)
4. `ownerApprovalRefusalError` → 5 (`exit.go:46-49`)
5. `CorruptionError` → 7 (`exit.go:50-53`)
6. `UsageError` → 2 (`exit.go:54-57`)
7. `UnknownCommandError` → 3 (`exit.go:58-61`)
8. `RetiredCommandError` → 3 (`exit.go:64-67`)
9. `ValidationError` → 3 (`exit.go:68-71`)
10. `storage.ValidationError` → 3 (`exit.go:72-75`)
11. `UnsupportedError` → 3 (`exit.go:76-79`)
12. `OutsideWorkspaceError` → 1 (`exit.go:80-83`)
13. `BulkFailureError` → 1 (`exit.go:84-90`)
14. `errors.Is(err, store.ErrTransientGCContention)` → 1 (`exit.go:91-93`)
15. anything else → 1 (`exit.go:94`)

Error types defined in `cli.go`: `MergeConflictError` (`cli.go:1890-1896`),
`CorruptionError` (`cli.go:1898-1902`), `UsageError` (`cli.go:1906-1910`),
`UnknownCommandError` — message `unknown command "<x>"` (`cli.go:1913-1917`),
`ValidationError` (`cli.go:1920-1924`), `UnsupportedError` with a `Feature` field
(`cli.go:1928-1933`), `RetiredCommandError` — message
`the "<cmd>" command has been retired; <replacement>` (`cli.go:1942-1949`),
`OutsideWorkspaceError` (`cli.go:1952-1956`). `BulkFailureError` in
`bulk.go:48-58`.

### 1.9 Error output convention

`WriteCommandError(stderr, err)` (`error_output.go:17-24`) — called from
`cmd/lit/main.go:20` as the process exit path:
- Line 1: `error (code=%d): %v\n` (exit code + `err.Error()`).
- Line 2 (only when non-empty): `remediation: %s\n`.

`commandErrorReason(err)` maps type → reason string (`error_output.go:29-90`):
`entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`,
`corruption_detected`, `unknown_command`, `retired_command`, `usage_error`,
`unsupported_output_flag` (only when `UnsupportedError.Feature == "--output"`;
other `UnsupportedError`s fall to `command_failed`, `error_output.go:62-68`),
`outside_git_workspace`, `bulk_partial_failure`, `workspace_write_blocked`,
`transient_gc_contention`, default `command_failed`.

`commandErrorRemediation(reason)` (`error_output.go:92-133`), verbatim strings:
- `unknown_command`: "Run `lit --help` (or `lit help <command>`) to select a supported command path."
- `retired_command`: "" (empty — message is self-contained)
- `usage_error`: "Run the command with `--help` and retry with valid arguments."
- `unsupported_output_flag`: "Remove `--output`. lit emits text output; there is no output-format flag."
- `entity_not_found`: "Verify the target ID exists with `lit ls` or `lit show <id>`."
- `merge_conflict`: "Sync and retry after resolving conflicts."
- `sync_divergence`: ""
- `owner_approval_required`: ""
- `corruption_detected`: "Run `lit doctor --fix integrity` and retry. \<agent-instructions>This command is idempotent and safe to run without confirmation.\</agent-instructions>"
- `transient_gc_contention`: "Retry once. If the error persists, run `lit doctor --fix`. \<agent-instructions>…\</agent-instructions>"
- `workspace_write_blocked`: "Wait a moment and retry — a normal command releases the store in well under a second. If it persists, a lit process is stuck: find it with `ps aux | grep '[l]it'` and terminate it, then retry; if none is running the hold is stale, so run `lit doctor --fix`. \<agent-instructions>…\</agent-instructions>"
- `outside_git_workspace`: "Run the command inside a git repository/worktree with links initialized."
- `bulk_partial_failure`: "Some items failed; see the per-item errors above. Re-run the command for only the failed IDs after addressing each error."
- default: "Retry the command. If it still fails, run `lit doctor` for diagnostics."

### 1.10 Progress channel

`progressf(operation, format, args...)` writes `lit: <operation>: <text>\n` to
`progressOut`, which is `os.Stderr` (`progress.go:15`, `progress.go:25-27`).
Stdout stays the result channel.

### 1.11 Identity resolution (assignee / actor)

- `resolveIdentity(explicit)` (`cli.go:1172-1177`): if env
  `CLAUDE_CODE_SESSION_ID` is non-empty (after trim), the identity is
  `"claude_" + sessionID`, **overriding any explicit value**; otherwise the
  trimmed explicit value.
- `registerActor(fs)` (`cli.go:1202-1206`) declares a **hidden** `--by` string
  flag (default `""`, empty usage string) and returns a resolver closure. The raw
  flag value is never exposed; `--by` does not appear in help output
  (`cli.go:1203-1204`).
- Empty actor is normalized by the store to `"unknown"`
  (`internal/store/store.go:1087-1090`).
- `displayAssignee("")` renders `"(unassigned)"` (`cli.go:1210-1215`).

Commands that register `--by`: `update` (`cli.go:939`), every transition via
`runTransition` (`cli.go:1356`), `comment add` (`cli.go:1468`), `import`
(`cli.go:1540`), `dep add` (`dependency.go:28`), `label add`
(`issue_relations.go:31`), `parent set` (`issue_relations.go:77`), `bulk label`
(`bulk.go:108`), `bulk close` (`bulk.go:141`), `bulk archive` (`bulk.go:172`).

### 1.12 Success-output breadcrumb

`emitBreadcrumb(w, token)` prints `deeper guidance: lit quickstart <token>`
(`quickstart_topics.go:62-75`). It **panics** if the token is not a registered
quickstart topic (`quickstart_topics.go:63-65`).

Breadcrumbs are emitted by: `new` → `"new"` (`cli.go:365`), `followup` → `"new"`
(`cli.go:437`), `update` → `"update"` (`cli.go:1032`), `rank` → `"update"`
(`cli.go:1113`), `rank set` → `"update"` (`cli.go:1148`), `dep add`/`dep rm` →
`"update"` (`dependency.go:65`, `dependency.go:90`), `label add`/`label rm` →
`"update"` (`issue_relations.go:48`, `issue_relations.go:70`), `parent set`/
`parent clear` → `"update"` (`issue_relations.go:103`, `issue_relations.go:121`),
and transitions via the table below.

`transitionBreadcrumbTopics` (`cli.go:1223-1227`): `start` → `"work"`,
`done` → `"done"`, `close` → `"done"`. Absent for `open`, `archive`,
`unarchive`, `delete`, `restore` → no breadcrumb (`cli.go:1444-1447`).

### 1.13 Sync-staleness banners

- Read commands print `printSyncStalenessWarning(ctx, w, ws, store, now)` FIRST,
  before their payload: `backlog` (`workable.go:137`), `next` (`next.go:53`),
  `show` **only in full-detail mode** (`cli.go:869-873`) — deliberately suppressed
  under `--field` so the machine-parseable output isn't corrupted
  (`cli.go:863-868`). Defined at `sync_staleness.go:186`.
- Write commands get `printMutationSyncStalenessWarning(stdout, ws, now)` after
  the handler succeeds and after the engine closes (`cli.go:135-137`,
  `sync_staleness.go:217`).

### 1.14 Workflow event dispatch

Every dispatch is `workflows.Dispatch(stdout, os.Stderr, ap.Workspace, occasion)`.
Occasion builders (`workflow_events.go`):
- `showTicketOccasion` → `EventShowTicket`, carries IssueID + Labels (`:21-27`)
- `backlogOccasion` → `EventShowBacklog`, no IssueID/Labels (`:32-34`)
- `nextPulledOccasion` → `EventNextPulled` (`:39-45`)
- `ticketCreatedOccasion` → `EventTicketCreated` (`:49-55`)
- `ticketUpdatedOccasion` → `EventTicketUpdated`; never carries Entered/Exited
  because `update` rejects `--status` (`:60-66`)
- `commentAddedOccasion` → `EventCommentAdded` (`:70-76`)
- `transitionOccasion(action, prior, issue)` → looks up
  `statusTransitionEvents` (`start`→`EventWorkStarted`, `done`→`EventWorkFinished`,
  `close`→`EventTicketClosed`, `open`→`EventTicketReopened`, `:83-88`), sets
  `Entered = issue.State()`, `Exited = prior.State()`; **panics** on an unmapped
  action name (`:103-115`).

Retention actions (archive/unarchive/delete/restore) are not `StatusAction`s and
fire no event (`cli.go:1408-1412`).

### 1.15 Prefix / ID resolution

There is **no fuzzy or short-prefix ID resolution anywhere in `internal/cli`.**
Issue IDs are passed verbatim to the store (e.g. `cli.go:874`, `cli.go:1372`,
`cli.go:1022`). A wrong ID yields `storage.NotFoundError` → exit 4.

The word "prefix" in this codebase means the *cosmetic ID prefix* on new IDs
(`lit prefix`, §2.24) — `ap.Workspace.IssuePrefix.Value()` is passed into
`CreateIssue` (`cli.go:354`, `cli.go:426`) and `ImportTree`/`BulkApply`
(`cli.go:1592`, `cli.go:1640`).

The only "does the literal look like a subcommand" disambiguation is in `rank`:
`args[0] == "set"` routes to `rank set`, justified because real IDs always carry a
prefix (`cli.go:1036-1042`).

### 1.16 Output rendering primitives (`output.go`)

- `contextIndent` = four spaces (`output.go:18`).
- `historyTimestampLayout` = `"Jan 2, 2006 3:04 PM MST"` (`output.go:22`),
  rendered in **local** time (`output.go:440-442`).
- `printIssueSummary` — the one-line success form used by `new`, `followup`,
  `update`, `rank`, and every transition:
  `"%s [%s/%s/%s/%s] %s%s\n"` = `id [state/type/topic/priority] title[labels]`
  (`output.go:49-52`). Labels render as `" [a,b]"` or empty (`output.go:426-431`).
- `formatIssueState(issue)` = `issue.State()`, plus `"+archived"` or `"+deleted"`
  when retention says so — never both (`output.go:526-541`).
- `resolveColumns(nil)` default column set = `id, state, topic, title`
  (`output.go:390-393`). Valid column names: `id, state, type, topic, priority,
  title, assignee, labels, updated_at, created_at, parent, blocked`
  (`output.go:395-397`). Unknown names are silently dropped; if nothing valid
  remains, the default set is used (`output.go:398-411`).
- `formatIssueColumns` per-column rendering (`output.go:345-378`): `priority` uses
  `Priority.String()` (normal/urgent); `assignee` and `labels` render `-` when
  empty; `updated_at`/`created_at` render RFC3339; `parent` renders the parent id
  or `-`; `blocked` renders the literal token `blocked` or `-`
  (`blockedLabel`, `output.go:383-388`).
- `printIssueLines` joins columns with `" | "` (`output.go:68-76`).
- `printIssueTable` writes an UPPERCASED tab-joined header then rows through a
  `tabwriter` (minwidth 2, tabwidth 2, padding 2, space pad) (`output.go:54-66`).
- `emptyDash(s)` → `"-"` when blank (`output.go:414-419`).
- `printLabels` prints labels comma-joined on one line (`output.go:421-424`).
- `humanizeCoarseDuration` buckets: ≥48h → `"%d days"`, ≥2h → `"%d hours"`,
  ≥2m → `"%d minutes"`, else `"under a minute"` (`output.go:451-462`).
- `indentLines(s, prefix)` prefixes every line, trailing newlines stripped
  (`output.go:550-556`).
- `writeJSON(w, v)` uses `json.Encoder` with two-space indent (`cli.go:1800-1804`).
  **`export` is the only command in this scope that emits JSON** (`cli.go:1524`).
  There is no `--json` / `--output` mode anywhere; `--output` is explicitly
  rejected (§1.1, §1.6).

### 1.17 Vocabularies (sealed sets used by flags)

- Issue types: `task, feature, bug, chore, epic` (`internal/model/issue_type.go:31-33`).
  `ParseIssueType` lowercases and trims; error text
  `"issue type must be <oxford-or list>"` (`issue_type.go:35`, `:42-50`).
  `epic` is the only container type (`issue_type.go:55-58`).
- Priorities: `0` normal, `1` urgent (`internal/model/priority.go:14-17`).
  `ParsePriority` rejects anything else with
  `"priority must be 0 (normal) or 1 (urgent)"` (`priority.go:38-44`).
- States: `open, in_progress, closed`; `in-progress` is normalized to
  `in_progress`; error `invalid status "<x>" (valid: open, in_progress, closed)`
  (`internal/model/lifecycle/lifecycle.go:98-109`).
- Resolutions: `duplicate, superseded, obsolete, wontfix`; error
  `"resolution must be one of: duplicate, superseded, obsolete, wontfix"`
  (`internal/model/lifecycle/resolution.go:47-54`).
- Relation types: `blocks, parent-child, related-to`; error
  `"relation type must be blocks, parent-child, or related-to"`
  (`internal/model/relation_type.go:16-33`).
- CLI parse-boundary wrappers: `parseIssueTypeFlag` and `parsePriorityFlag` wrap
  failures in `ValidationError` → exit 3 (`cli.go:1836-1854`);
  `parseIssueTypeSlice` (read path `--type`) returns the bare model error
  (`cli.go:1821-1830`); `parseStateSlice` likewise (`cli.go:1806-1815`).
- `issueTypeChoices()` renders `task|feature|bug|chore|epic` into flag help
  (`cli.go:1859-1866`).
- `splitCSV` splits on `,`, trims each part, drops empties, returns nil for a
  blank input (`cli.go:1875-1888`).

### 1.18 Readiness / workability — the exact predicate

**Step 1 — candidate set** (`classifyWorkable`, `cli.go:714-779`):
`ListIssues` with `Statuses = [open, in_progress]` (or the single `--status`
value if given), `IssueTypes`/`Assignees`/`LabelsAll` from the CLI filter,
`IncludeArchived=false`, `IncludeDeleted=false`, `Limit=0` (`cli.go:715-730`).
The store's default ordering is `item_rank ASC` (`cli.go:719-720`).

**Step 2 — leaves only**: `filterWorkableIssues` keeps issues whose
`Capabilities().Status != nil` (i.e. leaves, not containers) and whose status is
not `closed` (`cli.go:1151-1160`). Epics are therefore never workable rows.

**Step 3 — annotators** applied via `annotation.Annotate` (`cli.go:763-770`):
1. `newFieldAnnotator(requiredFields)` — from `config.Load(...).Ready.RequiredFields`
   (`cli.go:694-704`). Empty policy → no-op annotator (`ready_state.go:60-64`).
   A required field name not present in `model.IssueWireFields()` →
   `ValidationError{"required field %q does not exist on issue"}`
   (`ready_state.go:65-70`). For each unset required field it emits a
   `MissingField` annotation (`ready_state.go:71-86`). "Set" means: non-nil, and
   for strings non-blank, for arrays/maps non-empty; anything else counts as set
   (`isRequiredFieldSet`, `ready_state.go:447-460`).
2. `newBlockerAnnotator(details)` — for each `DependsOn` whose `State() != closed`,
   sorted by ID, emits `OpenDependency{Message: dep.ID}`; and additionally
   `RankInversion{Message: dep.ID}` when `dep.Rank > issue.Rank`
   (`ready_state.go:123-152`).
3. `newSiblingGateAnnotator(details, pendingSiblingsByEpic(siblingRelations))` —
   only when the parent exists and `parent.IsContainer()`; emits
   `EarlierSiblingPending{Message: sib.ID}` for each sibling satisfying
   `isEarlierSameLaneSibling` (`ready_state.go:173-190`).
   `isEarlierSameLaneSibling(sib, leaf) := sib.ID != leaf.ID && sib.Lane == leaf.Lane && sib.Rank < leaf.Rank`
   (`ready_state.go:198-200`). The sibling set is the epic's **unfiltered**
   `InPlay()` children (`pendingSiblingsByEpic`, `ready_state.go:228-238`), fetched
   via `GetRelationsByIDs(parentEpicIDs(details))` (`cli.go:746`), so siblings
   hidden by `--assignee/--type/--labels` still gate.
4. `newOrphanedAnnotator(orphanedThreshold)` — only for `in_progress` issues with
   `time.Since(UpdatedAt) >= 6h`; message
   `"in_progress for <dur truncated to minute> with no update"`
   (`ready_state.go:405-419`; threshold constant `orphanedThreshold = 6 * time.Hour`
   at `ready_state.go:48`).
5. `newNeedsDesignAnnotator()` — emits `NeedsDesign` for any issue carrying the
   label `needs-design` (`ready_state.go:22`, `:29-41`).
6. `newFocusPathAnnotator(focusPaths)` — emits `FocusPath{Message: goalID}` for
   issues on a focused goal's prerequisite closure (`ready_state.go:390-401`).

**Focus path derivation** (`fetchFocusPathGoals`, `ready_state.go:277-349`):
goals are issues with `Statuses=[open,in_progress]` and label `focus`
(`FocusLabel = "focus"`, `ready_state.go:246`; query at `:278-281`). BFS over the
prerequisite DAG: an issue's prerequisites are its `InPlay()` `DependsOn`, plus —
if it is a container — its `InPlay()` children, plus its earlier same-lane
`InPlay()` siblings (`:318-337`). The `path` map doubles as the visited set, so
shared prerequisites attribute to the first goal reached and cycles terminate
(`:297-299`). Relations are memoized through `relationsByID` (`:358-381`); a
frontier id missing from the store → `storage.NotFoundError` (`:313-317`).

**Step 4 — readiness classification** (`ClassifyReadiness`, `readiness.go:73-90`):
each annotation is dispatched on its declared `ReadinessRole`:
- `RoleBlocking` → appended to `blocking`
- `RoleOrphaned` → sets `orphaned = true`
- `RoleRankInversion` → appended to `rankInversions`
- `RoleNone` → contributes nothing (this is where `FocusPath` lands)
- anything else → **panics** with
  `"ClassifyReadiness: annotation carries an unclassified kind: <kind>"`
  (`readiness.go:85-87`).

`IsReady() := len(blocking) == 0` (`readiness.go:42`). So an issue is **ready**
iff it has no `MissingField`, no `OpenDependency`, no `EarlierSiblingPending`, and
no `NeedsDesign` annotation. `DependencyIDs()` returns only the `OpenDependency`
details (`readiness.go:55-63`).

**Step 5 — canonical ordering**, applied in this sequence (`cli.go:775-777`):
1. `sortByCompositeRank(rows, details)` — stable sort by
   (effective epic rank, own rank); a leaf whose parent is a container uses the
   parent's rank as its epic-position, otherwise its own rank
   (`ready_state.go:490-505`).
2. `sortByPriority` — stable, urgent (higher `Priority`) first
   (`ready_state.go:511-515`).
3. `sortByFocusPath` — stable, rows carrying a `FocusPath` annotation first;
   layered last so focus outranks urgent (`ready_state.go:530-537`).
Then `enrichWithParentEpic` sets `ParentEpic{ID,Title}` on rows whose parent is a
container (`ready_state.go:468-479`).

**Partition used by rollups**: `partitionWorkable` (`ready_state.go:582-595`):
`in_progress` state wins first (even if also blocked); else not-ready → blocked;
else ready.

`applyLimit(issues, limit)` truncates when `limit > 0` (`ready_state.go:539-544`).

---

## PART 2 — COMMANDS

### 2.1 `lit new` — Create an issue

- Registration: `{Name: "new", Summary: "Create an issue", GroupID: "operations"}`,
  `app.AccessWrite` (`register.go:288-289`). Handler `runNew` (`cli.go:327-366`).
- Flags (`cli.go:328-339`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--title` | string | `""` | Issue title. Help: "Issue title" |
| `--description` | string | `""` | "Issue description" |
| `--prompt` | string | `""` | "Reusable agent prompt for the work this issue captures" |
| `--type` | string | `task` | "Issue type: task\|feature\|bug\|chore\|epic" |
| `--topic` | string | `""` | "Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')" |
| `--parent` | string | `""` | "Optional parent issue ID; child IDs become parentID.\<n>" |
| `--priority` | int | `0` | "Priority: 0=normal, 1=urgent" |
| `--assignee` | string | `""` | "Assignee" (trimmed, `cli.go:352`) |
| `--labels` | string | `""` | "Comma-separated labels" (split by `splitCSV`) |
| `--lane` | string | `""` | "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes" |
| `--top` | bool | `false` | "Promote the new issue to the top of the order (the default appends it to the bottom of its frame)" |
| `--by` | string (hidden) | `""` | actor fallback (§1.11) — *note*: `runNew` registers no actor; `CreatedBy` is not set from the CLI here |

- `--top` maps to `storage.RankTop`; unflagged uses the zero `RankPlacement`
  (`rankPlacement`, `cli.go:319-325`).
- **No positional-argument check**: `runNew` never inspects `fs.NArg()`, so stray
  positionals are silently ignored (`cli.go:340-355`).
- Validation order: `--type` then `--priority`, both `ValidationError` → exit 3
  (`cli.go:343-350`).
- Store refusals (surfaced through `CreateIssue`, `internal/store/store.go:470-…`):
  blank title → `"title is required"` (`store.go:471-472`); blank/short/long topic
  → `"topic is required"` / `"topic must be at least N characters after
  normalization"` / `"topic must be at most N characters after normalization"`
  (`internal/issueid/slug.go:47-58`); a `--parent` id that does not exist → a
  not-found error (`store.go:511-515`). Labels are canonicalized, de-duplicated and
  sorted (`internal/store/labels.go:112-128`).
- Side effects: creates the issue; dispatches `EventTicketCreated`
  (`cli.go:359-361`); the store commit is Dolt-side.
- Output: `printIssueSummary` line then the `deeper guidance: lit quickstart new`
  breadcrumb (`cli.go:362-365`). Plus the mutation staleness banner from
  `runWithApp` (§1.13).

### 2.2 `lit followup` — File a follow-up parented to a just-closed ticket

- Registration `register.go:290-291`, `app.AccessWrite`. Handler `runFollowup`
  (`cli.go:375-438`).
- Flags (`cli.go:376-386`): `--on` (string, `""`, "Required parent issue ID
  (typically the just-closed ticket)"), `--title` (required), `--description`,
  `--prompt`, `--type` (default `task`), `--topic`, `--priority` (default 0),
  `--assignee`, `--labels`, `--top`. No `--parent`, no `--lane`.
- Refusal: blank `--on` or blank `--title` (after trim) →
  `UsageError{"usage: lit followup --on <id> --title <text> [--description <text>] [--topic <slug>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--top]"}`
  → exit 2 (`cli.go:390-394`).
- Reads the parent via `GetIssue(parentID)`; a missing parent is not-found → exit 4
  (`cli.go:395-398`).
- Defaults derived from the parent: blank `--topic` inherits `parent.Topic`
  (`cli.go:399-402`); blank `--description` becomes
  `"Follow-up surfaced at the close of <parent.ID>: <parent.Title>"`
  (`cli.go:403-406`).
- Creates with `ParentID = parent.ID` (`cli.go:415-427`), dispatches
  `EventTicketCreated`, prints the summary line, emits breadcrumb `new`
  (`cli.go:431-437`).
- No `fs.NArg()` check.

### 2.3 `lit ls` — List issues

- Registration `register.go:310-311`. **Not** wrapped by `appCmd`; the raw runner
  is `runList(ctx, stdout, args)` so `--at` can target a foreign store outside the
  current workspace (`register.go:307-311`, `cli.go:440-475`).
- Summary text: "List issues (rank by default; --at \<store-dir> lists a discovered
  store read-only)" (`register.go:310`).

**Store routing** (`runList`, `cli.go:453-475`):
- `extractAtDir(args)` scans for `--at <v>` or `--at=<v>`; a bare `--` terminates
  the scan (so a later `--at` is positional); a trailing `--at` with no value
  returns `("", true)` (`cli.go:486-502`).
- If `--at` is present and its value is blank-after-trim or starts with `-` →
  `UsageError{"usage: lit ls --at <store-dir>  (a storage directory from `lit stores`)"}`
  (`cli.go:458-461`).
- Otherwise opens `app.OpenLocationForRead(workspace.LocationFromStorageDir(atDir))`;
  a failure becomes `fmt.Errorf("open store at %q read-only: %w", atDir, err)`
  (`cli.go:462-468`), closed on return (`cli.go:469`).
- With no `--at`, opens the cwd workspace store with `app.AccessRead`
  (`cli.go:472-474`).

**Flags** (`runListWithStore`, `cli.go:504-524`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--at` | string | `""` | Registered so the shared parse accepts it; value already consumed by `runList` and *not re-read* (`cli.go:506-508`) |
| `--status` | string | `""` | Single state via `parseStateSlice`; error wrapped `parse --status: %w` (`cli.go:530-532`) |
| `--type` | string | `""` | Single issue type via `parseIssueTypeSlice`; error wrapped `parse --type: %w` (`cli.go:534-537`) |
| `--assignee` | string | `""` | Trimmed, single-element `Assignees` (`cli.go:541`) |
| `--search` | string | `""` | Appended to `SearchTerms` **only if the flag was visited** (`cli.go:555-557`) |
| `--ids` | string | `""` | CSV → `filter.IDs`, only if visited (`cli.go:558-560`) |
| `--labels` | string | `""` | CSV → `LabelsAll` (ALL must match), only if visited (`cli.go:561-563`) |
| `--has-comments` | bool | `false` | Only if visited; sets the pointer to the flag's value — so `--has-comments=false` filters to issues *without* comments (`cli.go:564-567`) |
| `--include-archived` | bool | `false` | `filter.IncludeArchived` (`cli.go:542`) |
| `--include-deleted` | bool | `false` | `filter.IncludeDeleted` (`cli.go:543`) |
| `--updated-after` | string | `""` | RFC3339; parse error → `parse --updated-after: %w` (`cli.go:568-574`) |
| `--updated-before` | string | `""` | RFC3339; parse error → `parse --updated-before: %w` (`cli.go:575-581`) |
| `--query` | string | `""` | Query language (see below) (`cli.go:520`) |
| `--sort` | string | `""` | `storage.ParseSortSpecs` (`cli.go:546-554`) |
| `--columns` | string | `""` | CSV of column names, lowercased (`cli.go:522`, `output.go:543-545`) |
| `--format` | string | `lines` | `lines` or `table` (`cli.go:523`) |
| `--limit` | int | `0` | `filter.Limit` (`cli.go:524`) |

- Flag help for `--query` (verbatim): "Query language: status:in_progress
  resolution:wontfix type:task has:comments sort:rank:asc limit:5 archived deleted
  text" (`cli.go:520`).
- `--sort` help: "Sort fields, e.g. rank:asc,updated_at:desc" (`cli.go:521`).
  `ParseSortSpecs` splits on `,`, then `field[:asc|desc]`; an unrecognized
  direction → `storage.ValidationError{"unsupported sort direction %q"}` → exit 3
  (`internal/storage/sort.go:18-50`).

**`--query` grammar** (`internal/query/query.go`):
- Tokenizer honors single and double quotes; an unterminated quote →
  `"unterminated quote in query"` (`query.go:241-274`).
- Terms (`query.go:76-164`): `status:<state>`, `resolution:<res>`, `type:<type>`,
  `assignee:<v>`, `id:<v>`, `label:<v>`, `has:comments` (any other `has:` →
  `unsupported has: filter %q`), `sort:<spec>`, `limit:<int>` (non-numeric →
  `limit must be an integer, got %q`; negative → `limit must be non-negative,
  got %q`), bare `archived`, bare `deleted`, `updated>=|>|<=|<|:<RFC3339>`
  (bad timestamp → `updated timestamp must be RFC3339`; unsupported comparator →
  `updated supports only >=, >, <=, <`; missing comparator/value errors wrapped
  `parse updated term %q`). Anything else becomes a free-text search term.
- `query.Merge(flagFilter, queryFilter)` (`query.go:31-74`): slices dedupe-merge
  (statuses, types, assignees, sort keys); resolutions/search/ids/labels plain
  append; `IncludeArchived`/`IncludeDeleted` OR; `Limit` overwritten when query
  limit > 0. Conflicting `has-comments` → `conflicting has-comments filters`;
  conflicting time bounds → `conflicting updated-after filters <t1> and <t2>`
  (`query.go:283-309`). `UpdatedAfter > UpdatedBefore` →
  `updated-after cannot be greater than updated-before` (`query.go:276-281`).

**Default active-work filter** (`cli.go:592-603`): if after all merging both
`filter.Statuses` and `filter.Resolutions` are empty, statuses default to
`[open, in_progress]`. A resolution filter alone therefore *does not* get clamped
(so `--query resolution:wontfix` reaches closed issues).

**Relation columns**: if the resolved column set contains `parent` or `blocked`
(`relationColumnNames`, `output.go:331`), `listRelationColumns` batch-loads
relations for the listed issues and derives `parentID` and
`blocked = len(liveIssues(rel.DependsOn)) > 0` (`cli.go:634-663`; `liveIssues`
keeps `InPlay()` issues, `output.go:480-488`). Otherwise no relation query is made
and both columns render `-`.

**Output**: `--format lines` (or empty) → `printIssueLines`; `table` →
`printIssueTable`; anything else →
`UnsupportedError{"unsupported --format \"<x>\"", Feature: "--format"}` → exit 3
(`cli.go:613-621`). Format value is lowercased and trimmed (`cli.go:613`).

- No `fs.NArg()` check; stray positionals are ignored.

### 2.4 `lit show` — Show issue details

- Registration `register.go:312-313`, `app.AccessRead`. Handler `runShow`
  (`cli.go:850-891`).
- Args: exactly one positional id; flag `--field` (string, `""`, help:
  "Comma-separated field names (e.g. description) to print with no surrounding
  context; omit for the full detail view") (`cli.go:851-853`).
- Refusals: `len(positional) != 1` or `fs.NArg() != 0` →
  `UsageError{"usage: lit show <id> [--field <name>[,<name>...]]"}` → exit 2
  (`cli.go:857-862`).
- Sync-staleness banner is printed first **only when `--field` is blank**
  (`cli.go:869-873`).
- Reads `GetIssueDetail(id)`; missing → exit 4 (`cli.go:874-877`).
- Dispatches `EventShowTicket` in **both** modes (`cli.go:878-881`).

**`--field` mode** (`printIssueFields`, `output.go:221-245`):
- Accepted field names and their renderings (`issueFieldNames`, `output.go:183-198`):
  `id`, `title`, `description`, `prompt`, `type`, `topic`,
  `priority` (`Priority.String()`), `status` (`i.State()`),
  `assignee` (raw, may be empty), `labels` (comma-joined, no spaces),
  `rank`, `lane`, `created_at` (RFC3339), `updated_at` (RFC3339).
- Names are lowercased and trimmed (`output.go:228`).
- Unknown name →
  `UsageError{"unknown --field \"<x>\"; valid fields: <sorted list>"}` → exit 2,
  and **nothing is printed** because all fields are resolved before any write
  (`output.go:229-234`, sorted list from `sortedIssueFieldNames`,
  `output.go:203-210`).
- Exactly one field → the bare value, no label (`output.go:235-238`).
- Two or more → `name: value` lines, in the requested order (`output.go:239-244`).
- No epic context, no parent block, no siblings (`cli.go:882-886`).

**Full-detail mode** (`printIssueDetail`, `output.go:78-176`), in exact order:
1. `<id>\n<title>\n\n` then
   `type: …`, `topic: …`, `priority: …`, `labels: …` (comma-space joined, `-` if
   none), `archived: …` (RFC3339 or `-`), `deleted: …` (`output.go:80-83`).
2. If the issue is a **leaf** (`Capabilities().Status != nil`):
   `status: <state>` and `assignee: <value or ->`; plus `resolution: <res>` only
   when a close recorded one (`output.go:87-98`).
   If it is a **container**: `children: %d closed, %d in_progress, %d open (%d total)`
   (`output.go:99-104`).
3. `unblocks: <ids>` — the IDs from `detail.Blocks` that are still `InPlay()`;
   omitted when empty (`output.go:105-112`, `openUnblockIDs` at `output.go:466-475`).
4. `\nparent:\n- <id> <title>\n`, and, when the parent has one, its description
   indented by two spaces (`output.go:117-126`).
5. `\ndescription:\n<text>\n` when non-empty (`output.go:127-131`).
6. `\nprompt:\n<text>\n` when non-empty (`output.go:132-136`).
7. Group blocks, each rendered as `\n<label>:\n` followed by
   `- <id> [<state>] <title>` lines and omitted entirely when empty
   (`printIssueGroup`, `output.go:297-313`), in this order:
   `children` (`output.go:137`), `depends_on` (`output.go:146`),
   `blocks` (`output.go:149`), `redirect` (single optional target adapted to a
   slice, `output.go:155`, `redirectGroup` at `output.go:290-295`),
   `related` (`output.go:158`).
   There is deliberately **no** `siblings` group here (`output.go:140-145`).
8. `\ncomments:` then `- [<createdBy>] <body>` with newlines in the body escaped
   to the literal `\n` (`output.go:161-170`).
9. **No** history block — history lives behind `lit history` (`output.go:171-175`).
10. Then `writeEpicContext` appends the epic plan block (§2.5).

### 2.5 Epic-context block appended by `lit show`

`writeEpicContext` (`epic_context.go:202-213`):
- `epicViewFor(issue, parent)` (`epic_context.go:186-194`): a container shows its
  own children with no focused child; a leaf whose parent is a container shows the
  parent's plan with itself focused; anything else returns nil and **nothing is
  printed**.
- Prints a leading blank line then `renderEpicContext(ec)` (`epic_context.go:211`).

`buildEpicContext` (`epic_context.go:116-166`):
- `GetRelationsByIDs([epicID])`; a missing epic → `storage.NotFoundError`
  (`epic_context.go:125-128`).
- One batch `GetRelationsByIDs(childIDs)`; a child listed but absent →
  `storage.NotFoundError` (`epic_context.go:136-157`).
- Per child, `classifyChildStatus(child, openBlockers(childRel))`
  (`epic_context.go:98-109`): `closed` → `[closed]`; `in_progress` →
  `[in_progress]`; else if it has ≥1 open blocker → `[blocked-by <firstBlockerID>]`;
  else `[ready]` (markers at `epic_context.go:28-42`). Blockers are the issue's
  non-closed `DependsOn`, sorted by id (`openBlockers`, `epic_context.go:233-240`).
- Cross-epic edges: for the epic node and every child that is not closed, each
  open `DependsOn` outside the epic membership set becomes a `BlockedExternally`
  edge, and each open `Blocks` outside becomes a `BlocksExternally` edge
  (`epic_context.go:247-258`). Membership = the epic id plus all child ids
  (`epicMemberIDs`, `epic_context.go:219-226`). Edges are sorted by
  (blocked, blocker) (`epic_context.go:281-292`).

`renderEpicContext` output shape (`epic_context.go:304-312`):
```
Epic: <epicID> — <epicTitle>
Why: <first non-blank line of epic description, leading '#'s stripped>

Children:
<child lines>
[Cross-epic dependencies block]
```
- `firstLine` strips whitespace and leading `#` characters (`epic_context.go:388-396`).
- Child line (`renderChildLine`, `epic_context.go:363-369`):
  `"    "` gutter (or `"  ▶ "` when focused), the status marker padded to
  `len("[in_progress]")` = 13 (`epic_context.go:90`), two spaces, the id, two
  spaces, the title, then `"  [lane: <lane>]"` when the lane is non-empty
  (`laneTag`, `epic_context.go:377-382`), then `"   (you are here)"` when focused.
- No children → `"  (none)\n"` (`epic_context.go:317-320`).
- Cross-epic block, omitted entirely when both directions are empty
  (`epic_context.go:333-342`):
```

Cross-epic dependencies:
  Blocks externally:
    <blocked> blocked by <blocker>
  Blocked externally:
    <blocked> blocked by <blocker>
```
  Each subsection is omitted when its slice is empty (`epic_context.go:348-358`).

### 2.6 `lit history` — State-transition history

- Registration `register.go:314-315`, `app.AccessRead`. Handler `runHistory`
  (`cli.go:898-912`).
- No flags beyond the implicit `--help`.
- Refusal: `len(positional) != 1 || fs.NArg() != 0` →
  `UsageError{"usage: lit history <id>"}` → exit 2 (`cli.go:904-906`).
- Reads `GetIssueDetail(id)` (`cli.go:907-910`).
- Output (`printIssueHistory`, `output.go:279-284`):
```
<id>
<title>

history:
```
  then `printHistoryEvents` (`output.go:254-272`): per event
  `- [<actor> @ <Jan 2, 2006 3:04 PM MST, local>] <action> <reason>` with newlines
  in the reason escaped to `\n`; an event with no `Action` displays the literal
  `update` (`output.go:257-263`). Then per change,
  `    <field>: <from or -> → <to or ->` (`output.go:265-269`).
- An empty event slice prints just the header (`output.go:276-278`).

### 2.7 `lit update` — Update issue fields

- Registration `register.go:316-317`, `app.AccessWrite`. Handler `runUpdate`
  (`cli.go:923-1033`).
- Flags (`cli.go:925-939`): `--title`, `--description`, `--prompt`, `--type`
  (default `""`), `--priority` (int, default 0), `--assignee`, `--labels`,
  `--lane`, `--status` (registered only to intercept it; help text
  "(removed) change status with the transition verbs: lit start|done|close|open"),
  `--reason` ("Reason recorded on the field-change event"), and hidden `--by`.
- Refusals:
  - `len(positional) != 1` or `fs.NArg() != 0` → `UsageError` with the usage line
    `"usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--lane <key>] [--reason <text>]"`
    (`cli.go:943-948`).
  - `--status` present (detected via `fs.Visit`) → `UsageError{statusViaVerbsGuidance}`
    → exit 2. Verbatim text: "lit update no longer changes status — the transition
    verbs are the single enforcer of the transition guardrails. Use: `lit start
    <id>` (claim → in_progress), `lit done <id>` (finish → closed), `lit close <id>
    --resolution <duplicate|superseded|obsolete|wontfix>` (close with an outcome),
    `lit open <id>` (reopen)" (`cli.go:921`, `cli.go:958-960`).
  - No field flag at all → `errors.New("lit update requires at least one field flag")`
    → exit 1 (`cli.go:1019-1021`). Note `--reason` alone does not count: `Reason`
    is set unconditionally but `Change.IsEmpty()` governs (`cli.go:968-973`,
    `cli.go:1019`).
- **Only visited flags are applied.** Each visited flag sets a pointer on
  `storage.UpdateIssueInput` (`cli.go:975-1018`):
  - `--title` / `--description` / `--prompt` set the raw value (no trimming at
    the CLI) (`cli.go:975-985`).
  - `--type` parses via `parseIssueTypeFlag` → `ValidationError` on a bad value
    (`cli.go:986-992`).
  - `--priority` parses via `parsePriorityFlag` (`cli.go:993-999`).
  - `--assignee` is trimmed and honored **verbatim** — session-identity resolution
    is deliberately *not* applied here, so an empty value clears the assignee
    (`cli.go:1000-1010`).
  - `--labels` CSV replaces the whole label set (`cli.go:1011-1014`).
  - `--lane` trimmed (`cli.go:1015-1018`).
- The `Change.Actor` is `resolveActor()` — session identity else `--by` else `""`
  (which the store normalizes to `unknown`) (`cli.go:969`).
- Applies via `Store.Apply(ctx, id, change)` (`cli.go:1022`).
- Dispatches `EventTicketUpdated` (`cli.go:1026-1028`), prints the summary line,
  emits the `update` breadcrumb (`cli.go:1029-1032`).

### 2.8 `lit rank` — Reorder an issue's rank

- Registration `register.go:318-319`, `app.AccessWrite`. Handler `runRank`
  (`cli.go:1035-1114`).
- If `args[0] == "set"`, routes to `runRankSet(args[1:])` (`cli.go:1040-1042`).
- Flags (`cli.go:1045-1048`): `--top` (bool, "Move to highest rank"),
  `--bottom` (bool, "Move to lowest rank"), `--above` (string, "Rank above this
  issue ID"), `--below` (string, "Rank below this issue ID").
- Refusals:
  - `len(positional) != 1` →
    `UsageError{"usage: lit rank <id> --top|--bottom|--above <id>|--below <id>"}`
    → exit 2 (`cli.go:1052-1054`).
  - Number of *visited* mode flags ≠ 1 →
    `ValidationError{"exactly one of --top, --bottom, --above, --below is required"}`
    → exit 3 (`cli.go:1055-1072`). Note this counts presence, not truthiness, so
    `--top=false` still counts.
  - No `fs.NArg()` check.
- Store calls (`cli.go:1080-1089`): `RankToTop`, `RankToBottom`,
  `RankAbove(issueID, *above)`, `RankBelow(issueID, *below)`. The relative forms
  return a `storage.RankMove{MovedID, AnchorID}`.
- **Frame substitution reporting** (`cli.go:1093-1105`):
  - If `move.MovedID != issueID`:
    `"<issueID> is inside <MovedID>; ranked the epic <MovedID> instead, leaving its internal order unchanged\n"`
  - If a named anchor was given and `move.AnchorID != namedAnchor`:
    `"<namedAnchor> is inside <AnchorID>; ranked relative to the epic <AnchorID> instead\n"`
  - (Asserted in `rank_frame_test.go:33-61`.)
- Then re-reads `GetIssue(move.MovedID)`, prints its summary line, emits the
  `update` breadcrumb (`cli.go:1106-1113`).

### 2.9 `lit rank set <id1> <id2> [...]`

- Handler `runRankSet` (`cli.go:1121-1149`). All args are treated as positionals
  (`splitArgs(args, len(args))`, `cli.go:1122`).
- Refusal: fewer than 2 positionals →
  `UsageError{"usage: lit rank set <id1> <id2> [<id3> ...]"}` → exit 2
  (`cli.go:1127-1129`).
- Calls `Store.RankSet(ctx, positional)` — atomic; stacks the named issues at the
  top in the given order (`cli.go:1130-1132`, doc at `cli.go:1116-1120`).
- Output: for each resolution where `RankedID != NamedID`,
  `"<NamedID> is inside <RankedID>; ranked the epic <RankedID> instead, leaving its internal order unchanged\n"`
  (`cli.go:1138-1144`); then
  `"ranked %d issues at top in order: <comma-joined RankedIDs>\n"`
  (`cli.go:1145-1147`); then the `update` breadcrumb (`cli.go:1148`).

### 2.10 Transition commands — `start`, `done`, `close`, `open`, `archive`, `unarchive`, `delete`, `restore`

All eight route through one handler `runTransition(ctx, stdout, ap, args, spec)`
(`cli.go:1353-1448`), registered via `r.transitionCmd(spec)` with
`app.AccessWrite` (`register.go:246-250`).

Registry rows and summaries:
- `start` — "Claim issue work", group `operations` (`register.go:320-321`)
- `done` — "Finish claimed work (success path; requires in_progress)" (`register.go:327-328`)
- `close` — "Close without finishing (wontfix / obsolete / duplicate; from any non-closed state)" (`register.go:329-330`)
- `open` — "Reopen issue(s)" (`register.go:331-332`)
- `archive` — "Archive issue(s)", group `retention` (`register.go:336-337`)
- `unarchive` — "Unarchive issue(s)" (`register.go:338-339`)
- `delete` — "Delete issue(s)" (`register.go:340-341`)
- `restore` — "Restore deleted issue(s)" (`register.go:342-343`)

**Common flags on every transition** (`cli.go:1354-1356`):
`--reason` (string, `""`, "Transition reason") and hidden `--by`.

**Per-spec flags** (`cli.go:1266-1306`):
- `start` adds `--assignee` (string, `""`, help "Assignee fallback when
  CLAUDE_CODE_SESSION_ID is unset (env always wins when set)") and `--take` (bool,
  `false`, help "Confirm taking over a lane another checkout claims right now
  (required for non-interactive callers; an interactive terminal is prompted
  instead)") (`cli.go:1274`, `cli.go:1280`). The action is
  `model.Start{Assignee: resolveIdentity(*assignee)}` (`cli.go:1276`).
- `close` adds `--resolution` (string, `""`, "Close resolution (required):
  duplicate|superseded|obsolete|wontfix") and `--of` (string, `""`, "Canonical
  ticket a duplicate/superseded close redirects to (required for those, rejected
  otherwise)") (`registerCloseOutcomeFlags`, `cli.go:1311-1315`).
- `done`, `open`, `archive`, `unarchive`, `delete`, `restore` register **no**
  extra flags — `fixedAction` (`cli.go:1260-1264`). Passing e.g.
  `lit done --resolution x` is therefore an unknown-flag `UsageError` → exit 2.

**Argument handling** (`cli.go:1362-1368`): after parsing, exactly one remaining
positional is required; otherwise `errors.New("usage: lit <name> <id> [--reason <text>]")`
— a **plain error**, so exit code 1, not 2.

**Sequence** (`cli.go:1370-1447`):
1. `GetIssue(issueID)` pre-read (`cli.go:1372-1375`) — missing → exit 4.
2. `buildAction()` (`cli.go:1377-1380`).
3. `authorize(ctx, stdout, ap, issueID, prior)` — §2.11 (`cli.go:1386-1388`).
4. `actor := resolveActor()`; `Store.Apply(ctx, issueID, Change{Action, Actor, Reason})`
   (`cli.go:1394-1398`).
5. If the action is a `StatusAction`, dispatch the transition occasion
   (`cli.go:1408-1412`).
6. If the action is `model.Start` and the prior assignee was non-empty and
   different from the new one:
   `"claim transferred: <priorOwner> -> <newAssignee or (unassigned)>\n"`
   (`cli.go:1417-1423`).
7. `printIssueSummary` (`cli.go:1425-1427`).
8. If the action is a `StatusAction` whose `Target() == model.StateClosed`
   (i.e. `done` and `close`), re-read `GetIssueDetail` and print the close
   adjacency block (`cli.go:1435-1443`) — §2.12.
9. Breadcrumb per `transitionBreadcrumbTopics` (`cli.go:1444-1447`).

**Close outcome validation** — `closeOutcomeFromFlags(resolution, target, usage)`
(`cli.go:1325-1351`), shared with `bulk close` (`bulk.go:149`):
- `model.ParseResolution` failure → `UsageError{"<usage>\n<parse error>"}` → exit 2
  (`cli.go:1326-1329`). For `lit close`, the usage string is
  `"usage: lit close <id> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]"`
  (`cli.go:1292`). A missing `--resolution` therefore fails through this path —
  `--resolution` is effectively required.
- Redirecting resolutions (`duplicate`, `superseded`) with a blank `--of` →
  `UsageError{"closing as <res> redirects to a canonical ticket — name it with --of"}`
  (`cli.go:1331-1334`).
- A non-blank `--of` on a terminal resolution (`obsolete`, `wontfix`) →
  `UsageError{"--of applies only to duplicate/superseded closes, not <res>"}`
  (`cli.go:1335-1337`).
- Outcome construction: `duplicate` → `model.Duplicate{Of: target}`;
  `superseded` → `model.Superseded{By: target}`; `obsolete` → `model.Obsolete{}`;
  `wontfix` → `model.Wontfix{}` (`cli.go:1338-1347`). An unreachable default
  returns `fmt.Errorf("resolution %q has no close outcome", parsed)`
  (`cli.go:1348-1350`).

**Store-level refusals reaching every transition:**
- An archived or deleted issue rejects any status action:
  `"cannot <action> archived or deleted issue"` (`internal/store/store.go:89-96`).
- A container (epic) rejects any status action with `ContainerActionError`
  (`internal/model/model.go:315-317`).
- The redirect target must exist, must not be the closing issue itself, and must
  not be deleted: `"closing as <res> requires a canonical target issue to redirect
  to"`, `"cannot redirect <id> to itself"`, `"cannot redirect <id> to <target>:
  the canonical issue is deleted"` (`internal/store/store.go:1542`, `:1547`,
  `:1554`).
- `archive` on a deleted issue → `"cannot archive deleted issue"`;
  `unarchive` on a deleted issue → `"cannot unarchive deleted issue"`
  (`internal/model/lifecycle/retention.go:73`, `:82`).
- **There is no from-state precondition on `done`.** `Store.Apply` performs no
  status-precondition check (`internal/store/store.go:1073-1130`), and
  `applyStatusAction` is total over the leaf states
  (`internal/model/lifecycle/status_states.go:134-161`). The registry summary
  "requires in_progress" (`register.go:327`) is not enforced by any code path in
  this repo. A same-state transition is a no-op that records nothing
  (`status_states.go:136-138`, `internal/store/store.go:1064-1071`).

### 2.11 `lit start` takeover gate

`startSpec.authorize` → `authorizeStart(ctx, stdout, ap, issueID, prior, *take)`
(`cli.go:1279-1284`, implementation `claims_takeover.go:65-87`):
1. `GetRelationsByIDs([issueID])` and `model.LaneOf(prior, parent)`
   (`claims_takeover.go:66-70`).
2. `gatherClaimContext(ctx, stdout, ap)` (`claims_takeover.go:71-74`).
3. `classifyTakeover(standing, self)` (`claims_takeover.go:37-51`):
   - `Held` by self, `Stale` by self, or `Unclaimed` → `takeoverNone` (no-op).
   - `Stale` by another → `takeoverStaleInformed`.
   - `Held` by another → `takeoverFreshConfirm`.
4. **Stale, foreign**: prints
   `"<claim line> — check for unmerged branches or PRs on this lane before building on it\n"`
   and proceeds (`claims_takeover.go:110-118`).
5. **Fresh, foreign** (`confirmFreshTakeover`, `claims_takeover.go:129-152`):
   - Non-interactive stdout (`!isTerminal(stdout)`) and no `--take` →
     `fmt.Errorf("<claim line> — this lane is claimed and active; pass --take to confirm the takeover")`
     → exit 1 (`claims_takeover.go:134-137`).
   - Non-interactive with `--take` → prints `"<claim line> — taking over (--take)\n"`
     and proceeds (`claims_takeover.go:138-140`).
   - Interactive → prints `"<claim line>\ntake over this lane? [y/N] "`, reads a
     line from stdin; a read error other than EOF →
     `"read takeover confirmation: %w"`; an answer not starting with `y`
     (case-insensitive, trimmed) → `errors.New("takeover declined")` → exit 1
     (`claims_takeover.go:142-151`).

Claim line format — `formatClaimLine(cc, lane, now)` (`claims_render.go:23-44`):
returns `false` (no line) for an `Unclaimed` lane. Otherwise
`"<prefix>[ · contested by <s1>, <s2>] · <age> ago[ · <lane progress>]"` where:
- prefix, when the holder resolves to a live local worktree:
  `"claimed here[ (stale)]: <path> (<branch or 'detached HEAD'>)"`
  (`claims_render.go:52-63`)
- otherwise: `"claimed: stream <first 8 chars of stream token> (elsewhere|stale)"`
  (`claims_render.go:64-68`, `shortStream` at `claims_render.go:92-99`)
- age via `humanizeCoarseDuration(now - tenure.LastActivity)` (`claims_render.go:39`)
- lane progress: `"<activeID> in progress, <done>/<total> done"` or
  `"<done>/<total> done"`; empty for a zero LaneProgress
  (`claims_render.go:76-84`).

`gatherClaimContext` (`claims_context.go:43-108`) reads config, lists **all**
issues including archived and deleted (`claims_context.go:57`), fetches all
relations (`:65`), lists all events (`:73`), builds evidence (`:77`), enumerates
live checkouts (`:91`). If checkout enumeration fails it prints to **stdout**:
`"warning: could not enumerate local checkouts (<err>) — claim liveness check and local addresses skipped, freshness alone governs\n"`
and continues with zero local checkouts (`claims_context.go:91-95`).

### 2.12 Close/done adjacency block

`printCloseAdjacency(w, detail)` (`output.go:498-524`), printed after the summary
line for `done` and `close`, each group omitted when empty:
- `\nparent:\n- <id> [<state>] <title>` (`output.go:499-505`)
- `\nsiblings:` — only the `InPlay()` siblings (`output.go:506`)
- `\nredirect:` — the redirect target if any (`output.go:512`)
- `\nrelated:` (`output.go:515`)
- `\nunblocks: <comma-joined ids of still-live dependents>` (`output.go:518-522`)

### 2.13 `lit backlog` — Full workable backlog

- Registration `register.go:298-299`, `app.AccessRead`, handler
  `workableRun(backlogView)`. Summary: "List the full workable backlog in
  priority/rank order (blocked items inline)".
- `backlogView` preset (`workable.go:90-97`): `hasFilters: true`,
  `hasLimit: true`, `hasColumns: true`, order = `orderCanonical` (no-op,
  `workable.go:82`), keep = `keepAll` (`workable.go:84`), render =
  `printBacklogOutput`, occasion = `backlogOccasion()`.
- Flags (`runWorkable`, `workable.go:111-117`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--assignee` | string | `""` | "Filter by assignee" (always registered) |
| `--type` | string | `""` | "Filter by issue type" |
| `--status` | string | `""` | "Filter by status: open\|in_progress" |
| `--labels` | string | `""` | "Comma-separated labels all of which must match" |
| `--limit` | int | `0` | "Limit results" — applied **after** ordering (`workable.go:159`) |
| `--columns` | string | `""` | "Comma-separated output columns" |

- Refusal: any positional argument → `UsageError{view.usage()}` (`workable.go:121-123`).
  `usage()` builds `"usage: lit backlog [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--limit N] [--columns ...]"`
  (`workable.go:67-80`).
- `--status closed` (or any unparseable state) →
  `UsageError{"invalid --status \"<x>\" (valid: open, in_progress)"}` → exit 2
  (`parseWorkableStatus`, `workable.go:176-185`).
- Bad `--type` → `UsageError{"invalid --type \"<x>\": <err>"}` → exit 2
  (`parseWorkableType`, `workable.go:191-200`).
- Prints the sync-staleness warning first (`workable.go:137-139`).
- Runs the shared workable pipeline (§1.18) via `gatherWorkableAnnotated`
  (`workable.go:148-156`), then `gatherClaimContext` (`workable.go:160-163`).
- Dispatches `EventShowBacklog` after rendering (`workable.go:167`).

**Output** (`printBacklogOutput`, `backlog.go:32-64`):
1. The `backlogPreamble` verbatim (`backlog.go:19-26`):
```
This is the full backlog in priority/rank order — every workable item, blocked or not.
Items at the top are ranked higher than items below them. Blocked items stay where they were ranked
so you can see WHY the queue is shaped this way, not just what is ready next.
Read every row: each carries its parent epic, dependencies, blocking reasons, and what closing it would unblock.
That context is the ordering rationale — the dependency graph IS the priority story.
Rows claimed by another checkout show who holds them and how fresh, but claim visibility here is
just that — visibility; only 'lit next' routes by claim, serving this checkout's own lanes first.
Use 'lit next' to pick the top workable item to start.
```
2. A rule of 80 `─` characters, then a blank line (`backlog.go:37-42`).
3. If empty: the literal `(backlog empty)` and nothing else (`backlog.go:44-49`).
4. Otherwise, per row: `"%2d. %s"` — a right-aligned 1-based index, then the
   resolved columns joined by two spaces (`backlog.go:54`).
5. Then the indented context block (`printBacklogContext`, `backlog.go:72-98`), in
   this order, each omitted when empty:
   - `    epic: <epicID>  <epicTitle>` (`output.go:28-34`)
   - `    blocked: <reasons joined by "; ">` — only non-dependency blockers,
     rendered as `missing <field>` for `MissingField` and `needs-design` for
     `NeedsDesign` (`backlog.go:79-83`, `nonDependencyBlockingReasons` at
     `backlog.go:104-115`). `EarlierSiblingPending` appears in **neither** the
     blocked line nor the depends-on line.
   - `    depends on: <ids joined by ", ">` (`backlog.go:84`, `output.go:41-47`)
   - `    in_progress: <age truncated to minute>[ (ORPHANED)]` for in-progress
     rows (`backlog.go:87-91`, `inProgressSuffix` at `ready_state.go:616-623`)
   - `    <claim line>` when the row's lane is Held or Stale (`backlog.go:92-96`)
   - `    unblocks: <ids of rows that depend on this one>` — derived from the
     classified open-dependency facts of the listed rows only
     (`backlog.go:97`, `buildUnblocksMap` at `ready_state.go:564-572`)
6. Finally, if any row carries a `RankInversion` annotation:
   `"\nWarning: %d rank inversion(s) — dependencies ranked below their dependents. Run `lit doctor --fix` to repair. <agent-instructions>This command is idempotent and safe to run without confirmation.</agent-instructions>\n"`
   (`printRankInversions`, `ready_state.go:627-637`).

Lane for the claim line is `model.LaneOf(entry.Issue, details[entry.ID].Parent)`
(`backlog.go:58`).

### 2.14 `lit next` — Print the next workable leaf

- Registration `register.go:302-303`, `app.AccessRead`. Handler `runNext`
  (`next.go:31-74`).
- Flags (`next.go:33-36`): `--assignee`, `--type`, `--status`, `--labels` — same
  parsing/refusals as `backlog` (`next.go:43-50`). **No** `--limit`, **no**
  `--columns`.
- Refusal: any positional → `UsageError{nextUsage}` where `nextUsage` =
  `"usage: lit next [--type ...] [--status ...] [--labels ...] [--assignee <user>]"`
  (`next.go:29`, `next.go:40-42`).
- Retired flag: `--continue` is intercepted by the shared parser with
  `UnsupportedError` → exit 3 (`cli.go:290-294`).
- Prints the sync-staleness warning first (`next.go:53`).
- Gathers the workable set, then the claim context (`next.go:56-68`), then routes
  (`next.go:69`).

**Routing precedence** — `routeNext(rows, details, standings, self)`
(`next_route.go:81-128`). `rows` are already in canonical order (§1.18).

Let `laneOf(row) = model.LaneOf(row.Issue, details[row.ID].Parent)`
(`next_route.go:82-84`), and
`isReadyRow(row) := row.State() == open && ClassifyReadiness(row.Annotations).IsReady()`
(`next_route.go:133-135`).

1. If `self.Present()` (`next_route.go:86`):
   - `ownLanes` = the lanes of rows whose standing is `claims.Held` with
     `By == self` (`next_route.go:87-92`).
   - If `ownLanes` is non-empty:
     a. First row in own lanes that `isReadyRow` → **`ServedFromClaim{Row}`**
        (`next_route.go:94-98`).
     b. Else `onPathDependency` — walk own-lane rows that are open but not ready,
        and return the first of their open dependencies that is itself present in
        the row set and ready → **`ServedFromClaim{Row: dep}`**
        (`next_route.go:99-101`, `next_route.go:160-176`).
     c. Else, in the epics of the own lanes: the first row whose lane belongs to
        one of those epics, is not an own lane, `isReadyRow`, and whose standing
        is `claims.Unclaimed` → **`ServedFromEpicLane{Row, Epic, Lane}`**
        (`next_route.go:102-113`).
     d. Else → **`Exhausted{Epics: sorted, Blocked: blockedDependencyIDs(...)}`**
        (`next_route.go:114-117`). `blockedDependencyIDs` collects distinct
        open-dependency IDs of the **open** rows in scope (own lanes plus the rest
        of their epics), in encounter order (`next_route.go:183-200`).
2. Otherwise (no self, or no own lanes): the first row that `isReadyRow` and whose
   lane is `claims.Unclaimed` → **`ServedFromGlobal{Row, Lane}`**
   (`next_route.go:121-126`).
3. Else → **`NoWork{}`** (`next_route.go:127`).

`isUnclaimed` accepts only `claims.Unclaimed` — a `Stale` lane is never routed
into by bare `next` (`next_route.go:148-151`).

**Rendering** — `renderNextOutcome` (`next.go:82-111`):
- `ServedFromClaim` → no announcement (`next.go:86-87`).
- `ServedFromEpicLane` → prints
  `"continuing epic <Epic>: starting <RowID> claims <Lane>\n"` (`next.go:88-91`).
- `ServedFromGlobal` → prints `"starting <RowID> claims <Lane>\n"` (`next.go:92-94`).
- `Exhausted` → returns `exhaustedError(o)` and prints no ticket (`next.go:95`).
  Message (`next_route.go:213-222`): scope is `"epic(s) <a, b>"` when epics are
  known else `"your claimed lane(s)"`; with no blockers:
  `"no ready work in <scope> — nothing else is queued behind what's already in progress; picking up other work is a deliberate re-focus, not a bare \`next\`"`;
  with blockers:
  `"no ready work in <scope> — blocked on <ids> (unclaimed, on your path — \`lit start\` it); picking up other work is a deliberate re-focus, not a bare \`next\`"`.
  Both exit 1.
- `NoWork` → `errors.New("no ready work")` → exit 1 (`next.go:96-97`).
- Any other outcome type → panic (`next.go:98-99`).
- On a served row: `printNextSummary(w, row, cc, lane)` (`next.go:106-109`), which
  prints the **default columns** (`id state topic title`) joined by two spaces
  (`ready_state.go:550-557`), then `printInlineDeps`
  (`ready_state.go:601-614`): `    epic: …`, `    depends on: …`, the claim line,
  and `    unblocks: …` — but `next` passes a **nil** unblocks map, so the
  unblocks line never appears (`ready_state.go:556`).
- On success it dispatches `EventNextPulled` (`next.go:73`, `next.go:110`).

`lit next` performs **no writes** — it is registered `app.AccessRead`
(`register.go:303`); the "claims <lane>" announcement describes what a subsequent
`lit start` would claim.

### 2.15 `lit orphaned` — Stale in-progress issues

- Registration `register.go:304-305`, `app.AccessRead`. Handler `runOrphaned`
  (`cli.go:790-831`). Summary: "List in_progress issues with no recent updates".
- Flags: `--assignee` (string, `""`, "Filter by assignee") (`cli.go:792`).
- Refusal: any positional → `UsageError{"usage: lit orphaned [--assignee <user>]"}`
  → exit 2 (`cli.go:796-798`).
- Query: `Statuses = [in_progress]`, assignee filter, no archived, no deleted
  (`cli.go:799-804`). Containers dropped via `filterWorkableIssues`
  (`cli.go:813`).
- Annotates with `newOrphanedAnnotator(orphanedThreshold)` only (`cli.go:814`) and
  keeps rows where `ClassifyReadiness(...).IsOrphaned()` (`cli.go:818-823`).
- Sorted oldest-`UpdatedAt` first (`cli.go:827-829`).
- Output (`printOrphanedText`, `cli.go:833-848`):
  - Empty → `"No orphaned issues."`
  - Otherwise, per row: columns `id | state | topic | assignee | title` joined by
    `" | "`, then `" | Last Update: <age truncated to minute>"`.

### 2.16 `lit children <parent-id>`

- Registration `register.go:350-351`, `app.AccessRead`. Handler `runChildren`
  (`issue_relations.go:124-138`). Summary: "List child issues by rank".
- No flags.
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit children <parent-id>"}` → exit 2
  (`issue_relations.go:130-132`). No `fs.NArg()` check.
- Output: `printIssueLines` with the fixed column set `id, state, title` joined by
  `" | "`, and a nil relations map (`issue_relations.go:137`).

### 2.17 `lit comment` — Add / remove comments

Family `commentFamily`, usage `"usage: lit comment <add|rm> ..."` (`cli.go:1456-1462`).
Both subcommands are `app.AccessWrite`. Missing/unknown subcommand → the bare
usage string as a plain error → exit 1 (`register.go:112-123`).

**`lit comment add <id> --body <text>`** (`runCommentAdd`, `cli.go:1464-1488`):
- Flags: `--body` (string, `""`, "Comment body"), hidden `--by`.
- Refusal: `len(positional) != 1` or `fs.NArg() != 0` →
  `UsageError{"usage: lit comment add <id> --body <text>"}` → exit 2
  (`cli.go:1472-1477`).
- Store refusal: a blank body (after trim) →
  `errors.New("comment body is required")` → exit 1
  (`internal/store/store.go:1141-1144`). A missing issue → exit 4
  (`store.go:1137-1140`).
- The comment id is `"cmt-" + uuid` and `CreatedBy` empty is normalized to
  `"unknown"` (`store.go:1146-1150`).
- Dispatches `EventCommentAdded` (`cli.go:1484-1486`).
- Output: `printComment` → `"<issueID> <commentID>\n"` (`cli.go:1509-1512`).
  **No breadcrumb.**

**`lit comment rm <comment-id>`** (`runCommentRm`, `cli.go:1490-1507`):
- No flags (not even `--by`).
- Refusal: `len(positional) != 1` or `fs.NArg() != 0` →
  `UsageError{"usage: lit comment rm <comment-id>"}` → exit 2 (`cli.go:1496-1501`).
- Store: blank id → `"comment id is required"`; unknown id →
  `storage.NotFoundError{Entity: "comment", ID: id}` → exit 4
  (`internal/store/store.go:1160-1179`).
- Output: `"<issueID> <commentID>\n"` for the deleted comment (`cli.go:1506`).

### 2.18 `lit label` — Manage labels

Family `labelFamily`, usage `"usage: lit label <add|rm> ..."`
(`issue_relations.go:12-18`); both `app.AccessWrite`.

**`lit label add <issue-id> <label>`** (`issue_relations.go:28-49`):
- Two positionals via `splitArgs(args, 2)`; hidden `--by` registered.
- Refusal: `len(positional) != 2` or `fs.NArg() != 0` →
  `UsageError{"usage: lit label add <issue-id> <label>"}` → exit 2
  (`issue_relations.go:35-40`).
- Calls `Store.AddLabel(AddLabelInput{IssueID, Name, CreatedBy: resolveActor()})`
  (`issue_relations.go:41`); labels are normalized through `model.NormalizeLabel`
  (`internal/store/labels.go:130-135`).
- Output: the resulting full label set, comma-joined on one line
  (`printLabels`, `output.go:421-424`), then the `update` breadcrumb.

**`lit label rm <issue-id> <label>`** (`issue_relations.go:51-71`):
- Same shape; no `--by`. Usage `"usage: lit label rm <issue-id> <label>"`.
- Calls `Store.RemoveLabel(issueID, label)`; prints the remaining labels and the
  `update` breadcrumb.

Reserved label semantics: `needs-design` blocks readiness (§1.18, `ready_state.go:22`);
`focus` marks a goal for focus-path ordering (`ready_state.go:246`).

### 2.19 `lit parent` — Manage parent relationships

Family `parentFamily`, usage `"usage: lit parent <set|clear> ..."`
(`issue_relations.go:20-26`); both `app.AccessWrite`. Group `structure`
(`register.go:348-349`).

**`lit parent set --child <id> --parent <id>`** (`issue_relations.go:73-104`):
- Flags: `--child` ("Child issue ID (required)"), `--parent` ("Parent issue ID
  (required)"), hidden `--by`.
- Refusal: blank `--child`, blank `--parent`, or `fs.NArg() != 0` →
  `UsageError{"usage: lit parent set --child <id> --parent <id>"}` → exit 2
  (`issue_relations.go:81-83`).
- Calls `Store.SetParent(SetParentInput{ChildID, ParentID, CreatedBy})`
  (`issue_relations.go:84-88`).
- Output: the edge rendered through the *same* projection `dep` uses —
  `"<child> --child-of--> <parent>"` (`issue_relations.go:100`, via
  `depRelationForCLI`/`depRelationLine`, `dependency.go:137-140`, `:181-192`) —
  then the `update` breadcrumb.

**`lit parent clear <child-id>`** (`issue_relations.go:106-122`):
- No flags. Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit parent clear <child-id>"}` → exit 2
  (`issue_relations.go:112-114`). No `fs.NArg()` check.
- Calls `Store.ClearParent(childID)`; prints `ok` then the `update` breadcrumb.

### 2.20 `lit dep` — Manage dependency edges

Family `depFamily`, usage `"usage: lit dep <add|rm|ls> ..."` (`dependency.go:14-21`).
`add`/`rm` are `app.AccessWrite`, `ls` is `app.AccessRead`. Group `structure`
(`register.go:352-353`).

**`lit dep add --from <id> --to <id> [--type ...]`** (`dependency.go:23-66`):
- Flags: `--type` (string, default `"blocks"`, help "Relation type:
  blocks|parent-child|related-to"), `--from` ("Source issue ID (required)"),
  `--to` ("Target issue ID (required)"), hidden `--by`.
- Refusals, in order:
  1. Blank `--from` or `--to`, or `fs.NArg() != 0` →
     `UsageError{"usage: lit dep add --from <id> --to <id> [--type blocks|parent-child|related-to]"}`
     → exit 2 (`dependency.go:32-34`).
  2. Bad `--type` → the bare `model.ParseRelationType` error → exit 1
     (`dependency.go:37-40`).
  3. Self-loop `from == to` → `fmt.Errorf("dep add: self-loop rejected (%s -> %s)")`
     → exit 1 (`dependency.go:45-47`). Transitive cycles are **not** detected
     (`dependency.go:43-44`).
  4. For `blocks` only: `rejectSameEpicBlocks` — if both endpoints resolve to the
     same epic membership, `ValidationError{sameEpicBlocksRejectionMessage}` →
     exit 3 (`dependency.go:51-55`, `:149-162`). Verbatim message:
     "Do not set 'blocks' relationships between two issues in the same epic.  Use
     rank to specify that one issue must be completed before another issue"
     (`dependency.go:145` — note the double space).
     Epic membership: the issue's own ID if it is a container, else the parent's
     ID if the parent is a container, else `""` (floating)
     (`issueEpicID`, `dependency.go:167-179`). Two floating issues are not
     same-epic (`dependency.go:158`).
- Endpoint orientation: `rt.StoreEndpoints(from, to)` swaps the pair for `blocks`
  (stored dependent→dependency) and is an involution
  (`dependency.go:56`, `internal/model/relation_type.go:39-44`).
- Output: `depRelationLine(depRelationForCLI(rel))` then the `update` breadcrumb
  (`dependency.go:61-65`). Line formats (`dependency.go:181-192`):
  - `blocks` → `"<src> --blocks--> <dst>"`
  - `parent-child` → `"<src> --child-of--> <dst>"`
  - `related-to` → `"<src> --related-to--> <dst>"`
  - default → `"<src> --depends-on--> <dst>"`

**`lit dep rm --from <id> --to <id> [--type ...]`** (`dependency.go:68-91`):
- Same flags minus `--by`; same usage refusal string with `rm`
  (`dependency.go:76-78`). No self-loop or same-epic check.
- Calls `Store.RemoveRelation(srcID, dstID, rt)`; prints `ok` and the `update`
  breadcrumb (`dependency.go:84-90`).

**`lit dep ls <issue-id> [--type ...]`** (`dependency.go:93-131`):
- One positional; `--type` (string, `""`, "Filter relation type").
- Refusal: `len(positional) != 1` or `fs.NArg() != 0` →
  `UsageError{"usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to]"}`
  → exit 2 (`dependency.go:100-105`).
- A non-blank `--type` is parsed (bad value errors); blank means no filter
  (`dependency.go:109-116`).
- Output: one `depRelationLine` per relation, flipped back to CLI orientation
  (`dependency.go:121-130`). No breadcrumb, no header, empty output for none.

### 2.21 `lit bulk` — Bulk issue operations

Family `bulkFamily`, usage `"usage: lit bulk <label|close|archive> ..."`
(`bulk.go:15-30`). Group `operations` (`register.go:391-392`).
Rows: `label` (write), `close` (write), `archive` (write), and hidden
`import` (`skipApp: true`, retired).

**Shared failure semantics** — `runBulkOver(stdout, ids, op)` (`bulk.go:66-81`):
- Applies `op` to each id **in order**.
- Each success writes `"<id> ok\n"` to stdout (`bulk.go:73`).
- Each failure is collected; **failures never reach stdout** (`bulk.go:69-71`).
- If any failed, returns `BulkFailureError{Failures}` → exit 1
  (`bulk.go:77-79`, `exit.go:84-90`), whose message is
  `"bulk operation: <n> item(s) failed: <id>: <err>; <id>: <err>"`
  (`bulk.go:52-58`), printed by `WriteCommandError` to stderr with the
  `bulk_partial_failure` remediation (§1.9).

**`lit bulk label <add|rm> --ids <csv> --label <name>`** (`runBulkLabel`,
`bulk.go:101-129`):
- Nested family `bulkLabelFamily`, usage `"usage: lit bulk label <add|rm> ..."`
  (`bulk.go:83-99`).
- With zero args after `label` → `errors.New(bulkLabelFamily.usage)` → exit 1
  (`bulk.go:102-104`).
- Flags parsed from `args[1:]`: `--ids` ("Comma-separated issue IDs"),
  `--label` ("Label name"), hidden `--by` (`bulk.go:106-109`).
- **Error precedence is deliberate** (`bulk.go:119-124`): empty `--ids` →
  `ValidationError{"--ids is required"}` → exit 3; then blank `--label` →
  `ValidationError{"--label is required"}` → exit 3; only *then* is an unknown
  add/rm action resolved (`bulk.go:112-124`).
- `add` calls `Store.AddLabel` with the resolved actor; `rm` calls
  `Store.RemoveLabel` (actor unused) (`bulk.go:86-98`).

**`lit bulk close --ids <csv> --resolution <res> [--of <id>] [--reason <text>]`**
(`runBulkClose`, `bulk.go:136-162`):
- Flags: `--ids`, `--reason` ("Lifecycle reason"), `--resolution` + `--of` (via the
  shared `registerCloseOutcomeFlags`), hidden `--by` (`bulk.go:137-141`).
- Empty `--ids` → `ValidationError{"--ids is required"}` → exit 3
  (`bulk.go:145-148`).
- Outcome via the same `closeOutcomeFromFlags` gate as `lit close`, with usage
  string `"usage: lit bulk close --ids <id,id,...> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]"`
  (`bulk.go:149-152`) — so `--resolution` is required and `--of` is required
  exactly for `duplicate`/`superseded`.
- One shared outcome, actor and reason applied to every id via
  `Store.Apply(Change{Action: model.Close{Outcome}, Actor, Reason})`
  (`bulk.go:154-161`).
- **No workflow events, no close-adjacency block, no breadcrumb** on the bulk path.

**`lit bulk archive --ids <csv> [--reason <text>]`** (`runBulkTransition(model.Archive{})`,
`bulk.go:167-186`, registered at `bulk.go:20`):
- Flag set name is `"bulk archive"` derived from `action.Name()` (`bulk.go:169`).
- Flags: `--ids`, `--reason`, hidden `--by`.
- Empty `--ids` → `ValidationError{"--ids is required"}` → exit 3 (`bulk.go:176-179`).
- Applies `model.Archive{}` per id.

**`lit bulk import`** (hidden, retired) →
`RetiredCommandError{Command: "bulk import", Replacement: bulkImportRetirementGuidance}`
→ exit 3 (`bulk.go:192-194`). Guidance verbatim:
"use `lit backup restore --path <export.json>` — it owns the same export-restore
mechanism `bulk import` duplicated" (`register.go:441`). Because the row is
`skipApp`, the pointer is returned even outside a git repository
(`register.go:209-214`; asserted in `retired_command_test.go:97-…`).

### 2.22 `lit export`

- Registration `register.go:358-359`, `app.AccessRead`. Summary: "Write the backlog
  out as a portable JSON tree (the data-export primitive; `import`'s inverse)".
- Handler `runExport` (`cli.go:1514-1525`): no flags of its own; parses argv (so
  `--help` works and any flag is an unknown-flag `UsageError`); calls
  `Store.Export(ctx)`; writes the result as **two-space-indented JSON** to stdout
  (`cli.go:1524`, `writeJSON` at `cli.go:1800-1804`).
- No positional check.

### 2.23 `lit import --path <file>`

- Registration `register.go:360-361`, `app.AccessWrite`. Summary: "Bulk-create/update
  issues from a file (the one bulk-ingest home): a JSON tree spec, or a YAML file
  for create-or-update by id selector".
- Handler `runImportTree` (`cli.go:1537-1570`).
- Flags: `--path` (string, `""`, "Path to a JSON tree-spec file or a YAML bulk
  create/update file"), hidden `--by` (`cli.go:1539-1540`).
- Refusals: blank `--path` (after trim) or `fs.NArg() != 0` →
  `UsageError{importUsage}` → exit 2. `importUsage` verbatim:
  `"usage: lit import --path <tree-spec.json | bulk-file.yaml> (see docs/cli-reference.md for both formats)"`
  (`cli.go:1529`, `cli.go:1544-1549`).
- Reads the file; a read error → `fmt.Errorf("read import spec: %w", err)` → exit 1
  (`cli.go:1550-1553`).
- **Format is selected by the file extension** (lowercased) (`cli.go:1554`):
  - `.yaml` / `.yml` → `runImportBulk`
  - anything else → `runImportTreeJSON`, but first: if `--by` was set →
    `UsageError{"usage: --by only applies to a YAML bulk-update file (--path *.yaml|*.yml); JSON tree-spec import always attributes creates to \"links\""}`
    → exit 2 (`cli.go:1565-1567`).

**JSON tree path** (`runImportTreeJSON`, `cli.go:1587-1605`):
- `storage.ParseImportTreeSpecs(data)` then
  `Store.ImportTree(ctx, workspacePrefix, specs)`.
- Documented spec shape (`cli.go:1580-1586`): an array of records each with
  `local_id`, optional `parent` (a local_id), optional `depends_on` (array of
  local_ids), `title`, `type`, `topic`, `priority`.
- Output: `"imported %d issues\n"` then, per mapping, `"  <local> -> <real>\n"`
  (map iteration order is unspecified) (`cli.go:1596-1603`).
- Best-effort rollback on failure is the store's behavior; the doc comment tells
  the caller to run `lit doctor` after a failed import (`cli.go:1572-1578`).

**YAML bulk path** (`runImportBulk`, `cli.go:1626-1661`):
- `storage.ParseBulkSpecs(data)`.
- If `--by` was set but no document has an `id` (i.e. no update documents) →
  `UsageError{"usage: --by only applies when the file has at least one update document (a document with \`id\` set); this file has none"}`
  → exit 2 (`cli.go:1637-1639`, `bulkSpecsHaveUpdate` at `cli.go:1665-1672`).
- Calls `Store.BulkApply(ctx, prefix, actor, specs)`.
- Documented YAML shape (`cli.go:1611-1625`): one document per issue separated by
  `---`; optional `local_id` for intra-file references; `id` present means
  **update** that issue instead of creating; `parent` may name a local_id or a
  real issue ID.
- Output (`cli.go:1644-1660`):
```
created <n> issues
  <ref> -> <realID>
  ...
updated <n> issues
  <id>
  ...
```

### 2.24 `lit prefix set <new-prefix> [--apply]`

- Registration `register.go:375-378`, workspace-mode (no store). Group
  `maintenance`. Summary: "Manage the cosmetic issue ID prefix".
- `runPrefix` (`prefix.go:21-26`): any invocation whose `args[0]` is not the
  literal `set` (including no args) →
  `UsageError{"usage: lit prefix set <new-prefix> [--apply]"}` → exit 2.
- `runPrefixSet` (`prefix.go:28-81`): one positional, flag `--apply` (bool, false,
  "Apply the rename (without this flag, prints a preview)").
  - `len(positional) != 1 || fs.NArg() != 0` → the same usage `UsageError`
    (`prefix.go:35-37`).
  - `workspace.ConfiguredPrefix(requested)` failure →
    `fmt.Errorf("invalid prefix %q: %w", requested, err)` → exit 1
    (`prefix.go:41-44`).
- Three outcomes (`prefixSetTextOutput`, `prefix.go:83-102`):
  - Normalized == current → `"issue_prefix: <p> (prefix unchanged)\n"`
    (`prefix.go:49-56`, `:88-91`).
  - Changed, no `--apply` →
    ```
    issue_prefix: <old> -> <new> (preview)
      preview only — pass --apply to write config.json. Existing issue IDs keep their old prefix; only new issues use the new one.
      Run with --apply to write config.json.
    ```
    (`prefix.go:58-66`, `:92-101`).
  - Changed with `--apply` → `workspace.UpdateConfig` writes `IssuePrefix`; a
    failure → `fmt.Errorf("update workspace config: %w", err)`; success prints
    `"issue_prefix: <old> -> <new> (applied)\n"` (`prefix.go:68-80`, `:84-87`).

### 2.25 `lit workspace`

- Registration `register.go:362-365`, workspace-mode. Summary: "Show workspace
  metadata". Handler `runWorkspace` (`cli.go:1674-1697`).
- No flags of its own; parses argv so `--help` works.
- Output: one `key: value` line per field, in this exact order
  (`cli.go:1682-1695`): `workspace_id`, `issue_prefix`, `git_common_dir`,
  `storage_dir`, `database_path`, `dolt_repo_path`, `traces_dir`.

### 2.26 `lit completion <bash|zsh|fish>`

- Registration `register.go:280-281`. Group `guidance`. Summary: "Generate shell
  completion script". Its own advertised subcommands come from
  `completionFamily.visibleSubcommands()`.
- `completionFamily` — usage `"usage: lit completion <bash|zsh|fish>"`, rows
  `bash`, `zsh`, `fish` (`cli.go:1706-1713`).
- `runCompletion(stdout, args)` (`cli.go:1715-1725`): `len(args) != 1` →
  `errors.New(completionFamily.usage)` → exit 1; an unknown shell → the same
  usage error via `resolve`; otherwise writes the generated script to stdout.
- `completionRenderer(shell)` panics for any shell not in the switch
  (`completion.go:45-55`).

**Completion model** (`commandCompletionModel`, `completion.go:17-35`): projects
`commandSpecs`, drops every `Hidden` spec, and appends a synthetic
`{Name: "help", Summary: "Help about any command"}` row. Retired commands never
appear in completion.

`familyNodes` flattens every command/subcommand that has children into
(trigger-word → children) pairs at any depth, de-duplicating by name and
**unioning** children when a word appears under two parents (e.g. `label` as both
a top-level command and a `bulk` subcommand) (`completion.go:87-131`).

- **bash** (`completion.go:133-153`): defines `_lit_completions` using
  `_init_completion`, a `commands` variable holding the top-level names, a
  `case "${prev}"` with a `lit)` arm and one arm per family node, and a fallback
  `compgen -W "${commands}"`. Ends with `complete -F _lit_completions lit`.
- **zsh** (`completion.go:155-179`): `#compdef lit`, a `commands` array of
  `'name:summary'` entries (single quotes in the summary escaped as `'\''`,
  `completion.go:184-186`), `_arguments '1:command:->command' '2:subcommand:->subcommand'`,
  a `_describe` for commands and a per-command `_values` arm for each command that
  has subcommands (only **top-level** commands' direct subcommands, not the
  flattened nodes).
- **fish** (`completion.go:188-197`): `complete -c lit -f`, one
  `__fish_use_subcommand` line with the top-level names, and one
  `__fish_seen_subcommand_from <name>` line per family node.

Subcommand trees fed into the registry (`register.go:269-271`): `sync` nests
`remote` and `reconcile`; `bulk` nests `label`. Explicit literals: `workflows`
declares `show`, `edit`, `dry-run` (`register.go:279`).

### 2.27 Retired commands (hidden, dispatchable)

Registered with `Hidden: true` and a `retiredCommandRun(command, replacement)`
handler that runs nothing and returns `RetiredCommandError` → exit 3
(`register.go:449-453`). Hidden specs are excluded from root `--help` and from
completion (`register.go:415-421`, `completion.go:20-29`).

| Command | Group | Replacement guidance (verbatim) | Citation |
|---|---|---|---|
| `ready` | operations | "use `lit backlog` for the full ranked queue (blocked items shown inline) or `lit next` for the single leaf to start" | `register.go:296-297`, `:431` |
| `queue` | operations | same as `ready` | `register.go:300-301` |
| `assign` | operations | "reassigning is a field write: use `lit update <id> --assignee <name>` (with an optional `--reason`)" | `register.go:325-326`, `:438` |
| `ls-at` | maintenance | "use `lit ls --at <store-dir>` — listing a discovered store read-only is now a flag on `ls`, not a separate command" | `register.go:371-372`, `:439` |
| `overview` | maintenance | "use `lit stores --counts` — the cross-project ready / in-flight / blocked rollup is now a flag on `stores`" | `register.go:373-374`, `:440` |
| `bulk import` | (bulk family) | "use `lit backup restore --path <export.json>` — it owns the same export-restore mechanism `bulk import` duplicated" | `bulk.go:28`, `register.go:441` |

Full error message form: `the "<command>" command has been retired; <replacement>`
(`cli.go:1947-1949`). Reason `retired_command`, remediation empty
(`error_output.go:54-57`, `:96-100`). Asserted in
`retired_command_test.go:17-52`, `:54-96`, `:134-…`.

Retired **flags** (intercepted by the shared parser, §1.6): `--output` anywhere
(`cli.go:174-179`, `cli.go:286-289`) and `--continue` (`cli.go:290-294`), both
`UnsupportedError`; `lit update --status` (`cli.go:958-960`), a `UsageError`.

### 2.28 `lit quickstart` (in-scope only as it is the bare-`lit` default)

`runQuickstart` (`cli.go:1727-1798`) — flags `--refresh` (bool),
`--eject` (string-optional; present-with-no-value = `"all"`), `--force` (bool),
plus at most one positional topic.
- `fs.NArg() > 1` → `UsageError{quickstartUsage}` (`cli.go:1736-1738`), where
  `quickstartUsage = "usage: lit quickstart [<topics|…>] [--refresh] [--eject[=LIST]] [--force]"`
  built from the topic token list (`quickstart_topics.go:55`).
- `--refresh` with `--eject` → `UsageError{"usage: --refresh and --eject are mutually exclusive"}`
  (`cli.go:1744-1746`).
- `--force` without `--eject` → `UsageError{"usage: --force is only valid with --eject"}`
  (`cli.go:1747-1749`).
- A topic positional combined with any flag →
  `UsageError{"usage: lit quickstart <topic> takes no flags"}` (`cli.go:1752-1755`).
- An unknown topic →
  `UsageError{"usage: unknown quickstart topic \"<x>\" (must be one of: <tokens>)"}`
  (`cli.go:1756-1759`).

---

## PART 3 — CROSS-CUTTING OBSERVATIONS (behavioral, non-editorial)

1. **JSON output exists on exactly one command in this scope**: `lit export`
   (`cli.go:1524`). Every other command emits line-oriented text. `--output` is
   rejected globally and per-command (§1.1, §1.6).
2. **`fs.NArg()` is not checked** by: `new` (`cli.go:340-355`),
   `followup` (`cli.go:387-415`), `ls` (`cli.go:525-621`), `rank`
   (`cli.go:1049-1073`), `export` (`cli.go:1516`), `children`
   (`issue_relations.go:127-132`), `parent clear` (`issue_relations.go:109-114`).
   Extra positionals on those commands are silently ignored.
3. **Family dispatch errors are plain errors (exit 1), not `UsageError` (exit 2)**
   (`register.go:112-123`), unlike the per-command usage refusals which are
   `UsageError` (exit 2). Likewise `runTransition`'s wrong-arity refusal
   (`cli.go:1365`) and `runCompletion`'s (`cli.go:1717`) are exit 1.
4. **`--help` output goes to stdout, not stderr**, and exits 0
   (`cli.go:265-272`, `cli.go:278-283`, `cli.go:47-49`).
5. **Assignee identity diverges by command on purpose**: `start` resolves through
   `resolveIdentity` (env `CLAUDE_CODE_SESSION_ID` wins) (`cli.go:1276`);
   `update --assignee` writes the trimmed literal, empty meaning clear
   (`cli.go:1000-1010`). `new`/`followup` also write the trimmed literal
   (`cli.go:352`, `cli.go:423`).
6. **Claim state never blocks anything except `lit start` on a fresh foreign
   hold.** `backlog` renders claims as visibility only (`backlog.go:24-25`,
   `:92-96`); `next` routes by claim but never writes (`next_route.go:81-128`);
   `start` is the only gate (`cli.go:1279-1284`, `claims_takeover.go:65-87`).
7. **Three functions panic on unreachable states** and would abort the process:
   `ClassifyReadiness` on an unclassified annotation kind (`readiness.go:86`),
   `renderNextOutcome` on an unhandled outcome type (`next.go:99`),
   `transitionOccasion` on an unmapped status action (`workflow_events.go:106`),
   `emitBreadcrumb`/`quickstartBreadcrumb` on an unknown topic
   (`quickstart_topics.go:64`), `completionRenderer` on an unknown shell
   (`completion.go:54`), `nestUnder` on a missing nest point (`register.go:153`),
   and `store.planLifecycleAction` on an impostor action
   (`internal/store/store.go:1236`).
8. **The `done` "requires in_progress" claim in the registry summary
   (`register.go:327`) has no enforcing code path** — see §2.10.
