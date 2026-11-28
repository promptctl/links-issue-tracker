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
subsystem is `$EDITOR`, launched by `lit workflows edit` and `new` (`internal/cli/workflows_edit.go`).

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
  (`internal/workflows/parse.go`). Such a file loads but never fires — see §1.8
  (`internal/workflows/parse_test.go`).
- Scanning then proceeds line by line until the next delimiter line; that line closes the header,
  and everything after it is the body (`internal/workflows/parse.go`).
- A frontmatter block opened but never closed is an error: `"unterminated frontmatter: missing
  closing ---"` (`internal/workflows/parse.go`). This makes the file **malformed**: it loads as a
  definition that never fires (§1.8), under the id its path gives and with an empty body, and
  produces exactly one warning (`internal/workflows/parse.go`,
  `internal/workflows/parse_test.go`).
- The body is stored with leading/trailing whitespace trimmed
  (`internal/workflows/parse.go`, `strings.TrimSpace(body)`).

### 1.2 Frontmatter keys — the complete list

The YAML struct is `frontmatter` (`internal/workflows/parse.go`):

| YAML key | Go type | Meaning |
|---|---|---|
| `id` | string | `internal/workflows/parse.go` |
| `name` | string | `internal/workflows/parse.go` |
| `when` | `yaml.Node`, read by `readKey` as a list of string, each entry by `ParseMoment` | `internal/workflows/parse.go` |
| `labels` | `yaml.Node`, read by `readKey` as a list of string, each entry by `model.NormalizeLabel` | `internal/workflows/parse.go` |
| *(any other key)* | `Unread map[string]any`, tagged `yaml:",inline"` | `internal/workflows/parse.go` |

The four named fields are the one list of keys lit reads; `Unread` collects whatever they do not
declare, so no second list can drift from them. A file with anything in `Unread` loads but never
fires — see §1.7. `when` and `labels` stay YAML nodes until `readKey` reads them: a node is zero
only for a key left out, so a key written empty (`labels: []`, a bare `when:`) reads apart from one
never written (§1.6a). `parseDefinition` (`internal/workflows/parse.go`) turns one file's content
into a `Definition{ID, Name, Activation, Body, Source, Path}` (`internal/workflows/workflows.go`)
and the warnings reading it produced. Every file yields a definition, so it returns no ok flag;
one lit cannot fire on its keys keeps the zero `Activation`, which never fires (§1.8).

Frontmatter must be a **YAML mapping**. A sequence (`- a\n- b`) or a bare scalar (`hello`) at the
top level is malformed: the definition never fires and holds the id its path gives, with one
`invalid frontmatter: <err>` warning (`internal/workflows/parse_test.go` cases
"non-mapping frontmatter" and "scalar frontmatter" — they fail `yaml.Unmarshal` into the struct at
`internal/workflows/parse.go`).

`labels: 17` and an unbracketed `when: enter closed` (a non-sequence for a sequence-typed key), and
a mapping as a `when:` entry (`- {enter: closed}`), decode into the header's nodes but not into a
list of strings: `readKey` warns once, `invalid frontmatter: %s %v`, `%s` being the key's name
(`when:` or `labels:`) and `%v` the YAML error, and the definition never fires. The rest of the
header has decoded, so the definition holds the id it declares — `id: done` beside
`when: enter closed` holds `done` — else the id its path gives
(`internal/workflows/parse_test.go`, `TestParseDefinitionMalformedNeverFiresAndHoldsItsID`, cases
"labels not a sequence", "when not a sequence", "when entry a mapping", each with exactly one
warning).

### 1.3 `id`

- Trimmed of surrounding whitespace (`internal/workflows/parse.go`, via
  `strings.TrimSpace(meta.ID)`; pinned `internal/workflows/parse_test.go` — `'  spaced  '`
  → `spaced`).
- If empty/absent, it defaults to `DefaultID(path)`: the **layer-relative file path** with the
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

- Each entry is passed through `model.NormalizeLabel` (`internal/workflows/parse.go`, via
  `readKey` and `Entries`, §1.6a), which lowercases and trims (`internal/model/label.go`), rejects
  empty (`internal/model/label.go`), and rejects any label containing a comma
  (`internal/model/label.go`).
- An entry `NormalizeLabel` rejects — blank after trimming, or containing a comma — is dropped
  **with a warning**: `label %q can never match: %v` (`internal/workflows/parse.go`). A written
  `labels:` left with no entry can never hold, so the definition never fires (§1.8).
- Entries that normalize alike are one label: `labels: [x, X, ' x ']` reads as `[x]`, with no
  warning, and gives the one reason `label:x` (`internal/workflows/parse_test.go`,
  `TestParseDefinitionReadsARepeatedLabelOnce`).
- Result: definition labels are stored in exactly the canonical form the store persists, so match
  comparison downstream is exact-string and therefore case-insensitive by construction
  (`internal/workflows/parse.go`). Pinned `internal/workflows/parse_test.go`.

### 1.6 `when` — the moments a definition fires at

Each `when:` entry is one **moment**. The types (`internal/workflows/workflows.go`):

- `Moment` — a sealed interface (`String()`, unexported `moment()`) whose only members
  are `Event` and `Crossing`. An occasion carries the moments that happened and a definition lists
  the moments it fires on, so matching is equality between the two.
- `Event` — a read moment, named by its catalog name (§3).
- `Crossing{Side Side; State model.StateNode}` — a ticket entering or leaving one of the
  lifecycle's state nodes (`model.StateNodes()`); a status, a phase and a retention are crossed
  the same way. `Crossing.String()` is `<side> <state>`.
- `Side` — `Enter` (`"enter"`) or `Exit` (`"exit"`). A transition exits the states the ticket
  leaves and enters the ones it reaches, so what it fires is those crossings, never a name for the
  transition.
- The crossable states are the lifecycle's state nodes, `model.StateNodes()`
  (`internal/model/lifecycle/state_node.go`), in the order the work passes through them: the
  statuses `open`, `in_progress`, `closed` (`States()`, `internal/model/lifecycle/lifecycle.go`),
  with the phases `plan`, `impl`, `review`, `merge` (`Phases()`,
  `internal/model/lifecycle/phase.go`) after `PhasedState` (`in_progress`), the status they nest
  inside, then the retentions `live`, `archived`, `deleted` (`Retentions()`,
  `internal/model/lifecycle/retention.go`) — `open`, `in_progress`, `plan`, `impl`, `review`,
  `merge`, `closed`, `live`, `archived`, `deleted`. **The states are a closed set**, each axis
  listing itself, so a state the lifecycle adds is crossable here with no edit. A `StateNode`'s
  name is unexported: a node is built only by `StatusNode`, `PhaseNode` or `RetentionNode`, or
  parsed by `ParseStateNode`.
- `Moments()` — every moment this lit fires, in display order: each catalog event, then `enter`
  and `exit` of each state node.
- `Spellings(moments)` — each moment's `String()`, in order: the one rendering every display, trace
  and event fact shares.

