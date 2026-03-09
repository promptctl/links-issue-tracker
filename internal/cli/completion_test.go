package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

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

func TestRunHelpIncludesCompletion(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"help"}); err != nil {
		t.Fatalf("Run(help) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "lit completion <bash|zsh|fish>") {
		t.Fatalf("help output missing completion command: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "lit quickstart [--json]") {
		t.Fatalf("help output missing quickstart command: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "lit ready [--assignee <user>] [--limit N] [--format lines|table] [--columns ...] [--json]") {
		t.Fatalf("help output missing ready command: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "lit hooks install [--json]") {
		t.Fatalf("help output missing hooks command: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "lit migrate beads [--apply] [--json]") {
		t.Fatalf("help output missing migrate command: %q", stdout.String())
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
