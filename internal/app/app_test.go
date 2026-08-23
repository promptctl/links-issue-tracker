package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// TestOpenModeContract pins the behavioral split the mode value carries:
// write bootstraps a missing database, read refuses one. [LAW:behavior-not-structure]
func TestOpenModeContract(t *testing.T) {
	cases := []struct {
		name string
		mode AccessMode
		// wantErr is the substring an uninitialized workspace must fail
		// with; empty means open must succeed and bootstrap the database.
		wantErr string
	}{
		{name: "write bootstraps uninitialized workspace", mode: AccessWrite},
		{name: "read refuses uninitialized workspace", mode: AccessRead, wantErr: "not initialized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := gitRepo(t)
			ap, err := Open(context.Background(), repo, tc.mode)
			if tc.wantErr != "" {
				if err == nil {
					ap.Close()
					t.Fatalf("Open(%v) on uninitialized workspace succeeded, want error containing %q", tc.mode, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Open(%v) error = %q, want substring %q", tc.mode, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%v) error = %v", tc.mode, err)
			}
			if err := ap.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
}

// TestOpenReadAfterWrite pins that read mode accepts the database write mode
// bootstrapped — the two modes describe one store, not two.
func TestOpenReadAfterWrite(t *testing.T) {
	repo := gitRepo(t)
	writer, err := Open(context.Background(), repo, AccessWrite)
	if err != nil {
		t.Fatalf("Open(write) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(write) error = %v", err)
	}
	reader, err := Open(context.Background(), repo, AccessRead)
	if err != nil {
		t.Fatalf("Open(read) after write error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(read) error = %v", err)
	}
}

// TestOpenRejectsUnknownMode pins that an unrecognized mode — including the
// zero value — fails closed instead of being granted write access.
// [LAW:no-silent-failure]
func TestOpenRejectsUnknownMode(t *testing.T) {
	repo := gitRepo(t)
	for _, mode := range []AccessMode{"", "admin"} {
		ap, err := Open(context.Background(), repo, mode)
		if err == nil {
			ap.Close()
			t.Fatalf("Open(%q) succeeded, want invalid-mode error", mode)
		}
		if !strings.Contains(err.Error(), "invalid access mode") {
			t.Fatalf("Open(%q) error = %q, want invalid-mode error", mode, err)
		}
	}
}

// gitRepoWithWorktree returns an initialized repo and a linked worktree of it.
// The worktree is the honest fixture for "a checkout that has never mutated":
// it shares the primary's store, so the database exists, while its own private
// git directory is untouched.
func gitRepoWithWorktree(t *testing.T) (string, string) {
	t.Helper()
	repo := gitRepo(t)
	commit := exec.Command("git", "-c", "user.name=T", "-c", "user.email=t@e", "commit", "--allow-empty", "-m", "init")
	commit.Dir = repo
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	// Bootstraps the database, which the read mode below requires to exist.
	ap, err := Open(context.Background(), repo, AccessWrite)
	if err != nil {
		t.Fatalf("Open(AccessWrite) error = %v", err)
	}
	if err := ap.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	add := exec.Command("git", "worktree", "add", linked)
	add.Dir = repo
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	return repo, linked
}

func openStream(t *testing.T, dir string, mode AccessMode) workspace.StreamID {
	t.Helper()
	ap, err := Open(context.Background(), dir, mode)
	if err != nil {
		t.Fatalf("Open(%v) error = %v", mode, err)
	}
	defer ap.Close()
	return ap.Stream
}

// TestAccessModeSelectsIdentityContract pins the pairing that accessContracts
// exists to guarantee: the mode that may write the store is the same mode that
// may mint an identity. Swapping the two resolveStream entries in that map is
// the regression this catches — write mode would stop minting and read mode
// would start. [LAW:behavior-not-structure] Asserted through App.Stream alone,
// never by inspecting the token file, so the test states the contract rather
// than the storage.
func TestAccessModeSelectsIdentityContract(t *testing.T) {
	repo, linked := gitRepoWithWorktree(t)

	if got := openStream(t, repo, AccessWrite); !got.Present() {
		t.Fatal("a write-mode open must mint this checkout's identity")
	}
	if got := openStream(t, linked, AccessRead); got.Present() {
		t.Fatalf("a read-mode open in a never-mutated checkout must mint nothing; got %q", got.Value())
	}
	// The proof that the read above created nothing: a second read still finds
	// no identity. Had the first one minted, this would report one.
	if got := openStream(t, linked, AccessRead); got.Present() {
		t.Fatalf("the first read-mode open minted an identity; second read found %q", got.Value())
	}
	// And the worktree gets its own identity once it actually mutates.
	worktreeID := openStream(t, linked, AccessWrite)
	if !worktreeID.Present() {
		t.Fatal("a write-mode open in the worktree must mint an identity")
	}
	if worktreeID.Value() == openStream(t, repo, AccessRead).Value() {
		t.Fatalf("the worktree and the primary must not share an identity; both %q", worktreeID.Value())
	}
}

// TestReadAdoptsTheIdentityAWriteMinted states what makes a fresh session
// inherit its checkout's stream: the identity comes off disk, so a read-mode
// command sees exactly what an earlier write-mode command minted.
func TestReadAdoptsTheIdentityAWriteMinted(t *testing.T) {
	repo := gitRepo(t)
	minted := openStream(t, repo, AccessWrite)
	if !minted.Present() {
		t.Fatal("write-mode open minted nothing")
	}
	for range 3 {
		if got := openStream(t, repo, AccessRead); got.Value() != minted.Value() {
			t.Fatalf("read-mode open saw %q, want the minted %q", got.Value(), minted.Value())
		}
	}
}

// TestFailedIdentityResolutionReleasesTheStore covers the abort path in Open:
// when identity resolution fails after the store is already open, the store
// must be closed, because Store.Close is also what releases the workspace lock.
// A leak there is invisible at the failure and resurfaces later as an
// unexplained "workspace busy" with nothing pointing back here.
//
// The proof is behavioral rather than an assertion about the error value: the
// checkout is repaired and opened again, which can only succeed if the failed
// open released what it held. [LAW:behavior-not-structure]
func TestFailedIdentityResolutionReleasesTheStore(t *testing.T) {
	repo := gitRepo(t)
	if minted := openStream(t, repo, AccessWrite); !minted.Present() {
		t.Fatal("write-mode open minted nothing")
	}
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// The workspace package owns this filename; naming it here couples the two,
	// but the coupling fails loudly rather than silently — a rename makes the
	// damaged-token open below succeed, and this test fails.
	tokenPath := filepath.Join(ws.PrivateGitDir, "lit-stream")
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("expected a minted token at %s: %v", tokenPath, err)
	}
	if err := os.WriteFile(tokenPath, []byte("not a token"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	ap, err := Open(context.Background(), repo, AccessWrite)
	if err == nil {
		ap.Close()
		t.Fatal("Open must fail when the checkout's identity is damaged")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("Open error = %q, want the malformed-token diagnosis", err)
	}

	// Repairing the checkout must make it usable again. If the aborted open had
	// leaked the store, this second open would fail on the still-held lock.
	if err := os.Remove(tokenPath); err != nil {
		t.Fatalf("Remove error = %v", err)
	}
	if got := openStream(t, repo, AccessWrite); !got.Present() {
		t.Fatal("a repaired checkout must open and mint a fresh identity")
	}
}
