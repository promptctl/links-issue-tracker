# Custom fields and required fields, declared per level

Status: draft / proposal. Written 2026-10-27 for owner review. The ask, in the
owner's words: *"I have a user who wants to define a specific hierarchy and
that would include specific required fields that you would define on each
level of the hierarchy. The hierarchy part fits already. What we need is the
custom fields and required fields."* The hierarchy is the three-level model of
the hierarchy epic; this document is the fields half of the
[workspace-schema draft](workspace-schema/design.md), carried forward on that
model, with its storage home settled by
[workspace-configuration.md](workspace-configuration.md).

Revised 2026-10-28: the storage design follows the
[event-store design's §migration](event-store/design.md), and this document
was revised to match the decisions recorded there.

## What exists

A ticket's fields are a fixed set: title, description, prompt, plan, type,
topic, priority, assignee, labels, lane. They are one table in the storage
layer (`internal/storage/fields.go`), and that table is what gives each field
its history row, its bulk-import key, its `--field` name and its merge rule, so
a field cannot be written without being recorded. There is no way to add a
field without a Go change and a column.

"Required" exists as `ready.required_fields` in `.lit/config.toml`: a list of
built-in field names, read by the readiness classifier, which marks a ticket
with an empty one `blocked: missing <field>` so `lit next` skips it. It applies
to every ticket regardless of level, it cannot name a field that does not
exist, and an outside reader of the store cannot see it (the cross-repo gap
[workspace-configuration.md](workspace-configuration.md) closes).

## Declaring a field

A field is declared on a level, in the workspace configuration document, under
the `[levels]` table the hierarchy epic introduces:

```toml
[levels]
1 = "ticket"
2 = "epic"
3 = "initiative"

[levels.2.fields.target_release]
kind = "pattern"
pattern = '^\d+\.\d+$'
required = true

[levels.1.fields.points]
kind = "enum"
values = ["1", "2", "3", "5", "8"]

[levels.1.fields.description]
required = true
```

A field has a name, a kind, the kind's parameters, and a required flag. The
kinds are the workspace-schema draft's five: `text`, `number`, `date`, `enum`
with its values, `pattern` with its regular expression. A built-in field is
declared implicitly on every level with its built-in kind; naming one, as the
last block does, may set `required` and nothing else. That block is what
replaces `ready.required_fields`: the same meaning, now per level and now in
the store.

A field declared on level 2 exists on epics and not on tickets. A name is
unique within a level; the same name on two levels is two fields, and a
built-in name cannot be redeclared with a kind. The document's parse refuses a
declaration that breaks any of this, naming the key, before a version is
written.

## Storing a value

A field set is an event; the fold holds the current value. Field values add no
Dolt table and no migration, so they are built after reads come from the fold
(S2 in the event-store design's §migration). The value recorded is the
canonical encoding of the parsed value: a number as its decimal text, a date as
`YYYY-MM-DD`, an enum member and a pattern match as given. `lit history` shows
a custom field set under the field's declared name, `points: - → 5`, exactly as
it shows `priority: normal → urgent`. Prose-shaped custom fields are
deliberately not offered; description and prompt exist for prose.

The parse is one boundary. Raw text crosses it at `lit new`, `lit update`, and
`lit import`, and comes back as a typed value or a refusal that names the
field, the kind and the offending input, exit 3. The store persists only what
passed, and nothing downstream re-checks `[LAW:parse-dont-validate]`. The
definitions the parse reads are the live workspace configuration, so a store
read from outside its repository parses, renders and judges readiness with the
same definitions the repository does.

## Surfaces

`lit new` and `lit update` take `--field <name>=<value>`, repeatable. A name
not declared on the item's level is a usage error listing the names that are,
the way an unknown `--columns` name is refused today. `lit show` prints a
`fields:` block after the built-in header, one `name: value` line per declared
field that has a value, and `lit show --field <name>` returns a custom field's
value bare, so the round trip into `lit update` works for custom names as it
does for built-in ones. `lit ls --columns <name>` and `lit backlog --columns
<name>` project a custom field; the column registry derives its vocabulary from
the live definitions. `lit import` takes a `fields:` map per document. `lit
history` needs nothing new.

## Required

Required means required to be pulled, not required to be created. The owner's
grooming direction ([grooming.md](grooming.md)) is that work enters the
backlog and is made ready later, by a person or an agent putting an epic in
order; a ticket that cannot be filed until every field is filled fights that
flow and pushes authors toward placeholder values. So `lit new` accepts an item
with required fields empty, and the item is not ready-to-pull until they are
filled.

The rule has one home and bites in two places that both read it. The readiness
classifier's existing `missing_field` kind keeps blocking an item whose
required field is empty, now per level and from the store document, which is
what `lit backlog`, `lit next`, `lit ls --columns blocked`, the epic plan in
`lit show`, and `lit stores --counts` all read. And `lit label add <id>
ready-to-pull`, the grooming confirmation, refuses when a required field of
that item's level is empty, naming the fields, so an epic cannot be declared
groomed while a ticket inside it is missing what the level requires. The
second site exists so the refusal lands at the moment a person is looking,
with the names in front of them, rather than as a blocked row discovered by the
next agent.

A required field on level 2 bites when the epic itself is labeled
ready-to-pull: the epic's own required fields are checked at that moment, and
an epic missing one cannot carry the label. On level 3 the same holds for an
initiative.

## Changing a declaration later

Declarations are versioned with the document, so a field can be added,
required can be turned on or off, and a field can be removed. Removing one does
not delete values: values of an undeclared field stay in the fold and `lit
show` prints them under `fields (undeclared):`, so nothing a person typed
vanishes because a definition changed `[LAW:nothing-unseen]`. Changing a kind
leaves existing values as they are; a value that no longer parses under the new
kind is reported by `lit doctor` with the issue, the field and the value, and
is never rewritten. Both are advisories to a human, not repairs.

## Scope

In: fields and their kinds, required per level, storage, history, the parse
boundary, the surfaces above, the move of `ready.required_fields`. Out, and
staying in the workspace-schema draft for a later decision: custom states and
transitions, per-type `show` templates, relation legality per type pair. Gone,
superseded by the hierarchy epic's decision that roles are fixed in code:
user-defined types and the `children` allow-list.

## Observability

The command event records each custom field set and its kind, parse refusals
with the kind, and on a `ready-to-pull` label the required-field check's
outcome with the count of fields checked and the names found empty, zero
included.

## Sequence, as tickets

Every step below follows the read flip (S2); none is built before it.

1. The workspace configuration document
   ([workspace-configuration.md](workspace-configuration.md)); this and the
   hierarchy's `[levels]` are its first sections.
2. Declarations parse: the `[levels.<n>.fields]` grammar, kinds and
   refusals, as a pure package with no I/O.
3. Values: the field-set event and its fold rule, `--field` on
   new and update, `lit show`, `--field`, history.
4. Required: per-level, from the document, in the classifier and in the
   `ready-to-pull` refusal; `ready.required_fields` imported into the document
   and no longer read from the file.
5. Columns, import and bulk.
6. Undeclared values and kind changes in `lit show` and `lit doctor`.
7. Checkpoint, in vivo: a person declares a required pattern field on epics and
   an enum on tickets in a scratch repository, files an epic and tickets with
   and without them, sees `missing` in `lit backlog` and the refusal on `lit
   label add ... ready-to-pull`, fills the fields, labels the epic, pulls a
   ticket with `lit next`, then reads the same store from another directory
   with `lit ls --at` and `lit stores --counts` and gets the same ready and
   blocked counts the repository's own `lit backlog` shows.

## Decisions for the owner

1. Required means required to be pulled, checked at the `ready-to-pull` label,
   never at creation. Recommended, for the grooming flow above.
2. The five kinds and no sixth. A `user` or `issue` reference kind would want
   its own validation against the store and is left out until asked for.
