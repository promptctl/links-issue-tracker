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
  `UnsupportedError{Message: "--output is no longer supported; omit it for text output"}`
  (`cli.go:210-212`).

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
  (`register.go:254-408`). Each `CommandSpec` carries `Name`, `Summary`, `Long`,
  `GroupID`, `Run`, `Subcommands`, `Hidden` (`register.go:18-38`).
- `applyRegistry` adds every group then every command (`register.go:412-419`).
- `buildPassthroughCommand` (`register.go:423-440`) creates each cobra command with
  `DisableFlagParsing: true` and `Args: cobra.ArbitraryArgs`. **Consequence:** cobra
  does not parse any per-command flags; each handler parses its own argv slice.
  `CommandSpec` carries no `Long`: because cobra parses no flags here, it cannot
  render a command's page either, so `lit help <cmd>` is rewritten in argv to
  `lit <cmd> --help` (`rewriteHelpCommand`, `cli.go`) and the leaf renders both the
  description and the real flag table. Descriptions are embedded under
  `internal/cli/helptext/`; `init`'s is `helptext/init.txt`.
- Help groups, in order (`register.go:61-76`):
  `bootstrap` "Human Bootstrap", `operations` "Agent Operations",
  `structure` "Dependencies & Structure", `data` "Sync & Data",
  `maintenance` "Setup & Maintenance", `retention` "Issue Retention",
  `guidance` "Guidance & Tooling".

### 1.4 Wrappers: how a handler gets a store

- `r.appCmd(access, fn)` → `runWithApp` with a fixed access mode
  (`register.go:187-189`); `r.appCmdDynamic` computes the mode from argv
  (`register.go:191-197`).
- `r.wsCmd(fn)` → `acquireFromWD` (workspace metadata only, no store)
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
    `sync_staleness.go:229`).
  - Then `maybeAutoSyncAfterCommand(ctx, accessMode, ws)` runs (`cli.go:145`).
  - Both only run when the command returned nil (`cli.go:123-125`).
- `acquireFromWD` / `resolveWorkspaceFromWD` (`register.go:448-450`, `cli.go:170-184`):
  same `OutsideWorkspaceError` translation (`cli.go:177-180`).

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
- `parseFlagSet(fs, args, stdout)` (`flagset.go:134-168`) is the single parse boundary:
  - On `pflag.ErrHelp` it prints `"Usage of <use>:\n"` followed by
    `PrintDefaults()` **to stdout** and returns `errHelpHandled` → exit 0
    (`cli.go:277-283`, printer at `cli.go:265-272`).
  - A `*pflag.NotExistError` whose parsed name is `continue` (from `--continue`
    or `--continue=<x>`; not `--continuex`, and not `-continue`, which pflag
    reads as the shorthand group `-c…`) →
    `UnsupportedError{Message: "--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag"}`
    → exit 3 (`flagset.go:138-141`).
  - Every other parse error — unknown flag, missing value, invalid value, bad
    syntax — → `UsageError{Message: err.Error()}` → exit 2 (`flagset.go:142`).
    A leaf-position `--output` is an ordinary unknown flag here; only
    `parseGlobalArgs` maps `--output` to `UnsupportedError`.
  - A parsed-and-changed `--help` flag also prints help and returns
    `errHelpHandled` (`cli.go:300-306`).

### 1.7 Positional/flag splitting (`splitArgs`)

`splitArgs(args []string, positionalCount int, fs *cobraFlagSet)`
(`flagset.go:297-363`):
- Any token starting with `-` goes to the flag slice; it consumes the *next*
  token as its value only when the flag set says that flag takes one —
  `flagTakesValue` (`flagset.go:201`) resolves the token against the command's
  flags and applies `takesValue` (`flagset.go:239`), which is pflag's own rule:
  an empty `NoOptDefVal` means the flag consumes the following token whatever
  that token looks like.
- A `--` terminator ends flag scanning: every token after it is a positional
  whatever it looks like, up to `positionalCount`; tokens past that ceiling stay
  in the flag stream and are refused as surplus.
- The first `positionalCount` non-flag tokens become positionals; any extra
  non-flag tokens are appended to the **flag** slice, where
  `refuseSurplusPositionals` (`register.go:342`) refuses them.

### 1.8 Exit-code taxonomy

Constants (`exit.go:12-33`):

| Name | Value |
|---|---|
| `ExitOK` | 0 |
| `ExitGeneric` | 1 |
| `ExitUsage` | 2 |
| `ExitValidation` | 3 |
| `ExitNotFound` | 4 |
| `ExitConflict` | 5 |
| `ExitNoWork` | 6 |
| `ExitCorruption` | 7 |

`ExitCode(err)` dispatches by `errors.As`, in this order (`exit.go:38-170`):
1. `storage.NotFoundError` → 4 (`exit.go:42-44`)
2. `MergeConflictError` → 5 (`exit.go:46-48`)
3. `SyncFailureError` → 5 (`exit.go:54-56`)
4. `templateShapeError` → 3 (`exit.go:62-64`)
5. `ownerApprovalRefusalError` → 5 (`exit.go:69-71`)
6. `CorruptionError` → 7 (`exit.go:73-75`)
7. `UsageError` → 2 (`exit.go:77-79`)
8. `UnknownCommandError` → 3 (`exit.go:81-83`)
9. `RetiredCommandError` → 3 (`exit.go:87-89`)
10. `ValidationError` → 3 (`exit.go:91-93`)
11. `storage.ValidationError` → 3 (`exit.go:95-97`)
12. `model.ContainerActionError` → 6 when `Satisfied()`, else 3 (`exit.go:107-113`)
13. `UnsupportedError` → 3 (`exit.go:114-116`)
14. `Exhausted` → 6 (`exit.go:122-124`)
15. `NoWork` → 6 (`exit.go:126-128`)
16. `OutsideWorkspaceError` → 3 (`exit.go:142-144`)
17. `errors.Is(err, store.ErrWorkspaceNotInitialized)` → 3 (`exit.go:146-148`)
18. `errors.Is(err, workspace.ErrIssuePrefixRefused)` → 3 (`exit.go:156-158`)
19. `BulkFailureError` → 1 (`exit.go:159-164`)
20. `errors.Is(err, store.ErrTransientGCContention)` → 1 (`exit.go:166-168`)
21. anything else → 1 (`exit.go:159`)

Error types defined in `cli.go`: `MergeConflictError` (`cli.go:1920-1926`),
`CorruptionError` (`cli.go:1928-1932`), `UsageError` (`cli.go:1936-1940`),
`UnknownCommandError` — message `unknown command "<x>"` (`cli.go:1943-1947`),
`ValidationError` (`cli.go:1950-1954`), `UnsupportedError` with a single `Message`
field (`errors.go:52-56`), `RetiredCommandError` — message
`the "<cmd>" command has been retired; <replacement>` (`cli.go:1969-1976`),
`OutsideWorkspaceError` (`cli.go:1982-1986`). `BulkFailureError` in
`bulk.go:48-58`.

### 1.9 Error output convention

`WriteCommandError(stderr, err)` (`error_output.go:18-25`) — called from
`cmd/lit/main.go:20` as the process exit path:
- Line 1: `error (code=%d): %v\n` (exit code + `err.Error()`).
- Line 2 (only when non-empty): `remediation: %s\n`.

`commandErrorReason(err)` maps type → reason string (`error_output.go:30-91`):
`entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`,
`corruption_detected`, `unknown_command`, `retired_command`, `usage_error`,
`unsupported_flag` (every `UnsupportedError`, `error_output.go:122-126`),
`outside_git_workspace`, `bulk_partial_failure`, `workspace_write_blocked`,
`transient_gc_contention`, `workspace_not_initialized`, default `command_failed`.

`commandErrorRemediation(reason)` (`error_output.go:92-133`), verbatim strings:
- `unknown_command`: "Run `lit --help` (or `lit help <command>`) to select a supported command path."
- `retired_command`: "" (empty — message is self-contained)
- `usage_error`: "Run the command with `--help` and retry with valid arguments."
- `unsupported_flag`: "Do not retry unchanged — this flag is refused on every run. Drop it and use what the message above names instead."
- `entity_not_found`: "Verify the target ID exists with `lit ls` or `lit show <id>`."
- `merge_conflict`: "Sync and retry after resolving conflicts."
- `sync_divergence`: ""
- `owner_approval_required`: ""
- `corruption_detected`: "Run `lit doctor --fix integrity` and retry. \<agent-instructions>This command is idempotent and safe to run without confirmation.\</agent-instructions>"
- `transient_gc_contention`: "Retry once. If the error persists, run `lit doctor --fix`. \<agent-instructions>…\</agent-instructions>"
- `workspace_write_blocked`: "Wait a moment and retry — a normal command releases the store in well under a second. If it persists, a lit process is stuck: find it with `ps aux | grep '[l]it'` and terminate it, then retry; if none is running the hold is stale, so run `lit doctor --fix`. \<agent-instructions>…\</agent-instructions>"
- `outside_git_workspace`: "Run the command inside a git repository/worktree with links initialized."
- `workspace_not_initialized`: "Do not retry unchanged — this repository has no lit workspace, and retrying this command cannot create one. Run `lit init` here to create it, or change to a directory that already has one."
- `bulk_partial_failure`: "Some items failed; see the per-item errors above. Re-run the command for only the failed IDs after addressing each error."
- default: "Retry the command. If it still fails, run `lit doctor` for diagnostics."

### 1.10 Progress channel

`progressf(operation, format, args...)` writes `lit: <operation>: <text>\n` to
`progressOut`, which is `os.Stderr` (`progress.go:15`, `progress.go:25-27`).
Stdout stays the result channel.

### 1.11 Identity resolution (assignee / actor)

- `resolveIdentity(explicit)` (`cli.go:1202-1207`): if env
  `CLAUDE_CODE_SESSION_ID` is non-empty (after trim), the identity is
  `"claude_" + sessionID`, **overriding any explicit value**; otherwise the
  trimmed explicit value.
- `registerActor(fs)` (`cli.go:1232-1236`) declares a **hidden** `--by` string
  flag (default `""`, empty usage string) and returns a resolver closure. The raw
  flag value is never exposed; `--by` does not appear in help output
  (`cli.go:1233-1234`).
- Empty actor is normalized by the store to `"unknown"`
  (`internal/store/store.go:1102-1105`).
- `displayAssignee("")` renders `"(unassigned)"` (`cli.go:1240-1245`).

Commands that register `--by`: `update` (`cli.go:969`), every transition via
`runTransition` (`cli.go:1386`), `comment add` (`cli.go:1498`), `import`
(`cli.go:1570`), `dep add` (`dependency.go:28`), `label add`
(`issue_relations.go:31`), `parent set` (`issue_relations.go:77`), `bulk label`
(`bulk.go:108`), `bulk close` (`bulk.go:141`), `bulk archive` (`bulk.go:172`).
`next` registers it too (`next.go:60`) and is the only read command that does:
it writes nothing, but its output names an identity, so it resolves the reader
by the same rule every writer resolves the actor.

### 1.12 Success-output breadcrumb

`emitBreadcrumb(w, token)` prints `deeper guidance: lit quickstart <token>`
(`quickstart_topics.go:62-75`). It **panics** if the token is not a registered
quickstart topic (`quickstart_topics.go:63-65`).

Breadcrumbs are emitted by: `new` → `"new"` (`cli.go:365`), `followup` → `"new"`
(`cli.go:437`), `update` → `"update"` (`cli.go:1062`), `rank` → `"update"`
(`cli.go:1143`), `rank set` → `"update"` (`cli.go:1178`), `dep add`/`dep rm` →
`"update"` (`dependency.go:65`, `dependency.go:90`), `label add`/`label rm` →
`"update"` (`issue_relations.go:48`, `issue_relations.go:70`), `parent set`/
`parent clear` → `"update"` (`issue_relations.go:103`, `issue_relations.go:121`),
and transitions via the table below.

`transitionBreadcrumbTopics` (`cli.go:1253-1257`): `start` → `"work"`,
`done` → `"done"`, `close` → `"done"`. Absent for `open`, `archive`,
`unarchive`, `delete`, `restore` → no breadcrumb (`cli.go:1474-1477`).

