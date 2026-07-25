package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// captureStderr swaps os.Stderr for a pipe across fn and returns what was written.
// The inline auto-sync writes its surface to os.Stderr directly (its stdout is
// already produced), so an end-to-end assertion on that surface must capture the
// process stderr rather than the command's writer. The inline receive runs
// synchronously inside the command, so a straight-line capture suffices.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stderr = w
	// Restore via defer so a t.Fatal inside fn (runtime.Goexit) cannot leave
	// os.Stderr pointing at a closed pipe for every later test in the process.
	// runtime.Goexit runs deferred funcs, so the restore always happens.
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close stderr pipe error = %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr pipe error = %v", err)
	}
	return string(data)
}

// assertSyncFailureBlock pins that a captured surface carries every element of the
// sync-failure contract — the same four-part check the unit tests use, applied to
// a real end-to-end surface so the wiring cannot regress while the renderer stays
// green.
func assertSyncFailureBlock(t *testing.T, surface string, wantCommands ...string) {
	t.Helper()
	for _, want := range append([]string{
		"<agent-instructions>", "MUST NOT IGNORE", "surface it to the user as blocking",
		"WHAT HAPPENED:", "HOW TO RESOLVE", "ESCALATION",
	}, wantCommands...) {
		if !strings.Contains(surface, want) {
			t.Fatalf("sync-failure surface missing %q:\n%s", want, surface)
		}
	}
	if strings.Contains(surface, "will retry") {
		t.Fatalf("sync-failure surface reintroduced the ignorable 'will retry' framing:\n%s", surface)
	}
}

// proseDivergedClones stands up a producer and a consumer that have rewritten the
// SAME ticket's description to different text — the divergence the field-aware
// engine settles to prose-pending (a held free-text conflict), which is the
// genuinely-unresolvable case the sync-failure contract must surface. It returns
// the consumer dir and the ticket id, with auto-sync left disabled so the caller
// controls exactly when the divergence surfaces.
func proseDivergedClones(t *testing.T) (consumer, ticketID string) {
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
	runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")
	ticketID = extractTicketID(t, runCLIInDir(t, producer, "new", "--title", "shared-ticket", "--description", "original-desc", "--topic", "demo", "--type", "task"))
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer = filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")

	// Both sides rewrite the SAME free-text field to different text.
	runCLIInDir(t, producer, "update", ticketID, "--description", "alpha-desc")
	runCLIInDir(t, producer, "sync", "push")
	runCLIInDir(t, consumer, "update", ticketID, "--description", "bravo-desc")
	return consumer, ticketID
}

// TestInlineReconcileSurfacesContractOnProseHeld is the incident replay: a clone
// diverges on a free-text field the engine cannot settle, and on the VERY FIRST
// ordinary command afterward the inline auto-reconcile surfaces the full
// sync-failure contract to stderr — directive, what, how, escalation — where the
// 2026-07-08 incident printed only a raw "will retry" line an agent ignored for
// two days. [LAW:no-silent-failure]
func TestInlineReconcileSurfacesContractOnProseHeld(t *testing.T) {
	consumer, ticketID := proseDivergedClones(t)

	// Auto-sync ON: the first ordinary command triggers the inline receive, which
	// reconciles, finds the held free-text conflict, and surfaces the contract.
	t.Setenv(disableAutoSyncEnvVar, "0")
	surface := captureStderr(t, func() {
		runCLIInDir(t, consumer, "ready")
	})

	assertSyncFailureBlock(t, surface, "lit sync reconcile")
	if !strings.Contains(surface, ticketID) || !strings.Contains(surface, "description") {
		t.Fatalf("inline contract did not name the held ticket/field:\n%s", surface)
	}
}

