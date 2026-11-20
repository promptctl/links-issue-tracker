# A path-addressed CLI: examples

Worked examples for the design in `path-addressed-cli.md`, which is the authority on
every command here. Nothing is implemented; the output shown is what the design says
a terminal will print.

The examples are grounded in 4,319 Claude Code sessions that ran `lit`, 24,223
invocations in all. Where the old command was short, it sits above the new one as
`was:`. The most common session in the transcripts is a plan review, then a worker
taking a ticket from `next` to `done`, so those come first. Shell quoting is as the
design states: a selector, glob, list, or object is single-quoted.

## Reviewing a plan

### Read what a plan reviewer is told before judging

    was: lit quickstart review
    $ lit get /workflows/plan-review/handlers/brief
    /workflows/plan-review/handlers/brief
      on       read */plan   shipped
      where    (none)        shipped
      enabled  true          shipped
      body                   shipped
    Load the laws and judge the whole mechanism at this revision. The reviewer is not
    the plan's author. Record one review with `add {$}/plan/reviews`, naming the
    revision in `of`.

### Read the ticket under review

    was: lit show mock-mock-flu.atb
    $ lit get atb
    $ = mock-mock-flu.atb
    $/title          The mock's token refresh retries on a clock the test controls
    $/status         plan
    $/parent         flu
    $/lane           atb
    $/claim          checkout 7f2e (elsewhere) · 2h ago
    $/body           The refresh path in src/oauth.ts sleeps on the wall clock …
    $/plan           revision 7
    $/plan/verdict   (none)
    $/comments/43s   [claude_7f2e…] revision 7 moves the clock behind the injected Clock port …

    guidance (plan): core first, prove each fact against the real system, record with `add atb/plan/revisions -`, hand it to a reviewer with a fresh context.

### Read only the plan text

    was: lit show orbit-brain-vu2t.abp --field plan > /tmp/abp_break/plan_v4.md
    $ lit get abp/plan/revisions/4 > /tmp/abp_break/plan_v4.md

### Get the revision to name in the review, and see whether a verdict already exists

    was: lit show orbit-brain-q9s8.45d | grep -o 'plan (v[0-9]*, digest [0-9a-f]*' | tail -1
    $ lit get 45d/plan/revisions/count 45d/plan/verdict
    $ = orbit-brain-q9s8.45d
    $/plan/revisions/count  4
    $/plan/verdict          (none)

Revisions are numbered 1, 2, 3 … and never renumbered, so the count is the latest
revision's number, the one `of` names.

### Check whether this plan revision was already reviewed

    was: lit show orbit-brain-vu2t.abp | grep -c "Plan review (digest f2623ef71b5d)"
    $ lit get 'abp/plan/reviews/(of=4)'
    abp/plan/reviews/9pe   fail  of:4  [claude_2eb9…] single-enforcer: the retry budget is checked in two places …

### Read the epic's sibling tickets to see what the plan must fit beside

    was: for t in 0m9 faz 03v 0go w0o; do lit show mock-mock-flu.$t | sed -n '1,/^Epic:/p' | grep -v '^sync:'; done
    $ lit get 'flu.*/title' 'flu.*/status'
    $ = mock-mock-flu
    $.0m9/title    The mock serves streamed responses in recorded chunk order
    $.faz/title    Credential login writes one file, read by every client
    $.03v/title    The mock's request log names the session that sent each call
    $.0go/title    A recorded fixture refuses a request it has no match for
    $.w0o/title    The CLI flag set is parsed once into a typed config
    $.0m9/status   done
    $.faz/status   impl
    $.03v/status   backlog
    $.0go/status   backlog
    $.w0o/status   plan

### Search for earlier tickets on the same mechanism before judging novelty

    was: lit ls --search powerd --status open,in_progress,closed
    $ lit get '(text~"powerd")/status'
    6on/status   plan
    hqi/status   done
    2wd/status   obsolete

### Record a fail with its reasons and the laws they cite

The reasons are the review's `result`, read from stdin; the review is the whole
record, so no separate comment carries them.

    was: lit comment add mock-mock-flu.atb --body-file - <<'REASONS'
    was: lit verdict mock-mock-flu.atb fail --plan b8fa35fac99f… --law no-ambient-temporal-coupling --law nothing-unseen
    $ lit add atb/plan/reviews '{of:7 how:"/laws:law-review" result:- verdict:fail}' <<'RESULT'
    1. no-ambient-temporal-coupling: step 3 retries on setTimeout, outside the injected Clock.
    2. nothing-unseen: the refresh failure is caught in step 4 and logged nowhere.
    RESULT
    + atb/plan/reviews/9k2  fail, of:7
    ⇒ atb/plan/verdict  (none) → fail

### Record a pass verdict

    was: lit verdict rich-joiners-er38.a1h pass --plan cea7cfc9d682… --law dataflow-not-control-flow --law types-are-the-program
    $ lit add a1h/plan/reviews '{of:2 how:"/laws:law-review" result:"dataflow-not-control-flow, types-are-the-program: hold" verdict:pass}'
    + a1h/plan/reviews/b97  pass, of:2
    ⇒ a1h/plan/verdict  (none) → pass

### A review that names no revision is refused

    $ lit add atb/plan/reviews '{how:"/laws:law-review" result:"…" verdict:fail}'
    error (reason=required): atb/plan/reviews needs of, the revision the review judged; nothing written

### The plan changed between reading it and recording the verdict

The reviewer read revision 7. The author added revision 8 while the review ran.

    $ lit add k7w/plan/reviews '{of:7 how:"/laws:law-review" result:"single-enforcer: holds" verdict:pass}'
    + k7w/plan/reviews/c2m  pass, of:7
    ⇒ k7w/plan/verdict  (none) (unchanged): latest revision is 8, last reviewed 7

The review lands as a judgment of revision 7 and derives nothing; revision 8 waits
for a review that names it.

### Confirm the verdict landed

152 of 296 reviewers in one slice wrote a review comment and recorded no verdict.

    $ lit get atb/plan/verdict
    fail

### The plan's author passes their own plan

    $ lit add s4l/plan/reviews '{of:1 how:"/laws:law-review" result:"one-source-of-truth: holds" verdict:pass}'
    + s4l/plan/reviews/q1z  pass, of:1
    ⇒ s4l/plan/verdict  (none) → pass

## Plan revisions and reviews

### Has the current plan been reviewed yet?

Before review, then after a reviewer passes revision 43.

    was: lit show 8ro6 | grep -nE '^plan \(v|reviews of plan'
    $ lit get 2kx/plan/verdict
    (none)
    $ lit get 2kx/plan/verdict
    pass

### List every revision of a plan

    $ lit get 8ro6/plan/revisions
    8ro6/plan/revisions
      1   claude_2eb9…  2026-10-21T03:12Z
      2   claude_2eb9…  2026-10-21T04:40Z
      …
      21  claude_2eb9…  2026-10-24T18:40Z

### Read plan revision 3

    was: lit history 8ro6 --version plan@3
    $ lit get 8ro6/plan/revisions/3
    ## Plan
    1. The watcher derives its target through one builder …

### Diff two plan revisions

    $ diff <(lit get 8ro6/plan/revisions/20) <(lit get 8ro6/plan/revisions/21)
    14c14
    < 3. The cache keys on three positional strings.
    ---
    > 3. The cache keys on a render origin.

### Record a verdict directly instead of adding a review

    $ lit set atb/plan/verdict pass
    error (reason=derived): atb/plan/verdict is derived, the verdict of the last review naming the latest revision; nothing written
      write instead  lit add atb/plan/reviews '{of:7 … verdict:pass}'

### Find plans waiting on a reviewer

    $ lit get '(status=plan plan/revisions/count>0 !plan/verdict)/title'
    8ro6/title   The render cache takes a render origin, not three positional strings
    nlf/title    A level's form is decided once, at parse

### Revise a plan after it passed

    was: lit update 8ro6 --plan-file /tmp/8ro6-ticket/plan.md
    $ lit add 2kx/plan/revisions - < /tmp/2kx-ticket/plan.md
    + 2kx/plan/revisions/43
    ⇒ 2kx/plan/verdict  pass → (none)

### Revise a plan while the ticket is already in impl

    $ lit add 2kx/plan/revisions - < /tmp/2kx-ticket/plan.md
    + 2kx/plan/revisions/44
    ⇒ 2kx/plan/verdict  pass → (none)

The ticket stays in `impl`: a requirement is checked on the transition and never
again. Moving on to review lands, with one warning naming the revision the verdict
was on:

    $ lit set 2kx/status review
    2kx/status  impl → review
      warning review/entry  plan/verdict=pass   verdict is empty; plan revised since the review of revision 43
      requirements: 1 evaluated · 0 met · 1 unmet (warning) · 0 unmet (error)

    guidance (review): when review is done and the merge is under way, `set 2kx/status merge`.

### My review lands on a revision I did not read

I read revision 21; the author added revision 22 while I was reviewing.

    was: lit verdict 8ro6 pass --plan f76132367de4b85c… --law one-source-of-truth   (refused: stale digest)
    $ lit add 8ro6/plan/reviews '{of:21 how:"/laws:law-review" result:"one-source-of-truth: holds" verdict:pass}'
    + 8ro6/plan/reviews/81f  pass, of:21
    ⇒ 8ro6/plan/verdict  (none) (unchanged): latest revision is 22, last reviewed 21
    $ lit get 8ro6/plan/revisions/22 > /tmp/8ro6-review/plan_r22.md
    $ lit add 8ro6/plan/reviews '{of:22 how:"/laws:law-review" result:"one-source-of-truth: holds" verdict:pass}'
    + 8ro6/plan/reviews/82a  pass, of:22
    ⇒ 8ro6/plan/verdict  (none) → pass
## Working a ticket, start to done

### Find the next ticket

    was: lit next
    $ lit get /next
    s4l   /lanes/history   rank 1   backlog
      set s4l/status plan claims /lanes/history

### See the whole lane before starting it

    $ lit get /lanes/history
    /lanes/history  rank 1
      s4l   backlog
      7ka   backlog
      m3d   backlog

### Start it

    was: lit start orange-history-4y3.s4l
    $ lit set s4l/status plan
    s4l/status  backlog → plan
    ⇒ s4l/claim = this checkout
    ⇒ /lanes/history  held by this checkout
      requirements: 5 evaluated · 5 met · 0 unmet (warning) · 0 unmet (error)

    guidance (plan): core first, prove each fact against the real system, record with `add s4l/plan/revisions -`, hand it to a reviewer with a fresh context.

### Start a ticket that is its own lane

    was: lit start brandon-menus-sjhy.ju8
    $ lit set ju8/status plan
    ju8/status  backlog → plan
    ⇒ ju8/claim = this checkout
      requirements: 5 evaluated · 5 met · 0 unmet (warning) · 0 unmet (error)

    guidance (plan): core first, prove each fact against the real system, record with `add ju8/plan/revisions -`, hand it to a reviewer with a fresh context.

### Starting a ticket behind another in its lane is refused

    $ lit set fw9/status plan
    error (reason=gate): fw9/status backlog → plan refused by backlog/exit; nothing written
      block  !blockers (ready)   fw9/blockers = [b64], ahead of it in /lanes/datetime

### Write the plan from stdin

    was: lit update orange-history-4y3.s4l --plan-file /tmp/s4l-plan.md
    $ lit add s4l/plan/revisions - <<'EOF'
    1. Move the history query behind HistoryStore (src/history/store.ts).
    2. …
    Proof: test/history.test.ts covers an empty store and a 10k-row store.
    EOF
    + s4l/plan/revisions/1

### Revise the plan after a fail

    was: lit update brandon-unfiled-3248.8ro6 --plan-file /tmp/8ro6-ticket/plan.md && lit show brandon-unfiled-3248.8ro6 | grep -nE '^plan \(v|reviews of plan'
    $ lit add 8ro6/plan/revisions - < /tmp/8ro6-ticket/plan.md
    + 8ro6/plan/revisions/22
    ⇒ 8ro6/plan/verdict  fail → (none)

### Read the previous plan revision to diff against

    was: lit history brandon-unfiled-3248.8ro6 --version plan@21
    $ lit get 8ro6/plan/revisions/21 > /tmp/8ro6-ticket/plan.r21.md

### Move to impl once the plan has passed

    was: lit phase orange-history-4y3.s4l --to impl
    $ lit set s4l/status impl
    s4l/status  plan → impl
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (impl): when the PR opens, `set s4l/status review`.

### Moving to impl with no verdict is refused

    $ lit set q3t/status impl
    error (reason=gate): q3t/status plan → impl refused by impl/entry; nothing written
      block  plan/verdict=pass   q3t/plan/verdict = (none)

    guidance (plan): core first, prove each fact against the real system, record with `add q3t/plan/revisions -`, hand it to a reviewer with a fresh context.
    guidance (refused impl): the plan has no passing review of its latest revision; have it reviewed, or revise it and have the new revision reviewed.

### Moving to impl on a failed plan is refused

    $ lit set atb/status impl
    error (reason=gate): atb/status plan → impl refused by impl/entry; nothing written
      block  plan/verdict=pass   atb/plan/verdict = fail (revision 7)

    guidance (plan): core first, prove each fact against the real system, record with `add atb/plan/revisions -`, hand it to a reviewer with a fresh context.
    guidance (refused impl): the plan has no passing review of its latest revision; have it reviewed, or revise it and have the new revision reviewed.

### Trying to finish a ticket whose plan was never reviewed

    was: lit done orange-devices-bue.355
    $ lit set 355/status done
    error (reason=not_in_next): 355/status plan → done refused: done is entered only from merge; nothing written

    guidance (plan): core first, prove each fact against the real system, record with `add 355/plan/revisions -`, hand it to a reviewer with a fresh context.

### Moving a backlog ticket straight to merge is refused

    was: lit phase orbit-schema-reversal-u0if.0ca --to merge
    $ lit set 0ca/status merge
    error (reason=not_in_next): 0ca/status backlog → merge refused: merge is entered only from review; nothing written

### Hand the branch to review

    was: lit phase orange-history-4y3.s4l --to review
    $ lit add s4l/pr/revisions 3f9c2a1e
    + s4l/pr/revisions/1
    $ lit set s4l/status review
    s4l/status  impl → review
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (review): when review is done and the merge is under way, `set s4l/status merge`.

### Move to merge after the last review pass

    was: lit phase orange-history-4y3.s4l --to merge
    $ lit set s4l/status merge
    s4l/status  review → merge
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (merge): once merged, `set s4l/status done`.

### Finish it; the lane hands this checkout the next ticket

    was: lit done template-unfiled-792a.dee
    $ lit set dee/status done
    dee/status  merge → done
    ⇒ dee/claim  this checkout → (none)
    ⇒ /lanes/datetime  head is now b64
    ⇒ b64/claim = this checkout
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)

    guidance (done): update the related tickets with what this work taught.

### Finish it with the merge on record

    was: lit done houdini-laws-etg.79h --reason "Merged in #129 (07e20a1). Follow-up: houdini-laws-etg.tva."
    $ lit set 79h/status '{column:done reason:"Merged in #129 (07e20a1). Follow-up: houdini-laws-etg.tva."}'
    79h/status         merge → done
    79h/status/reason  (none) → Merged in #129 (07e20a1). Follow-up: houdini-laws-etg.tva.
    ⇒ 79h/claim  this checkout → (none)
    ⇒ /lanes/scene-load  head is now lbc
    ⇒ lbc/claim = this checkout
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)

    guidance (done): update the related tickets with what this work taught.

