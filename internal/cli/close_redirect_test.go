package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
)

// seedOpenIssueRaw creates an issue through the CLI and returns its id.
func seedOpenIssueRaw(t *testing.T, ctx context.Context, ap *app.App, title string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runNew(ctx, &out, ap, []string{"--title", title, "--topic", "redirect", "--type", "task"}); err != nil {
		t.Fatalf("runNew(%q) error = %v", title, err)
	}
	return strings.Fields(out.String())[0]
}

// TestCloseAsDuplicateRequiresTarget pins the parse-boundary requirement: a
// redirect resolution without --of is rejected before any write.
func TestCloseAsDuplicateRequiresTarget(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedOpenIssueRaw(t, ctx, ap, "Duplicate without target")

	var out bytes.Buffer
	err := runTransition(ctx, &out, ap, []string{id, "--resolution", "duplicate"}, "close")
	if err == nil {
		t.Fatal("close --resolution duplicate without --of = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "--of") {
		t.Fatalf("error = %v, want guidance naming --of", err)
	}
}

// TestCloseAsObsoleteRejectsTarget pins the converse: a terminal resolution must
// not carry --of.
func TestCloseAsObsoleteRejectsTarget(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedOpenIssueRaw(t, ctx, ap, "Obsolete with target")
	canonical := seedOpenIssueRaw(t, ctx, ap, "Canonical")

	var out bytes.Buffer
	err := runTransition(ctx, &out, ap, []string{id, "--resolution", "obsolete", "--of", canonical}, "close")
	if err == nil {
		t.Fatal("close --resolution obsolete --of X = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "--of") {
		t.Fatalf("error = %v, want message about --of", err)
	}
}

// TestRedirectTargetRejectedOnNonClose pins that --of applies only to close.
func TestRedirectTargetRejectedOnNonClose(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedOpenIssueRaw(t, ctx, ap, "Reopen with stray target")

	var out bytes.Buffer
	err := runTransition(ctx, &out, ap, []string{id, "--of", "test-x"}, "reopen")
	if err == nil {
		t.Fatal("reopen --of X = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "--of applies only to close") {
		t.Fatalf("error = %v, want '--of applies only to close'", err)
	}
}

// TestCloseAsDuplicateRecordsRedirectEdge is the CLI happy path: a duplicate
// close with --of records the related-to edge to the canonical ticket.
func TestCloseAsDuplicateRecordsRedirectEdge(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	canonical := seedOpenIssueRaw(t, ctx, ap, "Canonical")
	dup := seedOpenIssueRaw(t, ctx, ap, "Duplicate")

	var out bytes.Buffer
	if err := runTransition(ctx, &out, ap, []string{dup, "--resolution", "duplicate", "--of", canonical}, "close"); err != nil {
		t.Fatalf("runTransition(close duplicate) error = %v", err)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, dup)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if len(detail.Related) != 1 || detail.Related[0].ID != canonical {
		t.Fatalf("Related = %#v, want one edge to %s", detail.Related, canonical)
	}
}