// TestExplicitPullSurfacesContractOnProseHeld proves the exit-contract change: an
// explicit `lit sync pull` that meets a held free-text conflict now returns the
// sync-failure contract and exits ExitConflict — the same exit `lit sync
// reconcile` gives for the identical state — instead of the pre-change exit-0
// stdout one-liner. [LAW:single-enforcer]
func TestExplicitPullSurfacesContractOnProseHeld(t *testing.T) {
	t.Setenv(disableAutoSyncEnvVar, "1") // drive sync explicitly; no inline race
	consumer, ticketID := proseDivergedClones(t)

	out, err := runCLIInDirErr(t, consumer, "sync", "pull")
	if err == nil {
		t.Fatalf("expected `sync pull` to surface a held conflict, got success:\n%s", out)
	}
	if code := ExitCode(err); code != ExitConflict {
		t.Fatalf("held-pull exit code = %d, want %d (ExitConflict)\noutput:\n%s\nerr:\n%v", code, ExitConflict, out, err)
	}
	// The block is the error's own message (self-rendering), so it reaches stderr
	// through the top-level sink.
	assertSyncFailureBlock(t, err.Error(), "lit sync reconcile")
	if !strings.Contains(err.Error(), ticketID) {
		t.Fatalf("held-pull contract did not name the ticket:\n%s", err.Error())
	}
}

// TestNoRawReconcileShrugInSource is the grep-level property from the acceptance:
// the exact ignorable line the 2026-07-08 incident printed — a raw backend error
// framed as "will retry" — must never return to the source. The inline reconcile
// failure now routes through the sync-failure contract (blockString); a future raw
// reprint of this literal is the specific regression this guards. It is a
// structural guard on purpose: no behavioral test can assert the ABSENCE of a
// future bad print. [LAW:no-silent-failure]
func TestNoRawReconcileShrugInSource(t *testing.T) {
	const shrug = "automatic reconcile of the diverged clone failed"
	for _, dir := range []string{".", "../store"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if strings.Contains(string(data), shrug) {
				t.Errorf("%s/%s reintroduced the ignorable incident shrug %q; route sync failures through the SyncFailure contract instead", dir, name, shrug)
			}
		}
	}
}

// TestInlineSyncFailureMapping pins the deterministic mapping from an inline
// reconcile outcome to the sync-failure contract for both non-converging classes,
// including the hard-backend-error case an end-to-end test cannot easily induce:
// the reconcile error becomes the divergedUnresolved class with the backend error
// demoted to the block's trailing cause, and a held prose conflict becomes the
// proseHeld class. A clean reconcile maps to no failure. [LAW:dataflow-not-control-flow]
func TestInlineSyncFailureMapping(t *testing.T) {
	now := time.Now()
	base := syncReceiveOutcome{
		remote: "origin", branch: "master", ahead: 41, behind: 5,
		oldestDivergedUnix: now.Add(-5 * 24 * time.Hour).Unix(),
	}

	// Hard backend failure -> divergedUnresolved, cause demoted into the block.
	backend := errors.New(`table "i" does not have column "resolution"`)
	hard := base
	hard.reconcile = &reconcileOutcome{err: backend}
	failure, ok := hard.inlineSyncFailure(now)
	if !ok {
		t.Fatal("hard reconcile error did not produce a sync failure")
	}
	if failure.Class != syncFailureDivergedUnresolved || failure.Cause == nil {
		t.Fatalf("hard failure = %+v, want divergedUnresolved with a cause", failure)
	}
	if block := failure.blockString(); !strings.Contains(block, backend.Error()) || !strings.Contains(block, "INCIDENT") {
		t.Fatalf("hard-failure block missing demoted cause or incident escalation:\n%s", block)
	}

	// Held prose conflict -> proseHeld with the pending fields.
	held := base
	held.reconcile = &reconcileOutcome{
		state:   store.SyncReconcileProsePending,
		pending: []merge.ProsePending{{IssueID: "links-x.1", Field: merge.ProseTitle}},
	}
	heldFailure, ok := held.inlineSyncFailure(now)
	if !ok || heldFailure.Class != syncFailureProseHeld {
		t.Fatalf("held outcome = %+v (ok=%v), want proseHeld", heldFailure, ok)
	}

	// A clean (linearized) reconcile surfaces nothing.
	clean := base
	clean.reconcile = &reconcileOutcome{state: store.SyncReconcileLinearized}
	if _, ok := clean.inlineSyncFailure(now); ok {
		t.Fatal("a linearized reconcile wrongly produced a sync failure")
	}

	// No reconcile ran at all -> nothing.
	if _, ok := base.inlineSyncFailure(now); ok {
		t.Fatal("an outcome with no reconcile wrongly produced a sync failure")
	}
}
