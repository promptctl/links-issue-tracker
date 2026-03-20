package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/legacydolt"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

var beadsSignalRegex = regexp.MustCompile(`(?i)\bbeads\b|(^|[^a-zA-Z0-9_])bd([^a-zA-Z0-9_]|$)|\.beads`)

type BeadsMigrationRequiredError struct {
	Summary            string
	Trigger            string
	BlockedCommand     string
	RemediationCommand string
	TraceRef           string
	TraceWriteError    string
}

func (e BeadsMigrationRequiredError) Error() string {
	remediationCommand := strings.TrimSpace(e.RemediationCommand)
	if remediationCommand == "" {
		remediationCommand = "lit migrate --apply --json"
	}
	parts := []string{}
	if strings.TrimSpace(e.Summary) == "" {
		parts = append(parts, fmt.Sprintf("beads residue detected; run '%s' before running other commands", remediationCommand))
	} else {
		parts = append(parts, fmt.Sprintf("beads residue detected (%s); run '%s' before running other commands", e.Summary, remediationCommand))
	}
	if strings.TrimSpace(e.BlockedCommand) != "" {
		parts = append(parts, fmt.Sprintf("blocked_command=%s", e.BlockedCommand))
	}
	if strings.TrimSpace(e.TraceRef) != "" {
		parts = append(parts, fmt.Sprintf("trace=%s", e.TraceRef))
	}
	if strings.TrimSpace(e.TraceWriteError) != "" {
		parts = append(parts, fmt.Sprintf("trace_error=%s", e.TraceWriteError))
	}
	return strings.Join(parts, "; ")
}

func (e BeadsMigrationRequiredError) ErrorDetails() map[string]any {
	remediationCommand := strings.TrimSpace(e.RemediationCommand)
	if remediationCommand == "" {
		remediationCommand = "lit migrate --apply --json"
	}
	details := map[string]any{
		"reason":              "beads_residue_detected",
		"summary":             strings.TrimSpace(e.Summary),
		"blocked_command":     strings.TrimSpace(e.BlockedCommand),
		"remediation_command": remediationCommand,
		"trigger":             strings.TrimSpace(e.Trigger),
	}
	if strings.TrimSpace(e.TraceRef) != "" {
		details["trace_ref"] = strings.TrimSpace(e.TraceRef)
	}
	if strings.TrimSpace(e.TraceWriteError) != "" {
		details["trace_error"] = strings.TrimSpace(e.TraceWriteError)
	}
	return details
}

type migrateReport struct {
	Mode                string   `json:"mode"`
	Applied             bool     `json:"applied"`
	ResidueDetected     bool     `json:"residue_detected"`
	DataImported        bool     `json:"data_imported"`
	ImportSource        string   `json:"import_source,omitempty"`
	ImportIssues        int      `json:"import_issues"`
	ImportRelations     int      `json:"import_relations"`
	ImportComments      int      `json:"import_comments"`
	ImportLabels        int      `json:"import_labels"`
	HooksDir            string   `json:"hooks_dir"`
	HookFilesScanned    int      `json:"hook_files_scanned"`
	BeadsHookFiles      []string `json:"beads_hook_files"`
	HookFilesModified   []string `json:"hook_files_modified"`
	HookFilesRemoved    []string `json:"hook_files_removed"`
	AgentsPath          string   `json:"agents_path"`
	AgentsFound         bool     `json:"agents_found"`
	AgentsBefore        int      `json:"agents_mentions_before"`
	AgentsAfter         int      `json:"agents_mentions_after"`
	AgentsUpdated       bool     `json:"agents_updated"`
	BeadsArtifacts      []string `json:"beads_artifacts,omitempty"`
	ConfigFilesScanned  int      `json:"config_files_scanned"`
	ConfigFilesDetected []string `json:"config_files_detected,omitempty"`
	ConfigFilesModified []string `json:"config_files_modified,omitempty"`
	ConfigFilesRemoved  []string `json:"config_files_removed,omitempty"`
	BackupPath          string   `json:"backup_path,omitempty"`
	LitHookInstalled    bool     `json:"lit_hook_installed"`
	LitHookPath         string   `json:"lit_hook_path,omitempty"`
	LitLegacyChainPath  string   `json:"lit_legacy_chain_path,omitempty"`
	Notes               []string `json:"notes,omitempty"`
}

