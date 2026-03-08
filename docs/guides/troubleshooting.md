# Troubleshooting

## `links requires running inside a git repository/worktree`

Run commands from a repo/worktree directory.

## `dolt <version>+ is required`

Upgrade Dolt to `>= 1.81.10`.

## Sync warning on push hook

The hook is warn-only and never blocks push. Retry manually:

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

## Unexpected state after import/restore

Use backups:

```sh
lit backup list --json
lit backup restore --latest --json
```
