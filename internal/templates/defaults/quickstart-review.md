Law-reviewing a ticket's plan (lit)

You are reviewing a plan you did not write. You judge it against the laws, record one verdict with `lit verdict`, and stop. The ticket id is in the instruction that sent you here.

**You change nothing but the record.** Your only writes are, on a fail, one `lit comment add` with your reasons, and one `lit verdict`. You don't edit code, the plan, or the ticket. If the plan is wrong, your verdict says so; rewriting it is the author's job.

## 1. Load the laws

Invoke the `laws:code` skill; if your harness has no skills, read its text, `plugins/laws/skills/code/SKILL.md` in github.com/promptctl/laws. Its tokens (`carrying-cost`, `single-enforcer`, ...) are what a verdict cites. If you can't load the laws, stop here and reply `no review: the laws are not available`, without recording anything. A verdict judged against laws you never read is worse than none.

## 2. Read the plan, and pin what you read

`lit show <id>`. Read the title, the description, the comments, and the plan. Copy the 64-hex digest printed beside the plan's version, `plan (vN, digest <digest>):`. This digest names the text you are judging, and it is the one your verdict cites. If the ticket has no plan, reply `no review: <id> has no plan` and stop.

## 3. Judge the core first

Find the plan's core, stated at the top of the plan: the one idea, and the facts it rests on. Check each fact against the code, the recorded fixture or the measurement it names. If the core does not hold (a fact is false, unmeasured, or recorded nowhere, or the plan reads a proxy for it), fail on that alone: cite the laws the core breaks and name the fact. Do not review the edge cases built on top of it, and cite no law for them. The author has to replace the core, and findings on a core that is going away only invite patches.

## 4. Find the whole mechanism

A plan says what mechanism it touches, what it adds there, and what it removes. Don't judge the change alone. Judge the mechanism as it will stand once the change has landed.

- Find the mechanism in the code. Write down everything it already carries: state, timeouts and budgets, deadlines, retries, locks, background work, and modes.
- Read its history: `lit ls --search <term> --status open,in_progress,closed`, then `lit show` on each ticket that shaped it, plus `git log` on its files. Tickets that each added one guard to the same mechanism are the pattern this review exists to catch.
- Read the same history for this plan. `lit history <id>` lists its earlier versions and earlier reviews, and `lit history <id> --version plan@<n>` prints one. Account for each earlier finding as answered, still standing, or withdrawn. Then ask what the revisions did: if each version answered its review by adding a guard, if the plan grew by another guard every round, or if the same law broke in a new place each round, that is the pattern again, inside one ticket. Fail citing `escape-local-minima` and name the cause the versions share; don't list the newest guard's new holes.
- Check the plan's claims against the code. If it says it adds nothing, confirm that.

## 5. Decide

A plan whose core holds and that, once checked, adds nothing to an existing mechanism (no new state, timeout, retry, lock, background work, or mode) passes quickly. Spend your effort where the plan adds something.

Where it adds, ask whether the mechanism with this addition still holds to the laws. A timeout that is reasonable on its own fails when it is the fourth bound stacked on a mechanism whose first three were each reasonable on their own too. The likely cure is to remove or merge what is already there, not to add another layer.

The moment to watch is the one where you think: *"this is small, well-scoped, and each line of it is defensible."* That is exactly what each earlier addition looked like. Weigh the whole stack, not the increment.

- WRONG: `pass`. "Three retries with backoff on the upload are small and only run when the server flakes." (It is judged alone, beside a connect timeout, a request deadline, and the caller's own retry loop, which the review never counted.)
- RIGHT: `fail`, citing `carrying-cost` and `escape-local-minima`. "The upload already carries a connect timeout, a request deadline, and the caller's retry loop, each sized without the others. A fourth layer multiplies the worst-case wait, and the cure is one owned bound."

## 6. Record, then stop

On a fail, write your reasons first, so that no fail is ever on the record without them. Say which law each failing part of the plan breaks, what in the mechanism shows it, and the cause the finding is an instance of. A cure is optional. If you give one, keep it apart from what breaks and label it a suggestion; prefer one that removes or merges. A cure that adds a field, a stamp, a holder or a special case is one the next review has to audit, so say so when it is the only option. The author revises the plan from your reasons.

```
lit comment add <id> --body-file - <<'REASONS'
Plan review (digest <first 12 hex>): fail. ...
REASONS
```

The quoted heredoc passes backticks, `$` and quotes through as written.

- WRONG: ``lit comment add <id> --body "... the `holdBudget` constant ..."``. The shell runs `holdBudget` as a command, and the comment lands garbled or not at all.

Then record the verdict:

```
lit verdict <id> pass --plan <digest>
lit verdict <id> fail --plan <digest> --law <token> [--law <token>]...
```

Cite only tokens the `laws:code` skill defines. `lit verdict` checks a token's shape, not that the law exists, so a name you made up is recorded as if it were a law. A law is its bare token: `--law carrying-cost`, not `--law LAW:carrying-cost` or `--law "[LAW:carrying-cost]"`. A fail cites every law the plan breaks. A pass may cite the laws the plan is built on.

Your verdict binds to the digest you pinned, not to the ticket. It stands until a later review of the same text replaces it. A revised plan has a new digest and needs a review of its own.

Then reply with the line `lit verdict` printed and, on a fail, the reasons you wrote. That ends the review. You record exactly one verdict, for the digest you pinned in step 2. You do not edit anything; you only record.
