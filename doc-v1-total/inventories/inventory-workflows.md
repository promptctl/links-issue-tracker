# Behavioral Inventory — workflows / eventing subsystem (`lit`)

Derived entirely from Go source under `/Users/bmf/code/links-issue-tracker`. No markdown/docs consulted.
All paths below are repo-relative; every claim carries a `file:line` citation.

**Terminology note up front, because the name is misleading:** in this codebase a "workflow" is
**not** an executed script, hook, or automation. It is a markdown file whose body is **printed
verbatim to the calling command's stdout** when a matching lifecycle moment occurs.
`internal/workflows/workflows.go`: "Workflow bodies are injected text, never executed, and no
lifecycle is enforced or gated — this package only answers 'which guidance is in place, and does it
apply to this moment'." There is no execution environment, no subprocess, no env vars passed to a
workflow, and no timeouts, because nothing is ever run. The only subprocess anywhere in this
subsystem is `$EDITOR`, launched by `lit workflows edit` (`internal/cli/workflows_edit.go`).

---

## 1. The workflow definition file format

### 1.1 File shape

A definition file is a **markdown file with optional YAML frontmatter**
(`internal/workflows/workflows.go`).

Frontmatter delimiter rules (`internal/workflows/parse.go`):

- The delimiter literal is exactly `---` (`internal/workflows/parse.go`).
- A line counts as a delimiter if, after stripping trailing spaces, tabs, and `\r`, it equals `---`
  (`internal/workflows/parse.go`). So `---   ` and `---\r` both work
  (`internal/workflows/parse_test.go` pin CRLF files and trailing-whitespace delimiters).
- The **first line of the file** must be a delimiter for frontmatter to exist at all. If it isn't,
  `found=false` and the **entire file content is the body**, with no frontmatter
  (`internal/workflows/parse.go`). Such a file loads successfully but is inert — see §1.7
  (`internal/workflows/parse_test.go`).
- Scanning then proceeds line by line until the next delimiter line; that line closes the header,
  and everything after it is the body (`internal/workflows/parse.go`).
- A frontmatter block opened but never closed is an error: `"unterminated frontmatter: missing
  closing ---"` (`internal/workflows/parse.go`). This makes the file **malformed**: it is
  skipped entirely and produces exactly one warning (`internal/workflows/parse.go`,
  `internal/workflows/parse_test.go`).
- The body is stored with leading/trailing whitespace trimmed
  (`internal/workflows/parse.go`, `strings.TrimSpace(body)`).

### 1.2 Frontmatter keys — the complete list

The YAML struct is `internal/workflows/parse.go`:

| YAML key | Go type | Meaning |
|---|---|---|
| `id` | string | `internal/workflows/parse.go` |
| `name` | string | `internal/workflows/parse.go` |
| `labels` | list of string | `internal/workflows/parse.go` |
| `states` | list of StateActivation | `internal/workflows/parse.go` |
| `events` | list of string | `internal/workflows/parse.go` |

**Unknown keys are tolerated and ignored** — deliberately, so a file authored for a newer `lit`
still loads (`internal/workflows/parse.go`; pinned by
`internal/workflows/parse_test.go`). There is no `yaml.KnownFields` strictness.

Frontmatter must be a **YAML mapping**. A sequence (`- a\n- b`) or a bare scalar (`hello`) at the
top level is malformed and the file is skipped (`internal/workflows/parse_test.go` cases
"non-mapping frontmatter" and "scalar frontmatter" — they fail `yaml.Unmarshal` into the struct at
`internal/workflows/parse.go`).

`labels: 17` (a non-sequence for a sequence-typed key) is likewise malformed and skips the file
(`internal/workflows/parse_test.go`).

### 1.3 `id`

- Trimmed of surrounding whitespace (`internal/workflows/parse.go`, via
  `strings.TrimSpace(meta.ID)`; pinned `internal/workflows/parse_test.go` — `'  spaced  '`
  → `spaced`).
- If empty/absent, it defaults to `defaultID(path)`: the **layer-relative file path** with the
  `.md` suffix removed and every space replaced by `_` (`internal/workflows/parse.go`).
  Example: `review tasks/design check.md` → `review_tasks/design_check`
  (`internal/workflows/parse_test.go`). Directory separators are **preserved** in the ID.
- The ID is the **primary key across layers**: a nearer layer's definition with the same ID
  overrides the farther one (`internal/workflows/workflows.go`, `internal/workflows/load.go`).
- ID matching in `Set.Lookup` is exact string equality; no case folding
  (`internal/workflows/workflows.go`).

### 1.4 `name`

Optional pretty display name; trimmed (`internal/workflows/parse.go`). Used only for display:
`formatDefinitionRef` renders `<id> "<name>" (<source>)` when non-empty, else `<id> (<source>)`
(`internal/cli/workflows.go`).

### 1.5 `labels` — activation dimension 1

- Each entry is passed through `model.NormalizeLabel` (`internal/workflows/parse.go`), which
  lowercases and trims (`internal/model/label.go`), rejects empty (`internal/model/label.go`),
  and rejects any label containing a comma (`internal/model/label.go`).
- Entries that trim to empty are **silently dropped** as authoring noise
  (`internal/workflows/parse.go`).
- An entry `NormalizeLabel` rejects (i.e. containing a comma) is dropped **with a warning**:
  `label %q can never match: %v` (`internal/workflows/parse.go`).
- Result: definition labels are stored in exactly the canonical form the store persists, so match
  comparison downstream is exact-string and therefore case-insensitive by construction
  (`internal/workflows/parse.go`). Pinned `internal/workflows/parse_test.go`.

### 1.6 `states` — activation dimension 2

A `states:` entry accepts **two authored shapes** (`internal/workflows/parse.go`):

1. **A bare scalar state name** — e.g. `states: [open]` or a YAML block sequence item `- open`.
   This means `when: enter` (`internal/workflows/parse.go`).
