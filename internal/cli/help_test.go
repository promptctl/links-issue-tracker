package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunHelpForInitMarksHumanBoundary(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"help", "init"}); err != nil {
		t.Fatalf("Run(help init) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Human bootstrap command.") {
		t.Fatalf("help init output missing human boundary: %q", output)
	}
	if !strings.Contains(output, "lit init [--json] [--skip-hooks] [--skip-agents]") {
		t.Fatalf("help init output missing usage: %q", output)
	}
}

func TestRunCommandHelpFlagForReady(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"ready", "--help"}); err != nil {
		t.Fatalf("Run(ready --help) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Agent-facing operational command.") {
		t.Fatalf("ready --help output missing agent-facing guidance: %q", output)
	}
	if !strings.Contains(output, "lit ready [--assignee <user>] [--limit N] [--format lines|table] [--columns ...] [--json]") {
		t.Fatalf("ready --help output missing usage: %q", output)
	}
}

func TestRunHelpUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	err := Run(context.Background(), &stdout, &stdout, []string{"help", "not-a-command"})
	if err == nil {
		t.Fatal("Run(help not-a-command) unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), `unknown command "not-a-command"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}