type migrateApplyOptions struct {
	InstallHooks  bool
	InstallAgents bool
}

type hookCleanupPlan struct {
	Path       string
	NewContent []byte
	RemoveFile bool
	Changed    bool
}

type agentsCleanupPlan struct {
	Path       string
	Found      bool
	Before     int
	After      int
	NewContent []byte
	Changed    bool
}

type configCleanupPlan struct {
	Path       string
	NewContent []byte
	RemoveFile bool
	Changed    bool
}

type beadsResidueScan struct {
	HooksDir           string
	HookFilesScanned   int
	BeadsHookFiles     []string
	HookPlans          []hookCleanupPlan
	AgentsPlan         agentsCleanupPlan
	BeadsArtifacts     []string
	ConfigFilesScanned int
	ConfigPlans        []configCleanupPlan
}

func (scan beadsResidueScan) HasResidue() bool {
	if len(scan.BeadsHookFiles) > 0 {
		return true
	}
	if scan.AgentsPlan.Found && scan.AgentsPlan.Before > 0 {
		return true
	}
	if len(scan.BeadsArtifacts) > 0 {
		return true
	}
	return len(scan.ConfigPlans) > 0
}

func (scan beadsResidueScan) Summary() string {
	parts := make([]string, 0, 4)
	if len(scan.BeadsHookFiles) > 0 {
		parts = append(parts, fmt.Sprintf("hooks=%d", len(scan.BeadsHookFiles)))
	}
	if scan.AgentsPlan.Found && scan.AgentsPlan.Before > 0 {
		parts = append(parts, "agents=1")
	}
	if len(scan.BeadsArtifacts) > 0 {
		parts = append(parts, fmt.Sprintf("artifacts=%d", len(scan.BeadsArtifacts)))
	}
	if len(scan.ConfigPlans) > 0 {
		parts = append(parts, fmt.Sprintf("configs=%d", len(scan.ConfigPlans)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func (scan beadsResidueScan) backupTargets() []string {
	paths := make([]string, 0, len(scan.HookPlans)+1+len(scan.BeadsArtifacts)+len(scan.ConfigPlans))
	for _, plan := range scan.HookPlans {
		if plan.Changed {
			paths = append(paths, plan.Path)
		}
	}
	if scan.AgentsPlan.Changed {
		paths = append(paths, scan.AgentsPlan.Path)
	}
	paths = append(paths, scan.BeadsArtifacts...)
	for _, plan := range scan.ConfigPlans {
		if plan.Changed {
			paths = append(paths, plan.Path)
		}
	}
	return uniqueExistingPaths(paths)
}

var repoLocalBeadsConfigFiles = []string{
	".claude/settings.json",
	".claude/settings.local.json",
	".gemini/settings.json",
	".junie/mcp/mcp.json",
	".mcp.json",
	".claude-plugin/marketplace.json",
	"claude-plugin/.claude-plugin/plugin.json",
}

func runMigrate(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	applyChanges := fs.Bool("apply", false, "Apply migration changes (default: dry-run)")
	jsonOut := fs.Bool("json", false, "Output JSON")
	skipHooks := fs.Bool("skip-hooks", false, "Skip git hook installation")
	skipAgents := fs.Bool("skip-agents", false, "Skip AGENTS.md integration update")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit migrate [--apply] [--json] [--skip-hooks] [--skip-agents]")
	}

	report, err := runMigrationWithOptions(
		ctx,
		ws,
		*applyChanges,
		migrateApplyOptions{InstallHooks: !*skipHooks, InstallAgents: !*skipAgents},
		nil,
	)
	if err != nil {
		return err
	}
	return printValue(stdout, report, *jsonOut, func(w io.Writer, v any) error {
		r := v.(migrateReport)
		_, printErr := fmt.Fprintf(
			w,
			"mode=%s scanned=%d beads_hooks=%d modified=%d removed=%d data_imported=%t agents_updated=%t lit_hook_installed=%t\n",
			r.Mode,
			r.HookFilesScanned,
			len(r.BeadsHookFiles),
			len(r.HookFilesModified),
			len(r.HookFilesRemoved),
			r.DataImported,
			r.AgentsUpdated,
			r.LitHookInstalled,
		)
		return printErr
	})
}

func runMigrationWithOptions(ctx context.Context, ws workspace.Info, applyChanges bool, options migrateApplyOptions, preScanned *beadsResidueScan) (migrateReport, error) {
	mode := "dry-run"
	if applyChanges {
		mode = "apply"
	}

	scan := beadsResidueScan{}
	if preScanned != nil {
		scan = *preScanned
	} else {
		var scanErr error
		scan, scanErr = scanBeadsResidue(ws)
		if scanErr != nil {
			return migrateReport{}, scanErr
		}
	}

	report := migrateReport{
		Mode:               mode,
		Applied:            applyChanges,
		ResidueDetected:    scan.HasResidue(),
		HooksDir:           scan.HooksDir,
		HookFilesScanned:   scan.HookFilesScanned,
		BeadsHookFiles:     append([]string(nil), scan.BeadsHookFiles...),
		AgentsPath:         scan.AgentsPlan.Path,
		AgentsFound:        scan.AgentsPlan.Found,
		AgentsBefore:       scan.AgentsPlan.Before,
		AgentsAfter:        scan.AgentsPlan.After,
		AgentsUpdated:      scan.AgentsPlan.Changed,
		BeadsArtifacts:     append([]string(nil), scan.BeadsArtifacts...),
		ConfigFilesScanned: scan.ConfigFilesScanned,
		Notes:              []string{},
	}
	for _, plan := range scan.ConfigPlans {
		report.ConfigFilesDetected = append(report.ConfigFilesDetected, plan.Path)
	}
	beadsDataPath, hasBeadsDataPath, beadsDataPathErr := detectBeadsDataPath(ws.RootDir)
	if beadsDataPathErr != nil {
		return report, beadsDataPathErr
	}
	if hasBeadsDataPath {
		report.ImportSource = beadsDataPath
	}

	if !applyChanges {
		report.Notes = append(report.Notes, "dry-run: no files modified; rerun with --apply")
		if options.InstallHooks || options.InstallAgents {
			report.Notes = append(report.Notes, "dry-run: lit setup stages skipped")
		}
		if hasBeadsDataPath {
			report.Notes = append(report.Notes, "dry-run: beads issue data import skipped")
		}
		return report, nil
	}

	targets := scan.backupTargets()
	if len(targets) > 0 {
		backupPath, backupErr := createMigrationBackup(ws, targets)
		if backupErr != nil {
			return report, backupErr
		}
		report.BackupPath = backupPath
	}

	if hasBeadsDataPath {
		// [LAW:one-source-of-truth] Reuse the canonical legacy importer so migrate paths share one translation boundary.
		importSummary, importErr := importBeadsData(ctx, ws, beadsDataPath)
		if importErr != nil {
			return report, importErr
		}
		report.DataImported = true
		report.ImportIssues = importSummary.Issues
		report.ImportRelations = importSummary.Relations
		report.ImportComments = importSummary.Comments
		report.ImportLabels = importSummary.Labels
	}

	if err := applyHookCleanup(scan.HookPlans, &report); err != nil {
		return report, err
	}
	if err := applyAgentsCleanup(scan.AgentsPlan, &report); err != nil {
		return report, err
	}
	if err := applyArtifactCleanup(scan.BeadsArtifacts, &report); err != nil {
		return report, err
	}
	if err := applyConfigCleanup(scan.ConfigPlans, &report); err != nil {
		return report, err
	}

	if options.InstallHooks {
		hookResult, hookErr := installHooks(ws)
		if hookErr != nil {
			return report, hookErr
		}
		report.LitHookInstalled = true
		report.LitHookPath = hookResult.HookPath
		report.LitLegacyChainPath = hookResult.LegacyPath
	}
	if options.InstallAgents {
		agentsResult, agentsErr := ensureLinksAgentsSection(ws.RootDir)
		if agentsErr != nil {
			return report, agentsErr
		}
		report.AgentsPath = agentsResult.Path
		report.AgentsFound = true
		if agentsResult.Changed {
			report.AgentsUpdated = true
		}
	}

	if !report.ResidueDetected {
		report.Notes = append(report.Notes, "no beads residue detected")
	}
	return report, nil
}

func scanBeadsResidue(ws workspace.Info) (beadsResidueScan, error) {
	scan := beadsResidueScan{
		HooksDir:   filepath.Join(ws.GitCommonDir, "hooks"),
		AgentsPlan: agentsCleanupPlan{Path: filepath.Join(ws.RootDir, "AGENTS.md")},
	}

	entries, err := os.ReadDir(scan.HooksDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(scan.HooksDir, entry.Name())
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return scan, fmt.Errorf("read hook %s: %w", path, readErr)
			}
			scan.HookFilesScanned++
			if !hasBeadsSignal(content) {
				continue
			}
			scan.BeadsHookFiles = append(scan.BeadsHookFiles, path)
			cleaned, removeFile, changed := stripBeadsHookMentions(content)
			scan.HookPlans = append(scan.HookPlans, hookCleanupPlan{
				Path:       path,
				NewContent: cleaned,
				RemoveFile: removeFile,
				Changed:    changed,
			})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return scan, fmt.Errorf("read hooks dir: %w", err)
	}

	agentsContent, agentsErr := os.ReadFile(scan.AgentsPlan.Path)
	if agentsErr == nil {
		scan.AgentsPlan.Found = true
		scan.AgentsPlan.Before = countRegexMatches(beadsSignalRegex, agentsContent)
		updated := string(agentsContent)
		if withoutSection, removed := stripBeadsAgentsSection(updated); removed {
			updated = withoutSection
		}
		if withoutBeadsLines, lineChanged := stripBeadsTextLines(updated); lineChanged {
			updated = withoutBeadsLines
		}
		scan.AgentsPlan.After = countRegexMatches(beadsSignalRegex, []byte(updated))
		scan.AgentsPlan.Changed = updated != string(agentsContent)
		scan.AgentsPlan.NewContent = []byte(updated)
	} else if !errors.Is(agentsErr, os.ErrNotExist) {
		return scan, fmt.Errorf("read AGENTS.md: %w", agentsErr)
	}

	for _, relative := range []string{".beads", ".beads-hooks"} {
		path := filepath.Join(ws.RootDir, relative)
		if _, statErr := os.Stat(path); statErr == nil {
			scan.BeadsArtifacts = append(scan.BeadsArtifacts, path)
		}
	}

	for _, relative := range repoLocalBeadsConfigFiles {
		fullPath := filepath.Join(ws.RootDir, relative)
		content, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return scan, fmt.Errorf("read config %s: %w", fullPath, readErr)
		}
		scan.ConfigFilesScanned++
		if !hasBeadsSignal(content) {
			continue
		}
		plan, planErr := buildConfigCleanupPlan(fullPath, content)
		if planErr != nil {
			return scan, planErr
		}
		scan.ConfigPlans = append(scan.ConfigPlans, plan)
	}

	sort.Strings(scan.BeadsHookFiles)
	sort.Strings(scan.BeadsArtifacts)
	return scan, nil
}

func hasBeadsSignal(content []byte) bool {
	return beadsSignalRegex.Match(content)
}

func applyHookCleanup(plans []hookCleanupPlan, report *migrateReport) error {
	for _, plan := range plans {
		if !plan.Changed {
			continue
		}
		if plan.RemoveFile {
			if err := os.Remove(plan.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove hook %s: %w", plan.Path, err)
			}
			report.HookFilesRemoved = append(report.HookFilesRemoved, plan.Path)
			continue
		}
		info, err := os.Stat(plan.Path)
		if err != nil {
			return fmt.Errorf("stat hook %s: %w", plan.Path, err)
		}
		if err := os.WriteFile(plan.Path, plan.NewContent, info.Mode().Perm()); err != nil {
			return fmt.Errorf("write hook %s: %w", plan.Path, err)
		}
		report.HookFilesModified = append(report.HookFilesModified, plan.Path)
	}
	return nil
}

func applyAgentsCleanup(plan agentsCleanupPlan, report *migrateReport) error {
	if !plan.Found || !plan.Changed {
		return nil
	}
	if err := os.WriteFile(plan.Path, plan.NewContent, 0o644); err != nil {
		return fmt.Errorf("write AGENTS.md: %w", err)
	}
	report.AgentsUpdated = true
	report.AgentsAfter = countRegexMatches(beadsSignalRegex, plan.NewContent)
	return nil
}

func applyArtifactCleanup(paths []string, report *migrateReport) error {
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove beads artifact %s: %w", path, err)
		}
	}
	return nil
}

