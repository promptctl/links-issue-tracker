---
id: phase-plan
name: Plan, have the plan reviewed, then record when the code starts
states: [plan]
---
<id> is in plan. Before any code, write its plan: what existing mechanism it touches, what it adds to it (state, timeouts, retries, locks, background work), and what it removes. Record it with `lit update <id> --plan-file <path>`, then give a reviewer with a fresh context (one that has not seen your work: a subagent, or a new session) the instruction "Law-review the plan of <id>: run `lit quickstart review` and follow it."; a fail stands until you revise the plan and have the new text reviewed. `lit done <id>` finishes the ticket wherever its plan stands, and warns when the current plan has not passed. When you start writing its code, record it: `lit phase <id> --to impl`. A statusline and other sessions read the phase to see where this work stands, so it is only as current as the last `lit phase` you ran.
