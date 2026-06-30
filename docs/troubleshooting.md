# Troubleshooting

## `links requires running inside a git repository/worktree`

Run commands from a repo/worktree directory.

## `dolt <version>+ is required`

Upgrade Dolt to `>= 1.81.10`.

## macOS build fails with `unicode/regex.h file not found` or zstd link errors

Building `lit` *from source* links the embedded Dolt engine via cgo, which needs
ICU and zstd headers/libraries on the search path. On macOS, Homebrew installs
them keg-only (off the default toolchain path), so cgo can't find them until
they're wired in. (The prebuilt release binaries link ICU/zstd statically and
need none of this — only compiling lit's own code does.)

The one-time fix:

```sh
just setup
```

`just setup` installs `icu4c@78` and `zstd` via Homebrew and persists the cgo
search paths into `go env`, so plain `go build`, `go test`, and your IDE all
work afterward. You don't strictly need it: `just build` and `just test` source
`scripts/cgo-env.sh` on every run, so they apply the paths even without setup.

To wire it by hand instead:

```sh
brew install icu4c@78 zstd
ICU_PREFIX="$(brew --prefix icu4c@78)"
ZSTD_PREFIX="$(brew --prefix zstd)"
go env -w CGO_CPPFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_LDFLAGS="-L${ICU_PREFIX}/lib -L${ZSTD_PREFIX}/lib"
```

`CGO_CPPFLAGS` feeds both the C and the C++ preprocessor, so it's the only
include flag needed (no separate `CGO_CFLAGS` / `CGO_CXXFLAGS`). Then retry:

```sh
just build       # or: go build -buildvcs=false ./cmd/lit
```

## Sync warning on push hook

The hook is warn-only and never blocks push. The warning includes `trigger=git-pre-push`, the remote, and a `trace=` path under the workspace `traces_dir`. Retry manually:

```sh
lit sync push
```

Then check status:

```sh
lit sync status
```

## Integrity errors

Run:

```sh
lit doctor
lit fsck --repair
```

## Startup preflight blocked by Beads residue

When a non-`init` command is blocked by startup preflight, the error includes the blocked command, the remediation command, and a trace path under `traces_dir`.

## Unexpected state after import/restore

Use backups:

```sh
lit backup list
lit backup restore --latest
```
