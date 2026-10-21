package store

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRemoteCacheKeyMatchesDoltLayout does a REAL git-backed push against a
// REAL bare remote, lets Dolt create the cache directory itself, and asserts
// remoteMirrorKeys names that exact directory for the remote, read the way the
// prune reads it: from dolt_remotes, through dbfactory.GitRemoteCacheKey. A key
// that missed would leave the live mirror looking abandoned.
// [LAW:behavior-not-structure] it asserts which directory a remote maps to,
// never how the key is computed.
// commitPayload gives the store something to push: an epic and one task
// filed under it, since a task lives inside an epic.
func commitPayload(t *testing.T, ctx context.Context, st *Store, title string) {
	t.Helper()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title + " epic", Topic: "topic", IssueType: "epic", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(%q epic) error = %v", title, err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue(%q) error = %v", title, err)
	}
}

func TestRemoteCacheKeyMatchesDoltLayout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()

	gitBacked := GitBackedRemoteURL(remote)
	if !strings.HasPrefix(gitBacked, "git+") {
		t.Fatalf("GitBackedRemoteURL(%q) = %q, want a git+ transport URL", remote, gitBacked)
	}
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), gitBacked); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}

	// What Dolt actually wrote.
	onDisk, err := listRemoteCacheKeys(sync.remoteCacheBase())
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(onDisk) != 1 {
		t.Fatalf("after one git-backed push: cache keys on disk = %v, want exactly 1", onDisk)
	}

	keys, err := sync.remoteMirrorKeys(ctx)
	if err != nil {
		t.Fatalf("remoteMirrorKeys() error = %v", err)
	}
	if len(keys) != 1 || keys["origin"] != onDisk[0] {
		t.Fatalf("remoteMirrorKeys() = %v, but Dolt created %q for origin; "+
			"pruning on these keys would delete the live mirror", keys, onDisk[0])
	}
}

// TestPruneRemoteCacheKeepsTheLiveMirror proves the whole path is safe in the
// ordinary case: one configured remote, one mirror, nothing removed. This is the
// case that runs on every `lit sync push`, so a regression here is a regression
// that churns the entire cache on every push.
func TestPruneRemoteCacheKeepsTheLiveMirror(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	result, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false)
	if err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	// Nothing was abandoned, so the engine has nothing worth saying and an
	// ordinary push gains no maintenance line at all.
	if result.Maintenance != "" {
		t.Fatalf("Maintenance = %q, want empty on a push with nothing to collect", result.Maintenance)
	}

	live, err := listRemoteCacheKeys(sync.remoteCacheBase())
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("after push + prune: cache keys = %v, want the live mirror to survive", live)
	}
}

// TestPruneRemoteCacheCollectsAbandonedMirrors proves the prune does its job:
// a mirror no configured remote maps to is removed, and the live one is not.
func TestPruneRemoteCacheCollectsAbandonedMirrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}

	// Plant an abandoned mirror: a well-formed key directory with real bytes in
	// it that no configured remote derives. This is the shape an org rename or a
	// URL re-spelling leaves behind.
	cacheBase := sync.remoteCacheBase()
	orphan := strings.Repeat("ab", 32)
	orphanDir := filepath.Join(cacheBase, orphan, "repo.git")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("plant orphan error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "packed-refs"), []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatalf("plant orphan payload error = %v", err)
	}

	outcome := sync.pruneRemoteCache(ctx)
	if outcome.Problem != "" {
		t.Fatalf("prune declined unexpectedly: %s", outcome.Problem)
	}
	if outcome.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 (the planted orphan)", outcome.Removed)
	}
	if outcome.Reclaimed < 4096 {
		t.Fatalf("Reclaimed = %d, want at least the orphan's 4096 bytes", outcome.Reclaimed)
	}
	if _, err := os.Stat(filepath.Join(cacheBase, orphan)); !os.IsNotExist(err) {
		t.Fatalf("orphan %s survived the prune (stat err = %v)", orphan, err)
	}

	remaining, err := listRemoteCacheKeys(cacheBase)
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("after prune: cache keys = %v, want only the live mirror", remaining)
	}
}

