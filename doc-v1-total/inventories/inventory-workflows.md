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
  (`internal/workflows/parse.go`). Such a file loads successfully but is inert — see §1.8
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

The YAML struct is `frontmatter` (`internal/workflows/parse.go`):

| YAML key | Go type | Meaning |
|---|---|---|
| `id` | string | `internal/workflows/parse.go` |
| `name` | string | `internal/workflows/parse.go` |
| `when` | list of string, each read by `ParseMoment` | `internal/workflows/parse.go` |
| `labels` | list of string | `internal/workflows/parse.go` |
| *(any other key)* | `Unread map[string]any`, tagged `yaml:",inline"` | `internal/workflows/parse.go` |

The four named fields are the one list of keys lit reads; `Unread` collects whatever they do not
declare, so no second list can drift from them. A file with anything in `Unread` loads but never
fires — see §1.7. `parseDefinition` (`internal/workflows/parse.go`) turns the parsed frontmatter
into a `Definition{ID, Name, When []Moment, Labels []string, Body, Source, Path}`
(`internal/workflows/workflows.go`).

Frontmatter must be a **YAML mapping**. A sequence (`- a\n- b`) or a bare scalar (`hello`) at the
top level is malformed and the file is skipped (`internal/workflows/parse_test.go` cases
"non-mapping frontmatter" and "scalar frontmatter" — they fail `yaml.Unmarshal` into the struct at
`internal/workflows/parse.go`).

