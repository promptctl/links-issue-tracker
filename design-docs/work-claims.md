# Work Claims: coordinating parallel checkouts over one shared backlog

Status: accepted design (validated in design dialogue, 2026-08-14). Written
tense-independent — it reads as a specification before implementation and as a
record of the architecture and its reasoning after. If behavior and this
document disagree and no superseding design exists, the divergence is a bug in
one of them, not an automatic win for either.

## Summary

One shared backlog serves many parallel lines of work: several worktrees on one
machine, several clones across machines, several people. Selection (`lit next`)
answers "what should be worked now" — but with only global state to consult, it
gives every checkout the *same* answer, so parallel streams collide on a single
cursor and users fight over rank to steer it.

This design introduces the **claim**: a lane of work belongs, temporarily and
advisorily, to the **checkout** that most recently worked it. Claims are
**purely derived** from work records already in the shared database. They are
never stored, never synchronized as their own state, and never cleaned up —
there are no claim objects, no claim commands, and no claim lifecycle. The
entire state footprint is: work events carry an opaque attribution, and each
checkout keeps one identity token in its private git directory. Who holds
what, what has gone stale, what is contested, and where `next` should route are
all computed at read time from facts the system already synchronizes.

Claiming is implicit — starting a ticket in a lane claims the lane. Release is
implicit — finishing the lane, going stale, or deleting the checkout releases
it. Focus and unfocus are not modes: a checkout with live claims is focused on
them; a checkout without claims sees the global backlog. The global case is the
natural zero state, not a second layer.

## The problem

The backlog is one shared, synchronized truth with one total order: a
hierarchical composite rank in which an epic's position carries all of its
children. That is a strength — every machine sees the same backlog — but it
makes "the top of the backlog" a single shared slot, and `next` a single global
cursor. Two symptoms follow:

- **Parallel checkouts pull the wrong work.** A second worktree is opened to
  run a different epic in parallel. Any fresh agent session there follows the
  standard workflow and runs `next` — which serves the *project's* next ticket,
  i.e. the other worktree's epic. Every new session must be re-briefed by hand,
  and an unbriefed one silently works the wrong stream.
- **Users fight over rank.** Two people each rank their own tickets "to top."
  Rank moves are single-row fractional-index writes, and sync merges them
  cell-wise without loss — both writes survive. The fight is not a merge bug
  and no representation change can fix it: two "before everything" intents are
  mutually exclusive *by meaning*. Each user reached for the only lever that
  steers `next` — project-level priority — to express something that was never
  a priority statement at all: "I am working on these now."

The general question underneath both: **how do distributed checkouts of one
repository coordinate work such that the shared backlog stays meaningful?**
Any answer that lives only in local state fails the second symptom — other
machines must be able to see that work is taken. Any answer that mutates
shared priority fails the first — that is the current behavior.

## The shaping principle: preference is local, commitment is shared

An earlier draft of this design gave each worktree a locally-stored "focus" — 
a persistent per-checkout filter over the backlog. It treated stream selection
as a *preference*, and preferences are private, so the state went local. The
draft failed the distributed requirement: a teammate's clone cannot see local
state, so their `next` would still serve work another stream was deep into.

The correction inverts the premise. What a working checkout holds is not a
preference but a **commitment** — "this line of work is being executed here,
and it will be worked until it is done." A commitment is information every
other stream needs, so it must travel through the one channel all checkouts
share: the synchronized database. What remains genuinely local is only
*identity* — a checkout must recognize which commitments are its own.

Everything in the design follows from placing each fact where it is
authoritative: work records in the shared database, checkout identity in the
checkout, physical addresses on the machine that owns them, and the claim
itself nowhere — it is a reading, not a record.

## The model

### Vocabulary

- **Checkout** — one working directory of the repository: the primary clone or
  any linked worktree. The durable unit of a work stream. Sessions, agents,
  and humans come and go inside a checkout; the checkout persists. Informally:
  the *worktree is the identity*; the *workstream* (an epic or lane) is what
  it currently carries; the claim is the derived binding between the two.
