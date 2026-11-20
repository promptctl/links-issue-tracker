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
    $ lit get /workflow/states/plan/guidance
    Before any code, write the plan: what changes, in which files, and how it is proven.
    A reviewer other than the author judges it against the laws and records one verdict.

### Read the ticket under review

    was: lit show mock-mock-flu.atb
    $ lit get atb
    $ = mock-mock-flu.atb
    $/title          The mock's token refresh retries on a clock the test controls
    $/status         plan
    $/parent         mock-mock-flu
    $/lane           atb
    $/claim          claude_7f2e… (elsewhere)
    $/body           The refresh path in src/oauth.ts sleeps on the wall clock …
    $/plan           v7  digest b8fa35fac99f…
    $/plan/verdict   (none)
    $/comments/43s   [claude_7f2e…] v7 moves the clock behind the injected Clock port …

    /workflow/states/plan/guidance  Before any code, write the plan: …

### Read only the plan text

    was: lit show promptctl-brain-vu2t.abp --field plan > /tmp/abp_break/plan_v4.md
    $ lit get abp/plan > /tmp/abp_break/plan_v4.md

### Get the digest to name in the reasons comment, and see whether a verdict already exists

    was: lit show promptctl-brain-q9s8.45d | grep -o 'plan (v[0-9]*, digest [0-9a-f]*' | tail -1
    $ lit get 45d/plan/digest 45d/plan/verdict
    $ = promptctl-brain-q9s8.45d
    $/plan/digest   b8fa35fac99f2c41e0d7a6b95e3f18c24d07a9e1c6b3f50e8d2a71c4f9e06b3d
    $/plan/verdict  (none)

### Stop computing the digest by hand

    was: printf '%s' "$(cat /tmp/8ro6-ticket/plan.md)" | shasum -a 256 | cut -c1-16
    $ lit get 8ro6/plan/digest
    a02badd67281e5f0c93d14b7a8e26c0f5d91b3e47a0c62d8f1e5b9a3c07d4e16

### Check whether this plan version was already reviewed

    was: lit show promptctl-brain-vu2t.abp | grep -c "Plan review (digest f2623ef71b5d)"
    $ lit get abp/plan/verdicts
    $ = promptctl-brain-vu2t.abp
    $/plan/verdicts/r4x   fail  v3  f2623ef71b5d  laws:[nothing-unseen]
    $/plan/verdicts/9pe   fail  v4  c81e07a2d4b9  laws:[single-enforcer, one-source-of-truth]

### Read the epic's sibling tickets to see what the plan must fit beside

    was: for t in 0m9 faz 03v 0go w0o; do lit show mock-mock-flu.$t | sed -n '1,/^Epic:/p' | grep -v '^sync:'; done
    $ lit get 'flu.*/title' 'flu.*/status'
    flu.0m9/title    The mock serves streamed responses in recorded chunk order
    flu.faz/title    Credential login writes one file, read by every client
    flu.03v/title    The mock's request log names the session that sent each call
    flu.0go/title    A recorded fixture refuses a request it has no match for
    flu.w0o/title    The CLI flag set is parsed once into a typed config
    flu.0m9/status   done
    flu.faz/status   impl
    flu.03v/status   open
    flu.0go/status   open
    flu.w0o/status   plan

### Search for earlier tickets on the same mechanism before judging novelty

    was: lit ls --search powerd --status open,in_progress,closed
    $ lit get '(text~"powerd")/status'
    orange-devices-bue.6on/status   plan
    orange-devices-bue.hqi/status   done
    orange-power-k3c.2wd/status     obsolete

### Write the reasons for a fail before recording it

    was: lit comment add mock-mock-flu.atb --body-file - <<'REASONS'
    $ lit add atb/comments - <<'REASONS'
    Plan review (digest b8fa35fac99f): fail.
    1. no-ambient-temporal-coupling: step 3 retries on setTimeout, outside the injected Clock.
    2. nothing-unseen: the refresh failure is caught in step 4 and logged nowhere.
    REASONS
    + atb/comments/7kq

### Record the fail verdict, citing the laws

    was: lit verdict mock-mock-flu.atb fail --plan b8fa35fac99f… --law no-ambient-temporal-coupling --law nothing-unseen
    $ lit add atb/plan/verdicts '{fail laws:[no-ambient-temporal-coupling, nothing-unseen]}'
    + atb/plan/verdicts/r4x
    ⇒ atb/plan/verdict = fail

### Record a pass verdict

    was: lit verdict rich-joiners-er38.a1h pass --plan cea7cfc9d682… --law dataflow-not-control-flow --law types-are-the-program
    $ lit add a1h/plan/verdicts '{pass laws:[dataflow-not-control-flow, types-are-the-program]}'
    + a1h/plan/verdicts/b97
    ⇒ a1h/plan/verdict = pass

### A fail that cites no law is refused

    $ lit add atb/plan/verdicts '{fail}'
    error (reason=requires): a fail verdict requires laws; nothing written

### The plan changed between reading it and recording the verdict

The reviewer read v7. The author saved v8 while the review ran.

    $ lit add atb/plan/verdicts '{pass laws:[single-enforcer]}'
    + atb/plan/verdicts/c2m
    ⇒ atb/plan/verdict = pass

### Confirm the verdict landed, not only the comment

152 of 296 reviewers in one slice wrote a review comment and recorded no verdict.

    $ lit get atb/plan/verdict
    fail

### The plan's author passes their own plan

    $ lit add s4l/plan/verdicts '{pass laws:[one-source-of-truth]}'
    + s4l/plan/verdicts/q1z
    ⇒ s4l/plan/verdict = pass

## Plan versions and verdicts

### Has the current plan been reviewed yet?

Before review, then after a reviewer passes v43.

    was: lit show 8ro6 | grep -nE '^plan \(v|reviews of plan'
    $ lit get 8ro6/plan/verdict
    (none)
    $ lit get 8ro6/plan/verdict
    pass  815  claude_9f1e…  v43 (digest a02badd67281…)

### List every version of a plan

    $ lit get 8ro6/plan/versions
    8ro6/plan/versions
      1   9a752806…  claude_2eb9…  2026-10-21T03:12Z
      2   0b7ecac8…  claude_2eb9…  2026-10-21T04:40Z
      …
      21  f7613236…  claude_2eb9…  2026-10-24T18:40Z

### Read plan version 3

    was: lit history 8ro6 --version plan@3
    $ lit get 8ro6/plan/versions/3
    ## Plan
    1. The watcher derives its target through one builder …

### Diff two plan versions

    $ diff <(lit get 8ro6/plan/versions/20) <(lit get 8ro6/plan/versions/21)
    14c14
    < 3. The cache keys on three positional strings.
    ---
    > 3. The cache keys on a render origin.

### Record a verdict directly instead of adding one

    $ lit set atb/plan/verdict pass
    error (reason=derived): atb/plan/verdict is derived (latest verdict on the current digest); add one: lit add atb/plan/verdicts '{pass laws:[…]}'; nothing written

### Find plans waiting on a reviewer

    $ lit get '(status=plan plan/verdict="")/title'
    8ro6/title   The render cache takes a render origin, not three positional strings
    nlf/title    A level's form is decided once, at parse

### Revise a plan after it passed

    was: lit update 8ro6 --plan-file /tmp/8ro6-ticket/plan.md
    $ lit set 8ro6/plan - < /tmp/8ro6-ticket/plan.md
    8ro6/plan  v42 → v43
    ⇒ 8ro6/plan/digest = a02badd67281…
    ⇒ 8ro6/plan/verdict = (none)   was pass on v42

### Revise a plan while the ticket is already in impl

    $ lit set 8ro6/plan - < /tmp/8ro6-ticket/plan.md
    8ro6/plan  v43 → v44
    ⇒ 8ro6/plan/digest = 7e0f15c2…
    ⇒ 8ro6/plan/verdict = (none)   was pass on v43

### My verdict lands on a version I did not read

I read v21; the author wrote v22 while I was reviewing.

    was: lit verdict 8ro6 pass --plan f76132367de4b85c… --law one-source-of-truth   (refused: stale digest)
    $ lit add 8ro6/plan/verdicts '{pass laws:[one-source-of-truth]}'
    + 8ro6/plan/verdicts/81f
    ⇒ 8ro6/plan/verdict = pass (v22, digest 3c1e9a04…)

## Working a ticket, start to done

### Find the next ticket

    was: lit next
    $ lit get /next
    /next   s4l   /lanes/history 1/3   top-ranked unclaimed lane

### See the whole lane before starting it

    $ lit get /lanes/history
    /lanes/history  rank 1
      s4l   open
      7ka   open
      m3d   open

### Start it

    was: lit start orange-history-4y3.s4l
    $ lit set s4l/status plan
    s4l/status  open → plan
    ⇒ s4l/claim = this checkout
    ⇒ /lanes/history  held by this checkout

### Start a ticket that is its own lane

    was: lit start brandon-menus-sjhy.ju8
    $ lit set ju8/status plan
    ju8/status  open → plan
    ⇒ ju8/claim = this checkout

### Starting a ticket behind another in its lane is refused

    $ lit set fw9/status plan
    error (reason=blocked): fw9/status open → plan; fw9/blocked_by is [b64], ahead of it in /lanes/datetime; nothing written

### Write the plan from stdin

    was: lit update orange-history-4y3.s4l --plan-file /tmp/s4l-plan.md
    $ lit set s4l/plan - <<'EOF'
    1. Move the history query behind HistoryStore (src/history/store.ts).
    2. …
    Proof: test/history.test.ts covers an empty store and a 10k-row store.
    EOF
    s4l/plan  (none) → v1  digest 3e1a9c0d77b2

### Revise the plan after a fail

    was: lit update brandon-unfiled-3248.8ro6 --plan-file /tmp/8ro6-ticket/plan.md && lit show brandon-unfiled-3248.8ro6 | grep -nE '^plan \(v|reviews of plan'
    $ lit set 8ro6/plan - < /tmp/8ro6-ticket/plan.md
    8ro6/plan  v21 → v22  digest f76132367de4 → a02badd67281
    ⇒ 8ro6/plan/verdict  fail → (none)

### Read the previous plan version to diff against

    was: lit history brandon-unfiled-3248.8ro6 --version plan@21
    $ lit get 8ro6/plan/versions/21 > /tmp/8ro6-ticket/plan.v21.md

### Move to impl once the plan has passed

    was: lit phase orange-history-4y3.s4l --to impl
    $ lit set s4l/status impl
    s4l/status  plan → impl

### Moving to impl with no verdict is refused

    $ lit set s4l/status impl
    error (reason=gate): s4l/status plan → impl is gated by /workflow/states/plan/gate
      plan/verdict=pass   s4l/plan/verdict = (none)
    nothing written

### Moving to impl on a failed plan is refused

    $ lit set atb/status impl
    error (reason=gate): atb/status plan → impl is gated by /workflow/states/plan/gate
      plan/verdict=pass   atb/plan/verdict = fail (v7)
    nothing written

### Trying to finish a ticket whose plan was never reviewed

    was: lit done orange-devices-bue.355
    $ lit set 355/status done
    error (reason=not_in_next): 355/status plan → done; /workflow/states/plan/next = [impl]; nothing written

### Moving an open ticket straight to merge is refused

    was: lit phase promptctl-schema-reversal-u0if.0ca --to merge
    $ lit set 0ca/status merge
    error (reason=not_in_next): 0ca/status open → merge; /workflow/states/open/next = [plan]; nothing written

### Hand the branch to review

    was: lit phase orange-history-4y3.s4l --to review
    $ lit set s4l/status review
    s4l/status  impl → review

### Move to merge after the last review pass

    was: lit phase orange-history-4y3.s4l --to merge
    $ lit set s4l/status merge
    s4l/status  review → merge

### Finish it; the lane hands this checkout the next ticket

    was: lit done template-unfiled-792a.dee
    $ lit set dee/status done
    dee/status  merge → done
    ⇒ dee/claim  this checkout → (none)
    ⇒ /lanes/datetime  head is now b64
    ⇒ b64/claim  (none) → this checkout

### Finish it with the merge on record

    was: lit done houdini-laws-etg.79h --reason "Merged in #129 (07e20a1). Follow-up: houdini-laws-etg.tva."
    $ lit set 79h/status done 79h/reason "Merged in #129 (07e20a1). Follow-up: houdini-laws-etg.tva."
    79h/status  merge → done
    79h/reason  "" → "Merged in #129 (07e20a1). Follow-up: houdini-laws-etg.tva."
    ⇒ 79h/claim  this checkout → (none)
    ⇒ /lanes/scene-load  head is now lbc
    ⇒ lbc/claim  (none) → this checkout

### Finish the last ticket in a lane

    $ lit set fw9/status done
    fw9/status  merge → done
    ⇒ fw9/claim  this checkout → (none)
    ⇒ /lanes/datetime  finished; this checkout holds no lane

### See what is served after the lane is finished

    $ lit get /next
    /next   bryp   /lanes/sync-p0 1/1   top-ranked unclaimed lane

### Starting a ticket whose lane another checkout holds is refused

    was: lit start links-plan-review-o3mh.avl
    $ lit set avl/status plan
    error (reason=claimed): avl is the head of /lanes/plan-review, held by stream ufijgjuy (elsewhere) · 5m; to take it over: lit rm avl/claim; nothing written

