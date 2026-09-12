package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

var (
	// [LAW:one-source-of-truth] Only the section between these markers is owned by lit.
	litHookMarkers    = markerPair{begin: "# --- BEGIN LIT INTEGRATION ---", end: "# --- END LIT INTEGRATION ---"}
	legacyHookMarkers = markerPair{begin: "# --- BEGIN LINKS INTEGRATION ---", end: "# --- END LINKS INTEGRATION ---"}
)

type hookInstallResult struct {
	HookPath string
	Changed  bool
	Managed  bool
	Reason   string
}

var hooksFamily = commandFamily[wsSubcommand]{
	usage: "usage: lit hooks install",
	subcommands: []subcommandRow[wsSubcommand]{
		{name: "install", payload: wsSubcommand{declare: hooksInstallLeaf}},
	},
}

func hooksInstallLeaf() wsLeaf {
	fs := newCobraFlagSet("hooks install")
	return wsLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
		if fs.NArg() != 0 {
			return UsageError{Message: "usage: lit hooks install"}
		}

		result, err := installHooks(ws)
		if err != nil {
			return err
		}

		_, err = fmt.Fprintf(stdout, "installed %s\n", result.HookPath)
		return err
	}}
}

func installHooks(ws workspace.Info) (hookInstallResult, error) {
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	hookPath := filepath.Join(hooksDir, "pre-push")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return hookInstallResult{}, fmt.Errorf("create hooks dir: %w", err)
	}

	section, err := renderLinksPrePushHookSection(ws.RootDir)
	if err != nil {
		return hookInstallResult{}, err
	}
	existing, err := os.ReadFile(hookPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return hookInstallResult{}, fmt.Errorf("read existing pre-push hook: %w", err)
	}

	mode := os.FileMode(0o755)
	if errors.Is(err, os.ErrNotExist) {
		updated := "#!/usr/bin/env bash\n" + section.text
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
	existingStr = migrateMarkers(existingStr, legacyHookMarkers, litHookMarkers)
	updated, changed := upsertManagedSection(existingStr, section)
	if !changed {
		return hookInstallResult{HookPath: hookPath, Changed: false, Managed: true}, nil
	}

	if err := os.WriteFile(hookPath, []byte(updated), mode); err != nil {
		return hookInstallResult{}, fmt.Errorf("write pre-push hook: %w", err)
	}
	return hookInstallResult{HookPath: hookPath, Changed: true, Managed: true}, nil
}

// renderLinksPrePushHookSection is the crossing where resolved template content —
// an override the user wrote, or the embedded default — becomes a managed section.
// [LAW:parse-dont-validate] Nothing downstream re-checks the shape, because
// downstream only ever holds a managedSection.
func renderLinksPrePushHookSection(workspaceRoot string) (managedSection, error) {
	content, source, err := templates.LoadWithSource(templates.PrePushHookTemplateName, workspaceRoot)
	if err != nil {
		return managedSection{}, fmt.Errorf("load pre-push hook template: %w", err)
	}
	section, err := litHookMarkers.parse(content)
	if err != nil {
		return managedSection{}, fmt.Errorf("pre-push hook template (via %s): %w", source, err)
	}
	return section, nil
}