- **Lane** — the declared unit of serialization inside an epic: tickets in one
  lane are sequential, tickets in different lanes may proceed in parallel. An
  epic that declares no lanes is one lane. A parentless ticket is its own
  lane. The lane is the claimable unit.
- **Evidence** — work records in the shared database attributed to a checkout:
  which tickets it started or completed, and when it last touched them.
- **Claim** — the derived relation "lane L is held by checkout S," recomputed
  from evidence on every read.
- **Freshness window (T)** — the configured age beyond which evidence no
  longer supports a claim.

### The claim predicate

A lane **L** is claimed by checkout **S** exactly when all of the following
hold:

1. **L is unfinished** — open or in-progress tickets remain in it.
2. **S produced the latest establishing event on L.** Only ticket lifecycle
   transitions establish or transfer a claim: starting a ticket
   (open → in progress) and completing one. Grooming edits, comments,
   ranking, and label changes never establish a claim — a drive-by comment
   from another checkout must not capture an epic.
3. **The claim is fresh.** Once established, *any* mutation by S on L's
   tickets refreshes it — ordinary working commentary keeps a claim alive
   through a long stretch on a single ticket. A claim whose last refresh is
   older than T no longer holds.
4. **S is live, as far as this machine can tell.** See the liveness prune
   below; machines that cannot check assume liveness and rely on freshness.

Derived annotations accompany the predicate:

- **Contested** — a second checkout also has live evidence on L (an offline
  race, or a takeover the previous holder has not yet seen). Contest is an
  annotation, not a state: routing stays deterministic (the latest
  establishing event holds the claim), both sides are notified the next time
  they look, and sync reconciliation surfaces it for judgment.
- **Stale** — the holder's evidence has aged past T while L remains
  unfinished. The lane is unclaimed again, and selection may offer it as a
  takeover with provenance rather than serving it silently.

A claim dissolves by the predicate ceasing to hold: the lane finishes, the
evidence ages out, or the holder's checkout is locally known to be gone.
Nothing is stored, so nothing is released, transferred, or cleaned up.

### Identity and attribution

Each checkout lazily mints a **stream id** — a short opaque token — on its
first mutation, and stores it in the checkout's private git directory (the
same per-worktree private area where git keeps HEAD; git guarantees one per
worktree and deletes it with the worktree). The token is write-once local
state and is never synchronized as configuration.

Work events carry an append-only attribution pair: **(stream id, workspace
id)** — the checkout's token plus the already-existing per-store workspace
identifier. Both are opaque. Attribution is historical fact and is never
rewritten.

Sessions, agent identities, and user names play no role in claims. Many
sessions in one checkout are one claimant; a new session inherits its
checkout's claims with no re-briefing, which is precisely the behavior the
design exists to produce.

**Local liveness prune** (the `git worktree prune` pattern): when deriving
claims, a machine enumerates its own live worktrees and their stream ids.
Evidence whose workspace id matches this store but whose stream id matches no
live local worktree comes from a checkout that no longer exists — it is
treated as void, immediately, on this machine. Remote machines cannot see
this filesystem, so for them the same claim ends by aging out. The asymmetry
is deliberate and honest: deletion is a local fact, and only its owner can
observe it instantly. A different clone on the same machine has a different
workspace id and is never pruned by this one.

### Granularity: why the lane

The claim unit is the lane because the lane is already the system's declared
unit of serialization, and "what must be sequential" and "what one stream
holds" are the same fact — a serial line of work in two hands is a merge
conflict in waiting.

- A conventional single-lane epic claims wholesale: start one ticket, hold
  the epic. This is the common case and the degenerate one — no lanes
  declared, one lane claimed.
- A deliberately multi-lane epic is claimable lane by lane. A catch-all epic
  (independent bugs, say) is never monopolized because one bug was started.
  Several checkouts may legitimately share one epic across lanes.
