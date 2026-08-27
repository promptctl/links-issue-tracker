# Behavioral Inventory — workflows / eventing subsystem (`lit`)

Derived entirely from Go source under `/Users/bmf/code/links-issue-tracker`. No markdown/docs consulted.
All paths below are repo-relative; every claim carries a `file:line` citation.

**Terminology note up front, because the name is misleading:** in this codebase a "workflow" is
**not** an executed script, hook, or automation. It is a markdown file whose body is **printed
verbatim to the calling command's stdout** when a matching lifecycle moment occurs.
`internal/workflows/workflows.go:21-23`: "Workflow bodies are injected text, never executed, and no
lifecycle is enforced or gated — this package only answers 'which guidance is in place, and does it
apply to this moment'." There is no execution environment, no subprocess, no env vars passed to a
workflow, and no timeouts, because nothing is ever run. The only subprocess anywhere in this
subsystem is `$EDITOR`, launched by `lit workflows edit` (`internal/cli/workflows_edit.go:144`).

---

## 1. The workflow definition file format

### 1.1 File shape

A definition file is a **markdown file with optional YAML frontmatter**
(`internal/workflows/workflows.go:5-6`).

Frontmatter delimiter rules (`internal/workflows/parse.go:77-110`):

- The delimiter literal is exactly `---` (`internal/workflows/parse.go:77`).
- A line counts as a delimiter if, after stripping trailing spaces, tabs, and `\r`, it equals `---`
  (`internal/workflows/parse.go:84-86`). So `---   ` and `---\r` both work
  (`internal/workflows/parse_test.go:204`, `:216` pin CRLF files and trailing-whitespace delimiters).
- The **first line of the file** must be a delimiter for frontmatter to exist at all. If it isn't,
  `found=false` and the **entire file content is the body**, with no frontmatter
  (`internal/workflows/parse.go:93-95`). Such a file loads successfully but is inert — see §1.7
  (`internal/workflows/parse_test.go:67-82`).
- Scanning then proceeds line by line until the next delimiter line; that line closes the header,
  and everything after it is the body (`internal/workflows/parse.go:97-109`).
- A frontmatter block opened but never closed is an error: `"unterminated frontmatter: missing
  closing ---"` (`internal/workflows/parse.go:105`). This makes the file **malformed**: it is
  skipped entirely and produces exactly one warning (`internal/workflows/parse.go:123-126`,
  `internal/workflows/parse_test.go:84`).
- The body is stored with leading/trailing whitespace trimmed
  (`internal/workflows/parse.go:142`, `strings.TrimSpace(body)`).

### 1.2 Frontmatter keys — the complete list

The YAML struct is `internal/workflows/parse.go:18-24`:

| YAML key | Go type | Meaning |
|---|---|---|
| `id` | string | `internal/workflows/parse.go:19` |
| `name` | string | `internal/workflows/parse.go:20` |
| `labels` | list of string | `internal/workflows/parse.go:21` |
| `states` | list of StateActivation | `internal/workflows/parse.go:22` |
| `events` | list of string | `internal/workflows/parse.go:23` |

**Unknown keys are tolerated and ignored** — deliberately, so a file authored for a newer `lit`
still loads (`internal/workflows/parse.go:15-17`; pinned by
`internal/workflows/parse_test.go:229`). There is no `yaml.KnownFields` strictness.

Frontmatter must be a **YAML mapping**. A sequence (`- a\n- b`) or a bare scalar (`hello`) at the
top level is malformed and the file is skipped (`internal/workflows/parse_test.go:85-86` cases
"non-mapping frontmatter" and "scalar frontmatter" — they fail `yaml.Unmarshal` into the struct at
`internal/workflows/parse.go:128-130`).

`labels: 17` (a non-sequence for a sequence-typed key) is likewise malformed and skips the file
(`internal/workflows/parse_test.go:91`).

### 1.3 `id`

- Trimmed of surrounding whitespace (`internal/workflows/parse.go:137`, via
  `strings.TrimSpace(meta.ID)`; pinned `internal/workflows/parse_test.go:126-129` — `'  spaced  '`
  → `spaced`).
- If empty/absent, it defaults to `defaultID(path)`: the **layer-relative file path** with the
  `.md` suffix removed and every space replaced by `_` (`internal/workflows/parse.go:161-163`).
  Example: `review tasks/design check.md` → `review_tasks/design_check`
  (`internal/workflows/parse_test.go:56-65`). Directory separators are **preserved** in the ID.
- The ID is the **primary key across layers**: a nearer layer's definition with the same ID
  overrides the farther one (`internal/workflows/workflows.go:55-59`, `internal/workflows/load.go:67-75`).
- ID matching in `Set.Lookup` is exact string equality; no case folding
  (`internal/workflows/workflows.go:103-110`).

### 1.4 `name`

Optional pretty display name; trimmed (`internal/workflows/parse.go:138`). Used only for display:
`formatDefinitionRef` renders `<id> "<name>" (<source>)` when non-empty, else `<id> (<source>)`
(`internal/cli/workflows.go:217-222`).

### 1.5 `labels` — activation dimension 1

- Each entry is passed through `model.NormalizeLabel` (`internal/workflows/parse.go:178`), which
  lowercases and trims (`internal/model/label.go:15`), rejects empty (`internal/model/label.go:16-18`),
  and rejects any label containing a comma (`internal/model/label.go:19-21`).
- Entries that trim to empty are **silently dropped** as authoring noise
  (`internal/workflows/parse.go:175-177`).
- An entry `NormalizeLabel` rejects (i.e. containing a comma) is dropped **with a warning**:
  `label %q can never match: %v` (`internal/workflows/parse.go:180`).
- Result: definition labels are stored in exactly the canonical form the store persists, so match
  comparison downstream is exact-string and therefore case-insensitive by construction
  (`internal/workflows/parse.go:165-170`). Pinned `internal/workflows/parse_test.go:147`.

### 1.6 `states` — activation dimension 2

A `states:` entry accepts **two authored shapes** (`internal/workflows/parse.go:26-60`):

