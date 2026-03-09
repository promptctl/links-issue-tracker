# Getting started

## 1. Verify workspace state

```sh
lit workspace --json
```

If this repository used Beads before, run the migration once:

```sh
lit migrate beads --apply --json
```

## 2. Create your first issue

```sh
lit new --title "First task" --type task --priority 2 --json
```

## 3. List and inspect

```sh
lit ls --json
lit show <issue-id> --json
```

## 4. Connect remotes (Git is canonical)

```sh
git remote -v
lit sync remote ls --json
```

## 5. Pull/push issue state

```sh
lit sync pull --remote origin --branch main
# ...make lit changes...
lit sync push --remote origin --branch main
```

## 6. Health check

```sh
lit doctor --json
```
