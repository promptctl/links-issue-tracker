# Access control: the charter

This document records the owner's directive for designing lit's access-control
system, so the design can proceed across multiple sessions without drifting from
what was actually asked. It is the question; [design.md](design.md) is the
answer. When the two disagree, this document wins — amend it deliberately, never
by reinterpretation.

## Why this exists

lit's current security model is none. The Dolt store syncs through
`refs/dolt/data` on the code repo's git remote, so anyone who can read the repo
can pull the entire ticket history, and anyone who can push can rewrite or
delete it. That is a major barrier to real adoption: people are not always
comfortable having every ticket public even when the repo itself is public, and
tickets routinely carry things the code doesn't — internal implementation
details of machines and networks, in the owner's own current backlog. Users
will want visibility control. We don't choose what users want.

Confidentiality is also the *lower* bar. The bar the design must actually meet
is real RBAC — the owner's example: "I'm a contributor and I can create tickets
but I can't delete epics or any tickets I didn't create." Basic stuff like
that.

## Hard constraints

These are settled. Do not reopen them in the design.

1. **One repo.** A separate repository for ticket data is not acceptable in any
   way. Tickets travel with the code repo, full stop.
2. **No server.** The git remote stays a dumb replicated store. All enforcement
   is client-side: cryptographic (signatures verified by every client,
   encryption for visibility) — the Monero model, where validity is enforced by
   every verifier instead of any trusted party. *(Amendment, 2026-08-25: an
   optional external witness that cosigns observed history for split-view
   detection is compatible with this constraint, provided it enforces nothing
   and nothing requires it. Enforcement never moves off the client.)*
3. **Transparent to the user.** Monero users don't need to know any
   cryptography to use it; neither may lit users. Key handling, signing, and
   verification happen without the user studying the system. This is a hard
   requirement, not a UX aspiration.
4. **Rough edges resolve toward lit, not away from the RBAC system.** Anything
   that would require an overly complicated implementation or data model in the
   access-control layer requires a redesign of lit first — never a weakening of
   the access-control architecture. The RBAC design's quality is the fixed
   point; lit's model is the movable one.
5. **Tried and true patterns only.** Reference existing systems wherever
   possible; do not reinvent the wheel — not for the crypto, not for the role
   vocabulary, not for the UX.

## Calibration

Be comprehensive but don't overdesign. Aim the design at **120% of the
primitives we need and 80% of the features** — the primitive layer should have
headroom beyond every identified use case, while the feature layer covers the
main body of them and leaves the tail unbuilt.

For future lit features: don't predict them. Understand and document the
**constraints and boundaries** any design or implementation imposes, so the
design never destroys our ability to update lit going forward.

## The process

In order, each step feeding the next:

1. **Competitor research.** What GitHub, Jira, Rally, and the rest actually
   offer for permissions and visibility — the role vocabularies, the
   granularity floors, the defaults.
2. **lit feature survey.** Walk lit's full feature set so the design covers
   what actually exists, not a sketch of it.
3. **Forward constraints.** For each design choice, what it forecloses or
   burdens in future lit development.
4. **Customer needs**, separated into the sections below — which also carry
   the capability model (the bar from "Why this exists") and step 3's
   forward-constraints output, per the amendment noted there.

## Required sections of the answer

**Foundational hard requirements.** Compliance demands from heavily regulated
industries (evaluated for the RBAC design itself — ignore whether lit today
would pass); the standard of security and privacy every user gets by default;
and the split between protections that CANNOT be disabled versus options that
can.

**Data model.** What granularity things can be encrypted at — e.g., can a
single comment be encrypted, or is "comments" one encrypted field? Bias toward
simplicity and defensibility. (Constraint 4 applies with force here: a
granularity that complicates the data model is a reason to redesign lit's data
model, not to coarsen the security model.)

**User-facing shape.** Broad strokes only, low effort for now: what this looks
like to use, what burdens it places on the user, and what risks it introduces —
data loss presumably chief among them. The transparency constraint (3) governs
everything in this section.

**Capability model** *(amendment, 2026-08-25 — the sections above are the
owner's dictated structure; this and the next were added so the checklist
covers the charter's own stated bar and process)*: the answer must specify
the RBAC vocabulary itself — the verbs, roles, grant targets, and how
"creator owns their ticket" binds — not merely the compliance, granularity,
and UX sections around it.

**Forward constraints** *(same amendment)*: process step 3's output is a
required section — the boundaries and burdens the design imposes on future
lit development.

## Status

- 2026-08-25: charter written from the owner's directive; research and the
  first draft of [design.md](design.md) in progress.
- 2026-08-25 (amendment): Required sections extended with **Capability
  model** and **Forward constraints**, so the checklist covers the charter's
  own stated bar and process step 3; the owner's three dictated sections are
  unchanged.
- 2026-08-25 (amendment): constraint 2 explicitly admits optional
  detection-only witness cosigning — never enforcement, never required —
  so the design's deferred witness primitive rests on a recorded amendment
  rather than reinterpretation.
