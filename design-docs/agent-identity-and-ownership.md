# Agent Identity and Ticket Ownership

Status: draft

Related issues:

- `links-misc-k20` Investigate agent identity and ticket ownership model (this doc)
- `links-misc-yba` Improve orphaned ticket workflow (downstream consumer)

## Summary

Today, ticket "ownership" is a free-form `assignee` string and orphan
detection is a 6-hour staleness heuristic on `UpdatedAt`. That works as
a proxy but cannot answer the real question: *is the owner still
working on this?* This doc proposes a two-phase path that replaces
heuristic with mechanical liveness — phase 1 ships now, phase 2 is
deferred until phase 1 is in use.

## Problem

`newOrphanedAnnotator` in `internal/cli/ready_state.go` defines orphan as:

```
in_progress AND time.Since(UpdatedAt) >= threshold
```

`UpdatedAt` is a proxy for "agent is alive and progressing." It fails
in both directions:

- A long-running task (build, eval, refactor) can be alive but silent.
  False-positive orphan.
- An abandoned ticket is silently still "owned" until threshold elapses.
  False-negative orphan.

`assignee` is a column with no enforced shape — `claude`, `bmf`, `""`,
anything. Orphan detection cannot use it.

## Core insight (from the ticket)

> An agent's identity is essentially a specific context (or child
> context through compaction).

The unit of ownership is the **session lineage**, not the human, not
the worktree, not the branch. A compaction preserves the session; a
new `claude` invocation does not. The Claude Code session ID is the
right primitive.

Worktrees and branches are *evidence* of liveness, not identity. An
agent in a worktree on a live branch is probably alive — but that's a
signal we layer on, not a definition.

## Proposal

### Phase 1 — convention + heartbeat (ship now)

**1. Canonical assignee shape: `claude_<sessionId>`**

The `--assignee` help text on transition commands (see `runTransition`
in `internal/cli/cli.go`) already hints this. Make it a
recommendation, not a hard validation — humans still need to assign
things to themselves (`bmf`, etc.), and agents on other tools (Cursor,
Codex) need a slot too. Document the convention:

- `claude_<sessionId>` — Claude Code session
- `<tool>_<sessionId>` — generic agent
- bare string — human or unknown

The shape lets future tooling parse owner-kind without breaking the
free-form column.

**2. Heartbeat event, not a heartbeat column**

Add `lit heartbeat <id>` that emits a field-history event of kind
`heartbeat` carrying `(actor, timestamp)` and no field changes. The
field-history `issue_events` table is already the right substrate —
it's an append-only event log keyed on issue.

Orphan detection becomes:

```
in_progress AND time.Since(latest_event_for(owner)) >= threshold
```

`UpdatedAt` is replaced by `latest_event_for(owner)` — an event the
owning agent produced, not "anybody touched the row." This kills the
false-negative case where a different actor's edit silently resets
the orphan clock.

Agents call `lit heartbeat` from their loop (e.g., once per
tool-cycle, or on session resume). No new schema; the event log
already exists.

**3. Ownership transfer is `lit assign`**

Already shipped (`lit assign` on the field-history branch). Document
it as the canonical handoff
mechanism. No new command.

### Phase 2 — process liveness (deferred)

Heartbeat replaces *staleness* with *quietness* — better, but still a
proxy. The ground-truth check is "is session `<sessionId>` actually
running right now?" That requires an external probe:

- A Claude Code "list active sessions" API or local socket
- A sentinel file written by the session, cleaned up on exit
- A registry process the session registers with on start

All three are out of scope for the lit repo — they're Claude Code (or
agent-tool) features. Defer phase 2 until phase 1 is in use and the
remaining false-positive rate justifies the integration cost.

## Answers to the ticket's questions

1. **What constitutes agent identity?** A session ID. Context lineage
   (including compactions) — not worktree, not branch, not user.
2. **How do we encode ownership?** `assignee = claude_<sessionId>` as
   convention. Free-form column unchanged.
3. **How do we enforce ownership?** `lit start --assignee` already
   stamps it (`runTransition` rejects `start` without `--assignee`).
   Phase 1 adds heartbeat events; phase 2
   adds process-level liveness.
4. **What about compaction?** Same session, same owner. Heartbeat
   continues uninterrupted across compaction boundaries.
5. **What about worktree agents?** Worktree existence is a soft
   liveness signal worth surfacing in `lit doctor`, but not part of
   identity. Don't bake it into ownership.

## Why this aligns with project laws

- **one-source-of-truth**: identity lives in `assignee`; liveness
  lives in the event log. No new column duplicating either.
- **dataflow-not-control-flow**: heartbeat is just another event in
  the stream; orphan detection reads the same event log everything
  else does. No new conditional path.
- **single-enforcer**: orphan classification stays in
  `newOrphanedAnnotator`; only the lookup changes.

## Out of scope

- Validating `assignee` shape (would break human assignees)
- Cross-tool session registry (phase 2)
- Worktree-aware liveness (phase 2 add-on)
- Automatic orphan reclamation — `links-misc-yba` owns that workflow
