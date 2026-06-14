# Installation

`lit` ships **prebuilt, self-contained binaries** for Linux, macOS, and Windows.
They carry no runtime dependencies — no Go toolchain, no system ICU or zstd — so
on a supported platform, installing `lit` is "download a binary and run it."
Building from source is the fallback for platforms without a prebuilt archive,
or for developing `lit` itself.

The `dolt` CLI is **not** required to run `lit` — the Dolt storage engine is
compiled into the binary. It's only used as a test oracle when developing `lit`.

## Requirements

- A git repository (or worktree) to track issues in.
- For the prebuilt install: nothing else on most systems. The installer needs
  `curl`, `tar`, and `sha256sum`/`shasum` (standard on Linux and macOS);
  `--latest-release` also needs `jq` to read the GitHub API.
- For building from source: the **Go toolchain** (see [below](#build-from-source-fallback)).

## Install a prebuilt binary (recommended)

### One line, latest release

```sh
curl -fsSL https://raw.githubusercontent.com/promptctl/links-issue-tracker/master/scripts/install.sh | bash -s -- --latest-release
```

This downloads the archive matching your OS and architecture from the latest
GitHub Release, verifies its SHA-256 against the published `checksums.txt`,
installs `lit` onto your `PATH`, and warns about any stale `lit` binaries that
would shadow it. Confirm it landed:

```sh
lit version
```

Pin a specific version instead of tracking the latest by swapping the flag:

```sh
curl -fsSL https://raw.githubusercontent.com/promptctl/links-issue-tracker/master/scripts/install.sh | bash -s -- --from-release v0.1.0
```

On Windows, run either form from Git Bash / MSYS — the archive is a `.zip`, so
`unzip` must be available there.

### Manual download

Prefer not to pipe a script through your shell? Download the archive for your
platform directly from the
[Releases page](https://github.com/promptctl/links-issue-tracker/releases/latest),
verify its checksum, extract the flat `lit` binary, and put it on your `PATH`:

```sh
VERSION=0.1.0
OS=linux            # linux | darwin | windows
ARCH=amd64          # amd64 | arm64
BASE="https://github.com/promptctl/links-issue-tracker/releases/download/v${VERSION}"

curl -fsSLO "${BASE}/lit_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "${BASE}/checksums.txt"

# Verify the checksum — Linux:
grep " lit_${VERSION}_${OS}_${ARCH}.tar.gz$" checksums.txt | sha256sum -c -
# ...or macOS:
grep " lit_${VERSION}_${OS}_${ARCH}.tar.gz$" checksums.txt | shasum -a 256 -c -

tar -xzf "lit_${VERSION}_${OS}_${ARCH}.tar.gz"
./lit version
sudo mv lit /usr/local/bin/        # or anywhere on your PATH
```

Windows publishes `lit_${VERSION}_windows_amd64.zip` containing `lit.exe`;
extract with `unzip` and move `lit.exe` onto your `PATH`.

Supported targets: `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`, `windows/amd64`.

## Build from source (fallback)

Use this when there's no prebuilt archive for your platform, or you're
developing `lit`. It requires the **Go toolchain** and a git checkout.

```sh
git clone https://github.com/promptctl/links-issue-tracker
cd links-issue-tracker
./scripts/install.sh
```

With no flags, `install.sh` builds `lit` from the checkout, installs it onto
your `PATH`, and warns about any stale `lit` binaries that would shadow it.

### macOS Homebrew note

Building from source on macOS links ICU and zstd. If `go build` fails with ICU
header or zstd linker errors, install the native dependencies and persist the
cgo search paths:

```sh
brew install icu4c@78 zstd
ICU_PREFIX="$(brew --prefix icu4c@78)"
ZSTD_PREFIX="$(brew --prefix zstd)"
go env -w CGO_CPPFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_CFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_CXXFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_LDFLAGS="-L${ICU_PREFIX}/lib -L${ZSTD_PREFIX}/lib"
```

(The prebuilt binaries above need none of this — ICU is statically linked into
them.)

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
