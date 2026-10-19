# The statusline file

lit keeps a small JSON file in each checkout, `.lit/statusline.json`, saying what
that checkout is working on and what `lit next` last picked for it. A terminal
statusline should read this file rather than run lit: every lit read waits on the
workspace lock, which can take several seconds while another command or the
background sync holds it.

## Where the file lives

The file is `.lit/statusline.json` at the root of the checkout. Each git worktree
is its own checkout with its own file, and a lit command only ever writes the
file of the checkout it runs in. From a script inside the checkout:

```sh
"$(git rev-parse --show-toplevel)/.lit/statusline.json"
```

The file does not exist until the first command that has something to record
runs in that checkout, usually `lit next` or `lit start`. `lit init` does not
create it. A script should treat a missing file as "nothing recorded yet".

The rest of `.lit/` holds committed project files, so the file is not
gitignored. Instead, the write that creates it adds `/.lit/statusline.json*` to
the repository's `info/exclude`, in the git common directory that every worktree
shares, so the file stays out of `git status`. Later writes do not check the
entry again. If the entry is removed while the file exists, `git status` lists
the file until you add the entry back, or delete the file so that the next write
recreates both.

lit writes the file to a temporary sibling and renames it into place, so a reader
always sees a complete file. Reading it needs no lock.

## Example

This is the file after `lit next` picked `links-auth-k2p`, `lit start` started
it, and `lit phase links-auth-k2p --to impl` moved it out of planning, in a lane
where one of four tickets was already closed:

```json
{
  "schema_version": 1,
  "working_on": {
    "ticket": {
      "id": "links-auth-k2p",
      "title": "Refresh tokens survive a clock skew"
    },
    "rollup_progress": {
      "rollup_completed_count": 1,
      "rollup_total_count": 4
    },
    "phase": "impl",
    "recorded_at": "2026-10-15T13:59:25.711166Z"
  },
  "next": {
    "ticket": {
      "id": "links-auth-k2p",
      "title": "Refresh tokens survive a clock skew"
    },
    "recorded_at": "2026-10-15T13:59:25.579056Z"
  }
}
```

