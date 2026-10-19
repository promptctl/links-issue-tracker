package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestPushAndCloneLeaveNothingOfTheirRemoteOpen pins that a store holds a
// remote's files only while it is open. A push and a clone each open the
// remote inside the engine; once the store that ran them is closed, no file
// under the remote's directory or the stores' is still open in this process.
// Windows refuses to remove a directory holding an open file, so a handle
// that outlives its store is a backlog that cannot be moved, re-adopted, or
// cleaned up there.
func TestPushAndCloneLeaveNothingOfTheirRemoteOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve TempDir: %v", err)
	}
	remoteDir := filepath.Join(base, "remote")
	remoteURL := "file://" + remoteDir

	seedReconcileRemote(t, ctx, filepath.Join(base, "producer"), remoteURL)
	assertNoOpenFileUnder(t, "after the push's store closed", remoteDir)

	if err := AdoptRemoteByClone(ctx, filepath.Join(base, "consumer"), "ws", "origin", remoteURL, "master"); err != nil {
		t.Fatalf("AdoptRemoteByClone: %v", err)
	}
	assertNoOpenFileUnder(t, "after the clone returned", base)
}

func assertNoOpenFileUnder(t *testing.T, when, dir string) {
	t.Helper()
	var held []string
	for _, p := range openFiles(t) {
		if strings.HasPrefix(p, dir+string(filepath.Separator)) {
			held = append(held, p)
		}
	}
	if len(held) > 0 {
		t.Fatalf("%s, this process still holds %d file(s) under %s open:\n%s", when, len(held), dir, strings.Join(held, "\n"))
	}
}