`ParseMoment(text) (Moment, error)` (`internal/workflows/parse.go`) is the one reader of the
spelling, for the frontmatter, `lit workflows dry-run --when` and `lit workflows new --when`. It
admits only moments this lit fires, and refuses anything else with an error saying why:

- The text is lowercased and split with `strings.Fields`. Blank text is the error
  `a blank entry names no moment`.
- A first word `enter` or `exit` **followed by at least one more word** is a `Crossing` of the
  state the remaining words, joined by single spaces, name, read through the lifecycle's one gate,
  `model.ParseStateNode` (`internal/model/lifecycle/state_node.go`), the only string-to-node gate.
  It reads a name as `ParseState` does — any case, surrounding space ignored, `in-progress` for
  `in_progress` (`stateName`, `internal/model/lifecycle/lifecycle.go`) — and refuses a name no node
  has with the error `unknown state %q: a ticket's states are %s`, `%s` being the state nodes
  joined by `, `.
- Anything else — a lone `enter` included — must be an event in `Catalog()`, else the error
  `unknown event %q: the events are %s, and a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])`,
  `%s` being the catalog's names joined by `, `.
- Pinned `internal/workflows/parse_test.go` (`TestParseMoment`): `Exit  Review` → `exit review`;
  `enter in-progress` → `enter in_progress`; `exit live` → `exit live`; `enter Archived` →
  `enter archived`; `show_ticket` → `EventShowTicket`; refused: a blank, `enter`
  (`unknown event "enter"`), `work_finished`, `enter deploy:staging`
  (`unknown state "deploy:staging"`) and `exit strategy`. Every moment in `Moments()` reads back
  through `ParseMoment` as itself (`TestMomentsReadBackAsThemselves`). `ParseStateNode` refuses
  `in progress`, `reveiw` and a blank with an error naming every state
  (`internal/model/lifecycle/state_node_test.go`, `TestParseStateNode`).

The `when:` list is read through `ParseMoment` by `readKey` (§1.6a). Each entry it refuses is
dropped with one warning, `when: entry %q never fires: %v`, `%v` being `ParseMoment`'s error; the
list's other entries still bind, since entries are alternatives. A written `when:` left with no
entry can never hold, so the definition never fires (§1.8). Case and runs of whitespace never
matter (`TestParseDefinitionNormalization`: `' Show_Ticket '`, `'  ENTER   In_Progress '` and
`'exit In-Progress'` load as `show_ticket`, `enter in_progress` and `exit in_progress`, with no
warning).

- An entry naming no moment this lit fires — an event outside the catalog (one a newer lit may
  fire), a state the lifecycle lacks, a blank — warns and never fires. `work_started`,
  `phase_set`, `work_finished`, `ticket_closed` and `ticket_reopened` are unknown events of this
  kind (`internal/workflows/parse_test.go`,
  `TestParseDefinitionUnreadableWhenEntryWarnsAndNeverFires`).

### 1.6a Reading a key — `readKey` and `Entries`

`readKey(name, node, read, refused, warn) (Key[T], holds bool, []Warning)`
(`internal/workflows/parse.go`) reads one activation key as its file wrote it; `parseDefinition`
calls it for `when:` (read by `ParseMoment`) and `labels:` (read by `model.NormalizeLabel`):

- A key left out (a zero node) is the zero `Key`, which holds on anything, with no warning.
- A written key that does not decode as a list of strings warns
  `invalid frontmatter: %s %v` and cannot hold (§1.2).
- Otherwise its texts go through `Entries` (below); each refused text warns through the key's own
  wording (`when: entry %q never fires: %v` or `label %q can never match: %v`). A key written with
  no texts at all — `[]`, or a bare `when:` — warns `lists no entry, so the definition never fires`
  after the key's name (`when: lists no entry, so the definition never fires`). A written key
  holds only when at least one entry is left.

`Entries[T](texts, read) ([]T, []Refusal)` (`internal/workflows/parse.go`) is the one reader of a
key's entries wherever they are written — a definition's `when:` or `labels:`, and the same key's
flags on `lit workflows dry-run` and `new` (`readKeyFlags`, §7.3, §8.2): each text through
`read`, each value kept once in the order first written, and each text `read` refuses returned as
`Refusal{Text, Err}`. Pinned `TestParseDefinitionKeyWithNoReadableEntryNeverWidens` (`labels: []`,
a bare `when:` and `when: []` beside the other key each never fire) and
`TestParseDefinitionBothKeysWrittenEmptyWarnEach` (`when: []` with `labels: []` warns once per key,
not as a file with no keys).

### 1.7 Keys lit does not read

A file whose frontmatter puts anything in `Unread` (§1.2) — `events:`, `states:`, a `where:`
written for a newer lit, a differently cased `When:` — yields a definition that **holds its id and
never fires** (`internal/workflows/parse.go`, `parseDefinition`; pinned
`internal/workflows/parse_test.go`, `TestParseDefinitionUnreadKeysNeverFire`):

- `parseDefinition` still reads `when` and `labels`, so their entries warn as they would anywhere,
  but a non-empty `Unread` leaves the `Activation` at its zero value, which never fires (§1.8),
  whatever they hold, and no keyless warning is added.
- Because it holds its id, a farther layer's definition of that id does not fire in its place: lit
  cannot tell whether the unread key was meant to narrow when the definition fires, so what fires
  for an id is what its nearest author wrote, or nothing.
- The unread keys carry one warning, built by `unreadKeysMessage` from them sorted and
  each `strconv.Quote`d, joined by `, `:
  `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on`
  (`internal/workflows/parse.go`).

### 1.8 Never firing

A definition's `Activation` (`internal/workflows/workflows.go`) is its two keys,
`Activation{When Key[Moment]; Labels Key[string]}`. `Key[T]`'s zero value is a key left out,
which constrains nothing, and `Key.Entries()` is a copy of what a key lists, each once, in the
order first written — none for a key left out. The zero `Activation`, writing neither key, is the
one way to say never: `Activation.Never()` reports it, why being in the load's warnings for the
file (`Set.WarningsFor`), and since a key's match reasons are the entries the occasion carried, an
activation writing no key has none to give and fires on nothing (§4). Only `parseDefinition`
(`internal/workflows/parse.go`) builds an `Activation` that fires, when both keys hold as read
(§1.6a), so a `Definition{}` no parse built — `Set.Lookup`'s miss — fires on nothing. The
definition keeps the zero `Activation` when the file:

- declares neither `when` nor `labels`, with the warning
  `no activation keys (when/labels): the definition never fires`;
- carries a key lit does not read (§1.7);
- writes a key with no entry lit reads — written empty, written as something other than a list of
  strings, every `when:` entry refused, or every `labels:` entry dropped — since that key can never
  hold, and dropping it would widen the definition to the other key alone; the key's own warnings
  say why (`internal/workflows/parse_test.go`,
  `TestParseDefinitionKeyWithNoReadableEntryNeverWidens`,
  `TestParseDefinitionBothKeysWrittenEmptyWarnEach`);
- has malformed frontmatter (§1.1, §1.2).

