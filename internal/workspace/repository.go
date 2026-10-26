package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Repository is a git repository as lit asks git about it: its common git
// dir, which every checkout of the repository shares and none of them is.
// Location.Repository is the only way to name one, so its dir is absolute and
// canonical.
//
// Every question here runs as `git --git-dir=<dir> --work-tree=<dir>` in that
// dir, never by discovery from a checkout. [LAW:single-enforcer] that is what
// makes the answer the same from every checkout and from none:
// safe.bareRepository=explicit refuses discovery, not an explicit git dir, and
// the explicit work tree keeps git from entering a core.worktree whose checkout
// is gone (a submodule's, say).
type Repository struct {
	gitDir string
}

// Repository is the repository whose store this is.
func (l Location) Repository() Repository { return Repository{gitDir: l.GitCommonDir} }

// localRepoEnv is git's local_repo_env (`git rev-parse --local-env-vars`)
// without the GIT_CONFIG* entries, which carry settings rather than a
// repository: the variables git itself clears when it moves into another
// repository. Inherited from a git hook they would override the explicit git
// dir — an inherited GIT_COMMON_DIR does — so a question is asked without
// them. TestLocalRepoEnvIsGits holds the list to git's.
var localRepoEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_INDEX_FILE",
	"GIT_GRAFT_FILE", "GIT_SHALLOW_FILE", "GIT_PREFIX",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE",
}

// git runs one question of the repository in dir. Only a path URL read the way
// a checkout reads it needs a dir other than the git dir itself.
func (r Repository) git(ctx context.Context, env environ.Env, dir string, args ...string) (string, error) {
	return gitOutput(ctx, env.Without(localRepoEnv...), dir, append([]string{"--git-dir", r.gitDir, "--work-tree", r.gitDir}, args...)...)
}

// lsRemote is `git ls-remote` of url, by URL rather than by remote name, so
// no name-keyed config (remote.<name>.uploadpack, .proxy, .vcs) makes a
// question answer otherwise than the push or fetch it is about: Dolt's
// transfer runs git in its own cache repository, whose remote carries the URL
// and none of that config. The "--" keeps a URL that begins with "-" from being read as an
// option. lit's wrapping adds no URL, which can carry a credential, and the
// caller names the remote. git's stderr, kept as the cause, may name the
// URL; git leaves an https URL's credential out of it
// (TestAFailedRemoteQuestionKeepsTheURLsCredentialOutOfItsError).
// [LAW:one-source-of-truth]
func (r Repository) lsRemote(ctx context.Context, env environ.Env, dir, url string, options []string, patterns ...string) (string, error) {
	args := append(append(append([]string{"ls-remote"}, options...), "--", url), patterns...)
	out, err := r.git(ctx, env, dir, args...)
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %w", err)
	}
	return out, nil
}

// RemoteHasRefs reports whether the remote advertises any ref at all.
func (r Repository) RemoteHasRefs(ctx context.Context, env environ.Env, remote SyncableRemote) (bool, error) {
	out, err := r.lsRemote(ctx, env, r.gitDir, remote.URL(), nil)
	return out != "", err
}

// RemoteDoltRefs is the remote's advertisement of lit's Dolt ticket data: every
// ref under refs/dolt/* — the namespace lit pushes its store into — with the
// object it points at, as `git ls-remote <url> refs/dolt/*` prints it and
// trimmed. It is one round trip and no transfer, so it is the cheapest true
// answer to "has the remote moved": the same bytes back means the same store
// is on the remote. Empty when the remote carries no Dolt data at all.
// [LAW:one-source-of-truth] the one ls-remote of that namespace; the
// has-data question below is derived from it, never asked again.
func (r Repository) RemoteDoltRefs(ctx context.Context, env environ.Env, remote SyncableRemote) (string, error) {
	return r.lsRemote(ctx, env, r.gitDir, remote.URL(), nil, doltRefs)
}

const doltRefs = "refs/dolt/*"

