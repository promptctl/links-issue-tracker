# Workspace schema: user-defined hierarchy, fields, states, and templates — the design

Status: draft, first pass. Written 2026-08-27. This is the feature the
event-store charter names twice in its birth requirements
([charter](../event-store/charter.md), "Birth requirements": *"validation as a
generic engine over that config, not hand-coded rules… primitives for the
access-control layers and the user-defined hierarchy feature"*) and whose
storage home the event-store design already reserves
([design](../event-store/design.md) §config-stream: *"hierarchy and
required-field templates"*). Nothing here contradicts those charters; this
document is the design of their planned consumer. Sections marked **OPEN** are
decisions a later session must close.

## The shape in one paragraph

lit's domain model — which ticket types exist, which may contain which, what
fields they carry and what makes a field value legal, what states a ticket
moves through and what guards each move, which relations may connect what, and
how a ticket renders — becomes one declared value: the **workspace schema**, a
human-authored file the machinery interprets. The engine ships no vocabulary
of its own; it ships an interpreter plus a **default profile** — a schema file
expressing lit's opinionated discipline — and a workspace that wants
`feature → story → task` with story points and a review state writes a
different file, not different code. Validation becomes a parser: raw input
crosses one boundary and comes back typed or rejected, and "required" is
demoted from the only rule to one datum among many. The acceptance test for
the machinery is that the default profile is pure data: **no Go source
contains the word "epic."**

## Chesterton's Trampoline — motivating problems, not requirements

Behavioral compatibility with today's lit is a **non-goal**. What is binding
is the register below: every current lit-ism exists because it answered a
concrete question about making a single autonomous agent work well, and each
answer, taken seriously, propels the design somewhere better than
preservation would. The obligation is to the question, never to the fence.

| Current lit-ism | The question it answered | Where this design lands |
|---|---|---|
| Agents flood sequenced epics with `blocks` edges | How does an agent know ticket 3 follows ticket 2 without an edge per pair? Real schedulers read the sequence. | Within a work unit, **rank is the only sequencer**. Sibling dependency edges are not discouraged — they are illegal, rejected by the engine as redundant with rank. |
| Epics are worked as a unit; agents shouldn't cherry-pick across them | Where is the boundary of "one piece of work"? | A type declares itself a **work-unit boundary**. The scheduler consumes one unit at a time; dependency edges may not reach inside one. |
| Dependency edges between epics are legitimate ("platform before UI"), between tasks absurd ("makefile before header styling") | At which altitude does *finish-A-before-starting-B* carry information? | **Relation legality is declared per type pair** in the schema, exactly parallel to the `children` allow-list. The default profile permits `depends` only between work units. |
| Epics have no status of their own; state is the fold of children | What does an agent do with an open-but-empty container? (It gets confused.) | **Rollup lifecycle is a first-class variant** any container type may declare — one arm of a sum, not epic-special code. |
| `ready.required_fields` (name-only, readiness-only) | How do we stop half-described tickets from being picked up? | **Guards on transitions.** "Required" is a presence guard on the create (or start) transition; the same guard mechanism carries kind checks and custom rules. |
| Free-form `relates` edges | Tickets genuinely reference each other outside any dependency semantics. | Kept, as a schema-declared relation with reference (not scheduling) semantics. A hard requirement. |

The register grows as design work uncovers more fences; each new entry records
the question before proposing the landing.

## Design principles

1. **Machinery and profile are different deliverables.** The machinery is a
   set of orthogonal, opinion-free primitives. The shipped default profile is
   a *product decision written as data*, justified line-by-line by the
   trampoline register. Arguments about "what lit should do" are arguments
   about a YAML file, never about the interpreter.
   ([project-intent](../project-intent.md): *"make the intervention points a
   stable, uniform substrate, and make the intervention policy data that flows
   through them"* — this feature is that sentence, executed.)
2. **Minimal-choice for agent consumers.** For an autonomous agent, every
   representable choice is a tax. The default profile is the minimal-choice
   profile: where the discipline can decide (what's next, whether an edge is
   needed), the schema decides, and the agent's option simply does not exist.
3. **Single-agent-first.** The core workflow is one agent per project,
   anywhere on the interactive↔autonomous spectrum. "What should be worked
   next" therefore has one consumer, so a total order per level — rank —
   is a complete scheduling answer. Multi-agent layers on later as multiple
   *claimants against the same order* (the claims machinery that already
   exists), never as a new scheduling model. The primitive that must not be
   corrupted: the schema defines the work discipline; consumers are
   interchangeable.
4. **Rules are data; the engine is a fixed interpreter.** The validation
   engine evaluates schema declarations; users extend behavior by writing
   data (a new enum, a regex, a transition), never by inducing Go rule
   changes. This is what keeps the event-store fold deterministic and the
   fold version stable across schema edits
   ([design](../event-store/design.md) §derived-state: a rule change bumps
   the fold version fleet-wide; a *data* change is just a new config version
   that events cite).
5. **The schema is human-authored and agent-read.** It is a file written and
   reviewed like code by users who want that control. Agents consume it; no
   lit command mutates it. (This also keeps it clean for the signed config
   stream later — schema changes become signed config events authored by a
   principal, which the write layer already models.)

## The meta-model

One value, versioned, declaring four vocabularies and their laws. Rendered
here as Go-shaped types for precision; the user-facing syntax is YAML (below).

```
Schema
  version      int                      // monotone; events cite it (INV:config-versioned)
  types        map[TypeName]TypeDef
  relations    map[RelationName]RelationDef

TypeDef
  children     []TypeName               // legal child types; empty = leaf
  ordering     sequence                 // rank among children is the dependency order
                                        // (sole variant today; named so a future
                                        //  'unordered' is a schema word, not a mode flag)
  workUnit     bool                     // scheduling boundary; edges may not reach inside
  lifecycle    Rollup{}                 // no own status; state is the fold of children's stages
             | Machine{states, initial, transitions}
  fields       map[FieldName]FieldDef
  show         TemplateRef              // per-surface template refs; §rendering

FieldDef
  kind         Text | Number | Date | Enum{values} | Pattern{regex}
  required     bool                     // sugar for a presence guard on the creating transition
  exposure     Prose | Structural       // encryptable vs workspace-public — declared at birth
                                        // (access-control forward constraint #1/#6)
  merge        MergeStrategy            // three-way merge behavior, declared not hand-coded

Machine
  states       map[StateName]StateDef   // StateDef: stage + whether a resolution attaches
  initial      StateName
  transitions  []Transition             // {from: []StateName, to: StateName, verb, requires: []Guard}

StateDef
  stage        todo | active | done     // the fixed trinity the machinery understands
  resolution   bool                     // may this state carry a resolution/redirect?

RelationDef
  semantics    Dependency | Reference   // scheduling-relevant vs annotation
  between      [](TypeName, TypeName)   // legal endpoint type pairs
  acyclic      bool                     // engine enforces for Dependency always
```

Load-bearing choices:

**Stages, not states, carry system meaning.** User state vocabularies are
arbitrary (`todo/doing/review/done`, `triage/accepted/shipped`); the machinery
never learns them. Every state declares a **stage** from a fixed trinity —
`todo | active | done` — and everything generic binds to stages: rollup folds
stages, the scheduler offers `todo`-stage work, readiness, staleness banners,
and any future Jira bridge map through stages. This is one-vocabulary-many-
instances: custom states are data; the engine's theorem is about stages. (Jira
proves the pattern at scale: its status *categories* are exactly this
trinity.) **OPEN:** whether the trinity needs a fourth member (`waiting`/
`parked`) for states that are neither workable nor active; resolve against
real profiles, not speculation.

**Retention is not in the schema.** Archive/delete/restore concern a ticket's
*existence*, not its workflow; they apply identically to every type in every
profile and remain the sealed universal axis (`internal/model/lifecycle/
retention.go` — already the codebase's one true transition table, and the
shape `Machine` generalizes).

**Guards unify validation.** A transition's `requires` list holds the same
predicate vocabulary field kinds use: presence, kind conformance, enum
membership, pattern match — over the ticket's own fields. `required: true` on
a field is sugar for a presence guard on the transition that creates the
ticket. One predicate language, two attachment points (field definitions,
transitions), one interpreter. This is why designing custom states *now* is
correct: the guard machinery states demand is strictly more general than a
required-fields list, and building the list first would build the wrong
smaller thing.

**Hierarchy legality is an allow-list, and it finally exists.** Today no code
prevents a task parenting a task, and parent cycles are caught only
defensively at read time (`internal/store/relations.go:437-472`,
`internal/store/ranking.go:203`). `children` gives `SetParent` a real check
and the engine a real cycle rejection — for every profile, including the
default.

**Rank's meaning splits by level, and the schema says so.** Among a work
unit's leaves, rank is *the* sequence — hard, complete, the reason sibling
dependency edges are illegal. Among work units, rank is preference order
*subject to* declared `depends` edges: rearrange the backlog freely, but A
finishes before B starts. The scheduler reads both; nothing else does.

## Two profiles

The shipped **default profile** — lit's discipline, as data (strawman syntax;
the format is settled in implementation design, but it must stay this legible):

```yaml
schema: 1

types:
  epic:
    children: [task]
    work-unit: true
    lifecycle: rollup
    fields:
      title:       {kind: text, required: true}
      description: {kind: text}

  task:
    lifecycle:
      initial: open
      states:
        open:        {stage: todo}
        in_progress: {stage: active}
        closed:      {stage: done, resolution: true}
      transitions:
        - {from: [open],              to: in_progress, verb: start}
        - {from: [in_progress],       to: closed,      verb: done}
        - {from: [open, in_progress], to: closed,      verb: close}
        - {from: [closed],            to: open,        verb: reopen}
    fields:
      title:       {kind: text, required: true}
      description: {kind: text}
      priority:    {kind: enum, values: ["0", "1"]}

relations:
  depends:  {semantics: dependency, between: [[epic, epic]], acyclic: true}
  relates:  {semantics: reference,  between: [[any, any]]}
```

Note what the default profile *changes*: `depends` between epics only —
leaf-level dependency edges are unrepresentable, per the trampoline. Today's
five leaf types (`task|feature|bug|chore`) collapsing to one is likewise a
profile decision to make deliberately, not an accident of the example.

The motivating **custom profile** — `feature → story → task`, custom fields
with validation, custom states, per-type show:

```yaml
schema: 1

types:
  feature:
    children: [story]
    lifecycle: rollup
    fields:
      title:          {kind: text, required: true}
      target-release: {kind: pattern, regex: '^\d+\.\d+$', required: true}
    show: feature.tmpl

  story:
    children: [task]
    work-unit: true            # the atomic unit is the story, not the feature
    lifecycle: rollup
    fields:
      title:  {kind: text, required: true}
      points: {kind: enum, values: ["1", "2", "3", "5", "8"]}
    show: story.tmpl

  task:
    lifecycle:
      initial: todo
      states:
        todo:   {stage: todo}
        doing:  {stage: active}
        review: {stage: active}
        done:   {stage: done}
      transitions:
        - {from: [todo],          to: doing,  verb: start}
        - {from: [doing],         to: review, verb: review, requires: [{present: description}]}
        - {from: [review],        to: done,   verb: done}
        - {from: [doing, review], to: todo,   verb: reset}
    fields:
      title:       {kind: text, required: true}
      description: {kind: text}
      due:         {kind: date}
    show: task.tmpl

relations:
  depends: {semantics: dependency, between: [[feature, feature], [story, story]], acyclic: true}
  relates: {semantics: reference,  between: [[any, any]]}
```

Everything the user asked for is a line of data: the hierarchy is `children`
chains, the validations are kinds and guards, the states are a machine, the
displays are template refs. Adding an `initiative` above `feature` touches no
code — the mirror test for the machinery.

## The validation engine

One pure package, no I/O, two layers, one enforcer:

- **Field layer — a parser, not a validator.** `Parse(raw, FieldDef) →
  FieldValue | error`, where `FieldValue` is a typed sum (text/number/date/
  enum member/pattern-stamped string). The output type is the proof; the
  store persists the canonical encoding and nothing downstream re-checks.
  Failure is structured (field, kind, offending input) — the CLI renders it,
  and an agent can act on it.
- **Structural layer — invariants over the graph.** Parent legality
  (`children`), parent acyclicity, relation endpoint legality and
  acyclicity (`between`, `acyclic`), work-unit boundary (no dependency edge
  reaches inside a unit), sibling-edge redundancy (illegal under `ordering:
  sequence`), transition legality (from-state × verb × guards). All are
  interpretations of schema data by fixed code.

It sits **above the store seam**, so the Dolt and memory engines both receive
already-proven values and the rules exist once — collapsing today's scatter
(~15 validation sites, duplicated per engine; see §displaced). Write-time it
refuses exactly as lit refuses today. Fold-time — once the event store lands
— the same engine judges each event against the config version the event
cites, and concurrent-offline contradictions resolve by deterministic rule
plus advisory, precisely as [event-store design](../event-store/design.md)
§validation already specifies. This engine must live inside the refold budget
(full refold from genesis < 1 s, [budgets](../event-store/budgets.md)) — a
further reason it is an interpreter over data, not pluggable user code.
User-supplied *executable* validation (scripts, hooks) is a **non-goal**:
it would break fold determinism, the refold budget, and the signed-config
trust model in one stroke.

## Scheduling discipline

What `next` means under a schema: descend the hierarchy from the top-ranked
work unit whose dependencies are all `done`-stage; within the unit, offer the
first leaf in rank order whose stage is `todo` and whose guards for its
start-verb transition pass. One consumer, one answer, no frontier
computation. Rollup states mean an exhausted unit disappears on its own — the
open-but-empty-epic confusion is structurally gone. Multi-agent later: the
same traversal, filtered by claims — a layer, not a rewrite.

## Rendering

Per-type templates enter through the funnel that already exists:

- `lit show`'s full-detail path is a single function
  (`printIssueDetail`, `internal/cli/output.go:78`); the template engine
  replaces its body. Go `text/template` — the binary's first template engine,
  introduced deliberately here and nowhere else.
- Template *resolution* reuses `internal/templates`' project > global >
  embedded layering (`templates.LoadWithSource`), so `--eject` and
  `quickstart --refresh` cover show-templates for free. The schema's
  `show:` ref names a file in that space; the default profile's templates
  reproduce today's output.
- Template *data* is the proven ticket: parsed field values plus the built-in
  accessors, derived from the schema — the field vocabulary that today is
  hand-relisted in seven places (§displaced) becomes one derivation.
- A broken user template must not brick `show`: fall back to the default
  renderer **with a loud warning** naming the file and the parse error.
  Silent fallback is forbidden; failing the read command outright punishes
  the wrong moment.
- `lit show --field` is untouchable — it is the machine contract that
  round-trips into `lit update` — and it extends naturally: custom field
  names join the vocabulary because the vocabulary is now schema-derived.
- The repo's printer idiom — dispatch on capability shape, never on type
  name (`output.go:84-87`) — is preserved, not reversed: the schema is what
  *produces* a ticket's capabilities (has-own-status vs rollup, which
  verbs, which fields); templates select by type, but code downstream still
  asks "what can this ticket do."