`readDefinition` (`internal/workflows/load.go`) gives a file it cannot read a definition that never
fires too, under the id its path gives, with a `cannot read: <err>` warning
(`internal/workflows/load_test.go`, `TestLoadUnreadableOverrideHoldsItsID`). A definition that
never fires:

- loads, is visible in `lit workflows` listings (`show` prints `fires: never`, §7.2; the overview
  binds it at no moment or label, §7.1), and **holds its id**, so a farther layer's definition of
  that id does not fire in its place,
- **never matches anything** — `MatchReasons` returns nil for it
  (`internal/workflows/workflows.go`, `internal/workflows/match.go`).

This is why a plain markdown file with no frontmatter loads but never fires
(`internal/workflows/parse_test.go`, `TestParseDefinitionNoFrontmatterNeverFiresWithWarning`).

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
| YAML unmarshal failure of the header | `invalid frontmatter: <err>` | `internal/workflows/parse.go` (`readFrontmatter`) |
| `when` or `labels` not a list of strings | `invalid frontmatter: %s %v`, the key's name and the YAML error | `internal/workflows/parse.go` (`readKey`) |
| `when` or `labels` written with no entries | `<key> lists no entry, so the definition never fires`, `<key>` being `when:` or `labels:` | `internal/workflows/parse.go` (`readKey`) |
| Blank or comma-bearing label | `label %q can never match: <err>` | `internal/workflows/parse.go` |
| Key lit does not read (file loads, never fires) | `this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on` | `internal/workflows/parse.go` (`unreadKeysMessage`) |
| Neither key declared | `no activation keys (when/labels): the definition never fires` | `internal/workflows/parse.go` |
| `when:` entry naming no moment lit fires | `when: entry %q never fires: %v`, `%v` being `ParseMoment`'s error (§1.6) | `internal/workflows/parse.go` |
| Duplicate ID *within one layer* | `duplicate id %q (already defined by %s): file ignored` | `internal/workflows/load.go` |

A malformed or unreadable file loads as a definition that never fires and holds its id (§1.8);
other files in the same layer still load (`internal/workflows/load_test.go`,
`TestLoadMalformedFileWarnsWithoutBreakingOthers`). `Warning.String()`
(`internal/workflows/workflows.go`) is the one-line rendering, `<source> <path>: <message>` with
the message's own newlines collapsed by `strings.Join(strings.Fields(msg), " ")`, shared by
`lit workflows` (§7.4), the stderr of every command that fires a moment, and its
`workflows.warnings` fact (§5).

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
| `EventCommentAdded` | `comment_added` | a comment lands on a ticket | `lit comment add`, `lit bulk comment` — `internal/workflows/events.go`, `internal/cli/bulk.go` |

`Catalog()` returns them in **display order**, which differs from declaration order:
`show_backlog, next_pulled, show_ticket, ticket_created, ticket_updated, comment_added`
(`internal/workflows/events.go`). This is the order `lit workflows` prints the events at the
head of its When section in (`internal/cli/workflows.go`, `Moments()` in
`internal/workflows/workflows.go`).

`ParseMoment` admits an event only when it is in `Catalog()` (`internal/workflows/parse.go`).
Catalog names are contractually lowercase snake_case, unique, and each reads back through `ParseMoment` as the
event, never as a crossing — enforced by `internal/workflows/events_test.go`
(`TestCatalogNamesAreStableContractShaped`). Every `Event` the package's source declares is in
`Catalog()`, so a when: entry can name each one; Go cannot enumerate a type's constants, so
`TestCatalogListsEveryDeclaredEvent` reads the declarations from the source.

Events are a **stable contract deliberately decoupled from command names** — commands may be
renamed freely, the event name is what user definitions bind to
(`internal/workflows/events.go`).

### 3.1 The Occasion payload

`Occasion` (`internal/workflows/match.go`) is the single payload type; its fields:

| Field | Meaning | Citation |
|---|---|---|
| `Moments []Moment` | what happened: the one event a read or an update fires, a creation's `ticket_created` and the states the new ticket starts in, or every crossing a transition made. Several when a state inside another moves with it — a start from open exits `open` and enters both `in_progress` and `plan`. Empty when a transition left the ticket where it was | `internal/workflows/match.go` |
| `IssueID model.ID` | acted-on ticket id; empty for backlog-wide moments. **Never read by matching** — used only for `<id>` interpolation, display, and tracing | `internal/workflows/match.go` |
| `Labels []string` | the ticket's labels in canonical form; nil when no single ticket | `internal/workflows/match.go` |

### 3.2 Per-occasion payloads, as actually built

Builders live in `internal/cli/workflow_events.go`:

| Occasion | Builder | Payload fields set |
|---|---|---|
| `show_ticket`, `next_pulled`, `ticket_updated`, `comment_added` | `ticketOccasion(event, issue)` `internal/cli/workflow_events.go` | Moments=`[event]`, IssueID=`issue.ID`, Labels=`issue.Labels`. `lit show` fires it with `EventShowTicket`, `lit next` with `EventNextPulled`, `lit update` with `EventTicketUpdated` and `lit comment add`/`lit bulk comment` with `EventCommentAdded`. `ticket_updated` **never carries a crossing** — `lit update` rejects `--status` (`internal/cli/workflow_events.go`; pinned `internal/cli/workflow_events_test.go`, `TestTicketOccasion`). |
| `show_backlog` | `backlogOccasion()` `internal/cli/workflow_events.go` | Moments=`[EventShowBacklog]` only. **No IssueID, no Labels.** |
| `ticket_created` | `createdOccasion(issue)` `internal/cli/workflow_events.go` | `ticketOccasion(EventTicketCreated, issue)` with the entries into the new ticket's configuration appended — `crossings(nil, model.ConfigurationOf(issue, nil))`, as a statechart's initial transition enters its initial configuration — so a ticket created open carries `ticket_created`, `enter open`, `enter live`; IssueID, Labels (the labels it was created with) (pinned `TestCreatedOccasionEntersTheInitialStates`). |
| every transition (start, phase, done, close, open, archive, unarchive, delete, restore) | `transitionOccasion(before, after)` `internal/cli/workflow_events.go` | Moments = `crossings(before.configuration(), after.configuration())`, IssueID=`after.issue.ID`, Labels (post-transition). |

`ticketState.configuration()` is `model.ConfigurationOf(issue, events)`
(`internal/model/phase.go`): every state node the ticket's row and history put it in, outermost
first within each region — its status, then the phase `PhaseOf` reads while it has one, then its
retention. `crossings(from, to []model.StateNode)` makes the moves in the order a statechart makes
them: the exits of every node `from` has and `to` lacks, innermost first (`from` walked backward),
then the entries of every node `to` has and `from` lacks, outermost first
(`internal/cli/workflow_events.go`). Pinned `internal/cli/workflow_events_test.go`
(`TestTransitionOccasionCrossesAsAStatechart`):

