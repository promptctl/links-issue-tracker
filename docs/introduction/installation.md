# Installation

## Requirements

- Git repository or worktree
- Dolt CLI `>= 1.81.10`
- Go toolchain (for `go install`)

## Install `lit`

```sh
./scripts/install.sh
```

Install from outside a checkout:

```sh
go install github.com/bmf/links-issue-tracker/cmd/lit@latest
```

## Enable shell completion (optional)

```sh
lit completion bash > ~/.local/share/bash-completion/completions/lit
lit completion zsh > ~/.zfunc/_lit
lit completion fish > ~/.config/fish/completions/lit.fish
```

## Install sync automation once per clone

```sh
lit hooks install
```

This installs a shared `pre-push` hook in your clone’s common Git dir so all worktrees inherit the same behavior.
