package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
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
	// Remotes is this checkout's git remote configuration, asked of git once
	// per invocation; see Remotes for who owns it.
	Remotes *Remotes
	// Project is the project config layer every config read of this unit of
	// work takes: the checkout's, or the one a detached worker was handed.
	// [LAW:one-source-of-truth] no config read derives it from RootDir again.
	Project config.ProjectLayer
}

// PrimaryClone says this checkout is the repository's primary clone rather than
// a linked worktree: its own git directory IS the common one. `lit next` reads
// it to say where a pick is started, because the primary clone is where every
// session starts. Read off the two directories rather than stored beside them,
// so it cannot disagree with them; both are canonical, so the comparison is of
// physical paths. [LAW:one-source-of-truth]
func (i Info) PrimaryClone() bool {
	return i.PrivateGitDir == i.GitCommonDir
}

// Location is the on-disk geometry of a lit store — every path derived from a
// git repository's git-common-dir, and nothing that requires reading or writing
// the store. It is what a filesystem scan can know about a store WITHOUT opening
// or creating it: Resolve layers store creation, config, and identity on top;
// Discover uses it to detect a store in place. [LAW:one-source-of-truth] The
// paths are minted only by Locate, so a discovered Location and the
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

// Remotes is a checkout's git remote configuration as one invocation reads it:
// the remotes the repository has, and the one the current branch tracks. Each is asked of
// git at most once, on first use, and every later read in the invocation is
// that answer — a command whose drift banner, sync-remote choice and automatic
// receive all need the remotes spawns `git remote -v` once, not once each.
//
// [LAW:no-shared-mutable-globals] Resolve mints one for the Info it returns and
// that Info is its owner: it lives as long as the unit of work that resolved
// it — one command — and detached workers resolve their own; a worker that
// runs several cycles gives each cycle its own (Reread). Holding git's answer
// for that long is sound because lit never writes git's remote or branch
// configuration; a remote added by hand mid-command is seen by the next one.
// NewRemotes is the only way to build one.
type Remotes struct {
	env  environ.Env
	repo Repository
	root CheckoutRoot
	// askUpstream is where the upstream answer comes from, kept as a value so
	// a Reread asks the same source: a checkout's own branch, or the answer a
	// detached worker was handed.
	askUpstream func() (string, error)
	listing     func() (remoteListing, error)
	upstream    func() (string, error)
	recorded    sync.Once
}

// NewRemotes reads, lazily, the remote configuration the checkout at root
// sees: the remote list is the repository's, asked of repo, and the upstream
// is the checkout's current branch's.
//
// [LAW:dataflow-not-control-flow] Both queries read local config and cannot
// hang on a network, so context.Background() — "never cancels" — is the honest
// value, as for every geometry query in this file. A reader's ctx names the
// event the listing's facts land on; it never cancels the read, which every
// later reader in the command shares.
func NewRemotes(env environ.Env, repo Repository, root CheckoutRoot) *Remotes {
	return remotesAsking(env, repo, root, func() (string, error) {
		return upstreamRemote(context.Background(), env, root.path)
	})
}

// HandedRemotes is the remote configuration a detached worker reads: the
// repository's remote list, with relative path URLs joined to the root its
// launcher handed it, and an upstream answered by the sync remote the
// launcher's precedence chose. The upstream's one reader is that precedence
// (chooseSyncRemote), so handing it the launcher's choice picks the remote the
// launcher picked, and no git runs in a checkout the worker cannot know still
// exists. [LAW:no-ambient-temporal-coupling]
func HandedRemotes(env environ.Env, repo Repository, root CheckoutRoot, syncRemote string) *Remotes {
	return remotesAsking(env, repo, root, func() (string, error) { return syncRemote, nil })
}

func remotesAsking(env environ.Env, repo Repository, root CheckoutRoot, askUpstream func() (string, error)) *Remotes {
	return &Remotes{
		env:         env,
		repo:        repo,
		root:        root,
		askUpstream: askUpstream,
		listing: sync.OnceValues(func() (remoteListing, error) {
			return gitRemotes(context.Background(), env, repo, root)
		}),
		upstream: sync.OnceValues(askUpstream),
	}
}

