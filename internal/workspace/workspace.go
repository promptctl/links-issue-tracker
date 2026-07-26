package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/promptctl/links-issue-tracker/internal/issueid"
)

var ErrNotGitRepo = errors.New("links requires a git repository/worktree")

type Config struct {
	WorkspaceID string    `json:"workspace_id"`
	IssuePrefix string    `json:"issue_prefix"`
	CreatedAt   time.Time `json:"created_at"`
	Version     int       `json:"schema_version"`
}

type Info struct {
	// Location holds the store's path geometry. Embedding it — rather than
	// re-listing StorageDir/DatabasePath/… as Info's own fields and copying each
	// across in Resolve — keeps those paths in exactly one place, so a new
	// Location field cannot silently fail to appear on Info. [LAW:one-source-of-truth]
	// Field access (ws.StorageDir, ws.DatabasePath) is unchanged via promotion.
	Location
	RootDir     string
	WorkspaceID string
	IssuePrefix PrefixSpec
}

// Location is the on-disk geometry of a lit store — every path derived from a
// git repository's git-common-dir, and nothing that requires reading or writing
// the store. It is what a filesystem scan can know about a store WITHOUT opening
// or creating it: Resolve layers store creation, config, and identity on top;
// Discover uses it to detect a store in place. [LAW:one-source-of-truth] The
// paths are minted only by deriveLocation, so a discovered Location and the
// Info Resolve opens for the same repository are the same store by construction.
type Location struct {
	GitCommonDir string
	StorageDir   string
	ConfigPath   string
	DatabasePath string
	DoltRepoPath string
}

// PrefixSpec is a resolved issue prefix together with its provenance. The
// value is normalized and non-empty by construction; the only ways to obtain
// one are ConfiguredPrefix and resolveIssuePrefix, so no consumer ever needs
// to re-trim or re-validate. [LAW:types-are-the-program]
type PrefixSpec struct {
	value   string
	derived bool
}

// ConfiguredPrefix validates and normalizes a prefix that a caller holds as a
// configured value (config file, user input, test fixture). It is the only
// exported way to mint a PrefixSpec. [LAW:single-enforcer]
func ConfiguredPrefix(raw string) (PrefixSpec, error) {
	normalized, err := issueid.NormalizeConfiguredPrefix(raw)
	if err != nil {
		return PrefixSpec{}, err
	}
	return PrefixSpec{value: normalized}, nil
}

func (p PrefixSpec) Value() string { return p.value }

// Derived reports whether this load minted the prefix from the repository
// name rather than reading it from config. The derived value is persisted
// immediately, so provenance is per-load: the next load reads it back as
// configured. Carried so the one run that invents a prefix the user never
// chose is observable, not silent. [LAW:no-silent-failure]
func (p PrefixSpec) Derived() bool { return p.derived }

type GitRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func UpstreamRemote(ctx context.Context, cwd string) string {
	upstreamRef, _ := gitOutput(ctx, cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	return upstreamRemoteFromRef(upstreamRef)
}

func RemoteHasRefs(ctx context.Context, cwd string, remote string) (bool, error) {
	remoteName := normalizeRemoteName(remote)
	output, err := gitOutput(ctx, cwd, "ls-remote", remoteName)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// RemoteHasDoltData reports whether the remote advertises lit's Dolt ticket data
// — the refs/dolt/* namespace lit pushes its store into. This is the
// authoritative "the remote carries a backlog" signal: RemoteHasRefs is true for
// any git repo (code refs alone), so only the presence of refs/dolt/* tells
// "remote has tickets to adopt" apart from "remote is just a code repo". The
// adopt step keys its loud-vs-silent decision on this so an empty store that
// hides a real remote backlog is unrepresentable. [LAW:one-source-of-truth]
func RemoteHasDoltData(ctx context.Context, cwd string, remote string) (bool, error) {
	remoteName := normalizeRemoteName(remote)
	output, err := gitOutput(ctx, cwd, "ls-remote", remoteName, "refs/dolt/*")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

func DefaultRemoteBranch(ctx context.Context, cwd string, remote string) string {
	remoteName := normalizeRemoteName(remote)
	symbolicRefOutput, _ := gitOutput(ctx, cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remoteName+"/HEAD")
	symbolicBranch := strings.TrimSpace(defaultRemoteBranchFromSymbolicRef(remoteName, symbolicRefOutput))
	if symbolicBranch != "" {
		return symbolicBranch
	}
	lsRemoteOutput, _ := gitOutput(ctx, cwd, "ls-remote", "--symref", remoteName, "HEAD")
	// [LAW:one-source-of-truth] Branch resolution follows one deterministic candidate chain: local remote HEAD, then remote HEAD advertisement.
	return strings.TrimSpace(defaultRemoteBranchFromLSRemote(lsRemoteOutput))
}

func Resolve(cwd string) (Info, error) {
	// [LAW:dataflow-not-control-flow] Store-geometry git calls are local rev-parse
	// queries that cannot hang on a network, so cancellation buys nothing here;
	// context.Background() is the honest "never cancels" value, and it keeps Resolve's
	// many callers free of a ctx they would only forward to a subprocess that never
	// blocks. Cancellation is threaded only through the receive/sync path's network
	// git calls, where a wedge is reachable.
	rootDir, err := gitOutput(context.Background(), cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return Info{}, classifyGitError(fmt.Sprintf("git rev-parse --show-toplevel in %q", cwd), err)
	}
	loc, err := deriveLocation(cwd)
	if err != nil {
		return Info{}, err
	}
	// [LAW:effects-at-boundaries] deriveLocation is pure geometry; store
	// creation is the one mutation, gathered here where the write is intended.
	if err := os.MkdirAll(loc.StorageDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create storage dir: %w", err)
	}
	cfg, prefix, err := loadOrCreateConfig(rootDir, loc.ConfigPath)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Location:    loc,
		RootDir:     rootDir,
		WorkspaceID: cfg.WorkspaceID,
		IssuePrefix: prefix,
	}, nil
}

// deriveLocation computes the lit store geometry for the git repository
// containing cwd, purely — the only effects are the git queries that read
// repository geometry and the symlink canonicalization that reads the
// filesystem; nothing is created or written. [LAW:effects-at-boundaries]
// [LAW:single-enforcer] This is the one place cwd becomes a set of store paths,
// so Resolve (which then creates the store) and Discover (which then only
// detects it) can never derive different paths for the same repository.
func deriveLocation(cwd string) (Location, error) {
	// [LAW:one-source-of-truth] Git owns repository geometry. --git-common-dir is
	// emitted relative to the invocation cwd (e.g. "../.git" from a subdirectory),
	// so a relative result must be anchored to the cwd. The original defect
	// anchored it to the toplevel instead, which climbed out of the repo and
	// resolved a subdirectory/worktree invocation to the wrong store. Anchoring
	// to the cwd is correct on every Git version (no dependency on the newer
	// --path-format=absolute flag, which would break older Git with a misleading
	// "not a git repo" error).
	// [LAW:dataflow-not-control-flow] Local geometry query; never blocks on a
	// network, so context.Background() (never cancels) is the correct value — see
	// Resolve for why the receive/sync path threads a real ctx and this does not.
	gitCommonDir, err := gitOutput(context.Background(), cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return Location{}, classifyGitError(fmt.Sprintf("git rev-parse --git-common-dir in %q", cwd), err)
	}
	if !filepath.IsAbs(gitCommonDir) {
		absCwd, err := filepath.Abs(cwd)
		if err != nil {
			return Location{}, fmt.Errorf("resolve absolute cwd: %w", err)
		}
		gitCommonDir = filepath.Join(absCwd, gitCommonDir)
	}
	// [LAW:one-source-of-truth] A store's identity is its physical directory, not
	// the path string a caller happened to hold. filepath.Abs makes a path
	// absolute but does not resolve symlinks, so the same store reached from a
	// symlinked ancestor (macOS /var -> /private/var) yields two spellings. The
	// dolt driver caches its environment per path string and serves the second
	// spelling a read-only handle, so two spellings of one store become a
	// read-only conflict. Canonicalizing here collapses every spelling to the
	// physical path before any storage path is derived from it. It also makes
	// worktree deduplication exact: every worktree of one repository shares this
	// canonical common dir, so all of them derive one identical StorageDir.
	canonicalCommonDir, err := filepath.EvalSymlinks(gitCommonDir)
	if err != nil {
		return Location{}, fmt.Errorf("canonicalize git-common-dir %q: %w", gitCommonDir, err)
	}
	gitCommonDir = filepath.Clean(canonicalCommonDir)
	return LocationFromStorageDir(filepath.Join(gitCommonDir, "links")), nil
}

// LocationFromStorageDir mints the store geometry rooted at an already-resolved
// StorageDir — the "dolt", "config.json", and links-repo suffixes and nothing
// else. [LAW:single-enforcer] These suffixes live here only; deriveLocation
// resolves a git-common-dir to a StorageDir and then hands off here, and a caller
// holding a StorageDir string (a `lit stores` line) reconstructs its Location the
// same way. So a Location rebuilt from a path and one derived from its repository
// are identical by construction, never two spellings of one store's geometry.
func LocationFromStorageDir(storageDir string) Location {
	databasePath := filepath.Join(storageDir, "dolt")
	return Location{
		// StorageDir is <git-common-dir>/links, so the common dir is its parent —
		// recovered rather than stored separately so the two cannot disagree.
		GitCommonDir: filepath.Dir(storageDir),
		StorageDir:   storageDir,
		ConfigPath:   filepath.Join(storageDir, "config.json"),
		DatabasePath: databasePath,
		DoltRepoPath: filepath.Join(databasePath, "links"),
	}
}

// gitOutput runs one git subprocess and returns its trimmed stdout. It spawns
// through exec.CommandContext so a cancelled ctx kills the subprocess promptly:
// a network-wedged call (ls-remote/fetch to an unreachable remote) abandons on
// cancellation instead of outliving it. Cancellation is a value crossing this one
// seam, not a second code path — a caller whose git call cannot hang on the network
// (local rev-parse geometry) passes context.Background(), which never cancels and so
// reproduces the pre-ctx behavior exactly. [LAW:dataflow-not-control-flow]
// [LAW:no-ambient-temporal-coupling] the subprocess lifecycle is owned by ctx, not
// left to outlive a cancelled command until a grace-timer hard-exit reaps it.
func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitFatalExitCode is git's universal exit code for a fatal condition — the code
// `rev-parse` returns both for "not a git repository" and "this operation must
// be run in a work tree", the two ways a directory legitimately fails to be a
// lit-usable repository.
const gitFatalExitCode = 128

// classifyGitError decides whether a failed git invocation means "this directory
// is not a lit-usable git repository" (the ErrNotGitRepo sentinel every repo
// gate skips on) or a real failure that must surface. Only git exiting with its
// fatal code (128) is the sentinel — that is exactly what rev-parse returns for
// a non-repository or a work-tree-less repo. Everything else would otherwise
// masquerade as "no repo here": git failing to run at all (not installed / not
// on PATH: not an ExitError), git killed by a signal (ExitCode() == -1: OOM
// killer, forced kill), or any other non-128 exit. Those are wrapped with
// context so a scan reports the real problem instead of silently returning "no
// stores found". Keying on the numeric code — not on git's human stderr text —
// keeps the classification locale-independent. [LAW:no-silent-failure]
//
// [LAW:single-enforcer] Both git repo gates — Resolve's --show-toplevel and
// deriveLocation's --git-common-dir — route through here, so they cannot drift
// into classifying the same git failure two different ways.
func classifyGitError(context string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == gitFatalExitCode {
		return ErrNotGitRepo
	}
	return fmt.Errorf("%s: %w", context, err)
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

func GitRemotes(ctx context.Context, cwd string) ([]GitRemote, error) {
	output, err := gitOutput(ctx, cwd, "remote", "-v")
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

// resolveIssuePrefix is the single enforcer of the prefix rule: an absent
// (empty after trimming) configured value is derived from the repository
// name; a present value is normalized; an invalid present value is a loud
// error, never a silent fallback to derivation. [LAW:single-enforcer]
// [LAW:no-silent-failure]
func resolveIssuePrefix(rootDir string, configured string) (PrefixSpec, error) {
	if strings.TrimSpace(configured) == "" {
		derived, err := deriveIssuePrefix(rootDir)
		if err != nil {
			return PrefixSpec{}, err
		}
		return PrefixSpec{value: derived, derived: true}, nil
	}
	spec, err := ConfiguredPrefix(configured)
	if err != nil {
		return PrefixSpec{}, fmt.Errorf("invalid issue_prefix: %w", err)
	}
	return spec, nil
}

// ReadConfig reads and validates a workspace config.json WITHOUT creating,
// deriving, or writing anything — the read-only counterpart to
// loadOrCreateConfig. It is how a foreign store is identified when opening it
// read-only across projects: the workspace_id OpenForRead needs lives here, and
// this store is one we must never mutate, so the create/derive/persist path is
// not an option. [LAW:effects-at-boundaries] Exactly one file read, no writes.
// [LAW:single-enforcer] The read+parse+workspace_id-present check is defined
// here; loadOrCreateConfig reuses it so the two cannot validate a config two
// different ways. The read error is wrapped preserving os.ErrNotExist via %w, so
// loadOrCreateConfig can still tell "no config yet, create one" from a real
// failure.
func ReadConfig(path string) (Config, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read workspace config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse workspace config: %w", err)
	}
	if cfg.WorkspaceID == "" {
		return Config{}, errors.New("workspace config missing workspace_id")
	}
	return cfg, nil
}

func loadOrCreateConfig(rootDir string, path string) (Config, PrefixSpec, error) {
	cfg, err := ReadConfig(path)
	if err == nil {
		prefix, err := resolveIssuePrefix(rootDir, cfg.IssuePrefix)
		if err != nil {
			return Config{}, PrefixSpec{}, err
		}
		// [LAW:one-source-of-truth] config.json holds the resolved value, so a
		// derivation or a normalization change is persisted the moment it happens.
		if prefix.Value() != cfg.IssuePrefix {
			cfg.IssuePrefix = prefix.Value()
			cfg, err = writeConfig(path, cfg)
			if err != nil {
				return Config{}, PrefixSpec{}, err
			}
		}
		return cfg, prefix, nil
	}
	// [LAW:no-silent-failure] A missing config is the ordinary "not yet
	// initialized" case that this create path handles; a parse error, an empty
	// workspace_id, or any other read failure is already fully described by
	// ReadConfig and must surface as-is rather than be mistaken for "create one".
	if !errors.Is(err, os.ErrNotExist) {
		return Config{}, PrefixSpec{}, err
	}
	prefix, err := resolveIssuePrefix(rootDir, "")
	if err != nil {
		return Config{}, PrefixSpec{}, err
	}
	cfg = Config{
		WorkspaceID: uuid.NewString(),
		IssuePrefix: prefix.Value(),
		CreatedAt:   time.Now().UTC(),
		Version:     1,
	}
	cfg, err = writeConfig(path, cfg)
	if err != nil {
		return Config{}, PrefixSpec{}, err
	}
	return cfg, prefix, nil
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
