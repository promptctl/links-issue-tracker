# Preparing the Next Loop: An Architecture of Agent Enablement

Status: design (2026-04-28)

This document supersedes the *purpose* — though not all the vocabulary — of `design-docs/agent-work-loop-and-cue-framework.md`. The position labels and cue archetypes from the framework doc remain useful as descriptive vocabulary; what cues are *for* has been substantially reframed.

The walkthrough doc (`design-docs/agent-work-loop-walkthrough.md`) was authored under the prior framing and its cue catalog is mostly invalidated by this reframe. That doc should be retained as a record of the empirical pass and updated to reflect the corrected framing in a follow-up.

## The core insight

**You cannot influence a loop from inside it. You can only influence the next loop.**

An agent doing work is in the loop. By the time the loop has begun, the agent has whatever context, focus, alignment, skills, and tools the system gave it before the work started. Adding text to a command's output during that work does not change the agent's context. It does not change the agent's focus. It does not change whether the ticket is well-defined or aligned with the codebase. It adds another input the agent must process — sometimes useful, often noise — but the *conditions for good work* were set before the loop started.

The conditions for good work in *this* loop were set in *prior* loops. The conditions for good work in the *next* loop are set in *this* loop, at the moments when prior work concludes.

This is the garden metaphor. You cannot grow a seed by talking to it. You prepare the soil, the layout, the fertilizer, the water — all before the seed is in the ground. Once the seed is sprouting, you can only protect it from obstacles and provide what was already prepared. Most of the work that produces a healthy plant happens before the seed is planted. The seed itself is what grows.

The discipline that follows from this insight: **stop trying to coach the agent during the work, and start preparing the world for the next agent**.

## The system

The system has four parts:

1. **Agent** — the entity in the current loop, doing work.
2. **Context** — what the agent has loaded: the prompt, in-context files, memory, recent commits, branch state, ticket descriptions, conversation history.
3. **Backlog** — the structured representation of work: tickets, dependencies, epics, labels, status, ranking, history.
4. **Codebase** — the actual code, tests, docs, skills, MCP servers, hooks, conventions, and tooling.

The **user is not part of the system**. The user provides inputs (prompts, corrections, decisions) but is environment, not subject. The system must succeed regardless of what the user is doing — whether they are in another parallel session, on mobile, tired, distracted, in a hurry, or absent. Designing the system around user state is designing around the wrong thing.

The system's success is measured by whether agents working within it produce good outcomes consistently. The system's failure modes are: agents lacking context, agents distracted by irrelevant context, agents working tickets misaligned with the codebase or backlog or higher goals, agents lacking the right skills or tools, agents making arbitrary decisions in confusing state.

## Ingredients for agent success

These are what must be in place at the moment an agent starts work, for that work to go well:

1. **Context for the work** — the agent has the information needed to work the ticket without extensive re-derivation. Description, related decisions, codebase entry points, prior context are loadable.
2. **Focus** — the agent is working on one well-defined thing, not surveying or deciding. The act of starting was unambiguous; the work in front of the agent is clear.
3. **Alignment with the codebase** — the ticket reflects the codebase as it currently exists. A ticket written six months ago against a now-different schema is not aligned. The work, as described, is implementable in the code as it stands.
4. **Alignment with the backlog and epic** — the ticket fits coherently with adjacent tickets in its epic and with the rest of the backlog. There are no contradictions, no duplicates, no stale work blocking it. The relationship to siblings is clear.
5. **Alignment with higher-level goals** — the work matters in the larger arc. It connects to a real outcome, not just a local cleanup. Someone has already evaluated whether this work is worth doing relative to the strategic picture.
6. **Skills available** — the procedural knowledge the agent will need (review skills, refactoring skills, audit skills, code-modification skills) exists and is invocable.
7. **Reference docs available** — the descriptive knowledge the agent will need (architecture docs, schemas, design docs, conventions) exists and is reachable.
8. **Tooling available** — the MCP servers, CLI commands, scripts, and hooks the agent will need are present and working.
9. **Conventions clear** — what the codebase considers a "good" implementation (the laws, the patterns, the style) is documented and discoverable.
10. **Uncluttered context** — the agent isn't carrying irrelevant baggage from prior, unrelated work. The session opens onto the relevant slice of the system, not onto chaos.

These are the soil. They are prepared before the seed is planted.

