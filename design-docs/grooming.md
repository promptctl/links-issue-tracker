# Grooming: the backlog is put in order, and ready-to-pull says it was

Status: draft / proposal. Written 2026-10-27 for owner review. The owner's
direction, in his words across two messages that day: *"when things go into
the backlog, they're not ready to pull yet. They have to be groomed and then
they're ready to pull."* *"Groomed should be a label and the label should be
ready to pull. `lit groom` should print guidance for grooming the ticket, or
the backlog, or whatever ... This would be like an event that users can override
or hook into. ... Lit is an orchestrator. The command doesn't have to actually
do anything except print guidance."* And on scope: *"you almost can't groom an
L1 unless it's completely independent. Grooming has a connotation of getting a
backlog in order."*

Revised 2026-10-28: the storage design follows the
[event-store design's §migration](event-store/design.md), and this document
was revised to match the decisions recorded there. Grooming adds no schema, so it
is built ahead of the event store, after the strip epic.

## What exists

A ticket is pullable the moment it is created. The readiness classifier
(`internal/cli/readiness.go`, the one interpreter of the annotation kinds)
knows six reasons to hold a ticket back: an open dependency, an inherited
dependency through its epic, an earlier sibling in its lane, a required field
empty, the `needs-design` label, the `external` label. Nothing records that
anyone has looked at a ticket and judged it ready, so a planning session that
files twelve tickets at midnight hands twelve pullable tickets to whatever
agent runs `lit next` at one.

The two reserved labels are the precedent this design follows. `needs-design`
and `external` are plain labels with one constant each in code; their presence
is a blocking fact, the classifier phrases it, every surface reads the
classifier. They are the house pattern for "a human's judgment, recorded as a
label, read by the gate".

## The label

`ready-to-pull` is a reserved label, the third constant beside the two above,
and it blocks by absence: a level-1 item is pullable only when it, or an
ancestor, carries the label. The classifier gains one kind, `ungroomed`,
blocking, phrased `ungroomed: no ready-to-pull on it or its epic`. Nothing else
in the gate changes, and every surface that reads the gate, `lit backlog`, `lit
next`, `lit ls --columns blocked`, the epic plan in `lit show`, and `lit stores
--counts` across repositories, reports the new reason without being taught it
`[LAW:single-enforcer]`.

The label is a label. It is added and removed with `lit label add` and `lit
label rm`, it syncs with the store, it is visible in every listing, and once the
hierarchy epic's label and relation history lands, its arrival and removal are in the trail with an
actor and a time. There is no groomed column, no groom event, no derived
predicate: the store already holds the fact in the shape users can see and
edit.

## Where the label goes: the queue unit

Grooming is done to an epic, or to the backlog, not to one ticket. The owner's
point is that a ticket's readiness is a property of its place: whether it is
the right size, whether its rank among its siblings is right, whether what it
depends on is ranked above it, whether the epic's description still says why
these tickets and not others. None of that is visible from inside one ticket.
So the label goes on the queue unit, the level-2 item, and every child inherits
it: grooming an epic ends with `lit label add <epic> ready-to-pull`, and its
twelve tickets become pullable in one act.

Inheritance down the tree is the rule the hierarchy epic already uses for a
dependency or a `focus` label on an initiative, so a label on a level-3 item
covers every epic beneath it by the same code path. Whether anyone labels an
initiative is left to use. A stray ticket, one in the Unfiled epic, carries its
own label, because the Unfiled epic is never groomed as a whole and never
carries the label itself; it would make every future stray pullable.

The consequence to be deliberate about: a ticket filed into a groomed epic is
pullable at once. That is the behavior the house workflow depends on. When
`lit next` says an epic is blocked and prints the `lit new --parent <epic>`
that would clear it, the agent files the ticket and `lit next` serves it on
the next run; a per-ticket label would put a grooming step between those two
commands for the one agent who understands the ticket best. The agent who files
a ticket it does not understand labels it `needs-design`, as today.

Two alternatives were weighed. A label on level-1 items only: twelve commands
to groom an epic of twelve, every follow-up re-groomed, and it contradicts the
connotation the owner named. A label on level 2 only: a stray can never be
groomed, and the Unfiled epic has to be special-cased. Inheritance covers both
with one rule.

Removing the label sends an epic back for grooming, after a re-scope, say, and
its tickets stop being served until it is put back. With label history, the
trail says who did both and when.

## `lit groom`: an event and nothing else

`lit groom` fires a workflow event and writes nothing. It takes an optional id:

- `lit groom` fires `groom_backlog`, an occasion with no issue id and no
  labels, exactly as `show_backlog` is built today.
- `lit groom <epic>` fires `groom_epic` with the epic's id and labels, as
  `show_ticket` is built.
- `lit groom <ticket>` resolves to the ticket's epic, prints one line saying
  so, and fires `groom_epic` for the epic. An initiative fires `groom_epic`
  for itself; its guidance walks its epics.

Both events join the catalog beside the eleven that exist. Dispatch, layering,
override by id, turning a piece off, `lit workflows dry-run`, firing traces:
all of it is the machinery that already serves every other event, so the new
code is one command, two catalog entries, and the shipped pieces below. A
project that wants its own definition of ready writes `.lit/workflows/
groom-epic.md` and the shipped text is gone; a user who wants one across every
repository writes the global layer's. This is what the owner means by
orchestrator: lit names the moment and prints what the project said to print
there.

## The shipped guidance

Three pieces, grouped as the `grooming` set once sets exist
([workflow-sets.md](workflow-sets.md)). Their bodies are written in the
ticket, not here; this is what each is for.

`groom-epic`, on `groom_epic`: read the epic's description and ask whether it
still says why this work and what finished looks like; walk the children in
rank order, and for each ask whether a stranger could start it, whether it is
one coherent push or two, whether what it waits on is ranked above it or
declared as a dependency, and whether its lane is right; fill the fields the
level requires, which `lit show <epic>` already marks `missing <field>` per
child; park what is not ready with `needs-design`; retire what will not be
done; then `lit label add <epic> ready-to-pull`, which refuses while a required
field is empty ([custom-fields.md](custom-fields.md)).

`groom-backlog`, on `groom_backlog`: `lit backlog` shows the ungroomed epics
with their tickets held; groom them top-down, highest rank first; an epic that
has sat ungroomed past the point anyone would pull it is a candidate to close.

`ticket-created-ungroomed`, on `ticket_created`: one sentence that a new ticket
is served only once its epic carries `ready-to-pull`, and that `lit show <id>`
says whether it does. The occasion carries only the new ticket's own labels,
not its epic's, so the piece cannot know whether the epic is groomed and is
worded to be true either way.

## What the surfaces say

`lit backlog` lists an ungroomed epic's tickets inline as blocked with the
reason, as it lists any blocked row, and its preamble counts ungroomed epics.
`lit next`, empty-handed because everything ready sits under ungroomed epics,
names them and prints `lit groom <epic>` as the way on, in the same message
that today names a blocker and the `lit new` that would clear it. `lit show
<epic>` prints `ready-to-pull: no` on the epic's own header when it lacks the
label, and the children's markers read `[blocked: ungroomed]`. `lit stores
--counts` counts them as blocked. Nothing is hidden anywhere a reader would
otherwise conclude the backlog is empty `[LAW:nothing-unseen]`.

## Upgrade

Every store has open epics nobody groomed under this rule. A one-time label
write, run by the first lit that knows the label, adds `ready-to-pull` to
every open level-2 item except the Unfiled epic, and to every open stray
inside it, each with the reason `grandfathered at upgrade` on its history event, so the first `lit next` after
upgrade serves what it served before and the record says why. This is one
honest write per epic rather than one per ticket.

## Cross-repository

The label is in the store, the rule is in the binary with no switch, and the
inheritance walks the parent edges the store holds. `lit ls --at` and `lit
stores --counts` give the same ready and blocked counts the repository's own
`lit backlog` gives, with no configuration to miss.

## Observability

The `lit groom` command event records the event fired, the id it resolved to
and from, and the pieces that fired, the way every dispatch site records
`workflows.fired` today. The `ready-to-pull` label write records the
required-field check's count and the names found empty. The grandfathering write
records how many epics and strays it labeled.

## Interactions

- Hierarchy (the hierarchy epic): inheritance down the tree, the
  Unfiled epic's special standing, and label history are all that epic's work;
  this design adds one label and one classifier kind on top of them.
- Custom fields ([custom-fields.md](custom-fields.md)): the `ready-to-pull`
  refusal is where required fields bite.
- Workflow sets ([workflow-sets.md](workflow-sets.md)): the three pieces are
  one set.
- The plan record epic: none; a plan is written after a ticket is pulled.
- `preparing-the-next-loop.md` names "backlog grooming as a first-class
  activity" as its third priority; this is that activity's hook.

## Sequence, as tickets

The sequence follows the strip epic, so it is built from the primitives the
strip produces.

1. The reserved label, the `ungroomed` kind with inheritance, every surface
   reading it, and the one-time grandfathering label write.
2. `lit groom` and the two events.
3. The three shipped pieces.
4. `lit next`'s empty-handed message names ungroomed epics and the command.
5. Checkpoint, in vivo: a person files an epic with three tickets in a scratch
   repository, sees `lit next` refuse with the epic named, runs `lit groom`
   with no id and with the epic's id and reads both texts, overrides
   `groom-epic` in the project layer and sees the override print, labels the
   epic and pulls a ticket, files a follow-up into the groomed epic and pulls
   it without grooming, removes the label and sees the epic held again, and
   reads the same store with `lit stores --counts` from outside.

## Decisions for the owner

1. The label on the epic with inheritance, not on each ticket. Recommended,
   for the reasons in "Where the label goes".
2. The label's name. `ready-to-pull` is the owner's phrase; `ready` is shorter
   for the agents who will type it.
3. `lit groom <ticket>` grooms the ticket's epic. The alternative is refusing
   with a pointer to the epic; both say the same thing, one saves a command.
