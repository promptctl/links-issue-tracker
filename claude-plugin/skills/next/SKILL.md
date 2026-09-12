---
name: next
description: Pull the next ticket
---

# Next

Pick up the next ready ticket and start work.

## Init

Run `lit quickstart` if you haven't already.  This provides instructions for using the work tracking system.

If the user provided specific information (e.g., a ticket id or area of the codebase to work on), SKIP THE REST OF THESE INSTRUCTIONS and follow the guidance from `lit quickstart` to follow the user instructions.  The following information is for determining which work to pick when the user did not specify.

## Finding work

Which command answers which question is the quickstart's job — run `lit quickstart work` for that reference.  This skill covers only the procedure: wrapping up work already in flight before pulling anything new, what `lit next`'s pick means, and how to work a ticket once you hold one.

Two branches, in order.  First settle whatever is already in flight here — uncommitted changes, then your open PRs.  Only once nothing is in flight do you pull new work.

### Wrap up work already in flight

#### Uncommitted changes

determine if these changes are related to a backlog item.  If so, that is your current ticket.  If not, stop and think to your self: Are these changes worthwhile?  Accidential?  Incidental?  Should we commit or discard them?  Use your brain to think about the right solution because there is no one size fits all rule.

Examples:
- uncommitted pnpm lockfile update: check it out to discard, but then regengerate the lockfile as part of your commit when you do work
- Uncommitted typo in a random file: check it out to discard, it's not needed
- Minor update to the readme to include some more instructions: commit it and proceed
- Major update to the readme that is related to the work on the current branch: commit it and proceed
- Major update to work that is clearly NOT on this branch: stash it and proceed
- A half finished feature: find the ticket it's related to.  THIS TICKET IS YOUR ASSIGNED WORK. Skip the selection steps and go straight to "Working the ticket". If it's not related to a ticket you see, do a quick code review.  does the code look experimental and temporary or high quality?  Does it look complete or barely started?  Then briefly explain the state of the code, what it does, and any other info you have (no ticket, etc).  Ask if they want you to create a ticket and continue the work, if they want it to committed to work as part of a different ticket, or whether they want you to stash or discard it.  Follow that instruction.

Before proceeding, confirm the uncommitted changes are now resolved (committed, stashed, or discarded per the above).  If anything you did previously resulted in a reference to a specific ticket, THAT IS YOUR TICKET ID and you should skip the selection steps and go straight to "Working the ticket".

Do NOT proceed without either:
- no uncommitted changes
OR
- A ticket id to work on

#### Open PRs

lit tracks tickets, not GitHub PRs, so no lit command will surface one.  Two checks, in this order.

**This branch.**

```
gh pr list --head "$(git branch --show-current)" --state open
```

A PR here is your ticket.  Skip the selection steps and work it through `/memento:address-pr-reviews` — a globally installed skill, not one this repo ships, so don't go looking for it in the tree; if it isn't installed, work the review threads to a clean, merged close-out by hand.  Then pick up the working steps under "Working the ticket".

**Your PRs elsewhere.**

```
gh pr list --author @me --state open
```

A fresh session sitting on the trunk has no head branch to match, so the branch check alone reports nothing and the wrap-up rule never fires — you start new work on top of your own unfinished PR and find out at merge time.

These are candidates, not assignments.  One `gh` account is shared by every checkout on the machine and by the human, so a PR authored by `@me` may belong to a checkout that is actively working it right now.  Before adopting one, take the ticket id from its branch name and check who holds that lane: `lit backlog` prints the holder and how stale the claim is.  A lane another checkout holds fresh is theirs — leave it, say so, and move on.

A branch whose name carries no ticket id at all — someone's typo fix, a hand-cut experiment — is not lit-tracked work, so there is no lane to look up and nothing here for you to adopt.  Leave it where it is.

A lane nobody holds, or one whose claim has gone stale, is yours to wrap up, and wrapping up means the same thing it meant above: check that branch out, treat its ticket as the one you now hold, and take it through `/memento:address-pr-reviews` to a merged close-out.  Then pick up the working steps under "Working the ticket".  Finish it before you pull anything new — a PR you leave open here is the one the next session finds and wraps up instead of its own work.

**If nothing is in flight,** proceed to "Pull new work" below.  Open PRs stay relevant after that — an older one may touch the files your new ticket touches — which is why the overlap check is a step in "Working the ticket".

### Pull new work

