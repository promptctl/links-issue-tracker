# `lit` platform inventory — bootstrap, configuration, discovery, release/build tooling

Derived entirely from source (Go, YAML, JSON, shell, Dockerfile, Justfile). No markdown
documentation was read. Every claim carries a `file:line` citation. Paths are repo-relative
to `/Users/bmf/code/links-issue-tracker`.

---

## 1. Module identity and dependencies

- Module path: `github.com/promptctl/links-issue-tracker` (`go.mod:1`).
- Go language/toolchain version: `go 1.25.7` (`go.mod:3`). CI derives its Go version from this
  file (`go-version-file: go.mod`, e.g. `.github/workflows/ci.yml:35`).
- Direct requirements (`go.mod:5-24`):
  - `github.com/CycloneDX/cyclonedx-go v0.11.0` (`go.mod:6`)
  - `github.com/cenkalti/backoff/v4 v4.2.1` (`go.mod:7`)
  - `github.com/dolthub/dolt/go v0.40.5-0.20260314011441-62975ef6bf36` (`go.mod:8`)
  - `github.com/dolthub/driver v0.2.1-0.20260314000741-0fe74e7ee31a` (`go.mod:9`)
  - `github.com/google/licenseclassifier` (`go.mod:10`)
  - `github.com/google/uuid v1.6.0` (`go.mod:11`)
  - `github.com/package-url/packageurl-go v0.1.6` (`go.mod:12`)
  - `github.com/pmezard/go-difflib` (`go.mod:13`)
  - `github.com/pressly/goose/v3 v3.27.1` (`go.mod:14`)
  - `github.com/promptctl/primitives v0.2.0` (`go.mod:15`)
  - `github.com/spf13/cobra v1.10.2`, `github.com/spf13/pflag v1.0.10`,
    `github.com/spf13/viper v1.21.0` (`go.mod:16-18`)
  - `golang.org/x/mod v0.35.0`, `golang.org/x/sync v0.22.0`, `golang.org/x/sys v0.45.0`
    (`go.mod:19-21`)
  - `gopkg.in/yaml.v3 v3.0.1` (`go.mod:22`)
- Three `replace` directives redirect the Dolt stack (`go.mod:186-196`):
  - `github.com/dolthub/dolt/go` → `github.com/promptctl/dolt/go v0.40.5-0.20260821231005-4b80eac34485`
    (`go.mod:187`)
  - `github.com/dolthub/go-mysql-server` → `github.com/promptctl/go-mysql-server v0.20.1-0.20260821032251-ab5cb9ec3b69`
    (`go.mod:189`)
  - `github.com/dolthub/driver` → local directory `./internal/vendor/dolthub-driver`
    (`go.mod:196`); the in-repo comment states the local copy removes an unconditional
    `eventsapi.dolthub.com` telemetry goroutine (`go.mod:191-195`).

---

## 2. Process startup and shutdown

### 2.1 `main()` sequence (`cmd/lit/main.go`)

1. `interrupt.Guard(context.Background(), interrupt.DefaultGrace)` installs the signal handler
   and returns `(ctx, stop)`; `stop` is deferred (`cmd/lit/main.go:17-18`).
2. `cli.Run(ctx, os.Stdout, os.Stderr, os.Args[1:])` executes the command
   (`cmd/lit/main.go:19`).
3. On a non-nil error, `os.Exit(cli.WriteCommandError(os.Stderr, err))`
   (`cmd/lit/main.go:20`). On nil error the process returns from `main` normally (exit 0).

### 2.2 Signal handling (`internal/interrupt/interrupt.go`)

- Caught signal set: `syscall.SIGINT`, `syscall.SIGTERM` (`internal/interrupt/interrupt.go:39`).
  SIGKILL is deliberately absent (`internal/interrupt/interrupt.go:37-38`).
- `DefaultGrace = 5 * time.Second` (`internal/interrupt/interrupt.go:33`).
- `Guard` allocates a 1-buffered signal channel, calls `signal.Notify`, derives a cancellable
  context, and spawns `watch` with the escalation closure `os.Exit(exitCode(sig))`
  (`internal/interrupt/interrupt.go:53-61`).
- `stop()` (deferred by `main`) is idempotent: it calls `signal.Stop`, `cancel()`, and closes
  the `done` channel; repeated calls are no-ops (`internal/interrupt/interrupt.go:62-71`).
- `watch` behavior (`internal/interrupt/interrupt.go:84-124`):
  - Blocks on `select` over `done` and `sigs`. `done` first ⇒ normal shutdown, returns without
    escalating (`internal/interrupt/interrupt.go:93-98`).
  - First interrupt ⇒ `cancel()` (`:101`), then `restoreDefault()` which is
    `signal.Stop(sigs)` (`:59`, `:104`), so a **second** interrupt terminates the process via
    the OS default disposition.
  - Then a `time.NewTimer(grace)` is started; a second `select` picks `done` (clean exit, no
    escalation) or `timer.C` (`escalate(sig)` ⇒ `os.Exit`) (`internal/interrupt/interrupt.go:116-123`).
- `exitCode` maps a signal to `128+signum` for `syscall.Signal` values and to `1` for any
  non-`syscall` signal (`internal/interrupt/interrupt.go:130-135`). Test-pinned values:
  SIGINT ⇒ 130, SIGTERM ⇒ 143, non-syscall ⇒ 1 (`internal/interrupt/interrupt_test.go:138-154`).

### 2.3 Acceptance behavior pinned by the signal tests (`cmd/lit/main_signal_test.go`)

- `TestMain` re-execs the test binary as the real `lit` binary when `LIT_TEST_REEXEC=1`
  (`cmd/lit/main_signal_test.go:31`, `:38-44`).
- A SIGTERM delivered while the post-write auto-sync (inline receive) is wedged on the commit
  lock must terminate the process in under 8 s and exit **0** — the write's own success code
  (`cmd/lit/main_signal_test.go:146-160`).
- A SIGTERM delivered while a git subprocess is wedged against a black-hole remote is pinned by
  `TestSIGTERMDuringWedgedGitSubprocessExitsCleanly` (`cmd/lit/main_signal_test.go:304`),
  using a listener-with-no-accept remote (`cmd/lit/main_signal_test.go:403`).

### 2.4 Argument parsing and the root command (`internal/cli/cli.go`)

- `Run` first calls `parseGlobalArgs(args)` (`internal/cli/cli.go:36`). That function scans
  leading arguments and:
  - `--` stops scanning and everything after is passed through (`internal/cli/cli.go:171-173`);
  - a bare `--output` or any `--output=…` in the leading position returns
    `UnsupportedError{Message: "--output is no longer supported; omit it for text output", Feature: "--output"}`
    (`internal/cli/cli.go:174-179`, `:310-312`);
  - any other first token stops scanning (`internal/cli/cli.go:176-181`).
- The root cobra command: `Use: "lit"`, `Long: "Agent-native issue tracker"`,
  `Args: cobra.ArbitraryArgs` (`internal/cli/cli.go:55-57`).
- Root with **no args**: resolves the workspace from cwd; if the error is
  `workspace.ErrNotGitRepo` it prints cobra help; otherwise it renders and prints the
  quickstart guidance for `ws.RootDir` (`internal/cli/cli.go:64-78`).
- Root with an unrecognized positional arg returns `UnknownCommandError{Command: args[0]}`
  (`internal/cli/cli.go:59-61`).
- The default `completion` command is disabled (`root.CompletionOptions.DisableDefaultCmd = true`,
  `internal/cli/cli.go:81`).
- Any global flag parse error is converted to `UsageError` (exit 2) via `SetFlagErrorFunc`
  (`internal/cli/cli.go:85-87`).
- `SilenceErrors` and `SilenceUsage` are both true; `pflag.ErrHelp` and the internal
  `errHelpHandled` sentinel are swallowed and reported as success
  (`internal/cli/cli.go:44-50`, `:28`).
- There are **no persistent global flags** registered on the root beyond cobra's own `help`;
  per-command flag sets are constructed by `newCobraFlagSet` (`internal/cli/cli.go:192-203`).
- Per-command flag parsing (`parseFlagSet`, `internal/cli/cli.go:274-308`) maps specific
  removed flags to typed errors:
  - `--output` ⇒ `UnsupportedError` "…omit it for text output" (`internal/cli/cli.go:286-288`);
  - `--continue` ⇒ `UnsupportedError` "--continue is retired; claim routing already keeps
    `lit next` in your checkout's own epic first — run `lit next` with no flag"
    (`internal/cli/cli.go:290-293`);
  - any other `unknown flag:` / `flag provided but not defined:` ⇒ `UsageError`
    (`internal/cli/cli.go:295-297`).
  - `--help` (or `pflag.ErrHelp`) prints `Usage of <cmd>:` followed by `PrintDefaults()` to
    stdout and returns the swallowed sentinel (`internal/cli/cli.go:265-272`, `:277-282`, `:300-306`).

### 2.5 Per-command bootstrap (`runWithApp` / `runWithWorkspace`)

- `runWithWorkspace` resolves the workspace from `os.Getwd()` and runs the handler
  (`internal/cli/cli.go:94-100`); `resolveWorkspaceFromWD` maps `workspace.ErrNotGitRepo` to
  `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}`
  and a getcwd failure to `get cwd: %w` (`internal/cli/cli.go:149-163`).
- `runWithApp` (`internal/cli/cli.go:102-147`):
  1. `os.Getwd()`; failure ⇒ `get cwd: %w` (`internal/cli/cli.go:103-106`).
  2. `app.Open(ctx, cwd, accessMode)`; `workspace.ErrNotGitRepo` ⇒
     `OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}`
     (`internal/cli/cli.go:108-111`).
  3. Captures `ap.Workspace` before running (`internal/cli/cli.go:118`), runs the handler with
     `defer ap.Close()` (`internal/cli/cli.go:119-122`).
  4. On success and `accessMode == app.AccessWrite`, prints the mutation sync-staleness banner
     (`printMutationSyncStalenessWarning(stdout, ws, time.Now())`, `internal/cli/cli.go:136`).
  5. Calls `maybeAutoSyncAfterCommand(ctx, accessMode, ws)` **after** the engine is closed
     (`internal/cli/cli.go:145`).

### 2.6 `app.Open` (`internal/app/app.go`)

- `AccessMode` is a string enum with exactly `AccessRead = "read"` and `AccessWrite = "write"`
  (`internal/app/app.go:30-35`).
- `accessContracts` maps each mode to `(engine.Mode, stream resolver)`:
  read ⇒ `engine.ReadOnly` + `workspace.ReadStream`; write ⇒ `engine.ReadWrite` +
  `workspace.EnsureStream` (`internal/app/app.go:57-60`).