### 1.13 Sync-staleness banners

- Read commands print `printStalenessWarning(ctx, w, ws, store, now)` FIRST,
  before their payload: `backlog` (`workable.go:187`), `next` (`next.go:72`),
  `show` **only in full-detail mode** (`cli.go:945-949`) — deliberately suppressed
  under `--field` so the machine-parseable output isn't corrupted.
  Defined at `sync_staleness.go:191`.
- Write commands get `printMutationSyncStalenessWarning(stdout, ws, now)` after
  the handler succeeds and after the engine closes (`cli.go:135-137`,
  `sync_staleness.go:229`).

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
fire no event (`cli.go:1438-1442`).

### 1.15 Prefix / ID resolution

There is **no fuzzy or short-prefix ID resolution anywhere in `internal/cli`.**
Issue IDs are passed verbatim to the store (e.g. `cli.go:900`, `cli.go:1402`,
`cli.go:1052`). A wrong ID yields `storage.NotFoundError` → exit 4.

The word "prefix" in this codebase means the *cosmetic ID prefix* on new IDs
(`lit prefix`, §2.24) — `ap.Workspace.IssuePrefix.Value()` is passed into
`CreateIssue` (`cli.go:354`, `cli.go:426`) and `ImportTree`/`BulkApply`
(`cli.go:1622`, `cli.go:1670`).

The only "does the literal look like a subcommand" disambiguation is in `rank`:
`args[0] == "set"` routes to `rank set`, justified because real IDs always carry a
prefix (`cli.go:1066-1072`).

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
- `writeJSON(w, v)` uses `json.Encoder` with two-space indent (`cli.go:1830-1834`).
  **`export` is the only command in this scope that emits JSON** (`cli.go:1554`).
  There is no `--json` / `--output` mode anywhere; `--output` is explicitly
  rejected (§1.1, §1.6).

### 1.17 Vocabularies (sealed sets used by flags)

- Issue types: `task, feature, bug, chore, epic` (`internal/model/issue_type.go:31-33`).
  `ParseIssueType` lowercases and trims; error text
  `"issue type must be <oxford-or list>"` (`issue_type.go:35`, `:42-50`).
  `epic` is the only container type (`issue_type.go:56-58`).
- Priorities: `0` normal, `1` urgent (`internal/model/priority.go:18-21`), both
  spellings held in one `priorityVocabulary` table (`priority.go:35-41`).
  `ParsePriority` gates the int payloads (`priority.go:95-101`) and
  `ParsePriorityName` gates the `--priority` flag (`:112-120`), the latter
  lowercasing and trimming, then taking the display word or the decimal. Both
  reject anything else with the shared
  `"priority must be normal (0) or urgent (1)"` (`priority.go:87`).
- States: `open, in_progress, closed`; `in-progress` is normalized to
  `in_progress`; error `invalid status "<x>" (valid: open, in_progress, closed)`
  (`internal/model/lifecycle/lifecycle.go:143-154`).
- Resolutions: `duplicate, superseded, obsolete, wontfix`; error
  `"resolution must be one of: duplicate, superseded, obsolete, wontfix"`
  (`internal/model/lifecycle/resolution.go:47-54`).
- Relation types: `blocks, parent-child, related-to`; error
  `"relation type must be blocks, parent-child, or related-to"`
  (`internal/model/relation_type.go:16-33`).
- CLI parse-boundary wrappers: `parseIssueTypeFlag` (`cli.go:1969-1975`) and
  `parsePriorityFlag` (`:1990-1996`) wrap failures in `ValidationError` → exit 3.
  `parsePriorityFlag` takes the raw **string**: the `ValidationError` is also what
  routes the refusal to the `validation_refused` remediation, which a bare pflag
  `ParseInt` error missed — it fell through to the default "Retry the command" on
  a refusal no retry can change (links-cli-bvko).
  `parseIssueTypeSlice` (read path `--type`) returns the bare model error
  (`cli.go:1954-1963`); the read path `--status` returns the bare model error
  from `model.ParseStates` (`internal/model/lifecycle/lifecycle.go`).
- `issueTypeChoices()` renders `task|feature|bug|chore|epic` into flag help
  (`cli.go:2013-2020`), and `priorityChoices()` renders `normal|urgent` the same
  way (`:2001-2008`) — into both the `--priority` help string and the `followup`
  and `update` usage lines, so neither can drift from the parse gate.
- `splitCSV` splits on `,`, trims each part, drops empties, returns nil for a
  blank input (`cli.go:1905-1918`).

### 1.18 Readiness / workability — the exact predicate

**Step 1 — candidate set** (`classifyWorkable`, `cli.go:652-682`):
`ListIssues` with `Statuses = [open, in_progress]` (or the single `--status`
value if given), `IssueTypes`/`Assignees`/`LabelsAll` from the CLI filter,
`IncludeArchived=false`, `IncludeDeleted=false`, `Limit=0` (`cli.go:659-667`).
The store's default ordering is `item_rank ASC` (`cli.go:719-720`).

**Step 2 — leaves only**: `filterWorkableIssues` keeps issues whose
`Capabilities().Status != nil` (i.e. leaves, not containers) and whose status is
not `closed` (`cli.go:1181-1190`). Epics are therefore never workable rows.

**Step 3 — annotators** applied via `annotation.Annotate` (`cli.go:786-793`):
1. `newFieldAnnotator(requiredFields)` — from `config.Load(...).Ready.RequiredFields`
   (`cli.go:694-704`). Empty policy → no-op annotator (`ready_state.go:63-67`).
   A required field name not present in `model.IssueWireFields()` →
   `ValidationError{"required field %q does not exist on issue"}`
   (`ready_state.go:68-73`). For each unset required field it emits a
   `MissingField` annotation (`ready_state.go:74-89`). "Set" means: non-nil, and
   for strings non-blank, for arrays/maps non-empty; anything else counts as set
   (`isRequiredFieldSet`, `ready_state.go:749-762`).
2. `newBlockerAnnotator(details, ancestry)` — for each `DependsOn` that is
   `InPlay()`, sorted by ID, emits `OpenDependency{Message: dep.ID}`; and
   additionally `RankInversion{Message: dep.ID}` when `dep.Rank > issue.Rank`.
   Then, for each id from `ancestry.inheritedDependencies(detail)` not already
   a direct dependency, emits `InheritedDependency{Message: dep.ID}` and no rank
   inversion (`ready_state.go:122-189`). `ancestry` is a `heldAncestry`
   (`:284-296`), built by `fetchHeldAncestry` (`:303-319`) from an
   `epicAncestry`: the relations of every epic above the issues, keyed by epic
   id, and each epic's gates (`:195-202`). `fetchContainerAncestry` builds both,
   a gate being any `InPlay()` `DependsOn` of a loaded epic (`:206-220`).
   `climbContainers` loads `parentEpicIDs` of the subjects, then of each loaded
   level, one fetch per level, until a level names no epic it has not loaded
   (`:228-249`). `epicsAbove` yields the container parents of an issue, nearest
   first, and stops at a parent it has already yielded (`:254-264`).
   `epicAncestry.inheritedDependencies` returns the gates of every epic
   `epicsAbove` the subject yields, other than the subject itself, deduplicated,
   sorted by ID (`:269-282`).
   `fetchHeldAncestry` then loads the wait graph once with `fetchWaitGraph`: a
   breadth-first walk of `fetchWaitLinks` from every gate over links that hold,
   keyed by waiter (`:331-358`). `settleWaits` computes, in memory, the ids
   each gate waits on, and each blocker the graph's inherited links name
   (`:374-420`). An inherited link holds unless its blocker waits on its waiter,
   so the closures are the alternating fixpoint: `upper` starts as the closure
   over every link; `lower` is the closure over the links `upper` lets hold, the
   next `upper` the closure over the links `lower` lets hold, until `upper` is
   unchanged, which is returned. `heldAncestry.inheritedDependencies` drops each
   gate whose closure contains the subject (`:323-327`), so a blocker never
   holds back an issue it waits on, itself included.
3. `newSiblingGateAnnotator(details, pendingSiblingsByEpic(held.ancestry.relations))` —
   only when the parent exists and `parent.IsContainer()`; emits
   `EarlierSiblingPending{Message: sib.ID}` for each sibling satisfying
   `isEarlierSameLaneSibling` (`ready_state.go:431-448`).
   `isEarlierSameLaneSibling(sib, leaf) := sib.ID != leaf.ID && sib.Lane == leaf.Lane && sib.Rank < leaf.Rank`
   (`ready_state.go:456-458`). The sibling set is the epic's **unfiltered**
   `InPlay()` children (`pendingSiblingsByEpic`, `ready_state.go:486-496`), fetched
   via `fetchHeldAncestry(ctx, memo, details)` (`cli.go:889`), so siblings
   hidden by `--assignee/--type/--labels` still gate.
4. `newOrphanedAnnotator(orphanedThreshold)` — only for `in_progress` issues with
   `time.Since(UpdatedAt) >= 6h`; message
   `"in_progress for <dur truncated to minute> with no update"`
   (`ready_state.go:707-721`; threshold constant `orphanedThreshold = 6 * time.Hour`
   at `ready_state.go:51`).
5. `newNeedsDesignAnnotator()` — emits `NeedsDesign` for any issue carrying the
   label `needs-design` (`ready_state.go:25`, `:32-44`).
6. `newFocusPathAnnotator(focusPaths)` — emits `FocusPath{Message: goalID}` for
   issues on a focused goal's prerequisite closure (`ready_state.go:731-750`).

**Focus path derivation** (`fetchFocusPathGoals`, `ready_state.go:581-615`):
goals are issues with `Statuses=[open,in_progress]` and label `focus`
(`FocusLabel = "focus"`, `ready_state.go:550`; query at `:582-585`). BFS over the
prerequisite DAG, one `fetchWaitLinks` expansion per level (`:596-613`). The
`path` map doubles as the visited set, so shared prerequisites attribute to the
first goal reached and cycles terminate. Relations are memoized through
`memoizeRelations` (`:689-697`) over `relationsByID` (`:706-729`), primed with the
seeds; `annotateIssues` passes the memo it built for `fetchHeldAncestry`.

**Wait links** (`fetchWaitLinks`, `ready_state.go:638-684`): for each frontier
issue, in frontier order, a `waitLink{waiter, prereq, holds, inherited}` (`:622-628`) for
each `InPlay()` `DependsOn`, each `epicAncestry.inheritedDependencies` entry over
the frontier's `fetchContainerAncestry` (every gate, before `heldAncestry` drops
any; `inherited` set), each `InPlay()` child of a container, and each earlier same-lane
`InPlay()` sibling under a container parent. `holds` is true for a child link,
and for any other link only when the waiter is not a container. A frontier id
missing from the fetch → `storage.NotFoundError` (`:654`). The focus walk
follows every link; `fetchWaitGraph` keeps only links that hold.

**Step 4 — readiness classification** (`ClassifyReadiness`, `readiness.go:131-148`):
each annotation is dispatched on its declared `ReadinessRole`:
- `RoleBlocking` → appended to `blocking`
- `RoleOrphaned` → sets `orphaned = true`
- `RoleRankInversion` → appended to `rankInversions`
- `RoleNone` → contributes nothing (this is where `FocusPath` lands)
- anything else → **panics** with
  `"ClassifyReadiness: annotation carries an unclassified kind: <kind>"`
  (`readiness.go:143-145`).

`IsReady() := len(blocking) == 0` (`readiness.go:69`). So an issue is **ready**
iff it has no `MissingField`, no `OpenDependency`, no `InheritedDependency`, no
`EarlierSiblingPending`, and no `NeedsDesign` annotation. `DependencyIDs()`
returns the details of the `OpenDependency` and `InheritedDependency` reasons,
and `DependencyLabels()` the same list with ` (via epic)` appended to each
inherited one, which the backlog and `lit next` print on their `depends on:`
lines (`readiness.go:80-121`).

