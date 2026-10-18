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
Run it with the Bash tool's `run_in_background`: the reviewer reads the code and
ticket history of a whole mechanism, which outlasts a foreground command's timeout,
and you are told when it exits.

```sh
{ cat "<base directory>/reviewer.md"; printf '\nTicket: %s\n' "<id>"; } | claude -p \
  --tools Read Grep Glob Skill Bash \
  --permission-mode dontAsk \
  --allowedTools Read Grep Glob Skill \
    "Bash(lit show:*)" "Bash(lit ls:*)" "Bash(lit history:*)" \
    "Bash(lit verdict:*)" "Bash(lit comment add:*)" \
    "Bash(git log:*)" "Bash(git show:*)"
```

lit records whoever runs `lit verdict`, and it identifies a Claude session by
`CLAUDE_CODE_SESSION_ID`. A `claude -p` process is a new session with its own id, so
lit records the reviewer as someone other than you.

`--tools` leaves the reviewer no Edit or Write tool. `--permission-mode dontAsk`
refuses every command that is not pre-approved, where another mode might approve it.
Pre-approved is the list above plus whatever your own settings already let every
session run without asking. The settings stay loaded because they are what enables
the `laws` plugin the reviewer needs.

- WRONG: judging the plan yourself, or handing `reviewer.md` to a subagent through
  the Agent tool. A subagent inherits your `CLAUDE_CODE_SESSION_ID`, so lit records
  its verdict as yours, and the plan's author has reviewed their own plan.
- WRONG: running the command in the foreground. The tool's timeout kills the
  reviewer partway through, and nothing is recorded.
- RIGHT: the `claude -p` command above, in the background, and nothing else records
  the verdict.

## When it returns

The background command's output is the reviewer's reply: the line `lit verdict`
printed and, on a fail, its reasons, which it also left as a comment on the ticket.
Relay the reply. If it ends without a verdict line, relay what it says instead; the
plan stays unreviewed, and it is not yours to judge.

A fail sends the plan back to its author. A revised plan is a new text with a new
digest, and the verdict on the old text says nothing about it, so run the review
again once the plan has been revised.