// TestPruneRemoteCacheIsNotBlockedByOneStuckMirror pins the head-of-line
// property. plan.abandoned is sorted, so an entry that can never be removed is
// reached first on every push for the life of the workspace; returning on it
// would mean nothing sorting after it is ever attempted again, however
// removable. The failure still has to be reported — the point is that it stops
// being a gate on everyone else's collection.
func TestPruneRemoteCacheIsNotBlockedByOneStuckMirror(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}

	// Two orphans, and the stuck one sorts first so it is reached first. Its own
	// directory cannot be removed, so the walk that measures it succeeds while
	// its removal fails.
	cacheBase := sync.remoteCacheBase()
	stuck, collectible := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	for _, key := range []string{stuck, collectible} {
		repo := filepath.Join(cacheBase, key, "repo.git")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("plant orphan %s: %v", key, err)
		}
		if err := os.WriteFile(filepath.Join(repo, "packed-refs"), make([]byte, 4096), 0o644); err != nil {
			t.Fatalf("plant orphan payload %s: %v", key, err)
		}
	}
	fsperm.Deny(t, filepath.Join(cacheBase, stuck), fsperm.Remove)

	outcome := sync.pruneRemoteCache(ctx)

	if outcome.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 — the collectible orphan sorts after the stuck one "+
			"and must still be collected (Problem = %q)", outcome.Removed, outcome.Problem)
	}
	if _, err := os.Stat(filepath.Join(cacheBase, collectible)); !os.IsNotExist(err) {
		t.Fatalf("the collectible orphan survived: one stuck mirror is blocking the rest (stat err = %v)", err)
	}
	if !strings.Contains(outcome.Problem, stuck) {
		t.Fatalf("Problem = %q, want the stuck mirror still reported by key", outcome.Problem)
	}
	if _, err := os.Stat(filepath.Join(cacheBase, stuck)); err != nil {
		t.Fatalf("the stuck orphan should still be there to report on: %v", err)
	}
	// The stuck orphan's payload sits in a writable repo.git, so it was
	// unlinked before its key directory refused; those bytes are freed and
	// count, beside the collectible orphan's.
	if outcome.Reclaimed != 2*4096 {
		t.Fatalf("Reclaimed = %d, want %d — both payloads were unlinked, the stuck one before its failure",
			outcome.Reclaimed, 2*4096)
	}
}

// seedBareGitRemote creates a bare git repo carrying one commit. The commit is
// required, not decoration: dbfactory's ensureRemoteHasBranches refuses to push
// to a remote advertising no heads.
func seedBareGitRemote(t *testing.T, base string) string {
	t.Helper()
	remote := filepath.Join(base, "remote.git")
	gitInDir(t, base, "init", "--bare", "remote.git")

	seed := filepath.Join(base, "seed")
	gitInDir(t, base, "clone", remote, "seed")
	gitInDir(t, seed, "config", "user.email", "seed@example.com")
	gitInDir(t, seed, "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed readme error = %v", err)
	}
	gitInDir(t, seed, "add", "-A")
	gitInDir(t, seed, "commit", "-m", "seed")
	gitInDir(t, seed, "push", "origin", "HEAD")
	return remote
}

func gitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
}

// pushedSyncStore opens a sync store that has pushed one commit through a
// git-backed remote named origin, so it holds origin's mirror and tracking
// branch.
func pushedSyncStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	remote := seedBareGitRemote(t, t.TempDir())
	doltRoot := migratedDoltDir(t)
	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	t.Cleanup(func() { _ = sync.Close() })
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	return sync
}

// plantOrphanMirror writes a well-formed cache directory no remote derives,
// carrying 4096 bytes: the shape an org rename or a URL re-spelling leaves.
func plantOrphanMirror(t *testing.T, base, key string) {
	t.Helper()
	repo := filepath.Join(base, key, "repo.git")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("plant orphan %s: %v", key, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "packed-refs"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("plant orphan payload %s: %v", key, err)
	}
}

