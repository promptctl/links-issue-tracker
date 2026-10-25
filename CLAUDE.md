## One PR per ticket; one release per epic

Each ticket lands as its **own** reviewable PR, and merging a ticket PR cuts
**no** release. A release is cut only when an **epic** is finished, by a
dedicated `chore(release)` PR.

## The changelog is written once, at release

The changelog is a receipt printed at checkout, not a tab kept open through the
cycle. These are the owner's rules:

- **A ticket PR never touches `CHANGELOG.md`.** No entry, no note for later, no
  `## [Unreleased]` section. No part of the changelog is written until a release
  is cut.
- **The release PR writes the release's section whole**, in one pass, and states
  the **new behavior only**. It never describes old behavior.
  - WRONG: "`lit sync` now exits 3 when the remote's schema is newer; it used to
    exit 5."
  - RIGHT: "`lit sync` exits 3 when the remote's schema is newer."
- **Nobody audits the changelog, ever.** No sentence-by-sentence check, no
  verifying its claims against the last tag, no review pass over the notes.
- **One escape hatch.** If you don't like the section you wrote, you may have a
  subagent write it, once. Commit what it returns exactly as returned, however
  bad or good.

Changelog upkeep was costing more time than the code it described. The code is
worth the time; the changelog is not. The rules will be tested at three moments:

- You finish a ticket and think *"the release author will need to know about
  this, so I'll leave a line under Unreleased."* Don't. The release author
  writes from what merged.
- You are cutting a release, spot a sentence that is wrong for someone
  upgrading, and think *"released notes are permanent; checking every claim is
  the responsible thing."* That check is the audit this rule bans. The notes
  ship as written.
- The subagent's section has one sentence you could fix in ten seconds. Fixing
  "just this one" is an edit. Commit it as returned.

For the release steps, the CI mechanism, and the versioning policy, see
[RELEASING.md](RELEASING.md) — the single home for the release procedure.

## Workflows run to their end

Never end a workflow prematurely, and never dictate counter to the workflow in a
handoff.
