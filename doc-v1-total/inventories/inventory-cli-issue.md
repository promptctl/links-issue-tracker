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

- `Run(ctx, stdout, stderr, args)` is the package entry (`internal/cli/cli.go`).
  It first runs `parseGlobalArgs` (`cli.go`), then builds the cobra root
  (`cli.go`), sets args/out/err, sets `SilenceErrors = true` and
  `SilenceUsage = true` (`cli.go`), and executes.
- `pflag.ErrHelp` and the internal `errHelpHandled` sentinel are swallowed and
  converted to a nil return, i.e. exit 0 (`cli.go`; sentinel defined at
  `cli.go`).
- `parseGlobalArgs` (`cli.go`) scans the flag-shaped args before the first
  positional: a literal `--` among them is consumed and scanning stops
  (`cli.go`); a `--output` or `--output=<x>` among them returns
  `unsupportedOutputFlagError()` (`cli.go`); the first token that is not
  flag-shaped stops the scan. Effect: the removed `--output` flag is rejected in
  *global* position before any command runs.
- `unsupportedOutputFlagError()` returns
  `UnsupportedError{Message: "--output is no longer supported; omit it for text output"}`
  (`cli.go`).

### 1.2 Root command

- `Use: "lit"`, `Long: "Agent-native issue tracker"`, `Args: cobra.ArbitraryArgs`,
  `DisableFlagParsing: true` (`cli.go`). The root's RunE parses its own argv
  against its flag set (only `-h/--help`, which the root declares itself so it
  exists before cobra routes) with interspersing off, so flags count as the root's
  only up to the first positional (`cli.go`).
- A positional reaching the root → `UnknownCommandError{Command: <first positional>}`
  (`cli.go`), whether or not a help flag stands before or after it:
  `lit bogus --help`, `lit -h bogus` and `lit bogus --nosuchflag` all refuse
  `bogus`, exit 3.
- A help flag with no positional prints cobra's root `Help()` (`cli.go`).
- Bare `lit` with no args: resolves the workspace from cwd (`cli.go`). If the
  error is `OutsideWorkspaceError` it prints cobra's `Help()` (`cli.go`);
  any other error is returned (`cli.go`); otherwise it renders and prints
  the quickstart guidance — byte-identical to `lit quickstart` (`cli.go`).
- Cobra's default `completion` command is disabled (`cli.go`); cobra's built-in
  `help` command remains.
- Root flag errors are wrapped as `UsageError` so an unknown global flag exits
  `ExitUsage`: the root's own parse does this in RunE, and `SetFlagErrorFunc`
  does it for cobra's `help` command, the one command cobra still parses
  (`cli.go`).

### 1.3 Command registry

- The whole command tree is a table: `commandSpecs(ctx, stdout, stderr) []CommandSpec`
  (`register.go`). Each `CommandSpec` carries `Name`, `Summary`, `Long`,
  `GroupID`, `Run`, `Subcommands`, `Hidden` (`register.go`).
- `applyRegistry` adds every group then every command (`register.go`).
- `buildPassthroughCommand` (`register.go`) creates each cobra command with
  `DisableFlagParsing: true` and `Args: cobra.ArbitraryArgs`. **Consequence:** cobra
  does not parse any per-command flags; each handler parses its own argv slice.
  `CommandSpec` carries no `Long`: because cobra parses no flags here, it cannot
  render a command's page either, so `lit help <cmd>` is rewritten in argv to
  `lit <cmd> --help` (`rewriteHelpCommand`, `cli.go`) and the leaf renders both the
  description and the real flag table. Descriptions are embedded under
  `internal/cli/helptext/`; `init`'s is `helptext/init.txt`.
- Help groups, in order (`register.go`):
  `bootstrap` "Human Bootstrap", `operations` "Agent Operations",
  `structure` "Dependencies & Structure", `data` "Sync & Data",
  `maintenance` "Setup & Maintenance", `retention` "Issue Retention",
  `guidance` "Guidance & Tooling".

### 1.4 Wrappers: how a handler gets a store

- `r.appCmd(access, fn)` → `runWithApp` with a fixed access mode
  (`register.go`); `r.appCmdDynamic` computes the mode from argv
  (`register.go`).
- `r.wsCmd(fn)` → `acquireFromWD` (workspace metadata only, no store)
  (`register.go`).
- `r.familyCmd(family)` resolves `args[0]` against the family table *before*
  opening anything; a row carrying `retired` returns that error before any
  parse or workspace open, and every other row parses its leaf, then opens the
  app in the row's access mode (`register.go`).
- `r.wsFamilyCmd(family)` same, but workspace-mode (`register.go`).
- `r.transitionCmd(spec)` → `transitionLeaf(spec)` under `app.AccessWrite`
  (`register.go`).
- `runWithApp` (`cli.go`):
  - `os.Getwd()` failure → `fmt.Errorf("get cwd: %w", err)` (`cli.go`).
  - `app.Open` failing with `workspace.ErrNotGitRepo` →
    `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}`
    (`cli.go`).
  - The handler runs with a deferred `ap.Close()` (`cli.go`).
  - **After a successful write command** (`accessMode == app.AccessWrite`), the
    mutation sync-staleness warning is printed at this one seam
    (`cli.go`, `printMutationSyncStalenessWarning` at
    `sync_staleness.go`).
  - Then `maybeAutoSyncAfterCommand(ctx, accessMode, ws)` runs (`cli.go`).
  - Both only run when the command returned nil (`cli.go`).
- `acquireFromWD` / `resolveWorkspaceFromWD` (`register.go`, `cli.go`):
  same `OutsideWorkspaceError` translation (`cli.go`).

### 1.5 Family dispatch (`commandFamily[P]`)

- `resolve(args)` (`register.go`): with **zero args**, or an args[0] that
  matches no row, it returns `errors.New(f.usage)` — a plain error, **not** a
  `UsageError`, so the exit code is `ExitGeneric` (1), not 2. Matching is exact
  string equality; no trimming.
- `visibleSubcommands()` drops `hidden` rows for the completion projection
  (`register.go`).
- `nestUnder(subs, name, children)` panics if `name` is absent
  (`register.go`).

### 1.6 Per-command flag parsing (`cobraFlagSet`)

- `newCobraFlagSet(use)` builds a throwaway cobra command with a default `--help`
  flag installed and all output discarded (`cli.go`).
- Registration helpers: `String`, `Bool`, `Int` (`cli.go`), `StringArray`
  (repeatable, never comma-split — `cli.go`), `StringOptional`
  (`--flag` with no value takes `defaultIfPresent`, absent takes
  `defaultIfAbsent` — `cli.go`), `Hide(name)` marks a flag hidden but
  functional (`cli.go`).
- `parseFlagSet(fs, args, stdout)` (`flagset.go`) is the single parse boundary:
  - On `pflag.ErrHelp` it prints `"Usage of <use>:\n"` followed by
    `PrintDefaults()` **to stdout** and returns `errHelpHandled` → exit 0
    (`cli.go`, printer at `cli.go`).
  - A `*pflag.NotExistError` whose parsed name is `continue` (from `--continue`
    or `--continue=<x>`; not `--continuex`, and not `-continue`, which pflag
    reads as the shorthand group `-c…`) →
    `UnsupportedError{Message: "--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag"}`
    → exit 3 (`flagset.go`).
  - Every other parse error — unknown flag, missing value, invalid value, bad
    syntax — → `UsageError{Message: err.Error()}` → exit 2 (`flagset.go`).
    A leaf-position `--output` is an ordinary unknown flag here; only
    `parseGlobalArgs` maps `--output` to `UnsupportedError`.
  - A parsed-and-changed `--help` flag also prints help and returns
    `errHelpHandled` (`cli.go`).

### 1.7 Positional/flag splitting (`splitArgs`)

`splitArgs(args []string, positionalCount int, fs *cobraFlagSet)`
(`flagset.go`):
- Any token starting with `-` goes to the flag slice; it consumes the *next*
  token as its value only when the flag set says that flag takes one —
  `flagTakesValue` (`flagset.go`) resolves the token against the command's
  flags and applies `takesValue` (`flagset.go`), which is pflag's own rule:
  an empty `NoOptDefVal` means the flag consumes the following token whatever
  that token looks like.
- A `--` terminator ends flag scanning: every token after it is a positional
  whatever it looks like, up to `positionalCount`; tokens past that ceiling stay
  in the flag stream and are refused as surplus.
- The first `positionalCount` non-flag tokens become positionals; any extra
  non-flag tokens are appended to the **flag** slice, where
  `refuseSurplusPositionals` (`register.go`) refuses them.

### 1.8 Exit-code taxonomy

Constants (`exit.go`):

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

`ExitCode(err)` dispatches by `errors.As`, in this order (`exit.go`):
1. `storage.NotFoundError` → 4 (`exit.go`)
2. `MergeConflictError` → 5 (`exit.go`)
3. `SyncFailureError` → 5 (`exit.go`)
4. `templateShapeError` → 3 (`exit.go`)
5. `ownerApprovalRefusalError` → 5 (`exit.go`)
6. `CorruptionError` → 7 (`exit.go`)
7. `UsageError` → 2 (`exit.go`)
8. `UnknownCommandError` → 3 (`exit.go`)
9. `RetiredCommandError` → 3 (`exit.go`)
10. `model.ValidationError` → 3 (`exit.go`)
11. `model.ContainerActionError` → 6 when `Satisfied()`, else 3 (`exit.go`)
12. `UnsupportedError` → 3 (`exit.go`)
13. `takeoverUnconfirmedError` → 3 (`exit.go`)
14. `Exhausted` → 6 (`exit.go`)
15. `NoWork` → 6 (`exit.go`)
16. `OutsideWorkspaceError` → 3 (`exit.go`)
17. `errors.Is(err, store.ErrWorkspaceNotInitialized)` → 3 (`exit.go`)
18. `errors.Is(err, workspace.ErrIssuePrefixRefused)` → 3 (`exit.go`)
19. `BulkFailureError` → 1 (`exit.go`)
20. `errors.Is(err, store.ErrTransientGCContention)` → 1 (`exit.go`)
21. anything else → 1 (`exit.go`)

Error types defined in `cli.go`: `MergeConflictError` (`cli.go`),
`CorruptionError` (`cli.go`), `UsageError` (`cli.go`),
`UnknownCommandError` — message `unknown command "<x>"` (`cli.go`),
`UnsupportedError` with a single `Message`
field (`errors.go`), `RetiredCommandError` — message
`the "<cmd>" command has been retired; <replacement>` (`cli.go`),
`OutsideWorkspaceError` (`cli.go`). `BulkFailureError` in
`bulk.go`. Value refusals use `model.ValidationError`, defined beside the
rules in `internal/model/validation.go`.

### 1.9 Error output convention

`WriteCommandError(stderr, err)` (`error_output.go`) — called from
`cmd/lit/main.go` as the process exit path:
- Line 1: `error (code=%d): %v\n` (exit code + `err.Error()`).
- Line 2 (only when non-empty): `remediation: %s\n`.

`commandErrorReason(err)` maps type → reason string (`error_output.go`):
`entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`,
`corruption_detected`, `unknown_command`, `retired_command`, `usage_error`,
`unsupported_flag` (every `UnsupportedError`, `error_output.go`),
`takeover_unconfirmed` (`takeoverUnconfirmedError`), `outside_git_workspace`, `bulk_partial_failure`, `workspace_write_blocked`,
`transient_gc_contention`, `workspace_not_initialized`, default `command_failed`.

`commandErrorRemediation(reason)` (`error_output.go`), verbatim strings:
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
- `takeover_unconfirmed`: "Rerun with `--take` to take the lane over, or run `lit next` for work nobody else holds. \<agent-instructions>Taking over a lane another checkout holds right now overrides that checkout's work: pass `--take` only when the user directs the takeover.\</agent-instructions>"
- `outside_git_workspace`: "Run the command inside a git repository/worktree with links initialized."
- `workspace_not_initialized`: "Do not retry unchanged — this repository has no lit workspace, and retrying this command cannot create one. Run `lit init` here to create it, or change to a directory that already has one."
- `bulk_partial_failure`: "Some items failed; see the per-item errors above. Re-run the command for only the failed IDs after addressing each error."
- default: "Retry the command. If it still fails, run `lit doctor` for diagnostics."

### 1.10 Progress channel

`progressf(operation, format, args...)` writes `lit: <operation>: <text>\n` to
`progressOut`, which is `os.Stderr` (`progress.go`).
Stdout stays the result channel.

### 1.11 Identity resolution (assignee / actor)

- `resolveIdentity(explicit)` (`cli.go`): if env
  `CLAUDE_CODE_SESSION_ID` is non-empty (after trim), the identity is
  `"claude_" + sessionID`, **overriding any explicit value**; otherwise the
  trimmed explicit value.
- `registerActor(fs)` (`cli.go`) declares a **hidden** `--by` string
  flag (default `""`, empty usage string) and returns a resolver closure. The raw
  flag value is never exposed; `--by` does not appear in help output
  (`cli.go`).
- Empty actor is normalized by the store to `"unknown"`
  (`internal/store/store.go`).
- A claimant with no assignee is named by its checkout: `describeClaimant` returns `nameCheckout(c.Checkout)` when `c.Assignee == ""` (`claims_render.go`).

Commands that register `--by`: `update` (`cli.go`), every transition via
`transitionLeaf` (`cli.go`), `comment add` (`cli.go`), `import`
(`cli.go`), `dep add` (`dependency.go`), `label add`
(`issue_relations.go`), `parent set` (`issue_relations.go`), `bulk label`
(`bulk.go`), `bulk close` (`bulk.go`), `bulk archive` (`bulk.go`).
`next` registers it too (`next.go`) and is the only read command that does:
it writes nothing, but its output names an identity, so it resolves the reader
by the same rule every writer resolves the actor.

### 1.12 Success-output breadcrumb

`emitBreadcrumb(w, token)` prints `deeper guidance: lit quickstart <token>`
(`quickstart_topics.go`). It **panics** if the token is not a registered
quickstart topic (`quickstart_topics.go`).

Breadcrumbs are emitted by: `new` → `"new"` (`cli.go`), `followup` → `"new"`
(`cli.go`), `update` → `"update"` (`cli.go`), `rank` → `"update"`
(`cli.go`), `rank set` → `"update"` (`cli.go`), `dep add`/`dep rm` →
`"update"` (`dependency.go`), `label add`/`label rm` →
`"update"` (`issue_relations.go`), `parent set`/
`parent clear` → `"update"` (`issue_relations.go`),
and transitions via the table below.

