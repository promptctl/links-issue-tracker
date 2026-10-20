package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The sync branch is the remote's default branch, and two kinds of command want
// it. A sync verb already talks to the remote, so when git does not know the
// answer it asks. A read command never waits on the network (links-scale-om3r),
// so it takes what this machine already knows — which includes what the last
// sync verb here was told, so both report the same branch.

// localSyncBranch is the sync branch from the answers that cost no network: the
// debug override, then git's refs/remotes/<remote>/HEAD.
// [LAW:one-source-of-truth] the one local chain both resolvers below start from.
func localSyncBranch(ctx context.Context, rootDir string, remote string) string {
	return precedence.First(
		strings.TrimSpace(os.Getenv(debugSyncBranchEnvVar)),
		workspace.LocalRemoteHead(ctx, rootDir, remote),
	)
}

// resolveSyncBranch is the branch push, pull, receive and init sync against.
// When git does not know the remote's default branch it asks the remote, and
// records the answer for knownSyncBranch.
// [LAW:single-enforcer] Sync branch selection is centralized so pull/push/hooks
// consume one canonical branch decision.
func resolveSyncBranch(ctx context.Context, ws workspace.Info, remote storage.RemoteName) (string, error) {
	if branch := localSyncBranch(ctx, ws.RootDir, remote.String()); branch != "" {
		return branch, nil
	}
	branch, err := workspace.AdvertisedRemoteHead(ctx, ws.RootDir, remote.String())
	if err != nil {
		return "", fmt.Errorf("resolve sync branch for remote %q: %w", remote, err)
	}
	if branch == "" {
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			remote,
			debugSyncBranchEnvVar,
		)
	}
	// The branch is resolved whether or not the record lands; a lost record only
	// costs read commands their sync banner until the next sync verb writes it.
	if err := writeMarkerAtomic(advertisedSyncBranchPath(ws, remote), []byte(branch+"\n")); err != nil {
		fmt.Fprintf(os.Stderr, "lit: sync branch record not written: %v\n", err)
	}
	return branch, nil
}

// knownSyncBranch is the sync branch for a command that must not wait on the
// network: resolveSyncBranch's local chain, then what the remote last told a
// sync verb on this machine. An unknown branch is an error, never a guess; an
// unreadable record is that error's cause, and matters only when nothing
// ahead of the record answered.
func knownSyncBranch(ctx context.Context, ws workspace.Info, remote storage.RemoteName) (string, error) {
	recorded, readErr := os.ReadFile(advertisedSyncBranchPath(ws, remote))
	branch := precedence.First(localSyncBranch(ctx, ws.RootDir, remote.String()), strings.TrimSpace(string(recorded)))
	if branch != "" {
		return branch, nil
	}
	notKnown := syncBranchNotKnownError{remote: remote}
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		// The text says what is not known, but the error is the unreadable
		// record's alone: a record that would name the branch is a fault,
		// not the absence a caller skips on.
		return "", fmt.Errorf("%v; the record of what the remote last said is unreadable: %w", notKnown, readErr)
	}
	return "", notKnown
}

// syncBranchNotKnownError is knownSyncBranch finding nothing on this machine
// that names the remote's default branch. No freshness read can report on the
// remote then, which is what a caller acting only for those reads keys on.
type syncBranchNotKnownError struct{ remote storage.RemoteName }

func (e syncBranchNotKnownError) Error() string {
	return fmt.Sprintf(
		"default branch of remote %q is not known on this machine: git names none in refs/remotes/%s/HEAD and no sync here has learned it from the remote; the next sync that reaches the remote learns it",
		e.remote, e.remote,
	)
}

// freshnessSyncTarget is the remote and branch the freshness reads report on
// — doctor and every read command's banner — resolved as `lit sync` resolves
// its remote, and with no network: the branch is knownSyncBranch's. ok false
// is a workspace with no sync remote. [LAW:one-source-of-truth] the one
// reading of what freshness is about.
func freshnessSyncTarget(ctx context.Context, ws workspace.Info, gitRemotes []workspace.GitRemote) (remote storage.RemoteName, branch string, ok bool, err error) {
	remote, ok, err = resolveSyncRemote("", workspace.UpstreamRemote(ctx, ws.RootDir), gitRemotes)
	if err != nil || !ok {
		return remote, "", ok, err
	}
	branch, err = knownSyncBranch(ctx, ws, remote)
	return remote, branch, true, err
}

// advertisedSyncBranchPath is where resolveSyncBranch records the branch a
// remote advertised, one file per remote: a RemoteName holds no "/", so the
// record is a file directly in StorageDir. The name is still path-escaped
// because a RemoteName admits characters the escape rewrites (";" among
// them), and every earlier lit wrote the record under the escaped name.
func advertisedSyncBranchPath(ws workspace.Info, remote storage.RemoteName) string {
	return filepath.Join(ws.StorageDir, "sync-branch."+url.PathEscape(remote.String()))
}