- An unknown/zero mode fails closed with `invalid access mode %q` (`internal/app/app.go:69-74`).
- Sequence: resolve workspace → open engine at `ws.DatabasePath` with `ws.WorkspaceID` →
  resolve the stream identity from `ws.PrivateGitDir` → `st.AttributeTo(stream.Value())` →
  return `*App` (`internal/app/app.go:75-106`). A stream-resolution failure closes the store and
  joins both errors (`internal/app/app.go:87-96`).
- `OpenLocationForRead(ctx, loc)` opens an already-derived `Location` strictly read-only,
  reading `workspace_id` from the store's own `config.json` via `ReadConfig`
  (`internal/app/app.go:125-131`).
- `(*App).Close()` = `Store.Close()` (`internal/app/app.go:133`).

### 2.7 Exit codes (`internal/cli/exit.go`)

Constants (`internal/cli/exit.go:10-18`):

| Name | Value |
|---|---|
| `ExitOK` | 0 |
| `ExitGeneric` | 1 |
| `ExitUsage` | 2 |
| `ExitValidation` | 3 |
| `ExitNotFound` | 4 |
| `ExitConflict` | 5 |
| `ExitCorruption` | 7 |

(No code 6 is defined.) `ExitCode(err)` dispatches by `errors.As` in this order
(`internal/cli/exit.go:23-95`):

- `storage.NotFoundError` ⇒ 4 (`:27-30`)
- `MergeConflictError` ⇒ 5 (`:31-34`)
- `SyncFailureError` ⇒ 5 (`:39-42`)
- `ownerApprovalRefusalError` ⇒ 5 (`:46-49`)
- `CorruptionError` ⇒ 7 (`:50-53`)
- `UsageError` ⇒ 2 (`:54-57`)
- `UnknownCommandError` ⇒ 3 (`:58-61`)
- `RetiredCommandError` ⇒ 3 (`:64-67`)
- `ValidationError` ⇒ 3 (`:68-71`)
- `storage.ValidationError` ⇒ 3 (`:72-75`)
- `UnsupportedError` ⇒ 3 (`:76-79`)
- `OutsideWorkspaceError` ⇒ 1 (`:80-83`)
- `BulkFailureError` ⇒ 1 (`:84-90`)
- `store.ErrTransientGCContention` ⇒ 1 (`:91-93`)
- anything else ⇒ 1 (`:94`)

### 2.8 Error rendering (`internal/cli/error_output.go`)

- `WriteCommandError` prints `error (code=%d): %v\n` to stderr, then, when a remediation exists
  for the error's reason, `remediation: %s\n`; it returns the exit code
  (`internal/cli/error_output.go:17-24`).
- `commandErrorReason` maps typed errors to machine reason strings, including
  `entity_not_found`, `merge_conflict`, `sync_divergence`, `owner_approval_required`
  (`internal/cli/error_output.go:29-46`).

---

## 3. Every environment variable the program reads

Complete grep of `os.Getenv` / `os.LookupEnv` / `os.Environ` across non-vendored source.

### 3.1 Read by the shipped binary

| Variable | Where read | Effect |
|---|---|---|
| `XDG_CONFIG_HOME` | `internal/config/config.go:176` | When non-empty, `ConfigDir()` = `$XDG_CONFIG_HOME/links-issue-tracker`; otherwise `$HOME/.config/links-issue-tracker` (`internal/config/config.go:175-184`). |
| `LIT_CONFIG_GLOBAL_PATH` | `internal/config/config.go:170`, read at `:271` | Overrides the global config file path entirely; otherwise `ConfigDir()/config.toml` (`internal/config/config.go:270-273`). |
| `LIT_CONFIG_PROJECT_PATH` | `internal/config/config.go:171`, read at `:276` | Overrides the project config file path; otherwise `<workspaceRoot>/.lit/config.toml` (`internal/config/config.go:275-278`). |
| `LIT_DISABLE_AUTO_SYNC` | const at `internal/cli/sync_cadence.go:34`; read at `internal/cli/sync_cadence.go:79` and `internal/cli/owner_notify.go:149` | When truthy, no command schedules a push mirror, runs an inline receive, **or** compacts (`internal/cli/sync_cadence.go:19-33`, `:79-81`), and the owner-notify hook never runs (`internal/cli/owner_notify.go:149-151`). Truthiness = `strconv.ParseBool` of the trimmed value; a parse error is false (`internal/cli/sync_cadence.go:307-313`). |
| `CLAUDE_CODE_SESSION_ID` | `internal/cli/cli.go:1173` | When non-empty (after trim), the acting identity is always `claude_<sessionID>`, overriding `--assignee`/`--by`; otherwise the trimmed explicit value passes through (`internal/cli/cli.go:1172-1177`). |
| `LNKS_AUTOMATION_TRIGGER` | const `internal/cli/automation_trace.go:15`; read `:53` | Non-empty enables automation-trace recording for the command; the value becomes the trace's `Trigger` field (`internal/cli/automation_trace.go:59-73`). Empty ⇒ no trace is written (`:61-63`). |
| `LNKS_AUTOMATION_REASON` | const `internal/cli/automation_trace.go:16`; read `:54` | Default `Reason` on the automation trace when the caller supplied none (`internal/cli/automation_trace.go:64-66`). |
| `LNKS_AUTOMATION_TRACE_REF_FILE` | const `internal/cli/automation_trace.go:17`; read `:55` | When non-empty, the recorded trace's path is written (plus newline, mode 0644) to that file (`internal/cli/automation_trace.go:78-81`). |
| `LINKS_DEBUG_DOLT_SYNC_BRANCH` | const `internal/cli/sync.go:20`; read `internal/cli/sync.go:690` | Overrides the resolved sync branch ahead of the remote's default branch (`internal/cli/sync.go:689-693`); named in the failure message when no branch can be resolved (`internal/cli/sync.go:713`). |
| `EDITOR` | `internal/cli/workflows_edit.go:140`, `:147` | Split on whitespace to form the editor command for `lit workflows edit`; named verbatim in the failure message. |
| (whole environment) | `internal/cli/sync_bg.go:110`, `internal/cli/owner_notify.go:205` | Inherited by child processes — see 3.2. |

### 3.2 Environment variables the binary **writes** for child processes

- Detached background mirror (`mirrorEnv`, `internal/cli/sync_bg.go:104-127`): inherits the
  parent environment with `LNKS_AUTOMATION_TRIGGER=`, `LNKS_AUTOMATION_REASON=`, and
  `LNKS_AUTOMATION_TRACE_REF_FILE=` prefixes **stripped** (`internal/cli/sync_bg.go:105-110`),
  then appends `LNKS_AUTOMATION_TRIGGER=on-change` and
  `LNKS_AUTOMATION_REASON=on-change cadence mirrored after a mutating command`
  (`internal/cli/sync_bg.go:124-127`).
- Owner-notify hook (`runOwnerNotifyHook`, `internal/cli/owner_notify.go:194-222`): the hook is
  run as `sh -c <hook>` with `cmd.Dir = repoRoot` (`internal/cli/owner_notify.go:197`, `:204`)
  and `os.Environ()` plus:
  `LIT_NOTIFY_KIND`, `LIT_NOTIFY_SUMMARY`, `LIT_NOTIFY_REMOTE`, `LIT_NOTIFY_BRANCH`,
  `LIT_NOTIFY_REPO` (`internal/cli/owner_notify.go:205-211`). The hook is time-boxed by
  `ownerNotifyHookTimeout` with a `WaitDelay` so a backgrounded child cannot hold the pipe
  (`internal/cli/owner_notify.go:195-203`).

### 3.3 Read only by tests / repo tooling

| Variable | Where | Effect |
|---|---|---|
| `GITHUB_ACTIONS` | `tools/testbudget/main.go:210` | When `== "true"`, budget-violation lines are prefixed `::error::` so they become GHA annotations (`tools/testbudget/main.go:208-215`). |
| `LIT_LICENSE_GRAPH_AUDIT` | `tools/licenses/graph_test.go:35` (via `graphAuditEnv`) | Empty ⇒ the whole-graph acceptance test is skipped; the license-graph-audit workflow sets it to `"1"` (`.github/workflows/license-graph-audit.yml:65-67`). |
| `LIT_TEST_REEXEC` | `cmd/lit/main_signal_test.go:31`, `:39` | `=1` makes the test binary run the real `main()` instead of the test suite. |
| `USER`, `HOME` | `internal/workspace/stream_test.go:380-381` | Used only as negative-evidence material asserting stream tokens contain no username/home material. |
| (`killHelperDirEnvVar`) | `internal/store/process_kill_test.go:35`, `:134` | Test-helper subprocess coordination directory. |

---

## 4. Configuration

### 4.1 Location, format, precedence

- Format: whatever viper infers from the file extension via `SetConfigFile`
  (`internal/config/config.go:286`); the shipped/default filename is `config.toml`
  (`internal/config/config.go:272`, `:277`).
- Layers, merged in slice order so **later overrides earlier**
  (`internal/config/config.go:186-193`):
  1. Global: `$LIT_CONFIG_GLOBAL_PATH`, else `ConfigDir()/config.toml`
     (`internal/config/config.go:270-273`). `ConfigDir()` = `$XDG_CONFIG_HOME/links-issue-tracker`
     if `XDG_CONFIG_HOME` is set, else `$HOME/.config/links-issue-tracker`; empty string if the
     home directory cannot be determined (`internal/config/config.go:175-184`).
  2. Project: `$LIT_CONFIG_PROJECT_PATH`, else `<workspaceRoot>/.lit/config.toml`
     (`internal/config/config.go:275-278`).
- A missing file is not an error; a parse error is (`parse config %s: %w`) and a merge error is
  (`merge config %s: %w`) (`internal/config/config.go:280-295`).
- An empty path (`PathSpec.IsEmpty()`) contributes nothing (`internal/config/config.go:281-284`).
- Each layer additionally contributes `ready.required_fields` **and** the legacy top-level
  `required_fields`, concatenated across layers in precedence order
  (`internal/config/config.go:297-299`); if that concatenation is non-empty it *replaces*
  `cfg.Ready.RequiredFields` (`internal/config/config.go:239-241`).

### 4.2 Complete key schema and defaults

Defaults are set in `Load` (`internal/config/config.go:217-228`).