func applyConfigCleanup(plans []configCleanupPlan, report *migrateReport) error {
	for _, plan := range plans {
		if !plan.Changed {
			continue
		}
		if plan.RemoveFile {
			if err := os.Remove(plan.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove config %s: %w", plan.Path, err)
			}
			report.ConfigFilesRemoved = append(report.ConfigFilesRemoved, plan.Path)
			continue
		}
		if err := os.WriteFile(plan.Path, plan.NewContent, 0o644); err != nil {
			return fmt.Errorf("write config %s: %w", plan.Path, err)
		}
		report.ConfigFilesModified = append(report.ConfigFilesModified, plan.Path)
	}
	return nil
}

func buildConfigCleanupPlan(path string, content []byte) (configCleanupPlan, error) {
	plan := configCleanupPlan{Path: path}
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		var payload any
		if err := json.Unmarshal(content, &payload); err == nil {
			cleaned, changed, keep := pruneBeadsJSON(payload)
			if !keep {
				plan.RemoveFile = true
				plan.Changed = true
				return plan, nil
			}
			if changed {
				encoded, encodeErr := json.MarshalIndent(cleaned, "", "  ")
				if encodeErr != nil {
					return plan, fmt.Errorf("marshal config %s: %w", path, encodeErr)
				}
				plan.NewContent = append(encoded, '\n')
				plan.Changed = true
				return plan, nil
			}
			return plan, nil
		}
	}
	// [LAW:no-silent-fallbacks] aggressive migration removes detected beads config artifacts if structured cleanup is not possible.
	plan.RemoveFile = true
	plan.Changed = true
	return plan, nil
}

