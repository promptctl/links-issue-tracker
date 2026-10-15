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
  "counts": {"next.rows_gathered": 1},
  "facts": {
    "next.outcome": "served_from_new_lane",
    "next.picked": "app-auth-082.oa5",
    "next.exhausted_epics": [],
    "next.blocked": [],
    "next.held": [],
    "next.off_path": [],
    "next.unreachable": [],
    "next.stay_offered": false
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
| `facts` | What the command decided, under keys prefixed with the command's name. |

A command that records no counts or facts writes `{}` for each.

## What `lit next` records

Every `lit next` that reaches a decision writes every key below, empty when it
does not apply. A run that fails before deciding, such as one given an invalid
filter, writes `"outcome": "error"` and none of these keys.

| Key | Meaning |
| --- | --- |
| `next.outcome` | How the pick was reached: `served_from_claim`, `resumed_own_work`, `served_from_epic_lane`, `served_from_new_lane`, `served_from_dependency`, `served_past_exhaustion`, `exhausted`, or `no_work`. |
| `next.picked` | The ticket id it printed, or `null` when it picked nothing. |
| `next.exhausted_epics` | The epics whose open work it found unreachable. |
| `next.blocked` | The dependencies blocking those epics. |
| `next.held` | Tickets in those epics held by something about the ticket itself, such as a reserved label. |
| `next.off_path` | Tickets a focus label kept out of the pick. |
| `next.unreachable` | When it found nothing at all, every ticket it went past. |
| `next.stay_offered` | Whether its output offered filing a ticket to unblock the epic instead of moving on. |
| `counts.next.rows_gathered` | How many tickets it chose among. |

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
| `not_ready` | Not held elsewhere, but blocked by a further dependency. |
| `awaiting_outside` | Labeled `external`: it waits on an event outside this repository. |
| `out_of_view` | Nothing that would clear it was among the tickets this run read, for example because a filter hid them. |
| `off_focus_path` | Outside the focus label's scope, so the pick never considered it. |

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