- A cross-epic dependency pulled onto a stream's path produces evidence in
  *that ticket's own lane* — working a dependency never swallows the
  neighboring epic.

## Routing

Selection consults claims in a fixed precedence:

1. **The checkout's own live claims come first**: ready tickets within claimed
   lanes, in backlog order, including the prerequisite closure — a dependency
   outside the claimed lane that gates it is on the path and is offered.
2. **Exhaustion is loud and diagnostic**, never silent: "5/9 done, 2 blocked
   on E.4 (unclaimed, on your path — start it?)". Completing the last ticket
   announces the epic's completion; the claim has dissolved by predicate, and
   the checkout is global again. Unfocus is not an action.
3. **Then the global pool**: the top-ranked ready ticket in unclaimed lanes,
   labeled as what it is — "starting B.1 claims B#1" — so the act of
   commitment is visible at the moment it happens.
4. **Lanes claimed elsewhere are routed around, not hidden.** `next` skips
   them silently; listings show everything with claim annotations.
   Visibility is not pullability.
5. **Stale claims surface as an option, never a default.** "A#1: claimed by
   7f3a, idle 3d, nothing completed — available for takeover." Taking over is
   the ordinary primitive — starting a ticket in the lane — explicitly
   targeted, never reached by bare `next`. Overriding a claim that is still
   fresh requires explicit confirmation. A claim is a well-founded default,
   never a lock.
6. **Contested lanes** keep deterministic routing (latest establishing event
   holds), while the other party's selection stands down and says why:
   "claim on A#1 moved to 7f3a at 14:02 — coordinate or stand down."

Priority composes predictably: urgency reorders *within* eligible sets, and
claims define eligibility. An urgent ticket landing in a claimed lane hoists
for its claimant, not for everyone.

## Release and abandonment

A claim ends three ways, none of them a command:

1. **Completion** — the lane finishes; the predicate no longer holds anywhere.
2. **Local prune** — the checkout is deleted; its machine voids the claim
   instantly, everyone else ages it out.
3. **Age-out** — the safety net for silent abandonment anywhere: evidence
   older than T stops supporting the claim.

There are deliberately no release verbs. Agents do not announce that they are
stopping — they just stop — so an explicit release would be ceremony that the
common case never performs, and the design refuses to depend on it. (A
ticket-lifecycle "stop"/put-it-back verb was considered and deferred: it would
give deliberate, even distributed, release for free if the lifecycle ever
wants it for its own reasons. One honest residual: a checkout whose latest
act was *completing* a ticket, then walking away mid-epic, has nothing to put
back; that claim waits out T or dies with the worktree.)

Abandonment needs no bookkeeping of its own because **the gradations of an
abandoned claim are the evidence itself**, already shared:

- *Claimed, nothing completed, gone stale* — the weakest claim; surfaced as
  freely takeover-eligible, with provenance.
- *Claimed with unmerged work in flight* — deliberately outside the tracker's
  knowledge: branches and pull requests belong to git and the forge. The
  takeover flow instructs the **taking agent** to check for unmerged work
  before proceeding. Judgment stays with the caller closest to the context.
- *Claimed, half the lane completed, then abandoned* — completed tickets are
  completed in the shared database; the claim carried no work-state of its
  own, so takeover inherits a half-done lane with nothing to transfer and
  nothing lost.

## Finding a claimant

Interrogation has two tiers, and only one requires locality:

- **The dossier travels with the data.** The evidence *is* the shared
  database: which tickets the claimant started and finished, when it last
  acted. Any machine renders "claimed: stream 7f3a (elsewhere) · 2h ago ·
  pgct.11 in progress, 5/9 done" — most of what "who has this and how is it
  going" means, available everywhere.