// Reread is a reader that asks the repository for its remote list afresh, and
// asks the upstream of the same source this one did.
func (r *Remotes) Reread() *Remotes { return remotesAsking(r.env, r.repo, r.root, r.askUpstream) }

// Root is the checkout root relative path URLs are joined to.
func (r *Remotes) Root() CheckoutRoot { return r.root }

// List is the checkout's remotes that have a fetch URL, sorted by name. git
// cannot fetch a remote with none (a pushurl-only remote, say), so it is left
// out, and named on the event as remotes.without_url.
func (r *Remotes) List(ctx context.Context) ([]GitRemote, error) {
	listing, err := r.read(ctx)
	return listing.remotes, err
}

// read is the listing, whose facts land on the event of the first reader that
// receives it. [LAW:nothing-unseen] the read every remote consumer passes
// through records them, so no consumer has to remember to.
func (r *Remotes) read(ctx context.Context) (remoteListing, error) {
	listing, err := r.listing()
	if err != nil {
		return remoteListing{}, err
	}
	r.recorded.Do(func() { listing.record(ctx) })
	return listing, nil
}

// Upstream is the name of the remote the current branch tracks, whole as git
// records it ("." for a local branch, "" for none or a detached HEAD). It is
// never cut from the upstream ref: in a/b/main the remote is a/b. A git that
// fails to answer is an error, never "no upstream": read as none, it would
// hand the sync-remote choice to whichever remote is the only one.
// [LAW:no-silent-failure]
func (r *Remotes) Upstream() (string, error) { return r.upstream() }

func upstreamRemote(ctx context.Context, env environ.Env, cwd string) (string, error) {
	remote, err := gitOutput(ctx, env, cwd, "for-each-ref", "--format=%(if)%(HEAD)%(then)%(upstream:remotename)%(end)", "refs/heads/")
	if err != nil {
		return "", fmt.Errorf("git for-each-ref refs/heads/ (the current branch's upstream) in %q: %w", cwd, err)
	}
	return remote, nil
}

// Resolve finds the workspace containing cwd, creating its config on first
// sight, with no prefix of its own to offer — the repository name supplies one.
func Resolve(env environ.Env, cwd string) (Info, error) {
	return ResolveWithPrefix(env, cwd, PrefixRequest{})
}

// ResolveWithPrefix is Resolve for the one caller that may carry an explicit
// issue prefix: `lit init --prefix`. The prefix is an input to CREATING the
// workspace rather than something a command can apply afterwards — resolution
// is exactly what fails without it in a repository whose name yields none — so
// it crosses this boundary as a value, instead of being parked somewhere
// ambient for this function to read back. [LAW:no-shared-mutable-globals]
// [LAW:effects-at-boundaries] the request reaches the one write that consumes it.
func ResolveWithPrefix(env environ.Env, cwd string, requested PrefixRequest) (Info, error) {
	geo, err := askGeometry(env, cwd)
	if err != nil {
		return Info{}, err
	}
	loc, err := locationOf(geo)
	if err != nil {
		return Info{}, err
	}
	// Resolved here as pure geometry — the path only. Minting the token that
	// lives in it is deliberately NOT done here: Resolve runs for read-only
	// commands too, and a checkout that has only ever been read must carry no
	// identity. The mint hangs off the access mode instead, in app.Open.
	privateGitDir, err := canonicalGitDir(geo.gitDir)
	if err != nil {
		return Info{}, err
	}
	// [LAW:effects-at-boundaries] Locate is pure geometry; store
	// creation is the one mutation, gathered here where the write is intended.
	if err := os.MkdirAll(loc.StorageDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create storage dir: %w", err)
	}
	cfg, prefix, err := loadOrCreateConfig(geo.root, loc.ConfigPath, requested)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Location:      loc,
		RootDir:       geo.root,
		WorkspaceID:   cfg.WorkspaceID,
		IssuePrefix:   prefix,
		PrivateGitDir: privateGitDir,
		// git prints --show-toplevel absolute with every symlink resolved, so its
		// answer is a CheckoutRoot as it stands.
		Remotes: NewRemotes(env, loc.Repository(), CheckoutRoot{path: geo.root}),
		Project: config.CheckoutProject(env, pathspec.New(geo.root)),
	}, nil
}