// RemoteHasDoltData reports whether the remote advertises lit's Dolt ticket
// data. This is the authoritative "the remote carries a backlog" signal:
// RemoteHasRefs is true for any git repo (code refs alone), so only the
// presence of refs/dolt/* tells "remote has tickets to adopt" apart from
// "remote is just a code repo". [LAW:one-source-of-truth]
func (r Repository) RemoteHasDoltData(ctx context.Context, env environ.Env, remote SyncableRemote) (bool, error) {
	refs, err := r.RemoteDoltRefs(ctx, env, remote)
	return refs != "", err
}

// RefusedRemoteHasDoltData is RemoteHasDoltData for a remote lit refuses to
// sync with, which init still asks so a fresh store cannot fork a backlog the
// remote carries. Such a remote may have no URL lit can name, so it is asked
// by the URL as written, from the checkout root it was listed in: the one
// reading of it that exists, and git's own for that checkout.
func (r Repository) RefusedRemoteHasDoltData(ctx context.Context, env environ.Env, refused RefusedGitRemote) (bool, error) {
	refs, err := r.lsRemote(ctx, env, refused.Remote.root.path, refused.Remote.written, nil, doltRefs)
	return refs != "", err
}

// AdvertisedRemoteHead asks the remote which branch its HEAD names: one
// `git ls-remote --symref` round trip, unbounded but by ctx, so only a command
// that already talks to the remote asks it. "" with a nil error means the
// remote answered and advertises no HEAD branch; a failed ask is the error.
// [LAW:no-silent-failure]
func (r Repository) AdvertisedRemoteHead(ctx context.Context, env environ.Env, remote SyncableRemote) (string, error) {
	out, err := r.lsRemote(ctx, env, r.gitDir, remote.URL(), []string{"--symref"}, "HEAD")
	if err != nil {
		return "", err
	}
	return defaultRemoteBranchFromLSRemote(out), nil
}

// LocalRemoteHead is the remote's default branch as this repository already
// records it: refs/remotes/<remote>/HEAD, which `git clone` and
// `git remote set-head` set and a plain `git remote add` does not. It never
// touches the network, so any command may ask it.
//
// "" with a nil error is the ref unset, which `symbolic-ref --quiet` says by
// exiting 1 (a ref that exists but is not symbolic says the same). Any other
// failure is the error, never "unset": read as unset it would hand the branch
// to the remote's advertised HEAD, which may name another. The full ref is
// read rather than --short, whose answer changes when a local branch shares
// the remote-tracking name (a branch "origin/main" makes it
// "remotes/origin/main"). [LAW:no-silent-failure]
func (r Repository) LocalRemoteHead(ctx context.Context, env environ.Env, remote storage.RemoteName) (string, error) {
	ref := "refs/remotes/" + remote.String() + "/HEAD"
	target, err := r.git(ctx, env, r.gitDir, "symbolic-ref", "--quiet", ref)
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git symbolic-ref %s in %q: %w", ref, r.gitDir, err)
	}
	branch, ok := strings.CutPrefix(target, "refs/remotes/"+remote.String()+"/")
	if !ok || branch == "" {
		return "", fmt.Errorf("%s in %q names %q, which is no branch of remote %q; `git remote set-head %s --auto` sets it from the remote", ref, r.gitDir, target, remote, remote)
	}
	return branch, nil
}

// defaultRemoteBranchFromLSRemote reads the branch `ls-remote --symref <url>
// HEAD` names on its "ref: refs/heads/<branch>\tHEAD" line; "" when it prints
// none.
func defaultRemoteBranchFromLSRemote(output string) string {
	for _, line := range strings.Split(output, "\n") {
		target, ok := strings.CutSuffix(strings.TrimSpace(line), "\tHEAD")
		if branch, isBranch := strings.CutPrefix(target, "ref: refs/heads/"); ok && isBranch {
			return branch
		}
	}
	return ""
}
