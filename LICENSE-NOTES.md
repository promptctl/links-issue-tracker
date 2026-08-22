# License notes for the dependency graph

`lit` is MIT, and everything linked into the shipped binary is gated against
[`tools/licenses/policy.json`](tools/licenses/policy.json) on every test run
and release. A license scanner pointed at this *repository*, though, reads
more than the binary: it resolves every coordinate in `go.mod` and `go.sum`,
and some of what it finds there looks alarming. This file is the written
answer for each of those findings — what the row is, how it gets into the
graph, and why it places no obligation on `lit` or its users. The generated
counterpart is `go run ./tools/licenses -graph`, which classifies every
module in the build list on demand; this file explains the rows that report
cannot explain by itself.

One distinction carries most of this document. A module in the build-and-test
closure — its packages imported by something `lit` builds, or by the *test
files* of a dependency in that closure — gets a full source hash in `go.sum`
(an `h1:` line); the narrower set actually compiled and linked into the
binary is what the `-check` gate covers. A module that is merely *named* in
some other module's `go.mod` gets only a `<version>/go.mod` hash line — Go
records it to make version selection reproducible, but never fetches its
source for a build. The second kind is bookkeeping, not a dependency: no
code from it is compiled, linked, tested, or shipped. Each section below
names which shape its module takes — and whatever the shape, none of them
places a line of the flagged code in the shipped binary.

## The GPL row: `github.com/golang/freetype`

The one GPL text that is part of a module's *own license grant* is
freetype's `licenses/gpl.txt` — the module is dual-licensed, FreeType
License or GPL-2.0. (GPL text appears in two other places, both data rather
than grants: one copy in libc's vendored test corpus, and several — every
GPL and LGPL version — in the license classifier's reference corpus, each
explained in its own section below.) Nothing in `lit` or its
forks imports it, and no freetype source carries an `h1:` hash in either
`go.sum`. It is named in the graph by a chain of dev-dependency metadata in
modules that predate Go 1.17's graph pruning: Dolt needs
`HdrHistogram/hdrhistogram-go` (MIT) and `apache/arrow/go/arrow`
(Apache-2.0); their go.mod files name old versions of `gonum.org/v1/gonum`
(BSD-3-Clause); those old gonum go.mods name `gonum.org/v1/plot`; and plot's
go.mod names freetype. Because those modules predate pruning, Go loads their
entire requirement graphs when computing versions, so the names propagate
into `go.sum` as `/go.mod` lines. No cut in code we control can remove them —
the edges live in third-party go.mod files — and they carry no code anywhere.

The same chain explains every font and graphics row: `go-fonts/liberation`
and `go-fonts/stix` (OFL-1.1), `go-fonts/latin-modern` (GUST font license),
`go-fonts/dejavu`, `ajstarks/svgo`, `go-latex/latex`, and the `gofpdf`
modules. Font licenses restrict selling the *font*, not software, and none of
these modules contributes a byte to any build here. Dolt's actual import of
the plot library — one hand-run benchmark chart — is removed in the fork
(patch 5 in [FORKS.md](FORKS.md)), which is why none of this stack has source
hashes any more.

## The MPL rows: `go-sql-driver/mysql`, `hashicorp/go-uuid`, `hashicorp/golang-lru`

