package store

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
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

// pushedMirror sets up a store pushed once to a fresh bare remote and returns
// its root, the remote, and the mirror the push left, with the push's session
// closed so the mirror is in its between-operations state.
func pushedMirror(t *testing.T) (doltRoot, remote, mirror string) {
	t.Helper()
	ctx := context.Background()
	remote = seedBareGitRemote(t, t.TempDir())
	doltRoot = migratedDoltDir(t)
	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	session, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := session.SyncAddRemote(ctx, "origin", GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := session.SyncCompactAndPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	mirror = soleMirrorGitDir(t, session)
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return doltRoot, remote, mirror
}

// gitAt runs git against one repository with a fixed identity and returns its
// trimmed output.
func gitAt(t *testing.T, gitDir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", gitDir}, args...)...)
	cmd.Env = append(append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, gitDir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// orphanSnapshot writes a commit holding size random bytes into the mirror,
// dated in the past so it sorts as the oldest state, and returns the commit and
// its blob. Only a ref the test points at it keeps it alive.
func orphanSnapshot(t *testing.T, mirror string, size int) (commit, blob string) {
	t.Helper()
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	blob = gitAt(t, mirror, nil, string(payload), "hash-object", "-w", "--stdin")
	tree := gitAt(t, mirror, nil, "100644 blob "+blob+"\ttable\n", "mktree")
	commit = gitAt(t, mirror, []string{"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z", "GIT_AUTHOR_DATE=2020-01-01T00:00:00Z"}, "", "commit-tree", tree, "-m", "a superseded snapshot")
	return commit, blob
}

// TestSweepCollectsOrphanedInstanceRefsAndKeepsTheNewestAsLastKnown drives the
// sweep over the mirror a pre-patch-7 lit leaves: instance refs in both of
// Dolt's namespaces and no last-known ref. The newest orphan's snapshot becomes
// the last-known ref, so the mirror keeps something to negotiate with; every
// orphan goes, and so do the objects only an orphan reached, which is what the
// reclaimed figure counts. A ref shaped almost like an instance ref is not one,
// so it is not the sweep's to delete.
func TestSweepCollectsOrphanedInstanceRefsAndKeepsTheNewestAsLastKnown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, remote, mirror := pushedMirror(t)
	lastKnown := lastKnownRefNamespace + "origin/dolt/data"
	head := gitRevParse(t, remote, "refs/dolt/data")
	gitAt(t, mirror, nil, "", "update-ref", "-d", lastKnown)

	const snapshotSize = 1 << 20
	oldCommit, oldBlob := orphanSnapshot(t, mirror, snapshotSize)
	newestOrphan := "refs/dolt/remotes/origin/dolt/data/" + uuid.NewString()
	oldOrphan := "refs/dolt/remotes/origin/dolt/data/" + uuid.NewString()
	localOrphan := "refs/dolt/blobstore/origin/dolt/data/" + uuid.NewString()
	notAnInstance := "refs/dolt/remotes/origin/dolt/data/{" + uuid.NewString() + "}"
	gitAt(t, mirror, nil, "", "update-ref", newestOrphan, head)
	gitAt(t, mirror, nil, "", "update-ref", oldOrphan, oldCommit)
	gitAt(t, mirror, nil, "", "update-ref", localOrphan, oldCommit)
	gitAt(t, mirror, nil, "", "update-ref", notAnInstance, head)

	session, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer session.Close()
	outcome := session.sweepOrphanedInstanceRefs(ctx)

	if outcome.Problem != "" || outcome.Removed != 3 {
		t.Fatalf("sweep outcome = %+v, want 3 refs removed and no problem", outcome)
	}
	if outcome.Reclaimed < snapshotSize {
		t.Fatalf("sweep reclaimed %d bytes, want at least the %d-byte snapshot only orphans reached", outcome.Reclaimed, snapshotSize)
	}
	want := []string{lastKnown + " " + head, notAnInstance + " " + head}
	sort.Strings(want)
	if got := mirrorRefValues(t, mirror); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mirror refs after sweep =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if err := exec.Command("git", "--git-dir", mirror, "cat-file", "-e", oldBlob).Run(); err == nil {
		t.Fatalf("the snapshot only orphans reached (%s) is still in the mirror", oldBlob)
	}
}

// TestSweepKeepsAnExistingLastKnownRef pins the other arm of the publish: a
// mirror that already has a last-known ref keeps it, even when an orphan names
// a different state, because the last-known ref is what a closed instance saw
// and an orphan is at best a peer of it.
func TestSweepKeepsAnExistingLastKnownRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, remote, mirror := pushedMirror(t)
	head := gitRevParse(t, remote, "refs/dolt/data")
	oldCommit, _ := orphanSnapshot(t, mirror, 1024)
	// Dated after the head, so a sweep that published the newest orphan over
	// an existing last-known ref would pick this one.
	newer := gitAt(t, mirror, []string{"GIT_COMMITTER_DATE=2099-01-01T00:00:00Z"}, "", "commit-tree", oldCommit+"^{tree}", "-m", "a newer-dated orphan")
	gitAt(t, mirror, nil, "", "update-ref", "refs/dolt/remotes/origin/dolt/data/"+uuid.NewString(), newer)

	session, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer session.Close()
	if outcome := session.sweepOrphanedInstanceRefs(ctx); outcome.Problem != "" || outcome.Removed != 1 {
		t.Fatalf("sweep outcome = %+v, want 1 ref removed and no problem", outcome)
	}
	want := []string{lastKnownRefNamespace + "origin/dolt/data " + head}
	if got := mirrorRefValues(t, mirror); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mirror refs after sweep = %v, want %v", got, want)
	}
}

// TestSyncCompactAndPushReportsTheSweep proves the sweep's account reaches the
// push's maintenance report, the line the CLI prints and writes into the sync
// trace's "maintenance" key (TestSyncPushTraceMetadataCarriesTheMaintenanceReport
// pins that half), and that a push over a leaky mirror leaves only the last-known
// ref behind.
func TestSyncCompactAndPushReportsTheSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot, remote, mirror := pushedMirror(t)
	oldCommit, _ := orphanSnapshot(t, mirror, 1<<20)
	gitAt(t, mirror, nil, "", "update-ref", "refs/dolt/remotes/origin/dolt/data/"+uuid.NewString(), oldCommit)

	session, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	commitPayload(t, ctx, session, "c2")
	result, err := session.SyncCompactAndPush(ctx, "origin", "master", false, false)
	if closeErr := session.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	if err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	if !strings.Contains(result.Maintenance, "remote-cache ref sweep: removed 1 orphaned ref, reclaimed ") {
		t.Fatalf("Maintenance = %q, want the sweep's account of the one orphaned ref", result.Maintenance)
	}
	want := []string{lastKnownRefNamespace + "origin/dolt/data " + gitRevParse(t, remote, "refs/dolt/data")}
	if got := mirrorRefValues(t, mirror); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mirror refs after the push = %v, want %v", got, want)
	}
}