1. **A bare scalar state name** — e.g. `states: [open]` or a YAML block sequence item `- open`.
   This means `when: enter` (`internal/workflows/parse.go:30-34`).
2. **A mapping** with keys `name` and optional `when` — e.g.
   `states: [{name: closed, when: exit}]` (`internal/workflows/parse.go:35-56`).

Rules:

- `when` legal values: `enter`, `exit`. Absent/empty means `enter`
  (`internal/workflows/parse.go:66-75`). Comparison is `strings.ToLower(strings.TrimSpace(raw))`, so
  ` EXIT ` works (`internal/workflows/parse.go:67`; pinned `internal/workflows/parse_test.go:178`).
- Any other `when` value is a **parse error** that makes the whole file malformed and skipped —
  error text `when must be "enter" or "exit", got %q` (`internal/workflows/parse.go:73`; pinned
  case "invalid when" `internal/workflows/parse_test.go:88`).
- A mapping entry with a name that trims to empty is a parse error: `state entry mapping requires a
  non-empty name` (`internal/workflows/parse.go:51-53`; pinned case "nameless state mapping"
  `internal/workflows/parse_test.go:90`).
- Any other YAML node kind (e.g. a nested sequence `- [open]`) is a parse error: `state entry must
  be a state name or a {name, when} mapping` (`internal/workflows/parse.go:57-59`; pinned case
  "invalid state entry kind" `internal/workflows/parse_test.go:89`).
- State names are canonicalized to `strings.ToLower(strings.TrimSpace(name))`
  (`internal/workflows/parse.go:192-194`). Pinned `internal/workflows/parse_test.go:162`.
- A **bare empty scalar** entry (`states: ['', '  open ']`) is dropped without warning by
  `compactStates` (`internal/workflows/parse.go:216-224`; pinned normalization test
  `internal/workflows/parse_test.go:122`).
- **State names are open strings, deliberately not a closed enum** (`internal/workflows/workflows.go:41-45`).
  Custom stage names are legal and require no code change. A state name may contain a colon or a
  comma (`internal/cli/workflows_test.go:267-294` scaffolds a state literally named `foo,bar`).
- The built-in three states are `open`, `in_progress`, `closed`
  (`internal/model/lifecycle/lifecycle.go:21-23`, surfaced as `builtinStates` at
  `internal/cli/workflows.go:76`).

Duplicate activations for the same state with different `when` are allowed and preserved as two
entries (`internal/workflows/parse_test.go:38-42`: `open(enter)`, `in_progress(enter)`,
`in_progress(exit)`).

### 1.7 `events` — activation dimension 3

- Entries are canonicalized to `strings.ToLower(strings.TrimSpace(value))`, and entries that trim to
  empty are dropped (`internal/workflows/parse.go:202-210`; pinned
  `internal/workflows/parse_test.go:191`).
- An event **not in this binary's catalog still loads**, preserved as authored, plus a warning:
  `unknown event %q: not in this lit's catalog, will never fire here`
  (`internal/workflows/parse.go:149-153`, `internal/workflows/events.go:63-68`; pinned
  `internal/workflows/parse_test.go:109`).

### 1.8 Inertness

`Definition.Inert()` is true when **all three** of Labels, States, Events are empty
(`internal/workflows/workflows.go:78-80`). An inert definition:

- loads successfully and is visible in `lit workflows` listings,
- produces a warning at parse time: `no activation keys (labels/states/events): definition is inert
  and will never fire` (`internal/workflows/parse.go:146-148`),
- **never matches anything** — `Matches` short-circuits false on inert
  (`internal/workflows/match.go:45-47`).

This is why a plain markdown file with no frontmatter loads but never fires
(`internal/workflows/parse_test.go:67-82`).

### 1.9 Body and the `<id>` substitution

- The body is everything after the closing delimiter, `TrimSpace`d
  (`internal/workflows/parse.go:142`).
- Exactly **one** substitution is applied at injection time: every occurrence of the literal
  string `<id>` is replaced with the occasion's IssueID
  (`internal/workflows/dispatch.go:82-84`, `strings.ReplaceAll(body, "<id>", issueID)`).
- When the occasion carries no issue ID (e.g. `show_backlog`), `<id>` is replaced with the empty
  string — the substitution is unconditional (`internal/workflows/dispatch.go:83`).
- The comment at `internal/workflows/dispatch.go:78-81` records that a prior mechanism also
  supported `<token>`, which is **not** supported here.

### 1.10 A complete, valid example file

The one file shipped as an embedded default, `internal/workflows/defaults/done.md:1-6`:

```
---
id: done
name: Post-close capture reminder
events: [work_finished]
---
Ticket <id> has been closed. Before moving on, take a moment to review related tickets. …
```

(Body text in full at `internal/workflows/defaults/done.md:6`.)

---

## 2. Where workflow files live, and how they load

### 2.1 The three layers, in precedence order

`internal/workflows/load.go:50-54`:

1. **Project**: `<workspaceRoot>/.lit/workflows/` → `templates.SourceProject`
   (`internal/workflows/load.go:135-137`, source constant `internal/templates/templates.go:87`).
2. **Global**: `<config.ConfigDir()>/workflows/` → `templates.SourceGlobal`
   (`internal/workflows/load.go:139-143`, source constant `internal/templates/templates.go:88`).
   `config.ConfigDir()` is `$XDG_CONFIG_HOME/links-issue-tracker` if `XDG_CONFIG_HOME` is set, else
   `$HOME/.config/links-issue-tracker`, else `""` if the home dir cannot be determined
   (`internal/config/config.go:175-184`). An empty PathSpec contributes no layer at all
   (`internal/workflows/load.go:83-88`).
3. **Embedded**: compiled into the binary via `//go:embed defaults/*.md`
   (`internal/workflows/load.go:17-22`), rooted at `defaults` so entries walk with root-relative
   paths identical to the other two layers → `templates.SourceEmbedded`
   (`internal/templates/templates.go:89`). Today the tree contains exactly one file, `done.md`.

### 2.2 Discovery within a layer

`loadLayer` (`internal/workflows/load.go:95-133`):

- Walks the layer root **recursively** with `fs.WalkDir` from `"."`
  (`internal/workflows/load.go:102`).