| Key | Go type | Default | Notes / validation |
|---|---|---|---|
| `logging.verbose` | bool | `false` | `internal/config/config.go:217`, `:29` |
| `logging.file` | string | `""` | `internal/config/config.go:218`, `:30` |
| `init.install_hooks` | bool | `true` | `internal/config/config.go:219`, `:34` |
| `init.install_agents` | bool | `true` | `internal/config/config.go:220`, `:35` |
| `migration.auto_apply` | bool | `false` | `internal/config/config.go:221`, `:39` |
| `ready.required_fields` | []string | `[]` | `internal/config/config.go:222`, `:43`; also fed by legacy top-level `required_fields` (`:298`) |
| `quickstart.soil_mode` | bool | `false` | `internal/config/config.go:223`, `:47` |
| `snapshot.retention_budget` | int | `5` | `internal/config/config.go:224`, `:51`; **must be > 0** or `Load` fails with `config: snapshot.retention_budget must be > 0, got %d` (`:245-247`) |
| `sync.cadence` | string enum | `"on-change"` | `internal/config/config.go:225`; legal values `on-push`, `on-change` (`:133`, `:140`, `:150`); invalid ⇒ `config: sync.cadence must be one of on-push, on-change, got %q` (`:252-254`) |
| `sync.receive` | bool | `true` | `internal/config/config.go:226`, `:109` |
| `sync.owner_notify_cmd` | string | `""` | `internal/config/config.go:227`, `:117`; empty means no owner-notification channel |
| `claims.freshness_window` | duration string | `"24h"` | `internal/config/config.go:228`, `:73`; read as a **string** (`v.GetString`) and parsed by `time.ParseDuration` (`:262`, `:89-98`) |

- `claims.freshness_window` is explicitly excluded from struct-tag decoding (`mapstructure:"-"`,
  `internal/config/config.go:73`). A bare number fails with
  `config: claims.freshness_window must be a duration with a unit, like "24h" or "90m" (got %q): %w`
  (`internal/config/config.go:92`); a non-positive duration fails with
  `config: claims.freshness_window must be positive, got %s` (`internal/config/config.go:95`).
- Decode failure of the whole config yields `decode config: %w` (`internal/config/config.go:237`).

### 4.3 Sync cadence semantics

- `on-push`: mirrors only when the managed pre-push git hook runs
  (`internal/config/config.go:127-133`).
- `on-change` (default): mirrors after every mutating `lit` command
  (`internal/config/config.go:134-140`).
- The default literal is chosen independently of the ordering of `syncCadences`
  (`internal/config/config.go:143-150`).
- `shouldSyncAfterMutation` returns true only for `AccessWrite` + `on-change`
  (`internal/cli/sync_cadence.go:64-66`).
- `maybeAutoSyncAfterCommand` (`internal/cli/sync_cadence.go:78-101`): returns immediately when
  `LIT_DISABLE_AUTO_SYNC` is truthy; loads config (unreadable ⇒
  `lit: automatic sync skipped, config unreadable: %v` on stderr and return); runs
  `ensureMirrorCoverage` when the cadence says so; runs `receiveInline` when `sync.receive`;
  runs `compactInline` when the access mode was write.
- Timing constants: `receiveDebounceInterval = 5 * time.Minute`
  (`internal/cli/sync_cadence.go:45`); `remoteAbsentRecheckInterval = 10 * time.Second`
  (`internal/cli/sync_cadence.go:56`).

### 4.4 Per-workspace store config (`<git-common-dir>/links/config.json`)

- Schema (`internal/workspace/workspace.go:21-26`): `workspace_id` (string),
  `issue_prefix` (string), `created_at` (RFC3339 time), `schema_version` (int).
- Created on first resolve with `WorkspaceID = uuid.NewString()`, `CreatedAt = time.Now().UTC()`,
  `Version = 1` (`internal/workspace/workspace.go:491-496`).
- `ReadConfig` fails with `read workspace config: %w`, `parse workspace config: %w`, or
  `workspace config missing workspace_id` (`internal/workspace/workspace.go:447-459`).
- Writes are atomic: temp file `.config.json.*` in the same directory, chmod 0644, close,
  rename (`internal/workspace/workspace.go:504-537`).
- `UpdateConfig(path, mutate)` is the single read-modify-write boundary
  (`internal/workspace/workspace.go:546-560`).
- Issue-prefix resolution (`internal/workspace/workspace.go:421-434`): a blank configured value
  is *derived* from the repository directory name; a present value is normalized; an invalid
  present value errors `invalid issue_prefix: %w`. A derived or renormalized value is persisted
  back into `config.json` immediately (`internal/workspace/workspace.go:469-477`).
- Derivation (`internal/workspace/workspace.go:562-579`): normalize `filepath.Base(rootDir)`,
  split on `-`, take the first hyphen-part that normalizes to a valid prefix, else the whole
  normalized base; failure ⇒ `derive issue_prefix: repository name %q does not produce a valid prefix`.

---

## 5. Workspace discovery

### 5.1 `Location` — pure path geometry (`internal/workspace/workspace.go:56-62`)

`LocationFromStorageDir(storageDir)` (`internal/workspace/workspace.go:284-295`) derives:

| Field | Value |
|---|---|
| `StorageDir` | the given dir (in practice `<git-common-dir>/links`, `internal/workspace/workspace.go:223`) |
| `GitCommonDir` | `filepath.Dir(storageDir)` |
| `ConfigPath` | `<storageDir>/config.json` |
| `DatabasePath` | `<storageDir>/dolt` |
| `DoltRepoPath` | `<storageDir>/dolt/links` |

### 5.2 `deriveLocation(cwd)` (`internal/workspace/workspace.go:188-224`)

1. `git rev-parse --git-common-dir` run with `cmd.Dir = cwd` (`internal/workspace/workspace.go:200`,
   `:306-314`).
2. `anchorGitPath(cwd, out)` — a relative git answer is joined onto the **absolute cwd**, not the
   repo toplevel; an absolute answer is only cleaned (`internal/workspace/workspace.go:239-248`).
3. `filepath.EvalSymlinks` canonicalizes the common dir; failure ⇒
   `canonicalize git-common-dir %q: %w` (`internal/workspace/workspace.go:218-221`).
4. `LocationFromStorageDir(filepath.Join(gitCommonDir, "links"))` (`internal/workspace/workspace.go:223`).

### 5.3 `Resolve(cwd)` (`internal/workspace/workspace.go:140-179`)

1. `git rev-parse --show-toplevel` ⇒ `RootDir`; failure classified by `classifyGitError`.
2. `deriveLocation(cwd)`.
3. `resolvePrivateGitDir(cwd)` = `git rev-parse --git-dir` anchored to cwd
   (`internal/workspace/workspace.go:266-275`) — **not** symlink-canonicalized
   (`internal/workspace/workspace.go:261-265`).
4. `os.MkdirAll(loc.StorageDir, 0o755)`; failure ⇒ `create storage dir: %w`
   (`internal/workspace/workspace.go:165-167`).
5. `loadOrCreateConfig(rootDir, loc.ConfigPath)`.
6. Returns `Info{Location, RootDir, WorkspaceID, IssuePrefix, PrivateGitDir}`.

All geometry git calls use `context.Background()` deliberately (`internal/workspace/workspace.go:141-146`,
`:197-199`, `:267-269`).

### 5.4 Git-error classification (`internal/workspace/workspace.go:337-343`)

- Only `*exec.ExitError` with exit code **128** (`gitFatalExitCode`,
  `internal/workspace/workspace.go:320`) maps to the sentinel `ErrNotGitRepo`
  (`internal/workspace/workspace.go:19`).
- Everything else (git not on PATH, killed by signal ⇒ `ExitCode() == -1`, any other exit code)
  is wrapped with context and surfaced.

### 5.5 `Discover(roots)` (`internal/workspace/discover.go:24-41`)

- Walks every root with `filepath.WalkDir`, folding results into a map keyed by canonical
  `StorageDir`, then returns them sorted by `StorageDir`
  (`internal/workspace/discover.go:25-40`).
- Per-directory algorithm (`internal/workspace/discover.go:48-113`):
  - A walk error is fatal: `scan %q: %w` (`:50-54`).
  - Any directory literally named `.git` ⇒ `filepath.SkipDir` (`:58-60`).
  - Non-directories are skipped (`:61-63`).
  - `os.Lstat(<path>/.git)`: `os.ErrNotExist` ⇒ skip; any other stat error ⇒ `stat %q: %w`
    (`:67-78`).
  - `deriveLocation(path)`: `ErrNotGitRepo` ⇒ skip; any other error ⇒ surfaced (`:79-90`).
  - `os.Stat(loc.DatabasePath)`: `os.ErrNotExist` ⇒ skip ("git repo, no lit store"); other error
    ⇒ `stat store database %q: %w` (`:91-102`).
  - The database path must be a **directory**; a regular file there is skipped (`:103-110`).
  - Otherwise `byStore[loc.StorageDir] = loc` — so all worktrees of one repository collapse to
    one entry (`:111`).

### 5.6 Multi-checkout enumeration (`internal/workspace/checkouts.go`)

- `Checkout{Stream, Path, Branch}` (`internal/workspace/checkouts.go:27-31`); `Branch` is empty
  for a detached HEAD (`internal/workspace/checkouts.go:26`).
- `LiveCheckouts(cwd)` runs `git worktree list --porcelain -z` with
  `context.Background()` (`internal/workspace/checkouts.go:61-66`). Any failure produces a
  message that always names the **git ≥ 2.36** `-z` requirement
  (`internal/workspace/checkouts.go:81`), and deliberately does **not** route through
  `classifyGitError` (`internal/workspace/checkouts.go:67-80`).
- Records are filtered by `uninhabited()` = `prunable || bare`
  (`internal/workspace/checkouts.go:92`, `:132`).
- For each remaining record: `resolvePrivateGitDir(record.path)` (failure ⇒
  `locate the git directory of worktree %q, which git lists as live: %w`,
  `internal/workspace/checkouts.go:101-104`) then `ReadStream(privateGitDir)` — any failure
  fails the whole enumeration (`internal/workspace/checkouts.go:105-108`).
- Porcelain `-z` parser (`internal/workspace/checkouts.go:179-215`):
  - Fields are NUL-separated; each field is `Cut` on the **first space only** (`:186`).
  - `worktree <path>` opens a record (`:187-190`); an empty field is a record terminator
    and is skipped (`:191-193`).
  - An attribute field before any `worktree` field ⇒
    `git worktree list --porcelain -z opened with %q, which is not a `worktree <path>` field` (`:194-196`).
  - Recognized attributes: `branch` (stripped of `refs/heads/`), `prunable`, `bare`
    (`:198-205`). `detached`, `locked`, `HEAD`, and anything else are ignored (`:148-157`).
  - Zero records ⇒ `git worktree list --porcelain -z named no worktrees at all` (`:207-213`).

### 5.7 Stream identity (`internal/workspace/stream.go`)

- Token file: `lit-stream` inside the checkout's **private** git dir
  (`internal/workspace/stream.go:22`, `:80`).
