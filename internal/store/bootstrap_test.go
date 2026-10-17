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
	if _, _, err := LocalHasTickets(ctx, root, "test-workspace-id"); err != nil {
		t.Fatalf("LocalHasTickets() error = %v", err)
	}
	created, err := EnsureDatabase(ctx, root, "test-workspace-id")
	if err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}
	fmt.Printf("created=%t\n", created)
}

// readTheFirstDatabaseToAppear opens root for reading as soon as it is not
// uninitialized, so a root that appears before its database is whole is read
// in that state. A build takes about a second here; the deadline only ever
// fires on a hang.
func readTheFirstDatabaseToAppear(t *testing.T, ctx context.Context, root string) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := OpenForRead(ctx, root, "test-workspace-id")
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
		built := 0
		for i, cmd := range cmds {
			if err := cmd.Wait(); err != nil {
				t.Fatalf("round %d racer %d failed: %v\n%s", round, i, err, outs[i])
			}
			built += strings.Count(outs[i].String(), "created=true")
		}
		if built != 1 {
			t.Fatalf("round %d: %d racers report building the database, want exactly 1", round, built)
		}
		assertNoBootstrapStaging(t, root)
		st, err := Open(context.Background(), root, "test-workspace-id")
		if err != nil {
			t.Fatalf("round %d: Open() after the race = %v", round, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
}

// A builder that died mid-build leaves its staging root behind. The next
// bootstrap holds the lock that proves the builder is gone, so it discards
// the leftover, builds, and leaves nothing at the staging path.
func TestBootstrapDiscardsADeadBuildersStaging(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "dolt")
	leftover := filepath.Join(bootstrapStagingPath(root), doltDatabaseName, ".dolt")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "repo_state.json"), []byte("half-written"), 0o644); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	created, err := EnsureDatabase(context.Background(), root, "test-workspace-id")
	if err != nil || !created {
		t.Fatalf("EnsureDatabase() = %t, %v; want it to build the database", created, err)
	}
	assertNoBootstrapStaging(t, root)
	st, err := Open(context.Background(), root, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

// A root directory with no database in it is built into only when it is
// empty. One holding anything else is refused by name and left untouched.
func TestBootstrapBuildsIntoAnEmptyRootAndRefusesAnOccupiedOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	empty := filepath.Join(t.TempDir(), "dolt")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	if created, err := EnsureDatabase(ctx, empty, "test-workspace-id"); err != nil || !created {
		t.Fatalf("EnsureDatabase(empty root) = %t, %v; want it to build the database", created, err)
	}

	occupied := filepath.Join(t.TempDir(), "dolt")
	stray := filepath.Join(occupied, "notes.txt")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	if err := os.WriteFile(stray, []byte("not lit's"), 0o644); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	_, err := EnsureDatabase(ctx, occupied, "test-workspace-id")
	if err == nil || !strings.Contains(err.Error(), "holds no database but is not empty") {
		t.Fatalf("EnsureDatabase(occupied root) error = %v, want the not-empty refusal", err)
	}
	if got, err := os.ReadFile(stray); err != nil || string(got) != "not lit's" {
		t.Fatalf("the stray file after the refusal = %q, %v; want it untouched", got, err)
	}
	assertNoBootstrapStaging(t, occupied)
}

func assertNoBootstrapStaging(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(bootstrapStagingPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap staging root after the bootstrap: stat error = %v, want it gone", err)
	}
}
