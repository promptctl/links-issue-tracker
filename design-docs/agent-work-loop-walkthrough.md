# Agent Work Loop Walkthrough — Empirical Evidence from Real Sessions

Status: design (2026-04-28)

Companion to: `design-docs/agent-work-loop-and-cue-framework.md`

This is the empirical pass on the cue framework: a structured walkthrough of real sessions to validate the position list, surface cue-emission moments, and find affordance gaps. The walkthrough operates on compact session digests produced by `tools/session-analysis/process_sessions.py` (raw .jsonl is one path lookup away when a specific tool result needs inspecting).

## Sessions sampled

Three sessions of distinct shape were walked end-to-end. Sample diversity matters more than corpus size for this pass — patterns that repeat across shapes are the load-bearing ones.

| Session | Date | Turns | Shape | Title / theme |
|---|---|---|---|---|
| `b1558792` | 2026-04-08–09 | 120 | guidance-audit + multi-PR housekeeping | strengthen-hook-agent-directive |
| `a4504c7b` | 2026-04-19 | 296 | quickstart fix → multi-branch sequence | fix-quickstart-output |
| `0567deb2` | 2026-04-24–26 | 568 | epic-driven implementation, 7 branches | lit-ready-exclude-epics |

Together they exercise: triage, design/plan, mechanical refactor, multi-PR housekeeping, long-running epic execution, in-flight scope discovery, and cross-session continuation. The patterns identified below all surfaced in at least two of the three sessions.

## Position list — confirmed and refined

The candidate position list from the framework doc held up. Refinements:

| Position | What the agent is doing | Confirmed from sessions |
|---|---|---|
| **Orienting** | "where do things stand" — git state, branch, open PRs, memory recall, top-of-ready scan | b1558792 turns 1–3; 0567deb2 turns 1–10 |
| **Selecting** | evaluating candidate work, picking the next thing | b1558792 turn 4 ("are they worthwhile to keep?"); 0567deb2 turn 0 (user-named work) |
| **Refining** | sharpening before starting — plan, decompose, write design notes, brainstorming | 0567deb2 turns 11–18 (writes plan file, ExitPlanMode); today's session (this doc) |
| **Engaged** | actively executing committed work | dominant mode of all three sessions; ~70% of turns |
| **Closing** | finalize a piece of work — commit, push, PR, validate, address reviews, merge, mark done | b1558792 turns 50–58 (commit→push→merge→delete); a4504c7b's repeated PR-finalization cycles |
| **Reflecting** | between pieces — noticing what just changed and what it implies for next | b1558792 turns 60, 73, 83 (each surfacing an adjacent fix the user noticed during their own use) |

**Two things the walk-through clarified:**

- *Reflecting is more often user-driven than agent-driven.* In b1558792, the user surfaces three adjacent fixes (sync-push, ANSI colors, doctor --fix) by noticing them during normal use. The agent doesn't reflect autonomously — it executes and waits. **This is an affordance gap, not a position-list problem.** Reflecting as a stage exists; the system just doesn't currently invite the agent into it.

- *Sub-modes exist within Engaged but don't need first-class names.* "Searching" (read same file 3x, redundant greps), "validating" (running tests), "writing" (edits), "scaffolding" (new files) all happen inside Engaged. They're tactics, not positions. Cues placed at Engaged boundaries are sufficient — no need for a finer position taxonomy.

## Cue moments observed — concrete catalog

Each of these fired in at least two sessions. Listed in priority order by impact.

### 1. Pre-recommendation cue — "check the user's preference before suggesting a structural split"

**Trigger:** agent is about to recommend splitting/restructuring work (separate PRs, multiple branches, separate commits).

**Example (b1558792 turn 9):** Agent recommended putting `install.sh` change on its own PR. User overrode immediately with "less PRs is better" — and again at turn 14 with "it's just a hassle to merge them." Captured retroactively as `feedback_one_pr_per_epic.md`. **The cue would have prevented the round-trip.**

**Archetype:** Reframe.
**Placement:** any command that's about to *recommend* (not just execute) — primarily mid-Engaged narrative output and Closing summaries.
**Cue text candidate:** `Before recommending a split, check the user's PR style preference (one bundled PR vs. one-per-concern). If unknown, default to bundled — splitting can be a follow-up.`

### 2. Bias-toward-action cue — "if it's reversible and you have evidence, do it; don't ask"

**Trigger:** agent is about to ask the user a yes/no on something the agent already has evidence for.

