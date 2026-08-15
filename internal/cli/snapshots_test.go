package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// firstSnapshotName returns the snapshot name leading the first non-empty line
// of `lit snapshots new`/`list` text output ("<name> <...>" per line).
func firstSnapshotName(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func TestSnapshotsNew_ProducesSnapshot(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	before := countUserSnapshots(t, ws)

	stderr := captureRun(t, "snapshots", "new")

	if stderr.Len() != 0 {
		t.Fatalf("happy path stderr should be empty, got: %q", stderr.String())
	}
	if got := countUserSnapshots(t, ws); got-before != 1 {
		t.Fatalf("user-snapshot delta = %d, want 1 (before=%d)", got-before, before)
	}
}

func TestSnapshotsList_NewestFirst(t *testing.T) {
	repo, _ := initBootstrapTestRepo(t)
	chdir(t, repo)

	for i := 0; i < 3; i++ {
		captureRun(t, "snapshots", "new")
	}

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"snapshots", "list"}); err != nil {
		t.Fatalf("snapshots list: %v", err)
	}
	// Each row leads with the snapshot name ("<name> <created> <path>"). The
	// listing includes the migration-recovery snapshot from the bootstrap Open
	// in addition to the three user snapshots above; the newest-first invariant
	// must hold across all entries.
	var names []string
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			names = append(names, fields[0])
		}
	}
	if len(names) < 3 {
		t.Fatalf("listed=%d, want at least 3 (raw=%s)", len(names), stdout.String())
	}
	prev := ""
	for i, name := range names {
		if i > 0 && name >= prev {
			t.Fatalf("not newest-first at index %d: %s >= %s", i, name, prev)
		}
		prev = name
	}
}

func TestSnapshotsRestore_RoundTrip(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	var newOut bytes.Buffer
	if err := Run(context.Background(), &newOut, &newOut, []string{"snapshots", "new"}); err != nil {
		t.Fatalf("snapshots new: %v", err)
	}
	// `snapshots new` text prints "<name> <path>".
	snapName := firstSnapshotName(newOut.String())
	if snapName == "" {
		t.Fatalf("snapshots new returned no name: %s", newOut.String())
	}

	// Mutate the database directory: drop a marker file Dolt would never own.
	markerPath := filepath.Join(ws.DatabasePath, "MUTATED.marker")
	if err := os.WriteFile(markerPath, []byte("after"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	stderr := captureRun(t, "snapshots", "restore", snapName)
	if stderr.Len() != 0 {
		t.Fatalf("restore stderr should be empty, got: %q", stderr.String())
	}

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker file should be gone after restore (err=%v)", err)
	}
	rotations, err := filepath.Glob(ws.DatabasePath + ".pre-restore-*")
	if err != nil {
		t.Fatalf("glob rotations: %v", err)
	}
	if len(rotations) != 1 {
		t.Fatalf("rotation count=%d, want 1", len(rotations))
	}
	if _, err := os.Stat(filepath.Join(rotations[0], "MUTATED.marker")); err != nil {
		t.Fatalf("rotated dir should retain mutated state: %v", err)
	}
}

func TestSnapshotsCommands_SilentOnHappyPath(t *testing.T) {
	repo, _ := initBootstrapTestRepo(t)
	chdir(t, repo)

	cases := [][]string{
		{"snapshots", "new"},
		{"snapshots", "list"},
	}
	for _, args := range cases {
		var stderr bytes.Buffer
		var stdout bytes.Buffer
		if err := Run(context.Background(), &stdout, &stderr, args); err != nil {
			t.Fatalf("Run(%v): %v\nstderr=%s", args, err, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("%v stderr should be empty, got: %q", args, stderr.String())
		}
	}
}

func TestSnapshotsNew_AcquiresCommitLock(t *testing.T) {
	// Pin the contract that `lit snapshots new` serializes against the
	// store-level commit lock. We hold the lock externally, then race a
	// `snapshots new` against a lock release on a goroutine. If the command
	// did not acquire the lock, it would complete before the release fires.
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	release, err := store.LockCommitPath(context.Background(), store.CommitLockPath(ws.DatabasePath))
	if err != nil {
		t.Fatalf("acquire commit lock: %v", err)
	}

	releaseTime := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		releaseTime <- time.Now()
		release()
	}()

	start := time.Now()
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), &stdout, &stderr, []string{"snapshots", "new"}); err != nil {
		t.Fatalf("snapshots new: %v\nstderr=%s", err, stderr.String())
	}
	elapsed := time.Since(start)
	<-done
	released := <-releaseTime
	if elapsed < 200*time.Millisecond {
		t.Fatalf("snapshots new completed in %v; expected to wait at least 200ms for the lock release at %v", elapsed, released)
	}
}

