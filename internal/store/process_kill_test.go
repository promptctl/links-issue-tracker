//go:build !windows

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/pressly/goose/v3"
)

// killHelperDirEnvVar carries the store directory a re-exec'd helper test
// must open. Its presence is also the gate that keeps the helper tests inert
// under an ordinary `go test ./...` run — TestHelperKillMidCommit and
// TestHelperKillMidMigrationStep both SIGKILL their own process, so nothing
// here may fire without an explicit opt-in from the parent that spawned them.
const killHelperDirEnvVar = "LIT_STORE_KILL_HELPER_DIR"

// TestHelperKillMidCommit is not a real test — it is re-executed as a
// separate OS process by TestMidMutationProcessKillRecovers. It installs the
// commitWorkingSetHookForTest injection point (commit_lock.go) to fire
// exactly where a real crash matters: after fn's writes are durably staged
// (tx.Commit succeeded, per withStampedMutation's staged flag) but before
// DOLT_COMMIT versions them. SIGKILL there is unblockable and immediate, so
// the process dies inside the hook — CreateIssue never returns, and no
// deferred cleanup (commit lock release included) runs, exactly the shape a
// genuine crash leaves behind.
func TestHelperKillMidCommit(t *testing.T) {
	dir := os.Getenv(killHelperDirEnvVar)
	if dir == "" {
		t.Skip("helper test; runs only via TestMidMutationProcessKillRecovers's re-exec")
	}
	ctx := context.Background()
	st, err := Open(ctx, dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("helper Open() error = %v", err)
	}
	st.commitWorkingSetHookForTest = func() error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // unreachable if the kill lands; guards against any delay
	}
	_, _ = st.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "mid-mutation kill target",
		Topic:     "crash",
		IssueType: "task",
		Priority:  0,
	})
	t.Fatal("helper process was not killed inside the commitWorkingSetOnce hook")
}

// TestMidMutationProcessKillRecovers is the acceptance pin for
// links-testing-tt0c.2's withMutation half: a real SIGKILL delivered while a
// mutation is staged-but-not-yet-versioned must not wedge the store. The
// existing panic-injection tests in crash_safety_test.go prove the deferred
// release fires for an in-process panic; this proves the same recovery holds
// for a process that a real crash removes with NO deferred cleanup at all —
// the direct modern analog of the lock-release race the historical
// high-severity finding this ticket cites was about.
func TestMidMutationProcessKillRecovers(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	dir := migratedDoltDir(t)

	cmd := exec.Command(self, "-test.run=^TestHelperKillMidCommit$")
	cmd.Env = append(os.Environ(), killHelperDirEnvVar+"="+dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	requireKilledBySIGKILL(t, runErr, stderr.String())

	// Reopen the SAME store path from this process. The acceptance bar: the
	// store must recover, not stay wedged behind a lock the dead process
	// never released, nor leave the working set unreadable.
	ctx := context.Background()
	st, err := Open(ctx, dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after mid-commit kill error = %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	assertCommitLockFree(t, st.commitLockPath)

	// The killed process's tx.Commit() succeeded before the hook fired, so
	// its write is durably staged in the working set even though the
	// process died before DOLT_COMMIT ran. A fresh process opening the same
	// path must see it.
	found, err := st.ListIssues(ctx, ListIssuesFilter{SearchTerms: []string{"mid-mutation kill target"}})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the killed mutation's staged write did not survive the kill — tx.Commit's durability guarantee did not hold under a real process kill")
	}

	// A fresh mutation must succeed — proves the store is not wedged behind
	// a lock the dead holder failed to release.
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "post-kill issue", Topic: "crash", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue() after mid-commit kill error = %v", err)
	}
	got, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() after mid-commit kill error = %v", err)
	}
	if got.Title != issue.Title {
		t.Fatalf("GetIssue().Title = %q, want %q", got.Title, issue.Title)
	}
}

