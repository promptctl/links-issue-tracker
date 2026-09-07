# Dolt Remote Sync

`links` sync is Dolt-native and uses Dolt git-remote support directly.
Git remotes are the canonical remote configuration.

## Version requirement

- Required Dolt version: `>= 1.81.10`
- Enforced at app startup through `internal/doltcli.RequireMinimumVersion`.

## Local data location

The Links Dolt database is shared across all worktrees in the same clone:

```txt
$(git rev-parse --git-common-dir)/links/dolt
```

`lit sync` commands run in the current repo/worktree root and operate on that database.

## First clone (bootstrap)

On a fresh clone of a repo that already uses `lit`, run `lit init`: it detects that
the remote carries existing ticket data and adopts that history wholesale, so the
clone starts with the real backlog. This is the first-receive path — not `lit sync
pull`. A fresh store has its own unrelated root commit, so a pull against the remote
fails with `no common ancestor`; adoption resets the local branch to the remote head
instead. Once the clone has adopted, its history is shared with the remote and the
ordinary `lit sync pull` / `lit sync push` flow applies.

## Typical setup

```sh
lit hooks install
git remote add origin https://github.com/<org>/<repo>.git
lit sync remote ls
lit sync fetch
lit sync pull
```

## Daily workflow

```sh
lit sync status
lit sync pull
# ...work with lit commands...
lit sync push
```

## Commands

- `lit sync status`
- `lit sync remote ls`
- `lit sync fetch [--remote <name>] [--prune] [--verbose]`
- `lit sync pull [--remote <name>] [--verbose]`
- `lit sync push [--remote <name>] [--set-upstream] [--force] [--verbose]`
- `lit sync reconcile` — merge a diverged clone into linear history; surfaces a concurrent free-text rewrite for the calling agent to merge
- `lit sync reconcile resolve --resolve ID:FIELD:FINGERPRINT=TEXT …` — finalize the reconcile with the agent's merged text (one `--resolve` per pending field; the fingerprint, copied from the guidance, pins the merge to the exact conflict)
- `lit sync reconcile abort` — leave the clone diverged for now

Sync branch selection:

- default: repository default branch from the configured remote
- debug override: set `LINKS_DEBUG_DOLT_SYNC_BRANCH=<branch>`

Sync remote selection for pull/push when `--remote` is omitted:

- branch upstream remote (when configured)
- otherwise, the single configured Git remote
- if no eligible remote exists, sync pull/push return `status=skipped` and do not run Dolt sync side effects

Text output behavior:

- default output is terse and hides remote-specific details
- use `--verbose` to include remote/branch details in text output

Before each `lit sync` command, `lit` reconciles Dolt remotes to exactly match `git remote -v` fetch URLs:

- add missing Dolt remotes
- update changed remote URLs
- remove Dolt remotes that no longer exist in Git

## Push automation

`lit hooks install` writes `$(git rev-parse --git-common-dir)/hooks/pre-push` and chains any existing user hook.
The hook auto-runs one canonical `lit sync push` per git push, never blocks the git push, and emits a warning that includes the trigger, remote, retry command, and trace path if DB sync fails.
Successful and failed automatic runs both write trace files under the workspace `traces_dir` returned by `lit workspace`.

## Durable sync/init decision trace

Every decision `lit init`'s remote-adopt step or a `lit sync fetch/pull/push/reconcile`
reaches — including the on-change mirror and the inline receive/reconcile below —
is recorded unconditionally as a JSON record under a `sync/` subdirectory
alongside the workspace's `automation/` trace directory (the one `lit workspace`
reports as `traces_dir`) — both live under one shared `traces/` parent, e.g.
`traces/automation/` and `traces/sync/` as siblings. It is recorded whether the
command ran directly or under automation (a git hook, the on-change mirror, the
inline receive). This is separate from, and in addition to, the automation
trace `lit workspace`'s `traces_dir` names above: that one is written only when
`LNKS_AUTOMATION_TRIGGER` is set, so a directly-run interactive command left no
record there at all. A sync-trace record's `trigger` field is empty for an
interactive occasion and names the trigger for an automated one, so the two trace
kinds can never disagree about what fired a given occasion. It gives every
sync/init decision a durable history to inspect after the fact, whether or not
it happened under automation — before this, an interactive session's decisions
(including its inline auto-receive/reconcile, which usually runs with no
trigger set) left no record anywhere once the process exited.

## Push cadence

The cadence — how often lit mirrors the store to the remote — is a single
config policy you own, not a per-command behavior. Set it under `[sync]` in
`config.toml` (global at `~/.config/links-issue-tracker/config.toml`, or
per-project at `.lit/config.toml`):

```toml
[sync]
cadence = "on-change"   # default
```