func pruneBeadsJSON(value any) (any, bool, bool) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any)
		changed := false
		for key, nested := range typed {
			if hasBeadsSignal([]byte(key)) {
				changed = true
				continue
			}
			pruned, nestedChanged, keep := pruneBeadsJSON(nested)
			changed = changed || nestedChanged
			if !keep {
				changed = true
				continue
			}
			result[key] = pruned
		}
		return result, changed || len(result) != len(typed), len(result) > 0
	case []any:
		result := make([]any, 0, len(typed))
		changed := false
		for _, nested := range typed {
			pruned, nestedChanged, keep := pruneBeadsJSON(nested)
			changed = changed || nestedChanged
			if !keep {
				changed = true
				continue
			}
			result = append(result, pruned)
		}
		return result, changed || len(result) != len(typed), len(result) > 0
	case string:
		if hasBeadsSignal([]byte(typed)) {
			return nil, true, false
		}
		return typed, false, true
	default:
		return value, false, true
	}
}

func createMigrationBackup(ws workspace.Info, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	backupDir := filepath.Join(ws.StorageDir, "migrations", timestamp+"-beads-cleanup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create migration backup dir: %w", err)
	}
	type backupEntry struct {
		Path string `json:"path"`
	}
	manifestEntries := make([]backupEntry, 0, len(paths))
	for index, path := range paths {
		entryDir := filepath.Join(backupDir, "entries", fmt.Sprintf("%03d", index))
		contentPath := filepath.Join(entryDir, "content")
		if err := os.MkdirAll(entryDir, 0o755); err != nil {
			return "", fmt.Errorf("create migration backup entry: %w", err)
		}
		if err := copyPath(path, contentPath); err != nil {
			return "", fmt.Errorf("backup %s: %w", path, err)
		}
		manifestEntries = append(manifestEntries, backupEntry{Path: path})
	}
	manifest := map[string]any{
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"paths":      manifestEntries,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal backup manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return "", fmt.Errorf("write backup manifest: %w", err)
	}
	return backupDir, nil
}

