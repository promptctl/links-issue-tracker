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

// syncBranchAnswer is one source of the sync branch, named on the event by
// where it comes from: the debug override, git's refs/remotes/<remote>/HEAD,
// the remote's advertised HEAD, or the record of what it last advertised here.
// ask is the source's question, "" for no answer.
type syncBranchAnswer struct {
	source string
	ask    func() (string, error)
}

// firstSyncBranch asks the sources in precedence order and takes the first
// branch named. A source is asked only when every source ahead of it named
// none: the override decides alone, so no git it would outrank can fail the
// resolve, and nothing is spawned for a branch already decided. The deciding
// source lands on the command's event as sync.branch_source.
// [LAW:nothing-unseen] [LAW:single-enforcer] the one precedence both resolvers run.
func firstSyncBranch(ctx context.Context, answers ...syncBranchAnswer) (string, error) {
	for _, answer := range answers {
		branch, err := answer.ask()
		if err != nil {
			return "", err
		}
		if branch != "" {
			telemetry.Set(ctx, "sync.branch_source", answer.source)
			return branch, nil
		}
	}
	return "", nil
}

// localSyncBranch is the sources that cost no network, ahead of both
// resolvers' last one: the debug override, then git's
// refs/remotes/<remote>/HEAD, whose failed read is the error, never an empty
// answer. [LAW:one-source-of-truth] the one local chain both resolvers start from.
func localSyncBranch(ctx context.Context, env environ.Env, ws workspace.Info, remote storage.RemoteName, last syncBranchAnswer) (string, error) {
	return firstSyncBranch(ctx,
		syncBranchAnswer{source: "override", ask: func() (string, error) { return strings.TrimSpace(env.Get(debugSyncBranchEnvVar)), nil }},
		syncBranchAnswer{source: "remote_head", ask: func() (string, error) { return ws.Repository().LocalRemoteHead(ctx, env, remote) }},
		last,
	)
}

// resolveSyncBranch is the branch push, pull, receive and init sync against.
// When git does not know the remote's default branch it asks the remote, and
// records the answer for knownSyncBranch.
// [LAW:single-enforcer] Sync branch selection is centralized so pull/push/hooks
// consume one canonical branch decision.
func resolveSyncBranch(ctx context.Context, env environ.Env, ws workspace.Info, remote workspace.SyncableRemote) (string, error) {
	branch, err := localSyncBranch(ctx, env, ws, remote.Name(), syncBranchAnswer{source: "advertised", ask: func() (string, error) {
		return advertiseSyncBranch(ctx, env, ws, remote)
	}})
	if err != nil {
		return "", fmt.Errorf("resolve sync branch for remote %q: %w", remote.Name(), err)
	}
	if branch == "" {
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			remote.Name(),
			debugSyncBranchEnvVar,
		)
	}
	return branch, nil
}

// advertiseSyncBranch asks the remote which branch its HEAD names, and
// records a named one for knownSyncBranch. The branch is resolved whether or
// not the record lands; a lost record only costs read commands their sync
// banner until the next sync verb writes it.
func advertiseSyncBranch(ctx context.Context, env environ.Env, ws workspace.Info, remote workspace.SyncableRemote) (string, error) {
	branch, err := ws.Repository().AdvertisedRemoteHead(ctx, env, remote)
	if err != nil || branch == "" {
		return "", err
	}
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
	var readErr error
	branch, err := localSyncBranch(ctx, env, ws, remote, syncBranchAnswer{source: "recorded", ask: func() (string, error) {
		recorded, err := os.ReadFile(advertisedSyncBranchPath(ws, remote))
		readErr = err
		return strings.TrimSpace(string(recorded)), nil
	}})
	if err != nil {
		return "", fmt.Errorf("known sync branch for remote %q: %w", remote, err)
	}
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
