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
| `github.com/dolthub/dolt/go` | [dolthub/dolt](https://github.com/dolthub/dolt) | [promptctl/dolt](https://github.com/promptctl/dolt) | `lit` | `v0.40.5-0.20260816040811-3eabc076e073` |
| `github.com/dolthub/go-mysql-server` | [dolthub/go-mysql-server](https://github.com/dolthub/go-mysql-server) | [promptctl/go-mysql-server](https://github.com/promptctl/go-mysql-server) | `lit` | `v0.20.1-0.20260816040904-aabd9c24450f` |
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

No `lit` source file imports `golang-lru`. Its importers are exactly the two
modules forked here, which is why both are forked and not just one — measured
with `go list -deps ./cmd/lit`: `github.com/dolthub/go-mysql-server/sql` is the
only importer of v1, from the single file `sql/cache.go`, and v2 comes from
several Dolt packages (`store/nbs`, `libraries/doltcore/doltdb`,
`libraries/doltcore/remotestorage`). The only way to stop importing a module is
to change the files that import it, and the only way to change those files is to
own a fork of the module they live in. The removals themselves are tracked
separately under the `links-licensing-c0ce` epic.

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

### promptctl/go-mysql-server

**No patches yet.** The fork was created before the work that needs it, because a
patch has to have somewhere to land before it can be written. Branch `lit` is
upstream `0986a7fcf0fe` plus a `README.lit-fork.md` that says so.

The first patch will remove `github.com/hashicorp/golang-lru` from `sql/cache.go`
— that one file is the sole reason an MPL-2.0 module is linked into `lit`.

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

One consequence worth knowing before you read a report: because `go.mod` still
`require`s the upstream coordinate, `tools/licenses` attributes these modules to
`github.com/dolthub/...` while reading their license text out of a
`promptctl/...` directory. That is not a mistake being hidden — `Module.ReplacedBy`
exists to carry the discrepancy, and the report prints it.

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
file: both pin tables, and any patch whose retirement condition the rebase just
met.

## Proving a rebase did not restore a copyleft import

```sh
go run ./tools/licenses -check
```

This classifies every module linked into `./cmd/lit` and exits non-zero on any
license that is neither in `tools/licenses/policy.json`'s `allowed_licenses` nor
carried by a reviewed entry in its `module_exceptions`. A rebase that reinstates
an import the fork had removed brings that module's license back into the linked
set, and the gate fails on it. You do not have to remember to run it: the same
policy is asserted by `TestDependencyLicensesArePermitted`, which `go test ./...`
picks up on every merge, and `-check` itself runs again in `release-validate`.

What that proves today is narrower than it sounds: nothing outside the
committed policy entered the linked set, and no more. It does not yet prove the absence of
copyleft, because `policy.json` still allows MPL-2.0 and still carries two
reviewed exceptions. Emptying it is the last ticket of the `links-licensing-c0ce`
epic; until then, read a green `-check` as "no new license slipped in," not as
"no copyleft is linked."

Two things `-check` deliberately does not cover. It reads only the *linked* set,
so a copyleft module that sits in the module graph without being linked passes —
`go run ./tools/licenses -graph` is the wider audit, and it reports rather than
gates. And it reads Go modules only; the statically linked native C libraries are
classified separately in `tools/licenses/native.go`.

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