Run `lit next` first, always.  It is the routing decision itself, not a suggestion box, and it is the only thing that knows what this checkout holds — a claim in lit is an (assignee, checkout) pair you cannot read off any listing.  It serves your own claims ahead of everything else: lanes you already hold, work you left in progress, other lanes of the epic you are already in, stale claims included.  Only once you hold nothing does it reach the global pool.  Whatever it hands you is your ticket — `lit start` it and go to "Working the ticket".

#### Orphaned tickets

`lit orphaned` lists in_progress tickets that have gone stale: claimed work somebody abandoned.  It is a repo-wide diagnostic view, not a queue you pull from, and you do not need it to find your *own* abandoned work — `lit next` hands that back to you already.

So reach for it only when `lit next` has nothing left to give, and read which of its three empty-handed answers you got — they are not interchangeable:

- **Bare `no ready work`** — nothing is queued at all.  An orphan is now the right pull: it is the most advanced work in the repo and somebody has to finish it.  `lit start <id>` takes it.  A stale claim transfers without prompting but prints who held it and how far they got, so check that lane for unmerged branches or PRs before building on it.  The orphan you take is your ticket — skip ahead to "Working the ticket".

- **`no ready work — the backlog is not empty, but nothing in it is startable here`**, naming ids.  Work exists; none of it is yours to take.  `lit orphaned` will not rescue this, and not by luck: the rows in that message are the ones held fresh by another checkout, or in flight and *not* abandoned, which is the exact complement of the stale claims `lit orphaned` lists.  Report what `lit next` named and stop.  Taking a lane another checkout holds fresh is `lit start --take`, a deliberate takeover the user directs — never your way around an empty-handed `next`.

- **`no ready work in <your epic>`**, naming what blocks it.  Then every orphan on that list is in somebody else's epic, and taking one is the epic-hop the order forbids — see "What the pick means" below, which is written for this exact moment.  Report the blocker and stop.  If a cross-epic orphan looks urgent, say so and let the user direct it; do not adopt it on your own initiative.

#### What the pick means

`lit next` is not showing you a menu.  Selection is epic-major: the epic in flight is finished before anything else starts.  Once this checkout has started work in an epic, `lit next` keeps handing you tickets from that same epic until it has nothing left to give — the ranked backlog is the rationale for that order, not a list you shop from.  Finishing a ticket does not open the field back up; run `lit next` again and it hands you the next thing in the same epic.  It is the order, and there is no knob.  (The exact routing precedence behind the pick is the quickstart's reference, not this skill's — `lit quickstart work` has it.)

**When your epic has open work but none of it is workable**, `lit next` does not quietly hand you another epic's ticket.  It stops and says so: no ready work in your epic, naming the ticket ids blocking it — or, when nothing is queued behind work already in progress, saying that instead.  If it names a blocker you can work, `lit start` that blocker; it's on your epic's path.  Otherwise you will be tempted to think "nothing for me here — I'll just grab the next thing off the backlog."  Do not.  An agent that reads the blocked-epic message and goes shopping — in the backlog, or in `lit orphaned`, which is the same aisle wearing a rescue badge — has recreated, by hand, exactly the epic-hopping failure this order exists to prevent.  Report the blocker and stop.  Moving to a different epic is a deliberate re-focus the user directs — never a fallback you improvise around a diagnostic.

## Working the ticket

However you arrived at a ticket — uncommitted work, an open PR, an orphan, or `lit next` — work it through these steps:

1. **Read the ticket fully.** Title, description, acceptance criteria, comments, linked PRs, linked tickets. If the ticket references a spec, doc, or prior PR, read that too. You are about to author code that claims to satisfy this ticket — earn the right to claim it.

2. **Surface blockers before starting.**
   - Acceptance criteria missing or vague? Investigate first (see below); ask only if it stays genuinely unresolvable.
   - Depends on another ticket that isn't done? Stop and report.
   - Spec referenced but doesn't exist? Stop and report.
   - The ticket conflicts with current branch state or uncommitted work? Stop and report.
   - **Does an older open PR touch the files this ticket will touch?** Rebuilding on top of stale code risks merge conflicts you cannot untangle later.

     ```
     gh pr list --state open --json number,headRefName,title,files \
       --jq '.[] | select(.headRefName | startswith("<ticket-id>") | not) | "\(.number) \(.headRefName) :: \(.files | map(.path) | join(", "))"'
     ```

     The `select` drops *this ticket's own* PR, and it keys on the ticket id for a reason worth keeping.  You reach this step from every arrival path, including one where you are still standing on the previous ticket's branch — step 3 has not switched you to the trunk yet.  Filtering on the checked-out branch instead would there hide the previous ticket's open PR: the single most likely overlap, and the one the very next step warns you about building on top of.

     It fails in the safe direction.  A PR whose branch does not carry its ticket id is not recognized as this ticket's own and shows up as a candidate — a false positive you read and dismiss, never a real overlap silently swallowed.

     If a *different* PR overlaps, surface it to the user with both the ticket and PR references before starting, rather than silently building over it.
   - Don't paper over ambiguity with assumptions — confirm scope first.

