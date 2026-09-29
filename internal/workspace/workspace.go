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

// ErrIssuePrefixRefused marks every refusal to settle on a workspace's
// issue prefix: a repository name that cannot produce one, an explicit request
// that contradicts the prefix a workspace already carries, and a stored
// issue_prefix the rules refuse. All three are terminal for the command as
// issued, so no caller may retry one unchanged, and that is what the shared
// sentinel carries.
//
// It is not the whole classification. Two of the three are cleared by adjusting
// the command; the third is cleared by running a different command first, and
// StoredPrefixError below is how a caller tells them apart without reading the
// English.
//
// Without a type these would reach the unclassified default, which tells the
// caller to retry a deterministic refusal and then to run `lit doctor` against
// the very workspace `lit init` has just declined to create.
// [LAW:no-silent-failure] [LAW:types-are-the-program] classification is carried
// by the error, never re-derived from its text.
var ErrIssuePrefixRefused = errors.New("issue prefix refused")

// StoredPrefixError is the member of that family that no rewording of the
// refused command clears: config.json carries an issue_prefix the rules refuse,
// so no issue can be minted under it. It is raised by whatever asks the
// workspace for a prefix to mint under, never by resolving the workspace, so
// `lit prefix set` (the repair) and `lit doctor` (the diagnosis) both run in
// the state it describes.
//
// That difference has to live in the type, because the CLI picks its remediation
// from the classification: the shared one ends "adjust the command to satisfy
// it", which is false here — the refused command is fine, and it is the
// workspace that needs a different command run against it first. An agent that
// acts on the remediation line rather than on the message body is the loop this
// whole mapping exists to prevent. [LAW:one-type-per-behavior] the act each
// refusal calls for is different, so a single reason could only name one of them.
//
// Unwrap returns the family sentinel, so the exit-code mapping and every
// existing errors.Is check still see one prefix refusal.
type StoredPrefixError struct {
	ConfigPath string
	Stored     string
	Err        error
}

func (e StoredPrefixError) Error() string {
	return fmt.Sprintf(
		"%s carries issue_prefix %q, which is not a legal prefix (%v), so no issue can be minted under it",
		e.ConfigPath, e.Stored, e.Err,
	)
}

func (e StoredPrefixError) Unwrap() error { return ErrIssuePrefixRefused }

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
	Location
	RootDir     string
	WorkspaceID string
	IssuePrefix PrefixState
	// PrivateGitDir is this CHECKOUT's own git directory, and it lives on Info
	// rather than on the embedded Location for a reason that is easy to get
	// backwards: Location is per-REPOSITORY geometry, identical from every
	// worktree, and LocationFromStorageDir reconstructs a whole Location from a
	// storage-dir string alone — which cannot possibly know which worktree a
	// caller is standing in. A per-checkout path put there would be silently
	// wrong in every reconstructed Location. Info is resolved from a cwd, so it
	// is the type that legitimately knows. [LAW:types-are-the-program]
	PrivateGitDir string
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

// PrefixRequest is the issue prefix a CALLER asks for — either an explicit one
// or the absence that means "derive it from the repository name". It is a
// separate type from PrefixSpec, which is a prefix a workspace HAS: that one is
// non-empty by construction, and spending its zero value on "absent" would have
// cost every consumer the guarantee its doc comment makes.
// [LAW:types-are-the-program] the strongest theorem each side can honestly
// state: one is always present, the other may not be.
type PrefixRequest struct {
	spec    PrefixSpec
	present bool
}

// RequestPrefix mints the request an explicit prefix makes, normalizing it
// through the same boundary every configured prefix crosses. It mints only
// PRESENT requests: the absence is the zero PrefixRequest, so an empty string
// reaching here is a caller who typed the flag and gave it nothing, and it is
// refused rather than quietly demoted to "no flag given".
// [LAW:single-enforcer] no caller normalizes a prefix on its own.
// [LAW:parse-dont-validate] what crosses into the workspace has already been
// checked, so nothing inland checks it again.
func RequestPrefix(raw string) (PrefixRequest, error) {
	spec, err := ConfiguredPrefix(raw)
	if err != nil {
		return PrefixRequest{}, err
	}
	return PrefixRequest{spec: spec, present: true}, nil
}

// Derived reports whether this load minted the prefix from the repository
// name rather than reading it from config. The derived value is persisted
// immediately, so provenance is per-load: the next load reads it back as
// configured. Carried so the one run that invents a prefix the user never
// chose is observable, not silent. [LAW:no-silent-failure]
func (p PrefixSpec) Derived() bool { return p.derived }