### Finish the last ticket in a lane

    $ lit set fw9/status done
    fw9/status  merge → done
    ⇒ fw9/claim  this checkout → (none)
    ⇒ /lanes/datetime/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)

    guidance (done): update the related tickets with what this work taught.

### See what is served after the lane is finished

    $ lit get /next
    bryp   rank 1   backlog
      set bryp/status plan claims bryp

### Starting a ticket whose lane another checkout holds is refused

    was: lit start acme-plan-review-o3mh.avl
    $ lit set avl/status plan
    error (reason=claimed): avl/status backlog → plan refused: avl is the head of /lanes/plan-review, held by checkout 3c7a (elsewhere) · 5m ago; nothing written
      take it over: lit add /lanes/plan-review/claim

### Take the lane over, then start

    was: lit start acme-plan-review-o3mh.avl --take
    $ lit add /lanes/plan-review/claim
    /lanes/plan-review/claim  checkout 3c7a (elsewhere) · 5m ago → this checkout
    ⇒ /lanes/plan-review/contested = checkout 3c7a (elsewhere) · 5m ago
    ⇒ avl/claim = this checkout
    $ lit set avl/status plan
    avl/status  backlog → plan
      requirements: 5 evaluated · 5 met · 0 unmet (warning) · 0 unmet (error)

    guidance (plan): core first, prove each fact against the real system, record with `add avl/plan/revisions -`, hand it to a reviewer with a fresh context.

### Take over a ticket another checkout left in impl

    was: lit start candybar-daemon-health-ajx0.co4 --take
    $ lit add /lanes/daemon-health/claim
    /lanes/daemon-health/claim  checkout 8c1d (elsewhere) · 4h ago → this checkout
    ⇒ /lanes/daemon-health/contested = checkout 8c1d (elsewhere) · 4h ago
    ⇒ co4/claim = this checkout
    $ lit set co4/status review
    co4/status  impl → review
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (review): when review is done and the merge is under way, `set co4/status merge`.

### Check who holds a lane before deciding to take it

    was: lit backlog 2>&1 | grep -n -B2 -A3 -i 'bryp\|claim'
    $ lit get /lanes/plan-review
    /lanes/plan-review  rank 3   claim checkout 3c7a (elsewhere) · 5m ago
      avl   backlog
      bjm   backlog

### Push ticket writes at the end of the session

    was: lit sync push
    $ lit admin sync
    published 14 events · fetched 2 writers

Every write already schedules this sync in the background; running it by hand
is for leaving with nothing unpublished.

## Claims

### Take over a lane whose holder is dead

    was: lit start 9u9 --take
    $ lit add /lanes/ipc/claim --reason "Holder checkout d1a9 is dead: no process has that worktree as cwd, no tmux pane, tree clean."
    /lanes/ipc/claim  checkout d1a9 (elsewhere) · 3h ago → this checkout
    ⇒ /lanes/ipc/contested = checkout d1a9 (elsewhere) · 3h ago
    ⇒ k2w/claim = this checkout
    $ lit set k2w/status review
    k2w/status  impl → review
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (review): when review is done and the merge is under way, `set k2w/status merge`.

The reason is printed in `get k2w/history` beside the takeover. The dead holder
stays named in `contested` until its claim expires.

### A claim that has expired

    $ lit get bci/claim
    (none)
    $ lit get /next
    bci   /lanes/asset-load   rank 1   impl
      add /lanes/asset-load/claim claims /lanes/asset-load

An expired claim is no claim: nothing prints about the holder that was, and the
lane is served like one nobody ever started.

### A contested lane

Two checkouts each started a ticket in px1f before either saw the other.

    was: lit start 9u9   →   claimed here: … contested by stream ufijgjuy …
    $ lit get /lanes/px1f
    /lanes/px1f  rank 2   claim this checkout   contested checkout 3c7a (elsewhere) · 3h ago
      9u9   impl
      xuv   backlog
      31y   backlog

The later start in the merged order holds the claim; the other checkout's
`/next` passes the lane over and says why.

### What next serves when every lane is held elsewhere

    was: lit next
    $ lit get /next
    (none): 3 lanes considered, every head held elsewhere
      /lanes/store-bugs   9r8   held by checkout 3c7a (elsewhere) · 2h ago
      /lanes/checkpoint   l1x   held by checkout f335 (elsewhere) · 40m ago
      /lanes/lm3          7iy   held by checkout 4c9d (elsewhere) · 5m ago

### An urgent ticket interrupts the lane I hold

    $ lit set c44/priority urgent
    c44/priority  normal → urgent
    $ lit get /next
    c44   rank 1   backlog   urgent, above /lanes/datetime held by this checkout
      set c44/status plan claims c44

### A claim on a lane versus on a solo ticket

    $ lit get /lanes/datetime hf4/lane hf4/claim
    /lanes/datetime  rank 4   claim this checkout
      dee   impl
      b64   backlog
      fw9   backlog
    hf4/lane    hf4
    hf4/claim   (none)

### Who am I writing as?

    was: lit whoami   →   unknown command "whoami"
    $ lit get /me
    /me/checkout   this checkout (7f3a…)
    /me/principal  4be1…
    /me/agent      claude_64e3
    /me/tiers      [internal]
## Reading

### Read one ticket

    was: lit show orbit-warnings-2d80.49k
    $ lit get 49k
    $ = orbit-warnings-2d80.49k
    $/title          A warning printed twice in one turn is shown once
    $/status         merge
    $/parent         2d80
    $/lane           49k
    $/claim          this checkout
    $/body           The bar dedupes by text, but two warnings with …
    $/plan           revision 3
    $/plan/verdict   pass
    $/pr             revision 2
    $/pr/verdict     pass

    guidance (merge): once merged, `set 49k/status done`.

### Read one field

    was: lit show acme-plan-review-o3mh --field status
    $ lit get o3mh/status
    done

### Read a few fields of one ticket

    was: lit show brandon-unfiled-3248.9uht --field created_at,updated_at
    $ lit get 9uht/created 9uht/updated
    $ = brandon-unfiled-3248.9uht
    $/created   2026-10-19T22:52:43Z
    $/updated   2026-10-21T04:10:09Z

### Read only the body

    was: lit show houdini-laws-etg.2ka | sed -n '/^description:/,/^Epic:/p'
    $ lit get 2ka/body
    Too-large tool results go whole to a file, and the reply names the path …

### Pull the body out, edit it, write it back

    was: lit show orbit-backlog-3vck.nhy | awk '/^description:/{f=1;next} /^Epic:/{f=0} f' > /tmp/nhy.md; lit update orbit-backlog-3vck.nhy --description-file /tmp/nhy.md
    $ lit get nhy/body > /tmp/nhy.md
    $ lit set nhy/body - < /tmp/nhy.md --reason "done-when made checkable"
    nhy/body  (+3 −2 lines)

### Put back an earlier description

    was: lit history acme-statusline-e9pl.yjx --version description@1 > /private/tmp/yjx-v1.txt
    $ lit get yjx/body/versions/1 | lit set yjx/body - --reason "back to the first description"
    yjx/body  (+6 −5 lines)

### Read only the comments

    was: lit show brandon-unfiled-3248.8ro6 | sed -n '/^comments:/,$p'
    $ lit get 8ro6/comments
    $ = brandon-unfiled-3248.8ro6
    $/comments/43s   [claude_64e3…] Facts from PR #43: the watcher reads …
    $/comments/7kq   [claude_9a1b…] The watcher's path list is built twice; finding 2 of the plan review …
    $/comments/b0e   [claude_9a1b…] Plan revision 43 answers every finding.

### Read one comment to quote it

    was: lit show orange-admin-helper-b2q.8ew | sed -n '/cmt-e28a7f6d/,/cmt-507e36b0/p'
    $ lit get 8ew/comments/e28
    [claude_96d7…] The install path is computed in two places. 1. one-source-of-truth: …

### List an epic's children

    was: lit children candybar-browser-surface-97sw
    $ lit get 97sw/children
    [97sw.1c2, 97sw.d4e, 97sw.k9f, 97sw.p0s]

### See every child's status in one read

    was: lit children candybar-testing-0x6u --columns id,state,blocked,title | grep -v closed
    $ lit get '0x6u.*/status'
    $ = candybar-testing-0x6u
    $.snf/status   impl
    $.1ur/status   done
    $.4bt/status   backlog
    $.yzt/status   done
    $.5bi/status   dup

### Read an epic without its children's bodies

    was: lit show orange-alerts-cjt | sed -n '/^description:/,/^children:/p'
    $ lit get cjt
    $ = orange-alerts-cjt
    $/title      Alerts fire on threshold crossings and say what crossed
    $/level      2
    $/stage      active
    $/rollup     backlog 1 · impl 1 · done 1
    $/body       Alerts today fire on every sample above the line …
    $/children   [cjt.3ns, cjt.7pq, cjt.v2a]
    $/lanes      [cjt/lanes/alerts]

### Read a child without the epic reprinted under it

    was: lit show orbit-testing-c3u.4xm | awk '/^Epic:/{exit} 1'
    $ lit get 4xm
    $ = orbit-testing-c3u.4xm
    $/title      The replay test waits on the brain's own signal, not a sleep
    $/status     backlog
    $/parent     c3u
    $/lane       4xm
    $/body       test/replay.test.ts sleeps 2s and then asserts …

### Read a lane

    $ lit get /lanes/datetime
    /lanes/datetime  rank 4   claim this checkout
      dee   impl
      b64   backlog
      fw9   backlog

### Read several tickets in one call

    was: lit show orange-devices-bue.1e6; lit show orange-devices-bue.chz; lit show orange-devices-bue.hqi
    $ lit get 1e6 chz hqi
    $ = orange-devices-bue
    $.1e6/title    The device list shows a sweep in progress
    $.1e6/status   done
    …
    $.chz/status   backlog
    …
    $.hqi/title    …
    …

### Read the same fields across several tickets

    was: lit ls --ids houdini-laws-etg.0yy,houdini-laws-etg.y04 --columns id,state,assignee,updated_at --format table
    $ lit get 0yy/status 0yy/claim 0yy/updated y04/status y04/claim y04/updated
    $ = houdini-laws-etg
    $.0yy/status    impl
    $.0yy/claim     checkout 4f1c (elsewhere) · 2h ago
    $.0yy/updated   2026-10-14T23:02:51Z
    $.y04/status    backlog
    $.y04/claim     (none)
    $.y04/updated   2026-10-12T08:40:17Z

### Check the status after a write

The write's echo already says it; this is the read a later session makes.

    was: lit done orbit-warnings-2d80.49k; lit show orbit-warnings-2d80.49k | grep -E "^(status|phase):"
    $ lit get 49k/status
    done

### Read what blocks a ticket

    was: lit show orange-devices-bue.2af 2>&1 | grep -A3 blocked_by
    $ lit get 2af/blocked_by
    [rec]

### Read the column graph a ticket resolves to

    was: lit workflows show orbit-brain-q9s8.45d
    $ lit get 45d/columns
    45d/columns  /columns
      backlog   initial:true  next:[plan]  exit:[ready]
      plan      next:[impl]
      impl      next:[review]  entry:[plan/verdict=pass]
      review    next:[merge]  entry:[{test:plan/verdict=pass severity:warning}]
      merge     next:[done]  entry:[pr/verdict=pass]
      …

### See who claimed a ticket and when

The claim is the lane's, taken by the move out of `backlog`; the history line that made the move names who and when.

    was: lit history houdini-laws-etg.6op 2>&1 | tail -8
    $ lit get 6op/history
    $ = houdini-laws-etg.6op
    $/history/h2k   2026-10-15T05:11Z   claude_214f…   status  backlog → plan
    $/history/h2n   2026-10-15T05:40Z   claude_214f…   plan/revisions + 1
    $/history/h3c   2026-10-15T09:02Z   claude_8e0c…   plan/reviews + c71  pass, of:1

### Find claims nobody has touched in half a day

    was: lit orphaned
    $ lit get '(status=plan updated<-12h)/claim'
    etg.bci/claim   checkout 5d2e (elsewhere) · 14h ago

## Reading many tickets in one call

### Title of every ticket, to see the backlog without 59 shows
The grooming pass ran one `show` per ticket and got ~270 KB back. Most of it was epic bodies, reprinted once per child.

    was: for id in $(lit ls --columns id | awk '{print $1}'); do lit show $id; done
    $ lit get '*/title'
    792a/title       Unfiled
    792a.dee/title   dateInZone accepts zone names in any letter case
    792a.b64/title   dateInZone accepts fixed offsets like +05:30
    792a.fw9/title   date parses RFC 3339 with fractional seconds
    rin/title        Subtemplates
    rin.2ae/title    define and template share one namespace
    …

### Status of every ticket
A node above level 1 has no `status`, so the epics print no line here.

    was: for id in …; do lit show $id | grep '^status:'; done
    $ lit get '*/status'
    792a.dee/status   impl
    792a.b64/status   backlog
    792a.fw9/status   backlog
    rin.2ae/status    review
    rin.9u9/status    done
    …

### Title, status, and lane of every ticket, one line per field
    $ lit get '*/title' '*/status' '*/lane'
    792a.dee/title    dateInZone accepts zone names in any letter case
    792a.dee/status   impl
    792a.dee/lane     datetime
    792a.b64/title    dateInZone accepts fixed offsets like +05:30
    792a.b64/status   backlog
    792a.b64/lane     datetime
    rin.2ae/title     define and template share one namespace
    rin.2ae/status    review
    rin.2ae/lane      2ae
    …

### Status of three tickets in another checkout's lane
    was: for t in 5tv zi4 8l0; do lit show dy46.$t | sed -n '1,12p'; done
    $ lit get dy46.5tv/status dy46.zi4/status dy46.8l0/status
    $ = acme-scale-dy46
    $.5tv/status   backlog
    $.zi4/status   impl
    $.8l0/status   backlog

### Status of every child of an epic
    was: lit children candybar-testing-0x6u --columns id,state,blocked,title | grep -v closed
    $ lit get '0x6u.*/status'
    $ = candybar-testing-0x6u
    $.snf/status   impl
    $.1ur/status   done
    $.4bt/status   backlog
    $.yzt/status   done
    $.5bi/status   dup

### Bodies of four tickets, without the epic tail
    was: for t in 2ka 4id y04 0yy; do lit show etg.$t | sed -n '/^description:/,/^Epic:/p'; done
    $ lit get etg.2ka/body etg.4id/body etg.y04/body etg.0yy/body
    $ = houdini-laws-etg
    $.2ka/body   Too-large tool results go whole to a file …
    $.4id/body   find_error_nodes skips summarization when …
    $.y04/body   The scene loader reports a missing asset as …
    $.0yy/body   Connections time out after 30 s with no …

### Head of one ticket: title, status, body, what blocks it
Several paths on one ticket print the ticket once, as `$`.

    was: lit show dy46.5tv | sed -n '1,12p;/^description:/,/^blocked_by:/p'
    $ lit get dy46.5tv/title dy46.5tv/status dy46.5tv/blocked_by dy46.5tv/body
    $ = acme-scale-dy46.5tv
    $/title        Backlog reads stay under 200 ms at 10k tickets
    $/status       backlog
    $/blocked_by   [bryp]
    $/body         The backlog query scans every issue_history row …

### Comments on one ticket, without the rest of the ticket
    was: lit show 3248.8ro6 | sed -n '/^comments:/,$p' | cut -c1-600
    $ lit get 3248.8ro6/comments
    $ = brandon-unfiled-3248.8ro6
    $/comments/43s   [claude_64e3…] Facts from PR #43: the watcher reads …
    $/comments/7kq   [claude_9a1b…] The watcher's path list is built twice; finding 2 of the plan review …
    $/comments/b0e   [claude_9a1b…] Plan revision 43 answers every finding.

