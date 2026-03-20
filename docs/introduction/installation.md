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
lit completion bash > ~/.local/share/bash-completion/completions/lit
lit completion zsh > ~/.zfunc/_lit
lit completion fish > ~/.config/fish/completions/lit.fish
```

## Install sync automation once per clone

```sh
lit hooks install
```

This installs a shared `pre-push` hook in your clone's common Git dir so all worktrees inherit the same behavior.
