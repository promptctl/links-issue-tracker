package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A temp file the take cannot remove is reported beside the snapshot the take
// still writes, and the take still removes every other one. Darwin's immutable
// flag is the one way an unprivileged test can make a file in a writable
// directory refuse its removal.
func TestATempFileThatWillNotGoCostsTheSetNoSnapshot(t *testing.T) {
	t.Parallel()
	storageDir := t.TempDir()
	if err := os.MkdirAll(dir(storageDir), 0o755); err != nil {
		t.Fatalf("make the backups dir: %v", err)
	}
	stuck := filepath.Join(dir(storageDir), "stuck.json.tmp-1")
	removable := filepath.Join(dir(storageDir), "removable.json.tmp-2")
	for _, residue := range []string{stuck, removable} {
		if err := os.WriteFile(residue, []byte("{"), 0o644); err != nil {
			t.Fatalf("leave a killed take's temp file: %v", err)
		}
	}
	if err := unix.Chflags(stuck, unix.UF_IMMUTABLE); err != nil {
		t.Fatalf("make %s immutable: %v", stuck, err)
	}
	t.Cleanup(func() {
		if err := unix.Chflags(stuck, 0); err != nil {
			t.Errorf("clear %s's immutable flag: %v", stuck, err)
		}
	})

	taken, err := Take(context.Background(), storageDir, Budget, readExport)
	if err == nil || !strings.Contains(err.Error(), stuck) {
		t.Errorf("Take error = %v, want one naming %s", err, stuck)
	}
	if taken == nil || taken.Written == nil || taken.ResidueRemoved != 1 {
		t.Fatalf("Take = %+v; want a snapshot and the removable temp file removed", taken)
	}
	if _, err := os.Stat(taken.Written.Path); err != nil {
		t.Errorf("the snapshot was not written: stat = %v", err)
	}
	if _, err := os.Stat(removable); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the removable temp file survived: stat = %v", err)
	}
}