### One comment, to quote it in a follow-up
    was: lit show b2q.8ew | sed -n '/cmt-e28a7f6d/,/cmt-507e36b0/p'
    $ lit get b2q.8ew/comments/e28
    [claude_96d7…] The install path is computed in two places. 1. one-source-of-truth: …

### Every epic's body, to audit the epics before decomposing them
    was: for e in $(lit ls --type epic --columns id | awk '{print $1}'); do lit show $e > /tmp/groom/epic-$e.txt; done
    $ lit get '(level=2)/body' > /tmp/groom/epics.txt
    $ grep -c '' /tmp/groom/epics.txt
    41

### Titles of the children of five epics
    was: for e in 6pj 1dh f00 5d8 b2l; do lit ls --parent 76bb.$e; done
    $ lit get '76bb.6pj.*/title' '76bb.1dh.*/title' '76bb.f00.*/title' '76bb.5d8.*/title' '76bb.b2l.*/title'
    $ = candybar-gather-76bb
    $.6pj.7q7/title   The gather renderer draws one row per source
    $.6pj.e1n/title   A source with no samples draws an empty row
    $.1dh.mxk/title   …
    …

### Parent of each ticket, before filing a sibling
    was: for t in …; do lit show $t | grep -A2 '^parent:' | sed -n 2p; done
    $ lit get 02f/parent dye.73t/parent dye.8yk/parent
    02f/parent       /
    dye.73t/parent   dye
    dye.8yk/parent   dye

### Created and updated times of one ticket
    was: lit show 3248.9uht --field created_at,updated_at
    $ lit get 3248.9uht/created 3248.9uht/updated
    $ = brandon-unfiled-3248.9uht
    $/created   2026-10-19T22:52:43Z
    $/updated   2026-10-21T04:10:09Z

### Every unfinished ticket's body in one file, to grep
    was: for id in $(lit ls --status open,in_progress --columns id | awk '{print $1}'); do lit show $id --no-plan > /tmp/groom/t-$id.txt; done
    $ lit get '(level=1 stage!=terminal)/body' > /tmp/groom/open-bodies.txt
    $ grep -n 'backoff' /tmp/groom/open-bodies.txt
    212:3k6u.ckq/body   … retries with exponential backoff capped at 30 s …

### History of every ticket carrying a label
    was: for id in $(lit ls --labels focus …| awk '{print $1}'); do lit history "$id" | grep -i -m1 label; done
    $ lit get '(labels=focus)/history'
    3k6u.knq/history/k4w   2026-10-20T22:52Z   claude_214f…   labels  [] → [focus]
    …
    dy46.48k/history/m9p   2026-10-22T03:11Z   claude_96d7…   labels  [external] → [external, focus]
    …

## Selectors

### Open bugs, by title
    was: lit ls --status open --type bug
    $ lit get '(status=backlog labels=bug)/title'
    3248.kler/title   daemon.log is ~90% 'schema: unknown field' lines
    xu4.8pq/title     A connected device sweeps with an empty name

### Every epic
    was: lit ls --type epic
    $ lit get '(level=2)/title'
    792a/title   Unfiled
    rin/title    Subtemplates
    6fq5/title   Theme colours
    …

### Children of an epic, by selector or by glob
`(parent=rin)` and `rin.*` expand to the same ids when every child sits directly under `rin`.

    was: lit ls --parent go-template-js-rin
    $ lit get '(parent=rin)/title'
    $ = go-template-js-rin
    $.2ae/title   define and template share one namespace
    $.9u9/title   block falls back to its own body when undefined
    $.k3x/title   template with no pipeline passes nil dot

### Dedupe before filing: has anyone already filed this?
Reviewers searched two or three terms before writing a ticket.

    was: lit ls --search powerd --status open,in_progress,closed; lit ls --search sweep --status open,in_progress,closed
    $ lit get '(text~"powerd")/status' '(text~"sweep")/status'
    $ = orange-devices-bue
    $.6on/status   plan
    $.1e6/status   done
    $.chz/status   backlog

### Dedupe among bugs, before filing one
    $ lit get '(text~"empty name" labels=bug)/title'
    xu4.8pq/title   A connected device sweeps with an empty name

### Orphaned: in flight, no update in six hours
`stage` is derived from the two flags the engine knows, so "in flight" is one key in every subtree's column graph.

    was: lit orphaned
    $ lit get '(stage=active updated<-6h)/claim'
    etg.bci/claim   checkout 5d2e (elsewhere) · 14h ago

### Tickets touched since yesterday
    was: lit ls --updated-after 2026-11-02
    $ lit get '(updated>2026-11-02)/updated'
    792a.dee/updated   2026-11-02T19:40:11Z
    rin.2ae/updated    2026-11-03T03:02:55Z

### Tickets that have comments
    was: lit ls --query 'has:comments'
    $ lit get '(comments/count>0)/title'
    3248.8ro6/title   Watchers derive their targets through one builder
    bue.6on/title     powerd sleeps the sweep during a call
    …

### First five finished tickets with comments, in rank order
    was: lit ls --query 'status:closed type:task has:comments sort:rank:asc limit:5'
    $ lit get '(level=1 stage=terminal comments/count>0)/title' --limit 5
    792a.2ku/title   dateInZone rejects a malformed offset
    792a.207/title   date accepts a Unix timestamp string
    rin.9u9/title    block falls back to its own body when undefined
    bue.1e6/title    The device list shows a sweep in progress
    0x6u.yzt/title   The replay harness reads fixtures from one directory

### Tickets closed as obsolete
    was: lit ls --ids hands-nativeui-02f,hands-install-dye --include-archived
    $ lit get '(status=obsolete)/title' '(status=obsolete)/status/reason'
    $ = hands-voice-r7y.8rr
    $/title           Wake word survives a sleep/wake cycle
    $/status/reason   Wake word dropped from the voice plan

### Restore an epic deleted in error
`rm x` is `set x/deleted true`; the inverse is the inverse.

    was: lit restore vhid-aim-sjt
    $ lit set sjt/deleted false --reason "deleted in error"
    sjt/deleted  true → false

### Every ticket in review, at every depth, in each subtree's own column graph
`rin` adds an `audit` column. `(status=review)` still matches its review tickets, because a selector matches each ticket's resolved column.

    $ lit get '(status=review)/title'
    rin.2ae/title      define and template share one namespace
    3248.8ro6/title    Watchers derive their targets through one builder

### Backlog tickets, without the epics
A node above level 1 has no `status`, so a `status` selector matches tickets only.

    was: lit ls --status open | grep -v epic
    $ lit get '(status=backlog)/title'
    792a.b64/title   dateInZone accepts fixed offsets like +05:30
    792a.fw9/title   date parses RFC 3339 with fractional seconds
    …

### A named set of unrelated ids, two fields each
    was: lit ls --ids etg.0yy,etg.y04 --columns id,state,assignee,updated_at
    $ lit get etg.0yy/status etg.0yy/claim etg.y04/status etg.y04/claim
    $ = houdini-laws-etg
    $.0yy/status   impl
    $.0yy/claim    checkout 4f1c (elsewhere) · 2h ago
    $.y04/status   backlog
    $.y04/claim    (none)

## Comments

### Leave a short note

    was: lit comment add orange-widgets-5jo.nlf --body "From PR #49 review: decide whether a level's form …"
    $ lit add nlf/comments "From PR #49 review: decide whether a level's form …"
    + nlf/comments/k2m

### Leave a long note from a heredoc

    was: lit comment add candybar-animation-wbxz.81o.2jn --body-file - <<'EOF'
    $ lit add 81o.2jn/comments - <<'EOF'
    Answered: stop() kills the child. On 2.1.299 the child outlives the mod by one tick …
    EOF
    + 81o.2jn/comments/p3d

### Remove a comment

    was: lit comment rm cmt-563ec2d8-…
    $ lit rm b06/comments/563
    b06/comments/563/deleted  false → true

### Remove two stale comments in one call

    was: lit comment rm cmt-2f277e42-… && lit comment rm cmt-f5f02379-…
    $ lit rm 03v/comments/2f2 03v/comments/f5f --reason "superseded by the merged plan"
    03v/comments/2f2/deleted  false → true
    03v/comments/f5f/deleted  false → true

### Correct an earlier comment by appending a new one

    was: lit comment add mock-mock-flu.3h8 --body "Correction to cmt-03d4a989: …"
    $ lit add 3h8/comments "Correction to 3h8/comments/03d: the credential file is src/oauth.ts, not src/login.ts."
    + 3h8/comments/v6n

### Leave a note on a ticket this session does not own

    was: lit comment add mock-mock-flu.aus --body "From mock-mock-flu.86x: the credential login writes is src/oauth.ts …"
    $ lit add aus/comments "From @flu.86x: the credential login writes is src/oauth.ts …"
    + aus/comments/j1t

## Notes between agents

### Only one author's comments

    $ lit get '8ew/comments/(by=claude_96d7)/body'
    $ = orange-admin-helper-b2q.8ew
    $/comments/e28/body   The install path is computed in two places …
    $/comments/507/body   Revision 5 reads it from one place; finding 1 is answered.
## Filing

### File a bug under an epic, body from stdin

    was: lit new --type bug --topic devices --parent orange-devices-bue --title "A connected device sweeps with an empty name" --description-file - <<'EOF'
    $ lit add bue/children '{title:"A connected device sweeps with an empty name" labels:[bug] body:-}' <<'EOF'
    Repro: pair a device with no name set; the sweep row renders "" …
    EOF
    + bue.k7r
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### File a task with a one-line body

    was: lit new --parent orbit-evals-bgz --type task --topic isolation --title "The Opus verification reads what the session runs" --description "…"
    $ lit add bgz/children '{title:"The Opus verification reads what the session runs, not what the model says it is" body:"Done looks like: …"}'
    + bgz.10
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### File three children in one call

    was: for t in one two three; do lit new --parent $E --topic wake-word --title "ticket $t"; done
    $ lit add i15t/children '[{title:"Wake word fires on the first syllable"}, {title:"Wake word ignores the TV"}, {title:"Wake word logs each miss"}]'
    + i15t.r2a
    + i15t.r2b
    + i15t.r2c
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### File a follow-up beside a closed ticket, in its epic

    was: lit followup --on orange-settings-7du.5uw --title "The app quit by itself twice right after the Settings window opened" --type bug
    $ lit get 5uw/parent
    7du
    $ lit add 7du/children '{title:"The app quit by itself twice right after the Settings window opened" labels:[bug]}'
    + 7du.a2c
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### File an epic

    was: lit new --type epic --topic history --title "History reads come from one store"
    $ lit add /children '{level:2 title:"History reads come from one store"}'
    + m2q
    ⇒ m2q/topic = history
    ⇒ m2q/parent = unfiled
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### File a bug with no epic

    was: lit new --type bug --topic daemon-git-identity --title "A daemon serving sessions …"
    $ lit add /children '{level:1 title:"A daemon serving sessions commits as the wrong identity" labels:[bug]}'
    + k3m
    ⇒ k3m/topic = daemon
    ⇒ k3m/parent = unfiled
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

A ticket created without a parent lands in the home of the level above it, so every ticket has an epic to move out of with `set k3m/parent <epic>`.

## Bulk create

### Create an epic's tickets from one list on stdin
    was: lit import --path /tmp/w0o-groom/tickets.yaml
    $ lit add flu/children - <<'EOF'
    {title:"Login writes the credential where the refresh reads it" labels:[bug]}
    {title:"The refresh retries once on a 401, then surfaces it"}
    {title:"Token expiry is read from the token, not assumed"}
    EOF
    + flu.8xa
    + flu.8xb
    + flu.8xc
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### Create with bodies and labels, values quoted where they contain a colon
The YAML import failed on `mapping values are not allowed`. In a value, a colon inside quotes is text.

    $ lit add cb/children - <<'EOF'
    {title:"Vocab: one word per gauge" labels:[refine] body:"Done when: every gauge label is one word."}
    {title:"Vocab: units in the legend, not the label" labels:[refine]}
    EOF
    + cb.v1a
    + cb.v1b
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### Dry-run a bulk create to check every item before minting ids
    $ lit add 0x6u/children - --dry-run < /tmp/epic-tickets.list
    error (reason=invalid_value): 0x6u/children item 3: label bugg is not declared; nothing written

### Create a new epic and its children in one call
    was: lit import --path /tmp/ccb-tickets/candybar.yaml   (created 18 issues / tmux-epic -> candybar-tmux-events-y457)
    $ lit add /children '{level:2 title:"tmux server events"
      children:[{title:"Parse %output notifications"},
                {title:"Reconnect after the server restarts"}]}'
    + y457
    ⇒ y457/topic = tmux-events
    ⇒ y457/parent = unfiled
    + y457.a1
    + y457.a2
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### Create tickets that depend on each other in one call
The YAML import used local keys (`plan-1 -> candybar-testing-ihvy.myy`) so one new ticket could block another. Inside one `add`, `@N` names the Nth object the value declares.

    $ lit add ihvy/children '[{title:"Write the test plan" blocks:[@2]}, {title:"Run the plan against the fake store"}]'
    + ihvy.myy
    + ihvy.myz
    + ihvy.myy/blocks/ihvy.myz
    ⇒ ihvy.myz/blocked_by  [] → [ihvy.myy]
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### Update many existing tickets from one list
The import help said a document with an `id` updates that ticket. Updating is `set`, with pairs.

    was: lit import --path /tmp/groom/lanes.yaml   (after two failed attempts to generate the YAML)
    $ lit set px1f.f9b/priority urgent px1f.9u9/priority urgent px1f.xuv/parent lq3
    px1f.f9b/priority   normal → urgent
    px1f.9u9/priority   normal → urgent
    px1f.xuv/parent     px1f → lq3

## Closing

### Close a merged ticket in a close-out session

    was: lit done orbit-warnings-2d80.49k --reason "PR #332 merged as f4cfaa0"
    $ lit set 49k/status '{column:done reason:"PR #332 merged as f4cfaa0"}'
    49k/status         merge → done
    49k/status/reason  (none) → PR #332 merged as f4cfaa0
    ⇒ /lanes/49k/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (done): update the related tickets with what this work taught.

### Close several merged tickets at once

    was: for t in 2ku 207 oq5; do lit done template-unfiled-792a.$t; done
    $ lit set 2ku/status done 207/status done oq5/status done
    2ku/status  merge → done
    ⇒ /lanes/2ku/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    207/status  merge → done
    ⇒ /lanes/207/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    oq5/status  merge → done
    ⇒ /lanes/oq5/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (done): update the related tickets with what this work taught.

### Close as won't-fix, with the reason

    was: lit close houdini-install-aby.80r --resolution wontfix --reason "…"
    $ lit set 80r/status '{column:wontfix reason:"Installing into the Houdini prefs is for the user to do; the README says how."}'
    80r/status         backlog → wontfix
    80r/status/reason  (none) → Installing into the Houdini prefs is for the user to do; the README says how.
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (wontfix): update the related tickets with what this work taught.

### Won't-fix with no reason is refused

    $ lit set 80r/status wontfix
    error (reason=gate): 80r/status backlog → wontfix refused by wontfix/entry; nothing written
      block  status/reason   80r/status/reason = (none)
    guidance (backlog): A ticket leaves the backlog once it says what to build and someone has put its epic in order and marked it ready. …