func detectBeadsDataPath(rootDir string) (string, bool, error) {
	path := filepath.Join(rootDir, ".beads")
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat beads data path %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", false, nil
	}
	candidates := []string{
		filepath.Join(path, ".dolt"),
		filepath.Join(path, "beads", ".dolt"),
	}
	entries, readErr := os.ReadDir(path)
	if readErr != nil {
		return "", false, fmt.Errorf("read beads data path %s: %w", path, readErr)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidates = append(candidates, filepath.Join(path, entry.Name(), ".dolt"))
	}

	for _, candidate := range candidates {
		doltInfo, doltErr := os.Stat(candidate)
		if doltErr != nil {
			if errors.Is(doltErr, os.ErrNotExist) {
				continue
			}
			return "", false, fmt.Errorf("stat beads dolt dir %s: %w", candidate, doltErr)
		}
		if doltInfo.IsDir() {
			return path, true, nil
		}
	}
	return "", false, nil
}

func importBeadsData(ctx context.Context, ws workspace.Info, beadsPath string) (legacydolt.Summary, error) {
	st, err := store.Open(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return legacydolt.Summary{}, fmt.Errorf("open links store for beads import: %w", err)
	}
	defer st.Close()
	summary, importErr := legacydolt.Import(ctx, st, beadsPath)
	if importErr != nil {
		return legacydolt.Summary{}, fmt.Errorf("import beads data from %s: %w", beadsPath, importErr)
	}
	return summary, nil
}

