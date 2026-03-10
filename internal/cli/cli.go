package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/backup"
	"github.com/bmf/links-issue-tracker/internal/beads"
	"github.com/bmf/links-issue-tracker/internal/doltcli"
	"github.com/bmf/links-issue-tracker/internal/merge"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/query"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/syncfile"
	"github.com/bmf/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

var missingRemoteBranchPattern = regexp.MustCompile(`branch "([^"]+)" not found on remote`)

const outputModeEnvVar = "LIT_OUTPUT"

const (
	commandLockRetryDelay = 50 * time.Millisecond
	commandLockStaleAfter = 10 * time.Minute
)

var commandLockPIDRunning = isCommandLockPIDRunning

type outputMode string

const (
	outputModeAuto outputMode = "auto"
	outputModeText outputMode = "text"
	outputModeJSON outputMode = "json"
)

type outputModeWriter struct {
	io.Writer
	mode outputMode
}

func (w outputModeWriter) linksOutputMode() outputMode {
	return w.mode
}

type outputModeProvider interface {
	linksOutputMode() outputMode
}

const (
	humanBootstrapHelp = "Human bootstrap command. Run once per repository/worktree setup before autonomous agent operations."
	agentCommandHelp   = "Agent-facing operational command. Prefer deterministic machine-readable output (`--json` or `--output json`) in automation."
)

