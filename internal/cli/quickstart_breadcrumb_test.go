package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
	if err := runTransition(ctx, &startOut, ap, []string{issueID}, startSpec); err != nil {
		t.Fatalf("runTransition(start) error = %v", err)
	}
	if got, want := lastLine(startOut.String()), quickstartBreadcrumb("ready"); got != want {
		t.Fatalf("lit start last line = %q, want breadcrumb %q", got, want)
	}

	var labelOut bytes.Buffer
	if err := runAppFamily(labelFamily, ctx, &labelOut, ap, []string{"add", issueID, "probe"}); err != nil {
		t.Fatalf("runLabel(add) error = %v", err)
	}
	if got, want := lastLine(labelOut.String()), quickstartBreadcrumb("update"); got != want {
		t.Fatalf("lit label add last line = %q, want breadcrumb %q", got, want)
	}

	var closeOut bytes.Buffer
	if err := runTransition(ctx, &closeOut, ap, []string{issueID, "--resolution", "wontfix", "--reason", "probe done"}, closeSpec); err != nil {
		t.Fatalf("runTransition(close) error = %v", err)
	}
	if got, want := lastLine(closeOut.String()), quickstartBreadcrumb("done"); got != want {
		t.Fatalf("lit close last line = %q, want breadcrumb %q", got, want)
	}
}