### Take the lane over: drop the claim, then start

    was: lit start links-plan-review-o3mh.avl --take
    $ lit rm avl/claim
    - avl/claim  stream ufijgjuy (elsewhere)
    ⇒ /lanes/plan-review  held by (none)
    $ lit set avl/status plan
    avl/status  open → plan
    ⇒ avl/claim = this checkout
    ⇒ /lanes/plan-review  held by this checkout

### Take over a ticket another checkout left in impl

    was: lit start candybar-daemon-health-ajx0.co4 --take
    $ lit rm co4/claim
    - co4/claim  stream 8c1d02aa (elsewhere)
    $ lit set co4/status review
    co4/status  impl → review
    ⇒ co4/claim = this checkout
    ⇒ /lanes/daemon-health  held by this checkout

### Check who holds a lane before deciding to take it

    was: lit backlog 2>&1 | grep -n -B2 -A3 -i 'bryp\|claim'
    $ lit get /lanes/plan-review
    /lanes/plan-review  rank 3
      avl   open   stream ufijgjuy (elsewhere), claimed 5m ago
      bjm   open

### Push ticket writes at the end of the session

    was: lit sync push
    $ lit admin sync push
    pushed

## Claims

### Take over a lane whose holder is dead

    was: lit start 9u9 --take
    $ lit add 9u9/comments "Took the px1f lane from voice-di1: no process has that worktree as cwd, no tmux pane, tree clean."
    + 9u9/comments/b7a0
    $ lit rm 9u9/claim
    - 9u9/claim   stream voice-di1 (elsewhere) · 3h
    ⇒ /lanes/px1f/claim = (none)
    $ lit set 9u9/status review
    9u9/status  impl → review
    ⇒ 9u9/claim = this checkout

### A claim that has expired

    $ lit get bci/claim
    claude_214f… (elsewhere) · since 2026-10-15T05:11Z · expired 09:11Z (claim_ttl 4h from /)
    $ lit get /next
    /next   bci   /lanes/scene-load 1/2   top-ranked lane; its claim expired

### A contested lane

Two checkouts each started a ticket in px1f before either saw the other.

    was: lit start 9u9   →   claimed here: … contested by stream ufijgjuy …
    $ lit get /lanes/px1f
    /lanes/px1f  rank 2
      9u9   impl   stream voice-di1 (elsewhere) · 3h
      xuv   open
      31y   open

### What next serves when every lane is held elsewhere

    was: lit next
    $ lit get /next
    /next  (none): every lane with an open head is held elsewhere
      /lanes/store-bugs   rank 1  head 9r8  stream ufijgjuy (elsewhere) · 2h
      /lanes/checkpoint   rank 2  head l1x  claude_f335… (elsewhere) · 40m
      /lanes/lm3          rank 3  head 7iy  stream 4c9d… (elsewhere) · 5m

### An urgent lane interrupts the one I hold

    $ lit set c44/priority urgent
    c44/priority  normal → urgent
    ⇒ /next = c44 (urgent head of /lanes/c44, rank 1, above /lanes/datetime held by this checkout)

### A claim on a lane versus on a solo ticket

    $ lit get /lanes/datetime hf4/lane hf4/claim
    /lanes/datetime  rank 4
      dee   impl   this checkout
      b64   open
      fw9   open
    hf4/lane    hf4
    hf4/claim   (none)

### Who am I writing as?

    was: lit whoami   →   unknown command "whoami"
    $ lit get dee/claim
    this checkout

## Reading

### Read one ticket

    was: lit show promptctl-warnings-2d80.49k
    $ lit get 49k
    $ = promptctl-warnings-2d80.49k
    $/title          A warning printed twice in one turn is shown once
    $/status         merge
    $/parent         promptctl-warnings-2d80
    $/lane           49k
    $/claim          this checkout
    $/body           The bar dedupes by text, but two warnings with …
    $/plan           v3  digest 9a7528062561…
    $/plan/verdict   pass

    /workflow/states/merge/guidance  Once it has merged, close it.

### Read one field

    was: lit show links-plan-review-o3mh --field status
    $ lit get o3mh/status
    done

### Read a few fields of one ticket

    was: lit show brandon-unfiled-3248.9uht --field created_at,updated_at
    $ lit get 9uht/created_at 9uht/updated_at
    $ = brandon-unfiled-3248.9uht
    $/created_at   2026-10-19T22:52:43Z
    $/updated_at   2026-10-21T04:10:09Z

### Read only the body

    was: lit show houdini-laws-etg.2ka | sed -n '/^description:/,/^Epic:/p'
    $ lit get 2ka/body
    Too-large tool results go whole to a file, and the reply names the path …

### Pull the body out, edit it, write it back

    was: lit show promptctl-backlog-3vck.nhy | awk '/^description:/{f=1;next} /^Epic:/{f=0} f' > /tmp/nhy.md; lit update promptctl-backlog-3vck.nhy --description-file /tmp/nhy.md
    $ lit get nhy/body > /tmp/nhy.md
    $ lit set nhy/body - < /tmp/nhy.md
    nhy/body  v3 → v4

### Put back an earlier description

    was: lit history links-statusline-e9pl.yjx --version description@1 > /private/tmp/yjx-v1.txt
    $ lit get yjx/body/versions/1 > /private/tmp/yjx-v1.txt
    $ lit set yjx/body - < /private/tmp/yjx-v1.txt
    yjx/body  v4 → v5

### Read only the comments

    was: lit show brandon-unfiled-3248.8ro6 | sed -n '/^comments:/,$p'
    $ lit get 8ro6/comments
    $ = brandon-unfiled-3248.8ro6
    $/comments/43s   [claude_64e3… Oct 21] Facts from PR #43: the watcher reads …
    $/comments/7kq   [claude_9a1b… Oct 22] Plan review (digest f76132367de4): fail. …
    $/comments/b0e   [claude_9a1b… Oct 23] Plan v43 (digest a02badd67281) passed law review.

### Read one comment to quote it

    was: lit show orange-admin-helper-b2q.8ew | sed -n '/cmt-e28a7f6d/,/cmt-507e36b0/p'
    $ lit get 8ew/comments/e28
    [claude_41c0… Oct 24] The helper's install path is read from one place: …

### List an epic's children

    was: lit children candybar-browser-surface-97sw
    $ lit get 97sw/children
    [97sw.1c2, 97sw.d4e, 97sw.k9f, 97sw.p0s]

### See every child's status in one read

    was: lit children candybar-testing-0x6u --columns id,state,blocked,title | grep -v closed
    $ lit get '0x6u.*/status'
    0x6u.4bt/status   open
    0x6u.snf/status   impl
    0x6u.1ur/status   done
    0x6u.yzt/status   open

### Read an epic without its children's bodies

    was: lit show orange-alerts-cjt | sed -n '/^description:/,/^children:/p'
    $ lit get cjt
    $ = orange-alerts-cjt
    $/title      Alerts fire on threshold crossings and say what crossed
    $/type       epic
    $/body       Alerts today fire on every sample above the line …
    $/children   [cjt.3ns, cjt.7pq, cjt.v2a]
    $/lanes      [/lanes/alerts]

### Read a child without the epic reprinted under it

    was: lit show promptctl-testing-c3u.4xm | awk '/^Epic:/{exit} 1'
    $ lit get 4xm
    $ = promptctl-testing-c3u.4xm
    $/title      The replay test waits on the brain's own signal, not a sleep
    $/status     open
    $/parent     promptctl-testing-c3u
    $/lane       4xm
    $/body       test/replay.test.ts sleeps 2s and then asserts …

### Read a lane

    $ lit get /lanes/datetime
    /lanes/datetime  rank 4
      dee   impl   this checkout
      b64   open
      fw9   open

### Read several tickets in one call

    was: lit show orange-devices-bue.1e6; lit show orange-devices-bue.chz; lit show orange-devices-bue.hqi
    $ lit get 1e6 chz hqi
    $ = orange-devices-bue.1e6
    $/title    The device list shows a sweep in progress
    $/status   open
    …
    $ = orange-devices-bue.chz
    …
    $ = orange-devices-bue.hqi
    …

### Read the same fields across several tickets

    was: lit ls --ids houdini-laws-etg.0yy,houdini-laws-etg.y04 --columns id,state,assignee,updated_at --format table
    $ lit get 0yy/status 0yy/claim 0yy/updated_at y04/status y04/claim y04/updated_at
    0yy/status       impl
    0yy/claim        stream 4f1c09e2 (elsewhere)
    0yy/updated_at   2026-10-14T23:02:51Z
    y04/status       open
    y04/claim        (none)
    y04/updated_at   2026-10-12T08:40:17Z

### Check the status after a write

The write's echo already says it; this is the read a later session makes.

    was: lit done promptctl-warnings-2d80.49k; lit show promptctl-warnings-2d80.49k | grep -E "^(status|phase):"
    $ lit get 49k/status
    done

### Read what blocks a ticket

    was: lit show orange-devices-bue.2af 2>&1 | grep -A3 blocked_by
    $ lit get 2af/blocked_by
    [orange-device-sources-rec]

### Read the workflow a ticket resolves to

    was: lit workflows show promptctl-brain-q9s8.45d
    $ lit get 45d/workflow
    45d/workflow  /workflow
      open    initial:true  next:[plan]
      plan    next:[impl]  gate:plan/verdict=pass
      impl    next:[review]
      …

### See who claimed a ticket and when

    was: lit history houdini-laws-etg.6op 2>&1 | tail -8
    $ lit get 6op/history
    6op/history/1   Oct 15 05:11  claude_214f…  status open → plan
    6op/history/2   Oct 15 05:40  claude_214f…  plan (none) → v1
    6op/history/3   Oct 15 09:02  claude_8e0c…  plan/verdicts + c71 pass

### Find claims nobody has touched in half a day

    was: lit orphaned
    $ lit get '(status=plan updated<-12h)/claim'
    houdini-laws-etg.bci/claim   claude_5d2e… (elsewhere)

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
    was: for id in …; do lit show $id | grep '^status:'; done
    $ lit get '*/status'
    792a.dee/status   impl
    792a.b64/status   open
    792a.fw9/status   open
    rin.2ae/status    review
    rin.9u9/status    done
    …

### Title, status, and lane of every ticket, one line per field
    $ lit get '*/title' '*/status' '*/lane'
    792a.dee/title    dateInZone accepts zone names in any letter case
    792a.dee/status   impl
    792a.dee/lane     datetime
    792a.b64/title    dateInZone accepts fixed offsets like +05:30
    792a.b64/status   open
    792a.b64/lane     datetime
    rin.2ae/title     define and template share one namespace
    rin.2ae/status    review
    rin.2ae/lane      2ae
    …

### Status of three tickets in another checkout's lane
    was: for t in 5tv zi4 8l0; do lit show dy46.$t | sed -n '1,12p'; done
    $ lit get dy46.5tv/status dy46.zi4/status dy46.8l0/status
    dy46.5tv/status   done
    dy46.zi4/status   impl
    dy46.8l0/status   open

### Status of every child of an epic
    was: lit children candybar-testing-0x6u --columns id,state,blocked,title | grep -v closed
    $ lit get '0x6u.*/status'
    0x6u.snf/status   impl
    0x6u.1ur/status   open
    0x6u.4bt/status   open
    0x6u.yzt/status   done
    0x6u.5bi/status   dup

### Bodies of four tickets, without the epic tail
The old read was `sed -n '/^description:/,/^Epic:/p'`, once per ticket.

    was: for t in 2ka 4id y04 0yy; do lit show etg.$t | sed -n '/^description:/,/^Epic:/p'; done
    $ lit get etg.2ka/body etg.4id/body etg.y04/body etg.0yy/body
    etg.2ka/body   Too-large tool results go whole to a file …
    etg.4id/body   find_error_nodes skips summarization when …
    etg.y04/body   The scene loader reports a missing asset as …
    etg.0yy/body   Connections time out after 30 s with no …

### Head of one ticket: title, status, body, what blocks it
Several paths on one ticket print the ticket once, as `$`.

    was: lit show dy46.5tv | sed -n '1,12p;/^description:/,/^blocked_by:/p'
    $ lit get dy46.5tv/title dy46.5tv/status dy46.5tv/blocked_by dy46.5tv/body
    $ = links-scale-dy46.5tv
    $/title        Backlog reads stay under 200 ms at 10k tickets
    $/status       open
    $/blocked_by   bryp
    $/body         The backlog query scans every issue_history row …

### Comments on one ticket, without the rest of the ticket
    was: lit show 3248.8ro6 | sed -n '/^comments:/,$p' | cut -c1-600
    $ lit get 3248.8ro6/comments
    $ = brandon-unfiled-3248.8ro6
    $/comments/43s   [claude_64e3…] Plan v21 recorded; digest f7613236 …
    $/comments/9kq   [claude_a1f9…] Plan review (digest f76132367de4): fail. …
    $/comments/2mw   [claude_64e3…] Plan v22 answers findings 1–3 …

### One comment, to quote it in a follow-up
    was: lit show b2q.8ew | sed -n '/cmt-e28a7f6d/,/cmt-507e36b0/p'
    $ lit get b2q.8ew/comments/e28
    [claude_9c02…] Plan review (digest 56fa5f1faa63): fail. 1. one-source-of-truth: …

