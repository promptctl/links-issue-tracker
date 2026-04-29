# Onboarding: Discovery Path to "Prepare the Next Loop"

Status: design (2026-04-28)

This document briefs a future session — most likely a fresh-context agent — on the design work that produced `design-docs/preparing-the-next-loop.md`. Its purpose is not to repeat the conclusion (that doc is authoritative). It is to communicate the *journey* — what we tried, what we rejected, the specific dead ends, and the patterns of failure that shaped current understanding.

An agent picking this up cold should read this doc first, then `preparing-the-next-loop.md`, then supporting docs as needed.

## Punchline first

The work started as "rebuild the docs around 5 lifecycle stages." It became "design the canonical agent work lifecycle." It became "design a cue system riding on loop positions." It became "design a cue system that doesn't read user state." It became "stop adding cue text during the work — you cannot influence a loop from inside it."

**Final landing:** cues, conventions, tickets, and tooling are all soil-preparation for the *next* agent. The agent in the moment is the seed. We cannot grow it by talking to it. We prepare the soil.

If you read no further: the principle is in `preparing-the-next-loop.md`. The dead ends in this document explain why obvious-looking moves are wrong, so you don't redo them.

## The starting position

Placeholder ticket `lit-c5519f81-233d205f` titled *"Docs IA: rebuild docs around agent-native lifecycle stages."* Five-bullet description naming stages: human bootstrap → agent session bootstrap → work acquisition → execution → sync and recovery.

This *looked* like a docs-reshuffle ticket. The companion doc `design-docs/agent-native-guidance-proposal.md` already proposed a 5-stage `lit quickstart` model with a JSON guidance envelope. The natural temptation was to start writing doc files around the stage headers.

That temptation was wrong. The reframes that followed kept pulling the design upstream from "rearrange artifacts" to "design what the artifacts express" to "design what the system does at boundaries between loops."

## The reframes

Each reframe was driven by user correction. Each pushed the design one step backwards in the loop.

### Reframe 1 — *this is design, not docs*

**What we tried:** taking the 5 bullets as the structure for a documentation reshuffle.

**What we rejected:** starting from `docs/` and rearranging files around lifecycle headers. That would produce a clean-looking docs site that didn't reflect any design that actually existed in the system.

**Insight:** when a ticket appears to be about an artifact, check whether the underlying problem is the artifact's existence or the absence of a design the artifact would express. The 5-stage model was a draft of a design, not a directive about doc structure.

### Reframe 2 — *not stages, interconnected loops*

**What we tried:** a 6-position taxonomy (Orienting, Selecting, Refining, Engaged, Closing, Reflecting) with cues at boundaries.

**What we rejected:** rigid stage traversal as a model of agent work.

**User reframe:** the work isn't a sequence of stages. It's one organic process where activities interleave and feed into each other. You pull a ticket, mid-implementation discover a better approach, that changes inputs to the next ticket, that ticket becomes unnecessary, you see an abstraction opportunity, the abstraction reframes the deliverable, the epic gets retroactively adjusted. Not "loop A finished, now we're in loop B." One process; the loops are nested and feeding into each other.

**Insight:** the work has structure but is not sequential. Real work tangles. Designs that depend on the agent traversing stages in order miss what actually happens.

### Reframe 3 — *smallest local action is wrong*

**What we tried:** "smallest local action" as the discipline that prevents tangled work from spiraling.

**What we rejected:** minimalism as the operating principle.

**User reframe:** that's the YAGNI trap as a positive principle. Smallest-local produces lock-in: code that dies when the feature dies. The right discipline is *right-scoped response*. More effort that produces less code, where the resulting code is robust against errors, robust against unanticipated changes, and most critically, **robust against deletion**. A good implementation has most of its lines outlive the specific feature that motivated it. Very little great software was built strictly to YAGNI.

**Insight:** deletion-robustness is the load-bearing test for code quality. If a feature were dropped tomorrow, what survives? If "almost nothing," the implementation was at the wrong scope. YAGNI is a useful negative filter against fabrication; it is not a positive operating principle. **Completion is the rail, not size.**

### Reframe 4 — *return to loops, but as cue-anchors*

