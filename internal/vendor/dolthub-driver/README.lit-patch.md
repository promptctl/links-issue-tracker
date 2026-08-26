# Vendored, patched copy of github.com/dolthub/driver

This directory is a full copy of `github.com/dolthub/driver` at the exact
version this project depends on
(`v0.2.1-0.20260314000741-0fe74e7ee31a`, from `$GOMODCACHE`), wired in via the
`replace` directive in the repo's top-level `go.mod`. It is not vendored in
the `go mod vendor` sense — it's a normal, independent Go module (own
`go.mod`/`go.sum`, same module path) that just happens to live in this repo
instead of on a remote host, so the patch travels with the PR that needed it.

## Why this exists

This copy carries **five** independent patches. When refreshing (below), all
must be re-applied.

### Patch 1 — telemetry removal (`connector.go`, `driver.go`)

Upstream's `connector.go` fired an unconditional goroutine
(`emitUsageEvent`) on every embedded-engine open that dialed
`eventsapi.dolthub.com` over gRPC to report anonymous usage. It was gated by
an env var (`DOLT_METRICS_DISABLED`) read exactly once in a package-level
`init()` — which, by Go's own initialization-order rules, always runs before
any code in an importing package (like `lit`'s `internal/store`) gets a
chance to set that env var. There is no reliable way for an embedding
process to opt out of it from inside the same binary; it can only be set in
the OS environment before the process starts, which `lit` cannot assume of
every invocation.

`lit` is a local issue tracker backed by an embedded Dolt store. It has no
product reason to report usage to a third party, so this patch removes the
emission path entirely — the goroutine, the env-var opt-out, the
once-per-24h rate-limit file, and every import that existed solely to serve
them — rather than defaulting it off. See `connector.go` for the exact diff
(search for `[LAW:effects-at-boundaries]`) and `driver.go` for the removed
call site.

### Patch 2 — surface first-row query errors (`statement.go`, `rows.go`)

Upstream's `doltStmt.Query` (`statement.go`) pre-reads the first row via
`peekableRowIter.Peek()` and discards its error (`row, _ := peekIter.Peek(...)`).
`Peek()` only buffers a row on success (`rows.go`), so a non-EOF error from the
first `Next()` is never recorded. The application's later `Next()` then calls the
underlying iterator a *second* time — and for a non-idempotent iterator (e.g.
`DOLT_MERGE_BASE` on refs with no common ancestor, which raises
`Error 1105: no common ancestor` on its first row) that second call returns
`io.EOF`, silently converting a real query error into an empty result set
(`sql.ErrNoRows` to the caller).

The patch carries the peek error on `doltRows.err` (translated, with `io.EOF`
left as a nil error since it is just an empty result set) and returns it from the
top of `doltRows.Next()` before re-driving the iterator, so the first-row error
is surfaced instead of swallowed. Search for `peekErr` in `statement.go` and the
`rows.err` guard at the top of `doltRows.Next()` in `rows.go`.
`peek_error_test.go` pins the contract. See lit ticket
`promptctl-dolt-driver-iip`.

### Patch 3 — working engine-open knobs (`config.go`, `connector.go`, `driver.go`)

Upstream keys both of dolt's embedded-open load params —
`DisableSingletonCacheParam` (fresh store per open, real close on engine
close) and `FailOnJournalLockTimeoutParam` (fail fast with
`nbs.ErrDatabaseLocked` instead of silently opening read-only) — on a single
condition: `Config.BackOff != nil`. lit needs them decoupled: every lit open
must bypass the singleton cache (so a Store's close actually releases the
journal manifest lock instead of pinning it until process exit), but only
*write-capable* opens want fail-fast + retry — read opens keep the read-only
fallback, which is their contract. The patch adds two explicit `Config`
fields, `DisableSingletonCache` and `FailOnJournalLockTimeout`, each mapped
to its load param independently; a non-nil `BackOff` still implies both, so
upstream behavior is unchanged. `config_load_params_test.go` pins the
mapping.

The patch also makes the params actually reach the databases (`driver.go`,
`openSqlEngine`), which upstream's plumbing never did: the params were
threaded into envs only inside `engine.NewSqlEngine`, *after*
`MultiEnvForDirectory` had loaded (or arranged lazy loads for) the databases
with default semantics. `openSqlEngine` now passes a params-carrier `DoltEnv`
into the env load (dolt clones `DBLoadParams` from it into every env it
creates, before those envs load), and then forces each env's lazy database
load and returns its `DBLoadError` as the open error — without that check,
`CollectDBs` dereferences a nil `DoltDB` and panics on exactly the
`ErrDatabaseLocked` failure the retry path exists to catch. See lit ticket
`links-sync-pgct.11`.

### Patch 4 — own the error type; drop go-sql-driver/mysql (`errors.go`, `mysql_error.go`, `errors_test.go`, `smoke_test.go`, `gorm_test.go` deleted)

Upstream's `translateError` (`errors.go`) built a
`github.com/go-sql-driver/mysql` `*MySQLError`, and lit's
`internal/store/sync_schema_guard.go` asserted that same type to read MySQL
error 1146 ("table doesn't exist") — the pre-goose "no `goose_db_version`
table → schema 0" path. So an **MPL-2.0** module was a permanent row in lit's
SBOM in order to carry two fields across a package boundary between two
modules lit already owns, on both ends.

`mysql_error.go` defines this driver's own `MySQLError` — a server error code
and a message, the two fields the MySQL protocol's ERR packet gives a client a
reason to branch on, and exactly the two our producer sets and our consumer
reads. It was written from the protocol and from those call sites, **not** from
go-sql-driver's source, per `design-docs/clean-room-reimplementation.md`: this
epic's default is rewrite, and a shape partly dictated by a wire protocol is
still not ours to copy out of an MPL-2.0 file. It carries no SQL state, because
nothing here produces one and nothing in lit reads one.

`Error()` renders `Error <code>: <message>`. That is not a free choice — it is
this driver's already-tested contract: seven assertions in `smoke_test.go`
(e.g. `"Error 1146: table not found: doesnotexist"`) pin it, and they pass
unchanged.

Two test-only removals were needed, because a test-only requirement is still a
line in `go.mod` and still a row an auditor reads:

- **`smoke_test.go`** carried upstream's MySQL-comparison mode — a hardcoded
  `var runTestsAgainstMySQL = false` plus `mysqlDsn`, seven
  `if !runTestsAgainstMySQL { … } else { … }` sites, and a connection swap to
  `sql.Open("mysql", …)`. Enabling it required editing the source *and* running
  a MySQL server on `localhost:3306`, which this repo never provisions, so the
  path was dead in every run. The mode is gone and the `else` arms with it;
  every test remains and still runs against the Dolt driver.
  [LAW:dataflow-not-control-flow] — a compile-time toggle gating whether
  assertions run is a mode, and it is now one fixed path.
- **`gorm_test.go` was deleted.** It was the *sole* remaining path to the
  MPL-2.0 coordinate (`dolthub/driver.test → gorm.io/driver/mysql →
  go-sql-driver/mysql`), and it tested GORM ORM compatibility — which upstream
  Dolthub cares about and **lit does not use anywhere**. Keeping it would have
  cost one MPL-2.0 row plus both `gorm.io` modules in lit's module graph to
  protect an integration lit never exercises.

On refresh, expect upstream to still have all of this. Re-apply the rewrite and
re-drop both test dependencies; do not "restore" them because the diff looks
lossy. See lit ticket `links-licensing-c0ce.2`.

### Patch 5 — serialize engine construction (`driver.go`)

Constructing a SQL engine writes package-level state in the dolt stack:
`go-mysql-server`'s `InitStatusVariables` (called from `gms.New`, inside
`engine.NewSqlEngine`) rewrites the process-global status-variable table,
and `NewSqlEngine` re-points the global binlog-consumer singleton
(`doltBinlogConsumer.SetEngine`). Two engines constructed concurrently in
one process therefore data-race on those globals even when their databases
live at unrelated paths — `go test -race` on lit's parallelized
`internal/store` suite caught exactly this (145 warnings, all at
construction; none during query execution).

