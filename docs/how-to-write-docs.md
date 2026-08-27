And how do turn these into lit tickets?  Use lit for a few minutes, get a feel for how it works, and help me understand how to take what you've got right now, which is a mind full of abstract context of all types, and turn that into the lit tickets that are going to produce the sessions that we want to engineer.  We want each ticket to fit into a session about twice this big, but keep in mind we have ticket updates and PR review overhead so let's say We're done with substantive work by 200k and just have the review process, some housekeeping, and handoff.  What looking for is several parts here. The first is the 'guidance for writing the documentation', the 2nd is the process for turning that guidance into lit tickets.  Note that this isn't a mechanical set of instructions, because the documents MUST NOT encode the tickets.  The docs are the docs and should be immortal (how about we version features so we know whats outdated).  I love the log idea for whatever doesn't fit into lit.  Would be great if lit could track it somehow.  But the process would be the guidance that leads to a backlog that is:
  - broken down into epics
  - the epics are logical in a technical sense - build entire units of functionality
  - include a 'circle back' pass where we integrate refactorings we learned while building.  "Oh that API really needs to work like this, but I had no way of knowing that until we did it".  This pass IS THE WORK.  Every ticket that is part of this work has been leading up to what we do here.  It should be "as many tickets as it takes + 1" and "as many tickets as you think x2"
  - each ticket is a unit of work that can be tested independently in some way.  build lightweight functional APIs for anything that isn't.  IO must be fully mocked and tested
  - slices should be vertical, but MUST MUST MUST be architecturally sound and aligned with the laws.  no exceptions even if it takes up half the epic.  It's THE LAW!  Thank you for your attention to this matter. (Joking)

  ultrathink on what makes a ticket like that and how we work backwards to the (high level, architectural) docs you're about to write get us those tickets in a way that is generalizable.  Thank you!!  You're a rockstar <3

Happy to — and the right way to answer this is to actually drive lit first, since the process I describe has to match the tool's real grain, not my mental model of it. Running commands one at a time (the store is strictly one-writer), with auto-sync off so the loop stays tight.

  Ran 4 shell commands

That ticket is a goldmine — I can see the house grammar directly now. One more surface to check: lit workflows, since it's how lit injects guidance at lifecycle points.

  Ran 1 shell command

────────────────────────────────────────────────── 1 new message ───────────────────────────────────────────────────

I drove lit through quickstart, backlog, a full lit show of a house-style ticket, and the workflows surface. The single most important thing I learned by using it rather than reading about it: lit show <child> prints the entire epic body inline above the ticket — the epic description is an auto-injected preamble for every child session. And the claims tickets already practice, informally, almost everything you're asking me to systematize: dated DECISION:-style rulings as comments, predecessor-to-successor handoff comments ("what .4 left you is one decision, not one function"), premise evidence as greppable commands, law tokens ([LAW:no-silent-failure]) cited inline, and lit followup --on as the capture primitive. So what follows is mostly naming and tightening a grammar your backlog already speaks, which is exactly why it will generalize.

The model that makes this coherent: four layers, four lifetimes

Everything below derives from one observation. There are four kinds of truth in this system, and each needs exactly one home:

┌─────────────────┬───────────────────┬────────────────────┬────────────────────────────────────────────────┐
│      Layer      │       Home        │      Lifetime      │                    Carries                     │
├─────────────────┼───────────────────┼────────────────────┼────────────────────────────────────────────────┤
│ Design truth    │ docs              │ immortal           │ the destination system, invariants, budgets    │
├─────────────────┼───────────────────┼────────────────────┼────────────────────────────────────────────────┤
│ Campaign truth  │ epic body         │ until epic closes  │ why this campaign, its destination, doc        │
│                 │                   │                    │ pointers                                       │
├─────────────────┼───────────────────┼────────────────────┼────────────────────────────────────────────────┤
│ Work truth      │ ticket            │ until ticket       │ the behavior this slice lands, acceptance      │
│                 │ description       │ closes             │                                                │
├─────────────────┼───────────────────┼────────────────────┼────────────────────────────────────────────────┤
│ Discovered      │ comments (the     │ append-only        │ rulings, learnings, handoffs, evidence         │
│ truth           │ log)              │ forever            │                                                │
└─────────────────┴───────────────────┴────────────────────┴────────────────────────────────────────────────┘

The circle-back pass is then not a cleanup chore — it's the reconciliation step that promotes discovered truth upward: into refactors (code) and doc amendments (design truth). That's what makes docs immortal without lying: they never contain work, and every time reality teaches us something, the circle-back is the scheduled moment the docs absorb it. The GRANULARITY RULING comment on claims .5 is the live proof: owner ruling logged as a comment, with "amend both doc sections when this ticket lands" as the reconciliation instruction. Your "would be great if lit could track the log" — verified: it already does. Comments on the epic for campaign-wide decisions, comments on tickets for local ones, DECISION: / LEARNING: prefixes for greppability. A first-class lit log view is a nice future ticket, not a prerequisite.

Part 1: guidance for writing the documentation

The docs' job, precisely stated: let a decomposer produce tickets whose orientation cost is small, without the docs ever knowing tickets exist.