// TestReadAbandonedMirrorsFollowsTheRemotesNotASnapshot pins the re-check the
// delete loop leans on. The loop reads again before every directory, so the
// answer for a key has to change when the remotes change, which a snapshot
// cannot do.
func TestReadAbandonedMirrorsFollowsTheRemotesNotASnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sync := pushedSyncStore(t, ctx)
	base := sync.remoteCacheBase()
	keys, err := listRemoteCacheKeys(base)
	if err != nil || len(keys) != 1 {
		t.Fatalf("cache keys after push = %v (err %v), want exactly the live mirror", keys, err)
	}

	abandoned, err := sync.readAbandonedMirrors(ctx, base)
	if err != nil || len(abandoned) != 0 {
		t.Fatalf("readAbandonedMirrors() with origin configured = (%v, %v), want nothing abandoned", abandoned, err)
	}
	if err := sync.SyncRemoveRemote(ctx, "origin"); err != nil {
		t.Fatalf("SyncRemoveRemote() error = %v", err)
	}
	abandoned, err = sync.readAbandonedMirrors(ctx, base)
	if err != nil || len(abandoned) != 1 || abandoned[0] != keys[0] {
		t.Fatalf("readAbandonedMirrors() after removing origin = (%v, %v), want [%s] — "+
			"the answer must follow the remotes, not a snapshot", abandoned, err, keys[0])
	}
}

// TestPruneRemoteCacheCollectsBesideANeverOpenedRemote is the workspace this
// ticket was filed for: a second remote configured and never pushed or fetched
// through, so it has no directory. The orphan is collected anyway.
func TestPruneRemoteCacheCollectsBesideANeverOpenedRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sync := pushedSyncStore(t, ctx)
	if err := sync.SyncAddRemote(ctx, remoteName(t, "backup"), GitBackedRemoteURL(seedBareGitRemote(t, t.TempDir()))); err != nil {
		t.Fatalf("SyncAddRemote(backup) error = %v", err)
	}
	orphan := strings.Repeat("ab", 32)
	plantOrphanMirror(t, sync.remoteCacheBase(), orphan)

	outcome := sync.pruneRemoteCache(ctx)
	if outcome.Problem != "" {
		t.Fatalf("prune declined beside a never-opened remote: %s", outcome.Problem)
	}
	if outcome.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 (the planted orphan)", outcome.Removed)
	}
	if _, err := os.Stat(filepath.Join(sync.remoteCacheBase(), orphan)); !os.IsNotExist(err) {
		t.Fatalf("orphan %s survived the prune (stat err = %v)", orphan, err)
	}
}

// TestPruneRemoteCacheKeepsTheMirrorOfAFailedFirstPush pins the mirror that
// exists before any push or fetch has landed. Dolt creates a remote's directory
// and initializes it before it asks the remote anything, so a first push to a
// remote advertising no branches fails and leaves the directory behind, with
// no tracking branch to show for it. It is backup's mirror all the same, and
// the next open through backup reuses it, so the prune must leave it.
func TestPruneRemoteCacheKeepsTheMirrorOfAFailedFirstPush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sync := pushedSyncStore(t, ctx)
	empty := t.TempDir()
	gitInDir(t, empty, "init", "--bare", "remote.git")
	if err := sync.SyncAddRemote(ctx, remoteName(t, "backup"), GitBackedRemoteURL(filepath.Join(empty, "remote.git"))); err != nil {
		t.Fatalf("SyncAddRemote(backup) error = %v", err)
	}
	base := sync.remoteCacheBase()
	before, err := listRemoteCacheKeys(base)
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "backup"), "master", unfiredBound(t), false, false); err == nil {
		t.Fatal("SyncCompactAndPush(backup) to a remote with no branches succeeded, want the failed first push")
	}
	after, err := listRemoteCacheKeys(base)
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("cache keys before %v, after the failed push %v: want the failed push to have left one directory", before, after)
	}

	outcome := sync.pruneRemoteCache(ctx)
	if outcome.Removed != 0 || outcome.Problem != "" {
		t.Fatalf("outcome = %+v, want nothing removed and no problem", outcome)
	}
	survivors, err := listRemoteCacheKeys(base)
	if err != nil || !slices.Equal(survivors, after) {
		t.Fatalf("cache keys after the prune = %v (err %v), want %v — backup's mirror was deleted", survivors, err, after)
	}
}

