# Platform: startup, configuration, discovery, and release

`lit` is a single Go binary that runs inside a git checkout. Everything it persists lives under the repository's git common directory, so a workspace is discovered from the current working directory by asking git, never by a config file naming a path. This chapter covers the process lifecycle (startup, signals, exit codes), every environment variable, the two-layer configuration system, workspace and multi-checkout discovery, the per-checkout stream identity, version identity, the release/self-install machinery, and the build/CI tooling that produces and gates releases.

## Module identity and dependencies

Module `github.com/promptctl/links-issue-tracker`, Go `1.25.7` (`go.mod:1-3`); CI derives its Go version from `go.mod`. Direct dependencies include the Dolt SQL engine stack, cobra/pflag/viper, goose (migrations), google/uuid, cenkalti/backoff, CycloneDX + licenseclassifier + packageurl (license tooling), and `promptctl/primitives` (`go.mod:5-24`). Three `replace` directives redirect the Dolt stack (`go.mod:186-196`): `dolthub/dolt/go` and `dolthub/go-mysql-server` point at `promptctl` forks, and `dolthub/driver` points at the in-repo copy `./internal/vendor/dolthub-driver`, which removes an unconditional telemetry goroutine that phoned `eventsapi.dolthub.com` (see chapter 04 for the driver's other modifications).

## Process lifecycle

### Startup

`main()` is three steps (`cmd/lit/main.go:17-20`): install the signal guard, run `cli.Run(ctx, os.Stdout, os.Stderr, os.Args[1:])`, and on error `os.Exit(cli.WriteCommandError(os.Stderr, err))`; a nil error exits 0.

Argument handling before any command runs (`internal/cli/cli.go`):

- `parseGlobalArgs` scans leading arguments; `--` stops scanning; a leading `--output`/`--output=…` is a typed `UnsupportedError` ("--output is no longer supported; omit it for text output") (`cli.go:171-181, 310-312`).
- The root command is `lit` ("Agent-native issue tracker"). With no args it resolves the workspace from cwd: outside a git repo it prints cobra help; inside one it prints the quickstart guidance for the workspace root (`cli.go:64-78`). An unrecognized positional is `UnknownCommandError` (`cli.go:59-61`).
- Cobra's default `completion` command is disabled; `SilenceErrors`/`SilenceUsage` are both set; help requests are swallowed and reported as success (`cli.go:44-50, 81`).
- There are no persistent global flags. Per-command flag parsing maps two removed flags to typed errors — `--output` (as above) and `--continue` ("--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag") — and any other unknown flag to `UsageError` (exit 2) (`cli.go:274-308`). `--help` prints `Usage of <cmd>:` plus flag defaults to stdout (`cli.go:265-272`).

Per-command bootstrap (`runWithApp`, `cli.go:102-147`): get cwd → `app.Open(ctx, cwd, accessMode)` (outside a git repo → `OutsideWorkspaceError`: "links requires running inside a git repository/worktree") → run the handler with `defer Close()` → on success of a write-mode command, print the mutation sync-staleness banner → after the engine is closed, `maybeAutoSyncAfterCommand` (see chapter 07).

`app.Open` (`internal/app/app.go:30-106`) takes an access mode — exactly `read` or `write`; anything else fails closed ("invalid access mode %q"). Mode selects both the engine mode (read-only vs read-write) and the stream-identity resolver: read uses `workspace.ReadStream` (never mints an identity), write uses `workspace.EnsureStream` (mints one if absent). It resolves the workspace, opens the store at `ws.DatabasePath` under `ws.WorkspaceID`, attributes the store to the checkout's stream token, and returns the app; a stream-resolution failure closes the store and joins both errors. `OpenLocationForRead` opens an already-derived location strictly read-only, reading `workspace_id` from the store's own `config.json` (`app.go:125-131`).

### Signals and shutdown

The guard catches SIGINT and SIGTERM with a 5-second grace period (`internal/interrupt/interrupt.go:33-39`):

1. First signal → cancel the command's context, then restore the OS default disposition — so a second signal kills the process immediately (`interrupt.go:101-104`).
2. A 5s timer starts; if the command finishes first, exit is clean; if the timer fires, the process exits `128+signum` (`interrupt.go:116-135`). Test-pinned: SIGINT → 130, SIGTERM → 143, non-syscall signal → 1.

Acceptance tests pin two shutdown properties (`cmd/lit/main_signal_test.go:146-160, 304`): SIGTERM during a post-write auto-sync wedged on the commit lock terminates in under 8s with exit **0** (the write's own success code), and SIGTERM during a git subprocess wedged on a black-hole remote also exits cleanly.

### Exit codes

Seven codes (`internal/cli/exit.go:10-18`); no code 6 exists.

| Code | Name | Produced by (dispatch order, `exit.go:23-95`) |
|---|---|---|
| 0 | `ExitOK` | success |
| 1 | `ExitGeneric` | `OutsideWorkspaceError`, `BulkFailureError`, `ErrTransientGCContention`, anything unclassified |
| 2 | `ExitUsage` | `UsageError` (flag/arg misuse) |
| 3 | `ExitValidation` | `UnknownCommandError`, `RetiredCommandError`, CLI and storage `ValidationError`, `UnsupportedError` |
| 4 | `ExitNotFound` | `storage.NotFoundError` |
| 5 | `ExitConflict` | `MergeConflictError`, `SyncFailureError`, `ownerApprovalRefusalError` |
| 7 | `ExitCorruption` | `CorruptionError` |

Error rendering (`internal/cli/error_output.go:17-46`): stderr gets `error (code=%d): %v`, then `remediation: %s` when a remediation exists for the error's machine reason (reasons include `entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`).

### Command registry

Commands are data, not imperative registration: `CommandSpec{Name, Summary, Long, GroupID, Run, Subcommands, Hidden}` (`internal/cli/register.go:17-52`). `Hidden` keeps a command dispatchable but out of `--help` and completion. Seven help groups render in a fixed order: Human Bootstrap, Agent Operations, Dependencies & Structure, Sync & Data, Setup & Maintenance, Issue Retention, Guidance & Tooling (`register.go:59-75`). Two standard blurbs distinguish the audiences: "Human bootstrap command…" and "Agent-facing operational command." (`cli.go:30-33`).

## Environment variables

Every variable the shipped binary reads:

| Variable | Effect |
|---|---|
| `XDG_CONFIG_HOME` | Global config dir = `$XDG_CONFIG_HOME/links-issue-tracker`, else `$HOME/.config/links-issue-tracker` (`internal/config/config.go:175-184`) |
| `LIT_CONFIG_GLOBAL_PATH` | Overrides the global config file path entirely (`config.go:270-273`) |
| `LIT_CONFIG_PROJECT_PATH` | Overrides the project config file path (`config.go:275-278`) |
| `LIT_DISABLE_AUTO_SYNC` | Truthy (`strconv.ParseBool`, parse error = false) → no command schedules a push mirror, runs an inline receive, or compacts, and the owner-notify hook never runs (`internal/cli/sync_cadence.go:19-33,79-81`; `owner_notify.go:149-151`) |
| `CLAUDE_CODE_SESSION_ID` | Non-empty after trim → the acting identity is always `claude_<sessionID>`, overriding `--assignee`/`--by` (`cli.go:1172-1177`) |
| `LNKS_AUTOMATION_TRIGGER` | Non-empty → automation-trace recording is on; the value is the trace's `Trigger` (`internal/cli/automation_trace.go:53-73`) |
| `LNKS_AUTOMATION_REASON` | Default `Reason` on the automation trace when the caller supplied none (`automation_trace.go:64-66`) |
| `LNKS_AUTOMATION_TRACE_REF_FILE` | Non-empty → the recorded trace's path is appended (mode 0644) to that file (`automation_trace.go:78-81`) |
| `LINKS_DEBUG_DOLT_SYNC_BRANCH` | Overrides the resolved sync branch ahead of the remote's default branch (`internal/cli/sync.go:689-693`) |
| `EDITOR` | Split on whitespace to form the editor command for `lit workflows edit` (`internal/cli/workflows_edit.go:140-147`) |

Variables the binary **writes** for children: the detached background mirror inherits the environment with the three `LNKS_AUTOMATION_*` variables stripped, then re-set to `TRIGGER=on-change` / `REASON=on-change cadence mirrored after a mutating command` (`internal/cli/sync_bg.go:104-127`). The owner-notify hook runs as `sh -c <hook>` in the repo root with `LIT_NOTIFY_KIND`, `LIT_NOTIFY_SUMMARY`, `LIT_NOTIFY_REMOTE`, `LIT_NOTIFY_BRANCH`, `LIT_NOTIFY_REPO` added, time-boxed with a `WaitDelay` so a backgrounded child cannot hold the pipe (`internal/cli/owner_notify.go:194-222`).

Read only by tests/tooling: `GITHUB_ACTIONS` (testbudget emits `::error::` annotations), `LIT_LICENSE_GRAPH_AUDIT` (gates the whole-graph license test), `LIT_TEST_REEXEC` (test binary re-execs as real `lit`), plus test-only `USER`/`HOME` negative-evidence checks and a kill-helper coordination variable.

## Configuration

### Files, format, precedence

Two layers merged in order, later overriding earlier (`internal/config/config.go:186-193`):

1. **Global**: `$LIT_CONFIG_GLOBAL_PATH`, else `<config-dir>/config.toml` (config dir per `XDG_CONFIG_HOME` above).
2. **Project**: `$LIT_CONFIG_PROJECT_PATH`, else `<workspaceRoot>/.lit/config.toml`.

Format is whatever viper infers from the extension; the default filename is TOML. A missing file is fine; a parse or merge error is fatal (`config.go:280-295`). Each layer also contributes `ready.required_fields` and the legacy top-level `required_fields`, concatenated across layers; a non-empty concatenation replaces `Ready.RequiredFields` (`config.go:239-241, 297-299`).

### Key schema

Defaults set in `Load` (`config.go:217-228`):

| Key | Type | Default | Validation |
|---|---|---|---|
| `logging.verbose` | bool | `false` | |
| `logging.file` | string | `""` | |
| `init.install_hooks` | bool | `true` | |
| `init.install_agents` | bool | `true` | |
| `migration.auto_apply` | bool | `false` | |
| `ready.required_fields` | []string | `[]` | also fed by legacy top-level `required_fields` |
| `quickstart.soil_mode` | bool | `false` | |
| `snapshot.retention_budget` | int | `5` | must be > 0 or `Load` fails (`config.go:245-247`) |
| `sync.cadence` | enum | `"on-change"` | `on-push` or `on-change`; anything else fails `Load` (`config.go:252-254`) |
| `sync.receive` | bool | `true` | |
| `sync.owner_notify_cmd` | string | `""` | empty = no owner-notification channel |
| `claims.freshness_window` | duration string | `"24h"` | read as a string and parsed by `time.ParseDuration`; a bare number or non-positive duration fails `Load` (`config.go:89-98, 262`) |

Cadence semantics (`config.go:127-150`): `on-push` mirrors only when the managed pre-push git hook runs; `on-change` (default) mirrors after every mutating command. `shouldSyncAfterMutation` is true only for write access + `on-change` (`sync_cadence.go:64-66`). `maybeAutoSyncAfterCommand` returns immediately under `LIT_DISABLE_AUTO_SYNC`; on an unreadable config it prints `lit: automatic sync skipped, config unreadable: %v` and returns; otherwise it runs mirror coverage per cadence, inline receive per `sync.receive`, and inline compaction for write commands (`sync_cadence.go:78-101`). Timing constants: receive debounce 5 minutes, remote-absent recheck 10 seconds (`sync_cadence.go:45,56`).

### Per-workspace store config

`<git-common-dir>/links/config.json` holds `workspace_id`, `issue_prefix`, `created_at` (RFC3339), `schema_version` (`internal/workspace/workspace.go:21-26`). Created on first resolve with a fresh UUID, UTC now, version 1 (`workspace.go:491-496`). Writes are atomic (temp file + chmod 0644 + rename); `UpdateConfig` is the single read-modify-write boundary (`workspace.go:504-560`). A blank `issue_prefix` is derived from the repository directory name — normalize the basename, split on `-`, take the first part that normalizes to a valid prefix, else the whole normalized base — and the derived/renormalized value is persisted back immediately (`workspace.go:421-434, 469-477, 562-579`).

## Workspace discovery

### One workspace from cwd

A `Location` is pure path geometry derived from the storage dir (`workspace.go:284-295`): `StorageDir` = `<git-common-dir>/links`, `ConfigPath` = `<storage>/config.json`, `DatabasePath` = `<storage>/dolt`, `DoltRepoPath` = `<storage>/dolt/links`.

`deriveLocation(cwd)` (`workspace.go:188-224`): run `git rev-parse --git-common-dir` anchored at cwd; join a relative answer onto the absolute cwd (not the repo toplevel); canonicalize through `filepath.EvalSymlinks`; append `links`. `Resolve(cwd)` adds: `git rev-parse --show-toplevel` for the root, `git rev-parse --git-dir` for the checkout's **private** git dir (deliberately not symlink-canonicalized, `workspace.go:261-275`), `MkdirAll` on the storage dir, and load-or-create of `config.json` (`workspace.go:140-179`). All geometry git calls use `context.Background()` deliberately.

Git-error classification (`workspace.go:337-343`): only an `exec.ExitError` with exit code 128 maps to the sentinel `ErrNotGitRepo`; git missing from PATH, signal-killed git, or any other exit code is surfaced with context.

Because everything hangs off the git *common* dir, all worktrees of one repository share one store; each worktree keeps its own private git dir, which is where its stream identity lives.

### Discovery across many roots

`Discover(roots)` walks each root with `filepath.WalkDir`, collecting locations keyed (and deduplicated) by canonical `StorageDir`, returned sorted (`internal/workspace/discover.go:24-41`). Per directory: skip directories literally named `.git`; require a `.git` entry (`Lstat`); derive the location (skip on `ErrNotGitRepo`); require `DatabasePath` to exist **and be a directory** (a regular file there is skipped); walk and stat errors are fatal with context (`discover.go:48-113`).

### Multi-checkout enumeration

`LiveCheckouts(cwd)` runs `git worktree list --porcelain -z` — any failure names the git ≥ 2.36 `-z` requirement (`internal/workspace/checkouts.go:61-81`). Records that are `prunable` or `bare` are filtered out; for each survivor the private git dir is located and its stream token read; any failure fails the whole enumeration (`checkouts.go:92-108`). The `-z` parser cuts each NUL-separated field on the first space, requires the record to open with `worktree <path>`, recognizes `branch` (stripped of `refs/heads/`), `prunable`, and `bare`, ignores `detached`/`locked`/`HEAD`/others, and errors on zero records (`checkouts.go:148-215`). `Checkout{Stream, Path, Branch}`; `Branch` is empty for detached HEAD.

### Stream identity

Each checkout's identity is a token in the file `lit-stream` inside its private git dir (`internal/workspace/stream.go:22,80`): 8 bytes of `crypto/rand`, unpadded lowercase base32 → exactly 13 characters over `[a-z2-7]` (`stream.go:28-38, 232-238`). `ReadStream` returns the zero ID for a missing file. `EnsureStream` reads, publishes if absent, re-reads, and returns whatever the file holds (`stream.go:106-130`). Publication is write-once by construction: write a temp file, fsync, then `os.Link` it to the final name — `os.ErrExist` from the link means another process won the race and is success (`stream.go:150-227`). A malformed token errors with instructions to delete the file and mint a fresh identity (`stream.go:260-282`). This token is one half of event attribution (chapter 01) and the anchor of claim derivation (chapter 08).

### Git-remote helpers

`UpstreamRemote` (first segment of `@{upstream}`), `RemoteHasRefs` (`git ls-remote` non-empty), `RemoteHasDoltData` (`git ls-remote <r> refs/dolt/*` non-empty), `DefaultRemoteBranch` (`symbolic-ref` on the remote-HEAD ref, falling back to `ls-remote --symref`), `GitRemotes` (`git remote -v` fetch lines, deduped, sorted). A blank remote name normalizes to `origin` (`workspace.go:98-138, 345-414`).

## Version identity

Link-time variables `Version`, `Commit`, `Date` are stamped by three writers: goreleaser (all three), `scripts/install.sh` source mode (all three), and the Justfile `build` recipe (Commit + Date only — a `just build` binary is a dev build) (`internal/version/version.go:28-38`). `Info` carries `version/commit/date/is_dev/schema_support`, where the supported schema range is derived from the embedded migration registry: min = baseline, max = highest migration (`version.go:57-93`). `BuildAge` is zero/false for an empty, unparseable, or future date (`version.go:101-113`).

`lit version` (`internal/cli/version.go:17-68`) rejects positionals (`usage: lit version`); prints `lit <version> (commit <c>, built <d>)` with `dev`/`unknown` fallbacks; then `built <age> ago` when the age is known; then, when the age ≥ the 7-day `StaleBuildThreshold`, a warning to rebuild; and finally `schema versions supported: <min>–<max>`.

## Release distribution

### Manifest

Each release publishes `release-manifest.json`: the `version.Info` fields plus an `artifacts` array of `{platform: "<goos>/<goarch>", url, sha256}` and a reserved, currently-unverified `signature` (`internal/release/manifest.go:35-61`). `is_dev` always serializes false for published manifests.

### Resolution

`HTTPResolver` fetches `<base>/<tag>/release-manifest.json` from `https://github.com/promptctl/links-issue-tracker/releases/download` (`internal/release/resolver.go:58,94`). Tag validation: must start with `v`, must match `^v[A-Za-z0-9._+-]+$`, must not contain `..` (`resolver.go:76-84`). 60s default HTTP timeout; non-200 errors carry the first 256 body bytes; the body is decoded through a 1 MiB limit with `DisallowUnknownFields`, and trailing JSON after the manifest is rejected (`resolver.go:104-132`). Artifact selection is an exact platform match against `runtime.GOOS + "/" + runtime.GOARCH`; a miss lists the available platforms (`internal/release/target.go:28-46`).

### Installer

`Install` (`internal/release/installer.go:60-119`) determines the format from the URL (`.tar.gz` → binary `lit`; `.zip` → `lit.exe`; anything else rejected), creates the temp file (`.lit-downgrade-*.tmp`) in the target directory *before* downloading (so an unwritable dir fails fast), downloads with SHA-256 verification, extracts, chmods 0755, and renames atomically into place; any pre-rename failure removes the temp file. Hardening caps and checks (`installer.go:124-438`):

- 256 MiB compressed cap, 256 MiB per-entry uncompressed cap, 512 MiB whole-stream cap; 5-minute default timeout.
- The expected SHA-256 must be a 64-char hex digest, checked before downloading; the download is capped and teed into the hash; mismatch is fatal.
- Every archive entry must have a flat, safe name (no `/`, `\`, `..`, `.`), be a regular file, and declare a size within cap; the entry's *actual* size is enforced during copy even when the header lied; exactly one entry may carry the binary name (zero or two is fatal).

### `lit downgrade`

One flag, `--to <version>` (v-prefixed tag; a missing `v` is added; `/`, `\`, `..`, or whitespace rejected) (`internal/cli/downgrade.go:77-83, 136-156`). Pipeline: require the store to support schema migration → resolve the target manifest for the current platform → `store.Downgrade` to the target's max supported schema → resolve the running binary via `os.Executable` + `EvalSymlinks` → install over it (`downgrade.go:38-124`). An install failure after a successful schema reversal names both recovery paths (manual artifact download, or `lit snapshots list`/`restore`). Success prints the installed version, schema, and path; no re-exec is performed. (The schema `Downgrade` itself is chapter 05's territory.)

### `scripts/install.sh`

Three modes: source build (default), `--from-release <tag>`, `--latest-release` (mutually exclusive; `--latest-release` needs `jq` and reads the GitHub API) (`scripts/install.sh:103-136, 241-254`). Target-directory priority: existing `lit` on PATH → `$GOBIN` → `go env GOBIN` → first `go env GOPATH` entry + `/bin` → `$HOME/.local/bin` (error if `HOME` unset) (`:141-187`).

- **Source mode**: version from `git describe --tags --always --dirty` (leading `v` stripped; empty stays empty so the build is dev), then `go build` with `-buildvcs=false` and ldflags for all three version variables (`:192-229`).
- **Release mode**: tag must be canonical `vX.Y.Z`; archive name `lit_<ver>_<os>_<arch>.<ext>` with an arch map (amd64/arm64 only) and OS map (linux/darwin → tar.gz, Windows shells → zip); downloads archive + `checksums.txt` into a temp dir created inside the target dir so the final `mv` is atomic; checksum extracted by exact awk field match and verified; tar/zip entry names are structurally validated *before* extraction (flat, regular files only); the extracted binary must not be a symlink, must be a regular file, and must be executable (`:230-456`).
- Post-install, unconditionally: removes stale `lnks` binaries in the target dir, prints `Installed lit -> <path>`, runs `lit version`, and warns about any other `lit` on PATH whose realpath differs from the just-installed one (`:459-492`).

### Helper scripts

- `scripts/version-ldflags.sh`: must be sourced (executing exits 64); exports `LIT_BUILD_COMMIT` (short HEAD) and `LIT_BUILD_DATE` (UTC RFC3339-like); deliberately never sets `Version`.
- `scripts/cgo-env.sh`: must be sourced (executing exits 64); a no-op off macOS; on Darwin requires Homebrew, locates `icu4c@78` (falling back to `icu4c`) and `zstd`, verifies headers exist, and exports `CGO_CPPFLAGS`/`CGO_LDFLAGS` preserving caller values.
- `scripts/next-version.sh <minor|patch>`: computes the next tag from the latest clean `vX.Y.Z` tag; major is frozen for this repo; distinct exit codes 2 (usage), 3 (no/malformed latest tag), 4 (computed tag already exists).
- `scripts/cleanroom-*.sh` exist but are referenced by no Justfile target or workflow.

## Build tooling

### Justfile

Every compile recipe sources `cgo-env.sh` first (`Justfile:1-5`).

| Target | Behavior |
|---|---|
| `default` | `just --list` |
| `setup` | Darwin: require Homebrew, `brew install icu4c@78 zstd`, persist `CGO_*` flags via `go env -w` when needed |
| `build` | `go build -buildvcs=false` with Commit + Date ldflags — deliberately no `Version`, so the result is a dev build |
| `test-short` | `go test -short ./...` |
| `test *args` | `go test -timeout 30m <args or ./...>`; a caller-supplied `-timeout` wins |
| `lint` | `golangci-lint run` |
| `install` | `./scripts/install.sh` |

### Repo tools

**`tools/testbudget`** — consumes `go test -json` on stdin (piped under `pipefail` so test failures still fail the job), replays output live, reports unfinished tests at stream end, prints a per-package elapsed-vs-budget table, and exits 1 on any budget violation (2 on an unreadable stream). Budgets: `internal/store` 210s, `internal/cli` 340s, `cmd/lit` 50s, `tools/licenses` 50s, everything else 30s (`tools/testbudget/budgets.go:39-73`). Under GitHub Actions violations are `::error::`-prefixed.

**`tools/mkmanifest`** — builds `release-manifest.json` from `dist/checksums.txt`. Required flags checked in fixed order (`-version` without `v`, `-tag` with `v`, `-commit`, `-date`, `-base-url`, `-out`); the schema range comes from the embedded migration registry; each checksum line must be `<64-hex>  <flat filename>`; referenced archives must exist; filenames must parse as exactly `lit_<version>_<goos>_<goarch>.{tar.gz,zip}` with the version segment matching `-version`; artifacts are sorted by platform and zero artifacts is fatal (`tools/mkmanifest/main.go:66-344`).

**`tools/licenses`** — three modes (`-check` and `-graph` are mutually exclusive): generate (default; writes `THIRD_PARTY_LICENSES`, `LICENSE-REPORT.md`, optional CycloneDX SBOM over `go list -deps` plus curated native C libraries), `-check` (the CI license-policy gate against `tools/licenses/policy.json`; exits non-zero on violations, writes nothing), `-graph` (audits the whole `go list -m all` build list; reports without gating). The graph acceptance test only runs when `LIT_LICENSE_GRAPH_AUDIT` is set (`tools/licenses/main.go:34-182`).

`tools/session-analysis` exists in the tree but is referenced by no Justfile target or workflow.

### Lint, attributes, dependency updates

- `.golangci.yml` enables exactly one linter: `depguard`, with a single rule denying any package outside `internal/model` from importing `internal/model/lifecycle` ("lifecycle internals are owned by internal/model; use model hydration and capability APIs") (`.golangci.yml:2-17`).
- `.gitattributes` forces LF on `*.sql` so the byte-pinned baseline migration and schema-snapshot canary (chapter 03) cannot be tripped by CRLF (`.gitattributes:1-12`).
- Dependabot: weekly gomod updates, minor+patch grouped as `go-safe`, majors arriving as individual PRs (`.github/dependabot.yml:1-16`).
- `.gitignore` covers build outputs (`/lit`, `/licenses`, `/mkmanifest`), generated compliance artifacts (`/THIRD_PARTY_LICENSES`, `/LICENSE-REPORT.md`, `/SBOM.cdx.json`), `artifacts/`, `site/`, `.claude/worktrees/`, `.claude/*.lock`, `tools/session-analysis/processed/`, `__pycache__/`.

## CI workflows

### `ci.yml` — merge gate

Push to `master` and PRs targeting `master`; concurrency-cancelled per ref; `ubuntu-latest` only, no matrix. Five parallel jobs (`.github/workflows/ci.yml`):

1. **build-and-test** — build, then `go test -short -json ./... | go run ./tools/testbudget` under `shell: bash` (pipefail load-bearing); no `-timeout` override by policy.
2. **race** — `go test -short -race ./cmd/... ./internal/...` (tools excluded deliberately; no budget pipe).
3. **verify** — `go mod tidy` must be a no-op (`git diff --exit-code`); `install.sh` must contain `-buildvcs=false`; then actually runs `install.sh`.
4. **lint** — golangci-lint pinned to v2.8.0.
5. **docs** — `mkdocs build --strict` (mkdocs 1.6.1 / material 9.7.6), so a missing docs page or broken nav link fails the merge gate. (`mkdocs.yml` defines the `links` material-theme site with a seven-entry nav.)

The composite action `.github/actions/install-dolt` installs the pinned Dolt CLI v1.81.10 — used as a test-only oracle, not shipped.

### `nightly.yml`

Cron `17 9 * * *` + manual dispatch; single 60-minute job running the full suite (`go test -timeout 30m ./...`, no `-short`). On failure *or cancellation* (a job timeout reports as cancelled) it force-creates the `nightly-failure` label and either comments on the open labeled issue or creates "Nightly full test lane is failing" with the run URL.

### `license-graph-audit.yml`

Mondays 09:00 UTC + manual; explicitly not a merge gate. Runs the graph acceptance test with a cold module cache (`cache: false` is the acceptance criterion) and `LIT_LICENSE_GRAPH_AUDIT=1`, then `tools/licenses -graph`, then asserts the audit did not modify `go.mod`/`go.sum`.

### `code-review.yml`

Generated by `agent-code-review-setup/install.sh` and reconverged (local edits overwritten) on every installer run. Runs on PR open/synchronize/reopen/ready-for-review — deliberately `pull_request`, not `pull_request_target` — checks out the PR head SHA with `persist-credentials: false` (checkout action pinned by SHA), and runs `promptctl/copirate-code-review-agent@v1` with `CLAUDE_CODE_OAUTH_TOKEN`, `DEEPSEEK_API_KEY`, and dependency-diff enabled; both credentials must exist in both the Actions and Dependabot secret stores.

### `release-smoke.yml` — PR-time release rehearsal

PRs targeting `master`. Builds (or pulls from GHCR) the release toolchain image — tagged by the **git tree hash of `build/`**, so the image is rebuilt exactly when `build/` changes — then runs `goreleaser build --single-target --snapshot` inside it with the repo, a read-only `GOMODCACHE`, and a cached `GOCACHE` mounted. Asserts the produced Linux binary is fully static (`readelf`: no `INTERP`, no `NEEDED`) and executes on both glibc (runner) and musl (`alpine:3.20`).

### `release-validate.yml` — the release pipeline

Push to `master` + manual dispatch; never on PRs; concurrency without cancellation. The release discriminator: if the newest `CHANGELOG.md` heading `## [X.Y.Z]` has no corresponding `vX.Y.Z` tag on origin (checked with `git ls-remote --exit-code`, three-way branch on the exit code), this push *is* a release.

- **license-gate** job: `tools/licenses -check` over the link closure with `CGO_ENABLED=1` (so `go list -deps` resolves the same set the release links).
- **validate** job: generates the license bundle/report/SBOM; builds all five targets in the pinned toolchain image (for a pending release, creates the ephemeral local tag first and runs a real `goreleaser release --clean`; otherwise `--snapshot`); generates the manifest with `mkmanifest` from `dist/metadata.json`; then a battery of assertions — manifest shape via jq (platform set restated literally as `["darwin/amd64","darwin/arm64","linux/amd64","linux/arm64","windows/amd64"]`, independent of `.goreleaser.yml` on purpose; every sha256 64-hex; every URL on github.com under the tag), the linux amd64 archive carries the license bundle + report + `FORKS.md` with the dolt row, the four native-library rows (icu 75.1 Unicode-3.0, zstd 1.5.6 BSD-3-Clause, musl 1.2.5 MIT, compiler-rt 0.14.0 MIT AND Apache-2.0 WITH LLVM-exception) and all three `replace` disclosures, the SBOM validates under the SHA-pinned CycloneDX CLI v0.27.2 and shows dolt exactly once with a pedigree descendant purl starting `pkg:golang/github.com/promptctl/dolt/go@`, and both Linux binaries are static and run on glibc + musl for amd64 and arm64. Release artifacts are uploaded (14-day retention), the smoke toolchain image is pushed to GHCR, and the snapshot `dist/` is always uploaded for inspection (7-day retention).
- **publish** job (only when a release is pending, needs validate + license-gate): downloads the validated artifact, verifies `metadata.json`'s commit equals the pushed SHA and its tag equals the CHANGELOG version, then `gh release create` with generated notes and only assets that actually exist — an empty asset list refuses to publish; a failed create prints the `gh release delete … --cleanup-tag` recovery command.

### goreleaser configuration

One build, `./cmd/lit`, CGO enabled, cross-compiled with per-target zig wrapper compilers (`zig-cc-<triple>`); ICU headers/libs from `/opt/icu/<os>_<arch>`; `-static` on linux only; `-tags=icu_static`; `-trimpath -buildvcs=false`; `-s -w` plus the three version ldflags. Targets: linux/darwin/windows × amd64/arm64 minus windows/arm64 = five. Archives are `lit_<version>_<os>_<arch>.tar.gz` (zip on Windows), no wrapper directory, bundling `LICENSE`, `README*`, `THIRD_PARTY_LICENSES`, `LICENSE-REPORT.md`, `FORKS.md`; sha256 `checksums.txt`; snapshot versions are `<incpatch>-snapshot+<shortcommit>`. Goreleaser itself never publishes (`release.disable: true`) — the workflow's publish job does (`.goreleaser.yml:21-225`).

### Release toolchain image

`build/Dockerfile.release` pins every ingredient by version and SHA-256: Go 1.25.7, goreleaser v2.16.0, zig 0.14.0, ICU 75.1, macOS SDK 13.3 (`Dockerfile.release:62-74`). Stages: a `base` with goreleaser, zig, generated `zig-cc-*` wrappers and a `zig-ar` shim; a native ICU build; three cross-ICU stages (linux amd64/arm64, windows amd64) via `build/icu-cross-build.sh` (static-only ICU, `--with-cross-build`, most components disabled); a `darwin-toolchain` stage with the pinned SDK and a four-line `tzfile.h` stub (`build/zig-macos-stubs/tzfile.h`, needed because ICU 75.1 includes `tzfile.h` unconditionally under `__APPLE__` and zig's SDK omits it); two darwin cross-ICU stages; a `smoke` target carrying only linux/amd64 ICU with `goreleaser` as entrypoint; and a `final` target adding the other four ICU prefixes, the SDK, and the darwin wrappers, with build-time `RUN` verifications that every ICU prefix has headers and archives and every darwin link input resolves.

## Claude integration shipped with the repo

- `.claude-plugin/marketplace.json` declares a local marketplace `links-marketplace` listing one plugin, `links`, sourced from `./claude-plugin`.
- The **entire** shipped plugin is one file, `claude-plugin/.claude-plugin/plugin.json`: name `links`, version 0.1.0, and two hooks — `SessionStart` and `PreCompact`, each with an empty matcher, each running `lit quickstart --refresh`. No commands, agents, skills, or MCP servers.
- The repo's own dogfooding wiring: `.claude/settings.json` runs `.claude/hooks/session-start.sh` on `SessionStart`; the script extracts `session_id` from the hook's stdin JSON and prints "Your Claude Code session id is: <id>. When using lit, your assignee identity is claude_<id>." — the same identity string `resolveIdentity` derives from `CLAUDE_CODE_SESSION_ID` (`cli.go:1173-1174`).
- `.claude/settings.local.json` carries local permissions (several `Bash(lit …)` allows, `gh pr`, two Read paths) and MCP server enablement.
