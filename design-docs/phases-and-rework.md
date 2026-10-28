# Phases, and whether rework is one

Status: draft / proposal. Written 2026-10-27 for owner review. The owner's
words: *"I think we also need a rework phase. and/or decide whether it even
exists."* This document lays out what a rework phase would be, what it would
buy, what it would cost, and recommends one of three shapes.

Revised 2026-10-28: the storage design follows the
[event-store design's §migration](event-store/design.md), and this document
was revised to match the decisions recorded there. Rework adds no schema, so it
is built ahead of the event store, after the strip epic.

## What exists

An in-progress ticket is in one of four phases: `plan`, `impl`, `review`,
`merge`. A phase is a state inside `in_progress`, never stored: `lit start`
puts a ticket in `plan`, `lit phase <id> --to <phase>` records a move as an
event, and the phase is read back as the newest of those events
(`internal/model/phase.go`). Any phase may follow any other, and the code says
so in as many words: *"review back to impl after findings is routine work, not
an error."* A phase a newer lit wrote under a name this lit does not know is
read back as that name, so an older lit never errors on a phase it does not know.

Three things read the phase. Workflow pieces bind to a phase being entered or
exited, and that is how the shipped `phase-impl`, `phase-review` and
`phase-merge` pieces say which command records the next move. The statusline
file carries `working_on.phase` so a prompt can show where the work stands. And
`lit show` prints it on the ticket's `phase:` line.

## What rework would mean

Work comes back from review. A code review posted findings, a check failed, a
reviewer asked for a change, and the author is now editing code again, not for
the first time. That moment is distinct from a plan-review fail, which keeps
the ticket in `plan` while the plan is revised and is governed by the plan
record epic, and from reopening a closed ticket, which is a status move. Rework
is the second and later passes through implementation.

## What a rework phase would buy

**Guidance at the moment.** The advice after findings differs from the advice
at first implementation: address every finding or say on the thread why not,
do not widen the change, re-request review when done, record a decision the
reviewer should see. A workflow piece could carry that. Today it cannot bind
to the moment. State activations within one piece combine with OR
(`internal/workflows/match.go`), and the occasion carries what was entered and
exited but not the pair as a transition, so `states: [impl, {name: review,
when: exit}]` fires on every entry to impl and every exit from review, not on
the one move from review to impl.

**A named fact for reports.** Time spent in rework against time in first
implementation is a quality signal, the kind of over-time question
the observability epic exists to answer. The trail already holds the fact,
an `impl` entry whose predecessor was `review`, but nothing names it, so every
report would re-derive it.

**A statusline that tells the fifth pass from the first.** A reader sees
`impl` and cannot tell whether this is the first implementation or the fourth
return from review.

## What it would cost

A fifth word to record. The phase is "only as current as the last `lit phase`
you ran", as the shipped guidance says, and agents already let it go stale;
another move to remember at a moment when the agent is reading findings is
another chance to drift. And the move is informationally redundant: review
exited, impl entered, says rework already. A stored word that duplicates a
derivable fact is a second source for one truth, and the two will disagree
the first time an agent runs `lit phase --to impl` out of habit.

## Three shapes

**A. A fifth phase.** `rework` joins the four; the author moves to it with `lit
phase <id> --to rework`; pieces bind `states: [rework]`; the statusline shows
it. Older lits read it without error, since an unknown phase name is
read back as written. Costs the fifth word and the drift above.

**B. A derived state.** The phase stays `impl`. The phase reader also reports
whether this `impl` was entered from `review`, and three consumers read that:
the phase occasion handed to workflows enters `rework` alongside `impl` when
the move came from review, so a piece binds `states: [rework]` with no new
recorded word; `lit show` and the statusline print `impl (rework)`; a report
counts time in `impl (rework)` apart from `impl`. No new command, no new
vocabulary to record, and the fact is read off the trail that already holds it
`[LAW:one-source-of-truth]`. The derivation generalizes if wanted: `plan`
entered from `review` or `impl` is rework of the plan, by the same rule.

**C. Nothing.** The `impl` entry piece carries both the first-pass and the
after-findings advice, and reports derive the fact when they need it.

## Recommendation

B. It gives the guidance hook, the display and the report the same fact from
the trail, and asks nothing new of the agent at the moment it is least likely
to comply. A is right if the owner wants rework to be a word an agent says
deliberately, a declaration rather than a reading; its compatibility is clean.
C leaves the guidance hook missing, which is the half of the ask that cannot be
derived later.

Whichever shape, the `phases` set in [workflow-sets.md](workflow-sets.md)
gains the rework piece, and its body is written in that ticket.

## Decisions for the owner

1. A, B or C.
2. Under B, whether the derived state also names rework of the plan, or only
   of the implementation.