### Close as obsolete, with the reason

    was: lit close orbit-hierarchy-6cqf.2la --resolution obsolete --reason "groom: its work (a migration dropping issue_type) is ruled out by …"
    $ lit set 2la/status '{column:obsolete reason:"groom: its work (a migration dropping issue_type) is ruled out by …"}'
    2la/status         backlog → obsolete
    2la/status/reason  (none) → groom: its work (a migration dropping issue_type) is ruled out by …
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (obsolete): update the related tickets with what this work taught.

### Close as a duplicate of the canonical ticket

    was: lit close orbit-warnings-2d80.d66 --resolution duplicate --of orbit-brain-lq3.9od
    $ lit set d66/status '{column:dup of:lq3.9od}'
    d66/status     backlog → dup
    d66/status/of  (none) → lq3.9od
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (dup): update the related tickets with what this work taught.

### A duplicate with no canonical is refused

    $ lit set d66/status dup
    error (reason=gate): d66/status backlog → dup refused by dup/entry; nothing written
      block  status/of   d66/status/of = (none)
    guidance (backlog): A ticket leaves the backlog once it says what to build and someone has put its epic in order and marked it ready. …

### Close three duplicates into one canonical

    was: for t in d66 c7e 61x; do lit close orbit-warnings-2d80.$t --resolution duplicate --of orbit-brain-lq3.9od; done
    $ lit set d66/status '{column:dup of:lq3.9od}' c7e/status '{column:dup of:lq3.9od}' 61x/status '{column:dup of:lq3.9od}'
    d66/status     backlog → dup
    d66/status/of  (none) → lq3.9od
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    c7e/status     backlog → dup
    c7e/status/of  (none) → lq3.9od
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    61x/status     backlog → dup
    61x/status/of  (none) → lq3.9od
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (dup): update the related tickets with what this work taught.

### Close as superseded by the ticket that replaced it

    was: lit close houdini-laws-etg.2ka --resolution superseded --of houdini-laws-etg.6op --reason "user decided: too-large results go whole to a file …"
    $ lit set 2ka/status '{column:superseded of:6op reason:"user decided: too-large results go whole to a file …"}'
    2ka/status         backlog → superseded
    2ka/status/of      (none) → 6op
    2ka/status/reason  (none) → user decided: too-large results go whole to a file …
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (superseded): update the related tickets with what this work taught.

### Close an abandoned epic's backlog children with one reason, previewed first

    was: lit close dotfiles-iterm2-restore-5k5.4.1 --resolution obsolete; lit close dotfiles-iterm2-restore-5k5 --resolution obsolete
    $ lit set '(parent/parent=5k5 status=backlog)/status' '{column:obsolete reason:"iTerm2 restore dropped"}' --dry-run
    (parent/parent=5k5 status=backlog) = [5k5.4.1, 5k5.4.2]
    5k5.4.1/status         backlog → obsolete
    5k5.4.1/status/reason  (none) → iTerm2 restore dropped
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    5k5.4.2/status         backlog → obsolete
    5k5.4.2/status/reason  (none) → iTerm2 restore dropped
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (obsolete): update the related tickets with what this work taught.
    (dry run: nothing written)

The epic has no status to close. Its stage is terminal once its last child's is.

### Reopen a closed ticket

    was: lit open sanctuary-render-tgx.rkm --reason "Done-when fails: the bump has not landed"
    $ lit set rkm/status backlog --reason "Done-when fails: the bump has not landed"
    rkm/status         done → backlog
    rkm/status/reason  Merged in #212 → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (backlog): A ticket leaves the backlog once it says what to build and someone has put its epic in order and marked it ready. …

### Park an in-flight ticket back in backlog and release its claim

    was: lit open acme-scale-dy46.37o --reason "parked behind orbit-sync-bryp"
    $ lit set 37o/status backlog --reason "parked behind orbit-sync-bryp"
    37o/status  impl → backlog
    ⇒ /lanes/37o/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (backlog): A ticket leaves the backlog once it says what to build and someone has put its epic in order and marked it ready. …

## Labels, parents, dependencies

### Add a label

    was: lit label add acme-perf-kw6z.5 external
    $ lit add kw6z.5/labels external
    + kw6z.5/labels/external

### Remove a label, then start the ticket

    was: lit label rm candybar-effort-gauge-9to external && lit start candybar-effort-gauge-9to
    $ lit rm 9to/labels/external
    - 9to/labels/external
    $ lit set 9to/status plan
    9to/status  backlog → plan
    ⇒ 9to/claim = this checkout
      requirements: 5 evaluated · 5 met · 0 unmet (warning) · 0 unmet (error)
    guidance (plan): core first, prove each fact against the real system, record with `add 9to/plan/revisions -`, hand it to a reviewer with a fresh context.

### Clear the label from the epics the pass finished

    was: for e in orange-history-4y3 orange-devices-bue orange-iphone-watch-lm3; do lit label rm $e decompose; done
    $ lit rm 4y3/labels/decompose bue/labels/decompose lm3/labels/decompose
    - 4y3/labels/decompose
    - bue/labels/decompose
    - lm3/labels/decompose

### Move a ticket under a different epic

    was: lit parent set --child acme-unfiled-fc79.rm9w --parent orbit-testing-0oqd
    $ lit set rm9w/parent 0oqd
    rm9w/parent  fc79 → 0oqd

### Move a ticket under an epic that carries its own columns

    $ lit set q4p/parent lq3
    q4p/parent  unfiled → lq3
    ⇒ q4p/columns  /columns → lq3/columns

### Move three tickets under one epic

    was: for t in bz6 3cz ths; do lit parent set --child tmux-pane-detail-aha.$t --parent tmux-capture-restore-99o; done
    $ lit set bz6/parent 99o 3cz/parent 99o ths/parent 99o
    bz6/parent  aha → 99o
    3cz/parent  aha → 99o
    ths/parent  aha → 99o

### Make a ticket wait on one in another epic

    was: lit dep add acme-scale-dy46.37o --blocked-by orbit-sync-bryp
    $ lit add bryp/blocks 37o
    + bryp/blocks/37o
    ⇒ 37o/blocked_by  [] → [bryp]

### Make five tickets wait on one

    was: for t in 0jy 04u vap 73s 7v5; do lit dep add houdini-scene-iteration-jcj.$t --blocked-by houdini-scene-iteration-jcj.l1e; done
    $ lit add l1e/blocks '[0jy, 04u, vap, 73s, 7v5]'
    + l1e/blocks/0jy
    ⇒ 0jy/blocked_by  [] → [l1e]
    + l1e/blocks/04u
    ⇒ 04u/blocked_by  [] → [l1e]
    + l1e/blocks/vap
    ⇒ vap/blocked_by  [] → [l1e]
    + l1e/blocks/73s
    ⇒ 73s/blocked_by  [] → [l1e]
    + l1e/blocks/7v5
    ⇒ 7v5/blocked_by  [] → [l1e]

### Make one ticket wait on a sibling in the same epic

    was: lit dep add --from copirate-efficiency-235.1 --to copirate-efficiency-235.2 --type blocks
    $ lit add 235.1/blocks 235.2
    + 235.1/blocks/235.2
    ⇒ 235.2/blocked_by  [] → [235.1]

### Remove a dependency

    was: lit dep rm houdini-connections-ob5 --blocked-by houdini-scene-iteration-jcj.l1e
    $ lit rm l1e/blocks/ob5
    - l1e/blocks/ob5
    ⇒ ob5/blocked_by  [l1e] → []

## Several fields on one ticket

### Title, body, and labels in one call
    was: lit update e9pl.9ym --title "…" --type task --description-file -
    $ lit set e9pl.9ym/title "The statusline names the lane it serves" e9pl.9ym/labels '[]' e9pl.9ym/body - <<'EOF'
    The statusline shows the lane's head and its position (2/3).
    EOF
    e9pl.9ym/title   The statusline shows rank → The statusline names the lane it serves
    e9pl.9ym/labels  [bug] → []
    - e9pl.9ym/labels/bug
    e9pl.9ym/body    (+1 −6 lines)

### Replace a ticket's labels, seeing what was removed
    was: lit label rm 9to external && lit label add 9to needs-design
    $ lit set 9to/labels '[needs-design]'
    9to/labels   [external] → [needs-design]
    - 9to/labels/external

### Parent and priority together
    $ lit set rm9w/parent 0oqd rm9w/priority urgent
    rm9w/parent     fc79 → 0oqd
    rm9w/priority   normal → urgent

## Bulk writes, dry run first

### Set priority on every CI-filed alarm, shown before it is done
    was: for id in z6sv.dbc z6sv.463 z6sv.bd3 z6sv.ix9 z6sv.86e; do lit update $id --priority normal --reason "groom: …"; done
    $ lit set '(parent=z6sv priority=urgent)/priority' normal --dry-run
    (parent=z6sv priority=urgent) = [z6sv.dbc, z6sv.463, z6sv.bd3, z6sv.ix9, z6sv.86e]
    z6sv.dbc/priority   urgent → normal
    z6sv.463/priority   urgent → normal
    z6sv.bd3/priority   urgent → normal
    z6sv.ix9/priority   urgent → normal
    z6sv.86e/priority   urgent → normal
    (dry run: nothing written)
    $ lit set '(parent=z6sv priority=urgent)/priority' normal --reason "groom: …"
    z6sv.dbc/priority   urgent → normal
    z6sv.463/priority   urgent → normal
    z6sv.bd3/priority   urgent → normal
    z6sv.ix9/priority   urgent → normal
    z6sv.86e/priority   urgent → normal

### Urgent on every ticket of an epic
    was: for id in w1p w1p.1 w1p.2 w1p.3; do lit update macklebox-safety-$id --priority 0; done
    $ lit set 'w1p.*/priority' urgent
    w1p.1/priority   normal → urgent
    w1p.2/priority   normal → urgent
    w1p.3/priority   normal → urgent

### Dependency fan-out: seven tickets wait on one blocker
    was: lit dep add lm3.fyh --blocked-by rec; lit dep add lm3.u33 --blocked-by rec; … (7 calls)
    $ lit add rec/blocks '[lm3.fyh, lm3.u33, lm3.cn0, lm3.7iy, lm3.8pk, lm3.1tt, lm3.497]'
    + rec/blocks/lm3.fyh
    ⇒ lm3.fyh/blocked_by   [] → [rec]
    + rec/blocks/lm3.u33
    ⇒ lm3.u33/blocked_by   [] → [rec]
    …

### One ticket waits on two blockers
    $ lit add 3248.kler/blocks sjhy.ju8 bryp/blocks sjhy.ju8
    + 3248.kler/blocks/sjhy.ju8
    ⇒ sjhy.ju8/blocked_by   [] → [3248.kler]
    + bryp/blocks/sjhy.ju8
    ⇒ sjhy.ju8/blocked_by   [3248.kler] → [3248.kler, bryp]

### Swap the blocker on two tickets
    was: for t in bsog.sfi 7qoh.8nx; do lit dep rm $t --blocked-by uoz0.nx9; lit dep add $t --blocked-by px1f.e4u; done
    $ lit rm uoz0.nx9/blocks/bsog.sfi uoz0.nx9/blocks/7qoh.8nx
    - uoz0.nx9/blocks/bsog.sfi
    ⇒ bsog.sfi/blocked_by   [uoz0.nx9] → []
    - uoz0.nx9/blocks/7qoh.8nx
    ⇒ 7qoh.8nx/blocked_by   [uoz0.nx9] → []
    $ lit add px1f.e4u/blocks '[bsog.sfi, 7qoh.8nx]'
    + px1f.e4u/blocks/bsog.sfi
    ⇒ bsog.sfi/blocked_by   [] → [px1f.e4u]
    + px1f.e4u/blocks/7qoh.8nx
    ⇒ 7qoh.8nx/blocked_by   [] → [px1f.e4u]

### Drop one dependency
    was: lit dep rm houdini-connections-ob5 --blocked-by jcj.l1e
    $ lit rm jcj.l1e/blocks/ob5
    - jcj.l1e/blocks/ob5
    ⇒ ob5/blocked_by   [jcj.l1e] → []

### Reparent twelve tickets
    was: for p in "bij aj0" "o4f aj0" "czk aj0" …; do set -- $p; lit parent set --child flu.$1 --parent flu.$2; done
    $ lit set bij/parent aj0 o4f/parent aj0 czk/parent aj0 w0o/parent aj0
    bij/parent   flu → aj0
    o4f/parent   flu → aj0
    czk/parent   flu → aj0
    w0o/parent   flu → aj0

Here `aj0` is an epic of its own, a level-2 node beside `flu`. A parent is one level up, so the tickets move under another epic, never under a sibling ticket.

### Move every child of one epic under another
    was: for t in aha.bz6 aha.3cz aha.ths; do lit parent set --child $t --parent tmux-capture-restore-99o; done
    $ lit set 'aha.*/parent' 99o --dry-run
    aha.* = [aha.bz6, aha.3cz, aha.ths]
    aha.bz6/parent   aha → 99o
    aha.3cz/parent   aha → 99o
    aha.ths/parent   aha → 99o
    (dry run: nothing written)

### Label every epic for the decompose pass
    was: for e in $(lit ls --type epic --columns id | awk '{print $1}'); do lit label add $e decompose; done
    $ lit add '(level=2)/labels' decompose --dry-run
    (level=2) = [4y3, bue, lm3, kg2, xu4, 7du, cjt]
    + 4y3/labels/decompose
    + bue/labels/decompose
    + lm3/labels/decompose
    + kg2/labels/decompose
    + xu4/labels/decompose
    + 7du/labels/decompose
    + cjt/labels/decompose
    (dry run: nothing written)

### Clear a label from every epic that still has it
    $ lit rm '(level=2 labels=decompose)/labels/decompose'
    - kg2/labels/decompose
    - cjt/labels/decompose

### Close stale tickets as obsolete, each with the reason obsolete requires
    was: for t in etg.8qa etg.3d4 etg.ugm etg.aj3 2pq; do lit close $t --resolution obsolete --reason "ws_client deleted in #55"; done
    $ lit set etg.8qa/status '{column:obsolete reason:"ws_client deleted in #55"}' etg.3d4/status '{column:obsolete reason:"ws_client deleted in #55"}' etg.ugm/status '{column:obsolete reason:"ws_client deleted in #55"}'
    etg.8qa/status          backlog → obsolete
    etg.8qa/status/reason   (none) → ws_client deleted in #55
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    etg.3d4/status          backlog → obsolete
    etg.3d4/status/reason   (none) → ws_client deleted in #55
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    etg.ugm/status          backlog → obsolete
    etg.ugm/status/reason   (none) → ws_client deleted in #55
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (obsolete): update the related tickets with what this work taught.

### Close every ticket matching a selector as obsolete, with the reason it requires
    $ lit set '(text~"ws_client" status=backlog)/status' '{column:obsolete reason:"ws_client deleted in #55"}' --dry-run
    (text~"ws_client" status=backlog) = [etg.8qa, etg.3d4, etg.ugm, etg.aj3, 2pq]
    etg.8qa/status          backlog → obsolete
    etg.8qa/status/reason   (none) → ws_client deleted in #55
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    …
    guidance (obsolete): update the related tickets with what this work taught.
    (dry run: nothing written)

### Mark three merged tickets done
    was: for t in 2ku 207 oq5; do lit done template-unfiled-792a.$t; done
    $ lit set 792a.2ku/status done 792a.207/status done 792a.oq5/status done
    792a.2ku/status   merge → done
    ⇒ /lanes/792a.2ku/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    792a.207/status   merge → done
    ⇒ /lanes/792a.207/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    792a.oq5/status   merge → done
    ⇒ /lanes/792a.oq5/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (done): update the related tickets with what this work taught.