### Every epic's body, to audit the epics before decomposing them
    was: for e in $(lit ls --type epic --columns id | awk '{print $1}'); do lit show $e > /tmp/groom/epic-$e.txt; done
    $ lit get '(type=epic)/body' > /tmp/groom/epics.txt
    $ grep -c '' /tmp/groom/epics.txt
    41

### Titles of the children of five epics
    was: for e in 6pj 1dh f00 5d8 b2l; do lit ls --parent 76bb.$e; done
    $ lit get '76bb.6pj.*/title' '76bb.1dh.*/title' '76bb.f00.*/title' '76bb.5d8.*/title' '76bb.b2l.*/title'
    76bb.6pj.7q7/title   The gather renderer draws one row per source
    76bb.6pj.e1n/title   A source with no samples draws an empty row
    76bb.1dh.mxk/title   …
    …

### Parent of each ticket, before filing a sibling
    was: for t in …; do lit show $t | grep -A2 '^parent:' | sed -n 2p; done
    $ lit get hands-nativeui-02f/parent hands-install-dye.73t/parent hands-install-dye.8yk/parent
    hands-nativeui-02f/parent      (none)
    hands-install-dye.73t/parent   hands-install-dye
    hands-install-dye.8yk/parent   hands-install-dye

### Created and updated times of one ticket
    was: lit show 3248.9uht --field created_at,updated_at
    $ lit get 3248.9uht/created_at 3248.9uht/updated_at
    $ = brandon-unfiled-3248.9uht
    $/created_at   2026-10-19T22:52:43Z
    $/updated_at   2026-10-21T04:10:07Z

### Every open ticket's body in one file, to grep
    was: for id in $(lit ls --status open,in_progress --columns id | awk '{print $1}'); do lit show $id --no-plan > /tmp/groom/t-$id.txt; done
    $ lit get '(status=open)/body' > /tmp/groom/open-bodies.txt
    $ grep -n 'backoff' /tmp/groom/open-bodies.txt
    212:3k6u.ckq/body   … retries with exponential backoff capped at 30 s …

### History of every ticket carrying a label
    was: for id in $(lit ls --labels focus …| awk '{print $1}'); do lit history "$id" | grep -i -m1 label; done
    $ lit get '(labels=focus)/history'
    3k6u.knq/history   2026-10-20T22:52:43Z  claude_214f…  labels  [] → [focus]
    dy46.48k/history   2026-10-22T03:11:09Z  claude_96d7…  labels  [external] → [external, focus]

## Selectors

### Open bugs, by title
    was: lit ls --status open --type bug
    $ lit get '(status=open type=bug)/title'
    3248.kler/title   daemon.log is ~90% 'schema: unknown field' lines
    xu4.8pq/title     A connected device sweeps with an empty name

### Every epic
    was: lit ls --type epic
    $ lit get '(type=epic)/title'
    792a/title   Unfiled
    rin/title    Subtemplates
    6fq5/title   Theme colours
    …

### Children of an epic, by selector or by glob
`(parent=rin)` and `rin.*` expand to the same ids when every child sits directly under `rin`.

    was: lit ls --parent go-template-js-rin
    $ lit get '(parent=rin)/title'
    rin.2ae/title   define and template share one namespace
    rin.9u9/title   block falls back to its own body when undefined
    rin.k3x/title   template with no pipeline passes nil dot

### Dedupe before filing: has anyone already filed this?
Reviewers searched two or three terms before writing a ticket.

    was: lit ls --search powerd --status open,in_progress,closed; lit ls --search sweep --status open,in_progress,closed
    $ lit get '(text~"powerd")/status' '(text~"sweep")/status'
    bue.6on/status   review
    bue.1e6/status   done
    bue.chz/status   open

### Dedupe within one type, before filing a bug
    $ lit get '(text~"empty name" type=bug)/title'
    xu4.8pq/title   A connected device sweeps with an empty name

### Orphaned: in flight, no update in six hours
`stage` is derived from the two flags the engine knows, so "in flight" is one key in every subtree's dialect.

    was: lit orphaned
    $ lit get '(stage=active updated<-6h)/claim'
    etg.bci/claim   claude_f335… (elsewhere)  claimed 12h ago

### Tickets touched since yesterday
    was: lit ls --updated-after 2026-11-02
    $ lit get '*/updated_at' | awk '$2 >= "2026-11-02"'
    792a.dee/updated_at   2026-11-02T19:40:11Z
    rin.2ae/updated_at    2026-11-03T03:02:55Z

### Tickets that have comments
    was: lit ls --query 'has:comments'
    $ lit get '(status=open)/comments'
    3248.8ro6/comments/43s   [claude_64e3…] Plan v21 recorded …
    3248.8ro6/comments/9kq   [claude_a1f9…] Plan review (digest f76132367de4): fail. …
    bue.6on/comments/1pa     [claude_7f2e…] powerd sleeps the sweep during a call …
    …

### First five closed tasks with comments, in rank order
    was: lit ls --query 'status:closed type:task has:comments sort:rank:asc limit:5'
    $ lit get '(status=done type=task)/title'
    792a.2ku/title   dateInZone rejects a malformed offset
    792a.207/title   date accepts a Unix timestamp string
    …   (every one, in no order)

### Archived tickets
    was: lit ls --ids hands-nativeui-02f,hands-install-dye --include-archived
    $ lit get '(archived=true)/title'
    voice-r7y/title       Voice: wake word
    voice-r7y.8rr/title   Wake word survives a sleep/wake cycle

### Restore an epic deleted in error
`rm x` is `set x/deleted true`; the inverse is the inverse.

    was: lit restore vhid-aim-sjt
    $ lit set sjt/deleted false
    sjt/deleted  true → false
    ⇒ sjt.*/deleted  true → false  (3 children)

### Every ticket in review, at every depth, in each subtree's own workflow
`rin` adds an `audit` state. `(status=review)` still matches its review tickets, because a selector matches each node's resolved state.

    $ lit get '(status=review)/title'
    rin.2ae/title      define and template share one namespace
    bue.6on/title      powerd sleeps the sweep during a call
    3248.8ro6/title    Watchers derive their targets through one builder

### Open tickets except epics
    was: lit ls --status open | grep -v epic
    $ lit get '(status=open)/title'
    792a/title       Unfiled
    792a.b64/title   dateInZone accepts fixed offsets like +05:30
    rin/title        Subtemplates
    …   (the epics are in it)

### A named set of unrelated ids, two fields each
    was: lit ls --ids etg.0yy,etg.y04 --columns id,state,assignee,updated_at
    $ lit get etg.0yy/status etg.0yy/claim etg.y04/status etg.y04/claim
    etg.0yy/status   impl
    etg.0yy/claim    claude_214f… (worktree etg-0yy)
    etg.y04/status   open
    etg.y04/claim    (none)

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
    - b06/comments/563

### Remove two stale comments in one call

    was: lit comment rm cmt-2f277e42-… && lit comment rm cmt-f5f02379-…
    $ lit rm 03v/comments/2f2 03v/comments/f5f
    - 03v/comments/2f2
    - 03v/comments/f5f

### Correct an earlier comment by appending a new one

    was: lit comment add mock-mock-flu.3h8 --body "Correction to cmt-03d4a989: …"
    $ lit add 3h8/comments "Correction to 3h8/comments/03d: the credential file is src/oauth.ts, not src/login.ts."
    + 3h8/comments/v6n

### Leave a note on a ticket this session does not own

    was: lit comment add mock-mock-flu.aus --body "From mock-mock-flu.86x: the credential login writes is src/oauth.ts …"
    $ lit add aus/comments "From flu.86x: the credential login writes is src/oauth.ts …"
    + aus/comments/j1t

## Notes between agents

### Only one author's comments

    $ lit get '8ew/comments/(by=claude_96d7)/body'
    8ew/comments/e28a/body   Plan review (digest 56fa5f1faa63): fail. …
    8ew/comments/507e/body   Plan review (digest 9c01e3a7b2f4): pass.

## Filing

### File a bug under an epic, body from stdin

    was: lit new --type bug --topic devices --parent orange-devices-bue --title "A connected device sweeps with an empty name" --description-file - <<'EOF'
    $ lit add bue/children '{title:"A connected device sweeps with an empty name" type:bug body:-}' <<'EOF'
    Repro: pair a device with no name set; the sweep row renders "" …
    EOF
    + bue.k7r

### File a task with a one-line body

    was: lit new --parent promptctl-evals-bgz --type task --topic isolation --title "The Opus verification reads what the session runs" --description "…"
    $ lit add bgz/children '{title:"The Opus verification reads what the session runs, not what the model says it is" type:task body:"Done looks like: …"}'
    + bgz.10

### File three children in one call

    was: for t in one two three; do lit new --parent $E --topic wake-word --title "ticket $t"; done
    $ lit add i15t/children '[{title:"Wake word fires on the first syllable" type:task}, {title:"Wake word ignores the TV" type:task}, {title:"Wake word logs each miss" type:task}]'
    + i15t.r2a
    + i15t.r2b
    + i15t.r2c

### File a follow-up beside a closed ticket, in its epic

    was: lit followup --on orange-settings-7du.5uw --title "The app quit by itself twice right after the Settings window opened" --type bug
    $ lit get 5uw/parent
    orange-settings-7du
    $ lit add 7du/children '{title:"The app quit by itself twice right after the Settings window opened" type:bug}'
    + 7du.a2c

### File a top-level epic

    was: lit new --type epic --topic history --title "History reads come from one store"
    $ lit add /children '{title:"History reads come from one store" type:epic}'
    + orange-m2q

### A bug filed with no epic is refused

    was: lit new --type bug --topic daemon-git-identity --title "A daemon serving sessions …"
    $ lit add /children '{title:"A daemon serving sessions commits as the wrong identity" type:bug}'
    error (reason=invalid_value): /children: a bug lives inside an epic; add it under <epic>/children; nothing written

## Bulk create

### Create an epic's tickets from one list on stdin
    was: lit import --path /tmp/w0o-groom/tickets.yaml
    $ lit add flu/children - <<'EOF'
    [{title:"Login writes the credential where the refresh reads it" type:bug},
     {title:"The refresh retries once on a 401, then surfaces it" type:task},
     {title:"Token expiry is read from the token, not assumed" type:task}]
    EOF
    + flu.8xa
    + flu.8xb
    + flu.8xc

### Create with bodies and labels, values quoted where they contain a colon
The YAML import failed on `mapping values are not allowed`. In a value, a colon inside quotes is text.

    $ lit add cb/children - <<'EOF'
    [{title:"Vocab: one word per gauge" type:task labels:[refine] body:"Done when: every gauge label is one word."},
     {title:"Vocab: units in the legend, not the label" type:task labels:[refine]}]
    EOF
    + cb.v1a
    + cb.v1b

### Dry-run a bulk create to check every item before minting ids
    $ lit add 0x6u/children - --dry-run < /tmp/epic-tickets.list
    + 0x6u/children  {title:"launchWaited returns after the first frame" type:bug}
    + 0x6u/children  {title:"The fake store answers schema v12" type:task}
    + 0x6u/children  {title:"…" type:bugg}   refused: type "bugg" is not a type
    (dry run: nothing written)

### Create a new epic and its children in one call
    was: lit import --path /tmp/ccb-tickets/candybar.yaml   (created 18 issues / tmux-epic -> candybar-tmux-events-y457)
    $ lit add /children - <<'EOF'
    [{title:"tmux server events" type:epic
      children:[{title:"Parse %output notifications" type:task},
                {title:"Reconnect after the server restarts" type:task}]}]
    EOF
    + y457
    + y457.a1
    + y457.a2

### Create tickets that depend on each other in one call
The YAML import used local keys (`plan-1 -> candybar-testing-ihvy.myy`) so one new ticket could block another.

    $ lit add ihvy/children - < /tmp/epic-tickets.list
    + ihvy.myy
    + ihvy.myz
    $ lit add ihvy.myy/blocks ihvy.myz
    + ihvy.myy/blocks/ihvy.myz

### Update many existing tickets from one list
The import help said a document with an `id` updates that ticket. Updating is `set`, with pairs.

    was: lit import --path /tmp/groom/lanes.yaml   (after two failed attempts to generate the YAML)
    $ lit set px1f.f9b/priority urgent px1f.9u9/priority urgent px1f.xuv/parent lq3
    px1f.f9b/priority   normal → urgent
    px1f.9u9/priority   normal → urgent
    px1f.xuv/parent     px1f → lq3

## Closing

### Close a merged ticket in a close-out session

    was: lit done promptctl-warnings-2d80.49k --reason "PR #332 merged as f4cfaa0"
    $ lit set 49k/status done 49k/reason "PR #332 merged as f4cfaa0"
    49k/status  merge → done
    49k/reason  "" → "PR #332 merged as f4cfaa0"
    ⇒ 49k/claim  this checkout → (none)
    ⇒ /lanes/49k  finished; this checkout holds no lane

### Close several merged tickets at once

    was: for t in 2ku 207 oq5; do lit done template-unfiled-792a.$t; done
    $ lit set 2ku/status done 207/status done oq5/status done
    2ku/status  merge → done
    207/status  merge → done
    oq5/status  merge → done
    ⇒ 2ku/claim  this checkout → (none)
    ⇒ 207/claim  this checkout → (none)
    ⇒ oq5/claim  this checkout → (none)

