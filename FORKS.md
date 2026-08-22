# Forks

`lit` builds Dolt and go-mysql-server from forks owned by the
[promptctl](https://github.com/promptctl) organization, not from upstream, wired
in by `replace` directives in [`go.mod`](go.mod). This file is the contract for
those forks: what is patched, why, what would retire each patch, how to move a
fork onto a newer upstream, and the check that proves a move did not quietly
drag a copyleft-licensed dependency back into the binary.

Forks exist because `lit` needs to change what Dolt *imports*, and you cannot do
that from outside Dolt. They are permanent. Individual patches are not — each
one below carries the condition that would let it be deleted.

## What `lit` forks

| Module in `go.mod` | Upstream | What `lit` builds | Branch | Current pin |
| --- | --- | --- | --- | --- |
| `github.com/dolthub/dolt/go` | [dolthub/dolt](https://github.com/dolthub/dolt) | [promptctl/dolt](https://github.com/promptctl/dolt) | `lit` | `v0.40.5-0.20260821231005-4b80eac34485` |
| `github.com/dolthub/go-mysql-server` | [dolthub/go-mysql-server](https://github.com/dolthub/go-mysql-server) | [promptctl/go-mysql-server](https://github.com/promptctl/go-mysql-server) | `lit` | `v0.20.1-0.20260821032251-ab5cb9ec3b69` |
| `github.com/dolthub/driver` | [dolthub/driver](https://github.com/dolthub/driver) | [`internal/vendor/dolthub-driver`](internal/vendor/dolthub-driver) | — | `v0.2.1-0.20260314000741-0fe74e7ee31a` |

The driver is the odd one out: a patched copy that lives in this repository
rather than on GitHub, so that a patch travels in the same pull request as the
change that needed it. Its ledger is
[`internal/vendor/dolthub-driver/README.lit-patch.md`](internal/vendor/dolthub-driver/README.lit-patch.md)
and is not repeated here.

## The invariant that makes this file checkable

**Each fork branch is based on exactly the upstream commit that `go.mod`'s
`require` line names for that module.** A `replace` directive substitutes the
source without touching the `require` line, so `go.mod` keeps stating the
upstream coordinate `lit` diverged from:

| Module | `require` names | Fork branch is that commit, plus the patches below |
| --- | --- | --- |
| `github.com/dolthub/dolt/go` | `v0.40.5-0.20260314011441-62975ef6bf36` | `62975ef6bf36` |
| `github.com/dolthub/go-mysql-server` | `v0.20.1-0.20260313230549-0986a7fcf0fe` | `0986a7fcf0fe` |

So "what did `lit` change?" is not a claim this file makes — it is a command
anyone can run:

```sh
git clone https://github.com/promptctl/dolt.git && cd dolt
git diff 62975ef6bf36..lit
```

Keep the invariant when you rebase, or that command stops answering the question
and this file becomes the only record — which is the state it exists to prevent.

Every version and commit quoted in this file's tables and shell snippets is
checked against `go.mod` on each `go test ./...`, in both directions:
`tools/licenses/forks_test.go` fails if a pin moves without the prose following,
and fails if the prose names a pin the build no longer uses. Prose is exempt
deliberately, so read a version in a sentence as illustration and a version in a
table as fact.

## Why forks, and not a `replace` pointed at a rewrite

Because a `replace` directive cannot remove a dependency, and removing
dependencies is the point.

The Go tool reports a replaced module under its **original** path and version,
with only the replacement's *directory* changed. Point
`github.com/hashicorp/golang-lru` at permissively licensed code and the `require`
line still says `golang-lru`, the SBOM entry is still
`pkg:golang/github.com/hashicorp/golang-lru@v2.0.7`, and every scanner that
resolves that coordinate still returns MPL-2.0 — while our own SBOM now asserts a
license the ecosystem disagrees with. The coordinate is what an auditor sees, and
a coordinate leaves `go.mod` only when nothing imports it any more.

No `lit` source file ever imported `golang-lru`. Its importers were exactly the
two modules forked here, which is why both are forked and not just one — measured
with `go list -deps ./cmd/lit` before the removal: `github.com/dolthub/go-mysql-server/sql`
was the only importer of v1, from the single file `sql/cache.go`, and v2 came from
several Dolt packages (`store/nbs`, `libraries/doltcore/doltdb`,
`libraries/doltcore/remotestorage`). The only way to stop importing a module is
to change the files that import it, and the only way to change those files is to
own a fork of the module they live in. Patches 1 (go-mysql-server) and 2 (dolt)
below are those removals, landed under `links-licensing-c0ce.5`; the replacement
they wire in is `github.com/promptctl/primitives` (MIT, clean-room provenance
recorded in that module's `PROVENANCE.md`).

## Patch ledger

### promptctl/dolt

#### Patch 1 — serve git-backed ranged reads from a materialized file

`go/store/blobstore/git_blobstore.go`, plus
`go/store/blobstore/git_blobstore_materialize_ranges_test.go` (a new file, not
upstream's).

Upstream's `GitBlobstore.getFromCache` served a ranged read by streaming the
whole blob through `git cat-file` and discarding every byte before the requested
offset. NBS reads a table file as many small ranged reads — footer, index, then
chunk by chunk — and footer reads use negative offsets, so each read re-spawned
`git` and re-inflated almost the entire blob from byte zero. A large fetch or
pull over a git-backed remote cost O(reads × blobsize): tens of gigabytes of
redundant zlib and thousands of subprocesses for a single ~18 MB table file.

The patch materializes each blob to a local seekable file once, keyed by its
content-addressed OID, and serves every range from it with one seek. Full reads
still stream directly, since a sequential pass needs no random access.

**Retire it when** [dolthub/dolt#11264](https://github.com/dolthub/dolt/pull/11264)
merges and the `require` line moves past it. Tracked as `promptctl-deps-4aes`.
Retiring the patch does not retire the fork.

#### Patch 2 — replace `hashicorp/golang-lru/v2` with `promptctl/primitives/lru`

Seven files under `go/`: `libraries/doltcore/remotestorage/map_chunk_cache.go`,
`libraries/doltcore/doltdb/doltdb.go`, `store/nbs/store.go`,
`store/nbs/archive_reader.go`, `store/nbs/archive_build.go`,
`store/nbs/table_set.go`, `store/nbs/table_set_test.go`.

Those were this module's only importers of `github.com/hashicorp/golang-lru/v2`
(MPL-2.0) — six linked files and one test file. The replacement carries the same
generic API, so the patch is the import line plus each file's `NOTICE` comment —
no call-site edits.

The same commit range adds a `replace` in the fork's own `go.mod` resolving
`github.com/dolthub/go-mysql-server` to the promptctl fork. Inside `lit`'s build
it is ignored (a non-main module's replaces always are); it exists so the fork
built standalone also links the fork, and so the upstream go-mysql-server
coordinate cannot re-record `golang-lru` as an indirect require of this module.
It must move whenever the go-mysql-server pin above moves.

**Retire it never** — this patch is the fork's reason to exist. It ends only if
upstream itself drops the MPL-2.0 dependency, at which point the whole ledger
entry collapses into a rebase.

#### Patch 3 — remove the dead rolling-hash splitter and the nbs benchmark harness

Deletes `go/store/prolly/tree/node_splitter.go`'s `rollingHashSplitter` (type,
constructor, constants, and pattern function), its benchmark in
`node_splitter_test.go`, the whole `go/store/nbs/benchmarks/` tree, and the
deleted files' rows in `go/utils/copyrightshdrs/main.go`'s expected-file table.
Nothing is added: this patch is a cut, not a substitution.

Both buzhash coordinates leave the module with it. `rollingHashSplitter` was
the sole importer of `github.com/kch42/buzhash` and was reachable only from
its own benchmark — `defaultSplitterFactory` has only ever been
`newKeySplitter`, which hashes keys against a Weibull-modelled threshold and
never touches buzhash. The benchmarks tree was a hand-run harness nothing in
the module imports, and its `gen` package was the sole importer of
`github.com/silvasur/buzhash` — the same 2016 code republished under the
author's renamed account, at the identical pseudo-version timestamp. Both
coordinates classify as Unknown (a hand-written, profane WTFPL variant no
classifier matches), which is the row the licensing epic treats as the worst
kind. No chunk boundary moves: nothing on the write path changed, verified
twice against the pins on both sides of this patch — a 200k-tuple prolly tree
built under each yields the identical root hash and identical 2,257 chunk
addresses, and enumerating every chunk of a real pre-existing lit workspace
store (56,933 across newgen and oldgen) under each is byte-identical in
address and size (both runs recorded on `links-licensing-c0ce.6`).

**Retire it when** upstream itself deletes the dead rolling-hash path or drops
buzhash, and the `require` line moves past that change. Until then, a rebase
that revives `rollingHashSplitter` on a live path is a storage-format decision,
not a conflict to resolve mechanically: stop and escalate.

#### Patch 4 — replace `dolthub/fslock` with `promptctl/primitives/filelock`

Ten files: `libraries/doltcore/dbfactory/git_remote.go`,
`libraries/events/event_flush.go`, `libraries/utils/filesys/lock.go`,
`store/blobstore/git_blobstore.go`, `store/blobstore/local.go`,
`store/nbs/file_manifest.go`, `store/nbs/journal.go`,
`store/nbs/journal_record.go`, `store/nbs/test/manifest_clobber.go`, and
`cmd/dolt/commands/engine/lock_release_test.go` — this module's only
importers of `github.com/dolthub/fslock` (LGPL-3.0 with a static-linking
exception), the one genuinely copyleft coordinate the `lit` binary still
linked. The replacement's `Lock` handle carries the same surface (`New`,
`Lock`, `TryLock`, `LockWithTimeout`, `Unlock`, and the `ErrLocked` /
`ErrTimeout` sentinels, compared by identity where upstream compares them so),
and the import is aliased to `fslock`, so the patch is the import line plus
each file's `NOTICE` comment — no call-site edits. The handle's contract was
derived from the nine non-test call sites and from black-box tests — the
test-only importer was swapped but was not derivation input — and never from
fslock's source; the provenance record is the `links-licensing-c0ce.4`
section of the primitives module's `PROVENANCE-ATTESTATIONS.md`.

Landed under `links-licensing-c0ce.4`, which also removed the fslock
exception from `tools/licenses/policy.json` — a rebase that restores the
import therefore fails the license gate, not just this ledger.

**Retire it never** — same standing as patch 2: it ends only if upstream
itself drops the LGPL dependency, at which point the ledger entry collapses
into a rebase.

#### Patch 5 — cut the plot-rendering test code that put `gonum.org/v1/plot` in go.mod

`go/store/prolly/tree/samples_test.go` loses `plotIntHistogram` and
`plotNodeSizeDistribution` — this module's only importers of
`gonum.org/v1/plot`, whose requirement graph carries
`github.com/golang/freetype` (GPL-2.0 dual FTL), `github.com/ajstarks/svgo`
(CC-BY-4.0), and four go-fonts font modules (OFL-1.1 twice, one GUST
license, and DejaVu's unclassifiable Bitstream-derived terms). The `Samples`
statistics and the text summaries stay, and the permanently skipped
`TestKeySplitterDistribution` harness in `node_splitter_test.go` now prints
summaries instead of rendering PNGs. `go mod tidy` drops the whole plot stack
from the fork's go.mod, and `lit`'s go.sum with it loses every source hash
(`h1:`) for those modules — two coordinates (`git.sr.ht/~sbinet/gg`,
`github.com/go-pdf/fpdf`) leave both go.sums entirely, and what remains of
the rest is go.mod-graph bookkeeping, explained in
[LICENSE-NOTES.md](LICENSE-NOTES.md).

**Retire it when** upstream deletes its plot dependency (it serves one
hand-run benchmark plot) and the `require` line moves past that change. A
rebase that revives the plot import restores GPL to the fork's manifest, so
treat a conflict here as a licensing decision, not a mechanical resolution.

### promptctl/go-mysql-server

#### Patch 1 — replace `hashicorp/golang-lru` with `promptctl/primitives/lruany`

`sql/cache.go`, the module's sole importer of `github.com/hashicorp/golang-lru`
(MPL-2.0) and the single file that put a second MPL coordinate into the `lit`
binary. The replacement carries the same method set, so the patch is the import
line, the `NOTICE` comment, and one deliberate behavioral change: both
`lru.New` call sites discarded the constructor's error, which now surfaces as a
panic at construction — at the call that owns the bad size — instead of a
failure at first use, far from its cause.

**Retire it never** — same standing as dolt's patch 2 above.

## Apache-2.0 obligations

Both upstreams are Apache-2.0, and forking them puts `lit` under obligations it
did not previously owe. Section 4 of that license asks for four things when you
distribute a derivative work:

- **4(a), a copy of the license.** Each fork keeps upstream's `LICENSE` byte for
  byte. Release builds additionally bundle it into `THIRD_PARTY_LICENSES`, which
  `tools/licenses` generates from the linked module set.
- **4(b), prominent notice on modified files.** Every file a fork changes carries
  a `NOTICE` comment directly under its upstream copyright header, naming the
  modification and pointing back at this ledger. Files this fork *adds* carry the
  same pointer, so a reader can tell fork code from upstream code without a diff.
- **4(c), retained attribution.** Upstream copyright headers are never edited,
  only appended to.
- **4(d), the NOTICE file.** **Neither upstream ships one.** Verified on
  2026-08-15 against the repository roots of `dolthub/dolt` and
  `dolthub/go-mysql-server`: both carry `LICENSE` and no `NOTICE`. There is
  therefore nothing to propagate. If a future rebase pulls in a `NOTICE` file,
  4(d) starts applying and it must travel with our distribution.

One consequence to know before you read a generated artifact: because `go.mod`
still `require`s the upstream coordinate, `tools/licenses` attributes these
modules to `github.com/dolthub/…` while reading their license text out of a
`promptctl/…` directory. `LICENSE-REPORT.md`, `THIRD_PARTY_LICENSES` and the
CycloneDX SBOM all name the upstream coordinate and say nothing about the
substitution — verified 2026-08-15 by generating all three. Only `go run
./tools/licenses -graph` prints it, under the heading `MODULES WHOSE SOURCE COMES
FROM A DIFFERENT COORDINATE`.

The license those artifacts report is correct: both forks are Apache-2.0 at both
ends, so no row is wrong. What is missing is the fact that the source is patched,
which a reader has to come here or run the graph audit to learn. CycloneDX has a
`pedigree` field for exactly this; wiring it up is tracked as
`links-licensing-c0ce.15`.

## Rebasing a fork onto a newer upstream

Do this in the fork, not here. Dolt is the example; go-mysql-server is identical
with its own URLs.

```sh
git clone git@github.com:promptctl/dolt.git && cd dolt
git remote add upstream https://github.com/dolthub/dolt.git
git fetch upstream
git checkout lit
git rebase <new-upstream-commit>          # the commit you intend to pin
git push --force-with-lease origin lit
```

When you resolve conflicts, keep the `NOTICE` comments — they are a license obligation,
not commentary, and a conflict resolution that takes upstream's side of a header
silently drops one. Then, back in this repository:

```sh
go mod edit -replace=github.com/dolthub/dolt/go=github.com/promptctl/dolt/go@<new-fork-sha>
go mod tidy
```

`go mod tidy` rewrites the raw SHA into a pseudo-version. Finally, update this
file: both pin tables, the `git diff` snippet, and any patch whose retirement
condition the rebase just met. `TestForkLedgerQuotesEveryCurrentPin` fails until
you do, so this is not a step you can forget.

**Watch the go-mysql-server line while you rebase Dolt.** `lit` requires that
module only *indirectly*, and the `replace` names no old version, so the two can
come apart without anyone editing go.mod: a rebased Dolt that wants a newer
go-mysql-server makes `go mod tidy` raise the require line on its own, while the
`replace` goes on substituting fork source built from the commit the fork branch
is really based on. go.mod would then name an upstream commit the fork has never
seen, and the `git diff` above would return upstream churn tangled with lit's
patches. The same test catches it. The fix is to rebase the fork onto the commit
go.mod now names — not to edit the table to match.

## Proving a rebase did not restore a copyleft import

```sh
go run ./tools/licenses -check
```

This classifies every module linked into `./cmd/lit` and exits non-zero on any
license outside `tools/licenses/policy.json`'s `allowed_licenses`. A rebase that
reinstates an import the fork had removed brings that module's license back into
the linked set, and the gate fails on it. You do not have to remember to run it:
the same policy is asserted by `TestDependencyLicensesArePermitted`, which
`go test ./...` picks up on every merge, and `-check` itself runs again in
`release-validate`.

A green `-check` now says the whole thing it appears to say: **every module
linked into the binary is under a permissive license, and every one of them was
identified.** Both halves took work to earn. `module_exceptions` is empty (patch
4 removed the last one, fslock's, and `TestLoadPolicyEmbedded` pins it staying
empty), so no linked module is excused by anything. `allowed_licenses` names
nine permissive licenses and nothing else — `links-licensing-c0ce.9` deleted
MPL-2.0, WTFPL and ISC once measurement showed nothing linked carried any of
them, so there is no longer a copyleft license sitting in the list for the gate
to wave through. And the
classifier's `Unknown` verdict is a hard failure with no route around it: not an
allowlist entry, not an exception, refused at the parse and dropped again when
the filter is built. A module whose license nobody can read fails this gate,
which is the row it would be easiest to talk past.

Read it as narrowly as it is written, though: `-check` rules on each linked
module's own license grant, the file a coordinate scanner reports. A copyleft
text sitting deeper inside a linked module's tree is a different question, and
`-graph` below is where it gets answered.

What `-check` does *not* cover is anything outside the linked set: a copyleft
module that sits in the module graph without being linked passes. `go run
./tools/licenses -graph` is the wider audit over everything `go list -m all`
resolves, and it reports rather than gates; every non-permissive row it finds
is explained in [LICENSE-NOTES.md](LICENSE-NOTES.md).

It does cover more than Go modules, which is easy to assume otherwise: the four
native C libraries cgo static-links (ICU, zstd, musl, compiler-rt) are invisible
to `go list -deps`, so `tools/licenses/native.go` carries them as a curated
inventory and `-check` gates them alongside the modules. The component count it
reports is that combined set — linked modules plus the four native libraries —
so it moves whenever a dependency enters or leaves the link closure.

## Verifying a fork change end to end

```sh
just build && just test          # cgo; on macOS needs keg-only icu4c and zstd
go run ./tools/licenses -check
grep -n "dolthub/dolt/go\|dolthub/go-mysql-server" go.mod
```

The last command should show the `require` lines still naming upstream and the
`replace` lines naming `promptctl` — that pairing is the invariant at the top of
this file, and it is what keeps `git diff <require-commit>..lit` a complete
answer.
