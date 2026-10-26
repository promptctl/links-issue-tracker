package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestOpenSyncDoesNotCreateStartupCommitWhenSchemaIsCurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	before := observedHead(t, ctx, doltRoot, "test-workspace-id")

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := syncStore.Close(); err != nil {
		t.Fatalf("Close() sync error = %v", err)
	}

	if after := observedHead(t, ctx, doltRoot, "test-workspace-id"); after != before {
		t.Fatalf("OpenSync() moved HEAD from %s to %s: it created a commit", before, after)
	}
}

func TestOpenSyncCreatesDatabaseWhenMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := syncStore.Close(); err != nil {
		t.Fatalf("Close() sync error = %v", err)
	}

	if branch := observedBranch(t, ctx, doltRoot, "test-workspace-id"); branch != "master" {
		t.Fatalf("database OpenSync created is on branch %q, want master", branch)
	}
}

// seedSyncEpic opens the store at doltRoot, creates one top-level epic, and closes
// it again, returning the epic's id: every leaf a test then commits lives
// inside it. The create is one commit of its own, landed before the test's
// first.
func seedSyncEpic(t *testing.T, ctx context.Context, doltRoot, workspace string) string {
	t.Helper()
	st, err := Open(ctx, testEnv(), doltRoot, workspace)
	if err != nil {
		t.Fatalf("Open(epic) error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(epic) error = %v", err)
	}
	return epic.ID
}

// premadeMainBranchDB builds the pre-made-database shape an adopt's clone
// arrives in: a real database at the root, created outside lit's bootstrap,
// still on Dolt's own default branch rather than master. Returns the dolt
// root, having asserted the branch is not yet master — the precondition both
// normalization tests depend on.
func premadeMainBranchDB(t *testing.T, ctx context.Context) (doltRoot string) {
	t.Helper()
	doltRoot = filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(doltRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(doltRoot) error = %v", err)
	}
	bootstrap, err := openDoltPool(doltRoot, "test-workspace-id", "", newLockHold(context.Background(), doltRoot, engineWrite))
	if err != nil {
		t.Fatalf("openDoltPool() bootstrap error = %v", err)
	}
	if _, err := bootstrap.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", doltDatabaseName)); err != nil {
		t.Fatalf("bootstrap create database error = %v", err)
	}
	if err := bootstrap.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap close error = %v", err)
	}
	if branch := observedBranch(t, ctx, doltRoot, "test-workspace-id"); branch == "master" {
		t.Fatal("pre-made database is already on master")
	}
	return doltRoot
}

// TestOpenSyncNormalizesPreMadeMainBranchToMaster proves the mirror-side
// rename actually fires: OpenSync on a pre-made non-master database must land
// it on master via its own engine — the sibling of the Open half proven
// below, so neither write-open path only ever no-ops through the
// normalization call.
func TestOpenSyncNormalizesPreMadeMainBranchToMaster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := premadeMainBranchDB(t, ctx)

	sync, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if branch := observedBranch(t, ctx, doltRoot, "test-workspace-id"); branch != "master" {
		t.Fatalf("after OpenSync the database is on branch %q, want master", branch)
	}
}

// TestOpenSyncOnNormalizedStoreIgnoresHeldCommitLock pins the liveness
// contract the rename's lock-free pre-check exists for: on an
// already-normalized store — every store but a freshly adopted one — OpenSync
// must complete while another holder (a snapshot copy, which can hold for
// minutes) owns the commit lock, because its normalization is a read that
// never needs the lock. Without the pre-check this test hangs on the commit
// lock's retry budget instead of returning.
func TestOpenSyncOnNormalizedStoreIgnoresHeldCommitLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	release, err := LockCommitPath(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockCommitPath() error = %v", err)
	}
	defer func() {
		if release != nil {
			_ = release()
		}
	}()

	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sync, err := OpenSync(openCtx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() under a held commit lock error = %v", err)
	}
	if _, err := sync.SyncListRemotes(ctx); err != nil {
		t.Fatalf("SyncListRemotes() under a held commit lock error = %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release commit lock: %v", err)
	}
	release = nil
}

func TestBranchNormalizationDefersToFirstWriteOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := premadeMainBranchDB(t, ctx)

	// EnsureDatabase over a pre-existing database is a no-op — branch
	// normalization is owned by write opens, so a pre-made main-branch store
	// stays on main here...
	if _, err := EnsureDatabase(ctx, doltRoot, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}
	if branch := observedBranch(t, ctx, doltRoot, "test-workspace-id"); branch == "master" {
		t.Fatal("EnsureDatabase should leave a pre-existing database untouched, but it is on master")
	}

	// ...and the contract — on master before any write use — holds at the
	// first write open. [LAW:behavior-not-structure]
	st, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if branch := observedBranch(t, ctx, doltRoot, "test-workspace-id"); branch != "master" {
		t.Fatalf("after the first write open the database is on branch %q, want master", branch)
	}
}

func TestSyncRemoteLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	if err := syncStore.SyncAddRemote(ctx, remoteName(t, "origin"), "https://example.com/repo.git"); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	remotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() after add error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Name != "origin" {
		t.Fatalf("remotes after add = %#v", remotes)
	}
	if remotes[0].URL == "" {
		t.Fatalf("remotes after add missing URL: %#v", remotes)
	}

	if err := syncStore.SyncRemoveRemote(ctx, "origin"); err != nil {
		t.Fatalf("SyncRemoveRemote() error = %v", err)
	}

	remotes, err = syncStore.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() after remove error = %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("remotes after remove = %#v, want empty", remotes)
	}
}

func TestSyncRemoteValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	testCases := []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{
			name:    "add remote requires url",
			run:     func() error { return syncStore.SyncAddRemote(ctx, remoteName(t, "origin"), "   ") },
			wantErr: "remote url is required",
		},
		{
			name:    "remove remote requires name",
			run:     func() error { return syncStore.SyncRemoveRemote(ctx, "   ") },
			wantErr: "remote is required",
		},
		{
			name: "push requires branch",
			run: func() error {
				_, err := syncStore.SyncPush(ctx, remoteName(t, "origin"), "   ", false, false)
				return err
			},
			wantErr: "branch is required",
		},
		{
			name: "compacting push requires branch",
			run: func() error {
				_, err := syncStore.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "   ", unfiredBound(t), false, false)
				return err
			},
			wantErr: "branch is required",
		},
	}

	for _, tc := range testCases {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s error = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestAFailedCheckAfterARefusalDoesNotReclassifyIt pins that a refused push
// keeps its own class when the check after it failed: a follow that met a
// busy store or a cancellation is named in the text, but errors.Is answers
// for the refusal alone, so the push-outcome record never calls a refusal
// busy or canceled.
func TestAFailedCheckAfterARefusalDoesNotReclassifyIt(t *testing.T) {
	t.Parallel()
	refusal := pushRefusedError{remote: "origin", cause: errors.New("permission denied")}
	for _, check := range []error{
		fmt.Errorf("fetch: %w", ErrWorkspaceBusy),
		fmt.Errorf("fetch: %w", context.Canceled),
	} {
		for name, joined := range map[string]error{
			"joinFollowFailure": joinFollowFailure(refusal, check),
			"unjudgedRefusal":   unjudgedRefusal(refusal, check),
		} {
			if errors.Is(joined, ErrWorkspaceBusy) || errors.Is(joined, context.Canceled) {
				t.Errorf("%s(refusal, %v) matches the check's class: %v", name, check, joined)
			}
			if !errors.As(joined, new(pushRefusedError)) || !strings.Contains(joined.Error(), check.Error()) {
				t.Errorf("%s(refusal, %v) = %v; want the refusal, naming the check", name, check, joined)
			}
		}
	}
}

// TestSyncCompactAndPushRefusesABlankBranchBeforeAnyWork pins where the
// compacting push refuses its arguments: before the commit lock and the
// compaction under it. A context already canceled fails any work that
// starts, so only a refusal that runs first reports the blank branch.
func TestSyncCompactAndPushRefusesABlankBranchBeforeAnyWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := st.SyncCompactAndPush(canceled, canceled, remoteName(t, "origin"), "   ", unfiredBound(t), false, false)
	if err == nil || err.Error() != "branch is required" {
		t.Fatalf("SyncCompactAndPush(blank branch) error = %v, want only %q", err, "branch is required")
	}
}

func TestSyncCompactRunsCleanlyAndPreservesData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "gc epic", Topic: "gc-test", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "gc target", Topic: "gc-test", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	// Both depths, and each twice: the second call proves a depth is
	// re-runnable against a store it already collected, which is the shape a
	// backstop invoking it on a cadence actually exercises.
	for _, mode := range []GCMode{GCNewGen, GCFull} {
		outcome, err := syncStore.SyncCompact(ctx, mode)
		if err != nil {
			t.Fatalf("SyncCompact(%v) error = %v", mode, err)
		}
		if !outcome.Ran || outcome.Depth != mode {
			t.Fatalf("SyncCompact(%v) outcome = %+v, want a pass at that depth", mode, outcome)
		}
		if outcome.Detail == "" {
			t.Fatalf("SyncCompact(%v) reported no detail; a caller cannot tell what was reclaimed", mode)
		}
		if _, err := syncStore.SyncCompact(ctx, mode); err != nil {
			t.Fatalf("second SyncCompact(%v) error = %v", mode, err)
		}
	}

	// A freshly compacted store is under every threshold, so the backstop's own
	// entrypoint must decline rather than collect again — that decline is what
	// keeps it cheap to ask often.
	idle, err := syncStore.MaintainIfDue(ctx)
	if err != nil {
		t.Fatalf("MaintainIfDue() on a just-compacted store error = %v", err)
	}
	if idle.Compaction.Ran {
		t.Fatalf("MaintainIfDue() ran a pass on a just-compacted store: %+v", idle)
	}

	got, err := syncStore.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() after compact error = %v", err)
	}
	if got.Title != "gc target" {
		t.Fatalf("GetIssue() after compact title = %q, want %q", got.Title, "gc target")
	}
}

// The contract's door guard, exercised where callers actually enter. An illegal
// depth reaching the pass would collect at the shallower default and report
// success — the wrong work done quietly, which is worse than a refusal because
// nothing downstream can tell it happened. [LAW:no-silent-failure]
func TestSyncCompactRefusesAnIllegalDepth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "illegal-depth-ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	outcome, err := syncStore.SyncCompact(ctx, GCMode(99))
	if err == nil {
		t.Fatal("SyncCompact() accepted a depth outside the contract; it must refuse rather than collect at one nobody named")
	}
	if outcome.Ran {
		t.Fatalf("SyncCompact() reported a pass it refused to run: %+v", outcome)
	}
	// A refusal at the door carries no depth at all, so an outcome never hands a
	// reader an illegal one to render. The depth it was asked for is named in the
	// error, where it belongs.
	if outcome.Depth.Valid() {
		t.Fatalf("a refused depth surfaced on the outcome as %v; an outcome must carry a legal depth or none", outcome.Depth)
	}
}

