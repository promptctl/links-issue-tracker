package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutomaticReconcileLinearizesDivergedClone is the end-to-end proof that a
// diverged clone reconciles itself: the consumer holds a local unpushed edit
// while another machine pushes a different edit to the same ticket, then the
// consumer runs an ordinary command and the inline receive — finding a
// divergence a fast-forward cannot absorb — runs the field-aware reconcile, so
// the consumer transparently ends up with BOTH edits and linear history that
// fast-forward pushes. No manual `lit sync pull`, no merge commit.
func TestAutomaticReconcileLinearizesDivergedClone(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	// Producer publishes a backlog with one ticket.
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
	ticketID := extractTicketID(t, runCLIInDir(t, producer, "new", "--title", "shared-ticket", "--topic", "demo", "--type", "task"))
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	// Consumer adopts the backlog on init.
	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	if !strings.Contains(runCLIInDir(t, consumer, "backlog"), "shared-ticket") {
		t.Fatalf("consumer did not adopt shared-ticket on init")
	}

	// Producer edits the ticket's LANE and pushes; consumer edits its PRIORITY
	// locally (unpushed). Same ticket, different code-owned fields -> divergence.
	runCLIInDir(t, producer, "update", ticketID, "--lane", "alpha")
	runCLIInDir(t, producer, "sync", "push")
	runCLIInDir(t, consumer, "update", ticketID, "--priority", "1")

	// Run an ordinary command with automatic sync enabled: the inline receive
	// fires, sees the divergence, and reconciles it into linear history.
	t.Setenv(disableAutoSyncEnvVar, "0")
	runCLIInDir(t, consumer, "ready")

	// The consumer now carries BOTH edits.
	show := runCLIInDir(t, consumer, "show", ticketID)
	if !strings.Contains(show, "alpha") {
		t.Fatalf("consumer missing producer's lane edit after reconcile:\n%s", show)
	}
	if !strings.Contains(strings.ToLower(show), "urgent") {
		t.Fatalf("consumer missing its own priority edit after reconcile:\n%s", show)
	}

	// Linear history fast-forward pushes with no force.
	push := runCLIInDir(t, consumer, "sync", "push")
	if strings.Contains(strings.ToLower(push), "error") || strings.Contains(strings.ToLower(push), "rejected") {
		t.Fatalf("consumer fast-forward push after reconcile failed:\n%s", push)
	}

	// Producer fast-forwards to the reconciled head and sees both edits.
	t.Setenv(disableAutoSyncEnvVar, "0")
	runCLIInDir(t, producer, "ready")
	producerShow := runCLIInDir(t, producer, "show", ticketID)
	if !strings.Contains(producerShow, "alpha") || !strings.Contains(strings.ToLower(producerShow), "urgent") {
		t.Fatalf("producer missing converged edits after fast-forward receive:\n%s", producerShow)
	}
}

// extractTicketID pulls the created ticket id from `lit new` output. lit new
// prints the id as the first token of its first line; capturing it (rather than
// guessing) is the only safe way to address the new ticket.
func extractTicketID(t *testing.T, newOutput string) string {
	t.Helper()
	fields := strings.Fields(newOutput)
	if len(fields) == 0 {
		t.Fatalf("could not find ticket id in lit new output:\n%s", newOutput)
	}
	return fields[0]
}
