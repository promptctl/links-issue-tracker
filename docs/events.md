# The events file

Every lit invocation appends one line to a per-user file saying which command
ran, where, how it ended, and what the command decided. When you need to know
afterwards what a command did — what `lit next` told a session an hour ago, or
how often a command failed and why — read this file. A command's own output
says the same things once, to whoever ran it; the events file keeps them.

## Where the file lives

The file is `$XDG_STATE_HOME/links-issue-tracker/events.jsonl`, or
`~/.local/state/links-issue-tracker/events.jsonl` when `XDG_STATE_HOME` is unset.
There is one file per user, not per repository: every checkout and every
repository on the machine appends to it, and so does an invocation outside any
repository, such as `lit version`. The `cwd` on each line says where it ran.
Only you can read the file: it is created with mode `0600`, in a directory
created with mode `0700`.

Each line is one complete JSON object, written in a single append, so lines
from commands finishing at the same moment never interleave. Once the file
grows past 8 MiB, the next invocation renames it to `events.jsonl.1`, replacing
any earlier `.1`, and starts a new file. Invocations take turns at this through
a lock file, `events.jsonl.lock`, so two of them never both rotate.

If lit cannot write the line, it prints a warning on stderr and the command's
own result is unchanged.

## What a line holds

This is the line `lit next` left after picking a ticket, spread over several
lines here for reading:

```json
{
  "event": "lit next",
  "trace_id": "c9ec05b836ebd4321e619173f658160d",
  "service": "lit",
  "version": "0.15.0",
  "commit": "90c0d2c",
  "pid": 96452,
  "cwd": "/home/dev/src/app",
  "flags": [],
  "started_at": "2026-10-15T14:42:02.943313Z",
  "duration_ms": 103,
  "outcome": "ok",
  "exit_code": 0,
  "sink": "file",
  "sink_rotation": "none",
  "counts": {"next.rows_gathered": 1, "statusline.bytes": 199},
  "facts": {
    "next.outcome": "served_from_new_lane",
    "next.picked": "app-auth-082.oa5",
    "next.dependency_path": [],
    "next.exhausted_epics": [],
    "next.exhausted_progress": {"done": 0, "total": 0},
    "next.blocked": [],
    "next.held": [],
    "next.off_path": [],
    "next.unreachable": [],
    "next.stay_offered": false,
    "statusline.changed": ["next"],
    "statusline.rollup_progress": null,
    "statusline.phase": null,
    "statusline.created": false,
    "statusline.exclude_added": false,
    "statusline.replaced": null,
    "statusline.error": null,
    "statusline.plugin_installed": false,
    "statusline.plugin_error": null
  }
}
```

| Field | Meaning |
| --- | --- |
| `event` | The command that ran, by its full path: `lit next`, `lit sync push`. An invocation that never reached a command, such as an unknown command name, is `lit`. |
| `trace_id` | A random 32-hex-digit id, different for every invocation. |
| `service` | Always `lit`. |
| `version`, `commit` | The binary's stamped version and commit. Both are empty for a build that was not stamped, such as `go build`. |
| `pid`, `cwd` | The process and the directory it ran in. |
| `flags` | The names of the flags given, in alphabetical order. Flag values are never recorded, because a value can be a ticket title, a comment body, or a pasted secret. |
| `started_at`, `duration_ms` | When the invocation started, in UTC, and how long it ran. |
| `outcome` | `ok`, `error`, `panic`, or `interrupted`: still running when the grace after a Ctrl-C or `SIGTERM` ran out, so lit exited without finishing. |
| `exit_code` | The process's exit code, the same one a failed command prints on its error line; `2` for a panic; `130` or `143` for an interrupted invocation (128 plus the signal number). |
| `reason` | Present only on a failure: the reason token printed beside the exit code (see the [CLI reference](cli-reference.md)). |
| `error` | Present unless the outcome is `ok`: the error message as lit printed it, which can quote a value you typed, the panic value, or, for an interrupted invocation, the signal (`SIGINT` or `SIGTERM`) whose grace ran out. |
| `sink` | Where the line was written. Always `file`. |
| `sink_rotation` | What happened to the file's size cap before this line was appended: `none`, `rotated` (this is the first line of a new file), or `failed` (the file keeps growing past its cap). |
| `sink_rotation_error` | Present only when `sink_rotation` is `failed`: why. |
| `counts` | The command's counts. A count of zero is written as `0`, so "ran and found none" is distinguishable from a command that does not count that thing. |
| `facts` | What the command decided, under keys prefixed with the command's name, with `statusline.` for what it wrote to the statusline file, with `workflows.` for the workflow occasion it fired, or with `schema_ahead.` for a refusal because the workspace's schema is ahead of this lit. |

