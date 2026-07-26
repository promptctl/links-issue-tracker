package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExplicitReconcileSurfacesUnrelatedHistories is the end-to-end proof of the
// ticket's acceptance: two stores that were initialized INDEPENDENTLY (the consumer
// inits before the producer has pushed, so it never adopts the producer's history)
// share a remote and diverge with no common ancestor. `lit sync reconcile` must
// detect that, exit ExitConflict with an unrelated-histories / no-common-ancestor
// message, and write nothing — never the pre-fix obscure backend error surfaced from
// the base-assuming merge path.
func TestExplicitReconcileSurfacesUnrelatedHistories(t *testing.T) {
	// Drive sync explicitly; the inline auto-sync must not reconcile out from under
	// the assertions.
	t.Setenv(DisableAutoSyncEnvVar, "1")

	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	// Producer initializes its own backlog but does NOT push yet.
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
	runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, producer, "new", "--title", "producer-ticket", "--topic", "demo", "--type", "task")

	// Consumer initializes its OWN backlog while the remote still has no lit data —
	// so it keeps its own bootstrap root instead of adopting the producer's. The two
	// histories are now disjoint.
	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, consumer, "new", "--title", "consumer-ticket", "--topic", "demo", "--type", "task")

	// Producer publishes its history to the shared branch.
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	// The consumer's explicit reconcile fetches the producer's head, sees a
	// divergence with no common ancestor, and surfaces it as ExitConflict.
	out, err := runCLIInDirErr(t, consumer, "sync", "reconcile")
	if err == nil {
		t.Fatalf("expected `sync reconcile` to surface unrelated histories, got success:\n%s", out)
	}
	if code := ExitCode(err); code != ExitConflict {
		t.Fatalf("unrelated reconcile exit code = %d, want %d (ExitConflict)\noutput:\n%s\nerr:\n%v", code, ExitConflict, out, err)
	}
	// The message is a clear domain diagnosis, not the pre-fix obscure backend error.
	msg := err.Error()
	if !strings.Contains(msg, "no common history") && !strings.Contains(msg, "no shared ancestor") {
		t.Fatalf("reconcile error does not name unrelated histories:\n%s", msg)
	}
	if strings.Contains(msg, "no rows in result set") || strings.Contains(msg, `read export at ""`) {
		t.Fatalf("reconcile leaked the pre-fix obscure base-shaped error:\n%s", msg)
	}

	// No partial write: the consumer still holds its own ticket and is still diverged
	// — a re-run surfaces the same state, nothing was merged or dropped.
	if !strings.Contains(runCLIInDir(t, consumer, "backlog"), "consumer-ticket") {
		t.Fatalf("consumer lost its own ticket during an unrelated-histories reconcile")
	}
	if strings.Contains(runCLIInDir(t, consumer, "backlog"), "producer-ticket") {
		t.Fatalf("consumer silently adopted the producer's ticket during an unrelated-histories reconcile")
	}
}