// TestPruneRemoteCacheKeepsTheMirrorOfARemoteWithItsOwnRef pins that a remote's
// key is read with the remote's params. DOLT_REMOTE's --ref is part of what
// Dolt hashes into the directory name, so a key derived without it names a
// directory that does not exist and leaves the live one looking abandoned.
func TestPruneRemoteCacheKeepsTheMirrorOfARemoteWithItsOwnRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sync := pushedSyncStore(t, ctx)
	backupURL := GitBackedRemoteURL(seedBareGitRemote(t, t.TempDir()))
	if _, err := sync.db.ExecContext(ctx, "CALL DOLT_REMOTE('add', '--ref', 'refs/dolt/other', 'backup', ?)", backupURL); err != nil {
		t.Fatalf("DOLT_REMOTE add --ref error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "backup"), "master", unfiredBound(t), false, false); err != nil {
		t.Fatalf("SyncCompactAndPush(backup) error = %v", err)
	}
	base := sync.remoteCacheBase()
	mirrors, err := listRemoteCacheKeys(base)
	if err != nil || len(mirrors) != 2 {
		t.Fatalf("cache keys after pushing origin and backup = %v (err %v), want 2", mirrors, err)
	}

	outcome := sync.pruneRemoteCache(ctx)
	if outcome.Removed != 0 || outcome.Problem != "" {
		t.Fatalf("outcome = %+v, want nothing removed and no problem", outcome)
	}
	survivors, err := listRemoteCacheKeys(base)
	if err != nil || !slices.Equal(survivors, mirrors) {
		t.Fatalf("cache keys after the prune = %v (err %v), want %v — backup's mirror was deleted", survivors, err, mirrors)
	}
}

// TestSyncCompactAndPushPrunesWhenThePushFails pins that the prune does not
// ride on the push landing: nothing about collecting an abandoned mirror
// depends on it, so a push that fails still collects one, and the failure
// carries the prune's line where an operator looks.
func TestSyncCompactAndPushPrunesWhenThePushFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sync := pushedSyncStore(t, ctx)
	empty := t.TempDir()
	gitInDir(t, empty, "init", "--bare", "remote.git")
	if err := sync.SyncAddRemote(ctx, remoteName(t, "backup"), GitBackedRemoteURL(filepath.Join(empty, "remote.git"))); err != nil {
		t.Fatalf("SyncAddRemote(backup) error = %v", err)
	}
	orphan := strings.Repeat("ab", 32)
	plantOrphanMirror(t, sync.remoteCacheBase(), orphan)

	_, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "backup"), "master", unfiredBound(t), false, false)
	if err == nil {
		t.Fatal("SyncCompactAndPush(backup) to a remote with no branches succeeded, want a failed push")
	}
	if !strings.Contains(err.Error(), "remote-cache prune: removed 1 abandoned mirror") {
		t.Fatalf("error = %q, want it to carry the prune's report", err)
	}
	if _, statErr := os.Stat(filepath.Join(sync.remoteCacheBase(), orphan)); !os.IsNotExist(statErr) {
		t.Fatalf("orphan %s survived a prune that ran beside a failed push (stat err = %v)", orphan, statErr)
	}
}

