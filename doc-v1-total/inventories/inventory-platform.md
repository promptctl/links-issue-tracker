# `lit` platform inventory — bootstrap, configuration, discovery, release/build tooling

Derived entirely from source (Go, YAML, JSON, shell, Dockerfile, Justfile). No markdown
documentation was read. Every claim carries a `file:line` citation. Paths are repo-relative
to `/Users/bmf/code/links-issue-tracker`.

---

## 1. Module identity and dependencies

- Module path: `github.com/promptctl/links-issue-tracker` (`go.mod`).
- Go language/toolchain version: `go 1.25.7` (`go.mod`). CI derives its Go version from this
  file (`go-version-file: go.mod`, e.g. `.github/workflows/ci.yml`).
- Direct requirements (`go.mod`):
  - `github.com/CycloneDX/cyclonedx-go v0.11.0` (`go.mod`)
  - `github.com/cenkalti/backoff/v4 v4.2.1` (`go.mod`)
  - `github.com/dolthub/dolt/go v0.40.5-0.20260314011441-62975ef6bf36` (`go.mod`)
  - `github.com/dolthub/driver v0.2.1-0.20260314000741-0fe74e7ee31a` (`go.mod`)
  - `github.com/google/licenseclassifier` (`go.mod`)
  - `github.com/google/uuid v1.6.0` (`go.mod`)
  - `github.com/package-url/packageurl-go v0.1.6` (`go.mod`)
  - `github.com/pmezard/go-difflib` (`go.mod`)
  - `github.com/pressly/goose/v3 v3.27.1` (`go.mod`)
  - `github.com/promptctl/primitives v0.2.0` (`go.mod`)
  - `github.com/spf13/cobra v1.10.2`, `github.com/spf13/pflag v1.0.10`,
    `github.com/spf13/viper v1.21.0` (`go.mod`)
  - `golang.org/x/mod v0.35.0`, `golang.org/x/sync v0.22.0`, `golang.org/x/sys v0.45.0`
    (`go.mod`)
  - `gopkg.in/yaml.v3 v3.0.1` (`go.mod`)
- Three `replace` directives redirect the Dolt stack (`go.mod`):
  - `github.com/dolthub/dolt/go` → `github.com/promptctl/dolt/go v0.40.5-0.20260821231005-4b80eac34485`
    (`go.mod`)
  - `github.com/dolthub/go-mysql-server` → `github.com/promptctl/go-mysql-server v0.20.1-0.20260821032251-ab5cb9ec3b69`
    (`go.mod`)
  - `github.com/dolthub/driver` → local directory `./internal/vendor/dolthub-driver`
    (`go.mod`); the in-repo comment states the local copy removes an unconditional
    `eventsapi.dolthub.com` telemetry goroutine (`go.mod`).

---

## 2. Process startup and shutdown

### 2.1 `main()` sequence (`cmd/lit/main.go`)

1. `interrupt.Guard(context.Background(), interrupt.DefaultGrace)` installs the signal handler
   and returns `(ctx, stop)`; `stop` is deferred (`cmd/lit/main.go`).
2. `cli.Run(ctx, os.Stdout, os.Stderr, os.Args[1:])` executes the command
   (`cmd/lit/main.go`).
3. On a non-nil error, `os.Exit(cli.WriteCommandError(os.Stderr, err))`
   (`cmd/lit/main.go`). On nil error the process returns from `main` normally (exit 0).

### 2.2 Signal handling (`internal/interrupt/interrupt.go`)

- Caught signal set: `syscall.SIGINT`, `syscall.SIGTERM` (`internal/interrupt/interrupt.go`).
  SIGKILL is deliberately absent (`internal/interrupt/interrupt.go`).
- `DefaultGrace = 5 * time.Second` (`internal/interrupt/interrupt.go`).
- `Guard` allocates a 1-buffered signal channel, calls `signal.Notify`, derives a cancellable
  context, and spawns `watch` with the escalation closure `os.Exit(exitCode(sig))`
  (`internal/interrupt/interrupt.go`).
- `stop()` (deferred by `main`) is idempotent: it calls `signal.Stop`, `cancel()`, and closes
  the `done` channel; repeated calls are no-ops (`internal/interrupt/interrupt.go`).
- `watch` behavior (`internal/interrupt/interrupt.go`):
  - Blocks on `select` over `done` and `sigs`. `done` first ⇒ normal shutdown, returns without
    escalating (`internal/interrupt/interrupt.go`).
  - First interrupt ⇒ `cancel()`, then `restoreDefault()` which is
    `signal.Stop(sigs)`, so a **second** interrupt terminates the process via
    the OS default disposition.
  - Then a `time.NewTimer(grace)` is started; a second `select` picks `done` (clean exit, no
    escalation) or `timer.C` (`escalate(sig)` ⇒ `os.Exit`) (`internal/interrupt/interrupt.go`).
- `exitCode` maps a signal to `128+signum` for `syscall.Signal` values and to `1` for any
  non-`syscall` signal (`internal/interrupt/interrupt.go`). Test-pinned values:
  SIGINT ⇒ 130, SIGTERM ⇒ 143, non-syscall ⇒ 1 (`internal/interrupt/interrupt_test.go`).

### 2.3 Acceptance behavior pinned by the signal tests (`cmd/lit/main_signal_test.go`)

- `TestMain` re-execs the test binary as the real `lit` binary when `LIT_TEST_REEXEC=1`
  (`cmd/lit/main_signal_test.go`).
- A SIGTERM delivered to the receive worker while it is wedged on the commit lock must end it
  in under 8 s on the clean path, its `receive end` line written; the `lit backlog` that
  spawned it has already returned (`cmd/lit/main_signal_test.go`).
- A SIGTERM delivered while a git subprocess is wedged against a black-hole remote is pinned by
  `TestSIGTERMDuringWedgedGitSubprocessExitsCleanly` (`cmd/lit/main_signal_test.go`),
  using a listener-with-no-accept remote (`cmd/lit/main_signal_test.go`).

### 2.4 Argument parsing and the root command (`internal/cli/cli.go`)

- `Run` first calls `parseGlobalArgs(args)` (`internal/cli/cli.go`). That function scans
  the flag-shaped arguments before the first positional and:
  - a `--` among them is removed and the scan ends, the rest passed through
    (`internal/cli/cli.go`);
  - a bare `--output` or any `--output=…` among them returns
    `UnsupportedError{Message: "--output is no longer supported; omit it for text output"}`
    (`internal/cli/cli.go`);
  - the first token that is not flag-shaped ends the scan (`internal/cli/cli.go`).
- The root cobra command: `Use: "lit"`, `Long: "Agent-native issue tracker"`,
  `Args: cobra.ArbitraryArgs`, `DisableFlagParsing: true` (`internal/cli/cli.go`).
- Root with **no args**: resolves the workspace from cwd; if the error is
  `workspace.ErrNotGitRepo` it prints cobra help; otherwise it renders and prints the
  quickstart guidance for `ws.RootDir` (`internal/cli/cli.go`).
- Root with an unrecognized positional arg returns `UnknownCommandError{Command: args[0]}`
  (`internal/cli/cli.go`).
- The default `completion` command is disabled (`root.CompletionOptions.DisableDefaultCmd = true`,
  `internal/cli/cli.go`).
- Any global flag parse error is converted to `UsageError` (exit 2) via `SetFlagErrorFunc`
  (`internal/cli/cli.go`).
- `SilenceErrors` and `SilenceUsage` are both true; `pflag.ErrHelp` and the internal
  `errHelpHandled` sentinel are swallowed and reported as success
  (`internal/cli/cli.go`).
- There are **no persistent global flags** registered on the root beyond cobra's own `help`;
  per-command flag sets are constructed by `newCobraFlagSet` (`internal/cli/cli.go`).
- Per-command flag parsing (`parseFlagSet`, `internal/cli/flagset.go`) maps specific
  removed flags to typed errors:
  - `--continue` ⇒ `UnsupportedError` "--continue is retired; claim routing already keeps
    `lit next` in your checkout's own epic first — run `lit next` with no flag"
    (`internal/cli/flagset.go`);
  - every other parse error (unknown flag, missing value, invalid value, bad syntax) ⇒
    `UsageError` (`internal/cli/flagset.go`).
  - `--help` (or `pflag.ErrHelp`) prints `Usage of <cmd>:` followed by `PrintDefaults()` to
    stdout and returns the swallowed sentinel (`internal/cli/cli.go`).

### 2.5 Per-command bootstrap (`runWithApp` / `acquireFromWD`)

- `acquireFromWD` is what every command but `lit init` acquires with: the workspace for the
  working directory, with no prefix of its own to offer (`internal/cli/register.go`).
  It calls `resolveWorkspaceFromWD`, which maps `workspace.ErrNotGitRepo` to
  `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}`
  and a getcwd failure to `get cwd: %w` (`internal/cli/cli.go`).
- `runWithApp` (`internal/cli/cli.go`):
  1. `os.Getwd()`; failure ⇒ `get cwd: %w` (`internal/cli/cli.go`).
  2. `app.Open(ctx, cwd, accessMode)`; `workspace.ErrNotGitRepo` ⇒
     `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}`
     (`internal/cli/cli.go`).
  3. Captures `ap.Workspace` before running (`internal/cli/cli.go`), runs the handler with
     `defer ap.Close()` (`internal/cli/cli.go`).
  4. On success and `accessMode == app.AccessWrite`, prints the mutation sync-staleness banner
     (`printMutationSyncStalenessWarning(stdout, ws, time.Now())`, `internal/cli/cli.go`).
  5. Calls `maybeAutoSyncAfterCommand(ctx, accessMode, ws)` **after** the engine is closed
     (`internal/cli/cli.go`).

### 2.6 `app.Open` (`internal/app/app.go`)

- `AccessMode` is a string enum with exactly `AccessRead = "read"` and `AccessWrite = "write"`
  (`internal/app/app.go`).
- `accessContracts` maps each mode to `(engine.Mode, stream resolver)`:
  read ⇒ `engine.ReadOnly` + `workspace.ReadStream`; write ⇒ `engine.ReadWrite` +
  `workspace.EnsureStream` (`internal/app/app.go`).
- An unknown/zero mode fails closed with `invalid access mode %q` (`internal/app/app.go`).
- Sequence: resolve workspace → open engine at `ws.DatabasePath` with `ws.WorkspaceID` →
  resolve the stream identity from `ws.PrivateGitDir` → `st.AttributeTo(stream.Value())` →
  return `*App` (`internal/app/app.go`). A stream-resolution failure closes the store and
  joins both errors (`internal/app/app.go`).
