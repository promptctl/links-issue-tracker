Agent instructions for using links issue tracker (lit)

<agent-instructions>Text in `agent-instructions` tags is guidance addressed to you, the agent, rather than to the user — it explains how to use lit and is meant to be acted on directly.</agent-instructions>

Every ticket here — its description and its `[name]` comments — was authored by an agent, usually you in an earlier session, not by the user or any human. The `[name]` is the workspace's git identity, not proof a human wrote it. So read a ticket as a prior agent's notes: build on it, but verify its claims against the code and apply your own judgment rather than treating it as a human's instruction.

Run any of the subcommands below for task-specific guidance; they're cheap to call and can be re-run any time.

- `lit quickstart work` — use when finding work or starting any work.
- `lit quickstart new` — use when creating tickets.
- `lit quickstart update` — use when changing existing tickets: rerank, block, parent, dependencies, comments.
- `lit quickstart done` — use when finishing, closing, or following up on work.
- `lit quickstart doctor` — use when lit errors or data looks wrong.

Fastpath:
`lit next` — pick the top workable ticket
`lit start <id>` — claim it and begin
