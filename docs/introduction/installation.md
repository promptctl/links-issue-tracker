# Installation

## Requirements

- Git repository or worktree
- Go toolchain (`lit` builds from source)

The `dolt` CLI is **not** required to run `lit` — the Dolt storage engine is compiled into
the binary. It's only used as a test oracle when developing `lit` itself.

## Install `lit`

```sh
git clone https://github.com/promptctl/links-issue-tracker
cd links-issue-tracker
./scripts/install.sh
```

`install.sh` builds `lit` from the checkout, installs it onto your `PATH`, and warns about
any stale `lit` binaries that would shadow it.

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