| value       | meaning                                                                 |
| ----------- | ----------------------------------------------------------------------- |
| `on-change` | mirror after mutating lit commands (`new`, `start`, `update`, `close`, `comment`, `rank`, …), in addition to the pre-push hook. Spawns coalesce through a `mirror-pending` marker in the workspace's storage dir: a mutation that finds the marker provably still answered for (the claiming command and the mirror it spawns hold overlapping kernel-flock beacon lifetimes on `.links-sync-mirror.lock` that together cover claim through push) simply rides along — that mirror's HEAD read is still ahead of the mutation's commit — and otherwise the mutation claims the marker and spawns the mirror itself, so a burst runs a handful of mirrors, not one per mutation, and the burst's **final** mutation either rides a provably-answering mirror or spawns its own; a covering process that fails before pushing surfaces as a recorded failure retried at the next occasion, never a silent timing bet. Every push attempt clears the marker as it starts; a mirror that finds the marker re-claimed after its push runs another cycle, so nothing waits for "the next command" to be swept out. The default: a mutation on a connected workspace reaches the remote without a separate push step, so "durable locally" and "durable on the remote" don't drift apart into a manual act someone has to remember. |
| `on-push`   | mirror only when the managed pre-push hook runs (one push per `git push`). Opt-in, for a workspace that deliberately wants to batch outgoing network traffic instead of pushing on every mutation. |

`on-change` runs the same `lit sync push` the pre-push hook runs, after the
command completes, as a non-blocking background mirror. It is best-effort: a
push failure is surfaced on stderr and recorded as an automation trace, but
never fails the command — the ticket change is already durable in the local
Dolt store. Whichever cadence is chosen, a push that stays pending is not
silent. Every push attempt — mirror or explicit, including a mirror that fails
before its push can even start (engine open failure, spawner never exiting) —
records how it ended in a `push-outcome.last` marker in the workspace's
storage dir, and two banners read it:

- `lit backlog`, `lit next`, and `lit show` print a `sync: N local change(s)
  not pushed …` line whenever the store reads ahead-of-remote, and lead with
  a `sync: automatic push … is FAILING` line when the last push attempt
  failed.
- **Every mutating command** (`new`, `start`, `update`, `close`, `comment`,
  `rank`, …) prints the same failure banner after its own output when the
  last push attempt failed — so a session that only chains mutations against
  a broken remote hears about it on its very next command, not only when it
  happens to run one of the three read commands. (The mutating-side banner
  keys on "the last attempt failed", not the ahead count: a mutating command
  is always momentarily ahead at its own end, because its mirror pushes only
  after the process exits.)

`lit doctor` additionally names the durable evidence when the last attempt
failed: the failure reason and the mirror worker's log
(`<storage-dir>/mirror.log`, with its last-write age). An unknown cadence
value is rejected at config load.

## Receive automation

Push cadence governs getting *local* work onto the remote; receive governs
seeing *other machines'* work arrive. An established clone (one that already
adopted the remote on init) no longer needs a manual `lit sync pull` to observe
another machine's pushes — after a command runs, lit fetches the remote and
**fast-forwards** the local store when it is strictly behind. It is enabled by
default and toggled independently of push cadence:

```toml
[sync]
receive = true   # default
```

The receive runs **inline** — in the command's own process, after the command's
work is done and its engine is closed — not in a background worker. Embedded Dolt
permits only one read-write engine on a path at a time, so a worker fetching
concurrently with the next foreground command would make that command fail
"database is read only"; running the receive sequentially after close keeps a
single engine open at any moment. It is the lossless half of arrival: it only
fast-forwards a branch with no local commits to lose, so it never creates a merge
commit and never touches divergent local work. It is best-effort and bounded —
debounced so a command burst triggers at most one fetch per interval, gated on a
configured remote, and time-boxed so an offline or slow remote cannot hang the
command; failures are recorded as automation traces, never failing the command.
Set `LIT_DISABLE_AUTO_SYNC=1` to disable all automatic sync (mirror and receive)
for a process — useful for CI and sandboxes.

