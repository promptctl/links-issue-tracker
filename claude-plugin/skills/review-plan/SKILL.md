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

Run this from the ticket's repository, with the id in place of `<id>`.
`start-reviewer` is in this skill's base directory, printed above as "Base
directory for this skill". Run it with the Bash tool's `run_in_background`: the
reviewer reads the code and ticket history of a whole mechanism, which outlasts a
foreground command's timeout, and you are told when it exits.

```sh
"<base directory>/start-reviewer" <id>
```

It starts the reviewer as an interactive Claude Code session in a detached tmux
session, types `reviewer.md`'s path and the ticket id into its input, waits until
the reviewer sits idle at its prompt, prints its reply, and closes the session.
The reviewer runs in your environment, less your own session's variables. It
needs `bash`, `tmux`, `claude`, `jq`, `uuidgen` and `lit` on your PATH.

lit records whoever runs `lit verdict`, and it identifies a Claude session by
`CLAUDE_CODE_SESSION_ID`. The reviewer is a new session with its own id, so lit
records it as someone other than you. Its tools stop at Read, Grep, Glob, Skill
and Bash, so it has no Edit or Write, and `--permission-mode dontAsk` refuses
every command not pre-approved: `lit show`, `lit ls`, `lit history`,
`lit verdict`, `lit comment add`, `git log` and `git show`, plus whatever your own
settings already let every session run without asking. The settings stay loaded
because they are what enables the `laws` plugin the reviewer needs.

The reviewer is never started in Claude Code's print mode, its `-p` flag. The
owner banned print mode: anything that asks Claude Code something types into an interactive
session's input, as a person would.

- WRONG: judging the plan yourself, or handing `reviewer.md` to a subagent through
  the Agent tool. A subagent inherits your `CLAUDE_CODE_SESSION_ID`, so lit records
  its verdict as yours, and the plan's author has reviewed their own plan.
- WRONG: running `start-reviewer` in the foreground. The tool's timeout kills it
  partway through, it closes the reviewer's session, and nothing is recorded.
- WRONG: print mode, in any form, to save the tmux session. It is banned.
- RIGHT: `start-reviewer` in the background, and nothing else records the verdict.

## When it returns

The background command's output is the reviewer's reply, then the lines
`lit show` holds by the reviewer: its verdict and, on a fail, its reasons
comment. Relay the reply. It exits 0 only when the reviewer recorded a verdict on
the current plan; exit 2 means the reviewer never got its instruction. On any
exit but 0, relay what it printed instead; the plan stays
unreviewed, and it is not yours to judge.

A fail sends the plan back to its author. A revised plan is a new text with a new
digest, and the verdict on the old text says nothing about it, so run the review
again once the plan has been revised.