### Close everything sitting in merge, after checking what that is
    $ lit set '(status=merge)/status' done --dry-run
    (status=merge) = [lq3.808, q4p, lq3.hwi, lq3.1dt]
    lq3.808/status   merge → done
    ⇒ /lanes/lq3.808/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    q4p/status       merge → done
    ⇒ /lanes/q4p/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    lq3.hwi/status   merge → done
    ⇒ /lanes/lq3.hwi/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    lq3.1dt/status   merge → done
    ⇒ /lanes/lq3.1dt/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (done): update the related tickets with what this work taught.
    (dry run: nothing written)

### Reopen several tickets in one call
    was: lit open 2bs 2bs.tsm 2bs.99c   (refused: usage: lit open <id>)
    $ lit set 2bs.tsm/status backlog 2bs.99c/status backlog
    2bs.tsm/status   done → backlog
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    2bs.99c/status   done → backlog
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    guidance (backlog): A ticket leaves the backlog once it says what to build and someone has put its epic in order and marked it ready. …

### Close every child of an abandoned epic as obsolete
    was: for id in …; do lit archive $id --reason "voice dropped"; done
    $ lit set 'r7y.*/status' '{column:obsolete reason:"voice dropped"}' --dry-run
    r7y.* = [r7y.8rr, r7y.ux7, r7y.k2q]
    r7y.8rr/status          backlog → obsolete
    r7y.8rr/status/reason   (none) → voice dropped
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    r7y.ux7/status          backlog → obsolete
    r7y.ux7/status/reason   (none) → voice dropped
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    r7y.k2q/status          backlog → obsolete
    r7y.k2q/status/reason   (none) → voice dropped
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (obsolete): update the related tickets with what this work taught.
    (dry run: nothing written)

### Delete two probe tickets
    was: lit delete ef3.9yn --reason "leftover probe"; lit delete ef3.vqr --reason "filed in error"
    $ lit rm ef3.9yn --reason "leftover probe"
    ef3.9yn/deleted   false → true
    $ lit rm ef3.vqr --reason "filed in error"
    ef3.vqr/deleted   false → true

### Purge an abandoned epic's whole subtree
    was: for id in 3aj.8k1 3aj.evr …; do lit purge $id --yes; done
    $ lit rm 3aj --purge --dry-run
    3aj = [3aj, 3aj.8k1, 3aj.evr, 3aj.q0p]
    - 3aj           (comments 0, labels 1, edges 0)
    - 3aj.8k1       (comments 3, labels 0, edges 2)
    - 3aj.evr       (comments 0, labels 1, edges 1)
    - 3aj.q0p       (comments 1, labels 0, edges 0)
    (dry run: nothing written)

### Remove two stale comments, then add the corrected one
    was: lit comment rm cmt-2f277e42-…; lit comment rm cmt-f5f02379-…; lit comment add flu.03v --body "…"
    $ lit rm flu.03v/comments/2f2 flu.03v/comments/f50
    flu.03v/comments/2f2/deleted  false → true
    flu.03v/comments/f50/deleted  false → true
    $ lit add flu.03v/comments "Login writes src/oauth.ts; the token refresh is in src/refresh.ts."
    + flu.03v/comments/7hd

### Remove one of a pair of identical comments posted to two tickets
    was: lit comment rm cmt-40f73118-…
    $ lit rm c3u.isp/comments/40f
    c3u.isp/comments/40f/deleted  false → true

### Remove every comment one session left on a ticket
    $ lit rm 'ef3.b06/comments/(by=claude_64e3)' --dry-run
    ef3.b06/comments/(by=claude_64e3) = [a1k, a1m, c9p]
    ef3.b06/comments/a1k/deleted  false → true
    ef3.b06/comments/a1m/deleted  false → true
    ef3.b06/comments/c9p/deleted  false → true
    (dry run: nothing written)

### The same comment on every child of an epic
    $ lit add '0x6u.*/comments' "Groom 2026-11-03: re-verified against master 2d7ee77."
    + 0x6u.snf/comments/m1c
    + 0x6u.1ur/comments/m1d
    + 0x6u.4bt/comments/m1e

### The same comment on three unrelated tickets
    was: for t in c3u.isp fxnj.cjq q9s8.qsk; do lit comment add $t --body "Fixed by #438 …"; done
    $ lit add c3u.isp/comments "Fixed by #438 (c3u.89i): the doubled emptying row came from the stale buffer." fxnj.cjq/comments "Fixed by #438 (c3u.89i): the doubled emptying row came from the stale buffer." q9s8.qsk/comments "Fixed by #438 (c3u.89i): the doubled emptying row came from the stale buffer."
    + c3u.isp/comments/q2a
    + fxnj.cjq/comments/q2b
    + q9s8.qsk/comments/q2c

### Eleven different grooming notes on eleven tickets
    was: lit comment add vu2t.7ro --body 'Grooming 2026-10-28: …'; lit comment add i15t.qg8 --body '…'; … (11 in one command)
    $ lit add vu2t.7ro/comments "Grooming 2026-10-28: superseded by the lq3 lane; kept for the repro." i15t.qg8/comments "Grooming 2026-10-28: …" …
    + vu2t.7ro/comments/b4n
    + i15t.qg8/comments/b4p
    …

### Rewrite six descriptions from a directory of files
    was: for t in b06 9e3 avb 0ho ll5 bl1; do lit update ef3.$t --description-file /tmp/groom-7/desc/$t.md --reason "groom: …"; done
    $ lit set ef3.b06/body "$(cat /tmp/groom-7/desc/b06.md)" ef3.9e3/body "$(cat /tmp/groom-7/desc/9e3.md)" ef3.avb/body "$(cat /tmp/groom-7/desc/avb.md)" --reason "groom: …"
    ef3.b06/body   (+12 −5 lines)
    ef3.9e3/body   (+8 −8 lines)
    ef3.avb/body   (+3 −1 lines)

### One description from stdin, the common case
    was: lit update 8x3 --description-file /tmp/groom-scope/8x3.txt
    $ lit set iwpa.8x3/body - < /tmp/groom-scope/8x3.txt
    iwpa.8x3/body   (+14 −9 lines)
## Lanes

### Three new tickets that must be worked in order, as one lane
One call files them, one call sequences them. Stdin on a list path is one element per line, and the order of the ids is the order in the lane.

    $ lit add 792a/children - <<'EOF'
    {title:"dateInZone accepts zone names in any letter case" labels:[bug]}
    {title:"dateInZone accepts fixed offsets like +05:30"}
    {title:"date parses RFC 3339 with fractional seconds"}
    EOF
    + 792a.dee
    + 792a.b64
    + 792a.fw9
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.
    $ lit add /lanes/datetime '[dee, b64, fw9]'
    + /lanes/datetime  [dee, b64, fw9]
    - /lanes/dee
    ⇒ dee/lane  dee → datetime
    - /lanes/b64
    ⇒ b64/lane  b64 → datetime
    ⇒ b64/blocked_by  [] → [dee]
    - /lanes/fw9
    ⇒ fw9/lane  fw9 → datetime
    ⇒ fw9/blocked_by  [] → [b64]
    ⇒ /lanes/datetime/rank = 9

### A lane from three existing tickets
    was: for t in 9r8 hf4 a53; do lit update 3k6u.$t --lane store-bugs --reason "independent of the checkpoint chain"; done
    $ lit add /lanes/store-bugs '[3k6u.9r8, 3k6u.hf4, 3k6u.a53]' --reason "independent of the checkpoint chain"
    + /lanes/store-bugs  [3k6u.9r8, 3k6u.hf4, 3k6u.a53]
    - /lanes/9r8
    ⇒ 3k6u.9r8/lane  9r8 → store-bugs
    - /lanes/hf4
    ⇒ 3k6u.hf4/lane  hf4 → store-bugs
    ⇒ 3k6u.hf4/blocked_by  [] → [3k6u.9r8]
    - /lanes/a53
    ⇒ 3k6u.a53/lane  a53 → store-bugs
    ⇒ 3k6u.a53/blocked_by  [] → [3k6u.hf4]
    ⇒ /lanes/store-bugs/rank = 5

### Add one existing ticket to the end of a lane
    was: lit update 3k6u.324 --lane store-bugs
    $ lit add /lanes/store-bugs 3k6u.324
    + /lanes/store-bugs/3k6u.324
    - /lanes/324
    ⇒ 3k6u.324/lane  324 → store-bugs
    ⇒ 3k6u.324/blocked_by  [] → [3k6u.a53]

### Put every child of an epic into one lane, in their current order
    was: for t in 5x1 cfm; do lit update e5r4.$t --lane policy; done
    $ lit add /lanes/policy '[e5r4.5x1, e5r4.cfm]'
    + /lanes/policy  [e5r4.5x1, e5r4.cfm]
    - /lanes/5x1
    ⇒ e5r4.5x1/lane  5x1 → policy
    - /lanes/cfm
    ⇒ e5r4.cfm/lane  cfm → policy
    ⇒ e5r4.cfm/blocked_by  [] → [e5r4.5x1]
    ⇒ /lanes/policy/rank = 8

### Which lane a ticket is in
    was: lit show 3k6u.9r8 --field lane
    $ lit get 3k6u.9r8/lane
    store-bugs

### A ticket in no lane is its own lane
It lives at `/lanes/<id>`, so it has a rank path like any other lane.

    $ lit get e9pl.7a2/lane
    7a2
    $ lit get /lanes/7a2
    /lanes/7a2  rank 5
      7a2   backlog

### Reorder within a lane

    $ lit set /lanes/datetime '[b64, dee, fw9]' --reason "b64 is the foundation dee builds on"
    /lanes/datetime   [dee, b64, fw9] → [b64, dee, fw9]
    ⇒ b64/blocked_by   [dee] → []
    ⇒ dee/blocked_by   [] → [b64]
    ⇒ fw9/blocked_by   [b64] → [dee]
    ⇒ /lanes/datetime  head is now b64

### Reorder with a typo, caught by the dry run
The list is replaced, not merged, so a dropped id is a removal, and the echo names it.

    $ lit set /lanes/datetime '[b64, fw9]' --dry-run
    /lanes/datetime   [dee, b64, fw9] → [b64, fw9]
    - /lanes/datetime/dee
    ⇒ dee/lane  datetime → dee
    ⇒ b64/blocked_by   [dee] → []
    (dry run: nothing written)
    $ lit set /lanes/datetime '[b64, dee, fw9]'
    /lanes/datetime   [dee, b64, fw9] → [b64, dee, fw9]
    …

### Take two tickets out of a lane so they run in parallel
The groomer found that `help.py`, `scene.py` and `code_policy.py` fixes share no files. The lane was serializing them falsely.

    was: lit update etg.qeo --lane help --reason "groom: … share no files; the unlaned default lane serialized them falsely"
    $ lit rm /lanes/etg-fixes/etg.qeo /lanes/etg-fixes/etg.j33 --reason "groom: the help.py, scene.py and code_policy.py fixes share no files"
    - /lanes/etg-fixes/etg.qeo
    ⇒ etg.qeo/lane  etg-fixes → qeo
    - /lanes/etg-fixes/etg.j33
    ⇒ etg.j33/lane  etg-fixes → j33

### Move a ticket from one lane to another
A ticket is in at most one lane, so adding it to another moves it, and the echo shows the removal.

    $ lit add /lanes/store-bugs q8n
    - /lanes/checkpoint/q8n
    + /lanes/store-bugs/q8n
    ⇒ q8n/lane  checkpoint → store-bugs
    ⇒ q8n/blocked_by  [l1x] → [3k6u.324]
    ⇒ 3sm/blocked_by  [q8n] → [l1x]

### Dissolve the lane every Unfiled ticket was stuck in
"Every ticket here is its own lane" was written in the Unfiled epic's description. Eight `--lane` calls were the fix. Removing the lane makes each member solo.

    was: for t in 1kx aki …; do lit update ihvy.$t --lane ihvy.$t; done
    $ lit rm /lanes/unfiled --dry-run
    - /lanes/unfiled   [1kx, aki, 2ku, 207, oq5, r5c, 7nq0, hpxb]
    ⇒ 1kx/lane  unfiled → 1kx
    ⇒ aki/lane  unfiled → aki
    ⇒ aki/blocked_by  [1kx] → []
    …
    (dry run: nothing written)

### Rerank a whole lane
Everything in the lane moves together.

    was: lit rank 3k6u.9r8 --top; lit rank 3k6u.hf4 --below 3k6u.9r8; lit rank 3k6u.a53 --below 3k6u.hf4
    $ lit set /lanes/store-bugs/rank '{above: /lanes/datetime}'
    /lanes/store-bugs/rank   5 → 2
    ⇒ /lanes/datetime/rank     2 → 3
    ⇒ /lanes/help/rank         3 → 4
    ⇒ /lanes/checkpoint/rank   4 → 5

### Rank one lane just below another
    was: lit rank acme-scale-dy46.aof --below acme-scale-dy46.48k
    $ lit set /lanes/aof/rank '{below: /lanes/48k}'
    /lanes/aof/rank   7 → 4
    ⇒ /lanes/help/rank   4 → 5
    …

### Thirteen tickets that are one sequence, as one lane
The session's 13-call `rank --top` / `--below` chain was an ordered list.

    was: lit rank kg2.c44 --top && lit rank xu4.1il --below kg2.c44 && lit rank xu4.53j --below xu4.1il && … (13 calls)
    $ lit add /lanes/release '[kg2.c44, xu4.1il, xu4.53j, bue.hqi, bue.1e6, lm3.7iy, lm3.1tt, kg2.1tz, 7du.5uw, 7du.usq, cjt.3ns, b2q.8ew, b2q.3cn]' --dry-run
    + /lanes/release  [kg2.c44, xu4.1il, …, b2q.3cn]
    - /lanes/c44
    ⇒ kg2.c44/lane  c44 → release
    - /lanes/1il
    ⇒ xu4.1il/lane  1il → release
    ⇒ xu4.1il/blocked_by  [] → [kg2.c44]
    - /lanes/53j
    ⇒ xu4.53j/lane  53j → release
    ⇒ xu4.53j/blocked_by  [] → [xu4.1il]
    …
    - /lanes/3cn
    ⇒ b2q.3cn/lane  3cn → release
    ⇒ b2q.3cn/blocked_by  [] → [b2q.8ew]
    ⇒ /lanes/release/rank = 11
    (dry run: nothing written)

### Ten independent epics, ordered by priority
The ten-call variant ranked epics, which are not a sequence. An epic is a folder and has no rank; each epic holds one lane, and the lane gets the rank. Each relation reads the order the same write has already placed.

    was: lit rank kg2 --top; lit rank xu4 --below kg2; lit rank bue --below xu4; …
    $ lit set kg2/lanes/main/rank top xu4/lanes/main/rank '{below: kg2/lanes/main}' bue/lanes/main/rank '{below: xu4/lanes/main}' lm3/lanes/main/rank '{below: bue/lanes/main}'
    kg2/lanes/main/rank   6 → 1
    xu4/lanes/main/rank   1 → 2
    bue/lanes/main/rank   9 → 3
    lm3/lanes/main/rank   2 → 4
    ⇒ acr/lanes/main/rank   3 → 5
    …

### Read the whole backlog
    was: lit backlog | head -150
    $ lit get /lanes --limit 150
    /lanes/pin            rank 1  pin        backlog                                         1/1  urgent
    /lanes/store-bugs     rank 2  3k6u.9r8   backlog                                         1/4
    /lanes/datetime       rank 3  b64        impl     this checkout                          1/3
    /lanes/checkpoint     rank 4  3k6u.l1x   impl     checkout 7f3a (elsewhere) · 5m ago     1/3
    /lanes/7a2            rank 5  7a2        backlog                                         1/1
    …

