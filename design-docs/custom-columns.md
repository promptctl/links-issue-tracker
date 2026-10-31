# Custom columns: rules that place tickets, computed on every read

Status: draft / proposal. Written 2026-10-30 from a design conversation with
the owner, for owner review. The ask, in the owner's words: *"User should be
able to define their own custom columns (ie states). I want all states to be
identical and arbitrary, with the behavior controlled by configuration."*
This document carries forward the custom-states half of the
[workspace-schema draft](workspace-schema/design.md), which left it undecided,
and replaces that draft's state machine with a different model.

## The model in one paragraph

A column is a name in configuration and nothing else; no column means anything
to the code. Behavior comes from **rules**, and there is one kind: *when a
condition holds for a ticket in some column, produce an action.* The action
places the ticket in a column, shows a label on it, or prints guidance. A
ticket's column is not stored. Every read computes the board from the stored
facts, the configuration and the clock, `board = tick(state, config, now)`,
and writes nothing. The same facts read with the same clock give the same
board on every clone, so tickets settle where the facts put them. Clocks that
disagree can disagree on a time-based rule near its threshold; that is the
skew the event-store design already surfaces as an advisory.

## What exists

A ticket's status is one of `open`, `in_progress` and `closed`, a sealed sum in
`internal/model/lifecycle/status_states.go` that a database check constraint
repeats (`internal/store/schema_reconcile.go`). Inside `in_progress`, a phase
(`plan`, `impl`, `review`, `merge`) is read back as the newest event that set
one (`internal/model/phase.go`), so lit already shows a board with six
positions on two axes. Readiness, the orphaned annotation and lane claims are
computed on each read and never stored. The orphaned threshold is in
`internal/cli/ready_state.go`. That makes three precedents for computing
position instead of storing it.

## One rule type

The conversation began with three kinds of criteria per column, as the owner
put them: the criteria to enter a column, the criteria to proceed to the next,
and the criteria to stay, "the criteria that would cause us to be
rejected/demoted from the column." The owner then asked whether these are all
instances of a single "condition-results-in-action" type. They are. Entry,
exit and stay differ only in where a rule applies: entering a column, leaving
it, or sitting in it. A label and a move differ only in the action.

```toml
[[rule]]
when = "missing description or AC"
in = "backlog"
then = { place = "draft" }

[[rule]]
when = "blockers not closed"
entering = "doing"
then = { place = "backlog" }

[[rule]]
when = "no update in 72h"
in = "doing"
then = { place = "backlog" }

[[rule]]
when = "no design doc"
in = "design"
then = { label = "needs-design" }
```

The syntax is illustrative. The condition language is the predicate vocabulary
the workspace-schema draft and [custom-fields.md](custom-fields.md) already
define (presence, kind, enum, pattern), widened to read other tickets and
time.

### The worked example

Take a board of `draft → backlog → doing`, where backlog requires a name, a
description and acceptance criteria.

- **A ticket missing AC needs a home.** That home is draft, a first column with
  no rule, so every ticket has somewhere to sit.
- **Backlog needs no exit criterion of its own.** On a straight board, "leave
  backlog" and "enter doing" are one check on one move, so doing's entry rule
  is enough. An exit rule earns its place when a column has several forward
  destinations: "approval recorded" goes on review's exit once instead of being
  copied onto merge and deploy.
- **Staying matters only when it differs from entering.** A column with no
  stay rule keeps a ticket only while its entry rules still hold, and rules
  are re-checked on every read, so if someone deletes the AC, the ticket
  settles back into draft with no extra rule. A stay rule replaces that test,
  for the case where staying is easier than getting in. Doing's entry rule
  might be "all blockers closed", judged when the ticket entered, and its stay
  rule "updated in the last 72 hours". A reopened blocker then leaves the
  ticket in doing, while a ticket nobody touches falls back to backlog. That
  fallback is today's orphaned annotation turned into a move.

### A drag is a fact

The owner's question: *"Why shouldn't someone be able to drag a ticket into a
column and have it immediately say 'needs-design'? Maybe that's the workflow
they want."* Nothing should stop it. A column's unmet conditions are its to-do
list, not proof the column is misconfigured. A drag is recorded as an
ordinary event saying someone put the ticket in design, and rules read it like
any other fact. A column whose only rules are labels always accepts a drag.
A column whose rule places tickets elsewhere, as "no PR" places a ticket in
impl, sets the drag against that rule, and which one wins is decision 2
below.

## Tickets move on their own, and settle

The owner asked whether moving tickets automatically was a terrible idea. It is
not, as long as the move is computed and never written.

A written move is a stored reaction to a condition, and it outlives the
condition. Suppose clone A writes a demotion because a blocker reopened, and
clone B closes the blocker again before syncing. The demotion survives the
merge, but its reason is gone, and nothing undoes it. Written moves also
cascade: demoting A breaks a rule on B, which moves B, which affects A, and
each move is a new fact the next check reacts to.

A computed position has neither problem. In the owner's words: *"If
automatically moving the tickets WAS the default, everything would 'settle'
into its natural space based on the facts."* When the blocker closes again,
the ticket is back where it was on the next read. Arrival order does not
matter, and position needs no merge rule.

Two constraints keep the board settling:

1. **Rules never write facts.** People and agents write facts; rules compute
   where a ticket sits, which labels it shows and what guidance it prints. A
   label a rule shows is computed like a position and kept apart from the
   labels people write, so a rule cannot mint something other rules read as
   a fact.
2. **Rule reads must not loop.** A rule may read another ticket's computed
   position or label, as "blockers closed" reads where each blocker sits. If
   A's column depends on B's and B's on A's, there is no answer. Loading the
   configuration refuses rules that can bounce one ticket between columns,
   such as "in draft, place backlog" beside "in backlog, place draft". A loop
   through other tickets depends on the graph, not the configuration, since
   two tickets can block each other, so tick has to find it at read time;
   what the board shows then is decision 5.

## Tick runs inside every read, and nothing is saved

The owner's framing: reading the board calls `tick`. It is not a command anyone
runs, and its output is never saved. The owner asked what there would be to
save, and the answer is nothing. Position is a function of the facts, the
configuration and the clock, so a saved copy is a second source for something
already computable. A cache is allowed for speed, invalidated when facts
change, and is never the source of truth.

```
state = fold(events)               // deterministic, cacheable, same bytes everywhere
board = tick(state, config, now)   // rules evaluated at read time, clock passed in
```

This is the split the [event-store design's §fold](event-store/design.md)
already draws under "Time enters at the edges": the fold never reads the clock,
and a read-time evaluation takes the folded state and the reader's `now`.
Tick is that evaluation, given rules. Its time-based rules read each event's
wall-clock time (`INV:wall-time-is-data` in the same design).

Every command that shows a ticket calls tick, including the cross-repo readers,
so a reader of another repository's store computes the same board and writes
nothing into a store it only reads.

## What this displaces

- **Status and phase become one board.** The default configuration would
  reproduce today's behavior with columns `open`, `plan`, `impl`, `review`,
  `merge` and `closed`, and their rules, plus a column for an in-progress
  epic, which has no phase today (`internal/model/phase.go`). This is a
  proposal; the owner has not ruled on it.
- **The workspace-schema draft's `Machine`** (states, an initial state, and
  transitions with guards) gives way to rules over a computed position. Its
  guards survive as rule conditions.
- **Readiness and required fields** become rules. In
  [custom-fields.md](custom-fields.md), required means required to be pulled,
  checked at the `ready-to-pull` label; under this model that check is one
  rule among many.
- **Phase drift goes away.** [phases-and-rework.md](phases-and-rework.md)
  notes that agents let the phase go stale. If an approved plan places a ticket
  in impl, no move remains for anyone to forget, and rework is the derived
  state that document recommends.
- **Archive and delete stay outside.** They govern whether a ticket exists,
  not where it sits.

## What it costs

Position stops being a command. `lit move`, or a drag, records a fact that
rules read, and how far it can overrule them is decision 2. Every condition must be
something lit can see, so "PR merged" counts only once something records it as
an event. Rules that react to a move rather than a resting place need history.
"Blockers closed when it entered doing" requires finding when it entered,
which means replaying tick over past moments. That is deterministic, but it
costs more than evaluating the present. With positions computed, "why is this
ticket in backlog?" becomes the common question, so a `lit why <id>` that
prints each rule that placed the ticket and the facts it read is worth
building alongside.

## Timing

The [event-store design's §migration](event-store/design.md) says
schema-bearing features wait for S2, the read flip. The column configuration
lives in the workspace configuration document
([workspace-configuration.md](workspace-configuration.md)). So this lands at
S2 or later, beside custom fields.

## Decisions for the owner

1. **Do columns belong to named groups?** A dependency rule has to say what
   "finished" means for a blocker. Listing columns in every such rule copies
   one list into many places, and the copies drift. Naming the group once
   avoids that. The choice is between the workspace-schema draft's fixed trinity
   (`todo`, `active`, `done`) and groups the configuration names freely. The
   trinity keeps epic rollup and hiding finished work simple. Free groups are
   more flexible, but each of those behaviors then needs its own
   configuration.
2. **When a drag and the rules disagree, which wins?** One answer is that
   rules always win: a ticket dragged to review with no PR settles in impl,
   labeled with why, and a drag is evidence, never an override. The proposal
   is that the newest fact wins. A later drag overrides earlier evidence, and
   later evidence overrides an earlier drag, so position stays a function of
   the facts. "Newest" means later in the event store's total order, never
   the later wall-clock time, so a fast clock cannot win the comparison. A
   condition that is an absence, such as "no PR", has no event to compare,
   so under the proposal it needs a ruling of its own.
3. **Does tick apply the current configuration to every ticket?** Validation in
   the event-store design judges each event against the configuration it was
   written under. For the board, the expected answer is the current
   configuration, so editing a rule re-settles every ticket. That is a
   different choice from validation's and should be made deliberately.
4. **Do phases become columns?** Recommended yes, as above.
5. **How does tick resolve rules that compete?** Two place rules can match one
   ticket, a placement can break the destination column's own rules, and
   tickets can block each other. Recommended: the first matching rule in
   configuration order wins, placement repeats until nothing moves, and a
   ticket on a loop through other tickets is placed without reading the
   positions on the loop, labeled with the loop. Two implementations that choose
   differently give two boards from the same facts, so this is fixed before
   tick is built.
