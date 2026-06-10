Finding and starting work (lit)

If the user asks you to pull a specific ticket: `lit ls --limit [limit] --search [query]`
If the user asks you to pull without specifying a ticket: `lit ready`
If the user asks for the backlog or for the ranking rationale: `lit backlog` (every workable item in rank order, blocked items inline so the queue shape is legible)
If the user is re-ranking and wants to read the pull order they are shaping: `lit queue` (terse rank-ordered list of pullable items only — blocked items dropped, no preamble)

Get details for a ticket: `lit show <id>` — for a ticket in an epic it auto-prints the epic plan (siblings in rank order, their status, your "you are here" spot, and any cross-epic dependencies).

Start work: `lit start <id>` — claims the ticket under your session identity and moves it to in_progress.