2. **A mapping** with keys `name` and optional `when` — e.g.
   `states: [{name: closed, when: exit}]` (`internal/workflows/parse.go`).

Rules:

- `when` legal values: `enter`, `exit`. Absent/empty means `enter`
  (`internal/workflows/parse.go`). Comparison is `strings.ToLower(strings.TrimSpace(raw))`, so
  ` EXIT ` works (`internal/workflows/parse.go`; pinned `internal/workflows/parse_test.go`).
- Any other `when` value is a **parse error** that makes the whole file malformed and skipped —
  error text `when must be "enter" or "exit", got %q` (`internal/workflows/parse.go`; pinned
  case "invalid when" `internal/workflows/parse_test.go`).
- A mapping entry with a name that trims to empty is a parse error: `state entry mapping requires a
  non-empty name` (`internal/workflows/parse.go`; pinned case "nameless state mapping"
  `internal/workflows/parse_test.go`).
- Any other YAML node kind (e.g. a nested sequence `- [open]`) is a parse error: `state entry must
  be a state name or a {name, when} mapping` (`internal/workflows/parse.go`; pinned case
  "invalid state entry kind" `internal/workflows/parse_test.go`).
- State names are canonicalized to `strings.ToLower(strings.TrimSpace(name))`
  (`internal/workflows/parse.go`). Pinned `internal/workflows/parse_test.go`.
- A **bare empty scalar** entry (`states: ['', '  open ']`) is dropped without warning by
  `compactStates` (`internal/workflows/parse.go`; pinned normalization test
  `internal/workflows/parse_test.go`).
- **State names are open strings, deliberately not a closed enum** (`internal/workflows/workflows.go`).
  Custom stage names are legal and require no code change. A state name may contain a colon or a
  comma (`internal/cli/workflows_test.go` scaffolds a state literally named `foo,bar`).
- The built-in three states are `open`, `in_progress`, `closed`
  (`internal/model/lifecycle/lifecycle.go`, surfaced as `builtinStates` at
  `internal/cli/workflows.go`).

Duplicate activations for the same state with different `when` are allowed and preserved as two
entries (`internal/workflows/parse_test.go`: `open(enter)`, `in_progress(enter)`,
`in_progress(exit)`).

### 1.7 `events` — activation dimension 3

- Entries are canonicalized to `strings.ToLower(strings.TrimSpace(value))`, and entries that trim to
  empty are dropped (`internal/workflows/parse.go`; pinned
  `internal/workflows/parse_test.go`).
- An event **not in this binary's catalog still loads**, preserved as authored, plus a warning:
  `unknown event %q: not in this lit's catalog, will never fire here`
  (`internal/workflows/parse.go`, `internal/workflows/events.go`; pinned
  `internal/workflows/parse_test.go`).

### 1.8 Inertness

`Definition.Inert()` is true when **all three** of Labels, States, Events are empty
(`internal/workflows/workflows.go`). An inert definition:

- loads successfully and is visible in `lit workflows` listings,
- produces a warning at parse time: `no activation keys (labels/states/events): definition is inert
  and will never fire` (`internal/workflows/parse.go`),
- **never matches anything** — `Matches` short-circuits false on inert
  (`internal/workflows/match.go`).

This is why a plain markdown file with no frontmatter loads but never fires
(`internal/workflows/parse_test.go`).

### 1.9 Body and the `<id>` substitution

- The body is everything after the closing delimiter, `TrimSpace`d
  (`internal/workflows/parse.go`).
- Exactly **one** substitution is applied at injection time: every occurrence of the literal
  string `<id>` is replaced with the occasion's IssueID
  (`internal/workflows/dispatch.go`, `strings.ReplaceAll(body, "<id>", issueID)`).
- When the occasion carries no issue ID (e.g. `show_backlog`), `<id>` is replaced with the empty
  string — the substitution is unconditional (`internal/workflows/dispatch.go`).
- The comment at `internal/workflows/dispatch.go` records that a prior mechanism also
  supported `<token>`, which is **not** supported here.

### 1.10 A complete, valid example file

The one file shipped as an embedded default, `internal/workflows/defaults/done.md`:

```
---
id: done
name: Post-close capture reminder
events: [work_finished]
---
Ticket <id> has been closed. Before moving on, take a moment to review related tickets. …
```

(Body text in full at `internal/workflows/defaults/done.md`.)

---

## 2. Where workflow files live, and how they load

### 2.1 The three layers, in precedence order

`internal/workflows/load.go`:

1. **Project**: `<workspaceRoot>/.lit/workflows/` → `templates.SourceProject`
   (`internal/workflows/load.go`, source constant `internal/templates/templates.go`).
2. **Global**: `<config.ConfigDir()>/workflows/` → `templates.SourceGlobal`
   (`internal/workflows/load.go`, source constant `internal/templates/templates.go`).
   `config.ConfigDir()` is `$XDG_CONFIG_HOME/links-issue-tracker` if `XDG_CONFIG_HOME` is set, else
   `$HOME/.config/links-issue-tracker`, else `""` if the home dir cannot be determined
   (`internal/config/config.go`). An empty PathSpec contributes no layer at all
   (`internal/workflows/load.go`).
3. **Embedded**: compiled into the binary via `//go:embed defaults/*.md`
   (`internal/workflows/load.go`), rooted at `defaults` so entries walk with root-relative
   paths identical to the other two layers → `templates.SourceEmbedded`
   (`internal/templates/templates.go`). Today the tree contains exactly one file, `done.md`.

### 2.2 Discovery within a layer

`loadLayer` (`internal/workflows/load.go`):

- Walks the layer root **recursively** with `fs.WalkDir` from `"."`
  (`internal/workflows/load.go`).
- Directories are skipped; a file participates **iff its path ends in `.md`**
  (`internal/workflows/load.go`). Case-sensitive suffix check — `.MD` does not participate.
