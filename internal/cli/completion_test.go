package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func normalizeWhitespace(input string) string {
	return strings.Join(strings.Fields(input), " ")
}

func TestCompletionScriptsRender(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var stdout bytes.Buffer
		if err := runCompletion(&stdout, []string{shell}); err != nil {
			t.Fatalf("runCompletion(%q) error = %v", shell, err)
		}
		if !strings.Contains(stdout.String(), "lit") {
			t.Fatalf("completion output for %q missing lit command name: %q", shell, stdout.String())
		}
	}
}

func TestCompletionScriptsIncludeUpdateCommand(t *testing.T) {
	cases := []struct {
		shell string
		want  string
	}{
		{shell: "bash", want: "show update rank start"},
		{shell: "zsh", want: "'update:update issue fields'"},
		{shell: "zsh", want: "'rank:reorder"},
		{shell: "fish", want: "show update rank start"},
	}
	for _, tc := range cases {
		var stdout bytes.Buffer
		if err := runCompletion(&stdout, []string{tc.shell}); err != nil {
			t.Fatalf("runCompletion(%q) error = %v", tc.shell, err)
		}
		if !strings.Contains(stdout.String(), tc.want) {
			t.Fatalf("%s completion missing update marker %q", tc.shell, tc.want)
		}
	}
}

func TestRunHelpIncludesCompletion(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"help"}); err != nil {
		t.Fatalf("Run(help) error = %v", err)
	}
	help := normalizeWhitespace(stdout.String())
	if !strings.Contains(help, "completion Generate shell completion script") {
		t.Fatalf("help output missing completion command: %q", help)
	}
	if !strings.Contains(help, "quickstart Agent quickstart workflow") {
		t.Fatalf("help output missing quickstart command: %q", help)
	}
	if !strings.Contains(help, "ready List open work") {
		t.Fatalf("help output missing ready command: %q", help)
	}
}
func TestQuickstartOutputsStructuredJSON(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart", "--json"}); err != nil {
		t.Fatalf("Run(quickstart --json) error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("quickstart json decode failed: %v", err)
	}
	if _, ok := payload["summary"]; !ok {
		t.Fatalf("quickstart payload missing summary: %#v", payload)
	}
	if _, ok := payload["workflow"]; !ok {
		t.Fatalf("quickstart payload missing workflow: %#v", payload)
	}
}