// TestSyncRemoteMirrorHoldsItsOwnPushNotAPeersLaterOne pins the one fact the
// push's received-refs proof rests on: after a store's push lands, the
// remote's head is a commit that store's git mirror holds, and a head written
// afterwards by another store (here a clone of it pushing past it, the
// mirror's own topology) is one it does not, until it fetches. Two-sided on
// purpose: the peer's mirror holds the same head this one lacks, so the
// answer depends on whose mirror is asked, not on the commit.
func TestSyncRemoteMirrorHoldsItsOwnPushNotAPeersLaterOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)
	remoteHead := func() string {
		t.Helper()
		out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "refs/dolt/data").Output()
		if err != nil {
			t.Fatalf("rev-parse refs/dolt/data: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(root, title string) {
		t.Helper()
		st, err := Open(ctx, root, "ws")
		if err != nil {
			t.Fatalf("Open(%q) error = %v", title, err)
		}
		commitPayload(t, ctx, st, title)
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", title, err)
		}
	}
	holds := func(label, root string, commits []string) bool {
		t.Helper()
		st, err := OpenSync(ctx, root, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync() error = %v", label, err)
		}
		defer st.Close()
		held, err := st.SyncRemoteMirrorHolds(ctx, remoteName(t, "origin"), commits)
		if err != nil {
			t.Fatalf("%s: SyncRemoteMirrorHolds() error = %v", label, err)
		}
		return held
	}

	commit(doltRoot, "c1")
	st, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := st.SyncAddRemote(ctx, remoteName(t, "origin"), GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if held, err := st.SyncRemoteMirrorHolds(ctx, remoteName(t, "origin"), []string{strings.Repeat("a", 40)}); err != nil || held {
		t.Fatalf("before any push: SyncRemoteMirrorHolds() = %t, %v; want false with no mirror on disk", held, err)
	}
	if _, err := st.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	if _, err := st.SyncRemoteMirrorHolds(ctx, remoteName(t, "upstream"), []string{remoteHead()}); err == nil {
		t.Fatal("SyncRemoteMirrorHolds() answered for a remote this store does not have")
	}
	if held, err := st.SyncRemoteMirrorHolds(ctx, remoteName(t, "origin"), nil); err != nil || held {
		t.Fatalf("no commits: SyncRemoteMirrorHolds() = %t, %v; want false, nothing proven", held, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	ownHead := remoteHead()
	if !holds("own push", doltRoot, []string{ownHead}) {
		t.Fatalf("the pushing store's mirror does not hold the head its own push left (%s)", ownHead)
	}

	peerRoot := filepath.Join(base, "peer", "dolt")
	if err := os.MkdirAll(filepath.Dir(peerRoot), 0o755); err != nil {
		t.Fatalf("mkdir peer: %v", err)
	}
	if err := dbsnapshot.CloneTree(ctx, doltRoot, peerRoot); err != nil {
		t.Fatalf("CloneTree() error = %v", err)
	}
	commit(peerRoot, "c2")
	peer, err := OpenSync(ctx, peerRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync(peer) error = %v", err)
	}
	if _, err := peer.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(peer) error = %v", err)
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("Close(peer) error = %v", err)
	}
	peerHead := remoteHead()
	if peerHead == ownHead {
		t.Fatalf("the peer's push left the remote where it was (%s); nothing distinguishes the two mirrors", peerHead)
	}
	if !holds("peer", peerRoot, []string{peerHead}) {
		t.Fatalf("the peer's mirror does not hold the head its own push left (%s)", peerHead)
	}
	if holds("after the peer pushed", doltRoot, []string{peerHead}) {
		t.Fatalf("this store's mirror claims the peer's later head %s it never fetched", peerHead)
	}
	if holds("mixed", doltRoot, []string{ownHead, peerHead}) {
		t.Fatal("one held commit answered for a list with an unheld one in it")
	}
}

// TestDecodeRemoteParamsGivesAMapToOpenWith decodes each params column
// dolt_remotes can hold. A remote with none - SQL NULL, or JSON null where
// Dolt rendered a nil map - has an empty map the open can add the git
// remote's name to, and one with params has them.
func TestDecodeRemoteParamsGivesAMapToOpenWith(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  sql.NullString
		want map[string]interface{}
	}{
		{raw: sql.NullString{}, want: map[string]interface{}{}},
		{raw: sql.NullString{String: "null", Valid: true}, want: map[string]interface{}{}},
		{raw: sql.NullString{String: "{}", Valid: true}, want: map[string]interface{}{}},
		{raw: sql.NullString{String: `{"k":"v"}`, Valid: true}, want: map[string]interface{}{"k": "v"}},
	} {
		params, err := decodeRemoteParams("origin", tc.raw)
		if err != nil || !maps.Equal(params, tc.want) {
			t.Fatalf("decodeRemoteParams(%+v) = %v, %v; want %v", tc.raw, params, err, tc.want)
		}
		params[dbfactory.GitRemoteNameParam] = "origin"
	}
}
