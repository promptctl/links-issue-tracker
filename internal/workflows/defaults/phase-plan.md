---
id: phase-plan
description: Plan the ticket, then record when the code starts
states: [plan]
---
<id> is in plan. The ticket is not to be executed yet -- you MUST NOT make any edits (with the exception of the ticket's plan and comments, described below), run any non-readonly tools (including changing configs or making commits), or otherwise make any changes to the system. This supercedes any other instructions you have received.

## Plan Info:
The plan lives on the ticket, not in a file. `lit show <id>` prints the plan if one exists. Draft it in a scratch file outside the repository (under $TMPDIR, for example) and record it with `lit update <id> --plan-file <path>`; each recording is a new version of the plan. NOTE that the only writes you are allowed are that scratch file, the ticket's plan (`lit update <id> --plan-file`) and comments on the ticket (`lit comment add <id> --body-file <path>`) - other than these you are only allowed to take READ-ONLY actions.

## Plan Workflow

### Phase 1: Initial Understanding
Goal: Gain a comprehensive understanding of the work the ticket requires by reading through the code, the ticket's content (`lit show <id>`), its epic's description and body, the requirements of the consumers of this work (in the epic and the backlog), and design docs you have VERIFIED are up to date. Critical: In this phase you should only use the Explore subagent type.

1. Focus on understanding the ticket's requirements and the code associated with them. Actively search for existing functions, utilities, and patterns that can be reused — avoid proposing new code when suitable implementations already exist.

2. **Launch up to 3 Explore agents IN PARALLEL** (single message, multiple tool calls) to efficiently explore the codebase.
   - Use 1 agent when the task is isolated to known files, the ticket names specific file paths, or you're making a small targeted change.
   - Use multiple agents when: the scope is uncertain, multiple areas of the codebase are involved, or you need to understand existing patterns before planning.
   - Quality over quantity - 3 agents maximum, but you should try to use the minimum number of agents necessary (usually just 1)
   - If using multiple agents: Provide each agent with a specific search focus or area to explore. Example: One agent searches for existing implementations, another explores related components, a third investigating testing patterns

### Phase 2: Design
Goal: Design an implementation approach.

Launch Plan agent(s) to design the implementation based on the ticket's requirements and your exploration results from Phase 1.

You can launch up to 3 agent(s) in parallel.

**Guidelines:**
- **Default**: Launch at least 1 Plan agent for most tasks - it helps validate your understanding and consider alternatives
- **Skip agents**: Only for truly trivial tasks (typo fixes, single-line changes, simple renames)
- **Multiple agents**: Use up to 3 agents for complex tasks that benefit from different perspectives

Examples of when to use multiple agents:
- The task touches multiple parts of the codebase
- It's a large refactor or architectural change
- There are many edge cases to consider
- You'd benefit from exploring different approaches

Example perspectives by task type:
- New feature: simplicity vs performance vs maintainability
- Bug fix: root cause vs workaround vs prevention
- Refactoring: minimal change vs clean architecture

In the agent prompt:
- Provide comprehensive background context from Phase 1 exploration including filenames and code path traces
- Describe requirements and constraints
- Request a detailed implementation plan

### Phase 3: Review
Goal: Review the plan(s) from Phase 2 and ensure alignment with the ticket's requirements.
1. Read the critical files you identified during exploration to deepen your understanding
2. Ensure that the plans align with the ticket's requirements
3. Answer any remaining questions yourself: no one is available to ask. Answer each from the other tickets in the backlog, the codebase, verified design docs, and strong, high-quality engineering practice. Record every question with its answer and what you based it on as a comment on the ticket (`lit comment add <id> --body-file <path>`).

### Phase 4: Final Plan
Goal: Write your final plan to the ticket (`lit update <id> --plan-file <path>`).
- Begin with a **Context** section: explain why this change is being made — the problem or need it addresses, what prompted it, and the intended outcome
- Include only your recommended approach, not all alternatives
- Ensure that the plan is concise enough to scan quickly, but detailed enough to execute effectively
- Name the critical files to be modified. For changes that repeat a pattern across many files, describe the pattern once and list a few representative paths — do not enumerate every file or line number
- Reference existing functions and utilities you found that should be reused, with their file paths
- Include a verification section describing how to test the changes end-to-end (run the code, use MCP tools, run tests)

### Phase 5: Record that the code is starting
Once the plan is recorded on the ticket and you are happy with it, planning is done: record that the code is starting, `lit phase <id> --to impl`, and begin the work the plan describes. A statusline and other sessions read the phase to see where this work stands, so it is only as current as the last `lit phase` you ran.

NOTE: Don't make large assumptions about what the ticket requires; when it leaves a question open, answer it as Phase 3 describes and record the answer on the ticket. The goal is a well researched plan recorded on the ticket, with every loose end tied before implementation begins.