### Close as won't-fix, with the reason

    was: lit close houdini-install-aby.80r --resolution wontfix --reason "…"
    $ lit set 80r/status wontfix 80r/reason "Installing into the user's Houdini prefs is theirs to do; the README says how."
    80r/status  open → wontfix
    80r/reason  "" → "Installing into the user's Houdini prefs is theirs to do; the README says how."

### Won't-fix with no reason is refused

    $ lit set 80r/status wontfix
    error (reason=requires): /workflow/states/wontfix requires 80r/reason, which is empty; nothing written

### Close as obsolete, with the reason

    was: lit close promptctl-hierarchy-6cqf.2la --resolution obsolete --reason "groom: its work (a migration dropping issue_type) is ruled out by …"
    $ lit set 2la/status obsolete 2la/reason "groom: its work (a migration dropping issue_type) is ruled out by …"
    2la/status  open → obsolete
    2la/reason  "" → "groom: its work (a migration dropping issue_type) is ruled out by …"

### Close as a duplicate of the canonical ticket

    was: lit close promptctl-warnings-2d80.d66 --resolution duplicate --of promptctl-brain-lq3.9od
    $ lit set d66/status dup d66/of lq3.9od
    d66/status  open → dup
    d66/of      "" → promptctl-brain-lq3.9od

### A duplicate with no canonical is refused

    $ lit set d66/status dup
    error (reason=requires): /workflow/states/dup requires d66/of, which is empty; nothing written

### Close three duplicates into one canonical

    was: for t in d66 c7e 61x; do lit close promptctl-warnings-2d80.$t --resolution duplicate --of promptctl-brain-lq3.9od; done
    $ lit set d66/status dup d66/of lq3.9od c7e/status dup c7e/of lq3.9od 61x/status dup 61x/of lq3.9od
    d66/status  open → dup
    d66/of      "" → promptctl-brain-lq3.9od
    c7e/status  open → dup
    c7e/of      "" → promptctl-brain-lq3.9od
    61x/status  open → dup
    61x/of      "" → promptctl-brain-lq3.9od

### Close as superseded by the ticket that replaced it

    was: lit close houdini-laws-etg.2ka --resolution superseded --of houdini-laws-etg.6op --reason "user decided: too-large results go whole to a file …"
    $ lit set 2ka/status superseded 2ka/of 6op 2ka/reason "user decided: too-large results go whole to a file …"
    2ka/status  open → superseded
    2ka/of      "" → houdini-laws-etg.6op
    2ka/reason  "" → "user decided: too-large results go whole to a file …"

### Close an abandoned epic's open children with one reason, previewed first

    was: lit close dotfiles-iterm2-restore-5k5.4.1 --resolution obsolete; lit close dotfiles-iterm2-restore-5k5 --resolution obsolete
    $ lit set '(parent=5k5 status=open)/status' obsolete '(parent=5k5 status=open)/reason' "iTerm2 restore dropped" --dry-run
    5k5.4.1/status  open → obsolete
    5k5.4.1/reason  "" → "iTerm2 restore dropped"
    5k5.4.2/status  open → obsolete
    5k5.4.2/reason  "" → "iTerm2 restore dropped"
    (dry run: nothing written)

### Reopen a closed ticket

    was: lit open sanctuary-render-tgx.rkm --reason "Done-when fails: the bump has not landed"
    $ lit set rkm/status open rkm/reason "Done-when fails: the bump has not landed"
    rkm/status  done → open
    rkm/reason  "Merged in #212" → "Done-when fails: the bump has not landed"

### Park an in-flight ticket back in open and release its claim

    was: lit open links-scale-dy46.37o --reason "parked behind promptctl-sync-bryp"
    $ lit set 37o/status open 37o/reason "parked behind promptctl-sync-bryp"
    37o/status  impl → open
    37o/reason  "" → "parked behind promptctl-sync-bryp"
    ⇒ 37o/claim  this checkout → (none)
    ⇒ /lanes/37o  held by (none)

## Labels, parents, dependencies

### Add a label

    was: lit label add links-perf-kw6z.5 external
    $ lit add kw6z.5/labels external
    + kw6z.5/labels/external

### Remove a label, then start the ticket

    was: lit label rm candybar-effort-gauge-9to external && lit start candybar-effort-gauge-9to
    $ lit rm 9to/labels/external
    - 9to/labels/external
    $ lit set 9to/status plan
    9to/status  open → plan
    ⇒ 9to/claim = this checkout

### Clear the label from the epics the pass finished

    was: for e in orange-history-4y3 orange-devices-bue orange-iphone-watch-lm3; do lit label rm $e decompose; done
    $ lit rm 4y3/labels/decompose bue/labels/decompose lm3/labels/decompose
    - 4y3/labels/decompose
    - bue/labels/decompose
    - lm3/labels/decompose

### Move a ticket under a different epic

    was: lit parent set --child links-unfiled-fc79.rm9w --parent promptctl-testing-0oqd
    $ lit set rm9w/parent 0oqd
    rm9w/parent  links-unfiled-fc79 → promptctl-testing-0oqd

### Move a ticket under an epic that carries its own workflow

    $ lit set q4p/parent lq3
    q4p/parent  (none) → promptctl-brain-lq3
    ⇒ q4p/workflow  /workflow → promptctl-brain-lq3/workflow

### Move three tickets under one epic

    was: for t in bz6 3cz ths; do lit parent set --child tmux-pane-detail-aha.$t --parent tmux-capture-restore-99o; done
    $ lit set bz6/parent 99o 3cz/parent 99o ths/parent 99o
    bz6/parent  tmux-pane-detail-aha → tmux-capture-restore-99o
    3cz/parent  tmux-pane-detail-aha → tmux-capture-restore-99o
    ths/parent  tmux-pane-detail-aha → tmux-capture-restore-99o

### Make a ticket wait on one in another epic

    was: lit dep add links-scale-dy46.37o --blocked-by promptctl-sync-bryp
    $ lit add bryp/blocks 37o
    + bryp/blocks/37o
    ⇒ 37o/blocked_by  [] → [promptctl-sync-bryp]

### Make five tickets wait on one

    was: for t in 0jy 04u vap 73s 7v5; do lit dep add houdini-scene-iteration-jcj.$t --blocked-by houdini-scene-iteration-jcj.l1e; done
    $ lit add l1e/blocks '[0jy, 04u, vap, 73s, 7v5]'
    + l1e/blocks/0jy
    + l1e/blocks/04u
    + l1e/blocks/vap
    + l1e/blocks/73s
    + l1e/blocks/7v5
    ⇒ 0jy/blocked_by  [] → [houdini-scene-iteration-jcj.l1e]
    ⇒ 04u/blocked_by  [] → [houdini-scene-iteration-jcj.l1e]
    ⇒ vap/blocked_by  [] → [houdini-scene-iteration-jcj.l1e]
    ⇒ 73s/blocked_by  [] → [houdini-scene-iteration-jcj.l1e]
    ⇒ 7v5/blocked_by  [] → [houdini-scene-iteration-jcj.l1e]

### Make one ticket wait on a sibling in the same epic

The old CLI refused this ("Do not set 'blocks' relationships between two issues in the same epic").

    was: lit dep add --from copirate-efficiency-235.1 --to copirate-efficiency-235.2 --type blocks
    $ lit add 235.1/blocks 235.2
    + 235.1/blocks/235.2
    ⇒ 235.2/blocked_by  [] → [copirate-efficiency-235.1]

### Remove a dependency

    was: lit dep rm houdini-connections-ob5 --blocked-by houdini-scene-iteration-jcj.l1e
    $ lit rm l1e/blocks/ob5
    - l1e/blocks/ob5
    ⇒ ob5/blocked_by  [houdini-scene-iteration-jcj.l1e] → []

## Several fields on one ticket

### Title, body, and type in one call
    was: lit update e9pl.9ym --title "…" --type task --description-file -
    $ lit set e9pl.9ym/title "The statusline names the lane it serves" e9pl.9ym/type task e9pl.9ym/body - <<'EOF'
    The statusline shows the lane's head and its position (2/3).
    EOF
    e9pl.9ym/title   The statusline shows rank → The statusline names the lane it serves
    e9pl.9ym/type    bug → task
    e9pl.9ym/body    v4 → v5  (+1 −6 lines)

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
    $ lit set '(parent=z6sv priority=urgent)/priority' normal
    z6sv.dbc/priority   urgent → normal
    z6sv.463/priority   urgent → normal
    z6sv.bd3/priority   urgent → normal
    z6sv.ix9/priority   urgent → normal
    z6sv.86e/priority   urgent → normal

### Urgent on an epic and every child
    was: for id in w1p w1p.1 w1p.2 w1p.3; do lit update macklebox-safety-$id --priority 0; done
    $ lit set w1p/priority urgent 'w1p.*/priority' urgent
    w1p/priority     normal → urgent
    w1p.1/priority   normal → urgent
    w1p.2/priority   normal → urgent
    w1p.3/priority   normal → urgent

### Dependency fan-out: seven tickets wait on one blocker
    was: lit dep add lm3.fyh --blocked-by rec; lit dep add lm3.u33 --blocked-by rec; … (7 calls)
    $ lit add rec/blocks '[lm3.fyh, lm3.u33, lm3.cn0, lm3.7iy, lm3.8pk, lm3.1tt, lm3.497]'
    + rec/blocks/lm3.fyh
    + rec/blocks/lm3.u33
    + rec/blocks/lm3.cn0
    + rec/blocks/lm3.7iy
    + rec/blocks/lm3.8pk
    + rec/blocks/lm3.1tt
    + rec/blocks/lm3.497
    ⇒ lm3.fyh/blocked_by   [] → [rec]
    ⇒ lm3.u33/blocked_by   [] → [rec]
    …

### One ticket waits on two blockers
    $ lit add 3248.kler/blocks sjhy.ju8
    + 3248.kler/blocks/sjhy.ju8
    $ lit add bryp/blocks sjhy.ju8
    + bryp/blocks/sjhy.ju8

### Swap the blocker on two tickets
    was: for t in bsog.sfi 7qoh.8nx; do lit dep rm $t --blocked-by uoz0.nx9; lit dep add $t --blocked-by px1f.e4u; done
    $ lit rm uoz0.nx9/blocks/bsog.sfi uoz0.nx9/blocks/7qoh.8nx
    - uoz0.nx9/blocks/bsog.sfi
    - uoz0.nx9/blocks/7qoh.8nx
    $ lit add px1f.e4u/blocks '[bsog.sfi, 7qoh.8nx]'
    + px1f.e4u/blocks/bsog.sfi
    + px1f.e4u/blocks/7qoh.8nx

### Drop one dependency
    was: lit dep rm houdini-connections-ob5 --blocked-by jcj.l1e
    $ lit rm jcj.l1e/blocks/ob5
    - jcj.l1e/blocks/ob5
    ⇒ ob5/blocked_by   [jcj.l1e] → []

### Reparent twelve tickets
    was: for p in "bij aj0" "o4f aj0" "czk aj0" …; do set -- $p; lit parent set --child flu.$1 --parent flu.$2; done
    $ lit set flu.bij/parent flu.aj0 flu.o4f/parent flu.aj0 flu.czk/parent flu.aj0 flu.w0o/parent flu.aj0
    flu.bij/parent   flu → flu.aj0
    flu.o4f/parent   flu → flu.aj0
    flu.czk/parent   flu → flu.aj0
    flu.w0o/parent   flu → flu.aj0

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
    $ lit add '(type=epic)/labels' decompose --dry-run
    (type=epic) = [4y3, bue, lm3, kg2, xu4, 7du, cjt]
    + 4y3/labels/decompose
    + bue/labels/decompose
    + lm3/labels/decompose
    + kg2/labels/decompose
    + xu4/labels/decompose
    + 7du/labels/decompose
    + cjt/labels/decompose
    (dry run: nothing written)

### Clear a label from every epic that still has it
    $ lit rm '(labels=decompose)/labels/decompose'
    - kg2/labels/decompose
    - cjt/labels/decompose

### Close stale tickets as obsolete, each with the reason obsolete requires
    was: for t in etg.8qa etg.3d4 etg.ugm etg.aj3 2pq; do lit close $t --resolution obsolete --reason "ws_client deleted in #55"; done
    $ lit set etg.8qa/status obsolete etg.8qa/reason "ws_client deleted in #55" etg.3d4/status obsolete etg.3d4/reason "ws_client deleted in #55" etg.ugm/status obsolete etg.ugm/reason "ws_client deleted in #55"
    etg.8qa/status   open → obsolete
    etg.8qa/reason   (none) → ws_client deleted in #55
    etg.3d4/status   open → obsolete
    etg.3d4/reason   (none) → ws_client deleted in #55
    etg.ugm/status   open → obsolete
    etg.ugm/reason   (none) → ws_client deleted in #55

### Close every ticket matching a selector as obsolete, with the reason it requires
    $ lit set '(text~"ws_client" status=open)/status' obsolete '(text~"ws_client" status=open)/reason' "ws_client deleted in #55" --dry-run
    (text~"ws_client" status=open) = [etg.8qa, etg.3d4, etg.ugm, etg.aj3, 2pq]
    etg.8qa/status   open → obsolete
    etg.8qa/reason   (none) → ws_client deleted in #55
    …
    (dry run: nothing written)