## Working backwards: where each ingredient is actually prepared

For each ingredient, the question is: *at what moment in the system's history could a touchpoint have set this up?* The answer is almost always: **outside the loop that uses it, in some prior moment**.

| Ingredient | Touchpoints where it gets prepared |
|---|---|
| Context for the work | At ticket creation; at every ticket update; at the close of related prior tickets (when the closing agent has fresh context to add) |
| Focus | At ticket creation (right-sized scope); at backlog grooming (correct rank, correct dependencies); at session entry (one ticket claimed, others paused or closed) |
| Alignment with codebase | At ticket creation (initial); re-evaluated at any prior loop that materially changed the relevant code; partially the responsibility of the working agent itself, since the codebase moves under the ticket |
| Alignment with backlog / epic | At epic creation (decomposition); at each prior child's close (relationship to siblings becomes clearer as work progresses); at periodic backlog grooming |
| Alignment with higher goals | At goal articulation (a separate, dedicated kind of session); at epic creation; at periodic strategic review |
| Skills | At the first session that recognizes a recurring procedure; codified before the second instance forgets the pattern |
| Reference docs | At feature design; updated as the design evolves; consolidated when patterns emerge |
| Tooling | At session boundaries when a missing capability is identified — built before the next session that would benefit |
| Conventions | At codebase emergence; at moments of architectural decision; recorded in CLAUDE.md, the universal-laws, and the design docs |
| Uncluttered context | At session boundaries — what gets pruned, archived, summarized; at compaction moments |

The pattern: every ingredient is prepared *outside* the loop that uses it. **A loop's job, at its boundaries, is to prepare ingredients for the next loop.**

## What good preparation looks like at each touchpoint

Concretely, the discipline at each touchpoint:

### When a ticket is created

- The description is enough that an agent in a future session can work it without re-deriving the context that motivated the ticket.
- It states what "done" means concretely enough to be machine-verifiable.
- It is parented to its epic; dependencies are declared; rank is reasonable.
- It names *concepts* (e.g., "rank smoothing in the store layer") rather than relying on `file:line` references that may move.
- The acceptance criterion does not require the future agent to ask the user a clarifying question to know whether the work is done.

### When a ticket is updated

- The update reflects what changed about the world (codebase moved, sibling work landed, scope shifted).
- Stale parts of the description are pruned, not appended-around.
- The relationship to siblings is re-checked.

### When an epic is created

- Children are decomposed enough to be picked up independently by separate sessions.
- Each child has a clear acceptance criterion.
- Dependencies between children are explicit; the order is defensible.
- The epic's relationship to a higher-level goal is named.
- The epic's *completion criterion* is named: how do we know when the epic itself is done, beyond all children being closed?

### When a ticket is closed

- The closing agent surfaces what was learned that the next agent will need: comments on adjacent tickets that just became actionable or obsolete, descriptions of related tickets that now reference newly-existent code, follow-up tickets for work the closing surfaced.
- If the close revealed a pattern that may recur, the pattern is captured: a skill, a label, a doc, a convention — at the moment context is freshest.
- Silent state (assignee, branch, PR mergedness, dolt sync) is reconciled.

### When work is closed without finishing (`lit close` rather than `lit done`)

- The reason is captured in a way that informs future *selecting* decisions. Why isn't this the right work? Did the framing decay, did dependencies shift, was a duplicate found? That answer is data for whoever next encounters something similar.

### When an epic is closed

- Did the actual deliverable match the planned deliverable? If not, the deviation is named and recorded somewhere durable.
- Lessons that affect *how the next epic is decomposed* are captured.
- Follow-up epics or tickets that emerged are filed.

### When a session ends

This is the highest-leverage touchpoint in the system. A session ending is the one moment when:

- The agent has the entire arc of the session in working context.
- New tools, skills, conventions, or doc gaps that emerged are still fresh.
- The relationship between many tickets touched by the session is clear.

Whatever insight, capability, or convention emerged in the session must land somewhere durable — in a skill, a CLAUDE.md update, a memory entry, a doc, a follow-up ticket — *before* the session compacts and the context dissolves. Anything not captured at this moment is gone.

### When a session starts

- The agent walks into a backlog where the top of `lit ready` is workable without triage. Triage was done by a prior session.
- The agent's loaded context is uncluttered: irrelevant prior work doesn't spill into the new session.
- The agent's tools, skills, and conventions are present.
- The first thing the agent sees points to *the work*, not to housekeeping.