A clone that has made its *own* unpushed commits while the remote also moved is
*diverged*, not merely behind — a fast-forward cannot absorb it. The receive does
not fast-forward that case; instead it runs a **field-aware reconcile** inline, on
the same engine, right after the fast-forward check. The reconcile reads the
three-way state (base = merge-base, ours = local head, theirs = remote head) and
resolves it field by field with deterministic, no-clock rules: a field only one
side moved is taken from that side; a field both sides moved to different values
is settled by its policy (e.g. priority and status take the dominant value). The
merged result is replayed **forward on top of the remote head**, so the history
stays linear — no merge commit, no per-machine DAG — and the next push
fast-forwards. The replay preserves the folded side's per-commit provenance: each
local commit the spine lacked lands individually with its original message,
timestamp, and author (commits whose projection changes nothing are dropped —
for example schema/migration-only commits whose work the lift commit below
already carries), and a marker commit naming the reconcile settles the sequence — its diff
is whatever the merge policy itself decided beyond the folded side's content. When
the remote head is at an older schema, one machinery commit (`reconcile: lift
remote head to current schema`) precedes the provenance commits, carrying the
schema DDL and migration bookkeeping so no replayed commit's diff includes schema
work that was never its own; on a current-schema head no such commit lands. The
same granular replay serves the unrelated-history `combine` (each local commit
projected as its union with the remote backlog) and `take local` (whose marker
commit's diff is the owner-approved discard of the remote-only issues). The
reconcile is transparent for everything the rules can settle.

The one class the rules cannot settle is a concurrent **free-text rewrite** —
title, description, or agent prompt changed to different text on both sides. Those
are the only fields a reader can genuinely *merge* (preserving both intents)
rather than pick, so the reconcile commits nothing, leaves the local branch
untouched (still diverged, still usable on local truth), and holds the conflict as
a **prose-pending** state recorded on an automation trace for the agent surface to
merge inline. Reverting a peer's semantic field is incoherent distrust, so every
other field converges deterministically; prose is the only class an agent
merges, and one of two that stop the reconcile — the other, below, cannot be
merged by anyone.

## Two tickets under one id

A child id used to be minted as `<parent>.<highest existing child number + 1>`,
counted over the rows in the local store. Two disconnected stores that each held
the same fifteen children of an epic did not *race* for `.16` — both computed
it, deterministically, every time. Disconnection was the only precondition, and
disconnection is the ordinary state of parallel checkouts between syncs, so it
was certain, not rare. New child ids no longer work that way (see *Child ids are
minted, not counted* below), but every child minted before that change still
carries its number, and an import or restore writes whatever ids its file names
— so the refusal described here is still what stands between those rows and a
silent fusion.

The two rows are two pieces of work wearing one name, not one ticket that
diverged. Field-merging them produces a well-formed, unexecutable row: one job's
title over another job's description, the loser gone with no trace. Before this
fix that is exactly what happened, and when the two tickets happened to carry
similar text there was no prose conflict to hold, so the fused row was committed
autonomously with no signal at all.

The reconcile now distinguishes "the same ticket, diverged" from "two tickets,
one id" and refuses to field-merge the second. It commits nothing, leaves the
local branch where it found it (still diverged, still usable on local truth),
and reports BOTH tickets whole — which side it came from, its creation
timestamp, title, and description. Nothing was merged in, so the report is the
only place the other side's ticket is visible; that is why it is printed in
full rather than summarized. The side is named local or remote, not by
workspace id: a reconcile stamps every export it reads with its own id, so a
workspace column would print the same name on both rows. The state is surfaced
through the same sync-failure block every other blocking sync condition uses,
and it notifies the owner like the other divergence kinds.

The two cases are told apart by ancestry first: a merge-base row for the id
proves both sides descend from one creation, so they are one ticket however far
their fields have drifted. With no merge-base — which is also the normal state
on the unrelated-history `combine` path, where the base is empty by construction
— the tie-breaker is the creation timestamp, which is fixed when a ticket is
minted and never edited afterwards. Two `lit new` calls on two machines are two
instants; a replica of one ticket is one instant twice.

The resolution policy is settled: lit does not auto-resolve a collision, and it
deliberately ships no "keep both and re-id one" command, because a re-id is
harder than it sounds in two independent ways. Inside the store, an id is
referenced by relations (source and destination), comments, issue events, and
labels, so a re-id is a multi-table rewrite that must also carry the losing
ticket's history intact. Outside the store it is worse: this project's
convention is one PR per ticket named for its id, so ids are cited in commit
messages, PR titles and bodies, changelog entries, branch names, and the prose
of other tickets. A re-id leaves every one of those pointing at an id that still
resolves — to the wrong ticket. That is not a dangling reference anyone would
notice; it is a silently wrong one nobody would. So resolution stays
human-directed: read both tickets in the report and re-file one of the two jobs
under a free id. Retiring the duplicate id is not yet a lit operation, and
closing or deleting the losing row does not free the id — both are soft states,
the row still exports, so it still collides.

## Child ids are minted, not counted

`<parent>.<max local child + 1>` was a map of every child that exists anywhere
drawn from only the local corner, so every id minted while disconnected was a
guess — and the guesses were *correlated* rather than random, which is why they
collided reliably instead of occasionally. Top-level ids never had the problem:
they hash the issue's content, creator, and creation instant, then check locally
for uniqueness, widening the hash as the population grows. Children were the
only minting path that did not.

Children now mint the same way. A new child id is `<parent>.<hash>` — the parent
id, a dot, and a base36 content hash — produced by the same one minting function
top-level ids go through, with the parent's id as the namespace instead of
`<prefix>-<topic>-`. There is one id-minting behavior where there were two.

What that buys, and what it does not:

- **Two disconnected stores are unlikely to mint the same id.** The hash carries
  the creation instant at nanosecond resolution, so an id is no longer a claim
  about what exists on other machines. This is exactly the guarantee top-level
  ids have always run on — no weaker, no stronger, and probabilistic: the hash
  is truncated, so `Mint` re-rolls against the local store and a birthday chance
  remains against ids no local probe can see.
- **A freed id stays unreachable.** The old counter ran over LIVE rows, and the
  import delta hard-deletes, so deleting the highest child freed its number for
  a brand new, unrelated ticket — which then inherited the deleted ticket's
  ancestry as apparent evidence that the two were one ticket diverged. No second
  create lands on the first's nanosecond, so the slot cannot be reoccupied.
- **Existing ids are untouched.** This changes how NEW children are minted and
  migrates nothing. Every `<epic>.7` still resolves, still ranks, still exports.
  An epic will commonly hold both shapes.
- **Parentage still rides the id.** A child id is still its parent's id plus a
  dot plus one segment, which is what the top-level population count keys on.
  Parentage itself is read from the `relations` table, not parsed out of the id;
  the prefix is a display convenience, not the record.

What is lost is legibility, not meaning: an epic's children are displayed in
rank order, never id order, so the ordinal conveyed nothing the tool relied on.

Detection and prevention are both needed, and neither replaces the other:
prevention does not fix a store that already holds a collided pair, and
detection does not stop the next one.

## Owner notifications

The in-band surfaces above talk to whoever runs the next command — usually an
agent. The party who can actually *lose work* when sync degrades is the OWNER,
so lit also carries the event out of the terminal: when it detects a real
divergence (no common ancestor, a reconcile it could not converge, a held
prose conflict, an id naming two different tickets) or a push attempt fails,
it runs a shell command you configure — e.g. a push to an
[ntfy](https://ntfy.sh) topic — at detection time:

```toml
[sync]
owner_notify_cmd = 'curl -s -H "Title: lit sync degraded" -d "$LIT_NOTIFY_SUMMARY" https://ntfy.example/lit'
```

The command runs via `sh -c` in the repo root, time-boxed to 10s, with the
event's facts in the environment:

| variable             | value                                                            |
| -------------------- | ---------------------------------------------------------------- |
| `LIT_NOTIFY_KIND`    | `unrelated_histories`, `prose_held`, `diverged_unresolved`, `id_collision`, or `push_failed` |
| `LIT_NOTIFY_SUMMARY` | the one-sentence domain description of what degraded             |
| `LIT_NOTIFY_REMOTE`  | the sync remote concerned (may be empty for an unresolved push)  |
| `LIT_NOTIFY_BRANCH`  | the sync branch concerned                                        |
| `LIT_NOTIFY_REPO`    | the repository root, for owners who run several backlogs         |

Notifications are de-duplicated **per episode**: the first detection of a
condition fires immediately, re-detections of the same ongoing condition are
suppressed (re-pinging at most daily while it persists), and the episode ends
when the condition resolves — a landed push, a converged reconcile — so the
*next* occurrence notifies immediately again. A failed hook is loud on stderr,
recorded in the sync traces, and retried on the next detection. Empty (the
default) configures no channel and runs nothing; `LIT_DISABLE_AUTO_SYNC=1`
suppresses the hook along with all other automatic sync side effects.

## Destructive reconcile requires owner approval

Unrelated histories (independently-initialized stores sharing one remote) never
merge automatically — resolving them is a deliberate choice among:

- `lit sync reconcile combine` — the union: every issue kept, shared ids
  field-merged, an on-both prose conflict held for inline resolution. This is
  the keep-everything default and stays **agent-runnable** with no approval. It
  stops wholesale on one condition: a shared id that names two different
  tickets, where it commits nothing and reports both sides — see "Two tickets
  under one id" above.
- `lit sync reconcile take local|remote` — one side survives **wholesale and
  the other side's unique issues are permanently discarded**.

Because a take destroys one side's work, it refuses to run without the owner's
explicit approval. Run bare, it exits 5 with a refusal block naming exactly
which issues the take would discard and a one-time approval token; only

```text
lit sync reconcile take <side> --owner-approved <token>
```

runs the destruction. The token is a digest of both heads *and* the chosen
side: any new commit on either side — or presenting a take-local token to
take-remote — voids it, and the refusal re-mints a fresh one for the fork as it
now stands. The gate is procedural, not cryptographic: an agent at the fork is
told, unmistakably, that this decision belongs to the human who owns the
backlog, and the audit trail (the sync traces record every `owner_approval_required`
refusal and every approved take) shows who claimed that authority and when.