**What we did:** wrote `design-docs/agent-work-loop-and-cue-framework.md`. Positions, transitions, cue archetypes (Reframe / Prompt-question / Next-concrete-action / Surface-to-human), placement rules, principle-encoding for deletion-robustness.

**What survived from this:** the vocabulary. Position labels and archetype names are still useful descriptive tools.

**What was later superseded:** the *purpose* of the cue system, which Reframe 7 inverted entirely.

### Reframe 5 — *the methodology was thin*

**What we tried:** drawing conclusions from one or two sessions read deeply (including the current one, which is unreliable).

**User reframe:** can't trust memory of the current session; one session is one datapoint; raw sessions are huge. We have hundreds of sessions. Build a tool to process them into compact analytic data.

**What we built:** `tools/session-analysis/process_sessions.py`. Reduces 24MB of raw `~/.claude/projects/<encoded>/<session-id>.jsonl` files (41 sessions) to 3.6MB of structured digests + per-session markdown timelines + an index for cataloguing. Survives.

**What we then did:** walked four sessions of distinct shape (b1558792 guidance-audit, a4504c7b quickstart-fix, 0567deb2 epic-driven implementation, a6cc0c74 debug/recovery). Wrote `design-docs/agent-work-loop-walkthrough.md` with a 9-cue catalog and 8 affordance gaps.

**What survived from the walkthrough:** the methodology. The position list (validated empirically). The observation that Reflecting is mostly user-driven today.

**What did not survive:** the cue catalog itself. Reframes 6 and 7 invalidated most of it.

### Reframe 6 — *don't read user state*

**What we tried:** cues including "bias-toward-action" (read user evidence), "re-emphasis-as-signal" (user repeating themselves means frustration), and a proposed "frustration markers" affordance gap.

**User reframe:** anything I say or do that isn't oriented toward the mechanics of effective work is *noise* — and noisy data with confounding factors you cannot see. I might be hangry, tired, on mobile, reacting to something in another parallel session. Don't tune the cue system to user emotional state. **What I want is for the tool to be more consistent than I am.**

**What we rejected:** cues that respond to user signals. The cue system is not a feedback loop on user reactions. The user is environment, not subject.

**Insight:** the system you are designing must succeed regardless of user state. Anything that depends on reading user mood, predicting user intent, or adapting to user emotion is fragile and will fail. The system is agent + context + backlog + codebase. The user provides inputs but is not part of the system.

### Reframe 7 — *you cannot influence the current loop*

**What we tried:** a revised 10-cue catalog focused on the "mechanics of work" — investigate before asking, demonstrate-don't-acknowledge, cycle integrity, sibling-search, right-scoped response, etc.

**User dismantled each:**