| Transition | Moments |
|---|---|
| start from `open` | `exit open`, `enter in_progress`, `enter plan` |
| reclaim (start while in progress) | none |
| done from `merge` | `exit merge`, `exit in_progress`, `enter closed` |
| close from `open` | `exit open`, `enter closed` |
| close from `impl` | `exit impl`, `exit in_progress`, `enter closed` |
| reopen (`lit open`) | `exit closed`, `enter open` |
| phase `impl` → `review` | `exit impl`, `enter review` |
| phase to the phase it is in | none |
| archive | `exit live`, `enter archived` |
| delete an archived ticket | `exit archived`, `enter deleted` |
| restore | `exit deleted`, `enter live` |

A transition that crosses nothing still fires; only a definition bound by labels alone can
fire on it (§4).
Each side's row and history are read by `readTicketState` before and
after `Store.Apply` (`internal/cli/cli.go`).

`transitionOccasion` is total: every transition builds its occasion the same way, with no
per-action exception. The retention verbs cross retentions and never the status: `lit archive`
makes `exit live`, `enter archived`; `lit unarchive` `exit archived`, `enter live`; `lit delete`
`exit live`, `enter deleted`; `lit restore` `exit deleted`, `enter live` — so a definition bound
to `enter closed` never fires on them, while one bound by labels alone does
(`internal/cli/workflow_events.go`; pinned `internal/cli/workflow_dispatch_test.go`,
`TestLabelBoundDefinitionsFireOnBulkCommentsAndRetentionMoves`, and `internal/cli/phase_test.go`,
`TestEveryCrossingFiresOnTheTransitionThatMakesIt`).

### 3.3 Dispatch call sites — where each occasion is actually fired

| Site | Moments | Position in output |
|---|---|---|
| `internal/cli/cli.go` (`newLeaf`) | `ticket_created`, `enter open`, `enter live` | **after** `CreateIssue` succeeds, **before** `printIssueSummary` and the `new` breadcrumb (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`followupLeaf`) | `ticket_created`, `enter open`, `enter live` | after create, before summary/breadcrumb (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`showLeaf`) | `show_ticket` | after `GetIssueDetail`, **before** either the `--field` output or the full detail view — fires for both (`internal/cli/cli.go`) |
| `internal/cli/cli.go` (`updateLeaf`) | `ticket_updated` | after `Store.Apply`, before summary/breadcrumb |
| `internal/cli/cli.go` (`applyTransition`), run by `transitionLeaf` (`lit start`/`phase`/`done`/`close`/`open`/`archive`/`unarchive`/`delete`/`restore`) and `bulkTransitionLeaf` (every `lit bulk` transition, `internal/cli/bulk.go`) | the crossings the transition made | after `authorize` and `Store.Apply`, on every transition. `loadedWorkflows.fire` returns the guidance, appended to the transition's owed notice (a claim hand-off, a phase move) as `transitionDone.said`. `transitionLeaf` writes `said` to stdout **before** `printIssueSummary`; `bulkTransitionLeaf` returns it as the item's said text, which `runBulkOver` prints ahead of the item's `<id> ok` line (`internal/cli/bulk.go`) |
| `internal/cli/cli.go` (`addComment`), run by `commentAddLeaf` and `bulkCommentLeaf` (`internal/cli/bulk.go`) | `comment_added` | after `Store.AddComment`. `addComment` returns the guidance: `commentAddLeaf` writes it before `printComment`; `bulkCommentLeaf` returns it ahead of the `commentLine` as the item's said text, which `runBulkOver` prints ahead of the item's `<id> ok` line |
| `internal/cli/next.go` (`nextLeaf`) | `next_pulled` | **last** — after the start advice and `printNextSummary` (`internal/cli/next.go`). Only reached when a row was actually served; `Exhausted`/`NoWork` return an error before any occasion is built (`internal/cli/next.go`) |
| `internal/cli/workable.go` (`workableLeaf`) | `show_backlog` | **last** — after the table render (`internal/cli/workable.go`) |

`backlogView` is the only `workableView` that sets an `occasion` function
(`internal/cli/workable.go`); it is invoked unconditionally at
`internal/cli/workable.go` as `view.occasion(rows)`.

The read, update and creation sites fire through `dispatchOccasion`; the transition and comment
sites fire through the `loadedWorkflows` their command loaded once (`loadWorkflows`, §5), one load serving every item of
a bulk command.

---

## 4. Matching semantics

`Set.Matching(Occasion) []Match` (`internal/workflows/match.go`) returns, in the set's ID order, a
`Match{Definition, Reasons}` for each definition whose `MatchReasons(o)` is non-empty, so firing
and its reasons are one computation and the reasons travel with the definition they fired:

```
MatchReasons(o) = Activation.reasons(o): When.carried(o.Moments) holds AND Labels.carried(o.Labels) holds,
                  each carried entry a reason; nil otherwise, and nil for an activation writing no key
```

- **OR within a key, AND across keys.** A key left out constrains nothing
  (`internal/workflows/match.go`, `internal/workflows/workflows.go`). Pinned
  `internal/workflows/match_test.go` (`TestMatchesSingleKeys`, `TestMatchesAndAcrossKeys`).
- `(Key[T]).carried(values []T) ([]T, bool)` (`internal/workflows/match.go`) is the one test both
  keys share: the key's entries the occasion carries, in listed order, and whether the key holds —
  a key left out holds on anything; a written one holds when the occasion carries **at least one**
  of its entries.
  - For `when`, values are `Moment`s compared by `==`: a `Crossing` is equal when its side and
    state node are, an `Event` when its name is. So a `when:`-bound definition can never fire on
    an occasion with no moments, such as a transition that crossed nothing.
  - For `labels`, exact string comparison — both sides are already canonicalized
    (`internal/workflows/parse.go`, `internal/workflows/match.go`).

**There are no wildcards, no globs, no regexes, and no negation** anywhere in matching. The only
"match everything" construct is *omitting* a key. Verify by reading the whole of
`internal/workflows/match.go` — the one comparison is `slices.Contains`, equality only.

**Precedence between multiple matches: there is none — every match fires.**
`Set.Matching` returns every matching definition, in the Set's ID-ascending order
(`internal/workflows/match.go`; ordering pinned `internal/workflows/match_test.go`).
The only "precedence" in the system is the layer/ID override applied at load time (§2.4), which
means at most one definition exists per ID.

### 4.1 MatchReasons — the "why"

`Definition.MatchReasons(o)` (`internal/workflows/match.go`) returns nil if the definition
does not match, which a definition that never fires never does; otherwise a list of strings in
this fixed order:

1. `when:<moment>` for each of the definition's `when:` moments the occasion carries, in the
   definition's order, spelled by the moment's `String()` — the canonical `when:` spelling
   (`internal/workflows/match.go`).
2. `label:<label>` for each of the definition's **declared** labels that appear in the occasion's
   labels (`internal/workflows/match.go`).

Pinned `internal/workflows/match_test.go` (`TestMatchReasonsNamesWhatMatchedInEachKey`): a
definition whose `when` lists `exit plan`, `enter closed` and `exit review` and whose `labels`
lists `needs-design` and `blocked`, on an occasion carrying
`exit review, exit in_progress, enter closed` and the label `blocked`, gives
`["when:enter closed", "when:exit review", "label:blocked"]` — only what overlapped, in the
definition's order.