- `OpenLocationForRead(ctx, loc)` opens an already-derived `Location` strictly read-only,
  reading `workspace_id` from the store's own `config.json` via `ReadConfig`
  (`internal/app/app.go`).
- `(*App).Close()` = `Store.Close()` (`internal/app/app.go`).

### 2.7 Exit codes (`internal/cli/exit.go`)

Constants (`internal/cli/exit.go`):

| Name | Value |
|---|---|
| `ExitOK` | 0 |
| `ExitGeneric` | 1 |
| `ExitUsage` | 2 |
| `ExitValidation` | 3 |
| `ExitNotFound` | 4 |
| `ExitConflict` | 5 |
| `ExitNoWork` | 6 |
| `ExitCorruption` | 7 |

`ExitCode(err)` dispatches by `errors.As` in this order
(`internal/cli/exit.go`):

- `storage.NotFoundError` ⇒ 4
- `MergeConflictError` ⇒ 5
- `SyncFailureError` ⇒ 5
- `templateShapeError` ⇒ 3
- `ownerApprovalRefusalError` ⇒ 5
- `CorruptionError` ⇒ 7
- `UsageError` ⇒ 2
- `UnknownCommandError` ⇒ 3
- `RetiredCommandError` ⇒ 3
- `ValidationError` ⇒ 3
- `storage.ValidationError` ⇒ 3
- `model.ContainerActionError` ⇒ 6 when `Satisfied()`, else 3
- `UnsupportedError` ⇒ 3
- `Exhausted` ⇒ 6
- `NoWork` ⇒ 6
- `OutsideWorkspaceError` ⇒ 3
- `store.ErrWorkspaceNotInitialized` ⇒ 3
- `BulkFailureError` ⇒ 1
- `store.ErrTransientGCContention` ⇒ 1
- anything else ⇒ 1

### 2.8 Error rendering (`internal/cli/error_output.go`)

- `WriteCommandError` prints `error (code=%d): %v\n` to stderr, then, when a remediation exists
  for the error's reason, `remediation: %s\n`; it returns the exit code
  (`internal/cli/error_output.go`).
- `commandErrorReason` maps typed errors to machine reason strings, including
  `entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`
  (`internal/cli/error_output.go`).

---

## 3. Every environment variable the program reads

Complete grep of `os.Getenv` / `os.LookupEnv` / `os.Environ` across non-vendored source.

### 3.1 Read by the shipped binary

| Variable | Where read | Effect |
|---|---|---|
| `XDG_CONFIG_HOME` | `internal/config/config.go` | When non-empty, `ConfigDir()` = `$XDG_CONFIG_HOME/links-issue-tracker`; otherwise `$HOME/.config/links-issue-tracker` (`internal/config/config.go`). |
| `LIT_CONFIG_GLOBAL_PATH` | `internal/config/config.go`, read at | Overrides the global config file path entirely; otherwise `ConfigDir()/config.toml` (`internal/config/config.go`). |
| `LIT_CONFIG_PROJECT_PATH` | `internal/config/config.go`, read at | Overrides the project config file path; otherwise `<workspaceRoot>/.lit/config.toml` (`internal/config/config.go`). |
| `LIT_DISABLE_AUTO_SYNC` | const at `internal/cli/sync_cadence.go`; read at `internal/cli/sync_cadence.go` and `internal/cli/owner_notify.go` | When truthy, no command schedules a push mirror or a receive, prints a pending receive block, **or** compacts (`internal/cli/sync_cadence.go`), and the owner-notify hook never runs (`internal/cli/owner_notify.go`). Truthiness = `strconv.ParseBool` of the trimmed value; a parse error is false (`internal/cli/sync_cadence.go`). |
| `CLAUDE_CODE_SESSION_ID` | `internal/cli/cli.go` | When non-empty (after trim), the acting identity is always `claude_<sessionID>`, overriding `--assignee`/`--by`; otherwise the trimmed explicit value passes through (`internal/cli/cli.go`). |
| `LNKS_AUTOMATION_TRIGGER` | const `internal/cli/automation_trace.go`; read | Non-empty enables automation-trace recording for the command; the value becomes the trace's `Trigger` field (`internal/cli/automation_trace.go`). Empty ⇒ no trace is written. |
| `LNKS_AUTOMATION_REASON` | const `internal/cli/automation_trace.go`; read | Default `Reason` on the automation trace when the caller supplied none (`internal/cli/automation_trace.go`). |
| `LNKS_AUTOMATION_TRACE_REF_FILE` | const `internal/cli/automation_trace.go`; read | When non-empty, the recorded trace's path is written (plus newline, mode 0644) to that file (`internal/cli/automation_trace.go`). |
| `LINKS_DEBUG_DOLT_SYNC_BRANCH` | const `internal/cli/sync.go`; read `internal/cli/sync.go` | Overrides the resolved sync branch ahead of the remote's default branch (`internal/cli/sync.go`); named in the failure message when no branch can be resolved (`internal/cli/sync.go`). |
| `EDITOR` | `internal/cli/workflows_edit.go` | Split on whitespace to form the editor command for `lit workflows edit`; named verbatim in the failure message. |
| (whole environment) | `internal/cli/sync_bg.go`, `internal/cli/owner_notify.go` | Inherited by child processes — see 3.2. |

### 3.2 Environment variables the binary **writes** for child processes

- Detached background mirror (`mirrorEnv`, `internal/cli/sync_bg.go`): inherits the
  parent environment with `LNKS_AUTOMATION_TRIGGER=`, `LNKS_AUTOMATION_REASON=`, and
  `LNKS_AUTOMATION_TRACE_REF_FILE=` prefixes **stripped** (`internal/cli/sync_bg.go`),
  then appends `LNKS_AUTOMATION_TRIGGER=on-change` and
  `LNKS_AUTOMATION_REASON=on-change cadence mirrored after a mutating command`
  (`internal/cli/sync_bg.go`).
- Owner-notify hook (`runOwnerNotifyHook`, `internal/cli/owner_notify.go`): the hook is
  run as `sh -c <hook>` with `cmd.Dir = repoRoot` (`internal/cli/owner_notify.go`)
  and `os.Environ()` plus:
  `LIT_NOTIFY_KIND`, `LIT_NOTIFY_SUMMARY`, `LIT_NOTIFY_REMOTE`, `LIT_NOTIFY_BRANCH`,
  `LIT_NOTIFY_REPO` (`internal/cli/owner_notify.go`). The hook is time-boxed by
  `ownerNotifyHookTimeout` with a `WaitDelay` so a backgrounded child cannot hold the pipe
  (`internal/cli/owner_notify.go`).

### 3.3 Read only by tests / repo tooling

| Variable | Where | Effect |
|---|---|---|
| `GITHUB_ACTIONS` | `tools/testbudget/main.go` | When `== "true"`, budget-violation lines are prefixed `::error::` so they become GHA annotations (`tools/testbudget/main.go`). |
| `LIT_LICENSE_GRAPH_AUDIT` | `tools/licenses/graph_test.go` (via `graphAuditEnv`) | Empty ⇒ the whole-graph acceptance test is skipped; the license-graph-audit workflow sets it to `"1"` (`.github/workflows/license-graph-audit.yml`). |
| `LIT_TEST_REEXEC` | `cmd/lit/main_signal_test.go` | `=1` makes the test binary run the real `main()` instead of the test suite. |
| `USER`, `HOME` | `internal/workspace/stream_test.go` | Used only as negative-evidence material asserting stream tokens contain no username/home material. |
| (`killHelperDirEnvVar`) | `internal/store/process_kill_test.go` | Test-helper subprocess coordination directory. |

---

## 4. Configuration

### 4.1 Location, format, precedence

- Format: whatever viper infers from the file extension via `SetConfigFile`
  (`internal/config/config.go`); the shipped/default filename is `config.toml`
  (`internal/config/config.go`).
- Layers, merged in slice order so **later overrides earlier**
  (`internal/config/config.go`):
  1. Global: `$LIT_CONFIG_GLOBAL_PATH`, else `ConfigDir()/config.toml`
     (`internal/config/config.go`). `ConfigDir()` = `$XDG_CONFIG_HOME/links-issue-tracker`
     if `XDG_CONFIG_HOME` is set, else `$HOME/.config/links-issue-tracker`; empty string if the
     home directory cannot be determined (`internal/config/config.go`).
  2. Project: `$LIT_CONFIG_PROJECT_PATH`, else `<workspaceRoot>/.lit/config.toml`
     (`internal/config/config.go`).
- A missing file is not an error; a parse error is (`parse config %s: %w`) and a merge error is
  (`merge config %s: %w`) (`internal/config/config.go`).
- An empty path (`PathSpec.IsEmpty()`) contributes nothing (`internal/config/config.go`).
- Each layer additionally contributes `ready.required_fields` **and** the legacy top-level
  `required_fields`, concatenated across layers in precedence order
  (`internal/config/config.go`); if that concatenation is non-empty it *replaces*
  `cfg.Ready.RequiredFields` (`internal/config/config.go`).

### 4.2 Complete key schema and defaults

Defaults are set in `Load` (`internal/config/config.go`).

| Key | Go type | Default | Notes / validation |
|---|---|---|---|
| `logging.verbose` | bool | `false` | `internal/config/config.go` |
| `logging.file` | string | `""` | `internal/config/config.go` |
| `init.install_hooks` | bool | `true` | `internal/config/config.go` |
| `init.install_agents` | bool | `true` | `internal/config/config.go` |
| `migration.auto_apply` | bool | `false` | `internal/config/config.go` |
| `ready.required_fields` | []string | `[]` | `internal/config/config.go`; also fed by legacy top-level `required_fields` |
| `quickstart.soil_mode` | bool | `false` | `internal/config/config.go` |
| `snapshot.retention_budget` | int | `5` | `internal/config/config.go`; **must be > 0** or `Load` fails with `config: snapshot.retention_budget must be > 0, got %d` |
| `sync.cadence` | string enum | `"on-change"` | `internal/config/config.go`; legal values `on-push`, `on-change`; invalid ⇒ `config: sync.cadence must be one of on-push, on-change, got %q` |
| `sync.receive` | bool | `true` | `internal/config/config.go` |
| `sync.owner_notify_cmd` | string | `""` | `internal/config/config.go`; empty means no owner-notification channel |
| `claims.freshness_window` | duration string | `"6h"` | `internal/config/config.go`; read as a **string** (`v.GetString`) and parsed by `time.ParseDuration` |

