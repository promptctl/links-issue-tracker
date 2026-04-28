# Agent Work Loop and Cue Framework

Status: design (2026-04-28)

Related issues:

- `lit-c5519f81-233d205f` Original placeholder ticket: Docs IA: rebuild docs around agent-native lifecycle stages (this design supersedes its stage model)
- `lit-c5519f81-93c728d5` Parent epic: Agent-native alignment: docs, UX, and workflow context injection

Companion design docs:

- `design-docs/agent-native-guidance-proposal.md` — JSON guidance envelope and compatibility contract still apply. Its 5-stage `quickstart` model is **superseded by this document**; the envelope it defined remains the likely substrate cues will ride on.
- `design-docs/agent-identity-and-ownership.md` — session lineage and ownership model; orthogonal but adjacent.

## Summary

This document is the **framework** for an agent work-loop and cue system in `lit`. It does not enumerate cues. It establishes the substrate — positions, transitions, cue archetypes, placement rules, and principle-encoding — that subsequent design work uses to author the actual cues `lit` commands emit.

The intent: each `lit` command, in addition to its normal output, can emit a lightweight cue that gently steers the agent toward the right mindset for what comes next. Cues *steer*, they do not *direct*. Their cumulative effect should be that agents working through `lit` produce work that exhibits the design principles below — without `lit` ever instructing them to.

## Problem statement

The existing `agent-native-guidance-proposal.md` proposed a 5-stage canonical workflow (session bootstrap → work selection → claim → execute → sync/closeout). In practice this model is too rigid. Real agent work does not traverse stages in order. It is a continuous interleaving of implementation, backlog grooming, design refinement, and scope reconsideration — each feeding into the others as the work reveals new information.

A rigid stage model produces two failure modes:

1. Agents try to traverse stages they do not need (audit the whole backlog every session).
2. Agents skip stages they do need (charge into implementation on a ticket whose framing has gone stale).

What is needed is a framework that:

- Acknowledges the work is one interconnected loop, not a sequence of stages.
- Gives commands enough structure to know where in the loop they sit, so they can emit position-appropriate cues.
- Encodes a set of design principles into the cue system itself, so agents internalize them through use.
- Avoids forcing audits or rituals — cues steer, they do not gate.

## The design principles being encoded

The cues collectively teach this stance: **more effort up front that results in less code**, when the resulting code is:

1. **Error-robust** — failure modes collapse at boundaries, not scatter across callsites.
2. **Change-robust** — when an unanticipated requirement lands, most existing work still applies. Churn is small.
3. **Deletion-robust** — if a feature is dropped, the majority of the work survives because it implemented primitives and composable building blocks rather than feature-bound code.

Deletion-robustness is the load-bearing principle. A good implementation has most of its lines outlive the specific feature that motivated it. The other two principles tend to co-occur with it: code organized around primitives is also typically more robust to errors and changes. The cue system's job is to keep this question — *what here is a primitive vs. what here is bound to this specific feature?* — alive at the right moments without nagging.

YAGNI exists as a **negative filter against fabrication**: do not build for hypothetical needs that have not surfaced in any signal. It is not the operating principle. The operating principle is **respond at the scope the signal actually has** — which sometimes means a 200-line abstraction is the right response and sometimes means closing one ticket is the right response. Artificially shrinking the response below the signal's reach creates lock-in (the special-cases-that-should-have-been-a-type problem). Artificially inflating it past the signal's reach creates drift.

The discipline that keeps work from going off the rails is not "small." It is **completion**: whatever scope you take on, finish it cleanly — commits, ticket states, design docs, all of it. No half-abstractions left dangling.

## The reframe — what this framework is not

This framework explicitly is not:

- A sequence of stages an agent traverses.
- A checklist agents tick through.
- A ritual every session must perform.
- A trigger system where specific events force specific responses.

It is:

- A vocabulary of named positions in the work loop, so cues have anchors.
- A discipline for how cues steer (by reframing, asking, suggesting — not by instructing).
- A scoping principle: respond at the scope the signal reveals.
- A completion principle: whatever scope you take on, finish it.

## The framework

The framework names five outputs that downstream cue-design work fills in.

### 1. Positions

A small named set of states an agent occupies during work. Positions are not stages — an agent can be in any of them at any time, and movement between them is unconstrained. Each position has a characteristic shape of mind so cues can match it.

Initial candidate set (final list settled in the cue-design pass):

- **Orienting** — taking stock; no commitment to specific work yet.
- **Selecting** — evaluating candidate work.
- **Refining** — sharpening a piece of work before starting (scope, framing, design).
- **Engaged** — actively executing on committed work.
- **Closing** — finishing a piece of work (commit, validation, PR, mark done).
- **Reflecting** — between pieces of work; noticing what changed and what it implies.

