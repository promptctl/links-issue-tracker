# Event store: the design

Status: draft, first pass. Answers [charter.md](charter.md); budget figures
live in [budgets.md](budgets.md) and prose here cites them without restating
them. Written 2026-08-25. Sections marked **OPEN** are decisions a later
session must close; everything else is a position this draft commits to.

Every system section carries a status line: `destination` (designed, not shipped),
`built (unreleased)` (merged to `master`, carried by no tag yet),
`built (vX.Y.Z)` (describes shipped reality as of that release), or
`superseded → §anchor`. A version appears here only once a tag actually carries
it: naming the release a state is *expected* to ship in makes this a second map
of a number [`CHANGELOG.md`](../../CHANGELOG.md) and the tags already own, and
it is wrong the moment the release is cut as a different version.
`grep 'status: destination'` lists the unbuilt frontier. Flipping a section's
status is part of closing the work that ships it, not a separate act of
documentation.

A section that describes several states — §migration is the only one — carries
its status per state in its own table, and keeps the section-level line at
`destination` until the last of them is built. The frontier grep is the reason:
a section holding four unshipped states must appear in it, and one holding a
shipped state must still say so somewhere a reader will look.

## The shape in one paragraph

Every lit mutation is an immutable, signed-slot, content-addressed **event**
appended to the writing checkout's own git ref inside the code repo's object
database. Nothing is ever modified or merged: state is a deterministic
**fold** over the union of all writers' events, computed locally, cached
disposably, and identical on every machine that holds the same events. Sync is
`git fetch`/`git push` of per-writer refs against the code repo's ordinary
remote — appending to a ref only you write contends with nothing, so
concurrency has nothing to manage. Remote bug-filing is the same write path
executed from a machine without a clone: mint events, push them to a fresh
ref. The apparatus the current store needs to manage shared mutable state —
one-writer engines, flock choreography, field-aware reconcile, inline receive
— has nothing to be about and does not exist here.

### Invariant registry

Tokens are stable; cite them from code comments, tickets, and reviews as
`[INV:token]`. Each is defined once, in its section.

| Token | One line |
|---|---|
| `INV:append-only` | events are written once and never modified, rewritten, or dropped |
| `INV:deterministic-fold` | same event set ⇒ byte-identical derived state on every machine; only a fold-version bump may change it |
| `INV:single-writer-ref` | each ref has exactly one writing checkout; all cross-writer coordination is read-side |
| `INV:preserve-unknown` | an event a reader cannot interpret is carried and surfaced, never discarded |
| `INV:no-foreground-network` | no lit command's foreground path waits on a network operation |
| `INV:disposable-cache` | derived local state may always be deleted and refolded; nothing behaves as if the cache were truth |
| `INV:opaque-envelope` | layers below the CLI boundary treat prose payloads as opaque bytes |
| `INV:config-versioned` | every event names the config version it was authored under; validation judges it against that version |

## §events — the event model
status: destination

An event is a small structured record: `(type, schema-version, issue-id,
payload, attribution, config-ref, causal-position, signature-slot)`.
Attribution is the (stream, workspace, principal) triple — the existing
work-claims pair plus the principal reference the access-control design
requires. `config-ref` names the config-stream version in force when the event
was authored (`INV:config-versioned`). `causal-position` is a Lamport
timestamp with the real Lamport advance rule, which is load-bearing and stated
here so no implementation ships the cheap half: a writer mints its next event
at `1 + max(its own last counter, the highest causal-position among all events
it has folded)`. Observing an event therefore always places your next event
after it — an intent minted *because* you saw something supersedes that
something on every machine. A plain per-writer sequence number would not have
this property, and the failure it produces is §fold's no-crime-scene kind: a
deliberate override sorting before the event it overrides, discarded
identically everywhere. Within a command's batched commit the counter still
advances **per event** — a command minting k events occupies k strictly
increasing positions — so the total order never leans on payload iteration
order. Combined with the writer id as tiebreak, the pair gives the fold a
total order without wall clocks. The signature slot is
structural from v1: before the access-control
write layer ships, events are self-signed by their checkout identity; the slot
exists so that shipping verification later changes policy, not schema.