**Examples:**
- b1558792 turn 54: "Want me to retry `lit sync push`, or leave it?" — the hook *itself* says "agent should retry."
- b1558792 turn 72: "Want this on a PR, or commit directly to a new branch?" — fix is non-trivial; PR is the obvious right scope.
- a4504c7b turn 91 (similar): "Want me to..." after evidence makes the answer obvious.

**Captured retroactively** in memory as `feedback_act_on_proof.md`. The cue would internalize this without requiring memory match.

**Archetype:** Prompt-question (asked of the agent, not the user) → Reframe to action.
**Placement:** Closing position; before any final summary that ends in a question to the user.
**Cue text candidate:** `If unambiguous evidence + reversible action, do it and report. Surface only as a question when scope or reversibility actually requires user judgment.`

### 3. Pattern-extraction cue — "this looks like a procedure; codify it before forgetting"

**Trigger:** user issues a multi-step procedural directive (or the agent notices it's executing the same procedure for the second time).

**Example (b1558792 turns 16–22):** User said *"now wait ~8 min and address the PR review threads. ... ^^ turn this into a claude skill for me."* Agent created the `address-pr-reviews` skill, then *used it* to do the work. This is the right-scoped response — completed both the immediate work *and* the reusable artifact.

**Archetype:** Reframe + Next-concrete-action.
**Placement:** Reflecting; also at first instance of a procedure when the agent recognizes recurrence.
**Cue text candidate:** `If this procedure is likely to recur, the right-scoped response is the procedure + the codified version. A one-off is rare; a one-off that you remember to codify saves the second instance.`

### 4. Sibling-search cue — "you just fixed one instance; are there others?"

**Trigger:** an Edit or commit that fixes a class-of-bug (passive guidance, missing tag, drifted phrasing, etc.).

**Examples (b1558792):**
- Turn 70: fixed sync-push hook directive. **No autonomous search for siblings.**
- Turn 83: user surfaces the doctor --fix issue (same root cause, different site). Could have been found by the agent at turn 70 if a sibling-search cue had fired.
- Turn 100: agent extends fix to `error_output.go` and `ready_state.go` on its own (after being directed to doctor --fix), explicitly citing the "complete full scope of work" principle. Once primed, the agent does this well.
- Turn 119: final `Explore: Audit guidance strings` subagent finds even more sites.

**The pattern:** the work expands organically through *user-noticed* siblings until an explicit audit catches up. With a sibling-search cue, the audit could happen earlier and more cheaply.

**Archetype:** Prompt-question.
**Placement:** Closing position, after any commit that fixes a pattern (not a literal bug).
**Cue text candidate:** `Just fixed an instance of a pattern. Does the same pattern live elsewhere in the codebase? A 30-second sibling search now is cheaper than discovering the second instance later.`

### 5. Right-scoped-response cue — "is the work that wants to exist bigger than the ticket?"

**Trigger:** mid-Engaged moment when the agent is about to take the smallest local fix on something that has structural implications.

**Examples:**
- 0567deb2 turn 17 Insight: agent explicitly chose `ExcludeIssueTypes` (mirroring an existing positive filter) over a bespoke `ExcludeEpics bool` flag. **Right-scoped response done well.** The cue framework should reinforce this whenever the choice presents itself.
- b1558792 turn 100 Insight: agent noted the bigger architectural pattern (audience tags as a reusable convention) after the third instance.

**Archetype:** Reframe + Prompt-question.
**Placement:** mid-Engaged, especially during structural changes (new types, new flags, new error cases, new API additions).
**Cue text candidate (this is the deletion-robustness cue from the framework):** `If this feature were dropped tomorrow, what survives? If "almost nothing," you're implementing at feature scope when a primitive may want to exist.`

### 6. Audience-check cue — "this output is agent-facing; is the form right?"

**Trigger:** about to commit or render text that goes into agent input (hooks, error remediations, quickstart, agent-instructions blocks).

**Examples (b1558792):**
- Turn 73: ANSI escape codes in agent-facing hook text. Pure noise to agents.
- Turn 100, 111: passive vs imperative guidance phrasing. Agents parrot passive phrasing as user-facing options.
- Turn 119 audit: 8 sites converted, 5+ more found needing the same treatment.

**Archetype:** Prompt-question.
**Placement:** any Edit/Write to known agent-facing files (hooks, templates, error_output.go, quickstart, AGENTS.md, ready_state.go, etc.).
**Cue text candidate:** `Agent-facing string detected. Check: imperative not permissive? Plain text not styled? Inside <agent-instructions> if directive?`

### 7. Scope-validation cue — "is this ticket's framing still right?"

**Trigger:** at Selecting position when an agent picks a ticket that has been open for a while or whose context has shifted.

**Examples:**
- Today's session: 18 stale tickets closed because their framing had decayed (test fixtures, one-off repros, superseded by other work).
- Earlier sessions: similar pattern — agent picks a ticket and partway through Engaged realizes the framing is wrong.

**Archetype:** Prompt-question.
**Placement:** Selecting → Refining transition; output of `lit show <id>` or `lit start <id>`.
**Cue text candidate:** `Does this ticket's framing still hold? If the description, dependencies, or topic feel decayed, refine before claiming.`

## Affordance gaps — what the system doesn't enable

These are places where current `lit` (or its surrounding tooling) makes the *right* move harder than necessary, forcing agents to either skip it or assemble it by hand.

### A. No integrated Orienting view

Agents currently assemble Orienting context by hand: `git status` + `git log` + `lit ready` + `gh pr list` + memory recall + reading recent commits. Five-plus commands per session start.

**Observed in:** b1558792 turn 1 (user explicitly asks for orienting summary, agent runs three commands); 0567deb2 turns 1–10 (agent runs ten commands before doing real work); today's session (manual `gh pr list --search` + `lit ready` + `git log`).

**Proposal:** `lit context` (or `lit orient`) — one command that emits a structured Orienting payload: dirty files, branch state vs. master, open PRs touching this branch, top-of-ready, in-progress tickets, recent comments on tickets I'm involved with, branch-relevant tickets if branch name implies one. Output mode: Markdown for humans, JSON for agents. **Aligns with `agent-native-guidance-proposal.md`'s envelope.**

### B. No sibling-search affordance after a pattern fix

After fixing one instance of a passive-guidance string / missing tag / drifted phrasing, no built-in way for the agent to ask "is there a sibling?" The user has to notice and surface each instance.

**Observed in:** b1558792 turns 60–119 (user surfaced three siblings; an audit subagent found more at the end).

**Proposal:** Two layers.
- *Skill-level:* the existing `vet-comments` and `variance-audit` skills cover related needs but aren't triggered. The cue framework's sibling-search cue at Closing position would invite their use.
- *App-level:* `lit guidance audit` — a meta-command that scans every agent-facing string emission site (a known closed set: hooks, error_output, quickstart, ready output) and reports on imperative-mode and `<agent-instructions>` coverage. Single enforcer for the audience-check cue.

### C. Passive guidance language is not lint-able

The class-of-bug fixed across b1558792 (passive "agent should retry" → imperative "AGENT DIRECTIVE: retry yourself") has no mechanical guardrail. A grep can find current sites; preventing new ones is harder.

**Proposal:** A test in `internal/cli/` that walks all agent-facing string sources (hooks template, error_output remediation switch, quickstart text) and asserts:
- no ANSI escape sequences
- imperative voice (heuristic: detects `should`, `try`, `consider`, `you may` → fail; allows `do`, `run`, `do NOT`, `immediately`)
- `<agent-instructions>` wrapping when content is directive

This is mechanical enforcement of the audience-check cue's intent. **Aligns with `[LAW:single-enforcer]`.**

### D. PR finalization is multi-step

Standard sequence after an agent finishes work: `git add` → `git commit` → `git push` → `gh pr create` → wait CI → `gh pr merge --squash --delete-branch`. Each is a separate command and the agent sometimes pauses between them.

**Observed in:** every Closing sequence across all three sessions. Average ~6–10 commands per Closing.

**Proposal (low-priority; cue at Closing covers most of the friction):** out of scope for the cue framework directly. Note for follow-up: this is a candidate for an `address-pr-reviews`-style skill that orchestrates a "ship this" sequence with the right defaults (squash, delete branch, conventional commit message format from session work).

### E. Background-task notification spillover

Old session tasks completing into new session as `<task-notification>` user-role messages. Agents have to recognize and ignore.

**Observed in:** b1558792 turn 55 (notification from session `30ba134c` finishing). Agent correctly ignored but spent a turn on it.

**Proposal:** out of scope for cue framework — this is a harness/runtime issue. Note for follow-up only.

### F. Re-reading the same file

Multiple sessions show 2–3 `Read` calls on the same file across nearby turns (b1558792 turns 36–38: `cli.go` read 3 times in a row).

**Proposal:** out of scope for cue framework. Note: an LSP-aware cache or per-session file fingerprint would help.

### G. `needs-design` label is binary

The label exists today (`feedback_agent_directive_judgment.md`) but it's a single bit: blocks readiness or not. Real design states are richer: brainstorming, drafted, reviewed-pending, ready-to-implement.

**Proposal:** out of scope for cue framework. Note for follow-up: a `design-status` field on issues with a small enum, gated through the same readiness predicate.

### H. No "this is a position-changing moment" signal in command output

When `lit start` runs, the agent transitions Selecting → Engaged. When `lit done` runs, it's Closing → Reflecting (or → next Selecting). The commands know this; the agent has to infer it.

**Observed in:** every session — agents do not currently emit position-aware language consistently.

**Proposal:** the JSON guidance envelope (`agent-native-guidance-proposal.md`) is the natural carrier. Add a `position` field to object-shaped command outputs: `{"position": "engaged", "next_likely": ["closing"]}`. This is the hook the cue framework rides on.

## Synthesis — what this means for the cue framework

1. **Position list is right at 6.** No merges, no splits, no additions. Reflecting is the most under-served (mostly user-driven today); cues there have the highest leverage.

2. **Top-priority cues to author** (in this order):
   1. Bias-toward-action (Closing) — directly captures `feedback_act_on_proof.md`
   2. Sibling-search (Closing) — addresses the b1558792 whack-a-mole pattern
   3. Pre-recommendation (Closing summaries) — captures `feedback_one_pr_per_epic.md`
   4. Audience-check (any Edit/Write to agent-facing files) — fires at the source of the b1558792 root cause
   5. Right-scoped-response / deletion-robustness (mid-Engaged structural moments) — the framework's load-bearing principle
   6. Pattern-extraction (Reflecting) — invites the `address-pr-reviews` move
   7. Scope-validation (Selecting → Refining) — addresses today's stale-ticket pattern

3. **Top-priority affordance gaps** (in this order, by leverage × cost-to-build):
   - **A** `lit context` / orient command — high leverage, modest cost
   - **C** lint-test for passive guidance + ANSI in agent strings — high leverage, low cost
   - **B** `lit guidance audit` — moderate leverage, moderate cost
   - **H** `position` field in JSON envelope — high leverage, prereq for the cue system itself

4. **Cue noise budget validation** — the seven cues above, fired only at their position-changing or risk moments, would emit roughly 2–4 cues per typical Engaged-heavy session and 4–6 cues per multi-PR session. That's well under any plausible noise threshold. The cues are structurally sparse because their placement rules tie them to *transitions* and *risk events*, not every command.

## Follow-up tickets to file

Each of these is a concrete piece of work the cue framework needs. They become children of the rescoped placeholder ticket `lit-c5519f81-233d205f`.

1. **`lit context` command** — integrated Orienting view (gap A).
2. **Agent-facing string lint test** — passive-voice + ANSI + tag-coverage assertions (gap C).
3. **`position` field in JSON envelope** — substrate for cue emission (gap H).
4. **Cue archetype catalog (initial)** — write the seven cues above as a concrete cue table with placement, archetype, text, and the principle each encodes.
5. **`lit guidance audit` command** — structural inventory of agent-facing string sites (gap B).
6. **Cue test contract** — how do we test that "command X at position Y emits the expected cue"?

These take the framework from substrate to working system in a bounded sequence.

## Open questions surfaced by the walkthrough

1. *Reflecting is mostly user-driven today.* Is the right move to make agents reflect more autonomously (post-Closing self-prompts), or to make Reflecting cheaper/sharper for the user (better summaries, better surfaced-pattern detection)? These have different cue placements.
2. *The "AGENT DIRECTIVE" / `<agent-instructions>` convention is now load-bearing.* The audience-check cue depends on this convention. Are we comfortable promoting it from convention to enforced contract (lint test)?
3. *Position transitions are not yet tracked anywhere.* Adding `position` to JSON envelope is one path; another is per-session telemetry (which positions did this session traverse, in what order, with what dwell time). Is the latter worth building before we have cues?
4. *Cue text discipline.* Cues should be ≤2 sentences. Are there cues above whose proposed text is too long, and should they be split into a short cue + a referenced doc?

These belong on the cue-design pass, not on the framework.