### Which top lanes are claimed
    was: lit backlog | grep -A2 -E '^\s*[12]\. ' | grep -E 'claimed'
    $ lit get '/lanes/*/claim'
    /lanes/datetime/claim     this checkout
    /lanes/checkpoint/claim   checkout 7f3a (elsewhere) · 5m ago

### An epic that holds two parallel lanes
`rin` has a parser stream and an evaluator stream. Neither waits on the other.

    $ lit add rin/lanes/parser '[rin.2ae, rin.k3x]'
    + rin/lanes/parser  [rin.2ae, rin.k3x]
    - /lanes/2ae
    ⇒ rin.2ae/lane  2ae → parser
    - /lanes/k3x
    ⇒ rin.k3x/lane  k3x → parser
    ⇒ rin.k3x/blocked_by  [] → [rin.2ae]
    ⇒ rin/lanes/parser/rank = 6
    $ lit add rin/lanes/eval '[rin.9u9, rin.m0p, rin.t7c]'
    + rin/lanes/eval  [rin.9u9, rin.m0p, rin.t7c]
    - /lanes/9u9
    ⇒ rin.9u9/lane  9u9 → eval
    - /lanes/m0p
    ⇒ rin.m0p/lane  m0p → eval
    ⇒ rin.m0p/blocked_by  [] → [rin.9u9]
    - /lanes/t7c
    ⇒ rin.t7c/lane  t7c → eval
    ⇒ rin.t7c/blocked_by  [] → [rin.m0p]
    ⇒ rin/lanes/eval/rank = 7
    $ lit get rin/lanes
    rin/lanes/parser  rank 6
      rin.2ae   review
      rin.k3x   backlog
    rin/lanes/eval  rank 7
      rin.9u9   backlog
      rin.m0p   backlog
      rin.t7c   backlog

### An urgent ticket ranked top
Nothing had published since v1.156.4. Urgency is the ticket's priority; the ticket is its own lane, ranked last at birth, and the filer moves that lane to the top.

    was: lit new --type bug --topic release --top --title "Nothing has published since v1.156.4"
    $ lit add 792a/children '{title:"Nothing has published since v1.156.4" labels:[bug] topic:release priority:urgent}'
    + 792a.pin
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.
    $ lit set /lanes/pin/rank top
    /lanes/pin/rank   12 → 1
    ⇒ /lanes/store-bugs/rank  1 → 2
    ⇒ /lanes/datetime/rank    2 → 3
    …

### What next serves when this checkout holds a lane
A higher lane whose head is not urgent does not pull it away.

    was: lit next
    $ lit get /next
    b64   /lanes/datetime 1/3   rank 3   impl   claim this checkout

### What next serves when an urgent head sits above the held lane
    $ lit get /next
    pin   rank 1   backlog   urgent, served before /lanes/datetime (claim this checkout)
      set pin/status plan would claim it

### What next serves when this checkout holds no lane
    $ lit get /next
    3k6u.9r8   /lanes/store-bugs 1/4   rank 1   backlog   outside the commitment: this checkout holds no lane
      set 3k6u.9r8/status plan would claim /lanes/store-bugs

### Drop a lane claim without finishing
The checkout is leaving the lane mid-way. The only way to do that is a write that says so.

    $ lit rm /lanes/datetime/claim --reason "handing the datetime lane to the parser session"
    ⇒ /lanes/datetime/claim  this checkout → (none)
    ⇒ /next  b64 → 3k6u.9r8

### Release a dead session's claim and put the ticket back
The plan phase had sat for ~12 hours with no commits or branch, in a worktree still locked, and a lock carries a claim past `claim_ttl`. Parking the ticket in `backlog` ends the hold, and the reason travels with the move.

    was: lit open etg.bci --reason "Plan phase ~12h, no commits or branch; dead session's claim released"
    $ lit set etg.bci/status backlog --reason "Plan phase ~12h, no commits or branch; dead session's claim released"
    etg.bci/status   plan → backlog
    ⇒ /lanes/bci/claim  checkout f335 (elsewhere) · 12h ago → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)

## Lanes at the edges

### Take one ticket out of a lane

    was: lit update tej.5 --lane ""
    $ lit rm /lanes/datetime/b64
    - /lanes/datetime/b64
    ⇒ b64/lane  datetime → b64
    ⇒ b64/blocked_by  [dee] → []
    ⇒ fw9/blocked_by  [b64] → [dee]

### Dissolve a lane I hold

    $ lit rm /lanes/datetime
    - /lanes/datetime   [dee, b64, fw9]
    ⇒ dee/lane  datetime → dee
    ⇒ b64/lane  datetime → b64
    ⇒ b64/blocked_by  [dee] → []
    ⇒ fw9/lane  datetime → fw9
    ⇒ fw9/blocked_by  [b64] → []
    ⇒ /lanes/dee/claim = this checkout

`dee`'s start is the establishing transition, so its own lane keeps the hold; `b64` and `fw9` were never started and are held by nobody.

### Reorder a lane so the claimed ticket is no longer its head

    $ lit set /lanes/datetime '[b64, dee, fw9]'
    /lanes/datetime  [dee, b64, fw9] → [b64, dee, fw9]
    ⇒ b64/blocked_by  [dee] → []
    ⇒ dee/blocked_by  [] → [b64]
    ⇒ fw9/blocked_by  [b64] → [dee]
    ⇒ /lanes/datetime  head is now b64

The claim is the lane's, so this checkout still holds it; `/next` serves `b64`.

### The held lane's head waits on an edge from outside the lane

    $ lit get /lanes/datetime
    /lanes/datetime  rank 4   claim this checkout
      dee   done
      b64   backlog   blocked_by [dee, bryp] (bryp/blocks)
      fw9   backlog

### An edge that closes a cycle through a lane

    $ lit add fw9/blocks dee
    error (reason=cycle): fw9/blocks dee closes a cycle: dee → b64 → fw9 (/lanes/datetime) → dee; nothing written

## A grooming pass, in order

### 1. Read every backlog ticket cheaply
Two lines per ticket, instead of the 270 KB the `show` loop printed.

    $ lit get '(status=backlog)/title' '(status=backlog)/lane'
    b64/title   dateInZone accepts fixed offsets like +05:30
    b64/lane    b64
    k3x/title   template with no pipeline passes nil dot
    k3x/lane    parser
    …

### 2. Read the backlog in served order
    $ lit get /lanes
    /lanes/store-bugs   rank 2  3k6u.9r8   backlog   1/4
    /lanes/dee          rank 7  792a.dee   backlog   1/1
    …

### 3. Read the bodies of the tickets that look alike
    $ lit get 2d80.d66/body 2d80.c7e/body lq3.9od/body
    2d80.d66/body   The 'no tools' Unknown rule fires on …
    2d80.c7e/body   Unknown: no tools listed for …
    lq3.9od/body    The warning rules classify a turn with no tools …

### 4. Close a duplicate and move its acceptance criteria to the canonical ticket, in one call
The canonical body is the merged text on stdin. The duplicate closes against it in the same write.

    $ lit set lq3.9od/body - 2d80.d66/status '{column:dup of:lq3.9od}' < /tmp/groom/9od-merged.md
    lq3.9od/body          (+6 −0 lines)
    2d80.d66/status       backlog → dup
    2d80.d66/status/of    (none) → lq3.9od
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (dup): update the related tickets with what this work taught.

### 5. Close a ticket whose work is already done
    was: lit close acme-error-output-ckk7 --resolution obsolete --reason "groom: already satisfied - stderr now prints …"
    $ lit set ckk7/status '{column:obsolete reason:"groom: already satisfied; stderr prints the code and reason since #811"}'
    ckk7/status          backlog → obsolete
    ckk7/status/reason   (none) → groom: already satisfied; stderr prints the code and reason since #811
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (obsolete): update the related tickets with what this work taught.

### 6. Rewrite a description the reader would misread
    $ lit set 9ym/body - <<'EOF'
    The statusline shows the lane's head and its position (2/3).
    Done when: a held lane renders as "datetime 2/3", a solo ticket as its id.
    EOF
    9ym/body   (+2 −6 lines)

### 7. Put the trio that shares files into one lane
    $ lit add /lanes/datetime '[792a.dee, 792a.b64, 792a.fw9]'
    + /lanes/datetime  [792a.dee, 792a.b64, 792a.fw9]
    - /lanes/dee
    ⇒ 792a.dee/lane  dee → datetime
    - /lanes/b64
    ⇒ 792a.b64/lane  b64 → datetime
    ⇒ 792a.b64/blocked_by  [] → [792a.dee]
    - /lanes/fw9
    ⇒ 792a.fw9/lane  fw9 → datetime
    ⇒ 792a.fw9/blocked_by  [] → [792a.b64]
    ⇒ /lanes/datetime/rank = 9

### 8. Rank the lanes
    $ lit set /lanes/datetime/rank '{above: /lanes/store-bugs}'
    /lanes/datetime/rank     9 → 2
    ⇒ /lanes/store-bugs/rank   2 → 3

### 9. Check integrity
`/integrity` lists what the fold could not settle on its own. Each line names the write that clears it; nothing on it is a repair lit runs.

    was: lit doctor
    $ lit get /integrity
    /integrity   2 advisories
      rank_anchors     /lanes/help was placed above /lanes/eval-old, which no longer exists; it ranks last
                       clear: lit set /lanes/help/rank '{above: /lanes/eval}'
      dangling_edges   jcj.l1e/blocks/ob5 names ob5, which is deleted
                       clear: lit rm jcj.l1e/blocks/ob5

### 10. Run the writes integrity names, dry run first
    was: lit doctor --fix
    $ lit set /lanes/help/rank '{above: /lanes/eval}' --dry-run
    /lanes/help/rank   12 → 4
    ⇒ /lanes/eval/rank        4 → 5
    ⇒ /lanes/aof/rank         5 → 6
    …
    ⇒ /integrity/rank_anchors = (none)
    (dry run: nothing written)
    $ lit set /lanes/help/rank '{above: /lanes/eval}'
    /lanes/help/rank   12 → 4
    ⇒ /lanes/eval/rank        4 → 5
    ⇒ /lanes/aof/rank         5 → 6
    …
    ⇒ /integrity/rank_anchors = (none)
    $ lit rm jcj.l1e/blocks/ob5
    - jcj.l1e/blocks/ob5
    ⇒ /integrity/dangling_edges = (none)

### 11. Report what changed
The echoes above are the grooming report. A session that saved them has the whole diff. The pass ends on an empty `/integrity`.

    $ lit get /integrity
    /integrity   ok

## Undo

### Undo a title edit from its own echo

    $ lit set 9ym/title "Status line hides the ceiling when idle"
    9ym/title  Status line shows the ceiling only past 80% → Status line hides the ceiling when idle
    $ lit set 9ym/title "Status line shows the ceiling only past 80%"
    9ym/title  Status line hides the ceiling when idle → Status line shows the ceiling only past 80%

### Undo a description rewrite

    was: lit history yjx --version description@1 > /tmp/yjx-v1.txt; lit update yjx --description "$(cat /tmp/yjx-v1.txt)"
    $ lit get yjx/body/versions
    yjx/body/versions/1   Brandon Fryslie  2026-10-19T16:02Z
    yjx/body/versions/2   claude_5c44…     2026-11-02T21:10Z   (+4 −1 lines)
    yjx/body/versions/3   claude_5c44…     2026-11-02T21:31Z   (+2 −7 lines)
    $ lit get yjx/body/versions/1 | lit set yjx/body - --reason "restore the original description"
    yjx/body  (+6 −5 lines)

### I meant to add a label and replaced the list

    $ lit set 9to/labels '[decompose]'
    9to/labels  [external, needs-design] → [decompose]
    $ lit set 9to/labels '[external, needs-design, decompose]'
    9to/labels  [decompose] → [external, needs-design, decompose]

### Find everything this session changed, to undo it

    $ lit get '(updated>-2h)/history/(by=claude_5c44)'
    yjx/history/e91   2026-11-02T21:10Z   claude_5c44…   body (+4 −1 lines)
    yjx/history/e94   2026-11-02T21:31Z   claude_5c44…   body (+2 −7 lines)
    9ym/history/e12   2026-11-02T21:44Z   claude_5c44…   title Status line shows the ceiling only past 80% → Status line hides the ceiling when idle
## Columns as data

### Which columns govern this ticket, and where each comes from

    was: lit workflows show rin.4ou   →   no workflow definition with id "rin.4ou"
    $ lit get rin.4ou/columns
    rin.4ou/columns  (resolved)
      backlog     initial   next:[plan]     exit:[ready]                          /columns
      plan                  next:[audit]                                          rin/columns
      audit                 next:[impl]                                           rin/columns
      impl                  next:[review]   entry:[plan/verdict=pass]             /columns
      review                next:[merge]    entry:[plan/verdict=pass (warning)]   /columns
      merge                 next:[done]     entry:[pr/verdict=pass]               /columns
      done        terminal                                                        /columns
      dup         terminal                  entry:[status/of]                     /columns
      superseded  terminal                  entry:[status/of]                     /columns
      obsolete    terminal                  entry:[status/reason]                 /columns
      wontfix     terminal                  entry:[status/reason]                 /columns

### Add an audit column to one epic

The column and the handler that speaks in it are written together; the write edits
the working-tree `.lit/workspace.toml`. Commit the file, then apply it.

    $ lit set rin/columns/audit '{next:[impl]}' rin/columns/plan/next '[audit]' rin/workflows/audit '{summary:"law audit before code review"}' rin/workflows/audit/handlers/law '{on:[enter audit, in audit] body:"Run the law audit on the diff before code review."}'
    rin/columns/audit                 (none) → {next:[impl]}
    rin/columns/plan/next             [impl] (from /columns) → [audit]
    rin/workflows/audit               (none) → {summary:"law audit before code review"}
    rin/workflows/audit/handlers/law  (none) → {on:[enter audit, in audit] body:"Run the law audit on the diff before code review."}
    ⇒ .lit/workspace.toml edited (working tree); in force after lit config commit
    $ lit config commit
    applied .lit/workspace.toml as the active version
    ⇒ 6 tickets under rin resolve plan → audit → impl

### Add an exit requirement to a column

Nothing in the live-verification epic leaves review until it carries the
`live-verified` label. An exit requirement on a column guards the forward
transitions out of it.

    $ lit add mel/columns/review/exit '{test:labels=live-verified severity:error}'
    + mel/columns/review/exit/1
    ⇒ .lit/workspace.toml edited (working tree); in force after lit config commit
    $ lit config commit
    applied .lit/workspace.toml as the active version

### Refused by that requirement

    $ lit set mel.l1e/status live-verify
    error (reason=gate): mel.l1e/status review → live-verify refused by review/exit; nothing written
      block  labels=live-verified   mel.l1e/labels = [external]

    guidance (review): when review is done and the merge is under way, `set mel.l1e/status merge`.

### Change the guidance on a column

The text a ticket in `merge` prints is the body of the shipped handler bound
`in merge`. Overriding its body writes the project layer and leaves its other
fields shipped.

    $ lit set /workflows/phases/handlers/merge/body - <<'EOF'
    Once it has merged, close it: lit set {$}/status done. The echo names the next ticket in your lane.
    EOF
    /workflows/phases/handlers/merge/body  once merged, set {$}/status done. → Once it has merged, close it: lit set {$}/status done. The echo names the next ticket in your lane.   (shadows shipped)
    ⇒ .lit/workspace.toml edited (working tree); in force after lit config commit

