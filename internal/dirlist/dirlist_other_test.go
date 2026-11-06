//go:build !windows

package dirlist

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRead_RefusesFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "owned")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Read(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Read(fifo) err = %v, want ENOTDIR", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read(fifo) blocked: the open waited for a writer instead of refusing a non-directory")
	}
}
