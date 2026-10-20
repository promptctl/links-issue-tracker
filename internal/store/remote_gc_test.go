package store

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// gitRemote is a bare git repository a checkout syncs with as a git remote,
// the shape of every lit checkout's remote: the URL a store adds it under,
// and its directory, for reading the tree Dolt keeps under refs/dolt/data.
func gitRemote(t *testing.T) (url, gitDir string) {
	t.Helper()
	gitDir = filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "-q", "--bare", gitDir)
	// Dolt pushes only to a remote that has a branch.
	empty := runGit(t, gitDir, "hash-object", "-t", "tree", "-w", "/dev/null")
	runGit(t, gitDir, "update-ref", "refs/heads/main", runGit(t, gitDir, "commit-tree", empty, "-m", "init"))
	return "git+file://" + filepath.ToSlash(gitDir), gitDir
}

func runGit(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	if gitDir != "" {
		args = append([]string{"--git-dir", gitDir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// dataTree is the table files of the tree the remote's refs/dolt/data names,
// read with git rather than through Dolt: how many there are and their bytes.
// A table file larger than a blob is a directory of parts, so it is counted
// once and its parts summed.
func dataTree(t *testing.T, gitDir string) (files int, bytes int64) {
	t.Helper()
	names := map[string]bool{}
	for _, line := range strings.Split(runGit(t, gitDir, "ls-tree", "-r", "--long", "refs/dolt/data"), "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("ls-tree line %q has no path", line)
		}
		name, _, _ := strings.Cut(path, "/")
		if name == "manifest" {
			continue
		}
		fields := strings.Fields(meta)
		size, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("ls-tree line %q: %v", line, err)
		}
		names[name] = true
		bytes += size
	}
	return len(names), bytes
}

// pushOtherBranch pushes a branch named other from alpha to its remote, so
// the remote holds a branch besides the sync branch, runs more calls after
// it, and returns other's head.
func pushOtherBranch(t *testing.T, ctx context.Context, alpha string, more ...[]string) string {
	t.Helper()
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	for _, call := range append([][]string{{"DOLT_BRANCH", "other"}, {"DOLT_PUSH", "origin", "other"}}, more...) {
		if err := execProcedureDiscard(ctx, st.db, call[0], call[1:]...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := commitHashOfRef(ctx, st.db, "other")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// remoteBranches is the remote's sync branch head and its branch other, as a
// pruning fetch from alpha finds them: other is empty once the remote no
// longer has it.
func remoteBranches(t *testing.T, ctx context.Context, alpha string) (master, other string) {
	t.Helper()
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if err := st.SyncFetch(ctx, "origin", true); err != nil {
		t.Fatal(err)
	}
	master, err := commitHashOfRef(ctx, st.db, "remotes/origin/master")
	if err != nil {
		t.Fatal(err)
	}
	other, _ = commitHashOfRef(ctx, st.db, "remotes/origin/other")
	return master, other
}

// TestAPushPastTheBoundCollectsTheRemote drives a checkout's writes past a
// small bound on a git remote that also holds a branch other than the sync
// branch. The push that reaches the bound replaces the remote's table files
// with the ones the re-rooted head reaches: the remote's tree ends no larger
// than the checkout's collected store plus one push's files, the other branch
// is gone, the push's event says what the GC found and did, and a checkout
// adopting the remote afterwards gets the same backlog.
func TestAPushPastTheBoundCollectsTheRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, gitDir := gitRemote(t)
	alpha := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	pushOtherBranch(t, ctx, alpha)
	st := openSyncOrFatal(t, ctx, alpha)
	seeded := heightOf(t, ctx, st, "HEAD")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	bound := windowOf(t, seeded+2, 3)
	if under := compactPush(t, ctx, alpha, epic, "under", bound); under.Fired() || under.RemoteGC.Outcome != "" {
		t.Fatalf("a push under the bound = %+v; want no remote GC", under)
	}
	_, grown := dataTree(t, gitDir)

	fired := compactPush(t, ctx, alpha, epic, "past", bound)
	if fired.Err != nil || fired.Lease != storage.LeaseHeld || !fired.Collection.Ran {
		t.Fatalf("the push past the bound = %+v; want its re-root held and the checkout collected", fired)
	}
	gc := fired.RemoteGC
	_, bytes := dataTree(t, gitDir)
	if gc.Outcome != storage.RemoteGCReplaced || !slices.Equal(gc.DroppedBranches, []string{"refs/heads/other"}) {
		t.Fatalf("the remote GC = %+v; want the table files replaced and the other branch dropped", gc)
	}
	if gc.CollectedBytes != bytes {
		t.Fatalf("the remote GC collected %d bytes of table files; git counts %d in the remote's tree", gc.CollectedBytes, bytes)
	}
	if gc.BytesBefore <= grown || bytes >= grown {
		t.Fatalf("the remote held %d bytes before the push past the bound, the GC found %d and left %d; want it found more than the push before it left, and left less", grown, gc.BytesBefore, bytes)
	}

	before := exportOf(t, ctx, alpha)
	afterPush := compactPush(t, ctx, alpha, epic, "after", bound)
	if afterPush.Fired() || afterPush.Err != nil {
		t.Fatalf("the push after the GC = %+v; want an ordinary push", afterPush)
	}
	_, pushed := dataTree(t, gitDir)
	if onePush := pushed - bytes; bytes > fired.Collection.BytesAfter+onePush {
		t.Fatalf("the remote holds %d bytes after its GC; want no more than the collected store's %d plus one push's %d", bytes, fired.Collection.BytesAfter, onePush)
	}

	if _, other := remoteBranches(t, ctx, alpha); other != "" {
		t.Fatalf("the remote still holds the branch other at %s; want the GC to have dropped it", other)
	}

	gamma := migratedDoltDir(t)
	adoptRemote(t, ctx, gamma, remoteURL)
	if got := exportOf(t, ctx, gamma); !backlogGrewBy(before, got, "after") {
		t.Fatalf("a checkout adopting the collected remote holds %d issues; want alpha's %d and the ticket pushed after", len(got.Issues), len(before.Issues))
	}

	event := fired.EventValue()
	want := map[string]any{
		"remote_gc.outcome":         string(storage.RemoteGCReplaced),
		"remote_gc.bytes_before":    gc.BytesBefore,
		"remote_gc.collected_bytes": bytes,
	}
	for key, value := range want {
		if event[key] != value {
			t.Fatalf("event %s = %v, want %v (event %v)", key, event[key], value, event)
		}
	}
	if dropped, _ := event["remote_gc.dropped_branches"].([]string); !slices.Equal(dropped, []string{"refs/heads/other"}) {
		t.Fatalf("event remote_gc.dropped_branches = %v, want [refs/heads/other] (event %v)", event["remote_gc.dropped_branches"], event)
	}
}

// landReRootWithoutGC publishes alpha's re-root of the remote head, keeping
// keep commits, the way a bound does up to its force push, and stops there:
// the moment a peer can write before the remote's GC runs.
func landReRootWithoutGC(t *testing.T, ctx context.Context, alpha string, keep uint64) storage.ReRooted {
	t.Helper()
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	seen, err := commitHashOfRef(ctx, st.db, "remotes/origin/master")
	if err != nil {
		t.Fatal(err)
	}
	landed, err := st.reRootScratch(ctx, "master", seen, keep)
	if err != nil {
		t.Fatalf("reRootScratch error = %v", err)
	}
	if lease, err := st.publishReRoot(ctx, "origin", "master", seen); err != nil || lease != storage.LeaseHeld {
		t.Fatalf("publishReRoot = %q, %v; want held", lease, err)
	}
	return landed
}

// TestARemoteAPeerWroteToAfterTheReRootKeepsItsTableFiles has a peer follow
// alpha's re-root and push on top of it before alpha's remote GC runs, on a
// remote that also holds a branch other than the sync branch. The remote's
// head is no longer the one alpha re-rooted, so the GC collects nothing and
// writes nothing: the remote's tree is untouched, the other branch is still
// there, and the peer's push is the head.
func TestARemoteAPeerWroteToAfterTheReRootKeepsItsTableFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, gitDir := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	otherHead := pushOtherBranch(t, ctx, alpha)
	createAndPush(t, ctx, alpha, epic, "a1")
	landed := landReRootWithoutGC(t, ctx, alpha, 3)
	adoptRemote(t, ctx, bravo, remoteURL)
	createAndPush(t, ctx, bravo, epic, "b1")
	bravoHead := headAt(t, ctx, bravo)
	files, bytes := dataTree(t, gitDir)

	st := openSyncOrFatal(t, ctx, alpha)
	gc, err := st.collectRemote(ctx, "origin", "master", landed.Head)
	if closeErr := st.Close(); err == nil {
		err = closeErr
	}
	want := storage.RemoteGC{Outcome: storage.RemoteGCMoved, BytesBefore: bytes}
	if err != nil || !reflect.DeepEqual(gc, want) {
		t.Fatalf("collectRemote over a remote bravo pushed to = %+v, %v; want %+v", gc, err, want)
	}
	if gotFiles, gotBytes := dataTree(t, gitDir); gotFiles != files || gotBytes != bytes {
		t.Fatalf("the remote's tree went from %d files of %d bytes to %d of %d; want it untouched", files, bytes, gotFiles, gotBytes)
	}
	if master, other := remoteBranches(t, ctx, alpha); master != bravoHead || other != otherHead {
		t.Fatalf("the remote holds master at %s and other at %s; want bravo's push %s and other kept at %s", master, other, bravoHead, otherHead)
	}
}

// TestAPeerWriteAfterTheGCReadTheRemoteKeepsEveryBranch has bravo push after
// alpha's GC read the remote, while the remote still carried alpha's re-root
// and a branch other than the sync branch. Every check the GC makes reads
// that view, so it collects and uploads; its root write lands only over the
// view's root, which bravo's push replaced, so the GC ends moved with every
// branch kept and bravo's push the head.
func TestAPeerWriteAfterTheGCReadTheRemoteKeepsEveryBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	otherHead := pushOtherBranch(t, ctx, alpha)
	createAndPush(t, ctx, alpha, epic, "a1")
	landed := landReRootWithoutGC(t, ctx, alpha, 3)

	st := openSyncOrFatal(t, ctx, alpha)
	view, err := st.openRemoteDB(ctx, "origin")
	if closeErr := st.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = view.Close() }()
	adoptRemote(t, ctx, bravo, remoteURL)
	createAndPush(t, ctx, bravo, epic, "b1")
	bravoHead := headAt(t, ctx, bravo)

	gc, err := collectRemoteView(ctx, view, t.TempDir(), "master", landed.Head)
	if err != nil || gc.Outcome != storage.RemoteGCMoved || gc.DroppedBranches != nil || gc.CollectedBytes == 0 {
		t.Fatalf("alpha's GC over the view it read before bravo's push = %+v, %v; want it collected and then moved", gc, err)
	}
	if master, other := remoteBranches(t, ctx, alpha); master != bravoHead || other != otherHead {
		t.Fatalf("the remote holds master at %s and other at %s; want bravo's push %s and other kept at %s", master, other, bravoHead, otherHead)
	}
}

// TestAPushThatReadTheRemoteBeforeItsGCLandsAfterIt is the push that races
// the remote's GC from the other side: bravo follows alpha's re-root and reads
// the remote before alpha's GC replaces its table files, then pushes a commit
// through that view after it. Bravo's view holds the GC generation the GC
// ended, so its push re-checks its table files against the new ones before
// adding them, lands, and is the head a checkout adopting the remote gets.
func TestAPushThatReadTheRemoteBeforeItsGCLandsAfterIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")
	landed := landReRootWithoutGC(t, ctx, alpha, 3)
	adoptRemote(t, ctx, bravo, remoteURL)
	st, err := Open(ctx, bravo, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "b1", Topic: "topic", IssueType: "task", ParentID: epic}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	bravoHead := hash.Parse(headAt(t, ctx, bravo))
	st = openSyncOrFatal(t, ctx, bravo)
	view, err := st.openRemoteDB(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = view.Close() }()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := view.ResolveCommitRef(ctx, ref.NewBranchRef("master")); err != nil {
		t.Fatal(err)
	}

	st = openSyncOrFatal(t, ctx, alpha)
	gc, err := st.collectRemote(ctx, "origin", "master", landed.Head)
	if closeErr := st.Close(); err == nil {
		err = closeErr
	}
	if err != nil || gc.Outcome != storage.RemoteGCReplaced {
		t.Fatalf("alpha's remote GC = %+v, %v; want replaced", gc, err)
	}

	source, err := openCloneChunkStore(ctx, bravo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := view.PullChunks(bounded, t.TempDir(), source, []hash.Hash{bravoHead}, nil, nil); err != nil {
		t.Fatalf("bravo's push through the view it read before the GC: %v", err)
	}
	commit, err := readFullCommit(ctx, source, bravoHead)
	if err != nil {
		t.Fatal(err)
	}
	if err := view.FastForward(bounded, ref.NewBranchRef("master"), commit); err != nil {
		t.Fatalf("move the remote's master to bravo's commit: %v", err)
	}

	gamma := migratedDoltDir(t)
	adoptRemote(t, ctx, gamma, remoteURL)
	if head := headAt(t, ctx, gamma); head != bravoHead.String() {
		t.Fatalf("a checkout adopting the remote is at %s; want bravo's push %s", head, bravoHead)
	}
}

// TestAPeerWriteAfterTheRootWriteKeepsTheTableSet is the race at the GC's
// last write: alpha's root write has dropped the other branches, and bravo's
// push lands before alpha replaces the remote's table files. The replacement
// refuses the manifest bravo wrote, so the GC ends dropped and the remote's
// tree is what bravo's push left.
func TestAPeerWriteAfterTheRootWriteKeepsTheTableSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, gitDir := gitRemote(t)
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")
	landed := landReRootWithoutGC(t, ctx, alpha, 3)

	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	remoteDB, err := st.openRemoteDB(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = remoteDB.Close() }()
	remote := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(remoteDB)).(*nbs.NomsBlockStore)
	viewRoot, err := remote.Root(ctx)
	if err != nil {
		t.Fatal(err)
	}
	collected, err := buildCollectedSet(ctx, t.TempDir(), remoteDB, "master", landed.Head)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = collected.Close() }()
	collectedCS := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(collected))
	sources, err := collectedCS.(chunks.TableFileStore).Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files, err := uploadTableFiles(ctx, sources, remote)
	if err != nil {
		t.Fatal(err)
	}
	if pointed, err := pointRootAt(ctx, collectedCS, sources.Root, remote, viewRoot); err != nil || !pointed {
		t.Fatalf("pointRootAt over the view's root = %v, %v; want it pointed", pointed, err)
	}

	adoptRemote(t, ctx, bravo, remoteURL)
	createAndPush(t, ctx, bravo, epic, "b1")
	treeFiles, treeBytes := dataTree(t, gitDir)

	outcome, err := replaceTableFiles(ctx, remote, files)
	if err != nil || outcome != storage.RemoteGCDropped {
		t.Fatalf("the GC's table-set replacement after bravo's push = %q, %v; want dropped", outcome, err)
	}
	if gotFiles, gotBytes := dataTree(t, gitDir); gotFiles != treeFiles || gotBytes != treeBytes {
		t.Fatalf("the remote's tree went from %d files of %d bytes to %d of %d; want bravo's push's tree untouched", treeFiles, treeBytes, gotFiles, gotBytes)
	}
}

