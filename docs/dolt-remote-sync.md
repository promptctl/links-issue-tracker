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
| `on-change` | mirror after every mutating lit command (`new`, `start`, `update`, `close`, `comment`, `rank`, …), in addition to the pre-push hook. The default: a mutation on a connected workspace reaches the remote without a separate push step, so "durable locally" and "durable on the remote" don't drift apart into a manual act someone has to remember. |
| `on-push`   | mirror only when the managed pre-push hook runs (one push per `git push`). Opt-in, for a workspace that deliberately wants to batch outgoing network traffic instead of pushing on every mutation. |

`on-change` runs the same `lit sync push` the pre-push hook runs, after the
command completes, as a non-blocking background mirror. It is best-effort: a
push failure is surfaced on stderr and recorded as an automation trace, but
never fails the command — the ticket change is already durable in the local
Dolt store. Whichever cadence is chosen, a push that stays pending is not
silent. Every push attempt — mirror or explicit — records how it ended in a
`push-outcome.last` marker in the workspace's storage dir, and two banners
read it:

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
merged result is replayed as **one forward commit on top of the remote head**, so
the history stays linear — no merge commit, no per-machine DAG — and the next push
fast-forwards. The reconcile is transparent for everything the rules can settle.

The one class the rules cannot settle is a concurrent **free-text rewrite** —
title, description, or agent prompt changed to different text on both sides. Those
are the only fields a reader can genuinely *merge* (preserving both intents)
rather than pick, so the reconcile commits nothing, leaves the local branch
untouched (still diverged, still usable on local truth), and holds the conflict as
a **prose-pending** state recorded on an automation trace for the agent surface to
merge inline. Reverting a peer's semantic field is incoherent distrust, so every
other field converges deterministically; prose is the sole agent boundary.

## Owner notifications

The in-band surfaces above talk to whoever runs the next command — usually an
agent. The party who can actually *lose work* when sync degrades is the OWNER,
so lit also carries the event out of the terminal: when it detects a real
divergence (no common ancestor, a reconcile it could not converge, a held prose
conflict) or a push attempt fails, it runs a shell command you configure —
e.g. a push to an [ntfy](https://ntfy.sh) topic — at detection time:

```toml
[sync]
owner_notify_cmd = 'curl -s -H "Title: lit sync degraded" -d "$LIT_NOTIFY_SUMMARY" https://ntfy.example/lit'
```

The command runs via `sh -c` in the repo root, time-boxed to 10s, with the
event's facts in the environment:

| variable             | value                                                            |
| -------------------- | ---------------------------------------------------------------- |
| `LIT_NOTIFY_KIND`    | `unrelated_histories`, `prose_held`, `diverged_unresolved`, or `push_failed` |
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
  the keep-everything default and stays **agent-runnable** with no approval.
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