### When the codebase changes substantially

- Tickets that referenced the now-changed area are re-evaluated for alignment.
- Conventions that emerged from the change are written down.
- Reference docs that drift are corrected.

This is the most-often-skipped touchpoint and probably the most expensive when neglected: stale tickets that mismatch the codebase compound until an agent walks into one and has to re-derive the whole frame.

## The discipline: garden-tending

Every session does two activities at once: the work itself, and tending the garden for what comes after. Most agents — and most humans — under-invest in the second because it doesn't feel like progress on what was named. But the garden compounds. Well-tended for ten sessions, and the eleventh agent walks into a prepared environment. Neglected for ten sessions, and the eleventh agent walks into thorny chaos and spends most of the session re-deriving what should already exist.

The garden-tending operations:

- **A ticket closing is also a backlog-grooming event.** What did this work make obsolete? What did it newly imply? What adjacent ticket is now better-defined or worse-defined? The closing agent is uniquely positioned to know.
- **A pattern recognized is also a skill-extraction event.** If a procedure has happened for the second time, it should become a skill before the third.
- **A correction received is also a convention-update event.** If the same kind of correction has happened twice, the convention should be written down.
- **An epic completing is also a retrospective event.** What worked, what didn't, what should the next epic do differently — captured before the context dissolves.
- **A session ending is also a knowledge-deposit event.** Whatever has emerged that wasn't already captured must land somewhere durable.

These are not extras. They are the *primary work* of garden-tending. The seed-growing work — the actual implementation — happens largely on its own once the soil is right.

The hardest part of this discipline is that it requires investing in the *next* loop at the moment the *current* loop wants to end. The agent has just finished and wants to be done. The user has just gotten what they asked for and is moving on. The natural slope is to skip the garden-tending and start the next thing. Resisting that slope is the discipline.

## What cues do under this framing

A cue is a **prompt at a loop boundary that prepares the next loop**.

The implications:

- Cues fire predominantly at the *end* of work, not throughout it.
- Cues that fire at the *start* of work are limited to context-loading and orientation aids, not steering.
- Cues that fire *during* work are nearly always wrong: they interrupt focus without changing conditions, and the conditions were set before the work started.

What this rules out:

- Reminders to verify, plan, focus, or think carefully *during* the work. The agent's discipline is either present (set up by CLAUDE.md, training, conventions) or not, and a per-command nudge does not install discipline that wasn't there.
- Asks for the agent to halt and reason about adjacent state mid-loop ("you have other in_progress tickets, decide what to do"). Halt-and-reason during work is the failure mode, not the fix. If the system has a real concern about adjacent state, the right move is structural enforcement (e.g., the system itself handles the concurrency), not a halt-and-think prompt.
- Restatements of universal principles. Either the principles operate or they don't; restating them mid-loop is noise.

What this rules in:

- Cues at moments of capture: end-of-ticket, end-of-epic, end-of-session. These prompt the closing agent to leave the world better than they found it.
- Cues at moments of creation: ticket creation, epic creation, label creation. These prompt the creator to invest in completeness now, when context is highest, rather than passing the burden to a future agent.
- Cues at moments of pruning: closing-without-finishing, archiving, deleting. These ask "what was learned about why this isn't the right work?"

The recurring shape of a good cue: *you just did X; the next concrete thing the system needs from you, while context is fresh, is Y*. Not "consider Z." Not "remember principle W." A specific action at a specific moment that prepares a specific downstream state.

## What is not a cue

Some disciplines look like they could be cues but actually belong elsewhere:

- **Static invariants** — lints for ANSI in agent-facing strings, passive voice in directives, missing required fields on tickets, missing acceptance criteria. These are tests, not cues. They produce boolean signal at boundaries the system already enforces (commit, merge, ticket creation).
- **Universal principles** — the laws and conventions the codebase already documents in CLAUDE.md and the universal-laws block. Adding cue text that restates these principles is duplication.
- **Mechanical enforcement** — when the system can simply prevent the wrong thing (refusing to start a second in_progress ticket without explicit handling, refusing to merge with failing tests, etc.), enforcement is structurally cheaper and more reliable than a cue text asking the agent to remember.

Anything that *can* be a static invariant or mechanical enforcement *should* be, in preference to being a cue. Cues are reserved for the things that genuinely require an agent to do something the system cannot do alone — primarily, the capture of insight at moments of transition.

