package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseGlobalOutputMode(t *testing.T) {
	t.Setenv(outputModeEnvVar, "")

	args, mode, err := parseGlobalOutputMode([]string{"--json", "--output", "text", "quickstart"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseGlobalOutputMode() error = %v", err)
	}
	if mode != outputModeText {
		t.Fatalf("mode = %q, want %q", mode, outputModeText)
	}
	if len(args) != 1 || args[0] != "quickstart" {
		t.Fatalf("args = %#v, want [quickstart]", args)
	}
}

func TestParseGlobalOutputModeFallsBackToEnv(t *testing.T) {
	t.Setenv(outputModeEnvVar, "text")

	_, mode, err := parseGlobalOutputMode([]string{"quickstart"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseGlobalOutputMode() error = %v", err)
	}
	if mode != outputModeText {
		t.Fatalf("mode = %q, want %q", mode, outputModeText)
	}
}

func TestParseGlobalOutputModeRejectsInvalidEnv(t *testing.T) {
	t.Setenv(outputModeEnvVar, "yaml")

	_, _, err := parseGlobalOutputMode([]string{"quickstart"}, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("expected error for invalid env mode")
	}
	if !strings.Contains(err.Error(), "expected auto|text|json") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunQuickstartDefaultsToJSONOnNonTTY(t *testing.T) {
	t.Setenv(outputModeEnvVar, "")
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart"}); err != nil {
		t.Fatalf("Run(quickstart) error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("quickstart default output should be json: %v", err)
	}
}

func TestRunQuickstartOutputFlagOverridesDefault(t *testing.T) {
	t.Setenv(outputModeEnvVar, "")
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"--output", "text", "quickstart"}); err != nil {
		t.Fatalf("Run(--output text quickstart) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Agent quickstart for links issue tracking") {
		t.Fatalf("expected text quickstart output, got %q", stdout.String())
	}
}

func TestRunQuickstartJSONFlagOverridesEnv(t *testing.T) {
	t.Setenv(outputModeEnvVar, "text")
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"--json", "quickstart"}); err != nil {
		t.Fatalf("Run(--json quickstart) error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("expected json output when --json is set: %v", err)
	}
}
