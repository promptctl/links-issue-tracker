# Event store: the charter

This document records the owner's directive for replacing lit's storage engine
with an event-sourced store, so the design can proceed across many sessions
without drifting from what was asked. It is the question;
[design.md](design.md) is the answer and [budgets.md](budgets.md) is the
numbers. Precedence: **charter over design, design over tickets, budgets.md
over any prose that repeats its figures.** When two disagree, the higher one
wins — amend it deliberately, never by reinterpretation.

## Why this exists

Three pressures point at the same architecture.

**Concurrency.** lit's unit of truth today is mutable state (Dolt table rows),
so every concurrent writer contends for the same thing, and the cost is the
machinery around that contention: one read-write engine per path, the
mirror-pending flock choreography, field-aware reconcile with linear-history
replay, an inline receive that holds the foreground because a background
worker would break it. Each piece is correct; together they are the tax on
choosing mutable shared state. The owner's direction: stop paying the tax —
make writes immutable appends and derive state deterministically, so
concurrency is opted out of by construction rather than managed by protocol.

**Access control.** The accepted access-control charter
([../access-control/charter.md](../access-control/charter.md)) demands signed
mutations verified by every client over a dumb replicated remote. That is an
event log wearing table clothes. Nearly every strain point in the
access-control draft — re-signing replayed commits, ciphertext merge
fingerprints, tier-scoped reconcile — is friction between RBAC and *reconcile*,
and the event store removes reconcile. Building the write layer against Dolt
first would build its hardest machinery twice.

**Licensing and weight.** The Dolt dependency tree (including the vendored
driver and its patch ledger) exits the picture. What remains must be
permissively licensed; prior art with non-permissive licenses (git-bug,
GPL-3.0) informs at the behavior level only and is never copied.

## Hard constraints

These are settled. Do not reopen them in the design.

1. **One repo, no server, git-native.** Ticket data lives in the code repo's
   own git object database and syncs as git refs to the same remote the code
   uses. The remote stays a dumb replicated store; all intelligence is
   client-side. (Inherits access-control charter constraints 1–2.)
2. **Writes are appends; state is a fold.** Every mutation is an immutable,
   content-addressed event. Current state is a deterministic pure function of
   the event set — same events, same backlog, on every machine, every time. No
   locks, no write engines, no merge/reconcile machinery. The only shared
   mutable state permitted outside the event log is a cache that any process
   may delete at any time.
3. **Every command under one second.** The perf target is <1s wall time for
   every lit command, met at 10x current fleet scale on both axes (10x history
   per store × 10x stores per machine), with the baselines and budgets recorded
   in [budgets.md](budgets.md). Network transfer is excluded only where
   physics demands (first-clone download); network never sits on a command's
   foreground path.
4. **Permissive licensing only.** No copyleft dependencies. Executing the
   system `git` binary is acceptable; linked libraries must be permissive.
   Where prior art is non-permissive, rewrite from behavior, never from code.
5. **Local enforcement stays; cross-machine conflicts resolve at the fold.**
   A machine validates its own writes against its own current fold exactly as
   strictly as today. Contradictions that only concurrent offline machines can
   produce are settled by deterministic fold rules plus a surfaced advisory to
   the next agent — never by blocking, never by a lock. Concurrent free-text
   rewrites remain the one class delegated to agent judgment, held as
   coexisting events until resolved.
6. **Birth requirements.** The event schema carries from its first shipped
   version: a signature slot and principal reference on every event; prose
   payloads opaque-envelope-capable; a versioned, signed configuration stream
   that events reference by version; and validation as a generic engine over
   that config, not hand-coded rules. These are primitives for the
   access-control layers and the user-defined hierarchy feature; retrofitting
   any of them is a fleet migration, so they ship in v1 even where v1 leaves
   them inert.
7. **The audit log is complete.** Signed events are never truncated,
   compacted away, or rewritten. Storage growth is monotone and bounded by
   budget, not by deletion. Erasure of content is cryptographic (key
   destruction), never physical.
8. **No heartbeats, no liveness probes.** The standing owner ban applies to
   this store as to all of lit, mechanism-wide, storage and locking included
   (design-docs/work-claims.md records the ban).
9. **The migration is a strangler fig with the old system as oracle.** The
   current Dolt store remains authoritative while the event store shadows it;
   every advance is gated by the differential oracle agreeing and by
   [budgets.md](budgets.md) passing at 10x; each superseded mechanism is
   deleted in the same campaign that replaces it. A store with two truths is a
   temporary state with a gate out, never a resting place.

## Calibration

120% of the primitives, 80% of the features: the primitive layer (event shape,
config stream, signature slots, fold engine) carries headroom beyond every
identified consumer; the feature layer covers the main body of uses and leaves
the tail unbuilt. For future lit features, do not predict them — document the
constraints this design imposes on them, so evolution is burdened knowingly or
not at all.

## The process

Decisions made while building land as dated `DECISION:` comments on the
relevant lit ticket (campaign-wide rulings on the epic); the docs absorb the
resolved position when the affected work closes. The docs themselves never
carry work items or schedules — every sentence in design.md must stay true
when any given ticket closes. Sections carry a status line
(`destination` / `built (vX.Y.Z)` / `superseded → §anchor`) so the unbuilt
frontier is greppable.

## Status

- 2026-08-25: charter written from the owner's directive, distilled from the
  design conversation that produced it; design.md and budgets.md first drafts
  alongside.