// compactWithinLock is the only place that can observe whether DOLT_GC landed,
// so the attempt it returns is the sole carrier of that fact — everything above
// it sees one error and cannot tell a pass that never ran from a pass that ran
// and was then followed by a failure.
//
// Both reachable arms are pinned here: a real pass reports Ran, and a depth Dolt
// has no spelling for reports its depth without it. The third arm — DOLT_GC
// completing and the reconnect after it failing — has no fault-injection seam in
// this engine and is not driven from here. It differs from the tested success
// arm by nothing but reconnect's return value, and the outcome it produces (Ran
// set alongside an error) is driven end to end in the cli tests, which is where
// its consequence lives.
func TestCompactWithinLockReportsWhetherThePassLanded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "compaction-attempt-ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	var landed compactionAttempt
	if err := syncStore.runSyncMutation(ctx, func(ctx context.Context) error {
		var compactErr error
		landed, _, compactErr = syncStore.compactWithinLock(ctx, GCNewGen)
		return compactErr
	}); err != nil {
		t.Fatalf("compactWithinLock() error = %v", err)
	}
	if !landed.Ran {
		t.Fatal("a completed pass reported Ran = false; every caller downstream then reads a real rewrite as a pass that never happened")
	}
	if landed.Depth != GCNewGen {
		t.Fatalf("attempt depth = %v, want %v", landed.Depth, GCNewGen)
	}

	var refused compactionAttempt
	err = syncStore.runSyncMutation(ctx, func(ctx context.Context) error {
		var compactErr error
		refused, _, compactErr = syncStore.compactWithinLock(ctx, GCMode(99))
		return compactErr
	})
	if err == nil {
		t.Fatal("compactWithinLock() accepted a depth Dolt has no spelling for")
	}
	if refused.Ran {
		t.Fatal("a pass that never reached DOLT_GC reported Ran = true; the push path would then announce a collection that never happened")
	}
	if refused.Depth != GCMode(99) {
		t.Fatalf("a refused attempt lost its depth (%v); the trail needs to say which one was asked for", refused.Depth)
	}
}

// TestMeasureCompactionSignalsMatchesDoltsRealOldGenLayout is the pin that makes
// oldGenDirName and archiveFileExt safe to ship. Dolt exports no constant for
// either, so lit spells them itself — and every other footprint test writes the
// layout it then reads using those same two constants, so fixture and code
// share one map and would agree with each other even if both were wrong about
// Dolt. This is the test that checks the map against the territory: it runs a
// REAL pass, lets Dolt lay the old generation down itself, and asserts the two
// names describe what Dolt actually wrote.
//
// The failure it guards is silent, which is what earns it a real store. If an
// embedded-Dolt bump moved either name, OldGenArchives would read 0 forever,
// archivesDueCount would never fire, and the deep pass would simply stop
// happening — no error, no red test, just a backstop quietly doing nothing.
// [LAW:no-silent-failure] The same pinning TestRemoteCacheKeyMatchesDoltLayout
// does for the remote-cache key.
func TestMeasureCompactionSignalsMatchesDoltsRealOldGenLayout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "layout-ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	// Enough history that a pass has something to archive; an empty store would
	// leave an old generation Dolt never wrote, proving nothing.
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "layout epic", Topic: "layout", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	for i := range 8 {
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("layout %d", i), Topic: "layout", IssueType: "task", Priority: 0, ParentID: epic.ID,
		}); err != nil {
			t.Fatalf("CreateIssue() error = %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "layout-ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()
	if _, err := syncStore.SyncCompact(ctx, GCNewGen); err != nil {
		t.Fatalf("SyncCompact(GCNewGen) error = %v", err)
	}

	// Everything below DISCOVERS the layout from disk. nomsDir is derived from
	// Dolt's own exported dbfactory constants, so it is not part of the claim
	// under test; oldGenDirName and archiveFileExt appear only as the thing
	// being checked, never as the way anything is found.
	noms := nomsDir(doltRoot)
	entries, err := os.ReadDir(noms)
	if err != nil {
		t.Fatalf("read noms dir: %v", err)
	}
	var subdirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			subdirs = append(subdirs, entry.Name())
		}
	}
	if len(subdirs) != 1 {
		t.Fatalf("noms holds subdirectories %v; expected exactly one (the old generation) to pin against", subdirs)
	}
	if subdirs[0] != oldGenDirName {
		t.Fatalf("Dolt's old-generation directory is %q, but oldGenDirName says %q — the footprint counts a path Dolt does not write", subdirs[0], oldGenDirName)
	}

	// Dolt's manifest and LOCK carry no suffix; everything suffixed in here is
	// an archive. Asserting the suffix rather than filtering by it is what
	// catches a rename.
	oldgen, err := os.ReadDir(filepath.Join(noms, subdirs[0]))
	if err != nil {
		t.Fatalf("read old generation: %v", err)
	}
	archives := 0
	for _, entry := range oldgen {
		ext := filepath.Ext(entry.Name())
		if entry.IsDir() || ext == "" {
			continue
		}
		if ext != archiveFileExt {
			t.Fatalf("Dolt wrote %q in the old generation, but archiveFileExt says %q", entry.Name(), archiveFileExt)
		}
		archives++
	}
	if archives == 0 {
		t.Fatal("a real pass left no archive in the old generation; this test can prove nothing about a layout Dolt did not write")
	}

	got, err := measureCompactionSignals(doltRoot)
	if err != nil {
		t.Fatalf("measureCompactionSignals() error = %v", err)
	}
	if got.OldGenArchives != archives {
		t.Fatalf("OldGenArchives = %d, but Dolt wrote %d archive(s) — the signals and the disk disagree", got.OldGenArchives, archives)
	}
}

func TestValidateEmbeddedSyncSupportAcceptsRequiredVersions(t *testing.T) {
	t.Parallel()
	err := validateEmbeddedSyncSupport(map[string]string{
		"github.com/dolthub/dolt/go": minEmbeddedDoltVersion,
		"github.com/dolthub/driver":  minEmbeddedDriverVersion,
	})
	if err != nil {
		t.Fatalf("validateEmbeddedSyncSupport() error = %v", err)
	}
}

func TestValidateEmbeddedSyncSupportRejectsOlderVersions(t *testing.T) {
	t.Parallel()
	err := validateEmbeddedSyncSupport(map[string]string{
		"github.com/dolthub/dolt/go": "v0.40.5-0.20240702155756-bcf4dd5f5cc1",
		"github.com/dolthub/driver":  "v0.2.0",
	})
	if err == nil {
		t.Fatal("validateEmbeddedSyncSupport() error = nil, want version failure")
	}
	if !strings.Contains(err.Error(), "embedded sync requires") {
		t.Fatalf("validateEmbeddedSyncSupport() error = %v, want embedded sync guidance", err)
	}
}

func TestSyncFreshnessStateClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   storage.SyncFreshness
		want storage.SyncFreshnessState
	}{
		{"never synced ignores counts", storage.SyncFreshness{Synced: false, Ahead: 0, Behind: 0}, storage.SyncNeverSynced},
		{"up to date", storage.SyncFreshness{Synced: true, Ahead: 0, Behind: 0}, storage.SyncUpToDate},
		{"ahead only", storage.SyncFreshness{Synced: true, Ahead: 2, Behind: 0}, storage.SyncAhead},
		{"behind only", storage.SyncFreshness{Synced: true, Ahead: 0, Behind: 3}, storage.SyncBehind},
		{"diverged", storage.SyncFreshness{Synced: true, Ahead: 2, Behind: 3}, storage.SyncDiverged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.State(); got != tc.want {
				t.Fatalf("State() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSyncFreshnessRequiresBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	st, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	if _, err := st.SyncFreshness(ctx, remoteName(t, "origin"), "  "); err == nil || !strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("SyncFreshness with blank branch error = %v, want branch required", err)
	}
}

// TestSyncFreshnessTracksAheadBehindAgainstRemote drives a real file-backed
// remote through every freshness state so the dolt_log range counting and the
// tracking-ref guard are proven against the embedded engine, not asserted in a
// vacuum.
func TestSyncFreshnessTracksAheadBehindAgainstRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	epicID := seedSyncEpic(t, ctx, doltRoot, "ws")

	commit := func(title string) {
		st, err := Open(ctx, testEnv(), doltRoot, "ws")
		if err != nil {
			t.Fatalf("Open(%q) error = %v", title, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0, ParentID: epicID}); err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", title, err)
		}
	}

	assertFreshness := func(label string, sync *Store, wantState storage.SyncFreshnessState, wantAhead, wantBehind int64) {
		t.Helper()
		got, err := sync.SyncFreshness(ctx, remoteName(t, "origin"), "master")
		if err != nil {
			t.Fatalf("%s: SyncFreshness() error = %v", label, err)
		}
		if got.State() != wantState || got.Ahead != wantAhead || got.Behind != wantBehind {
			t.Fatalf("%s: freshness = %+v state=%q, want state=%q ahead=%d behind=%d", label, got, got.State(), wantState, wantAhead, wantBehind)
		}
		// The divergence timestamp dates the fork: it is populated exactly when the
		// branch is DIVERGED (commits on both sides), and absent otherwise — an
		// ahead-only or behind-only branch is not diverged, so the field a divergence
		// names stays 0. This exercises the real UNIX_TIMESTAMP(MIN(date)) query.
		wantHasAge := wantAhead > 0 && wantBehind > 0
		if hasAge := got.OldestDivergedUnix > 0; hasAge != wantHasAge {
			t.Fatalf("%s: OldestDivergedUnix=%d, want populated=%v (ahead=%d behind=%d)", label, got.OldestDivergedUnix, wantHasAge, wantAhead, wantBehind)
		}
	}

	commit("c1")

	sync, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	// Remote configured but never pushed/fetched: tracking ref absent.
	assertFreshness("never synced", sync, storage.SyncNeverSynced, 0, 0)

	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(c1) error = %v", err)
	}
	var c1Hash string
	if err := sync.db.QueryRowContext(ctx, `SELECT hash FROM dolt_branches WHERE name = 'master'`).Scan(&c1Hash); err != nil {
		t.Fatalf("read c1 hash error = %v", err)
	}
	assertFreshness("after first push", sync, storage.SyncUpToDate, 0, 0)
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() after push error = %v", err)
	}

	// Local commit not pushed: ahead by 1.
	commit("c2")
	sync, err = OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after c2 error = %v", err)
	}
	assertFreshness("after local commit", sync, storage.SyncAhead, 1, 0)

	// Publish c2 so the remote-tracking ref advances to c2, then rewind the
	// local branch to c1: the tracking ref is now ahead of local → behind by 1.
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(c2) error = %v", err)
	}
	assertFreshness("after publishing c2", sync, storage.SyncUpToDate, 0, 0)
	if _, err := sync.db.ExecContext(ctx, `CALL DOLT_RESET('--hard', ?)`, c1Hash); err != nil {
		t.Fatalf("DOLT_RESET to c1 error = %v", err)
	}
	assertFreshness("after rewind to c1", sync, storage.SyncBehind, 0, 1)

	// New local commit on top of the rewound branch: c3 is not on the remote and
	// c2 is not local → diverged.
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() before c3 error = %v", err)
	}
	commit("c3")
	sync, err = OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after c3 error = %v", err)
	}
	assertFreshness("after divergent commit", sync, storage.SyncDiverged, 1, 1)
	if err := sync.Close(); err != nil {
		t.Fatalf("Close() after divergence error = %v", err)
	}

	// `lit doctor` opens the store read-only, so prove SyncFreshness's queries
	// (dolt_remote_branches, ACTIVE_BRANCH, dolt_log) run on an OpenForRead store
	// and not just the sync store the rest of this test drives.
	readStore, err := OpenForRead(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer readStore.Close()
	assertFreshness("read-only store", readStore, storage.SyncDiverged, 1, 1)
}