**Step 5 — canonical ordering**, applied in this sequence (`cli.go:677-679`):
1. `sortByCompositeRank(rows, details)` — stable sort by
   (effective epic rank, own rank); a leaf whose parent is a container uses the
   parent's rank as its epic-position, otherwise its own rank
   (`ready_state.go:792-807`).
2. `sortByPriority` — stable, urgent (higher `Priority`) first
   (`ready_state.go:813-817`).
3. `sortByFocusPath` — stable, rows carrying a `FocusPath` annotation first;
   layered last so focus outranks urgent (`ready_state.go:832-839`).
Then `enrichWithParentEpic` sets `ParentEpic{ID,Title}` on rows whose parent is a
container (`ready_state.go:770-781`).

**Partition used by rollups**: `partitionWorkable` (`ready_state.go:884-897`):
`in_progress` state wins first (even if also blocked); else not-ready → blocked;
else ready.

`applyLimit(issues, limit)` truncates when `limit > 0` (`ready_state.go:841-846`).

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
| `--priority` | string | `normal` | "Priority: normal\|urgent", the choice list derived from `model.Priorities()` via `priorityChoices()`. A string, not an int, deliberately: an `fs.Int` let pflag's `strconv.ParseInt` refuse the display word before lit's own gate ran (links-cli-bvko). The decimal spelling is still accepted, by `ParsePriorityName` rather than by pflag |
| `--assignee` | string | `""` | "Assignee" (trimmed, `cli.go:352`) |
| `--labels` | string | `""` | "Comma-separated labels" (split by `splitCSV`) |
| `--lane` | string | `""` | "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes" |
| `--top` | bool | `false` | "Promote the new issue to the top of its frame (the default appends it to the bottom)" |
| `--by` | string (hidden) | `""` | actor fallback (§1.11) — *note*: `runNew` registers no actor; `CreatedBy` is not set from the CLI here |

- `--top` maps to `storage.RankTop`; unflagged uses the zero `RankPlacement`
  (`rankPlacement`, `cli.go:319-325`).
- **Surplus positionals refused before `runNew` runs**: `refuseSurplusPositionals`
  (`register.go:342`), called from `parseLeaf` (`register.go:307`).
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
- Surplus positionals refused by `refuseSurplusPositionals` (`register.go:342`),
  called from `parseLeaf` (`register.go:307`), before `runFollowup` runs.

### 2.3 `lit ls` — List issues

- Registration `register.go:492-493`. **Not** wrapped by `appCmd`; the raw runner
  is `runList(ctx, stdout, lsSurface, args)` so `--at` can target a foreign store
  outside the current workspace (`register.go:488-493`, `cli.go:381-422`).
  `lsSurface` is `listSurface{name: "ls"}`, a surface with no positionals
  (`cli.go:352-360`); `lit children` runs the same `runList` over
  `childrenSurface` (§2.16).
- Summary text: "List issues (rank by default; --at \<store-dir> lists a discovered
  store read-only)" (`register.go:506`).

**Store routing** (`runList`, `cli.go:378-419`):
- `runList` builds the leaf and the `--at` value pointer with `listLeaf(surface)`
  (`cli.go:382`), parses argv with `parseLeaf` (`cli.go:383-385`), then reads the
  positionals with `listPositionals` (`cli.go:386-389`). Every step below runs
  after the parse, so `lit ls --help` and a malformed flag are answered before any
  store opens. pflag owns the flag grammar: a bare `--` ends flag parsing, so a
  later `--at` is a positional, and a trailing `--at` with no value is refused by
  the parse as a `UsageError` → exit 2 (`flagset.go:120-142`).
- Presence of `--at` is `l.fs.Changed(lsAtFlag)`, where `lsAtFlag = "at"`
  (`cli.go:390`, `:455`).
- If `--at` is present and its value is blank-after-trim or starts with `-` →
  `UsageError{"usage: lit ls --at <store-dir>  (a storage directory from `lit stores`)"}`
  (`cli.go:396-398`); the command name in the message is `surface.name`.
- Otherwise opens `app.OpenLocationForRead(ctx, workspace.LocationFromStorageDir(atDir))`;
  a failure becomes `fmt.Errorf("open store at %q read-only: %w", atDir, …)`,
  wrapping the open error marked with holder contention for that location
  (`cli.go:399-409`). The store is closed on return (`cli.go:410`). The work runs
  with `noReadyPolicy`, which returns no required fields (`cli.go:417`, `:790`).
- With no `--at`, opens the cwd workspace store through `runWithApp` with
  `app.AccessRead`, and the work runs with `workspaceReadyPolicy(ap)`
  (`cli.go:419-421`, `:759-761`).

**Flags** (declared in `listLeaf`, `cli.go:454-486`; read in its `work` closure,
`cli.go:492-606`). "Only if visited" means the flag appears on the command line
(`fs.Visit`, `cli.go:505-506`).

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--at` | string | `""` | Declared so the parse accepts it (`cli.go:461`); `listLeaf` returns its value pointer to `runList` (`cli.go:606`), which routes on it. The work closure does not read it |
| `--status` | string array | `nil` | State set via `model.ParseStates` — comma-separated and/or repeated, every fragment parsed; error wrapped `parse --status: %w` (`cli.go:470`, `:507-510`) |
| `--type` | string | `""` | Single issue type via `parseIssueTypeSlice` (blank → no narrowing); error wrapped `parse --type: %w` (`cli.go:511-514`, `:2016-2025`) |
| `--assignee` | string | `""` | Trimmed, single-element `Assignees`; blank → none (`cli.go:534`) |
| `--search` | string | `""` | Trimmed and appended to `SearchTerms` **only if visited** (`cli.go:548-550`) |
| `--ids` | string | `""` | CSV → `filter.IDs`, only if visited (`cli.go:551-553`) |
| `--parent` | string array | `nil` | Issue ids, comma-separated and/or repeated; each occurrence is split with `splitCSV` and the ids of all occurrences are collected, then the surface's positionals are appended → `filter.ParentIDs` (direct children, ORed). An occurrence that names no id (`--parent=`, `--parent " , "`) → `UsageError{"--parent needs an issue id, e.g. --parent <epic-id>"}` → exit 2, checked per occurrence before the positionals are appended, so `lit children <id> --parent=` is refused too. An id naming no issue → `NotFoundError` from the store → exit 4 (`cli.go:478`, `:520-529`, `:533`; `store.go:679-681`, `:770-773`) |
| `--labels` | string | `""` | CSV → `LabelsAll` (ALL must match), only if visited (`cli.go:554-556`) |
| `--has-comments` | bool | `false` | Only if visited; sets the pointer to the flag's value — so `--has-comments=false` filters to issues *without* comments (`cli.go:557-560`) |
| `--include-archived` | bool | `false` | `filter.IncludeArchived` (`cli.go:535`) |
| `--include-deleted` | bool | `false` | `filter.IncludeDeleted` (`cli.go:536`) |
| `--updated-after` | string | `""` | Only if visited; trimmed, RFC3339; parse error → `parse --updated-after: %w` (`cli.go:561-567`) |
| `--updated-before` | string | `""` | Only if visited; trimmed, RFC3339; parse error → `parse --updated-before: %w` (`cli.go:568-574`) |
| `--query` | string | `""` | Query language (see below), applied when non-blank after trim (`cli.go:575-584`) |
| `--sort` | string | `""` | `storage.ParseSortSpecs`, applied when non-blank after trim (`cli.go:539-547`) |
| `--columns` | string | `""` | CSV of column names, lowercased, via `parseColumnSelection` (`cli.go:497-500`, `columns.go:192-212`) |
| `--format` | string | `lines` | `lines` or `table` via `parseListFormat` (`cli.go:488`, `:501-504`) |
| `--limit` | int | `0` | `filter.Limit` (`cli.go:537`) |

- Flag help for `--query` (verbatim): "Query language: status:closed,in_progress
  resolution:wontfix type:task has:comments sort:rank:asc limit:5 archived deleted
  text" (`cli.go:485`).
- `--sort` help: "Sort fields, e.g. rank:asc,updated_at:desc" (`cli.go:486`).
  `ParseSortSpecs` splits on `,`, skips blank fragments, then reads
  `field[:asc|desc]` (a bare field is ascending); an unrecognized direction →
  `storage.ValidationError{"unsupported sort direction %q"}` → exit 3
  (`internal/storage/sort.go:18-50`).
- `--columns` help: "Comma-separated output columns: " followed by the sorted
  registry names (`cli.go:487`, `columns.go:176-178`). An empty selection (or one
  holding only commas) is the default `id,state,topic,title`
  (`columns.go:194-200`, `:158-160`). An unknown name →
  `UsageError{"unknown --columns name %q; valid columns: %s"}` → exit 2
  (`columns.go:202-208`). Valid names: `assignee`, `blocked`, `created_at`, `id`,
  `labels`, `parent`, `priority`, `rank`, `state`, `title`, `topic`, `type`,
  `updated_at` (`columns.go:92-124`).

**`--query` grammar** (`internal/query/query.go`):
- `Parse` trims the input and tokenizes it (`query.go:17-29`). The tokenizer
  splits on space, tab and newline and honors single and double quotes, which it
  strips; an unterminated quote → `"unterminated quote in query"`
  (`query.go:235-268`).
- Terms (`applyTerm`, `query.go:73-176`): `status:<state>[,<state>...]` (via
  `model.ParseStates`), `resolution:<res>` (via `model.ParseResolution`),
  `type:<type>` (via `model.ParseIssueType`), `assignee:<v>`, `id:<v>`,
  `parent:<v>` (bare `parent:` →
  `storage.ValidationError{"parent: needs an issue id, e.g. parent:<epic-id>"}`,
  `query.go:112-122`), `label:<v>`, `has:comments` (any other `has:` →
  `unsupported has: filter %q`), `sort:<spec>` (via `storage.ParseSortSpecs`),
  `limit:<int>` (non-numeric → `limit must be an integer, got %q`; negative →
  `limit must be non-negative, got %q`), bare `archived`, bare `deleted`, and any
  term beginning `updated` (`query.go:170-171`). Anything else becomes a free-text
  search term (`query.go:172-174`).
- `updated` terms (`applyTimeTerm`, `query.go:199-219`; `splitComparator`,
  `query.go:221-233`): the comparator is one of `>=`, `<=`, `>`, `<`, `:`; a missing
  comparator or empty value is wrapped `parse updated term %q`. The value parses as
  RFC3339, then RFC3339Nano; failure → `updated timestamp must be RFC3339`. `>=` and
  `>` both set updated-after; `<=` and `<` both set updated-before; `:` with a
  valid timestamp → `updated supports only >=, >, <=, <`.
- `query.Merge(flagFilter, queryFilter)` (`query.go:31-71`): statuses, types,
  assignees, parent ids and sort keys dedupe-merge, flag values first
  (`mergeSlice`, `query.go:181-197`); resolutions, search terms, ids and labels
  plain append; `IncludeArchived`/`IncludeDeleted` OR; `Limit` overwritten when the
  query limit > 0. Conflicting `has-comments` → `conflicting has-comments filters`;
  conflicting time bounds → `conflicting updated-after filters <t1> and <t2>`
  (`query.go:277-303`). `UpdatedAfter > UpdatedBefore` →
  `updated-after cannot be greater than updated-before` (`query.go:270-275`).

**Default active-work filter** (`cli.go:594-596`): if after all merging both
`filter.Statuses` and `filter.Resolutions` are empty, statuses default to
`[open, in_progress]`. A resolution filter alone therefore *does not* get clamped
(so `--query resolution:wontfix` reaches closed issues).

**Relation columns**: after `ListIssues` (`cli.go:597`), `listDerivedColumns`
(`cli.go:601`, `:633-660`) loads the data for the highest source any selected
column declares (`columnSourceFor`, `columns.go:232-238`; sources
`sourceIssue < sourceRelations < sourceReadiness`, `columns.go:74-84`). `parent`
declares `sourceRelations` and `blocked` declares `sourceReadiness`; every other
column is `sourceIssue` (`columns.go:92-124`).
- `sourceIssue`: no load; the cell map is nil (`cli.go:635-636`).
- `sourceRelations`: `fetchIssueRelations` batch-loads relations for the listed
  issues, and `parentColumnsFor` sets only `parentID` (`cli.go:637-642`,
  `:666-672`; `ready_state.go:101-120`).
- `sourceReadiness`: calls the policy for required fields, runs `annotateIssues`,
  and `readinessColumnsFor` sets `parentID` from the relation graph and
  `blocked = !ClassifyReadiness(row.Annotations).IsReady()` (`cli.go:640-653`;
  `workable.go:111-120`). A policy error fails the command. Over `--at`,
  `noReadyPolicy` supplies no required fields.
- A missing cell renders as the zero `derivedColumns` (`output.go:457-465`):
  `parent` renders `-` for an empty id, and `blocked` renders `blocked` when set
  and `-` otherwise (`columns.go:112-123`, `output.go:470-475`).

**Output**: `parseColumnSelection` and then `parseListFormat` run at the top of the
work closure, before the query (`cli.go:497-504`). `parseListFormat`
(`output.go:112-119`) lowercases and trims the value and looks it up in
`listFormats` (`output.go:92-95`): `lines` → `printIssueLines` (columns joined with
`" | "`, no header, `output.go:134-141`), `table` → `printIssueTable` (uppercased
tab-aligned header, then rows, `output.go:121-132`). Anything else, including an
explicit empty value, → `ValidationError{"unsupported --format \"<x>\" (valid: lines, table)"}`
→ exit 3, reason `validation_refused`. The `--format` help string ("Output format:
lines|table") is built from the same map (`cli.go:488`, `output.go:97-104`).

- Stray positionals: the leaf declares `positionals: 0` (`cli.go:492`), and
  `listPositionals` reads pflag's leftover arguments and refuses any count other
  than 0 → `UsageError{"usage: lit ls [flags]  (got N positional arguments: [...])"}`
  → exit 2, checked after the parse and before any store opens (`cli.go:386-389`,
  `:433-444`). `lit ls stray` is refused this way, and so is `lit ls -- --at <dir>`,
  whose two tokens after `--` are positionals.

### 2.4 `lit show` — Show issue details

- Registration `register.go:317-318`, `app.AccessRead`. Handler `runShow`
  (`cli.go:845-898`).
- Args: exactly one positional id; flag `--field` (string, `""`, help:
  "Comma-separated field names (e.g. description) to print with no surrounding
  context; omit for the full detail view") (`cli.go:846-848`).
- Refusals: `len(positional) != 1` →
  `UsageError{"usage: lit show <id> [--field <name>[,<name>...]]"}` → exit 2
  (`cli.go:851-857`).
- Sync-staleness banner is printed first **only when `--field` is blank**
  (`cli.go:864-868`).
- Reads `GetIssueDetail(id)`; missing → exit 4 (`cli.go:869-870`).
- Dispatches `EventShowTicket` in **both** modes (`cli.go:870-882`).

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
- No epic context, no parent block, no siblings (`cli.go:883-884`).

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
4. The `parent` group — one optional issue adapted to a slice and rendered by
   the shared group renderer, so the parent line carries a standing marker like
   every other relation line: `\nparent:\n- <id> [<standing>] <title>\n`, and,
   when the parent has one, its description indented by two spaces
   (`output.go:146-153`, `optionalGroup` at `output.go:326-331`).
5. `\ndescription:\n<text>\n` when non-empty (`output.go:127-131`).
6. `\nprompt:\n<text>\n` when non-empty (`output.go:132-136`).
7. Group blocks, each rendered as `\n<label>:\n` followed by
   `- <id> [<standing>] <title>` lines and omitted entirely when empty
   (`printIssueGroup`, `output.go:333-349`), in this order:
   `children` (`output.go:164`), `depends_on` (`output.go:173`),
   `blocks` (`output.go:176`), `redirect` (single optional target adapted to a
   slice, `output.go:182`, `optionalGroup` at `output.go:326-331`),
   `related` (`output.go:185`).
   `<standing>` is `issueStanding` (`output.go:372-378`): the retention name
   when the issue is frozen, else its state, and a closed state carries the
   resolution the close recorded as a `:<resolution>` tail —
   `[closed:duplicate]`, `[closed:superseded]`, `[closed:obsolete]`,
   `[closed:wontfix]` — with a bare `[closed]` for a close that recorded none.
   There is deliberately **no** `siblings` group here (`output.go:140-145`).
8. `\ncomments:` then `- [<createdBy>] <body>` with newlines in the body escaped
   to the literal `\n` (`output.go:161-170`).
9. **No** history block — history lives behind `lit history` (`output.go:171-175`).
10. Then `writeEpicContext` appends the epic plan block (§2.5). The block is
    **resolved before step 1 writes anything** (`cli.go:885-897`), so the body and
    the block are all-or-nothing: a failure to build the block exits nonzero with
    neither printed, rather than after a partial body. The staleness banner and any
    fired show-ticket workflow body are written before that point either way.

### 2.5 Epic-context block appended by `lit show`

`resolveEpicContext` (`epic_context.go:265-281`) and `writeEpicContext`
(`epic_context.go:292-298`):
- `epicViewFor(issue, parent)` (`epic_context.go:242-250`): a container shows its
  own children with no focused child; a leaf whose parent is a container shows the
  parent's plan with itself focused; anything else returns nil and **nothing is
  printed**.
- The target is resolved first, so the `ready.required_fields` policy is read
  from repo config (`readyRequiredFields` → `config.Load`, `cli.go:637-643`)
  **only when a plan slice exists**. An issue in no epic reads no repo config, so
  a `.lit/config.toml` that fails validation for an unrelated reason (for example
  `snapshot.retention_budget <= 0`) does not affect `lit show` on it; for an epic
  member the same config error fails the command, before any output.
- Prints a leading blank line then `renderEpicContext(ec)` (`epic_context.go:296`);
  a nil context (no plan slice) writes nothing.

`buildEpicContext` (`epic_context.go:170-222`):
- `GetRelationsByIDs([epicID])`; a missing epic → `storage.NotFoundError`
  (`epic_context.go:171-182`).
- The children run through `annotateIssues` (`cli.go`) — the same annotator set
  the workable pipeline uses — which batches their relations; a child listed but
  absent → `storage.NotFoundError`.
- Per child, `classifyChildStatus(child, ClassifyReadiness(row.Annotations))`:
  any child that is frozen or not `open` renders the word `issueStanding`
  composes, bracketed — archived/deleted → `[archived]` / `[deleted]`;
  `in_progress` → `[in_progress]`; `closed` → `[closed]` for a close that
  recorded no resolution, else `[closed:<resolution>]`
  (`[closed:duplicate]`, `[closed:superseded]`, `[closed:obsolete]`,
  `[closed:wontfix]`). An `open` child gets the readiness verdict — ready →
  `[ready]`, otherwise `[blocked: <reason>[; <reason>…]]` over every blocking
  reason the annotation registry minted, phrased by `BlockingReason.Phrase`
  (`readiness.go`): `depends on <id>`, `earlier sibling <id> still open`,
  `missing <field>`, `needs-design`.
- Cross-epic edges: for the epic node and every child that is not closed, each
  open `DependsOn` outside the epic membership set becomes a `BlockedExternally`
  edge, and each open `Blocks` outside becomes a `BlocksExternally` edge
  (`epic_context.go:321-332`). Membership = the epic id plus all child ids
  (`epicMemberIDs`, `epic_context.go:304-311`). Edges are sorted by
  (blocked, blocker) (`epic_context.go:358-369`).

`renderEpicContext` output shape (`epic_context.go:381-389`):
```
Epic: <epicID> — <epicTitle>
Why: <first non-blank line of epic description, leading '#'s stripped>

