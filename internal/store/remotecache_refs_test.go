package store

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRemoteOperationsLeaveTheMirrorHoldingOnlyItsLastKnownRef pins what a
// mirror holds between operations: exactly one ref per remote, the last-known
// remote snapshot, at the remote's current head. Every operation runs in its
// own session and the session is closed before the mirror is read, because a
// closed session is the state every later process inherits.
//
// Both halves of the assertion carry weight. "Only that ref" is the leak: each
// blobstore instance names its refs with its own UUID, so an instance that is
// never closed leaves one behind per operation and each one pins a whole
// superseded snapshot against git gc. "That ref, at the remote's head" is what
// the cure must not lose: a mirror with no refs at all negotiates its next
// fetch with nothing and downloads the whole snapshot again.
func TestRemoteOperationsLeaveTheMirrorHoldingOnlyItsLastKnownRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := seedBareGitRemote(t, t.TempDir())
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	operations := []struct {
		name string
		run  func(*Store) error
	}{
		{"push", func(s *Store) error {
			if err := s.SyncAddRemote(ctx, "origin", GitBackedRemoteURL(remote)); err != nil {
				return err
			}
			_, err := s.SyncCompactAndPush(ctx, "origin", "master", true, false)
			return err
		}},
		{"fetch", func(s *Store) error { return s.SyncFetch(ctx, "origin", false) }},
		{"pull", func(s *Store) error { _, err := s.SyncPull(ctx, "origin", "master"); return err }},
		{"receive", func(s *Store) error { _, err := s.SyncReceive(ctx, "origin", "master"); return err }},
		{"fetch again", func(s *Store) error { return s.SyncFetch(ctx, "origin", false) }},
	}
	var mirror string
	for _, op := range operations {
		session, err := OpenSync(ctx, doltRoot, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync() error = %v", op.name, err)
		}
		runErr := op.run(session)
		mirror = soleMirrorGitDir(t, session)
		if err := session.Close(); err != nil {
			t.Fatalf("%s: Close() error = %v", op.name, err)
		}
		if runErr != nil {
			t.Fatalf("%s: error = %v", op.name, runErr)
		}

		want := []string{"refs/dolt/last-known/origin/dolt/data " + gitRevParse(t, remote, "refs/dolt/data")}
		if got := mirrorRefValues(t, mirror); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("after %s: mirror refs =\n  %s\nwant exactly\n  %s", op.name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	}
}

// soleMirrorGitDir is the bare repo of the one mirror a single-remote store
// keeps, failing the test if there is not exactly one.
func soleMirrorGitDir(t *testing.T, s *Store) string {
	t.Helper()
	keys, err := listRemoteCacheKeys(s.remoteCacheBase())
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("cache keys = %v, want exactly one mirror", keys)
	}
	return filepath.Join(s.remoteCacheBase(), keys[0], "repo.git")
}

// mirrorRefValues lists every ref in a mirror as "<name> <object>", sorted.
func mirrorRefValues(t *testing.T, gitDir string) []string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", gitDir, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		t.Fatalf("for-each-ref in %s: %v", gitDir, err)
	}
	refs := strings.Split(strings.TrimSpace(string(out)), "\n")
	if refs[0] == "" {
		refs = nil
	}
	sort.Strings(refs)
	return refs
}

func gitRevParse(t *testing.T, gitDir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", gitDir, "rev-parse", rev).Output()
	if err != nil {
		t.Fatalf("rev-parse %s in %s: %v", rev, gitDir, err)
	}
	return strings.TrimSpace(string(out))
}
