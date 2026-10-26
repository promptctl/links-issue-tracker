# Platform: startup, configuration, discovery, and release

`lit` is a single Go binary that runs inside a git checkout. Everything it persists lives under the repository's git common directory, so a workspace is discovered from the current working directory by asking git, never by a config file naming a path. This chapter covers the process lifecycle (startup, signals, exit codes), every environment variable, the two-layer configuration system, workspace and multi-checkout discovery, the per-checkout stream identity, version identity, the release/self-install machinery, and the build/CI tooling that produces and gates releases.

## Module identity and dependencies

Module `github.com/promptctl/links-issue-tracker`, Go `1.25.7` (`go.mod`); CI derives its Go version from `go.mod`. Direct dependencies include the Dolt SQL engine stack, cobra/pflag/viper, goose (migrations), google/uuid, cenkalti/backoff, CycloneDX + licenseclassifier + packageurl (license tooling), and `promptctl/primitives` (`go.mod`). Three `replace` directives redirect the Dolt stack (`go.mod`): `dolthub/dolt/go` and `dolthub/go-mysql-server` point at `promptctl` forks, and `dolthub/driver` points at the in-repo copy `./internal/vendor/dolthub-driver`, which removes an unconditional telemetry goroutine that phoned `eventsapi.dolthub.com` (see chapter 04 for the driver's other modifications).

## Process lifecycle

### Startup

`main()` is three steps (`cmd/lit/main.go`): install the signal guard, run `cli.Run(ctx, inv)` with a `cli.Invocation` built from the process (`os.Getwd`, `os.Args[1:]`, `environ.Of(os.Environ())`, `os.Stdin`, `os.Stdout`, `os.Stderr`), and on error `os.Exit(cli.WriteCommandError(os.Stderr, err))`; a nil error exits 0. No non-test code `cli.Run` can reach names `os.Getwd`, `os.Args`, `os.Stdin`, `os.Getenv`, `os.LookupEnv`, `os.Environ`, `os.UserHomeDir`, `os.UserConfigDir`, `os.TempDir` or `filepath.Abs`; it reads the invocation's. Nor does it build a child process but through `environ.Env`'s `Command`, `CommandContext` and `Git`, which run the child in the invocation's environment (`TestNothingRunReachesReadsTheProcess`). `cli.WorkingDirOf` refuses a directory that is not absolute, and the workspace layer runs git only in an absolute one, so no relative path resolves against the process's directory there.

Argument handling before any command runs (`internal/cli/cli.go`):

- `parseGlobalArgs` scans the flag-shaped arguments before the first positional; a `--` among them is removed and ends the scan; a `--output`/`--output=…` among them is a typed `UnsupportedError` ("--output is no longer supported; omit it for text output") (`cli.go`).
- The root command is `lit` ("Agent-native issue tracker"). With no args it resolves the workspace from cwd: outside a git repo it prints cobra help; inside one it prints the quickstart guidance for the workspace root (`cli.go`). An unrecognized positional is `UnknownCommandError` (`cli.go`).
- Cobra's default `completion` command is disabled; `SilenceErrors`/`SilenceUsage` are both set; help requests are swallowed and reported as success (`cli.go`).
- There are no persistent global flags. Per-command flag parsing maps removed flags to typed errors — `--output` (as above), and each flag a leaf declares retired with `Retire`: `next`'s `--continue` ("--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag"), and `--assignee` on every leaf whose `registerActor` adds `--by` and that registers no `--assignee` of its own ("--assignee does not name who is acting here; name yourself with --by <name> (CLAUDE_CODE_SESSION_ID wins when set), or change a ticket's assignee with `lit update <id> --assignee <name>`") — and any other unknown flag to `UsageError` (exit 2) (`flagset.go`). `--help` prints `Usage of <cmd>:` plus flag defaults to stdout (`cli.go`).

Per-command bootstrap (`runWithApp`, `cli.go`): get cwd → `app.Open(ctx, cwd, accessMode)` (outside a git repo → `OutsideWorkspaceError`: "links requires running inside a git repository/worktree") → run the handler with `defer Close()` → on success of a write-mode command, print the mutation sync-staleness banner → after the engine is closed, `maybeAutoSyncAfterCommand` (see chapter 07).

`app.Open` (`internal/app/app.go`) takes an access mode — exactly `read` or `write`; anything else fails closed ("invalid access mode %q"). Mode selects both the engine mode (read-only vs read-write) and the stream-identity resolver: read uses `workspace.ReadStream` (never mints an identity), write uses `workspace.EnsureStream` (mints one if absent). It resolves the workspace, opens the store at `ws.DatabasePath` under `ws.WorkspaceID`, attributes the store to the checkout's stream token, and returns the app; a stream-resolution failure closes the store and joins both errors. `OpenLocationForRead` opens an already-derived location strictly read-only, reading `workspace_id` from the store's own `config.json` (`app.go`).

### Signals and shutdown

The guard catches SIGINT and SIGTERM with a 5-second grace period (`internal/interrupt/interrupt.go`):

1. First signal → cancel the command's context, then restore the OS default disposition — so a second signal kills the process immediately (`interrupt.go`).
2. A 5s timer starts; if the command finishes first, exit is clean; if the timer fires, the process exits `128+signum` (`interrupt.go`). Test-pinned: SIGINT → 130, SIGTERM → 143, non-syscall signal → 1.

Acceptance tests pin two shutdown properties (`cmd/lit/main_signal_test.go`): SIGTERM during a post-write auto-sync wedged on the commit lock terminates in under 8s with exit **0** (the write's own success code), and SIGTERM during a git subprocess wedged on a black-hole remote also exits cleanly.

### Exit codes

Eight codes, 0 through 7 (`internal/cli/exit.go`), mapped from error type by `ExitCode`. The code-to-error-type table is in `06-issue-commands.md` and is maintained there; what belongs to this chapter is the process-level contract around it: a code is the whole of what the process says on exit, and a signal-terminated run exits `128+signum` instead, from the path above rather than from `ExitCode`.

Error rendering (`internal/cli/error_output.go`): stderr gets `error (code=%d, reason=%s): %v`, then `remediation: %s` when a remediation exists for that reason (reasons include `entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`).

### Command registry

Commands are data, not imperative registration: `CommandSpec{Name, Summary, GroupID, Run, Subcommands, Hidden, Retired}` (`internal/cli/register.go`). `Hidden` keeps a command dispatchable but out of `--help` and completion. Seven help groups render in a fixed order: Human Bootstrap, Agent Operations, Dependencies & Structure, Sync & Data, Setup & Maintenance, Issue Retention, Guidance & Tooling (`register.go`). A command's long-form description is not a registry field: every command sets `DisableFlagParsing`, so its help page is rendered by its own leaf, from text embedded under `internal/cli/helptext/`.

## Environment variables

Every variable the shipped binary reads:

| Variable | Effect |
|---|---|
| `XDG_CONFIG_HOME` | Global config dir = `$XDG_CONFIG_HOME/links-issue-tracker`, else `$HOME/.config/links-issue-tracker` (`internal/config/config.go`) |
| `XDG_STATE_HOME` | State dir = `$XDG_STATE_HOME/links-issue-tracker`, else `$HOME/.local/state/links-issue-tracker` (`config.go`) |
| `HOME` | The home dir of the two above; `USERPROFILE` on Windows (`internal/environ/environ.go`) |
| `LIT_CONFIG_GLOBAL_PATH` | Overrides the global config file path entirely (`config.go`) |
| `LIT_CONFIG_PROJECT_PATH` | Overrides the project config file path (`config.go`) |
| `LIT_DISABLE_AUTO_SYNC` | Truthy (`strconv.ParseBool`, parse error = false) → no command schedules a push mirror or a receive, prints a pending receive block, or compacts, and the owner-notify hook never runs (`internal/cli/sync_cadence.go`; `owner_notify.go`) |
| `CLAUDE_CODE_SESSION_ID` | Non-empty after trim → the acting identity is always `claude_<sessionID>`, overriding `--by` (`cli.go`) |
| `LNKS_AUTOMATION_TRIGGER` | Non-empty → automation-trace recording is on; the value is the trace's `Trigger` (`internal/cli/automation_trace.go`) |
| `LNKS_AUTOMATION_REASON` | Default `Reason` on the automation trace when the caller supplied none (`automation_trace.go`) |
| `LNKS_AUTOMATION_TRACE_REF_FILE` | Non-empty → the recorded trace's path is appended (mode 0644) to that file (`automation_trace.go`) |
| `LINKS_DEBUG_DOLT_SYNC_BRANCH` | Overrides the resolved sync branch ahead of the remote's default branch (`internal/cli/sync.go`) |
| `EDITOR` | Split on whitespace to form the editor command for `lit workflows edit` (`internal/cli/workflows_edit.go`) |

Variables the binary **writes** for children: the detached background mirror runs in the invocation's environment with the three `LNKS_AUTOMATION_*` variables stripped, then re-set to `TRIGGER=on-change` / `REASON=on-change cadence mirrored after a mutating command` (`internal/cli/sync_bg.go`). The owner-notify hook runs as `sh -c <hook>` in the repo root with `LIT_NOTIFY_KIND`, `LIT_NOTIFY_SUMMARY`, `LIT_NOTIFY_REMOTE`, `LIT_NOTIFY_BRANCH`, `LIT_NOTIFY_REPO` added, time-boxed with a `WaitDelay` so a backgrounded child cannot hold the pipe (`internal/cli/owner_notify.go`).

Read only by tests/tooling: `GITHUB_ACTIONS` (testbudget emits `::error::` annotations; `internal/testoracle` fails, rather than skips, a test whose `claude` oracle is not on PATH), `LIT_LICENSE_GRAPH_AUDIT` (gates the whole-graph license test), `LIT_TEST_REEXEC` (test binary re-execs as real `lit`), plus test-only `USER`/`HOME` negative-evidence checks and a kill-helper coordination variable.

## Configuration

### Files, format, precedence

Two layers merged in order, later overriding earlier (`internal/config/config.go`):

1. **Global**: `$LIT_CONFIG_GLOBAL_PATH`, else `<config-dir>/config.toml` (config dir per `XDG_CONFIG_HOME` above).
2. **Project**: `$LIT_CONFIG_PROJECT_PATH`, else `<workspaceRoot>/.lit/config.toml`.

Every layer is TOML, whatever the file is named, and a project layer handed to a detached sync worker is parsed by the same function; the default filename is `config.toml`. A missing file is fine; a parse or merge error is fatal (`config.go`). Each layer also contributes `ready.required_fields` and the legacy top-level `required_fields`, concatenated across layers; a non-empty concatenation replaces `Ready.RequiredFields` (`config.go`).

### Key schema

The `settings` table, which supplies `Load`'s defaults (`config.go`):

| Key | Type | Default | Validation |
|---|---|---|---|
| `ready.required_fields` | []string | `[]` | also fed by legacy top-level `required_fields` |
| `quickstart.soil_mode` | bool | `false` | |
| `snapshot.retention_budget` | int | `5` | must be > 0 or `Load` fails (`config.go`) |
| `sync.cadence` | enum | `"on-change"` | `on-push` or `on-change`; anything else fails `Load` (`config.go`) |
| `sync.receive` | bool | `true` | |
| `sync.owner_notify_cmd` | string | `""` | empty = no owner-notification channel |
| `sync.history_bound.re_root_at` | integer | `2000` | the remote-head height at which a landed push's history bound re-roots (chapter 05); a value that is not a TOML integer, or is negative, fails `Load` (`config.go`) |
| `sync.history_bound.keep` | integer | `1000` | the least height the re-root leaves the head at (always below `.re_root_at`); read as `.re_root_at` is, and unless 1 <= keep < re_root_at, `Load` fails with `config: sync.history_bound: …` (`config.go`); a landed push whose config `Load` refuses keeps no history bound and says so (chapter 05) |
| `claims.freshness_window` | duration string | `"6h"` | read as a string and parsed by `time.ParseDuration`; a bare number or non-positive duration fails `Load` (`config.go`) |

A key the table does not list, other than the legacy top-level `required_fields`, has no effect, and `Load` does not refuse it: the project layer is committed and read by clones on older lits. `UnknownKeys` lists each such key, lowercased as viper reads it, with the file that sets it, in layer order and alphabetically within a file; a layer that does not parse contributes its error and no keys, and the other layer is still listed (`config.go`). Every command acquires its workspace through `resolveWorkspace` (`cli.go`), which calls it once, before the command's work, and prints `warning: config keys were not checked: <err>` on stderr when a layer does not parse, then `warning: <file> sets "<key>", which this version of lit does not read, so it has no effect` for each key; the command runs either way, and its event records `config.unknown_keys` and `config.unknown_keys_error` (`internal/cli/config_keys.go`). `TestSettingsAreTheKeysLoadReads` holds the table to the keys `Config` decodes plus `claims.freshness_window` and the two `sync.history_bound` keys (`config_test.go`).

Cadence semantics (`config.go`): `on-push` mirrors only when the managed pre-push git hook runs; `on-change` (default) mirrors after every mutating command. `shouldSyncAfterMutation` is true only for write access + `on-change` (`sync_cadence.go`). `maybeAutoSyncAfterCommand` returns immediately under `LIT_DISABLE_AUTO_SYNC`; on an unreadable config it prints `lit: automatic sync skipped, config unreadable: %v` and returns; otherwise it prints any pending receive block, runs mirror coverage per cadence, schedules the receive worker per `sync.receive`, and runs inline compaction for write commands (`sync_cadence.go`). Timing constants: receive debounce 5 minutes, remote-absent recheck 10 seconds (`sync_cadence.go`).

### Per-workspace store config

`<git-common-dir>/links/config.json` holds `workspace_id`, `issue_prefix`, `created_at` (RFC3339), `schema_version` (`internal/workspace/workspace.go`). Created on first resolve with a fresh UUID, UTC now, version 1 (`workspace.go`). Writes go through `atomicfile.WriteFile` with mode 0644, durably; `UpdateConfig` is the single read-modify-write boundary (`workspace.go`). A blank `issue_prefix` is filled from one of two sources, ranked by `resolveIssuePrefix`: an explicit `workspace.PrefixRequest` if the caller carries one (`lit init --prefix`), otherwise derivation from the repository directory name — normalize the basename, split on `-`, take the first part that normalizes to a valid prefix, else the whole normalized base. Either way the value is persisted back immediately and reads back as *configured*, not derived, when it came from a request. Derivation failing is `ErrIssuePrefixRefused`, exit 3; so is a request that contradicts a non-blank stored prefix, which is refused rather than applied — `lit prefix set` is the command that changes a prefix already in use. The third case, a STORED `issue_prefix` the rules refuse, does not fail resolution: the workspace resolves into the refused arm of `PrefixState`, `config.json` is left exactly as found, and reading, `lit doctor` and `lit prefix set` all run. Only minting refuses — `PrefixState.Mintable()` returns the typed `StoredPrefixError`, which unwraps to the same sentinel and so shares exit 3, but classifies as its own reason `stored_prefix_refused`, whose remediation names `lit prefix set <prefix>`: the preview lists the prefixes the store's ids already use, so a typo is repaired back to the original rather than rewritten to a guess (`workspace.go`, `internal/cli/error_output.go`).

## Workspace discovery

### One workspace from cwd

A `Location` is pure path geometry derived from the storage dir (`workspace.go`): `StorageDir` = `<git-common-dir>/links`, `ConfigPath` = `<storage>/config.json`, `DatabasePath` = `<storage>/dolt`, `DoltRepoPath` = `<storage>/dolt/links`.

`Locate(cwd)` and `Resolve(cwd)` share one git query, `git rev-parse --show-toplevel --git-common-dir --git-dir`, run in cwd (`workspace.go`). git prints the three paths one per line, unquoted, and a path may hold a newline, so the output is cut into the three answers every possible way and the one cut whose paths all name an existing directory is the answer; no such cut, or more than one, is an error (`workspace.go`). A relative git dir is joined onto the absolute cwd (not the repo toplevel); `Locate` canonicalizes the common dir through `filepath.EvalSymlinks` and appends `links`. `Resolve` adds: the toplevel as the root, git's `--git-dir` answer, symlink-canonicalized, as the checkout's **private** git dir (so a `GIT_DIR` or `GIT_WORK_TREE` git honours is honoured), `MkdirAll` on the storage dir, load-or-create of `config.json`, and the checkout's `Remotes` (`workspace.go`). The geometry query uses `context.Background()` deliberately.

Git-error classification (`workspace.go`): only an `exec.ExitError` with exit code 128 maps to the sentinel `ErrNotGitRepo`; git missing from PATH, signal-killed git, or any other exit code is surfaced with context.

Because everything hangs off the git *common* dir, all worktrees of one repository share one store; each worktree keeps its own private git dir, which is where its stream identity lives.

### Discovery across many roots

`Discover(roots)` walks each root with `filepath.WalkDir`, collecting locations keyed (and deduplicated) by canonical `StorageDir`, returned sorted with how many directories the walk visited and how many working trees it reached (`internal/workspace/discover.go`). A directory holding a `.git` entry (`Lstat`) is a working tree: the walk records it and does not descend into it, so a store whose repository sits inside another repository's working tree is found only by naming its directory as a root. Each working tree is then derived on a pool of `GOMAXPROCS` goroutines: derive the location (skip on `ErrNotGitRepo`); require `DatabasePath` to exist **and be a directory** (a regular file there is skipped). Walk, stat and derivation errors are fatal with context, and the one reported is the first in walk order (`discover.go`). `lit stores` records `discover.directories`, `discover.working_trees` and `discover.stores` on its event (`internal/cli/stores.go`).

### Multi-checkout enumeration

`LiveCheckouts(ws)` runs `git worktree list --porcelain -z` in `ws.RootDir` — any failure names the git ≥ 2.36 `-z` requirement (`internal/workspace/checkouts.go`). Records that are `prunable` or `bare` are filtered out; for each survivor the private git dir is read with no git subprocess. The main worktree, which git lists first, is the common dir, because in a submodule or `--separate-git-dir` clone git lists it at the git dir itself. A linked worktree's is read from the `.git` entry at its path — the directory itself, or the path a gitfile names after `gitdir: ` (read whole, trailing `\n`/`\r` removed, relative to the path), symlink-canonicalized — and its stream token read; any failure fails the whole enumeration (`checkouts.go`). The `-z` parser cuts each NUL-separated field on the first space, requires the record to open with `worktree <path>`, recognizes `branch` (stripped of `refs/heads/`), `prunable`, and `bare`, ignores `detached`/`locked`/`HEAD`/others, and errors on zero records (`checkouts.go`). `Checkout{Stream, Path, Branch}`; `Branch` is empty for detached HEAD.

### Stream identity

Each checkout's identity is a token in the file `lit-stream` inside its private git dir (`internal/workspace/stream.go`): 8 bytes of `crypto/rand`, unpadded lowercase base32 → exactly 13 characters over `[a-z2-7]` (`stream.go`). `ReadStream` returns the zero ID for a missing file. `EnsureStream` reads, publishes if absent, re-reads, and returns whatever the file holds (`stream.go`). Publication is write-once by construction: write a temp file, fsync, then `os.Link` it to the final name — `os.ErrExist` from the link means another process won the race and is success (`stream.go`). A malformed token errors with instructions to delete the file and mint a fresh identity (`stream.go`). This token is one half of event attribution (chapter 01) and the anchor of claim derivation (chapter 08).

### Git-remote helpers

`Remotes` (`workspace.go`) is a checkout's remote configuration, minted by `Resolve` for its `Info` and asked of git at most once per question per invocation: `List` (`git remote -v` fetch lines, asked of the repository with `--git-dir`, deduped, sorted) and `Upstream` (the current branch's `%(upstream:remotename)`, whole, so a branch tracking `a/b` names `a/b`; a git that fails to answer is an error). `Repository` (`repository.go`) is the repository's common git dir. Each of its questions runs `git --git-dir=<dir> --work-tree=<dir>` with git's repository-locating variables removed, so no checkout, cwd, or inherited `GIT_DIR` decides the answer. It asks a remote by URL, never by name: `RemoteHasRefs` (`git ls-remote` non-empty), `RemoteDoltRefs` (the trimmed output of `git ls-remote -- <url> refs/dolt/*`, one round trip), `RemoteHasDoltData` (that output non-empty), `RefusedRemoteHasDoltData` (the same, for a `RefusedGitRemote`, by its written URL from its checkout root), `LocalRemoteHead` (`symbolic-ref` on the remote-HEAD ref, local only; unset is `""`, a failed git is an error), and `AdvertisedRemoteHead` (the remote's symbolic HEAD from `ls-remote`, one round trip, its failure returned).

## Version identity

Link-time variables `Version`, `Commit`, `Date`, `Origin` are stamped by three writers: goreleaser (all four, `Origin` the literal `release`), `scripts/install.sh` source mode (all four, `Version` via `git describe`), and the Justfile `build` recipe (Commit + Date + Origin only — a `just build` binary is a dev build) (`internal/version/version.go`). Two of those four fields are enforced against the producer files themselves rather than trusted to the prose here: `TestEveryProducerStampsOrigin` owns `Origin`, and `TestOnlyTheJustfileOmitsVersion` owns `Version` — the Justfile must not stamp it, `scripts/install.sh` and goreleaser must (`internal/version/stamp_sites_test.go`). `Info` carries `Version`, `Commit`, `Date`, `IsDev`, `FromSource` and `Schema`; every field but `FromSource` is on the wire, as `version/commit/date/is_dev/schema_support` (`version.go`), where the supported schema range is derived from the embedded migration registry at call time: min = baseline, max = highest migration (`Get`, `version.go`). `BuildAge` is zero/false for an empty, unparseable, or future date (`version.go`); `StaleSourceBuild` returns that age plus the one staleness verdict every surface reads (`version.go`).

`lit version` (`internal/cli/version.go`) rejects positionals (`usage: lit version`); prints `lit <version> (commit <c>, built <d>)` with `dev`/`unknown` fallbacks; then `built <age> ago` when the age is known; then, when `Info.StaleSourceBuild` reports true — a from-source build whose age is at or past the 7-day `StaleBuildThreshold`, so a release build never warns at any age — ``WARNING: this build is at least <threshold> old — run `just build` (or `just install`) to refresh``, rendered by the pure `versionStalenessWarning` (`version.go`) through the same `stalenessThresholdClause` and `buildRefreshRemedy` its sibling surfaces use; and finally `schema versions supported: <min>–<max>`.

## Release distribution

### Manifest

Each release publishes `release-manifest.json`: the `version.Info` fields plus an `artifacts` array of `{platform: "<goos>/<goarch>", url, sha256}` and a reserved, currently-unverified `signature` (`internal/release/manifest.go`). `is_dev` always serializes false for a published manifest (a release is by definition not a dev build) and stays on the wire; `from_source` never appears on the wire at all, in any value — `Info.FromSource` is tagged `json:"-"` at its declaration (`internal/version/version.go`), because the question is meaningless for a release and a manifest carrying the field would fail to decode in every lit already installed. `TestManifestNeverSerializesFromSource` asserts the marshalled bytes, not the tag.

### Resolution

`HTTPResolver` fetches `<base>/<tag>/release-manifest.json` from `https://github.com/promptctl/links-issue-tracker/releases/download` (`internal/release/resolver.go`). `Resolve` takes a `release.Tag`, which only `ParseTag` makes: exactly `^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`, `release-tags.sh`'s pattern, else `"<s>" is not a release tag (a release tag is vX.Y.Z with no leading zeros, e.g. v0.4.1)` (`internal/release/tag.go`). `LatestTag` parses the feed's `tag_name` through the same `ParseTag`. 60s default HTTP timeout; non-200 errors carry the first 256 body bytes; the body is decoded through a 1 MiB limit that **tolerates unknown fields** — `lit upgrade` is run by the installed binary to discover a newer release, so a manifest's consumer is by construction older than its producer and a field the producer added later is the ordinary case, not an attack (`TestHTTPResolverToleratesFieldsFromANewerProducer`); trailing JSON after the manifest is still rejected (`resolver.go`). The key set is checked on the producer side instead: release-validate's "Assert manifest shape" step requires the top level, `schema_support`, and every artifact to carry exactly the keys it lists (restated literally in the step, so a new wire field is added there in the same change; `TestManifestWireKeys` pins the same keys on every PR, since the step runs only after merge), and asserts the required fields well-formed (`.version` a non-empty string, `.commit` 7 hex characters, `.date` RFC 3339, `.is_dev` false, `.schema_support.min`/`.max` numbers, every artifact's `platform`, `url` and `sha256` matching their patterns with the tag segment present in the URL) and that the artifact platform set equals the release contract exactly. Artifact selection is an exact platform match against `runtime.GOOS + "/" + runtime.GOARCH`; a miss lists the available platforms (`internal/release/target.go`).

### Installer

`Install` (`internal/release/installer.go`) determines the format from the URL (`.tar.gz` → binary `lit`; `.zip` → `lit.exe`; anything else rejected), replaces the target through `atomicfile.Replace` with mode 0755, durably, downloading with SHA-256 verification and extracting inside it, after its temp file exists (so an unwritable dir fails fast); any failure leaves the target unchanged and removes the temp file. Hardening caps and checks (`installer.go`):

- 256 MiB compressed cap, 256 MiB per-entry uncompressed cap, 512 MiB whole-stream cap; 5-minute default timeout.
- The expected SHA-256 must be a 64-char hex digest, checked before downloading; the download is capped and teed into the hash; mismatch is fatal.
- Every archive entry must have a flat, safe name (no `/`, `\`, `..`, `.`), be a regular file, and declare a size within cap; the entry's *actual* size is enforced during copy even when the header lied; exactly one entry may carry the binary name (zero or two is fatal).

### `lit downgrade`

One flag, `--to <version>` (v-prefixed tag; a missing `v` is added; `/`, `\`, `..`, or whitespace rejected) (`internal/cli/downgrade.go`). Pipeline: require the store to support schema migration → resolve the target manifest for the current platform → `store.Downgrade` to the target's max supported schema → resolve the running binary via `os.Executable` + `EvalSymlinks` → install over it (`downgrade.go`). An install failure after a successful schema reversal names both recovery paths (manual artifact download, or `lit snapshots list`/`restore`). Success prints the installed version, schema, and path; no re-exec is performed. (The schema `Downgrade` itself is chapter 05's territory.)

### `scripts/install.sh`

Three modes: source build (default), `--from-release <tag>`, `--latest-release` (mutually exclusive; both release modes need `jq`; `--latest-release` reads the GitHub API) (`scripts/install.sh`). Target-directory priority: existing `lit` on PATH → `$GOBIN` → `go env GOBIN` → first `go env GOPATH` entry + `/bin` (both read in `/`, outside any module) → `$HOME/.local/bin` (error if `HOME` unset). Release mode's `REPO_DOWNLOAD_BASE` and `REPO_LATEST_API` are copies of `release.DefaultBaseURL` and `release.DefaultLatestReleaseURL`, kept because it runs piped from curl; `TestInstallFetchesTheReleaseURLsLitUpgradeFetches` fails when install.sh and `release.HTTPResolver` request a different latest-release feed or a different release manifest (`scripts/install_test.go`).

- **Source mode**: builds the checkout in the script's parent directory, from that directory, when its `go.mod` names `github.com/promptctl/links-issue-tracker`; otherwise (the script read from stdin, as a curl pipe runs it, a process substitution, or a copy outside a checkout) it exits 1 naming `--from-release <tag>` and `--latest-release`, right after flag parsing and before the target directory is made. Before building lit, it builds and runs `tools/schema-release-check` with the newest local release tag (selected by `scripts/release-tags.sh`); a nonzero exit stops the install before lit is built: exit 3 when the tree carries a migration that release does not hold byte for byte, and exit 1 when the checkout has no release tag, which a tree with no `.git` of its own never has (`scripts/install.sh`). Then version from `git describe --tags --always --dirty` (leading `v` stripped), then `go build` with `-buildvcs=false` and ldflags for all four version variables — `Version`, `Commit`, `Date`, and `Origin` (`source`), the last of which is what keeps the stamped `Version` from making an installed working-tree build read as a release.
- **Release mode**: tag must be canonical `vX.Y.Z`; downloads `<base>/<tag>/release-manifest.json` and installs the artifact it lists for the current platform, as `lit upgrade` does: the platform is `uname` in Go's names (`mingw*`/`msys*`/`cygwin*` → windows; `x86_64` → amd64, `aarch64` → arm64, `i?86` → 386, `armv*` → arm), a platform the manifest does not list is refused naming the platforms it does, and the archive format is the artifact URL's suffix (`.tar.gz` or `.zip`, anything else refused); downloads the artifact URL and verifies the manifest's SHA256; tar/zip entry names are structurally validated *before* extraction (flat, regular files only); the extracted binary must not be a symlink and must be a regular file.
- **Install, every mode**: the build or download lands in a temp dir created inside the target dir, so the final `mv` is atomic. The staged binary's `lit version` must succeed before the rename; if it fails, the script says nothing was installed at the target path and exits 1.
- Post-install, unconditionally: removes stale `lnks` binaries in the target dir, prints `Installed lit -> <path>` followed by the version output, and warns about any other `lit` on PATH whose realpath differs from the just-installed one.

### Helper scripts

- `scripts/version-ldflags.sh`: must be sourced (executing exits 64); exports `LIT_BUILD_COMMIT` (short HEAD), `LIT_BUILD_DATE` (UTC RFC3339-like), and `LIT_BUILD_ORIGIN` (the constant `source`, shared by both from-source entrypoints); deliberately never sets `Version`.
- `scripts/cgo-env.sh`: must be sourced (executing exits 64); a no-op off macOS; on Darwin requires Homebrew, locates `icu4c@78` (falling back to `icu4c`) and `zstd`, verifies headers exist, and exports `CGO_CPPFLAGS`/`CGO_LDFLAGS` preserving caller values.
- `scripts/release-tags.sh`: must be sourced (executing exits 64); the one definition of a release tag, exactly `vX.Y.Z` with no leading zero in any part, and `newest_release_tag`, which reads tag names on stdin and prints the highest release by version order, or nothing. `changelog-frozen.sh`, `next-version.sh`, and `install.sh`'s source mode select through it, and `release-kind.sh` refuses a newest heading whose `v<version>` it does not match; `install.sh`'s release mode keeps a copy of the pattern, because it runs piped from curl, and so does `release.ParseTag`, because Go cannot source it; `TestReleaseModeAcceptsExactlyTheReleaseTags`, `TestReleaseTagParsesExactlyTheReleaseTags`, and `TestReleaseKindCutsExactlyTheReleaseTags` hold each consumer to the same accept-set (`scripts/install_test.go`).
- `scripts/next-version.sh <minor|patch>`: computes the next tag from the newest release tag; major is frozen for this repo; distinct exit codes 2 (usage), 3 (no release tag), 4 (computed tag already exists).
- `scripts/cleanroom-*.sh` exist but are referenced by no Justfile target or workflow.

## Build tooling

### Justfile

Every compile recipe sources `cgo-env.sh` first (`Justfile`).

| Target | Behavior |
|---|---|
| `default` | `just --list` |
| `setup` | Darwin: require Homebrew, `brew install icu4c@78 zstd`, persist `CGO_*` flags via `go env -w` when needed |
| `build` | `go build -buildvcs=false` with Commit + Date + Origin (`source`) ldflags — deliberately no `Version`, so the result is a dev build |
| `test-short` | `go test -short ./...` |
| `test *args` | `go test -timeout 30m <args or ./...>`; a caller-supplied `-timeout` wins |
| `lint` | `golangci-lint run` |
| `install` | `./scripts/install.sh` |

### Repo tools

**`tools/testbudget`** — consumes `go test -json` on stdin (piped under `pipefail` so test failures still fail the job), replays output live, prints the buffered output of tests still unfinished when their package ends (or when the stream ends, for a package that never reports), prints a per-package elapsed-vs-budget table, and exits 1 on any budget violation (2 on an unreadable stream). Each violation lists the package's ten slowest top-level tests, slowest first, with elapsed seconds, `parallel` or `serial`, and `[unfinished]` for a test with no verdict, whose elapsed is its clock when the package ended. A finished test's elapsed is go test's, extended to the end of its last subtest, so a parent of `t.Parallel` subtests counts their time. Budgets: `internal/store` 210s, `internal/cli` 340s, `cmd/lit` 50s, `tools/licenses` 50s, everything else 30s (`tools/testbudget/budgets.go`). Under GitHub Actions violations are `::error::`-prefixed.

**`tools/mkmanifest`** — builds `release-manifest.json` from `dist/checksums.txt`. Required flags checked in fixed order (`-version` without `v`, `-tag` with `v`, `-commit`, `-date`, `-base-url`, `-out`); the schema range comes from the embedded migration registry; each checksum line must be `<64-hex>  <flat filename>`; referenced archives must exist; filenames must parse as exactly `lit_<version>_<goos>_<goarch>.{tar.gz,zip}` with the version segment matching `-version`; artifacts are sorted by platform and zero artifacts is fatal (`tools/mkmanifest/main.go`).

**`tools/schema-release-check <tag>`** — the install guard `scripts/install.sh` runs in source mode, with the newest release tag or the empty string. The tree's schema is the `MaxVersion` of its own embedded registry, `migrations.Embedded()`; the release's is the `MaxVersion` of `migrations.ParseRegistry` over the file names `git ls-tree` lists in the tag's `internal/store/migrations/`, so both sides are read by one registry rule. The tree passes only when every embedded migration's `git hash-object --no-filters` id is the tag's id for the same file. Every run prints `schema-release-check: tree=<n> release_tag=<tag|none> release_schema=<n|none|unknown> verdict=<install|refuse>` on stdout. Exits 0 to install, 3 when the tag lacks a tree migration or holds different bytes for it, 1 when there is no tag or either side cannot be read; a refusal names `scripts/install.sh --latest-release` (`tools/schema-release-check/main.go`).

**`tools/candybar-check`** — installs the pinned `@promptctl/cc-candybar@1.136.0` with `npm install --prefix` into a temp dir, then renders ten cases with `cc-candybar check --render`, each in its own checkout (`git init`) under an environment of only `PATH` and its own `HOME`, `XDG_DATA_HOME` and `XDG_CONFIG_HOME`. Each case's `.lit/statusline.json` is written by `statusline.Write` from facts, except `no-phase-key`, whose `working_on` has the `phase` key removed, as a lit from before phases wrote it, `null-rollup`, whose `rollup_progress` is `null`, as lit 0.19 to 0.21 wrote it, and `no-file`, which has no file; the plugin is installed by `cc-candybar plugin install <repository root>` from `.cc-candybar/plugins/lit/`, under the same environment. The config makes `plugins.lit.ticket`, the plugin's one segment, the whole `root`; `description-open` renders under a config whose override of the segment's `detail` state variable defaults it to `open` (keyed `plugins.lit.detail`, as cc-candybar keeps a plugin's keys under its namespace), since `check` renders with no session state, and `description-off` places the segment with `description chars` at 0. The printed lines are read as cells: SGR sequences stripped, OSC 8 hyperlinks written `[text](url)`, split on U+E0B0 and U+E0B1, every piece between the first and last arrow a cell (a blank one kept); the first line after `config OK` is the bar, its leading 🍫 menu cell dropped, and every later line a row a segment dropped under it. Any other print (no `config OK` line, an unread escape, a line that does not open and close on an arrow, no menu cell) fails the case. Each case prints `candybar-check: case=<name> verdict=<ok|mismatch|error> cells=<cells> rows=<rows>`, and the run ends with `candybar-check: cc-candybar=<version> plugin=<repository root> cases=<n> failed=<n>`. Exits 1 when any case fails, or, printing no summary, when `git rev-parse --show-toplevel` finds no repository or cc-candybar cannot be installed; 0 otherwise (`tools/candybar-check/main.go`).

**`tools/licenses`** — three modes (`-check` and `-graph` are mutually exclusive): generate (default; writes `THIRD_PARTY_LICENSES`, `LICENSE-REPORT.md`, optional CycloneDX SBOM over `go list -deps` plus curated native C libraries), `-check` (the CI license-policy gate against `tools/licenses/policy.json`; exits non-zero on violations, writes nothing), `-graph` (audits the whole `go list -m all` build list; reports without gating). The graph acceptance test only runs when `LIT_LICENSE_GRAPH_AUDIT` is set (`tools/licenses/main.go`).

`tools/session-analysis` exists in the tree but is referenced by no Justfile target or workflow.

### Lint, attributes, dependency updates

- `.golangci.yml` enables exactly one linter: `depguard`, with a single rule denying any package outside `internal/model` from importing `internal/model/lifecycle` ("lifecycle internals are owned by internal/model; use model hydration and capability APIs") (`.golangci.yml`).
- `.gitattributes` checks every text file out with LF on every platform (`* text=auto eol=lf`), so the byte-pinned baseline migration and schema-snapshot canary (chapter 03) cannot be tripped by CRLF (`.gitattributes`).
- Dependabot: weekly gomod updates, minor+patch grouped as `go-safe`, majors arriving as individual PRs (`.github/dependabot.yml`).
- `.gitignore` covers build outputs (`/lit`, `/licenses`, `/mkmanifest`), generated compliance artifacts (`/THIRD_PARTY_LICENSES`, `/LICENSE-REPORT.md`, `/SBOM.cdx.json`), `artifacts/`, `site/`, `.claude/worktrees/`, `.claude/*.lock`, `tools/session-analysis/processed/`, `__pycache__/`.

## CI workflows

### `ci.yml` — merge gate

Push to `master` and PRs targeting `master`; concurrency-cancelled per ref; `ubuntu-latest` only, no matrix. Six parallel jobs (`.github/workflows/ci.yml`):

1. **build-and-test** — installs Dolt and Claude Code, builds, then runs `go test -short -json ./... | go run ./tools/testbudget` and the same for the vendored driver module (`go -C internal/vendor/dolthub-driver test …`), under `shell: bash` (pipefail load-bearing); no `-timeout` override by policy.
2. **race** — installs Dolt and Claude Code, then `go test -short -race -timeout 30m ./cmd/... ./internal/...` (tools excluded deliberately; no budget pipe). The timeout is sized above internal/cli's testbudget ceiling as the detector inflates it, so slowness fails build-and-test's budget check, not this lane's timeout.
3. **verify** — `go mod tidy` must be a no-op (`git diff --exit-code`); `install.sh` must contain `-buildvcs=false`; then actually runs `install.sh`, passing on exit 0 or on exit 3, the schema guard's refusal of a tree no release supports, which master is between a migration merging and its release.
4. **lint** — golangci-lint pinned to v2.8.0.
5. **docs** — `mkdocs build --strict` (mkdocs 1.6.1 / material 9.7.6), so a missing docs page or broken nav link fails the merge gate. (`mkdocs.yml` defines the `links` material-theme site with a seven-entry nav.)
6. **candybar** — Go plus Node 22 (`actions/setup-node`), then `go run ./tools/candybar-check`, so a lit cc-candybar plugin that does not render each case's cells fails the merge gate.

The composite action `.github/actions/install-claude` installs Node 22 and the pinned Claude Code CLI 2.1.292, with its auto-updater disabled for the rest of the job — the test-only oracle for `claude plugin validate --json`. Every job that runs `go test` (build-and-test, race, nightly `full-suite` and `windows`) runs it, and under `GITHUB_ACTIONS=true` a test whose oracle is missing fails instead of skipping (`internal/testoracle`).

The composite action `.github/actions/report-to-lit` (inputs `label`, `title`, `body`, `token`) files a scheduled workflow's red run into this repository's lit backlog: it sets a global git `extraheader` from the token (embedded Dolt runs git against its own cache repository), builds lit, runs `lit init` with `LIT_DISABLE_AUTO_SYNC=1` to adopt the remote backlog, then comments `Red again: <run URL>` on the first open or in-progress ticket carrying the label, or files an urgent `--top` bug with that label and topic inside epic `links-ci-alarm-z6sv`, and finishes with `lit sync push`. A refused push is followed by `lit sync pull` and another push, three pushes at most.

### `nightly.yml`

Cron `17 9 * * *` + manual dispatch. Job `full-suite` (60 minutes, `contents: read`) installs Dolt and Claude Code, then runs the full suite (`go test -timeout 30m ./...`, no `-short`) and then `go run ./tools/lawtokens-sync -check`. Job `windows` (`windows-latest`, 60 minutes) is the one lane that executes the Windows release target: it provisions gcc and ICU through MSYS2's `UCRT64` environment, installs Dolt and Claude Code, then builds and runs `go test -short` over `./cmd/... ./internal/...` with `CGO_ENABLED` set, skipping the known Windows failures listed in `.github/windows-known-failures.txt` (one `<ticket> <package> <top-level test>` per line, each ticket a child of epic `links-windows-qr8l`; `*` for a package whose tests do not compile there, which is not run). A package with listed tests gets its own invocation with `-skip`, every invocation runs even after one fails, a line whose third field is neither `*` nor a `Test…` name fails the step, as does a line naming a test (checked with `go test -list`) or a package that does not exist, and the step posts a notice counting what it skipped. It then runs `go -C internal/vendor/dolthub-driver test -short ./...`, even when the first test step failed. Each lane has its own report job (`contents: write`) that runs on a `master` run whenever its lane did not succeed, timeout and cancellation included, and calls `report-to-lit` under its own alarm label, so one lane's open ticket never absorbs the other's red runs: `report` for `full-suite`, with label `nightly-failure` and title "Nightly full test lane is failing", and `report-windows` for `windows`, with label `nightly-windows-failure` and title "Nightly Windows test lane is failing".

### `license-graph-audit.yml`

Mondays 09:00 UTC + manual; explicitly not a merge gate. Runs the graph acceptance test with a cold module cache (`cache: false` is the acceptance criterion) and `LIT_LICENSE_GRAPH_AUDIT=1`, then `tools/licenses -graph`, then asserts the audit did not modify `go.mod`/`go.sum`.

### `release-smoke.yml` — PR-time release rehearsal

PRs targeting `master`. Builds (or pulls from GHCR) the release toolchain image — tagged by the **git tree hash of `build/`**, so the image is rebuilt exactly when `build/` changes — then runs `goreleaser build --single-target --snapshot` inside it with the repo, a read-only `GOMODCACHE`, and a cached `GOCACHE` mounted. Asserts the produced Linux binary is fully static (`readelf`: no `INTERP`, no `NEEDED`) and executes on both glibc (runner) and musl (`alpine:3.20`).

### `release-validate.yml` — the release pipeline

Push to `master` + manual dispatch; never on PRs; concurrency without cancellation. The release discriminator: if the newest `CHANGELOG.md` heading `## [X.Y.Z]` has no corresponding `vX.Y.Z` tag on origin (checked with `git ls-remote --exit-code`, three-way branch on the exit code), this push *is* a release.

- **license-gate** job: `tools/licenses -check` over the link closure with `CGO_ENABLED=1` (so `go list -deps` resolves the same set the release links).
- **validate** job: generates the license bundle/report/SBOM; builds all five targets in the pinned toolchain image (for a pending release, creates the ephemeral local tag first and runs a real `goreleaser release --clean`; otherwise `--snapshot`); generates the manifest with `mkmanifest` from `dist/metadata.json`; then a battery of assertions — manifest shape via jq (platform set restated literally as `["darwin/amd64","darwin/arm64","linux/amd64","linux/arm64","windows/amd64"]`, independent of `.goreleaser.yml` on purpose; the key set of the top level, `schema_support` and every artifact restated literally, independent of `internal/release`; a 7-hex commit, an RFC 3339 date and `is_dev` false; every sha256 64-hex; every URL on github.com under the tag), the linux amd64 archive carries the license bundle + report + `FORKS.md` with the dolt row, the four native-library rows (icu 75.1 Unicode-3.0, zstd 1.5.6 BSD-3-Clause, musl 1.2.5 MIT, compiler-rt 0.14.0 MIT AND Apache-2.0 WITH LLVM-exception) and all three `replace` disclosures, the SBOM validates under the SHA-pinned CycloneDX CLI v0.27.2 and shows dolt exactly once with a pedigree descendant purl starting `pkg:golang/github.com/promptctl/dolt/go@`, and both Linux binaries are static and run on glibc + musl for amd64 and arm64. Release artifacts are uploaded (14-day retention), the smoke toolchain image is pushed to GHCR, and the snapshot `dist/` is always uploaded for inspection (7-day retention).
- **publish** job (only when a release is pending, needs validate + license-gate): downloads the validated artifact, verifies `metadata.json`'s commit equals the pushed SHA and its tag equals the CHANGELOG version, then `gh release create` with generated notes and only assets that actually exist — an empty asset list refuses to publish; a failed create prints the `gh release delete … --cleanup-tag` recovery command.

### `release-watchdog.yml`

Cron `23 * * * *` + manual dispatch; concurrency group `release-watchdog` without cancellation. Job `watchdog` (10 minutes, `contents: read`, `GRACE_MINUTES: 120`) runs `scripts/release-kind.sh`: `snapshot` or `tagged …` passes; `pending <version>` measures minutes since the newest commit whose `CHANGELOG.md` diff adds or removes `## [<version>]` and fails once that exceeds the grace window, or when no such commit exists; any other verdict fails. Job `report` (`contents: write`) runs on a `master` run whenever `watchdog` did not succeed and calls `report-to-lit` with label `release-stuck`, title "A promoted release is stuck: no tag published", and a body naming the `gh workflow run release-validate.yml --ref master` recovery.

### goreleaser configuration

One build, `./cmd/lit`, CGO enabled, cross-compiled with per-target zig wrapper compilers (`zig-cc-<triple>`); ICU headers/libs from `/opt/icu/<os>_<arch>`; `-static` on linux only; `-tags=icu_static`; `-trimpath -buildvcs=false`; `-s -w` plus the four version ldflags (`Origin` is the literal `release`, the only producer that stamps it). Targets: linux/darwin/windows × amd64/arm64 minus windows/arm64 = five. Archives are `lit_<version>_<os>_<arch>.tar.gz` (zip on Windows), no wrapper directory, bundling `LICENSE`, `README*`, `THIRD_PARTY_LICENSES`, `LICENSE-REPORT.md`, `FORKS.md`; sha256 `checksums.txt`; snapshot versions are `<incpatch>-snapshot+<shortcommit>`. Goreleaser itself never publishes (`release.disable: true`) — the workflow's publish job does (`.goreleaser.yml`).

### Release toolchain image

`build/Dockerfile.release` pins every ingredient by version and SHA-256: Go 1.25.7, goreleaser v2.16.0, zig 0.14.0, ICU 75.1, macOS SDK 13.3 (`Dockerfile.release`). Stages: a `base` with goreleaser, zig, generated `zig-cc-*` wrappers and a `zig-ar` shim; a native ICU build; three cross-ICU stages (linux amd64/arm64, windows amd64) via `build/icu-cross-build.sh` (static-only ICU, `--with-cross-build`, most components disabled); a `darwin-toolchain` stage with the pinned SDK and a four-line `tzfile.h` stub (`build/zig-macos-stubs/tzfile.h`, needed because ICU 75.1 includes `tzfile.h` unconditionally under `__APPLE__` and zig's SDK omits it); two darwin cross-ICU stages; a `smoke` target carrying only linux/amd64 ICU with `goreleaser` as entrypoint; and a `final` target adding the other four ICU prefixes, the SDK, and the darwin wrappers, with build-time `RUN` verifications that every ICU prefix has headers and archives and every darwin link input resolves.

## Claude integration shipped with the repo

- `.claude-plugin/marketplace.json` declares a marketplace `lit` listing one plugin, `lit`, sourced from `./claude-plugin`.
- The shipped plugin is two files: the manifest `claude-plugin/.claude-plugin/plugin.json` (name `lit`, no version, no hooks) and the skill `claude-plugin/skills/next/SKILL.md` (`next`, "Pull the next ticket"). No commands, agents, hooks, or MCP servers.
- `.claude/settings.local.json` carries local permissions (several `Bash(lit …)` allows, `gh pr`, two Read paths) and MCP server enablement.
