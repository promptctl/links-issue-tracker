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
pre-commit install  # one-time per clone: install the commit hooks (see below)
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

Inner-loop runtime is enforced, not just observed: CI's PR gate pipes the test
run through [`tools/testbudget`](tools/testbudget/main.go), which fails the
build when any package exceeds its wall-clock budget, naming the package and
the overage. The budgets — and the rules for when a number may move — live in
[`tools/testbudget/budgets.go`](tools/testbudget/budgets.go); read that file
before raising one. To check locally:

```sh
(set -o pipefail; go test -short -json ./... | go run ./tools/testbudget)
```

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
the way it is. Every token you cite must be a canonical one. The token index is
owned by the [universal-laws code skill](https://github.com/promptctl/laws/blob/master/plugins/laws/skills/code/SKILL.md),
and [`internal/lawtokens`](internal/lawtokens) holds a copy generated from it,
`canonical_gen.go`. A gate (`go test ./internal/lawtokens/`, which runs as part
of `go test ./...`) fails loudly naming any marker whose token is absent from
that copy. The same check runs on your staged files at commit time once you
have installed the hooks in [`.pre-commit-config.yaml`](.pre-commit-config.yaml)
with [pre-commit](https://pre-commit.com) (`pre-commit install`). If your git
config sets `core.hooksPath`, pre-commit refuses to install; a hooks directory
there that runs the repository's `.git/hooks/pre-commit` can host it, installed
with `GIT_CONFIG_GLOBAL=/dev/null pre-commit install`. Don't invent a token to
make a comment read well. When a citation
fails the gate, look the token up in the upstream index. If it is there, the
copy is behind: run `just lawtokens-sync` and commit the regenerated file. If it
is not there, fix the token. Never edit `canonical_gen.go` by hand. The nightly
workflow runs the same tool with `-check` and fails when the copy and the
upstream index differ.

## Documented messages

`doc-v1-total/` is a specification of what this binary does, so where a chapter
quotes a user-facing message it holds a second copy of a string whose original
is a Go literal. Two copies of one fact drift. Here they drifted in silence:
three merged tickets each falsified a documented claim and every CI check
stayed green over all three, because nothing compared the two.

[`internal/docclaims`](internal/docclaims) is that comparison.
`manifest_gen.go` records every message literal the specification quotes that
was present in the shipped Go when it was generated — each entry carrying the
whole literal it was found inside, so deleting that exact message is reported
rather than passing silently when an unrelated string happens to contain the
same words. An entry anchored to an embedded asset is held only to "still
somewhere in that file", which is the weaker of the two holds; `Claim.Src`
says why, and `links-doc-v1-tepa` closes it. A gate
(`go test ./internal/docclaims/`, which runs as part of `go test ./...`) fails
naming the chapter and the sentence, in both directions: a quotation the
manifest records that the tree no longer yields, and a quotation the tree
yields that the manifest does not record. It is one test printing one report,
because when there were two they twice came to tell a contributor opposite
things about a single entry in a single run. `go run ./tools/docclaims-sync
-check` is the same comparison as a command.

What counts as shipped is whatever a binary under `cmd/` actually links, walked
out from each `main` package through the import graph, minus test files. Not a
list of directories: two successive lists both leaked. The first swallowed
`artifacts/`, a gitignored vendored copy of an unrelated project whose literals
outnumbered lit's own by three to one; the `cmd/` and `internal/` list that
replaced it still admitted `internal/vendor/dolthub-driver/example`, a program
nothing imports, and `internal/docsclaims`, a registry of quotations from other
documents. Neither was harmless noise — a quotation anchors to the *shortest*
source holding it, so a stray copy in unlinked code becomes the evidence for a
chapter's claim and survives deleting the real message. Three entries were being
held up that way, including two chapters' `CREATE TABLE` anchored to an example
table in the vendored driver.

Vendored is not the disqualifier; unreachable is. `github.com/dolthub/driver` is
`replace`d onto `internal/vendor/dolthub-driver` and imported by `internal/store`,
so its documented error messages ship and are gated like any other.

Embedded text assets count too, resolved from the `//go:embed` directives
themselves rather than guessed from file extensions. They have to: this
repository is moving user-facing text out of Go literals and into embedded
files — `links-help-h0di` moved whole help pages into
`internal/cli/helptext/` — so a gate reading only literals would go blind in
exactly the direction the corpus is travelling. `lit quickstart doctor` is
quoted by two chapters and exists only in
`internal/templates/defaults/quickstart.md`.

An entry keeps the source it was anchored to for as long as that source still
ships and still carries the quotation. Re-anchoring on every run would let any
newly added shorter literal retarget unrelated entries, failing the freshness
test on a branch that changed no documented message — wording indistinguishable
from real drift, which is what trains people to regenerate without reading.

When you deliberately change a message the specification quotes, the gate
fails on purpose. Correct the prose first, then run
`go run ./tools/docclaims-sync` and commit the regenerated manifest. Never edit
`manifest_gen.go` by hand. The diff it produces is the review signal — an entry
leaving the manifest is a sentence that stopped describing the binary — so
regenerating without reading what left is the one use that defeats the gate.
Read the entries that *moved*, too: a re-anchored entry does not leave the
manifest, only its recorded source changes, and that change is the tool judging
a reworded literal to be the same message. It prints each one it made.

A failure names which of three things happened, because the remedy differs and
one of them is destroyed by regenerating.

A chapter that stopped quoting a message is fixed by regenerating — and that
includes the ordinary deliberate change, where you delete a message from the
code *and* remove the sentence that quoted it. The report tells you **not** to
regenerate in one case only: a message that nothing ships any more is still
quoted somewhere. There, regenerating drops the entry and leaves that sentence
describing a binary which does not have it. *Somewhere*, not *in the chapter the
entry names*: move a sentence to another chapter in the same change that deletes
the message it quotes, and the entry's own chapter has indeed stopped quoting it
while the assertion is alive and false in its new home.

Where the recorded source no longer carries the quotation but some other
shipped source does, the report names that source — it cannot tell a reworded
message from an unrelated string that happens to share the words, and that is
the one case only a reader can settle. `-check` stops there. The writer does
not stop, because rewording a literal around a quotation is an ordinary edit and
refusing it would put the scary warning on the common path; it names every entry
it re-anchors and writes, which is what makes that judgement reviewable in the
manifest diff instead of invisible in it.

The writer enforces the order this section asks for. `go run
./tools/docclaims-sync` refuses to write while a chapter still quotes a message
that stopped shipping, naming each one, so the regeneration that would erase
that evidence cannot happen in passing on the way to fixing something else.

The check is one-directional and narrow on purpose. Every literal the manifest
records must still ship; no chapter is ever required to quote any particular
string, so prose that never quoted code needs no allowlist and adds no upkeep.
Literals are all it reads: whether `file.go:12-33` still brackets the
declaration its sentence names is a different question over a different corpus,
answered by [`internal/doccites`](internal/doccites) above, and whether a
chapter's claim about a type's shape
or a command's exit code still holds is a third that no gate here answers yet.
A green run means the quoted messages still exist, and nothing more.

## Cited line numbers

**A citation names the symbol it is about.** Write ``internal/model/model.go``
(`UpdatedAt`), and keep the line span if it helps a reader jump. The symbol is
the anchor; the numbers are a convenience that decays.

The reason is that the numbers decay silently. Any edit above a cited line
redraws the code and leaves the citation untouched, so a reader who follows a
stale one lands in unrelated code and reads it as the thing the sentence
described — worse than no citation, because it is followed with confidence.
A symbol survives every edit that does not rename it, and a rename is a compile
error somewhere. Naming one is also what makes a citation checkable:
over half the existing corpus can be checked no further than that its lines
exist, because nothing beside those citations names a symbol in the file they
point at.

This is not a forecast. Measured over `doc-v1-total/` when the gate below was
written, of 9,813 citations:

| | share | |
|---|---|---|
| resolve to the symbol their sentence names | 25.2% | 2,470 |
| point somewhere else, or at a missing file or past its end | 20.9% | 2,050 |
| name no symbol in the cited file, so nothing can judge them | 53.9% | 5,293 |

Among the 3,909 whose sentence names a symbol the cited file declares — the
ones a checker can judge either way — **36.8% are wrong**, and every CI check
was green over all of them. Re-derive any of it with `go run
./tools/doccites`; `-list` prints each citation that does not resolve.

### The gate

[`internal/doccites`](internal/doccites) resolves each citation against the
code and holds the corpus to what already worked. `manifest_gen.go` records
every citation that resolved to the symbol its sentence names when it was
generated, and a gate (`go test ./internal/doccites/`, which runs as part of
`go test ./...`) fails naming each entry that stopped resolving and where the
symbol went.

It is baselined rather than absolute, because a corpus already wrong a third of
the time cannot be held to "every citation resolves": that fails on the first
run and is switched off within a week. It is held to "every citation that
resolved still resolves" — the bleeding, not the wound. Repairing the existing
2,050 is separate work, and deliberately not done by a sweep: a citation that
was already wrong gets re-derived into a fresh wrong number, producing a large
diff that reads like a repair while fixing nothing.

When you move cited code, repoint the citations your change broke and run `go
run ./tools/doccites -sync`. The diff is the review signal, exactly as
docclaims' is: an entry leaving the manifest is a sentence that stopped
pointing at its subject, so regenerating without reading what left is the one
use that defeats the gate.

### What it reads, and what it does not

Four spellings occur, and the parser resolves all of them to a path before
anything judges them — a qualified path, a bare basename inheriting its
directory from the prose, a bare continuation (`` `:47-54` ``) inheriting the
whole path, and further spans after a comma. Just 20.6% carry a path of their own;
a reader anchored on ``file.go:`` sees a fraction of the corpus and reports
success over the rest, which four separate sweeps have now done, the last by an
author who had the blind spot written down.

The gate does not judge whether a citation is *right*. That depends on the
claim its sentence is making, which no rule over line numbers can see: a span
of pure comments looks structurally impossible and is correct when the prose
cites a comment for stating a deliberation. It answers the decidable question —
the cited lines either contain the symbol's declaration or use it, or they have
nothing to do with it. Whether a quoted message still ships is
[`internal/docclaims`](internal/docclaims); whether a chapter names identifiers
that exist at all is `links-doc-v1-mujn`.

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
- Keep the suite green (`go test -short ./...` is what the PR gate runs;
  `just test` for the full lane) and the linter clean
  (`golangci-lint run`) before requesting review.

## Cutting a release

A release ships once per **epic**, not per ticket. Ticket PRs accumulate their
notes under `## [Unreleased]` in `CHANGELOG.md` and cut nothing on merge; no
ticket PR, whatever its type, cuts a release on its own. When the epic's tickets
are all merged, cut the release with a dedicated `chore(release)` PR that promotes
the changelog version.

See [RELEASING.md](RELEASING.md) for the promotion steps, the CI mechanism, and
the versioning policy — it is the single home for the release procedure.