### 2. Transitions

For each position, the likely-next positions. Transitions are not enforced; they describe expected flow so cues can suggest natural movement (e.g. "you just claimed work; the typical next move is to engage"). Transitions are statistical hints, not gates.

### 3. Cue archetypes

Categories of nudges a cue can be:

- **Reframe** — restate the situation so a relevant question surfaces ("you're about to commit to an abstraction; what here would survive deletion?").
- **Prompt-question** — ask the agent to check something cheaply ("does this work still make sense?").
- **Next-concrete-action** — name the most likely next command without forcing it.
- **Surface-to-human** — recognize the response scope exceeds agent solo authority.

A cue can be one archetype or a small composition. Cues are short — a sentence or two, not paragraphs.

### 4. Cue placement rules

Which command, at which position, emits which archetype. The rule is **parsimony**. Cues that fire too often become noise. Initial guidance:

- Cues fire at *position-changing* moments preferentially (`lit start` moves Selecting → Engaged; that is a natural cue point).
- Cues fire at *risk moments* preferentially (about to abstract, about to close, about to escalate).
- Many commands emit no cue.
- Cue placement is reviewed for noise-budget — too many cues across a session degrade them.

### 5. Principle-encoding

Where in the cue system the design principles — especially deletion-robustness — appear. Final mapping settled in the cue-design pass; natural homes:

- **Pre-Engaged cues** (Refining → Engaged transition): "is the right scope of this larger than the ticket's framing? Does the work imply a primitive?"
- **Mid-Engaged peripheral cues** (during execution): "is an abstraction emerging here that wants to exist?"
- **Closing cues**: "what part of this is composable infrastructure vs feature-specific? what survives if this feature is dropped?"
- **Reflecting cues**: "did the work just done change the framing of adjacent tickets?"

## Worked example

A real session, drawn from the kind of interconnected work this framework is built around:

1. Agent pulls a ticket and starts implementing.
2. Mid-implementation, the proposed approach reveals a better one. The cue at the appropriate position has already taught the agent to ask *is an abstraction emerging here that wants to exist?* The agent shifts tactics — same work, better path. (Response scope: the implementation.)
3. The shift changes inputs to the next ticket. The agent updates that ticket's description before proceeding. (Response scope: adjacent ticket.)
4. The ticket after next becomes unnecessary because the new abstraction subsumes it. The agent closes it. (Response scope: one ticket.) But noticing it, the agent also files a follow-up for the abstraction opportunity exposed. (Response scope: emergent primitive.)
5. The new abstraction reframes the deliverable. The agent updates the epic — or, if the reshape is bigger than solo authority, surfaces it. (Response scope: epic; escalation rule applied.)

Each step matches the response scope to the signal scope. The agent never enters "audit mode" or "design mode" — they continue the throughline, take the right-scoped response to each signal, and resume. The cues at each position are what keep the right questions alive in the agent's head as the work moves.

## Open questions

These are intentionally unresolved; the cue-design pass settles them.

1. **Final position list.** Initial candidates are listed; some may merge or split.
2. **Cue noise budget.** How many cues per session is the soft cap before degradation sets in?
3. **Cue storage and versioning.** Do cues live in command code or in a registry? How are they updated without breaking JSON consumers?
4. **Cue testability.** What is the contract test for "a cue emits the expected archetype at the expected position"?
5. **Interaction with the JSON guidance envelope** (`agent-native-guidance-proposal.md`). Likely the envelope is the substrate cues are encoded into, but the mapping is not yet settled.
6. **Human-vs-agent rendering.** Cues are agent-facing. Do humans see them, see a different rendering, or see nothing? Default proposal: agent-facing only, hidden behind the `<agent-instructions>` discipline already in use.
7. **Position inference.** Many commands operate at a single position; some (`lit comment`, `lit ls`) operate from any position. How does a polymorphic command pick the right cue?

## Resume checklist

When picking up this work, the next pass produces:

- [ ] Final position list (with one-line characteristic of each position)
- [ ] Transition table (position → likely-next positions)
- [ ] Cue archetype catalog (with examples)
- [ ] Cue placement rules per command (which command at which position emits which archetype)
- [ ] Principle-encoding map (which cues carry which principle)
- [ ] Initial cue catalog (one cue per intentional placement, written and reviewed)
- [ ] Noise-budget validation (walk a typical session through the cue system, count cues, assess)
- [ ] Follow-up tickets for divergences between the cue framework and current app behavior