- Directories are skipped; a file participates **iff its path ends in `.md`**
  (`internal/workflows/load.go:111-113`). Case-sensitive suffix check — `.MD` does not participate.
- **The folder hierarchy carries no activation meaning whatsoever.** Nesting only seeds the default
  ID (`internal/workflows/load.go:90-94`, `internal/workflows/workflows.go:7-9`). Pinned end-to-end
  at `internal/cli/workflow_injection_test.go:34-54` (a file at
  `.lit/workflows/reviews/deep/nested/close.md` fires normally).
- Walk order is lexical, so "first wins" for within-layer duplicates is deterministic
  (`internal/workflows/load.go:92-94`).

### 2.3 Failure handling during load — Load can never fail

`Load` returns a `Set`, no error (`internal/workflows/load.go:45`). Every problem is a `Warning`
(`internal/workflows/workflows.go:83-89`) with `{Source, Path, Message}`:

| Condition | Message | Citation |
|---|---|---|
| Walk error that is `fs.ErrNotExist` (absent layer root) | *(none — genuine absence, not failure)* | `internal/workflows/load.go:104-107` |
| Any other walk error | `cannot read: <err>` | `internal/workflows/load.go:108` |
| File read error | `cannot read: <err>` | `internal/workflows/load.go:116` |
| Unterminated frontmatter | `unterminated frontmatter: missing closing ---` | `internal/workflows/parse.go:105`, `:125` |
| YAML unmarshal failure | `invalid frontmatter: <err>` | `internal/workflows/parse.go:129` |
| Comma-bearing label | `label %q can never match: <err>` | `internal/workflows/parse.go:180` |
| Inert definition | `no activation keys (labels/states/events): definition is inert and will never fire` | `internal/workflows/parse.go:147` |
| Unknown event | `unknown event %q: not in this lit's catalog, will never fire here` | `internal/workflows/parse.go:151` |
| Duplicate ID *within one layer* | `duplicate id %q (already defined by %s): file ignored` | `internal/workflows/load.go:125` |

A malformed file is skipped; other files in the same layer still load
(`internal/workflows/load_test.go:166`).

### 2.4 Merge / override semantics

- Within a layer: first file in lexical walk order claims an ID; later files with the same ID are
  **ignored with a warning** (`internal/workflows/load.go:124-127`; pinned
  `internal/workflows/load_test.go:122`).
- Across layers: the first layer to claim an ID wins outright; farther layers with that ID are
  skipped **silently — no warning** (`internal/workflows/load.go:69-75`). This is the override
  feature (pinned `internal/workflows/load_test.go:71`, `:102`).
- Override does not merge fields. A project file with `id: done` **wholly replaces** the embedded
  `done` default, including its body (`internal/cli/workflow_injection_test.go:59-86`, which asserts
  the embedded body no longer appears).
- Because the default ID is the layer-relative path, placing a file at the *same relative path* in a
  nearer layer overrides without authoring an explicit `id`
  (`internal/workflows/parse.go:158-160`; `internal/workflows/load_test.go:102`).
- The resolved `Set.Definitions` is sorted by ID ascending
  (`internal/workflows/load.go:77`), giving a stable iteration/display/injection order
  (`internal/workflows/workflows.go:94-96`).
- Warnings from **all** layers accumulate, including layers whose definitions were overridden
  (`internal/workflows/load.go:66`).

---

## 3. The complete event catalog

Ten events, defined as string constants at `internal/workflows/events.go:13-44`:

| Event constant name | Wire name | Fires when | Command |
|---|---|---|---|
| `EventShowBacklog` | `show_backlog` | agent views the workable backlog | `lit backlog` — `internal/workflows/events.go:15-16` |
| `EventShowTicket` | `show_ticket` | agent views one ticket's details | `lit show` — `internal/workflows/events.go:17-18` |
| `EventNextPulled` | `next_pulled` | agent asks for the next workable ticket | `lit next` — `internal/workflows/events.go:19-22` |
| `EventWorkStarted` | `work_started` | a ticket is claimed and work begins | `lit start` — `internal/workflows/events.go:23-25` |
| `EventWorkFinished` | `work_finished` | claimed work finishes on the success path | `lit done` — `internal/workflows/events.go:26-28` |
| `EventTicketClosed` | `ticket_closed` | a ticket is closed without finishing (wontfix/obsolete/duplicate) | `lit close` — `internal/workflows/events.go:29-31` |
| `EventTicketReopened` | `ticket_reopened` | a closed ticket is reopened | `lit open` — `internal/workflows/events.go:32-34` |
| `EventTicketCreated` | `ticket_created` | a new ticket is created | `lit new`, `lit followup` — `internal/workflows/events.go:35-37` |
| `EventTicketUpdated` | `ticket_updated` | an existing ticket's fields change | `lit update` — `internal/workflows/events.go:38-40` |
| `EventCommentAdded` | `comment_added` | a comment lands on a ticket | `lit comment` — `internal/workflows/events.go:41-43` |

`Catalog()` returns them in **display order**, which differs from declaration order:
`show_backlog, next_pulled, show_ticket, work_started, work_finished, ticket_closed,
ticket_reopened, ticket_created, ticket_updated, comment_added`
(`internal/workflows/events.go:48-61`). This is the order `lit workflows` prints the Events spine
in (`internal/cli/workflows.go:131`).

`Event.Known()` = membership in `Catalog()` (`internal/workflows/events.go:66-68`). Catalog names
are contractually lowercase snake_case, enforced by
`internal/workflows/events_test.go:10` (`TestCatalogNamesAreStableContractShaped`).

Events are a **stable contract deliberately decoupled from command names** — commands may be
renamed freely, the event name is what user definitions bind to
(`internal/workflows/events.go:5-10`).

### 3.1 The Occasion payload

`Occasion` (`internal/workflows/match.go:13-35`) is the single payload type; its fields:

| Field | Meaning | Citation |
|---|---|---|
| `Event Event` | the semantic event, or zero when none | `internal/workflows/match.go:15-16` |
| `IssueID string` | acted-on ticket id; empty for backlog-wide moments. **Never read by matching** — used only for `<id>` interpolation, display, and tracing | `internal/workflows/match.go:17-24` |
| `Labels []string` | the ticket's labels in canonical form; nil when no single ticket | `internal/workflows/match.go:25-29` |
| `Entered string` | state the ticket entered, if a transition happened | `internal/workflows/match.go:30-34` |
| `Exited string` | state the ticket exited, if a transition happened | `internal/workflows/match.go:30-34` |

### 3.2 Per-event payloads, as actually built

Builders live in `internal/cli/workflow_events.go`:

| Event | Builder | Payload fields set |
|---|---|---|
| `show_ticket` | `showTicketOccasion` `internal/cli/workflow_events.go:21-27` | Event, IssueID=`issue.ID`, Labels=`issue.Labels`. No Entered/Exited. |
| `show_backlog` | `backlogOccasion` `internal/cli/workflow_events.go:32-34` | Event only. **No IssueID, no Labels, no transition.** |
| `next_pulled` | `nextPulledOccasion` `internal/cli/workflow_events.go:39-45` | Event, IssueID, Labels. |
| `ticket_created` | `ticketCreatedOccasion` `internal/cli/workflow_events.go:49-55` | Event, IssueID, Labels (the labels it was created with). |
| `ticket_updated` | `ticketUpdatedOccasion` `internal/cli/workflow_events.go:60-66` | Event, IssueID, Labels. **Never carries a transition** — `lit update` rejects `--status` (`internal/cli/workflow_events.go:57-59`; pinned `internal/cli/workflow_events_test.go:65`). |
| `comment_added` | `commentAddedOccasion` `internal/cli/workflow_events.go:70-76` | Event, IssueID, Labels. |
| `work_started` / `work_finished` / `ticket_closed` / `ticket_reopened` | `transitionOccasion` `internal/cli/workflow_events.go:103-115` | Event (from the action), IssueID, Labels (post-transition), `Entered = issue.State()` (post), `Exited = prior.State()` (pre). |

Status-action → event mapping (`internal/cli/workflow_events.go:83-88`):
`ActionStart→work_started`, `ActionDone→work_finished`, `ActionClose→ticket_closed`,
`ActionReopen→ticket_reopened`. A `StatusAction` with no map entry **panics**:
`workflow_events: no event mapped for status action %q` (`internal/cli/workflow_events.go:105-107`).

Retention actions (archive/unarchive/delete/restore) are **not** `StatusAction`s and therefore fire
**no event at all** — the type assertion at `internal/cli/cli.go:1408` excludes them
(`internal/cli/workflow_events.go:78-82`; pinned `internal/cli/workflow_events_test.go:163`).

### 3.3 Dispatch call sites — where each event is actually fired

| Site | Event | Position in output |
|---|---|---|
| `internal/cli/cli.go:359` (`runNew`) | `ticket_created` | **after** `CreateIssue` succeeds, **before** `printIssueSummary` and the `new` breadcrumb (`internal/cli/cli.go:362-365`) |
| `internal/cli/cli.go:431` (`runFollowup`) | `ticket_created` | after create, before summary/breadcrumb (`internal/cli/cli.go:434-437`) |
| `internal/cli/cli.go:879` (`runShow`) | `show_ticket` | after `GetIssueDetail`, **before** either the `--field` output or the full detail view — fires for both (`internal/cli/cli.go:878`, `:884-890`) |
| `internal/cli/cli.go:1026` (`runUpdate`) | `ticket_updated` | after `Store.Apply`, before summary/breadcrumb |
| `internal/cli/cli.go:1409` (`runTransition`) | one of the four transition events | after `Store.Apply` and after `authorize`; **before** the claim-transfer notice at `internal/cli/cli.go:1417-1420`. Guarded by `action.(model.StatusAction)` (`internal/cli/cli.go:1408`) |
| `internal/cli/cli.go:1484` (`runCommentAdd`) | `comment_added` | after `AddComment`, before `printComment` |
| `internal/cli/next.go:73` (`runNext`) | `next_pulled` | **last** — after the claim announcement and `printNextSummary` (`internal/cli/next.go:101-110`). Only reached when a row was actually served; `Exhausted`/`NoWork` return an error before any occasion is built (`internal/cli/next.go:94-97`) |
| `internal/cli/workable.go:167` (`runWorkable`) | `show_backlog` | **last** — after the table render (`internal/cli/workable.go:164-167`) |

`backlogView` is the only `workableView` that sets an `occasion` function
(`internal/cli/workable.go:90-97`); it is invoked unconditionally at
`internal/cli/workable.go:167` as `view.occasion(rows)`.

---

## 4. Matching semantics

`Definition.Matches(Occasion)` (`internal/workflows/match.go:44-51`):

```
Inert → false
otherwise: matchEvents(d.Events, o.Event) AND matchLabels(d.Labels, o.Labels) AND matchStates(d.States, o)
```

- **OR within a dimension, AND across dimensions.** An undeclared dimension constrains nothing
  (`internal/workflows/match.go:37-43`, `internal/workflows/workflows.go:15-18`). Pinned
  `internal/workflows/match_test.go:96` (AND across) and `:120` (all three).
- `matchEvents` (`internal/workflows/match.go:65-70`): empty bound list → true (unconstrained).
  Otherwise the fired event must be **non-empty** and present in the bound list. So a definition
  bound to events can never fire on an occasion with no event.
- `matchLabels` (`internal/workflows/match.go:72-79`): empty bound list → true. Otherwise **at least
  one** bound label must appear in the occasion's carried labels. Exact string comparison — both
  sides are already canonicalized (`internal/workflows/parse.go:165-170`,
  `internal/workflows/match.go:25-28`).
- `matchStates` (`internal/workflows/match.go:81-88`) → `StateActivation.matches`
  (`internal/workflows/match.go:93-96`): the activation picks the occasion side matching its `When`
  (`Entered` for `enter`, `Exited` for `exit`), and requires that side to be **non-empty and exactly
  equal** to the activation's state name. "No transition happened" never satisfies a state binding.

