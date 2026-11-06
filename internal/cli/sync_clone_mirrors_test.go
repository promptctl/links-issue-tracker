package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// pulledConsumer is a workspace that has pulled from a remote another
// workspace pushed to, so its live store holds a git mirror of that remote.
func pulledConsumer(t *testing.T) workspace.Info {
	t.Helper()
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks")
	epicID := newEpicInDir(t, producer, "shared-epic")
	runCLIInDir(t, producer, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks")
	runCLIInDir(t, consumer, "sync", "pull")

	ws, err := workspace.Resolve(runEnv(t), consumer)
	if err != nil {
		t.Fatalf("resolve consumer workspace: %v", err)
	}
	return ws
}

// mirrorRefs is every ref of every mirror in base, as `git for-each-ref`
// prints them, keyed by mirror.
func mirrorRefs(t *testing.T, base string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read mirror base %s: %v", base, err)
	}
	refs := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		out, err := exec.Command("git", "--git-dir", filepath.Join(base, entry.Name(), "repo.git"), "for-each-ref").CombinedOutput()
		if err != nil {
			t.Fatalf("list refs of mirror %s: %v: %s", entry.Name(), err, out)
		}
		refs[entry.Name()] = string(out)
	}
	return refs
}

// A receive's clone starts with the live store's mirrors, refs and all, so
// its fetch asks the remote only for what the live mirrors lack; and removing
// the clone removes its mirrors, which live outside its tree.
func TestAReceiveCloneStartsWithTheLiveMirrorsAndTakesItsOwnAwayWithIt(t *testing.T) {
	t.Parallel()
	ws := pulledConsumer(t)
	mirrorRoot, err := store.MirrorRootOf(testEnv())
	if err != nil {
		t.Fatal(err)
	}
	live := mirrorRefs(t, store.MirrorBase(mirrorRoot, ws.DatabasePath))
	if len(live) == 0 {
		t.Fatal("the consumer's pull left no live mirror")
	}

	clone, err := takeReceiveClone(context.Background(), testStderr(t), testEnv(), ws)
	if err != nil {
		t.Fatalf("takeReceiveClone() error = %v", err)
	}
	cloneBase := store.MirrorBase(mirrorRoot, clone.databasePath)
	seeded := mirrorRefs(t, cloneBase)
	for key, refs := range live {
		if seeded[key] != refs {
			t.Fatalf("the clone's mirror %s holds refs\n%s\nbefore its fetch; want the live mirror's\n%s", key, seeded[key], refs)
		}
	}

	removeReceiveClone(testStderr(t), clone)
	for _, gone := range []string{clone.dir, cloneBase} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s outlived the clone's removal (stat err = %v)", gone, err)
		}
	}
	if _, err := os.Stat(store.MirrorBase(mirrorRoot, ws.DatabasePath)); err != nil {
		t.Fatalf("the live mirror base is gone after the clone's removal: %v", err)
	}
}

// A clone a killed receive left behind, tree and mirrors, is collected by the
// next receive's take.
func TestTheNextTakeCollectsAKilledReceivesCloneAndItsMirrors(t *testing.T) {
	t.Parallel()
	ws := pulledConsumer(t)
	mirrorRoot, err := store.MirrorRootOf(testEnv())
	if err != nil {
		t.Fatal(err)
	}
	// The first take's clone is never removed, as a SIGKILL leaves it.
	dead, err := takeReceiveClone(context.Background(), testStderr(t), testEnv(), ws)
	if err != nil {
		t.Fatalf("takeReceiveClone(first) error = %v", err)
	}
	deadBase := store.MirrorBase(mirrorRoot, dead.databasePath)
	if _, err := os.Stat(deadBase); err != nil {
		t.Fatalf("the first clone has no mirror base to leave behind: %v", err)
	}

	next, err := takeReceiveClone(context.Background(), testStderr(t), testEnv(), ws)
	if err != nil {
		t.Fatalf("takeReceiveClone(next) error = %v", err)
	}
	defer removeReceiveClone(testStderr(t), next)
	for _, gone := range []string{dead.dir, deadBase} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s of the killed receive's clone outlived the next take (stat err = %v)", gone, err)
		}
	}
}