### Mark three merged tickets done
    was: for t in 2ku 207 oq5; do lit done template-unfiled-792a.$t; done
    $ lit set 792a.2ku/status done 792a.207/status done 792a.oq5/status done
    792a.2ku/status   merge → done
    ⇒ 792a.2ku/claim  this checkout → (none)
    792a.207/status   merge → done
    ⇒ 792a.207/claim  this checkout → (none)
    792a.oq5/status   merge → done
    ⇒ 792a.oq5/claim  this checkout → (none)

### Close everything sitting in merge, after checking what that is
    $ lit set '(status=merge)/status' done --dry-run
    (status=merge) = [lq3.808, backlog-q4p, lq3.hwi, lq3.1dt]
    lq3.808/status       merge → done
    backlog-q4p/status   merge → done
    lq3.hwi/status       merge → done
    lq3.1dt/status       merge → done
    (dry run: nothing written)

### Reopen several tickets in one call
    was: lit open 2bs 2bs.tsm 2bs.99c   (refused: usage: lit open <id>)
    $ lit set 2bs.tsm/status open 2bs.99c/status open
    2bs.tsm/status   done → open
    2bs.99c/status   done → open

### Archive every child of an abandoned epic
    was: for id in …; do lit archive $id --reason "voice dropped"; done
    $ lit set 'voice-r7y.*/archived' true --dry-run
    voice-r7y.* = [voice-r7y.8rr, voice-r7y.ux7, voice-r7y.k2q]
    voice-r7y.8rr/archived   false → true
    voice-r7y.ux7/archived   false → true
    voice-r7y.k2q/archived   false → true
    (dry run: nothing written)

### Delete two probe tickets
    was: lit delete ef3.9yn --reason "leftover probe"; lit delete ef3.vqr --reason "filed in error"
    $ lit rm ef3.9yn ef3.vqr
    - ef3.9yn
    - ef3.vqr

### Purge an abandoned epic's whole subtree
    was: for id in 3aj.8k1 3aj.evr …; do lit purge $id --yes; done
    $ lit rm '76j.3aj.*' --purge --dry-run
    76j.3aj.* = [76j.3aj.8k1, 76j.3aj.evr, 76j.3aj.q0p]
    - 76j.3aj.8k1   (comments 3, labels 0, edges 2)
    - 76j.3aj.evr   (comments 0, labels 1, edges 1)
    - 76j.3aj.q0p   (comments 1, labels 0, edges 0)
    (dry run: nothing written)

### Remove two stale comments, then add the corrected one
    was: lit comment rm cmt-2f277e42-…; lit comment rm cmt-f5f02379-…; lit comment add flu.03v --body "…"
    $ lit rm flu.03v/comments/2f2 flu.03v/comments/f50
    - flu.03v/comments/2f2
    - flu.03v/comments/f50
    $ lit add flu.03v/comments "Login writes src/oauth.ts; the token refresh is in src/refresh.ts."
    + flu.03v/comments/7hd

### Remove one of a pair of identical comments posted to two tickets
    was: lit comment rm cmt-40f73118-…
    $ lit rm c3u.isp/comments/40f
    - c3u.isp/comments/40f

### Remove every comment one session left on a ticket
    $ lit rm 'ef3.b06/comments/*' --dry-run
    ef3.b06/comments/* = [a1k, a1m, c9p]
    - ef3.b06/comments/a1k
    - ef3.b06/comments/a1m
    - ef3.b06/comments/c9p
    (dry run: nothing written)

### The same comment on every child of an epic
    $ lit add '0x6u.*/comments' "Groom 2026-11-03: re-verified against master 2d7ee77."
    + 0x6u.snf/comments/m1c
    + 0x6u.1ur/comments/m1d
    + 0x6u.4bt/comments/m1e

### The same comment on three unrelated tickets
    was: for t in c3u.isp fxnj.cjq q9s8.qsk; do lit comment add $t --body "Fixed by #438 …"; done
    $ lit add c3u.isp/comments "Fixed by #438 (c3u.89i): the doubled emptying row came from the stale buffer."
    + c3u.isp/comments/q2a
    $ lit add fxnj.cjq/comments "Fixed by #438 (c3u.89i): the doubled emptying row came from the stale buffer."
    + fxnj.cjq/comments/q2b
    $ lit add q9s8.qsk/comments "Fixed by #438 (c3u.89i): the doubled emptying row came from the stale buffer."
    + q9s8.qsk/comments/q2c

### Eleven different grooming notes on eleven tickets
    was: lit comment add vu2t.7ro --body 'Grooming 2026-10-28: …'; lit comment add i15t.qg8 --body '…'; … (11 in one command)
    $ lit add vu2t.7ro/comments "Grooming 2026-10-28: superseded by the lq3 lane; kept for the repro."
    + vu2t.7ro/comments/b4n

### Rewrite six descriptions from a directory of files
    was: for t in b06 9e3 avb 0ho ll5 bl1; do lit update ef3.$t --description-file /tmp/groom-7/desc/$t.md --reason "groom: …"; done
    $ lit set ef3.b06/body "$(cat /tmp/groom-7/desc/b06.md)" ef3.9e3/body "$(cat /tmp/groom-7/desc/9e3.md)" ef3.avb/body "$(cat /tmp/groom-7/desc/avb.md)"
    ef3.b06/body   v3 → v4  (+12 −5 lines)
    ef3.9e3/body   v1 → v2  (+8 −8 lines)
    ef3.avb/body   v5 → v6  (+3 −1 lines)

### One description from stdin, the common case
    was: lit update 8x3 --description-file /tmp/groom-scope/8x3.txt
    $ lit set iwpa.8x3/body - < /tmp/groom-scope/8x3.txt
    iwpa.8x3/body   v2 → v3  (+14 −9 lines)

## Lanes

### Three new tickets that must be worked in order, as one lane
Three tickets in a row, one call. Order on stdin is the order in the lane.

    $ lit add /lanes/datetime - <<'EOF'
    [{title:"dateInZone accepts zone names in any letter case" type:bug parent:792a},
     {title:"dateInZone accepts fixed offsets like +05:30" type:task parent:792a},
     {title:"date parses RFC 3339 with fractional seconds" type:task parent:792a}]
    EOF
    + 792a.dee
    + 792a.b64
    + 792a.fw9
    + /lanes/datetime  [dee, b64, fw9]
    ⇒ b64/blocked_by = dee
    ⇒ fw9/blocked_by = b64
    ⇒ /lanes/datetime/rank = 9

### A lane from three existing tickets
    was: for t in 9r8 hf4 a53; do lit update 3k6u.$t --lane store-bugs --reason "independent of the checkpoint chain"; done
    $ lit add /lanes/store-bugs '[3k6u.9r8, 3k6u.hf4, 3k6u.a53]'
    + /lanes/store-bugs  [3k6u.9r8, 3k6u.hf4, 3k6u.a53]
    ⇒ 3k6u.9r8/lane = store-bugs
    ⇒ 3k6u.hf4/lane = store-bugs
    ⇒ 3k6u.a53/lane = store-bugs
    ⇒ 3k6u.hf4/blocked_by = 3k6u.9r8
    ⇒ 3k6u.a53/blocked_by = 3k6u.hf4

### Add one existing ticket to the end of a lane
    was: lit update 3k6u.324 --lane store-bugs
    $ lit add /lanes/store-bugs 3k6u.324
    + /lanes/store-bugs/3k6u.324
    ⇒ 3k6u.324/lane = store-bugs
    ⇒ 3k6u.324/blocked_by = 3k6u.a53

### Put every child of an epic into one lane, in their current order
    was: for t in 5x1 cfm; do lit update e5r4.$t --lane policy; done
    $ lit add /lanes/policy '[e5r4.5x1, e5r4.cfm]'
    + /lanes/policy  [e5r4.5x1, e5r4.cfm]
    ⇒ e5r4.cfm/blocked_by = e5r4.5x1

### Which lane a ticket is in
    was: lit show 3k6u.9r8 --field lane
    $ lit get 3k6u.9r8/lane
    store-bugs

### A ticket in no lane is its own lane
It lives at `/lanes/<id>`, so it has a rank path like any other lane.

    $ lit get e9pl.7a2/lane
    7a2
    $ lit get /lanes/7a2
    /lanes/7a2  rank 6
      7a2   open

### Reorder within a lane
`b64` turned out to be the foundation `dee` builds on.

    $ lit set /lanes/datetime '[b64, dee, fw9]'
    /lanes/datetime   [dee, b64, fw9] → [b64, dee, fw9]
    ⇒ b64/blocked_by   dee → (none)
    ⇒ dee/blocked_by   (none) → b64
    ⇒ fw9/blocked_by   b64 → dee
    ⇒ /lanes/datetime  head is now b64

### Reorder with a typo, caught by the dry run
The list is replaced, not merged, so a dropped id is a removal, and the echo names it.

    $ lit set /lanes/datetime '[b64, fw9]' --dry-run
    /lanes/datetime   [dee, b64, fw9] → [b64, fw9]
    - /lanes/datetime/dee
    ⇒ dee/lane  datetime → dee
    ⇒ b64/blocked_by   dee → (none)
    (dry run: nothing written)
    $ lit set /lanes/datetime '[b64, dee, fw9]'
    /lanes/datetime   [dee, b64, fw9] → [b64, dee, fw9]
    …

### Take two tickets out of a lane so they run in parallel
The groomer found that `help.py`, `scene.py` and `code_policy.py` fixes share no files. The lane was serializing them falsely.

    was: lit update etg.qeo --lane help --reason "groom: … share no files; the unlaned default lane serialized them falsely"
    $ lit rm /lanes/etg-fixes/etg.qeo /lanes/etg-fixes/etg.j33
    - /lanes/etg-fixes/etg.qeo
    - /lanes/etg-fixes/etg.j33
    ⇒ etg.qeo/lane  etg-fixes → qeo
    ⇒ etg.j33/lane  etg-fixes → j33

### Move a ticket from one lane to another
A ticket is in at most one lane, so adding it to another moves it, and the echo shows the removal.

    $ lit add /lanes/store-bugs 9r8
    - /lanes/checkpoint/9r8
    + /lanes/store-bugs/9r8
    ⇒ 9r8/lane  checkpoint → store-bugs
    ⇒ 3sm/blocked_by  9r8 → l1x

### Dissolve the lane every Unfiled ticket was stuck in
"Every ticket here is its own lane" was written in the Unfiled epic's description. Eight `--lane` calls were the fix. Removing the lane makes each member solo.

    was: for t in 1kx aki …; do lit update ihvy.$t --lane ihvy.$t; done
    $ lit rm /lanes/unfiled --dry-run
    - /lanes/unfiled   [2ku, 207, oq5, dee, b64, fw9, 7nq0, hpxb]
    ⇒ 2ku/lane  unfiled → 2ku
    ⇒ 207/lane  unfiled → 207
    …
    (dry run: nothing written)

### Rerank a whole lane
Everything in the lane moves together.

    was: lit rank 3k6u.9r8 --top; lit rank 3k6u.hf4 --below 3k6u.9r8; lit rank 3k6u.a53 --below 3k6u.hf4
    $ lit set /lanes/store-bugs/rank 2
    /lanes/store-bugs/rank   5 → 2
    ⇒ /lanes/datetime/rank     2 → 3
    ⇒ /lanes/help/rank         3 → 4
    ⇒ /lanes/checkpoint/rank   4 → 5

### Rank one lane just below another
    was: lit rank links-scale-dy46.aof --below links-scale-dy46.48k
    $ lit get /lanes/scale-48k/rank
    3
    $ lit set /lanes/scale-aof/rank 4
    /lanes/scale-aof/rank   7 → 4
    ⇒ /lanes/help/rank      4 → 5
    …

### Thirteen tickets that are one sequence, as one lane
The session's 13-call `rank --top` / `--below` chain was an ordered list.

    was: lit rank kg2.c44 --top && lit rank xu4.1il --below kg2.c44 && lit rank xu4.53j --below xu4.1il && … (13 calls)
    $ lit add /lanes/release '[kg2.c44, xu4.1il, xu4.53j, bue.hqi, bue.1e6, lm3.7iy, lm3.1tt, kg2.1tz, 7du.5uw, 7du.usq, cjt.3ns, b2q.8ew, b2q.3cn]' --dry-run
    + /lanes/release  [kg2.c44, xu4.1il, …, b2q.3cn]
    ⇒ xu4.1il/blocked_by = kg2.c44
    ⇒ xu4.53j/blocked_by = xu4.1il
    …
    ⇒ b2q.3cn/blocked_by = b2q.8ew
    (dry run: nothing written)

### Ten independent epics, ordered by priority
The ten-call variant ranked epics, which are not a sequence. Each epic's lane gets its own rank.

    was: lit rank kg2 --top; lit rank xu4 --below kg2; lit rank bue --below xu4; …
    $ lit set /lanes/kg2/rank 1 /lanes/xu4/rank 2 /lanes/bue/rank 3 /lanes/lm3/rank 4
    /lanes/kg2/rank   6 → 1
    /lanes/xu4/rank   1 → 2
    /lanes/bue/rank   9 → 3
    /lanes/lm3/rank   2 → 4
    ⇒ /lanes/acr/rank   3 → 5
    …

