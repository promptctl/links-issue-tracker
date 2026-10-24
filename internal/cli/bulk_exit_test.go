package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// These tests pin the contract through behavior: any item failing yields a
// non-OK exit code, the failed ID's error never reaches stdout, and successful
// items still persist and report.
// [LAW:behavior-not-structure] [LAW:no-silent-failure]

// TestBulkTransitionPartialFailureExitsNonZero pins that a mixed valid/bogus run
// fails at the process level, keeps the failed ID off stdout, and still applies
// the valid transition.
func TestBulkTransitionPartialFailureExitsNonZero(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "bulk-exit-sess")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	good := newAttributionIssue(t, ap, "real target")
	const bogus = "does-not-exist-xyz"

	var out bytes.Buffer
	err := runBulkClose(ctx, &out, ap, []string{"--ids", good + "," + bogus, "--resolution", "obsolete", "--reason", "done"})
	if err == nil {
		t.Fatal("bulk close with one bogus ID returned nil error, want a failure that drives a non-zero exit code")
	}
	if got := ExitCode(err); got == ExitOK {
		t.Fatalf("ExitCode(err) = %d (ExitOK), want non-zero so chained scripts halt", got)
	}
	var bulkFailure BulkError
	if !errors.As(err, &bulkFailure) {
		t.Fatalf("error type = %T, want BulkError carrying per-item failures", err)
	}

	stdout := out.String()
	if strings.Contains(stdout, bogus) {
		t.Fatalf("stdout contains the failed ID %q — per-item failures must not ride the data channel; stdout = %q", bogus, stdout)
	}
	if !strings.Contains(stdout, good+" ok") {
		t.Fatalf("stdout missing success line for %q; stdout = %q", good, stdout)
	}

	// The valid item's transition must have persisted despite the sibling failure.
	assertCloseEventActor(t, ap, good, "claude_bulk-exit-sess")
}

// TestBulkLabelAllSucceedExitsZero pins the converse: when every item succeeds,
// the command returns nil (exit 0) and reports each success on stdout.
func TestBulkLabelAllSucceedExitsZero(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	a := newAttributionIssue(t, ap, "label a")
	b := newAttributionIssue(t, ap, "label b")

	var out bytes.Buffer
	if err := runBulkLabel(ctx, &out, ap, []string{"add", "--ids", a + "," + b, "--label", "urgent"}); err != nil {
		t.Fatalf("runBulkLabel(all valid) error = %v, want nil (exit 0)", err)
	}
	stdout := out.String()
	for _, id := range []string{a, b} {
		if !strings.Contains(stdout, id+" ok") {
			t.Fatalf("stdout missing success line for %q; stdout = %q", id, stdout)
		}
	}
}

// TestBulkLabelTotalFailureExitsNonZero pins that an all-bogus run also fails
// loudly rather than reporting success on an empty result set.
func TestBulkLabelTotalFailureExitsNonZero(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var out bytes.Buffer
	err := runBulkLabel(ctx, &out, ap, []string{"add", "--ids", "nope-1,nope-2", "--label", "urgent"})
	if err == nil {
		t.Fatal("bulk label add over only bogus IDs returned nil, want a non-zero exit")
	}
	if got := ExitCode(err); got == ExitOK {
		t.Fatalf("ExitCode(err) = %d (ExitOK), want non-zero", got)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("stdout non-empty on total failure = %q, want no result rows", out.String())
	}
}

// commandAnswer is how the real CLI answers args in repo: what main prints to
// stderr for its error ("error (code=N, reason=R): …" and on), its
// "remediation: …" line, and stdout alone. A run that succeeds, or a reason
// with no remediation, answers with empty strings there.
func commandAnswer(t *testing.T, repo string, args ...string) (stderr, remediation, stdout string) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), dirInvocation(repo, &out, io.Discard, args))
	if err == nil {
		return "", "", out.String()
	}
	var written bytes.Buffer
	WriteCommandError(&written, err)
	for _, line := range strings.Split(written.String(), "\n") {
		if strings.HasPrefix(line, "remediation: ") {
			remediation = line
		}
	}
	return written.String(), remediation, out.String()
}