A command that records no counts or facts writes `{}` for each.

## What `lit next` records

Every `lit next` that reaches a decision writes every key below, empty when it
does not apply. A run that fails before deciding, such as one given an invalid
filter, writes `"outcome": "error"` and none of these keys.

| Key | Meaning |
| --- | --- |
| `next.outcome` | How the pick was reached: `served_from_claim`, `resumed_own_work`, `served_from_epic_lane`, `served_from_new_lane`, `served_from_dependency`, `served_past_exhaustion`, `served_past_narrowing`, `exhausted`, `narrowed`, or `no_work`. |
| `next.picked` | The ticket id it printed, or `null` when it picked nothing. |
| `next.dependency_path` | For `served_from_dependency`, how the pick clears the way: the ticket of yours it unblocks, each prerequisite between — an epic followed by the ticket under it the walk went through — then the dependency the pick is work toward, which is the pick itself or the epic it sits under. Empty for every other outcome. |
| `next.exhausted_epics` | The epics whose open work it found unreachable. |
| `next.exhausted_progress` | How far the stopped scope had come, counted over every lane of it: `done` is how many of its tickets are closed and `total` is how many there are, archived and deleted tickets left out. Both are `0` when no scope stopped. |
| `next.blocked` | The prerequisites from outside those epics that block them, at any depth: dependencies, and the earlier ticket of a blocker's own lane. Each entry also carries `clearable`: whether a ticket filed here could free it, which is what the route to stay offers to clear. |
| `next.held` | Tickets in those epics held by something about the ticket itself, such as a reserved label. |
| `next.off_path` | Tickets a focus label kept out of the pick. |
| `next.hidden_by_filter` | The ticket the `--type`, `--status`, `--labels` or `--assignee` filter left out when that is why the run served past its epic or served nothing: the one `lit next` without the filter would hand you. Always set for `served_past_narrowing` and `narrowed`, set for `exhausted` when the filter hid every ready ticket outside the epic, and `null` otherwise. |
| `next.unreachable` | When it found nothing at all, every ticket it went past. |
| `next.stay_offered` | Whether its output offered filing a ticket to unblock the epic instead of moving on. |
| `counts.next.rows_gathered` | How many tickets it chose among: the workable tickets its filter selects. |

Each entry in the four ticket lists has the same shape:

```json
{"id": "app-auth-082", "ticket": "app-auth-082.oa5", "reach": "held_fresh"}
```

`id` is the id the output named, `ticket` is the ticket the entry's reach was
read from (the same id, a ticket under an epic, or empty when lit could see
none), and `reach` is what this checkout can do about it:

| `reach` | Meaning |
| --- | --- |
| `takeable` | This checkout can start it. |
| `held_fresh` | Another checkout holds its lane right now. |
| `held_by_itself` | Not held elsewhere, but held by something about the ticket itself, such as a reserved label or a missing field. |
| `not_ready` | Not held elsewhere, but blocked by further open tickets and nothing about itself. |
| `awaiting_outside` | Labeled `external`: it waits on an event outside this repository. |
| `out_of_view` | Nothing that would clear it was among the tickets this run read, for example an epic with no open ticket under it. A filter never causes it: `lit next` reads every workable ticket and filters only what it hands you. |
| `off_focus_path` | Outside the focus label's scope, so the pick never considered it. |

## What a statusline-file write records