- Entropy: 8 bytes from `crypto/rand` (`internal/workspace/stream.go:28`, `:232-238`), encoded
  as unpadded base32 and lowercased ⇒ 13 characters, alphabet `[a-z2-7]`
  (`internal/workspace/stream.go:33`, `:38`, `:237`).
- `ReadStream` returns the zero `StreamID` for a missing file, an error
  (`read stream id %q: %w`) for other read failures, and parses otherwise
  (`internal/workspace/stream.go:79-89`).
- `EnsureStream` reads first, publishes if absent, then re-reads and returns whatever the FILE
  holds; a token that vanishes between publish and read ⇒
  `stream id %q vanished immediately after it was written`
  (`internal/workspace/stream.go:106-130`).
- `publishStreamToken` (`internal/workspace/stream.go:150-227`): `os.CreateTemp` in the private
  git dir named `lit-stream.tmp-*`; write `token + "\n"`; `Chmod(0o644)`; `Sync()`; `Close()`;
  then `os.Link(tmp, path)`. `os.ErrExist` from the link is **success** (someone else won the
  race) (`:203-205`). Any other link error message always states the hard-link requirement
  (`:224`). The temp file is removed via `defer` on every path (`:173`).
- `parseStreamToken` accepts exactly 13 characters from `[a-z2-7]`; otherwise
  `stream id %q is malformed: %s; delete the file to mint a fresh identity for this checkout …`
  (`internal/workspace/stream.go:260-282`).

### 5.8 Git-remote helpers (`internal/workspace/workspace.go`)

- `UpstreamRemote(ctx, cwd)`: `git rev-parse --abbrev-ref --symbolic-full-name @{upstream}`,
  first `/`-separated segment (`:98-101`, `:375-382`).
- `RemoteHasRefs(ctx, cwd, remote)`: `git ls-remote <remote>` non-empty (`:103-110`).
- `RemoteHasDoltData(ctx, cwd, remote)`: `git ls-remote <remote> refs/dolt/*` non-empty
  (`:119-126`).
- `DefaultRemoteBranch(ctx, cwd, remote)`: `git symbolic-ref --quiet --short refs/remotes/<r>/HEAD`
  first, else `git ls-remote --symref <r> HEAD` parsed for `ref: refs/heads/…\tHEAD`
  (`:128-138`, `:362-373`).
- `GitRemotes(ctx, cwd)`: `git remote -v`, keeps only lines whose third field is `(fetch)`,
  deduped by name and sorted by name (`:384-414`).
- An empty/blank remote name normalizes to `origin` (`:345-351`).

---

## 6. Version identity

`internal/version/version.go`:

- Link-time variables `Version`, `Commit`, `Date` (`:34-38`). Three writers are named in-source:
  goreleaser (all three), `scripts/install.sh` source mode (all three), and the Justfile `build`
  recipe (Commit + Date only) (`:28-33`).
- `StaleBuildThreshold = 7 * 24 * time.Hour` (`:47`).
- `Info{Version, Commit, Date, IsDev, Schema}` with JSON tags
  `version/commit/date/is_dev/schema_support` (`:57-63`).
- `SchemaSupport{Min int64 "min", Max int64 "max"}` (`:73-76`).
- `Get()` derives `Schema.Max` from `migrations.MaxVersion()` (one ReadDir over the embedded
  registry) and `Schema.Min` from `migrations.Baseline`; `IsDev = (Version == "")` (`:81-93`).
- `BuildAge(now)` returns `(0,false)` when `Date` is empty, unparseable as RFC3339, or in the
  future; otherwise `now.Sub(stamped)` (`:101-113`).

`lit version` output (`internal/cli/version.go:17-68`):

- Rejects any positional argument: `usage: lit version` (`:22-24`).
- Line 1: `lit %s (commit %s, built %s)\n`, where an `IsDev` build prints `dev`, an empty commit
  prints `unknown`, an empty date prints `unknown` (`:31-45`).
- Line 2 (only when `BuildAge` is ok): `built %s ago\n` (`:52-55`).
- Line 3 (only when the age ≥ `StaleBuildThreshold`):
  `WARNING: binary is older than %s — run `just build` (or `just install`) to pick up recent fixes\n`
  (`:56-63`).
- Final line: `schema versions supported: %d–%d\n` (`:66`).

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
(`internal/release/manifest.go:35-61`.) `IsDev` always serializes `false` for published
manifests (`internal/release/manifest.go:32-34`). `Signature` is reserved and unverified today
(`internal/release/manifest.go:53-57`).

### 7.2 Target selection (`internal/release/target.go`)

- `Target{Manifest, Artifact}` (`:17-20`).
- `CurrentPlatform()` = `runtime.GOOS + "/" + runtime.GOARCH` (`:28-30`).
- `SelectArtifact(m, platform)` does an **exact** platform match; on miss it errors
  `release %s has no artifact for platform %s (available: %v)` listing the manifest's platforms
  (`:35-46`).

### 7.3 Manifest resolution (`internal/release/resolver.go`)

- `DefaultBaseURL = "https://github.com/promptctl/links-issue-tracker/releases/download"`
  (`:58`).
- URL fetched: `<base>/<tag>/release-manifest.json` (trailing `/` trimmed from base) (`:94`).
- Tag validation, all applied inside `Resolve` (`:76-84`):
  - must start with `v` ⇒ else `release: tag must be v-prefixed (got %q)`;
  - must match `^v[A-Za-z0-9._+-]+$` (`tagAcceptPattern`, `:28`) ⇒ else
    `release: tag %q must match %s (v-prefix + alphanumerics, dots, dashes, underscores, plus)`;
  - must not contain `..` ⇒ else `release: tag %q contains path-traversal sequence`.
- Default HTTP client timeout `defaultResolverTimeout = 60 * time.Second` when `Client` is nil
  (`:39`, `:89-93`).
- Non-200 ⇒ `release: fetch %s: HTTP %d: %s` with the first 256 body bytes (`:104-107`).
- Body is decoded through `io.LimitReader(resp.Body, 1<<20)` with
  `dec.DisallowUnknownFields()` (`:113-114`); decode failure ⇒ `release: decode %s: %w`.
- A second `Decode` must return `io.EOF`; a second document ⇒
  `release: decode %s: unexpected trailing JSON after manifest`; any other error ⇒
  `release: decode %s: unexpected trailing data after manifest: %w` (`:123-132`).
- Finally `SelectArtifact` (`:133-137`).

### 7.4 Installer (`internal/release/installer.go`)

- `BinaryName = "lit"` (`:38`); windows archives are expected to hold `lit.exe`
  (`:239-258`).
- Caps: `maxArchiveBytes = 256 << 20` (compressed download, `:47`),
  `maxUncompressedBytes = 256 << 20` (per entry, `:177`),
  `maxTotalUncompressedBytes = 2 * maxUncompressedBytes` (whole gunzip stream, `:195`).
- `defaultInstallerTimeout = 5 * time.Minute` when `Client` is nil (`:188`, `:73-77`).
- `Install` sequence (`:60-119`):
  1. `archiveFormatForURL(url)`: `.tar.gz` ⇒ tar/gzip + binary name `lit`; `.zip` ⇒ zip +
     `lit.exe`; anything else ⇒
     `release: unsupported archive extension in %q (want .tar.gz or .zip)` (`:249-258`).
  2. Create the temp file `.lit-downgrade-*.tmp` **in the target directory first**, before
     downloading, so an unwritable install dir fails fast (`:79-95`).
  3. `downloadAndVerify`.
  4. `extractBinary` into the temp file, `Chmod(0o755)`, `Close`, `os.Rename` into place
     (`:103-118`). Any failure before the rename removes the temp file (`:91-95`).
- `downloadAndVerify` (`:124-167`): GET with ctx; non-200 ⇒
  `release: fetch %s: HTTP %d: %s` (256-byte snippet); the expected SHA256 is hex-decoded
  **before** the download and must be exactly 32 bytes, else
  `release: artifact SHA256 %q is not a 64-char hex digest`; the body is read through
  `io.LimitReader(..., maxArchiveBytes+1)` teed into sha256; over-cap ⇒
  `release: archive %s exceeds %d byte cap`; mismatch ⇒
  `release: SHA256 mismatch for %s: expected %s, got %s`.
