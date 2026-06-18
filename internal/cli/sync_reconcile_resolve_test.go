package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
)

func TestParseProseResolutions(t *testing.T) {
	// TEXT may itself contain ':' and '=' and newlines; only the first ':' (id) and
	// the first '=' (field) are separators.
	got, err := parseProseResolutions([]string{
		"links-x.1:description=line one\nline two: with = signs",
		"links-x.1:title=merged title",
		"links-x.1:agent_prompt=do the thing",
	})
	if err != nil {
		t.Fatalf("parseProseResolutions() error = %v", err)
	}
	want := []merge.ProseResolution{
		{IssueID: "links-x.1", Field: merge.ProseDescription, Text: "line one\nline two: with = signs"},
		{IssueID: "links-x.1", Field: merge.ProseTitle, Text: "merged title"},
		{IssueID: "links-x.1", Field: merge.ProsePrompt, Text: "do the thing"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d resolutions, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolution[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestParseProseResolutionsRejectsMalformed(t *testing.T) {
	for _, raw := range []string{"no-separators", "links-x.1=missing-colon-field", "links-x.1:nofield", ":description=text"} {
		if _, err := parseProseResolutions([]string{raw}); err == nil {
			t.Fatalf("parseProseResolutions(%q) accepted a malformed value", raw)
		}
	}
}

func TestParseProseResolutionsRejectsUnknownField(t *testing.T) {
	if _, err := parseProseResolutions([]string{"links-x.1:status=closed"}); err == nil {
		t.Fatalf("parseProseResolutions accepted a non-prose field")
	}
}

// runCLIInDirErr runs the CLI and returns its output and error instead of failing
// the test, so a command expected to exit non-zero (a prose-pending reconcile) can
// be asserted on.
func runCLIInDirErr(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q) error = %v", dir, err)
	}
	defer func() { _ = os.Chdir(prevWD) }()

	var out bytes.Buffer
	runErr := Run(context.Background(), &out, &out, args)
	return out.String(), runErr
}

// TestProseReconcileSurfacesAndResolves is the end-to-end proof of the agent
// surface: two clones rewrite the SAME ticket's description, the consumer's
// explicit `lit sync reconcile` surfaces the prose divergence as an ExitConflict
// with base/ours/theirs guidance, and `lit sync reconcile resolve` finalizes it
// with the agent's merged text into linear history that fast-forward pushes.
func TestProseReconcileSurfacesAndResolves(t *testing.T) {
	// This test drives sync explicitly; the inline auto-sync must not reconcile the
	// divergence out from under the assertions.
	t.Setenv(disableAutoSyncEnvVar, "1")

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
	ticketID := extractTicketID(t, runCLIInDir(t, producer, "new", "--title", "shared-ticket", "--description", "original-desc", "--topic", "demo", "--type", "task"))
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")

	// Both sides rewrite the SAME free-text field to different text -> a divergence
	// the engine settles to prose-pending, not a winner.
	runCLIInDir(t, producer, "update", ticketID, "--description", "alpha-desc")
	runCLIInDir(t, producer, "sync", "push")
	runCLIInDir(t, consumer, "update", ticketID, "--description", "bravo-desc")

	// The explicit reconcile surfaces the divergence and exits ExitConflict.
	surfaced, surfaceErr := runCLIInDirErr(t, consumer, "sync", "reconcile")
	if surfaceErr == nil {
		t.Fatalf("expected `sync reconcile` to surface a conflict, got success:\n%s", surfaced)
	}
	if code := ExitCode(surfaceErr); code != ExitConflict {
		t.Fatalf("reconcile exit code = %d, want %d (ExitConflict)\noutput:\n%s", code, ExitConflict, surfaced)
	}
	for _, want := range []string{ticketID, "description", "original-desc", "bravo-desc", "alpha-desc", "lit sync reconcile resolve"} {
		if !strings.Contains(surfaced, want) {
			t.Fatalf("guidance missing %q:\n%s", want, surfaced)
		}
	}

	// The agent supplies its merged text; the reconcile finalizes into linear history.
	resolved := runCLIInDir(t, consumer, "sync", "reconcile", "resolve", "--resolve", ticketID+":description=alpha-desc and bravo-desc merged")
	if !strings.Contains(strings.ToLower(resolved), "reconciled") {
		t.Fatalf("resolve did not report a reconciled state:\n%s", resolved)
	}

	// The consumer now carries the merged text and fast-forward pushes with no force.
	show := runCLIInDir(t, consumer, "show", ticketID)
	if !strings.Contains(show, "alpha-desc and bravo-desc merged") {
		t.Fatalf("consumer missing merged description after resolve:\n%s", show)
	}
	push := runCLIInDir(t, consumer, "sync", "push")
	if strings.Contains(strings.ToLower(push), "error") || strings.Contains(strings.ToLower(push), "rejected") {
		t.Fatalf("consumer fast-forward push after resolve failed:\n%s", push)
	}
}

// TestProseReconcileAbortLeavesCloneDiverged proves the escape: abort exits zero,
// leaves the clone diverged and usable, and a later reconcile still surfaces.
func TestProseReconcileAbortLeavesCloneDiverged(t *testing.T) {
	t.Setenv(disableAutoSyncEnvVar, "1")

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
	ticketID := extractTicketID(t, runCLIInDir(t, producer, "new", "--title", "shared-ticket", "--description", "original-desc", "--topic", "demo", "--type", "task"))
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")

	runCLIInDir(t, producer, "update", ticketID, "--description", "alpha-desc")
	runCLIInDir(t, producer, "sync", "push")
	runCLIInDir(t, consumer, "update", ticketID, "--description", "bravo-desc")

	// Abort exits zero (a clean escape), unlike the unresolved state's ExitConflict.
	abort := runCLIInDir(t, consumer, "sync", "reconcile", "abort")
	if !strings.Contains(strings.ToLower(abort), "diverged") {
		t.Fatalf("abort did not report the clone left diverged:\n%s", abort)
	}

	// Local truth still serves: the consumer's own edit is intact.
	if show := runCLIInDir(t, consumer, "show", ticketID); !strings.Contains(show, "bravo-desc") {
		t.Fatalf("consumer lost its local edit after abort:\n%s", show)
	}

	// The divergence is still there: a later reconcile re-surfaces it.
	_, surfaceErr := runCLIInDirErr(t, consumer, "sync", "reconcile")
	if code := ExitCode(surfaceErr); code != ExitConflict {
		t.Fatalf("post-abort reconcile exit code = %d, want %d (ExitConflict)", code, ExitConflict)
	}
}