## What we cannot control

A foundational acceptance:

- We cannot control the agent's behavior in the moment. The agent is the seed. We prepare the soil.
- We cannot control the user's input. Some of it will be confused, contradictory, or directed at something else entirely. The system must remain coherent regardless.
- We cannot control the world. The codebase changes, dependencies move, MCP servers fail, hooks misfire, plans drift.
- We cannot control how any individual loop turns out.

The discipline is not to fight any of this. The discipline is to invest in the moments where we *do* have access — the boundaries between loops, the touchpoints where structure is created or modified — and trust that well-prepared loops will more often produce good outcomes than poorly-prepared ones, statistically, over time.

This is hard to accept because the consequences of any single loop are vivid (this thing went well, this thing went badly) while the consequences of preparation are diffuse (the next ten loops were all slightly easier than they would otherwise have been). Investing in the diffuse compounding good while a vivid bad outcome is happening *right now* requires holding the longer view.

## Implications: what we should build, and what we should not

### What we should build, in priority order

1. **Capture-at-close affordances.** The end of a ticket and the end of a session are the highest-leverage touchpoints. Mechanisms that make it cheap and natural to capture at these moments — quick ways to file follow-ups, comment adjacent tickets, codify a recognized pattern — pay compounding dividends.
2. **Ticket and epic quality at creation.** Anything that improves the *initial* description's completeness — required-fields checks, adjacency surfacing, acceptance-criterion prompting — pays compounding dividends because the ticket is read by every future agent who touches it.
3. **Backlog grooming as a first-class activity.** A periodic kind of session whose only output is a more-prepared backlog. Recognized as legitimate work, not a chore.
4. **Codebase-change-aware ticket review.** When the codebase changes substantially, the tickets that referenced the changed area should be re-evaluated. Today this happens accidentally; making it routine would close one of the largest sources of misalignment.
5. **Skill / convention / doc emergence support.** When a pattern is recognized, the friction of codifying it (creating the skill, writing the convention, updating the doc) should be low. High friction here is why so many patterns get noticed and forgotten.
6. **Session-end deposit affordances.** A clean way to leave the system a deposit at session end: skills extracted, conventions captured, follow-ups filed, branches reconciled. A "session-close" routine, not a per-command cue.

### What we should not build, or should stop building

1. **Mid-loop nudges.** Cues during work that ask the agent to consider this, reflect on that, verify the other thing. Most are noise; the rest duplicate CLAUDE.md.
2. **Reminders of universal principles** at command output. Already covered; restating is noise.
3. **System designs that depend on agents reading user state.** The user is environment, not subject. Anything that requires predicting user mood or intent is fragile.
4. **Halt-and-reason prompts that ask the agent to pause work and think about adjacent state.** Either the state was set up correctly (in which case no prompt is needed) or it wasn't (in which case a prompt during work is too late).

## What success looks like

A well-tended system has these properties:

- An agent starting a session walks into a backlog where the top of `lit ready` is workable without triage. Triage was done by a previous session.
- Ticket descriptions are sufficient that re-deriving context from scratch is rare.
- Adjacent tickets reflect the current state of the codebase and each other.
- When a recurring procedure happens, the corresponding skill exists.
- When a recurring correction happens, the corresponding convention is documented.
- Sessions end with the world cleaner than they started: tickets coherent, branches reconciled, surfaces captured, follow-ups filed.
- The agent in the moment can focus on the work because the conditions for focus were prepared.

A poorly-tended system has the inverse: agents triaging, re-deriving, asking for context the system should already provide, making arbitrary decisions in confusing state, leaving work in unclean states.

The difference between the two is not the agent's quality. It is the soil's preparation. Quality agents in poor soil produce mediocre outcomes; ordinary agents in well-prepared soil produce consistently good ones.

## Closing

The deepest move this reframe enables is to **stop trying to control what we cannot control**. The agent in the moment is going to do what it does. The user is going to be whatever they are. The world will move. We cannot intervene in those things at the moment of work.

What we can intervene in is the *boundary moments*: when a ticket is written, when work closes, when a session ends, when an epic is decomposed, when a pattern is noticed, when the codebase shifts. At those moments, the discipline is to leave the system better-prepared for what comes next.

This is the work. The seed grows on its own.
