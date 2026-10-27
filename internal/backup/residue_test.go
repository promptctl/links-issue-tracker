package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/primitives/filelock"
)

// killedWriteEnv names the path a re-run of this test binary starts a write
// to and is killed inside, as a take killed mid-write is.
const killedWriteEnv = "LIT_BACKUP_TEST_KILLED_WRITE"

func TestMain(m *testing.M) {
	if path := os.Getenv(killedWriteEnv); path != "" {
		err := atomicfile.Replace(path, 0o644, atomicfile.Durable, func(w io.Writer) error {
			if _, err := w.Write([]byte("{")); err != nil {
				return err
			}
			time.Sleep(time.Hour)
			return nil
		})
		fmt.Fprintf(os.Stderr, "the write to %s outlived its kill: %v\n", path, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// killMidWrite runs this test binary again as a write to path, kills it once
// its temp file stands beside path, and returns that temp file. The writer is
// killed on every way out, a failed wait for its temp file included, so none
// outlives the test.
func killMidWrite(t *testing.T, path string) string {
	t.Helper()
	writer := exec.Command(os.Args[0], "-test.run=^$")
	writer.Env = append(os.Environ(), killedWriteEnv+"="+path)
	writer.Stderr = os.Stderr
	if err := writer.Start(); err != nil {
		t.Fatalf("start the writer: %v", err)
	}
	defer func() {
		if err := writer.Process.Kill(); err != nil {
			t.Errorf("kill the writer: %v", err)
		}
		var exit *exec.ExitError
		if err := writer.Wait(); !errors.As(err, &exit) {
			t.Errorf("wait for the killed writer = %v, want its kill", err)
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		temps, err := filepath.Glob(path + ".tmp-*")
		if err != nil {
			t.Fatalf("glob the writer's temp file: %v", err)
		}
		if len(temps) == 1 {
			return temps[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the writer made no temp file beside %s in 30s (found %v)", path, temps)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readExport() (model.Export, error) {
	return model.Export{Version: 1, WorkspaceID: "ws-test"}, nil
}

// The next take removes the temp file a take killed mid-write left, and
// leaves the temp file of anything that is not a snapshot, and a directory
// that only wears a temp file's name.
func TestTakeRemovesTheTempFileOfAKilledTake(t *testing.T) {
	t.Parallel()
	storageDir := t.TempDir()
	if err := os.MkdirAll(dir(storageDir), 0o755); err != nil {
		t.Fatalf("make the backups dir: %v", err)
	}
	residue := killMidWrite(t, filepath.Join(dir(storageDir), "killed.json"))
	foreign := filepath.Join(dir(storageDir), "notes.txt.tmp-1")
	if err := os.WriteFile(foreign, []byte("mine"), 0o644); err != nil {
		t.Fatalf("write a temp file of something else: %v", err)
	}
	directory := filepath.Join(dir(storageDir), "kept.json.tmp-1")
	if err := os.MkdirAll(filepath.Join(directory, "inside"), 0o755); err != nil {
		t.Fatalf("make a directory under a temp file's name: %v", err)
	}

	taken, err := Take(context.Background(), storageDir, Budget, readExport)
	if err != nil || taken == nil || taken.Written == nil || taken.ResidueRemoved != 1 {
		t.Fatalf("Take = %+v, %v; want a snapshot and the killed take's temp file removed", taken, err)
	}
	if _, err := os.Stat(residue); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the killed take's temp file survived: stat = %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a temp file of something other than a snapshot was removed: stat = %v", err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Errorf("a directory under a temp file's name was removed: stat = %v", err)
	}
}

// A take whose read fails has still removed what killed takes left, and says
// so: it held the lock, and the removal runs before the read.
func TestATakeThatWritesNothingStillReportsTheResidueItRemoved(t *testing.T) {
	t.Parallel()
	storageDir := t.TempDir()
	if err := os.MkdirAll(dir(storageDir), 0o755); err != nil {
		t.Fatalf("make the backups dir: %v", err)
	}
	residue := killMidWrite(t, filepath.Join(dir(storageDir), "killed.json"))
	readFailed := errors.New("read failed")

	taken, err := Take(context.Background(), storageDir, Budget, func() (model.Export, error) {
		return model.Export{}, readFailed
	})
	if !errors.Is(err, readFailed) || taken == nil || taken.Written != nil || taken.ResidueRemoved != 1 {
		t.Fatalf("Take = %+v, %v; want the read's error, no snapshot, and one temp file removed", taken, err)
	}
	if _, err := os.Stat(residue); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the killed take's temp file survived: stat = %v", err)
	}
}

// A take never removes the temp file of a take still writing: the writing take
// holds the set's lock, so the other waits on it and, given up, has held
// nothing and removed nothing.
func TestTakeLeavesTheTempFileOfATakeStillWriting(t *testing.T) {
	t.Parallel()
	storageDir := t.TempDir()
	release, acquired, err := filelock.Acquire(context.Background(), lockPath(storageDir), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("hold the backups lock: acquired=%v, %v", acquired, err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release the backups lock: %v", err)
		}
	})
	if err := os.MkdirAll(dir(storageDir), 0o755); err != nil {
		t.Fatalf("make the backups dir: %v", err)
	}
	path := filepath.Join(dir(storageDir), "live.json")
	inFlight := make(chan string, 1)
	finish := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- atomicfile.Replace(path, 0o644, atomicfile.Durable, func(w io.Writer) error {
			temps, err := filepath.Glob(path + ".tmp-*")
			if err != nil || len(temps) != 1 {
				return fmt.Errorf("find the in-flight temp file: %v, %v", temps, err)
			}
			inFlight <- temps[0]
			<-finish
			_, err = w.Write([]byte("{}"))
			return err
		})
	}()
	temp := <-inFlight

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	taken, err := Take(ctx, storageDir, Budget, readExport)
	if !errors.Is(err, context.DeadlineExceeded) || taken != nil {
		t.Fatalf("Take beside a take still writing = %+v, %v; want it to give up without holding the lock", taken, err)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("the writing take's temp file is gone: stat = %v", err)
	}
	close(finish)
	if err := <-written; err != nil {
		t.Fatalf("the writing take's write = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "{}" {
		t.Errorf("the writing take's snapshot = %q, %v; want {}", got, err)
	}
}