Children:
<child lines>
[Cross-epic dependencies block]
```
- `firstLine` strips whitespace and leading `#` characters (`epic_context.go:465-473`).
- Child line (`renderChildLine`, `epic_context.go:440-446`):
  `"    "` gutter (or `"  ▶ "` when focused), the status marker padded to
  `len("[in_progress]")` = 13 (`epic_context.go:124`), two spaces, the id, two
  spaces, the title, then `"  [lane: <lane>]"` when the lane is non-empty
  (`laneTag`, `epic_context.go:454-459`), then `"   (you are here)"` when focused.
- No children → `"  (none)\n"` (`epic_context.go:394-403`).
- Cross-epic block, omitted entirely when both directions are empty
  (`epic_context.go:410-419`):
```

Cross-epic dependencies:
  Blocks externally:
    <blocked> blocked by <blocker>
  Blocked externally:
    <blocked> blocked by <blocker>
```
  Each subsection is omitted when its slice is empty (`epic_context.go:425-435`).

### 2.6 `lit history` — State-transition history

- Registration `register.go:319-320`, `app.AccessRead`. Handler `runHistory`
  (`cli.go:928-942`).
- No flags beyond the implicit `--help`.
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit history <id>"}` → exit 2 (`cli.go:934-936`).
- Reads `GetIssueDetail(id)` (`cli.go:937-940`).
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

- Registration `register.go:321-322`, `app.AccessWrite`. Handler `runUpdate`
  (`cli.go:953-1063`).
- Flags (`cli.go:955-969`): `--title`, `--description`, `--prompt`, `--type`
  (default `""`), `--priority` (int, default 0), `--assignee`, `--labels`,
  `--lane`, `--status` (registered only to intercept it; help text
  "(removed) change status with the transition verbs: lit start|done|close|open"),
  `--reason` ("Reason recorded on the field-change event"), and hidden `--by`.
- Refusals:
  - `len(positional) != 1` → `UsageError` with the usage line
    `"usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--lane <key>] [--reason <text>]"`
    (`cli.go:973-978`).
  - `--status` present (detected via `fs.Visit`) → `UsageError{statusViaVerbsGuidance}`
    → exit 2. Verbatim text: "lit update no longer changes status — the transition
    verbs are the single enforcer of the transition guardrails. Use: `lit start
    <id>` (claim → in_progress), `lit done <id>` (finish → closed), `lit close <id>
    --resolution <duplicate|superseded|obsolete|wontfix>` (close with an outcome),
    `lit open <id>` (reopen)" (`cli.go:951`, `cli.go:988-990`).
  - No field flag at all → `errors.New("lit update requires at least one field flag")`
    → exit 1 (`cli.go:1049-1051`). Note `--reason` alone does not count: `Reason`
    is set unconditionally but `Change.IsEmpty()` governs (`cli.go:998-1003`,
    `cli.go:1049`).
- **Only visited flags are applied.** Each visited flag sets a pointer on
  `storage.UpdateIssueInput` (`cli.go:1005-1048`):
  - `--title` / `--description` / `--prompt` set the raw value (no trimming at
    the CLI) (`cli.go:1005-1015`).
  - `--type` parses via `parseIssueTypeFlag` → `ValidationError` on a bad value
    (`cli.go:1016-1022`).
  - `--priority` parses via `parsePriorityFlag` (`cli.go:1023-1029`).
  - `--assignee` is trimmed and honored **verbatim** — session-identity resolution
    is deliberately *not* applied here, so an empty value clears the assignee
    (`cli.go:1030-1040`).
  - `--labels` CSV replaces the whole label set (`cli.go:1041-1044`).
  - `--lane` trimmed (`cli.go:1045-1048`).
- The `Change.Actor` is `resolveActor()` — session identity else `--by` else `""`
  (which the store normalizes to `unknown`) (`cli.go:999`).
- Applies via `Store.Apply(ctx, id, change)` (`cli.go:1052`).
- Dispatches `EventTicketUpdated` (`cli.go:1056-1058`), prints the summary line,
  emits the `update` breadcrumb (`cli.go:1059-1062`).

### 2.8 `lit rank` — Reorder an issue's rank

- Registration `register.go:323-324`, `app.AccessWrite`. Handler `runRank`
  (`cli.go:1065-1144`).