IN ALL CASES YOU MUST DO AS MUCH OBVIOUS PREPARATORY WORK AS YOU CAN BEFORE ASKING THE USER.

A mature engineer knows when to ask for help, and it isn't at the slightest hint of ambiguity and before they've put in a shred of effort to answer the question themselves.  "What do I do with this uncommited work" is only a good question if it isn't obviously work that Directly corresponds to the ticket matching the branch name.  "Acceptance criteria missing or vague?" It is only a good question if it's not clearly answerable via common sense or existing documentation or some other method. If there's real ambiguity, surface it. If it's just basic information about the repo, see if you can figure it out for yourself. In all cases, the user should be presented with The results of an Extremely quick Investigation rather than "Hey, I don't know what to do. Tell me what to do." 

3. **Set up the workspace.**

   - **Already on the ticket's branch?**  You are, if you arrived here from uncommitted work or an open PR.  Stay on it and skip to the cleanliness check — creating a second branch here strands the work you just claimed.

   - **Otherwise, branch from an up-to-date trunk** — not from wherever HEAD happens to be sitting.  Finishing a ticket leaves you standing on its branch, and `git checkout -b` from there silently forks the new ticket off unmerged work: the PR opens carrying the previous ticket's diff on top of its own, and it cannot be reviewed or merged on its own.  Get onto the trunk and update it first:

     ```
     git checkout master        # or the repo's default branch
     git pull --rebase
     ```

   - **Look for an existing branch before you create one.**  A previously-started ticket may already have one, and it may carry a slug — `<ticket-id>_slug` — so an exact-name checkout reads it as missing and you start a duplicate.  Search by substring, locally and on the remote, and ask for clean names:

     ```
     git branch --all --list '*<ticket-id>*' --format='%(refname:short)'
     ```

     Judge this on its *output*, not its exit status: the command succeeds with rc=0 and prints nothing when there is no match.  `--format` is not optional — the default output is decorated (`*` for the current branch, `+` for one checked out in another worktree, `remotes/` on every remote ref), and none of those lines can be pasted into a checkout.

     Then read what it printed:

     - **A bare name** — a local branch.  `git checkout <name>`.
     - **A bare name *and* `origin/<name>`** — one branch seen twice, local and remote.  Use the bare one.
     - **Only `origin/<name>`** — a branch pushed from somewhere you have never checked out here.  `git checkout -b <name> origin/<name>`.  Checking out the `origin/...` ref itself lands you on a detached HEAD with no branch and nothing to push to, which you will discover at the end, when you try to open the PR.
     - **Several genuinely different branches** — the search is a substring match, so a sibling ticket id can collide with yours.  Read them and choose deliberately; don't take the first line.

     One failure to expect, because `--format` is what hides it.  The `+` you just discarded meant "checked out in another worktree", so a checkout can come back:

     ```
     fatal: '<name>' is already used by worktree at '<path>'
     ```

     That is another local worktree holding the branch, and the message names where.  Go work in that worktree, or stop and report it.  Do not delete the branch and do not cut a second one for the same ticket — you would be starting the duplicate this whole step exists to prevent.

     Only when the search prints nothing do you create one:

     ```
     git checkout -b <ticket-id>
     ```

     Plain ticket id, no slug.  A slug is invented, so two sessions that both slug one ticket invent two different names for it, and the next search turns up "several genuinely different branches" and a judgment call that never needed to exist.  The bare id is the one name every session picks independently.

   - Confirm the working tree is clean before starting. If dirty, Figure it the f*ck out. You're a mature, responsible, highly skilled engineer. 

4. **State the plan in one paragraph, then start.** What the ticket asks for, how you'll verify it's done (the machine-verifiable criterion), and the first concrete step. Then begin.

## When to stop and ask

To be honest, rarely. You should be capable of figuring this stuff out. 

If you think that there's a chance that this could have negative impacts on other work, you can ask a quick question, but like I said, You need to make an attempt to answer the question yourself.  (The one recurring case — a branch carrying uncommitted work that belongs to no current ticket — is already handled in "Uncommitted changes" above.)