Events carry **intent, not derived state** — the fold owns derivation, and an
event that embeds a derived value forks truth into two homes:

- WRONG: `{"type":"rank.set","issue":"x1","position":4}` — position 4 of
  *what list*? The value is a fold output smuggled into an input; two
  concurrent events like it cannot both be true, and the fold is forced to
  arbitrate nonsense.
- RIGHT: `{"type":"rank.place","issue":"x1","above":"y2"}` — an intent that
  stays meaningful whatever else happened concurrently.

**Issue identity.** Ids are minted with no coordination, so they must be
collision-proof by construction: an issue id derives from the creating event's
`(writer-id, causal-position, content)` — opaque, and impossible for two
offline checkouts to collide on. The current store's child scheme
(`epic.<maxChild+1>` computed from the local view — see
`newChildIssueID` in `internal/store/issue_ids.go`) is a guaranteed collision
under concurrent filing and does not carry over as identity. Sequential child
numbering survives as **fold-derived presentation**: the fold assigns display
ordinals deterministically, so two children filed offline under one epic are
two distinct issues that both survive and both get a number. Ordinals are
deterministic at any event set but **not stable across time** — a concurrent
sibling syncing in late can sort earlier in the total order and renumber its
peers — so `.n` is never a durable reference; the opaque id is what agents
store. This section is the minting mechanism's **home** for every id,
top-level and child alike; the access-control design's "required lit
redesigns" §1 owns the id *policy* (opacity, format, what stays visible) and
defers the mechanism here.

**Schema versioning.** Events are immortal, so every shape ever shipped stays
readable forever. Readers hold a chain of pure upcasters (v1→v2→…) applied at
fold time; bytes on disk never change (`INV:append-only`). Know what an
upcaster is before writing one: a frozen historical artifact, kept and tested
forever — the same class of object as `schema_reconcile.go`, whose deletion
once bricked every pre-v1 workspace (the P000 incident). The temptation will
arrive as hygiene: *"this upcaster handles a version nothing produces anymore
— dead code."* It is not dead; its producers are merely finished. Somewhere a
store still holds their events, and deleting the upcaster deletes that store's
ability to read its own history.

**Unknown events** (`INV:preserve-unknown`). A reader meeting an event type or
version it cannot interpret carries it byte-for-byte and surfaces one warning
naming the version gap. It never drops, never guesses, and never treats
failure-to-read as absence:

- WRONG: skip the unknown event and fold the rest — the fold now silently
  disagrees with every up-to-date machine, and the divergence has no bad data
  anywhere to find.
- RIGHT: fold what is readable, report "3 events from a newer lit (min reader
  vX) not applied," and refuse mutations that would race the unread events'
  issues.

A `min-reader-version` stamp in the config stream lets a workspace gate
too-old writers entirely, so a fleet is never half-blind for long.

**Codec — OPEN.** The framing must let the fold meet the refold budget in
[budgets.md](budgets.md) (a compact self-describing binary encoding is the
lean; the harness decides). The codec choice is invisible above the event
package boundary.

## §store — git as the substrate
status: destination

Events live in the code repo's git object database under
`$(git rev-parse --git-common-dir)`, the same shared location the Dolt store
occupies today, so all worktrees of a clone see one store. Each checkout
appends to exactly one ref — `refs/lit/log/<checkout-id>` — and no checkout
ever writes another's ref (`INV:single-writer-ref`). The checkout-id is a
**full-entropy token** (128 bits, minted once into the checkout's private git
dir — the same slot the work-claims identity occupies); the short forms
existing surfaces render are display aliases, never identity. Everything
downstream leans on this uniqueness — ref names, the fold's writer-id
tiebreak, the id-minting salt — and at full entropy it holds even between
never-connected workspaces that later meet on one remote. A lit command batches all
its events into **one** commit (one object walk, one ref update, atomic per
command); the commit's first parent is the writer's previous head, so each
writer's ref is fast-forward-only and linear in its **first-parent** history —
the one exception to single-parent commits is the sweep anchor below, which
adds a second parent, so tooling walks first-parent for a writer's own chain.

