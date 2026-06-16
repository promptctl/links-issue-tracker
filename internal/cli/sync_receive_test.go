package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutomaticReceiveFastForwardsEstablishedClone is the end-to-end proof of the
// feature: an established clone that adopted the remote on init, then has another
// machine push a new ticket, sees that ticket after running an ordinary command —
// with NO manual `lit sync pull`. It drives the real CLI for both clones over a
// real git remote. The receive is inline (it runs after a command, in the same
// process, once the command's engine is closed), so the assertion is deterministic
// rather than racing a background worker.
func TestAutomaticReceiveFastForwardsEstablishedClone(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	// Producer publishes the initial backlog, then a second ticket.
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
	runCLIInDir(t, producer, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	// Consumer adopts the producer's backlog on init.
	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	if !strings.Contains(runCLIInDir(t, consumer, "backlog"), "first-ticket") {
		t.Fatalf("consumer did not adopt first-ticket on init")
	}

	// Another machine pushes a second ticket to the remote.
	runCLIInDir(t, producer, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push")

	// With automatic sync still disabled (TestMain default), the established
	// consumer cannot yet see it.
	if strings.Contains(runCLIInDir(t, consumer, "backlog"), "second-ticket") {
		t.Fatalf("consumer saw second-ticket before any receive — test cannot prove receive")
	}

	// Enable automatic receive for this test, then run an ordinary command: the
	// inline receive fires after it and fast-forwards the consumer.
	t.Setenv(disableAutoSyncEnvVar, "0")
	runCLIInDir(t, consumer, "ready")

	backlog := runCLIInDir(t, consumer, "backlog")
	if !strings.Contains(backlog, "second-ticket") {
		t.Fatalf("consumer backlog missing second-ticket after automatic receive:\n%s", backlog)
	}
}