Example from a real trace: `["when:show_ticket", "label:needs-design"]`
(`internal/workflows/dispatch_test.go`).

This is the single shared "why" computation behind both the real firing trace and `dry-run`
(`internal/workflows/match.go`).

---

## 5. Firing mechanics

`workflows.Fire(set Set, ws workspace.Info, o Occasion) Firing`
(`internal/workflows/dispatch.go`). It matches the occasion against a `Set` the command already
loaded and returns `Firing{Fired []Match; Guidance string; Untraced error}`: the matches that
fired, each a definition and its reasons, in `Set.Matching`'s ID order; the guidance they inject, each body `Interpolate`d with
the occasion's `IssueID`, followed by a newline, one after another; and why the firing trace was
not recorded, nil when it was or when nothing fired. The command prints the guidance where its own
output puts it (§3.3). Its one effect is the firing trace (§6).

The command side is `internal/cli/workflow_dispatch.go`:

- `loadWorkflows(ctx, inv, ws)` runs `workflows.Load` once for the command and returns a
  `loadedWorkflows` holding the Set, the workspace and the invocation's stderr. It prints every
  load warning to stderr, one line apiece, via `fmt.Fprintf(inv.Stderr, "lit: workflow %s\n", line)`
  — the line is `Warning.String()` — in `WarningsByLayer` order, and sets `workflows.warnings` on
  the command's event to those lines. A bulk command calls it once, before its first item, and
  fires every item's occasion against that one load; `lit workflows dry-run` calls it too (§7.3).
- `(loadedWorkflows).fire(ctx, o)` calls `Fire`, prints
  `lit: workflow firing trace could not be recorded (%v)` on stderr when `Untraced` is set, and
  sets `workflows.when`, `workflows.fired` and `workflows.trace_error` on ctx's event, returning
  the guidance. Every firing goes through it.
- `dispatchOccasion(ctx, inv, ws, o)` is the whole path of a read, `lit update` and a creation:
  load, fire, and write the guidance to stdout.

The four facts:

| Fact | Value |
|---|---|
| `workflows.when` | `workflows.Spellings(o.Moments)` — the occasion's moments in their `when:` spelling, `[]` when empty |
| `workflows.fired` | the fired definitions' ids, `[]` when empty |
| `workflows.trace_error` | `telemetry.ErrorText(firing.Untraced)` — the trace failure's text, null when there was none |
| `workflows.warnings` | each load warning's `Warning.String()` line, `[]` when empty |

So "nothing is bound here" (`fired` empty, `warnings` empty) and "what was bound here could not be
read" (`warnings` non-empty) are told apart from the event alone. In `lit bulk`, `runBulkOver`
runs each item in its own `telemetry.Occurrence` scope, so `workflows.when`, `workflows.fired` and
`workflows.trace_error` land per item under `bulk.item.facts`, while `workflows.warnings`, set
before the first item, is on the command's event once (`internal/cli/bulk.go`; pinned
`internal/cli/phase_test.go`, `TestEveryCrossingFiresOnTheTransitionThatMakesIt`, and
`internal/cli/workflow_dispatch_test.go`,
`TestLabelBoundDefinitionsFireOnBulkCommentsAndRetentionMoves`).

- **Synchronous, in-process, called directly from the command path — no bus, no async queue, no
  subscriber list; there is exactly one `Fire`, not a registry**
  (`internal/workflows/dispatch.go`).
- **The definition Set is loaded from disk once per command**, through `loadWorkflows`. There is
  no caching across commands: every firing command walks all three layers from disk. A transition
  verb, `lit comment add` and a bulk command load before their write, so the warnings print even
  when the write is then refused; a read moment's `dispatchOccasion` loads after its read
  succeeded (`internal/cli/cli.go`, `internal/cli/bulk.go`).
- **Every load/parse warning is printed to stderr** (above), so a file lit cannot read or cannot
  fully fire (one written for another lit, say) is announced by every command that fires a moment,
  not only by `lit workflows`. stderr, never stdout: stdout is the agent-facing stream, where a
  config diagnostic would read as guidance. Pinned `internal/cli/phase_test.go`
  (`TestDefinitionsInTheRetiredSpellingsWarnAndNeverFire`: a file with an `events:` key, one with
  a `states:` key and one listing a retired event under `when:` each warn on `lit done`'s stderr
  and never fire, and `lit done` still succeeds).
- `Fire` writes no guidance itself; the command prints what it returns to **stdout**:
  `dispatchOccasion` directly, `applyTransition` as part of `transitionDone.said`, and `addComment`
  as its returned guidance (§3.3) — the same agent-facing stream every way.
- Output order = `Set.Matching` order = ID-ascending
  (`internal/workflows/dispatch.go`; pinned `internal/workflows/dispatch_test.go`,
  `TestFireInjectsMultipleMatchesInIDOrder`).
- No environment variable is set, no subprocess is spawned, no timeout exists — there is no
  execution (`internal/workflows/dispatch.go` in full; `internal/workflows/workflows.go`). The
  `env` `Load` takes reads the global layer's location
  (`globalWorkflowsDir`, `internal/workflows/load.go`).

### 5.1 Blocking / exit-code interaction

- `Fire` is a blocking function call on the command's own goroutine; the command does not
  continue until it returns.
- `Fire` returns no error. The only error on the firing path is the command's write of the
  guidance to stdout, which **propagates as the command's error** at every call site
  (`internal/cli/workflow_dispatch.go`, `internal/cli/cli.go`, `internal/cli/bulk.go`); in
  practice a workflow can never fail a command.
- A malformed or broken workflow file **cannot break a lit invocation** — it degrades to a warning
  and a definition that never fires (`internal/workflows/load.go`; pinned
  `internal/workflows/load_test.go`, `internal/cli/phase_test.go`).
- A **trace `Fire` cannot record never fails it** — the definitions fired all the same, and the
  guidance is returned; the failure comes back as `Firing.Untraced`
  (`internal/workflows/dispatch.go`; pinned `internal/workflows/dispatch_test.go`,
  `TestFireReportsATraceItCannotRecord`), and `(loadedWorkflows).fire` prints it on the
  invocation's stderr as `lit: workflow firing trace could not be recorded (%v)` and records its
  text as `workflows.trace_error` (`internal/cli/workflow_dispatch.go`).
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
  (`internal/workflows/dispatch.go`; pinned `internal/workflows/dispatch_test.go`,
  `TestFireRecordsFiringTraceOnlyWhenSomethingFires`).
- `trace.Write` **refuses a storage dir that is not an absolute path** (`internal/trace/trace.go`,
  `filepath.IsAbs`) with `write %s trace: storage dir %q is not an absolute path`, so a record
  never lands relative to the process CWD; every trace kind writes through it, and `Fire` reports
  the refusal like any trace it cannot record (§5.1; pinned `internal/trace/trace_test.go`,
  `TestWriteRefusesAStorageDirThatIsNotAbsolute`, and `internal/workflows/dispatch_test.go`,
  `TestFireReportsATraceItCannotRecord`).