Nothing `lit` builds or ships links either module; `lit`'s own former use of
the mysql driver was removed outright. The mysql coordinate stays visible in
two ways. Seven upstream modules (goose, the AWS SDK, gocloud, and others)
name it in their go.mod files, which keeps `/go.mod` bookkeeping lines in
`go.sum`. Separately, the *test suites* of Dolt and go-mysql-server use the
driver to connect to real servers — those tests are inside packages `lit`
imports, so Go also records the driver's source hash for `go test`
completeness, though no `lit` build or test executes them.
`hashicorp/go-uuid` is quieter still: a `/go.mod` bookkeeping line only, no
source hash, named by `colinmarc/hdfs` (Dolt's HDFS blobstore support) —
another pre-pruning module whose whole requirement list Go records.
`hashicorp/golang-lru` — whose linked use both forks removed outright — has
the same shape: two `/go.mod`-only lines (v0.5.0 and v0.5.1), named by
old `google.golang.org/api` and `go.opencensus.io` go.mods from the same
pre-pruning era, with no source hash anywhere. MPL-2.0
is file-level copyleft triggered by distributing the covered files; `lit`
distributes none of them. MPL-2.0 is currently in the policy allowlist;
whether the gate should also assert anything about unlinked graph rows is the
gate ticket's decision (`links-licensing-c0ce.9`).

## `modernc.org/libc`: a GPL file inside test data

Tree-walking scanners flag
`modernc.org/libc/testdata/nsz.repo.hu/libc-test/src/math/crlibm/COPYING`,
which really is the GPL-2.0 text. It sits inside a vendored copy of the
`libc-test` conformance corpus — test *inputs* the module's author uses to
validate their libc port. The module's own license, at its root, is
BSD-2-Clause. Unlike the plot-stack rows, this module does carry a full
source hash in `go.sum` — `lit` links goose, and goose's *own test suite*
imports `modernc.org/sqlite`, which imports libc, so Go records the source
hash for test completeness. No libc package is in `lit`'s link closure and
none is compiled by any `lit` build or test; the corpus itself is not
compiled into anything, and a license on test data imposes nothing on
software that never ships or runs it.

## `opencontainers/go-digest`: CC-BY-SA on the docs, not the code

This module's root holds two license files: `LICENSE` (Apache-2.0, covering
the code) and `LICENSE.docs` (CC-BY-SA-4.0, covering its documentation).
Share-alike on documentation binds someone republishing those docs, which
`lit` does not do — and the module is the faintest kind of presence there
is: it appears in neither `go.mod` nor either `go.sum`, and only the
`-graph` audit's `go list -m all` resolves it, through goose's own go.mod
requirement. Never imported, never linked, never fetched by a build.

## `google/licenseclassifier`: the detector's own corpus

The largest block of alarming rows — roughly 140 nested texts including
AGPL-3.0, every GPL and LGPL version, and assorted non-commercial licenses —
is the `licenses/` directory of `github.com/google/licenseclassifier`: one
reference copy of each license the classifier can recognize. It is required
by *this repository's own compliance tooling* (`tools/licenses`), which is
how a license auditor ends up reporting the auditor. Reference texts are data
to that tool, not license grants, and the module itself is Apache-2.0.

## Rows the classifier cannot name

The graph report's "could not identify" section lists files no SPDX license
matches confidently. They fall into three shapes, none a hidden obligation:
pointer files that just name the real choice elsewhere (freetype's root
LICENSE, svgo's link file), public-domain dedications with no SPDX identity
(`modernc.org/sqlite`'s SQLITE-LICENSE), and fixtures or scripts that merely
look license-shaped (a mock-generator test fixture, a check-license tool
script). The classifier corpus contributes another thirty-one header
fragments. Each is worth reading once; that reading has been done, and none
changed a verdict above.

## `Zlib` and `BSL-1.0`

Both appear only because `policy.json`'s allowlist does not name them —
`go-gl/glfw` vendors its C library's Zlib license, and gonum carries a Boost
license in a third-party directory. Both are uncontested permissive licenses;
adding them to the allowlist is a policy decision that belongs to the gate
ticket, not a dependency problem.

## Modules with no license file

Three modules (`chzyer/logex`, `dolthub/sqllogictest/go`, `gobwas/pool`)
ship no license text at all, so no attribution can be generated from their
source. None is linked into any build. Two are the `/go.mod`-only
bookkeeping kind described at the top (`chzyer/logex`, `gobwas/pool`);
`dolthub/sqllogictest/go` is fainter still — like go-digest above, it
appears in neither `go.mod` nor either `go.sum` and only the `-graph`
audit's `go list -m all` resolves it.