- **The address is resolved where it exists.** For claimants that are live
  worktrees of this store on this machine, the liveness enumeration also
  yields a path and branch at render time: "claimed here: ../links-wt-pgct
  (links-sync-pgct.11)". A human or an agent can walk over — open the
  worktree, read its log, inspect the in-progress ticket. No interrogation
  protocol; a resolvable address. Remote claimants render as the opaque
  discriminator plus the dossier — on their end, it's just claimed — and the
  human coordination handle for remote conversation is the work's own name,
  as it would be anyway: "who's got pgct?"

## Distribution, races, and failure modes

Claims ride the existing sync channel because their inputs are ordinary work
records; there is no side channel to drift and no claim-specific merge logic.
With an eager (on-change) sync cadence, claim propagation is near-real-time
when online.

Two partitioned checkouts can start work in the same lane; nothing can
prevent that without locks, and the design refuses locks — under partition an
advisory system must choose availability. The race resolves into the
**contested** annotation at reconciliation: both parties' evidence lands,
routing follows the latest establishing event, both sides get told, and the
sync report surfaces the lane for judgment — consistent with the system's
standing philosophy that sync conflicts are resolved by the agent's judgment,
not by silent policy. The worst case under partition is *visible duplicate
effort*, never blocked work — the correct failure mode for coordination
metadata.

Cold start is graceful by construction: historical events carry no
attribution, so a freshly upgraded repository derives zero claims and behaves
exactly as before until newly attributed work exists.

## The privacy invariant

**Only opaque discriminators enter the shared database.** No hostnames, no
usernames, no directory names, no paths, no device details — the database
syncs to shared remotes and this archive itself is public. Resolution from
discriminator to physical context (path, branch, machine) happens only at
render time, only on the machine that owns that context. Nothing needs to be
redacted because nothing identifying is ever copied away from its owner.

This is the locality principle finishing its own thought: each fact lives
where it is authoritative and therefore travels nowhere. A standing flag
follows from adopting the invariant: any attribution that predates it —
user-name-shaped actor fields, for example — deserves the same review.

An **opt-in, user-chosen stream label** (chosen, never harvested) remains
open as a future nicety for teams that want friendlier remote rendering than
an opaque token.

## State inventory

- **Shared, synchronized**: work records, now carrying the append-only
  attribution pair (stream id, workspace id). Nothing else is added.
- **Local, per checkout**: one write-once stream-id token in the checkout's
  private git directory.
- **Derived at read time**: claims, freshness, staleness, contest, liveness,
  routing, and every rendering of "who has what."

There is no shared mutable state between worktrees outside the database — the
database is the one concurrency-safe shared medium, and this design adds no
second one. There are no claim tables, no registries, no lockfiles, no
sidecar maps.

## Relationship to adjacent mechanisms

- **Ticket assignment and session identity** (see
  [agent-identity-and-ownership.md](agent-identity-and-ownership.md)): that
  model holds that the unit of *ticket execution* is the agent session. This
  design does not revise it — it adds a second, coarser level: the session
  executes a ticket; the **checkout carries a workstream**. Both are true at
  once, and claims are derived from checkout attribution precisely because
  sessions are too ephemeral to carry a multi-session line of work. Claims do
  **not** resurrect that document's deferred process-liveness phase: there
  are no heartbeats and no liveness probes here, only staleness heuristics —
  the same family as orphan detection, applied one level up (lane instead of
  ticket, T instead of the orphan threshold).
- **The focus label** — the shared label that hoists a goal's prerequisite
  chain for everyone — is orthogonal and unchanged: it expresses *project*
  intent ("this goal matters most"), a statement to all streams. A claim
  expresses *stream* commitment ("this lane is being executed here").
- **The continue bias** on selection (prefer leaves under in-progress epics)
  is subsumed: claim routing is that bias made correct, automatic, and
  per-checkout.
- **Rank** remains the shared default agenda — what a zero-context checkout
  should do — and ranking-to-top returns to being a genuine project-priority
  statement, because "I'm working this" no longer needs it. Companion
  decision, accepted with this design: **new tickets default to
  bottom-of-frame placement** rather than top, so filing work stops being an
  implicit claim on the project's attention.