**There are no wildcards, no globs, no regexes, and no negation** anywhere in matching. The only
"match everything" construct is *omitting* a dimension. Verify by reading the whole of
`internal/workflows/match.go:44-96` — the operations are `slices.Contains` and `==` only.

**Precedence between multiple matches: there is none — every match fires.**
`Set.Matching` returns every matching definition, in the Set's ID-ascending order
(`internal/workflows/match.go:55-63`; ordering pinned `internal/workflows/match_test.go:142`).
The only "precedence" in the system is the layer/ID override applied at load time (§2.4), which
means at most one definition exists per ID.

### 4.1 MatchReasons — the "why"

`Definition.MatchReasons(o)` (`internal/workflows/match.go:106-124`) returns nil if the definition
does not match; otherwise a list of strings in this fixed order:

1. `event:<the occasion's event>` — emitted once if the definition declared **any** events
   (`internal/workflows/match.go:111-113`). Note it names `o.Event`, not the bound value.
2. `label:<label>` for each of the definition's **declared** labels that appear in the occasion's
   labels (`internal/workflows/match.go:114-118`; pinned `internal/workflows/match_test.go:184` —
   only overlapping labels are listed).
3. `state:<state>(<when>)` for each declared activation that matched
   (`internal/workflows/match.go:119-123`).

Example from a real trace: `["event:show_ticket", "label:needs-design"]`
(`internal/workflows/dispatch_test.go:141`).

This is the single shared "why" computation behind both the real firing trace and `dry-run`
(`internal/workflows/match.go:98-105`).

---

## 5. Dispatch mechanics

`workflows.Dispatch(w io.Writer, errOut io.Writer, ws workspace.Info, o Occasion) error`
(`internal/workflows/dispatch.go:60-74`).

- **Synchronous, in-process, called directly from the command path — no bus, no async queue, no
  subscriber list; there is exactly one `Dispatch`, not a registry**
  (`internal/workflows/dispatch.go:12-15`).
- **It re-loads the full definition Set on every call** (`internal/workflows/dispatch.go:61`,
  `set := Load(ws.RootDir)`). There is no caching. Every dispatching command walks all three layers
  from disk.
- Matches, then for each match writes `Interpolate(def.Body, o.IssueID)` followed by a newline to
  `w` via `fmt.Fprintln` (`internal/workflows/dispatch.go:63-67`). `w` is the calling command's own
  **stdout** at every call site (§3.3) — the same agent-facing stream.
- Output order = `Set.Matching` order = ID-ascending
  (`internal/workflows/dispatch.go:19-21`; pinned `internal/workflows/dispatch_test.go:41`).
- **A write failure to `w` aborts and returns the error**, which the calling command returns
  directly (`internal/workflows/dispatch.go:64-66`; e.g. `internal/cli/cli.go:1409-1411`).
- **Load/parse warnings are never printed by Dispatch** — deliberately, so a workflow-authoring
  diagnostic doesn't appear on every invocation. They are only visible via `lit workflows`
  (`internal/workflows/dispatch.go:48-59`).
- No environment variables are read or set, no subprocess is spawned, no timeout exists — there is
  no execution (`internal/workflows/dispatch.go` in full; `internal/workflows/workflows.go:21-23`).

### 5.1 Blocking / exit-code interaction

- Dispatch is a blocking function call on the command's own goroutine; the command does not
  continue until it returns.
- Its error return **propagates as the command's error** at every call site
  (`internal/cli/cli.go:359-361`, `:431-433`, `:879-881`, `:1026-1028`, `:1409-1411`, `:1484-1486`;
  `internal/cli/next.go:73`; `internal/cli/workable.go:167`). Since the only error it can return is
  an `io.Writer` failure or a `fmt.Fprintln` error, in practice a workflow can never fail a command.
- A malformed or broken workflow file **cannot break a lit invocation** — it degrades to a warning
  (`internal/workflows/load.go:41-44`; pinned `internal/workflows/dispatch_test.go:75`).
- A **trace-write failure never fails Dispatch** — the guidance was already written; the failure
  goes to `errOut` as
  `lit: workflow firing trace could not be recorded (%v); guidance was still injected\n`
  (`internal/workflows/dispatch.go:68-72`). Every CLI call site passes `os.Stderr` as `errOut`.
- Exit codes are unaffected by workflow firing; the general mapping is
  `ExitOK=0, ExitGeneric=1, ExitUsage=2, ExitValidation=3, ExitNotFound=4, ExitConflict=5,
  ExitCorruption=7` (`internal/cli/exit.go:10-18`).

---

## 6. Firing traces

`internal/workflows/trace.go`.

- Trace kind directory name is `workflows` (`internal/workflows/trace.go:14`), written under
  `trace.Dir(storageDir, kind)` = `<StorageDir>/traces/workflows`
  (`internal/trace/trace.go:23-25`). `StorageDir` is `<git-common-dir>/links`
  (`internal/workspace/workspace.go:223`).
- **A trace is written only when at least one definition fired** — an occasion nothing matches
  leaves no trace, so the directory stays proportional to guidance actually injected
  (`internal/workflows/dispatch.go:29-33`, guard at `internal/workflows/dispatch.go:68`; pinned
  `internal/workflows/dispatch_test.go:106-157`).
- Recording is **skipped outright when `ws.StorageDir` is not an absolute path**
  (`internal/workflows/dispatch.go:68`, `filepath.IsAbs`), guarding against writing relative to the
  process CWD (pinned `internal/workflows/dispatch_test.go:165-185`).
- Filename: `<UTC timestamp 20060102T150405.000000000Z>-<slug>.json`, where slug is
  `trace.Slug(string(o.Event))` (`internal/workflows/trace.go:54`, `internal/trace/trace.go:41`,
  `:49`). `Slug` lowercases, replaces every run of non-`[a-z0-9]` with `-`, trims leading/trailing
  `-`, and falls back to `"trace"` when the result is empty
  (`internal/trace/trace.go:72-81`). So `work_finished` → `work-finished`
  (`internal/trace/trace_test.go:74`), and an eventless occasion slugs to `trace`.