- `claims.freshness_window` is explicitly excluded from struct-tag decoding (`mapstructure:"-"`,
  `internal/config/config.go`). A bare number fails with
  `config: claims.freshness_window must be a duration with a unit, like "72h" or "90m" (got %q): %w`
  (`internal/config/config.go`); a non-positive duration fails with
  `config: claims.freshness_window must be positive, got %s` (`internal/config/config.go`).
- Decode failure of the whole config yields `decode config: %w` (`internal/config/config.go`).

### 4.3 Sync cadence semantics

- `on-push`: mirrors only when the managed pre-push git hook runs
  (`internal/config/config.go`).
- `on-change` (default): mirrors after every mutating `lit` command
  (`internal/config/config.go`).
- The default literal is chosen independently of the ordering of `syncCadences`
  (`internal/config/config.go`).
- `shouldSyncAfterMutation` returns true only for `AccessWrite` + `on-change`
  (`internal/cli/sync_cadence.go`).
- `maybeAutoSyncAfterCommand` (`internal/cli/sync_cadence.go`): returns immediately when
  `LIT_DISABLE_AUTO_SYNC` is truthy; loads config (unreadable ⇒
  `lit: automatic sync skipped, config unreadable: %v` on stderr and return); runs
  `ensureMirrorCoverage` when the cadence says so; runs `scheduleReceive` when `sync.receive`;
  runs `compactInline` when the access mode was write.
- Timing constants: `receiveDebounceInterval = 5 * time.Minute`
  (`internal/cli/sync_cadence.go`); `remoteAbsentRecheckInterval = 10 * time.Second`
  (`internal/cli/sync_cadence.go`).

### 4.4 Per-workspace store config (`<git-common-dir>/links/config.json`)

- Schema (`internal/workspace/workspace.go`): `workspace_id` (string),
  `issue_prefix` (string), `created_at` (RFC3339 time), `schema_version` (int).
- Created on first resolve with `WorkspaceID = uuid.NewString()`, `CreatedAt = time.Now().UTC()`,
  `Version = 1` (`internal/workspace/workspace.go`).
- `ReadConfig` fails with `read workspace config: %w`, `parse workspace config: %w`, or
  `workspace config missing workspace_id` (`internal/workspace/workspace.go`).
- Writes are atomic: temp file `.config.json.*` in the same directory, chmod 0644, close,
  rename (`internal/workspace/workspace.go`).
- `UpdateConfig(path, mutate)` is the single read-modify-write boundary
  (`internal/workspace/workspace.go`).
- Issue-prefix resolution (`internal/workspace/workspace.go`, which takes the config
  path as its second parameter so it can name that file in its own remediation) ranks three
  sources: a non-blank configured value wins and is normalized; a blank one is filled by the
  caller's explicit `PrefixRequest` if present, else by derivation. A stored value the rules
  refuse is the typed `StoredPrefixError`, whose `Unwrap` returns `ErrIssuePrefixRefused`, so
  it exits 3 with its siblings but classifies as its own reason `stored_prefix_refused`, and
  its message names the config file by path and says to edit `issue_prefix` in it — because
  no command clears that state: `lit prefix set` and `lit doctor` both resolve the workspace
  first and die in the same place. The separate reason exists for that last fact:
  `validation_refused`'s remediation ends "adjust the command to satisfy it", which is false
  for a refusal no command touches, and `stored_prefix_refused`'s names no command at all. A request that contradicts a non-blank configured value is
  refused, not applied, naming `lit prefix set <p>`, which previews the change while
  `--apply` writes it. The resolved value is persisted
  back into `config.json` immediately (`internal/workspace/workspace.go`); a value
  that came from a request persists with `derived=false`, so `lit doctor` reports
  `issue_prefix_source=configured`.
- `PrefixRequest` is the optional counterpart to `PrefixSpec`: the zero value is the absence,
  and `RequestPrefix(raw)` mints only present requests through `ConfiguredPrefix`, so an empty
  string is an error rather than a silent demotion to "no request".
- Derivation (`internal/workspace/workspace.go`): normalize `filepath.Base(rootDir)`,
  split on `-`, take the first hyphen-part that normalizes to a valid prefix, else the whole
  normalized base. Both ways of coming up short — nothing survives normalization, or too
  little does — are one failure wrapping `ErrIssuePrefixRefused` ⇒
  `issue prefix refused: repository name %q yields none (a prefix needs %d or more characters
  once punctuation is normalized away); run ``lit init --prefix <prefix>`` to set one
  explicitly`. `internal/cli` maps that sentinel to reason `validation_refused` and exit 3.

---

## 5. Workspace discovery

### 5.1 `Location` — pure path geometry (`internal/workspace/workspace.go`)

`LocationFromStorageDir(storageDir)` (`internal/workspace/workspace.go`) derives:

| Field | Value |
|---|---|
| `StorageDir` | the given dir (in practice `<git-common-dir>/links`, `internal/workspace/workspace.go`) |
| `GitCommonDir` | `filepath.Dir(storageDir)` |
| `ConfigPath` | `<storageDir>/config.json` |
| `DatabasePath` | `<storageDir>/dolt` |
| `DoltRepoPath` | `<storageDir>/dolt/links` |

### 5.2 `deriveLocation(cwd)` (`internal/workspace/workspace.go`)

1. `git rev-parse --git-common-dir` run with `cmd.Dir = cwd` (`internal/workspace/workspace.go`,
).
2. `anchorGitPath(cwd, out)` — a relative git answer is joined onto the **absolute cwd**, not the
   repo toplevel; an absolute answer is only cleaned (`internal/workspace/workspace.go`).
3. `filepath.EvalSymlinks` canonicalizes the common dir; failure ⇒
   `canonicalize git-common-dir %q: %w` (`internal/workspace/workspace.go`).
4. `LocationFromStorageDir(filepath.Join(gitCommonDir, "links"))` (`internal/workspace/workspace.go`).

### 5.3 `Resolve(cwd)` (`internal/workspace/workspace.go`, `ResolveWithPrefix`)

1. `git rev-parse --show-toplevel` ⇒ `RootDir`; failure classified by `classifyGitError`.
2. `deriveLocation(cwd)`.
3. `resolvePrivateGitDir(cwd)` = `git rev-parse --git-dir` anchored to cwd
   (`internal/workspace/workspace.go`) — **not** symlink-canonicalized
   (`internal/workspace/workspace.go`).
4. `os.MkdirAll(loc.StorageDir, 0o755)`; failure ⇒ `create storage dir: %w`
   (`internal/workspace/workspace.go`).
5. `loadOrCreateConfig(rootDir, loc.ConfigPath)`.
6. Returns `Info{Location, RootDir, WorkspaceID, IssuePrefix, PrivateGitDir}`.

All geometry git calls use `context.Background()` deliberately (`internal/workspace/workspace.go`,
).

### 5.4 Git-error classification (`internal/workspace/workspace.go`)

- Only `*exec.ExitError` with exit code **128** (`gitFatalExitCode`,
  `internal/workspace/workspace.go`) maps to the sentinel `ErrNotGitRepo`
  (`internal/workspace/workspace.go`).
- Everything else (git not on PATH, killed by signal ⇒ `ExitCode() == -1`, any other exit code)
  is wrapped with context and surfaced.

### 5.5 `Discover(roots)` (`internal/workspace/discover.go`)

- Walks every root with `filepath.WalkDir`, folding results into a map keyed by canonical
  `StorageDir`, then returns them sorted by `StorageDir`
  (`internal/workspace/discover.go`).
- Per-directory algorithm (`internal/workspace/discover.go`):
  - A walk error is fatal: `scan %q: %w`.
  - Any directory literally named `.git` ⇒ `filepath.SkipDir`.
  - Non-directories are skipped.
  - `os.Lstat(<path>/.git)`: `os.ErrNotExist` ⇒ skip; any other stat error ⇒ `stat %q: %w`
.
  - `deriveLocation(path)`: `ErrNotGitRepo` ⇒ skip; any other error ⇒ surfaced.
  - `os.Stat(loc.DatabasePath)`: `os.ErrNotExist` ⇒ skip ("git repo, no lit store"); other error
    ⇒ `stat store database %q: %w`.
  - The database path must be a **directory**; a regular file there is skipped.
  - Otherwise `byStore[loc.StorageDir] = loc` — so all worktrees of one repository collapse to
    one entry.

### 5.6 Multi-checkout enumeration (`internal/workspace/checkouts.go`)

- `Checkout{Stream, Path, Branch}` (`internal/workspace/checkouts.go`); `Branch` is empty
  for a detached HEAD (`internal/workspace/checkouts.go`).
- `LiveCheckouts(cwd)` runs `git worktree list --porcelain -z` with
  `context.Background()` (`internal/workspace/checkouts.go`). Any failure produces a
  message that always names the **git ≥ 2.36** `-z` requirement
  (`internal/workspace/checkouts.go`), and deliberately does **not** route through
  `classifyGitError` (`internal/workspace/checkouts.go`).
- Records are filtered by `uninhabited()` = `prunable || bare`
  (`internal/workspace/checkouts.go`).
- For each remaining record: `resolvePrivateGitDir(record.path)` (failure ⇒
  `locate the git directory of worktree %q, which git lists as live: %w`,
  `internal/workspace/checkouts.go`) then `ReadStream(privateGitDir)` — any failure
  fails the whole enumeration (`internal/workspace/checkouts.go`).
- Porcelain `-z` parser (`internal/workspace/checkouts.go`):
  - Fields are NUL-separated; each field is `Cut` on the **first space only**.
  - `worktree <path>` opens a record; an empty field is a record terminator
    and is skipped.
  - An attribute field before any `worktree` field ⇒
    `git worktree list --porcelain -z opened with %q, which is not a `worktree <path>` field`.
  - Recognized attributes: `branch` (stripped of `refs/heads/`), `prunable`, `bare`
. `detached`, `locked`, `HEAD`, and anything else are ignored.
  - Zero records ⇒ `git worktree list --porcelain -z named no worktrees at all`.

### 5.7 Stream identity (`internal/workspace/stream.go`)

- Token file: `lit-stream` inside the checkout's **private** git dir
  (`internal/workspace/stream.go`).