func Run(ctx context.Context, stdout io.Writer, stderr io.Writer, args []string) error {
	normalizedArgs, resolvedOutputMode, err := parseGlobalOutputMode(args, stdout)
	if err != nil {
		return err
	}
	stdout = outputModeWriter{Writer: stdout, mode: resolvedOutputMode}
	root := newRootCommand(ctx, stdout, stderr)
	root.SetArgs(normalizedArgs)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SilenceErrors = true
	root.SilenceUsage = true
	err = root.ExecuteContext(ctx)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func newRootCommand(ctx context.Context, stdout io.Writer, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "lit",
		Short: "Worktree-native issue tracker",
		Long: strings.Join([]string{
			"Worktree-native issue tracker with Dolt-backed sync.",
			"",
			"Global output mode:",
			"  default auto (TTY -> text, non-TTY -> json)",
			"  --json shorthand for JSON compatibility",
			"  --output auto|text|json to force mode",
			"  LIT_OUTPUT environment default",
		}, "\n"),
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q", args[0])
			}
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddGroup(
		&cobra.Group{ID: "bootstrap", Title: "Human Bootstrap"},
		&cobra.Group{ID: "operations", Title: "Agent Operations"},
		&cobra.Group{ID: "structure", Title: "Dependencies & Structure"},
		&cobra.Group{ID: "data", Title: "Sync & Data"},
		&cobra.Group{ID: "maintenance", Title: "Setup & Maintenance"},
		&cobra.Group{ID: "guidance", Title: "Guidance & Tooling"},
	)

	addGroupedPassthrough(root, "bootstrap", "init", "Initialize links", func(args []string) error {
		return runWithWorkspace(ctx, append([]string{"init"}, args...), false, func(ws workspace.Info) error {
			return runInit(ctx, stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "guidance", "quickstart", "Agent quickstart workflow", func(args []string) error {
		return runWithPreflight(append([]string{"quickstart"}, args...), func() error {
			return runQuickstart(stdout, args)
		})
	})
	addGroupedPassthrough(root, "guidance", "completion", "Generate shell completion script", func(args []string) error {
		return runCompletion(stdout, args)
	})
	addGroupedPassthrough(root, "maintenance", "hooks", "Install git hook automation", func(args []string) error {
		if err := validateHooksCommandPath(args); err != nil {
			return err
		}
		return runWithWorkspace(ctx, append([]string{"hooks"}, args...), false, func(ws workspace.Info) error {
			return runHooks(stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "migrate", "Migrate from Beads to links", func(args []string) error {
		if err := validateMigrateCommandPath(args); err != nil {
			return err
		}
		return runWithWorkspace(ctx, append([]string{"migrate"}, args...), false, func(ws workspace.Info) error {
			return runMigrate(stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "data", "sync", "Mirror Dolt data through git remotes", func(args []string) error {
		if err := validateSyncCommandPath(args); err != nil {
			return err
		}
		return runWithWorkspace(ctx, append([]string{"sync"}, args...), true, func(ws workspace.Info) error {
			return runSync(ctx, stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "operations", "new", "Create an issue", func(args []string) error {
		return runWithApp(ctx, append([]string{"new"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runNew(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "ready", "List open work", func(args []string) error {
		return runWithApp(ctx, append([]string{"ready"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runReady(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "ls", "List issues", func(args []string) error {
		return runWithApp(ctx, append([]string{"ls"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runList(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "show", "Show issue details", func(args []string) error {
		return runWithApp(ctx, append([]string{"show"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runShow(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "start", "Claim issue work", func(args []string) error {
		return runWithApp(ctx, append([]string{"start"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "start")
		})
	})
	addGroupedPassthrough(root, "operations", "done", "Mark claimed work complete", func(args []string) error {
		return runWithApp(ctx, append([]string{"done"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "done")
		})
	})
	addGroupedPassthrough(root, "operations", "close", "Close issue(s)", func(args []string) error {
		return runWithApp(ctx, append([]string{"close"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "close")
		})
	})
	addGroupedPassthrough(root, "operations", "open", "Reopen issue(s)", func(args []string) error {
		return runWithApp(ctx, append([]string{"open"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "reopen")
		})
	})
	addGroupedPassthrough(root, "operations", "archive", "Archive issue(s)", func(args []string) error {
		return runWithApp(ctx, append([]string{"archive"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "archive")
		})
	})
	addGroupedPassthrough(root, "operations", "delete", "Delete issue(s)", func(args []string) error {
		return runWithApp(ctx, append([]string{"delete"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "delete")
		})
	})
	addGroupedPassthrough(root, "operations", "unarchive", "Unarchive issue(s)", func(args []string) error {
		return runWithApp(ctx, append([]string{"unarchive"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "unarchive")
		})
	})
	addGroupedPassthrough(root, "operations", "restore", "Restore deleted issue(s)", func(args []string) error {
		return runWithApp(ctx, append([]string{"restore"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "restore")
		})
	})
	addGroupedPassthrough(root, "operations", "comment", "Add issue comments", func(args []string) error {
		if err := validateCommentCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"comment"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runComment(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "label", "Manage labels", func(args []string) error {
		if err := validateLabelCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"label"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runLabel(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "structure", "parent", "Manage parent relationships", func(args []string) error {
		if err := validateParentCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"parent"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runParent(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "structure", "children", "List child issues", func(args []string) error {
		return runWithApp(ctx, append([]string{"children"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runChildren(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "structure", "dep", "Manage dependency edges", func(args []string) error {
		if err := validateDepCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"dep"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runDep(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "export", "Export workspace snapshot", func(args []string) error {
		return runWithApp(ctx, append([]string{"export"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runExport(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "beads", "Import/export Beads databases", func(args []string) error {
		if err := validateBeadsCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"beads"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runBeads(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "workspace", "Show workspace metadata", func(args []string) error {
		return runWithApp(ctx, append([]string{"workspace"}, args...), func(_ context.Context, ap *app.App) error {
			return runWorkspace(stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "doctor", "Health check", func(args []string) error {
		return runWithApp(ctx, append([]string{"doctor"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runDoctor(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "fsck", "Integrity check and optional repair", func(args []string) error {
		return runWithApp(ctx, append([]string{"fsck"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runFsck(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "backup", "Backup snapshot operations", func(args []string) error {
		if err := validateBackupCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"backup"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runBackup(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "recover", "Recover from backup or sync", func(args []string) error {
		return runWithApp(ctx, append([]string{"recover"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runRecover(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "bulk", "Bulk issue operations", func(args []string) error {
		if err := validateBulkCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, append([]string{"bulk"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runBulk(commandCtx, stdout, ap, args)
		})
	})
	return root
}

func addGroupedPassthrough(root *cobra.Command, groupID string, name string, summary string, run func(args []string) error) {
	cmd := newPassthroughCommand(name, summary, run)
	cmd.GroupID = groupID
	root.AddCommand(cmd)
}

func newPassthroughCommand(name string, summary string, run func(args []string) error) *cobra.Command {
	description := agentCommandHelp
	if name == "init" {
		description = humanBootstrapHelp
	}
	return &cobra.Command{
		Use:                name,
		Short:              summary,
		Long:               description,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(args)
		},
	}
}

func runWithPreflight(commandArgs []string, run func() error) error {
	_, _, err := enforceBeadsPreflight(commandArgs)
	if err != nil {
		return err
	}
	return run()
}

func validateNestedCommandPath(args []string, usage string, commands ...string) error {
	// [LAW:single-enforcer] Nested command path validation is centralized here so invalid/help paths fail before startup side effects.
	if len(args) == 0 {
		return errors.New(usage)
	}
	subcommand := strings.TrimSpace(args[0])
	if subcommand == "" || subcommand == "--help" || subcommand == "-h" || strings.HasPrefix(subcommand, "-") {
		return errors.New(usage)
	}
	for _, command := range commands {
		if subcommand == command {
			return nil
		}
	}
	return errors.New(usage)
}

func validateHooksCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit hooks install [--json]", "install")
}

func validateMigrateCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit migrate beads [--apply] [--json]", "beads")
}

func validateSyncCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit sync <status|remote|fetch|pull|push> ...", "status", "remote", "fetch", "pull", "push")
}

func validateCommentCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit comment add <id> --body <text>", "add")
}

func validateLabelCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit label <add|rm> ...", "add", "rm")
}

func validateParentCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit parent <set|clear> ...", "set", "clear")
}

func validateDepCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit dep <add|rm|ls> ...", "add", "rm", "ls")
}

func validateBeadsCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit beads <import|export> --db <path> [--json]", "import", "export")
}

func validateBackupCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit backup <create|list|restore> ...", "create", "list", "restore")
}

func validateBulkCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit bulk <label|close|archive|import> ...", "label", "close", "archive", "import")
}

func runWithWorkspace(ctx context.Context, commandArgs []string, requireDoltReady bool, run func(workspace.Info) error) error {
	preflightWorkspace, hasPreflightWorkspace, err := enforceBeadsPreflight(commandArgs)
	if err != nil {
		return err
	}
	// [LAW:one-source-of-truth] Reuse the preflight workspace resolution when available.
	ws := preflightWorkspace
	if !hasPreflightWorkspace {
		ws, err = resolveWorkspaceFromWD()
		if err != nil {
			return err
		}
	}
	if requireDoltReady {
		if _, err := doltcli.RequireMinimumVersion(ctx, ws.RootDir, doltcli.MinSupportedVersion); err != nil {
			return err
		}
		if err := store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID); err != nil {
			return err
		}
	}
	return run(ws)
}

func runWithApp(ctx context.Context, commandArgs []string, run func(context.Context, *app.App) error) error {
	_, _, err := enforceBeadsPreflight(commandArgs)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return fmt.Errorf("links requires running inside a git repository/worktree")
		}
		return err
	}
	// [LAW:single-enforcer] CLI acquires a workspace command lock before opening app state so startup and mutation phases cannot overlap across commands.
	releaseWorkspaceLock, err := acquireWorkspaceCommandLock(ctx, ws.DatabasePath)
	if err != nil {
		return err
	}
	defer releaseWorkspaceLock()

	ap, err := app.Open(ctx, cwd)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return fmt.Errorf("links requires running inside a git repository/worktree")
		}
		return err
	}
	defer ap.Close()
	// [LAW:single-enforcer] Command-level workspace mutation lock is acquired once at CLI dispatch to prevent overlapping write phases.
	ctx, releaseMutationLock, err := ap.Store.AcquireMutationLock(ctx)
	if err != nil {
		return err
	}
	defer releaseMutationLock()
	return run(ctx, ap)
}

func enforceBeadsPreflight(commandArgs []string) (workspace.Info, bool, error) {
	if shouldBypassBeadsPreflight(commandArgs) {
		return workspace.Info{}, false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return workspace.Info{}, false, fmt.Errorf("get cwd: %w", err)
	}
	ws, resolveErr := workspace.Resolve(cwd)
	if resolveErr == nil {
		if err := requireBeadsMigrationPreflight(ws); err != nil {
			return workspace.Info{}, false, err
		}
		return ws, true, nil
	}
	if !errors.Is(resolveErr, workspace.ErrNotGitRepo) {
		return workspace.Info{}, false, resolveErr
	}
	return workspace.Info{}, false, nil
}

func resolveWorkspaceFromWD() (workspace.Info, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return workspace.Info{}, fmt.Errorf("get cwd: %w", err)
	}
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return workspace.Info{}, fmt.Errorf("links requires running inside a git repository/worktree")
		}
		return workspace.Info{}, err
	}
	return ws, nil
}

func parseGlobalOutputMode(args []string, stdout io.Writer) ([]string, outputMode, error) {
	// [LAW:single-enforcer] Global output precedence (--output/--json/env/auto) is enforced exactly once at CLI entry.
	mode, err := modeFromEnv()
	if err != nil {
		return nil, "", err
	}
	index := 0
	for index < len(args) {
		switch {
		case args[index] == "--":
			index++
			goto done
		case args[index] == "--json":
			mode = outputModeJSON
			index++
		case strings.HasPrefix(args[index], "--json="):
			jsonValue := strings.TrimSpace(strings.TrimPrefix(args[index], "--json="))
			parsed, parseErr := strconv.ParseBool(jsonValue)
			if parseErr != nil {
				return nil, "", fmt.Errorf("invalid --json value %q (expected true|false)", jsonValue)
			}
			if parsed {
				mode = outputModeJSON
			} else {
				mode = outputModeText
			}
			index++
		case args[index] == "--output":
			if index+1 >= len(args) {
				return nil, "", errors.New("usage: lit [--output auto|text|json] [--json] [command]")
			}
			parsedMode, parseErr := parseOutputMode(args[index+1])
			if parseErr != nil {
				return nil, "", parseErr
			}
			mode = parsedMode
			index += 2
		default:
			if strings.HasPrefix(args[index], "--output=") {
				parsedMode, parseErr := parseOutputMode(strings.TrimPrefix(args[index], "--output="))
				if parseErr != nil {
					return nil, "", parseErr
				}
				mode = parsedMode
				index++
				continue
			}
			goto done
		}
	}

done:
	if mode == outputModeAuto {
		mode = detectOutputMode(stdout)
	}
	return args[index:], mode, nil
}

func parseFlagSet(fs *flag.FlagSet, args []string, helpOutput io.Writer) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// [LAW:single-enforcer] Flag help rendering is normalized in one parser path.
			fs.SetOutput(helpOutput)
			fs.Usage()
		}
		return err
	}
	return nil
}

func modeFromEnv() (outputMode, error) {
	config := viper.New()
	if err := config.BindEnv("output", outputModeEnvVar); err != nil {
		return "", fmt.Errorf("bind %s: %w", outputModeEnvVar, err)
	}
	raw := strings.TrimSpace(strings.ToLower(config.GetString("output")))
	if raw == "" {
		return outputModeAuto, nil
	}
	return parseOutputMode(raw)
}

func acquireWorkspaceCommandLock(ctx context.Context, databasePath string) (func(), error) {
	lockDir := filepath.Clean(databasePath)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("acquire workspace command lock: create lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, ".links-command.lock")
	for {
		release, lockErr := tryAcquireCommandLockFile(lockPath)
		if lockErr == nil {
			return release, nil
		}
		if !errors.Is(lockErr, os.ErrExist) {
			return nil, fmt.Errorf("acquire workspace command lock: %w", lockErr)
		}
		if staleErr := removeStaleCommandLockFile(lockPath, commandLockStaleAfter); staleErr != nil {
			return nil, fmt.Errorf("acquire workspace command lock: %w", staleErr)
		}
		if waitErr := waitForCommandLock(ctx, commandLockRetryDelay); waitErr != nil {
			return nil, waitErr
		}
	}
}

func tryAcquireCommandLockFile(lockPath string) (func(), error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		_ = os.Remove(lockPath)
		return nil, err
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(lockPath)
		return nil, closeErr
	}
	return func() {
		_ = os.Remove(lockPath)
	}, nil
}

func removeStaleCommandLockFile(lockPath string, staleAfter time.Duration) error {
	info, err := os.Stat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	isStaleByAge := time.Since(info.ModTime()) > staleAfter
	isStaleByOwner, err := commandLockOwnedByDeadProcess(lockPath)
	if err != nil {
		return err
	}
	if !isStaleByAge && !isStaleByOwner {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func commandLockOwnedByDeadProcess(lockPath string) (bool, error) {
	// [LAW:single-enforcer] Command-lock owner liveness classification is centralized here to avoid divergent stale-lock behavior.
	pid, hasOwnerPID, err := readCommandLockOwnerPID(lockPath)
	if err != nil {
		return false, err
	}
	if !hasOwnerPID {
		return false, nil
	}
	running, err := commandLockPIDRunning(pid)
	if err != nil {
		return false, err
	}
	return !running, nil
}

func readCommandLockOwnerPID(lockPath string) (int, bool, error) {
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	pidText := strings.TrimSpace(string(content))
	if pidText == "" {
		return 0, false, nil
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, false, nil
	}
	return pid, true, nil
}

func isCommandLockPIDRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	// Unknown probe errors are treated as running to avoid removing an active lock.
	return true, nil
}

func waitForCommandLock(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseOutputMode(raw string) (outputMode, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case string(outputModeAuto):
		return outputModeAuto, nil
	case string(outputModeText):
		return outputModeText, nil
	case string(outputModeJSON):
		return outputModeJSON, nil
	default:
		return "", fmt.Errorf("unsupported output mode %q (expected auto|text|json)", raw)
	}
}

func detectOutputMode(stdout io.Writer) outputMode {
	file, ok := stdout.(*os.File)
	if !ok {
		return outputModeJSON
	}
	if term.IsTerminal(int(file.Fd())) {
		return outputModeText
	}
	return outputModeJSON
}

func runNew(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	issueType := fs.String("type", "task", "Issue type: task|feature|bug|chore|epic")
	priority := fs.Int("priority", 2, "Priority 0..4 (lower is more important)")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title: *title, Description: *description, IssueType: *issueType, Priority: *priority, Assignee: *assignee, Labels: splitCSV(*labels),
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runList(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	status := fs.String("status", "", "Filter by status: open|in_progress|closed")
	issueType := fs.String("type", "", "Filter by issue type")
	assignee := fs.String("assignee", "", "Filter by assignee")
	priorityMin := fs.Int("priority-min", -1, "Minimum priority 0..4")
	priorityMax := fs.Int("priority-max", -1, "Maximum priority 0..4")
	search := fs.String("search", "", "Search title and description text")
	ids := fs.String("ids", "", "Comma-separated issue IDs")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	hasComments := fs.Bool("has-comments", false, "Only include issues with comments")
	includeArchived := fs.Bool("include-archived", false, "Include archived issues")
	includeDeleted := fs.Bool("include-deleted", false, "Include deleted issues")
	updatedAfter := fs.String("updated-after", "", "Only include issues updated at or after RFC3339 timestamp")
	updatedBefore := fs.String("updated-before", "", "Only include issues updated at or before RFC3339 timestamp")
	queryExpr := fs.String("query", "", "Query language: status:in_progress type:task priority<=2 has:comments text")
	sortExpr := fs.String("sort", "", "Sort fields, e.g. priority:asc,updated_at:desc")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	format := fs.String("format", "lines", "Output format: lines|table")
	limit := fs.Int("limit", 0, "Limit results")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	filter := store.ListIssuesFilter{
		Status:          strings.TrimSpace(*status),
		IssueType:       strings.TrimSpace(*issueType),
		Assignee:        strings.TrimSpace(*assignee),
		IncludeArchived: *includeArchived,
		IncludeDeleted:  *includeDeleted,
		Limit:           *limit,
	}
	if strings.TrimSpace(*sortExpr) != "" {
		sortSpecs, err := parseSortSpecs(*sortExpr)
		if err != nil {
			return err
		}
		filter.SortBy = sortSpecs
	}
	if visited["priority-min"] {
		value := *priorityMin
		filter.PriorityMin = &value
	}
	if visited["priority-max"] {
		value := *priorityMax
		filter.PriorityMax = &value
	}
	if visited["search"] {
		filter.SearchTerms = append(filter.SearchTerms, strings.TrimSpace(*search))
	}
	if visited["ids"] {
		filter.IDs = splitCSV(*ids)
	}
	if visited["labels"] {
		filter.LabelsAll = splitCSV(*labels)
	}
	if visited["has-comments"] {
		value := *hasComments
		filter.HasComments = &value
	}
	if visited["updated-after"] {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*updatedAfter))
		if err != nil {
			return fmt.Errorf("parse --updated-after: %w", err)
		}
		filter.UpdatedAfter = &parsed
	}
	if visited["updated-before"] {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*updatedBefore))
		if err != nil {
			return fmt.Errorf("parse --updated-before: %w", err)
		}
		filter.UpdatedBefore = &parsed
	}
	if strings.TrimSpace(*queryExpr) != "" {
		parsed, err := query.Parse(*queryExpr)
		if err != nil {
			return err
		}
		filter, err = query.Merge(filter, parsed.Filter)
		if err != nil {
			return err
		}
	}
	issues, err := ap.Store.ListIssues(ctx, filter)
	if err != nil {
		return err
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		return writeJSON(stdout, issues)
	}
	columns := parseColumns(*columnsExpr)
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "", "lines":
		return printIssueLines(stdout, issues, columns)
	case "table":
		return printIssueTable(stdout, issues, columns)
	default:
		return fmt.Errorf("unsupported --format %q", *format)
	}
}

func runReady(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	assignee := fs.String("assignee", "", "Filter by assignee")
	limit := fs.Int("limit", 0, "Limit results")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	format := fs.String("format", "lines", "Output format: lines|table")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit ready [--assignee <user>] [--limit N] [--format lines|table] [--columns ...] [--json]")
	}
	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{
		Status:          "open",
		Assignee:        strings.TrimSpace(*assignee),
		IncludeArchived: false,
		IncludeDeleted:  false,
		Limit:           *limit,
		SortBy: []store.SortSpec{
			{Field: "priority"},
			{Field: "updated_at", Desc: true},
		},
	})
	if err != nil {
		return err
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		return writeJSON(stdout, issues)
	}
	columns := parseColumns(*columnsExpr)
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "", "lines":
		return printIssueLines(stdout, issues, columns)
	case "table":
		return printIssueTable(stdout, issues, columns)
	default:
		return fmt.Errorf("unsupported --format %q", *format)
	}
}

func runShow(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit show <id>")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit show <id>")
	}
	detail, err := ap.Store.GetIssueDetail(ctx, positional[0])
	if err != nil {
		return err
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		return writeJSON(stdout, detail)
	}
	return printIssueDetail(stdout, detail)
}

func runTransition(ctx context.Context, stdout io.Writer, ap *app.App, args []string, action string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "Lifecycle transition reason")
	by := fs.String("by", os.Getenv("USER"), "Lifecycle actor")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: lit %s <id> --reason <text>", transitionCommandName(action))
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit %s <id> --reason <text>", transitionCommandName(action))
	}
	issue, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID:   positional[0],
		Action:    action,
		Reason:    *reason,
		CreatedBy: *by,
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runComment(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	positional, flagArgs := splitArgs(args[1:], 1)
	fs := flag.NewFlagSet("comment add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	body := fs.String("body", "", "Comment body")
	by := fs.String("by", os.Getenv("USER"), "Comment author")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	comment, err := ap.Store.AddComment(ctx, store.AddCommentInput{IssueID: positional[0], Body: *body, CreatedBy: *by})
	if err != nil {
		return err
	}
	return printValue(stdout, comment, *jsonOut, func(w io.Writer, v any) error {
		c := v.(model.Comment)
		_, err := fmt.Fprintf(w, "%s %s\n", c.IssueID, c.ID)
		return err
	})
}

func runDep(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit dep <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := flag.NewFlagSet("dep add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit dep add <src-id> <dst-id> [--type blocks|parent-child|related-to]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep add <src-id> <dst-id> [--type blocks|parent-child|related-to]")
		}
		rel, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: positional[0], DstID: positional[1], Type: *relType, CreatedBy: *by})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			r := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --%s--> %s\n", r.SrcID, r.Type, r.DstID)
			return err
		})
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := flag.NewFlagSet("dep rm", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit dep rm <src-id> <dst-id> [--type ...]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep rm <src-id> <dst-id> [--type ...]")
		}
		if err := ap.Store.RemoveRelation(ctx, positional[0], positional[1], *relType); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	case "ls":
		positional, flagArgs := splitArgs(args[1:], 1)
		fs := flag.NewFlagSet("dep ls", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		relType := fs.String("type", "", "Filter relation type")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
		}
		relations, err := ap.Store.ListRelationsForIssue(ctx, positional[0], *relType)
		if err != nil {
			return err
		}
		return printValue(stdout, relations, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]model.Relation)
			for _, rel := range list {
				if _, err := fmt.Fprintf(w, "%s --%s--> %s\n", rel.SrcID, rel.Type, rel.DstID); err != nil {
					return err
				}
			}
			return nil
		})
	default:
		return errors.New("usage: lit dep <add|rm|ls> ...")
	}
}

func runLabel(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit label <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := flag.NewFlagSet("label add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		by := fs.String("by", os.Getenv("USER"), "Label author")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		labels, err := ap.Store.AddLabel(ctx, store.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: *by})
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := flag.NewFlagSet("label rm", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1])
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	default:
		return errors.New("usage: lit label <add|rm> ...")
	}
}

func runParent(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit parent <set|clear> ...")
	}
	switch args[0] {
	case "set":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := flag.NewFlagSet("parent set", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit parent set <child-id> <parent-id> [--by <user>] [--json]")
		}
		rel, err := ap.Store.SetParent(ctx, store.SetParentInput{
			ChildID:   positional[0],
			ParentID:  positional[1],
			CreatedBy: *by,
		})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			relation := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --parent-child--> %s\n", relation.SrcID, relation.DstID)
			return err
		})
	case "clear":
		positional, flagArgs := splitArgs(args[1:], 1)
		fs := flag.NewFlagSet("parent clear", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("usage: lit parent clear <child-id> [--json]")
		}
		if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	default:
		return errors.New("usage: lit parent <set|clear> ...")
	}
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := flag.NewFlagSet("children", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit children <parent-id> [--json]")
	}
	children, err := ap.Store.ListChildren(ctx, positional[0])
	if err != nil {
		return err
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		return writeJSON(stdout, children)
	}
	return printIssueLines(stdout, children, []string{"id", "state", "title"})
}

func runExport(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", true, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	export, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	return printValue(stdout, export, *jsonOut, func(w io.Writer, _ any) error {
		return writeJSON(w, export)
	})
}

func runBeads(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit beads <import|export> --db <path> [--json]")
	}
	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("beads import", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "", "Path to beads Dolt root/database")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if strings.TrimSpace(*dbPath) == "" {
			return errors.New("usage: lit beads import --db <path> [--json]")
		}
		summary, err := beads.Import(ctx, ap.Store, *dbPath)
		if err != nil {
			return err
		}
		return printValue(stdout, summary, *jsonOut, func(w io.Writer, v any) error {
			s := v.(beads.Summary)
			_, err := fmt.Fprintf(w, "imported issues=%d relations=%d comments=%d labels=%d\n", s.Issues, s.Relations, s.Comments, s.Labels)
			return err
		})
	case "export":
		fs := flag.NewFlagSet("beads export", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "", "Path to beads Dolt root/database")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if strings.TrimSpace(*dbPath) == "" {
			return errors.New("usage: lit beads export --db <path> [--json]")
		}
		summary, err := beads.Export(ctx, ap.Store, *dbPath)
		if err != nil {
			return err
		}
		return printValue(stdout, summary, *jsonOut, func(w io.Writer, v any) error {
			s := v.(beads.Summary)
			_, err := fmt.Fprintf(w, "exported issues=%d relations=%d comments=%d labels=%d\n", s.Issues, s.Relations, s.Comments, s.Labels)
			return err
		})
	default:
		return errors.New("usage: lit beads <import|export> --db <path> [--json]")
	}
}

func runWorkspace(stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("workspace", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	payload := map[string]string{
		"workspace_id":   ap.Workspace.WorkspaceID,
		"git_common_dir": ap.Workspace.GitCommonDir,
		"storage_dir":    ap.Workspace.StorageDir,
		"database_path":  ap.Workspace.DatabasePath,
		"dolt_repo_path": ap.Workspace.DoltRepoPath,
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		return writeJSON(stdout, payload)
	}
	for _, key := range []string{"workspace_id", "git_common_dir", "storage_dir", "database_path", "dolt_repo_path"} {
		if _, err := fmt.Fprintf(stdout, "%s: %s\n", key, payload[key]); err != nil {
			return err
		}
	}
	return nil
}

func runSync(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
	syncState, err := syncDoltRemotesFromGit(ctx, ws)
	if err != nil {
		return err
	}
	switch args[0] {
	case "remote":
		if len(args) < 2 {
			return errors.New("usage: lit sync remote ls [--json]")
		}
		switch args[1] {
		case "ls":
			fs := flag.NewFlagSet("sync remote ls", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			jsonOut := fs.Bool("json", false, "Output JSON")
			if err := parseFlagSet(fs, args[2:], stdout); err != nil {
				return err
			}
			payload := map[string]any{
				"git_remotes":  syncState.gitRemotes,
				"dolt_remotes": syncState.doltRemotes,
				"changes":      syncState.changes,
			}
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				p := v.(map[string]any)
				_, err := fmt.Fprintf(
					w,
					"git=%d dolt=%d added=%d updated=%d removed=%d\n",
					len(p["git_remotes"].([]workspace.GitRemote)),
					len(p["dolt_remotes"].([]map[string]string)),
					len(syncState.changes.Added),
					len(syncState.changes.Updated),
					len(syncState.changes.Removed),
				)
				return err
			})
		default:
			return errors.New("usage: lit sync remote ls [--json]")
		}
	case "fetch":
		fs := flag.NewFlagSet("sync fetch", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		remote := fs.String("remote", "origin", "Remote name")
		prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		commandArgs := []string{"fetch", strings.TrimSpace(*remote)}
		if *prune {
			commandArgs = append(commandArgs, "--prune")
		}
		output, err := doltcli.Run(ctx, ws.DoltRepoPath, commandArgs...)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": strings.TrimSpace(*remote),
			"raw":    output,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if strings.TrimSpace(p["raw"].(string)) != "" {
				_, err := fmt.Fprintln(w, strings.TrimSpace(p["raw"].(string)))
				return err
			}
			_, err := fmt.Fprintf(w, "fetched %s\n", p["remote"])
			return err
		})
	case "pull":
		fs := flag.NewFlagSet("sync pull", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		remote := fs.String("remote", "origin", "Remote name")
		branch := fs.String("branch", "", "Branch name (defaults to current)")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		remoteName := strings.TrimSpace(*remote)
		branchName := strings.TrimSpace(*branch)
		commandArgs := []string{"pull", remoteName}
		if branchName != "" {
			commandArgs = append(commandArgs, branchName)
		}
		output, err := doltcli.Run(ctx, ws.DoltRepoPath, commandArgs...)
		payload, handledErr := buildSyncPullPayload(remoteName, branchName, output, err)
		if handledErr != nil {
			return handledErr
		}
		return printValue(stdout, payload, *jsonOut, printSyncPullPayload)
	case "push":
		fs := flag.NewFlagSet("sync push", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		remote := fs.String("remote", "origin", "Remote name")
		branch := fs.String("branch", "", "Branch name (defaults to current)")
		setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
		force := fs.Bool("force", false, "Pass --force to dolt push")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		remoteName := strings.TrimSpace(*remote)
		requestedBranch := strings.TrimSpace(*branch)
		currentBranch, err := resolveSyncPushCurrentBranch(ctx, ws, requestedBranch)
		if err != nil {
			return err
		}
		commandArgs := buildSyncPushCommandArgs(
			remoteName,
			requestedBranch,
			currentBranch,
			*setUpstream,
			*force,
		)
		output, err := doltcli.Run(ctx, ws.DoltRepoPath, commandArgs...)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": remoteName,
			"branch": requestedBranch,
			"raw":    output,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if strings.TrimSpace(p["raw"].(string)) != "" {
				_, err := fmt.Fprintln(w, strings.TrimSpace(p["raw"].(string)))
				return err
			}
			if strings.TrimSpace(p["branch"].(string)) != "" {
				_, err := fmt.Fprintf(w, "pushed %s/%s\n", p["remote"], p["branch"])
				return err
			}
			_, err := fmt.Fprintf(w, "pushed %s\n", p["remote"])
			return err
		})
	case "status":
		fs := flag.NewFlagSet("sync status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		version, err := doltcli.InstalledVersion(ctx, ws.DoltRepoPath)
		if err != nil {
			return err
		}
		branch, err := doltcli.Run(ctx, ws.DoltRepoPath, "branch", "--show-current")
		if err != nil {
			return err
		}
		remotesOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "-v")
		if err != nil {
			return err
		}
		statusOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "status")
		if err != nil {
			return err
		}
		logOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "log", "-n", "1", "--oneline")
		if err != nil {
			return err
		}
		payload := map[string]any{
			"dolt_version": version.String(),
			"branch":       strings.TrimSpace(branch),
			"head":         strings.TrimSpace(logOutput),
			"status":       strings.TrimSpace(statusOutput),
			"git_remotes":  syncState.gitRemotes,
			"dolt_remotes": parseDoltRemoteVerbose(remotesOutput),
			"changes":      syncState.changes,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			_, err := fmt.Fprintf(
				w,
				"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d\n",
				p["dolt_version"],
				p["branch"],
				p["head"],
				len(p["git_remotes"].([]workspace.GitRemote)),
				len(p["dolt_remotes"].([]map[string]string)),
				len(syncState.changes.Added),
				len(syncState.changes.Updated),
				len(syncState.changes.Removed),
			)
			return err
		})
	default:
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
}

func syncPushBranchLookupArgs(requestedBranch string) []string {
	// [LAW:dataflow-not-control-flow] Sync push always runs this stage; branch data selects no-op vs lookup inputs.
	if strings.TrimSpace(requestedBranch) == "" {
		return []string{}
	}
	return []string{"branch", "--show-current"}
}

func resolveSyncPushCurrentBranch(ctx context.Context, ws workspace.Info, requestedBranch string) (string, error) {
	lookupArgs := syncPushBranchLookupArgs(requestedBranch)
	if len(lookupArgs) == 0 {
		return "", nil
	}
	currentBranch, err := doltcli.Run(ctx, ws.DoltRepoPath, lookupArgs...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(currentBranch), nil
}

func buildSyncPullPayload(remote string, requestedBranch string, output string, runErr error) (map[string]any, error) {
	if runErr == nil {
		return map[string]any{
			"status": "ok",
			"remote": remote,
			"branch": requestedBranch,
			"raw":    output,
		}, nil
	}
	message := strings.TrimSpace(runErr.Error())
	missingBranch, matchesMissingBranch := detectMissingRemoteBranch(message, requestedBranch)
	if !matchesMissingBranch {
		return nil, runErr
	}
	nextCommand := fmt.Sprintf("lit sync push --remote %s --branch %s --set-upstream", remote, missingBranch)
	retryCommand := fmt.Sprintf("lit sync pull --remote %s --branch %s", remote, missingBranch)
	// [LAW:dataflow-not-control-flow] Sync pull always returns structured payload; outcome variance lives in status/reason fields.
	return map[string]any{
		"status":        "skipped",
		"reason":        "remote_branch_missing",
		"remote":        remote,
		"branch":        missingBranch,
		"next_command":  nextCommand,
		"retry_command": retryCommand,
		"raw":           message,
	}, nil
}

func detectMissingRemoteBranch(message string, requestedBranch string) (string, bool) {
	// [LAW:single-enforcer] Remote-branch-missing classification is centralized here to avoid drift across callsites.
	normalized := strings.ToLower(strings.TrimSpace(message))
	if !strings.HasPrefix(normalized, "dolt pull ") {
		return "", false
	}
	if !strings.Contains(normalized, "not found on remote") {
		return "", false
	}
	matches := missingRemoteBranchPattern.FindStringSubmatch(message)
	branch := strings.TrimSpace(requestedBranch)
	if len(matches) == 2 && strings.TrimSpace(matches[1]) != "" {
		branch = strings.TrimSpace(matches[1])
	}
	if branch == "" {
		return "", false
	}
	return branch, true
}

func printSyncPullPayload(w io.Writer, v any) error {
	payload := v.(map[string]any)
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	switch status {
	case "skipped":
		nextCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["next_command"]))
		retryCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["retry_command"]))
		_, err := fmt.Fprintf(
			w,
			"skipped pull %s/%s: remote branch missing; run `%s`, then retry `%s`\n",
			remote,
			branch,
			nextCommand,
			retryCommand,
		)
		return err
	default:
		raw, hasRaw := payload["raw"].(string)
		if hasRaw && strings.TrimSpace(raw) != "" {
			_, err := fmt.Fprintln(w, raw)
			return err
		}
		if branch != "" {
			_, err := fmt.Fprintf(w, "pulled %s/%s\n", remote, branch)
			return err
		}
		_, err := fmt.Fprintf(w, "pulled %s\n", remote)
		return err
	}
}

func buildSyncPushCommandArgs(remote string, requestedBranch string, currentBranch string, setUpstream bool, force bool) []string {
	// [LAW:one-source-of-truth] Push source always comes from the current Dolt branch; requested branch is a remote projection target.
	commandArgs := []string{"push"}
	if setUpstream {
		commandArgs = append(commandArgs, "-u")
	}
	if force {
		commandArgs = append(commandArgs, "--force")
	}
	commandArgs = append(commandArgs, remote)
	if requestedBranch == "" {
		return commandArgs
	}
	sourceRef := strings.TrimSpace(currentBranch)
	if sourceRef == "" {
		// [LAW:dataflow-not-control-flow] Empty branch-name output maps to a deterministic source ref (`HEAD`) instead of changing push stage behavior.
		sourceRef = "HEAD"
	}
	refspec := fmt.Sprintf("%s:%s", sourceRef, requestedBranch)
	return append(commandArgs, refspec)
}

type remoteSyncChanges struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []map[string]string
	changes     remoteSyncChanges
}

func syncDoltRemotesFromGit(ctx context.Context, ws workspace.Info) (remoteSyncState, error) {
	gitRemotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return remoteSyncState{}, fmt.Errorf("read git remotes: %w", err)
	}
	doltOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "-v")
	if err != nil {
		return remoteSyncState{}, err
	}
	doltRemotes := parseDoltRemoteVerbose(doltOutput)
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)

	changes := remoteSyncChanges{
		Added:   []string{},
		Updated: []string{},
		Removed: []string{},
	}

	for _, remote := range gitRemotes {
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "add", remote.Name, remote.URL); err != nil {
				return remoteSyncState{}, err
			}
			changes.Added = append(changes.Added, remote.Name)
			continue
		}
		if !sameRemoteURL(currentURL, remote.URL) {
			if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "remove", remote.Name); err != nil {
				return remoteSyncState{}, err
			}
			if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "add", remote.Name, remote.URL); err != nil {
				return remoteSyncState{}, err
			}
			changes.Updated = append(changes.Updated, remote.Name)
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; keep {
			continue
		}
		if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "remove", name); err != nil {
			return remoteSyncState{}, err
		}
		changes.Removed = append(changes.Removed, name)
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Updated)
	sort.Strings(changes.Removed)

	finalOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "-v")
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: parseDoltRemoteVerbose(finalOutput),
		changes:     changes,
	}, nil
}

func mapGitRemotesByName(remotes []workspace.GitRemote) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		out[remote.Name] = remote.URL
	}
	return out
}

func mapRemotesByName(remotes []map[string]string) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		name := strings.TrimSpace(remote["name"])
		url := strings.TrimSpace(remote["url"])
		scope := strings.TrimSpace(remote["scope"])
		if name == "" || url == "" {
			continue
		}
		// [LAW:one-source-of-truth] Remote URL projection always prefers fetch scope as the canonical source.
		if scope == "fetch" {
			out[name] = url
			continue
		}
		if _, exists := out[name]; !exists {
			out[name] = url
		}
	}
	return out
}

func sameRemoteURL(left, right string) bool {
	return normalizeRemoteURL(left) == normalizeRemoteURL(right)
}

func normalizeRemoteURL(input string) string {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.TrimPrefix(trimmed, "git+")
	return trimmed
}

func parseDoltRemoteVerbose(output string) []map[string]string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	remotes := make([]map[string]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			remotes = append(remotes, map[string]string{"raw": trimmed})
			continue
		}
		entry := map[string]string{
			"name": fields[0],
			"url":  fields[1],
		}
		if len(fields) >= 3 {
			entry["scope"] = strings.Trim(fields[2], "()")
		}
		remotes = append(remotes, entry)
	}
	return remotes
}

func runDoctor(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	report, err := ap.Store.Doctor(ctx)
	if err != nil {
		return err
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d\n", report.IntegrityCheck, report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows); err != nil {
			return err
		}
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}

func runFsck(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repair := fs.Bool("repair", false, "Attempt safe repairs")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	report, err := ap.Store.Fsck(ctx, *repair)
	if err != nil {
		return err
	}
	if shouldWriteJSON(stdout, *jsonOut) {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d repair=%t\n", report.IntegrityCheck, report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows, *repair); err != nil {
			return err
		}
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}

func runBackup(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		keep := fs.Int("keep", 20, "Snapshots to keep after rotation")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		export, err := ap.Store.Export(ctx)
		if err != nil {
			return err
		}
		snapshot, err := backup.Create(ap.Workspace.StorageDir, export)
		if err != nil {
			return err
		}
		if err := backup.Prune(ap.Workspace.StorageDir, *keep); err != nil {
			return err
		}
		return printValue(stdout, snapshot, *jsonOut, func(w io.Writer, v any) error {
			s := v.(backup.Snapshot)
			_, err := fmt.Fprintf(w, "%s %s\n", s.Name, s.Path)
			return err
		})
	case "list":
		fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		snapshots, err := backup.List(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		return printValue(stdout, snapshots, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]backup.Snapshot)
			for _, snapshot := range list {
				if _, err := fmt.Fprintf(w, "%s %d %s\n", snapshot.Name, snapshot.Size, snapshot.Path); err != nil {
					return err
				}
			}
			return nil
		})
	case "restore":
		fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "", "Backup snapshot path")
		latest := fs.Bool("latest", false, "Restore latest backup snapshot")
		force := fs.Bool("force", false, "Force restore over unsynced state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		restorePath := strings.TrimSpace(*path)
		if *latest {
			latestSnapshot, err := backup.Latest(ap.Workspace.StorageDir)
			if err != nil {
				return err
			}
			if latestSnapshot == nil {
				return errors.New("no backups available")
			}
			restorePath = latestSnapshot.Path
		}
		if restorePath == "" {
			return errors.New("usage: lit backup restore --path <snapshot.json> [--force] [--json] or --latest")
		}
		if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "restored", "path": restorePath}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
}

func runRecover(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromSync := fs.String("from-sync", "", "Restore from sync file")
	fromBackup := fs.String("from-backup", "", "Restore from backup snapshot")
	latestBackup := fs.Bool("latest-backup", false, "Restore from latest backup snapshot")
	force := fs.Bool("force", false, "Force restore over unsynced state")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	var restorePath string
	switch {
	case strings.TrimSpace(*fromSync) != "":
		restorePath = strings.TrimSpace(*fromSync)
	case strings.TrimSpace(*fromBackup) != "":
		restorePath = strings.TrimSpace(*fromBackup)
	case *latestBackup:
		latest, err := backup.Latest(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		if latest == nil {
			return errors.New("no backups available")
		}
		restorePath = latest.Path
	default:
		return errors.New("usage: lit recover --from-sync <path> | --from-backup <path> | --latest-backup [--force] [--json]")
	}
	if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
		return err
	}
	payload := map[string]string{"status": "recovered", "path": restorePath}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
		return err
	})
}

func runBulk(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
	switch args[0] {
	case "label":
		if len(args) < 2 {
			return errors.New("usage: lit bulk label <add|rm> ...")
		}
		action := args[1]
		fs := flag.NewFlagSet("bulk label", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		label := fs.String("label", "", "Label name")
		by := fs.String("by", os.Getenv("USER"), "Label actor")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[2:], stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*label) == "" {
			return errors.New("--label is required")
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			switch action {
			case "add":
				_, err := ap.Store.AddLabel(ctx, store.AddLabelInput{
					IssueID:   issueID,
					Name:      *label,
					CreatedBy: *by,
				})
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			case "rm":
				_, err := ap.Store.RemoveLabel(ctx, issueID, *label)
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			default:
				return errors.New("usage: lit bulk label <add|rm> ...")
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "close", "archive":
		fs := flag.NewFlagSet("bulk transition", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		reason := fs.String("reason", "", "Lifecycle reason")
		by := fs.String("by", os.Getenv("USER"), "Lifecycle actor")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("--reason is required")
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			_, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
				IssueID:   issueID,
				Action:    args[0],
				Reason:    *reason,
				CreatedBy: *by,
			})
			if err != nil {
				results[issueID] = err.Error()
				continue
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "import":
		fs := flag.NewFlagSet("bulk import", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "", "Path to JSON export")
		force := fs.Bool("force", false, "Force import over unsynced local state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if strings.TrimSpace(*path) == "" {
			return errors.New("--path is required")
		}
		if err := restoreFromExportPath(ctx, ap, *path, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "imported", "path": filepath.Clean(*path)}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
}

func runCompletion(stdout io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lit completion <bash|zsh|fish>")
	}
	switch args[0] {
	case "bash":
		_, err := io.WriteString(stdout, bashCompletionScript)
		return err
	case "zsh":
		_, err := io.WriteString(stdout, zshCompletionScript)
		return err
	case "fish":
		_, err := io.WriteString(stdout, fishCompletionScript)
		return err
	default:
		return errors.New("usage: lit completion <bash|zsh|fish>")
	}
}

func runQuickstart(stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit quickstart [--json]")
	}

	payload := map[string]any{
		"summary": "Agent quickstart for links issue tracking",
		"workflow": []string{
			"Initialize and auto-migrate with `lit init --json`.",
			"Discover workspace identity with `lit workspace --json`.",
			"Migrate legacy Beads wiring explicitly with `lit migrate beads --apply --json` when needed.",
			"Install git hook automation once with `lit hooks install`.",
			"List ready work with `lit ready --json` (or `lit ls --query \"status:open\" --json`).",
			"Create issues with `lit new ...`; use `--type epic` for epics.",
			"Connect issues using `lit parent set` and `lit dep add --type related-to|blocks`.",
			"Configure remotes with `git remote`; `lit sync` mirrors those remotes into Dolt automatically.",
			"Run health checks with `lit doctor` and repair known corruption with `lit fsck --repair`.",
			"Snapshot and rollback using `lit backup create`, `lit backup restore`, or `lit recover`.",
		},
		"examples": []string{
			"lit init --json",
			"lit migrate beads --apply --json",
			"lit hooks install --json",
			"lit workspace --json",
			"lit ready --json",
			"lit start <issue-id> --reason \"claim\" --json",
			"lit done <issue-id> --reason \"completed\" --json",
			"lit ls --query \"status:open type:task\" --sort priority:asc,updated_at:desc --json",
			"lit new --title \"Fix renderer race\" --type bug --priority 1 --labels renderer,urgent --json",
			"lit parent set <issue-id> <parent-issue-id> --json",
			"lit dep add <issue-id> <dependency-issue-id> --type related-to --json",
			"git remote add origin https://github.com/org/repo.git",
			"lit sync remote ls --json",
			"lit sync pull --remote origin --branch main --json",
			"lit sync push --remote origin --branch main --json",
		},
		"exit_codes": map[string]int{
			"ok":         ExitOK,
			"usage":      ExitUsage,
			"validation": ExitValidation,
			"not_found":  ExitNotFound,
			"conflict":   ExitConflict,
			"corruption": ExitCorruption,
		},
	}

	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		instructions := v.(map[string]any)
		lines := []string{
			"links agent quickstart",
			"",
			"1) Discover context",
			"   `lit init --json`",
			"   `lit migrate beads --apply --json`  # for legacy Beads repos",
			"   `lit hooks install --json`",
			"   `lit workspace --json`",
			"",
			"2) Find work",
			"   `lit ready --json`",
			"   `lit start <issue-id> --reason \"claim\" --json`",
			"   `lit ls --format lines --json`",
			"   `lit ls --query \"status:open type:task\" --sort priority:asc,updated_at:desc --json`",
			"",
			"3) Create and relate issues/epics",
			"   `lit new --title \"...\" --type task|bug|feature|chore|epic --json`",
			"   `lit parent set <child-id> <parent-id> --json`",
			"   `lit dep add <src-id> <dst-id> --type blocks|related-to|parent-child --json`",
			"",
			"4) Mutations",
			"   Use command outputs directly for follow-up writes.",
			"",
			"5) Dolt remote sync",
			"   Configure remotes with git, then run sync commands.",
			"   `git remote -v`",
			"   `lit sync remote ls --json`",
			"   `lit sync fetch --remote origin --json`",
			"   `lit sync pull --remote origin --branch main --json`",
			"   `lit sync push --remote origin --branch main --json`",
			"   If sync warning appears, agent should auto-retry silently before escalating.",
			"",
			"6) Integrity and recovery",
			"   `lit doctor --json`",
			"   `lit fsck --repair --json`",
			"   `lit backup create --json`",
			"   `lit backup restore --latest --json`",
			"   `lit recover --latest-backup --json`",
			"",
			fmt.Sprintf("Exit codes: ok=%d usage=%d validation=%d not_found=%d conflict=%d corruption=%d", ExitOK, ExitUsage, ExitValidation, ExitNotFound, ExitConflict, ExitCorruption),
		}
		if summary, ok := instructions["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			lines[0] = summary
		}
		_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
		return err
	})
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func shouldWriteJSON(w io.Writer, jsonOut bool) bool {
	if jsonOut {
		return true
	}
	// [LAW:one-source-of-truth] Writer-bound output mode is the canonical default signal for format selection.
	return outputModeFromWriter(w) == outputModeJSON
}

func outputModeFromWriter(w io.Writer) outputMode {
	provider, ok := w.(outputModeProvider)
	if !ok {
		return outputModeText
	}
	return provider.linksOutputMode()
}

func printValue(w io.Writer, v any, jsonOut bool, textFn func(io.Writer, any) error) error {
	if shouldWriteJSON(w, jsonOut) {
		return writeJSON(w, v)
	}
	return textFn(w, v)
}

func printIssueSummary(w io.Writer, v any) error {
	issue := v.(model.Issue)
	_, err := fmt.Fprintf(w, "%s [%s/%s/P%d] %s%s\n", issue.ID, formatIssueState(issue), issue.IssueType, issue.Priority, issue.Title, formatLabels(issue.Labels))
	return err
}

func printIssueTable(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(resolved, "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, resolved, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, resolved, " | ")); err != nil {
			return err
		}
	}
	return nil
}

func printIssueDetail(w io.Writer, detail model.IssueDetail) error {
	issue := detail.Issue
	if _, err := fmt.Fprintf(w, "%s\n%s\n\nstatus: %s\ntype: %s\npriority: %d\nassignee: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, issue.Title, issue.Status, issue.IssueType, issue.Priority, emptyDash(issue.Assignee), emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(issue.ArchivedAt), formatOptionalTime(issue.DeletedAt)); err != nil {
		return err
	}
	if issue.Description != "" {
		if _, err := fmt.Fprintf(w, "\ndescription:\n%s\n", issue.Description); err != nil {
			return err
		}
	}
	if detail.Parent != nil {
		if _, err := fmt.Fprintf(w, "\nparent:\n- %s %s\n", detail.Parent.ID, detail.Parent.Title); err != nil {
			return err
		}
	}
	if err := printIssueGroup(w, "children", detail.Children); err != nil {
		return err
	}
	if err := printIssueGroup(w, "depends_on", detail.DependsOn); err != nil {
		return err
	}
	if err := printIssueGroup(w, "blocked_by", detail.BlockedBy); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	if len(detail.Comments) > 0 {
		if _, err := fmt.Fprintln(w, "\ncomments:"); err != nil {
			return err
		}
		for _, c := range detail.Comments {
			if _, err := fmt.Fprintf(w, "- [%s] %s\n", c.CreatedBy, strings.ReplaceAll(c.Body, "\n", "\\n")); err != nil {
				return err
			}
		}
	}
	if len(detail.History) > 0 {
		if _, err := fmt.Fprintln(w, "\nhistory:"); err != nil {
			return err
		}
		for _, event := range detail.History {
			if _, err := fmt.Fprintf(w, "- [%s] %s %s (%s -> %s)\n", event.CreatedBy, event.Action, strings.ReplaceAll(event.Reason, "\n", "\\n"), emptyDash(event.FromStatus), emptyDash(event.ToStatus)); err != nil {
				return err
			}
		}
	}
	return nil
}

func printIssueGroup(w io.Writer, label string, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", label); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintf(w, "- %s %s\n", issue.ID, issue.Title); err != nil {
			return err
		}
	}
	return nil
}

