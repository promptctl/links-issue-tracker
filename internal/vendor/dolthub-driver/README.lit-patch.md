# Vendored, patched copy of github.com/dolthub/driver

This directory is a full copy of `github.com/dolthub/driver` at the exact
version this project depends on
(`v0.2.1-0.20260314000741-0fe74e7ee31a`, from `$GOMODCACHE`), wired in via the
`replace` directive in the repo's top-level `go.mod`. It is not vendored in
the `go mod vendor` sense — it's a normal, independent Go module (own
`go.mod`/`go.sum`, same module path) that just happens to live in this repo
instead of on a remote host, so the patch travels with the PR that needed it.

## Why this exists

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

## Refreshing this copy

When bumping the `dolthub/driver` version pinned in the top-level `go.mod`:

1. Fetch the new version into the module cache (`go mod download
   github.com/dolthub/driver@<version>` from the repo root, with the
   `replace` directive temporarily removed).
2. Diff `$(go env GOMODCACHE)/github.com/dolthub/driver@<version>` against
   this directory to see what upstream changed.
3. Re-apply this same patch (delete `emitUsageEvent` and its supporting
   machinery in `connector.go`, and the `go emitUsageEvent(...)` call in
   `driver.go`) against the new version, copying the rest of upstream's
   changes through untouched.
4. Update the `replace` directive's version comment in the top-level
   `go.mod` to match.

Drop this whole directory and the `replace` directive if upstream ever ships
a released version with this telemetry removed or reliably disable-able
before process start.