The store has **no eraser**. A wrong event — bad status, mangled title,
mistaken close — is corrected by appending the correcting event, exactly as a
ledger corrects an entry. The temptation is git fluency itself: *"I'll just
rewrite my ref and drop the bad commit, it's my ref."* Refuse it. A rewritten
ref breaks every peer's fast-forward fetch, voids any signature chain over the
sequence, and teaches the codebase that history is negotiable. There is one
eraser in the whole design and it is cryptographic (§security-and-config).

**Ref hygiene.** Writer refs are per-checkout, and checkouts die; the store
prunes refs whose checkout the existing dead-checkout detection (work-claims
liveness leg) proves deleted — pruning trims the *namespace*, never the
events. The sweep needs no shared ref and no exception to
`INV:single-writer-ref`: the sweeping checkout appends a sweep event to **its
own** ref, with the dead ref's head as an additional git parent, so the dead
writer's objects stay reachable from a live chain; the dead ref is then
deleted — locally *and* on the remote, via a ref-deletion push — with a
compare-and-swap against the anchored head. "Write," in the invariant's
sense, means appending commits to a ref, and no checkout ever does that to
another's; deleting a proven-dead, already-anchored ref is namespace
maintenance, not authorship. Fetch cannot resurrect a *stale* swept ref, and
the anchor is what discriminates stale from alive: a reappearing ref whose
head is at or behind its folded sweep anchor is the old corpse and is
re-deleted; a ref whose head carries commits **past** the anchor proves its
writer lives, and the receive keeps it and folds the new events. A checkout
whose own ref is absent — swept locally under a shared common dir, or deleted
on the remote — never roots a fresh chain: on its next mutation it looks up
its latest sweep anchor in the fold and chains onto that anchored head, so
its new commits are past the anchor **by construction** and pass the
liveness test. So a false-positive death costs nothing durable: an idle
falsely-swept checkout stays swept (harmless — its events are anchored)
until its next mutation, which resumes from the anchor and durably restores
the ref. Two survivors racing to sweep the same
corpse is harmless: sweep events are idempotent facts the fold deduplicates,
and each CAS delete has at most one winner — the loser no-ops.
Loose objects from event bursts are packed on the same maintenance occasions
git already runs; the budgets file carries the ceiling on what lit may add to
the code repo's own `git status`/`git fetch` times, because the first place
this design may not degrade is the developer's ordinary git experience.

**Remote filing** is the ordinary write path from a storeless machine: mint
events, commit them parentless, push to a fresh uniquely-named ref under
`refs/lit/inbox/`. Any established clone's receive folds inbox refs in and
anchors them. No special case: an inbox ref is a writer ref whose writer
happened to have nothing.

## §fold — one function owns derivation
status: destination

`fold(events) → state`. Events are ordered by `(causal-position, writer-id)` —
a total order every machine computes identically — and applied by pure
per-type rules. Two properties are the whole contract
(`INV:deterministic-fold`):

1. **Total**: every event in the readable set is applied or explicitly
   surfaced (`INV:preserve-unknown`); there is no quiet skip.
2. **Deterministic**: no clock reads, no map-iteration order, no filesystem
   or environment input. Same events, same bytes out.

The stakes are worth stating plainly: a nondeterministic fold is the one bug
in this design with no crime scene. Two machines hold identical events,
compute different backlogs, and every individual read looks healthy — the
divergence surfaces weeks later as two agents confidently working two
realities. Determinism is enforced, not hoped: golden-fixture tests pin
`fold(corpus)` byte-for-byte, and the **fold version** — an explicit constant
— bumps with any rule change, invalidating snapshots (§cache) so no machine
mixes two folds' outputs.