// TestSyncPushDelivers proves SyncPush delivers every commit to the remote on
// its own, without composing compaction. This is the load-bearing claim behind
// the on-change mirror, which pushes without DOLT_GC: garbage collection only
// reclaims local chunks, so a bare push must still deliver the full history. The
// remote is a real file-backed Dolt remote, so "delivered" means the tracking
// ref advanced to match local, not a stub.
func TestSyncPushDelivers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	epicID := seedSyncEpic(t, ctx, doltRoot, "ws")

	commit := func(title string) {
		st, err := Open(ctx, testEnv(), doltRoot, "ws")
		if err != nil {
			t.Fatalf("Open(%q) error = %v", title, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0, ParentID: epicID}); err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", title, err)
		}
	}

	commit("c1")
	sync, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	// First push seeds the remote and the tracking ref.
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(set-upstream) error = %v", err)
	}
	got, err := sync.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness() error = %v", err)
	}
	if got.State() != storage.SyncUpToDate {
		t.Fatalf("after no-compact push: state = %q (%+v), want up-to-date", got.State(), got)
	}
	// Close before the next commit/OpenSync round: embedded Dolt permits only
	// one read-write engine per path (links-sync-pgct.11), and this test's
	// own commit() helper opens a fresh Open() next — sync must release
	// first, exactly as a real command sequence would.
	if err := sync.Close(); err != nil {
		t.Fatalf("sync.Close() error = %v", err)
	}

	// A later commit pushed without compaction must also reach the remote.
	commit("c2")
	sync2, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after c2 error = %v", err)
	}
	defer sync2.Close()
	if freshness, _ := sync2.SyncFreshness(ctx, remoteName(t, "origin"), "master"); freshness.Ahead != 1 {
		t.Fatalf("before second push: ahead = %d, want 1", freshness.Ahead)
	}
	if _, err := sync2.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(c2) error = %v", err)
	}
	after, err := sync2.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness() after c2 push error = %v", err)
	}
	if after.State() != storage.SyncUpToDate {
		t.Fatalf("after second no-compact push: state = %q (%+v), want up-to-date", after.State(), after)
	}
}

// TestSyncCompactAndPushDelivers proves the explicit command's push variant
// compacts and delivers in one call. The atomicity (compact + push under one
// commit lock) is structural in SyncCompactAndPush; this pins that the composed
// operation still reaches the remote, against a real file-backed Dolt remote.
func TestSyncCompactAndPushDelivers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	st, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "c1", Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	got, err := sync.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness() error = %v", err)
	}
	if got.State() != storage.SyncUpToDate {
		t.Fatalf("after compact+push: state = %q (%+v), want up-to-date", got.State(), got)
	}
}

// TestSyncCompactAndPushDeepensOnAFragmentedOldGeneration drives the push
// path's escalation, which nothing else in the suite reaches: every other
// SyncCompactAndPush test uses a fresh store, so chooseCompactionDepth always
// takes its GCNewGen floor and the deep branch never runs.
// TestDueModeSelectsDepthByFootprint covers the decision as a pure function of
// two numbers; this covers the composition that carries a decision through to
// the operator — chooseCompactionDepth into compactWithinLock into
// compactionReport into result.Maintenance — which is where a miswiring would
// live and where the pure test cannot see.
//
// The old generation is seeded because crossing archivesDueCount honestly would
// take hundreds of passes. Dolt tolerates entries absent from its manifest, so
// the push still succeeds and the escalation is the only thing under test.
// [LAW:behavior-not-structure] the assertion is what the operator is told, not
// which internal chose it.
func TestSyncCompactAndPushDeepensOnAFragmentedOldGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	st, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "fragmented", Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID,
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	oldgen := filepath.Join(nomsDir(doltRoot), oldGenDirName)
	if err := os.MkdirAll(oldgen, 0o755); err != nil {
		t.Fatalf("make old generation: %v", err)
	}
	for i := range archivesDueCount {
		if err := os.WriteFile(filepath.Join(oldgen, "seeded"+itoa(i)+archiveFileExt), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
	}

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()
	if err := syncStore.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	result, err := syncStore.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false)
	if err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	if !strings.Contains(result.Maintenance, "full pass") {
		t.Fatalf("Maintenance = %q, want it to report the deep pass the footprint owed", result.Maintenance)
	}
}

// A pass that completed inside a call whose push then failed is still a pass
// that rewrote the store — the push failing afterwards does not un-rewrite it.
// Reporting maintenance only on the success path would lose a deep collection
// whenever the push it precedes fails, leaving an operator with "push failed"
// and no account of the long full-store rewrite that just happened, which is
// also the only thing explaining why the failed attempt took so long.
// [LAW:no-silent-failure]
//
// The push is failed by naming a remote that was never added, so the failure
// lands in pushWithinLock — after compactWithinLock has already run inside the
// same closure, which is the ordering that makes that loss possible.
func TestSyncCompactAndPushNamesADeepPassInsideAFailedPush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "doomed push", Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID,
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	oldgen := filepath.Join(nomsDir(doltRoot), oldGenDirName)
	if err := os.MkdirAll(oldgen, 0o755); err != nil {
		t.Fatalf("make old generation: %v", err)
	}
	for i := range archivesDueCount {
		if err := os.WriteFile(filepath.Join(oldgen, "seeded"+itoa(i)+archiveFileExt), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
	}

	syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	_, err = syncStore.SyncCompactAndPush(ctx, ctx, remoteName(t, "no-such-remote"), "master", unfiredBound(t), false, false)
	if err == nil {
		t.Fatal("SyncCompactAndPush() to a remote that does not exist succeeded; this test cannot say anything unless the push fails")
	}
	if !strings.Contains(err.Error(), "full pass") {
		t.Fatalf("error = %v, want it to also name the deep pass that already rewrote the store before the push failed", err)
	}
}