1. No work-language, ever. The test for every sentence: does it become false when some ticket closes? "The fold validates events against the config version they cite" — survives forever. "We will add validation" — banned; that's a ticket leaking into a doc. This single rule is what makes docs immortal and is the enforcement of your "docs MUST NOT encode the tickets."
2. Feature versioning via per-section status lines (your versioning idea, made concrete): every section header carries status: destination | status: built (v0.9.0) | status: superseded → §anchor. Flipping destination → built with the release version is a named circle-back duty, so outdatedness is greppable (grep 'status: destination' = the unbuilt frontier; a built section describes shipped reality as of that tag).
3. Stable addresses. Sections get stable anchors; invariants get tokens ([INV:visible-skeleton]) cited from both docs and code comments. Tickets point with tokens, never line numbers (line numbers are the stale pointers your own tickets warn about) and never by restating the design (the PR #406 lesson: a claim duplicated across media rots per-copy).
4. Sized for scoped reads. One entry page mapping the sections; each section self-sufficient. Budget: any single ticket's assigned doc reading ≤ ~10–15k tokens. An implementer of a rank ticket reads entry + §rank, nothing else.
5. Claims carry their evidence. Any doc assertion about current code includes the one-line command that re-verifies it. Cheap insurance against the doc aging between decomposition and implementation.
6. Budgets as data. The measured baselines and 10x/<1s targets live in one harness-readable table, so "done" is a number, not a vibe.
7. No archaeology in docs. The doc states the current resolved position only; the why it changed lives in the lit comment that ruled it. Docs stay clean; history stays queryable where the work happened.

Part 2: the process — docs → backlog

Stage 0, the precondition: decomposition is done by a session holding both the docs and code reality in context — because the decomposer's entire economic function is to pay the discovery cost once instead of letting N implementers pay it N times. That's the session we're in right now, which is why you're right to do this before the context dissolves.

Stage 1, epic partition. An epic = a releasable unit of capability. The operational test comes free from repo policy (one release per epic): could we cut a release when this closes, and would its changelog describe one coherent capability? If the changelog would read as two stories, it's two epics. For this work, hypothesis: (A) Store-seam carve, (B) event store in shadow + 10x fleet harness, (C) the flip (reads, writes, sync-over-git-refs), (D) Dolt exit and deletion — then the RBAC epics. Each independently releasable, each with its own circle-back block.

Stage 2, the epic body, in the house grammar I saw on links-claims-1ihf: DESTINATION (the observable end state, unbriefed-agent phrasing), the symptoms (why this exists), THE MECHANISM in one breath, the normative pointer with precedence stated ("where a ticket and the doc disagree, the doc wins"), and WHY. This is the only design prose that ever lives in lit — a compression plus a pointer, never a copy — and because lit show injects it into every child session, whatever every implementer needs is written exactly once, here.

Stage 3, spine then slices. First tickets build the seams the laws demand — interfaces, types, fakes — even if that's half the epic; that's sanctioned explicitly. Then vertical capability slices through those seams. Every ticket must name its single test target that runs green in isolation; IO always behind a functional seam with an in-memory fake, plus one designated per-epic harness ticket that exercises the real IO. The heuristic that enforces your testability rule: if you can't name the ticket's test target in one line, it's two tickets. Order is authored via one lit import YAML batch (verified: a batch keeps its authored order; rank = readiness = execution order).

Stage 4, per-ticket authoring — working backwards from the session it will produce. This is the ultrathink core. A ticket engineers a session by setting three dials:

- Orientation cost (target ≤ ~30–40k): prepay discovery. Name the doc sections by anchor, the entry-point files/symbols, and premises with their evidence commands — the claims comments' grep -rn ... idiom — so re-verification is one command, not an expedition.
- Build scope (target ~100–130k): one new concept per ticket, roughly one package-neighborhood plus tests. Description in behavior language ("what survives a refactor of the code it concerns" — lit's own instruction), acceptance as a runnable scenario.
- Surprise rate (reserve ~30k): explicit non-goals naming which neighboring ticket owns each ("routing around stale claims is .7's, don't build it here"), and already-made decisions cited with authority and date so no session relitigates them and no review bot expands scope.

That totals ~200k substantive, leaving the tail for the review loop, housekeeping, and handoff — your stated envelope. Volatile pointers (current file paths, "as of today the function lives in X") go in a dated first comment, not the description, so the description never rots.

Stage 5, the circle-back block. Seeded at decomposition with one deliberately thin ticket — its content is definitionally unknowable upfront; that's the whole point. It's fed continuously: every ticket's close ritual is lit followup --on <id> for discovered refactorings plus a LEARNING: comment addressed to the future circle-back worker (the work_finished workflow event already carries an embedded post-close capture reminder — the hook exists). At epic end, a grooming pass converts the accumulated log into real tickets, sized by your rule: honest estimate ×2, and always +1 — where the +1 is specifically the coherence pass: read the epic's whole surface as one artifact and fix what's only visible at that altitude. The final circle-back ticket is always doc reconciliation: flip destination → built (vX), amend superseded sections per the logged rulings, cut the release.

★ Insight ─────────────────────────────────────
The backlog is a prompt compiler. lit show assembles each future session's context from layers with different lifetimes — epic body (shared preamble), description (the task), comments (the changelog of truth) — so decomposition is literally prompt engineering at one remove: the decomposer compiles its rich context into N session-prompts, and the quality of each future session was fixed at compile time. That's also why the circle-back must be tickets and not virtue: discovered truth has no other path back into design truth, and a backlog without that return edge slowly diverges from its own docs.
─────────────────────────────────────────────────

Why this generalizes: nothing above mentions event stores. It's four homes with stated precedence, discovery paid once at decomposition, three dials per ticket, and a scheduled reconciliation pass. Any future campaign — RBAC, hierarchy templates — runs the same loop against its own charter.

