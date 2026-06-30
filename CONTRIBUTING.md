# Contributing to links (`lit`)

Thanks for your interest in improving `links-issue-tracker`. This project
**dogfoods itself**: its own backlog lives in the repo and is driven with `lit`.
Contributing here means using the same agent-native loop the tool is built for.

## Code of Conduct

This project is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). By
participating, you are expected to uphold it.

## Security

Found a vulnerability? Please report it privately — see the
[Security Policy](SECURITY.md).

## Development setup

You need:

- **A git repository** — `lit` stores its data inside one.
- **The Go toolchain.** The required version is the `go` directive in
  [`go.mod`](go.mod); a recent Go will auto-download a matching toolchain.
- **The `dolt` CLI** — *only for running the test suite*. `lit` itself compiles
  the Dolt storage engine in and does **not** need the CLI at runtime; some
  tests use `dolt` as an oracle. Install it from
  [dolthub/dolt](https://github.com/dolthub/dolt) (CI pins the exact version it
  installs in [`.github/workflows/ci.yml`](.github/workflows/ci.yml)).

> On macOS, building the embedded engine needs ICU and zstd headers, which
> Homebrew installs keg-only. Run `just setup` once (it installs `icu4c@78` +
> `zstd` and persists the cgo paths into `go env`), and every build below — `just`
> recipes, raw `go build`/`go test`, and your IDE — just works. Details:
> [docs/introduction/installation.md](docs/introduction/installation.md).

## Build, install, test, lint

With [`just`](https://github.com/casey/just) installed, the
[`Justfile`](Justfile) is the build entrypoint — its recipes put ICU/zstd on the
cgo path automatically (via `scripts/cgo-env.sh`), so they work on macOS even if
you skip `just setup`:

```sh
just setup    # one-time per machine: native deps + persist cgo paths (macOS)
just build    # build the lit binary
just test     # run the full suite (needs the dolt CLI; see above) — args pass through
just lint     # golangci-lint against .golangci.yml
just install  # build from source and install onto your PATH
```

The equivalent raw commands (these need the cgo paths already on your env — i.e.
after `just setup`, or run via `just`):

```sh
go build ./cmd/lit    # build the lit binary
./scripts/install.sh  # build and install onto your PATH (wires cgo paths itself)
go test ./...         # run the full test suite (needs the dolt CLI; see above)
golangci-lint run     # lint against .golangci.yml before opening a PR
go mod tidy           # CI fails if go.mod/go.sum aren't tidy — run and commit any diff
```

Linting needs [`golangci-lint`](https://golangci-lint.run/welcome/install/) on
your PATH.

The install story is the same one end users follow — see
[README.md](README.md#install).

## Architectural-law markers

Decisions in this codebase are cited inline against the architectural laws they
serve — `// [LAW:single-enforcer] ...`, `// [LAW:no-silent-failure] ...`. The
markers are the codebase's machine-greppable record of *why* a seam is shaped
the way it is. Every token you cite must be a canonical one: the single in-repo
authority is [`internal/lawtokens`](internal/lawtokens), and a gate
(`go test ./internal/lawtokens/`, which runs as part of `go test ./...`) fails
loudly naming any marker whose token is absent from that set. Don't invent a
token to make a comment read well — if a citation fails the gate, fix the token;
adding a genuinely new law means adding it to `lawtokens.Canonical` first.

## Issue tracking — this repo uses `lit`

Work is tracked with `lit`, not GitHub Issues. After cloning and building, run:

```sh
lit quickstart      # prints the live command reference and the agent loop
```

Pull the next ready ticket (`lit ready`), claim it (`lit start <id>`), and mark
it done when complete (`lit done <id>`). If you're pointing an AI agent at the
repo, hand it [docs/agent-setup.md](docs/agent-setup.md).

## Branch & PR conventions

- Branch off `master` and keep your branch up to date with `git pull --rebase`.
- **One PR per epic**, not per leaf ticket: all children of an epic land on a
  single branch/PR.
- Open a PR against `master` — don't push directly to it.
- Keep the suite green (`go test ./...`) and the linter clean
  (`golangci-lint run`) before requesting review.
