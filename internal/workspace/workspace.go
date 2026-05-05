package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/issueid"
	"github.com/google/uuid"
)

var ErrNotGitRepo = errors.New("links requires a git repository/worktree")

type Config struct {
	WorkspaceID string    `json:"workspace_id"`
	IssuePrefix string    `json:"issue_prefix"`
	CreatedAt   time.Time `json:"created_at"`
	Version     int       `json:"schema_version"`
}

type Info struct {
	RootDir      string
	GitCommonDir string
	StorageDir   string
	ConfigPath   string
	DatabasePath string
	DoltRepoPath string
	WorkspaceID  string
	IssuePrefix  string
}

type GitRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func UpstreamRemote(cwd string) string {
	upstreamRef, _ := gitOutput(cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	return upstreamRemoteFromRef(upstreamRef)
}

func RemoteHasRefs(cwd string, remote string) (bool, error) {
	remoteName := normalizeRemoteName(remote)
	output, err := gitOutput(cwd, "ls-remote", remoteName)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

func DefaultRemoteBranch(cwd string, remote string) string {
	remoteName := normalizeRemoteName(remote)
	symbolicRefOutput, _ := gitOutput(cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remoteName+"/HEAD")
	symbolicBranch := strings.TrimSpace(defaultRemoteBranchFromSymbolicRef(remoteName, symbolicRefOutput))
	if symbolicBranch != "" {
		return symbolicBranch
	}
	lsRemoteOutput, _ := gitOutput(cwd, "ls-remote", "--symref", remoteName, "HEAD")
	// [LAW:one-source-of-truth] Branch resolution follows one deterministic candidate chain: local remote HEAD, then remote HEAD advertisement.
	return strings.TrimSpace(defaultRemoteBranchFromLSRemote(lsRemoteOutput))
}

func Resolve(cwd string) (Info, error) {
	rootDir, err := gitOutput(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return Info{}, ErrNotGitRepo
	}
	gitCommonDirRaw, err := gitOutput(cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return Info{}, ErrNotGitRepo
	}
	gitCommonDir := gitCommonDirRaw
	if !filepath.IsAbs(gitCommonDir) {
		gitCommonDir = filepath.Join(rootDir, gitCommonDirRaw)
	}
	storageDir := filepath.Join(filepath.Clean(gitCommonDir), "links")
	configPath := filepath.Join(storageDir, "config.json")
	databasePath := filepath.Join(storageDir, "dolt")
	doltRepoPath := filepath.Join(databasePath, "links")
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create storage dir: %w", err)
	}
	cfg, err := loadOrCreateConfig(rootDir, configPath)
	if err != nil {
		return Info{}, err
	}
	return Info{
		RootDir:      rootDir,
		GitCommonDir: filepath.Clean(gitCommonDir),
		StorageDir:   storageDir,
		ConfigPath:   configPath,
		DatabasePath: databasePath,
		DoltRepoPath: doltRepoPath,
		WorkspaceID:  cfg.WorkspaceID,
		IssuePrefix:  cfg.IssuePrefix,
	}, nil
}

func gitOutput(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func normalizeRemoteName(remote string) string {
	trimmed := strings.TrimSpace(remote)
	if trimmed == "" {
		return "origin"
	}
	return trimmed
}

func defaultRemoteBranchFromSymbolicRef(remote string, symbolicRef string) string {
	ref := strings.TrimSpace(symbolicRef)
	prefix := strings.TrimSpace(remote) + "/"
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(ref, prefix))
}

func defaultRemoteBranchFromLSRemote(output string) string {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ref: refs/heads/") || !strings.HasSuffix(trimmed, "\tHEAD") {
			continue
		}
		branch := strings.TrimPrefix(trimmed, "ref: refs/heads/")
		branch = strings.TrimSuffix(branch, "\tHEAD")
		return strings.TrimSpace(branch)
	}
	return ""
}

