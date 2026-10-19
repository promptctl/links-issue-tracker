---
id: phase-plan
name: Plan, have the plan reviewed, then record when the code starts
states: [plan]
---
<id> is in plan. Before any code, write its plan: what existing mechanism it touches, what it adds to it (state, timeouts, retries, locks, background work), and what it removes. Record it with `lit update <id> --plan-file <path>`, then have a reviewer other than you law-review it with `/lit:review-plan <id>`; a fail sends the plan back to you to revise and have reviewed again. `lit done <id>` finishes the ticket wherever its plan stands, and warns when the current plan has not passed. When you start writing its code, record it: `lit phase <id> --to impl`. A statusline and other sessions read the phase to see where this work stands, so it is only as current as the last `lit phase` you ran.
