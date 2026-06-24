package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Bulk operations once collected each item's outcome into a map, printed every
// "id <status>" row to stdout (failures interleaved with results), then returned
// nil unconditionally — so the process exited 0 even when items failed and the
// failure rode the data channel. These tests pin the corrected contract through
// behavior: any item failing yields a non-OK exit code, the failed ID's error
// never reaches stdout, and successful items still persist and report.
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
	err := runBulkTransition(model.ActionClose)(ctx, &out, ap, []string{"--ids", good + "," + bogus, "--reason", "done"})
	if err == nil {
		t.Fatal("bulk close with one bogus ID returned nil error, want a failure that drives a non-zero exit code")
	}
	if got := ExitCode(err); got == ExitOK {
		t.Fatalf("ExitCode(err) = %d (ExitOK), want non-zero so chained scripts halt", got)
	}
	var bulkFailure BulkFailureError
	if !errors.As(err, &bulkFailure) {
		t.Fatalf("error type = %T, want BulkFailureError carrying per-item failures", err)
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