- **Lanes** gain a second consumer: the same declaration that orders work
  within an epic now scopes possession of it. One vocabulary for both facts,
  by design.

## Alternatives considered and rejected

- **Per-checkout persistent "focus" state** (the previous draft of this
  design): treats stream selection as local preference; invisible to other
  clones, so it cannot make the shared backlog meaningful under distribution.
  Superseded by the commitment inversion.
- **An environment variable selecting the focused epic**: does not survive
  into fresh sessions without external plumbing, and is invisible state; at
  most an override layer, never a foundation.
- **Epic order as local-only state** (shared backlog genuinely unordered
  across epics): deletes real shared knowledge — projects do have a priority
  — and makes every machine's backlog display permanently divergent,
  undermining the one-consistent-view property that is the tool's core
  strength.
- **Local rank overlays** (a per-machine reordering consulted before shared
  rank): two truths about order, with every view obliged to disclose which
  one it is showing; claims deliver the needed 90% with a binary,
  self-explaining concept.
- **Written claim rows**: explicit claim state created on start and retired
  on completion or expiry. Buys pre-claiming and instant explicit release at
  the cost of a lifecycle, cleanup paths, and a second representation of
  who-works-what that can drift from the work records. Rejected for the
  derived form, which cannot drift from evidence because it *is* a reading
  of evidence.
- **Epic-granularity claims**: one started bug in a catch-all epic would
  monopolize twenty unrelated ones. The lane is the honest unit.
- **Session-bound claims**: every new session would start unclaimed — the
  original failure mode, rebuilt.
- **Stream identity derived from the claimed epic's name**: names the holder
  after the held, which destroys exactly the distinction the system exists
  to draw — two streams erroneously in one lane would be indistinguishable,
  and contested-claim detection collapses. Identity that changes with its
  referent is not identity.
- **Hostname / path / directory-name attribution** for friendly display:
  leaks device details into a shared, potentially public database; replaced
  by opaque discriminators plus render-time local resolution, which lost no
  functionality.
- **A shared local registry file** (per-repo JSON map of streams): shared
  mutable state between worktrees outside the database — reinventing,
  without the database's locking and merge machinery, exactly the problems
  the database exists to solve.
- **Locks / enforcement / heartbeats**: a claim that blocks others is wrong
  under partition (two offline clones cannot consult each other's locks) and
  wrong by philosophy — the system surfaces and advises; agents and humans
  decide. This boundary was settled before this design and is preserved by
  it.
- **Intent-replay (CRDT-style) rank merging**: heavier merge machinery
  cannot help, because two "mine first" intents are semantically exclusive —
  no merge honors both. The fix was never in the merge; it was in scoping
  the intent.

## Parameters and open edges

- **Freshness window T**: default 24 hours, per-repository configurable.
  Repositories where humans idle over weekends may prefer ~72h; agent-heavy
  repositories may tighten it. Revisit with usage.
- **Takeover confirmation shape** for fresh-claim override (interactive
  confirm vs. explicit flag for non-interactive agents): decide at
  implementation; the invariant is that overriding fresh evidence is always
  a deliberate act.
- **Opt-in stream labels** for remote display: open, low priority.
- **The residual release gap** (completed-then-walked-away mid-epic): lives
  with age-out and prune; if it turns out to bite in practice, the deferred
  lifecycle "stop" verb is the natural door.

## Alignment with project intent

The project's bias is toward less: additions must earn their place against a
largely feature-complete tool. Claims add one derived concept and one local
token; no new shared state, no new commands for the common path, and no
ceremony — the mechanism is invisible exactly when there is nothing to
coordinate (a solo checkout behaves as before, plus stickiness to its own
epic). It serves the intent's autonomous default directly: a fresh agent
session in any checkout pulls the right work with zero re-briefing, because
the checkout — not the session, not a human's memory — carries the
commitment.