- Writes are `O_WRONLY|O_CREATE|O_EXCL`, mode `0644`, dir mode `0755`; on filename collision it
  retries up to 5 attempts with a fresh timestamp and an `-<attempt>` suffix, re-running the build
  callback each attempt (`internal/trace/trace.go:36-66`). Exhausting retries yields
  `create workflows trace: too many id collisions` (`internal/trace/trace.go:66`).

### 6.1 Trace record JSON schema

`FiringRecord` (`internal/workflows/trace.go:29-39`), marshaled with `json.MarshalIndent(record, "", "  ")`
plus a trailing newline (`internal/workflows/trace.go:66-70`):

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
(`internal/workflows/trace.go:19-24`). `fired` has no `omitempty`
(`internal/workflows/trace.go:38`). `recorded_at` uses `time.RFC3339Nano`
(`internal/workflows/trace.go:57`).

---

## 7. `lit workflows` — the command surface

Registered as a workspace-only command (no store access) at `internal/cli/register.go:278-279`:

- Summary: *"See the work lifecycle and the guidance active at each point (`workflows show <id>`
  resolved, `edit <id-or-point>` to customize, `dry-run` to explain a hypothetical)"*
- GroupID `guidance`; declared subcommands for completion: `show`, `edit`, `dry-run`.
- Run via `r.wsCmd(runWorkflows)` (`internal/cli/register.go:279`), i.e. it resolves a workspace from
  the working directory but never opens the store (`internal/cli/register.go:238-244`,
  `internal/cli/cli.go:94-100`).

Usage string (`internal/cli/workflows.go:22`):

```
usage: lit workflows [show <id> | edit <id-or-point> | dry-run [--event <name>] [--label <name>]... [--enter <state>] [--exit <state>] [--issue <id>]]
```

Routing (`internal/cli/workflows.go:34-58`), on `splitArgs(args, 2)`
(`internal/cli/cli.go:1958-1978` — leading `-`-prefixed tokens and their values go to flags; the
first up-to-2 bare tokens are positional; extras spill into flagArgs):

| Shape | Behavior |
|---|---|
| 0 positionals | overview (`internal/cli/workflows.go:39-42`) |
| `show <id>` | one definition resolved (`internal/cli/workflows.go:43-47`) |
| `edit <id-or-point>` | scaffold/open (`internal/cli/workflows.go:48-52`) |
| `dry-run` (1 positional) | hypothetical (`internal/cli/workflows.go:53-54`) |
| anything else | `UsageError{workflowsUsage}` → exit 2 (`internal/cli/workflows.go:56`) |

Overview/show/edit take **no flags**: `parseNoWorkflowsFlags` parses an empty cobra flagset and then
requires `NArg()==0`, so any flag-shaped token or an oversupplied positional is a `UsageError`
(`internal/cli/workflows.go:60-72`; pinned `internal/cli/workflows_test.go:168`, `:429`).

### 7.1 Bare `lit workflows` — the overview

`renderWorkflowsOverview` (`internal/cli/workflows.go:123-194`) prints, in order:

1. Header line: `lit workflows — work lifecycle guidance (project > global > embedded)`
   (`internal/cli/workflows.go:124`).
2. Blank line + `Events` (`internal/cli/workflows.go:128`), then one line per catalog event in
   `Catalog()` order, indented 2 spaces (`internal/cli/workflows.go:131-144`).
3. Blank line + `States` (`internal/cli/workflows.go:146`), then for each spine state: the state name
   at 2-space indent (`internal/cli/workflows.go:150`), then `enter` and `exit` sub-lines at 4-space
   indent (`internal/cli/workflows.go:153-166`).
4. Blank line + `Labels` (`internal/cli/workflows.go:170`). If no definition binds any label, prints
   `  (none bound)` (`internal/cli/workflows.go:172-177`). Otherwise one line per label.
5. Warnings section (§7.4).

Each spine point line is `printSpinePoint` (`internal/cli/workflows.go:201-212`): the bare label if
nothing is bound there, else `<label>  [<ref>, <ref>, …]` where each ref is `formatDefinitionRef`:
`<id> "<name>" (<source>)` when a name is set, else `<id> (<source>)`
(`internal/cli/workflows.go:217-222`).

Spine composition:

- **States spine** = the three built-ins in lifecycle order (`open`, `in_progress`, `closed` —
  `internal/cli/workflows.go:76`), followed by any **custom** state a loaded definition binds to,
  sorted alphabetically (`internal/cli/workflows.go:83-100`; pinned
  `internal/cli/workflows_test.go:74`).
- **Labels spine** = every label any loaded definition binds to, deduped and sorted — not every label
  ever used on a ticket (`internal/cli/workflows.go:105-118`).
- **Events spine** = the full catalog, always, whether bound or not
  (`internal/cli/workflows.go:131`).

A definition appears at **every** dimension point it binds (pinned
`internal/cli/workflows_test.go:52`).

### 7.2 `lit workflows show <id>`

`renderWorkflowDefinition` (`internal/cli/workflows.go:249-273`). Unknown id →
`ValidationError{no workflow definition with id %q (run 'lit workflows' to see loaded ids)}`
→ exit 3 (`internal/cli/workflows.go:252`; pinned `internal/cli/workflows_test.go:139`).
`lit workflows show` with no id is a `UsageError` (`internal/cli/workflows_test.go:154`).

Output is exactly (`internal/cli/workflows.go:254-272`):

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

`orDash` renders `-` for an empty value (`internal/cli/workflows.go:275-280`);
`formatStateActivations` renders each as `<state>(<when>)` (`internal/cli/workflows.go:282-288`);
`formatEvents` joins raw event names (`internal/cli/workflows.go:290-296`).

### 7.3 `lit workflows dry-run`

`runWorkflowsDryRun` (`internal/cli/workflows_dryrun.go:23-51`). Flags
(`internal/cli/workflows_dryrun.go:25-29`):