Agent-facing guidance follows the same route: the workflows subsystem
(`internal/workflows`) already layers user-authored markdown by labels,
states, and events; it gains a `types:` dimension so a profile can teach its
own vocabulary through machinery that already has precedence, dry-run, and
firing traces.

## What this displaces — the grounding inventory

The current code, mapped so the design's claims are checkable:

- **Type vocabulary:** sealed five-constant enum; `IsContainer() == (t ==
  TypeEpic)` is the entire hierarchy model (`internal/model/issue_type.go:56`)
  with ~25 branch sites. Becomes: schema `types` + capabilities derived from
  `TypeDef`. The enum's own comment says it is a function "so nothing can
  widen the set at runtime" — this design formally inverts that intent, and
  says so.
- **Parent/child legality:** none exists; no parent-cycle write check.
  Becomes: structural layer, first profile onward.
- **Fields:** fixed struct + one column each; the vocabulary hand-relisted in
  `output.go:183` (`--field`), `output.go:390` (columns),
  `storage/issues.go:131` (sort), `storage/bulk.go:19` (import),
  `merge/resolve.go:86` (three-way merge, one line per field),
  `store/store.go:2091` (positional SELECT), `shapemap_known.go:160`
  (recovery). Becomes: one schema-derived vocabulary; custom values persist
  in one narrow field-agnostic shape (the pattern `issue_event_changes`
  already proves), with per-field declared `merge` strategy replacing the
  hand-written merge lines.