### Read the whole backlog
    was: lit backlog | head -150
    $ lit get /lanes
    /lanes/release-pin    rank 1  pin        open                    1/1  urgent
    /lanes/store-bugs     rank 2  3k6u.9r8   open                    1/4
    /lanes/datetime       rank 3  b64        impl  this checkout     1/3
    /lanes/checkpoint     rank 4  3k6u.l1x   impl  stream ufijgjuy   1/3
    /lanes/7a2            rank 5  7a2        open                    1/1
    …

### Which top lanes are claimed
    was: lit backlog | grep -A2 -E '^\s*[12]\. ' | grep -E 'claimed'
    $ lit get '/lanes/*/claim'
    /lanes/datetime/claim     this checkout
    /lanes/checkpoint/claim   stream ufijgjuy  5 minutes ago

### An epic that holds two parallel lanes
`rin` has a parser stream and an evaluator stream. Neither waits on the other.

    $ lit add rin/lanes/parser '[rin.2ae, rin.k3x]'
    + rin/lanes/parser  [rin.2ae, rin.k3x]
    $ lit add rin/lanes/eval '[rin.9u9, rin.m0p, rin.t7c]'
    + rin/lanes/eval  [rin.9u9, rin.m0p, rin.t7c]
    $ lit get rin/lanes
    rin/lanes/parser  rank 6
      rin.2ae   review
      rin.k3x   open
    rin/lanes/eval  rank 7
      rin.9u9   open
      rin.m0p   open
      rin.t7c   open

### An urgent ticket as a one-ticket lane ranked top
Nothing had published since v1.156.4.

    was: lit new --type bug --topic release --top --title "Nothing has published since v1.156.4"
    $ lit add /lanes/release-pin '[{title:"Nothing has published since v1.156.4" type:bug parent:792a priority:urgent}]'
    + 792a.pin
    + /lanes/release-pin  [pin]
    $ lit set /lanes/release-pin/rank 1
    /lanes/release-pin/rank   12 → 1
    ⇒ /lanes/store-bugs/rank  1 → 2
    ⇒ /lanes/datetime/rank    2 → 3
    …

### What next serves when this checkout holds a lane
A higher lane whose head is not urgent does not pull it away.

    was: lit next
    $ lit get /next
    /next   b64   /lanes/datetime 1/3   held by this checkout

### What next serves when an urgent lane sits above the held one
    $ lit get /next
    /next   pin   /lanes/release-pin 1/1   urgent, ranked above /lanes/datetime (held by this checkout)

### What next serves when this checkout holds no lane
    $ lit get /next
    /next   3k6u.9r8   /lanes/store-bugs 1/4   top-ranked unclaimed lane

### Drop a lane claim without finishing
The checkout is leaving the lane mid-way. The only way to do that is a write that says so.

    $ lit rm b64/claim
    - b64/claim
    ⇒ /lanes/datetime   claim this checkout → (none)
    ⇒ /next   3k6u.9r8   /lanes/store-bugs

### Release a dead session's claim and put the ticket back
The plan phase had sat for ~12 hours with no commits or branch.

    was: lit open etg.bci --reason "Plan phase ~12h, no commits or branch; dead session's claim released"
    $ lit add etg.bci/comments "Plan phase ~12h, no commits or branch; dead session's claim released."
    + etg.bci/comments/r8v
    $ lit set etg.bci/status open
    etg.bci/status   plan → open
    ⇒ etg.bci/claim   claude_f335… → (none)

## Lanes at the edges

### Take one ticket out of a lane

    was: lit update tej.5 --lane ""
    $ lit rm /lanes/datetime/b64
    - /lanes/datetime/b64
    ⇒ b64/lane = b64
    ⇒ fw9/blocked_by = dee   (was b64)

### Dissolve a lane I hold

    $ lit rm /lanes/datetime
    - /lanes/datetime   [dee, b64, fw9]
    ⇒ dee/lane = dee
    ⇒ b64/lane = b64
    ⇒ fw9/lane = fw9
    ⇒ dee/claim = this checkout (its own lane now)

### Reorder a lane so the claimed ticket is no longer its head

    $ lit set /lanes/datetime '[b64, dee, fw9]'
    /lanes/datetime  [dee, b64, fw9] → [b64, dee, fw9]
    ⇒ b64/blocked_by = (none)
    ⇒ dee/blocked_by = b64

### The held lane's head waits on an edge from outside the lane

    $ lit get /lanes/datetime
    /lanes/datetime  rank 4
      dee   done
      b64   open   this checkout   blocked_by bryp (bryp/blocks)
      fw9   open

### An edge that closes a cycle through a lane

    $ lit add fw9/blocks dee
    error (reason=cycle): fw9/blocks dee closes a cycle: dee → b64 → fw9 (/lanes/datetime) → dee; nothing written

## A grooming pass, in order

### 1. Read every open ticket cheaply
Two lines per ticket, instead of the 270 KB the `show` loop printed.

    $ lit get '(status=open)/title' '(status=open)/lane'
    792a.b64/title   dateInZone accepts fixed offsets like +05:30
    792a.b64/lane    datetime
    rin.k3x/title    template with no pipeline passes nil dot
    rin.k3x/lane     parser
    …

### 2. Read the backlog in served order
    $ lit get /lanes
    /lanes/store-bugs   rank 2  3k6u.9r8   open                  1/4
    /lanes/datetime     rank 9  792a.dee   impl  this checkout   1/3
    …

### 3. Read the bodies of the tickets that look alike
    $ lit get 2d80.d66/body 2d80.c7e/body lq3.9od/body
    2d80.d66/body   The 'no tools' Unknown rule fires on …
    2d80.c7e/body   Unknown: no tools listed for …
    lq3.9od/body    The warning rules classify a turn with no tools …

### 4. Close a duplicate and move its acceptance criteria to the canonical ticket, in one call
The canonical body is the merged text on stdin. The duplicate closes against it.

    $ lit set lq3.9od/body - 2d80.d66/status dup 2d80.d66/of lq3.9od < /tmp/groom/9od-merged.md
    lq3.9od/body      v2 → v3  (+6 −0 lines)
    2d80.d66/status   open → dup
    2d80.d66/of       (none) → lq3.9od

### 5. Close a ticket whose work is already done
    was: lit close links-error-output-ckk7 --resolution obsolete --reason "groom: already satisfied - stderr now prints …"
    $ lit set ckk7/status obsolete ckk7/reason "groom: already satisfied; stderr prints the code and reason since #811"
    ckk7/status   open → obsolete
    ckk7/reason   (none) → groom: already satisfied; stderr prints the code and reason since #811

### 6. Rewrite a description the reader would misread
    $ lit set 9ym/body - <<'EOF'
    The statusline shows the lane's head and its position (2/3).
    Done when: a held lane renders as "datetime 2/3", a solo ticket as its id.
    EOF
    9ym/body   v4 → v5  (+2 −6 lines)

### 7. Put the trio that shares files into one lane
    $ lit add /lanes/datetime '[792a.dee, 792a.b64, 792a.fw9]'
    + /lanes/datetime  [792a.dee, 792a.b64, 792a.fw9]
    ⇒ 792a.b64/blocked_by = 792a.dee
    ⇒ 792a.fw9/blocked_by = 792a.b64

### 8. Rank the lanes
    $ lit set /lanes/datetime/rank 2 /lanes/store-bugs/rank 3
    /lanes/datetime/rank     9 → 2
    /lanes/store-bugs/rank   2 → 3

### 9. Check integrity
    was: lit doctor
    $ lit get /integrity
    /integrity   3 problems
      /lanes/help/rank        rank 4 shared with /lanes/eval
      3k6u.ckq/blocks/l1x     cycle: 3k6u.l1x/blocks/3k6u.ckq
      jcj.l1e/blocks/ob5      ob5 is deleted

### 10. Fix integrity, shown before it is done
    was: lit doctor --fix
    $ lit set /integrity ok --dry-run
    /integrity   3 problems → ok
    ⇒ /lanes/eval/rank   4 → 5
    ⇒ /lanes/scale-aof/rank   5 → 6
    ⇒ - 3k6u.ckq/blocks/3k6u.l1x
    ⇒ - jcj.l1e/blocks/ob5
    (dry run: nothing written)
    $ lit set /integrity ok
    /integrity   3 problems → ok
    ⇒ /lanes/eval/rank   4 → 5
    ⇒ /lanes/scale-aof/rank   5 → 6
    ⇒ - 3k6u.ckq/blocks/3k6u.l1x
    ⇒ - jcj.l1e/blocks/ob5

### 11. Report what changed
The echoes above are the grooming report. A session that saved them has the whole diff.

    $ lit get /integrity
    /integrity   ok

## Undo

### Undo a title edit from its own echo

    $ lit set 9ym/title "Status line hides the ceiling when idle"
    9ym/title  "Status line shows the ceiling only past 80%" → "Status line hides the ceiling when idle"
    $ lit set 9ym/title "Status line shows the ceiling only past 80%"
    9ym/title  "Status line hides the ceiling when idle" → "Status line shows the ceiling only past 80%"

### Undo a description rewrite

    was: lit history yjx --version description@1 > /tmp/yjx-v1.txt; lit update yjx --description "$(cat /tmp/yjx-v1.txt)"
    $ lit get yjx/body/versions
    yjx/body/versions
      1  Brandon Fryslie  2026-10-19T16:02Z
      2  claude_5c44…     2026-11-02T21:10Z
      3  claude_5c44…     2026-11-02T21:31Z
    $ lit get yjx/body/versions/1 | lit set yjx/body -
    yjx/body  v3 → v4 (same text as v1)

### I meant to add a label and replaced the list

    $ lit set 9to/labels '[decompose]'
    9to/labels  [external, needs-design] → [decompose]
    $ lit set 9to/labels '[external, needs-design, decompose]'
    9to/labels  [decompose] → [external, needs-design, decompose]

### Find everything this session changed, to undo it

    $ lit get '(updated>-2h)/history/(by=claude_5c44)'
    yjx/history/e91   body v1 → v2
    yjx/history/e94   body v2 → v3
    9ym/history/e12   title "Status line shows…" → "Status line hides…"

## Workflow as data

### Which workflow governs this ticket, and where each state comes from

    was: lit workflows show rin.4ou   →   no workflow definition with id "rin.4ou"
    $ lit get rin.4ou/workflow
    rin.4ou/workflow  (resolved)
      open      initial next:[plan]      /workflow
      plan      next:[audit]  gate:…     rin/workflow (next), /workflow (gate)
      audit     next:[impl]              rin/workflow
      impl      next:[review]            /workflow
      review    next:[merge, impl]       /workflow
      merge     next:[done]              /workflow
      done      terminal                 /workflow
      dup       terminal requires:of     /workflow
      superseded terminal requires:of    /workflow
      obsolete  terminal requires:reason /workflow
      wontfix   terminal requires:reason /workflow

### Add an audit state to one epic

    $ lit set rin/workflow/states/audit '{next:[impl] guidance:"Run the law audit on the diff before code review."}' rin/workflow/states/plan/next '[audit]'
    rin/workflow/states/audit      (none) → {next:[impl] guidance:"Run the law audit on the diff before code review."}
    rin/workflow/states/plan/next  [impl] (from /workflow) → [audit]
    ⇒ rin/workflow/states/plan/gate = plan/verdict=pass … (from /workflow)
    ⇒ 6 tickets under rin now resolve plan → audit → impl

### Add a gate to a state

Nothing in the live-verification epic merges until it carries the `live-verified` label. A gate on a state guards the transitions out of it.

    $ lit set mel/workflow/states/review/gate 'labels~live-verified'
    mel/workflow/states/review/gate  (none) → labels~live-verified

### Refused by that gate

    $ lit set mel.l1e/status merge
    error (reason=gate): mel.l1e/status review → merge is gated by mel/workflow/states/review/gate
      labels~live-verified   mel.l1e/labels = [external]
    nothing written

### Change the guidance on a state

    $ lit set /workflow/states/merge/guidance - <<'EOF'
    Once it has merged, close it: lit set $/status done. The echo names the next ticket in your lane.
    EOF
    /workflow/states/merge/guidance  v2 → v3

### Read the guidance a ticket in a custom state will see

    was: lit quickstart review
    $ lit get rin.4ou/workflow/states/audit/guidance
    Run the law audit on the diff before code review.

### Remove a state that tickets are in

    $ lit rm /workflow/states/merge
    error (reason=in_use): /workflow/states/merge is occupied and referenced; nothing written
      occupied by   0814 s33 tbk               (status=merge)
      named in      /workflow/states/review/next = [merge, impl]

### Remove an override nobody occupies

    $ lit rm rin/workflow/states/audit rin/workflow/states/plan/next --dry-run
    - rin/workflow/states/audit
    - rin/workflow/states/plan/next
    ⇒ rin/workflow/states/plan/next = [impl] (from /workflow)
    (dry run: nothing written)

### Add a terminal state that requires a field

    $ lit set /workflow/states/superseded '{terminal:true requires:of}'
    /workflow/states/superseded  (none) → {terminal:true requires:of}

### Close as superseded without naming what supersedes it

    was: lit close 2ka --resolution superseded --of 6op --reason "…"
    $ lit set 2ka/status superseded
    error (reason=requires): /workflow/states/superseded requires 2ka/of, which is empty; nothing written
    $ lit set 2ka/status superseded 2ka/of 6op 2ka/reason "user decided: too-large results go whole to a file"
    2ka/status  open → superseded
    2ka/of      (none) → 6op
    2ka/reason  (none) → "user decided: too-large results go whole to a file"