`labels: 17` and an unbracketed `when: enter closed` (a non-sequence for a sequence-typed key), and
a mapping as a `when:` entry (`- {name: closed}`), are likewise malformed and skip the file
(`internal/workflows/parse_test.go` cases "labels not a sequence", "when not a sequence", "when
entry a mapping").

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

### 1.5 `labels` — the tickets a definition fires on

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

### 1.6 `when` — the moments a definition fires at

Each `when:` entry is one **moment**. The types (`internal/workflows/workflows.go`):

- `Moment` — a sealed interface (`String()`, `Known()`, unexported `moment()`) whose only members
  are `Event` and `Crossing`. An occasion carries the moments that happened and a definition lists
  the moments it fires on, so matching is equality between the two.
- `Event` — a read moment, named by its catalog name (§3). `Event.Known()` is catalog membership
  (`internal/workflows/events.go`).
- `Crossing{Side, State}` — the ticket's state changing into or out of one state.
  `Crossing.String()` is `<side> <state>`; `Crossing.Known()` is always true, since a state is an
  open string.
- `Side` — `Enter` (`"enter"`) or `Exit` (`"exit"`). A transition between two states is
  exit-of-one plus enter-of-the-other; there is no separate transition concept.
- `Spellings(moments)` — each moment's `String()`, in order: the one rendering every display, trace
  and event fact shares.

`ParseMoment(text) (Moment, bool)` (`internal/workflows/parse.go`) is the one reader of the
spelling, for the frontmatter, `lit workflows dry-run --when` and `lit workflows edit`:

- The text is lowercased and split with `strings.Fields`. Blank text is no moment (`ok=false`).
- A first word `enter` or `exit` **followed by at least one more word** is a `Crossing` whose state
  is the remaining words joined by single spaces. Anything else — a lone `enter` included — is an
  `Event` of the words joined by single spaces.
- Pinned `internal/workflows/parse_test.go` (`TestParseMoment`): `Exit  Review` → `exit review`;
  `enter deploy:staging` → state `deploy:staging`; `enter foo,bar` → state `foo,bar`;
  `enter ready  to  pull` → state `ready to pull`; `show_ticket` → `EventShowTicket`; `enter` →
  `Event("enter")`. Every result's `String()` reads back through `ParseMoment` as itself.

`parseWhen` (`internal/workflows/parse.go`) reads the `when:` list through `ParseMoment`, dropping
blank entries as authoring noise, like an empty label. Case and runs of whitespace never matter
(`TestParseDefinitionNormalization`: `' Show_Ticket '` and `'  ENTER   In_Progress '` load as
`show_ticket` and `enter in_progress`, with no warning).

- **State names are open strings, deliberately not a closed enum** (`internal/workflows/workflows.go`).
  Custom stage names are legal and require no code change. A state name may contain a colon, a
  comma or a space (`internal/workflows/parse_test.go`; `internal/cli/workflows_test.go` scaffolds
  `exit foo,bar`).
- The built-in states are `open`, `in_progress`, `closed`
  (`internal/model/lifecycle/lifecycle.go`) and the phases `plan`, `impl`, `review`, `merge`,
  states inside `in_progress` (`internal/model/lifecycle/phase.go`), surfaced in that lifecycle
  order — `open`, `in_progress`, the phases, `closed` — as `builtinStates` at
  `internal/cli/workflows.go`.
- An event **not in this binary's catalog still loads**, preserved as authored, with one warning
  per such entry:
  `unknown event %q: not in this lit's catalog, will never fire here; a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])`
  (`internal/workflows/parse.go`). The list's other entries still bind, since entries are
  alternatives. `work_started`, `phase_set`, `work_finished`, `ticket_closed` and
  `ticket_reopened` are unknown events of this kind: each loads, warns, and never fires
  (`internal/workflows/parse_test.go`, `TestParseDefinitionUnknownEventWarnsButLoads`).

### 1.7 Keys lit does not read

A file whose frontmatter puts anything in `Unread` (§1.2) — `events:`, `states:`, a `where:`
written for a newer lit, a differently cased `When:` — yields a definition that **holds its id and
never fires** (`internal/workflows/parse.go`, `parseDefinition`; pinned
`internal/workflows/parse_test.go`, `TestParseDefinitionUnreadKeysNeverFire`):

- `parseDefinition` returns as soon as `Unread` is non-empty, with `ID`, `Name`, `Body`, `Source`
  and `Path` set and neither `when` nor `labels` read, so the definition is inert (§1.8) and no
  label or unknown-event warning is produced.
- Because it holds its id, a farther layer's definition of that id does not fire in its place: lit
  cannot tell whether the unread key was meant to narrow when the definition fires, so what fires
  for an id is what its nearest author wrote, or nothing.
- It carries exactly one warning, built by `unreadKeysMessage` from the unread keys sorted and
  each `strconv.Quote`d, joined by `, `:
  `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on`
  (`internal/workflows/parse.go`).

### 1.8 Inertness

`Definition.Inert()` is true when **both** `When` and `Labels` are empty
(`internal/workflows/workflows.go`). An inert definition:

- loads successfully and is visible in `lit workflows` listings,
- produces a warning at parse time: `no activation keys (when/labels): definition is inert and will never fire`
  (`internal/workflows/parse.go`) — except a file with unread keys, whose one warning is the
  unread-key warning (§1.7),
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

### 1.10 A complete, valid example file

One of the five files shipped as embedded defaults, `internal/workflows/defaults/done.md`:

```
---
id: done
name: Post-close capture reminder
when: [enter closed]
---
Ticket <id> has been closed. Before moving on, take a moment to review related tickets. …
```

(Body text in full at `internal/workflows/defaults/done.md`.) `lit done` and `lit close` both make
`enter closed`, so both print it (`internal/cli/phase_test.go`,
`TestEveryCrossingFiresOnTheTransitionThatMakesIt`).

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
   (`internal/templates/templates.go`). Today the tree contains five files: `done.md`
   (`when: [enter closed]`), and `phase-plan.md`, `phase-impl.md`, `phase-review.md` and
   `phase-merge.md`, each bound to entering its phase (`when: [enter plan]` and so on), whose
   bodies name the command that records the next move — `lit phase <id> --to impl`,
   `lit phase <id> --to review`, `lit phase <id> --to merge`, and `lit done <id>`.

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
| Key lit does not read (file loads, never fires) | `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on` | `internal/workflows/parse.go` (`unreadKeysMessage`) |
| Inert definition | `no activation keys (when/labels): definition is inert and will never fire` | `internal/workflows/parse.go` |
| Unknown event | `unknown event %q: not in this lit's catalog, will never fire here; a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])` | `internal/workflows/parse.go` |
| Duplicate ID *within one layer* | `duplicate id %q (already defined by %s): file ignored` | `internal/workflows/load.go` |

A malformed file is skipped; other files in the same layer still load
(`internal/workflows/load_test.go`). `Warning.String()` (`internal/workflows/workflows.go`) is the
one-line rendering, `<source> <path>: <message>` with the message's own newlines collapsed by
`strings.Join(strings.Fields(msg), " ")`, shared by `lit workflows` (§7.4), every dispatching
command's stderr, and its `workflows.warnings` fact (§5).

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

Six events, defined as string constants at `internal/workflows/events.go`. An `Event` names a
**read moment** — lit showing or recording something that changes no ticket's state. A change of
state is never an event: it is the `Crossing`s it makes (§1.6, §3.2), whichever command makes it,
so `lit done` and `lit close` reach one `enter closed` and no definition can tell the commands
apart (`internal/workflows/events.go`).

| Event constant name | Wire name | Fires when | Command |
|---|---|---|---|
| `EventShowBacklog` | `show_backlog` | agent views the workable backlog | `lit backlog` — `internal/workflows/events.go` |
| `EventShowTicket` | `show_ticket` | agent views one ticket's details | `lit show` — `internal/workflows/events.go` |
| `EventNextPulled` | `next_pulled` | agent asks for the next workable ticket | `lit next` — `internal/workflows/events.go` |
| `EventTicketCreated` | `ticket_created` | a new ticket is created | `lit new`, `lit followup` — `internal/workflows/events.go` |
| `EventTicketUpdated` | `ticket_updated` | an existing ticket's fields change | `lit update` — `internal/workflows/events.go` |
| `EventCommentAdded` | `comment_added` | a comment lands on a ticket | `lit comment` — `internal/workflows/events.go` |

`Catalog()` returns them in **display order**, which differs from declaration order:
`show_backlog, next_pulled, show_ticket, ticket_created, ticket_updated, comment_added`
(`internal/workflows/events.go`). This is the order `lit workflows` prints the events at the
head of its When section in (`internal/cli/workflows.go`, `catalogMoments` in
`internal/cli/workflows_dryrun.go`).

`Event.Known()` = membership in `Catalog()` (`internal/workflows/events.go`). Catalog names
are contractually lowercase snake_case, unique, and each reads back through `ParseMoment` as the
event, never as a crossing — enforced by `internal/workflows/events_test.go`
(`TestCatalogNamesAreStableContractShaped`).

Events are a **stable contract deliberately decoupled from command names** — commands may be
renamed freely, the event name is what user definitions bind to
(`internal/workflows/events.go`).

### 3.1 The Occasion payload

`Occasion` (`internal/workflows/match.go`) is the single payload type; its fields:

| Field | Meaning | Citation |
|---|---|---|
| `Moments []Moment` | what happened: the one event a read fires, or every crossing a transition made. Several when a state inside another moves with it — a start from open exits `open` and enters both `in_progress` and `plan`. Empty when a transition left the ticket where it was | `internal/workflows/match.go` |
| `IssueID model.ID` | acted-on ticket id; empty for backlog-wide moments. **Never read by matching** — used only for `<id>` interpolation, display, and tracing | `internal/workflows/match.go` |
| `Labels []string` | the ticket's labels in canonical form; nil when no single ticket | `internal/workflows/match.go` |

### 3.2 Per-occasion payloads, as actually built

Builders live in `internal/cli/workflow_events.go`:

| Occasion | Builder | Payload fields set |
|---|---|---|
| `show_ticket` | `showTicketOccasion` `internal/cli/workflow_events.go` | Moments=`[EventShowTicket]`, IssueID=`issue.ID`, Labels=`issue.Labels`. |
| `show_backlog` | `backlogOccasion` `internal/cli/workflow_events.go` | Moments=`[EventShowBacklog]` only. **No IssueID, no Labels.** |
| `next_pulled` | `nextPulledOccasion` `internal/cli/workflow_events.go` | Moments=`[EventNextPulled]`, IssueID, Labels. |
| `ticket_created` | `ticketCreatedOccasion` `internal/cli/workflow_events.go` | Moments=`[EventTicketCreated]`, IssueID, Labels (the labels it was created with). |
| `ticket_updated` | `ticketUpdatedOccasion` `internal/cli/workflow_events.go` | Moments=`[EventTicketUpdated]`, IssueID, Labels. **Never carries a crossing** — `lit update` rejects `--status` (`internal/cli/workflow_events.go`; pinned `internal/cli/workflow_events_test.go`). |
| `comment_added` | `commentAddedOccasion` `internal/cli/workflow_events.go` | Moments=`[EventCommentAdded]`, IssueID, Labels. |
| every lifecycle transition (start, phase, done, close, open) | `transitionOccasion` `internal/cli/workflow_events.go` | Moments = `crossings(Exit, before.states(), after.states())` followed by `crossings(Enter, after.states(), before.states())`, IssueID=`after.issue.ID`, Labels (post-transition). |

`ticketState.states()` is every state the ticket is in, outermost first: its status, then its
phase if any (`internal/cli/workflow_events.go`). `crossings(side, from, to)` is the side's
`Crossing` of every state in `from` that is not in `to`, kept in `from`'s order
(`internal/cli/workflow_events.go`). So a transition's moments are its exits, then its entries,
each outermost first. Pinned `internal/cli/workflow_events_test.go`
(`TestTransitionOccasionCrossesBothLevels`):

| Transition | Moments |
|---|---|
| start from `open` | `exit open`, `enter in_progress`, `enter plan` |
| reclaim (start while in progress) | none |
| done from `merge` | `exit in_progress`, `exit merge`, `enter closed` |
| close from `open` | `exit open`, `enter closed` |
| close from `impl` | `exit in_progress`, `exit impl`, `enter closed` |
| reopen (`lit open`) | `exit closed`, `enter open` |
| phase `impl` → `review` | `exit impl`, `enter review` |
| phase to the phase it is in | none |

A transition that crosses nothing still dispatches; only a definition bound by labels alone can
fire on it (§4).
The phase on each side is `model.PhaseOf` over the issue's events, read by `readTicketState`
(through `ticketStateOf`) before and after `Store.Apply` (`internal/cli/cli.go`).

Retention actions (archive/unarchive/delete/restore) fire **no occasion at all** —
`transitionOccasion` returns `fires = false` for a `RetentionAction`
(`internal/cli/workflow_events.go`; pinned `internal/cli/workflow_events_test.go`).

### 3.3 Dispatch call sites — where each occasion is actually fired

| Site | Moments | Position in output |
|---|---|---|
| `internal/cli/cli.go` (`newLeaf`) | `ticket_created` | **after** `CreateIssue` succeeds, **before** `printIssueSummary` and the `new` breadcrumb (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`followupLeaf`) | `ticket_created` | after create, before summary/breadcrumb (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`showLeaf`) | `show_ticket` | after `GetIssueDetail`, **before** either the `--field` output or the full detail view — fires for both (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`updateLeaf`) | `ticket_updated` | after `Store.Apply`, before summary/breadcrumb |
| `internal/cli/cli.go` (`applyTransition`), run by `transitionLeaf` (`lit start`/`phase`/`done`/`close`/`open`) and `bulkTransitionLeaf` (`lit bulk start`/`done`/`close`/`open`, `internal/cli/bulk.go`) | the crossings the transition made | after `authorize` and `Store.Apply`. Guarded by `transitionOccasion`'s `fires`. Dispatch writes into a `strings.Builder` that already holds the transition's owed notice (a claim hand-off, a phase move), and the result is `transitionDone.said`. `transitionLeaf` writes `said` to stdout **before** `printIssueSummary`; `bulkTransitionLeaf` returns it as the item's notice, which `runBulkOver` prints ahead of the item's `<id> ok` line (`internal/cli/bulk.go`) |
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
otherwise: matchAny(d.When, o.Moments) AND matchAny(d.Labels, o.Labels)
```

- **OR within a key, AND across keys.** An undeclared key constrains nothing
  (`internal/workflows/match.go`, `internal/workflows/workflows.go`). Pinned
  `internal/workflows/match_test.go` (`TestMatchesSingleKeys`, `TestMatchesAndAcrossKeys`).
- `matchAny[T comparable](bound, carried []T)` (`internal/workflows/match.go`) is the one test
  both keys share: empty bound list → true (unconstrained); otherwise **at least one** bound value
  must appear among the carried values.
  - For `when`, values are `Moment`s compared by `==`: a `Crossing` is equal when its side and
    state are, an `Event` when its name is. So a `when:`-bound definition can never fire on an
    occasion with no moments, such as a transition that crossed nothing.
  - For `labels`, exact string comparison — both sides are already canonicalized
    (`internal/workflows/parse.go`, `internal/workflows/match.go`).

**There are no wildcards, no globs, no regexes, and no negation** anywhere in matching. The only
"match everything" construct is *omitting* a key. Verify by reading the whole of
`internal/workflows/match.go` — the operations are `slices.Contains`, `slices.ContainsFunc` and
`==` only.

**Precedence between multiple matches: there is none — every match fires.**
`Set.Matching` returns every matching definition, in the Set's ID-ascending order
(`internal/workflows/match.go`; ordering pinned `internal/workflows/match_test.go`).
The only "precedence" in the system is the layer/ID override applied at load time (§2.4), which
means at most one definition exists per ID.

### 4.1 MatchReasons — the "why"

`Definition.MatchReasons(o)` (`internal/workflows/match.go`) returns nil if the definition
does not match; otherwise a list of strings in this fixed order:

1. `when:<moment>` for each of the definition's `when:` moments the occasion carries, in the
   definition's order, spelled by the moment's `String()` — the canonical `when:` spelling
   (`internal/workflows/match.go`).
2. `label:<label>` for each of the definition's **declared** labels that appear in the occasion's
   labels (`internal/workflows/match.go`).

Pinned `internal/workflows/match_test.go` (`TestMatchReasonsNamesWhatMatchedInEachKey`): a
definition with `when: [exit plan, enter closed, exit review]` and
`labels: [needs-design, blocked]`, on an occasion carrying
`exit in_progress, exit review, enter closed` and the label `blocked`, gives
`["when:enter closed", "when:exit review", "label:blocked"]` — only what overlapped, in the
definition's order.

Example from a real trace: `["when:show_ticket", "label:needs-design"]`
(`internal/workflows/dispatch_test.go`).

This is the single shared "why" computation behind both the real firing trace and `dry-run`
(`internal/workflows/match.go`).

---

## 5. Dispatch mechanics

`workflows.Dispatch(env environ.Env, w io.Writer, errOut io.Writer, ws workspace.Info, o Occasion) ([]Definition, []Warning, error)`
(`internal/workflows/dispatch.go`). It returns the definitions that fired and the load's warnings
(in `WarningsByLayer` order), both with a write error too, since they fired whether or not their
bodies reached `w`. Every CLI call site reaches it through `dispatchOccasion`
(`internal/cli/workflow_dispatch.go`), which sets three facts on the command's event, each `[]`
when empty:

| Fact | Value |
|---|---|
| `workflows.when` | `workflows.Spellings(o.Moments)` — the occasion's moments in their `when:` spelling |
| `workflows.fired` | the fired definitions' ids |
| `workflows.warnings` | each load warning's `Warning.String()` line |

So "nothing is bound here" (`fired` empty, `warnings` empty) and "what was bound here could not be
read" (`warnings` non-empty) are told apart from the event alone. In `lit bulk`, `runBulkOver`
runs each item in its own `telemetry.Occurrence` scope, so these facts land per item under
`bulk.item.facts` (`internal/cli/bulk.go`; pinned `internal/cli/phase_test.go`,
`TestEveryCrossingFiresOnTheTransitionThatMakesIt`).

- **Synchronous, in-process, called directly from the command path — no bus, no async queue, no
  subscriber list; there is exactly one `Dispatch`, not a registry**
  (`internal/workflows/dispatch.go`).
- **It re-loads the full definition Set on every call** (`internal/workflows/dispatch.go`,
  `set := Load(env, ws.RootDir)`). There is no caching. Every dispatching command walks all three layers
  from disk.
- **Every load/parse warning is printed to `errOut`** before matching, one line apiece, via
  `fmt.Fprintf(errOut, "lit: workflow %s\n", warning)` — the `%s` is `Warning.String()` — in
  `WarningsByLayer` order, on every call (`internal/workflows/dispatch.go`). So a file lit cannot
  read or cannot fully fire (one written for another lit, say) is announced by every dispatching
  command, not only by `lit workflows`. `errOut`, never `w`: `w` is the agent-facing stream, where
  a config diagnostic would read as guidance. Pinned `internal/workflows/dispatch_test.go`
  (`TestDispatchReportsLoadWarningsOnErrOut`) and `internal/cli/phase_test.go`
  (`TestDefinitionsInTheRetiredSpellingsWarnAndNeverFire`: a file with an `events:` key, one with
  a `states:` key and one listing a retired event under `when:` each warn on `lit done`'s stderr
  and never fire, and `lit done` still succeeds).
- Matches, then for each match writes `Interpolate(def.Body, o.IssueID)` followed by a newline to
  `w` via `fmt.Fprintln` (`internal/workflows/dispatch.go`). `w` is the calling command's own
  **stdout** at every read call site, and for a transition the `strings.Builder` behind
  `transitionDone.said`, which the command then prints to stdout (§3.3) — the same agent-facing
  stream either way.
- Output order = `Set.Matching` order = ID-ascending
  (`internal/workflows/dispatch.go`; pinned `internal/workflows/dispatch_test.go`).
- **A write failure to `w` aborts and returns the error**, which the calling command returns
  directly (`internal/workflows/dispatch.go`; e.g. `internal/cli/cli.go`).
- No environment variable is set, no subprocess is spawned, no timeout exists — there is no
  execution (`internal/workflows/dispatch.go` in full; `internal/workflows/workflows.go`). The
  `env` it takes serves `Load` alone, which reads the global layer's location from it
  (`globalWorkflowsDir`, `internal/workflows/load.go`).

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
  (`internal/workflows/dispatch.go`). Every CLI call site passes its invocation's stderr as `errOut`.
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
  `trace.Slug(strings.Join(Spellings(o.Moments), " "))` (`internal/workflows/trace.go`,
  `internal/trace/trace.go`). `Slug` lowercases, replaces every run of non-`[a-z0-9]` with `-`,
  trims leading/trailing `-`, and falls back to `"trace"` when the result is empty
  (`internal/trace/trace.go`). So `show_ticket` → `show-ticket` and
  `exit review enter closed` → `exit-review-enter-closed` (`internal/trace/trace_test.go`), and
  an occasion with no moments — a labels-only definition firing on a transition that crossed
  nothing — slugs to `trace`.
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
  "when": ["show_ticket"],       // omitempty; Spellings(o.Moments)
  "issue_id": "lit-42",          // omitempty
  "labels": ["needs-design"],    // omitempty
  "fired": [
    { "id": "needs-design-note", "source": "project", "path": "needs-design.md",
      "reasons": ["when:show_ticket", "label:needs-design"] }
  ]
}
```

A transition's record carries every crossing it made, e.g.
`"when": ["exit in_progress", "exit review", "enter closed"]` for a `lit done` from `review`.

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
usage: lit workflows [show <id> | edit <id-or-point> | dry-run [--when <moment>]... [--label <name>]... [--issue <id>]]
```

Routing (`internal/cli/workflows.go`): a help flag gives the family usage;
zero args or a leading `-` gives `workflowsOverviewLeaf`; anything else goes to
`resolveWsLeaf(ctx, workflowsFamily, args)` against the family table
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
2. Blank line + `When` (`internal/cli/workflows.go`), then one line per moment of
   `spineMoments(set)`, spelled by its `String()` and indented 2 spaces
   (`internal/cli/workflows.go`). A definition is listed at a moment when its `When` contains it.
3. Blank line + `Labels` (`internal/cli/workflows.go`). If no definition binds any label, prints
   `  (none bound)` (`internal/cli/workflows.go`). Otherwise one line per label.
4. Warnings section (§7.4).

Each spine point line is `printSpinePoint` (`internal/cli/workflows.go`): the bare label if
nothing is bound there, else `<label>  [<ref>, <ref>, …]` where each ref is `formatDefinitionRef`:
`<id> "<name>" (<source>)` when a name is set, else `<id> (<source>)`
(`internal/cli/workflows.go`).

Spine composition:

- **When spine** = `spineMoments(set)` (`internal/cli/workflows.go`): first `catalogMoments()`
  (`internal/cli/workflows_dryrun.go`) — the full catalog as moments, always, whether bound or
  not — then `enter <state>` and `exit <state>` for each built-in state in lifecycle order (`open`,
  `in_progress`, `plan`, `impl`, `review`, `merge`, `closed` — `builtinStates`,
  `internal/cli/workflows.go`), then the same pair for any **custom** state a loaded definition's
  `Crossing` names, sorted alphabetically (`internal/cli/workflows.go`; pinned
  `internal/cli/workflows_test.go`, `TestWorkflowsOverviewSurfacesCustomStateOnTheSpine`:
  `exit closed` is followed by `enter verify  [verify-reminder (project)]` and `exit verify`). An
  unknown event a definition lists appears on no spine line.
- **Labels spine** = `spineLabels(set)`: every label any loaded definition binds to, deduped and
  sorted — not every label ever used on a ticket (`internal/cli/workflows.go`).

A definition appears at **every** moment and label it binds (pinned
`internal/cli/workflows_test.go`, `TestWorkflowsOverviewPlacesProjectDefinitionAtEveryBoundKey`).
With only the embedded layer, the overview's `enter closed` line reads
`enter closed  [done "Post-close capture reminder" (embedded)]`
(`TestWorkflowsOverviewShowsEmbeddedDefaultAtItsCrossing`).

### 7.2 `lit workflows show <id>`

`renderWorkflowDefinition` (`internal/cli/workflows.go`). Unknown id →
``ValidationError{no workflow definition with id %q (run `lit workflows` to see loaded ids)}``
→ exit 3 (`internal/cli/workflows.go`; pinned `internal/cli/workflows_test.go`).
`lit workflows show` with no id is a `UsageError` (`internal/cli/workflows_test.go`).

Output is exactly (`internal/cli/workflows.go`):

```
id: <id>
name: <name or ->
source: <project|global|embedded>
path: <layer-relative path>
when: <Spellings(def.When) comma-space joined, or ->
labels: <comma-space joined, or ->
---
<body>
```

`orDash` renders `-` for an empty value (`internal/cli/workflows.go`); `when:` lists each entry
in its canonical spelling via `workflows.Spellings` (`internal/cli/workflows.go`).

### 7.3 `lit workflows dry-run`

`workflowsDryRunLeaf` (`internal/cli/workflows_dryrun.go`). Flags
(`internal/cli/workflows_dryrun.go`):

| Flag | Type | Help text |
|---|---|---|
| `--when <moment>` | **string array, repeatable** | "A moment the hypothetical occasion carries, spelled as a when: entry: enter <state>, exit <state>, or an event (repeatable: a start from open is exit open, enter in_progress, enter plan)" |
| `--label <name>` | **string array, repeatable** | "Label the hypothetical ticket carries (repeatable)" |
| `--issue <id>` | string | "Issue id to interpolate into <id> in previewed bodies" |

Any stray positional after `dry-run` is a `UsageError` with `workflowsUsage`
(`internal/cli/workflows_dryrun.go`); an unknown flag is likewise a `UsageError`
(`internal/cli/workflows_test.go`, `TestWorkflowsDryRunRejectsUnknownFlags`).

`dryRunMoments` (`internal/cli/workflows_dryrun.go`) reads every `--when` value through
`workflows.ParseMoment`, so a dry-run is asked in the spelling a definition is written in, and
canonicalized the same way (`--when "Enter  Review"` is `enter review`). A blank value is skipped.
A value that parses to a moment this lit can never produce — an event outside the catalog, a lone
`enter` among them — is refused with a `model.ValidationError` (exit 3):
`--when %q is not a moment lit fires: write enter <state> or exit <state> for a change of a ticket's state, or one of the events %s`,
where `%s` is `workflows.Spellings(catalogMoments())` joined by `, ` (pinned
`internal/cli/workflows_test.go`, `TestWorkflowsDryRunRefusesAMomentLitNeverFires`).

`--label` values build the `Occasion` **verbatim, with no canonicalization**
(`internal/cli/workflows_dryrun.go`). (Definitions were canonicalized at parse time; dry-run
labels are not, so a mixed-case `--label Needs-Design` will not match a canonicalized definition
label.)

Output (`internal/cli/workflows_dryrun.go`, `printDryRunOccasion`):

```
occasion: when=<["moment",...]|[]> labels=<comma-joined|-> issue=<id|->
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

`when=` is `quotedList(workflows.Spellings(o.Moments))`: each spelling `strconv.Quote`d, joined by
`,`, in brackets — `[]` when none (`internal/cli/workflows_dryrun.go`). Labels can join on a bare
comma because a label cannot contain one; a state name can, so quoting keeps one crossing of a
state named `foo,bar` (`when=["enter foo,bar"]`) apart from two (`when=["enter foo","enter bar"]`)
(pinned `internal/cli/workflows_test.go`, `TestWorkflowsDryRunReadsEveryWhenSpelling`).

Concrete pinned example (`internal/cli/workflows_test.go`,
`TestWorkflowsDryRunExplainsAHypotheticalMatch`): with a project file `design.md` bound to
`labels: [needs-design]` + `when: [enter closed]`,
`lit workflows dry-run --when "enter closed" --label needs-design --issue lit-7` prints
`occasion: when=["enter closed"] labels=needs-design issue=lit-7`, `Fired (2)` (the project file
**plus the embedded `done` default**, `done "Post-close capture reminder" (embedded)  [when:enter closed]`)
and the line `design-note (project)  [when:enter closed, label:needs-design]`.

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

Each line is `Warning.String()` (`internal/workflows/workflows.go`), which collapses the message
with `strings.Join(strings.Fields(msg), " ")`, so an embedded multi-line YAML error still occupies
exactly one line. Pinned `internal/cli/workflows_test.go`
(`TestWorkflowsOverviewFlagsMalformedFileAsAWarning`).

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

`classifyWorkflowPoint(point) (key, liveLine)` (`internal/cli/workflows_edit.go`) reads the
point with `workflows.ParseMoment`, the same parser as a `when:` entry, so the two spellings
cannot drift:

1. **A moment this lit fires** — `ParseMoment` returns ok and `moment.Known()`: `enter <state>` or
   `exit <state>` for any state, built-in or custom, or an event in the catalog → key `when`,
   live line `when: ["<moment.String()>"]`, the canonical spelling (`Enter  In_Progress` scaffolds
   `when: ["enter in_progress"]`) (`internal/cli/workflows_edit.go`; pinned
   `internal/cli/workflows_test.go`, `TestWorkflowsEditScaffoldsFreshMomentPoints`).
2. **Anything else** → key `labels`, live line `labels: ["<point>"]` with the point as typed. A
   bare state name is a label point, since a state is crossed, not named: `review` scaffolds
   `labels: ["review"]`; so is an event outside the catalog (`internal/cli/workflows_edit.go`;
   pinned `internal/cli/workflows_test.go`, `TestWorkflowsEditScaffoldsFreshLabelPointAsFallback`).

Live-line rendering:
- All embedded values go through `yamlDoubleQuoted`, which wraps in `"` and escapes `\` and `"`
  (`internal/cli/workflows_edit.go`) — applied **uniformly**, not only when a special
  character is present. This is what keeps a comma-bearing state name from splitting into two flow
  entries: `exit foo,bar` scaffolds `when: ["exit foo,bar"]`, which loads as one crossing
  (`internal/cli/workflows_test.go`).

Filename:
- `scaffoldFilenameSlug` slugs the point as typed: lowercases, trims, replaces every run of
  characters outside `[a-z0-9_-]` with `-` (`unsafeScaffoldFilenameChars`), trims
  leading/trailing `-`, and falls back to `"point"` if empty (`internal/cli/workflows_edit.go`).
  **Underscore is deliberately kept** so `show_ticket` → `show_ticket.md` and
  `enter in_progress` → `enter-in_progress.md` (`internal/cli/workflows_edit.go`).
- Worked examples from tests: `show_ticket` → `show_ticket.md`; `enter review` →
  `enter-review.md`; `exit closed` → `exit-closed.md`; `Enter  In_Progress` →
  `enter-in_progress.md`; `enter deploy:staging` → `enter-deploy-staging.md`; `exit foo,bar` →
  `exit-foo-bar.md`; `needs-design` → `needs-design.md`; `review` → `review.md`. Each moment
  scaffold loads binding exactly its moment, with no warning
  (`TestWorkflowsEditScaffoldsFreshMomentPoints`).

Then `editFreshDefinition` (`internal/cli/workflows_edit.go`) refuses on an existing file,
writes `workflows.ScaffoldFresh(key, liveLine)`, prints
`scaffolded a new definition at <path>`, and opens/prints.

### 8.3 Fresh-scaffold file content

`ScaffoldFresh(key, liveLine)` (`internal/workflows/scaffold.go`) emits exactly:

```
---
# when: lists the moments the definition fires at, labels: narrows it to
# tickets carrying a label. Entries within one key are alternatives (OR);
# declaring both keys requires both (AND).
# Run `lit workflows --help` for the full format and every moment.
# when: [enter review, exit plan, show_ticket]  # entering or exiting a state, or an event   ← omitted if key=="when"
# labels: [needs-design, blocked]                                                           ← omitted if key=="labels"
<liveLine>
# id: my-custom-id     # optional; defaults to this file's relative path under .lit/workflows/
# name: My Guidance    # optional pretty name shown by `lit workflows`
---
Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.
```

Line-by-line: `internal/workflows/scaffold.go` (`---`) (the four comment lines), (when example,
conditional) (labels example, conditional) (live line + newline) (id comment) (name comment)
(closing `---`) (body placeholder). Pinned `internal/workflows/scaffold_test.go`
(`TestScaffoldFreshOmitsTheLiveKeysCommentedExample`; `TestScaffoldFreshParsesCleanly` — the
scaffold for `when: ["enter closed"]` parses to exactly that moment with no warning).

### 8.4 No-clobber semantics

Two-stage enforcement (`internal/cli/workflows_edit.go`):

1. `refuseExistingFile` stats the path; if it exists, returns `MergeConflictError` (**exit 5**,
   `internal/cli/error_output.go`) with message
   ``cannot scaffold "<subject>": <path> already exists (edit it directly, or run `lit workflows` to
   see what's already loaded there)`` (`internal/cli/workflows_edit.go`). A stat error other
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
2. Unless stdin **and** stdout are both terminals → return; print only
   (`interactive`, `internal/cli/interactive.go`). `isTerminal` = the stream is an `*os.File`
   **and** `term.IsTerminal` holds for its descriptor, asked through `SyscallConn`
   (`internal/cli/interactive.go`). A `bytes.Buffer` (tests), a pipe, or a redirect on either
   stream all return false.
3. `$EDITOR` is split on whitespace with `strings.Fields`; if empty → return
   (`internal/cli/workflows_edit.go`). So `EDITOR="code -w"` works as command + args.
4. Otherwise `exec.Command(fields[0], fields[1:]... + path)` is run in the invocation's
   directory (an unreadable one → `open %s in $EDITOR: %w`) with the invocation's
   stdin/stdout/stderr wired through, **synchronously** (`cmd.Run()`)
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

**Can:** bind on `when`, `labels`, or both; bind `enter <state>` and `exit <state>` independently;
bind to custom (non-lifecycle) state names; nest files arbitrarily; override a farther layer by ID
or by matching relative path; use `<id>` in the body; list an event a newer `lit` fires (the entry
loads with a warning and the list's other entries still bind).

**Cannot:** run anything, receive environment variables, gate/block a command, set an exit code,
use a wildcard/glob/regex matcher, negate a matcher, order or prioritize between simultaneously
matching definitions (all fire, ID-ascending), require ALL labels (label matching is OR — see
`internal/workflows/match.go`), match on issue ID, tell which command made a crossing (`lit done`
and `lit close` both make `enter closed`), fire a `when:`-bound definition on a transition that
crossed nothing (`internal/workflows/match.go`), or have a file fire while it carries a key this
`lit` does not read (`internal/workflows/parse.go`).
