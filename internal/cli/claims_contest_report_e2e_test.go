package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncReconcileNamesContestedLaneAfterPartitionedDoubleStart is the
// ticket's own acceptance line (links-claims-1ihf.8): two partitioned
// checkouts start the same lane before either sees the other's push, and the
// first moment either CAN know is when the histories meet at reconcile. Both
// checkouts' establishing evidence lands; `lit sync reconcile` names the
// contest rather than routing silently.
func TestSyncReconcileNamesContestedLaneAfterPartitionedDoubleStart(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	alpha := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, alpha, "config", "user.email", "a@a.co")
	runGit(t, alpha, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(alpha, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, alpha, "add", "-A")
	runGit(t, alpha, "commit", "-m", "seed")
	runGit(t, alpha, "push", "origin", "HEAD")
	runCLIInDir(t, alpha, "init", "--skip-hooks", "--skip-agents")
	ticket := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "contested lane", "--topic", "contest-e2e", "--type", "task"))
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	// bravo clones AFTER the ticket exists on the remote but BEFORE either
	// side starts it — the partition the ticket describes: from here, neither
	// checkout can see the other's establishing event until they sync again.
	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")

	runCLIInDir(t, alpha, "start", ticket, "--assignee", "alpha-agent")
	runCLIInDir(t, alpha, "sync", "push")

	// bravo starts the SAME lane without ever having pulled alpha's start —
	// the double-start the design says a partition cannot prevent.
	runCLIInDir(t, bravo, "start", ticket, "--assignee", "bravo-agent")

	// The histories meet: bravo's explicit reconcile is the first moment
	// either side CAN know about the other's evidence.
	out := runCLIInDir(t, bravo, "sync", "reconcile")
	if !strings.Contains(out, "contested") {
		t.Fatalf("sync reconcile after a partitioned double-start = %q, want it to name the contest", out)
	}
	if !strings.Contains(out, ticket) {
		t.Fatalf("sync reconcile contest report = %q, want it to name the contested lane %s", out, ticket)
	}
}

// TestSyncReconcileNoContestNamesNoContest proves the negative half of the
// acceptance line: an ordinary reconcile with no double-start says nothing
// about contests at all, and leaves the database exactly as an uninstrumented
// reconcile would.
func TestSyncReconcileNoContestNamesNoContest(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	alpha := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, alpha, "config", "user.email", "a@a.co")
	runGit(t, alpha, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(alpha, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, alpha, "add", "-A")
	runGit(t, alpha, "commit", "-m", "seed")
	runGit(t, alpha, "push", "origin", "HEAD")
	runCLIInDir(t, alpha, "init", "--skip-hooks", "--skip-agents")
	ticketA := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "alphas ticket", "--topic", "contest-e2e", "--type", "task"))
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")

	// Ordinary divergence with no shared lane: alpha starts its own ticket,
	// bravo files an unrelated one. Reconciling this merges histories with no
	// contest anywhere in them.
	runCLIInDir(t, alpha, "start", ticketA, "--assignee", "alpha-agent")
	runCLIInDir(t, alpha, "sync", "push")
	runCLIInDir(t, bravo, "new", "--title", "bravos ticket", "--topic", "contest-e2e", "--type", "task")

	out := runCLIInDir(t, bravo, "sync", "reconcile")
	if strings.Contains(out, "contested") {
		t.Fatalf("sync reconcile with no shared lane = %q, want no mention of contests", out)
	}
}
