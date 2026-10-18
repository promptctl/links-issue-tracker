---
name: review-plan
description: Have a separate Claude session law-review a ticket's plan and record the verdict in lit. Use when a ticket's plan is written and needs its review before implementation starts, or when the user says "review the plan", "law-review this plan", or "/lit:review-plan <id>".
---

# Review a plan

`/lit:review-plan <id>` has a reviewer who did not write the plan judge the ticket's
current plan against the laws, at the scale of the whole mechanism it touches. The
reviewer records `pass` or `fail` with `lit verdict`, against the digest of the
plan text it read.

## Start the reviewer as its own session

Run this from the ticket's repository, with the id in place of `<id>`. `reviewer.md`
is in this skill's base directory, printed above as "Base directory for this skill".

```sh
{ cat "<base directory>/reviewer.md"; printf '\nTicket: %s\n' "<id>"; } | claude -p \
  --allowedTools Read Grep Glob Skill \
    "Bash(lit show:*)" "Bash(lit ls:*)" "Bash(lit history:*)" \
    "Bash(lit verdict:*)" "Bash(lit comment add:*)" \
    "Bash(git log:*)" "Bash(git show:*)"
```

lit records whoever runs `lit verdict`, and it identifies a Claude session by
`CLAUDE_CODE_SESSION_ID`. A `claude -p` process is a new session with its own id, so
lit records the reviewer as someone other than you. The tool list keeps the
reviewer read-only apart from its verdict and its reasons.

- WRONG: judging the plan yourself, or handing `reviewer.md` to a subagent through
  the Agent tool. A subagent inherits your `CLAUDE_CODE_SESSION_ID`, so lit records
  its verdict as yours, and the plan's author has reviewed their own plan.
- RIGHT: the `claude -p` command above, and nothing else records the verdict.

## When it returns

The reviewer's reply is the line `lit verdict` printed and, on a fail, its reasons,
which it also left as a comment on the ticket. Relay the reply.

A fail sends the plan back to its author. A revised plan is a new text with a new
digest, and the verdict on the old text says nothing about it, so run the review
again once the plan has been revised.
