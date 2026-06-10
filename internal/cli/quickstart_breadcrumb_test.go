package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func TestQuickstartBreadcrumbDerivesFromTopicTable(t *testing.T) {
	for _, token := range quickstartTopicTokens() {
		crumb := quickstartBreadcrumb(token)
		if !strings.Contains(crumb, "lit quickstart "+token) {
			t.Fatalf("quickstartBreadcrumb(%q) = %q, want pointer at `lit quickstart %s`", token, crumb, token)
		}
		if strings.Contains(crumb, "\n") {
			t.Fatalf("quickstartBreadcrumb(%q) = %q, must be a single line", token, crumb)
		}
	}
}

func TestQuickstartBreadcrumbPanicsOnUnknownTopic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("quickstartBreadcrumb(bogus) should panic; a wiring typo must fail loudly, not print a broken hint")
		}
	}()
	_ = quickstartBreadcrumb("bogus")
}

func TestTransitionBreadcrumbTopicsAreValidTokens(t *testing.T) {
	for action, topic := range transitionBreadcrumbTopics {
		if _, ok := quickstartTopicTemplate(topic); !ok {
			t.Fatalf("transitionBreadcrumbTopics[%q] = %q, which is not a quickstartTopics token", action, topic)
		}
	}
}

func TestMutationTextOutputEndsWithBreadcrumb(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	lastLine := func(out string) string {
		t.Helper()
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		return lines[len(lines)-1]
	}

	var newOut bytes.Buffer
	if err := runNew(ctx, &newOut, ap, []string{"--title", "Crumb probe", "--topic", "crumbs", "--type", "task"}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}
	if got, want := lastLine(newOut.String()), quickstartBreadcrumb("new"); got != want {
		t.Fatalf("lit new last line = %q, want breadcrumb %q", got, want)
	}
	issueID := strings.Fields(newOut.String())[0]

	var startOut bytes.Buffer
	if err := runTransition(ctx, &startOut, ap, []string{issueID}, "start"); err != nil {
		t.Fatalf("runTransition(start) error = %v", err)
	}
	if got, want := lastLine(startOut.String()), quickstartBreadcrumb("ready"); got != want {
		t.Fatalf("lit start last line = %q, want breadcrumb %q", got, want)
	}

	var labelOut bytes.Buffer
	if err := runLabel(ctx, &labelOut, ap, []string{"add", issueID, "probe"}); err != nil {
		t.Fatalf("runLabel(add) error = %v", err)
	}
	if got, want := lastLine(labelOut.String()), quickstartBreadcrumb("update"); got != want {
		t.Fatalf("lit label add last line = %q, want breadcrumb %q", got, want)
	}

	var closeOut bytes.Buffer
	if err := runTransition(ctx, &closeOut, ap, []string{issueID, "--reason", "probe done"}, "close"); err != nil {
		t.Fatalf("runTransition(close) error = %v", err)
	}
	if got, want := lastLine(closeOut.String()), quickstartBreadcrumb("done"); got != want {
		t.Fatalf("lit close last line = %q, want breadcrumb %q", got, want)
	}
}

func TestBreadcrumbAbsentFromJSONOutput(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var newOut bytes.Buffer
	if err := runNew(ctx, &newOut, ap, []string{"--title", "JSON probe", "--topic", "crumbs", "--type", "task", "--json"}); err != nil {
		t.Fatalf("runNew(--json) error = %v", err)
	}
	var created model.Issue
	if err := json.Unmarshal(newOut.Bytes(), &created); err != nil {
		t.Fatalf("lit new --json output must be exactly one JSON document, got %q: %v", newOut.String(), err)
	}
	if strings.Contains(newOut.String(), "quickstart") {
		t.Fatalf("breadcrumb leaked into JSON output: %q", newOut.String())
	}

	// Global --json (outputModeWriter) must be just as breadcrumb-free as the
	// command-local flag: both routes resolve inside printValue.
	globalOut := outputModeWriter{Writer: &bytes.Buffer{}, mode: outputModeJSON}
	var labelBuf bytes.Buffer
	globalOut.Writer = &labelBuf
	if err := runLabel(ctx, globalOut, ap, []string{"add", created.ID, "probe"}); err != nil {
		t.Fatalf("runLabel(add) under global JSON mode error = %v", err)
	}
	var labels []string
	if err := json.Unmarshal(labelBuf.Bytes(), &labels); err != nil {
		t.Fatalf("label add output under global JSON mode must be exactly one JSON document, got %q: %v", labelBuf.String(), err)
	}
	if strings.Contains(labelBuf.String(), "quickstart") {
		t.Fatalf("breadcrumb leaked into global JSON mode output: %q", labelBuf.String())
	}

	// Sanity: the store really holds what JSON mode reported.
	if _, err := ap.Store.GetIssue(ctx, created.ID); err != nil {
		t.Fatalf("GetIssue(%s) error = %v", created.ID, err)
	}
}