- **Validation:** ~15 scattered sites duplicated across two engines
  (e.g. `store/store.go:978` ≡ `storage/memory/apply.go:219`). Becomes: the
  one engine above the seam; the conformance suite verifies engines against
  proven inputs instead of re-verifying rules.
- **Lifecycle:** sealed sums, no from-state legality anywhere
  (`status_states.go:148` — any status action is legal from any state);
  `Retain` (`retention.go:64`) is the one real transition table and the
  shape `Machine` generalizes; `AllOf` is `Rollup{}` before it had a name.
- **CHECK constraints generated from Go constants**
  (`schema_reconcile.go:95-107`): under a workspace schema these become
  schema-derived or dissolve into the engine — resolved with the storage
  substrate, not before.
- **Ranking's two-level frame** ("a child's stand-in is its epic,"
  `internal/store/ranking.go:212-251`), **lanes** as epic-scoped by
  definition (`model.go:212`), and **ID minting** (`parent.<n>`,
  `issue_ids.go:14`) all assume one containment level. §open carries them.

## Where the schema lives, and the road

**Now (design-time):** a project-level YAML file, human-authored, reviewed
like code. Config precedence machinery exists (`internal/config`), but the
schema is *not* machine-local config — it is workspace truth every machine
must agree on, so its interim home is with the store, its destination the
**signed config stream**: schema edits become signed config events with a
monotone version; every ticket event cites the version it was authored under;
the engine judges each event against its cited version. Existing data is
never invalidated by a schema change — it is *grandfathered by construction*,
and current-state violations surface as advisories, not corruption. This
design therefore also closes event-store **OPEN #5** (config bootstrap): the
first config version of a fresh workspace is the default profile, and
`lit init` writes it.

