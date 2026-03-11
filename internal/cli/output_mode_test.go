package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseGlobalOutputMode(t *testing.T) {
	tests := []struct {
		name      string
		envOutput string
		args      []string
		wantArgs  []string
		wantMode  outputMode
	}{
		{
			name:      "output overrides json and env",
			envOutput: "json",
			args:      []string{"--json", "--output", "text", "quickstart"},
			wantArgs:  []string{"quickstart"},
			wantMode:  outputModeText,
		},
		{
			name:      "output precedence is stable regardless of flag order",
			envOutput: "text",
			args:      []string{"--output", "text", "--json", "quickstart"},
			wantArgs:  []string{"quickstart"},
			wantMode:  outputModeText,
		},
		{
			name:      "json false sets text mode",
			envOutput: "json",
			args:      []string{"--json=false", "quickstart"},
			wantArgs:  []string{"quickstart"},
			wantMode:  outputModeText,
		},
		{
			name:      "json true sets json mode",
			envOutput: "text",
			args:      []string{"--json=true", "quickstart"},
			wantArgs:  []string{"quickstart"},
			wantMode:  outputModeJSON,
		},
		{
			name:      "double dash ends global parsing",
			envOutput: "text",
			args:      []string{"--output", "json", "--", "quickstart"},
			wantArgs:  []string{"quickstart"},
			wantMode:  outputModeJSON,
		},
		{
			name:      "command args that look like output flags are preserved",
			envOutput: "text",
			args:      []string{"new", "--title", "--output"},
			wantArgs:  []string{"new", "--title", "--output"},
			wantMode:  outputModeText,
		},
		{
			name:      "last json flag wins within json precedence tier",
			envOutput: "text",
			args:      []string{"--json=false", "--json=true", "quickstart"},
			wantArgs:  []string{"quickstart"},
			wantMode:  outputModeJSON,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(outputModeEnvVar, tc.envOutput)
			gotArgs, gotMode, err := parseGlobalOutputMode(tc.args, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("parseGlobalOutputMode() error = %v", err)
			}
			if gotMode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", gotMode, tc.wantMode)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", gotArgs, tc.wantArgs)
			}
		})
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

func TestParseGlobalOutputModeErrors(t *testing.T) {
	nonTTY := &bytes.Buffer{}

	t.Run("invalid env output mode", func(t *testing.T) {
		t.Setenv(outputModeEnvVar, "yaml")
		_, _, err := parseGlobalOutputMode([]string{"quickstart"}, nonTTY)
		if err == nil {
			t.Fatalf("expected error for invalid env mode")
		}
		if !strings.Contains(err.Error(), "expected auto|text|json") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid output mode", func(t *testing.T) {
		t.Setenv(outputModeEnvVar, "")
		_, _, err := parseGlobalOutputMode([]string{"--output", "yaml", "quickstart"}, nonTTY)
		if err == nil {
			t.Fatalf("expected error for invalid --output")
		}
	})

	t.Run("missing output value", func(t *testing.T) {
		t.Setenv(outputModeEnvVar, "")
		_, _, err := parseGlobalOutputMode([]string{"--output"}, nonTTY)
		if err == nil {
			t.Fatalf("expected error for missing --output value")
		}
	})

	t.Run("invalid json bool value", func(t *testing.T) {
		t.Setenv(outputModeEnvVar, "")
		_, _, err := parseGlobalOutputMode([]string{"--json=nope", "quickstart"}, nonTTY)
		if err == nil {
			t.Fatalf("expected error for invalid --json value")
		}
	})
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