- Entropy: 8 bytes from `crypto/rand` (`internal/workspace/stream.go`), encoded
  as unpadded base32 and lowercased ⇒ 13 characters, alphabet `[a-z2-7]`
  (`internal/workspace/stream.go`).
- `ReadStream` returns the zero `StreamID` for a missing file, an error
  (`read stream id %q: %w`) for other read failures, and parses otherwise
  (`internal/workspace/stream.go`).
- `EnsureStream` reads first, publishes if absent, then re-reads and returns whatever the FILE
  holds; a token that vanishes between publish and read ⇒
  `stream id %q vanished immediately after it was written`
  (`internal/workspace/stream.go`).
- `publishStreamToken` (`internal/workspace/stream.go`): `os.CreateTemp` in the private
  git dir named `lit-stream.tmp-*`; write `token + "\n"`; `Chmod(0o644)`; `Sync()`; `Close()`;
  then `os.Link(tmp, path)`. `os.ErrExist` from the link is **success** (someone else won the
  race). Any other link error message always states the hard-link requirement
. The temp file is removed via `defer` on every path.
- `parseStreamToken` accepts exactly 13 characters from `[a-z2-7]`; otherwise
  `stream id %q is malformed: %s; delete the file to mint a fresh identity for this checkout …`
  (`internal/workspace/stream.go`).

### 5.8 Git-remote helpers (`internal/workspace/workspace.go`)

- `UpstreamRemote(ctx, cwd)`: `git rev-parse --abbrev-ref --symbolic-full-name @{upstream}`,
  first `/`-separated segment.
- `RemoteHasRefs(ctx, cwd, remote)`: `git ls-remote <remote>` non-empty.
- `RemoteDoltRefs(ctx, cwd, remote)`: the trimmed output of `git ls-remote <remote> refs/dolt/*` — one round trip, no transfer; empty when the remote carries no Dolt data.
- `RemoteHasDoltData(ctx, cwd, remote)`: `RemoteDoltRefs` non-empty
.
- `LocalRemoteHead(ctx, cwd, remote)`: `git symbolic-ref --quiet --short refs/remotes/<r>/HEAD`,
  `""` when unset; never touches the network.
- `AdvertisedRemoteHead(ctx, cwd, remote)`: `git ls-remote --symref <r> HEAD` parsed for
  `ref: refs/heads/…\tHEAD`; a failed ls-remote is returned as an error.
- `GitRemotes(ctx, cwd)`: `git remote -v`, keeps only lines whose third field is `(fetch)`,
  deduped by name and sorted by name.
- An empty/blank remote name normalizes to `origin`.

---

## 6. Version identity

`internal/version/version.go`:

- Link-time variables `Version`, `Commit`, `Date`, `Origin`. Which producer stamps
  which field is asserted against the producer files themselves in `stamp_sites_test.go`, not
  recited in-source.
- `OriginRelease = "release"` — the only value meaning "rebuilding a working tree will not
  refresh this binary"; every other value, including the empty string a bare `go build` leaves,
  reads as from-source. `OriginSource = "source"`, what both from-source entrypoints
  stamp through `scripts/version-ldflags.sh`.
- `StaleBuildThreshold = 7 * 24 * time.Hour`.
- `Info{Version, Commit, Date, IsDev, FromSource, Schema}` with JSON tags
  `version/commit/date/is_dev/schema_support`; `FromSource` is tagged `json:"-"` and never
  reaches the wire (the tag).
- `SchemaSupport{Min int64 "min", Max int64 "max"}`.
- `Get()` derives `Schema.Max` from `migrations.MaxVersion()` (one ReadDir over the embedded
  registry) and `Schema.Min` from `migrations.Baseline`; `IsDev = (Version == "")`;
  `FromSource = (Origin != OriginRelease)`.
- `BuildAge(now)` returns `(0,false)` when `Date` is empty, unparseable as RFC3339, or in the
  future; otherwise `now.Sub(stamped)`.
- `StaleSourceBuild(now)` returns that age plus the one staleness verdict every surface reads:
  `ok && FromSource && age >= StaleBuildThreshold` — the comparison is `>=`, so the boundary
  itself is stale.

`lit version` output (`internal/cli/version.go`):

- Rejects any positional argument: `usage: lit version`.
- Line 1: `lit %s (commit %s, built %s)\n`, where an `IsDev` build prints `dev`, an empty commit
  prints `unknown`, an empty date prints `unknown`.
- Line 2 (only when `BuildAge` is ok): `built %s ago\n`.
- Line 3 (only when `Info.StaleSourceBuild(now)` is true — a from-source build whose age is
  ≥ `StaleBuildThreshold`):
  `WARNING: this build is at least <threshold> old — run `just build` (or `just install`) to refresh`
  — rendered by `versionStalenessWarning` from `stalenessThresholdClause` and
  `buildRefreshRemedy`, so "at least" tracks the `>=` comparison.
- Final line: `schema versions supported: %d–%d\n`.

---

## 7. Release manifest, resolution, and self-install

### 7.1 Manifest format (`internal/release/manifest.go`)

```
Manifest = version.Info (embedded: version, commit, date, is_dev, schema_support)
         + "artifacts": [Artifact]
         + "signature": Signature (omitempty)
Artifact = {"platform": "<goos>/<goarch>", "url": string, "sha256": string}
Signature = {"algorithm": string, "value": string}
```
(`internal/release/manifest.go`.) `IsDev` always serializes `false` for published
manifests (`internal/release/manifest.go`). `Signature` is reserved and unverified today
(`internal/release/manifest.go`).

### 7.2 Target selection (`internal/release/target.go`)

- `Target{Manifest, Artifact}`.
- `CurrentPlatform()` = `runtime.GOOS + "/" + runtime.GOARCH`.
- `SelectArtifact(m, platform)` does an **exact** platform match; on miss it errors
  `release %s has no artifact for platform %s (available: %v)` listing the manifest's platforms
.

### 7.3 Manifest resolution (`internal/release/resolver.go`)

- `DefaultBaseURL = "https://github.com/promptctl/links-issue-tracker/releases/download"`
.
- URL fetched: `<base>/<tag>/release-manifest.json` (trailing `/` trimmed from base).
- Tag validation, in `acceptTag`, applied by `Resolve` before any fetch
:
  - must start with `v` ⇒ else `release: tag must be v-prefixed (got %q)`;
  - must match `^v[A-Za-z0-9._+-]+$` (`tagAcceptPattern`) ⇒ else
    `release: tag %q must match %s (v-prefix + alphanumerics, dots, dashes, underscores, plus)`;
  - must not contain `..` ⇒ else `release: tag %q contains path-traversal sequence`.
- Default HTTP client timeout `defaultResolverTimeout = 60 * time.Second` when `Client` is nil
.
- Non-200 ⇒ `release: fetch %s: HTTP %d: %s` with the first 256 body bytes.
- Body is decoded through `io.LimitReader(resp.Body, 1<<20)` into a `Manifest`;
  decode failure ⇒ `release: decode %s: %w`.
- A second `Decode` must return `io.EOF`; a second document ⇒
  `release: decode %s: unexpected trailing JSON after manifest`; any other error ⇒
  `release: decode %s: unexpected trailing data after manifest: %w`.
- Finally `SelectArtifact`.

### 7.4 Installer (`internal/release/installer.go`)

- `BinaryName = "lit"`; windows archives are expected to hold `lit.exe`
.
- Caps: `maxArchiveBytes = 256 << 20` (compressed download),
  `maxUncompressedBytes = 256 << 20` (per entry),
  `maxTotalUncompressedBytes = 2 * maxUncompressedBytes` (whole gunzip stream).
- `defaultInstallerTimeout = 5 * time.Minute` when `Client` is nil.
- `Install` sequence:
  1. `archiveFormatForURL(url)`: `.tar.gz` ⇒ tar/gzip + binary name `lit`; `.zip` ⇒ zip +
     `lit.exe`; anything else ⇒
     `release: unsupported archive extension in %q (want .tar.gz or .zip)`.
  2. Create the temp file `.lit-downgrade-*.tmp` **in the target directory first**, before
     downloading, so an unwritable install dir fails fast.
  3. `downloadAndVerify`.
  4. `extractBinary` into the temp file, `Chmod(0o755)`, `Close`, `os.Rename` into place
. Any failure before the rename removes the temp file.
- `downloadAndVerify`: GET with ctx; non-200 ⇒
  `release: fetch %s: HTTP %d: %s` (256-byte snippet); the expected SHA256 is hex-decoded
  **before** the download and must be exactly 32 bytes, else
  `release: artifact SHA256 %q is not a 64-char hex digest`; the body is read through
  `io.LimitReader(..., maxArchiveBytes+1)` teed into sha256; over-cap ⇒
  `release: archive %s exceeds %d byte cap`; mismatch ⇒
  `release: SHA256 mismatch for %s: expected %s, got %s`.