// codeAndReason reads "code=6, reason=state_already_holds" off stderr that
// opens "error (code=6, reason=state_already_holds): …".
func codeAndReason(t *testing.T, stderr string) string {
	t.Helper()
	start, end := strings.Index(stderr, "("), strings.Index(stderr, ")")
	if !strings.HasPrefix(stderr, "error (") || end < start {
		t.Fatalf("stderr = %q, want error (code=…, reason=…)", stderr)
	}
	return stderr[start+1 : end]
}

// The ticket's acceptance, through the real CLI: `lit bulk archive` over an
// already-archived ticket answers as `lit archive` does (exit 6,
// state_already_holds), over a deleted one as `lit archive` refuses it (exit 3,
// validation_refused). Beside a ticket that lands, and over both stuck tickets
// at once, it answers as a partial failure. Every run names each stuck ticket
// with its reason and advises re-running none of them.
func TestBulkArchiveAnswersEachTicketItCannotMoveAsLitArchiveDoes(t *testing.T) {
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "bulk answers")
	task := func(t *testing.T, title string) string {
		t.Helper()
		return extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--title", title, "--topic", "demo", "--parent", epic))
	}
	archived, deleted := task(t, "archived"), task(t, "deleted")
	runCLIInDir(t, repo, "archive", archived)
	runCLIInDir(t, repo, "delete", deleted)
	const partial = "code=1, reason=bulk_partial_failure"

	for _, tc := range []struct {
		name   string
		stuck  string
		beside bool // whether a ticket that lands rides in the same run
		code   int
		reason string
	}{
		{"already archived", archived, false, 6, "state_already_holds"},
		{"already archived, beside one that lands", archived, true, 6, "state_already_holds"},
		{"deleted", deleted, false, 3, "validation_refused"},
		{"deleted, beside one that lands", deleted, true, 3, "validation_refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := fmt.Sprintf("code=%d, reason=%s", tc.code, tc.reason)
			single, _, _ := commandAnswer(t, repo, "archive", tc.stuck)
			if got := codeAndReason(t, single); got != want {
				t.Fatalf("lit archive %s answers %q, want %q", tc.stuck, got, want)
			}
			ids, wantStdout := tc.stuck, ""
			if tc.beside {
				live := task(t, "lands")
				ids, wantStdout, want = tc.stuck+","+live, live+" ok\n", partial
			}
			stderr, remediation, stdout := commandAnswer(t, repo, "bulk", "archive", "--ids", ids)
			if got := codeAndReason(t, stderr); got != want {
				t.Errorf("lit bulk archive --ids %s answers %q, want %q", ids, got, want)
			}
			if named := tc.stuck + " (" + tc.reason + ")"; !strings.Contains(stderr, named) {
				t.Errorf("stderr = %q, want it to name %q", stderr, named)
			}
			if strings.Contains(remediation, "Re-run the command for only the failed IDs") {
				t.Errorf("remediation = %q, advises re-running the ticket", remediation)
			}
			if tc.beside && !strings.HasPrefix(remediation, "remediation: Do not re-run the command unchanged") {
				t.Errorf("remediation = %q, want it to advise against a re-run of what landed", remediation)
			}
			if stdout != wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, wantStdout)
			}
		})
	}

	t.Run("already archived and deleted", func(t *testing.T) {
		stderr, remediation, stdout := commandAnswer(t, repo, "bulk", "archive", "--ids", archived+","+deleted)
		if got := codeAndReason(t, stderr); got != partial {
			t.Errorf("answer = %q, want %q", got, partial)
		}
		for _, named := range []string{archived + " (state_already_holds)", deleted + " (validation_refused)"} {
			if !strings.Contains(stderr, named) {
				t.Errorf("stderr = %q, want it to name %q", stderr, named)
			}
		}
		if !strings.HasPrefix(remediation, "remediation: Do not re-run the command unchanged") {
			t.Errorf("remediation = %q, want it to advise against a re-run", remediation)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want no ok line: nothing landed", stdout)
		}
	})
}