// PrefixState is what a resolved workspace knows about its issue prefix: the
// legal PrefixSpec new issues are minted under, or the value config.json
// carries that the rules refuse. The refused arm is a state the workspace is
// IN, not a failure to resolve it — reading, diagnosing and re-prefixing a
// workspace need no prefix to mint under, so only minting is refused, and
// `lit prefix set` and `lit doctor` run in exactly the state they exist to
// repair and report.
//
// There is no accessor that hands out the refused text as if it were a prefix:
// a minting caller must go through Mintable, which is the one place the
// refusal is raised. [LAW:types-are-the-program] [LAW:parse-dont-validate]
type PrefixState struct {
	spec    PrefixSpec
	refusal *StoredPrefixError
}

// LegalPrefixState is the state of a workspace whose prefix is spec.
func LegalPrefixState(spec PrefixSpec) PrefixState { return PrefixState{spec: spec} }

// Mintable is the prefix a new issue is minted under, or the StoredPrefixError
// that says why there is none. [LAW:single-enforcer] every minting path reads
// its prefix here, so every one of them refuses the same way.
func (p PrefixState) Mintable() (PrefixSpec, error) {
	if p.refusal != nil {
		return PrefixSpec{}, *p.refusal
	}
	return p.spec, nil
}

// StoredPrefix is the issue_prefix text config.json carries, legal or not:
// what an operator reads to recognize their own workspace. It is its own type
// so it cannot be handed to a minting path as a prefix — a string-typed prefix
// field refuses it without a conversion, and Mintable is the accessor that
// fits. [LAW:types-are-the-program]
type StoredPrefix string

// Stored is the text config.json carries, legal or not — never a value to mint
// under.
func (p PrefixState) Stored() StoredPrefix {
	if p.refusal != nil {
		return StoredPrefix(p.refusal.Stored)
	}
	return StoredPrefix(p.spec.Value())
}

// Derived reports whether this load minted the prefix from the repository
// name. A refused prefix was read from config, so it never is.
func (p PrefixState) Derived() bool { return p.spec.Derived() }

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

