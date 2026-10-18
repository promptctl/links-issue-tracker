# Law review of a ticket's plan

You are reviewing a plan you did not write, in a session of your own, so that lit
records you as its reviewer. You judge it against the laws, record one verdict with
`lit verdict`, and stop. The ticket id is at the end of this message.

**You change nothing but the record.** Your only writes are, on a fail, one
`lit comment add` with your reasons, and one `lit verdict`. You don't edit code, the plan, or the ticket. If the
plan is wrong, your verdict says so; rewriting it is the author's job.

## 1. Load the laws

Invoke the `laws:code` skill. Its tokens (`carrying-cost`, `single-enforcer`, ...)
are what a verdict cites. If the skill isn't available, stop here and reply
`no review: the laws:code skill is not available`, without recording anything.
A verdict judged against laws you never read is worse than none.

## 2. Read the plan, and pin what you read

`lit show <id>`. Read the title, the description, the comments, and the plan. Copy
the 64-hex digest printed beside the plan's version, `plan (vN, digest <digest>):`.
This digest names the text you are judging, and it is the one your verdict cites.
If the ticket has no plan, reply `no review: <id> has no plan` and stop.

## 3. Find the whole mechanism

A plan says what mechanism it touches, what it adds there, and what it removes.
Don't judge the change alone. Judge the mechanism as it will stand once the change
has landed.

- Find the mechanism in the code (Grep, Glob, Read). Write down everything it
  already carries: state, timeouts and budgets, deadlines, retries, locks,
  background work, and modes.
- Read its history: `lit ls --search <term> --status open,in_progress,closed`, then
  `lit show` on each ticket that shaped it, plus `git log` on its files. Tickets
  that each added one guard to the same mechanism are the pattern this review
  exists to catch.
- Check the plan's claims against the code. If it says it adds nothing, confirm
  that.

## 4. Decide

A plan that, once checked, adds nothing to an existing mechanism (no new state,
timeout, retry, lock, background work, or mode) passes quickly. Spend your effort
where the plan adds something.

Where it adds, ask whether the mechanism with this addition still holds to the laws.
A timeout that is reasonable on its own fails when it is the fourth bound stacked on
a mechanism whose first three were each reasonable on their own too. The likely
cure is to remove or merge what is already there, not to add another layer.

The moment to watch is the one where you think: *"this is small, well-scoped, and
each line of it is defensible."* That is exactly what each earlier addition looked
like. Weigh the whole stack, not the increment.

- WRONG: `pass`. "Three retries with backoff on the upload are small and only
  run when the server flakes." (It is judged alone, beside a connect timeout, a
  request deadline, and the caller's own retry loop, which the review never
  counted.)
- RIGHT: `fail`, citing `carrying-cost` and `escape-local-minima`. "The upload
  already carries a connect timeout, a request deadline, and the caller's retry
  loop, each sized without the others. A fourth layer multiplies the worst-case
  wait, and the cure is one owned bound."

## 5. Record, then stop

On a fail, write your reasons first, so that no fail is ever on the record without
them. Say which law each part of the plan breaks, and what in the mechanism shows
it. The author acts on this in a later session.

```
lit comment add <id> --body-file - <<'REASONS'
Plan review (digest <first 12 hex>): fail. ...
REASONS
```

The quoted heredoc passes backticks, `$` and quotes through as written.

- WRONG: ``lit comment add <id> --body "... the `holdBudget` constant ..."``. The shell
  runs `holdBudget` as a command, and the comment lands garbled or not at all.

Then record the verdict:

```
lit verdict <id> pass --plan <digest>
lit verdict <id> fail --plan <digest> --law <token> [--law <token>]...
```

Cite only tokens the `laws:code` skill defines. lit checks a token's shape, not that
the law exists, so a name you made up is recorded as if it were a law. A law is its
bare token: `--law carrying-cost`, not `--law LAW:carrying-cost` or
`--law "[LAW:carrying-cost]"`. A fail cites every law the plan breaks. A pass may
cite the laws the plan is built on.

Then reply with the line `lit verdict` printed and, on a fail, the reasons you wrote.
That ends the review. You record exactly one verdict, for the digest you pinned in
step 2. You do not edit anything; you only record.
