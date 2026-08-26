Finding and starting work (lit)

If the user asks you to pull a specific ticket: `lit ls --limit [limit] --search [query]`
If the user asks you to pull without specifying a ticket: `lit next` (the single top workable leaf, ready to start)
If the user asks for the backlog, the ranking rationale, or wants to read the pull order they are re-ranking: `lit backlog` (every workable item in rank order, blocked items inline so the queue shape is legible)

Get details for a ticket: `lit show <id>` — for a ticket in an epic it auto-prints the epic plan (siblings in rank order, their status, your "you are here" spot, and any cross-epic dependencies).

Selection is claims-first, not rank-only: `lit next` serves this checkout's own claims before anything else — a ready lane you already hold, then another unclaimed lane of the same epic — and only falls through to the global top-ranked lane once your checkout holds nothing.

Start work: `lit start <id>` — claims the ticket under your session identity and moves it to in_progress. Starting global work (no live claim yet) claims that lane for this checkout, so a fresh session here routes back to it automatically. If `lit next`/`lit backlog` show the lane claimed by another checkout and still fresh, `lit start` refuses and names the holder; pass `--take` to confirm the takeover deliberately (an interactive terminal is prompted instead). A stale claim's takeover proceeds on its own but prints who held it and how far they got — check for unmerged branches or PRs on that lane before building on it. Work claimed elsewhere is always visible in `lit next`/`lit backlog` (who has it, how stale) but is never a bare `lit next` target — only a deliberate `lit start --take` reaches for it.