### Read the guidance a ticket in a custom column will see

    was: lit quickstart review
    $ lit get rin/workflows/audit/handlers/law
    rin/workflows/audit/handlers/law
      on        enter audit, in audit   rin
      where     (none)                  default
      enabled   true                    default

    Run the law audit on the diff before code review.

### Remove a column that tickets are in

    $ lit rm /columns/merge
    error (reason=in_use): /columns/merge is occupied and named; nothing written
      occupied by   0814 s33 w3c               (status=merge)
      named in      /columns/review/next = [merge]

### Remove an override nobody occupies

    $ lit rm rin/columns/audit rin/columns/plan/next rin/workflows/audit --dry-run
    - rin/columns/audit
    - rin/columns/plan/next
    ⇒ rin/columns/plan/next  [audit] → [impl] (from /columns)
    - rin/workflows/audit
    (dry run: nothing written)

### Add a terminal column that requires a field

    $ lit set /columns/moved '{terminal:true entry:[{test:status/of severity:error}]}'
    /columns/moved  (none) → {terminal:true entry:[{test:status/of severity:error}]}
    ⇒ .lit/workspace.toml edited (working tree); in force after lit config commit

### Close as superseded without naming what supersedes it

    was: lit close 2ka --resolution superseded --of 6op --reason "…"
    $ lit set 2ka/status superseded
    error (reason=gate): 2ka/status backlog → superseded refused by superseded/entry; nothing written
      block  status/of   2ka/status/of = (none)
    $ lit set 2ka/status '{column:superseded of:6op}' --reason "user decided: too-large results go whole to a file"
    2ka/status     backlog → superseded
    2ka/status/of  (none) → 6op
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (superseded): update the related tickets with what this work taught.

### A transition that is not in next

    was: lit phase tbk --to merge   (from impl)
    $ lit set tbk/status merge
    error (reason=not_in_next): tbk/status impl → merge refused: merge is entered only from review; nothing written

    guidance (impl): when the PR opens, `set tbk/status review`.

### Park an in-progress ticket, or drop it as obsolete

A column no `next` names, `backlog` and every terminal but `done`, is entered from
every column; `next` lists only the forward edges.

    was: lit open 37o --reason "parked behind bryp"; lit close 2m0 --resolution obsolete --reason "…"
    $ lit set 37o/status backlog --reason "parked behind bryp"
    37o/status  impl → backlog
    ⇒ /lanes/37o/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)
    $ lit set 2m0/status '{column:obsolete reason:"ws_client deleted in PR #55"}'
    2m0/status         review → obsolete
    2m0/status/reason  (none) → ws_client deleted in PR #55
    ⇒ /lanes/2m0/claim  checkout 7c1d (elsewhere) · 3h ago → (none)
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (obsolete): update the related tickets with what this work taught.

### One selector across two dialects of review

The live-verification epic defines its own `review`; everything else uses the root's.

    $ lit get '(status=review)/title'
    mel.l1e/title    Scene load reports a missing asset with its warning
    sjhy.ju8/title   Menus open on the focused display
    $ lit get mel/columns/review/next /columns/review/next
    mel/columns/review/next   [live-verify]
    /columns/review/next      [merge]

### Give one epic's claims a longer life

    $ lit set px1f/claim_ttl 12h
    px1f/claim_ttl  4h (from /) → 12h
    ⇒ .lit/workspace.toml edited (working tree); in force after lit config commit
    $ lit config commit
    applied .lit/workspace.toml as the active version
    ⇒ px1f.9u9/claim expiry  2026-11-03T14:04Z → 2026-11-03T22:04Z

### Where does this claim's expiry come from?

A claim lasts `claim_ttl` past the holder's newest event on any member of the lane.

    $ lit get px1f.9u9/claim px1f.9u9/claim_ttl
    $ = orbit-testing-px1f.9u9
    $/claim        checkout 9b2e (elsewhere) · 2h ago · newest event 2026-11-03T10:04Z on px1f.4kd · expires 2026-11-03T22:04Z
    $/claim_ttl    12h (from px1f)

## Derived fields

### Set a derived field

    $ lit set dee/claim claude_64e3
    error (reason=derived): dee/claim is derived, not settable; nothing written
      field  claim  scalar  derived  read from the holder of dee's lane
      take   lit add /lanes/datetime/claim

### Type the doc's own lane example

    $ lit set 'unfiled.*/lane' @id
    error (reason=derived): unfiled.*/lane is derived from lane membership, not settable; nothing written
    $ lit rm /lanes/unfiled
    - /lanes/unfiled
    ⇒ 2ku/lane  unfiled → 2ku
    ⇒ 207/lane  unfiled → 207
    ⇒ oq5/lane  unfiled → oq5

### Clear a ticket's blocker by hand

    $ lit set b64/blocked_by '[]'
    error (reason=derived): b64/blocked_by is derived, not settable; nothing written
      from   dee precedes b64 in /lanes/datetime
      clear  reorder the lane, or lit rm /lanes/datetime/b64

### Blocked by lane position versus by an edge

    was: lit show 37o | grep -A3 '^blocked_by:'
    $ lit get b64/blocked_by 37o/blocked_by
    b64/blocked_by   [dee]    (position 2 in /lanes/datetime)
    37o/blocked_by   [bryp]   (bryp/blocks)

### Reopen a closed epic by reopening a child

    was: lit open o3mh   →   cannot open epic: it is closed, and an epic's state derives from its children
    $ lit set o3mh.avl/status backlog --reason "Done-when fails on the second checkout"
    o3mh.avl/status  done → backlog
    ⇒ o3mh/stage  terminal → active
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)

### Rank a ticket

A ticket has no rank; its lane does.

    was: lit rank aof --below 48k
    $ lit set aof/rank '{below: 48k}'
    error (reason=unknown_field): aof/rank: a ticket has no field rank; nothing written
      rank is on lanes: aof is in /lanes/aof, 48k in /lanes/48k
      write  lit set /lanes/aof/rank '{below: /lanes/48k}'
    $ lit set /lanes/aof/rank '{below: /lanes/48k}'
    /lanes/aof/rank  7 → 4

### Change a ticket's topic

    was: lit update bwy.16 --topic context   →   unknown flag: --topic
    $ lit set bwy.16/topic context
    bwy.16/topic  budget → context

### Turn a leaf into an epic

An item's level is fixed where it is filed. The work becomes a node by filing the
node with its children and superseding the leaf with it.

    was: lit update hje --type epic   →   cannot change issue_type between container ([epic]) and leaf types
    $ lit set hje/level 2
    error (reason=derived): hje/level is derived, not settable; nothing written
      a level-1 item stays a leaf
    $ lit add /children '{level:2 title:"Per-display hotkeys" children:[{title:"Hotkeys load from the per-display config"}, {title:"The settings pane edits one display's hotkeys"}]}'
    + k3p
    ⇒ k3p/topic = hotkeys
    ⇒ k3p/parent = unfiled
    + k3p.v8e
    + k3p.w2n

    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.
    $ lit set hje/status '{column:superseded of:k3p}' --reason "the work is a feature with two stories"
    hje/status     backlog → superseded
    hje/status/of  (none) → k3p
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)

    guidance (superseded): update the related tickets with what this work taught.

## Integrity

### Is the store healthy?

`/integrity` lists what the fold could not settle silently. Each advisory names the
write that clears it; nothing on it is a repair lit runs.

    was: lit doctor
    $ lit get /integrity
    /integrity/rank_anchors       /lanes/scale was placed below /lanes/sync, which no longer exists; it ranks last
      clear: lit set /lanes/scale/rank '{below: /lanes/lm3}'
    /integrity/dependency_cycles  37o → bryp → 37o, from edges written in two clones
      clear: lit rm 37o/blocks/bryp   or   lit rm bryp/blocks/37o
    /integrity/shared_segments    c9d.4k names two tickets
      clear: (no write; the segment addresses neither ticket)

### Read one check

    $ lit get /integrity/rank_anchors
    /integrity/rank_anchors       /lanes/scale was placed below /lanes/sync, which no longer exists; it ranks last
      clear: lit set /lanes/scale/rank '{below: /lanes/lm3}'

### See what the clearing write would do

    was: lit doctor --fix
    $ lit set /lanes/scale/rank '{below: /lanes/lm3}' --dry-run
    /lanes/scale/rank  23 → 4
    ⇒ /integrity/rank_anchors  advisory → (none)
    (dry run: nothing written)

### Run it

    $ lit set /lanes/scale/rank '{below: /lanes/lm3}'
    /lanes/scale/rank  23 → 4
    ⇒ /integrity/rank_anchors  advisory → (none)

### A cycle only the agent can break

A sync merged two edges written in different clones. The advisory names both
writes that clear it; which edge is wrong is the agent's call.

    $ lit get /integrity/dependency_cycles
    /integrity/dependency_cycles  37o → bryp → 37o, from edges written in two clones
      clear: lit rm 37o/blocks/bryp   or   lit rm bryp/blocks/37o
    $ lit rm 37o/blocks/bryp --reason "bryp needs 37o's parser; the edge runs one way"
    - 37o/blocks/bryp
    ⇒ bryp/blocked_by  [37o] → []
    ⇒ /integrity/dependency_cycles  advisory → (none)

### Two clones minted the same segment

Each ticket keeps its own id; the shared segment resolves to neither.

    $ lit get c9d.4k/title
    error (reason=ambiguous): c9d.4k names two tickets; nothing read
      c9d.4k   Context budget reads the model's window   created 2026-11-02T09:14Z in checkout a1c0
      c9d.4k   Speech pipeline drops the first word      created 2026-11-02T11:40Z in checkout c93e

## Close out, delete, restore, purge

### Close an epic's open children out of the flow

    was: for id in …; do lit archive $id --reason "…"; done
    $ lit set 'r7y.*/status' '{column:obsolete reason:"wake word and barge-in move to the new audio stack"}' --dry-run
    r7y.* = [r7y.8rr, r7y.ux7, r7y.k2q]
    r7y.8rr/status         backlog → obsolete
    r7y.8rr/status/reason  (none) → wake word and barge-in move to the new audio stack
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    r7y.ux7/status         backlog → obsolete
    r7y.ux7/status/reason  (none) → wake word and barge-in move to the new audio stack
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    r7y.k2q/status         backlog → obsolete
    r7y.k2q/status/reason  (none) → wake word and barge-in move to the new audio stack
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    ⇒ r7y/stage  initial → terminal

    guidance (obsolete): update the related tickets with what this work taught.
    (dry run: nothing written)

### Read tickets closed out of the flow

Globs and selectors match tickets in every column; a deleted ticket only when the
selector names `deleted`.

    was: lit ls --ids r7y.ux7,r7y.8rr --include-archived
    $ lit get '(parent=r7y status=obsolete)/title'
    $ = hands-voice-r7y
    $.8rr/title   Wake word survives a sleep/wake cycle
    $.ux7/title   Barge-in stops playback within 100ms
    $.k2q/title   Voice activity ignores keyboard clicks

### Delete a ticket filed in error

    was: lit delete fko --reason "made in error: a doc edit, committed to master"
    $ lit rm fko --reason "made in error: a doc edit, committed to master"
    fko/deleted  false → true

### Restore a deleted ticket

    was: lit restore 9ym --reason "deleted in error; not phase-related"
    $ lit set 9ym/deleted false --reason "deleted in error; not phase-related"
    9ym/deleted  true → false

### Read a deleted ticket

    $ lit get 9yn/status 9yn/deleted
    $ = houdini-tuneup-ef3.9yn
    $/status    done
    $/deleted   true
    $ lit get '(deleted=true text~"(?i)probe")/title'
    9yn/title   PROBE ticket (delete me)
    9ci/title   probe-delete-me

### Purge a probe ticket for good

    was: lit purge 8k1 --yes
    $ lit rm 3aj.8k1 --purge
    - 3aj.8k1       (comments 3, labels 0, edges 2)

### Purge an abandoned epic, shown first

    $ lit rm 3aj --purge --dry-run
    3aj = [3aj, 3aj.8k1, 3aj.evr, 3aj.q0p]
    - 3aj           (comments 0, labels 1, edges 0)
    - 3aj.8k1       (comments 3, labels 0, edges 2)
    - 3aj.evr       (comments 0, labels 1, edges 1)
    - 3aj.q0p       (comments 1, labels 0, edges 0)
    ⇒ /lanes/monetization removed (no members left)
    (dry run: nothing written)

## Root paths

### What is at the root?

    $ lit get /
    /
      children    14
      lanes       23
      columns     10
      claim_ttl   4h
      integrity   ok
      next        b64
      workspace   active version matches .lit/workspace.toml

### The root column graph

    was: lit workflows
    $ lit get /columns
    /columns
      backlog     initial   next:[plan]     exit:[ready]
      plan                  next:[impl]                                           guidance:plan-review/plan
      impl                  next:[review]   entry:[plan/verdict=pass]             guidance:phases/impl
      review                next:[merge]    entry:[plan/verdict=pass (warning)]   guidance:phases/review, phases/findings (pr/verdict=fail)
      merge                 next:[done]     entry:[pr/verdict=pass]               guidance:phases/merge
      done        terminal
      dup         terminal                  entry:[status/of]
      superseded  terminal                  entry:[status/of]
      obsolete    terminal                  entry:[status/reason]
      wontfix     terminal                  entry:[status/reason]

### Every lane's rank

    was: lit queue
    $ lit get '/lanes/*/rank'
    /lanes/c44/rank          1
    /lanes/store-bugs/rank   2
    /lanes/checkpoint/rank   3
    /lanes/datetime/rank     4

### A whole subtree, depth by depth

    was: lit tree   →   unknown command "tree"
    $ lit get 'rin.*/title' 'rin.*.*/title'
    $ = acme-hierarchy-rin
    $.4ou/title       Ancestors resolve columns by walking up
    $.4ou.1ab/title   A child's column reads its own subtree's dialect
    $.9m5/title       Lanes are cross-cutting

## Output shape corners

### `--json` on a glob

    $ lit get '792a.*/status' --json
    {"792a.dee/status":"done","792a.b64/status":"impl","792a.fw9/status":"backlog"}

### Two paths under one ancestor

    $ lit get 792a.dee/title 792a.dee/status 792a.b64/status
    $ = template-unfiled-792a
    $.dee/title    dateInZone accepts zone names in any letter case
    $.dee/status   done
    $.b64/status   impl

### Guidance printed after the data

    $ lit get s33
    $ = template-unfiled-792a.s33
    $/title    dateInZone reports the zone it resolved
    $/status   merge
    $/lane     s33
    $/claim    this checkout

    guidance (merge): once merged, `set s33/status done`.

### A ticket in a subtree with its own column

    $ lit get rin.4ou
    $ = acme-hierarchy-rin.4ou
    $/title    Ancestors resolve columns by walking up
    $/status   audit
    $/claim    this checkout

    guidance (audit): Run the law audit on the diff before code review.

### A multi-line value in path-tab-value output

A value holding a newline prints as a quoted string that runs across lines to its
closing quote; a `"` inside it prints as `""`.

    $ lit get '792a.*/body'
    $ = template-unfiled-792a
    $.dee/body   "Go's LoadLocation is case-sensitive; dateInZone is not.

    Done when: …"
    $.b64/body   "Builds on dee: the ""zone"" argument is the one dee normalizes.

    Done when: …"

### A lane with a finished member

    $ lit get /lanes/datetime
    /lanes/datetime  rank 4   claim this checkout
      dee   done
      b64   impl
      fw9   backlog

