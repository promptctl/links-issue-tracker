---
id: next
description: Start the ticket lit next hands back
events: [next]
---
`lit next` answers for this checkout:

<ticket>

If that says no ticket, there is nothing to start. Filter flags and a focus label each narrow what `lit next` asks about: run it again without the filter, or with `--all` to look past the focus, if either applies. Otherwise `lit backlog --all` shows the whole queue and which checkout holds each lane. Report what is in the way and stop. Do not take a lane another checkout holds.

Otherwise this is the routing decision: work this ticket, not another from the backlog. If it is already in progress, read `lit show <id>` and continue from its branch or PR. Then work it through these steps, in order.

1. **Read the ticket fully.** Title, description, acceptance criteria, comments, linked PRs, linked tickets. If the ticket references a spec, doc, or prior PR, read that too. You are about to author code that is meant to satisfy this ticket, so earn the right to deliver it.

2. **Surface blockers before starting.**
   - Acceptance criteria missing or vague? Investigate first, and ask only if the gap stays genuinely unresolvable.
   - Depends on a ticket that isn't done? Stop and report.
   - Spec referenced but doesn't exist? Stop and report.
   - The ticket conflicts with the current branch state or uncommitted work? Stop and report.
   - Does an older open PR touch the files this ticket will touch? Rebuilding on stale code risks conflicts you cannot untangle later. Check with:

     ```
     gh pr list --state open --limit 1000 --json number,headRefName,title,files \
       --jq '.[] | select(.headRefName | . == "<id>" or startswith("<id>_") or startswith("<id>/") | not) | "\(.number) \(.headRefName) :: \(.files | map(.path) | join(", "))"'
     ```

     The filter drops this ticket's own PR and compares literal strings, so it cannot match a longer id that begins with yours. `--limit 1000` matters because `gh pr list` fetches 30 by default and says nothing when it stops. If a different PR overlaps, surface both references to the user before building over it.
   - Don't paper over ambiguity with assumptions. Confirm scope first.

   Do as much obvious investigation as you can before asking the user. Present them what you found, not "I don't know what to do."

3. **Set up the workspace.**
   - Work in this checkout if the ticket's lane is claimed here, or if this is a worktree holding no other work. Otherwise make a new worktree off the up-to-date trunk and work there.
   - If you are already on this ticket's branch, stay on it.
   - Otherwise fetch first (`git fetch origin`) and cut the branch from the up-to-date trunk, never from wherever HEAD sits. Finishing a ticket leaves you on its branch, and branching from there forks the new ticket off unmerged work.
   - Before creating a branch, search for one that already exists, since a previous session may have made it:

     ```
     git branch --all --list '*<id>*' --format='%(refname:short)'
     ```

     This prints nothing when there is no match, so read the output, not the exit status. A bare name is a local branch: check it out. A name seen both locally and as `origin/<name>` is one branch: use the local one. Only `origin/<name>` means it was pushed from elsewhere: `git checkout -b <name> origin/<name>`. Several different matches mean a sibling id collided with yours, so choose deliberately. If checkout fails with "already used by worktree", that worktree holds the branch: go work there, or stop and report. Do not delete the branch and do not cut a second one.
   - When nothing exists, create the branch with the bare ticket id and no slug, so every session that picks this ticket names it the same way:

     ```
     git checkout -b <id>
     ```

   - Confirm the working tree is clean before starting. If it is not, work out whether the changes belong to this ticket, and commit, stash, or discard them accordingly.
   - Run `lit start <id>` from the worktree you will work in. It prints the plan guidance for this ticket. Follow that guidance before making any edit.

`lit workflows` shows where guidance is bound. Each text prints when its moment comes, so there is nothing to read ahead.