**Validation is a fold-time engine, not scattered rules.** The config stream
(§security-and-config) defines what a valid ticket is — today the built-in
invariants (epic/leaf rules, id uniqueness), later the user-defined hierarchy
and required-field templates. The engine evaluates each event against the
config version it names (`INV:config-versioned`). Write-time, a machine
validates against its own current fold and refuses exactly as lit refuses
today, so single-machine behavior is unchanged. Fold-time, a contradiction
that only concurrent offline machines can produce (two individually-legal,
jointly-contradictory structures) resolves by the deterministic rule for that
invariant plus a surfaced advisory — the same philosophy the field-aware
reconcile applies today, relocated into the one function that owns state.

**Concurrent prose rewrites** — title, description, agent prompt changed to
different text on both sides — remain the one class no rule settles. The fold
holds both events, renders the field as conflicted with both texts
addressable, and the resolution is simply another event from whoever judges
the merge (under access control, a keyholder). Nothing blocks; the store never
has a "diverged" state, only a field with two live candidates.

## §rank — order as intents
status: destination

Rank is the most contended object in the system — one shared total order the
whole fleet reorders constantly — and the hottest fold path (see budgets).
Rank events are **relative intents**: `place X above Y`, `place X at
top/bottom of frame F`. The fold applies intents in the global total order,
and supersession is scoped honestly to what the order can promise. For
**causally ordered** intents — the writer had folded the earlier intent when
it minted the later one — the later strictly supersedes, guaranteed by the
Lamport advance rule in §events; that is the "the later decision stands" a
human expects, and it covers every intent made in view of the state it
changes. For **genuinely concurrent** same-issue intents there is no "later":
the shared total order settles them as deterministic arbitration, and the
design claims convergence there, not recency. Concurrent intents **commute
only when their subject-and-anchor sets are disjoint**: `place X above Y`
concurrent with `place Y above X` has different subjects but contradictory
content, and it too resolves by the total order — for this reason the fold
may not be partitioned or parallelized by subject issue, which would break
`INV:deterministic-fold` on exactly the anchor-overlap case.

An intent survives interleaving in a way a position never can: "above Y" stays
above Y however the list shuffled around them, where a stored position 4 is a
lie the moment anyone else moves. (This is §events' WRONG/RIGHT pair operating
at scale — rank is why that rule exists.)

**OPEN:** frame semantics under reparenting (an intent naming a frame that a
concurrent event dissolved), and the compaction rule that keeps the intent
replay incremental from a snapshot rather than from genesis. The budget in
budgets.md is the constraint any answer must meet.

## §cache — scaffolding, never structure
status: destination

Reads come from a local materialized state under the common dir, keyed by
`(fold-version, set-of-ref-heads)`, rebuilt incrementally by folding only
events newer than its snapshot. It is **scaffolding, never structure**
(`INV:disposable-cache`): any process may delete it at any moment and the only
cost is one refold. Correctness never depends on it; every command's staleness
check reads the writer refs' heads (cheap, local) and folds the delta.

This is where concurrency tries to reenter the design. Two worktree processes
will race on the cache file, and the temptation will be reflexive: *"I'll add
a small flock around the cache write."* Stop — that lock is the first cell of
the disease this design exists to cure. The cache needs no winner: writes are
atomic-rename, a loser's work is discarded, a corrupt or torn cache fails its
checksum and is deleted and refolded. Losing a race costs a refold; adding a
lock costs the design.

"Premature optimization" does not apply to snapshots, and the objection should
be met on its own turf: the proverb correctly bans speculative machinery for
unmeasured problems. The refold ceiling in budgets.md is not speculative — it
descends directly from the charter's hard constraint 3, and a fold that only
meets it warm is a fold that misses it on every fold-version bump. Snapshots
are load-bearing from the first shipped version.