func formatIssueColumns(issue model.Issue, columns []string, delimiter string) string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		switch column {
		case "id":
			values = append(values, issue.ID)
		case "state":
			values = append(values, formatIssueState(issue))
		case "type":
			values = append(values, issue.IssueType)
		case "priority":
			values = append(values, strconv.Itoa(issue.Priority))
		case "title":
			values = append(values, issue.Title)
		case "assignee":
			values = append(values, emptyDash(issue.Assignee))
		case "labels":
			values = append(values, emptyDash(strings.Join(issue.Labels, ",")))
		case "updated_at":
			values = append(values, issue.UpdatedAt.Format(time.RFC3339))
		case "created_at":
			values = append(values, issue.CreatedAt.Format(time.RFC3339))
		}
	}
	return strings.Join(values, delimiter)
}

func resolveColumns(columns []string) []string {
	if len(columns) == 0 {
		// [LAW:dataflow-not-control-flow] Default listing still flows through the same projection path.
		return []string{"id", "state", "title"}
	}
	valid := map[string]struct{}{
		"id": {}, "state": {}, "type": {}, "priority": {}, "title": {}, "assignee": {}, "labels": {}, "updated_at": {}, "created_at": {},
	}
	out := make([]string, 0, len(columns))
	for _, column := range columns {
		normalized := strings.ToLower(strings.TrimSpace(column))
		if normalized == "" {
			continue
		}
		if _, ok := valid[normalized]; ok {
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return []string{"id", "state", "title"}
	}
	return out
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func printLabels(w io.Writer, v any) error {
	labels := v.([]string)
	_, err := fmt.Fprintln(w, strings.Join(labels, ","))
	return err
}

func formatLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return " [" + strings.Join(labels, ",") + "]"
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func transitionCommandName(action string) string {
	switch action {
	case "reopen":
		return "open"
	default:
		return action
	}
}

func formatIssueState(issue model.Issue) string {
	parts := []string{issue.Status}
	if issue.ArchivedAt != nil {
		parts = append(parts, "archived")
	}
	if issue.DeletedAt != nil {
		parts = append(parts, "deleted")
	}
	return strings.Join(parts, "+")
}

func splitCSV(input string) []string {
	if strings.TrimSpace(input) == "" {
		return nil
	}
	parts := strings.Split(input, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseColumns(input string) []string {
	return splitCSV(strings.ToLower(input))
}

func parseSortSpecs(input string) ([]store.SortSpec, error) {
	parts := splitCSV(input)
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]store.SortSpec, 0, len(parts))
	for _, part := range parts {
		spec := strings.TrimSpace(part)
		field := spec
		desc := false
		if strings.Contains(spec, ":") {
			chunks := strings.SplitN(spec, ":", 2)
			field = strings.TrimSpace(chunks[0])
			direction := strings.ToLower(strings.TrimSpace(chunks[1]))
			switch direction {
			case "asc":
				desc = false
			case "desc":
				desc = true
			default:
				return nil, fmt.Errorf("unsupported sort direction %q", direction)
			}
		}
		out = append(out, store.SortSpec{Field: field, Desc: desc})
	}
	return out, nil
}

func syncBasePath(ap *app.App) string {
	return filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")
}

func restoreFromExportPath(ctx context.Context, ap *app.App, path string, force bool) error {
	restorePath := filepath.Clean(path)
	targetExport, _, err := syncfile.Read(restorePath)
	if err != nil {
		return err
	}
	localExport, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	state, err := ap.Store.GetSyncState(ctx)
	if err != nil {
		return err
	}
	if state.ContentHash != "" && !force {
		baseHash, hashErr := syncfile.HashFile(syncBasePath(ap))
		if hashErr != nil {
			return hashErr
		}
		if baseHash != "" {
			localHash, localHashErr := hashExport(localExport)
			if localHashErr != nil {
				return localHashErr
			}
			if localHash != baseHash {
				return MergeConflictError{Message: "restore conflict: local workspace has unsynced changes since last sync base"}
			}
		}
	}
	if _, err := backup.Create(ap.Workspace.StorageDir, localExport); err != nil {
		return err
	}
	if err := backup.Prune(ap.Workspace.StorageDir, 20); err != nil {
		return err
	}
	if err := ap.Store.ReplaceFromExport(ctx, targetExport); err != nil {
		return err
	}
	if _, err := syncfile.WriteAtomic(syncBasePath(ap), targetExport); err != nil {
		return err
	}
	hash, err := syncfile.HashFile(restorePath)
	if err != nil {
		return err
	}
	return ap.Store.RecordSyncState(ctx, store.SyncState{
		Path:        restorePath,
		ContentHash: hash,
	})
}

func hashExport(export model.Export) (string, error) {
	payload, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal export: %w", err)
	}
	payload = append(payload, '\n')
	sum := sha256.Sum256(payload)
	return strings.ToLower(hex.EncodeToString(sum[:])), nil
}

type MergeConflictError struct {
	Message   string
	Conflicts []merge.IssueConflict
}

func (e MergeConflictError) Error() string {
	return e.Message
}

type CorruptionError struct {
	Message string
}

func (e CorruptionError) Error() string { return e.Message }

func printUsage(w io.Writer) {
	fmt.Fprint(w, `links / lit

Worktree-native issue tracker with Dolt-backed sync.

Output:
  --output auto|json|text     Output mode for commands that support structured output.
  --json                      Shorthand for --output json.
  Precedence: --output > --json > LIT_OUTPUT > auto
  Auto behavior: TTY -> text, non-TTY -> json

Usage:
  lit [--output auto|text|json] [--json] [command]
  lit [--output auto|text|json] [--json] [command] [flags]

Global Output Mode:
  default        auto (TTY -> text, non-TTY -> json)
  --json         Explicit shorthand for JSON output compatibility
  --output MODE  Force output mode (auto|text|json)
  LIT_OUTPUT     Environment default when flags are not provided

Issue Workflow:
  init           Initialize links in the current repository (auto-migrates Beads residue)
  ready          List open work ordered by priority and recency
  new            Create an issue
  ls             List issues with filters/query/sort
  show           Show issue details
  start          Claim work (open -> in_progress)
  done           Mark work complete (in_progress -> closed)
  close          Close issue(s)
  open           Reopen issue(s)
  archive        Archive issue(s)
  delete         Soft-delete issue(s)
  unarchive      Unarchive issue(s)
  restore        Restore deleted issue(s)
  comment        Add issue comments
  label          Add/remove issue labels
  bulk           Bulk issue operations (label, close, archive, import)

Dependencies & Structure:
  parent         Manage parent/child links
  children       List child issues
  dep            Manage dependency edges

Sync & Data:
  export         Export workspace snapshot JSON
  sync           Mirror Dolt data through git remotes
  backup         Create/list/restore backup snapshots
  recover        Recover from sync file or backup
  beads          Import/export from Beads Dolt databases

Setup & Maintenance:
  workspace      Show workspace metadata
  hooks          Install git hook automation
  migrate        Migrate from Beads to links
  doctor         Health check
  fsck           Integrity check and optional repair

Guidance & Tooling:
  quickstart     Agent quickstart workflow
  completion     Generate shell completion script
  help           Show this help output

Command Syntax:
  lit init [--json] [--skip-hooks] [--skip-agents]
  lit ready [--assignee <user>] [--limit N] [--format lines|table] [--columns ...] [--json]
  lit start <id> --reason <text> [--by <user>] [--json]
  lit done <id> --reason <text> [--by <user>] [--json]
  lit hooks install [--json]
  lit migrate beads [--apply] [--json]
  lit quickstart [--json]
  lit completion <bash|zsh|fish>
  lit workspace [--json]
  lit sync remote ls [--json]
  lit sync pull --remote <name> --branch <name> [--json]
  lit sync push --remote <name> --branch <name> [--set-upstream] [--force] [--json]

Examples:
  lit init --json
  lit ready --json
  lit start <issue-id> --reason "claim" --json
  lit done <issue-id> --reason "completed" --json
  lit new --title "Fix renderer race" --type bug --priority 1 --json
  lit ls --query "status:open type:task" --sort priority:asc,updated_at:desc --json

Use "lit [command] --help" for more information about a command.
`)
}

func splitArgs(args []string, positionalCount int) ([]string, []string) {
	positionals := make([]string, 0, positionalCount)
	flags := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				flags = append(flags, args[index+1])
				index++
			}
			continue
		}
		if len(positionals) < positionalCount {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
	}
	return positionals, flags
}
