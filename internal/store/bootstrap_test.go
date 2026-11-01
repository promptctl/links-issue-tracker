package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

const (
	bootstrapRacerRootEnvVar   = "LIT_TEST_BOOTSTRAP_RACER_ROOT"
	bootstrapRacerReaderEnvVar = "LIT_TEST_BOOTSTRAP_RACER_READER"
)

// TestHelperBootstrapRacer is one racer in TestRacingFirstOpensBuildOneDatabase,
// run in its own process. It blocks until the parent closes its stdin, then
// either does what `lit init` does to a fresh workspace — ask whether the local
// store holds tickets, then make the database exist — and prints whether it
// built it, or, as the reader, opens the workspace for reading the moment
// there is one to open.
func TestHelperBootstrapRacer(t *testing.T) {
	root := os.Getenv(bootstrapRacerRootEnvVar)
	if root == "" {
		t.Skip("helper process for TestRacingFirstOpensBuildOneDatabase")
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for the start signal: %v", err)
	}
	ctx := context.Background()
	if os.Getenv(bootstrapRacerReaderEnvVar) != "" {
		readTheFirstDatabaseToAppear(t, ctx, root)
		return
	}
	if _, _, err := LocalHasTickets(ctx, testEnv(), testStderr(), root, "test-workspace-id"); err != nil {
		t.Fatalf("LocalHasTickets() error = %v", err)
	}
	bootstrap, err := EnsureDatabase(ctx, testStderr(), root, "test-workspace-id")
	if err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}
	fmt.Printf("created=%t\n", bootstrap.Built())
}

// readTheFirstDatabaseToAppear opens root for reading as soon as it is not
// uninitialized, so a root that appears before its database is whole is read
// in that state. A build takes about a second here; the deadline only ever
// fires on a hang.
func readTheFirstDatabaseToAppear(t *testing.T, ctx context.Context, root string) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := OpenForRead(ctx, testEnv(), testStderr(), root, "test-workspace-id")
		if errors.Is(err, ErrWorkspaceNotInitialized) {
			if time.Now().After(deadline) {
				t.Fatal("no database appeared before the deadline")
			}
			continue
		}
		if err != nil {
			t.Fatalf("OpenForRead() on the first root to appear: %v", err)
		}
		if _, err := st.LocalIssueCount(ctx); err != nil {
			t.Fatalf("LocalIssueCount() on the first root to appear: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
		return
	}
}