| Flag | Type | Help text |
|---|---|---|
| `--event <name>` | string | "Semantic event the hypothetical occasion fires (see the event catalog in 'lit workflows')" |
| `--label <name>` | **string array, repeatable** | "Label the hypothetical ticket carries (repeatable)" |
| `--enter <state>` | string | "State the hypothetical ticket enters" |
| `--exit <state>` | string | "State the hypothetical ticket exits" |
| `--issue <id>` | string | "Issue id to interpolate into `<id>` in previewed bodies" |

Any stray positional after `dry-run` is a `UsageError` with `workflowsUsage`
(`internal/cli/workflows_dryrun.go:33-35`); an unknown flag is likewise a `UsageError`
(`internal/cli/workflows_test.go:440-450`).

The flags build an `Occasion` **verbatim, with no canonicalization** — `--event`, `--label`,
`--enter`, `--exit` values are used exactly as typed (`internal/cli/workflows_dryrun.go:37-43`).
(Definitions were canonicalized at parse time; dry-run inputs are not, so a mixed-case `--label
Needs-Design` will not match a canonicalized definition label.)

Output (`internal/cli/workflows_dryrun.go:53-79`):

```
occasion: event=<e|-> labels=<comma-joined|-> entered=<s|-> exited=<s|-> issue=<id|->
<blank line>
Fired (<n>)
```
then either `  (none)` when n=0 (`internal/cli/workflows_dryrun.go:63-66`), or for each match:
```
  <definitionRef>  [<reason>, <reason>]
    <each body line, interpolated, indented 4 spaces>
```
(`internal/cli/workflows_dryrun.go:67-78`). Body is split on `\n` and every line is indented
(`internal/cli/workflows_dryrun.go:73-77`).

Concrete pinned example (`internal/cli/workflows_test.go:395-413`): with a project file
`design.md` bound to `labels: [needs-design]` + `events: [work_finished]`,
`lit workflows dry-run --event work_finished --label needs-design --issue lit-7` prints
`Fired (2)` (the project file **plus the embedded `done` default**) and the line
`design-note (project)  [event:work_finished, label:needs-design]`.

**Dry-run never writes a firing trace** (`internal/cli/workflows_dryrun.go:20-22`; pinned
`internal/cli/workflows_test.go:451-462`) and never reads the store
(`internal/cli/workflows_dryrun.go:21-22`).

### 7.4 Warnings display

`printWorkflowWarnings` (`internal/cli/workflows.go:228-244`) — appended to the overview only
(`internal/cli/workflows.go:193`); prints nothing when there are no warnings
(`internal/cli/workflows.go:229-231`). Otherwise:

```
<blank line>
Warnings (loaded but not fully active)
  <source> <path>: <message>
```

Each warning is collapsed to one line by `strings.Join(strings.Fields(msg), " ")`, so an embedded
multi-line YAML error still occupies exactly one line (`internal/cli/workflows.go:236-241`).
Pinned `internal/cli/workflows_test.go:92`.

---

## 8. `lit workflows edit <id-or-point>` — scaffolding

`runWorkflowsEdit` (`internal/cli/workflows_edit.go:37-43`) loads the Set and branches on whether
`target` resolves to a loaded definition ID.

### 8.1 Target is an existing definition ID

`editExistingDefinition` (`internal/cli/workflows_edit.go:45-64`):

- Project path is `<RootDir>/.lit/workflows/<def.Path>` (with `/` converted to the OS separator)
  (`internal/cli/workflows_edit.go:81-83`).
- **Source is `project`** → the file *is* the override; just print/open it, no rescaffold
  (`internal/cli/workflows_edit.go:47-49`; pinned `internal/cli/workflows_test.go:371`).
- **Source is `global` or `embedded`** → refuse if the project path already exists
  (§8.3), read the raw bytes via `workflows.RawDefault`, write them **verbatim** to the project
  path, print
  `scaffolded override for "<id>" (was <source>) -> <path>`, then open/print
  (`internal/cli/workflows_edit.go:50-63`; pinned `internal/cli/workflows_test.go:295`).

`workflows.RawDefault(source, relPath)` (`internal/workflows/scaffold.go:22-40`):
- `global` → reads `<config.ConfigDir()>/workflows/<relPath>`; error
  `read global workflow default %s: %w` (`internal/workflows/scaffold.go:24-30`).
- `embedded` → reads from the embedded FS; error `read embedded workflow default %s: %w`
  (`internal/workflows/scaffold.go:31-36`).
- `project` (or anything else) → error
  `workflows: no raw default for the %s layer (it is the override target, not a source to copy from)`
  (`internal/workflows/scaffold.go:37-39`; pinned `internal/workflows/scaffold_test.go:41`).

### 8.2 Target is not a loaded ID — treated as a lifecycle "point"

`classifyWorkflowPoint` (`internal/cli/workflows_edit.go:201-212`) resolves it in this **fixed
order**:

1. **`<state>:enter` / `<state>:exit` suffix** → dimension `states`. The split is at the **LAST**
   colon, so `deploy:staging:enter` yields state `deploy:staging`
   (`internal/cli/workflows_edit.go:227-246`; pinned `internal/cli/workflows_test.go:227-244`).
   The suffix comparison is `strings.ToLower(strings.TrimSpace(suffix))`, and the state name is
   `strings.TrimSpace`d (`internal/cli/workflows_edit.go:238-244`).
2. **A name in the event catalog** (`workflows.Event(point).Known()`) → dimension `events`, live
   line `events: ["<point>"]` (`internal/cli/workflows_edit.go:205-207`; pinned
   `internal/cli/workflows_test.go:181`).
3. **One of the three built-in states, bare** → dimension `states`, defaulting to `enter`
   (`internal/cli/workflows_edit.go:208-210`, `isBuiltinState` at `:248-256` compares
   lowercased+trimmed against `builtinStates`).
4. **Anything else** → dimension `labels`, live line `labels: ["<point>"]`
   (`internal/cli/workflows_edit.go:211`; pinned `internal/cli/workflows_test.go:246`).

Live-line rendering:
- `states` enter: `states: ["<state>"]`; exit: `states: [{name: "<state>", when: exit}]`
  (`internal/cli/workflows_edit.go:258-263`).