- **Halt-and-reason cues** (e.g., "you have other in_progress tickets, decide what to do"): asks the agent to halt momentum and reason about unrelated state. Not a nudge; permission to make arbitrary decisions while confused.
- **Generic restatements** (cycle-integrity "plan→implement→verify"): obvious, already known, already in CLAUDE.md.
- **Wrong-timing cues** (verify gate at `lit done`): assumes verify happens *after* close; it actually happens *before*.
- **Vague cues** (right-scoped-response text): five sentences that mean whatever the agent decides.
- **Single-case cues** (sibling-search at every `lit done`): noise most of the time.
- **CLAUDE.md duplicates** (investigate-before-asking): already in `verifiable-goals` law.
- **Already-happening cues** (investigate-before-proposing): describes existing default; cue adds nothing.
- **Evidence-free cues** (demonstrate-don't-acknowledge): I had no measurement that tool calls after corrections improve outcomes; I was extrapolating from one observation.

User: ***NONE*** *of these is qualified for inclusion in the quickstart preamble. That text is highly curated. Every word counts.*

**Then the deepest reframe:**

*You cannot impact the loop from within the loop. You can only impact the current loop from a previous loop — or stated another way, you can only influence the next loop. The conditions for good work were set before the loop started. Adding text during the loop doesn't change those conditions; it adds another input the agent must process.*

*Working backwards from "agent doing good work": every prerequisite (context, focus, alignment with codebase / backlog / goals, skills, reference docs, tools, conventions) is prepared at touchpoints outside the loop that uses it. The discipline is **garden-tending**: prepare soil before planting; water and protect after; trust the seed to grow on its own.*

**What we rejected (cumulatively):** the entire frame of "cue the agent during the work." Almost all mid-loop cues are noise. The real work is at boundaries.

**Final landing:** `design-docs/preparing-the-next-loop.md`. Cues, when they exist, fire predominantly at *end-of-work* boundaries (ticket close, epic close, session end) where context is freshest and the closing agent can leave a deposit for what comes next.

## Patterns in the rejections

The dead ends shared shapes worth recognizing — they are the failure modes most likely to recur in future design work:

- **Restating CLAUDE.md.** The universal-laws block is dense and load-bearing. Anything you propose adding to a command's output should first be checked against what is already in `~/.claude/CLAUDE.md`. If it is already there, you are not adding signal; you are diluting it.
- **Halt-and-reason during work.** Asking the agent to pause and consider adjacent state is permission to make arbitrary decisions, not guidance toward a path. If the system has a real concern, the right move is structural enforcement, not a halt-and-think prompt.
- **Reading user state.** Confounding factors are invisible to the system; reactions are noise; the user is environment, not subject. Designs that depend on predicting user mood or intent are fragile.
- **Smallest-local discipline.** Lock-in trap. The right axis is response *scope* (matching the signal) and *completion* (cleanly finishing what you take on), not size.
- **Stage-as-checklist.** Real work tangles. Sequential traversals miss what actually happens.
- **Generic / vague cue text.** "Verify carefully," "consider scope," "think about the pattern" — undecidable, unenforceable, noise.
- **Asserting evidence you don't have.** If a cue's value is "I think this might help," that is a hypothesis, not a finding. Frame it as such, or drop it.
- **Designing for the current loop.** Almost any time you find yourself adding text that fires *during* an agent's work, ask: could this have been set up before the loop started? Almost always the answer is yes.

## Where we landed

The principle: *you cannot influence the loop from inside it; you can only influence the next loop.*

The discipline: garden-tending. Every session prepares the next session's environment. Capture-at-close. Quality at creation. Codebase-aware ticket review. Session-end deposits. Skill/convention/doc emergence at the moment patterns are recognized.

What we cannot control: the agent in the moment, the user, the world. The acceptance of this is the discipline.

## Status of the artifacts

| Doc | Status |
|---|---|
| `design-docs/preparing-the-next-loop.md` | Authoritative principle. Read this first after this onboarding. |
| `design-docs/agent-work-loop-and-cue-framework.md` | Vocabulary (positions, archetypes) survives; *purpose* superseded. |
| `design-docs/agent-work-loop-walkthrough.md` | Methodology survives; cue catalog mostly invalidated. Needs a follow-up correction pass. |
| `design-docs/agent-native-guidance-proposal.md` | JSON envelope substrate survives; the 5-stage `quickstart` model is superseded. |
| `tools/session-analysis/process_sessions.py` | Survives. Useful for any future cross-session analysis. |
| `tools/session-analysis/processed/` | Gitignored output. Run `python3 tools/session-analysis/process_sessions.py` to regenerate against the current session corpus. |

## For the next session: how to engage

If you are picking up this work fresh:

1. Read `preparing-the-next-loop.md` first. It is the principle. ~240 lines.
2. Read this onboarding doc second for the journey. The dead ends matter; they explain why obvious-looking moves are wrong.
3. The framework doc and walkthrough doc are background. Their vocabulary is useful; their conclusions are partially invalidated. Consult selectively.
4. The next move is most likely *not* designing more cues. It is identifying which touchpoints in the current `lit` surface need garden-tending affordances and building those. The "what we should build" priority list lives in `preparing-the-next-loop.md`'s Implications section.
5. Be skeptical of any design move that:
   - Asks the agent to read or predict the user
   - Adds text to a command's output during work
   - Asks the agent to halt mid-loop and reason about adjacent state
   - Restates a principle already in CLAUDE.md or the universal-laws
   - Treats a procedure as sequential when real work tangles
   - Optimizes for "smallest" rather than "right-scoped + complete"

These are the recurring failure shapes. If you find yourself proposing one, step back.

6. Trust the principle. Resist re-deriving it.

The garden grows on its own once the soil is right.
