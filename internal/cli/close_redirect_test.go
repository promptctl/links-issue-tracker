package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/store"
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
	err := runTransition(ctx, &out, ap, []string{id, "--resolution", "duplicate"}, closeSpec)
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
	err := runTransition(ctx, &out, ap, []string{id, "--resolution", "obsolete", "--of", canonical}, closeSpec)
	if err == nil {
		t.Fatal("close --resolution obsolete --of X = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "--of") {
		t.Fatalf("error = %v, want message about --of", err)
	}
}

// TestRedirectTargetRejectedOnNonClose pins that --of exists only on close:
// no other transition registers the flag, so misuse is the parser's
// unknown-flag error rather than a runtime carve-out. [LAW:types-are-the-program]
func TestRedirectTargetRejectedOnNonClose(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	id := seedOpenIssueRaw(t, ctx, ap, "Reopen with stray target")

	var out bytes.Buffer
	err := runTransition(ctx, &out, ap, []string{id, "--of", "test-x"}, openSpec)
	if err == nil {
		t.Fatal("reopen --of X = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "unknown flag: --of") {
		t.Fatalf("error = %v, want unknown-flag parse rejection", err)
	}
}

// TestCloseAsDuplicateRecordsRedirectEdge is the CLI happy path: a duplicate
// close with --of records the canonical ticket as the redirect target, lifted
// out of the generic related group.
func TestCloseAsDuplicateRecordsRedirectEdge(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	canonical := seedOpenIssueRaw(t, ctx, ap, "Canonical")
	dup := seedOpenIssueRaw(t, ctx, ap, "Duplicate")

	var out bytes.Buffer
	if err := runTransition(ctx, &out, ap, []string{dup, "--resolution", "duplicate", "--of", canonical}, closeSpec); err != nil {
		t.Fatalf("runTransition(close duplicate) error = %v", err)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, dup)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.RedirectTarget == nil || detail.RedirectTarget.ID != canonical {
		t.Fatalf("RedirectTarget = %#v, want the canonical ticket %s", detail.RedirectTarget, canonical)
	}
	if len(detail.Related) != 0 {
		t.Fatalf("Related = %#v, want empty (redirect lifted out)", detail.Related)
	}
}

// TestShowRendersRedirectDistinctFromRelated pins the rendering acceptance: a
// ticket closed as duplicate presents its canonical target under a `redirect:`
// group, not flattened into `related:`.
func TestShowRendersRedirectDistinctFromRelated(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	canonical := seedOpenIssueRaw(t, ctx, ap, "Canonical")
	dup := seedOpenIssueRaw(t, ctx, ap, "Duplicate")

	var sink bytes.Buffer
	if err := runTransition(ctx, &sink, ap, []string{dup, "--resolution", "duplicate", "--of", canonical}, closeSpec); err != nil {
		t.Fatalf("runTransition(close duplicate) error = %v", err)
	}

	var out bytes.Buffer
	if err := runShow(ctx, &out, ap, []string{dup}); err != nil {
		t.Fatalf("runShow(%s) error = %v", dup, err)
	}
	text := out.String()
	if !strings.Contains(text, "redirect:\n- "+canonical) {
		t.Fatalf("show output missing redirect group for %s; got:\n%s", canonical, text)
	}
	if strings.Contains(text, "related:") {
		t.Fatalf("redirect must not render under related:; got:\n%s", text)
	}
}

// TestShowManualRelatedRendersUnchanged pins the no-regression half of the
// acceptance: a ticket with a manual related edge and no redirect renders the
// related group exactly as before, with no redirect group.
func TestShowManualRelatedRendersUnchanged(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	focal := seedOpenIssueRaw(t, ctx, ap, "Focal")
	peer := seedOpenIssueRaw(t, ctx, ap, "Peer")
	if _, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: focal, DstID: peer, Type: "related-to", CreatedBy: "test"}); err != nil {
		t.Fatalf("AddRelation(related) error = %v", err)
	}

	var out bytes.Buffer
	if err := runShow(ctx, &out, ap, []string{focal}); err != nil {
		t.Fatalf("runShow(%s) error = %v", focal, err)
	}
	text := out.String()
	if !strings.Contains(text, "related:\n- "+peer) {
		t.Fatalf("show output missing related group for %s; got:\n%s", peer, text)
	}
	if strings.Contains(text, "redirect:") {
		t.Fatalf("a manual related edge must not render as a redirect; got:\n%s", text)
	}
}

// TestShowRedirectAmbiguousWhenManualEdgePresent pins the no-mislabel fallback:
// when a duplicate-closed ticket also carries a manual related edge, storage
// cannot tell the redirect from the peer, so neither is promoted — both stay
// under related and no redirect group is emitted.
func TestShowRedirectAmbiguousWhenManualEdgePresent(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	canonical := seedOpenIssueRaw(t, ctx, ap, "Canonical")
	peer := seedOpenIssueRaw(t, ctx, ap, "Peer")
	dup := seedOpenIssueRaw(t, ctx, ap, "Duplicate")
	if _, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: dup, DstID: peer, Type: "related-to", CreatedBy: "test"}); err != nil {
		t.Fatalf("AddRelation(related) error = %v", err)
	}

	var sink bytes.Buffer
	if err := runTransition(ctx, &sink, ap, []string{dup, "--resolution", "duplicate", "--of", canonical}, closeSpec); err != nil {
		t.Fatalf("runTransition(close duplicate) error = %v", err)
	}

	detail, err := ap.Store.GetIssueDetail(ctx, dup)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.RedirectTarget != nil {
		t.Fatalf("RedirectTarget = %#v, want nil when redirect is indistinguishable from a manual peer", detail.RedirectTarget)
	}
	if len(detail.Related) != 2 {
		t.Fatalf("Related = %#v, want both edges under related", detail.Related)
	}

	var out bytes.Buffer
	if err := runShow(ctx, &out, ap, []string{dup}); err != nil {
		t.Fatalf("runShow(%s) error = %v", dup, err)
	}
	if strings.Contains(out.String(), "redirect:") {
		t.Fatalf("ambiguous redirect must not be promoted; got:\n%s", out.String())
	}
}

// TestCloseAsDuplicateRendersRedirectAdjacency pins the epic's done/close arm:
// closing as duplicate surfaces the redirect target in the capture-at-close
// adjacency output, the moment the closing agent most needs "where it went".
func TestCloseAsDuplicateRendersRedirectAdjacency(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	canonical := seedOpenIssueRaw(t, ctx, ap, "Canonical")
	dup := seedOpenIssueRaw(t, ctx, ap, "Duplicate")

	var out bytes.Buffer
	if err := runTransition(ctx, &out, ap, []string{dup, "--resolution", "duplicate", "--of", canonical}, closeSpec); err != nil {
		t.Fatalf("runTransition(close duplicate) error = %v", err)
	}
	if !strings.Contains(out.String(), "redirect:\n- "+canonical) {
		t.Fatalf("close adjacency missing redirect group for %s; got:\n%s", canonical, out.String())
	}
}
