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

// TestARefusalNoMergeCanFixIsNotDiverged is the control beside
// TestAPushRefusedOverAPeersCommitsIsDiverged: a remote made read-only refuses
// every push, and two refusals that a merge cannot fix come back plainly. The
// live store holds c4 and is only ahead of the remote. The clone diverged
// from it, but its push is forced, and a forced push overwrites divergence.
// Each refusal is a judged pushRefusedError: the fetch and the freshness read
// both ran, so the judgment, not a read that failed, is what declined to call
// it diverged. A read-only directory is POSIX semantics, hence the build tag.
func TestARefusalNoMergeCanFixIsNotDiverged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	race := buildDivergedClone(t, ctx)
	commitTitled(t, ctx, race.liveRoot, race.epicID, "c4")
	makeTreeReadOnly(t, race.remoteDir)

	refusals := []struct {
		name  string
		root  string
		force bool
	}{
		{"SyncPush() of c4, only ahead", race.liveRoot, false},
		{"SyncPush(force) of the diverged c3", race.cloneRoot, true},
	}
	for _, refused := range refusals {
		st, err := OpenSync(ctx, refused.root, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync() error = %v", refused.name, err)
		}
		_, err = st.SyncPush(ctx, "origin", "master", false, refused.force)
		if closeErr := st.Close(); closeErr != nil {
			t.Fatalf("%s: Close() error = %v", refused.name, closeErr)
		}
		var refusal pushRefusedError
		if !errors.As(err, &refusal) || errors.Is(err, storage.ErrPushDiverged) || strings.Contains(err.Error(), "could not be") {
			t.Fatalf("%s to a read-only remote error = %v; want a judged refusal that is not storage.ErrPushDiverged", refused.name, err)
		}
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
