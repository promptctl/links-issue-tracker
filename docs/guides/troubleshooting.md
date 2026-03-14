# Troubleshooting

## `links requires running inside a git repository/worktree`

Run commands from a repo/worktree directory.

## `dolt <version>+ is required`

Upgrade Dolt to `>= 1.81.10`.

## Sync warning on push hook

The hook is warn-only and never blocks push. The warning includes `trigger=git-pre-push`, the remote, and a `trace=` path under the workspace `traces_dir`. Retry manually:

```sh
lnks sync push
```

Then check status:

```sh
lnks sync status --json
```

## Integrity errors

Run:

```sh
lnks doctor --json
lnks fsck --repair --json
```

## Startup preflight blocked by Beads residue

When a non-`init` command is blocked by startup preflight, the error includes the blocked command, the remediation command, and a trace path under `traces_dir`.

## Unexpected state after import/restore

Use backups:

```sh
lnks backup list --json
lnks backup restore --latest --json
```
