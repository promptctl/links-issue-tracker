# Getting started

## 1. Verify workspace state

```sh
lnks workspace --json
```

Use `--json` when you need machine-readable output. Without it, `lnks` always prints standard text.

If this repository used Beads before, run the migration once:

```sh
lnks migrate --apply --json
```

## 2. Create your first issue

```sh
lnks new --title "First task" --type task --priority 2 --json
```

## 3. List and inspect

```sh
lnks ls --json
lnks show <issue-id> --json
```

## 4. Connect remotes (Git is canonical)

```sh
git remote -v
lnks sync remote ls --json
```

## 5. Pull/push issue state

```sh
lnks sync pull --json
# ...make lnks changes...
lnks sync push --json
```

## 6. Health check

```sh
lnks doctor --json
```