`next` still names the ticket after it was started. It keeps the last answer a
plain `lit next` gave until another plain `lit next` replaces it, or until a
lifecycle change clears it, as described under
[clearing](#which-commands-set-and-clear-each-value).

## Fields

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | integer | The version of the file's shape. Currently `1`. |
| `working_on` | object or `null` | The ticket this checkout is working on. `null` when nothing is recorded. |
| `working_on.ticket.id` | string | The ticket's id. |
| `working_on.ticket.title` | string | The ticket's title. |
| `working_on.rollup_progress` | object | The progress of the ticket's lane, counted when `working_on` is recorded. lit 0.19 and 0.20 write `null` when the command that recorded `working_on` did not count the lane, and a lit older than 0.19 writes no `rollup_progress` key at all. |
| `working_on.rollup_progress.rollup_completed_count` | integer | How many of the lane's tickets are closed: the `N` of the `N/M done` on that lane's claim line in `lit backlog`. Archived and deleted tickets are not counted. |
| `working_on.rollup_progress.rollup_total_count` | integer | How many tickets the lane holds, not counting archived or deleted ones: the `M` of that `N/M done`. |
| `working_on.phase` | string or `null` | Where the work on the ticket stands: `plan`, `impl`, `review` or `merge`, as `lit show` prints it on the ticket's `phase:` line. `null` when the command that recorded `working_on` did not read the phase. A lit older than this field writes no `phase` key at all. |
| `working_on.recorded_at` | string | When lit recorded this ticket as worked on. |
| `next` | object or `null` | The last plain `lit next` answer. `null` when no answer is recorded. |
| `next.ticket` | object or `null` | The ticket that `lit next` picked, with `id` and `title` as above. `null` means `lit next` ran and found nothing to pick. |
| `next.recorded_at` | string | When that `lit next` ran. |

Timestamps are RFC 3339 in UTC, as in the example, and may carry fractional
seconds.

`next` has three states, and a statusline should tell them apart:

- `"next": null`: no answer on record.
- `"next": {"ticket": null, ...}`: `lit next` found no ready work.
- `"next": {"ticket": {...}, ...}`: `lit next` picked that ticket.

## Which commands set and clear each value

**`working_on`** is set by:

- `lit start <id>`, which records the ticket it started with its
  `rollup_progress`, counted once the start has landed. Starting a closed
  ticket takes it out of `rollup_completed_count`.
- `lit next`, when it hands back a ticket that is already in progress in a lane
  this checkout holds. This happens with or without filter flags, and it also
  counts `rollup_progress` again, reads `phase` again and updates
  `recorded_at`.

**`working_on.phase`** is the phase the ticket's history puts it in when one of
those commands records `working_on`, and `lit phase` changes it:

- `plan` is set by `lit start`, which enters plan when it moves a ticket into
  progress. Starting a ticket that is already in progress, as a takeover from
  another checkout does, keeps the phase its work had reached.
- `impl`, `review` and `merge`, and `plan` again, are set by
  `lit phase <id> --to <phase>`. Any phase can follow any other, so moving back
  from `review` to `impl` after review findings is a normal move. A move that
  names the phase the ticket is already in changes nothing in the file.

`lit phase` does not change `recorded_at`, which stays the time `working_on`
was recorded, and it changes nothing in the file when it moves a ticket other
than the one `working_on` names. A phase that a newer lit wrote under a name
this lit does not know is recorded as written, so a script should show a value
it does not recognise rather than drop it.

**`next`** is set by a plain `lit next`: one that neither narrows the pick with
`--assignee`, `--type`, `--status` or `--labels` nor turns on `--all`. A flag
that narrows nothing, such as `--status ""` or `--all=false`, leaves the run
plain. A filtered `lit next`, or one with `--all`, answers a different question
and leaves `next` alone. A focus scope configured for the repository does not
count as a filter.

**Titles.** After any command in this checkout changes a ticket's title, such as
`lit update --title` or a `lit import` document that sets `title`, every value
naming that ticket shows the new title. Its `recorded_at` does not change.

**Clearing.** A value becomes `null` when this checkout moves the ticket it names
through any lifecycle change other than a start: `lit done`, `lit close`,
`lit open`, `lit archive`, `lit unarchive`, `lit delete`, `lit restore`,
`lit purge`, `lit bulk close` and `lit bulk archive`. After `lit done` on the
ticket in the example above, both values are `null`. `lit backup restore`
replaces every ticket in the store, so it sets both values to `null` whichever
tickets they named.

Commands that record nothing, such as `lit show`, `lit ls` or `lit sync pull`,
leave the file untouched.

## When a value can be stale

The file records what this checkout's own commands did and when. Nothing else
updates it, so it can fall behind the store. Show `recorded_at` if the age
matters to you.

- **Changes made elsewhere.** A ticket closed, retitled or taken over in another
  checkout keeps its old value here, even after `lit sync pull` brings the change
  in. Only a command run in this checkout updates this checkout's file. This
  holds for `lit purge` too: the purged ticket's title stays in every other
  checkout's file. To remove the title, delete that checkout's file; a missing
  file reads as nothing recorded.
- **`phase` follows this checkout only.** A `lit phase` run in another
  checkout, on the ticket this one is working on, does not change this file
  until a command here records `working_on` again.
- **`rollup_progress` is a snapshot.** `working_on.rollup_progress` is counted when
  `working_on` is recorded. Tickets closed, reopened or filed in that lane
  afterwards, here or in another checkout, do not change it until the next
  `lit start` or `lit next` records `working_on` again.
- **`next` is a snapshot.** It is the answer as of `next.recorded_at`. Tickets
  filed, ranked or finished since then can change what `lit next` would pick, and
  nothing reruns it.
- **Claims are not tracked.** `working_on` stays set after this checkout's claim
  on the lane expires.
- **Two commands at once.** If two lit commands finish in the same checkout at
  the same moment, the later write wins, and the earlier command's values are
  lost until something records them again.
- **A failed write.** If lit cannot write the file, it prints
  `warning: statusline file not written:` on stderr. The command itself still
  succeeds, and the file keeps its previous contents. Each command's line in
  the [events file](events.md#what-a-statusline-file-write-records) also
  records what its write did, or why it failed.

## `schema_version`

`schema_version` changes when a field is removed or a field's meaning changes. It
does not change when a field is added. A script should check that
`schema_version` is the version it was written for and ignore fields it does not
know. A field added later can be missing even at the same `schema_version`,
because an older lit that rewrites the file drops the fields it does not know.

When lit finds a file it cannot parse, one with a different `schema_version`,
or one whose `working_on` has no `rollup_progress`, it replaces the whole file with what the current command recorded. It prints
`warning: statusline file was unreadable and has been rewritten:` on stderr when
it does.

## Showing it in cc-candybar

lit ships a [cc-candybar](https://github.com/promptctl/cc-candybar) plugin
that reads this file, at `.cc-candybar/plugins/lit/plugin.json5` in this
repository. cc-candybar 1.135.0 or later installs it:

```sh
cc-candybar plugin install promptctl/links-issue-tracker
```

Re-running it updates the plugin. After that, cc-candybar's add-segment
library lists three segments:

- `plugins.lit.ticket` shows the ticket being worked on, followed by the lane's
  `rollup_completed_count/rollup_total_count` when the file records them.
- `plugins.lit.phase` shows `phase` and the phase of the ticket being worked
  on, such as `phase review`, when the file records one.
- `plugins.lit.next` shows the ticket `lit next` last picked when it differs
  from the one being worked on, and `nothing ready` when that `lit next` found
  nothing.

Place one from cc-candybar's edit mode. Each segment hides while there is
nothing to show, and shows a warning naming the version when the file's
`schema_version` is not the one the plugin reads, and says which of lit and
the plugin to update.

## A minimal statusline

This script prints the ticket being worked on, with its rollup progress and
phase, and the next pick. It prints nothing when the file is missing, has a
schema version it does not know, or records nothing:

```sh
#!/bin/sh
f="$(git rev-parse --show-toplevel 2>/dev/null)/.lit/statusline.json"
[ -r "$f" ] || exit 0
jq -r '
  select(.schema_version == 1)
  | [ (.working_on | if . then "working: \(.ticket.id) \(.ticket.title)"
                             + (.rollup_progress | if . then " (\(.rollup_completed_count)/\(.rollup_total_count) done)" else "" end)
                             + (.phase | if . then " [\(.)]" else "" end)
                   else empty end),
      (.next | if . == null then empty
               elif .ticket == null then "next: nothing ready"
               else "next: \(.ticket.id)" end) ]
  | join("  |  ")
  | select(. != "")
' "$f"
```