// Processes that bootstrap one fresh workspace at the same moment all succeed,
// exactly one of them builds the database, and what they leave behind opens
// for writing. A reader racing them reads the first database to appear, so a
// database that is visible before it is whole fails the round.
// The racers are processes, not goroutines: the embedded driver serializes
// engines within one process, which hides the race. Closing every racer's
// stdin at once is the starting gun.
func TestRacingFirstOpensBuildOneDatabase(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	const rounds, builders = 4, 3
	for round := range rounds {
		root := filepath.Join(t.TempDir(), "dolt")
		// The last racer is the reader.
		cmds := make([]*exec.Cmd, builders+1)
		outs := make([]*bytes.Buffer, len(cmds))
		starts := make([]io.Closer, len(cmds))
		for i := range cmds {
			cmd := exec.Command(self, "-test.run=^TestHelperBootstrapRacer$", "-test.v")
			cmd.Env = append(os.Environ(), bootstrapRacerRootEnvVar+"="+root)
			if i == builders {
				cmd.Env = append(cmd.Env, bootstrapRacerReaderEnvVar+"=1")
			}
			outs[i] = &bytes.Buffer{}
			cmd.Stdout, cmd.Stderr = outs[i], outs[i]
			start, err := cmd.StdinPipe()
			if err != nil {
				t.Fatalf("StdinPipe() = %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start racer: %v", err)
			}
			cmds[i], starts[i] = cmd, start
		}
		for _, start := range starts {
			if err := start.Close(); err != nil {
				t.Fatalf("signal racer start: %v", err)
			}
		}
		// Every racer is waited for before any verdict, so a failed round
		// leaves no racer running past the test.
		waitErrs := make([]error, len(cmds))
		for i, cmd := range cmds {
			waitErrs[i] = cmd.Wait()
		}
		built := 0
		for i, err := range waitErrs {
			if err != nil {
				t.Fatalf("round %d racer %d failed: %v\n%s", round, i, err, outs[i])
			}
			built += strings.Count(outs[i].String(), "created=true")
		}
		if built != 1 {
			t.Fatalf("round %d: %d racers report building the database, want exactly 1", round, built)
		}
		assertNoBootstrapStaging(t, root)
		st, err := Open(context.Background(), testEnv(), testStderr(), root, "test-workspace-id")
		if err != nil {
			t.Fatalf("round %d: Open() after the race = %v", round, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
}

// A racer waits out a builder however long the build runs, since a build's
// length is the machine's load: with the stand-still wait shrunk far below
// the builder's hold, the racer still takes the lock once the builder lets
// go, and its wait reports acquired.
func TestARacerWaitsOutABuilderPastTheStandStillWait(t *testing.T) {
	// serial: rewrites the package-level coResidentHolderWait.
	shrinkWait(t, 100*time.Millisecond)
	root := filepath.Join(t.TempDir(), "dolt")
	builder, err := acquireBootstrapLock(WithHolderCommand(context.Background(), "lit init"), testStderr(), root)
	if err != nil {
		t.Fatalf("builder acquireBootstrapLock() error = %v", err)
	}
	hold := 15 * coResidentHolderWait
	released := make(chan error, 1)
	go func() {
		time.Sleep(hold)
		released <- builder()
	}()
	reports := &heldReports{}
	racer, err := acquireBootstrapLock(WithLockObserver(context.Background(), reports.observer()), testStderr(), root)
	if err != nil {
		t.Fatalf("a racer behind a builder holding for %s failed: %v", hold, err)
	}
	if err := errors.Join(racer(), <-released); err != nil {
		t.Fatalf("release error = %v", err)
	}
	wait := lockWaitOn(reports, filepath.Base(bootstrapLockPath(root)))
	if wait == nil || wait.Outcome != LockWaitAcquired || wait.Waited < 10*coResidentHolderWait {
		t.Fatalf("the racer's bootstrap wait was reported as %+v, want acquired after waiting most of the builder's %s hold", wait, hold)
	}
}

// The caller's context is the one end to a racer's wait: it ends with the
// context's own error, naming the builder it waited on, and is reported
// cancelled, never refused as busy.
func TestARacersContextEndsItsWaitNamingTheBuilder(t *testing.T) {
	// serial: rewrites the package-level coResidentHolderWait.
	shrinkWait(t, 100*time.Millisecond)
	root := filepath.Join(t.TempDir(), "dolt")
	builder, err := acquireBootstrapLock(WithHolderCommand(context.Background(), "lit init"), testStderr(), root)
	if err != nil {
		t.Fatalf("builder acquireBootstrapLock() error = %v", err)
	}
	t.Cleanup(func() {
		if err := builder(); err != nil {
			t.Errorf("builder release error = %v", err)
		}
	})
	reports := &heldReports{}
	ctx, cancel := context.WithTimeout(WithLockObserver(context.Background(), reports.observer()), 5*coResidentHolderWait)
	defer cancel()
	racer, err := acquireBootstrapLock(ctx, testStderr(), root)
	if err == nil {
		_ = racer()
		t.Fatal("a racer took the bootstrap lock beside a live builder")
	}
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("racer error = %v, want the context's deadline and not ErrWorkspaceBusy", err)
	}
	if !strings.Contains(err.Error(), "(lit init)") {
		t.Fatalf("racer error %q does not name the builder", err)
	}
	if wait := lockWaitOn(reports, filepath.Base(bootstrapLockPath(root))); wait == nil || wait.Outcome != LockWaitCancelled {
		t.Fatalf("the racer's bootstrap wait was reported as %+v, want cancelled", wait)
	}
}

// An open of a store that already exists is not deciding to build, so a
// builder's hold on the bootstrap lock is nothing to it: with the lock held
// throughout, each open completes and reports no wait on it. The deadline
// turns an open that queued behind the holder into a failure, not a hang.
func TestAnExistingStoresOpensPassABootstrapLockHolder(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "dolt")
	if _, err := EnsureDatabase(context.Background(), testStderr(), root, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() = %v", err)
	}
	builder, err := acquireBootstrapLock(WithHolderCommand(context.Background(), "lit init"), testStderr(), root)
	if err != nil {
		t.Fatalf("builder acquireBootstrapLock() error = %v", err)
	}
	t.Cleanup(func() {
		if err := builder(); err != nil {
			t.Errorf("builder release error = %v", err)
		}
	})
	for name, open := range map[string]func(context.Context) error{
		"EnsureDatabase": func(ctx context.Context) error {
			bootstrap, err := EnsureDatabase(ctx, testStderr(), root, "test-workspace-id")
			if err == nil && bootstrap != storage.BootstrapNone {
				return fmt.Errorf("bootstrap = %v, want %v", bootstrap, storage.BootstrapNone)
			}
			return err
		},
		"Open": func(ctx context.Context) error {
			st, err := Open(ctx, testEnv(), testStderr(), root, "test-workspace-id")
			if err != nil {
				return err
			}
			return st.Close()
		},
		"OpenSync": func(ctx context.Context) error {
			st, err := OpenSync(ctx, testEnv(), testStderr(), root, "test-workspace-id")
			if err != nil {
				return err
			}
			return st.Close()
		},
	} {
		reports := &heldReports{}
		ctx, cancel := context.WithTimeout(WithLockObserver(context.Background(), reports.observer()), 30*time.Second)
		err := open(ctx)
		cancel()
		if err != nil {
			t.Errorf("%s with the bootstrap lock held = %v", name, err)
		}
		if wait := lockWaitOn(reports, filepath.Base(bootstrapLockPath(root))); wait != nil {
			t.Errorf("%s waited on the bootstrap lock: %+v", name, wait)
		}
	}
}

// A builder that died mid-build leaves its partial work behind: a staging
// root, or, from a lit that built in place, a database directory with no
// .dolt in it. The next bootstrap holds the lock that proves no builder owns
// either, so it discards the leftover, builds, says it built over a dead
// build, and leaves nothing at the staging path.
func TestBootstrapDiscardsADeadBuildsLeftover(t *testing.T) {
	t.Parallel()
	for _, leftover := range []func(root string) string{
		func(root string) string { return filepath.Join(bootstrapStagingPath(root), doltDatabaseName, ".dolt") },
		func(root string) string { return filepath.Join(root, doltDatabaseName, "partial") },
	} {
		root := filepath.Join(t.TempDir(), "dolt")
		dir := leftover(root)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() = %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "repo_state.json"), []byte("half-written"), 0o644); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		bootstrap, err := EnsureDatabase(context.Background(), testStderr(), root, "test-workspace-id")
		if err != nil || bootstrap != storage.BootstrapBuiltOverDeadBuild {
			t.Fatalf("EnsureDatabase() over %s = %v, %v; want %v", dir, bootstrap, err, storage.BootstrapBuiltOverDeadBuild)
		}
		assertNoBootstrapStaging(t, root)
		st, err := Open(context.Background(), testEnv(), testStderr(), root, "test-workspace-id")
		if err != nil {
			t.Fatalf("Open() after building over %s = %v", dir, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
}

// Only a database's confirmed absence licenses discarding what stands in its
// place, so a thing at its path that is neither nothing nor a directory, a
// symlink that points nowhere included, stays where it is, and the open that
// found it fails naming it rather than deep inside the engine.
func TestAnOpenRefusesWhatStandsWhereADatabaseShouldBe(t *testing.T) {
	t.Parallel()
	dangling := func(t *testing.T, path string) {
		t.Helper()
		if err := os.Symlink(filepath.Join(t.TempDir(), "unmounted"), path); err != nil {
			t.Fatalf("Symlink() = %v", err)
		}
	}
	file := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
	}
	for name, tc := range map[string]struct {
		place func(*testing.T, string)
		under string
	}{
		"a database directory that points nowhere": {place: dangling},
		"a file at the database directory":         {place: file},
		"a .dolt that points nowhere":              {place: dangling, under: ".dolt"},
		"a file at .dolt":                          {place: file, under: ".dolt"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "dolt")
			database := filepath.Join(root, doltDatabaseName)
			standing := database
			parent := root
			if tc.under != "" {
				standing = filepath.Join(database, tc.under)
				parent = database
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatalf("MkdirAll() = %v", err)
			}
			tc.place(t, standing)
			for open, run := range map[string]func() error{
				"EnsureDatabase": func() error {
					_, err := EnsureDatabase(context.Background(), testStderr(), root, "test-workspace-id")
					return err
				},
				"Open": func() error {
					st, err := Open(context.Background(), testEnv(), testStderr(), root, "test-workspace-id")
					if err != nil {
						return err
					}
					return st.Close()
				},
			} {
				if err := run(); err == nil || !strings.Contains(err.Error(), "check for a database") || !strings.Contains(err.Error(), standing) {
					t.Errorf("%s = %v; want an error naming %s", open, err, standing)
				}
			}
			if _, err := os.Lstat(standing); err != nil {
				t.Fatalf("lstat %s after the opens = %v; want it untouched", standing, err)
			}
		})
	}
}

