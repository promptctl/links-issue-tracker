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
	// it is set, which would flatten alpha's and bravo's assignees to one value
	// if this process inherited it from an agent's own shell. Clearing it makes
	// --assignee the real identity source, as CLAUDE_CODE_SESSION_ID is for a
	// real agent session, so this test drives the two-distinct-identities case.
	// The shared-identity case is its own test below, and it transfers too.
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

// TestStartTakesOverAStaleLaneUnderOneSharedIdentity is links-claims-6ghp: the
// takeover that carries no assignee change at all.
//
// Both checkouts name the SAME assignee, which is not a contrived case but the
// ordinary one — a checkout driving no agent session resolves no assignee
// whatsoever, so every checkout one person runs looks identical on that axis,
// and two worktrees of one agent session share a session id. Ownership is keyed
// on the checkout, not the assignee, so this IS a transfer; a write side that
// compared assignees read it as a repeated self-start, exited 0, recorded
// nothing, and left both checkouts believing they held the lane.
// [LAW:no-silent-failure]
func TestStartTakesOverAStaleLaneUnderOneSharedIdentity(t *testing.T) {
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

	// One identity, both checkouts. Every start below names it.
	const shared = "one-identity"
	ticket := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "solo ticket", "--topic", "takeover-e2e", "--type", "task"))
	runCLIInDir(t, alpha, "start", ticket, "--assignee", shared)
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")

	writeTinyFreshnessWindow(t, bravo)
	waitPastFreshnessWindow(t)

	out := runCLIInDir(t, bravo, "start", ticket, "--assignee", shared)
	if !strings.Contains(out, "claim transferred") {
		t.Fatalf("start %s over a stale lane of the same identity = %q, want the transfer announced", ticket, out)
	}
	// The assignee is identical on both sides, so the stream labels are the only
	// thing in that line that can say anything moved.
	if !strings.Contains(out, "stream ") {
		t.Fatalf("transfer notice = %q, want the checkouts named: the assignee is the same on both sides and names nothing", out)
	}

	// The lane is now bravo's, and that is a fact about the RECORD rather than
	// about the line just printed: a second start finds no foreign hold to warn
	// about, which is only true if the first one wrote the establishing event.
	// Bravo's own claim is stale too under the 1ms window — a stale lane of your
	// own is still yours — so a surviving advisory here means the takeover was
	// discarded.
	out = runCLIInDir(t, bravo, "start", ticket, "--assignee", shared)
	if strings.Contains(out, "check for unmerged branches or PRs") {
		t.Fatalf("start %s on bravo's own lane = %q, want no takeover ceremony: the lane never moved", ticket, out)
	}
	if strings.Contains(out, "claim transferred") {
		t.Fatalf("start %s on bravo's own lane = %q, want no transfer notice: nothing moved", ticket, out)
	}
}