Every command that works on the tickets of the checkout it runs in, such as
`lit next`, `lit start` or `lit show`, records what it did to the checkout's
[statusline file](statusline.md) once it has run, even when it ends in an
error. A command that learned nothing for the file writes nothing to it and
records zero bytes and no changed slots. The keys are present exactly when the
command reached that write: a line without them is either a command that does
not work on this checkout's tickets, such as `lit sync push`, `lit init` or
`lit ls --at`, or one whose `outcome` is `panic` or `interrupted`.

| Key | Meaning |
| --- | --- |
| `statusline.changed` | The file's slots whose records this write changed: `working_on`, `next`, both, or `[]`. A slot re-recorded with the ticket it already held is listed, since its `recorded_at` moved; a slot the command's facts did not reach is not. When the file was replaced, both are listed. |
| `statusline.rollup_progress` | The rollup progress the file's `working_on` shows after this write, as `{"rollup_completed_count": N, "rollup_total_count": M}`, whichever command counted it; `null` when nothing was written or `working_on` is `null` or carries no count. |
| `statusline.phase` | The phase the file's `working_on` shows after this write, such as `"impl"`, whichever command read it; `null` when nothing was written or `working_on` is `null` or carries no phase. |
| `statusline.created` | Whether the file did not exist before this write. |
| `statusline.exclude_added` | Whether this write added the file's entry to the repository's `info/exclude`. Only a write that creates the file adds it, and only when the entry is missing. |
| `statusline.replaced` | Why the file already on disk could not be read and was rewritten from this command's facts alone, or `null`. |
| `statusline.error` | Why the write failed, or `null`. The command's own result is unchanged; the other keys say what the write had done before it failed. |
| `statusline.plugin_installed` | Whether this write installed lit's [cc-candybar plugin](statusline.md#showing-it-in-cc-candybar), because no copy of this lit's definition was there. `false` when the definition was already installed, and when nothing was written, since the plugin is installed only after the file is. |
| `statusline.plugin_error` | Why the plugin could not be installed, or `null`. The file is written all the same. |
| `counts.statusline.bytes` | The size of the file written; `0` when nothing was written. |

## What opening a store records

A command that opens a store to read or change its tickets records what the
open did to the store's schema. An open that finds the schema behind this lit
migrates it forward, taking a recovery snapshot first, then removes the oldest
migration snapshots past the ten it keeps. For `lit ls --at`, that store is the
one `--at` names, not the working directory's.

| Key | Meaning |
| --- | --- |
| `migration.snapshot` | The recovery snapshot the open took before migrating, or `""` when there was nothing to migrate. |
| `migration.snapshot_prune_error` | Why older migration snapshots could not be removed after the migration, or `null`. The command runs all the same, and prints a `warning: old migration snapshots were not removed:` line on stderr. A command whose open fails after migrating records neither key, and its error includes the failure. |

`lit stores --counts` opens every store it finds, so it records counts
instead.

| Key | Meaning |
| --- | --- |
| `counts.stores.migrated` | The stores whose open migrated them. |
| `counts.stores.snapshot_prune_failures` | The stores whose open migrated them and then could not remove older migration snapshots. Each gets a `~ <storage dir>: warning: old migration snapshots were not removed:` line after the table. |

## What a schema-ahead refusal records

A command ends with reason `workspace_schema_ahead` when a schema it was asked
to operate on is ahead of this lit: the workspace's, or the sync remote's. Which
keys it records says which.

A command refused because the workspace's schema is ahead records which snapshot
the refusal offered as a lossy rollback, and how many migration snapshots it read
to decide. Every key is present exactly when the command ended in that refusal.

| Key | Meaning |
| --- | --- |
| `schema_ahead.rollback_snapshot` | The migration snapshot the refusal offered to `lit snapshots restore`, or `""` when it found none holding data this lit opens. |
| `schema_ahead.rollback_lookup_error` | Why snapshots could not be listed or read, or why the search stopped, or `null`. Set beside an offered snapshot when a newer one could not be read. |
| `counts.schema_ahead.snapshots_examined` | The migration snapshots the refusal read, newest first, until it found one to offer. |
| `counts.schema_ahead.snapshots_unreadable` | Those of them it could not read. |

