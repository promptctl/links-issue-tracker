package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartRefusesAndThenTakesOverAFreshForeignClaim is the ticket's own
// acceptance line, driven end-to-end: "an agent without a TTY can take over
// a fresh-claimed lane only by passing the explicit flag." Two checkouts
// over one store, exactly as claims_attribution_test.go and
// next_claims_e2e_test.go drive their scenarios — the claim being tested is
// about evidence that actually made the trip through sync, not an in-process
// stub.
func TestStartRefusesAndThenTakesOverAFreshForeignClaim(t *testing.T) {
	// resolveIdentity prefers CLAUDE_CODE_SESSION_ID over --assignee whenever
	// it is set, which would flatten alpha's and bravo's assignees to one
	// value if this process inherited it from an agent's own shell — and a
	// same-state start whose assignee does not change is store.Apply's
	// documented no-op, so the claim would never actually transfer. Clearing
	// it makes --assignee the real identity source, as CLAUDE_CODE_SESSION_ID
	// is for a real agent session.
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

	ticket := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "solo ticket", "--topic", "takeover-e2e", "--type", "task"))
	runCLIInDir(t, alpha, "start", ticket, "--assignee", "alpha-agent")
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")

	// Fresh foreign claim, no --take: refused, and the refusal names the
	// holder and instructs the flag rather than failing silently.
	// [LAW:no-silent-failure]
	out, err := runCLIInDirAllowError(t, bravo, "start", ticket, "--assignee", "bravo-agent")
	if err == nil {
		t.Fatalf("start %s without --take = %q, want a refusal (alpha's claim is fresh)", ticket, out)
	}
	if !strings.Contains(err.Error(), "--take") {
		t.Fatalf("refusal error = %v, want it to instruct --take", err)
	}
	if !strings.Contains(err.Error(), "claimed") {
		t.Fatalf("refusal error = %v, want provenance naming the current holder", err)
	}

	// Same command, --take: proceeds, and the takeover is visible in the
	// output rather than silent.
	out = runCLIInDir(t, bravo, "start", ticket, "--assignee", "bravo-agent", "--take")
	if !strings.Contains(out, "taking over") {
		t.Fatalf("start %s --take = %q, want it to announce the takeover", ticket, out)
	}

	// The lane is now bravo's: starting it again, still as bravo-agent, is
	// the happy path — no confirmation, no warning, no ceremony.
	out = runCLIInDir(t, bravo, "start", ticket, "--assignee", "bravo-agent")
	if strings.Contains(out, "claimed") || strings.Contains(out, "--take") {
		t.Fatalf("start %s on bravo's own lane = %q, want no takeover ceremony", ticket, out)
	}
}

// TestStartOnAStaleForeignClaimProceedsInformed is the ticket's other half:
// "a stale takeover proceeds unprompted but prints the provenance and the
// unmerged-work instruction." The freshness window is configured down to
// force alpha's claim stale by the time bravo looks, rather than waiting out
// the real default — the same technique config_test.go uses to make
// claims.freshness_window a controllable test input.
func TestStartOnAStaleForeignClaimProceedsInformed(t *testing.T) {
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

	ticket := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "solo ticket", "--topic", "takeover-e2e", "--type", "task"))
	runCLIInDir(t, alpha, "start", ticket, "--assignee", "alpha-agent")
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")

	writeTinyFreshnessWindow(t, bravo)
	waitPastFreshnessWindow(t)

	out := runCLIInDir(t, bravo, "start", ticket, "--assignee", "bravo-agent")
	if !strings.Contains(out, "check for unmerged branches or PRs") {
		t.Fatalf("start %s over a stale claim = %q, want the unmerged-work advisory", ticket, out)
	}
	if !strings.Contains(out, "stale") {
		t.Fatalf("start %s over a stale claim = %q, want the provenance to say stale", ticket, out)
	}
}

// waitPastFreshnessWindow sleeps long enough that any evidence timestamped
// before the call reads as older than a 1ms claims.freshness_window.
func waitPastFreshnessWindow(t *testing.T) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
}

// writeTinyFreshnessWindow points dir's workspace config at a
// claims.freshness_window short enough that any evidence already on disk
// reads as stale the moment waitPastFreshnessWindow returns.
func writeTinyFreshnessWindow(t *testing.T, dir string) {
	t.Helper()
	litDir := filepath.Join(dir, ".lit")
	if err := os.MkdirAll(litDir, 0o755); err != nil {
		t.Fatalf("mkdir %s error = %v", litDir, err)
	}
	config := "[claims]\nfreshness_window = \"1ms\"\n"
	if err := os.WriteFile(filepath.Join(litDir, "config.toml"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config.toml error = %v", err)
	}
}
