# Troubleshooting

## `links requires running inside a git repository/worktree`

Run commands from a repo/worktree directory.

## `dolt <version>+ is required`

Upgrade Dolt to `>= 1.81.10`.

## Sync warning on push hook

The hook is warn-only and never blocks push. The warning includes `trigger=git-pre-push`, the affected branch, and a `trace=` path under the workspace `traces_dir`. Retry manually:

```sh
lit sync push --remote origin --branch <branch>
```

Then check status:

```sh
lit sync status --json
```

## Integrity errors

Run:

```sh
lit doctor --json
lit fsck --repair --json
```

## Startup preflight blocked by Beads residue

When a non-`init` command is blocked by startup preflight, the error includes the blocked command, the remediation command, and a trace path under `traces_dir`.

## Unexpected state after import/restore

Use backups:

```sh
lit backup list --json
lit backup restore --latest --json
```
