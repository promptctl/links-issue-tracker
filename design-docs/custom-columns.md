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

A leaf ticket's status is one of `open`, `in_progress` and `closed`, a sealed
sum in `internal/model/lifecycle/status_states.go` that a database check
constraint repeats (`internal/store/schema_reconcile.go`). An epic stores no
status; its state derives from its children. Inside `in_progress`, a phase
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
entering = "backlog"
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
description and acceptance criteria. Each rule names the failure and where it
sends the ticket, so "backlog requires AC" is written as "missing AC, place
in draft".

- **A ticket missing AC needs a home.** That home is draft, a first column with
  no rule, so every ticket has somewhere to sit. A ticket sits in the first
  column in configuration order until a drag or a rule puts it elsewhere.
- **Backlog needs no exit criterion of its own.** On a straight board, "leave
  backlog" and "enter doing" are one check on one move, so doing's entry rule
  is enough. An exit rule earns its place when a column has several forward
  destinations: "approval recorded" goes on review's exit once instead of being
  copied onto merge and deploy.
- **Entry rules hold a ticket at the door until they pass.** A ticket dragged
  to doing while a blocker is open waits in backlog, and on the first read
  after the blocker closes it is in doing. Nothing has to drag it again. Once
  the ticket is in, the drag is spent: a stay rule that sends it out is not
  undone by the drag, and only a new drag brings it back. This example
  follows the first answer to decision 2.
- **Staying matters only when it differs from entering.** A column with no
  stay rule keeps a ticket only while its entry rules still hold, and rules
  are re-checked on every read, so if someone deletes the AC, backlog's entry
  rule settles the ticket back into draft with no extra rule. A stay rule
  that places tickets replaces that test once the ticket is in, for the case
  where staying is easier than getting in; a label rule never does. Doing's entry rule places a ticket in backlog while
  any blocker is open, and its stay rule places it there after 72 hours with
  no update. A reopened blocker then leaves the ticket in doing, while a
  ticket nobody touches falls back to backlog. That fallback is the signal
  today's orphaned annotation gives, turned into a move. The annotation's own
  test is stricter, six hours without an update and nobody holding the lane
  (`internal/cli/ready_state.go`), and a default rule would read both.

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
   labels people write, so another rule reads it as computed output, the
   way it reads a position, never as a fact.
2. **Rule reads must not loop.** A rule may read another ticket's computed
   position or label, as "blockers closed" reads where each blocker sits. If
   A's column depends on B's and B's on A's, there is no answer. Whether
   rules loop depends on which conditions can hold together and on the
   graph, since two tickets can block each other, so the configuration alone
   cannot settle it. Tick finds a loop at read time, whether it bounces one
   ticket between columns or runs through other tickets; what the board
   shows then is decision 5.

## Tick runs inside every read, and nothing is saved

The owner's framing: reading the board calls `tick`. It is not a command anyone
runs, and its output is never saved. The owner asked what there would be to
save, and the answer is nothing. Position is a function of the facts, the
configuration and the clock, so a saved copy is a second source for something
already computable. A cache is allowed for speed and is never the source of
truth. The fold's cache is invalidated when facts change; a cache of tick's
output also takes `now`, so it expires at the next moment a time-based rule
would give a different answer.

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
  `merge` and `closed`, and their rules. An epic has no status or phase of
  its own today (`internal/model/phase.go`), so its column comes from rules
  that read its children's columns, the dependency decisions 1 and 5 cover.
  This is a proposal; the owner has not ruled on it.
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
an event. Entry rules need history: whether a ticket is still at doing's door
or already in doing depends on whether its entry rules passed since the drag,
which means replaying tick over past moments. That is deterministic, but a
read pays it for every ticket shown and every ticket their rules read, so the
cache the previous section allows is what makes it affordable. With positions computed, "why is this
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
2. **When a drag and the rules disagree, which wins?** Recommended: rules
   win. A ticket dragged to review with no PR settles in impl, labeled with
   why, and a drag is evidence, never an override; the worked example
   assumes this. The alternative is that the newest fact wins, so a later
   drag overrides earlier evidence and later evidence overrides an earlier
   drag. It has two costs. A condition with no event behind it, an absence
   such as "no PR" or a lapse such as "no update in 72h", has nothing to
   compare. And "newest" would have to mean later in the event store's total
   order, which between concurrent events can rank a writer who worked
   offline for days ahead of today's evidence.
3. **Does tick apply the current configuration to every ticket?** Validation in
   the event-store design judges each event against the configuration it was
   written under. For the board, the expected answer is the current
   configuration, so editing a rule re-settles every ticket. Replay then
   recomputes when each ticket passed an entry rule too, so an edit can move
   tickets whose events never changed. That is a different choice from
   validation's and should be made deliberately.
4. **Do phases become columns?** Recommended yes, as above.
5. **How does tick resolve rules that compete?** Two place rules can match one
   ticket, a placement can break the destination column's own rules, and
   rules can loop. Recommended: the first matching rule in configuration
   order wins, and placement repeats until nothing moves. A ticket whose
   placement revisits a column within one read stops at the first column it
   reached, and a ticket on a loop through other tickets is placed without
   reading the positions on the loop; either way it is labeled with the
   loop. The history replay that entry rules need is part of the same
   question: which past moments it visits, and which `now` each one uses,
   given that recorded times are not monotonic across writers. Two
   implementations that choose differently give two boards from the same
   facts, so this is fixed before tick is built.
