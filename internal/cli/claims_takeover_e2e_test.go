package cli

import (
	"os"
	"path/filepath"
	"regexp"
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
	// What the caller is told to do next is the rendered remediation, and it
	// once contradicted the message: exit 1 under "Retry the command … run `lit
	// doctor`", a retry refused identically and a diagnosis of a healthy
	// workspace (links-cli-errors-iz41). The gate is a refusal, and the act that
	// clears it is the flag.
	var stderr strings.Builder
	if code := WriteCommandError(&stderr, err); code != ExitValidation {
		t.Fatalf("refusal exit = %d, want %d (a refusal, not a fault):\n%s", code, ExitValidation, stderr.String())
	}
	_, remediation, found := strings.Cut(stderr.String(), "remediation: ")
	if !found {
		t.Fatalf("refusal rendered no remediation line:\n%s", stderr.String())
	}
	if !strings.Contains(remediation, "--take") {
		t.Fatalf("remediation = %q, want it to name --take", remediation)
	}
	for _, fault := range []string{"Retry the command", "lit doctor"} {
		if strings.Contains(remediation, fault) {
			t.Fatalf("remediation = %q, want no fault advice (%q)", remediation, fault)
		}
	}

	// Same command, --take: proceeds, and the takeover is visible in the
	// output rather than silent.
	out = runCLIInDir(t, bravo, "start", ticket, "--assignee", "bravo-agent", "--take")
	if !strings.Contains(out, "taking over") {
		t.Fatalf("start %s --take = %q, want it to announce the takeover", ticket, out)
	}
	// The transfer line itself, composed, not sampled: this is the only test
	// where the two claimants differ in BOTH halves, so it is the only one that
	// can pin describeClaimant's both-halves render. Substring checks would pass
	// on a swapped order or a dropped stream.
	bothHalves := regexp.MustCompile(`claim transferred: alpha-agent \(stream ([a-z0-9]+)\) -> bravo-agent \(stream ([a-z0-9]+)\)`)
	streams := bothHalves.FindStringSubmatch(out)
	if streams == nil {
		t.Fatalf("start %s --take = %q, want the transfer line naming both assignees and both streams", ticket, out)
	}
	if streams[1] == streams[2] {
		t.Fatalf("transfer notice = %q, want two different streams: alpha and bravo are different checkouts", out)
	}

	// The lane is now bravo's: starting it again, still as bravo-agent, is
	// the happy path — no confirmation, no warning, no ceremony.
	out = runCLIInDir(t, bravo, "start", ticket, "--assignee", "bravo-agent")
	if strings.Contains(out, "claimed") || strings.Contains(out, "--take") {
		t.Fatalf("start %s on bravo's own lane = %q, want no takeover ceremony", ticket, out)
	}
}

// TestStartOnAnExpiredForeignClaimIsSilent: alpha's claim has expired by the
// time bravo looks, and bravo's start reads exactly as a start on a ticket
// nobody ever touched — no refusal, no provenance line, no advisory to check
// for unmerged work, no transfer notice. An expired claim is not a claim, so
// there is nothing for the gate to inform anybody of.
//
// The freshness window is configured down to force alpha's claim to expire by
// the time bravo looks, rather than waiting out the real default — the same
// technique config_test.go uses to make claims.freshness_window a controllable
// test input.
func TestStartOnAnExpiredForeignClaimIsSilent(t *testing.T) {
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
	// "--take", "take over" and "taking over" rather than a bare "take": the
	// fixture's topic is takeover-e2e, and the ticket id carries it.
	for _, residue := range []string{"claimed", "stale", "check for unmerged", "--take", "take over", "taking over", "claim transferred"} {
		if strings.Contains(out, residue) {
			t.Fatalf("start %s over an expired claim = %q, want no %q: an expired claim is not a claim, and the start reads as one on an untouched ticket", ticket, out, residue)
		}
	}
	if !strings.Contains(out, ticket) {
		t.Fatalf("start %s over an expired claim = %q, want the ordinary start summary naming the ticket", ticket, out)
	}
}