func TestSnapshotsRestore_LockSurvivesRotation(t *testing.T) {
	// Pins the contract that the commit lock lives outside the rotated dolt
	// directory. Pre-fix: lock path was <databaseDir>/.links-commit.lock,
	// rotated away with the database dir during Restore, leaving the canonical
	// path empty for another process to grab while the in-flight restore's
	// release would later delete that other process's lock file.
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	lockPath := store.CommitLockPath(ws.DatabasePath)
	if filepath.Dir(lockPath) == filepath.Clean(ws.DatabasePath) {
		t.Fatalf("lock path %q lives inside the rotated database dir; rotation would clobber it", lockPath)
	}

	// Take a snapshot via the CLI, then restore it. The lock path should be
	// stable across the rotation (no lock file there afterwards because the
	// restore released the lock, but the path semantics are unchanged).
	captureRun(t, "snapshots", "new")
	var listOut bytes.Buffer
	if err := Run(context.Background(), &listOut, &listOut, []string{"snapshots", "list"}); err != nil {
		t.Fatalf("snapshots list: %v", err)
	}
	// `snapshots list` is newest-first; the first row's leading token is the
	// most recent snapshot name.
	firstName := firstSnapshotName(listOut.String())
	if firstName == "" {
		t.Fatal("expected at least one snapshot")
	}

	captureRun(t, "snapshots", "restore", firstName)

	if pathDir := filepath.Dir(store.CommitLockPath(ws.DatabasePath)); pathDir != filepath.Dir(lockPath) {
		t.Fatalf("lock dir moved across Restore: was %q, now %q", filepath.Dir(lockPath), pathDir)
	}
	// And another lock acquisition succeeds at the same path afterwards.
	release, err := store.LockCommitPath(context.Background(), store.CommitLockPath(ws.DatabasePath))
	if err != nil {
		t.Fatalf("acquire commit lock after restore: %v", err)
	}
	release()
}

func TestDataMutations_ProduceZeroSnapshots(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	// Baseline after bootstrap migration; data mutations below must not
	// move this count. The only producers of snapshots are `lit snapshots
	// new` and the migration system on first-touch / actually-mutating
	// Opens — and the bootstrap above already accounts for that.
	before := snapshotsOnDisk(t, ws)

	// Drive a series of data mutations and reads that must not produce snapshots.
	captureRun(t, "new", "--title", "test", "--type", "task", "--topic", "test-topic")
	captureRun(t, "ls")

	after := snapshotsOnDisk(t, ws)
	for _, name := range after {
		found := false
		for _, b := range before {
			if b == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("data mutation produced a new snapshot (%s) — the only producers must be `lit snapshots new` and the migration system, neither of which is exercised by this test", name)
		}
	}
}

// snapshotsOnDisk returns the names of stable snapshot directories under
// the workspace's snapshot dir. Tests use this to assert deltas rather than
// totals, since the migration system seeds a baseline snapshot during
// initBootstrapTestRepo's bootstrap Open.
func snapshotsOnDisk(t *testing.T, ws workspace.Info) []string {
	t.Helper()
	entries, err := os.ReadDir(snapshotsDirFor(ws))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read snapshots dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".reserve") {
			continue
		}
		names = append(names, name)
	}
	return names
}

// countUserSnapshots counts snapshots whose name doesn't carry the
// migration-recovery label, i.e. snapshots that originated from `lit
// snapshots new` (or other future user-facing producers). Tests that
// specifically count user actions, not migration-driven side effects,
// route through this helper.
//
// [LAW:one-source-of-truth] Classification uses store.IsMigrationSnapshotName
// so the test cannot drift from the label the migration system actually
// stamps.
func countUserSnapshots(t *testing.T, ws workspace.Info) int {
	t.Helper()
	count := 0
	for _, name := range snapshotsOnDisk(t, ws) {
		if store.IsMigrationSnapshotName(name) {
			continue
		}
		count++
	}
	return count
}