### A transition that is not in next

    was: lit phase tbk --to merge   (from impl)
    $ lit set tbk/status merge
    error (reason=not_in_next): tbk/status impl → merge; /workflow/states/impl/next = [review]; nothing written

### Park an in-progress ticket, or drop it as obsolete
The initial state is reachable from every state, and every terminal from every non-terminal; `next` lists only the forward edges.

    was: lit open 37o --reason "parked behind bryp"; lit close 2m0 --resolution obsolete --reason "…"
    $ lit set 37o/status open 37o/reason "parked behind bryp"
    37o/status  impl → open
    37o/reason  (none) → "parked behind bryp"
    ⇒ 37o/claim  this checkout → (none)
    $ lit set 2m0/status obsolete 2m0/reason "ws_client deleted in PR #55"
    2m0/status  review → obsolete
    2m0/reason  (none) → "ws_client deleted in PR #55"
    ⇒ 2m0/claim  stream ufijgjuy (elsewhere) → (none)

### One selector across two dialects of review

The live-verification epic defines its own `review`; everything else uses the root's.

    $ lit get '(status=review)/title'
    mel.l1e/title    Scene load reports a missing asset with its warning
    sjhy.ju8/title   Menus open on the focused display
    $ lit get mel/workflow/states/review/next /workflow/states/review/next
    mel/workflow/states/review/next   [live-verify, impl]
    /workflow/states/review/next      [merge, impl]

### Give one epic's claims a longer life

    $ lit set px1f/claim_ttl 12h
    px1f/claim_ttl  4h (from /) → 12h
    ⇒ px1f.9u9/claim expires 2026-11-03T22:04Z (was 14:04Z)

### Where does this claim's expiry come from?

    $ lit get px1f.9u9/claim px1f.9u9/claimed_at px1f.9u9/claim_ttl
    $ = promptctl-testing-px1f.9u9
    $/claim        stream voice-di1 (elsewhere)
    $/claimed_at   2026-11-03T10:04Z
    $/claim_ttl    12h (from px1f)

## Derived fields

### Set a derived field

    $ lit set dee/claim claude_64e3
    error (reason=derived): dee/claim is derived, not settable (field table: claim  scalar  derived  stamped by a transition when none is held); nothing written

### Type the doc's own lane example

    $ lit set 'unfiled.*/lane' @id
    error (reason=derived): unfiled.*/lane is derived from lane membership, not settable; nothing written
    $ lit rm '/lanes/unfiled/*'
    - /lanes/unfiled/2ku
    - /lanes/unfiled/207
    - /lanes/unfiled/oq5
    ⇒ 2ku/lane = 2ku
    ⇒ 207/lane = 207
    ⇒ oq5/lane = oq5

### Clear a ticket's blocker by hand

    $ lit set b64/blocked_by ""
    error (reason=derived): b64/blocked_by is derived: dee precedes b64 in /lanes/datetime; reorder the lane or remove b64 from it; nothing written

### Blocked by lane position versus by an edge

    was: lit show 37o | grep -A3 '^blocked_by:'
    $ lit get b64/blocked_by 37o/blocked_by
    b64/blocked_by   dee    (position 2 in /lanes/datetime)
    37o/blocked_by   bryp   (bryp/blocks)

### Reopen a closed epic by reopening a child

    was: lit open o3mh   →   cannot open epic: it is closed, and an epic's state derives from its children
    $ lit set o3mh.avl/status open o3mh.avl/reason "Done-when fails on the second checkout"
    o3mh.avl/status  done → open
    o3mh.avl/reason  (none) → "Done-when fails on the second checkout"
    ⇒ o3mh/status = open (was done)

### Rank a ticket

    was: lit rank aof --below 48k
    $ lit set aof/rank 3
    error (reason=unknown_field): a ticket has no field rank; rank is on lanes. aof is in /lanes/scale (rank 6): lit set /lanes/scale/rank <n>

### Change a ticket's topic

    was: lit update bwy.16 --topic context   →   unknown flag: --topic
    $ lit set bwy.16/topic context
    error (reason=immutable): bwy.16/topic is set at creation only (field table: topic  scalar  immutable); nothing written

### Turn a leaf into an epic

    was: lit update hje --type epic   →   cannot change issue_type between container ([epic]) and leaf types
    $ lit set hje/type epic
    error (reason=invalid_value): hje/type task → epic crosses leaf and container; nothing written

## Integrity

### Is the store healthy?

    was: lit doctor
    $ lit get /integrity
    /integrity
      integrity_check      ok
      foreign_key_issues   0
      rank_inversions      2
      dependency_cycle     none
      id_collisions        0

### Read one check

    $ lit get /integrity/rank_inversions
    /integrity/rank_inversions
      /lanes/lm3 (rank 3) waits on /lanes/device-sources (rank 7)
      /lanes/scale (rank 5) waits on /lanes/sync (rank 9)

### See what the repair would do

    was: lit doctor --fix
    $ lit set /integrity ok --dry-run
    /lanes/device-sources/rank   7 → 3
    /lanes/lm3/rank              3 → 4
    /lanes/sync/rank             9 → 5
    /lanes/scale/rank            5 → 6
    ⇒ /integrity/rank_inversions = 0
    (dry run: nothing written)

### Do the repair

    $ lit set /integrity ok
    /lanes/device-sources/rank   7 → 3
    /lanes/lm3/rank              3 → 4
    /lanes/sync/rank             9 → 5
    /lanes/scale/rank            5 → 6
    ⇒ /integrity/rank_inversions = 0
    ⇒ /integrity = ok

### A cycle the repair will not choose how to break

A sync pull merged two edges written in different clones.

    $ lit set /integrity ok
    error (reason=needs_decision): /integrity/dependency_cycle: 37o → bryp → 37o; the repair cannot choose which edge to drop; nothing written
      lit rm 37o/blocks/bryp
      lit rm bryp/blocks/37o

### Two clones minted the same id

    $ lit set /integrity ok
    error (reason=needs_decision): /integrity/id_collisions: bwy.16 names two tickets; the repair cannot choose which keeps the id; nothing written
      bwy.16   "Context budget reads the model's window"   claude_a1c0…   clone A
      bwy.16   "Speech pipeline drops the first word"      claude_c93e…   clone B

## Archive, delete, restore, purge

### Archive an epic's children

    was: for id in …; do lit archive $id --reason "…"; done
    $ lit set 'r7y.*/archived' true --dry-run
    r7y.ux7/archived  false → true
    r7y.8rr/archived  false → true
    r7y.qg8/archived  false → true
    (dry run: nothing written)

### Read archived tickets

Globs and selectors pass over archived and deleted tickets unless the selector names them.

    was: lit ls --ids r7y.ux7,r7y.8rr --include-archived
    $ lit get '(archived=true parent=r7y)/title'
    r7y.ux7/title   Wake word survives a sleep
    r7y.8rr/title   Barge-in stops playback within 100ms

### Delete a ticket filed in error

    was: lit delete fko --reason "made in error: a doc edit, committed to master"
    $ lit add fko/comments "Deleted: made in error, a doc edit committed to master."
    + fko/comments/a210
    $ lit rm fko
    fko/deleted  false → true

### Restore a deleted ticket

    was: lit restore 9ym --reason "deleted in error; not phase-related"
    $ lit set 9ym/deleted false
    9ym/deleted  true → false

### Read a deleted ticket

    $ lit get 9yn/status 9yn/deleted
    $ = houdini-tuneup-ef3.9yn
    $/status    done
    $/deleted   true
    $ lit get '(deleted=true text~"probe")/title'
    9yn/title   PROBE ticket (delete me)
    9ci/title   probe-delete-me

### Purge a probe ticket for good

    was: lit purge 8k1 --yes
    $ lit rm 3aj.8k1 --purge
    - 3aj.8k1   (comments=3 labels=0 edges=2 events=3)

### Purge an abandoned epic, shown first

    $ lit rm 3aj --purge --dry-run
    - 3aj
    - 3aj.8k1   (comments=3 edges=2)
    - 3aj.evr   (comments=1 edges=0)
    - 3aj.yjb   (comments=0 edges=1)
    ⇒ /lanes/monetization removed (no members left)
    (dry run: nothing written)

## Root paths

### What is at the root?

    $ lit get /
    /
      children    14 epics
      lanes       23
      workflow    9 states
      claim_ttl   4h
      integrity   ok
      next        b64

### The root workflow

    was: lit workflows
    $ lit get /workflow
    /workflow/states
      open        initial  next:[plan]
      plan        next:[impl]          gate:plan/verdict=pass   guidance:v4
      impl        next:[review]                                 guidance:v2
      review      next:[merge, impl]                            guidance:v3
      merge       next:[done]                                   guidance:v3
      done        terminal
      dup         terminal  requires:of
      superseded  terminal  requires:of
      obsolete    terminal  requires:reason
      wontfix     terminal  requires:reason

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
    $ = links-hierarchy-rin
    $.4ou/title       Ancestors resolve workflow by walking up
    $.4ou.1ab/title   A child's state reads its own subtree's dialect
    $.9m5/title       Lanes are cross-cutting

## Output shape corners

### `--json` on a glob

    $ lit get '792a.*/status' --json
    {"792a.dee/status":"done","792a.b64/status":"impl","792a.fw9/status":"open"}

### Two paths under one ancestor

    $ lit get 792a.dee/title 792a.dee/status 792a.b64/status
    $ = template-unfiled-792a
    $.dee/title    dateInZone accepts zone names in any letter case
    $.dee/status   done
    $.b64/status   impl

### Guidance printed after the data

    $ lit get dee
    $ = template-unfiled-792a.dee
    $/title    dateInZone accepts zone names in any letter case
    $/status   merge
    $/lane     datetime
    $/claim    this checkout

    /workflow/states/merge/guidance
    Once it has merged, close it: lit set $/status done. The echo names the next ticket in your lane.

### A ticket in a subtree with its own state

    $ lit get rin.4ou
    $ = links-hierarchy-rin.4ou
    $/title    Ancestors resolve workflow by walking up
    $/status   audit
    $/claim    this checkout

    rin/workflow/states/audit/guidance
    Run the law audit on the diff before code review.

### A multi-line value in path-tab-value output

    $ lit get '792a.*/body'
    792a.dee/body   "Go's LoadLocation is case-sensitive; dateInZone is not.\n\nDone when: …"
    792a.b64/body   "Builds on dee.\n\nDone when: …"

### A lane with a finished member

    $ lit get /lanes/datetime
    /lanes/datetime  rank 4
      dee   done
      b64   impl   this checkout
      fw9   open

### Read, edit, write without JSON

    $ lit get '(parent=z6sv priority=urgent)/priority' | awk '{print $1, "normal"}' | xargs lit set
    z6sv.dbc/priority  urgent → normal
    z6sv.463/priority  urgent → normal
    z6sv.bd3/priority  urgent → normal

### Commits and PRs for a ticket

    was: lit log 9uht   →   a2292fc4 2026-10-21 feat(daemon): a fed bar shows the pace…
    $ lit get 9uht/history
    9uht/history
      e01  2026-10-15T05:11Z  claude_214f…  status open → plan   ⇒ claim = claude_214f…
      e02  2026-10-15T06:02Z  claude_214f…  plan (none) → v1
      e07  2026-10-21T19:40Z  claude_214f…  status merge → done

## Odd nodes and refusals

### Remove a label that is not there

    was: lit label rm 9to external
    $ lit rm 9to/labels/external
    error (reason=not_found): 9to/labels has no external; 9to/labels = [needs-design, decompose]; nothing written

### Remove a comment twice

    $ lit rm 03v/comments/2f27
    - 03v/comments/2f27
    $ lit rm 03v/comments/2f27
    error (reason=not_found): 03v/comments has no 2f27; nothing written

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
    error (reason=unknown_field): a ticket has no field phase; phases are states: lit get dee/status
      ticket fields: archived blocked_by blocks body children claim claimed_at claim_ttl comments
                     created_at deleted history labels lane of parent plan priority prompt reason
                     status title topic type updated_at workflow

### A glob that matches nothing

    $ lit set 'ogn.*/priority' urgent
    error (reason=no_match): ogn.* matched no tickets (ogn has no children); nothing written

### A selector with an unknown key

    was: lit ls --ids … --columns id,status   →   unknown --columns name "status"; valid columns: … state …
    $ lit get '(state=open)/title'
    error (reason=unknown_field): selector key state is not a field; did you mean status; nothing printed

### A selector value no workflow defines

    was: lit ls --status closed,open,in_progress,blocked --search b2q.   (four times, unchanged)
    $ lit get '(status=blocked)/title'
    error (reason=invalid_value): no resolved workflow has a state blocked
      states: open plan impl review merge done dup obsolete superseded; audit under rin; live-verify under mel
      blocked is the derived field blocked_by
    nothing printed

### Stdin twice in one command

    $ lit set 8ro6/body - 8ro6/plan - < /tmp/8ro6/both.md
    error (reason=usage): - reads stdin once per command; it appears twice (8ro6/body, 8ro6/plan); nothing written

### A `.` where a `/` belongs

    $ lit get 792a.dee.title
    error (reason=not_found): no ticket 792a.dee.title: 792a.dee has no child title; for the field: lit get 792a.dee/title

