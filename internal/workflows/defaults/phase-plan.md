---
id: phase-plan
name: Plan the ticket, then record when the code starts
states: [plan]
---
## Order
1. Plan it: run the `lit:plan` skill with the ticket id (`/lit:plan <id>`). It holds the procedure, and ends with the plan recorded on the ticket (`lit show <id>` prints it).
2. Once the plan is recorded, record that the code is starting: `lit phase <id> --to impl`. A statusline and other sessions read the phase to see where this work stands, so it is only as current as the last `lit phase` you ran.
3. Any guidance that prints when the ticket leaves plan (a review of the plan, say) comes before you write any code.
