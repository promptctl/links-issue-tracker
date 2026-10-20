package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/fsperm"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestAPushTheRemotesStorageFailsIsNotARefusal is the control beside
// TestAPushRefusedOverAPeersCommitsIsDiverged: a remote made read-only opens,
// then fails the push's write. The remote ruled nothing on history, so
// neither failure is a refusal: neither is judged diverged, and no fetch or
// freshness read is joined to it. The live store holds c4 and is only ahead
// of the remote. The clone diverged from it and pushes forced, so judging its
// failure would tell the user to merge what --force meant to overwrite.
func TestAPushTheRemotesStorageFailsIsNotARefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	race := buildDivergedClone(t, ctx)
	commitTitled(t, ctx, race.liveRoot, race.epicID, "c4")
	makeTreeReadOnly(t, race.remoteDir)

	for _, failed := range []struct {
		name  string
		root  string
		force bool
	}{
		{"SyncPush() of c4, only ahead", race.liveRoot, false},
		{"SyncPush(force) of the diverged c3", race.cloneRoot, true},
	} {
		st, err := OpenSync(ctx, failed.root, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync() error = %v", failed.name, err)
		}
		_, err = st.SyncPush(ctx, remoteName(t, "origin"), "master", false, failed.force)
		if closeErr := st.Close(); closeErr != nil {
			t.Fatalf("%s: Close() error = %v", failed.name, closeErr)
		}
		if err == nil || errors.As(err, new(pushRefusedError)) || errors.Is(err, storage.ErrPushDiverged) || strings.Contains(err.Error(), "could not be") {
			t.Fatalf("%s to a read-only remote error = %v; want the write's failure as it came, not a refusal and not storage.ErrPushDiverged", failed.name, err)
		}
	}
}

// makeTreeReadOnly makes the directory tree at root read-only until the test
// ends: nothing can be created in root, and no file in the tree written.
func makeTreeReadOnly(t *testing.T, root string) {
	t.Helper()
	fsperm.DenyWrite(t, root)
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
}