// Handed is the workspace a detached worker runs against: the store at loc,
// and the values the command that launched it handed it from its own
// checkout — the root relative path URLs are joined to, the sync remote its
// precedence chose, and its project config layer. Nothing is asked of a
// checkout, which may be gone by now, and nothing is created: the store
// exists, because the launcher ran against it. [LAW:effects-at-boundaries]
func Handed(env environ.Env, loc Location, root CheckoutRoot, syncRemote string, project config.ProjectLayer) (Info, error) {
	cfg, err := ReadConfig(loc.ConfigPath)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Location:    loc,
		RootDir:     root.path,
		WorkspaceID: cfg.WorkspaceID,
		IssuePrefix: parseStoredPrefix(loc.ConfigPath, cfg.IssuePrefix),
		Remotes:     HandedRemotes(env, loc.Repository(), root, syncRemote),
		Project:     project,
	}, nil
}

// Locate computes the lit store geometry for the git repository
// containing cwd, purely — the only effects are the git queries that read
// repository geometry and the symlink canonicalization that reads the
// filesystem; nothing is created or written. [LAW:effects-at-boundaries]
// [LAW:single-enforcer] This is the one place cwd becomes a set of store paths,
// so Resolve (which then creates the store) and Discover (which then only
// detects it) can never derive different paths for the same repository.
func Locate(env environ.Env, cwd string) (Location, error) {
	geo, err := askGeometry(env, cwd)
	if err != nil {
		return Location{}, err
	}
	return locationOf(geo)
}

// geometry is where git says the checkout containing a directory lives: the
// root of its working tree, the repository's common git dir, and the
// checkout's own git dir, all absolute.
type geometry struct {
	root      string
	commonDir string
	gitDir    string
}

// geometryQuery is the one git command that answers geometry, its answers in
// the order geometry's fields are filled; its words after "git" are the argv.
const geometryQuery = "git rev-parse --show-toplevel --git-common-dir --git-dir"

// askGeometry asks git, in one subprocess, for every part of a checkout's
// geometry. [LAW:single-enforcer] It is the one place lit asks git where a
// checkout is, so Resolve and Locate — and through Locate, Discover — read one
// answer to one question.
//
// [LAW:dataflow-not-control-flow] A local rev-parse cannot hang on a network,
// so cancellation buys nothing here; context.Background() is the honest "never
// cancels" value, and it keeps Resolve's many callers free of a ctx they would
// only forward to a subprocess that never blocks. Cancellation is threaded only
// through the receive/sync path's network git calls, where a wedge is reachable.
func askGeometry(env environ.Env, cwd string) (geometry, error) {
	out, err := gitStdout(context.Background(), env, cwd, strings.Fields(geometryQuery)[1:]...)
	if err != nil {
		return geometry{}, classifyGitError(fmt.Sprintf("%s in %q", geometryQuery, cwd), err)
	}
	return parseGeometry(cwd, out)
}

