package cli

import (
	"bytes"
	"context"
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
	if !strings.Contains(help, "ready List open work") {
		t.Fatalf("help output missing ready command: %q", help)
	}
}

func TestRunHelpDocumentsRankOrderingDefaults(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"help"}); err != nil {
		t.Fatalf("Run(help) error = %v", err)
	}
	help := normalizeWhitespace(stdout.String())
	if !strings.Contains(help, "ready List open work by readiness and rank") {
		t.Fatalf("help output missing rank-based ready description: %q", help)
	}
	if !strings.Contains(help, "ls List issues (rank by default)") {
		t.Fatalf("help output missing default rank ls description: %q", help)
	}
	if !strings.Contains(help, "children List child issues by rank") {
		t.Fatalf("help output missing rank-based children description: %q", help)
	}
}