- **The folder hierarchy carries no activation meaning whatsoever.** Nesting only seeds the default
  ID (`internal/workflows/load.go`, `internal/workflows/workflows.go`). Pinned end-to-end
  at `internal/cli/workflow_injection_test.go` (a file at
  `.lit/workflows/reviews/deep/nested/close.md` fires normally).
- Walk order is lexical, so "first wins" for within-layer duplicates is deterministic
  (`internal/workflows/load.go`).

### 2.3 Failure handling during load — Load can never fail

`Load` returns a `Set`, no error (`internal/workflows/load.go`). Every problem is a `Warning`
(`internal/workflows/workflows.go`) with `{Source, Path, Message}`:

| Condition | Message | Citation |
|---|---|---|
| Walk error that is `fs.ErrNotExist` (absent layer root) | *(none — genuine absence, not failure)* | `internal/workflows/load.go` |
| Any other walk error | `cannot read: <err>` | `internal/workflows/load.go` |
| File read error | `cannot read: <err>` | `internal/workflows/load.go` |
| Unterminated frontmatter | `unterminated frontmatter: missing closing ---` | `internal/workflows/parse.go` |
| YAML unmarshal failure | `invalid frontmatter: <err>` | `internal/workflows/parse.go` |
| Comma-bearing label | `label %q can never match: <err>` | `internal/workflows/parse.go` |
| Inert definition | `no activation keys (labels/states/events): definition is inert and will never fire` | `internal/workflows/parse.go` |
| Unknown event | `unknown event %q: not in this lit's catalog, will never fire here` | `internal/workflows/parse.go` |
| Duplicate ID *within one layer* | `duplicate id %q (already defined by %s): file ignored` | `internal/workflows/load.go` |

A malformed file is skipped; other files in the same layer still load
(`internal/workflows/load_test.go`).

### 2.4 Merge / override semantics

- Within a layer: first file in lexical walk order claims an ID; later files with the same ID are
  **ignored with a warning** (`internal/workflows/load.go`; pinned
  `internal/workflows/load_test.go`).
- Across layers: the first layer to claim an ID wins outright; farther layers with that ID are
  skipped **silently — no warning** (`internal/workflows/load.go`). This is the override
  feature (pinned `internal/workflows/load_test.go`).
- Override does not merge fields. A project file with `id: done` **wholly replaces** the embedded
  `done` default, including its body (`internal/cli/workflow_injection_test.go`, which asserts
  the embedded body no longer appears).
- Because the default ID is the layer-relative path, placing a file at the *same relative path* in a
  nearer layer overrides without authoring an explicit `id`
  (`internal/workflows/parse.go`; `internal/workflows/load_test.go`).
- The resolved `Set.Definitions` is sorted by ID ascending
  (`internal/workflows/load.go`), giving a stable iteration/display/injection order
  (`internal/workflows/workflows.go`).
- Warnings from **all** layers accumulate, including layers whose definitions were overridden
  (`internal/workflows/load.go`).

---

## 3. The complete event catalog

Ten events, defined as string constants at `internal/workflows/events.go`:

| Event constant name | Wire name | Fires when | Command |
|---|---|---|---|
| `EventShowBacklog` | `show_backlog` | agent views the workable backlog | `lit backlog` — `internal/workflows/events.go` |
| `EventShowTicket` | `show_ticket` | agent views one ticket's details | `lit show` — `internal/workflows/events.go` |
| `EventNextPulled` | `next_pulled` | agent asks for the next workable ticket | `lit next` — `internal/workflows/events.go` |
| `EventWorkStarted` | `work_started` | a ticket is claimed and work begins | `lit start` — `internal/workflows/events.go` |
| `EventWorkFinished` | `work_finished` | claimed work finishes on the success path | `lit done` — `internal/workflows/events.go` |
| `EventTicketClosed` | `ticket_closed` | a ticket is closed without finishing (wontfix/obsolete/duplicate) | `lit close` — `internal/workflows/events.go` |
| `EventTicketReopened` | `ticket_reopened` | a closed ticket is reopened | `lit open` — `internal/workflows/events.go` |
| `EventTicketCreated` | `ticket_created` | a new ticket is created | `lit new`, `lit followup` — `internal/workflows/events.go` |
| `EventTicketUpdated` | `ticket_updated` | an existing ticket's fields change | `lit update` — `internal/workflows/events.go` |
| `EventCommentAdded` | `comment_added` | a comment lands on a ticket | `lit comment` — `internal/workflows/events.go` |

`Catalog()` returns them in **display order**, which differs from declaration order:
`show_backlog, next_pulled, show_ticket, work_started, work_finished, ticket_closed,
ticket_reopened, ticket_created, ticket_updated, comment_added`
(`internal/workflows/events.go`). This is the order `lit workflows` prints the Events spine
in (`internal/cli/workflows.go`).

`Event.Known()` = membership in `Catalog()` (`internal/workflows/events.go`). Catalog names
are contractually lowercase snake_case, enforced by
`internal/workflows/events_test.go` (`TestCatalogNamesAreStableContractShaped`).

Events are a **stable contract deliberately decoupled from command names** — commands may be
renamed freely, the event name is what user definitions bind to
(`internal/workflows/events.go`).

### 3.1 The Occasion payload

`Occasion` (`internal/workflows/match.go`) is the single payload type; its fields:

| Field | Meaning | Citation |
|---|---|---|
| `Event Event` | the semantic event, or zero when none | `internal/workflows/match.go` |
| `IssueID string` | acted-on ticket id; empty for backlog-wide moments. **Never read by matching** — used only for `<id>` interpolation, display, and tracing | `internal/workflows/match.go` |
| `Labels []string` | the ticket's labels in canonical form; nil when no single ticket | `internal/workflows/match.go` |
| `Entered string` | state the ticket entered, if a transition happened | `internal/workflows/match.go` |
| `Exited string` | state the ticket exited, if a transition happened | `internal/workflows/match.go` |

### 3.2 Per-event payloads, as actually built

Builders live in `internal/cli/workflow_events.go`:

| Event | Builder | Payload fields set |
|---|---|---|
| `show_ticket` | `showTicketOccasion` `internal/cli/workflow_events.go` | Event, IssueID=`issue.ID`, Labels=`issue.Labels`. No Entered/Exited. |
| `show_backlog` | `backlogOccasion` `internal/cli/workflow_events.go` | Event only. **No IssueID, no Labels, no transition.** |
| `next_pulled` | `nextPulledOccasion` `internal/cli/workflow_events.go` | Event, IssueID, Labels. |
| `ticket_created` | `ticketCreatedOccasion` `internal/cli/workflow_events.go` | Event, IssueID, Labels (the labels it was created with). |
| `ticket_updated` | `ticketUpdatedOccasion` `internal/cli/workflow_events.go` | Event, IssueID, Labels. **Never carries a transition** — `lit update` rejects `--status` (`internal/cli/workflow_events.go`; pinned `internal/cli/workflow_events_test.go`). |
| `comment_added` | `commentAddedOccasion` `internal/cli/workflow_events.go` | Event, IssueID, Labels. |
| `work_started` / `work_finished` / `ticket_closed` / `ticket_reopened` | `transitionOccasion` `internal/cli/workflow_events.go` | Event (from the action), IssueID, Labels (post-transition), `Entered = issue.State()` (post), `Exited = prior.State()` (pre). |

Status-action → event mapping (`internal/cli/workflow_events.go`):
`ActionStart→work_started`, `ActionDone→work_finished`, `ActionClose→ticket_closed`,
`ActionReopen→ticket_reopened`. A `StatusAction` with no map entry **panics**:
`workflow_events: no event mapped for status action %q` (`internal/cli/workflow_events.go`).

Retention actions (archive/unarchive/delete/restore) are **not** `StatusAction`s and therefore fire
**no event at all** — the type assertion at `internal/cli/cli.go` excludes them
(`internal/cli/workflow_events.go`; pinned `internal/cli/workflow_events_test.go`).

### 3.3 Dispatch call sites — where each event is actually fired

| Site | Event | Position in output |
|---|---|---|
| `internal/cli/cli.go` (`newLeaf`) | `ticket_created` | **after** `CreateIssue` succeeds, **before** `printIssueSummary` and the `new` breadcrumb (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`followupLeaf`) | `ticket_created` | after create, before summary/breadcrumb (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`showLeaf`) | `show_ticket` | after `GetIssueDetail`, **before** either the `--field` output or the full detail view — fires for both (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`updateLeaf`) | `ticket_updated` | after `Store.Apply`, before summary/breadcrumb |
| `internal/cli/cli.go` (`transitionLeaf`) | one of the four transition events | after `Store.Apply` and after `authorize`; **before** the claim-transfer notice at `internal/cli/cli.go`. Guarded by `action.(model.StatusAction)` (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`commentAddLeaf`) | `comment_added` | after `AddComment`, before `printComment` |
| `internal/cli/next.go` (`nextLeaf`) | `next_pulled` | **last** — after the start advice and `printNextSummary` (`internal/cli/next.go`). Only reached when a row was actually served; `Exhausted`/`NoWork` return an error before any occasion is built (`internal/cli/next.go`) |
| `internal/cli/workable.go` (`workableLeaf`) | `show_backlog` | **last** — after the table render (`internal/cli/workable.go`) |

`backlogView` is the only `workableView` that sets an `occasion` function
(`internal/cli/workable.go`); it is invoked unconditionally at
`internal/cli/workable.go` as `view.occasion(rows)`.

---

## 4. Matching semantics

`Definition.Matches(Occasion)` (`internal/workflows/match.go`):

```
Inert → false
otherwise: matchEvents(d.Events, o.Event) AND matchLabels(d.Labels, o.Labels) AND matchStates(d.States, o)
```

- **OR within a dimension, AND across dimensions.** An undeclared dimension constrains nothing
  (`internal/workflows/match.go`, `internal/workflows/workflows.go`). Pinned
  `internal/workflows/match_test.go` (AND across) (all three).
- `matchEvents` (`internal/workflows/match.go`): empty bound list → true (unconstrained).
  Otherwise the fired event must be **non-empty** and present in the bound list. So a definition
  bound to events can never fire on an occasion with no event.
- `matchLabels` (`internal/workflows/match.go`): empty bound list → true. Otherwise **at least
  one** bound label must appear in the occasion's carried labels. Exact string comparison — both
  sides are already canonicalized (`internal/workflows/parse.go`,
  `internal/workflows/match.go`).
- `matchStates` (`internal/workflows/match.go`) → `StateActivation.matches`
  (`internal/workflows/match.go`): the activation picks the occasion side matching its `When`
  (`Entered` for `enter`, `Exited` for `exit`), and requires that side to be **non-empty and exactly
  equal** to the activation's state name. "No transition happened" never satisfies a state binding.

**There are no wildcards, no globs, no regexes, and no negation** anywhere in matching. The only
"match everything" construct is *omitting* a dimension. Verify by reading the whole of
`internal/workflows/match.go` — the operations are `slices.Contains` and `==` only.

**Precedence between multiple matches: there is none — every match fires.**
`Set.Matching` returns every matching definition, in the Set's ID-ascending order
(`internal/workflows/match.go`; ordering pinned `internal/workflows/match_test.go`).
The only "precedence" in the system is the layer/ID override applied at load time (§2.4), which
means at most one definition exists per ID.

### 4.1 MatchReasons — the "why"

`Definition.MatchReasons(o)` (`internal/workflows/match.go`) returns nil if the definition
does not match; otherwise a list of strings in this fixed order:

1. `event:<the occasion's event>` — emitted once if the definition declared **any** events
   (`internal/workflows/match.go`). Note it names `o.Event`, not the bound value.
2. `label:<label>` for each of the definition's **declared** labels that appear in the occasion's
   labels (`internal/workflows/match.go`; pinned `internal/workflows/match_test.go` —
   only overlapping labels are listed).
