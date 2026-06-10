Managing existing tickets (lit)

Update fields: `lit update <issue-id> --title "..." --description "..." --priority 1 [--labels a,b]`
Rerank: `lit rank <id> [--top] [--bottom] [--above <other-id>] [--below <other-id>]`
Block/unblock: `lit label add <id> needs-design` / `lit label rm <id> needs-design`
Focus a goal: `lit label add <id> focus` surfaces the goal's unfinished prerequisite chain at the top of ready/queue/next (ordering only — blocked items stay blocked); `lit label rm <id> focus` restores normal order
Set parent: `lit parent set <child-id> <parent-id>`
Set dependency: `lit dep add <blocker-id> <blocked-id> --type blocks` (not allowed between two issues in the same epic — within one epic, rank is the ordering signal; cross-epic and free-floating issues are unrestricted)
Set related: `lit dep add <issue-id> <peer-id> --type related-to`
Add comment: `lit comment add <issue-id> --body "Starting: <plan>"`