- Filename: `<UTC timestamp 20060102T150405.000000000Z>-<slug>.json`, where slug is
  `trace.Slug(strings.Join(Spellings(o.Moments), " "))` (`internal/workflows/trace.go`,
  `internal/trace/trace.go`). `Slug` lowercases, replaces every run of non-`[a-z0-9]` with `-`,
  trims leading/trailing `-`, and falls back to `"trace"` when the result is empty
  (`internal/trace/trace.go`). So `show_ticket` → `show-ticket` and
  `exit review enter closed` → `exit-review-enter-closed` (`internal/trace/trace_test.go`), and
  an occasion with no moments — a labels-only definition firing on a transition that crossed
  nothing, such as a reclaim — slugs to `trace`.
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
  "when": ["show_ticket"],       // always present, [] for no moments; Spellings(o.Moments)
  "issue_id": "lit-42",          // omitempty
  "labels": ["needs-design"],    // omitempty
  "fired": [
    { "id": "needs-design-note", "source": "project", "path": "needs-design.md",
      "reasons": ["when:show_ticket", "label:needs-design"] }
  ]
}
```

A transition's record carries every crossing it made, e.g.
`"when": ["exit review", "exit in_progress", "enter closed"]` for a `lit done` from `review`.

`FiredDefinition` fields and their JSON tags: `id`, `source`, `path`, `reasons`
(`internal/workflows/trace.go`). `when` and `fired` have no `omitempty`, so a record of no moments
reads apart from one missing the field (`internal/workflows/trace.go`). `recorded_at` uses `time.RFC3339Nano`
(`internal/workflows/trace.go`).

---

## 7. `lit workflows` — the command surface

Registered as a workspace-only command (no store access) at `internal/cli/register.go`:

- Summary: *"See the work lifecycle and the guidance active at each point (`workflows show <id>`
  resolved, `edit <id>` to customize, `new --when <moment>` to add one, `dry-run` to explain a
  hypothetical)"*
- GroupID `guidance`; declared subcommands for completion: `show`, `edit`, `new`, `dry-run`.
- Run via `r.wsCmdPipeline(withWDAcquire(workflowsDispatch))` (`internal/cli/register.go`), i.e. it resolves a workspace from
  the working directory but never opens the store (`internal/cli/register.go`,
  `internal/cli/cli.go`).

Usage string (`internal/cli/workflows.go`):

```
usage: lit workflows [show <id> | edit <id> | new [--when <moment>]... [--label <name>]... | dry-run [--when <moment>]... [--label <name>]... [--issue <id>]]
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
| `edit <id>` | open or override a loaded definition, `positionals: 1` (`internal/cli/workflows.go`) |
| `new` with `--when`/`--label` | scaffold a new definition, `positionals: 0` (`internal/cli/workflows.go`) |
| `dry-run` | hypothetical, `positionals: 0` (`internal/cli/workflows_dryrun.go`) |

All five set `usage: workflowsUsage`. Overview, show and edit register no flags beyond the
implicit `--help`; new registers the repeatable `--when` and `--label` (§8), and dry-run its own
(§7.3).

### 7.1 Bare `lit workflows` — the overview

`renderWorkflowsOverview` (`internal/cli/workflows.go`) prints, in order:

1. Header line: `lit workflows — work lifecycle guidance (project > global > embedded)`
   (`internal/cli/workflows.go`).
2. Blank line + `When` (`internal/cli/workflows.go`), then one line per moment of
   `workflows.Moments()`, spelled by its `String()` and indented 2 spaces
   (`internal/cli/workflows.go`). A definition is listed at a moment when `boundAt` finds it: its
   `Activation.When.Entries()` contain the moment; one that never fires writes no key, so is
   bound nowhere.
3. Blank line + `Labels` (`internal/cli/workflows.go`). If no definition binds any label, prints
   `  (none bound)` (`internal/cli/workflows.go`). Otherwise one line per label.
4. Warnings section (§7.4).

Each spine point line is `printSpinePoint` (`internal/cli/workflows.go`): the bare label if
nothing is bound there, else `<label>  [<ref>, <ref>, …]` where each ref is `formatDefinitionRef`:
`<id> "<name>" (<source>)` when a name is set, else `<id> (<source>)`
(`internal/cli/workflows.go`).

Spine composition:

- **When spine** = `workflows.Moments()` (`internal/workflows/workflows.go`): the full catalog,
  always, whether bound or not, then `enter <state>` and `exit <state>` for each state node of
  `model.StateNodes()` in order (`open`, `in_progress`, `plan`, `impl`, `review`, `merge`,
  `closed`, `live`, `archived`, `deleted`) — the same for every workspace, whatever its
  definitions name (pinned `internal/cli/workflows_test.go`,
  `TestWorkflowsOverviewWalksThePhasesInsideInProgress`). An entry naming no moment lit fires
  appears on no spine line and is under Warnings instead
  (`TestWorkflowsOverviewWarnsOfAStateTheLifecycleLacks`: `when: [enter verify]` puts no `verify`
  on the spine, and the overview warns
  `project verify.md: when: entry "enter verify" never fires: unknown state "verify": a ticket's states are open, in_progress, plan, impl, review, merge, closed, live, archived, deleted`).
- **Labels spine** = `spineLabels(set)`: every label the `Labels.Entries()` of a loaded definition
  that fires lists, sorted and compacted — not every label ever used on a ticket
  (`internal/cli/workflows.go`).

