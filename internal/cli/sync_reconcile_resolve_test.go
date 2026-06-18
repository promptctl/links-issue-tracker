package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
)

func TestParseProseResolutions(t *testing.T) {
	// TEXT may itself contain ':' and '=' and newlines; the prefix before the first
	// '=' splits on ':' into exactly id/field/fingerprint.
	got, err := parseProseResolutions([]string{
		"links-x.1:description:abc123=line one\nline two: with = signs",
		"links-x.1:title:def456=merged title",
		"links-x.1:agent_prompt:99=do the thing",
	})
	if err != nil {
		t.Fatalf("parseProseResolutions() error = %v", err)
	}
	want := []merge.ProseResolution{
		{IssueID: "links-x.1", Field: merge.ProseDescription, Fingerprint: "abc123", Text: "line one\nline two: with = signs"},
		{IssueID: "links-x.1", Field: merge.ProseTitle, Fingerprint: "def456", Text: "merged title"},
		{IssueID: "links-x.1", Field: merge.ProsePrompt, Fingerprint: "99", Text: "do the thing"},
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
	// no '='; only two prefix parts (missing fingerprint); empty fingerprint; empty id.
	for _, raw := range []string{"no-separators", "links-x.1:title=text", "links-x.1:title:=text", ":title:fp=text"} {
		if _, err := parseProseResolutions([]string{raw}); err == nil {
			t.Fatalf("parseProseResolutions(%q) accepted a malformed value", raw)
		}
	}
}

func TestParseProseResolutionsRejectsUnknownField(t *testing.T) {
	if _, err := parseProseResolutions([]string{"links-x.1:status:fp=closed"}); err == nil {
		t.Fatalf("parseProseResolutions accepted a non-prose field")
	}
}

func TestProseGuidanceNamesRealAbortCommand(t *testing.T) {
	var b bytes.Buffer
	if err := renderProsePendingGuidance(&b, []merge.ProsePending{{IssueID: "links-x.1", Field: merge.ProseTitle, Base: "b", Ours: "o", Theirs: "t"}}); err != nil {
		t.Fatalf("renderProsePendingGuidance() error = %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "lit sync reconcile abort") {
		t.Fatalf("guidance missing abort command:\n%s", out)
	}
	// The abort command is a subcommand, not a flag: the guidance must not print the
	// non-existent `abort --abort` form.
	if strings.Contains(out, "--abort") {
		t.Fatalf("guidance prints a non-existent --abort flag:\n%s", out)
	}
}

func parsedReconcileFlags(t *testing.T, args ...string) *cobraFlagSet {
	t.Helper()
	fs := newCobraFlagSet("sync reconcile resolve")
	fs.StringArray("resolve", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse(%v) error = %v", args, err)
	}
	return fs
}

func TestGuardReconcileInputRejectsStrayPositional(t *testing.T) {
	// A stray positional must fail loudly rather than be silently ignored, or a
	// malformed finalize could appear to succeed.
	var text bytes.Buffer
	err := guardReconcileInput(parsedReconcileFlags(t, "junk", "--resolve", "links-x.1:title=merged"), &text, "sync reconcile resolve")
	if code := ExitCode(err); code != ExitUsage {
		t.Fatalf("stray positional exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}

	if err := guardReconcileInput(parsedReconcileFlags(t, "--resolve", "links-x.1:title=merged"), &text, "sync reconcile resolve"); err != nil {
		t.Fatalf("guardReconcileInput() on a clean text command = %v, want nil", err)
	}
}

func TestGuardReconcileInputRejectsJSONMode(t *testing.T) {
	// The reconcile surface is agent-facing text guidance with no JSON form, so a
	// JSON output mode must be rejected rather than silently emit plain text under
	// the strict one-document JSON contract.
	var buf bytes.Buffer
	jsonStdout := newOutputModeWriter(&buf, outputModeJSON)
	err := guardReconcileInput(parsedReconcileFlags(t, "--resolve", "links-x.1:title=merged"), jsonStdout, "sync reconcile resolve")
	if code := ExitCode(err); code != ExitValidation {
		t.Fatalf("JSON-mode exit code = %d, want %d (ExitValidation for an unsupported flag)", code, ExitValidation)
	}
}

// extractResolveFingerprint pulls the conflict fingerprint the guidance printed
// for one issue+field out of its `--resolve ID:FIELD:FP=...` template, mirroring
// what the calling agent copies.
func extractResolveFingerprint(t *testing.T, guidance, issueID, field string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(issueID+":"+field+":") + `([^=]+)=`)
	m := re.FindStringSubmatch(guidance)
	if m == nil {
		t.Fatalf("no resolve template for %s:%s in guidance:\n%s", issueID, field, guidance)
	}
	return m[1]
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

	// The agent copies the conflict fingerprint the guidance prints, merges the
	// text, and finalizes into linear history.
	fp := extractResolveFingerprint(t, surfaced, ticketID, "description")
	resolved := runCLIInDir(t, consumer, "sync", "reconcile", "resolve", "--resolve", ticketID+":description:"+fp+"=alpha-desc and bravo-desc merged")
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
