# Troubleshooting

## `links requires running inside a git repository/worktree`

Run commands from a repo/worktree directory.

## `dolt <version>+ is required`

Upgrade Dolt to `>= 1.81.10`.

## macOS build fails with `unicode/regex.h file not found` or zstd link errors

On macOS with Homebrew, the Dolt driver stack needs ICU and zstd headers / libraries on the cgo search path.

Install dependencies:

```sh
brew install icu4c@78 zstd
```

Persist the Go toolchain flags:

```sh
ICU_PREFIX="$(brew --prefix icu4c@78)"
ZSTD_PREFIX="$(brew --prefix zstd)"
go env -w CGO_CPPFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_CFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_CXXFLAGS="-I${ICU_PREFIX}/include -I${ZSTD_PREFIX}/include"
go env -w CGO_LDFLAGS="-L${ICU_PREFIX}/lib -L${ZSTD_PREFIX}/lib"
```

Then retry:

```sh
go test ./internal/cli -run TestQuickstartOutputsStructuredJSON -count=1
go build -buildvcs=false ./cmd/lnks
```

## Sync warning on push hook

The hook is warn-only and never blocks push. The warning includes `trigger=git-pre-push`, the remote, and a `trace=` path under the workspace `traces_dir`. Retry manually:

```sh
lnks sync push
```

Then check status:

```sh
lnks sync status --json
```

## Integrity errors

Run:

```sh
lnks doctor --json
lnks fsck --repair --json
```

## Startup preflight blocked by Beads residue

When a non-`init` command is blocked by startup preflight, the error includes the blocked command, the remediation command, and a trace path under `traces_dir`.

## Unexpected state after import/restore

Use backups:

```sh
lnks backup list --json
lnks backup restore --latest --json
```