func upstreamRemoteFromRef(ref string) string {
	trimmed := strings.TrimSpace(ref)
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func GitRemotes(cwd string) ([]GitRemote, error) {
	output, err := gitOutput(cwd, "remote", "-v")
	if err != nil {
		return nil, err
	}
	entries := strings.Split(strings.TrimSpace(output), "\n")
	byName := map[string]string{}
	for _, entry := range entries {
		line := strings.TrimSpace(entry)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		url := fields[1]
		scope := strings.Trim(fields[2], "()")
		if scope != "fetch" {
			continue
		}
		byName[name] = url
	}
	remotes := make([]GitRemote, 0, len(byName))
	for name, url := range byName {
		remotes = append(remotes, GitRemote{Name: name, URL: url})
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return remotes, nil
}

func loadOrCreateConfig(rootDir string, path string) (Config, error) {
	payload, err := os.ReadFile(path)
	if err == nil {
		var cfg Config
		if err := json.Unmarshal(payload, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse workspace config: %w", err)
		}
		if cfg.WorkspaceID == "" {
			return Config{}, errors.New("workspace config missing workspace_id")
		}
		if strings.TrimSpace(cfg.IssuePrefix) == "" {
			derivedPrefix, err := deriveIssuePrefix(rootDir)
			if err != nil {
				return Config{}, err
			}
			cfg.IssuePrefix = derivedPrefix
			return writeConfig(path, cfg)
		}
		normalizedPrefix, err := issueid.NormalizeConfiguredPrefix(cfg.IssuePrefix)
		if err != nil {
			return Config{}, fmt.Errorf("invalid issue_prefix: %w", err)
		}
		if normalizedPrefix != cfg.IssuePrefix {
			cfg.IssuePrefix = normalizedPrefix
			return writeConfig(path, cfg)
		}
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read workspace config: %w", err)
	}
	cfg := Config{
		WorkspaceID: uuid.NewString(),
		CreatedAt:   time.Now().UTC(),
		Version:     1,
	}
	cfg.IssuePrefix, err = deriveIssuePrefix(rootDir)
	if err != nil {
		return Config{}, err
	}
	return writeConfig(path, cfg)
}

func writeConfig(path string, cfg Config) (Config, error) {
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Config{}, fmt.Errorf("marshal workspace config: %w", err)
	}
	payload = append(payload, '\n')
	// [LAW:single-enforcer] Same-directory temp-file + rename is the atomic-write
	// boundary every config writer flows through, so a crash between truncate
	// and write cannot leave config.json empty or partially written.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json.*")
	if err != nil {
		return Config{}, fmt.Errorf("create workspace config temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return Config{}, fmt.Errorf("write workspace config temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return Config{}, fmt.Errorf("chmod workspace config temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return Config{}, fmt.Errorf("close workspace config temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return Config{}, fmt.Errorf("rename workspace config: %w", err)
	}
	return cfg, nil
}

// UpdateConfig reads the workspace config at path, applies mutate, and writes
// the result back. The mutate callback owns validation of the new shape; a
// non-nil error from it aborts the write. Returns the post-mutate config.
//
// [LAW:single-enforcer] All in-place edits to the workspace config go through
// this single read-modify-write boundary so partial writes can't desync
// callers from on-disk state.
func UpdateConfig(path string, mutate func(Config) (Config, error)) (Config, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read workspace config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse workspace config: %w", err)
	}
	updated, err := mutate(cfg)
	if err != nil {
		return Config{}, err
	}
	return writeConfig(path, updated)
}

func deriveIssuePrefix(rootDir string) (string, error) {
	base := issueid.NormalizeSlug(filepath.Base(rootDir))
	if base == "" {
		return "", fmt.Errorf("derive issue_prefix: repository name %q does not contain at least %d normalized characters", filepath.Base(rootDir), issueid.PrefixMinLength)
	}
	parts := strings.Split(base, "-")
	for _, part := range parts {
		candidate, err := issueid.NormalizeConfiguredPrefix(part)
		if err == nil && candidate != "" {
			return candidate, nil
		}
	}
	candidate, err := issueid.NormalizeConfiguredPrefix(base)
	if err != nil || candidate == "" {
		return "", fmt.Errorf("derive issue_prefix: repository name %q does not produce a valid prefix", filepath.Base(rootDir))
	}
	return candidate, nil
}