// TestHelperKillMidMigrationStep is not a real test — it is re-executed as a
// separate OS process by TestMidMigrationStepProcessKillRecovers. It installs
// migrationUpByOneForTest (migration_runner.go) to run the REAL goose step
// (the same s.upByOne production code takes) and then SIGKILL the process
// immediately after — before applyPendingMigrations' loop reaches that
// step's own commitWorkingSet call. The step's DDL (and goose's own
// bookkeeping row in goose_db_version) land in the working set exactly as
// production code writes them; only the versioning commit is missing when
// the process dies.
func TestHelperKillMidMigrationStep(t *testing.T) {
	dir := os.Getenv(killHelperDirEnvVar)
	if dir == "" {
		t.Skip("helper test; runs only via TestMidMigrationStepProcessKillRecovers's re-exec")
	}
	migrationUpByOneForTest = func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error) {
		result, err := provider.UpByOne(ctx)
		if err != nil {
			return result, err
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // unreachable if the kill lands; guards against any delay
	}
	_, _ = Open(context.Background(), dir, "test-workspace-id")
	t.Fatal("helper process was not killed inside the migration-step hook")
}

// TestMidMigrationStepProcessKillRecovers is the acceptance pin for
// links-testing-tt0c.2's goose-migration half: a real SIGKILL delivered
// after one migration step's DDL lands in the working set but before its
// commitWorkingSet call must still let the NEXT Open reach a consistent,
// usable schema — not a QuarantineBlockError, not a half-migrated table
// shape. The existing checkpoint/quarantine tests
// (migration_quarantine_test.go) only ever inject a returned Go error;
// nothing until now has proven the same recovery holds when the process
// itself disappears mid-step with no error to catch.
func TestMidMigrationStepProcessKillRecovers(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	ctx := context.Background()
	dir := migratedDoltDir(t)

	// Force a real migration to be pending on the next Open: drop goose's
	// version history and revert the schema to the pre-goose baseline, the
	// same setup migration_quarantine_test.go's failure-injection tests use.
	withGooseHistoryDropped(t, ctx, dir)

	cmd := exec.Command(self, "-test.run=^TestHelperKillMidMigrationStep$")
	cmd.Env = append(os.Environ(), killHelperDirEnvVar+"="+dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	requireKilledBySIGKILL(t, runErr, stderr.String())

	// Reopen from this process. Whether the killed step's own migration is
	// still pending (goose reapplies it) or already recorded in the working
	// set (goose sees it as done and moves on), Open must complete —
	// neither shape may surface as QuarantineBlockError or
	// CheckpointResetError, since no Go error was ever returned for the
	// killed step to be quarantined against.
	st, err := Open(ctx, dir, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after mid-migration-step kill error = %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	assertCommitLockFree(t, st.commitLockPath)

	// The schema must be fully usable: CreateIssue depends on the complete,
	// current schema, so its success is the proof no partially-applied
	// migration left the working set in a broken shape.
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "post-migration-kill issue", Topic: "crash", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue() after mid-migration-step kill error = %v", err)
	}
	got, err := st.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() after mid-migration-step kill error = %v", err)
	}
	if got.Title != issue.Title {
		t.Fatalf("GetIssue().Title = %q, want %q", got.Title, issue.Title)
	}
}

// requireKilledBySIGKILL fails the test unless runErr reports that the child
// process terminated by SIGKILL. A clean exit (nil) or any other exit shape
// means the process never reached the hook, which would make everything the
// caller asserts afterward a false pass rather than proof of recovery from an
// actual kill.
func requireKilledBySIGKILL(t *testing.T, runErr error, stderr string) {
	t.Helper()
	if runErr == nil {
		t.Fatalf("helper process exited cleanly; expected it to be killed mid-write:\n%s", stderr)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("helper process error = %v (%T); want *exec.ExitError:\n%s", runErr, runErr, stderr)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("helper process did not terminate by SIGKILL (state=%v):\n%s", exitErr.ProcessState, stderr)
	}
}
