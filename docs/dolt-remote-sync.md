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
lit sync remote ls --json
lit sync fetch
lit sync pull --json
```

## Daily workflow

```sh
lit sync status
lit sync pull --json
# ...work with lit commands...
lit sync push --json
```

## Commands

- `lit sync status [--json]`
- `lit sync remote ls [--json]`
- `lit sync fetch [--remote <name>] [--prune] [--verbose] [--json]`
- `lit sync pull [--remote <name>] [--verbose] [--json]`
- `lit sync push [--remote <name>] [--set-upstream] [--force] [--verbose] [--json]`
- `lit sync reconcile` — merge a diverged clone into linear history; surfaces a concurrent free-text rewrite for the calling agent to merge
- `lit sync reconcile resolve --resolve ID:FIELD=TEXT …` — finalize the reconcile with the agent's merged text (one `--resolve` per pending field)
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
Successful and failed automatic runs both write trace files under the workspace `traces_dir` returned by `lit workspace --json`.

## Push cadence

The cadence — how often lit mirrors the store to the remote — is a single
config policy you own, not a per-command behavior. Set it under `[sync]` in
`config.toml` (global at `~/.config/links-issue-tracker/config.toml`, or
per-project at `.lit/config.toml`):

```toml
[sync]
cadence = "on-push"   # default
```

| value       | meaning                                                                 |
| ----------- | ----------------------------------------------------------------------- |
| `on-push`   | mirror only when the managed pre-push hook runs (one push per `git push`). The default and historical behavior. |
| `on-change` | additionally mirror after every mutating lit command (`new`, `start`, `update`, `close`, `comment`, `rank`, …), shrinking the window where local ticket state is invisible to other clones. |

`on-change` runs the same `lit sync push` the pre-push hook runs, after the
command completes. It is best-effort: a push failure is surfaced on stderr and
recorded as an automation trace, but never fails the command — the ticket
change is already durable in the local Dolt store. An unknown cadence value is
rejected at config load.

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