// parseGeometry reads rev-parse's three answers. git prints them one per line,
// unquoted — `--sq` does not reach these options and rev-parse has no `-z` — and
// a newline is a legal byte in a path, so a checkout at "wt\nprunable" prints
// more lines than answers. The lines are therefore not split at a guessed
// boundary: every way of cutting them into the three answers is tried, and the
// answer is the one cut whose paths all name a directory that exists. Three
// lines admit exactly one cut. [LAW:parse-dont-validate]
//
// The git dirs are printed relative to the invocation cwd (e.g. "../.git" from
// a subdirectory), so a relative answer is anchored to the cwd; anchoring to
// the toplevel instead would climb out of the repo and resolve a subdirectory
// or worktree invocation to the wrong store.
func parseGeometry(cwd string, out string) (geometry, error) {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	answer := func(from, to int) string { return anchorGitPath(cwd, strings.Join(lines[from:to], "\n")) }
	var cuts []geometry
	for i := 1; i < len(lines); i++ {
		for j := i + 1; j < len(lines); j++ {
			cut := geometry{root: answer(0, i), commonDir: answer(i, j), gitDir: answer(j, len(lines))}
			if isDir(cut.root) && isDir(cut.commonDir) && isDir(cut.gitDir) {
				cuts = append(cuts, cut)
			}
		}
	}
	switch len(cuts) {
	case 1:
		return cuts[0], nil
	case 0:
		return geometry{}, fmt.Errorf("%s in %q printed %q, which names no working-tree root, common dir and git dir that all exist", geometryQuery, cwd, out)
	default:
		return geometry{}, fmt.Errorf("%s in %q printed %q, which reads as %d different sets of existing directories; lit cannot tell which one git meant", geometryQuery, cwd, out, len(cuts))
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// locationOf is the store geometry of the repository geo names.
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
func locationOf(geo geometry) (Location, error) {
	canonicalCommonDir, err := filepath.EvalSymlinks(geo.commonDir)
	if err != nil {
		return Location{}, fmt.Errorf("canonicalize git-common-dir %q: %w", geo.commonDir, err)
	}
	return LocationFromStorageDir(filepath.Join(filepath.Clean(canonicalCommonDir), "links")), nil
}

// anchorGitPath makes a path git wrote absolute against the directory it is
// relative to. rev-parse answers relative to the invocation cwd (e.g. "../.git"
// from a subdirectory) and a gitfile relative to the working tree it sits in, so
// the caller names that base. Anchoring by hand rather than asking git for
// absolute paths keeps this working on every git version: the
// --path-format=absolute flag that would do it is recent, and older git rejects
// it with a misleading "not a git repository".
//
// [LAW:single-enforcer] Every path lit reads out of git — the geometry's two
// answers and a gitfile's target — anchors through here, so none of them can
// drift into resolving a relative path differently.
//
// base is absolute: it is the directory gitOutput already ran git in, which
// gitOutput refuses unless absolute, or a working-tree root git printed.
func anchorGitPath(base string, gitPath string) string {
	if filepath.IsAbs(gitPath) {
		return filepath.Clean(gitPath)
	}
	return filepath.Clean(filepath.Join(base, gitPath))
}

// checkoutGitDir is the checkout's own git directory — the per-worktree
// private area git keeps HEAD and the index in, which `git worktree remove`
// removes with the worktree. It is the counterpart to the common dir, and the
// difference between the two is what lets one repository hold one shared
// backlog and many distinct identities: the common dir is the SAME from every
// worktree, while this is a DIFFERENT one per checkout.
//
// It is read from the `.git` entry git keeps at every working-tree root, in the
// two forms gitrepository-layout(5) defines: a directory is the git dir itself
// (a primary clone), and a regular file is a gitfile whose `gitdir: ` line names
// it (a linked worktree, a submodule, a --separate-git-dir clone). A `.git` at
// the root is also how Discover recognises a working tree. [LAW:one-source-of-truth]
// Git owns which directory belongs to which checkout; lit reads git's own record
// of it and keeps no worktree registry.
//
// It is how LiveCheckouts finds every linked worktree's identity without a git
// subprocess per worktree, and it reads only what git wrote at each root: a
// GIT_DIR a git hook exported answers rev-parse in every directory, and would
// lend the hook's checkout's directory to every checkout asked about. Resolve
// takes the checkout it runs in from git's own --git-dir answer instead, which
// is the one that honours GIT_DIR and GIT_WORK_TREE for that checkout; in every
// layout git writes, the two name the same directory
// (TestResolveAndTheEnumerationAgreeOnEveryCheckoutsIdentity).
//
// It is canonicalized through EvalSymlinks, as the common dir is, so the two
// compare as physical paths: on macOS a checkout under /var is the same
// directory as /private/var, and Info.PrimaryClone would otherwise call the
// primary clone a linked worktree.
func checkoutGitDir(root string) (string, error) {
	entry := filepath.Join(root, ".git")
	info, err := os.Stat(entry)
	if err != nil {
		return "", fmt.Errorf("read the .git entry of working tree %q: %w", root, err)
	}
	gitDir := entry
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		gitDir, err = gitfileTarget(root, entry)
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("the .git entry of working tree %q is neither a directory nor a gitfile", root)
	}
	return canonicalGitDir(gitDir)
}

// canonicalGitDir is a checkout's git dir as the physical path both Resolve
// and LiveCheckouts compare it by.
func canonicalGitDir(gitDir string) (string, error) {
	canonical, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", fmt.Errorf("canonicalize git dir %q: %w", gitDir, err)
	}
	return filepath.Clean(canonical), nil
}