// TestARemoteHoldingATagKeepsEveryBranch runs the remote's GC over a remote
// that holds a tag beside a branch other than the sync branch. The collected
// files hold the sync branch's history alone and a tag is nothing the GC can
// drop, so the GC is an error and the other branch is still on the remote.
func TestARemoteHoldingATagKeepsEveryBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL, _ := gitRemote(t)
	alpha := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	otherHead := pushOtherBranch(t, ctx, alpha, []string{"DOLT_TAG", "v1"}, []string{"DOLT_PUSH", "origin", "v1"})
	createAndPush(t, ctx, alpha, epic, "a1")
	landed := landReRootWithoutGC(t, ctx, alpha, 3)

	st := openSyncOrFatal(t, ctx, alpha)
	gc, err := st.collectRemote(ctx, "origin", "master", landed.Head)
	if closeErr := st.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "refs/tags/v1") || gc.Outcome != "" || gc.DroppedBranches != nil {
		t.Fatalf("collectRemote over a remote holding a tag = %+v, %v; want an error naming the tag and nothing written", gc, err)
	}
	if _, other := remoteBranches(t, ctx, alpha); other != otherHead {
		t.Fatalf("the remote's branch other is at %q; want it kept at %s", other, otherHead)
	}
}

// TestEachOpenOfARemoteIsItsCallersOwn opens a remote's database twice in one
// process through openRemoteDB, the open every remote GC runs, as a checkout
// following two re-roots does. Each open is the caller's own handle: closing
// the first leaves nothing the second is handed.
func TestEachOpenOfARemoteIsItsCallersOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "producer")
	seedReconcileRemote(t, ctx, root, "file://"+filepath.Join(base, "remote"))
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	for _, which := range []string{"first", "second"} {
		remoteDB, err := st.openRemoteDB(ctx, "origin")
		if err != nil {
			t.Fatalf("openRemoteDB(%s): %v", which, err)
		}
		if _, err := remoteDB.GetBranches(ctx); err != nil {
			t.Fatalf("read the remote's branches (%s open): %v", which, err)
		}
		if err := remoteDB.Close(); err != nil {
			t.Fatalf("close the remote (%s open): %v", which, err)
		}
	}
}