- `extractBinary` accept shape: every entry must pass `safeFlatName` (non-empty,
  not `.`/`..`, contains neither `/` nor `\` nor `..` —) else
  `release: archive entry has unsafe path: %q`; every entry must be a regular file else
  `release: archive contains non-regular entry %q`; every entry's declared size must be in
  `[0, maxUncompressedBytes]` else
  `release: archive entry %q declares %d uncompressed bytes (cap %d)`; exactly one entry named
  `format.binaryName` — two ⇒ `release: archive contains multiple %q entries`, zero ⇒
  `release: archive did not contain a %q entry`.
- `copyCappedEntry` streams with `io.CopyN(dest, body, maxUncompressedBytes+1)` and rejects an
  actual size over the cap even when the header lied:
  `release: %q exceeded uncompressed cap %d`.
- The gzip stream is wrapped in a `boundedReader` capped at `maxTotalUncompressedBytes`;
  exceeding it yields `errStreamCap` = `release: uncompressed archive stream exceeds total cap`
.
- Tar accepts both `tar.TypeReg` and `tar.TypeRegA` as regular. Zip sizes come from
  `UncompressedSize64` cast to int64, with the negative case rejected upstream.

### 7.5 `lit downgrade` (`internal/cli/downgrade.go`)

- One flag: `--to` (`downgrade: Target binary version (v-prefixed git tag, e.g. v0.4.1)`,
  `internal/cli/downgrade.go`). Any positional arg ⇒ `usage: lit downgrade --to <version>`
.
- `normalizeReleaseTag`: blank ⇒
  `ValidationError{"<verb>: --to requires a non-empty version"}`; a missing leading `v` is added;
  a value containing `/`, `\`, `..`, or whitespace ⇒
  `ValidationError{"<verb>: --to %q is not a valid release tag"}`.
- Pipeline: require the store to expose `storage.SchemaMigration`;
  resolve `Target` via `release.HTTPResolver{}` at `release.CurrentPlatform()`;
  `store.Downgrade(ctx, target.Manifest.Schema.Max)`; resolve the running binary via
  `currentBinaryPath()` = `os.Executable()` + `filepath.EvalSymlinks`;
  `release.HTTPInstaller{}.Install(ctx, target, binPath)`.
- An install failure after a successful schema reversal produces a message naming both recovery
  paths (manual download from the artifact URL, or `lit snapshots list` + `lit snapshots restore`)
.
- Success prints
  `downgraded to %s (schema v%d) installed at %s\nre-run \`lit version\` to confirm.\n`.
  No re-exec is performed.

### 7.6 `scripts/install.sh` (492 lines)

Three modes, one target-resolution rule (`scripts/install.sh`):

- Flags: default (source build); `--from-release <tag>`; `--latest-release`; `-h|--help`
  (`scripts/install.sh`). `--from-release` and `--latest-release` are mutually exclusive;
  combining them errors `error: cannot combine <flag> with <flag>` and prints
  `usage: $0 [--from-release <tag>|--latest-release]`. `--from-release` with no
  argument ⇒ `error: --from-release requires a tag (e.g. v0.1.0)`.
- Constants: `REPO_DOWNLOAD_BASE=https://github.com/promptctl/links-issue-tracker/releases/download`
, `REPO_LATEST_API=https://api.github.com/repos/promptctl/links-issue-tracker/releases/latest`
.
- Binary name is `lit`, or `lit.exe` on windows, detected once from `uname -s`.
- `realpath_compat` cascade: `realpath` → `readlink -f` → a pure-shell link walk; no `python3`
.
- **Target directory priority**: an existing `lit` on PATH (via `type -P`,
  symlink-resolved, dirname) → `$GOBIN` → `go env GOBIN` → `go env GOPATH` first entry + `/bin`
  → `$HOME/.local/bin`. If `HOME` is unset at that last step it errors
  `error: cannot determine install directory — $HOME is unset …`.
  `mkdir -p "$TARGET_DIR"`.
- **Source mode**: `ver=$(git describe --tags --always --dirty)` with a leading `v`
  stripped; empty stays empty so `IsDev` remains true; sources `scripts/cgo-env.sh` and
  `scripts/version-ldflags.sh`; builds with
  `GOFLAGS=…-buildvcs=false go build -ldflags "-X <pkg>.Version=… -X <pkg>.Commit=… -X <pkg>.Date=… -X <pkg>.Origin=…" -o "$TARGET_DIR/$BIN_NAME" ./cmd/lit`.
- **Release/latest mode**:
  - Requires `curl`; `--latest-release` additionally requires `jq` and reads
    `.tag_name` from the GitHub API.
  - Tag normalization: strip then re-add a leading `v`, then require
    `^v[0-9]+\.[0-9]+\.[0-9]+$` else
    `error: release tag '<tag>' is not a canonical semver release tag (expected vX.Y.Z)`.
  - Archive name `lit_${tag#v}_${os}_${arch}.${ext}`; arch map
    `x86_64|amd64→amd64`, `arm64|aarch64→arm64`, else
    `error: unsupported architecture`; OS map `linux|darwin→tar.gz`,
    `mingw*|msys*|cygwin*→windows/zip`, else `error: unsupported OS`.
  - Extractor probes: `tar` for `.tar.gz`, `unzip` for `.zip`.
  - Downloads `<base>/<tag>/<archive>` and `<base>/<tag>/checksums.txt` into a temp dir created
    **inside `$TARGET_DIR`** (`mktemp -d "$TARGET_DIR/.lit-install.XXXXXX"`) so the final `mv` is
    atomic; `trap rm -rf` on EXIT.
  - Expected checksum is extracted with `awk '$2 == want'` (exact field match, not grep)
; digest computed by `sha256sum` or `shasum -a 256`, else an explicit error
    naming both tools; a mismatch prints expected/actual and exits 1.
  - **Structural archive validation before extraction**: tar entry names must be
    flat (`.`/`..`/`*/*`/`/*` rejected) and `tar -tvzf` column 1 must be `-` (regular) for every
    line; zip names are checked for `/`, `\`, `.`, `..`, leading `/`.
  - Extract, then reject a symlink at the binary path
    (`error: extracted '<bin>' is a symlink; archive rejected`), require a regular
    file, `chmod +x`, require executable, then
    `mv -f "$tmp/$BIN_NAME" "$TARGET_DIR/$BIN_NAME"`.
- Post-install, unconditional: removes any stale `lnks`/`lnks.exe` in the target
  dir; walks every `PATH` entry, canonicalizing each `lit` candidate, and collects those
  whose realpath differs from the just-installed binary; prints
  `Installed lit -> <path>` and runs `<installed> version` (errors ignored); prints a
  `WARNING: other 'lit' binaries found on PATH that were NOT updated:` block listing each
.

### 7.7 `scripts/version-ldflags.sh`

- Must be sourced; executing it prints a message and exits **64**.
- `LIT_BUILD_COMMIT="$(git rev-parse --short HEAD)"`; empty ⇒ message and `return 1`
. `LIT_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"`.
  `LIT_BUILD_ORIGIN="source"`, the constant both from-source entrypoints stamp so
  `internal/version.FromSource` reads true for them. All three exported.
- Deliberately never sets `Version`.

### 7.8 `scripts/cgo-env.sh`

- Must be sourced; executing it exits **64**.
- Off macOS it exports nothing.
- On Darwin: requires `brew` (else message + `return 1`); prefers
  `brew --prefix icu4c@78`, falling back to `brew --prefix icu4c`; also `brew --prefix zstd`
; verifies `include/unicode/regex.h` and `include/zstd.h` exist, else
  `cgo-env: ICU headers not found. Run: brew install icu4c@78` /
  `cgo-env: zstd headers not found. Run: brew install zstd`; exports
  `CGO_CPPFLAGS="-I<icu>/include -I<zstd>/include …"` and
  `CGO_LDFLAGS="-L<icu>/lib -L<zstd>/lib …"`, preserving any caller-set values.

### 7.9 `scripts/next-version.sh`

- Usage `scripts/next-version.sh <minor|patch>`; wrong argument count ⇒ exit **2**;
  a bump other than `minor`/`patch` ⇒ exit **2** with
  `bump must be 'minor' or 'patch' (major is frozen for this repo)`.
- Latest clean tag = `git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | grep -vE '-' | head -1`
; none ⇒ exit **3**; non-`vX.Y.Z` shape ⇒ exit **3**.
- `minor` ⇒ MINOR+1, PATCH=0; `patch` ⇒ PATCH+1. MAJOR is never bumped.
- If the computed tag already exists ⇒ exit **4** with
  `computed next tag already exists: <tag> (is your master up to date?)`.
- Otherwise prints the tag on stdout.

### 7.10 Other scripts present

`scripts/cleanroom-modpath.sh`, `scripts/cleanroom-reach-probe.sh`,
`scripts/cleanroom-sandbox.sh` exist in the repo (`scripts/` listing) and are not referenced by
the Justfile or any workflow file in `.github/workflows/`.

---

## 8. Justfile targets

`Justfile` — every recipe sources `scripts/cgo-env.sh` before compiling (`Justfile`).

| Target | Behavior |
|---|---|
| `default` | `just --list` (`Justfile`) |
| `setup` | On Darwin: requires Homebrew (`Install Homebrew first: https://brew.sh`, exit 1) then `brew install icu4c@78 zstd`; then sources `cgo-env.sh` and, if `CGO_CPPFLAGS` is set, persists `CGO_CPPFLAGS`/`CGO_LDFLAGS` via `go env -w`; otherwise prints that no extra flags are needed (`Justfile`) |
| `build` | `go build -buildvcs=false -ldflags "-X <version pkg>.Commit=$LIT_BUILD_COMMIT -X <version pkg>.Date=$LIT_BUILD_DATE -X <version pkg>.Origin=$LIT_BUILD_ORIGIN" ./cmd/lit` — deliberately does **not** stamp `Version` (`Justfile`) |
| `test-short` | `go test -short ./...` (`Justfile`) |
| `test *args` | `go test -timeout 30m ${args:-./...}`; a later `-timeout` in args wins (`Justfile`) |
| `lint` | `golangci-lint run` (`Justfile`) |
| `install` | `./scripts/install.sh` (`Justfile`) |

---

## 9. Repo tooling (`tools/`)

### 9.1 `tools/testbudget`

- Invoked as `go test -short -json ./... | go run ./tools/testbudget`
  (`tools/testbudget/main.go`, `.github/workflows/ci.yml`).
- Reads `test2json` events (`Action`, `Package`, `Test`, `Elapsed`, `Output`) from stdin
  (`tools/testbudget/main.go`), replays package result lines as they arrive and a failed
  test's buffered output, and reports unfinished tests when the stream ends
  (`tools/testbudget/main.go`).
- Prints a per-package `elapsed / budget` table headed
  `testbudget: per-package wall clock vs budget (budgets: tools/testbudget/budgets.go)`
  (`tools/testbudget/main.go`).
- Violation line: `test runtime budget exceeded: %s took %.1fs, budget is %ds (over by %.1fs, +%.0f%%) — see tools/testbudget/budgets.go before raising the number`
  (`tools/testbudget/main.go`), prefixed `::error::` under GitHub Actions
  (`tools/testbudget/main.go`).
- Exit codes: **2** on an unreadable stream (`tools/testbudget/main.go`), **1** when any
  budget is exceeded, 0 otherwise. Test pass/fail is left to `go test` under
  `pipefail` (`tools/testbudget/main.go`).
- Budgets (`tools/testbudget/budgets.go`):
  - `internal/store` — 210 s
  - `internal/cli` — 340 s
  - `cmd/lit` — 50 s
  - `tools/licenses` — 50 s
  - every other package — `defaultBudget = 30 * time.Second` (`tools/testbudget/budgets.go`)

### 9.2 `tools/mkmanifest`

- Flags (`tools/mkmanifest/main.go`): `-version` (v-stripped, required), `-tag`
  (v-prefixed, required), `-commit` (required), `-date` (RFC3339, required), `-dist`
  (default `dist`), `-base-url` (required), `-out` (required).
- All required flags are trimmed in place and checked in a **fixed order** so the first-missing
  diagnostic is reproducible; missing ⇒ `mkmanifest: required flag <name> missing` and exit 1
  (`tools/mkmanifest/main.go`). `-dist` is trimmed too.
- `validateVerTag` (`tools/mkmanifest/main.go`): `-tag` must start with `v`; `-version`
  must **not**; `-tag` must contain no `/`, `\`, `..`, or whitespace.
- Schema range comes from `migrations.Baseline` and `migrations.MaxVersion()`
  (`tools/mkmanifest/main.go`); `IsDev` is hard-coded `false`, and
  `FromSource` is stated `false` beside it but cannot reach the file (rationale).
- `collectArtifacts` (`tools/mkmanifest/main.go`) parses `<dist>/checksums.txt`:
  - lines are split on exactly two spaces; otherwise
    `%s:%d malformed (want '<sha256>  <filename>'): %q`;
  - the digest must be 64 hex chars, else a length or `sha256 not hex` error;
  - the filename must have no path components, else `filename has unsafe path shape`;
  - non-archive rows are skipped silently;
  - each referenced archive must exist on disk, else
    `%s:%d references archive %q but %s: %w`;
  - URL = `<base-url>/<tag>/<filename>`;
  - artifacts sorted by platform; zero artifacts ⇒
    `no per-platform artifacts found in <checksums.txt>`.
- `platformFromFilename` (`tools/mkmanifest/main.go`) accepts exactly
  `lit_<version>_<goos>_<goarch>.{tar.gz,zip}` — four underscore-separated parts, literal
  project prefix `lit`, and the version segment must equal `-version`.
- Output is written with `json.Encoder` at two-space indent, and `Close()` is checked explicitly
  on the success path (`tools/mkmanifest/main.go`).

### 9.3 `tools/licenses`

- Flags (`tools/licenses/main.go`): `-pkg` (default `./cmd/lit`), `-bundle` (default
  `THIRD_PARTY_LICENSES`), `-report` (default `LICENSE-REPORT.md`), `-sbom` (default empty ⇒
  skip), `-app-version` (default empty ⇒ omit), `-check` (bool), `-graph` (bool).
- `selectMode` (`tools/licenses/main.go`): `-check` and `-graph` together ⇒
  `-check and -graph select different operations; run the tool once for each`; otherwise
  `modeCheck`, `modeGraph`, or `modeGenerate`.
- Modes (`tools/licenses/main.go`):
  - **generate** (default): resolves `go list -deps <pkg>` plus curated native C libraries,
    classifies once, and writes bundle + report (+ SBOM when `-sbom` is given).
  - **`-check`**: the CI license-policy gate over the link closure against
    `tools/licenses/policy.json`; writes no artifacts; non-zero exit on any violation.
  - **`-graph`**: audits `go list -m all` (the whole build list); writes no artifacts and does
    not gate.
- Any error ⇒ `licenses: %v` on stderr and exit **1** (`tools/licenses/main.go`).
- The graph acceptance test is env-gated on `LIT_LICENSE_GRAPH_AUDIT`
  (`tools/licenses/graph_test.go`).

### 9.4 `tools/session-analysis`

Present in the tree (`tools/session-analysis/`), with a `processed/` directory that is gitignored
(`.gitignore`) and `__pycache__/` ignored (`.gitignore`). It is not referenced by the
Justfile or any workflow in `.github/workflows/`.

---

## 10. Lint, attributes, dependency updates

- `.golangci.yml`: `version: "2"`; `linters.default: none`; the **only** enabled linter is
  `depguard` (`.golangci.yml`). Its single rule `lifecycle-boundary` runs in `list-mode: lax`
  over files matching `!**/internal/model/**` and denies importing
  `github.com/promptctl/links-issue-tracker/internal/model/lifecycle` with the message
  "lifecycle internals are owned by internal/model; use model hydration and capability APIs"
  (`.golangci.yml`).
- `.gitattributes`: `*.sql text eol=lf` — forces LF normalization so byte-comparison gates
  (a sha256-pinned baseline migration and a schema-snapshot drift canary) cannot be tripped by
  CRLF (`.gitattributes`).
- `.github/dependabot.yml`: `gomod` ecosystem at `/`, `weekly` schedule, with a `go-safe` group
  covering `minor` and `patch` update types; majors match no group and arrive as individual PRs
  (`.github/dependabot.yml`).
- `.gitignore` ignores `artifacts/`, `site/`, `.claude/worktrees/`,
  `tools/session-analysis/processed/`, `__pycache__/`, `.claude/*.lock`, the three build outputs
  `/lit`, `/licenses`, `/mkmanifest`, and the three generated compliance artifacts
  `/THIRD_PARTY_LICENSES`, `/LICENSE-REPORT.md`, `/SBOM.cdx.json` (`.gitignore`).

---

## 11. CI workflows

### 11.1 `.github/workflows/ci.yml` — "CI"

- Triggers: `push` to `master`, `pull_request` targeting `master`
  (`.github/workflows/ci.yml`).
- `permissions: contents: read`. Concurrency group `ci-${{ github.ref }}` with
  `cancel-in-progress: true`. Linux (`ubuntu-latest`) only, no build matrix
.
- Five jobs, all in parallel:
  1. **`build-and-test`**: checkout@v4 → setup-go@v5 (`go-version-file: go.mod`) →
     `./.github/actions/install-dolt` → `go build ./cmd/lit` →
     `go test -short -json ./... | go run ./tools/testbudget` under `shell: bash` (pipefail is
     load-bearing). No `-timeout` override by policy.
  2. **`race`**: same setup; `go test -short -race -timeout 30m ./cmd/... ./internal/...` —
     scope excludes `tools/` deliberately; no testbudget pipe. The `-timeout 30m`
     sits above internal/cli's race-inflated testbudget ceiling (~21m), so the
     lane fails on a race or a hang, never on slowness.
  3. **`verify`**: `go mod tidy` then `git diff --exit-code go.mod go.sum`, failing
     with `::error::go.mod/go.sum are not tidy…`; `grep -q -- "-buildvcs=false" scripts/install.sh`
; `bash scripts/install.sh`.
  4. **`lint`**: `golangci/golangci-lint-action@v8` pinned to `version: v2.8.0`.
  5. **`docs`**: setup-python@v5 `3.x`; `pip install mkdocs==1.6.1 mkdocs-material==9.7.6`;
     `mkdocs build --strict`.

### 11.2 `.github/actions/install-dolt/action.yml`

Composite action; installs the pinned Dolt CLI used as a **test-only** oracle
(`.github/actions/install-dolt/action.yml`): downloads
`https://github.com/dolthub/dolt/releases/download/v1.81.10/install.sh`, runs it with `sudo bash`,
then `dolt version`.

### 11.3 `.github/workflows/nightly.yml` — "Nightly full suite"

- Triggers: `schedule: cron '17 9 * * *'` and `workflow_dispatch`
  (`.github/workflows/nightly.yml`).
- `permissions: contents: read, issues: write`. Concurrency group
  `nightly-full-suite`, `cancel-in-progress: false`.
- Single job `full-suite`, `ubuntu-latest`, `timeout-minutes: 60`.
- Steps: checkout → setup-go → install-dolt → `go build ./cmd/lit` →
  `go test -timeout 30m ./...` (no `-short`, so `testing.Short()` is false).
- Failure reporting step runs `if: failure() || cancelled()` — the `cancelled()` arm exists
  because a job timeout is reported as cancelled. With `GH_TOKEN=github.token`, it
  `gh label create nightly-failure --force --description "The scheduled full test lane is red" --color B60205`,
  looks for an open issue with that label, and either comments on it or creates
  `Nightly full test lane is failing` with the run URL.

### 11.4 `.github/workflows/license-graph-audit.yml` — "License graph audit"

- Triggers: `schedule: cron "0 9 * * 1"` (Mondays 09:00 UTC) and `workflow_dispatch`
  (`.github/workflows/license-graph-audit.yml`). `permissions: contents: read`.
- Explicitly **not** a merge gate and not wired into ci.yml; it reports and exits 0
.
- Job `audit` on `ubuntu-latest`: checkout → setup-go with `cache: false` (cold cache
  is the acceptance criterion) → `go test ./tools/licenses/ -run TestGraph -v` with
  `LIT_LICENSE_GRAPH_AUDIT: "1"` and **no** `-timeout` override →
  `go run ./tools/licenses -graph` → assert `git diff --exit-code go.mod go.sum`,
  failing with `::error::the license graph audit modified go.mod/go.sum — withGoSumPreserved is not holding.`
.

### 11.5 `.github/workflows/code-review.yml` — "AI Code Review"

- Header states the file is **generated** by `agent-code-review-setup/install.sh` and is
  reconverged (overwriting local edits) on every installer run
  (`.github/workflows/code-review.yml`).
- Trigger: `pull_request` with types `[opened, synchronize, reopened, ready_for_review]`
 — deliberately `pull_request`, not `pull_request_target`.
- `permissions: contents: read, issues: write, pull-requests: write`.
- Concurrency `code-review-${{ github.event.pull_request.number }}`, `cancel-in-progress: true`
.
- Job `review` ("Review") on `ubuntu-latest`, `timeout-minutes: 30`.
- Steps: `actions/checkout` pinned to SHA `d23441a48e516b6c34aea4fa41551a30e30af803` (v6),
  checking out `github.event.pull_request.head.sha` with `persist-credentials: false`
; then `promptctl/copirate-code-review-agent@v1` (moving `v1` tag by design,
) with inputs `CLAUDE_CODE_OAUTH_TOKEN`, `DEEPSEEK_API_KEY`, and
  `DEPENDENCY_DIFF: "true"`. The comment records that both credentials must exist in
  both the Actions and the Dependabot secret stores.

### 11.6 `.github/workflows/release-smoke.yml` — "Release smoke"

- Trigger: `pull_request` targeting `master` only (`.github/workflows/release-smoke.yml`).
- `permissions: contents: read, packages: read`. Concurrency
  `release-smoke-${{ github.ref }}`, `cancel-in-progress: true`.
- Job `smoke` on `ubuntu-latest`. Steps:
  1. checkout@v4 with `fetch-depth: 0` (goreleaser derives the snapshot version from git,
).
  2. setup-go@v5 with `cache: true`.
  3. `SMOKE_GOCACHE=$RUNNER_TEMP/smoke-gocache` written into `$GITHUB_ENV`.
  4. `go mod download` to populate `GOMODCACHE` before the read-only mount.
  5. `docker/setup-buildx-action@v3`.
  6. `tree=$(git rev-parse HEAD:build)` — the toolchain image tag is the **git tree hash of
     `build/`**.
  7. `docker/login-action@v3` to `ghcr.io` with `github.actor` / `secrets.GITHUB_TOKEN`
.
  8. `docker pull ghcr.io/<owner>/lit-release-builder:smoke-<tree>` and retag to
     `lit-release-builder:smoke`, with `continue-on-error: true`.
  9. Fallback `docker/build-push-action@v6` on `steps.pull.outcome == 'failure'`, building
     `build/Dockerfile.release` `target: smoke`, `load: true`, GHA cache scope
     `release-builder`.
  10. `actions/cache@v4` for `$SMOKE_GOCACHE`, key `smoke-gocache-<hash(go.sum)>-<sha>` with
      restore-keys `smoke-gocache-<hash(go.sum)>-` then `smoke-gocache-`.
  11. `docker run --rm` mounting the repo at `/go/src/app`, `GOMODCACHE` at `/go/pkg/mod:ro`,
      the cache dir at `/gocache` with `GOCACHE=/gocache`, invoking
      `build --single-target --snapshot --clean`; then `sudo chown -R` the cache back to the
      runner user.
  12. Static/execution assertions on the produced binary: fail if `readelf -l`
      shows `INTERP`, fail if `readelf -d` shows `NEEDED`, then run `./<bin> version` on the
      glibc runner and `docker run --rm … alpine:3.20 lit version` on musl.

### 11.7 `.github/workflows/release-validate.yml` — "Release validate"

- Triggers: `push` to `master` and `workflow_dispatch: {}` — never on `pull_request`
  (`.github/workflows/release-validate.yml`).
- `permissions: contents: read, packages: write`. Concurrency
  `release-validate-${{ github.ref }}` with `cancel-in-progress: false`.
- This workflow **is** the release pipeline: when the newest CHANGELOG version has no tag, the
  `validate` job builds the real version and `publish` cuts the tag.

**Job `license-gate`**: `ubuntu-latest`, job-level `env: CGO_ENABLED: "1"` (required
so `go list -deps` resolves the same set the release links). Steps: checkout@v4 →
setup-go@v5 (`cache: true`) → `go mod download` → `go run ./tools/licenses -check -pkg ./cmd/lit`.

**Job `validate`**, `ubuntu-latest`, with job outputs
`release` and `version` from `steps.kind`:

1. checkout@v4 `fetch-depth: 0`; setup-go@v5 `cache: true`.
2. `SMOKE_GOCACHE=$RUNNER_TEMP/smoke-gocache` into `$GITHUB_ENV`.
3. `go mod download`.
4. **`Resolve build kind`** (`id: kind`): requires `CHANGELOG.md` to exist
   (`::error::CHANGELOG.md not found at repo root…`, exit 1); takes the first `## [` heading that
   is not `## [Unreleased]`; none ⇒ `release=false`; a heading not matching
   `^## \[[0-9]+\.[0-9]+\.[0-9]+\]( - .*)?$` ⇒ `::error::newest CHANGELOG release heading is malformed`
   and exit 1; otherwise `git ls-remote --exit-code --tags origin refs/tags/v$VER` with a
   three-way branch on the exit code — `0` ⇒ `release=false`, `2` ⇒ `release=true` +
   `version=$VER`, anything else ⇒ `::error::git ls-remote failed (exit $r)…` and exit 1.
5. **`Generate third-party license bundle, report + SBOM`**:
   `go run ./tools/licenses -pkg ./cmd/lit -bundle THIRD_PARTY_LICENSES -report LICENSE-REPORT.md -sbom SBOM.cdx.json -app-version "<kind.version>"`.
6. `docker/setup-buildx-action@v3`; `docker/setup-qemu-action@v3` with
   `platforms: arm64`.
7. `tree=$(git rev-parse HEAD:build)`; GHCR login.
8. `docker/build-push-action@v6` building `build/Dockerfile.release` (default `final` target)
   as `lit-release-builder:local`, `load: true`, GHA cache scope `release-builder`
.
9. `goreleaser check` inside the image.
10. `actions/cache@v4` seeding the same `smoke-gocache-*` namespace release-smoke restores
.
11. **`Build artifacts`**: `ARGS="release --snapshot --clean"`; for a pending
    release it first creates the ephemeral local tag `git tag "v<version>"` and switches to
    `ARGS="release --clean"`. The container mounts the repo, `GOMODCACHE:ro`, and the GOCACHE
    dir; then `sudo chown -R` the cache back.
12. **`Generate release manifest`**: `sudo chown -R` `dist`, then reads
    `.version`, `.tag`, `.commit` (first 7 chars), `.date` from `dist/metadata.json` and runs
    `go run ./tools/mkmanifest -version … -tag … -commit … -date … -dist ./dist -base-url https://github.com/<repo>/releases/download -out ./dist/release-manifest.json`.
13. **`Assert manifest shape`**: jq assertions that `.version` is a non-empty
    string; `.schema_support.min` is a number ≥ 1 and `.schema_support.max` is a number;
    `.artifacts` is non-empty with every `.platform` matching `^[a-z0-9]+/[a-z0-9]+$`, every
    `.url` starting `https://github.com/` and containing `/<tag>/`, and every `.sha256` matching
    `^[0-9a-f]{64}$`; and the sorted platform list equals the literal
    `["darwin/amd64","darwin/arm64","linux/amd64","linux/arm64","windows/amd64"]`, restated
    independently of `.goreleaser.yml` on purpose.
14. **`Assert release archive carries the license bundle + report`**: asserts
    exactly one `dist/lit_*_linux_amd64.tar.gz`, extracts `THIRD_PARTY_LICENSES`,
    `LICENSE-REPORT.md`, `FORKS.md`, and greps for the dolt module row, an `Apache License`
    within 12 lines of dolt's bundle header, the four native-library rows
    (`| icu | 75.1 | Unicode-3.0 |`, `| zstd | 1.5.6 | BSD-3-Clause |`,
    `| musl | 1.2.5 | MIT |`, `| compiler-rt | 0.14.0 | MIT AND Apache-2.0 WITH LLVM-exception |`)
    with a notice substring for each, the `| Module | Version | License | Source |` header, all
    three `replace` disclosures (promptctl/dolt, promptctl/go-mysql-server,
    `./internal/vendor/dolthub-driver`) in both artifacts, and a non-empty `FORKS.md`.
15. **`Stage the SBOM as a versioned release asset`** — `if: steps.kind.outputs.release == 'true'`
: copies `SBOM.cdx.json` to `dist/lit_<version>_sbom.cdx.json` and fails if
    `.metadata.component.version` differs from the filename version.
16. **`Validate the CycloneDX SBOM…`** — release-only: downloads
    `cyclonedx-linux-x64` at pinned `CYCLONEDX_CLI_VERSION: v0.27.2` and verifies it against
    `CYCLONEDX_CLI_SHA256: 5e1595542a6367378a3944bbd3008caab3de65d572345361d3b9597b1dbbaaa0`
    with `sha256sum -c`, runs `validate --input-file … --input-format json --fail-on-errors`,
    then jq-asserts that `github.com/dolthub/dolt/go` appears exactly once with a non-empty
    version and carries a pedigree whose single descendant purl starts
    `pkg:golang/github.com/promptctl/dolt/go@`.
17. **`Assert linux binaries are static and run on glibc + musl (amd64 + arm64)`**:
    for both arches, no `INTERP`, no `NEEDED`, and `docker run --platform linux/<arch> … alpine:3.20 lit version`;
    plus `lit version` natively for amd64.
18. **`Upload validated release artifact`** — release-only:
    `actions/upload-artifact@v4` name `release-dist-<sha>`, paths
    `dist/lit_*_*_*.tar.gz`, `dist/lit_*_*_*.zip`, `dist/lit_*_sbom.cdx.json`,
    `dist/checksums.txt`, `dist/release-manifest.json`, `dist/metadata.json`;
    `retention-days: 14`; `if-no-files-found: error`.
19. **`Publish release-builder smoke image to GHCR`**: pushes
    `ghcr.io/<owner>/lit-release-builder:smoke-<tree>` built at `target: smoke`.
20. **`Upload snapshot dist/ for inspection`** — `if: always()`, name
    `release-validate-dist-<sha>`, `retention-days: 7`.

**Job `publish`**: `needs: [validate, license-gate]`,
`if: needs.validate.outputs.release == 'true'`, `ubuntu-latest`,
`permissions: contents: write`. Steps:

- `actions/download-artifact@v4` name `release-dist-<sha>` into `_artifact`.
- Locate the real dist dir by `find _artifact -name metadata.json`, requiring exactly one, and
  export `DIST`.
- Verify `.commit` in `metadata.json` equals `github.sha` and `.tag` equals
  `v<needs.validate.outputs.version>`.
- `gh release create "$TAG" --repo <repo> --target <sha> --title "$TAG" --generate-notes <assets…>`
  with `GH_TOKEN: secrets.GITHUB_TOKEN`, where the asset list is assembled from files that
  actually exist (`nullglob` plus a `-f` test) and an empty list is refused with
  `::error::no release assets found under $DIST — refusing to publish an empty release`; a
  failing `gh release create` prints a recovery instruction naming
  `gh release delete $TAG --repo … --cleanup-tag --yes`.

---

## 12. `.goreleaser.yml`

- `version: 2`, `project_name: lit` (`.goreleaser.yml`). No `before.hooks` — the
  removed `go mod tidy` hook is called out at `.goreleaser.yml`.
- One build (`.goreleaser.yml`): `id: lit`, `main: ./cmd/lit`, `binary: lit`.
  - `env` sets `CGO_ENABLED=1` and, by Go template on `.Os`/`.Arch`, the zig cross
    wrappers: `zig-cc-aarch64-apple-darwin` / `zig-cc-x86_64-apple-darwin` (+ `CXX` twins)
    for darwin, `zig-cc-x86_64-windows-gnu` (+ CXX) for windows, and
    `zig-cc-x86_64-linux-musl` / `zig-cc-aarch64-linux-musl` (+ CXX) for linux.
  - `CGO_CPPFLAGS=-I/opt/icu/{{ .Os }}_{{ .Arch }}/include`.
  - `CGO_LDFLAGS=-L/opt/icu/{{ .Os }}_{{ .Arch }}/lib -static` on linux; without `-static`
    elsewhere.
  - `GOFLAGS=-tags=icu_static`.
  - `goos: [linux, darwin, windows]`, `goarch: [amd64, arm64]`, with `windows/arm64` ignored
 ⇒ five targets.
  - `flags: [-trimpath, -buildvcs=false]`.
  - `ldflags: -s -w` plus
    `-X …/internal/version.Version={{ .Version }}`,
    `-X …/internal/version.Commit={{ .ShortCommit }}`,
    `-X …/internal/version.Date={{ .Date }}`,
    `-X …/internal/version.Origin=release`.
- Archives: `name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`
  — required to match mkmanifest's parser; `formats: [tar.gz]` with a windows
  override to `[zip]`; `wrap_in_directory: false`; `files:` `LICENSE`,
  `README*`, `THIRD_PARTY_LICENSES`, `LICENSE-REPORT.md`, `FORKS.md`.
- Checksums: `name_template: "checksums.txt"`, `algorithm: sha256`.
- Snapshot version template: `"{{ incpatch .Version }}-snapshot+{{.ShortCommit}}"`.
- `release: disable: true` — goreleaser never publishes; the workflow's `publish` job does
.
- `changelog: disable: true`.

---

## 13. Release build image (`build/`)

### 13.1 `build/Dockerfile.release`

Pinned ARGs (`build/Dockerfile.release`):

| ARG | Value |
|---|---|
| `GO_VERSION` | `1.25.7` |
| `GORELEASER_VERSION` | `v2.16.0` |
| `GORELEASER_SHA256` | `eaae05b5eba07533bd0f06846b68c808399504784df00c62eb219541fc04e5e2` |
| `ZIG_VERSION` | `0.14.0` |
| `ZIG_SHA256` | `473ec26806133cf4d1918caf1a410f8403a13d979726a9045b421b685031a982` |
| `ICU_MAJOR` / `ICU_MINOR` | `75` / `1` |
| `ICU_SHA256` | `cb968df3e4d2e87e8b11c49a5d01c787bd13b9545280fc6642f826527618caef` |
| `MACOS_SDK_VERSION` | `13.3` |
| `MACOS_SDK_SHA256` | `518e35eae6039b3f64e8025f4525c1c43786cc5cf39459d609852faf091e34be` |

Stages:

- **`base`** = `golang:${GO_VERSION}-bookworm`: apt packages, goreleaser and zig
  downloads, a `zig-ar` shim (`printf '#!/bin/sh\nexec zig ar "$@"\n'`),
  generated `zig-cc-*`/`zig-cxx-*` wrappers, the ICU source download, and
  `build/icu-cross-build.sh` installed as `/usr/local/bin/icu-cross-build`.
- **`native-icu`**: a native ICU build used as `--with-cross-build`.
- **`cross-linux-amd64`**, **`cross-linux-arm64`**, **`cross-windows-amd64`** (
) — parallel `icu-cross-build <goos> <goarch> …` stages.
- **`darwin-toolchain`**: downloads the pinned macOS SDK, copies
  `build/zig-macos-stubs/tzfile.h` to `/usr/local/include/zig-macos-stubs/tzfile.h`,
  and generates the darwin wrappers with
  `SDK_FLAGS="-F/opt/macos-sdk/System/Library/Frameworks -L/opt/macos-sdk/usr/lib"`.
- **`cross-darwin-arm64`**, **`cross-darwin-amd64`**.
- **`smoke`**: `FROM base`, `SHELL ["/bin/bash","-c"]`, copies only
  `/opt/icu/linux_amd64`, verifies `include/unicode/regex.h` and a `libicuuc.a`
, `WORKDIR /go/src/app`,
  `git config --global --add safe.directory /go/src/app`, and
  `ENTRYPOINT ["/usr/local/bin/goreleaser"]`.
- **`final`**: `FROM smoke`, adds the remaining four ICU prefixes, the
  macOS SDK plus a re-created `/opt/macos-sdk` symlink, the tzfile stub,
  and the four darwin wrappers. Two build-time verification `RUN`s: all four extra
  ICU prefixes carry `unicode/regex.h` and a `libicuuc|sicuuc` archive, and the
  darwin link inputs resolve — `CoreFoundation.framework` present, all four wrappers executable,
  the tzfile stub present.

### 13.2 `build/icu-cross-build.sh`

- Positional args: `goos`, `goarch`, host triplet, `CC`, `CXX`, `AR`, `RANLIB`, optional
  `extra_cppflags` (`build/icu-cross-build.sh`).
- Reads `/tmp/icu-build/src/` and `/tmp/icu-build/native-build/source/`; writes
  `/opt/icu/<goos>_<goarch>/{include,lib}`.
- Copies the source tree to `/tmp/icu-build/cross-<goos>-<goarch>`, then configures with
  `--host`, `--prefix`, `--with-cross-build=/tmp/icu-build/native-build/source`,
  `--enable-static --disable-shared --disable-tests --disable-samples --disable-tools
  --disable-extras --disable-icuio --disable-layoutex`; `CPPFLAGS` is set solely
  from `extra_cppflags` (nothing inherited).
- `make -j$(nproc)`, pre-creates `$prefix/{bin,lib,include}` (needed because `--disable-tools`
  leaves `bin/` uncreated), `make install`, then removes the build dir.

### 13.3 `build/zig-macos-stubs/tzfile.h`

A four-line stub defining `TZDIR "/var/db/timezone/zoneinfo"` and `TZDEFAULT "/etc/localtime"`,
because ICU 75.1's `putil.cpp` includes `tzfile.h` unconditionally under `__APPLE__` and zig's
bundled SDK omits it (`build/zig-macos-stubs/tzfile.h`).

---

## 14. Shipped Claude plugin and the repo's own hook wiring

### 14.1 `.claude-plugin/marketplace.json`

A marketplace named `lit`, description
"Plugin marketplace for lit, the agent-native issue tracker that lives in your git repo.", owner `lit`,
listing one plugin: `lit`, source `./claude-plugin`, description "The /next skill for repos tracked with lit",
version `0.5.0` (`.claude-plugin/marketplace.json`).

### 14.2 `claude-plugin/.claude-plugin/plugin.json`

`find claude-plugin -type f` yields two files: this manifest and one skill,
`claude-plugin/skills/next/SKILL.md`. The manifest declares name `lit`, description
"The /next skill for repos tracked with lit, the agent-native issue tracker.", version `0.5.0`,
and no hooks. The skill is named `next`, description "Pull the next ticket"
(`claude-plugin/skills/next/SKILL.md`).

There are no commands, agents, hooks, or MCP servers in the shipped plugin.

### 14.3 `.claude/settings.json` (this repo's dogfooding wiring)

One `SessionStart` hook with an empty matcher running the command
`.claude/hooks/session-start.sh` (`.claude/settings.json`).

### 14.4 `.claude/hooks/session-start.sh`

`#!/usr/bin/env bash` under `set -euo pipefail`. Reads the SessionStart hook's JSON from
stdin, extracts `session_id` with `grep -o '"session_id":"[^"]*"' || true` and
`head -1 | cut -d'"' -f4`, and — only when non-empty — prints
`Your Claude Code session id is: ${session_id}. When using lit, your assignee identity is claude_${session_id}.`
. That identity string is the same one `resolveIdentity` produces from
`CLAUDE_CODE_SESSION_ID` (`internal/cli/cli.go`).

### 14.5 `.claude/settings.local.json`

Local (untracked-style) permissions: allow list `Read(//Users/bmf/**)`,
`Read(//Users/bmf/code/dotfiles/config/claude.zai/**)`, `Bash(gh pr *)`,
`Bash(lit quickstart *)`, `Bash(lit backlog *)`, `Bash(lit show *)`, `Bash(lit *)`;
`enabledMcpjsonServers: ["chromedevtools/chrome-devtools-mcp"]`;
`enableAllProjectMcpServers: true` (`.claude/settings.local.json`).

### 14.6 `.mcp.json`

One project MCP server, `chrome-devtools-mcp`, over stdio: `npx -y
chrome-devtools-mcp@1.10.1 --isolated --no-usage-statistics` (`.mcp.json`).
The version is pinned exactly; `--isolated` gives each server a temporary Chrome profile.

---

## 15. Docs site configuration (gated by CI)

`mkdocs.yml` — `site_name: links`, `site_description: Agent-native issue tracker`,
`repo_url: https://github.com/promptctl/links-issue-tracker`, `docs_dir: docs`, theme
`material` with `navigation.sections`, `navigation.expand`, `navigation.instant`,
`content.code.copy`, markdown extensions `admonition` and `toc` with `permalink: true`, and a
seven-entry `nav` tree (`mkdocs.yml`). `ci.yml`'s `docs` job builds it under `--strict`
so a missing page or an out-of-tree link fails the merge gate
(`.github/workflows/ci.yml`).

---

## 16. Command registry structure (startup-relevant)

- Commands are data, not imperative registration: `CommandSpec{Name, Summary, GroupID,
  Run, Subcommands, Hidden, Retired}` (`internal/cli/register.go`), with
  `SubcommandSpec{Name, Subcommands}` for family commands (`internal/cli/register.go`) and
  `CommandRunner func(args []string) error` as the fully-wrapped handler
  (`internal/cli/register.go`).
- `Hidden` keeps a dispatchable command out of root `--help` and the completion projection
  without removing it from dispatch (`internal/cli/register.go`); the same property exists
  per-subcommand row (`internal/cli/register.go`).
- Help groups, in render order (`internal/cli/register.go`):
  `bootstrap` "Human Bootstrap", `operations` "Agent Operations", `structure`
  "Dependencies & Structure", `data` "Sync & Data", `maintenance` "Setup & Maintenance",
  `retention` "Issue Retention", `guidance` "Guidance & Tooling".
- A command's long-form description is not a registry field. `CommandSpec` carries no
  `Long`: a command's help page is rendered by its own leaf, from text embedded under
  `internal/cli/helptext/` and declared with `newCobraFlagSet(...).Detail(helpText(...))`.
  `init`'s blurb lives in `internal/cli/helptext/init.txt`.
- The registry is applied by `applyRegistry(root, commandGroups, commandSpecs(ctx, stdout, stderr))`
  (`internal/cli/cli.go`).