`transitionBreadcrumbTopics` (`cli.go`): `start` → `"work"`,
`done` → `"done"`, `close` → `"done"`. Absent for `open`, `archive`,
`unarchive`, `delete`, `restore` → no breadcrumb (`cli.go`).

### 1.13 Sync-staleness banners

- Read commands print `printStalenessWarning(ctx, w, ws, store, now)` FIRST,
  before their payload: `backlog` (`workable.go`), `next` (`next.go`),
  `show` **only in full-detail mode** (`cli.go`) — deliberately suppressed
  under `--field` so the machine-parseable output isn't corrupted.
  Defined at `sync_staleness.go`.
- Write commands get `printMutationSyncStalenessWarning(stdout, ws, now)` after
  the handler succeeds and after the engine closes (`cli.go`,
  `sync_staleness.go`).

### 1.14 Workflow event dispatch

Every dispatch is `workflows.Dispatch(stdout, os.Stderr, ap.Workspace, occasion)`.
Occasion builders (`workflow_events.go`):
- `showTicketOccasion` → `EventShowTicket`, carries IssueID + Labels
- `backlogOccasion` → `EventShowBacklog`, no IssueID/Labels
- `nextPulledOccasion` → `EventNextPulled`
- `ticketCreatedOccasion` → `EventTicketCreated`
- `ticketUpdatedOccasion` → `EventTicketUpdated`; never carries Entered/Exited
  because `update` rejects `--status`
- `commentAddedOccasion` → `EventCommentAdded`
- `transitionOccasion(action, prior, issue)` → looks up
  `statusTransitionEvents` (`start`→`EventWorkStarted`, `done`→`EventWorkFinished`,
  `close`→`EventTicketClosed`, `open`→`EventTicketReopened`), sets
  `Entered = issue.State()`, `Exited = prior.State()`; **panics** on an unmapped
  action name.

Retention actions (archive/unarchive/delete/restore) are not `StatusAction`s and
fire no event (`cli.go`).

### 1.15 Prefix / ID resolution

There is **no fuzzy or short-prefix ID resolution anywhere in `internal/cli`.**
Issue IDs are passed verbatim to the store (e.g. `cli.go`,
`cli.go`). A wrong ID yields `storage.NotFoundError` → exit 4.

The word "prefix" in this codebase means the *cosmetic ID prefix* on new IDs
(`lit prefix`, §2.24) — `ap.Workspace.IssuePrefix.Value()` is passed into
`CreateIssue` (`cli.go`) and `ImportTree`/`BulkApply`
(`cli.go`).

The only "does the literal look like a subcommand" disambiguation is in `rank`:
`args[0] == "set"` routes to `rank set`, justified because real IDs always carry a
prefix (`cli.go`).

### 1.16 Output rendering primitives (`output.go`)

- `contextIndent` = four spaces (`output.go`).
- `historyTimestampLayout` = `"Jan 2, 2006 3:04 PM MST"` (`output.go`),
  rendered in **local** time (`output.go`).
- `printIssueSummary` — the one-line success form used by `new`, `followup`,
  `update`, `rank`, and every transition:
  `"%s [%s/%s/%s/%s] %s%s\n"` = `id [state/type/topic/priority] title[labels]`
  (`output.go`). Labels render as `" [a,b]"` or empty (`output.go`).
- `formatIssueState(issue)` = `issue.State()`, then `":" + resolution` when
  the close recorded one (`resolutionSuffix`, shared with `issueStanding`),
  then `"+archived"` or `"+deleted"` when retention says so — never both
  (`output.go`). The list `state` column renders this cell (`columns.go`).
- `defaultColumns()` default column set = `id, state, topic, title`
  (`columns.go`). Valid column names: `id, state, type, topic, priority,
  title, rank, assignee, labels, updated_at, created_at, parent, blocked`
  (`columns.go`). An unknown name is a `UsageError` naming it and the valid
  set; an empty selection gives the default set (`columns.go`).
- `formatIssueColumns` renders each selected column through its registry entry
  (`output.go`); the per-column renderers live in `columnRegistry`
  (`columns.go`): `priority` uses
  `Priority.String()` (normal/urgent); `assignee` and `labels` render `-` when
  empty; `updated_at`/`created_at` render RFC3339; `parent` renders the parent id
  or `-`; `blocked` renders the literal token `blocked` or `-`
  (`blockedLabel`, `output.go`).
- `printIssueLines` joins columns with `" | "` (`output.go`).
- `printIssueTable` writes an UPPERCASED tab-joined header then rows through a
  `tabwriter` (minwidth 2, tabwidth 2, padding 2, space pad) (`output.go`).
- `emptyDash(s)` → `"-"` when blank (`output.go`).
- `printLabels` prints labels comma-joined on one line (`output.go`).
- `humanizeCoarseDuration` buckets: ≥48h → `"%d days"`, ≥2h → `"%d hours"`,
  ≥2m → `"%d minutes"`, else `"under a minute"` (`output.go`).
- `indentLines(s, prefix)` prefixes every line, trailing newlines stripped
  (`output.go`).
- `writeJSON(w, v)` uses `json.Encoder` with two-space indent (`cli.go`).
  **`export` is the only command in this scope that emits JSON** (`cli.go`).
  There is no `--json` / `--output` mode anywhere; `--output` is explicitly
  rejected (§1.1, §1.6).

### 1.17 Vocabularies (sealed sets used by flags)

- Issue types: `task, feature, bug, chore, epic` (`internal/model/issue_type.go`).
  `ParseIssueType` lowercases and trims; error text
  `"issue type must be <oxford-or list>"` (`issue_type.go`).
  `epic` is the only container type (`issue_type.go`).
- Priorities: `0` normal, `1` urgent (`internal/model/priority.go`), both
  spellings held in one `priorityVocabulary` table (`priority.go`).
  `ParsePriority` gates the int payloads (`priority.go`) and
  `ParsePriorityName` gates the `--priority` flag, the latter
  lowercasing and trimming, then taking the display word or the decimal. Both
  reject anything else with the shared
  `"priority must be normal (0) or urgent (1)"` (`priority.go`).
- States: `open, in_progress, closed`; `in-progress` is normalized to
  `in_progress`; error `invalid status "<x>" (valid: open, in_progress, closed)`
  (`internal/model/lifecycle/lifecycle.go`).
- Resolutions: `duplicate, superseded, obsolete, wontfix`; error
  `"resolution must be one of: duplicate, superseded, obsolete, wontfix"`
  (`internal/model/lifecycle/resolution.go`).
- Relation types: `blocks, parent-child, related-to`; error
  `"relation type must be blocks, parent-child, or related-to"`
  (`internal/model/relation_type.go`).
- The write-path `--type` and `--priority` flags call `model.ParseIssueType` and
  `model.ParsePriorityName` directly; both refuse with `model.ValidationError` →
  exit 3 and the `validation_refused` remediation. `--priority` is a string flag
  so the word form reaches `ParsePriorityName` — a pflag `ParseInt` refused it with
  a bare error that fell through to the default "Retry the command"
  (links-cli-bvko).
  The read path (`lit ls`) parses `--type` with `model.ParseIssueTypes`
  (`internal/model/issue_type.go`) and `--status` with `model.ParseStates`
  (`internal/model/lifecycle/lifecycle.go`), and wraps either failure in
  `model.ValidationError` → exit 3 (`listLeaf`, `cli.go`).
- `issueTypeChoices()` renders `task|feature|bug|chore|epic` into flag help
  (`cli.go`), and `priorityChoices()` renders `normal|urgent` the same
  way — into both the `--priority` help string and the `followup`
  and `update` usage lines, so neither can drift from the parse gate.
- `splitCSV` splits on `,`, trims each part, drops empties, returns nil for a
  blank input (`cli.go`).

### 1.18 Readiness / workability — the exact predicate

**Step 1 — candidate set** (`classifyWorkable`, `cli.go`):
`ListIssues` with `Statuses = [open, in_progress]` (or the single `--status`
value if given), `IssueTypes`/`Assignees`/`LabelsAll` from the CLI filter,
`IncludeArchived=false`, `IncludeDeleted=false`, `Limit=0` (`cli.go`).
The store's default ordering is `item_rank ASC` (`cli.go`).

**Step 2 — leaves only**: `filterWorkableIssues` keeps issues whose
`Capabilities().Status != nil` (i.e. leaves, not containers) and whose status is
not `closed` (`cli.go`). Epics are therefore never workable rows.

**Step 3 — annotators** applied via `annotation.Annotate` (`cli.go`):
1. `newFieldAnnotator(requiredFields)` — from `config.Load(...).Ready.RequiredFields`
   (`cli.go`). Empty policy → no-op annotator (`ready_state.go`).
   A required field name not present in `model.IssueWireFields()` →
   `ValidationError{"required field %q does not exist on issue"}`
   (`ready_state.go`). For each unset required field it emits a
   `MissingField` annotation (`ready_state.go`). "Set" means: non-nil, and
   for strings non-blank, for arrays/maps non-empty; anything else counts as set
   (`isRequiredFieldSet`, `ready_state.go`).
2. `newBlockerAnnotator(details, ancestry)` — for each `DependsOn` that is
   `InPlay()`, sorted by ID, emits `OpenDependency{Message: dep.ID}`; and
   additionally `RankInversion{Message: dep.ID}` when `dep.Rank > issue.Rank`.
   Then, for each id from `ancestry.inheritedDependencies(detail)` not already
   a direct dependency, emits `InheritedDependency{Message: dep.ID}` and no rank
   inversion (`ready_state.go`). `ancestry` is a `heldAncestry`
, built by `fetchHeldAncestry` from an
   `epicAncestry`: the relations of every epic above the issues, keyed by epic
   id, and each epic's gates. `fetchContainerAncestry` builds both,
   a gate being any `InPlay()` `DependsOn` of a loaded epic.
   `climbContainers` loads `parentEpicIDs` of the subjects, then of each loaded
   level, one fetch per level, until a level names no epic it has not loaded
. `epicsAbove` yields the container parents of an issue, nearest
   first, and stops at a parent it has already yielded.
   `epicAncestry.inheritedDependencies` returns the gates of every epic
   `epicsAbove` the subject yields, other than the subject itself, deduplicated,
   sorted by ID.
   `fetchHeldAncestry` then loads the wait graph once with `fetchWaitGraph`: a
   breadth-first walk of `fetchWaitLinks` from every gate over links that hold,
   keyed by waiter. `settleWaits` computes, in memory, the ids
   each gate waits on, and each blocker the graph's inherited links name
. An inherited link holds unless its blocker waits on its waiter,
   so the closures are the alternating fixpoint: `upper` starts as the closure
   over every link; `lower` is the closure over the links `upper` lets hold, the
   next `upper` the closure over the links `lower` lets hold, until `upper` is
   unchanged, which is returned. `heldAncestry.inheritedDependencies` drops each
   gate whose closure contains the subject, so a blocker never
   holds back an issue it waits on, itself included.
3. `newSiblingGateAnnotator(details, pendingSiblingsByEpic(held.ancestry.relations))` —
   only when the parent exists and `parent.IsContainer()`; emits
   `EarlierSiblingPending{Message: sib.ID}` for each sibling satisfying
   `isEarlierSameLaneSibling` (`ready_state.go`).
   `isEarlierSameLaneSibling(sib, leaf) := sib.ID != leaf.ID && sib.Lane == leaf.Lane && sib.Rank < leaf.Rank`
   (`ready_state.go`). The sibling set is the epic's **unfiltered**
   `InPlay()` children (`pendingSiblingsByEpic`, `ready_state.go`), fetched
   via `fetchHeldAncestry(ctx, memo, details)` (`cli.go`), so siblings
   hidden by `--assignee/--type/--labels` still gate.
4. `newOrphanedAnnotator(orphanedThreshold)` — only for `in_progress` issues with
   `time.Since(UpdatedAt) >= 6h`; message
   `"in_progress for <dur truncated to minute> with no update"`
   (`ready_state.go`; threshold constant `orphanedThreshold = 6 * time.Hour`
   at `ready_state.go`).
5. `newNeedsDesignAnnotator()` — emits `NeedsDesign` for any issue carrying the
   label `needs-design` (`ready_state.go`).
6. `newFocusPathAnnotator(focusPaths)` — emits `FocusPath{Message: goalID}` for
   issues on a focused goal's prerequisite closure (`ready_state.go`).

**Focus path derivation** (`fetchFocusPathGoals`, `ready_state.go`):
goals are issues with `Statuses=[open,in_progress]` and label `focus`
(`FocusLabel = "focus"`, `ready_state.go`). BFS over the
prerequisite DAG, one `fetchWaitLinks` expansion per level. The
`path` map doubles as the visited set, so shared prerequisites attribute to the
first goal reached and cycles terminate. Relations are memoized through
`memoizeRelations` over `relationsByID`, primed with the
seeds; `annotateIssues` passes the memo it built for `fetchHeldAncestry`.

**Wait links** (`fetchWaitLinks`, `ready_state.go`): for each frontier
issue, in frontier order, a `waitLink{waiter, prereq, holds, kind}` for
each `InPlay()` `DependsOn` (`waitDependency`), each `epicAncestry.inheritedDependencies` entry over
the frontier's `fetchContainerAncestry` (every gate, before `heldAncestry` drops
any; `waitInherited`), each `InPlay()` child of a container (`waitChild`), and each earlier same-lane
`InPlay()` sibling under a container parent (`waitEarlierSibling`). `holds` is true for a child link,
and for any other link only when the waiter is not a container. A frontier id
missing from the fetch → `storage.NotFoundError`. The focus walk
follows every link; `fetchWaitGraph` keeps only links that hold.

**Step 4 — readiness classification** (`ClassifyReadiness`, `readiness.go`):
each annotation is dispatched on its declared `ReadinessRole`:
- `RoleBlocking` → appended to `blocking`
- `RoleOrphaned` → sets `orphaned = true`
- `RoleRankInversion` → appended to `rankInversions`
- `RoleNone` → contributes nothing (this is where `FocusPath` lands)
- anything else → **panics** with
  `"ClassifyReadiness: annotation carries an unclassified kind: <kind>"`
  (`readiness.go`).

`IsReady() := len(blocking) == 0` (`readiness.go`). So an issue is **ready**
iff it has no `MissingField`, no `OpenDependency`, no `InheritedDependency`, no
`EarlierSiblingPending`, and no `NeedsDesign` annotation. `DependencyIDs()`
returns the details of the `OpenDependency` and `InheritedDependency` reasons,
and `DependencyLabels()` the same list with ` (via epic)` appended to each
inherited one, which the backlog and `lit next` print on their `depends on:`
lines (`readiness.go`).