3. `state:<state>(<when>)` for each declared activation that matched
   (`internal/workflows/match.go`).

Example from a real trace: `["event:show_ticket", "label:needs-design"]`
(`internal/workflows/dispatch_test.go`).

This is the single shared "why" computation behind both the real firing trace and `dry-run`
(`internal/workflows/match.go`).

---

## 5. Dispatch mechanics

`workflows.Dispatch(w io.Writer, errOut io.Writer, ws workspace.Info, o Occasion) error`
(`internal/workflows/dispatch.go`).

- **Synchronous, in-process, called directly from the command path — no bus, no async queue, no
  subscriber list; there is exactly one `Dispatch`, not a registry**
  (`internal/workflows/dispatch.go`).
- **It re-loads the full definition Set on every call** (`internal/workflows/dispatch.go`,
  `set := Load(ws.RootDir)`). There is no caching. Every dispatching command walks all three layers
  from disk.
- Matches, then for each match writes `Interpolate(def.Body, o.IssueID)` followed by a newline to
  `w` via `fmt.Fprintln` (`internal/workflows/dispatch.go`). `w` is the calling command's own
  **stdout** at every call site (§3.3) — the same agent-facing stream.
- Output order = `Set.Matching` order = ID-ascending
  (`internal/workflows/dispatch.go`; pinned `internal/workflows/dispatch_test.go`).
- **A write failure to `w` aborts and returns the error**, which the calling command returns
  directly (`internal/workflows/dispatch.go`; e.g. `internal/cli/cli.go`).
- **Load/parse warnings are never printed by Dispatch** — deliberately, so a workflow-authoring
  diagnostic doesn't appear on every invocation. They are only visible via `lit workflows`
  (`internal/workflows/dispatch.go`).
- No environment variables are read or set, no subprocess is spawned, no timeout exists — there is
  no execution (`internal/workflows/dispatch.go` in full; `internal/workflows/workflows.go`).

### 5.1 Blocking / exit-code interaction

- Dispatch is a blocking function call on the command's own goroutine; the command does not
  continue until it returns.
- Its error return **propagates as the command's error** at every call site
  (`internal/cli/cli.go`;
  `internal/cli/next.go`; `internal/cli/workable.go`). Since the only error it can return is
  an `io.Writer` failure or a `fmt.Fprintln` error, in practice a workflow can never fail a command.
- A malformed or broken workflow file **cannot break a lit invocation** — it degrades to a warning
  (`internal/workflows/load.go`; pinned `internal/workflows/dispatch_test.go`).
- A **trace-write failure never fails Dispatch** — the guidance was already written; the failure
  goes to `errOut` as
  `lit: workflow firing trace could not be recorded (%v); guidance was still injected\n`
  (`internal/workflows/dispatch.go`). Every CLI call site passes `os.Stderr` as `errOut`.
- Exit codes are unaffected by workflow firing; the general mapping is
  `ExitOK=0, ExitGeneric=1, ExitUsage=2, ExitValidation=3, ExitNotFound=4, ExitConflict=5,
  ExitNoWork=6, ExitCorruption=7` (`internal/cli/exit.go`).

---

## 6. Firing traces

`internal/workflows/trace.go`.

- Trace kind directory name is `workflows` (`internal/workflows/trace.go`), written under
  `trace.Dir(storageDir, kind)` = `<StorageDir>/traces/workflows`
  (`internal/trace/trace.go`). `StorageDir` is `<git-common-dir>/links`
  (`internal/workspace/workspace.go`).
- **A trace is written only when at least one definition fired** — an occasion nothing matches
  leaves no trace, so the directory stays proportional to guidance actually injected
  (`internal/workflows/dispatch.go`, guard at `internal/workflows/dispatch.go`; pinned
  `internal/workflows/dispatch_test.go`).
- Recording is **skipped outright when `ws.StorageDir` is not an absolute path**
  (`internal/workflows/dispatch.go`, `filepath.IsAbs`), guarding against writing relative to the
  process CWD (pinned `internal/workflows/dispatch_test.go`).
- Filename: `<UTC timestamp 20060102T150405.000000000Z>-<slug>.json`, where slug is
  `trace.Slug(string(o.Event))` (`internal/workflows/trace.go`, `internal/trace/trace.go`,
). `Slug` lowercases, replaces every run of non-`[a-z0-9]` with `-`, trims leading/trailing
  `-`, and falls back to `"trace"` when the result is empty
  (`internal/trace/trace.go`). So `work_finished` → `work-finished`
  (`internal/trace/trace_test.go`), and an eventless occasion slugs to `trace`.
- Writes are `O_WRONLY|O_CREATE|O_EXCL`, mode `0644`, dir mode `0755`; on filename collision it
  retries up to 5 attempts with a fresh timestamp and an `-<attempt>` suffix, re-running the build
  callback each attempt (`internal/trace/trace.go`). Exhausting retries yields
  `create workflows trace: too many id collisions` (`internal/trace/trace.go`).

### 6.1 Trace record JSON schema

`FiringRecord` (`internal/workflows/trace.go`), marshaled with `json.MarshalIndent(record, "", "  ")`
plus a trailing newline (`internal/workflows/trace.go`):

```json
{
  "id": "<trace id = filename stem>",
  "recorded_at": "<RFC3339Nano>",
  "workspace_id": "<ws.WorkspaceID>",
  "event": "show_ticket",        // omitempty
  "issue_id": "lit-42",          // omitempty
  "labels": ["needs-design"],    // omitempty
  "entered": "closed",           // omitempty
  "exited": "in_progress",       // omitempty
  "fired": [
    { "id": "needs-design-note", "source": "project", "path": "needs-design.md",
      "reasons": ["event:show_ticket", "label:needs-design"] }
  ]
}
```

