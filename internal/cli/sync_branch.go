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

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The sync branch is the remote's default branch, and two kinds of command want
// it. A sync verb already talks to the remote, so when git does not know the
// answer it asks. A read command never waits on the network (links-scale-om3r),
// so it takes what this machine already knows — which includes what the last
// sync verb here was told, so both report the same branch.

// syncBranchAnswer is one answer to "which branch", named by where it came
// from: the debug override, git's refs/remotes/<remote>/HEAD, the record of
// what the remote last advertised here, or the remote's advertised HEAD.
type syncBranchAnswer struct {
	source string
	branch string
}

// localSyncBranch is the answers that cost no network, in precedence order:
// the debug override, then git's refs/remotes/<remote>/HEAD. A git that fails
// to read that ref is the error, never an empty answer.
// [LAW:one-source-of-truth] the one local chain both resolvers below start from.
func localSyncBranch(ctx context.Context, env environ.Env, ws workspace.Info, remote storage.RemoteName) ([]syncBranchAnswer, error) {
	head, err := ws.Repository().LocalRemoteHead(ctx, env, remote)
	if err != nil {
		return nil, err
	}
	return []syncBranchAnswer{
		{source: "override", branch: strings.TrimSpace(env.Get(debugSyncBranchEnvVar))},
		{source: "remote_head", branch: head},
	}, nil
}

// firstSyncBranch is the first non-empty answer, its source set on the
// command's event as sync.branch_source. [LAW:nothing-unseen] which answer
// decided the branch is recorded where the decision is made.
func firstSyncBranch(ctx context.Context, answers ...syncBranchAnswer) string {
	for _, answer := range answers {
		if answer.branch != "" {
			telemetry.Set(ctx, "sync.branch_source", answer.source)
			return answer.branch
		}
	}
	return ""
}

// resolveSyncBranch is the branch push, pull, receive and init sync against.
// When git does not know the remote's default branch it asks the remote, and
// records the answer for knownSyncBranch.
// [LAW:single-enforcer] Sync branch selection is centralized so pull/push/hooks
// consume one canonical branch decision.
func resolveSyncBranch(ctx context.Context, env environ.Env, ws workspace.Info, remote workspace.SyncableRemote) (string, error) {
	local, err := localSyncBranch(ctx, env, ws, remote.Name())
	if err != nil {
		return "", fmt.Errorf("resolve sync branch for remote %q: %w", remote.Name(), err)
	}
	if branch := firstSyncBranch(ctx, local...); branch != "" {
		return branch, nil
	}
	advertised, err := ws.Repository().AdvertisedRemoteHead(ctx, env, remote)
	if err != nil {
		return "", fmt.Errorf("resolve sync branch for remote %q: %w", remote.Name(), err)
	}
	branch := firstSyncBranch(ctx, syncBranchAnswer{source: "advertised", branch: advertised})
	if branch == "" {
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			remote.Name(),
			debugSyncBranchEnvVar,
		)
	}
	// The branch is resolved whether or not the record lands; a lost record only
	// costs read commands their sync banner until the next sync verb writes it.
	if err := writeMarkerAtomic(advertisedSyncBranchPath(ws, remote.Name()), []byte(branch+"\n")); err != nil {
		fmt.Fprintf(os.Stderr, "lit: sync branch record not written: %v\n", err)
	}
	return branch, nil
}

// knownSyncBranch is the sync branch for a command that must not wait on the
// network: resolveSyncBranch's local chain, then what the remote last told a
// sync verb on this machine. An unknown branch is an error, never a guess; an
// unreadable record is that error's cause, and matters only when nothing
// ahead of the record answered.
func knownSyncBranch(ctx context.Context, env environ.Env, ws workspace.Info, remote storage.RemoteName) (string, error) {
	local, err := localSyncBranch(ctx, env, ws, remote)
	if err != nil {
		return "", fmt.Errorf("known sync branch for remote %q: %w", remote, err)
	}
	recorded, readErr := os.ReadFile(advertisedSyncBranchPath(ws, remote))
	branch := firstSyncBranch(ctx, append(local, syncBranchAnswer{source: "recorded", branch: strings.TrimSpace(string(recorded))})...)
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
// — doctor and every read command's banner — with no network: the remote is
// workspaceSyncRemote's and the branch is knownSyncBranch's. ok false is a
// workspace with no sync remote.
func freshnessSyncTarget(ctx context.Context, env environ.Env, ws workspace.Info) (remote storage.RemoteName, branch string, ok bool, err error) {
	syncable, ok, err := workspaceSyncRemote(ctx, ws, "")
	if err != nil || !ok {
		return storage.RemoteName{}, "", ok, err
	}
	remote = syncable.Name()
	branch, err = knownSyncBranch(ctx, env, ws, remote)
	return remote, branch, true, err
}

// workspaceSyncRemote is the remote lit syncs with in this workspace: the one
// --remote requested, else the upstream remote, else the single remote ("" is
// no request). ok false is a workspace with no sync remote.
// [LAW:one-source-of-truth] every caller reads the remote here: the sync verbs
// and the automatic receive fetch it and mark its fetch, the banners read that
// mark, and init adopts from it, so no two of them can name different remotes.
func workspaceSyncRemote(ctx context.Context, ws workspace.Info, requested string) (workspace.SyncableRemote, bool, error) {
	gitRemotes, err := ws.Remotes.List(ctx)
	if err != nil {
		return workspace.SyncableRemote{}, false, err
	}
	return resolveSyncRemote(requested, ws.Remotes.Upstream, gitRemotes)
}

// advertisedSyncBranchPath is where resolveSyncBranch records the branch a
// remote advertised.
func advertisedSyncBranchPath(ws workspace.Info, remote storage.RemoteName) string {
	return perRemotePath(ws, "sync-branch", remote)
}

// perRemotePath is the file holding one fact about one remote: a RemoteName
// holds no "/", so it is a file directly in StorageDir. The name is still
// path-escaped because a RemoteName admits characters the escape rewrites
// (";" among them), and every earlier lit wrote the sync-branch record under
// the escaped name. [LAW:one-type-per-behavior] one naming rule, the stem is
// data.
func perRemotePath(ws workspace.Info, stem string, remote storage.RemoteName) string {
	return filepath.Join(ws.StorageDir, stem+"."+url.PathEscape(remote.String()))
}
