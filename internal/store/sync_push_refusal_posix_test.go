//go:build !windows

package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestAPushTheRemotesStorageFailsIsNotARefusal is the control beside
// TestAPushRefusedOverAPeersCommitsIsDiverged: a remote made read-only opens,
// then fails the push's write. The remote ruled nothing on history, so the
// failure is not a refusal: it is not judged diverged, and no fetch or
// freshness read is joined to it. The live store holds c4 and is only ahead
// of the remote. A read-only directory is POSIX semantics, hence the build tag.
func TestAPushTheRemotesStorageFailsIsNotARefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	race := buildDivergedClone(t, ctx)
	commitTitled(t, ctx, race.liveRoot, race.epicID, "c4")
	makeTreeReadOnly(t, race.remoteDir)

	st, err := OpenSync(ctx, race.liveRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	_, err = st.SyncPush(ctx, "origin", "master", false, false)
	if closeErr := st.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	if err == nil || errors.As(err, new(pushRefusedError)) || errors.Is(err, storage.ErrPushDiverged) || strings.Contains(err.Error(), "could not be") {
		t.Fatalf("SyncPush() of c4 to a read-only remote error = %v; want the write's failure as it came, not a refusal and not storage.ErrPushDiverged", err)
	}
}

// makeTreeReadOnly makes the directory tree at root read-only until the test
// ends.
func makeTreeReadOnly(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		t.Fatalf("make %s read-only: %v", root, err)
	}
	t.Cleanup(func() {
		if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o755)
		}); err != nil {
			t.Errorf("restore %s's permissions: %v", root, err)
		}
	})
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("make %s read-only: %v", root, err)
	}
}
