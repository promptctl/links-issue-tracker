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
func resolveSyncBranch(ctx context.Context, ws workspace.Info, remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if branch := localSyncBranch(ctx, ws.RootDir, remote); branch != "" {
		return branch, nil
	}
	branch, err := workspace.AdvertisedRemoteHead(ctx, ws.RootDir, remote)
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
	if err := writeMarkerAtomic(ws, advertisedSyncBranchPath(ws, remote), []byte(branch+"\n")); err != nil {
		fmt.Fprintf(os.Stderr, "lit: sync branch record not written: %v\n", err)
	}
	return branch, nil
}

// knownSyncBranch is the sync branch for a command that must not wait on the
// network: resolveSyncBranch's local chain, then what the remote last told a
// sync verb on this machine. An unknown branch is an error naming how to teach
// it, never a guess.
func knownSyncBranch(ctx context.Context, ws workspace.Info, remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	recorded, err := os.ReadFile(advertisedSyncBranchPath(ws, remote))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read sync branch record for remote %q: %w", remote, err)
	}
	branch := precedence.First(localSyncBranch(ctx, ws.RootDir, remote), strings.TrimSpace(string(recorded)))
	if branch == "" {
		return "", fmt.Errorf(
			"default branch of remote %q is not known on this machine: refs/remotes/%s/HEAD is unset and no sync here has asked the remote yet; 'lit sync pull' asks it",
			remote, remote,
		)
	}
	return branch, nil
}

// advertisedSyncBranchPath is where resolveSyncBranch records the branch a
// remote advertised, one file per remote. The name is path-escaped because a
// git remote name may contain '/'.
func advertisedSyncBranchPath(ws workspace.Info, remote string) string {
	return filepath.Join(ws.StorageDir, "sync-branch."+url.PathEscape(remote))
}