- All embedded values go through `yamlDoubleQuoted`, which wraps in `"` and escapes `\` and `"`
  (`internal/cli/workflows_edit.go:222-225`) — applied **uniformly**, not only when a special
  character is present. This is what keeps a comma-bearing state name from splitting into two flow
  entries (`internal/cli/workflows_test.go:267-294`).

Filename:
- `scaffoldFilenameSlug` lowercases, trims, replaces every run of characters outside `[a-z0-9_-]`
  with `-`, trims leading/trailing `-`, and falls back to `"point"` if empty
  (`internal/cli/workflows_edit.go:176-186`). **Underscore is deliberately kept** so `work_started`
  → `work_started.md` (`internal/cli/workflows_edit.go:172-175`).
- For a state point with `when: exit`, `_exit` is appended to the slug
  (`internal/cli/workflows_edit.go:265-270`): `closed:exit` → `closed_exit.md`
  (`internal/cli/workflows_test.go:206-225`).
- Worked examples from tests: `work_started` → `work_started.md`; `closed:exit` → `closed_exit.md`;
  `deploy:staging:enter` → `deploy-staging.md`; `needs-design` → `needs-design.md`;
  `foo,bar:enter` → `foo-bar.md`.

Then `editFreshDefinition` (`internal/cli/workflows_edit.go:66-79`) refuses on an existing file,
writes `workflows.ScaffoldFresh(dimension, liveLine)`, prints
`scaffolded a new definition at <path>`, and opens/prints.

### 8.3 Fresh-scaffold file content

`ScaffoldFresh(dimension, liveLine)` (`internal/workflows/scaffold.go:49-71`) emits exactly:

```
---
# Uncomment or add any of these activation dimensions; declared dimensions
# combine with AND, values within one dimension combine with OR — see docs/workflows.md.
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

Line-by-line: `internal/workflows/scaffold.go:51` (`---`), `:52-53` (the two comment lines),
`:54-56` (labels example, conditional), `:57-60` (both states examples, conditional), `:61-63`
(events example, conditional), `:64-65` (live line + newline), `:66` (id comment), `:67` (name
comment), `:68` (closing `---`), `:69` (body placeholder). Pinned
`internal/workflows/scaffold_test.go:47-61`.

### 8.4 No-clobber semantics

Two-stage enforcement (`internal/cli/workflows_edit.go:85-125`):

1. `refuseExistingFile` stats the path; if it exists, returns `MergeConflictError` (**exit 5**,
   `internal/cli/exit.go:31-33`) with message
   `cannot scaffold "<subject>": <path> already exists (edit it directly, or run 'lit workflows' to
   see what's already loaded there)` (`internal/cli/workflows_edit.go:91-102`). A stat error other
   than not-exist is returned as `stat %s: %w` (`internal/cli/workflows_edit.go:94-96`).
2. `writeWorkflowScaffold` is the real enforcer: `os.MkdirAll(dir, 0755)` then
   `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0644)`. `os.IsExist` → the same
   `MergeConflictError`, closing the TOCTOU gap (`internal/cli/workflows_edit.go:104-125`; pinned
   `internal/cli/workflows_test.go:326`).

Scaffolding **only ever writes under the project `.lit/workflows/`** — never into global or embedded
layers (`internal/cli/workflows_edit.go:33-36`, `internal/cli/workflows_edit.go:81-83`).

### 8.5 Opening the file — the one subprocess in the subsystem

`openOrPrintWorkflowFile` (`internal/cli/workflows_edit.go:133-150`):

1. **Always** prints the path on its own line to stdout first (`internal/cli/workflows_edit.go:134`).
2. If stdout is **not** a terminal → return; print only (`internal/cli/workflows_edit.go:137-139`).
   `isTerminal` = `w` is an `*os.File` **and** `Stat().Mode()&os.ModeCharDevice != 0`
   (`internal/cli/workflows_edit.go:160-170`). A `bytes.Buffer` (tests), a pipe, or a redirect all
   return false.
3. `$EDITOR` is split on whitespace with `strings.Fields`; if empty → return
   (`internal/cli/workflows_edit.go:140-143`). So `EDITOR="code -w"` works as command + args.
4. Otherwise `exec.Command(fields[0], fields[1:]... + path)` is run with the process's real
   `os.Stdin/os.Stdout/os.Stderr` wired through, **synchronously** (`cmd.Run()`)
   (`internal/cli/workflows_edit.go:144-146`).
5. A non-zero editor exit becomes the error
   `open %s in $EDITOR (%s): %w` (`internal/cli/workflows_edit.go:146-148`).

No other environment variable is consulted anywhere in the subsystem except `$EDITOR` here and
`$XDG_CONFIG_HOME`/`$HOME` via `config.ConfigDir()` (`internal/config/config.go:176-183`).

---

## 9. `internal/cli/hooks.go` — relationship to workflow events

**There is none.** `hooks.go` installs a **git `pre-push` hook**, not a workflow event hook. It
writes/updates a managed section of `<git-common-dir>/hooks/pre-push`
(`internal/cli/hooks.go:59-60`), bounded by the markers
`# --- BEGIN LIT INTEGRATION ---` / `# --- END LIT INTEGRATION ---`
(`internal/cli/hooks.go:18-19`), migrating the legacy
`# --- BEGIN LINKS INTEGRATION ---` markers (`internal/cli/hooks.go:20-21`, `:115`). The section
content comes from `templates.Load(templates.PrePushHookTemplateName, workspaceRoot)`
(`internal/cli/hooks.go:127-129`). It refuses to manage a hook whose first line is not a `#!`
shebang containing `bash` (`internal/cli/hooks.go:92-114`).

The file imports neither `internal/workflows` nor anything event-related
(`internal/cli/hooks.go:3-14`), contains no reference to `Occasion`, `Dispatch`, or any event
constant, and no workflow event fires on `lit hooks install`
(`internal/cli/hooks.go:40-56` — the only output is `installed <path>`). The only shared vocabulary
is the word "hook" and the sibling trace-kind comment at `internal/cli/sync_trace.go:15` noting
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
`internal/workflows/match.go:76-78`), match on issue ID, or match a state binding when the moment
carries no transition (`internal/workflows/match.go:93-96`).