// RemoteDoltRefs is the remote's advertisement of lit's Dolt ticket data: every
// ref under refs/dolt/* — the namespace lit pushes its store into — with the
// object it points at, as `git ls-remote <remote> refs/dolt/*` prints it and
// trimmed. It is one round trip and no transfer, so it is the cheapest true
// answer to "has the remote moved": the same bytes back means the same store
// is on the remote. Empty when the remote carries no Dolt data at all.
// [LAW:one-source-of-truth] the one ls-remote of that namespace; the
// has-data question below is derived from it, never asked again.
func RemoteDoltRefs(ctx context.Context, cwd string, remote string) (string, error) {
	remoteName := normalizeRemoteName(remote)
	output, err := gitOutput(ctx, cwd, "ls-remote", remoteName, "refs/dolt/*")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// RemoteHasDoltData reports whether the remote advertises lit's Dolt ticket data
// — the refs/dolt/* namespace lit pushes its store into. This is the
// authoritative "the remote carries a backlog" signal: RemoteHasRefs is true for
// any git repo (code refs alone), so only the presence of refs/dolt/* tells
// "remote has tickets to adopt" apart from "remote is just a code repo". The
// adopt step keys its loud-vs-silent decision on this so an empty store that
// hides a real remote backlog is unrepresentable. [LAW:one-source-of-truth]
func RemoteHasDoltData(ctx context.Context, cwd string, remote string) (bool, error) {
	refs, err := RemoteDoltRefs(ctx, cwd, remote)
	if err != nil {
		return false, err
	}
	return refs != "", nil
}

// LocalRemoteHead is the remote's default branch as this repository already
// records it: refs/remotes/<remote>/HEAD, which `git clone` sets and a plain
// `git remote add` does not. It never touches the network, so any command may
// ask it; "" when the ref is unset.
func LocalRemoteHead(ctx context.Context, cwd string, remote string) string {
	remoteName := normalizeRemoteName(remote)
	symbolicRefOutput, _ := gitOutput(ctx, cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remoteName+"/HEAD")
	return strings.TrimSpace(defaultRemoteBranchFromSymbolicRef(remoteName, symbolicRefOutput))
}

// AdvertisedRemoteHead asks the remote which branch its HEAD names: one
// `git ls-remote --symref` round trip, unbounded but by ctx, so only a command
// that already talks to the remote asks it. "" with a nil error means the
// remote answered and advertises no HEAD branch; a failed ask is the error.
// [LAW:no-silent-failure]
func AdvertisedRemoteHead(ctx context.Context, cwd string, remote string) (string, error) {
	remoteName := normalizeRemoteName(remote)
	lsRemoteOutput, err := gitOutput(ctx, cwd, "ls-remote", "--symref", remoteName, "HEAD")
	if err != nil {
		return "", fmt.Errorf("git ls-remote --symref %s HEAD: %w", remoteName, err)
	}
	return strings.TrimSpace(defaultRemoteBranchFromLSRemote(lsRemoteOutput)), nil
}

// Resolve finds the workspace containing cwd, creating its config on first
// sight, with no prefix of its own to offer — the repository name supplies one.
func Resolve(cwd string) (Info, error) {
	return ResolveWithPrefix(cwd, PrefixRequest{})
}

// ResolveWithPrefix is Resolve for the one caller that may carry an explicit
// issue prefix: `lit init --prefix`. The prefix is an input to CREATING the
// workspace rather than something a command can apply afterwards — resolution
// is exactly what fails without it in a repository whose name yields none — so
// it crosses this boundary as a value, instead of being parked somewhere
// ambient for this function to read back. [LAW:no-shared-mutable-globals]
// [LAW:effects-at-boundaries] the request reaches the one write that consumes it.
func ResolveWithPrefix(cwd string, requested PrefixRequest) (Info, error) {
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
	// Resolved here as pure geometry — the path only. Minting the token that
	// lives in it is deliberately NOT done here: Resolve runs for read-only
	// commands too, and a checkout that has only ever been read must carry no
	// identity. The mint hangs off the access mode instead, in app.Open.
	privateGitDir, err := resolvePrivateGitDir(cwd)
	if err != nil {
		return Info{}, err
	}
	// [LAW:effects-at-boundaries] deriveLocation is pure geometry; store
	// creation is the one mutation, gathered here where the write is intended.
	if err := os.MkdirAll(loc.StorageDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create storage dir: %w", err)
	}
	cfg, prefix, err := loadOrCreateConfig(rootDir, loc.ConfigPath, requested)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Location:      loc,
		RootDir:       rootDir,
		WorkspaceID:   cfg.WorkspaceID,
		IssuePrefix:   prefix,
		PrivateGitDir: privateGitDir,
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
	// so a relative result must be anchored to the cwd. Anchoring it to the
	// toplevel instead would climb out of the repo and resolve a
	// subdirectory/worktree invocation to the wrong store. Anchoring to the cwd is
	// correct on every Git version (no dependency on the newer
	// --path-format=absolute flag, which would break older Git with a misleading
	// "not a git repo" error).
	// [LAW:dataflow-not-control-flow] Local geometry query; never blocks on a
	// network, so context.Background() (never cancels) is the correct value — see
	// Resolve for why the receive/sync path threads a real ctx and this does not.
	gitCommonDir, err := gitOutput(context.Background(), cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return Location{}, classifyGitError(fmt.Sprintf("git rev-parse --git-common-dir in %q", cwd), err)
	}
	gitCommonDir, err = anchorGitPath(cwd, gitCommonDir)
	if err != nil {
		return Location{}, err
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

// anchorGitPath makes a path git printed absolute. Every `rev-parse` query that
// answers with a path answers RELATIVE TO THE INVOCATION CWD (e.g. "../.git" from
// a subdirectory, ".git/worktrees/feature" from a linked worktree), so the cwd
// git was run in is the only correct anchor — anchoring to the repository
// toplevel instead climbs out of the repo and resolves a subdirectory
// invocation to the wrong store. Anchoring by hand rather than asking git for
// absolute paths keeps this working on every git version: the
// --path-format=absolute flag that would do it is recent, and older git rejects
// it with a misleading "not a git repository".
//
// [LAW:single-enforcer] Both path-answering queries — deriveLocation's
// --git-common-dir and resolvePrivateGitDir's --git-dir — anchor through here,
// so the two cannot drift into resolving the same git output differently.
func anchorGitPath(cwd string, gitPath string) (string, error) {
	if filepath.IsAbs(gitPath) {
		return filepath.Clean(gitPath), nil
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve absolute cwd: %w", err)
	}
	return filepath.Clean(filepath.Join(absCwd, gitPath)), nil
}

// resolvePrivateGitDir locates the checkout's own git directory — the per-worktree
// private area git keeps HEAD and the index in. It is the deliberate counterpart
// to deriveLocation's --git-common-dir query, and the difference between the two
// is what lets one repository hold one shared backlog and many distinct
// identities: --git-common-dir answers with the SAME directory from every
// worktree, while --git-dir answers with a DIFFERENT one (".git" in the primary
// clone, ".git/worktrees/<name>" in a linked worktree) and that directory is
// removed by `git worktree remove`. [LAW:one-source-of-truth] Git owns the
// question of which directories belong to which checkout; lit reads the answer
// and keeps no worktree registry of its own.
//
// Unlike the common dir this is NOT canonicalized through EvalSymlinks. That
// canonicalization exists to collapse two spellings of one store path, because
// the dolt driver caches engines per path string and serves a second spelling a
// read-only handle. Nothing here is keyed by path: the identity is the token
// inside the file, and any spelling that opens the file yields the same token.
func resolvePrivateGitDir(cwd string) (string, error) {
	// [LAW:dataflow-not-control-flow] Local geometry query that cannot block on a
	// network, so context.Background() — "never cancels" — is the honest value,
	// matching every other rev-parse in this file. See Resolve for the full note.
	privateGitDir, err := gitOutput(context.Background(), cwd, "rev-parse", "--git-dir")
	if err != nil {
		return "", classifyGitError(fmt.Sprintf("git rev-parse --git-dir in %q", cwd), err)
	}
	return anchorGitPath(cwd, privateGitDir)
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
// (local rev-parse geometry) passes context.Background(), which never cancels.
// [LAW:dataflow-not-control-flow]
// [LAW:no-ambient-temporal-coupling] the subprocess lifecycle is owned by ctx, not
// left to outlive a cancelled command until a grace-timer hard-exit reaps it.
func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", gitFailure(ctx, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitFailure names why a git call failed. A git that ctx killed exits with
// "signal: killed", so the ctx error is put in the chain as the cause; a git
// that failed on its own said why on stderr, which Output captured and the bare
// exit status drops. The *exec.ExitError stays in the chain for
// classifyGitError. [LAW:no-silent-failure]
func gitFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			return fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return err
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

// resolveIssuePrefix is the single enforcer of the prefix rule. A workspace's
// prefix has three possible sources and this is the one place that ranks them:
// the value config.json already carries wins, an explicit request supplies one
// for a workspace that carries none, and derivation from the repository name is
// the last resort. A stored value the rules refuse becomes the refused arm of
// PrefixState — never a silent fallback to derivation, and never a failure to
// resolve, since only minting needs a legal prefix.
//
// A request that CONTRADICTS a stored value is refused rather than applied,
// whether the stored value is legal or not. Rewriting the prefix of a workspace
// that already carries one is `lit prefix set`'s job, which previews the change
// against the ids the store actually holds before `--apply` writes it;
// honouring the request here would do that consequential thing silently, from a
// command whose whole contract is to be safe to re-run, and before the store
// that could tell a repair from a rewrite is even open.
// [LAW:single-enforcer] [LAW:no-silent-failure]
func resolveIssuePrefix(rootDir string, configPath string, configured string, requested PrefixRequest) (PrefixState, error) {
	if strings.TrimSpace(configured) == "" {
		// The request is consulted BEFORE derivation, not bolted onto one of its
		// failure exits, so every way the repository name can come up short is
		// answered by the same flag. [LAW:dataflow-not-control-flow]
		if requested.present {
			return LegalPrefixState(requested.spec), nil
		}
		derived, err := deriveIssuePrefix(rootDir)
		if err != nil {
			return PrefixState{}, err
		}
		return LegalPrefixState(PrefixSpec{value: derived, derived: true}), nil
	}
	state := parseStoredPrefix(configPath, configured)
	if requested.present && StoredPrefix(requested.spec.Value()) != state.Stored() {
		return PrefixState{}, fmt.Errorf(
			"%w: this workspace already carries %q, so --prefix %s cannot be honoured here; run `lit prefix set %s` to change the prefix of a workspace that already has one (it previews the change; `--apply` writes it)",
			ErrIssuePrefixRefused, state.Stored(), requested.spec.Value(), requested.spec.Value(),
		)
	}
	return state, nil
}

// parseStoredPrefix reads the prefix config.json carries into the state it puts
// the workspace in. A refusal is kept typed rather than flattened into a string
// so the CLI can route it to a remediation naming `lit prefix set`; sharing its
// siblings' reason would print "adjust the command to satisfy it" over a
// command that was never the problem. [LAW:one-type-per-behavior]
func parseStoredPrefix(configPath string, configured string) PrefixState {
	spec, err := ConfiguredPrefix(configured)
	if err != nil {
		return PrefixState{refusal: &StoredPrefixError{ConfigPath: configPath, Stored: configured, Err: err}}
	}
	return LegalPrefixState(spec)
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

func loadOrCreateConfig(rootDir string, path string, requested PrefixRequest) (cfg Config, prefix PrefixState, err error) {
	err = withConfigLock(path, func() error {
		cfg, prefix, err = settleConfig(rootDir, path, requested)
		return err
	})
	return cfg, prefix, err
}

// settleConfig reads config.json, decides what it should hold, and writes that
// back. The caller holds the config lock across all three.
func settleConfig(rootDir string, path string, requested PrefixRequest) (Config, PrefixState, error) {
	cfg, err := ReadConfig(path)
	if err == nil {
		prefix, err := resolveIssuePrefix(rootDir, path, cfg.IssuePrefix, requested)
		if err != nil {
			return Config{}, PrefixState{}, err
		}
		// [LAW:one-source-of-truth] config.json holds the resolved value, so a
		// derivation or a normalization change is persisted the moment it happens.
		// A refused value's Stored text is what config.json already carries, so
		// it is left exactly as found: repairing it is `lit prefix set`'s
		// decision, never a side effect of resolving.
		if string(prefix.Stored()) != cfg.IssuePrefix {
			cfg.IssuePrefix = string(prefix.Stored())
			cfg, err = writeConfig(path, cfg)
			if err != nil {
				return Config{}, PrefixState{}, err
			}
		}
		return cfg, prefix, nil
	}
	// [LAW:no-silent-failure] A missing config is the ordinary "not yet
	// initialized" case that this create path handles; a parse error, an empty
	// workspace_id, or any other read failure is already fully described by
	// ReadConfig and must surface as-is rather than be mistaken for "create one".
	if !errors.Is(err, os.ErrNotExist) {
		return Config{}, PrefixState{}, err
	}
	prefix, err := resolveIssuePrefix(rootDir, path, "", requested)
	if err != nil {
		return Config{}, PrefixState{}, err
	}
	cfg = Config{
		WorkspaceID: uuid.NewString(),
		IssuePrefix: string(prefix.Stored()),
		CreatedAt:   time.Now().UTC(),
		Version:     1,
	}
	cfg, err = writeConfig(path, cfg)
	if err != nil {
		return Config{}, PrefixState{}, err
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
// mutate runs holding the config lock, which is a leaf, so it must compute and
// return without taking any lock of its own.
//
// [LAW:single-enforcer] All in-place edits to the workspace config go through
// this single read-modify-write boundary so partial writes can't desync
// callers from on-disk state.
func UpdateConfig(path string, mutate func(Config) (Config, error)) (updated Config, err error) {
	err = withConfigLock(path, func() error {
		updated, err = updateConfigLocked(path, mutate)
		return err
	})
	return updated, err
}

func updateConfigLocked(path string, mutate func(Config) (Config, error)) (Config, error) {
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

// deriveIssuePrefix proposes an issue prefix from the repository's directory
// name — the convenience default for a caller who supplied none, never the only
// way in. Normalization can come up short two ways, with nothing legal
// surviving it (a directory named "___") or with too little (one named "ab"),
// and both ask the caller for the identical act, so they are one refusal and
// one sentence rather than two the reader has to tell apart.
// [LAW:one-type-per-behavior]
//
// The message names `lit init --prefix` rather than the bare flag because every
// workspace command reaches here, not just init: a repository whose name yields
// no prefix fails `lit ls` too, and running init with a prefix is the act that
// resolves it for all of them. [LAW:no-silent-failure] the remediation an agent
// acts on has to name something that actually works.
func deriveIssuePrefix(rootDir string) (string, error) {
	name := filepath.Base(rootDir)
	base := issueid.NormalizeSlug(name)
	// Dash-separated parts first and the whole name last, so a repository named
	// "links-issue-tracker" prefixes its issues "links" and not "links-issue-".
	// A part that normalizes to nothing is refused by the same boundary that
	// refuses a short one, so there is no separate emptiness test here.
	for _, candidate := range append(strings.Split(base, "-"), base) {
		if prefix, err := issueid.NormalizeConfiguredPrefix(candidate); err == nil {
			return prefix, nil
		}
	}
	return "", fmt.Errorf(
		"%w: repository name %q yields none (a prefix needs %d or more characters once punctuation is normalized away); run `lit init --prefix <prefix>` to set one explicitly",
		ErrIssuePrefixRefused, name, issueid.PrefixMinLength,
	)
}