### A short id that names two tickets

    was: lit show lm3.1tt   →   issue "lm3.1tt" not found
    $ lit get 1tt
    error (reason=ambiguous): 1tt names 2 tickets: orange-iphone-watch-lm3.1tt, promptctl-voice-i15t.1tt; give more of the id

### Selector, glob, or literal left unquoted in zsh

    $ lit get (status=review)/title
    zsh: no matches found: (status=review)/title
    $ lit get unfiled.*/title
    zsh: no matches found: unfiled.*/title
    $ lit add a1h/plan/verdicts {pass laws:[one-source-of-truth, single-enforcer]}
    zsh: parse error near `}'

### A title with a colon and quotes in it

    was: lit import --path /tmp/cb-breakdown/vocab.yaml   →   yaml: line 2: mapping values are not allowed in this context
    $ lit add unfiled/children '{title:daemon.log is mostly unknown-field lines type:bug}'
    error (reason=parse): value {…}: "is" is not k:v; quote a value that holds spaces; nothing written
    $ lit add unfiled/children - <<'EOF'
    {title:"daemon.log is ~90% 'schema: unknown field' lines" type:bug topic:daemon}
    EOF
    + unfiled.k4e
    ⇒ /lanes/k4e = [k4e]   rank 24

### A write where one path of three is refused

    $ lit set 2ku/status done 207/status done oq5/status done
    error (reason=not_in_next): oq5/status impl → done; /workflow/states/impl/next = [review]
      2ku/status  merge → done   not written
      207/status  merge → done   not written
    nothing written

### Set a field to the value it already has

    was: lit done cjt   →   state_already_holds
    $ lit set dee/status impl
    dee/status  impl (unchanged)

## The old fumbles, in the new grammar

### Comment text given positionally

The value is positional, so the old failing form is now the working one.

    was: lit comment add links-supply-chain-w6m9.2 "PR #333 opened: …"
    $ lit add w6m9.2/comments "PR #333 opened: https://github.com/promptctl/links-issue-tracker/pull/333 — CycloneDX 1.6 SBOM …"
    + w6m9.2/comments/x4c

### Comment with no subcommand, or with -m or --message

    was: lit comment orange-widgets-5jo.nlf "From PR #49 review: …"
    was: lit comment add brandon-daemon-lifecycle-gad.3 -m "Completed by PR #163 …"
    was: lit comment add room-live-verify-56w --message "LIVE-VERIFIED …"
    $ lit add gad.3/comments "Completed by PR #163 …"
    + gad.3/comments/e9k

### Comment removal given the ticket id and the comment id

The old `comment rm` took only the comment id; the new path needs both.

    was: lit comment rm houdini-laws-etg.4id cmt-5ae7b8da-29f3-4328-bebf-6bb988c5e60b
    $ lit rm 4id/comments/5ae
    - 4id/comments/5ae

### Editing a comment

    was: lit comment edit --help
    $ lit set 3h8/comments/03d "The credential file is src/oauth.ts."
    3h8/comments/03d  "The credential file is src/login.ts." → "The credential file is src/oauth.ts."

### Several ids handed to show

    was: lit show orange-devices-bue.1e6 orange-devices-bue.chz orange-devices-bue.hqi
    $ lit get 1e6/status chz/status hqi/status
    1e6/status   open
    chz/status   impl
    hqi/status   done

### Comments asked for as a field

    was: lit show orange-iphone-watch-lm3.7iy --field status,labels,description,comments
    $ lit get 7iy/status 7iy/labels 7iy/body 7iy/comments
    $ = orange-iphone-watch-lm3.7iy
    $/status         plan
    $/labels         [watch]
    $/body           The watch app reads heart rate through …
    $/comments/2fa   [claude_96d7… Oct 24] Plan v5 drops the polling loop …

### JSON wanted for one field

    was: lit show promptctl-brain-vu2t.abp --json 2>/dev/null | jq -r '.plan // .issue.plan // empty' > /tmp/abp_break/plan_v4.md
    $ lit get abp/plan > /tmp/abp_break/plan_v4.md

### JSON wanted for a few fields

    was: lit show links-scale-dy46.19m --json
    $ lit get 19m/status 19m/plan/digest --json
    {"19m/status":"impl","19m/plan/digest":"45d6d009cb32…"}

### Wrong id prefix

    was: lit show orange-drink-lm3.8pk
    $ lit get lm3.8pk/title
    Pairing survives the watch rebooting

### Filter by a field there was no flag for

    was: lit ls --topic devices
    $ lit get '(topic=devices)/title'
    orange-devices-bue.1e6/title   The device list shows a sweep in progress
    orange-devices-bue.k7r/title   A connected device sweeps with an empty name

### A selector key that is not a field

    was: lit ls --epic orange-alerts-cjt
    $ lit get '(epic=cjt)/title'
    error (reason=unknown_field): selector key epic is not a field; did you mean parent; nothing printed

### An epic's tickets, asked for with --epic

    was: lit ls --epic orange-alerts-cjt
    $ lit get '(parent=cjt)/status'
    orange-alerts-cjt.3ns/status   done
    orange-alerts-cjt.7pq/status   impl
    orange-alerts-cjt.v2a/status   open

### Next, scoped to one epic

    was: lit next --epic orange-admin-helper-b2q
    $ lit get b2q/lanes
    b2q/lanes
      /lanes/admin-helper   3cn   open

### A reason given to a dependency

    was: lit dep add candybar-testing-ihvy.cbz --blocked-by candybar-testing-0x6u.snf --reason "the gauge test needs the fake clock"
    $ lit add snf/blocks cbz
    + snf/blocks/cbz
    ⇒ cbz/blocked_by  [] → [candybar-testing-0x6u.snf]
    $ lit add cbz/comments "Waits on 0x6u.snf: the gauge test needs the fake clock."
    + cbz/comments/u7y

### Done on an epic whose children are all closed

    was: lit done orange-alerts-cjt
    $ lit set cjt/status done
    error (reason=derived): cjt is an epic; cjt/status is derived from cjt.*/status; nothing written

### Phase given positionally

    was: lit phase brandon-unfiled-3248.8ro6 impl
    $ lit set 8ro6/status impl
    8ro6/status  plan → impl

### Status changed through update

    was: lit update brandon-menus-sjhy.ju8 --status open
    $ lit set ju8/status open
    ju8/status  plan → open
    ⇒ ju8/claim  this checkout → (none)

### A resolution that is not a state

    was: lit close houdini-install-aby.80r --resolution fixed
    $ lit set 80r/status fixed
    error (reason=invalid_value): no resolved workflow has a state fixed; terminal states: done dup superseded obsolete wontfix; nothing written

### A priority level that does not exist

    was: lit update houdini-connections-ob5 --priority high
    $ lit set ob5/priority high
    error (reason=invalid_value): ob5/priority takes normal or urgent; got high; nothing written

### Reasons passed to the verdict

    was: lit verdict mock-mock-flu.17w fail --plan 0b7ecac89fe6… --law single-enforcer --body "…"
    $ lit add 17w/comments - <<'REASONS'
    Plan review (digest 0b7ecac89fe6): fail. …
    REASONS
    + 17w/comments/n5g
    $ lit add 17w/plan/verdicts '{fail laws:[single-enforcer]}'
    + 17w/plan/verdicts/t8b
    ⇒ 17w/plan/verdict = fail

### Guessed verbs: search, list, create, comments, epics

    was: lit search RacingFirstOpens
    $ lit get '(text~"RacingFirstOpens")/title'
    links-perf-kw6z.5/title   RacingFirstOpens opens the store once per race
    was: lit list
    $ lit get /lanes
    …
    was: lit create --type epic --title E1 --topic t1
    $ lit add /children '{title:E1 type:epic}'
    + links-v3p
    was: lit comments candybar-isolated-daemon-bfab.agt
    $ lit get agt/comments
    …
    was: lit epics
    $ lit get '(type=epic)/title'
    …

## The admin boundary and the store lock

### Push the backlog

`stores`, `snapshots`, `lifeboat`, `upgrade`, `downgrade`, `prefix` and `backup` answer the same way.

    was: lit sync push
    $ lit sync push
    error (reason=unknown_verb): lit has four verbs: get set add rm; sync is lit admin sync
    $ lit admin sync push
    pushed

### Write while an admin sync holds the store

    was: lit verdict rm9w … --law single-enforcer   →   workspace_busy: held by pid 95859 (lit sync push --remote origin)
    $ lit add rm9w/plan/verdicts '{pass laws:[single-enforcer]}'
    lit: waiting for the store; held by pid 95859 (lit admin sync push --remote origin) since 2s
    error (reason=workspace_busy): store held by pid 95859 (lit admin sync push --remote origin) for 30s; nothing written; rerun unchanged

### Start a workspace in a repo whose name yields no prefix

    was: lit init   →   issue prefix refused: repository name "ws" yields none … run lit init --prefix <prefix>
    $ lit get /
    error (reason=not_initialized): no lit workspace in this repository; lit admin init
    $ lit admin init
    error (reason=invalid_value): repository name "ws" yields no prefix; lit admin init --prefix <3+ chars>
    $ lit admin init --prefix links
    initialized  prefix links

## What the examples could not say

Writing the examples above against the design found places the grammar has no
spelling. Each was tried, by a session in the transcripts or by a writer here, and
written as far as the grammar goes. The design doc already absorbed the ones that
were contradictions inside it or conventions every writer reached independently
(quoting, `stage`, the edges `next` never lists, `/lanes/<id>`, all-or-nothing
writes, the refusal and dry-run shapes, `--json` keyed by path, short ids, `rm x` as
`set x/deleted true`, `lit admin init`). What remains is open.

**Selectors and globs**

- No negation (`type!=epic`), no OR on one key (`status=open,impl`), no `>` or
  absolute dates (`updated>2026-11-02`), no existence or count predicate (`has
  comments`), no sort, no limit. Sessions used every one of these through `ls --query`
  and `grep -v`.
- Which fields `text~` searches; whether `labels=focus` means the list contains it;
  whether a selector matches closed, archived, and deleted tickets by default. The
  examples assume every status, and archived or deleted only when named.
- Nothing selects below the ticket: comments by author, history events by writer.
  `rm '*/comments/*'` "filtered by author" in the design has no spelling.
- No recursive glob, no alternation, no multi-field path, no way to name an arbitrary
  set of ids. N ids × M fields is N×M paths, and one value on three unrelated tickets
  is three calls.
- A glob or selector cannot be a value: `add /lanes/policy 'e5r4.*'`.

**Values**

- No file form, and `-` reads stdin once per command, so N long bodies in one call
  need shell substitution, which the worktree guard refuses.
- Quoted strings define no escapes, and `path<TAB>value` output defines no encoding
  for a multi-line value, so the round trip the Output section promises is undefined
  for bodies.
- `add` has no many-pairs form. Eleven different grooming notes on eleven tickets is
  eleven calls, where `set` would take them as pairs.
- Whether an object may nest `children`, and whether items in one `add` can refer to
  each other. A `blocks` edge between two tickets minted by the same call needs a
  second call.

**Reasons**

- No reason on a write, an edge, or an `rm`. The old `--reason` on `update`, `dep
  add`, `delete`, and `archive` has nowhere to go but a comment written first.
  Sessions tried `dep add --reason` seven times in a row and dropped the reason.

**Plans and gates**

- A verdict binds to the digest current when it is written, not the one the reviewer
  read. A reviewer who read v21 while the author saved v22 passes v22 unread, and the
  old stale-digest refusal has no equivalent.
- "The reviewer is not the plan's author" compares two paths; the gate form shows
  only `path=literal`. Where "a fail cites at least one law" lives: field table or
  workflow.
- Editing the plan of a ticket already in `impl` empties the verdict `impl` was
  entered on. What happens to the ticket, and what the echo says.
- The echo on a verdict cannot say whether the gate now passes.
- Guidance is one field per state, but `plan` has two readers, the author and the
  reviewer, and `quickstart review` was the reviewer's own text. Whether a transition
  prints the guidance of the state it enters: the old `start` printed "write its plan"
  and that line is what sent sessions to write one.
- `/next` exists only at the root. `b2q/next`, by nearest-ancestor, is undefined.

**Claims and lanes**

- A claim is one scalar, so a contested lane (two checkouts started tickets in it
  before either saw the other, today's `contested by stream …`) has no representation.
- No node holds the computed expiry, so "claim expired" has no selector key.
- A reorder that puts a held, in-flight ticket behind an open one.
- `next` when the held lane's head is blocked by an edge from outside the lane:
  serve it, print nothing, or fall through.
- Rank is absolute. "Below that lane" is a read, then a write, with a race between.
- Where a ticket created through a lane `add` lives when its object names no parent.
- Nearest-ancestor wins per state or per field: overriding `plan/next` in an epic
  either keeps the root's `plan/gate` or replaces the whole state.
- The writer has no node. `--by` and the session id decide it, and "this checkout"
  in an echo is the only place it shows.

**Fields**

- `topic`: immutable today, absent from the path list. `follows_up`: the `followup`
  mapping drops the link. A comment's body: settable or append-only; sessions
  appended corrections on purpose. Where "only an epic lives at the root" is declared.
- Commits and PRs that name a ticket are not nodes; `lit log` has no path.
- Two clones that minted the same id: every path names one node, so neither ticket
  can be read, renamed, or removed on its own.