// gitfilePrefix opens the one line a gitfile holds.
const gitfilePrefix = "gitdir: "

// gitfileTarget reads the git dir a gitfile names, as git reads it: the file is
// `gitdir: <path>`, the trailing line break is not part of the path, and a
// relative path is relative to the directory the gitfile is in.
func gitfileTarget(root string, gitfile string) (string, error) {
	content, err := os.ReadFile(gitfile)
	if err != nil {
		return "", fmt.Errorf("read gitfile %q: %w", gitfile, err)
	}
	target, ok := strings.CutPrefix(strings.TrimRight(string(content), "\r\n"), gitfilePrefix)
	if !ok || target == "" {
		return "", fmt.Errorf("gitfile %q does not name a git dir: it holds %q, not a %q line", gitfile, content, gitfilePrefix+"<path>")
	}
	return anchorGitPath(root, target), nil
}

// LocationFromStorageDir mints the store geometry rooted at an already-resolved
// StorageDir — the "dolt", "config.json", and links-repo suffixes and nothing
// else. [LAW:single-enforcer] These suffixes live here only; Locate
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

// gitOutput runs one git subprocess and returns its trimmed stdout. It starts
// git through gitcmd, so a cancelled ctx ends git and everything git started: a
// network-wedged call (ls-remote/fetch to an unreachable remote) abandons on
// cancellation, its transport included, instead of outliving it. Cancellation
// is a value crossing this one seam, not a second code path — a caller whose
// git call cannot hang on the network (local rev-parse geometry) passes
// context.Background(), which never cancels. [LAW:dataflow-not-control-flow]
// [LAW:no-ambient-temporal-coupling] the subprocess lifecycle is owned by ctx, not
// left to outlive a cancelled command until a grace-timer hard-exit reaps it.
//
// cwd must be absolute. This is the one place a workspace function's cwd
// becomes a directory something runs in, and git run in an empty or relative
// one would answer for the process's working directory instead of the
// caller's. [LAW:single-enforcer] [LAW:effects-at-boundaries]
func gitOutput(ctx context.Context, env environ.Env, cwd string, args ...string) (string, error) {
	out, err := gitStdout(ctx, env, cwd, args...)
	return strings.TrimSpace(out), err
}

// gitStdout is gitOutput's stdout byte for byte, for an answer whose edges
// are data: a path can begin or end with whitespace, which trimming would cut.
func gitStdout(ctx context.Context, env environ.Env, cwd string, args ...string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("run git in %q: the directory is not absolute", cwd)
	}
	out, err := env.Git(ctx, cwd, nil, args...).Output()
	if err != nil {
		return "", gitFailure(err)
	}
	return string(out), nil
}

// gitFailure names why a git call failed. A git that failed on its own said
// why on stderr, which Output captured and the bare exit status drops. A git
// its ctx ended is a *gitcmd.Cancelled, which names the cause itself. The
// *exec.ExitError stays in the chain for classifyGitError.
// [LAW:no-silent-failure]
func gitFailure(err error) error {
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
// [LAW:single-enforcer] The git repo gate — askGeometry, which Resolve and
// Locate share — routes through here, so no caller classifies the same git
// failure a second way.
func classifyGitError(context string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == gitFatalExitCode {
		return ErrNotGitRepo
	}
	return fmt.Errorf("%s: %w", context, err)
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
// here; loadOrCreateConfig and UpdateConfig reuse it so no two paths can
// validate a config two different ways. The read error is wrapped preserving os.ErrNotExist via %w, so
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
	// [LAW:single-enforcer] Every config writer flows through here, and the
	// atomic replace means a crash cannot leave config.json empty or partially
	// written. Durable: it holds the workspace's identity, which nothing
	// recomputes.
	if err := atomicfile.WriteFile(path, payload, 0o644, atomicfile.Durable); err != nil {
		return Config{}, fmt.Errorf("write workspace config: %w", err)
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
	cfg, err := ReadConfig(path)
	if err != nil {
		return Config{}, err
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