A definition appears at **every** moment and label its keys list (`boundAt`; pinned
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
when: <Spellings(keys.When.Entries()) comma-space joined, or ->
labels: <keys.Labels.Entries() comma-space joined, or ->
<the file's warnings, when it has any (§7.4)>
---
<body>
```

`orDash` renders `-` for an empty value (`internal/cli/workflows.go`); `when:` lists each entry
in its canonical spelling via `workflows.Spellings` (`internal/cli/workflows.go`), and a key left
out shows `-`. A definition that never fires (`Activation.Never()`) prints the one line
`fires: never` in place of the `when:` and `labels:` lines. Then
`printWorkflowWarnings(w, set.WarningsFor(def))` prints the load's warnings about that
definition's file (`Set.WarningsFor` matches a warning's source and path), under the heading the
overview uses (§7.4), so a definition that fires less than it says, or never, shows why beside when
it fires (pinned `internal/cli/workflows_test.go`,
`TestWorkflowsShowNamesWhyADefinitionFiresLessThanItSays`: a file with an unbracketed `when:`
shows `fires: never` and `invalid frontmatter: when:`, one with an `events:` key `fires: never` and
`this lit does not read "events"`).

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

`readKeyFlags(whenTexts, labelTexts)` (`internal/cli/workflows.go`) reads the flags as a
definition's own keys read their entries, through `workflows.Entries` (§1.6a), for `dry-run` and
`new` alike; the occasion is `Occasion{Moments, IssueID: --issue, Labels}` from what it returns.
A repeated value counts once: `--label Foo --label foo --when show_ticket --when SHOW_TICKET`
gives `occasion: when=show_ticket labels=foo issue=-` (pinned
`TestWorkflowsDryRunReadsARepeatedFlagOnce`). Every `--when` value goes through
`workflows.ParseMoment`, so a dry-run is asked in the spelling a definition is written in, and
canonicalized the same way (`--when "Enter  Review"` is `enter review`, `--when "enter in-progress"`
is `enter in_progress`). The first value `ParseMoment` refuses — a blank, an event outside the
catalog (a lone `enter` among them), a state the lifecycle lacks — is refused with a
`model.ValidationError` (exit 3): `--when %q is not a moment lit fires: %v`, `%v` being
`ParseMoment`'s error (§1.6; pinned `internal/cli/workflows_test.go`,
`TestWorkflowsDryRunRefusesAMomentLitNeverFires`).

Every `--label` value goes through `model.NormalizeLabel`, so it is trimmed and lowercased into
the form a stored label and a definition's label take: `--label " Needs-Design "` matches a
definition bound to `needs-design`. The first blank or comma-bearing value is refused with a
`model.ValidationError` (exit 3): `--label %q is not a label a ticket can carry: %v` (pinned
`internal/cli/workflows_test.go`, `TestWorkflowsDryRunReadsLabelsAsATicketCarriesThem`).
Every `--when` is read before any `--label`, so a refused `--when` is the error.

Dry-run loads the definitions through `loadWorkflows` (§5), as a firing command does, so each
load warning prints on stderr as `lit: workflow <warning>` and lands in the event's
`workflows.warnings`: with a project `done.md` carrying `events:`, `--when "enter closed"` prints
`Fired (0)` and, on stderr, `lit: workflow project done.md: this lit does not read "events"…`
(pinned `TestWorkflowsDryRunNamesWhatCanNeverFire`).

Output (`internal/cli/workflows_dryrun.go`, `printDryRunOccasion`):

```
occasion: when=<comma-joined spellings|-> labels=<comma-joined|-> issue=<id|->
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

`when=` is `workflows.Spellings(o.Moments)` joined by `,`, and `labels=` the labels joined by
`,`, each `-` when none (`orDash`, `internal/cli/workflows_dryrun.go`); a start from open reads
`when=exit open,enter in_progress,enter plan` (pinned `internal/cli/workflows_test.go`,
`TestWorkflowsDryRunReadsEveryWhenSpelling`).

Concrete pinned example (`internal/cli/workflows_test.go`,
`TestWorkflowsDryRunExplainsAHypotheticalMatch`): with a project file `design.md` bound to
`labels: [needs-design]` + `when: [enter closed]`,
`lit workflows dry-run --when "enter closed" --label needs-design --issue lit-7` prints
`occasion: when=enter closed labels=needs-design issue=lit-7`, `Fired (2)` (the project file
**plus the embedded `done` default**, `done "Post-close capture reminder" (embedded)  [when:enter closed]`)
and the line `design-note (project)  [when:enter closed, label:needs-design]`.

**Dry-run never writes a firing trace** (`internal/cli/workflows_dryrun.go`; pinned
`internal/cli/workflows_test.go`, `TestWorkflowsDryRunNeverRecordsAFiringTrace`) and never reads
the store (`internal/cli/workflows_dryrun.go`).

### 7.4 Warnings display

`printWorkflowWarnings` (`internal/cli/workflows.go`) — appended to the overview with every
warning, and to `show <id>` with that definition's file's warnings (§7.2)
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

## 8. `lit workflows edit` and `new` — overriding and scaffolding

`workflowsEditLeaf` (`internal/cli/workflows.go`) takes one loaded definition's id and no flag, to
open or override it (§8.1). `workflowsNewLeaf` takes the keys of a new definition and no
positional, to scaffold it (§8.2); the family table tells the two apart, so no text is read two
ways. `new` registers two repeatable string-array flags:

| Flag | Help text |
|---|---|
| `--when <moment>` | `A moment the new definition fires at, spelled as a when: entry: enter <state>, exit <state>, or an event (repeatable)` |
| `--label <name>` | `A label the new definition's tickets carry (repeatable)` |

Which path runs:

| Arguments | Path |
|---|---|
| `edit` and one positional | `runWorkflowsEdit(inv, ws, id)` (§8.1) |
| `new` and at least one `--when` or `--label` | `newDefinition(inv, ws, when, labels)` (§8.2) |
| `edit` with no id, an extra positional or a flag; `new` with no flag or a positional | `UsageError` with `workflowsUsage` (exit 2) |

Pinned `internal/cli/workflows_test.go`, `TestWorkflowsEditAndNewRejectWhatTheirShapeLacks`
(`edit done extra`, `edit done --when "enter closed"`, bare `edit`, bare `new`, and
`new done --when "enter closed"`).

### 8.1 `edit <id>` — an existing definition

`runWorkflowsEdit` (`internal/cli/workflows_edit.go`) loads the Set and looks the id up. An id
nothing loaded holds is a `model.ValidationError` (exit 3) naming the command that scaffolds a new
definition, and nothing is written:
``no workflow definition with id %q (run `lit workflows` to see loaded ids); to scaffold a new one, name when it fires: `lit workflows new --when "enter closed"`, `--label needs-design`, or both``
(pinned `TestWorkflowsEditRefusesAnIDNothingHolds`: `lit workflows edit "enter closed"` is that
refusal). A loaded id goes to `editExistingDefinition` (`internal/cli/workflows_edit.go`):

- Project path is `<RootDir>/.lit/workflows/<def.Path>` (with `/` converted to the OS separator)
  (`internal/cli/workflows_edit.go`).
- **Source is `project`** → the file *is* the definition; just print/open it, no rescaffold
  (`internal/cli/workflows_edit.go`; pinned `internal/cli/workflows_test.go`,
  `TestWorkflowsEditReopensAnExistingProjectOverrideWithoutRescaffolding`).
- **Source is `global` or `embedded`** → refuse if the project path already exists
  (§8.4), read the raw bytes via `workflows.RawDefault`, write them **verbatim** to the project
  path as the override, print
  `scaffolded override for "<id>" (was <source>) -> <path>`, then open/print
  (`internal/cli/workflows_edit.go`; pinned `internal/cli/workflows_test.go`,
  `TestWorkflowsEditOverridesEmbeddedDefaultVerbatim`).

`workflows.RawDefault(source, relPath)` (`internal/workflows/scaffold.go`):
- `global` → reads `<config.ConfigDir()>/workflows/<relPath>`; error
  `read global workflow default %s: %w` (`internal/workflows/scaffold.go`).
- `embedded` → reads from the embedded FS; error `read embedded workflow default %s: %w`
  (`internal/workflows/scaffold.go`).
- `project` (or anything else) → error
  `workflows: no raw default for the %s layer (it is the override target, not a source to copy from)`
  (`internal/workflows/scaffold.go`; pinned `internal/workflows/scaffold_test.go`).

### 8.2 `new --when <moment>... --label <name>...` — a new definition

`newDefinition` (`internal/cli/workflows_edit.go`) scaffolds a new project-layer
definition firing on exactly the keys the flags name:

1. `readKeyFlags` (`internal/cli/workflows.go`, §7.3) reads the flags as a definition's own keys
   read their entries — each `--when` by `workflows.ParseMoment`, each `--label` by
   `model.NormalizeLabel`, each value once. The first value naming no moment lit fires or no label
   a ticket can carry is a `model.ValidationError` (exit 3), `--when %q is not a moment lit fires: %v`
   or `--label %q is not a label a ticket can carry: %v`, and nothing is written (pinned
   `TestWorkflowsNewRefusesWhatNoKeyReads`: `--when "enter reveiw"`, `--when work_finished`,
   `--when "exit in progress"`, `--when "enter closed" --label a,b`, and `--label "  "`).
   A `--label` is a label whatever it reads like: `--label "exit plan"` binds the label
   `exit plan`.
2. `bindingOrder(when, labels)` puts the entries in one order, the moments in
   `workflows.Moments()` order and the labels sorted, since a key's entries are a set;
   `scaffoldName(when, labels)` names the file (below). The name is the new definition's id
   (`workflows.DefaultID`), so the loaded set is asked for it first, and an id a loaded
   definition holds is a `MergeConflictError` (exit 5) naming it:
   ``cannot scaffold %q: the %s definition %s already holds that id, and two definitions of one id never both fire (run `lit workflows edit %s` to change that one)``
   (pinned `TestWorkflowsNewRefusesAnIDALoadedDefinitionHolds`: `--label done` and
   `--label Phase-Impl` name the embedded default, and `--when "enter closed"` names a project
   `notes.md` declaring `id: enter-closed`). Then `refuseExistingFile` refuses an existing file
   (§8.4), with the name as its subject.
3. `writeWorkflowScaffold` writes `workflows.ScaffoldFresh(when, labels)` (§8.3), then stdout gets
   `scaffolded a new definition at %s`, and the file is opened/printed (§8.5).

Filename:
- `scaffoldName(when, labels)` is each moment's `Spellings` entry, then each label, in
  `bindingOrder`, each made filename-safe by `scaffoldFilenameSlug` and joined by `+`, plus `.md`
  (`internal/cli/workflows_edit.go`). `scaffoldFilenameSlug` replaces every run of characters
  outside `[a-z0-9_-]` with `-` (`unsafeScaffoldFilenameChars`), trims leading/trailing `-`, and
  falls back to `"definition"` if empty; the entries reach it already canonical, lowercase
  included. **Underscore is deliberately kept** so `show_ticket` → `show_ticket.md` and
  `enter in_progress` → `enter-in_progress.md` (`internal/cli/workflows_edit.go`).
- The name comes from what the file binds, so every spelling and ordering of one binding names
  one file: after `--when "enter in_progress"`, `--when "Enter In-Progress"` is refused as a
  `MergeConflictError` (exit 5), as is `--when show_ticket --when "enter closed"` after
  `--when "enter closed" --when show_ticket`, and `--label a --label b` after
  `--label b --label a` (pinned `TestWorkflowsNewScaffoldsOneFilePerBinding`). A slug is lossy,
  so distinct bindings can share a name (`--label "needs design"`, `--label needs-design`); the
  second is refused naming the first.
- Worked examples (pinned `TestWorkflowsNewScaffoldsADefinitionOnTheKeysItNames`, each loading
  with no warning and binding exactly its keys): `--when show_ticket` → `show_ticket.md`;
  `--when "enter review"` → `enter-review.md`; `--when "Enter  In_Progress"` →
  `enter-in_progress.md`; `--when "exit in-progress"` → `exit-in_progress.md`;
  `--when "enter archived" --when "exit live"` → `exit-live+enter-archived.md`;
  `--when "enter closed" --when show_ticket` → `show_ticket+enter-closed.md`;
  `--label Needs-Design` → `needs-design.md`; `--label needs-design --label blocked` →
  `blocked+needs-design.md`; `--label "exit plan"` → `exit-plan.md`;
  `--when "enter closed" --label needs-design` → `enter-closed+needs-design.md`.

### 8.3 Fresh-scaffold file content

`ScaffoldFresh(when []Moment, labels []string) []byte` (`internal/workflows/scaffold.go`) emits
exactly:

```
---
# when: lists the moments the definition fires at, labels: narrows it to
# tickets carrying a label. Entries within one key are alternatives (OR);
# declaring both keys requires both (AND).
# Run `lit workflows --help` for the full format and every moment.
<when line>
<labels line>
# id: my-custom-id     # optional; defaults to this file's relative path under .lit/workflows/
# name: My Guidance    # optional pretty name shown by `lit workflows`
---
Write the guidance to inject here. `<id>` is replaced with the acted-on ticket's id when there is one.
```

Each key line is `keyLine(key, entries, example)`: a key with entries is written live, each entry
double-quoted by `strconv.Quote` — `when: ["enter closed"]`, `when: ["exit plan", "show_ticket"]`,
`labels: ["a", "b"]` — and a key with none is written commented, `"# " + key + ": " + example`.
The `<when line>`'s example is
`[enter review, exit plan, show_ticket]  # entering or exiting a state, or an event` and the
`<labels line>`'s `[needs-design, blocked]`, each commented out after its key's name when the
flags leave that key out. The same lines are written every time; whether each key is live is the value that varies.

Line-by-line: `internal/workflows/scaffold.go` (`---`) (the four comment lines) (when line, live
or commented) (labels line, live or commented) (id comment) (name comment) (closing `---`) (body
placeholder). Pinned `internal/workflows/scaffold_test.go`
(`TestScaffoldFreshWritesEachKeyLiveOrCommented` — a moment, a label, and both keys;
`TestScaffoldFreshParsesCleanly` — scaffolds of a crossing, a phase move, an event, a label, and
`enter archived` with two labels each parse to exactly their keys with no warning).

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
(`internal/cli/hooks.go`), contains no reference to `Occasion`, `Fire`, or any event
constant, and no workflow event fires on `lit hooks install`
(`internal/cli/hooks.go` — the only output is `installed <path>`). The only shared vocabulary
is the word "hook" and the sibling trace-kind comment at `internal/cli/sync_trace.go` noting
"automation" and "workflows" as adjacent trace kinds.

---

## 10. Summary of things a workflow author can and cannot do

**Can:** bind on `when`, `labels`, or both; bind `enter <state>` and `exit <state>` independently;
nest files arbitrarily; override a farther layer by ID or by matching relative path; use `<id>` in
the body; list an event a newer `lit` fires (the entry warns and never fires, and the list's other
entries still bind).

**Cannot:** run anything, receive environment variables, gate/block a command, set an exit code,
bind a state outside `model.StateNodes()` (`internal/model/lifecycle/state_node.go`),
use a wildcard/glob/regex matcher, negate a matcher, order or prioritize between simultaneously
matching definitions (all fire, ID-ascending), require ALL labels (label matching is OR — see
`internal/workflows/match.go`), match on issue ID, tell which command made a crossing (`lit done`
and `lit close` both make `enter closed`), fire a `when:`-bound definition on a transition that
crossed nothing (`internal/workflows/match.go`), or have a file fire while it carries a key this
`lit` does not read (`internal/workflows/parse.go`).