### Read, edit, write without JSON

    $ lit get '(parent=z6sv priority=urgent)/priority' | awk '{print $1, "normal"}' | xargs lit set
    z6sv.dbc/priority  urgent → normal
    z6sv.463/priority  urgent → normal
    z6sv.bd3/priority  urgent → normal

### Commits and PRs for a ticket

A PR's revisions are the head commits recorded for it.

    was: lit log 9uht   →   a2292fc4 2026-10-21 feat(daemon): a fed bar shows the pace…
    $ lit get 9uht/pr/revisions
    $ = brandon-unfiled-3248.9uht
    $/pr/revisions/1   5be01d3e   2026-10-21T16:02Z
    $/pr/revisions/2   a2292fc4   2026-10-21T18:55Z
    $ lit get 9uht/history
    $ = brandon-unfiled-3248.9uht
    $/history/k4e  2026-10-15T05:11Z  claude_214f…  status  backlog → plan   ⇒ claim = this checkout
    $/history/m7q  2026-10-15T06:02Z  claude_214f…  + plan/revisions/1
    $/history/r2x  2026-10-21T16:02Z  claude_214f…  + pr/revisions/1  5be01d3e
    $/history/t9c  2026-10-21T18:55Z  claude_214f…  + pr/revisions/2  a2292fc4
    $/history/w3h  2026-10-21T19:40Z  claude_214f…  status  merge → done
      reason: merged in #61
## Odd nodes and refusals

### Remove a label that is not there

    was: lit label rm 9to external
    $ lit rm 9to/labels/external
    error (reason=not_found): 9to/labels has no external; 9to/labels = [needs-design, decompose]; nothing written

### Remove a comment twice

`rm` on a comment is `set …/deleted true`: the comment stays listed, marked, so the
second `rm` finds it already removed and changes nothing.

    $ lit rm 03v/comments/2f27
    03v/comments/2f27/deleted  false → true
    $ lit rm 03v/comments/2f27
    03v/comments/2f27/deleted  true (unchanged)

### Append to a scalar

    $ lit add dee/title "in any letter case"
    error (reason=not_a_list): dee/title is a scalar; add appends to a list; to replace it: lit set dee/title "…"; nothing written

### Empty a list

    $ lit set 9to/labels '[]'
    9to/labels  [needs-design, decompose] → []

### Set with an odd number of arguments

    $ lit set dee/status done b64/status
    error (reason=usage): set takes path value pairs; b64/status has no value; nothing written

### A field that does not exist

    was: lit show jsh --field comments   →   unknown --field "comments"; valid fields: assignee, created_at, …
    $ lit get dee/phase
    error (reason=unknown_field): a ticket has no field phase; phases are columns: lit get dee/status; nothing read
      ticket fields: blocked_by blockers blocks body checklist claim claim_ttl columns comments
                     created deleted held history id labels lane lanes level parent plan pr
                     priority prompt stage status tier title topic updated

### A glob that matches nothing

    $ lit set 'ogn.*/priority' urgent
    error (reason=no_match): ogn.* matched no tickets (ogn has no children); nothing written

### A selector with an unknown key

    was: lit ls --ids … --columns id,status   →   unknown --columns name "status"; valid columns: … state …
    $ lit get '(state=backlog)/title'
    error (reason=unknown_field): selector key state is not a field; did you mean status; nothing read

### A selector value no column graph declares

    was: lit ls --status closed,open,in_progress,blocked --search b2q.   (four times, unchanged)
    $ lit get '(status=blocked)/title'
    error (reason=invalid_value): no resolved column graph has a column blocked; nothing read
      columns: backlog plan impl review merge done dup superseded obsolete wontfix; audit under rin; live-verify under mel
      blocked is derived: lit get '(blocked_by/count>0)/title'

### Stdin twice in one command

    $ lit set 8ro6/body - 8ro6/prompt - < /tmp/8ro6/both.md
    error (reason=usage): - reads stdin once per command; it appears twice (8ro6/body, 8ro6/prompt); nothing written

### A `.` where a `/` belongs

    $ lit get 792a.dee.title
    error (reason=not_found): no ticket 792a.dee.title: 792a.dee has no child title; for the field: lit get 792a.dee/title; nothing read

### A segment two tickets share

    was: lit show lm3.1tt   →   issue "lm3.1tt" not found
    $ lit get 1tt
    error (reason=ambiguous): 1tt names 2 tickets: lm3.1tt and i15t.1tt; add the parent's segment; nothing read

### Selector, glob, or literal left unquoted in zsh

    $ lit get (status=review)/title
    zsh: no matches found: (status=review)/title
    $ lit get unfiled.*/title
    zsh: no matches found: unfiled.*/title
    $ lit add a1h/plan/reviews {of:3 how:"/laws:law-review" verdict:pass}
    zsh: parse error near `}'

### A title with a colon and quotes in it

    was: lit import --path /tmp/cb-breakdown/vocab.yaml   →   yaml: line 2: mapping values are not allowed in this context
    $ lit add unfiled/children '{title:daemon.log is mostly unknown-field lines labels:[bug]}'
    error (reason=parse): value {…}: "is" is not k:v; quote a value that holds spaces; nothing written
    $ lit add unfiled/children - <<'EOF'
    {title:"daemon.log is ~90% 'schema: unknown field' lines" labels:[bug] topic:daemon}
    EOF
    + k4e
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.

### A write where one path of three is refused

    $ lit set 2ku/status merge 207/status merge oq5/status merge
    error (reason=not_in_next): oq5/status impl → merge refused: merge is entered only from review; nothing written
      2ku/status  review → merge   not written
      207/status  review → merge   not written
    guidance (impl): when the PR opens, `set oq5/status review`.

### Set a field to the value it already has

    was: lit done cjt   →   state_already_holds
    $ lit set dee/status impl
    dee/status  impl (unchanged)

## The old fumbles, in the new grammar

### Comment text given positionally

The comment text is the value, given positionally.

    was: lit comment add acme-supply-chain-w6m9.2 "PR #333 opened: …"
    $ lit add w6m9.2/comments "PR #333 opened: https://github.com/promptctl/links-issue-tracker/pull/333 — CycloneDX 1.6 SBOM …"
    + w6m9.2/comments/x4c

### Comment with no subcommand, or with -m or --message

    was: lit comment orange-widgets-5jo.nlf "From PR #49 review: …"
    was: lit comment add brandon-daemon-lifecycle-gad.3 -m "Completed by PR #163 …"
    was: lit comment add room-live-verify-56w --message "LIVE-VERIFIED …"
    $ lit add gad.3/comments "Completed by PR #163 …"
    + gad.3/comments/e9k

### Comment removal given the ticket id and the comment id

A comment lives under its ticket, so its path names both.

    was: lit comment rm houdini-laws-etg.4id cmt-5ae7b8da-29f3-4328-bebf-6bb988c5e60b
    $ lit rm 4id/comments/5ae
    4id/comments/5ae/deleted  false → true

### Editing a comment

    was: lit comment edit --help
    $ lit set 3h8/comments/03d "The credential file is src/oauth.ts."
    3h8/comments/03d  The credential file is src/login.ts. → The credential file is src/oauth.ts.

### Several ids handed to show

    was: lit show orange-devices-bue.1e6 orange-devices-bue.chz orange-devices-bue.hqi
    $ lit get 1e6/status chz/status hqi/status
    $ = orange-devices-bue
    $.1e6/status   backlog
    $.chz/status   impl
    $.hqi/status   done

### Comments asked for as a field

    was: lit show orange-iphone-watch-lm3.7iy --field status,labels,description,comments
    $ lit get 7iy/status 7iy/labels 7iy/body 7iy/comments
    $ = orange-iphone-watch-lm3.7iy
    $/status         plan
    $/labels         [watch]
    $/body           The watch app reads heart rate through …
    $/comments/2fa   [claude_96d7… Oct 24] Plan revision 5 drops the polling loop …

### JSON wanted for one field

    was: lit show orbit-brain-vu2t.abp --json 2>/dev/null | jq -r '.plan // .issue.plan // empty' > /tmp/abp_break/plan_v4.md
    $ lit get abp/plan/revisions/4 > /tmp/abp_break/plan_v4.md

### JSON wanted for a few fields

    was: lit show acme-scale-dy46.19m --json
    $ lit get 19m/status 19m/plan/verdict --json
    {"19m/status":"impl","19m/plan/verdict":"pass"}

### Wrong id prefix

    was: lit show orange-drink-lm3.8pk
    $ lit get orange-drink-lm3.8pk/title
    error (reason=not_found): lm3.8pk has never had topic drink; it is orange-iphone-watch-lm3.8pk; nothing read
    $ lit get 8pk/title
    Pairing survives the watch rebooting

### Filter by a field there was no flag for

    was: lit ls --topic devices
    $ lit get '(topic=devices)/title'
    $ = orange-devices-bue
    $.1e6/title   The device list shows a sweep in progress
    $.k7r/title   A connected device sweeps with an empty name

### A selector key that is not a field

    was: lit ls --epic orange-alerts-cjt
    $ lit get '(epic=cjt)/title'
    error (reason=unknown_field): selector key epic is not a field; did you mean parent; nothing read

### An epic's tickets, asked for with --epic

    was: lit ls --epic orange-alerts-cjt
    $ lit get '(parent=cjt)/status'
    $ = orange-alerts-cjt
    $.3ns/status   done
    $.7pq/status   done
    $.v2a/status   obsolete

### Next, scoped to one epic

    was: lit next --epic orange-admin-helper-b2q
    $ lit get b2q/next
    3cn   rank 3   backlog   outside this checkout's claims
      set 3cn/status plan would claim it

### A reason given to a dependency

    was: lit dep add candybar-testing-ihvy.cbz --blocked-by candybar-testing-0x6u.snf --reason "the gauge test needs the fake clock"
    $ lit add snf/blocks cbz --reason "the gauge test needs the fake clock"
    + snf/blocks/cbz
    ⇒ cbz/blocked_by  [] → [snf]

### Done on an epic whose children are all closed

    was: lit done orange-alerts-cjt
    $ lit set cjt/status done
    error (reason=unknown_field): cjt is an epic, a node: it has no status; its stage derives from its children: lit get cjt/stage; nothing written
    $ lit get cjt/stage
    terminal

### Phase given positionally

    was: lit phase brandon-unfiled-3248.8ro6 impl
    $ lit set 8ro6/status impl
    8ro6/status  plan → impl
      requirements: 1 evaluated · 1 met · 0 unmet (warning) · 0 unmet (error)
    guidance (impl): when the PR opens, `set 8ro6/status review`.

### Status changed through update

    was: lit update brandon-menus-sjhy.ju8 --status open
    $ lit set ju8/status backlog
    ju8/status  plan → backlog
    ⇒ /lanes/ju8/claim  this checkout → (none)
      requirements: 0 evaluated · 0 met · 0 unmet (warning) · 0 unmet (error)

### A resolution that is not a column

    was: lit close houdini-install-aby.80r --resolution fixed
    $ lit set 80r/status fixed
    error (reason=invalid_value): no resolved column graph has a column fixed; terminal columns: done dup superseded obsolete wontfix; nothing written

### A priority level that does not exist

    was: lit update houdini-connections-ob5 --priority high
    $ lit set ob5/priority high
    error (reason=invalid_value): ob5/priority takes normal or urgent; got high; nothing written

### Reasons passed to the verdict

The review carries its own reasons: `how` it was run, the reviewer's `result`, and the
verdict, naming the revision it judged.

    was: lit verdict mock-mock-flu.17w fail --plan 0b7ecac89fe6… --law single-enforcer --body "…"
    $ lit add 17w/plan/reviews '{of:4 how:"/laws:law-review" result:"single-enforcer: …" verdict:fail}'
    + 17w/plan/reviews/t8b  fail, of:4
    ⇒ 17w/plan/verdict  (none) → fail

### Guessed verbs: search, list, create, comments, epics

    was: lit search RacingFirstOpens
    $ lit get '(text~"RacingFirstOpens")/title'
    $ = acme-perf-kw6z
    $.5/title   RacingFirstOpens opens the store once per race
    was: lit list
    $ lit get /lanes
    …
    was: lit create --type epic --title E1 --topic t1
    $ lit add /children '{level:2 title:E1 topic:t1}'
    + v3p
    ⇒ v3p/parent = unfiled
    guidance (capture/created): the description says what and why and survives a refactor; done-when is checkable.
    was: lit comments candybar-isolated-daemon-bfab.agt
    $ lit get agt/comments
    …
    was: lit epics
    $ lit get '(level=2)/title'
    …

## The admin boundary and the store lock

### Push the backlog

`stores`, `snapshots`, `lifeboat`, `upgrade`, `downgrade`, `prefix` and `backup` answer the same way.

    was: lit sync push
    $ lit sync push
    error (reason=unknown_verb): lit has four verbs: get set add rm; sync is lit admin sync; nothing written
    $ lit admin sync
    published 14 events · fetched 2 writers

### Write while an admin sync holds the store

    was: lit verdict rm9w … --law single-enforcer   →   workspace_busy: held by pid 95859 (lit sync push --remote origin)
    $ lit add rm9w/plan/reviews '{of:2 how:"/laws:law-review" result:"single-enforcer holds" verdict:pass}'
    lit: waiting for the store; held by pid 95859 (lit admin sync) since 2s
    error (reason=workspace_busy): store held by pid 95859 (lit admin sync) for 30s; rerun unchanged; nothing written

### Start a workspace in a repo whose name yields no prefix

    was: lit init   →   issue prefix refused: repository name "ws" yields none … run lit init --prefix <prefix>
    $ lit get /
    error (reason=not_initialized): no lit workspace in this repository; lit admin init; nothing read
    $ lit admin init
    error (reason=invalid_value): repository name "ws" yields no prefix; wrote .lit/workspace.toml with prefix = ""; give it 3+ chars and rerun; nothing applied
    $ sed -i '' 's/^prefix = ""/prefix = "links"/' .lit/workspace.toml
    $ lit admin init
    initialized  prefix links
## What the examples could not say

Writing the examples above against the design found places the grammar has no
spelling. Each was tried, by a session in the transcripts or by a writer here, and
written as far as the grammar goes. What remains is open.

**Selectors and globs**

- No recursive glob, no alternation, no multi-field path. N ids × M fields is N×M
  paths, and one value on three unrelated tickets repeats the value three times.
- Glob depth: whether `5k5.*` matches grandchildren, and whether `5k5.*.*` is the
  spelling for them.
- No selector key picks history events by the field they changed, so the history of
  every labelled ticket prints whole.

**Values**

- No file form, and `-` reads stdin once per command, so N long bodies in one call
  need shell substitution, which the worktree guard refuses.
- Whether `@N` counts across the lines of a stdin list, or only within one value.

**Plans and reviews**

- A review carries its laws only as text in `result`; "a fail cites at least one
  law" has no field to test.
- No layout is given for a listed review, a listed revision, or what `get x/plan`
  prints when piped: the text alone, or the text under its revision number.

**Claims and lanes**

- A reorder that puts a held, in-flight ticket behind a `backlog` one is accepted
  and echoed; whether it should be refused or warned is unstated.
- `next` when the held lane's head is blocked by an edge from outside the lane:
  serve it, print nothing, or fall through.

**Output**

- No format is given for `get x/history` lines, including how a `--reason` prints,
  for `get /me`, or for what `lit config commit` and `lit admin sync` print.

**Fields**

- A comment's body: settable or append-only; sessions appended corrections on purpose.
- Whether a node above level 1 carries `priority`.
- Two clones that fixed one segment for two items: after sync the segment resolves
  to neither and both are listed, but no spelling addresses one of them.