`FiredDefinition` fields and their JSON tags: `id`, `source`, `path`, `reasons`
(`internal/workflows/trace.go`). `fired` has no `omitempty`
(`internal/workflows/trace.go`). `recorded_at` uses `time.RFC3339Nano`
(`internal/workflows/trace.go`).

---

## 7. `lit workflows` — the command surface

Registered as a workspace-only command (no store access) at `internal/cli/register.go`:

- Summary: *"See the work lifecycle and the guidance active at each point (`workflows show <id>`
  resolved, `edit <id-or-point>` to customize, `dry-run` to explain a hypothetical)"*
- GroupID `guidance`; declared subcommands for completion: `show`, `edit`, `dry-run`.
- Run via `r.wsCmdPipeline(withWDAcquire(workflowsDispatch))` (`internal/cli/register.go`), i.e. it resolves a workspace from
  the working directory but never opens the store (`internal/cli/register.go`,
  `internal/cli/cli.go`).

Usage string (`internal/cli/workflows.go`):

```
usage: lit workflows [show <id> | edit <id-or-point> | dry-run [--event <name>] [--label <name>]... [--enter <state>] [--exit <state>] [--issue <id>]]
```

Routing (`internal/cli/workflows.go`): a help flag gives the family usage;
zero args or a leading `-` gives `workflowsOverviewLeaf`; anything else goes to
`resolveWsLeaf(workflowsFamily, args)` against the family table
(`internal/cli/workflows.go`). Each shape is its own leaf declaring its own
arity:

| Shape | Behavior |
|---|---|
| 0 positionals | overview, `positionals: 0` (`internal/cli/workflows.go`) |
| `show <id>` | one definition resolved, `positionals: 1` (`internal/cli/workflows.go`) |
| `edit <id-or-point>` | scaffold/open, `positionals: 1` (`internal/cli/workflows.go`) |
| `dry-run` | hypothetical, `positionals: 0` (`internal/cli/workflows_dryrun.go`) |

All four set `usage: workflowsUsage`. Overview, show, and edit register no flags
beyond the implicit `--help`.

### 7.1 Bare `lit workflows` — the overview

`renderWorkflowsOverview` (`internal/cli/workflows.go`) prints, in order:

1. Header line: `lit workflows — work lifecycle guidance (project > global > embedded)`
   (`internal/cli/workflows.go`).
2. Blank line + `Events` (`internal/cli/workflows.go`), then one line per catalog event in
   `Catalog()` order, indented 2 spaces (`internal/cli/workflows.go`).
3. Blank line + `States` (`internal/cli/workflows.go`), then for each spine state: the state name
   at 2-space indent (`internal/cli/workflows.go`), then `enter` and `exit` sub-lines at 4-space
   indent (`internal/cli/workflows.go`).
4. Blank line + `Labels` (`internal/cli/workflows.go`). If no definition binds any label, prints
   `  (none bound)` (`internal/cli/workflows.go`). Otherwise one line per label.
5. Warnings section (§7.4).

Each spine point line is `printSpinePoint` (`internal/cli/workflows.go`): the bare label if
nothing is bound there, else `<label>  [<ref>, <ref>, …]` where each ref is `formatDefinitionRef`:
`<id> "<name>" (<source>)` when a name is set, else `<id> (<source>)`
(`internal/cli/workflows.go`).

Spine composition:

- **States spine** = the three built-ins in lifecycle order (`open`, `in_progress`, `closed` —
  `internal/cli/workflows.go`), followed by any **custom** state a loaded definition binds to,
  sorted alphabetically (`internal/cli/workflows.go`; pinned
  `internal/cli/workflows_test.go`).
- **Labels spine** = every label any loaded definition binds to, deduped and sorted — not every label
  ever used on a ticket (`internal/cli/workflows.go`).
- **Events spine** = the full catalog, always, whether bound or not
  (`internal/cli/workflows.go`).

A definition appears at **every** dimension point it binds (pinned
`internal/cli/workflows_test.go`).

### 7.2 `lit workflows show <id>`

`renderWorkflowDefinition` (`internal/cli/workflows.go`). Unknown id →
`ValidationError{no workflow definition with id %q (run 'lit workflows' to see loaded ids)}`
→ exit 3 (`internal/cli/workflows.go`; pinned `internal/cli/workflows_test.go`).
`lit workflows show` with no id is a `UsageError` (`internal/cli/workflows_test.go`).

Output is exactly (`internal/cli/workflows.go`):

```
id: <id>
name: <name or ->
source: <project|global|embedded>
path: <layer-relative path>
labels: <comma-space joined, or ->
states: <"state(when)" comma-space joined, or ->
events: <comma-space joined, or ->
---
<body>
```

`orDash` renders `-` for an empty value (`internal/cli/workflows.go`);
`formatStateActivations` renders each as `<state>(<when>)` (`internal/cli/workflows.go`);
`formatEvents` joins raw event names (`internal/cli/workflows.go`).

### 7.3 `lit workflows dry-run`

`workflowsDryRunLeaf` (`internal/cli/workflows_dryrun.go`). Flags
(`internal/cli/workflows_dryrun.go`):

| Flag | Type | Help text |
|---|---|---|
| `--event <name>` | string | "Semantic event the hypothetical occasion fires (see the event catalog in 'lit workflows')" |
| `--label <name>` | **string array, repeatable** | "Label the hypothetical ticket carries (repeatable)" |
| `--enter <state>` | string | "State the hypothetical ticket enters" |
| `--exit <state>` | string | "State the hypothetical ticket exits" |
| `--issue <id>` | string | "Issue id to interpolate into `<id>` in previewed bodies" |

Any stray positional after `dry-run` is a `UsageError` with `workflowsUsage`
(`internal/cli/workflows_dryrun.go`); an unknown flag is likewise a `UsageError`
(`internal/cli/workflows_test.go`).

The flags build an `Occasion` **verbatim, with no canonicalization** — `--event`, `--label`,
`--enter`, `--exit` values are used exactly as typed (`internal/cli/workflows_dryrun.go`).
(Definitions were canonicalized at parse time; dry-run inputs are not, so a mixed-case `--label
Needs-Design` will not match a canonicalized definition label.)