// TestReconnectRotatorRecoversPoisonedOperation proves, end to end against a
// REAL store, that when an operation fails with Dolt's online-GC
// connection-reset error, the retry boundary rotates the live connection via
// the real s.reconnect and the subsequent attempt succeeds on the fresh handle.
// The CLI race that produces this error is timing-dependent and cannot be
// summoned on demand, so this injects the exact Dolt error string at the seam
// and asserts the recovery machinery — reconnect + retry — actually makes a
// real store usable again. A post-recovery write confirms the rotated handle is
// fully functional.
func TestReconnectRotatorRecoversPoisonedOperation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}

	// The exact Dolt ErrServerPerformedGC text (verified against the pinned
	// module), wrapped the way callIntProcedure would surface it.
	gcReset := errors.New("compact dolt store: Error 1105: this connection was established when this server performed an online garbage collection. this connection can no longer be used. please reconnect.")

	dbBefore := st.db
	attempts := 0
	err = st.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientGCContention(ctx, func(context.Context) error {
			attempts++
			if attempts == 1 {
				return gcReset
			}
			return nil
		}, st.reconnect)
	})
	if err != nil {
		t.Fatalf("retry with real reconnect rotator error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one poisoned, one post-reconnect success)", attempts)
	}
	if st.db == dbBefore {
		t.Fatal("s.db was not rotated; reconnect rotator did not run")
	}

	// The rotated connection must be a fully working handle, not just non-nil:
	// a real mutation through it has to commit.
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "after-reconnect", Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue() after reconnect error = %v", err)
	}
}

// TestStagedWorkingSetSurvivesReconnect proves the load-bearing safety property
// of routing commitWorkingSet's retry through a reconnect: the staged working set
// must survive the connection rotation. If Dolt's working set were connection-
// local, rotating the handle between staging a mutation and DOLT_COMMIT would
// silently drop the change — the commit would find "nothing to commit" and the
// mutation would vanish with no error. [LAW:no-silent-failure] This stages a
// write, reconnects, commits on the fresh handle, then reads it back through a
// brand-new Open (its own engine) to confirm the change is durably committed.
func TestStagedWorkingSetSurvivesReconnect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	// Staged, but deliberately not DOLT_COMMIT'd — so the reconnect below lands
	// between the staged write and its Dolt commit.
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := st.setMeta(ctx, tx, "reconnect_probe", "survived"); err != nil {
		t.Fatalf("setMeta() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit() error = %v", err)
	}

	// Rotate the connection between the staged write and the Dolt commit — the
	// exact sequence the GC-contention retry performs.
	if err := st.reconnect(ctx); err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	if err := st.commitWorkingSetOnce(ctx, commitStamp{Message: "commit staged probe after reconnect"}); err != nil {
		t.Fatalf("commitWorkingSetOnce() after reconnect error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A brand-new Open (fresh engine) must see the committed value. If the staged
	// write had been lost on reconnect, the commit above would have been a no-op
	// and this read returns empty.
	reopened, err := Open(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("reopen Open() error = %v", err)
	}
	defer reopened.Close()
	got, err := reopened.getMeta(ctx, nil, "reconnect_probe")
	if err != nil {
		t.Fatalf("getMeta() error = %v", err)
	}
	if got != "survived" {
		t.Fatalf("reconnect_probe = %q, want \"survived\" (staged working set lost across reconnect)", got)
	}
}

// TestSyncResetToRemoteHeadAdoptsUnrelatedHistory drives the bootstrap adopt
// path against a real file-backed remote: a producer pushes a ticket, then a
// brand-new store (its own unrelated bootstrap root) fetches and adopts the
// remote head. It proves the producer's ticket lands locally and that a
// subsequent regular Open — which runs migrations — reads it, the real
// post-`lit init` usage path. A plain SyncPull cannot do this; it fails with
// "no common ancestor" across the unrelated roots, which is why adopt resets.
func TestSyncResetToRemoteHeadAdoptsUnrelatedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	remoteURL := "file://" + filepath.Join(base, "remote")

	// Producer: a real workspace with a ticket inside an epic, pushed to the remote.
	producerRoot := migratedDoltDir(t)
	producer, err := Open(ctx, testEnv(), producerRoot, "ws-producer")
	if err != nil {
		t.Fatalf("Open(producer) error = %v", err)
	}
	epic, err := producer.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "remote-epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(producer epic) error = %v", err)
	}
	if _, err := producer.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "remote-ticket", Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue(producer) error = %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatalf("Close(producer) error = %v", err)
	}
	producerSync, err := OpenSync(ctx, testEnv(), producerRoot, "ws-producer")
	if err != nil {
		t.Fatalf("OpenSync(producer) error = %v", err)
	}
	if err := producerSync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(producer) error = %v", err)
	}
	if _, err := producerSync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(producer) error = %v", err)
	}
	if err := producerSync.Close(); err != nil {
		t.Fatalf("Close(producerSync) error = %v", err)
	}

	// Consumer: a brand-new store with an unrelated bootstrap root.
	consumerRoot := unrelatedDoltDir(t)
	consumer, err := OpenSync(ctx, testEnv(), consumerRoot, "ws-consumer")
	if err != nil {
		t.Fatalf("OpenSync(consumer) error = %v", err)
	}
	if err := consumer.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) error = %v", err)
	}
	if err := consumer.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(consumer) error = %v", err)
	}
	// The tracking ref exists post-fetch, so the consumer reports Synced — the
	// signal init uses to decide there is remote data to adopt.
	freshness, err := consumer.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness(consumer) error = %v", err)
	}
	if !freshness.Synced {
		t.Fatalf("consumer freshness Synced = false, want true (remote carries data): %+v", freshness)
	}
	if err := consumer.SyncResetToRemoteHead(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead() error = %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("Close(consumer) error = %v", err)
	}

	// A regular Open runs migrations on the adopted store and reads the ticket
	// and the epic it lives in.
	adopted, err := Open(ctx, testEnv(), consumerRoot, "ws-consumer")
	if err != nil {
		t.Fatalf("Open(consumer after adopt) error = %v", err)
	}
	defer adopted.Close()
	issues, err := adopted.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues(consumer) error = %v", err)
	}
	titles := make([]string, 0, len(issues))
	for _, issue := range issues {
		titles = append(titles, issue.Title)
	}
	slices.Sort(titles)
	if !slices.Equal(titles, []string{"remote-epic", "remote-ticket"}) {
		t.Fatalf("adopted issues = %+v, want exactly [remote-epic remote-ticket]", issues)
	}
}

