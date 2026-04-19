package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/templates"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

const (
	// [LAW:one-source-of-truth] Only the section between these markers is owned by links.
	linksHookBeginMarker = "# --- BEGIN LINKS INTEGRATION ---"
	linksHookEndMarker   = "# --- END LINKS INTEGRATION ---"
)

type hookInstallResult struct {
	HookPath string
	Changed  bool
	Managed  bool
	Reason   string
}

func runHooks(stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit hooks install [--json]")
	}
	switch args[0] {
	case "install":
		return runHooksInstall(stdout, ws, args[1:])
	default:
		return errors.New("usage: lit hooks install [--json]")
	}
}

func runHooksInstall(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("hooks install")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit hooks install [--json]")
	}

	result, err := installHooks(ws)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"status":     "installed",
		"hook":       result.HookPath,
		"changed":    result.Changed,
		"managed":    result.Managed,
		"reason":     result.Reason,
		"traces_dir": automationTraceDir(ws),
	}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]any)
		_, printErr := fmt.Fprintf(w, "installed %s\n", p["hook"])
		return printErr
	})
}

func installHooks(ws workspace.Info) (hookInstallResult, error) {
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	hookPath := filepath.Join(hooksDir, "pre-push")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return hookInstallResult{}, fmt.Errorf("create hooks dir: %w", err)
	}

	section, err := renderLinksPrePushHookSection(ws.RootDir)
	if err != nil {
		return hookInstallResult{}, fmt.Errorf("load pre-push hook template: %w", err)
	}
	existing, err := os.ReadFile(hookPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return hookInstallResult{}, fmt.Errorf("read existing pre-push hook: %w", err)
	}

	mode := os.FileMode(0o755)
	if errors.Is(err, os.ErrNotExist) {
		updated := "#!/usr/bin/env bash\n" + section
		if writeErr := os.WriteFile(hookPath, []byte(updated), mode); writeErr != nil {
			return hookInstallResult{}, fmt.Errorf("write pre-push hook: %w", writeErr)
		}
		return hookInstallResult{HookPath: hookPath, Changed: true, Managed: true}, nil
	}

	if info, statErr := os.Stat(hookPath); statErr == nil {
		mode = info.Mode().Perm()
		if mode&0o111 == 0 {
			mode = 0o755
		}
	}

	existingStr := string(existing)

	// Treat a hook as bash-compatible only if its shebang explicitly references bash.
	isBashCompatible := func(script string) bool {
		firstLineEnd := strings.IndexByte(script, '\n')
		var firstLine string
		if firstLineEnd == -1 {
			firstLine = strings.TrimSpace(script)
		} else {
			firstLine = strings.TrimSpace(script[:firstLineEnd])
		}
		if !strings.HasPrefix(firstLine, "#!") {
			return false
		}
		return strings.Contains(firstLine, "bash")
	}

	if !isBashCompatible(existingStr) {
		return hookInstallResult{
			HookPath: hookPath,
			Changed:  false,
			Managed:  false,
			Reason:   "incompatible",
		}, nil
	}
	updated, changed := upsertManagedSection(existingStr, section, linksHookBeginMarker, linksHookEndMarker)
	if !changed {
		return hookInstallResult{HookPath: hookPath, Changed: false, Managed: true}, nil
	}

	if err := os.WriteFile(hookPath, []byte(updated), mode); err != nil {
		return hookInstallResult{}, fmt.Errorf("write pre-push hook: %w", err)
	}
	return hookInstallResult{HookPath: hookPath, Changed: true, Managed: true}, nil
}

func renderLinksPrePushHookSection(workspaceRoot string) (string, error) {
	return templates.Load(templates.PrePushHookTemplateName, workspaceRoot)
}