A sync command refused because the remote's schema is ahead, so this lit will
not push to or reconcile with it, records the remote and both schema versions.
Every key is present exactly when the command ended in that refusal.

| Key | Meaning |
| --- | --- |
| `schema_ahead.remote` | The remote whose schema is ahead. |
| `schema_ahead.branch` | The branch on that remote. |
| `schema_ahead.remote_schema` | The schema version of the remote branch's head. |
| `schema_ahead.running_schema_max` | The highest schema version this lit supports. |

## What `lit init` records

Init sets its key once the workspace has a database, so an init that stops
before then carries its refusal instead.

| Key | Meaning |
| --- | --- |
| `init.db_created` | `true` when this init built the database or adopted the remote's; `false` when the database was already there, including one another init built while this one waited for it. |

## What `lit start` records

A start reads who holds the ticket's lane, confirms a takeover when another
checkout holds it, and then writes. `start.lane_held` is set before the
takeover is confirmed, so a refused takeover carries it beside its refusal;
the other two keys are set once the write has landed.

| Key | Meaning |
| --- | --- |
| `start.lane_held` | `true` when a checkout held the lane, this one or another; `false` when nobody did, including when the last claim expired. |
| `start.handover` | `true` when the write moved the ticket's claim from whoever last started it to a new holder: another checkout, or a new assignee on the same one. |
| `start.transfer_announced` | `true` when the start printed `claim transferred`. That needs a handover and a lane that still named the previous holder, so it is `false` when that claim had expired, the ticket was finished, or a later start elsewhere in the lane had taken over. |

## What `lit phase` records

A phase move reads the phase the issue is in, then moves it. A refused move,
such as one on an issue that is not `in_progress`, fails before these keys are
set and carries its refusal instead.

| Key | Meaning |
| --- | --- |
| `phase.from` | The phase the issue was in. |
| `phase.to` | The phase the command moved it to, the one `--to` names. |
| `phase.changed` | `false` when the issue was already in that phase, so nothing was recorded; `true` otherwise. |

## What a long-text field records

`lit new`, `lit followup` and `lit update` record where their `--description` and
`--prompt` came from, and `lit comment add` records its `--body` the same way.
Fields are read in that order, and a refusal stops the reading. A field given both
ways, or one of two fields naming stdin, is refused before its key is set. A path
that cannot be read, or a file or stdin whose text is blank, is refused with its
key already set.

| Key | Meaning |
| --- | --- |
| `text_source.<name>` | `inline` for `--<name>`, `file` for `--<name>-file <path>`, `stdin` for `--<name>-file -`, `absent` when the invocation gave neither. |

## What a workflow occasion records

A command that fires a [workflow](workflows.md) occasion records it:
`lit new`, `lit followup`, `lit show`, `lit update`, `lit comment add`,
`lit next` when it hands back a ticket, `lit backlog`, and the lifecycle
transitions `lit start`, `lit done`, `lit close`, `lit open` and `lit phase`.
A command that failed before the occasion fired, such as a refused transition,
has none of these keys.

| Key | Meaning |
| --- | --- |
| `workflows.event` | The event the occasion fired, such as `work_started` or `phase_set`. |
| `workflows.entered` | The states the ticket entered, outermost first: `["in_progress", "plan"]` for a start from `open`, `["review"]` for a phase move to `review`. `[]` when the command moved no state. |
| `workflows.exited` | The states the ticket left, in the same order: `["in_progress", "review"]` for a `lit done` from `review`. `[]` when the command moved no state. |
| `workflows.fired` | The ids of the definitions that fired, in the order their guidance was printed; `[]` when none did. |

## What `lit upgrade` records

An upgrade resolves a release, the one `--to` names or the latest published one,
then either keeps this binary (a bare run already on that release), refuses (the
release's schema support ends below the workspace's schema), or installs it. Each
key is set as soon as the command knows it, so a run that stops early still
carries the ones it had.

