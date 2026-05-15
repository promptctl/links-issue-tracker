package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotsNew_ProducesSnapshot(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	stderr := captureRun(t, "snapshots", "new", "--json")

	if stderr.Len() != 0 {
		t.Fatalf("happy path stderr should be empty, got: %q", stderr.String())
	}
	entries, err := os.ReadDir(snapshotsDirFor(ws))
	if err != nil {
		t.Fatalf("read snapshots dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("snapshot count on disk = %d, want 1", count)
	}
}

func TestSnapshotsList_NewestFirst(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
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
	if len(listed) != 3 {
		t.Fatalf("listed=%d, want 3 (raw=%s)", len(listed), stdout.String())
	}
	prev := ""
	for i, s := range listed {
		name, _ := s["name"].(string)
		if i > 0 && name >= prev {
			t.Fatalf("not newest-first at index %d: %s >= %s", i, name, prev)
		}
		prev = name
	}
	_ = ws // referenced for symmetry with other tests; assertion is on listed JSON
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

func TestDataMutations_ProduceZeroSnapshots(t *testing.T) {
	repo, ws := initBootstrapTestRepo(t)
	chdir(t, repo)

	// Drive a series of data mutations and reads that must not produce snapshots.
	captureRun(t, "new", "--title", "test", "--type", "task", "--topic", "test-topic", "--json")
	captureRun(t, "ls", "--json")

	entries, err := os.ReadDir(snapshotsDirFor(ws))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read snapshots dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("data mutation produced a snapshot (%s) — the only producer must be `lit snapshots new` or the migration system", e.Name())
		}
	}
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