Output (`internal/cli/workflows_dryrun.go`):

```
occasion: event=<e|-> labels=<comma-joined|-> entered=<s|-> exited=<s|-> issue=<id|->
<blank line>
Fired (<n>)
```
then either `  (none)` when n=0 (`internal/cli/workflows_dryrun.go`), or for each match:
```
  <definitionRef>  [<reason>, <reason>]
    <each body line, interpolated, indented 4 spaces>
```
(`internal/cli/workflows_dryrun.go`). Body is split on `\n` and every line is indented
(`internal/cli/workflows_dryrun.go`).

Concrete pinned example (`internal/cli/workflows_test.go`): with a project file
`design.md` bound to `labels: [needs-design]` + `events: [work_finished]`,
`lit workflows dry-run --event work_finished --label needs-design --issue lit-7` prints
`Fired (2)` (the project file **plus the embedded `done` default**) and the line
`design-note (project)  [event:work_finished, label:needs-design]`.

**Dry-run never writes a firing trace** (`internal/cli/workflows_dryrun.go`; pinned
`internal/cli/workflows_test.go`) and never reads the store
(`internal/cli/workflows_dryrun.go`).

### 7.4 Warnings display

`printWorkflowWarnings` (`internal/cli/workflows.go`) — appended to the overview only
(`internal/cli/workflows.go`); prints nothing when there are no warnings
(`internal/cli/workflows.go`). Otherwise:

```
<blank line>
Warnings (loaded but not fully active)
  <source> <path>: <message>
```

Each warning is collapsed to one line by `strings.Join(strings.Fields(msg), " ")`, so an embedded
multi-line YAML error still occupies exactly one line (`internal/cli/workflows.go`).
Pinned `internal/cli/workflows_test.go`.

---

## 8. `lit workflows edit <id-or-point>` — scaffolding

`runWorkflowsEdit` (`internal/cli/workflows_edit.go`) loads the Set and branches on whether
`target` resolves to a loaded definition ID.

### 8.1 Target is an existing definition ID

`editExistingDefinition` (`internal/cli/workflows_edit.go`):

- Project path is `<RootDir>/.lit/workflows/<def.Path>` (with `/` converted to the OS separator)
  (`internal/cli/workflows_edit.go`).
- **Source is `project`** → the file *is* the override; just print/open it, no rescaffold
  (`internal/cli/workflows_edit.go`; pinned `internal/cli/workflows_test.go`).
- **Source is `global` or `embedded`** → refuse if the project path already exists
  (§8.3), read the raw bytes via `workflows.RawDefault`, write them **verbatim** to the project
  path, print
  `scaffolded override for "<id>" (was <source>) -> <path>`, then open/print
  (`internal/cli/workflows_edit.go`; pinned `internal/cli/workflows_test.go`).

`workflows.RawDefault(source, relPath)` (`internal/workflows/scaffold.go`):
- `global` → reads `<config.ConfigDir()>/workflows/<relPath>`; error
  `read global workflow default %s: %w` (`internal/workflows/scaffold.go`).
- `embedded` → reads from the embedded FS; error `read embedded workflow default %s: %w`
  (`internal/workflows/scaffold.go`).
- `project` (or anything else) → error
  `workflows: no raw default for the %s layer (it is the override target, not a source to copy from)`
  (`internal/workflows/scaffold.go`; pinned `internal/workflows/scaffold_test.go`).

### 8.2 Target is not a loaded ID — treated as a lifecycle "point"

`classifyWorkflowPoint` (`internal/cli/workflows_edit.go`) resolves it in this **fixed
order**:

1. **`<state>:enter` / `<state>:exit` suffix** → dimension `states`. The split is at the **LAST**
   colon, so `deploy:staging:enter` yields state `deploy:staging`
   (`internal/cli/workflows_edit.go`; pinned `internal/cli/workflows_test.go`).
   The suffix comparison is `strings.ToLower(strings.TrimSpace(suffix))`, and the state name is
   `strings.TrimSpace`d (`internal/cli/workflows_edit.go`).
2. **A name in the event catalog** (`workflows.Event(point).Known()`) → dimension `events`, live
   line `events: ["<point>"]` (`internal/cli/workflows_edit.go`; pinned
   `internal/cli/workflows_test.go`).
3. **One of the three built-in states, bare** → dimension `states`, defaulting to `enter`
   (`internal/cli/workflows_edit.go`, `isBuiltinState` at compares
   lowercased+trimmed against `builtinStates`).
4. **Anything else** → dimension `labels`, live line `labels: ["<point>"]`
   (`internal/cli/workflows_edit.go`; pinned `internal/cli/workflows_test.go`).

Live-line rendering:
- `states` enter: `states: ["<state>"]`; exit: `states: [{name: "<state>", when: exit}]`
  (`internal/cli/workflows_edit.go`).
- All embedded values go through `yamlDoubleQuoted`, which wraps in `"` and escapes `\` and `"`
  (`internal/cli/workflows_edit.go`) — applied **uniformly**, not only when a special
  character is present. This is what keeps a comma-bearing state name from splitting into two flow
  entries (`internal/cli/workflows_test.go`).

Filename:
- `scaffoldFilenameSlug` lowercases, trims, replaces every run of characters outside `[a-z0-9_-]`
  with `-`, trims leading/trailing `-`, and falls back to `"point"` if empty
  (`internal/cli/workflows_edit.go`). **Underscore is deliberately kept** so `work_started`
  → `work_started.md` (`internal/cli/workflows_edit.go`).
- For a state point with `when: exit`, `_exit` is appended to the slug
  (`internal/cli/workflows_edit.go`): `closed:exit` → `closed_exit.md`
  (`internal/cli/workflows_test.go`).
