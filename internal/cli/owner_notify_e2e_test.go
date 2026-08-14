package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These are the end-to-end proofs of links-sync-pgct.4's acceptance criteria:
// inducing a divergence fires the configured owner notification exactly once
// per episode; a scripted take-side reconcile without the owner-confirmation
// step exits refusing with nothing destroyed; and a failing push reaches the
// owner the day it happens.

// writeOwnerNotifyProjectConfig points the repo's project config at a
// file-appending hook and pins auto-sync fully off via CONFIG (cadence on-push,
// receive off), so a test can re-enable LIT_DISABLE_AUTO_SYNC=0 — which the
// notify path requires — without the mirror or the inline receive reconciling
// state out from under its assertions.
func writeOwnerNotifyProjectConfig(t *testing.T, repo, sink string) {
	t.Helper()
	litDir := filepath.Join(repo, ".lit")
	if err := os.MkdirAll(litDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", litDir, err)
	}
	payload := "[sync]\ncadence = 'on-push'\nreceive = false\n" +
		"owner_notify_cmd = 'printf \"%s %s/%s\\n\" \"$LIT_NOTIFY_KIND\" \"$LIT_NOTIFY_REMOTE\" \"$LIT_NOTIFY_BRANCH\" >> " + sink + "'\n"
	if err := os.WriteFile(filepath.Join(litDir, "config.toml"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

// maskGlobalConfig keeps the developer's real ~/.config lit settings — possibly
// including a real notifier — out of any test that re-enables auto-sync.
func maskGlobalConfig(t *testing.T) {
	t.Helper()
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", filepath.Join(t.TempDir(), "global-config.toml"))
}

// forkedBacklogs builds the field incident's shape: producer and consumer
// initialize their own lit stores against one shared remote, the producer
// publishes after the consumer already bootstrapped its own root, and the two
// histories share no common ancestor. Returns both repo roots and both issue
// ids. Auto-sync must be disabled (the suite default) while this runs, so the
// setup itself cannot publish or adopt early.
func forkedBacklogs(t *testing.T, base string) (producer, consumer, producerID, consumerID string) {
	t.Helper()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	producer = filepath.Join(base, "alpha")
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
	producerID = firstIssueID(t, runCLIInDir(t, producer, "new", "--title", "producer-ticket", "--topic", "demo", "--type", "task"))

	consumer = filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	consumerID = firstIssueID(t, runCLIInDir(t, consumer, "new", "--title", "consumer-ticket", "--topic", "demo", "--type", "task"))

	runCLIInDir(t, producer, "sync", "push", "--set-upstream")
	return producer, consumer, producerID, consumerID
}

// TestDivergenceNotifiesOwnerOutOfBand is the acceptance proof for the
// notification half: detecting the no-common-ancestor divergence runs the
// configured hook with the event's facts, exactly once for the episode however
// many commands re-detect it.
func TestDivergenceNotifiesOwnerOutOfBand(t *testing.T) {
	t.Setenv(DisableAutoSyncEnvVar, "1")
	maskGlobalConfig(t)
	base := t.TempDir()
	_, consumer, _, _ := forkedBacklogs(t, base)

	sink := filepath.Join(base, "notifications")
	writeOwnerNotifyProjectConfig(t, consumer, sink)
	if _, err := os.Stat(sink); !os.IsNotExist(err) {
		t.Fatalf("notification sink exists before any divergence surfaced (stat err=%v)", err)
	}

	// The channel opens; the divergence surfaces; the owner hears once.
	t.Setenv(DisableAutoSyncEnvVar, "0")
	if _, err := runCLIInDirErr(t, consumer, "sync", "reconcile"); err == nil {
		t.Fatalf("expected `sync reconcile` to surface unrelated histories")
	}
	payload, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("divergence did not fire the configured notification: %v", err)
	}
	if got, want := string(payload), "unrelated_histories origin/master\n"; got != want {
		t.Fatalf("notification payload = %q, want %q", got, want)
	}

	// Re-detecting the same episode is deduplicated: the owner is not spammed
	// on every command while the fork stands.
	if _, err := runCLIInDirErr(t, consumer, "sync", "reconcile"); err == nil {
		t.Fatalf("expected the re-run reconcile to still surface unrelated histories")
	}
	payload, err = os.ReadFile(sink)
	if err != nil {
		t.Fatalf("read sink after re-detection: %v", err)
	}
	if got := strings.Count(string(payload), "unrelated_histories"); got != 1 {
		t.Fatalf("the persisting episode notified %d times, want 1:\n%s", got, payload)
	}
}

// TestTakeRefusesWithoutOwnerApprovalEndToEnd is the acceptance proof for the
// confirmation half: a scripted take-side reconcile without the confirmation
// step exits refusing with both backlogs intact; a wrong token is refused the
// same way; the token from the refusal — the owner's approval — runs the take.
func TestTakeRefusesWithoutOwnerApprovalEndToEnd(t *testing.T) {
	t.Setenv(DisableAutoSyncEnvVar, "1")
	maskGlobalConfig(t)
	base := t.TempDir()
	_, consumer, _, _ := forkedBacklogs(t, base)

	// Scripted, no confirmation: refused, conflict exit, nothing destroyed.
	out, err := runCLIInDirErr(t, consumer, "sync", "reconcile", "take", "local")
	if err == nil {
		t.Fatalf("scripted take without owner approval succeeded:\n%s", out)
	}
	if code := ExitCode(err); code != ExitConflict {
		t.Fatalf("bare take exit code = %d, want %d (ExitConflict)\nerr: %v", code, ExitConflict, err)
	}
	block := err.Error()
	for _, want := range []string{"DESTRUCTIVE", "owner", "lit sync reconcile combine", "--owner-approved"} {
		if !strings.Contains(block, want) {
			t.Fatalf("take refusal missing %q:\n%s", want, block)
		}
	}
	assertBacklogHolds(t, consumer, "consumer-ticket", true)
	assertBacklogHolds(t, consumer, "producer-ticket", false)

	// A guessed/stale token is refused identically — still nothing destroyed.
	if _, err := runCLIInDirErr(t, consumer, "sync", "reconcile", "take", "local", "--owner-approved", "ffffffffffff"); err == nil {
		t.Fatalf("take with a wrong token succeeded")
	} else if !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("wrong-token refusal does not explain the mismatch:\n%s", err.Error())
	}
	assertBacklogHolds(t, consumer, "consumer-ticket", true)

	// The refusal's token is the approval; with it, the take runs and reports
	// the discard.
	tokenMatch := regexp.MustCompile(`--owner-approved ([0-9a-f]{12})`).FindStringSubmatch(block)
	if tokenMatch == nil {
		t.Fatalf("refusal block carries no approval token:\n%s", block)
	}
	approved := runCLIInDir(t, consumer, "sync", "reconcile", "take", "local", "--owner-approved", tokenMatch[1])
	if !strings.Contains(approved, "took local") {
		t.Fatalf("approved take did not report taking local:\n%s", approved)
	}
	assertBacklogHolds(t, consumer, "consumer-ticket", true)
	assertBacklogHolds(t, consumer, "producer-ticket", false)
}

// TestPushFailureNotifiesOwner is the push half of the notification acceptance:
// a push that stops landing notifies the owner once per episode, a landed push
// ends the episode, and the next failure is a new episode that fires
// immediately — the field incident's "stopped pushing on Aug 2, nobody heard
// for a week" can no longer happen silently.
func TestPushFailureNotifiesOwner(t *testing.T) {
	t.Setenv(DisableAutoSyncEnvVar, "1")
	maskGlobalConfig(t)
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remoteGit := filepath.Join(base, "remote.git")

	repo := filepath.Join(base, "solo")
	runGit(t, base, "clone", remoteGit, "solo")
	runGit(t, repo, "config", "user.email", "s@s.co")
	runGit(t, repo, "config", "user.name", "solo")
	if err := os.WriteFile(filepath.Join(repo, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	runGit(t, repo, "push", "origin", "HEAD")
	runCLIInDir(t, repo, "init", "--skip-hooks", "--skip-agents")
	firstIssueID(t, runCLIInDir(t, repo, "new", "--title", "solo-ticket", "--topic", "demo", "--type", "task"))

	sink := filepath.Join(base, "notifications")
	writeOwnerNotifyProjectConfig(t, repo, sink)
	t.Setenv(DisableAutoSyncEnvVar, "0")

	// Healthy push: no notification.
	runCLIInDir(t, repo, "sync", "push", "--set-upstream")
	if _, err := os.Stat(sink); !os.IsNotExist(err) {
		t.Fatalf("a healthy push notified the owner (stat err=%v)", err)
	}

	// The remote dies; the failing push notifies once, however often it fails.
	runGit(t, repo, "remote", "set-url", "origin", filepath.Join(base, "gone", "nowhere.git"))
	for i := 0; i < 2; i++ {
		if _, err := runCLIInDirErr(t, repo, "sync", "push"); err == nil {
			t.Fatalf("push against a dead remote succeeded")
		}
	}
	payload, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("failing push never notified the owner: %v", err)
	}
	if got := strings.Count(string(payload), "push_failed"); got != 1 {
		t.Fatalf("failing-push episode notified %d times, want 1:\n%s", got, payload)
	}

	// The remote returns; a landed push ends the episode; the next failure is a
	// NEW episode and fires immediately, not a cooldown later.
	runGit(t, repo, "remote", "set-url", "origin", remoteGit)
	runCLIInDir(t, repo, "sync", "push")
	runGit(t, repo, "remote", "set-url", "origin", filepath.Join(base, "gone", "nowhere.git"))
	if _, err := runCLIInDirErr(t, repo, "sync", "push"); err == nil {
		t.Fatalf("push against the re-dead remote succeeded")
	}
	payload, err = os.ReadFile(sink)
	if err != nil {
		t.Fatalf("read sink after second episode: %v", err)
	}
	if got := strings.Count(string(payload), "push_failed"); got != 2 {
		t.Fatalf("the second failing-push episode fired %d total notifications, want 2:\n%s", got, payload)
	}
}

// assertBacklogHolds fails unless the repo's backlog does (want=true) or does
// not (want=false) mention the given ticket title.
func assertBacklogHolds(t *testing.T, repo, title string, want bool) {
	t.Helper()
	backlog := runCLIInDir(t, repo, "backlog")
	if got := strings.Contains(backlog, title); got != want {
		t.Fatalf("backlog contains %q = %v, want %v:\n%s", title, got, want, backlog)
	}
}