**Step 5 — canonical ordering**, applied in this sequence (`cli.go`):
1. `sortByCompositeRank(rows, details)` — stable sort by
   (effective epic rank, own rank); a leaf whose parent is a container uses the
   parent's rank as its epic-position, otherwise its own rank
   (`ready_state.go`).
2. `sortByPriority` — stable, urgent (higher `Priority`) first
   (`ready_state.go`).
Then `enrichWithParentEpic` sets `ParentEpic{ID,Title}` on rows whose parent is a
container (`ready_state.go`). Focus does not reorder rows: the `FocusPath`
annotation is read as a scope (`focusScope.holds`, `ready_state.go`), never as
a sort key.

**Partition used by rollups**: `partitionWorkable` (`ready_state.go`):
`in_progress` state wins first (even if also blocked); else not-ready → blocked;
else ready.

`applyLimit(issues, limit)` truncates when `limit > 0` (`ready_state.go`).

---

## PART 2 — COMMANDS

### 2.1 `lit new` — Create an issue

- Registration: `{Name: "new", Summary: "Create an issue", GroupID: "operations"}`,
  `app.AccessWrite` (`register.go`). Handler `newLeaf` (`cli.go`).
- Flags (`cli.go`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--title` | string | `""` | Issue title. Help: "Issue title" |
| `--description` | string | `""` | "Issue description" |
| `--prompt` | string | `""` | "Reusable agent prompt for the work this issue captures" |
| `--type` | string | `task` | "Issue type: task\|feature\|bug\|chore\|epic" |
| `--topic` | string | `""` | "Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')" |
| `--parent` | string | `""` | "Optional parent issue ID; child IDs become parentID.\<n>" |
| `--priority` | string | `normal` | "Priority: normal\|urgent", the choice list derived from `model.Priorities()` via `priorityChoices()`. A string, not an int, deliberately: an `fs.Int` let pflag's `strconv.ParseInt` refuse the display word before lit's own gate ran (links-cli-bvko). The decimal spelling is still accepted, by `ParsePriorityName` rather than by pflag |
| `--assignee` | string | `""` | "Assignee" (trimmed, `cli.go`) |
| `--labels` | string | `""` | "Comma-separated labels" (split by `splitCSV`) |
| `--lane` | string | `""` | "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes" |
| `--top` | bool | `false` | "Promote the new issue to the top of its frame (the default appends it to the bottom)" |
| `--by` | string (hidden) | `""` | actor fallback (§1.11) — *note*: `newLeaf` registers no actor; `CreatedBy` is not set from the CLI here |

- `--top` maps to `storage.RankTop`; unflagged uses the zero `RankPlacement`
  (`rankPlacement`, `cli.go`).
- **Surplus positionals refused before `newLeaf` runs**: `refuseSurplusPositionals`
  (`register.go`), called from `parseLeaf` (`register.go`).
- Validation order: `--type` then `--priority`, both `ValidationError` → exit 3
  (`cli.go`).
- Store refusals (surfaced through `CreateIssue`, `internal/store/store.go`),
  each a `model.ValidationError` → exit 3:
  blank title → `"title is required"` (`store.go`); blank/short/long topic
  → `"topic is required"` / `"topic must be at least N characters after
  normalization"` / `"topic must be at most N characters after normalization"`
  (`internal/issueid/slug.go`); a `--parent` id that does not exist → a
  not-found error (`store.go`). Labels are canonicalized, de-duplicated and
  sorted (`internal/store/labels.go`).
- Side effects: creates the issue; dispatches `EventTicketCreated`
  (`cli.go`); the store commit is Dolt-side.
- Output: `printIssueSummary` line then the `deeper guidance: lit quickstart new`
  breadcrumb (`cli.go`). Plus the mutation staleness banner from
  `runWithApp` (§1.13).

### 2.2 `lit followup` — File a follow-up parented to a just-closed ticket

- Registration `register.go`, `app.AccessWrite`. Handler `followupLeaf`
  (`cli.go`).
- Flags (`cli.go`): `--on` (string, `""`, "Required parent issue ID
  (typically the just-closed ticket)"), `--title` (required), `--description`,
  `--prompt`, `--type` (default `task`), `--topic`, `--priority` (default 0),
  `--assignee`, `--labels`, `--top`. No `--parent`, no `--lane`.
- Refusal: blank `--on` or blank `--title` (after trim) →
  `UsageError{"usage: lit followup --on <id> --title <text> [--description <text>] [--topic <slug>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--top]"}`
  → exit 2 (`cli.go`).
- Reads the parent via `GetIssue(parentID)`; a missing parent is not-found → exit 4
  (`cli.go`).
- Defaults derived from the parent: blank `--topic` inherits `parent.Topic`
  (`cli.go`); blank `--description` becomes
  `"Follow-up surfaced at the close of <parent.ID>: <parent.Title>"`
  (`cli.go`).
- Creates with `ParentID = parent.ID` (`cli.go`), dispatches
  `EventTicketCreated`, prints the summary line, emits breadcrumb `new`
  (`cli.go`).
- Surplus positionals refused by `refuseSurplusPositionals` (`register.go`),
  called from `parseLeaf` (`register.go`), before `followupLeaf` runs.

### 2.3 `lit ls` — List issues

- Registration `register.go`. **Not** wrapped by `appCmd`; the raw runner
  is `runList(ctx, stdout, lsSurface, args)` so `--at` can target a foreign store
  outside the current workspace (`register.go`, `cli.go`).
  `lsSurface` is `listSurface{name: "ls"}`, a surface with no positionals
  (`cli.go`); `lit children` runs the same `runList` over
  `childrenSurface` (§2.16).
- Summary text: "List issues (rank by default; --at \<store-dir> lists a discovered
  store read-only)" (`register.go`).

**Store routing** (`runList`, `cli.go`):
- `runList` builds the leaf and the `--at` value pointer with `listLeaf(surface)`
  (`cli.go`), parses argv with `parseLeaf` (`cli.go`), then reads the
  positionals with `listPositionals` (`cli.go`). Every step below runs
  after the parse, so `lit ls --help` and a malformed flag are answered before any
  store opens. pflag owns the flag grammar: a bare `--` ends flag parsing, so a
  later `--at` is a positional, and a trailing `--at` with no value is refused by
  the parse as a `UsageError` → exit 2 (`flagset.go`).
- Presence of `--at` is `l.fs.Changed(lsAtFlag)`, where `lsAtFlag = "at"`
  (`cli.go`).
- If `--at` is present and its value is blank-after-trim or starts with `-` →
  `UsageError{"usage: lit ls --at <store-dir>  (a storage directory from `lit stores`)"}`
  (`cli.go`); the command name in the message is `surface.name`.
- Otherwise opens `app.OpenLocationForRead(ctx, workspace.LocationFromStorageDir(atDir))`;
  a failure becomes `fmt.Errorf("open store at %q read-only: %w", atDir, …)`,
  wrapping the open error marked with holder contention for that location
  (`cli.go`). The store is closed on return (`cli.go`). The work runs
  with `noReadyPolicy`, which returns no required fields (`cli.go`).
- With no `--at`, opens the cwd workspace store through `runWithApp` with
  `app.AccessRead`, and the work runs with `workspaceReadyPolicy(ap)`
  (`cli.go`).

**Flags** (declared in `listLeaf`, `cli.go`; read in its `work` closure,
`cli.go`). "Only if visited" means the flag appears on the command line
(`fs.Visit`, `cli.go`).

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--at` | string | `""` | Declared so the parse accepts it (`cli.go`); `listLeaf` returns its value pointer to `runList` (`cli.go`), which routes on it. The work closure does not read it |
| `--status` | string array | `nil` | State set via `model.ParseStates` — comma-separated and/or repeated, every fragment parsed; refusal → `ValidationError{"parse --status: " + err}` → exit 3 (`cli.go`) |
| `--type` | string array | `nil` | Type set via `model.ParseIssueTypes` — comma-separated and/or repeated, every fragment parsed, so a blank or unknown member is refused and named; refusal → `ValidationError{"parse --type: " + err}` → exit 3 (`cli.go`) |
| `--assignee` | string | `""` | Trimmed, single-element `Assignees`; blank → none (`cli.go`) |
| `--search` | string | `""` | Trimmed and appended to `SearchTerms` **only if visited** (`cli.go`) |
| `--ids` | string array | `nil` | Issue ids, comma-separated and/or repeated, read by `storage.ParseNames` exactly as `--parent` is → `filter.IDs`; a blank slot (`--ids=`, `--ids a,`) → `model.ValidationError{"--ids needs an issue id in every slot, e.g. --ids <issue-id>"}` → exit 3 (`cli.go`; `internal/storage/selects.go`) |
| `--parent` | string array | `nil` | Issue ids, comma-separated and/or repeated; `storage.ParseNames` splits each occurrence on commas and collects the ids of all occurrences, then the surface's positionals are appended → `filter.ParentIDs` (direct children, ORed). A blank slot (`--parent=`, `--parent ", "`, `--parent a,`) → `model.ValidationError{"--parent needs an issue id in every slot, e.g. --parent <epic-id>"}` → exit 3, checked before the positionals are appended, so `lit children <id> --parent=` is refused too. An id naming no issue → `NotFoundError` from the store → exit 4 (`cli.go`; `store.go`) |
| `--labels` | string array | `nil` | Labels, comma-separated and/or repeated, read by `storage.ParseNames` → `LabelsAll` (ALL must match, so each added label narrows); a blank slot → `model.ValidationError{"--labels needs a label in every slot, e.g. --labels <label>"}` → exit 3 (`cli.go`) |
| `--has-comments` | bool | `false` | Only if visited; sets the pointer to the flag's value — so `--has-comments=false` filters to issues *without* comments (`cli.go`) |
| `--include-archived` | bool | `false` | `filter.IncludeArchived` (`cli.go`) |
| `--include-deleted` | bool | `false` | `filter.IncludeDeleted` (`cli.go`) |
| `--updated-after` | string | `""` | Only if visited; read by `query.ParseTimestamp`; its `model.ValidationError` wrapped `parse --updated-after: %w` → exit 3 (`cli.go`) |
| `--updated-before` | string | `""` | Only if visited; read by `query.ParseTimestamp`; its `model.ValidationError` wrapped `parse --updated-before: %w` → exit 3 (`cli.go`) |
| `--query` | string | `""` | Query language (see below). A blank query parses to an empty filter, so `Parse` and `Merge` run on every listing and a flag-only filter meets the same whole-filter rules (`cli.go`) |
| `--sort` | string | `""` | `storage.ParseSortSpecs`, applied when non-blank after trim (`cli.go`) |
| `--columns` | string | `""` | CSV of column names, lowercased, via `parseColumnSelection` (`cli.go`, `columns.go`) |
| `--format` | string | `lines` | `lines` or `table` via `parseListFormat` (`cli.go`) |
| `--limit` | int | `0` | `filter.Limit`, read by `query.ParseLimit` before the query merge; a negative value → `limit must be non-negative, got %d` wrapped `parse --limit: %w` → exit 3 (`cli.go`, `query.go`) |

- Flag help for `--query` (verbatim): "Query language: status:closed,in_progress
  resolution:wontfix type:task has:comments sort:rank:asc limit:5 archived deleted
  text" (`cli.go`).
- `--sort` help: "Sort fields, e.g. rank:asc,updated_at:desc" (`cli.go`).
  `ParseSortSpecs` splits on `,`, skips blank fragments, then reads
  `field[:asc|desc]` (a bare field is ascending); an unrecognized direction →
  `model.ValidationError{"unsupported sort direction %q"}` → exit 3
  (`internal/storage/sort.go`).
- `--columns` help: "Comma-separated output columns: " followed by the sorted
  registry names (`cli.go`, `columns.go`). An empty selection (or one
  holding only commas) is the default `id,state,topic,title`
  (`columns.go`). An unknown name →
  `UsageError{"unknown --columns name %q; valid columns: %s"}` → exit 2
  (`columns.go`). Valid names: `assignee`, `blocked`, `created_at`, `id`,
  `labels`, `parent`, `priority`, `rank`, `state`, `title`, `topic`, `type`,
  `updated_at` (`columns.go`).

**`--query` grammar** (`internal/query/query.go`):
- `Parse` trims the input and tokenizes it (`query.go`). The tokenizer
  splits on space, tab and newline and honors single and double quotes, which it
  strips; an unterminated quote → `"unterminated quote in query"`
  (`query.go`). `Parse` returns every refusal as a `model.ValidationError`
  carrying the underlying message, so each exits 3 (`query.go`).
- Terms (`applyTerm`, `query.go`): `status:<state>[,<state>...]` (via
  `model.ParseStates`), `resolution:<res>` (via `model.ParseResolution`),
  `type:<type>[,<type>...]` (via `model.ParseIssueTypes`), `assignee:<v>`,
  `id:<v>[,<v>...]`, `parent:<v>[,<v>...]`, `label:<v>[,<v>...]` (the last
  three via `storage.ParseNames`, the reader the matching flag uses; a blank
  slot, such as bare `parent:` →
  `model.ValidationError{"parent: needs an issue id in every slot, e.g. parent:<epic-id>"}`,
  `query.go`), `has:comments` (any other `has:` →
  `unsupported has: filter %q`), `sort:<spec>` (via `storage.ParseSortSpecs`),
  `limit:<int>` (non-numeric → `limit must be an integer, got %q`; negative →
  `limit must be non-negative, got %d`), bare `archived`, bare `deleted`, and any
  term beginning `updated` (`query.go`). Anything else becomes a free-text
  search term (`query.go`).
- `updated` terms (`applyTimeTerm`, `query.go`; `splitComparator`,
  `query.go`): the comparator is one of `>=`, `<=`, `>`, `<`, `:`; a missing
  comparator or empty value is wrapped `parse updated term %q`; a value
  `query.ParseTimestamp` refuses → `timestamp must be RFC3339, got %q`. `>=` and
  `>` both set updated-after; `<=` and `<` both set updated-before; `:` with a
  valid timestamp → `updated supports only >=, >, <=, <`.
- `query.Merge(filter, parsed.Filter)` (`query.go`): statuses, types,
  assignees, parent ids and sort keys dedupe-merge, flag values first
  (`mergeSlice`, `query.go`); resolutions, search terms, ids and labels
  plain append; `IncludeArchived`/`IncludeDeleted` OR; `Limit` overwritten when the
  query limit > 0. Conflicting `has-comments` → `conflicting has-comments filters`;
  conflicting time bounds → `conflicting updated-after filters <t1> and <t2>`
  (`query.go`). `Merge` ends with `validateFilter`: `UpdatedAfter > UpdatedBefore` →
  `updated-after cannot be greater than updated-before` (`query.go`). `Merge`
  returns each refusal as a `model.ValidationError`, so each exits 3, and `lit ls`
  runs it even with no `--query`.

**Default active-work filter** (`cli.go`): if after all merging both
`filter.Statuses` and `filter.Resolutions` are empty, statuses default to
`[open, in_progress]`. A resolution filter alone therefore *does not* get clamped
(so `--query resolution:wontfix` reaches closed issues).

**Relation columns**: after `ListIssues` (`cli.go`), `listDerivedColumns`
(`cli.go`) loads the data for the highest source any selected
column declares (`columnSourceFor`, `columns.go`; sources
`sourceIssue < sourceRelations < sourceReadiness`, `columns.go`). `parent`
declares `sourceRelations` and `blocked` declares `sourceReadiness`; every other
column is `sourceIssue` (`columns.go`).
- `sourceIssue`: no load; the cell map is nil (`cli.go`).
- `sourceRelations`: `fetchIssueRelations` batch-loads relations for the listed
  issues, and `parentColumnsFor` sets only `parentID` (`cli.go`,
; `ready_state.go`).
- `sourceReadiness`: calls the policy for required fields, runs `annotateIssues`,
  and `readinessColumnsFor` sets `parentID` from the relation graph and
  `blocked = !ClassifyReadiness(row.Annotations).IsReady()` (`cli.go`;
  `workable.go`). A policy error fails the command. Over `--at`,
  `noReadyPolicy` supplies no required fields.
- A missing cell renders as the zero `derivedColumns` (`output.go`):
  `parent` renders `-` for an empty id, and `blocked` renders `blocked` when set
  and `-` otherwise (`columns.go`, `output.go`).

**Output**: `parseColumnSelection` and then `parseListFormat` run at the top of the
work closure, before the query (`cli.go`). `parseListFormat`
(`output.go`) lowercases and trims the value and looks it up in
`listFormats` (`output.go`): `lines` → `printIssueLines` (columns joined with
`" | "`, no header, `output.go`), `table` → `printIssueTable` (uppercased
tab-aligned header, then rows, `output.go`). Anything else, including an
explicit empty value, → `ValidationError{"unsupported --format \"<x>\" (valid: lines, table)"}`
→ exit 3, reason `validation_refused`. The `--format` help string ("Output format:
lines|table") is built from the same map (`cli.go`, `output.go`).

- Stray positionals: the leaf declares `positionals: 0` (`cli.go`), and
  `listPositionals` reads pflag's leftover arguments and refuses any count other
  than 0 → `UsageError{"usage: lit ls [flags]  (got N positional arguments: [...])"}`
  → exit 2, checked after the parse and before any store opens (`cli.go`,
). `lit ls stray` is refused this way, and so is `lit ls -- --at <dir>`,
  whose two tokens after `--` are positionals.

### 2.4 `lit show` — Show issue details

- Registration `register.go`, `app.AccessRead`. Handler `showLeaf`
  (`cli.go`).
- Args: exactly one positional id; flag `--field` (string, `""`, help:
  "Comma-separated field names (e.g. description) to print with no surrounding
  context; omit for the full detail view") (`cli.go`).
- Refusals: `len(positional) != 1` →
  `UsageError{"usage: lit show <id> [--field <name>[,<name>...]]"}` → exit 2
  (`cli.go`).
- Sync-staleness banner is printed first **only when `--field` is blank**
  (`cli.go`).
- Reads `GetIssueDetail(id)`; missing → exit 4 (`cli.go`).
- Dispatches `EventShowTicket` in **both** modes (`cli.go`).

**`--field` mode** (`printIssueFields`, `output.go`):
- Accepted field names and their renderings (`issueFieldNames`, `output.go`):
  `id`, `title`, `description`, `prompt`, `type`, `topic`,
  `priority` (`Priority.String()`), `status` (`i.State()`),
  `assignee` (raw, may be empty), `labels` (comma-joined, no spaces),
  `rank`, `lane`, `created_at` (RFC3339), `updated_at` (RFC3339).
- Names are lowercased and trimmed (`output.go`).
- Unknown name →
  `UsageError{"unknown --field \"<x>\"; valid fields: <sorted list>"}` → exit 2,
  and **nothing is printed** because all fields are resolved before any write
  (`output.go`, sorted list from `sortedIssueFieldNames`,
  `output.go`).
- Exactly one field → the bare value, no label (`output.go`).
- Two or more → `name: value` lines, in the requested order (`output.go`).
- No epic context, no parent block, no siblings (`cli.go`).

**Full-detail mode** (`printIssueDetail`, `output.go`), in exact order:
1. `<id>\n<title>\n\n` then
   `type: …`, `topic: …`, `priority: …`, `labels: …` (comma-space joined, `-` if
   none), `archived: …` (RFC3339 or `-`), `deleted: …` (`output.go`).
2. If the issue is a **leaf** (`Capabilities().Status != nil`):
   `status: <state>` and `assignee: <value or ->`; plus `resolution: <res>` only
   when a close recorded one (`output.go`).
   If it is a **container**: `children: %d closed, %d in_progress, %d open (%d total)`
   (`output.go`).
3. `unblocks: <ids>` — the IDs from `detail.Blocks` that are still `InPlay()`;
   omitted when empty (`output.go`, `openUnblockIDs` at `output.go`).
4. The `parent` group — one optional issue adapted to a slice and rendered by
   the shared group renderer, so the parent line carries a standing marker like
   every other relation line: `\nparent:\n- <id> [<standing>] <title>\n`, and,
   when the parent has one, its description indented by two spaces
   (`output.go`, `optionalGroup` at `output.go`).
5. `\ndescription:\n<text>\n` when non-empty (`output.go`).
6. `\nprompt:\n<text>\n` when non-empty (`output.go`).
7. Group blocks, each rendered as `\n<label>:\n` followed by
   `- <id> [<standing>] <title>` lines and omitted entirely when empty
   (`printIssueGroup`, `output.go`), in this order:
   `children` (`output.go`), `depends_on` (`output.go`),
   `blocks` (`output.go`), `redirect` (single optional target adapted to a
   slice, `output.go`, `optionalGroup` at `output.go`),
   `related` (`output.go`).
   `<standing>` is `issueStanding` (`output.go`): the retention name
   when the issue is frozen, else its state, and a closed state carries the
   resolution the close recorded as a `:<resolution>` tail —
   `[closed:duplicate]`, `[closed:superseded]`, `[closed:obsolete]`,
   `[closed:wontfix]` — with a bare `[closed]` for a close that recorded none.
   There is deliberately **no** `siblings` group here (`output.go`).
8. `\ncomments:` then `- [<createdBy>] <body>` with newlines in the body escaped
   to the literal `\n` (`output.go`).
9. **No** history block — history lives behind `lit history` (`output.go`).
10. Then `writeEpicContext` appends the epic plan block (§2.5). The block is
    **resolved before step 1 writes anything** (`cli.go`), so the body and
    the block are all-or-nothing: a failure to build the block exits nonzero with
    neither printed, rather than after a partial body. The staleness banner and any
    fired show-ticket workflow body are written before that point either way.

### 2.5 Epic-context block appended by `lit show`

`resolveEpicContext` (`epic_context.go`) and `writeEpicContext`
(`epic_context.go`):
- `epicViewFor(issue, parent)` (`epic_context.go`): a container shows its
  own children with no focused child; a leaf whose parent is a container shows the
  parent's plan with itself focused; anything else returns nil and **nothing is
  printed**.
- The target is resolved first, so the `ready.required_fields` policy is read
  from repo config (`readyRequiredFields` → `config.Load`, `cli.go`)
  **only when a plan slice exists**. An issue in no epic reads no repo config, so
  a `.lit/config.toml` that fails validation for an unrelated reason (for example
  `snapshot.retention_budget <= 0`) does not affect `lit show` on it; for an epic
  member the same config error fails the command, before any output.
- Prints a leading blank line then `renderEpicContext(ec)` (`epic_context.go`);
  a nil context (no plan slice) writes nothing.

`buildEpicContext` (`epic_context.go`):
- `GetRelationsByIDs([epicID])`; a missing epic → `storage.NotFoundError`
  (`epic_context.go`).
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
  (`epic_context.go`). Membership = the epic id plus all child ids
  (`epicMemberIDs`, `epic_context.go`). Edges are sorted by
  (blocked, blocker) (`epic_context.go`).

`renderEpicContext` output shape (`epic_context.go`):
```
Epic: <epicID> — <epicTitle>
Why: <first non-blank line of epic description, leading '#'s stripped>

Children:
<child lines>
[Cross-epic dependencies block]
```
- `firstLine` strips whitespace and leading `#` characters (`epic_context.go`).
- Child line (`renderChildLine`, `epic_context.go`):
  `"    "` gutter (or `"  ▶ "` when focused), the status marker padded to
  `len("[in_progress]")` = 13 (`epic_context.go`), two spaces, the id, two
  spaces, the title, then `"  [lane: <lane>]"` when the lane is non-empty
  (`laneTag`, `epic_context.go`), then `"   (you are here)"` when focused.
- No children → `"  (none)\n"` (`epic_context.go`).
- Cross-epic block, omitted entirely when both directions are empty
  (`epic_context.go`):
```

Cross-epic dependencies:
  Blocks externally:
    <blocked> blocked by <blocker>
  Blocked externally:
    <blocked> blocked by <blocker>
```
  Each subsection is omitted when its slice is empty (`epic_context.go`).

### 2.6 `lit history` — State-transition history

- Registration `register.go`, `app.AccessRead`. Handler `historyLeaf`
  (`cli.go`).
- No flags beyond the implicit `--help`.
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit history <id>"}` → exit 2 (`cli.go`).
- Reads `GetIssueDetail(id)` (`cli.go`).
- Output (`printIssueHistory`, `output.go`):
```
<id>
<title>

history:
```
  then `printHistoryEvents` (`output.go`): per event
  `- [<actor> @ <Jan 2, 2006 3:04 PM MST, local>] <action> <reason>` with newlines
  in the reason escaped to `\n`; an event with no `Action` displays the literal
  `update` (`output.go`). Then per change,
  `    <field>: <from or -> → <to or ->` (`output.go`).
- An empty event slice prints just the header (`output.go`).

### 2.7 `lit update` — Update issue fields

- Registration `register.go`, `app.AccessWrite`. Handler `updateLeaf`
  (`cli.go`).
- Flags (`cli.go`): `--title`, `--description`, `--prompt`, `--type`
  (default `""`), `--priority` (int, default 0), `--assignee`, `--labels`,
  `--lane`, `--status` (registered only to intercept it; help text
  "(removed) change status with the transition verbs: lit start|done|close|open"),
  `--reason` ("Reason recorded on the field-change event"), and hidden `--by`.
- Refusals:
  - `len(positional) != 1` → `UsageError` with the usage line
    `"usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--lane <key>] [--reason <text>]"`
    (`cli.go`).
  - `--status` present (detected via `fs.Visit`) → `UsageError{statusViaVerbsGuidance}`
    → exit 2. Verbatim text: "lit update no longer changes status — the transition
    verbs are the single enforcer of the transition guardrails. Use: `lit start
    <id>` (claim → in_progress), `lit done <id>` (finish → closed), `lit close <id>
    --resolution <duplicate|superseded|obsolete|wontfix>` (close with an outcome),
    `lit open <id>` (reopen)" (`cli.go`).
  - No field flag at all → `UsageError{"lit update requires at least one field flag; " + usage}`
    → exit 2 (`cli.go`). Note `--reason` alone does not count: `Reason`
    is set unconditionally but `Change.IsEmpty()` governs (`cli.go`,
    `cli.go`).
- **Only visited flags are applied.** Each visited flag sets a pointer on
  `storage.UpdateIssueInput` (`cli.go`):
  - `--title` / `--description` / `--prompt` set the raw value (no trimming at
    the CLI) (`cli.go`).
  - `--type` parses via `model.ParseIssueType` → `model.ValidationError` on a bad
    value (`cli.go`).
  - `--priority` parses via `model.ParsePriorityName` (`cli.go`).
  - `--assignee` is trimmed and honored **verbatim** — session-identity resolution
    is deliberately *not* applied here, so an empty value clears the assignee
    (`cli.go`).
  - `--labels` CSV replaces the whole label set (`cli.go`).
  - `--lane` trimmed (`cli.go`).
- The `Change.Actor` is `resolveActor()` — session identity else `--by` else `""`
  (which the store normalizes to `unknown`) (`cli.go`).
- Applies via `Store.Apply(ctx, id, change)` (`cli.go`).
- Dispatches `EventTicketUpdated` (`cli.go`), prints the summary line,
  emits the `update` breadcrumb (`cli.go`).

### 2.8 `lit rank` — Reorder an issue's rank

- Registration `register.go`, `app.AccessWrite`, dispatched by `rankDispatch`
  (`cli.go`) to `rankLeaf`.
- If `args[0] == "set"`, `rankDispatch` returns `rankSetLeaf()` with `args[1:]` (`cli.go`).
- Flags (`cli.go`): `--top` (bool, "Move to highest rank"),
  `--bottom` (bool, "Move to lowest rank"), `--above` (string, "Rank above this
  issue ID"), `--below` (string, "Rank below this issue ID").
- Refusals:
  - `len(positional) != 1` →
    `UsageError{"usage: lit rank <id> --top|--bottom|--above <id>|--below <id>"}`
    → exit 2 (`cli.go`).
  - Number of *visited* mode flags ≠ 1 →
    `ValidationError{"exactly one of --top, --bottom, --above, --below is required"}`
    → exit 3 (`cli.go`). Note this counts presence, not truthiness, so
    `--top=false` still counts.
  - Surplus positionals refused by `refuseSurplusPositionals` (`register.go`),
    called from `parseLeaf` (`register.go`).
- Store calls (`cli.go`): `RankToTop`, `RankToBottom`,
  `RankAbove(issueID, *above)`, `RankBelow(issueID, *below)`. The relative forms
  return a `storage.RankMove{MovedID, AnchorID}`.
- **Frame substitution reporting** (`cli.go`):
  - If `move.MovedID != issueID`:
    `"<issueID> is inside <MovedID>; ranked the epic <MovedID> instead, leaving its internal order unchanged\n"`
  - If a named anchor was given and `move.AnchorID != namedAnchor`:
    `"<namedAnchor> is inside <AnchorID>; ranked relative to the epic <AnchorID> instead\n"`
  - (Asserted in `rank_frame_test.go`.)
- Then re-reads `GetIssue(move.MovedID)`, prints its summary line, emits the
  `update` breadcrumb (`cli.go`).

### 2.9 `lit rank set <id1> <id2> [...]`

- Handler `rankSetLeaf` (`cli.go`). Declares `positionals: allPositionals`
  (`cli.go`), the unbounded ceiling defined at `register.go`.
- Refusal: fewer than 2 positionals →
  `UsageError{"usage: lit rank set <id1> <id2> [<id3> ...]"}` → exit 2
  (`cli.go`).
- Calls `Store.RankSet(ctx, positional)` — atomic; stacks the named issues at the
  top in the given order (`cli.go`, doc at `cli.go`).
- Output: for each resolution where `RankedID != NamedID`,
  `"<NamedID> is inside <RankedID>; ranked the epic <RankedID> instead, leaving its internal order unchanged\n"`
  (`cli.go`); then
  `"ranked %d issues at top in order: <comma-joined RankedIDs>\n"`
  (`cli.go`); then the `update` breadcrumb (`cli.go`).

### 2.10 Transition commands — `start`, `done`, `close`, `open`, `archive`, `unarchive`, `delete`, `restore`

All eight route through one handler `transitionLeaf(spec)`
(`cli.go`), registered via `r.transitionCmd(spec)` with
`app.AccessWrite` (`register.go`).

Registry rows and summaries:
- `start` — "Claim issue work", group `operations` (`register.go`)
- `done` — "Finish claimed work (success path; requires in_progress)" (`register.go`)
- `close` — "Close without finishing (wontfix / obsolete / duplicate; from any non-closed state)" (`register.go`)
- `open` — "Reopen issue(s)" (`register.go`)
- `archive` — "Archive issue(s)", group `retention` (`register.go`)
- `unarchive` — "Unarchive issue(s)" (`register.go`)
- `delete` — "Delete issue(s)" (`register.go`)
- `restore` — "Restore deleted issue(s)" (`register.go`)

**Common flags on every transition** (`cli.go`):
`--reason` (string, `""`, "Transition reason") and hidden `--by`.

**Per-spec flags** (`cli.go`):
- `start` adds `--assignee` (string, `""`, help "Assignee fallback when
  CLAUDE_CODE_SESSION_ID is unset (env always wins when set)") and `--take` (bool,
  `false`, help "Confirm taking over a lane another checkout claims right now
  (required for non-interactive callers; without it an interactive terminal is
  prompted instead)") (`cli.go`). The action is
  `model.Start{Assignee: resolveIdentity(*assignee)}` (`cli.go`).
- `close` adds `--resolution` (string, `""`, "Close resolution (required):
  duplicate|superseded|obsolete|wontfix") and `--of` (string, `""`, "Canonical
  ticket a duplicate/superseded close redirects to (required for those, rejected
  otherwise)") (`registerCloseOutcomeFlags`, `cli.go`).
- `done`, `open`, `archive`, `unarchive`, `delete`, `restore` register **no**
  extra flags — `fixedAction` (`cli.go`). Passing e.g.
  `lit done --resolution x` is therefore an unknown-flag `UsageError` → exit 2.

**Argument handling** (`cli.go`): after parsing, exactly one remaining
positional is required; otherwise `errors.New("usage: lit <name> <id> [--reason <text>]")`
— a **plain error**, so exit code 1, not 2.

**Sequence** (`transitionLeaf`, `cli.go`):
1. `GetIssue(issueID)` pre-read (`cli.go`) — missing → exit 4.
2. `buildAction()` (`cli.go`).
3. `authorize(ctx, stdout, ap, issueID, prior)` — §2.11 (`cli.go`).
4. `transferNotice(ctx, ap, issueID, action)` (`cli.go`, implementation
   `claims_context.go`): for a `model.Start` whose prior claimant was
   held and actually changes hands, `"claim transferred: %s -> %s\n"` with
   both sides rendered by `describeClaimant` (`claims_render.go`),
   which names the assignee when there is one, the checkout via `nameCheckout`
   (`claims_render.go`) when there isn't or alongside it, and the
   literal `"the public checkout"` — never `"(unassigned)"` — for a prior
   claimant with no assignee and no minted token. Computed here, on
   pre-Apply state, but not written out until step 7 — a failed `Apply`
   must announce nothing.
5. `actor := resolveActor()`; `Store.Apply(ctx, issueID, Change{Action, Actor, Reason})`
   (`cli.go`).
6. If the action is a `StatusAction`, dispatch the transition occasion
   (`cli.go`).
7. Write the transfer notice string, then `printIssueSummary`
   (`cli.go`).
8. If the action is a `StatusAction` whose `Target() == model.StateClosed`
   (i.e. `done` and `close`), re-read `GetIssueDetail` and print the close
   adjacency block (`cli.go`) — §2.12.
9. Breadcrumb per `transitionBreadcrumbTopics` (`cli.go`).

**Close outcome validation** — `closeOutcomeFromFlags(resolution, target, usage)`
(`cli.go`), shared with `bulk close` (`bulk.go`):
- `model.ParseResolution` failure → `UsageError{"<usage>\n<parse error>"}` → exit 2
  (`cli.go`). For `lit close`, the usage string is
  `"usage: lit close <id> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]"`
  (`cli.go`). A missing `--resolution` therefore fails through this path —
  `--resolution` is effectively required.
- Redirecting resolutions (`duplicate`, `superseded`) with a blank `--of` →
  `UsageError{"closing as <res> redirects to a canonical ticket — name it with --of"}`
  (`cli.go`).
- A non-blank `--of` on a terminal resolution (`obsolete`, `wontfix`) →
  `UsageError{"--of applies only to duplicate/superseded closes, not <res>"}`
  (`cli.go`).
- Outcome construction: `duplicate` → `model.Duplicate{Of: target}`;
  `superseded` → `model.Superseded{By: target}`; `obsolete` → `model.Obsolete{}`;
  `wontfix` → `model.Wontfix{}` (`cli.go`). An unreachable default
  returns `fmt.Errorf("resolution %q has no close outcome", parsed)`
  (`cli.go`).

**Store-level refusals reaching every transition:**
- An archived or deleted issue rejects any status action:
  `"cannot <action> archived or deleted issue"` (`internal/store/store.go`).
- A container (epic) rejects any status action with `ContainerActionError`
  (`internal/model/model.go`).
- The redirect target must exist, must not be the closing issue itself, and must
  not be deleted: `"closing as <res> requires a canonical target issue to redirect
  to"`, `"cannot redirect <id> to itself"`, `"cannot redirect <id> to <target>:
  the canonical issue is deleted"` (`internal/store/store.go`,
).
- `archive` on a deleted issue → `"cannot archive deleted issue"`;
  `unarchive` on a deleted issue → `"cannot unarchive deleted issue"`
  (`internal/model/lifecycle/retention.go`).
- **There is no from-state precondition on `done`.** `Store.Apply` performs no
  status-precondition check (`internal/store/store.go`), and
  `applyStatusAction` is total over the leaf states
  (`internal/model/lifecycle/status_states.go`). The registry summary
  "requires in_progress" (`register.go`) is not enforced by any code path in
  this repo. A same-state transition is a no-op that records nothing
  (`status_states.go`, `internal/store/store.go`).

### 2.11 `lit start` takeover gate

`startSpec.authorize` → `authorizeStart(ctx, stdout, ap, issueID, prior, *take)`
(`cli.go`, implementation `claims_takeover.go`):
1. `GetRelationsByIDs([issueID])` and `model.LaneOf(prior, parent)`.
2. `gatherClaimContext(ctx, stdout, ap)`.
3. `relationOf(standing, self)` (`claims_takeover.go`), the one reading of a
   standing against an identity, shared with routing:
   - `Held` by self, or anything that is not `Held` (`Unclaimed`, which is also what
     a lane whose claim has expired derives) → no ceremony: the start proceeds and
     prints nothing about the lane.
   - `Held` by another → `confirmFreshTakeover` (`claims_takeover.go`):
     - `--take`, at a terminal or not → prints `"<claim line> — taking over (--take)\n"`
       and proceeds.
     - Non-interactive stdout (`!isTerminal(stdout)`) and no `--take` →
       `takeoverUnconfirmedError` "<claim line> — this lane is claimed and active; pass --take to confirm the takeover"
       → exit 3, reason `takeover_unconfirmed`.
     - Interactive and no `--take` → prints `"<claim line>\ntake over this lane? [y/N] "`, reads a
       line from stdin; a read error other than EOF →
       `"read takeover confirmation: %w"`; an answer not starting with `y`
       (case-insensitive, trimmed) → `takeoverUnconfirmedError` "takeover declined" → exit 3,
       reason `takeover_unconfirmed`.
4. Returns the line the start owes after Apply: from a held lane, ours or another's,
   `transferNotice(ctx, ap, issueID, start)` (`claims_context.go`), which is
   `claim transferred: <old> -> <new>` when the recorded claimant changes hands and
   empty otherwise; from an unclaimed lane, `""` without asking. `transitionLeaf` writes
   the hook's string after Apply (`cli.go`), so a start on a lane whose claim has
   expired announces no transfer, whatever the row's history records.

There is no "stale-informed" path. Until links-claims-y6yz an expired claim derived
its own standing, and `start` on such a lane printed the lapsed holder's claim line
tagged `(stale)` plus `check for unmerged branches or PRs on this lane before building
on it`; an expired claim is not a claim, so both the standing and the printout are gone.

Claim line format — `formatClaimLine(cc, lane, now)` (`claims_render.go`):
returns `false` (no line) for anything but a `Held` lane — an `Unclaimed` lane,
including one whose claim has expired, prints nothing. Otherwise
`"<prefix>[ · contested by <s1>, <s2>] · <age> ago[ · <lane progress>]"` where:
- prefix, when the holder resolves to a live local worktree:
  `"claimed here: <path> (<branch or 'detached HEAD'>)"`
  (`claimPrefix`, `claims_render.go`)
- otherwise: `"claimed: <name> (<state>)"` — `<name>` is `nameCheckout(by)`
  (`claims_render.go`): the stream's first 8 chars, or the literal
  `the public checkout` when `by` is the zero Attribution (an unattributed
  establishing event); `<state>` is `holdState(by)`
  (`claims_render.go`): `elsewhere` for an identified holder, or
  `unaddressed` for the public checkout, which has no address to be
  "elsewhere" from
- age via `humanizeCoarseDuration(now - held.LastActivity)` (`claims_render.go`)
- lane progress: `"<activeID> in progress, <done>/<total> done"` or
  `"<done>/<total> done"`; empty for a zero LaneProgress
  (`claims_render.go`).

`gatherClaimContext` (`claims_context.go`) reads config, lists **all**
issues including archived and deleted (`claims_context.go`), fetches all
relations, lists all events, builds evidence, enumerates
live checkouts. If checkout enumeration fails it prints to **stdout**:
`"warning: could not enumerate local checkouts (<err>) — claim liveness check and local addresses skipped, freshness alone governs\n"`
and continues with zero local checkouts (`claims_context.go`).

### 2.12 Close/done adjacency block

`printCloseAdjacency(w, detail)` (`output.go`), printed after the summary
line for `done` and `close`, each group omitted when empty:
- `\nparent:\n- <id> [<state>] <title>` (`output.go`)
- `\nsiblings:` — only the `InPlay()` siblings (`output.go`)
- `\nredirect:` — the redirect target if any (`output.go`)
- `\nrelated:` (`output.go`)
- `\nunblocks: <comma-joined ids of still-live dependents>` (`output.go`)

### 2.13 `lit backlog` — Full workable backlog

- Registration `register.go`, `app.AccessRead`, handler
  `workableLeafFn(backlogView)`. Summary: "List the full workable backlog in
  priority/rank order (blocked items inline)".
- `backlogView` preset (`workable.go`): `hasFilters: true`,
  `hasLimit: true`, `hasColumns: true`, order = `orderCanonical` (no-op,
  `workable.go`), keep = `keepAll` (`workable.go`), render =
  `printBacklogOutput`, occasion = `backlogOccasion()`.
- Flags (`workableLeaf`, `workable.go`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--assignee` | string | `""` | "Filter by assignee" (always registered) |
| `--type` | string | `""` | "Filter by issue type" |
| `--status` | string | `""` | "Filter by status: open\|in_progress" |
| `--labels` | string | `""` | "Comma-separated labels all of which must match" |
| `--limit` | int | `0` | "Limit results" — applied **after** ordering; read by `query.ParseLimit`, so a negative value → `parse --limit: %w` → exit 3 (`workable.go`) |
| `--columns` | string | `""` | "Comma-separated output columns" |

- Refusal: any positional argument → `UsageError{view.usage()}` (`workable.go`).
  `usage()` builds `"usage: lit backlog [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--limit N] [--columns ...]"`
  (`workable.go`).
- `--status closed` (or any unparseable state) →
  `UsageError{"invalid --status \"<x>\" (valid: open, in_progress)"}` → exit 2
  (`parseWorkableStatus`, `workable.go`).
- Bad `--type` → `UsageError{"invalid --type \"<x>\": <err>"}` → exit 2
  (`parseWorkableType`, `workable.go`).
- Prints the sync-staleness warning first (`workable.go`).
- Runs the shared workable pipeline (§1.18) via `gatherWorkableAnnotated`
  (`workable.go`), then `gatherClaimContext` (`workable.go`).
- Dispatches `EventShowBacklog` after rendering (`workable.go`).

**Output** (`printBacklogOutput`, `backlog.go`):
1. The `backlogPreamble` verbatim (`backlog.go`):
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
2. A rule of 80 `─` characters, then a blank line (`backlog.go`).
3. If empty: the literal `(backlog empty)` and nothing else (`backlog.go`).
4. Otherwise, per row: `"%2d. %s"` — a right-aligned 1-based index, then the
   resolved columns joined by two spaces (`backlog.go`).
5. Then the indented context block (`printBacklogContext`, `backlog.go`), in
   this order, each omitted when empty:
   - `    epic: <epicID>  <epicTitle>` (`output.go`)
   - `    blocked: <reasons joined by "; ">` — only non-dependency blockers,
     rendered as `missing <field>` for `MissingField` and `needs-design` for
     `NeedsDesign` (`backlog.go`, `nonDependencyBlockingReasons` at
     `backlog.go`). `EarlierSiblingPending` appears in **neither** the
     blocked line nor the depends-on line.
   - `    depends on: <ids joined by ", ">` (`backlog.go`, `output.go`)
   - `    in_progress: <age truncated to minute>[ (ORPHANED)]` for in-progress
     rows (`backlog.go`, `inProgressSuffix` at `ready_state.go`);
     the `(ORPHANED)` suffix is withdrawn when somebody holds the row's lane
     (`backlog.go`), since the claim line beneath names them
   - `    <claim line>` when the row's lane is Held (`backlog.go`)
   - `    unblocks: <ids of rows that depend on this one>` — derived from the
     classified open-dependency facts of the whole workable queue, before any
     narrowing, so the line survives a filter or a limit that cuts the
     dependent row (`backlog.go`, `deriveQueueFacts` at
     `queue_facts.go`, read back through `queueFacts.Unblocks` at
     `queue_facts.go`)
6. Finally, if any row carries a `RankInversion` annotation:
   `"\nWarning: %d rank inversion(s) — dependencies ranked below their dependents. Run `lit doctor --fix` to repair. <agent-instructions>This command is idempotent and safe to run without confirmation.</agent-instructions>\n"`
   (`printRankInversions`, `ready_state.go`).

Lane for the claim line is `model.LaneOf(entry.Issue, details[entry.ID].Parent)`
(`backlog.go`).

### 2.14 `lit next` — Print the next workable leaf

- Registration `register.go`, `app.AccessRead`, handler `nextLeaf`
  (`next.go`). Summary: "Print the next workable leaf to lit start".
- Flags (`next.go`, and the hidden `--by`):

| Flag | Type | Default | Effect |
|---|---|---|---|
| `--assignee` | string | `""` | "Filter by assignee" |
| `--type` | string | `""` | "Filter by issue type" |
| `--status` | string | `""` | "Filter by status: open\|in_progress" |
| `--labels` | string | `""` | "Comma-separated labels all of which must match" |
| `--all` | bool | `false` | "Ignore the focus scope and route over the whole queue" |
| `--by` | string (hidden) | `""` | identity fallback (§1.11), read by `resumeAdvice` to say whether work in flight is the reader's (`next.go`) |

- `--status` and `--type` go through the same `parseWorkableStatus` /
  `parseWorkableType` refusals as `backlog` (`next.go`). **No** `--limit`,
  **no** `--columns`.
- Refusal: any positional → `UsageError{nextUsage}` → exit 2, where `nextUsage` =
  `"usage: lit next [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--all]"`
  (`next.go`), declared as the leaf's `usage` rather than checked inline
  (`next.go`).
- Retired flag: `--continue` is intercepted at the shared parse boundary as
  `UnsupportedError` → exit 3, message
  ``"--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag"``
  (`flagset.go`).
- Prints the sync-staleness warning first (`next.go`).
- Gathers the workable set — rows, relation details, **and the focus scope** —
  via `gatherWorkableAnnotated` (`next.go`, `cli.go`), then the claim
  context (`next.go`), and then routes (`next.go`), handing the render the
  reader's own identity from `actor()` alongside it:
  `routeNext(rows, details, cc.standings, cc.self, focus.scopeFor(*all))`.
  `scopeFor(true)` returns the zero `focusScope`, which holds every row
  (`ready_state.go`).

**`NextOutcome`** — a sealed sum interface (`next_route.go`,
`next_route.go`) with **eight** cases:

| Case | Fields | Meaning |
|---|---|---|
| `ServedFromClaim` | `Row` | a ready ticket in a lane this checkout already holds — step 1 (`next_route.go`) |
| `ResumedOwnWork` | `Row` | a ticket already in flight in a lane this checkout holds, handed back to its holder — also step 1 (`next_route.go`) |
| `ServedFromEpicLane` | `Row`, `Lane model.LaneID` | a pick from a different lane of the same epic this checkout already holds a lane in — step 2. The epic is `Lane.Epic()`; there is no `Epic` field (`next_route.go`) |
| `ServedFromNewLane` | `Row`, `Lane model.LaneID` | a ticket in a lane this checkout does **not** hold — produced by step 4 alone (`next_route.go`) |
| `ServedFromDependency` | `Row`, `Lane model.LaneID`, `Gates string` | step 1b's or 2b's on-path dependency; `Gates` is the id of the blocked row it unblocks, so the pick explains itself (`next_route.go`) |
| `ServedPastExhaustion` | `Row`, `Lane model.LaneID`, `Exhaustion Exhausted` | the checkout's own epic(s) have open work, none of it reachable, and the global pool has a ready ticket outside them — step 3 (`next_route.go`) |
| `Exhausted` | `Epics []string`, `Blocked []rowReach`, `OffPath []rowReach` | the same, with nothing ready in the global pool either — step 3; `OffPath` is the rows outside the scope a focus label withheld from the pool, set only on the terminal answer (`next_route.go`) |
| `NoWork` | `Unreachable []rowReach` | the global pool produced nothing — step 4 (`next_route.go`) |

`Exhausted` and `NoWork` implement `error` and travel outward as themselves
rather than being rendered into a generic error (`next_route.go`,
`next_route.go`).

**Admission** — `capacityFor(row, standing, self) capacity`
(`next_route.go`) is the single eligibility verdict. The three capacities
are `routeAround`, `serveWork`, `resumeWork` (`next_route.go`). With
`relation = relationOf(standing, self)` (`claims_takeover.go` —
`laneOurs` requires `self.Present() && held.By == self`, so a checkout with
no minted token never reads a lane as its own, even one the public checkout
itself holds) and `started = row.State() == model.StateInProgress`:

1. `relation == laneHeldForeign` → `routeAround`.
2. `!started` → `ClassifyReadiness(row.Annotations).IsReady()` ? `serveWork`: `routeAround`.
3. `started` and `relation == laneOurs` → `resumeWork`.
4. `started`, otherwise (`laneUnclaimed`) → `serveWork`.

An in-flight row in a lane nobody holds is abandoned by definition — whoever
started it no longer holds a claim there — and is served on that fact alone; the
orphan annotation does not enter routing. A lane whose claim has expired is
`Unclaimed` and so `laneUnclaimed`, whoever held it, this checkout included.
Only `laneHeldForeign` is routed around; a locked worktree past the clock
derives `Held` and is one of these. Servability is not gated on `model.StateOpen`.

**Routing precedence** — `routeNext(rows, details, epics, standings, self, scope focusScope)`
(`next_route.go`). `rows` are already in composite-rank order (§1.18).
`laneOf(row) = model.LaneOf(row.Issue, details[row.ID].Parent)`
(`next_route.go`);
`verdict(row) = capacityFor(row, standings.Of(laneOf(row)), self)`
(`next_route.go`);
`reachFor(row) = reachOf(row, standings.Of(laneOf(row)), self)`
(`next_route.go`);
and, once the checkout holds a lane,
`workToward = workTowardEach(rows, details, epics)` (`next_route.go`), the
gathered rows indexed by each id they are work toward: their own, and every
epic above them.
`pickFrom(from, inScope, accept ...capacity)` keeps the first row of `from`, in
rank order, whose lane `inScope` admits and whose verdict is in `accept`
(`next_route.go`); `pick` is `pickFrom` over all `rows`
(`next_route.go`). `accept` is a **set**, never a preference order —
composite rank is the only tiebreak routing applies (`next_route.go`).
`ownScope(standings, self)` yields `ownLanes` and `ownEpics`, read from the
**standings** and not from the gathered rows (`next_route.go`);
`mine(lane) = ownLanes[lane]` (`next_route.go`).

If `len(ownLanes) > 0` (`next_route.go`):

1. **Own lanes**, accepting `{serveWork, resumeWork}`, whichever the backlog ranks
   first (`next_route.go`). `resumeWork` → **`ResumedOwnWork{Row}`**;
   `serveWork` → **`ServedFromClaim{Row}`**.
   - 1b. Else `onPathDependency(gatingDependencies(rows, laneOf, mine, reachFor, workToward), laneOf)`
     — the first dependency gating one of our own lanes whose `reachKind` is
     `reachTakeable` (`next_route.go`), drawn from `gatingDependencies`, which
     collects the distinct open dependency IDs of the in-scope **open** rows in
     rank order and stamps each with the lowest `reachFor` among
     `workToward[id]`, carried by the first row in rank order with it, or
     `reachOutOfView` when that is empty (`next_route.go`) →
     **`ServedFromDependency{Row: dep.Row, Lane: laneOf(dep.Row), Gates: dep.Gates, Blocker: dep.ID}`** (`next_route.go`), the `Gates` being the blocked row the dependency gates and `Row` the dependency itself or, for an epic, a ticket under it.
2. Else **the rest of our epic, in lanes we do not already hold** — predicate
   `lane.Epic() != "" && ownEpics[lane.Epic()] && !mine(lane)`, accepting
   `serveWork` → **`ServedFromEpicLane{Row, Lane: laneOf(row)}`**.
   - 2b. Else, with `gating := gatingDependencies(rows, laneOf, ourScope, reachFor, workToward)`
     where `ourScope` is `func(lane) bool { return mine(lane) || ourEpic(lane) }`,
     `onPathDependency(gating, laneOf)` →
     **`ServedFromDependency{Row: dep.Row, Lane: laneOf(dep.Row), Gates: dep.Gates, Blocker: dep.ID}`**.
3. Else `exhausted := Exhausted{Epics, Blocked}` (`next_route.go`), where
   `Epics` is `slices.Sorted(maps.Keys(ownEpics))` and `Blocked` is
   `blockedRows(gating)` — the walk step 2b declined; `blockedRows` drops the
   gated id. When step 4's pick finds nothing, `OffPath` is set to
   `withheldByScope` over the focus-excluded rows whose lane `ourScope` does
   not admit. If step 4's pick finds a row →
   **`ServedPastExhaustion{Row, Lane: laneOf(row), Exhaustion: exhausted}`**;
   else → **`exhausted`**. The pick is outside our epic by construction.

Step 4 is reached directly by a checkout holding no lanes:

4. **The global pool, focus-scoped.** `pool, offPath := scope.partition(rows)`
   (`next_route.go`, `ready_state.go`), then
   `pickFrom(pool, func(model.LaneID) bool { return true }, serveWork)` →
   **`ServedFromNewLane{Row, Lane: laneOf(row)}`**.
   Else → **`NoWork{Unreachable: append(passedOver(pool, reachFor), withheldByScope(offPath)...)}`**
   (`next_route.go`), where `passedOver` stamps every walked pool row with
   `reachFor(row)` (`next_route.go`) and `withheldByScope` stamps
   every scope-excluded row `reachOffFocusPath` (`next_route.go`).

Steps 1-3 walk every gathered row; step 4 walks the focus-scoped pool. The row
set is passed to `pickFrom` explicitly at each step so that difference stays
visible (`next_route.go`).

**`reachKind`** (`next_route.go`) — what one row is to this checkout right
now: `reachTakeable`, `reachHeldFresh`, `reachNotReady`, `reachOutOfView`, plus
`reachOffFocusPath`, which only the pool diagnostic stamps, and the bound
`reachKindCount`. `reachOf(row, standing, self)` answers `reachTakeable` when
`capacityFor(...) != routeAround`, `reachHeldFresh` when
`relationOf(...) == laneHeldForeign`, else `reachNotReady`
(`next_route.go`). `rowReach{ID string, Row annotation.AnnotatedIssue, Kind reachKind}`
(`next_route.go`) is what both terminal outcomes carry.

`exhaustedNotes` (`next_route.go`), with no `reachTakeable` entry — exhaustion reports the very walk step 2b declined:
- `reachHeldFresh`: `"on your path but claimed by another checkout right now"`
- `reachNotReady`: ``"on your path but not startable right now — `lit show` it"``
- `reachOutOfView`: ``"on your path but outside this view — `lit show` it"``

`poolNotes` (`next_route.go`):
- `reachHeldFresh`: `"in progress or claimed in a lane another checkout holds right now"`
- `reachNotReady`: `"not startable — blocked by a dependency, or in flight and not abandoned"`
- `reachOffFocusPath`: ``"off the focus path this run answered over — `lit next --all` to route over the whole queue"``

`describeReach(rows, lead, notes)` renders `"<lead><names> (<note>)"` for each kind
that has rows, joined by `"; "`, in `reachKind` declaration order
(`next_route.go`). An entry's name is its `ID`, or `"<Row.ID> under <ID>"` when
it carries a `Row` whose id differs — a dependency read through a ticket under it
(`rowReach.name`, `next_route.go`). `nameIDs` names at most `maxNamedPerKind = 12` ids and
otherwise appends `" and <n> more"` (`next_route.go`).

**Terminal messages.** `Exhausted.scope()` (`next_route.go`) names the scope:
`"epic(s) <Epics joined by ", ">"` when `Epics` is non-empty, else
`"your claimed lane(s)"`. `Exhausted.home()` is ``"with `lit new --top`"``
when `Epics` is empty, else `"under the epic with "` +
``"`lit new --parent <epic> --top`"`` for each epic, joined by `" or "`.
`Exhausted.stay()` is empty when `Blocked` is empty, else the one route
``"to stay, file the ticket that clears a blocker <home()>, then make that blocker wait on it with `lit dep add --from <new> --to <blocker>`"``.
`Exhausted.why()`:
- `Blocked` empty: `"no ready work in <scope> — nothing else is queued behind what's already in progress"`
- Otherwise: `"no ready work in <scope> — <describeReach(Blocked, "blocked on ", exhaustedNotes)>"`

`Exhausted.outside()`: ``"`lit next` has nothing ready outside it either"``
when `OffPath` is empty, else
`"nothing on the focus path outside it is ready either: <describeReach(OffPath, "", poolNotes)>"`.

`Exhausted.Error()`: `"<why()>; <outside()>"`, then each `stay()` route,
joined by `" — "`.

`NoWork.Error()` (`next_route.go`):
- `Unreachable` empty: `"no ready work"`
- Any row `reachOffFocusPath` (`NoWork.withheld()`, `next_route.go`):
  `"no ready work on the focus path — the backlog is not empty, and each row below says why this run did not serve it: <describeReach(Unreachable, "", poolNotes)>"`
- Otherwise: `"no ready work — the backlog is not empty, but nothing in it is startable here: <describeReach(Unreachable, "", poolNotes)>"`

Both map to `ExitNoWork` = **6** (`exit.go`), with reasons
`scope_exhausted` and `no_ready_work` respectively (`error_output.go`).

**Rendering** — `renderNextOutcome(w, outcome, details, cc)` (`next.go`):
- `ServedFromClaim` → no announcement at all (`next.go`).
- `ResumedOwnWork` → `resumeAdvice(o.Row, cc.actingAs)` + `"\n"` (`next.go`,
  `next.go`). The row's assignee decides which of two sentences: when it is
  non-empty and differs from the identity running the command,
  ``<RowID> is in progress and assigned to <assignee>, not to you — check that they have stopped before you continue it, or take other work from `lit backlog` ``;
  otherwise `"<RowID> is already in progress in a lane you hold — continue where you left off"`.
- `ServedFromEpicLane` → `startAdvice(o.Row, o.Lane)`
  + `" (a second lane of an epic you already hold a lane in)\n"`.
- `ServedFromNewLane` → the same `startAdvice(...)` + `"\n"`.
- `ServedFromDependency` → the same `startAdvice(...)` + `dependencyReason(o)` + `"\n"`:
  `" (gates %s, which is on your path)"` on `Gates` when `Blocker == Row.ID`,
  else `" (it is in epic %s, which gates %s on your path)"` on `Blocker`, `Gates`.
- `ServedPastExhaustion` → `"<why()>\n"`, then the `stay()` routes followed by `"move on to the top ready ticket outside it: <startAdvice(o.Row, o.Lane)>"`, joined by `"\nor "`, then `"\n"`.
- `Exhausted`, `NoWork` → returned as themselves; no ticket printed.
- Any other outcome type → panic.

`startAdvice(row, lane)` (`next.go`) is one sentence with an optional
lead clause. `object, named := lane.Describe()`, `object = "it"` when not named:
- ``"run `lit start <id>` to claim <object>"``
- in progress: ``"<id> is in progress and nobody holds it — run `lit start <id>` to claim <object>"``

The lead clause says only what the standing proves — routing serves an
in-progress row from a lane this checkout does not hold only when nobody holds
that lane — and nothing about who left the row or when.

`LaneID.Describe() (string, bool)` (`model.go`):
- solo lane → `("", false)`
- empty key → `("the default lane of epic <epic>", true)`
- otherwise → `("lane <key> of epic <epic>", true)`

On a served row, `renderNextOutcome` calls `printNextSummary(w, row, cc, lane)`
with `lane = model.LaneOf(row.Issue, details[row.ID].Parent)` (`next.go`),
which prints the **default columns** (`id state topic title`) joined by two
spaces (`ready_state.go`, `columns.go`), then `printInlineDeps`
(`ready_state.go`): `    epic: …`, `    depends on: …`, the claim line,
and `    unblocks: …` — but `next` passes a **nil** unblocks map, so the unblocks
line never appears (`ready_state.go`). It then returns
`nextPulledOccasion(row.Issue)` (`next.go`, `workflow_events.go`),
dispatched as `EventNextPulled` (`next.go`).

`lit next` performs **no writes** — it is registered `app.AccessRead`
(`register.go`). `startAdvice` names what a subsequent `lit start` would
claim; this command claims nothing.

### 2.15 `lit orphaned` — quiet in-progress issues

- Registration `register.go`, `app.AccessRead`. Handler `orphanedLeaf`
  (`cli.go`). Summary: "List in_progress issues with no recent updates".
- Flags: `--assignee` (string, `""`, "Filter by assignee") (`cli.go`).
- Refusal: any positional → `UsageError{"usage: lit orphaned [--assignee <user>]"}`
  → exit 2 (`cli.go`).
- Query: `Statuses = [in_progress]`, assignee filter, no archived, no deleted
  (`cli.go`). Containers dropped via `filterWorkableIssues`
  (`cli.go`).
- Annotates with `newOrphanedAnnotator(orphanedThreshold)` only (`cli.go`) and
  keeps rows where `ClassifyReadiness(...).IsOrphaned()` (`cli.go`).
- Sorted oldest-`UpdatedAt` first (`cli.go`).
- Output (`printOrphanedText`, `cli.go`):
  - Empty → `"No orphaned issues."`
  - Otherwise, per row: columns `id | state | topic | assignee | title` joined by
    `" | "`, then `" | Last Update: <age truncated to minute>"`.

### 2.16 `lit children <parent-id>`

- Registration `register.go`: `runList(ctx, stdout, childrenSurface, args)`,
  the same entrypoint and leaf as `lit ls` (§ `lit ls` above). Summary: "List an
  issue's direct children by rank (`lit ls --parent <id>`; takes every ls flag)".
- `childrenSurface = listSurface{name: "children", positionals: []string{"<parent-id>"}}`
  (`cli.go`). `listLeaf(surface)` names the flag set after the surface and
  declares every `ls` flag, so `lit children --help` lists them (`cli.go`).
- Refusal: the leaf declares `positionals: 0`, so every token reaches pflag
  (`cli.go`), and `listPositionals` reads the positionals from pflag's leftover
  arguments, `l.fs.cmd.Flags().Args()`. Because pflag knows which flags are
  booleans, the id is found before or after any flag, including after
  `--include-archived` or `--`. Each positional is trimmed. A count other than 1,
  or a positional that is blank after trimming, →
  `UsageError{"usage: lit children <parent-id> [flags]  (got N positional arguments: [...])"}`
  (the list printed with `%q`) → exit 2, checked after the parse and before any
  store opens (`cli.go`).
  A blank or `-`-prefixed `--at` → `UsageError{"usage: lit children --at <store-dir>  (a storage directory from `lit stores`)"}`
  (`cli.go`).
- Filter: the positional is appended to the `--parent` ids (`cli.go`), so
  `lit children <id> [flags]` builds the filter `lit ls --parent <id> [flags]` builds
  and prints the same output. The `ls` defaults apply: statuses default to
  `[open, in_progress]` when no status or resolution filter is set
  (`cli.go`), archived and deleted children are excluded unless
  `--include-archived`/`--include-deleted`, and the default columns are
  `id,state,topic,title`. A parent id naming no issue → `NotFoundError` → exit 4.

### 2.17 `lit comment` — Add / remove comments

Family `commentFamily`, usage `"usage: lit comment <add|rm> ..."` (`cli.go`).
Both subcommands are `app.AccessWrite`. Missing/unknown subcommand → the bare
usage string as a `UsageError` → exit 2 (`resolve`, `register.go`).

**`lit comment add <id> --body <text>`** (`commentAddLeaf`, `cli.go`):
- Flags: `--body` (string, `""`, "Comment body"), hidden `--by`.
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit comment add <id> --body <text>"}` → exit 2
  (`cli.go`).
- Store refusal: a blank body (after trim) →
  `errors.New("comment body is required")` → exit 1
  (`internal/store/store.go`). A missing issue → exit 4
  (`store.go`).
- The comment id is `"cmt-" + uuid` and `CreatedBy` empty is normalized to
  `"unknown"` (`store.go`).
- Dispatches `EventCommentAdded` (`cli.go`).
- Output: `printComment` → `"<issueID> <commentID>\n"` (`cli.go`).
  **No breadcrumb.**

**`lit comment rm <comment-id>`** (`commentRmLeaf`, `cli.go`):
- No flags (not even `--by`).
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit comment rm <comment-id>"}` → exit 2 (`cli.go`).
- Store: blank id → `"comment id is required"`; unknown id →
  `storage.NotFoundError{Entity: "comment", ID: id}` → exit 4
  (`internal/store/store.go`).
- Output: `"<issueID> <commentID>\n"` for the deleted comment (`cli.go`).

### 2.18 `lit label` — Manage labels

Family `labelFamily`, usage `"usage: lit label <add|rm> ..."`
(`issue_relations.go`); both `app.AccessWrite`.

**`lit label add <issue-id> <label>`** (`issue_relations.go`):
- Declares `positionals: 2` (`issue_relations.go`); `parseLeaf` splits once.
  Hidden `--by` registered.
- Refusal: `len(positional) != 2` →
  `UsageError{"usage: lit label add <issue-id> <label>"}` → exit 2
  (`issue_relations.go`).
- Calls `Store.AddLabel(AddLabelInput{IssueID, Name, CreatedBy: resolveActor()})`
  (`issue_relations.go`); labels are normalized through `model.NormalizeLabel`
  (`internal/store/labels.go`).
- Output: the resulting full label set, comma-joined on one line
  (`printLabels`, `output.go`), then the `update` breadcrumb.

**`lit label rm <issue-id> <label>`** (`issue_relations.go`):
- Same shape; no `--by`. Usage `"usage: lit label rm <issue-id> <label>"`.
- Calls `Store.RemoveLabel(issueID, label)`; prints the remaining labels and the
  `update` breadcrumb.

Reserved label semantics: `needs-design` blocks readiness (§1.18, `ready_state.go`);
`focus` marks a goal for focus-path ordering (`ready_state.go`).

### 2.19 `lit parent` — Manage parent relationships

Family `parentFamily`, usage `"usage: lit parent <set|clear> ..."`
(`issue_relations.go`); both `app.AccessWrite`. Group `structure`
(`register.go`).

**`lit parent set --child <id> --parent <id>`** (`issue_relations.go`):
- Flags: `--child` ("Child issue ID (required)"), `--parent` ("Parent issue ID
  (required)"), hidden `--by`.
- Refusals, in order: blank `--child` or blank `--parent` →
  `UsageError{"usage: lit parent set --child <id> --parent <id>"}` → exit 2
  (`issue_relations.go`).
- Calls `Store.SetParent(SetParentInput{ChildID, ParentID, CreatedBy})`
  (`issue_relations.go`).
- Output: the edge rendered through the *same* projection `dep` uses —
  `"<child> --child-of--> <parent>"` (`issue_relations.go`, via
  `depRelationForCLI`/`depRelationLine`, `dependency.go`) —
  then the `update` breadcrumb.

**`lit parent clear <child-id>`** (`issue_relations.go`):
- No flags. Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit parent clear <child-id>"}` → exit 2
  (`issue_relations.go`). Surplus positionals refused by
  `refuseSurplusPositionals` (`register.go`) before `parent clear`'s work runs.
- Calls `Store.ClearParent(childID)`; prints `ok` then the `update` breadcrumb.

### 2.20 `lit dep` — Manage dependency edges

Family `depFamily`, usage `"usage: lit dep <add|rm|ls> ..."` (`dependency.go`).
`add`/`rm` are `app.AccessWrite`, `ls` is `app.AccessRead`. Group `structure`
(`register.go`).

**`lit dep add --from <id> --to <id> [--type ...]`** (`dependency.go`):
- Flags: `--type` (string, default `"blocks"`, help "Relation type:
  blocks|parent-child|related-to"), `--from` ("Source issue ID (required)"),
  `--to` ("Target issue ID (required)"), hidden `--by`.
- Refusals, in order:
  1. Blank `--from` or `--to` →
     `UsageError{"usage: lit dep add --from <id> --to <id> [--type blocks|parent-child|related-to]"}`
     → exit 2 (`dependency.go`).
  2. Bad `--type` → the bare `model.ParseRelationType` error → exit 1
     (`dependency.go`).
  3. Self-loop `from == to` → `model.ValidationError{Message: fmt.Sprintf("dep add: self-loop rejected (%s -> %s)")}`
     → exit 1 (`dependency.go`). Transitive cycles are **not** detected
     (`dependency.go`).
  4. For `blocks` only: `rejectSameEpicBlocks` — if both endpoints are leaves of
     the same epic, `ValidationError{storage.SameEpicBlocksRejectionMessage}` →
     exit 3 (`dependency.go`). Verbatim message:
     "Do not set 'blocks' relationships between two issues in the same epic.  Use
     rank to specify that one issue must be completed before another issue"
     (`internal/storage/edges.go` — note the double space).
     Leaf epic: the parent's ID if the issue is not a container and its parent
     is, else `""` (`leafEpicID`, `dependency.go`). Two floating issues are not
     same-epic (`dependency.go`). An endpoint inside the other's epic at any
     depth, climbing only through epic parents, is refused with the same message by the store's `AddRelation`
     (`storage.RejectBlocksAlongHierarchy`, `internal/storage/edges.go`).
- Endpoint orientation: `rt.StoreEndpoints(from, to)` swaps the pair for `blocks`
  (stored dependent→dependency) and is an involution
  (`dependency.go`, `internal/model/relation_type.go`).
- Output: `depRelationLine(depRelationForCLI(rel))` then the `update` breadcrumb
  (`dependency.go`). Line formats (`dependency.go`):
  - `blocks` → `"<src> --blocks--> <dst>"`
  - `parent-child` → `"<src> --child-of--> <dst>"`
  - `related-to` → `"<src> --related-to--> <dst>"`
  - default → `"<src> --depends-on--> <dst>"`

**`lit dep rm --from <id> --to <id> [--type ...]`** (`dependency.go`):
- Same flags minus `--by`; same usage refusal string with `rm`
  (`dependency.go`). No self-loop or same-epic check.
- Calls `Store.RemoveRelation(srcID, dstID, rt)`; prints `ok` and the `update`
  breadcrumb (`dependency.go`).

**`lit dep ls <issue-id> [--type ...]`** (`dependency.go`):
- One positional; `--type` (string, `""`, "Filter relation type").
- Refusal: `len(positional) != 1` →
  `UsageError{"usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to]"}`
  → exit 2 (`dependency.go`).
- A non-blank `--type` is parsed (bad value errors); blank means no filter
  (`dependency.go`).
- Output: one `depRelationLine` per relation, flipped back to CLI orientation
  (`dependency.go`). No breadcrumb, no header, empty output for none.

### 2.21 `lit bulk` — Bulk issue operations

Family `bulkFamily`, usage `"usage: lit bulk <label|close|archive> ..."`
(`bulk.go`). Group `operations` (`register.go`).
Rows: `label` (write), `close` (write), `archive` (write), and hidden
`import` (`retiredSubcommand`, retired).

**Shared failure semantics** — `runBulkOver(stdout, ids, op)` (`bulk.go`):
- Applies `op` to each id **in order**.
- Each success writes `"<id> ok\n"` to stdout (`bulk.go`).
- Each failure is collected; **failures never reach stdout** (`bulk.go`).
- If any failed, returns `BulkFailureError{Failures}` → exit 1
  (`bulk.go`, `exit.go`), whose message is
  `"bulk operation: <n> item(s) failed: <id>: <err>; <id>: <err>"`
  (`bulk.go`), printed by `WriteCommandError` to stderr with the
  `bulk_partial_failure` remediation (§1.9).

**`lit bulk label <add|rm> --ids <csv> --label <name>`** (`bulkLabelLeaf`,
`bulk.go`):
- Nested family `bulkLabelFamily`, usage `"usage: lit bulk label <add|rm> ..."`
  (`bulk.go`).
- With zero args after `label` → `errors.New(bulkLabelFamily.usage)` → exit 1
  (`bulk.go`).
- Flags parsed from `args[1:]`: `--ids` ("Comma-separated issue IDs"),
  `--label` ("Label name"), hidden `--by` (`bulk.go`).
- **Error precedence is deliberate** (`bulk.go`): empty `--ids` →
  `ValidationError{"--ids is required"}` → exit 3; then blank `--label` →
  `ValidationError{"--label is required"}` → exit 3; only *then* is an unknown
  add/rm action resolved (`bulk.go`).
- `add` calls `Store.AddLabel` with the resolved actor; `rm` calls
  `Store.RemoveLabel` (actor unused) (`bulk.go`).

**`lit bulk close --ids <csv> --resolution <res> [--of <id>] [--reason <text>]`**
(`bulkCloseLeaf`, `bulk.go`):
- Flags: `--ids`, `--reason` ("Lifecycle reason"), `--resolution` + `--of` (via the
  shared `registerCloseOutcomeFlags`), hidden `--by` (`bulk.go`).
- Empty `--ids` → `ValidationError{"--ids is required"}` → exit 3
  (`bulk.go`).
- Outcome via the same `closeOutcomeFromFlags` gate as `lit close`, with usage
  string `"usage: lit bulk close --ids <id,id,...> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]"`
  (`bulk.go`) — so `--resolution` is required and `--of` is required
  exactly for `duplicate`/`superseded`.
- One shared outcome, actor and reason applied to every id via
  `Store.Apply(Change{Action: model.Close{Outcome}, Actor, Reason})`
  (`bulk.go`).
- **No workflow events, no close-adjacency block, no breadcrumb** on the bulk path.

**`lit bulk archive --ids <csv> [--reason <text>]`** (`bulkTransitionLeaf(model.Archive{})`,
`bulk.go`, registered at `bulk.go`):
- Flag set name is `"bulk archive"` derived from `action.Name()` (`bulk.go`).
- Flags: `--ids`, `--reason`, hidden `--by`.
- Empty `--ids` → `ValidationError{"--ids is required"}` → exit 3 (`bulk.go`).
- Applies `model.Archive{}` per id.

**`lit bulk import`** (hidden, retired) →
`RetiredCommandError{Command: "bulk import", Replacement: bulkImportRetirementGuidance}`
→ exit 3 (`bulk.go`). Guidance verbatim:
"use `lit backup restore --path <export.json>` — it owns the same export-restore
mechanism `bulk import` duplicated" (`register.go`). The row is built by
`retiredSubcommand` (`register.go`), and a row carrying a `retired` error
returns it before any parse or workspace open, so the pointer is returned even
outside a git repository (`register.go`; asserted in `retired_command_test.go`).

### 2.22 `lit export`

- Registration `register.go`, `app.AccessRead`. Summary: "Write the whole
  workspace out as one versioned JSON export (the data-export primitive;
  `backup restore` reads it back)".
- Handler `exportLeaf` (`cli.go`): no flags of its own; parses argv (so
  `--help` works and any flag is an unknown-flag `UsageError`); calls
  `Store.Export(ctx)`; writes the result as **two-space-indented JSON** to stdout
  (`cli.go`, `writeJSON` at `cli.go`).
- No positional check.

### 2.23 `lit import --path <file>`

- Registration `register.go`, `app.AccessWrite`. Summary: "Bulk-create/update
  issues from a file (the one bulk-ingest home): a JSON tree spec, or a YAML file
  for create-or-update by id selector".
- Handler `importTreeLeaf` (`cli.go`).
- Flags: `--path` (string, `""`, "Path to a JSON tree-spec file or a YAML bulk
  create/update file"), hidden `--by` (`cli.go`).
- Refusals: blank `--path` (after trim) →
  `UsageError{importUsage}` → exit 2. `importUsage` verbatim:
  `"usage: lit import --path <tree-spec.json | bulk-file.yaml> (run `lit import --help` for both formats)"`
  (`cli.go`).
- Reads the file; a read error → `fmt.Errorf("read import spec: %w", err)` → exit 1
  (`cli.go`).
- **Format is selected by the file extension** (lowercased) (`cli.go`):
  - `.yaml` / `.yml` → `runImportBulk`
  - anything else → `runImportTreeJSON`, but first: if `--by` was set →
    `UsageError{"usage: --by only applies to a YAML bulk-update file (--path *.yaml|*.yml); JSON tree-spec import always attributes creates to \"links\""}`
    → exit 2 (`cli.go`).

**JSON tree path** (`runImportTreeJSON`, `cli.go`):
- `storage.ParseImportTreeSpecs(data)` then
  `Store.ImportTree(ctx, prefix, specs)`.
- Documented spec shape (`cli.go`): an array of records each with
  `local_id`, optional `parent` (a local_id), optional `depends_on` (array of
  local_ids), `title`, `type`, `topic`, `priority`.
- Output: `"imported %d issues\n"` then, per mapping, `"  <local> -> <real>\n"`
  (map iteration order is unspecified) (`cli.go`).
- Best-effort rollback on failure is the store's behavior; the doc comment tells
  the caller to run `lit doctor` after a failed import (`cli.go`).

**YAML bulk path** (`runImportBulk`, `cli.go`):
- `storage.ParseBulkSpecs(data)`.
- If `--by` was set but no document has an `id` (i.e. no update documents) →
  `UsageError{"usage: --by only applies when the file has at least one update document (a document with \`id\` set); this file has none"}`
  → exit 2 (`cli.go`, `bulkSpecsHaveUpdate` at `cli.go`).
- Calls `Store.BulkApply(ctx, prefix, actor, specs)`.
- Documented YAML shape (`cli.go`): one document per issue separated by
  `---`; optional `local_id` for intra-file references; `id` present means
  **update** that issue instead of creating; `parent` may name a local_id or a
  real issue ID.
- Output (`cli.go`):
```
created <n> issues
  <ref> -> <realID>
  ...
updated <n> issues
  <id>
  ...
```

### 2.24 `lit prefix set <new-prefix> [--apply]`

- Registration `register.go`, workspace-mode (no store). Group
  `maintenance`. Summary: "Manage the cosmetic issue ID prefix".
- `prefixFamily` (`prefix.go`), dispatched by `resolve` (`register.go`): a
  first argument of `-h`/`--help` answers help; any other invocation whose
  `args[0]` is not the literal `set` (including no args) →
  `UsageError{"usage: lit prefix set <new-prefix> [--apply]"}` → exit 2.
- `prefixSetLeaf` (`prefix.go`): one positional, flag `--apply` (bool, false,
  "Apply the rename (without this flag, prints a preview)").
  - `len(positional) != 1` → the same usage `UsageError`
    (`prefix.go`).
  - `workspace.ConfiguredPrefix(requested)` failure →
    `ValidationError{Message: fmt.Sprintf("invalid prefix %q: %v", requested, err)}` →
    reason `validation_refused`, exit 3 (`prefix.go`). Typed so a deterministic
    refusal does not reach the unclassified default's retry-then-doctor remediation.
- Three outcomes (`prefixSetTextOutput`, `prefix.go`):
  - Normalized == current → `"issue_prefix: <p> (prefix unchanged)\n"`
    (`prefix.go`).
  - Changed, no `--apply` →
    ```
    issue_prefix: <old> -> <new> (preview)
      preview only — pass --apply to write config.json. Existing issue IDs keep their old prefix; only new issues use the new one.
      Run with --apply to write config.json.
    ```
    (`prefix.go`).
  - Changed with `--apply` → `workspace.UpdateConfig` writes `IssuePrefix`; a
    failure → `fmt.Errorf("update workspace config: %w", err)`; success prints
    `"issue_prefix: <old> -> <new> (applied)\n"` (`prefix.go`).

### 2.25 `lit workspace`

- Registration `register.go`, workspace-mode. Summary: "Show workspace
  metadata". Handler `workspaceLeaf` (`cli.go`).
- No flags of its own; parses argv so `--help` works.
- Output: one `key: value` line per field, in this exact order
  (`cli.go`): `workspace_id`, `issue_prefix`, `git_common_dir`,
  `storage_dir`, `database_path`, `dolt_repo_path`, `traces_dir`.

### 2.26 `lit completion <bash|zsh|fish>`

- Registration `register.go`. Group `guidance`. Summary: "Generate shell
  completion script". Its own advertised subcommands come from
  `completionFamily.visibleSubcommands()`.
- `completionFamily` — usage `"usage: lit completion <bash|zsh|fish>"`, rows
  `bash`, `zsh`, `fish` (`cli.go`).
- `runCompletion(stdout, args)` (`cli.go`): `len(args) != 1` →
  `errors.New(completionFamily.usage)` → exit 1; an unknown shell → the same
  usage error via `resolve`; otherwise writes the generated script to stdout.
- `completionRenderer(shell)` panics for any shell not in the switch
  (`completion.go`).

**Completion model** (`commandCompletionModel`, `completion.go`): projects
`commandSpecs`, drops every `Hidden` spec, and appends a synthetic
`{Name: "help", Summary: "Help about any command"}` row. Retired commands never
appear in completion.

`familyNodes` flattens every command/subcommand that has children into
(trigger-word → children) pairs at any depth, de-duplicating by name and
**unioning** children when a word appears under two parents (e.g. `label` as both
a top-level command and a `bulk` subcommand) (`completion.go`).

- **bash** (`completion.go`): defines `_lit_completions` using
  `_init_completion`, a `commands` variable holding the top-level names, a
  `case "${prev}"` with a `lit)` arm and one arm per family node, and a fallback
  `compgen -W "${commands}"`. Ends with `complete -F _lit_completions lit`.
- **zsh** (`completion.go`): `#compdef lit`, a `commands` array of
  `'name:summary'` entries (single quotes in the summary escaped as `'\''`,
  `completion.go`), `_arguments '1:command:->command' '2:subcommand:->subcommand'`,
  a `_describe` for commands and a per-command `_values` arm for each command that
  has subcommands (only **top-level** commands' direct subcommands, not the
  flattened nodes).
- **fish** (`completion.go`): `complete -c lit -f`, one
  `__fish_use_subcommand` line with the top-level names, and one
  `__fish_seen_subcommand_from <name>` line per family node.

Subcommand trees fed into the registry (`register.go`): `sync` nests
`remote` and `reconcile`; `bulk` nests `label`. Explicit literals: `workflows`
declares `show`, `edit`, `dry-run` (`register.go`).

### 2.27 Retired commands (hidden, dispatchable)

Registered with `Hidden: true` and a `retiredCommandRun(command, replacement)`
handler that runs nothing and returns `RetiredCommandError` → exit 3
(`register.go`). Hidden specs are excluded from root `--help` and from
completion (`register.go`, `completion.go`).

| Command | Group | Replacement guidance (verbatim) | Citation |
|---|---|---|---|
| `ready` | operations | "use `lit backlog` for the full ranked queue (blocked items shown inline) or `lit next` for the single leaf to start" | `register.go` |
| `queue` | operations | same as `ready` | `register.go` |
| `assign` | operations | "reassigning is a field write: use `lit update <id> --assignee <name>` (with an optional `--reason`)" | `register.go` |
| `ls-at` | maintenance | "use `lit ls --at <store-dir>` — listing a discovered store read-only is now a flag on `ls`, not a separate command" | `register.go` |
| `overview` | maintenance | "use `lit stores --counts` — the cross-project ready / in-flight / blocked rollup is now a flag on `stores`" | `register.go` |
| `bulk import` | (bulk family) | "use `lit backup restore --path <export.json>` — it owns the same export-restore mechanism `bulk import` duplicated" | `bulk.go`, `register.go` |

Full error message form: `the "<command>" command has been retired; <replacement>`
(`cli.go`). Reason `retired_command`, remediation empty
(`error_output.go`). Asserted in
`retired_command_test.go`.

Retired **flags** (intercepted by the shared parser, §1.6): `--output` anywhere
(`cli.go`) and `--continue` (`cli.go`), both
`UnsupportedError`; `lit update --status` (`cli.go`), a `UsageError`.

### 2.28 `lit quickstart` (in-scope only as it is the bare-`lit` default)

`quickstartLeaf` (`cli.go`) — flags `--refresh` (bool),
`--eject` (string-optional; present-with-no-value = `"all"`), `--force` (bool),
plus at most one positional topic.
- More than one positional → refused by `refuseSurplusPositionals` (`register.go`),
  called from `parseLeaf` (`register.go`), before `quickstartLeaf`'s work
  (`cli.go`) runs, using `quickstartUsage`, where
  `quickstartUsage = "usage: lit quickstart [<topics|…>] [--refresh] [--eject[=LIST]] [--force]"`
  built from the topic token list (`quickstart_topics.go`).
- `--refresh` with `--eject` → `UsageError{"usage: --refresh and --eject are mutually exclusive"}`
  (`cli.go`).
- `--force` without `--eject` → `UsageError{"usage: --force is only valid with --eject"}`
  (`cli.go`).
- A topic positional combined with any flag →
  `UsageError{"usage: lit quickstart <topic> takes no flags"}` (`cli.go`).
- An unknown topic →
  `UsageError{"usage: unknown quickstart topic \"<x>\" (must be one of: <tokens>)"}`
  (`cli.go`).

---

## PART 3 — CROSS-CUTTING OBSERVATIONS (behavioral, non-editorial)

1. **JSON output exists on exactly one command in this scope**: `lit export`
   (`cli.go`). Every other command emits line-oriented text. `--output` is
   rejected globally and per-command (§1.1, §1.6).
2. **Surplus positionals are refused for every command**: `refuseSurplusPositionals`
   (`register.go`), called once from `parseLeaf` (`register.go`) before any
   leaf's work runs — `new`, `followup`, `ls`, `rank`, `export`, `children`, and
   `parent clear` included.
3. **Family dispatch errors are plain errors (exit 1), not `UsageError` (exit 2)**
   (`register.go`), unlike the per-command usage refusals which are
   `UsageError` (exit 2). Likewise `transitionLeaf`'s wrong-arity refusal
   (`cli.go`) and `runCompletion`'s (`cli.go`) are exit 1.
4. **`--help` output goes to stdout, not stderr**, and exits 0
   (`cli.go`).
5. **Assignee identity diverges by command on purpose**: `start` resolves through
   `resolveIdentity` (env `CLAUDE_CODE_SESSION_ID` wins) (`cli.go`);
   `update --assignee` writes the trimmed literal, empty meaning clear
   (`cli.go`). `new`/`followup` also write the trimmed literal
   (`cli.go`).
6. **Claim state never blocks anything except `lit start` on a fresh foreign
   hold.** `backlog` renders claims as visibility only (`backlog.go`); `next` routes by claim but never writes (`next_route.go`);
   `start` is the only gate (`cli.go`, `relationOf` at
   `claims_takeover.go`).
7. **Three functions panic on unreachable states** and would abort the process:
   `ClassifyReadiness` on an unclassified annotation kind (`readiness.go`),
   `renderNextOutcome` on an unhandled outcome type (`next.go`),
   `transitionOccasion` on an unmapped status action (`workflow_events.go`),
   `emitBreadcrumb`/`quickstartBreadcrumb` on an unknown topic
   (`quickstart_topics.go`), `completionRenderer` on an unknown shell
   (`completion.go`), `nestUnder` on a missing nest point (`register.go`),
   and `store.planLifecycleAction` on an impostor action
   (`internal/store/store.go`).
8. **The `done` "requires in_progress" claim in the registry summary
   (`register.go`) has no enforcing code path** — see §2.10.
