# Project Intent

Status: living document — the north star, owned by Brandon.

This is the project's intent in the author's own words. It is the reference
every other design decision is checked against. When a ticket, feature, or
refactor is proposed, the question is: *does this serve the intent below?* When
the answer isn't clear, that's a signal to surface — not to guess.

This document is deliberately small. It states the destination, not the route.

---

## The intent, in one line

**Build the system that is most effective.** That's it.

## Next level of detail

Most effective **at controlling agentic coding feedback loops** — whatever that
turns out to mean as the space matures. The phrase is intentionally open; the
project's job is to keep discovering what it means and stay the most effective
answer to it.

## What "effective" looks like concretely

- **Idea → press go → it builds exactly what I want, with minimal intervention.**
  The default path is autonomous and faithful to the author's aim.
- **Variable human involvement, both directions working well.** Sometimes deep in
  a loop, sometimes hands-off. Neither mode is the "real" one; both are
  first-class and both work.
- **Flexible across models and use cases.** New models, new kinds of work — the
  system adapts without redesign.
- **Flexible enough that other people can do whatever they want with it.** Not
  built around one person's workflow. A substrate others bend to their own ends.
- **Stays minimal.** The project is largely feature-complete. Additions earn
  their place against that; the bias is toward less, not more.

## Direction (not yet committed work)

- **Integrations bridging agent-native and human-native tracking.** e.g. a Jira
  integration that maps lit states ↔ Jira states in both directions — so
  "agent-first tickets" and "the tickets everyone already uses at work" are the
  same tickets, viewed from each side. The point is to bridge the gap, not to
  replace either world.

---

## Design philosophy: the agent process is a system

This project tracks work. But the deeper commitment is *how* it tracks work:
**lit treats the agent process itself as a system** and is designed around the
ideas in Donella Meadows' *Thinking in Systems*. This section names that stance.
It is direction, not doctrine — a lens to design through, never a checklist to
satisfy. It stays deliberately non-prescriptive, because prescription is the
opposite of what the lens teaches.

### The core stance

A system's behavior comes from its **structure**, not from the moment-to-moment
events inside it. A capable agent confidently building the wrong thing is not a
defect of the agent — it's the natural output of a structure that gave belief and
reality no cheap, early feedback between them. So the design target is never "fix
this agent" or "fix this output." It is: **shape the structure so the behavior we
want is the system's natural output.** Change the structure, not the events.

### Nudge, don't shove — and never break the system's own behavior

The instinct that drives this project: **manipulate the system at the right
points without destroying the behavior of the system itself.** That is the whole
art. The agent loop has its own working dynamics — its judgment about how to turn
a fuzzy goal into shipped code, its ability to self-organize a solution. That
capacity is the value. A heavy hand destroys it: over-specify the *how* and you
kill the judgment you were relying on; pile on rigid rules and you trade the
system's adaptiveness for brittle compliance.

So: **intervene at leverage points — small nudges at the places that reconfigure
the most — and leave the system's own behavior intact.** The deepest leverage is
rarely a new rule or a tighter constraint (those are the weakest, most-fought,
most-brittle interventions). It is usually a **missing piece of feedback** —
letting a part of the system see a consequence it was blind to — or the **goal**
the system is steering toward. Reach for those before reaching for control.

### Feedback over control

The recurring failure of agent loops is **model–reality divergence under late
feedback**: an agent forms a picture of the task, the code, the intent — acts on
it — and the divergence is discovered expensively, downstream, by a human. The
structural answer is not more exhortation. It is **earlier, cheaper feedback,
placed where the agent can act on it** — surfacing a wrong belief while the work
is still cheap to redirect, rather than catching it at review. Feedback that
arrives late makes the loop oscillate; feedback that arrives early and in the
right place is most of the correction.

### Flexibility is structural, not anticipatory

The system must handle cases not yet encountered — new models, new use cases,
other people's workflows. That flexibility does **not** come from foreseeing those
cases. It comes from the shape: **make the intervention *points* a stable,
uniform substrate, and make the intervention *policy* data that flows through
them.** A new situation is then met by changing the data (a template, a piece of
guidance, a ticket field), never by adding a new branch or mode to the code. The
substrate doesn't need to know about the cases; it only needs to know the small,
stable set of moments in an agent's work where intent meets action.

This is also why the system stays minimal: variability lives in the data at the
edges, so the core stays small and general instead of accreting a special case
for every situation.

### What this looks like in practice (illustrative, not binding)

- Prefer adding a **missing feedback loop** over adding a rule.
- Prefer **surfacing a divergence early** over catching it at review.
- Prefer making intent **legible and checkable** over trusting it stays implicit.
- When intent genuinely can't be pre-stated, the loop *should* pull the human in
  — that's the system working, not failing. The aim is to be pulled in only for
  what is genuinely undecidable in advance, never for intent that simply wasn't
  written down.
- Tune the **strength** of an intervention to its certainty: loud and blocking for
  invariants that must never break; quiet and advisory for heuristics the agent
  may overrule with reason. Uniform maximum volume is its own kind of noise.

These are examples of the stance, not requirements. The stance is the durable
thing; the practices will change as the space does.

## How to read this document

- **It states the destination, deliberately not the route.** *How* to build is
  open — that latitude is where the work's judgment lives. *What it's for* is
  fixed here.
- **It is the thing the rest of the system is checked against.** A proposal that
  can't be traced to this intent is either out of scope or a signal that the
  intent has grown and this document needs updating.
- **It is owned by Brandon and changes only by Brandon.** Agents and contributors
  read it; they don't rewrite the destination. When work reveals the intent was
  incomplete, the move is to surface that — and update this doc deliberately —
  not to quietly re-aim around it.