// A store reached through a symlink opens as it always has: the link stands
// for the directory it points at, so an existing database is found, and the
// link is not mistaken for anything to discard.
func TestAnOpenFollowsASymlinkToAnExistingDatabase(t *testing.T) {
	t.Parallel()
	elsewhere := filepath.Join(t.TempDir(), "dolt")
	if _, err := EnsureDatabase(context.Background(), testStderr(), elsewhere, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() = %v", err)
	}
	root := filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, doltDatabaseName), filepath.Join(root, doltDatabaseName)); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}
	bootstrap, err := EnsureDatabase(context.Background(), testStderr(), root, "test-workspace-id")
	if err != nil || bootstrap != storage.BootstrapNone {
		t.Fatalf("EnsureDatabase() through a symlink = %v, %v; want %v", bootstrap, err, storage.BootstrapNone)
	}
}

// A root directory that is already there but holds no database, empty or
// holding other files, gets the database built into it, and the files it
// held are still there after.
func TestBootstrapBuildsIntoARootThatHoldsNoDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, stray := range []string{"", ".links-adopt-pending.tmp-123"} {
		root := filepath.Join(t.TempDir(), "dolt")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("MkdirAll() = %v", err)
		}
		if stray != "" {
			if err := os.WriteFile(filepath.Join(root, stray), []byte("lit's own"), 0o644); err != nil {
				t.Fatalf("WriteFile() = %v", err)
			}
		}
		bootstrap, err := EnsureDatabase(ctx, testStderr(), root, "test-workspace-id")
		if err != nil || bootstrap != storage.BootstrapBuilt {
			t.Fatalf("EnsureDatabase(root holding %q) = %v, %v; want %v", stray, bootstrap, err, storage.BootstrapBuilt)
		}
		if stray != "" {
			if got, err := os.ReadFile(filepath.Join(root, stray)); err != nil || string(got) != "lit's own" {
				t.Fatalf("%s after the build = %q, %v; want it untouched", stray, got, err)
			}
		}
		assertNoBootstrapStaging(t, root)
		st, err := Open(ctx, testEnv(), testStderr(), root, "test-workspace-id")
		if err != nil {
			t.Fatalf("Open(root holding %q) = %v", stray, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
}

// A received-refs record describes the database it was written beside. A
// bootstrap that builds a new database forgets it, and one that finds the
// database there leaves it alone.
func TestBootstrapForgetsReceivedRefsOnlyWhenItBuilds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "dolt")
	record := []byte("refs of a database that is gone")
	for _, want := range []storage.Bootstrap{storage.BootstrapBuilt, storage.BootstrapNone} {
		if err := WriteReceivedRefs(root, record); err != nil {
			t.Fatalf("WriteReceivedRefs() = %v", err)
		}
		bootstrap, err := EnsureDatabase(ctx, testStderr(), root, "test-workspace-id")
		if err != nil || bootstrap != want {
			t.Fatalf("EnsureDatabase() = %v, %v; want %v", bootstrap, err, want)
		}
		got, err := ReadReceivedRefs(root)
		if err != nil {
			t.Fatalf("ReadReceivedRefs() = %v", err)
		}
		if kept := got != nil; kept != !want.Built() {
			t.Fatalf("after a bootstrap that did %v, received-refs record = %q; want it kept only when nothing was built", want, got)
		}
	}
}

func assertNoBootstrapStaging(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(bootstrapStagingPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap staging root after the bootstrap: stat error = %v, want it gone", err)
	}
}