The patch holds a package-level mutex (`engineConstructMu` in `driver.go`)
around the `engine.NewSqlEngine` call, the one funnel every engine
construction in this driver passes through. Construction is serialized;
opened engines run concurrently exactly as before. This protects any lit
path that opens multiple stores in one process (the cross-project rollup,
parallel tests).

## Refreshing this copy

When bumping the `dolthub/driver` version pinned in the top-level `go.mod`:

1. Fetch the new version into the module cache (`go mod download
   github.com/dolthub/driver@<version>` from the repo root, with the
   `replace` directive temporarily removed).
2. Diff `$(go env GOMODCACHE)/github.com/dolthub/driver@<version>` against
   this directory to see what upstream changed.
3. Re-apply **all three** patches against the new version, copying the rest
   of upstream's changes through untouched:
   - Patch 1: delete `emitUsageEvent` and its supporting machinery in
     `connector.go`, and the `go emitUsageEvent(...)` call in `driver.go`.
   - Patch 2: re-apply the first-row error propagation in `statement.go`
     (capture `Peek()`'s error into `doltRows.err`, mapping `io.EOF` to nil)
     and the `rows.err` guard at the top of `doltRows.Next()` in `rows.go`.
     Run `peek_error_test.go` to confirm it holds.
   - Patch 3: re-add the `DisableSingletonCache` / `FailOnJournalLockTimeout`
     fields to `Config` and their independent mapping to `DBLoadParams` in
     `connector.go` (`BackOff != nil` still implies both), and re-apply
     `driver.go`'s `openSqlEngine` changes: the params-carrier `DoltEnv`
     passed into the env load, and the forced lazy-load check that surfaces
     `DBLoadError` instead of letting `CollectDBs` panic on a nil `DoltDB`.
     Run `config_load_params_test.go` to confirm the mapping holds, and lit's
     `internal/store` engine_open_contract_test.go for the end-to-end
     behavior.
   - Patch 4: re-add `mysql_error.go`, point `translateError` at this module's
     own `*MySQLError`, and re-drop the go-sql-driver/mysql references from
     `errors_test.go` and `smoke_test.go` (including upstream's
     `runTestsAgainstMySQL` mode) and delete `gorm_test.go` again. Confirm with
     `go mod tidy && grep -rn "go-sql-driver" --include="*.go" --include=go.mod .`
     returning no import or requirement, and that lit's
     `internal/store` still matches error 1146 via `isMissingTableError`.
4. Update the `replace` directive's version comment in the top-level
   `go.mod` to match.

Drop this whole directory and the `replace` directive if upstream ever ships
a released version with this telemetry removed or reliably disable-able
before process start.