// TestSnapshotsRestore_RefusesWhileWorkspaceBusy pins the workspace-exclusivity
// contract end-to-end: while an open Store holds the shared workspace lock
// (the shape an `lit ls` reader would take), `lit snapshots restore` must
// refuse with a clear workspace-busy error instead of rotating the Dolt
// directory out from under the reader.
//
// This is the headline acceptance criterion for links-schema-rebuild-r5v9.7
// — the failure mode pre-fix was a query error mid-read or, depending on
// platform/timing, inconsistent results from mmap'd inodes.
func TestSnapshotsRestore_RefusesWhileWorkspaceBusy(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	// Produce a snapshot to restore.
	var newOut bytes.Buffer
	if err := Run(context.Background(), &newOut, &newOut, []string{"snapshots", "new"}); err != nil {
		t.Fatalf("snapshots new: %v", err)
	}
	snapName := firstSnapshotName(newOut.String())
	if snapName == "" {
		t.Fatalf("snapshots new returned no name: %s", newOut.String())
	}

	// Open a long-lived Store that holds the shared workspace lock — the
	// concrete shape of an `lit ls`/`lit show` reader.
	reader, err := store.OpenForRead(context.Background(), ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("OpenForRead: %v", err)
	}
	defer reader.Close()

	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), &stdout, &stderr, []string{"snapshots", "restore", snapName})
	if err == nil {
		t.Fatalf("snapshots restore succeeded while a reader was open; expected workspace-busy refusal\nstdout=%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "workspace busy") {
		t.Fatalf("restore error %q must name the workspace-busy condition", err.Error())
	}
	// The Dolt directory must NOT have been rotated.
	rotations, globErr := filepath.Glob(ws.DatabasePath + ".pre-restore-*")
	if globErr != nil {
		t.Fatalf("glob rotations: %v", globErr)
	}
	if len(rotations) != 0 {
		t.Fatalf("restore rotated the Dolt directory despite the workspace-busy refusal: %v", rotations)
	}
}

// TestSnapshotsNew_RefusesWhileWorkspaceExclusive pins the other direction of
// the reader-vs-rotator contract: while a holder has the exclusive workspace
// lock — the shape AdoptRemoteByClone's displace+clone window, snapshots
// restore's rotation, and candidate promotion all take — `lit snapshots new`
// must refuse with a workspace-busy error instead of walking a directory that
// is being rewritten under it. Pre-fix, the copy ran under only the commit
// lock (a different file the rotators never touch), so a snapshot taken
// mid-adopt was a torn copy of whatever files DOLT_CLONE had written so far
// (links-sync-pgct.14).
//
// Duration: the refusal lands only after the shared acquisition's ~5s retry
// budget elapses — the same grace every Store open extends to a transient
// rotation — so this test intentionally takes ~5s of wall clock.
func TestSnapshotsNew_RefusesWhileWorkspaceExclusive(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	release, err := store.LockWorkspaceExclusive(context.Background(), ws.DatabasePath)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive: %v", err)
	}
	defer func() {
		if relErr := release(); relErr != nil {
			t.Errorf("release exclusive: %v", relErr)
		}
	}()

	before := rawSnapshotDirEntries(t, ws)

	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), &stdout, &stderr, []string{"snapshots", "new"})
	if err == nil {
		t.Fatalf("snapshots new succeeded while the workspace was exclusively held; expected workspace-busy refusal\nstdout=%s", stdout.String())
	}
	// errors.Is is the documented contention discriminator; the message text
	// is context guidance, not the contract.
	if !errors.Is(err, store.ErrWorkspaceBusy) {
		t.Fatalf("snapshots new error %v must wrap store.ErrWorkspaceBusy", err)
	}

	// "Refuses rather than copying" — the refusal happens before any snapshot
	// artifact (final dir, .tmp clone target, .reserve sentinel) is created,
	// so the raw directory listing is byte-identical.
	after := rawSnapshotDirEntries(t, ws)
	if !slices.Equal(before, after) {
		t.Fatalf("refused snapshots new left artifacts behind:\nbefore=%v\nafter=%v", before, after)
	}
}

// TestSnapshotsNew_RefusesOnPendingAdoptMarker pins that the copy honors the
// adopt-pending condemnation the same way every store open does: a marker
// present under the held workspace lock means the directory is a dead adopt's
// partial residue, and snapshotting it would install garbage in the listing
// as a "restorable" recovery point — worse, the retention prune would then
// evict a good snapshot to keep it.
func TestSnapshotsNew_RefusesOnPendingAdoptMarker(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	markerPath := store.AdoptPendingMarkerPath(ws.DatabasePath)
	if err := os.WriteFile(markerPath, []byte(`{"remote":"origin","branch":"main"}`), 0o644); err != nil {
		t.Fatalf("write adopt-pending marker: %v", err)
	}
	defer func() { _ = os.Remove(markerPath) }()

	before := rawSnapshotDirEntries(t, ws)

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), &stdout, &stderr, []string{"snapshots", "new"})
	if err == nil {
		t.Fatalf("snapshots new succeeded on a marker-condemned workspace; expected refusal\nstdout=%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "adopt was interrupted") {
		t.Fatalf("error %q must carry the standard interrupted-adopt condemnation", err.Error())
	}

	after := rawSnapshotDirEntries(t, ws)
	if !slices.Equal(before, after) {
		t.Fatalf("refused snapshots new left artifacts behind:\nbefore=%v\nafter=%v", before, after)
	}
}