- Worked examples from tests: `work_started` → `work_started.md`; `closed:exit` → `closed_exit.md`;
  `deploy:staging:enter` → `deploy-staging.md`; `needs-design` → `needs-design.md`;
  `foo,bar:enter` → `foo-bar.md`.

Then `editFreshDefinition` (`internal/cli/workflows_edit.go`) refuses on an existing file,
writes `workflows.ScaffoldFresh(dimension, liveLine)`, prints
`scaffolded a new definition at <path>`, and opens/prints.

### 8.3 Fresh-scaffold file content

`ScaffoldFresh(dimension, liveLine)` (`internal/workflows/scaffold.go`) emits exactly:

```
---
# Uncomment or add any of these activation dimensions; declared dimensions
# combine with AND, values within one dimension combine with OR.
# Run `lit workflows --help` for the full format and the event catalog.
# labels: [needs-design, blocked]                        ← omitted if dimension=="labels"
# states: [open]                       # fires when the ticket ENTERS this state    ← omitted if dimension=="states"
# states: [{name: closed, when: exit}] # fires when the ticket EXITS this state     ← omitted if dimension=="states"
# events: [work_started]                                 ← omitted if dimension=="events"
<liveLine>
# id: my-custom-id     # optional; defaults to this file's relative path under .lit/workflows/
# name: My Guidance    # optional pretty name shown by `lit workflows`
---
Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.
```

Line-by-line: `internal/workflows/scaffold.go` (`---`) (the two comment lines),
 (labels example, conditional) (both states examples, conditional)
(events example, conditional) (live line + newline) (id comment) (name
comment) (closing `---`) (body placeholder). Pinned
`internal/workflows/scaffold_test.go`.

### 8.4 No-clobber semantics

Two-stage enforcement (`internal/cli/workflows_edit.go`):

1. `refuseExistingFile` stats the path; if it exists, returns `MergeConflictError` (**exit 5**,
   `internal/cli/exit.go`) with message
   `cannot scaffold "<subject>": <path> already exists (edit it directly, or run 'lit workflows' to
   see what's already loaded there)` (`internal/cli/workflows_edit.go`). A stat error other
   than not-exist is returned as `stat %s: %w` (`internal/cli/workflows_edit.go`).
2. `writeWorkflowScaffold` is the real enforcer: `os.MkdirAll(dir, 0755)` then
   `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0644)`. `os.IsExist` → the same
   `MergeConflictError`, closing the TOCTOU gap (`internal/cli/workflows_edit.go`; pinned
   `internal/cli/workflows_test.go`).

Scaffolding **only ever writes under the project `.lit/workflows/`** — never into global or embedded
layers (`internal/cli/workflows_edit.go`).

### 8.5 Opening the file — the one subprocess in the subsystem

`openOrPrintWorkflowFile` (`internal/cli/workflows_edit.go`):

1. **Always** prints the path on its own line to stdout first (`internal/cli/workflows_edit.go`).
2. If stdout is **not** a terminal → return; print only (`internal/cli/workflows_edit.go`).
   `isTerminal` = `w` is an `*os.File` **and** `Stat().Mode()&os.ModeCharDevice != 0`
   (`internal/cli/workflows_edit.go`). A `bytes.Buffer` (tests), a pipe, or a redirect all
   return false.
3. `$EDITOR` is split on whitespace with `strings.Fields`; if empty → return
   (`internal/cli/workflows_edit.go`). So `EDITOR="code -w"` works as command + args.
4. Otherwise `exec.Command(fields[0], fields[1:]... + path)` is run with the process's real
   `os.Stdin/os.Stdout/os.Stderr` wired through, **synchronously** (`cmd.Run()`)
   (`internal/cli/workflows_edit.go`).
5. A non-zero editor exit becomes the error
   `open %s in $EDITOR (%s): %w` (`internal/cli/workflows_edit.go`).

No other environment variable is consulted anywhere in the subsystem except `$EDITOR` here and
`$XDG_CONFIG_HOME`/`$HOME` via `config.ConfigDir()` (`internal/config/config.go`).

---

## 9. `internal/cli/hooks.go` — relationship to workflow events

**There is none.** `hooks.go` installs a **git `pre-push` hook**, not a workflow event hook. It
writes/updates a managed section of `<git-common-dir>/hooks/pre-push`
(`internal/cli/hooks.go`), bounded by the markers
`# --- BEGIN LIT INTEGRATION ---` / `# --- END LIT INTEGRATION ---`
(`internal/cli/hooks.go`), migrating the legacy
`# --- BEGIN LINKS INTEGRATION ---` markers (`internal/cli/hooks.go`). The section
content comes from `templates.Load(templates.PrePushHookTemplateName, workspaceRoot)`
(`internal/cli/hooks.go`). It refuses to manage a hook whose first line is not a `#!`
shebang containing `bash` (`internal/cli/hooks.go`).

The file imports neither `internal/workflows` nor anything event-related
(`internal/cli/hooks.go`), contains no reference to `Occasion`, `Dispatch`, or any event
constant, and no workflow event fires on `lit hooks install`
(`internal/cli/hooks.go` — the only output is `installed <path>`). The only shared vocabulary
is the word "hook" and the sibling trace-kind comment at `internal/cli/sync_trace.go` noting
"automation" and "workflows" as adjacent trace kinds.

---

## 10. Summary of things a workflow author can and cannot do

**Can:** bind on any subset of {labels, states, events}; use `enter`/`exit` sides independently;
bind to custom (non-lifecycle) state names; nest files arbitrarily; override a nearer layer by ID
or by matching relative path; use `<id>` in the body; author for a future `lit` (unknown events and
unknown YAML keys both load).

**Cannot:** run anything, receive environment variables, gate/block a command, set an exit code,
use a wildcard/glob/regex matcher, negate a matcher, order or prioritize between simultaneously
matching definitions (all fire, ID-ascending), require ALL labels (label matching is OR — see
`internal/workflows/match.go`), match on issue ID, or match a state binding when the moment
carries no transition (`internal/workflows/match.go`).
