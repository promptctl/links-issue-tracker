package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// listFormatOutput runs `lit ls --format expr` against a store holding one
// issue and returns stdout and the error.
func listFormatOutput(t *testing.T, expr string) (string, error) {
	t.Helper()
	ctx := context.Background()
	ap := newTestCLIApp(t)
	if _, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "fmt", Title: "T", Topic: "fmt", IssueType: "task", Priority: 0}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	var out bytes.Buffer
	err := runListWithStore(ctx, &out, ap.Store, noReadyPolicy, []string{"--format", expr})
	return out.String(), err
}

// TestListFormatRejectsUnknownName is the reject half of the `--format` table.
// The bug it closes: a bad format was classified as a generic failure, so its
// remediation told the caller to retry a command that can never succeed, and
// the message never said which formats exist.
func TestListFormatRejectsUnknownName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expr    string
		unknown string
	}{
		{name: "json, the format the ticket asked for", expr: "json", unknown: "json"},
		{name: "explicit empty value", expr: "", unknown: ""},
		{name: "normalized before reporting", expr: " JSON ", unknown: "json"},
		// Near-misses: a lookup that loosened to a prefix or substring match
		// would accept these while every row above still passed.
		{name: "prefix of a valid name", expr: "line", unknown: "line"},
		{name: "valid name with a suffix", expr: "tables", unknown: "tables"},
		{name: "valid name with a prefix", expr: "xtable", unknown: "xtable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := listFormatOutput(t, tc.expr)
			if err == nil {
				t.Fatalf("--format %q: want error, got nil (output:\n%s)", tc.expr, out)
			}
			if out != "" {
				t.Errorf("--format %q printed output before refusing:\n%s", tc.expr, out)
			}
			if got := ExitCode(err); got != ExitValidation {
				t.Errorf("--format %q exit code = %d, want %d (ExitValidation)", tc.expr, got, ExitValidation)
			}
			if quoted := fmt.Sprintf("%q", tc.unknown); !strings.Contains(err.Error(), quoted) {
				t.Errorf("--format %q error %q does not name the offending format as %s", tc.expr, err, quoted)
			}
			if !strings.Contains(err.Error(), "(valid: lines, table)") {
				t.Errorf("--format %q error %q does not name the valid formats", tc.expr, err)
			}
			var stderr bytes.Buffer
			WriteCommandError(&stderr, err)
			if strings.Contains(stderr.String(), "Retry the command") {
				t.Errorf("--format %q remediation tells the caller to retry a deterministic refusal:\n%s", tc.expr, stderr.String())
			}
		})
	}
}

// TestListFormatAcceptsEveryAdvertisedName is the accept half: every name the
// help text advertises renders the listing in its own shape, including when
// typed in mixed case. The advertised set must equal this table, so a format
// added to the vocabulary without a row here fails.
func TestListFormatAcceptsEveryAdvertisedName(t *testing.T) {
	// firstLine is what each format puts on its first line: lines starts
	// straight in on the issue row, table leads with a header.
	cases := []struct {
		expr      string
		firstLine func(string) bool
	}{
		{expr: "lines", firstLine: func(l string) bool { return strings.HasPrefix(l, "fmt-") && strings.Contains(l, " | ") }},
		{expr: "table", firstLine: func(l string) bool { return strings.HasPrefix(l, "ID ") }},
		{expr: " TABLE ", firstLine: func(l string) bool { return strings.HasPrefix(l, "ID ") }},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[strings.ToLower(strings.TrimSpace(tc.expr))] = true
		t.Run(tc.expr, func(t *testing.T) {
			out, err := listFormatOutput(t, tc.expr)
			if err != nil {
				t.Fatalf("--format %q: %v", tc.expr, err)
			}
			first, _, _ := strings.Cut(out, "\n")
			if !tc.firstLine(first) || !strings.Contains(out, "fmt-") {
				t.Errorf("--format %q rendered the wrong shape:\n%s", tc.expr, out)
			}
		})
	}
	for _, name := range sortedListFormatNames() {
		if !covered[name] {
			t.Errorf("advertised --format %q has no accept row", name)
		}
	}
}