**Access-control interplay** (forward constraints honored): every type name
is a stable identifier capability selectors can reference ("can't delete
epics" is a type restriction — the selector's type dimension exists for
exactly this); every field declares `exposure` at birth, because a prose
field must be envelope-encryptable and a structural field is
workspace-public — a field used by guards, rollup, or scheduling must be
structural, and the schema makes that trade-off visible where the field is
declared. Transition verbs join the capability vocabulary as data:
policy names verbs the schema defines, rather than lit minting a Go verb per
command. **OPEN:** whether policy grants name individual verbs or the generic
`transition` capability with a state selector; decide when RBAC lands.

**Implementation sequencing is out of scope for this document** beyond one
commitment: the meta-model, the parse engine, and the template layer are pure
and substrate-independent — they are the same code over Dolt rows or a fold
cache — and nothing in this design may take a dependency that dies with Dolt
(migration state S4).

## OPEN questions

1. **Deep-hierarchy ranking and lanes.** Rank framing assumes one containment
   level; lanes are epic-scoped. Under `feature → story → task`: does ranking
   at each level compose (likely — rank is per-sibling-set), and what does a
   lane generalize to (a per-work-unit sub-grouping?)? Needs its own design
   pass with the rank machinery open.
2. **ID minting at depth.** `parent.<n>` nests legibly
   (`feat-x.2.3`), but reparenting and unit moves interact with ID identity.
   Position wanted before the storage design.
3. **Type identity vs display name.** Is the type name the stable ID policy
   references (rename = new type + migration), or do types carry a separate
   immutable ID? Leaning name-is-ID for legibility; revisit with RBAC.
4. **The stage trinity's fourth member** (`waiting`?) — see §meta-model.
5. **Cross-unit partial dependency.** "Only part of epic A must precede B"
   is inexpressible by design (the answer is: split A). Confirm this stance
   survives real use, since it is the load-bearing simplification.
6. **Verb collisions and CLI surface.** Custom verbs become `lit <verb>`
   subcommands or a generic `lit move <id> <state>`; collisions with built-in
   command names must fail at schema load, loudly. Decide with the CLI
   design.
