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
| `github.com/dolthub/dolt/go` | [dolthub/dolt](https://github.com/dolthub/dolt) | [promptctl/dolt](https://github.com/promptctl/dolt) | `lit` | `v0.40.5-0.20261019140221-16ea880f8fde` |
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

#### Patch 6 — make `emitStats` shutdown tolerate an already-gone stats consumer

`go/store/datas/pull/puller.go`, plus
`go/store/datas/pull/puller_emit_stats_shutdown_test.go` (a new file, not
upstream's).

`Puller.Pull` starts `emitStats`, whose sender goroutine writes progress to an
unbuffered channel, and defers a cancel that `wg.Wait`s for it. The consumer
(`pullerProgFunc` in `dolt_pull.go`, shared by DOLT_PUSH) is torn down by its
own context, which nothing orders relative to emitStats's `done` signal. When
the caller's context is canceled mid-push, the consumer exits first and the
sender's plain sends block forever — the periodic ticker send pins the
goroutine before it can see `done`, and the final post-`done` flush pins it
during shutdown — so `Pull` never returns and the caller's database engine
stays held. `lit` hit this the moment it bounded the background mirror's
engine hold with a context deadline (`links-sync-pgct.11.1`): the deadline
killed the hung push's git subprocess, and the shutdown deadlock then held the
engine anyway. The patch makes the ticker send also select on `done` and the
final flush best-effort (`select`/`default`), and the new test pins that
`cancel()` returns with no consumer draining.

**Retire it when** upstream fixes the emitStats shutdown ordering (no upstream
PR exists yet) and the `require` line moves past it. Retiring the patch does
not retire the fork.

#### Patch 7 — close what fetch and pull open, and keep one last-known ref per remote

`go/libraries/doltcore/sqle/dprocedures/dolt_fetch.go`,
`go/libraries/doltcore/sqle/dprocedures/dolt_pull.go`,
`go/store/blobstore/git_blobstore.go`, `go/store/blobstore/git_refs.go`, plus
`go/store/blobstore/git_blobstore_last_known_ref_test.go` (a new file, not
upstream's).

`DOLT_FETCH` and `DOLT_PULL` open their remote through
`GetRemoteDBWithoutCaching`, whose contract makes closing it the caller's job,
and neither closed it. A git-backed remote's blobstore deletes the two refs it
names with its own UUID only in `Close`, so every fetch and pull left a
`refs/dolt/remotes/<remote>/dolt/data/<uuid>` ref in the store's
`git-remote-cache` mirror, and each one pinned a whole superseded snapshot of
the remote against `git gc`. `lit`'s own store had 2,049 of them and a 256 MB
pack against 93 MB for the newest snapshot (`links-storage-bd8w.qif`). Both
procedures now close the database they open.

Closing alone would have traded the leak for a download. The UUID refs were
the only refs the mirror held — `DOLT_PUSH`'s cached instance already deleted
its own at engine close — so a mirror with them gone negotiates its next fetch
with no haves and receives the whole snapshot again. `Close` therefore first
publishes its tracking ref to `refs/dolt/last-known/<remote>/dolt/data`, the
one ref a mirror keeps between operations, and a successful lease push moves
the tracking ref to the pushed commit, as `git push` does, so a writer
publishes what it pushed rather than what it fetched before pushing. The
last-known ref sits outside `refs/dolt/remotes/` because git cannot hold a ref
at a path other refs sit beneath.

**Retire it when** upstream closes the remote database in both procedures and
keeps a durable negotiation ref in the git blobstore, and the `require` line
moves past that change. Retiring the patch does not retire the fork. A rebase
that drops only the publish while keeping the closes is the download
regression above, not a cleanup.

#### Patch 8 — export the git remote cache key

`go/libraries/doltcore/dbfactory/git_remote.go` and its test.

`GitRemoteFactory` keeps each git remote's mirror at
`.dolt/git-remote-cache/<sha256(url|ref)>`, and the derivation was
unexported. `lit` prunes the mirrors no configured remote maps to, so it had
to know each remote's directory name and re-derived it by hand, with a
different URL parser and a fixed ref, and had to refuse to prune whenever its
copy might have drifted. `GitRemoteCacheKey(url, params)` parses, picks the
factory and hashes through the same helpers `CreateDB` uses, so the name
`lit` computes is the name Dolt writes.

**Retire it when** upstream exports the cache directory name (or a prune of
its own) and the `require` line moves past that change. Dropping it without
that leaves `internal/store/remotecache.go` with no way to recognize a live
mirror.

#### Patch 9 — `DOLT_PUSH --force-with-lease`, a compare-and-swap on the pushed branch

`go/store/datas/database.go`, `go/store/datas/database_common.go`,
`go/libraries/doltcore/doltdb/doltdb.go`, `go/libraries/doltcore/ref/ref.go`,
`go/libraries/doltcore/env/remotes.go`,
`go/libraries/doltcore/env/actions/remotes.go`,
`go/libraries/doltcore/sqle/dprocedures/dolt_push.go`,
`go/cmd/dolt/cli/flags.go`, `go/cmd/dolt/cli/arg_parser_helpers.go`, plus
`go/store/datas/database_lease_test.go` and
`go/libraries/doltcore/env/push_lease_test.go` (new files, not upstream's).

A forced push overwrote whatever the remote branch named: the root update
behind it retries an optimistic-lock failure against the fresh root, so a
peer's push that landed after the pusher last read the remote was lost.
Publishing a re-rooted history (`links-storage-3k6u`) has to refuse instead.
`--force-with-lease=<branch>:<commit>` checks the branch's head inside the
root update, on every retry, and refuses with `datas.ErrStaleInfo` unless it
is `<commit>` or already the pushed commit — the second so that a push
retried after its first attempt landed does not report its own write as
stale. The SQL boundary renders the error as text, so
`internal/store/push_lease.go` recognizes a stale lease by the sentinel's
own message.

**Retire it when** upstream ships a conditional push (git's
`--force-with-lease` or an equivalent compare-and-swap) and the `require`
line moves past that change. Dropping it without that turns every leased
push in `internal/store` into an error, and a bare `--force` in its place
would overwrite a peer's push.

#### Patch 10 — re-check against a new GC generation, and replace a store's table files under one

`go/store/nbs/store.go`, plus `go/store/nbs/gc_generation_test.go` (a new
file, not upstream's).

A writer adding table files reads its view of the store's GC generation, and
retried a manifest whose generation had changed against that same view, so it
found the same mismatch every time: a writer whose view predated another
process collecting the store spun until its context ended, and without a
deadline never returned. It now rebases onto the manifest before the retry,
whose ref check then runs against the files the store holds, so a chunk
referencing one the new generation dropped fails with
`ErrTableFileNotFound`. `nbs.ReplaceStoreTableFiles` points a store's manifest
at exactly the given table files, keeping its root and starting a new GC
generation, in one manifest write; a refusal while the manifest has moved past
the view is `ErrConcurrentManifestWriteDuringOverwrite`.
`internal/store/remote_gc.go` uses it to run a git remote's GC after a
re-root (`links-storage-3k6u`): the remote never collects itself, and a force
push only adds table files.

**Retire it when** upstream rebases on a GC-generation mismatch and offers a
way to replace a remote's table set under a new generation, and the `require`
line moves past that change. Dropping it without that leaves the shared
remote holding every write ever pushed, and the bare
`OverwriteStoreManifest` in its place keeps the generation, so a writer
holding the old one could reference a chunk the replacement dropped.

#### Patch 11 — the engine owns every remote a push opens, and a clone closes its source

`go/libraries/doltcore/sqle/database_provider.go`.

`DOLT_PUSH` asks the provider for a cached remote. The provider cached only
`git+` remotes, in its own map, closed with the engine; every other remote
was opened through dbfactory's process-wide singleton map, which only
`CloseAllLocalDatabases` closes and `lit` never calls. A `file://` remote's
table files therefore stayed open until the process exited, and on Windows
nothing under that remote's directory could be removed: 79 `internal/store`
tests still held a remote's files when their TempDir was removed
(`links-windows-qr8l.7na`). The provider now caches every remote it hands
out, opened past the singleton map, so its `Close` releases them all and the
git-only branch is gone. `DOLT_CLONE`'s source is opened the same way and
closed when the clone returns, as patch 7 closes fetch's and pull's — for a
git-backed source that also retires the blobstore's UUID refs.
`internal/store/remote_handles_test.go` fails without the patch.

**Retire it when** upstream closes the remote a push or a clone opens without
routing it through the singleton map, and the `require` line moves past that
change. Dropping it without that brings back every pushed or cloned remote
held open for the life of the process.

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
`promptctl/…` directory. Each component therefore keeps its upstream identity —
name, version and purl — in every artifact, and **declares the substitution
alongside it**. That split is deliberate: the identity answers *what does `lit`
depend on*, which is what `go.mod` says and what every advisory feed matches a
purl against, while the substitution answers *what did `lit` actually compile*.
Renaming the component to the fork would have collapsed the two questions into
one wrong answer and orphaned the coordinate every vulnerability scanner keys on.

Where each artifact says it, as of `links-licensing-c0ce.15` — verified
2026-08-22 by generating all three:

- **`SBOM.cdx.json`** — the component carries a CycloneDX `pedigree`. For the
  two forks that is a `pedigree.descendants` entry naming
  `github.com/promptctl/…` with its own version and a resolvable purl, plus
  `pedigree.notes` saying in words that the compiled code came from that fork
  and not from the component's own purl. The vendored driver is the other shape:
  `replace github.com/dolthub/driver => ./internal/vendor/dolthub-driver` has no
  published coordinate, so it gets notes naming the directory and stating why no
  component is recorded beside it — an unexplained empty list reads as an
  omission, which is the opposite of the truth.

  The fork goes under `descendants`, **not** `ancestors`, and the difference is
  a true statement against a false one. CycloneDX defines descendants as the
  forks of a component, which `promptctl/dolt` is with respect to
  `dolthub/dolt`. Filing it under `ancestors` — "the component this one was
  derived from" — would assert that `dolthub/dolt` derives from our fork,
  reversing the real genealogy inside a structured field that machines read
  without the notes beside it.
- **`LICENSE-REPORT.md`** — a `Source` column on every row. It reads `-` for
  every component built from its own named source, and names the substitute
  otherwise. Three of the rows name a substitute today. (Deliberately not "the
  module the first column names": four rows are native C libraries whose first
  column is a bare name like `icu`, naming no module at all.)
- **`THIRD_PARTY_LICENSES`** — a `Source:` line directly under the module header
  of each substituted section. This is the file that legally accompanies the
  binary and it has no index, so a recipient has nothing but the section itself
  to consult.

`go run ./tools/licenses -graph` still prints the same fact under `MODULES WHOSE
SOURCE COMES FROM SOMEWHERE OTHER THAN THE COORDINATE NAMING THEM`; it is no
longer the only place.

The license those artifacts report was always correct — both forks are
Apache-2.0 at both ends, so no row was ever wrong. What was missing, and is no
longer, was the fact that the source is patched.

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
identified.** Both halves took work to earn, and the first rests on an empty
array rather than on a rule. `module_exceptions` is empty (patch 4 removed the
last one, fslock's, and `TestLoadPolicyEmbedded` pins it staying empty), so no
linked module is excused by anything — and note that an exception is the one
accept shape carrying no permissiveness rule at all, deliberately, since naming
a license the allowlist refuses is the only reason to write one. fslock's
exception named an LGPL. So re-opening that array does not merely add a row: it
weakens this paragraph, which is a second reason it is the owner's call.
`allowed_licenses` names
ten permissive licenses and nothing else — nine that linked components
actually carry, plus `Apache-2.0 WITH LLVM-exception`, which nothing carries
bare and which is there only because compiler-rt's expression names it as an
arm. `links-licensing-c0ce.9` deleted MPL-2.0, WTFPL and ISC once measurement
showed nothing linked carried any of them, so there is no longer a copyleft
license sitting in the list for the gate to wave through — and one cannot be
added quietly, since the parse refuses an entry the license classifier types
as restricted, reciprocal or forbidden. And the
classifier's `Unknown` verdict is a hard failure with no route around it: not an
allowlist entry, not an exception. A policy naming it is refused at the parse,
and every ruling refuses it again regardless of what the policy says, so the
guarantee does not depend on the file having been read through the parse. A
module whose license nobody can read fails this gate,
which is the row it would be easiest to talk past.

Read it as narrowly as it is written, though, on both axes.

`-check` rules on each linked module's own license grant — the file a
coordinate scanner reports. A copyleft text sitting deeper inside a linked
module's tree is a different question, and `-graph` below is where it gets
answered.

And it rules on the linked set of **the platform it runs on**. `go list -deps`
resolves a platform-dependent set, so "every module linked into the binary" is
a claim about *a* binary: linux/amd64 links 154 components where darwin links
153 (`github.com/klauspost/cpuid/v2`). Both were checked for this change, and
CI runs Linux, which is the platform the release artifacts are built for — but
a rebase that reinstates an import behind a build tag for a third platform is
not covered by a green run on either of these two.

To check another platform, build the tool natively and then set the target on
the run — and set `CGO_ENABLED` with it:

```sh
go build -o /tmp/lic ./tools/licenses
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 /tmp/lic -check -pkg ./cmd/lit
```

Both halves matter. `GOOS=linux go run ./tools/licenses` cross-*builds* the
tool and dies with `exec format error`, because the tool takes its target from
the environment rather than a flag. And cross-compiling defaults `CGO_ENABLED`
to 0, which resolves a module set that is neither platform's — it drops
`github.com/dolthub/go-icu-regex`, which darwin links, and adds
`github.com/klauspost/cpuid/v2`, which darwin does not — and then prints a
green line with a component count that happens to match darwin's exactly. A
verification step whose whole purpose is catching a reinstated import must not
have a mode that quietly gates the wrong set.

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
