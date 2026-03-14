# Installation

## Requirements

- Git repository or worktree
- Dolt CLI `>= 1.81.10`
- Go toolchain (for `go install`)

## Install `lnks`

```sh
./scripts/install.sh
```

Install from outside a checkout:

```sh
go install github.com/bmf/links-issue-tracker/cmd/lnks@latest
```

## Enable shell completion (optional)

```sh
lnks completion bash > ~/.local/share/bash-completion/completions/lnks
lnks completion zsh > ~/.zfunc/_lnks
lnks completion fish > ~/.config/fish/completions/lnks.fish
```

## Install sync automation once per clone

```sh
lnks hooks install
```

This installs a shared `pre-push` hook in your clone's common Git dir so all worktrees inherit the same behavior.
