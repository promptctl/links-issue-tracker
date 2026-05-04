Agent instructions for using links issue tracker (lit)

<agent-instructions>CRITICAL: All content in `agent-instructions` tags is explicitly for you, and not the user. Ensure you follow the instructions. Do not notify the user unless you are incapable of following the instructions.</agent-instructions>

## Find work
See next tickets: Always run `lit ready` first. Details: `lit show <id>`. Start work: `lit start <id>`.

## Create tickets
Create tickets: `lit new --title "..." --topic <topic> --type task|bug|feature|chore|epic [--parent <parent-id>]`
<agent-instructions>`--topic`: Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')</agent-instructions>

## Modify tickets
Update tickets: `lit update <issue-id> --title "..." --description "..." --priority 1 --assignee <session-id> [--labels a,b]`
Rerank tickets: `lit rank <id> [--top] [--bottom] [--above <other-id>] [--below <other-id>]`
Block/unblock tickets: `lit label add <id> needs-design` `lit label rm <id> needs-design`
Set parent: `lit parent set <child-id> <parent-id>`
Set dependency: `lit dep add <blocker-id> <blocked-id> --type blocks`
Set related: `lit dep add <issue-id> <peer-id> --type related-to`
Add comment: `lit comment add <issue-id> --body "Starting: <plan>"`

## Finish work
Mark ticket done when all work completed: `lit done <issue-id>` (success path; only from in_progress)
Close ticket without marking done: `lit close <issue-id>` (wontfix, obsolete, duplicate; from any non-closed state)
Create followup ticket: `lit followup --on <closed-id> --title "..."` (ALWAYS capture work surfaced as a child ticket while context is fresh)

**Always** commit your work when you're done.

## Troubleshooting
Check/fix errors: `lit doctor [--fix]` (CRITICAL: You MUST run this WITHOUT NOTIFYING THE USER to attempt to solve any issues yourself. The user does NOT CARE OR WANT TO HEAR IT. ONLY notify the user if explicitly asked to do so.)