- `extractBinary` accept shape (`:362-408`): every entry must pass `safeFlatName` (non-empty,
  not `.`/`..`, contains neither `/` nor `\` nor `..` — `:433-438`) else
  `release: archive entry has unsafe path: %q`; every entry must be a regular file else
  `release: archive contains non-regular entry %q`; every entry's declared size must be in
  `[0, maxUncompressedBytes]` else
  `release: archive entry %q declares %d uncompressed bytes (cap %d)`; exactly one entry named
  `format.binaryName` — two ⇒ `release: archive contains multiple %q entries`, zero ⇒
  `release: archive did not contain a %q entry`.
- `copyCappedEntry` streams with `io.CopyN(dest, body, maxUncompressedBytes+1)` and rejects an
  actual size over the cap even when the header lied:
  `release: %q exceeded uncompressed cap %d` (`:412-429`).
- The gzip stream is wrapped in a `boundedReader` capped at `maxTotalUncompressedBytes`;
  exceeding it yields `errStreamCap` = `release: uncompressed archive stream exceeds total cap`
  (`:208-232`, `:290-300`).
- Tar accepts both `tar.TypeReg` and `tar.TypeRegA` as regular (`:302-316`). Zip sizes come from
  `UncompressedSize64` cast to int64, with the negative case rejected upstream (`:333-347`).

### 7.5 `lit downgrade` (`internal/cli/downgrade.go`)

- One flag: `--to` (`downgrade: Target binary version (v-prefixed git tag, e.g. v0.4.1)`,
  `internal/cli/downgrade.go:77`). Any positional arg ⇒ `usage: lit downgrade --to <version>`
  (`:81-83`).
- `normalizeReleaseTag` (`:136-156`): blank ⇒
  `ValidationError{"<verb>: --to requires a non-empty version"}`; a missing leading `v` is added;
  a value containing `/`, `\`, `..`, or whitespace ⇒
  `ValidationError{"<verb>: --to %q is not a valid release tag"}`.
- Pipeline (`:38-124`): require the store to expose `storage.SchemaMigration` (`:44-47`);
  resolve `Target` via `release.HTTPResolver{}` at `release.CurrentPlatform()`;
  `store.Downgrade(ctx, target.Manifest.Schema.Max)`; resolve the running binary via
  `currentBinaryPath()` = `os.Executable()` + `filepath.EvalSymlinks` (`:162-172`);
  `release.HTTPInstaller{}.Install(ctx, target, binPath)`.
- An install failure after a successful schema reversal produces a message naming both recovery
  paths (manual download from the artifact URL, or `lit snapshots list` + `lit snapshots restore`)
  (`:105-112`).
- Success prints
  `downgraded to %s (schema v%d) installed at %s\nre-run \`lit version\` to confirm.\n` (`:119-122`).
  No re-exec is performed (`:114-118`).

### 7.6 `scripts/install.sh` (492 lines)

Three modes, one target-resolution rule (`scripts/install.sh:3-18`):

- Flags: default (source build); `--from-release <tag>`; `--latest-release`; `-h|--help`
  (`scripts/install.sh:103-136`). `--from-release` and `--latest-release` are mutually exclusive;
  combining them errors `error: cannot combine <flag> with <flag>` and prints
  `usage: $0 [--from-release <tag>|--latest-release]` (`:104-127`). `--from-release` with no
  argument ⇒ `error: --from-release requires a tag (e.g. v0.1.0)` (`:113-115`).
- Constants: `REPO_DOWNLOAD_BASE=https://github.com/promptctl/links-issue-tracker/releases/download`
  (`:24`), `REPO_LATEST_API=https://api.github.com/repos/promptctl/links-issue-tracker/releases/latest`
  (`:25`).
- Binary name is `lit`, or `lit.exe` on windows, detected once from `uname -s` (`:27-36`).
- `realpath_compat` cascade: `realpath` → `readlink -f` → a pure-shell link walk; no `python3`
  (`:38-92`).
- **Target directory priority** (`:141-187`): an existing `lit` on PATH (via `type -P`,
  symlink-resolved, dirname) → `$GOBIN` → `go env GOBIN` → `go env GOPATH` first entry + `/bin`
  → `$HOME/.local/bin`. If `HOME` is unset at that last step it errors
  `error: cannot determine install directory — $HOME is unset …` (`:176-184`).
  `mkdir -p "$TARGET_DIR"` (`:187`).
- **Source mode** (`:192-229`): `ver=$(git describe --tags --always --dirty)` with a leading `v`
  stripped; empty stays empty so `IsDev` remains true; sources `scripts/cgo-env.sh` and
  `scripts/version-ldflags.sh`; builds with
  `GOFLAGS=…-buildvcs=false go build -ldflags "-X <pkg>.Version=… -X <pkg>.Commit=… -X <pkg>.Date=…" -o "$TARGET_DIR/$BIN_NAME" ./cmd/lit`.
- **Release/latest mode** (`:230-456`):
  - Requires `curl` (`:233-237`); `--latest-release` additionally requires `jq` and reads
    `.tag_name` from the GitHub API (`:241-254`).
  - Tag normalization: strip then re-add a leading `v` (`:271`), then require
    `^v[0-9]+\.[0-9]+\.[0-9]+$` else
    `error: release tag '<tag>' is not a canonical semver release tag (expected vX.Y.Z)` (`:277-283`).
  - Archive name `lit_${tag#v}_${os}_${arch}.${ext}` (`:286`, `:321`); arch map
    `x86_64|amd64→amd64`, `arm64|aarch64→arm64`, else
    `error: unsupported architecture` (`:294-298`); OS map `linux|darwin→tar.gz`,
    `mingw*|msys*|cygwin*→windows/zip`, else `error: unsupported OS` (`:299-304`).
  - Extractor probes: `tar` for `.tar.gz`, `unzip` for `.zip` (`:307-320`).
  - Downloads `<base>/<tag>/<archive>` and `<base>/<tag>/checksums.txt` into a temp dir created
    **inside `$TARGET_DIR`** (`mktemp -d "$TARGET_DIR/.lit-install.XXXXXX"`) so the final `mv` is
    atomic; `trap rm -rf` on EXIT (`:326-333`).
  - Expected checksum is extracted with `awk '$2 == want'` (exact field match, not grep)
    (`:337-341`); digest computed by `sha256sum` or `shasum -a 256`, else an explicit error
    naming both tools (`:342-355`); a mismatch prints expected/actual and exits 1 (`:356-362`).
  - **Structural archive validation before extraction** (`:365-412`): tar entry names must be
    flat (`.`/`..`/`*/*`/`/*` rejected) and `tar -tvzf` column 1 must be `-` (regular) for every
    line; zip names are checked for `/`, `\`, `.`, `..`, leading `/`.
  - Extract, then reject a symlink at the binary path
    (`error: extracted '<bin>' is a symlink; archive rejected`, `:427-431`), require a regular
    file (`:432-435`), `chmod +x`, require executable (`:436-446`), then
    `mv -f "$tmp/$BIN_NAME" "$TARGET_DIR/$BIN_NAME"` (`:449`).
- Post-install, unconditional (`:459-492`): removes any stale `lnks`/`lnks.exe` in the target
  dir (`:461`); walks every `PATH` entry, canonicalizing each `lit` candidate, and collects those
  whose realpath differs from the just-installed binary (`:472-483`); prints
  `Installed lit -> <path>` and runs `<installed> version` (errors ignored) (`:485-486`); prints a
  `WARNING: other 'lit' binaries found on PATH that were NOT updated:` block listing each
  (`:487-491`).

### 7.7 `scripts/version-ldflags.sh`

- Must be sourced; executing it prints a message and exits **64** (`:26-29`).
- `LIT_BUILD_COMMIT="$(git rev-parse --short HEAD)"`; empty ⇒ message and `return 1`
  (`:35-40`). `LIT_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"` (`:41`). Both exported (`:42`).
- Deliberately never sets `Version` (`:16-22`).

### 7.8 `scripts/cgo-env.sh`

- Must be sourced; executing it exits **64** (`:30-33`).
- Off macOS it exports nothing (`:11-15`).
- On Darwin: requires `brew` (else message + `return 1`, `:36-40`); prefers
  `brew --prefix icu4c@78`, falling back to `brew --prefix icu4c`; also `brew --prefix zstd`
  (`:45-47`); verifies `include/unicode/regex.h` and `include/zstd.h` exist, else
  `cgo-env: ICU headers not found. Run: brew install icu4c@78` /
  `cgo-env: zstd headers not found. Run: brew install zstd` (`:49-58`); exports
  `CGO_CPPFLAGS="-I<icu>/include -I<zstd>/include …"` and
  `CGO_LDFLAGS="-L<icu>/lib -L<zstd>/lib …"`, preserving any caller-set values (`:64-65`).

### 7.9 `scripts/next-version.sh`

- Usage `scripts/next-version.sh <minor|patch>`; wrong argument count ⇒ exit **2** (`:21-24`);
  a bump other than `minor`/`patch` ⇒ exit **2** with
  `bump must be 'minor' or 'patch' (major is frozen for this repo)` (`:29-32`).
- Latest clean tag = `git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | grep -vE '-' | head -1`
  (`:42-44`); none ⇒ exit **3** (`:45-48`); non-`vX.Y.Z` shape ⇒ exit **3** (`:50-53`).
- `minor` ⇒ MINOR+1, PATCH=0; `patch` ⇒ PATCH+1 (`:60-63`). MAJOR is never bumped (`:27-28`).
- If the computed tag already exists ⇒ exit **4** with
  `computed next tag already exists: <tag> (is your master up to date?)` (`:69-72`).
- Otherwise prints the tag on stdout (`:74`).

### 7.10 Other scripts present

`scripts/cleanroom-modpath.sh`, `scripts/cleanroom-reach-probe.sh`,
`scripts/cleanroom-sandbox.sh` exist in the repo (`scripts/` listing) and are not referenced by
the Justfile or any workflow file in `.github/workflows/`.

---

## 8. Justfile targets

`Justfile` — every recipe sources `scripts/cgo-env.sh` before compiling (`Justfile:1-5`).

| Target | Behavior |
|---|---|
| `default` | `just --list` (`Justfile:8-9`) |
| `setup` | On Darwin: requires Homebrew (`Install Homebrew first: https://brew.sh`, exit 1) then `brew install icu4c@78 zstd`; then sources `cgo-env.sh` and, if `CGO_CPPFLAGS` is set, persists `CGO_CPPFLAGS`/`CGO_LDFLAGS` via `go env -w`; otherwise prints that no extra flags are needed (`Justfile:14-27`) |
| `build` | `go build -buildvcs=false -ldflags "-X <version pkg>.Commit=$LIT_BUILD_COMMIT -X <version pkg>.Date=$LIT_BUILD_DATE" ./cmd/lit` — deliberately does **not** stamp `Version` (`Justfile:32-40`) |
| `test-short` | `go test -short ./...` (`Justfile:45-49`) |
| `test *args` | `go test -timeout 30m ${args:-./...}`; a later `-timeout` in args wins (`Justfile:58-63`) |
| `lint` | `golangci-lint run` (`Justfile:66-70`) |
| `install` | `./scripts/install.sh` (`Justfile:73-74`) |

---

## 9. Repo tooling (`tools/`)

### 9.1 `tools/testbudget`

- Invoked as `go test -short -json ./... | go run ./tools/testbudget`
  (`tools/testbudget/main.go:10-12`, `.github/workflows/ci.yml:52-54`).
- Reads `test2json` events (`Action`, `Package`, `Test`, `Elapsed`, `Output`) from stdin
  (`tools/testbudget/main.go:38-45`), replays package result lines as they arrive and a failed
  test's buffered output (`:82-156`), and reports unfinished tests when the stream ends
  (`tools/testbudget/main.go:128-150`).
- Prints a per-package `elapsed / budget` table headed
  `testbudget: per-package wall clock vs budget (budgets: tools/testbudget/budgets.go)`
  (`tools/testbudget/main.go:172-186`).
- Violation line: `test runtime budget exceeded: %s took %.1fs, budget is %ds (over by %.1fs, +%.0f%%) — see tools/testbudget/budgets.go before raising the number`
  (`tools/testbudget/main.go:191-197`), prefixed `::error::` under GitHub Actions
  (`tools/testbudget/main.go:208-215`).
- Exit codes: **2** on an unreadable stream (`tools/testbudget/main.go:200-204`), **1** when any
  budget is exceeded (`:216-218`), 0 otherwise. Test pass/fail is left to `go test` under
  `pipefail` (`tools/testbudget/main.go:20-24`).
- Budgets (`tools/testbudget/budgets.go:39-66`):
  - `internal/store` — 210 s
  - `internal/cli` — 340 s
  - `cmd/lit` — 50 s
  - `tools/licenses` — 50 s
  - every other package — `defaultBudget = 30 * time.Second` (`tools/testbudget/budgets.go:73`)

### 9.2 `tools/mkmanifest`

- Flags (`tools/mkmanifest/main.go:66-74`): `-version` (v-stripped, required), `-tag`
  (v-prefixed, required), `-commit` (required), `-date` (RFC3339, required), `-dist`
  (default `dist`), `-base-url` (required), `-out` (required).
- All required flags are trimmed in place and checked in a **fixed order** so the first-missing
  diagnostic is reproducible; missing ⇒ `mkmanifest: required flag <name> missing` and exit 1
  (`tools/mkmanifest/main.go:90-107`, `:341-344`). `-dist` is trimmed too (`:108-112`).
- `validateVerTag` (`tools/mkmanifest/main.go:165-186`): `-tag` must start with `v`; `-version`
  must **not**; `-tag` must contain no `/`, `\`, `..`, or whitespace.
- Schema range comes from `migrations.Baseline` and `migrations.MaxVersion()`
  (`tools/mkmanifest/main.go:117-120`, `:130-132`); `IsDev` is hard-coded `false` (`:131`).
- `collectArtifacts` (`tools/mkmanifest/main.go:206-284`) parses `<dist>/checksums.txt`:
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
- `platformFromFilename` (`tools/mkmanifest/main.go:307-336`) accepts exactly
  `lit_<version>_<goos>_<goarch>.{tar.gz,zip}` — four underscore-separated parts, literal
  project prefix `lit` (`:306`), and the version segment must equal `-version`.
- Output is written with `json.Encoder` at two-space indent, and `Close()` is checked explicitly
  on the success path (`tools/mkmanifest/main.go:136-156`).

### 9.3 `tools/licenses`

- Flags (`tools/licenses/main.go:114-122`): `-pkg` (default `./cmd/lit`), `-bundle` (default
  `THIRD_PARTY_LICENSES`), `-report` (default `LICENSE-REPORT.md`), `-sbom` (default empty ⇒
  skip), `-app-version` (default empty ⇒ omit), `-check` (bool), `-graph` (bool).
- `selectMode` (`tools/licenses/main.go:152-171`): `-check` and `-graph` together ⇒
  `-check and -graph select different operations; run the tool once for each`; otherwise
  `modeCheck`, `modeGraph`, or `modeGenerate`.
- Modes (`tools/licenses/main.go:34-56`, `:174-182`):
  - **generate** (default): resolves `go list -deps <pkg>` plus curated native C libraries,
    classifies once, and writes bundle + report (+ SBOM when `-sbom` is given).
  - **`-check`**: the CI license-policy gate over the link closure against
    `tools/licenses/policy.json`; writes no artifacts; non-zero exit on any violation.
  - **`-graph`**: audits `go list -m all` (the whole build list); writes no artifacts and does
    not gate.
- Any error ⇒ `licenses: %v` on stderr and exit **1** (`tools/licenses/main.go:127-132`).
- The graph acceptance test is env-gated on `LIT_LICENSE_GRAPH_AUDIT`
  (`tools/licenses/graph_test.go:35`).

### 9.4 `tools/session-analysis`

Present in the tree (`tools/session-analysis/`), with a `processed/` directory that is gitignored
(`.gitignore:4`) and `__pycache__/` ignored (`.gitignore:5`). It is not referenced by the
Justfile or any workflow in `.github/workflows/`.

---

## 10. Lint, attributes, dependency updates

- `.golangci.yml`: `version: "2"`; `linters.default: none`; the **only** enabled linter is
  `depguard` (`.golangci.yml:2-6`). Its single rule `lifecycle-boundary` runs in `list-mode: lax`
  over files matching `!**/internal/model/**` and denies importing
  `github.com/promptctl/links-issue-tracker/internal/model/lifecycle` with the message
  "lifecycle internals are owned by internal/model; use model hydration and capability APIs"
  (`.golangci.yml:8-17`).
- `.gitattributes`: `*.sql text eol=lf` — forces LF normalization so byte-comparison gates
  (a sha256-pinned baseline migration and a schema-snapshot drift canary) cannot be tripped by
  CRLF (`.gitattributes:1-12`).
- `.github/dependabot.yml`: `gomod` ecosystem at `/`, `weekly` schedule, with a `go-safe` group
  covering `minor` and `patch` update types; majors match no group and arrive as individual PRs
  (`.github/dependabot.yml:1-16`).
- `.gitignore` ignores `artifacts/`, `site/`, `.claude/worktrees/`,
  `tools/session-analysis/processed/`, `__pycache__/`, `.claude/*.lock`, the three build outputs
  `/lit`, `/licenses`, `/mkmanifest`, and the three generated compliance artifacts
  `/THIRD_PARTY_LICENSES`, `/LICENSE-REPORT.md`, `/SBOM.cdx.json` (`.gitignore:1-20`).

---

## 11. CI workflows

### 11.1 `.github/workflows/ci.yml` — "CI"

- Triggers: `push` to `master`, `pull_request` targeting `master`
  (`.github/workflows/ci.yml:3-7`).
- `permissions: contents: read` (`:9-10`). Concurrency group `ci-${{ github.ref }}` with
  `cancel-in-progress: true` (`:12-14`). Linux (`ubuntu-latest`) only, no build matrix
  (`:23-27`).
- Five jobs, all in parallel:
  1. **`build-and-test`** (`:29-54`): checkout@v4 → setup-go@v5 (`go-version-file: go.mod`) →
     `./.github/actions/install-dolt` → `go build ./cmd/lit` →
     `go test -short -json ./... | go run ./tools/testbudget` under `shell: bash` (pipefail is
     load-bearing, `:50-54`). No `-timeout` override by policy (`:45-47`).
  2. **`race`** (`:78-93`): same setup; `go test -short -race ./cmd/... ./internal/...` —
     scope excludes `tools/` deliberately (`:71-75`); no testbudget pipe (`:76-77`).
  3. **`verify`** (`:98-125`): `go mod tidy` then `git diff --exit-code go.mod go.sum`, failing
     with `::error::go.mod/go.sum are not tidy…` (`:112-119`); `grep -q -- "-buildvcs=false" scripts/install.sh`
     (`:121-122`); `bash scripts/install.sh` (`:124-125`).
  4. **`lint`** (`:131-143`): `golangci/golangci-lint-action@v8` pinned to `version: v2.8.0`.
  5. **`docs`** (`:149-162`): setup-python@v5 `3.x`; `pip install mkdocs==1.6.1 mkdocs-material==9.7.6`;
     `mkdocs build --strict`.

### 11.2 `.github/actions/install-dolt/action.yml`

Composite action; installs the pinned Dolt CLI used as a **test-only** oracle
(`.github/actions/install-dolt/action.yml:1-3`): downloads
`https://github.com/dolthub/dolt/releases/download/v1.81.10/install.sh`, runs it with `sudo bash`,
then `dolt version` (`:14-18`).

### 11.3 `.github/workflows/nightly.yml` — "Nightly full suite"

- Triggers: `schedule: cron '17 9 * * *'` and `workflow_dispatch`
  (`.github/workflows/nightly.yml:13-16`).
- `permissions: contents: read, issues: write` (`:18-20`). Concurrency group
  `nightly-full-suite`, `cancel-in-progress: false` (`:26-28`).
- Single job `full-suite`, `ubuntu-latest`, `timeout-minutes: 60` (`:31-36`).
- Steps: checkout → setup-go → install-dolt → `go build ./cmd/lit` →
  `go test -timeout 30m ./...` (no `-short`, so `testing.Short()` is false) (`:38-56`).
- Failure reporting step runs `if: failure() || cancelled()` — the `cancelled()` arm exists
  because a job timeout is reported as cancelled (`:58-63`). With `GH_TOKEN=github.token`, it
  `gh label create nightly-failure --force --description "The scheduled full test lane is red" --color B60205`,
  looks for an open issue with that label, and either comments on it or creates
  `Nightly full test lane is failing` with the run URL (`:64-84`).

### 11.4 `.github/workflows/license-graph-audit.yml` — "License graph audit"

- Triggers: `schedule: cron "0 9 * * 1"` (Mondays 09:00 UTC) and `workflow_dispatch`
  (`.github/workflows/license-graph-audit.yml:31-35`). `permissions: contents: read` (`:37-38`).
- Explicitly **not** a merge gate and not wired into ci.yml (`:5-16`); it reports and exits 0
  (`:22-29`).
- Job `audit` on `ubuntu-latest` (`:41-42`): checkout → setup-go with `cache: false` (cold cache
  is the acceptance criterion, `:49-55`) → `go test ./tools/licenses/ -run TestGraph -v` with
  `LIT_LICENSE_GRAPH_AUDIT: "1"` and **no** `-timeout` override (`:57-67`) →
  `go run ./tools/licenses -graph` (`:72-73`) → assert `git diff --exit-code go.mod go.sum`,
  failing with `::error::the license graph audit modified go.mod/go.sum — withGoSumPreserved is not holding.`
  (`:79-84`).

### 11.5 `.github/workflows/code-review.yml` — "AI Code Review"

- Header states the file is **generated** by `agent-code-review-setup/install.sh` and is
  reconverged (overwriting local edits) on every installer run
  (`.github/workflows/code-review.yml:1-4`).
- Trigger: `pull_request` with types `[opened, synchronize, reopened, ready_for_review]`
  (`:23-25`) — deliberately `pull_request`, not `pull_request_target` (`:7-11`).
- `permissions: contents: read, issues: write, pull-requests: write` (`:27-30`).
- Concurrency `code-review-${{ github.event.pull_request.number }}`, `cancel-in-progress: true`
  (`:34-36`).
- Job `review` ("Review") on `ubuntu-latest`, `timeout-minutes: 30` (`:39-47`).
- Steps: `actions/checkout` pinned to SHA `d23441a48e516b6c34aea4fa41551a30e30af803` (v6),
  checking out `github.event.pull_request.head.sha` with `persist-credentials: false`
  (`:49-63`); then `promptctl/copirate-code-review-agent@v1` (moving `v1` tag by design,
  `:65-68`) with inputs `CLAUDE_CODE_OAUTH_TOKEN`, `DEEPSEEK_API_KEY`, and
  `DEPENDENCY_DIFF: "true"` (`:77-91`). The comment records that both credentials must exist in
  both the Actions and the Dependabot secret stores (`:13-21`).

### 11.6 `.github/workflows/release-smoke.yml` — "Release smoke"

- Trigger: `pull_request` targeting `master` only (`.github/workflows/release-smoke.yml:31-33`).
- `permissions: contents: read, packages: read` (`:35-37`). Concurrency
  `release-smoke-${{ github.ref }}`, `cancel-in-progress: true` (`:39-41`).
- Job `smoke` on `ubuntu-latest` (`:44-45`). Steps:
  1. checkout@v4 with `fetch-depth: 0` (goreleaser derives the snapshot version from git,
     `:47-49`).
  2. setup-go@v5 with `cache: true` (`:51-54`).
  3. `SMOKE_GOCACHE=$RUNNER_TEMP/smoke-gocache` written into `$GITHUB_ENV` (`:62-63`).
  4. `go mod download` to populate `GOMODCACHE` before the read-only mount (`:71-72`).
  5. `docker/setup-buildx-action@v3` (`:74-75`).
  6. `tree=$(git rev-parse HEAD:build)` — the toolchain image tag is the **git tree hash of
     `build/`** (`:80-82`).
  7. `docker/login-action@v3` to `ghcr.io` with `github.actor` / `secrets.GITHUB_TOKEN`
     (`:84-89`).
  8. `docker pull ghcr.io/<owner>/lit-release-builder:smoke-<tree>` and retag to
     `lit-release-builder:smoke`, with `continue-on-error: true` (`:99-107`).
  9. Fallback `docker/build-push-action@v6` on `steps.pull.outcome == 'failure'`, building
     `build/Dockerfile.release` `target: smoke`, `load: true`, GHA cache scope
     `release-builder` (`:113-123`).
  10. `actions/cache@v4` for `$SMOKE_GOCACHE`, key `smoke-gocache-<hash(go.sum)>-<sha>` with
      restore-keys `smoke-gocache-<hash(go.sum)>-` then `smoke-gocache-` (`:133-140`).
  11. `docker run --rm` mounting the repo at `/go/src/app`, `GOMODCACHE` at `/go/pkg/mod:ro`,
      the cache dir at `/gocache` with `GOCACHE=/gocache`, invoking
      `build --single-target --snapshot --clean`; then `sudo chown -R` the cache back to the
      runner user (`:144-160`).
  12. Static/execution assertions on the produced binary (`:170-188`): fail if `readelf -l`
      shows `INTERP`, fail if `readelf -d` shows `NEEDED`, then run `./<bin> version` on the
      glibc runner and `docker run --rm … alpine:3.20 lit version` on musl.

### 11.7 `.github/workflows/release-validate.yml` — "Release validate"

- Triggers: `push` to `master` and `workflow_dispatch: {}` — never on `pull_request`
  (`.github/workflows/release-validate.yml:32-34`, `:9-12`).
- `permissions: contents: read, packages: write` (`:37-41`). Concurrency
  `release-validate-${{ github.ref }}` with `cancel-in-progress: false` (`:43-52`).
- This workflow **is** the release pipeline: when the newest CHANGELOG version has no tag, the
  `validate` job builds the real version and `publish` cuts the tag (`:14-18`).

**Job `license-gate`** (`:75-88`): `ubuntu-latest`, job-level `env: CGO_ENABLED: "1"` (required
so `go list -deps` resolves the same set the release links, `:64-70`). Steps: checkout@v4 →
setup-go@v5 (`cache: true`) → `go mod download` → `go run ./tools/licenses -check -pkg ./cmd/lit`.

**Job `validate`** (`:90-687`), `ubuntu-latest`, with job outputs
`release` and `version` from `steps.kind` (`:94-98`):

1. checkout@v4 `fetch-depth: 0`; setup-go@v5 `cache: true` (`:99-105`).
2. `SMOKE_GOCACHE=$RUNNER_TEMP/smoke-gocache` into `$GITHUB_ENV` (`:114-115`).
3. `go mod download` (`:124`).
4. **`Resolve build kind`** (`id: kind`, `:138-189`): requires `CHANGELOG.md` to exist
   (`::error::CHANGELOG.md not found at repo root…`, exit 1); takes the first `## [` heading that
   is not `## [Unreleased]`; none ⇒ `release=false`; a heading not matching
   `^## \[[0-9]+\.[0-9]+\.[0-9]+\]( - .*)?$` ⇒ `::error::newest CHANGELOG release heading is malformed`
   and exit 1; otherwise `git ls-remote --exit-code --tags origin refs/tags/v$VER` with a
   three-way branch on the exit code — `0` ⇒ `release=false`, `2` ⇒ `release=true` +
   `version=$VER`, anything else ⇒ `::error::git ls-remote failed (exit $r)…` and exit 1.
5. **`Generate third-party license bundle, report + SBOM`** (`:205-206`):
   `go run ./tools/licenses -pkg ./cmd/lit -bundle THIRD_PARTY_LICENSES -report LICENSE-REPORT.md -sbom SBOM.cdx.json -app-version "<kind.version>"`.
6. `docker/setup-buildx-action@v3` (`:211-212`); `docker/setup-qemu-action@v3` with
   `platforms: arm64` (`:216-218`).
7. `tree=$(git rev-parse HEAD:build)` (`:225-227`); GHCR login (`:229-234`).
8. `docker/build-push-action@v6` building `build/Dockerfile.release` (default `final` target)
   as `lit-release-builder:local`, `load: true`, GHA cache scope `release-builder`
   (`:236-245`).
9. `goreleaser check` inside the image (`:247-256`).
10. `actions/cache@v4` seeding the same `smoke-gocache-*` namespace release-smoke restores
    (`:273-281`).
11. **`Build artifacts`** (`:301-333`): `ARGS="release --snapshot --clean"`; for a pending
    release it first creates the ephemeral local tag `git tag "v<version>"` and switches to
    `ARGS="release --clean"`. The container mounts the repo, `GOMODCACHE:ro`, and the GOCACHE
    dir; then `sudo chown -R` the cache back.
12. **`Generate release manifest`** (`:336-354`): `sudo chown -R` `dist`, then reads
    `.version`, `.tag`, `.commit` (first 7 chars), `.date` from `dist/metadata.json` and runs
    `go run ./tools/mkmanifest -version … -tag … -commit … -date … -dist ./dist -base-url https://github.com/<repo>/releases/download -out ./dist/release-manifest.json`.
13. **`Assert manifest shape`** (`:356-406`): jq assertions that `.version` is a non-empty
    string; `.schema_support.min` is a number ≥ 1 and `.schema_support.max` is a number;
    `.artifacts` is non-empty with every `.platform` matching `^[a-z0-9]+/[a-z0-9]+$`, every
    `.url` starting `https://github.com/` and containing `/<tag>/`, and every `.sha256` matching
    `^[0-9a-f]{64}$`; and the sorted platform list equals the literal
    `["darwin/amd64","darwin/arm64","linux/amd64","linux/arm64","windows/amd64"]`, restated
    independently of `.goreleaser.yml` on purpose (`:388-401`).
14. **`Assert release archive carries the license bundle + report`** (`:408-486`): asserts
    exactly one `dist/lit_*_linux_amd64.tar.gz`, extracts `THIRD_PARTY_LICENSES`,
    `LICENSE-REPORT.md`, `FORKS.md`, and greps for the dolt module row, an `Apache License`
    within 12 lines of dolt's bundle header, the four native-library rows
    (`| icu | 75.1 | Unicode-3.0 |`, `| zstd | 1.5.6 | BSD-3-Clause |`,
    `| musl | 1.2.5 | MIT |`, `| compiler-rt | 0.14.0 | MIT AND Apache-2.0 WITH LLVM-exception |`)
    with a notice substring for each, the `| Module | Version | License | Source |` header, all
    three `replace` disclosures (promptctl/dolt, promptctl/go-mysql-server,
    `./internal/vendor/dolthub-driver`) in both artifacts, and a non-empty `FORKS.md`.
15. **`Stage the SBOM as a versioned release asset`** — `if: steps.kind.outputs.release == 'true'`
    (`:508-540`): copies `SBOM.cdx.json` to `dist/lit_<version>_sbom.cdx.json` and fails if
    `.metadata.component.version` differs from the filename version.
16. **`Validate the CycloneDX SBOM…`** — release-only (`:545-602`): downloads
    `cyclonedx-linux-x64` at pinned `CYCLONEDX_CLI_VERSION: v0.27.2` and verifies it against
    `CYCLONEDX_CLI_SHA256: 5e1595542a6367378a3944bbd3008caab3de65d572345361d3b9597b1dbbaaa0`
    with `sha256sum -c`, runs `validate --input-file … --input-format json --fail-on-errors`,
    then jq-asserts that `github.com/dolthub/dolt/go` appears exactly once with a non-empty
    version and carries a pedigree whose single descendant purl starts
    `pkg:golang/github.com/promptctl/dolt/go@`.
17. **`Assert linux binaries are static and run on glibc + musl (amd64 + arm64)`** (`:604-636`):
    for both arches, no `INTERP`, no `NEEDED`, and `docker run --platform linux/<arch> … alpine:3.20 lit version`;
    plus `lit version` natively for amd64.
18. **`Upload validated release artifact`** — release-only (`:638-660`):
    `actions/upload-artifact@v4` name `release-dist-<sha>`, paths
    `dist/lit_*_*_*.tar.gz`, `dist/lit_*_*_*.zip`, `dist/lit_*_sbom.cdx.json`,
    `dist/checksums.txt`, `dist/release-manifest.json`, `dist/metadata.json`;
    `retention-days: 14`; `if-no-files-found: error`.
19. **`Publish release-builder smoke image to GHCR`** (`:662-671`): pushes
    `ghcr.io/<owner>/lit-release-builder:smoke-<tree>` built at `target: smoke`.
20. **`Upload snapshot dist/ for inspection`** — `if: always()`, name
    `release-validate-dist-<sha>`, `retention-days: 7` (`:673-679`).

**Job `publish`** (`:689-780`): `needs: [validate, license-gate]`,
`if: needs.validate.outputs.release == 'true'`, `ubuntu-latest`,
`permissions: contents: write` (`:696-700`). Steps:

- `actions/download-artifact@v4` name `release-dist-<sha>` into `_artifact` (`:705-710`).
- Locate the real dist dir by `find _artifact -name metadata.json`, requiring exactly one, and
  export `DIST` (`:718-729`).
- Verify `.commit` in `metadata.json` equals `github.sha` and `.tag` equals
  `v<needs.validate.outputs.version>` (`:731-750`).
- `gh release create "$TAG" --repo <repo> --target <sha> --title "$TAG" --generate-notes <assets…>`
  with `GH_TOKEN: secrets.GITHUB_TOKEN`, where the asset list is assembled from files that
  actually exist (`nullglob` plus a `-f` test) and an empty list is refused with
  `::error::no release assets found under $DIST — refusing to publish an empty release`; a
  failing `gh release create` prints a recovery instruction naming
  `gh release delete $TAG --repo … --cleanup-tag --yes` (`:752-780`).

---

## 12. `.goreleaser.yml`

- `version: 2`, `project_name: lit` (`.goreleaser.yml:21`, `:23`). No `before.hooks` — the
  removed `go mod tidy` hook is called out at `.goreleaser.yml:25-30`.
- One build (`.goreleaser.yml:32-144`): `id: lit`, `main: ./cmd/lit`, `binary: lit`.
  - `env` sets `CGO_ENABLED=1` (`:47`) and, by Go template on `.Os`/`.Arch`, the zig cross
    wrappers: `zig-cc-aarch64-apple-darwin` / `zig-cc-x86_64-apple-darwin` (+ `CXX` twins)
    for darwin (`:55-64`), `zig-cc-x86_64-windows-gnu` (+ CXX) for windows (`:65-72`), and
    `zig-cc-x86_64-linux-musl` / `zig-cc-aarch64-linux-musl` (+ CXX) for linux (`:73-82`).
  - `CGO_CPPFLAGS=-I/opt/icu/{{ .Os }}_{{ .Arch }}/include` (`:93`).
  - `CGO_LDFLAGS=-L/opt/icu/{{ .Os }}_{{ .Arch }}/lib -static` on linux; without `-static`
    elsewhere (`:101-106`).
  - `GOFLAGS=-tags=icu_static` (`:113`).
  - `goos: [linux, darwin, windows]`, `goarch: [amd64, arm64]`, with `windows/arm64` ignored
    (`:116-127`) ⇒ five targets.
  - `flags: [-trimpath, -buildvcs=false]` (`:128-130`).
  - `ldflags: -s -w` plus
    `-X …/internal/version.Version={{ .Version }}`,
    `-X …/internal/version.Commit={{ .ShortCommit }}`,
    `-X …/internal/version.Date={{ .Date }}` (`:131-144`).
- Archives (`:146-189`): `name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`
  — required to match mkmanifest's parser (`:148-151`); `formats: [tar.gz]` with a windows
  override to `[zip]` (`:152-155`); `wrap_in_directory: false` (`:163`); `files:` `LICENSE`,
  `README*`, `THIRD_PARTY_LICENSES`, `LICENSE-REPORT.md`, `FORKS.md` (`:170-189`).
- Checksums: `name_template: "checksums.txt"`, `algorithm: sha256` (`:191-194`).
- Snapshot version template: `"{{ incpatch .Version }}-snapshot+{{.ShortCommit}}"` (`:196-199`).
- `release: disable: true` — goreleaser never publishes; the workflow's `publish` job does
  (`:213-214`).
- `changelog: disable: true` (`:224-225`).

---

## 13. Release build image (`build/`)

### 13.1 `build/Dockerfile.release`

Pinned ARGs (`build/Dockerfile.release:62-74`):

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

- **`base`** = `golang:${GO_VERSION}-bookworm` (`:81`): apt packages (`:94`), goreleaser and zig
  downloads (`:104`, `:115`), a `zig-ar` shim (`printf '#!/bin/sh\nexec zig ar "$@"\n'`, `:130`),
  generated `zig-cc-*`/`zig-cxx-*` wrappers (`:140`), the ICU source download (`:152-153`), and
  `build/icu-cross-build.sh` installed as `/usr/local/bin/icu-cross-build` (`:161-162`).
- **`native-icu`** (`:169-170`): a native ICU build used as `--with-cross-build`.
- **`cross-linux-amd64`**, **`cross-linux-arm64`**, **`cross-windows-amd64`** (`:181`, `:190`,
  `:199`) — parallel `icu-cross-build <goos> <goarch> …` stages.
- **`darwin-toolchain`** (`:214-265`): downloads the pinned macOS SDK (`:224`), copies
  `build/zig-macos-stubs/tzfile.h` to `/usr/local/include/zig-macos-stubs/tzfile.h` (`:243`),
  and generates the darwin wrappers with
  `SDK_FLAGS="-F/opt/macos-sdk/System/Library/Frameworks -L/opt/macos-sdk/usr/lib"` (`:251`).
- **`cross-darwin-arm64`**, **`cross-darwin-amd64`** (`:267`, `:276`).
- **`smoke`** (`:290-322`): `FROM base`, `SHELL ["/bin/bash","-c"]` (`:293`), copies only
  `/opt/icu/linux_amd64` (`:295`), verifies `include/unicode/regex.h` and a `libicuuc.a`
  (`:299-304`), `WORKDIR /go/src/app` (`:306`),
  `git config --global --add safe.directory /go/src/app` (`:320`), and
  `ENTRYPOINT ["/usr/local/bin/goreleaser"]` (`:322`).
- **`final`** (`:330-386`): `FROM smoke`, adds the remaining four ICU prefixes (`:332-335`), the
  macOS SDK plus a re-created `/opt/macos-sdk` symlink (`:348-349`), the tzfile stub (`:350`),
  and the four darwin wrappers (`:351-356`). Two build-time verification `RUN`s: all four extra
  ICU prefixes carry `unicode/regex.h` and a `libicuuc|sicuuc` archive (`:360-369`), and the
  darwin link inputs resolve — `CoreFoundation.framework` present, all four wrappers executable,
  the tzfile stub present (`:373-386`).

### 13.2 `build/icu-cross-build.sh`

- Positional args: `goos`, `goarch`, host triplet, `CC`, `CXX`, `AR`, `RANLIB`, optional
  `extra_cppflags` (`build/icu-cross-build.sh:4-20`, `:36-37`).
- Reads `/tmp/icu-build/src/` and `/tmp/icu-build/native-build/source/`; writes
  `/opt/icu/<goos>_<goarch>/{include,lib}` (`:22-27`).
- Copies the source tree to `/tmp/icu-build/cross-<goos>-<goarch>`, then configures with
  `--host`, `--prefix`, `--with-cross-build=/tmp/icu-build/native-build/source`,
  `--enable-static --disable-shared --disable-tests --disable-samples --disable-tools
  --disable-extras --disable-icuio --disable-layoutex` (`:43-50`); `CPPFLAGS` is set solely
  from `extra_cppflags` (nothing inherited, `:15-18`, `:45`).
- `make -j$(nproc)`, pre-creates `$prefix/{bin,lib,include}` (needed because `--disable-tools`
  leaves `bin/` uncreated), `make install`, then removes the build dir (`:60-66`).

### 13.3 `build/zig-macos-stubs/tzfile.h`

A four-line stub defining `TZDIR "/var/db/timezone/zoneinfo"` and `TZDEFAULT "/etc/localtime"`,
because ICU 75.1's `putil.cpp` includes `tzfile.h` unconditionally under `__APPLE__` and zig's
bundled SDK omits it (`build/zig-macos-stubs/tzfile.h:1-13`).

---

## 14. Shipped Claude plugin and the repo's own hook wiring

### 14.1 `.claude-plugin/marketplace.json`

A local marketplace named `links-marketplace`, description
"Local marketplace for links plugin development", owner `links`, listing one plugin: `links`,
source `./claude-plugin`, description "Issue tracking workflow integration for links",
version `0.1.0` (`.claude-plugin/marketplace.json:1-15`).

### 14.2 `claude-plugin/.claude-plugin/plugin.json`

The **entire** plugin is one manifest — `find claude-plugin -type f` yields only this file. It
declares name `links`, description "Issue tracker integration for links repositories.",
version `0.1.0`, and two hooks, each with an empty matcher and a single command hook running
`lit quickstart --refresh`:

- `SessionStart` (`claude-plugin/.claude-plugin/plugin.json:6-16`)
- `PreCompact` (`claude-plugin/.claude-plugin/plugin.json:17-27`)

There are no commands, agents, skills, or MCP servers in the shipped plugin.

### 14.3 `.claude/settings.json` (this repo's dogfooding wiring)

One `SessionStart` hook with an empty matcher running the command
`.claude/hooks/session-start.sh` (`.claude/settings.json:1-15`).

### 14.4 `.claude/hooks/session-start.sh`

`#!/usr/bin/env bash` under `set -euo pipefail` (`:1-3`). Reads the SessionStart hook's JSON from
stdin (`:5`), extracts `session_id` with `grep -o '"session_id":"[^"]*"' || true` and
`head -1 | cut -d'"' -f4` (`:6-7`), and — only when non-empty — prints
`Your Claude Code session id is: ${session_id}. When using lit, your assignee identity is claude_${session_id}.`
(`:9-11`). That identity string is the same one `resolveIdentity` produces from
`CLAUDE_CODE_SESSION_ID` (`internal/cli/cli.go:1173-1174`).

### 14.5 `.claude/settings.local.json`

Local (untracked-style) permissions: allow list `Read(//Users/bmf/**)`,
`Read(//Users/bmf/code/dotfiles/config/claude.zai/**)`, `Bash(gh pr *)`,
`Bash(lit quickstart *)`, `Bash(lit backlog *)`, `Bash(lit show *)`, `Bash(lit *)`;
`enabledMcpjsonServers: ["chromedevtools/chrome-devtools-mcp"]`;
`enableAllProjectMcpServers: true` (`.claude/settings.local.json:1-17`).

---

## 15. Docs site configuration (gated by CI)

`mkdocs.yml` — `site_name: links`, `site_description: Agent-native issue tracker`,
`repo_url: https://github.com/promptctl/links-issue-tracker`, `docs_dir: docs`, theme
`material` with `navigation.sections`, `navigation.expand`, `navigation.instant`,
`content.code.copy`, markdown extensions `admonition` and `toc` with `permalink: true`, and a
seven-entry `nav` tree (`mkdocs.yml:1-37`). `ci.yml`'s `docs` job builds it under `--strict`
so a missing page or an out-of-tree link fails the merge gate
(`.github/workflows/ci.yml:145-162`).

---

## 16. Command registry structure (startup-relevant)

- Commands are data, not imperative registration: `CommandSpec{Name, Summary, Long, GroupID,
  Run, Subcommands, Hidden}` (`internal/cli/register.go:17-37`), with
  `SubcommandSpec{Name, Subcommands}` for family commands (`internal/cli/register.go:41-45`) and
  `CommandRunner func(args []string) error` as the fully-wrapped handler
  (`internal/cli/register.go:47-52`).
- `Hidden` keeps a dispatchable command out of root `--help` and the completion projection
  without removing it from dispatch (`internal/cli/register.go:28-36`); the same property exists
  per-subcommand row (`internal/cli/register.go:83-90`).
- Help groups, in render order (`internal/cli/register.go:59-75`):
  `bootstrap` "Human Bootstrap", `operations` "Agent Operations", `structure`
  "Dependencies & Structure", `data` "Sync & Data", `maintenance` "Setup & Maintenance",
  `retention` "Issue Retention", `guidance` "Guidance & Tooling".
- Two standard help blurbs are defined for the two audiences:
  `humanBootstrapHelp` = "Human bootstrap command. Run once per repository/worktree setup before
  autonomous agent operations." and `agentCommandHelp` = "Agent-facing operational command."
  (`internal/cli/cli.go:30-33`).
- The registry is applied by `applyRegistry(root, commandGroups, commandSpecs(ctx, stdout, stderr))`
  (`internal/cli/cli.go:90`).