| Key | Meaning |
| --- | --- |
| `upgrade.tag` | The release the command resolved: the one it installed, kept the binary past, or refused. Set once the tag is known. |
| `upgrade.target_schema_max` | The highest schema version that release supports. Set once the release resolves. |
| `upgrade.running_schema_min` | The lowest schema version this lit supports. Set with the target. |
| `upgrade.running_schema_max` | The highest schema version this lit supports. Set with the target. |
| `upgrade.workspace_schema` | The workspace's applied schema version. Set once it is read, before the install. |
| `upgrade.already_current` | `true` when a bare run kept this binary because it is already on the latest release; `false` when the command went on to install. |
| `upgrade.installed_path` | The path the release was installed to. Absent when nothing was installed. |

## What `lit downgrade` records

A downgrade reverses the workspace's schema from its applied version down to the
`--to` release's, and it can do that only when this lit's schema support covers
every version in between. When the workspace is past that support (a newer lit
migrated it, and only that newer lit carries the down-migrations), the command
refuses before it takes a snapshot, with reason `workspace_schema_ahead`. Each key
is set as soon as the command knows it, so a run that stops early still carries
the ones it had.

| Key | Meaning |
| --- | --- |
| `downgrade.tag` | The `--to` release. Set once the tag is known. |
| `downgrade.target_schema_max` | The schema version of the `--to` release. Set once the release resolves. |
| `downgrade.running_schema_min` | The lowest schema version this lit supports. Set with the target. |
| `downgrade.running_schema_max` | The highest schema version this lit supports. Set with the target. |
| `downgrade.workspace_schema` | The workspace's applied schema version, as the reverse read it under the workspace lock and decided from. Set once the reverse has read it, whether the command then succeeds, refuses, or fails; absent, with the snapshot and the count, when it failed before that read. |
| `downgrade.snapshot` | The recovery snapshot taken before the first down-migration, or `""` when none was taken. |
| `counts.downgrade.reversed` | The down-migrations applied. `0` when none ran; on a reverse that failed part-way, those applied before it stopped. |
| `downgrade.snapshot_prune_error` | Why older downgrade snapshots could not be removed after the reverse, or `null`. The downgrade prints the failure as a `warning:` line and still installs the `--to` release. |
| `downgrade.installed_path` | The path the `--to` release was installed to. Absent when nothing was installed. |

## What `lit doctor` records

Doctor sets its keys once the report it prints is read, after any `--fix`
repairs have run, so they describe the state the command left behind. The
exception is `doctor.reranked`, which the repair sets as it runs.

| Key | Meaning |
| --- | --- |
| `doctor.duplicate_ranks` | The rank keys more than one non-deleted issue holds, as the status line prints it: a count such as `"0"`, or `"unchecked"` when a parent cycle kept the check from running. |
| `doctor.rank_inversions` | The blocked-by edges between live issues whose dependency tree order lists after its dependent, as the status line prints it: a count, or `"unchecked"`. Edges inside a group of frame-mates whose lifted edges loop are not counted. |
| `doctor.dependency_cycle` | A loop of blocked-by edges between live issues, its members joined by `->` with the first repeated last, `"none"` when there is no loop, or `"unchecked"`. `--fix=rank` refuses while one stands. |
| `doctor.interleaved` | How many interleaved groups the report names, or `"unchecked"`: frame-mates whose issues wait on one another both ways through no loop of their own edges. |
| `doctor.reranked` | The issues `--fix=rank` gave a new rank key, as an integer. Present only when that repair runs. |

## Reading the file

`jq` answers most questions directly. The last ten `lit next` decisions made in
one checkout:

```sh
jq -c 'select(.event == "lit next" and .cwd == "'"$PWD"'")
       | {started_at, outcome: .facts["next.outcome"], picked: .facts["next.picked"]}' \
  ~/.local/state/links-issue-tracker/events.jsonl | tail -10
```

How often each command failed, by reason:

```sh
jq -r 'select(.outcome == "error") | "\(.event)\t\(.reason)"' \
  ~/.local/state/links-issue-tracker/events.jsonl | sort | uniq -c | sort -rn
```
