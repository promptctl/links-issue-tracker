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
  installs in
  [`.github/actions/install-dolt/action.yml`](.github/actions/install-dolt/action.yml)).

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
just setup       # one-time per machine: native deps + persist cgo paths (macOS)
just build       # build the lit binary
just test-short  # the inner loop: full suite minus the generated-scale tests
just test        # run the full suite (needs the dolt CLI; see above) — args pass through
just lint        # golangci-lint against .golangci.yml
just install     # build from source and install onto your PATH
```

The equivalent raw commands (these need the cgo paths already on your env — i.e.
after `just setup`, or run via `just`):

```sh
go build ./cmd/lit    # build the lit binary
./scripts/install.sh  # build and install onto your PATH (wires cgo paths itself)
go test -short ./...          # the inner loop (needs the dolt CLI; see above)
go test -timeout 30m ./...    # the full suite, generated-scale tests included
golangci-lint run     # lint against .golangci.yml before opening a PR
go mod tidy           # CI fails if go.mod/go.sum aren't tidy — run and commit any diff
```

The suite has two lanes. The **inner loop** — `go test -short ./...` — is what
you run while working and what CI's PR gate runs; `-short` skips only the tests
whose cost comes from deliberately generated scale (today that is exactly one:
a sync-reconcile combine folding 500 commits over a 1000-issue backlog — 119s
at the 2026-08-24 measurement on the development machine; the test logs its
own runtime, so a full-lane run's output is the current number). The **full lane** — `go test -timeout 30m ./...`, since the suite's
real work brushes go test's 10-minute per-package default — runs everything,
and a scheduled job
([`.github/workflows/nightly.yml`](.github/workflows/nightly.yml)) runs it
nightly, filing a `nightly-failure` issue when it breaks so the skipped tests
stay covered rather than quietly rotting. A test belongs behind
`testing.Short()` only when its cost is the scale it generates, not the
behavior it pins — never move a test there to make a slow test disappear.

Linting needs [`golangci-lint`](https://golangci-lint.run/welcome/install/) on
your PATH.

One group of tests is opt-out of `go test ./...` and says so when it skips: the
whole-module-graph license audit in `tools/licenses`. Running it means
`go mod download all`, which fetches every module the build does not need —
several gigabytes against a cold cache — so it is gated behind an environment
variable rather than charged to every run:

```sh
LIT_LICENSE_GRAPH_AUDIT=1 go test ./tools/licenses/
```

Run it when you touch `tools/licenses` or change a dependency; CI does not.

The install story is the same one end users follow — see
[README.md](README.md#install).

## Forked dependencies

`lit` does not build against upstream Dolt or go-mysql-server. Both resolve,
through `replace` directives in [`go.mod`](go.mod), to forks owned by the
`promptctl` organization — removing a copyleft-licensed transitive dependency
means changing what its importer imports, and you can only do that from inside a
fork. Before bumping either pin, rebasing a fork, or wondering why a `replace` is
there at all, read **[FORKS.md](FORKS.md)**: the ledger of what each fork
patches and what would retire each patch, the Apache-2.0 notices those patches
oblige, the rebase procedure, and the check that proves a rebase did not restore
a copyleft import.

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

Pull the next ready ticket (`lit next`), claim it (`lit start <id>`), and mark
it done when complete (`lit done <id>`). If you're pointing an AI agent at the
repo, hand it [docs/agent-setup.md](docs/agent-setup.md).

## Branch & PR conventions

- Branch off `master` and keep your branch up to date with `git pull --rebase`.
- **One PR per ticket**, so each change is reviewed on its own. The children of
  an epic land as separate PRs; the release for the whole epic is a final,
  dedicated `chore(release)` PR (see *Cutting a release*).
- Open a PR against `master` — don't push directly to it.
- Keep the suite green (`go test ./...`) and the linter clean
  (`golangci-lint run`) before requesting review.

## Cutting a release

A release ships once per **epic**, not per ticket. Ticket PRs accumulate their
notes under `## [Unreleased]` in `CHANGELOG.md` and cut nothing on merge; no
ticket PR, whatever its type, cuts a release on its own. When the epic's tickets
are all merged, cut the release with a dedicated `chore(release)` PR that promotes
the changelog version.

See [RELEASING.md](RELEASING.md) for the promotion steps, the CI mechanism, and
the versioning policy — it is the single home for the release procedure.
