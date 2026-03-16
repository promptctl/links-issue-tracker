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

### macOS Homebrew note

If Go builds fail with ICU header or zstd linker errors, install the native dependencies and persist the cgo search paths:

```sh
brew install icu4c@78 zstd
ICU_PREFIX="$(brew --prefix icu4c@78)"
ZSTD_PREFIX="$(brew --prefix zstd)"
go env -w CGO_CPPFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_CFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_CXXFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_LDFLAGS="-L${ICU_PREFIX}/lib -L${ZSTD_PREFIX}/lib"
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
