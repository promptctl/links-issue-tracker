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
- **Node.js (`npx`)** — *only for agents using the browser.* The repo's
  [`.mcp.json`](.mcp.json) registers `chrome-devtools-mcp` as a project MCP
  server, launched with `npx`. It is pinned to an exact version, so upgrading it
  is a reviewed diff, and it runs with `--isolated` so parallel agent sessions
  each get a throwaway Chrome profile instead of fighting over one.

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

## Performance envelope

Size performance work to **5x the largest `lit` backlog on this device**, and
find that number the cheap way — `lit stores --counts` lists every store this
machine knows with its ready/in-flight/blocked totals. Never go looking for
stores with a filesystem sweep. At the time of writing there was one store at
118 workable rows, so the design target is ~590.

The envelope exists to make "is this fast enough?" a measurement rather than an
argument, and it cuts both ways: do not optimize for a million rows that will
never exist, and do not conclude a quadratic pass is fine because 46 rows felt
instant. `internal/cli/workable_bench_test.go` benchmarks the workable gather at
both sizes; extend it rather than reasoning about cost in a PR description.

Two instruments measure that envelope, and they answer different questions.
`go test -bench` measures a stage *in process*, with the store already open, so
it isolates the cost of one pass over the rows. `just perf` measures what a user
actually waits for: it builds the binary from your working tree, generates
stores at 0 / 118 / 590 rows with `lit init` and `lit import`, and times every
user-facing command as a fresh process against each one, reporting each store's
bytes on disk alongside.

```sh
just perf                        # the whole table
just perf --sizes 0,118          # pick the store sizes
just perf --keep /tmp/litperf    # leave the generated stores behind to inspect
```

`--keep` needs a directory that holds no stores yet, and says so if it does not.
That is deliberate rather than fussy: `lit init` is idempotent and `lit import`
appends, so generating a second time into the same directory would produce a
store of twice the rows wearing the row count of the first — a wrong number
with nothing on screen to mark it. Comparing two runs means two paths.

Reach for `just perf` whenever a claim is about latency a user feels or about
store size, and for `go test -bench` when it is about the cost of a pass. The
end-to-end figures are the ones that caught what the microbenchmarks
structurally cannot see: process start plus store open is ~0.14s, which on a
590-row store is most of what `lit backlog` spends.

Read the table the way it is built. Each probe runs once unrecorded as a
warm-up, then five times round-robin, and the reported figure is the
**minimum**, with the maximum in parentheses: benchmark noise on a developer
machine is one-sided, so the mean measures the machine's load and the min
measures the code — and this checkout is routinely shared by several agent
sessions, one of which compiling Go during a round would otherwise land in the
number. The warm-up is what makes the max readable: it absorbs the cold start
(page cache, dynamic linking, and on the empty store the first write's inline
maintenance), so a max near the min means a quiet machine and a max several
times the min means the run fought for CPU and only the min survived it. Two
full runs minutes apart reproduced 24 of 27 latency cells to within 0.01s,
worst case 18%. Store bytes come from a single generated store and vary about
2–3% run to run, so a couple of percent is not a regression.

The measured `lit` runs under a scrubbed environment — your global lit config,
ejected templates, `LIT_*` variables and `CLAUDE_CODE_SESSION_ID` are all out
of scope — so two machines time the same code path and a difference in the
table is a difference in the code. Larger `--sizes` are fine: the import
budget grows with the row count (590 rows import in about 22s here).

The scale epic's headline figures live in that command and nowhere else,
because the hand-measured version rotted in under a month: `lit backlog` was
quoted at 7.97s on 2026-08-25 and measured 0.54s on 2026-09-21, while the store
grew from 188 MB to 279 MB. Quote `just perf` output with its date, and
regenerate rather than copying an old number forward.

One rule is absolute: **anything that adds milliseconds PER ROW is banned.**
Fixed costs are fine and batched calls are fine — a reader that loads a level of
the graph in one query costs the same at 10 rows and 600. A per-row store round
trip does not, so a batch reader should expose no singular accessor at all,
leaving the expensive shape unwritable rather than merely discouraged.

## Fix or file — never trade one defect for another

**A broken window is never an excuse to break another window just to get some
fresh air.** An existing defect is not a budget you may spend on new damage; it
is either in scope, and you fix it, or out of scope, and you file it.

Two shapes this takes here, both seductive because they sound like focus:

- *Two surfaces disagree, so I will make them agree on whichever answer is
  cheaper to reach.* Unify on the **correct** answer and pay for it. Deleting the
  richer of two answers closes the duplication and loses the facts.
- *My change broke something next door, but the ticket was about this.* Breakage
  your own change caused is in scope whatever the ticket says. That includes
  cited line numbers: see [Cited line numbers](#cited-line-numbers) for how to
  tell which citations you own.

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
declaration its sentence names is a different question over a different corpus
that no gate here answers, and whether a chapter's claim about a type's shape
or a command's exit code still holds is a third that no gate here answers.
A green run means the quoted messages still exist, and nothing more.

## Documented names

A chapter that names a Go function sends a reader to find it, and the
specification cites files, never lines, so the name is the only anchor the
reader has. A name that exists nowhere reads as a rename to chase, and when the
function never existed there is nothing to find.
[`internal/docnames`](internal/docnames) checks every Go-shaped code span in
`doc-v1-total/` against the identifiers in the code of the source this
repository carries, tests and tools included; `go test ./internal/docnames/`
runs it as part of `go test ./...`. A name surviving only in a comment or a
string does not count as present, and a name qualified by one of this tree's
packages (`storage.IssueOrdering`) must be declared in that package, as a
top-level name or a method. A member written on an exported type this tree
declares (`Store.Close`) must be that type's field or method, its own or
promoted by embedding.

A span is judged when its callee is dot-joined identifiers with a mixed-case
segment; the mixed-case identifiers among a call's arguments are judged too,
so `wsCmd(runUpgrade)` fails when `runUpgrade` is gone. `*T`, `[]T` and
`(*T).M` are read as the names they are built on. Lowercase and all-caps words
are left alone, because in these chapters they are columns, statuses,
commands, SQL and environment variables. A failure names the chapter line; the
fix is to name what the code does now, not the nearest-looking identifier, and
an argument written as a placeholder gets the code's own name for it.

Some names are rightly not in the tree: standard-library and third-party
symbols, agent-harness names, units, format patterns, file names, and code
quoted verbatim where a variable shares a package's name. List those by
name in
[`doc-v1-total/names-outside-the-tree.txt`](doc-v1-total/names-outside-the-tree.txt),
one per line with the reason. The gate also reports an entry no chapter writes
any more, or one the tree now has, so the list cannot outlive what it excuses.

A green run means every named identifier exists somewhere, tests included. It
does not mean the name is the production code a sentence describes: the
`run*` adapters in `internal/cli/leaf_adapters_test.go` keep old handler names
alive for tests, and a chapter naming one passes. Nor does it mean the sentence
describes the identifier correctly.

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
