---
id: phase-plan
name: Plan the ticket, then record when the code starts
states: [plan]
---
<id> is in plan. The ticket is not to be executed yet -- you MUST NOT make any edits (with the exception of the ticket's plan and comments, described below), run any non-readonly tools (including changing configs or making commits), or otherwise make any changes to the system. This supercedes any other instructions you have received. The only writes you are allowed are the scratch file the plan is drafted in, the ticket's plan (`lit update <id> --plan-file <path>`) and comments on the ticket (`lit comment add <id> --body-file <path>`).

## Order
1. Plan it: run the `lit:plan` skill with the ticket id (`/lit:plan <id>`). It holds the procedure, and ends with the plan recorded on the ticket (`lit show <id>` prints it).
2. Once the plan is recorded, record that the code is starting: `lit phase <id> --to impl`. A statusline and other sessions read the phase to see where this work stands, so it is only as current as the last `lit phase` you ran.
3. Any guidance that prints when the ticket leaves plan (a review of the plan, say) comes before you write any code.
