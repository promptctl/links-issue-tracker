package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNextRoutesEachCheckoutToItsOwnClaimedWork is the epic's motivating
// scenario end-to-end (design-docs/work-claims.md's Summary, and the
// ticket's own acceptance line): two worktrees over one store, working
// different epics — a fresh, unbriefed session in either runs bare
// `lit next` and pulls its own checkout's work, never the other's claimed
// lane; a third checkout with no claims of its own routes around BOTH.
//
// Driven through the real CLI over a real git remote, not an in-process
// fixture — the claim being tested is about what a checkout derives from
// evidence that actually made the trip through sync, exactly as
// claims_attribution_test.go's rationale states.
func TestNextRoutesEachCheckoutToItsOwnClaimedWork(t *testing.T) {
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

	// Creation order sets rank: epicB and epicC both outrank epicA, so a
	// claims-blind selector would never reach epicA's remaining work while
	// either of the others has anything unclaimed left to offer.
	epicB := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "Epic B", "--topic", "claims-e2e", "--type", "epic"))
	b1 := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "B.1", "--topic", "claims-e2e", "--type", "task", "--parent", epicB))
	epicC := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "Epic C", "--topic", "claims-e2e", "--type", "epic"))
	c1 := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "C.1", "--topic", "claims-e2e", "--type", "task", "--parent", epicC))
	epicA := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "Epic A", "--topic", "claims-e2e", "--type", "epic"))
	a1 := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "A.1", "--topic", "claims-e2e", "--type", "task", "--parent", epicA))
	a2 := extractTicketID(t, runCLIInDir(t, alpha, "new", "--title", "A.2", "--topic", "claims-e2e", "--type", "task", "--parent", epicA))

	// alpha claims epic A's lane by completing its first ticket: A.2 is now
	// ready (the lane gate no longer holds it behind a finished sibling) and
	// the lane stays Held — A.2 itself is still open — attributed to alpha.
	runCLIInDir(t, alpha, "start", a1)
	runCLIInDir(t, alpha, "done", a1)
	runCLIInDir(t, alpha, "sync", "push", "--set-upstream")

	// bravo clones after alpha's claim is on the remote, adopting epicA/B/C
	// and A.1's history, then claims epic B's lane the same way.
	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	runCLIInDir(t, bravo, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, bravo, "start", b1)
	runCLIInDir(t, bravo, "sync", "push")

	// alpha pulls bravo's claim before asking `next` again, so the fixture
	// proves alpha's own-lane precedence even with bravo's claim visible —
	// not merely "alpha never learned about B.1."
	t.Setenv(DisableAutoSyncEnvVar, "0")
	alphaNext := runCLIInDir(t, alpha, "next")
	if !strings.Contains(alphaNext, a2) {
		t.Fatalf("alpha's `next` = %q, want %q (its own claimed epic, ranked lowest but held)", alphaNext, a2)
	}
	if strings.Contains(alphaNext, b1) || strings.Contains(alphaNext, c1) {
		t.Fatalf("alpha's `next` = %q, want neither bravo's %q nor unclaimed %q ahead of alpha's own claim", alphaNext, b1, c1)
	}

	// charlie clones last, with no claims of its own anywhere: routing must
	// skip both alpha's held A.2 and bravo's in-progress B.1 and land on the
	// only fully unclaimed ready ticket, C.1 — the acceptance scenario's
	// other half, proven against real synced evidence rather than a stub.
	charlie := filepath.Join(base, "charlie")
	runGit(t, base, "clone", remote, "charlie")
	runGit(t, charlie, "config", "user.email", "c@c.co")
	runGit(t, charlie, "config", "user.name", "charlie")
	runCLIInDir(t, charlie, "init", "--skip-hooks", "--skip-agents")

	charlieNext := runCLIInDir(t, charlie, "next")
	if !strings.Contains(charlieNext, c1) {
		t.Fatalf("charlie's `next` = %q, want %q (the only lane nobody holds)", charlieNext, c1)
	}
	if strings.Contains(charlieNext, a2) || strings.Contains(charlieNext, b1) {
		t.Fatalf("charlie's `next` = %q, want neither %q (held by alpha) nor %q (held by bravo)", charlieNext, a2, b1)
	}
}
