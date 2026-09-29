Managing existing tickets (lit)

Update fields: `lit update <issue-id> --title "..." --description "..." --priority urgent [--labels a,b]`
Update many at once: `lit import --path <file.yaml>` with an `id:` in each YAML document
selects that ticket for the same field patch `lit update` applies — run `lit import --help`
for the format.
Rerank: `lit rank <id> [--top] [--bottom] [--above <other-id>] [--below <other-id>]`
Block/unblock: `lit label add <id> needs-design` / `lit label rm <id> needs-design`
Wait on an outside event: `lit label add <id> external` when the ticket's resolution is an event outside this repository (an upstream fix, a vendor release); name the event and how to re-check it in the description, make whatever waits on it depend on it with `lit dep add --from <id> --to <waiting-id> --type blocks`, and close it with `lit done <id>` once the event has happened
Focus a goal: `lit label add <id> focus` narrows what `lit backlog` lists, and the pool `lit next` picks a NEW ticket from, to the goal's unfinished prerequisite chain (membership only — blocked items stay blocked, and rank still orders what is left); work this checkout already holds is still served by `lit next` even when it is off the path, so focus decides where a fresh start goes, never whether your own in-flight work is still yours; `--all` on either command ignores the scope for one run, and `lit label rm <id> focus` lifts it
Set parent: `lit parent set --child <child-id> --parent <parent-id>`
Set dependency: `lit dep add --from <blocker-id> --to <blocked-id> --type blocks` (not allowed between two issues in the same epic, or between an issue and any epic above it — within one epic, rank is the ordering signal; cross-epic and free-floating issues are unrestricted; blocking an epic holds back every issue under it)
Set related: `lit dep add --from <issue-id> --to <peer-id> --type related-to`
Add comment: `lit comment add <issue-id> --body "Starting: <plan>"`