// TestSnapshotsNew_RejectsPositionalArgs pins the usage contract: the
// command's only input is --label, and a stray positional (a natural typo,
// since sibling restore takes its argument positionally) is a usage error —
// not a silently unlabeled snapshot the operator can't find later.
func TestSnapshotsNew_RejectsPositionalArgs(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	before := rawSnapshotDirEntries(t, ws)

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), &stdout, &stderr, []string{"snapshots", "new", "nightly-backup"})
	if err == nil {
		t.Fatalf("snapshots new with a positional succeeded; expected usage error\nstdout=%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "usage: lit snapshots new") {
		t.Fatalf("error %q must be the snapshots-new usage error", err.Error())
	}

	after := rawSnapshotDirEntries(t, ws)
	if !slices.Equal(before, after) {
		t.Fatalf("rejected snapshots new left artifacts behind:\nbefore=%v\nafter=%v", before, after)
	}
}

// rawSnapshotDirEntries lists every entry name in the snapshots dir (in
// os.ReadDir's sorted-by-filename order) with no kind filtering — unlike
// snapshotsOnDisk it includes .tmp/.reserve residue, because tests asserting
// "nothing was created" must see partial artifacts too.
func rawSnapshotDirEntries(t *testing.T, ws workspace.Info) []string {
	t.Helper()
	entries, err := os.ReadDir(snapshotsDirFor(ws))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read snapshots dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSnapshotsRestore_RequiresName(t *testing.T) {
	repo, _ := initBootstrapTestRepo(t)
	chdir(t, repo)

	var stdout bytes.Buffer
	err := Run(context.Background(), &stdout, &stdout, []string{"snapshots", "restore"})
	if err == nil {
		t.Fatal("snapshots restore with no name should error")
	}
}

// chdir is a t.Helper wrapper that cd's into dir for the test and restores the
// previous wd on cleanup. captureRun runs the CLI and returns stderr separately
// so tests can assert silence.

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func captureRun(t *testing.T, args ...string) *bytes.Buffer {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), &stdout, &stderr, args); err != nil {
		t.Fatalf("Run(%v): %v\nstdout=%s\nstderr=%s", args, err, stdout.String(), stderr.String())
	}
	return &stderr
}

// TestSnapshotsNew_CollectsInterruptOrphanedResidue pins links-snapshots-3dtv's
// acceptance shape end-to-end: .tmp/.reserve residue stranded by an
// interrupted snapshot copy (fabricated here exactly as a post-grace hard
// exit leaves it) is invisible to `lit snapshots list` yet reclaimed by the
// very next `lit snapshots new`, whose retention tail runs the residue
// collection under the producer beacon's liveness proof.
func TestSnapshotsNew_CollectsInterruptOrphanedResidue(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	snapshotsDir := snapshotsDirFor(ws)
	tmpResidue := filepath.Join(snapshotsDir, "1700000000000000001.tmp")
	reserveResidue := filepath.Join(snapshotsDir, "1700000000000000001.reserve")
	if err := os.MkdirAll(filepath.Join(tmpResidue, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpResidue, "nested", "partial"), []byte("half-copied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reserveResidue, 0o755); err != nil {
		t.Fatal(err)
	}

	// The residue is invisible to the listing (the pre-fix trap: nothing an
	// operator can see or prune).
	var listOut bytes.Buffer
	if err := Run(context.Background(), &listOut, &listOut, []string{"snapshots", "list"}); err != nil {
		t.Fatalf("snapshots list: %v", err)
	}
	if strings.Contains(listOut.String(), "1700000000000000001") {
		t.Fatalf("residue leaked into the listing:\n%s", listOut.String())
	}

	var newOut bytes.Buffer
	if err := Run(context.Background(), &newOut, &newOut, []string{"snapshots", "new"}); err != nil {
		t.Fatalf("snapshots new: %v", err)
	}
	if firstSnapshotName(newOut.String()) == "" {
		t.Fatalf("snapshots new returned no name: %s", newOut.String())
	}

	for _, residue := range []string{tmpResidue, reserveResidue} {
		if _, err := os.Stat(residue); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("residue %s must be collected by the next snapshots new, stat err=%v", residue, err)
		}
	}
}
