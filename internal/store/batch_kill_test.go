//go:build !windows

package store

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// batchKillMarkerEnvVar names the file the helper writes once its batch's
// first create has committed, the moment the parent reads beside it and then
// kills it.
const batchKillMarkerEnvVar = "LIT_STORE_BATCH_KILL_MARKER"

// batchKillEpicEnvVar carries the epic the helper's file creates under.
const batchKillEpicEnvVar = "LIT_STORE_BATCH_KILL_EPIC"

// batchKillFile is the two-create file the helper imports and the parent
// imports again after the kill.
func batchKillFile(epic string) []storage.BulkIssueSpec {
	topic, taskType := "crash", "task"
	first, second := "batch kill first", "batch kill second"
	return []storage.BulkIssueSpec{
		{Title: &first, Topic: &topic, IssueType: &taskType, Parent: &epic},
		{Title: &second, Topic: &topic, IssueType: &taskType, Parent: &epic},
	}
}

// TestHelperBlockMidBatch is not a real test — it is re-executed as a
// separate OS process by TestABatchKilledPartwayWritesNothing. It imports a
// two-create file and, at the second create's commit (the first is
// committed on the batch's branch by then), writes the marker file and
// blocks until the parent kills it, so no deferred cleanup runs.
func TestHelperBlockMidBatch(t *testing.T) {
	dir := os.Getenv(killHelperDirEnvVar)
	if dir == "" {
		t.Skip("helper test; runs only via TestABatchKilledPartwayWritesNothing's re-exec")
	}
	ctx := context.Background()
	st, err := Open(ctx, testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("helper Open() error = %v", err)
	}
	// Fire 1 is the stranded-writes commit at the batch's start, 2 the first
	// create's, 3 the second's.
	fires := 0
	st.commitWorkingSetHookForTest = func() error {
		fires++
		if fires == 3 {
			if err := os.WriteFile(os.Getenv(batchKillMarkerEnvVar), nil, 0o644); err != nil {
				t.Errorf("write marker: %v", err)
			}
			select {} // the parent kills this process here
		}
		return nil
	}
	_, _ = st.BulkApply(ctx, "test", "ada", batchKillFile(os.Getenv(batchKillEpicEnvVar)))
	t.Fatal("helper process was not killed inside its batch")
}

// A batch is invisible to a reader until it lands, and a process killed
// partway writes nothing: the branch it started from holds none of its
// writes, the file imports again whole with each create made once, and the
// branch the killed batch wrote on shows on that import's record as an
// orphan beside the import's own finished one.
func TestABatchKilledPartwayWritesNothing(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	ctx := context.Background()
	dir := migratedDoltDir(t)
	st, err := Open(ctx, testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		t.Fatal(err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "home", Topic: "crash", IssueType: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	head := headCommit(t, ctx, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "mid-batch")
	cmd := exec.Command(self, "-test.run=^TestHelperBlockMidBatch$")
	cmd.Env = append(os.Environ(), killHelperDirEnvVar+"="+dir, batchKillMarkerEnvVar+"="+marker, batchKillEpicEnvVar+"="+epic.ID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, marker, cmd)

	reader, err := OpenForRead(ctx, testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("OpenForRead() beside the batch error = %v", err)
	}
	beside := countTitled(t, ctx, reader, "batch kill")
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	requireKilledBySIGKILL(t, cmd.Wait(), stderr.String())
	if beside != 0 {
		t.Fatalf("a reader beside the batch saw %d of its creates; want none before it lands", beside)
	}

	st, err = Open(ctx, testEnv(), testStderr(), dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after the kill error = %v", err)
	}
	defer func() { _ = st.Close() }()
	if got := headCommit(t, ctx, st); got != head {
		t.Fatalf("head after the kill = %s; want %s, the head before the batch", got, head)
	}
	if n := countTitled(t, ctx, st, "batch kill"); n != 0 {
		t.Fatalf("creates after the kill = %d; want none", n)
	}
	result, err := st.BulkApply(ctx, "test", "ada", batchKillFile(epic.ID))
	if err != nil {
		t.Fatalf("BulkApply() again after the kill error = %v", err)
	}
	if len(result.Created) != 2 || countTitled(t, ctx, st, "batch kill") != 2 {
		t.Fatalf("result = %+v; want each create made once", result)
	}
	deletes, looked := st.ScratchDeletes()
	causes := map[storage.ScratchCause]int{}
	for _, d := range deletes {
		if !strings.HasPrefix(d.Branch, batchScratchPrefix) || d.Err != nil {
			t.Errorf("scratch delete %+v; want a batch branch, deleted", d)
		}
		causes[d.Cause]++
	}
	if !looked || len(deletes) != 2 || causes[storage.ScratchOrphan] != 1 || causes[storage.ScratchFinished] != 1 {
		t.Fatalf("ScratchDeletes() = %+v, looked %v; want the killed batch's branch as an orphan and the import's own as finished", deletes, looked)
	}
}

// waitForFile waits for the helper to write path, failing if the helper
// exits first or a minute passes.
func waitForFile(t *testing.T, path string, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if cmd.ProcessState != nil {
			t.Fatalf("helper exited before its batch reached the marker")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatalf("helper never reached the marker %s", path)
}

func countTitled(t *testing.T, ctx context.Context, st *Store, term string) int {
	t.Helper()
	found, err := st.ListIssues(ctx, storage.ListIssuesFilter{SearchTerms: []string{term}})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	return len(found)
}