// TestLocalIssueCountAcrossLifecycle pins the adopt-safety signal at each store
// lifecycle stage: a pristine sync store (no baseline migration, issues table
// absent) reports 0, a migrated store with no tickets still reports 0, and a
// store with a ticket reports its count.
func TestLocalIssueCountAcrossLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// From nothing on purpose: the pristine stage exists to reach the
	// issues-table-absent short-circuit, which a pre-migrated template copy
	// can never exercise.
	root := filepath.Join(t.TempDir(), "dolt")

	pristine, err := OpenSync(ctx, testEnv(), root, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if got, err := pristine.LocalIssueCount(ctx); err != nil || got != 0 {
		t.Fatalf("pristine LocalIssueCount() = %d, %v; want 0, nil", got, err)
	}
	if err := pristine.Close(); err != nil {
		t.Fatalf("Close(pristine) error = %v", err)
	}

	st, err := Open(ctx, testEnv(), root, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got, err := st.LocalIssueCount(ctx); err != nil || got != 0 {
		t.Fatalf("migrated-empty LocalIssueCount() = %d, %v; want 0, nil", got, err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "e1", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if got, err := st.LocalIssueCount(ctx); err != nil || got != 1 {
		t.Fatalf("after-epic LocalIssueCount() = %d, %v; want 1, nil", got, err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "t1", Topic: "topic", IssueType: "task", Priority: 0, ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if got, err := st.LocalIssueCount(ctx); err != nil || got != 2 {
		t.Fatalf("after-ticket LocalIssueCount() = %d, %v; want 2, nil", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestSyncResetToRemoteHeadRequiresBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	st, err := OpenSync(ctx, testEnv(), doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer st.Close()
	if err := st.SyncResetToRemoteHead(ctx, remoteName(t, "origin"), "  "); err == nil || !strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("blank branch error = %v, want branch required", err)
	}
}

func TestGitBackedRemoteURL(t *testing.T) {
	t.Parallel()
	// A path rooted at a separator is local: on POSIX it is absolute, and on
	// Windows it names a directory on the current drive, so it gains that drive.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rooted := "git+file://" + filepath.ToSlash(filepath.VolumeName(wd))
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://github.com/org/repo.git", "git+https://github.com/org/repo.git"},
		{"https without .git", "https://github.com/org/repo", "git+https://github.com/org/repo"},
		{"http without .git", "http://example.com/org/repo", "git+http://example.com/org/repo"},
		{"scp-like with .git", "git@github.com:org/repo.git", "git+ssh://git@github.com/./org/repo.git"},
		{"scp-like without .git", "git@github.com:org/repo", "git+ssh://git@github.com/./org/repo"},
		{"ssh url with .git", "ssh://git@github.com/org/repo.git", "git+ssh://git@github.com/org/repo.git"},
		{"file url", "file:///srv/git/repo.git", "git+file:///srv/git/repo.git"},
		{"local absolute path with .git", "/srv/git/repo.git", rooted + "/srv/git/repo.git"},
		{"local absolute path without .git", "/srv/git/repo", rooted + "/srv/git/repo"},
		{"schemeless host without .git", "github.com/org/repo", "git+https://github.com/org/repo"},
		{"already git+ prefixed", "git+https://github.com/org/repo", "git+https://github.com/org/repo"},
		{"empty", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitBackedRemoteURL(tc.in); got != tc.want {
				t.Fatalf("GitBackedRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A bare relative remote path is a local repository to git, while Dolt's own
// normalizer reads it as an https host. workspace joins it to the checkout
// root before it gets here, so Dolt is handed an absolute path and pushes
// over git+file.
func TestABareRelativeRemoteReachesDoltAsALocalPath(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := workspace.ParseCheckoutRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := workspace.NewGitRemote("origin", "remote.git", root).SyncableRemote()
	if err != nil {
		t.Fatalf("SyncableRemote() error = %v", err)
	}
	got := GitBackedRemoteURL(remote.URL())
	if want := GitBackedRemoteURL(filepath.Join(dir, "remote.git")); got != want || !strings.HasPrefix(got, "git+file://") {
		t.Fatalf("GitBackedRemoteURL(%q) = %q, want the git+file URL of the joined path, %q", remote.URL(), got, want)
	}
	if got := GitBackedRemoteURL("remote.git"); got != "git+https://remote.git" {
		t.Fatalf("GitBackedRemoteURL(remote.git) unjoined = %q, want Dolt's host reading, which the join exists to avoid", got)
	}
}

func TestGitBackedRemoteURLIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"https://github.com/org/repo",
		"git@github.com:org/repo.git",
		"/srv/git/repo.git",
	} {
		once := GitBackedRemoteURL(in)
		twice := GitBackedRemoteURL(once)
		if once != twice {
			t.Fatalf("GitBackedRemoteURL not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestGitBackedRemoteURLRoundTripsThroughDolt guards the no-churn invariant in the
// sync reconciliation loop: the URL lit computes for a git remote must equal the URL
// Dolt stores when that same value is added, or every reconcile would remove+re-add
// the remote forever. Covers the suffix-less and local-path forms that a pure unit
// test of the string output cannot prove against the real store.
func TestGitBackedRemoteURLRoundTripsThroughDolt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, raw := range []string{
		"https://github.com/org/repo.git",
		"https://github.com/org/repo",
		"git@github.com:org/repo.git",
		"git@github.com:org/repo",
		"/srv/git/repo.git",
		"/srv/git/repo",
	} {
		t.Run(raw, func(t *testing.T) {
			doltRoot := migratedDoltDir(t)
			st, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			syncStore, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
			if err != nil {
				t.Fatalf("OpenSync() error = %v", err)
			}
			defer syncStore.Close()

			desired := GitBackedRemoteURL(raw)
			if err := syncStore.SyncAddRemote(ctx, remoteName(t, "origin"), desired); err != nil {
				t.Fatalf("SyncAddRemote(%q) error = %v", desired, err)
			}
			remotes, err := syncStore.SyncListRemotes(ctx)
			if err != nil {
				t.Fatalf("SyncListRemotes() error = %v", err)
			}
			if len(remotes) != 1 {
				t.Fatalf("remotes = %#v, want 1", remotes)
			}
			if remotes[0].URL != desired {
				t.Fatalf("stored URL = %q, want %q (would cause reconcile churn)", remotes[0].URL, desired)
			}
		})
	}
}

// TestSyncReceiveFastForwardsWhenBehindAndDefersDivergence drives a real
// file-remote two-clone scenario through every freshness state and pins
// SyncReceive's contract: it fast-forwards a strictly-behind clone (the only
// data-mutating state), is a no-op when up-to-date or ahead, and — critically —
// reports a divergence WITHOUT merging it (that is the foreground reconcile's
// job), so no divergent local work is ever touched by the background receive.
func TestSyncReceiveFastForwardsWhenBehindAndDefersDivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	// A's epic is the one every issue in this test lives in; B holds it once it
	// adopts the remote, so B's own commit files under it too.
	epicID := seedSyncEpic(t, ctx, rootA, "ws")
	createIssue := func(root, title string) {
		t.Helper()
		st, err := Open(ctx, testEnv(), root, "ws")
		if err != nil {
			t.Fatalf("Open(%s,%q) error = %v", root, title, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", ParentID: epicID}); err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close after CreateIssue(%q) error = %v", title, err)
		}
	}
	issueCount := func(root string) int {
		t.Helper()
		st, err := OpenForRead(ctx, testEnv(), root, "ws")
		if err != nil {
			t.Fatalf("OpenForRead(%s) error = %v", root, err)
		}
		defer st.Close()
		list, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
		if err != nil {
			t.Fatalf("ListIssues(%s) error = %v", root, err)
		}
		return len(list)
	}
	pushFrom := func(root string, setUpstream bool) {
		t.Helper()
		st, err := OpenSync(ctx, testEnv(), root, "ws")
		if err != nil {
			t.Fatalf("OpenSync(%s) for push error = %v", root, err)
		}
		if _, err := st.SyncPush(ctx, remoteName(t, "origin"), "master", setUpstream, false); err != nil {
			t.Fatalf("SyncPush(%s) error = %v", root, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%s) after push error = %v", root, err)
		}
	}
	receive := func(label string, wantState storage.SyncReceiveState) storage.SyncReceiveResult {
		t.Helper()
		st, err := OpenSync(ctx, testEnv(), rootB, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync(B) error = %v", label, err)
		}
		defer st.Close()
		res, err := st.SyncReceive(ctx, remoteName(t, "origin"), "master")
		if err != nil {
			t.Fatalf("%s: SyncReceive() error = %v", label, err)
		}
		if res.State != wantState {
			t.Fatalf("%s: receive state = %q (ahead=%d behind=%d), want %q", label, res.State, res.Ahead, res.Behind, wantState)
		}
		return res
	}

	// A seeds the remote with its epic and one issue.
	createIssue(rootA, "c1")
	syncA, err := OpenSync(ctx, testEnv(), rootA, "ws")
	if err != nil {
		t.Fatalf("OpenSync(A) error = %v", err)
	}
	if err := syncA.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(A) error = %v", err)
	}
	if _, err := syncA.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(A c1) error = %v", err)
	}
	if err := syncA.Close(); err != nil {
		t.Fatalf("Close(A) error = %v", err)
	}

	// B adopts the remote: it now has the epic and c1 and is up to date.
	syncB, err := OpenSync(ctx, testEnv(), rootB, "ws")
	if err != nil {
		t.Fatalf("OpenSync(B) error = %v", err)
	}
	if err := syncB.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(B) error = %v", err)
	}
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B) error = %v", err)
	}
	if err := syncB.SyncResetToRemoteHead(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead(B) error = %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B) after adopt error = %v", err)
	}
	if got := issueCount(rootB); got != 2 {
		t.Fatalf("B issue count after adopt = %d, want 2", got)
	}

	// Up to date: receive is a no-op.
	receive("up to date", storage.SyncReceiveUpToDate)
	if got := issueCount(rootB); got != 2 {
		t.Fatalf("B issue count after up-to-date receive = %d, want 2", got)
	}

	// A pushes c2: B is now behind by one. Receive fast-forwards it.
	createIssue(rootA, "c2")
	pushFrom(rootA, false)
	res := receive("behind", storage.SyncReceiveFastForwarded)
	if res.Behind != 1 {
		t.Fatalf("behind receive: Behind = %d, want 1", res.Behind)
	}
	if got := issueCount(rootB); got != 3 {
		t.Fatalf("B issue count after fast-forward = %d, want 3", got)
	}

	// B makes a local, unpushed commit: it is ahead, remote unchanged. No-op.
	createIssue(rootB, "b-local")
	res = receive("ahead", storage.SyncReceiveAhead)
	if res.Ahead != 1 {
		t.Fatalf("ahead receive: Ahead = %d, want 1", res.Ahead)
	}
	if got := issueCount(rootB); got != 4 {
		t.Fatalf("B issue count after ahead receive = %d, want 4", got)
	}

	// A pushes c3 while B holds its unpushed commit: B is diverged. Receive must
	// report it and NOT merge — B's four issues are untouched.
	createIssue(rootA, "c3")
	pushFrom(rootA, false)
	res = receive("diverged", storage.SyncReceiveDiverged)
	if res.Ahead != 1 || res.Behind != 1 {
		t.Fatalf("diverged receive: ahead=%d behind=%d, want 1/1", res.Ahead, res.Behind)
	}
	if got := issueCount(rootB); got != 4 {
		t.Fatalf("B issue count after diverged receive = %d, want 4 (no merge)", got)
	}
}
