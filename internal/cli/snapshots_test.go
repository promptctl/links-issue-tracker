package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestSnapshotsNew_ProducesSnapshot(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	before := countUserSnapshots(t, ws)

	stderr := captureRun(t, "snapshots", "new", "--json")

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
		captureRun(t, "snapshots", "new", "--json")
	}

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"snapshots", "list", "--json"}); err != nil {
		t.Fatalf("snapshots list: %v", err)
	}
	var listed []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode JSON: %v\nraw=%s", err, stdout.String())
	}
	// listed includes the migration-recovery snapshot from the bootstrap
	// Open in addition to the three user snapshots above. The newest-first
	// invariant must still hold across all entries.
	if len(listed) < 3 {
		t.Fatalf("listed=%d, want at least 3 (raw=%s)", len(listed), stdout.String())
	}
	prev := ""
	for i, s := range listed {
		name, _ := s["name"].(string)
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
	if err := Run(context.Background(), &newOut, &newOut, []string{"snapshots", "new", "--json"}); err != nil {
		t.Fatalf("snapshots new: %v", err)
	}
	var snap struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(newOut.Bytes(), &snap); err != nil {
		t.Fatalf("decode new JSON: %v\nraw=%s", err, newOut.String())
	}

	// Mutate the database directory: drop a marker file Dolt would never own.
	markerPath := filepath.Join(ws.DatabasePath, "MUTATED.marker")
	if err := os.WriteFile(markerPath, []byte("after"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	stderr := captureRun(t, "snapshots", "restore", snap.Name, "--json")
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
		{"snapshots", "new", "--json"},
		{"snapshots", "list", "--json"},
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
	captureRun(t, "snapshots", "new", "--json")
	var listOut bytes.Buffer
	if err := Run(context.Background(), &listOut, &listOut, []string{"snapshots", "list", "--json"}); err != nil {
		t.Fatalf("snapshots list: %v", err)
	}
	var listed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(listOut.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("expected at least one snapshot")
	}

	captureRun(t, "snapshots", "restore", listed[0].Name, "--json")

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
	captureRun(t, "new", "--title", "test", "--type", "task", "--topic", "test-topic", "--json")
	captureRun(t, "ls", "--json")

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