func copyPath(src string, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(src)
		if readErr != nil {
			return readErr
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	return copyFile(src, dst, info.Mode().Perm())
}

func copyDir(src string, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src string, dst string, mode fs.FileMode) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, input, mode)
}

func uniqueExistingPaths(paths []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		if _, err := os.Stat(trimmed); err != nil {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	sort.Strings(result)
	return result
}

func stripBeadsHookMentions(content []byte) ([]byte, bool, bool) {
	original := string(content)
	lines := strings.Split(original, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if hasBeadsSignal([]byte(line)) {
			continue
		}
		kept = append(kept, line)
	}
	normalized := normalizeBlankLines(strings.Join(kept, "\n"))
	changed := normalized != original
	if !changed {
		return content, false, false
	}
	if !hasHookLogic(normalized) {
		return []byte{}, true, true
	}
	if !strings.HasSuffix(normalized, "\n") {
		normalized += "\n"
	}
	return []byte(normalized), false, true
}

func stripBeadsTextLines(input string) (string, bool) {
	lines := strings.Split(input, "\n")
	kept := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if hasBeadsSignal([]byte(line)) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	normalized := normalizeBlankLines(strings.Join(kept, "\n"))
	if !strings.HasSuffix(normalized, "\n") && normalized != "" {
		normalized += "\n"
	}
	return normalized, changed || normalized != input
}

func normalizeBlankLines(input string) string {
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	previousBlank := false
	for _, line := range lines {
		blank := strings.TrimSpace(line) == ""
		if blank && previousBlank {
			continue
		}
		out = append(out, line)
		previousBlank = blank
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func hasHookLogic(input string) bool {
	lines := strings.Split(input, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#!") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return true
	}
	return false
}

func countRegexMatches(pattern *regexp.Regexp, content []byte) int {
	return len(pattern.FindAll(content, -1))
}