## §sync — refs out, refs in, nothing in front
status: destination

**The foreground never waits on a wire** (`INV:no-foreground-network`). Push
mirrors and receive fetches run in background processes; a command reads and
writes local objects only. The temptation is always modest: *"one quick fetch
inline here — it's fast and the data's fresher."* That exact reasoning, made
policy, is how `lit backlog` came to overshoot the per-command directive by an
order of magnitude under the current store (budgets.md's pre-#414 baseline
row; fixed only by PR #414 after the fact). The current architecture had an
excuse: embedded Dolt permits one read-write engine, so background sync would
break foreground commands, and inline was the least-bad seat for the fetch.
This design removes the excuse — appends and folds cannot conflict with a
background fetch — so nothing earns a seat on the foreground path again.

**Out:** a background mirror pushes the checkout's own ref after mutations
(cadence config carries over from the current design). Pushing a ref only you
write cannot conflict (`INV:single-writer-ref`); the remote is only ever asked
to fast-forward.

**In:** a background receive fetches peers' refs on a debounce. Arrival needs
no merge — new events simply exist locally, and the next command's staleness
check folds them (`INV:deterministic-fold` makes arrival order irrelevant).
The current design's degradation surfaces carry over with less to say: there
is no "diverged" state to report, only "N events not yet pushed" and "peers
last fetched T ago," plus the owner-notification hook for push failures.

**Adoption** (first clone of an existing workspace): fetch `refs/lit/log/*`
**and** `refs/lit/inbox/*` — inbox events fold exactly as an ordinary receive
would fold them, so a workspace-founding adopter sees remotely-filed bugs
with no established-clone prerequisite — verify what the active policy
requires (§security-and-config), fold. Two
workspaces independently initialized against one remote simply union — the
destructive take/combine choice the current store forces does not arise from
histories (there is no shared spine to diverge). Identity cannot collide
(§events derives ids from writer and position), so what a union surfaces is
*semantic* duplication — two issues describing one bug — held as a fold
advisory for an agent to settle by closing one. **OPEN:** whether adoption may trust a signed snapshot to
defer full-history verification, and what that concedes — the
verification-cost budget in budgets.md forces this decision.

## §security-and-config — the slots the future stands on
status: destination

This section binds the charter's birth requirements to their mechanism; the
access-control design ([../access-control/design.md](../access-control/design.md))
remains the normative home for the layers themselves.

- **Signature slot.** Every event carries one (§events). Pre-RBAC it holds the
  checkout's self-signature; the write layer later upgrades verification
  policy without touching stored bytes. The replayed-commit re-signing problem
  the access-control draft holds OPEN dissolves here: nothing is ever
  replayed, so every signature is original forever.
- **Envelopes.** Prose payload fields are defined as opaque byte strings end
  to end (`INV:opaque-envelope`); plaintext is an encoding those bytes happen
  to use before the read layer ships. The fold, cache, codec, and sync never
  inspect prose, so tiered encryption changes what the CLI boundary does, not
  what the store does.
- **Config stream.** Workspace configuration — policy document, hierarchy and
  required-field templates, min-reader-version, invariant parameters — is
  itself an event stream with a monotone version, signed like any events.
  Threshold operations (the owner-rotation quorum the access-control design
  requires) are **quorum-by-events**, not multi-signature events: the change
  is a proposal event, each co-signer independently appends an approval event
  citing the proposal's hash, and the new config version activates at fold
  only when **both** legs of the access-control rotation rule are satisfied:
  the prior policy's threshold of outgoing-owner approvals *and* a threshold
  of the proposed new owner set — each incoming owner attesting with the very
  key being enrolled, so no one is rotated in without their own key's
  consent. Per-writer refs make this the
  only natural shape — co-signers cannot share a commit — and it is why one
  signature slot per event remains sufficient as a birth requirement.
  Events cite the version they were authored under (`INV:config-versioned`);
  the validation engine judges each event against its cited version, so two
  machines straddling a config change still fold identically. One seam here
  is deliberately OPEN rather than hand-waved: cited-version judgment alone
  would let a revoked principal keep minting forever under the last
  permissive version it saw — the revocation bypass the access-control
  enforcement section exists to prevent. Any fix must stay deterministic (a
  rule keyed to what a receiver happens to know would fold differently on
  different machines); see OPEN #6.
- **Erasure is cryptographic.** An append-only replicated store has no
  physical delete (`INV:append-only`, restated where it bites: even an owner
  cannot recall pushed plaintext). Content that may ever need destroying is
  ciphertext whose key can be shredded — which moves the read layer's
  encrypted-by-default posture from good hygiene to the load-bearing erasure
  mechanism. The access-control design owns the key machinery.

YAGNI deserves its answer here, because every slot above will attract it:
*"nothing verifies signatures yet — ship without the slot."* YAGNI is right
about features: unused surface is carrying cost, and the 80% feature line in
the charter exists for exactly that reason. These are not features; they are
the primitive layer's headroom (the 120% side of the same rule), and their
absence is not a smaller system but a fleet-wide schema migration standing
between the system and its own roadmap. The slots ship inert; the features
wait their turn.

## §migration — five states, four gates
status: destination

The store advances through five architectural states. Each gate is a
condition, not a date; a state whose gate hasn't passed is where the system
rests. The strangler-fig discipline: the Dolt store is the oracle until the
gates prove the fold against it, on real fleet data, at 10x.

| State | The system is | Gate to advance | Status |
|---|---|---|---|
| S0 seam | CLI depends on a storage interface; Dolt implements it, and a second engine proves it is one | interface carved; behavior unchanged | built (v0.9.0) |
| S1 shadow | every mutation dual-writes; differential oracle diffs fold vs Dolt after each command | oracle diff empty over sustained real use; budgets pass at 10x | destination |
| S2 read-flip | reads serve from the fold behind a flag; Dolt still authoritative for writes | flag default-on with no regressions; oracle still clean; budgets re-green at 10x | destination |
| S3 write-flip | events are truth; sync is git refs; Dolt shadows as rollback | rollback window expires quiet; budgets re-green at 10x | destination |
| S4 exit | Dolt, the vendored driver, reconcile, engine-serialization, mirror-flock machinery are **deleted**; the oracle retires with Dolt | deletions merged; budgets green as CI regression checks; docs' statuses flipped | destination |

This table's Status column is load-bearing beyond this document: README.md and
docs/architecture.md carry user-facing claims that Dolt is lit's storage —
true today, falsified at S3 — and `internal/docsclaims` registers each one
against this table. Flipping a state's status fails that gate test until those
docs are rewritten, so the doc-update duty rides the same flip that closes the
shipping work.

S0 shipped as four things, and the shape matters to every state after it.
`internal/storage` is the contract, stated in the model's vocabulary with no
engine in any signature. Beside it sit seven **capabilities** — sync,
reconcile, checkpoints, repair, schema migration, import, and raw test access —
each an interface an engine may decline, asked for by a discovery call that
returns the interface or a typed refusal naming the engine. The split rule is
that two operations share a capability only when no engine could plausibly
offer one without the other, which is why reconcile is not part of sync: this
design's §sync says arrival needs no merge, so the event store will offer sync
and decline reconcile, and it must be able to say so rather than stub a
reconciliation it did not perform. What each state after S0 attaches to is
therefore specific: S1's dual-write is a decorator installed at the engine
factory, the one place a store is opened; the oracle diffs through the
contract's whole-state export, which is core rather than a capability precisely
so that two engines are comparable; and S4 deletes the capabilities the event
store never offered.

The **conformance suite** is what makes the contract a boundary rather than a
description of Dolt. An interface pins shapes, and shapes are not behavior; the
suite is a set of statements about behavior, parameterized over "give me a
fresh engine," and an engine that satisfies the interface but fails the suite
does not implement the contract. Both engines run it. Its second job starts at
S1: the suite is where a behavior the two engines disagree about gets settled
in writing, before the oracle meets the disagreement and reports it as
divergence.

Two such disagreements were settled at S0, and both are settled AGAINST the
better answer on purpose, because an engine that is right where the other is
arbitrary still reads as divergence. A listing's order ends in the issue id as
its last key, always, so no tie is left to an engine's incidental row order.
History is ordered by creation time and then by event id — not a happens-before
claim, since ids are random and a coarse clock ties routinely, but a rule both
engines can obey identically. Ordering that genuinely carries happens-before
arrives with §events' Lamport positions, which supersedes this rule for both
engines at once rather than refining it.

S1 is also where history backfills: the existing backlog replays into events
via export, with a provenance marker so pre-migration history is attributed
honestly rather than re-authored.

Gate scope, stated once: the budgets gate binds from S1 onward — at S0 no
event store exists to measure, so S0's gate is behavioral identity alone.

The oracle's comparison ends at the write flip: once S3 flips writes, Dolt
receives none, so there is nothing left to diff — S2's gate is deliberately
the last to carry an oracle clause, and S3→S4 is judged on rollback-quiet
plus budgets alone.

The dangerous state is the comfortable one. S1–S2 *work* — everything green,
both stores humming — and the temptation will be to live there: *"the shadow's
been clean for weeks; flipping is risk with no feature."* A dual-truth store
is not a conservative resting point; it is two systems' carrying cost plus a
divergence risk neither system has alone, and every week in it teaches new
code to straddle both. The gates are the pace: pass a gate, advance; fail a
gate, fix what failed. Parking is the one move the charter forbids, and S4's
deletions are part of the campaign's definition of done, not a someday.

## Rejected alternatives

Recorded so they are not re-proposed from scratch; each was weighed in the
2026-08-25 design conversation.

- **CAS retry loop on one shared spine** (single data branch, fetch-rebase-
  push): safe but serializes all writers through retry contention, keeps a
  merge/replay step alive, and re-imports the remote's availability into the
  write path. Per-writer refs need no winner at all.
- **In-place remote mutation of the Dolt store**: requires partial chunk
  fetch the git transport can't promise, and a full lit+Dolt peer wherever a
  bug is filed. The inbox ref subsumes the use case.
- **Existence-hiding visibility** (per-ticket secrecy of *presence*): breaks
  rank, claims, and epic derivation wholesale; the access-control design's
  visible-skeleton rule is adopted unchanged.
- **Compaction that truncates history**: violates the audit-completeness
  constraint (charter #7) and the signature chain; snapshots accelerate, they
  never replace.
- **Heartbeat/liveness signals for writer refs**: banned mechanism-wide by
  standing owner directive; ref hygiene keys off the dead-checkout evidence
  the claims design already derives.

## OPEN questions

1. Event codec (§events) — decided by the harness against the refold budget.
2. Rank frame semantics under concurrent reparenting, and the incremental
   intent-replay scheme (§rank).
3. Adoption verification: full-history verify vs signed-snapshot trust
   trade (§sync); budgets.md's verification ceiling forces the choice.
4. Snapshot sharing: purely local, or a shared snapshot ref to accelerate
   adoption — and what trust that implies (couples to 3).
5. Config-stream bootstrap: how the first config version is established in a
   fresh workspace and how the current `.lit/config.toml` maps in.
6. Config-version currency at enforcement time — the revocation-bypass seam
   (§security-and-config): a deterministic rule that rejects a revoked
   principal's post-revocation events. Candidate shape: validity judged
   against the config version in force at the event's causal position, plus
   per-ref causal-position monotonicity so positions cannot be backdated.
   Owned jointly with the access-control write layer.