- If `args[0] == "set"`, routes to `runRankSet(args[1:])` (`cli.go:1070-1072`).
- Flags (`cli.go:1075-1078`): `--top` (bool, "Move to highest rank"),
  `--bottom` (bool, "Move to lowest rank"), `--above` (string, "Rank above this
  issue ID"), `--below` (string, "Rank below this issue ID").
- Refusals:
  - `len(positional) != 1` →
    `UsageError{"usage: lit rank <id> --top|--bottom|--above <id>|--below <id>"}`
    → exit 2 (`cli.go:1082-1084`).
  - Number of *visited* mode flags ≠ 1 →
    `ValidationError{"exactly one of --top, --bottom, --above, --below is required"}`
    → exit 3 (`cli.go:1085-1102`). Note this counts presence, not truthiness, so
    `--top=false` still counts.
  - Surplus positionals refused by `refuseSurplusPositionals` (`register.go:342`),
    called from `parseLeaf` (`register.go:307`).
- Store calls (`cli.go:1110-1119`): `RankToTop`, `RankToBottom`,
  `RankAbove(issueID, *above)`, `RankBelow(issueID, *below)`. The relative forms
  return a `storage.RankMove{MovedID, AnchorID}`.
- **Frame substitution reporting** (`cli.go:1123-1135`):
  - If `move.MovedID != issueID`:
    `"<issueID> is inside <MovedID>; ranked the epic <MovedID> instead, leaving its internal order unchanged\n"`
  - If a named anchor was given and `move.AnchorID != namedAnchor`:
    `"<namedAnchor> is inside <AnchorID>; ranked relative to the epic <AnchorID> instead\n"`
  - (Asserted in `rank_frame_test.go:33-61`.)
- Then re-reads `GetIssue(move.MovedID)`, prints its summary line, emits the
  `update` breadcrumb (`cli.go:1136-1143`).

### 2.9 `lit rank set <id1> <id2> [...]`

- Handler `runRankSet` (`cli.go:1151-1179`). Declares `positionals: allPositionals`
  (`cli.go:1393`), the unbounded ceiling defined at `register.go:20`.
- Refusal: fewer than 2 positionals →
  `UsageError{"usage: lit rank set <id1> <id2> [<id3> ...]"}` → exit 2
  (`cli.go:1157-1159`).
- Calls `Store.RankSet(ctx, positional)` — atomic; stacks the named issues at the
  top in the given order (`cli.go:1160-1162`, doc at `cli.go:1146-1150`).
- Output: for each resolution where `RankedID != NamedID`,
  `"<NamedID> is inside <RankedID>; ranked the epic <RankedID> instead, leaving its internal order unchanged\n"`
  (`cli.go:1168-1174`); then
  `"ranked %d issues at top in order: <comma-joined RankedIDs>\n"`
  (`cli.go:1175-1177`); then the `update` breadcrumb (`cli.go:1178`).

### 2.10 Transition commands — `start`, `done`, `close`, `open`, `archive`, `unarchive`, `delete`, `restore`

All eight route through one handler `runTransition(ctx, stdout, ap, args, spec)`
(`cli.go:1383-1478`), registered via `r.transitionCmd(spec)` with
`app.AccessWrite` (`register.go:246-250`).

Registry rows and summaries:
- `start` — "Claim issue work", group `operations` (`register.go:325-326`)
- `done` — "Finish claimed work (success path; requires in_progress)" (`register.go:327-328`)
- `close` — "Close without finishing (wontfix / obsolete / duplicate; from any non-closed state)" (`register.go:329-330`)
- `open` — "Reopen issue(s)" (`register.go:336-337`)
- `archive` — "Archive issue(s)", group `retention` (`register.go:341-342`)
- `unarchive` — "Unarchive issue(s)" (`register.go:343-344`)
- `delete` — "Delete issue(s)" (`register.go:340-341`)
- `restore` — "Restore deleted issue(s)" (`register.go:342-343`)

**Common flags on every transition** (`cli.go:1384-1386`):
`--reason` (string, `""`, "Transition reason") and hidden `--by`.

**Per-spec flags** (`cli.go:1296-1336`):
- `start` adds `--assignee` (string, `""`, help "Assignee fallback when
  CLAUDE_CODE_SESSION_ID is unset (env always wins when set)") and `--take` (bool,
  `false`, help "Confirm taking over a lane another checkout claims right now
  (required for non-interactive callers; an interactive terminal is prompted
  instead)") (`cli.go:1304`, `cli.go:1310`). The action is
  `model.Start{Assignee: resolveIdentity(*assignee)}` (`cli.go:1306`).
- `close` adds `--resolution` (string, `""`, "Close resolution (required):
  duplicate|superseded|obsolete|wontfix") and `--of` (string, `""`, "Canonical
  ticket a duplicate/superseded close redirects to (required for those, rejected
  otherwise)") (`registerCloseOutcomeFlags`, `cli.go:1341-1345`).
- `done`, `open`, `archive`, `unarchive`, `delete`, `restore` register **no**
  extra flags — `fixedAction` (`cli.go:1290-1294`). Passing e.g.
  `lit done --resolution x` is therefore an unknown-flag `UsageError` → exit 2.

**Argument handling** (`cli.go:1392-1398`): after parsing, exactly one remaining
positional is required; otherwise `errors.New("usage: lit <name> <id> [--reason <text>]")`
— a **plain error**, so exit code 1, not 2.

**Sequence** (`transitionLeaf`, `cli.go:1546-1649`):
1. `GetIssue(issueID)` pre-read (`cli.go:1569-1572`) — missing → exit 4.
2. `buildAction()` (`cli.go:1574-1577`).
3. `authorize(ctx, stdout, ap, issueID, prior)` — §2.11 (`cli.go:1583-1585`).
4. `transferNotice(ctx, ap, issueID, action)` (`cli.go:1593-1596`, implementation
   `claims_context.go:163-177`): for a `model.Start` whose prior claimant was
   held and actually changes hands, `"claim transferred: %s -> %s\n"` with
   both sides rendered by `describeClaimant` (`claims_render.go:186-194`),
   which names the assignee when there is one, the checkout via `nameCheckout`
   (`claims_render.go:215-225`) when there isn't or alongside it, and the
   literal `"the public checkout"` — never `"(unassigned)"` — for a prior
   claimant with no assignee and no minted token. Computed here, on
   pre-Apply state, but not written out until step 7 — a failed `Apply`
   must announce nothing.
5. `actor := resolveActor()`; `Store.Apply(ctx, issueID, Change{Action, Actor, Reason})`
   (`cli.go:1602-1606`).
6. If the action is a `StatusAction`, dispatch the transition occasion
   (`cli.go:1616-1620`).
7. Write the transfer notice string, then `printIssueSummary`
   (`cli.go:1625-1631`).
8. If the action is a `StatusAction` whose `Target() == model.StateClosed`
   (i.e. `done` and `close`), re-read `GetIssueDetail` and print the close
   adjacency block (`cli.go:1639-1647`) — §2.12.
9. Breadcrumb per `transitionBreadcrumbTopics` (`cli.go:1648-1650`).

**Close outcome validation** — `closeOutcomeFromFlags(resolution, target, usage)`
(`cli.go:1355-1381`), shared with `bulk close` (`bulk.go:149`):
- `model.ParseResolution` failure → `UsageError{"<usage>\n<parse error>"}` → exit 2
  (`cli.go:1356-1359`). For `lit close`, the usage string is
  `"usage: lit close <id> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]"`
  (`cli.go:1322`). A missing `--resolution` therefore fails through this path —
  `--resolution` is effectively required.
- Redirecting resolutions (`duplicate`, `superseded`) with a blank `--of` →
  `UsageError{"closing as <res> redirects to a canonical ticket — name it with --of"}`
  (`cli.go:1361-1364`).
- A non-blank `--of` on a terminal resolution (`obsolete`, `wontfix`) →
  `UsageError{"--of applies only to duplicate/superseded closes, not <res>"}`
  (`cli.go:1365-1367`).
- Outcome construction: `duplicate` → `model.Duplicate{Of: target}`;
  `superseded` → `model.Superseded{By: target}`; `obsolete` → `model.Obsolete{}`;
  `wontfix` → `model.Wontfix{}` (`cli.go:1368-1377`). An unreachable default
  returns `fmt.Errorf("resolution %q has no close outcome", parsed)`
  (`cli.go:1378-1380`).

**Store-level refusals reaching every transition:**
- An archived or deleted issue rejects any status action:
  `"cannot <action> archived or deleted issue"` (`internal/store/store.go:89-96`).
- A container (epic) rejects any status action with `ContainerActionError`
  (`internal/model/model.go:315-317`).
- The redirect target must exist, must not be the closing issue itself, and must
  not be deleted: `"closing as <res> requires a canonical target issue to redirect
  to"`, `"cannot redirect <id> to itself"`, `"cannot redirect <id> to <target>:
  the canonical issue is deleted"` (`internal/store/store.go:1562`, `:1567`,
  `:1574`).
- `archive` on a deleted issue → `"cannot archive deleted issue"`;
  `unarchive` on a deleted issue → `"cannot unarchive deleted issue"`
  (`internal/model/lifecycle/retention.go:73`, `:82`).
- **There is no from-state precondition on `done`.** `Store.Apply` performs no
  status-precondition check (`internal/store/store.go:1088-1145`), and
  `applyStatusAction` is total over the leaf states
  (`internal/model/lifecycle/status_states.go:134-161`). The registry summary
  "requires in_progress" (`register.go:332`) is not enforced by any code path in
  this repo. A same-state transition is a no-op that records nothing
  (`status_states.go:136-138`, `internal/store/store.go:1079-1086`).

### 2.11 `lit start` takeover gate

`startSpec.authorize` → `authorizeStart(ctx, stdout, ap, issueID, prior, *take)`
(`cli.go:1413-1418`, implementation `claims_takeover.go:132-154`):
1. `GetRelationsByIDs([issueID])` and `model.LaneOf(prior, parent)`
   (`claims_takeover.go:133-137`).
2. `gatherClaimContext(ctx, stdout, ap)` (`claims_takeover.go:138-141`).
3. `classifyTakeover(standing, self)` (`claims_takeover.go:110-119`), switching on `relationOf` rather than on the standing directly:
   - `Held` by self, `Stale` by self, or `Unclaimed` → `takeoverNone` (no-op).
   - `Stale` by another → `takeoverStaleInformed`, **except** when `s.Holder == claims.Locked`, which `relationOf` reports as `laneHeldForeign` (`:94`) and which therefore takes the `takeoverFreshConfirm` path below.
   - `Held` by another → `takeoverFreshConfirm`.
4. **Stale, foreign**: prints
   `"<claim line> — check for unmerged branches or PRs on this lane before building on it\n"`
   and proceeds (`printStaleProvenance`, `claims_takeover.go:177-184`).
5. **Fresh, foreign** (`confirmFreshTakeover`, `claims_takeover.go:195-218`):
   - Non-interactive stdout (`!isTerminal(stdout)`) and no `--take` →
     `fmt.Errorf("<claim line> — this lane is claimed and active; pass --take to confirm the takeover")`
     → exit 1 (`claims_takeover.go:200-202`).
   - Non-interactive with `--take` → prints `"<claim line> — taking over (--take)\n"`
     and proceeds (`claims_takeover.go:204-205`).
   - Interactive → prints `"<claim line>\ntake over this lane? [y/N] "`, reads a
     line from stdin; a read error other than EOF →
     `"read takeover confirmation: %w"`; an answer not starting with `y`
     (case-insensitive, trimmed) → `fmt.Errorf("takeover declined")` → exit 1
     (`claims_takeover.go:207-216`).

Claim line format — `formatClaimLine(cc, lane, now)` (`claims_render.go:23-44`):
returns `false` (no line) for an `Unclaimed` lane. Otherwise
`"<prefix>[ · contested by <s1>, <s2>] · <age> ago[ · <lane progress>]"` where:
- prefix, when the holder resolves to a live local worktree:
  `"claimed here[ (stale|locked)]: <path> (<branch or 'detached HEAD'>)"`
  (`claimPrefix`, `claims_render.go:102-111`)
- otherwise: `"claimed: <name> (<state>)"` — `<name>` is `nameCheckout(by)`
  (`claims_render.go:215-225`): the stream's first 8 chars, or the literal
  `the public checkout` when `by` is the zero Attribution (an unattributed
  establishing event); `<state>` is `holdState(by, kind)`
  (`claims_render.go:141-151`): `elsewhere` for an identified holder, `stale`,
  `locked`, or `unaddressed` for the public checkout, which has no address to
  be "elsewhere" from
- age via `humanizeCoarseDuration(now - tenure.LastActivity)` (`claims_render.go:39`)
- lane progress: `"<activeID> in progress, <done>/<total> done"` or
  `"<done>/<total> done"`; empty for a zero LaneProgress
  (`claims_render.go:158-166`).

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
- `backlogView` preset (`workable.go:92-99`): `hasFilters: true`,
  `hasLimit: true`, `hasColumns: true`, order = `orderCanonical` (no-op,
  `workable.go:84`), keep = `keepAll` (`workable.go:86`), render =
  `printBacklogOutput`, occasion = `backlogOccasion()`.
- Flags (`runWorkable`, `workable.go:113-119`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--assignee` | string | `""` | "Filter by assignee" (always registered) |
| `--type` | string | `""` | "Filter by issue type" |
| `--status` | string | `""` | "Filter by status: open\|in_progress" |
| `--labels` | string | `""` | "Comma-separated labels all of which must match" |
| `--limit` | int | `0` | "Limit results" — applied **after** ordering (`workable.go:161`) |
| `--columns` | string | `""` | "Comma-separated output columns" |

- Refusal: any positional argument → `UsageError{view.usage()}` (`workable.go:123-125`).
  `usage()` builds `"usage: lit backlog [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--limit N] [--columns ...]"`
  (`workable.go:61-82`).
- `--status closed` (or any unparseable state) →
  `UsageError{"invalid --status \"<x>\" (valid: open, in_progress)"}` → exit 2
  (`parseWorkableStatus`, `workable.go:178-187`).
- Bad `--type` → `UsageError{"invalid --type \"<x>\": <err>"}` → exit 2
  (`parseWorkableType`, `workable.go:193-202`).
- Prints the sync-staleness warning first (`workable.go:187`).
- Runs the shared workable pipeline (§1.18) via `gatherWorkableAnnotated`
  (`workable.go:150-158`), then `gatherClaimContext` (`workable.go:162-165`).
- Dispatches `EventShowBacklog` after rendering (`workable.go:169`).

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
     `backlog.go:104-113`). `EarlierSiblingPending` appears in **neither** the
     blocked line nor the depends-on line.
   - `    depends on: <ids joined by ", ">` (`backlog.go:84`, `output.go:41-47`)
   - `    in_progress: <age truncated to minute>[ (ORPHANED)]` for in-progress
     rows (`backlog.go:87-91`, `inProgressSuffix` at `ready_state.go:918-925`)
   - `    <claim line>` when the row's lane is Held or Stale (`backlog.go:92-96`)
   - `    unblocks: <ids of rows that depend on this one>` — derived from the
     classified open-dependency facts of the whole workable queue, before any
     narrowing, so the line survives a filter or a limit that cuts the
     dependent row (`backlog.go:97`, `deriveQueueFacts` at
     `queue_facts.go:45-56`, read back through `queueFacts.Unblocks` at
     `queue_facts.go:34`)
6. Finally, if any row carries a `RankInversion` annotation:
   `"\nWarning: %d rank inversion(s) — dependencies ranked below their dependents. Run `lit doctor --fix` to repair. <agent-instructions>This command is idempotent and safe to run without confirmation.</agent-instructions>\n"`
   (`printRankInversions`, `ready_state.go:929-939`).

Lane for the claim line is `model.LaneOf(entry.Issue, details[entry.ID].Parent)`
(`backlog.go:58`).

### 2.14 `lit next` — Print the next workable leaf

- Registration `register.go:537-538`, `app.AccessRead`, handler `nextLeaf`
  (`next.go:31-93`). Summary: "Print the next workable leaf to lit start".
- Flags (`next.go:32-40`, and the hidden `--by` at `:60`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--assignee` | string | `""` | "Filter by assignee" |
| `--type` | string | `""` | "Filter by issue type" |
| `--status` | string | `""` | "Filter by status: open\|in_progress" |
| `--labels` | string | `""` | "Comma-separated labels all of which must match" |
| `--all` | bool | `false` | "Ignore the focus scope and route over the whole queue" |
| `--by` | string (hidden) | `""` | identity fallback (§1.11), read by `resumeAdvice` to say whether work in flight is the reader's (`next.go:60`) |

- `--status` and `--type` go through the same `parseWorkableStatus` /
  `parseWorkableType` refusals as `backlog` (`next.go:62-69`). **No** `--limit`,
  **no** `--columns`.
- Refusal: any positional → `UsageError{nextUsage}` → exit 2, where `nextUsage` =
  `"usage: lit next [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--all]"`
  (`next.go:29`), declared as the leaf's `usage` rather than checked inline
  (`next.go:61`).
- Retired flag: `--continue` is intercepted at the shared parse boundary as
  `UnsupportedError` → exit 3, message
  ``"--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag"``
  (`flagset.go:138-141`).
- Prints the sync-staleness warning first (`next.go:72`).
- Gathers the workable set — rows, relation details, **and the focus scope** —
  via `gatherWorkableAnnotated` (`next.go:75-80`, `cli.go:655`), then the claim
  context (`next.go:84`), and then routes (`next.go:88`), handing the render the
  reader's own identity from `actor()` alongside it:
  `routeNext(rows, details, cc.standings, cc.self, focus.scopeFor(*all))`.
  `scopeFor(true)` returns the zero `focusScope`, which holds every row
  (`ready_state.go:813-818`, `ready_state.go:795`, `ready_state.go:805-807`).

**`NextOutcome`** — a sealed sum interface (`next_route.go:26`,
`next_route.go:208-214`) with **seven** cases:

| Case | Fields | Meaning |
|---|---|---|
| `ServedFromClaim` | `Row` | a ready ticket in a lane this checkout already holds — step 1 (`next_route.go:30`) |
| `ResumedOwnWork` | `Row` | a ticket already in flight in a lane this checkout holds, handed back to its holder — also step 1 (`next_route.go:40`) |
| `ServedFromEpicLane` | `Row`, `Lane model.LaneID` | a pick from a different lane of the same epic this checkout already holds a lane in — step 2. The epic is `Lane.Epic()`; there is no `Epic` field (`next_route.go:50-53`) |
| `ServedFromNewLane` | `Row`, `Lane model.LaneID` | a ticket in a lane this checkout does **not** hold — produced by step 4 alone (`next_route.go:75-78`) |
| `ServedFromDependency` | `Row`, `Lane model.LaneID`, `Gates string` | step 1b's on-path dependency; `Gates` is the id of the blocked row it unblocks, so the pick explains itself (`next_route.go:80-104`) |
| `Exhausted` | `Epics []string`, `Blocked []rowReach` | the checkout's own epic(s) have open work, none of it reachable — step 3 (`next_route.go:118-121`) |
| `NoWork` | `Unreachable []rowReach` | the global pool produced nothing — step 4 (`next_route.go:206`) |

`Exhausted` and `NoWork` implement `error` and travel outward as themselves
rather than being rendered into a generic error (`next_route.go:551`,
`next_route.go:658`).

**Admission** — `capacityFor(row, standing, self) capacity`
(`next_route.go:259-280`) is the single eligibility verdict. The four capacities
are `routeAround`, `serveWork`, `resumeWork`, `takeoverWork`
(`next_route.go:226-238`). With `readiness = ClassifyReadiness(row.Annotations)`,
`relation = relationOf(standing, self)` (`claims_takeover.go:68-101` —
`laneOurs` requires `self.Present() && standing.By == self`, so a checkout with
no minted token never reads a lane as its own, even one the public checkout
itself holds), and `started = row.State() == model.StateInProgress`:

1. `relation == laneOurs`: `started` → `resumeWork`; else `readiness.IsReady()` →
   `serveWork`; else `routeAround`.
2. Otherwise `takeable := (started && readiness.IsOrphaned()) || (!started && readiness.IsReady())`,
   then: `!takeable` **or** `relation == laneHeldForeign` → `routeAround`;
   `started` **or** `relation == laneStaleForeign` → `takeoverWork`; else
   `serveWork`.

So a `laneStaleForeign` lane yields `takeoverWork`, and steps 2 and 4 accept it.
Only `laneHeldForeign` is routed around — which includes a `claims.Stale`
standing whose `Holder` is `claims.Locked`, since `relationOf` reads a locked
worktree as a fresh foreign hold (`claims_takeover.go:48-60`,
`claims_takeover.go:94-96`). Servability is not gated on `model.StateOpen`.

**Routing precedence** — `routeNext(rows, details, standings, self, scope focusScope)`
(`next_route.go:337`). `rows` are already in composite-rank order (§1.18).
`laneOf(row) = model.LaneOf(row.Issue, details[row.ID].Parent)`
(`next_route.go:338-340`);
`verdict(row) = capacityFor(row, standings.Of(laneOf(row)), self)`
(`next_route.go:341-343`);
`reachFor(row, gathered) = reachOf(row, gathered, standings.Of(laneOf(row)), self)`
(`next_route.go:344-346`).
`pickFrom(from, inScope, accept ...capacity)` keeps the first row of `from`, in
rank order, whose lane `inScope` admits and whose verdict is in `accept`
(`next_route.go:360-367`); `pick` is `pickFrom` over all `rows`
(`next_route.go:368-370`). `accept` is a **set**, never a preference order —
composite rank is the only tiebreak routing applies (`next_route.go:350-355`).
`ownScope(standings, self)` yields `ownLanes` and `ownEpics`, read from the
**standings** and not from the gathered rows (`next_route.go:295-308`);
`mine(lane) = ownLanes[lane]` (`next_route.go:373`).

If `len(ownLanes) > 0` (`next_route.go:374`):

1. **Own lanes**, accepting `{serveWork, resumeWork}`, whichever the backlog ranks
   first (`next_route.go:377-382`). `resumeWork` → **`ResumedOwnWork{Row}`**;
   `serveWork` → **`ServedFromClaim{Row}`**.
   - 1b. Else `onPathDependency(rows, laneOf, mine, reachFor)` — the first
     dependency gating one of our own lanes whose `reachKind` is `reachTakeable`
     (`next_route.go:540-547`), drawn from `gatingDependencies`, which collects
     the distinct open dependency IDs of the in-scope **open** rows in rank order
     and stamps each with `reachFor` (`next_route.go:492-525`) →
     **`ServedFromDependency{Row: dep, Lane: laneOf(dep), Gates: gates}`** (`next_route.go:387-389`), the `Gates` being the blocked row the dependency gates.
2. Else **the rest of our epic, in lanes we do not already hold** — predicate
   `lane.Epic() != "" && ownEpics[lane.Epic()] && !mine(lane)`, accepting
   `{serveWork, takeoverWork}` → **`ServedFromEpicLane{Row, Lane: laneOf(row)}`**
   (`next_route.go:390-396`).
3. Else → **`Exhausted{Epics, Blocked}`** (`next_route.go:398-403`), where `Epics`
   is `slices.Sorted(maps.Keys(ownEpics))` and `Blocked` is `blockedRows` over
   `gatingDependencies` for the lanes admitted by
   `func(lane) bool { return mine(lane) || ourEpic(lane) }`; `blockedRows` drops
   the gated id, so this diagnostic is unchanged. Exhaustion never falls through
   to the global pool.

Step 4 is reached only by a checkout holding no lanes, which starts there
directly:

4. **The global pool, focus-scoped.** `pool, offPath := scope.partition(rows)`
   (`next_route.go:415`, `ready_state.go:825-834`), then
   `pickFrom(pool, func(model.LaneID) bool { return true }, serveWork, takeoverWork)` →
   **`ServedFromNewLane{Row, Lane: laneOf(row)}`** (`next_route.go:416-418`).
   Else → **`NoWork{Unreachable: append(passedOver(pool, reachFor), withheldByScope(offPath)...)}`**
   (`next_route.go:419`), where `passedOver` stamps every walked pool row with
   `reachFor(row, true)` (`next_route.go:448-454`) and `withheldByScope` stamps
   every scope-excluded row `reachOffFocusPath` (`next_route.go:427-433`).

Steps 1-3 walk every gathered row; step 4 walks the focus-scoped pool. The row
set is passed to `pickFrom` explicitly at each step so that difference stays
visible (`next_route.go:356-359`).

**`reachKind`** (`next_route.go:135-160`) — what one row is to this checkout right
now: `reachTakeable`, `reachHeldFresh`, `reachNotReady`, `reachOutOfView`, plus
`reachOffFocusPath`, which only the pool diagnostic stamps, and the bound
`reachKindCount`. `reachOf(row, gathered, standing, self)` answers
`reachOutOfView` when `!gathered`, `reachTakeable` when
`capacityFor(...) != routeAround`, `reachHeldFresh` when
`relationOf(...) == laneHeldForeign`, else `reachNotReady`
(`next_route.go:179-189`). `rowReach{ID string, Row annotation.AnnotatedIssue, Kind reachKind}`
(`next_route.go:164-168`) is what both terminal outcomes carry.

`exhaustedNotes` (`next_route.go:592-597`):
- `reachTakeable`: ``"on your path and yours to take — `lit start` it"``
- `reachHeldFresh`: `"on your path but claimed by another checkout right now"`
- `reachNotReady`: ``"on your path but not startable right now — `lit show` it"``
- `reachOutOfView`: ``"on your path but outside this view — `lit show` it"``

`poolNotes` (`next_route.go:598-602`):
- `reachHeldFresh`: `"in progress or claimed in a lane another checkout holds right now"`
- `reachNotReady`: `"not startable — blocked by a dependency, or in flight and not abandoned"`
- `reachOffFocusPath`: ``"off the focus path this run answered over — `lit next --all` to route over the whole queue"``

`describeReach(rows, lead, notes)` renders `"<lead><ids> (<note>)"` for each kind
that has rows, joined by `"; "`, in `reachKind` declaration order
(`next_route.go:632-646`). `nameIDs` names at most `maxNamedPerKind = 12` ids and
otherwise appends `" and <n> more"` (`next_route.go:611`, `next_route.go:617-624`).

**Terminal messages.** `Exhausted.Error()` (`next_route.go:551-560`): `scope` is
`"epic(s) <Epics joined by ", ">"` when `Epics` is non-empty, else
`"your claimed lane(s)"`.
- `Blocked` empty: ``"no ready work in <scope> — nothing else is queued behind what's already in progress; picking up other work is a deliberate re-focus, not a bare `next`"``
- Otherwise: ``"no ready work in <scope> — <describeReach(Blocked, "blocked on ", exhaustedNotes)>; picking up other work is a deliberate re-focus, not a bare `next`"``

`NoWork.Error()` (`next_route.go:658-674`):
- `Unreachable` empty: `"no ready work"`
- Any row `reachOffFocusPath` (`NoWork.withheld()`, `next_route.go:679-686`):
  `"no ready work on the focus path — the backlog is not empty, and each row below says why this run did not serve it: <describeReach(Unreachable, "", poolNotes)>"`
- Otherwise: `"no ready work — the backlog is not empty, but nothing in it is startable here: <describeReach(Unreachable, "", poolNotes)>"`

Both map to `ExitNoWork` = **6** (`exit.go:31`, `exit.go:122-129`), with reasons
`scope_exhausted` and `no_ready_work` respectively (`error_output.go:112-119`).

**Rendering** — `renderNextOutcome(w, outcome, details, cc)` (`next.go:111-161`):
- `ServedFromClaim` → no announcement at all (`next.go:115-116`).
- `ResumedOwnWork` → `resumeAdvice(o.Row, cc.actingAs)` + `"\n"` (`next.go:117-119`,
  `next.go:228-233`). The row's assignee decides which of two sentences: when it is
  non-empty and differs from the identity running the command,
  ``<RowID> is in progress and assigned to <assignee>, not to you — check that they have stopped before you continue it, or take other work from `lit backlog` ``;
  otherwise `"<RowID> is already in progress in a lane you hold — continue where you left off"`.
- `ServedFromEpicLane` → `startAdvice(o.Row, o.Lane, expiredHolder(cc.standings.Of(o.Lane)))`
  + `" (a second lane of an epic you already hold a lane in)\n"` (`next.go:120-122`).
- `ServedFromNewLane` → the same `startAdvice(...)` + `"\n"` (`next.go:123-125`).
- `ServedFromDependency` → the same `startAdvice(...)` + `" (gates %s, which is in a lane you hold)\n"` on `Gates` (`next.go:131-134`).
- `Exhausted`, `NoWork` → returned as themselves; no ticket printed
  (`next.go:144-147`).
- Any other outcome type → panic (`next.go:148-149`).

`startAdvice(row, lane, holder)` (`next.go:259-272`) is exactly four sentences,
selected by the row's state and by whether `lane.Describe()` reports a named lane:
- in progress, lane not named: ``"<id> is in progress and <state> — run `lit start <id>` to take it over"``
- in progress, lane named: ``"<id> is in progress and <state> — run `lit start <id>` to take over <described>"``
- not in progress, lane not named: ``"run `lit start <id>` to claim it"``
- not in progress, lane named: ``"run `lit start <id>` to claim <described>"``

`<state>` is `inFlightState(holder)` (`next.go:175-183`): `claims.Locked` →
`"claimed by a locked worktree whose claim has gone stale"`; `claims.Present` →
`"stale, though its holder's worktree is still on disk"`; otherwise
`"abandoned"`. `holder` is `expiredHolder(standing)` — the `Stale` standing's
`Holder`, and `claims.Unprovable` for every other standing
(`claims_render.go:89-94`).

`LaneID.Describe() (string, bool)` (`model.go:255-263`):
- solo lane → `("", false)`
- empty key → `("the default lane of epic <epic>", true)`
- otherwise → `("lane <key> of epic <epic>", true)`

On a served row, `renderNextOutcome` calls `printNextSummary(w, row, cc, lane)`
with `lane = model.LaneOf(row.Issue, details[row.ID].Parent)` (`next.go:175-183`),
which prints the **default columns** (`id state topic title`) joined by two
spaces (`ready_state.go:965-970`, `columns.go:158-160`), then `printInlineDeps`
(`ready_state.go:1000-1020`): `    epic: …`, `    depends on: …`, the claim line,
and `    unblocks: …` — but `next` passes a **nil** unblocks map, so the unblocks
line never appears (`ready_state.go:970`). It then returns
`nextPulledOccasion(row.Issue)` (`next.go:160`, `workflow_events.go:39-45`),
dispatched as `EventNextPulled` (`next.go:92`).

`lit next` performs **no writes** — it is registered `app.AccessRead`
(`register.go:499`). `startAdvice` names what a subsequent `lit start` would
claim; this command claims nothing.

### 2.15 `lit orphaned` — Stale in-progress issues

- Registration `register.go:304-305`, `app.AccessRead`. Handler `runOrphaned`
  (`cli.go:813-854`). Summary: "List in_progress issues with no recent updates".
- Flags: `--assignee` (string, `""`, "Filter by assignee") (`cli.go:815`).
- Refusal: any positional → `UsageError{"usage: lit orphaned [--assignee <user>]"}`
  → exit 2 (`cli.go:819-821`).
- Query: `Statuses = [in_progress]`, assignee filter, no archived, no deleted
  (`cli.go:822-827`). Containers dropped via `filterWorkableIssues`
  (`cli.go:836`).
- Annotates with `newOrphanedAnnotator(orphanedThreshold)` only (`cli.go:837`) and
  keeps rows where `ClassifyReadiness(...).IsOrphaned()` (`cli.go:841-846`).
- Sorted oldest-`UpdatedAt` first (`cli.go:850-852`).
- Output (`printOrphanedText`, `cli.go:856-870`):
  - Empty → `"No orphaned issues."`
  - Otherwise, per row: columns `id | state | topic | assignee | title` joined by
    `" | "`, then `" | Last Update: <age truncated to minute>"`.

### 2.16 `lit children <parent-id>`

- Registration `register.go:570-573`: `runList(ctx, stdout, childrenSurface, args)`,
  the same entrypoint and leaf as `lit ls` (§ `lit ls` above). Summary: "List an
  issue's direct children by rank (`lit ls --parent <id>`; takes every ls flag)".
- `childrenSurface = listSurface{name: "children", positionals: []string{"<parent-id>"}}`
  (`cli.go:360-363`). `listLeaf(surface)` names the flag set after the surface and
  declares every `ls` flag, so `lit children --help` lists them (`cli.go:457-458`).
- Refusal: the leaf declares `positionals: 0`, so every token reaches pflag
  (`cli.go:492`), and `listPositionals` reads the positionals from pflag's leftover
  arguments, `l.fs.cmd.Flags().Args()`. Because pflag knows which flags are
  booleans, the id is found before or after any flag, including after
  `--include-archived` or `--`. Each positional is trimmed. A count other than 1,
  or a positional that is blank after trimming, →
  `UsageError{"usage: lit children <parent-id> [flags]  (got N positional arguments: [...])"}`
  (the list printed with `%q`) → exit 2, checked after the parse and before any
  store opens (`cli.go:386-389`, `:433-444`).
  A blank or `-`-prefixed `--at` → `UsageError{"usage: lit children --at <store-dir>  (a storage directory from `lit stores`)"}`
  (`cli.go:396-398`).
- Filter: the positional is appended to the `--parent` ids (`cli.go:529`), so
  `lit children <id> [flags]` builds the filter `lit ls --parent <id> [flags]` builds
  and prints the same output. The `ls` defaults apply: statuses default to
  `[open, in_progress]` when no status or resolution filter is set
  (`cli.go:594-596`), archived and deleted children are excluded unless
  `--include-archived`/`--include-deleted`, and the default columns are
  `id,state,topic,title`. A parent id naming no issue → `NotFoundError` → exit 4.

### 2.17 `lit comment` — Add / remove comments

Family `commentFamily`, usage `"usage: lit comment <add|rm> ..."` (`cli.go:1486-1492`).
Both subcommands are `app.AccessWrite`. Missing/unknown subcommand → the bare
usage string as a plain error → exit 1 (`register.go:112-123`).

**`lit comment add <id> --body <text>`** (`runCommentAdd`, `cli.go:1494-1518`):
- Flags: `--body` (string, `""`, "Comment body"), hidden `--by`.
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit comment add <id> --body <text>"}` → exit 2
  (`cli.go:1502-1507`).
- Store refusal: a blank body (after trim) →
  `errors.New("comment body is required")` → exit 1
  (`internal/store/store.go:1156-1159`). A missing issue → exit 4
  (`store.go:1152-1155`).
- The comment id is `"cmt-" + uuid` and `CreatedBy` empty is normalized to
  `"unknown"` (`store.go:1161-1165`).
- Dispatches `EventCommentAdded` (`cli.go:1514-1516`).
- Output: `printComment` → `"<issueID> <commentID>\n"` (`cli.go:1539-1542`).
  **No breadcrumb.**

**`lit comment rm <comment-id>`** (`runCommentRm`, `cli.go:1517-1534`):
- No flags (not even `--by`).
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit comment rm <comment-id>"}` → exit 2 (`cli.go:1526-1531`).
- Store: blank id → `"comment id is required"`; unknown id →
  `storage.NotFoundError{Entity: "comment", ID: id}` → exit 4
  (`internal/store/store.go:1175-1194`).
- Output: `"<issueID> <commentID>\n"` for the deleted comment (`cli.go:1536`).

### 2.18 `lit label` — Manage labels

Family `labelFamily`, usage `"usage: lit label <add|rm> ..."`
(`issue_relations.go:12-18`); both `app.AccessWrite`.

**`lit label add <issue-id> <label>`** (`issue_relations.go:28-49`):
- Declares `positionals: 2` (`issue_relations.go:32`); `parseLeaf` splits once.
  Hidden `--by` registered.
- Refusal: `len(positional) != 2` →
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

Reserved label semantics: `needs-design` blocks readiness (§1.18, `ready_state.go:25`);
`focus` marks a goal for focus-path ordering (`ready_state.go:504`).

### 2.19 `lit parent` — Manage parent relationships

Family `parentFamily`, usage `"usage: lit parent <set|clear> ..."`
(`issue_relations.go:20-26`); both `app.AccessWrite`. Group `structure`
(`register.go:348-349`).

**`lit parent set --child <id> --parent <id>`** (`issue_relations.go:73-104`):
- Flags: `--child` ("Child issue ID (required)"), `--parent` ("Parent issue ID
  (required)"), hidden `--by`.
- Refusals, in order: blank `--child` or blank `--parent` →
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
  (`issue_relations.go:112-114`). Surplus positionals refused by
  `refuseSurplusPositionals` (`register.go:342`) before `parent clear`'s work runs.
- Calls `Store.ClearParent(childID)`; prints `ok` then the `update` breadcrumb.

### 2.20 `lit dep` — Manage dependency edges

Family `depFamily`, usage `"usage: lit dep <add|rm|ls> ..."` (`dependency.go:14-21`).
`add`/`rm` are `app.AccessWrite`, `ls` is `app.AccessRead`. Group `structure`
(`register.go:366-367`).

**`lit dep add --from <id> --to <id> [--type ...]`** (`dependency.go:23-66`):
- Flags: `--type` (string, default `"blocks"`, help "Relation type:
  blocks|parent-child|related-to"), `--from` ("Source issue ID (required)"),
  `--to` ("Target issue ID (required)"), hidden `--by`.
- Refusals, in order:
  1. Blank `--from` or `--to` →
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
- Refusal: `len(positional) != 1` →
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
mechanism `bulk import` duplicated" (`register.go:455`). Because the row is
`skipApp`, the pointer is returned even outside a git repository
(`register.go:209-214`; asserted in `retired_command_test.go:97-…`).

### 2.22 `lit export`

- Registration `register.go:372-373`, `app.AccessRead`. Summary: "Write the backlog
  out as a portable JSON tree (the data-export primitive; `import`'s inverse)".
- Handler `runExport` (`cli.go:1544-1555`): no flags of its own; parses argv (so
  `--help` works and any flag is an unknown-flag `UsageError`); calls
  `Store.Export(ctx)`; writes the result as **two-space-indented JSON** to stdout
  (`cli.go:1554`, `writeJSON` at `cli.go:1830-1834`).
- No positional check.

### 2.23 `lit import --path <file>`

- Registration `register.go:374-375`, `app.AccessWrite`. Summary: "Bulk-create/update
  issues from a file (the one bulk-ingest home): a JSON tree spec, or a YAML file
  for create-or-update by id selector".
- Handler `runImportTree` (`cli.go:1567-1600`).
- Flags: `--path` (string, `""`, "Path to a JSON tree-spec file or a YAML bulk
  create/update file"), hidden `--by` (`cli.go:1569-1570`).
- Refusals: blank `--path` (after trim) →
  `UsageError{importUsage}` → exit 2. `importUsage` verbatim:
  `"usage: lit import --path <tree-spec.json | bulk-file.yaml> (run `lit import --help` for both formats)"`
  (`cli.go:1559`, `cli.go:1574-1579`).
- Reads the file; a read error → `fmt.Errorf("read import spec: %w", err)` → exit 1
  (`cli.go:1580-1583`).
- **Format is selected by the file extension** (lowercased) (`cli.go:1584`):
  - `.yaml` / `.yml` → `runImportBulk`
  - anything else → `runImportTreeJSON`, but first: if `--by` was set →
    `UsageError{"usage: --by only applies to a YAML bulk-update file (--path *.yaml|*.yml); JSON tree-spec import always attributes creates to \"links\""}`
    → exit 2 (`cli.go:1595-1597`).

**JSON tree path** (`runImportTreeJSON`, `cli.go:1617-1635`):
- `storage.ParseImportTreeSpecs(data)` then
  `Store.ImportTree(ctx, workspacePrefix, specs)`.
- Documented spec shape (`cli.go:1610-1616`): an array of records each with
  `local_id`, optional `parent` (a local_id), optional `depends_on` (array of
  local_ids), `title`, `type`, `topic`, `priority`.
- Output: `"imported %d issues\n"` then, per mapping, `"  <local> -> <real>\n"`
  (map iteration order is unspecified) (`cli.go:1626-1633`).
- Best-effort rollback on failure is the store's behavior; the doc comment tells
  the caller to run `lit doctor` after a failed import (`cli.go:1602-1608`).

**YAML bulk path** (`runImportBulk`, `cli.go:1656-1691`):
- `storage.ParseBulkSpecs(data)`.
- If `--by` was set but no document has an `id` (i.e. no update documents) →
  `UsageError{"usage: --by only applies when the file has at least one update document (a document with \`id\` set); this file has none"}`
  → exit 2 (`cli.go:1667-1669`, `bulkSpecsHaveUpdate` at `cli.go:1695-1702`).
- Calls `Store.BulkApply(ctx, prefix, actor, specs)`.
- Documented YAML shape (`cli.go:1641-1655`): one document per issue separated by
  `---`; optional `local_id` for intra-file references; `id` present means
  **update** that issue instead of creating; `parent` may name a local_id or a
  real issue ID.
- Output (`cli.go:1674-1690`):
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
  - `len(positional) != 1` → the same usage `UsageError`
    (`prefix.go:35-37`).
  - `workspace.ConfiguredPrefix(requested)` failure →
    `ValidationError{Message: fmt.Sprintf("invalid prefix %q: %v", requested, err)}` →
    reason `validation_refused`, exit 3 (`prefix.go:44-51`). Typed so a deterministic
    refusal does not reach the unclassified default's retry-then-doctor remediation.
- Three outcomes (`prefixSetTextOutput`, `prefix.go:87-106`):
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

- Registration `register.go:376-379`, workspace-mode. Summary: "Show workspace
  metadata". Handler `runWorkspace` (`cli.go:1704-1727`).
- No flags of its own; parses argv so `--help` works.
- Output: one `key: value` line per field, in this exact order
  (`cli.go:1712-1725`): `workspace_id`, `issue_prefix`, `git_common_dir`,
  `storage_dir`, `database_path`, `dolt_repo_path`, `traces_dir`.

### 2.26 `lit completion <bash|zsh|fish>`

- Registration `register.go:280-281`. Group `guidance`. Summary: "Generate shell
  completion script". Its own advertised subcommands come from
  `completionFamily.visibleSubcommands()`.
- `completionFamily` — usage `"usage: lit completion <bash|zsh|fish>"`, rows
  `bash`, `zsh`, `fish` (`cli.go:1736-1743`).
- `runCompletion(stdout, args)` (`cli.go:1745-1755`): `len(args) != 1` →
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
(`register.go:463-467`). Hidden specs are excluded from root `--help` and from
completion (`register.go:415-421`, `completion.go:20-29`).

| Command | Group | Replacement guidance (verbatim) | Citation |
|---|---|---|---|
| `ready` | operations | "use `lit backlog` for the full ranked queue (blocked items shown inline) or `lit next` for the single leaf to start" | `register.go:296-297`, `:431` |
| `queue` | operations | same as `ready` | `register.go:300-301` |
| `assign` | operations | "reassigning is a field write: use `lit update <id> --assignee <name>` (with an optional `--reason`)" | `register.go:325-326`, `:438` |
| `ls-at` | maintenance | "use `lit ls --at <store-dir>` — listing a discovered store read-only is now a flag on `ls`, not a separate command" | `register.go:371-372`, `:439` |
| `overview` | maintenance | "use `lit stores --counts` — the cross-project ready / in-flight / blocked rollup is now a flag on `stores`" | `register.go:387-388`, `:440` |
| `bulk import` | (bulk family) | "use `lit backup restore --path <export.json>` — it owns the same export-restore mechanism `bulk import` duplicated" | `bulk.go:28`, `register.go:455` |

Full error message form: `the "<command>" command has been retired; <replacement>`
(`cli.go:1977-1979`). Reason `retired_command`, remediation empty
(`error_output.go:54-57`, `:96-100`). Asserted in
`retired_command_test.go:17-52`, `:54-96`, `:134-…`.

Retired **flags** (intercepted by the shared parser, §1.6): `--output` anywhere
(`cli.go:174-179`, `cli.go:286-289`) and `--continue` (`cli.go:290-294`), both
`UnsupportedError`; `lit update --status` (`cli.go:988-990`), a `UsageError`.

### 2.28 `lit quickstart` (in-scope only as it is the bare-`lit` default)

`runQuickstart` (`cli.go:1757-1828`) — flags `--refresh` (bool),
`--eject` (string-optional; present-with-no-value = `"all"`), `--force` (bool),
plus at most one positional topic.
- More than one positional → refused by `refuseSurplusPositionals` (`register.go:342`),
  called from `parseLeaf` (`register.go:307`), before `quickstartLeaf`'s work
  (`cli.go:2028`) runs, using `quickstartUsage`, where
  `quickstartUsage = "usage: lit quickstart [<topics|…>] [--refresh] [--eject[=LIST]] [--force]"`
  built from the topic token list (`quickstart_topics.go:55`).
- `--refresh` with `--eject` → `UsageError{"usage: --refresh and --eject are mutually exclusive"}`
  (`cli.go:1774-1776`).
- `--force` without `--eject` → `UsageError{"usage: --force is only valid with --eject"}`
  (`cli.go:1777-1779`).
- A topic positional combined with any flag →
  `UsageError{"usage: lit quickstart <topic> takes no flags"}` (`cli.go:1779-1782`).
- An unknown topic →
  `UsageError{"usage: unknown quickstart topic \"<x>\" (must be one of: <tokens>)"}`
  (`cli.go:1786-1789`).

---

## PART 3 — CROSS-CUTTING OBSERVATIONS (behavioral, non-editorial)

1. **JSON output exists on exactly one command in this scope**: `lit export`
   (`cli.go:1554`). Every other command emits line-oriented text. `--output` is
   rejected globally and per-command (§1.1, §1.6).
2. **Surplus positionals are refused for every command**: `refuseSurplusPositionals`
   (`register.go:342`), called once from `parseLeaf` (`register.go:307`) before any
   leaf's work runs — `new`, `followup`, `ls`, `rank`, `export`, `children`, and
   `parent clear` included.
3. **Family dispatch errors are plain errors (exit 1), not `UsageError` (exit 2)**
   (`register.go:112-123`), unlike the per-command usage refusals which are
   `UsageError` (exit 2). Likewise `runTransition`'s wrong-arity refusal
   (`cli.go:1395`) and `runCompletion`'s (`cli.go:1747`) are exit 1.
4. **`--help` output goes to stdout, not stderr**, and exits 0
   (`cli.go:265-272`, `cli.go:278-283`, `cli.go:47-49`).
5. **Assignee identity diverges by command on purpose**: `start` resolves through
   `resolveIdentity` (env `CLAUDE_CODE_SESSION_ID` wins) (`cli.go:1306`);
   `update --assignee` writes the trimmed literal, empty meaning clear
   (`cli.go:1027-1037`). `new`/`followup` also write the trimmed literal
   (`cli.go:352`, `cli.go:423`).
6. **Claim state never blocks anything except `lit start` on a fresh foreign
   hold.** `backlog` renders claims as visibility only (`backlog.go:24-25`,
   `:92-96`); `next` routes by claim but never writes (`next_route.go:139-186`);
   `start` is the only gate (`cli.go:1461-1476`, `classifyTakeover` at
   `claims_takeover.go:110-119`).
7. **Three functions panic on unreachable states** and would abort the process:
   `ClassifyReadiness` on an unclassified annotation kind (`readiness.go:144`),
   `renderNextOutcome` on an unhandled outcome type (`next.go:149`),
   `transitionOccasion` on an unmapped status action (`workflow_events.go:110`),
   `emitBreadcrumb`/`quickstartBreadcrumb` on an unknown topic
   (`quickstart_topics.go:64`), `completionRenderer` on an unknown shell
   (`completion.go:54`), `nestUnder` on a missing nest point (`register.go:153`),
   and `store.planLifecycleAction` on an impostor action
   (`internal/store/store.go:1251`).
8. **The `done` "requires in_progress" claim in the registry summary
   (`register.go:332`) has no enforcing code path** — see §2.10.