// waitPastFreshnessWindow sleeps long enough that any evidence timestamped
// before the call reads as older than a 1ms claims.freshness_window.
func waitPastFreshnessWindow(t *testing.T) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
}

// writeTinyFreshnessWindow points dir's workspace config at a
// claims.freshness_window short enough that any claim already on disk has
// expired the moment waitPastFreshnessWindow returns.
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

// TestStartTakesOverALiveLaneUnderOneSharedIdentity is the takeover that
// carries no assignee change at all.
//
// Both checkouts name the SAME assignee, which is not a contrived case but the
// ordinary one — a checkout driving no agent session resolves no assignee
// whatsoever, so every checkout one person runs looks identical on that axis,
// and two worktrees of one agent session share a session id. Ownership is keyed
// on the checkout, not the assignee, so this IS a transfer.
// [LAW:no-silent-failure]
func TestStartTakesOverALiveLaneUnderOneSharedIdentity(t *testing.T) {
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

	// Alpha's claim is live, so the takeover is the deliberate --take crossing;
	// the notice it prints is the subject here.
	out := runCLIInDir(t, bravo, "start", ticket, "--assignee", shared, "--take")
	if !strings.Contains(out, "claim transferred") {
		t.Fatalf("start %s --take over a live lane of the same identity = %q, want the transfer announced", ticket, out)
	}
	// The assignee is identical on both sides, so the stream labels are the only
	// thing in that line that can say anything moved.
	if !strings.Contains(out, "stream ") {
		t.Fatalf("transfer notice = %q, want the checkouts named: the assignee is the same on both sides and names nothing", out)
	}

	// The lane is now bravo's, and that is a fact about the RECORD rather than
	// about the line just printed: a second start by bravo is the happy path —
	// no gate, no notice — which is only true if the first start wrote the
	// establishing event that moved the lane here.
	out = runCLIInDir(t, bravo, "start", ticket, "--assignee", shared)
	if strings.Contains(out, "claim transferred") || strings.Contains(out, "--take") {
		t.Fatalf("start %s on bravo's own lane = %q, want no ceremony and no transfer notice: nothing moved", ticket, out)
	}
}

// TestStartOnAPresetAssigneeAnnouncesNoTransfer pins the distinction between an
// assignee and a holder. `lit new --assignee X` writes a row field and no
// establishing event, so nobody holds the lane — claims.Derive reads that state
// as Unclaimed and authorizeStart asks for no ceremony. The first start must
// agree and stay silent.
//
// Every other test here creates its ticket without --assignee, so the prior
// claimant is the exact zero value and the branch never fires.
func TestStartOnAPresetAssigneeAnnouncesNoTransfer(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	repo := filepath.Join(base, "solo")
	runGit(t, base, "clone", remote, "solo")
	runGit(t, repo, "config", "user.email", "a@a.co")
	runGit(t, repo, "config", "user.name", "solo")
	if err := os.WriteFile(filepath.Join(repo, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	runGit(t, repo, "push", "origin", "HEAD")
	runCLIInDir(t, repo, "init", "--skip-hooks", "--skip-agents")

	ticket := extractTicketID(t, runCLIInDir(t, repo, "new", "--title", "preset", "--topic", "takeover-e2e", "--type", "task", "--assignee", "alice"))

	out := runCLIInDir(t, repo, "start", ticket, "--assignee", "alice")
	if strings.Contains(out, "claim transferred") {
		t.Fatalf("first start of %s = %q, want no transfer notice: an assignee set at creation is a field, not a hold", ticket, out)
	}

	// The same command once the lane IS held announces the change, so the
	// silence above is the preset case specifically and not a dead notice.
	out = runCLIInDir(t, repo, "start", ticket, "--assignee", "bob")
	if !strings.Contains(out, "claim transferred") {
		t.Fatalf("start %s over a held lane = %q, want the transfer announced", ticket, out)
	}
}
