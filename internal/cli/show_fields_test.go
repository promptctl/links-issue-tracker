package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A single --field request prints the bare value with no label, so it
// round-trips directly into `lit update --description` and similar.
func TestPrintIssueFieldsSingleFieldPrintsBareValue(t *testing.T) {
	t.Parallel()
	issue := model.Issue{ID: "test.1", Title: "A title", Description: "Multi-line\ndescription body"}

	var buf bytes.Buffer
	if err := printIssueFields(&buf, issue, []string{"description"}); err != nil {
		t.Fatalf("printIssueFields() error = %v", err)
	}

	want := "Multi-line\ndescription body\n"
	if buf.String() != want {
		t.Fatalf("printIssueFields() = %q, want %q", buf.String(), want)
	}
}

// Multiple --field names print as "name: value" lines, in the requested
// order, so a multi-line value stays attributable to its field.
func TestPrintIssueFieldsMultipleFieldsPrintsLabeledLines(t *testing.T) {
	t.Parallel()
	issue := model.Issue{ID: "test.1", Title: "A title", Description: "The body"}

	var buf bytes.Buffer
	if err := printIssueFields(&buf, issue, []string{"title", "description"}); err != nil {
		t.Fatalf("printIssueFields() error = %v", err)
	}

	want := "title: A title\ndescription: The body\n"
	if buf.String() != want {
		t.Fatalf("printIssueFields() = %q, want %q", buf.String(), want)
	}
}

// An unknown field name fails clean with a UsageError naming the accepted
// vocabulary, and prints nothing — fields are validated before any output,
// so a typo in a multi-field request never emits a partial result.
func TestPrintIssueFieldsUnknownFieldReturnsUsageErrorWithNoOutput(t *testing.T) {
	t.Parallel()
	issue := model.Issue{ID: "test.1", Title: "A title", Description: "The body"}

	var buf bytes.Buffer
	err := printIssueFields(&buf, issue, []string{"title", "descriptoin"})
	var usageErr UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("printIssueFields() error = %v, want UsageError", err)
	}
	if !strings.Contains(usageErr.Error(), `"descriptoin"`) {
		t.Errorf("error should name the bad field, got %q", usageErr.Error())
	}
	if !strings.Contains(usageErr.Error(), "description") {
		t.Errorf("error should list valid field names, got %q", usageErr.Error())
	}
	if buf.Len() != 0 {
		t.Errorf("no output should be written on a validation error, got %q", buf.String())
	}
}

// `lit show <id> --field description` on an epic child returns only the
// description — no header fields, no parent-epic body, no siblings, no
// epic/children summary block. This is the ticket's core contract: an agent
// can read a ticket's description without the full multi-hundred-line dump.
func TestRunShowFieldOmitsAllSurroundingContext(t *testing.T) {
	f := newEpicFixture(t, "Plan epic", "the epic's own long description")
	sibling := f.addChild("Sibling")
	focus, err := f.ap.Store.CreateIssue(f.ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Focused child", Topic: "epic-view", IssueType: "task", Priority: 0,
		ParentID: f.epicID, Description: "the focused child's own description",
	})
	if err != nil {
		t.Fatalf("CreateIssue(focus) error = %v", err)
	}
	_ = sibling

	var buf bytes.Buffer
	if err := runShow(context.Background(), &buf, f.ap, []string{focus.ID, "--field", "description"}); err != nil {
		t.Fatalf("runShow(--field description) error = %v", err)
	}

	want := "the focused child's own description\n"
	if buf.String() != want {
		t.Fatalf("runShow(--field description) = %q, want %q", buf.String(), want)
	}
}

// Requesting an unknown field through the full command path surfaces the
// same UsageError printIssueFields returns, so `lit show <id> --field bogus`
// fails clean instead of silently falling back to the full dump.
func TestRunShowUnknownFieldReturnsUsageError(t *testing.T) {
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(context.Background(), storage.CreateIssueInput{
		Prefix: "test", Title: "Free floating", Topic: "misc", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	var buf bytes.Buffer
	err = runShow(context.Background(), &buf, ap, []string{issue.ID, "--field", "bogus"})
	var usageErr UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("runShow(--field bogus) error = %v, want UsageError", err)
	}
}

// Omitting --field entirely leaves the default full-detail view (header,
// epic context, siblings, ...) unchanged — the new capability is additive.
func TestRunShowWithoutFieldFlagIsUnchanged(t *testing.T) {
	f := newEpicFixture(t, "Plan epic", "epic body")
	focus := f.addChild("Focused child")

	out := showOutput(t, f.ap, focus)

	if !strings.Contains(out, "Epic: "+f.epicID+" — Plan epic") {
		t.Errorf("default show should still append the epic block:\n%s", out)
	}
}
